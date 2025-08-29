package store

import (
	"bytes"
	"context"
	"testing"
)

// TestFSBlobCompressionRoundTrip: compressed blobs read back identical, and
// a legacy uncompressed blob is still readable.
func TestFSBlobCompressionRoundTrip(t *testing.T) {
	root := t.TempDir()
	comp, err := NewFSBlobCompressed(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("quarterly report text "), 100)
	n, err := comp.Put(ctx, "email-1-1", int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("put returned %d, want %d (logical size)", n, len(payload))
	}
	// On disk it must be smaller (gzip).
	sz, err := comp.Stat(ctx, "email-1-1")
	if err != nil {
		t.Fatal(err)
	}
	if sz >= int64(len(payload)) {
		t.Fatalf("compressed size %d not smaller than %d", sz, len(payload))
	}
	var got bytes.Buffer
	if err := comp.Get(ctx, "email-1-1", &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatal("round trip mismatch")
	}

	// Legacy uncompressed blob in the same directory stays readable.
	plain, err := NewFSBlob(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Put(ctx, "email-old-1", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	got.Reset()
	if err := comp.Get(ctx, "email-old-1", &got); err != nil {
		t.Fatalf("legacy blob unreadable: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatal("legacy round trip mismatch")
	}
}
