package mailbuffer

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestMemoryBuffer(t *testing.T) {
	in := []byte("small message")
	buf, err := NewFromReader(bytes.NewReader(in), 1024, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Remove()
	if buf.Len() != int64(len(in)) {
		t.Fatalf("Len = %d, want %d", buf.Len(), len(in))
	}
	got, err := buf.ReadAll()
	if err != nil || !bytes.Equal(got, in) {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
	// Multiple opens.
	for i := 0; i < 2; i++ {
		r, err := buf.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(data, in) {
			t.Fatalf("open %d = %q, %v", i, data, err)
		}
	}
	if buf.Remove() != nil {
		t.Fatal("memory remove should be a no-op")
	}
}

func TestFileBufferSpill(t *testing.T) {
	in := strings.Repeat("x", 4096)
	buf, err := NewFromReader(strings.NewReader(in), 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Remove()
	if buf.Len() != int64(len(in)) {
		t.Fatalf("Len = %d, want %d", buf.Len(), len(in))
	}
	got, err := buf.ReadAll()
	if err != nil || string(got) != in {
		t.Fatalf("ReadAll mismatch: %d bytes, %v", len(got), err)
	}
	// Streaming read works too.
	r, err := buf.Open()
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r)
	r.Close()
	if err != nil || n != int64(len(in)) {
		t.Fatalf("stream = %d, %v", n, err)
	}
}

func TestLimitExceeded(t *testing.T) {
	_, err := NewFromReader(strings.NewReader(strings.Repeat("y", 100)), 50, 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// Exceeding the limit after the memory threshold.
	_, err = NewFromReader(strings.NewReader(strings.Repeat("z", 3000)), 2000, 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("spill err = %v, want ErrTooLarge", err)
	}
}

func TestRemoveFile(t *testing.T) {
	buf, err := NewFromReader(strings.NewReader(strings.Repeat("w", 4096)), 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := buf.(*fileBuffer)
	if !ok {
		t.Fatal("expected file buffer")
	}
	if err := buf.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := buf.Remove(); err != nil {
		t.Fatalf("second remove should be a no-op: %v", err)
	}
	if _, err := os.Stat(fb.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file still present: %v", err)
	}
}
