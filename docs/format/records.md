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
| Bytes      | ftBytes          | Size + 1 bytes                                  | BYTES(n)                       |
| Currency   | ftCurrency       | 8 bytes, IEEE-754 double — _not_ a scaled int64 | CURRENCY, MONEY                |
| Date       | ftDate           | 4 bytes, int32 LE — days, 0001-01-01 = 1        | DATE                           |
| DateTime   | ftDateTime       | 8 bytes, `TABSDateTime{int32 Date; int32 Time}` | DATETIME                       |
| Extended   | ftExtended       | 10 bytes, x87 80-bit — explicit integer bit     | EXTENDED                       |
| Float      | ftFloat          | 8 bytes (float64)                               | FLOAT, DOUBLE, REAL, NUMERIC   |
| FmtMemo    | ftFmtMemo        | BLOB pointer                                    | FMTMEMO                        |
| Graphic    | ftGraphic        | BLOB pointer                                    | GRAPHIC                        |
| GUID       | ftGUID           | Char(38): the braced text, then a NUL           | GUID                           |
| Integer    | ftInteger        | 4 bytes (int32)                                 | INTEGER, INT, INT32            |
| LargeInt   | ftLargeInt       | 8 bytes (int64)                                 | LARGEINT, BIGINT, INT64        |
| Logical    | ftBoolean        | 2 bytes (Delphi WordBool)                       | LOGICAL, BOOLEAN, BOOL, BIT    |
| Memo       | ftMemo           | BLOB pointer                                    | MEMO, CLOB, TEXT               |
| SmallInt   | ftSmallint       | 2 bytes (int16)                                 | SMALLINT, INT16                |
| String     | ftString         | fixed bytes, Windows-1252 (up to 65500)         | STRING(n), CHAR(n), VARCHAR(n) |
| Time       | ftTime           | 4 bytes, int32 LE — milliseconds since midnight | TIME                           |
| TimeStamp  | ftTimeStamp      | 8 bytes, base type DateTime, layout undecoded   | TIMESTAMP                      |
| VarBytes   | ftVarBytes       | Size + 1 bytes                                  | VARBYTES(n)                    |
| WideMemo   | ftBlob (subtype) | BLOB pointer                                    | WIDEMEMO                       |
| WideString | ftWideString     | fixed UTF-16LE (up to 65500)                    | WIDESTRING(n), NCHAR(n)        |
| Word       | ftWord           | 2 bytes (uint16)                                | WORD, UNSIGNEDINT16, UINT16    |

### What `Types.abs` settled

Four of those rows say something other than what this package assumed, and each was wrong in a
way no synthetic test could have caught — the encoder and the decoder agreed with each other.

**Currency is a `double`, not a scaled `int64`.** `TReal.R4` holds `8765.4321` as
`4d 84 0d 4f b7 1e c1 40`, which is that IEEE-754 double exactly and is nothing like the scaled
`87654321`. `ABSTypes.hpp` agrees: `typedef double TABSCurrency`. Read as a Delphi in-memory
Currency, that field came back as `4.67e+14`.

**A GUID is text.** `aftGuid` has no base type of its own — there is no `bftGuid` — and a GUID
column is `Char` with `Size` 38, holding the braced form and a NUL:

```
7b 33 46 32 35 30 34 45 30 2d ... 33 30 31 7d 00    "{3F2504E0-4F89-11D3-9A0C-0305E82C3301}"
```

So `TABSGuid`'s typedef of the Win32 `GUID` struct describes the in-memory value, not the stored
one, and there is no byte order to reverse. The engine accepts a bare literal too and stores it
as it was written, so both forms occur.

**`Bytes` and `VarBytes` store `Size + 1` bytes**, as `Char` and `Varchar` do. A `BYTES(16)`
occupies seventeen. `VarBytes` is not the length-prefixed `Size + 2` it was modelled as. What the
extra byte holds is not established: every byte column in the corpus is NULL, because the engine
refuses an SQL literal for one — both `MIMETOBIN('...')` and a plain string are rejected with
`Invalid variant type or size`, so a value can only be written through a parameter.

**A TimeStamp is not a DateTime**, though it shares `BftDateTime` as its base type. Where a
DateTime writes `{int32 Date; int32 Time}`, `2019-03-07 01:02:03` is stored as

```
e3 07 03 00 07 00 01 00     2019, 3, 7, 1
```

which reads as the year, month, day and hour rather than as a day count and a millisecond count,
and accounts for no minutes or seconds. The layout is **undecoded**: one value is not enough to
settle it, and `Record.Time` returns the zero time for a TimeStamp rather than a confidently
wrong instant.

Dates are **not** Delphi `TDateTime` floats. `ABSTypes.hpp` declares
`TABSDateTime { int Date; int Time }`: `Date` is days with 0001-01-01 as day 1 (the BDE
`ftDate` convention, `Trunc(TDateTime) + 693594`), `Time` is milliseconds since midnight.

## Strings

String fields and column names are **Windows-1252** and must be decoded to UTF-8.
`WideString`/`WideChar` are UTF-16LE over the field's full width — not null-terminated
Windows-1252.

## Type coverage

Only six base types occur anywhere in the **private** fixtures: Int32, Varchar, Double, Logical,
Blob, Clob. Everything else was correct by construction only until `Types.abs`, which the engine
wrote for exactly this purpose and which now covers every type in the table above — see
[../testing.md](../testing.md).

That is engine-written evidence rather than field data, so it says what the engine stores, not
what any application stores. One gap remains: **TimeStamp**'s layout is undecoded, and it is in
[../open-questions.md](../open-questions.md).

### Extended

An `EXTENDED` column holds the x87 80-bit format the Delphi `Extended` type compiles to on
32-bit Windows: little-endian, a 64-bit significand first, then a word carrying the sign in its
top bit and a 15-bit exponent biased by 16383 in the rest. Unlike every IEEE binary format the
significand's leading bit is **explicit** rather than implied, so the value is that 64-bit
integer scaled by `2^(exponent - 16383 - 63)` with no hidden bit to restore. An exponent of all
ones is an infinity when the significand is exactly the integer bit and a NaN otherwise.

`Types.abs` pins it: `TReal.R3` holds `1.6180339887498949` as
`00 40 a5 bf dc bc 1b cf ff 3f` — significand `0xCF1BBCDCBFA54000`, exponent `0x3FFF`, so an
unbiased exponent of zero and a value of that integer over 2^63.

`Record.Float` rounds it to `float64`, which is the one lossy read in this package: 64 bits of
significand do not fit in 53. That is also why an Extended column stays refused by the write
path (`ErrColumnNotWritable`) — a rewrite of some other column in the row would silently
truncate this one.

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
