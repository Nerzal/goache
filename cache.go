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
package goache

import (
	"hash/maphash"
	"sync"
)

// defaultShardCount is the number of shards used when no shard count is
// configured. It must be a power of two so shard selection can use a bitmask
// instead of a modulo.
const defaultShardCount = 256

// entry is the value stored per key inside a shard. It currently holds only
// the cached value. Phase 2 (TTL/eviction) can add fields here — e.g. an
// expiry timestamp or access metadata — without changing the public Cache
// API, since callers never see entry directly.
type entry[V any] struct {
	value V
}

// shard is one independently-locked partition of the cache.
type shard[K comparable, V any] struct {
	mu   sync.RWMutex
	data map[K]entry[V]
}

// Entry is a key/value pair used for bulk operations.
type Entry[K comparable, V any] struct {
	Key   K
	Value V
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

// Set adds or overwrites a single key/value pair.
func (c *Cache[K, V]) Set(key K, value V) {
	s := c.shardFor(key)
	s.mu.Lock()
	s.data[key] = entry[V]{value: value}
	s.mu.Unlock()
}

// SetMany adds or overwrites 1..N key/value pairs. Entries are grouped by
// destination shard up front so each shard's lock is acquired at most once,
// instead of once per entry.
func (c *Cache[K, V]) SetMany(entries []Entry[K, V]) {
	if len(entries) == 0 {
		return
	}

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
			s.data[e.Key] = entry[V]{value: e.Value}
		}
		s.mu.Unlock()
	}
}

// Get returns the value stored for key, and whether it was found.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	s := c.shardFor(key)
	s.mu.RLock()
	e, ok := s.data[key]
	s.mu.RUnlock()
	return e.value, ok
}

// Len returns the total number of items currently stored in the cache.
func (c *Cache[K, V]) Len() int {
	total := 0
	for _, s := range c.shards {
		s.mu.RLock()
		total += len(s.data)
		s.mu.RUnlock()
	}
	return total
}
