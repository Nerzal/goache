# 0003: Generics API, phase 1 scope (Set/SetMany/Get, no eviction)

## Status

Accepted

## Context

Phase 1 needs a clean API to add 1..N objects and retrieve one, with type
safety and no `interface{}` boxing/unboxing overhead. Eviction (LRU/LFU/TTL)
is explicitly out of scope for phase 1, but the design must not require a
rewrite to add it in phase 2.

## Decision

`Cache[K comparable, V any]` with:

- `Set(key K, value V)` — single insert/overwrite.
- `SetMany(entries []Entry[K, V])` — bulk insert/overwrite; groups entries
  by destination shard before locking, so each shard's mutex is acquired at
  most once per call regardless of batch size, instead of once per key.
- `Get(key K) (V, bool)` — single lookup.
- `Len() int`.

Values are stored per-shard as `entry[V]{value V}` — a wrapper struct
holding only the value today, but the designated extension point for phase
2. TTL/eviction metadata (expiry timestamp, access time, etc.) goes into
`entry[V]`'s fields, not into a new type — the public `Cache` API doesn't
need to change.

`Entry[K, V]{Key, Value}` is a plain exported struct for bulk operations,
chosen over accepting `map[K]V` so callers with a slice of pairs (e.g. rows
read from a DB) don't have to build an intermediate map just to call
`SetMany`.

Eviction, LRU/LFU, TTL: explicitly not implemented in phase 1.

## Consequences

- No `interface{}` boxing anywhere on the hot path — confirmed by benchmark
  (`Set`/`Get` are 0 allocs/op).
- Phase 2 eviction is additive (new fields on `entry[V]`, new methods on
  `Cache`), not a rewrite.
- `SetMany` takes a slice, not a map — callers building from a map need one
  conversion, which was judged the less common case for the target
  "bulk ingestion" workload (see [0006](0006-with-capacity-pre-sizing.md)).
