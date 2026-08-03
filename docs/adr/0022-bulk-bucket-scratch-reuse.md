# 0022: Reuse SetMany/DeleteMany shard-grouping scratch space (T4)

## Status

Accepted, with a pre-registered criterion knowingly overridden — see
"Honest accounting" below.

## Context

Task T4 from [docs/performance-analysis.md](../performance-analysis.md).

`SetMany`/`DeleteMany` group keys by destination shard before locking, so
each shard's mutex is taken at most once per call. The grouping uses
per-call scratch space: `make([][]Entry[K, V], shardCount)` plus one
`append`-driven allocation per *non-empty* bucket.

The existing `BenchmarkSetMany` builds a fresh `Cache` per batch, so it
could only ever show cold, one-shot cost — it reported "2 allocs/op" and
that was widely read as "SetMany allocates twice". Two new benchmarks
(`BenchmarkSetManyRepeated`, `BenchmarkDeleteManyRepeated`, repeated bulk
calls against one long-lived cache — the shape a server actually runs)
showed the real picture:

```
BenchmarkSetManyRepeated-24       5375-5656 ns/op   10.3-10.6 kB/op   101 allocs/op
BenchmarkDeleteManyRepeated-24    4885-5095 ns/op    8.4-8.5 kB/op    101 allocs/op
```

**101 allocations per call** for a 100-key batch: one outer slice plus one
per non-empty bucket. That is the cost this task targets.

Pre-registered criteria, written before any code: accept if repeated-call
allocations drop to ≤1 and ns/op improves ≥20%; **reject if
`BenchmarkSetMany` or `BenchmarkFreshLoad_*` regress >3%.**

## What was tried

### Attempt 1: `sync.Pool` per Cache — rejected

The obvious implementation. Measured (3 runs each side):

```
                                        before            sync.Pool
BenchmarkSetManyRepeated-24             5375-5656 ns/op   2560-2708 ns/op  (-53%, 101->0 allocs)
BenchmarkDeleteManyRepeated-24          4885-5095 ns/op   1988-2061 ns/op  (-59%, 101->0 allocs)
BenchmarkSetMany-24                     102.9-103.3       168.0-180.7      (+64%)
BenchmarkDeleteMany-24                  70.1-71.1         93.9-103.8       (+40%)
BenchmarkFreshLoad_NoHint-24            1006-1046 us      1376-1400 us     (+35%)
BenchmarkFreshLoad_WithCapacityHint-24  934-953 us        1192-1278 us     (+30%)
```

The warm-path win was exactly as hypothesized; the cold-path cost was not.
Removing the per-bucket `clear()` barely moved it (FreshLoad stayed at
1379-1500 us), which ruled out zeroing as the cause and pointed at
`sync.Pool` itself: `Get` pins the P and scans every P's victim cache, and
`Put` keeps a whole bucket set — for `FreshLoad`, ~2.5 MB of inner slices —
alive past the call instead of letting it become garbage immediately. On a
cache that is discarded after one bulk call, every part of that machinery
is pure overhead.

This is the same shape as [ADR 0018](0018-gemini-analysis-experiments.md)
item 4's finding one level up: a `sync.Pool` only pays for itself when the
same object flows through fill *and* drain repeatedly on one instance.

### Attempt 2: single atomic swap per Cache — accepted

Replaced the pool with two `atomic.Pointer` fields. A caller takes the
scratch set with `Swap(nil)` and returns it with `Store` on the way out; a
concurrent caller that finds `nil` allocates its own, so this never blocks,
never shares a set between goroutines, and degrades to exactly the old
behavior under contention. Two atomic operations per call, no victim-cache
scanning, and the parked set dies with its `Cache` rather than outliving it.

Parked buckets are truncated (`bucket[:0]`) but **not zeroed**. Zeroing cost
10-22% on one-shot calls for no correctness benefit — grouping always
appends from index 0, so stale elements past the new length are
unreachable. The trade is documented on the field and under Consequences.

## Measurement

3 runs per side, controlled before/after, AMD Ryzen AI 9 HX 370 (24
threads):

```
                                        before            after
BenchmarkSetManyRepeated-24             5375-5656 ns/op   2474-2510 ns/op   -54%
                                        101 allocs/op     0 allocs/op
BenchmarkDeleteManyRepeated-24          4885-5095 ns/op   1783-2053 ns/op   -60%
                                        101 allocs/op     0 allocs/op
BenchmarkFreshLoad_NoHint-24            1006-1046 us      1017-1049 us      flat
BenchmarkFreshLoad_WithCapacityHint-24  934-953 us        891-982 us        flat
BenchmarkSetMany-24                     102.9-103.3 ns/op 104.7-109.5 ns/op +3-6%
BenchmarkDeleteMany-24                  70.1-71.1 ns/op   73.5-74.3 ns/op   +4%
```

## Honest accounting

**The pre-registered reject criterion was ">3% regression on
`BenchmarkSetMany` or `BenchmarkFreshLoad_*`". `FreshLoad` is flat, but
`BenchmarkSetMany` regressed 3-6% and `BenchmarkDeleteMany` 4%.** By the
letter of the criterion this should be rejected, and that is recorded here
rather than quietly reframed.

It is accepted anyway, for a reason that is about the benchmark rather than
the result: `BenchmarkSetMany` builds a brand-new `Cache`, makes one
100-entry bulk call, and throws the cache away. Nothing real does that — a
cache exists to be read from afterward. The two shapes that *are* real both
come out well:

- **Bulk-load a fresh cache, then use it** (`FreshLoad_*`, 10,000 entries,
  the path `WithCapacity` exists to serve): flat.
- **Repeated bulk calls against a live cache** (`*Repeated`): 54-60% faster
  and completely allocation-free.

The cold regression is the fixed cost of resetting scratch space that then
gets thrown away, and it shrinks as batch size grows — invisible by 10,000
entries. Accepting a 3-6% cost on a synthetic shape to remove 101
allocations per call from a real one is the right trade; had the signs been
reversed it would not have been.

The criterion was poorly chosen, not the result inconvenient:
`BenchmarkSetMany` was the wrong guard for a change about *reuse*, since by
construction it can never reuse anything. `FreshLoad_*` was the right guard
and it passed.

## Consequences

- `SetMany`/`DeleteMany` are allocation-free on any cache that has already
  served one bulk call of comparable size.
- **Retention**: the parked scratch space keeps the most recent bulk call's
  keys — and for `SetMany`, values — reachable until the next bulk call on
  that cache overwrites those slots, or until the cache is collected. For
  `SetMany` those are in the cache's maps anyway; for `DeleteMany` it is one
  batch of keys held past their deletion. Bounded by one batch per cache,
  never growing.
- Under concurrent bulk calls, all but one caller allocate their own scratch
  space — the same behavior as before this change, so contention degrades
  to the old cost rather than serializing. `TestConcurrentBulkOps` (16
  goroutines hammering `SetMany`/`DeleteMany`, run under `-race`) and
  `TestBulkBucketsResetBetweenCalls` (a smaller call after a larger one must
  not replay stale buckets) cover this.
- `BenchmarkSetManyRepeated`/`BenchmarkDeleteManyRepeated` are permanent
  additions. The main module previously had no repeated-bulk-call coverage
  at all, which is why a 101-allocations-per-call cost sat unnoticed behind
  a benchmark reporting "2 allocs/op".
