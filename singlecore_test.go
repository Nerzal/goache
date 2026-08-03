package goache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// The tests below mirror cache_test.go's coverage against SingleCoreCache.
// They are deliberately duplicated rather than shared through the Cacher
// interface: the point of a second implementation is that it does not share
// code with the first, so a test that exercised both through one interface
// would only prove the interface is satisfied, not that each type's own
// storage, ring and freelist bookkeeping is correct.

func TestSingleCoreSetGet(t *testing.T) {
	c := NewSingleCore[string, int]()

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
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() after overwrite = %d; want 1", got)
	}
}

func TestSingleCoreSetMany(t *testing.T) {
	c := NewSingleCore[int, string]()

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

func TestSingleCoreSetManyEmpty(t *testing.T) {
	c := NewSingleCore[int, int]()
	c.SetMany(nil)
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d; want 0", got)
	}
}

func TestSingleCoreGrowth(t *testing.T) {
	c := NewSingleCore[int, int]()

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

func TestSingleCoreWithCapacity(t *testing.T) {
	c := NewSingleCore[int, int](WithCapacity(1000))

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

// TestSingleCoreIgnoresShardCount pins that an Option set built for New can
// be handed to NewSingleCore unchanged: WithShardCount has no meaning here
// and must be silently ignored, not panic or misconfigure anything.
func TestSingleCoreIgnoresShardCount(t *testing.T) {
	c := NewSingleCore[int, int](WithShardCount(4096), WithCapacity(10))
	for i := range 100 {
		c.Set(i, i)
	}
	if got := c.Len(); got != 100 {
		t.Fatalf("Len() = %d; want 100", got)
	}
}

func TestSingleCoreSetWithTTL(t *testing.T) {
	c := NewSingleCore[string, int]()

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

func TestSingleCoreSetWithTTLNonPositiveMeansNoExpiry(t *testing.T) {
	c := NewSingleCore[string, int]()

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

func TestSingleCoreSetManyWithMixedTTL(t *testing.T) {
	c := NewSingleCore[string, int]()

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

// TestSingleCoreOverwriteClearsTTL pins that a plain Set on a key that
// previously had a TTL drops the deadline — the entry object is reused in
// place, so a stale expiresAt would make the key expire out from under the
// caller.
func TestSingleCoreOverwriteClearsTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"unbounded", nil},
		{"bounded", []Option{WithMaxSize(8)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewSingleCore[string, int](tc.opts...)
			c.SetWithTTL("k", 1, 10*time.Millisecond)
			c.Set("k", 2)

			time.Sleep(50 * time.Millisecond)

			if v, ok := c.Get("k"); !ok || v != 2 {
				t.Fatalf("Get(k) = %d, %v; want 2, true (TTL must be cleared by Set)", v, ok)
			}
		})
	}
}

func TestSingleCorePurge(t *testing.T) {
	c := NewSingleCore[string, int]()

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
	if got := c.Purge(); got != 0 {
		t.Fatalf("second Purge() = %d; want 0", got)
	}
}

func TestSingleCoreDelete(t *testing.T) {
	c := NewSingleCore[string, int]()

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

	c.Delete("a")
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() after second Delete = %d; want 1", got)
	}
}

// TestSingleCoreDeleteParksEntryForReuse is the SingleCoreCache counterpart
// of TestDeleteParksEntryForReuse: the deleted entry goes into the one-slot
// freelist and the next brand-new key reuses that exact allocation, carrying
// no state from its previous life.
func TestSingleCoreDeleteParksEntryForReuse(t *testing.T) {
	c := NewSingleCore[string, int]()

	c.SetWithTTL("old", 1, time.Hour)
	old := c.data["old"]
	c.Delete("old")
	if c.freeData != old {
		t.Fatalf("freeData after Delete = %p; want the deleted entry %p", c.freeData, old)
	}

	c.Set("new", 2)
	if c.freeData != nil {
		t.Fatalf("freeData after reuse = %p; want nil", c.freeData)
	}
	if got := c.data["new"]; got != old {
		t.Fatalf("new key's entry = %p; want reused parked entry %p", got, old)
	}
	if e := c.data["new"]; e.expiresAt != 0 {
		t.Fatalf("reused entry expiresAt = %d; want 0 (no inherited TTL)", e.expiresAt)
	}
	if v, ok := c.Get("new"); !ok || v != 2 {
		t.Fatalf("Get(new) = %d, %v; want 2, true", v, ok)
	}

	// Overwriting an existing key must not consume the freelist.
	c.Delete("new")
	parked := c.freeData
	c.Set("other", 3)
	c.Set("other", 4)
	if c.freeData != nil || parked == nil {
		t.Fatalf("freelist after overwrite: free=%p parked=%p; want consumed exactly once", c.freeData, parked)
	}
}

// TestSingleCoreBoundedRecyclesEntry pins the same reuse on the bounded path,
// where every insert past the limit frees exactly one entry and immediately
// needs one — the 1:1 shape the single-slot freelist is chosen for.
func TestSingleCoreBoundedRecyclesEntry(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)

	evicted := c.clock[1] // oldest, untouched: next eviction victim
	c.Set(4, 4)           // full: evicts key 1, then must reuse its entry

	if got := c.clock[4]; got != evicted {
		t.Fatalf("entry for key 4 = %p; want the recycled evicted entry %p", got, evicted)
	}
	if c.freeRing != nil {
		t.Fatalf("freeRing after reuse = %p; want nil", c.freeRing)
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}
}

func TestSingleCoreDeleteMany(t *testing.T) {
	c := NewSingleCore[int, string]()

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

func TestSingleCoreDeleteManyEmpty(t *testing.T) {
	c := NewSingleCore[int, int]()
	c.Set(1, 1)
	c.DeleteMany(nil)
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d; want 1", got)
	}
}

func TestSingleCoreClear(t *testing.T) {
	c := NewSingleCore[int, int]()

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

	c.Set(42, 99)
	if v, ok := c.Get(42); !ok || v != 99 {
		t.Fatalf("Get(42) after Clear+Set = %d, %v; want 99, true", v, ok)
	}
}

func TestSingleCoreWithMaxSizeIsAHardUpperBound(t *testing.T) {
	for _, maxSize := range []int{1, 7, 100, 500, 1000, 12345} {
		c := NewSingleCore[int, int](WithMaxSize(maxSize))
		for i := range maxSize * 20 {
			c.Set(i, i)
		}
		if got := c.Len(); got > maxSize {
			t.Fatalf("maxSize=%d: Len() = %d; want <= %d", maxSize, got, maxSize)
		}
	}
}

// TestSingleCoreWithMaxSizeFillsToTheBound is the half of the bound that
// sharded Cache cannot promise: with one map there is no per-shard skew, so a
// bounded single-core cache reaches exactly its limit, never settling below.
func TestSingleCoreWithMaxSizeFillsToTheBound(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(100))
	for i := range 1000 {
		c.Set(i, i)
	}
	if got := c.Len(); got != 100 {
		t.Fatalf("Len() = %d; want exactly 100", got)
	}
}

func TestSingleCoreWithMaxSizeEvictsWhenFull(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}

	// None of the three have been read, so CLOCK degenerates to FIFO: the
	// oldest insert, key 1, goes first.
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

func TestSingleCoreCLOCKProtectsRecentlyUsed(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)

	if _, ok := c.Get(1); !ok {
		t.Fatalf("Get(1) before overflow: want hit")
	}

	c.Set(4, 4) // must evict key 2 (oldest untouched), not key 1

	if _, ok := c.Get(1); !ok {
		t.Fatalf("Get(1) after overflow: want hit (protected by CLOCK second chance)")
	}
	if _, ok := c.Get(2); ok {
		t.Fatalf("Get(2) after overflow: found, want evicted")
	}
	for _, k := range []int{3, 4} {
		if v, ok := c.Get(k); !ok || v != k {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", k, v, ok, k)
		}
	}
}

func TestSingleCoreWithMaxSizeZeroMeansUnbounded(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(0))

	for i := range 1000 {
		c.Set(i, i)
	}
	if got := c.Len(); got != 1000 {
		t.Fatalf("Len() = %d; want 1000 (no eviction without WithMaxSize)", got)
	}
	if c.clock != nil {
		t.Fatalf("clock map allocated for an unbounded cache")
	}
}

func TestSingleCoreWithMaxSizeOverwriteDoesNotEvict(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)

	c.Set(1, 100)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after overwrite = %d; want 3", got)
	}
	if v, ok := c.Get(1); !ok || v != 100 {
		t.Fatalf("Get(1) = %d, %v; want 100, true", v, ok)
	}
	for _, k := range []int{2, 3} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("Get(%d) after overwrite of key 1: missing, want present", k)
		}
	}
}

func TestSingleCoreWithMaxSizeDeleteUpdatesRing(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)
	c.Delete(2)

	if got := c.Len(); got != 2 {
		t.Fatalf("Len() after Delete = %d; want 2", got)
	}

	c.Set(4, 4)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}
	for _, k := range []int{1, 3, 4} {
		if v, ok := c.Get(k); !ok || v != k {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", k, v, ok, k)
		}
	}

	c.Set(5, 5)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after second overflow = %d; want 3", got)
	}
}

// TestSingleCoreDeleteEveryEntryThenRefill drains the ring completely — the
// case where the hand must end up nil rather than dangling at a removed
// entry — and then refills past the limit to prove the ring rebuilt cleanly.
func TestSingleCoreDeleteEveryEntryThenRefill(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(4))

	for i := range 4 {
		c.Set(i, i)
	}
	for i := range 4 {
		c.Delete(i)
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() after deleting everything = %d; want 0", got)
	}
	if c.hand != nil {
		t.Fatalf("hand after draining the ring = %p; want nil", c.hand)
	}

	for i := 10; i < 20; i++ {
		c.Set(i, i)
	}
	if got := c.Len(); got != 4 {
		t.Fatalf("Len() after refill past the limit = %d; want 4", got)
	}
}

func TestSingleCoreWithMaxSizeClearResetsRing(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(3))

	c.Set(1, 1)
	c.Set(2, 2)
	c.Set(3, 3)
	c.Clear()

	if got := c.Len(); got != 0 {
		t.Fatalf("Len() after Clear = %d; want 0", got)
	}
	if c.hand != nil {
		t.Fatalf("hand after Clear = %p; want nil", c.hand)
	}

	c.Set(10, 10)
	c.Set(20, 20)
	c.Set(30, 30)
	c.Set(40, 40)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() after refill+overflow = %d; want 3", got)
	}
}

func TestSingleCoreWithMaxSizePurgeUpdatesRing(t *testing.T) {
	c := NewSingleCore[string, int](WithMaxSize(3))

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

	c.Set("c", 4)
	c.Set("d", 5)
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3", got)
	}
}

// TestSingleCoreHashCollisions is the counterpart of TestHashCollisions: with
// no shard routing at all, every key necessarily lands in the same map and
// probes past every other one.
func TestSingleCoreHashCollisions(t *testing.T) {
	c := NewSingleCore[string, int]()

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

// --- concurrency ---
//
// SingleCoreCache targets GOMAXPROCS=1, but it must stay correct anywhere:
// goroutines interleave via preemption even on one core, and nothing stops a
// caller from running this type on 24. These run under -race like their
// Cache counterparts.

func TestSingleCoreConcurrentSetGet(t *testing.T) {
	c := NewSingleCore[int, int]()

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

func TestSingleCoreConcurrentMixedReadWrite(t *testing.T) {
	c := NewSingleCore[int, int]()
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

func TestSingleCoreConcurrentTTLAndPurge(t *testing.T) {
	c := NewSingleCore[int, int]()
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

func TestSingleCoreConcurrentBulkOps(t *testing.T) {
	c := NewSingleCore[int, int]()
	const goroutines = 16
	const perGoroutine = 500

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			entries := make([]Entry[int, int], perGoroutine)
			keys := make([]int, perGoroutine)
			for i := range perGoroutine {
				key := g*perGoroutine + i
				entries[i] = Entry[int, int]{Key: key, Value: key * 2}
				keys[i] = key
			}
			for range 20 {
				c.SetMany(entries)
				c.DeleteMany(keys)
				c.SetMany(entries)
			}
		})
	}
	wg.Wait()

	for g := range goroutines {
		for i := range perGoroutine {
			key := g*perGoroutine + i
			v, ok := c.Get(key)
			if !ok || v != key*2 {
				t.Fatalf("Get(%d) = %d, %v; want %d, true", key, v, ok, key*2)
			}
		}
	}
	if got, want := c.Len(), goroutines*perGoroutine; got != want {
		t.Fatalf("Len() = %d; want %d", got, want)
	}
}

// TestSingleCoreConcurrentEvictionChurn is the ring-corruption hunt: a tiny
// limit so nearly every Set evicts, hammered with concurrent
// Set/Get/Delete/Purge/Clear. Correctness here is "never panics, race
// detector stays clean, Len never exceeds the bound".
func TestSingleCoreConcurrentEvictionChurn(t *testing.T) {
	c := NewSingleCore[int, int](WithMaxSize(20))

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

	for i := range 50 {
		c.Set(i, i)
	}
	if got := c.Len(); got > 20 {
		t.Fatalf("Len() after post-Clear refill = %d; want <= 20 (bounded)", got)
	}
}
