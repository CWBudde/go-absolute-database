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
byte    flags
6 bytes                           BLOB descriptor, present only for Blob/Clob/WideClob
        padding                   zeros, then 0xFF bytes
0x7F 0x00 <byte>                  terminator; <byte> is the baseType echo or 0x00
        default                   a typed value (below)
```

**`DEFAULT` is not a constraint record** — it is the typed value at the end of the column
definition, which means a `DEFAULT` clause moves the terminator. `A INTEGER` ends
`7F 00 07 FF`; `A INTEGER DEFAULT 7` ends `7F 00 07 00 04 00 00 00 07 00 00 00`.

A **typed value** is `byte flag` — `0xFF` absent, `0x00` present — followed when present by an
`int32` byte count and that many bytes.

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
