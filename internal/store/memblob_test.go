package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMemoryBlob(t *testing.T) {
	b := NewMemoryBlob()
	ctx := context.Background()
	n, err := b.Put(ctx, "msg1", 5, bytes.NewReader([]byte("hello")))
	if err != nil || n != 5 {
		t.Fatalf("put: n=%d err=%v", n, err)
	}
	var buf bytes.Buffer
	if err := b.Get(ctx, "msg1", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello" {
		t.Fatalf("get: %q", buf.String())
	}
	if sz, err := b.Stat(ctx, "msg1"); err != nil || sz != 5 {
		t.Fatalf("stat: sz=%d err=%v", sz, err)
	}
	if err := b.Delete(ctx, "msg1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Get(ctx, "msg1", &buf); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
