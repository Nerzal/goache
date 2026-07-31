// Package goache implements a low-latency, low-allocation, goroutine-safe
// in-memory cache built on generics.
//
// # Architecture
//
// The cache is sharded (lock-striped): keys are hashed and distributed across
// a fixed number of independent shards, each guarded by its own sync.RWMutex.
// This was chosen over two simpler alternatives:
//
//   - A single global sync.RWMutex serializes every writer against every
//     other writer, regardless of key. Under concurrent Set/Get load from
//     many goroutines this becomes the bottleneck long before the map itself
//     does.
//   - sync.Map is optimized for two specific access patterns: keys written
//     once and read many times ("append-only" growth), or many goroutines
//     operating on disjoint key sets. A general-purpose cache with mixed
//     read/write/overwrite traffic on shared keys does not fit that profile
//     and sync.Map falls back to its slower, mutex-guarded "dirty" path,
//     plus it boxes keys/values as any, adding allocation and losing type
//     safety.
//
// Sharding keeps lock contention proportional to 1/shardCount: goroutines
// touching different keys typically land on different shards and never
// block each other, while the per-shard map stays a plain Go map (no
// interface boxing for K/V, since both are generic type parameters).
//
// Shard selection uses hash/maphash.Comparable, which hashes any comparable
// type using the runtime's built-in hash function — no reflection, no
// per-key allocation, and no requirement that callers supply a hash
// function.
//
// Instead, allocation pressure is kept low by: storing values inline in the
// map (no interface{} boxing, since V is a concrete generic type param),
// pre-sizing shard maps via WithCapacity when the final size is known
// upfront (see New), and batching multi-key writes (SetMany) so the map
// only rehashes/grows as needed rather than once per key.
//
// A hand-rolled open-addressed per-shard table (avoiding Go's map entirely,
// to skip the redundant internal re-hash of a key already hashed once for
// shard routing) was tried and measured 2.4-3.5x *slower* across Set/Get/
// parallel-Get than the plain Go map used here. Go's map runtime uses
// SIMD-friendly grouped buckets with a tophash pre-filter and incremental
// (non-stop-the-world) resizing — reproducing that level of engineering in
// pure Go isn't worth attempting again without a much larger effort than
// the one redundant hash call saves. Don't re-attempt this without new
// evidence it can actually win.
//
// Shards are stored as a contiguous []shard value slice, each padded to a
// full cache line, rather than a []*shard pointer slice. An earlier version
// of this cache used the pointer-slice form specifically to avoid false
// sharing, since packing every shard's sync.RWMutex back-to-back with no
// padding measured ~9% slower on concurrent Get. Adding padding instead of
// giving up the contiguous layout measured ~15-20% *faster* on
// BenchmarkParallelGet/BenchmarkParallelGetSet than the pointer-slice
// version — the padding prevents the false sharing the pointer-slice was
// working around, while the value slice still saves one pointer
// dereference and one heap object per shard access. See
// docs/adr/0018-gemini-analysis-experiments.md for the measurements; it
// supersedes docs/adr/0004's pointer-slice conclusion now that padding is
// part of the comparison.
//
// # Optional per-entry TTL
//
// Entries may optionally expire: SetWithTTL and Entry.TTL (for SetMany) set
// an absolute deadline (time.Now().Add(ttl).UnixNano()) stored in entry's
// expiresAt int64 field — 0 means "never expires" and is Go's zero value,
// so entries created via plain Set/SetMany without a TTL cost nothing extra
// anywhere. Expiry is checked lazily in Get: the clock is read only when
// the found entry's expiresAt is non-zero, so looking up a non-TTL entry
// never calls time.Now() and is exactly as fast as before TTL support
// existed (verified by benchmark — see cache_bench_test.go). An expired
// entry is treated as a miss but is not removed from the map by Get, since
// Get only holds a read lock; active reclamation is Purge, which callers
// invoke themselves (e.g. from their own ticker) — goache does not start
// any background goroutine, timer, or ticker of its own. See
// docs/adr/0011-lazy-ttl-no-background-janitor.md for why an internal
// auto-janitor was considered and rejected.
//
// # Automatic eviction (bounded cache size)
//
// WithMaxSize(n) bounds the cache to at most n total entries and turns on
// per-shard eviction: when a shard is full, inserting a new key evicts an
// existing one first. The budget is split so the per-shard limits sum to
// exactly n (the first n%shardCount shards get one extra slot), making n a
// hard ceiling on Len rather than an approximation; when n is below the
// shard count, the shard count is lowered to the largest power of two <= n
// so every shard still gets at least one slot. See
// docs/adr/0020-shard-count-does-not-scale-eviction.md. The policy is CLOCK
// (a second-chance approximation of LRU), chosen deliberately over two
// alternatives:
//
//   - True LRU (move-to-front on every access) would require Get to take a
//     write lock to update recency on every hit, defeating the sharding
//     design's whole point of letting concurrent readers proceed without
//     blocking each other. Rejected for that reason alone.
//   - W-TinyLFU-style frequency-sketch admission (what theine-go and
//     otter/v2 use, see docs/competitor-analysis.md) gives a better hit
//     ratio under skewed/Zipf workloads, but costs real CPU on every Set
//     (updating a frequency estimate, comparing admission candidates) —
//     exactly the bookkeeping goache's Set currently beats those libraries
//     on. Rejected to keep Set's cost close to its current numbers; may be
//     revisited later if hit-ratio-under-skew becomes a measured problem.
//
// CLOCK keeps Get's cost close to zero: each entry carries a single atomic
// "referenced" bit, set by Get with a plain atomic store — no write lock,
// no ring-structure mutation, just one bit flip safe under concurrent
// readers. Eviction (which does need the write lock, since it mutates the
// shard's map and ring) starts at a per-shard "hand" pointer and walks
// forward, clearing the referenced bit on anything it finds set and
// evicting the first entry it finds already clear — approximating "evict
// what hasn't been touched since we last passed by" without ever taking a
// write lock on the read path.
//
// This does cost something even when WithMaxSize isn't used: to let Get
// flip that bit without a write lock, entries must be individually
// addressable, heap-allocated objects rather than plain values inlined in
// the map — so shards store map[K]*entry[K, V] instead of the pre-eviction
// map[K]entry[V]. That means every *new* key (not overwrites of existing
// keys) now costs one heap allocation, whether or not WithMaxSize is ever
// called. This is a real, deliberate trade-off, not an oversight — see
// docs/adr/0016-clock-eviction.md for the full reasoning and the measured
// cost. Ring-maintenance work (linking/unlinking entries, the eviction
// sweep itself) is skipped entirely when a shard has no configured limit,
// so non-eviction users pay the one allocation per new key but no extra
// CPU beyond that.
//
// The obvious fix — give unbounded shards their own inline map[K]entry
// storage and keep pointers only for WithMaxSize shards — was implemented
// in full and reverted. It delivered exactly the wins it promised on
// allocation count and full-map sweeps (Purge -51%, Clear -25% and 100,000
// fewer allocs/op, SetMany/Delete -17%), but cost 20% of concurrent mixed
// read/write throughput (BenchmarkParallelGetSet): storing values inline
// makes an overwrite a mapassign *into the bucket array concurrent readers
// are probing*, where pointer storage writes through the pointer and leaves
// the buckets clean. The predicted single-threaded Get win never appeared
// either. See docs/adr/0021-reject-inline-storage-unbounded.md — the
// allocation wins are real and will keep looking tempting in a profile, so
// don't re-attempt this without first solving that co-location problem.
//
// Shards with a configured WithMaxSize limit recycle entries through a
// sync.Pool: every eviction and every Delete/DeleteMany/Clear/Purge removal
// returns its entry to the pool instead of abandoning it to the GC, and a
// subsequent set for a new key takes one from the pool before falling back
// to a fresh allocation. This measurably cuts both allocation count and
// ns/op on sustained eviction churn (WithMaxSize Set, always-evicting
// churn) — see docs/adr/0018-gemini-analysis-experiments.md. Unbounded
// shards (no WithMaxSize) deliberately never touch the pool: nothing ever
// calls evict() to reclaim into it, so a cold pool.Get() is pure overhead
// over a plain allocation, and Delete/Clear/Purge stay exactly as cheap on
// unbounded caches as they always were.
package goache

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"
)

// defaultShardCount is the number of shards used when no shard count is
// configured. It must be a power of two so shard selection can use a bitmask
// instead of a modulo.
const defaultShardCount = 256

// entry is the value stored per key inside a shard. It's heap-allocated and
// referenced by pointer from shard.data (rather than stored inline as a
// plain value) specifically so Get can flip referenced under only a read
// lock — see the package doc comment's "Automatic eviction" section.
//
// expiresAt is an absolute deadline in UnixNano, or 0 for "never expires".
// 0 is a safe sentinel: it's Set's zero value (entries created via Set/
// SetMany without a TTL never pay any expiry-check cost beyond one int64
// comparison against 0 in Get), and no real deadline is ever exactly the
// Unix epoch.
//
// referenced, prev, and next only matter to shards with a configured
// WithMaxSize limit: referenced is set by Get (atomically, no write lock)
// and cleared/read by the eviction sweep; prev/next thread the entry into
// its shard's circular CLOCK ring and are only ever mutated under the
// shard's write lock. Shards with no limit configured never touch them —
// left at their zero values (false, nil, nil) forever.
type entry[K comparable, V any] struct {
	key        K
	value      V
	expiresAt  int64
	referenced atomic.Bool
	prev, next *entry[K, V]
}

// expired reports whether the entry's TTL (if any) has passed as of now
// (UnixNano). An entry with expiresAt == 0 never expires.
func (e *entry[K, V]) expired(now int64) bool {
	return e.expiresAt != 0 && now >= e.expiresAt
}

// shard is one independently-locked partition of the cache.
//
// hand and limit only matter when limit > 0 (a WithMaxSize was configured):
// hand is the shard's CLOCK hand — the next candidate eviction considers —
// and limit is this shard's share of the configured maximum size. limit == 0
// (the default) means unbounded: no ring is maintained, no eviction ever
// runs, matching the zero-value convention used by WithCapacity/TTL
// elsewhere in this package.
type shard[K comparable, V any] struct {
	mu    sync.RWMutex
	data  map[K]*entry[K, V]
	hand  *entry[K, V]
	limit int
	// free is a one-entry freelist for unbounded shards (limit == 0):
	// Delete/DeleteMany park the removed entry here (a single pointer
	// store — no zeroing, since set overwrites key/value/expiresAt on
	// reuse anyway, and unbounded shards never touch referenced/prev/next)
	// and set takes it back for the next brand-new key, turning the
	// delete-then-reinsert churn workload from one heap allocation per
	// cycle into zero. Bounded shards don't use it — their eviction path
	// already recycles through Cache.entryPool. At most one entry per
	// shard (256 by default) is retained this way, keeping its last
	// key/value alive until reuse — a deliberately bounded, negligible
	// cost. Only ever touched under mu's write lock.
	free *entry[K, V]
	// _ pads the shard out so adjacent shards in Cache.shards don't share a
	// cache line — see the package doc comment's shard-storage paragraph.
	_ [64]byte
}

// set stores key/value (with the given absolute expiresAt deadline, 0 for
// none) in the shard, evicting one entry first if the shard has a
// configured limit and is already full. Caller must hold s.mu (write lock).
func (s *shard[K, V]) set(key K, value V, expiresAt int64, pool *sync.Pool) {
	if e, ok := s.data[key]; ok {
		e.value = value
		e.expiresAt = expiresAt
		if s.limit > 0 {
			e.referenced.Store(true)
		}
		return
	}

	var e *entry[K, V]
	switch {
	case s.limit > 0:
		if len(s.data) >= s.limit {
			s.evict(pool)
		}
		e, _ = pool.Get().(*entry[K, V])
	case s.free != nil:
		e = s.free
		s.free = nil
	default:
		e = &entry[K, V]{}
	}
	e.key = key
	e.value = value
	e.expiresAt = expiresAt
	s.data[key] = e
	if s.limit > 0 {
		s.linkNew(e)
	}
}

// linkNew inserts a freshly-created entry into the shard's circular CLOCK
// ring, just behind the hand (so it's the last entry the hand will revisit).
// Caller must hold s.mu and s.limit must be > 0.
func (s *shard[K, V]) linkNew(e *entry[K, V]) {
	if s.hand == nil {
		e.prev, e.next = e, e
		s.hand = e
		return
	}
	last := s.hand.prev
	last.next = e
	e.prev = last
	e.next = s.hand
	s.hand.prev = e
}

// removeFromRing unlinks e from its neighbors in the CLOCK ring, without
// touching s.hand — callers are responsible for fixing up s.hand themselves
// since the correct replacement differs between an eviction sweep (which
// already knows the next hand position) and an explicit delete (which
// doesn't). A no-op if e is the ring's sole entry (e.next == e); the caller
// must detect that case and reset s.hand to nil itself.
func (s *shard[K, V]) removeFromRing(e *entry[K, V]) {
	if e.next == e {
		return
	}
	e.prev.next = e.next
	e.next.prev = e.prev
}

// evict removes exactly one entry via CLOCK: starting at the hand, skip
// (clearing as it goes) any entry marked referenced, and remove the first
// one that isn't. Caller must hold s.mu (write lock); s.limit must be > 0
// and the shard must be non-empty (s.hand != nil).
func (s *shard[K, V]) evict(pool *sync.Pool) {
	e := s.hand
	for e.referenced.Load() {
		e.referenced.Store(false)
		e = e.next
	}

	next := e.next
	if next == e {
		next = nil
	}
	s.removeFromRing(e)
	delete(s.data, e.key)
	s.hand = next
	release(e, pool)
}

// deleteEntry unlinks e (already removed from s.data by the caller) from
// the CLOCK ring, fixing up s.hand if it pointed at e, and returns e to pool
// for reuse by a future set. Caller must hold s.mu and s.limit must be > 0 —
// unbounded shards never call this; there's no ring to maintain and nothing
// ever calls pool.Get() to reclaim, so releasing to the pool would be pure
// overhead with no payback (see the sync.Pool experiment writeup).
func (s *shard[K, V]) deleteEntry(e *entry[K, V], pool *sync.Pool) {
	if s.hand == e {
		if e.next == e {
			s.hand = nil
		} else {
			s.hand = e.next
		}
	}
	s.removeFromRing(e)
	release(e, pool)
}

// release zeroes e's key/value (so the pool doesn't keep a stale reference
// alive past its natural lifetime) and returns it to pool for a future set
// to reuse — see the package doc comment's "Automatic eviction" section.
func release[K comparable, V any](e *entry[K, V], pool *sync.Pool) {
	var zeroK K
	var zeroV V
	e.key = zeroK
	e.value = zeroV
	e.expiresAt = 0
	e.referenced.Store(false)
	e.prev, e.next = nil, nil
	pool.Put(e)
}

// Entry is a key/value pair used for bulk operations. TTL is optional: zero
// (the default) means the entry never expires. A positive TTL makes the
// entry expire ttl after SetMany is called, same as SetWithTTL.
type Entry[K comparable, V any] struct {
	Key   K
	Value V
	TTL   time.Duration
}

// Cache is a goroutine-safe, sharded key/value cache.
type Cache[K comparable, V any] struct {
	// shards is a contiguous value slice, each element padded to a cache
	// line (see shard's own padding field) — see the package doc comment's
	// shard-storage paragraph for why this beats a []*shard pointer slice
	// once padding is in the picture.
	shards []shard[K, V]
	mask   uint64
	seed   maphash.Seed
	// entryPool recycles entry objects freed by Delete/DeleteMany/Clear/
	// Purge/eviction on WithMaxSize-bounded shards so a subsequent set for
	// a new key can reuse one instead of a fresh heap allocation — see the
	// package doc comment's "Automatic eviction" section.
	entryPool sync.Pool
}

// Option configures a Cache created with New.
type Option func(*config)

type config struct {
	shardCount int
	capacity   int
	maxSize    int
}

// WithShardCount sets the number of shards the cache is split into. The
// value is rounded up to the next power of two. More shards reduce lock
// contention under high concurrency at the cost of a small amount of
// per-shard memory overhead. The default is 256.
//
// Raising this is not a way to make a WithMaxSize-bounded cache evict
// faster — measured across 1,000 to 1,000,000 entries, more shards left
// eviction cost flat at best and up to 25% worse at large sizes (see
// docs/adr/0020-shard-count-does-not-scale-eviction.md). It is also capped
// at maxSize when WithMaxSize is smaller — see WithMaxSize.
func WithShardCount(n int) Option {
	return func(c *config) {
		c.shardCount = n
	}
}

// WithCapacity pre-sizes every shard's underlying map for roughly n total
// items (split evenly across shards), so bulk-loading close to n items
// avoids Go's incremental map growth/rehashing entirely. Use this whenever
// the approximate final size is known upfront — e.g. loading a fixed data
// set at startup — since it turns many small map growths into zero.
// Ignored if n <= 0.
func WithCapacity(n int) Option {
	return func(c *config) {
		c.capacity = n
	}
}

// WithMaxSize bounds the cache to at most n total entries by evicting via
// CLOCK (a second-chance approximation of LRU) once a shard is full — see
// the package doc comment's "Automatic eviction" section for why CLOCK was
// chosen over true LRU or W-TinyLFU-style admission. Ignored if n <= 0 (the
// default: no limit, no eviction), same convention as WithCapacity.
//
// n is a hard upper bound, not an approximation: the budget is split across
// shards so the per-shard limits sum to exactly n, and Len never exceeds it.
// Because keys are distributed by hash rather than perfectly evenly, a
// shard can reach its own share and start evicting while others still have
// room, so a cache under churn typically settles somewhat below n — n
// bounds memory, it doesn't reserve it.
//
// If n is smaller than the configured shard count, the shard count is
// lowered to the largest power of two <= n (every shard needs at least one
// slot). A cache that small doesn't need many shards to avoid contention,
// so this costs nothing in practice.
func WithMaxSize(n int) Option {
	return func(c *config) {
		c.maxSize = n
	}
}

// New creates an empty Cache.
func New[K comparable, V any](opts ...Option) *Cache[K, V] {
	cfg := config{shardCount: defaultShardCount}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.shardCount < 1 {
		cfg.shardCount = 1
	}
	n := nextPowerOfTwo(cfg.shardCount)

	// A bounded cache can't spread its budget over more shards than it has
	// entries to give them: every shard needs a limit of at least 1 (a limit
	// of 0 means "unbounded" by this package's convention), so a shard count
	// above maxSize would make the cache's real capacity shardCount rather
	// than maxSize. Cap the shard count at the largest power of two <=
	// maxSize so that can't happen.
	if cfg.maxSize > 0 && n > cfg.maxSize {
		n = previousPowerOfTwo(cfg.maxSize)
	}

	perShard := 0
	if cfg.capacity > 0 {
		perShard = (cfg.capacity + n - 1) / n
	}

	shards := make([]shard[K, V], n)
	for i := range shards {
		shards[i].data = make(map[K]*entry[K, V], perShard)
	}

	if cfg.maxSize > 0 {
		// Split maxSize across shards so the per-shard limits sum to
		// exactly maxSize: every shard gets maxSize/n, and the first
		// maxSize%n shards get one extra slot to absorb the remainder.
		// Rounding every shard up instead (the obvious ceil) would let the
		// cache hold up to n-1 entries more than asked for.
		base, rem := cfg.maxSize/n, cfg.maxSize%n
		for i := range shards {
			shards[i].limit = base
			if i < rem {
				shards[i].limit++
			}
		}
	}

	c := &Cache[K, V]{
		shards: shards,
		mask:   uint64(n - 1),
		seed:   maphash.MakeSeed(),
	}
	c.entryPool.New = func() any { return &entry[K, V]{} }
	return c
}

func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// previousPowerOfTwo returns the largest power of two <= n. n must be >= 1.
func previousPowerOfTwo(n int) int {
	p := 1
	for p<<1 <= n {
		p <<= 1
	}
	return p
}

func (c *Cache[K, V]) shardFor(key K) *shard[K, V] {
	h := maphash.Comparable(c.seed, key)
	return &c.shards[h&c.mask]
}

// Set adds or overwrites a single key/value pair with no expiry. Identical
// cost to before TTL support existed — it never touches the clock.
func (c *Cache[K, V]) Set(key K, value V) {
	s := c.shardFor(key)
	s.mu.Lock()
	s.set(key, value, 0, &c.entryPool)
	s.mu.Unlock()
}

// SetWithTTL adds or overwrites a single key/value pair that expires after
// ttl. ttl <= 0 is treated as "no expiry", same as Set — consistent with
// WithCapacity's "n <= 0 is ignored" convention elsewhere in this package.
func (c *Cache[K, V]) SetWithTTL(key K, value V, ttl time.Duration) {
	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UnixNano()
	}
	s := c.shardFor(key)
	s.mu.Lock()
	s.set(key, value, expiresAt, &c.entryPool)
	s.mu.Unlock()
}

// SetMany adds or overwrites 1..N key/value pairs, each optionally expiring
// per its own Entry.TTL. Entries are grouped by destination shard up front
// so each shard's lock is acquired at most once, instead of once per entry.
// time.Now() is called at most once per SetMany call (only if at least one
// entry has a positive TTL), not once per entry.
func (c *Cache[K, V]) SetMany(entries []Entry[K, V]) {
	if len(entries) == 0 {
		return
	}

	var now time.Time
	nowSet := false

	buckets := make([][]Entry[K, V], len(c.shards))
	for _, e := range entries {
		idx := maphash.Comparable(c.seed, e.Key) & c.mask
		buckets[idx] = append(buckets[idx], e)
	}

	for i, bucket := range buckets {
		if len(bucket) == 0 {
			continue
		}
		s := &c.shards[i]
		s.mu.Lock()
		for _, e := range bucket {
			var expiresAt int64
			if e.TTL > 0 {
				if !nowSet {
					now = time.Now()
					nowSet = true
				}
				expiresAt = now.Add(e.TTL).UnixNano()
			}
			s.set(e.Key, e.Value, expiresAt, &c.entryPool)
		}
		s.mu.Unlock()
	}
}

// Get returns the value stored for key, and whether it was found. An entry
// past its TTL is treated as a miss without being removed from the shard —
// see Purge for active reclamation. The clock is only read when the found
// entry actually has a TTL (expiresAt != 0), so looking up entries created
// via Set/SetMany without a TTL costs exactly what it did before TTL
// support existed. When the cache has a configured WithMaxSize limit, a hit
// also flips the entry's CLOCK "referenced" bit via a single atomic store —
// no write lock is taken to do this, so concurrent Get calls never block
// each other.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	s := c.shardFor(key)
	s.mu.RLock()

	e, ok := s.data[key]
	if !ok {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}
	if e.expiresAt != 0 && time.Now().UnixNano() >= e.expiresAt {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}
	if s.limit > 0 {
		e.referenced.Store(true)
	}
	v := e.value
	s.mu.RUnlock()
	return v, true
}

// Delete removes key from the cache, if present. Deleting a key that isn't
// present is a no-op.
func (c *Cache[K, V]) Delete(key K) {
	s := c.shardFor(key)
	s.mu.Lock()
	if e, ok := s.data[key]; ok {
		delete(s.data, key)
		if s.limit > 0 {
			s.deleteEntry(e, &c.entryPool)
		} else {
			s.free = e
		}
	}
	s.mu.Unlock()
}

// DeleteMany removes 1..N keys. Keys are grouped by destination shard up
// front so each shard's lock is acquired at most once, instead of once per
// key — same pattern as SetMany. Keys not present are silently skipped.
func (c *Cache[K, V]) DeleteMany(keys []K) {
	if len(keys) == 0 {
		return
	}

	buckets := make([][]K, len(c.shards))
	for _, k := range keys {
		idx := maphash.Comparable(c.seed, k) & c.mask
		buckets[idx] = append(buckets[idx], k)
	}

	for i, bucket := range buckets {
		if len(bucket) == 0 {
			continue
		}
		s := &c.shards[i]
		s.mu.Lock()
		for _, k := range bucket {
			if e, ok := s.data[k]; ok {
				delete(s.data, k)
				if s.limit > 0 {
					s.deleteEntry(e, &c.entryPool)
				} else {
					s.free = e
				}
			}
		}
		s.mu.Unlock()
	}
}

// Clear removes every entry from the cache, across all shards. Each shard's
// underlying map storage is retained (via the built-in clear) rather than
// replaced, so a Clear followed by refilling the cache to roughly its
// previous size doesn't pay for map growth again. Any CLOCK ring a shard
// was maintaining is discarded along with it (its entries are only
// reachable from each other after this, and Go's tracing GC collects that
// cycle normally once nothing outside references it).
func (c *Cache[K, V]) Clear() {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		if s.limit > 0 {
			for _, e := range s.data {
				release(e, &c.entryPool)
			}
		}
		clear(s.data)
		s.hand = nil
		s.mu.Unlock()
	}
}

// Len returns the number of items physically stored in the cache. This
// includes entries whose TTL has passed but haven't been reclaimed yet by
// Purge — Len is O(shard count), not O(item count), specifically so it
// stays cheap regardless of cache size; Get is the source of truth for
// whether any individual key is still alive. Call Purge first if you need
// an exact live count.
func (c *Cache[K, V]) Len() int {
	total := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.RLock()
		total += len(s.data)
		s.mu.RUnlock()
	}
	return total
}

// Purge actively removes all expired entries and returns how many were
// removed. Nothing calls this automatically — goache starts no background
// goroutines, timers, or tickers of its own (see the package doc comment
// and docs/adr/0011-lazy-ttl-no-background-janitor.md for why). Callers
// using TTLs and who need bounded memory for keys that expire and are
// never looked up again should call Purge periodically themselves (e.g.
// from their own ticker).
func (c *Cache[K, V]) Purge() int {
	now := time.Now().UnixNano()
	removed := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k, e := range s.data {
			if e.expired(now) {
				delete(s.data, k)
				if s.limit > 0 {
					s.deleteEntry(e, &c.entryPool)
				}
				removed++
			}
		}
		s.mu.Unlock()
	}
	return removed
}
