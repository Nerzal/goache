# 0026: A second, unsharded implementation for single-core deployments

## Status

Accepted. **The decision stands; the measurement in this record does not.**
The go-cache comparison below was run at `-count=3` against four of the
suite's eight categories, and
[ADR 0027](0027-single-core-field-claim.md) re-ran the full matrix at
`-count=10`. Two of this ADR's numbers do not survive that:

- the read leads reported here as 20-25% are **4.9-6.5%** — go-cache's
  `-count=3` figures were noise-inflated, not goache's;
- `Delete` churn, unmeasured here against go-cache, is a **tie**.

The write leads (-37% on `Set`, -48% on bounded `Set`) and the entire
"against sharded `Cache`" table reproduced. Read ADR 0027's tables as the
current numbers; read this record for why the type exists and what was
rejected on the way.

## Context

[ADR 0025](0025-cpu-constrained-benchmarks.md) measured goache losing to
patrickmn/go-cache at `GOMAXPROCS=1` — 24.51 vs 16.88 ns/op on concurrent
`Get`, a 45% deficit — and concluded that below two cores goache's
architecture is pure overhead. README's use-case guidance was corrected to
send single-core users elsewhere.

That is a large group to send away. Go 1.25+ derives `GOMAXPROCS` from the
cgroup CPU quota, so every Kubernetes pod with `limits.cpu` at or below
`1000m` runs with one `P`.

Sharding relieves contention between threads running *simultaneously*. With
one `P` there is none, and four costs are paid for nothing:

1. a `maphash.Comparable` call per operation purely to pick a shard, on top
   of the hash Go's map does internally anyway;
2. an indirection through the shard slice plus a cache-line-padded shard
   struct;
3. an `s.limit > 0` check on every read and write, even when eviction is
   never configured;
4. per-entry CLOCK metadata — `key`, `referenced`, `prev`, `next` — which
   inflates an entry from 16 bytes to roughly 56 for a string key ([ADR
   0016](0016-clock-eviction.md)), tripling the memory a lookup walks.

## Options considered, and how the probe decided between them

A feasibility probe (five prototypes, `-cpu=1 -count=5`, 100,000-entry
working set, AMD Ryzen AI 9 HX 370) was written before any production code:

| Variant | `Get` ns/op |
|---|---|
| unsharded, one `RWMutex`, `map[K]*{value, expiresAt}` | **16.16-16.68** |
| the same plus goache's `{key, referenced, prev, next}`, unused | 17.00-17.36 |
| go-cache | 17.52-19.22 |
| goache's exact structure with **one** shard and a hashless `shardFor` | 18.63-19.04 |
| goache, 256 shards | 24.34-24.99 |

**A branch in `shardFor` was the obvious cheap option and it is not enough.**
Skipping the hash when `len(c.shards) == 1` recovers 24% (24.7 to 18.8) but
lands *above* go-cache, because costs 2-4 above survive a branch — they are
properties of the struct layout, not of the routing code. A purpose-built
type reaches 16.4.

**Returning an interface from `New` was rejected on measurement.** The
literal "one constructor, two implementations" shape requires `New` to
return an interface, which makes every call indirect and un-inlinable:

```
                              direct         via interface
Get (1 core)                  24.42          26.33          +7.8%
ParallelGet (24 cores)        4.520-4.551    4.636-4.774    +2-5%
```

Roughly 2 ns per call, paid by every goache user including 24-core ones who
gain nothing — and, at 4.5 ns/op, a large fraction of the operation. It also
breaks the existing `*Cache[K, V]` return type.

**Inline value storage was reconsidered and stays rejected.**
[ADR 0021](0021-reject-inline-storage-unbounded.md) rejected `map[K]entry[V]`
because an inline overwrite is a `mapassign` into the bucket array concurrent
readers on other cores are probing — a failure mode that requires two cores,
so it looked revivable here. The probe says no: inline is slower than pointer
storage at one core on every axis (`Get` 19.3 vs 16.4, `Set` 27.7 vs 24.3,
mixed 20.9 vs 19.1). A 16-byte map value doubles the bucket payload the map
probes, costing more than the saved dereference returns — the same
single-threaded null result ADR 0021 measured, now confirmed at one core too.

## Decision

Add `SingleCoreCache[K, V]` and `NewSingleCore`, a second concrete
implementation selected at **compile** time by which constructor the caller
calls. `Cache[K, V]` is not modified — not one line, not even a branch — so
the sharded path cannot regress.

`SingleCoreCache` is one map behind one `sync.RWMutex`, with two entry
layouts chosen once at construction and never mixed: `scEntry`
(`{value, expiresAt}`) when unbounded, `scRingEntry` (plus `key` and CLOCK
fields) when `WithMaxSize` is configured. A one-slot freelist replaces
`Cache`'s `sync.Pool` on both paths — bounded eviction frees exactly one
entry and immediately needs exactly one, which a single slot covers without
any pool bookkeeping.

`Cacher[K, V]`, an interface both types satisfy, ships for callers that must
choose at run time. It is opt-in and its ~2 ns cost is documented on the
interface itself.

## Measurement

`-cpu=1`, 100,000-entry working set, one back-to-back run per table.

### Against go-cache, the library it has to beat (`bench/`, `-count=3`)

```
                    goache single-core   go-cache          sharded goache
Get                 17.13-17.29          22.04-24.26       27.80-30.19
Set                 25.57-27.86          49.93-56.25 (1 alloc)  36.35-38.44
ParallelGet         16.77-17.28          21.04-21.92       26.83-29.62
ParallelGetSet      18.00-18.46          23.35-23.65       28.81-33.64
```

All four pre-registered accept criteria met, by 20-48% rather than the
parity that was the bar. Note these absolute numbers sit above ADR 0025's
for the same benchmarks (different machine state); the comparison is valid
because all three libraries were measured in one run.

### Against sharded `Cache`, benchmark by benchmark (root module)

```
                              Cache        SingleCoreCache
Get                           25.20        16.39           -35%
GetMiss                       60.62        51.69           -15%
Set                           31.51        26.04           -17%
GetWithTTL                    29.93        21.68           -28%
SetWithTTL                    44.59        35.31           -21%
SetMany                       100.1        58.79           -41%  (2->1 allocs)
SetManyRepeated               2513         919.8           -63%
DeleteManyRepeated            1880         203.8           -89%
DeleteSetChurn                91.16        78.35           -14%
Purge                         7.21 ms      3.86 ms         -47%
Clear                         12.90 ms     9.28 ms         -28%  (23.0 -> 8.6 MB)
GetWithMaxSize                27.56        20.55           -25%
SetWithMaxSize                85.60        58.43           -32%
EvictionChurn                 131.5        103.3           -21%
EvictionChurnLarge            146.1        140.6            -4%
EvictionChurnHot              142.8        131.3            -8%
ParallelGet                   24.46        16.25           -34%
ParallelGetSet                27.14        17.70           -35%
ParallelGetConstrained        24.42        16.20           -34%
ParallelGetSetConstrained     26.73        18.37           -31%
```

`DeleteManyRepeated` at -89% and `SetManyRepeated` at -63% are the largest
wins and were not the ones predicted: with one shard there is nothing to
group, so the entire bulk machinery [ADR 0022](0022-bulk-bucket-scratch-reuse.md)
exists to amortize disappears rather than being amortized.

### Risks the plan flagged, now answered

- **One CLOCK ring instead of 256.** [ADR 0023](0023-reject-clock-bitmap.md)
  measured 0.00-2.54 hand steps per eviction at 256 shards and that did not
  carry over automatically. Measured here: `EvictionChurnLarge` (100,000
  entries in one ring) is 140.6 vs `Cache`'s 146.1, and
  `EvictionChurnHot` — every reference bit pre-set, CLOCK's worst case — is
  131.3 vs 142.8. Both *better*. ADR 0023's conclusion holds: the sweep is
  not where bounded-cache cost lives.
- **`Purge`/`Clear` as one long critical section.** Faster, not slower
  (-47% / -28%), and `Clear` allocates 8.6 MB against 23.0 MB.
- **Single-lock write serialization.** `ParallelGetSet` at one core is 17.70
  against `Cache`'s 27.14 and go-cache's ~23.5. One lock is not a liability
  when only one goroutine can run.

### The default path

`cache.go` is untouched by this change, so the sharded path is unregressed
by construction. Verified anyway at 24 cores: `Get` 24.76-25.33,
`Set` 31.92-32.16, `ParallelGet` 4.509-4.602, `ParallelGetSet` 6.184-6.285 —
all matching README's existing numbers.

## Consequences

- **goache now has two implementations to keep correct.** Every future
  feature lands twice or is documented as sharded-only. ADR 0021 rejected a
  `WithInlineStorage()` opt-in partly on "would double the test matrix for
  every operation permanently", and that objection is real here too. It is
  accepted because a branch demonstrably cannot capture the win: the branch
  option was built and measured at 18.8 ns/op against 16.4 for a dedicated
  type, and it captures none of the bulk-path or entry-size wins at all.
  `singlecore_test.go` duplicates `cache_test.go`'s coverage rather than
  sharing it through `Cacher` — a shared test would only prove the interface
  is satisfied, not that each type's own storage, ring and freelist
  bookkeeping is right.
- **A caller who declares single-core and runs on many cores gets one global
  lock.** That is go-cache's profile. Said bluntly in the type's doc comment;
  `New` remains the default recommendation.
- **README's "at one core, reach for something else" guidance is now wrong
  and has been corrected** — that advice is what this change exists to
  invalidate. The crossover framing stays: `New` above one core,
  `NewSingleCore` at one.
- **`SingleCoreCache` fills to exactly its `WithMaxSize` bound.** `Cache`
  can only promise "at most n", since hash skew lets one shard evict while
  others have room ([ADR 0020](0020-shard-count-does-not-scale-eviction.md)).
  With one map there is no skew. Pinned by
  `TestSingleCoreWithMaxSizeFillsToTheBound`.
- **`Len` is O(1)** here rather than O(shard count).
- **A spin-off remains open and is deliberately not part of this change.**
  The probe priced goache's per-entry CLOCK metadata on a cache that never
  evicts at ~5% (17.2 vs 16.4). Unbounded *sharded* shards could store a
  slim `{value, expiresAt}` object — pointer storage kept, only the
  pointed-to struct shrunk — and reclaim that on the default path. This is
  not what ADR 0021 tried: shrinking the target of a pointer keeps writes off
  the bucket array entirely, so ADR 0021's failure mode does not apply. It
  needs its own design and measurement.
- The full pre-registered plan, including the options rejected along the
  way, is [docs/single-core-mode-plan.md](../single-core-mode-plan.md).
