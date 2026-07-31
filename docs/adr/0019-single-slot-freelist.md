# 0019: Per-shard single-slot entry freelist for unbounded shards

## Status

Accepted

## Context

Task T1 from [docs/performance-analysis.md](../performance-analysis.md).
The size-parametrized comparison ([ADR 0017](0017-size-parametrized-benchmarks.md))
showed goache losing the Delete category (measured as delete-then-reinsert
churn) to go-cache at every size and to freecache up to n=100,000. The
profile-backed diagnosis: on an *unbounded* cache, the reinsert half of the
cycle is always a brand-new key, so it pays the one-heap-allocation-per-new-key
cost [ADR 0016](0016-clock-eviction.md) introduced — go-cache's reinsert is
a plain inline map assign (zero alloc) and freecache writes into its ring
buffer. The eviction-path `sync.Pool` from
[ADR 0018](0018-gemini-analysis-experiments.md) deliberately excludes
unbounded shards (a cold pool is pure overhead), so it can't help here.

A new main-module benchmark, `BenchmarkDeleteSetChurn` (unbounded cache,
`Delete(k); Set(k, i)` per iteration over a 100k key pool), was added first
to pin the diagnosis: **124.8 ns/op, 64 B/op, exactly 1 alloc/op** — the
entry allocation, as predicted.

## Decision

Each shard gets a one-entry freelist: `free *entry[K, V]`, only ever
touched under the shard's write lock.

- `Delete`/`DeleteMany` on an unbounded shard (`limit == 0`) park the
  removed entry: `s.free = e`. One pointer store — **no zeroing, no
  `sync.Pool`, no atomics**. Zeroing is unnecessary because `set`
  overwrites `key`, `value`, and `expiresAt` on reuse, and unbounded shards
  never touch `referenced`/`prev`/`next` (permanently zero).
- `set` for a brand-new key takes it back (`e = s.free; s.free = nil`)
  before falling back to a fresh allocation.
- Bounded shards are untouched — their delete path already recycles
  through `Cache.entryPool` (ADR 0018), which `set`'s eviction path drains.

This dodges the blanket-pooling trap ADR 0018 item 4 measured (35-78%
regressions from cold-pool overhead and pay-with-no-payback release work)
by construction: the park is ~1ns of work with no bookkeeping, and the
worst case — a workload that deletes but never reinserts — retains at most
one entry per shard (256 by default), whose old key/value stay reachable
only until the next new-key `set` on that shard. Bounded, negligible.

`TestDeleteParksEntryForReuse` pins the semantics: the parked entry is
byte-identical the same allocation on reuse, a reused entry inherits no TTL
from its previous life, overwrites don't consume the freelist, and bounded
shards never populate it.

## Measurement

AMD Ryzen AI 9 HX 370 (24 threads), 3 runs each side:

```
                              before                 after
BenchmarkDeleteSetChurn-24    124.7-124.8 ns/op      87.5-93.6 ns/op   (-28%)
                              64 B/op, 1 alloc/op    0 B/op, 0 allocs/op
BenchmarkDelete-24            39.2-39.5 ns/op        41.1-41.7 ns/op   (+~2ns: the park store)
BenchmarkDeleteMany-24        68.5-72.0 ns/op        67.9-70.7 ns/op   (flat)
BenchmarkSet-24               31.6-37.3 ns/op        31.8-33.1 ns/op   (flat)
BenchmarkSetMany-24           99.7-145 ns/op         104.2-113.4 ns/op (flat, within noise)
```

Cross-library Delete churn (`bench/`, goache row only re-run — competitors'
code didn't change):

```
n=          1,000    5,000    50,000   100,000  1,000,000
before      93.27    105.1    118.9    123.7    220.5     (1 alloc/op)
after       66.89    66.37    82.49    95.79    215.4     (0 allocs/op)
go-cache    53.64    58.12    68.42    72.00    191.4
freecache   83.20    102.9    159.0    200.4    274.5
```

goache now beats freecache at **every** size in this category (previously
lost up to n=100,000) and closed 54-66% of the gap to go-cache. The
remaining gap is structural: the churn cycle pays goache's shard-routing
hash twice per iteration (once in Delete, once in Set) plus one entry
indirection — costs go-cache's single unsharded map doesn't have, and pays
back thousandfold in `ParallelGet` (see README's comparison tables).

Accept criteria from the task (churn ≥25% better, `BenchmarkDelete` ≤ ~42
ns/op, Set/SetMany flat): all met.

## Consequences

- `entry` reuse on unbounded shards means a deleted entry's key/value can
  remain reachable (not GC-collectable) until the next new-key `set` on
  that shard — at most 256 entries by default. Documented on the `free`
  field.
- [docs/performance-analysis.md](../performance-analysis.md) T3 (inline
  entry storage for unbounded caches), if it lands, removes the per-entry
  allocation entirely and with it this freelist — T1 was sequenced first
  because it's ~15 lines and proves the allocation diagnosis T3's larger
  investment rests on. This ADR should be superseded in that case.
- `BenchmarkDeleteSetChurn` is a permanent addition to
  `cache_bench_test.go` — the main module previously had no
  delete-then-reinsert coverage at all, despite `bench/` comparing exactly
  that workload across libraries.
