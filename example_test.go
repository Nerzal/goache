package goache_test

// Runnable examples for the public API. They double as tests: `go test` runs
// each one and compares stdout against its Output comment, so an API change
// that breaks a documented usage pattern fails CI rather than rotting on
// pkg.go.dev.
//
// Everything printed here is deterministic on purpose. Map iteration order is
// not, so no example ranges over a cache's contents — they read known keys and
// print counts instead.

import (
	"fmt"
	"runtime"
	"time"

	"github.com/Nerzal/goache"
)

// The common case: a sharded, goroutine-safe cache with no configuration.
func ExampleNew() {
	cache := goache.New[string, int]()

	cache.Set("answer", 42)

	value, ok := cache.Get("answer")
	fmt.Println(value, ok)

	_, ok = cache.Get("missing")
	fmt.Println(ok)

	// Output:
	// 42 true
	// false
}

// Any comparable type works as a key, including structs. Keys are hashed with
// hash/maphash.Comparable, so there is no reflection and no per-key allocation.
func ExampleNew_structKey() {
	type coord struct{ X, Y int }

	cache := goache.New[coord, string]()
	cache.Set(coord{1, 2}, "origin-ish")

	value, ok := cache.Get(coord{1, 2})
	fmt.Println(value, ok)

	// Output: origin-ish true
}

// WithCapacity pre-sizes every shard map when the final size is known upfront,
// which skips most of Go's incremental map growth during a bulk load. The hint
// is ignored if n <= 0.
func ExampleNew_withCapacity() {
	cache := goache.New[int, int](goache.WithCapacity(10_000))

	for i := range 10_000 {
		cache.Set(i, i*i)
	}

	fmt.Println(cache.Len())

	// Output: 10000
}

// WithMaxSize bounds the cache. Once the limit is reached, each insert evicts
// one entry using CLOCK (second-chance), so Len stops at the limit instead of
// growing without end.
func ExampleNew_withMaxSize() {
	cache := goache.New[int, int](goache.WithMaxSize(100))

	for i := range 1_000 {
		cache.Set(i, i)
	}

	fmt.Println(cache.Len() <= 100)

	// Output: true
}

// WithShardCount overrides the default of 256 shards. The count is rounded up
// to a power of two so shard selection stays a bitmask rather than a modulo.
func ExampleNew_withShardCount() {
	// 100 rounds up to 128.
	cache := goache.New[string, int](goache.WithShardCount(100))

	cache.Set("k", 1)
	fmt.Println(cache.Len())

	// Output: 1
}

// SetMany groups entries by destination shard before locking, so each shard
// mutex is acquired at most once per call regardless of batch size. Entries may
// mix expiring and non-expiring values; a zero TTL means no expiry.
func ExampleCache_SetMany() {
	cache := goache.New[string, string]()

	cache.SetMany([]goache.Entry[string, string]{
		{Key: "permanent", Value: "stays"},
		{Key: "temporary", Value: "goes", TTL: 10 * time.Millisecond},
	})

	fmt.Println(cache.Len())

	time.Sleep(50 * time.Millisecond)

	_, ok := cache.Get("permanent")
	fmt.Println("permanent:", ok)

	_, ok = cache.Get("temporary")
	fmt.Println("temporary:", ok)

	// Output:
	// 2
	// permanent: true
	// temporary: false
}

// An entry past its TTL reads as a miss immediately, but its memory is not
// reclaimed until Purge runs — goache starts no background goroutine, timer or
// ticker. Len reports what is physically stored, so it still counts the expired
// entry until then.
func ExampleCache_Purge() {
	cache := goache.New[string, int]()

	cache.Set("keep", 1)
	cache.SetWithTTL("expire", 2, 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	_, ok := cache.Get("expire")
	fmt.Println("readable:", ok)
	fmt.Println("stored:", cache.Len())

	fmt.Println("purged:", cache.Purge())
	fmt.Println("stored:", cache.Len())

	// Output:
	// readable: false
	// stored: 2
	// purged: 1
	// stored: 1
}

// DeleteMany skips keys that are not present, so callers need not check first.
func ExampleCache_DeleteMany() {
	cache := goache.New[string, int]()
	cache.SetMany([]goache.Entry[string, int]{
		{Key: "a", Value: 1},
		{Key: "b", Value: 2},
		{Key: "c", Value: 3},
	})

	cache.DeleteMany([]string{"a", "c", "never-existed"})

	fmt.Println(cache.Len())

	_, ok := cache.Get("b")
	fmt.Println(ok)

	// Output:
	// 1
	// true
}

// NewSingleCore is a second, independent implementation for processes pinned to
// one core — a Kubernetes pod with limits.cpu of 1000m or less runs at
// GOMAXPROCS=1 under Go 1.25+. It drops the shard routing hash and the shard
// slice indirection for one map behind one mutex, and has the same API as
// Cache. Above one core it loses to Cache and is the wrong choice.
func ExampleNewSingleCore() {
	cache := goache.NewSingleCore[string, int]()

	cache.Set("answer", 42)
	cache.SetWithTTL("session", 7, time.Hour)

	value, ok := cache.Get("answer")
	fmt.Println(value, ok)
	fmt.Println(cache.Len())

	// Output:
	// 42 true
	// 2
}

// Picking the implementation from the process's actual core count requires one
// variable that can hold either type, which is what Cacher is for. Its dynamic
// dispatch costs roughly 2 ns per call (+8.8% on a single-core Get), so prefer
// a concrete type whenever the choice can be made at compile time.
func ExampleCacher() {
	var cache goache.Cacher[string, int]

	if runtime.GOMAXPROCS(0) == 1 {
		cache = goache.NewSingleCore[string, int]()
	} else {
		cache = goache.New[string, int]()
	}

	cache.Set("answer", 42)

	value, ok := cache.Get("answer")
	fmt.Println(value, ok)

	// Output: 42 true
}
