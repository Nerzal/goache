module github.com/Nerzal/goache/bench

go 1.26.2

replace github.com/Nerzal/goache => ../

require (
	github.com/Nerzal/goache v0.0.0
	github.com/coocood/freecache v1.2.7
	github.com/dgraph-io/ristretto/v2 v2.4.2
	github.com/patrickmn/go-cache v2.1.0+incompatible
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	golang.org/x/sys v0.36.0 // indirect
)
