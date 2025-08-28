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
	}
}

// Manager owns the queue: spooling, scheduling and outcome recording.
type Manager struct {
	kv        store.KV
	blob      store.Blob
	deliver   Deliverer
	signer    Signer
	onEvent   func(event string)
	bounce    BounceHandler
	delayWarn DelayWarningHandler
	mtr       *metrics.Metrics // optional Prometheus instrumentation (SetMetrics)
	logger    *slog.Logger
	opts      Options

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
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		kv:        kv,
		blob:      blob,
		deliver:   deliver,
		logger:    logger,
		opts:      opts,
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
	id, err := m.nextID()
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
			return errors.New("queue: corrupt due index key")
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
		m.workerSem <- struct{}{}
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

// Retry reschedules a terminal or deferred message immediately.
func (m *Manager) Retry(ctx context.Context, id uint64) error {
	msg, err := m.load(ctx, id)
	if err != nil {
		return err
	}
	oldNext := msg.NextAttempt
	now := m.opts.Now()
	msg.State = StateQueued
	msg.Attempts = 0
	msg.NextAttempt = now
	return m.save(msg, oldNext)
}

// Cancel withdraws a message from the queue and removes its body blob.
func (m *Manager) Cancel(ctx context.Context, id uint64) error {
	msg, err := m.load(ctx, id)
	if err != nil {
		return err
	}
	ops := []store.Op{
		{Key: store.QueueKey(queueName, id), Delete: true},
		{Key: store.QueueDueKey(msg.NextAttempt.Unix(), id), Delete: true},
	}
	if err := m.kv.Batch(ops); err != nil {
		return err
	}
	return m.blob.Delete(ctx, msg.BlobID)
}

func (m *Manager) nextID() (uint64, error) {
	v, err := m.kv.Get(store.QueueCounterKey())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	var next uint64
	if len(v) == 8 {
		next = binary.BigEndian.Uint64(v)
	}
	next++
	if err := m.kv.Put(store.QueueCounterKey(), beUint64(next)); err != nil {
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

// save persists metadata and keeps the due index consistent with
// NextAttempt. oldNext is the value before the update (for index moves).
func (m *Manager) save(msg Message, oldNext time.Time) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ops := []store.Op{{Key: store.QueueKey(queueName, msg.ID), Value: data}}
	switch {
	case isTerminal(msg.State):
		ops = append(ops, store.Op{Key: store.QueueDueKey(msg.NextAttempt.Unix(), msg.ID), Delete: true})
	case oldNext.IsZero():
		// New message: create the due index entry here.
		ops = append(ops, store.Op{Key: store.QueueDueKey(msg.NextAttempt.Unix(), msg.ID), Value: nil})
	case !oldNext.Equal(msg.NextAttempt):
		ops = append(ops,
			store.Op{Key: store.QueueDueKey(oldNext.Unix(), msg.ID), Delete: true},
			store.Op{Key: store.QueueDueKey(msg.NextAttempt.Unix(), msg.ID), Value: nil},
		)
	}
	return m.kv.Batch(ops)
}

func (m *Manager) processMessage(ctx context.Context, id uint64) error {
	msg, err := m.load(ctx, id)
	if err != nil {
		return err
	}
	if isTerminal(msg.State) {
		_ = m.kv.Delete(store.QueueDueKey(msg.NextAttempt.Unix(), id))
		return nil
	}
	var body bytes.Buffer
	if err := m.blob.Get(ctx, msg.BlobID, &body); err != nil {
		msg.State = StateFailed
		msg.LastError = err.Error()
		_ = m.save(msg, msg.NextAttempt)
		return err
	}

	var pending []string
	for _, r := range msg.Recipients {
		if r.Status == RecipientPending {
			pending = append(pending, r.Address)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	oldNext := msg.NextAttempt
	msg.State = StateActive
	msg.Attempts++
	if err := m.save(msg, oldNext); err != nil {
		return err
	}

	start := time.Now()
	results, derr := m.deliver.Deliver(ctx, msg.From, pending, bytes.NewReader(body.Bytes()))
	m.observeDelivery(time.Since(start), derr == nil)
	if derr != nil {
		// Transport-level failure: defer every pending recipient.
		return m.deferAll(ctx, &msg, oldNext, derr, body.Bytes())
	}
	byAddr := map[string]Result{}
	for _, res := range results {
		byAddr[res.To] = res
	}

	delivered, bounced := 0, 0
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
			delivered++
			continue
		}
		r.LastError = resultError(res)
		if res.Permanent {
			r.Status = RecipientBounced
			bounced++
		}
	}

	switch {
	case len(msg.Recipients) == delivered:
		msg.State = StateDelivered
		msg.LastError = ""
		m.event("delivered")
	case delivered+bounced == len(msg.Recipients):
		msg.State = StateBounced
		m.event("bounced")
		m.maybeBounce(ctx, &msg, body.Bytes())
	case msg.Attempts >= msg.MaxAttempts:
		for i := range msg.Recipients {
			if msg.Recipients[i].Status == RecipientPending {
				msg.Recipients[i].Status = RecipientBounced
				bounced++
			}
		}
		msg.State = StateBounced
		m.event("bounced")
		m.maybeBounce(ctx, &msg, body.Bytes())
	default:
		msg.State = StateDeferred
		msg.NextAttempt = m.opts.Now().Add(backoff(m.opts, msg.Attempts))
		m.event("deferred")
		m.maybeDelayWarning(ctx, &msg, body.Bytes())
	}
	if isTerminal(msg.State) {
		_ = m.blob.Delete(ctx, msg.BlobID)
	}
	return m.save(msg, oldNext)
}

func (m *Manager) deferAll(ctx context.Context, msg *Message, oldNext time.Time, err error, body []byte) error {
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
		m.maybeBounce(ctx, msg, body)
	} else {
		msg.State = StateDeferred
		msg.NextAttempt = m.opts.Now().Add(backoff(m.opts, msg.Attempts))
		m.event("deferred")
		m.maybeDelayWarning(ctx, msg, body)
	}
	if isTerminal(msg.State) {
		_ = m.blob.Delete(ctx, msg.BlobID)
	}
	return m.save(*msg, oldNext)
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
