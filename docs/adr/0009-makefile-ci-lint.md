# 0009: Makefile, GitHub Actions CI, golangci-lint

## Status

Accepted

## Context

Up to this point, build/test/vet/bench/race commands were run ad hoc and
documented only in CLAUDE.md prose. Needed a single, discoverable entry
point for common commands, automated CI enforcement on push/PR, and a
configured linter beyond `go vet`.

## Decision

- **Makefile** at the repo root: `build`, `test`, `test-race`, `bench`,
  `bench-compare` (delegates into `bench/`), `vet` (both modules), `fmt`,
  `fmt-check`, `lint` (both modules), `tidy` (both modules), `charts`
  (regenerates README bar charts, see
  [0010](0010-svg-bar-charts-in-readme.md)), `ci` (the full gate:
  `fmt-check vet lint test test-race`), `clean`.
- **`.golangci.yml`** (schema v2, matching the installed `golangci-lint`
  v2.12.1): `linters.default: standard` plus `prealloc`, `unconvert`,
  `misspell`, `gocritic`; `formatters`: `gofmt`, `goimports`. Applies to
  both the root module and `bench/` (golangci-lint finds the config by
  searching upward from the working directory, so `bench/` picks up the
  same root `.golangci.yml` without its own copy).
- **`.github/workflows/ci.yml`**: on push to `main` and on every PR —
  `test` job (build, vet, gofmt check, `go test -race` — GitHub's
  `ubuntu-latest` runners ship gcc, so cgo/`-race` works there even though
  it required a manual gcc install on this Windows dev machine), `lint`
  job (golangci-lint-action for both modules), and a `benchmark` job
  (`continue-on-error: true` — informational only, not a merge gate, since
  CI runner performance is noisy and not representative of the numbers
  documented in README.md).

## Consequences

- `make ci` reproduces exactly what CI enforces, runnable locally before
  pushing.
- CI's `-race` job is the first *automated* race verification for this
  project — locally it depends on gcc being installed (see CLAUDE.md), but
  CI always has it.
- The benchmark job is explicitly non-blocking: CI runner numbers will
  differ from the machine numbers in README.md, and are not meant to
  replace them — see [0010](0010-svg-bar-charts-in-readme.md) and the
  Performance Policy in CLAUDE.md for how README's authoritative numbers
  are maintained.
