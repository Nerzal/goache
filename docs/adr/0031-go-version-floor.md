# 0031: The module's Go floor is 1.25, not whatever was installed

## Status

Accepted

## Context

`go.mod` declared `go 1.26.2` and `bench/go.mod` the same. Neither number was chosen — both are what `go mod init` wrote from the toolchain that happened to be installed.

For an unreleased repository that costs nothing. At the moment of a first tagged release it stops being free: the `go` directive is a hard floor. Every consumer on Go 1.25 would have been told to upgrade their toolchain to use a cache that never needed 1.26 for anything.

## Decision

`go 1.25`, in both modules.

That is the real floor, and it is a principled one rather than a guess:

- **`hash/maphash.Comparable` (Go 1.24)** is how keys are hashed — stdlib, no reflection, no per-key allocation. Below 1.24 the routing in `cache.go` does not exist.
- **`b.Loop()` (Go 1.24)** is what every benchmark in the repo is written against.
- **`sync.Go` (Go 1.25)** is used by the concurrency tests in `cache_test.go` and `singlecore_test.go`. This is the binding constraint, and it comes from tests rather than library code — but the `go` directive covers the whole module, so it sets the floor regardless.
- **Go 1.25 is also where the runtime began deriving `GOMAXPROCS` from the cgroup CPU quota**, which is the entire premise of `NewSingleCore` ([ADR 0025](0025-cpu-constrained-benchmarks.md), [ADR 0026](0026-single-core-cache.md)). A consumer on an older toolchain would not see the behaviour that type exists to serve.

So 1.25 is not merely "the lowest that compiles" — it is the version at which goache's single-core story is true at all.

## Measurement

Tested by lowering the directive and building:

```
go 1.24 → cache_test.go:67:6: sync.Go requires go1.25 or later (module is go1.24)
          (library code itself built clean; only the tests fail)
go 1.25 → go build, go vet, go test, go test -race all clean, both modules
```

`bench/` was checked separately, including a benchmark smoke run — its dependencies (theine, otter/v2, ristretto/v2, freecache, go-cache) all resolve at 1.25.

## Consequences

- Consumers on Go 1.25 can use goache. Under the previous directive they could not.
- CI installs its toolchain via `go-version-file: go.mod`, so it now runs at 1.25 — the declared floor is the version actually tested, which is the correct arrangement and was not previously true.
- Both modules must move together. `bench/` builds against the root module through a `replace`, and CI resolves its Go version from the root `go.mod`, so a root floor below `bench/`'s would break the benchmarks job.
- Dropping `sync.Go` from the tests would allow 1.24, which would buy one more minor version. Not worth rewriting working concurrency tests for, and 1.25 is where `NewSingleCore`'s premise starts holding anyway.
- The floor should be raised deliberately from here, with a reason stated, rather than drifting upward with whatever toolchain is installed when someone next runs `go mod tidy`.
