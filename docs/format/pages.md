# Pages, page headers and allocation

## The `ABSP` page header

An `.abs` file is not a sequence of self-contained pages. It is a byte stream in which a
40-byte `TABSDiskPageHeader` is interleaved at a **fixed phase**: every `PageSize` bytes, at
block offset `0x17C..0x1A3` (380..419).

| Offset (rel.) | Size | Type      | Field        | Notes                                        |
| ------------- | ---- | --------- | ------------ | -------------------------------------------- |
| +0            | 4    | char[4]   | `Signature`  | `ABSP`                                       |
| +4            | 4    | int32 LE  | `State`      | See [Page `State`](#page-state) below        |
| +8            | 2    | uint16 LE | `PageType`   | See the table below                          |
| +10           | 4    | int32 LE  | `NextPageNo` | Chain link; `-1` ends the chain              |
| +14           | 4    | uint32 LE | `CRC32`      | Payload checksum; **`!= 0` means encrypted** |
| +18           | 1    | byte      | `CRCType`    |                                              |
| +19           | 1    | byte      | `HashType`   |                                              |
| +20           | 1    | byte      | `CipherType` | `TABSCryptoAlgorithm` value for this page    |
| +21           | 1    | byte      | `MACType`    |                                              |
| +22           | 4    | int32 LE  | `ObjectID`   | Owning table id — only data pages set it     |
| +26           | 4    | int32 LE  | `RecPageNo`  | `RecordID.PageNo`                            |
| +30           | 2    | uint16 LE | `RecItemNo`  | `RecordID.ItemNo`                            |
| +32           | 8    | byte[8]   | `Reserved`   |                                              |

`CRC32` is `absCRC32` over the **decrypted** payload — the reflected IEEE polynomial
`0xEDB88320`, init 0, no final XOR. Structural pages carry `CRC32 == 0` and are never
encrypted.

## Payload extent

Page _N_'s payload is the **contiguous** run between its own header and the next one:

```
page N payload = file[N*PageSize + 0x1A4 : (N+1)*PageSize + 0x17C]
length         = PageSize - 40                     (4056 for a 4096-byte page)
```

The payload starts right after page _N_'s header and continues into the **next block's first
380 bytes**. It does not wrap around within the block: the tail of page _N_ lives at the start
of block _N+1_.

Three independent confirmations:

1. **File length** is exactly `TotalPageCount * PageSize + 380`; the trailing 380 bytes exist
   precisely so the last page's payload is complete.
2. **Records.** A data page with occupancy bitmap `ff ff 7f` holds 23 slots, and only under
   this model do all 23 carry a valid null-flag prefix — the last two live in the next block's
   first 380 bytes.
3. **Index entries.** A leaf's entries continue across the block boundary as an unbroken
   `key + PageNo + ItemNo` sequence.

The ciphertext extent of an encrypted page is this same span.

## Page types

| Value | Constant            | Role                                                                                                                          |
| ----- | ------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| 2     | `PageTypeSystemDir` | Extent Allocation Map (page 1)                                                                                                |
| 3     | `PageTypeFileHdr`   | Database header block (page 0); its payload is the Page Free Space map                                                        |
| 4     | —                   | System file directory (page 2)                                                                                                |
| 5     | —                   | Connection/lock table (page 3)                                                                                                |
| 6     | `PageTypeTableList` | Table catalog                                                                                                                 |
| 7     | `PageTypeSystem`    | Per-table system page; role unidentified. Two occur in each table's page run, and the table's catalog entry names one of them |
| 8     | `PageTypeSchema`    | Column definitions (zlib-compressed internal file)                                                                            |
| 9     | `PageTypeTableInfo` | Table info — the per-table counters                                                                                           |
| 10    | `PageTypeData`      | Data page: occupancy bitmap plus fixed-size records                                                                           |
| 11    | `PageTypeBlob`      | BLOB storage                                                                                                                  |
| 12    | `PageTypeIndex`     | B-tree index page                                                                                                             |

Only **data pages** record an owner in `ObjectID`. Schema, table info, index and BLOB pages
all carry `0xFFFFFFFF`.

Each table occupies a fixed six-page run — two type-7, then schema, table info, its internal
record-page index, then its first data page — and the run's position is the only thing tying
three of those to their table, apart from the catalog naming two of them outright. A **user**
index is not in the run; it is attributed to a table by the data pages its leaf entries point
at.

## Page `State`

`State` at `ABSP + 4` is **seeded randomly** when a page is allocated. Across the corpus's 663
live pages it is uniform in `[0, 2^30)`, and 29 groups of byte-identical page payloads carry
different `State`s, so it is neither a content hash nor a fixed sequence. Once allocated, it
is incremented by one on every write to the page.

`0x7FFFFFFF` is the tombstone value: `DROP TABLE` sets it on every page it frees, leaving the
page's type, owner and contents intact.

**Pages 0 and 1 are the exception — their `State`s are counters, not seeds.** Page 0's counts
one bump per bit set in the Page Free Space map. Page 1's counts one bump per Extent
Allocation Map entry that _changed value_.

## The allocation maps

Pages 0 and 1 each carry a bitmap in their payload, and between them they are the free list.
The names are the engine's own — `ABSDiskEngine.dcu` exports `GetPageUsageFromPFS`,
`ABS_PAGE_IS_FREE`, `ABS_EXTENT_IS_FREE`, `ABS_EXTENT_IS_PARTIAL_USED` and
`ABS_EXTENT_IS_FULL`.

| Structure                       | Where             | Layout                                                     |
| ------------------------------- | ----------------- | ---------------------------------------------------------- |
| **PFS** — Page Free Space       | payload of page 0 | 1 bit per page, LSB first, set while the page is allocated |
| **EAM** — Extent Allocation Map | payload of page 1 | 2 bits per extent of `PageCountInExtent` pages             |

EAM values are `0` free, `1` partially used, `3` full.

The two maps are checked differently, and the asymmetry is a property of the format: **a PFS
bit is exact, but the EAM is only ever downgraded.** Freeing pages turns a full extent into a
partial one and never turns a partially used extent back into a free one, so "partial" claims
nothing and only "full" and "free" can be asserted.

For a large enough file the PFS and EAM recur rather than being only pages 0 and 1 —
`ABSDiskEngine.hpp` exports `PfsPageNoForPageNo`, `EamPageNoForPageNo` and `IsPagePfsOrEam`.
Nothing in this corpus is close: one PFS payload addresses 32 448 pages at a 4096-byte page
size. This package refuses to grow past that boundary (`ErrDatabaseTooLarge`).

Freed pages are reused **lowest first**.

## Object ids

Object ids come from one database-wide sequence, and are handed out one per table, one per
column, one per index and one per constraint. `LastObjectID` in the database header holds the
last one issued. This is why a three-table database has table ids 1, 4 and 8 rather than
1, 2 and 3: the first table takes 1 and gives 2 and 3 to its two columns, the second takes 4
and gives 5, 6 and 7 to its three.

The ids are fully determined by the **order** in which objects are created, which is what
makes a rebuild reproducible.

## Growth

`ABSDiskEngine.hpp` names the machinery: `TABSDatabaseFreeSpaceManager` exports
`GetPage(DesiredStartPageNo)`, `FindAndReusePage`, `AddNewPageAndExtentFile`,
`AddPagesStep`/`DelPagesStep`, `TruncateFile` and `FindLastUsedPageNo`, over
`TABSDiskPageManager.ExtendFile(PageCount)`.

**The rule is `ceil(shortfall / PageCountInExtent)` whole extents**, in a single extension —
never a loop of one-page ones. Measured over two geometries, since one geometry cannot
distinguish the extent size from a constant 8:

| geometry | free | needed | shortfall | grew by              |
| -------- | ---- | ------ | --------- | -------------------- |
| 4096 / 8 | 0    | 1      | 1         | +8 — one extent      |
| 4096 / 8 | 0    | 5      | 5         | +8 — one extent      |
| 2048 / 4 | 1    | 6      | 5         | +8 — **two** extents |
| 2048 / 4 | 3    | 6      | 3         | +4 — one extent      |

The whole of a one-page growth diff:

```
hdr+30  TotalPageCount   30 -> 38
hdr+34  LastUsedPageNo   29 -> 30
hdr+38  State            13 -> 14
hdr+376 LastObjectID     15 -> 16
p0 +4   State            30 -> 31      (page 0 advances once per PFS bit set)
p0 +43  PFS payload[3]   0x3F -> 0x7F  (the single bit for page 30)
```

Two consequences an implementation would otherwise guess wrong:

- The appended pages are **pure zeros**. No `ABSP` header is stamped until something allocates
  them.
- **Page 1, the EAM, is not written at all**, and its `State` does not move. A new extent is
  already zero in the existing payload, so there is nothing to write.

## Shrinking

The engine shortens a file in exactly one observed situation: compaction, which ends by
truncating to `LastUsedPageNo + 1`, floored at the six pages of a fresh database. Dropping the
last table leaves the same six pages.

Dropping a table whose pages are the file's highest does **not** shorten it — trailing free
pages are left in place.
