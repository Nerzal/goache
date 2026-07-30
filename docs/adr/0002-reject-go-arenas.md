# 0002: Reject Go arenas for memory management

## Status

Accepted

## Context

Phase 1 requires low RAM usage and minimal GC pressure. Go's experimental
`arena` package (`GOEXPERIMENT=arenas`) was evaluated as a way to offload
cache-item memory from the garbage collector.

## Decision

Do not use arenas. Rely instead on: storing values inline in the map (no
`interface{}` boxing, since `V` is a concrete generic type parameter),
pre-sizing shard maps via `WithCapacity` when the final size is known
upfront (see [0006](0006-with-capacity-pre-sizing.md)), and batching
multi-key writes (`SetMany`) so the map only rehashes/grows as needed
rather than once per key.

## Why

- The `arena` package was never stabilized and is not available as a
  supported package in the Go toolchain used here (Go 1.26). Shipping a
  library depending on it would force every consumer to opt into an
  experimental, unstable-API build flag.
- Arenas free memory in bulk when the *whole* arena is released, not per
  object. A cache has an open-ended set of long-lived, individually
  replaced/removed entries — there's no point at which "free everything in
  this arena" is valid without also destroying live entries.
- Per-item arenas would add far more overhead (one arena allocation per
  `Set`) than they'd save.
- Without eviction (out of scope for phase 1 — see
  [0003](0003-generics-api-phase1-scope.md)), items live indefinitely
  anyway, so there's nothing for an arena to reclaim early that the normal
  GC doesn't already handle in steady state.

## Consequences

- No dependency on experimental Go features; the library works on any
  standard Go 1.26 toolchain.
- Revisit if/when Go ships a stabilized region-based memory API *and*
  phase 2 introduces TTL/eviction — bounded-lifetime entries would change
  this trade-off.
