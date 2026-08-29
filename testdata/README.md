# testdata

Almost everything in this directory is **deliberately not committed**. The `.abs`
fixtures the parser is developed against are real customer project data, and
`.gitignore` excludes `testdata/*` for that reason. A fresh clone therefore has only the
`Employees-*`, `Writes*`, `MultiTable*`, `Empty*` and `Constraints` fixtures below, and every
test that needs one of the others skips (see `requireFixture` in `absdb_test.go`). A green CI run does **not** mean the parser was
validated against real customer files — run `just test` locally for that.

## The committed fixtures

Five sets are committed, forty files in all: the eight `Employees-*.abs` files, one per
encryption algorithm, the fourteen `Writes*.abs` files that pin the write path, the twelve
`MultiTable*.abs` files that pin the table catalog and the schema operations over it, the five
`Empty*.abs` files that pin what the engine writes for a brand-new database, and
`Constraints.abs`, which isolates one column constraint or index variation per table.

### `Employees-*.abs` — one per encryption algorithm

They are committed because without them the encryption code is never exercised on a
runner at all: every other encrypted fixture is customer data.

All eight hold the identical database and differ only in the cipher:

|          |                                                                                         |
| -------- | --------------------------------------------------------------------------------------- |
| Password | `Bla`                                                                                   |
| Table    | `Employees` — `Id` INTEGER, `Name` VARCHAR(20), `Salary` FLOAT, `Active` BOOLEAN        |
| Rows     | `(1, 'Ada', 1234.5, True)`, `(2, 'Grace', 2345.75, False)`, `(3, 'Kurt', 999.25, True)` |
| Index    | `IdxId` on `Id`                                                                         |
| Origin   | Created 2026-08-28 with the ComponentAce Absolute Database Manager                      |

| File                         | `TABSCryptoAlgorithm` | Cipher                                                          |
| ---------------------------- | --------------------- | --------------------------------------------------------------- |
| `Employees-Rijndael_128.abs` | 0                     | AES-128 (DEC's schedule coincides with AES)                     |
| `Employees-Rijndael_256.abs` | 1                     | DEC's Rijndael — **not** AES-256, see docs/format/encryption.md |
| `Employees-DES_Single.abs`   | 2                     | DES                                                             |
| `Employees-DES_Triple.abs`   | 3                     | DEC's `TCipher_3TDES`, 24-byte block                            |
| `Employees-Blowfish.abs`     | 4                     | Blowfish (`golang.org/x/crypto/blowfish`)                       |
| `Employees-Twofish_128.abs`  | 5                     | DEC's Twofish variant                                           |
| `Employees-Twofish_256.abs`  | 6                     | DEC's Twofish variant                                           |
| `Employees-Square.abs`       | 7                     | Square                                                          |

They contain no customer data — the schema and all three rows are invented — and no
vendor material. This was checked rather than assumed: scanning both the ciphertext and
the decrypted payload of every file finds no path, user name, vendor string or licence
code, only the format's own `ABS0LUTEDATABASE` and `ABSP` magic plus the invented table
and row values. (`Employees-Rijndael_256.abs` happens to contain the three bytes `Z:/`
at offset 40666, which looks like a Wine drive mapping but is coincidence inside
ciphertext: it does not appear in the decrypted plaintext.) The Absolute Database licence
restricts distributing "the Software or any of its parts"; these are output files
produced by the Software, not parts of it, and carry no registration or access codes.

## Why they matter

Three of them found real bugs that no amount of reading could have, and the fourth
ruled one out:

- `Employees-Rijndael_256.abs` proved that DEC's Rijndael key schedule diverges from AES
  for 256-bit keys, so the shipped `crypto/aes` implementation was silently wrong.
- `Employees-DES_Triple.abs` proved that `DES_Triple` is `TCipher_3TDES` with a **24-byte
  block**, not two-key EDE Triple DES over 8 bytes as had been inferred.
- `Employees-Square.abs` validated the Square port end to end.
- `Employees-Blowfish.abs` was the last one added, and is the only algorithm whose
  implementation it did **not** change: Blowfish read its three rows correctly on the
  first attempt. That is a result, not a formality — the two before it did not.

They are also the only encrypted fixtures with any rows. The three `Addresses-*` fixtures
are encrypted copies of an _empty_ table, so before these existed, encrypted _record_
decryption had never been validated for any cipher. With Blowfish in place there is no
longer any algorithm whose record path rests on an empty table.

Because they have no plaintext twin, decryption is checked against each page's `ABSP`
checksum, which covers the decrypted payload and so is an oracle needing no twin, and
against the B-tree leaf scan in `oracle_test.go`.

### `Writes*.abs` — the write path's ground truth

Fourteen unencrypted files that differ from one another by exactly one SQL statement. Each was
made by copying its predecessor and running that single statement in the Absolute Database
Manager, so a byte diff between a file and its parent shows precisely what the engine
changes for that operation — and nothing else.

| File                  | Made from         | Statement                                         | Rows |
| --------------------- | ----------------- | ------------------------------------------------- | ---- |
| `Writes.abs`          | created fresh     | `CREATE TABLE` + three `INSERT`s, **no index**    | 3    |
| `Writes-ins1.abs`     | `Writes.abs`      | `INSERT INTO Writes VALUES (4,'Alan',555.5,True)` | 4    |
| `Writes-ins2.abs`     | `Writes-ins1.abs` | `INSERT ... (5,'Emmy',777.25,False)`              | 5    |
| `Writes-upd.abs`      | `Writes.abs`      | `UPDATE Writes SET Salary = 1.5 WHERE Id = 2`     | 3    |
| `Writes-updname.abs`  | `Writes.abs`      | `UPDATE Writes SET Name = 'Grazia' WHERE Id = 2`  | 3    |
| `Writes-upd2.abs`     | `Writes.abs`      | `UPDATE Writes SET Salary = 3.25 WHERE Id < 3`    | 3    |
| `Writes-del.abs`      | `Writes.abs`      | `DELETE FROM Writes WHERE Id = 2`                 | 2    |
| `Writes-del2.abs`     | `Writes.abs`      | `DELETE FROM Writes WHERE Id < 3`                 | 1    |
| `Writes-delins.abs`   | `Writes-del.abs`  | `INSERT ... (6,'Rosa',42.0,True)`                 | 3    |
| `Writes-idx.abs`      | created fresh     | as `Writes.abs` **plus** `CREATE INDEX IdxId`     | 3    |
| `Writes-idx-ins.abs`  | `Writes-idx.abs`  | `INSERT ... (4,'Alan',555.5,True)` **with** index | 4    |
| `Writes-idx-ins0.abs` | `Writes-idx.abs`  | `INSERT ... (0,'Zero',1.0,True)` **with** index   | 4    |
| `Writes-idx-del.abs`  | `Writes-idx.abs`  | `DELETE FROM Writes WHERE Id = 2` **with** index  | 2    |
| `Writes-idx-upd.abs`  | `Writes-idx.abs`  | `UPDATE Writes SET Id = 9 WHERE Id = 2`           | 3    |

The table is `Writes` — `Id` INTEGER, `Name` VARCHAR(20), `Salary` FLOAT, `Active`
BOOLEAN — with the same three rows as `Employees` and no password. The first nine files
deliberately carry **no index**, dating from when this package refused to write to an
indexed table at all; the five `Writes-idx*` files do, and are what hold index maintenance
to the engine's bytes. `oracle_test.go` names them in `unindexedFixtures` so its
leaf-scan cross-check skips them by name rather than falling back to a silent skip for any
index-less file.

`TestWriterMatchesEngineByteForByte` applies each statement through this package and
requires the result to be **byte-identical** to the engine's file. That is what the two
pairs affecting two rows each (`Writes-upd2.abs`, `Writes-del2.abs`) are for: every
single-row case moves the table's change counter by one, which cannot distinguish counting
transactions from counting records. The two-row cases move it by two, and without them the
writer would have been wrong for every multi-row transaction.

The last four all carry the `IdxId` user index, and together they are the ground truth for
index maintenance (`writer_index.go`). Each pins one splice of the B-tree leaf against the
engine's own bytes: `-idx-ins` a key that sorts last, `-idx-ins0` one that sorts first,
`-idx-del` the removal of the middle entry, and `-idx-upd` an update that moves an indexed
column's value. `-idx-del` is the one that changed an implementation: it shows the engine
leaving the vacated entry's bytes in place rather than clearing them, which no amount of
reading would have suggested.

They contain no customer data and no vendor material, checked the same way as the
`Employees-*` files: scanning each one finds only `ABS0LUTEDATABASE`, `ABSP`, the table
name and the invented row values.

### `MultiTable*.abs` — the table catalog and the schema operations over it

Twelve unencrypted files, the only ones in the corpus holding more than one table. Every
other fixture, customer files included, has exactly one, which is why the multi-table bug
they close survived unnoticed for so long.

| File                           | Made from                  | Contents                                             |
| ------------------------------ | -------------------------- | ---------------------------------------------------- |
| `MultiTable.abs`               | created fresh              | `Alpha`, `Beta`, `Gamma` + `CREATE INDEX` on Alpha   |
| `MultiTable-drop.abs`          | `MultiTable.abs`           | the same, then `DROP TABLE Beta`                     |
| `MultiTable-dropfirst.abs`     | `MultiTable.abs`           | the same, then `DROP TABLE Alpha`                    |
| `MultiTable-droplast.abs`      | `MultiTable.abs`           | the same, then `DROP TABLE Gamma`                    |
| `MultiTable-create.abs`        | `MultiTable.abs`           | the same, then `CREATE TABLE Delta (X, Y)`           |
| `MultiTable-createdrop.abs`    | `MultiTable-create.abs`    | the same, then `DROP TABLE Delta`                    |
| `MultiTable-alteradd.abs`      | `MultiTable.abs`           | the same, then `ALTER TABLE Gamma ADD (W INTEGER)`   |
| `MultiTable-alterdrop.abs`     | `MultiTable.abs`           | the same, then `ALTER TABLE Gamma DROP (V)`          |
| `MultiTable-createidx.abs`     | `MultiTable-create.abs`    | the same, then `CREATE INDEX IdxDeltaX ON Delta (X)` |
| `MultiTable-createidxgrow.abs` | `MultiTable-createidx.abs` | the same, then `CREATE INDEX IdxDeltaY ON Delta (Y)` |
| `MultiTable-createidxtab.abs`  | `MultiTable-createidx.abs` | the same, then `CREATE TABLE Epsilon (X, Y)`         |
| `MultiTable-dropcompact.abs`   | `MultiTable-drop.abs`      | the same, then `Database -> Compact Database`        |

The `drop*` files are what `TestDropTableMatchesEngineByteForByte` holds the drop to, and each
covers something the original pair could not. Dropping `Alpha` rewrites every catalog entry
behind it and frees the file's highest page, so `LastUsedPageNo` has to move; dropping
`Gamma` rewrites none; dropping `Delta`, which was created and never inserted into, is the
only case of a table that owns no data page at all, so its index page can only be found by
reading the page number out of its column definitions.

The two `alter*` files are the newest and the ones that changed a conclusion rather than
confirming one. They were long recorded as impossible to regenerate; produced under
DBManager they show that the engine implements `ALTER TABLE` as `CREATE TABLE <temp>` +
copy rows + rename + `DROP TABLE`, not as an in-place edit — four transactions, three
catalog writes, new object ids, six pages allocated and six freed. This package splices in
place instead, because matching the engine needs six free pages and `MultiTable.abs` is the
only file in the corpus that has them. See docs/writing.md's
"`ALTER TABLE` — a deliberate divergence"; `TestAlterTableMatchesEngineSemantically` and
`TestEngineAlterTableRebuildsTheTable` are what these two fixtures pin.

`MultiTable-create.abs` earns its place twice over. Besides being the base of the
`createdrop` case, it is the only evidence for what `CREATE TABLE` allocates: five pages
(two for the table's system internal file, one each for the column definitions, the
counters and the record page index) and **no data page** — that arrives with the first
insert.

The four newest files are the growth and compaction set. `MultiTable-createidx.abs` matters
for what it _lacks_: `CREATE INDEX` costs exactly one page, and `MultiTable-create.abs` had
exactly one free, so the result is the only file in the corpus with **zero free pages** — the
base from which a grow event can be observed on its own, with no page reuse mixed into the
diff. The two files derived from it then pin the rule: a one-page request and a five-page
request both extend the file by a single eight-page extent, and `Empty-p2048-e4-grow.abs`
below extends by two four-page ones, which is what shows the step is `PageCountInExtent`
rather than a constant. `MultiTable-dropcompact.abs` is what `Database -> Compact Database`
wrote for a file with twelve free pages: eighteen pages where thirty were, no free page left,
the file `State` reset from 12 to 6 and `LastObjectID` from 11 to 7. Object ids being
reallocated is the proof that compaction is a rebuild into a new file rather than a
defragment in place — the engine's `CompactDatabase` calls `InternalCopyDatabase`.

The three tables are deliberately different shapes, so that anything reading one through
another's schema is obvious rather than subtle:

| Table   | Columns                                            | Rows | ID  | Index |
| ------- | -------------------------------------------------- | ---- | --- | ----- |
| `Alpha` | `Id` INTEGER, `Name` VARCHAR(20)                   | 2    | 1   | yes   |
| `Beta`  | `Code` VARCHAR(10), `Amount` FLOAT, `Flag` BOOLEAN | 3    | 4   | no    |
| `Gamma` | `K` INTEGER, `V` INTEGER                           | 1    | 8   | no    |

Note the IDs: **1, 4, 8**, not 1, 2, 3. Anything that treats a table's ID as its position
in the catalog reads the wrong pages, and these numbers make that fail loudly.

They earned their place immediately. Before them, `OpenTable()` on `MultiTable.abs`
returned **six rows for a two-row table** — four of them other tables' bytes decoded
through Alpha's schema — with no error. They also exposed a bug in the _write_ path that
the `Writes*` fixtures structurally could not: the table info counters were read at
fixed offsets that are correct only for a four-column table, and every write fixture has
four columns. See docs/writing.md.

`MultiTable-drop.abs` is the same size as its parent and differs by 45 bytes; the other
drops differ by 61, 34 and 29. They record that `DROP TABLE` tombstones the dropped
table's pages (`ABSP` `State` = `0x7FFFFFFF`) rather than erasing them, and that it
compacts the catalog while leaving the last entry duplicated at the end — so a parser that
ignores the catalog's length field reports the surviving table twice. Between them they
also pin the two allocation maps: the Page Free Space bitmap on page 0 and the Extent
Allocation Map on page 1. See docs/format/pages.md.

They contain no customer data and no vendor material, checked the same way as the other
committed fixtures: scanning each one finds only `ABS0LUTEDATABASE`, `ABSP`, the three
invented table names and the invented row values, and no UTF-16 strings at all.

### `Empty*.abs` — what the engine writes for a brand-new database

Five files, each produced by DBManager's File -> Create Database with one setting changed
from the defaults, so that every field can be attributed to the setting that moved it.

| File                      | Setting changed from the defaults                               | Bytes |
| ------------------------- | --------------------------------------------------------------- | ----- |
| `Empty.abs`               | none — 4096 / extent 8 / 500 conn                               | 24956 |
| `Empty-p2048-e4.abs`      | `PageSize` 2048, extent 4                                       | 12668 |
| `Empty-encrypted.abs`     | encrypted, Rijndael_128, `Bla`                                  | 24956 |
| `Empty-mc100.abs`         | `Max Connections` 100                                           | 24956 |
| `Empty-p2048-e4-grow.abs` | `Empty-p2048-e4.abs` + `CREATE TABLE T1 (X INTEGER, Y INTEGER)` | 29052 |

A fresh database is exactly **six pages** — five allocated, one free — with `LastUsedPageNo`
4, `State` 1 and `LastObjectID` 0. That is the same shape the engine truncates down to when
the last table is dropped, which is what `ErrLastTable` was refusing to guess at.

What the one-variable-at-a-time diffs settle:

- **Geometry.** `Empty.abs` against `Empty-p2048-e4.abs` differs in the 380-byte header by
  **exactly two bytes** — `PageSize` at offset 26 and `PageCountInExtent` at 28. Nothing else
  in the header depends on the page size.
- **Encryption.** `Empty-encrypted.abs` sets offset 43 to **`0xFF`**, not 1, and fills offsets
  **80..339** with 260 bytes of key material that are all zero in an unencrypted file.
- **Max Connections is not a header field at all.** `Empty-mc100.abs` differs from `Empty.abs`
  only in the random `State` words of pages 2, 3 and 4 and in **page 3's internal-file Size**,
  `0x1F4` (500) -> `0x64` (100). Page 3 holds a zero-filled connection table of that many
  bytes. Without this file the 500 would have looked like a header constant.

The system pages are otherwise fixed. An internal file begins with a ten-byte header —
`0x0A` version, `int32 Size`, `int32 DecompressedSize`, `byte CompressionAlgorithm` — and
**page 2 is byte-identical in every database examined**, a twenty-byte directory naming page
3 and page 4. Page 4 is the table catalog, empty here.

Three of them are now byte targets: `TestCreateDatabaseMatchesEngineByteForByte` reproduces
`Empty.abs`, `Empty-p2048-e4.abs` and `Empty-mc100.abs` with twelve bytes excluded, the
`State` words of pages 2, 3 and 4. Pages 0 and 1 are reproduced exactly, which is how their
`State`s were identified as counters rather than random seeds — and the 2048/extent-4 file's
Extent Allocation Map count of **3** is what says the engine allocates a fresh database's
five pages one at a time rather than in a batch. `Empty-encrypted.abs` is the one this
package cannot write: the 260 bytes at 80..339 are located but undecoded, so
`CreateDatabase` refuses `Encrypted: true` rather than guessing.

### `Constraints.abs` — one variation per table

Twelve two-column tables in one database, each differing from the control in exactly one
clause, so that subtracting the control's schema stream isolates a single record. It is to
the schema tail's constraint records what `Writes.abs`/`Writes-idx.abs` were to its index
record, and it exists because those records are what made `parseSchemaTail` refuse most real
customer tables.

| Table        | Declaration                                       | Isolates                               |
| ------------ | ------------------------------------------------- | -------------------------------------- |
| `CNone`      | `(A INTEGER, B VARCHAR(10))`                      | the control — no constraint, no index  |
| `CNotNull`   | `A INTEGER NOT NULL`                              | the required flag                      |
| `CPk`        | `A INTEGER PRIMARY KEY`                           | primary key, and its implicit index    |
| `CUnique`    | `A INTEGER UNIQUE`                                | unique                                 |
| `CDefault`   | `A INTEGER DEFAULT 7`                             | a constraint carrying a typed value    |
| `CMinMax`    | `A INTEGER MINVALUE 0 MAXVALUE 99`                | two value-carrying records, one column |
| `CBoth`      | `A INTEGER NOT NULL, B VARCHAR(10) UNIQUE`        | two records on two columns             |
| `CPkMulti`   | `PRIMARY KEY (A, B)`                              | a multi-column key                     |
| `CIdxOne`    | `CREATE INDEX IdxOne ON CIdxOne (A)`              | the single-column index record         |
| `CIdxDesc`   | `CREATE INDEX IdxDesc ON CIdxDesc (A DESC)`       | the descending flag                    |
| `CIdxMulti`  | `CREATE INDEX IdxMulti ON CIdxMulti (A, B)`       | a multi-column index record            |
| `CIdxNoCase` | `CREATE INDEX IdxNoCase ON CIdxNoCase (B NOCASE)` | the case-insensitive flag              |

It paid for itself twice over. Besides settling the constraint record's layout — the `int32` the
old parser read as a reserved zero is the **constraint count**, and there are two arrays in the
tail rather than one — it turned up a **read** bug. `DEFAULT` is not stored as a constraint record
at all; it lives in the column definition as a typed value, which moves the column terminator.
`findColumnTerminator` did not know that, so `CDefault` could not be opened at all. Across the
whole corpus that is the only table whose parse changed: 20 customer tables went from refused to
parsed, and `CDefault` from unreadable to readable, with every other column list and row digest
identical before and after.

Two sources in `legacy/` corroborate the vocabulary rather than leaving it to inference: the
manual's `CREATE TABLE` grammar (`7z x -so legacy/Help/AbsDbManual.chm createtablestatement.htm`)
and DBManager's own Table Properties dialog, which lists the per-field columns as Name, Type,
Size, Required, Default, MinValue, MaxValue, BLOBCompressionAlgorithm, BLOBCompressionMode,
BLOBBlockSize, and the per-index-column ones as ColumnName, CaseInsensitive, Asc,
MaxIndexedSize.

## Regenerating them

They can be recreated on Linux by driving `DBManager.exe` from the SDK under Wine with a
virtual desktop. Three things make it much easier than it first appears:

- `DBManager.exe` takes the database path as `argv[1]` and opens it, which skips the
  `File → Open Database` dialog entirely. Its window title then reads
  `AbsDb Manager {path}`, which is also how a script can check that the open succeeded.
- The main window's own **SQL** tab is a plain editor with an execute button on its
  toolbar, and the **Log** tab below reports `Ok.` or the error. Typing a statement there
  with `xdotool type` and clicking the button is markedly more robust than driving
  `SQL → Execute SQL Script` through a file dialog: no file to write, no dialog
  coordinates to get right, and the result is legible in one screenshot.
- `SQL → Execute SQL Script` runs a whole semicolon-separated `.sql` file in one pass, and
  is the right tool the moment a fixture needs more than one statement — `Constraints.abs`
  is sixteen of them and took one file dialog. Its parser skips `--` and `/* */` comments
  and quoted text, so the script can be commented. The dialog opens in the last-used
  directory, so write the `.sql` next to the working copies and it is one double-click.
- `File → Create Database` makes a fixture from nothing. Its Database Properties dialog
  **remembers the previous run's settings, the Encrypted tick and password included**, so
  reset every field explicitly — otherwise a file meant to vary one thing varies three.
- Clearing `HKCU\Software\ComponentAce\Absolute Database\DatabaseManager\History\File1..5`
  before launch stops DBManager reopening the last database and prompting for its
  password. If it does reopen one, the window title is `AbsDb Manager {path}` and a search
  for `Absolute Database Manager` finds nothing.
- DBManager's own Delphi source is in `legacy/Utils/Source/DBManager/`. Read it rather than
  guessing at the GUI: the script runner's parser, the file dialog's behaviour and
  `aCompactDatabaseExecute` (which does nothing but call `db.CompactDatabase`) all came from
  there. `ABSDiskEngine.hpp` beside it names the free-space manager's entire API, and
  `AbsDbManual.chm` (`7z x -so`) documents the SQL grammar.

Verify the result by reading the file rather than by screenshot: byte 43 is the
`Encrypted` flag and byte 78 is the algorithm, which must match the table above.

The `Writes*`, `MultiTable*`, `Empty*` and `Constraints` files use the same route with the
encryption checkbox left off, opening a copy of the parent through `argv[1]` and typing the
one statement into the SQL tab.
**Always work on copies in a scratch directory.** The file dialogs open wherever they were
last used, and that is often this directory, which is full of irreplaceable customer files.
Verify those by bytes too: a derived file must actually differ from its parent
(`cmp -l parent child | wc -l`), because a script that silently failed to run produces a
file that is byte-identical to its parent and looks like a valid fixture.

## `zlib1/` — golden vectors for the level-1 deflate encoder

`internal/zlib1` must reproduce the C zlib library's level-1 output byte for byte, because
that is what the engine writes and what every compressed internal file in the corpus is.
`testdata/zlib1/` holds 37 pairs pinning that: `<case>.in` is the input, `<case>.z` is
exactly what C zlib 1.2.13 produced for it at level 1.

These are committed, unlike most of this directory, and deliberately so — they let CI check
the encoder without any `.abs` fixture at all. Nothing here is customer data: every case is
either synthetic or a column-definition stream taken from one of the committed `Writes*` /
`MultiTable*` fixtures.

The set covers all three deflate block types, both `BFINAL` states, multi-block streams,
long and far matches, and inputs that slide the 32 KiB window. Twelve `boundary-*` cases
sit near zlib's static-versus-dynamic block decision: a mis-transcribed `extra_blbits`
table once emitted byte-identical Huffman trees and only mis-accounted `opt_len`, so the
sole symptom was picking the wrong block type, and no other case in the set was close
enough to that boundary to notice.
