// KV is a mailstore over the logical store (account → collection → document
// + blob), for the Pebble/TiDB + MinIO/FS storage modes.
package mailstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
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
	s          *store.Store
	batch      deliverBatcher
	batchLimit int
	// idCache memoizes hot-path id resolutions: account email → account
	// ID, and (account, mailbox name) → mailbox doc ID. Every delivery
	// resolves both and every Poll version gate resolves the mailbox, so
	// on single-node Pebble this saves a few point reads per operation
	// and over TiDB a network round trip each. Doc IDs are stable and
	// never reused, so entries are invalidated on the only events that
	// can stale them — mailbox delete/rename and account purge; the TTL
	// bounds the cross-node staleness window the same way the metadata
	// cache's TTL does (process-local invalidation cannot observe another
	// node's admin operations in multi-active mode). Positive entries
	// only: a cached "not found" would delay recognition of freshly
	// created accounts and mailboxes.
	idCache sync.Map
}

// idCacheTTL bounds how long an id resolution is trusted. See the KV.idCache
// note on invalidation and the cross-node staleness window.
const idCacheTTL = time.Minute

type idCacheEntry struct {
	id      uint64
	expires time.Time
}

func idCacheKeyAccount(email string) string {
	return "a\x00" + email
}

func idCacheKeyMailbox(acctID store.AccountID, mailbox string) string {
	return "m\x00" + strconv.FormatUint(uint64(acctID), 10) + "\x00" + mailbox
}

// cachedAccountID resolves an account email to its ID through idCache,
// falling back to a fresh store read on miss. Only successful resolutions
// are memoized.
func (k *KV) cachedAccountID(ctx context.Context, email string) (store.AccountID, error) {
	if v, ok := k.idCache.Load(idCacheKeyAccount(email)); ok {
		if e := v.(*idCacheEntry); time.Now().Before(e.expires) {
			return store.AccountID(e.id), nil
		}
	}
	acctID, err := k.s.AccountByEmail(ctx, email)
	if err != nil {
		return 0, err
	}
	k.idCache.Store(idCacheKeyAccount(email), &idCacheEntry{id: uint64(acctID), expires: time.Now().Add(idCacheTTL)})
	return acctID, nil
}

// cachedMailboxDocID resolves a mailbox name to its document ID through
// idCache, falling back to mailboxDocID's index read (with its repair
// fallback) on miss. Only successful resolutions are memoized.
func (k *KV) cachedMailboxDocID(ctx context.Context, acctID store.AccountID, mailbox string) (uint64, error) {
	key := idCacheKeyMailbox(acctID, mailbox)
	if v, ok := k.idCache.Load(key); ok {
		if e := v.(*idCacheEntry); time.Now().Before(e.expires) {
			return e.id, nil
		}
	}
	id, err := k.mailboxDocID(ctx, acctID, mailbox)
	if err != nil {
		return 0, err
	}
	k.idCache.Store(key, &idCacheEntry{id: id, expires: time.Now().Add(idCacheTTL)})
	return id, nil
}

func (k *KV) invalidateMailboxID(acctID store.AccountID, mailbox string) {
	k.idCache.Delete(idCacheKeyMailbox(acctID, mailbox))
}

// invalidateAccountIDs drops the account's own resolution plus every cached
// mailbox resolution under its ID (prefix range over the small id cache —
// account purge is a rare administrative operation).
func (k *KV) invalidateAccountIDs(email string, acctID store.AccountID) {
	k.idCache.Delete(idCacheKeyAccount(email))
	prefix := "m\x00" + strconv.FormatUint(uint64(acctID), 10) + "\x00"
	k.idCache.Range(func(key, _ any) bool {
		if ks, ok := key.(string); ok && strings.HasPrefix(ks, prefix) {
			k.idCache.Delete(ks)
		}
		return true
	})
}

// NewKV wraps a store facade.
func NewKV(s *store.Store) *KV {
	return &KV{s: s, batchLimit: defaultDeliverBatchLimit}
}

// deliverBatchLimit bounds one micro-batch: the number of queued messages a
// leader drains into a single transaction. It bounds txn memory under
// sustained overload; excess stays queued for the next leader.
const defaultDeliverBatchLimit = 256

// deliverRequest is one staged delivery waiting to join a batch. All the
// expensive pre-lock work (blob upload, field building) has already happened
// by the time a request is staged.
type deliverRequest struct {
	mbID   uint64
	fields map[byte][]byte
	blobID string
	size   int64
	done   chan deliverResult
}

type deliverResult struct {
	uid uint32
	err error
}

// perAccountBatch coalesces concurrent same-account deliveries. Exactly one
// goroutine is the leader at a time: it keeps draining staged requests and
// committing them batch-by-batch until the queue runs dry, then resigns.
// Arrivals during a commit queue behind the leader and join its NEXT batch —
// the same equilibrium RocksDB group commit reaches, where the WAL fsync is
// amortized over the whole arrival burst. No background goroutines, no
// timers, nothing to shut down.
type perAccountBatch struct {
	mu     sync.Mutex
	queue  []*deliverRequest
	active bool
}

// deliverBatcher owns one perAccountBatch per account.
type deliverBatcher struct {
	mu    sync.Mutex
	accts map[store.AccountID]*perAccountBatch
}

// submit stages req. It returns false when the caller becomes the leader
// (no batch is in flight) — the caller must run the commit loop in runBatch.
// It returns true when a leader is active: req is queued and that leader
// will deliver its result to req.done.
func (b *deliverBatcher) submit(id store.AccountID, req *deliverRequest) bool {
	b.mu.Lock()
	ab := b.accts[id]
	if ab == nil {
		ab = &perAccountBatch{}
		if b.accts == nil {
			b.accts = make(map[store.AccountID]*perAccountBatch)
		}
		b.accts[id] = ab
	}
	b.mu.Unlock()

	ab.mu.Lock()
	if ab.active {
		// A leader is committing: join the queue, wait for its result.
		ab.queue = append(ab.queue, req)
		ab.mu.Unlock()
		return true
	}
	ab.active = true
	ab.mu.Unlock()
	return false
}

// take hands the leader up to limit queued requests. It does NOT touch the
// active flag — resignation happens in resignIfEmpty after the leader's last
// commit has finished, so arrivals during a commit are always served.
func (b *deliverBatcher) take(id store.AccountID, limit int) []*deliverRequest {
	b.mu.Lock()
	ab := b.accts[id]
	b.mu.Unlock()
	if ab == nil {
		return nil
	}
	ab.mu.Lock()
	n := len(ab.queue)
	if n > limit {
		n = limit
	}
	batch := ab.queue[:n:n]
	ab.queue = ab.queue[n:]
	ab.mu.Unlock()
	return batch
}

// resignIfEmpty retires the leadership when no requests are queued. It
// reports true when leadership was handed off (queue empty): the caller's
// commit loop ends. A non-empty queue means arrivals raced in during the
// last commit and the same leader keeps serving them.
func (b *deliverBatcher) resignIfEmpty(id store.AccountID) bool {
	b.mu.Lock()
	ab := b.accts[id]
	b.mu.Unlock()
	if ab == nil {
		return true
	}
	ab.mu.Lock()
	if len(ab.queue) == 0 {
		ab.active = false
		ab.mu.Unlock()
		return true
	}
	ab.mu.Unlock()
	return false
}

// failAll hands every queued request the same error (commit-path abort).
func (b *deliverBatcher) failAll(id store.AccountID, err error) {
	for {
		batch := b.take(id, 1<<30)
		if len(batch) == 0 {
			return
		}
		for _, r := range batch {
			r.done <- deliverResult{err: err}
		}
	}
}

func (k *KV) runBatch(ctx context.Context, acctID store.AccountID, req *deliverRequest) (uint32, error) {
	if k.batch.submit(acctID, req) {
		res := <-req.done
		return res.uid, res.err
	}
	// Leader loop: commit the staged requests (self first), then keep
	// draining whatever queued while we were committing. Batching thus
	// scales with load by itself: the faster requests arrive, the larger
	// the batches. The commit deliberately detaches from the leader's
	// context — an unrelated follower's failure must not doom peers that
	// were batched together.
	commitCtx := context.WithoutCancel(ctx)
	batch := append([]*deliverRequest{req}, k.batch.take(acctID, k.batchLimit)...)
	var out []store.DeliverResult
	var err error
	myUID := uint32(0) // batch[0] is req in the first iteration
	for iter := 0; ; iter++ {
		in := make([]store.DeliverRequest, len(batch))
		for i, r := range batch {
			in[i] = store.DeliverRequest{MailboxID: r.mbID, Fields: r.fields, BlobID: r.blobID, Size: r.size}
		}
		out, err = k.s.DeliverEmailBatch(commitCtx, acctID, in)
		if err != nil {
			err = fmt.Errorf("batch delivery: %w", err)
			break
		}
		if iter == 0 {
			myUID = uint32(out[0].UID)
		}
		for i, r := range batch {
			r.done <- deliverResult{uid: uint32(out[i].UID)}
		}
		if k.batch.resignIfEmpty(acctID) {
			return myUID, nil
		}
		batch = k.batch.take(acctID, k.batchLimit)
	}
	// Commit failed: fail this batch and every queued request, then resign
	// so the account is not wedged.
	for _, r := range batch {
		r.done <- deliverResult{err: err}
	}
	k.batch.failAll(acctID, err)
	k.batch.resignIfEmpty(acctID)
	return 0, err
}

// Deliver stores one message. The blob is content-addressed and uploaded
// BEFORE anything locks the account, so concurrent deliveries pipeline the
// blob I/O; then one account-locked transaction commits UID, modseq,
// document ID, fields, blob link, quota, change log and the per-mailbox
// index together (ARCHITECTURE.md §3.5 ordering: the blob exists before any
// link to it becomes visible). Concurrent delivery used to serialize at
// ~4 KV commits per message; this path takes the account lock once.
func (k *KV) Deliver(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return 0, err
	}
	mbID, err := k.ensureMailbox(ctx, acctID, mailbox)
	if err != nil {
		return 0, err
	}
	data := msg.Data
	if data == nil {
		data = []byte{}
	}
	sum := sha256.Sum256(data)
	blobID := "sha256-" + hex.EncodeToString(sum[:])
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
		fieldMailbox:  []byte(mailbox),
		fieldFlags:    []byte(strings.Join(system, ",")),
		fieldDate:     []byte(date.UTC().Format(time.RFC3339)),
		fieldFrom:     []byte(msg.From),
		fieldSize:     beUint64(uint64(size)),
		fieldKeywords: []byte(strings.Join(keywords, ",")),
	}
	// Delivery micro-batch: join (or lead) a per-account batch so a burst
	// of arrivals commits in ONE transaction/fsync. Idle traffic keeps the
	// single-message path — batching only emerges under load.
	uid, err := k.runBatch(ctx, acctID, &deliverRequest{
		mbID:   mbID,
		fields: fields,
		blobID: blobID,
		size:   size,
		done:   make(chan deliverResult, 1),
	})
	if err != nil {
		return 0, err
	}
	// The sweep may have reclaimed this blob between the PutBlob above and
	// the link commit (it re-creates content that was claimed ≥10 min ago;
	// the sweep's own scan interleaved with that window). The link row is
	// committed — make sure the file exists again. Content-addressed puts
	// are idempotent, so this is always safe.
	if _, serr := k.s.BlobStat(ctx, blobID); serr != nil {
		if _, perr := k.s.PutBlob(ctx, blobID, int64(len(data)), bytes.NewReader(data)); perr != nil {
			return 0, perr
		}
	}
	return uid, nil
}
func (k *KV) ensureAccount(ctx context.Context, account string) (store.AccountID, error) {
	acctID, err := k.cachedAccountID(ctx, account)
	if err == nil {
		return acctID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	acctID, err = k.s.CreateAccount(ctx, account)
	if errors.Is(err, store.ErrExists) {
		// Lost a concurrent create: adopt the winner's registration instead
		// of surfacing a spurious temporary failure.
		return k.cachedAccountID(ctx, account)
	}
	return acctID, err
}

// ensureMailbox returns the mailbox document ID, creating the mailbox (and
// its UID counter) on first use. INBOX is created eagerly so delivery and
// IMAP always see it. Creation is a single atomic check-create transaction:
// concurrent first deliveries to the same new mailbox resolve to exactly one
// document (the loser adopts the winner via conflict replay).
func (k *KV) ensureMailbox(ctx context.Context, acctID store.AccountID, mailbox string) (uint64, error) {
	if id, err := k.cachedMailboxDocID(ctx, acctID, mailbox); err == nil {
		return id, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	return k.s.EnsureMailboxDoc(ctx, acctID, mailbox, func(docID uint64) map[byte][]byte {
		return map[byte][]byte{
			mbFieldName:        []byte(mailbox),
			mbFieldUIDValidity: beUint32(uint32(docID)),
			mbFieldSubscribed:  []byte{0},
		}
	})
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

// QuotaUsedBytes reports the account's used-bytes counter. Appends and
// expunges maintain it transactionally (AppendEmailAtomically /
// DeleteEmailAtomically adjust it in the same commit as the document), so
// this is an O(1) counter read instead of a full document scan.
func (k *KV) QuotaUsedBytes(ctx context.Context, account string) (int64, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil // no account yet ⇒ nothing stored
	}
	if err != nil {
		return 0, err
	}
	return k.s.QuotaUsed(ctx, acctID)
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
