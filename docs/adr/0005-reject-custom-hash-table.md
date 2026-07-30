# 0005: Reject custom open-addressed hash table, keep Go's map

## Status

Rejected / reverted

## Context

Each shard hashes its key once (`hash/maphash.Comparable`) to pick the
shard, then Go's built-in `map[K]entry[V]` hashes the *same* key again
internally to place/find it in a bucket — a redundant hash computation per
operation. Explored replacing the per-shard `map[K]entry[V]` with a
hand-rolled table that reuses the single precomputed hash for both shard
routing and in-shard probing, eliminating the second hash entirely.

## What was tried

Implemented a per-shard open-addressed table using linear probing:

- `slot[K, V]{hash uint64, key K, value V, occupied bool}`, stored in a flat
  `[]slot[K, V]`.
- No delete in phase 1 ([0003](0003-generics-api-phase1-scope.md)) means no
  tombstones are needed — a probe can stop at the first empty slot, which
  materially simplifies correctness.
- Grow (double capacity, powers of two) at load factor 0.75, reinserting
  live entries using their already-known hash (no rehashing of keys).
- Lazy allocation: a shard's table starts `nil` and only allocates on first
  write, so untouched shards cost nothing beyond the shard struct.

Added correctness tests for growth across many resizes and for many keys
colliding into one shard (`TestGrowth`, `TestHashCollisions` — kept in
`cache_test.go`, since they're valid regardless of backing store). Build,
`vet`, and `-race` all passed cleanly.

## Measurement

Benchmarked against the `map[K]entry[V]` baseline, 3 runs:

| Benchmark | Go map (baseline) | Custom table (tried) | Delta |
|---|---|---|---|
| `BenchmarkSet` | ~29.7 ns/op | ~75.9 ns/op | **~2.6x slower** |
| `BenchmarkGet` | ~22.2 ns/op | ~76.7 ns/op | **~3.5x slower** |
| `BenchmarkParallelGet` | ~4.5 ns/op | ~10.5 ns/op | **~2.4x slower** |

## Decision

Reverted entirely. Kept the plain Go `map[K]entry[V]` per shard.

## Why

Go's map runtime is far more engineered than a straightforward linear-probe
table written in plain Go: SIMD-friendly grouped buckets (8 key/value pairs
per bucket), a "tophash" byte-level pre-filter that lets a probe skip most
non-matching slots with a single word compare, and incremental (not
stop-the-world) resizing. Reproducing that level of engineering to save one
redundant hash call isn't worth it — the redundant hash was cheap; Go's map
internals are not easily beaten by hand-rolled code without matching their
sophistication.

## Consequences

- No code change from this experiment. Documented in the `cache.go` package
  doc comment and here specifically so it is **not re-attempted** without
  new evidence (e.g. a bucketed/grouped design with a tophash-style
  pre-filter, not plain linear probing) that it could actually win.
- If eviction/TTL (phase 2) ever needs tombstone support anyway, this
  decision should be revisited only with a design that matches Go's map
  sophistication, not a simpler one.
