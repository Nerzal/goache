# 0023: Reject the contiguous CLOCK bit map (T5) — the sweep it optimizes costs ~1 step

## Status

Rejected — on measurement, before implementation

## Context

Task T5 from [docs/performance-analysis.md](../performance-analysis.md), and
the last standing candidate for goache's one remaining competitive gap:
`Bounded` (Set against a cache evicting under real pressure) growing ~5.7x
from 1,000 to 1,000,000 entries in the cross-library comparison, where
ristretto's stays flat or falls.

[ADR 0016](0016-clock-eviction.md) attributed that growth to the CLOCK
hand's walk: the ring is an intrusive linked list of individually
heap-allocated entries, so a longer ring means a longer chain of *dependent*
cache misses per eviction. [ADR 0020](0020-shard-count-does-not-scale-eviction.md)
tested the cheap fix (more shards → shorter rings) and found it made things
worse, explicitly leaving T5 as "the only standing candidate" and noting
that ruling out the cheap explanation made T5 *more* interesting.

T5's plan: drop the linked list, give each entry a slot index, keep the
reference bits in one contiguous `[]atomic.Uint64` per shard, and sweep in
slot order — turning pointer-chasing into a linear scan where an all-ones
word clears 64 candidates with a single `And` and a zero word finds a victim
immediately.

The whole design rests on one unmeasured assumption: **that the hand
actually walks.**

## Measurement

Before building any of it, the current implementation was instrumented with
two counters (hand steps taken, evictions performed) and run against the
workloads whose cost T5 targets — 200,000 evictions each, after filling to
the limit:

```
workload                                    hand steps per eviction
1 shard  / 100,000 limit                              0.00
1 shard  / 100,000 limit, every key read first        0.50
256 shards / 500,000 limit                            0.00
256 shards / 500,000 limit, every key read first      2.54
1 shard  / 100,000 limit, 1 read per write            1.00
1 shard  / 100,000 limit, 9 reads per write           1.00
256 shards / 500,000 limit, 9 reads per write         0.00
```

A "hand step" is one iteration of the skip-referenced loop — the
pointer-chasing T5 exists to eliminate. **It is essentially zero.** Even
under a continuous 9-reads-per-write mix, the sweep clears at most one
entry before finding its victim; in pure churn it evicts the very first
entry it looks at, every time.

That is not an accident of the benchmark, it is CLOCK working as designed.
The entries near the hand are the *oldest* ones, whose reference bits were
already cleared on a previous pass; reads land on recently-inserted keys,
which sit far from the hand. A CLOCK whose hand routinely walked far would
be a degenerate CLOCK.

So the sweep costs roughly one pointer dereference per eviction. A
contiguous bit map cannot beat that.

### Where the Bounded scaling actually comes from

With the sweep ruled out, the README comparison numbers answer it directly.
goache's **unbounded** `Set` — no eviction, no ring, no reference bits —
already scales almost as badly:

```
                n=1,000    n=1,000,000   growth
Set (unbounded)   27.91        127.2       4.56x
Bounded           47.52        269.8       5.68x
```

Eviction's marginal contribution to the scaling is 5.68 / 4.56 ≈ **1.25x**.
The other ~80% is the same effect that slows plain `Set` down: at a million
entries the working set is far past L3, so every map probe and every entry
dereference is a DRAM access. That cost is shared with every other library
in the comparison (all six degrade with size on single-threaded `Get`, as
README's takeaways already note) and is not something an eviction-policy
change can address.

## Decision

**Rejected without implementing.** The projected win does not exist: T5
would optimize a step that measures at ~1 dereference per eviction, while
adding

- a slot allocator and free-slot stack per shard,
- a second indirection on the eviction path (`slots[i]` → entry),
- and — the pre-registered reject condition from the task — **false sharing
  on the read path**: 64 slots share one bit-map word and 512 share a cache
  line, so concurrent `Get`s anywhere in a shard would contend on the same
  line, where today each entry's `referenced` bit sits in its own heap
  object and contends with nothing.

Trading a real concurrency risk for an improvement to a ~0-cost path is not
a trade worth making, and the correctness surface (slot lifecycle across
Set/Delete/Purge/Clear/evict) is the largest of any task on the backlog.

## Consequences

- `cache.go` is unchanged. The CLOCK ring stays as
  [ADR 0016](0016-clock-eviction.md) built it.
- **The premise ADR 0016 stated in the abstract — that the ring walk is the
  locality problem — is now measured and false**, at least for
  churn-and-read workloads. ADR 0016's other documented cost (the pointer
  storage layout itself, which slows *full-map sweeps* like `Purge` and
  `Clear`) is real and separate; that one was attacked by T3 and rejected
  for a different reason ([ADR 0021](0021-reject-inline-storage-unbounded.md)).
- `BenchmarkEvictionChurnLarge` and `BenchmarkEvictionChurnHot` are kept as
  permanent coverage — a 100,000-entry single-shard bound and an
  every-bit-set variant. They are also the cheapest available evidence for
  the finding above: the large-shard case (125 ns/op, a 100,000-slot ring)
  is *faster* than the cross-library n=1,000,000 case (269.8 ns/op, a
  ~1,950-slot ring), which on its own falsifies "longer ring is the cost".
- **The `Bounded`-at-1,000,000 gap is now understood rather than open**: it
  is dominated by working-set-versus-cache effects that also govern
  unbounded `Set`, not by eviction bookkeeping. Closing it would mean
  attacking memory footprint per entry (what T3 tried, and what cost 20% of
  concurrent throughput) or accepting ristretto's trade of dropping writes
  under pressure — a semantic goache deliberately does not copy. No further
  eviction-policy micro-optimization is indicated.
- With T5 rejected, the performance-analysis backlog is fully worked
  through: T1 and T4 accepted, T2 rejected (with a capacity bug found and
  fixed along the way), T3 and T5 rejected.
