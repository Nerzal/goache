package goache

import (
	"strconv"
	"testing"
	"time"
)

func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

func BenchmarkSet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i)
		i++
	}
}

func BenchmarkSetMany(b *testing.B) {
	const batch = 100
	b.ReportAllocs()
	for i := 0; i < b.N; i += batch {
		n := batch
		if i+n > b.N {
			n = b.N - i
		}
		c := New[string, int]()
		entries := make([]Entry[string, int], n)
		for j := 0; j < n; j++ {
			entries[j] = Entry[string, int]{Key: strconv.Itoa(i + j), Value: i + j}
		}
		b.StartTimer()
		c.SetMany(entries)
		b.StopTimer()
	}
}

// BenchmarkSetManyRepeated / BenchmarkDeleteManyRepeated measure repeated
// bulk calls against one long-lived cache — the shape a server actually
// runs, and the one a per-Cache bucket pool can help (see
// docs/performance-analysis.md T4). BenchmarkSetMany above deliberately
// builds a fresh Cache per batch to measure cold ingestion, so it can never
// show reuse; these two complete the picture.

func BenchmarkSetManyRepeated(b *testing.B) {
	const batch = 100
	c := New[string, int]()
	entries := make([]Entry[string, int], batch)
	for j := range entries {
		entries[j] = Entry[string, int]{Key: strconv.Itoa(j), Value: j}
	}

	b.ReportAllocs()
	for b.Loop() {
		c.SetMany(entries)
	}
}

func BenchmarkDeleteManyRepeated(b *testing.B) {
	const batch = 100
	c := New[string, int]()
	keys := make([]string, batch)
	for j := range keys {
		keys[j] = strconv.Itoa(j)
		c.Set(keys[j], j)
	}

	b.ReportAllocs()
	for b.Loop() {
		c.DeleteMany(keys)
	}
}

func BenchmarkGet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkGetMiss(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	for i, k := range benchKeys(n) {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get("missing-" + strconv.Itoa(i))
		i++
	}
}

func BenchmarkParallelGetSet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%n]
			if i%10 == 0 {
				c.Set(k, i)
			} else {
				c.Get(k)
			}
			i++
		}
	})
}

// BenchmarkFreshLoad* measure bulk-loading a brand new cache from empty —
// the "ingestion" scenario (final size known upfront) that WithCapacity
// targets. Compare the two to see the effect of pre-sizing shard maps.

func BenchmarkFreshLoad_NoHint(b *testing.B) {
	const n = 10000
	entries := make([]Entry[string, int], n)
	for i := range entries {
		entries[i] = Entry[string, int]{Key: strconv.Itoa(i), Value: i}
	}

	b.ReportAllocs()
	for b.Loop() {
		c := New[string, int]()
		c.SetMany(entries)
	}
}

func BenchmarkFreshLoad_WithCapacityHint(b *testing.B) {
	const n = 10000
	entries := make([]Entry[string, int], n)
	for i := range entries {
		entries[i] = Entry[string, int]{Key: strconv.Itoa(i), Value: i}
	}

	b.ReportAllocs()
	for b.Loop() {
		c := New[string, int](WithCapacity(n))
		c.SetMany(entries)
	}
}

// BenchmarkSetWithTTL / BenchmarkGetWithTTL measure the TTL-using path —
// compare against BenchmarkSet / BenchmarkGet above to see TTL's cost when
// it's actually used (Get against a non-TTL entry should show ~zero
// difference from BenchmarkGet, since the clock is only read when the
// found entry's expiresAt is non-zero).

func BenchmarkSetWithTTL(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.SetWithTTL(keys[i%n], i, time.Hour)
		i++
	}
}

func BenchmarkGetWithTTL(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.SetWithTTL(k, i, time.Hour)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkPurge(b *testing.B) {
	const n = 100000
	keys := benchKeys(n)

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		c := New[string, int]()
		for i, k := range keys {
			c.SetWithTTL(k, i, time.Nanosecond)
		}
		time.Sleep(time.Millisecond)
		b.StartTimer()

		c.Purge()
	}
}

// BenchmarkDelete measures a single Delete call. It uses the same
// batch-rebuild pattern as BenchmarkSetMany rather than b.Loop() with a
// per-iteration full-cache rebuild: Delete itself is cheap (tens of ns), so
// b.Loop() would pick an iteration count in the millions, each paying to
// rebuild an unrelated cache — rebuilding only every `batch` iterations
// keeps that overhead amortized instead of dominating the measurement.
func BenchmarkDelete(b *testing.B) {
	const batch = 100
	b.ReportAllocs()
	for i := 0; i < b.N; i += batch {
		n := batch
		if i+n > b.N {
			n = b.N - i
		}
		c := New[string, int]()
		keys := make([]string, n)
		for j := 0; j < n; j++ {
			keys[j] = strconv.Itoa(i + j)
			c.Set(keys[j], i+j)
		}
		b.StartTimer()
		for _, k := range keys {
			c.Delete(k)
		}
		b.StopTimer()
	}
}

func BenchmarkDeleteMany(b *testing.B) {
	const batch = 100
	b.ReportAllocs()
	for i := 0; i < b.N; i += batch {
		n := batch
		if i+n > b.N {
			n = b.N - i
		}
		c := New[string, int]()
		keys := make([]string, n)
		for j := 0; j < n; j++ {
			keys[j] = strconv.Itoa(i + j)
			c.Set(keys[j], i+j)
		}
		b.StartTimer()
		c.DeleteMany(keys)
		b.StopTimer()
	}
}

// BenchmarkDeleteSetChurn measures the delete-then-reinsert cycle on an
// unbounded cache — the workload behind bench/'s cross-library Delete
// comparison. Every reinsert is a brand-new key from the map's perspective,
// so before the per-shard freelist (docs/performance-analysis.md T1) each
// iteration paid one heap allocation for a fresh entry; the freelist lets
// the entry parked by Delete be reused instead.
func BenchmarkDeleteSetChurn(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		k := keys[i%n]
		c.Delete(k)
		c.Set(k, i)
		i++
	}
}

// BenchmarkClear measures populate-then-Clear together, not Clear in
// isolation: the builtin clear() used internally is fast enough (unlike
// Purge's per-key loop) that isolating it via b.StopTimer()/b.StartTimer()
// around a per-iteration full rebuild hits the same blowup BenchmarkDelete's
// comment describes — the framework would pick an iteration count in the
// billions to accumulate enough *measured* time, each paying an *unmeasured*
// 100k-entry rebuild. Folding population into the measured portion (same
// shape as BenchmarkFreshLoad_NoHint) keeps the iteration count sane;
// Clear's own marginal cost is the difference between this number and
// BenchmarkFreshLoad_NoHint's.
func BenchmarkClear(b *testing.B) {
	const n = 100000
	entries := make([]Entry[string, int], n)
	for i := range entries {
		entries[i] = Entry[string, int]{Key: strconv.Itoa(i), Value: i}
	}

	b.ReportAllocs()
	for b.Loop() {
		c := New[string, int]()
		c.SetMany(entries)
		c.Clear()
	}
}

// BenchmarkSetWithMaxSize / BenchmarkGetWithMaxSize measure the eviction-
// tracking path against a cache that's actually bounded (WithMaxSize set to
// half the key pool, so Set is evicting on roughly every other call once
// the shard fills) — compare against BenchmarkSet/BenchmarkGet above to see
// eviction's own marginal cost on top of the pointer-storage cost every
// cache now pays (see BenchmarkSet's own numbers moving after this change,
// documented in docs/adr/0016-clock-eviction.md).

func BenchmarkSetWithMaxSize(b *testing.B) {
	c := New[string, int](WithMaxSize(50000))
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i)
		i++
	}
}

func BenchmarkGetWithMaxSize(b *testing.B) {
	const n = 100000
	c := New[string, int](WithMaxSize(n))
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

// BenchmarkEvictionChurn measures steady-state Set cost once the cache is
// already full and every new key forces exactly one eviction — the
// worst-case cost for the CLOCK sweep (as opposed to BenchmarkSetWithMaxSize
// above, where roughly half the calls are cheap overwrites).
func BenchmarkEvictionChurn(b *testing.B) {
	const limit = 10000
	c := New[string, int](WithShardCount(1), WithMaxSize(limit))
	for i := range limit {
		c.Set(strconv.Itoa(i), i)
	}

	b.ReportAllocs()
	i := limit
	for b.Loop() {
		c.Set(strconv.Itoa(i), i) // always a brand-new key: always evicts
		i++
	}
}

// BenchmarkParallelGetWithMaxSize measures concurrent Get against a
// WithMaxSize-bounded cache — the one path where every hit does an atomic
// store (the CLOCK "referenced" bit), unlike BenchmarkParallelGet's unbounded
// cache where Get never touches shared state beyond the RWMutex. This is
// where an unconditional Store(true) can cost more than a plain read under
// heavy contention on the same keys: every Store invalidates that cache line
// on every other core, even when the value doesn't change.
// BenchmarkParallelGetSetWithTTL measures mixed concurrent Get/Set against
// TTL entries specifically to see whether moving time.Now() outside Get's
// read lock (an experiment from gemini-analysis.md) reduces write-side wait
// time: a pending writer's Lock() call has to wait for every outstanding
// RLock to release, so anything a reader does while holding that RLock
// (including a time.Now() call) extends the writer's wait under contention.
// A single-threaded benchmark can't show this since the calling goroutine
// pays the same total time regardless of lock hold order.
func BenchmarkParallelGetSetWithTTL(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.SetWithTTL(k, i, time.Hour)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%n]
			if i%10 == 0 {
				c.SetWithTTL(k, i, time.Hour)
			} else {
				c.Get(k)
			}
			i++
		}
	})
}

func BenchmarkParallelGetWithMaxSize(b *testing.B) {
	const n = 100000
	c := New[string, int](WithMaxSize(n))
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(keys[i%n])
			i++
		}
	})
}

// BenchmarkEvictionChurnLarge is BenchmarkEvictionChurn with a shard an
// order of magnitude bigger, isolating how the CLOCK sweep scales with the
// number of entries it may have to walk — the cost behind goache's Bounded
// numbers growing ~5.7x from 1,000 to 1,000,000 entries in the cross-library
// comparison (see docs/adr/0016-clock-eviction.md and
// docs/performance-analysis.md T5). One shard keeps the sweep length equal
// to the entry count instead of dividing it by the shard count.
func BenchmarkEvictionChurnLarge(b *testing.B) {
	const limit = 100000
	c := New[string, int](WithShardCount(1), WithMaxSize(limit))
	for i := range limit {
		c.Set(strconv.Itoa(i), i)
	}

	b.ReportAllocs()
	i := limit
	for b.Loop() {
		c.Set(strconv.Itoa(i), i) // always a brand-new key: always evicts
		i++
	}
}

// BenchmarkEvictionChurnHot evicts against a shard where most entries were
// just read, so nearly every slot the hand passes carries a set reference
// bit and has to be cleared before anything can be evicted — CLOCK's
// worst case, and the one a contiguous bit map can clear in bulk.
func BenchmarkEvictionChurnHot(b *testing.B) {
	const limit = 100000
	c := New[string, int](WithShardCount(1), WithMaxSize(limit))
	keys := benchKeys(limit)
	for i, k := range keys {
		c.Set(k, i)
	}
	for _, k := range keys { // set every reference bit
		c.Get(k)
	}

	b.ReportAllocs()
	i := limit
	for b.Loop() {
		c.Set(strconv.Itoa(i), i)
		i++
	}
}

// The *Constrained benchmarks model the shape a Go service actually has in
// a CPU-limited container: far more concurrent request goroutines than
// cores to run them on. A Kubernetes pod with `limits.cpu: 100m` gets a
// tenth of a core, and Go 1.25+ derives GOMAXPROCS from the cgroup quota,
// so such a pod runs with GOMAXPROCS=1 while still serving dozens of
// requests at once.
//
// b.RunParallel's default spawns exactly GOMAXPROCS goroutines, which at
// -cpu=1 means a single goroutine and therefore no contention at all —
// that measures overhead, not the constrained case. SetParallelism(32)
// puts 32 goroutines per P in flight instead, so at -cpu=1 thirty-two
// goroutines contend for one core: goroutines get preempted mid-critical-
// section, and whether they collide on the same lock starts to matter
// again even without true parallelism.
//
// Run the sweep with `make bench-cpu`. See
// docs/adr/0025-cpu-constrained-benchmarks.md.

const constrainedGoroutinesPerP = 32

func BenchmarkParallelGetConstrained(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.SetParallelism(constrainedGoroutinesPerP)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkParallelGetSetConstrained(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.SetParallelism(constrainedGoroutinesPerP)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%n]
			if i%10 == 0 {
				c.Set(k, i)
			} else {
				c.Get(k)
			}
			i++
		}
	})
}

func BenchmarkParallelGet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(keys[i%n])
			i++
		}
	})
}

// The BenchmarkSingleCore* benchmarks mirror the ones above against
// SingleCoreCache. Every one has a same-named Cache counterpart on purpose:
// the pair is only meaningful read together, and only at -cpu=1, which is
// the regime SingleCoreCache exists for. Run them with `make bench-singlecore`
// and read the numbers next to the cross-library comparison in bench/. See
// docs/adr/0026-single-core-cache.md.

func BenchmarkSingleCoreSet(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i)
		i++
	}
}

func BenchmarkSingleCoreGet(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkSingleCoreGetMiss(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	for i, k := range benchKeys(n) {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get("missing-" + strconv.Itoa(i))
		i++
	}
}

func BenchmarkSingleCoreSetWithTTL(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.SetWithTTL(keys[i%n], i, time.Hour)
		i++
	}
}

func BenchmarkSingleCoreGetWithTTL(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.SetWithTTL(k, i, time.Hour)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

// BenchmarkSingleCoreSetMany uses the same fresh-cache-per-batch shape as
// BenchmarkSetMany so the two are directly comparable — this measures cold
// bulk ingestion, where SingleCoreCache skips shard grouping entirely.
func BenchmarkSingleCoreSetMany(b *testing.B) {
	const batch = 100
	b.ReportAllocs()
	for i := 0; i < b.N; i += batch {
		n := batch
		if i+n > b.N {
			n = b.N - i
		}
		c := NewSingleCore[string, int]()
		entries := make([]Entry[string, int], n)
		for j := 0; j < n; j++ {
			entries[j] = Entry[string, int]{Key: strconv.Itoa(i + j), Value: i + j}
		}
		b.StartTimer()
		c.SetMany(entries)
		b.StopTimer()
	}
}

func BenchmarkSingleCoreSetManyRepeated(b *testing.B) {
	const batch = 100
	c := NewSingleCore[string, int]()
	entries := make([]Entry[string, int], batch)
	for j := range entries {
		entries[j] = Entry[string, int]{Key: strconv.Itoa(j), Value: j}
	}

	b.ReportAllocs()
	for b.Loop() {
		c.SetMany(entries)
	}
}

func BenchmarkSingleCoreDeleteManyRepeated(b *testing.B) {
	const batch = 100
	c := NewSingleCore[string, int]()
	keys := make([]string, batch)
	for j := range keys {
		keys[j] = strconv.Itoa(j)
		c.Set(keys[j], j)
	}

	b.ReportAllocs()
	for b.Loop() {
		c.DeleteMany(keys)
	}
}

func BenchmarkSingleCoreDeleteSetChurn(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		k := keys[i%n]
		c.Delete(k)
		c.Set(k, i)
		i++
	}
}

func BenchmarkSingleCorePurge(b *testing.B) {
	const n = 100000
	keys := benchKeys(n)

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		c := NewSingleCore[string, int]()
		for i, k := range keys {
			c.SetWithTTL(k, i, time.Nanosecond)
		}
		time.Sleep(time.Millisecond)
		b.StartTimer()

		c.Purge()
	}
}

func BenchmarkSingleCoreClear(b *testing.B) {
	const n = 100000
	entries := make([]Entry[string, int], n)
	for i := range entries {
		entries[i] = Entry[string, int]{Key: strconv.Itoa(i), Value: i}
	}

	b.ReportAllocs()
	for b.Loop() {
		c := NewSingleCore[string, int]()
		c.SetMany(entries)
		c.Clear()
	}
}

func BenchmarkSingleCoreSetWithMaxSize(b *testing.B) {
	c := NewSingleCore[string, int](WithMaxSize(50000))
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i)
		i++
	}
}

func BenchmarkSingleCoreGetWithMaxSize(b *testing.B) {
	const n = 100000
	c := NewSingleCore[string, int](WithMaxSize(n))
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

// BenchmarkSingleCoreEvictionChurn* are the sweep-length probes ADR 0023
// demands before claiming the bounded path is fine here: Cache spreads its
// eviction budget over 256 CLOCK rings, SingleCoreCache has exactly one, so
// ADR 0023's "0.00-2.54 hand steps per eviction" measurement does not carry
// over. Large is an order of magnitude bigger and Hot pre-reads every key so
// the hand has to clear a set reference bit on every slot it passes — CLOCK's
// worst case. Compare against BenchmarkEvictionChurn/-Large/-Hot, which use
// WithShardCount(1) for exactly this reason.

func BenchmarkSingleCoreEvictionChurn(b *testing.B) {
	const limit = 10000
	c := NewSingleCore[string, int](WithMaxSize(limit))
	for i := range limit {
		c.Set(strconv.Itoa(i), i)
	}

	b.ReportAllocs()
	i := limit
	for b.Loop() {
		c.Set(strconv.Itoa(i), i) // always a brand-new key: always evicts
		i++
	}
}

func BenchmarkSingleCoreEvictionChurnLarge(b *testing.B) {
	const limit = 100000
	c := NewSingleCore[string, int](WithMaxSize(limit))
	for i := range limit {
		c.Set(strconv.Itoa(i), i)
	}

	b.ReportAllocs()
	i := limit
	for b.Loop() {
		c.Set(strconv.Itoa(i), i)
		i++
	}
}

func BenchmarkSingleCoreEvictionChurnHot(b *testing.B) {
	const limit = 100000
	c := NewSingleCore[string, int](WithMaxSize(limit))
	keys := benchKeys(limit)
	for i, k := range keys {
		c.Set(k, i)
	}
	for _, k := range keys { // set every reference bit
		c.Get(k)
	}

	b.ReportAllocs()
	i := limit
	for b.Loop() {
		c.Set(strconv.Itoa(i), i)
		i++
	}
}

func BenchmarkSingleCoreParallelGet(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkSingleCoreParallelGetSet(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%n]
			if i%10 == 0 {
				c.Set(k, i)
			} else {
				c.Get(k)
			}
			i++
		}
	})
}

// BenchmarkSingleCoreParallelGet*Constrained model the same
// more-goroutines-than-cores shape as their Cache counterparts — the actual
// shape of a request handler in a CPU-limited pod, which is precisely
// SingleCoreCache's target deployment.

func BenchmarkSingleCoreParallelGetConstrained(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.SetParallelism(constrainedGoroutinesPerP)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkSingleCoreParallelGetSetConstrained(b *testing.B) {
	c := NewSingleCore[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.SetParallelism(constrainedGoroutinesPerP)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%n]
			if i%10 == 0 {
				c.Set(k, i)
			} else {
				c.Get(k)
			}
			i++
		}
	})
}

// BenchmarkCacherDispatch* price the Cacher interface: identical work, once
// through the concrete type and once through the interface. The gap is what
// a caller pays for choosing an implementation at run time, and the reason
// New/NewSingleCore return concrete types. pickSingleCore is never true, but
// the compiler cannot prove it, so the interface variable genuinely has two
// possible dynamic types and the call cannot be devirtualized.

var pickSingleCore = false

func BenchmarkCacherDispatchDirect(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkCacherDispatchViaInterface(b *testing.B) {
	var c Cacher[string, int] = New[string, int]()
	if pickSingleCore {
		c = NewSingleCore[string, int]()
	}
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}
