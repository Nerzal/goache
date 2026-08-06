.PHONY: build test test-race bench bench-cpu bench-singlecore bench-compare bench-compare-cpu bench-compare-singlecore bench-compare-singlecore-sizes vet fmt fmt-check lint tidy charts ci clean

build:
	go build ./...

test:
	go test ./...

# Requires cgo (a C compiler on PATH). See CLAUDE.md if this fails with
# "requires cgo; enable cgo by setting CGO_ENABLED=1".
test-race:
	go test -race ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./...

# CPU-constrained sweep: how goache behaves when it does not get 24 cores.
# A Kubernetes pod with `limits.cpu: 100m` runs with GOMAXPROCS=1 under
# Go 1.25+, which is the leftmost column. See docs/adr/0025.
bench-cpu:
	go test -bench='Parallel' -benchmem -run=^$$ -cpu=1,2,4,8,24 ./...

# The same sweep across every compared library, at a fixed 100k working set.
bench-compare-cpu:
	cd bench && go test -bench='ParallelGet/n=100000$$' -benchmem -run=^$$ -cpu=1,2,4,8,24 ./...

# SingleCoreCache against its sharded Cache counterparts, at the one core it
# exists for. Every BenchmarkSingleCore* has a same-named Cache benchmark;
# the pair is only meaningful read together. See docs/adr/0026.
bench-singlecore:
	go test -bench='SingleCore|Cacher' -benchmem -run=^$$ -cpu=1 ./...

# The same claim checked against the whole competitor field at one core, at a
# 100k working set: every library, every category it has an equivalent for.
# Deliberately not filtered down to goache vs go-cache — "fastest single-core
# cache" is a claim about the field, and ristretto/theine/otter are the ones
# that could refute it. See docs/adr/0027.
bench-compare-singlecore:
	cd bench && go test -bench='.*/n=100000$$' -benchmem -run=^$$ -cpu=1 ./...

# The same field swept across every working-set size, to catch a competitor
# that only wins at one end of the curve (freecache keeps entries off the Go
# heap, which is a bet that pays at large n or not at all).
bench-compare-singlecore-sizes:
	cd bench && go test -bench=. -benchmem -run=^$$ -cpu=1 ./...

bench-compare:
	cd bench && go test -bench=. -benchmem -run=^$$ ./...

vet:
	go vet ./...
	cd bench && go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

lint:
	golangci-lint run ./...
	cd bench && golangci-lint run ./...

tidy:
	go mod tidy
	cd bench && go mod tidy

# Regenerates the benchmark bar charts embedded in README.md from the
# numbers documented there. See docs/benchcharts/.
charts:
	go run ./docs/benchcharts

ci: fmt-check vet lint test test-race

clean:
	go clean ./...
	cd bench && go clean ./...
