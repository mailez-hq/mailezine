// KV is a mailstore over the logical store (account → collection → document
// + blob), for the Pebble/TiDB + MinIO/FS storage modes.
package mailstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"mailezine/internal/store"
)

// Error aliases for the protocol layers (IMAP/POP3/ManageSieve/management):
// matching must go through these — not the store package — so consumers of
// the MailboxStore interface never need to import internal/store. They are
// the same error values the KV and maildir backends return.
var (
	ErrNotFound = store.ErrNotFound
	ErrExists   = store.ErrExists
)

// Document field ids for Email documents (ARCHITECTURE.md §2.1) — aliases
// to the canonical table in internal/store so the write path can never drift
// from the atomic-delete accounting there.
const (
	fieldBlobID   = store.EmailFieldBlob
	fieldUID      = store.EmailFieldUID
	fieldMailbox  = store.EmailFieldMailbox
	fieldFlags    = store.EmailFieldFlags
	fieldDate     = store.EmailFieldDate
	fieldFrom     = store.EmailFieldFrom
	fieldSize     = store.EmailFieldSize
	fieldKeywords = store.EmailFieldKeywords
	fieldModSeq   = store.EmailFieldModSeq
)

// Mailbox document fields (CollectionMailbox).
const (
	mbFieldName        byte = 1
	mbFieldUIDValidity byte = 2
	mbFieldSubscribed  byte = 3
	mbFieldAttrs       byte = 4
	mbFieldACL         byte = 5 // JSON: map[string]string (identifier → rights)
)

// KV implements Store on top of a store.Store facade.
type KV struct {
	s *store.Store
}

// NewKV wraps a store facade.
func NewKV(s *store.Store) *KV {
	return &KV{s: s}
}

// Deliver stores one message: blob first, then an Email document with the
// metadata, then quota and change log — the ARCHITECTURE.md §3.5 ordering.
func (k *KV) Deliver(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return 0, err
	}
	mbID, err := k.ensureMailbox(ctx, acctID, mailbox)
	if err != nil {
		return 0, err
	}
	uid, err := k.nextUID(ctx, acctID, mbID)
	if err != nil {
		return 0, err
	}
	modseq, err := k.s.BumpMailboxModSeq(ctx, acctID, mbID)
	if err != nil {
		return 0, err
	}
	docID, err := k.s.CreateDocument(ctx, acctID, store.CollectionEmail)
	if err != nil {
		return 0, err
	}
	blobID := fmt.Sprintf("email-%d-%d", acctID, docID)
	data := msg.Data
	if data == nil {
		data = []byte{}
	}
	size, err := k.s.PutBlob(ctx, blobID, int64(len(data)), bytes.NewReader(data))
	if err != nil {
		return 0, err
	}

	date := msg.InternalDate
	if date.IsZero() {
		date = time.Now()
	}
	system, kws := splitFlags(normalizeFlags(msg.Flags, msg.Seen))
	keywords := append(append([]string(nil), kws...), msg.Keywords...)
	fields := map[byte][]byte{
		fieldBlobID:   []byte(blobID),
		fieldUID:      beUint64(uid),
		fieldMailbox:  []byte(mailbox),
		fieldFlags:    []byte(strings.Join(system, ",")),
		fieldDate:     []byte(date.UTC().Format(time.RFC3339)),
		fieldFrom:     []byte(msg.From),
		fieldSize:     beUint64(uint64(size)),
		fieldKeywords: []byte(strings.Join(keywords, ",")),
		fieldModSeq:   beUint64(modseq),
	}
	// Fields + blob link + quota + change log + per-mailbox index commit
	// atomically.
	idx := store.Op{Key: store.IndexEmailKey(uint32(acctID), mbID, uid), Value: beUint64(docID)}
	if err := k.s.AppendEmailAtomically(ctx, acctID, store.CollectionEmail, docID, fields, blobID, size, idx); err != nil {
		return 0, err
	}
	return uint32(uid), nil
}

func (k *KV) ensureAccount(ctx context.Context, account string) (store.AccountID, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return k.s.CreateAccount(ctx, account)
	}
	return acctID, err
}

// ensureMailbox returns the mailbox document ID, creating the mailbox (and
// its UID counter) on first use. INBOX is created eagerly so delivery and
// IMAP always see it.
func (k *KV) ensureMailbox(ctx context.Context, acctID store.AccountID, mailbox string) (uint64, error) {
	if id, err := k.mailboxDocID(ctx, acctID, mailbox); err == nil {
		return id, nil
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
	return docID, nil
}

// mailboxDocID resolves a mailbox name to its document ID. The primary path
// is a single index GET; a missing or stale entry falls back to the linear
// collection scan and repairs the index in place.
func (k *KV) mailboxDocID(ctx context.Context, acctID store.AccountID, mailbox string) (uint64, error) {
	key := store.IndexMailboxNameKey(uint32(acctID), mailbox)
	if v, err := k.s.GetRaw(ctx, key); err == nil && len(v) == 8 {
		id := binary.BigEndian.Uint64(v)
		if fields, ferr := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, id); ferr == nil &&
			string(fields[mbFieldName]) == mailbox {
			return id, nil
		}
		// Stale (points at a deleted/renamed mailbox) — repair below.
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionMailbox)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, id)
		if err != nil {
			return 0, err
		}
		if string(fields[mbFieldName]) == mailbox {
			if err := k.s.PutRaw(ctx, key, beUint64(id)); err != nil {
				return 0, err
			}
			return id, nil
		}
	}
	return 0, store.ErrNotFound
}

// nextUID allocates the next UID of a mailbox. The counter is keyed by the
// mailbox document ID so UIDs survive renames (INV-UID per mailbox identity).
func (k *KV) nextUID(ctx context.Context, acctID store.AccountID, mbID uint64) (uint64, error) {
	sub := append([]byte{store.CollectionEmail}, beUint64(mbID)...)
	return k.s.NextCounter(ctx, acctID, store.CounterKindNextDoc, sub)
}

func (k *KV) uidNext(ctx context.Context, acctID store.AccountID, mbID uint64) (uint64, error) {
	sub := append([]byte{store.CollectionEmail}, beUint64(mbID)...)
	n, err := k.s.CounterValue(ctx, acctID, store.CounterKindNextDoc, sub)
	return n + 1, err
}

func beUint32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func beUint32Value(b []byte) uint32 {
	if len(b) != 4 {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func beUint64Value(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// QuotaUsedBytes sums the sizes of every Email document of the account.
func (k *KV) QuotaUsedBytes(ctx context.Context, account string) (int64, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil // no account yet ⇒ nothing stored
	}
	if err != nil {
		return 0, err
	}
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionEmail)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, id)
		if err != nil {
			return 0, err
		}
		if v, ok := fields[fieldSize]; ok && len(v) == 8 {
			total += int64(binary.BigEndian.Uint64(v))
		}
	}
	return total, nil
}

// GetBlob streams a stored message body (used by IMAP and tests).
func (k *KV) GetBlob(ctx context.Context, blobID string, w io.Writer) error {
	return k.s.GetBlob(ctx, blobID, w)
}

// Email is the readable form of a stored email (used by tests and IMAP).
type Email struct {
	DocID    uint64
	UID      uint32
	Mailbox  string
	Flags    []string
	Keywords []string
	Date     time.Time
	From     string
	Size     int64
	BlobID   string
	ModSeq   uint64 // CONDSTORE: last change sequence of this message
}

// Seen reports whether the \Seen flag is set.
func (e *Email) Seen() bool {
	for _, f := range e.Flags {
		if f == "\\Seen" {
			return true
		}
	}
	return false
}

// EmailByUID reads one email document. UID uniqueness is per mailbox; the
// lookup is a single index GET followed by one document read.
func (k *KV) EmailByUID(ctx context.Context, account, mailbox string, uid uint32) (*Email, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return nil, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return nil, err
	}
	docID, err := k.s.GetRaw(ctx, store.IndexEmailKey(uint32(acctID), mbID, uint64(uid)))
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrNotFound
	}
	if err != nil || len(docID) != 8 {
		return nil, store.ErrNotFound
	}
	fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionEmail, binary.BigEndian.Uint64(docID))
	if err != nil {
		return nil, err
	}
	return emailFromFields(binary.BigEndian.Uint64(docID), fields), nil
}

func emailFromFields(docID uint64, fields map[byte][]byte) *Email {
	e := &Email{DocID: docID}
	e.BlobID = string(fields[fieldBlobID])
	if len(fields[fieldUID]) == 8 {
		e.UID = uint32(binary.BigEndian.Uint64(fields[fieldUID]))
	}
	e.Mailbox = string(fields[fieldMailbox])
	e.Flags = splitCSV(fields[fieldFlags])
	e.Keywords = splitCSV(fields[fieldKeywords])
	if t, err := time.Parse(time.RFC3339, string(fields[fieldDate])); err == nil {
		e.Date = t
	}
	e.From = string(fields[fieldFrom])
	if len(fields[fieldSize]) == 8 {
		e.Size = int64(binary.BigEndian.Uint64(fields[fieldSize]))
	}
	if len(fields[fieldModSeq]) == 8 {
		e.ModSeq = binary.BigEndian.Uint64(fields[fieldModSeq])
	}
	return e
}

func splitCSV(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), ",")
}

func beUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

var _ Store = (*KV)(nil)
