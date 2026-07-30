package goache

import (
	"strconv"
	"testing"
)

func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

func BenchmarkSet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%n], i)
		i++
	}
}

func BenchmarkSetMany(b *testing.B) {
	const batch = 100
	b.ReportAllocs()
	for i := 0; i < b.N; i += batch {
		n := batch
		if i+n > b.N {
			n = b.N - i
		}
		c := New[string, int]()
		entries := make([]Entry[string, int], n)
		for j := 0; j < n; j++ {
			entries[j] = Entry[string, int]{Key: strconv.Itoa(i + j), Value: i + j}
		}
		b.StartTimer()
		c.SetMany(entries)
		b.StopTimer()
	}
}

func BenchmarkGet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get(keys[i%n])
		i++
	}
}

func BenchmarkGetMiss(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	for i, k := range benchKeys(n) {
		c.Set(k, i)
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Get("missing-" + strconv.Itoa(i))
		i++
	}
}

func BenchmarkParallelGetSet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%n]
			if i%10 == 0 {
				c.Set(k, i)
			} else {
				c.Get(k)
			}
			i++
		}
	})
}

func BenchmarkParallelGet(b *testing.B) {
	c := New[string, int]()
	const n = 100000
	keys := benchKeys(n)
	for i, k := range keys {
		c.Set(k, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(keys[i%n])
			i++
		}
	})
}
