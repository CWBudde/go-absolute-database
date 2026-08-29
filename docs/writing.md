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

Maintenance allocates no page, so every splice is byte-identical to the engine's files with
**no `State` exclusion** — the four `Writes-idx*` pairs for a plain index and seven `Keys*`
pairs for a key-enforcing one.

### A `UNIQUE` or `PRIMARY` index

The leaf is spliced exactly as a plain one's is: the `Keys*.abs` family repeats every
`Writes-idx*` case against a `PRIMARY KEY` index and adds two of its own — an update that
leaves the key alone, and an insert keeping a primary key and a unique index in step at once.
What a key index adds is a refusal in front of the splice, and each part of it comes off a
file rather than off SQL's rules:

| Statement                                   | Engine                                  | Here                 |
| ------------------------------------------- | --------------------------------------- | -------------------- |
| a key the leaf already holds                | refused, file byte-identical            | `ErrDuplicateKey`    |
| a `NULL` into a `PRIMARY` index             | refused, file byte-identical            | `ErrNotNullViolated` |
| the **first** `NULL` into a `UNIQUE` index  | accepted, stored with the null flag set | accepted             |
| the **second** `NULL` into a `UNIQUE` index | refused as a duplicate                  | `ErrDuplicateKey`    |

Two of those are not what SQL would say. A `PRIMARY KEY` column carries **no `NOT NULL`
constraint record** — `Keys.abs` has none — and the engine refuses a `NULL` in it anyway, so a
writer consulting only the constraint array would let one through. And a `UNIQUE` index
compares `NULL` keys by value rather than treating them as distinct, so two of them collide.

A `NULL` key also **sorts before every value**, which `Keys-uniqnull.abs` is the only evidence
for anywhere. Comparing the null flag byte as a number puts it last; that is what
`compareInt32Keys` did, and no index in the corpus held a `NULL` key to notice.

Every refusal is made before any page is touched, because that is what the engine does: each of
the refused statements left its file byte-identical to its parent, transaction counter
included.

### An `AUTOINC` column

An index over an `AUTOINC` column is maintained on the same terms as one over an `Integer`
column, because it is the same index: `Auto.abs`'s record and leaf are the `Int32` shape byte
for byte. What kept it refused was never the key but the column's **counter**, which the engine
keeps in the [table info file](format/internal-files.md#the-autoinc-counters) and which a
writer that ignored it would leave stale — so the engine's next insert would reissue a value the
table already holds, and on the `PRIMARY KEY` column such a column usually is, refuse its own
write.

This package maintains it by the rule those fixtures pin: an insert raises the counter to the
value written when that value is larger, and a delete and an update leave it alone. Passing
`nil` for an `AUTOINC` column means "number this row", which takes `counter + Increment` — the
engine's own `INSERT INTO Auto (Name) VALUES (…)`. Seven pairs are byte-identical with no
`State` exclusion, three of them writing a value this package chose rather than one copied out
of the fixture, and the whole of `Auto.abs` is rebuilt from `Empty.abs` with only page `State`
words excluded.

Two edges stay refused rather than guessed: a `CYCLED` column (`ErrAutoIncNotMaintained`; none
exists anywhere) and an assignment past `MAXVALUE` (`ErrAutoIncExhausted`; no fixture reaches a
bound, so whether the engine refuses or wraps is unknown). The narrower `AUTOINC` field types —
`AutoIncInt8` through `AutoIncUint32` — are refused by name for the same reason, so that a
column of one is never mistaken for an ordinary integer and left with a stale counter.

Everything else is refused with `ErrIndexNotMaintained` rather than guessed at: a tree deep
enough to have split, a key of another shape, a multi-column index (`ErrMultiColumnIndex`), a
`DESC` or `NOCASE` index, which orders its leaf differently than this package compares, and a
column that is not `Int32`.

Sixteen of the corpus's 111 tables are refused, and counting every reason each one carries rather
than only the first one reported, the shapes rank like this:

| Shape                  | Sole reason a table is refused | Named among a table's reasons |
| ---------------------- | ------------------------------ | ----------------------------- |
| a multi-column key     | 5                              | 9                             |
| a `VARCHAR` key column | 2                              | 7                             |
| a split leaf           | 1                              | 3                             |
| `NOCASE`               | 0                              | 3                             |
| `DESC`                 | 1                              | 1                             |

The first column is the one to plan by, and it is not the one a first-reason count gives. A
multi-column index is reported against eight tables but is the _only_ thing wrong with five of
them: two of the other three also key a `VARCHAR` column and one also has a split leaf. `NOCASE`
is the sharper case — it is the sole reason for nothing at all, because every table that has a
`NOCASE` index keys a string column with it.

Indexes are resolved from the table's own schema records, with the pages kept as a cross-check
in the other direction: an index the pages show that the schema does not name stops the write.
Trusting the pages alone left an index whose leaf is empty invisible, because
[index attribution](format/indexes.md#attributing-a-user-index-to-a-table) works by which data
pages an entry points at — and a table whose `PRIMARY KEY` index has no rows yet is exactly
that case, which is the state every table this package creates starts in.

That closed a hole as well as opening the door. Four rowless tables carrying an index this
package cannot maintain — `Constraints.abs`'s `CIdxDesc`, `CIdxNoCase` and `CIdxMulti`, and
`MultiTable-createidxgrow.abs`'s `Delta` with its string-keyed index — used to **accept**
writes, because the index nobody could see was the index nobody refused. The first insert into
any of them would have left it describing nothing.

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
(`TestCreateTableWritesTheEngineSchemaStream`). The replay reaches `CPk` and `CUnique` as well,
whose ids are only right if an index is handed one between the last column and the constraint;
that is what checks the id order — table, columns, indexes, constraints. Only the index
record's root page number is excluded, because the page an index lands on depends on where its
table was created, and the test separately checks that the number written is the page
allocated.

A key constraint's index is built alongside it: one page allocated after the record-page index
(`Constraints.abs` puts `CPk`'s five pages at 15–19 and its index at 20), and an empty leaf
which is the record-page index's own page with a 5-byte key instead of a 4-byte one.
`TestCreateTableWithAPrimaryKeyMatchesEngineByteForByte` replays the four statements
`Keys.abs` was made with into a copy of `Empty.abs` and reproduces the **whole file**, page
`State` words aside.

**Checking.** A write tests the encoded record before any page is touched:

| Kind                            | Checked                                       | Refused with         |
| ------------------------------- | --------------------------------------------- | -------------------- |
| `NOT NULL` (3)                  | the record's null flag for the covered column | `ErrNotNullViolated` |
| `MINVALUE`/`MAXVALUE` (4)       | the value against either bound, inclusive     | `ErrCheckViolated`   |
| `PRIMARY KEY` (0), `UNIQUE` (2) | by the index implementing it                  | `ErrDuplicateKey`    |

A key record carries nothing to test a row against: it names an index, and it is that index
that refuses the duplicate. So what the checker does for a key is structural — establish that
the index exists, that it is one this writer maintains and that it is flagged — and record no
per-row check at all. A key whose index fails any of that keeps `ErrConstraintsNotEnforced`,
because checking a constraint while leaving its index stale is the exact failure Phase 7's
unguarded `Update` was.

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
in catalog order — and that is how it is implemented. An index backing a `PRIMARY KEY` or
`UNIQUE` record is not among the `CREATE INDEX` calls: `CREATE TABLE` builds it along with the
constraint, so creating it again would give the table the same index twice. Two details come
from the fixture rather than the SDK:

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
| `ErrConstraintsNotRebuilt`   | Compaction of a table carrying a constraint record `CREATE TABLE` cannot write back — a key over more than one column, or over a column an index leaf is not built for                   |
| `ErrConstraintsNotEnforced`  | A write to a table declaring a constraint this package does not check — a record whose column or bound type does not resolve, or a key whose index is not maintained                     |
| `ErrDuplicateKey`            | A write whose key a `UNIQUE` or `PRIMARY` index already holds, or `CreateUniqueIndex` over a column that already holds one twice                                                         |
| `ErrAutoIncNotMaintained`    | A table carrying an `AUTOINC` column whose counter this package will not keep in step — a `CYCLED` column, or one of the narrower `AUTOINC` field types                                  |
| `ErrAutoIncExhausted`        | An `AUTOINC` value to assign past the column's `MAXVALUE`, or past what an `Int32` column holds                                                                                          |
| `ErrNotNullViolated`         | A write storing `NULL` in a column a `NOT NULL` record covers, or in one a `PRIMARY` index covers                                                                                        |
| `ErrCheckViolated`           | A write storing a value outside a column's `MINVALUE`/`MAXVALUE` pair                                                                                                                    |
| `ErrEncryptionUnsupported`   | `CreateDatabase` with `Encrypted: true`, or compaction of an encrypted database                                                                                                          |
| `ErrUnsupportedColumnType`   | `CREATE TABLE` with a column type no fixture evidences                                                                                                                                   |
| `ErrUnsupportedIndexColumn`  | `CREATE INDEX` over a column type no fixture evidences, or a `DESC`/`NOCASE` record put through the serializer                                                                           |
| `ErrBadGeometry`             | A `CreateDatabaseOptions` the format cannot express                                                                                                                                      |

A commit is **not crash-atomic**. Rollback is exact, because nothing is written before
`Commit`, but a crash inside `Commit` leaves some pages written. The engine's own journalling
is not reproduced.
