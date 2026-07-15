package cache

import "hash/maphash"

// defaultShardCount is used whenever a caller doesn't specify one.
// Power-of-two so shard selection is a mask, not a modulo. 32 comfortably
// covers the core counts (8-64) prepared-statement and plan caches actually
// run on without slicing a modest (e.g. 2000-entry) capacity into so many
// shards that each one's LRU window becomes too small to hold a workload's
// working set.
const defaultShardCount = 32

// ShardedLRU is a bounded, thread-safe cache with the same semantics as LRU
// (same eviction policy, same GetOrCompute miss-coalescing) but split into N
// independently-locked shards instead of one.
//
// Why this exists: LRU serializes every Get/Set/GetOrCompute behind a single
// mutex. That's fine for a write-once, read-often cache with a diverse key
// space. But the prepared-statement cache under a benchmark (GOMAXPROCS=12,
// every goroutine hammering the same handful of query shapes) has the
// opposite pattern: a small, hot key set hit concurrently from every P. A
// single mutex there serializes all 12 goroutines on every call for no
// reason. Sharding by hash of the key means two goroutines only contend if
// they land in the same shard — with 32 shards and a few hot keys, that's rare.
//
// IMPORTANT: the hash is used ONLY to pick a shard, never as the map key.
// Each shard is a full LRU[K, V] keyed on the real K with exact equality.
// A hash collision just costs a shard mis-pick (mild contention), never a
// wrong-value return. This matters most for the prepared-statement cache:
// returning the wrong *Stmt for a SQL string would silently run the wrong query.
type ShardedLRU[K comparable, V any] struct {
        shards []*LRU[K, V]
        seed   maphash.Seed
        mask   uint64 // len(shards)-1; len(shards) is always a power of two
        hash   func(maphash.Seed, K) uint64
}

// NewShardedLRU creates a sharded cache with totalCapacity split evenly
// across numShards shards (each getting totalCapacity/numShards, minimum 1;
// numShards itself is rounded up to the next power of two, minimum 1 — a
// numShards <= 0 falls back to defaultShardCount). hash must be a stable
// hash function over K; StringHash and Uint64Hash below cover the two key
// types this codebase actually caches on (rendered SQL text, and the
// uint64 structural query hash from pkg/compiler). onEvict is invoked
// exactly as it would be for a plain LRU, per-shard.
func NewShardedLRU[K comparable, V any](totalCapacity, numShards int, hash func(maphash.Seed, K) uint64, onEvict func(K, V)) *ShardedLRU[K, V] {
        if numShards <= 0 {
                numShards = defaultShardCount
        }
        if totalCapacity <= 0 {
                totalCapacity = 1
        }
        numShards = nextPow2(numShards)
        if numShards > totalCapacity {
                // More shards than capacity would mean most shards hold zero
                // entries and the ones that do hold exactly one — i.e. an LRU that
                // (almost) never evicts via the normal "least recently used" path,
                // just via "whichever shard you happened to land in is full."
                // Capping shard count at capacity keeps small, deliberately-tight
                // caches (e.g. tests exercising eviction behavior directly, or a
                // production override sized down for a memory-constrained deploy)
                // behaving like a real bounded LRU instead of silently becoming
                // (effectively) unbounded up to numShards entries.
                numShards = prevPow2(totalCapacity)
        }
        perShard := totalCapacity / numShards
        if perShard <= 0 {
                perShard = 1
        }
        s := &ShardedLRU[K, V]{
                shards: make([]*LRU[K, V], numShards),
                seed:   maphash.MakeSeed(),
                mask:   uint64(numShards - 1),
                hash:   hash,
        }
        for i := range s.shards {
                s.shards[i] = NewLRU[K, V](perShard, onEvict)
        }
        return s
}

func nextPow2(n int) int {
        p := 1
        for p < n {
                p <<= 1
        }
        return p
}

// prevPow2 returns the largest power of two <= n (minimum 1, so n <= 0
// still yields a valid single-shard result rather than zero shards).
func prevPow2(n int) int {
        if n < 1 {
                return 1
        }
        p := 1
        for p*2 <= n {
                p *= 2
        }
        return p
}

func (s *ShardedLRU[K, V]) shardFor(key K) *LRU[K, V] {
        return s.shards[s.hash(s.seed, key)&s.mask]
}

// Get returns the cached value for key, promoting it within its shard.
func (s *ShardedLRU[K, V]) Get(key K) (V, bool) {
        return s.shardFor(key).Get(key)
}

// Set inserts or updates a value in key's shard, evicting that shard's LRU
// entry if the shard is over its (total/numShards) capacity.
func (s *ShardedLRU[K, V]) Set(key K, value V) {
        s.shardFor(key).Set(key, value)
}

// GetOrCompute returns the cached value for key, computing it via compute
// on a miss. Coalescing of concurrent misses (see LRU.GetOrCompute) is
// per-shard: concurrent first-time callers for the same key still coalesce
// into a single compute() call; callers for different keys in different
// shards run fully in parallel instead of queuing behind one lock.
func (s *ShardedLRU[K, V]) GetOrCompute(key K, compute func() (V, error)) (V, error) {
        return s.shardFor(key).GetOrCompute(key, compute)
}

// GetNoTouch returns the cached value for key WITHOUT promoting it to
// most-recently-used (see LRU.GetNoTouch). Useful for read-hot caches
// where the access pattern is a small hot set hit millions of times —
// the MoveToFront on every Get is pure overhead under that pattern.
func (s *ShardedLRU[K, V]) GetNoTouch(key K) (V, bool) {
        return s.shardFor(key).GetNoTouch(key)
}

// GetOrComputeNoTouch is GetOrCompute with the read-path optimization of
// GetNoTouch: the hit path uses RLock and does not MoveToFront. The
// miss-coalescing behavior is unchanged. See LRU.GetOrComputeNoTouch.
func (s *ShardedLRU[K, V]) GetOrComputeNoTouch(key K, compute func() (V, error)) (V, error) {
        return s.shardFor(key).GetOrComputeNoTouch(key, compute)
}

// Delete removes a single key from its shard.
func (s *ShardedLRU[K, V]) Delete(key K) {
        s.shardFor(key).Delete(key)
}

// Len returns the total number of cached entries across all shards.
func (s *ShardedLRU[K, V]) Len() int {
        n := 0
        for _, sh := range s.shards {
                n += sh.Len()
        }
        return n
}

// Stats returns cumulative hit/miss counters summed across all shards.
func (s *ShardedLRU[K, V]) Stats() (hits, misses int64) {
        for _, sh := range s.shards {
                h, m := sh.Stats()
                hits += h
                misses += m
        }
        return hits, misses
}

// Purge evicts every entry in every shard, invoking onEvict for each —
// used on shutdown to deterministically close every cached resource (e.g.
// every prepared statement) rather than leaking them until GC.
func (s *ShardedLRU[K, V]) Purge() {
        for _, sh := range s.shards {
                sh.Purge()
        }
}

// StringHash hashes a string key for shard selection. Pass to
// NewShardedLRU for string-keyed caches (e.g. rendered SQL text ->
// prepared statement).
func StringHash(seed maphash.Seed, k string) uint64 {
        var h maphash.Hash
        h.SetSeed(seed)
        h.WriteString(k)
        return h.Sum64()
}

// Uint64Hash "hashes" an already-well-distributed uint64 key (e.g. the
// structural query hash pkg/compiler.PreHash produces) for shard selection.
// It deliberately does NOT run the key through maphash again — the input is
// already a good hash of its original data, so re-hashing it would just
// spend CPU without improving distribution. Instead this applies a cheap
// avalanche mix (splitmix64's finalizer) so that even keys which happen to
// be numerically close (unlikely for a maphash output, but not guaranteed
// impossible depending on the producer) still spread across shard bits
// rather than landing in adjacent shards. seed is accepted (and ignored) so
// this has the same signature shape as StringHash and both can be passed
// interchangeably to NewShardedLRU.
func Uint64Hash(_ maphash.Seed, k uint64) uint64 {
        k ^= k >> 33
        k *= 0xff51afd7ed558ccd
        k ^= k >> 33
        k *= 0xc4ceb9fe1a85ec53
        k ^= k >> 33
        return k
}
