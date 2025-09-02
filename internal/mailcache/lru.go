// Package mailcache provides a concurrency-safe cache primitive shared by
// every memoization point in the engine. Capacity is measured in weight
// (bytes) rather than entry count, the reference design for single-binary
// mail servers, so large entries cannot silently balloon memory. Three variants are available:
//
//   - NewCache: immutable values, no expiry (IMAP envelope / body structure —
//     messages never change and UIDs are never reused);
//   - NewCacheWithTTL: values expire after a fixed TTL (auth results);
//   - NewCacheWithNegative: adds negative caching — a Get hit whose value is
//     nil means "known absent", so callers avoid hammering the origin.
package mailcache

import (
	"container/list"
	"sync"
	"time"
)

// Cache is a fixed-weight least-recently-used cache with optional TTL and
// negative caching.
type Cache struct {
	mu     sync.Mutex
	max    int64
	weight int64
	ll     *list.List
	items  map[string]*list.Element
	ttl    time.Duration
	negTTL time.Duration
}

type entry struct {
	key     string
	value   any
	weight  int64
	expires time.Time
}

// NewCache returns a cache bounded by maxWeight bytes with no expiry.
func NewCache(maxWeight int64) *Cache {
	return newCache(maxWeight, 0)
}

// NewCacheWithTTL returns a cache bounded by maxWeight bytes whose entries
// expire ttl after being stored.
func NewCacheWithTTL(maxWeight int64, ttl time.Duration) *Cache {
	return newCache(maxWeight, ttl)
}

// NewCacheWithNegative returns a cache with the given TTL that also stores
// negative entries (nil values) for negTTL. A Get hit with a nil value means
// "known absent".
func NewCacheWithNegative(maxWeight int64, ttl, negTTL time.Duration) *Cache {
	c := newCache(maxWeight, ttl)
	c.negTTL = negTTL
	return c
}

func newCache(maxWeight int64, ttl time.Duration) *Cache {
	if maxWeight <= 0 {
		maxWeight = 8 << 20
	}
	return &Cache{
		max:   maxWeight,
		ll:    list.New(),
		items: make(map[string]*list.Element),
		ttl:   ttl,
	}
}

// Get returns the cached value for key, moving it to the front. A hit whose
// value is nil means the key was negatively cached ("known absent").
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if e := el.Value.(*entry); !e.expires.IsZero() && time.Now().After(e.expires) {
		c.removeLocked(el)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*entry).value, true
}

// Put stores value under key with the given weight, evicting the
// least-recently-used entries until the cache is within capacity.
func (c *Cache) Put(key string, value any, weight int64) {
	// Every entry carries a fixed footprint (map slot, list element, key
	// storage) on top of the value. Account for it so callers passing tiny
	// weights (e.g. 1 for a bool) cannot balloon memory.
	weight += int64(64 + len(key))
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		c.weight += weight - e.weight
		e.value = value
		e.weight = weight
		e.expires = c.expiry(weight)
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&entry{key: key, value: value, weight: weight, expires: c.expiry(weight)})
		c.items[key] = el
		c.weight += weight
	}
	for c.weight > c.max {
		oldest := c.ll.Back()
		if oldest != nil {
			c.removeLocked(oldest)
		}
	}
}

// PutNegative records that key is known absent. The entry expires after the
// negative TTL (or the regular TTL when no negative TTL was configured).
func (c *Cache) PutNegative(key string) {
	ttl := c.negTTL
	if ttl == 0 {
		ttl = c.ttl
	}
	if ttl <= 0 {
		return
	}
	c.Put(key, nil, 1)
	// Restore the negative TTL (Put used the regular expiry).
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		el.Value.(*entry).expires = time.Now().Add(ttl)
	}
	c.mu.Unlock()
}

// Remove deletes a key from the cache.
func (c *Cache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
}

// Clear empties the cache.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[string]*list.Element)
	c.weight = 0
}

func (c *Cache) expiry(weight int64) time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

func (c *Cache) removeLocked(el *list.Element) {
	c.ll.Remove(el)
	e := el.Value.(*entry)
	delete(c.items, e.key)
	c.weight -= e.weight
}
