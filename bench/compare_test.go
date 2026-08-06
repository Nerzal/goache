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
//   - Yiling-J/theine-go: Caffeine-inspired cache using adaptive W-TinyLFU
//     admission plus a hierarchical timer wheel for TTL. Found via research
//     into competing Go cache libraries (see docs/competitor-analysis.md);
//     publishes benchmark claims of far higher parallel read throughput than
//     ristretto. Requires a bounded MaximumSize (no unbounded mode), and its
//     Set takes an explicit per-entry cost.
//   - maypok86/otter/v2: also Caffeine-inspired (adaptive W-TinyLFU as of v2,
//     succeeding v1's modified S3-FIFO), documented by its own author as
//     long having "unbeatable throughput" among Go caches. Included for the
//     same reason as theine-go. Options-struct constructor rather than a
//     builder; supports unbounded caches directly.
//
// All benchmarks use string keys and int values (or their equivalent for
// libraries that require []byte, e.g. freecache) so the comparison exercises
// the same shape of workload as cache_bench_test.go in the main module.
//
// # Structure
//
// Every benchmark below is run at each of sizes (1k/5k/50k/100k/1M entries)
// via b.Run sub-benchmarks, e.g. `BenchmarkGoache_Set/n=50000` — see
// runSizes. Four categories are covered per library, matching goache's own
// public API surface so every feature goache has gets measured against
// every competitor that has an equivalent:
//
//   - Set / Get / ParallelGet: the baseline, unbounded/no-TTL workload.
//   - SetWithTTL / GetWithTTL: per-entry expiry. otter is the one library
//     without a per-Set TTL parameter — it configures expiry once at
//     construction (ExpiryCalculator) — so its TTL benchmarks use a
//     separately-constructed cache with a fixed write-based expiry policy
//     instead of a per-call TTL argument; see newOtterTTL's comment.
//   - Delete: measured as delete-then-reinsert churn (see deleteChurn's
//     comment) rather than isolated deletion, so the pre-populated cache
//     size stays constant across b.Loop() rather than draining to empty —
//     see docs/adr/0015-delete-clear-api.md for why an earlier isolated-
//     Delete attempt in the main module's own benchmarks hung for minutes.
//   - Bounded: only for libraries with a native size-bounded eviction
//     policy (goache's WithMaxSize, theine, otter, ristretto) — go-cache
//     has no eviction and freecache bounds by byte-buffer size rather than
//     entry count, so neither has a comparable "n/2 entry limit" mode and
//     both are skipped here (they're still covered by the unbounded Set/Get
//     benchmarks above).
package bench

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Yiling-J/theine-go"
	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto/v2"
	"github.com/maypok86/otter/v2"
	gocache "github.com/patrickmn/go-cache"

	"github.com/Nerzal/goache"
)

// sizes is the set of cache/working-set sizes every benchmark below runs
// against, via runSizes' b.Run sub-benchmarks.
var sizes = []int{1000, 5000, 50000, 100000, 1000000}

// runSizes runs fn once per entry in sizes, as a named sub-benchmark
// ("n=<size>") so a single size can be targeted directly, e.g.
// `go test -bench=BenchmarkGoache_Set/n=50000`.
func runSizes(b *testing.B, fn func(b *testing.B, n int)) {
	b.Helper()
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			fn(b, n)
		})
	}
}

func benchKeys(count int) []string {
	keys := make([]string, count)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

func encodeInt(v int) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(v))
	return buf
}

// --- goache ---

func BenchmarkGoache_Set(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.New[string, int]()
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

func BenchmarkGoache_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
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
	})
}

func BenchmarkGoache_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
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
	})
}

func BenchmarkGoache_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.New[string, int]()
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.SetWithTTL(keys[i%n], i, time.Hour)
			i++
		}
	})
}

func BenchmarkGoache_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.New[string, int]()
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
	})
}

func BenchmarkGoache_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.New[string, int]()
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
	})
}

func BenchmarkGoache_Bounded(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.New[string, int](goache.WithMaxSize(n / 2))
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

// --- patrickmn/go-cache ---

func BenchmarkGoCache_Set(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i, gocache.NoExpiration)
			i++
		}
	})
}

func BenchmarkGoCache_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
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
	})
}

func BenchmarkGoCache_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
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
	})
}

func BenchmarkGoCache_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i, time.Hour)
			i++
		}
	})
}

func BenchmarkGoCache_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, time.Hour)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkGoCache_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := gocache.New(gocache.NoExpiration, gocache.NoExpiration)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, gocache.NoExpiration)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			k := keys[i%n]
			c.Delete(k)
			c.Set(k, i, gocache.NoExpiration)
			i++
		}
	})
}

// --- coocood/freecache ---
// freecache is []byte-only, so int values are encoded with encoding/binary
// to keep the comparison fair (no reflection/JSON overhead on either side).
// Its buffer is sized generously above the working set (200 bytes/entry,
// 10MB floor) so it never evicts under memory pressure during the
// unbounded Set/Get/Delete benchmarks below — freecache has no separate
// "bounded to exactly n" mode the way theine/otter/goache do (see the
// package doc comment's note on why it's skipped from BenchmarkX_Bounded).

func freecacheBufSize(n int) int {
	size := n * 200
	const floor = 10 * 1024 * 1024
	if size < floor {
		size = floor
	}
	return size
}

func BenchmarkFreecache_Set(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			_ = c.Set([]byte(keys[i%n]), encodeInt(i), 0)
			i++
		}
	})
}

func BenchmarkFreecache_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
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
	})
}

func BenchmarkFreecache_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
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
	})
}

func BenchmarkFreecache_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			_ = c.Set([]byte(keys[i%n]), encodeInt(i), 3600)
			i++
		}
	})
}

func BenchmarkFreecache_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
		keys := benchKeys(n)
		for i, k := range keys {
			_ = c.Set([]byte(k), encodeInt(i), 3600)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			_, _ = c.Get([]byte(keys[i%n]))
			i++
		}
	})
}

func BenchmarkFreecache_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
		keys := benchKeys(n)
		for i, k := range keys {
			_ = c.Set([]byte(k), encodeInt(i), 0)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			k := keys[i%n]
			_ = c.Del([]byte(k))
			_ = c.Set([]byte(k), encodeInt(i), 0)
			i++
		}
	})
}

// --- dgraph-io/ristretto/v2 ---
// ristretto's Set is async (admission-policy controlled); Wait() is called
// after population so Get benchmarks measure steady-state hits, not races
// against the admission pipeline. NumCounters/MaxCost are sized generously
// above n so the unbounded Set/Get/Delete benchmarks don't hit real
// eviction pressure; BenchmarkRistretto_Bounded below configures a much
// tighter MaxCost specifically to force it.

func newRistretto(b *testing.B, n int) *ristretto.Cache[string, int] {
	b.Helper()
	c, err := ristretto.NewCache(&ristretto.Config[string, int]{
		NumCounters: int64(n) * 10,
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
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i, 1)
			i++
		}
	})
}

func BenchmarkRistretto_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
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
	})
}

func BenchmarkRistretto_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
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
	})
}

func BenchmarkRistretto_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.SetWithTTL(keys[i%n], i, 1, time.Hour)
			i++
		}
	})
}

func BenchmarkRistretto_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.SetWithTTL(k, i, 1, time.Hour)
		}
		c.Wait()
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkRistretto_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, 1)
		}
		c.Wait()
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			k := keys[i%n]
			c.Del(k)
			c.Set(k, i, 1)
			i++
		}
	})
}

// BenchmarkRistretto_Bounded configures MaxCost to half the working set
// (cost=1 per item) so, unlike the unbounded benchmarks above, admission is
// under real pressure and TinyLFU actively rejects/evicts on most Sets —
// the same "roughly half evicts" shape as goache's own BenchmarkSetWithMaxSize.
func BenchmarkRistretto_Bounded(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		limit := n / 2
		c, err := ristretto.NewCache(&ristretto.Config[string, int]{
			NumCounters: int64(limit) * 10,
			MaxCost:     int64(limit),
			BufferItems: 64,
		})
		if err != nil {
			b.Fatalf("ristretto.NewCache: %v", err)
		}
		b.Cleanup(c.Close)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i, 1)
			i++
		}
	})
}

// --- Yiling-J/theine-go ---
// theine requires a bounded MaximumSize up front (no unbounded constructor).
// The unbounded-equivalent benchmarks size it exactly to n (the full
// working set) so no eviction pressure occurs; BenchmarkTheine_Bounded
// below sizes it to n/2 specifically to force eviction.

func newTheine(b *testing.B, maxSize int) *theine.Cache[string, int] {
	b.Helper()
	c, err := theine.NewBuilder[string, int](int64(maxSize)).Build()
	if err != nil {
		b.Fatalf("theine.NewBuilder.Build: %v", err)
	}
	return c
}

func BenchmarkTheine_Set(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i, 1)
			i++
		}
	})
}

func BenchmarkTheine_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, 1)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkTheine_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, 1)
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
	})
}

func BenchmarkTheine_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.SetWithTTL(keys[i%n], i, 1, time.Hour)
			i++
		}
	})
}

func BenchmarkTheine_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.SetWithTTL(k, i, 1, time.Hour)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Get(keys[i%n])
			i++
		}
	})
}

func BenchmarkTheine_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, 1)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			k := keys[i%n]
			c.Delete(k)
			c.Set(k, i, 1)
			i++
		}
	})
}

// BenchmarkTheine_Bounded sizes the builder to n/2 so, unlike the
// unbounded benchmarks above (sized to exactly n), W-TinyLFU admission is
// under real pressure for the full duration of the benchmark.
func BenchmarkTheine_Bounded(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n/2)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i, 1)
			i++
		}
	})
}

// --- maypok86/otter/v2 ---

func newOtter(b *testing.B, maxSize int) *otter.Cache[string, int] {
	b.Helper()
	c, err := otter.New(&otter.Options[string, int]{
		MaximumSize: maxSize,
	})
	if err != nil {
		b.Fatalf("otter.New: %v", err)
	}
	return c
}

// newOtterTTL builds an otter cache with a fixed write-based expiry policy
// configured at construction, since otter (unlike every other library
// compared here) has no per-Set TTL parameter — TTL is a cache-wide policy,
// not a per-call argument. This is the closest architectural equivalent:
// every entry expires ttl after being written, same effective behavior as
// the other libraries' SetWithTTL(key, value, ttl) for a fixed ttl.
func newOtterTTL(b *testing.B, maxSize int, ttl time.Duration) *otter.Cache[string, int] {
	b.Helper()
	c, err := otter.New(&otter.Options[string, int]{
		MaximumSize:      maxSize,
		ExpiryCalculator: otter.ExpiryWriting[string, int](ttl),
	})
	if err != nil {
		b.Fatalf("otter.New: %v", err)
	}
	return c
}

func BenchmarkOtter_Set(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtter(b, n)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

func BenchmarkOtter_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtter(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.GetIfPresent(keys[i%n])
			i++
		}
	})
}

func BenchmarkOtter_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtter(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				c.GetIfPresent(keys[i%n])
				i++
			}
		})
	})
}

func BenchmarkOtter_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtterTTL(b, n, time.Hour)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

func BenchmarkOtter_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtterTTL(b, n, time.Hour)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.GetIfPresent(keys[i%n])
			i++
		}
	})
}

func BenchmarkOtter_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtter(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			k := keys[i%n]
			c.Invalidate(k)
			c.Set(k, i)
			i++
		}
	})
}

// BenchmarkOtter_Bounded sizes MaximumSize to n/2 so, unlike the unbounded
// benchmarks above (sized to exactly n), W-TinyLFU admission is under real
// pressure for the full duration of the benchmark.
func BenchmarkOtter_Bounded(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtter(b, n/2)
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

// --- single-core comparison ---
//
// The benchmarks above answer "which library is fastest", and their headline
// numbers are 24-core numbers. These answer a narrower question:
// docs/adr/0025-cpu-constrained-benchmarks.md measured go-cache beating
// goache at GOMAXPROCS=1, because a sharded cache pays for machinery that a
// single P returns nothing for. goache.NewSingleCore exists to close that
// gap (docs/adr/0026-single-core-cache.md), and this is where the claim is
// checked against the library it has to beat.
//
// Read them at -cpu=1 via `make bench-compare-singlecore`. At higher core
// counts SingleCoreCache's one lock is expected to lose to sharded goache —
// that is the documented trade, not a regression.
//
// Mixed read/write is the case with no prior coverage: BenchmarkGoCache_*
// above has no ParallelGetSet, so the three below are added together.
//
// SingleCoreCache covers the same seven categories every other library here
// does — Set/Get/ParallelGet/SetWithTTL/GetWithTTL/Delete/Bounded — because
// a "fastest single-core cache" claim measured on three of them is a claim
// about three of them. TTL and bounded eviction are exactly where theine and
// otter spend their engineering (timer wheels, W-TinyLFU admission), so
// leaving those unmeasured would skip the competitors' strongest ground.

func BenchmarkGoacheSingleCore_Set(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

func BenchmarkGoacheSingleCore_Get(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
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
	})
}

func BenchmarkGoacheSingleCore_ParallelGet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
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
	})
}

func BenchmarkGoacheSingleCore_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
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
	})
}

func BenchmarkGoacheSingleCore_SetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.SetWithTTL(keys[i%n], i, time.Hour)
			i++
		}
	})
}

func BenchmarkGoacheSingleCore_GetWithTTL(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
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
	})
}

func BenchmarkGoacheSingleCore_Delete(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int]()
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
	})
}

func BenchmarkGoacheSingleCore_Bounded(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := goache.NewSingleCore[string, int](goache.WithMaxSize(n / 2))
		keys := benchKeys(n)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			c.Set(keys[i%n], i)
			i++
		}
	})
}

func BenchmarkGoache_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
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
				k := keys[i%n]
				if i%10 == 0 {
					c.Set(k, i)
				} else {
					c.Get(k)
				}
				i++
			}
		})
	})
}

func BenchmarkGoCache_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
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
				k := keys[i%n]
				if i%10 == 0 {
					c.Set(k, i, gocache.NoExpiration)
				} else {
					c.Get(k)
				}
				i++
			}
		})
	})
}

// The remaining ParallelGetSet benchmarks complete the 9-read/1-write mixed
// workload across the whole field. Without them that axis compares goache
// against go-cache only, which is the weakest competitor here — a mixed-load
// claim has to survive ristretto, theine and otter too.

func BenchmarkFreecache_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := freecache.NewCache(freecacheBufSize(n))
		keys := benchKeys(n)
		for i, k := range keys {
			_ = c.Set([]byte(k), encodeInt(i), 0)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				k := keys[i%n]
				if i%10 == 0 {
					_ = c.Set([]byte(k), encodeInt(i), 0)
				} else {
					_, _ = c.Get([]byte(k))
				}
				i++
			}
		})
	})
}

func BenchmarkRistretto_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newRistretto(b, n)
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
				k := keys[i%n]
				if i%10 == 0 {
					c.Set(k, i, 1)
				} else {
					c.Get(k)
				}
				i++
			}
		})
	})
}

func BenchmarkTheine_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newTheine(b, n)
		keys := benchKeys(n)
		for i, k := range keys {
			c.Set(k, i, 1)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				k := keys[i%n]
				if i%10 == 0 {
					c.Set(k, i, 1)
				} else {
					c.Get(k)
				}
				i++
			}
		})
	})
}

func BenchmarkOtter_ParallelGetSet(b *testing.B) {
	runSizes(b, func(b *testing.B, n int) {
		c := newOtter(b, n)
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
					c.GetIfPresent(k)
				}
				i++
			}
		})
	})
}
