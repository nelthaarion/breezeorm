package cache

import (
        "container/list"
        "sync"
        "sync/atomic"
)

// LRU is a bounded, thread-safe, size-limited cache with least-recently-used
// eviction.
//
// Use this (not Cache) when the cache key is something a client or attacker
// can influence — SQL text, query shape hashes, etc. An unbounded map there
// is a memory-exhaustion vector: a workload generating many distinct query
// shapes could grow it forever. Here, eviction kicks in at capacity.
//
// Pass onEvict to release whatever resource the evicted value holds (e.g.
// closing a *sql.Stmt). Eviction is capacity-triggered, not time-based.
type LRU[K comparable, V any] struct {
        mu       sync.RWMutex // Task 2.5: RWMutex so GetNoTouch can take RLock
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
//
// PANIC SAFETY: if compute() panics, the inflight entry is removed and
// cl.done is closed (so any waiting concurrent callers unblock), then the
// panic is re-thrown. Without this, a single panic would wedge the cache
// forever: every subsequent call for the same key would block on the
// never-closed cl.done channel. The cache is NOT populated on panic
// (cl.err is set to a non-nil sentinel via the recover path, which the
// setLocked guard below treats as "don't install").
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

        // Capture the panic so we can run cleanup, then re-panic.
        // Cleanup MUST run regardless of whether compute returned normally
        // or panicked — otherwise:
        //   - cl.done is never closed → concurrent callers block forever
        //   - inflight[key] is never deleted → the key is permanently wedged
        var panicked bool
        var panicVal any
        func() {
                defer func() {
                        if r := recover(); r != nil {
                                panicked = true
                                panicVal = r
                        }
                }()
                cl.value, cl.err = compute()
        }()

        c.mu.Lock()
        var evicted []entry[K, V]
        if !panicked && cl.err == nil {
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

        if panicked {
                panic(panicVal) // re-throw so the caller sees the panic
        }
        if cl.err != nil {
                var zero V
                return zero, cl.err
        }
        return cl.value, nil
}

// GetNoTouch returns the cached value for key WITHOUT promoting it to
// most-recently-used. Uses an RLock, so concurrent readers don't block
// each other. Use this for read-hot caches where the access pattern is
// "a small set of entries hit millions of times" — the LRU order barely
// changes, so the MoveToFront on every Get is pure overhead and a source
// of write-lock contention under concurrent load.
//
// Trade-off: if the cache is at capacity and a diverse working set is
// evicting entries, GetNoTouch'd entries are more likely to be evicted
// (they look "cold" to the LRU policy). For caches where the working set
// fits comfortably (compiledCache, scanPlanCache — both default 2000
// entries, plenty for typical apps), this trade-off is fine.
func (c *LRU[K, V]) GetNoTouch(key K) (V, bool) {
        c.mu.RLock()
        el, ok := c.items[key]
        c.mu.RUnlock()

        if ok {
                c.hits.Add(1)
                return el.Value.(*entry[K, V]).value, true
        }
        c.misses.Add(1)
        var zero V
        return zero, false
}

// GetOrComputeNoTouch is GetOrCompute with the read-path optimization of
// GetNoTouch: the hit path uses RLock and does not MoveToFront. The
// miss-coalescing behavior is unchanged.
//
// Use this instead of GetOrCompute for read-hot caches where the LRU
// order rarely changes (compiledCache, scanPlanCache). The trade-off is
// the same as GetNoTouch's: under capacity pressure, no-touch entries are
// more likely to be evicted.
func (c *LRU[K, V]) GetOrComputeNoTouch(key K, compute func() (V, error)) (V, error) {
        // Fast path: read-only lookup under RLock.
        c.mu.RLock()
        if el, ok := c.items[key]; ok {
                v := el.Value.(*entry[K, V]).value
                c.mu.RUnlock()
                c.hits.Add(1)
                return v, nil
        }
        c.mu.RUnlock()
        c.misses.Add(1)

        // Slow path: upgrade to write lock, re-check, compute.
        c.mu.Lock()
        if el, ok := c.items[key]; ok {
                v := el.Value.(*entry[K, V]).value
                c.mu.Unlock()
                c.hits.Add(1)
                return v, nil
        }
        if cl, ok := c.inflight[key]; ok {
                c.mu.Unlock()
                <-cl.done
                return cl.value, cl.err
        }
        cl := &call[V]{done: make(chan struct{})}
        c.inflight[key] = cl
        c.mu.Unlock()

        // Same panic-safe compute + cleanup logic as GetOrCompute (Task 1.3).
        var panicked bool
        var panicVal any
        func() {
                defer func() {
                        if r := recover(); r != nil {
                                panicked = true
                                panicVal = r
                        }
                }()
                cl.value, cl.err = compute()
        }()

        c.mu.Lock()
        var evicted []entry[K, V]
        if !panicked && cl.err == nil {
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

        if panicked {
                panic(panicVal)
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
