package directory

import (
	"context"
	"errors"
	"testing"
)

func testDev() *Dev {
	return NewDev(DevData{
		Users: map[string]User{
			"alice@example.com": {
				Email:      "alice@example.com",
				Enabled:    true,
				QuotaBytes: 1 << 30,
			},
		},
		Domains: []string{"example.com"},
		Aliases: map[string][]string{
			"team@example.com": {"alice@example.com"},
		},
		Relays: map[string]Relay{
			"relay.example.net": {Domain: "relay.example.net", Transport: "smtp:relay.example.net:25"},
		},
		Senders: map[string][]string{
			"alice@example.com": {"alice@example.com"},
		},
		Sieve: map[string]string{
			"alice@example.com": `require "fileinto"; if header :contains "Subject" "spam" { fileinto "Junk"; }`,
		},
	})
}

func TestDevUser(t *testing.T) {
	d := testDev()
	u, err := d.User(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Enabled || u.QuotaBytes != 1<<30 {
		t.Fatalf("unexpected user: %+v", u)
	}
	if _, err := d.User(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDevDomain(t *testing.T) {
	d := testDev()
	dom, err := d.Domain(context.Background(), "EXAMPLE.COM")
	if err != nil {
		t.Fatal(err)
	}
	if !dom.IsLocal {
		t.Fatalf("expected local domain: %+v", dom)
	}
	if _, err := d.Domain(context.Background(), "elsewhere.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDevAliases(t *testing.T) {
	d := testDev()
	targets, err := d.Aliases(context.Background(), "team@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != "alice@example.com" {
		t.Fatalf("unexpected targets: %v", targets)
	}
	// A user resolves to itself.
	targets, err = d.Aliases(context.Background(), "alice@example.com")
	if err != nil || len(targets) != 1 || targets[0] != "alice@example.com" {
		t.Fatalf("user self-resolution: %v err=%v", targets, err)
	}
	// Bare domain resolves to the domain (post-office).
	targets, err = d.Aliases(context.Background(), "example.com")
	if err != nil || len(targets) != 1 || targets[0] != "example.com" {
		t.Fatalf("bare domain: %v err=%v", targets, err)
	}
	if _, err := d.Aliases(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDevRelaySender(t *testing.T) {
	d := testDev()
	r, err := d.Relay(context.Background(), "x@relay.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if r.Transport != "smtp:relay.example.net:25" {
		t.Fatalf("unexpected relay: %+v", r)
	}
	s, err := d.Sender(context.Background(), "alice@example.com", "alice@example.com")
	if err != nil || !s.Allowed {
		t.Fatalf("sender: %+v err=%v", s, err)
	}
	// An authenticated user may not use another user's address without a
	// send-as grant.
	if _, err := d.Sender(context.Background(), "mallory@example.com", "alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign sender, got %v", err)
	}
	// A send-as grant in Senders[user] allows that user's address.
	if _, err := d.Sender(context.Background(), "bob@example.com", "alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound without grant, got %v", err)
	}
}

func TestDevSRS(t *testing.T) {
	d := testDev()
	if _, err := d.SRSForward(context.Background(), "alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("local sender must not be rewritten, got %v", err)
	}
	rewritten, err := d.SRSForward(context.Background(), "bob@remote.net")
	if err != nil {
		t.Fatal(err)
	}
	original, err := d.SRSRestore(context.Background(), rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if original != "bob@remote.net" {
		t.Fatalf("round-trip failed: %q", original)
	}
	if _, err := d.SRSRestore(context.Background(), "not-an-srs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDevQuotaAndSieve(t *testing.T) {
	d := testDev()
	q, err := d.Quota(context.Background(), "alice@example.com")
	if err != nil || q.Limit != 1<<30 || q.Used != 0 {
		t.Fatalf("quota: %+v err=%v", q, err)
	}
	if err := d.UpdateQuotaUsed(context.Background(), "alice@example.com", 42); err != nil {
		t.Fatal(err)
	}
	q, _ = d.Quota(context.Background(), "alice@example.com")
	if q.Used != 42 {
		t.Fatalf("quota used not updated: %+v", q)
	}
	s, err := d.Sieve(context.Background(), "alice@example.com")
	if err != nil || s.Name != "default" || s.Script == "" {
		t.Fatalf("sieve: %+v err=%v", s, err)
	}
	// Unknown user has no quota/sieve.
	if _, err := d.Quota(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
