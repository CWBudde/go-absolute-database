# Data pages, records and field types

## Data page layout

```
[ occupancy bitmap ][ record 0 ][ record 1 ] ... [ record N-1 ]
```

All of it is computable from the schema. No search or heuristic is required:

```
nullFlagBytes  = ceil(numColumns / 8)
recordSize     = nullFlagBytes + sum(fieldStoreSize(col))     # no trailer, no padding
recordsPerPage = max n with ceil(n/8) + n*recordSize <= PageSize - 40
bitmapBytes    = ceil(recordsPerPage / 8)
record k       at payload[bitmapBytes + k*recordSize]
```

`recordsPerPage` is a fixed point: the bitmap size depends on the slot count, which depends on
the bitmap size.

Each record begins with `nullFlagBytes` of null flags (bit set = NULL), followed by the fields
in column order at their natural fixed widths. **There is no record trailer.**

The **spare high bits of the last null-flag byte are set to 1**, which gives a free
self-check: if the computed `nullFlagBytes` is right, those bits are set. This is validated as
an error, never used as a search.

Verified across the corpus:

| Columns | `nullFlagBytes` | `recordSize` | `bitmapBytes` | Records/page |
| ------- | --------------- | ------------ | ------------- | ------------ |
| 9       | 2               | 99           | 5             | 40           |
| 20      | 3               | 176          | 3             | 23           |
| 7       | 1               | 47           | 11            | 86           |
| 31      | 4               | 286          | 2             | 14           |

## Deletion

The page's leading occupancy bitmap marks which slots hold live records. That is the whole of
how deletion is represented — there is no per-record tombstone, and the deleted record's bytes
stay in place until the slot is reused.

`bitmapBytes` must never be inferred from the number of set bits: a page can hold far more
slots than rows, leaving most of the bitmap zero.

## Field types

| Type       | Delphi ftType    | Storage                                         | SQL aliases                    |
| ---------- | ---------------- | ----------------------------------------------- | ------------------------------ |
| AutoInc    | ftAutoInc        | uint32 (4 bytes)                                | AUTOINC                        |
| BLOB       | ftBlob           | 6-byte pointer: PageNo int32 + ItemNo uint16    | BLOB                           |
| Bytes      | ftBytes          | fixed byte array                                | BYTES(n)                       |
| Currency   | ftCurrency       | 8 bytes (Delphi Currency = int64 / 10000)       | CURRENCY, MONEY                |
| Date       | ftDate           | 4 bytes, int32 LE — days, 0001-01-01 = 1        | DATE                           |
| DateTime   | ftDateTime       | 8 bytes, `TABSDateTime{int32 Date; int32 Time}` | DATETIME                       |
| Extended   | ftExtended       | 10 bytes (80-bit extended)                      | EXTENDED                       |
| Float      | ftFloat          | 8 bytes (float64)                               | FLOAT, DOUBLE, REAL, NUMERIC   |
| FmtMemo    | ftFmtMemo        | BLOB pointer                                    | FMTMEMO                        |
| Graphic    | ftGraphic        | BLOB pointer                                    | GRAPHIC                        |
| GUID       | ftGUID           | 16 bytes                                        | GUID                           |
| Integer    | ftInteger        | 4 bytes (int32)                                 | INTEGER, INT, INT32            |
| LargeInt   | ftLargeInt       | 8 bytes (int64)                                 | LARGEINT, BIGINT, INT64        |
| Logical    | ftBoolean        | 2 bytes (Delphi WordBool)                       | LOGICAL, BOOLEAN, BOOL, BIT    |
| Memo       | ftMemo           | BLOB pointer                                    | MEMO, CLOB, TEXT               |
| SmallInt   | ftSmallint       | 2 bytes (int16)                                 | SMALLINT, INT16                |
| String     | ftString         | fixed bytes, Windows-1252 (up to 65500)         | STRING(n), CHAR(n), VARCHAR(n) |
| Time       | ftTime           | 4 bytes, int32 LE — milliseconds since midnight | TIME                           |
| TimeStamp  | ftTimeStamp      | 8 bytes                                         | TIMESTAMP                      |
| VarBytes   | ftVarBytes       | length-prefixed bytes                           | VARBYTES(n)                    |
| WideMemo   | ftBlob (subtype) | BLOB pointer                                    | WIDEMEMO                       |
| WideString | ftWideString     | fixed UTF-16LE (up to 65500)                    | WIDESTRING(n), NCHAR(n)        |
| Word       | ftWord           | 2 bytes (uint16)                                | WORD, UNSIGNEDINT16, UINT16    |

Dates are **not** Delphi `TDateTime` floats. `ABSTypes.hpp` declares
`TABSDateTime { int Date; int Time }`: `Date` is days with 0001-01-01 as day 1 (the BDE
`ftDate` convention, `Trunc(TDateTime) + 693594`), `Time` is milliseconds since midnight.

## Strings

String fields and column names are **Windows-1252** and must be decoded to UTF-8.
`WideString`/`WideChar` are UTF-16LE over the field's full width — not null-terminated
Windows-1252.

## Type coverage

Only six base types occur anywhere in the fixture corpus: **Int32, Varchar, Double, Logical,
Blob, Clob**. Everything else — LargeInt, SmallInt, Word, Single, Currency, WideString, GUID,
Extended, VarBytes, Bytes, TimeStamp, Date, Time, DateTime — has zero rows of coverage and is
correct by construction only. See [../open-questions.md](../open-questions.md).

## Capacity limits

From the official documentation:

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
