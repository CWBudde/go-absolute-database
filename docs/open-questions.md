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
- **A second PFS or EAM page.** The engine's `PfsPageNoForPageNo` says they recur; one PFS
  payload addresses 32 448 pages at 4096 bytes and the largest file in the corpus is 78.
- **Page sizes other than 4096 and 2048.** The payload model is expressed in terms of
  `PageSize` and the fixed header offset `0x17C`; if that offset were proportional to the page
  size rather than fixed, another page size would break it. Two sizes agree that it is fixed.
- **Version differences.** The corpus spans 5.13, 7.10, 7.61 and 7.94 with no structural
  difference isolated.

## Validation gaps

- **Fourteen field types have zero corpus coverage** — Single, SmallInt, Word, Int64, Currency,
  WideString, GUID, Extended, VarBytes, Bytes, TimeStamp, Date, Time, DateTime. They are
  correct by construction and covered only by hand-built records. Closing this needs a
  round-trip against the Delphi engine: create a table with known values in every type, export,
  compare.
- **BZIP and PPM BLOB compression** are named by the format and unimplemented; no fixture uses
  either.

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
