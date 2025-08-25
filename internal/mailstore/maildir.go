//go:build unix

// Maildir is a mailstore over the Maildir++ backend (POSIX-only; see
// DECISIONS.md D7). The IMAP surface maps system flags to the ":2," info
// section; \Deleted and keywords live in the rebuildable sidecar
// (the uidlist carries no per-message flags in our v3 subset).
package mailstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"time"

	"mailezine/internal/store"
	maildirpkg "mailezine/internal/store/maildir"
)

// Maildir implements MailboxStore over maildir accounts rooted at root
// (e.g. /mail). One instance owns the root: single-writer per account.
type Maildir struct {
	root string
}

// NewMaildir opens a maildir-based mailstore.
func NewMaildir(root string) *Maildir {
	return &Maildir{root: root}
}

func (m *Maildir) Deliver(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return 0, err
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return 0, err
	}
	flags, keywords := maildirFlags(msg.Flags, msg.Seen)
	keywords = append(keywords, msg.Keywords...)
	date := msg.InternalDate
	if date.IsZero() {
		date = time.Now()
	}
	uid, err := mb.Append(bytes.NewReader(msg.Data), flags, date)
	if err != nil {
		return 0, err
	}
	if len(keywords) > 0 {
		if err := mb.SetKeywords(uid, keywords); err != nil {
			return 0, err
		}
	}
	modseq, err := acct.BumpMailboxModSeq(mailbox)
	if err != nil {
		return 0, err
	}
	if err := acct.SetMessageModSeq(mailbox, uid, modseq); err != nil {
		return 0, err
	}
	return uid, nil
}

func (m *Maildir) QuotaUsedBytes(ctx context.Context, account string) (int64, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return 0, err
	}
	return acct.QuotaBytes()
}

func (m *Maildir) ListMailboxes(ctx context.Context, account string) ([]Mailbox, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	// INBOX always exists (RFC 3501): ensure the maildir structure.
	if _, err := acct.OpenMailbox("INBOX"); err != nil {
		return nil, err
	}
	names, err := acct.Mailboxes()
	if err != nil {
		return nil, err
	}
	if err := m.EnsureDefaultMailboxes(ctx, account); err != nil {
		return nil, err
	}
	names, err = acct.Mailboxes()
	if err != nil {
		return nil, err
	}
	out := make([]Mailbox, 0, len(names))
	for _, name := range names {
		mb, err := m.MailboxStatus(ctx, account, name)
		if err != nil {
			return nil, err
		}
		mb.Subscribed, _ = acct.Subscribed(name)
		out = append(out, mb)
	}
	return applyDefaultAttrs(out), nil
}

// EnsureDefaultMailboxes creates and subscribes Trash/Drafts/Sent/Junk
// (OpenMailbox creates the directory; subscription lives in the sidecar).
func (m *Maildir) EnsureDefaultMailboxes(ctx context.Context, account string) error {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return err
	}
	for name := range DefaultMailboxes {
		if _, err := acct.OpenMailbox(name); err != nil {
			return err
		}
		sub, err := acct.Subscribed(name)
		if err != nil {
			return err
		}
		if !sub {
			if err := acct.SetSubscribed(name, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Maildir) MailboxStatus(ctx context.Context, account, mailbox string) (Mailbox, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return Mailbox{}, err
	}
	if !acct.MailboxExists(mailbox) {
		return Mailbox{}, store.ErrNotFound
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return Mailbox{}, err
	}
	validity, err := mb.UIDValidity()
	if err != nil {
		return Mailbox{}, err
	}
	uidNext, err := mb.UIDNext()
	if err != nil {
		return Mailbox{}, err
	}
	sub, _ := acct.Subscribed(mailbox)
	highest, _ := acct.MailboxModSeq(mailbox)
	out := Mailbox{
		Name:          mailbox,
		UIDValidity:   validity,
		UIDNext:       uidNext,
		Subscribed:    sub,
		HighestModSeq: highest,
	}
	msgs, err := mb.Messages()
	if err != nil {
		return Mailbox{}, err
	}
	for _, msg := range msgs {
		out.NumMessages++
		out.Size += msg.Size
		kws, _ := mb.Keywords(msg.UID)
		flags := imapFlags(msg.Flags, kws)
		if !HasFlag(flags, "\\Seen") {
			out.NumUnseen++
		}
		if HasFlag(flags, "\\Deleted") {
			out.NumDeleted++
		}
	}
	return out, nil
}

func (m *Maildir) CreateMailbox(ctx context.Context, account, mailbox string) (uint32, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return 0, err
	}
	if acct.MailboxExists(mailbox) {
		return 0, store.ErrExists
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return 0, err
	}
	return mb.UIDValidity()
}

func (m *Maildir) DeleteMailbox(ctx context.Context, account, mailbox string) error {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return err
	}
	if !acct.MailboxExists(mailbox) {
		return store.ErrNotFound
	}
	return acct.DeleteMailbox(mailbox)
}

func (m *Maildir) RenameMailbox(ctx context.Context, account, oldName, newName string) error {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return err
	}
	if !acct.MailboxExists(oldName) {
		return store.ErrNotFound
	}
	if acct.MailboxExists(newName) {
		return store.ErrExists
	}
	if err := acct.RenameMailbox(oldName, newName); err != nil {
		return mapMaildirErr(err)
	}
	return nil
}

func (m *Maildir) SetSubscribed(ctx context.Context, account, mailbox string, subscribed bool) error {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return err
	}
	return acct.SetSubscribed(mailbox, subscribed)
}

func (m *Maildir) ListMessages(ctx context.Context, account, mailbox string) ([]*Message, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return nil, err
	}
	msgs, err := mb.Messages()
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(msgs))
	for _, msg := range msgs {
		modseq, _ := acct.MessageModSeq(mailbox, msg.UID)
		kws, _ := mb.Keywords(msg.UID)
		out = append(out, &Message{
			UID:          msg.UID,
			Flags:        imapFlags(msg.Flags, kws),
			Keywords:     kws,
			InternalDate: msg.InternalDate,
			Size:         msg.Size,
			ModSeq:       modseq,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

func (m *Maildir) MessageByUID(ctx context.Context, account, mailbox string, uid uint32) (*Message, error) {
	msgs, err := m.ListMessages(ctx, account, mailbox)
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		if msg.UID == uid {
			return msg, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *Maildir) SetFlags(ctx context.Context, account, mailbox string, uid uint32, flags []string) error {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return err
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return err
	}
	system, keywords := maildirFlags(flags, false)
	if err := mb.SetFlags(uid, system); err != nil {
		return mapMaildirErr(err)
	}
	if err := mb.SetKeywords(uid, keywords); err != nil {
		return mapMaildirErr(err)
	}
	modseq, err := acct.BumpMailboxModSeq(mailbox)
	if err != nil {
		return err
	}
	return acct.SetMessageModSeq(mailbox, uid, modseq)
}

func (m *Maildir) Append(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	return m.Deliver(ctx, account, mailbox, msg)
}

func (m *Maildir) Expunge(ctx context.Context, account, mailbox string, uids []uint32) ([]uint32, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return nil, err
	}
	msgs, err := mb.Messages()
	if err != nil {
		return nil, err
	}
	uidFilter := map[uint32]struct{}{}
	for _, u := range uids {
		uidFilter[u] = struct{}{}
	}
	var deleted []uint32
	for _, msg := range msgs {
		if len(uidFilter) > 0 {
			if _, ok := uidFilter[msg.UID]; !ok {
				continue
			}
		} else {
			kws, _ := mb.Keywords(msg.UID)
			if !HasFlag(imapFlags(msg.Flags, kws), "\\Deleted") {
				continue
			}
		}
		if err := mb.Delete(msg.UID); err != nil {
			return deleted, mapMaildirErr(err)
		}
		deleted = append(deleted, msg.UID)
	}
	if len(deleted) > 0 {
		if _, err := acct.BumpMailboxModSeq(mailbox); err != nil {
			return deleted, err
		}
	}
	sort.Slice(deleted, func(i, j int) bool { return deleted[i] < deleted[j] })
	return deleted, nil
}

func (m *Maildir) Copy(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	srcMB, err := acct.OpenMailbox(src)
	if err != nil {
		return nil, err
	}
	if !acct.MailboxExists(dst) {
		return nil, store.ErrNotFound
	}
	dstMB, err := acct.OpenMailbox(dst)
	if err != nil {
		return nil, err
	}
	uidFilter := map[uint32]struct{}{}
	for _, u := range uids {
		uidFilter[u] = struct{}{}
	}
	msgs, err := srcMB.Messages()
	if err != nil {
		return nil, err
	}
	mapping := map[uint32]uint32{}
	for _, msg := range msgs {
		if len(uidFilter) > 0 {
			if _, ok := uidFilter[msg.UID]; !ok {
				continue
			}
		}
		rc, err := srcMB.Open(msg.UID)
		if err != nil {
			return mapping, err
		}
		newUID, err := dstMB.Append(rc, msg.Flags, msg.InternalDate)
		_ = rc.Close()
		if err != nil {
			return mapping, err
		}
		kws, _ := srcMB.Keywords(msg.UID)
		if len(kws) > 0 {
			_ = dstMB.SetKeywords(newUID, kws)
		}
		modseq, err := acct.BumpMailboxModSeq(dst)
		if err != nil {
			return mapping, err
		}
		if err := acct.SetMessageModSeq(dst, newUID, modseq); err != nil {
			return mapping, err
		}
		mapping[msg.UID] = newUID
	}
	return mapping, nil
}

func (m *Maildir) Move(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	srcMB, err := acct.OpenMailbox(src)
	if err != nil {
		return nil, err
	}
	if !acct.MailboxExists(dst) {
		return nil, store.ErrNotFound
	}
	dstMB, err := acct.OpenMailbox(dst)
	if err != nil {
		return nil, err
	}
	uidFilter := map[uint32]struct{}{}
	for _, u := range uids {
		uidFilter[u] = struct{}{}
	}
	msgs, err := srcMB.Messages()
	if err != nil {
		return nil, err
	}
	mapping := map[uint32]uint32{}
	for _, msg := range msgs {
		if len(uidFilter) > 0 {
			if _, ok := uidFilter[msg.UID]; !ok {
				continue
			}
		}
		// Append a copy to learn the destination UID, then remove the
		// source (same observable outcome as Mailbox.Move).
		rc, err := srcMB.Open(msg.UID)
		if err != nil {
			return mapping, err
		}
		newUID, err := dstMB.Append(rc, msg.Flags, msg.InternalDate)
		_ = rc.Close()
		if err != nil {
			return mapping, err
		}
		kws, _ := srcMB.Keywords(msg.UID)
		if len(kws) > 0 {
			_ = dstMB.SetKeywords(newUID, kws)
		}
		if err := srcMB.Delete(msg.UID); err != nil {
			return mapping, mapMaildirErr(err)
		}
		mapping[msg.UID] = newUID
	}
	if len(mapping) > 0 {
		if _, err := acct.BumpMailboxModSeq(src); err != nil {
			return mapping, err
		}
	}
	return mapping, nil
}

func (m *Maildir) OpenMessage(ctx context.Context, account, mailbox string, uid uint32) (io.ReadCloser, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return nil, err
	}
	mb, err := acct.OpenMailbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mb.Open(uid)
}

// --- SieveStore (sidecar) ---

func (m *Maildir) sieveAccount(ctx context.Context, account string) (*maildirpkg.Account, error) {
	return maildirpkg.OpenAccount(filepath.Join(m.root, account))
}

func (m *Maildir) ListSieveScripts(ctx context.Context, account string) ([]SieveScriptMeta, error) {
	acct, err := m.sieveAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	scripts, err := acct.ListSieveScripts()
	if err != nil {
		return nil, err
	}
	var out []SieveScriptMeta
	for name, active := range scripts {
		out = append(out, SieveScriptMeta{Name: name, Active: active})
	}
	return out, nil
}

func (m *Maildir) GetSieveScript(ctx context.Context, account, name string) (string, error) {
	acct, err := m.sieveAccount(ctx, account)
	if err != nil {
		return "", err
	}
	content, err := acct.GetSieveScript(name)
	return content, mapMaildirErr(err)
}

func (m *Maildir) PutSieveScript(ctx context.Context, account, name, content string, activate bool) error {
	acct, err := m.sieveAccount(ctx, account)
	if err != nil {
		return err
	}
	return acct.PutSieveScript(name, content, activate)
}

func (m *Maildir) SetSieveActive(ctx context.Context, account, name string) error {
	acct, err := m.sieveAccount(ctx, account)
	if err != nil {
		return err
	}
	return mapMaildirErr(acct.SetSieveActive(name))
}

func (m *Maildir) DeleteSieveScript(ctx context.Context, account, name string) error {
	acct, err := m.sieveAccount(ctx, account)
	if err != nil {
		return err
	}
	return mapMaildirErr(acct.DeleteSieveScript(name))
}

// mapMaildirErr normalizes the backend's sentinel errors to the store
// contract so IMAP/POP3/ManageSieve get one consistent error language.
func mapMaildirErr(err error) error {
	if errors.Is(err, maildirpkg.ErrNotFound) {
		return store.ErrNotFound
	}
	return err
}

// Flag mapping between the maildir ":2," info section and IMAP system
// flags. \Deleted has no maildir letter and lives in the sidecar keywords.
var imapFromMaildir = map[maildirpkg.Flag]string{
	maildirpkg.FlagDraft:   "\\Draft",
	maildirpkg.FlagFlagged: "\\Flagged",
	maildirpkg.FlagPassed:  "\\Passed",
	maildirpkg.FlagReplied: "\\Answered",
	maildirpkg.FlagSeen:    "\\Seen",
	maildirpkg.FlagTrashed: "\\Trashed",
}

func imapFlags(flags []maildirpkg.Flag, keywords []string) []string {
	out := append([]string(nil), keywords...)
	for _, f := range flags {
		if s, ok := imapFromMaildir[f]; ok {
			out = append(out, s)
		}
	}
	return out
}

// MaildirToIMAPFlags converts maildir ":2," system flags to IMAP flag
// strings (used by migration; \Deleted has no maildir letter and lives in
// the sidecar keywords).
func MaildirToIMAPFlags(flags []maildirpkg.Flag) []string {
	return imapFlags(flags, nil)
}

var maildirFromIMAP = map[string]maildirpkg.Flag{
	"\\Draft":    maildirpkg.FlagDraft,
	"\\Flagged":  maildirpkg.FlagFlagged,
	"\\Passed":   maildirpkg.FlagPassed,
	"\\Answered": maildirpkg.FlagReplied,
	"\\Seen":     maildirpkg.FlagSeen,
	"\\Trashed":  maildirpkg.FlagTrashed,
}

func maildirFlags(flags []string, seen bool) (system []maildirpkg.Flag, keywords []string) {
	for _, f := range flags {
		if fl, ok := maildirFromIMAP[f]; ok {
			system = append(system, fl)
		} else {
			keywords = append(keywords, f)
		}
	}
	if seen && !HasFlag(flags, "\\Seen") {
		system = append(system, maildirpkg.FlagSeen)
	}
	return system, keywords
}

var _ MailboxStore = (*Maildir)(nil)
var _ SieveStore = (*Maildir)(nil)
