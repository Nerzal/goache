// Package bench compares goache against other well-known Go cache libraries
// under equivalent workloads. It lives in its own module (see go.mod) so
// these comparison-only dependencies never leak into goache's own go.mod —
// consumers of goache should not have to pull in freecache/ristretto/etc.
// just to use the cache.
//
// Libraries compared, and why each was picked:
//
//   - patrickmn/go-cache: the "naive" baseline. Single global RWMutex, values
//     stored as interface{} (boxed). Represents the approach goache's
//     sharding + generics were explicitly designed to beat.
//   - coocood/freecache: closest architectural relative to goache — sharded,
//     lock-striped, built specifically to minimize GC pressure. Keys/values
//     are []byte only (no generics), so it pre-dates Go generics and avoids
//     GC pressure by keeping entries off the Go heap (in ring buffers)
//     entirely, a step further than goache currently goes.
//   - dgraph-io/ristretto/v2: industry-standard high-performance cache with
//     an admission-policy (TinyLFU) and async writes through an internal
//     ring buffer. Represents the "state of the art" general-purpose cache;
//     its Set is fire-and-forget (may be dropped by the admission policy),
//     which is a materially different contract than goache's synchronous,
//     always-succeeds Set.
//
// All benchmarks use string keys and int values (or their equivalent for
// libraries that require []byte, e.g. freecache) so the comparison exercises
// the same shape of workload as cache_bench_test.go in the main module.
package bench

import (
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto/v2"
	gocache "github.com/patrickmn/go-cache"

	"github.com/Nerzal/goache"
)

const n = 100000

func benchKeys(count int) []string {
	keys := make([]string, count)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

// --- goache ---

func BenchmarkGoache_Set(b *testing.B) {
	c := goache.New[string, int]()
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i)
		i++
	}
}

func BenchmarkGoache_Get(b *testing.B) {
	c := goache.New[string, int]()
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

func BenchmarkGoache_ParallelGet(b *testing.B) {
	c := goache.New[string, int]()
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

// --- patrickmn/go-cache ---

func BenchmarkGoCache_Set(b *testing.B) {
	c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i, gocache.NoExpiration)
		i++
	}
}

func BenchmarkGoCache_Get(b *testing.B) {
	c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i, gocache.NoExpiration)
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkGoCache_ParallelGet(b *testing.B) {
	c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i, gocache.NoExpiration)
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

// --- coocood/freecache ---
// freecache is []byte-only, so int values are encoded with encoding/binary
// to keep the comparison fair (no reflection/JSON overhead on either side).

func encodeInt(v int) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(v))
	return buf
}

func BenchmarkFreecache_Set(b *testing.B) {
	c := freecache.NewCache(100 * 1024 * 1024)
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		_ = c.Set([]byte(keys[i%n]), encodeInt(i), 0)
		i++
	}
}

func BenchmarkFreecache_Get(b *testing.B) {
	c := freecache.NewCache(100 * 1024 * 1024)
	keys := benchKeys(n)
	for i, k := range keys {
		_ = c.Set([]byte(k), encodeInt(i), 0)
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		_, _ = c.Get([]byte(keys[i%n]))
		i++
	}
}

func BenchmarkFreecache_ParallelGet(b *testing.B) {
	c := freecache.NewCache(100 * 1024 * 1024)
	keys := benchKeys(n)
	for i, k := range keys {
		_ = c.Set([]byte(k), encodeInt(i), 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = c.Get([]byte(keys[i%n]))
			i++
		}
	})
}

// --- dgraph-io/ristretto/v2 ---
// ristretto's Set is async (admission-policy controlled); Wait() is called
// after population so Get benchmarks measure steady-state hits, not races
// against the admission pipeline.

func newRistretto(b *testing.B) *ristretto.Cache[string, int] {
	b.Helper()
	c, err := ristretto.NewCache(&ristretto.Config[string, int]{
		NumCounters: 1e7,
		MaxCost:     1 << 30,
		BufferItems: 64,
	})
	if err != nil {
		b.Fatalf("ristretto.NewCache: %v", err)
	}
	b.Cleanup(c.Close)
	return c
}

func BenchmarkRistretto_Set(b *testing.B) {
	c := newRistretto(b)
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i, 1)
		i++
	}
}

func BenchmarkRistretto_Get(b *testing.B) {
	c := newRistretto(b)
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i, 1)
	}
	c.Wait()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkRistretto_ParallelGet(b *testing.B) {
	c := newRistretto(b)
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i, 1)
	}
	c.Wait()
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
