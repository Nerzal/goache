# 0008: Separate `bench/` module for competitor comparisons

## Status

Accepted

## Context

Needed to benchmark goache head-to-head against other well-known Go cache
libraries (patrickmn/go-cache, coocood/freecache, dgraph-io/ristretto/v2).
Pulling those as direct dependencies of the main module would mean every
consumer of goache transitively depends on them, contradicting the
dependency-free, low-overhead design goal.

## Decision

Created `bench/` as its own Go module (`bench/go.mod`, module
`github.com/Nerzal/goache/bench`), with `replace github.com/Nerzal/goache =>
../` pointing at the local checkout. Competitor libraries are dependencies
of `bench/go.mod` only, never of the root `go.mod`.

`bench/compare_test.go` runs equivalent workloads (string keys, int values,
100k-key working set) across goache and the three competitors, standardizing
on the same operations (`Set`, `Get`, parallel `Get`) despite API
differences (e.g. freecache's `[]byte`-only keys/values needed
`encoding/binary` encoding to keep the comparison fair).

## Consequences

- Root `go.mod` stays dependency-free (verified: `cat go.mod` shows only
  the module declaration and Go version).
- Comparison results are documented in README.md's "Comparison with other
  Go cache libraries" section — run `cd bench && go test -bench=. -benchmem
  -run=^$ ./...` and update both when re-running or adding a competitor.
- Headline results (24-thread machine): goache's parallel Get (~4.47 ns/op)
  beat go-cache (~36.8, naive single-lock) by ~8x, ristretto (~10.0,
  TinyLFU admission-controlled) by ~2.2x, and freecache (~16.7, sharded
  byte-based) by ~3.7x. Single-threaded Get: go-cache edges out goache
  slightly (~19.85 vs ~22.17 ns/op) since an uncontended single mutex beats
  shard-hash overhead when there's no contention to avoid.
