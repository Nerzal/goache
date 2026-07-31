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
