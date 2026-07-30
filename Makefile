.PHONY: build test test-race bench bench-compare vet fmt fmt-check lint tidy charts ci clean

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
