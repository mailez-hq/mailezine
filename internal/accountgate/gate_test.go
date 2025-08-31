package accountgate

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"mailezine/internal/metrics"
	"mailezine/internal/store"
)

func newGate(kv store.KV, node string, opts Options) *Gate {
	return New(kv, node, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Two nodes, one account: the second node's writer must not enter its
// section while the first node's is inside — the cross-node serialization
// the gate exists for.
func TestCrossNodeSerialization(t *testing.T) {
	kv := store.NewMemoryKV()
	ga := newGate(kv, "node-a", Options{TTL: 5 * time.Second, IdleRelease: 10 * time.Second, Wait: 5 * time.Second})
	gb := newGate(kv, "node-b", Options{TTL: 5 * time.Second, IdleRelease: 10 * time.Second, Wait: 5 * time.Second})
	ctx := context.Background()

	var mu sync.Mutex
	var events []string
	releaseA := make(chan struct{})
	startedB := make(chan struct{})
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)

	go func() {
		doneA <- ga.WithAccount(ctx, "alice@example.com", func() error {
			mu.Lock()
			events = append(events, "A:start")
			mu.Unlock()
			<-releaseA // node A stays inside its write section
			mu.Lock()
			events = append(events, "A:end")
			mu.Unlock()
			return nil
		})
	}()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 1
	})

	go func() {
		doneB <- gb.WithAccount(ctx, "alice@example.com", func() error {
			close(startedB)
			mu.Lock()
			events = append(events, "B:start")
			mu.Unlock()
			return nil
		})
	}()

	// Node B must be waiting, not writing.
	select {
	case <-startedB:
		t.Fatal("node B entered the write section while node A held the account")
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseA) // node A finishes; node B may now proceed
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 3
	})
	mu.Lock()
	defer mu.Unlock()
	want := []string{"A:start", "A:end", "B:start"}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

// The gate is advisory: a writer outlasting the wait budget proceeds
// WITHOUT the lease — mail is never blocked, correctness falls back to
// transactional replay.
func TestFallbackProceedsWithoutLease(t *testing.T) {
	kv := store.NewMemoryKV()
	ga := newGate(kv, "node-a", Options{TTL: 5 * time.Second, IdleRelease: 10 * time.Second, Wait: 5 * time.Second})
	gb := newGate(kv, "node-b", Options{TTL: 5 * time.Second, IdleRelease: 10 * time.Second, Wait: 150 * time.Millisecond})
	mm := metrics.New()
	gb.SetMetrics(mm)
	ctx := context.Background()

	blockA := make(chan struct{})
	doneA := make(chan error, 1)
	go func() {
		doneA <- ga.WithAccount(ctx, "alice@example.com", func() error {
			<-blockA
			return nil
		})
	}()
	time.Sleep(100 * time.Millisecond) // let node A take the lease

	start := time.Now()
	ran := make(chan struct{})
	if err := gb.WithAccount(ctx, "alice@example.com", func() error {
		close(ran)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-ran
	close(blockA)
	<-doneA

	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("fallback too fast (%v): the wait budget was not honoured", elapsed)
	}
	if got := mm.AccountGateEvents; got == nil {
		t.Fatal("gate metrics not registered")
	}
}

// Held accounts renew in the background and release once idle: ownership
// follows traffic instead of accumulating.
func TestRenewAndIdleRelease(t *testing.T) {
	kv := store.NewMemoryKV()
	opts := Options{TTL: 60 * time.Millisecond, IdleRelease: 150 * time.Millisecond, Wait: time.Second}
	g := newGate(kv, "node-a", opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	if err := g.WithAccount(ctx, "alice@example.com", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if g.Held() != 1 {
		t.Fatalf("held = %d, want 1", g.Held())
	}
	// Several TTLs elapse while inside the idle window (TTL 60ms, idle
	// 150ms): renewal must keep the lease alive.
	time.Sleep(100 * time.Millisecond)
	if g.Held() != 1 {
		t.Fatalf("held after renewals = %d, want 1 (lease was not renewed)", g.Held())
	}
	// Idle past the release window: the lease is dropped.
	time.Sleep(300 * time.Millisecond)
	waitFor(t, func() bool { return g.Held() == 0 })
}

// A foreign expired lease is taken over: pinning follows failures.
func TestExpiredOwnerTakeover(t *testing.T) {
	kv := store.NewMemoryKV()
	ga := newGate(kv, "node-a", Options{TTL: 40 * time.Millisecond, IdleRelease: time.Second, Wait: time.Second})
	gb := newGate(kv, "node-b", Options{TTL: 40 * time.Millisecond, IdleRelease: time.Second, Wait: time.Second})
	ctx := context.Background()

	if err := ga.WithAccount(ctx, "alice@example.com", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// node-a's lease lapses (no maintenance loop running for it).
	time.Sleep(120 * time.Millisecond)
	if err := gb.WithAccount(ctx, "alice@example.com", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if gb.Held() != 1 {
		t.Fatal("node B did not take over the expired account lease")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition never became true")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
