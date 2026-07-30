# 0013: TTL API shape — SetWithTTL + Entry.TTL, Set left untouched

## Status

Accepted

## Context

Needed an API for optional per-entry expiry that (a) doesn't cost anything
for callers who never use it, (b) works for both the single-item and bulk
paths, and (c) follows conventions already established elsewhere in this
package.

## Decision

- **`Set(key, value)` is untouched** — same signature, same code, same
  cost as before TTL existed. A dedicated `SetWithTTL(key, value, ttl
  time.Duration)` method is added instead of adding a TTL parameter to
  `Set` (which would force every caller, TTL or not, to pass something,
  and would tempt a "just pass 0" pattern that's easy to get backwards).
- **`Entry[K, V]` (used by `SetMany`) gains a `TTL time.Duration` field**,
  zero value meaning "never expires" — so existing `SetMany` call sites
  (`Entry{Key: k, Value: v}`, using named fields) keep compiling and keep
  their original no-expiry behavior with no code change.
- **`ttl <= 0` means "no expiry"** for both `SetWithTTL` and `Entry.TTL` —
  consistent with `WithCapacity`'s existing "n <= 0 is ignored" convention
  ([0006](0006-with-capacity-pre-sizing.md)). Considered the alternative
  (`ttl <= 0` = "expire immediately", closer to some other caches' `PEXPIRE
  0` semantics) but rejected it: `SetWithTTL` is a dedicated, deliberately
  named method, so a zero-value `time.Duration` reaching it is far more
  likely to be an accidental unset field than an intentional request to
  store-then-immediately-expire — following the established package
  convention was judged safer.
- **Deadline representation**: `entry.expiresAt` is an absolute
  `int64` UnixNano deadline (`time.Now().Add(ttl).UnixNano()`), computed
  once at `Set`/`SetMany` time, not a relative duration re-checked against
  an elapsed-time counter. Standard "absolute wall-clock deadline" TTL
  semantics, same as most cache libraries — accepts the same wall-clock-
  adjustment caveat (NTP jumps) that virtually all such caches accept, in
  exchange for a trivial expiry check (one int64 comparison against
  `time.Now().UnixNano()`).
- **`SetMany` calls `time.Now()` at most once per call**, not once per
  entry — only if at least one `Entry` in the batch has `TTL > 0`
  (`nowSet` guard), reusing that single reading for every entry's deadline
  computation in that batch.

## Consequences

- Callers not using TTL write no different code than phase 1 and pay no
  different cost than phase 1 (see [0012](0012-entry-ttl-field-size-cost.md)
  for the one caveat: cache/shard *creation* cost changed slightly because
  `entry` got wider — steady-state `Get`/`Set` did not).
- Two ways to set a TTL (`SetWithTTL` for one key, `Entry.TTL` for
  `SetMany`) rather than one unified way — judged acceptable since they
  mirror the existing `Set`/`SetMany` split rather than introducing a third
  pattern.
- No `GetWithTTLRemaining` or similar introspection API was added — not
  asked for, and would be premature surface area for a phase-1-adjacent
  feature; add it later only if a real caller needs it.
