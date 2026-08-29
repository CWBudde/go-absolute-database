# The database header

The first **380 bytes** of an `.abs` file are the database header. It is not a page: page 0's
own `ABSP` header sits at file offset 380, so the database header occupies exactly the space
in front of it.

## `TABSDBHeader` — offsets 0..75

Transcribed from the SDK's C++Builder header (`ABSTypes.hpp`) and verified against every
fixture.

| Offset | Size | Type       | Field               | Description                                      |
| ------ | ---- | ---------- | ------------------- | ------------------------------------------------ |
| 0      | 16   | char[16]   | `Signature`         | `ABS0LUTEDATABASE` (digit zero, not letter O)    |
| 16     | 2    | int16 LE   | `HeaderSize`        | 76                                               |
| 18     | 8    | float64 LE | `Version`           | Engine version — 5.13, 7.10, 7.61, 7.94 observed |
| 26     | 2    | uint16 LE  | `PageSize`          | Page size in bytes; 4096 and 2048 observed       |
| 28     | 2    | uint16 LE  | `PageCountInExtent` | Pages per allocation extent; 8 and 4 observed    |
| 30     | 4    | int32 LE   | `TotalPageCount`    | Total pages in the file                          |
| 34     | 4    | int32 LE   | `LastUsedPageNo`    | Highest page number still allocated              |
| 38     | 4    | int32 LE   | `State`             | Transaction counter — one per committed write    |
| 42     | 1    | byte       | `WriteChangesState` | 2 in every fixture; nothing observed moves it    |
| 43     | 1    | bytebool   | `Encrypted`         | `0x00` = plaintext, **`0xFF`** = encrypted       |
| 44     | 32   | byte[32]   | `Reserved`          | Zero in all samples                              |

## Offsets 76..379

| Offset | Size | Field                                                                                          |
| ------ | ---- | ---------------------------------------------------------------------------------------------- |
| 76     | 280  | `TABSCryptoHeader` when `Encrypted` is set; zero otherwise. See [encryption.md](encryption.md) |
| 356    | 2    | int16 LE — a third size field, value 20                                                        |
| 358    | 18   | The block that size field introduces. Zero in every fixture; contents unidentified             |
| 376    | 4    | int32 LE `LastObjectID` — the last object id handed out                                        |

`356 + 20 = 376`, so the trailing block accounts for the whole gap between the crypto
sub-header and `LastObjectID`.

## Geometry

Two header fields fix the file's shape, and the file length follows from them:

```
file length = TotalPageCount * PageSize + 380
```

The trailing 380 bytes exist so that the last page's payload is complete; see
[pages.md](pages.md). `parseHeader` checks this invariant.

Four header fields are mutable, and this package writes each through a dedicated setter:
`TotalPageCount` (30) when the file grows, `LastUsedPageNo` (34) when the highest allocated
page moves, `State` (38) once per committed transaction, and `LastObjectID` (376) when an
object id is handed out.

## A fresh database

`File → Create Database` produces exactly **six pages** — five allocated and one free —
regardless of extent size, with `LastUsedPageNo` 4, `State` 1, `LastObjectID` 0, and page
types 3, 2, 4, 5, 6. That is also the floor the engine truncates to when the last table is
dropped.

The header is almost entirely constant across databases:

- Changing `PageSize` and `PageCountInExtent` moves **exactly two header bytes**, at 26 and 28.
- Encryption sets offset 43 to `0xFF` and fills offsets **80..339** with 260 bytes of key
  material that are zero in an unencrypted file.
- **`Max Connections` is not a header field.** It sizes the connection table on page 3 — a
  zero-filled internal file whose `Size` field carries the count — and touches nothing in the
  header.

The five system pages are:

| Page | Type | Role                                                                 |
| ---- | ---- | -------------------------------------------------------------------- |
| 0    | 3    | Database header block; its payload is the Page Free Space map        |
| 1    | 2    | Extent Allocation Map                                                |
| 2    | 4    | System file directory — a 20-byte internal file naming pages 3 and 4 |
| 3    | 5    | Connection/lock table — `MaxConnections` zero bytes                  |
| 4    | 6    | Table catalog, zero-length in a fresh file                           |

Page 2 is byte-identical in every database examined.

## Version

Files are read at 5.13, 7.10, 7.61 and 7.94. No structural difference between versions has
been isolated. Databases this package creates are stamped **7.94**.
