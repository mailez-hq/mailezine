// Package queue implements the outbound delivery queue (ARCHITECTURE.md §5).
//
// Messages are spooled as blobs with metadata in KV; a scheduler walks the
// due index, hands each message to the Deliverer and records per-recipient
// outcomes. The lifecycle follows the classic MTA state machine:
//
//	SUBMITTED → QUEUED → ACTIVE → DELIVERED | DEFERRED → … → BOUNCED | FAILED
//
// Retries use exponential backoff with jitter; a message is terminal when
// every recipient is delivered or bounced.
package queue

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"mailezine/internal/kvlease"
	"mailezine/internal/metrics"
	"mailezine/internal/store"
)

const queueName = "meta"

// State is the queue state of a message.
type State string

// Queue states (ARCHITECTURE.md §5.1).
const (
	StateSubmitted State = "submitted"
	StateQueued    State = "queued"
	StateActive    State = "active"
	StateDelivered State = "delivered"
	StateDeferred  State = "deferred"
	StateBounced   State = "bounced"
	StateFailed    State = "failed"
)

// RecipientStatus tracks one recipient of a message.
type RecipientStatus string

// Recipient statuses.
const (
	RecipientPending   RecipientStatus = "pending"
	RecipientDelivered RecipientStatus = "delivered"
	RecipientBounced   RecipientStatus = "bounced"
)

// Recipient is one envelope recipient with its delivery outcome.
type Recipient struct {
	Address   string          `json:"address"`
	Status    RecipientStatus `json:"status"`
	LastError string          `json:"lastError,omitempty"`
}

// Message is the spooled metadata of one queued message.
type Message struct {
	ID            uint64      `json:"id"`
	From          string      `json:"from"`
	Recipients    []Recipient `json:"recipients"`
	Subject       string      `json:"subject,omitempty"`
	BlobID        string      `json:"blobId"`
	Size          int64       `json:"size"`
	State         State       `json:"state"`
	Attempts      int         `json:"attempts"`
	MaxAttempts   int         `json:"maxAttempts"`
	NextAttempt   time.Time   `json:"nextAttempt"`
	LastError     string      `json:"lastError,omitempty"`
	CreatedAt     time.Time   `json:"createdAt"`
	UpdatedAt     time.Time   `json:"updatedAt"`
	LastWarningAt time.Time   `json:"lastWarningAt,omitempty"`
	// Owner is the node that claimed the ACTIVE delivery; LeaseUntil is
	// when that claim may be stolen (crash recovery). Multi-active only —
	// empty in single-node deployments.
	Owner      string    `json:"owner,omitempty"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
}

// Options tunes the scheduler.
type Options struct {
	MaxAttempts  int
	BaseRetry    time.Duration
	MaxRetry     time.Duration
	PollInterval time.Duration
	Workers      int
	// DelayWarning is the interval after which a still-queued message
	// triggers a delay warning; 0 disables.
	DelayWarning time.Duration
	// NodeID identifies this manager in claim ownership (multi-active
	// deployments); empty auto-generates a per-process ID.
	NodeID string
	// ClaimLease bounds how long one delivery attempt may run before
	// another node may steal the claim. It must comfortably exceed the
	// worst-case single delivery (MX fallbacks, slow recipients); a steal
	// under a live worker turns into an at-least-once duplicate. Default
	// 10m.
	ClaimLease time.Duration
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// DefaultOptions returns production defaults (conservative retry envelope).
func DefaultOptions() Options {
	return Options{
		MaxAttempts:  10,
		BaseRetry:    time.Minute,
		MaxRetry:     24 * time.Hour,
		PollInterval: 5 * time.Second,
		Workers:      16,
		DelayWarning: 5 * time.Minute,
		ClaimLease:   10 * time.Minute,
	}
}

// Manager owns the queue: spooling, scheduling and outcome recording. In
// multi-active deployments several managers share one KV+blob pair; every
// due message is claimed transactionally before delivery and outcomes are
// fenced by claim ownership, so each message has exactly one live worker
// at a time across the deployment.
type Manager struct {
	kv        store.KV
	txn       store.TxnKV
	blob      store.Blob
	deliver   Deliverer
	signer    Signer
	onEvent   func(event string)
	bounce    BounceHandler
	delayWarn DelayWarningHandler
	mtr       *metrics.Metrics // optional Prometheus instrumentation (SetMetrics)
	logger    *slog.Logger
	opts      Options
	nodeID    string
	lease     time.Duration

	mu        sync.Mutex // serializes ID allocation
	workerSem chan struct{}
	wake      chan struct{}
	paused    atomic.Bool
	wg        sync.WaitGroup // in-flight delivery workers
}

// SetOnEvent installs a callback for queue lifecycle events
// (submitted/delivered/bounced/deferred/failed), used for metrics.
func (m *Manager) SetOnEvent(fn func(event string)) {
	m.onEvent = fn
}

// Pause stops the scheduler from delivering messages (submissions still
// spool). Resume clears it and wakes the scheduler.
func (m *Manager) Pause() {
	m.paused.Store(true)
}

// Resume re-enables delivery.
func (m *Manager) Resume() {
	m.paused.Store(false)
	m.signal()
}

func (m *Manager) event(event string) {
	if m.onEvent != nil {
		m.onEvent(event)
	}
}

// New builds a queue manager. The caller owns kv and blob.
func New(kv store.KV, blob store.Blob, deliver Deliverer, opts Options, logger *slog.Logger) *Manager {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultOptions().MaxAttempts
	}
	if opts.BaseRetry <= 0 {
		opts.BaseRetry = DefaultOptions().BaseRetry
	}
	if opts.MaxRetry <= 0 {
		opts.MaxRetry = DefaultOptions().MaxRetry
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultOptions().PollInterval
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultOptions().Workers
	}
	if opts.ClaimLease <= 0 {
		opts.ClaimLease = DefaultOptions().ClaimLease
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	nodeID := opts.NodeID
	if nodeID == "" {
		nodeID = kvlease.RandomNodeID()
	}
	return &Manager{
		kv:        kv,
		txn:       store.AsTxn(kv),
		blob:      blob,
		deliver:   deliver,
		logger:    logger,
		opts:      opts,
		nodeID:    nodeID,
		lease:     opts.ClaimLease,
		workerSem: make(chan struct{}, opts.Workers),
		wake:      make(chan struct{}, 1),
	}
}

// Signer optionally signs a message once at enqueue (DKIM; ARCHITECTURE.md
// §5). The signed bytes are what gets spooled and delivered.
type Signer interface {
	Sign(ctx context.Context, from string, msg []byte) ([]byte, error)
}

// SetSigner installs the optional enqueue signer. Call it before Run; the
// field is read only by Submit.
func (m *Manager) SetSigner(s Signer) {
	m.signer = s
}

// Submit spools a message for delivery and returns its ID.
func (m *Manager) Submit(ctx context.Context, from string, to []string, subject string, r io.Reader) (uint64, error) {
	if len(to) == 0 {
		return 0, errors.New("queue: no recipients")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	if m.signer != nil {
		data, err = m.signer.Sign(ctx, from, data)
		if err != nil {
			return 0, fmt.Errorf("queue: sign: %w", err)
		}
	}
	body := bytes.NewReader(data)
	m.mu.Lock()
	id, err := m.nextID(ctx)
	m.mu.Unlock()
	if err != nil {
		return 0, err
	}

	blobID := fmt.Sprintf("q-%016x", id)
	size, err := m.blob.Put(ctx, blobID, int64(len(data)), body)
	if err != nil {
		return 0, err
	}

	now := m.opts.Now()
	recipients := make([]Recipient, len(to))
	for i, addr := range to {
		recipients[i] = Recipient{Address: addr, Status: RecipientPending}
	}
	msg := Message{
		ID:          id,
		From:        from,
		Recipients:  recipients,
		Subject:     subject,
		BlobID:      blobID,
		Size:        size,
		State:       StateQueued,
		MaxAttempts: m.opts.MaxAttempts,
		NextAttempt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := m.save(msg, time.Time{}); err != nil {
		return 0, err
	}
	m.event("submitted")
	m.signal()
	return id, nil
}

// ProcessDue delivers every message whose next-attempt time has passed.
// Exported for tests and manual scheduling; Run loops it.
func (m *Manager) ProcessDue(ctx context.Context) error {
	if m.paused.Load() {
		return nil
	}
	now := m.opts.Now()
	var ids []uint64
	err := m.kv.Scan(store.QueueDuePrefix(), func(k, _ []byte) error {
		if len(k) != 21 {
			// A stray key under the due prefix (migration leftovers, a
			// foreign encoding) must not stall the whole outbound queue:
			// skip it loudly and keep scanning.
			m.logger.Warn("queue: skipping malformed due index key", "key", fmt.Sprintf("%x", k))
			return nil
		}
		ts := int64(binary.BigEndian.Uint64(k[5:13]))
		if ts > now.Unix() {
			return errStopScan
		}
		ids = append(ids, binary.BigEndian.Uint64(k[13:21]))
		return nil
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return err
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case m.workerSem <- struct{}{}:
		case <-ctx.Done():
			// Shutdown must not hang on the semaphore while every worker
			// sits in a long delivery.
			wg.Wait()
			return nil
		}
		m.wg.Add(1)
		wg.Add(1)
		go func(id uint64) {
			defer func() {
				<-m.workerSem
				wg.Done()
				m.wg.Done()
			}()
			if err := m.processMessage(ctx, id); err != nil {
				m.logger.Error("queue: deliver", "message", id, "err", err)
			}
		}(id)
	}
	wg.Wait()
	return nil
}

// Run is the scheduler loop; it returns when ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain in-flight workers before returning so the caller can
			// safely close the KV store afterwards (graceful shutdown).
			m.wg.Wait()
			return nil
		case <-ticker.C:
			if err := m.ProcessDue(ctx); err != nil {
				m.logger.Error("queue: process due", "err", err)
			}
		case <-m.wake:
			if err := m.ProcessDue(ctx); err != nil {
				m.logger.Error("queue: process due", "err", err)
			}
		}
	}
}

// List returns all queued messages ordered by ID.
func (m *Manager) List(ctx context.Context) ([]Message, error) {
	var out []Message
	err := m.kv.Scan(store.QueueMetaPrefix(queueName), func(k, v []byte) error {
		var msg Message
		if err := json.Unmarshal(v, &msg); err != nil {
			return err
		}
		out = append(out, msg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Retry reschedules a terminal or deferred message immediately. The state
// check and the write run in one transaction: in multi-active deployments
// another node may claim the message between a plain read and a plain
// write, and an unfenced Retry would resurrect the meta row under the
// in-flight worker (whose fenced outcome would then be silently dropped,
// re-sending mail the remote already accepted).
func (m *Manager) Retry(ctx context.Context, id uint64) error {
	now := m.opts.Now()
	return m.txn.WithTxn(ctx, func(t store.TxnOps) error {
		v, err := t.Get(store.QueueKey(queueName, id))
		if err != nil {
			return err
		}
		var cur Message
		if err := json.Unmarshal(v, &cur); err != nil {
			return err
		}
		if cur.State == StateActive {
			return fmt.Errorf("queue: message %d is being delivered; retry after it completes", id)
		}
		oldDue := duePos(cur)
		cur.State = StateQueued
		cur.Attempts = 0
		cur.NextAttempt = now
		cur.Owner = ""
		cur.LeaseUntil = time.Time{}
		cur.UpdatedAt = now
		data, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		t.Put(store.QueueKey(queueName, id), data)
		// Deleting the old position is idempotent (terminal messages have
		// no live entry).
		t.Append(
			store.Op{Key: store.QueueDueKey(oldDue.Unix(), id), Delete: true},
			store.Op{Key: store.QueueDueKey(now.Unix(), id), Value: nil},
		)
		return nil
	})
}

// Cancel withdraws a message from the queue and removes its body blob.
// A message being delivered (StateActive) cannot be canceled: the in-flight
// worker's fenced outcome would resurrect the record (or, unfenced, the
// cancel would drop a delivery the remote already accepted). The state
// check and the deletes commit in one transaction.
func (m *Manager) Cancel(ctx context.Context, id uint64) error {
	var msg Message
	err := m.txn.WithTxn(ctx, func(t store.TxnOps) error {
		v, err := t.Get(store.QueueKey(queueName, id))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(v, &msg); err != nil {
			return err
		}
		if msg.State == StateActive {
			return fmt.Errorf("queue: message %d is being delivered; cancel after it completes", id)
		}
		t.Delete(store.QueueKey(queueName, id))
		t.Append(
			store.Op{Key: store.QueueDueKey(duePos(msg).Unix(), id), Delete: true},
			store.Op{Key: store.QueueDueKey(msg.LeaseUntil.Unix(), id), Delete: true},
		)
		return nil
	})
	if err != nil {
		return err
	}
	if isTerminal(msg.State) {
		return nil
	}
	if err := m.blob.Delete(ctx, msg.BlobID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// nextID allocates the next message ID. The read-modify-write runs inside
// a transaction so multi-active managers sharing the store cannot hand out
// the same ID (serialized by conflict replay on TiDB, by the write lock on
// the in-memory backend, by m.mu on buffer-mode single-node backends).
func (m *Manager) nextID(ctx context.Context) (uint64, error) {
	var next uint64
	err := m.txn.WithTxn(ctx, func(t store.TxnOps) error {
		v, err := t.Get(store.QueueCounterKey())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if len(v) == 8 {
			next = binary.BigEndian.Uint64(v)
		}
		next++
		t.Put(store.QueueCounterKey(), beUint64(next))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

func (m *Manager) load(_ context.Context, id uint64) (Message, error) {
	v, err := m.kv.Get(store.QueueKey(queueName, id))
	if err != nil {
		return Message{}, err
	}
	var msg Message
	if err := json.Unmarshal(v, &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// duePos is a message's position in the due index: when the scheduler next
// needs to look at it — the next attempt while it waits, the claim expiry
// while a worker delivers (so a crashed owner is rediscovered exactly when
// its claim lapses).
func duePos(msg Message) time.Time {
	if msg.State == StateActive {
		return msg.LeaseUntil
	}
	return msg.NextAttempt
}

// save persists metadata and keeps the due index consistent with duePos.
// oldDue is the index position before the update (zero for new messages).
func (m *Manager) save(msg Message, oldDue time.Time) error {
	msg.UpdatedAt = m.opts.Now()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ops := []store.Op{{Key: store.QueueKey(queueName, msg.ID), Value: data}}
	newDue := duePos(msg)
	switch {
	case isTerminal(msg.State):
		ops = append(ops, store.Op{Key: store.QueueDueKey(oldDue.Unix(), msg.ID), Delete: true})
	case oldDue.IsZero():
		// New message: create the due index entry here.
		ops = append(ops, store.Op{Key: store.QueueDueKey(newDue.Unix(), msg.ID), Value: nil})
	case !oldDue.Equal(newDue):
		ops = append(ops,
			store.Op{Key: store.QueueDueKey(oldDue.Unix(), msg.ID), Delete: true},
			store.Op{Key: store.QueueDueKey(newDue.Unix(), msg.ID), Value: nil},
		)
	}
	return m.kv.Batch(ops)
}

// claim atomically takes ownership of one due message: a QUEUED/DEFERRED
// message transitions to ACTIVE under this node's identity; an ACTIVE
// message whose claim expired is stolen — counting as an attempt, so a
// crash-looping owner cannot deliver forever. Terminal leftovers self-heal
// their stale due entries. It returns false when another node holds a live
// claim (the scheduler skips the message).
func (m *Manager) claim(ctx context.Context, id uint64, out *Message) (bool, error) {
	now := m.opts.Now()
	claimed := false
	err := m.txn.WithTxn(ctx, func(t store.TxnOps) error {
		claimed = false
		v, err := t.Get(store.QueueKey(queueName, id))
		if err != nil {
			return err
		}
		var cur Message
		if err := json.Unmarshal(v, &cur); err != nil {
			return err
		}
		if isTerminal(cur.State) {
			// Self-heal a stale due entry left by a crash between the
			// terminal commit and index maintenance. The orphan can sit at
			// either historical position; deletes are idempotent, so clear
			// both candidates.
			t.Append(
				store.Op{Key: store.QueueDueKey(duePos(cur).Unix(), id), Delete: true},
				store.Op{Key: store.QueueDueKey(cur.LeaseUntil.Unix(), id), Delete: true},
			)
			return nil
		}
		if cur.State == StateActive && cur.LeaseUntil.After(now) {
			return nil // live claim (ours or a foreign node's): hands off
		}
		if cur.State == StateActive {
			// Steal after expiry: the previous owner crashed or stalled
			// mid-delivery. The outcome is unknowable — at-least-once.
			cur.LastError = "claim expired"
			m.observeClaim("stolen")
		} else {
			m.observeClaim("claimed")
		}
		oldDue := duePos(cur)
		cur.State = StateActive
		cur.Owner = m.nodeID
		cur.LeaseUntil = now.Add(m.lease)
		cur.Attempts++
		data, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		t.Put(store.QueueKey(queueName, id), data)
		// Re-index at the claim expiry so a crashed owner is rediscovered
		// exactly when its claim lapses.
		t.Append(
			store.Op{Key: store.QueueDueKey(oldDue.Unix(), id), Delete: true},
			store.Op{Key: store.QueueDueKey(cur.LeaseUntil.Unix(), id), Value: nil},
		)
		*out = cur
		claimed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// processMessage claims one due message and delivers it under the claim.
func (m *Manager) processMessage(ctx context.Context, id uint64) error {
	var msg Message
	claimed, err := m.claim(ctx, id, &msg)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	var body bytes.Buffer
	if err := m.blob.Get(ctx, msg.BlobID, &body); err != nil {
		// Blob read failures are storage-transient, not a verdict on the
		// message: defer with backoff instead of terminating it. Only retry
		// exhaustion (or a permanently missing blob) bounces — with the DSN
		// noting the storage failure rather than a delivery verdict.
		m.applyTransportFailure(&msg, fmt.Errorf("blob read: %w", err))
		return m.commitOutcome(ctx, &msg, nil)
	}

	pending := make([]string, 0, len(msg.Recipients))
	for _, r := range msg.Recipients {
		if r.Status == RecipientPending {
			pending = append(pending, r.Address)
		}
	}
	if len(pending) == 0 {
		// Nothing left to deliver (cross-round leftovers): finalize now
		// instead of idling on a claim that would ping-pong between nodes
		// at every expiry.
		m.finalizeState(&msg)
		return m.commitOutcome(ctx, &msg, body.Bytes())
	}

	start := time.Now()
	results, derr := m.deliver.Deliver(ctx, msg.From, pending, bytes.NewReader(body.Bytes()))
	m.observeDelivery(time.Since(start), derr == nil)
	if derr != nil {
		// Transport-level failure: defer every pending recipient. This is
		// at-least-once delivery: a connection lost AFTER the message body
		// was sent leaves the remote's commit state unknowable, so the
		// retry can duplicate for recipients whose copy actually landed
		// (SMTP has no per-recipient transaction; LMTP is not an option
		// against remote MXes). Failures before DATA cannot duplicate.
		m.applyTransportFailure(&msg, derr)
		return m.commitOutcome(ctx, &msg, body.Bytes())
	}
	byAddr := map[string]Result{}
	for _, res := range results {
		byAddr[res.To] = res
	}
	for i := range msg.Recipients {
		r := &msg.Recipients[i]
		if r.Status != RecipientPending {
			continue
		}
		res, ok := byAddr[r.Address]
		if !ok {
			continue // deliverer skipped this recipient; it stays pending
		}
		if res.OK {
			r.Status = RecipientDelivered
			continue
		}
		r.LastError = resultError(res)
		if res.Permanent {
			r.Status = RecipientBounced
		}
	}
	m.finalizeState(&msg)
	return m.commitOutcome(ctx, &msg, body.Bytes())
}

// applyTransportFailure mutates msg for a transport-level failure (or an
// unreadable spool): every pending recipient defers with backoff, and retry
// exhaustion terminates the message.
func (m *Manager) applyTransportFailure(msg *Message, err error) {
	for i := range msg.Recipients {
		if msg.Recipients[i].Status == RecipientPending {
			msg.Recipients[i].LastError = err.Error()
		}
	}
	msg.LastError = err.Error()
	if msg.Attempts >= msg.MaxAttempts {
		for i := range msg.Recipients {
			if msg.Recipients[i].Status == RecipientPending {
				msg.Recipients[i].Status = RecipientBounced
			}
		}
		msg.State = StateBounced
		m.event("bounced")
		return
	}
	msg.State = StateDeferred
	msg.NextAttempt = m.opts.Now().Add(backoff(m.opts, msg.Attempts))
	m.event("deferred")
}

// finalizeState decides the post-round state after per-recipient outcomes
// have been applied. Terminal state is decided by "no recipient remains
// pending", not by this round's counts: a cross-round partial success
// (round 1 resolved some recipients, round 2 the rest) must still
// terminate — and a recipient that fails PERMANENTLY in a later round must
// still get its bounce DSN instead of the message idling in deferred
// forever.
func (m *Manager) finalizeState(msg *Message) {
	bounced, pendingLeft := 0, 0
	for _, r := range msg.Recipients {
		switch r.Status {
		case RecipientPending:
			pendingLeft++
		case RecipientBounced:
			bounced++
		}
	}
	switch {
	case pendingLeft == 0 && bounced == 0:
		msg.State = StateDelivered
		msg.LastError = ""
		m.event("delivered")
	case pendingLeft == 0:
		msg.State = StateBounced
		m.event("bounced")
	case msg.Attempts >= msg.MaxAttempts:
		for i := range msg.Recipients {
			if msg.Recipients[i].Status == RecipientPending {
				msg.Recipients[i].Status = RecipientBounced
				bounced++
			}
		}
		msg.State = StateBounced
		m.event("bounced")
	default:
		msg.State = StateDeferred
		msg.NextAttempt = m.opts.Now().Add(backoff(m.opts, msg.Attempts))
		m.event("deferred")
	}
}

// commitOutcome durably records the outcome of a claimed delivery. The
// write is fenced by claim ownership: when the claim was stolen after its
// lease expired (owner changed under us), the outcome is discarded — the
// stealing node is already re-delivering and must not be overwritten by a
// stale worker. Bounce/delay DSNs fire only after the state is committed,
// and the terminal blob reclaim follows the commit (a crash between the
// two must leave the KV row terminal — a leaked blob is GC-able — never an
// active row pointing at a deleted blob: that would spin the retry loop on
// a missing body and, once attempts run out, bounce mail that may already
// have been delivered).
func (m *Manager) commitOutcome(ctx context.Context, msg *Message, body []byte) error {
	// The claim indexed this message at its LeaseUntil — duePos(msg) would
	// now read the post-outcome state (NextAttempt), delete the wrong key
	// and leak a ghost entry that re-triggers the scheduler forever.
	oldDue := msg.LeaseUntil
	committed := false
	err := m.txn.WithTxn(ctx, func(t store.TxnOps) error {
		committed = false
		v, err := t.Get(store.QueueKey(queueName, msg.ID))
		if err != nil {
			return err
		}
		var cur Message
		if err := json.Unmarshal(v, &cur); err != nil {
			return err
		}
		if cur.State != StateActive || cur.Owner != m.nodeID {
			return nil // claim was stolen; drop our outcome
		}
		msg.UpdatedAt = m.opts.Now()
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		t.Put(store.QueueKey(queueName, msg.ID), data)
		newDue := duePos(*msg)
		if isTerminal(msg.State) {
			t.Delete(store.QueueDueKey(oldDue.Unix(), msg.ID))
		} else {
			t.Append(
				store.Op{Key: store.QueueDueKey(oldDue.Unix(), msg.ID), Delete: true},
				store.Op{Key: store.QueueDueKey(newDue.Unix(), msg.ID), Value: nil},
			)
		}
		committed = true
		return nil
	})
	if err != nil {
		return err
	}
	if !committed {
		m.observeClaim("lost")
		m.logger.Warn("queue: claim lost; outcome discarded", "message", msg.ID, "owner", msg.Owner)
		return nil
	}
	if isTerminal(msg.State) {
		if err := m.blob.Delete(ctx, msg.BlobID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if msg.State == StateBounced {
		m.maybeBounce(ctx, msg, body)
	} else if msg.State == StateDeferred {
		m.maybeDelayWarning(ctx, msg, body)
	}
	return nil
}

// maybeBounce invokes the bounce handler once for terminal bounce states.
func (m *Manager) maybeBounce(ctx context.Context, msg *Message, body []byte) {
	if m.bounce == nil || msg.State != StateBounced {
		return
	}
	var failures []BounceFailure
	for _, r := range msg.Recipients {
		if r.Status == RecipientBounced {
			failures = append(failures, BounceFailure{
				To:      r.Address,
				Status:  "5.0.0",
				Comment: r.LastError,
			})
		}
	}
	if len(failures) == 0 {
		return
	}
	m.bounce(ctx, msg.From, msg, body, failures)
}

func backoff(opts Options, attempt int) time.Duration {
	d := opts.BaseRetry
	for i := 1; i < attempt && d < opts.MaxRetry; i++ {
		d *= 2
		if d > opts.MaxRetry {
			d = opts.MaxRetry
		}
	}
	// ±20% jitter.
	j := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * j)
}

func isTerminal(s State) bool {
	return s == StateDelivered || s == StateBounced || s == StateFailed
}

func resultError(res Result) string {
	if res.Err == nil {
		return "delivery failed"
	}
	return res.Err.Error()
}

func beUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

var errStopScan = errors.New("queue: stop scan")
