# 0014: Add theine-go and otter/v2 to the competitor comparison

## Status

Accepted

## Context

[0008](0008-separate-comparison-bench-module.md) set up `bench/` comparing
goache against patrickmn/go-cache, coocood/freecache, and
dgraph-io/ristretto/v2. Research into `fooo.md`'s two links —
[Yiling-J/theine-go](https://github.com/Yiling-J/theine-go)'s README and
otter's own [cache-evolution blog
post](https://maypok86.github.io/otter/blog/cache-evolution/) — surfaced
further Go cache libraries with published performance claims: theine-go
itself, maypok86/otter (v1 and v2), and sturdyc. The blog post's own author
describes otter as having had "long-standing unbeatable throughput" among Go
caches, and theine-go's README claims 2-4x the parallel-read throughput of
ristretto — both worth measuring directly against goache rather than taking
on faith. Full writeup of what each library in that survey does better/worse:
[docs/competitor-analysis.md](../competitor-analysis.md).

sturdyc was deliberately **not** added to `bench/`: per the otter blog, it
uses a custom O(n) eviction policy documented as slower with a worse hit
ratio than LRU, and is oriented around request-coalescing/refresh features
that don't correspond to a comparable Set/Get benchmark shape. It's covered
in the analysis doc for completeness but isn't a throughput peer.

## Decision

Added `github.com/Yiling-J/theine-go` and `github.com/maypok86/otter/v2` as
dependencies of `bench/go.mod` only (never the root module). Extended
`bench/compare_test.go` with `BenchmarkTheine_*` and `BenchmarkOtter_*`
covering the same Set/Get/ParallelGet shape as the existing competitors:

- theine-go requires a bounded `MaximumSize` at construction (no unbounded
  constructor) and an explicit per-entry `cost` on `Set` — sized to the
  benchmark's fixed 100k-key pool, cost `1` per entry, matching how
  ristretto's cost parameter is already used in this file.
- otter/v2 uses an `Options` struct with `MaximumSize`; reads go through
  `GetIfPresent` (no loader) to match the no-loader shape of every other
  competitor's Get benchmark.

## Consequences

- Headline results (24-thread machine, same run as the other four):
  parallel Get — otter (3.99 ns/op) edges out goache (4.59 ns/op), both far
  ahead of theine (6.42), ristretto (8.17), freecache (16.4), go-cache
  (37.1). Single-threaded Set — goache (34.95 ns/op) is fastest and the only
  zero-allocation Set; theine (239.6 ns/op) and otter (146.5 ns/op) both pay
  real admission-policy bookkeeping cost on every Set, same category of cost
  as ristretto already documented in 0008.
- Neither theine-go nor otter/v2 is a drop-in semantic replacement for
  goache: both implement admission/eviction policies (W-TinyLFU) where a
  `Set` can be dropped if the entry loses the admission race. goache has no
  eviction policy — every `Set` always lands. This is the same category of
  caveat 0008 already recorded for ristretto's async best-effort `Set`.
- README.md's "Comparison with other Go cache libraries" section, and
  `docs/benchcharts/main.go`'s `compare-set`/`compare-parallel-get` bar data,
  were updated together with this change (per the repo's standing
  performance policy) — regenerate `docs/img/*.svg` via `make charts`
  whenever those numbers move again.
