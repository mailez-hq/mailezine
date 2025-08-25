// Crash-consistency matrix (ARCHITECTURE.md §11): a child process writes
// through the store and dies without a graceful close (kill -9
// equivalent); the parent reopens the database and verifies the invariants
// survive WAL recovery (INV-UID, INV-CHANGE, INV-QUOTA, INV-BLOB).
package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCrashConsistency(t *testing.T) {
	runCrashConsistency(t)
}

// runCrashConsistency spawns a child that writes and dies without closing,
// then reopens and verifies every invariant. Shared by the Pebble and
// RocksDB variants.
func runCrashConsistency(t *testing.T) {
	if os.Getenv("MAILEZINE_CRASH_CHILD") == "1" {
		crashChildWrite()
		os.Exit(1) // simulate a hard crash: no Close, no flush of buffers
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashConsistency$")
	cmd.Env = append(os.Environ(),
		"MAILEZINE_CRASH_CHILD=1",
		"MAILEZINE_CRASH_DIR="+dbPath,
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("child process unexpectedly exited cleanly")
	}

	// Reopen and verify every invariant.
	kv, err := crashOpen(dbPath)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer kv.Close()
	blob, err := NewFSBlob(dbPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	s := New(kv, blob)
	ctx := context.Background()

	acct, err := s.AccountByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("account lost after crash: %v", err)
	}
	email, err := s.AccountEmail(ctx, acct)
	if err != nil || email != "alice@example.com" {
		t.Fatalf("account email: %q err=%v", email, err)
	}

	// INV-UID: doc IDs are exactly 1..3, monotonic, no reuse.
	ids, err := s.ListDocumentIDs(ctx, acct, CollectionEmail)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("doc ids after crash: %v", ids)
	}

	// INV-CHANGE: gapless, ordered change log.
	changes, err := s.ChangesSince(ctx, acct, CollectionEmail, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes after crash: %d", len(changes))
	}
	for i, c := range changes {
		if c.ChangeID != uint64(i+1) || c.Op != OpCreate {
			t.Fatalf("change %d: %+v", i, c)
		}
	}

	// INV-QUOTA: used bytes equals the sum of stored message sizes.
	quota, err := s.QuotaUsed(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, id := range ids {
		fields, err := s.GetDocumentFields(ctx, acct, CollectionEmail, id)
		if err != nil {
			t.Fatal(err)
		}
		want += int64(binary.BigEndian.Uint64(fields[EmailFieldSize]))
	}
	if quota != want {
		t.Fatalf("quota after crash = %d, want %d", quota, want)
	}

	// INV-BLOB: every blob has exactly one reference.
	for _, id := range ids {
		fields, _ := s.GetDocumentFields(ctx, acct, CollectionEmail, id)
		refs, err := s.BlobRefCount(ctx, acct, string(fields[EmailFieldBlob]))
		if err != nil || refs != 1 {
			t.Fatalf("blob refs for doc %d = %d err=%v", id, refs, err)
		}
		var buf bytes.Buffer
		if err := s.GetBlob(ctx, string(fields[EmailFieldBlob]), &buf); err != nil {
			t.Fatalf("blob content lost after crash (doc %d): %v", id, err)
		}
		if int64(buf.Len()) != wantSize(id) {
			t.Fatalf("blob size for doc %d = %d, want %d", id, buf.Len(), wantSize(id))
		}
	}

	// A new append after recovery must continue the sequence (no UID reuse).
	next, err := s.CreateDocument(ctx, acct, CollectionEmail)
	if err != nil {
		t.Fatal(err)
	}
	if next != 4 {
		t.Fatalf("post-crash allocation = %d, want 4", next)
	}
}

func wantSize(docID uint64) int64 {
	return int64(100 + docID - 1)
}

// crashChildWrite performs the same write sequence the parent verifies, with
// fsync-committed writes only (no graceful close).
func crashChildWrite() {
	dir := os.Getenv("MAILEZINE_CRASH_DIR")
	kv, err := crashOpen(dir)
	if err != nil {
		os.Exit(2)
	}
	blob, err := NewFSBlob(dir + ".blobs")
	if err != nil {
		os.Exit(2)
	}
	s := New(kv, blob)
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com")
	if err != nil {
		os.Exit(2)
	}
	for i := 0; i < 3; i++ {
		docID, err := s.CreateDocument(ctx, acct, CollectionEmail)
		if err != nil {
			os.Exit(2)
		}
		blobID := "email-" + string(rune('a'+i))
		size := int64(100 + i)
		if _, err := s.PutBlob(ctx, blobID, int64(size), bytes.NewReader(make([]byte, size))); err != nil {
			os.Exit(2)
		}
		if err := s.AppendEmailAtomically(ctx, acct, CollectionEmail, docID, map[byte][]byte{
			EmailFieldBlob: []byte(blobID),
			EmailFieldSize: beUint64(uint64(size)),
		}, blobID, size); err != nil {
			os.Exit(2)
		}
	}
	// Intentionally no kv.Close(): the process "dies" here.
}
