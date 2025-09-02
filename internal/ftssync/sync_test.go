package ftssync

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"mailezine/internal/fts"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// newCluster builds a mailstore over shared in-memory KV+blob plus one
// node-local FTS index (the multi-active shape: N indexes, one store).
func newNode(t *testing.T, kv store.KV, blob store.Blob) (*mailstore.KV, *fts.Indexer, string) {
	t.Helper()
	idxPath := t.TempDir()
	ix, err := fts.Open(idxPath, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open fts: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return mailstore.NewKV(store.New(kv, blob)), ix, idxPath
}

func newWorker(t *testing.T, kv store.KV, blob store.Blob, ix *fts.Indexer, stateDir string) *Worker {
	t.Helper()
	return &Worker{
		Store:     store.New(kv, blob),
		Index:     ix,
		StatePath: filepath.Join(stateDir, "sync.json"),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func deliver(t *testing.T, ms *mailstore.KV, account, mailbox, subject string) uint32 {
	t.Helper()
	uid, err := ms.Deliver(context.Background(), account, mailbox, &mailstore.Message{
		From: "sender@example.com",
		Data: []byte("From: sender@example.com\r\nSubject: " + subject + "\r\n\r\nbody of " + subject + "\r\n"),
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return uid
}

func hit(t *testing.T, ix *fts.Indexer, account, mailbox, term string) bool {
	t.Helper()
	uids, err := ix.SearchText(context.Background(), account, mailbox, []string{term})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return len(uids) > 0
}

// The delivering node and a second node both converge: the tailer is the
// only indexer in multi-active, so mail delivered "elsewhere" (here: any
// write into the shared store) becomes searchable on every node.
func TestTailConvergesEveryNode(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()

	nodeA, idxA, stateA := newNode(t, kv, blob)
	nodeB, idxB, stateB := newNode(t, kv, blob)

	deliver(t, nodeA, "user@example.com", "INBOX", "quarterly report")
	deliver(t, nodeB, "user@example.com", "INBOX", "birthday party")

	wa := newWorker(t, kv, blob, idxA, stateA)
	wb := newWorker(t, kv, blob, idxB, stateB)
	for _, w := range []*Worker{wa, wb} {
		if _, err := w.SyncOnce(context.Background()); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}

	for _, w := range []struct {
		name string
		ix   *fts.Indexer
	}{{"A", idxA}, {"B", idxB}} {
		if !hit(t, w.ix, "user@example.com", "INBOX", "quarterly") {
			t.Fatalf("node %s: delivered-on-A mail not searchable", w.name)
		}
		if !hit(t, w.ix, "user@example.com", "INBOX", "birthday") {
			t.Fatalf("node %s: delivered-on-B mail not searchable", w.name)
		}
	}
}

// Expunges remove the exact copy on every node: the extended delete entry
// carries (mailbox, UID) past the vanished KV row.
func TestTailRemovesExpunged(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	ms, ix, state := newNode(t, kv, blob)

	keep := deliver(t, ms, "user@example.com", "INBOX", "important contract")
	gone := deliver(t, ms, "user@example.com", "INBOX", "spam offer")

	w := newWorker(t, kv, blob, ix, state)
	if _, err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetFlags(context.Background(), "user@example.com", "INBOX", gone, []string{"\\Deleted"}); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	if _, err := ms.Expunge(context.Background(), "user@example.com", "INBOX", []uint32{gone}); err != nil {
		t.Fatalf("expunge: %v", err)
	}

	if _, err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if hit(t, ix, "user@example.com", "INBOX", "spam") {
		t.Fatal("expunged message still searchable")
	}
	if !hit(t, ix, "user@example.com", "INBOX", "contract") {
		t.Fatal("kept message lost from index")
	}
	if keep == gone {
		t.Fatal("setup: uids collided")
	}
}

// Watermarks persist across worker restarts: the second pass applies
// nothing new.
func TestWatermarkPersists(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	ms, ix, state := newNode(t, kv, blob)

	deliver(t, ms, "user@example.com", "INBOX", "hello world")
	w := newWorker(t, kv, blob, ix, state)
	n, err := w.SyncOnce(context.Background())
	if err != nil || n == 0 {
		t.Fatalf("first pass: n=%d err=%v", n, err)
	}

	// Fresh worker instance, same state file: nothing left to apply.
	w2 := newWorker(t, kv, blob, ix, state)
	n2, err := w2.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second pass re-applied %d changes", n2)
	}

	// New mail after a "restart" is picked up incrementally.
	deliver(t, ms, "user@example.com", "INBOX", "second message")
	n3, err := w2.SyncOnce(context.Background())
	if err != nil || n3 != 1 {
		t.Fatalf("incremental pass: n=%d err=%v", n3, err)
	}
	if !hit(t, ix, "user@example.com", "INBOX", "second") {
		t.Fatal("incremental mail not searchable")
	}
}

// Flag updates (change-log OpUpdate) re-index without duplicating or
// losing the entry.
func TestFlagUpdateReindexes(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	ms, ix, state := newNode(t, kv, blob)

	uid := deliver(t, ms, "user@example.com", "Archive", "archive me")
	w := newWorker(t, kv, blob, ix, state)
	if _, err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetFlags(context.Background(), "user@example.com", "Archive", uid, []string{"\\Seen"}); err != nil {
		t.Fatalf("setflags: %v", err)
	}
	if _, err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hit(t, ix, "user@example.com", "Archive", "archive") {
		t.Fatal("message lost from index after flag update")
	}
}

// A deleted account drops out of the watermarks without stalling the pass.
func TestDeletedAccountSkips(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	ms, ix, state := newNode(t, kv, blob)
	w := newWorker(t, kv, blob, ix, state)

	deliver(t, ms, "gone@example.com", "INBOX", "old mail")
	if _, err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ms.DeleteAccount(context.Background(), "gone@example.com"); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := w.SyncOnce(context.Background()); err != nil {
		t.Fatalf("pass after account deletion must not fail: %v", err)
	}
}

// Regression: a deleted-and-recreated same-address account restarts its
// change log at 1. The watermark must detect the account-ID change and
// replay from zero — carrying the old high-water mark forward silently
// skipped the new account's first N changes (its mail stayed unsearchable
// until the IDs caught up).
func TestRecreatedAccountReplaysFromZero(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	ms, ix, state := newNode(t, kv, blob)
	w := newWorker(t, kv, blob, ix, state)
	ctx := context.Background()

	// Old account accumulates a high watermark.
	for i := 0; i < 5; i++ {
		deliver(t, ms, "user@example.com", "INBOX", "old mail")
	}
	if _, err := w.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Delete + recreate the same address; the fresh account's change IDs
	// restart at 1 — far below the stale watermark.
	if err := ms.DeleteAccount(ctx, "user@example.com"); err != nil {
		t.Fatal(err)
	}
	deliver(t, ms, "user@example.com", "INBOX", "fresh start")

	if _, err := w.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !hit(t, ix, "user@example.com", "INBOX", "fresh") {
		t.Fatal("re-created account's mail not indexed (stale watermark swallowed the fresh change log)")
	}
}

// The extended delete encoding round-trips through ChangesSince.
func TestExtendedDeleteEncoding(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	facade := store.New(kv, blob)
	ms := mailstore.NewKV(facade)
	ctx := context.Background()

	uid := deliver(t, ms, "user@example.com", "Sent", "delete me")
	if err := ms.SetFlags(ctx, "user@example.com", "Sent", uid, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(ctx, "user@example.com", "Sent", []uint32{uid}); err != nil {
		t.Fatal(err)
	}
	acctID, err := facade.AccountByEmail(ctx, "user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := facade.ChangesSince(ctx, acctID, store.CollectionEmail, 0)
	if err != nil {
		t.Fatal(err)
	}
	var del *store.Change
	for i := range changes {
		if changes[i].Op == store.OpDelete {
			del = &changes[i]
		}
	}
	if del == nil {
		t.Fatal("no delete change recorded")
	}
	if del.Mailbox != "Sent" || del.UID != uid {
		t.Fatalf("extended delete = (%q, %d), want (Sent, %d)", del.Mailbox, del.UID, uid)
	}
}
