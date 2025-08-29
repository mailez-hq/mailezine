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
	maxSeq := uint32(len(msgs))
	maxUID := maxSeq
	if maxSeq > 0 {
		maxUID = msgs[maxSeq-1].UID
	}
	for i, msg := range msgs {
		seq := uint32(i) + 1
		if ftsCandidates != nil {
			if _, ok := ftsCandidates[msg.UID]; !ok {
				continue
			}
		}
		var bodyCache []byte
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
		buf, err := body()
		if err != nil {
			return false, err
		}
		hdr := headerBlock(buf)
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
		raw := string(buf)
		for _, text := range c.Text {
			if !strings.Contains(strings.ToLower(raw), strings.ToLower(text)) {
				return false, nil
			}
		}
		bp := bodyPart(buf)
		for _, pat := range c.Body {
			if !strings.Contains(strings.ToLower(bp), strings.ToLower(pat)) {
				return false, nil
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

func bodyPart(buf []byte) string {
	if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
		return string(buf[i+4:])
	}
	return ""
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
