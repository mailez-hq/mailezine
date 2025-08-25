//go:build unix

package maildir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Account is one mailbox account rooted at a maildir directory
// (corresponding to /mail/%u in the mailez deployment).
type Account struct {
	root string
	mu   sync.Mutex // serializes sidecar read-modify-write
}

// OpenAccount opens (creating if needed) an account directory.
func OpenAccount(root string) (*Account, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Account{root: root}, nil
}

// Mailboxes lists mailbox names, INBOX first, in sorted order.
func (a *Account) Mailboxes() ([]string, error) {
	var names []string
	if isMaildirDir(a.root) {
		names = append(names, "INBOX")
	}
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if isMaildirDir(filepath.Join(a.root, e.Name())) {
			names = append(names, nameFromDir(e.Name()))
		}
	}
	sort.Strings(names[1:])
	return names, nil
}

// OpenMailbox returns the mailbox, creating its directory structure on
// first use.
func (a *Account) OpenMailbox(name string) (*Mailbox, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dir := mailboxDir(a.root, name)
	if err := ensureMaildir(dir); err != nil {
		return nil, err
	}
	return &Mailbox{acct: a, name: name, dir: dir}, nil
}

// MailboxExists reports whether a mailbox directory exists on disk (lazy
// OpenMailbox creates it; existence checks need the on-disk truth).
func (a *Account) MailboxExists(name string) bool {
	if err := ValidateName(name); err != nil {
		return false
	}
	return isMaildirDir(mailboxDir(a.root, name))
}

// DeleteMailbox removes a mailbox and its contents. INBOX cannot be
// deleted; the target path is validated to stay under the account root
// before any removal.
func (a *Account) DeleteMailbox(name string) error {
	if name == "INBOX" {
		return errors.New("maildir: INBOX cannot be deleted")
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	dir := mailboxDir(a.root, name)
	root, err := filepath.Abs(a.root)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("maildir: mailbox path escapes account root")
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return a.withSidecar(func(s *sidecarData) error {
		delete(s.UIDNext, name)
		delete(s.Keywords, name)
		delete(s.Subscriptions, name)
		return nil
	})
}

// RenameMailbox renames a mailbox directory and migrates its sidecar state
// (UID counter, keywords, subscription). INBOX cannot be renamed.
func (a *Account) RenameMailbox(oldName, newName string) error {
	if oldName == "INBOX" {
		return errors.New("maildir: INBOX cannot be renamed")
	}
	if err := ValidateName(oldName); err != nil {
		return err
	}
	if err := ValidateName(newName); err != nil {
		return err
	}
	oldDir := mailboxDir(a.root, oldName)
	newDir := mailboxDir(a.root, newName)
	if !isMaildirDir(oldDir) {
		return ErrNotFound
	}
	if isMaildirDir(newDir) {
		return ErrMailboxExists
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		return err
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	return a.withSidecar(func(s *sidecarData) error {
		if next, ok := s.UIDNext[oldName]; ok {
			s.UIDNext[newName] = next
			delete(s.UIDNext, oldName)
		}
		if kws, ok := s.Keywords[oldName]; ok {
			s.Keywords[newName] = kws
			delete(s.Keywords, oldName)
		}
		if sub, ok := s.Subscriptions[oldName]; ok {
			s.Subscriptions[newName] = sub
			delete(s.Subscriptions, oldName)
		}
		if acl, ok := s.ACL[oldName]; ok {
			s.ACL[newName] = acl
			delete(s.ACL, oldName)
		}
		return nil
	})
}

// Subscribed reports the LSUB subscription state of a mailbox (INBOX is
// subscribed by default).
func (a *Account) Subscribed(name string) (bool, error) {
	var sub bool
	err := a.withSidecar(func(s *sidecarData) error {
		v, ok := s.Subscriptions[name]
		if name == "INBOX" && !ok {
			v = true
		}
		sub = v
		return nil
	})
	return sub, err
}

// SetSubscribed updates the LSUB subscription state of a mailbox.
func (a *Account) SetSubscribed(name string, subscribed bool) error {
	return a.withSidecar(func(s *sidecarData) error {
		s.Subscriptions[name] = subscribed
		return nil
	})
}

// VacationLastSent returns the last auto-reply time for a sender (zero
// when never).
func (a *Account) VacationLastSent(sender string) (time.Time, error) {
	var un int64
	err := a.withSidecar(func(s *sidecarData) error {
		un = s.Vacation[sender]
		return nil
	})
	if err != nil || un == 0 {
		return time.Time{}, err
	}
	return time.Unix(0, un), nil
}

// SetVacationLastSent records the last auto-reply time for a sender.
func (a *Account) SetVacationLastSent(sender string, t time.Time) error {
	return a.withSidecar(func(s *sidecarData) error {
		if t.IsZero() {
			delete(s.Vacation, sender)
			return nil
		}
		s.Vacation[sender] = t.UnixNano()
		return nil
	})
}

// MailboxModSeq returns the mailbox's highest modification sequence.
func (a *Account) MailboxModSeq(mailbox string) (uint64, error) {
	var n uint64
	err := a.withSidecar(func(s *sidecarData) error {
		n = s.ModSeq[mailbox]
		return nil
	})
	return n, err
}

// BumpMailboxModSeq increments the mailbox's modification sequence and
// returns the new value.
func (a *Account) BumpMailboxModSeq(mailbox string) (uint64, error) {
	var n uint64
	err := a.withSidecar(func(s *sidecarData) error {
		s.ModSeq[mailbox]++
		n = s.ModSeq[mailbox]
		return nil
	})
	return n, err
}

// MessageModSeq returns the modseq of one message copy (0 when unknown).
func (a *Account) MessageModSeq(mailbox string, uid uint32) (uint64, error) {
	var n uint64
	err := a.withSidecar(func(s *sidecarData) error {
		if s.MsgModSeq[mailbox] != nil {
			n = s.MsgModSeq[mailbox][strconv.FormatUint(uint64(uid), 10)]
		}
		return nil
	})
	return n, err
}

// SetMessageModSeq records the modseq of one message copy.
func (a *Account) SetMessageModSeq(mailbox string, uid uint32, modseq uint64) error {
	return a.withSidecar(func(s *sidecarData) error {
		if s.MsgModSeq[mailbox] == nil {
			s.MsgModSeq[mailbox] = map[string]uint64{}
		}
		s.MsgModSeq[mailbox][strconv.FormatUint(uint64(uid), 10)] = modseq
		return nil
	})
}

// ListSieveScripts returns the stored script names and the active flag.
func (a *Account) ListSieveScripts() (map[string]bool, error) {
	out := map[string]bool{}
	err := a.withSidecar(func(s *sidecarData) error {
		for name := range s.Sieve {
			out[name] = name == s.ActiveSieve
		}
		return nil
	})
	return out, err
}

// GetSieveScript returns one stored script.
func (a *Account) GetSieveScript(name string) (string, error) {
	var content string
	var ok bool
	err := a.withSidecar(func(s *sidecarData) error {
		content, ok = s.Sieve[name]
		return nil
	})
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotFound
	}
	return content, nil
}

// PutSieveScript stores a script, optionally activating it.
func (a *Account) PutSieveScript(name, content string, activate bool) error {
	return a.withSidecar(func(s *sidecarData) error {
		s.Sieve[name] = content
		if activate {
			s.ActiveSieve = name
		}
		return nil
	})
}

// SetSieveActive activates a stored script ("" deactivates).
func (a *Account) SetSieveActive(name string) error {
	return a.withSidecar(func(s *sidecarData) error {
		if name != "" {
			if _, ok := s.Sieve[name]; !ok {
				return ErrNotFound
			}
		}
		s.ActiveSieve = name
		return nil
	})
}

// DeleteSieveScript removes a script; the active pointer is cleared when it
// pointed at the removed script.
func (a *Account) DeleteSieveScript(name string) error {
	return a.withSidecar(func(s *sidecarData) error {
		if _, ok := s.Sieve[name]; !ok {
			return ErrNotFound
		}
		delete(s.Sieve, name)
		if s.ActiveSieve == name {
			s.ActiveSieve = ""
		}
		return nil
	})
}

// ActiveSieveScript returns the active script name and content.
func (a *Account) ActiveSieveScript() (string, string, error) {
	var name, content string
	err := a.withSidecar(func(s *sidecarData) error {
		name = s.ActiveSieve
		if name != "" {
			content = s.Sieve[name]
		}
		return nil
	})
	return name, content, err
}

// QuotaBytes sums message sizes under every cur/ and new/ directory.
// Sidecar and tmp files are excluded.
func (a *Account) QuotaBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(a.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "tmp" || d.Name() == ".mailezine" {
				return filepath.SkipDir
			}
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		if parent == "cur" || parent == "new" {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	return total, err
}
