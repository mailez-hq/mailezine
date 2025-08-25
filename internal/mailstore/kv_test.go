package mailstore

import (
	"context"
	"errors"
	"testing"

	"mailezine/internal/store"
)

func newTestKV(t *testing.T) (*KV, *store.Store) {
	t.Helper()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	return NewKV(s), s
}

func TestKVDeliverAndRead(t *testing.T) {
	ms, _ := newTestKV(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	u1, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body), Seen: true})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body), Keywords: []string{"$Snoozed"}})
	if err != nil {
		t.Fatal(err)
	}
	if u1 != 1 || u2 != 2 {
		t.Fatalf("uids = %d,%d want 1,2", u1, u2)
	}

	e1, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !e1.Seen() || len(e1.Keywords) != 0 {
		t.Fatalf("email1: %+v", e1)
	}
	e2, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 2)
	if err != nil {
		t.Fatal(err)
	}
	if e2.Seen() || len(e2.Keywords) != 1 || e2.Keywords[0] != "$Snoozed" {
		t.Fatalf("email2: %+v", e2)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found for uid 3, got %v", err)
	}

	q, err := ms.QuotaUsedBytes(ctx, "alice@example.com")
	if err != nil || q != 2*int64(len(body)) {
		t.Fatalf("quota = %d err=%v, want %d", q, err, 2*int64(len(body)))
	}
}

func TestKVQuotaEmptyAccount(t *testing.T) {
	ms, _ := newTestKV(t)
	q, err := ms.QuotaUsedBytes(context.Background(), "nobody@example.com")
	if err != nil || q != 0 {
		t.Fatalf("quota = %d err=%v, want 0", q, err)
	}
}
