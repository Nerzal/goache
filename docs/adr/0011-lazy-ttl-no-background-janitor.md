# 0011: Lazy TTL enforcement, no background janitor goroutine

## Status

Accepted

## Context

Entries can now optionally expire (`SetWithTTL`, `Entry.TTL` for `SetMany`).
Two things are needed: (1) `Get` must never return an expired value, and
(2) memory for entries that expire and are never looked up again should be
reclaimable, or the cache grows unbounded for that access pattern.

Considered two designs for (2):

1. An internal background goroutine per `Cache`, started in `New` (or
   lazily on first TTL use), running on a ticker to periodically sweep all
   shards and delete expired entries — the common "active expiration"
   design (e.g. how many TTL caches, including Redis, behave).
2. A caller-driven `Purge` method that does the same sweep, but only runs
   when the caller calls it.

## Decision

Implemented both correctness and reclamation, but split them:

- **Lazy correctness (mandatory, always on)**: `Get` checks the found
  entry's `expiresAt` and treats an expired entry as a miss. The clock
  (`time.Now()`) is only read when `expiresAt != 0` — a non-TTL entry's
  `Get` never calls it. This is not optional; without it, `Get` would
  return stale values past their deadline.
- **Active reclamation (opt-in, caller-driven)**: `Purge()` sweeps every
  shard under its lock and deletes expired entries, returning the count
  removed. Nothing calls it automatically. No goroutine, ticker, or timer
  is started by any `Cache` method or by `New`.

## Why not an internal janitor

- **Every `Cache` would carry a background goroutine merely for existing**,
  even for the (likely common, given phase 1's core use case) caller that
  never uses TTL at all. That contradicts goache's existing minimalism:
  no dependencies, no implicit background work (see
  [0002](0002-reject-go-arenas.md), [0008](0008-separate-comparison-bench-module.md)
  for the same instinct applied elsewhere).
- **Lifecycle management this would force on every caller**: a `Cache`
  with a live goroutine needs a `Close()` (or similar) to stop it cleanly,
  or it leaks — especially visible in tests that create many short-lived
  `Cache` instances. That's a new API surface and a new failure mode
  (forget to `Close()`, leak a goroutine per test) that wasn't asked for.
- **The caller already knows their own operational cadence** better than
  goache could guess (how often keys actually expire, how tight memory
  budgets are, whether they already have a ticker/cron loop to hook into).
  A fixed default interval picked by goache would be wrong for some
  callers regardless of what it is.
- **Nothing forces immediate incorrectness without it** — `Get` already
  guarantees expired entries are never returned, with or without `Purge`
  ever being called. The janitor is purely about memory reclamation
  timing, which is exactly the kind of policy decision better left to the
  caller.

## Consequences

- Callers using TTLs and caring about bounded memory for "set-and-forget"
  expiring keys must call `Purge()` themselves periodically (their own
  ticker, cron, or request-count-based trigger). This is documented in the
  package doc comment, README.md, and `Purge`'s doc comment.
- `Len()` intentionally still reports the raw stored count (may include
  expired-but-not-yet-purged entries) — see the `Len` doc comment. This
  was an existing phase-1 design choice ([0003](0003-generics-api-phase1-scope.md))
  and TTL doesn't change it: `Get` is the source of truth for whether an
  individual key is alive, not `Len`.
- If a future phase decides most callers *do* want automatic reclamation,
  this can be layered on top as an *additional* opt-in constructor option
  (e.g. `WithJanitorInterval(d)`) without breaking anything — `Purge` being
  a plain, idempotent, externally-callable method makes that an additive
  change, not a rewrite.
