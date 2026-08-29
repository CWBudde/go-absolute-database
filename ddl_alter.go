package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// ALTER TABLE ADD COLUMN and ALTER TABLE DROP COLUMN.
//
// Read this file's honesty warning before trusting it more than the corpus
// does. These two operations are the only writes in this package that do not
// reproduce the engine's bytes, and that is a decision rather than a gap.
//
// The fixtures exist: MultiTable-alteradd.abs and MultiTable-alterdrop.abs
// are what DBManager wrote for the two statements below, run against
// MultiTable.abs. They show the engine does not edit a table in place at all.
// It runs CREATE TABLE <temp> / copy the rows / rename <temp> to the original
// name / DROP TABLE the original -- four transactions, three catalog writes,
// a new object id for the table and every one of its columns, the old pages
// tombstoned and six new ones allocated. ddl_alter_test.go's own section
// comment lays out the counter-by-counter evidence, and
// TestEngineAlterTableRebuildsTheTable pins it.
//
// Reproducing that sequence was considered and rejected, and the reason has
// since expired. It needed six free pages, and at the time nothing here could
// grow a database: MultiTable.abs is the only file in the corpus that has
// six, Writes.abs has three, every Employees-*.abs has two, and the customer
// fixtures have between none and five, so an engine-faithful ALTER TABLE
// would have been byte-perfect on one fixture and refused on every other file
// this package exists to read. The splice below works on all of them.
//
// ddl_grow.go has since removed that constraint -- the file extends by whole
// extents on demand -- so the rebuild is no longer blocked, merely not done.
// It would still have to reproduce all four transactions, the three catalog
// writes and a fresh set of object ids, which is a good deal more than
// allocating six pages. Recorded as an open question so the reasoning does
// not outlive the constraint that produced it.
//
// So the guarantee here is weaker than DropTable's or CreateTable's, and
// differently shaped: not byte identity, but semantic identity against the
// engine's own output for the same statement -- same tables, same columns,
// same rows, checked by TestAlterTableMatchesEngineSemantically -- plus
// round-trip correctness and the B-tree leaf oracle. Column ids are the one
// thing that legitimately differs, because the engine's rebuild reallocates
// them and this splice does not.
//
// Two edits, and the second is the one with teeth:
//
//  1. The column-definition stream (page type 8, zlib-compressed) is edited
//     surgically: schemaColumnSpans locates each column definition's exact
//     byte range using parseColumnDef -- the same function Schema() uses to
//     read the file -- so the splice point is derived, not guessed. Only the
//     bytes between the columnCount field and the first byte past the last
//     column definition are touched. Everything from there on (indexCount,
//     index records, constraint records, the reserved field, and the two
//     trailing page numbers -- see
//     /home/christian/.claude/jobs/61b3bdd6/tmp/index-definition-format.md)
//     is copied through unexamined and unmodified. That tail's own internal
//     layout does not matter to this splice: it only ever moves as a whole
//     block, never parsed except where DropColumn needs to know which
//     columns an index covers (columnCoveredByIndex, below).
//
//  2. Every existing record is decoded under the OLD schema and re-encoded
//     under the NEW one (rewriteDataPages), because nullFlagBytes -- and
//     therefore the byte offset of every field in every record -- depends on
//     the column count. A column count crossing a multiple of 8 changes the
//     null-flag prefix width and shifts every field in every record on every
//     page, not just the field being added or removed. This is exactly the
//     shape of the null-flag sizing bug docs/format/records.md guards
//     against, so
//     TestAlterTableColumnCountBoundary constructs tables that cross a
//     multiple of 8 deliberately rather than relying on the corpus to happen
//     to contain one.
//
// What this file refuses rather than guesses at, and why:
//
//   - a column type serializeColumnDef (ddl_create.go) has no corpus
//     evidence for: reusing that function rather than writing a second
//     serializer is what keeps ADD COLUMN's on-disk column definition
//     byte-identical in shape to CREATE TABLE's, for the two type
//     combinations either has evidence for.
//   - a table with BLOB pages: nothing here can rewrite a record carrying a
//     live BLOB reference safely, matching DropTable's own
//     ErrTableHasBlobPages refusal.
//   - dropping a column an index covers, or one a constraint names: both
//     records embed their covered columns' names (columnCoveredByIndex,
//     columnNamedByConstraint), which is precise enough to check rather than
//     refusing on any indexed or constrained table outright, the way
//     writer.go's ErrIndexNotMaintained does for insert/delete.
//   - a record that would not fit its page after the column change: no
//     page-splitting exists in this package (see writer.go's ErrTableFull),
//     so a page that cannot hold its own existing rows under the new record
//     width is a hard refusal, checked before any byte is written.
//
// Neither operation touches the table catalog, a table's counters file, or
// any index tree: RecordID identity -- the (page, slot) pair a B-tree leaf
// entry references -- is preserved exactly, because rewriteDataPages never
// moves a record to a different slot. That is what lets an existing index
// keep pointing at the right rows across the schema change without this
// package having to touch the index pages at all.
var (
	// ErrColumnExists reports an AddColumn whose column name is already used
	// by the table, case-insensitively -- the same matching Schema/DropColumn
	// use for every other name lookup in this package.
	ErrColumnExists = errors.New("absdb: column already exists")

	// ErrLastColumn reports a DropColumn that would leave the table with no
	// columns at all. No fixture or analysis says what the engine does with a
	// zero-column table, so this package refuses rather than create one.
	ErrLastColumn = errors.New("absdb: cannot drop a table's only column")

	// ErrColumnIndexed reports a DropColumn naming a column an index covers.
	// The index format does not let this package repair or rebuild the
	// index, so the column staying in place is the only safe outcome.
	ErrColumnIndexed = errors.New("absdb: column is covered by an index this package cannot repair")

	// ErrColumnConstrained reports a DropColumn naming a column a NOT NULL,
	// PRIMARY KEY, UNIQUE or MINVALUE/MAXVALUE constraint record covers.
	//
	// The record format is decoded now (ddl_constraint.go), so this is found
	// by parsing the constraint array and asking each record which columns it
	// covers, not by the text scan that used to stand in for it. What has not
	// changed is the outcome: dropping the column would leave a constraint
	// naming a column the file no longer has, and nothing in the corpus says
	// what the engine does with one. Reading the record is not the same as
	// knowing how to rewrite the array around a removal, and this package will
	// not guess at the second from the first. What would settle it: an
	// engine-produced ALTER TABLE ... DROP COLUMN fixture run against a
	// constrained table.
	ErrColumnConstrained = errors.New("absdb: column is named by a constraint this package cannot repair")

	// ErrRecordWontFit reports an existing record that would no longer fit
	// its page once re-encoded under the new column layout. It is
	// deliberately distinct from ErrTableFull: that sentinel means no free
	// slot exists, this one means an occupied slot's own record has grown
	// past what the page can hold. Neither implies page splitting, which
	// this package does not do.
	ErrRecordWontFit = errors.New("absdb: an existing record would not fit its page after the column change")
)

// AddColumn appends a new column to the end of a table's schema. Every
// existing row reads back with the new column NULL.
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when the table already has a column of that name,
// when the column's type has no corpus evidence backing its on-disk encoding
// (ErrUnsupportedColumnType), when the table owns BLOB pages
// (ErrTableHasBlobPages), or when adding the column would make an existing
// record too wide for its page (ErrRecordWontFit).
func (db *File) AddColumn(table string, column Column) error {
	if !db.writable {
		return ErrReadOnly
	}

	t, err := db.Table(table)
	if err != nil {
		return err
	}

	schemaPage, err := t.schemaPageNo()
	if err != nil {
		return err
	}

	if err := db.refuseBlobPages(schemaPage, t.info.Name); err != nil {
		return err
	}

	oldSchema, err := t.Schema()
	if err != nil {
		return err
	}

	newCol, newColDef, err := prepareNewColumn(db, oldSchema, column)
	if err != nil {
		return err
	}

	newColumns := make([]Column, 0, len(oldSchema.Columns)+1)
	newColumns = append(newColumns, oldSchema.Columns...)
	newColumns = append(newColumns, newCol)
	newSchema := &TableSchema{Columns: newColumns}

	w := newPageEdit(db)

	if err := db.spliceSchemaColumns(w, schemaPage, -1, newColDef); err != nil {
		return err
	}

	if err := db.rewriteDataPages(w, t, oldSchema, newSchema, func(old []any) []any {
		return append(append([]any(nil), old...), nil)
	}); err != nil {
		return err
	}

	if err := db.commitAlter(w); err != nil {
		return err
	}

	if err := db.setLastObjectID(int32(newCol.ID)); err != nil {
		return err
	}

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing ADD COLUMN on %q: %w", table, err)
	}

	return nil
}

// prepareNewColumn refuses a duplicate name and returns column with its ID
// and Position assigned the way CreateTable assigns them (see
// assignColumnIDs), together with its serialized column definition.
func prepareNewColumn(db *File, oldSchema *TableSchema, column Column) (Column, []byte, error) {
	for _, c := range oldSchema.Columns {
		if strings.EqualFold(c.Name, column.Name) {
			return Column{}, nil, fmt.Errorf("%w: %q", ErrColumnExists, column.Name)
		}
	}

	newCol := column
	newCol.ID = uint32(int(db.lastObjectID) + 1) //nolint:gosec // small object ids
	newCol.Position = len(oldSchema.Columns)

	newColDef, err := serializeColumnDef(newCol)
	if err != nil {
		return Column{}, nil, err
	}

	return newCol, newColDef, nil
}

// commitAlter flushes a schema operation's buffered pages and advances the
// database's transaction State counter, the tail both AddColumn and
// DropColumn share once their own work is done.
func (db *File) commitAlter(w *pageEdit) error {
	if err := db.flushPages(w.order, w.pages); err != nil {
		return err
	}

	return db.bumpFileState()
}

// DropColumn removes a column from a table. Every remaining column keeps its
// original value in every row.
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when the table has no such column, when it is the
// table's only column (ErrLastColumn), when an index covers the column
// (ErrColumnIndexed), when a constraint record names the column
// (ErrColumnConstrained), when the table owns BLOB pages
// (ErrTableHasBlobPages), or when dropping the column would still leave a
// record wider than its page (ErrRecordWontFit) -- a defensive check, since
// dropping a column only ever shrinks a record, but one this package makes
// rather than assumes.
//
// AddColumn carries no equivalent constraint check: a constraint record can
// only name a column that already exists, and AddColumn's new column has no
// name any existing constraint could already be referring to. Nothing in
// this package rewrites the constraint region either way -- it is always
// copied through byte for byte -- so the only way it can go stale is losing
// a column it still names, which is DropColumn's risk alone.
func (db *File) DropColumn(table, column string) error {
	if !db.writable {
		return ErrReadOnly
	}

	t, err := db.Table(table)
	if err != nil {
		return err
	}

	schemaPage, err := t.schemaPageNo()
	if err != nil {
		return err
	}

	if err := db.refuseBlobPages(schemaPage, t.info.Name); err != nil {
		return err
	}

	covered, err := db.columnCoveredByIndex(schemaPage, column)
	if err != nil {
		return err
	}

	if covered {
		return fmt.Errorf("%w: %q", ErrColumnIndexed, column)
	}

	constrained, err := db.columnNamedByConstraint(schemaPage, column)
	if err != nil {
		return err
	}

	if constrained {
		return fmt.Errorf("%w: %q", ErrColumnConstrained, column)
	}

	oldSchema, err := t.Schema()
	if err != nil {
		return err
	}

	dropIdx, err := columnToDrop(oldSchema, t.info.Name, column)
	if err != nil {
		return err
	}

	newSchema := &TableSchema{Columns: dropColumnAt(oldSchema.Columns, dropIdx)}

	w := newPageEdit(db)

	if err := db.spliceSchemaColumns(w, schemaPage, dropIdx, nil); err != nil {
		return err
	}

	if err := db.rewriteDataPages(w, t, oldSchema, newSchema, func(old []any) []any {
		out := make([]any, 0, len(old)-1)
		out = append(out, old[:dropIdx]...)

		return append(out, old[dropIdx+1:]...)
	}); err != nil {
		return err
	}

	if err := db.commitAlter(w); err != nil {
		return err
	}

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing DROP COLUMN on %q: %w", table, err)
	}

	return nil
}

// columnToDrop resolves column to its index among oldSchema.Columns,
// case-insensitively, and refuses a name that does not exist or names the
// table's only column.
func columnToDrop(oldSchema *TableSchema, tableName, column string) (int, error) {
	for i, c := range oldSchema.Columns {
		if !strings.EqualFold(c.Name, column) {
			continue
		}

		if len(oldSchema.Columns) == 1 {
			return 0, fmt.Errorf("%w: %q", ErrLastColumn, tableName)
		}

		return i, nil
	}

	return 0, fmt.Errorf("%w: %q", ErrNoSuchColumn, column)
}

// dropColumnAt returns cols with the column at dropIdx removed and every
// remaining column's Position renumbered to match its new index.
func dropColumnAt(cols []Column, dropIdx int) []Column {
	out := make([]Column, 0, len(cols)-1)

	for i, c := range cols {
		if i == dropIdx {
			continue
		}

		c.Position = len(out)
		out = append(out, c)
	}

	return out
}

// refuseBlobPages reports ErrTableHasBlobPages when the schema stream's
// blobIndexRoot trailer field names a real page, mirroring the check
// DropTable makes before touching a table's pages.
func (db *File) refuseBlobPages(schemaPage int, tableName string) error {
	blobRoot, err := db.schemaBlobIndexRoot(schemaPage)
	if err != nil {
		return err
	}

	if blobRoot >= 0 {
		return fmt.Errorf("%w: %q", ErrTableHasBlobPages, tableName)
	}

	return nil
}

// schemaBlobIndexRoot reads the second of the two trailing page numbers
// systemIndexRoots (ddl.go) also reads: the root of the index over a table's
// BLOB pages, or -1 when it stores no BLOBs. It is reimplemented here off a
// page number directly, rather than reusing systemIndexRoots, because that
// function re-resolves the table by name through db.Table -- redundant work
// AddColumn and DropColumn have already done once by the time they call this.
func (db *File) schemaBlobIndexRoot(schemaPage int) (int, error) {
	data, err := db.readInternalFilePages(schemaPage)
	if err != nil {
		return 0, err
	}

	raw, err := decompressInternalFile(data)
	if err != nil {
		return 0, err
	}

	if len(raw) < systemIndexRootsSize {
		return 0, fmt.Errorf("%w: %d bytes hold no index roots", ErrBadSchema, len(raw))
	}

	tail := raw[len(raw)-systemIndexRootsSize:]

	return int(int32(binary.LittleEndian.Uint32(tail[4:8]))), nil
}

// columnSpan is the byte range of one column definition within the
// decompressed schema blob, columnCount field excluded.
type columnSpan struct {
	start, end int
}

// schemaColumnSpans walks the column-definition array the same way
// parseSchema (schema.go) does, using the very same parseColumnDef, but keeps
// each column's byte range instead of decoding it into a Column. It is what
// lets spliceSchemaColumns cut or extend the array without touching a single
// byte of what follows it -- the index records, constraint records and
// trailer this package does not otherwise parse.
func schemaColumnSpans(data []byte) ([]columnSpan, int, error) {
	if len(data) < 4 {
		return nil, 0, ErrBadSchema
	}

	count := int64(binary.LittleEndian.Uint32(data[0:4]))
	if count > maxSchemaColumns {
		return nil, 0, fmt.Errorf("%w: invalid column count %d", ErrBadSchema, count)
	}

	if count > int64(len(data)/minColumnDefSize) {
		return nil, 0, fmt.Errorf("%w: column count %d exceeds %d bytes of schema data", ErrBadSchema, count, len(data))
	}

	spans := make([]columnSpan, 0, count)
	pos := 4

	for i := range int(count) {
		_, next, err := parseColumnDef(data, pos, i)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: column %d: %w", ErrBadSchema, i, err)
		}

		spans = append(spans, columnSpan{start: pos, end: next})
		pos = next
	}

	return spans, pos, nil
}

// spliceSchemaColumns rewrites a table's column-definition internal file:
// removing the column at dropIndex when it is >= 0, appending addColDef when
// it is non-nil (both may happen in principle, but AddColumn and DropColumn
// each only ever use one), and adjusting the leading columnCount field. Every
// byte from the first column definition's start to the last one's end may
// move; every byte from there on -- the tail schemaColumnSpans' second result
// marks the start of -- is copied through unexamined.
func (db *File) spliceSchemaColumns(w *pageEdit, schemaPage, dropIndex int, addColDef []byte) error {
	data, err := db.readInternalFilePages(schemaPage)
	if err != nil {
		return err
	}

	raw, err := decompressInternalFile(data)
	if err != nil {
		return err
	}

	spans, tailStart, err := schemaColumnSpans(raw)
	if err != nil {
		return err
	}

	if dropIndex >= len(spans) {
		return fmt.Errorf("%w: column index %d among %d", ErrBadSchema, dropIndex, len(spans))
	}

	count := len(spans)

	var body []byte

	switch {
	case dropIndex >= 0:
		body = append(body, raw[4:spans[dropIndex].start]...)
		body = append(body, raw[spans[dropIndex].end:tailStart]...)

		count--
	default:
		body = append(body, raw[4:tailStart]...)
	}

	if addColDef != nil {
		body = append(body, addColDef...)
		count++
	}

	newRaw := make([]byte, 0, 4+len(body)+len(raw)-tailStart)

	var head [4]byte

	binary.LittleEndian.PutUint32(head[:], uint32(count)) //nolint:gosec // bounded by maxSchemaColumns via schemaColumnSpans
	newRaw = append(newRaw, head[:]...)
	newRaw = append(newRaw, body...)
	newRaw = append(newRaw, raw[tailStart:]...)

	compressed, err := compressInternalFile(newRaw, 1)
	if err != nil {
		return err
	}

	return db.writeInternalFilePages(w, schemaPage, compressed)
}

// columnCoveredByIndex reports whether any index recorded in the schema
// tail's index array covers columnName, so DropColumn can refuse precisely
// instead of refusing on any indexed table.
//
// The parse itself is ddl_index.go's: parseIndexRecords/parseIndexRecord are
// shared with CREATE INDEX/DROP INDEX rather than duplicated, so there is one
// decode of this byte layout in the package, not two independently drifting
// ones. Unlike ddl_index.go's parseSchemaTail, this does not require the rest
// of the tail to parse -- a table whose constraint array this package cannot
// read is still safe to answer this question about, because DropColumn's
// splice (spliceSchemaColumns) never touches or moves that region.
//
// A multi-column index counts, which is the case this missed while
// parseIndexRecord refused coveredColumnCount > 1: back then such a table
// errored out of DropColumn rather than being answered, so nothing was lost;
// now it is answered, and every covered column of a multi-column index blocks
// the drop.
func (db *File) columnCoveredByIndex(schemaPage int, columnName string) (bool, error) {
	raw, tailStart, err := db.readSchemaTailStart(schemaPage)
	if err != nil {
		return false, err
	}

	if tailStart+4 > len(raw) {
		return false, fmt.Errorf("%w: schema tail too short for an index count", ErrBadSchema)
	}

	rawCount := int32(binary.LittleEndian.Uint32(raw[tailStart : tailStart+4]))
	if rawCount < 0 {
		return false, fmt.Errorf("%w: negative indexCount %d", ErrBadSchema, rawCount)
	}

	records, _, err := parseIndexRecords(raw, tailStart+4, int(rawCount))
	if err != nil {
		return false, fmt.Errorf("%w: index record array: %w", ErrBadSchema, err)
	}

	for _, rec := range records {
		if rec.coversColumn(columnName) {
			return true, nil
		}
	}

	return false, nil
}

// columnNamedByConstraint reports whether a constraint record names columnName,
// so DropColumn can refuse a column a NOT NULL, PRIMARY KEY, UNIQUE or
// MINVALUE/MAXVALUE clause still depends on.
//
// It prefers the decoded answer: parseSchemaTail reads the constraint array
// (ddl_constraint.go), and a record is checked against the columns it actually
// covers. That is a real narrowing -- before the array was decoded, the text
// scan below flagged any column whose name appeared anywhere in the tail, so a
// table with six NOT NULL constraints refused a drop of any of the six even
// when the seventh column was the one being dropped.
//
// When the tail does not parse the scan is still used, because a region this
// package cannot read is exactly where a conservative answer belongs.
func (db *File) columnNamedByConstraint(schemaPage int, columnName string) (bool, error) {
	raw, tailStart, err := db.readSchemaTailStart(schemaPage)
	if err != nil {
		return false, err
	}

	constraints, ok := tailConstraints(raw)
	if !ok {
		return scanConstraintMarkers(raw[tailStart:], columnName), nil
	}

	for _, c := range constraints {
		if c.namesColumn(columnName) {
			return true, nil
		}
	}

	return false, nil
}

// tailConstraints returns a schema stream's constraint array, and reports false
// when the tail does not parse.
//
// The parse error is deliberately dropped rather than propagated: a tail this
// package cannot read is a reason for its one caller to fall back to the
// conservative text scan, not a reason to fail a DropColumn the scan can answer
// safely on its own.
func tailConstraints(raw []byte) ([]constraintRecord, bool) {
	//nolint:dogsled // only the constraint array is wanted here
	_, _, _, constraints, _, err := parseSchemaTail(raw)

	return constraints, err == nil
}

// readSchemaTailStart decompresses a table's column-definition internal file
// and returns it together with the byte offset the tail (everything past the
// column-definition array) starts at -- the shared first step of
// columnCoveredByIndex and columnNamedByConstraint.
func (db *File) readSchemaTailStart(schemaPage int) ([]byte, int, error) {
	data, err := db.readInternalFilePages(schemaPage)
	if err != nil {
		return nil, 0, err
	}

	raw, err := decompressInternalFile(data)
	if err != nil {
		return nil, 0, err
	}

	_, tailStart, err := schemaColumnSpans(raw)
	if err != nil {
		return nil, 0, err
	}

	return raw, tailStart, nil
}

// scanConstraintMarkers conservatively text-scans tail for a constraint
// marker naming columnName. It does not parse the constraint record format,
// which this package does not know (see ErrColumnConstrained) -- it looks
// for the two-byte anchor "C_" that every observed constraint marker
// contains ($C_NotNull$<file>.abs$<Column>, $C_Unique$<Column>,
// C_PK$<Col1>[$<Col2>...]), reads the printable run around each occurrence
// (bounded by the first non-printable byte on either side, since every
// observed marker sits in a text run next to binary object ids and reserved
// fields), and checks whether columnName appears as one of that run's
// $-separated tokens.
//
// A marker whose column token cannot be matched this way still contains
// "C_", so callers that need the conservative fallback the analysis
// document's own judgement calls for -- refuse on any marker at all when
// precise bounds cannot be trusted -- can do so by checking bytes.Contains
// for "C_" directly; this function's precision is strictly additional, not a
// narrowing that a caller should rely on to prove a marker is safe to
// ignore.
func scanConstraintMarkers(tail []byte, columnName string) bool {
	for i := range tail {
		if i+1 >= len(tail) || tail[i] != 'C' || tail[i+1] != '_' {
			continue
		}

		start := i
		for start > 0 && isConstraintTextByte(tail[start-1]) {
			start--
		}

		end := i + 2
		for end < len(tail) && isConstraintTextByte(tail[end]) {
			end++
		}

		marker := string(tail[start:end])
		for tok := range strings.SplitSeq(marker, "$") {
			if strings.EqualFold(tok, columnName) {
				return true
			}
		}
	}

	return false
}

// isConstraintTextByte reports whether b could be part of the printable text
// a constraint marker's embedded string is made of: ASCII letters, digits,
// and the punctuation the corpus's marker strings use ('$', '.', '_'). It is
// a conservative subset of printable Windows-1252, not the full set, so a
// stray printable byte adjacent to the real string in the surrounding binary
// fields cannot silently extend the scanned token.
func isConstraintTextByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '$' || b == '.' || b == '_':
		return true
	default:
		return false
	}
}

// rewriteDataPages re-lays out every data page of a table: each occupied slot
// is decoded under oldSchema, passed through transform (which adds or drops
// one value), re-encoded under newSchema, and written back to the SAME slot
// index it started in.
//
// Keeping the slot index fixed, rather than compacting records into however
// many the new layout fits, is what preserves every RecordID -- and every
// B-tree leaf entry that references one as a (page, slot) pair -- across the
// schema change without this package having to touch a single index page.
// The cost is that a slot the new, wider layout cannot address at all (its
// offset would run past the page) is a hard refusal rather than a silent
// compaction: ErrRecordWontFit.
//
// Nothing is written to disk until the caller flushes w; an error here always
// leaves the file exactly as it was.
func (db *File) rewriteDataPages(w *pageEdit, t *Table, oldSchema, newSchema *TableSchema, transform func([]any) []any) error {
	dataPages, err := t.dataPages()
	if err != nil {
		return err
	}

	oldReader := &Reader{db: db, schema: oldSchema}
	oldReader.computeLayout()

	newReader := &Reader{db: db, schema: newSchema}
	newReader.computeLayout()

	for _, pageNo := range dataPages {
		buf, err := w.load(pageNo)
		if err != nil {
			return err
		}

		newPayload, err := rewriteDataPage(buf.payload, oldReader, newReader, transform)
		if err != nil {
			return fmt.Errorf("absdb: page %d: %w", pageNo, err)
		}

		copy(buf.payload, newPayload)
		buf.dirty = true
	}

	return nil
}

// rewriteDataPage builds one data page's new payload in a fresh buffer, so
// that decoding under the old layout never reads bytes this same call has
// already overwritten under the new one -- old and new record offsets
// overlap whenever nullFlagBytes changes.
func rewriteDataPage(oldPayload []byte, oldReader, newReader *Reader, transform func([]any) []any) ([]byte, error) {
	newPayload := make([]byte, len(oldPayload))

	for slot := range oldReader.recordsPerPage {
		start, ok := recordSlotStart(oldReader, oldPayload, slot)
		if !ok {
			continue
		}

		rec := Record{
			reader:    oldReader,
			nullFlags: oldPayload[start : start+oldReader.nullFlagBytes],
			fieldData: oldPayload[start+oldReader.nullFlagBytes : start+oldReader.recordSize],
		}

		oldValues, err := decodeRowValues(oldReader.schema.Columns, rec)
		if err != nil {
			return nil, fmt.Errorf("slot %d: %w", slot, err)
		}

		newRec, err := newReader.encodeRecord(transform(oldValues))
		if err != nil {
			return nil, fmt.Errorf("slot %d: %w", slot, err)
		}

		newStart := newReader.bitmapBytes + slot*newReader.recordSize
		if slot >= newReader.recordsPerPage || newStart+newReader.recordSize > len(newPayload) {
			return nil, fmt.Errorf("%w: slot %d", ErrRecordWontFit, slot)
		}

		copy(newPayload[newStart:newStart+newReader.recordSize], newRec)
		setBit(newPayload, slot, newReader.bitmapBytes)
	}

	return newPayload, nil
}

// recordSlotStart is recordStart (reader.go) reimplemented over an explicit
// payload rather than r.pageData, because rewriteDataPage reads a buffered
// page a TableWriter never opened a Reader against.
func recordSlotStart(r *Reader, payload []byte, slot int) (int, bool) {
	if slot < 0 || slot >= r.recordsPerPage || r.recordSize <= 0 {
		return 0, false
	}

	if !bitSet(payload, slot, r.bitmapBytes) {
		return 0, false
	}

	start := r.bitmapBytes + slot*r.recordSize
	if start+r.recordSize > len(payload) {
		return 0, false
	}

	return start, true
}

// decodeRowValues decodes every column of rec into the Go values encodeRecord
// accepts back, so a record can be decoded under one schema and immediately
// re-encoded under another. A NULL column decodes to nil; every other column
// is decoded through the same typed accessor Record itself exposes, so a
// value read this way and re-encoded reproduces the same stored bytes.
func decodeRowValues(cols []Column, rec Record) ([]any, error) {
	values := make([]any, len(cols))

	for i, c := range cols {
		if rec.IsNull(i) {
			continue
		}

		v, err := decodeColumnValue(c, rec, i)
		if err != nil {
			return nil, err
		}

		values[i] = v
	}

	return values, nil
}

// decodeColumnValue decodes one non-NULL column to the Go value type
// encode.go's encodeField expects for it. The BaseType set matches
// encodeField's own switch exactly -- BLOB/CLOB/WideCLOB columns never reach
// here because AddColumn and DropColumn both refuse a table with BLOB pages
// before rewriteDataPages runs; anything else unlisted (Extended, VarBytes)
// is exactly what writer.go's ErrColumnNotWritable already names.
func decodeColumnValue(c Column, rec Record, col int) (any, error) {
	// Mirrors encodeField's refusal: a GUID would otherwise decode through
	// the BftBytes arm and re-encode as plain bytes, so an ALTER on a table
	// carrying one has to stop here rather than rewrite the column.
	if c.FieldType == FieldGUID {
		return nil, fmt.Errorf("%w: column %q (%s)", ErrColumnNotWritable, c.Name, c.FieldType)
	}

	switch c.BaseType {
	case BftInt8, BftUint8, BftInt16, BftUint16, BftInt32, BftUint32, BftInt64, BftCurrency:
		// Record.Int64 sign-extends per column width and reads the Currency
		// column's raw scaled value, which encodeCurrency's integer branch
		// writes back byte for byte.
		return rec.Int64(col), nil
	case BftSingle, BftDouble:
		return rec.Float(col), nil
	case BftLogical:
		return rec.Bool(col), nil
	case BftChar, BftVarchar, BftWideChar, BftWideVarchar:
		return rec.String(col), nil
	case BftDate, BftTime, BftDateTime:
		return rec.Time(col), nil
	case BftBytes:
		return rec.Bytes(col), nil
	default:
		return nil, fmt.Errorf("%w: column %q (%s)", ErrColumnNotWritable, c.Name, c.FieldType)
	}
}
