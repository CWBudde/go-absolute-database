# go-absolute-database

[![CI](https://github.com/cwbudde/go-absolute-database/actions/workflows/ci.yml/badge.svg)](https://github.com/cwbudde/go-absolute-database/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cwbudde/go-absolute-database.svg)](https://pkg.go.dev/github.com/cwbudde/go-absolute-database)
[![Go Report Card](https://goreportcard.com/badge/github.com/cwbudde/go-absolute-database)](https://goreportcard.com/report/github.com/cwbudde/go-absolute-database)

A Go library for reading and writing ComponentAce **Absolute Database** (`.abs`) files, with no
Delphi, no Windows and no vendor runtime involved.

Absolute Database is a single-file embedded database engine for Delphi that replaced the Borland
Database Engine, and it is still used by commercial Windows applications — notably **SoundPlan**,
the dominant DACH noise-calculation tool, which stores result tables, train-type catalogues,
address lists and attribute tables as `.abs` files. If you have such files and no Delphi, this
library reads them.

There is no public specification of the format. Everything here was reverse-engineered from real
files; see [Provenance and legal basis](#provenance-and-legal-basis).

> **Independent project.** Not affiliated with, authorised by, endorsed by or sponsored by
> ComponentAce. "Absolute Database" and "ComponentAce" are their trademarks, used here only to
> name the format this library reads. See [`NOTICE`](NOTICE).

## Install

```bash
go get github.com/cwbudde/go-absolute-database
```

Go 1.25 or newer. The library depends only on the standard library plus `golang.org/x/text`
(Windows-1252 decoding) and `golang.org/x/crypto` (Blowfish).

## Reading

```go
db, err := absdb.Open("project.abs")
if err != nil {
    return err
}
defer db.Close()

table, err := db.Table("Addresses")
if err != nil {
    return err
}

r, err := table.Open()
if err != nil {
    return err
}

for r.Next() {
    rec := r.Record()
    fmt.Println(rec.Int(0), rec.String(1), rec.Time(2))
}

return r.Err()
```

Encrypted databases open with `absdb.OpenWithPassword`. All eight of the engine's algorithms are
supported: Rijndael-128/256, Blowfish, Twofish-128/256, Square, DES and DES-Triple.

## Writing

Writing is opt-in. `Open` returns a read-only handle and every write path checks that flag, so a
program that only reads cannot corrupt a file by accident.

```go
db, err := absdb.OpenForWrite("project.abs")
```

That gives you record insert, update and delete with index maintenance, plus `CREATE TABLE`,
`DROP TABLE`, `CREATE INDEX`, `DROP INDEX`, `ALTER TABLE ADD/DROP COLUMN`, database creation and
compaction. Writes are held to a deliberately harsh standard: for each supported operation, a test
requires this package to reproduce **byte for byte** the file the vendor's own DB Manager produced
for the same SQL statement. Reading a write back correctly is not accepted as evidence.

Where this package cannot meet that standard it refuses the write rather than guessing — a table
declaring any constraint, or an index whose ordering is not reproduced, is rejected with a named
error. [`docs/writing.md`](docs/writing.md) lists every refusal and why it exists.

## Command line

```bash
go install github.com/cwbudde/go-absolute-database/cmd/absdb@latest
```

```
absdb info    <file>              database header, geometry, encryption
absdb tables  <file>              the table catalog
absdb schema  <file>              column definitions
absdb dump    <file>              records as text
absdb pages   <file>              page map by type
absdb blob    <file> <row> <col>  extract one BLOB
absdb hexpage <file> <page>       hex dump one page
```

## Status

Reading is complete: header, schema, records, BLOBs, indexes, multi-table catalogs and all eight
encryption algorithms. Writing covers records and schema operations, as listed above. Creating an
_encrypted_ database is not supported, and there is no `database/sql` driver yet.
[`PLAN.md`](PLAN.md) tracks what is built and what comes next;
[`docs/open-questions.md`](docs/open-questions.md) tracks what is still undecoded.

## Documentation

Start at [`docs/README.md`](docs/README.md). The format reference lives under
[`docs/format/`](docs/format/) — the file header, pages, internal files, schema, records, indexes,
BLOBs and encryption, each documented against real files rather than against a specification that
does not exist.

## Testing

```bash
just test        # or: go test ./...
just ci          # everything CI runs, plus the parts CI cannot
```

**A green CI badge is narrower than it looks.** `testdata/` is gitignored: most fixtures are real
customer project files and are never committed, and every test that opens one skips when the file
is absent. What CI covers is the synthetic and unit tests plus the forty committed fixtures, which
are ours — they hold invented tables and invented values. Those forty do exercise the full
encrypted read path for all eight algorithms and the byte-for-byte write tests, but validation
against the customer corpus happens locally only. [`docs/testing.md`](docs/testing.md) is explicit
about the boundary.

## Provenance and legal basis

This is interoperability work: reading data that belongs to the people who own the files, using an
independently written program.

- The format is not protected by copyright. In the EU that follows from Case C-406/10, _SAS
  Institute v World Programming_, in which the CJEU held that neither a program's functionality
  nor the format of its data files is a form of expression protected by copyright.
- Study and observation of a lawfully obtained program, and decompilation for interoperability,
  are permitted under Directive 2009/24/EC Arts. 5(3) and 6 — in German law §69d(3) and §69e UrhG
  — and §69g(2) UrhG voids contract terms that purport to forbid them.
- No ComponentAce code, header, binary or documentation is included here, and none ever has been.
  Redistribution is a separate question from reverse engineering, and this repository stays on the
  right side of it by containing nothing of theirs.

[`docs/provenance.md`](docs/provenance.md) records the full reasoning, including the questions
that are still open, and [`NOTICE`](NOTICE) carries the third-party attributions.

## Licence

MIT — see [`LICENSE`](LICENSE). Third-party notices for the zlib level-1 port and for DEC are in
[`NOTICE`](NOTICE).
