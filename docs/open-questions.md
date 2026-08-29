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
- **What the engine does with a row a `NOT NULL` or `CHECK` constraint forbids.** A rejected
  write leaves no file behind, so no fixture can show it, and the two checks in
  `writer_constraint.go` are therefore held to the narrower standard of never passing a row the
  constraint's own bytes forbid. Two rules are assumed rather than observed, both of which can
  only make this package _accept_ a row: a `NULL` passes a `CHECK` constraint (what SQL says,
  and what would otherwise make a nullable `MINVALUE` column unwritable), and a bound is
  inclusive, so `MINVALUE 0` admits 0. A parameterised insert against the Delphi engine would
  settle both.

  The same gap for a **key** constraint is closed. Three refused statements against `Keys.abs`
  each left the file byte-identical to its parent, transaction counter included, and the Log
  tab named the constraint and the native error; `testdata/README.md` lists them. That is why
  the checks run before any page is touched.

- ~~**Whether the engine writes anything else when inserting into a `UNIQUE` or `PRIMARY`
  index.**~~ Closed by the `Keys*.abs` family: it writes nothing else. Seven pairs are
  byte-identical with no `State` exclusion, and the leaf splice is the plain index's exactly.
  What the fixtures did change is two things nobody would have guessed — a `NULL` key sorts
  _before_ every value, and a `UNIQUE` index treats a second `NULL` as a duplicate. See
  [format/indexes.md](format/indexes.md#key-enforcing-indexes).
- **Whether an `AUTOINC` column has anything a write must maintain.** This is the gap that now
  stands between the write path and most of the private corpus: fifteen of its twenty-five key
  constraints are backed by a single-column, single-page index over an `AUTOINC` column, whose
  index record and leaf are the `Int32` shape exactly. `Auto-ins.abs` says there is no counter
  page — an `AUTOINC` insert touches the same page types an `Int32`-keyed one does — but it
  says nothing about how the engine picks the next value when a row is inserted **without**
  one, which is the case DBManager's SQL tab exercises and a caller of this package does not.
  Until that is settled, `describeIndex` refuses the column's field type.

## Deliberate divergences

Not unknowns — decisions, recorded so they can be revisited on purpose.

- **`ALTER TABLE` splices in place** where the engine rebuilds the table. See
  [writing.md](writing.md#alter-table--a-deliberate-divergence). The original reason (the
  rebuild needs six free pages and nothing could grow a file) has expired now that growth
  exists, and compaction has since shown object-id replay works. What remains is the work of
  reproducing the four-transaction sequence.
- **Compaction refuses a table whose key covers more than one column**, or whose key column an
  index leaf is not built for (`ErrConstraintsNotRebuilt`). A single-column `Int32` key is
  rebuilt now, index and all. What is left is the same two shapes index maintenance refuses,
  which is not a coincidence: the rows are copied through that writer, so an index it would not
  maintain could not be filled anyway.
