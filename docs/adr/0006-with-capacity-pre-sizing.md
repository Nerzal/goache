# 0006: `WithCapacity` pre-sizing for bulk ingestion

## Status

Accepted

## Context

After [0004](0004-shard-storage-pointer-slice.md) and
[0005](0005-reject-custom-hash-table.md) both failed to beat the existing
design, looked for a lower-risk, genuinely additive optimization. The
original goal names "maximal ingestion performance" explicitly. A cache
built fresh and bulk-loaded via `SetMany` pays for Go's incremental map
growth/rehashing as each shard's map grows from empty — entirely avoidable
if the caller knows the approximate final size upfront (e.g. loading a
fixed data set at startup).

## Decision

Added `WithCapacity(n int) Option`. Pre-sizes every shard's map for roughly
`n` total items split evenly across shards
(`make(map[K]entry[V], ceil(n/shardCount))`), so a bulk load close to `n`
items skips almost all incremental growth. Ignored if `n <= 0` — purely an
optional hint, no behavior change if unused.

## Measurement

`BenchmarkFreshLoad_NoHint` vs `BenchmarkFreshLoad_WithCapacityHint`
(bulk-load 10,000 entries into a brand-new cache via `SetMany`), 5 runs:

| | No hint | `WithCapacity(10000)` |
|---|---|---|
| ns/op | ~793,654 | ~475,442 (**~38% faster**) |
| allocs/op | 4232 | 2973 (**~30% fewer**) |
| B/op | 1,579,119 | 1,192,161 (**~25% less**) |

Existing `BenchmarkSet`/`BenchmarkGet`/`BenchmarkParallelGet` (which don't
use the hint) confirmed unchanged — this is purely additive.

## Consequences

- A real, stable, measured win for the ingestion-heavy workload the
  original goal called out, with zero regression when unused.
- Callers who don't know the final size upfront get identical behavior to
  before (capacity 0 → no pre-sizing, same as prior default).
