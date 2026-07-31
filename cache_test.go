package goache

import (
	"fmt"
	"sync"
	"testing"
	"time"
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

func TestConcurrentTTLAndPurge(t *testing.T) {
	c := New[int, int](WithShardCount(16))
	const goroutines = 8
	const iterations = 2000

	var wg sync.WaitGroup

	for g := range goroutines {
		wg.Go(func() {
			for i := range iterations {
				key := g*iterations + i
				if i%2 == 0 {
					c.SetWithTTL(key, key, time.Millisecond)
				} else {
					c.Set(key, key)
				}
			}
		})
	}

	for range goroutines {
		wg.Go(func() {
			for i := range iterations {
				c.Get(i)
			}
		})
	}

	for range 4 {
		wg.Go(func() {
			for range iterations {
				c.Purge()
			}
		})
	}

	wg.Wait()
}

func TestConcurrentDeleteAndSet(t *testing.T) {
	c := New[int, int](WithShardCount(16))
	const goroutines = 8
	const iterations = 2000

	var wg sync.WaitGroup

	for g := range goroutines {
		wg.Go(func() {
			for i := range iterations {
				key := g*iterations + i
				c.Set(key, key)
			}
		})
	}

	for range goroutines {
		wg.Go(func() {
			for i := range iterations {
				c.Delete(i)
			}
		})
	}

	for range goroutines {
		wg.Go(func() {
			for i := range iterations {
				c.Get(i)
			}
		})
	}

	wg.Wait()
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

func TestSetWithTTL(t *testing.T) {
	c := New[string, int]()

	c.SetWithTTL("short", 1, 10*time.Millisecond)
	c.Set("forever", 2)

	if v, ok := c.Get("short"); !ok || v != 1 {
		t.Fatalf("Get(short) before expiry = %d, %v; want 1, true", v, ok)
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := c.Get("short"); ok {
		t.Fatalf("Get(short) after expiry: found, want miss")
	}
	if v, ok := c.Get("forever"); !ok || v != 2 {
		t.Fatalf("Get(forever) = %d, %v; want 2, true (no TTL set)", v, ok)
	}
}

func TestSetWithTTLNonPositiveMeansNoExpiry(t *testing.T) {
	c := New[string, int]()

	c.SetWithTTL("zero", 1, 0)
	c.SetWithTTL("negative", 2, -time.Second)

	time.Sleep(10 * time.Millisecond)

	if v, ok := c.Get("zero"); !ok || v != 1 {
		t.Fatalf("Get(zero) = %d, %v; want 1, true", v, ok)
	}
	if v, ok := c.Get("negative"); !ok || v != 2 {
		t.Fatalf("Get(negative) = %d, %v; want 2, true", v, ok)
	}
}

func TestSetManyWithMixedTTL(t *testing.T) {
	c := New[string, int]()

	c.SetMany([]Entry[string, int]{
		{Key: "short", Value: 1, TTL: 10 * time.Millisecond},
		{Key: "forever", Value: 2},
	})

	time.Sleep(50 * time.Millisecond)

	if _, ok := c.Get("short"); ok {
		t.Fatalf("Get(short) after expiry: found, want miss")
	}
	if v, ok := c.Get("forever"); !ok || v != 2 {
		t.Fatalf("Get(forever) = %d, %v; want 2, true", v, ok)
	}
}

func TestPurge(t *testing.T) {
	c := New[string, int](WithShardCount(4))

	c.SetWithTTL("short-a", 1, 10*time.Millisecond)
	c.SetWithTTL("short-b", 2, 10*time.Millisecond)
	c.Set("forever", 3)

	if got := c.Len(); got != 3 {
		t.Fatalf("Len() before purge = %d; want 3", got)
	}

	time.Sleep(50 * time.Millisecond)

	if got := c.Purge(); got != 2 {
		t.Fatalf("Purge() = %d; want 2", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() after purge = %d; want 1", got)
	}
	if v, ok := c.Get("forever"); !ok || v != 3 {
		t.Fatalf("Get(forever) after purge = %d, %v; want 3, true", v, ok)
	}

	// Purging again finds nothing left to remove.
	if got := c.Purge(); got != 0 {
		t.Fatalf("second Purge() = %d; want 0", got)
	}
}

func TestDelete(t *testing.T) {
	c := New[string, int]()

	c.Delete("missing") // no-op, must not panic

	c.Set("a", 1)
	c.Set("b", 2)
	c.Delete("a")

	if _, ok := c.Get("a"); ok {
		t.Fatalf("Get(a) after Delete: found, want miss")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("Get(b) = %d, %v; want 2, true", v, ok)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d; want 1", got)
	}

	c.Delete("a") // deleting again is a no-op
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() after second Delete = %d; want 1", got)
	}
}

func TestDeleteMany(t *testing.T) {
	c := New[int, string]()

	entries := make([]Entry[int, string], 0, 100)
	for i := range 100 {
		entries = append(entries, Entry[int, string]{Key: i, Value: fmt.Sprintf("v%d", i)})
	}
	c.SetMany(entries)

	toDelete := make([]int, 0, 50)
	for i := range 100 {
		if i%2 == 0 {
			toDelete = append(toDelete, i)
		}
	}
	c.DeleteMany(toDelete)

	if got := c.Len(); got != 50 {
		t.Fatalf("Len() = %d; want 50", got)
	}
	for i := range 100 {
		v, ok := c.Get(i)
		if i%2 == 0 {
			if ok {
				t.Fatalf("Get(%d) after DeleteMany: found (%q), want miss", i, v)
			}
			continue
		}
		want := fmt.Sprintf("v%d", i)
		if !ok || v != want {
			t.Fatalf("Get(%d) = %q, %v; want %q, true", i, v, ok, want)
		}
	}
}

func TestDeleteManyEmpty(t *testing.T) {
	c := New[int, int]()
	c.Set(1, 1)
	c.DeleteMany(nil)
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d; want 1", got)
	}
}

func TestClear(t *testing.T) {
	c := New[int, int](WithShardCount(8))

	for i := range 1000 {
		c.Set(i, i*2)
	}
	if got := c.Len(); got != 1000 {
		t.Fatalf("Len() before Clear = %d; want 1000", got)
	}

	c.Clear()

	if got := c.Len(); got != 0 {
		t.Fatalf("Len() after Clear = %d; want 0", got)
	}
	for i := range 1000 {
		if _, ok := c.Get(i); ok {
			t.Fatalf("Get(%d) after Clear: found, want miss", i)
		}
	}

	// Cache must still be usable after Clear.
	c.Set(42, 99)
	if v, ok := c.Get(42); !ok || v != 99 {
		t.Fatalf("Get(42) after Clear+Set = %d, %v; want 99, true", v, ok)
	}
}

func TestWithMaxSizeEvictsWhenFull(t *testing.T) {
	// Single shard so eviction behavior is deterministic and not spread
	// across independently-limited shards.
	c := New[int, int](WithShardCount(1), WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}

	// Shard is full; inserting a 4th key must evict exactly one existing
	// entry (none of them have been Get, so CLOCK falls back to FIFO here:
	// oldest inserted, key 1, goes first).
	c.Set(4, 4)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after overflow = %d; want 3 (bounded)", got)
	}
	if _, ok := c.Get(1); ok {
		t.Fatalf("Get(1) after overflow: found, want evicted")
	}
	for _, k := range []int{2, 3, 4} {
		if v, ok := c.Get(k); !ok || v != k {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", k, v, ok, k)
		}
	}
}

// TestWithMaxSizeCLOCKProtectsRecentlyUsed verifies the CLOCK second-chance
// behavior: an entry that was Get after insertion survives an eviction that
// would otherwise take it, at the cost of evicting something else instead.
func TestWithMaxSizeCLOCKProtectsRecentlyUsed(t *testing.T) {
	c := New[int, int](WithShardCount(1), WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)

	// Touch key 1 so its referenced bit is set before the shard fills up
	// further — CLOCK must give it a second chance instead of evicting it
	// immediately.
	if _, ok := c.Get(1); !ok {
		t.Fatalf("Get(1) before overflow: want hit")
	}

	c.Set(4, 4) // shard full; must evict key 2 (oldest untouched), not key 1

	if _, ok := c.Get(1); !ok {
		t.Fatalf("Get(1) after overflow: want hit (protected by CLOCK second chance)")
	}
	if _, ok := c.Get(2); ok {
		t.Fatalf("Get(2) after overflow: found, want evicted (untouched, oldest remaining)")
	}
	for _, k := range []int{3, 4} {
		if v, ok := c.Get(k); !ok || v != k {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", k, v, ok, k)
		}
	}
}

func TestWithMaxSizeZeroMeansUnbounded(t *testing.T) {
	c := New[int, int](WithShardCount(1))

	for i := range 1000 {
		c.Set(i, i)
	}
	if got := c.Len(); got != 1000 {
		t.Fatalf("Len() = %d; want 1000 (no eviction without WithMaxSize)", got)
	}
}

func TestWithMaxSizeOverwriteDoesNotEvict(t *testing.T) {
	c := New[int, int](WithShardCount(1), WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)

	// Overwriting an existing key must never trigger eviction — the shard
	// isn't gaining a new key.
	c.Set(1, 100)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after overwrite = %d; want 3", got)
	}
	if v, ok := c.Get(1); !ok || v != 100 {
		t.Fatalf("Get(1) = %d, %v; want 100, true", v, ok)
	}
	if _, ok := c.Get(2); !ok {
		t.Fatalf("Get(2) after overwrite of key 1: found missing, want present")
	}
	if _, ok := c.Get(3); !ok {
		t.Fatalf("Get(3) after overwrite of key 1: found missing, want present")
	}
}

func TestWithMaxSizeDeleteUpdatesRing(t *testing.T) {
	c := New[int, int](WithShardCount(1), WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)
	c.Delete(2)

	if got := c.Len(); got != 2 {
		t.Fatalf("Len() after Delete = %d; want 2", got)
	}

	// The shard has room again; adding two more keys must not evict
	// anything (ring bookkeeping after Delete must stay consistent).
	c.Set(4, 4)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}
	for _, k := range []int{1, 3, 4} {
		if v, ok := c.Get(k); !ok || v != k {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", k, v, ok, k)
		}
	}

	c.Set(5, 5) // now full again; must evict exactly one
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after second overflow = %d; want 3", got)
	}
}

func TestWithMaxSizeClearResetsRing(t *testing.T) {
	c := New[int, int](WithShardCount(1), WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)
	c.Clear()

	if got := c.Len(); got != 0 {
		t.Fatalf("Len() after Clear = %d; want 0", got)
	}

	// Cache must still enforce its limit correctly after Clear.
	c.Set(10, 10)
	c.Set(20, 20)
	c.Set(30, 30)
	c.Set(40, 40)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after refill+overflow = %d; want 3", got)
	}
}

func TestWithMaxSizePurgeUpdatesRing(t *testing.T) {
	c := New[string, int](WithShardCount(1), WithMaxSize(3))

	c.SetWithTTL("short", 1, 10*time.Millisecond)
	c.Set("a", 2)
	c.Set("b", 3)

	time.Sleep(50 * time.Millisecond)
	if got := c.Purge(); got != 1 {
		t.Fatalf("Purge() = %d; want 1", got)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len() after Purge = %d; want 2", got)
	}

	// Ring bookkeeping after Purge must stay consistent: filling back up to
	// the limit and beyond must still evict exactly one entry, not corrupt
	// the ring.
	c.Set("c", 4)
	c.Set("d", 5)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}
}

func TestConcurrentSetGetWithMaxSize(t *testing.T) {
	c := New[int, int](WithShardCount(8), WithMaxSize(200))

	const goroutines = 16
	const iterations = 5000

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range iterations {
				key := (g*iterations + i) % 500
				c.Set(key, key)
				c.Get(key)
			}
		})
	}
	wg.Wait()

	if got := c.Len(); got > 200 {
		t.Fatalf("Len() = %d; want <= 200 (bounded)", got)
	}
}

// TestConcurrentEvictionChurn hammers a tiny-limit cache (forcing eviction
// on nearly every Set) with concurrent Set/Get/Delete/Purge/Clear — the
// combination most likely to corrupt the CLOCK ring if evict/deleteEntry/
// Clear's bookkeeping has a bug, since nearly every operation mutates the
// ring under this workload. Correctness here is "never panics, race
// detector stays clean, Len never exceeds the bound" rather than exact
// eviction order (which isn't deterministic under concurrent access).
func TestConcurrentEvictionChurn(t *testing.T) {
	c := New[int, int](WithShardCount(4), WithMaxSize(20))

	const goroutines = 16
	const iterations = 3000

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range iterations {
				key := (g*iterations + i) % 100
				switch i % 5 {
				case 0:
					c.Delete(key)
				case 1:
					c.SetWithTTL(key, key, time.Millisecond)
				default:
					c.Set(key, key)
				}
				c.Get(key)
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for range 200 {
				c.Purge()
			}
		})
	}
	wg.Wait()

	if got := c.Len(); got > 20 {
		t.Fatalf("Len() = %d; want <= 20 (bounded)", got)
	}

	c.Clear()
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() after Clear = %d; want 0", got)
	}

	// Cache must still be fully usable after this much churn plus Clear.
	for i := range 50 {
		c.Set(i, i)
	}
	if got := c.Len(); got > 20 {
		t.Fatalf("Len() after post-Clear refill = %d; want <= 20 (bounded)", got)
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
