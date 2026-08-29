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
> `[ ]` not started.

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
      `secondaries[0]` rather than the index that covers the column asked for.
- [ ] Benchmark index lookup against a full scan.

### Phase 7 — record writes

- [ ] Split a B-tree leaf. `ErrIndexTooManyRows` and `ErrTableFull` are both single-page
      ceilings. Split trees themselves are in the corpus and read correctly (five of them, see
      [`docs/format/indexes.md`](docs/format/indexes.md#capacity-and-splitting)); what no fixture
      shows is the engine performing a split, and the fullest observed leaf holds 232 of a
      possible 367, so the split point is not the ceiling either. The rule is unknown, so a
      multi-level tree is refused.
- [ ] Crash-atomic commit. Rollback is exact, but a crash inside `Commit` leaves some pages
      written; the engine's journalling is not reproduced.

### Phase 8 — schema operations

- [x] **Write and check the column-shaped constraint records.** `NOT NULL` and
      `MINVALUE`/`MAXVALUE` are written by `CREATE TABLE`, rebuilt by compaction and checked on
      every insert and update (`ErrNotNullViolated`, `ErrCheckViolated`). See
      [docs/writing.md](docs/writing.md#constraint-records).
- [x] **Write and check a `PRIMARY KEY` or `UNIQUE` record.** Closed by the `Keys*.abs`
      fixtures. `CREATE TABLE` builds the backing index, compaction rebuilds both, the index
      refuses a duplicate (`ErrDuplicateKey`) and `CreateUniqueIndex` writes the pair the engine
      writes. See [docs/writing.md](docs/writing.md#a-unique-or-primary-index).
- [ ] **Index an `AUTOINC` column.** This is what the item above turned out to be blocked
      behind, and it is worth more than it was: fifteen of the corpus's twenty-five key
      constraints are backed by a single-column, single-page index over an `AUTOINC` column,
      whose record and leaf are the `Int32` shape exactly. What is unknown is how the engine
      picks the next value for a row inserted without one; see
      [docs/open-questions.md](docs/open-questions.md).
- [ ] **A multi-column or `VARCHAR` key.** The other two shapes still refused, by index
      maintenance and by the rebuild alike: `Constraints.abs`'s `CPkMulti` and `CBoth`, and the
      three private fixtures whose key covers two or three columns.
- [ ] **Write a `DEFAULT`.** The same problem one level down, refused the same way
      (`ErrColumnDefault`): it lives in the column definition rather than the constraint array,
      and `serializeColumnDef` writes the absent marker unconditionally.
- [ ] **Write a column's `AUTOINC` options.** The same shape again (`ErrColumnAutoIncOptions`):
      the five parameters are read, and `serializeColumnDef` writes the engine's defaults
      unconditionally, so a column carrying real ones is refused rather than silently reset.
      `Types.abs`'s `TAutoInc` is the only table anywhere that has any.
- [ ] **Encrypted writes at the database level.** Existing encrypted files are read and written
      page by page, but `CreateDatabase` refuses `Encrypted: true` and compaction refuses an
      encrypted database, because the 260-byte control block at header offset 80 is located and
      undecoded.
- [ ] Reconsider the **engine-faithful `ALTER TABLE` rebuild**. The original objection — it needs
      six free pages and nothing could grow a file — has expired, and compaction has since shown
      that replaying object ids works.

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

In rough order of what unblocks the most:

1. **An `AUTOINC` key column**, which is what a key-enforcing index turned out to be blocked
   behind rather than the key itself. Fifteen of the corpus's twenty-five key constraints are
   backed by an index over one, and the index is the shape this package already maintains — the
   refusal is on the column's field type, because nothing here knows how the engine picks the
   next value for a row inserted without one. `testdata/Auto.abs` and `Auto-ins.abs` are the
   start of the evidence and say a counter page is not involved; a parameterised insert against
   the Delphi engine is what would finish it. A `DEFAULT` is a separate gap one level down
   (`ErrColumnDefault`).
2. **The `ALTER TABLE` rebuild**, which would retire the last deliberate divergence from the
   engine's byte output.
3. **Encrypted writes**, which need the control block decoded first. A guess produces a file the
   engine will not open, so this starts with analysis, not code.

The remaining validation gaps need the Delphi engine driven directly: evidence for a **split
B-tree leaf** or a **second PFS page**, an **`AUTOINC` value the engine assigned**, and a
**`Bytes` value** — the last two need a parameterised insert rather than DBManager's SQL tab.
The fixture recipe in `testdata/README.md` is the route to the first two; it is what produced
`Types.abs`, `Types2.abs` and the `Keys*.abs` family, each of which closed a gap this list used
to carry.
