package cache

import (
	"fmt"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShardedLRU_SetGetRoundTrip(t *testing.T) {
	c := NewShardedLRU[string, int](100, 8, StringHash, nil)
	for i := 0; i < 50; i++ {
		c.Set(fmt.Sprintf("key-%d", i), i)
	}
	for i := 0; i < 50; i++ {
		v, ok := c.Get(fmt.Sprintf("key-%d", i))
		if !ok || v != i {
			t.Fatalf("Get(key-%d) = %v, %v; want %d, true", i, v, ok, i)
		}
	}
}

// TestShardedLRU_HashCollisionNeverReturnsWrongValue is the correctness
// guard called out in sharded.go's doc comment: the hash function is only
// used to pick a shard, so even two keys that deliberately hash to the same
// shard must never be confused with each other.
func TestShardedLRU_HashCollisionNeverReturnsWrongValue(t *testing.T) {
	// A hash function that collapses every key into shard 0 — the worst
	// case for shard distribution, but correctness must still hold.
	constantHash := func(_ maphash.Seed, _ string) uint64 { return 0 }
	c := NewShardedLRU[string, string](100, 8, constantHash, nil)

	c.Set("alpha", "value-alpha")
	c.Set("beta", "value-beta")
	c.Set("gamma", "value-gamma")

	if v, ok := c.Get("alpha"); !ok || v != "value-alpha" {
		t.Errorf(`Get("alpha") = %q, %v; want "value-alpha", true`, v, ok)
	}
	if v, ok := c.Get("beta"); !ok || v != "value-beta" {
		t.Errorf(`Get("beta") = %q, %v; want "value-beta", true`, v, ok)
	}
	if v, ok := c.Get("gamma"); !ok || v != "value-gamma" {
		t.Errorf(`Get("gamma") = %q, %v; want "value-gamma", true`, v, ok)
	}
}

func TestShardedLRU_Uint64KeyedRoundTrip(t *testing.T) {
	c := NewShardedLRU[uint64, string](100, 16, Uint64Hash, nil)
	keys := []uint64{0, 1, 2, 1 << 32, ^uint64(0), 123456789}
	for _, k := range keys {
		c.Set(k, fmt.Sprintf("v-%d", k))
	}
	for _, k := range keys {
		want := fmt.Sprintf("v-%d", k)
		if v, ok := c.Get(k); !ok || v != want {
			t.Errorf("Get(%d) = %q, %v; want %q, true", k, v, ok, want)
		}
	}
}

func TestShardedLRU_CapacitySplitAcrossShards(t *testing.T) {
	// totalCapacity=16, numShards=4 -> 4 entries per shard. Verify total
	// capacity across all shards roughly matches what was requested (exact
	// eviction boundary depends on hash distribution across shards, so this
	// checks the aggregate never wildly exceeds the request rather than an
	// exact per-shard count).
	c := NewShardedLRU[string, int](16, 4, StringHash, nil)
	for i := 0; i < 200; i++ {
		c.Set(fmt.Sprintf("key-%d", i), i)
	}
	if got := c.Len(); got > 16 {
		t.Errorf("Len() = %d, want <= 16 (total capacity across shards)", got)
	}
}

func TestShardedLRU_OnEvictCalledOnClose(t *testing.T) {
	type resource struct{ closed bool }
	var mu sync.Mutex
	res := map[string]*resource{}
	for i := 0; i < 10; i++ {
		res[fmt.Sprintf("k%d", i)] = &resource{}
	}

	c := NewShardedLRU[string, *resource](2, 4, StringHash, func(_ string, v *resource) {
		mu.Lock()
		v.closed = true
		mu.Unlock()
	})
	for i := 0; i < 10; i++ {
		c.Set(fmt.Sprintf("k%d", i), res[fmt.Sprintf("k%d", i)])
	}

	mu.Lock()
	defer mu.Unlock()
	closedCount := 0
	for _, r := range res {
		if r.closed {
			closedCount++
		}
	}
	if closedCount == 0 {
		t.Error("expected at least one eviction to have fired onEvict, given capacity 2 (across shards) and 10 inserted keys")
	}
}

func TestShardedLRU_GetOrCompute_CoalescesConcurrentMissesPerKey(t *testing.T) {
	c := NewShardedLRU[string, int](10, 4, StringHash, nil)
	var calls atomic.Int64

	compute := func() (int, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return 99, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrCompute("shared-key", compute)
			if err != nil || v != 99 {
				t.Errorf("unexpected result: %v %v", v, err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("compute called %d times, want exactly 1 (concurrent misses for the same key must coalesce)", got)
	}
}

func TestShardedLRU_StatsAggregateAcrossShards(t *testing.T) {
	c := NewShardedLRU[string, int](100, 8, StringHash, nil)
	c.Set("a", 1)
	c.Get("a")        // hit
	c.Get("does-not-exist") // miss

	hits, misses := c.Stats()
	if hits < 1 {
		t.Errorf("hits = %d, want >= 1", hits)
	}
	if misses < 1 {
		t.Errorf("misses = %d, want >= 1", misses)
	}
}

func TestShardedLRU_Purge(t *testing.T) {
	var closedCount atomic.Int64
	c := NewShardedLRU[string, int](100, 8, StringHash, func(string, int) { closedCount.Add(1) })
	for i := 0; i < 20; i++ {
		c.Set(fmt.Sprintf("k%d", i), i)
	}
	c.Purge()
	if closedCount.Load() != 20 {
		t.Errorf("closedCount = %d, want 20", closedCount.Load())
	}
	if c.Len() != 0 {
		t.Errorf("Len() after Purge = %d, want 0", c.Len())
	}
}

func TestNewShardedLRU_NumShardsRoundedToPowerOfTwo(t *testing.T) {
	c := NewShardedLRU[string, int](100, 5, StringHash, nil) // 5 -> 8
	if got := len(c.shards); got != 8 {
		t.Errorf("shard count = %d, want 8 (next power of two >= 5)", got)
	}
}

func TestNewShardedLRU_NonPositiveNumShardsUsesDefault(t *testing.T) {
	c := NewShardedLRU[string, int](100, 0, StringHash, nil)
	if got := len(c.shards); got != defaultShardCount {
		t.Errorf("shard count = %d, want defaultShardCount (%d)", got, defaultShardCount)
	}
}

func TestUint64Hash_IsDeterministic(t *testing.T) {
	seed := maphash.MakeSeed()
	a := Uint64Hash(seed, 12345)
	b := Uint64Hash(seed, 12345)
	if a != b {
		t.Errorf("Uint64Hash not deterministic: %d != %d", a, b)
	}
}
