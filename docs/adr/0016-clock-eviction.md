# 0016: CLOCK (second-chance) eviction via WithMaxSize

## Status

Accepted

## Context

[docs/roadmap.md](../roadmap.md) flagged automatic/bounded eviction as the
single biggest strategic gap against every competitor surveyed in
[docs/competitor-analysis.md](../competitor-analysis.md) (ristretto, theine,
otter v1/v2 all implement one) — and explicitly called it out as the one
roadmap item needing its own ADR-first discussion before any code was
written, since it directly trades away goache's current "every `Set`
always lands, zero eviction-policy overhead" identity.

Two shapes were on the table:

1. **A simple per-shard LRU/CLOCK policy** — cheap, easy to reason about,
   fits goache's existing "no fancy bookkeeping" philosophy.
2. **W-TinyLFU-style frequency-sketch admission** — what theine-go and
   otter v2 use; better hit ratio under skewed/Zipf workloads, but costs
   real CPU on every `Set` (frequency estimate update, admission
   comparison) — exactly the bookkeeping cost goache's benchmarks currently
   beat theine (239.6 ns/op Set) and otter (146.5 ns/op Set) on.

The user chose option 1, with an explicit follow-up design question:
whether `Get` should update recency by taking a write lock (true LRU,
move-to-front) or by flipping a single atomic bit checked only at eviction
time (CLOCK / second-chance), to preserve `Get`'s existing RLock-only
invariant. CLOCK was chosen specifically to preserve that invariant — the
entire sharded-lock architecture (see the package doc comment's
"Architecture" section) exists so concurrent readers never block each
other, and a per-Get write lock would undo that for every cache user, not
just ones who opt into `WithMaxSize`.

## Decision

Added `WithMaxSize(n int)` (ignored if `n <= 0`, same zero-value convention
as `WithCapacity`/TTL). When set, it bounds the cache to roughly `n` total
entries split evenly across shards, and each shard runs an independent
CLOCK eviction:

- Every entry carries a `referenced atomic.Bool`. `Get`, holding only
  `RLock`, sets it with a plain atomic store on a hit — no write lock, no
  ring mutation, just one bit flip that's safe under arbitrarily many
  concurrent readers.
- Every entry also carries `prev`/`next` pointers threading it into its
  shard's circular ring, mutated only under the shard's write lock (`Set`,
  `Delete`, eviction, `Purge`).
- A shard's "hand" is the next eviction candidate. Eviction walks forward
  from the hand, clearing `referenced` on anything it finds set (giving it
  a second chance) and evicting the first entry it finds already clear.

**This required changing shard storage from `map[K]entry[V]` to
`map[K]*entry[K, V]`** (entries are now individually heap-allocated,
addressable objects, not values inlined in the map) — this is the only way
`Get` can mutate a per-entry bit without taking the map's write lock. This
applies to *every* `Cache`, whether or not `WithMaxSize` is ever called;
ring-pointer maintenance and the eviction sweep itself are skipped when a
shard has no configured limit (`if s.limit > 0` guards around all of it),
but the pointer-per-entry storage change itself is unconditional.

`entry` also gained a `key K` field (needed so eviction, which only has a
pointer to the victim entry, knows which map key to `delete`).

## Consequences

Measured on AMD Ryzen AI 9 HX 370 (24 threads), Go 1.26.2, before/after this
change:

```
                                        before          after
BenchmarkSet-24                        32.42 ns/op     32.28 ns/op    (0 allocs both)
BenchmarkGet-24                        24.07 ns/op     25.42 ns/op    (0 allocs both)
BenchmarkSetMany-24                    90.96 ns/op     97.40 ns/op    (1 -> 2 allocs/op)
BenchmarkDelete-24                     33.99 ns/op     40.66 ns/op    (0 allocs both)
BenchmarkFreshLoad_NoHint-24            931764 ns/op   1031954 ns/op  (4263 -> 14263 allocs/op)
BenchmarkFreshLoad_WithCapacityHint-24  703699 ns/op    973060 ns/op  (3003 -> 13004 allocs/op)
BenchmarkPurge-24                      2563396 ns/op   7013482 ns/op  (0 allocs both)
BenchmarkClear-24                      7898728 ns/op  11488978 ns/op  (6661 -> 106661 allocs/op)
```

**Steady-state single-key operations (`Set`, `Get`, `Delete`) are
essentially unregressed.** All the core benchmarks cycle repeatedly over
the same fixed 100k-key pool, so after the first pass every subsequent call
is an overwrite of an already-allocated entry (the `set` helper's
overwrite-in-place path never allocates), not a cold insert. The one
heap allocation this change adds only happens on a *genuinely new* key.

**Cold-insert-heavy benchmarks show the real cost, in two distinct ways**:

1. **Direct allocation cost.** `FreshLoad_NoHint`/`FreshLoad_WithCapacityHint`
   and `Clear` (which measures populate-then-Clear, see its own doc
   comment) all insert 100k *unique* keys per iteration — every one is a
   cold insert, so every one now pays a pointer allocation. This is exactly
   the cost documented in the package doc comment's "Automatic eviction"
   section and is not a surprise.
2. **A locality regression on full-map sweeps, independent of allocation
   count.** `Purge`'s own timed portion (`c.Purge()`) reports **0
   allocs/op both before and after** — its `s.limit == 0` in this benchmark
   (no `WithMaxSize` configured), so the `if s.limit > 0` guards mean it
   runs the exact same code as before this change. Yet its measured cost
   nearly tripled (2.56ms -> 7.0ms, confirmed stable across three repeated
   runs). The likely cause: `map[K]*entry[K, V]` scatters entries across
   the heap as individually-allocated objects instead of packing them
   inline in the map's own bucket arrays, so a full sweep now chases one
   extra pointer per entry with correspondingly worse cache locality —
   100k extra cache misses adds up. This is a real, distinct cost from the
   allocation-count regression above, and applies to **any** future
   full-cache-iteration operation (not just `Purge`), not only to
   `WithMaxSize` users, since it's a consequence of the storage layout
   change itself.

**`WithCapacity`'s benefit shrank substantially**: before this change it
was ~24% faster (2227967 -> 1708948 ns/op in the pre-Delete/Clear
measurement, later 931764 -> 703699 ns/op); after, it's only ~6% faster
(1031954 -> 973060 ns/op). Pre-sizing the map's bucket array no longer
addresses the now-dominant cost (one heap allocation per entry), so its
relative payoff is smaller than before, though still a net win.

**This was accepted as a deliberate trade-off, not a regression to fix**:
the roadmap explicitly named this cost before code was written, the user
chose the CLOCK approach knowing the alternative (write-lock-per-Get true
LRU) was worse for concurrent read throughput, and steady-state
single-key operations — the workload goache's benchmarks are built around
— are unaffected. Cold-insert-heavy and full-sweep operations pay a real,
now-documented cost.

## Alternatives considered and rejected

- **True LRU (write lock on every `Get`)**: rejected outright — would
  regress `ParallelGet` for every cache user, not just ones with a
  configured limit, undoing the sharded-lock design's central benefit.
- **W-TinyLFU admission**: rejected for this iteration — better hit ratio
  under skew, but the frequency-sketch bookkeeping cost lands on every
  `Set`, unconditionally, which is a larger and less avoidable cost than
  CLOCK's per-new-key allocation. May be revisited if hit-ratio-under-skew
  becomes a measured problem for real workloads; nothing here forecloses
  it — see [docs/competitor-analysis.md](../competitor-analysis.md) for the
  comparison this decision was based on.
- **A parallel `map[K]*atomic.Bool` alongside `map[K]entry[V]`** (keeping
  entries inline, avoiding the pointer-per-entry storage change entirely):
  considered and rejected — doubles the number of map operations per
  Get/Set (two map lookups instead of one), and the second map still needs
  the same per-new-key allocation this ADR already accepts, so it trades
  one cost for a different, likely worse one without avoiding the core
  trade-off.
- **A custom open-addressed table giving entries a stable slot index**
  (avoiding pointer indirection by using an index into a flat array instead
  of a heap pointer): this is exactly the approach [ADR 0005](0005-reject-custom-hash-table.md)
  already measured at 2.4-3.5x *slower* than Go's built-in map for
  Set/Get/ParallelGet, for unrelated reasons (losing Go's SIMD-friendly
  bucket layout). Not revisited here.
