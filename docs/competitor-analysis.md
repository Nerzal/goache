# Go cache library competitor analysis

Findings from a deep dive into two sources

1. [Yiling-J/theine-go](https://github.com/Yiling-J/theine-go) (README)
2. [otter's "Cache evolution" blog post](https://maypok86.github.io/otter/blog/cache-evolution/)

Goal: know every competing Go cache library these two sources point at, and
for each, what it does better or worse than the others — and where goache
sits relative to all of them. Two of these libraries (theine-go, otter/v2)
were added to `bench/compare_test.go` for direct measurement; see
[ADR 0014](adr/0014-add-theine-otter-to-comparison.md) and the updated
"Comparison with other Go cache libraries" section in the root README.

## The libraries

### Ristretto (dgraph-io/ristretto)

- **Design**: TinyLFU admission (Count-Min Sketch frequency estimate) with a
  Bloom-filter doorkeeper, BP-Wrapper batching for buffer contention. Writes
  go through an async ring buffer — `Set` is fire-and-forget and can be
  dropped by the admission policy.
- **Better than others at**: relatively high throughput and decent hit
  rates; was for a long time the default "state of the art" pick, which is
  why goache's `bench/` module already benchmarked against it (see
  [ADR 0008](adr/0008-separate-comparison-bench-module.md)).
- **Worse than others at**: poor performance on OLTP-shaped workloads;
  documented bugs in its count-min sketch implementation; `Set` can fail
  under high contention; stores key hashes without collision handling (two
  different keys can collide and shadow each other); no loading/refresh
  API; breaking changes around metadata/cost handling across versions.
- **Measured in `bench/`**: Set 346.4 ns/op (85 B/op, 1 alloc), Get 131.2
  ns/op, ParallelGet 8.165 ns/op — matches the "decent but not best"
  characterization above; goache beats it on every axis measured.

### Theine (Yiling-J/theine-go)

- **Design**: adaptive W-TinyLFU (Caffeine-inspired), hierarchical timer
  wheel for TTL expiration, optional entry pooling (off by default — race
  risk), optional doorkeeper, removal listeners, and a hybrid (in-memory +
  secondary tier) mode.
- **Better than others at**: after integrating `xsync`-based primitives, its
  own benchmarks claim high speed alongside a hit rate that stays high on
  **any** workload shape (not just skewed/Zipf); excellent
  expiration/timer-wheel design; has cache-stampede protection built in.
  Its own README benchmarks show it beating ristretto substantially:
  ~95.7M ops/s vs ~53M ops/s (32 CPUs, 100% read, Zipf) and ~1.9M vs ~1.4M
  ops/s on 100% write, with 0 B/op vs ristretto's 112 B/op.
- **Worse than others at**: its sharded-map design caps how far it scales
  with core count (per the otter blog's critique); its lossy read buffers
  cost memory and reduce hit rate under contention; no bulk-loading API;
  can permit transiently dirty data during concurrent operations; no
  refresh support.
- **Requires a bounded `MaximumSize` up front** — no unbounded constructor,
  unlike goache or otter.
- **Measured in `bench/`**: Set 239.6 ns/op (3 B/op), Get 134.2 ns/op (16
  B/op, 1 alloc), ParallelGet 6.422 ns/op. Its own published numbers put it
  well ahead of ristretto, which our measurement corroborates in relative
  terms (theine's ParallelGet beats ristretto's here too), though absolute
  numbers differ from theine's README since that used a Zipf-distributed
  32-core read-heavy benchmark, not this repo's fixed 100k-key uniform
  workload.

### Otter v1 (maypok86/otter, pre-v2)

- **Design**: modified S3-FIFO with BP-Wrapper for buffer contention.
- **Better than others at**: per its own authors, "long-standing unbeatable
  throughput" among Go caches, high hit rates on most workloads, minimal
  memory overhead — optional features only cost memory when actually
  turned on.
- **Worse than others at**: lossy read buffers are especially problematic
  for small caches; hit rate on frequency-skewed workloads is worse than
  TinyLFU-family algorithms (S3-FIFO's recency-only signal misses
  frequency information TinyLFU captures); no loading/refresh API.

### Otter v2 (maypok86/otter/v2)

- **Design**: switched from S3-FIFO to adaptive W-TinyLFU, explicitly
  modeled on Caffeine's architecture (same lineage as theine).
- **Better than others at**: implements the full feature set — loading,
  refreshing, entry pinning; the blog claims "one of the highest hit rates
  across **all** workloads" of anything surveyed; adds HashDoS protection;
  auto-configures its lossy read buffer size instead of requiring manual
  tuning.
- **Worse than others at**: much less real-world production usage/track
  record than ristretto or the v1 lineage, being the newest design here.
- **API**: `Options` struct constructor (`otter.New`/`otter.Must`), supports
  a genuinely unbounded cache (`&otter.Options[K,V]{}` with no size limit)
  unlike theine-go.
- **Measured in `bench/`**: Set 146.5 ns/op (49 B/op, 1 alloc), Get 42.43
  ns/op, ParallelGet 3.985 ns/op — the fastest ParallelGet of every library
  measured, goache included, on this workload. Corroborates the blog's own
  throughput claim.

### Sturdyc

- **Design**: custom O(n) eviction policy, built around request
  coalescing/deduplication, refresh-ahead, and bulk operations rather than
  raw hit-ratio optimization.
- **Better than others at**: it's the only one of these with built-in
  request/response batching that can deduplicate concurrent identical
  loads, plus refresh and bulk-load support out of the box, and cache
  stampede protection.
- **Worse than others at**: the O(n) eviction policy itself is slow and
  gives a worse hit ratio than plain LRU; expiration handling is
  suboptimal; performance is generally sluggish next to the TinyLFU/S3-FIFO
  family above; keys are strings only (no generics over key type); can't
  deduplicate a mix of batched and non-batched requests to the same key;
  can permit dirty data.
- **Not added to `bench/`**: its value proposition is request-coalescing
  and refresh semantics, not raw Set/Get throughput, so a like-for-like
  Set/Get/ParallelGet benchmark against it would be measuring the wrong
  thing. Noted here for completeness per the "know all competitors" goal.

## Where goache sits

goache is architecturally closest to Ristretto's "high-performance general
cache" niche but takes the opposite trade-off from the entire
TinyLFU/S3-FIFO family (ristretto, theine, otter v1/v2): **no eviction
policy at all**. Every library above spends CPU and/or memory on admission
and eviction bookkeeping (frequency sketches, timer wheels, lossy read
buffers) in exchange for bounded memory and a controlled working set. goache
is a fixed-shard-count map with lock striping and no automatic eviction —
optional TTL (lazy, sweep-on-`Purge`) is the only lifecycle feature that
exists at all. That buys it:

- The only zero-allocation `Set` among everything measured in `bench/`.
- Guaranteed writes — no admission policy can silently drop a `Set`, unlike
  ristretto (documented, async/best-effort) or the W-TinyLFU family
  (a `Set` can lose the admission race).
- Competitive-to-best parallel-read throughput (4.59 ns/op, essentially
  tied with otter v2's 3.985 ns/op and clearly ahead of theine's 6.42,
  ristretto's 8.17, freecache's 16.4, go-cache's 37.1) without any of
  TinyLFU's bookkeeping cost.

The trade-off: goache has no hit-ratio-optimizing eviction, so it isn't a
substitute for these libraries in a memory-bounded, "keep the hottest N
items" use case — that's squarely what W-TinyLFU (theine, otter v2) and
S3-FIFO (otter v1) exist for. Pick goache for a fixed/known working set with
guaranteed-write, low-overhead concurrent access (with optional TTL); pick
theine/otter/ristretto when you need automatic eviction under memory
pressure and can accept probabilistic admission.

## Sources

- theine-go README: <https://github.com/Yiling-J/theine-go>
- otter cache-evolution blog post: <https://maypok86.github.io/otter/blog/cache-evolution/>
- Measurements: `bench/compare_test.go`, run via `cd bench && go test -bench=. -benchmem -run=^$ ./...` on AMD Ryzen AI 9 HX 370 (24 threads), Go 1.26.2 — same run documented in README.md's "Comparison with other Go cache libraries" section.
