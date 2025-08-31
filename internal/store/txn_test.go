// Byte-level and transactional contract tests (契约对拍): MemoryKV, PebbleKV
// and (env-gated) TiDBKV must all satisfy the same KV invariants, the same
// TxnOps semantics and the same Store-level concurrency guarantees.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func ctUint(t TxnOps, key string) (uint64, error) {
	v, err := t.Get([]byte(key))
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(v), nil
}

func ctBytes(u uint64) []byte { return beUint64(u) }

// runKVContract exercises the plain KV surface every backend must honour.
func runKVContract(t *testing.T, newKV func(*testing.T) KV) {
	t.Helper()
	sub := func(name string, fn func(*testing.T, KV)) {
		t.Run(name, func(t *testing.T) { fn(t, newKV(t)) })
	}

	sub("get put delete", func(t *testing.T, kv KV) {
		if _, err := kv.Get([]byte("k1")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing key: want ErrNotFound, got %v", err)
		}
		if err := kv.Put([]byte("k1"), []byte("v1")); err != nil {
			t.Fatal(err)
		}
		if v, err := kv.Get([]byte("k1")); err != nil || string(v) != "v1" {
			t.Fatalf("get: v=%q err=%v", v, err)
		}
		if err := kv.Put([]byte("k1"), []byte("v2")); err != nil {
			t.Fatal(err)
		}
		if v, _ := kv.Get([]byte("k1")); string(v) != "v2" {
			t.Fatalf("overwrite: got %q want v2", v)
		}
		if err := kv.Delete([]byte("k1")); err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Get([]byte("k1")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted key: got %v", err)
		}
		if err := kv.Delete([]byte("never-existed")); err != nil {
			t.Fatalf("delete absent key: %v", err)
		}
	})

	sub("scan order and prefix bound", func(t *testing.T, kv KV) {
		fixtures := [][2]string{{"p/b", "2"}, {"p/a", "1"}, {"x", "9"}, {"p/c", "3"}}
		for _, f := range fixtures {
			if err := kv.Put([]byte(f[0]), []byte(f[1])); err != nil {
				t.Fatal(err)
			}
		}
		var seen []string
		if err := kv.Scan([]byte("p/"), func(k, v []byte) error {
			seen = append(seen, fmt.Sprintf("%s=%s", k, v))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		want := []string{"p/a=1", "p/b=2", "p/c=3"}
		if len(seen) != len(want) {
			t.Fatalf("scan saw %v, want %v", seen, want)
		}
		for i := range want {
			if seen[i] != want[i] {
				t.Fatalf("scan row %d = %q, want %q (all: %v)", i, seen[i], want[i], seen)
			}
		}
	})

	sub("batch applies mixed ops", func(t *testing.T, kv KV) {
		if err := kv.Put([]byte("gone"), []byte("old")); err != nil {
			t.Fatal(err)
		}
		err := kv.Batch([]Op{
			{Key: []byte("gone"), Delete: true},
			{Key: []byte("n1"), Value: []byte("a")},
			{Key: []byte("n2"), Value: []byte("b")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Get([]byte("gone")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("batched delete: got %v", err)
		}
		for _, f := range [][2]string{{"n1", "a"}, {"n2", "b"}} {
			v, err := kv.Get([]byte(f[0]))
			if err != nil || string(v) != f[1] {
				t.Fatalf("batched put %s: v=%q err=%v", f[0], v, err)
			}
		}
	})

	// Present-but-empty values (index marker entries carry Value nil) must
	// survive every backend: SQL-backed stores bind a nil []byte as NULL,
	// which a NOT NULL value column rejects.
	sub("nil value round-trips as empty", func(t *testing.T, kv KV) {
		if err := kv.Put([]byte("marker"), nil); err != nil {
			t.Fatalf("put nil value: %v", err)
		}
		v, err := kv.Get([]byte("marker"))
		if err != nil || len(v) != 0 {
			t.Fatalf("get nil-valued key: v=%q err=%v", v, err)
		}
		if err := kv.Batch([]Op{{Key: []byte("marker2"), Value: nil}}); err != nil {
			t.Fatalf("batch nil value: %v", err)
		}
		if _, err := kv.Get([]byte("marker2")); err != nil {
			t.Fatalf("batched nil-valued key: %v", err)
		}
	})
}

// runTxnContract exercises the TxnOps semantics AsTxn promises on every
// backend: native SQL transactions and the buffer-mode adapter alike.
func runTxnContract(t *testing.T, newKV func(*testing.T) KV) {
	t.Helper()
	run := func(name string, body func(*testing.T, TxnKV)) {
		t.Run(name, func(t *testing.T) {
			body(t, AsTxn(newKV(t)))
		})
	}
	ctx := context.Background()

	run("read-your-writes inside closure", func(t *testing.T, txn TxnKV) {
		if err := txn.Put([]byte("c"), ctBytes(10)); err != nil {
			t.Fatal(err)
		}
		err := txn.WithTxn(ctx, func(h TxnOps) error {
			cur, err := ctUint(h, "c")
			if err != nil {
				return err
			}
			h.Put([]byte("c"), ctBytes(cur+1))
			next, err := ctUint(h, "c")
			if err != nil {
				return err
			}
			if next != 11 {
				return fmt.Errorf("staged read sees %d, want 11", next)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		final, gerr := txn.Get([]byte("c"))
		if gerr != nil || len(final) != 8 || binary.BigEndian.Uint64(final) != 11 {
			t.Fatalf("after commit: v=%x err=%v", final, gerr)
		}
	})

	run("last write wins on repeated key", func(t *testing.T, txn TxnKV) {
		err := txn.WithTxn(ctx, func(h TxnOps) error {
			h.Put([]byte("x"), []byte("one"))
			h.Put([]byte("x"), []byte("two"))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := txn.Get([]byte("x")); string(v) != "two" {
			t.Fatalf("collapsed value %q, want two", v)
		}
	})

	run("staged delete overrides staged put", func(t *testing.T, txn TxnKV) {
		err := txn.WithTxn(ctx, func(h TxnOps) error {
			h.Put([]byte("y"), []byte("temp"))
			h.Delete([]byte("y"))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Get([]byte("y")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("put+delete collapsed: got %v", err)
		}
	})

	run("append stages prebuilt ops", func(t *testing.T, txn TxnKV) {
		if err := txn.Put([]byte("old"), []byte("v")); err != nil {
			t.Fatal(err)
		}
		err := txn.WithTxn(ctx, func(h TxnOps) error {
			h.Append(
				Op{Key: []byte("old"), Delete: true},
				Op{Key: []byte("new"), Value: []byte("n")},
			)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := txn.Get([]byte("old")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("appended delete: got %v", err)
		}
		if v, _ := txn.Get([]byte("new")); string(v) != "n" {
			t.Fatalf("appended put: %q", v)
		}
	})

	run("scan merges staged writes with base range", func(t *testing.T, txn TxnKV) {
		for _, f := range [][2]string{{"p/a", "1"}, {"p/b", "2"}, {"p/c", "3"}, {"q/x", "9"}} {
			if err := txn.Put([]byte(f[0]), []byte(f[1])); err != nil {
				t.Fatal(err)
			}
		}
		var seen []string
		err := txn.WithTxn(ctx, func(h TxnOps) error {
			h.Put([]byte("p/a"), []byte("1m")) // overwrite base
			h.Delete([]byte("p/b"))            // hide base
			h.Put([]byte("p/d"), []byte("4"))  // extend inside prefix
			h.Put([]byte("q/y"), []byte("yy")) // outside prefix: ignored
			return h.Scan([]byte("p/"), func(k, v []byte) error {
				seen = append(seen, fmt.Sprintf("%s=%s", k, v))
				return nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"p/a=1m", "p/c=3", "p/d=4"}
		if len(seen) != len(want) {
			t.Fatalf("txn scan saw %v, want %v", seen, want)
		}
		for i := range want {
			if seen[i] != want[i] {
				t.Fatalf("txn scan row %d = %q, want %q (all: %v)", i, seen[i], want[i], seen)
			}
		}
		// Visitor errors abort the scan and propagate.
		sentinel := errors.New("stop")
		err = txn.WithTxn(ctx, func(h TxnOps) error {
			return h.Scan([]byte("p/"), func(k, v []byte) error { return sentinel })
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("scan error propagation: got %v", err)
		}
	})

	run("closure error rolls back", func(t *testing.T, txn TxnKV) {
		if err := txn.Put([]byte("keep"), []byte("orig")); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		err := txn.WithTxn(ctx, func(h TxnOps) error {
			h.Put([]byte("keep"), []byte("changed"))
			h.Put([]byte("spill"), []byte("x"))
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("want boom, got %v", err)
		}
		if v, _ := txn.Get([]byte("keep")); string(v) != "orig" {
			t.Fatalf("rollback left %q", v)
		}
		if _, err := txn.Get([]byte("spill")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("spilled value survived: %v", err)
		}
	})
}

// runConcurrencySuite pins the Store-level guarantees that motivated TxnKV:
// counters allocate without duplication and account creation has exactly one
// winner under contention (process-lock serialisation on single-node
// backends, transaction replay on TiDB).
func runConcurrencySuite(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()

	t.Run("concurrent counter allocates unique sequence", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		acct, err := s.CreateAccount(ctx, "seq@example.com")
		if err != nil {
			t.Fatal(err)
		}
		const workers, perWorker = 16, 25
		total := workers * perWorker
		ids := make(chan uint64, total)
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < perWorker; j++ {
					id, err := s.NextCounter(ctx, acct, 't', []byte("jobs"))
					if err != nil {
						errs <- err
						return
					}
					ids <- id
				}
			}()
		}
		wg.Wait()
		close(ids)
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		seen := make(map[uint64]bool, total)
		for id := range ids {
			if seen[id] {
				t.Fatalf("duplicate sequence number %d", id)
			}
			seen[id] = true
		}
		for i := uint64(1); i <= uint64(total); i++ {
			if !seen[i] {
				t.Fatalf("sequence gap at %d (%d/%d allocated)", i, len(seen), total)
			}
		}
	})

	t.Run("account creation race elects single winner", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		const racers = 16
		wins := make(chan AccountID, racers)
		results := make(chan error, racers)
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id, err := s.CreateAccount(ctx, "race@example.com")
				if err != nil {
					results <- err
					return
				}
				wins <- id
			}()
		}
		wg.Wait()
		close(wins)
		close(results)
		if n := len(wins); n != 1 {
			t.Fatalf("%d creators succeeded, want exactly 1", n)
		}
		for err := range results {
			if !errors.Is(err, ErrExists) {
				t.Fatalf("losers must fail with ErrExists, got %v", err)
			}
		}
	})
}

// openTiDBContract opens a throwaway TiDB table for one contract case; the
// whole group skips unless the enterprise build registered the TiDB opener
// and MAILEZINE_TEST_TIDB_DSN points at a live server.
func openTiDBContract(t *testing.T) KV {
	t.Helper()
	op := LookupKVOpener("tidb")
	if op == nil {
		t.Skip("TiDB backend not registered (enterprise build only)")
	}
	dsn := os.Getenv("MAILEZINE_TEST_TIDB_DSN")
	if dsn == "" {
		t.Skip("set MAILEZINE_TEST_TIDB_DSN to exercise the TiDB backend")
	}
	table := fmt.Sprintf("mailezine_kv_ct_%d", time.Now().UnixNano())
	kv, err := op(dsn, table)
	if err != nil {
		t.Fatalf("open tidb: %v", err)
	}
	db, derr := sql.Open("mysql", dsn)
	if derr == nil {
		q := fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table)
		t.Cleanup(func() { _, _ = db.Exec(q); _ = db.Close(); _ = kv.Close() })
	} else {
		t.Cleanup(func() { _ = kv.Close() })
	}
	return kv
}

func TestContractsMemory(t *testing.T) {
	newKV := func(*testing.T) KV { return NewMemoryKV() }
	runKVContract(t, newKV)
	runTxnContract(t, newKV)
	runConcurrencySuite(t, func(*testing.T) *Store {
		return New(NewMemoryKV(), NewMemoryBlob())
	})
}

func TestContractsPebble(t *testing.T) {
	newKV := func(t *testing.T) KV {
		kv, err := OpenPebble(t.TempDir())
		if err != nil {
			t.Fatalf("open pebble: %v", err)
		}
		t.Cleanup(func() { _ = kv.Close() })
		return kv
	}
	runKVContract(t, newKV)
	runTxnContract(t, newKV)
	runConcurrencySuite(t, func(t *testing.T) *Store {
		return New(newKV(t), NewMemoryBlob())
	})
}

func TestContractsTiDB(t *testing.T) {
	newKV := func(t *testing.T) KV { return openTiDBContract(t) }
	runKVContract(t, newKV)
	runTxnContract(t, newKV)
	runConcurrencySuite(t, func(t *testing.T) *Store {
		return New(openTiDBContract(t), NewMemoryBlob())
	})
	// Full logical invariants against the real server as well.
	runStoreSuite(t, func(t *testing.T) *Store {
		return New(openTiDBContract(t), NewMemoryBlob())
	})
}
