# B-tree indexes

## Page header

An index page (type 12) starts with an 18-byte `TABSBTreePageHeader` (`ABSBTree.hpp`):

| Offset | Size | Type      | Field            |
| ------ | ---- | --------- | ---------------- |
| 0      | 1    | bytebool  | `IsRoot`         |
| 1      | 1    | bytebool  | `IsLeaf`         |
| 2      | 4    | int32 LE  | `LeftPageNo`     |
| 6      | 4    | int32 LE  | `RightPageNo`    |
| 10     | 1    | bytebool  | `HasKeys`        |
| 11     | 1    | bytebool  | `HasSuffixes`    |
| 12     | 2    | uint16 LE | `KeyPrefixSize`  |
| 14     | 2    | uint16 LE | `EntryCount`     |
| 16     | 2    | uint16 LE | `PagePrefixSize` |

## Entry stride differs by node kind

| Node kind | Entry layout                                     | Stride        |
| --------- | ------------------------------------------------ | ------------- |
| Leaf      | key + `RecPageNo` (int32) + `RecItemNo` (uint16) | `keySize + 6` |
| Internal  | key + child `PageNo` (int32)                     | `keySize + 4` |

Entry _i_ starts at `18 + stride*i`.

## Key encoding

An `Int32` key is `[null flag byte] + int32 little-endian`, for a total of 5 bytes. Keys must be
compared **by value**, not with `bytes.Compare`: the page is sorted by value, while a byte-wise
comparison of little-endian integers orders 256 before 2.

### Multi-column key width

The empty index roots in the committed `Constraints.abs` fixture settle how component widths
combine even though they cannot yet settle the bytes of an occupied compound key:

| Declaration                   | `KeyPrefixSize` |
| ----------------------------- | --------------- |
| `CIdxOne (A INTEGER)`         | 5               |
| `CBoth (B VARCHAR(10))`       | 12              |
| `CPkMulti PRIMARY KEY (A, B)` | 17              |
| `CIdxMulti INDEX (A, B)`      | 17              |

So a multi-column key's `KeyPrefixSize` is the sum of its component key widths: here the 5-byte
integer component and the 12-byte string component make 17 bytes. The width accounting reserves
the full single-column width for each component; it does not subtract a byte for a hypothetical
shared null flag. This is also consistent with the 10- and 15-byte compound keys in the private
corpus being made from two and three 5-byte integer components.

Because both committed compound roots are empty, this evidence does **not** yet establish the
occupied byte layout, whether the component encodings are concatenated in column order, the
comparison at component boundaries or which column breaks a tie. Those need the planned
row-bearing fixture before index maintenance can rely on them.

An internal node's separator key is the child's smallest key, with a `0` sentinel on the first
entry, so a descent takes the rightmost separator `<= searchKey`.

## The leaf chain

`RightPageNo` links leaves horizontally. A full leaf scan is an **independent decoder** of the
same data the record reader produces — it yields exact row counts and `(PageNo, ItemNo)` pairs —
which makes it the strongest oracle available for validating the record path.

## System indexes

Two indexes per table are the engine's own bookkeeping, and their roots are the last two
`int32`s of the [schema stream](schema.md):

- The **record-page index** maps each data page number to the number of live records on it.
- The **BLOB-page index** names the page each BLOB starts on.

A system index entry is **6 bytes — a 4-byte key and a 2-byte count** — not the 10-byte record
reference a user index leaf holds. Read with a user-index stride the same page decodes as
nonsense.

The two are told apart by their keys: the record-page index's keys are the table's data pages.

## Attributing a user index to a table

Index pages carry `ObjectID == 0xFFFFFFFF`, and a user index is not part of the table's
six-page run, so the page itself has no recorded owner. The table's schema stream supplies the
authoritative join: each index definition names its root page, index name and covered columns.
`Table.OpenIndex` joins that record to the scanned B-tree root by page number, so `IndexInfo`
exposes `Name` and `Columns` and even an empty user index can be attributed in a multi-table
file. The column order is preserved because it is also the key's comparison order.

System indexes have no index-definition record. They retain the evidence-based fallback: an
index belongs to a table when its leaf entries name that table's data pages. The BLOB-page index
can therefore still be unattributable in a multi-table file when its keys offer no such link.

The schema metadata also makes lookup selection explicit. `FindByStringKey` takes a column name
and selects the single-column index whose `Columns` entry matches it case-insensitively; it no
longer assumes that the first secondary index happens to cover the requested value. Compound
indexes are deliberately excluded until their occupied key layout is pinned by an engine-made
fixture.

## Key-enforcing indexes

A `PRIMARY KEY` or `UNIQUE` index is an ordinary index with two of the record's three flag
bytes doing the work — `00 00 FF` for a primary key, `00 FF 00` for a unique one. **Its page is
the same in every other respect**: the same 18-byte header, the same `[null flag byte] + int32`
key, the same 11-byte stride, and the same splices on insert, delete and a key-moving update.
An empty one is the record-page index root with `KeyPrefixSize` 5 instead of 4, which pages 20
and 26 of `Constraints.abs` are byte-identical to page 19 but for.

What differs is what the engine refuses, and it is not what SQL would say:

| Statement                                   | Engine                                      |
| ------------------------------------------- | ------------------------------------------- |
| a key the leaf already holds                | refused; the file is left byte-identical    |
| a `NULL` into a `PRIMARY` index             | refused, though no `NOT NULL` record exists |
| the **first** `NULL` into a `UNIQUE` index  | accepted, null flag set                     |
| the **second** `NULL` into a `UNIQUE` index | refused as a duplicate                      |

So a `UNIQUE` index compares `NULL` keys **by value**, where SQL treats them as distinct. And a
`NULL` key **sorts before every value**: `Keys-uniqnull.abs` stores it ahead of 10, 20 and 30,
which comparing the flag byte as a number gets backwards. No index in the corpus held a `NULL`
key before that file, so nothing had contradicted the wrong order.

`CREATE UNIQUE INDEX` writes **two** objects, not one: the index record and a `UNIQUE`
constraint record naming it, taking consecutive object ids. That record's name is generated
from the covered column (`C_Unique$Alt`), and its **table name is empty** — the only empty one
anywhere in the corpus, where a `CREATE TABLE ... UNIQUE` clause names its table.

## Capacity and splitting

A single-page leaf has room for 367 entries at the measured key shape — `(4056 - 18) / 11` for
the 5-byte key and 6-byte reference of an `Int32` user index. That is a computed ceiling, not an
observed fill: **the engine splits well before a leaf is full.**

Five indexes in the corpus are split trees, all of them depth 2:

| File           | Root | Key size | Leaves | Entries | Fullest leaf |
| -------------- | ---- | -------- | ------ | ------- | ------------ |
| `RCFQ0011.abs` | 10   | 5        | 3      | 600     | 232          |
| `RCON0011.abs` | 10   | 15       | 2      | 300     | 152          |
| `RCON0011.abs` | 11   | 10       | 2      | 300     | 173          |
| `RMPA0011.abs` | 10   | 5        | 3      | 600     | 232          |
| `RMPA0011.abs` | 11   | 10       | 4      | 600     | 187          |

`TestFindByPrimaryKeyRoundTrip` reads two of these, which is what exercises the internal-node
entry stride and the descent. So **reading** a multi-level tree is covered by real files. What no
file demonstrates is the engine **performing** a split — the before/after page pair that would
show how it chooses a split point and rewrites the parent. Since the fullest observed leaf is 232
of a possible 367, the split point is evidently not "leaf full", and nothing here reproduces the
rule. That is why every write path refuses a multi-level tree.

These five trees all live in private fixtures, which are gitignored; a fresh clone and CI see
none of them.
