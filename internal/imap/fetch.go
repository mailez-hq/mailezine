// FETCH implementation: metadata + body sections, with automatic \Seen on
// non-PEEK body fetches (RFC 3501 semantics).
package imap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/textproto"
	"mailezine/internal/imapserver"

	"mailezine/internal/mailstore"
)

// maxCachedSectionBytes caps what the body-section memo stores. The list path
// fetches BODY.PEEK[HEADER] for every row (a couple of KB) and the preview
// path a 4KB text fragment, both on every page load; whole-body sections are
// left out so they cannot evict the envelope memo this shares a budget with.
const maxCachedSectionBytes = 64 << 10

// maxCachedRawBytes caps what the whole-message memo stores. Below it, the
// buffer answers every later envelope, body-structure and section request for
// that message; above it the message is large enough that pinning it would
// crowd out the messages a page actually revisits.
const maxCachedRawBytes = 256 << 10

// rawKey identifies one message's buffer (immutable, like the sections).
func rawKey(envKey string) string { return "raw\x00" + envKey }

// sectionsAllCached reports whether every requested body section came from
// the memo, so none of them needs the message blob.
func sectionsAllCached(hit []bool) bool {
	for _, ok := range hit {
		if !ok {
			return false
		}
	}
	return true
}

// sectionKey identifies one body section of one immutable message: the
// message identity (account, mailbox, uidvalidity, UID) plus every field that
// shapes the extracted bytes. Partial must be spelled out — %v on the pointer
// would key on its address and never hit.
func sectionKey(envKey string, bs *imap.FetchItemBodySection) string {
	var b strings.Builder
	b.WriteString("sec\x00")
	b.WriteString(envKey)
	fmt.Fprintf(&b, "\x00%q\x00%v\x00%q\x00%q\x00%t\x00",
		bs.Specifier, bs.Part, bs.HeaderFields, bs.HeaderFieldsNot, bs.Peek)
	if bs.Partial != nil {
		fmt.Fprintf(&b, "%d:%d", bs.Partial.Offset, bs.Partial.Size)
	}
	return b.String()
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	ctx := context.Background()
	// FETCH operates on the selected snapshot: the mailbox was listed at
	// SELECT time and refreshSnapshot keeps it current after writes, so a
	// fetch burst does not re-scan the whole mailbox per command.
	msgs := s.snap
	if msgs == nil {
		var err error
		msgs, err = s.srv.Store.ListMessages(ctx, s.user, s.mbox)
		if err != nil {
			return err
		}
	}
	markSeen := false
	changed := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}
	// EXAMINE semantics: reading a body section from an examined mailbox
	// must not set \Seen (the implicit flag change would be a write).
	if s.readOnly {
		markSeen = false
	}
	maxSeq := uint32(len(msgs))
	maxUID := maxSeq
	if maxSeq > 0 {
		maxUID = msgs[maxSeq-1].UID
	}
	// Collect the matched messages first so their body buffers can be
	// prefetched in parallel: a cold-cache FETCH burst (mailbox open, list
	// refresh) otherwise pays one sequential blob round-trip per message —
	// 50 messages × a blob GET is seconds of wall clock and dominated
	// mailbox-open latency. Response processing below stays sequential.
	type fetchItem struct {
		seq     uint32
		msg     *mailstore.Message
		envKey  string
		bsKey   string
		env     *imap.Envelope
		envRaw  string
		bs      imap.BodyStructure
		bsRaw   string
		secHit  []bool
		secBuf  [][]byte
		needBuf bool
		buf     []byte
		err     error
	}
	var items []fetchItem
	for i, msg := range msgs {
		seq := uint32(i) + 1
		if !numMatches(numSet, seq, msg.UID, maxSeq, maxUID) {
			continue
		}
		it := fetchItem{seq: seq, msg: msg}
		// Envelope and body structure are memoised per (account, mailbox,
		// uidvalidity, UID); only body sections and cache misses need the
		// blob. The uidvalidity dimension prevents a delete+recreate of the
		// mailbox from serving stale cached envelopes under reused UIDs.
		// The envelope memo holds the pre-encoded wire payload (see
		// imapserver.EncodeEnvelope), so a hit is one raw write — no
		// per-response re-walk of the envelope structure.
		it.envKey = s.user + "\x00" + s.mbox + "\x00" + strconv.FormatUint(uint64(s.uidvalidity), 10) + "\x00" + strconv.FormatUint(uint64(msg.UID), 10)
		it.bsKey = "bs\x00" + it.envKey + "\x00" + strconv.FormatBool(options.BodyStructure != nil && options.BodyStructure.Extended)
		if options.Envelope {
			if e, ok := s.srv.cache.Get(it.envKey); ok {
				switch v := e.(type) {
				case string:
					it.envRaw = v
				case *imap.Envelope:
					it.env = v
				}
			}
		}
		if options.BodyStructure != nil {
			if b, ok := s.srv.cache.Get(it.bsKey); ok {
				it.bs = b.(imap.BodyStructure)
			}
			// The store caches the BODYSTRUCTURE (extended) form at delivery,
			// which is the shape go-imap clients ask for. Replaying it spares
			// the whole-message walk: a search hydrates one per hit, so a
			// 479-hit query was ~6s of structure reads. A client asking for
			// the non-extended BODY still walks the message.
			if options.BodyStructure.Extended && len(it.msg.Body) > 0 {
				it.bsRaw = string(it.msg.Body)
			}
		}
		// Body sections are memoised as well: their bytes are content, and
		// content is immutable for a given (account, mailbox, uidvalidity,
		// UID) — flags live outside every section — so the memo needs no
		// invalidation and stays correct on multi-active nodes, where the
		// mailbox-metadata cache cannot be used at all. Without it a 50-row
		// list page re-reads 50 blobs for its headers (~620ms, measured
		// 2026-09-12) and the same again for its previews.
		if n := len(options.BodySection); n > 0 {
			it.secHit = make([]bool, n)
			it.secBuf = make([][]byte, n)
			for i, bs := range options.BodySection {
				if v, ok := s.srv.cache.Get(sectionKey(it.envKey, bs)); ok {
					it.secBuf[i], it.secHit[i] = v.([]byte), true
				}
			}
		}
		it.needBuf = len(options.BinarySection) > 0 ||
			len(options.BinarySectionSize) > 0 ||
			!sectionsAllCached(it.secHit) ||
			// A cached header block answers the envelope on its own, which is
			// what keeps the list's thread scan (300 envelopes, no sections)
			// off the blob store entirely.
			(options.Envelope && it.env == nil && it.envRaw == "" && len(it.msg.Head) == 0) ||
			(options.BodyStructure != nil && it.bs == nil && it.bsRaw == "")
		items = append(items, it)
	}
	// Bounded parallel blob prefetch. Buffers are read-only once loaded, so
	// the sequential processing loop can consume them without extra locks.
	{
		const prefetchWorkers = 8
		var wg sync.WaitGroup
		sem := make(chan struct{}, prefetchWorkers)
		for idx := range items {
			if !items[idx].needBuf {
				continue
			}
			wg.Add(1)
			go func(it *fetchItem) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if v, ok := s.srv.raw.Get(rawKey(it.envKey)); ok {
					it.buf = v.([]byte)
					return
				}
				rc, err := s.srv.Store.OpenMessage(ctx, s.user, s.mbox, it.msg.UID)
				if err != nil {
					it.err = err
					return
				}
				buf, err := io.ReadAll(rc)
				_ = rc.Close()
				it.buf, it.err = buf, err
				if err == nil && len(buf) > 0 && len(buf) <= maxCachedRawBytes {
					s.srv.raw.Put(rawKey(it.envKey), buf, int64(len(buf)))
				}
			}(&items[idx])
		}
		wg.Wait()
	}
	for _, it := range items {
		seq := it.seq
		msg := it.msg
		if it.needBuf && it.err != nil {
			// The message vanished between the snapshot and this fetch
			// (concurrent expunge, or a dangling index entry): RFC 3501
			// §6.4.8 — omit it from the response instead of failing the
			// whole command and locking the client out of the mailbox.
			if errors.Is(it.err, mailstore.ErrNotFound) {
				continue
			}
			return it.err
		}
		envKey := it.envKey
		bsKey := it.bsKey
		var env *imap.Envelope = it.env
		var envRaw string = it.envRaw
		var bs imap.BodyStructure = it.bs
		buf := it.buf
		if markSeen && !mailstore.HasFlag(msg.Flags, "\\Seen") {
			flags := append([]string(nil), msg.Flags...)
			flags = append(flags, "\\Seen")
			if err := s.srv.Store.SetFlags(ctx, s.user, s.mbox, msg.UID, flags); err != nil {
				if errors.Is(err, mailstore.ErrNotFound) {
					continue
				}
				return err
			}
			msg.Flags = flags
			changed = true
		}

		rw := w.CreateMessage(seq)
		rw.WriteUID(imap.UID(msg.UID))
		if options.ModSeq {
			rw.WriteModSeq(msg.ModSeq)
		}
		if options.Flags {
			rw.WriteFlags(imapFlags(msg.Flags))
		}
		if options.InternalDate {
			rw.WriteInternalDate(msg.InternalDate)
		}
		if options.RFC822Size {
			rw.WriteRFC822Size(msg.Size)
		}
		if options.Envelope {
			if envRaw == "" {
				if env == nil {
					src := buf
					if len(it.msg.Head) > 0 {
						src = it.msg.Head
					}
					env = envelopeOf(src)
				}
				// A message whose Date header is missing or unparseable
				// yields the zero time, which clients render literally as
				// year 1 (the webmail list showed "1年1月1日"). Fall back to
				// INTERNALDATE — the arrival time every other client shows
				// for such mail — so ENVELOPE never carries a bogus date.
				if env.Date.IsZero() && !msg.InternalDate.IsZero() {
					env.Date = msg.InternalDate
				}
				envRaw = imapserver.EncodeEnvelope(env)
				s.srv.cache.Put(envKey, envRaw, int64(len(envRaw)))
			}
			rw.WriteEnvelopeRaw(envRaw)
		}
		if options.BodyStructure != nil {
			switch {
			case bs != nil:
				rw.WriteBodyStructure(bs)
			case it.bsRaw != "":
				rw.WriteBodyStructureRaw(it.bsRaw, true)
			default:
				bs = imapserver.ExtractBodyStructure(bytes.NewReader(buf))
				s.srv.cache.Put(bsKey, bs, 2048)
				rw.WriteBodyStructure(bs)
			}
		}
		for i, bs := range options.BodySection {
			section := it.secBuf[i]
			if !it.secHit[i] {
				section = imapserver.ExtractBodySection(bytes.NewReader(buf), bs)
				if n := len(section); n > 0 && int64(n) <= maxCachedSectionBytes {
					s.srv.cache.Put(sectionKey(it.envKey, bs), section, int64(n))
				}
			}
			wc := rw.WriteBodySection(bs, int64(len(section)))
			if _, err := wc.Write(section); err != nil {
				_ = rw.Close()
				return err
			}
			if err := wc.Close(); err != nil {
				_ = rw.Close()
				return err
			}
		}
		for _, bs := range options.BinarySection {
			section := imapserver.ExtractBinarySection(bytes.NewReader(buf), bs)
			wc := rw.WriteBinarySection(bs, int64(len(section)))
			if _, err := wc.Write(section); err != nil {
				_ = rw.Close()
				return err
			}
			if err := wc.Close(); err != nil {
				_ = rw.Close()
				return err
			}
		}
		for _, bss := range options.BinarySectionSize {
			rw.WriteBinarySectionSize(bss, imapserver.ExtractBinarySectionSize(bytes.NewReader(buf), bss))
		}
		if err := rw.Close(); err != nil {
			return err
		}
	}
	if changed {
		return s.refreshSnapshot(ctx)
	}
	return nil
}

// numContains resolves a seq/UID set against one message.
func numContains(numSet imap.NumSet, seq uint32, uid uint32) bool {
	switch ns := numSet.(type) {
	case imap.SeqSet:
		return ns.Contains(seq)
	case imap.UIDSet:
		return ns.Contains(imap.UID(uid))
	}
	return false
}

// numMatches reports set membership with RFC 3501 §6.4.8 "n:*" semantics: an
// open-ended range always includes the final message of the mailbox, even
// when n exceeds the mailbox size ("5:*" with three messages is {3}).
// maxSeq/maxUID are the current message count / highest existing UID.
func numMatches(numSet imap.NumSet, seq, uid, maxSeq, maxUID uint32) bool {
	if numContains(numSet, seq, uid) {
		return true
	}
	if !numSet.Dynamic() {
		return false
	}
	switch numSet.(type) {
	case imap.UIDSet:
		return uid == maxUID
	default:
		return seq == maxSeq
	}
}

// seqSetMatches is the SeqSet form of numMatches.
func seqSetMatches(set imap.SeqSet, seq, maxSeq uint32) bool {
	if set.Contains(seq) {
		return true
	}
	return set.Dynamic() && seq == maxSeq
}

// uidSetMatches is the UIDSet form of numMatches.
func uidSetMatches(set imap.UIDSet, uid, maxUID uint32) bool {
	if set.Contains(imap.UID(uid)) {
		return true
	}
	return set.Dynamic() && uid == maxUID
}

func imapFlags(flags []string) []imap.Flag {
	out := make([]imap.Flag, 0, len(flags))
	for _, f := range flags {
		out = append(out, imap.Flag(f))
	}
	return out
}

func envelopeOf(buf []byte) *imap.Envelope {
	br := bufio.NewReader(bytes.NewReader(buf))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		// Unparseable headers (e.g. a BOM or garbage in front of the first
		// key) must degrade to an empty envelope, never a nil one: the
		// weight/cache/write path below dereferences it unconditionally.
		return &imap.Envelope{}
	}
	return imapserver.ExtractEnvelope(header)
}
