// Multi-active semantics: several managers share one KV+blob pair (the
// in-memory backend supplies real transactional serialization, so these
// tests exercise the same fencing a TiDB-backed cluster gets). The
// guarantees pinned here:
//
//   - exactly one live worker per message at any moment (claim)
//   - a crashed owner's message is rediscovered when its claim lapses
//     (steal counts as an attempt, bounding crash loops)
//   - a stale worker that lost its claim cannot overwrite the successor's
//     state (fenced outcome)
package queue

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mailezine/internal/metrics"
	"mailezine/internal/store"
)

// gatedDeliverer blocks each Deliver call until released, modelling a slow
// MX hung mid-delivery. The outcome is fixed at construction: every node
// gets its own instance, exactly like the real per-node SMTP client.
type gatedDeliverer struct {
	release chan struct{}
	err     error
	calls   atomic.Int32
}

func newGatedDeliverer(err error) *gatedDeliverer {
	return &gatedDeliverer{release: make(chan struct{}, 16), err: err}
}

func (g *gatedDeliverer) Deliver(_ context.Context, _ string, _ []string, msg io.Reader) ([]Result, error) {
	_, _ = io.Copy(io.Discard, msg)
	g.calls.Add(1)
	<-g.release
	if g.err != nil {
		return nil, g.err
	}
	// Success for every recipient; the caller passes the pending list.
	return nil, nil
}

func (g *gatedDeliverer) callsCount() int32 { return g.calls.Load() }

// okGated is a gated deliverer that succeeds for every pending recipient.
type okGated struct {
	gated *gatedDeliverer
}

func (o *okGated) Deliver(ctx context.Context, from string, to []string, msg io.Reader) ([]Result, error) {
	if _, err := o.gated.Deliver(ctx, from, to, msg); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(to))
	for _, addr := range to {
		out = append(out, Result{To: addr, OK: true})
	}
	return out, nil
}

func newMultiManager(kv store.KV, blob store.Blob, d Deliverer, node string, clock *fakeClock, lease time.Duration) *Manager {
	opts := DefaultOptions()
	opts.NodeID = node
	opts.ClaimLease = lease
	opts.Now = clock.Now
	return New(kv, blob, d, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// dueEntries returns the (timestamp, id) pairs currently in the due index —
// the invariant surface every claim/commit transition must keep exact: one
// entry per non-terminal message, positioned at its duePos, none for
// terminal ones.
func dueEntries(t *testing.T, kv store.KV) map[uint64]int64 {
	t.Helper()
	out := map[uint64]int64{}
	if err := kv.Scan(store.QueueDuePrefix(), func(k, _ []byte) error {
		if len(k) != 21 {
			t.Fatalf("malformed due key in index: %x", k)
		}
		id := binary.BigEndian.Uint64(k[13:21])
		if _, dup := out[id]; dup {
			t.Fatalf("duplicate due entries for message %d", id)
		}
		out[id] = int64(binary.BigEndian.Uint64(k[5:13]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Regression (release-blocking): the claim indexes a message at its
// LeaseUntil, so the outcome commit must move/delete THAT entry. Reading
// duePos after the outcome mutation (NextAttempt) deleted the wrong key and
// leaked a ghost entry per delivered message — ghosts re-triggered the
// scheduler at lease expiry, re-claiming deferred messages early (burning
// attempts into premature bounces) and growing the index without bound.
func TestDueIndexStaysExact(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: map[string]Result{
		"a@example.com": {To: "a@example.com", OK: true},
		"b@example.com": {To: "b@example.com", Err: errors.New("451 temp")},
	}}
	opts := DefaultOptions()
	opts.BaseRetry = time.Minute
	opts.Now = clock.Now
	m := New(kv, blob, d, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	id1, err := m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "ok", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := m.Submit(ctx, "s@example.com", []string{"b@example.com"}, "defer", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	entries := dueEntries(t, kv)
	if len(entries) != 2 {
		t.Fatalf("after submit: due entries = %v, want 2", entries)
	}

	// Round 1: one delivered (terminal), one deferred.
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	entries = dueEntries(t, kv)
	if _, ok := entries[id1]; ok {
		t.Fatalf("terminal message %d still has a due entry: %v", id1, entries)
	}
	retryAt, ok := entries[id2]
	if !ok {
		t.Fatalf("deferred message %d lost its due entry: %v", id2, entries)
	}
	msgs, _ := m.List(ctx)
	var deferred Message
	for _, msg := range msgs {
		if msg.ID == id2 {
			deferred = msg
		}
	}
	if retryAt != deferred.NextAttempt.Unix() {
		t.Fatalf("deferred due entry at %d, want NextAttempt %d", retryAt, deferred.NextAttempt.Unix())
	}

	// Retry re-opens the still-deferred message and refuses the terminal one,
	// whose body blob is already gone.
	if err := m.Retry(ctx, id2); err != nil {
		t.Fatal(err)
	}
	entries = dueEntries(t, kv)
	if len(entries) != 1 || entries[id2] != clock.now.Unix() {
		t.Fatalf("after retry: due entries = %v, want single entry for %d at now", entries, id2)
	}
	if err := m.Retry(ctx, id1); err == nil {
		t.Fatalf("retry of delivered message %d: want refusal", id1)
	}
	if entries := dueEntries(t, kv); len(entries) != 1 || entries[id2] != clock.now.Unix() {
		t.Fatalf("refused terminal retry mutated the due index: %v", entries)
	}

	// Round 2 after backoff: the deferred message delivers and its entry
	// disappears; nothing resurrects the terminal one.
	clock.Advance(2 * time.Hour)
	d.results["b@example.com"] = Result{To: "b@example.com", OK: true}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if entries := dueEntries(t, kv); len(entries) != 0 {
		t.Fatalf("due index after full delivery = %v, want empty", entries)
	}

	// Cancel removes the terminal row; the due index stays exact (empty).
	if err := m.Cancel(ctx, id1); err != nil {
		t.Fatal(err)
	}
	if entries := dueEntries(t, kv); len(entries) != 0 {
		t.Fatalf("due index after cancel = %v, want empty", entries)
	}
}

// Two schedulers racing on the same due messages deliver each exactly once.
func TestConcurrentManagersDeliverOnce(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m1 := newMultiManager(kv, blob, d, "node-1", clock, time.Minute)
	m2 := newMultiManager(kv, blob, d, "node-2", clock, time.Minute)
	ctx := context.Background()

	const msgs = 8
	for i := 0; i < msgs; i++ {
		if _, err := m1.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body")); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for _, m := range []*Manager{m1, m2} {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			if err := m.ProcessDue(ctx); err != nil {
				t.Error(err)
			}
		}(m)
	}
	wg.Wait()

	if n := d.callsCount(); n != msgs {
		t.Fatalf("deliver calls = %d, want %d (each message exactly once)", n, msgs)
	}
	list, _ := m2.List(ctx)
	if len(list) != msgs {
		t.Fatalf("messages = %d, want %d", len(list), msgs)
	}
	for _, msg := range list {
		if msg.State != StateDelivered {
			t.Fatalf("message %d state = %s, want delivered", msg.ID, msg.State)
		}
	}
}

// A manager that dies mid-delivery (claim never committed) releases the
// message to the cluster once the claim lease lapses; the steal counts as
// an attempt so a crash-looping owner cannot deliver forever.
func TestExpiredClaimIsStolen(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	lease := time.Minute
	dead := newMultiManager(kv, blob, d, "dead-node", clock, lease)
	successor := newMultiManager(kv, blob, d, "node-2", clock, lease)
	// Health metrics ride the successor: the steal below must be visible.
	mm := metrics.New()
	successor.SetMetrics(mm, context.Background())
	ctx := context.Background()

	if _, err := dead.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body")); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash: the message sits ACTIVE under a claim whose owner
	// will never commit an outcome.
	msgs, _ := dead.List(ctx)
	msg := msgs[0]
	oldDue := msg.NextAttempt
	msg.State = StateActive
	msg.Owner = "dead-node"
	msg.LeaseUntil = clock.now.Add(lease)
	msg.Attempts++
	if err := dead.save(msg, oldDue); err != nil {
		t.Fatal(err)
	}

	// Claim still live: the successor must keep hands off.
	if err := successor.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := d.callsCount(); n != 0 {
		t.Fatalf("successor delivered under a live foreign claim: %d", n)
	}

	// Lease lapses; the successor steals and delivers.
	clock.Advance(2 * lease)
	if err := successor.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := d.callsCount(); n != 1 {
		t.Fatalf("deliver calls after steal = %d, want 1", n)
	}
	// The steal is visible in the multi-active health metrics.
	if got := testutil.ToFloat64(mm.QueueClaims.WithLabelValues("stolen")); got != 1 {
		t.Fatalf("stolen counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(mm.QueueClaims.WithLabelValues("lost")); got != 0 {
		t.Fatalf("lost counter = %v, want 0 (no fenced writes in this scenario)", got)
	}
	msgs, _ = successor.List(ctx)
	if msgs[0].State != StateDelivered {
		t.Fatalf("state after steal = %s, want delivered", msgs[0].State)
	}
	if msgs[0].Attempts != 2 {
		t.Fatalf("steal must count as an attempt: attempts = %d, want 2", msgs[0].Attempts)
	}
	if msgs[0].Owner != "node-2" {
		t.Fatalf("owner after steal = %q, want node-2", msgs[0].Owner)
	}
}

// A worker whose claim was stolen after expiry must not overwrite the
// successor's state with its own (late) outcome — the write is fenced by
// ownership. The late worker's copy may still land at the recipient
// (at-least-once), but the queue's committed state stays the successor's.
func TestStaleWorkerOutcomeIsDiscarded(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	clock := &fakeClock{now: time.Now()}
	lease := time.Minute
	// Each node delivers through its own client: node-1's connection dies,
	// node-2's succeeds.
	d1 := newGatedDeliverer(errors.New("dial: connection lost"))
	d2 := newGatedDeliverer(nil)
	m1 := newMultiManager(kv, blob, d1, "node-1", clock, lease)
	m2 := newMultiManager(kv, blob, &okGated{gated: d2}, "node-2", clock, lease)
	ctx := context.Background()

	if _, err := m1.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body")); err != nil {
		t.Fatal(err)
	}

	// node-1 claims and hangs inside Deliver.
	done1 := make(chan error, 1)
	go func() { done1 <- m1.ProcessDue(ctx) }()
	waitFor(t, func() bool { return d1.callsCount() == 1 })

	// The claim lapses; node-2 steals and hangs inside Deliver too.
	clock.Advance(2 * lease)
	done2 := make(chan error, 1)
	go func() { done2 <- m2.ProcessDue(ctx) }()
	waitFor(t, func() bool { return d2.callsCount() == 1 })

	// Both attempts finish — node-1's transport fails, node-2's succeeds.
	d1.release <- struct{}{}
	d2.release <- struct{}{}
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if err := <-done2; err != nil {
		t.Fatal(err)
	}

	msgs, _ := m1.List(ctx)
	if msgs[0].State != StateDelivered {
		t.Fatalf("stale worker overwrote the successor: state = %s, want delivered", msgs[0].State)
	}
	if msgs[0].Owner != "node-2" {
		t.Fatalf("owner = %q, want node-2 (successor)", msgs[0].Owner)
	}
	// Both nodes attempted (the documented at-least-once duplicate window).
	if d1.callsCount() != 1 || d2.callsCount() != 1 {
		t.Fatalf("attempts: node-1=%d node-2=%d, want 1 each", d1.callsCount(), d2.callsCount())
	}
}

// Terminal leftovers with stale due entries self-heal: the claim
// transaction removes the index entry instead of delivering again.
func TestTerminalLeftoverSelfHeals(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m := newMultiManager(kv, blob, d, "node-1", clock, time.Minute)
	ctx := context.Background()

	id, err := m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	// Forge the crash-between-commit-and-index window: terminal row, stale
	// due entry.
	if err := kv.Put(store.QueueDueKey(clock.now.Unix(), id), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := d.callsCount(); n != 1 {
		t.Fatalf("terminal leftover redelivered: calls = %d, want 1", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition never became true")
		case <-time.After(time.Millisecond):
		}
	}
}
