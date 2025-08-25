// MemoryBlob is an in-process Blob used by tests and standalone dev mode.
package store

import (
	"context"
	"io"
	"sync"
)

// MemoryBlob implements Blob over an in-memory map.
type MemoryBlob struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemoryBlob returns an empty in-memory blob store.
func NewMemoryBlob() *MemoryBlob {
	return &MemoryBlob{m: make(map[string][]byte)}
}

func (b *MemoryBlob) Put(_ context.Context, id string, _ int64, r io.Reader) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	b.m[id] = data
	b.mu.Unlock()
	return int64(len(data)), nil
}

func (b *MemoryBlob) Get(_ context.Context, id string, w io.Writer) error {
	b.mu.RLock()
	data, ok := b.m[id]
	b.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	_, err := w.Write(data)
	return err
}

func (b *MemoryBlob) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.m[id]; !ok {
		return ErrNotFound
	}
	delete(b.m, id)
	return nil
}

func (b *MemoryBlob) Stat(_ context.Context, id string) (int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	data, ok := b.m[id]
	if !ok {
		return 0, ErrNotFound
	}
	return int64(len(data)), nil
}

var _ Blob = (*MemoryBlob)(nil)
