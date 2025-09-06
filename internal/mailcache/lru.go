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
	"hash/fnv"
	"io"
	"sync"
	"time"
)

// shardCount is the LRU's lock striping. Reads (Get) move entries to the
// front, so every cache touch takes its shard's mutex; at fifty concurrent
// IMAP sessions a single lock was the remaining global serial point.
const shardCount = 16

// Cache is a fixed-weight least-recently-used cache with optional TTL and
// negative caching. Entries are striped across shardCount locks when the
// capacity is large enough to shard; the weight bound applies per shard
// (total capacity = maxWeight).
type Cache struct {
	shards []cacheShard
	ttl    time.Duration
	negTTL time.Duration
}

type cacheShard struct {
	mu     sync.Mutex
	max    int64
	weight int64
	ll     *list.List
	items  map[string]*list.Element
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
	n := shardsFor(maxWeight)
	c := &Cache{ttl: ttl}
	c.shards = make([]cacheShard, n)
	per := maxWeight / int64(n)
	for i := range c.shards {
		c.shards[i].max = per
		c.shards[i].ll = list.New()
		c.shards[i].items = make(map[string]*list.Element)
	}
	return c
}

// shardsFor picks the lock striping for a capacity. Real memoizations are
// MiB-scale and contend across dozens of connections, so they stripe; a
// tiny cache stays single-shard — splitting 100 bytes across 16 locks
// would make every entry oversized and break strict-LRU eviction.
func shardsFor(maxWeight int64) int {
	if maxWeight >= 1<<20 {
		return shardCount
	}
	return 1
}

// shardOf stripes keys across shards. FNV-1a over short ASCII-ish keys
// ("msgs\x00acct\x00mailbox", envelope keys) distributes well and the
// 32-bit hash never allocates.
func (c *Cache) shardOf(key string) *cacheShard {
	h := fnv.New32a()
	_, _ = io.WriteString(h, key)
	return &c.shards[h.Sum32()&uint32(len(c.shards)-1)]
}

// Get returns the cached value for key, moving it to the front. A hit whose
// value is nil means the key was negatively cached ("known absent").
func (c *Cache) Get(key string) (any, bool) {
	return c.shardOf(key).get(key)
}

func (s *cacheShard) get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return nil, false
	}
	if e := el.Value.(*entry); !e.expires.IsZero() && time.Now().After(e.expires) {
		s.removeLocked(el)
		return nil, false
	}
	s.ll.MoveToFront(el)
	return el.Value.(*entry).value, true
}

// Put stores value under key with the given weight, evicting the
// least-recently-used entries until the shard is within its capacity share.
func (c *Cache) Put(key string, value any, weight int64) {
	c.shardOf(key).put(key, value, weight, c.expiry(weight))
}

func (s *cacheShard) put(key string, value any, weight int64, expires time.Time) {
	// Every entry carries a fixed footprint (map slot, list element, key
	// storage) on top of the value. Account for it so callers passing tiny
	// weights (e.g. 1 for a bool) cannot balloon memory.
	weight += int64(64 + len(key))
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		e := el.Value.(*entry)
		s.weight += weight - e.weight
		e.value = value
		e.weight = weight
		e.expires = expires
		s.ll.MoveToFront(el)
	} else {
		el := s.ll.PushFront(&entry{key: key, value: value, weight: weight, expires: expires})
		s.items[key] = el
		s.weight += weight
	}
	for s.weight > s.max {
		oldest := s.ll.Back()
		if oldest != nil {
			s.removeLocked(oldest)
		}
	}
}

// PutNegative records that key is known absent. The entry expires after the
// negative TTL (or the regular TTL when no negative TTL was configured).
func (c *Cache) PutNegative(key string) {
	ttl := c.negTTL
	if ttl <= 0 {
		ttl = c.ttl
	}
	if ttl <= 0 {
		return
	}
	c.shardOf(key).put(key, nil, 1, time.Now().Add(ttl))
}

// Remove deletes a key from the cache.
func (c *Cache) Remove(key string) {
	c.shardOf(key).remove(key)
}

func (s *cacheShard) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.removeLocked(el)
	}
}

// Clear empties the cache.
func (c *Cache) Clear() {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		s.ll.Init()
		s.items = make(map[string]*list.Element)
		s.weight = 0
		s.mu.Unlock()
	}
}

func (c *Cache) expiry(weight int64) time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// Weight reports the current total cached weight across shards.
func (c *Cache) Weight() int64 {
	var total int64
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		total += s.weight
		s.mu.Unlock()
	}
	return total
}

func (s *cacheShard) removeLocked(el *list.Element) {
	s.ll.Remove(el)
	e := el.Value.(*entry)
	delete(s.items, e.key)
	s.weight -= e.weight
}
