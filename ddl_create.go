package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// CREATE TABLE -- the second schema operation Stage 3 of Phase 8 unblocked.
// DROP TABLE (ddl.go) never touches a compressed stream; CREATE TABLE writes
// one (the column definitions), which is why it waited for
// internal/zlib1.Compress.
//
// What CREATE TABLE Delta (X, Y) writes, measured against MultiTable.abs (see
// docs/writing.md and the analysis this file was built from):
//
//   - allocates five pages and no data page -- a data page arrives with the
//     first insert: two chained type-7 pages for a 6000-byte all-zero
//     "system" internal file (systemPageNo in the catalog; its role is still
//     unidentified), one type-8 page for the column definitions, one type-9
//     page for the counters, one type-12 page for the empty record-page index
//     root. Five is the count at a 4096-byte page size only: the system file
//     spans however many pages its bytes need, which is three (and so six
//     pages in all) at 2048 -- see systemFilePageCount;
//   - appends a 272-byte entry to the table catalog;
//   - writes the column-definition internal file: columnCount, one
//     definition per column, an empty index-definition array, and the
//     8-byte trailer systemIndexRoots reads;
//   - writes the 28-byte counters file tableInfoOffsets already knows the
//     layout of;
//   - moves LastUsedPageNo, LastObjectID and the State counters the pages and
//     the header carry.
//
// Object ids are handed out one per table and one per column: docs/format/pages.md records
// Delta's X and Y taking 13 and 14 after Delta itself took 12, which is what
// moves LastObjectID from 11 to 14 -- one more than the column count, because
// the table itself takes one too.
//
// Pages are allocated lowest free page number first: docs/format/pages.md records a table
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

	// ErrColumnDefault reports a column whose definition carries a DEFAULT
	// clause. serializeColumnDef always writes the no-default marker, because
	// Column has no field for a default and CREATE TABLE has no syntax here to
	// declare one. Re-serializing a parsed column that has one would therefore
	// drop it silently -- the table would read back fine and would no longer
	// fill the column in on an insert that omits it -- so the column is
	// refused instead. testdata/Constraints.abs's CDefault is the fixture.
	ErrColumnDefault = errors.New("absdb: column carries a DEFAULT clause this package cannot write")

	// ErrColumnAutoIncOptions reports a column whose AUTOINC parameters are
	// not the engine's defaults. serializeColumnDef writes those defaults
	// unconditionally, so re-serializing such a column would silently reset a
	// real INCREMENT, INITIALVALUE, MINVALUE, MAXVALUE or CYCLED clause.
	// Types.abs's TAutoInc is the only table anywhere that carries any.
	ErrColumnAutoIncOptions = errors.New("absdb: column carries AUTOINC options this package cannot write")
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

	// columnDefDefaultSize is the typed value that closes a column
	// definition when there is no DEFAULT: the TABSVariant type tag and the
	// absent marker.
	columnDefDefaultSize = 2

	// schemaTrailerSize is the 8 bytes closing the schema stream: the
	// root/blobroot pair systemIndexRoots reads. What FINDING 2 of the CREATE
	// TABLE analysis recorded as a "still-unidentified int32" sitting between
	// indexCount and that pair is the constraint array's own count, which
	// parseSchemaTail decoded later; a fresh table has zero of both, which is
	// why one zero int32 was indistinguishable from the other.
	schemaTrailerSize = 8
)

// knownColumnTypes is the set of (BaseType, FieldType) combinations CREATE
// TABLE has corpus evidence for.
//
// Int32/AutoInc is the newest, and its evidence is Auto.abs: the column
// definition is the Int32/Integer one with the field-type byte changed, size 0
// and the same autoinc block and absent-DEFAULT marker every other column
// carries. TestCreateAutoIncTableMatchesEngineByteForByte rebuilds the whole of
// that file from Empty.abs, which is what pins it -- a serializer that had the
// shape wrong would not reproduce the compressed schema stream.
var knownColumnTypes = map[BaseFieldType][]FieldType{
	BftInt32:   {FieldInteger, FieldAutoInc},
	BftVarchar: {FieldString},
}

// knownColumnType reports whether serializeColumnDef has evidence for this
// column's on-disk encoding.
func knownColumnType(col Column) bool {
	if col.IsBLOB() {
		return false
	}

	return slices.Contains(knownColumnTypes[col.BaseType], col.FieldType)
}

// CreateTable adds a new, empty table to the database: no rows, no index, no
// constraint and no BLOB column. Columns are given in name/type order; any ID
// or Position the caller sets on them is ignored, because the engine assigns
// both itself (see the file comment).
//
// Constraints are not part of the exported signature because a caller has no
// way to describe one: Column's nullability is unexported, and a MINVALUE pair
// has no home on it at all. What does carry them is the internal createTable,
// which the compaction rebuild uses to hand a table's own records back to it.
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when the table already exists, when the catalog
// cannot be grown in place, or when a column's type has no corpus evidence
// backing its on-disk encoding (ErrUnsupportedColumnType). A file without
// enough free pages is no longer among those refusals: it grows by whole
// extents to make room, the way the engine does (ddl_grow.go).
func (db *File) CreateTable(name string, columns []Column) error {
	return db.createTable(name, columns, nil)
}

// createTable is CreateTable with the constraint records a rebuild carries
// over. They are written into the new table's schema stream rather than
// spliced in afterwards, so the table declares them from the moment it exists
// and the whole creation stays one transaction, the way the engine's own
// CREATE TABLE ... NOT NULL is.
//
// A PRIMARY KEY or UNIQUE record brings an index with it, and that index is
// built here: one more page, one more object id and one more record in the
// schema stream's index array. Constraints.abs pins a compound key's schema
// record and empty root exactly; later writes maintain it when every occupied
// component is the all-Int32 shape MultiKeys.abs measures. A key with another
// occupied component remains refused by writer index resolution.
func (db *File) createTable(name string, columns []Column, constraints []constraintRecord) error {
	if !db.writable {
		return ErrReadOnly
	}

	if err := db.checkTableNameFree(name); err != nil {
		return err
	}

	if len(columns) == 0 {
		return fmt.Errorf("%w: no columns", ErrBadSchema)
	}

	tableID := int(db.lastObjectID) + 1

	plan, err := planNewTable(name, tableID, columns, constraints)
	if err != nil {
		return err
	}

	w := newPageEdit(db)

	catalogBuf, err := db.openCatalogForAppend(w, name)
	if err != nil {
		return err
	}

	pages, err := db.allocateTablePages(w, len(plan.indexes))
	if err != nil {
		return err
	}

	files, err := plan.internalFiles(pages)
	if err != nil {
		return err
	}

	if err := db.writeTableInternalFiles(w, pages, files); err != nil {
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

	if err := db.setLastObjectID(int32(plan.lastObjectID)); err != nil { //nolint:gosec // small object ids
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

// newTablePlan is everything CreateTable works out before it touches the file:
// the columns with their object ids, the index records a key constraint brings
// with it, the constraint records themselves, and the serialized column array.
// Only the index array is left, because it embeds root pages that are not
// allocated yet.
type newTablePlan struct {
	columns      []Column
	indexes      []indexRecord
	constraints  []constraintRecord
	colDefs      []byte
	lastObjectID int
}

// planNewTable hands out the table's object ids and serializes what does not
// depend on a page number.
//
// The ids run table, columns, indexes, constraints -- the order
// Constraints.abs's run in (CPk is table 8, columns 9 and 10, index 11,
// constraint 12, and the next table is 13). Reproducing that order is what
// makes the replay in TestCreateTableWritesTheEngineSchemaStream land on the
// engine's own ids.
func planNewTable(
	name string, tableID int, columns []Column, constraints []constraintRecord,
) (newTablePlan, error) {
	assigned := assignColumnIDs(tableID, columns)

	colDefs, err := serializeColumnDefs(assigned)
	if err != nil {
		return newTablePlan{}, err
	}

	indexes, records, err := planTableConstraints(name, tableID+len(assigned)+1, assigned, constraints)
	if err != nil {
		return newTablePlan{}, err
	}

	return newTablePlan{
		columns:      assigned,
		indexes:      indexes,
		constraints:  records,
		colDefs:      colDefs,
		lastObjectID: tableID + len(assigned) + len(indexes) + len(records),
	}, nil
}

// internalFiles completes the plan once the pages are allocated, which is when
// each index's root page number is finally known.
func (p newTablePlan) internalFiles(pages newTablePages) (tableInternalFiles, error) {
	indexArray, err := serializeIndexArray(rootIndexRecords(p.indexes, pages.keys))
	if err != nil {
		return tableInternalFiles{}, err
	}

	keySizes := make([]int, len(p.indexes))
	for i, rec := range p.indexes {
		keySizes[i], err = indexRecordKeySize(rec, p.columns)
		if err != nil {
			return tableInternalFiles{}, err
		}
	}

	constraintArray, err := serializeConstraintArray(p.constraints)
	if err != nil {
		return tableInternalFiles{}, err
	}

	return tableInternalFiles{
		colDefs:     p.colDefs,
		indexes:     indexArray,
		constraints: constraintArray,
		columnCount: len(p.columns),
		keySizes:    keySizes,
	}, nil
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

// planTableConstraints stamps a rebuild's constraint records with the object
// ids and owners they take in their new database, and returns the indexes the
// key ones need built alongside them.
//
// The ids run table, columns, indexes, constraints, so firstID -- the id after
// the last column's -- goes to the first index if there is one and to the first
// constraint otherwise. A key record's ownerObjectID names its index; a NOT
// NULL or CHECK record's names the column it covers.
//
// Everything else is carried over unchanged, each record's own name and the
// index name it quotes included. Regenerating a name from a
// "$C_NotNull$<table>$<column>" template would reproduce every name in the
// corpus and would still be a guess about a name the engine chose; copying it
// cannot be wrong.
//
// A record this function cannot place is refused rather than dropped: one
// naming another table, one covering no column or several, one naming a column
// the new table does not have, and a key whose column is not the Int32 shape
// an index leaf is built for.
func planTableConstraints(
	table string, firstID int, columns []Column, constraints []constraintRecord,
) ([]indexRecord, []constraintRecord, error) {
	owners := make([][]Column, 0, len(constraints))

	for _, rec := range constraints {
		owner, err := constraintOwnerColumns(table, rec, columns)
		if err != nil {
			return nil, nil, err
		}

		owners = append(owners, owner)
	}

	indexes := make([]indexRecord, 0, len(constraints))

	for i, rec := range constraints {
		if rec.kind != constraintPrimaryKey && rec.kind != constraintUnique {
			continue
		}

		idx, err := keyConstraintIndex(rec, owners[i], firstID+len(indexes))
		if err != nil {
			return nil, nil, err
		}

		indexes = append(indexes, idx)
	}

	records := make([]constraintRecord, 0, len(constraints))
	nextIndex := 0

	for i, rec := range constraints {
		rec.objectID = uint32(firstID + len(indexes) + len(records)) //nolint:gosec // small object ids
		rec.ownerID = owners[i][0].ID
		rec.start, rec.end = 0, 0

		for j := range rec.columns {
			rec.columns[j].objectID = owners[i][j].ID
		}

		if rec.kind == constraintPrimaryKey || rec.kind == constraintUnique {
			rec.ownerID = indexes[nextIndex].objectID
			nextIndex++
		}

		records = append(records, rec)
	}

	return indexes, records, nil
}

// constraintOwnerColumns resolves every column a constraint record covers.
// Column-shaped NOT NULL and CHECK records still require exactly one; a key
// record may cover several, which Constraints.abs's CPkMulti pins.
//
// An empty table name passes: a UNIQUE record CREATE UNIQUE INDEX wrote carries
// none (testdata/Keys-uniqidx.abs), and refusing it would make such a table
// uncompactable for a field the engine itself left blank.
func constraintOwnerColumns(table string, rec constraintRecord, columns []Column) ([]Column, error) {
	if rec.table != "" && !strings.EqualFold(rec.table, table) {
		return nil, fmt.Errorf("%w: the constraint %q names table %q, not %q",
			ErrConstraintsNotRebuilt, rec.name, rec.table, table)
	}

	key := rec.kind == constraintPrimaryKey || rec.kind == constraintUnique
	if len(rec.columns) == 0 || (!key && len(rec.columns) != 1) {
		return nil, fmt.Errorf("%w: the constraint %q covers %d columns",
			ErrConstraintsNotRebuilt, rec.name, len(rec.columns))
	}

	owners := make([]Column, 0, len(rec.columns))
	for _, covered := range rec.columns {
		owner, ok := columnByName(columns, covered.name)
		if !ok {
			return nil, fmt.Errorf("%w: the constraint %q covers %q, which is not a column of %q",
				ErrConstraintsNotRebuilt, rec.name, covered.name, table)
		}

		owners = append(owners, owner)
	}

	return owners, nil
}

// keyConstraintIndex builds the index record a PRIMARY KEY or UNIQUE record is
// implemented by, without its root page: that is allocated later and filled in
// by rootIndexRecords, because the stream embeds the page number and the page
// cannot be reserved before the request is known to be buildable.
//
// The UNIQUE and PRIMARY flags follow the kind, which is what Constraints.abs
// shows DBManager writing: CPk's index is primary and not unique, CUnique's is
// unique and not primary. (A private fixture's own "p" index sets both; nothing
// this package writes produces that.)
func keyConstraintIndex(rec constraintRecord, owners []Column, objectID int) (indexRecord, error) {
	if rec.index == "" {
		return indexRecord{}, fmt.Errorf("%w: the %s constraint %q names no index",
			ErrConstraintsNotRebuilt, rec.kind, rec.name)
	}

	if len(owners) == 1 && !indexableKeyColumn(owners[0]) {
		return indexRecord{}, fmt.Errorf(
			"%w: the %s constraint %q is on %q, which is base type %d / field type %s",
			ErrConstraintsNotRebuilt, rec.kind, rec.name,
			owners[0].Name, owners[0].BaseType, owners[0].FieldType,
		)
	}

	columns := make([]indexColumn, len(owners))
	for i, owner := range owners {
		if _, ok := knownEmptyIndexComponentSize(owner); !ok {
			return indexRecord{}, fmt.Errorf(
				"%w: component %q of the %s constraint %q has no measured empty-index width",
				ErrConstraintsNotRebuilt, owner.Name, rec.kind, rec.name,
			)
		}

		columns[i] = indexColumn{
			name: owner.Name, descending: false, caseInsensitive: false,
			maxIndexedSize: indexColumnMaxIndexedSize,
		}
	}

	return indexRecord{
		name:     rec.index,
		objectID: uint32(objectID), //nolint:gosec // small object ids
		unique:   rec.kind == constraintUnique,
		primary:  rec.kind == constraintPrimaryKey,
		columns:  columns,
	}, nil
}

// knownEmptyIndexComponentSize returns the component width established by an
// empty engine-written root. It is deliberately narrower than an encoder: an
// Int32 contributes five bytes, while Constraints.abs establishes twelve for
// its VARCHAR(10), hence declared size plus null flag and terminator. A string
// longer than MaxIndexedSize remains unknown because the empty fixtures cannot
// say whether the engine truncates it there.
func knownEmptyIndexComponentSize(col Column) (int, bool) {
	if indexableKeyColumn(col) {
		return indexKeySize, true
	}

	if col.BaseType == BftVarchar && col.FieldType == FieldString &&
		col.Size > 0 && col.Size <= indexColumnMaxIndexedSize {
		return int(col.Size) + 2, true
	}

	return 0, false
}

// indexRecordKeySize sums the measured component widths in schema order. The
// committed CPkMulti root establishes that 5 + 12 is written as 17.
func indexRecordKeySize(rec indexRecord, columns []Column) (int, error) {
	size := 0

	for _, covered := range rec.columns {
		owner, ok := columnByName(columns, covered.name)
		if !ok {
			return 0, fmt.Errorf("%w: index %q covers unknown column %q", ErrBadSchema, rec.name, covered.name)
		}

		component, ok := knownEmptyIndexComponentSize(owner)
		if !ok {
			return 0, fmt.Errorf("%w: index %q component %q has no measured empty-index width",
				ErrUnsupportedIndexColumn, rec.name, owner.Name)
		}

		size += component
	}

	if size == 0 || size > math.MaxUint16 {
		return 0, fmt.Errorf("%w: index %q key width %d", ErrValueRange, rec.name, size)
	}

	return size, nil
}

// rootIndexRecords fills in each planned index's root page from the pages just
// allocated for them, in the same order.
func rootIndexRecords(indexes []indexRecord, pages []int) []indexRecord {
	out := make([]indexRecord, len(indexes))

	for i, rec := range indexes {
		rec.rootPageNo = int32(pages[i]) //nolint:gosec // small page numbers
		out[i] = rec
	}

	return out
}

// columnByName finds a column by name, case-insensitively like every other
// name lookup here. It takes the slice rather than a TableSchema because the
// columns it searches are the ones assignColumnIDs has just stamped, which no
// schema holds yet.
func columnByName(columns []Column, name string) (Column, bool) {
	for _, c := range columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}

	return Column{}, false
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

// newTablePages names the pages CreateTable allocates for a fresh table: the
// chain the system internal file needs, and one each for the column
// definitions, the counters and the record-page index root.
type newTablePages struct {
	system []int
	schema int
	info   int
	index  int

	// keys is one page per key constraint's backing index, allocated after
	// the record-page index and in the order the constraints appear.
	// Constraints.abs shows the engine allocating them there: CPk's five
	// pages are 15 to 19 and its PRIMARY KEY index is page 20.
	keys []int
}

// systemFilePageCount is how many pages the fresh table's system internal file
// occupies at this file's page size: the whole file rounded up to the payload a
// page carries.
//
// It is not the constant 2 the 4096-byte fixtures show. The file is
// systemInternalFileSize+internalFileHeaderSize bytes whatever the geometry, so
// a 4096-byte page (4056 bytes of payload) needs two of them and a 2048-byte
// page (2008 bytes) needs three -- which is exactly what the engine allocates
// in Empty-p2048-e4-grow.abs, where CREATE TABLE T1 takes six pages rather than
// five and the extra one is a third type-7. Hard-coding 2 got the page count
// right only at 4096 and, worse, got the allocation *order* wrong everywhere
// else: writeInternalFilePages' resizeChain would have appended the third page
// after schema, info and index had already taken the lower numbers.
func (db *File) systemFilePageCount() int {
	payload := db.payloadSize()
	if payload <= 0 {
		return 1
	}

	size := systemInternalFileSize + internalFileHeaderSize

	return max((size+payload-1)/payload, 1)
}

// allocateTablePages reserves and chains CreateTable's pages, in the order the
// engine itself allocates them (see the file comment): the system internal
// file's chain first, then schema, then info, then the record-page index, then
// one page per key constraint's own index.
func (db *File) allocateTablePages(w *pageEdit, keys int) (newTablePages, error) {
	var pages newTablePages

	systemPages, err := db.allocatePages(w, db.systemFilePageCount(), PageTypeSystem, -1)
	if err != nil {
		return pages, err
	}

	pages.system = systemPages

	for i := 1; i < len(systemPages); i++ {
		if err := db.linkChain(w, systemPages[i-1], systemPages[i]); err != nil {
			return pages, err
		}
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

	indexPages, err := db.allocatePages(w, 1+keys, PageTypeIndex, -1)
	if err != nil {
		return pages, err
	}

	pages.index = indexPages[0]
	pages.keys = indexPages[1:]

	return pages, nil
}

// tableInternalFiles is the serialized content CreateTable has ready before it
// allocates anything: the column definitions, the index array and the
// constraint array of the schema stream, plus the column count the counters
// file opens with.
type tableInternalFiles struct {
	colDefs     []byte
	indexes     []byte
	constraints []byte
	columnCount int
	keySizes    []int
}

// writeTableInternalFiles writes the content of every one of CreateTable's new
// pages: the all-zero system file, the empty B-tree index roots, the
// column-definition file, and the counters file.
func (db *File) writeTableInternalFiles(
	w *pageEdit, pages newTablePages, files tableInternalFiles,
) error {
	if err := db.writeInternalFilePages(w, pages.system[0], buildSystemInternalFile()); err != nil {
		return err
	}

	if err := db.writeIndexRoot(w, pages.index); err != nil {
		return err
	}

	// A key constraint's index starts empty, and an empty one is the
	// record-page index's page with a 5-byte key instead of a 4-byte one:
	// pages 20 and 26 of Constraints.abs are byte-identical to page 19 but
	// for that field.
	for i, pageNo := range pages.keys {
		if err := db.writeIndexLeafOfSize(w, pageNo, files.keySizes[i], nil); err != nil {
			return err
		}
	}

	schemaFile, err := compressInternalFile(
		buildSchemaFile(files.colDefs, files.indexes, files.constraints, pages.index), 1,
	)
	if err != nil {
		return err
	}

	if err := db.writeInternalFilePages(w, pages.schema, schemaFile); err != nil {
		return err
	}

	infoFile, err := compressInternalFile(buildTableInfoFile(files.columnCount), 0)
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
//
// The database's own connection table (ddl_database.go) has exactly the same
// shape, which is why buildZeroInternalFile is shared rather than duplicated.
func buildSystemInternalFile() []byte {
	return buildZeroInternalFile(systemInternalFileSize)
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
// the column definitions themselves, the index-definition array, the
// constraint array, and the trailer systemIndexRoots reads.
//
// Both arrays come in already serialized, so a table created with a NOT NULL,
// a MINVALUE/MAXVALUE pair or a PRIMARY KEY declares it from the moment it
// exists, rather than having it spliced in afterwards as a second transaction
// the engine never performs. A table with no key constraint gets the empty
// index array a fresh CREATE TABLE writes.
func buildSchemaFile(colDefs, indexes, constraints []byte, recordIndexRoot int) []byte {
	// colDefs already opens with its own columnCount int32, written by
	// serializeColumnDefs, so it is copied in as-is.
	out := make([]byte, 0, len(colDefs)+len(indexes)+len(constraints)+schemaTrailerSize)
	out = append(out, colDefs...)
	out = append(out, indexes...)
	out = append(out, constraints...)
	out = binary.LittleEndian.AppendUint32(out, uint32(int32(recordIndexRoot))) //nolint:gosec // page number

	return binary.LittleEndian.AppendUint32(out, noPageNo) // blobIndexRoot: no BLOB column supported yet
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
// middle byte are only pinned by corpus evidence for those two. A column
// carrying a DEFAULT is refused with ErrColumnDefault for the same reason in
// reverse: the trailing typed value written here is always the absent marker,
// so a column that had one would come out without it.
func serializeColumnDef(col Column) ([]byte, error) {
	if !knownColumnType(col) {
		return nil, fmt.Errorf("%w: base type %d / field type %s", ErrUnsupportedColumnType, col.BaseType, col.FieldType)
	}

	// Only a column read off a disk image can have either of these set, so
	// they refuse re-serializing a parsed definition whose DEFAULT or whose
	// AUTOINC options the output would lose, never a column a caller built
	// for CreateTable.
	if col.hasDefault {
		return nil, fmt.Errorf("%w: column %q", ErrColumnDefault, col.Name)
	}

	if !col.autoInc.engineDefault() {
		return nil, fmt.Errorf("%w: column %q", ErrColumnAutoIncOptions, col.Name)
	}

	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(col.Name))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrStringEncoding, col.Name, err)
	}

	if len(raw) == 0 || len(raw) > 255 {
		return nil, fmt.Errorf("%w: %d-byte column name", ErrValueRange, len(raw))
	}

	out := make([]byte, 0, 1+len(raw)+4+2+4+autoIncBlockSize+columnDefDefaultSize)

	out = append(out, byte(len(raw))) //nolint:gosec // checked above: len(raw) <= 255
	out = append(out, raw...)

	var idBuf, sizeBuf [4]byte

	binary.LittleEndian.PutUint32(idBuf[:], col.ID)
	out = append(out, idBuf[:]...)

	out = append(out, byte(col.BaseType), byte(col.FieldType))

	binary.LittleEndian.PutUint32(sizeBuf[:], col.Size)
	out = append(out, sizeBuf[:]...)

	// TABSFieldDef's five autoinc fields, at the engine's defaults. Every
	// column in the corpus carries exactly these; a parsed column that does
	// not was refused above.
	var block [8]byte

	for _, v := range []int64{1, 0, 0, math.MaxInt64} {
		binary.LittleEndian.PutUint64(block[:], uint64(v))
		out = append(out, block[:]...)
	}

	out = append(out, 0) // AutoincCycled

	// The DEFAULT typed value: the variant's type tag, then the absent
	// marker. serializeColumnDef never writes a present one; a column
	// carrying a DEFAULT was refused above.
	out = append(out, byte(col.BaseType), typedValueAbsent)

	return out, nil
}
