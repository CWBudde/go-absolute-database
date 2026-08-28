# CLAUDE.md

## Project Overview

`go-absolute-database` is a Go library for reading (and later writing) ComponentAce Absolute Database `.abs` files. The binary format is reverse-engineered — there is no public specification.

## Commands

```bash
just                             # list all recipes
just build                       # go build ./...
just test                        # go test ./...
just test-race                   # race detector
just lint                        # golangci-lint
just check-formatted             # formatting check (read-only, never rewrites)
just fmt                         # apply formatting (rewrites files)
just fuzz                        # 30s per discovered fuzz target
just ci                          # everything CI runs, plus the parts CI cannot
```

Raw equivalents: `go test ./...`, `go test -race ./...`, `go test -run '^$' -fuzz FuzzName -fuzztime=30s .`

## Architecture

- Single Go module: `github.com/cwbudde/go-absolute-database`
- Library lives in the repo root (package `absdb`); the `absdb` CLI is in `cmd/absdb/`
- Page-based I/O: all file access goes through page reads
- Lazy loading: only read pages when accessed

## Key Policies

- **Minimal dependencies**: the core read path uses the standard library plus two `golang.org/x` packages, which are maintained by the Go team and are effectively extended stdlib: `golang.org/x/text/encoding/charmap` for Windows-1252 decoding (`reader.go`) and `golang.org/x/crypto/blowfish` for the Blowfish cipher used by encrypted files (`crypto.go`). `github.com/spf13/cobra` is a CLI-only dependency and must not be imported by the library. Anything beyond that needs a reason.
- **Twofish stays in-tree**: `twofish.go` is not a stylistic preference over `golang.org/x/crypto/twofish`. ABSCipher's Twofish is DEC 3.0's, whose key schedule has a `shr`/`shl` typo that makes it incompatible with reference Twofish, so the x/crypto package cannot read these files. `TestDECTwofishSelfTest` pins this against DEC's own self-test vector — do not "fix" the deviation.
- **Read-only first**: Phases 1–6 (header, schema, records, BLOBs, indexes, encryption) are read-only. No write code until Phase 7.
- **No panics**: All error paths return errors. Never panic on malformed input.
- **Fuzz-safe**: The parser must handle arbitrary byte sequences without crashes or unbounded allocations.
- **Test against real files**: Primary validation uses real `.abs` fixtures in `testdata/`. That directory is gitignored — the files are real customer project data and are never committed. Tests that need a fixture must `t.Skip` when it is missing, so a fresh clone (and CI) still runs green on the synthetic and unit tests alone. A green CI run therefore does **not** mean the parser was validated against real files; run `just test` locally for that.
- **Windows-1252 aware**: String fields use Windows-1252 encoding by default. Always decode to UTF-8.

## Formatting and Linting

`gofumpt` (a strict superset of `gofmt`) plus `gci` for import grouping. This is configured in two places and they must agree:

- `treefmt.toml` — runs `gofumpt` and `gci` in write mode, plus `prettier` for Markdown/YAML/JSON and `shfmt`/`shellcheck` for shell.
- `.golangci.yml` — enables `gofumpt` and `gci` under `formatters:`.

Use `just fmt` to format and `just check-formatted` to verify. `check-formatted` formats a scratch copy of the tree, so it never rewrites your files.

Linting is `golangci-lint` with `default: all` and a curated disable list. Every entry in `.golangci.yml` — disabled linter or exclusion rule — carries a comment explaining why; keep that up if you add one.

## CI

`.github/workflows/ci.yml` runs build, `go vet`, `gofmt`, `go mod tidy -diff`, race tests, `golangci-lint`, and a short fuzz budget per target. It cannot see `testdata/`, so read the scope note at the top of that file before trusting a green check.
