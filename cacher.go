package goache

import "time"

// Cacher is the operation set shared by Cache and SingleCoreCache. It exists
// for callers that must pick between the two at run time — typically from
// runtime.GOMAXPROCS(0) at startup — and therefore need one variable that can
// hold either.
//
// Using it is opt-in and not free. Calls through an interface are indirect
// and cannot be inlined, which measured roughly 2 ns per operation on this
// package's benchmarks: about +8% on a single-core Get (24.4 to 26.3 ns/op)
// and +2-5% on Cache's concurrent Get at 24 cores (4.52 to 4.71 ns/op). That
// is exactly why New and NewSingleCore return their concrete types instead of
// this interface — nobody pays the dispatch unless they ask for it.
//
// If the choice can be made at compile time, or if the two branches can each
// keep their own concrete variable, prefer that and skip this entirely.
type Cacher[K comparable, V any] interface {
	// Set adds or overwrites a single key/value pair with no expiry.
	Set(key K, value V)
	// SetWithTTL adds or overwrites a single key/value pair that expires
	// after ttl. ttl <= 0 means no expiry.
	SetWithTTL(key K, value V, ttl time.Duration)
	// SetMany adds or overwrites 1..N pairs, each optionally expiring per its
	// own Entry.TTL.
	SetMany(entries []Entry[K, V])
	// Get returns the value stored for key and whether it was found. An entry
	// past its TTL reports as a miss.
	Get(key K) (V, bool)
	// Delete removes key if present.
	Delete(key K)
	// DeleteMany removes 1..N keys, skipping any that aren't present.
	DeleteMany(keys []K)
	// Clear removes every entry.
	Clear()
	// Len returns the number of items physically stored, including expired
	// entries not yet reclaimed by Purge.
	Len() int
	// Purge removes all expired entries and returns how many were removed.
	Purge() int
}

var (
	_ Cacher[string, int] = (*Cache[string, int])(nil)
	_ Cacher[string, int] = (*SingleCoreCache[string, int])(nil)
)
