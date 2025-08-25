package mailcache

import "testing"

func TestLRU(t *testing.T) {
	c := NewLRU(2)
	c.Put("a", 1)
	c.Put("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("get a = %v %v", v, ok)
	}
	c.Put("c", 3) // evicts "b" (LRU)
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

func TestLRUUpdate(t *testing.T) {
	c := NewLRU(1)
	c.Put("k", "v1")
	c.Put("k", "v2")
	if v, _ := c.Get("k"); v != "v2" {
		t.Fatalf("update: %v", v)
	}
}
