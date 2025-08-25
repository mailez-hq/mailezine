package ha

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestFSLeaseLifecycle(t *testing.T) {
	store := NewFSStore(filepath.Join(t.TempDir(), "lease.json"))
	ctx := context.Background()

	if err := store.TryAcquire(ctx, "a", time.Second); err != nil {
		t.Fatal(err)
	}
	// Second instance cannot acquire while a holds a valid lease.
	if err := store.TryAcquire(ctx, "b", time.Second); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("b acquired while a holds: %v", err)
	}
	// Renew works only for the holder.
	if err := store.Renew(ctx, "a", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.Renew(ctx, "b", time.Second); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("b renewed without holding: %v", err)
	}
	// Expiry lets b take over.
	time.Sleep(1100 * time.Millisecond)
	if err := store.TryAcquire(ctx, "b", time.Second); err != nil {
		t.Fatalf("b takeover after expiry: %v", err)
	}
	// Release by owner frees the lease.
	if err := store.Release(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := store.TryAcquire(ctx, "c", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestFSLeaseExpiredByTime(t *testing.T) {
	store := NewFSStore(filepath.Join(t.TempDir(), "lease.json"))
	ctx := context.Background()
	if err := store.TryAcquire(ctx, "a", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := store.TryAcquire(ctx, "b", time.Second); err != nil {
		t.Fatalf("b could not claim expired lease: %v", err)
	}
}
