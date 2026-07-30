# 0001: Sharded lock-striping over global RWMutex / sync.Map

## Status

Accepted

## Context

Phase 1 requires the cache to be multi-goroutine safe with maximal Set/Get
throughput under concurrent load. Three designs from the `sync` package were
evaluated:

1. A single global `sync.RWMutex` guarding one map.
2. `sync.Map`.
3. Sharding (lock-striping): partition keys across N independently-locked
   segments.

## Decision

Use sharding. `Cache[K comparable, V any]` hashes each key once with
`hash/maphash.Comparable` (stdlib, Go 1.24+— hashes any comparable type via
the runtime's built-in hash, no reflection, no per-key allocation) and routes
it to one of N shards, each an independent `map[K]entry[V]` guarded by its
own `sync.RWMutex`. Shard count defaults to 256, always a power of two, so
shard selection is a bitmask (`hash & mask`) instead of a modulo.

## Why not the alternatives

- **Global `sync.RWMutex`**: serializes every writer against every other
  writer regardless of key. Under concurrent Set/Get from many goroutines
  this becomes the bottleneck long before the map itself does.
- **`sync.Map`**: optimized for two specific patterns — keys written once
  and read many times ("append-only" growth), or many goroutines operating
  on disjoint key sets. A general-purpose cache with mixed read/write/
  overwrite traffic on shared keys doesn't fit that profile; `sync.Map`
  falls back to its slower, mutex-guarded "dirty" path. It also boxes
  keys/values as `any`, adding allocation and losing type safety — exactly
  what generics are meant to avoid here.

## Consequences

- Lock contention scales ~1/shardCount: goroutines touching different keys
  typically land on different shards and never block each other.
- Measured ~8x faster than a naive single-mutex cache (patrickmn/go-cache)
  on parallel Get (see [0008](0008-separate-comparison-bench-module.md) and
  README.md's comparison section).
- Adds one hash computation per operation, which single-threaded/uncontended
  workloads pay for without benefiting from (go-cache's uncontended Get is
  marginally faster: ~19.85ns vs goache's ~22.17ns). Accepted trade-off since
  concurrent access is the target workload.
