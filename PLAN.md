# go-absolute-database — Implementation Plan

Read-only (later: read-write) Go library for ComponentAce **Absolute Database** `.abs` files.

## Background

Absolute Database is a single-file embedded database engine for Delphi (by ComponentAce). It replaced the Borland Database Engine (BDE) and is used by several commercial Windows applications — notably **SoundPlan**, the dominant DACH noise calculation tool, which stores all result tables, train type catalogues, address lists, and attribute tables as `.abs` files.

No public specification of the binary format exists. This library is based on reverse-engineering of real `.abs` files (SoundPlan 7.x/8.x output, Absolute Database versions 5.13–7.61) and the official Absolute Database documentation at <https://www.componentace.com/help/absdb_manual/>.

### Official documentation references

- [Absolute Database Manual](https://www.componentace.com/help/absdb_manual/absdbmanual_content.htm)
- [Field Data Types](https://www.componentace.com/help/absdb_manual/supporteddatatypes.htm)
- [Maximum Capacity Specifications](https://www.componentace.com/help/absdb_manual/maximumcapacityspecification.htm)
- [CREATE TABLE syntax](https://www.componentace.com/help/absdb_manual/createtablestatement.htm)
- [Personal Edition download](https://www.componentace.com/download/)

---

## Format knowledge (reverse-engineered)

### File header (page 0)

| Offset | Size | Type       | Description                                                          |
| ------ | ---- | ---------- | -------------------------------------------------------------------- |
| 0      | 16   | bytes      | Magic: `ABS0LUTEDATABASE` (note: zero, not letter O)                 |
| 16     | 1    | byte       | Mode flag: `L` (0x4C) = local/single-user                            |
| 17     | 1    | byte       | Unknown (always 0x00 in samples)                                     |
| 18     | 8    | float64 LE | Database engine version (e.g. 5.13, 7.10, 7.61)                      |
| 26     | 2    | uint16 LE  | Page size in bytes (observed: 4096, max: 65536)                      |
| 28     | 2    | uint16 LE  | Unknown (always 8 in samples — possibly header version or alignment) |
| 30     | 2    | uint16 LE  | Total column count (user columns + internal columns)                 |
| 34     | 4    | uint32 LE  | User-visible column count                                            |
| 38     | 4    | uint32 LE  | Varies per file (possibly row count or free page pointer)            |
| 42     | 4    | uint32 LE  | Unknown (observed values: 2)                                         |
| 76     | 4    | uint32 LE  | Pointer/offset (observed: 0x0118 = 280 in some files)                |

### ABSP marker (page metadata)

Every page contains an **ABSP** marker at a fixed offset within the page. In page 0, the ABSP marker appears at absolute offset **0x17C** (380 bytes). The ABSP marker structure:

| Offset | Size | Type      | Description                                                  |
| ------ | ---- | --------- | ------------------------------------------------------------ |
| 0      | 4    | bytes     | Marker: `ABSP`                                               |
| 4      | 2    | uint16 LE | Column/entry count for this page                             |
| 6      | 2    | uint16 LE | Unknown (observed: 0)                                        |
| 8      | 2    | uint16 LE | Page type or flags (observed: 3, 6, 10, 12)                  |
| 10+    | var  | bytes     | Bitmask and metadata (column presence flags, free space map) |

### Page types (preliminary classification)

| Page role         | Characteristics                                                              | Observed                         |
| ----------------- | ---------------------------------------------------------------------------- | -------------------------------- |
| File header       | Page 0; contains magic, version, schema metadata                             | All files                        |
| Table metadata    | Contains filename string at page+0x1AF                                       | Page 4 in samples                |
| Schema/catalog    | ABSP marker with column definitions, field descriptors                       | Page 9 in TS03                   |
| B-tree index leaf | Fixed-size key entries (e.g. 29 bytes = string key + child pointers), sorted | Page 12 in TS03                  |
| Data page         | Fixed-size records (e.g. 99 bytes), ABSP header + record array               | Page 13 in TS03                  |
| Free/empty        | All zeros                                                                    | Most middle pages in small files |

### Record structure (data pages)

Records are **fixed-size** within a table. Each record contains fields in column order, serialised as:

- **String**: fixed-length byte array (Windows-1252 or UTF-16 for WideString), null-terminated, padded with zeros
- **Float/Double**: 8 bytes, float64 little-endian
- **Integer**: 4 bytes, int32 little-endian
- **SmallInt**: 2 bytes, int16 little-endian
- **Word**: 2 bytes, uint16 little-endian
- **LargeInt**: 8 bytes, int64 little-endian
- **Logical/Boolean**: 1–2 bytes
- **AutoInc**: 4 bytes, uint32 little-endian
- **Date/Time/DateTime**: 8 bytes, Delphi TDateTime format (float64: integer part = days since 1899-12-30, fractional = time of day)
- **BLOB/Memo/Graphic**: stored out-of-band in BLOB block area; record contains a pointer (offset + size)

Records end with a **2–3 byte trailer** containing a record sequence number or flags.

### B-tree index pages

Index pages contain sorted key entries for B\*-tree traversal. Each entry is:

- The indexed field value (fixed size matching field definition)
- Child page pointer(s) (uint32 LE)

Index pages allow O(log n) lookup without scanning all data pages.

### Observed field-to-offset mapping (TS03.abs, 13 user columns)

| Record offset | Size | Content (Record 0: "D/FD-Zug 1988")                     |
| ------------- | ---- | ------------------------------------------------------- |
| +0x00         | 19   | String: train type name (null-terminated, Windows-1252) |
| +0x13         | 3    | String: short code "22"                                 |
| +0x16         | ~19  | Binary fields (integers, flags)                         |
| +0x29         | 8    | Float64: 30.0 (likely a speed or correction parameter)  |
| +0x31         | 8    | Float64: 160.0 (likely length or another parameter)     |
| +0x39         | 8    | Float64: 341.0 (likely a parameter)                     |
| +0x41         | ~20  | Remaining fields (floats/ints, mostly zero)             |
| +0x5d         | 3    | Record trailer (flags + sequence number)                |

### Known data types (from official docs)

| Type       | Delphi ftType    | Storage                                   | SQL aliases                    |
| ---------- | ---------------- | ----------------------------------------- | ------------------------------ |
| AutoInc    | ftAutoInc        | uint32 (4 bytes)                          | AUTOINC                        |
| BLOB       | ftBlob           | pointer to BLOB area                      | BLOB                           |
| Bytes      | ftBytes          | fixed byte array                          | BYTES(n)                       |
| Currency   | ftCurrency       | 8 bytes (Delphi Currency = int64 / 10000) | CURRENCY, MONEY                |
| Date       | ftDate           | 8 bytes (TDateTime float64)               | DATE                           |
| DateTime   | ftDateTime       | 8 bytes (TDateTime float64)               | DATETIME                       |
| Extended   | ftExtended       | 10 bytes (80-bit extended)                | EXTENDED                       |
| Float      | ftFloat          | 8 bytes (float64)                         | FLOAT, DOUBLE, REAL, NUMERIC   |
| FmtMemo    | ftFmtMemo        | pointer to BLOB area                      | FMTMEMO                        |
| Graphic    | ftGraphic        | pointer to BLOB area                      | GRAPHIC                        |
| GUID       | ftGUID           | 16 bytes                                  | GUID                           |
| Integer    | ftInteger        | 4 bytes (int32)                           | INTEGER, INT, INT32            |
| LargeInt   | ftLargeInt       | 8 bytes (int64)                           | LARGEINT, BIGINT, INT64        |
| Logical    | ftBoolean        | 2 bytes (Delphi WordBool)                 | LOGICAL, BOOLEAN, BOOL, BIT    |
| Memo       | ftMemo           | pointer to BLOB area                      | MEMO, CLOB, TEXT               |
| SmallInt   | ftSmallint       | 2 bytes (int16)                           | SMALLINT, INT16                |
| String     | ftString         | fixed bytes (up to 65500)                 | STRING(n), CHAR(n), VARCHAR(n) |
| Time       | ftTime           | 8 bytes (TDateTime float64)               | TIME                           |
| TimeStamp  | ftTimeStamp      | 8 bytes                                   | TIMESTAMP                      |
| VarBytes   | ftVarBytes       | length-prefixed bytes                     | VARBYTES(n)                    |
| WideMemo   | ftBlob (subtype) | pointer to BLOB area                      | WIDEMEMO                       |
| WideString | ftWideString     | fixed UTF-16LE (up to 65500)              | WIDESTRING(n), NCHAR(n)        |
| Word       | ftWord           | 2 bytes (uint16)                          | WORD, UNSIGNEDINT16, UINT16    |

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

### Encryption (not in scope for Phase 1)

Seven algorithms supported: Rijndael/AES-128, AES-256, Blowfish-448, Twofish-128, Twofish-256, Square, Single DES, Triple DES. Encrypted files likely have a different header or flag.

### BLOB compression

Algorithms: None, ZLIB, BZIP, PPM. Compression levels 1–9.

---

## Unknowns requiring further investigation

1. **Complete page header structure** — the ABSP marker fields beyond offset +10 are not fully decoded. Need to correlate page types (index vs data vs metadata vs free) with their header signatures.

2. **Schema storage location** — column names, types, sizes, and constraints are stored somewhere (likely in the catalog/schema pages). Need to find and decode the column definition records that map column index → name, type, size.

3. **Page allocation map** — how the engine tracks which pages are free, which are data pages, and which are index pages. Likely a bitmap or free-list in the header or a dedicated page.

4. **BLOB block layout** — how BLOB data is stored and referenced from records. The BLOB area likely has its own block allocation and chaining mechanism.

5. **Multi-table databases** — a single `.abs` file can contain multiple tables. How are table boundaries and the table catalog stored?

6. **Record deletion** — how deleted records are marked (flag byte? moved to free list?).

7. **Index page child pointers** — exact layout of B-tree node entries (key, left child, right child, data page pointer).

8. **Record trailer bytes** — the 2–3 bytes at the end of each record (observed: `00 80 ff NN 00 00 00`). Could be overflow pointer, record status flags, or B-tree link.

9. **Version differences** — files range from v5.13 to v7.61. Structural differences between versions are unknown.

---

## Phase 1 — File header and page navigation (read-only)

**Goal:** Open an `.abs` file, parse the file header, and navigate pages.

### Steps

- [x] Define `File` struct: holds file handle, parsed header, page size
- [x] Parse file header (magic validation, version, page size, column counts)
- [x] Implement `ReadPage(pageNum int) ([]byte, error)` — read a single page by number
- [x] Implement `PageCount() int` — total pages in file
- [x] Parse ABSP marker from any page (offset, column count, page type flags)
- [x] Classify pages: scan all pages and categorize by ABSP signature and content patterns
- [x] Write tests against real `.abs` files (TS03.abs, RREC0011.abs, Addresses.abs)
- [x] Create `testdata/` with small representative `.abs` files (or reference Aconiq interoperability fixtures)

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
    ABSP   *ABSPHeader // nil if no ABSP marker found
}

type ABSPHeader struct {
    ColumnCount uint16
    PageType    uint16
    Flags       uint16
}
```

---

## Phase 2 — Schema extraction

**Goal:** Read column definitions (names, types, sizes) from the database file.

### Steps

- [x] Locate the schema/catalog page(s) — page type 8, zlib-compressed internal file
- [x] Decode column definition records: name (string), type (enum), size (uint32), flags
- [x] Map Delphi `ftXxx` type codes to Go `FieldType` enum (TABSBaseFieldType + TABSAdvancedFieldType)
- [x] Build `TableSchema` struct: column definitions
- [ ] Handle multi-table databases (table catalog enumeration) — deferred, single-table works
- [x] Test against TS03.abs (9 columns: AutoInc + String + 5×Double + Memo + Graphic)
- [x] Test against RREC0011.abs (20 columns: Integer + String + Boolean + Double)
- [x] Test against Addresses.abs (19 columns: AutoInc + 16×String + DateTime + Memo)

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
    Size     int       // for String/Bytes: max length; 0 for fixed-size types
    Nullable bool
    Position int       // 0-based column index
}

type TableSchema struct {
    Name    string
    Columns []Column
    Indexes []IndexDef
}

func (f *File) Tables() ([]string, error)
func (f *File) Schema(table string) (*TableSchema, error)
```

---

## Phase 3 — Record reading

**Goal:** Iterate over data records and read field values.

### Steps

- [x] Locate data pages for a table (page type 10 scan)
- [x] Calculate record size from schema (auto-detected from data page patterns)
- [x] Implement record iteration: walk data pages, extract fixed-size records
- [x] Implement field value deserialization for numeric types:
  - [x] Integer (int32 LE)
  - [x] LargeInt (int64 LE)
  - [x] Float (float64 LE)
  - [x] AutoInc (uint32 LE)
  - [ ] SmallInt (int16 LE) — type defined, untested
  - [ ] Word (uint16 LE) — type defined, untested
  - [ ] Currency (int64 LE / 10000) — type defined, untested
- [x] Implement String field deserialization (Windows-1252 → UTF-8, null-terminated)
- [ ] Implement WideString deserialization (UTF-16LE) — type defined, untested
- [x] Implement Date/Time/DateTime deserialization (Delphi int32 epoch → Go time.Time)
- [x] Implement Logical/Boolean deserialization (WordBool)
- [ ] Implement GUID deserialization — deferred
- [x] Handle null values (null flag bitmask per record)
- [x] Skip deleted records (zero null flags = empty slot)
- [x] Test: read all 18 records from TS03.abs, verify train type names and SBA values
- [x] Test: read receiver results from RREC0011.abs (27 records, coordinates verified)
- [ ] Test: read contributions from RCON0011.abs (40 columns, 15 records) — deferred

### API sketch

```go
type Reader struct { /* unexported fields */ }

func (f *File) OpenTable(name string) (*Reader, error)

func (r *Reader) Schema() *TableSchema
func (r *Reader) Next() bool
func (r *Reader) Err() error
func (r *Reader) Record() Record

type Record struct { /* unexported fields */ }

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

- [ ] Investigate BLOB block storage layout (separate area within the file, or interleaved pages?)
- [ ] Decode BLOB pointer format in records (offset + size, or block chain head)
- [ ] Implement BLOB block reading and chaining (for BLOBs spanning multiple blocks)
- [ ] Implement ZLIB decompression for compressed BLOBs
- [ ] Implement BZIP2 decompression (if encountered in test data)
- [ ] Add `Record.Blob(col int) ([]byte, error)` method
- [ ] Add `Record.Memo(col int) (string, error)` method (Memo = text BLOB)
- [ ] Test against files with BLOB data (if available in SoundPlan project)

---

## Phase 5 — Index reading (read-only)

**Goal:** Read B-tree indexes for efficient key-based lookups.

### Steps

- [ ] Decode index definition records from schema (index name, column list, sort order, case sensitivity)
- [ ] Decode B-tree node page structure (keys, child pointers, leaf indicators)
- [ ] Implement B-tree traversal for single-key lookup
- [ ] Implement range scan via index
- [ ] Implement `Reader.FindByIndex(indexName string, key interface{}) (Record, error)`
- [ ] Test: look up train type by name in TS03.abs
- [ ] Benchmark: index lookup vs full scan

---

## Phase 6 — Encryption support (read-only)

**Goal:** Open encrypted `.abs` files given a password.

### Steps

- [ ] Detect encrypted files (header flag or different magic)
- [ ] Implement AES-128 decryption (most commonly used)
- [ ] Implement AES-256 decryption
- [ ] Implement Blowfish, Twofish, DES, Triple DES, Square (lower priority)
- [ ] Key derivation from password (determine if PBKDF or simple hash)
- [ ] Integrate decryption into page reading (transparent to API consumers)
- [ ] Add `OpenWithPassword(path, password string) (*File, error)`

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

Primary test corpus: SoundPlan project files from `../Aconiq/interoperability/Schienenprojekt - Schall 03/`:

| File                    | Version | Columns | Description                                              |
| ----------------------- | ------- | ------- | -------------------------------------------------------- |
| `TS03.abs`              | 5.13    | 13      | Train type catalogue (20 records, string + float fields) |
| `Addresses.abs`         | 7.10    | 12      | Address database                                         |
| `AttrEsse.abs`          | 5.13    | 11      | Emission attributes                                      |
| `RSPS0011/RREC0011.abs` | 7.61    | 12      | Receiver immission levels                                |
| `RSPS0011/RCON0011.abs` | 7.61    | 40      | Source contributions (15 records)                        |
| `RSPS0011/RPDG0011.abs` | 7.61    | 73      | Propagation diagnostics (32 records)                     |
| `RSPS0011/RCFQ0011.abs` | 7.61    | 20      | Frequency contributions (602 records)                    |

### Validation approach

1. **Round-trip with Delphi**: Use the Absolute Database Personal Edition to create known test databases with specific field types and values. Export to CSV, then verify Go reader produces identical values.
2. **Cross-reference SoundPlan**: For SoundPlan result files, validate extracted values against SoundPlan's own export/report features.
3. **Fuzz testing**: Feed random/corrupted `.abs` files to ensure the parser doesn't panic or produce unbounded allocations.

---

## Project structure

```
go-absolute-database/
├── absdb.go           # File, Page, Open/Close — Phase 1
├── schema.go          # TableSchema, Column, FieldType — Phase 2
├── reader.go          # Reader, Record, field deserialization — Phase 3
├── blob.go            # BLOB reading and decompression — Phase 4
├── index.go           # B-tree index reading — Phase 5
├── crypto.go          # Encryption/decryption — Phase 6
├── writer.go          # Record insert/update/delete — Phase 7
├── ddl.go             # Schema operations — Phase 8
├── driver/
│   └── driver.go      # database/sql driver — Phase 9
├── internal/
│   ├── encoding/      # Windows-1252, UTF-16LE, TDateTime converters
│   └── page/          # Low-level page I/O and caching
├── testdata/           # Small .abs fixtures
├── PLAN.md
├── go.mod
└── go.sum
```

---

## Priority

Phases 1–3 are the immediate priority (needed for Aconiq SoundPlan interoperability).
Phases 4–6 are needed when encountering files with BLOBs or encryption.
Phases 7–9 are deferred until there is a concrete use case for writing `.abs` files.
