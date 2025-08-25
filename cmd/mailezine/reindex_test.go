//go:build unix

package main

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

func TestReindex(t *testing.T) {
	dir := t.TempDir()
	rocksPath := filepath.Join(dir, "rocks")
	ftsPath := rocksPath + ".fts"

	kv, err := store.OpenPebble(rocksPath)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.NewFSBlob(rocksPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	ms := mailstore.NewKV(store.New(kv, blob))
	ctx := context.Background()
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte("From: a@x.test\r\nSubject: quarterly report\r\n\r\nneedle in haystack"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Deliver(ctx, "bob@example.com", "INBOX", &mailstore.Message{
		Data: []byte("From: b@x.test\r\nSubject: other\r\n\r\nunrelated"),
	}); err != nil {
		t.Fatal(err)
	}
	_ = kv.Close()

	if code := runReindex([]string{"--storage", "pebble", "--rocks-path", rocksPath, "--fts-path", ftsPath}); code != 0 {
		t.Fatalf("reindex exit = %d", code)
	}

	ix, err := fts.Open(ftsPath, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	got, err := ix.SearchText(ctx, "alice@example.com", "INBOX", []string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("alice search = %v, want 1 hit", got)
	}
	if _, ok := got[1]; !ok {
		t.Fatalf("uid 1 missing: %v", got)
	}
	bob, err := ix.SearchText(ctx, "bob@example.com", "INBOX", []string{"unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bob) != 1 {
		t.Fatalf("bob search = %v, want 1 hit", bob)
	}
}
