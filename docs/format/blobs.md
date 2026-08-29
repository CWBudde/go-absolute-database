# BLOBs

## The record-side pointer

A BLOB, Memo, Graphic, FmtMemo or WideMemo column stores a **6-byte pointer** in the record:
`PageNo` (int32 LE) + `ItemNo` (uint16 LE).

## The storage page

A type-11 page carries three `int64 LE` values at payload offset 0 — **24 bytes**, not the
22-byte packed `TABSDiskBLOBHeader` the SDK header declares:

| Offset | Size | Type     | Field              |
| ------ | ---- | -------- | ------------------ |
| 0      | 8    | int64 LE | `ItemCount`        |
| 8      | 8    | int64 LE | `CompressedSize`   |
| 16     | 8    | int64 LE | `UncompressedSize` |

The payload follows. `CompressedSize == UncompressedSize` means the BLOB is stored
uncompressed.

Verified over all 60 type-11 pages of the largest BLOB fixture; read as 22 packed bytes it
yields nonsense.

## Compression

Algorithms: None, ZLIB, BZIP, PPM, at levels 1–9. Only None and ZLIB occur in the corpus;
BZIP and PPM are unimplemented.

BLOB inflation is bounded by zlib's own maximum expansion rather than by the tighter
[internal-file ceiling](internal-files.md#expansion-bounds), because a BLOB holds user payload
of unknown shape.

## Chaining

`NextPageNo` chains a BLOB across pages, with `-1` ending the chain. **Every BLOB page in the
corpus has `NextPageNo == -1`**, so the on-disk chaining form is unverified; the reader guards
it against cycles and is exercised only by synthetic files.

`ItemNo` is not used to select among several BLOBs on one page, so multiple BLOBs per page is
unsupported.

## Ownership

BLOB pages carry `ObjectID == 0xFFFFFFFF` and are reached only through a record's pointer or
the table's BLOB-page index. The BLOB-page index names the page each BLOB **starts** on and not
the ones it continues on, which is why this package refuses to free a table's BLOB pages
(`ErrTableHasBlobPages`) — freeing what the index lists would leak the rest.

No fixture has a BLOB in a multi-table database.
