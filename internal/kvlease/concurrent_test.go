package kvlease

import (
	"context"
	"sync"
	"testing"
	"time"

	"mailezine/internal/store"
)

// barrierKV holds the lease read open until every concurrent reader has
// arrived (or a short grace period passes), so the first-acquisition
// interleaving is forced instead of being a scheduling accident: without
// serialization both workers read "no lease" and both claim it.
type barrierKV struct {
	store.KV
	readers int

	mu   sync.Mutex
	seen int
	gate chan struct{}
}

func (b *barrierKV) Get(key []byte) ([]byte, error) {
	b.mu.Lock()
	if b.seen < b.readers {
		b.seen++
		if b.seen == b.readers {
			close(b.gate)
		}
		gate := b.gate
		b.mu.Unlock()
		select {
		case <-gate:
		case <-time.After(50 * time.Millisecond):
		}
	} else {
		b.mu.Unlock()
	}
	return b.KV.Get(key)
}

// Two workers pointed at the same KV must never both hold the lease, however
// the goroutines interleave: the AsTxn buffer commits the claim only after the
// closure has read the key, so an unserialized read-modify-write lets both win.
func TestAcquireIsAtomicUnderConcurrency(t *testing.T) {
	kv := &barrierKV{KV: store.NewMemoryKV(), readers: 2, gate: make(chan struct{})}
	clock := &fakeClock{now: time.Now()}

	for round := 0; round < 5; round++ {
		a := newLease(kv, "a", time.Minute, clock)
		b := newLease(kv, "b", time.Minute, clock)

		var wg sync.WaitGroup
		results := make([]bool, 2)
		start := make(chan struct{})
		for i, l := range []*Lease{a, b} {
			wg.Add(1)
			go func(i int, l *Lease) {
				defer wg.Done()
				<-start
				held, err := l.Acquire(context.Background())
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				results[i] = held
			}(i, l)
		}
		close(start)
		wg.Wait()

		if results[0] && results[1] {
			t.Fatalf("round %d: both workers acquired the same lease", round)
		}
		if !results[0] && !results[1] {
			t.Fatalf("round %d: neither worker acquired a free lease", round)
		}
		if err := a.Release(context.Background()); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
}
