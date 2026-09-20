package mailstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

	aid, err := s.AccountByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if err := ms.DeleteAccount(ctx, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	// Both counter kinds must go with the account, or they stay orphaned
	// under the dead account ID.
	for _, kind := range []byte{store.CounterKindNextDoc, store.CounterKindChange} {
		err := s.ScanRaw(ctx, store.CounterKey(uint32(aid), kind, nil), func(k, _ []byte) error {
			return fmt.Errorf("counter row survived purge: %q", k)
		})
		if err != nil {
			t.Fatal(err)
		}
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

func TestKVListMessagesUIDOrder(t *testing.T) {
	ms, _ := newTestKV(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	// The Work copy has the older document ID but the newer UID: the point
	// where docID order and UID order disagree.
	if _, err := ms.Deliver(ctx, "alice@example.com", "Work", &Message{From: "w@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "i@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	moved, err := ms.Move(ctx, "alice@example.com", "Work", "INBOX", []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	newUID, ok := moved[1]
	if !ok {
		t.Fatalf("move result missing uid mapping: %v", moved)
	}

	msgs, err := ms.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].UID <= msgs[i-1].UID {
			t.Fatalf("listing not in UID order: %v (move map %v)", []uint32{msgs[0].UID, msgs[1].UID}, moved)
		}
	}
	if msgs[1].UID != newUID {
		t.Fatalf("last uid = %d, want moved uid %d", msgs[1].UID, newUID)
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
// reference disappears the blob must be reclaimable by the sweep (stageBlob
// unlink used to delete the link row outright, making the refs==0 reclaim
// branch unreachable — every message blob leaked forever; later the inline
// reclaim was removed because it raced deliveries and could delete another
// account's shared copy — the sweep is the only reclamation path now).
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
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", 1, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(ctx, "alice@example.com", "INBOX", []uint32{1}); err != nil {
		t.Fatal(err)
	}
	// Reclamation is deferred to the globally-verified sweep; a zero-grace
	// pass reclaims the claimed blob immediately.
	n, err := s.SweepBlobs(ctx, 0, time.Now(), s.DeleteBlob)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sweep reclaimed %d blobs, want 1", n)
	}
	var buf bytes.Buffer
	if err := s.GetBlob(ctx, blobID, &buf); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blob survived expunge+sweep: err=%v", err)
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

// TestBlobGCSharedAcrossAccounts pins the reason reclamation is a sweep:
// blob IDs are content-addressed and global, so identical content delivered
// to two accounts shares one file. Deleting one account's copy (and sweeping
// at zero grace) must leave the other account's mail readable.
func TestBlobGCSharedAcrossAccounts(t *testing.T) {
	ms, s := newTestKV(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: both@example.test\r\nSubject: shared\r\n\r\nsame bytes\r\n"

	for _, acct := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := ms.Deliver(ctx, acct, "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
			t.Fatal(err)
		}
	}
	ae, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	be, err := ms.EmailByUID(ctx, "bob@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ae.BlobID != be.BlobID {
		t.Fatalf("expected content-addressed sharing: %q vs %q", ae.BlobID, be.BlobID)
	}
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", 1, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(ctx, "alice@example.com", "INBOX", []uint32{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepBlobs(ctx, 0, time.Now(), s.DeleteBlob); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.GetBlob(ctx, be.BlobID, &buf); err != nil {
		t.Fatalf("bob's mail lost after alice's expunge: %v", err)
	}
	if _, err := ms.OpenMessage(ctx, "bob@example.com", "INBOX", 1); err != nil {
		t.Fatalf("bob cannot read his message: %v", err)
	}
}

// TestBlobGCSweepReverifiesClaim covers a claim that disappears after the
// sweep's mark scan: the sweep re-reads the live claim before deleting, so a
// blob it no longer claims must survive.
func TestBlobGCSweepReverifiesClaim(t *testing.T) {
	ms, s := newTestKV(t)
	ctx := context.Background()

	bodies := []string{
		"From: s@remote.test\r\nSubject: keep\r\n\r\nunique-A\r\n",
		"From: s@remote.test\r\nSubject: drop\r\n\r\nunique-B\r\n",
	}
	for _, body := range bodies {
		if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
			t.Fatal(err)
		}
	}
	e1, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", 1, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", 2, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(ctx, "alice@example.com", "INBOX", []uint32{1, 2}); err != nil {
		t.Fatal(err)
	}
	// e2's blob is claimed first, so its reclaim runs first.
	var older [8]byte
	binary.BigEndian.PutUint64(older[:], uint64(time.Now().Add(-time.Hour).Unix()))
	if err := s.PutRaw(ctx, store.MetaBlobGCKey(e2.BlobID), older[:]); err != nil {
		t.Fatal(err)
	}

	reclaimed := []string{}
	n, err := s.SweepBlobs(ctx, 0, time.Now(), func(ctx context.Context, blobID string) error {
		if blobID == e2.BlobID {
			// A re-link commit for the other blob, after the mark scan.
			if err := s.DeleteBlobGCClaim(ctx, e1.BlobID); err != nil {
				return err
			}
		}
		if err := s.DeleteBlob(ctx, blobID); err != nil {
			return err
		}
		reclaimed = append(reclaimed, blobID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(reclaimed) != 1 || reclaimed[0] != e2.BlobID {
		t.Fatalf("sweep reclaimed %v (n=%d), want only %s", reclaimed, n, e2.BlobID)
	}
	var buf bytes.Buffer
	if err := s.GetBlob(ctx, e1.BlobID, &buf); err != nil {
		t.Fatalf("re-linked blob was deleted despite claim re-check: %v", err)
	}
	if err := s.GetBlob(ctx, e2.BlobID, &buf); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first-queued blob survived sweep: %v", err)
	}
}

// TestExpungeTombstonesSurviveBatch guards the QRESYNC tombstone key
// layout: a batch expunge of N messages shares one expunge modseq, and the
// tombstone key must carry the UID — without it the N writes collapse onto
// one key and VANISHED (EARLIER) reports only the last UID of the batch.
func TestExpungeTombstonesSurviveBatch(t *testing.T) {
	ms, s := newTestKV(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		body := "From: s@remote.test\r\nSubject: m\r\n\r\nbody\r\n"
		if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", uint32(i), []string{"\\Deleted"}); err != nil {
			t.Fatal(err)
		}
	}
	gone, err := ms.Expunge(ctx, "alice@example.com", "INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 3 {
		t.Fatalf("expunged %d messages, want 3", len(gone))
	}
	uids, err := ms.ExpungedSince(ctx, "alice@example.com", "INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 3 || uids[0] != 1 || uids[1] != 2 || uids[2] != 3 {
		t.Fatalf("VANISHED reported %v, want [1 2 3] — tombstones collapsed?", uids)
	}
	_ = s
}

// TestUIDExpungeRespectsDeletedFlag pins the store-level \Deleted check:
// UID EXPUNGE must not destroy a message whose \Deleted flag was cleared by
// a concurrent session between the caller's snapshot and the delete
// transaction.
func TestUIDExpungeRespectsDeletedFlag(t *testing.T) {
	ms, s := newTestKV(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nSubject: keep\r\n\r\nstay\r\n"
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", 1, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	// Simulate the concurrent clear: drop the flag behind the caller's back
	// (direct flag rewrite, no expunge).
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", 1, nil); err != nil {
		t.Fatal(err)
	}
	gone, err := ms.Expunge(ctx, "alice@example.com", "INBOX", []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Fatalf("UID EXPUNGE deleted %v despite cleared \\Deleted flag", gone)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); err != nil {
		t.Fatalf("message must survive: %v", err)
	}
	_ = s
}
