// Package mailbuffer provides tiered temporary storage for message bytes:
// small messages stay in memory, larger ones spill to a temporary file so a
// burst of big submissions does not multiply peak memory by concurrency.
// Buffers are immutable and may be opened multiple times.
package mailbuffer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrTooLarge is returned when the input exceeds the configured limit.
var ErrTooLarge = errors.New("mailbuffer: input exceeds limit")

// Buffer is an immutable byte store that can be read multiple times.
type Buffer interface {
	// Open returns a fresh reader over the stored bytes.
	Open() (io.ReadCloser, error)
	// Len returns the stored byte count.
	Len() int64
	// ReadAll returns the stored bytes (memory copy for file-backed
	// buffers; prefer Open + streaming for large messages).
	ReadAll() ([]byte, error)
	// Remove releases temporary storage. Idempotent.
	Remove() error
}

// memoryBuffer keeps small messages in RAM.
type memoryBuffer struct {
	data []byte
}

func (b *memoryBuffer) Open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

func (b *memoryBuffer) Len() int64 { return int64(len(b.data)) }

func (b *memoryBuffer) ReadAll() ([]byte, error) {
	return append([]byte(nil), b.data...), nil
}

func (b *memoryBuffer) Remove() error { return nil }

// fileBuffer keeps large messages in a temporary file.
type fileBuffer struct {
	path string
	size int64
}

func (b *fileBuffer) Open() (io.ReadCloser, error) {
	return os.Open(b.path)
}

func (b *fileBuffer) Len() int64 { return b.size }

func (b *fileBuffer) ReadAll() ([]byte, error) {
	data, err := os.ReadFile(b.path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (b *fileBuffer) Remove() error {
	if b.path == "" {
		return nil
	}
	err := os.Remove(b.path)
	b.path = ""
	return err
}

// FromFile adopts an already-written temporary file as a file buffer. The
// file is closed on success; the caller must call Remove to delete it.
func FromFile(f *os.File) (Buffer, error) {
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mailbuffer: stat: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("mailbuffer: close: %w", err)
	}
	return &fileBuffer{path: f.Name(), size: info.Size()}, nil
}

// NewFromReader reads r into a memory buffer when its size stays within
// memThreshold bytes, otherwise spills to a temporary file. limit bounds the
// total accepted size; exceeding it returns ErrTooLarge after consuming r up
// to limit (the caller must not reuse r).
func NewFromReader(r io.Reader, limit, memThreshold int64) (Buffer, error) {
	if limit <= 0 {
		limit = 50 << 20
	}
	if memThreshold <= 0 || memThreshold > limit {
		memThreshold = 1 << 20
	}
	if memThreshold > limit {
		memThreshold = limit
	}

	var mem bytes.Buffer
	_, err := io.CopyN(&mem, r, memThreshold)
	if err == io.EOF {
		// Input ended within the memory threshold.
		return &memoryBuffer{data: mem.Bytes()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mailbuffer: read: %w", err)
	}
	// Exactly memThreshold bytes were read and the input may continue:
	// spill the prefix and stream the rest to a temporary file.
	f, err := os.CreateTemp("", "mailezine-msg-*")
	if err != nil {
		return nil, fmt.Errorf("mailbuffer: temp file: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if _, err := f.Write(mem.Bytes()); err != nil {
		cleanup()
		return nil, fmt.Errorf("mailbuffer: write: %w", err)
	}
	remaining := limit - memThreshold
	written, err := io.Copy(f, io.LimitReader(r, remaining))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("mailbuffer: spill: %w", err)
	}
	// Detect overflow: one more byte beyond the limit means ErrTooLarge.
	// Reader-level errors (e.g. a limited DATA reader reporting its cap) are
	// propagated so the caller can map them precisely.
	var probe [1]byte
	m, perr := r.Read(probe[:])
	if m > 0 {
		cleanup()
		return nil, ErrTooLarge
	}
	if perr != nil && !errors.Is(perr, io.EOF) {
		cleanup()
		return nil, fmt.Errorf("mailbuffer: read: %w", perr)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("mailbuffer: close: %w", err)
	}
	return &fileBuffer{path: f.Name(), size: memThreshold + written}, nil
}
