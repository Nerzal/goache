# 0020: Shard count does not scale bounded eviction (T2 rejected); WithMaxSize made a hard bound

## Status

Rejected (the shard-count hypothesis) / Accepted (the capacity fix it uncovered)

## Context

Task T2 from [docs/performance-analysis.md](../performance-analysis.md).
The size-parametrized comparison ([ADR 0017](0017-size-parametrized-benchmarks.md))
showed goache's `Bounded` category (Set against a cache evicting under real
pressure) degrading ~5.5x from n=1,000 to n=1,000,000, losing to ristretto
at the top size. [ADR 0016](0016-clock-eviction.md) attributed that to the
CLOCK hand walking an intrusive linked list of individually heap-allocated
entries — a chain of dependent cache misses that gets longer as shards get
fuller.

The hypothesis: at n=1,000,000 with the default 256 shards, each shard holds
roughly 1,950 entries, so raising the shard count would shorten every ring
and cut the walk. Phase 1 was deliberately zero-code — measure
`WithShardCount(256/512/1024/2048/4096)` against the same workload before
changing any default.

## Measurement

Temporary benchmark mirroring `bench/`'s `BenchmarkGoache_Bounded` shape
(limit = n/2 against an n-key pool, so every Set past the fill point
evicts). AMD Ryzen AI 9 HX 370 (24 threads):

```
                    shards=256  512     1024    2048    4096
n=1,000              47.18      47.78   45.05   39.38   33.73   ns/op
n=100,000            78.58      81.89   86.21   92.49   98.22   ns/op
n=1,000,000         245.7      249.1   255.0   271.9   292.2    ns/op
```

Concurrent-read guard (`ParallelGet` on a bounded 1M cache): 11.29 / 11.64 /
11.29 ns/op at 256 / 1024 / 4096 shards — flat, no harm and no help.

**The hypothesis is refuted, and inverted.** At the sizes that motivated it,
more shards are *worse*: +25% at n=100,000 and +19% at n=1,000,000 going
from 256 to 4096 shards. The CLOCK walk is evidently not the dominant cost
at these sizes; spreading the same working set over more shards costs more
than the shorter rings save. Each extra shard adds its own map header and
bucket array, and the padded shard array itself grows (4096 shards ≈ 800KB),
so a Set's random shard access misses cache and TLB more often — the ring
walk it saves is shorter, but everything around it got more expensive.

Phase 2 (auto-deriving shard count from `maxSize`) is therefore **not
implemented**: it would have made large bounded caches measurably slower.

### The n=1,000 row is not a win — it exposed a capacity defect

The apparent 28% improvement at n=1,000 turned out to be an artifact of a
real bug. Per-shard limits were `max(ceil(maxSize/shardCount), 1)`. Once the
shard count exceeds `maxSize`, that floor of 1 makes the cache's true
capacity equal to the *shard count*, not `maxSize`. Confirmed directly:

```
WithMaxSize(500) WithShardCount(256):  Len()=512   (shardLimit=2)
WithMaxSize(500) WithShardCount(4096): Len()=4096  (shardLimit=1)
```

The 4096-shard configuration was faster because it was holding 8.2x more
entries than requested, so it evicted far less often against the same
1,000-key pool. Not a speedup — a violated bound.

This was not exotic-configuration-only: on the **default** 256 shards, any
`WithMaxSize(n)` with n < 256 silently held up to 256 entries
(`WithMaxSize(100)` → 256, 2.56x over), and even large values overshot by up
to shardCount-1 from the per-shard `ceil` rounding.

## Decision

1. **Shard count stays at its documented default of 256**
   ([ADR 0007](0007-shard-count-default.md) unchanged), and
   `WithShardCount`'s doc comment now states explicitly that raising it is
   not a way to make bounded eviction faster, with the measured numbers, so
   this isn't re-explored on intuition.
2. **`WithMaxSize(n)` is now a hard upper bound.** The budget is distributed
   so per-shard limits sum to exactly n: every shard gets `n/shardCount`,
   and the first `n%shardCount` shards get one extra slot. `Len()` can
   therefore never exceed n.
3. **When n is below the shard count, the shard count is lowered** to the
   largest power of two ≤ n, so every shard still gets at least one slot
   without inflating capacity. A cache that small has no contention problem
   that more shards would solve.

`TestWithMaxSizeIsAHardUpperBound` (limits sum to exactly maxSize, every
shard ≥ 1, `Len()` ≤ maxSize after 20x overfill, across maxSize 1 / 7 / 100 /
500 / 1000 / 12345) and `TestWithMaxSizeCapsShardCount` pin this.

## Consequences

No performance cost — the change is entirely in `New`, and the per-shard
`limit` field is read exactly as before. Verified with a surgical revert to
get a true 3-run baseline on both sides (the first comparison mistakenly
read a ~4% Get "regression" off a single-run number from an earlier session
— exactly the methodology error [ADR 0017](0017-size-parametrized-benchmarks.md)
warns about):

```
                              ceil (before)      exact (after)
BenchmarkSetWithMaxSize-24    82.45-90.30 ns/op  78.20-79.63 ns/op
BenchmarkGetWithMaxSize-24    26.84-27.70 ns/op  27.53-27.68 ns/op
BenchmarkParallelGetWithMaxSize-24  4.810-4.962  4.859-5.425 ns/op
```

Flat to slightly better; nothing regressed.

**Behavior change for existing users**: a cache configured with a `maxSize`
near or below its shard count now holds fewer entries than it did before —
correctly so, but it *is* a change. Callers who relied on the old
over-allocation were relying on a bug; the doc comment previously promised
"roughly n" and now promises "at most n", which is strictly more useful.

`Bounded` performance at n=1,000,000 remains the open problem T2 set out to
fix. The remaining candidate is
[docs/performance-analysis.md](../performance-analysis.md)'s T5 (moving
CLOCK referenced-bits into a contiguous per-shard bitmap so the hand scans
linearly instead of pointer-chasing), which attacks the walk's locality
directly rather than trying to shorten it. T2's result makes T5 *more*
interesting, not less: it rules out the cheap explanation and leaves
locality as the one standing.
