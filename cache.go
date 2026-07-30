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
package goache

import (
	"hash/maphash"
	"sync"
	"time"
)

// defaultShardCount is the number of shards used when no shard count is
// configured. It must be a power of two so shard selection can use a bitmask
// instead of a modulo.
const defaultShardCount = 256

// entry is the value stored per key inside a shard.
//
// expiresAt is an absolute deadline in UnixNano, or 0 for "never expires".
// 0 is a safe sentinel: it's Set's zero value (entries created via Set/
// SetMany without a TTL never pay any expiry-check cost beyond one int64
// comparison against 0 in Get), and no real deadline is ever exactly the
// Unix epoch. Storing an absolute int64 deadline (rather than e.g. a
// *time.Timer per entry) keeps entry the same size as before plus 8 bytes,
// with no allocation and no per-entry background work — expiry is checked
// lazily on Get, and reclaimed either lazily (an expired entry is simply
// never returned) or actively via Purge.
type entry[V any] struct {
	value     V
	expiresAt int64
}

// expired reports whether the entry's TTL (if any) has passed as of now
// (UnixNano). An entry with expiresAt == 0 never expires.
func (e *entry[V]) expired(now int64) bool {
	return e.expiresAt != 0 && now >= e.expiresAt
}

// shard is one independently-locked partition of the cache.
type shard[K comparable, V any] struct {
	mu   sync.RWMutex
	data map[K]entry[V]
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
	// shards is a pointer slice: each shard is its own heap allocation.
	// A tried alternative — one contiguous []shard value slice — measured
	// ~9% slower under concurrent access (see cache_bench_test.go /
	// BenchmarkParallelGet history): packing every shard's sync.RWMutex
	// back-to-back in one array puts adjacent shards' mutexes on shared
	// cache lines, so unrelated shards contend over cache-line ownership
	// (false sharing) even though they never contend over a lock. Separate
	// heap objects avoid that at the cost of one extra pointer dereference
	// per access, which is the cheaper trade-off here.
	shards []*shard[K, V]
	mask   uint64
	seed   maphash.Seed
}

// Option configures a Cache created with New.
type Option func(*config)

type config struct {
	shardCount int
	capacity   int
}

// WithShardCount sets the number of shards the cache is split into. The
// value is rounded up to the next power of two. More shards reduce lock
// contention under high concurrency at the cost of a small amount of
// per-shard memory overhead. The default is 256.
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

	perShard := 0
	if cfg.capacity > 0 {
		perShard = (cfg.capacity + n - 1) / n
	}

	shards := make([]*shard[K, V], n)
	for i := range shards {
		shards[i] = &shard[K, V]{data: make(map[K]entry[V], perShard)}
	}

	return &Cache[K, V]{
		shards: shards,
		mask:   uint64(n - 1),
		seed:   maphash.MakeSeed(),
	}
}

func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func (c *Cache[K, V]) shardFor(key K) *shard[K, V] {
	h := maphash.Comparable(c.seed, key)
	return c.shards[h&c.mask]
}

// Set adds or overwrites a single key/value pair with no expiry. Identical
// cost to before TTL support existed — it never touches the clock.
func (c *Cache[K, V]) Set(key K, value V) {
	s := c.shardFor(key)
	s.mu.Lock()
	s.data[key] = entry[V]{value: value}
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
	s.data[key] = entry[V]{value: value, expiresAt: expiresAt}
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
		s := c.shards[i]
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
			s.data[e.Key] = entry[V]{value: e.Value, expiresAt: expiresAt}
		}
		s.mu.Unlock()
	}
}

// Get returns the value stored for key, and whether it was found. An entry
// past its TTL is treated as a miss without being removed from the shard —
// see Purge for active reclamation. The clock is only read when the found
// entry actually has a TTL (expiresAt != 0), so looking up entries created
// via Set/SetMany without a TTL costs exactly what it did before TTL
// support existed.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	s := c.shardFor(key)
	s.mu.RLock()
	e, ok := s.data[key]
	s.mu.RUnlock()
	if !ok {
		return e.value, false
	}
	if e.expiresAt != 0 && time.Now().UnixNano() >= e.expiresAt {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Len returns the number of items physically stored in the cache. This
// includes entries whose TTL has passed but haven't been reclaimed yet by
// Purge — Len is O(shard count), not O(item count), specifically so it
// stays cheap regardless of cache size; Get is the source of truth for
// whether any individual key is still alive. Call Purge first if you need
// an exact live count.
func (c *Cache[K, V]) Len() int {
	total := 0
	for _, s := range c.shards {
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
	for _, s := range c.shards {
		s.mu.Lock()
		for k, e := range s.data {
			if e.expired(now) {
				delete(s.data, k)
				removed++
			}
		}
		s.mu.Unlock()
	}
	return removed
}
