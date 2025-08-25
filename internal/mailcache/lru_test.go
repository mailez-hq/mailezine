package mailcache

import (
	"testing"
	"time"
)

func TestLRUWeightEviction(t *testing.T) {
	// Each entry carries ~65B overhead (64 + key length), plus the value.
	c := NewCache(200)
	c.Put("a", 1, 4)
	c.Put("b", 2, 4)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("get a = %v %v", v, ok)
	}
	c.Put("c", 3, 4) // total 3×69 > 200: evicts "b" (LRU)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("get c = %v %v", v, ok)
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("get a after c = %v %v", v, ok)
	}
}

func TestLRUOversizedEntry(t *testing.T) {
	c := NewCache(70)
	c.Put("big", "x", 100)
	if c.weight > 70 {
		t.Fatalf("oversized entry should be evicted immediately, weight=%d", c.weight)
	}
	if _, ok := c.Get("big"); ok {
		t.Fatal("oversized entry should not be cached")
	}
}

func TestLRUUpdate(t *testing.T) {
	c := NewCache(200)
	c.Put("k", "v1", 2) // weight includes entry overhead
	c.Put("k", "v2", 2)
	if v, _ := c.Get("k"); v != "v2" {
		t.Fatalf("update: %v", v)
	}
}

func TestTTLExpiry(t *testing.T) {
	c := NewCacheWithTTL(100, 30*time.Millisecond)
	c.Put("k", "v", 1)
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("before expiry: %v %v", v, ok)
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestNegativeCache(t *testing.T) {
	c := NewCacheWithNegative(100, time.Minute, 20*time.Millisecond)
	c.PutNegative("missing")
	v, ok := c.Get("missing")
	if !ok || v != nil {
		t.Fatalf("negative hit = %v %v", v, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get("missing"); ok {
		t.Fatal("negative entry should have expired")
	}
	// A regular value is not nil.
	c.Put("present", "x", 1)
	if v, ok := c.Get("present"); !ok || v != "x" {
		t.Fatalf("regular hit = %v %v", v, ok)
	}
}

func TestRemoveAndClear(t *testing.T) {
	c := NewCache(100)
	c.Put("a", 1, 1)
	c.Put("b", 2, 1)
	c.Remove("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be removed")
	}
	c.Clear()
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be cleared")
	}
	if c.weight != 0 {
		t.Fatalf("weight after clear = %d", c.weight)
	}
}
