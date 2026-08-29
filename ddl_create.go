package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// CREATE TABLE -- the second schema operation Stage 3 of Phase 8 unblocked.
// DROP TABLE (ddl.go) never touches a compressed stream; CREATE TABLE writes
// one (the column definitions), which is why it waited for
// internal/zlib1.Compress.
//
// What CREATE TABLE Delta (X, Y) writes, measured against MultiTable.abs (see
// PLAN.md Phase 8 and the analysis this file was built from):
//
//   - allocates five pages and no data page -- a data page arrives with the
//     first insert: two chained type-7 pages for a 6000-byte all-zero
//     "system" internal file (systemPageNo in the catalog; its role is still
//     unidentified), one type-8 page for the column definitions, one type-9
//     page for the counters, one type-12 page for the empty record-page index
//     root;
//   - appends a 272-byte entry to the table catalog;
//   - writes the column-definition internal file: columnCount, one
//     definition per column, an empty index-definition array, and the
//     8-byte trailer systemIndexRoots reads;
//   - writes the 28-byte counters file tableInfoOffsets already knows the
//     layout of;
//   - moves LastUsedPageNo, LastObjectID and the State counters the pages and
//     the header carry.
//
// Object ids are handed out one per table and one per column: PLAN.md records
// Delta's X and Y taking 13 and 14 after Delta itself took 12, which is what
// moves LastObjectID from 11 to 14 -- one more than the column count, because
// the table itself takes one too.
//
// Pages are allocated lowest free page number first: PLAN.md records a table
// created after a drop taking the freed pages before any higher one.
//
// A newly allocated page's ABSP State is seeded by the engine with an
// unreproducible random value (see newPageState in ddl.go), so CREATE TABLE
// cannot be byte-identical to the engine's own output the way DROP TABLE is.
// TestCreateTableMatchesEngineByteForByte asserts every byte outside the five
// new pages' State words instead, and says why in its own comment.
var (
	// ErrTableExists reports a CREATE TABLE naming a table already present in
	// the catalog.
	ErrTableExists = errors.New("absdb: table already exists")

	// ErrUnsupportedColumnType reports a column type the serializer has no
	// corpus evidence for. Only the two combinations CREATE TABLE Delta (X, Y)
	// exercises -- Int32/Integer and Varchar/String -- are known; guessing at
	// the padding or terminator byte for any other type risks writing a file
	// the engine cannot read back. See docs/... FINDING 2: the stream this
	// serializer writes also carries an index-definition array this package
	// does not build (a fresh table's is empty), so it must be edited
	// surgically rather than through a general re-serializer.
	ErrUnsupportedColumnType = errors.New("absdb: column type has no corpus evidence for CREATE TABLE")
)

const (
	// systemInternalFileSize is the size of the all-zero internal file a fresh
	// table's systemPageNo names. Its role is unidentified (see
	// TableInfo.systemPageNo); its size and contents -- algorithm 0, 6000
	// zero bytes -- are what CREATE TABLE Delta (X, Y) measured.
	systemInternalFileSize = 6000

	// tableInfoCounterFields is the width, in bytes, of one column's slot in
	// the table info file between columnCount and the trailing two counters.
	// tableInfoOffsets (writer.go) already reads this layout; CreateTable is
	// the first place that writes it.
	tableInfoCounterFields = 8

	// columnDefPaddingZeros and columnDefPaddingFF are the fixed padding a
	// non-BLOB column definition carries between its flags byte and its
	// 4-byte terminator, measured identically on both of Delta's columns.
	columnDefPaddingZeros = 23
	columnDefPaddingFF    = 7

	// columnDefTerminatorSize is the 0x7F 0x00 <baseType> 0xFF sequence
	// findColumnTerminator (schema.go) scans for.
	columnDefTerminatorSize = 4

	// schemaTrailerSize is the 16 bytes following the column definitions:
	// int32 indexCount (0 for a fresh table), a still-unidentified int32
	// (FINDING 2; 0 alongside indexCount 0), and the 8-byte pair
	// systemIndexRoots reads.
	schemaTrailerSize = 16
)

// knownColumnTypes is the set of (BaseType, FieldType) combinations CREATE
// TABLE has corpus evidence for.
var knownColumnTypes = map[BaseFieldType]FieldType{
	BftInt32:   FieldInteger,
	BftVarchar: FieldString,
}

// CreateTable adds a new, empty table to the database: no rows, no index and
// no BLOB column. Columns are given in name/type order; any ID or Position the
// caller sets on them is ignored, because the engine assigns both itself (see
// the file comment).
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when the table already exists, when the catalog
// cannot be grown in place, when the file does not have five free pages, or
// when a column's type has no corpus evidence backing its on-disk encoding
// (ErrUnsupportedColumnType).
func (db *File) CreateTable(name string, columns []Column) error {
	if !db.writable {
		return ErrReadOnly
	}

	if err := db.checkTableNameFree(name); err != nil {
		return err
	}

	if len(columns) == 0 {
		return fmt.Errorf("%w: no columns", ErrBadSchema)
	}

	// Object ids: the table takes the next one, then each column takes the
	// next after that, in order.
	tableID := int(db.lastObjectID) + 1
	assigned := assignColumnIDs(tableID, columns)

	colDefs, err := serializeColumnDefs(assigned)
	if err != nil {
		return err
	}

	w := newPageEdit(db)

	catalogBuf, err := db.openCatalogForAppend(w, name)
	if err != nil {
		return err
	}

	pages, err := db.allocateTablePages(w)
	if err != nil {
		return err
	}

	if err := db.writeTableInternalFiles(w, pages, colDefs, len(assigned)); err != nil {
		return err
	}

	if err := appendCatalogEntry(catalogBuf.payload, TableInfo{
		Name:         name,
		ID:           tableID,
		SchemaPageNo: pages.schema,
		InfoPageNo:   pages.info,
		systemPageNo: pages.system[0],
	}); err != nil {
		return err
	}

	catalogBuf.dirty = true

	if err := db.flushPages(w.order, w.pages); err != nil {
		return err
	}

	if err := db.setLastObjectID(int32(tableID + len(assigned))); err != nil { //nolint:gosec // small object ids
		return err
	}

	if err := db.bumpFileState(); err != nil {
		return err
	}

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing CREATE TABLE %q: %w", name, err)
	}

	return nil
}

// checkTableNameFree refuses a CREATE TABLE whose name is already in the
// catalog, case-insensitively -- the same matching Table and DropTable use.
func (db *File) checkTableNameFree(name string) error {
	tables, err := db.Tables()
	if err != nil {
		return err
	}

	for _, t := range tables {
		if strings.EqualFold(t.Name, name) {
			return fmt.Errorf("%w: %q", ErrTableExists, name)
		}
	}

	return nil
}

// assignColumnIDs returns columns with their ID and Position set the way the
// engine assigns them: the table itself takes tableID, so its columns start
// at tableID+1 and count up in order. Any ID or Position the caller set is
// overwritten.
func assignColumnIDs(tableID int, columns []Column) []Column {
	assigned := make([]Column, len(columns))

	for i, c := range columns {
		c.ID = uint32(tableID + 1 + i) //nolint:gosec // small object ids
		c.Position = i
		assigned[i] = c
	}

	return assigned
}

// openCatalogForAppend buffers the table catalog page and dry-runs the append
// against a scratch copy of its payload, so a "no room" or "not writable"
// refusal happens before CreateTable allocates anything.
func (db *File) openCatalogForAppend(w *pageEdit, name string) (*pageWriteBuf, error) {
	catalogPageNo, err := db.findPageByType(PageTypeTableList)
	if err != nil {
		return nil, err
	}

	if catalogPageNo < 0 {
		return nil, ErrNoCatalog
	}

	catalogBuf, err := w.load(catalogPageNo)
	if err != nil {
		return nil, err
	}

	scratch := append([]byte(nil), catalogBuf.payload...)
	if err := appendCatalogEntry(scratch, TableInfo{Name: name}); err != nil {
		return nil, err
	}

	return catalogBuf, nil
}

// newTablePages names the five pages CreateTable allocates for a fresh table:
// two chained pages for the system internal file, and one each for the
// column definitions, the counters and the record-page index root.
type newTablePages struct {
	system [2]int
	schema int
	info   int
	index  int
}

// allocateTablePages reserves and chains CreateTable's five pages, in the
// order the engine itself allocates them (see the file comment): the system
// internal file's two pages first, then schema, then info, then index.
func (db *File) allocateTablePages(w *pageEdit) (newTablePages, error) {
	var pages newTablePages

	systemPages, err := db.allocatePages(w, 2, PageTypeSystem, -1)
	if err != nil {
		return pages, err
	}

	pages.system[0], pages.system[1] = systemPages[0], systemPages[1]

	if err := db.linkChain(w, pages.system[0], pages.system[1]); err != nil {
		return pages, err
	}

	schemaPages, err := db.allocatePages(w, 1, PageTypeSchema, -1)
	if err != nil {
		return pages, err
	}

	pages.schema = schemaPages[0]

	infoPages, err := db.allocatePages(w, 1, PageTypeTableInfo, -1)
	if err != nil {
		return pages, err
	}

	pages.info = infoPages[0]

	indexPages, err := db.allocatePages(w, 1, PageTypeIndex, -1)
	if err != nil {
		return pages, err
	}

	pages.index = indexPages[0]

	return pages, nil
}

// writeTableInternalFiles writes the content of all five of CreateTable's new
// pages: the all-zero system file, the empty B-tree index root, the
// column-definition file, and the counters file.
func (db *File) writeTableInternalFiles(w *pageEdit, pages newTablePages, colDefs []byte, columnCount int) error {
	if err := db.writeInternalFilePages(w, pages.system[0], buildSystemInternalFile()); err != nil {
		return err
	}

	if err := db.writeIndexRoot(w, pages.index); err != nil {
		return err
	}

	schemaFile, err := compressInternalFile(buildSchemaFile(colDefs, pages.index), 1)
	if err != nil {
		return err
	}

	if err := db.writeInternalFilePages(w, pages.schema, schemaFile); err != nil {
		return err
	}

	infoFile, err := compressInternalFile(buildTableInfoFile(columnCount), 0)
	if err != nil {
		return err
	}

	return db.writeInternalFilePages(w, pages.info, infoFile)
}

// writeIndexRoot writes an empty B-tree root/leaf page: the shape CREATE
// TABLE Delta's new record-page index takes before any data page exists to
// index. Its 18-byte TABSBTreePageHeader is IsRoot=true, IsLeaf=true, no
// siblings, HasKeys=true, HasSuffixes=false, a systemKeySize key and no
// entries -- measured byte for byte on page 28 of MultiTable-create.abs.
func (db *File) writeIndexRoot(w *pageEdit, pageNo int) error {
	buf, err := w.load(pageNo)
	if err != nil {
		return err
	}

	h := buf.payload[:btreeHeaderSize]

	clear(h)

	h[0] = 1                                               // IsRoot
	h[1] = 1                                               // IsLeaf
	binary.LittleEndian.PutUint32(h[2:6], noPageNo)        // LeftPageNo
	binary.LittleEndian.PutUint32(h[6:10], noPageNo)       // RightPageNo
	h[10] = 1                                              // HasKeys
	h[11] = 0                                              // HasSuffixes
	binary.LittleEndian.PutUint16(h[12:14], systemKeySize) // KeyPrefixSize
	binary.LittleEndian.PutUint16(h[14:16], 0)             // EntryCount
	binary.LittleEndian.PutUint16(h[16:18], 0)             // PagePrefixSize

	buf.dirty = true

	return nil
}

// buildSystemInternalFile builds the all-zero internal file a fresh table's
// systemPageNo names: algorithm 0 (no compression), a compressed-size field
// of systemInternalFileSize, and -- measured on page 24 of
// MultiTable-create.abs, whose payload carries no non-zero byte past the
// compressed-size field -- a decompressedSize field left at 0 rather than
// mirroring it. decompressInternalFile does not read that field for
// algorithm 0, so the engine evidently does not populate it here the way it
// does for the catalog's own internal file.
func buildSystemInternalFile() []byte {
	payload := make([]byte, systemInternalFileSize)

	out := make([]byte, internalFileHeaderSize+len(payload))
	out[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(out[1:5], uint32(len(payload))) //nolint:gosec // bounded by systemInternalFileSize
	// out[5:9] (decompressedSize) and out[9] (algorithm 0) are left at zero.
	copy(out[internalFileHeaderSize:], payload)

	return out
}

// buildTableInfoFile builds the 28-byte counters file tableInfoOffsets
// (writer.go) reads: int32 columnCount, columnCount*8 zero bytes (one slot
// per column, still unwritten by CreateTable), int32 changes, int32 records.
// A fresh table starts with no rows and no recorded changes.
func buildTableInfoFile(columnCount int) []byte {
	out := make([]byte, 4+columnCount*tableInfoCounterFields+tableInfoTrailerSize)
	binary.LittleEndian.PutUint32(out[0:4], uint32(columnCount)) //nolint:gosec // bounded by maxSchemaColumns via serializeColumnDefs

	return out
}

// buildSchemaFile assembles the column-definition internal file: columnCount,
// the column definitions themselves, an empty index-definition array (a
// freshly created table has none), and the trailer systemIndexRoots reads.
//
// FINDING 2 of the CREATE TABLE analysis: the stream continues past the
// columns with int32 indexCount, indexCount index records, one further
// unidentified int32, then the two trailing page numbers. Only indexCount=0
// is understood well enough to write, which is what a fresh table needs.
func buildSchemaFile(colDefs []byte, recordIndexRoot int) []byte {
	// colDefs already opens with its own columnCount int32, written by
	// serializeColumnDefs, so it is copied in as-is.
	out := make([]byte, len(colDefs)+schemaTrailerSize)
	copy(out, colDefs)

	tail := out[len(colDefs):]
	// tail[0:4] indexCount = 0, tail[4:8] the unidentified field = 0: both
	// already zero from make().
	binary.LittleEndian.PutUint32(tail[8:12], uint32(int32(recordIndexRoot))) //nolint:gosec // page number
	binary.LittleEndian.PutUint32(tail[12:16], noPageNo)                      // blobIndexRoot: no BLOB column supported yet

	return out
}

// serializeColumnDefs builds the columnCount-prefixed run of column
// definitions that opens the schema internal file.
func serializeColumnDefs(columns []Column) ([]byte, error) {
	if len(columns) > maxSchemaColumns {
		return nil, fmt.Errorf("%w: %d columns", ErrBadSchema, len(columns))
	}

	var head [4]byte

	binary.LittleEndian.PutUint32(head[:], uint32(len(columns))) //nolint:gosec // bounded above

	buf := make([]byte, 0, 4)
	buf = append(buf, head[:]...)

	for _, col := range columns {
		def, err := serializeColumnDef(col)
		if err != nil {
			return nil, err
		}

		buf = append(buf, def...)
	}

	return buf, nil
}

// serializeColumnDef builds one column definition, the mirror of
// parseColumnDef (schema.go): a Pascal name, the column's id, its two type
// bytes, its size, a flags byte, fixed padding, and the 4-byte terminator
// findColumnTerminator scans for.
//
// Only the two (BaseType, FieldType) combinations knownColumnTypes lists are
// supported -- everything else is refused with ErrUnsupportedColumnType
// rather than guessed at, because the padding width and the terminator's
// middle byte are only pinned by corpus evidence for those two.
func serializeColumnDef(col Column) ([]byte, error) {
	want, ok := knownColumnTypes[col.BaseType]
	if !ok || want != col.FieldType || col.IsBLOB() {
		return nil, fmt.Errorf("%w: base type %d / field type %s", ErrUnsupportedColumnType, col.BaseType, col.FieldType)
	}

	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(col.Name))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrStringEncoding, col.Name, err)
	}

	if len(raw) == 0 || len(raw) > 255 {
		return nil, fmt.Errorf("%w: %d-byte column name", ErrValueRange, len(raw))
	}

	out := make([]byte, 0, 1+len(raw)+4+1+1+4+1+columnDefPaddingZeros+columnDefPaddingFF+columnDefTerminatorSize)

	out = append(out, byte(len(raw))) //nolint:gosec // checked above: len(raw) <= 255
	out = append(out, raw...)

	var idBuf, sizeBuf [4]byte

	binary.LittleEndian.PutUint32(idBuf[:], col.ID)
	out = append(out, idBuf[:]...)

	out = append(out, byte(col.BaseType), byte(col.FieldType))

	binary.LittleEndian.PutUint32(sizeBuf[:], col.Size)
	out = append(out, sizeBuf[:]...)

	// The flags byte: both of Delta's columns carry 1, and Column has no
	// field yet to say a different value is ever wanted.
	out = append(out, 1)

	out = append(out, make([]byte, columnDefPaddingZeros)...)

	for range columnDefPaddingFF {
		out = append(out, 0xFF)
	}

	out = append(out, 0x7F, 0x00, byte(col.BaseType), 0xFF)

	return out, nil
}
