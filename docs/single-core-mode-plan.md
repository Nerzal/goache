# Plan: a dedicated single-core cache that beats go-cache at `GOMAXPROCS=1`

Status: **executed.** Implemented as `SingleCoreCache`/`NewSingleCore`; the
outcome and the decisions are recorded in
[ADR 0026](adr/0026-single-core-cache.md). All four pre-registered accept
criteria were met — the bar was parity with go-cache and every one of them
cleared it. This document is kept as-written (pre-implementation) because it
is the record of what was predicted before any code existed — including the
two options that were built or priced and rejected.

**For the numbers, go to [ADR 0027](adr/0027-single-core-field-claim.md), not
to ADR 0026.** ADR 0026 measured at `-count=3` against one competitor on
half the suite; ADR 0027 re-ran the full eight-category matrix against all
six at `-count=10` and corrected the read margins down from a reported
20-25% to a measured 4.9-6.5%.

## Goal

Let a caller state at construction time that the process runs on at most one
core, and be **at least as fast as patrickmn/go-cache** in that mode.

## Why goache loses at one core today

[ADR 0025](adr/0025-cpu-constrained-benchmarks.md), concurrent `Get`,
100,000 entries:

```
              1 core     2       4       8      24
go-cache       16.88   25.50   17.56   27.29   37.41
goache         24.51   14.08   7.399   7.587   4.687
```

Sharding relieves contention between threads running *simultaneously*. At
`GOMAXPROCS=1` exactly one goroutine executes at a time, so there is no such
contention, and every cost the sharded design pays to enable it is pure
overhead: a `maphash.Comparable` call the single map would not need, a shard
slice indirection, the `s.limit > 0` eviction checks, and a per-entry
`{key, referenced, prev, next}` payload that only `WithMaxSize` uses.

Two things goache does *not* lose on, so nobody "fixes" them later:
go-cache also guards its map with a `sync.RWMutex`, and it also reads the
clock only when an item has an expiration. goache additionally has a
structural advantage go-cache cannot match — go-cache stores `interface{}`,
so every value is boxed, while goache's `V` is a concrete generic type.

## Measurements — the feasibility probe

Five variants, all `map[string]…` with a 100,000-entry working set, run
back-to-back at `-cpu=1 -count=5` on an AMD Ryzen AI 9 HX 370. The probe
source is not committed (it would pollute `bench/`, whose results feed
README); each variant is described precisely enough below to recreate.

| Variant | `Get` ns/op | vs go-cache |
|---|---|---|
| **`protoPtr`** — unsharded, one `RWMutex`, `map[K]*{value, expiresAt}` | **16.16-16.68** | **-10%** |
| `protoFat` — `protoPtr` + goache's `{key, referenced, prev, next}` on the entry, unused | 17.00-17.36 | -5% |
| go-cache | 17.52-19.22 | — |
| `protoBranch` — goache's exact structure (shard slice, padding, CLOCK fields, `limit` checks) with **one** shard and a hashless `shardFor` | 18.63-19.04 | +3% |
| goache today, 256 shards | 24.34-24.99 | +36% |

Mixed 90% Get / 10% Set at `-cpu=1`, same working set:

```
protoPtr                  18.97-19.27
protoInline               20.52-21.31
go-cache                  20.73-22.46
protoBranch (1 shard)     25.87-26.49
goache today              26.88
```

### What the probe settles

**1. The branch-based approach reaches parity with go-cache, not victory.**
Skipping the hash via `if len(c.shards) == 1` gets `Get` from 24.7 to 18.8 —
a real 24% win, but still *above* go-cache's 18.2. The residual is the shard
slice indirection, the padded shard struct, and the `s.limit > 0` checks
that a shared implementation cannot remove. A dedicated type reaches 16.4.
**This is the finding that decides the design**: your instinct was right,
and the reason is not the one either of us started with.

**2. The hash was never the whole story — entry size matters too.**
`protoFat` differs from `protoPtr` only by fields it never reads, and costs
~0.8 ns/op (5%) for it. goache's entry is ~56 bytes for a string key
(`key` + `value` + `expiresAt` + `referenced` + `prev` + `next`) against 16
for a value+deadline entry; at 100,000 entries that is 5.6 MB of working set
against 1.6 MB. `key`, `prev`, `next` and `referenced` exist solely for
CLOCK eviction — an unbounded cache carries them for nothing.

**3. The "one lock serializes writers" risk is dissolved.** The previous
draft flagged single-shard mixed read/write as the main danger. Measured, an
unsharded prototype does mixed traffic at 19.1 ns/op against go-cache's 21.4
and current goache's 26.9. One lock is not a liability when only one
goroutine can run.

**4. Inline value storage stays rejected, now on single-core evidence too.**
[ADR 0021](adr/0021-reject-inline-storage-unbounded.md) rejected
`map[K]entry[V]` because of concurrent-write co-location at 24 cores — a
failure mode that needs two cores, so the previous draft proposed reviving
it here. The probe says don't: `protoInline` is *slower than* `protoPtr` at
one core on every axis (`Get` 19.3 vs 16.4, `Set` 27.7 vs 24.3, mixed 20.9
vs 19.1). The 16-byte map value costs more in probe traffic than the saved
pointer chase returns, exactly as ADR 0021's own analysis predicted for the
single-threaded case. **Stage 2 of the previous draft is cancelled** — one
fewer thing to build.

### What the probe says about returning an interface

The literal form of the idea — `New` hands back one of two implementations —
requires `New` to return an interface instead of `*Cache[K, V]`. Priced with
two concrete types stored behind a runtime flag so the compiler cannot
devirtualize:

```
                              direct        via interface
Get (1 core)                  24.42          26.33          +7.8%
ParallelGet (24 cores)        4.520-4.551    4.636-4.774    +2-5%
```

Roughly 2 ns of indirect call per operation. That is tolerable at 24 ns/op
and expensive at 4.5, and it would be paid by **every** goache user
including the 24-core ones who gain nothing from single-core mode. It also
breaks the existing `*Cache[K, V]` return type.

**So: keep the idea, drop the mechanism.** Two implementations, yes — but
selected at *compile* time by which constructor the caller calls, not at run
time behind an interface.

## Design

### Two concrete types, two constructors

```go
// Unchanged. Sharded, the right choice from two cores upward.
func New[K comparable, V any](opts ...Option) *Cache[K, V]

// New. Unsharded: one map, one RWMutex, no shard routing, no hash beyond
// the map's own. For processes that run on a single core — a Kubernetes pod
// at limits.cpu: 500m or below, where Go 1.25+ derives GOMAXPROCS=1.
func NewSingleCore[K comparable, V any](opts ...Option) *SingleCoreCache[K, V]
```

`*Cache[K, V]` is **not touched at all** — not even by a branch in
`shardFor`. That is the whole point of the second implementation: the
multi-core path cannot regress, because no line of it changes. The ≤1%
default-path criterion becomes trivially satisfiable rather than something
to measure and hope for.

`SingleCoreCache` carries the same public method set (`Set`, `SetWithTTL`,
`SetMany`, `Get`, `Delete`, `DeleteMany`, `Clear`, `Len`, `Purge`) and
honours `WithCapacity` and `WithMaxSize`. `WithShardCount` and any
`WithParallelism` are meaningless to it and must be documented as ignored
(not as errors — an option set built once and passed to either constructor
is the normal usage).

### What the single-core type may do that `Cache` may not

- **One `sync.RWMutex` and one map as direct struct fields** — no shard
  slice, no `&c.shards[i]`, no cache-line padding. Worth ~1.6 ns/op against
  the branch approach.
- **No `maphash` at all** — no `seed` field, no `hash/maphash` dependency on
  this path.
- **Slim entries when unbounded.** With no `WithMaxSize`, store
  `map[K]*entry[V]` where `entry` is `{value V; expiresAt int64}` — 16 bytes
  instead of ~56. Pointer storage is kept (probe finding 4). With
  `WithMaxSize`, use the full CLOCK-capable entry; the two are separate
  types inside one file, not a branch inside one type.
- **`SetMany`/`DeleteMany` take the lock once and iterate directly** — no
  bucket grouping, no `atomic.Pointer` scratch swap, no per-entry hash. The
  `setBuckets`/`deleteBuckets` machinery ([ADR 0022](adr/0022-bulk-bucket-scratch-reuse.md))
  does not exist here.
- **`Len` is `len(c.data)`** under an `RLock` — O(1), against `Cache`'s
  O(shard count).

### On `WithParallelism(0)`

**0 must not mean single-core.** Every existing option in this package
treats `n <= 0` as "unspecified, ignore me" — `WithCapacity`, `WithMaxSize`,
and `Entry.TTL` all do. Making 0 mean "one core" would make the zero value
of a config struct silently select the unsharded implementation, which is
the worst possible default for a caller who forgot to set the field.

With two constructors, `WithParallelism` is not needed to select an
implementation at all. Whether to add it as a *shard count* hint for
`New` — `WithParallelism(n)` → `nextPowerOfTwo(n * 16)` capped at 256, so a
4-core box gets 64 shards instead of 256 — is a separate, smaller question.
Recommendation: **defer it.** It is unmeasured, it is not needed for this
goal, and shipping it in the same change would make the ADR argue two things
at once.

### The cost of two types

Stated plainly, because it is the real price:

- **Two implementations to keep correct.** Every future feature lands twice
  or is documented as sharded-only. [ADR 0021](adr/0021-reject-inline-storage-unbounded.md)
  rejected a `WithInlineStorage()` opt-in partly on "would double the test
  matrix for every operation permanently" — that objection applies here and
  must be answered in the ADR, not dodged. The answer this plan proposes:
  the duplication is justified where a *branch cannot capture the win*, and
  the probe shows the branch leaves ~13% on the table (18.8 vs 16.4) plus
  all of the slim-entry and bulk-path wins.
- **Callers who choose at run time pay dispatch themselves.** Ship a
  `goache.Cacher[K, V]` interface both types satisfy (with compile-time
  `var _ Cacher[string, int] = …` assertions) so they can, and document the
  ~2 ns cost measured above. Nobody pays it by default.
- **A caller who declares single-core and then runs on 24 cores gets one
  global lock.** That is go-cache's profile — bad, but not worse than the
  library we are beating. The doc comment must say it in one blunt sentence.

## Pre-registered accept/reject criteria

**Accept if all of:**

- At `-cpu=1`, 100,000 entries, `SingleCoreCache` is **≤ go-cache** on `Get`,
  `ParallelGet`, `Set`, and mixed `ParallelGetSet`. The probe predicts
  wins of 5-10%; the bar is parity.
- `*Cache[K, V]`'s benchmarks at 24 cores are unchanged — enforced by the
  diff containing no edits to the sharded path, verified by re-running
  `make bench` anyway.
- Full test suite and `-race` pass for both types.

**Reject / rethink if:**

- `SingleCoreCache` fails to beat go-cache on any of the four. In that case
  the fallback is the branch-in-`shardFor` approach from the previous draft
  (parity, no second type) — measurably worse, but a tenth of the code.
- The bounded (`WithMaxSize`) single-core path regresses against today's
  `Cache` with `WithMaxSize` at `-cpu=1` by more than 3%.

## Risks still open

1. **`WithMaxSize` + one CLOCK ring.** [ADR 0023](adr/0023-reject-clock-bitmap.md)
   measured 0.00-2.54 hand steps per eviction — at 256 shards. One ring of
   1,000,000 entries is a different structure and that measurement does not
   carry over. Re-run ADR 0023's sweep-length instrumentation against
   `SingleCoreCache` before claiming the bounded path is fine. The probe
   covered unbounded only.
2. **`Purge`/`Clear` under one lock** become a single long critical section
   instead of 256 short ones. At one core probably better; it is a latency
   change for anyone calling `Purge` from a ticker, and must be measured,
   not assumed.
3. **Probe noise.** An earlier run of the same binaries put `protoPtr_Get`
   at 17.5 and a later one at 16.4, and one intermediate run drifted ~15%
   across the board. All comparisons above come from single back-to-back
   `-count=5` runs. Per [ADR 0021](adr/0021-reject-inline-storage-unbounded.md)'s
   methodology note, the final numbers must come from a controlled
   before/after run, never from comparing against a figure recorded in an
   earlier session.

## Spin-off finding — worth its own task, not this one

`protoFat` vs `protoPtr` (17.2 vs 16.4) prices goache's per-entry CLOCK
metadata on a cache that never evicts. **Unbounded sharded shards could
store `map[K]*slimEntry[V]` — pointer storage kept, just `{value, expiresAt}`
in the object — and reclaim that ~5% on the default path.** This is *not*
what [ADR 0021](adr/0021-reject-inline-storage-unbounded.md) tried and
rejected: ADR 0021 changed pointer storage to inline storage, which is what
caused the concurrent-write co-location regression. Shrinking the pointed-to
struct keeps writes off the bucket array entirely, so ADR 0021's failure
mode does not apply.

It needs its own design work (two entry types inside the sharded cache, or
making the CLOCK fields conditional) and its own measurement. Filed here so
it is not lost; **do not fold it into this change.**

## Work breakdown

1. `singlecore.go`: `SingleCoreCache[K, V]`, `NewSingleCore`, the two entry
   types (slim / CLOCK-capable), full method set. Doc comment covers the
   "if you declare one core and run on 24, you get one global lock" hazard
   and points at ADR 0025 for the crossover.
2. `Cacher[K, V]` interface + compile-time assertions for both types,
   documented with the measured ~2 ns dispatch cost so the trade-off is the
   caller's, made knowingly.
3. `WithShardCount` doc comment: note it is ignored by `NewSingleCore`.
4. `singlecore_test.go`: the full correctness suite re-verified against the
   new type — set/get, `SetMany`, delete, `DeleteMany`, `Clear`, `Len`,
   `Purge`, TTL, `WithCapacity`, `WithMaxSize` eviction, hash-collision
   behaviour — plus the three concurrency tests under `-race`.
5. `cache_bench_test.go`: `BenchmarkSingleCore_{Get,Set,SetMany,Delete,
   Purge,ParallelGet,ParallelGetSet}`, `b.Loop()` with a fixed-size key pool
   per repo convention.
6. `bench/compare_test.go`: `BenchmarkGoacheSingleCore_{Get,ParallelGet,
   ParallelGetSet}` **and `BenchmarkGoCache_ParallelGetSet`, which does not
   exist yet** — without it there is nothing to compare mixed read/write
   against, and mixed traffic is where the single-lock design is most
   questioned.
7. Makefile: `bench-singlecore`, `bench-compare-singlecore`; both added to
   CLAUDE.md's command list.
8. ADR 0026 — decision, the probe table above, and explicitly: why an
   interface-returning `New` was rejected (measured), why inline storage
   stays rejected even at one core (measured, updating ADR 0021's
   Consequences), and why two types are worth their duplication cost.
9. README: `NewSingleCore` in the API section, a `runtime.GOMAXPROCS(0)`
   worked example, new numbers in "Performance under a CPU limit", and a
   correction to that section's current "at one core, reach for something
   else" advice — which is exactly what this work exists to invalidate.
10. `docs/benchcharts/main.go` entries for every new table, then
    `make charts` ([ADR 0024](adr/0024-chart-per-benchmark-table.md)).
11. `make ci`, push, then **verify the GitHub Actions run is green** — a
    local green `make ci` is not proof, per CLAUDE.md.

## What this plan deliberately does not do

- No interface return from `New` (measured, rejected above).
- No branch in `shardFor` — the sharded path is not edited at all.
- No inline value storage (measured, rejected above and in ADR 0021).
- No `WithParallelism` in this change (deferred, see design).
- No lock removal in single-core mode. `GOMAXPROCS=1` does not make an
  `RWMutex` unnecessary: goroutines still interleave via preemption, and a
  caller who runs this type on many cores must still get correct results.
- No background goroutine, timer, or ticker
  ([ADR 0011](adr/0011-lazy-ttl-no-background-janitor.md)).
- No CFS-throttling modelling — that is the separate, still-unexecuted
  [sub-core benchmark plan](sub-core-benchmark-plan.md).
