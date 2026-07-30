package goache

import (
	"fmt"
	"sync"
	"testing"
)

func TestSetGet(t *testing.T) {
	c := New[string, int]()

	if _, ok := c.Get("missing"); ok {
		t.Fatalf("expected miss for unset key")
	}

	c.Set("a", 1)
	v, ok := c.Get("a")
	if !ok || v != 1 {
		t.Fatalf("Get(a) = %d, %v; want 1, true", v, ok)
	}

	c.Set("a", 2)
	v, ok = c.Get("a")
	if !ok || v != 2 {
		t.Fatalf("overwrite: Get(a) = %d, %v; want 2, true", v, ok)
	}
}

func TestSetMany(t *testing.T) {
	c := New[int, string]()

	entries := make([]Entry[int, string], 0, 100)
	for i := range 100 {
		entries = append(entries, Entry[int, string]{Key: i, Value: fmt.Sprintf("v%d", i)})
	}
	c.SetMany(entries)

	if got := c.Len(); got != 100 {
		t.Fatalf("Len() = %d; want 100", got)
	}
	for i := range 100 {
		v, ok := c.Get(i)
		want := fmt.Sprintf("v%d", i)
		if !ok || v != want {
			t.Fatalf("Get(%d) = %q, %v; want %q, true", i, v, ok, want)
		}
	}
}

func TestSetManyEmpty(t *testing.T) {
	c := New[int, int]()
	c.SetMany(nil)
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d; want 0", got)
	}
}

func TestConcurrentSetGet(t *testing.T) {
	c := New[int, int](WithShardCount(64))

	const goroutines = 32
	const perGoroutine = 1000

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				key := g*perGoroutine + i
				c.Set(key, key*2)
			}
		})
	}
	wg.Wait()

	var rwg sync.WaitGroup
	for g := range goroutines {
		rwg.Go(func() {
			for i := range perGoroutine {
				key := g*perGoroutine + i
				v, ok := c.Get(key)
				if !ok || v != key*2 {
					t.Errorf("Get(%d) = %d, %v; want %d, true", key, v, ok, key*2)
				}
			}
		})
	}
	rwg.Wait()

	if got, want := c.Len(), goroutines*perGoroutine; got != want {
		t.Fatalf("Len() = %d; want %d", got, want)
	}
}

func TestConcurrentMixedReadWrite(t *testing.T) {
	c := New[int, int](WithShardCount(16))
	const key = 42
	const iterations = 10000
	c.Set(key, 0)

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Go(func() {
			for j := range iterations {
				c.Set(key, i*1_000_000+j)
			}
		})
	}

	for range 8 {
		wg.Go(func() {
			for range iterations {
				c.Get(key)
			}
		})
	}

	wg.Wait()
}

func TestWithShardCountRoundsUpToPowerOfTwo(t *testing.T) {
	c := New[int, int](WithShardCount(10))
	if got := len(c.shards); got != 16 {
		t.Fatalf("shard count = %d; want 16", got)
	}
}

// TestGrowth forces a single shard through many doublings of its
// open-addressed table and verifies every key survives every resize with
// the right value, and that overwrites during growth don't duplicate slots.
func TestGrowth(t *testing.T) {
	c := New[int, int](WithShardCount(1))

	const n = 20000
	for i := range n {
		c.Set(i, i)
	}
	if got := c.Len(); got != n {
		t.Fatalf("Len() = %d; want %d", got, n)
	}
	for i := range n {
		v, ok := c.Get(i)
		if !ok || v != i {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", i, v, ok, i)
		}
	}

	// Overwrite every key; count must stay the same (no duplicate slots).
	for i := range n {
		c.Set(i, i*2)
	}
	if got := c.Len(); got != n {
		t.Fatalf("Len() after overwrite = %d; want %d", got, n)
	}
	for i := range n {
		v, ok := c.Get(i)
		if !ok || v != i*2 {
			t.Fatalf("Get(%d) after overwrite = %d, %v; want %d, true", i, v, ok, i*2)
		}
	}
}

func TestWithCapacity(t *testing.T) {
	c := New[int, int](WithShardCount(8), WithCapacity(1000))

	for i := range 1000 {
		c.Set(i, i*2)
	}
	if got := c.Len(); got != 1000 {
		t.Fatalf("Len() = %d; want 1000", got)
	}
	for i := range 1000 {
		v, ok := c.Get(i)
		if !ok || v != i*2 {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", i, v, ok, i*2)
		}
	}
}

// TestHashCollisions verifies correctness when many keys land in the same
// shard and probe past each other (small shard count forces this even
// without deliberately colliding hashes).
func TestHashCollisions(t *testing.T) {
	c := New[string, int](WithShardCount(1))

	entries := make([]Entry[string, int], 0, 5000)
	for i := range 5000 {
		entries = append(entries, Entry[string, int]{Key: fmt.Sprintf("key-%d", i), Value: i})
	}
	c.SetMany(entries)

	for i := range 5000 {
		key := fmt.Sprintf("key-%d", i)
		v, ok := c.Get(key)
		if !ok || v != i {
			t.Fatalf("Get(%q) = %d, %v; want %d, true", key, v, ok, i)
		}
	}
	if got := c.Len(); got != 5000 {
		t.Fatalf("Len() = %d; want 5000", got)
	}
}
