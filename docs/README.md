# Documentation

Reference documentation for the `.abs` format and for this package's write path. The format is
reverse-engineered — there is no public specification — so everything here is what real files
and the ComponentAce SDK headers say, not what a specification promises.

`PLAN.md` in the repository root tracks what is built and what comes next; it does not repeat
what is documented here.

## The format

| Document                                             | Covers                                                                                  |
| ---------------------------------------------------- | --------------------------------------------------------------------------------------- |
| [format/file-header.md](format/file-header.md)       | The 380-byte database header, geometry, and what a fresh database contains              |
| [format/pages.md](format/pages.md)                   | The `ABSP` page header, payload extent, page types, the PFS/EAM allocation maps, growth |
| [format/internal-files.md](format/internal-files.md) | Internal files, zlib level 1, the system directory, the table catalog, table info       |
| [format/schema.md](format/schema.md)                 | Column definitions, index records, constraint records                                   |
| [format/records.md](format/records.md)               | Data page layout, record layout, field types, capacity limits                           |
| [format/indexes.md](format/indexes.md)               | B-tree pages, key encoding, the two system indexes                                      |
| [format/blobs.md](format/blobs.md)                   | BLOB pointers, storage pages, compression, chaining                                     |
| [format/encryption.md](format/encryption.md)         | DEC, the cipher deviations that must not be "fixed", self-test vectors                  |

## Working with the package

| Document                               | Covers                                                                        |
| -------------------------------------- | ----------------------------------------------------------------------------- |
| [writing.md](writing.md)               | What a write must keep in step, the byte-identity standard, every refusal     |
| [testing.md](testing.md)               | Fixtures, oracles, fuzzing, generating fixtures, what CI can and cannot check |
| [open-questions.md](open-questions.md) | What is still undecoded, unverified, or diverges from the engine on purpose   |
| [provenance.md](provenance.md)         | Legal basis, and what must never be committed                                 |

## Design notes

[plans/](plans/) holds earlier design notes and investigation reports, kept as written.

## Reference material

- [Absolute Database Manual](https://www.componentace.com/help/absdb_manual/absdbmanual_content.htm)
- [Field Data Types](https://www.componentace.com/help/absdb_manual/supporteddatatypes.htm)
- [Maximum Capacity Specifications](https://www.componentace.com/help/absdb_manual/maximumcapacityspecification.htm)
- [CREATE TABLE syntax](https://www.componentace.com/help/absdb_manual/createtablestatement.htm)

The SDK's own C++Builder headers — `ABSTypes.hpp`, `ABSBTree.hpp`, `ABSCipher.hpp`,
`ABSDiskEngine.hpp` — are the source for every structure declaration quoted in these documents,
and DBManager's Delphi source under `<sdk>/Utils/Source/DBManager/` is the source for the
engine's behaviour.

`<sdk>` throughout these documents means an extracted copy of the ComponentAce SDK, which lives
**outside this repository** — by convention a sibling directory, `../absolute-database-sdk/`. It
is not redistributable, it is not part of this repository and never has been, and reproducing the
work below needs your own licensed copy. See [provenance.md](provenance.md).

## Source layout

All library code is in the repository root, package `absdb`.

```
absdb.go              File, Page, Open/Close, page headers
schema.go             TableSchema, Column, FieldType
catalog.go            The table catalog and Table handles
reader.go             Reader, Record, field deserialization
blob.go               BLOB reading and decompression
index.go              B-tree index reading
crypto.go             Encryption and decryption
cryptowrite.go        Re-encrypting a modified page
crc.go                absCRC32
ripemd128.go          RIPEMD-128, ripemd256.go RIPEMD-256
rijndael.go           DEC's Rijndael (needed for Rijndael-256)
twofish.go            DEC's Twofish variant
tdes.go               DEC's TCipher_3TDES, 24-byte block
square.go             DEC's Square
encode.go             Field encoding for the write path
writer.go             Record insert, update, delete
writer_index.go       User index maintenance
ddl.go                The allocation model and DROP TABLE
ddl_grow.go           Extending the file by whole extents
ddl_create.go         CREATE TABLE
ddl_index.go          CREATE INDEX, DROP INDEX, the schema tail
ddl_constraint.go     Constraint records
ddl_alter.go          ALTER TABLE ADD/DROP COLUMN
ddl_database.go       CreateDatabase
ddl_compact.go        CompactDatabase
internal/zlib1/       A deflate encoder bit-compatible with C zlib level 1
cmd/absdb/            The CLI
testdata/             Fixtures (gitignored bar an allowlist)
```

The ComponentAce SDK is deliberately not in this list: it lives outside the repository as
`<sdk>`, never inside it.

See [provenance.md](provenance.md) for what may and may not be committed, and why.
