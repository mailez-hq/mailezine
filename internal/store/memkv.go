// MemoryKV is an in-process KV used by tests and the standalone dev mode.
// It preserves ordering and atomic-batch semantics so protocol code can run
// against it before a real backend lands (PLAN.md §9.2 track B).
package store

import (
	"bytes"
	"context"
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

// WithTxn serializes the closure and its commit under the write lock: on an
// in-memory backend concurrent commits cannot interleave, so every
// transaction applies exactly what it read (serializable; replay is never
// needed). Serving it natively instead of through the AsTxn buffer adapter
// is what makes claim protocols (queue ownership, singleton leases) hold
// their multi-writer semantics in tests: read-modify-write races are
// resolved by the lock, not by last-write-wins.
func (k *MemoryKV) WithTxn(_ context.Context, fn func(t TxnOps) error) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := &memTxn{m: k.m}
	if err := fn(w); err != nil {
		return err
	}
	for _, op := range w.ops {
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

// memTxn stages mutations and reads directly from the (write-locked) map —
// it must never re-enter the public locking surface.
type memTxn struct {
	m   map[string][]byte
	idx map[string]int
	ops []Op
}

func (w *memTxn) Get(key []byte) ([]byte, error) {
	if i, ok := w.idx[string(key)]; ok {
		op := w.ops[i]
		if op.Delete {
			return nil, ErrNotFound
		}
		return append([]byte(nil), op.Value...), nil
	}
	v, ok := w.m[string(key)]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (w *memTxn) Put(key, val []byte) { w.stage(Op{Key: key, Value: val}) }

func (w *memTxn) Delete(key []byte) { w.stage(Op{Key: key, Delete: true}) }

func (w *memTxn) Append(ops ...Op) {
	for _, op := range ops {
		w.stage(op)
	}
}

func (w *memTxn) Scan(prefix []byte, fn func(k, v []byte) error) error {
	merged := make(map[string][]byte)
	for key, v := range w.m {
		if bytes.HasPrefix([]byte(key), prefix) {
			merged[key] = v
		}
	}
	for _, op := range w.ops {
		if !bytes.HasPrefix(op.Key, prefix) {
			continue
		}
		if op.Delete {
			delete(merged, string(op.Key))
			continue
		}
		merged[string(op.Key)] = op.Value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := fn([]byte(key), append([]byte(nil), merged[key]...)); err != nil {
			return err
		}
	}
	return nil
}

func (w *memTxn) stage(op Op) {
	k := string(op.Key)
	if i, ok := w.idx[k]; ok {
		w.ops[i] = op
		return
	}
	if w.idx == nil {
		w.idx = make(map[string]int)
	}
	w.idx[k] = len(w.ops)
	w.ops = append(w.ops, op)
}

var _ TxnKV = (*MemoryKV)(nil)
