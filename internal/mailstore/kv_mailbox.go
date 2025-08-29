// The IMAP-facing mailbox surface on the KV backend: mailbox documents live
// in CollectionMailbox, messages in CollectionEmail with UID counters keyed
// by mailbox document ID (UIDs survive renames; INV-UID per mailbox
// identity). Mutations go through the atomic store primitives so quota,
// blob references and the change log stay consistent (ARCHITECTURE.md §3.5).
package mailstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"strings"

	"mailezine/internal/store"
)

// ListMailboxes returns every mailbox of the account, INBOX first.
func (k *KV) ListMailboxes(ctx context.Context, account string) ([]Mailbox, error) {
	// Lazy provisioning matches maildir: a listed (authenticated) account
	// always has at least INBOX.
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	// INBOX always exists (RFC 3501): create it lazily.
	if _, err := k.ensureMailbox(ctx, acctID, "INBOX"); err != nil {
		return nil, err
	}
	if err := k.EnsureDefaultMailboxes(ctx, account); err != nil {
		return nil, err
	}
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionMailbox)
	if err != nil {
		return nil, err
	}
	out := make([]Mailbox, 0, len(ids))
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, id)
		if err != nil {
			return nil, err
		}
		uidNext, err := k.uidNext(ctx, acctID, id)
		if err != nil {
			return nil, err
		}
		mb := Mailbox{
			Name:          string(fields[mbFieldName]),
			UIDValidity:   beUint32Value(fields[mbFieldUIDValidity]),
			UIDNext:       uint32(uidNext),
			Subscribed:    len(fields[mbFieldSubscribed]) == 1 && fields[mbFieldSubscribed][0] == 1,
			Attrs:         splitCSV(fields[mbFieldAttrs]),
			HighestModSeq: beUint64Value(fields[store.FieldMailboxModSeq]),
		}
		out = append(out, mb)
	}
	// Fill counts from a single pass over the email collection.
	emails, err := k.emailsOf(ctx, acctID, "")
	if err != nil {
		return nil, err
	}
	byName := map[string]*Mailbox{}
	for i := range out {
		byName[out[i].Name] = &out[i]
	}
	for _, e := range emails {
		mb := byName[e.Mailbox]
		if mb == nil {
			continue
		}
		mb.NumMessages++
		mb.Size += e.Size
		if !e.Seen() {
			mb.NumUnseen++
		}
		if e.HasFlag("\\Deleted") {
			mb.NumDeleted++
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == "INBOX" {
			return true
		}
		if out[j].Name == "INBOX" {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return applyDefaultAttrs(out), nil
}

// EnsureDefaultMailboxes creates and subscribes Trash/Drafts/Sent/Junk.
func (k *KV) EnsureDefaultMailboxes(ctx context.Context, account string) error {
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return err
	}
	for name := range DefaultMailboxes {
		if _, err := k.ensureMailbox(ctx, acctID, name); err != nil {
			return err
		}
		mbID, err := k.mailboxDocID(ctx, acctID, name)
		if err != nil {
			return err
		}
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, mbID)
		if err != nil {
			return err
		}
		subscribed := len(fields[mbFieldSubscribed]) == 1 && fields[mbFieldSubscribed][0] == 1
		if !subscribed {
			if err := k.s.PutDocumentFields(ctx, acctID, store.CollectionMailbox, mbID,
				map[byte][]byte{mbFieldSubscribed: []byte{1}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// MailboxStatus reports the metadata of one mailbox.
func (k *KV) MailboxStatus(ctx context.Context, account, mailbox string) (Mailbox, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return Mailbox{}, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return Mailbox{}, err
	}
	fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, mbID)
	if err != nil {
		return Mailbox{}, err
	}
	uidNext, err := k.uidNext(ctx, acctID, mbID)
	if err != nil {
		return Mailbox{}, err
	}
	mb := Mailbox{
		Name:          mailbox,
		UIDValidity:   beUint32Value(fields[mbFieldUIDValidity]),
		UIDNext:       uint32(uidNext),
		Subscribed:    len(fields[mbFieldSubscribed]) == 1 && fields[mbFieldSubscribed][0] == 1,
		HighestModSeq: beUint64Value(fields[store.FieldMailboxModSeq]),
	}
	emails, err := k.mailboxEmails(ctx, acctID, mbID)
	if err != nil {
		return Mailbox{}, err
	}
	for _, e := range emails {
		mb.NumMessages++
		mb.Size += e.Size
		if !e.Seen() {
			mb.NumUnseen++
		}
		if e.HasFlag("\\Deleted") {
			mb.NumDeleted++
		}
	}
	return mb, nil
}

// CreateMailbox creates a mailbox and returns its UIDVALIDITY (the mailbox
// document ID, stable and never reused).
func (k *KV) CreateMailbox(ctx context.Context, account, mailbox string) (uint32, error) {
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return 0, err
	}
	if _, err := k.mailboxDocID(ctx, acctID, mailbox); err == nil {
		return 0, store.ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	docID, err := k.s.CreateDocument(ctx, acctID, store.CollectionMailbox)
	if err != nil {
		return 0, err
	}
	fields := map[byte][]byte{
		mbFieldName:        []byte(mailbox),
		mbFieldUIDValidity: beUint32(uint32(docID)),
		mbFieldSubscribed:  []byte{0},
	}
	if err := k.s.PutDocumentFields(ctx, acctID, store.CollectionMailbox, docID, fields); err != nil {
		return 0, err
	}
	if err := k.s.PutRaw(ctx, store.IndexMailboxNameKey(uint32(acctID), mailbox), beUint64(docID)); err != nil {
		return 0, err
	}
	return uint32(docID), nil
}

// DeleteMailbox removes the mailbox document and every message in it
// (blobs are garbage-collected once their link count reaches zero).
func (k *KV) DeleteMailbox(ctx context.Context, account, mailbox string) error {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return err
	}
	emails, err := k.mailboxEmails(ctx, acctID, mbID)
	if err != nil {
		return err
	}
	for _, e := range emails {
		if err := k.deleteEmail(ctx, acctID, e.DocID, mbID); err != nil {
			return err
		}
	}
	if err := k.s.DeleteDocument(ctx, acctID, store.CollectionMailbox, mbID); err != nil {
		return err
	}
	return k.s.DeleteRaw(ctx, store.IndexMailboxNameKey(uint32(acctID), mailbox))
}

// RenameMailbox moves the mailbox (messages keep their UIDs; both the UID
// counter and the per-mailbox email index are keyed by mailbox identity —
// docID — so only the name index and each email's mailbox field change).
func (k *KV) RenameMailbox(ctx context.Context, account, oldName, newName string) error {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, oldName)
	if err != nil {
		return err
	}
	if _, err := k.mailboxDocID(ctx, acctID, newName); err == nil {
		return store.ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	nameIdx := []store.Op{
		{Key: store.IndexMailboxNameKey(uint32(acctID), oldName), Delete: true},
		{Key: store.IndexMailboxNameKey(uint32(acctID), newName), Value: beUint64(mbID)},
	}
	if err := k.s.UpdateDocumentAtomically(ctx, acctID, store.CollectionMailbox, mbID,
		map[byte][]byte{mbFieldName: []byte(newName)}, nameIdx...); err != nil {
		return err
	}
	emails, err := k.mailboxEmails(ctx, acctID, mbID)
	if err != nil {
		return err
	}
	for _, e := range emails {
		if err := k.s.UpdateDocumentAtomically(ctx, acctID, store.CollectionEmail, e.DocID,
			map[byte][]byte{fieldMailbox: []byte(newName)}); err != nil {
			return err
		}
	}
	return nil
}

// SetSubscribed flips the LSUB subscription bit.
func (k *KV) SetSubscribed(ctx context.Context, account, mailbox string, subscribed bool) error {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return err
	}
	v := []byte{0}
	if subscribed {
		v = []byte{1}
	}
	return k.s.UpdateDocumentAtomically(ctx, acctID, store.CollectionMailbox, mbID,
		map[byte][]byte{mbFieldSubscribed: v})
}

// ListMessages returns the messages of a mailbox ordered by UID.
func (k *KV) ListMessages(ctx context.Context, account, mailbox string) ([]*Message, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return nil, err
	}
	emails, err := k.mailboxEmails(ctx, acctID, mbID)
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(emails))
	for _, e := range emails {
		out = append(out, messageFromEmail(e))
	}
	return out, nil
}

// emailDocIDByUID resolves one UID through the per-mailbox index (a single
// GET) and returns the document with its fields.
func (k *KV) emailByIndexUID(ctx context.Context, acctID store.AccountID, mbID, uid uint64) (*Email, error) {
	val, err := k.s.GetRaw(ctx, store.IndexEmailKey(uint32(acctID), mbID, uid))
	if errors.Is(err, store.ErrNotFound) || (err == nil && len(val) != 8) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	docID := binary.BigEndian.Uint64(val)
	fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, docID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrNotFound // stale index entry (pre-delete)
	}
	if err != nil {
		return nil, err
	}
	return emailFromFields(docID, fields), nil
}

// MessageByUID returns one message's metadata (without body).
func (k *KV) MessageByUID(ctx context.Context, account, mailbox string, uid uint32) (*Message, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return nil, err
	}
	e, err := k.emailByIndexUID(ctx, acctID, mbID, uint64(uid))
	if err != nil {
		return nil, err
	}
	return messageFromEmail(e), nil
}

// SetFlags replaces the system flags and keywords of one message.
func (k *KV) SetFlags(ctx context.Context, account, mailbox string, uid uint32, flags []string) error {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return err
	}
	e, err := k.emailByIndexUID(ctx, acctID, mbID, uint64(uid))
	if err != nil {
		return err
	}
	modseq, err := k.s.BumpMailboxModSeq(ctx, acctID, mbID)
	if err != nil {
		return err
	}
	system, keywords := splitFlags(flags)
	return k.s.UpdateDocumentAtomically(ctx, acctID, store.CollectionEmail, e.DocID,
		map[byte][]byte{
			fieldFlags:    []byte(strings.Join(system, ",")),
			fieldKeywords: []byte(strings.Join(keywords, ",")),
			fieldModSeq:   beUint64(modseq),
		})
}

// Append stores a message and returns its new UID (APPEND semantics).
func (k *KV) Append(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	return k.Deliver(ctx, account, mailbox, msg)
}

// Expunge removes messages marked \Deleted (or, for UID EXPUNGE, the given
// UIDs regardless of flag) and returns the expunged UIDs in ascending order.
func (k *KV) Expunge(ctx context.Context, account, mailbox string, uids []uint32) ([]uint32, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return nil, err
	}
	emails, err := k.mailboxEmails(ctx, acctID, mbID)
	if err != nil {
		return nil, err
	}
	uidFilter := map[uint32]struct{}{}
	for _, u := range uids {
		uidFilter[u] = struct{}{}
	}
	var deleted []uint32
	for _, e := range emails {
		// An explicit UID set deletes exactly those messages (the caller —
		// POP3 QUIT — owns the precondition); the flagless form deletes
		// everything carrying \Deleted (IMAP EXPUNGE).
		if len(uidFilter) > 0 {
			if _, ok := uidFilter[e.UID]; !ok {
				continue
			}
		} else if !e.HasFlag("\\Deleted") {
			continue
		}
		if err := k.deleteEmail(ctx, acctID, e.DocID, mbID); err != nil {
			return deleted, err
		}
		deleted = append(deleted, e.UID)
	}
	if len(deleted) > 0 {
		if _, err := k.s.BumpMailboxModSeq(ctx, acctID, mbID); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// Copy appends copies of the given UIDs to dst and returns the UID mapping.
func (k *KV) Copy(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	srcMBID, err := k.mailboxDocID(ctx, acctID, src)
	if err != nil {
		return nil, err
	}
	emails, err := k.mailboxEmails(ctx, acctID, srcMBID)
	if err != nil {
		return nil, err
	}
	uidFilter := map[uint32]struct{}{}
	for _, u := range uids {
		uidFilter[u] = struct{}{}
	}
	mapping := map[uint32]uint32{}
	for _, e := range emails {
		if len(uidFilter) > 0 {
			if _, ok := uidFilter[e.UID]; !ok {
				continue
			}
		}
		var buf bytes.Buffer
		if err := k.s.GetBlob(ctx, e.BlobID, &buf); err != nil {
			return mapping, err
		}
		newUID, err := k.Deliver(ctx, account, dst, &Message{
			From:         e.From,
			Data:         buf.Bytes(),
			Flags:        e.Flags,
			Keywords:     e.Keywords,
			InternalDate: e.Date,
		})
		if err != nil {
			return mapping, err
		}
		mapping[e.UID] = uint32(newUID)
	}
	return mapping, nil
}

// Move copies the messages to dst and deletes the source copies (a crash
// between the two leaves a copy in both mailboxes — a safe IMAP MOVE
// outcome; RFC 6851 allows either side).
func (k *KV) Move(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	srcMBID, err := k.mailboxDocID(ctx, acctID, src)
	if err != nil {
		return nil, err
	}
	emails, err := k.mailboxEmails(ctx, acctID, srcMBID)
	if err != nil {
		return nil, err
	}
	uidFilter := map[uint32]struct{}{}
	for _, u := range uids {
		uidFilter[u] = struct{}{}
	}
	mapping := map[uint32]uint32{}
	for _, e := range emails {
		if len(uidFilter) > 0 {
			if _, ok := uidFilter[e.UID]; !ok {
				continue
			}
		}
		var buf bytes.Buffer
		if err := k.s.GetBlob(ctx, e.BlobID, &buf); err != nil {
			return mapping, err
		}
		newUID, err := k.Deliver(ctx, account, dst, &Message{
			From:         e.From,
			Data:         buf.Bytes(),
			Flags:        e.Flags,
			Keywords:     e.Keywords,
			InternalDate: e.Date,
		})
		if err != nil {
			return mapping, err
		}
		if err := k.deleteEmail(ctx, acctID, e.DocID, srcMBID); err != nil {
			return mapping, err
		}
		mapping[e.UID] = uint32(newUID)
	}
	if len(mapping) > 0 {
		if _, err := k.s.BumpMailboxModSeq(ctx, acctID, srcMBID); err != nil {
			return mapping, err
		}
	}
	return mapping, nil
}

// OpenMessage streams the raw message body.
func (k *KV) OpenMessage(ctx context.Context, account, mailbox string, uid uint32) (io.ReadCloser, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return nil, err
	}
	e, err := k.emailByIndexUID(ctx, acctID, mbID, uint64(uid))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := k.s.GetBlob(ctx, e.BlobID, &buf); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// deleteEmail removes one email document (and its per-mailbox index entry,
// committed in the same batch) and garbage-collects its blob.
func (k *KV) deleteEmail(ctx context.Context, acctID store.AccountID, docID, mbID uint64) error {
	// Read the blob reference and UID before deletion (the fields are
	// removed in the same batch as the link decrement and index delete).
	var blobID string
	var idx []store.Op
	fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, docID)
	if err == nil {
		blobID = string(fields[fieldBlobID])
		if len(fields[fieldUID]) == 8 {
			uid := binary.BigEndian.Uint64(fields[fieldUID])
			idx = append(idx, store.Op{Key: store.IndexEmailKey(uint32(acctID), mbID, uid), Delete: true})
		}
	}
	if err := k.s.DeleteEmailAtomically(ctx, acctID, store.CollectionEmail, docID, idx...); err != nil {
		return err
	}
	// Garbage-collect the blob when the link count reaches zero.
	if blobID != "" {
		if refs, err := k.s.BlobRefCount(ctx, acctID, blobID); err == nil && refs == 0 {
			_ = k.s.DeleteBlob(ctx, blobID)
		}
	}
	return nil
}

// emailsOf lists every Email document of an account, optionally filtered by
// mailbox, ordered by UID. The unfiltered form is only used for account-wide
// statistics; per-mailbox callers use mailboxEmails.
func (k *KV) emailsOf(ctx context.Context, acctID store.AccountID, mailbox string) ([]*Email, error) {
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionEmail)
	if err != nil {
		return nil, err
	}
	var out []*Email
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, id)
		if err != nil {
			return nil, err
		}
		e := emailFromFields(id, fields)
		if mailbox != "" && e.Mailbox != mailbox {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// Reindex backfills the secondary indexes (mailbox-name → docID,
// per-mailbox (mbID, UID) → docID) from the document collections — the
// one-time migration for data written before the indexes existed.
// Idempotent: entries are only rewritten when missing or divergent, so it
// is safe to run on every start-up of a KV-backed engine.
func (k *KV) Reindex(ctx context.Context) (int, error) {
	accounts, err := k.s.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}
	fixed := 0
	for _, account := range accounts {
		acctID, err := k.s.AccountByEmail(ctx, account)
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return fixed, err
		}
		// Mailbox documents → name index.
		mbIDs, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionMailbox)
		if err != nil {
			return fixed, err
		}
		byName := map[string]uint64{}
		for _, id := range mbIDs {
			fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, id)
			if err != nil {
				return fixed, err
			}
			name := string(fields[mbFieldName])
			byName[name] = id
			key := store.IndexMailboxNameKey(uint32(acctID), name)
			if v, gerr := k.s.GetRaw(ctx, key); gerr != nil || len(v) != 8 ||
				binary.BigEndian.Uint64(v) != id {
				if err := k.s.PutRaw(ctx, key, beUint64(id)); err != nil {
					return fixed, err
				}
				fixed++
			}
		}
		// Email documents → per-mailbox index (keyed by mailbox identity,
		// so renamed mailboxes keep their entries).
		ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionEmail)
		if err != nil {
			return fixed, err
		}
		for _, id := range ids {
			fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			} else if err != nil {
				return fixed, err
			}
			e := emailFromFields(id, fields)
			mbID, ok := byName[e.Mailbox]
			if !ok {
				continue // mailbox deleted; nothing to hang the entry on
			}
			key := store.IndexEmailKey(uint32(acctID), mbID, uint64(e.UID))
			if v, gerr := k.s.GetRaw(ctx, key); gerr != nil || len(v) != 8 ||
				binary.BigEndian.Uint64(v) != id {
				if err := k.s.PutRaw(ctx, key, beUint64(id)); err != nil {
					return fixed, err
				}
				fixed++
			}
		}
	}
	return fixed, nil
}

// mailboxEmails lists the messages of one mailbox via the (mbID, UID) →
// docID secondary index: a bounded prefix scan (the mailbox's own messages,
// in ascending UID order) plus one document read per message. Stale entries
// pointing at deleted documents are skipped.
func (k *KV) mailboxEmails(ctx context.Context, acctID store.AccountID, mbID uint64) ([]*Email, error) {
	var out []*Email
	err := k.s.ScanRaw(ctx, store.IndexEmailPrefix(uint32(acctID), mbID), func(_ []byte, val []byte) error {
		if len(val) != 8 {
			return nil
		}
		docID := binary.BigEndian.Uint64(val)
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, docID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // stale index entry; deletion path cleans it up
		}
		if err != nil {
			return err
		}
		out = append(out, emailFromFields(docID, fields))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func messageFromEmail(e *Email) *Message {
	return &Message{
		UID:  e.UID,
		From: e.From,
		// Match the maildir backend: keywords are exposed through Flags so
		// FETCH and SEARCH (KEYWORD) see them; SetFlags re-splits them.
		Flags:        append(append([]string(nil), e.Flags...), e.Keywords...),
		Keywords:     append([]string(nil), e.Keywords...),
		InternalDate: e.Date,
		Size:         e.Size,
		ModSeq:       e.ModSeq,
	}
}

// splitFlags separates system flags (backslash-prefixed) from keywords.
func splitFlags(flags []string) (system, keywords []string) {
	for _, f := range flags {
		if strings.HasPrefix(f, "\\") {
			system = append(system, f)
		} else {
			keywords = append(keywords, f)
		}
	}
	return system, keywords
}

func (e *Email) HasFlag(flag string) bool {
	for _, f := range e.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

var _ MailboxStore = (*KV)(nil)
