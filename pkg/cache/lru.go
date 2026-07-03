package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// LRU is a bounded, thread-safe, size-limited cache with least-recently-used
// eviction. Unlike Cache (which is deliberately unbounded and lock-free-read,
// appropriate for caches keyed by a finite, program-controlled set such as
// Go types), LRU is for caches keyed by attacker- or client-influenced
// strings — SQL text, structural plan hashes — where an unbounded map is a
// memory-exhaustion vector: an attacker (or just a very dynamic workload)
// that generates many distinct query shapes can otherwise grow the cache
// without bound.
//
// Eviction is capacity-triggered (not time-based); pass onEvict to release
// any resource the evicted value holds (e.g. closing a *sql.Stmt).
type LRU[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List // front = most recently used
	items    map[K]*list.Element
	inflight map[K]*call[V] // coalesces concurrent misses for the same key

	onEvict func(key K, value V)

	hits   atomic.Int64
	misses atomic.Int64
}

type entry[K comparable, V any] struct {
	key   K
	value V
}

type call[V any] struct {
	done  chan struct{}
	value V
	err   error
}

// NewLRU creates a bounded cache holding at most capacity entries.
// capacity <= 0 is treated as 1 (a cache that always evicts everything but
// the most recent entry) rather than "unbounded" — callers who genuinely
// want unbounded caching should use Cache instead, explicitly.
func NewLRU[K comparable, V any](capacity int, onEvict func(K, V)) *LRU[K, V] {
	if capacity <= 0 {
		capacity = 1
	}
	return &LRU[K, V]{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[K]*list.Element, capacity),
		inflight: make(map[K]*call[V]),
		onEvict:  onEvict,
	}
}

// Get returns the cached value for key, promoting it to most-recently-used.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	el, ok := c.items[key]
	if ok {
		c.ll.MoveToFront(el)
	}
	c.mu.Unlock()

	if ok {
		c.hits.Add(1)
		return el.Value.(*entry[K, V]).value, true
	}
	c.misses.Add(1)
	var zero V
	return zero, false
}

// Set inserts or updates a value, evicting the least-recently-used entry if
// the cache is over capacity. onEvict (if configured) is invoked for any
// evicted entry, outside the lock, so it may safely do I/O (e.g. Stmt.Close).
func (c *LRU[K, V]) Set(key K, value V) {
	var evicted []entry[K, V]

	c.mu.Lock()
	evicted = c.setLocked(key, value)
	c.mu.Unlock()

	if c.onEvict != nil {
		for _, ev := range evicted {
			c.onEvict(ev.key, ev.value)
		}
	}
}

func (c *LRU[K, V]) setLocked(key K, value V) []entry[K, V] {
	if el, ok := c.items[key]; ok {
		el.Value.(*entry[K, V]).value = value
		c.ll.MoveToFront(el)
		return nil
	}

	el := c.ll.PushFront(&entry[K, V]{key: key, value: value})
	c.items[key] = el

	var evicted []entry[K, V]
	for c.ll.Len() > c.capacity {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.ll.Remove(back)
		ev := back.Value.(*entry[K, V])
		delete(c.items, ev.key)
		evicted = append(evicted, *ev)
	}
	return evicted
}

// GetOrCompute returns the cached value for key, computing it via compute on
// a miss. Concurrent misses for the *same* key are coalesced — only one
// caller actually runs compute (important when compute does real I/O, such
// as preparing a statement against the database: without coalescing, a burst
// of concurrent requests for a not-yet-cached query would each independently
// pay the round trip).
func (c *LRU[K, V]) GetOrCompute(key K, compute func() (V, error)) (V, error) {
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		v := el.Value.(*entry[K, V]).value
		c.mu.Unlock()
		c.hits.Add(1)
		return v, nil
	}
	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		c.misses.Add(1)
		<-cl.done
		return cl.value, cl.err
	}
	c.misses.Add(1)
	cl := &call[V]{done: make(chan struct{})}
	c.inflight[key] = cl
	c.mu.Unlock()

	cl.value, cl.err = compute()

	c.mu.Lock()
	var evicted []entry[K, V]
	if cl.err == nil {
		evicted = c.setLocked(key, cl.value)
	}
	delete(c.inflight, key)
	close(cl.done)
	c.mu.Unlock()

	if c.onEvict != nil {
		for _, ev := range evicted {
			c.onEvict(ev.key, ev.value)
		}
	}
	if cl.err != nil {
		var zero V
		return zero, cl.err
	}
	return cl.value, nil
}

// Len returns the current number of cached entries.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Stats returns cumulative hit/miss counters.
func (c *LRU[K, V]) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// Delete removes a single key, invoking onEvict if it was present.
func (c *LRU[K, V]) Delete(key K) {
	c.mu.Lock()
	el, ok := c.items[key]
	var val V
	if ok {
		val = el.Value.(*entry[K, V]).value
		c.ll.Remove(el)
		delete(c.items, key)
	}
	c.mu.Unlock()
	if ok && c.onEvict != nil {
		c.onEvict(key, val)
	}
}

// Purge evicts every entry, invoking onEvict for each — used on shutdown to
// deterministically close every cached resource (e.g. all prepared
// statements) rather than leaking them until GC.
func (c *LRU[K, V]) Purge() {
	c.mu.Lock()
	all := make([]entry[K, V], 0, len(c.items))
	for _, el := range c.items {
		all = append(all, *el.Value.(*entry[K, V]))
	}
	c.items = make(map[K]*list.Element)
	c.ll = list.New()
	c.mu.Unlock()

	if c.onEvict != nil {
		for _, e := range all {
			c.onEvict(e.key, e.value)
		}
	}
}
