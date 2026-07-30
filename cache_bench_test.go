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
