# 0007: Default shard count: 256

## Status

Accepted

## Context

Shard count trades off lock contention (more shards = less contention)
against per-shard memory overhead and array-indirection cost (more shards =
more indirection, worse locality for the shards array itself). Needed a
sensible default for `New` when `WithShardCount` isn't specified.

## What was tried

Swept shard counts 64/128/256/512/1024/2048/4096 under
`BenchmarkParallelGet` (24-thread machine, 100k-key working set), 3 runs
each.

## Measurement

| Shards | ns/op |
|---|---|
| 64 | ~6.4-6.6 |
| 128 | ~4.7-4.9 |
| **256** | **~4.36-4.46 (best)** |
| 512 | ~4.4-4.47 (ties 256) |
| 1024 | ~4.6-4.7 |
| 2048 | ~4.9-5.1 |
| 4096 | ~5.1-5.4 |

## Decision

Keep 256 as `defaultShardCount`. It's already at (or tied for) the measured
optimum on a 24-thread machine; no change made.

## Consequences

- Confirms the existing default rather than changing it — recorded so this
  sweep isn't re-run from scratch next time someone asks "should we tune
  shard count."
- This was measured on one 24-thread machine. If the target deployment
  environment's core count differs drastically, re-run the sweep (the
  `BenchmarkShardCountTuning` pattern used for this — parameterized
  `b.Run` over `WithShardCount` values — can be recreated from this record
  if needed; it was a throwaway benchmark file, not kept in the repo).
