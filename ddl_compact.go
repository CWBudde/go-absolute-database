package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// COMPACT DATABASE -- a rebuild, not a defragment.
//
// Three independent sources agree on what the engine's Database -> Compact
// Database does. TABSDatabase.CompactDatabase routes to InternalCopyDatabase;
// the SDK help calls the result "a new compact copy of a database"; and
// DBManager's own handler (legacy/Utils/Source/DBManager/main.pas:1420) closes
// the file, calls the no-argument overload and reopens it. The bytes settle it
// beyond doubt. testdata/MultiTable-dropcompact.abs is Compact Database run on
// MultiTable-drop.abs, which had twelve free pages:
//
//	                  before  after
//	pages             30      18, none free
//	LastUsedPageNo    23      17
//	file State        12       6   -- reset, not continued
//	LastObjectID      11       7   -- ids reallocated; Gamma goes from 8 to 5
//
// A file whose transaction counter has been reset and whose object ids have
// been renumbered is a new file, so this is implemented as exactly what it is:
// CreateDatabase, then per table in catalog order CreateTable, CreateIndex for
// each of its indexes, and a copy of every row, reusing ddl_create.go and
// ddl_index.go unchanged.
//
// The output is the engine's, page for page
//
// The rebuild is not merely plausible: replaying it reproduces the engine's
// file down to every counter except the ones no writer can reproduce.
// TestCompactDatabaseMatchesEnginePageLayout asserts it, and the arithmetic is
// worth writing down because it is what pins the order of the three steps per
// table.
//
// A fresh database is six pages with one free, so Alpha's CREATE TABLE grows it
// by one extent to fourteen and takes pages 5-9; its CREATE INDEX takes 10; its
// two rows take data page 11. Gamma's CREATE TABLE takes 12 and 13, grows the
// file again for 14, and takes 15 and 16; its row takes 17. That is the
// engine's page sequence exactly, type for type -- including the index page
// landing before the data page, which is what says the engine creates a table's
// indexes before it copies its rows rather than after.
//
// The counters follow from the same replay. Page 0's State ends at 18, one per
// Page Free Space bit set, which is what the engine's carries. Page 1's ends at
// 5: extent 0 goes partial at page 0 and full at page 7, extent 1 partial at 8
// and full at 15, extent 2 partial at 16 -- five entry changes, which is what
// the engine's carries. The file State ends at 6: one transaction for the
// creation, one per CREATE TABLE, one for the CREATE INDEX, one per table's
// rows. And LastObjectID ends at 7: Alpha 1, its columns 2 and 3, its index 4,
// Gamma 5, its columns 6 and 7.
//
// The one step that does not follow from the replay is the last. Growing by
// whole extents leaves the file 22 pages long, and the engine's is 18 -- which
// is LastUsedPageNo+1, with no free page at all. So the copy ends by shortening
// the file to the pages actually in use (shrinkToLastUsedPage). That is the
// "compact" in Compact Database, and MultiTable-dropcompact.abs is the only
// evidence in the corpus for the engine shortening a file at all, which is why
// DROP TABLE still refuses to.
//
// Byte identity, and the sixteen bytes it costs
//
// Every page of the output is newly allocated, so every one of them carries a
// State the engine seeded randomly and no writer can reproduce (FINDING 1,
// ddl.go). Nothing else diverges: the object ids come out identical because the
// replay hands them out in the engine's own order, and so does every other
// byte. TestCompactDatabaseMatchesEngineByteForByte therefore holds compaction
// to the same standard as CREATE TABLE -- reproduce the engine's own file,
// excluding exactly the State words of the sixteen pages the rebuild allocates,
// pages 2 through 17. Pages 0 and 1 are not excluded: their States are the two
// counters above, and they match.
//
// TestCompactDatabaseMatchesEngineSemantically is kept alongside it, in the
// register of TestAlterTableMatchesEngineSemantically: it says which property a
// future divergence broke -- tables, columns, rows or index contents -- where
// the byte comparison can only say which offset moved.
//
// What stays refused
//
// Compaction re-creates a table through CreateTable and CreateIndex, so it can
// only handle what those two can write, and anything else is refused before the
// destination file is created rather than silently dropped from it: an
// encrypted source (ErrEncryptionUnsupported, since CreateDatabase cannot write
// the key material), a column type CREATE TABLE has no corpus evidence for
// (ErrUnsupportedColumnType), a schema tail that does not parse
// (ErrSchemaTailNotUnderstood), constraint records, which a re-created table
// would lose (ErrConstraintsNotRebuilt), and an index that is not the plain,
// ascending, case-sensitive, single-column Int32 index CreateIndex builds
// (ErrIndexNotMaintained, ErrMultiColumnIndex or ErrUnsupportedIndexColumn).
// Losing an index is not an acceptable outcome of a compaction, so a
// string-keyed index -- which CreateIndex cannot build -- refuses the whole
// operation.

// ErrConstraintsNotRebuilt reports a compaction of a table carrying constraint
// records. A table this package re-creates is built by CreateTable, whose
// schema stream carries an empty constraint array, so compacting such a table
// would quietly return a database that no longer enforces its NOT NULL, PRIMARY
// KEY, UNIQUE or MINVALUE/MAXVALUE rules. Refusing is the only outcome that
// cannot lose them.
var ErrConstraintsNotRebuilt = errors.New("absdb: table carries constraint records a rebuild would lose")

// compactIndex is one index CompactDatabase re-creates: its name and the single
// column it keys on.
type compactIndex struct {
	name   string
	column string
}

// compactTable is one table's whole definition, which is everything CreateTable
// and CreateIndex need to rebuild it in an empty database.
type compactTable struct {
	name    string
	columns []Column
	indexes []compactIndex
}

// CompactDatabase writes a compacted copy of the database at srcPath to
// dstPath, which must not already exist.
//
// It is the engine's own Database -> Compact Database: a rebuild into a new
// file rather than a defragment in place. The result holds the same tables, in
// the same order, with the same columns, rows and indexes, on the smallest run
// of pages they fit on -- no free page at all -- with a reset transaction
// counter and freshly allocated object ids. See this file's comment for the
// evidence, and for what the rebuild reproduces of the engine's own output.
//
// The source is opened read-only and is never modified. Everything the rebuild
// cannot reproduce is refused before dstPath is created, so a refusal leaves no
// file behind; see "What stays refused" in this file's comment for the list.
func CompactDatabase(srcPath, dstPath string) error {
	src, err := Open(srcPath)
	if err != nil {
		return err
	}

	defer src.Close()

	plan, err := planCompaction(src)
	if err != nil {
		return err
	}

	opts, err := src.compactionGeometry()
	if err != nil {
		return err
	}

	dst, err := CreateDatabase(dstPath, opts)
	if err != nil {
		return err
	}

	err = rebuildInto(src, dst, plan)

	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		// CreateDatabase created dstPath exclusively, so the file being removed
		// is the one this call made and nothing else.
		os.Remove(dstPath)

		return err
	}

	return nil
}

// compactionGeometry reads the source's geometry back out of it, so a
// compaction preserves the settings the database was created with. Page size
// and extent size are header fields; the connection count is not, and comes
// from the page-3 internal file that holds it (see ddl_database.go).
func (db *File) compactionGeometry() (CreateDatabaseOptions, error) {
	connections, err := db.maxConnections()
	if err != nil {
		return CreateDatabaseOptions{}, err
	}

	return CreateDatabaseOptions{
		PageSize:          db.PageSize(),
		PageCountInExtent: int(db.pagesInExtent),
		MaxConnections:    connections,
	}, nil
}

// maxConnections returns the size of the database's connection/lock table,
// which is the Max Connections setting it was created with. Empty-mc100.abs
// pins that this internal file's Size field is the only place the value is
// recorded.
func (db *File) maxConnections() (int, error) {
	pageNo, err := db.findPageByType(pageTypeConnectionTable)
	if err != nil {
		return 0, err
	}

	if pageNo < 0 {
		return 0, fmt.Errorf("%w: the file holds no connection table page", ErrBadGeometry)
	}

	page, err := db.ReadPage(pageNo)
	if err != nil {
		return 0, err
	}

	data := page.PageData()
	if len(data) < internalFileHeaderSize {
		return 0, fmt.Errorf("%w: page %d holds no internal file header", ErrBadGeometry, pageNo)
	}

	size := int32(binary.LittleEndian.Uint32(data[1:5]))
	if size < 1 {
		return 0, fmt.Errorf("%w: connection table of %d bytes", ErrBadGeometry, size)
	}

	return int(size), nil
}

// planCompaction reads every table's whole definition and refuses anything the
// rebuild cannot reproduce. It writes nothing: the whole plan is validated
// before CreateDatabase is called, so a refusal never leaves a partial
// destination behind.
func planCompaction(db *File) ([]compactTable, error) {
	if db.Encrypted() {
		return nil, fmt.Errorf("%w: the source database is encrypted", ErrEncryptionUnsupported)
	}

	tables, err := db.Tables()
	if err != nil {
		return nil, err
	}

	plan := make([]compactTable, 0, len(tables))

	for _, info := range tables {
		t, err := db.Table(info.Name)
		if err != nil {
			return nil, err
		}

		entry, err := planCompactTable(db, t)
		if err != nil {
			return nil, fmt.Errorf("absdb: compacting table %q: %w", info.Name, err)
		}

		plan = append(plan, entry)
	}

	return plan, nil
}

// planCompactTable collects one table's columns and indexes, refusing every
// shape CreateTable or CreateIndex would not write.
func planCompactTable(db *File, t *Table) (compactTable, error) {
	schema, err := t.Schema()
	if err != nil {
		return compactTable{}, err
	}

	for _, c := range schema.Columns {
		// The same check CreateTable's serializer makes, made here so that a
		// column it cannot write refuses the compaction before any file exists.
		if want, ok := knownColumnTypes[c.BaseType]; !ok || want != c.FieldType || c.IsBLOB() {
			return compactTable{}, fmt.Errorf("%w: column %q is base type %d / field type %s",
				ErrUnsupportedColumnType, c.Name, c.BaseType, c.FieldType)
		}
	}

	schemaPageNo, err := t.schemaPageNo()
	if err != nil {
		return compactTable{}, err
	}

	raw, err := db.readSchemaStream(schemaPageNo)
	if err != nil {
		return compactTable{}, err
	}

	records, constraints, err := schemaTailArrays(raw)
	if err != nil {
		return compactTable{}, err
	}

	if len(constraints) > 0 {
		return compactTable{}, fmt.Errorf("%w: %d of them", ErrConstraintsNotRebuilt, len(constraints))
	}

	indexes, err := planCompactIndexes(schema, records)
	if err != nil {
		return compactTable{}, err
	}

	return compactTable{name: t.Name(), columns: schema.Columns, indexes: indexes}, nil
}

// schemaTailArrays is parseSchemaTail reduced to the two record arrays a
// rebuild cares about. CREATE INDEX and DROP INDEX splice around the tail and
// need its offsets; a caller that writes the whole table again from its
// definition needs only what is in it.
//
// what this wrapper exists to drop, and dropping them here means no caller has
// to.
//
//nolint:dogsled // the three offsets parseSchemaTail also returns are exactly
func schemaTailArrays(raw []byte) ([]indexRecord, []constraintRecord, error) {
	_, _, records, constraints, _, err := parseSchemaTail(raw)

	return records, constraints, err
}

// planCompactIndexes checks every index record against what CreateIndex builds:
// a plain, ascending, case-sensitive index over one Int32 column.
// maintainableIndexColumn is the same gate the record writer puts in front of
// index maintenance, which is not a coincidence -- the rows are copied through
// that writer, so an index it would not maintain could not be filled anyway.
func planCompactIndexes(schema *TableSchema, records []indexRecord) ([]compactIndex, error) {
	indexes := make([]compactIndex, 0, len(records))

	for _, rec := range records {
		col, err := maintainableIndexColumn(rec)
		if err != nil {
			return nil, err
		}

		colIdx, err := findColumnIndex(schema, col.name)
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", rec.name, err)
		}

		if c := schema.Columns[colIdx]; c.BaseType != BftInt32 || c.FieldType != FieldInteger {
			return nil, fmt.Errorf("%w: index %q keys %q, which is base type %d / field type %s",
				ErrUnsupportedIndexColumn, rec.name, c.Name, c.BaseType, c.FieldType)
		}

		indexes = append(indexes, compactIndex{name: rec.name, column: col.name})
	}

	return indexes, nil
}

// rebuildInto replays the plan into the freshly created destination, table by
// table in catalog order, and shortens the file to what it ended up using.
//
// The order within a table -- create, index, then rows -- is the engine's own,
// read off the page sequence of MultiTable-dropcompact.abs; see this file's
// comment.
func rebuildInto(src, dst *File, plan []compactTable) error {
	for _, tbl := range plan {
		if err := dst.CreateTable(tbl.name, tbl.columns); err != nil {
			return err
		}

		for _, idx := range tbl.indexes {
			if err := dst.CreateIndex(tbl.name, idx.name, idx.column); err != nil {
				return err
			}
		}

		if err := copyTableRows(src, dst, tbl.name); err != nil {
			return err
		}
	}

	return dst.shrinkToLastUsedPage()
}

// copyTableRows copies every row of one table, in the order the source reads
// them out, as a single transaction -- which is what leaves the destination's
// header State where the engine leaves it, one bump per table rather than one
// per row.
//
// Values go through decodeRowValues, the same decode/re-encode pair ALTER TABLE
// rewrites its rows with, so a value read here and written there reproduces the
// same stored bytes.
func copyTableRows(src, dst *File, name string) error {
	srcTable, err := src.Table(name)
	if err != nil {
		return err
	}

	reader, err := srcTable.Open()
	if err != nil {
		return err
	}

	dstTable, err := dst.Table(name)
	if err != nil {
		return err
	}

	writer, err := dstTable.OpenWriter()
	if err != nil {
		return err
	}

	defer writer.Close()

	columns := reader.Schema().Columns

	for reader.Next() {
		values, err := decodeRowValues(columns, reader.Record())
		if err != nil {
			return fmt.Errorf("absdb: copying %q: %w", name, err)
		}

		if _, err := writer.Insert(values); err != nil {
			return fmt.Errorf("absdb: copying %q: %w", name, err)
		}
	}

	if err := reader.Err(); err != nil {
		return err
	}

	return writer.Commit()
}

// shrinkToLastUsedPage shortens the file to exactly the pages it is using:
// LastUsedPageNo+1 of them, with no free page past the end. It is the last step
// of a compaction and the only place this package shortens a file at all, since
// MultiTable-dropcompact.abs is the only evidence in the corpus for the engine
// doing so.
//
// It never goes below freshDatabasePageCount, which is not a rounding but the
// other end of the same evidence. A database with no tables uses five pages and
// the engine's own Create Database still writes six, spare page and all
// (Empty.abs), so six is what the engine considers a database's floor. Cutting
// to five would produce a shape no fixture shows; keeping the floor makes
// compacting an empty database reproduce Empty.abs exactly, which
// TestCompactDatabaseOfAnEmptyDatabaseIsAFreshOne asserts.
//
// The order of the two steps is the reverse of extendFile's, for the same
// reason: a page must never be announced without being present, so growth
// lengthens before it announces and shrinking un-announces before it removes.
//
// It refuses if the allocation map still marks anything at or past the new end
// allocated. That cannot happen while LastUsedPageNo is the highest allocated
// page, which allocatePages and releasePages both maintain -- which is exactly
// why it is worth checking before a truncation, the one operation here that
// destroys data if the invariant ever fails to hold.
func (db *File) shrinkToLastUsedPage() error {
	if !db.writable {
		return ErrReadOnly
	}

	total := max(int(db.lastUsedPageNo)+1, freshDatabasePageCount)
	if total >= db.PageCount() {
		return nil
	}

	page, err := db.ReadPage(pfsPageNo)
	if err != nil {
		return err
	}

	for no := total; no < db.PageCount(); no++ {
		if pfsAllocated(page.PageData(), no) {
			return fmt.Errorf("absdb: page %d is allocated past LastUsedPageNo %d",
				no, db.lastUsedPageNo)
		}
	}

	if err := db.setTotalPageCount(total); err != nil {
		return err
	}

	size := int64(total)*int64(db.pageSize) + diskPageHeaderOffset

	if err := db.f.Truncate(size); err != nil {
		return fmt.Errorf("absdb: shortening the file to %d pages: %w", total, err)
	}

	db.size = size

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing the shortened file: %w", err)
	}

	return nil
}
