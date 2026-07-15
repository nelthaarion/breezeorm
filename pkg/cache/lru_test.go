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

// --- Task 1.3 regression tests: GetOrCompute panic safety -----------------

func TestGetOrCompute_PanicDoesNotWedgeCache(t *testing.T) {
	c := NewLRU[string, int](10, nil)
	key := "k"

	// First call: compute panics. The cache must NOT be wedged after this.
	var panicked bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_, _ = c.GetOrCompute(key, func() (int, error) { panic("boom") })
	}()
	if !panicked {
		t.Fatal("expected panic to propagate to caller")
	}

	// Second call: must NOT block. The cache was not populated (panic path
	// skips setLocked), so this should re-run compute and succeed.
	done := make(chan int, 1)
	go func() {
		v, err := c.GetOrCompute(key, func() (int, error) { return 42, nil })
		if err != nil {
			t.Errorf("unexpected err after panic: %v", err)
		}
		done <- v
	}()
	select {
	case v := <-done:
		if v != 42 {
			t.Errorf("got %d, want 42", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cache wedged: GetOrCompute blocked after a previous panic")
	}
}

func TestGetOrCompute_ConcurrentCallersUnblockOnPanic(t *testing.T) {
	c := NewLRU[string, int](10, nil)
	key := "k"
	startCompute := make(chan struct{})
	computeStarted := make(chan struct{})

	// Goroutine 1: holds the compute slot and panics.
	go func() {
		defer func() { _ = recover() }()
		_, _ = c.GetOrCompute(key, func() (int, error) {
			close(computeStarted)
			<-startCompute
			panic("boom")
		})
	}()

	<-computeStarted

	// Goroutine 2: should be blocked on cl.done, waiting for goroutine 1.
	result := make(chan int, 1)
	go func() {
		v, _ := c.GetOrCompute(key, func() (int, error) { return 42, nil })
		result <- v
	}()

	close(startCompute) // triggers the panic in goroutine 1

	select {
	case <-result:
		// Goroutine 2 unblocked and re-ran compute. Pass.
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent caller blocked forever after panic in compute")
	}
}

func TestGetOrCompute_PanicDoesNotPopulateCache(t *testing.T) {
	c := NewLRU[string, int](10, nil)
	key := "k"

	// First call: compute panics with a sentinel value.
	func() {
		defer func() { _ = recover() }()
		_, _ = c.GetOrCompute(key, func() (int, error) { panic("nope") })
	}()

	// The cache must NOT contain the key (panic path skips setLocked).
	if _, ok := c.Get(key); ok {
		t.Fatal("cache was populated on panic path — should not be")
	}
}

// --- Task 2.5 tests: GetNoTouch / GetOrComputeNoTouch ---------------------

func TestLRU_GetNoTouch_DoesNotPromote(t *testing.T) {
	c := NewLRU[string, int](2, nil)
	c.Set("a", 1)
	c.Set("b", 2)        // "a" is now LRU
	c.GetNoTouch("a")    // does NOT promote "a"
	c.Set("c", 3)        // evicts LRU = "a"
	if _, ok := c.Get("a"); ok {
		t.Error("GetNoTouch should not have promoted 'a', but it was not evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("'b' should still be present")
	}
}

func TestLRU_GetNoTouch_ReturnsCorrectValue(t *testing.T) {
	c := NewLRU[string, int](2, nil)
	c.Set("a", 1)
	v, ok := c.GetNoTouch("a")
	if !ok || v != 1 {
		t.Errorf("GetNoTouch('a') = (%d, %v), want (1, true)", v, ok)
	}
	// Non-existent key.
	v, ok = c.GetNoTouch("missing")
	if ok || v != 0 {
		t.Errorf("GetNoTouch('missing') = (%d, %v), want (0, false)", v, ok)
	}
}

func TestLRU_GetOrComputeNoTouch_DoesNotPromoteOnHit(t *testing.T) {
	c := NewLRU[string, int](2, nil)
	c.Set("a", 1)
	c.Set("b", 2)
	// GetOrComputeNoTouch on "a" should NOT promote it.
	v, err := c.GetOrComputeNoTouch("a", func() (int, error) { return 99, nil })
	if err != nil || v != 1 {
		t.Fatalf("GetOrComputeNoTouch hit returned (%d, %v), want (1, nil)", v, err)
	}
	c.Set("c", 3) // should evict LRU = "a"
	if _, ok := c.Get("a"); ok {
		t.Error("GetOrComputeNoTouch should not have promoted 'a', but it survived eviction")
	}
}

func TestLRU_GetOrComputeNoTouch_ComputesOnMiss(t *testing.T) {
	c := NewLRU[string, int](2, nil)
	v, err := c.GetOrComputeNoTouch("new", func() (int, error) { return 42, nil })
	if err != nil || v != 42 {
		t.Fatalf("GetOrComputeNoTouch miss returned (%d, %v), want (42, nil)", v, err)
	}
	// Verify it was stored.
	if got, ok := c.Get("new"); !ok || got != 42 {
		t.Errorf("after GetOrComputeNoTouch, Get('new') = (%d, %v), want (42, true)", got, ok)
	}
}

func TestShardedLRU_GetNoTouch(t *testing.T) {
	s := NewShardedLRU[string, int](10, 4, StringHash, nil)
	s.Set("k", 1)
	v, ok := s.GetNoTouch("k")
	if !ok || v != 1 {
		t.Errorf("GetNoTouch('k') = (%d, %v), want (1, true)", v, ok)
	}
}

func TestShardedLRU_GetOrComputeNoTouch(t *testing.T) {
	s := NewShardedLRU[string, int](10, 4, StringHash, nil)
	v, err := s.GetOrComputeNoTouch("k", func() (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Fatalf("GetOrComputeNoTouch returned (%d, %v), want (7, nil)", v, err)
	}
}
