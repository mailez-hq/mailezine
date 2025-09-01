// Package mailstore defines the account-level mailbox operations that the
// delivery and protocol layers depend on (ARCHITECTURE.md §2-§3). The
// implementation is the KV+blob backend (Pebble/TiDB + MinIO/FS).
package mailstore

import (
	"context"
	"io"
	"strings"
	"time"
)

// Message is one copy of a delivered message.
type Message struct {
	From         string
	To           []string
	Data         []byte
	UID          uint32 // 0 = allocate the next UID
	Seen         bool
	Flags        []string // canonical IMAP flags (\Seen, \Answered, …)
	Keywords     []string
	InternalDate time.Time
	Size         int64
	ModSeq       uint64 // CONDSTORE: last change sequence of this copy
}

// Mailbox is the metadata of one mailbox as seen by IMAP.
type Mailbox struct {
	Name          string
	UIDValidity   uint32
	UIDNext       uint32
	Subscribed    bool
	Attrs         []string
	NumMessages   uint32
	NumUnseen     uint32
	NumDeleted    uint32
	Size          int64
	HighestModSeq uint64 // CONDSTORE HIGHESTMODSEQ of the mailbox
}

// Store is the mailbox surface used by delivery.
type Store interface {
	// Deliver appends one message copy to a mailbox and returns its UID.
	Deliver(ctx context.Context, account, mailbox string, msg *Message) (uint32, error)
	// QuotaUsedBytes sums the message bytes of an account.
	QuotaUsedBytes(ctx context.Context, account string) (int64, error)
}

// MailboxStore is the full mailbox surface used by IMAP (KV+blob:
// Pebble/TiDB + MinIO/FS). All UID/mailbox operations are account-scoped.
type MailboxStore interface {
	Store
	// EnsureDefaultMailboxes creates and subscribes the stack's default
	// mailboxes (Trash/Drafts/Sent/Junk) for the account.
	EnsureDefaultMailboxes(ctx context.Context, account string) error
	ListMailboxes(ctx context.Context, account string) ([]Mailbox, error)
	MailboxStatus(ctx context.Context, account, mailbox string) (Mailbox, error)
	CreateMailbox(ctx context.Context, account, mailbox string) (uint32, error)
	DeleteMailbox(ctx context.Context, account, mailbox string) error
	RenameMailbox(ctx context.Context, account, oldName, newName string) error
	SetSubscribed(ctx context.Context, account, mailbox string, subscribed bool) error
	ListMessages(ctx context.Context, account, mailbox string) ([]*Message, error)
	MessageByUID(ctx context.Context, account, mailbox string, uid uint32) (*Message, error)
	SetFlags(ctx context.Context, account, mailbox string, uid uint32, flags []string) error
	Append(ctx context.Context, account, mailbox string, msg *Message) (uint32, error)
	Expunge(ctx context.Context, account, mailbox string, uids []uint32) ([]uint32, error)
	// ExpungedSince returns the UIDs tombstoned as expunged from the
	// mailbox after the given CONDSTORE modseq (QRESYNC VANISHED (EARLIER)
	// and UID FETCH ... (CHANGEDSINCE ... VANISHED)).
	ExpungedSince(ctx context.Context, account, mailbox string, sinceModSeq uint64) ([]uint32, error)
	Copy(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error)
	Move(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error)
	OpenMessage(ctx context.Context, account, mailbox string, uid uint32) (io.ReadCloser, error)
}

// AccountPurger is the optional account-level purge surface used by the
// management API: it removes an account with all of its data so a
// control-plane user deletion does not leave orphans behind.
type AccountPurger interface {
	// DeleteAccount removes the account and every mailbox, message, blob
	// reference and counter it owns. Purging an absent account returns
	// store.ErrNotFound.
	DeleteAccount(ctx context.Context, account string) error
}

// DefaultMailboxes maps the mailez stack default mailboxes to their
// special-use attribute.
var DefaultMailboxes = map[string]string{
	"Trash":  "\\Trash",
	"Drafts": "\\Drafts",
	"Sent":   "\\Sent",
	"Junk":   "\\Junk",
}

// applyDefaultAttrs annotates the default mailboxes with their special-use
// attribute so LIST consumers can identify them regardless of backend.
func applyDefaultAttrs(boxes []Mailbox) []Mailbox {
	for i := range boxes {
		if attr, ok := DefaultMailboxes[boxes[i].Name]; ok && !containsFlag(boxes[i].Attrs, attr) {
			boxes[i].Attrs = append(boxes[i].Attrs, attr)
		}
	}
	return boxes
}

// ACLStore is the optional RFC 4314 mailbox access control surface. The
// IMAP layer type-asserts the MailboxStore for ACL support; the owner of a
// mailbox always has full implicit rights (RFC 4314 §5.1) and is never
// stored in the ACL.
type ACLStore interface {
	// GetACL returns the ACL entries of one mailbox, identifier → right
	// string (canonical order).
	GetACL(ctx context.Context, account, mailbox string) (map[string]string, error)
	// SetACL replaces the rights of identifier, or removes the entry when
	// rights is empty.
	SetACL(ctx context.Context, account, mailbox, identifier, rights string) error
	// DeleteACL removes every right of identifier.
	DeleteACL(ctx context.Context, account, mailbox, identifier string) error
}

// VacationStateStore persists the per-sender last auto-reply time so the
// RFC 5230 :days throttle survives restarts.
type VacationStateStore interface {
	VacationLastSent(ctx context.Context, account, sender string) (time.Time, error)
	SetVacationLastSent(ctx context.Context, account, sender string, t time.Time) error
}

// CanonicalFlags are the IMAP system flags understood by the store.
var CanonicalFlags = []string{
	"\\Answered", "\\Flagged", "\\Deleted", "\\Seen", "\\Draft",
}

// normalizeFlags folds the legacy Seen flag into the canonical \Seen flag.
func normalizeFlags(flags []string, seen bool) []string {
	out := append([]string(nil), flags...)
	if seen && !containsFlag(out, "\\Seen") {
		out = append(out, "\\Seen")
	}
	return out
}

func containsFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// HasFlag reports whether a flag is present (IMAP flags are
// case-insensitive keywords).
func HasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}
