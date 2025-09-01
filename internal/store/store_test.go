package store

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

func runAccountLifecycle(t *testing.T, s *Store) {
	ctx := context.Background()

	id, err := s.CreateAccount(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero account ID")
	}
	if _, err := s.CreateAccount(ctx, "alice@example.com"); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	got, err := s.AccountByEmail(ctx, "alice@example.com")
	if err != nil || got != id {
		t.Fatalf("resolve: id=%d err=%v", got, err)
	}
	email, err := s.AccountEmail(ctx, id)
	if err != nil || email != "alice@example.com" {
		t.Fatalf("email: %q err=%v", email, err)
	}
	if _, err := s.AccountByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// IDs are monotonic across accounts.
	id2, err := s.CreateAccount(ctx, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id {
		t.Fatalf("account IDs must be monotonic: %d then %d", id, id2)
	}
}

func runDocuments(t *testing.T, s *Store) {
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}

	var docIDs []uint64
	for i := 0; i < 3; i++ {
		id, err := s.CreateDocument(ctx, acct, CollectionEmail)
		if err != nil {
			t.Fatal(err)
		}
		docIDs = append(docIDs, id)
	}
	if docIDs[0] >= docIDs[1] || docIDs[1] >= docIDs[2] {
		t.Fatalf("document IDs must be monotonic: %v", docIDs)
	}

	// An empty document is discoverable.
	ids, err := s.ListDocumentIDs(ctx, acct, CollectionEmail)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 documents, got %v", ids)
	}

	// Put fields on docIDs[1] and read them back.
	fields := map[byte][]byte{1: []byte("subject"), 2: []byte("body")}
	if err := s.PutDocumentFields(ctx, acct, CollectionEmail, docIDs[1], fields); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDocumentFields(ctx, acct, CollectionEmail, docIDs[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[1], []byte("subject")) || !bytes.Equal(got[2], []byte("body")) {
		t.Fatalf("fields mismatch: %v", got)
	}

	// Deleting docIDs[1] removes it; the others remain.
	if err := s.DeleteDocument(ctx, acct, CollectionEmail, docIDs[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDocumentFields(ctx, acct, CollectionEmail, docIDs[1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	ids, err = s.ListDocumentIDs(ctx, acct, CollectionEmail)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != docIDs[0] || ids[1] != docIDs[2] {
		t.Fatalf("unexpected remaining docs: %v", ids)
	}
}

func runChangeLog(t *testing.T, s *Store) {
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}

	id1, _ := s.CreateDocument(ctx, acct, CollectionEmail)
	id2, _ := s.CreateDocument(ctx, acct, CollectionEmail)

	c1, err := s.AppendChange(ctx, acct, CollectionEmail, id1, OpCreate)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.AppendChange(ctx, acct, CollectionEmail, id1, OpUpdate)
	if err != nil {
		t.Fatal(err)
	}
	c3, err := s.AppendChange(ctx, acct, CollectionEmail, id2, OpDelete)
	if err != nil {
		t.Fatal(err)
	}
	if c1 >= c2 || c2 >= c3 {
		t.Fatalf("change IDs must be monotonic: %d %d %d", c1, c2, c3)
	}

	all, err := s.ChangesSince(ctx, acct, CollectionEmail, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ChangeID != c1 || all[2].ChangeID != c3 {
		t.Fatalf("changes: %+v", all)
	}
	if all[0].DocID != id1 || all[0].Op != OpCreate || all[2].Op != OpDelete {
		t.Fatalf("change payload mismatch: %+v", all)
	}

	tail, err := s.ChangesSince(ctx, acct, CollectionEmail, c2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].ChangeID != c3 {
		t.Fatalf("tail: %+v", tail)
	}

	// Different collections have independent logs.
	none, err := s.ChangesSince(ctx, acct, CollectionMailbox, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected empty log for other collection, got %+v", none)
	}
}

func runQuotaAccounting(t *testing.T, s *Store) {
	ctx := context.Background()
	acct, _ := s.CreateAccount(ctx, "alice@example.com")

	if n, err := s.AddQuotaUsed(ctx, acct, 100); err != nil || n != 100 {
		t.Fatalf("add 100: n=%d err=%v", n, err)
	}
	if n, err := s.AddQuotaUsed(ctx, acct, 50); err != nil || n != 150 {
		t.Fatalf("add 50: n=%d err=%v", n, err)
	}
	if n, err := s.QuotaUsed(ctx, acct); err != nil || n != 150 {
		t.Fatalf("used: n=%d err=%v", n, err)
	}
	// Conservation: remove exactly what was added.
	if _, err := s.AddQuotaUsed(ctx, acct, -150); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QuotaUsed(ctx, acct); n != 0 {
		t.Fatalf("expected 0 after conservation, got %d", n)
	}
}

func runBlobLinks(t *testing.T, s *Store) {
	ctx := context.Background()
	acct, _ := s.CreateAccount(ctx, "alice@example.com")

	if err := s.LinkBlob(ctx, acct, "blob1"); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkBlob(ctx, acct, "blob1"); err != nil {
		t.Fatal(err)
	}
	n, err := s.BlobRefCount(ctx, acct, "blob1")
	if err != nil || n != 2 {
		t.Fatalf("refcount: n=%d err=%v", n, err)
	}
	if err := s.UnlinkBlob(ctx, acct, "blob1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.BlobRefCount(ctx, acct, "blob1"); n != 1 {
		t.Fatalf("refcount after unlink: %d", n)
	}
	if err := s.UnlinkBlob(ctx, acct, "blob1"); err != nil {
		t.Fatal(err)
	}
	// At zero the counter is tombstoned (reads 0), not removed: the GC path
	// in mailstore reclaims the blob on refs==0 and then drops the tombstone.
	if n, err := s.BlobRefCount(ctx, acct, "blob1"); err != nil || n != 0 {
		t.Fatalf("refcount at zero: n=%d err=%v (want tombstoned 0)", n, err)
	}
	// Unlinking an absent link is a caller bug and must be loud; a tombstone
	// is equally absent (re-link after zero recreates a live counter).
	if err := s.UnlinkBlob(ctx, acct, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for absent link, got %v", err)
	}
	if err := s.UnlinkBlob(ctx, acct, "blob1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for tombstoned link, got %v", err)
	}
	if err := s.LinkBlob(ctx, acct, "blob1"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BlobRefCount(ctx, acct, "blob1"); err != nil || n != 1 {
		t.Fatalf("re-link after zero: n=%d err=%v", n, err)
	}
}

func runConcurrentAllocation(t *testing.T, s *Store) {
	ctx := context.Background()
	acct, _ := s.CreateAccount(ctx, "alice@example.com")

	const workers = 32
	const perWorker = 20
	var wg sync.WaitGroup
	ids := make(chan uint64, workers*perWorker)
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				id, err := s.CreateDocument(ctx, acct, CollectionEmail)
				if err != nil {
					errs <- err
					return
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	seen := map[uint64]bool{}
	var count int
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate document ID %d under concurrency", id)
		}
		seen[id] = true
		count++
	}
	if count != workers*perWorker {
		t.Fatalf("allocated %d IDs, want %d", count, workers*perWorker)
	}
}

// runDeliverBatch pins the micro-batch contract: state after one
// DeliverEmailBatch equals state after the same messages go through
// DeliverEmail one by one (uids, docs, modseqs, quota, changelog), batches
// span multiple mailboxes correctly, and concurrent batchers never duplicate
// identities.
func runDeliverBatch(t *testing.T, s *Store) {
	ctx := context.Background()
	acct, _ := s.CreateAccount(ctx, "alice@example.com")
	mb, err := s.CreateDocument(ctx, acct, CollectionMailbox)
	if err != nil {
		t.Fatal(err)
	}
	mb2, err := s.CreateDocument(ctx, acct, CollectionMailbox)
	if err != nil {
		t.Fatal(err)
	}
	fields := func(subject string) map[byte][]byte {
		return map[byte][]byte{99: []byte(subject)}
	}

	// Sequential baseline.
	var wantUID, wantModseq, wantDoc []uint64
	for i := 0; i < 3; i++ {
		uid, ms, doc, err := s.DeliverEmail(ctx, acct, mb, fields("x"), "b-seq", 100)
		if err != nil {
			t.Fatal(err)
		}
		wantUID = append(wantUID, uid)
		wantModseq = append(wantModseq, ms)
		wantDoc = append(wantDoc, doc)
	}

	// One batch of 3 in the same mailbox: identical allocations continuing
	// the sequence (as if the three had been delivered one by one).
	batch := []DeliverRequest{
		{MailboxID: mb, Fields: fields("a"), BlobID: "b1", Size: 100},
		{MailboxID: mb, Fields: fields("b"), BlobID: "b2", Size: 100},
		{MailboxID: mb, Fields: fields("c"), BlobID: "b3", Size: 100},
	}
	res, err := s.DeliverEmailBatch(ctx, acct, batch)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		if r.UID != wantUID[i]+3 || r.ModSeq != wantModseq[i]+3 || r.DocID != wantDoc[i]+3 {
			t.Fatalf("batch[%d] = uid %d modseq %d doc %d, want uid %d modseq %d doc %d",
				i, r.UID, r.ModSeq, r.DocID, wantUID[i]+3, wantModseq[i]+3, wantDoc[i]+3)
		}
	}

	// Counter state must equal six sequential deliveries.
	uidKey := CounterKey(uint32(acct), CounterKindNextDoc, append([]byte{CollectionEmail}, beUint64(mb)...))
	cur, err := readCounter(s.kv.Get, uidKey)
	if err != nil {
		t.Fatal(err)
	}
	if cur != 6 {
		t.Fatalf("uid counter = %d, want 6", cur)
	}
	quotaKey := QuotaKey(uint32(acct))
	qv, err := quotaValue(s.kv.Get, quotaKey)
	if err != nil {
		t.Fatal(err)
	}
	if qv != 600 {
		t.Fatalf("quota = %d, want 600", qv)
	}

	// Multi-mailbox batch: per-mailbox UID sequences are independent.
	res, err = s.DeliverEmailBatch(ctx, acct, []DeliverRequest{
		{MailboxID: mb2, Fields: fields("d"), BlobID: "b4", Size: 10},
		{MailboxID: mb, Fields: fields("e"), BlobID: "b5", Size: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].UID != 1 || res[1].UID != 7 || res[0].ModSeq != 1 {
		t.Fatalf("multi-mailbox batch = %+v", res)
	}

	// Concurrent batches must never duplicate or skip identities.
	const goroutines = 8
	const per = 5
	var wg sync.WaitGroup
	uids := make(chan uint64, goroutines*per)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reqs := make([]DeliverRequest, per)
			for i := range reqs {
				reqs[i] = DeliverRequest{MailboxID: mb, Fields: fields("r"), BlobID: "br", Size: 1}
			}
			out, err := s.DeliverEmailBatch(ctx, acct, reqs)
			if err != nil {
				t.Error(err)
				return
			}
			for _, r := range out {
				uids <- r.UID
			}
		}()
	}
	wg.Wait()
	close(uids)
	seen := map[uint64]bool{}
	for uid := range uids {
		if seen[uid] {
			t.Fatalf("duplicate UID %d under concurrent batches", uid)
		}
		seen[uid] = true
	}
	wantTotal := uint64(7 + goroutines*per) // 3 seq + 3 batch + multi(mb→7) + 40 raced
	cur, err = readCounter(s.kv.Get, uidKey)
	if err != nil {
		t.Fatal(err)
	}
	if cur != wantTotal {
		t.Fatalf("uid counter = %d, want %d", cur, wantTotal)
	}
}

// runStoreSuite executes the full invariant suite against one backend
// constructor. Every KV/Blob backend must pass the same suite (双后端对拍).
func runStoreSuite(t *testing.T, newStore func(t *testing.T) *Store) {
	t.Helper()
	sub := func(name string, fn func(t *testing.T, s *Store)) {
		t.Run(name, func(t *testing.T) {
			fn(t, newStore(t))
		})
	}
	sub("account lifecycle", runAccountLifecycle)
	sub("documents", runDocuments)
	sub("change log", runChangeLog)
	sub("quota accounting", runQuotaAccounting)
	sub("blob links", runBlobLinks)
	sub("concurrent allocation", runConcurrentAllocation)
	sub("deliver batch", runDeliverBatch)
}

func TestStoreMemory(t *testing.T) {
	runStoreSuite(t, func(*testing.T) *Store {
		return New(NewMemoryKV(), NewMemoryBlob())
	})
}

func TestStorePebble(t *testing.T) {
	runStoreSuite(t, func(t *testing.T) *Store {
		kv, err := OpenPebble(t.TempDir())
		if err != nil {
			t.Fatalf("open pebble: %v", err)
		}
		t.Cleanup(func() { _ = kv.Close() })
		return New(kv, NewMemoryBlob())
	})
}
