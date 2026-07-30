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
```

## Architecture

`Cache[K comparable, V any]` shards keys across N independently-locked segments (`sync.RWMutex` per shard, `hash/maphash.Comparable` for routing) instead of one global lock or `sync.Map`. See the package doc comment in `cache.go` for the full reasoning, including why Go's experimental `arena` package was evaluated and rejected for this phase, and two optimizations that were tried and measured as regressions (a contiguous shard-value-slice layout, and a custom open-addressed per-shard table replacing Go's map) — both reverted, kept only as documented lessons.

Phase 1 scope: `Set`, `SetMany`, `Get`, `WithCapacity` (pre-sizing). Phase 2 (partial): optional per-entry TTL via `SetWithTTL`/`Entry.TTL`, lazily enforced in `Get`, reclaimed via the caller-driven `Purge` — no background goroutine (see docs/adr/0011). LRU/LFU eviction is still not implemented.

## Benchmarks

Run: `make bench` (or `go test -bench=. -benchmem -run=^$ ./...`). Charts below are regenerated with `make charts` after updating the numbers in this file — see `docs/benchcharts/main.go`.

Last measured on AMD Ryzen AI 9 HX 370 (24 threads), Go 1.26.2:

```
BenchmarkSet-24               39047482    30.82 ns/op     0 B/op   0 allocs/op
BenchmarkSetMany-24            7647193   162.4 ns/op    342 B/op   1 allocs/op
BenchmarkGet-24               48276721    25.71 ns/op     0 B/op   0 allocs/op
BenchmarkGetMiss-24           18264889    65.04 ns/op     7 B/op   0 allocs/op
BenchmarkParallelGetSet-24   129884720     9.243 ns/op    0 B/op   0 allocs/op
BenchmarkParallelGet-24      248949174     4.562 ns/op    0 B/op   0 allocs/op
```

`Set`/`Get` are zero-allocation on the hot path. `SetMany`'s single alloc/op comes from its per-call shard-bucket grouping (scales with shard count, not batch size); `GetMiss`'s alloc is the benchmark's own key-string construction, not the cache. (Numbers moved slightly from earlier revisions of this table because `entry` grew by 8 bytes to hold TTL — see "Optional TTL" below for the full explanation and why it doesn't affect steady-state Get/Set.)

![goache core operations benchmark chart](docs/img/core-ops.svg)

### Ingestion: `WithCapacity` pre-sizing

Bulk-loading a fresh cache when the final size is roughly known upfront (`BenchmarkFreshLoad_*`, 10k entries via `SetMany` into a brand-new `Cache`):

```
BenchmarkFreshLoad_NoHint-24                534   2227967 ns/op   2110923 B/op   4263 allocs/op
BenchmarkFreshLoad_WithCapacityHint-24      706   1708948 ns/op   1602288 B/op   3003 allocs/op
```

![WithCapacity ingestion benchmark chart](docs/img/capacity-hint.svg)

`WithCapacity` pre-sizes every shard's map so bulk inserts skip Go's incremental map growth/rehashing almost entirely: **~23% faster, ~30% fewer allocations, ~24% less memory** for a fresh bulk load. Use it whenever you know the approximate final size upfront (e.g. loading a fixed data set at startup).

### Optional TTL

`SetWithTTL`/`Entry.TTL` let individual entries expire. The clock is only read when the *found* entry actually has a TTL, so looking up a plain `Set`/`SetMany` entry (no TTL) costs exactly what `Get` costs above — the numbers below are `BenchmarkSetWithTTL`/`BenchmarkGetWithTTL` (against entries that *do* have a TTL) next to the plain `Set`/`Get` numbers for comparison, plus `BenchmarkPurge` (sweeping 100k already-expired entries across 256 shards):

```
BenchmarkSetWithTTL-24      22926139    50.21 ns/op     0 B/op   0 allocs/op
BenchmarkGetWithTTL-24      41511574    33.98 ns/op     0 B/op   0 allocs/op
BenchmarkPurge-24                368   3243940 ns/op     0 B/op   0 allocs/op
```

![TTL overhead benchmark chart](docs/img/ttl-overhead.svg)

`entry` grew by 8 bytes (an `int64` expiry deadline, 0 = never expires) to support this. That extra 8 bytes shows up in two places: a genuine per-call cost on `SetWithTTL` itself (~19ns — one `time.Now()` + `Add` + `UnixNano()`, unavoidable if you're actually setting a deadline) and on `GetWithTTL` (~8ns — one clock read, only for entries that actually have a TTL); and a larger, one-time cost on cache/shard *creation* (`New`, and any point a shard's map grows) since Go's map buckets got bigger to hold the wider value type — this is what moved the core-ops and ingestion numbers above. Steady-state `Get`/`Set` against non-TTL entries is unaffected (verified with `-cpu=1` to rule out scheduler noise — see docs/adr/0012). `Purge` is never called automatically; call it yourself (e.g. from your own ticker) if you use TTLs and want expired keys' memory reclaimed promptly — see the package doc comment in `cache.go` and docs/adr/0011 for why goache doesn't start a background goroutine for this itself.

## Comparison with other Go cache libraries

Benchmarked head-to-head in `bench/` (a separate module — see `bench/go.mod` — so these comparison-only dependencies never leak into consumers of goache). Run: `cd bench && go test -bench=. -benchmem -run=^$ ./...`

Same workload (string keys, int values, 100k-key working set) against:

- **[patrickmn/go-cache](https://github.com/patrickmn/go-cache)** — naive baseline: single global `RWMutex`, values boxed as `interface{}`.
- **[coocood/freecache](https://github.com/coocood/freecache)** — sharded, GC-avoiding cache (`[]byte`-only keys/values, no generics).
- **[dgraph-io/ristretto/v2](https://github.com/dgraph-io/ristretto)** — industry-standard cache with TinyLFU admission control and async, best-effort `Set`.

Last measured on AMD Ryzen AI 9 HX 370 (24 threads), Go 1.26.2:

```
BenchmarkGoache_Set-24              35925884    29.84 ns/op     0 B/op   0 allocs/op
BenchmarkGoache_Get-24              40742186    26.99 ns/op     0 B/op   0 allocs/op
BenchmarkGoache_ParallelGet-24     264036742     4.531 ns/op    0 B/op   0 allocs/op

BenchmarkGoCache_Set-24             25102554    41.88 ns/op     8 B/op   1 allocs/op
BenchmarkGoCache_Get-24             66479047    17.34 ns/op     0 B/op   0 allocs/op
BenchmarkGoCache_ParallelGet-24     32337865    36.86 ns/op     0 B/op   0 allocs/op

BenchmarkFreecache_Set-24           11280877   117.1 ns/op      1 B/op   0 allocs/op
BenchmarkFreecache_Get-24            9309873   122.1 ns/op      8 B/op   1 allocs/op
BenchmarkFreecache_ParallelGet-24   64656913    15.93 ns/op      8 B/op   1 allocs/op

BenchmarkRistretto_Set-24            3744554   344.0 ns/op     85 B/op   1 allocs/op
BenchmarkRistretto_Get-24            4793264   249.8 ns/op      7 B/op   0 allocs/op
BenchmarkRistretto_ParallelGet-24  149477314     8.998 ns/op    0 B/op   0 allocs/op
```

![Set benchmark comparison chart](docs/img/compare-set.svg)

![Parallel Get benchmark comparison chart](docs/img/compare-parallel-get.svg)

Takeaways:

- **Parallel reads**: goache is fastest by a clear margin (4.53 ns/op) — ~8.1x faster than go-cache's single global lock (36.9 ns/op), ~2.0x faster than ristretto's admission-controlled path (9.0 ns/op), ~3.5x faster than freecache (15.9 ns/op). This is the payoff of lock-striping under real concurrent load.
- **Single-threaded Get**: go-cache edges out goache (17.34 vs 26.99 ns/op) — an uncontended single global mutex has less overhead than hashing a key to pick a shard, and goache's `Get` now also does one extra `expiresAt != 0` check per lookup to support optional TTL (see "Optional TTL" above). Sharding's benefit only shows up under concurrency, which is the workload this cache is designed for.
- **Set**: goache is fastest and the only zero-allocation `Set` in the comparison. freecache and ristretto both pay for GC-avoidance or admission-control bookkeeping (byte-copying into ring buffers / cost-tracking), and go-cache pays for boxing into `interface{}`.
- **ristretto's `Set` is asynchronous and best-effort** (values can be dropped by its admission policy) — a materially different contract than goache's synchronous, always-succeeds `Set`. Not a drop-in semantic replacement, but included since it's the most commonly reached-for high-performance Go cache today.
