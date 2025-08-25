//go:build unix

// RFC 4314 ACL persistence on the maildir backend: entries live in the
// rebuildable account sidecar (`.mailezine/keywords.json`, mailbox-scoped),
// where derived metadata is recoverable and messages/UIDs are the source
// of truth.
package mailstore

import (
	"context"
	"path/filepath"

	maildirpkg "mailezine/internal/store/maildir"
)

var _ ACLStore = (*Maildir)(nil)

// GetACL returns the ACL entries of one mailbox (identifier → rights).
func (m *Maildir) GetACL(ctx context.Context, account, mailbox string) (map[string]string, error) {
	mb, err := m.openMailbox(account, mailbox)
	if err != nil {
		return nil, err
	}
	return mb.ACL()
}

// SetACL replaces the rights of identifier, or removes the entry when
// rights is empty.
func (m *Maildir) SetACL(ctx context.Context, account, mailbox, identifier, rights string) error {
	mb, err := m.openMailbox(account, mailbox)
	if err != nil {
		return err
	}
	return mb.SetACL(identifier, rights)
}

// DeleteACL removes every right of identifier.
func (m *Maildir) DeleteACL(ctx context.Context, account, mailbox, identifier string) error {
	return m.SetACL(ctx, account, mailbox, identifier, "")
}

func (m *Maildir) openMailbox(account, mailbox string) (*maildirpkg.Mailbox, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	return acct.OpenMailbox(mailbox)
}
