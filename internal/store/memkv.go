// MemoryKV is an in-process KV used by tests and the standalone dev mode.
// It preserves ordering and atomic-batch semantics so protocol code can run
// against it before a real backend lands (PLAN.md §9.2 track B).
package store

import (
	"bytes"
	"sort"
	"sync"
)

// MemoryKV implements KV over an in-memory map.
type MemoryKV struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemoryKV returns an empty in-memory KV.
func NewMemoryKV() *MemoryKV {
	return &MemoryKV{m: make(map[string][]byte)}
}

func (k *MemoryKV) Get(key []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.m[string(key)]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (k *MemoryKV) Put(key, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	v := make([]byte, len(value))
	copy(v, value)
	k.m[string(key)] = v
	return nil
}

func (k *MemoryKV) Delete(key []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, string(key))
	return nil
}

func (k *MemoryKV) Scan(prefix []byte, fn func(k2, v []byte) error) error {
	k.mu.RLock()
	keys := make([]string, 0, len(k.m))
	for key := range k.m {
		if bytes.HasPrefix([]byte(key), prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	values := make([][]byte, len(keys))
	for i, key := range keys {
		values[i] = append([]byte(nil), k.m[key]...)
	}
	k.mu.RUnlock()

	for i, key := range keys {
		if err := fn([]byte(key), values[i]); err != nil {
			return err
		}
	}
	return nil
}

func (k *MemoryKV) Batch(ops []Op) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, op := range ops {
		if op.Delete {
			delete(k.m, string(op.Key))
			continue
		}
		v := make([]byte, len(op.Value))
		copy(v, op.Value)
		k.m[string(op.Key)] = v
	}
	return nil
}

func (k *MemoryKV) Close() error { return nil }
