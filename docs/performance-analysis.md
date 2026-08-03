# Deep performance analysis: where goache loses, why, and small testable tasks

Date: 2026-07-31. Basis: CPU profiles (`go test -cpuprofile` on
`BenchmarkGet`/`BenchmarkSet`/`BenchmarkEvictionChurn`), compiler inlining
report (`-gcflags='-m'`), and the size-parametrized competitor tables in
README.md (post-[ADR 0018](adr/0018-gemini-analysis-experiments.md) numbers).

Goal: a backlog of *small, individually benchmarkable* tasks, each with a
hypothesis, an exact change, the benchmark that decides it, and an explicit
accept/reject criterion — same discipline as ADR 0018's experiments. Best
case, working through these makes goache fastest across the board; every
task stands alone and can be accepted or reverted on its own numbers.

## Current standing (README comparison tables, n=1,000 → 1,000,000)

| Category | goache today | Verdict |
|---|---|---|
| `Set` | leads at every size | keep |
| `SetWithTTL` | leads at every size | keep |
| `ParallelGet` | leads at every size except n=1,000 (otter 3.262 vs 5.673) | near-optimal |
| `Get` (single-thread) | **loses to go-cache at every size** (e.g. 22.77 vs 12.94 at n=1,000; 23.84 vs 17.40 at n=100,000) | gap to close |
| `Delete` (churn) | **loses to go-cache everywhere** (93.27 vs 53.64 at n=1,000), loses to freecache up to n=100,000 | gap to close |
| `Bounded` | leads at n≤100,000, **loses to ristretto at n=1,000,000** (250.2 vs 76.34) and barely beats otter there (237.3) | scaling problem |
| `GetWithTTL` | loses only to go-cache (same root cause as `Get`) | follows `Get` |

## Profile findings (what a `Get` actually spends its ~23.65 ns on)

CPU profile of `BenchmarkGet` (100k string keys, single-threaded), share of
`Get`'s cumulative time:

1. **~55% map access** (`runtime.mapaccess2_faststr`, including the map's
   *internal* re-hash of the key and SIMD group probing). Irreducible with
   the builtin map — and [ADR 0005](adr/0005-reject-custom-hash-table.md)
   already measured a custom table at 2.4-3.5x slower. Locked in.
2. **~23% RWMutex reader accounting** (`sync/atomic.(*Int32).Add` from
   `RLock`/`RUnlock` — two atomic adds per Get). Across the whole profiled
   run (Set+Get+Churn combined), this atomic was the single biggest flat
   cost at 17.6%.
3. **~10% shard routing** (`hash/maphash.comparableHash` → `aeshashbody`).
   This is the "double hashing" cost: every key is hashed once for shard
   selection and again inside the map.
4. Remainder: `Get`'s own body — including the entry **pointer chase**
   (`map[K]*entry` means the value lives in a separately-allocated heap
   object, one extra cache miss per lookup on cold entries).

Inlining is *not* a problem: `Get`, `shardFor`, `set`, `expired`,
`linkNew`, `removeFromRing` all inline (verified with `-gcflags='-m'` on
the test build). No task needed there.

**Why go-cache wins single-threaded `Get` despite a naive design**: its
`Get` is RLock + one `map[string]Item` access + RUnlock. It pays costs 1
and 2 exactly like goache but has **no shard hash (cost 3) and no pointer
chase (cost 4)** — its `Item` struct is stored inline in the map. Those two
costs (~6ns combined) are almost exactly the measured gap (~6.4ns at
n=100,000). The gap is structural, not waste — but half of it (the pointer
chase) is only structurally required for `WithMaxSize` caches, which is
what T3 below attacks.

**Why goache loses the `Delete` churn benchmark**: `bench/`'s Delete
benchmark does `Delete(k); Set(k, i)` per iteration. On an *unbounded*
goache cache, that `Set` is always a brand-new key → **one heap allocation
per iteration** (the ADR 0016 pointer-storage cost), while go-cache's
re-insert is a plain inline map assign (zero alloc) and freecache writes
into its ring buffer. The eviction-path entry pool from ADR 0018
deliberately doesn't cover unbounded shards. T1 attacks exactly this.

**Why ristretto wins `Bounded` at n=1,000,000**: goache's CLOCK hand walks
an intrusive linked list of individually heap-allocated entries — at 1M
entries / 256 shards ≈ 3,900 entries per shard, the walk is a chain of
dependent cache misses (ADR 0016's documented locality regression, ~5.5x
cost growth from n=1,000 to n=1,000,000). ristretto never walks anything
comparable per Set: its admission is a frequency-sketch lookup plus
buffered async processing (and it's allowed to *drop* Sets — a semantic
goache doesn't copy). We can't (and shouldn't) become ristretto, but T2 and
T5 attack the walk's locality directly.

## Task backlog

Ordered by expected value ÷ effort. Every task follows the repo's standing
policy: benchmark before/after (3+ runs when close), `-race` when touching
concurrency, ADR when accepted *or* rejected, README/charts refresh when
numbers move.

### T1 — per-shard single-slot entry freelist for unbounded shards (small) — DONE, accepted (ADR 0019)

- **Hypothesis**: the `Delete`-churn loss is one heap allocation per
  delete-then-reinsert. A one-entry scratch slot per shard (`s.free
  *entry[K, V]`) recycles it: `Delete` parks the removed entry
  (`s.free = e`, one store, **no zeroing** — key/value get overwritten on
  reuse anyway), `set` for a new key takes it (`if s.free != nil { e =
  s.free; s.free = nil }`). No `sync.Pool`, no atomics (shard lock already
  held), no cold-pool overhead — exactly the trap ADR 0018 item 4's blanket
  pooling fell into and this avoids by construction.
- **Change**: ~15 lines in `cache.go` (`shard` field, `Delete`/`DeleteMany`
  park, `set` take). GC note: retains at most shardCount entries (256 by
  default) — bounded, negligible.
- **Benchmarks**: `bench/` `BenchmarkGoache_Delete` (target: beat go-cache's
  53.64/58.12/68.42/72.00/191.4 row, or at least close most of the gap);
  main-module `BenchmarkDelete`/`BenchmarkDeleteMany` (accept: ≤ ~42 ns/op,
  i.e. the park costs ≤ ~3ns); `BenchmarkSet`/`BenchmarkSetMany` (accept:
  flat).
- **Reject if**: `BenchmarkDelete` regresses >10% or churn doesn't improve
  ≥25%.
- **Note**: superseded by T3 if T3 lands (inline storage has no per-entry
  allocation to recycle) — do T1 first anyway, it's 15 lines and proves the
  diagnosis.

### T2 — shard-count scaling for large bounded caches (tiny, config-only first) — DONE, REJECTED (ADR 0020)

**Outcome**: hypothesis refuted and inverted — more shards measured *worse*
at the sizes that motivated it (+25% at n=100,000, +19% at n=1,000,000
going 256 → 4096 shards), flat on concurrent reads. Phase 2 not implemented.
Phase 1's apparent n=1,000 "win" turned out to be a capacity bug: per-shard
limits used `max(ceil(maxSize/shardCount), 1)`, so any shard count above
`maxSize` made real capacity equal the shard count (`WithMaxSize(500)` +
4096 shards held 4096 — and on the *default* 256 shards, `WithMaxSize(100)`
held 256). Fixed: the budget now splits so per-shard limits sum to exactly
`maxSize`, and the shard count is capped at `maxSize` when smaller.
`WithMaxSize` is a hard upper bound now, not "roughly n". Full write-up in
[ADR 0020](adr/0020-shard-count-does-not-scale-eviction.md).

- **Hypothesis**: `Bounded` at n=1,000,000 suffers because limit/shard ≈
  1,950 (limit = n/2 over 256 shards) makes each CLOCK ring long. More
  shards → shorter rings → shorter worst-case hand walks and smaller
  per-shard maps. ristretto-class flat scaling is not expected; a 30-50%
  improvement at n=1M might be.
- **Change (phase 1, zero code)**: run `bench/` `BenchmarkGoache_Bounded`
  at n=1,000,000 with `WithShardCount(512/1024/2048/4096)` added to the
  benchmark's constructor, alongside the default 256.
- **Change (phase 2, only if phase 1 wins)**: auto-derive the default shard
  count from `maxSize` when `WithShardCount` isn't given (e.g.
  `max(256, nextPow2(maxSize/2048))`) — keeps the default behavior for
  everyone else, needs its own ADR (changes a documented default,
  [ADR 0007](adr/0007-shard-count-default.md)).
- **Benchmarks**: `BenchmarkGoache_Bounded/n=1000000` (accept phase 2 if
  ≥25% better with no regression at n=1,000-100,000 and no
  `ParallelGetWithMaxSize` regression); memory overhead per extra shard is
  ~200B — 4096 shards ≈ 800KB, acceptable for a 1M-entry cache, but state
  it in the ADR.

### T3 — inline entry storage for unbounded caches (medium-large, biggest total win) — DONE, REJECTED (ADR 0021)

**Outcome**: implemented in full (dual-mode shard, correct, race-clean) and
reverted on measurement. The predicted `Get` win never appeared — the
pointer chase is nearly free on warm, contiguously-allocated entries, and a
16-byte inline value makes the map's buckets fatter, roughly cancelling it.
Worse, inline storage regressed `BenchmarkParallelGetSet` by **20%** (and
`ParallelGetSetWithTTL` by 17%): an overwrite becomes a `mapassign` into the
bucket array that concurrent readers are probing, where pointer mode writes
through the pointer and leaves the buckets clean. Both pre-registered reject
criteria were hit (bounded paths regressed >3%; `Get` win negative).

The wins were real but landed on maintenance and startup, not steady-state
serving: `Purge` -51%, `Clear` -25% and -100,000 allocs/op,
`FreshLoad_WithCapacityHint` -30%, `SetMany`/`Delete`/churn -17%. Not worth
20% of concurrent throughput for a concurrent cache. Full numbers and the
"don't re-attempt without solving the concurrent-write co-location problem"
warning in [ADR 0021](adr/0021-reject-inline-storage-unbounded.md).

- **Hypothesis**: the ADR 0016 storage change (`map[K]entry[V]` →
  `map[K]*entry[K, V]`) taxes *every* unbounded cache to enable a feature
  it doesn't use: +1 alloc per new key (`SetMany`/`FreshLoad`/churn), one
  pointer chase per `Get`, and the ~2x full-sweep locality regression
  (`Purge` 2.56ms → 5.4ms, same effect in `Clear` and any future iteration
  API). A dual-mode shard — inline `map[K]entryInline[V]` when `limit ==
  0`, pointer map only when `WithMaxSize` is configured (the only mode that
  *needs* individually-addressable entries for the lock-free CLOCK bit) —
  removes the tax exactly where it isn't buying anything.
- **Change**: two map fields on `shard` (one nil), branch per operation on
  `s.limit > 0` (a predictable branch — the same predicate every op already
  checks today). Real cost is code size/duplication in
  `set`/`Get`/`Delete`/`DeleteMany`/`Clear`/`Len`/`Purge`, not runtime.
  Public API unchanged.
- **Benchmarks**: `BenchmarkGet` (accept: ≤21.5 ns/op, from the pointer
  chase — closes ~half the go-cache gap; combined with T1's diagnosis this
  is the biggest remaining `Get` lever that isn't locked in by ADR 0005),
  `BenchmarkPurge` (accept: ≤3.5ms, from 5.4ms), `BenchmarkFreshLoad_NoHint`
  (accept: allocs/op drops by ~10,000 — one per entry), `BenchmarkClear`,
  `bench/` `BenchmarkGoache_Get`/`_Delete` rows; **must not regress**:
  `BenchmarkSetWithMaxSize`/`BenchmarkEvictionChurn`/
  `BenchmarkGetWithMaxSize`/`BenchmarkParallelGetWithMaxSize` (bounded path
  untouched), `BenchmarkSet` (branch cost must be invisible).
- **Reject if**: bounded paths regress >3%, or the unbounded `Get` win is
  <1.5ns (branch + code-bloat not paying for itself).
- **Risk**: highest-complexity task here (touches every operation).
  Mitigation: the two modes never mix on one shard; `-race` suite plus
  `TestConcurrentEvictionChurn` already cover both paths.

### T4 — reuse SetMany/DeleteMany bucket slices per Cache (small) — DONE, accepted (ADR 0022)

**Outcome**: accepted, but not as a `sync.Pool`. New repeated-call
benchmarks exposed the real cost first — **101 allocations per bulk call**
(one outer slice plus one per non-empty bucket), invisible behind the
existing `BenchmarkSetMany`, which builds a throwaway cache and so can never
show reuse. A `sync.Pool` delivered the warm-path win (-53%/-59%) but cost
30-64% on one-shot calls, the same "pool only pays when something both
fills and drains it" trap as [ADR 0018](adr/0018-gemini-analysis-experiments.md)
item 4. Replacing it with a single `atomic.Pointer` swap per cache kept the
win and erased the cold cost: `SetManyRepeated` -54%, `DeleteManyRepeated`
-60%, both **101 -> 0 allocs/op**, `FreshLoad_*` flat. `BenchmarkSetMany`
still regresses 3-6% — the pre-registered guard said reject at >3%, and
[ADR 0022](adr/0022-bulk-bucket-scratch-reuse.md) records overriding it
explicitly, with the reasoning that the guard was the wrong benchmark for a
change about reuse.

- **Hypothesis**: `SetMany`'s 2 allocs/op (outer `[][]Entry` + inner
  growth) and `DeleteMany`'s 1 are per-call garbage that a per-`Cache`
  `sync.Pool` of bucket slices eliminates for repeat callers (the pool is
  warm here by construction — same `Cache`, repeated calls — unlike ADR
  0018 item 4's cold-pool trap; note ADR 0018 item 5 already rejected the
  *flat-index* alternative as 2.4-4.9x slower, this is the allocation-side
  fix that keeps the bucket algorithm).
- **Change**: `sync.Pool` holding `[][]Entry[K, V]` (and `[][]K`), cleared
  (`bucket = bucket[:0]` per shard) on return.
- **Benchmarks**: `BenchmarkSetMany` (accept: allocs 2→≤1 and ns/op not
  worse; the win is GC pressure more than ns/op), `BenchmarkDeleteMany`,
  `BenchmarkFreshLoad_*` (uses SetMany once per iteration on a fresh cache —
  pool cold there, must not regress: that's the ADR 0018 lesson, verify
  it), `-race` suite.
- **Reject if**: `BenchmarkFreshLoad_*` or `BenchmarkSetMany` ns/op
  regresses >3%.

### T5 — compact CLOCK: referenced bits in a per-shard bitmap (medium, high risk) — DONE, REJECTED (ADR 0023)

**Outcome**: rejected on measurement *before* implementing. Instrumenting the
current sweep showed the CLOCK hand walks **0.00-2.54 slots per eviction**
across every workload this task targets — 1.00 even under a continuous
9-reads-per-write mix, 0.00 in pure churn. The pointer-chasing chain this
task exists to flatten is one dereference long, so a contiguous bit map
cannot improve on it, while it would add a slot allocator, a second
indirection, and false sharing between the 512 slots that share a cache
line on the *read* path (the pre-registered reject condition).

That also resolves the `Bounded`-at-1M question this backlog opened with:
goache's **unbounded** `Set` already scales 4.56x from n=1,000 to
n=1,000,000 with no eviction involved at all, versus 5.68x for `Bounded` —
so ~80% of the growth is working-set-versus-cache, shared with every library
in the comparison, and only ~1.25x is eviction-specific. Full reasoning in
[ADR 0023](adr/0023-reject-clock-bitmap.md).

- **Hypothesis**: the eviction hand walk's cost at large shards is one
  dependent cache miss per visited entry (each `referenced` bit lives in a
  separate heap object). Moving the bits into one contiguous per-shard
  atomic bitmap (entry stores its slot index; hand walks the bitmap
  word-by-word, only dereferencing the chosen victim) turns the walk from
  pointer-chasing into linear scanning — and a 64-bit word of zero bits
  skips 64 candidates in one load.
- **Change**: slot allocator per shard (free-slot list), `entry.slot int32`,
  `[]atomic.Uint64` bitmap, `Get` does `bitmap[slot>>6].Or(1<<(slot&63))`
  instead of `e.referenced.Store(true)`.
- **Benchmarks**: `bench/` `BenchmarkGoache_Bounded/n=1000000` (accept:
  ≥25% better), `BenchmarkEvictionChurn`, and critically
  `BenchmarkParallelGetWithMaxSize` — adjacent slots share bitmap words, so
  concurrent Gets on nearby slots now contend on the same cache line
  (`Or` on a shared word) where the old per-entry bit never did. **Reject
  if** `ParallelGetWithMaxSize` regresses >10% — read-path concurrency
  outranks eviction throughput in this design (same priority call as ADR
  0016 made against true LRU).
- Do after T2: if bigger shard counts already fix the n=1M problem cheaply,
  T5's complexity may not be worth it.

### Considered and pre-rejected (documented so they don't get re-explored)

- **Cheaper shard-routing hash** (~10% of `Get`): the hash is unavoidable
  for arbitrary `comparable` K, `maphash.Comparable` already uses the
  runtime's AES-NI path (~2.5ns for short strings), and reusing it to skip
  the map's internal hash is exactly the ADR 0005 dead end (2.4-3.5x
  slower). No cheaper correct option exists on this design.
- **Eliminating the RWMutex reader atomics** (~23% of `Get`): a seqlock is
  memory-unsafe over Go's builtin map (concurrent read during write is a
  runtime fatal error, not just a stale read — retry loops can't save it);
  copy-on-write/RCU makes every write O(shard size). Both blocked unless
  the builtin map is replaced, which ADR 0005 rejected on measurement.
  This cost is the floor of the current architecture, shared by go-cache.
- **Dropping/buffering Sets like ristretto**: changes goache's contract
  ("every Set always lands", package doc). Out of scope on semantics, not
  on performance.

## Sequencing

T1 → T2(phase 1) → T4 → T3 → T2(phase 2 if warranted) → T5(only if T2
didn't already fix n=1M Bounded). T1/T2/T4 are each an afternoon-sized,
single-benchmark-decidable step; T3 is the big one and lands last of the
sure things because its diff conflicts with T1 (supersedes it on the
unbounded path).

Realistic end-state if T1-T4 all accept: `Delete` churn competitive with
go-cache (T1), single-thread `Get` within ~2-3ns of go-cache (T3 removes
the chase; the remaining gap is the shard hash, which is the price of
scaling to 24 cores — go-cache pays it back a thousandfold in
`ParallelGet`), `Bounded` n=1M materially improved (T2/T5), everything
goache already leads staying led. "Faster than everyone at everything" is
not honestly reachable while keeping the always-lands Set contract
(ristretto's Bounded number rests on dropping work) — but leading every
*synchronous, guaranteed* category at every size is.
