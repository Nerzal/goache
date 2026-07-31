# 0021: Reject inline entry storage for unbounded caches (T3)

## Status

Rejected / reverted

## Context

Task T3 from [docs/performance-analysis.md](../performance-analysis.md), the
largest item on that backlog and the one it called "biggest total win".

[ADR 0016](0016-clock-eviction.md) changed shard storage from
`map[K]entry[V]` (values inline) to `map[K]*entry[K, V]` (individually
heap-allocated entries) so `Get` could flip a CLOCK recency bit while
holding only a read lock. That cost is paid by **every** cache, including
ones that never call `WithMaxSize` and therefore never evict: one heap
allocation per new key, one pointer dereference per lookup, and a measured
~2x locality regression on full-map sweeps (`Purge`, `Clear`).

T3's hypothesis: give unbounded shards their own inline storage
(`map[K]inlineEntry[V]`, holding just value + expiry) and keep the pointer
map only for `WithMaxSize`-configured shards, which are the only ones that
need individually addressable entries. Predicted: `Get` ≤21.5 ns/op (from
dropping the pointer chase), `Purge` ≤3.5ms, ~10,000 fewer allocations per
`FreshLoad`, with the bounded path untouched.

Pre-registered accept/reject criteria (from the task, written before any
code): **reject if bounded paths regress >3%, or the unbounded `Get` win is
under 1.5ns.**

## What was implemented

A full dual-mode shard: `inline map[K]inlineEntry[V]` and
`data map[K]*entry[K, V]`, exactly one non-nil per shard, chosen in `New`
from whether `WithMaxSize` was set. Every operation
(`set`/`Get`/`Delete`/`DeleteMany`/`Clear`/`Len`/`Purge`) branches on
`s.limit == 0`. The unbounded `set` collapsed to a single map assign
covering both insert and overwrite — no separate lookup, no allocation, no
ring. [ADR 0019](0019-single-slot-freelist.md)'s freelist was removed as
dead code, since inline storage has no per-entry allocation to recycle.

Build, `go vet`, full test suite, and `-race` (including a 3x repeat of the
concurrency tests) all passed. This was a working implementation, rejected
on measurement, not on correctness.

## Measurement

AMD Ryzen AI 9 HX 370 (24 threads), 3 runs per side, before/after with only
this change in between.

**The wins were real and large:**

```
                                        before             after
BenchmarkPurge-24                       5.13-5.32 ms/op    2.52-2.64 ms/op    -51%
BenchmarkClear-24                       10.4-10.7 ms/op    7.86-8.20 ms/op    -25%
                                        106,406 allocs/op  6,406 allocs/op    -100,000
BenchmarkFreshLoad_WithCapacityHint-24  858-876 us/op      589-636 us/op      -30%
                                        12,748 allocs/op   2,748 allocs/op    -10,000
BenchmarkFreshLoad_NoHint-24            965-973 us/op      846-1018 us/op     flat
                                        14,008 allocs/op   4,008 allocs/op    -10,000
BenchmarkSetMany-24                     103-116 ns/op      86.2-87.4 ns/op    -17%, 2->1 allocs
BenchmarkDelete-24                      39.6-39.9 ns/op    33.0-33.3 ns/op    -17%
BenchmarkDeleteSetChurn-24              86.5-87.6 ns/op    71.8-72.7 ns/op    -17%
BenchmarkDeleteMany-24                  69.8-72.6 ns/op    64.3-64.4 ns/op    -9%
BenchmarkParallelGet-24                 4.63-4.70 ns/op    4.56-4.58 ns/op    flat
```

**But the predicted `Get` win never materialized, and concurrent mixed
read/write regressed badly:**

```
                                        before             after
BenchmarkGet-24                         23.32-23.37 ns/op  23.12-23.83 ns/op  flat (predicted <=21.5)
BenchmarkGetMiss-24                     56.5-56.9 ns/op    58.4-59.4 ns/op    +4%
BenchmarkGetWithTTL-24                  27.85-28.12 ns/op  28.23-29.63 ns/op  +3%
BenchmarkSetWithTTL-24                  43.33-45.03 ns/op  45.68-46.68 ns/op  +4%
BenchmarkParallelGetSet-24              6.19-6.41 ns/op    7.35-7.96 ns/op    +20%
BenchmarkParallelGetSetWithTTL-24       6.73-6.89 ns/op    8.00-8.04 ns/op    +17%
BenchmarkSetWithMaxSize-24              80.5-86.1 ns/op    86.4-94.0 ns/op    +6%   (bounded)
BenchmarkGetWithMaxSize-24              27.4-27.8 ns/op    28.2-28.6 ns/op    +3%   (bounded)
BenchmarkEvictionChurn-24               114.4-116.4 ns/op  113.0-113.4 ns/op  flat  (bounded)
BenchmarkParallelGetWithMaxSize-24      4.86-5.00 ns/op    4.82-4.89 ns/op    flat  (bounded)
```

Both pre-registered reject conditions were hit: bounded paths regressed
more than 3% (`SetWithMaxSize` +6%, `GetWithMaxSize` +3%), and the unbounded
`Get` win was negative rather than ≥1.5ns.

### Why `Get` didn't get faster

The pointer chase T3 set out to eliminate is nearly free in these
benchmarks. Entries are allocated in a tight loop during setup, so they land
contiguously on the heap and the dereference hits warm, prefetch-friendly
memory. Meanwhile inline storage makes the map's value 16 bytes
(`V` + `int64`) instead of an 8-byte pointer, doubling the bucket payload —
more memory traffic per probe, worse density for the map itself. The two
roughly cancel, with the fatter map winning slightly on misses (where the
zero value of a 16-byte struct must be produced instead of a nil pointer).

### Why concurrent mixed read/write regressed 20% — the important finding

This one wasn't anticipated at all, and it's the finding worth keeping.

In pointer mode, overwriting an existing key does `mapaccess` (a *read* of
the bucket array) followed by a write **through the pointer**, into a
separate heap object. The map's bucket array is never modified. In inline
mode, the same overwrite is a `mapassign` that writes **into the bucket
array itself** — the exact memory concurrent readers are probing.

Under `BenchmarkParallelGetSet` (24 goroutines, 90% Get / 10% Set), that
turns every write into a cache-line invalidation on memory every reader
core is actively touching. Pointer mode confines the dirtying to the
entry object, which readers only touch if they happen to look up that same
key. This is the same class of effect as the false sharing
[ADR 0018](0018-gemini-analysis-experiments.md) fixed with shard padding,
one level down: inline storage co-locates hot written data with hot read
data.

It only shows up in a mixed *concurrent* benchmark. Single-threaded `Set`
measured flat, which is why the first measurement pass — which didn't
include `ParallelGetSet` in the controlled before/after set — looked like a
clean win.

### Methodology note

The first pass compared `ParallelGetSet`, `SetWithTTL`, and the bounded
benchmarks against numbers recorded in an *earlier session* rather than
against a controlled baseline, because those benchmarks weren't in the
before-run. That is exactly the error
[ADR 0017](0017-size-parametrized-benchmarks.md) warns about and
[ADR 0020](0020-shard-count-does-not-scale-eviction.md) already tripped
over once. The numbers above come from reconstructing the exact pre-T3
source and re-measuring both sides back to back. Without that step the
20% concurrency regression would have been dismissed as cross-session noise
and shipped.

## Decision

**Rejected and reverted.** goache is a concurrent cache; `ParallelGetSet` is
the benchmark closest to steady-state serving under load, and the sharded
design exists specifically to make that case fast. Trading 20% there — plus
a bounded-path regression that violated a pre-registered criterion — for
gains concentrated in periodic maintenance (`Purge`, `Clear`) and startup
(`FreshLoad`) is the wrong trade for this library's positioning.

[ADR 0019](0019-single-slot-freelist.md)'s freelist is restored and remains
the accepted answer for the delete-then-reinsert allocation, which was the
one steady-state cost T3 would have improved.

## Consequences

- `cache.go` is unchanged from its pre-T3 state. The costs
  [ADR 0016](0016-clock-eviction.md) documented (one allocation per new key,
  the full-sweep locality regression) remain, now with a measured price for
  the obvious fix.
- **Do not re-attempt inline storage without a plan for the concurrent-write
  problem.** The allocation and sweep wins are genuinely large and will keep
  looking tempting in a profile; they cost 20% of concurrent mixed
  read/write throughput to obtain. Any future attempt needs to break the
  co-location of written values and probed buckets — not merely re-derive
  that inline storage allocates less.
- A `WithInlineStorage()` opt-in was considered and not pursued: it adds
  public API for a trade-off callers cannot evaluate without benchmarking
  their own read/write mix, and would double the test matrix for every
  operation permanently.
- T3 was the backlog's largest projected win. With it rejected, the
  remaining open items are T4 (bucket-slice reuse in `SetMany`/`DeleteMany`)
  and T5 (contiguous CLOCK bitmap for large bounded caches), the latter now
  the only standing candidate for the `Bounded`-at-1M gap after
  [ADR 0020](0020-shard-count-does-not-scale-eviction.md) ruled out shard
  count.
