# 0028: Runnable examples as API tests

## Status

Accepted

## Context

The public API had no examples at all — no `Example*` functions, no `examples/`
directory. Everything a caller needed to see was in README's "Usage" section and
in doc comments, as prose plus non-compiling snippets.

Two problems with that, and only one of them is about documentation.

The first is reach: pkg.go.dev renders `Example*` functions inline under the
identifiers they name and offers a Run button. A package with none shows bare
signatures. Callers arriving from `go doc` or the module proxy never see the
README at all.

The second is drift, and it is the one that actually matters here. A README
snippet is text. It is not compiled, not run, and not checked against anything.
The snippets in README's "Usage" section had already accumulated a real error —
`c.SetWithTTL("session-token", "abc123", 5*time.Minute)` is called on a
`Cache[string, int]` declared eleven lines earlier and would not compile. Nothing
in `make ci` could have caught it, because nothing in `make ci` reads it. The
same class of rot is what put wrong benchmark numbers in three documents at once,
which [ADR 0027](0027-single-core-field-claim.md) had to go back and correct.

## Decision

Add `example_test.go` in package `goache_test` (external test package, so the
examples compile against the package's public surface exactly as a caller
would), with one runnable example per documented usage pattern:

| Example | Covers |
| --- | --- |
| `ExampleNew` | hit and miss on the default sharded cache |
| `ExampleNew_structKey` | any comparable key type, via `maphash.Comparable` |
| `ExampleNew_withCapacity` | pre-sizing a known-size bulk load |
| `ExampleNew_withMaxSize` | bounded cache, `Len` stops at the limit |
| `ExampleNew_withShardCount` | override the 256 default, rounded to a power of two |
| `ExampleCache_SetMany` | batch insert mixing TTL and no-TTL entries |
| `ExampleCache_Purge` | expired-but-not-reclaimed, and what `Len` counts |
| `ExampleCache_DeleteMany` | bulk delete tolerating absent keys |
| `ExampleNewSingleCore` | the single-core implementation, same API |
| `ExampleCacher` | choosing between the two at run time from `GOMAXPROCS` |

Every one carries an `// Output:` comment, so `go test` executes it and diffs
stdout. They are tests that happen to be documentation, not documentation that
happens to compile.

Rejected: an `examples/` directory of `main` packages. Those compile under
`go build ./...` but never *run*, so they catch signature changes and nothing
else — a `Purge` that stopped reclaiming would pass. They also do not appear on
pkg.go.dev. The only thing they offer over `Example*` is the ability to show a
full program with imports and a `func main`, which is not what this API needs.

Also rejected: examples that print the cache's contents by ranging over it.
Go map iteration order is randomized, so any such example is a flake waiting for
CI. Examples print known keys, `Len()`, and counts instead — stated in the file's
header comment and in CLAUDE.md's testing conventions so the next addition
follows it.

## Consequences

- `make test` now covers the documented API surface. A change that breaks a
  usage pattern fails CI rather than surviving until a user hits it.
- Cost is about 0.1s of test wall-clock, most of it the two `time.Sleep` calls
  the TTL examples need. Same approach as the TTL correctness tests
  ([ADR 0011](0011-lazy-ttl-no-background-janitor.md)) — real short sleeps, no
  mockable clock.
- README's "Usage" section and `example_test.go` are now two statements of the
  same thing and can disagree. README points at the file and CLAUDE.md requires
  new usage patterns to land in both. The examples are the ones that are checked,
  so they are the source of truth when the two differ.
- Examples exercise only the public API, which is the point, but it means they
  add no coverage of shard routing, ring bookkeeping or the freelist. Those stay
  with `cache_test.go` and `singlecore_test.go`.
