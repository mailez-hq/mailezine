package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"mailezine/internal/mailcache"
)

type countingService struct {
	calls int
	ok    bool
	err   error
}

func (s *countingService) Authenticate(_ context.Context, _, password string, _ Options) (bool, error) {
	s.calls++
	if password != "right" {
		return false, errors.New("bad")
	}
	return s.ok, s.err
}

func (s *countingService) Close() error { return nil }

func TestCachedAuthentication(t *testing.T) {
	inner := &countingService{ok: true}
	c := NewCached(inner, mailcache.NewCacheWithTTL(1024, time.Minute))
	ctx := context.Background()

	ok, err := c.Authenticate(ctx, "u@example.com", "right", Options{})
	if err != nil || !ok {
		t.Fatalf("first auth: %v %v", ok, err)
	}
	ok, err = c.Authenticate(ctx, "u@example.com", "right", Options{})
	if err != nil || !ok {
		t.Fatalf("cached auth: %v %v", ok, err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner called %d times, want 1 (cached)", inner.calls)
	}
}

func TestCachedWrongPasswordNotCached(t *testing.T) {
	inner := &countingService{ok: true}
	c := NewCached(inner, mailcache.NewCacheWithTTL(1024, time.Minute))
	ctx := context.Background()

	if _, err := c.Authenticate(ctx, "u@example.com", "wrong", Options{}); err == nil {
		t.Fatal("wrong password should fail")
	}
	if _, err := c.Authenticate(ctx, "u@example.com", "wrong", Options{}); err == nil {
		t.Fatal("wrong password should fail twice (failures not cached)")
	}
	if inner.calls != 2 {
		t.Fatalf("inner called %d times, want 2 (no negative cache)", inner.calls)
	}
}

func TestCachedTTLExpiry(t *testing.T) {
	inner := &countingService{ok: true}
	c := NewCached(inner, mailcache.NewCacheWithTTL(1024, 20*time.Millisecond))
	ctx := context.Background()

	_, _ = c.Authenticate(ctx, "u@example.com", "right", Options{})
	time.Sleep(30 * time.Millisecond)
	_, _ = c.Authenticate(ctx, "u@example.com", "right", Options{})
	if inner.calls != 2 {
		t.Fatalf("inner called %d times, want 2 (TTL expired)", inner.calls)
	}
}
