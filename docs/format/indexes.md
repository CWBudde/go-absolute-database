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

A key is `[null flag byte] + int32 little-endian` for the `Int32` columns every measured index
in the corpus covers. Keys must be compared **by value**, not with `bytes.Compare`: the page is
sorted by value, while a byte-wise comparison of little-endian integers orders 256 before 2.

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
six-page run, so it has no recorded owner. It is attributed by evidence instead: an index is a
table's when its leaf entries point at that table's data pages.

Two cases have no evidence and are therefore not returned for a multi-table file — an index
whose leftmost leaf is empty, and the BLOB-page index, whose keys are BLOB pages and whose
owner nothing in the file records. Neither arises for a single-table file, where every index is
returned because there is no other table it could belong to.

## Capacity

A single-page leaf holds 367 entries at the measured key shape. No table in the corpus is close
to a tree deep enough to have split.
