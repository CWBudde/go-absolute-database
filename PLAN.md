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

- [ ] Expose the column definition's **flags byte**, which is parsed and discarded, so `Column`
      can report nullability without going through the constraint records.

### Phase 3 — records

- [ ] `GUID` deserialization.
- [~] `Currency`, `Single`, `SmallInt`, `Word`, `Int64`, `WideString` and the date/time types
  are implemented against the SDK declaration and have **zero corpus coverage**. See
  [docs/open-questions.md](docs/open-questions.md#validation-gaps).

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

- [ ] **Write and check constraint records.** They are read; nothing writes or enforces them,
      and that one gap blocks two operations. Compaction refuses a table carrying them
      (`ErrConstraintsNotRebuilt`) because `CREATE TABLE` cannot write them back, and the record
      writer refuses one too (`ErrConstraintsNotEnforced`, `refuseConstraints` in
      `writer_index.go`) because no write here checks a `NOT NULL`, a `MINVALUE`/`MAXVALUE` pair
      or a uniqueness rule. Between them that keeps both operations off most real tables, which
      makes this the highest-leverage item in this file.
- [ ] **Write a `DEFAULT`.** The same problem one level down, refused the same way
      (`ErrColumnDefault`): it lives in the column definition rather than the constraint array,
      and `serializeColumnDef` writes the absent marker unconditionally.
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

1. **Writing and checking constraint records.** The missing half of work whose reading side is
   already done and pinned by `Constraints.abs`, and it now unblocks two things rather than one:
   compaction on real tables (`ErrConstraintsNotRebuilt`) and record writes to a constrained
   table (`ErrConstraintsNotEnforced`). A `DEFAULT` is the same gap one level down
   (`ErrColumnDefault`).
2. **The `ALTER TABLE` rebuild**, which would retire the last deliberate divergence from the
   engine's byte output.
3. **Encrypted writes**, which need the control block decoded first. A guess produces a file the
   engine will not open, so this starts with analysis, not code.

Two validation gaps cannot be closed from inside this repository, and both need the Delphi
engine driven directly: the **fourteen uncovered field types**, and any evidence for a **split
B-tree leaf** or a **second PFS page**. The fixture recipe in `testdata/README.md` is the route
to all three.
