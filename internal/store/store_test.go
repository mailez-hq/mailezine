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
	if _, err := s.BlobRefCount(ctx, acct, "blob1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound at zero refs, got %v", err)
	}
	// Unlinking an absent link is a caller bug and must be loud.
	if err := s.UnlinkBlob(ctx, acct, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for absent link, got %v", err)
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
