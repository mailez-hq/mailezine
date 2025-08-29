// RFC 5256 SORT: parse the sort keys and search criteria, delegate to the
// session, and emit the ordered sequence numbers.
package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapwire"
)

// SortKey is a SORT sort key (RFC 5256).
type SortKey string

// SORT keys.
const (
	SortKeyArrival SortKey = "ARRIVAL"
	SortKeyCc      SortKey = "CC"
	SortKeyDate    SortKey = "DATE"
	SortKeyFrom    SortKey = "FROM"
	SortKeySize    SortKey = "SIZE"
	SortKeySubject SortKey = "SUBJECT"
	SortKeyTo      SortKey = "TO"
)

// SortCriterion is one sort key with an optional REVERSE modifier.
type SortCriterion struct {
	Key     SortKey
	Reverse bool
}

// SessionSort is implemented by sessions that support SORT.
type SessionSort interface {
	Sort(criteria []SortCriterion, search *imap.SearchCriteria) ([]uint32, error)
}

// SessionSortUID is implemented by sessions that support UID SORT
// (RFC 5256 §3): identical ordering, UIDs in the response.
type SessionSortUID interface {
	SortUID(criteria []SortCriterion, search *imap.SearchCriteria) ([]uint32, error)
}

func (c *Conn) handleSort(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}
	criteria, err := readSortCriteria(dec)
	if err != nil {
		return err
	}
	if !dec.ExpectSP() {
		return dec.Err()
	}
	var charset string
	if !dec.ExpectAString(&charset) || !dec.ExpectSP() {
		return dec.Err()
	}
	if !strings.EqualFold(charset, "UTF-8") {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeBadCharset, Text: "Only UTF-8 is supported"}
	}

	var search imap.SearchCriteria
	for {
		if err := readSearchKey(&search, dec); err != nil {
			return fmt.Errorf("in sort-search-key: %w", err)
		}
		if !dec.SP() {
			break
		}
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}
	var nums []uint32
	if numKind == NumKindUID {
		sess, ok := c.session.(SessionSortUID)
		if !ok {
			return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "UID SORT not supported"}
		}
		uids, err := sess.SortUID(criteria, &search)
		if err != nil {
			return err
		}
		nums = uids
	} else {
		sess, ok := c.session.(SessionSort)
		if !ok {
			return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "SORT not supported"}
		}
		seqs, err := sess.Sort(criteria, &search)
		if err != nil {
			return err
		}
		nums = seqs
	}

	enc := newResponseEncoder(c)
	enc.Atom("*").SP().Atom("SORT")
	for _, n := range nums {
		enc.SP().Number(n)
	}
	if err := enc.CRLF(); err != nil {
		enc.end()
		return err
	}
	enc.end()
	return nil // readCommand writes the tagged OK
}

func readSortCriteria(dec *imapwire.Decoder) ([]SortCriterion, error) {
	var out []SortCriterion
	_, err := dec.List(func() error {
		var rev bool
		var name string
		if !dec.ExpectAtom(&name) {
			return dec.Err()
		}
		if strings.EqualFold(name, "REVERSE") {
			rev = true
			if !dec.ExpectSP() || !dec.ExpectAtom(&name) {
				return dec.Err()
			}
		}
		key := SortKey(strings.ToUpper(name))
		switch key {
		case SortKeyArrival, SortKeyCc, SortKeyDate, SortKeyFrom, SortKeySize, SortKeySubject, SortKeyTo:
		default:
			return fmt.Errorf("invalid sort key %q", name)
		}
		out = append(out, SortCriterion{Key: key, Reverse: rev})
		return nil
	})
	return out, err
}
