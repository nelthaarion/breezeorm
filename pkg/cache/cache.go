// Package cache provides a generic, lock-free-read cache. Reads never take a
// lock: they atomically load an immutable map snapshot. Writes are
// copy-on-write (build a new map, swap the pointer) and serialized behind a
// mutex, which is fine because writes are rare relative to reads (a plan or
// prepared statement is written once, then read millions of times).
package cache

import (
	"sync"
	"sync/atomic"
)

// Cache is a generic, lock-free-read, copy-on-write cache keyed by comparable K.
type Cache[K comparable, V any] struct {
	snapshot atomic.Pointer[map[K]V]
	writeMu  sync.Mutex

	// hits/misses are for observability (pkg/plugins/metrics can read these).
	hits   atomic.Int64
	misses atomic.Int64
}

// New creates an empty Cache.
func New[K comparable, V any]() *Cache[K, V] {
	c := &Cache[K, V]{}
	empty := map[K]V{}
	c.snapshot.Store(&empty)
	return c
}

// Get performs a lock-free read.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	m := *c.snapshot.Load()
	v, ok := m[key]
	if ok {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return v, ok
}

// Set inserts or replaces a value. Copy-on-write: builds a new map so
// concurrent readers of the old snapshot are never affected.
func (c *Cache[K, V]) Set(key K, val V) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	old := *c.snapshot.Load()
	next := make(map[K]V, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[key] = val
	c.snapshot.Store(&next)
}

// GetOrCompute performs a lock-free read; on miss it computes the value
// under the write lock (re-checking to avoid duplicate work from concurrent
// callers) and installs it.
func (c *Cache[K, V]) GetOrCompute(key K, compute func() (V, error)) (V, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	old := *c.snapshot.Load()
	if v, ok := old[key]; ok {
		return v, nil
	}
	v, err := compute()
	if err != nil {
		var zero V
		return zero, err
	}
	next := make(map[K]V, len(old)+1)
	for k, vv := range old {
		next[k] = vv
	}
	next[key] = v
	c.snapshot.Store(&next)
	return v, nil
}

// Len returns the current number of cached entries.
func (c *Cache[K, V]) Len() int {
	return len(*c.snapshot.Load())
}

// Stats returns cumulative hit/miss counters.
func (c *Cache[K, V]) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// Delete removes a key (copy-on-write).
func (c *Cache[K, V]) Delete(key K) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	old := *c.snapshot.Load()
	if _, ok := old[key]; !ok {
		return
	}
	next := make(map[K]V, len(old))
	for k, v := range old {
		if k == key {
			continue
		}
		next[k] = v
	}
	c.snapshot.Store(&next)
}
