# Writing `.abs` files

`Open` returns a read-only handle. Writing needs `OpenForWrite`, and every write path checks
that flag (`ErrReadOnly`). Changes are buffered in memory until `Commit`.

## The standard of correctness

A write is judged **byte for byte against the engine's own output**, not by whether it reads
back correctly. Each byte-identity test takes a database DBManager produced, applies through
this package the one SQL statement or menu action DBManager was given, and requires the result
to equal DBManager's file:

| Test                                          | Operation                                   |
| --------------------------------------------- | ------------------------------------------- |
| `TestWriterMatchesEngineByteForByte`          | `INSERT`, `UPDATE`, `DELETE`, index splices |
| `TestDropTableMatchesEngineByteForByte`       | `DROP TABLE`                                |
| `TestCreateTableMatchesEngineByteForByte`     | `CREATE TABLE`                              |
| `TestCreateIndexMatchesEngineByteForByte`     | `CREATE INDEX`                              |
| `TestGrowthMatchesEngineByteForByte`          | file growth                                 |
| `TestCreateDatabaseMatchesEngineByteForByte`  | `CreateDatabase`                            |
| `TestCompactDatabaseMatchesEngineByteForByte` | `CompactDatabase`                           |

Reading a write back correctly is **not sufficient evidence**: the engine keeps counters a
naive writer would miss, and this package's own reader would not notice.

### What `State` exclusions may and may not be

A page's `State` is a random seed on allocation (see
[format/pages.md](format/pages.md#page-state)), so an operation that allocates a page cannot be
byte-identical in that word. Exclusions must be **explicit and narrow** — four bytes per named
page — and must never be widened to make a test pass. Excluding a page the operation should
have reproduced is how a real defect hides.

The operations differ in how much they perturb:

- `DROP TABLE`, the record writer and index maintenance reach **full byte identity with no
  exclusion at all** — they only touch pages that already exist.
- `CREATE TABLE` **increments** existing pages' `State` and reseeds only the five it allocates.
- `CREATE INDEX` **reseeds every page it rewrites**, evidently rewriting the whole file: pages
  with byte-identical payloads before and after still come back with a new `State`. Its test
  masks every page's `State` word and requires zero remaining differences — a wider mask but a
  stronger assertion, since it pins the content of every page including the ones the operation
  never writes.
- `CreateDatabase` excludes twelve bytes: the `State` words of the three pages the engine seeds
  randomly. Pages 0 and 1 are **not** excluded — their `State`s are the two allocation counters
  and reproducing them exactly is most of what the test proves.
- `CompactDatabase` excludes sixteen page `State` words and nothing else.

## Record writes

An `INSERT` changes 18 bytes across three pages plus one in the file header, and every one is
accounted for:

| What                  | Where                                           | INSERT        | UPDATE             | DELETE        |
| --------------------- | ----------------------------------------------- | ------------- | ------------------ | ------------- |
| Record bytes          | data page, slot `bitmapBytes + slot*recordSize` | written       | changed field only | left in place |
| Occupancy bit         | data page, bitmap bit `slot`                    | set           | —                  | cleared       |
| Per-page record count | record-page index entry for that page           | +1            | —                  | −1            |
| Table record count    | table info file, last `int32`                   | +1            | —                  | −1            |
| Table change counter  | table info file, second-to-last `int32`         | +1 per record | **+1 per record**  | +1 per record |
| Page `State`          | `ABSP` + 4, on every page written               | +1 per write  | +1 per write       | +1 per write  |
| Database `State`      | file header offset 38                           | +1 per commit | +1 per commit      | +1 per commit |

The change counter counts **records touched**; the `State` counters count **writes**. A commit
affecting two rows moves the change counter by two and each `State` by one.

The engine leaves a deleted record's bytes in place and **reuses the freed slot on the next
insert**, so `freeSlot` scans bitmap order rather than appending.

## Index maintenance

A user index over an `Int32` column is kept in step on insert, delete and a key-moving update.
Two behaviours come from fixtures and must not be tidied:

- **A removal leaves the entry slot it vacates untouched.** Removing the middle of three
  entries changes `State`, `EntryCount` 3 → 2, and entry 1's key and `ItemNo`, which take entry
  2's values. Entry 2's own bytes are left exactly as they were, a stale copy past the new end.
  Clearing that tail is the obvious implementation and misses byte identity.
- **An update of an indexed column is a removal followed by a sorted insertion**, not an
  in-place patch: keys `[1,2,3]` with `Id 2 → 9` become `[1,3,9]`, not `[1,9,3]`. The table
  info page's record count is untouched and only its change counter moves.

Maintenance allocates no page, so all four splices are byte-identical to the engine's files
with **no `State` exclusion**.

Everything else is refused with `ErrIndexNotMaintained` rather than guessed at: a tree deep
enough to have split, a key of another shape, a multi-column index (`ErrMultiColumnIndex`), and
a `DESC` or `NOCASE` index, which orders its leaf differently than this package compares. That
still covers every indexed private fixture — they all carry a multi-column index or a
uniqueness constraint this package does not check.

## Constraint records

Read since the schema tail was decoded, written and checked since. The two halves landed
together because each is useless without the other: writing a `NOT NULL` a write then ignores
puts a row in the file the engine would have rejected, and checking one the rebuild drops
enforces a rule the copy no longer has.

**Writing.** `serializeConstraintRecord` is the exact mirror of the parser, and its oracle runs
in the only direction the corpus allows: no fixture shows the engine adding a constraint to a
table that did not have one, so every record in every fixture is parsed and written again and
compared against the bytes it came from — 75 of them over 27 tables, both bodies, all four
kinds, one, two and three covered columns, and a `CHECK` record's bounds
(`TestConstraintRecordsReserializeByteForByte`).

Placing the records is measured separately, because a correct record in the wrong place is
still a broken stream. `CREATE TABLE` writes the array itself rather than splicing it in
afterwards, and replaying `Constraints.abs`'s own statements into a database this package
created reproduces the engine's schema stream byte for byte, **object ids included**
(`TestCreateTableWritesTheEngineSchemaStream`). Reaching `CMinMax`'s ids means standing in for
the three tables between it and `CNotNull` that this package cannot create; that it then lands
on the engine's bytes is what checks the id order — table, columns, indexes, constraints.

**Checking.** A write tests the encoded record before any page is touched:

| Kind                            | Checked                                       | Refused with                |
| ------------------------------- | --------------------------------------------- | --------------------------- |
| `NOT NULL` (3)                  | the record's null flag for the covered column | `ErrNotNullViolated`        |
| `MINVALUE`/`MAXVALUE` (4)       | the value against either bound, inclusive     | `ErrCheckViolated`          |
| `PRIMARY KEY` (0), `UNIQUE` (2) | not checked                                   | `ErrConstraintsNotEnforced` |

A key constraint keeps the blanket refusal, and the reason has changed. The duplicate scan it
needs is straightforward; what it would buy is nothing, because such a constraint always comes
with an index record implementing it and `maintainableIndexColumn` will not maintain that
index. Letting the insert through would leave the index describing rows it no longer covers —
the exact failure Phase 7's unguarded `Update` was. The two have to lift together, and the
index half needs a fixture of the engine inserting into a `UNIQUE` or `PRIMARY` index; all four
`Writes-idx*` files carry a plain one.

No fixture can show the engine _rejecting_ a write, because a rejected write leaves no file
behind, so the checks are held to the narrower standard of never passing a row the constraint's
own bytes forbid. The two rules assumed rather than observed are in
[open-questions.md](open-questions.md); both can only make this package accept a row.

## Schema operations

Measured as one-statement diffs from a three-table database:

| Statement                                 | Bytes | What it does                                                                                                                                                                                             |
| ----------------------------------------- | ----- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CREATE TABLE Delta (X, Y)`               | 198   | allocates **five** pages — two system, one each for column definitions, counters and the record-page index — and **no data page**; appends a catalog entry; moves `LastObjectID` by 1 + the column count |
| `DROP TABLE Beta; CREATE TABLE Delta …`   | 128   | the new table takes the freed pages, **lowest first**                                                                                                                                                    |
| `ALTER TABLE Gamma ADD (W INTEGER)`       | 248   | rebuilds the table — see below                                                                                                                                                                           |
| `ALTER TABLE Gamma DROP (V)`              | 234   | as above                                                                                                                                                                                                 |
| `CREATE INDEX IdxBetaCode ON Beta (Code)` | 93    | allocates one index page, rewrites the table's column definitions, moves `LastObjectID` by 1                                                                                                             |
| `DROP INDEX Alpha.IdxAlphaId`             | 25    | frees the index page, rewrites the table's column definitions                                                                                                                                            |

### `DROP TABLE`

- Every page the table owns is **tombstoned**: `ABSP.State` set to `0x7FFFFFFF`. Nothing is
  erased — pages keep their type, owner and contents.
- The catalog entry is removed by moving later entries down and shortening the internal file by
  one entry; the bytes past the new end are left alone.
- The PFS loses one bit per freed page, and page 0's `State` advances **once per bit**, not once
  for the write.
- The EAM downgrades every extent that was full, and page 1's `State` advances once per entry
  changed — so it is not written at all when nothing changes.
- `LastUsedPageNo` follows the highest page still allocated; the header `State` advances once.

A table's pages are found without guessing: the catalog entry names its system internal file,
column definitions and counters; `ABSP.ObjectID` names its data pages; the last eight bytes of
the column definitions name its two system index roots. Only a user index has to be recognised
by the data pages its leaves point at.

### `ALTER TABLE` — a deliberate divergence

**The engine does not edit a table in place.** It runs a four-statement sequence:

```
CREATE TABLE <temp> (the new column list)
copy every row across
rename <temp> to the original name
DROP TABLE the original
```

Every counter agrees, and nothing else explains all of them at once: the file header `State`
moves by four; the catalog page's `State` by three (append, rename, the drop's shift); the
table keeps its catalog position while a stale entry with the same name sits behind it; the
table and column ids are all new; six pages are freed and six allocated.

This package **splices the schema stream and rewrites the records in place** instead: three
pages touched, thirty bytes, object ids preserved. It is held to _semantic_ identity against
the engine's own `ALTER` output (`TestAlterTableMatchesEngineSemantically`) — same table list,
same columns, same rows, with column ids excluded and only column ids — and
`TestEngineAlterTableRebuildsTheTable` pins the engine's strategy so the reasoning cannot go
stale.

The case most likely to be silently wrong is covered deliberately: `nullFlagBytes` is sized
from the column count, so a count crossing a multiple of 8 moves every field in every record.
`TestAlterTableColumnCountBoundary` drives 8→9 and 16→17 and back, asserting that
`nullFlagBytes` actually changed, so an implementation that kept the old width cannot pass by
coincidence.

What the tests do not prove: no byte-for-byte guarantee for either `ALTER TABLE` form; the
table-info counters are not validated by them; the B-tree cross-check runs against one indexed
table only; and no constrained table is run through `ADD COLUMN`, so the claim that the splice
is constraint-agnostic rests on its structure rather than a direct test.

### Compaction

`TABSDatabase.CompactDatabase` routes to `InternalCopyDatabase`. It is **a full rebuild into a
new file**, not a defragment: compacting a 30-page database with twelve free pages yields 18
pages with none free, `LastUsedPageNo` 23 → 17, the file `State` **reset** from 12 to 6, and
`LastObjectID` 11 → 7 with object ids **reallocated**.

The output is exactly `CreateDatabase` + `CREATE TABLE` + `CREATE INDEX` + copy rows, per table
in catalog order — and that is how it is implemented. Two details come from the fixture rather
than the SDK:

- The per-table order is **`CREATE TABLE` → `CREATE INDEX` → copy rows**, not rows before
  indexes: a table's index page lands _ahead_ of its data page in the engine's output.
- Rebuilding by whole extents overshoots, so compaction ends by **shortening the file** to
  `LastUsedPageNo + 1` (`shrinkToLastUsedPage`), floored at the six pages of a fresh database.

Reallocating object ids does not defeat byte identity: the ids differ from the _input_ file but
are fully determined in the _output_ by the order in which objects are created, and replaying
that order hands out the same ones.

## Ordering constraints

- An index's root page must be **allocated before** the schema stream is serialized, because
  the stream embeds its page number.
- The file must be **physically extended before** a new page is buffered: `ReadPage` rejects
  `n >= PageCount()` and returns `ErrTruncated` on a short read.

## Refusals

Each is an error rather than a silent success.

| Error                        | Meaning                                                                                                                                                                                  |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ErrReadOnly`                | The handle was not opened with `OpenForWrite`                                                                                                                                            |
| `ErrTableFull`               | No record slot could be found or made — the single-page record-page index root is out of entries, or the row is too wide to fit any data page. A page-splitting ceiling, not a space one |
| `ErrIndexTooManyRows`        | The single-page index leaf is full — likewise                                                                                                                                            |
| `ErrOutOfSpace`              | No free page and the file cannot grow                                                                                                                                                    |
| `ErrDatabaseTooLarge`        | Growth would pass the first PFS page's 32 448-page reach                                                                                                                                 |
| `ErrIndexNotMaintained`      | The table carries an index shape this package will not keep in step                                                                                                                      |
| `ErrMultiColumnIndex`        | Specifically, an index over more than one column                                                                                                                                         |
| `ErrIndexBacksConstraint`    | `DROP INDEX` on the index a `PRIMARY KEY` or `UNIQUE` is built on; DBManager drops the constraint, not the index                                                                         |
| `ErrSchemaTailNotUnderstood` | The schema stream's tail does not parse as the documented layout                                                                                                                         |
| `ErrColumnConstrained`       | `DROP COLUMN` on a column a constraint record covers                                                                                                                                     |
| `ErrColumnIndexed`           | `DROP COLUMN` on a column an index covers                                                                                                                                                |
| `ErrBlobReferenceLost`       | An update would overwrite a live BLOB reference, whose pages nothing here can free                                                                                                       |
| `ErrBookkeepingMismatch`     | Stored counters disagree with the records present, so a write cannot bring them forward without guessing                                                                                 |
| `ErrLastTable`               | `DROP TABLE` on the database's only table                                                                                                                                                |
| `ErrTableHasBlobPages`       | `DROP TABLE` on a table owning BLOB pages                                                                                                                                                |
| `ErrPageUnattributed`        | The file holds an allocated page belonging to no table                                                                                                                                   |
| `ErrCatalogNotWritable`      | A compressed catalog, or one spanning more than one page                                                                                                                                 |
| `ErrConstraintsNotRebuilt`   | Compaction of a table carrying a `PRIMARY KEY` or `UNIQUE` record, whose index a re-created table would not have                                                                         |
| `ErrConstraintsNotEnforced`  | A write to a table declaring a constraint this package does not check — a key constraint, or a record whose column or bound type does not resolve                                        |
| `ErrNotNullViolated`         | A write storing `NULL` in a column a `NOT NULL` record covers                                                                                                                            |
| `ErrCheckViolated`           | A write storing a value outside a column's `MINVALUE`/`MAXVALUE` pair                                                                                                                    |
| `ErrEncryptionUnsupported`   | `CreateDatabase` with `Encrypted: true`, or compaction of an encrypted database                                                                                                          |
| `ErrUnsupportedColumnType`   | `CREATE TABLE` with a column type no fixture evidences                                                                                                                                   |
| `ErrUnsupportedIndexColumn`  | `CREATE INDEX` over a column type no fixture evidences                                                                                                                                   |
| `ErrBadGeometry`             | A `CreateDatabaseOptions` the format cannot express                                                                                                                                      |

A commit is **not crash-atomic**. Rollback is exact, because nothing is written before
`Commit`, but a crash inside `Commit` leaves some pages written. The engine's own journalling
is not reproduced.
