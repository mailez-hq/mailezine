// RFC 4314 ACL extension on the IMAP session: GETACL/SETACL/DELETEACL/
// MYRIGHTS/LISTRIGHTS, dispatched through the imapserver SessionExtension
// hook. Persistence goes through mailstore.ACLStore (KV or maildir); the
// owner always has full implicit rights and is never stored in the ACL.
package imap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"

	"mailezine/internal/acl"
	"mailezine/internal/imapserver"
	"mailezine/internal/imapwire"
	"mailezine/internal/mailstore"
)

// hasACL reports whether the server store provides RFC 4314 ACLs.
func (s *Server) hasACL() bool {
	_, ok := s.Store.(mailstore.ACLStore)
	return ok
}

func (s *session) aclStore() mailstore.ACLStore {
	return s.srv.Store.(mailstore.ACLStore)
}

// HandleExtension implements imapserver.SessionExtension for the ACL
// commands. All responses are untagged data lines written via w; the Conn
// completes the tagged status.
func (s *session) HandleExtension(name string, dec *imapwire.Decoder, w *imapserver.ExtensionWriter) (bool, error) {
	switch name {
	case "GETACL", "SETACL", "DELETEACL", "MYRIGHTS", "LISTRIGHTS", "GETQUOTA", "GETQUOTAROOT":
	default:
		return false, nil
	}
	if name == "GETQUOTA" || name == "GETQUOTAROOT" {
		return s.handleQuota(name, dec, w)
	}

	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) {
		return true, dec.Err()
	}

	switch name {
	case "GETACL":
		if !dec.ExpectCRLF() {
			return true, dec.Err()
		}
		return true, s.handleGetACL(mailbox, w)
	case "MYRIGHTS":
		if !dec.ExpectCRLF() {
			return true, dec.Err()
		}
		return true, s.handleMyRights(mailbox, w)
	case "SETACL", "DELETEACL":
		var identifier string
		if !dec.ExpectSP() || !dec.ExpectAString(&identifier) {
			return true, dec.Err()
		}
		if name == "DELETEACL" {
			if !dec.ExpectCRLF() {
				return true, dec.Err()
			}
			return true, s.handleDeleteACL(mailbox, identifier)
		}
		var rights string
		if !dec.ExpectSP() || !dec.ExpectAString(&rights) || !dec.ExpectCRLF() {
			return true, dec.Err()
		}
		return true, s.handleSetACL(mailbox, identifier, rights)
	case "LISTRIGHTS":
		var identifier string
		if !dec.ExpectSP() || !dec.ExpectAString(&identifier) || !dec.ExpectCRLF() {
			return true, dec.Err()
		}
		return true, s.handleListRights(mailbox, identifier, w)
	}
	return false, nil
}

func (s *session) handleGetACL(mailbox string, w *imapserver.ExtensionWriter) error {
	entries, err := s.aclEntries(context.Background(), mailbox)
	if err != nil {
		return aclError(err)
	}
	w.Atom("*").SP().Atom("ACL").SP().Mailbox(mailbox)
	for _, e := range entries {
		w.SP().String(e.Identifier).SP().Atom(e.Rights)
	}
	return w.CRLF()
}

func (s *session) handleMyRights(mailbox string, w *imapserver.ExtensionWriter) error {
	rights, err := s.myRights(context.Background(), mailbox)
	if err != nil {
		return aclError(err)
	}
	w.Atom("*").SP().Atom("MYRIGHTS").SP().Mailbox(mailbox).SP().Atom(rights)
	return w.CRLF()
}

func (s *session) handleListRights(mailbox, identifier string, w *imapserver.ExtensionWriter) error {
	ctx := context.Background()
	if err := acl.ValidateIdentifier(identifier); err != nil {
		return err
	}
	current, err := s.aclEntries(ctx, mailbox)
	if err != nil {
		return aclError(err)
	}
	granted := ""
	for _, e := range current {
		if e.Identifier == identifier {
			granted = e.Rights
			break
		}
	}
	// RFC 4314 §6.4: LISTRIGHTS lists the rights that may be granted to
	// the identifier; the last atom may be prefixed with * for implicit
	// rights. We advertise the full canonical set.
	w.Atom("*").SP().Atom("LISTRIGHTS").SP().Mailbox(mailbox).SP().String(identifier).SP()
	if granted == "" {
		w.NIL()
	} else {
		w.Atom(granted)
	}
	for i := 0; i < len(acl.Canonical); i++ {
		w.SP().Atom(string(acl.Canonical[i]))
	}
	return w.CRLF()
}

func (s *session) handleSetACL(mailbox, identifier, rights string) error {
	ctx := context.Background()
	if err := acl.ValidateIdentifier(identifier); err != nil {
		return err
	}
	// RFC 4314 §5.1: the owner always retains full rights and entries for
	// the authenticated user cannot remove them.
	if strings.EqualFold(identifier, s.user) {
		return nil
	}
	// RFC 4314 §6.3: "+rights"/"-rights" modify the identifier's current
	// rights; a bare string replaces them.
	current := ""
	raw, err := s.aclStore().GetACL(ctx, s.user, mailbox)
	if err != nil {
		return aclError(err)
	}
	if cur, ok := raw[identifier]; ok {
		current = cur
	}
	normalized, err := acl.ApplyModification(current, rights)
	if err != nil {
		return err
	}
	if err := s.aclStore().SetACL(ctx, s.user, mailbox, identifier, normalized); err != nil {
		return aclError(err)
	}
	return nil
}

func (s *session) handleDeleteACL(mailbox, identifier string) error {
	ctx := context.Background()
	if err := acl.ValidateIdentifier(identifier); err != nil {
		return err
	}
	if strings.EqualFold(identifier, s.user) {
		return nil
	}
	if err := s.aclStore().DeleteACL(ctx, s.user, mailbox, identifier); err != nil {
		return aclError(err)
	}
	return nil
}

func (s *session) aclEntries(ctx context.Context, mailbox string) ([]acl.Entry, error) {
	raw, err := s.aclStore().GetACL(ctx, s.user, mailbox)
	if err != nil {
		return nil, err
	}
	out := make([]acl.Entry, 0, len(raw))
	for id, rights := range raw {
		out = append(out, acl.Entry{Identifier: id, Rights: rights})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out, nil
}

// myRights returns the effective rights of the session user on a mailbox.
func (s *session) myRights(ctx context.Context, mailbox string) (string, error) {
	// GetACL resolves the mailbox (lazily provisioning INBOX) and fails
	// with ErrNotFound for unknown mailboxes, exactly like SELECT.
	if _, err := s.aclStore().GetACL(ctx, s.user, mailbox); err != nil {
		return "", err
	}
	return acl.Canonical, nil // owner: full implicit rights
}

func aclError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*imap.Error); ok {
		return err
	}
	if err == mailstore.ErrNotFound {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	}
	return fmt.Errorf("acl: %w", err)
}
