# Feature roadmap: goache toward a full-fledged cache library

Derived from [docs/competitor-analysis.md](competitor-analysis.md) (ristretto,
theine-go, otter v1/v2, sturdyc) plus a direct audit of goache's public API
at the time this roadmap was written (`New`, `WithShardCount`,
`WithCapacity`, `Set`, `SetWithTTL`, `SetMany`, `Get`, `Len`, `Purge`).
`Delete`/`DeleteMany`/`Clear` (P0) and `WithMaxSize` eviction (P1 item 3)
have since shipped — see their respective status notes below and
`docs/adr/` for the specifics; the remaining items are still a prioritized
gap list, not a commitment. Each still needs its own design discussion and,
per the repo's standing performance policy, benchmark coverage in
`cache_bench_test.go` plus an ADR before landing (see `CLAUDE.md`).

## P0 — table-stakes gaps (missing even against a "naive" cache)

**Status: done.** Both items below shipped — see
[ADR 0015](adr/0015-delete-clear-api.md), `Delete`/`DeleteMany`/`Clear` in
`cache.go`, and the "Deletion" subsection of benchmarks/README.md.

### 1. `Delete(key)` / `DeleteMany(keys)`

**There is currently no way to remove a single key from goache.** Every
competitor surveyed — go-cache, freecache, ristretto, theine, otter, sturdyc
— has a `Delete`. This isn't a competitive nice-to-have, it's a basic
correctness gap: without TTL, an entry set with `Set` can never leave the
cache short of process restart. `Purge()` only reclaims *already-expired*
TTL entries; it does nothing for a plain `Set` entry a caller wants gone now.

- Scope: small. One shard-locked map delete per key, `DeleteMany` groups by
  shard the same way `SetMany` already does (see `cache.go`'s per-shard
  bucketing).
- Needs: `BenchmarkDelete`/`BenchmarkDeleteMany` alongside `SetMany`'s
  existing benchmark pattern.

### 2. `Clear()` / `Reset()`

Drop every entry across all shards. Useful for tests and for
config-reload-style full invalidation. Every general-purpose cache library
in the comparison has an equivalent. Trivial to implement (lock each shard,
replace its map) but currently has no substitute — recreating a whole `New`
cache is the only workaround today.

## P1 — the core strategic gap: bounded memory / eviction

**Status: item 3 done.** See [ADR 0016](adr/0016-clock-eviction.md),
`WithMaxSize` in `cache.go`, and the "Automatic eviction" subsection of
benchmarks/README.md. Item 4 (stats) is still open.

### 3. An eviction policy (the single biggest gap vs. every competitor)

goache's entire design (see `CLAUDE.md` architecture section) is a
fixed-shard-count map with **no eviction at all** — `entry[V]` is explicitly
noted as "still the extension point for further phase-2 metadata (e.g.
LRU/LFU bookkeeping)". Every other library surveyed — ristretto and theine
and otter v1/v2 (W-TinyLFU or S3-FIFO), even sturdyc (custom O(n) policy) —
exists specifically to bound memory by evicting the least valuable entries
under a size/weight limit. Without this, goache cannot be used as a
memory-bounded cache in production; callers must externally cap how many
keys they ever `Set`.

This is a major design decision, not a quick add — it directly trades away
goache's current headline property ("every `Set` always lands, zero
eviction-policy overhead"). Two shapes to weigh against each other:

- **Simple bounded LRU/CLOCK per shard** — cheap to reason about, keeps
  goache's "no fancy bookkeeping" philosophy, but per
  `docs/competitor-analysis.md`, plain LRU is known to lose to
  frequency-aware policies (TinyLFU) on skewed/Zipf workloads — exactly the
  workload theine's own benchmarks target.
- **W-TinyLFU-style admission** (what theine and otter v2 converged on) —
  much better hit ratio under skew, but is the exact bookkeeping cost
  goache's benchmarks currently beat theine/otter on (`Set`: goache 34.95
  ns/op zero-alloc vs theine 239.6 ns/op, otter 146.5 ns/op). Adopting it
  would mean deliberately giving up that headline number for a hit-ratio
  guarantee goache doesn't currently make at all.

Either choice needs a maximum-size/weight config option (mirroring
`WithCapacity`'s existing `Option` pattern), a per-shard-or-global bound
decision (global bound needs cross-shard coordination goache doesn't have
today), and new benchmarks comparing bounded-cache hit ratio and Set/Get
cost against the current unbounded baseline. This is the one item on this
list that warrants its own ADR *before* any code is written, given how much
of goache's current design (documented in the package doc comment) is
premised on not having this.

**Resolved**: per-shard CLOCK, added as `WithMaxSize(n)`. The write-lock
concern with true LRU was decisive — CLOCK keeps `Get`'s cost near-zero
(one atomic bit store) at the cost of a real, unconditional change to
shard storage (`map[K]entry[V]` -> `map[K]*entry[K, V]`, needed so that bit
is individually addressable) that applies to every `Cache`, not just ones
using `WithMaxSize`. Full measurement, including a locality regression on
full-map sweeps (`Purge`, `Clear`) that's separate from the allocation-count
increase, is in [ADR 0016](adr/0016-clock-eviction.md). W-TinyLFU admission
was not implemented and isn't ruled out for later — revisit if
hit-ratio-under-skew becomes a measured problem for real workloads.

**New evidence relevant to that "revisit later" call**: the size-parametrized
competitor benchmarks added in [ADR 0017](adr/0017-size-parametrized-benchmarks.md)
show goache's `WithMaxSize` (CLOCK) eviction cost growing ~3.5-3.75x from
1,000 to 1,000,000 entries, reproducibly across two independent runs — a
concrete, measured version of the locality concern ADR 0016 only described
in the abstract. Whether competitors scale better at that bound is *not*
settled, though: a first run suggested ristretto's frequency-sketch
admission stayed flat with size, but a second clean re-run showed it
*decreasing* instead — see ADR 0017's write-up on why ristretto's and
theine's own background machinery makes their numbers noisier run-to-run
than the synchronous libraries', and don't trust a single run of either
library's numbers for this kind of comparison. goache's own upward trend is
the one finding here that reproduced and can be relied on; it doesn't
change the moderate-scale recommendation (goache is still fastest for
guaranteed writes at reasonable sizes), but it's a real, confirmed scaling
cost worth weighing before reaching for `WithMaxSize` at very large bounds.

### 4. Cache statistics (hit/miss/eviction counters)

ristretto, theine, and otter all expose a `Stats()` (hits, misses,
evictions, sometimes cost/weight tracking). goache currently exposes none —
`Len()` is the only introspection available, and it counts raw stored
entries, not live ones (documented trade-off, see
[ADR 0011](adr/0011-lazy-ttl-no-background-janitor.md)). Needed as a
prerequisite for evaluating eviction policy quality once #3 lands, and
independently useful for operators today. Low complexity: atomic counters
per shard or globally, aggregated on read — should be cheap enough not to
regress `BenchmarkGet`/`BenchmarkSet`, but needs to be *proven* cheap with a
benchmark, not assumed.

## P2 — features theine/otter/sturdyc have that goache doesn't

### 5. Loading cache / `GetOrLoad(key, loader)` with stampede protection

theine's `LoadingCache`, otter v2's `Get(ctx, key, loader)`, and sturdyc's
whole design center on: on a miss, call a user-supplied loader function, and
ensure concurrent misses for the *same* key collapse into one loader call
(single-flight) rather than a thundering herd hitting the backing store.
goache has neither loading nor stampede protection today. This is
additive — it can sit as a new method set alongside the existing
`Set`/`Get`, doesn't require touching the core shard/map design, and is
a natural fit for a `golang.org/x/sync/singleflight`-style pattern scoped
per shard. Moderate complexity, no conflict with goache's zero-eviction
stance (it's orthogonal to #3).

### 6. Removal listeners / callbacks

theine supports a callback invoked on eviction/expiration/explicit delete,
useful for cache-aside patterns that need to react to invalidation (e.g.
metrics, cascading cleanup). Depends on #1 (`Delete`) and #3 (eviction)
existing first to have anything to report on; low value in isolation.

### 7. Refresh-ahead

otter v2 and sturdyc both support proactively refreshing a hot entry before
it expires, avoiding a synchronous reload on the access that finally misses.
Meaningfully useful, but it's a layer on top of #5 (loading) — sequence
after loading lands, not before.

## P3 — niche / large-scope, lowest priority

### 8. Persistence (`SaveCache`/`LoadCache`)

theine supports serializing the whole cache to an `io.Writer` and restoring
it. Real but niche use case (warm restart across deploys); large surface
area (needs a stable wire format across `K`/`V` generic types) for
comparatively few consumers. Lowest priority on this list.

### 9. Hybrid / tiered cache (in-memory + secondary store)

theine's "hybrid cache" mode layers a secondary (e.g. disk-backed) tier
under the in-memory one. This is close to a different product than what
goache is today (a single-process in-memory map) — flagged here only
because the competitor analysis surfaced it, not because it fits goache's
current scope. Would need its own design doc if ever pursued.

## Deliberately not on this roadmap

- **Background janitor / auto-expiry goroutine.** goache's lazy-TTL,
  caller-driven `Purge()` design is a considered decision
  ([ADR 0011](adr/0011-lazy-ttl-no-background-janitor.md)), not a gap.
  Nothing above should reopen it; if a convenience ticker is ever wanted, it
  belongs as an opt-in helper outside the core `Cache` type, not a change to
  `Get`/`Set`'s current zero-goroutine contract.
- **sturdyc-style request coalescing as the primary API.** Its value prop
  (batched/deduplicated loads) is subsumed by #5 above; no need to match its
  API shape beyond that.

## Suggested sequencing

P0 (#1 `Delete`/`Clear`) has landed — see [ADR 0015](adr/0015-delete-clear-api.md).
It was correctly sequenced first: it's a correctness gap, not a competitive
one, and every other item (removal listeners, eviction reporting) assumes
deletion exists. #3 (eviction) has also landed — see
[ADR 0016](adr/0016-clock-eviction.md) — as CLOCK, chosen over W-TinyLFU to
keep `Get` write-lock-free. #4 (stats) is next: it's cheap and should land
now so eviction-policy quality (hit/miss/eviction counts) is measurable —
`WithMaxSize`'s CLOCK sweep currently has no introspection beyond `Len()`.
#5-7 (loading/stampede/refresh) are additive and can proceed in parallel
with #4 now that #1 and #3 are both done. #8-9 are optional, low-priority, and
should only be picked up on explicit request.
