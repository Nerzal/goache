# 0029: The comparison benchmarks select goache's implementation by core count

## Status

Accepted

## Context

`bench/compare_test.go` compares goache against six other cache libraries. Every
`BenchmarkGoache_*` in it constructed its cache with `goache.New`, at every core
count, because when those benchmarks were written `New` was the only constructor.

[ADR 0026](0026-single-core-cache.md) added a second one. `NewSingleCore` returns
a different type for processes at `GOMAXPROCS=1`, and
[ADR 0027](0027-single-core-field-claim.md) measured what it is worth there: at
n=100k it beats every library in the comparison in all eight categories, and it
beats the sharded `Cache` by 14-90%.

The comparison did not know that. Run at `-cpu=1` — which is what
`make bench-compare-cpu` does, and what a Kubernetes pod at `limits.cpu: 1000m`
or below gives you under Go 1.25+ — the row labelled "goache" was the sharded
`Cache`: a routing hash and a shard-slice indirection priced with none of the
contention relief they exist to buy, since there is no second thread to spread
across shard mutexes. That is how [ADR 0025](0025-cpu-constrained-benchmarks.md)
found goache losing to patrickmn/go-cache at one core in the first place.

The comparison was therefore measuring something no informed caller would run.
It is also not the handicap the competitors carry: go-cache has exactly one
implementation and it is the same one at every core count. A benchmark that
picks goache's wrong implementation and the competitor's only implementation is
not measuring the libraries against each other.

## Decision

`BenchmarkGoache_*` selects the implementation a caller on that machine should
be using:

```go
func singleCore() bool {
	return runtime.GOMAXPROCS(0) == 1
}

func BenchmarkGoache_Set(b *testing.B) {
	if singleCore() {
		benchSingleCoreSet(b)
		return
	}
	benchShardedSet(b)
}
```

Selection happens in the benchmark entry point, before any timing starts, so it
costs nothing that lands in a measurement.

Each workload body exists once and has three entry points:

| Entry point | Implementation | Purpose |
| --- | --- | --- |
| `BenchmarkGoache_*` | selected by `GOMAXPROCS` | the headline row; what belongs in a cross-library table |
| `BenchmarkGoacheSingleCore_*` | always `NewSingleCore` | the losing side above one core |
| `BenchmarkGoacheSharded_*` | always `New` | the sharded numbers at one core |

`BenchmarkGoacheSharded_*` is new here and is not optional. Before it, making
`BenchmarkGoache_*` select `NewSingleCore` at `-cpu=1` would have made the
sharded numbers unreachable at one core — and those are exactly where ADR 0027's
"against `New`" table and the bounded crossover below both come from. Adding the
selection without the escape hatch would have removed measurements to gain one.

### Rejected: routing both through `Cacher`

The obvious implementation is one `goache.Cacher[string, int]` variable assigned
from either constructor, which collapses the three entry points into one body
with no duplication at all.

It also puts an interface dispatch — roughly 2 ns/op, the cost measured on
`Cacher` itself and documented in its doc comment — inside the measured loop of
every goache benchmark at every core count. On a `Get` of 16.17 ns that is over
12%, and it would apply to the 24-core numbers too, silently reducing every
existing goache result in the README. The thing being measured cannot be routed
through the thing whose cost is the reason both types exist. Sharing the workload
body and keeping the concrete type is the version that duplicates a little source
and nothing at run time.

### Rejected: selecting per size for `Bounded`

`Bounded` is the one category where "select what the caller should use" and
"select the faster one" disagree. ADR 0027 measured the sharded `Cache` evicting
faster at one core below roughly 10k entries — 47.77 vs 50.60 ns/op at n=1k —
because 256 shards partition the CLOCK ring into rings one or two nodes long.
Reproduced again while making this change:

```
BenchmarkGoacheSharded_Bounded/n=1000       47.10-48.12 ns/op
BenchmarkGoacheSingleCore_Bounded/n=1000    50.54-54.33 ns/op
BenchmarkGoache_Bounded/n=1000              51.20-53.64 ns/op   (selects SingleCore)
```

Selecting `New` for `Bounded` below a threshold would report goache's best
number, but the threshold has only ever been bracketed between 5k and 50k, never
pinned, and encoding an unpinned constant would make this the only benchmark in
the file whose implementation changes mid-sweep. `Bounded` follows the same rule
as the other seven categories. The exception stays documented in the README, and
`BenchmarkGoacheSharded_Bounded` is how it is measured.

## Measurement

Verification that selection actually fires, at `n=100000`, `-count=6`:

At `-cpu=1`, `BenchmarkGoache_*` tracks `SingleCore`:

```
Goache_Set             25.20-27.78 ns/op
GoacheSingleCore_Set   25.25-26.08 ns/op
Goache_Get             16.76-17.30 ns/op
GoacheSingleCore_Get   16.63-17.38 ns/op
```

At `-cpu=4`, `ParallelGet`, `-count=5`, it tracks the sharded `Cache` instead,
and the forced single-core entry point shows what the wrong choice costs there:

```
Goache_ParallelGet             7.930-9.840 ns/op
GoacheSharded_ParallelGet      7.902-9.698 ns/op
GoacheSingleCore_ParallelGet   17.96-19.08 ns/op
```

## Consequences

- The cross-library comparison at `-cpu=1` now reports goache as a caller would
  experience it. The README's single-core tables already carried these numbers,
  sourced from `BenchmarkGoacheSingleCore_*`; they are now what the headline
  benchmark produces directly.
- `BenchmarkGoache_*` and one of the two forced sets measure the same thing at
  any given core count, so a full `-bench=.` run does that work twice. This is
  the price of keeping both sides reachable at every core count. `make
  bench-singlecore-vs-sharded` runs only the two forced sets.
- A benchmark result is now only interpretable together with the `-cpu` it ran
  at. It always was — that is ADR 0025's whole point — but previously the goache
  row was at least the same code at every core count.
- No library code changed. This is a change to what the benchmarks construct.
