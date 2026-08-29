# go-absolute-database — Implementation Plan

A Go library for reading and writing ComponentAce **Absolute Database** `.abs` files.

Absolute Database is a single-file embedded database engine for Delphi. It replaced the Borland
Database Engine and is used by several commercial Windows applications — notably **SoundPlan**,
the dominant DACH noise calculation tool, which stores result tables, train type catalogues,
address lists and attribute tables as `.abs` files. No public specification of the binary format
exists; this library is based on reverse-engineering real files, the SDK's C++Builder headers and
DBManager's Delphi source.

**Format knowledge lives in [`docs/`](docs/README.md), not here.** This file tracks what is
built and what comes next.

## Status

| Phase | Scope                                                     | State                  |
| ----- | --------------------------------------------------------- | ---------------------- |
| 1     | File header and page navigation                           | Done                   |
| 2     | Schema extraction                                         | Done                   |
| 3     | Record reading                                            | Done                   |
| 4     | BLOB and compression support                              | Done                   |
| 5     | Index reading                                             | Done                   |
| 5e    | Multi-table support                                       | Done                   |
| 6     | Encryption — all eight algorithms, read and write         | Done                   |
| 7     | Record writes — insert, update, delete, index maintenance | Done                   |
| 8     | Schema operations — DDL, growth, create, compact          | Done                   |
| T     | Test suite, fixtures, fuzzing, CI                         | Done bar packaging     |
| 9     | `database/sql` driver                                     | Not started — deferred |

Every write path is held to byte identity against the engine's own output, with two recorded
exceptions; see [docs/writing.md](docs/writing.md).

> **Checkbox legend:** `[x]` done and verified · `[~]` partially done or unvalidated ·
> `[ ]` not started. An item with sub-steps is ticked only once every sub-step is. The sub-steps
> are there so that a session's work lands somewhere visible rather than only in the git log:
> most remaining items are several sessions of work each, and used to be one line.

## What is left, and what each piece unblocks

The write path takes **95 of the corpus's 111 tables**. Every one of the sixteen it refuses is
refused for the shape of an index, and several are refused for more than one shape at once. The
table below counts **every** reason a table is refused, not just the first one reported, which is
what makes the ordering meaningful:

| Capability             | Unblocks alone | Appears in | Cumulative, in this order |
| ---------------------- | -------------- | ---------- | ------------------------- |
| A multi-column key     | 5              | 9          | 5                         |
| A `VARCHAR` key column | 2              | 7          | 9                         |
| `NOCASE` ordering      | 0              | 3          | 12                        |
| A split B-tree leaf    | 1              | 3          | 15                        |
| `DESC` ordering        | 1              | 1          | 16                        |

Two things follow from the middle column that did not follow from the first-reason count this
file used to quote. A multi-column key is still the largest single item, but it unblocks **five**
tables, not the nine it appears in: two of the other four also need a `VARCHAR` key and two also
need a split leaf. (Eight of the nine name it as their first reported reason, which is the eight
this file used to quote.) And `NOCASE` unblocks nothing at all on its own — every table with a
`NOCASE` index keys a `VARCHAR` column with it, so it is only ever worth doing after that one.

Everything else on this page is either read-path polish or a schema-write shape no corpus table
needs (`DEFAULT`, `AUTOINC` options, encrypted creation).

## How an item closes here

The last two items to close — a key-enforcing index and an `AUTOINC` column — took the same eight
steps, and the sub-steps below are written in that shape so each is separately tickable:

1. **Survey.** Measure what the item unblocks before building it. Both of the last two turned out
   to unblock less than this file claimed.
2. **Fixture.** Drive DBManager under Wine to produce the engine's own before/after pair, per
   [testdata/README.md](testdata/README.md). Nothing below step 2 is implemented on inference.
3. **Read.** A test that reads the new fixtures and pins the shape; the finding goes to
   `docs/format/`.
4. **Maintain.** Insert, delete and a key-moving update, byte-identical to the fixture with no
   page `State` exclusion, because maintenance allocates no page.
5. **Check.** The constraint gate resolves the shape, or goes on refusing it explicitly.
6. **Create.** `CREATE TABLE` and `CREATE INDEX` build it.
7. **Compact.** The rebuild reproduces it.
8. **Record.** `docs/`, `testdata/README.md`, this file, `CLAUDE.md` — and re-run the survey from
   step 1, because the numbers above are what the next item is chosen by.

## Index shapes the write path refuses (Phases 7 and 8)

These five are the whole of the sixteen refusals. Each crosses index maintenance, the constraint
gate, `CREATE TABLE`/`CREATE INDEX` and compaction, which is why each is a list rather than a
line.

### A multi-column key — 5 tables alone, 9 in total

`Constraints.abs`'s `CPkMulti` and `CIdxMulti` and five private tables' `RecIdx`/`p`.

- [ ] a. Confirm from the files already committed what a concatenated key looks like — the
      corpus's key sizes of 10 and 15 bytes are consistent with two and three 5-byte keys, and
      that is a hypothesis, not a reading. Needs no Wine run.
- [ ] b. Fixtures: a two-column index with rows, plus an insert, a delete and a key-moving
      update, in the shape of the `Writes-idx*` family.
- [ ] c. Read the leaf: the key's layout, its `KeyPrefixSize`, and which column breaks a tie.
      → `docs/format/indexes.md`.
- [ ] d. Maintain it — a comparison over the concatenation, and the splice
      (`ErrMultiColumnIndex` lifted only for the shape the fixtures pin).
- [ ] e. Resolve a multi-column `PRIMARY KEY`/`UNIQUE` in the constraint gate.
- [ ] f. `CREATE TABLE` builds it; `CREATE INDEX` writes a multi-column record.
- [ ] g. Compaction rebuilds it (`planCompactIndexes`, `planCompactConstraints`).
- [ ] h. Docs, and re-measure.

### A `VARCHAR` key column — 2 tables alone, 7 in total

`Constraints.abs`'s `CBoth`, `MultiTable-createidxgrow.abs`'s `Delta`, and it is a prerequisite
for three of the `NOCASE` tables and two of the multi-column ones.

- [ ] a. Read what a string key stores in the leaf: padded to the column width or to
      `maxIndexedSize`, and how a shorter value is terminated. The corpus has such indexes to
      read; only the ordering rule needs the engine.
- [ ] b. Fixtures: insert, delete and a key-moving update against a `VARCHAR`-keyed index.
- [ ] c. Ordering — whether the engine compares by Windows-1252 byte or by something else.
- [ ] d. Maintain it (`indexableKeyColumn` widened, `indexKeySize` no longer a constant).
- [ ] e. `CREATE INDEX`/`CREATE TABLE` build one, and compaction rebuilds it.
- [ ] f. Docs, and re-measure.

### `NOCASE` ordering — 0 alone, 3 in total

`Addresses.abs`'s `NameSort`, `Constraints.abs`'s `CIdxNoCase`, `TS03.abs`'s `EN`. Do this after
the `VARCHAR` key: all three key a string column, so neither is any use alone.

- [ ] a. Which case-folding the engine uses, from the order of an existing `NOCASE` leaf.
- [ ] b. Maintenance, `CREATE INDEX`, compaction, docs.

### A split B-tree leaf — 1 alone, 3 in total

`RCFQ0011.abs` and, with a multi-column key, `RCON0011.abs` and `RMPA0011.abs`. This is also the
ceiling under `ErrIndexTooManyRows` and under the `ErrTableFull` that the record-page index root
raises, so it is the only item here that closes a limit as well as a shape.

- [ ] a. Fixture: drive the engine past a split. The corpus's fullest leaf holds 232 of a
      possible 367, so the trigger is not "leaf full" and no existing file shows the moment.
- [ ] b. The rule: what triggers a split and where the split point falls.
      → `docs/format/indexes.md`.
- [ ] c. Perform a split in index maintenance.
- [ ] d. The same for the internal record-page index, which raises `ErrTableFull` for the same
      reason.
- [ ] e. Docs, and re-measure.

### `DESC` ordering — 1 alone, 1 in total

`Constraints.abs`'s `CIdxDesc`. The smallest of the five, and self-contained.

- [ ] a. Reverse the comparison in maintenance, and let `CREATE INDEX` write the flag.
- [ ] b. A fixture pair pinning an insert into a `DESC` index.

## Open items in completed phases

Each of these is a known gap rather than an oversight. Nothing below blocks anything above.

### Phase 2 — schema

- [x] Nullability is reported by `Column.NotNull`, read from the `NOT NULL` constraint records.
      The item this replaces asked for it via the column definition's "flags byte"; there is no
      such byte, and nullability is not in the column definition at all. See
      [docs/format/schema.md](docs/format/schema.md#column-definition).

### Phase 3 — records

- [x] `GUID` reading, via `Record.GUID` and `ParseGUID`.
- [x] The fourteen uncovered field types, closed by `testdata/Types.abs`. It cost four
      corrections; see [docs/format/records.md](docs/format/records.md#what-typesabs-settled).
- [x] **TimeStamp**, decoded by `testdata/Types2.abs` and now read and written. The engine keeps
      only year, month, day and hour of one; see
      [docs/format/records.md](docs/format/records.md#timestamp).
- [x] `Extended`, decoded from the x87 80-bit format and rounded to `float64` by
      `Record.Float`. That rounding is why it stays unwritable; see
      [docs/format/records.md](docs/format/records.md#extended).
- [ ] What a `Bytes` or `VarBytes` column's **extra byte** holds. Both store `Size + 1`, and no
      such column in any fixture holds a value. `Types2.abs` is eleven attempts at one, and the
      reason they fail is now known — `MIMETOBIN` builds a BLOB value, not a fixed-width one — so
      what is left is a parameterised insert against the Delphi engine.

### Phase 4 — BLOBs

- [ ] BZIP2 and PPM decompression. Named by the format; no fixture uses either.
- [ ] Several BLOBs on one page — `ItemNo` is parsed and not used to select among them.
- [~] Multi-page chaining is guarded against cycles but unverified: every BLOB page in the
  corpus ends its chain.

### Phase 5 — indexes

- [ ] Carry the **index name and covered column** on `IndexInfo`. Both are decoded from the
      schema stream now, but `IndexInfo` does not expose them, so `FindByStringKey` still takes
      `secondaries[0]` rather than the index that covers the column asked for. Small, and it is
      the read-side half of what the multi-column item needs anyway.
- [ ] Benchmark index lookup against a full scan.

### Phase 7 — record writes

- [ ] Crash-atomic commit. Rollback is exact, but a crash inside `Commit` leaves some pages
      written; the engine's journalling is not reproduced.

  The two single-page ceilings that used to sit here — `ErrIndexTooManyRows` and `ErrTableFull` —
  are the split-leaf item above.

### Phase 8 — schema operations

- [x] **Write and check the column-shaped constraint records.** `NOT NULL` and
      `MINVALUE`/`MAXVALUE` are written by `CREATE TABLE`, rebuilt by compaction and checked on
      every insert and update (`ErrNotNullViolated`, `ErrCheckViolated`). See
      [docs/writing.md](docs/writing.md#constraint-records).
- [x] **Write and check a `PRIMARY KEY` or `UNIQUE` record.** Closed by the `Keys*.abs`
      fixtures. `CREATE TABLE` builds the backing index, compaction rebuilds both, the index
      refuses a duplicate (`ErrDuplicateKey`) and `CreateUniqueIndex` writes the pair the engine
      writes. See [docs/writing.md](docs/writing.md#a-unique-or-primary-index).
- [x] **Index an `AUTOINC` column.** Closed by seven more `Auto*.abs` fixtures, which located
      the counter the engine keeps for such a column: one `int64` per column in the table info
      file, raised by an insert and left alone by a delete and an update. `CREATE TABLE`,
      compaction and index maintenance all take the column now, and this package assigns the
      next value the way the engine does. See
      [docs/format/internal-files.md](docs/format/internal-files.md#the-autoinc-counters).
- [ ] **Write a `DEFAULT`.** Refused with `ErrColumnDefault`: it lives in the column definition
      rather than the constraint array, and `serializeColumnDef` writes the absent marker
      unconditionally. No corpus table has one, so nothing is unblocked by it — but it is a
      small, self-contained pair with the next item.
  - [ ] a. Serialize the clause the way the column definition stores it.
  - [ ] b. A `CREATE TABLE` fixture carrying one, to compare against.
- [ ] **Write a column's `AUTOINC` options.** The same shape (`ErrColumnAutoIncOptions`): the
      five parameters are read, and `serializeColumnDef` writes the engine's defaults
      unconditionally, so a column carrying real ones is refused rather than silently reset.
  - [ ] a. Serialize the five parameters. `Types.abs`'s `TAutoInc` is the only table anywhere
        that has any, and is therefore also the oracle.
  - [ ] b. Decide what happens at `MAXVALUE` — refuse or wrap — which needs a fixture, and is
        why `ErrAutoIncExhausted` and the `CYCLED` refusal exist.
- [ ] **Encrypted writes at the database level.** Existing encrypted files are read and written
      page by page, but `CreateDatabase` refuses `Encrypted: true` and compaction refuses an
      encrypted database.
  - [ ] a. Decode the 260-byte control block at header offset 80. This is analysis, not code: a
        guess produces a file the engine will not open.
  - [ ] b. `CreateDatabase` with `Encrypted: true`, byte-identical against `Empty-encrypted.abs`.
  - [ ] c. Compaction of an encrypted database.
- [ ] **Reconsider the engine-faithful `ALTER TABLE` rebuild.** The original objection — it needs
      six free pages and nothing could grow a file — has expired, and compaction has since shown
      that replaying object ids works. What is left is the work itself.
  - [ ] a. Reproduce the four transactions the engine performs.
  - [ ] b. Reproduce the three catalog writes and the fresh object ids.
  - [ ] c. Byte identity against `MultiTable-alteradd.abs` and `-alterdrop.abs`, replacing the
        semantic comparison.

### Phase T — packaging

- [ ] **Re-tag `v0.1.0`.** The existing tag shares no ancestry with `main`, yet `../Aconiq`
      requires exactly `v0.1.0` with no `replace`. Re-tagging changes what Aconiq resolves to,
      so it is not a change to make from inside this repository alone.
- [ ] Consider moving `cmd/` to its own module so library consumers do not inherit cobra,
      pflag and mousetrap.

## Phase 9 — `database/sql` driver

Deferred until there is a concrete use case.

- [ ] `database/sql/driver.Driver`, registered as `absdb`
- [ ] `Connector`, `Conn`, `Stmt`, `Rows`, `Result`
- [ ] `SELECT`, `INSERT`, `UPDATE`, `DELETE`
- [ ] `CREATE TABLE`, `DROP TABLE`

## Next

The index-shape table above is the order: **a multi-column key**, then a **`VARCHAR` key
column**, then **`NOCASE`**, which together take the write path from 95 of 111 tables to 107.
Step (a) of the multi-column item is desk work on files already committed and is where to start;
step (b) is the first that needs the engine.

Three of the remaining validation gaps need the Delphi engine driven directly rather than through
DBManager's SQL tab: a **`Bytes` value**, a **split B-tree leaf**, and a **second PFS page**. The
fixture recipe in `testdata/README.md` is the route to the last two; it is what produced
`Types.abs`, `Types2.abs` and the `Keys*.abs` and `Auto*.abs` families, each of which closed a
gap this list used to carry.
