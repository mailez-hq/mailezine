// SEARCH implementation: a bounded but client-compatible subset
// (seq/uid/date/flags/size/header/text/body + NOT/OR nesting).
package imap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapserver"

	"mailezine/internal/mailstore"
)

// errVanished marks a message whose body is no longer readable (deleted
// under us / dangling index entry): the message is skipped from results
// instead of failing the whole SEARCH.
var errVanished = errors.New("imap: message vanished")

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	return s.searchImpl(kind, criteria, options, 0)
}

// SearchModSeq serves RFC 7162 §3.1.6 "SEARCH MODSEQ n": messages whose
// mod-sequence is greater than or equal to n AND which match criteria.
func (s *session) SearchModSeq(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions, modSeq uint64) (*imap.SearchData, error) {
	return s.searchImpl(kind, criteria, options, modSeq)
}

func (s *session) searchImpl(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions, modSeqAtLeast uint64) (*imap.SearchData, error) {
	ctx := context.Background()
	// Sequence numbers resolve against the session snapshot (snapshotMsgs)
	// so reported seqs match the client's view.
	msgs, err := s.snapshotMsgs(ctx)
	if err != nil {
		return nil, err
	}
	// Full-text candidates: when an index is available, restrict the scan
	// to indexed UIDs (word/prefix semantics, verified below against raw
	// bytes). An index error/absence falls back to the full scan.
	var ftsCandidates map[uint32]struct{}
	if s.srv.FTS != nil && len(criteria.Text) > 0 {
		if c, err := s.srv.FTS.SearchText(ctx, s.user, s.mbox, criteria.Text); err == nil && c != nil {
			ftsCandidates = c
		} else if err != nil {
			s.srv.Logger.Debug("imap: fts search fallback to scan", "err", err)
		}
	}
	data := &imap.SearchData{}
	var allSeq imap.SeqSet
	var allUID imap.UIDSet
	// A SEARCH that needs the raw message reads one blob per candidate: on a
	// 500-message mailbox that measured ~90ms each (45s total). Prefetch the
	// candidates in parallel under a byte budget; the sequential pass below
	// stays the only place that decides matches, and anything over budget is
	// still read lazily, so results are unchanged and only wall-clock drops.
	//
	// Header conditions (SUBJECT/FROM/HEADER/…, and the sent-date range) need
	// only the header block, which the store caches at delivery — those
	// searches read no candidate bodies at all, and only fall back to the
	// blob for the messages that have no cached block. TEXT is answered by
	// the block too whenever it hits inside it (see matchSearch), which is the
	// usual case for a keyword search: without that, every hit's body is read
	// just to confirm a subject match.
	bodyOnly := len(criteria.Body) > 0
	needsBody := len(criteria.Text) > 0 || bodyOnly
	needsHeader := len(criteria.Header) > 0 || !criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero()
	var prefetched map[uint32][]byte
	if needsBody || needsHeader {
		cands := make([]uint32, 0, len(msgs))
		for _, msg := range msgs {
			if modSeqAtLeast != 0 && msg.ModSeq < modSeqAtLeast {
				continue
			}
			if ftsCandidates != nil {
				if _, ok := ftsCandidates[msg.UID]; !ok {
					continue
				}
			}
			// Nothing to read when the cached header block already answers
			// everything this search needs from the message.
			headerAnswers := len(criteria.Text) == 0 || textInHeaderBlock(msg.Head, criteria.Text)
			if !bodyOnly && len(msg.Head) > 0 && headerAnswers {
				continue
			}
			cands = append(cands, msg.UID)
		}
		if len(cands) > 0 {
			prefetched = s.prefetchBodies(ctx, cands)
		}
	}
	maxSeq := uint32(len(msgs))
	maxUID := maxSeq
	if maxSeq > 0 {
		maxUID = msgs[maxSeq-1].UID
	}
	for i, msg := range msgs {
		seq := uint32(i) + 1
		if modSeqAtLeast != 0 && msg.ModSeq < modSeqAtLeast {
			continue
		}
		if ftsCandidates != nil {
			if _, ok := ftsCandidates[msg.UID]; !ok {
				continue
			}
		}
		var bodyCache []byte
		if prefetched != nil {
			bodyCache = prefetched[msg.UID]
		}
		body := func() ([]byte, error) {
			if bodyCache != nil {
				return bodyCache, nil
			}
			rc, err := s.srv.Store.OpenMessage(ctx, s.user, s.mbox, msg.UID)
			if err != nil {
				if errors.Is(err, mailstore.ErrNotFound) {
					return nil, errVanished
				}
				return nil, err
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				return nil, err
			}
			bodyCache = b
			return b, nil
		}
		ok, err := matchSearch(msg, seq, criteria, maxSeq, maxUID, body)
		if err != nil {
			if errors.Is(err, errVanished) {
				continue
			}
			return nil, err
		}
		if !ok {
			continue
		}
		allUID.AddNum(imap.UID(msg.UID))
		var num uint32
		switch kind {
		case imapserver.NumKindSeq:
			allSeq.AddNum(seq)
			num = seq
		case imapserver.NumKindUID:
			num = msg.UID
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if data.Max == 0 || num > data.Max {
			data.Max = num
		}
		data.Count++
	}
	switch kind {
	case imapserver.NumKindSeq:
		data.All = allSeq
	case imapserver.NumKindUID:
		data.All = allUID
	}
	return data, nil
}

// prefetchBodies loads candidate message bodies concurrently, bounded by a
// worker count and a total byte budget. A message that fails to load is simply
// absent from the map and is read lazily during evaluation.
func (s *session) prefetchBodies(ctx context.Context, uids []uint32) map[uint32][]byte {
	const workers = 8
	const byteBudget = 64 << 20
	out := make(map[uint32][]byte, len(uids))
	if len(uids) == 0 {
		return out
	}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		budget atomic.Int64
		sem    = make(chan struct{}, workers)
	)
	for _, uid := range uids {
		wg.Add(1)
		go func(uid uint32) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if budget.Load() > byteBudget {
				return
			}
			rc, err := s.srv.Store.OpenMessage(ctx, s.user, s.mbox, uid)
			if err != nil {
				return
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				return
			}
			budget.Add(int64(len(b)))
			mu.Lock()
			out[uid] = b
			mu.Unlock()
		}(uid)
	}
	wg.Wait()
	return out
}

// matchSearch evaluates c against one message. maxSeq/maxUID carry the
// mailbox size for RFC 3501 §6.4.8 "n:*" semantics (an open-ended range
// always includes the final message).
func matchSearch(msg *mailstore.Message, seq uint32, c *imap.SearchCriteria, maxSeq, maxUID uint32, body func() ([]byte, error)) (bool, error) {
	for _, seqSet := range c.SeqNum {
		if seq == 0 || !seqSetMatches(seqSet, seq, maxSeq) {
			return false, nil
		}
	}
	for _, uidSet := range c.UID {
		if !uidSetMatches(uidSet, msg.UID, maxUID) {
			return false, nil
		}
	}
	if !matchDate(msg.InternalDate, c.Since, c.Before) {
		return false, nil
	}
	for _, f := range c.Flag {
		if !mailstore.HasFlag(msg.Flags, string(f)) {
			return false, nil
		}
	}
	for _, f := range c.NotFlag {
		if mailstore.HasFlag(msg.Flags, string(f)) {
			return false, nil
		}
	}
	if c.Larger != 0 && msg.Size <= c.Larger {
		return false, nil
	}
	if c.Smaller != 0 && msg.Size >= c.Smaller {
		return false, nil
	}
	if len(c.Header) > 0 || len(c.Text) > 0 || len(c.Body) > 0 || !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		// The header block the store cached at delivery answers every header
		// condition (and the sent-date range) without a blob read.
		hdr := msg.Head
		if len(hdr) == 0 {
			b, err := body()
			if err != nil {
				return false, err
			}
			hdr = headerBlock(b)
		}
		for _, h := range c.Header {
			if !matchHeader(hdr, h.Key, h.Value) {
				return false, nil
			}
		}
		if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
			t, err := sentDate(hdr)
			if err != nil {
				return false, nil
			}
			if !matchDate(t, c.SentSince, c.SentBefore) {
				return false, nil
			}
		}
		// TEXT matches anywhere in the message and the header block is a
		// prefix of it, so a hit inside the block is conclusive: the body is
		// read only when some pattern is not already in the header. BODY
		// always needs the raw message.
		needRaw := len(c.Body) > 0 || (len(c.Text) > 0 && !textInHeaderBlock(hdr, c.Text))
		if needRaw {
			b, err := body()
			if err != nil {
				return false, err
			}
			// A message sent in GBK, Big5 or Shift_JIS carries the query in
			// its own bytes, and a base64 part carries no readable text at
			// all: matching the raw buffer alone misses both. Match the
			// message decoded to UTF-8 instead. (A header hit already returned
			// above; this is the body's turn.)
			if len(c.Text) > 0 {
				text := strings.ToLower(imapserver.MessageText(b))
				for _, want := range c.Text {
					if !strings.Contains(text, strings.ToLower(want)) {
						return false, nil
					}
				}
			}
			if len(c.Body) > 0 {
				bodyText := strings.ToLower(imapserver.MessageBodyText(b))
				for _, pat := range c.Body {
					if !strings.Contains(bodyText, strings.ToLower(pat)) {
						return false, nil
					}
				}
			}
		}
	}
	for _, not := range c.Not {
		ok, err := matchSearch(msg, seq, &not, maxSeq, maxUID, body)
		if err != nil || ok {
			return false, err
		}
	}
	for _, or := range c.Or {
		a, err := matchSearch(msg, seq, &or[0], maxSeq, maxUID, body)
		if err != nil {
			return false, err
		}
		b, err := matchSearch(msg, seq, &or[1], maxSeq, maxUID, body)
		if err != nil {
			return false, err
		}
		if !a && !b {
			return false, nil
		}
	}
	return true, nil
}

func matchDate(t, since, before time.Time) bool {
	// RFC 3501: date comparison is zone-unaware.
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}

func headerBlock(buf []byte) []byte {
	if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
		return buf[:i]
	}
	return buf
}

// textInHeaderBlock reports whether every TEXT pattern already appears in the
// cached header block. A hit there is conclusive for TEXT (which matches
// anywhere in the message, and the block is a prefix of it), so the caller can
// skip reading the body. An absent block means "unknown", never "no match".
func textInHeaderBlock(head []byte, texts []string) bool {
	if len(head) == 0 || len(texts) == 0 {
		return false
	}
	lower := strings.ToLower(string(head))
	for _, t := range texts {
		if !strings.Contains(lower, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

func matchHeader(block []byte, key, pattern string) bool {
	key = strings.ToLower(key) + ":"
	pattern = strings.ToLower(pattern)
	// Fold continuations: accumulate lines while the next line starts with
	// whitespace.
	lines := strings.Split(string(block), "\r\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.ToLower(lines[i]), key) {
			continue
		}
		value := strings.TrimPrefix(lines[i][len(key):], " ")
		for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t")) {
			i++
			value += " " + strings.TrimSpace(lines[i])
		}
		if pattern == "" || strings.Contains(strings.ToLower(value), pattern) {
			return true
		}
	}
	return false
}

func sentDate(hdr []byte) (time.Time, error) {
	for _, line := range strings.Split(string(hdr), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "date:") {
			return mail.ParseDate(strings.TrimSpace(line[len("date:"):]))
		}
	}
	return time.Time{}, errors.New("no date header")
}
