package store

import (
	"bytes"
	"errors"
	"testing"
)

func TestMemoryKVPutGetDelete(t *testing.T) {
	kv := NewMemoryKV()
	if _, err := kv.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := kv.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	v, err := kv.Get([]byte("a"))
	if err != nil || !bytes.Equal(v, []byte("1")) {
		t.Fatalf("get after put: v=%q err=%v", v, err)
	}
	if err := kv.Delete([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestMemoryKVScanOrderAndPrefix(t *testing.T) {
	kv := NewMemoryKV()
	for _, kv2 := range [][2]string{{"b/x", "1"}, {"a/y", "2"}, {"a/z", "3"}, {"c/w", "4"}} {
		if err := kv.Put([]byte(kv2[0]), []byte(kv2[1])); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err := kv.Scan([]byte("a/"), func(k, v []byte) error {
		got = append(got, string(k)+"="+string(v))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/y=2", "a/z=3"}
	if len(got) != len(want) {
		t.Fatalf("scan got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scan order mismatch at %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestMemoryKVScanStopsOnError(t *testing.T) {
	kv := NewMemoryKV()
	_ = kv.Put([]byte("a"), []byte("1"))
	_ = kv.Put([]byte("b"), []byte("2"))
	sentinel := errors.New("stop")
	err := kv.Scan(nil, func(k, v []byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestMemoryKVBatchAtomicity(t *testing.T) {
	kv := NewMemoryKV()
	err := kv.Batch([]Op{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("a"), Delete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected a deleted")
	}
	v, err := kv.Get([]byte("b"))
	if err != nil || string(v) != "2" {
		t.Fatalf("b: v=%q err=%v", v, err)
	}
}
