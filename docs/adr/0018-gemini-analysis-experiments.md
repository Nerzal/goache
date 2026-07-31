# 0018: gemini-analysis.md experiments — one accepted, one accepted in
refined form, three rejected

## Status

Accepted (partially — see per-item verdicts below)

## Context

An untracked file `gemini-analysis.md` appeared in the working tree,
containing a modified `cache.go` with five numbered optimizations attributed
to a prior Gemini-based analysis of an older snapshot of this codebase. The
user asked that each proposal actually be tried and measured against the
current `cache.go` (which has since gained `Delete`/`Clear`/`WithMaxSize`
CLOCK eviction — a materially different starting point than whatever
snapshot the analysis was run against), with results documented in full.
`gemini-analysis.md` itself is left in place, untouched, as the source
record; see [gemini-analysis-result.md](../../gemini-analysis-result.md) at
the repo root for the narrative write-up aimed at a human reader. This ADR
is the terse decision record for the same five experiments.

Each of the five was implemented in isolation (build, `go vet`, full test
suite, `-race` where the change touched concurrency-sensitive code),
benchmarked against the specific `cache_bench_test.go` benchmarks it could
plausibly affect, and kept or reverted based on measurement — not on the
analysis document's own claims, several of which did not hold up.

All numbers below: AMD Ryzen AI 9 HX 370 (24 threads), Go, `windows/amd64`,
single clean runs (repeated 3x for anything close enough to matter — see
each item).

## 1. Value slice + cache-line padding for shard storage — **accepted**

**Claim**: switch `Cache.shards` from `[]*shard[K, V]` back to a contiguous
`[]shard[K, V]`, adding a 64-byte pad field to each `shard` to prevent false
sharing between adjacent shards' `sync.RWMutex`.

This directly revisits [ADR 0004](0004-shard-storage-pointer-slice.md),
which measured a *bare* (unpadded) value slice at ~9% slower than the
pointer slice and kept the pointer-slice form. The padding is the genuinely
new angle CLAUDE.md's own guidance asks for before retrying a
previously-rejected optimization.

**Measured** (3 runs each, stable within ~0.1 ns/op):

```
                             pointer-slice (before)   padded value-slice (after)
BenchmarkSet-24              32.75 ns/op              ~31.9-32.5 ns/op   (flat)
BenchmarkGet-24               24.78 ns/op              ~23.2-24.2 ns/op   (flat/slightly better)
BenchmarkParallelGetSet-24     7.561 ns/op              ~5.94-6.00 ns/op  (~21% faster)
BenchmarkParallelGet-24        5.415 ns/op              ~4.49-4.59 ns/op  (~17% faster)
```

**Verdict: accepted.** Reproduced across 3 independent runs with no overlap
between before/after ranges. The padding removes exactly the false-sharing
cost ADR 0004 originally worked around by paying a pointer indirection —
once that cost is removed directly, the contiguous layout's own advantage
(one fewer heap dereference, better locality for `Cache.shards` itself)
shows through, especially under concurrency where multiple cores are
touching adjacent shards' mutexes simultaneously. **This supersedes ADR
0004's conclusion**; ADR 0004 has been updated to point here.

Single-threaded `Set`/`Get` are flat, as expected — false sharing is a
multi-core effect and single-threaded benchmarks never exercise it.

## 2. Read-before-write on the CLOCK `referenced` bit — **rejected**

**Claim**: change `e.referenced.Store(true)` (unconditional) to
`if !e.referenced.Load() { e.referenced.Store(true) }` in `Get` and in
`set`'s overwrite path, to avoid invalidating the entry's cache line on
every hit when the bit is already `true` (cache-line bouncing under
concurrent readers hammering the same key).

No existing benchmark exercised concurrent `Get` against a `WithMaxSize`
cache, so `BenchmarkParallelGetWithMaxSize` was added (kept regardless of
this item's outcome — it's a real gap in eviction-path coverage).

**Measured** (3 runs each):

```
                                    unconditional Store   read-before-write
BenchmarkParallelGetWithMaxSize-24   4.65-5.02 ns/op        4.68-4.75 ns/op
```

**Verdict: rejected.** No measurable difference — both ranges overlap
completely. Reverted to the unconditional store to avoid the extra branch
and `Load` for zero benefit. Plausible explanation: x86's atomic store
already costs a full cache-line-ownership request regardless of the prior
value, so skipping it only when the load happens to already be `true` saves
nothing in practice on this hardware — the branch's own cost roughly
cancels out whatever it occasionally avoids.

## 3. Move `time.Now()` outside `Get`'s `RLock` — **rejected**

**Claim**: copy `expiresAt` and the value out under the read lock, release
the lock, then call `time.Now()` afterward — reducing how long `Get` holds
`RLock` on a TTL entry, on the theory that a writer waiting on `Lock()`
would wait less if readers didn't do a (comparatively expensive) clock read
while holding the lock.

Single-threaded `BenchmarkGetWithTTL` can't show a lock-contention effect
(the calling goroutine pays the same total time regardless of hold-order),
so `BenchmarkParallelGetSetWithTTL` was added (kept regardless of outcome —
same rationale as item 2's new benchmark).

**Measured** (3 runs each):

```
                                    time.Now() under RLock   time.Now() after RUnlock
BenchmarkParallelGetSetWithTTL-24    6.33-6.48 ns/op           6.51-6.91 ns/op
```

**Verdict: rejected** — reproducibly *slower* (~5%, no overlap between the
two 3-run ranges), the opposite of the claimed effect. Reverted. Most
likely explanation: Go's `sync.RWMutex` doesn't behave like a naive
FIFO/ticket lock where reader hold-time directly gates writer wait-time in
the way the theory assumes, and moving the expiry check after unlock adds
an extra local variable and a data dependency across the unlock boundary
that the original, more direct code didn't have. This is a case where the
intuitive optimization actively lost to leaving the lock/logic ordering as
Go's compiler already had it.

## 4. `sync.Pool` entry recycling — **accepted, in a narrower form than proposed**

**Claim**: pool every `entry[K, V]` allocation cache-wide, having
`Delete`/`DeleteMany`/`Clear`/`Purge`/eviction return entries to a
`sync.Pool` and `set` draw from it, for "zero allocations in steady state."

**First attempt (blanket pooling, every `Cache` regardless of
`WithMaxSize`)** measured a serious mixed result:

```
                                        before        blanket pool (after)
BenchmarkSet-24                        32.75 ns/op    33.32 ns/op   (flat)
BenchmarkSetMany-24                    104.6 ns/op    186.3 ns/op   (+78%, WORSE)
BenchmarkFreshLoad_NoHint-24           1038249 ns/op  1847838 ns/op (+78%, WORSE)
BenchmarkFreshLoad_WithCapacityHint-24  888895 ns/op  1585137 ns/op (+78%, WORSE)
BenchmarkDelete-24                      38.15 ns/op     61.56 ns/op (+61%, WORSE)
BenchmarkDeleteMany-24                  69.70 ns/op     94.41 ns/op (+35%, WORSE)
BenchmarkClear-24                     10543726 ns/op  17997319 ns/op (+71%, WORSE)
BenchmarkSetWithMaxSize-24             127.1 ns/op     77.76 ns/op  (-39%, better; 1->0 allocs)
BenchmarkEvictionChurn-24              167.1 ns/op    113.0 ns/op   (-32%, better; 2->1 allocs)
```

Root cause of the regressions: every one of `BenchmarkSetMany`,
`BenchmarkFreshLoad_*`, and `BenchmarkClear` constructs a brand-new `Cache`
(and therefore a cold, empty pool) every iteration, so `pool.Get()` never
actually returns a recycled entry — it falls through to `pool.New()`, which
just allocates a fresh `&entry{}` anyway, but pays `sync.Pool`'s own
per-P/victim-cache bookkeeping overhead on top for no benefit.
`BenchmarkDelete`/`BenchmarkDeleteMany`/`BenchmarkClear` on an *unbounded*
cache (`s.limit == 0`, no `WithMaxSize`) got strictly worse because
`deleteEntry`/`release` now unconditionally zero the key/value, clear the
CLOCK fields, and call `pool.Put` — real work that used to be nothing
(unbounded-cache deletes just dropped the entry for the GC), spent for a
pool that nothing will ever draw back down, since eviction (the only thing
that calls `pool.Get()` inside `set`) never runs without `WithMaxSize`.

**Refined version**: `set` only touches the pool when `s.limit > 0`
(falls back to a plain `&entry[K, V]{}` allocation otherwise, same as
before pooling existed); `deleteEntry`/`Clear`'s per-entry release loop are
likewise gated on `s.limit > 0` so unbounded-cache deletes go back to doing
nothing beyond the map delete itself.

```
                                        before        refined pool (after)
BenchmarkSet-24                        32.75 ns/op    33.18 ns/op   (flat)
BenchmarkSetMany-24                    104.6 ns/op    103.2 ns/op   (flat)
BenchmarkFreshLoad_NoHint-24           1038249 ns/op   979083 ns/op  (flat)
BenchmarkFreshLoad_WithCapacityHint-24  888895 ns/op   926558 ns/op  (flat)
BenchmarkDelete-24                      38.15 ns/op     38.34 ns/op  (flat)
BenchmarkDeleteMany-24                  69.70 ns/op     67.48 ns/op  (flat)
BenchmarkClear-24                     10543726 ns/op  10520000-11510000 ns/op (flat, confirmed over 3 runs)
BenchmarkSetWithMaxSize-24             127.1 ns/op      74.94 ns/op  (-41%, 1->0 allocs)
BenchmarkEvictionChurn-24              167.1 ns/op     112.3 ns/op   (-33%, 2->1 allocs)
```

**Verdict: accepted, gated on `s.limit > 0`.** The pooling win is real and
substantial specifically for the sustained-churn workload `WithMaxSize`
exists for (repeatedly evicting and reinserting on the *same* long-lived
`Cache`, so the pool is warm) — but only there. Applied cache-wide it's a
straightforward regression on every other path, because a `sync.Pool` only
pays for itself when something both fills it and later drains it on the
same instance; goache's non-eviction paths do neither. Unbounded (no
`WithMaxSize`) `Cache` instances — goache's default and most common
configuration — are completely unaffected, preserving the "every `Set`
always lands, zero eviction-policy overhead" property `docs/adr/0016`
established.

## 5. Flat `shardIndices` slice instead of `[][]Entry`/`[][]K` buckets in
`SetMany`/`DeleteMany` — **rejected**

**Claim**: replace the per-call `make([][]Entry[K, V], len(c.shards))` (plus
per-key `append` growth) with one flat `make([]int, len(entries))` recording
each entry's destination shard index, then a `for shard { for entry {...} }`
double loop that skips entries not destined for the current shard — fewer
allocations, "drastically more GC-friendly" per the analysis document.

**Measured**:

```
                                        bucket-of-slices (before)   flat index + nested loop (after)
BenchmarkSetMany-24 (batch=100)         103.2 ns/op, 2 allocs        441.4 ns/op, 1 alloc   (4.3x SLOWER)
BenchmarkFreshLoad_NoHint-24 (n=10000)   979083 ns/op               4777538 ns/op            (4.9x SLOWER)
BenchmarkFreshLoad_WithCapacityHint-24    926558 ns/op               4533330 ns/op            (4.9x SLOWER)
BenchmarkDeleteMany-24 (batch=100)         67.48 ns/op                164.9 ns/op             (2.4x SLOWER)
```

**Verdict: rejected, decisively.** The nested loop is `O(shardCount ×
len(entries))` — with the default 256 shards, a 100-entry `SetMany` does
25,600 inner-loop iterations instead of 100; a 10,000-entry fresh load does
2.56 million instead of 10,000. The single flat slice genuinely does
allocate less (1 alloc instead of 2, and a smaller one), but that saving is
worth nanoseconds while the algorithmic complexity change costs
milliseconds at real batch sizes. This is a textbook case of "fewer
allocations" and "faster" being different, sometimes opposed, goals — the
bucket approach's per-key `append` (amortized O(1), a handful of
reallocations total) is far cheaper in aggregate than iterating the entire
shard set once per entry. Reverted to the original bucket grouping
unchanged.

## Consequences

Net effect on `cache.go`: items 1 and 4 are permanent, documented changes
(see the package doc comment's shard-storage and "Automatic eviction"
sections); items 2, 3, and 5 left no trace in `cache.go` beyond this record
and `gemini-analysis-result.md`. `BenchmarkParallelGetWithMaxSize` and
`BenchmarkParallelGetSetWithTTL` are new, permanent additions to
`cache_bench_test.go` — they were needed to evaluate items 2 and 3 and
remain useful eviction-path/TTL-path concurrent coverage regardless of
those items' outcomes.

README.md's benchmark tables and `docs/benchcharts/main.go`'s `bar{}`
entries were updated to the final combined-state numbers (items 1 and 4
together); see README.md's "Benchmarks" section for the current numbers.
