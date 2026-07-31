# 0017: Size-parametrized, feature-parity competitor benchmarks

## Status

Accepted

## Context

The `bench/` comparison (see [ADR 0008](0008-separate-comparison-bench-module.md),
[ADR 0014](0014-add-theine-otter-to-comparison.md)) only ever measured a
single fixed 100k-key working set, and only covered goache's unbounded
Set/Get/ParallelGet — not TTL, not Delete, not `WithMaxSize` eviction (added
in [ADR 0016](0016-clock-eviction.md)). The user asked for more thorough
benchmarks: every feature goache has, compared against every competitor
that has an equivalent, across a range of cache sizes (1,000 / 5,000 /
50,000 / 100,000 / 1,000,000 entries) — to see not just who's faster at one
size, but how each library's cost scales as the working set grows.

## Decision

Restructured `bench/compare_test.go` around two axes:

- **Size**: every benchmark runs at all five sizes via a `runSizes` helper
  that wraps the benchmark body in `b.Run(fmt.Sprintf("n=%d", n), ...)` —
  e.g. `BenchmarkGoache_Set/n=50000`. A single size can be targeted with
  `go test -bench=BenchmarkGoache_Set/n=1000000`.
- **Feature parity**: four categories per library, matching goache's public
  API surface:
  - `Set`/`Get`/`ParallelGet` — the existing unbounded/no-TTL baseline.
  - `SetWithTTL`/`GetWithTTL` — per-entry expiry, for every library that has
    it (all but... see below).
  - `Delete` — measured as delete-then-reinsert churn, not isolated
    deletion (see the "Delete churn" section below for why).
  - `Bounded` — Set against a cache configured to evict under real
    pressure, for every library with a native size-bounded eviction policy
    (goache's `WithMaxSize`, theine, otter, ristretto).

### otter's TTL: architecturally different, approximated

Every library compared takes a TTL as a per-`Set` argument
(`SetWithTTL(key, value, ttl)` or equivalent) — except otter v2, which
configures expiry once at cache construction via an `ExpiryCalculator`
(`ExpiryWriting`, `ExpiryAccessing`, etc.), not per entry. `newOtterTTL`
builds a separate otter cache with `ExpiryCalculator:
otter.ExpiryWriting[string, int](ttl)` and then uses plain `Set`/
`GetIfPresent` — the closest equivalent behavior (every entry expires `ttl`
after being written), documented in the function's own comment so this
difference isn't mistaken for measurement error.

### Delete churn, not isolated Delete

An isolated-Delete benchmark (pre-populate n keys, `b.Loop()` just calls
`Delete`) drains the cache to empty after n iterations, and `b.Loop()` can
run far more than n iterations at these key-pool sizes. The alternative —
rebuilding the cache every iteration — is exactly the trap
[ADR 0015](0015-delete-clear-api.md) already hit and fixed once (the
attempted-first version of the main module's own `BenchmarkDelete` hung for
minutes because `Delete`'s cost is too small relative to a full-cache
rebuild's cost, so `b.Loop()`'s auto-calibration picks an iteration count
in the millions).

Every `*_Delete` benchmark here instead does `Delete(k); Set(k, i)` per
iteration — delete then immediately reinsert the same key. This keeps the
cache at steady-state size n throughout, measures a real combined cost
(not isolated `Delete`), and avoids the rebuild trap entirely since
population happens once, outside `b.Loop()`.

### Why go-cache and freecache are missing from `Bounded`

go-cache has no eviction policy at all — nothing to configure. freecache
bounds by byte-buffer size, not entry count, so there's no way to configure
"evict once n/2 entries are stored" the way `WithMaxSize`/theine's
builder/otter's `MaximumSize` all do. Both are still covered by every other
category (`Set`/`Get`/`ParallelGet`/TTL/`Delete`); they're only absent from
`Bounded` because there's no comparable knob to turn.

### freecache's buffer sizing

freecache's byte-buffer is sized to `max(n*200, 10MB)` rather than a fixed
100MB as before — at n=1,000,000 a fixed 100MB budget risked the buffer
actually being under memory pressure and evicting keys the Get/Delete
benchmarks assume are still present, silently turning "hit" benchmarks into
a mix of hits and misses. Scaling with n keeps every size comfortably clear
of that ceiling.

### `ristretto`/theine/otter's "unbounded" sizing

For the non-`Bounded` categories, ristretto's `MaxCost` stays generously
above the working set (`1 << 30`, cost=1/entry) and theine/otter's
size limit is set to exactly `n` (the full key range) — large enough that
no eviction pressure occurs, so these benchmarks measure the same
"no eviction happening" shape as goache's own unbounded Set/Get. `Bounded`
deliberately halves each library's limit (`n/2`) against the same n-key
working set, forcing real, continuous eviction — same shape as goache's
main-module `BenchmarkSetWithMaxSize`/`BenchmarkEvictionChurn`.

## Consequences

See README.md's "Comparison with other Go cache libraries" section for the
full measured results across all five sizes and every category. Key
findings worth calling out here (only ones that changed the *qualitative*
picture from the single-size comparison, not just moved a number):

- **Every library's single-threaded `Get` degrades noticeably from 1,000 to
  1,000,000 entries** — go-cache, goache, otter, ristretto, and theine all
  show this. This is a universal effect of the working set outgrowing CPU
  cache as it grows, not a property of any one library's design — the
  single-size comparison couldn't have shown this at all, since it never
  tested past 100,000 entries.
- **Sharded/striped designs hold their `ParallelGet` advantage at every
  size**: goache, otter, and theine all stay in single-digit-to-low-double-digit
  ns/op even at 1,000,000 entries; go-cache's single global lock stays flat
  around 36-39 ns/op *regardless* of size, because its bottleneck was always
  lock contention, never cache size. Confirms the sharding argument at
  1,000x the scale the original single-size comparison tested.
- goache's `WithMaxSize` eviction cost (measured in `BenchmarkX_Bounded`)
  scales up noticeably and *reproducibly* with cache size across both runs
  described below — roughly 3.5-3.75x from 1,000 to 1,000,000 entries. This
  matches the locality regression [ADR 0016](0016-clock-eviction.md)
  already flagged in the abstract: goache's CLOCK ring is an intrusive
  linked list of individually heap-allocated entries, so a bigger shard
  means a bigger, more cache-unfriendly ring to walk on eviction.

### The methodologically important finding: re-running the exact same suite moved some libraries' numbers substantially

The user asked for this suite to be re-run a second time under a stricter
protocol — launched in the background, and this time *waited on with no
other work happening in parallel*, specifically so nothing else competing
for CPU could skew the timing. Comparing that clean re-run against the
first clean run (same machine, same code, no changes in between) surfaced
something more important than any single number: **goache, go-cache,
freecache, and otter reproduced closely across both runs, but ristretto and
theine did not.**

Concretely: theine's `Set` at n=1,000 moved from 271.7 to 157.1 ns/op (a
~42% swing) between two runs with nothing else different. More
significantly, **ristretto's `Bounded` scaling trend flipped direction**:
the first run showed it essentially flat across all five sizes (111.6 ->
117.8 ns/op, 1,000 -> 1,000,000); the second showed it clearly *decreasing*
with size (125.5 -> 76.34 ns/op). The original version of this ADR drew a
conclusion from the first run's "flat" reading (specifically, that
ristretto's frequency-sketch admission scales better than goache's CLOCK
ring) — that specific comparative claim did not survive a second clean run
and has been removed from README.md's takeaways; goache's own upward trend
in `Bounded` cost did reproduce and is the one solid finding here.

The most plausible explanation is architectural: ristretto and theine both
run real background machinery (ristretto's buffered-write goroutines and
admission-policy processing; theine's hierarchical timer wheel and
maintenance work) that competes for scheduler time independently of
whatever else is running on the machine, making their own benchmark numbers
inherently noisier than the synchronous libraries' (goache, go-cache,
freecache) or otter's. This means: **a single run of ristretto's or
theine's numbers should be treated as directional, not precise** — any
comparative claim resting on one of their measurements deserves a second
run before being trusted, a discipline this ADR failed to apply the first
time and is now recording so it isn't skipped again.

This ADR's job is the *methodology* decision (parametrize by size, cover
every feature with an equivalent, and — as of this update — don't trust a
single run for the noisier libraries); the numbers themselves live in
README.md and move every time `bench/` is re-run, per the repo's standing
performance policy (`CLAUDE.md`).
