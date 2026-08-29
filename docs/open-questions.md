# Open questions

What the format still hides, and what this package therefore refuses rather than guesses.

## Undecoded

- **The 260-byte encrypted control block** at database header offsets 80..339. Located, not
  decoded. It is why `CreateDatabase` refuses `Encrypted: true` and why compaction refuses an
  encrypted database.
- **The 18-byte block** the third header size field introduces at offset 358. Zero in every
  fixture.
- **The type-7 page** each table's catalog entry names at offset 268. Its role is unidentified,
  so it is not exported.
- **Constraint record kind 1.** Not observed anywhere in the corpus.
- **What the constraint body's leading count counts.** It is 1 in every observed record.

## Unverified by any file

- **Multi-page BLOB chaining.** Every BLOB page in the corpus has `NextPageNo == -1`, so
  `readBlobChain` is guarded against cycles but has never run against real data.
- **Several BLOBs on one page.** `ItemNo` is parsed and not used to select among them.
- **Splitting a B-tree leaf.** Multi-level trees themselves _are_ in the corpus and are read
  correctly — five of them, all depth 2, see [`format/indexes.md`](format/indexes.md#capacity-and-splitting).
  What no file shows is the engine performing the split: the fullest observed leaf holds 232 of a
  possible 367, so the split point is not "leaf full", and the rule is unknown. Every write path
  refuses a multi-level tree.
- **The column definition's autoinc block ever varying.** The five `TABSFieldDef` autoinc fields
  decoded in [format/schema.md](format/schema.md#column-definition) carry `increment 1`,
  `initial 0`, `min 0`, `max High(Int64)`, `cycled False` in all 495 column definitions in the
  corpus — the AutoInc columns included, so even they are stored with the defaults. The reading
  rests on `TABSFieldDef`'s declaration order plus a byte-exact fit that leaves nothing over, not
  on having seen the fields hold anything else. A single `CREATE TABLE` with explicit
  `INCREMENT`/`INITIALVALUE`/`MAXVALUE` options would confirm it outright.
- **A second PFS or EAM page.** The engine's `PfsPageNoForPageNo` says they recur; one PFS
  payload addresses 32 448 pages at 4096 bytes and the largest file in the corpus is 78.
- **Page sizes other than 4096 and 2048.** The payload model is expressed in terms of
  `PageSize` and the fixed header offset `0x17C`; if that offset were proportional to the page
  size rather than fixed, another page size would break it. Two sizes agree that it is fixed.
- **Version differences.** The corpus spans 5.13, 7.10, 7.61 and 7.94 with no structural
  difference isolated.

## Validation gaps

- ~~**Fourteen field types have zero corpus coverage.**~~ Closed by `testdata/Types.abs`, which
  is that round-trip against the engine. It cost four corrections — see
  [format/records.md](format/records.md#what-typesabs-settled) — and what remains of the gap is
  narrower and named below.
- **What a `Bytes` or `VarBytes` column's extra byte holds.** Both store `Size + 1`, and no such
  column in any fixture holds a value. `Types2.abs` is eleven attempts at producing one — see
  [format/records.md](format/records.md#bytes-and-varbytes) — and the reason they fail is now
  known: `MIMETOBIN` builds a BLOB value, not a fixed-width one. A value needs a parameterised
  insert, which means driving the Delphi engine rather than DBManager's SQL tab. Until then this
  package leaves the byte zero, which reads back correctly here and is not known to be what the
  engine writes.
- **Whether a TimeStamp could ever carry minutes.** The engine keeps only year, month, day and
  hour, because the remaining `TSQLTimeStamp` fields do not fit the eight bytes a `BftDateTime`
  column gets — that much `Types2.abs` settles. What it does not say is whether some other path
  into the engine (a parameterised insert again) stores something else; the one SQL literal
  carrying a fraction was rejected by the parser.
- **BZIP and PPM BLOB compression** are named by the format and unimplemented; no fixture uses
  either.
- **What the engine does with a row a constraint forbids.** A rejected write leaves no file
  behind, so no fixture can show it, and the two checks in `writer_constraint.go` are therefore
  held to the narrower standard of never passing a row the constraint's own bytes forbid. Two
  rules are assumed rather than observed, both of which can only make this package _accept_ a
  row: a `NULL` passes a `CHECK` constraint (what SQL says, and what would otherwise make a
  nullable `MINVALUE` column unwritable), and a bound is inclusive, so `MINVALUE 0` admits 0.
  A parameterised insert against the Delphi engine would settle both.
- **Whether the engine writes anything else when inserting into a `UNIQUE` or `PRIMARY` index.**
  All four `Writes-idx*` fixtures carry a plain index, so the leaf splice for a key-enforcing one
  has no byte identity behind it. This is why a `PRIMARY KEY` or `UNIQUE` constraint still
  refuses every write (`ErrConstraintsNotEnforced`) even though the duplicate scan it would need
  is straightforward: the constraint and the index that implements it have to lift together.

## Deliberate divergences

Not unknowns — decisions, recorded so they can be revisited on purpose.

- **`ALTER TABLE` splices in place** where the engine rebuilds the table. See
  [writing.md](writing.md#alter-table--a-deliberate-divergence). The original reason (the
  rebuild needs six free pages and nothing could grow a file) has expired now that growth
  exists, and compaction has since shown object-id replay works. What remains is the work of
  reproducing the four-transaction sequence.
- **Compaction refuses a table carrying constraint records** (`ErrConstraintsNotRebuilt`),
  because `CREATE TABLE` cannot write them back and silently dropping a `NOT NULL` or a
  `PRIMARY KEY` is worse than refusing. Constraint records are read; nothing writes them. That
  one gap is what keeps compaction off most real tables.
