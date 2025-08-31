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
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
