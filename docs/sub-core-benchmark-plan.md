# Plan: measuring goache below one core

Status: **plan, not yet executed.** No decision has been made from it; it
exists so the work is scoped before anyone starts. Feasibility probes cited
below were run on 2026-08-03 and are real.

## Why the existing numbers cannot answer this

[ADR 0025](adr/0025-cpu-constrained-benchmarks.md) added a `GOMAXPROCS`
sweep down to one core and flagged its own limit: `GOMAXPROCS` models *how
many threads run in parallel*, not *how much CPU time the process is
allowed*. Those are different failure modes.

- `GOMAXPROCS=1` means: one thread runs, continuously, forever.
- `limits.cpu: 100m` means: your threads run at full speed for 10 ms, then
  **every thread is frozen for the remaining 90 ms** of the CFS period, and
  this repeats.

A Kubernetes pod at `100m` is the second thing. `GOMAXPROCS` cannot express
it — one core is its floor.

**Measured proof that the difference is large.** Running the existing
single-goroutine `BenchmarkGet` inside `docker run --cpus=0.1`:

| Setting | `BenchmarkGet` |
|---|---|
| host, `GOMAXPROCS=1`, no quota | 25.02 ns/op |
| container, `--cpus=0.1` | **485.3 ns/op** |

19x slower on a benchmark with no concurrency at all — so none of it is
contention, all of it is stall. Proportional scaling would predict ~250
ns/op at 10% quota; the measured 485 is **~1.9x worse than the quota alone
explains**. That excess is the thing worth investigating, and nothing in the
current suite can see it.

Container `cpu.stat` during that run: `nr_throttled 2090`,
`throttled_usec 672278460`, `usage_usec 20870428` — the process spent
roughly 97% of wall-clock frozen.

## What sub-core CPU actually does to a cache

Two distinct effects, and only the first is obvious:

1. **Everything is proportionally slower.** Uninteresting — it applies
   equally to every library and follows from the quota. Any benchmark that
   reports wall-clock mean ns/op measures mostly this.

2. **A goroutine can be frozen while holding a lock.** This is the
   interesting one and it is specific to *design*. When the quota runs out
   mid-critical-section, the lock holder is descheduled and every waiter
   blocks until the next period — up to ~100 ms with the default CFS
   period, four orders of magnitude above a normal cache operation.

Effect 2 is where sharding should matter enormously and where every
benchmark so far is blind:

> **Hypothesis H1 (lock convoy).** With one global lock (go-cache), a holder
> frozen by the throttle blocks *all* traffic. With 256 shards, it blocks
> only the ~1/256 of traffic hashing to that shard. Sharding should
> therefore convert a global stall into a localized one, showing up as a
> large p99.9 difference while the *mean* barely moves.

If H1 holds, README's current statement — "on a single core sharding buys
nothing" — is right about throughput and **wrong about tail latency**, and
goache gains a genuine sub-core argument it does not currently claim.

> **Hypothesis H2 (throttling tax).** The 1.9x excess over proportional
> scaling above is not evenly distributed across operations. Candidates:
> GC competing for the same tiny quota (so allocation rate matters far more
> than at 24 cores — where goache's zero-alloc paths would pay off), and
> period-boundary wake latency.

## What to measure — not ns/op

The probe above is the argument: **485.3 ns/op is a fact about the cgroup,
not about goache.** Every library would show a similar inflation. Under
throttling the wall-clock mean is dominated by stall time and cannot
distinguish designs.

Report instead:

- **Latency percentiles** — p50 / p99 / p99.9 / max. This is where H1 lives;
  a lock convoy is invisible in a mean and unmissable at p99.9.
- **Operations per CPU-second** — throughput normalized by `usage_usec`
  deltas read from `cpu.stat` around the timed region, not by wall clock.
  This is the quota-independent efficiency number and the only one
  comparable across `--cpus` settings.
- **Throttle context** — `nr_throttled` and `throttled_usec` deltas, so
  every result states how throttled it actually was rather than assuming
  the limit was binding.

## Phases

### Phase 0 — lock-hold-time audit (no container, ~half a day)

Bound the blast radius analytically before measuring it. For each operation,
how long is a shard lock held?

- `Get` — `RLock`, one map lookup. Microseconds at worst.
- `Set` — `Lock`, one map assign, plus a CLOCK eviction step when bounded
  ([ADR 0023](adr/0023-reject-clock-bitmap.md) measured that at ~1 step).
- `Purge` / `Clear` — **hold a write lock across an entire shard's map.**
  These are the long poles: `BenchmarkPurge` is ~5.6 ms for 100k entries
  across 256 shards. Per shard that is ~22 µs, but it scales with shard
  occupancy and is the operation most likely to be caught by a throttle.

Deliverable: a table of worst-case hold times, and an explicit note of which
operations a caller should *not* run on a sub-core deployment (likely:
`Purge` on a large cache, unbatched).

This phase is worth doing first because it may make Phase 3 unnecessary or
tell it exactly where to look.

### Phase 1 — latency-histogram harness (no container, ~1 day)

Go's `testing` benchmark reports a mean and auto-calibrates against wall
clock — both wrong here (calibration will pick absurd iteration counts when
90% of wall time is stall; use fixed `-benchtime=Nx` instead, as the probe
above did).

Build a small driver — not a `testing.B` — that runs a fixed operation count
across a fixed goroutine count, records every latency into a histogram, and
prints percentiles plus `cpu.stat` deltas. Keep it in `bench/` so it never
enters the main module.

Useful immediately, container or not: percentiles at `GOMAXPROCS=1` are
already more informative than the means currently published.

### Phase 2 — containerized sweep (~1 day, feasible on this machine today)

`docker run --cpus=X` with the Phase 1 harness, sweeping
X ∈ {0.05, 0.1, 0.25, 0.5, 1.0, 2.0}, across goache and the five compared
libraries at a 100,000-entry working set.

Confirmed working here: Docker Desktop, cgroup v2, `--cpus=0.1` produces
`cpu.max = 10000 100000` and observable throttling counters. The full
toolchain runs — the probe above compiled and ran the real benchmark inside
the container.

### Phase 3 — the sharding experiment (~half a day, the actual point)

Phase 2 compares libraries, which confounds sharding with every other design
difference between them. The clean experiment is **internal**:

| Variant | Isolates |
|---|---|
| goache, default 256 shards | the real thing |
| goache, `WithShardCount(1)` | same code, same allocations, same everything — *only* sharding removed |
| go-cache | independent confirmation |

Any p99.9 gap between the first two is attributable to sharding alone. This
is the measurement that decides H1, and `WithShardCount(1)` makes it
possible without writing a single line of library code.

Sweep shard counts (1 / 4 / 16 / 64 / 256) at `--cpus=0.1` to see whether
tail latency improves monotonically — the signature H1 predicts.

### Phase 4 — publish (~half a day)

If the findings are solid: a README section under "Performance under a CPU
limit", a chart per table (per [ADR 0024](adr/0024-chart-per-benchmark-table.md)),
and an ADR recording the decision. Wire Phase 2 into CI as a manually
triggered job — GitHub Actions `ubuntu-latest` runners support Docker, so
this is reproducible off this machine, which matters (see Risks).

## Risks and limits

- **Docker Desktop on Windows runs in a WSL2 VM.** The throttling is real
  cgroup v2, but the VM adds a scheduling layer between the benchmark and
  the hardware. Numbers measured here are directionally trustworthy;
  anything published should be re-measured on a Linux runner.
- **Percentiles need samples, and sub-core gives few operations per second.**
  At `--cpus=0.05` a p99.9 needs ≥ 10,000 samples, which is minutes of wall
  clock per data point. Budget the sweep accordingly, or drop the lowest
  quota.
- **`testing.B` calibration misbehaves under throttling** — already
  observed. Phase 1's harness must use fixed counts; the probe used
  `-benchtime=200000x` for exactly this reason.
- **The CFS period matters as much as the quota.** All of the above assumes
  the default 100 ms. A cluster tuned to a shorter period has shorter
  stalls and a different tail. Record `cpu.max` verbatim with every result.
- **This measures steady state, not startup.** A throttled pod is slowest
  while its caches are cold; that is a separate question.

## What we do with each outcome

- **H1 confirmed** (sharding materially cuts p99.9 under throttling) —
  correct README's "sharding buys nothing below two cores", which would then
  be true only for throughput. goache gains a real sub-core claim, and
  `WithShardCount` becomes a tuning knob with a documented purpose at low
  CPU rather than only at high.
- **H1 refuted** (no tail difference) — strengthen the existing caveat:
  below one core, recommend go-cache, and say so plainly. That is a useful
  answer too, and cheaper to act on than to keep guessing at.
- **H2 confirmed** (the excess is GC-driven) — allocation-free paths become
  a headline sub-core argument rather than a footnote, and the rejected
  inline-storage work ([ADR 0021](adr/0021-reject-inline-storage-unbounded.md))
  may deserve revisiting *for this deployment shape specifically*, where its
  20% concurrent-throughput cost is irrelevant because there is no
  concurrency to lose.

## Estimated total

Roughly 3–4 focused days for Phases 0–4, of which Phase 0 and Phase 1 are
useful on their own even if the container work is never done.
