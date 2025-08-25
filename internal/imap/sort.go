// RFC 5256 SORT: filter with the search criteria (reusing the SEARCH
// matcher) and order the results by the requested keys. Subject sorting
// strips leading Re:/Fwd: prefixes; DATE uses the Date header.
package imap

import (
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

type sortItem struct {
	seq uint32
	msg *mailstore.Message
	raw []byte
}

func (s *session) Sort(criteria []imapserver.SortCriterion, search *imap.SearchCriteria) ([]uint32, error) {
	ctx := context.Background()
	msgs, err := s.srv.Store.ListMessages(ctx, s.user, s.mbox)
	if err != nil {
		return nil, err
	}
	var matched []sortItem
	for i, msg := range msgs {
		seq := uint32(i) + 1
		needsBody := len(search.Header) > 0 || len(search.Text) > 0 || len(search.Body) > 0
		var raw []byte
		if needsBody {
			raw = s.readMessageBody(ctx, msg.UID)
		}
		body := func() ([]byte, error) { return raw, nil }
		ok, err := matchSearch(msg, seq, search, body)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, sortItem{seq: seq, msg: msg, raw: raw})
		}
	}

	// Load bodies for header-based sort keys.
	for i := range matched {
		if matched[i].raw == nil {
			matched[i].raw = s.readMessageBody(ctx, matched[i].msg.UID)
		}
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
		out[i] = m.seq
	}
	return out, nil
}

// sortCmp compares a and b for a sort key: -1, 0, or 1.
func sortCmp(key imapserver.SortKey, a, b sortItem) int {
	var less bool
	switch key {
	case imapserver.SortKeyArrival:
		less = a.msg.InternalDate.Before(b.msg.InternalDate)
	case imapserver.SortKeySize:
		less = a.msg.Size < b.msg.Size
	case imapserver.SortKeyDate:
		less = sentDateOf(a.raw).Before(sentDateOf(b.raw))
	case imapserver.SortKeyFrom:
		less = addressOf(a.raw, "From") < addressOf(b.raw, "From")
	case imapserver.SortKeyTo:
		less = addressOf(a.raw, "To") < addressOf(b.raw, "To")
	case imapserver.SortKeyCc:
		less = addressOf(a.raw, "Cc") < addressOf(b.raw, "Cc")
	case imapserver.SortKeySubject:
		less = subjectOf(a.raw) < subjectOf(b.raw)
	}
	greater := false
	switch key {
	case imapserver.SortKeyArrival:
		greater = b.msg.InternalDate.Before(a.msg.InternalDate)
	case imapserver.SortKeySize:
		greater = b.msg.Size < a.msg.Size
	case imapserver.SortKeyDate:
		greater = sentDateOf(b.raw).Before(sentDateOf(a.raw))
	case imapserver.SortKeyFrom:
		greater = addressOf(b.raw, "From") < addressOf(a.raw, "From")
	case imapserver.SortKeyTo:
		greater = addressOf(b.raw, "To") < addressOf(a.raw, "To")
	case imapserver.SortKeyCc:
		greater = addressOf(b.raw, "Cc") < addressOf(a.raw, "Cc")
	case imapserver.SortKeySubject:
		greater = subjectOf(b.raw) < subjectOf(a.raw)
	}
	if less {
		return -1
	}
	if greater {
		return 1
	}
	return 0
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

func sentDateOf(data []byte) time.Time {
	msg, err := mail.ReadMessage(strings.NewReader(string(data)))
	if err != nil {
		return time.Time{}
	}
	t, err := msg.Header.Date()
	if err != nil {
		return time.Time{}
	}
	return t
}

func addressOf(data []byte, key string) string {
	msg, err := mail.ReadMessage(strings.NewReader(string(data)))
	if err != nil {
		return ""
	}
	addrs, err := msg.Header.AddressList(key)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return strings.ToLower(addrs[0].Address)
}

var subjectPrefixRe = strings.NewReplacer("re:", "", "fwd:", "", "fw:", "")

func subjectOf(data []byte) string {
	msg, err := mail.ReadMessage(strings.NewReader(string(data)))
	if err != nil {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(msg.Header.Get("Subject")))
	for {
		trimmed := strings.TrimSpace(subjectPrefixRe.Replace(s))
		if trimmed == s {
			break
		}
		s = trimmed
	}
	return s
}
