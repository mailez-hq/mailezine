package ha

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFSLeaseLifecycle(t *testing.T) {
	store := NewFSStore(filepath.Join(t.TempDir(), "lease.json"))
	ctx := context.Background()

	l, err := store.TryAcquire(ctx, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if l.Epoch != 1 {
		t.Fatalf("first lease epoch = %d, want 1", l.Epoch)
	}
	// Second instance cannot acquire while a holds a valid lease.
	if _, err := store.TryAcquire(ctx, "b", time.Second); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("b acquired while a holds: %v", err)
	}
	// Renew works only for the holder.
	if l, err := store.Renew(ctx, "a", time.Second); err != nil || l.Epoch != 1 {
		t.Fatalf("renew: lease=%+v err=%v", l, err)
	}
	if _, err := store.Renew(ctx, "b", time.Second); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("b renewed without holding: %v", err)
	}
	// Expiry lets b take over; the fencing epoch must advance.
	time.Sleep(1100 * time.Millisecond)
	l, err = store.TryAcquire(ctx, "b", time.Second)
	if err != nil {
		t.Fatalf("b takeover after expiry: %v", err)
	}
	if l.Epoch != 2 {
		t.Fatalf("takeover epoch = %d, want 2", l.Epoch)
	}
	// The evicted holder sees its lease as stolen.
	if _, err := store.Renew(ctx, "a", time.Second); !errors.Is(err, ErrLeaseStolen) {
		t.Fatalf("stale holder renew err = %v, want ErrLeaseStolen", err)
	}
	// Release by owner frees the lease.
	if err := store.Release(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TryAcquire(ctx, "c", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestFSLeaseExpiredByTime(t *testing.T) {
	store := NewFSStore(filepath.Join(t.TempDir(), "lease.json"))
	ctx := context.Background()
	if _, err := store.TryAcquire(ctx, "a", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := store.TryAcquire(ctx, "b", time.Second); err != nil {
		t.Fatalf("b could not claim expired lease: %v", err)
	}
}

// Concurrent acquirers racing for a free (and expired) lease must yield
// exactly one winner per round.
func TestFSLeaseConcurrentAcquire(t *testing.T) {
	store := NewFSStore(filepath.Join(t.TempDir(), "lease.json"))
	ctx := context.Background()
	if _, err := store.TryAcquire(ctx, "seed", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)

	const racers = 8
	winners := make(chan string, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.TryAcquire(ctx, fmt.Sprintf("r%d", i), time.Second); err == nil {
				winners <- fmt.Sprintf("r%d", i)
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	n := 0
	for range winners {
		n++
	}
	if n != 1 {
		t.Fatalf("race produced %d winners, want exactly 1", n)
	}
}

// fakeStore drives Leader.Run without real storage.
type fakeStore struct {
	mu       sync.Mutex
	holder   string
	renewErr error
	renews   int
}

func (f *fakeStore) TryAcquire(_ context.Context, owner string, _ time.Duration) (*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holder = owner
	return &Lease{Owner: owner, Epoch: 1}, nil
}

func (f *fakeStore) Renew(_ context.Context, owner string, _ time.Duration) (*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	if f.holder != owner {
		return nil, ErrLeaseStolen
	}
	return &Lease{Owner: owner, ExpiresAt: time.Now().Add(time.Minute), Epoch: 1}, nil
}

func (f *fakeStore) Release(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder == owner {
		f.holder = ""
	}
	return nil
}

func TestLeaderRunReportsLossOnStolen(t *testing.T) {
	fs := &fakeStore{holder: "self"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := NewLeader(fs, "self", 30*time.Millisecond, nil)
	if err := l.TryAcquire(ctx); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock() // someone else quietly holds it now
	fs.holder = "other"
	fs.mu.Unlock()

	lost := make(chan struct{})
	done := make(chan struct{})
	go func() { go func() { l.Run(ctx, func() { close(lost) }) }(); close(done) }()

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("onLost not invoked after steal")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

func TestLeaderRunGivesUpAfterTransientFailures(t *testing.T) {
	fs := &fakeStore{holder: "self"}
	fs.mu.Lock()
	fs.renewErr = errors.New("connection reset")
	fs.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := NewLeader(fs, "self", 30*time.Millisecond, nil)
	_, _ = fs.TryAcquire(ctx, "self", time.Minute)

	lost := make(chan struct{})
	go l.Run(ctx, func() { close(lost) })

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("onLost not invoked after repeated renewal failures")
	}
}

func TestLeaderRunQuietOnHealthyRenewals(t *testing.T) {
	fs := &fakeStore{holder: "self"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := NewLeader(fs, "self", 20*time.Millisecond, nil)
	_, _ = fs.TryAcquire(ctx, "self", time.Minute)

	lost := make(chan struct{})
	go l.Run(ctx, func() { close(lost) })
	time.Sleep(150 * time.Millisecond) // several renewal ticks

	select {
	case <-lost:
		t.Fatal("leadership lost despite healthy renewals")
	default:
	}
}
