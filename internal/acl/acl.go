// Package acl implements the RFC 4314 access control model for mailboxes:
// the canonical right set, right-string parsing/normalization (including
// "+rights"/"-rights" modifications) and identifier validation. The
// protocol layer (IMAP) and the storage layer (mailstore) share this model,
// so the wire representation and the persisted form can never diverge.
package acl

import (
	"fmt"
	"strings"
)

// Rights are the RFC 4314 section 4 access rights, in canonical order.
const (
	Lookup     = 'l' // mailbox visible in LIST, selectable
	Read       = 'r' // message data may be read
	Seen       = 's' // \Seen flag may be set/cleared
	Write      = 'w' // flags and keywords may be changed
	Insert     = 'i' // messages may be appended
	Post       = 'p' // messages may be posted to the mailbox
	Create     = 'k' // sub-mailboxes may be created
	Mailbox    = 'x' // mailbox may be deleted/renamed
	Delete     = 't' // \Deleted flag may be set
	Expunge    = 'e' // expunge may be performed
	Administer = 'a' // SETACL/DELETEACL may be performed
)

// Canonical is every right in RFC 4314 order; the owner of a mailbox always
// holds it (implicit rights, section 5.1).
const Canonical = "lrswipkxtecda"

// ValidateIdentifier reports whether id is a valid RFC 4314 identifier
// (an astring). Atoms containing atom-specials and strings with control
// characters are rejected so they cannot corrupt responses.
func ValidateIdentifier(id string) error {
	if id == "" {
		return fmt.Errorf("acl: empty identifier")
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`(){%*"\]`, r) {
			return fmt.Errorf("acl: invalid identifier %q", id)
		}
	}
	return nil
}

// Normalize validates rights and returns them in canonical order with
// duplicates removed.
func Normalize(rights string) (string, error) {
	var present [len(Canonical)]bool
	for _, r := range rights {
		idx := strings.IndexRune(Canonical, r)
		if idx < 0 {
			return "", fmt.Errorf("acl: unknown right %q", r)
		}
		present[idx] = true
	}
	var out strings.Builder
	for i := range Canonical {
		if present[i] {
			out.WriteByte(Canonical[i])
		}
	}
	return out.String(), nil
}

// ApplyModification applies one RFC 4314 rights modification (an
// astring possibly prefixed with + or -) to current, returning the
// resulting canonical right string.
func ApplyModification(current, mod string) (string, error) {
	switch {
	case mod == "":
		return "", nil
	case mod[0] == '+':
		add, err := Normalize(mod[1:])
		if err != nil {
			return "", err
		}
		cur, err := Normalize(current)
		if err != nil {
			return "", err
		}
		return Normalize(cur + add)
	case mod[0] == '-':
		remove, err := Normalize(mod[1:])
		if err != nil {
			return "", err
		}
		cur, err := Normalize(current)
		if err != nil {
			return "", err
		}
		for _, r := range remove {
			cur = strings.ReplaceAll(cur, string(r), "")
		}
		return cur, nil
	default:
		return Normalize(mod)
	}
}

// Entry is one ACL grant: identifier → right string.
type Entry struct {
	Identifier string `json:"identifier"`
	Rights     string `json:"rights"`
}

// Has reports whether the rights string grants r.
func Has(rights string, r rune) bool {
	return strings.ContainsRune(rights, r)
}
