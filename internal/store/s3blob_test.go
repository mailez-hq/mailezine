package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"mailezine/internal/store/s3test"
)

func newTestS3Blob(t *testing.T) *S3Blob {
	t.Helper()
	endpoint, _, cleanup := s3test.New(t)
	t.Cleanup(cleanup)
	b, err := NewS3Blob(endpoint, "test", "test", "blobs", false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestS3Blob(t *testing.T) {
	b := newTestS3Blob(t)
	ctx := context.Background()
	if err := b.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}

	n, err := b.Put(ctx, "abc123", 5, bytes.NewReader([]byte("hello")))
	if err != nil || n != 5 {
		t.Fatalf("put: n=%d err=%v", n, err)
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
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := b.Get(ctx, "abc123", &buf); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on get, got %v", err)
	}
}

func TestS3BlobRejectsUnsafeIDs(t *testing.T) {
	b := newTestS3Blob(t)
	ctx := context.Background()
	for _, id := range []string{"../escape", "a/b", "a b", ""} {
		if _, err := b.Put(ctx, id, 0, bytes.NewReader(nil)); err == nil {
			t.Fatalf("expected rejection for id %q", id)
		}
	}
}
