// The logical model over KV, aligned with ARCHITECTURE.md §2-§3.
//
// Store owns the invariants of the v1 model:
//
//	Account ──▶ Collection ──▶ Document(fields) + BlobRefs
//
// Document IDs and change IDs come from per-account monotonic counters, so
// they are unique, never reused and never decrease (INV-UID, INV-CHANGE).
// Quota and blob-link counters are updated in the same KV batches as the
// operations that cause them (INV-QUOTA, INV-BLOB).
package store

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// AccountID is a globally unique account identifier.
type AccountID uint32

// Email document field ids referenced by the store itself (atomic delete
// needs the blob link and size for INV-BLOB / INV-QUOTA accounting).
// mailstore defines the full field table; these must stay in sync.
const (
	EmailFieldBlob byte = 1
	EmailFieldSize byte = 7
)

// Store is a KV+Blob facade exposing account-scoped operations. It is safe
// for concurrent use; per-account mutations are serialized by sharded locks
// (ARCHITECTURE.md §7.2).
type Store struct {
	kv   KV
	blob Blob

	metaMu    sync.Mutex
	accountMu [64]sync.Mutex
}

// New builds a Store over a KV and a Blob. The caller owns both and must
// close them (Close is not part of the facade to keep ownership explicit).
func New(kv KV, blob Blob) *Store {
	return &Store{kv: kv, blob: blob}
}

// CreateAccount registers a new account and returns its ID. Duplicate emails
// are rejected with ErrExists.
func (s *Store) CreateAccount(_ context.Context, email string) (AccountID, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	if _, err := s.kv.Get(MetaEmailKey(email)); err == nil {
		return 0, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	next, err := s.metaCounter()
	if err != nil {
		return 0, err
	}
	id := uint32(next + 1)
	ops := []Op{
		{Key: MetaNextAccountKey(), Value: beUint64(next + 1)},
		{Key: AccountKey(id), Value: []byte(email)},
		{Key: MetaEmailKey(email), Value: beUint32(id)},
	}
	if err := s.kv.Batch(ops); err != nil {
		return 0, err
	}
	return AccountID(id), nil
}

// AccountByEmail resolves an email to its account ID.
func (s *Store) AccountByEmail(_ context.Context, email string) (AccountID, error) {
	v, err := s.kv.Get(MetaEmailKey(email))
	if err != nil {
		return 0, err
	}
	if len(v) != 4 {
		return 0, errors.New("store: corrupt email index")
	}
	return AccountID(binary.BigEndian.Uint32(v)), nil
}

// AccountEmail returns the email of an account.
func (s *Store) AccountEmail(_ context.Context, id AccountID) (string, error) {
	v, err := s.kv.Get(AccountKey(uint32(id)))
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// ListAccounts returns every account email in creation order (scans the
// email registry). Used by migration and provisioning tooling.
func (s *Store) ListAccounts(_ context.Context) ([]string, error) {
	prefix := []byte{SpaceMeta}
	prefix = append(prefix, "email:"...)
	var emails []string
	err := s.kv.Scan(prefix, func(k, _ []byte) error {
		if len(k) > len(prefix) {
			emails = append(emails, string(k[len(prefix):]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return emails, nil
}

// PutBlob stores an immutable blob (message bodies, attachments). size is
// the exact byte count when known; -1 defers to the backend (S3 falls back
// to multipart).
func (s *Store) PutBlob(ctx context.Context, id string, size int64, r io.Reader) (int64, error) {
	return s.blob.Put(ctx, id, size, r)
}

// GetRaw reads an engine-owned metadata key. These keys live outside the
// account document model (vacation state, future lease-like metadata).
func (s *Store) GetRaw(_ context.Context, key []byte) ([]byte, error) {
	return s.kv.Get(key)
}

// PutRaw writes an engine-owned metadata key.
func (s *Store) PutRaw(_ context.Context, key, value []byte) error {
	return s.kv.Put(key, value)
}

// DeleteRaw removes an engine-owned metadata key.
func (s *Store) DeleteRaw(_ context.Context, key []byte) error {
	return s.kv.Delete(key)
}

// ScanRaw visits every raw key with the given prefix in ascending order.
// Returning an error from fn aborts the scan and is propagated.
func (s *Store) ScanRaw(_ context.Context, prefix []byte, fn func(key, value []byte) error) error {
	return s.kv.Scan(prefix, fn)
}

// MailboxModSeq returns the mailbox's current modification sequence
// (CONDSTORE HIGHESTMODSEQ; 0 when the mailbox has no modseq yet).
func (s *Store) MailboxModSeq(ctx context.Context, accountID AccountID, mbID uint64) (uint64, error) {
	fields, err := s.GetDocumentFields(ctx, accountID, CollectionMailbox, mbID)
	if err != nil {
		return 0, err
	}
	return beUint64Value(fields[FieldMailboxModSeq]), nil
}

// BumpMailboxModSeq increments the mailbox modification sequence under the
// account lock and returns the new value. CONDSTORE semantics: every change
// to messages in the mailbox (append/flag/expunge/move-in) bumps it.
func (s *Store) BumpMailboxModSeq(ctx context.Context, accountID AccountID, mbID uint64) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	key := FieldKey(uint32(accountID), CollectionMailbox, mbID, FieldMailboxModSeq)
	next, err := s.kv.Get(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	modseq := beUint64Value(next) + 1
	if err := s.kv.Put(key, beUint64(modseq)); err != nil {
		return 0, err
	}
	return modseq, nil
}

// GetBlob streams a blob to w.
func (s *Store) GetBlob(ctx context.Context, id string, w io.Writer) error {
	return s.blob.Get(ctx, id, w)
}

// DeleteBlob removes a blob.
func (s *Store) DeleteBlob(ctx context.Context, id string) error {
	return s.blob.Delete(ctx, id)
}

// NextCounter allocates the next value of a per-account counter (UIDs,
// change IDs, …) atomically under the account lock (INV-UID).
func (s *Store) NextCounter(_ context.Context, accountID AccountID, kind byte, sub []byte) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()
	key := CounterKey(uint32(accountID), kind, sub)
	next, err := s.getCounter(key)
	if err != nil {
		return 0, err
	}
	next++
	if err := s.kv.Put(key, beUint64(next)); err != nil {
		return 0, err
	}
	return next, nil
}

// CounterValue reads the current value of a per-account counter (0 when
// never allocated). The next allocation is CounterValue+1.
func (s *Store) CounterValue(_ context.Context, accountID AccountID, kind byte, sub []byte) (uint64, error) {
	return s.getCounter(CounterKey(uint32(accountID), kind, sub))
}

// AppendEmailAtomically commits an Email document together with its blob
// link, quota accounting and change-log entry in one KV batch
// (INV-BLOB / INV-QUOTA / INV-CHANGE). Callers allocate the UID and document
// ID first (gaps are harmless; reuse is not).
func (s *Store) AppendEmailAtomically(
	_ context.Context,
	accountID AccountID,
	collection byte,
	docID uint64,
	fields map[byte][]byte,
	blobID string,
	size int64,
) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	ops := make([]Op, 0, len(fields)+5)
	for field, value := range fields {
		ops = append(ops, Op{Key: FieldKey(uint32(accountID), collection, docID, field), Value: value})
	}

	// Blob link (reference count +1).
	linkKey := BlobLinkKey(uint32(accountID), blobID)
	refs, err := s.blobRefs(linkKey)
	if errors.Is(err, ErrNotFound) {
		refs = 0
	} else if err != nil {
		return err
	}
	ops = append(ops, Op{Key: linkKey, Value: beUint64(uint64(refs + 1))})

	// Quota (used bytes + size).
	quotaKey := QuotaKey(uint32(accountID))
	cur, err := s.quotaCounter(quotaKey)
	if err != nil {
		return err
	}
	ops = append(ops, Op{Key: quotaKey, Value: beUint64(uint64(cur + size))})

	// Change log (allocate the next change ID in the same batch).
	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)
	nextChange, err := s.getCounter(changeCounter)
	if err != nil {
		return err
	}
	nextChange++
	ops = append(ops,
		Op{Key: changeCounter, Value: beUint64(nextChange)},
		Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: encodeChangeValue(collection, docID, OpCreate)},
	)
	return s.kv.Batch(ops)
}

// DeleteEmailAtomically removes an Email document together with its blob
// link, quota accounting and a delete changelog entry in one batch
// (INV-BLOB / INV-QUOTA / INV-CHANGE). The blob itself is garbage-collected
// by the caller when the link count reaches zero.
func (s *Store) DeleteEmailAtomically(
	_ context.Context,
	accountID AccountID,
	collection byte,
	docID uint64,
) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	prefix := DocumentKey(uint32(accountID), collection, docID)
	var blobID string
	var size int64
	var ops []Op
	err := s.kv.Scan(prefix, func(k, v []byte) error {
		key := append([]byte(nil), k...)
		ops = append(ops, Op{Key: key, Delete: true})
		switch field := k[len(k)-1]; field {
		case EmailFieldBlob:
			blobID = string(v)
		case EmailFieldSize:
			if len(v) == 8 {
				size = int64(binary.BigEndian.Uint64(v))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return ErrNotFound
	}

	// Blob link (reference count -1).
	if blobID != "" {
		linkKey := BlobLinkKey(uint32(accountID), blobID)
		refs, err := s.blobRefs(linkKey)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if refs > 1 {
			ops = append(ops, Op{Key: linkKey, Value: beUint64(uint64(refs - 1))})
		} else {
			ops = append(ops, Op{Key: linkKey, Delete: true})
		}
	}

	// Quota (used bytes - size).
	if size != 0 {
		quotaKey := QuotaKey(uint32(accountID))
		cur, err := s.quotaCounter(quotaKey)
		if err != nil {
			return err
		}
		if cur-size < 0 {
			return errors.New("store: quota underflow on delete")
		}
		ops = append(ops, Op{Key: quotaKey, Value: beUint64(uint64(cur - size))})
	}

	// Change log (delete).
	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)
	nextChange, err := s.getCounter(changeCounter)
	if err != nil {
		return err
	}
	nextChange++
	ops = append(ops,
		Op{Key: changeCounter, Value: beUint64(nextChange)},
		Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: encodeChangeValue(collection, docID, OpDelete)},
	)
	return s.kv.Batch(ops)
}

// UpdateDocumentAtomically writes fields and a changelog update entry in one
// batch (INV-CHANGE: flag/keyword/mailbox moves are observable changes).
func (s *Store) UpdateDocumentAtomically(
	_ context.Context,
	accountID AccountID,
	collection byte,
	docID uint64,
	fields map[byte][]byte,
) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	ops := make([]Op, 0, len(fields)+2)
	for field, value := range fields {
		ops = append(ops, Op{Key: FieldKey(uint32(accountID), collection, docID, field), Value: value})
	}
	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)
	nextChange, err := s.getCounter(changeCounter)
	if err != nil {
		return err
	}
	nextChange++
	ops = append(ops,
		Op{Key: changeCounter, Value: beUint64(nextChange)},
		Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: encodeChangeValue(collection, docID, OpUpdate)},
	)
	return s.kv.Batch(ops)
}

func (s *Store) metaCounter() (uint64, error) {
	v, err := s.kv.Get(MetaNextAccountKey())
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(v), nil
}

func (s *Store) lockAccount(id AccountID) func() {
	mu := &s.accountMu[uint32(id)%uint32(len(s.accountMu))]
	mu.Lock()
	return mu.Unlock
}

func (s *Store) getCounter(key []byte) (uint64, error) {
	v, err := s.kv.Get(key)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("store: corrupt counter")
	}
	return binary.BigEndian.Uint64(v), nil
}

func beUint32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func beUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// beUint64Value decodes a big-endian uint64 field; missing or short values
// decode as zero.
func beUint64Value(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
