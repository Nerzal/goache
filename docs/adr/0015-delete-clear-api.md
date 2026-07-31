# 0015: Add Delete / DeleteMany / Clear

## Status

Accepted

## Context

[0003](0003-generics-api-phase1-scope.md) scoped phase 1 to
`Set`/`SetMany`/`Get`/`Len`, deliberately leaving eviction/removal out. That
left a real correctness gap, surfaced while writing
[docs/roadmap.md](../roadmap.md) against the competitor analysis: **goache
had no way to remove a single key.** `Purge()` only reclaims entries whose
TTL has already passed — a plain `Set` entry (no TTL) could never leave the
cache short of process restart. Every competitor surveyed (go-cache,
freecache, ristretto, theine-go, otter, sturdyc) has some form of `Delete`.
This was flagged as the roadmap's P0 item, ahead of eviction-policy work,
specifically because it's a basic-usability gap rather than a competitive
one.

## Decision

Added three methods, following the same shape as the existing `Set`/
`SetMany` pair:

- `Delete(key K)` — single-key removal, shard-locked, no-op if the key
  isn't present. Mirrors `Set`'s signature and cost shape exactly.
- `DeleteMany(keys []K)` — bulk removal. Groups keys by destination shard
  before locking, identical pattern to `SetMany`, so each shard's mutex is
  acquired at most once per call regardless of how many keys are being
  removed.
- `Clear()` — removes every entry across all shards, using Go's builtin
  `clear(map)` per shard rather than a manual per-key delete loop (which is
  what `Purge` does, since `Purge` also needs to check each entry's
  expiry). `clear()` is markedly faster than a manual loop for the "drop
  everything unconditionally" case, since it doesn't need to inspect each
  entry.

## Consequences

- No change to `entry[V]` or shard layout — this is pure removal, no new
  per-entry metadata, so it doesn't interact with the eviction-policy
  extension point `entry[V]` is reserved for (see [0003](0003-generics-api-phase1-scope.md),
  [ADR 0014](0014-add-theine-otter-to-comparison.md)'s note on the
  strategic eviction gap being separate and much larger in scope).
- `Delete`/`DeleteMany` are zero/one-allocation respectively, same cost
  class as `Set`/`SetMany` (measured: `BenchmarkDelete` 33.99 ns/op 0
  allocs, `BenchmarkDeleteMany` 63.48 ns/op 1 alloc/op — see README.md's
  "Deletion" subsection).
- **Benchmarking `Clear` in isolation doesn't work with this repo's usual
  `b.StopTimer()`/`b.StartTimer()`-around-a-per-iteration-rebuild pattern**
  (the one `BenchmarkPurge` and originally-attempted `BenchmarkDelete` and
  `BenchmarkClear` all tried first). `clear(map)` turned out to be fast
  enough (unlike `Purge`'s per-key loop) that Go's benchmark framework
  wants an iteration count in the tens of millions to accumulate enough
  *measured* time, and each of those iterations was paying an *unmeasured*
  100k-entry cache rebuild during the paused timer — this hung both
  `go test -bench=BenchmarkDelete` and `go test -bench=BenchmarkClear` for
  several minutes before being caught. Fixed by:
  - `BenchmarkDelete`: rebuild only once per `batch` (100) iterations
    instead of once per single `Delete` call — the same pattern
    `BenchmarkSetMany` already uses for exactly this reason.
  - `BenchmarkClear`: fold population into the *measured* portion (same
    shape as `BenchmarkFreshLoad_NoHint`) rather than trying to isolate
    `Clear`'s cost — its own marginal cost is the difference between
    `BenchmarkClear` and `BenchmarkFreshLoad_NoHint`'s numbers. If a future
    benchmark needs to isolate a *fast* operation that requires expensive
    per-iteration setup, don't reach for the Purge-style
    stop/rebuild/start pattern by default — check first whether the timed
    operation is fast enough for this failure mode to recur.
