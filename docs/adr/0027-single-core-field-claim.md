# 0027: Establishing the single-core claim against the whole field

## Status

Accepted

## Context

[ADR 0026](0026-single-core-cache.md) built `SingleCoreCache` and measured it
against patrickmn/go-cache, because go-cache was the library
[ADR 0025](0025-cpu-constrained-benchmarks.md) had shown goache losing to at
`GOMAXPROCS=1`. That was the right target for *that* decision — the question
was whether a second implementation could close a known deficit.

It is the wrong evidence for the claim the README then makes. "The fastest
Go cache on a single core" is a claim about the field, and ADR 0026's
measurement covered:

- **four of eight benchmark categories** — `Set`, `Get`, `ParallelGet`,
  `ParallelGetSet`. `bench/` also measures `SetWithTTL`, `GetWithTTL`,
  `Delete` and `Bounded` for every library, and `GoacheSingleCore_*` had no
  entry in any of them. Those four are precisely where theine and otter
  spend their engineering (hierarchical timer wheels, W-TinyLFU admission),
  so the omitted categories were the competitors' strongest ground.
- **three of seven libraries** — goache sharded, go-cache, and
  `SingleCoreCache` itself. freecache, ristretto, theine and otter were not
  measured at one core at all.
- **one of five working-set sizes**, and at `-count=3`.

## Decision

Complete the matrix before making the claim: every category
`SingleCoreCache` has an equivalent for, against every library that has one,
at one core. `bench/compare_test.go` gains
`BenchmarkGoacheSingleCore_{SetWithTTL,GetWithTTL,Delete,Bounded}` and
`BenchmarkX_ParallelGetSet` for freecache, ristretto, theine and otter — the
mixed read/write axis previously existed only for goache and go-cache.

`make bench-compare-singlecore` is widened from a three-library filter to
the whole field, and `make bench-compare-singlecore-sizes` is added for the
full size sweep.

No cache code changed as part of this ADR. It is a measurement decision, and
the two probes below record why the implementation was left alone.

## Measurement

`-cpu=1`, 100,000-entry working set, AMD Ryzen AI 9 HX 370, Go 1.26.2,
`-count=10`, summarized with `benchstat` (median ± range).

| Category | `NewSingleCore` | Best competitor | Lead |
|---|---|---|---|
| `Bounded` | **54.84n** ± 0% | ristretto 105.6n ± 6% | -48% |
| `Set` | **23.90n** ± 1% | go-cache 38.20n ± 3% | -37% |
| `SetWithTTL` | **33.27n** ± 1% | go-cache 52.56n ± 2% | -37% |
| `ParallelGetSet` | **17.14n** ± 0% | go-cache 19.37n ± 1% | -11.5% |
| `Get` | **16.17n** ± 0% | go-cache 17.29n ± 2% | -6.5% |
| `GetWithTTL` | **21.03n** ± 0% | go-cache 22.41n ± 1% | -6.2% |
| `ParallelGet` | **16.11n** ± 1% | go-cache 16.94n ± 1% | -4.9% |
| `Delete` | **75.11n** ± 0% | go-cache 75.39n ± 1% | -0.4% (a tie) |

Fastest in all eight — but two of these numbers correct the record rather
than extend it, and both corrections go against us.

### The read margins are much thinner than ADR 0026 reported

README claimed 20-25% over go-cache on reads, from ADR 0026's `-count=3`
run. Measured properly the lead is **4.9-6.5%**. The earlier figure was not
a goache measurement error — it was go-cache's numbers being noise-inflated
(`Get` reported as 22.54-35.57 ns/op there, against 17.29n ± 2% here).
README has been corrected downward.

The margins that *are* large — `Set` and `SetWithTTL` at -37%, `Bounded` at
-48% — hold up, and they are the structural ones: go-cache boxes every value
as `interface{}` and allocates on `Set`, which no amount of noise creates or
removes.

### `Delete` is a tie, and that is the pointer-storage trade being paid

Delete-churn (delete then reinsert, see
[ADR 0015](0015-delete-clear-api.md)) decomposed with a standalone probe at
`-count=10`:

```
bare map delete+insert, no lock                  50.94n ± 1%
  + two write-lock pairs                         63.39n ± 1%   +12.45
  + a lookup before the insert                   73.02n ± 1%    +9.63
SingleCoreCache Delete+Set                       78.25n ± 2%    +5.23
```

The +9.63 step is the whole story. `set` looks the key up before assigning,
because pointer storage has to *find* the existing entry to update it in
place and to keep the one-slot freelist fed
([ADR 0019](0019-single-slot-freelist.md)). go-cache stores its `Item`
inline in `map[string]Item` and blindly overwrites — two map operations
where we do three. After a `Delete` our lookup always misses, so in this
churn pattern it is pure cost.

Removing it is not available: without the lookup, every overwrite would
allocate a fresh entry instead of reusing the existing one, and overwrite is
the dominant case in `BenchmarkSet` (keys cycle modulo n). That trades a
narrow churn win for a broad `Set` regression.

Switching to inline storage to avoid the read-before-write is the same
proposal [ADR 0021](0021-reject-inline-storage-unbounded.md) rejected for
`Cache` and [ADR 0026](0026-single-core-cache.md) re-rejected at one core on
its own measurement (`Get` 19.3 vs 16.4). The trade is explicit: pointer
storage costs ~9.6 ns on delete-churn and wins ~2.9 ns on every `Get`. Gets
dominate real cache traffic by orders of magnitude. Keeping pointer storage
and tying go-cache on `Delete` is the correct side of that trade.

### Rejected: keying the freelist so `set` can skip the lookup

`Delete` could park the deleted key alongside the parked entry, letting
`set` skip its map lookup when the very next write targets that same key:

```go
if c.freeData != nil && key == c.freeKey { /* reuse without lookup */ }
```

Rejected without implementing. It accelerates exactly the shape of
`BenchmarkX_Delete` — delete key k, immediately set key k — and pays for it
with a key comparison (a string compare for the common key type) on *every*
`Set`, including the overwhelming majority that follow no `Delete` at all.
That is optimizing the benchmark rather than the workload.

### Where `Get`'s 16.17 ns actually goes

The same probe, decomposing a read against a 100,000-entry
`map[string]*{value, expiresAt}`:

```
key indexing + modulo (benchmark harness only)  0.7508n ± 1%
bare map lookup (includes the above)             12.16n ± 1%
  + sync.RWMutex RLock/RUnlock                   15.71n ± 1%   +3.55
SingleCoreCache.Get                              16.35n ± 2%   +0.64
```

Roughly 70% of a `Get` is the Go map lookup, 22% is the lock, and 4% is
goache's own code. Two conclusions, both of which close off optimization
routes:

- **`sync.Mutex` is not cheaper than `sync.RWMutex` here** — 15.74n vs
  15.71n, indistinguishable. A single-core cache gains nothing from
  downgrading to an exclusive lock.
- **A hand-rolled reader scheme is worth 0.35 ns.** An `atomic.Int32`
  reader counter with no writer-starvation handling at all — not a
  shippable design — measured 15.36n against `sync.RWMutex`'s 15.71n.
  `sync.RWMutex` is already within 2% of the floor set by the two atomic
  read-modify-writes any such scheme must perform.

That leaves ~0.64 ns (3.9%) of addressable overhead in goache's own code,
against a map cost of 11.4 ns that is shared with every competitor that
uses Go's map. Reads are finished; there is no further win here that does
not mean replacing the map, which
[ADR 0005](0005-reject-custom-hash-table.md) already measured at 2.4-3.5x
slower.

## Across working-set sizes: one category does not hold

The table above is n=100,000. Swept across 1,000 / 5,000 / 50,000 /
1,000,000, seven of the eight categories keep their ranking. `Bounded` does
not, and the library that beats us is our own sharded `Cache`:

```
Bounded (limit = n/2), -cpu=1, -count=10, one run
                      NewSingleCore    New (sharded)
n=1,000               50.60n ± 3%      47.77n ± 1%     sharded -5.6%
n=5,000               67.50n ± 2%      60.85n ± 2%     sharded -9.9%
n=50,000              52.53n ± 1%      70.24n ± 1%     ours    -25%
n=100,000             57.87n ± 2%      85.36n ± 3%     ours    -32%
```

Reproduced three times across independent runs, always in the same
direction, at ±1-3%. The crossover sits between 5,000 and 50,000 entries.

The mechanism is plausible: at n=1,000 with `WithMaxSize(500)`, the sharded
cache splits that budget across 256 shards
([ADR 0020](0020-shard-count-does-not-scale-eviction.md)), giving each shard
a CLOCK ring of one or two nodes and a map holding one or two entries — both
trivially cache-resident. `SingleCoreCache` has one ring of 500 heap-allocated
nodes. Sharding, useless for locking at one core, turns out to be useful
here as *partitioning*.

### Refuted: map growth is not the cause

The obvious hypothesis was that the bounded map grows from empty to the
limit while each sharded map never really grows at all — and `WithMaxSize(n)`
being a hard bound means `n` is the exact final size, so pre-sizing to it
wastes nothing. That would have made `NewSingleCore` pre-size by default a
free win.

Measured against the shipped library (`WithMaxSize(l)` plus
`WithCapacity(l)`, no code change needed) and it does nothing:

```
n=1,000     plain 50.60n ± 3%    pre-sized 51.38n ± 3%
n=5,000     plain 67.50n ± 2%    pre-sized 68.56n ± 2%
n=50,000    plain 52.53n ± 1%    pre-sized 52.88n ± 1%
n=100,000   plain 57.87n ± 2%    pre-sized 57.29n ± 2%
```

Indistinguishable at every size, marginally worse at three of four. The
default is left alone.

### Left open

`SingleCoreCache`'s bounded curve is **not monotonic** — n=5,000 (67.50n)
costs more per operation than n=50,000 (52.53n), a smaller cache being
slower than one ten times its size. That reproduces in both the plain and
pre-sized variants, so it is a property of the configuration rather than
noise, and it is not explained by the partitioning argument above. Cause
unidentified; not chased here because the regime it lives in is one where
`New` is the better recommendation anyway. Recorded so the next person to
see it has the measurement rather than a surprise.

## The methodologically important finding: `-count=3` decides the wrong winner

The first full-field run at `-count=3` reported `ParallelGetSet` as a
**loss** — go-cache 20.63-21.39 against `SingleCoreCache`'s 21.25-22.10.
Re-measured in isolation at `-count=10`, the same two benchmarks on the same
machine with no code change gave 22.71-28.24 for go-cache and 17.32-20.78
for `SingleCoreCache`: a 20% win, the opposite verdict.

The tell was that the two libraries moved in *opposite* directions between
runs, which rules out a uniform thermal or frequency drift affecting the
whole run equally.

[ADR 0017](0017-size-parametrized-benchmarks.md) already recorded that
ristretto and theine are individually noisy because of their background
machinery, and concluded that *their* numbers need a second run. This
extends that finding in a way that ADR is explicit about not having covered:
**go-cache and goache — the two synchronous libraries it named as
reproducing closely — are also capable of swapping rank at `-count=3`** when
the margin is under ~15%. The variance is per-benchmark, not per-library.

### And a second one: long runs drift, so position in the run biases results

The size sweep (972 measurements over 29 minutes in one process) reported
`SingleCoreCache` **losing `Set`** to the sharded `Cache` at n=1,000 and
n=5,000 — 34.10n vs 28.93n and 34.91n vs 23.53n, both at ±1-3%, which looks
conclusive. Re-measured in a focused 10-minute run covering only `Set` and
`Bounded`:

```
Set, -cpu=1, -count=10           NewSingleCore   New (sharded)   go-cache
n=1,000                          21.17n ± 1%     29.16n ± 1%     28.64n ± 5%
n=5,000                          21.08n ± 1%     23.57n ± 1%     31.40n ± 1%
n=50,000                         22.75n ± 1%     28.43n ± 2%     37.13n ± 1%
n=100,000                        24.53n ± 2%     32.05n ± 1%     43.66n ± 3%
```

goache wins every size. `SingleCoreCache`'s number moved 34.10 to 21.17
(-38%) between the two runs while the sharded cache's moved 28.93 to 29.16
(+0.8%) — the same opposite-directions signature as the `ParallelGetSet`
case above.

The cause is position within the run. `GoacheSingleCore_*` benchmarks sit at
the end of `bench/compare_test.go` and therefore run last, on a CPU that has
been at full load for twenty-plus minutes. Tight per-benchmark variance says
nothing about this: every sample of a late benchmark is taken under the same
degraded conditions, so `± 1%` is entirely consistent with being 38% wrong.

Note the `Bounded` loss above survived exactly this test, which is why it is
reported as real: it reproduced in the focused short run too, with goache
last in the ordering and therefore disadvantaged if anything.

Adopted as standing practice for any comparative claim in this repo:

- `-count=10` and `benchstat` medians, not `-count=3` ranges. Ranges from a
  low-count run are reported as ranges only, never as a ranking.
- A comparison must come from a **short run containing only the benchmarks
  being compared**. A ranking read off a full-suite sweep is a ranking of
  file positions as much as of implementations.
- Tight variance within a benchmark is not evidence that the benchmark is
  comparable to one that ran twenty minutes earlier.

## Consequences

- **The headline claim is now supported by the matrix it asserts**: eight of
  eight categories at one core, against all six competitors, rather than
  four of eight against one.
- **README's single-core section was corrected downward.** The read leads
  drop from a claimed 20-25% to a measured 4.9-6.5%, and `Delete` is
  reported as the tie it is. Overstated numbers in our favor are worse than
  no numbers.
- **`benchstat` is now a documented dependency of the benchmarking
  workflow** (`go install golang.org/x/perf/cmd/benchstat@latest`). It is a
  tool, not a module dependency — neither `go.mod` is touched.
- **Both probes are recorded here rather than kept**, per the same reasoning
  as ADR 0026's feasibility probe: they answer a question once and would
  otherwise rot as un-run code in the repo.
- **No implementation change came out of this.** The read path is within
  4% of its floor and the `Delete` tie is a deliberate, measured trade. An
  ADR that concludes "measured, nothing to fix, here is why" is the point of
  keeping these records — the next person to look at `Delete` and see a tie
  will find the decomposition instead of re-deriving it.
