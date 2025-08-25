// Package mailcache provides a small concurrency-safe LRU used to memoize
// message-derived data (IMAP envelope, body structure) across sessions.
// Entries are immutable per (account, mailbox, UID) — UIDs are never reused
// and message bytes never change — so no invalidation is required; the LRU
// bounds memory by evicting cold entries.
package mailcache

import (
	"container/list"
	"sync"
)

// LRU is a fixed-capacity least-recently-used cache.
type LRU struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
}

type entry struct {
	key   string
	value any
}

// NewLRU returns a cache holding at most max entries.
func NewLRU(max int) *LRU {
	if max <= 0 {
		max = 4096
	}
	return &LRU{
		max:   max,
		ll:    list.New(),
		items: make(map[string]*list.Element, max),
	}
}

// Get returns the cached value for key, moving it to the front.
func (c *LRU) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*entry).value, true
}

// Put stores value under key, evicting the least-recently-used entry when at
// capacity.
func (c *LRU) Put(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*entry).value = value
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&entry{key: key, value: value})
	c.items[key] = el
	if c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*entry).key)
		}
	}
}
