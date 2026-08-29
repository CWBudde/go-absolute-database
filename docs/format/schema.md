# The schema stream

A table's column definitions live in a zlib-compressed [internal file](internal-files.md) on
its type-8 page, named by the table's catalog entry. The stream is:

```
int32 columnCount
columnCount x column definition
int32 indexCount
indexCount x index record
int32 constraintCount
constraintCount x constraint record
int32 recordPageIndexRoot
int32 blobPageIndexRoot
```

The two trailing page numbers are the root of the index over the table's data pages and the
root of the index over its BLOB pages; the second is `-1` for a table with no BLOB column.

Because the stream is a sequence of variable-length records, every write path edits it
**surgically** — inflate, splice bytes in or out, re-compress — rather than parsing it into a
`TableSchema` and re-serializing. A re-serializer drops what it does not understand.

## Column definition

```
Pascal  name                      Windows-1252
int32   columnID                  database-wide object id
byte    baseType                  TABSBaseFieldType
byte    advancedType              TABSAdvancedFieldType
int32   size                      max length for variable-width types
Blob/Clob/WideClob only:
  byte    BLOBCompressionAlgorithm  0 none, 1 zlib
  byte    BLOBCompressionMode       the level, 0 when the algorithm is none
  int32   BLOBBlockSize             102400 in every column in the corpus
int64   AutoincIncrement          1
int64   AutoincInitialValue       0
int64   AutoincMinValue           0
int64   AutoincMaxValue           High(Int64), i.e. FF FF FF FF FF FF FF 7F
byte    AutoincCycled             ByteBool
        default                   a typed value (below)
```

The five autoinc fields are `TABSFieldDef`'s, in its declaration order. They are **33 bytes with
nothing between them and nothing left over**, which is what identifies them: an earlier reading of
this span as a `flags` byte, then 23 zero bytes of padding, then seven `0xFF` bytes, then a
`0x7F 0x00 <baseType>` terminator, describes exactly the same 33 bytes. The `flags` byte was the
low byte of `AutoincIncrement`; the zeros were the rest of it plus `InitialValue` and `MinValue`;
the `0xFF` run and the `0x7F` were `AutoincMaxValue`; the `0x00` was `AutoincCycled`; and the
`<baseType>` was the type tag of the `DEFAULT` typed value that follows.

`CNone.A` in `Constraints.abs` is the worked example:

```
01 41                      name "A"
02 00 00 00                columnID 2
07 07                      baseType Int32, advancedType Integer
00 00 00 00                size 0
01 00 00 00 00 00 00 00    AutoincIncrement    = 1
00 00 00 00 00 00 00 00    AutoincInitialValue = 0
00 00 00 00 00 00 00 00    AutoincMinValue     = 0
ff ff ff ff ff ff ff 7f    AutoincMaxValue     = High(Int64)
00                         AutoincCycled       = False
07 ff                      DEFAULT: type tag, then the absent marker
```

All five fields carry those same values in **all 495 column definitions in the corpus**, including
the AutoInc columns, so nothing in it has ever been observed to vary. The BLOB descriptor does
vary: `Addresses.abs` and `RPDG0011.abs` store their BLOBs uncompressed (`0 / 0 / 102400`) and
`TS03.abs` stores its `Kommentar` and `Graphic` columns zlib-compressed at level 4
(`1 / 4 / 102400`).

**`NOT NULL` is not in the column definition.** `Constraints.abs` settles it directly: `CNone.A`
and `CNotNull.A` differ by their object id and by nothing else, so a reader cannot learn
nullability from this record. It is the [constraint record](#constraint-record) of kind 3 that
carries it, which is what `Column.NotNull` reads.

**`DEFAULT` is not a constraint record** — it is the typed value that closes the column
definition, which is why a `DEFAULT` clause makes the definition longer rather than changing
anything before it. `A INTEGER` ends `7F 00 07 FF`; `A INTEGER DEFAULT 7` ends
`7F 00 07 00 04 00 00 00 07 00 00 00`. In both, the `7F` is the top byte of `AutoincMaxValue`
and the `00` after it is `AutoincCycled`; the typed value itself is what follows.

A **typed value** is `byte type` — the `TABSVariant` tag, which echoes the column's base type in
files DBManager wrote and is `0x00` in the SoundPlan files — then `byte flag`, `0xFF` absent and
`0x00` present, followed when present by an `int32` byte count and that many bytes.

## Index record

```
Pascal  name
int32   objectID
byte    reserved
byte    UNIQUE                    ByteBool
byte    PRIMARY                   ByteBool
int32   coveredColumnCount
int32   rootPageNo                the index's own B-tree root
coveredColumnCount x:
    Pascal  columnName
    byte    DESC                  ByteBool
    byte    NOCASE                ByteBool
    int32   maxIndexedSize        0x14 in every record in the corpus
```

An index's **name is stored** — in this stream, after the last column definition. It is not
visible to a raw `strings` scan of the file, because the stream is compressed.

`rootPageNo` is confirmed on 34 of the corpus's 35 occurrences: the `int32` names a page whose
`PageType` is 12. The single exception belongs to a composite primary key, which reuses the
covered-column shape without being an index.

That field forces an ordering on any implementation: **the index page must be allocated before
the stream is serialized**, because the stream embeds its number.

## Constraint record

Every record opens the same way:

```
byte    kind                      0 PRIMARY KEY, 2 UNIQUE, 3 NOT NULL,
                                  4 CHECK (MINVALUE/MAXVALUE)
Pascal  name                      "C_PK$SrcNo$RecNo", "$C_NotNull$<file>$<Column>", ...
int32   objectID
8 bytes reserved                  zero in all 66 records in the corpus
int32   ownerObjectID             the covered column for kinds 3 and 4,
                                  the backing index for kinds 0 and 2
```

The body then splits, and the split is the one surprise in the format: **key-shaped records
size their strings and counts with `int32` fields, column-shaped ones size the same fields
with single bytes.**

Key-shaped — kinds 0 and 2:

```
int32   count                     1 in every observed record
byte    pad                       0
int32 + Pascal  tableName
int32 + Pascal  indexName         the index record implementing this key
int32   columnCount
  int32           columnObjectID
  int32 + Pascal  columnName
```

Column-shaped — kinds 3 and 4:

```
byte    count                     1 in every observed record
byte    pad                       0
byte + Pascal   tableName
byte + Pascal   columnName
kind 4 only: two typed values, MINVALUE then MAXVALUE, either of which may be absent
```

The size field counts the Pascal string **including its own length byte**, so `"CPk"` is
written `04 00 00 00 03 'C' 'P' 'k'`. That redundancy is checked rather than skipped: it makes
a mis-parse fail loudly instead of sliding.

Kind 1 is not observed anywhere in the corpus and is refused.

## Object ids in the stream

A table hands out ids in a fixed order: one for itself, one per column, one per index, one per
constraint record. In a database of twelve two-column tables that order is what makes the table
ids run 1, 4, 8, 13, 18, 21, 25, 31, 36, 39, 42, 45 rather than three apart — and it is
corroborating evidence for the record layout above.

## What is refused rather than guessed

`ErrSchemaTailNotUnderstood` covers: an unobserved constraint kind, a non-zero reserved field,
a body count other than 1, a non-zero pad byte, a size disagreeing with its string, an index
flag byte that is neither `0x00` nor `0xFF`, a `maxIndexedSize` other than `0x14`, and any
leftover bytes before the two trailing page numbers.
