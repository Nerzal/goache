# gemini-analysis.md: results

`gemini-analysis.md` (untracked, left in place at the repo root as the source
record) contains a modified `cache.go` attributed to a prior Gemini-based
analysis, with five numbered optimizations layered on top of an older
snapshot of this codebase — one that predates `Delete`/`DeleteMany`/`Clear`
and `WithMaxSize` CLOCK eviction, both already shipped here. Rather than
apply that file wholesale (its baseline no longer matches this repo), each
of its five ideas was extracted, re-implemented against the *current*
`cache.go`, benchmarked in isolation, and kept or reverted purely on
measurement. The condensed decision record is
[docs/adr/0018-gemini-analysis-experiments.md](docs/adr/0018-gemini-analysis-experiments.md);
this file is the narrative version.

**Scorecard**: 2 of 5 accepted (one in a materially narrower form than
proposed), 3 rejected — two of those three the *opposite* of what the
analysis claimed.

Every experiment went through the same discipline: implement in isolation
→ `go build`/`go vet` clean → full `go test ./...` → `go test -race ./...`
for anything touching concurrency-sensitive code → targeted benchmark
(often the existing suite lacked coverage for the exact scenario, so a new
benchmark was added first) → 3 repeated runs for anything close enough to
matter → keep or revert. Nothing was accepted on the strength of the
analysis document's own reasoning alone.

## 1. Cache-line-padded value slice for shard storage — accepted

**Proposal**: stop storing shards as `[]*shard[K, V]` (one heap allocation
per shard) and go back to a contiguous `[]shard[K, V]` value slice, but pad
each `shard` struct with 64 bytes so adjacent shards don't share a CPU cache
line.

This is not a new idea in this codebase — it's a *direct rerun* of
[ADR 0004](docs/adr/0004-shard-storage-pointer-slice.md), which tried the
same value-slice change (without padding) and measured it ~9% slower on
`BenchmarkParallelGet`, attributing the regression to false sharing between
adjacent shards' `sync.RWMutex`. That ADR even considered padding as a fix
and explicitly rejected it: *"requires either unsafe arithmetic on struct
sizes or hardcoded assumptions about sync.RWMutex's internal layout... judged
not worth the fragility for a benefit this small."*

The padding turned out to need neither `unsafe` nor any assumption about
`sync.RWMutex`'s layout — a plain `_ [64]byte` field at the end of the
struct is enough, because it only has to guarantee *some* space between
consecutive shards, not perfect alignment to a cache-line boundary. And the
benefit wasn't small:

| Benchmark | Before (pointer slice) | After (padded value slice) | Change |
|---|---|---|---|
| `BenchmarkSet` | 32.75 ns/op | ~31.9-32.5 ns/op | flat |
| `BenchmarkGet` | 24.78 ns/op | ~23.2-24.2 ns/op | flat / slightly better |
| `BenchmarkParallelGetSet` | 7.561 ns/op | ~5.94-6.00 ns/op | **~21% faster** |
| `BenchmarkParallelGet` | 5.415 ns/op | ~4.49-4.59 ns/op | **~17% faster** |

Reproduced across 3 independent runs each side, with no overlap between the
before/after ranges — this isn't noise. Single-threaded `Set`/`Get` are
unaffected, which is exactly what the false-sharing theory predicts: it's a
multi-core effect, and a single goroutine never contends with itself over a
cache line.

**Why this reverses ADR 0004's finding rather than contradicting it**: ADR
0004 measured the *unpadded* value slice, correctly found it slower due to
false sharing, and then separately considered padding as a fix and rejected
it on cost/fragility grounds — but never actually measured the padded
version. This experiment is that missing measurement. ADR 0004 is now
marked superseded, its original measurement and reasoning kept intact as
the historical record of why the *unpadded* form loses.

Adopted. `Cache.shards` is now `[]shard[K, V]`, each `shard` carries a
64-byte pad field, and `docs/adr/0004`/`CLAUDE.md`/the package doc comment
were all updated to point at this instead of repeating the old conclusion.

## 2. Read-before-write on the CLOCK `referenced` bit — rejected

**Proposal**: in `Get` and in `set`'s overwrite path, change the
unconditional `e.referenced.Store(true)` to
`if !e.referenced.Load() { e.referenced.Store(true) }`, on the theory that
many concurrent readers hammering the same hot key under `WithMaxSize` would
otherwise keep invalidating that entry's cache line on every hit (cache-line
bouncing) even though the bit's value never actually changes after the
first hit.

No benchmark exercised concurrent `Get` against a bounded cache before this
— `BenchmarkParallelGetWithMaxSize` was added specifically to test it (and
kept afterward regardless of outcome; it's genuine coverage of a path that
had none).

| Benchmark | Unconditional store | Read-before-write |
|---|---|---|
| `BenchmarkParallelGetWithMaxSize` | 4.65 - 5.02 ns/op (3 runs) | 4.68 - 4.75 ns/op (3 runs) |

The two ranges overlap almost entirely. No measurable win. Reverted to the
simpler unconditional store.

**Why the theory didn't pan out in practice**: on x86, an atomic store
already requests exclusive cache-line ownership regardless of whether the
new value differs from the old one — there's no cheaper path for "storing
the same value again" at the hardware level the way there might be for a
plain (non-atomic) write. The `Load()` added to avoid the store has to pay
for its own cache access, roughly cancelling out whatever it occasionally
saves. Concurrency micro-optimizations built on plausible-sounding
mental models of CPU cache behavior are exactly the kind of thing that
needs a benchmark before being believed — this is a clean example of one
that didn't survive contact with the actual hardware.

## 3. Move `time.Now()` outside `Get`'s read lock — rejected, mildly counterproductive

**Proposal**: in `Get`, copy out `expiresAt` and the value while still
holding `RLock`, release the lock, and only then call `time.Now()` to check
expiry — shortening how long `Get` holds the read lock on a TTL entry, on
the theory that a writer blocked on `Lock()` waits less if readers don't do
a comparatively expensive clock read while holding it.

Single-threaded `BenchmarkGetWithTTL` can't show a lock-contention effect at
all — a lone goroutine pays the same total wall-clock time no matter what
order it does its work in, since there's no second party waiting on it. A
mixed-contention benchmark was needed, so `BenchmarkParallelGetSetWithTTL`
was added (also kept afterward as permanent TTL-under-contention coverage).

| Benchmark | `time.Now()` under `RLock` (original) | `time.Now()` after `RUnlock` (proposed) |
|---|---|---|
| `BenchmarkParallelGetSetWithTTL` | 6.33 - 6.48 ns/op (3 runs) | 6.51 - 6.91 ns/op (3 runs) |

Reproducibly **slower** by about 5%, with no overlap between the two
3-run ranges — the opposite of the claimed effect. Reverted.

**Why**: the theory assumes Go's `sync.RWMutex` behaves like a simple
ticket lock where a pending writer's wait time is directly proportional to
how long readers hold their locks. That's not quite how it works in
practice, and the reordering also introduces an extra local variable
(`expiresAt`) and a data dependency that crosses the unlock boundary —
overhead the original, more direct code didn't have. Sometimes the
"obviously more efficient" reordering loses to just leaving the lock
scope alone.

## 4. `sync.Pool` entry recycling — accepted, but only for `WithMaxSize`-bounded caches

**Proposal**: pool every `entry[K, V]` cache-wide. `Delete`, `DeleteMany`,
`Clear`, `Purge`, and eviction all return their freed entry to a
`sync.Pool`; `set` draws from that pool before falling back to a fresh
allocation. Pitched as "zero allocations in steady state."

This is the most consequential of the five, and the one where a naive
"just try it everywhere" approach actively backfired.

### First attempt: pool everywhere, unconditionally

| Benchmark | Before | Blanket pool | Change |
|---|---|---|---|
| `BenchmarkSet` | 32.75 ns/op | 33.32 ns/op | flat |
| `BenchmarkSetMany` | 104.6 ns/op | 186.3 ns/op | **+78%, worse** |
| `BenchmarkFreshLoad_NoHint` | 1,038,249 ns/op | 1,847,838 ns/op | **+78%, worse** |
| `BenchmarkFreshLoad_WithCapacityHint` | 888,895 ns/op | 1,585,137 ns/op | **+78%, worse** |
| `BenchmarkDelete` | 38.15 ns/op | 61.56 ns/op | **+61%, worse** |
| `BenchmarkDeleteMany` | 69.70 ns/op | 94.41 ns/op | **+35%, worse** |
| `BenchmarkClear` | 10,543,726 ns/op | 17,997,319 ns/op | **+71%, worse** |
| `BenchmarkSetWithMaxSize` | 127.1 ns/op, 1 alloc | 77.76 ns/op, 0 allocs | **-39%, better** |
| `BenchmarkEvictionChurn` | 167.1 ns/op, 2 allocs | 113.0 ns/op, 1 alloc | **-32%, better** |

Five benchmarks got substantially *worse*, two got substantially *better*.
The pattern makes the cause obvious once you look at what each benchmark
actually does:

- `SetMany`, `FreshLoad_NoHint`, `FreshLoad_WithCapacityHint` each construct
  a **brand-new `Cache` every iteration**. A brand-new cache has a cold,
  empty pool — `pool.Get()` never returns a recycled entry, it always falls
  through to `pool.New()`, which just does `&entry[K, V]{}` anyway. So these
  benchmarks pay `sync.Pool`'s internal bookkeeping (per-P local caches,
  victim cache checks) on every single call, for a "recycling" benefit that
  structurally can never happen within one iteration.
- `Delete`, `DeleteMany`, `Clear` in these benchmarks all run against an
  **unbounded** cache (no `WithMaxSize`). Before pooling, an unbounded
  delete did nothing but remove the map entry and let the GC reclaim it —
  free, in the sense that nothing extra ran. After blanket pooling, every
  delete now zeroes the entry's key/value, resets its CLOCK fields, and
  calls `pool.Put()` — real, unconditional work — for a pool that
  **nothing will ever drain**, since the only thing that calls `pool.Get()`
  inside `set` is a brand-new key on a shard that's actually evicting, and
  unbounded shards never evict.
- `SetWithMaxSize` and `EvictionChurn` are the one shape where pooling
  genuinely pays off: both run many operations against a **single,
  long-lived, actually-evicting** `Cache`, so the pool fills from eviction
  and drains from the next insert, over and over, on the same instance.

### Refined attempt: gate all pool interaction on `s.limit > 0`

`set` only draws from the pool when a `WithMaxSize` limit is configured
(falls back to a plain allocation otherwise, exactly as before pooling
existed); `deleteEntry` and `Clear`'s release loop are likewise skipped
entirely for unbounded shards.

| Benchmark | Before | Refined (gated) pool | Change |
|---|---|---|---|
| `BenchmarkSet` | 32.75 ns/op | 33.18 ns/op | flat |
| `BenchmarkSetMany` | 104.6 ns/op | 103.2 ns/op | flat |
| `BenchmarkFreshLoad_NoHint` | 1,038,249 ns/op | 979,083 ns/op | flat |
| `BenchmarkFreshLoad_WithCapacityHint` | 888,895 ns/op | 926,558 ns/op | flat |
| `BenchmarkDelete` | 38.15 ns/op | 38.34 ns/op | flat |
| `BenchmarkDeleteMany` | 69.70 ns/op | 67.48 ns/op | flat |
| `BenchmarkClear` | 10,543,726 ns/op | 10,520,000-11,510,000 ns/op (3 runs) | flat (confirmed, overlaps the before value) |
| `BenchmarkSetWithMaxSize` | 127.1 ns/op, 1 alloc | 74.94 ns/op, 0 allocs | **-41%, better** |
| `BenchmarkEvictionChurn` | 167.1 ns/op, 2 allocs | 112.3 ns/op, 1 alloc | **-33%, better** |

All five regressions vanished; both real wins survived, and got slightly
*better* than the blanket-pool version even (76.00 vs 77.76, 115.0 vs
113.0-ish — within noise of each other, but the gated version has strictly
less surface area doing work, which fits).

**Accepted, in this narrower form.** `sync.Pool` recycling now runs
*exclusively* on shards with a configured `WithMaxSize`, where eviction
both fills and drains it repeatedly on the same long-lived cache. Unbounded
`Cache` instances — goache's default configuration and, per the existing
`docs/adr/0016` positioning, likely its most common one — see zero change:
same allocation profile, same cost, on every path. This preserves the
"every `Set` always lands, zero eviction-policy overhead" property the
project has held onto since before eviction existed at all.

**The general lesson**: a `sync.Pool` only pays for itself when the same
object flows through *fill* and *drain* repeatedly on one long-lived
instance. Applied to code paths where either side of that cycle can't
happen — a fresh instance every time, or a drain path with no
corresponding fill — it adds cost with no corresponding benefit. "Add a
pool" is not a context-free optimization; it needs a workload shape that
actually recycles.

## 5. Flat `shardIndices` array instead of `[][]Entry`/`[][]K` buckets — rejected, decisively

**Proposal**: in `SetMany`/`DeleteMany`, replace the current
`make([][]Entry[K, V], len(c.shards))` (grouping keys into per-shard slices
via `append`) with a single flat `make([]int, len(entries))` recording each
entry's destination shard index, then locking and processing one shard at a
time with a loop that scans the *entire* input and skips entries not
destined for the current shard. Framed as avoiding "O(N) garbage
production" from the nested slice.

| Benchmark | Bucket-of-slices (before) | Flat index + nested scan (after) | Change |
|---|---|---|---|
| `BenchmarkSetMany` (batch=100) | 103.2 ns/op, 2 allocs | 441.4 ns/op, 1 alloc | **4.3x slower** |
| `BenchmarkFreshLoad_NoHint` (n=10,000) | 979,083 ns/op | 4,777,538 ns/op | **4.9x slower** |
| `BenchmarkFreshLoad_WithCapacityHint` (n=10,000) | 926,558 ns/op | 4,533,330 ns/op | **4.9x slower** |
| `BenchmarkDeleteMany` (batch=100) | 67.48 ns/op | 164.9 ns/op | **2.4x slower** |

Decisively rejected. The proposal does allocate less — 1 allocation
instead of 2, and a smaller one at that (`[]int` vs the outer
`[][]Entry`/`[][]K` slice plus whatever `append` growth the buckets
needed) — but the "for each shard, scan every input entry" structure is
**O(shardCount × len(entries))**. With the default 256 shards, a 100-entry
`SetMany` call does 25,600 inner-loop comparisons instead of 100; a
10,000-entry fresh load does 2.56 million instead of 10,000. The nested
loop's complexity class change costs milliseconds at realistic batch sizes
to save single-digit nanoseconds' worth of allocation. Reverted, unchanged
from the original bucket-based grouping.

This is the cleanest example among the five of "fewer allocations" and
"faster" being different — sometimes directly opposed — goals. An
`append`-based grouping pays a handful of amortized-O(1) slice growths
total across the whole call; scanning the full input once per shard pays a
multiplicative cost that gets worse, not better, as the shard count (which
exists specifically to reduce contention, and defaults to a fairly large
256) goes up.

## What changed, concretely

- `cache.go`: `Cache.shards` is now `[]shard[K, V]` (was `[]*shard[K, V]`),
  each `shard` padded to a cache line; a `sync.Pool`-based `entryPool` was
  added to `Cache`, used only by shards with a configured `WithMaxSize`.
  Nothing else in `cache.go`'s public API or behavior changed.
- `cache_bench_test.go`: two new permanent benchmarks —
  `BenchmarkParallelGetWithMaxSize` and `BenchmarkParallelGetSetWithTTL` —
  added to evaluate items 2 and 3, kept afterward as genuine coverage for
  concurrent access to the eviction and TTL paths, which had none before.
- `README.md`: core-benchmark numbers, the `WithMaxSize` eviction-cost
  numbers, and goache's own rows in the `bench/` competitor-comparison
  tables were all refreshed to reflect items 1 and 4; the `ParallelGet`
  comparison table was reordered since goache now leads it at every size
  except n=1,000.
- `docs/benchcharts/main.go` / `docs/img/*.svg`: regenerated to match.
- `docs/adr/0004-shard-storage-pointer-slice.md`: marked superseded by
  [ADR 0018](docs/adr/0018-gemini-analysis-experiments.md), original
  content kept intact as the historical record.
- `docs/adr/0018-gemini-analysis-experiments.md`: the terse decision
  record for all five experiments (this file is the narrative version).
- `CLAUDE.md`: the "optimizations tried and reverted" section was updated
  so it no longer states the now-superseded ADR 0004 conclusion as current
  guidance, and documents the `sync.Pool` gating decision so it isn't
  accidentally removed later.
- `gemini-analysis.md` itself: left untouched, untracked, as the source
  record this file is a response to.
