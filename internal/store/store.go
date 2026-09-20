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
	"strings"
	"sync"
)

// AccountID is a globally unique account identifier.
type AccountID uint32

// Canonical Email document field ids (ARCHITECTURE.md §2.1). This is the
// single source of truth for the full field table: mailstore aliases it on
// the write path, and the atomic-delete accounting below references the same
// constants so quota/blob bookkeeping can never drift from writes.
const (
	EmailFieldBlob     byte = 1
	EmailFieldUID      byte = 2
	EmailFieldMailbox  byte = 3
	EmailFieldFlags    byte = 4
	EmailFieldDate     byte = 5
	EmailFieldFrom     byte = 6
	EmailFieldSize     byte = 7
	EmailFieldKeywords byte = 8
	EmailFieldModSeq   byte = 9
	// EmailFieldHeader is the message's header block (through the blank line
	// that ends it), cached at delivery so envelope fetches do not have to
	// read the whole message blob. Absent on documents written before it
	// existed; empty means "no usable header block" (see mailstore).
	EmailFieldHeader byte = 10
	// 11 is reserved: an unreleased build cached the non-extended BODY form
	// there. The cached form is the extended BODYSTRUCTURE one, and reusing the
	// id would let a stale payload be replayed as an extended structure.
	//
	// EmailFieldBodyStructureExt is the message's body structure in its
	// extended BODYSTRUCTURE form, cached at delivery so callers can report it
	// without reading the whole message blob. Absent on documents written
	// before it existed; empty means "not cached" (see mailstore).
	EmailFieldBodyStructureExt byte = 12
)

// Store is a KV+Blob facade exposing account-scoped operations. It is safe
// for concurrent use; conflicting mutations are serialized by sharded locks
// as a single-node optimization, while correctness under concurrency is
// carried by the backend transaction itself (TxnKV.WithTxn). Native TiDB
// deployments can therefore run many processes against one cluster; buffer
// backends (Pebble/MemoryKV) keep today's single-writer semantics.
type Store struct {
	kv   KV
	blob Blob

	metaMu    sync.Mutex
	accountMu [64]sync.Mutex

	txn TxnKV
}

// New builds a Store over a KV and a Blob. The caller owns both and must
// close them (Close is not part of the facade to keep ownership explicit).
// Plain KV engines are transparently upgraded with AsTxn.
func New(kv KV, blob Blob) *Store {
	return &Store{kv: kv, blob: blob, txn: AsTxn(kv)}
}

// CreateAccount registers a new account and returns its ID. Duplicate emails
// are rejected with ErrExists. The email registry is guarded by metaMu
// single-node; under TiDB the existence check lives inside the transaction,
// so concurrent creators race on the key itself and exactly one commits.
func (s *Store) CreateAccount(ctx context.Context, email string) (AccountID, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	var id AccountID
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		if _, err := t.Get(MetaEmailKey(email)); err == nil {
			return ErrExists
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		next, err := metaNext(t.Get)
		if err != nil {
			return err
		}
		nid := uint32(next + 1)
		t.Append(
			Op{Key: MetaNextAccountKey(), Value: beUint64(next + 1)},
			Op{Key: AccountKey(nid), Value: []byte(email)},
			Op{Key: MetaEmailKey(email), Value: beUint32(nid)},
		)
		id = AccountID(nid)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
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

// ScanRawRange visits every raw key in [start, end) in ascending order.
// Returning an error from fn aborts the scan and is propagated.
func (s *Store) ScanRawRange(_ context.Context, start, end []byte, fn func(key, value []byte) error) error {
	return s.kv.ScanRange(start, end, fn)
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
	var modseq uint64
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		cur, err := t.Get(key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		modseq = beUint64Value(cur) + 1
		t.Put(key, beUint64(modseq))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return modseq, nil
}

// DeliverEmail commits one delivered message in a SINGLE account-locked
// transaction: UID allocation (INV-UID, per mailbox identity), document ID
// allocation (INV-UID, per collection), the mailbox modseq bump (CONDSTORE),
// the message fields, the blob link (INV-BLOB), quota accounting (INV-QUOTA)
// and the change-log entry (INV-CHANGE) plus any extra ops (secondary
// index). The blob DATA must already be in the blob store under blobID —
// callers upload it outside the account lock so concurrent deliveries
// pipeline. modseq is allocated here and written as the message's
// fieldModSeq; fields must not contain it.
func (s *Store) DeliverEmail(
	ctx context.Context,
	accountID AccountID,
	mbID uint64,
	fields map[byte][]byte,
	blobID string,
	size int64,
	extra ...Op,
) (uid uint64, modseq uint64, docID uint64, err error) {
	unlock := s.lockAccount(accountID)
	defer unlock()
	err = s.txn.WithTxn(ctx, func(t TxnOps) error {
		uid, modseq, docID, err = s.deliverOne(t, accountID, mbID, fields, blobID, size, extra...)
		return err
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return uid, modseq, docID, nil
}

// DeliverRequest is one message staged for DeliverEmailBatch. Same contract
// as the DeliverEmail parameters.
type DeliverRequest struct {
	MailboxID uint64
	Fields    map[byte][]byte
	BlobID    string
	Size      int64
}

// DeliverResult reports the identities allocated for one DeliverRequest.
type DeliverResult struct {
	UID    uint64
	ModSeq uint64
	DocID  uint64
}

// DeliverEmailBatch commits k delivered messages under ONE account lock and
// ONE transaction — one fsync instead of k. The resulting storage state is
// identical to k sequential DeliverEmail calls: each message still gets its
// own sequential UID, document ID, modseq bump, change-log entry, blob link
// and index entry; the KV write buffer collapses the repeated counter
// writes into their final values. This is the delivery-side micro-batch:
// under load several SMTP arrivals queue while the leader holds the account
// gate, and they drain into a single commit.
func (s *Store) DeliverEmailBatch(ctx context.Context, accountID AccountID, reqs []DeliverRequest) ([]DeliverResult, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()
	res := make([]DeliverResult, len(reqs))
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		for i := range reqs {
			r := &reqs[i]
			uid, modseq, docID, err := s.deliverOne(t, accountID, r.MailboxID, r.Fields, r.BlobID, r.Size)
			if err != nil {
				return err
			}
			res[i] = DeliverResult{UID: uid, ModSeq: modseq, DocID: docID}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// deliverOne stages exactly one message's mutations (see DeliverEmail).
// Counters are written per call; the transaction write buffer's
// last-write-wins collapsing keeps repeated writes of the same key cheap
// and correct.
func (s *Store) deliverOne(
	t TxnOps,
	accountID AccountID,
	mbID uint64,
	fields map[byte][]byte,
	blobID string,
	size int64,
	extra ...Op,
) (uid uint64, modseq uint64, docID uint64, err error) {
	uidKey := CounterKey(uint32(accountID), CounterKindNextDoc, append([]byte{CollectionEmail}, beUint64(mbID)...))
	docKey := CounterKey(uint32(accountID), CounterKindNextDoc, []byte{CollectionEmail})
	modseqKey := FieldKey(uint32(accountID), CollectionMailbox, mbID, FieldMailboxModSeq)
	quotaKey := QuotaKey(uint32(accountID))
	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)

	cur, err := readCounter(t.Get, uidKey)
	if err != nil {
		return 0, 0, 0, err
	}
	uid = cur + 1
	t.Put(uidKey, beUint64(uid))

	curMs, err := t.Get(modseqKey)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, 0, 0, err
	}
	modseq = beUint64Value(curMs) + 1
	t.Put(modseqKey, beUint64(modseq))

	curDoc, err := readCounter(t.Get, docKey)
	if err != nil {
		return 0, 0, 0, err
	}
	docID = curDoc + 1
	t.Append(
		Op{Key: docKey, Value: beUint64(docID)},
		Op{Key: FieldKey(uint32(accountID), CollectionEmail, docID, FieldMeta), Value: []byte{1}},
	)

	full := make(map[byte][]byte, len(fields)+2)
	for field, value := range fields {
		full[field] = value
	}
	full[EmailFieldUID] = beUint64(uid)
	full[EmailFieldModSeq] = beUint64(modseq)
	t.Append(orderedFieldOps(accountID, CollectionEmail, docID, full)...)

	if err := stageBlobLink(t, accountID, blobID); err != nil {
		return 0, 0, 0, err
	}

	curQuota, err := quotaValue(t.Get, quotaKey)
	if err != nil {
		return 0, 0, 0, err
	}
	t.Append(Op{Key: quotaKey, Value: beUint64(uint64(curQuota + size))})

	nextChange, err := readCounter(t.Get, changeCounter)
	if err != nil {
		return 0, 0, 0, err
	}
	nextChange++
	t.Append(
		Op{Key: changeCounter, Value: beUint64(nextChange)},
		Op{Key: ChangeLogKey(uint32(accountID), CollectionEmail, nextChange), Value: encodeChangeValue(CollectionEmail, docID, OpCreate)},
	)

	// Per-mailbox secondary index: (account, mailbox, UID) -> docID.
	t.Append(Op{Key: IndexEmailKey(uint32(accountID), mbID, uid), Value: beUint64(docID)})

	t.Append(extra...)
	return uid, modseq, docID, nil
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
// change IDs, …). The allocation reads and writes the counter inside one
// transaction (INV-UID); the account lock remains as a contention filter.
func (s *Store) NextCounter(ctx context.Context, accountID AccountID, kind byte, sub []byte) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()
	key := CounterKey(uint32(accountID), kind, sub)
	var next uint64
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		cur, err := readCounter(t.Get, key)
		if err != nil {
			return err
		}
		next = cur + 1
		t.Put(key, beUint64(next))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// CounterValue reads the current value of a per-account counter (0 when
// never allocated). The next allocation is CounterValue+1.
func (s *Store) CounterValue(_ context.Context, accountID AccountID, kind byte, sub []byte) (uint64, error) {
	return readCounter(s.kv.Get, CounterKey(uint32(accountID), kind, sub))
}

// AppendEmailAtomically commits an Email document together with its blob
// link, quota accounting and change-log entry in one KV batch
// (INV-BLOB / INV-QUOTA / INV-CHANGE). Callers allocate the UID and document
// ID first (gaps are harmless; reuse is not). Extra ops (e.g. secondary
// index maintenance) commit in the same batch.
func (s *Store) AppendEmailAtomically(
	ctx context.Context,
	accountID AccountID,
	collection byte,
	docID uint64,
	fields map[byte][]byte,
	blobID string,
	size int64,
	extra ...Op,
) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	quotaKey := QuotaKey(uint32(accountID))
	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)

	return s.txn.WithTxn(ctx, func(t TxnOps) error {
		t.Append(orderedFieldOps(accountID, collection, docID, fields)...)

		// Blob link (reference count +1), staged so a replayed optimistic
		// transaction cannot double-increment.
		if err := stageBlobLink(t, accountID, blobID); err != nil {
			return err
		}

		// Quota (used bytes + size).
		cur, err := quotaValue(t.Get, quotaKey)
		if err != nil {
			return err
		}
		t.Append(Op{Key: quotaKey, Value: beUint64(uint64(cur + size))})

		// Change log (allocate the next change ID in the same commit).
		nextChange, err := readCounter(t.Get, changeCounter)
		if err != nil {
			return err
		}
		nextChange++
		t.Append(
			Op{Key: changeCounter, Value: beUint64(nextChange)},
			Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: encodeChangeValue(collection, docID, OpCreate)},
		)

		t.Append(extra...)
		return nil
	})
}

// orderedFieldOps converts a field map into per-field ops. The buffer keeps
// map iteration order irrelevant: each key lands at most once either way.
func orderedFieldOps(accountID AccountID, collection byte, docID uint64, fields map[byte][]byte) []Op {
	ops := make([]Op, 0, len(fields))
	for field, value := range fields {
		ops = append(ops, Op{Key: FieldKey(uint32(accountID), collection, docID, field), Value: value})
	}
	return ops
}

// ErrNotMarkedDeleted is returned by DeleteEmailAtomically when a
// requireFlag was requested and the message's live flags (read inside the
// delete transaction) do not carry it. No rows are written; the caller must
// treat the message as not expunged.
var ErrNotMarkedDeleted = errors.New("store: message not marked for deletion")

// hasListToken reports whether the comma-joined system-flag list contains
// want exactly (no substring matches across flag boundaries). IMAP flags are
// case-insensitive, so the comparison folds case.
func hasListToken(list, want string) bool {
	for list != "" {
		var tok string
		if i := strings.IndexByte(list, ','); i >= 0 {
			tok, list = list[:i], list[i+1:]
		} else {
			tok, list = list, ""
		}
		if strings.EqualFold(tok, want) {
			return true
		}
	}
	return false
}

// DeleteEmailAtomically removes an Email document together with its blob
// link, quota accounting and a delete changelog entry in one batch
// (INV-BLOB / INV-QUOTA / INV-CHANGE). The blob itself is reclaimed by the
// sweep (SweepBlobs) once no account links it. Extra ops (e.g. secondary
// index removal) commit in the same batch.
//
// requireFlag (empty = no check) is verified inside the transaction against
// the message's live flags: UID EXPUNGE must only delete messages that are
// still marked \Deleted, and a session's snapshot of that flag can be stale
// — the authoritative check has to share the delete's atomic commit.
func (s *Store) DeleteEmailAtomically(
	ctx context.Context,
	accountID AccountID,
	collection byte,
	docID uint64,
	requireFlag string,
	extra ...Op,
) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	prefix := DocumentKey(uint32(accountID), collection, docID)
	quotaKey := QuotaKey(uint32(accountID))
	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)

	return s.txn.WithTxn(ctx, func(t TxnOps) error {
		var blobID string
		var size int64
		var mailbox string
		var uid uint32
		var flags string
		var ops []Op
		// Scan INSIDE the transaction (read-your-writes): the read set joins
		// the commit, so a concurrent writer racing the delete either
		// conflicts (optimistic backends replay the closure) or waits
		// behind the account lock — a plain outside scan could read fields
		// a racing append had not yet committed.
		err := t.Scan(prefix, func(k, v []byte) error {
			key := append([]byte(nil), k...)
			ops = append(ops, Op{Key: key, Delete: true})
			switch field := k[len(k)-1]; field {
			case EmailFieldBlob:
				blobID = string(v)
			case EmailFieldFlags:
				flags = string(v)
			case EmailFieldSize:
				if len(v) == 8 {
					size = int64(binary.BigEndian.Uint64(v))
				}
			case EmailFieldMailbox:
				mailbox = string(v)
			case EmailFieldUID:
				if len(v) == 8 {
					uid = uint32(binary.BigEndian.Uint64(v))
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
		// Authoritative \Deleted check (UID EXPUNGE): evaluated against the
		// flags read in THIS transaction, so a concurrent flag clear between
		// the caller's snapshot and here aborts the delete instead of
		// destroying an unmarked message. Staged ops roll back on closure
		// error.
		if requireFlag != "" && !hasListToken(flags, requireFlag) {
			return ErrNotMarkedDeleted
		}
		t.Append(ops...)

		// Blob link (reference count -1). A missing link (refs already
		// zeroed by a crash between commit and cleanup) still deletes the
		// document: the link invariant is restored by the delete itself.
		if blobID != "" {
			if err := stageBlobUnlink(t, accountID, blobID); err != nil {
				if !errors.Is(err, ErrNotFound) {
					return err
				}
				t.Delete(BlobLinkKey(uint32(accountID), blobID))
			}
		}

		// Quota (used bytes - size), clamped at zero: a counter that drifted
		// after a crash must not make the message impossible to expunge.
		if size != 0 {
			cur, err := quotaValue(t.Get, quotaKey)
			if err != nil {
				return err
			}
			nv := cur - size
			if nv < 0 {
				nv = 0
			}
			t.Append(Op{Key: quotaKey, Value: beUint64(uint64(nv))})
		}

		// Change log (allocate the next change ID in the same commit).
		// Email deletes carry the copy's (mailbox, UID): derived-state
		// consumers cannot recover them after the row is gone.
		nextChange, err := readCounter(t.Get, changeCounter)
		if err != nil {
			return err
		}
		nextChange++
		changeVal := encodeChangeValue(collection, docID, OpDelete)
		if collection == CollectionEmail && mailbox != "" && uid != 0 {
			changeVal = encodeEmailDeleteValue(collection, docID, mailbox, uid)
		}
		t.Append(
			Op{Key: changeCounter, Value: beUint64(nextChange)},
			Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: changeVal},
		)
		t.Append(extra...)
		return nil
	})
}

// UpdateDocumentAtomically writes fields and a changelog update entry in one
// batch (INV-CHANGE: flag/keyword/mailbox moves are observable changes).
// Extra ops (e.g. secondary index maintenance) commit in the same batch.
func (s *Store) UpdateDocumentAtomically(
	ctx context.Context,
	accountID AccountID,
	collection byte,
	docID uint64,
	fields map[byte][]byte,
	extra ...Op,
) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)

	return s.txn.WithTxn(ctx, func(t TxnOps) error {
		t.Append(orderedFieldOps(accountID, collection, docID, fields)...)
		nextChange, err := readCounter(t.Get, changeCounter)
		if err != nil {
			return err
		}
		nextChange++
		t.Append(
			Op{Key: changeCounter, Value: beUint64(nextChange)},
			Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: encodeChangeValue(collection, docID, OpUpdate)},
		)
		t.Append(extra...)
		return nil
	})
}

// DocUpdate is one document's field replacement inside a batched write.
type DocUpdate struct {
	DocID  uint64
	Fields map[byte][]byte
}

// UpdateDocumentsAtomically stages several documents' fields and their
// changelog entries in one transaction: the change counter is read once and
// the batch shares a single commit. Each document still gets its own
// changelog row, exactly as UpdateDocumentAtomically would write it.
func (s *Store) UpdateDocumentsAtomically(
	ctx context.Context,
	accountID AccountID,
	collection byte,
	docs []DocUpdate,
	extra ...Op,
) error {
	if len(docs) == 0 {
		return nil
	}
	unlock := s.lockAccount(accountID)
	defer unlock()

	changeCounter := CounterKey(uint32(accountID), CounterKindChange, nil)

	return s.txn.WithTxn(ctx, func(t TxnOps) error {
		nextChange, err := readCounter(t.Get, changeCounter)
		if err != nil {
			return err
		}
		for _, d := range docs {
			t.Append(orderedFieldOps(accountID, collection, d.DocID, d.Fields)...)
			nextChange++
			t.Append(
				Op{Key: changeCounter, Value: beUint64(nextChange)},
				Op{Key: ChangeLogKey(uint32(accountID), collection, nextChange), Value: encodeChangeValue(collection, d.DocID, OpUpdate)},
			)
		}
		t.Append(extra...)
		return nil
	})
}

// blobRefValue decodes a blob link count through get.
func blobRefValue(get func([]byte) ([]byte, error), key []byte) (int64, error) {
	v, err := get(key)
	if errors.Is(err, ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("store: corrupt blob link")
	}
	return int64(binary.BigEndian.Uint64(v)), nil
}

// quotaValue decodes the quota counter through get; absent keys decode as 0.
func quotaValue(get func([]byte) ([]byte, error), key []byte) (int64, error) {
	v, err := get(key)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("store: corrupt quota counter")
	}
	return int64(binary.BigEndian.Uint64(v)), nil
}

// metaNext decodes the global account-id allocator through get (0 when the
// key has never been written).
func metaNext(get func([]byte) ([]byte, error)) (uint64, error) {
	v, err := get(MetaNextAccountKey())
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("store: corrupt account allocator")
	}
	return binary.BigEndian.Uint64(v), nil
}

// readCounter decodes a big-endian uint64 counter through get; absent keys
// decode as zero.
func readCounter(get func([]byte) ([]byte, error), key []byte) (uint64, error) {
	v, err := get(key)
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

func (s *Store) lockAccount(id AccountID) func() {
	mu := &s.accountMu[uint32(id)%uint32(len(s.accountMu))]
	mu.Lock()
	return mu.Unlock
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
