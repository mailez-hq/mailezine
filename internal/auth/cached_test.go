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
	if password != "token-right" {
		return false, errors.New("bad")
	}
	return s.ok, s.err
}

func (s *countingService) Close() error { return nil }

func TestCachedAuthentication(t *testing.T) {
	inner := &countingService{ok: true}
	c := NewCached(inner, mailcache.NewCacheWithTTL(1024, time.Minute))
	ctx := context.Background()

	ok, err := c.Authenticate(ctx, "u@example.com", "token-right", Options{})
	if err != nil || !ok {
		t.Fatalf("first auth: %v %v", ok, err)
	}
	ok, err = c.Authenticate(ctx, "u@example.com", "token-right", Options{})
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

	if _, err := c.Authenticate(ctx, "u@example.com", "token-wrong", Options{}); err == nil {
		t.Fatal("wrong password should fail")
	}
	if _, err := c.Authenticate(ctx, "u@example.com", "token-wrong", Options{}); err == nil {
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

	_, _ = c.Authenticate(ctx, "u@example.com", "token-right", Options{})
	time.Sleep(30 * time.Millisecond)
	_, _ = c.Authenticate(ctx, "u@example.com", "token-right", Options{})
	if inner.calls != 2 {
		t.Fatalf("inner called %d times, want 2 (TTL expired)", inner.calls)
	}
}

// protocolGatedService allows submission and denies IMAP.
type protocolGatedService struct {
	calls map[string]int
}

func (s *protocolGatedService) Authenticate(_ context.Context, _, password string, opts Options) (bool, error) {
	s.calls[opts.Protocol]++
	if password != "right" {
		return false, nil
	}
	return opts.Protocol != "imap", nil
}

func (s *protocolGatedService) Close() error { return nil }

// TestCachedNoCrossProtocolReuse pins the bypass this cache used to allow:
// an SMTP submission primed the pair's entry and the next IMAP login
// succeeded without the control plane seeing it.
func TestCachedNoCrossProtocolReuse(t *testing.T) {
	inner := &protocolGatedService{calls: map[string]int{}}
	c := NewCached(inner, mailcache.NewCacheWithTTL(1024, 10*time.Minute))
	ctx := context.Background()

	if ok, _ := c.Authenticate(ctx, "u@example.com", "right", Options{Protocol: "smtp", Port: "1587"}); !ok {
		t.Fatal("submission with the account password must succeed")
	}
	ok, err := c.Authenticate(ctx, "u@example.com", "right", Options{Protocol: "imap", Port: "143"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("smtp success authorized imap: enable_imap was bypassed through the auth cache")
	}
	if inner.calls["imap"] != 1 {
		t.Fatalf("control plane saw %d imap attempts, want 1", inner.calls["imap"])
	}
}

// TestCachedSkipsRealClientCredentials pins the policy: a password or app
// token always goes to the control plane.
func TestCachedSkipsRealClientCredentials(t *testing.T) {
	inner := &protocolGatedService{calls: map[string]int{}}
	c := NewCached(inner, mailcache.NewCacheWithTTL(1024, 10*time.Minute))
	ctx := context.Background()

	for range 3 {
		if ok, _ := c.Authenticate(ctx, "u@example.com", "right", Options{Protocol: "smtp"}); !ok {
			t.Fatal("submission should succeed")
		}
	}
	if inner.calls["smtp"] != 3 {
		t.Fatalf("control plane saw %d smtp attempts, want 3 (no memo for passwords)", inner.calls["smtp"])
	}
}
