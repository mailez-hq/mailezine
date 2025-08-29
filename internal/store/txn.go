package store

import (
	"bytes"
	"context"
	"sort"
)

// AsTxn upgrades a plain KV to the transactional TxnKV contract.
//
// Backends that implement TxnKV natively (e.g. TiDB, where conflicts and
// atomicity are enforced by the database) are returned unchanged. Anything
// else gets a write-buffer adapter: the closure stages mutations in memory
// and they are committed with one atomic kv.Batch on success — bit-for-bit
// the behaviour the Store relied on before the transactional refactor, with
// correctness pinned to the caller-side serialization (sharded account
// locks). That makes single-node deployments strictly backwards compatible;
// distributed serialization arrives only with a native backend.
func AsTxn(kv KV) TxnKV {
	if t, ok := kv.(TxnKV); ok {
		return t
	}
	return &bufTxn{base: kv}
}

// bufTxn forwards the classic KV surface to the base engine while serving
// WithTxn through a per-call writeBuf.
type bufTxn struct{ base KV }

func (b *bufTxn) Get(key []byte) ([]byte, error) { return b.base.Get(key) }
func (b *bufTxn) Put(key, value []byte) error    { return b.base.Put(key, value) }
func (b *bufTxn) Delete(key []byte) error        { return b.base.Delete(key) }
func (b *bufTxn) Scan(prefix []byte, fn func(k, v []byte) error) error {
	return b.base.Scan(prefix, fn)
}
func (b *bufTxn) Batch(ops []Op) error { return b.base.Batch(ops) }
func (b *bufTxn) Close() error         { return b.base.Close() }

// WithTxn runs fn exactly once and commits whatever it staged. There is no
// retry: buffer-mode conflict resolution is the process-local locks held by
// Store, which serialize every conflicting writer before WithTxn is entered.
func (b *bufTxn) WithTxn(_ context.Context, fn func(t TxnOps) error) error {
	w := &writeBuf{base: b.base, idx: make(map[string]int)}
	if err := fn(w); err != nil {
		return err
	}
	return b.base.Batch(w.ops)
}

// writeBuf accumulates staged mutations with last-write-wins key collapsing
// so redundant rewrites inside one closure collapse into a single stored op.
// Get serves read-your-writes from the buffer before falling through.
type writeBuf struct {
	base KV
	idx  map[string]int
	ops  []Op
}

func (w *writeBuf) Get(key []byte) ([]byte, error) {
	if i, ok := w.idx[string(key)]; ok {
		op := w.ops[i]
		if op.Delete {
			return nil, ErrNotFound
		}
		return op.Value, nil
	}
	return w.base.Get(key)
}

func (w *writeBuf) Put(key, val []byte) { w.stage(Op{Key: key, Value: val}) }

func (w *writeBuf) Delete(key []byte) { w.stage(Op{Key: key, Delete: true}) }

func (w *writeBuf) Append(ops ...Op) {
	for _, op := range ops {
		w.stage(op)
	}
}

// Scan merges the base prefix range with the staged writes and visits the
// union in ascending key order (read-your-writes semantics).
func (w *writeBuf) Scan(prefix []byte, fn func(k, v []byte) error) error {
	type entry struct{ k, v []byte }
	var merged []entry
	staged := make(map[string]bool)
	for _, op := range w.ops {
		if !bytes.HasPrefix(op.Key, prefix) {
			continue
		}
		staged[string(op.Key)] = true
		if !op.Delete {
			merged = append(merged, entry{k: append([]byte(nil), op.Key...), v: append([]byte(nil), op.Value...)})
		}
	}
	if err := w.base.Scan(prefix, func(k, v []byte) error {
		if staged[string(k)] {
			return nil
		}
		merged = append(merged, entry{k: append([]byte(nil), k...), v: append([]byte(nil), v...)})
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(merged, func(i, j int) bool { return bytes.Compare(merged[i].k, merged[j].k) < 0 })
	for _, e := range merged {
		if err := fn(e.k, e.v); err != nil {
			return err
		}
	}
	return nil
}

func (w *writeBuf) stage(op Op) {
	k := string(op.Key)
	if i, ok := w.idx[k]; ok {
		w.ops[i] = op
		return
	}
	w.idx[k] = len(w.ops)
	w.ops = append(w.ops, op)
}

var _ TxnOps = (*writeBuf)(nil)
