package mailstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"mailezine/internal/store"
)

func newTestKV(t *testing.T) (*KV, *store.Store) {
	t.Helper()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	return NewKV(s), s
}

// TestKVDeleteAccount proves the purge primitive drops every trace of the
// account: registry rows, mailboxes, messages and quota, and that a same
// address re-created afterwards starts empty (no inheritance).
func TestKVDeleteAccount(t *testing.T) {
	ms, s := newTestKV(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	if err := ms.EnsureDefaultMailboxes(ctx, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Deliver(ctx, "bob@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}

	if err := ms.DeleteAccount(ctx, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountByEmail(ctx, "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("email registry row survived purge: %v", err)
	}
	if err := ms.DeleteAccount(ctx, "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("re-purge of absent account should be ErrNotFound, got %v", err)
	}

	// The other account is untouched.
	if boxes, err := ms.ListMailboxes(ctx, "bob@example.com"); err != nil || len(boxes) == 0 {
		t.Fatalf("bob affected by alice purge: %v %+v", err, boxes)
	}

	// A same-address re-creation starts with a clean slate: UID 1 proves the
	// UID counter was purged rather than inherited (the pre-purge INBOX
	// already had a message at UID 1).
	uid, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1 {
		t.Fatalf("re-created account inherited UID counter: first uid = %d", uid)
	}
	msgs, err := ms.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("re-created account inherited data: %d messages", len(msgs))
	}
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

// Concurrent first deliveries to the same fresh mailbox must resolve to
// exactly one mailbox document (INV-DELIVERY): the check-create sequence
// must resolve in a single transaction, otherwise racers fork duplicate
// mailboxes whose messages then hide from clients.
func TestConcurrentFirstDeliverSingleMailbox(t *testing.T) {
	ms, _ := newTestKV(t)
	ctx := context.Background()
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf("From: s@remote.test\r\nSubject: c%d\r\n\r\n%d\r\n", i, i))
			if _, err := ms.Deliver(ctx, "alice@example.com", "Junk", &Message{From: "s@remote.test", Data: body}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	boxes, err := ms.ListMailboxes(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	dupes := 0
	for _, b := range boxes {
		if b.Name == "Junk" {
			dupes++
		}
	}
	if dupes != 1 {
		t.Fatalf("Junk appears %d times in mailbox list, want exactly 1: %v", dupes, boxes)
	}
	msgs, err := ms.ListMessages(ctx, "alice@example.com", "Junk")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("messages in Junk = %d, want %d", len(msgs), n)
	}
	// UIDs stay dense and ordered (INV-UID).
	for i, m := range msgs {
		if m.UID != uint32(i+1) {
			t.Fatalf("uid[%d] = %d, want %d", i, m.UID, i+1)
		}
	}
}

// TestKVExpungeReclaimsBlob proves the blob GC path is live: once the last
// reference disappears the blob itself must be gone (stageBlobUnlink used
// to delete the link row outright, making the refs==0 reclaim branch in
// deleteEmail unreachable — every message blob leaked forever).
func TestKVExpungeReclaimsBlob(t *testing.T) {
	ms, s := newTestKV(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	e, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	blobID := e.BlobID
	if blobID == "" {
		t.Fatal("delivered email has no blob reference")
	}
	if _, err := ms.Expunge(ctx, "alice@example.com", "INBOX", []uint32{1}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.GetBlob(ctx, blobID, &buf); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blob survived expunge: err=%v", err)
	}
	// The zero-count tombstone must be dropped as well.
	aid, err := s.AccountByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if refs, err := s.BlobRefCount(ctx, aid, blobID); err == nil {
		t.Fatalf("blob link tombstone survived reclaim: refs=%d", refs)
	}
}
