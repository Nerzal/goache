package goache

import (
	"sync"
	"sync/atomic"
	"time"
)

// SingleCoreCache is a goroutine-safe key/value cache for processes that run
// on a single core, where Cache's sharded design is pure overhead.
//
// # When to use this instead of Cache
//
// Cache is lock-striped: keys are hashed and routed to one of N independently
// locked shards so concurrent goroutines on different cores don't block each
// other. That only pays off when goroutines actually run at the same time. A
// Kubernetes pod with limits.cpu at or below 1000m runs with GOMAXPROCS=1
// under Go 1.25+, which means exactly one goroutine executes at a time — and
// there Cache pays for machinery that returns nothing:
//
//   - a hash/maphash.Comparable call per operation purely to pick a shard,
//     on top of the hash Go's map does internally anyway,
//   - an indirection through the shard slice plus a cache-line-padded shard
//     struct,
//   - an s.limit > 0 check on every read and write even when eviction is
//     never configured,
//   - per-entry CLOCK metadata (key, referenced, prev, next) that inflates
//     an entry from 16 bytes to roughly 56 for a string key, tripling the
//     working set a lookup walks.
//
// SingleCoreCache drops all four. It is one map behind one sync.RWMutex, with
// entries holding only what the configured feature set needs. Measured at
// GOMAXPROCS=1 against a 100,000-entry working set, it is the fastest of the
// seven caches bench/ compares in all eight benchmark categories — but the
// size of that lead varies enormously by category, and quoting one number
// for it would be misleading:
//
//   - Writes win big: Set 23.90 vs go-cache's 38.20 ns/op (-37%), and
//     bounded Set 54.84 vs ristretto's 105.6 (-48%). go-cache boxes every
//     value as interface{} and allocates; goache does neither.
//   - Reads win narrowly: Get 16.17 vs go-cache's 17.29 (-6.5%). About 70%
//     of a Get is Go's own map lookup, which every competitor built on
//     map[K]V pays identically.
//   - Delete-then-reinsert churn is a tie (75.11 vs 75.39). Pointer storage
//     must look a key up to recycle its entry where go-cache, storing values
//     inline, blindly overwrites. That lookup costs ~9.6 ns here and saves
//     ~2.9 ns on every Get — a deliberate trade, not an oversight.
//
// Against the sharded Cache at one core the margin is wider and more uniform
// (-14% to -89%). See docs/adr/0027-single-core-field-claim.md for the full
// matrix and the cost decompositions, docs/adr/0026-single-core-cache.md for
// why this type exists, and docs/adr/0025-cpu-constrained-benchmarks.md for
// the crossover point.
//
// The trade is exact and one-directional: with two or more cores available,
// Cache pulls ahead immediately and keeps improving as cores are added,
// while SingleCoreCache's one lock becomes the bottleneck — the same shape
// go-cache has. **If you declare single-core and then run on many cores, you
// get one global lock and you will feel it.** When in doubt, use New.
//
// # What it does not change
//
// Locking is not weakened. GOMAXPROCS=1 does not make a mutex unnecessary:
// goroutines still interleave via preemption, and this type must stay correct
// on any machine regardless of what the caller claimed. Nothing here starts a
// background goroutine, timer, or ticker either — TTL reclamation is Purge,
// called by the caller, exactly as with Cache (see
// docs/adr/0011-lazy-ttl-no-background-janitor.md).
//
// # Storage
//
// Two entry layouts, chosen once in NewSingleCore and never mixed: without
// WithMaxSize, entries are scEntry (value + deadline, 16 bytes for an int
// value) held in data; with WithMaxSize, they are scRingEntry (value +
// deadline + key + CLOCK bookkeeping) held in clock, since eviction needs the
// key to delete by and a ring to sweep. Exactly one of the two maps is
// non-nil for the lifetime of the cache, so the unbounded path never carries
// the bounded path's per-entry cost.
//
// Values are stored behind a pointer rather than inline in the map. Inline
// storage was measured here too and is slower on every axis at one core
// (Get 19.3 vs 16.4 ns/op, Set 27.7 vs 24.3, mixed read/write 20.9 vs 19.1):
// a 16-byte map value doubles the bucket payload the map probes, which costs
// more than the saved dereference returns. See
// docs/adr/0021-reject-inline-storage-unbounded.md, which reached the same
// conclusion for Cache by a different route.
type SingleCoreCache[K comparable, V any] struct {
	mu sync.RWMutex

	// data holds entries when no WithMaxSize was configured; clock holds them
	// when one was. Exactly one is non-nil.
	data  map[K]*scEntry[V]
	clock map[K]*scRingEntry[K, V]

	// hand and limit are only meaningful when clock is non-nil: hand is the
	// CLOCK hand (the next eviction candidate) and limit is the configured
	// maximum entry count.
	hand  *scRingEntry[K, V]
	limit int

	// freeData/freeRing are one-slot freelists holding the most recently
	// removed entry so the next brand-new key can reuse it instead of
	// allocating — the same idea as Cache's per-shard freelist
	// (docs/adr/0019-single-slot-freelist.md), and a perfect fit for the
	// bounded path in particular, where every insert past the limit frees
	// exactly one entry and immediately needs exactly one. Only ever touched
	// under mu's write lock, so no sync.Pool is needed on either path.
	freeData *scEntry[V]
	freeRing *scRingEntry[K, V]
}

// scEntry is the unbounded storage entry: the value and its absolute
// expiry deadline in UnixNano (0 = never expires), and deliberately nothing
// else. A cache that never evicts has no use for the key (the map already
// has it) or for CLOCK bookkeeping, and leaving those fields out is a
// measurable part of why this type is faster than Cache at one core.
type scEntry[V any] struct {
	value     V
	expiresAt int64
}

// scRingEntry is the bounded storage entry. It carries key (eviction deletes
// from the map by key), referenced (the CLOCK second-chance bit), and
// prev/next (the circular sweep ring) on top of what scEntry holds.
//
// referenced stays an atomic.Bool even though this type targets single-core
// deployments: Get flips it while holding only a read lock, so concurrent
// Gets would otherwise be a genuine data race — one that GOMAXPROCS=1 makes
// unlikely to manifest but does not prevent, and that -race would rightly
// report.
type scRingEntry[K comparable, V any] struct {
	key        K
	value      V
	expiresAt  int64
	referenced atomic.Bool
	prev, next *scRingEntry[K, V]
}

// NewSingleCore creates an empty SingleCoreCache.
//
// It accepts the same Options as New. WithCapacity pre-sizes the single
// underlying map and WithMaxSize bounds the cache and turns on CLOCK
// eviction, both exactly as they do for Cache. WithShardCount is meaningless
// here — this cache has no shards — and is ignored rather than treated as an
// error, so one shared []Option can be passed to either constructor.
//
// Typical use, deciding at startup from the runtime's own view of the CPU
// budget:
//
//	if runtime.GOMAXPROCS(0) == 1 {
//		cache = goache.NewSingleCore[string, User]()
//	} else {
//		cache = goache.New[string, User]()
//	}
//
// Note that binding the two types to one variable like that requires a
// Cacher[K, V] interface, whose dynamic dispatch costs roughly 2 ns per
// call — see Cacher.
func NewSingleCore[K comparable, V any](opts ...Option) *SingleCoreCache[K, V] {
	cfg := config{shardCount: defaultShardCount}
	for _, opt := range opts {
		opt(&cfg)
	}

	capacity := max(cfg.capacity, 0)

	c := &SingleCoreCache[K, V]{}
	if cfg.maxSize > 0 {
		c.limit = cfg.maxSize
		// Pre-sizing beyond the limit would reserve buckets the cache can
		// never fill.
		capacity = min(capacity, cfg.maxSize)
		c.clock = make(map[K]*scRingEntry[K, V], capacity)
		return c
	}
	c.data = make(map[K]*scEntry[V], capacity)
	return c
}

// Set adds or overwrites a single key/value pair with no expiry.
func (c *SingleCoreCache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	c.set(key, value, 0)
	c.mu.Unlock()
}

// SetWithTTL adds or overwrites a single key/value pair that expires after
// ttl. ttl <= 0 is treated as "no expiry", same as Set and same as Cache.
func (c *SingleCoreCache[K, V]) SetWithTTL(key K, value V, ttl time.Duration) {
	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UnixNano()
	}
	c.mu.Lock()
	c.set(key, value, expiresAt)
	c.mu.Unlock()
}

// SetMany adds or overwrites 1..N key/value pairs, each optionally expiring
// per its own Entry.TTL.
//
// Unlike Cache.SetMany this does no shard grouping — there is one shard, so
// there is nothing to group. The lock is taken once and the entries are
// applied in order, which skips the per-entry routing hash, the per-call
// bucket-slice bookkeeping and the scratch-space handoff that
// docs/adr/0022-bulk-bucket-scratch-reuse.md exists to amortize for Cache.
// time.Now() is still called at most once per call, and only if at least one
// entry carries a positive TTL.
func (c *SingleCoreCache[K, V]) SetMany(entries []Entry[K, V]) {
	if len(entries) == 0 {
		return
	}

	var now time.Time
	nowSet := false

	c.mu.Lock()
	for _, e := range entries {
		var expiresAt int64
		if e.TTL > 0 {
			if !nowSet {
				now = time.Now()
				nowSet = true
			}
			expiresAt = now.Add(e.TTL).UnixNano()
		}
		c.set(e.Key, e.Value, expiresAt)
	}
	c.mu.Unlock()
}

// set stores key/value with the given absolute deadline, evicting one entry
// first if the cache is bounded and already full. Caller must hold c.mu
// (write lock).
func (c *SingleCoreCache[K, V]) set(key K, value V, expiresAt int64) {
	if c.limit > 0 {
		c.setBounded(key, value, expiresAt)
		return
	}

	if e, ok := c.data[key]; ok {
		e.value = value
		e.expiresAt = expiresAt
		return
	}

	e := c.freeData
	if e != nil {
		c.freeData = nil
	} else {
		e = &scEntry[V]{}
	}
	e.value = value
	e.expiresAt = expiresAt
	c.data[key] = e
}

// setBounded is set's WithMaxSize path. Caller must hold c.mu and c.limit
// must be > 0.
func (c *SingleCoreCache[K, V]) setBounded(key K, value V, expiresAt int64) {
	if e, ok := c.clock[key]; ok {
		e.value = value
		e.expiresAt = expiresAt
		e.referenced.Store(true)
		return
	}

	if len(c.clock) >= c.limit {
		c.evict()
	}

	e := c.freeRing
	if e != nil {
		c.freeRing = nil
	} else {
		e = &scRingEntry[K, V]{}
	}
	e.key = key
	e.value = value
	e.expiresAt = expiresAt
	c.clock[key] = e
	c.linkNew(e)
}

// linkNew inserts a freshly-created entry into the circular CLOCK ring just
// behind the hand, so it is the last entry the hand will revisit. Caller must
// hold c.mu and c.limit must be > 0.
func (c *SingleCoreCache[K, V]) linkNew(e *scRingEntry[K, V]) {
	if c.hand == nil {
		e.prev, e.next = e, e
		c.hand = e
		return
	}
	last := c.hand.prev
	last.next = e
	e.prev = last
	e.next = c.hand
	c.hand.prev = e
}

// evict removes exactly one entry via CLOCK: starting at the hand, skip
// (clearing as it goes) any entry marked referenced, and remove the first one
// that isn't. Caller must hold c.mu; c.limit must be > 0 and the ring must be
// non-empty.
func (c *SingleCoreCache[K, V]) evict() {
	e := c.hand
	for e.referenced.Load() {
		e.referenced.Store(false)
		e = e.next
	}

	next := e.next
	if next == e {
		next = nil
	}
	unlinkRing(e)
	delete(c.clock, e.key)
	c.hand = next
	c.releaseRing(e)
}

// deleteRingEntry unlinks e (already removed from c.clock by the caller) from
// the ring, fixing up the hand if it pointed at e, and parks e for reuse.
// Caller must hold c.mu and c.limit must be > 0.
func (c *SingleCoreCache[K, V]) deleteRingEntry(e *scRingEntry[K, V]) {
	if c.hand == e {
		if e.next == e {
			c.hand = nil
		} else {
			c.hand = e.next
		}
	}
	unlinkRing(e)
	c.releaseRing(e)
}

// unlinkRing detaches e from its neighbors without touching any hand pointer —
// callers fix that up themselves, since the right replacement differs between
// an eviction sweep (which already knows where the hand goes next) and an
// explicit delete (which does not). A no-op when e is the ring's only entry;
// the caller must detect that and clear the hand itself.
func unlinkRing[K comparable, V any](e *scRingEntry[K, V]) {
	if e.next == e {
		return
	}
	e.prev.next = e.next
	e.next.prev = e.prev
}

// releaseRing zeroes e's key/value so a parked entry doesn't keep them alive
// past their natural lifetime, and parks it for the next brand-new key.
// Caller must hold c.mu.
func (c *SingleCoreCache[K, V]) releaseRing(e *scRingEntry[K, V]) {
	var zeroK K
	var zeroV V
	e.key = zeroK
	e.value = zeroV
	e.expiresAt = 0
	e.referenced.Store(false)
	e.prev, e.next = nil, nil
	c.freeRing = e
}

// Get returns the value stored for key, and whether it was found. An entry
// past its TTL is reported as a miss without being removed — see Purge for
// active reclamation. The clock is only read when the found entry actually
// carries a TTL, so looking up entries written without one costs nothing
// extra. On a bounded cache a hit also flips the entry's CLOCK referenced bit
// with a single atomic store, taken under the read lock so concurrent readers
// never block each other.
func (c *SingleCoreCache[K, V]) Get(key K) (V, bool) {
	if c.limit > 0 {
		return c.getBounded(key)
	}

	c.mu.RLock()
	e, ok := c.data[key]
	if !ok {
		c.mu.RUnlock()
		var zero V
		return zero, false
	}
	if e.expiresAt != 0 && time.Now().UnixNano() >= e.expiresAt {
		c.mu.RUnlock()
		var zero V
		return zero, false
	}
	v := e.value
	c.mu.RUnlock()
	return v, true
}

func (c *SingleCoreCache[K, V]) getBounded(key K) (V, bool) {
	c.mu.RLock()
	e, ok := c.clock[key]
	if !ok {
		c.mu.RUnlock()
		var zero V
		return zero, false
	}
	if e.expiresAt != 0 && time.Now().UnixNano() >= e.expiresAt {
		c.mu.RUnlock()
		var zero V
		return zero, false
	}
	e.referenced.Store(true)
	v := e.value
	c.mu.RUnlock()
	return v, true
}

// Delete removes key from the cache, if present. Deleting a key that isn't
// present is a no-op.
func (c *SingleCoreCache[K, V]) Delete(key K) {
	c.mu.Lock()
	c.deleteLocked(key)
	c.mu.Unlock()
}

// DeleteMany removes 1..N keys, taking the lock once. Keys not present are
// silently skipped. As with SetMany there is no shard grouping to do.
func (c *SingleCoreCache[K, V]) DeleteMany(keys []K) {
	if len(keys) == 0 {
		return
	}
	c.mu.Lock()
	for _, k := range keys {
		c.deleteLocked(k)
	}
	c.mu.Unlock()
}

// deleteLocked removes key and parks its entry for reuse. Caller must hold
// c.mu (write lock).
func (c *SingleCoreCache[K, V]) deleteLocked(key K) {
	if c.limit > 0 {
		if e, ok := c.clock[key]; ok {
			delete(c.clock, key)
			c.deleteRingEntry(e)
		}
		return
	}
	if e, ok := c.data[key]; ok {
		delete(c.data, key)
		var zero V
		e.value = zero
		e.expiresAt = 0
		c.freeData = e
	}
}

// Clear removes every entry. The underlying map's storage is retained (via
// the builtin clear) rather than replaced, so refilling the cache to roughly
// its previous size doesn't pay for map growth again. Any CLOCK ring is
// discarded along with it — its entries only reference each other afterwards,
// which Go's tracing GC collects normally.
func (c *SingleCoreCache[K, V]) Clear() {
	c.mu.Lock()
	if c.limit > 0 {
		clear(c.clock)
		c.hand = nil
		c.freeRing = nil
	} else {
		clear(c.data)
		c.freeData = nil
	}
	c.mu.Unlock()
}

// Len returns the number of items physically stored, including entries whose
// TTL has passed but which Purge hasn't reclaimed yet — same contract as
// Cache.Len. Unlike Cache.Len, which is O(shard count), this is O(1).
func (c *SingleCoreCache[K, V]) Len() int {
	c.mu.RLock()
	n := len(c.data) + len(c.clock)
	c.mu.RUnlock()
	return n
}

// Purge actively removes all expired entries and returns how many were
// removed. Nothing calls this automatically — see the type doc comment.
func (c *SingleCoreCache[K, V]) Purge() int {
	now := time.Now().UnixNano()
	removed := 0

	c.mu.Lock()
	if c.limit > 0 {
		for k, e := range c.clock {
			if e.expiresAt != 0 && now >= e.expiresAt {
				delete(c.clock, k)
				c.deleteRingEntry(e)
				removed++
			}
		}
	} else {
		for k, e := range c.data {
			if e.expiresAt != 0 && now >= e.expiresAt {
				delete(c.data, k)
				removed++
			}
		}
	}
	c.mu.Unlock()

	return removed
}
