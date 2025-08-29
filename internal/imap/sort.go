// RFC 5256 SORT: filter with the search criteria (reusing the SEARCH
// matcher) and order the results by the requested keys. Subject sorting
// strips leading Re:/Fwd: prefixes; DATE uses the Date header.
//
// Performance: sort keys are extracted ONCE per message from a bounded
// header read (RFC 5256 keys are all header/metadata based), never from
// full bodies, and comparisons run over the pre-extracted values instead of
// re-parsing messages inside the O(n log n) comparator.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"mailezine/internal/imapserver"
	"mailezine/internal/mailstore"
)

// maxSortHeaderBytes bounds the header block read for sort keys; messages
// with pathological headers sort by their empty fallback keys instead of
// pinning memory.
const maxSortHeaderBytes = 256 << 10

type sortItem struct {
	seq uint32
	uid uint32
	msg *mailstore.Message
	// raw is set only when the SEARCH criteria required body access.
	raw []byte
	// keys are pre-extracted RFC 5256 sort keys.
	keys sortKeys
}

type sortKeys struct {
	sent    time.Time
	from    string
	to      string
	cc      string
	subject string
}

func (s *session) Sort(criteria []imapserver.SortCriterion, search *imap.SearchCriteria) ([]uint32, error) {
	return s.sortMessages(criteria, search, false)
}

// SortUID is the UID SORT variant (RFC 5256 §3): identical ordering, but the
// response reports UIDs instead of sequence numbers.
func (s *session) SortUID(criteria []imapserver.SortCriterion, search *imap.SearchCriteria) ([]uint32, error) {
	return s.sortMessages(criteria, search, true)
}

func (s *session) sortMessages(criteria []imapserver.SortCriterion, search *imap.SearchCriteria, uidMode bool) ([]uint32, error) {
	ctx := context.Background()
	// Sequence numbers resolve against the session snapshot (snapshotMsgs).
	msgs, err := s.snapshotMsgs(ctx)
	if err != nil {
		return nil, err
	}
	// Body access is needed for TEXT/BODY search only; the lazy body()
	// closure below serves header-only reads for header search keys
	// (HEADER, SENT*) — sort keys never touch bodies.
	needsFull := len(search.Text) > 0 || len(search.Body) > 0
	var matched []sortItem
	maxSeq := uint32(len(msgs))
	maxUID := maxSeq
	if maxSeq > 0 {
		maxUID = msgs[maxSeq-1].UID
	}
	for i, msg := range msgs {
		seq := uint32(i) + 1
		var raw []byte
		if needsFull {
			raw = s.readMessageBody(ctx, msg.UID)
		}
		full := raw
		body := func() ([]byte, error) {
			if full != nil {
				return full, nil
			}
			full = s.readMessageHeader(ctx, msg.UID)
			return full, nil
		}
		ok, err := matchSearch(msg, seq, search, maxSeq, maxUID, body)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, sortItem{seq: seq, uid: msg.UID, msg: msg, raw: raw})
		}
	}

	// Extract every sort key exactly once.
	for i := range matched {
		hdr := matched[i].raw
		if hdr == nil {
			hdr = s.readMessageHeader(ctx, matched[i].uid)
		}
		matched[i].keys = extractSortKeys(hdr)
	}

	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		for _, c := range criteria {
			cmp := sortCmp(c.Key, a, b)
			if cmp == 0 {
				continue
			}
			if c.Reverse {
				return cmp > 0
			}
			return cmp < 0
		}
		return a.seq < b.seq
	})
	out := make([]uint32, len(matched))
	for i, m := range matched {
		if uidMode {
			out[i] = m.uid
		} else {
			out[i] = m.seq
		}
	}
	return out, nil
}

// sortNeedsHeader reports whether any criterion orders by a header field.
func sortNeedsHeader(criteria []imapserver.SortCriterion) bool {
	for _, c := range criteria {
		switch c.Key {
		case imapserver.SortKeyDate, imapserver.SortKeyFrom, imapserver.SortKeyTo,
			imapserver.SortKeyCc, imapserver.SortKeySubject:
			return true
		}
	}
	return false
}

// sortCmp compares a and b for a sort key: -1, 0, or 1.
func sortCmp(key imapserver.SortKey, a, b sortItem) int {
	var less bool
	var greater bool
	switch key {
	case imapserver.SortKeyArrival:
		less = a.msg.InternalDate.Before(b.msg.InternalDate)
		greater = b.msg.InternalDate.Before(a.msg.InternalDate)
	case imapserver.SortKeySize:
		less = a.msg.Size < b.msg.Size
		greater = b.msg.Size < a.msg.Size
	case imapserver.SortKeyDate:
		less = a.keys.sent.Before(b.keys.sent)
		greater = b.keys.sent.Before(a.keys.sent)
	case imapserver.SortKeyFrom:
		less = a.keys.from < b.keys.from
		greater = b.keys.from < a.keys.from
	case imapserver.SortKeyTo:
		less = a.keys.to < b.keys.to
		greater = b.keys.to < a.keys.to
	case imapserver.SortKeyCc:
		less = a.keys.cc < b.keys.cc
		greater = b.keys.cc < a.keys.cc
	case imapserver.SortKeySubject:
		less = a.keys.subject < b.keys.subject
		greater = b.keys.subject < a.keys.subject
	}
	switch {
	case less:
		return -1
	case greater:
		return 1
	default:
		return 0
	}
}

func (s *session) readMessageBody(ctx context.Context, uid uint32) []byte {
	rc, err := s.srv.Store.OpenMessage(ctx, s.user, s.mbox, uid)
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return b
}

// readMessageHeader reads only the header block of a message (bounded): the
// bytes up to the first blank line. Sort keys and header searches never need
// the body, which can be arbitrarily large.
func (s *session) readMessageHeader(ctx context.Context, uid uint32) []byte {
	rc, err := s.srv.Store.OpenMessage(ctx, s.user, s.mbox, uid)
	if err != nil {
		return nil
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 16<<10)
	var buf []byte
	for len(buf) < maxSortHeaderBytes {
		line, err := br.ReadSlice('\n')
		buf = append(buf, line...)
		if len(buf) >= maxSortHeaderBytes {
			break
		}
		if err == io.EOF {
			break
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			break
		}
		// Blank line (CRLF or bare LF): the header block ends.
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			break
		}
	}
	return buf
}

// extractSortKeys parses the RFC 5256 sort keys from one header block.
func extractSortKeys(hdr []byte) sortKeys {
	var k sortKeys
	if len(hdr) == 0 {
		return k
	}
	msg, err := mail.ReadMessage(bytes.NewReader(hdr))
	if err != nil {
		return k
	}
	if t, err := msg.Header.Date(); err == nil {
		k.sent = t
	}
	k.from = firstAddress(msg, "From")
	k.to = firstAddress(msg, "To")
	k.cc = firstAddress(msg, "Cc")
	k.subject = baseSubject(msg.Header.Get("Subject"))
	return k
}

func firstAddress(msg *mail.Message, key string) string {
	addrs, err := msg.Header.AddressList(key)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return strings.ToLower(addrs[0].Address)
}

// baseSubject lowercases and strips LEADING Re:/Fwd:/Fw: prefixes (RFC 5256
// §2.1 base subject). Mid-string occurrences must not be touched: subjects
// like "fire: safety" would otherwise sort under a different key than they
// display.
func baseSubject(subject string) string {
	s := strings.ToLower(strings.TrimSpace(subject))
	for {
		trimmed := s
		for _, p := range []string{"re:", "fwd:", "fw:"} {
			if strings.HasPrefix(trimmed, p) {
				trimmed = trimmed[len(p):]
				break
			}
		}
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == s {
			return s
		}
		s = trimmed
	}
}
