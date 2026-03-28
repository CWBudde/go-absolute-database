# CLAUDE.md

## Project Overview

`go-absolute-database` is a Go library for reading (and later writing) ComponentAce Absolute Database `.abs` files. The binary format is reverse-engineered — there is no public specification.

## Commands

```bash
go test ./...                    # Run all tests
go test -v ./...                 # Verbose test output
go test -race ./...              # Race detector
go test -fuzz FuzzName ./...     # Fuzz testing
```

## Architecture

- Single Go module: `github.com/cwbudde/go-absolute-database`
- No external dependencies for core read path (stdlib only)
- Page-based I/O: all file access goes through page reads
- Lazy loading: only read pages when accessed

## Key Policies

- **Read-only first**: Phases 1–3 focus entirely on reading. No write code until Phase 7.
- **No panics**: All error paths return errors. Never panic on malformed input.
- **Fuzz-safe**: The parser must handle arbitrary byte sequences without crashes or unbounded allocations.
- **Test against real files**: Primary validation uses real SoundPlan `.abs` files from `../Aconiq/interoperability/`.
- **Windows-1252 aware**: String fields use Windows-1252 encoding by default. Always decode to UTF-8.

## Formatting and Linting

Use standard `gofmt`. No special formatter config.
