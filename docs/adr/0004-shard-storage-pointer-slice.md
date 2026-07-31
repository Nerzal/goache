# 0004: Shard storage: pointer slice, not contiguous value slice

## Status

**Superseded by [ADR 0018](0018-gemini-analysis-experiments.md).** A later
experiment revisited the "cache-line padding" fix this ADR's Consequences
section considered and dismissed as "not worth the fragility for a benefit
this small" — a plain fixed-size byte-array pad field turned out not to
need `unsafe` or any assumption about `sync.RWMutex`'s layout, and the
benefit was ~17-21% faster on `BenchmarkParallelGet`/`BenchmarkParallelGetSet`,
not small. `Cache.shards` is now `[]shard[K, V]` with each `shard` padded to
a cache line; this ADR's measurement and reasoning are kept below as the
historical record of why the *unpadded* value slice was rejected — that
part of the analysis still holds, it's the padding option that changed the
conclusion.

Original status: Accepted (after reverting a tried alternative)

## Context

While looking for further speedups, tried changing `Cache.shards` from
`[]*shard[K, V]` (each shard its own heap allocation) to `[]shard[K, V]`
(one contiguous array, shard accessed via `&c.shards[i]`). Motivation: one
allocation instead of N, and one fewer pointer dereference per access.

## What was tried

Rewrote `Cache` to hold `shards []shard[K, V]`, updated `New`, `shardFor`,
`SetMany`, `Len` to index-and-address instead of dereferencing a pointer
element. Build and `go vet` clean (after fixing the expected `copylocks`
issues from accidentally range-copying a struct containing a mutex).

## Measurement

Benchmarked against the existing pointer-slice baseline, 5 runs each,
`GOMAXPROCS` default (24):

| Benchmark | Pointer slice (baseline) | Value slice (tried) |
|---|---|---|
| `BenchmarkSet` | ~29.5 ns/op | ~29.5-30.9 ns/op (no meaningful change) |
| `BenchmarkGet` | ~22.1 ns/op | ~22.0 ns/op (no meaningful change, an early 44ns reading was system noise — reproduced at ~22-24ns on repeat) |
| `BenchmarkParallelGet` | ~4.6-4.8 ns/op | ~5.1-5.2 ns/op — **consistently ~9% slower across 5 runs** |

## Decision

Reverted to `[]*shard[K, V]`. Keep each shard as its own heap allocation.

## Why

Packing every shard's `sync.RWMutex` back-to-back in one contiguous array
puts adjacent shards on shared CPU cache lines. Even though two adjacent
shards never contend over the *same* lock, cores touching nearby-but-
different shards under concurrent access still invalidate each other's
cache lines (false sharing) — the exact overhead sharding is meant to
avoid, just moved to the hardware level. Separate heap allocations from
`new`/composite-literal calls naturally end up less tightly packed, avoiding
this at the cost of one extra pointer dereference per access — a cheaper
trade-off, confirmed by measurement.

## Consequences

- No code change from this experiment; documented so it isn't re-attempted
  without new evidence (e.g. an explicit cache-line-padded slot design)
  that it could actually win.
- Cache-line padding (rounding each shard struct up to 64 bytes) was
  considered as a fix for the false sharing, but requires either `unsafe`
  arithmetic on struct sizes or hardcoded assumptions about `sync.RWMutex`'s
  internal layout, *and* doesn't even guarantee alignment unless the
  backing array's base address happens to be cache-line aligned (which Go's
  allocator doesn't guarantee for arbitrary sizes). Judged not worth the
  fragility for a benefit this small.
