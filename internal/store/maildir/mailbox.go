//go:build unix

package maildir

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mailbox is one mailbox backed by a Maildir++ directory.
type Mailbox struct {
	acct *Account
	name string
	dir  string

	mu  sync.Mutex
	seq uint64 // filename uniqueness within this process
}

// Name returns the mailbox name ("INBOX", "Sent", "Foo/Bar").
func (m *Mailbox) Name() string { return m.name }

// UIDValidity returns the current uidvalidity (INV-UID).
func (m *Mailbox) UIDValidity() (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ul, err := loadUIDList(m.dir)
	if err != nil {
		return 0, err
	}
	return ul.validity, nil
}

// UIDNext returns the next UID to allocate: the persisted sidecar value, or
// max(UID)+1 when the sidecar has not been written yet. UIDs never
// decrease (INV-UID).
func (m *Mailbox) UIDNext() (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := m.sidecarUIDNext()
	if err != nil {
		return 0, err
	}
	if next > 0 {
		return next, nil
	}
	msgs, err := m.scanLocked()
	if err != nil {
		return 0, err
	}
	var max uint32
	for _, msg := range msgs {
		if msg.UID > max {
			max = msg.UID
		}
	}
	return max + 1, nil
}

// Messages returns all messages sorted by UID, reconciling the uidlist with
// the filesystem. New files get fresh UIDs; stale entries are dropped; the
// uidlist is rewritten when anything changed.
func (m *Mailbox) Messages() ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	release, err := lockMaildir(m.dir)
	if err != nil {
		return nil, err
	}
	defer release()
	return m.scanLocked()
}

// Message returns one message by UID.
func (m *Mailbox) Message(uid uint32) (Message, error) {
	msgs, err := m.Messages()
	if err != nil {
		return Message{}, err
	}
	for _, msg := range msgs {
		if msg.UID == uid {
			return msg, nil
		}
	}
	return Message{}, ErrNotFound
}

// Open returns the raw message bytes of a UID.
func (m *Mailbox) Open(uid uint32) (io.ReadCloser, error) {
	msg, err := m.Message(uid)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(m.dir, msg.Subdir, msg.Filename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// Append stores a message, allocating the next UID. The file is written to
// tmp, fsynced, renamed into new/ (or cur/ when \Seen is set) and the
// uidlist is committed — the ARCHITECTURE.md §3.5 delivery ordering.
func (m *Mailbox) Append(r io.Reader, flags []Flag, internalDate time.Time) (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	release, err := lockMaildir(m.dir)
	if err != nil {
		return 0, err
	}
	defer release()

	ul, err := loadUIDList(m.dir)
	if err != nil {
		return 0, err
	}
	uid := m.nextUIDLocked(ul)

	now := internalDate
	if now.IsZero() {
		now = time.Now()
	}
	m.seq++
	base := newFilename(now, os.Getpid(), m.seq, flags)
	subdir := subdirFor(flags)
	rel := subdir + "/" + base + ":" + FormatFlags(flags)

	tmpPath := filepath.Join(m.dir, "tmp", base)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	finalPath := filepath.Join(m.dir, subdir, base+":"+FormatFlags(flags))
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	_ = os.Chtimes(finalPath, now, now)

	ul.entries[uid] = rel
	if err := saveUIDList(m.dir, ul); err != nil {
		return 0, err
	}
	if err := m.setSidecarUIDNext(uid + 1); err != nil {
		return 0, err
	}
	return uid, nil
}

// SetFlags replaces the system flags of a message, renaming the file and
// moving it between cur/ and new/ as \Seen changes.
func (m *Mailbox) SetFlags(uid uint32, flags []Flag) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	release, err := lockMaildir(m.dir)
	if err != nil {
		return err
	}
	defer release()

	msgs, err := m.scanLocked()
	if err != nil {
		return err
	}
	var msg *Message
	for i := range msgs {
		if msgs[i].UID == uid {
			msg = &msgs[i]
			break
		}
	}
	if msg == nil {
		return ErrNotFound
	}
	ul, err := loadUIDList(m.dir)
	if err != nil {
		return err
	}
	base, _ := SplitInfo(msg.Filename)
	oldPath := filepath.Join(m.dir, msg.Subdir, msg.Filename)
	newSubdir := subdirFor(flags)
	newName := base + ":" + FormatFlags(flags)
	newRel := newSubdir + "/" + newName
	newPath := filepath.Join(m.dir, newSubdir, newName)
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	ul.entries[uid] = newRel
	return saveUIDList(m.dir, ul)
}

// Delete removes a message and its metadata (uidlist entry + keywords).
func (m *Mailbox) Delete(uid uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	release, err := lockMaildir(m.dir)
	if err != nil {
		return err
	}
	defer release()

	msgs, err := m.scanLocked()
	if err != nil {
		return err
	}
	var msg *Message
	for i := range msgs {
		if msgs[i].UID == uid {
			msg = &msgs[i]
			break
		}
	}
	if msg == nil {
		return ErrNotFound
	}
	path := filepath.Join(m.dir, msg.Subdir, msg.Filename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	ul, err := loadUIDList(m.dir)
	if err != nil {
		return err
	}
	delete(ul.entries, uid)
	if err := saveUIDList(m.dir, ul); err != nil {
		return err
	}
	return m.dropKeywords(uid)
}

// Move relocates a message to another mailbox. It is implemented as a copy
// (new UID in the destination) followed by a delete — correct and simple;
// same-filesystem rename can be an optimization later.
func (m *Mailbox) Move(uid uint32, dst *Mailbox) error {
	if m == dst {
		return nil
	}
	// Deterministic lock order by directory path avoids deadlocks.
	first, second := m, dst
	if m.dir > dst.dir {
		first, second = dst, m
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	releaseA, err := lockMaildir(m.dir)
	if err != nil {
		return err
	}
	defer releaseA()
	releaseB, err := lockMaildir(dst.dir)
	if err != nil {
		return err
	}
	defer releaseB()

	msgs, err := m.scanLocked()
	if err != nil {
		return err
	}
	var msg *Message
	for i := range msgs {
		if msgs[i].UID == uid {
			msg = &msgs[i]
			break
		}
	}
	if msg == nil {
		return ErrNotFound
	}
	rc, err := os.Open(filepath.Join(m.dir, msg.Subdir, msg.Filename))
	if err != nil {
		return err
	}
	defer rc.Close()

	newUID, err := dst.appendLocked(rc, msg.Flags, msg.InternalDate)
	if err != nil {
		return err
	}
	_ = rc.Close()

	// Keywords follow the message.
	kws, err := m.keywords(uid)
	if err != nil {
		return err
	}
	if err := dst.setKeywords(newUID, kws); err != nil {
		return err
	}
	if err := m.dropKeywords(uid); err != nil {
		return err
	}

	// Remove the source file BEFORE dropping its uidlist entry: a crash in
	// between leaves a stale entry (reconcile marks it stale and cleans up)
	// rather than an entry-less file that would be re-assigned a fresh UID
	// and resurrect as a duplicate copy in the source mailbox.
	if err := os.Remove(filepath.Join(m.dir, msg.Subdir, msg.Filename)); err != nil {
		return err
	}
	ul, err := loadUIDList(m.dir)
	if err != nil {
		return err
	}
	delete(ul.entries, uid)
	return saveUIDList(m.dir, ul)
}

// UnseenCount returns the number of messages without \Seen.
func (m *Mailbox) UnseenCount() (int, error) {
	msgs, err := m.Messages()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, msg := range msgs {
		if !msg.Has(FlagSeen) {
			n++
		}
	}
	return n, nil
}

// Keywords returns the keywords of a message.
func (m *Mailbox) Keywords(uid uint32) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keywords(uid)
}

// SetKeywords replaces the keywords of a message.
func (m *Mailbox) SetKeywords(uid uint32, keywords []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	release, err := lockMaildir(m.dir)
	if err != nil {
		return err
	}
	defer release()
	msgs, err := m.scanLocked()
	if err != nil {
		return err
	}
	found := false
	for _, msg := range msgs {
		if msg.UID == uid {
			found = true
			break
		}
	}
	if !found {
		return ErrNotFound
	}
	return m.setKeywords(uid, keywords)
}

// ACL returns the RFC 4314 ACL entries of the mailbox.
func (m *Mailbox) ACL() (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out map[string]string
	err := m.acct.withSidecar(func(s *sidecarData) error {
		out = make(map[string]string, len(s.ACL[m.name]))
		for id, rights := range s.ACL[m.name] {
			out[id] = rights
		}
		return nil
	})
	return out, err
}

// SetACL replaces the rights of identifier, or removes the entry when
// rights is empty.
func (m *Mailbox) SetACL(identifier, rights string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acct.withSidecar(func(s *sidecarData) error {
		if s.ACL[m.name] == nil {
			s.ACL[m.name] = map[string]string{}
		}
		if rights == "" {
			delete(s.ACL[m.name], identifier)
			return nil
		}
		s.ACL[m.name][identifier] = rights
		return nil
	})
}

// DeleteACL removes every right of identifier on the mailbox.
func (m *Mailbox) DeleteACL(identifier string) error {
	return m.SetACL(identifier, "")
}

// appendLocked assumes m.mu and the uidlist flock are held (Append and Move).
func (m *Mailbox) appendLocked(r io.Reader, flags []Flag, internalDate time.Time) (uint32, error) {
	ul, err := loadUIDList(m.dir)
	if err != nil {
		return 0, err
	}
	uid := m.nextUIDLocked(ul)

	now := internalDate
	if now.IsZero() {
		now = time.Now()
	}
	m.seq++
	base := newFilename(now, os.Getpid(), m.seq, flags)
	subdir := subdirFor(flags)
	rel := subdir + "/" + base + ":" + FormatFlags(flags)

	tmpPath := filepath.Join(m.dir, "tmp", base)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	finalPath := filepath.Join(m.dir, subdir, base+":"+FormatFlags(flags))
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	_ = os.Chtimes(finalPath, now, now)

	ul.entries[uid] = rel
	if err := saveUIDList(m.dir, ul); err != nil {
		return 0, err
	}
	if err := m.setSidecarUIDNext(uid + 1); err != nil {
		return 0, err
	}
	return uid, nil
}

// nextUIDLocked allocates the next UID: never below max(uidlist)+1 and never
// below the persisted uidnext (no reuse after deletes, INV-UID).
func (m *Mailbox) nextUIDLocked(ul *uidList) uint32 {
	next := maxUID(ul) + 1
	if ul.next > next {
		next = ul.next
	}
	if saved, err := m.sidecarUIDNext(); err == nil && saved > next {
		next = saved
	}
	return next
}

// scanLocked reconciles files and uidlist; caller holds mutex + flock.
func (m *Mailbox) scanLocked() ([]Message, error) {
	files, err := listMessageFiles(m.dir)
	if err != nil {
		return nil, err
	}
	ul, err := loadUIDList(m.dir)
	if err != nil {
		// Unreadable/corrupt uidlist: rebuild from files with fresh validity.
		ul = &uidList{validity: uint32(time.Now().Unix()), entries: map[uint32]string{}}
	}
	// Reconcile allocation starts from the same three-source floor as
	// nextUIDLocked (max uid, persisted N field, sidecar uidnext): reusing a
	// deleted UID here would break IMAP caches exactly like it would on the
	// append path (INV-UID).
	next := maxUID(ul) + 1
	if ul.next > next {
		next = ul.next
	}
	if saved, err := m.sidecarUIDNext(); err == nil && saved > next {
		next = saved
	}
	dirty := false

	var msgs []Message
	for uid, rel := range ul.entries {
		info, ok := files[rel]
		if !ok {
			dirty = true // stale entry
			continue
		}
		msgs = append(msgs, messageFromFile(uid, info))
	}
	// Base-name index of surviving entries: a crash between a flag rename
	// and the uidlist rewrite leaves the file under a new name while the
	// entry still points at the old one — match by base name to keep the UID
	// instead of assigning a fresh one.
	baseOf := func(name string) string {
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		base, _ := SplitInfo(name)
		return base
	}
	byBase := map[string]uint32{}
	for uid, rel := range ul.entries {
		if _, ok := files[rel]; ok {
			byBase[baseOf(rel)] = uid
		}
	}
	var fresh []string
	for rel, info := range files {
		if hasEntry(ul, rel) {
			continue
		}
		if uid, ok := byBase[baseOf(rel)]; ok {
			// Renamed twin of a known entry (SetFlags crash window): adopt
			// the file under the original UID.
			ul.entries[uid] = rel
			dirty = true
			msgs = append(msgs, messageFromFile(uid, info))
			continue
		}
		fresh = append(fresh, rel)
	}
	// Assign UIDs in filename order (maildir names are timestamp-prefixed,
	// so this approximates arrival order — RFC 3501 requires ascending UIDs
	// by arrival; Go map iteration would make it random).
	sort.Strings(fresh)
	for _, rel := range fresh {
		uid := next
		next++
		ul.entries[uid] = rel
		dirty = true
		msgs = append(msgs, messageFromFile(uid, files[rel]))
	}
	if dirty {
		if err := saveUIDList(m.dir, ul); err != nil {
			return nil, err
		}
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })
	return msgs, nil
}

type messageFile struct {
	rel    string
	subdir string
	base   string
	full   string // complete filename including the info section
	flags  []Flag
	size   int64
	mtime  time.Time
}

func listMessageFiles(dir string) (map[string]messageFile, error) {
	out := map[string]messageFile{}
	for _, sub := range []string{"cur", "new"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				return nil, err
			}
			base, info := SplitInfo(e.Name())
			rel := sub + "/" + e.Name()
			out[rel] = messageFile{
				rel:    rel,
				subdir: sub,
				base:   base,
				full:   e.Name(),
				flags:  ParseFlags(info),
				size:   fi.Size(),
				mtime:  fi.ModTime(),
			}
		}
	}
	return out, nil
}

func messageFromFile(uid uint32, info messageFile) Message {
	return Message{
		UID:          uid,
		Subdir:       info.subdir,
		Filename:     info.full,
		Flags:        info.flags,
		Size:         info.size,
		InternalDate: info.mtime,
	}
}

func subdirFor(flags []Flag) string {
	for _, f := range flags {
		if f == FlagSeen {
			return "cur"
		}
	}
	return "new"
}
