// Package cache provides a small thread-safe least-recently-used cache used by
// db.Store (object-ID lookups) and nostr.Signer (derived-key memoization). Both
// caches are pure-performance — eviction has no correctness impact because the
// next miss just re-derives the value from the source of truth (DB or HKDF).
package cache

import (
	"container/list"
	"sync"
)

// LRU is a thread-safe least-recently-used cache with a hard entry cap.
// Zero value is not usable — construct with New.
type LRU[K comparable, V any] struct {
	mu      sync.Mutex
	cap     int
	items   map[K]*list.Element
	order   *list.List // front = most recently used; back = least
}

type entry[K comparable, V any] struct {
	key   K
	value V
}

// New returns an LRU with the given capacity. A capacity <= 0 disables eviction
// (effectively an unbounded map — only useful for tests).
func New[K comparable, V any](capacity int) *LRU[K, V] {
	return &LRU[K, V]{
		cap:   capacity,
		items: make(map[K]*list.Element, capacity),
		order: list.New(),
	}
}

// Get returns the cached value for key and promotes it to most-recently-used.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*entry[K, V]).value, true
	}
	var zero V
	return zero, false
}

// Add inserts or updates the entry for key and promotes it to MRU. If adding
// would exceed capacity, the LRU entry is evicted.
func (c *LRU[K, V]) Add(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*entry[K, V]).value = value
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&entry[K, V]{key: key, value: value})
	c.items[key] = el
	if c.cap > 0 && c.order.Len() > c.cap {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*entry[K, V]).key)
		}
	}
}

// Remove deletes the entry for key if present. No-op if absent.
func (c *LRU[K, V]) Remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

// Len returns the number of cached entries.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
