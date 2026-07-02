package cache

import (
	"sync"
	"testing"
)

func TestCache_SetGet(t *testing.T) {
	c := New[string, int]()
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected miss on empty cache")
	}
	c.Set("a", 1)
	v, ok := c.Get("a")
	if !ok || v != 1 {
		t.Fatalf("got (%v, %v), want (1, true)", v, ok)
	}
}

func TestCache_GetOrCompute_ComputesOnce(t *testing.T) {
	c := New[string, int]()
	calls := 0
	compute := func() (int, error) {
		calls++
		return 42, nil
	}
	for i := 0; i < 5; i++ {
		v, err := c.GetOrCompute("k", compute)
		if err != nil || v != 42 {
			t.Fatalf("unexpected result: %v %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("compute called %d times, want 1", calls)
	}
}

func TestCache_ConcurrentReadsWrites(t *testing.T) {
	c := New[int, int]()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Set(i, i*i)
			_, _ = c.Get(i)
		}(i)
	}
	wg.Wait()
	if c.Len() != 100 {
		t.Errorf("Len() = %d, want 100", c.Len())
	}
}

func TestCache_Delete(t *testing.T) {
	c := New[string, int]()
	c.Set("a", 1)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Error("expected key to be gone after Delete")
	}
}
