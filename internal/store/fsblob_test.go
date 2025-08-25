package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFSBlob(t *testing.T) {
	root := t.TempDir()
	b, err := NewFSBlob(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	n, err := b.Put(ctx, "abc123", 5, bytes.NewReader([]byte("hello")))
	if err != nil || n != 5 {
		t.Fatalf("put: n=%d err=%v", n, err)
	}
	// Idempotent content-addressed put returns the stored size.
	n, err = b.Put(ctx, "abc123", 5, bytes.NewReader([]byte("hello")))
	if err != nil || n != 5 {
		t.Fatalf("re-put: n=%d err=%v", n, err)
	}
	var buf bytes.Buffer
	if err := b.Get(ctx, "abc123", &buf); err != nil || buf.String() != "hello" {
		t.Fatalf("get: %q err=%v", buf.String(), err)
	}
	if sz, err := b.Stat(ctx, "abc123"); err != nil || sz != 5 {
		t.Fatalf("stat: sz=%d err=%v", sz, err)
	}
	if err := b.Delete(ctx, "abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, "abc123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := b.Delete(ctx, "abc123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on double delete, got %v", err)
	}
}

func TestFSBlobRejectsUnsafeIDs(t *testing.T) {
	b, err := NewFSBlob(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"../escape", "a/b", `a\b`, "a b", "", "..", ".", strings.Repeat("x", 129)} {
		if _, err := b.Put(ctx, id, 0, bytes.NewReader(nil)); err == nil {
			t.Fatalf("expected rejection for id %q", id)
		}
	}
}
