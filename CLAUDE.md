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
- **DEC's ciphers stay in-tree**: `twofish.go`, `square.go`, `rijndael.go` and `tdes.go` are not stylistic preferences over `golang.org/x/crypto` or `crypto/aes`. ABSCipher is a fork of DEC 3.0, and three of its ciphers deviate from the published algorithms, so a correct implementation cannot read these files:
  - **Twofish** — DEC's key schedule has a `shr`/`shl` typo, so `golang.org/x/crypto/twofish` is incompatible.
  - **Rijndael-256** — DEC's key schedule matches AES for 128- and 192-bit keys but diverges for 256-bit ones, so `crypto/aes` is correct for `Rijndael_128` and wrong for `Rijndael_256`.
  - **DES-Triple** — this is DEC's `TCipher_3TDES`, a **24-byte** block, and its word swap contains a typo that must be reproduced.
  - **Square** — no deviation, but no Go implementation exists anywhere, DEC-specific or otherwise.

  Each is pinned by a `TestDEC*SelfTest` against DEC's own self-test vector and by a real fixture. **Do not "fix" any of these deviations** — every affected file would stop decrypting. The deviations were found by real fixtures, not by reading; two of them shipped as silently wrong code for months.

- **One table is a special case, not the shape of the API**: a database can hold several tables, and only data pages record which one they belong to. Reads and writes are scoped through `File.Table(name)`; the no-argument `Schema`, `OpenTable` and `OpenTableWriter` are shorthand for `Table("")` and report `ErrAmbiguousTable` rather than mixing tables. Before this existed, `OpenTable` on a three-table file returned six rows for a two-row table with no error. When adding anything that scans pages by type, ask what happens with three tables in the file — the answer is usually "it silently reads someone else's".

- **Read-only by default**: Phases 1–6 (header, schema, records, BLOBs, indexes, encryption) are read-only, and `Open` still returns a read-only handle. Writing needs `OpenForWrite` explicitly, and every write path checks that flag. Phase 7 (`writer.go`) adds record insert, update and delete within existing pages; Phase 8 adds the schema operations — `DROP TABLE` and the allocation model in `ddl.go`, `CREATE TABLE` in `ddl_create.go`, `CREATE INDEX`/`DROP INDEX` in `ddl_index.go`, and `ALTER TABLE ADD`/`DROP COLUMN` in `ddl_alter.go`. Database compaction is the one Phase 8 step still open.

- **The engine's zlib is the C library at level 1, and `internal/zlib1` reproduces it**: every compressed internal file in the corpus — schema (type 8), table info (9), catalog (6) — is byte-identical to `zlib.compress(data, 1)`, all 48 of them, and to no other level. Go's `compress/zlib` matches none of them at any level, because its level 1 is its own fast encoder rather than zlib's `deflate_fast`. This blocked every schema operation except `DROP TABLE` until `internal/zlib1` ported zlib's level-1 path; `TestZlib1ReproducesEveryCorpusStream` re-compresses all 48 and requires each to reproduce the engine's bytes, and `testdata/zlib1` holds 37 golden vectors that pin it without any fixture. **Never write a stream the engine will read with `compress/zlib`** — it is for reading only. A writer using it produces a file that reads back correctly and is not what the engine wrote, which is exactly the failure the byte-identity tests exist to catch.

- **Writes are judged byte-for-byte**: `TestWriterMatchesEngineByteForByte`,
  `TestDropTableMatchesEngineByteForByte`, `TestCreateTableMatchesEngineByteForByte` and
  `TestCreateIndexMatchesEngineByteForByte` require each operation to reproduce the file DBManager
  itself produced for the same SQL statement. Reading a write back correctly is not sufficient
  evidence — the engine keeps counters a naive writer would miss, and this package's own reader would
  not notice. If you change the write path, those are the tests that matter.

  Two operations cannot meet that bar, for reasons recorded rather than waved away. `ALTER TABLE ADD`
  and `DROP COLUMN` have **no engine fixture at all** — none was ever produced — so they rest on
  round-trip plus the independent B-tree leaf oracle, and `TestAlterTableMatchesEngineByteForByte`
  skips until `MultiTable-alteradd.abs`/`-alterdrop.abs` exist. And every comparison excludes page
  `State` words, because of the next point.

- **A page's `State` is seeded randomly, so allocating a page breaks byte identity**: across the
  corpus's 663 live pages `State` is uniform in `[0, 2^30)`, and 29 groups of byte-identical page
  payloads carry different `State`s — it is neither a content hash nor a fixed sequence. This is why
  `DROP TABLE` and the record writer reach full byte identity (they only touch pages that already
  exist) while `CREATE TABLE` and `CREATE INDEX` cannot. The two operations also differ from each
  other: `CREATE TABLE` increments existing pages' `State` and reseeds only the five it allocates,
  whereas `CREATE INDEX` reseeds **every** page it rewrites — pages whose payload is byte-identical
  before and after still come back with a new `State`. Exclude `State` words explicitly and narrowly;
  never widen an exclusion to make a test pass.
- **No panics**: All error paths return errors. Never panic on malformed input.
- **Fuzz-safe**: The parser must handle arbitrary byte sequences without crashes or unbounded allocations.
- **Test against real files**: Primary validation uses real `.abs` fixtures in `testdata/`. That directory is gitignored — almost all of the files are real customer project data and are never committed. The exceptions are the eight `testdata/Employees-*.abs` fixtures (one per encryption algorithm), the eleven `testdata/Writes*.abs` fixtures (the write path's ground truth) and the six `testdata/MultiTable*.abs` fixtures (the table catalog and the schema operations over it), which are ours and are committed; see `testdata/README.md`. Tests that need a fixture must `t.Skip` when it is missing, so a fresh clone (and CI) still runs green on the synthetic, unit, `Employees-*` and `Writes*` tests alone. A green CI run therefore still does **not** mean the parser was validated against the customer files; run `just test` locally for that.
- **Windows-1252 aware**: String fields use Windows-1252 encoding by default. Always decode to UTF-8.

## Formatting and Linting

`gofumpt` (a strict superset of `gofmt`) plus `gci` for import grouping. This is configured in two places and they must agree:

- `treefmt.toml` — runs `gofumpt` and `gci` in write mode, plus `prettier` for Markdown/YAML/JSON and `shfmt`/`shellcheck` for shell.
- `.golangci.yml` — enables `gofumpt` and `gci` under `formatters:`.

Use `just fmt` to format and `just check-formatted` to verify. `check-formatted` formats a scratch copy of the tree, so it never rewrites your files.

Linting is `golangci-lint` with `default: all` and a curated disable list. Every entry in `.golangci.yml` — disabled linter or exclusion rule — carries a comment explaining why; keep that up if you add one.

## CI

`.github/workflows/ci.yml` runs build, `go vet`, `gofmt`, `go mod tidy -diff`, race tests, `golangci-lint`, and a short fuzz budget per target. It sees only the two committed Twofish fixtures, not the customer ones, so read the scope note at the top of that file before trusting a green check.
