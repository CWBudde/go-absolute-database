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

## Documentation

- `docs/` — the format reference and the write-path guide. Start at `docs/README.md`.
  **Format findings belong there**, not in `PLAN.md` and not only in a code comment.
- `PLAN.md` — what is built and what comes next. No format knowledge.
- `testdata/README.md` — the fixtures and how to generate more under Wine.
- `NOTICE` — third-party attributions (zlib, DEC) and the ComponentAce trademark disclaimer.
  `docs/provenance.md` says what each one is for; keep both current when adding a dependency or
  a ported algorithm. The ComponentAce SDK lives **outside** this repository, referred to as
  `<sdk>` (by convention `../absolute-database-sdk/`), and must never be committed.

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

- **Read-only by default**: Phases 1–6 (header, schema, records, BLOBs, indexes, encryption) are read-only, and `Open` still returns a read-only handle. Writing needs `OpenForWrite` explicitly, and every write path checks that flag. Phase 7 (`writer.go`) adds record insert, update and delete, growing a table by a free page when every existing one is full, keeping the table's user indexes in step (`writer_index.go`) and checking the constraints a table declares (`writer_constraint.go`); Phase 8 adds the schema operations — `DROP TABLE` and the allocation model in `ddl.go`, `CREATE TABLE` in `ddl_create.go`, `CREATE INDEX`/`DROP INDEX` in `ddl_index.go`, `ALTER TABLE ADD`/`DROP COLUMN` in `ddl_alter.go`, and the constraint-record serializer `CREATE TABLE` and compaction write through in `ddl_constraint_write.go`. Database growth — extending the file by whole extents when no free page remains — is in `ddl_grow.go`. `ddl_database.go` writes an `.abs` from nothing (`CreateDatabase`), and `ddl_compact.go` rebuilds one into a new file on top of it (`CompactDatabase`), which closes Phase 8.

- **The engine's zlib is the C library at level 1, and `internal/zlib1` reproduces it**: every compressed internal file in the corpus — schema (type 8), table info (9), catalog (6) — is byte-identical to `zlib.compress(data, 1)`, all 48 of them, and to no other level. Go's `compress/zlib` matches none of them at any level, because its level 1 is its own fast encoder rather than zlib's `deflate_fast`. This blocked every schema operation except `DROP TABLE` until `internal/zlib1` ported zlib's level-1 path; `TestZlib1ReproducesEveryCorpusStream` re-compresses all 48 and requires each to reproduce the engine's bytes, and `testdata/zlib1` holds 37 golden vectors that pin it without any fixture. **Never write a stream the engine will read with `compress/zlib`** — it is for reading only. A writer using it produces a file that reads back correctly and is not what the engine wrote, which is exactly the failure the byte-identity tests exist to catch.

- **Writes are judged byte-for-byte**: `TestWriterMatchesEngineByteForByte`,
  `TestDropTableMatchesEngineByteForByte`, `TestCreateTableMatchesEngineByteForByte`,
  `TestCreateIndexMatchesEngineByteForByte`, `TestGrowthMatchesEngineByteForByte`,
  `TestCreateDatabaseMatchesEngineByteForByte`, `TestCompactDatabaseMatchesEngineByteForByte`,
  `TestKeyIndexWritesMatchEngineByteForByte`,
  `TestCreateTableWithAPrimaryKeyMatchesEngineByteForByte`,
  `TestCreateUniqueIndexMatchesEngineByteForByte`,
  `TestAutoIncWritesMatchEngineByteForByte`,
  `TestCreateAutoIncTableMatchesEngineByteForByte` and
  `TestCompactAutoIncMatchesEngineByteForByte`
  require each operation to reproduce the file DBManager itself produced for the same SQL statement
  or menu action. That includes index maintenance: four of the `Writes-idx*` pairs, seven
  `Keys*` pairs and seven `Auto*` pairs pin the B-tree leaf splices an insert, a delete and a
  key-moving update perform on a plain, a key-enforcing and an `AUTOINC`-keyed index, with
  **no `State` exclusion**, because maintenance allocates no page. Reading a write back correctly is not sufficient
  evidence — the engine keeps counters a naive writer would miss, and this package's own reader would
  not notice. If you change the write path, those are the tests that matter.

  `TestCreateTableWritesTheEngineSchemaStream` is the other kind of oracle and worth knowing about:
  it replays `Constraints.abs`'s own `CREATE TABLE` statements into a database this package created
  and requires the schema streams to match **including the engine's object ids**, which is how the
  id order (table, columns, indexes, constraints) is checked without a whole-file comparison.

  Two operations cannot meet that bar, for reasons recorded rather than waved away. `ALTER TABLE ADD`
  and `DROP COLUMN` **diverge from the engine by design**, and the fixtures that prove it are
  committed: `MultiTable-alteradd.abs`/`-alterdrop.abs` show the engine implements `ALTER TABLE` as
  `CREATE TABLE <temp>` + copy rows + rename + `DROP TABLE` — four transactions, new object ids, six
  pages allocated and six freed. This package splices in place instead and is held to _semantic_
  identity against those same fixtures (`TestAlterTableMatchesEngineSemantically`), with
  `TestEngineAlterTableRebuildsTheTable` pinning the engine's strategy so the reasoning cannot go
  stale. **The reason for that choice has expired and the choice has not been revisited.** It was
  that matching the engine needs six free pages and nothing here could grow a database, so a faithful
  `ALTER` would have been byte-perfect on `MultiTable.abs` and refused on every other file. `ddl_grow.go`
  removed that constraint: the file now extends by whole extents on demand. So an engine-faithful
  rebuild is newly _possible_ — it is no longer blocked, merely not done, and it would still have to
  reproduce four transactions, three catalog writes and a fresh set of object ids to be worth doing.
  `CompactDatabase` since showed that last part is tractable: replaying object creation in the
  engine's order hands out the engine's ids. Treat it as an open question, not as a settled
  impossibility. And every byte comparison excludes
  page `State` words, because of the next point.

- **Compaction is a rebuild into a new file, not a defragment in place**: `TABSDatabase.CompactDatabase`
  routes to `InternalCopyDatabase`, the SDK help calls the result "a new compact copy of a database",
  and DBManager's menu handler closes the file, copies it and reopens. `MultiTable-dropcompact.abs`
  proves it from the bytes: 30 pages become 18 with none free, the file `State` is **reset** from 12
  to 6 and `LastObjectID` from 11 to 7, so `Gamma` comes back with a different object id. A file whose
  transaction counter has been reset and whose ids have been reallocated is a new file, not a squeezed
  one. That is why compaction needed `CreateDatabase` (`ddl_database.go`) first, and why it must never
  be implemented as an in-place free-space sweep — that would read back correctly through this package
  and would not be what the engine wrote.

  This entry used to add that byte identity was therefore unreachable "by construction", every object
  id being different. **That was wrong, and `TestCompactDatabaseMatchesEngineByteForByte` is the
  refutation**: `CompactDatabase` reproduces `MultiTable-dropcompact.abs` with only sixteen page
  `State` words excluded. The ids differ from the _source_ file but are deterministic in the _output_,
  because a replay that creates objects in the engine's order hands out the same ids. The lesson is
  narrower than the old claim: reallocation defeats byte identity only when the allocation order is
  not reproduced. `ALTER TABLE`'s fallback to semantic identity is a choice about effort, not a
  consequence of reallocation.

- **Index maintenance is deliberately narrow, and its narrowness is load-bearing**: insert, delete
  and a key-moving update keep a **root-only index over one or more `Int32` components** in step—
  `Integer` or `AUTOINC`, which are the same component byte for byte—and that is exactly the
  occupied shape `CREATE INDEX` builds. `MultiKeys*.abs` pins the compound concatenation,
  lexicographic component order and all three splice operations with full byte identity.
  `indexableKeyColumn` (`index.go`) is the single definition of the component rule;
  it had four copies before, and they have to agree, because a rebuild that builds an index the
  writer will not maintain produces a table nothing can insert into. Everything else is refused
  with `ErrIndexNotMaintained` rather than guessed at: a tree deep enough to have split, an
  occupied component of another shape, or ordering this package does not reproduce (`DESC`,
  `NOCASE`). The last private-corpus survey found sixteen refused tables and showed that an
  all-integer multi-column key was the sole reason for five of them, so this implementation
  predicts eleven remaining. That re-measurement is still pending because the private 111-table
  corpus is not in this checkout; `PLAN.md` keeps the measured old table and labels the projection.
  Three behaviours come from fixtures and must not be "tidied": a removal
  **leaves the entry slot it vacates untouched** (`Writes-idx-del.abs`), a key-moving update is a
  **removal followed by a sorted insertion** rather than an in-place patch (`Writes-idx-upd.abs`),
  and a `NULL` key **sorts before every value** (`Keys-uniqnull.abs`, which is the only file
  anywhere holding one).

- **An `AUTOINC` column's next value is stored, not derived, and a write must maintain it**: it
  is one `int64` per column in the **table info file**, in the array `buildTableInfoFile` writes
  as zeroes (`writer_autoinc.go`, `docs/format/internal-files.md`). An insert raises it to the
  value written when that value is larger; a delete and an update **leave it alone**, so it
  records what the engine has assigned rather than what the column holds — `Auto-upd.abs` sets
  `Id` to 20 and leaves the counter at 3, and a writer that recomputed it from the rows would be
  wrong on exactly that file. Passing `nil` for such a column means "number this row" and takes
  `counter + Increment`. **Never recompute this counter from the data**, and never leave it
  stale: a stale counter has the engine reissue a value the table already holds and then refuse
  its own insert.

  The way this was missed for a whole cycle is worth keeping. `Auto-ins.abs` was read as saying
  no counter is stored, because an `AUTOINC` insert touches the same page _types_ an
  `Int32`-keyed insert does. It does — and the counter is on one of them, on the page an
  ordinary insert rewrites anyway for the record count. Comparing page types is not comparing
  pages.

  A `UNIQUE`/`PRIMARY KEY` index used to be on that refusal list, and lifting it was gated in this
  file on "do not do it on the strength of the duplicate check alone". That gate was met the way it
  asked to be: the `Keys*.abs` family shows the engine splicing a key-enforcing leaf exactly as it
  splices a plain one, seven pairs byte-identical with no `State` exclusion, and the constraint and
  its index lifted together. Three of its refusals are engine behaviour that SQL would not predict
  and that only a file could settle — a `PRIMARY KEY` column refuses a `NULL` though it carries **no
  `NOT NULL` record**, a `UNIQUE` index admits the first `NULL` and refuses the second as a
  duplicate, and every refused write leaves the file **byte-identical**, which is why every check
  runs before a page is touched.

  In front of maintenance sits the constraint gate (`newConstraintChecks` in
  `writer_constraint.go`): a `NOT NULL` and a `MINVALUE`/`MAXVALUE` pair are **checked** per row,
  a `PRIMARY KEY` or `UNIQUE` is checked **by its index** and so records no per-row check at all,
  and anything else — a record whose column or bound type does not resolve, a key whose index is
  not maintained — refuses every write with `ErrConstraintsNotEnforced`. A write that ignores a
  rule leaves the file holding a row the engine would have rejected. What the gate replaced is
  worse than a refusal: `Update` went through and silently left the index describing a key the row
  no longer had.

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
- **Test against real files**: Primary validation uses real `.abs` fixtures in `testdata/`. That directory is gitignored — almost all of the files are real private project data and are never committed. The exceptions are the eight `testdata/Employees-*.abs` fixtures (one per encryption algorithm), the fourteen `testdata/Writes*.abs` fixtures (the write path's ground truth, four of them carrying a user index) and the twelve `testdata/MultiTable*.abs` fixtures (the table catalog and the schema operations over it), the five `testdata/Empty*.abs` fixtures (what the engine writes for a brand-new database, and how it grows one) `testdata/Constraints.abs` (twelve tables differing from a control by one column constraint or index variation each) `testdata/Types.abs` (eight tables covering every field type, each column of unknown width followed by a sentinel so a wrong width fails loudly) and `testdata/Types2.abs` (what `Types.abs` could not settle: the TimeStamp layout, from eleven instants, and eleven refused attempts at a `BYTES` value), which are ours and are committed; see `testdata/README.md`. Tests that need a fixture must `t.Skip` when it is missing, so a fresh clone (and CI) still runs green on the synthetic, unit, `Employees-*` and `Writes*` tests alone. A green CI run therefore still does **not** mean the parser was validated against the private fixtures; run `just test` locally for that.
- **Windows-1252 aware**: String fields use Windows-1252 encoding by default. Always decode to UTF-8.

## Formatting and Linting

`gofumpt` (a strict superset of `gofmt`) plus `gci` for import grouping. This is configured in two places and they must agree:

- `treefmt.toml` — runs `gofumpt` and `gci` in write mode, plus `prettier` for Markdown/YAML/JSON and `shfmt`/`shellcheck` for shell.
- `.golangci.yml` — enables `gofumpt` and `gci` under `formatters:`.

Use `just fmt` to format and `just check-formatted` to verify. `check-formatted` formats a scratch copy of the tree, so it never rewrites your files.

Linting is `golangci-lint` with `default: all` and a curated disable list. Every entry in `.golangci.yml` — disabled linter or exclusion rule — carries a comment explaining why; keep that up if you add one.

## CI

`.github/workflows/ci.yml` runs build, `go vet`, `gofmt`, `go mod tidy -diff`, race tests, `golangci-lint`, and a short fuzz budget per target. It sees only the two committed Twofish fixtures, not the private ones, so read the scope note at the top of that file before trusting a green check.
