package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// CREATE INDEX and DROP INDEX -- the third and fourth schema operations Phase 8
// lists. Like CREATE TABLE (ddl_create.go), CREATE INDEX rewrites the
// compressed column-definition internal file, which is why it waited for
// internal/zlib1.Compress the same way CREATE TABLE did.
//
// What the two operations write is decoded in
// docs/... (the index-definition-format analysis this file was built from) and
// summarised here:
//
//   - the column-definition stream continues past the column array with
//     int32 indexCount, indexCount index records, then a 12-byte trailer:
//     a still-unidentified int32 (constant 0 in every clean example), and the
//     two page numbers systemIndexRoots reads (record-page index root,
//     BLOB-page index root);
//   - a single-covered-column index record is a Pascal name, a database-wide
//     object id, 3 reserved bytes, coveredColumnCount (only 1 is proven),
//     the index's own B-tree root page number, a Pascal covered-column name,
//     2 reserved bytes and a constant terminator (0x00000014);
//   - CREATE INDEX allocates the index's root page before serializing the
//     stream, because the stream embeds that page's number;
//   - CREATE INDEX hands out one object id, the next free one, and advances
//     LastObjectID by it;
//   - only a table with no constraint records between its index array and the
//     trailer is understood well enough to edit -- see parseSchemaTail and
//     ErrSchemaTailNotUnderstood.
//
// Only a single-covered-column, int32-keyed index is supported: it is the only
// shape any fixture in the corpus exercises, and it is also the only leaf
// entry format PLAN.md's engine measurement covers ([null flag byte] + int32
// LE key, then PageNo int32 + ItemNo uint16).
var (
	// ErrIndexExists reports a CREATE INDEX naming an index that already exists
	// on the table.
	ErrIndexExists = errors.New("absdb: index already exists")

	// ErrNoSuchIndex reports a DROP INDEX naming an index the table does not
	// have.
	ErrNoSuchIndex = errors.New("absdb: no such index")

	// ErrNoSuchColumn reports a CREATE INDEX naming a column the table does not
	// have.
	ErrNoSuchColumn = errors.New("absdb: no such column")

	// ErrMultiColumnIndex reports a CREATE INDEX over more than one column, or
	// an existing index record that already covers more than one. Corpus
	// evidence covers only coveredColumnCount == 1 (see the index-definition
	// analysis §2.2); a record claiming otherwise is refused rather than
	// guessed at.
	ErrMultiColumnIndex = errors.New("absdb: multi-column indexes are not supported")

	// ErrUnsupportedIndexColumn reports a CREATE INDEX over a column whose type
	// the leaf-entry format has no corpus evidence for. Every measured index in
	// the corpus covers an Int32/Integer column, and the engine's own leaf
	// entry layout (PLAN.md) is "[null flag byte] + int32 LE key", so that is
	// the only column type this package builds an index over.
	ErrUnsupportedIndexColumn = errors.New("absdb: index column type has no corpus evidence for CREATE INDEX")

	// ErrSchemaTailNotUnderstood reports a column-definition stream whose tail,
	// past the index-record array, is not exactly the 12-byte reserved/root/
	// blobroot trailer. Every fixture with a NOT NULL, PRIMARY KEY or UNIQUE
	// constraint carries constraint records in that region (see the
	// index-definition analysis §4), and neither their format nor their
	// position relative to a newly inserted index record is confirmed. Settling
	// it needs a table created with constraints added incrementally so the
	// diff isolates the constraint-record layout the way Writes.abs/
	// Writes-idx.abs isolated the index-record layout.
	ErrSchemaTailNotUnderstood = errors.New("absdb: schema stream tail is not understood")

	// ErrIndexTooManyRows reports a CREATE INDEX over a table with more rows
	// than fit on a single B-tree leaf page. Multi-page index trees are not
	// built by this package.
	ErrIndexTooManyRows = errors.New("absdb: table has too many rows for a single-page index")
)

const (
	// indexRecordFlagsSize is the width of the reserved field between an index
	// record's objectId and its coveredColumnCount. Only 0x000000 is observed
	// in the corpus (index-definition analysis §2.1).
	indexRecordFlagsSize = 3

	// indexRecordCoveredTrailerSize is the width of the reserved field between
	// a covered column's name and the record's terminator. Only 0x0000 is
	// observed.
	indexRecordCoveredTrailerSize = 2

	// indexRecordTerminator is the constant closing every observed index
	// record (index-definition analysis §2.1).
	indexRecordTerminator = 0x00000014

	// schemaTailTrailerSize is the width of what follows the index-record
	// array in a table with no constraints: a reserved int32 plus the two page
	// numbers systemIndexRootsSize covers.
	schemaTailTrailerSize = 4 + systemIndexRootsSize

	// indexKeySize is primaryKeySize spelled out for index records this file
	// builds: one null-flag byte plus an int32 LE key (index.go).
	indexKeySize = primaryKeySize
)

// indexRecord is one parsed entry of the schema stream's index-definition
// array (index-definition analysis §2). start and end are its byte range
// within the decompressed schema stream, so a caller can splice around it.
type indexRecord struct {
	name       string
	objectID   uint32
	rootPageNo int32
	column     string
	start, end int
}

// createIndexPlan holds what validating a CREATE INDEX request produces: the
// table, the column to key by, and the schema stream already split at the
// points a new record is spliced in at. Splitting CreateIndex into "plan" and
// "apply" keeps each half's branching simple enough for cyclop -- the plan is
// all refusals, the apply is all writes.
type createIndexPlan struct {
	table        *Table
	colIdx       int
	schemaPageNo int
	raw          []byte
	colsEnd      int
	indexCount   int32
	tailStart    int
}

// planCreateIndex validates a CREATE INDEX request and returns everything
// applyCreateIndex needs, without writing anything.
func (db *File) planCreateIndex(table, index, column string) (createIndexPlan, error) {
	t, err := db.Table(table)
	if err != nil {
		return createIndexPlan{}, err
	}

	schema, err := t.Schema()
	if err != nil {
		return createIndexPlan{}, err
	}

	colIdx, err := findColumnIndex(schema, column)
	if err != nil {
		return createIndexPlan{}, err
	}

	schemaPageNo, err := t.schemaPageNo()
	if err != nil {
		return createIndexPlan{}, err
	}

	raw, err := db.readSchemaStream(schemaPageNo)
	if err != nil {
		return createIndexPlan{}, err
	}

	// The tail's shape is checked before anything else about the request,
	// because it is the safety property that matters most: a table this
	// package cannot safely edit around must be refused before any other
	// validation gets a chance to look like a green light.
	colsEnd, indexCount, records, tailStart, err := parseSchemaTail(raw)
	if err != nil {
		return createIndexPlan{}, err
	}

	for _, r := range records {
		if strings.EqualFold(r.name, index) {
			return createIndexPlan{}, fmt.Errorf("%w: %q on %q", ErrIndexExists, index, t.Name())
		}
	}

	if col := schema.Columns[colIdx]; col.BaseType != BftInt32 || col.FieldType != FieldInteger {
		return createIndexPlan{}, fmt.Errorf("%w: %q is base type %d / field type %s",
			ErrUnsupportedIndexColumn, col.Name, col.BaseType, col.FieldType)
	}

	return createIndexPlan{
		table:        t,
		colIdx:       colIdx,
		schemaPageNo: schemaPageNo,
		raw:          raw,
		colsEnd:      colsEnd,
		indexCount:   indexCount,
		tailStart:    tailStart,
	}, nil
}

// CreateIndex adds a single-column index to a table.
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when: the table does not exist (ErrNoSuchTable),
// the index name is already used (ErrIndexExists), the column does not exist
// (ErrNoSuchColumn), the column is not an Int32/Integer column
// (ErrUnsupportedIndexColumn), the table's schema stream carries constraint
// records this package cannot place a new index record around
// (ErrSchemaTailNotUnderstood), or the table has more rows than fit on one
// index leaf page (ErrIndexTooManyRows).
func (db *File) CreateIndex(table, index, column string) error {
	if !db.writable {
		return ErrReadOnly
	}

	plan, err := db.planCreateIndex(table, index, column)
	if err != nil {
		return err
	}

	entries, err := db.buildIndexLeafEntries(plan.table, plan.colIdx)
	if err != nil {
		return err
	}

	w := newPageEdit(db)

	indexPages, err := db.allocatePages(w, 1, PageTypeIndex, -1)
	if err != nil {
		return err
	}

	rootPageNo := indexPages[0]

	if err := db.writeIndexLeaf(w, rootPageNo, entries); err != nil {
		return err
	}

	objectID := int(db.lastObjectID) + 1

	record, err := serializeIndexRecord(index, uint32(objectID), int32(rootPageNo), column) //nolint:gosec // small object/page ids
	if err != nil {
		return err
	}

	newRaw := spliceIndexRecord(plan.raw, plan.colsEnd, plan.indexCount+1,
		plan.raw[plan.colsEnd+4:plan.tailStart], record, plan.raw[plan.tailStart:])

	if err := db.writeSchemaStream(w, plan.schemaPageNo, newRaw); err != nil {
		return err
	}

	if err := db.flushPages(w.order, w.pages); err != nil {
		return err
	}

	if err := db.setLastObjectID(int32(objectID)); err != nil { //nolint:gosec // small object ids
		return err
	}

	if err := db.bumpFileState(); err != nil {
		return err
	}

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing CREATE INDEX %q on %q: %w", index, table, err)
	}

	return nil
}

// writeSchemaStream compresses a rebuilt column-definition stream and writes
// it back to its page chain -- the shared tail of CreateIndex and DropIndex.
func (db *File) writeSchemaStream(w *pageEdit, schemaPageNo int, raw []byte) error {
	compressed, err := compressInternalFile(raw, 1)
	if err != nil {
		return err
	}

	return db.writeInternalFilePages(w, schemaPageNo, compressed)
}

// DropIndex removes an index from a table, freeing its B-tree pages.
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when the table does not exist (ErrNoSuchTable),
// the index does not exist (ErrNoSuchIndex), or the table's schema stream
// carries constraint records this package cannot safely edit around
// (ErrSchemaTailNotUnderstood).
func (db *File) DropIndex(table, index string) error {
	if !db.writable {
		return ErrReadOnly
	}

	t, err := db.Table(table)
	if err != nil {
		return err
	}

	schemaPageNo, err := t.schemaPageNo()
	if err != nil {
		return err
	}

	raw, err := db.readSchemaStream(schemaPageNo)
	if err != nil {
		return err
	}

	colsEnd, indexCount, records, tailStart, err := parseSchemaTail(raw)
	if err != nil {
		return err
	}

	rec, err := findIndexRecord(records, index, t.Name())
	if err != nil {
		return err
	}

	w := newPageEdit(db)

	treePages, err := db.indexTreePages(int(rec.rootPageNo))
	if err != nil {
		return err
	}

	if err := db.freeChainPages(w, treePages); err != nil {
		return err
	}

	newRaw := spliceIndexRecord(raw, colsEnd, indexCount-1, raw[colsEnd+4:rec.start], nil, raw[rec.end:tailStart], raw[tailStart:])

	if err := db.writeSchemaStream(w, schemaPageNo, newRaw); err != nil {
		return err
	}

	if err := db.flushPages(w.order, w.pages); err != nil {
		return err
	}

	if err := db.bumpFileState(); err != nil {
		return err
	}

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing DROP INDEX %q on %q: %w", index, table, err)
	}

	return nil
}

// findColumnIndex resolves a column by name, case-insensitively like every
// other name lookup in this package.
func findColumnIndex(schema *TableSchema, name string) (int, error) {
	for i, c := range schema.Columns {
		if strings.EqualFold(c.Name, name) {
			return i, nil
		}
	}

	return 0, fmt.Errorf("%w: %q", ErrNoSuchColumn, name)
}

// findIndexRecord resolves an index by name, case-insensitively.
func findIndexRecord(records []indexRecord, name, table string) (indexRecord, error) {
	for _, r := range records {
		if strings.EqualFold(r.name, name) {
			return r, nil
		}
	}

	return indexRecord{}, fmt.Errorf("%w: %q on %q", ErrNoSuchIndex, name, table)
}

// readSchemaStream reads and decompresses a table's column-definition
// internal file. Like Schema and systemIndexRoots, it reads a single page: no
// fixture's schema stream spans a chain.
func (db *File) readSchemaStream(schemaPageNo int) ([]byte, error) {
	page, err := db.ReadPage(schemaPageNo)
	if err != nil {
		return nil, err
	}

	data := page.PageData()
	if len(data) < internalFileHeaderSize {
		return nil, ErrBadSchema
	}

	return decompressInternalFile(data)
}

// parseSchemaTail parses everything the column array is followed by: indexCount,
// its index records, and requires exactly schemaTailTrailerSize bytes to remain
// after the last one. That requirement is the safety property described at
// ErrSchemaTailNotUnderstood: every fixture with a NOT NULL/PRIMARY KEY/UNIQUE
// constraint fails it, because their schema streams carry constraint records
// in this region that this package does not understand (index-definition
// analysis §4). Refusing here is what stops CREATE/DROP INDEX from corrupting
// a real customer database.
func parseSchemaTail(data []byte) (colsEnd int, indexCount int32, records []indexRecord, tailStart int, err error) {
	colsEnd, err = schemaColumnsEnd(data)
	if err != nil {
		return 0, 0, nil, 0, err
	}

	if colsEnd+4 > len(data) {
		return 0, 0, nil, 0, fmt.Errorf("%w: no room for indexCount after %d bytes of columns", ErrBadSchema, colsEnd)
	}

	rawCount := int32(binary.LittleEndian.Uint32(data[colsEnd : colsEnd+4]))
	if rawCount < 0 {
		return 0, 0, nil, 0, fmt.Errorf("%w: negative indexCount %d", ErrBadSchema, rawCount)
	}

	pos := colsEnd + 4

	records, pos, err = parseIndexRecords(data, pos, int(rawCount))
	if err != nil {
		return 0, 0, nil, 0, fmt.Errorf("%w: index record array: %w", ErrSchemaTailNotUnderstood, err)
	}

	if remaining := len(data) - pos; remaining != schemaTailTrailerSize {
		return 0, 0, nil, 0, fmt.Errorf(
			"%w: %d bytes remain after %d index record(s), want the %d-byte reserved/root/blobroot trailer",
			ErrSchemaTailNotUnderstood, remaining, rawCount, schemaTailTrailerSize,
		)
	}

	return colsEnd, rawCount, records, pos, nil
}

// schemaColumnsEnd walks the column-definition array with the same loop
// parseSchema (schema.go) uses, and returns the position immediately past it
// -- the start of the index-definition array parseSchemaTail then reads.
func schemaColumnsEnd(data []byte) (int, error) {
	if len(data) < 4 {
		return 0, fmt.Errorf("%w: schema stream is %d bytes", ErrBadSchema, len(data))
	}

	columnCount := int64(binary.LittleEndian.Uint32(data[0:4]))
	if columnCount > maxSchemaColumns || columnCount > int64(len(data)/minColumnDefSize) {
		return 0, fmt.Errorf("%w: invalid column count %d", ErrBadSchema, columnCount)
	}

	pos := 4

	for i := range int(columnCount) {
		_, next, err := parseColumnDef(data, pos, i)
		if err != nil {
			return 0, fmt.Errorf("%w: column %d: %w", ErrBadSchema, i, err)
		}

		pos = next
	}

	return pos, nil
}

// parseIndexRecords parses count consecutive index records starting at pos.
func parseIndexRecords(data []byte, pos, count int) ([]indexRecord, int, error) {
	records := make([]indexRecord, 0, count)

	for i := range count {
		rec, next, err := parseIndexRecord(data, pos)
		if err != nil {
			return nil, 0, fmt.Errorf("record %d: %w", i, err)
		}

		rec.start, rec.end = pos, next
		records = append(records, rec)
		pos = next
	}

	return records, pos, nil
}

// parseIndexRecord parses one index record, the layout confirmed by the
// index-definition analysis §2 for a single covered column. Anything claiming
// more than one covered column is refused with ErrMultiColumnIndex: no fixture
// in the corpus proves what that shape looks like.
func parseIndexRecord(data []byte, pos int) (indexRecord, int, error) {
	name, pos, err := readPascalString(data, pos)
	if err != nil {
		return indexRecord{}, 0, fmt.Errorf("name: %w", err)
	}

	objectID, pos, err := readUint32(data, pos, "objectId")
	if err != nil {
		return indexRecord{}, 0, err
	}

	pos, err = skipBytes(data, pos, indexRecordFlagsSize, "flags")
	if err != nil {
		return indexRecord{}, 0, err
	}

	coveredCount, pos, err := readUint32(data, pos, "coveredColumnCount")
	if err != nil {
		return indexRecord{}, 0, err
	}

	if coveredCount != 1 {
		return indexRecord{}, 0, fmt.Errorf("%w: %d covered columns", ErrMultiColumnIndex, coveredCount)
	}

	rootPage, pos, err := readUint32(data, pos, "root page")
	if err != nil {
		return indexRecord{}, 0, err
	}

	column, pos, err := readPascalString(data, pos)
	if err != nil {
		return indexRecord{}, 0, fmt.Errorf("covered column name: %w", err)
	}

	pos, err = skipBytes(data, pos, indexRecordCoveredTrailerSize, "covered column trailer")
	if err != nil {
		return indexRecord{}, 0, err
	}

	terminator, pos, err := readUint32(data, pos, "terminator")
	if err != nil {
		return indexRecord{}, 0, err
	}

	if terminator != indexRecordTerminator {
		return indexRecord{}, 0, fmt.Errorf("terminator = %#x, want %#x", terminator, indexRecordTerminator)
	}

	return indexRecord{
		name:       name,
		objectID:   objectID,
		rootPageNo: int32(rootPage),
		column:     column,
	}, pos, nil
}

// readPascalString reads a one-byte-length-prefixed, Windows-1252-encoded
// string, the same shape parseColumnDef reads column names with.
func readPascalString(data []byte, pos int) (string, int, error) {
	if pos >= len(data) {
		return "", 0, errors.New("truncated length")
	}

	n := int(data[pos])
	pos++

	if pos+n > len(data) {
		return "", 0, errors.New("string extends beyond data")
	}

	return decodeANSI(data[pos : pos+n]), pos + n, nil
}

// readUint32 reads a little-endian uint32, naming the field in its error.
func readUint32(data []byte, pos int, field string) (uint32, int, error) {
	if pos+4 > len(data) {
		return 0, 0, fmt.Errorf("truncated %s", field)
	}

	return binary.LittleEndian.Uint32(data[pos : pos+4]), pos + 4, nil
}

// skipBytes advances pos by n, naming the field in its error if there is not
// enough data.
func skipBytes(data []byte, pos, n int, field string) (int, error) {
	if pos+n > len(data) {
		return 0, fmt.Errorf("truncated %s", field)
	}

	return pos + n, nil
}

// serializeIndexRecord builds one index record, the mirror of parseIndexRecord:
// a Pascal name, the object id, 3 reserved zero bytes, coveredColumnCount=1,
// the index's own root page, a Pascal covered-column name, 2 reserved zero
// bytes and the constant terminator.
func serializeIndexRecord(name string, objectID uint32, rootPageNo int32, column string) ([]byte, error) {
	rawName, err := encodeIndexName(name)
	if err != nil {
		return nil, err
	}

	rawColumn, err := encodeIndexName(column)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 1+len(rawName)+4+indexRecordFlagsSize+4+4+1+len(rawColumn)+indexRecordCoveredTrailerSize+4)

	out = append(out, byte(len(rawName))) //nolint:gosec // checked in encodeIndexName
	out = append(out, rawName...)

	var buf4 [4]byte

	binary.LittleEndian.PutUint32(buf4[:], objectID)
	out = append(out, buf4[:]...)

	out = append(out, make([]byte, indexRecordFlagsSize)...)

	binary.LittleEndian.PutUint32(buf4[:], 1) // coveredColumnCount
	out = append(out, buf4[:]...)

	binary.LittleEndian.PutUint32(buf4[:], uint32(rootPageNo))
	out = append(out, buf4[:]...)

	out = append(out, byte(len(rawColumn))) //nolint:gosec // checked in encodeIndexName
	out = append(out, rawColumn...)

	out = append(out, make([]byte, indexRecordCoveredTrailerSize)...)

	binary.LittleEndian.PutUint32(buf4[:], indexRecordTerminator)
	out = append(out, buf4[:]...)

	return out, nil
}

// encodeIndexName Windows-1252 encodes a name for a Pascal string field, the
// same encoding column and table names use.
func encodeIndexName(name string) ([]byte, error) {
	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(name))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrStringEncoding, name, err)
	}

	if len(raw) == 0 || len(raw) > 255 {
		return nil, fmt.Errorf("%w: %d-byte name", ErrValueRange, len(raw))
	}

	return raw, nil
}

// spliceIndexRecord reassembles a schema stream from its pieces: the column
// array (data[:colsEnd]), a new indexCount, and the caller's chosen sequence of
// byte slices making up the rest -- the index-record array (with or without one
// record added or removed) followed by the trailer. It exists so CreateIndex
// and DropIndex build the new stream the same way, by concatenation rather than
// a general re-serializer, matching every byte this package does not
// understand (ddl.go's file comment: "edit the stream surgically").
func spliceIndexRecord(data []byte, colsEnd int, newCount int32, parts ...[]byte) []byte {
	size := colsEnd + 4
	for _, p := range parts {
		size += len(p)
	}

	out := make([]byte, 0, size)
	out = append(out, data[:colsEnd]...)

	var cnt4 [4]byte

	binary.LittleEndian.PutUint32(cnt4[:], uint32(newCount))
	out = append(out, cnt4[:]...)

	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

// buildIndexLeafEntries reads every row of a table and returns the leaf
// entries a single-page B-tree index over one Int32 column holds: sorted by
// key, [null flag byte]+int32 LE, then the row's data page and slot.
func (db *File) buildIndexLeafEntries(t *Table, colIdx int) ([]BTreeEntry, error) {
	r, err := t.Open()
	if err != nil {
		return nil, err
	}

	var entries []BTreeEntry

	for r.Next() {
		rec := r.Record()

		id, ok := r.RecordID()
		if !ok {
			return nil, fmt.Errorf("absdb: index build: %w", ErrBadLayout)
		}

		key := make([]byte, indexKeySize)
		if rec.IsNull(colIdx) {
			key[0] = 1
		} else {
			binary.LittleEndian.PutUint32(key[1:], uint32(rec.Int(colIdx)))
		}

		entries = append(entries, BTreeEntry{
			Key:    key,
			PageNo: int32(id.PageNo), //nolint:gosec // page numbers are small and positive
			ItemNo: uint16(id.Slot),  //nolint:gosec // slots are small and positive
		})
	}

	if err := r.Err(); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return compareInt32Keys(entries[i].Key, entries[j].Key) < 0
	})

	return entries, nil
}

// writeIndexLeaf writes a single B-tree root/leaf page holding entries, the
// shape writeIndexRoot (ddl_create.go) writes for an empty record-page index:
// IsRoot=true, IsLeaf=true, no siblings, HasKeys=true, HasSuffixes=false, keyed
// by indexKeySize, followed by one leafEntrySuffixSize-suffixed entry per row.
func (db *File) writeIndexLeaf(w *pageEdit, pageNo int, entries []BTreeEntry) error {
	buf, err := w.load(pageNo)
	if err != nil {
		return err
	}

	stride := indexKeySize + leafEntrySuffixSize
	if btreeHeaderSize+len(entries)*stride > len(buf.payload) {
		return fmt.Errorf("%w: %d rows need %d bytes, page holds %d",
			ErrIndexTooManyRows, len(entries), btreeHeaderSize+len(entries)*stride, len(buf.payload))
	}

	h := buf.payload[:btreeHeaderSize]

	clear(h)

	h[0] = 1                                                      // IsRoot
	h[1] = 1                                                      // IsLeaf
	binary.LittleEndian.PutUint32(h[2:6], noPageNo)               // LeftPageNo
	binary.LittleEndian.PutUint32(h[6:10], noPageNo)              // RightPageNo
	h[10] = 1                                                     // HasKeys
	h[11] = 0                                                     // HasSuffixes
	binary.LittleEndian.PutUint16(h[12:14], indexKeySize)         // KeyPrefixSize
	binary.LittleEndian.PutUint16(h[14:16], uint16(len(entries))) //nolint:gosec // bounded by the capacity check above
	binary.LittleEndian.PutUint16(h[16:18], 0)                    // PagePrefixSize

	for i, e := range entries {
		off := btreeHeaderSize + i*stride
		copy(buf.payload[off:off+indexKeySize], e.Key)
		binary.LittleEndian.PutUint32(buf.payload[off+indexKeySize:off+indexKeySize+4], uint32(e.PageNo))
		binary.LittleEndian.PutUint16(buf.payload[off+indexKeySize+4:off+indexKeySize+6], e.ItemNo)
	}

	buf.dirty = true

	return nil
}
