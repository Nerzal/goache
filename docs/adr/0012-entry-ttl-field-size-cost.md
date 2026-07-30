# 0012: entry struct grows 8 bytes for TTL — accepted, isolated to creation-time cost

## Status

Accepted

## Context

Adding optional TTL required storing an expiry deadline somewhere per
entry. `entry[V]` gained an `expiresAt int64` field (0 = never expires).
After implementing this, `BenchmarkSetMany` and `BenchmarkFreshLoad_*`
showed real, repeatable slowdowns (SetMany ~118-148ns baseline vs
~160-162ns after; FreshLoad_NoHint ~1.3-1.5ms baseline vs ~2.0-2.3ms
after — roughly 40-70% slower), which needed investigating before being
accepted as a reasonable trade-off rather than dismissed as noise or
shipped as a hidden regression.

## Investigation

The concern: does TTL support add a runtime cost to the *existing*
non-TTL hot path (`Set`, `Get`), or is the cost confined to something else?

1. Isolated `BenchmarkSet`/`BenchmarkGet` (no `New()` inside the timed
   region for either) with `-cpu=1` to remove scheduler-contention noise
   (the whole machine was measurably noisier this session than in prior
   sessions — a `git stash` of the change showed the *unchanged* baseline
   also swinging 33-56ns/op run to run under default `GOMAXPROCS`).
   Result at `-cpu=1`: Set ~31-38ns, Get ~25.6-27.9ns — matching this
   project's established baseline range from earlier sessions. **No
   regression on the non-TTL Set/Get path.**
2. Checked `BenchmarkSetMany`'s harness: it excludes `New()` from the timed
   region (`c := New(...)` happens before `b.StartTimer()`). Its ~13%
   increase reflects only `SetMany`'s own per-entry cost — consistent with
   `Entry[K,V]` gaining an 8-byte `TTL time.Duration` field (a real,
   proportional size increase to what's copied per entry during the
   shard-bucket grouping).
3. `BenchmarkFreshLoad_*`, unlike `BenchmarkSetMany`, times `New()` *and*
   `SetMany` together — that's the point of the benchmark (cold-start
   ingestion). Its larger increase is explained by `New()`'s per-shard
   `make(map[K]entry[V], perShard)`: a wider value type (`entry[V]` grew
   8 bytes) means Go's map buckets are bigger, so allocating/growing 256
   shard maps moves more bytes than before. This is a **map/shard creation
   cost**, not a per-`Get`/`Set` steady-state cost.

## Decision

Accept the entry-size growth and its creation-time cost. Do not attempt to
shrink `expiresAt` (e.g. packing into a smaller int, seconds-since-epoch,
or a side-table keyed separately from the value map) — an `int64` absolute
UnixNano deadline is the simplest correct representation, and the cost it
adds is confined to cache/shard creation (`New`, and whenever a shard's map
grows), not to the steady-state `Get`/`Set` calls that dominate a
long-lived cache's total operation count.

## Consequences

- Documented honestly in README.md's "Optional TTL" section and the
  `cache.go` package doc comment: TTL support moved the `BenchmarkSet`/
  `BenchmarkSetMany`/`BenchmarkFreshLoad_*`/comparison-module numbers
  measurably, and this ADR explains why moving those specific numbers is
  expected and acceptable, while `Get`/`Set` against non-TTL entries is not
  regressed (verified at `-cpu=1`).
- `WithCapacity`'s benefit ([0006](0006-with-capacity-pre-sizing.md)) is
  still real and still worth using — pre-sizing still avoids repeated
  growth of the now-larger buckets, just from a higher absolute baseline.
- If a future phase needs to claw back the creation-time cost (e.g. a
  library user creating thousands of short-lived `Cache` instances), the
  first thing to try is a smaller `expiresAt` representation — but only if
  a real benchmark shows creation-time cost actually matters for that use
  case; don't preemptively shrink it without that evidence.
