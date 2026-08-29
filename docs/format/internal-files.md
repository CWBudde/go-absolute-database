# Internal files

Several format structures are stored as an **internal file**: a byte stream with a ten-byte
header, held in a page's payload and optionally compressed.

## The header

| Offset | Size | Type     | Field                  |
| ------ | ---- | -------- | ---------------------- |
| 0      | 1    | byte     | Version — `0x0A`       |
| 1      | 4    | int32 LE | `Size`                 |
| 5      | 4    | int32 LE | `DecompressedSize`     |
| 9      | 1    | byte     | `CompressionAlgorithm` |

The connection table is the one internal file that sets `Size` and leaves `DecompressedSize`
at **zero**. A writer must reproduce that asymmetry rather than tidy it.

## Compression: C zlib at level 1

Every compressed internal file in the corpus — schema (type 8), table info (9), catalog (6) —
is byte-identical to `zlib.compress(data, 1)`, all 48 of them, and to no other level.

**Go's `compress/zlib` reproduces none of them, at any level.** Go's level 1 is its own fast
encoder, not zlib's `deflate_fast`; the shortest schema in the corpus is 93 bytes from zlib
and 105 from Go.

`internal/zlib1` is a port of zlib's level-1 path: the `deflate_fast` state machine with the
`{good 4, lazy 4, nice 8, chain 4}` configuration, hash chains and `longest_match`, the symbol
buffer, and `trees.c`'s tree construction and block emission including the stored/static/dynamic
choice. It is the encoder every write path must use; `compress/zlib` is for reading only.

## Expansion bounds

Two ceilings, split by source, because the two streams have different shapes:

- A **BLOB** holds user payload of unknown shape, so the only bound that cannot reject a
  legitimate file is zlib's own maximum expansion: `maxCompressionRatio = 1000` (the limit is
  1032:1).
- An **internal file** is a format structure. Across the corpus's compressed internal files the
  widest expansion is **4.65x**, so `maxInternalCompressionRatio = 64` keeps more than an order
  of magnitude of headroom.

The gap matters on a hot path: a zlib bomb small enough to fit one 4 KiB page costs 8.8 ms and
9.7 MB of allocation before the BLOB bound rejects it, and 0.2 µs and 105 bytes before the
internal-file bound does. `Schema()` takes the second path.

## The system file directory — page 2, type 4

A twenty-byte internal file whose two entries name the connection table and the table catalog.
An entry is a kind byte and an `int32` page number; the kinds are 2 (connections) and 1
(catalog). This page is byte-identical in every database examined.

## The connection table — page 3, type 5

`MaxConnections` zero bytes. The count lives in the internal file's `Size` field and nowhere
else — in particular, not in the database header.

## The table catalog — type 6

An array of 272-byte `TABSTableListItem` records with no count of its own; its length comes
from the internal file header in front of it.

| Offset | Size | Field                                                                    |
| ------ | ---- | ------------------------------------------------------------------------ |
| 0      | 256  | Delphi `ShortString` — one length byte, then 255 bytes of storage        |
| 256    | 4    | `TableID` — this is what `ABSP.ObjectID` holds on the table's data pages |
| 260    | 4    | Schema page (type 8)                                                     |
| 264    | 4    | Table info page (type 9)                                                 |
| 268    | 4    | A type-7 system page; role unidentified                                  |

The catalog is stored **uncompressed** in every fixture.

`DROP TABLE` **compacts** the array rather than holing it: the entry after the dropped one is
copied over it and the internal file's length shrinks by 272, but the copy at the end is left
in place. A parser that iterated entries without honouring the length field would report the
last table twice and never notice the dropped one was gone.

## Table info — type 9

Two counters at the **end** of the file, so their position moves with the table's width:

```
int32 ColumnCount, ColumnCount * 8 bytes, int32 Changes, int32 Records
```

`Records` is the number of live rows. `Changes` counts **records touched**, not transactions:
an `UPDATE` affecting two rows moves it by two while the page and database `State` counters
move by one.

Every fixture obeys this shape, across 5.13, 7.61 and 7.94, and every stored record count
matches the rows actually present.

## The schema stream — type 8

The table's column definitions and their tail. See [schema.md](schema.md).
