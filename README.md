# goache

Fast golang based cache system. Generic, sharded, goroutine-safe. Optimized for low latency, zero-allocation hot paths, and low memory overhead.

## Install

```
go get github.com/Nerzal/goache
```

## Usage

```go
c := goache.New[string, int]()

c.Set("a", 1)
v, ok := c.Get("a") // 1, true

c.SetMany([]goache.Entry[string, int]{
    {Key: "b", Value: 2},
    {Key: "c", Value: 3},
})

// Know roughly how many items you're loading upfront? Pre-size to skip
// Go's incremental map growth entirely — see Benchmarks below.
c2 := goache.New[string, int](goache.WithCapacity(10000))

// Entries can optionally expire. Plain Set/Get never touch the clock —
// only entries actually given a TTL pay for one, see Benchmarks below.
c.SetWithTTL("session-token", "abc123", 5*time.Minute)

c.SetMany([]goache.Entry[string, int]{
    {Key: "d", Value: 4, TTL: time.Minute}, // expires in a minute
    {Key: "e", Value: 5},                   // no TTL: TTL zero value = never expires
})

// Expired entries are hidden by Get automatically, but not reclaimed until
// something overwrites the key or Purge is called — goache runs no
// background goroutines of its own. Call this periodically if you use TTLs
// and want expired keys' memory reclaimed promptly.
removed := c.Purge()

// Remove entries explicitly, single or bulk.
c.Delete("a")
c.DeleteMany([]string{"b", "c"})

// Drop everything at once; the cache is still usable afterwards.
c.Clear()

// Bound the cache to at most n entries instead of growing forever. Once a
// shard is full, Set evicts via CLOCK (a second-chance approximation of
// LRU) — see Benchmarks below and docs/adr/0016-clock-eviction.md for why
// CLOCK and what it costs.
c3 := goache.New[string, int](goache.WithMaxSize(100_000))
```

## Architecture

`Cache[K comparable, V any]` shards keys across N independently-locked segments (`sync.RWMutex` per shard, `hash/maphash.Comparable` for routing) instead of one global lock or `sync.Map`. See the package doc comment in `cache.go` for the full reasoning, including why Go's experimental `arena` package was evaluated and rejected for this phase, and a log of optimizations that were tried and measured — some kept (cache-line-padded contiguous shard storage, entry recycling on bounded shards), several reverted and documented so they aren't re-attempted blind (a custom open-addressed per-shard table replacing Go's map, and three more in [docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md)).

Phase 1 scope: `Set`, `SetMany`, `Get`, `Delete`, `DeleteMany`, `Clear`, `WithCapacity` (pre-sizing). Phase 2: optional per-entry TTL via `SetWithTTL`/`Entry.TTL`, lazily enforced in `Get`, reclaimed via the caller-driven `Purge` — no background goroutine (see docs/adr/0011) — plus bounded automatic eviction via `WithMaxSize`, using a per-shard CLOCK (second-chance) policy chosen specifically so `Get` never needs a write lock to track recency (see docs/adr/0016). Remaining roadmap items (loading cache, stampede protection, stats) are tracked in [docs/roadmap.md](docs/roadmap.md).

## Benchmarks

Run: `make bench` (or `go test -bench=. -benchmem -run=^$ ./...`). Charts below are regenerated with `make charts` after updating the numbers in this file — see `docs/benchcharts/main.go`.

Last measured on AMD Ryzen AI 9 HX 370 (24 threads), Go 1.26.2:

```
BenchmarkSet-24               37476438    31.75 ns/op     0 B/op   0 allocs/op
BenchmarkSetMany-24           12227479    98.16 ns/op    340 B/op   2 allocs/op
BenchmarkGet-24               47577134    23.84 ns/op     0 B/op   0 allocs/op
BenchmarkGetMiss-24           21062607    56.86 ns/op     7 B/op   0 allocs/op
BenchmarkParallelGetSet-24   191022606     6.160 ns/op    0 B/op   0 allocs/op
BenchmarkParallelGet-24     264300820     4.621 ns/op    0 B/op   0 allocs/op
```

`Set`/`Get` are zero-allocation on the hot path — both cycle over the same fixed 100k-key pool, so after the first pass every call overwrites an already-allocated entry rather than inserting a new one. `SetMany`'s benchmark never repeats a key, so it shows the real cost of a cold insert: 2 allocs/op (the per-call shard-bucket grouping, same as before, plus one now-unavoidable heap allocation per new entry — see "Automatic eviction" below for why entries became individually heap-allocated). `GetMiss`'s alloc is the benchmark's own key-string construction, not the cache.

`ParallelGetSet`/`ParallelGet` dropped ~15-20% from pre-padding numbers (7.366→6.160, 5.474→4.621) after padding each shard to a full cache line in a contiguous `[]shard[K, V]` slice, removing false sharing between adjacent shards' mutexes — see [docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md), which supersedes [docs/adr/0004](docs/adr/0004-shard-storage-pointer-slice.md)'s earlier (unpadded) measurement. Single-threaded `Set`/`Get` are flat, as expected — false sharing is a multi-core effect.

![goache core operations benchmark chart](docs/img/core-ops.svg)

### Ingestion: `WithCapacity` pre-sizing

Bulk-loading a fresh cache when the final size is roughly known upfront (`BenchmarkFreshLoad_*`, 10k entries via `SetMany` into a brand-new `Cache`):

```
BenchmarkFreshLoad_NoHint-24                1110   1024314 ns/op   2519389 B/op   14008 allocs/op
BenchmarkFreshLoad_WithCapacityHint-24      1347    864716 ns/op   2131498 B/op   12748 allocs/op
```

![WithCapacity ingestion benchmark chart](docs/img/capacity-hint.svg)

`WithCapacity` pre-sizes every shard's map so bulk inserts skip Go's incremental map growth/rehashing — but its relative payoff got noticeably noisier once `WithMaxSize` support made every new key's entry a separate heap allocation (see "Automatic eviction" below and [docs/adr/0016](docs/adr/0016-clock-eviction.md)): pre-sizing the map no longer addresses the now-dominant allocation cost, and the speed improvement has ranged from ~6% to ~17% faster across repeated runs (allocations/memory savings stay a consistent ~9%/~15% though), down from a steady ~24%/~30%/~24% before that change. Still worth using whenever you know the approximate final size upfront (e.g. loading a fixed data set at startup) — just don't expect it to compensate for allocation-heavy cold-insert workloads the way it used to, and expect run-to-run variance in exactly how much it helps.

### Optional TTL

`SetWithTTL`/`Entry.TTL` let individual entries expire. The clock is only read when the *found* entry actually has a TTL, so looking up a plain `Set`/`SetMany` entry (no TTL) costs exactly what `Get` costs above — the numbers below are `BenchmarkSetWithTTL`/`BenchmarkGetWithTTL` (against entries that *do* have a TTL) next to the plain `Set`/`Get` numbers for comparison, plus `BenchmarkPurge` (sweeping 100k already-expired entries across 256 shards):

```
BenchmarkSetWithTTL-24      28656522    41.61 ns/op     0 B/op   0 allocs/op
BenchmarkGetWithTTL-24      42207597    28.08 ns/op     0 B/op   0 allocs/op
BenchmarkPurge-24                225   5406601 ns/op     0 B/op   0 allocs/op
```

![TTL overhead benchmark chart](docs/img/ttl-overhead.svg)

`entry` grew by 8 bytes (an `int64` expiry deadline, 0 = never expires) to support this. That extra 8 bytes shows up as a genuine per-call cost on `SetWithTTL` itself (~19ns — one `time.Now()` + `Add` + `UnixNano()`, unavoidable if you're actually setting a deadline) and on `GetWithTTL` (~8ns — one clock read, only for entries that actually have a TTL). Steady-state `Get`/`Set` against non-TTL entries is unaffected by TTL specifically (verified with `-cpu=1` to rule out scheduler noise — see docs/adr/0012). `Purge` is never called automatically; call it yourself (e.g. from your own ticker) if you use TTLs and want expired keys' memory reclaimed promptly — see the package doc comment in `cache.go` and docs/adr/0011 for why goache doesn't start a background goroutine for this itself.

`Purge`'s own number moved substantially since TTL was added (2.56ms baseline -> 5.7-7.1ms across repeated post-eviction runs) for a reason unrelated to TTL: it's a consequence of `WithMaxSize` support (see below), which changed shard storage to individually heap-allocated entries. `Purge`'s sweep here reports 0 allocs/op consistently (this benchmark doesn't configure `WithMaxSize`, so no eviction bookkeeping runs) — the slowdown is a cache-locality cost: entries are no longer packed inline in the map's own buckets, so a full-map sweep now chases one extra pointer per entry. The exact magnitude varies noticeably run-to-run (more than doubled, not a precise multiplier) — see [docs/adr/0016](docs/adr/0016-clock-eviction.md) for the full measurement and reasoning; it applies to any full-cache sweep, not just `Purge`.

### Deletion: `Delete` / `DeleteMany` / `Clear`

`Delete` and `DeleteMany` follow the same shape as `Set`/`SetMany` — `DeleteMany` groups keys by destination shard so each shard's lock is acquired once regardless of how many keys are being removed. `Clear` empties every shard using Go's builtin `clear()`, which is fast enough that isolating its cost from cache (re)population isn't meaningful the way `Purge`'s cost is — `BenchmarkClear` below measures populate-then-Clear together (same shape as `BenchmarkFreshLoad_NoHint`); Clear's own marginal cost is roughly the difference between the two numbers.

```
BenchmarkDelete-24         31262211    40.33 ns/op      0 B/op   0 allocs/op
BenchmarkDeleteMany-24     16596254    71.58 ns/op     84 B/op   1 allocs/op
BenchmarkDeleteSetChurn-24 13120848    88.08 ns/op      0 B/op   0 allocs/op
BenchmarkClear-24                115  10692182 ns/op   22988834 B/op   106406 allocs/op
```

`Delete` is zero-allocation, same cost class as `Set`. `DeleteMany`'s single alloc/op is the same per-call shard-bucket grouping cost `SetMany` already pays. `Clear`'s number includes populating a fresh 100k-entry cache first (see `BenchmarkFreshLoad_NoHint` above, ~1.0ms) — the `Clear()` call itself only accounts for a fraction of the ~11.0ms total; the rest, including essentially all 106406 allocs/op, is the `SetMany` population that precedes it in the benchmark (see "Automatic eviction" below for why every new key now allocates).

`BenchmarkDeleteSetChurn` (delete a key, immediately re-insert it — the workload `bench/`'s cross-library Delete comparison measures) is fully allocation-free: on unbounded caches, `Delete` parks the removed entry in a one-slot per-shard freelist and the next brand-new-key `Set` on that shard reuses it, instead of paying the one-heap-allocation-per-new-key cost that eviction support introduced. That's ~29% faster churn (124.8 → 88.1 ns/op) for ~1-2ns added to `Delete` itself (the park is a single pointer store; see [docs/adr/0019](docs/adr/0019-single-slot-freelist.md)). Bounded (`WithMaxSize`) caches don't use the freelist — their delete/evict path already recycles entries through a `sync.Pool` (see [docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md)).

### Automatic eviction: `WithMaxSize`

`WithMaxSize(n)` bounds the cache to **at most** n entries and turns on per-shard CLOCK (second-chance) eviction once a shard is full. The budget is split so per-shard limits sum to exactly n, so `Len()` can never exceed it; if n is below the shard count, the shard count drops to the largest power of two ≤ n so every shard still gets a slot ([docs/adr/0020](docs/adr/0020-shard-count-does-not-scale-eviction.md) — this used to be a soft "roughly n" that could overshoot badly: `WithMaxSize(100)` on the default 256 shards held 256 entries). Because keys land by hash rather than perfectly evenly, a cache under churn typically settles somewhat *below* n — n bounds memory, it doesn't reserve it. CLOCK was chosen over two alternatives — see [docs/adr/0016-clock-eviction.md](docs/adr/0016-clock-eviction.md) for the full reasoning:

- True LRU (move-to-front on every `Get`) would need `Get` to take a write lock on every hit, undoing the entire point of sharding: letting concurrent readers never block each other.
- W-TinyLFU-style admission (what theine-go and otter/v2 use — see [docs/competitor-analysis.md](docs/competitor-analysis.md)) gives a better hit ratio under skewed workloads, but costs real CPU on every `Set`, which is exactly the cost goache currently beats those libraries on.

CLOCK keeps `Get` at effectively its current cost: each entry carries one atomic "referenced" bit, flipped by `Get` with a plain atomic store — no write lock, no ring mutation. Eviction (which does need the write lock) walks a per-shard circular ring from a "hand" pointer, clearing the bit on anything it finds set and evicting the first entry it finds already clear.

```
BenchmarkSetWithMaxSize-24   14006179    85.35 ns/op     0 B/op   0 allocs/op
BenchmarkGetWithMaxSize-24   43005973    26.98 ns/op     0 B/op   0 allocs/op
BenchmarkEvictionChurn-24    10584315   114.6 ns/op      8 B/op   1 allocs/op
BenchmarkParallelGetWithMaxSize-24 245828545   4.846 ns/op   0 B/op   0 allocs/op
```

![Eviction cost benchmark chart](docs/img/eviction-cost.svg)

`GetWithMaxSize` is barely different from plain `Get` (26.98 vs 23.84 ns/op) — the only added cost is one atomic bit store; `ParallelGetWithMaxSize` confirms this holds under concurrency too (4.846 ns/op, in line with plain `ParallelGet`'s 4.621). `BenchmarkSetWithMaxSize` (cache capped at half the key pool, so roughly every other `Set` evicts) and `BenchmarkEvictionChurn` (cache always full, *every* `Set` evicts) show the cost of enabling eviction: 85.35 and 114.6 ns/op respectively, both down substantially (~35% and ~34%) from pre-pooling numbers (130.8 and 174.9 ns/op) after entries on `WithMaxSize`-bounded shards started recycling through a `sync.Pool` instead of always allocating fresh on every eviction-driven insert — allocs/op dropped from 1→0 and 2→1 accordingly. See [docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md) for the full measurement, including why this pooling is deliberately *not* applied to unbounded caches (it regressed `Delete`/`Clear`/`SetMany`/`FreshLoad_*` there in an earlier, cache-wide attempt, since nothing ever draws those entries back out of a pool that's never drained by eviction).

**Supporting eviction at all still requires a real, unconditional cost, not just an opt-in one**: to let `Get` flip the CLOCK bit without taking a write lock, shard storage changed from `map[K]entry[V]` (values inlined in the map) to `map[K]*entry[K, V]` (individually heap-allocated entries) — for *every* `Cache`, whether or not `WithMaxSize` is ever called. Ring maintenance, the eviction sweep, and pool interaction are all skipped entirely when no limit is configured, but the pointer-per-entry storage isn't. In practice this cost is small for steady-state workloads (see the core-ops numbers above — cycling over an existing key pool almost never allocates, since overwrites reuse the existing pointer) but real for cold-insert-heavy and full-sweep workloads on unbounded caches (`SetMany`, `FreshLoad_*`, `Purge`, `Clear` above all moved from the pre-eviction baseline). [docs/adr/0016](docs/adr/0016-clock-eviction.md) has the full before/after measurement and reasoning, including a locality regression on full-map sweeps that's separate from the allocation-count increase.

## Comparison with other Go cache libraries

Benchmarked head-to-head in `bench/` (a separate module — see `bench/go.mod` — so these comparison-only dependencies never leak into consumers of goache). Run: `cd bench && go test -bench=. -benchmem -run=^$ ./...`

Against:

- **[patrickmn/go-cache](https://github.com/patrickmn/go-cache)** — naive baseline: single global `RWMutex`, values boxed as `interface{}`.
- **[coocood/freecache](https://github.com/coocood/freecache)** — sharded, GC-avoiding cache (`[]byte`-only keys/values, no generics).
- **[dgraph-io/ristretto/v2](https://github.com/dgraph-io/ristretto)** — industry-standard cache with TinyLFU admission control and async, best-effort `Set`.
- **[Yiling-J/theine-go](https://github.com/Yiling-J/theine-go)** — Caffeine-inspired cache with adaptive W-TinyLFU admission and a hierarchical timer wheel for TTL. Requires a bounded `MaximumSize` up front (no unbounded mode) and an explicit per-entry cost on `Set`.
- **[maypok86/otter/v2](https://github.com/maypok86/otter)** — also Caffeine-inspired (adaptive W-TinyLFU as of v2); its author documents it as having had "long-standing unbeatable throughput" among Go caches even in its v1 S3-FIFO incarnation.

See [docs/competitor-analysis.md](docs/competitor-analysis.md) for what each library trades off qualitatively, and [docs/adr/0017-size-parametrized-benchmarks.md](docs/adr/0017-size-parametrized-benchmarks.md) for the benchmark methodology below: every category runs at five working-set sizes (1,000 / 5,000 / 50,000 / 100,000 / 1,000,000 entries), and covers every feature goache has an equivalent for — not just the unbounded Set/Get/ParallelGet baseline, but per-entry TTL, `Delete` (measured as delete-then-reinsert churn — see the ADR for why), and bounded/eviction behavior (for the libraries that have a comparable size-bounded eviction knob: goache's `WithMaxSize`, theine, otter, ristretto — go-cache has no eviction and freecache bounds by byte size, not entry count, so neither has one).

Machine: AMD Ryzen AI 9 HX 370 (24 threads), Go 1.26.2. All tables are ns/op, lower is better. **Mixed provenance, noted for transparency**: the five competitor libraries' numbers are from a single clean run (no other work running in parallel on this machine while it executed — see [docs/adr/0017](docs/adr/0017-size-parametrized-benchmarks.md)'s update on why that matters: two independent clean runs of this exact suite produced meaningfully different numbers for ristretto and theine specifically, discussed in the takeaways below). goache's own rows were re-measured afterward, alone (`go test -bench='^BenchmarkGoache_'`), after [docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md)'s changes to `cache.go` — only goache's implementation changed, the competitor libraries and their code didn't, so re-running just goache's benchmarks to refresh its own rows is valid without re-running the full comparison; but this does mean goache's cells and the other five libraries' cells were measured in separate runs, not the same one.

### Set

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| goache | 27.91 | 23.78 | 28.72 | 32.63 | 127.2 |
| go-cache | 28.27 | 30.07 | 35.28 | 39.11 | 130.6 |
| freecache | 35.83 | 36.63 | 59.13 | 86.50 | 187.0 |
| otter | 139.7 | 130.7 | 133.2 | 137.6 | 225.8 |
| theine | 157.1 | 158.0 | 176.2 | 200.4 | 413.0 |
| ristretto | 223.7 | 236.4 | 240.5 | 287.4 | 362.3 |

### Get

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| go-cache | 12.94 | 12.56 | 15.79 | 17.40 | 70.39 |
| goache | 22.77 | 16.67 | 21.01 | 23.84 | 87.98 |
| otter | 34.71 | 35.01 | 37.89 | 40.79 | 149.2 |
| ristretto | 42.59 | 41.93 | 61.51 | 79.35 | 178.5 |
| freecache | 47.67 | 48.57 | 72.66 | 108.4 | 208.3 |
| theine | 87.17 | 86.33 | 111.6 | 132.1 | 286.4 |

### ParallelGet

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| goache | 5.673 | 4.573 | 4.235 | 4.578 | 9.250 |
| otter | 3.262 | 6.722 | 7.479 | 5.980 | 9.934 |
| theine | 5.258 | 5.220 | 5.978 | 6.324 | 10.52 |
| ristretto | 9.079 | 8.217 | 7.534 | 8.674 | 10.88 |
| freecache | 14.59 | 14.95 | 15.39 | 15.66 | 17.59 |
| go-cache | 36.74 | 37.01 | 37.05 | 36.61 | 38.42 |

### SetWithTTL

otter has no per-`Set` TTL argument — it configures a fixed write-based expiry policy at construction instead (`ExpiryWriting`); its numbers here use that, documented in `bench/compare_test.go`'s `newOtterTTL`.

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| goache | 35.33 | 30.91 | 37.67 | 42.10 | 171.7 |
| go-cache | 34.41 | 36.26 | 46.66 | 51.80 | 201.2 |
| freecache | 36.32 | 38.01 | 63.18 | 88.99 | 194.8 |
| otter | 159.2 | 159.2 | 168.8 | 174.2 | 260.6 |
| theine | 181.6 | 176.5 | 207.0 | 261.3 | 420.2 |
| ristretto | 248.2 | 253.1 | 299.5 | 372.8 | 441.2 |

### GetWithTTL

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| go-cache | 14.10 | 14.98 | 20.59 | 22.70 | 94.21 |
| goache | 28.26 | 21.08 | 26.07 | 28.35 | 93.24 |
| otter | 47.96 | 50.17 | 54.57 | 59.86 | 163.2 |
| ristretto | 47.16 | 46.00 | 58.42 | 94.23 | 187.8 |
| freecache | 47.64 | 48.50 | 74.35 | 106.1 | 211.9 |
| theine | 86.90 | 83.13 | 103.1 | 136.0 | 287.9 |

### Delete (measured as delete-then-reinsert churn, not isolated deletion — see the ADR)

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| go-cache | 53.64 | 58.12 | 68.42 | 72.00 | 191.4 |
| goache | 65.60 | 64.54 | 82.52 | 87.69 | 198.0 |
| freecache | 83.20 | 102.9 | 159.0 | 200.4 | 274.5 |
| otter | 225.0 | 226.0 | 243.7 | 257.1 | 361.5 |
| theine | 408.9 | 418.1 | 431.3 | 492.9 | 583.1 |
| ristretto | 485.0 | 471.8 | 514.6 | 537.1 | 649.0 |

### Bounded (Set against a cache actively evicting — limit = n/2; go-cache and freecache have no comparable mode, see above)

| Library | 1,000 | 5,000 | 50,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|---|
| goache | 47.52 | 61.31 | 69.12 | 110.4 | 269.8 |
| otter | 140.9 | 131.3 | 156.1 | 159.9 | 237.3 |
| ristretto | 125.5 | 108.6 | 104.0 | 89.94 | 76.34 |
| theine | 199.4 | 190.3 | 184.8 | 233.1 | 347.5 |

![Set benchmark comparison chart](docs/img/compare-set.svg)

![Parallel Get benchmark comparison chart](docs/img/compare-parallel-get.svg)

Takeaways:

- **Every library's single-threaded `Get` degrades noticeably from 1,000 to 1,000,000 entries** (go-cache 12.94→70.39, goache 22.77→87.98, otter 34.71→149.2, ristretto 42.59→178.5, theine 87.17→286.4) — a universal effect of the working set outgrowing CPU cache, not specific to any one library's design. goache's *relative* degradation is in line with everyone else's, and its *absolute* numbers stay competitive throughout.
- **Under real concurrency, sharded/striped designs hold their ground at every size**: goache, otter, and theine all stay roughly in the 4-11 ns/op range even at 1,000,000 entries, while go-cache's single global lock is essentially flat regardless of size (36.7 → 38.4 ns/op) — it was never bottlenecked by cache size, only by lock contention, and that bottleneck doesn't change no matter how big the map gets. This is the sharding argument playing out exactly as designed, confirmed at 1,000x the original single-size comparison's scale.
- **goache now leads `ParallelGet` at every size except n=1,000** (see the reordered table above), after padding each shard to a cache line to remove false sharing between adjacent shards ([docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md)) — previously it trailed both otter and theine at most sizes.
- **Delete churn is now allocation-free and second only to go-cache**: the per-shard freelist ([docs/adr/0019](docs/adr/0019-single-slot-freelist.md)) dropped goache's row from 93.27-220.5 to 65.60-198.0 ns/op, overtaking freecache at every size (previously goache lost to it up to n=100,000). The remaining gap to go-cache is structural — the churn cycle pays goache's shard-routing hash twice per iteration, the price of the sharding that wins `ParallelGet`.
- **ristretto and theine show real, substantial run-to-run variance — enough to flip a conclusion.** Re-running this exact suite (same machine, same code, a clean wait with no other work in parallel — see [docs/adr/0017](docs/adr/0017-size-parametrized-benchmarks.md)) moved theine's `Set` at n=1,000 from 271.7 to 157.1 ns/op (a ~42% swing) and, more importantly, **flipped ristretto's `Bounded` scaling trend**: the first run showed it essentially flat (111.6→117.8 ns/op, 1,000→1,000,000), this run shows it clearly *decreasing* with size (125.5→76.34 ns/op). goache, go-cache, freecache, and otter's numbers stayed consistent across both runs. The most plausible explanation is that ristretto's and theine's own internal async machinery (ristretto's buffered-write goroutines, theine's timer wheel and background maintenance) makes them more sensitive to scheduler noise than the synchronous libraries — meaning single-run comparisons involving those two specifically should be read as directional, not precise, and any conclusion drawn from one run of their numbers deserves a second run before being trusted.
- **goache's `WithMaxSize` eviction cost still grows with cache size** (the "Bounded" table): 47.52 ns/op at 1,000 entries to 269.8 ns/op at 1,000,000, roughly 5.7x — still matches [ADR 0016](docs/adr/0016-clock-eviction.md)'s locality-regression finding (goache's CLOCK ring is an intrusive per-shard linked list of individually heap-allocated entries, so a bigger shard means a bigger, more cache-unfriendly ring to walk), though the absolute numbers at smaller/mid sizes dropped substantially (roughly 35-40% at n=1,000-100,000) after gating `sync.Pool` entry recycling to `WithMaxSize`-bounded shards ([docs/adr/0018](docs/adr/0018-gemini-analysis-experiments.md)) — the pooling win shrinks at n=1,000,000, converging back to roughly the old number, which is itself worth noting rather than glossing over. Given the variance noted above, whether competitors' bounded-eviction cost scales better is genuinely unclear from this data — ristretto's own numbers moved in opposite directions between two clean runs — but goache's own upward trend is the one solid, reproduced finding here. **Raising the shard count does not fix it**: measured across 256/512/1024/2048/4096 shards, bounded eviction got *worse* with more shards at the sizes that matter (+25% at n=100,000, +19% at n=1,000,000), so `WithShardCount` is not the knob to reach for here — see [docs/adr/0020](docs/adr/0020-shard-count-does-not-scale-eviction.md).
- **`Set`**: goache is fastest at every size up to 100,000, and the only zero-allocation `Set` for repeated keys (see "Automatic eviction" above for the cold-insert cost `WithMaxSize` support added). Every admission-policy cache (theine, otter, ristretto) pays for eviction-policy bookkeeping on every `Set` — the trade goache's default (no eviction) opts out of.
- **ristretto's `Set` is asynchronous and best-effort** (values can be dropped by its admission policy) — a materially different contract than goache's synchronous, always-succeeds `Set`. Not a drop-in semantic replacement, but included since it's the most commonly reached-for high-performance Go cache today.
- **theine and otter are not drop-in semantic replacements either**: both implement W-TinyLFU admission/eviction, meaning `Set` can be a no-op if the entry loses the admission race — goache's `Set` always lands, whether or not `WithMaxSize` is configured; a full shard evicts something else to make room rather than rejecting the incoming write. Pick goache when you want guaranteed writes with a size bound whose cost is at least predictable (even if it grows with scale); pick theine/otter/ristretto when you need a hit-ratio-optimizing admission policy (better under skewed access patterns) and can accept probabilistic admission — and budget for their benchmark numbers being noisier run-to-run than goache's own.
