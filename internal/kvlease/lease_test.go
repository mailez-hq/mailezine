package kvlease

import (
	"context"
	"testing"
	"time"

	"mailezine/internal/store"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newLease(kv store.KV, owner string, ttl time.Duration, clock *fakeClock) *Lease {
	l := New(kv, "test", owner, ttl)
	if clock != nil {
		l.SetClock(clock.Now)
	}
	return l
}

func TestMutualExclusion(t *testing.T) {
	kv := store.NewMemoryKV()
	clock := &fakeClock{now: time.Now()}
	a := newLease(kv, "a", time.Minute, clock)
	b := newLease(kv, "b", time.Minute, clock)
	ctx := context.Background()

	if ok, err := a.Acquire(ctx); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := b.Acquire(ctx); err != nil || ok {
		t.Fatalf("second acquire must fail while held: ok=%v err=%v", ok, err)
	}
	// The holder re-acquires (renewal path).
	if ok, err := a.Acquire(ctx); err != nil || !ok {
		t.Fatalf("re-acquire by holder: ok=%v err=%v", ok, err)
	}
}

func TestExpirySteal(t *testing.T) {
	kv := store.NewMemoryKV()
	clock := &fakeClock{now: time.Now()}
	a := newLease(kv, "a", time.Minute, clock)
	b := newLease(kv, "b", time.Minute, clock)
	ctx := context.Background()

	if ok, _ := a.Acquire(ctx); !ok {
		t.Fatal("setup: a must acquire")
	}
	// Not yet expired: b still refused.
	clock.Advance(30 * time.Second)
	if ok, _ := b.Acquire(ctx); ok {
		t.Fatal("b stole a live lease")
	}
	// Expired: b takes over, a can no longer renew.
	clock.Advance(45 * time.Second)
	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("b must steal the expired lease")
	}
	if ok, _ := a.Acquire(ctx); ok {
		t.Fatal("expired owner must not re-acquire over the successor")
	}
}

func TestReleaseFreesAndRespectsForeignHolder(t *testing.T) {
	kv := store.NewMemoryKV()
	clock := &fakeClock{now: time.Now()}
	a := newLease(kv, "a", time.Minute, clock)
	b := newLease(kv, "b", time.Minute, clock)
	ctx := context.Background()

	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("setup: b must acquire")
	}
	// a releasing a foreign lease must be a no-op.
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Acquire(ctx); !ok {
		t.Fatal("foreign release must not drop the lease")
	}
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Acquire(ctx); !ok {
		t.Fatal("released lease must be acquirable")
	}
}

func TestRunElectsSingleWorker(t *testing.T) {
	kv := store.NewMemoryKV()
	clock := &fakeClock{now: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := 10 * time.Millisecond
	ttl := time.Minute
	var runsA, runsB int
	done := make(chan struct{})
	go func() {
		newLease(kv, "a", ttl, clock).Run(ctx, tick, func(context.Context) { runsA++ })
		close(done)
	}()
	go newLease(kv, "b", ttl, clock).Run(ctx, tick, func(context.Context) { runsB++ })

	deadline := time.After(2 * time.Second)
	for {
		if runsA+runsB >= 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("workers never ran: a=%d b=%d", runsA, runsB)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	// One node ran every tick, the other none: exactly-once per tick.
	if runsA > 0 && runsB > 0 {
		t.Fatalf("both nodes ran the worker: a=%d b=%d", runsA, runsB)
	}
	if runsA+runsB < 5 {
		t.Fatalf("expected repeated ticks, got a=%d b=%d", runsA, runsB)
	}
}
