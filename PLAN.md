# go-absolute-database — Implementation Plan

Read-only (later: read-write) Go library for ComponentAce **Absolute Database** `.abs` files.

## Background

Absolute Database is a single-file embedded database engine for Delphi (by ComponentAce). It replaced the Borland Database Engine (BDE) and is used by several commercial Windows applications — notably **SoundPlan**, the dominant DACH noise calculation tool, which stores all result tables, train type catalogues, address lists, and attribute tables as `.abs` files.

No public specification of the binary format exists. This library is based on reverse-engineering of real `.abs` files (SoundPlan 7.x/8.x output, Absolute Database versions 5.13–7.94) and the official Absolute Database documentation at <https://www.componentace.com/help/absdb_manual/>.

### Official documentation references

- [Absolute Database Manual](https://www.componentace.com/help/absdb_manual/absdbmanual_content.htm)
- [Field Data Types](https://www.componentace.com/help/absdb_manual/supporteddatatypes.htm)
- [Maximum Capacity Specifications](https://www.componentace.com/help/absdb_manual/maximumcapacityspecification.htm)
- [CREATE TABLE syntax](https://www.componentace.com/help/absdb_manual/createtablestatement.htm)
- [Personal Edition download](https://www.componentace.com/download/)

---

## Format knowledge (reverse-engineered)

### File header (page 0)

The first 76 bytes are `TABSDBHeader`, declared verbatim in the SDK's C++Builder
header (`ABSTypes.hpp`). This is **not** guesswork — the layout below is transcribed
from that declaration and verified against every fixture.

| Offset | Size | Type       | Field               | Description                                    |
| ------ | ---- | ---------- | ------------------- | ---------------------------------------------- |
| 0      | 16   | char[16]   | `Signature`         | Magic: `ABS0LUTEDATABASE` (zero, not letter O) |
| 16     | 2    | int16 LE   | `HeaderSize`        | Size of this header (76)                       |
| 18     | 8    | float64 LE | `Version`           | Engine version (5.13, 7.10, 7.61, 7.94)        |
| 26     | 2    | uint16 LE  | `PageSize`          | Page size in bytes (observed 4096, max 65536)  |
| 28     | 2    | uint16 LE  | `PageCountInExtent` | Pages per allocation extent (observed 8)       |
| 30     | 4    | int32 LE   | `TotalPageCount`    | Total pages in the file                        |
| 34     | 4    | int32 LE   | `LastUsedPageNo`    | Highest page number in use                     |
| 38     | 4    | int32 LE   | `State`             | Database state flags                           |
| 42     | 1    | byte       | `WriteChangesState` | Write-in-progress marker                       |
| 43     | 1    | bytebool   | `Encrypted`         | `0x00` = plaintext, `0xFF` = encrypted         |
| 44     | 32   | byte[32]   | `Reserved`          | Zero in all samples                            |

> **Correction (2026-08-28 review).** Earlier revisions of this document described
> offsets 30 and 34 as "total column count" and "user-visible column count", and byte
> 16 as a mode flag `'L'`. That was wrong. There are **no column counts in the file
> header** — column counts come from the schema page (Phase 2). Byte 16 is the low
> byte of `HeaderSize` (76 = 0x4C = `'L'`), which is what made it look like a mode flag.

If `Encrypted` is set, a `TABSCryptoHeader` follows immediately at offset 76 — see
[Encryption](#encryption-solved-and-verified).

### ABSP marker — `TABSDiskPageHeader`

Every page carries a 40-byte `TABSDiskPageHeader` at the **fixed offset `0x17C`
(380)** within the page — not at the page start. Layout transcribed from
`ABSTypes.hpp` and verified in code (`absdb.go:parseDiskPageHeader`):

| Offset (rel.) | Size | Type      | Field        | Notes                                                |
| ------------- | ---- | --------- | ------------ | ---------------------------------------------------- |
| +0            | 4    | char[4]   | `Signature`  | `ABSP`                                               |
| +4            | 4    | int32 LE  | `State`      | Page state flags                                     |
| +8            | 2    | uint16 LE | `PageType`   | See page-type table below                            |
| +10           | 4    | int32 LE  | `NextPageNo` | Chain link; `-1` = end of chain                      |
| +14           | 4    | uint32 LE | `CRC32`      | Page checksum; **`!= 0` ⇒ page is encrypted**        |
| +18           | 1    | byte      | `CRCType`    |                                                      |
| +19           | 1    | byte      | `HashType`   |                                                      |
| +20           | 1    | byte      | `CipherType` | `TABSCryptoAlgorithm` value for this page            |
| +21           | 1    | byte      | `MACType`    |                                                      |
| +22           | 4    | int32 LE  | `ObjectID`   | **Owning table ID** — the key to multi-table support |
| +26           | 4    | int32 LE  | `RecPageNo`  | `RecordID.PageNo`                                    |
| +30           | 2    | uint16 LE | `RecItemNo`  | `RecordID.ItemNo`                                    |
| +32           | 8    | byte[8]   | `Reserved`   |                                                      |

`ObjectID` is parsed today but ignored by every consumer — see Phase 5e.

### Page data extent (corrected)

This is the single most important structural correction from the 2026-08-28 review.

The usable data area of a page is **`pageSize - 40` bytes**: it begins at `0x1A4`
(immediately after the page header), runs to the end of the page, and **wraps around
to physical offset 0**, skipping only the 40-byte header block at `0x17C..0x1A3`.

The implementation currently models the data area as `data[0x1A4:]` only — 3676 bytes
instead of 4056 — and therefore **silently drops the trailing records of every full
data page**.

Proof (RREC0011.abs, record size 176, page 11):

```
occupancy bitmap = ff ff 7f  ->  23 records present
3 (bitmap) + 23 * 176        =  4051 bytes required
pageSize - 40                =  4056 bytes available   OK
data[0x1A4:] only            =  3676 bytes available   TOO SMALL -> 3 records lost
```

The primary index independently confirms 30 rows across pages 11 and 12 (23 + 7);
the reader returns 27.

### Page types

`PageType` values confirmed across the whole fixture corpus:

| Value     | Constant            | Role                                                                                                                        |
| --------- | ------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| 2         | `PageTypeSystemDir` | System directory (table catalog lives here — see Phase 5e)                                                                  |
| 3         | `PageTypeFileHdr`   | Page 0                                                                                                                      |
| 8         | `PageTypeSchema`    | Schema metadata (zlib-compressed internal file)                                                                             |
| 10        | `PageTypeData`      | Data page (occupancy bitmap + fixed-size records)                                                                           |
| 11        | —                   | BLOB storage page                                                                                                           |
| 12        | `PageTypeIndex`     | B-tree index page                                                                                                           |
| 4,5,6,7,9 | —                   | **Not yet identified.** Present in every file; the CLI prints them as `Type_N`. Includes EAM / PFS / free-space structures. |

Structural pages (1, 3, 5, 6) always carry `CRC32 == 0` and are never encrypted.

### Record structure (data pages) — DERIVED, not guessed

A data page is laid out as:

```
[ occupancy bitmap ][ record 0 ][ record 1 ] ... [ record N-1 ]
```

All of it is computable from the schema. **No search or heuristic is required:**

```
nullFlagBytes  = ceil(numColumns / 8)          # spare high bits are set to 1
fieldDataSize  = sum(fieldStoreSize(col) for col in columns)
recordSize     = nullFlagBytes + fieldDataSize # no trailer, no padding
bitmapBytes    = ceil(recordsPerPage / 8)
recordsPerPage = floor((pageSize - 40 - bitmapBytes) / recordSize)
```

Each record begins with `nullFlagBytes` of null flags (bit set = NULL), followed by
the fields in column order at their natural fixed widths. The page's leading occupancy
bitmap marks which slots hold live records — that is how deletion is represented; there
is no per-record tombstone.

Verified against four files:

| File     | cols | nullFlagBytes | recordSize | bitmapBytes | records/page |
| -------- | ---- | ------------- | ---------- | ----------- | ------------ |
| TS03     | 9    | 2             | 99         | 5           | 40           |
| RREC0011 | 20   | 3             | 176        | 3           | 23           |
| RCFQ0011 | 7    | 1             | 47         | 11          | 86           |
| RMPA0011 | 31   | 4             | 286        | 2           | 14           |

The spare high bits of the null-flag byte are set to `1`, which gives a **free
self-check**: if the computed `nullFlagBytes` is right, those bits are set.

> **Corrections (2026-08-28 review).** Two claims in earlier revisions were wrong:
>
> 1. _"Records end with a 2–3 byte trailer containing a record sequence number or
>    flags."_ There is no trailer. The bytes previously read as a trailer were the
>    null-flag prefix of the **next** record.
> 2. The implementation derives this layout with a **576-candidate brute-force search**
>    (`detectRecordLayout`, `reader.go`) scored by "do these bytes look plausible?",
>    and hardcodes `nullFlagBytes = (numColumns + 2 + 7) / 8`. The `+2` is a
>    fixture-fitted constant with no basis, and it makes the search space **provably
>    unable to represent the correct answer** whenever `numColumns mod 8` is 0 or 7 —
>    25% of all schemas. See Phase 5c.

### B-tree index pages

Index pages start with an 18-byte `TABSBTreePageHeader` (`ABSBTree.hpp`), verified in
`index.go:parseBTreeHeader`:

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

**Entry stride differs by node kind** — this is a known defect in the current code:

| Node kind | Entry layout                                     | Stride        |
| --------- | ------------------------------------------------ | ------------- |
| Leaf      | key + `RecPageNo` (int32) + `RecItemNo` (uint16) | `keySize + 6` |
| Internal  | key + child `PageNo` (int32)                     | `keySize + 4` |

`readBTreeEntries` uses `keySize + 6` for **both**, so any multi-level tree decodes to
garbage. Verified on RCFQ0011 page 10: at a 9-byte stride the three entries decode
cleanly as `(key=0 → page 16, key=185 → page 17, key=369 → page 20)`; at 11 bytes they
are nonsense. `FindByPrimaryKey` consequently fails on every row of RCFQ0011 and
RMPA0011. `findLeftmostLeaf` works only by accident (entry 0's page number happens to
sit at the same offset under both layouts). See Phase 5c.

The leaf sibling chain (`RightPageNo`) is decoded correctly, and a full leaf scan is
currently the **most trustworthy oracle in the codebase** — it yields exact row counts
and record IDs. Phase T uses it to validate the record reader.

### Observed field-to-offset mapping (superseded)

An earlier revision documented a hand-derived byte map of TS03.abs records ("13 user
columns", a 19-byte name field, a 3-byte trailer). It was based on the incorrect
trailer theory above and on a wrong column count — TS03 has **9** columns, not 13.
It has been removed rather than corrected: the formula in
[Record structure](#record-structure-data-pages--derived-not-guessed) supersedes it and
is verified across the whole corpus.

### Known data types (from official docs)

| Type       | Delphi ftType    | Storage                                             | SQL aliases                    |
| ---------- | ---------------- | --------------------------------------------------- | ------------------------------ |
| AutoInc    | ftAutoInc        | uint32 (4 bytes)                                    | AUTOINC                        |
| BLOB       | ftBlob           | pointer to BLOB area                                | BLOB                           |
| Bytes      | ftBytes          | fixed byte array                                    | BYTES(n)                       |
| Currency   | ftCurrency       | 8 bytes (Delphi Currency = int64 / 10000)           | CURRENCY, MONEY                |
| Date       | ftDate           | **4 bytes, int32 LE** (days, epoch 0001-01-01 = 1)  | DATE                           |
| DateTime   | ftDateTime       | **8 bytes, `TABSDateTime{int32 Date; int32 Time}`** | DATETIME                       |
| Extended   | ftExtended       | 10 bytes (80-bit extended)                          | EXTENDED                       |
| Float      | ftFloat          | 8 bytes (float64)                                   | FLOAT, DOUBLE, REAL, NUMERIC   |
| FmtMemo    | ftFmtMemo        | pointer to BLOB area                                | FMTMEMO                        |
| Graphic    | ftGraphic        | pointer to BLOB area                                | GRAPHIC                        |
| GUID       | ftGUID           | 16 bytes                                            | GUID                           |
| Integer    | ftInteger        | 4 bytes (int32)                                     | INTEGER, INT, INT32            |
| LargeInt   | ftLargeInt       | 8 bytes (int64)                                     | LARGEINT, BIGINT, INT64        |
| Logical    | ftBoolean        | 2 bytes (Delphi WordBool)                           | LOGICAL, BOOLEAN, BOOL, BIT    |
| Memo       | ftMemo           | pointer to BLOB area                                | MEMO, CLOB, TEXT               |
| SmallInt   | ftSmallint       | 2 bytes (int16)                                     | SMALLINT, INT16                |
| String     | ftString         | fixed bytes (up to 65500)                           | STRING(n), CHAR(n), VARCHAR(n) |
| Time       | ftTime           | **4 bytes, int32 LE** (milliseconds since midnight) | TIME                           |
| TimeStamp  | ftTimeStamp      | 8 bytes                                             | TIMESTAMP                      |
| VarBytes   | ftVarBytes       | length-prefixed bytes                               | VARBYTES(n)                    |
| WideMemo   | ftBlob (subtype) | pointer to BLOB area                                | WIDEMEMO                       |
| WideString | ftWideString     | fixed UTF-16LE (up to 65500)                        | WIDESTRING(n), NCHAR(n)        |
| Word       | ftWord           | 2 bytes (uint16)                                    | WORD, UNSIGNEDINT16, UINT16    |

> **Correction (2026-08-28 review).** Earlier revisions claimed Date/Time/DateTime are
> stored as Delphi `TDateTime` (float64 days since 1899-12-30). They are not. The SDK
> declares `TABSDateTime { int Date; int Time }` (`ABSTypes.hpp`), i.e. two int32s:
> `Date` = days with 0001-01-01 as day 1 (the BDE `ftDate` convention,
> `Trunc(TDateTime) + 693594`), `Time` = milliseconds since midnight. `reader.go`
> already implements this correctly; only this document was wrong.
>
> **Untested types.** Only six base types occur anywhere in the fixture corpus:
> Int32, Varchar, Double, Logical, Blob, Clob. Everything else in the table above has
> **zero rows of coverage** — LargeInt, SmallInt, Word, Currency, WideString, GUID,
> Extended, VarBytes, Bytes, Single, TimeStamp, Date, Time, DateTime. Two are known to
> be actively broken: `Single` is decoded as float64, and `WideString` is truncated to
> its first character (see Phase 5d).

### Capacity limits (from official docs)

| Limit                   | Value                               |
| ----------------------- | ----------------------------------- |
| Max database size       | 32 TB                               |
| Max pages per file      | 2,147,483,647                       |
| Max bytes per page      | 65,536                              |
| Max tables per database | 2,147,483,647                       |
| Max rows per table      | 2,147,483,647                       |
| Max columns per table   | 65,000                              |
| Max columns per index   | 10,000                              |
| Max string field size   | 64,000 bytes (limited by page size) |
| Max BLOB field size     | 2 GB                                |
| Max bytes per row       | 65,400 (limited by page size)       |
| Max identifier length   | 255 characters                      |

### Encryption (SOLVED and verified)

`ABSCipher` is **DEC — the Delphi Encryption Compendium** (Hagen Reddmann), which is
open source. The class tree in `ABSCipher.hpp` (`TCipher_Rijndael`, `TCipher_Blowfish`,
`TCipher_1DES`, `THash_RipeMD128`, `THash_RipeMD256`,
`TCipherMode { cmCTS, cmCBC, cmCFB, cmOFB, cmECB, ... }`) identifies it unambiguously.
This means **the algorithms can be read from public source — no decompilation is
required.**

#### `TABSCryptoHeader` (at file offset 76, plaintext)

| Offset | Size | Type      | Field              | Notes                              |
| ------ | ---- | --------- | ------------------ | ---------------------------------- |
| 76     | 2    | int16 LE  | `CryptoHeaderSize` | 280                                |
| 78     | 1    | byte      | `CryptoAlgorithm`  | `TABSCryptoAlgorithm`              |
| 79     | 1    | byte      | `CryptoMode`       | 0 = `cmCTS`                        |
| 80     | 256  | byte[256] | `ControlBlock`     | encrypted; used to verify password |
| 336    | 4    | uint32 LE | `ControlBlockCRC`  | checksum of decrypted ControlBlock |

#### The scheme (verified byte-exactly)

1. **Key** = `RIPEMD-128(password)` or `RIPEMD-256(password)` over the raw AnsiString
   bytes, truncated to the cipher's key size.
2. **IV / initial feedback** = **`0xFF` repeated to the block size** (DEC `InitEnd`),
   reset for every page. _Not_ a zero IV.
3. **Mode** = DEC `cmCTS`:
   `P_i = D(C_i) XOR F_i`, `F_{i+1} = C_i XOR F_i`;
   trailing partial block: `P = C XOR E(F)`.
4. **Ciphers are the standard ones** — Go's `crypto/aes`, `crypto/des` and
   `golang.org/x/crypto/blowfish` work unmodified. No DEC-specific byte swapping.
5. **Encrypted region** = page bytes **`[420, 4096)`** (3676 bytes), i.e. everything
   after the 40-byte `ABSP` header. Page 0 is never encrypted, and the `ABSP` header
   itself stays in the clear.
6. **Is this page encrypted?** `ABSP.CRC32 != 0`.
7. **Password verification** = decrypt `ControlBlock`, compute CRC32 with the
   **reflected IEEE polynomial `0xEDB88320`, init 0, no final XOR** (_not_ Go's
   `crc32.ChecksumIEEE`, which inverts both ends), compare against `ControlBlockCRC`.

#### Algorithm → hash mapping

Recovered from `ABSDiskEngine.dcu` via go-dede. The `.hpp` headers alone do **not**
carry this mapping — it was the one piece that genuinely needed the DCU.

| Value | Cipher       | Hash           | Key bytes |
| ----- | ------------ | -------------- | --------- |
| 0     | Rijndael-128 | RIPEMD-128     | 16        |
| 1     | Rijndael-256 | **RIPEMD-256** | 32        |
| 2     | DES-Single   | RIPEMD-128     | 8         |
| 3     | DES-Triple   | RIPEMD-128     | 16        |
| 4     | Blowfish     | **RIPEMD-256** | 32        |
| 5     | Twofish-128  | RIPEMD-128     | 16        |
| 6     | Twofish-256  | **RIPEMD-256** | 32        |
| 7     | Square       | RIPEMD-128     | 16        |

Blowfish using RIPEMD-**256** is the non-obvious entry, and is why earlier Blowfish
attempts failed.

#### Verification status

Independently reproduced from scratch against the local fixtures, password **`"Bla"`**:

```
Addresses-Rijndael_128.abs   ControlBlock CRC 0xffac4819 == 0xffac4819   8/8 pages byte-identical
Addresses-DES_Single.abs     ControlBlock CRC 0xd9f59b5b == 0xd9f59b5b   8/8 pages byte-identical
Addresses-Blowfish.abs       verified separately (needs RIPEMD-256)
wrong passwords ("bla", "wrong")                                          correctly rejected, 0/8
```

"Every encrypted page decrypts byte-identically to the plaintext `Addresses.abs`,
including the trailing partial block."

#### Caveats

- The three encrypted fixtures are encrypted copies of an **empty table**. They validate
  header and schema decryption but can **never** validate encrypted _record_
  decryption. A fixture with rows and a known password is needed.
- Rijndael-256, Twofish-128/256, Square and DES-Triple are untested — no fixtures.
  AES-256 and 3DES are stdlib; Twofish and Square would need Go implementations
  (Square is DEC-specific and would have to be ported).
- The `ABSP.CRC32` page checksum does not yet reproduce over the decrypted data
  (computed `8316267a` vs stored `6b705972`, consistent across all three files, so it
  _is_ over plaintext — probably a different length or range). Cosmetic: it affects
  optional page-integrity checking only, not decryption.

### BLOB compression

Algorithms: None, ZLIB, BZIP, PPM. Compression levels 1–9.

---

## Status of the original unknowns

| #   | Unknown (as originally posed)  | Status                                                                                                                                                                                                                                                               |
| --- | ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Complete page header structure | **Resolved.** `TABSDiskPageHeader`, all 40 bytes, from `ABSTypes.hpp`.                                                                                                                                                                                               |
| 2   | Schema storage location        | **Mostly resolved.** Type-8 page, zlib internal file. But only ~26% of the blob is decoded — ~50% is skipped by a pattern search for `7F 00 <baseType> FF`, and ~24% (index definitions, constraints, table name) is never read.                                     |
| 3   | Page allocation map            | **Open.** Page types 4/5/6/7/9 are the EAM/PFS/free-space structures and remain unidentified. Not needed for reading.                                                                                                                                                |
| 4   | BLOB block layout              | **Mostly resolved.** Type-11 pages, `TABSDiskBLOBHeader { Word BlobID; int NumBlocks; int64 CompressedSize; int64 UncompressedSize }` = **22 bytes packed**. The code assumes 24 and ignores `ItemNo`, so multiple BLOBs per page and >1-page BLOBs are unsupported. |
| 5   | Multi-table databases          | **Resolved in principle, unimplemented.** `ABSP.ObjectID` tags every page with its owning table, and `TABSTableListItem { TableName; TableID; MetaDataFilePageNo; ... }` in the type-2 System Directory is the catalog. See Phase 5e.                                |
| 6   | Record deletion                | **Resolved.** A per-page occupancy bitmap at the head of the data area. No per-record tombstone.                                                                                                                                                                     |
| 7   | Index page child pointers      | **Resolved.** Leaf stride `keySize+6`, internal stride `keySize+4`. The code uses `+6` for both — see Phase 5c.                                                                                                                                                      |
| 8   | Record trailer bytes           | **Dissolved — there is no trailer.** Those bytes are the next record's null-flag prefix.                                                                                                                                                                             |
| 9   | Version differences            | **Open.** Corpus spans 5.13, 7.10, 7.61, 7.94. No structural difference has been isolated; the RRAI/RRAD problem originally attributed to v7.61 turned out to be the `nullFlagBytes` bug, not a version difference.                                                  |

### Still genuinely unknown

- Page types 4, 5, 6, 7, 9.
- The ~24% tail of the schema page: index definitions, constraints, table name.
- The `ABSP.CRC32` page-checksum input range.
- Whether any `.abs` in the wild uses a page size other than 4096.

---

## Review findings (2026-08-28) — current state

A full review was run against the committed tree. Summary of what it changed about the
picture above:

**The good.** Everything backed by the SDK's C++ headers is correct and
SDK-faithful: file header, page header, B-tree page header, internal file header, both
field-type enums, `TABSDateTime`. RIPEMD-128 passes all seven official vectors. The
B-tree leaf scan is correct. Encryption is now fully solved (above). 16 of 20 fixtures
dump without error.

**The problems, in priority order.**

1. **The record reader silently returns wrong or missing data.** Two independent bugs
   (Phase 5c). Nothing in the API signals it.
2. **Robustness policy is unmet.** `go test -fuzz` reaches a 4.4 GB OOM in 11 seconds;
   nine distinct panics, two infinite loops and three unbounded allocations were
   reproduced. There are zero fuzz tests (Phase 5d).
3. **The test suite is red on `main`** — five failures from fixture drift, and
   `testdata/` is gitignored so a fresh clone cannot run 36 of 44 tests. There is no
   CI, which is why this went unnoticed (Phase T).
4. **`crypto.go` ships an implementation now known to be wrong** in four ways: zero IV,
   plain CBC, whole-page decryption, RIPEMD-128 only (Phase 6).
5. **Packaging.** The `v0.1.0` tag is **orphaned** — it shares no ancestry with `main`
   (history was rewritten during the `meko-tech` → `cwbudde` module rename), yet
   `../Aconiq` requires exactly `v0.1.0` with no `replace` directive. `go.mod` is not
   tidy and carries `golang.org/x/crypto` as an unused dependency.

### Legal basis and repository hygiene

Reverse engineering for interoperability is permitted under EU Directive 2009/24/EC
Art. 6, and Art. 8 voids contractual terms that purport to forbid it — so the ComponentAce
EULA's anti-reverse-engineering clause is very likely unenforceable here. **Redistribution
is a separate matter that Art. 6 does not cover.** Accordingly:

- `legacy/` (57 MB of ComponentAce binaries, help files and EULAs) and `testdata/`
  (SoundPlan-derived fixtures) are **gitignored and have never been committed** — verified.
  Keep it that way; ideally move `legacy/` outside the repo tree so no ignore rule is
  load-bearing.
- Before any public release, form a deliberate view on the EULA clause "You may not
  develop a component, a library or a developer's toolkit using this Software", which
  Art. 8 does **not** neutralise, and add a `NOTICE` attributing the format to ComponentAce.
- Confirm the provenance of the `testdata/` fixtures (they contain German street
  addresses) or replace them with synthetic files — which would also make the test
  suite runnable by anyone. See Phase T.

> **Checkbox legend:** `[x]` done and verified · `[~]` partially done or unvalidated ·
> `[!]` claimed done but demonstrably wrong · `[ ]` not started.

## Phase 1 — File header and page navigation (read-only)

**Goal:** Open an `.abs` file, parse the file header, and navigate pages.

### Steps

- [x] Define `File` struct: holds file handle, parsed header, page size
- [x] Parse file header (magic validation, version, page size, column counts)
- [x] Implement `ReadPage(pageNum int) ([]byte, error)` — read a single page by number
- [x] Implement `PageCount() int` — total pages in file
- [x] Parse ABSP marker from any page (offset, column count, page type flags)
- [~] Classify pages: scan all pages and categorize by ABSP signature and content patterns
  — only 5 of ~12 observed page types are named; 4/5/6/7/9 print as `Type_N`
- [x] Write tests against real `.abs` files (TS03.abs, RREC0011.abs, Addresses.abs)
- [!] Create `testdata/` with small representative `.abs` files
  — fixtures exist locally but `testdata/` is **gitignored**, so a fresh clone runs none of them (Phase T)

### API sketch

```go
type File struct { /* unexported fields */ }

func Open(path string) (*File, error)
func (f *File) Close() error
func (f *File) Version() float64
func (f *File) PageSize() int
func (f *File) PageCount() int
func (f *File) ReadPage(n int) (Page, error)

type Page struct {
    Number int
    Data   []byte
    Header *DiskPageHeader // nil if no ABSP marker found
}

// DiskPageHeader mirrors TABSDiskPageHeader; see the format section above.
type DiskPageHeader struct {
    State      int32
    PageType   uint16
    NextPageNo int32
    CRC32      uint32
    CipherType byte
    ObjectID   int32
    RecPageNo  int32
    RecItemNo  uint16
    // ... CRCType, HashType, MACType
}
```

---

## Phase 2 — Schema extraction

**Goal:** Read column definitions (names, types, sizes) from the database file.

### Steps

- [~] Locate the schema/catalog page(s) — page type 8, zlib-compressed internal file
  — takes the **first** type-8 page only; schema-page chaining unsupported
- [~] Decode column definition records: name (string), type (enum), size (uint32), flags
  — name/type/size decoded; the **flags byte is read and discarded**, so `Column` has no `Nullable`.
  Only ~26% of the schema blob is parsed; ~50% skipped by pattern search, ~24% never read
- [x] Map Delphi `ftXxx` type codes to Go `FieldType` enum (TABSBaseFieldType + TABSAdvancedFieldType)
- [~] Build `TableSchema` struct: column definitions
  — actual struct is `{Columns []Column}`; the `Name` and `Indexes` fields promised in the API sketch are missing
- [ ] Handle multi-table databases (table catalog enumeration) — deferred, single-table works
- [x] Test against TS03.abs (9 columns: AutoInc + String + 5×Double + Memo + Graphic)
- [~] Test against RREC0011.abs (20 columns: Integer + String + Boolean + Double)
  — test asserts only `len(Columns) != 0`; it cannot fail on wrong data
- [~] Test against Addresses.abs (19 columns) — asserts only columns 0 and 1.
  Note the fixture is now **v7.94 and an empty table** (0 rows), which is why
  `TestAddressesHeader` fails expecting 7.10

### API sketch

```go
type FieldType int

const (
    FieldAutoInc FieldType = iota
    FieldString
    FieldInteger
    FieldSmallInt
    FieldWord
    FieldLargeInt
    FieldFloat
    FieldCurrency
    FieldDate
    FieldTime
    FieldDateTime
    FieldLogical
    FieldBLOB
    FieldMemo
    FieldBytes
    FieldGUID
    FieldWideString
    FieldWideMemo
    FieldExtended
    // ...
)

type Column struct {
    Name     string
    Type     FieldType
    BaseType BaseFieldType
    Size     int       // for String/Bytes: max length; 0 for fixed-size types
    Position int       // 0-based column index
    // Nullable is NOT implemented — the flags byte is parsed and discarded.
}

type TableSchema struct {
    Columns []Column
    // Name and Indexes are NOT implemented — Name needs the System Directory
    // (Phase 5e), Indexes needs the untouched tail of the schema page (Phase 2).
}

// Actual API today (single-table only):
func (f *File) Schema() (*TableSchema, error)

// Planned for Phase 5e — a BREAKING change to the above:
func (f *File) Tables() ([]string, error)
func (f *File) Schema(table string) (*TableSchema, error)
```

---

## Phase 3 — Record reading

**Goal:** Iterate over data records and read field values.

### Steps

- [~] Locate data pages for a table (page type 10 scan)
  — collects **every** type-10 page in the file, ignoring `ABSP.ObjectID` (breaks multi-table)
- [!] Calculate record size from schema
  — **not calculated, guessed.** 576-candidate brute-force search with a
  plausibility score, plus a wrong hardcoded `nullFlagBytes`. See Phase 5c
- [x] Implement record iteration: walk data pages, extract fixed-size records
- [x] Implement field value deserialization for numeric types:
  - [x] Integer (int32 LE)
  - [~] LargeInt (int64 LE) — implemented, **zero** columns of this type in the corpus
  - [x] Float (float64 LE)
  - [x] AutoInc (uint32 LE)
  - [!] SmallInt (int16 LE) — **broken**: `Int()` reads 4 bytes for a 2-byte field
  - [!] Word (uint16 LE) — **broken**: same 4-byte read over a 2-byte field
  - [ ] Currency (int64 LE / 10000) — type defined, untested
- [x] Implement String field deserialization (Windows-1252 → UTF-8, null-terminated)
- [!] Implement WideString deserialization (UTF-16LE) — **broken**, not merely untested:
  `String()` stops at the first `0x00` and decodes Windows-1252, so UTF-16LE `"Hallo"`
  returns `"H"`. The CLI prints this silently. See Phase 5d
- [~] Implement Date/Time/DateTime deserialization (`TABSDateTime` int32 pair → Go `time.Time`)
  — implementation is correct, but **never exercised**: the only DateTime column in the
  corpus is in `Addresses.abs`, which has no rows
- [x] Implement Logical/Boolean deserialization (WordBool)
- [ ] Implement GUID deserialization — deferred
- [x] Handle null values (null flag bitmask per record)
- [x] Skip deleted records — via the **per-page occupancy bitmap**, not "zero null flags"
      (the original description in this plan was wrong; the implementation is right)
- [~] Test: read all 18 records from TS03.abs
  — asserts `count >= 15`, `name != ""` and `!IsNaN(sba)`; no value is checked against a known source
- [!] Test: read receiver results from RREC0011.abs
  — **the true row count is 30, not 27.** "27" is the truncated count produced by the
  page-extent bug; it was recorded here as if validated. The test asserts no count at all
- [ ] Test: read contributions from RCON0011.abs (**36** columns, **300** records) — deferred.
      Currently returns 275 rows (Phase 5c bug 2)

### API sketch

```go
type Reader struct { /* unexported fields */ }

func (f *File) OpenTable() (*Reader, error)   // actual — no table argument
// func (f *File) OpenTable(name string) (*Reader, error)  // Phase 5e, breaking

func (r *Reader) Schema() *TableSchema
func (r *Reader) Next() bool
func (r *Reader) Err() error
func (r *Reader) Record() Record

type Record struct { /* unexported fields */ }

// NOTE (Phase 5d): these accessors currently panic on an out-of-range col or a
// short trailing field, and Reader.Record() mutates iteration state — calling it
// twice per Next() slices out of range. Both need fixing before a stable tag.
func (rec Record) IsNull(col int) bool
func (rec Record) String(col int) string
func (rec Record) Int(col int) int32
func (rec Record) Int64(col int) int64
func (rec Record) Float(col int) float64
func (rec Record) Bool(col int) bool
func (rec Record) Time(col int) time.Time
func (rec Record) Bytes(col int) []byte
```

---

## Phase 4 — BLOB and compression support (read-only)

**Goal:** Read BLOB, Memo, Graphic, FmtMemo, and WideMemo fields.

### Steps

- [~] Investigate BLOB block storage layout — page type 11
  — SDK says `TABSDiskBLOBHeader` is **22 bytes packed**; the code assumes 24.
  `BlobRef.ItemNo` is parsed and never used, so "one BLOB per page" is an assumption, not a finding
- [x] Decode BLOB pointer format in records — 6 bytes: PageNo(int32) + ItemNo(uint16)
- [ ] Implement BLOB block reading and chaining for multi-page BLOBs
      — **not working.** `readBlobChain` is never reached (`NextPageNo` is always -1); the
      fallback returns _the entire rest of the page_. Measured on RPDG0011: 4 of 60 BLOBs come
      back as exactly 3652 bytes and are the only 4 that violate the file's own
      multiple-of-12 invariant. BLOBs larger than one page cannot be read
- [x] Implement ZLIB decompression for compressed BLOBs
- [ ] Implement BZIP2 decompression (if encountered in test data) — deferred, no test data
- [x] Add `Record.Blob(col int) ([]byte, error)` method
- [x] Add `Record.Memo(col int) (string, error)` method (Memo = text BLOB)
- [~] Test against RPDG0011.abs — 30 records, 60 non-null BLOBs
  — checks only `len(data) != 0`; the multiple-of-12 invariant is asserted for record 1 only,
  which is why the 4 broken BLOBs slip through

---

## Phase 5 — Index reading (read-only)

**Goal:** Read B-tree indexes for efficient key-based lookups.

### Steps

- [x] Decode B-tree node page structure — TABSBTreePageHeader (18 bytes: IsRoot, IsLeaf, siblings, KeyPrefixSize, EntryCount)
- [x] Discover indexes by scanning type-12 pages for root nodes
- [~] Classify indexes by key size — heuristic tuned to TS03. RREC0011 has key sizes {4,10} and
  RCON0011 {4,15,10}, so `PrimaryKeyIndex()` returns `ErrNoIndex` on both
- [!] Implement B-tree traversal for single-key lookup
  — **broken for multi-level trees**: internal nodes use stride `keySize+4`, the code uses
  `keySize+6` everywhere. `FindByPrimaryKey` fails 600/600 on RCFQ0011 and RMPA0011
- [x] Implement index scan via leaf chain (RightPageNo horizontal links)
- [x] Implement `FindByPrimaryKey(key int32)` — exact match on AutoInc/RecNo
- [~] Implement `FindByStringKey(value string)`
  — linear scan of the leaf, not binary search; always uses `secondaries[0]` because index
  _definitions_ are never parsed, so it cannot know which column an index covers
- [x] String key comparison handles garbage bytes after null terminator
- [x] Test: look up train type by name in TS03.abs ("EC / IC" → page 13, item 2)
- [x] Test: primary key lookup across TS03 (1–18) and RPDG0011
- [x] Test: full index scan returns 18 sorted entries for both primary and secondary indexes
- [ ] Decode index definition records from schema metadata — deferred (complex serialization)
- [ ] Benchmark: index lookup vs full scan — deferred

---

## Phase 5b — Fix record layout detection for v7.61 emission files (SUPERSEDED by 5c)

> **Retrospective (2026-08-28 review).** This phase fixed the RRAI/RRAD symptom by
> _strengthening the guessing heuristic_ rather than by decoding the layout. That worked
> for those six files but left the underlying cause in place, and the added scoring
> function is what Phase 5c removes. The root cause was never a v7.61 page-format
> variant — it was the `nullFlagBytes` `+2` fudge factor plus the too-small page extent.
> Keep this section as history; act on Phase 5c.

**Goal:** The auto-detect heuristic in `detectRecordLayout` (reader.go) fails for certain v7.61 `.abs` files — specifically RRAI*.abs (rail emission) and RRAD*.abs (train emission) files from SoundPlan result directories. These files have schemas with AutoInc + Integer + String + multiple Double columns, but the reader returns zeros/garbage for all numeric fields. RGRP*.abs and RREC*.abs files of the same version work correctly.

**Symptom:** `absdb dump` on RRAI0011.abs shows all-null or garbage values for IDX, ObjID, Railname, and all Double columns. The schema is parsed correctly (12 columns: AutoInc, Integer, String/40, 9×Double). The issue is that `detectRecordLayout` picks incorrect pageHdrSize, nullFlagBytes, or extraBytes for these files, causing field offsets to be wrong.

**Approach:**

- [x] Compare the working v7.61 files (RGRP, RREC) against the broken ones (RRAI, RRAD) to identify what differs in page structure
- [x] Hex-dump the first data page of RRAI0011.abs and manually locate the first record's AutoInc=1 value to determine the correct page header size and record stride
- [x] Improve `detectRecordLayout` to handle the variant page header format, or add a fallback that uses the schema's expected field sizes to validate candidates
- [!] Test against all .abs files in `../Aconiq/interoperability/Schienenprojekt - Schall 03/`
  — **not done.** `RCFQ0011.abs` and `RMPA0011.abs` come from that same corpus, sit in
  `testdata/`, are still broken, and have no test

Implemented in repo with regression coverage for local fixtures: `RRAI0011.abs`, `RRAI0012.abs`, `RRAI0023.abs`, `RRAD0011.abs`, `RRAD0012.abs`, `RRAD0023.abs`. All six still read correctly after the Phase 5c fix was trialled, so 5c is a safe replacement.

**Test files:** `RSPS0011/RRAI0011.abs`, `RSPS0011/RRAD0011.abs`, `RRLK0022/RRAI0022.abs`, `RRLK0022/RRAD0022.abs`

---

## Phase 5c — Record and index layout correctness (HIGHEST PRIORITY)

**Goal:** Replace the record-layout guessing machine with the derivation, and fix the
page extent and B-tree stride. These are data-fidelity bugs: the library currently
returns wrong or missing rows with no error.

### Bug 1 — `nullFlagBytes` is computed with a `+2` fudge factor

`reader.go` hardcodes `(numColumns + 2 + 7) / 8`. Correct is `ceil(numColumns / 8)`.
The `+2` is not merely suboptimal — it puts the correct answer **outside** the search
space whenever `numColumns mod 8` is 0 or 7.

| File     | cols | mod 8     | code            | true | result                                                                       |
| -------- | ---- | --------- | --------------- | ---- | ---------------------------------------------------------------------------- |
| RCFQ0011 | 7    | 7         | 2               | 1    | every field shifted 2 bytes → `SrcNo = 65536` (`1<<16`), `FOT500 = 8.1e-292` |
| RMPA0011 | 31   | 7         | 5               | 4    | garbage from row 2 on; `dump --json` emits invalid JSON                      |
| other 14 | —    | 1,3,4,5,6 | correct by luck | —    | plausible values                                                             |

A one-line change to `ceil(cols/8)` was tested against the full corpus: **zero
regressions**, RCFQ0011 goes from garbage to correct SoundPlan values
(`Tag`/`Nacht`, `Rail`, 44.909 dB), RMPA0011 from invalid JSON to 516 clean rows.

- [ ] `nullFlagBytes = ceil(numColumns / 8)`
- [ ] Validate the result using the spare high bits (they are set to 1 when correct)
- [ ] Delete `detectRecordLayout` and `scoreRecordData` entirely; compute
      `recordSize = nullFlagBytes + fieldDataSize` directly from the schema
- [ ] Remove the now-dead tuning constants (`pageHdr <= 64`, `extra <= 8`, the score
      weights, `1e9`, the 85%-printable rule, `1e±100`)

### Bug 2 — page data extent is 3676 bytes instead of `pageSize - 40`

Trailing records of every full data page are silently dropped.

| File     | true rows | reader rows | lost |
| -------- | --------- | ----------- | ---- |
| RCON0011 | 300       | 275         | 25   |
| RREC0011 | 30        | 27          | 3    |
| RR240011 | 30        | 28          | 2    |

- [ ] Model the data area as `pageSize - 40` bytes, wrapping around the `ABSP` header
      block at `0x17C..0x1A3`
- [ ] `recordsPerPage = floor((pageSize - 40 - bitmapBytes) / recordSize)`

### Bug 3 — B-tree internal-node stride

- [ ] Use `keySize + 4` for internal nodes and `keySize + 6` for leaves in
      `readBTreeEntries`
- [ ] Re-test `FindByPrimaryKey` on RCFQ0011 and RMPA0011 (currently 0/600 on both)
- [ ] Make `FindByStringKey` a binary search rather than a linear scan

### Bug 4 — empty tables are an error

- [ ] `OpenTable()` on a table with no rows (e.g. `Addresses.abs`) returns
      `ErrNoData`; it should return an iterator that yields nothing

**Exit criterion:** reader row count equals index-scan row count for every fixture
(Phase T), and RCFQ0011/RMPA0011 produce correct values.

---

## Phase 5d — Robustness and fuzz-safety

**Goal:** Meet the "No panics" and "Fuzz-safe" policies stated in `CLAUDE.md`, which
are currently unmet. All of the following were reproduced against the committed tree.

### Unbounded resource use

- [ ] Validate `TotalPageCount` against the real file size in `parseHeader` and reject
      negatives — a 148 KB input currently triggers
      `runtime: out of memory: cannot allocate 4429185024-byte block` via
      `ScanPages`' `make([]PageSummary, count)`; a negative value reaches `make()` and
      panics with `makeslice: len out of range`
- [ ] Make `ReadPage` reject reads past EOF instead of swallowing `io.EOF`; it
      currently fabricates zero-filled pages, which is what converts a bogus header
      field into an OOM rather than an error
- [ ] Add cycle detection to the BLOB page chain — a self-referential `NextPageNo`
      grows to 7.3 GiB in 3 seconds from a 4 KB file
- [ ] Add cycle/depth guards to all four B-tree walks (`scanLeaves`, `searchBTree`,
      `searchBTreeString`, `findLeftmostLeaf`) — a cyclic leaf chain loops forever
- [ ] Bound both zlib readers with `io.LimitReader` using the already-parsed
      decompressed size — a 64 KB page currently expands to 157 MiB. The size field is
      parsed and then discarded in two places (`schema.go`, and a parameter literally
      named `_ int64` in `blob.go`)

### Panics on malformed input

- [ ] `blob.go` — the guard tests `page.Header == nil` and then dereferences
      `page.Header.PageType` **inside that same branch**. Same shape in `readBlobChain`
- [ ] `blob.go` — `hdr.CompressedSize` is a raw int64; a negative value reaches
      `compressedData[:hdr.CompressedSize]` → `slice bounds out of range [:-1]`
- [ ] `index.go` — `entries[0]` on an empty internal node
- [ ] `index.go` — `KeyPrefixSize == 0` reaches `makeStringKey` → index out of range
- [ ] Bounds-check **every** `Record` accessor: validate `col` is in range and that
      `off + width <= len(fieldData)`. All of `Int`, `Int64`, `Float`, `Bool`, `Time`,
      `Bytes`, `String`, `BlobRef` currently panic on a short trailing column or an
      out-of-range index — and this is reachable from the shipped CLI, not just misuse

### Type-decoding bugs

- [ ] `Single` (4 bytes) is decoded with `math.Float64frombits` over 8 bytes — always
      wrong, and reads into the next field
- [ ] `Int()` reads 4 bytes for 2-byte `SmallInt`/`Word` — wrong value and wrong sign
      extension
- [ ] `WideString`/`WideChar` stop at the first `0x00` and decode as Windows-1252, so
      UTF-16LE `"Hallo"` returns `"H"`. The CLI routes these types here and prints
      silently wrong data

### Fuzzing

- [ ] Add `FuzzOpen`, `FuzzParseSchema`, `FuzzReadBlob` seeded from `testdata/*.abs`
- [ ] Add a `go test -fuzz` job to CI with a time budget

---

## Phase 5e — Multi-table support

**Goal:** Support `.abs` files containing more than one table. Everything needed is
already on disk and partly parsed.

Today `Schema()` returns the first type-8 page and `OpenTable()` collects **all**
type-10 pages regardless of owner, so a second table's rows would be decoded with the
first table's schema and stride and emitted from the same iterator — silently wrong.
All 20 fixtures are single-table (`ObjectID == 1` everywhere), so no test would notice.

- [ ] Parse the type-2 System Directory page and its `TABSTableListItem` entries
      (`TableName`, `TableID`, `MetaDataFilePageNo`)
- [ ] Implement `Tables() ([]string, error)`
- [ ] Filter data, index and BLOB pages by `ABSP.ObjectID`
- [ ] Add `Name` to `TableSchema`
- [ ] Decide the API: `Schema(table string)` / `OpenTable(name string)` per the
      original sketch is a **breaking change** to the current no-argument form, so it
      should land before `../Aconiq` pins a stable version

---

## Phase T — Test suite, fixtures and CI (do this alongside 5c)

**Goal:** Make the suite green, runnable by anyone, and capable of catching the
Phase 5c/5d class of bug. Without this, nothing else stays fixed.

### Get to green

- [ ] `TestAddressesHeader` expects version 7.10; the fixture is now v7.94
- [ ] All four crypto tests target `testdata/Addresses.abs`, which is the
      **unencrypted** fixture. Repoint them at `Addresses-{Rijndael_128,Blowfish,DES_Single}.abs`
      with the now-known password `"Bla"`
- [ ] Replace `TestVerifyPassword`'s "try both casings and pass if either works" with a
      real assertion — a verification test that does not know its own input validates nothing

### Make fixtures available

- [ ] `testdata/` is gitignored, so 36 of 44 tests fail on a fresh clone. Either commit
      small fixtures or generate synthetic ones in-test. **Confirm provenance first** —
      the current files contain German street addresses from a SoundPlan project
- [ ] Add an encrypted fixture that actually **contains rows**, with a documented
      password. The three existing encrypted fixtures are copies of an empty table and
      can never validate record decryption
- [ ] Make the test helper `t.Skip` on a missing fixture rather than `t.Fatalf`

### Add the missing oracle

- [ ] **Cross-check the record reader against the index scan for every fixture.** The
      B-tree leaf scan already yields exact row counts and record IDs and is
      independent of the record decoder. Nothing does this today, which is the direct
      reason all of Phase 5c went unnoticed
- [ ] Replace shape assertions (`!= ""`, `!IsNaN`, `count >= 15`, `len(Columns) != 0`)
      with expected values
- [ ] Assert the multiple-of-12 invariant for **all** RPDG BLOBs, not just record 1
- [ ] Cover `readBlobChain`, `decompressBlob` and the decryption path — currently 0%
      and 10.7% respectively

### CI

- [ ] Add `.github/workflows` running `just ci` — there is **no CI at all** today
- [ ] Fix `just check-formatted`: it runs `prettier -w` / `gofumpt -w` / `gci write`, so
      it **rewrites tracked files while "checking"**
- [ ] `go mod tidy` — `golang.org/x/crypto` is required but never imported; `cobra` and
      `x/text` are wrongly marked `// indirect`
- [ ] Decide on the `stdlib only` claim in `CLAUDE.md`: the core read path imports
      `golang.org/x/text/encoding/charmap`. Either inline the 128-entry Windows-1252
      table or amend the policy

### Packaging

- [ ] The `v0.1.0` tag is **orphaned** — it shares no ancestry with `main`, yet
      `../Aconiq` requires it with no `replace`. Re-tag from `main` and bump Aconiq
- [ ] Consider moving `cmd/` to its own module so library consumers do not inherit
      cobra + pflag + mousetrap

---

## Phase 6 — Encryption support (read-only)

**Goal:** Open encrypted `.abs` files given a password.

The cryptography is **solved and verified** — see
[Encryption](#encryption-solved-and-verified) for the full scheme. What remains is
mechanical Go coding against a known-correct specification. `crypto.go` currently ships
an implementation that is wrong in four independent ways: zero IV, plain AES-CBC,
whole-page decryption, and RIPEMD-128-only key derivation.

### Steps

- [x] Detect encrypted files — `TABSDBHeader.Encrypted` at offset 43
- [x] Parse `TABSCryptoHeader` at offset 76
- [x] RIPEMD-128 (passes all seven official vectors)
- [x] Recover the key derivation, cipher mode, IV and CRC convention
- [x] Recover the per-algorithm hash mapping (via go-dede on `ABSDiskEngine.dcu`)
- [ ] Add `ripemd256.go` (needed for Rijndael-256, Blowfish and Twofish-256)
- [ ] Add `absCRC32` — reflected `0xEDB88320`, init 0, **no** final XOR
- [ ] Replace `decryptCBC` with `decryptCTS` using the **`0xFF` IV** and DEC feedback
      rule `F_{i+1} = C_i XOR F_i`, including the trailing partial block
- [ ] Make `deriveKey` algorithm-aware (it currently takes an `algo` parameter and
      ignores it, so Blowfish and DES files are silently treated as AES-128 and fail
      with a misleading `ErrWrongPassword`)
- [ ] Return the already-declared `ErrUnsupportedCipher` for Twofish/Square instead of
      `ErrWrongPassword` — it is currently never returned anywhere
- [ ] Decrypt `data[420:pageSize]` in `ReadPage`, gated on `ABSP.CRC32 != 0`, leaving
      the `ABSP` header in the clear
- [ ] Add `golang.org/x/crypto/blowfish`
- [x] Add `OpenWithPassword(path, password string) (*File, error)`
- [ ] Add a `--password` flag to the CLI — `OpenWithPassword` exists and three
      encrypted fixtures are present, but no command can reach the feature
- [ ] Repoint the crypto tests at the real encrypted fixtures (Phase T)

### Deferred

- [ ] Twofish-128/256 and Square — need Go implementations; Square is DEC-specific and
      would have to be ported. No fixtures exist to test against
- [ ] Rijndael-256 and DES-Triple — stdlib ciphers, but no fixtures
- [ ] Reproduce the `ABSP.CRC32` page checksum (cosmetic; integrity checking only)

---

## Phase 7 — Write support: record modification

**Goal:** Create, update, and delete records in existing tables.

### Steps

- [ ] Implement record insertion into data pages (find free slot or append new page)
- [ ] Implement record update (overwrite in place for fixed-size records)
- [ ] Implement record deletion (mark as deleted, add to free list)
- [ ] Implement free space management (reclaim deleted record slots)
- [ ] Implement transaction support (buffered writes, commit/rollback)
- [ ] Flush dirty pages to disk
- [ ] Test: insert, update, delete records; verify with read-back

---

## Phase 8 — Write support: schema operations

**Goal:** Create tables, add/remove columns, manage indexes.

### Steps

- [ ] Implement `CREATE TABLE` — allocate pages, write schema, create initial data page
- [ ] Implement `ALTER TABLE ADD COLUMN` — update schema, pad existing records
- [ ] Implement `ALTER TABLE DROP COLUMN` — update schema, compact records
- [ ] Implement `CREATE INDEX` — build B-tree from existing data
- [ ] Implement `DROP INDEX` — remove index pages, update catalog
- [ ] Implement database compaction (defragment free space)
- [ ] Test: create database from scratch, add tables, insert data, verify

---

## Phase 9 — database/sql driver (optional)

**Goal:** Implement Go's `database/sql` driver interface for Absolute Database files.

### Steps

- [ ] Implement `database/sql/driver.Driver` — register "absdb" driver
- [ ] Implement `Connector`, `Conn`, `Stmt`, `Rows`, `Result`
- [ ] Support basic SQL: `SELECT`, `INSERT`, `UPDATE`, `DELETE` (or delegate to existing SQL parser)
- [ ] Support `CREATE TABLE`, `DROP TABLE`
- [ ] Test with standard `database/sql` API

---

## Test strategy

### Test data

Fixtures live in `testdata/` (currently **gitignored** — see Phase T). Derived from
SoundPlan project files in `../Aconiq/interoperability/Schienenprojekt - Schall 03/`.

Columns and row counts below are **measured**, not estimated. "True rows" comes from the
B-tree index scan; "reader" is what the record iterator returns today.

| File                         | Version | Cols | True rows | Reader | Status                                  |
| ---------------------------- | ------- | ---- | --------- | ------ | --------------------------------------- |
| `TS03.abs`                   | 5.13    | 9    | 18        | 18     | OK                                      |
| `RREC0011.abs`               | 7.61    | 20   | 30        | 27     | 3 rows lost (Phase 5c bug 2)            |
| `RCON0011.abs`               | 7.61    | 36   | 300       | 275    | 25 rows lost                            |
| `RCFQ0011.abs`               | 7.61    | 7    | 600       | 504    | **garbage values** (bug 1) + rows lost  |
| `RMPA0011.abs`               | 7.61    | 31   | 600       | —      | **garbage**, emits invalid JSON (bug 1) |
| `RPDG0011.abs`               | 7.61    | 5    | 30        | 30     | OK, but 4 of 60 BLOBs are wrong         |
| `RR240011.abs`               | 7.61    | 27   | 30        | 28     | 2 rows lost                             |
| `RFRQ0011.abs`               | 7.61    | 5    | 60        | 60     | OK                                      |
| `RGRP0011.abs`               | 7.61    | 6    | 30        | 30     | OK                                      |
| `RMND0011.abs`               | 7.61    | 3    | 10        | 10     | OK                                      |
| `RRAD0011/0012/0023.abs`     | 7.61    | 12   | 20 each   | 20     | OK                                      |
| `RRAI0011/0012/0023.abs`     | 7.61    | 12   | 5 each    | 5      | OK                                      |
| `Addresses.abs`              | 7.94    | 19   | 0 (empty) | error  | empty table returned as `ErrNoData`     |
| `Addresses-Rijndael_128.abs` | 7.94    | 19   | 0 (empty) | —      | encrypted; password `"Bla"`             |
| `Addresses-Blowfish.abs`     | 7.94    | 19   | 0 (empty) | —      | encrypted; password `"Bla"`             |
| `Addresses-DES_Single.abs`   | 7.94    | 19   | 0 (empty) | —      | encrypted; password `"Bla"`             |

> **Correction (2026-08-28 review).** The previous version of this table was written
> from notes rather than from the files, and was wrong throughout: `RCFQ0011` was listed
> as 20 columns / 602 rows (really 7 / 600), `RCON0011` as 40 / 15 (really 36 / 300),
> `RPDG0011` as 73 / 32 (really 5 / 30), and `Addresses.abs` as v7.10 / 12 columns
> (really v7.94 / 19, and empty). `AttrEsse.abs` was listed but is not in `testdata/`.

### Validation approach

1. **Index cross-check (the strongest available oracle, and unused today).** The B-tree
   leaf scan is decoded correctly and is independent of the record decoder. It yields
   exact row counts and `(page, item)` for every row. Assert reader output against it
   for every fixture — this alone would have caught every Phase 5c bug.
2. **Structural invariants.** Where the data has a known shape, assert it: RPDG BLOBs
   are float32 triplets, so `len(data) % 12 == 0`. This is genuinely independent of the
   parser, unlike constants read out of the reader's own output.
3. **Decryption equality.** Each encrypted fixture must decrypt byte-identically to its
   plaintext twin — already demonstrated for Rijndael-128 and DES-Single.
4. **Round-trip with Delphi.** Use the Absolute Database Personal Edition to create
   databases with known field types and values, export to CSV, and compare. This is the
   only way to validate the 14 field types with zero corpus coverage.
5. **Cross-reference SoundPlan** for result files, via SoundPlan's own export/report.
6. **Fuzz testing** — see Phase 5d.

> **On circular validation.** Many current expected values (`PageCount() = 14`,
> `EntryCount = 18`, `RootPageNo = 12`, `HeaderSize = 280`) were read out of this
> reader's own output and frozen. They pin behaviour, which is useful as regression
> detection, but they prove nothing about correctness. Prefer oracles 1–4.

---

## Project structure

Current (all library code is in the root package):

```
go-absolute-database/
├── absdb.go           # File, Page, Open/Close, page headers      — Phase 1
├── schema.go          # TableSchema, Column, FieldType            — Phase 2
├── reader.go          # Reader, Record, field deserialization     — Phase 3
├── blob.go            # BLOB reading and decompression            — Phase 4
├── index.go           # B-tree index reading                      — Phase 5
├── crypto.go          # Encryption/decryption                     — Phase 6
├── ripemd128.go       # RIPEMD-128 (DEC key derivation)
├── cmd/absdb/         # CLI: info, pages, schema, dump, hexpage, blob
├── testdata/          # .abs fixtures (GITIGNORED — see Phase T)
├── legacy/            # ComponentAce SDK, reference only (GITIGNORED, never commit)
├── docs/plans/        # design notes
├── PLAN.md
├── justfile, .golangci.yml, treefmt.toml
└── go.mod
```

Planned additions:

```
├── ripemd256.go       # needed for Rijndael-256 / Blowfish / Twofish-256 — Phase 6
├── fuzz_test.go       # FuzzOpen, FuzzParseSchema, FuzzReadBlob          — Phase 5d
├── .github/workflows/ # CI running `just ci`                             — Phase T
├── writer.go          # Record insert/update/delete                      — Phase 7
├── ddl.go             # Schema operations                                — Phase 8
└── driver/driver.go   # database/sql driver                              — Phase 9
```

The `internal/encoding` and `internal/page` packages sketched in earlier revisions were
never created; the functionality lives in `reader.go` and `absdb.go`. `ripemd128.go` is a
self-contained primitive with no dependency on the format and would be a reasonable
candidate for `internal/`.

---

## Priority

**Immediate — correctness and trust.** The library is already a dependency of
`../Aconiq`, and it currently loses rows and returns garbage without signalling either.

1. **Phase 5c** — record and index layout correctness. Two of the four fixes are one
   line each and were verified to have zero regressions.
2. **Phase T** — the index cross-check oracle, a green suite, committed fixtures, CI.
   5c without T means the next regression is equally invisible.
3. **Phase 5d** — robustness. Required before the library is pointed at any `.abs`
   file that Aconiq did not produce itself.

**Next — capability.**

4. **Phase 6** — ship the verified encryption scheme. The hard part is done.
5. **Phase 5e** — multi-table, if any real file needs it. Note it forces a breaking API
   change, so it should land before a stable version is tagged.

**Deferred.** Phases 7–9 (write support, DDL, `database/sql`) remain out of scope until
there is a concrete use case for writing `.abs` files.
