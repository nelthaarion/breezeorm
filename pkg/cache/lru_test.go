package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLRU_EvictsOverCapacity(t *testing.T) {
	var evicted []string
	c := NewLRU[string, int](2, func(k string, v int) { evicted = append(evicted, k) })

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3) // should evict "a" (least recently used)

	if _, ok := c.Get("a"); ok {
		t.Error("expected a to be evicted")
	}
	if len(evicted) != 1 || evicted[0] != "a" {
		t.Errorf("onEvict = %v, want [a]", evicted)
	}
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
}

func TestLRU_GetPromotes(t *testing.T) {
	var evicted []string
	c := NewLRU[string, int](2, func(k string, v int) { evicted = append(evicted, k) })

	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a")    // promote a; b is now LRU
	c.Set("c", 3) // should evict "b", not "a"

	if len(evicted) != 1 || evicted[0] != "b" {
		t.Errorf("onEvict = %v, want [b]", evicted)
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("expected a to still be cached")
	}
}

func TestLRU_OnEvictCalledOnClose(t *testing.T) {
	type resource struct{ closed bool }
	var mu sync.Mutex
	res := map[string]*resource{"a": {}, "b": {}, "c": {}}

	c := NewLRU[string, *resource](2, func(k string, v *resource) {
		mu.Lock()
		v.closed = true
		mu.Unlock()
	})
	c.Set("a", res["a"])
	c.Set("b", res["b"])
	c.Set("c", res["c"]) // evicts a

	mu.Lock()
	defer mu.Unlock()
	if !res["a"].closed {
		t.Error("expected evicted resource to be closed via onEvict")
	}
	if res["b"].closed || res["c"].closed {
		t.Error("did not expect b or c to be closed")
	}
}

func TestLRU_GetOrCompute_CoalescesConcurrentMisses(t *testing.T) {
	c := NewLRU[string, int](10, nil)
	var calls atomic.Int64

	compute := func() (int, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // simulate a real I/O round trip
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
		t.Errorf("compute called %d times, want exactly 1 (concurrent misses should coalesce)", got)
	}
}

func TestLRU_Purge(t *testing.T) {
	var closedCount int
	c := NewLRU[string, int](10, func(string, int) { closedCount++ })
	c.Set("a", 1)
	c.Set("b", 2)
	c.Purge()
	if closedCount != 2 {
		t.Errorf("closedCount = %d, want 2", closedCount)
	}
	if c.Len() != 0 {
		t.Errorf("Len() after Purge = %d, want 0", c.Len())
	}
}
