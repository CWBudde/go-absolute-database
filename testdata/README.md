# testdata

Almost everything in this directory is **deliberately not committed**. The `.abs`
fixtures the parser is developed against are real customer project data, and
`.gitignore` excludes `testdata/*` for that reason. A fresh clone therefore has only the
`Employees-*`, `Writes*` and `MultiTable*` fixtures below, and every test that needs one of
the others skips (see `requireFixture` in `absdb_test.go`). A green CI run does **not** mean the parser was
validated against real customer files — run `just test` locally for that.

## The committed fixtures

Three sets are committed: the eight `Employees-*.abs` files, one per encryption algorithm,
the eleven `Writes*.abs` files that pin the write path, and the six `MultiTable*.abs` files
that pin the table catalog and the schema operations over it.

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

| File                         | `TABSCryptoAlgorithm` | Cipher                                        |
| ---------------------------- | --------------------- | --------------------------------------------- |
| `Employees-Rijndael_128.abs` | 0                     | AES-128 (DEC's schedule coincides with AES)   |
| `Employees-Rijndael_256.abs` | 1                     | DEC's Rijndael — **not** AES-256, see PLAN.md |
| `Employees-DES_Single.abs`   | 2                     | DES                                           |
| `Employees-DES_Triple.abs`   | 3                     | DEC's `TCipher_3TDES`, 24-byte block          |
| `Employees-Blowfish.abs`     | 4                     | Blowfish (`golang.org/x/crypto/blowfish`)     |
| `Employees-Twofish_128.abs`  | 5                     | DEC's Twofish variant                         |
| `Employees-Twofish_256.abs`  | 6                     | DEC's Twofish variant                         |
| `Employees-Square.abs`       | 7                     | Square                                        |

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

Eleven unencrypted files that differ from one another by exactly one SQL statement. Each was
made by copying its predecessor and running that single statement in the Absolute Database
Manager, so a byte diff between a file and its parent shows precisely what the engine
changes for that operation — and nothing else.

| File                 | Made from         | Statement                                         | Rows |
| -------------------- | ----------------- | ------------------------------------------------- | ---- |
| `Writes.abs`         | created fresh     | `CREATE TABLE` + three `INSERT`s, **no index**    | 3    |
| `Writes-ins1.abs`    | `Writes.abs`      | `INSERT INTO Writes VALUES (4,'Alan',555.5,True)` | 4    |
| `Writes-ins2.abs`    | `Writes-ins1.abs` | `INSERT ... (5,'Emmy',777.25,False)`              | 5    |
| `Writes-upd.abs`     | `Writes.abs`      | `UPDATE Writes SET Salary = 1.5 WHERE Id = 2`     | 3    |
| `Writes-updname.abs` | `Writes.abs`      | `UPDATE Writes SET Name = 'Grazia' WHERE Id = 2`  | 3    |
| `Writes-upd2.abs`    | `Writes.abs`      | `UPDATE Writes SET Salary = 3.25 WHERE Id < 3`    | 3    |
| `Writes-del.abs`     | `Writes.abs`      | `DELETE FROM Writes WHERE Id = 2`                 | 2    |
| `Writes-del2.abs`    | `Writes.abs`      | `DELETE FROM Writes WHERE Id < 3`                 | 1    |
| `Writes-delins.abs`  | `Writes-del.abs`  | `INSERT ... (6,'Rosa',42.0,True)`                 | 3    |
| `Writes-idx.abs`     | created fresh     | as `Writes.abs` **plus** `CREATE INDEX IdxId`     | 3    |
| `Writes-idx-ins.abs` | `Writes-idx.abs`  | `INSERT ... (4,'Alan',555.5,True)` **with** index | 4    |

The table is `Writes` — `Id` INTEGER, `Name` VARCHAR(20), `Salary` FLOAT, `Active`
BOOLEAN — with the same three rows as `Employees`, no password and, deliberately, **no
index**: this package refuses to insert into or delete from an indexed table, because it
cannot yet maintain a B-tree. `oracle_test.go` names them in `unindexedFixtures` so its
leaf-scan cross-check skips them by name rather than falling back to a silent skip for any
index-less file.

`TestWriterMatchesEngineByteForByte` applies each statement through this package and
requires the result to be **byte-identical** to the engine's file. That is what the two
pairs affecting two rows each (`Writes-upd2.abs`, `Writes-del2.abs`) are for: every
single-row case moves the table's change counter by one, which cannot distinguish counting
transactions from counting records. The two-row cases move it by two, and without them the
writer would have been wrong for every multi-row transaction.

The last two are the odd pair out: they _do_ carry a user index, so this package refuses
to insert into them (`TestWriterRefusesToStrandAnIndex` pins that). They are committed
because `Writes-idx-ins.abs` is exactly what the engine produces for the insert that is
currently refused, and so is the ground truth for implementing index maintenance — see
PLAN.md, Phase 7.

They contain no customer data and no vendor material, checked the same way as the
`Employees-*` files: scanning each one finds only `ABS0LUTEDATABASE`, `ABSP`, the table
name and the invented row values.

### `MultiTable*.abs` — the table catalog and the schema operations over it

Six unencrypted files, the only ones in the corpus holding more than one table. Every other
fixture, customer files included, has exactly one, which is why the multi-table bug they
close survived unnoticed for so long.

| File                        | Made from               | Contents                                           |
| --------------------------- | ----------------------- | -------------------------------------------------- |
| `MultiTable.abs`            | created fresh           | `Alpha`, `Beta`, `Gamma` + `CREATE INDEX` on Alpha |
| `MultiTable-drop.abs`       | `MultiTable.abs`        | the same, then `DROP TABLE Beta`                   |
| `MultiTable-dropfirst.abs`  | `MultiTable.abs`        | the same, then `DROP TABLE Alpha`                  |
| `MultiTable-droplast.abs`   | `MultiTable.abs`        | the same, then `DROP TABLE Gamma`                  |
| `MultiTable-create.abs`     | `MultiTable.abs`        | the same, then `CREATE TABLE Delta (X, Y)`         |
| `MultiTable-createdrop.abs` | `MultiTable-create.abs` | the same, then `DROP TABLE Delta`                  |

The last four are what `TestDropTableMatchesEngineByteForByte` holds the drop to, and each
covers something the original pair could not. Dropping `Alpha` rewrites every catalog entry
behind it and frees the file's highest page, so `LastUsedPageNo` has to move; dropping
`Gamma` rewrites none; dropping `Delta`, which was created and never inserted into, is the
only case of a table that owns no data page at all, so its index page can only be found by
reading the page number out of its column definitions.

`MultiTable-create.abs` earns its place twice over. Besides being the base of the
`createdrop` case, it is the only evidence for what `CREATE TABLE` allocates: five pages
(two for the table's system internal file, one each for the column definitions, the
counters and the record page index) and **no data page** — that arrives with the first
insert.

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
the eleven `Writes*` fixtures structurally could not: the table info counters were read at
fixed offsets that are correct only for a four-column table, and every write fixture has
four columns. See PLAN.md, Phase 7.

`MultiTable-drop.abs` is the same size as its parent and differs by 45 bytes; the other
drops differ by 61, 34 and 29. They record that `DROP TABLE` tombstones the dropped
table's pages (`ABSP` `State` = `0x7FFFFFFF`) rather than erasing them, and that it
compacts the catalog while leaving the last entry duplicated at the end — so a parser that
ignores the catalog's length field reports the surviving table twice. Between them they
also pin the two allocation maps: the Page Free Space bitmap on page 0 and the Extent
Allocation Map on page 1. See PLAN.md, Phase 8.

They contain no customer data and no vendor material, checked the same way as the other
committed fixtures: scanning each one finds only `ABS0LUTEDATABASE`, `ABSP`, the three
invented table names and the invented row values, and no UTF-16 strings at all.

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
- `SQL → Execute SQL Script` runs a whole semicolon-separated `.sql` file in one pass, so
  the `CREATE TABLE` / `INSERT` / `CREATE INDEX` statements above need no per-statement
  typing.
- Clearing `HKCU\Software\ComponentAce\Absolute Database\DatabaseManager\History\File1..5`
  before launch stops DBManager reopening the last database and prompting for its
  password.

Verify the result by reading the file rather than by screenshot: byte 43 is the
`Encrypted` flag and byte 78 is the algorithm, which must match the table above.

The `Writes*` and `MultiTable*` files use the same route with the encryption checkbox left
off, opening a copy of the parent through `argv[1]` and typing the one statement into the
SQL tab.
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
