# 0025: Benchmark under a CPU limit, not just on 24 cores

## Status

Accepted

## Context

Every number in README.md was measured on a 24-thread desktop CPU. That is
not where most Go services run. A Kubernetes pod with `limits.cpu: 100m` is
entitled to one tenth of a core; `500m` gets half of one. Since Go 1.25 the
runtime derives `GOMAXPROCS` from the cgroup CPU quota, so such a pod runs
with **`GOMAXPROCS=1`** — one `P`, one goroutine executing at a time — while
the benchmarks that motivated goache's whole design ran with 24.

goache's central design claim is that sharding beats a single global lock
under concurrency, and README's "When goache is the right choice" section
led with an 8x margin over go-cache on concurrent `Get`. That margin was
measured at 24 cores. Whether any of it survives at one core had never been
tested, and the answer determines whether the library's headline advice is
correct for a large fraction of its likely users.

## Decision

Added a CPU-constrained axis to the benchmark suite, reproducible via two
Makefile targets:

- `make bench-cpu` — every `Parallel*` benchmark at `-cpu=1,2,4,8,24`.
- `make bench-compare-cpu` — the same sweep across all six compared
  libraries at a fixed 100,000-entry working set.

`-cpu` needs no new code for the existing benchmarks: `b.RunParallel`
already honours `GOMAXPROCS`. But its default spawns exactly `GOMAXPROCS`
goroutines, so at `-cpu=1` it runs *one* goroutine and measures overhead
with no contention at all — which is not the constrained case, it is the
uncontended case. Two new benchmarks model the real shape:
`BenchmarkParallelGetConstrained` and `BenchmarkParallelGetSetConstrained`
call `b.SetParallelism(32)`, putting 32 goroutines per `P` in flight, the
way a request handler serves many connections on little CPU.

## Measurement

### goache alone (100,000 entries, ns/op)

```
                                    1 core     2       4       8      24
ParallelGet                          23.36   14.02   7.506   7.758   4.614
ParallelGetSet                       27.47   17.32   11.58   9.312   6.044
ParallelGetWithMaxSize               26.74   16.56   8.687   9.067   4.794
ParallelGetConstrained (32/core)     23.29   14.58   7.609   7.762   4.638
ParallelGetSetConstrained (32/core)  25.98   15.35   9.609   10.42   6.639
Get (single-goroutine, reference)    25.02   24.93   25.27   24.16   24.24
```

**At one core, concurrent `Get` costs what a plain single-goroutine `Get`
costs** (23.36 vs 25.02). With one `P` there is no contention for sharding
to relieve, so the shard hash is paid and nothing is returned for it. Cost
roughly halves per core doubling up to four; 24 cores is 5.1x cheaper than
one.

**Oversubscription turned out not to be a distinct regime.** The
`*Constrained` rows are within noise of the defaults for reads (23.29 vs
23.36 at one core) — with a single `P` only one goroutine runs regardless of
how many are runnable, and the read path is too short to be preempted
mid-critical-section often enough to matter. Only mixed read/write at high
core counts pays: `ParallelGetSetConstrained` is ~10% above
`ParallelGetSet` at 24 cores (6.639 vs 6.044), from genuine write
contention among 768 goroutines. The benchmarks are kept because that null
result is itself worth pinning — it says a throttled pod does not need to
cap its handler concurrency on the cache's account.

### Against the other libraries (concurrent `Get`, 100,000 entries, ns/op)

```
              1 core     2       4       8      24
go-cache       16.88   25.50   17.56   27.29   37.41
goache         24.51   14.08   7.399   7.587   4.687
otter          34.38   24.86   9.695   5.899   3.924
ristretto      37.31   32.37   16.10   15.99   8.211
freecache     103.7    52.92   27.16   26.02   16.18
theine        133.5    81.79   75.17   12.57    6.443
```

**go-cache is the fastest library here on a single core, and goache costs
45% more than it** (24.51 vs 16.88). Its single global mutex is uncontended
when only one goroutine can run, so it pays nothing for locking and — unlike
goache — nothing for a shard hash either.

The inversion is immediate and then permanent. At two cores goache costs 45%
*less* than go-cache (14.08 vs 25.50); at 24 cores, 8x less. go-cache gets
monotonically *worse* as cores are added (16.88 → 37.41) because its one
lock is the bottleneck; goache keeps improving.

freecache (103.7) and theine (133.5) are four to six times goache's cost at
one core — their designs assume parallelism a throttled container does not
have. theine only becomes competitive at eight cores.

## Consequences

- **README's use-case guidance was wrong as written and has been
  corrected.** "Many goroutines hit the cache at once" now reads "…*and the
  process has at least two cores*", with the single-core case moved into the
  "reach for something else" list and pointing at the new section. The 8x
  figure is now explicitly labelled as a 24-core number.
- A new README section, "Performance under a CPU limit", carries both
  tables, a chart under each, and the Kubernetes framing (`100m` →
  `GOMAXPROCS=1`). The goache-only chart puts single-goroutine `Get` on the
  same axes as the `Parallel*` lines specifically so the convergence at one
  core is visible rather than only stated.
- The crossover point — between one and two cores — is the single most
  actionable number here: below it goache's architecture is pure overhead,
  above it the architecture is the reason to choose goache.
- These numbers use `GOMAXPROCS` as the proxy for a CPU limit. That models
  how many threads run in parallel, which is what the sharding argument
  turns on, but it does *not* model CFS throttling — a pod that exhausts its
  quota mid-period is descheduled entirely for the remainder, which no
  in-process benchmark can reproduce. Treat the one-core column as the
  optimistic end of the constrained case. A probe since then put the
  existing single-goroutine `BenchmarkGet` inside `docker run --cpus=0.1`
  and measured **485.3 ns/op against 25.02 at GOMAXPROCS=1** — 19x, with no
  concurrency involved, and ~1.9x worse than the 10% quota alone explains.
  [docs/sub-core-benchmark-plan.md](../sub-core-benchmark-plan.md) scopes
  the work to measure that regime properly.
- `docs/benchcharts/main.go`'s line renderer was generalised three ways to
  serve these charts: from a hardcoded working-set axis to any x-axis; from
  a library-keyed colour map to `colorFor`, which falls back to palette
  slots by position for series that are not libraries (and panics rather
  than drawing an invisible line if a chart ever exceeds the validated
  six); and from a fixed legend column to one sized from the longest label,
  so a long series name widens the image instead of being clipped by it.
  All three were caught by rendering the charts and looking at them, not by
  reading the code.
