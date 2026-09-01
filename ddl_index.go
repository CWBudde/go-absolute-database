package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// CREATE INDEX and DROP INDEX -- the third and fourth schema operations Phase 8
// lists. Like CREATE TABLE (ddl_create.go), CREATE INDEX rewrites the
// compressed column-definition internal file, which is why it waited for
// internal/zlib1.Compress the same way CREATE TABLE did.
//
// What the two operations write is decoded in ddl_constraint.go (the schema
// stream's second array) and here (its first), and is summarised as:
//
//   - the column-definition stream continues past the column array with
//     int32 indexCount, indexCount index records, int32 constraintCount,
//     constraintCount constraint records, and then the two page numbers
//     systemIndexRoots reads (record-page index root, BLOB-page index root);
//   - an index record is a Pascal name, a database-wide object id, three flag
//     bytes (reserved, UNIQUE, PRIMARY), coveredColumnCount, the index's own
//     B-tree root page number, and then one entry per covered column: a Pascal
//     name, a DESC flag byte, a NOCASE flag byte and an int32 maximum indexed
//     size (0x14 in every record in the corpus);
//   - CREATE INDEX allocates the index's root page before serializing the
//     stream, because the stream embeds that page's number;
//   - CREATE INDEX hands out one object id, the next free one, and advances
//     LastObjectID by it;
//   - a new index record is spliced in at the end of the index array, before
//     the constraint array, which is copied through byte for byte.
//
// The per-column DESC and NOCASE flags and the multi-column form were decoded
// from testdata/Constraints.abs: CIdxDesc, CIdxNoCase and CIdxMulti differ from
// CIdxOne in exactly one of them. Before that file existed, coveredColumnCount
// was only ever seen as 1 and the two flag bytes read as a reserved field, so a
// multi-column index -- which every indexed private fixture has -- was refused.
//
// A populated index is built for one or more ascending, case-sensitive Int32
// columns. MultiKeys.abs pins that each ordinary five-byte component is
// concatenated in schema order and compared lexicographically. A rowless
// compound CREATE INDEX may additionally carry a measured VARCHAR component:
// Constraints.abs pins its metadata and summed width, while an occupied string
// component remains refused by the separate VARCHAR-key boundary.

var (
	// ErrIndexExists reports a CREATE INDEX naming an index that already exists
	// on the table.
	ErrIndexExists = errors.New("absdb: index already exists")

	// ErrNoSuchIndex reports a DROP INDEX naming an index the table does not
	// have.
	ErrNoSuchIndex = errors.New("absdb: no such index")

	// ErrNoSuchColumn reports a CREATE INDEX or a DROP COLUMN (ddl_alter.go)
	// naming a column the table does not have.
	ErrNoSuchColumn = errors.New("absdb: no such column")

	// ErrMultiColumnIndex reports a compound operation whose occupied component
	// shape is still unmeasured, currently a string component or the generated
	// constraint form of CREATE UNIQUE INDEX. All-Int32 compound leaves are
	// built and maintained from the MultiKeys*.abs evidence.
	ErrMultiColumnIndex = errors.New("absdb: occupied multi-column index shape is not supported")

	// ErrIndexBacksConstraint reports a DROP INDEX naming the index a PRIMARY
	// KEY or UNIQUE constraint record is built on. Dropping it would leave
	// that record naming an index the file no longer has, and nothing in the
	// corpus says what the engine does about the constraint when its index
	// goes away -- DBManager drops the constraint, not the index. Refusing is
	// the only outcome this package can show to be safe.
	ErrIndexBacksConstraint = errors.New("absdb: index implements a PRIMARY KEY or UNIQUE constraint")

	// ErrUnsupportedIndexColumn reports a CREATE INDEX over a column whose type
	// the leaf-entry format has no corpus evidence for. Every measured index in
	// the corpus covers an Int32/Integer column, and the engine's own leaf
	// entry layout (docs/format/indexes.md) is "[null flag byte] + int32 LE key", so that is
	// the only column type this package builds an index over.
	ErrUnsupportedIndexColumn = errors.New("absdb: index column type has no corpus evidence for CREATE INDEX")

	// ErrSchemaTailNotUnderstood reports a column-definition stream whose tail
	// -- the index array, the constraint array and the two trailing page
	// numbers -- does not parse as the layout ddl_constraint.go documents. It
	// used to cover every table carrying a constraint record at all, which was
	// most real tables; testdata/Constraints.abs retired that, and what is left
	// is the genuinely unknown: a constraint kind the corpus does not show, a
	// reserved field that is not zero, a size field that disagrees with the
	// string it introduces, or bytes left over once both arrays are read.
	ErrSchemaTailNotUnderstood = errors.New("absdb: schema stream tail is not understood")

	// ErrIndexTooManyRows reports a CREATE INDEX over a table with more rows
	// than fit on a single B-tree leaf page. Multi-page index trees are not
	// built by this package.
	ErrIndexTooManyRows = errors.New("absdb: table has too many rows for a single-page index")
)

const (
	// indexRecordFlagsSize is the width of the flag field between an index
	// record's objectId and its coveredColumnCount. The first byte is zero in
	// every record in the corpus; the second marks UNIQUE and the third
	// PRIMARY, both as 0x00/0xFF booleans. Constraints.abs's CPk and CUnique
	// isolate them: 00 00 FF for a primary key, 00 FF 00 for a unique index,
	// both set at once for a private fixture's "p", all zero for a plain one.
	indexRecordFlagsSize = 3

	// indexColumnMaxIndexedSize is the int32 closing every covered-column
	// entry, DBManager's MaxIndexedSize. It is 0x14 in all 41 index records in
	// the corpus, over Int32 and Varchar columns alike, so a different value is
	// data this package has no evidence for and refuses.
	indexColumnMaxIndexedSize = 0x00000014

	// indexFlagTrue is how the format spells a true ByteBool in an index
	// record: DESC, NOCASE, UNIQUE and PRIMARY are all 0xFF when set and 0x00
	// when not.
	indexFlagTrue = 0xFF

	// indexKeySize is primaryKeySize spelled out for index records this file
	// builds: one null-flag byte plus an int32 LE key (index.go).
	indexKeySize = primaryKeySize
)

// indexColumn is one covered column of an index record: its name and the two
// per-column flags the CREATE INDEX grammar spells ASC/DESC and CASE/NOCASE.
// maxIndexedSize is DBManager's fourth per-column property, constant across the
// corpus and carried here only so a record round-trips through the parse.
type indexColumn struct {
	name            string
	descending      bool
	caseInsensitive bool
	maxIndexedSize  uint32
}

// indexRecord is one parsed entry of the schema stream's index-definition
// array. start and end are its byte range within the decompressed schema
// stream, so a caller can splice around it.
type indexRecord struct {
	name       string
	objectID   uint32
	unique     bool
	primary    bool
	rootPageNo int32
	columns    []indexColumn
	start, end int
}

// singleColumn returns the one column this index covers, and reports false for
// a multi-column index. Callers that specifically implement single-column
// lookup or metadata presentation ask here rather than assuming it.
func (r indexRecord) singleColumn() (indexColumn, bool) {
	if len(r.columns) != 1 {
		return indexColumn{}, false
	}

	return r.columns[0], true
}

// coversColumn reports whether this index covers a column of that name,
// case-insensitively. A multi-column index counts, which is what DropColumn
// needs: dropping any covered column orphans the whole record.
func (r indexRecord) coversColumn(name string) bool {
	for _, c := range r.columns {
		if strings.EqualFold(c.name, name) {
			return true
		}
	}

	return false
}

// createIndexPlan holds what validating a CREATE INDEX request produces: the
// table, the columns to key by, their measured empty-root width, and the schema
// stream already split at the points a new record is spliced in at. Splitting CreateIndex into "plan" and
// "apply" keeps each half's branching simple enough for cyclop -- the plan is
// all refusals, the apply is all writes.
type createIndexPlan struct {
	table        *Table
	colIdxs      []int
	columns      []Column
	keySize      int
	schemaPageNo int
	raw          []byte
	colsEnd      int
	indexCount   int32
	tailStart    int

	// constraintCount is what the constraint array opens with. A unique index
	// adds a record to that array as well as to the index one, so the count
	// has to be rewritten rather than copied through.
	constraintCount int
}

// planCreateIndex validates a CREATE INDEX request and returns everything
// applyCreateIndex needs, without writing anything.
func (db *File) planCreateIndex(table, index string, columns []string) (createIndexPlan, error) {
	t, err := db.Table(table)
	if err != nil {
		return createIndexPlan{}, err
	}

	schema, err := t.Schema()
	if err != nil {
		return createIndexPlan{}, err
	}

	colIdxs, resolved, keySize, err := resolveCreateIndexColumns(schema, index, columns)
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
	colsEnd, indexCount, records, constraints, tailStart, err := parseSchemaTail(raw)
	if err != nil {
		return createIndexPlan{}, err
	}

	for _, r := range records {
		if strings.EqualFold(r.name, index) {
			return createIndexPlan{}, fmt.Errorf("%w: %q on %q", ErrIndexExists, index, t.Name())
		}
	}

	return createIndexPlan{
		table:           t,
		colIdxs:         colIdxs,
		columns:         resolved,
		keySize:         keySize,
		schemaPageNo:    schemaPageNo,
		raw:             raw,
		colsEnd:         colsEnd,
		indexCount:      indexCount,
		tailStart:       tailStart,
		constraintCount: len(constraints),
	}, nil
}

// resolveCreateIndexColumns resolves and sizes the request independently of
// the schema stream splice, keeping planCreateIndex's validation readable.
func resolveCreateIndexColumns(
	schema *TableSchema, index string, columns []string,
) ([]int, []Column, int, error) {
	if len(columns) == 0 || len(columns) > maxSchemaColumns {
		return nil, nil, 0, fmt.Errorf("%w: index %q covers %d columns", ErrValueRange, index, len(columns))
	}

	colIdxs := make([]int, len(columns))
	resolved := make([]Column, len(columns))
	keySize := 0

	for i, column := range columns {
		colIdx, err := findColumnIndex(schema, column)
		if err != nil {
			return nil, nil, 0, err
		}

		colIdxs[i], resolved[i] = colIdx, schema.Columns[colIdx]

		component, ok := knownEmptyIndexComponentSize(resolved[i])
		if !ok || (len(columns) == 1 && !indexableKeyColumn(resolved[i])) {
			return nil, nil, 0, fmt.Errorf("%w: %q is base type %d / field type %s",
				ErrUnsupportedIndexColumn, resolved[i].Name, resolved[i].BaseType, resolved[i].FieldType)
		}

		keySize += component
	}

	if keySize > math.MaxUint16 {
		return nil, nil, 0, fmt.Errorf("%w: index %q key width %d", ErrValueRange, index, keySize)
	}

	return colIdxs, resolved, keySize, nil
}

// splice rebuilds the schema stream with the new index record appended to the
// index array, and -- for a unique index -- the constraint record appended to
// the constraint array as well.
//
// Appending to both is what testdata/Keys-uniqidx.abs shows: CREATE UNIQUE
// INDEX IdxAlt ON Keys (Alt) leaves the primary key's records first and adds
// IdxAlt and C_Unique$Alt behind them.
func (p createIndexPlan) splice(record, constraint []byte) []byte {
	indexArray := p.raw[p.colsEnd+4 : p.tailStart]

	if constraint == nil {
		return spliceIndexRecord(p.raw, p.colsEnd, p.indexCount+1, indexArray, record, p.raw[p.tailStart:])
	}

	trailerStart := len(p.raw) - systemIndexRootsSize
	count := binary.LittleEndian.AppendUint32(nil, uint32(p.constraintCount+1)) //nolint:gosec // small counts

	return spliceIndexRecord(p.raw, p.colsEnd, p.indexCount+1,
		indexArray, record,
		count, p.raw[p.tailStart+4:trailerStart], constraint,
		p.raw[trailerStart:])
}

// CreateIndex adds an index to a table. Existing single-column calls pass one
// column; additional names build the multi-column schema record and root.
// Constraints.abs establishes empty mixed-component roots, and MultiKeys.abs
// establishes populated all-Int32 roots.
//
// It fails with ErrReadOnly unless the file was opened with OpenForWrite, and
// refuses rather than guess when: the table does not exist (ErrNoSuchTable),
// the index name is already used (ErrIndexExists), the column does not exist
// (ErrNoSuchColumn), an occupied component is not an Int32/Integer column
// (ErrUnsupportedIndexColumn/ErrMultiColumnIndex), the schema stream tail does not parse
// (ErrSchemaTailNotUnderstood), or the table has more rows than fit on one
// index leaf page (ErrIndexTooManyRows).
//
// A table carrying NOT NULL, PRIMARY KEY, UNIQUE or MINVALUE/MAXVALUE
// constraints is no longer refused: its constraint records are parsed, and the
// new index record is spliced in ahead of them so they come back byte for byte.
// Note that the new index is a plain one -- CreateIndex neither creates nor
// enforces a constraint. CreateUniqueIndex does both.
func (db *File) CreateIndex(table, index string, columns ...string) error {
	return db.createIndex(table, index, columns, false)
}

// CreateUniqueIndex adds a single-column index that refuses a duplicate key,
// the way CREATE UNIQUE INDEX does.
//
// It is not CreateIndex with a flag set in the file: the engine writes a UNIQUE
// constraint record alongside the index and hands out two object ids rather
// than one, which testdata/Keys-uniqidx.abs is the evidence for. The record's
// name is generated from the covered column -- "C_Unique$Alt" for a unique
// index on Alt -- and its table name is left empty, both of which that file
// pins.
//
// Every refusal CreateIndex makes applies, plus one: a column already holding
// two equal values cannot be indexed uniquely (ErrDuplicateKey), which is what
// the SDK manual says the engine checks "when the index is created (if data
// already exist)". A NULL counts as a value, so two of those collide as well.
func (db *File) CreateUniqueIndex(table, index string, columns ...string) error {
	return db.createIndex(table, index, columns, true)
}

func (db *File) createIndex(table, index string, columns []string, unique bool) error {
	if !db.writable {
		return ErrReadOnly
	}

	plan, err := db.planCreateIndex(table, index, columns)
	if err != nil {
		return err
	}

	entries, err := db.createIndexEntries(plan, index, unique)
	if err != nil {
		return err
	}

	if unique {
		if err := refuseDuplicateEntries(entries, index, plan.columns[0].Name, len(plan.colIdxs)); err != nil {
			return err
		}
	}

	w := newPageEdit(db)

	indexPages, err := db.allocatePages(w, 1, PageTypeIndex, -1)
	if err != nil {
		return err
	}

	rootPageNo := indexPages[0]

	if err := db.writeIndexLeafOfSize(w, rootPageNo, plan.keySize, entries); err != nil {
		return err
	}

	objectID := int(db.lastObjectID) + 1

	record, constraint, err := serializeNewIndex(plan, index, objectID, rootPageNo, unique)
	if err != nil {
		return err
	}

	if err := db.writeSchemaStream(w, plan.schemaPageNo, plan.splice(record, constraint)); err != nil {
		return err
	}

	if err := db.flushPages(w.order, w.pages); err != nil {
		return err
	}

	last := objectID
	if unique {
		last++
	}

	if err := db.setLastObjectID(int32(last)); err != nil { //nolint:gosec // small object ids
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

// createIndexEntries builds occupied entries for the measured all-Int32 shape.
// An empty compound index may additionally contain components whose width is
// known from an empty engine root but whose occupied encoding is not yet known.
func (db *File) createIndexEntries(plan createIndexPlan, index string, unique bool) ([]BTreeEntry, error) {
	if unique && len(plan.columns) > 1 {
		return nil, fmt.Errorf("%w: UNIQUE index %q covers %d columns",
			ErrMultiColumnIndex, index, len(plan.columns))
	}

	allInt32 := true
	for _, column := range plan.columns {
		allInt32 = allInt32 && indexableKeyColumn(column)
	}

	if allInt32 {
		return db.buildIndexLeafEntries(plan.table, plan.colIdxs)
	}

	empty, err := tableIsEmpty(plan.table)
	if err != nil || empty {
		return nil, err
	}

	return nil, fmt.Errorf("%w: index %q has an unmeasured occupied compound component",
		ErrMultiColumnIndex, index)
}

// tableIsEmpty checks whether a request needs occupied component evidence.
// All-Int32 compound indexes no longer need this escape hatch; mixed compound
// roots retain it because their empty width is measured but their string bytes
// are not.
func tableIsEmpty(t *Table) (bool, error) {
	r, err := t.Open()
	if err != nil {
		return false, err
	}

	if r.Next() {
		return false, nil
	}

	if err := r.Err(); err != nil {
		return false, err
	}

	return true, nil
}

// serializeNewIndex builds the records CREATE INDEX splices in: the index
// record always, and the UNIQUE constraint record naming it when the index is
// a unique one. The constraint takes the object id after the index's, which is
// the order testdata/Keys-uniqidx.abs hands them out in.
func serializeNewIndex(
	plan createIndexPlan, index string, objectID, rootPageNo int, unique bool,
) (record, constraint []byte, err error) {
	covered := make([]indexColumn, len(plan.columns))
	for i, column := range plan.columns {
		covered[i] = indexColumn{
			name: column.Name, descending: false, caseInsensitive: false,
			maxIndexedSize: indexColumnMaxIndexedSize,
		}
	}

	record, err = serializeIndexRecord(indexRecord{
		name:       index,
		objectID:   uint32(objectID), //nolint:gosec // small object ids
		unique:     unique,
		rootPageNo: int32(rootPageNo), //nolint:gosec // small page numbers
		columns:    covered,
	})
	if err != nil {
		return nil, nil, err
	}

	if !unique {
		return record, nil, nil
	}

	constraint, err = serializeConstraintRecord(constraintRecord{
		kind:     constraintUnique,
		name:     uniqueConstraintName(plan.columns[0].Name),
		objectID: uint32(objectID + 1), //nolint:gosec // small object ids
		ownerID:  uint32(objectID),     //nolint:gosec // small object ids
		index:    index,
		columns:  []constraintColumn{{name: plan.columns[0].Name, objectID: plan.columns[0].ID}},
	})
	if err != nil {
		return nil, nil, err
	}

	return record, constraint, nil
}

// uniqueConstraintName is the name the engine generates for the constraint a
// UNIQUE clause or a CREATE UNIQUE INDEX produces: "C_Unique$" and the covered
// column. Constraints.abs's CUnique gives "C_Unique$A" for the clause and
// Keys-uniqidx.abs gives "C_Unique$Alt" for the statement, so the two routes
// agree.
func uniqueConstraintName(column string) string {
	return "C_Unique$" + column
}

// refuseDuplicateEntries refuses to build a unique index over a column that
// already holds a key twice, which the SDK manual says the engine checks when
// the index is created. The entries are sorted by key, so equal keys are
// adjacent.
func refuseDuplicateEntries(entries []BTreeEntry, index, column string, components int) error {
	for i := 1; i < len(entries); i++ {
		if compareCompoundInt32Keys(entries[i-1].Key, entries[i].Key, components) == 0 {
			return fmt.Errorf("%w: %q already holds two rows with the same value, so %q cannot be unique",
				ErrDuplicateKey, column, index)
		}
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
// the index does not exist (ErrNoSuchIndex), the table's schema stream tail
// does not parse (ErrSchemaTailNotUnderstood), or the index implements a
// PRIMARY KEY or UNIQUE constraint (ErrIndexBacksConstraint).
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

	colsEnd, indexCount, records, constraints, tailStart, err := parseSchemaTail(raw)
	if err != nil {
		return err
	}

	rec, err := findIndexRecord(records, index, t.Name())
	if err != nil {
		return err
	}

	if err := refuseConstraintIndex(rec, constraints, t.Name()); err != nil {
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

// refuseConstraintIndex reports ErrIndexBacksConstraint when rec is the index a
// PRIMARY KEY or UNIQUE constraint record is built on. Both halves of the
// evidence are checked -- the index record's own PRIMARY/UNIQUE flags and a
// constraint record pointing at its object id -- because either one alone
// would let the other shape through.
func refuseConstraintIndex(rec indexRecord, constraints []constraintRecord, table string) error {
	if rec.primary || rec.unique {
		return fmt.Errorf("%w: %q on %q", ErrIndexBacksConstraint, rec.name, table)
	}

	for _, c := range constraints {
		if c.ownerID == rec.objectID {
			return fmt.Errorf("%w: %q on %q backs %s %q", ErrIndexBacksConstraint, rec.name, table, c.kind, c.name)
		}
	}

	return nil
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

// parseSchemaTail parses everything the column array is followed by: the index
// array, the constraint array, and the two trailing page numbers, requiring
// exactly systemIndexRootsSize bytes to remain once both arrays are read.
//
// That final length check is what makes the parse self-validating. Both arrays
// are variable-length and full of length-prefixed strings, so a record read one
// field out of step almost never lands on the trailer -- which is why the whole
// corpus parsing to the byte is evidence the layout in ddl_constraint.go is
// right, and why a stream that does not is refused rather than half-read.
//
// tailStart is the offset of the constraint count, i.e. the first byte past the
// index array. Everything from there on is copied through verbatim by the
// splicing callers, so a constrained table's records survive CREATE INDEX and
// DROP INDEX byte for byte.
func parseSchemaTail(data []byte) (
	colsEnd int,
	indexCount int32,
	records []indexRecord,
	constraints []constraintRecord,
	tailStart int,
	err error,
) {
	colsEnd, err = schemaColumnsEnd(data)
	if err != nil {
		return 0, 0, nil, nil, 0, err
	}

	rawCount, pos, err := readArrayCount(data, colsEnd, "indexCount")
	if err != nil {
		return 0, 0, nil, nil, 0, err
	}

	records, pos, err = parseIndexRecords(data, pos, int(rawCount))
	if err != nil {
		return 0, 0, nil, nil, 0, fmt.Errorf("%w: index record array: %w", ErrSchemaTailNotUnderstood, err)
	}

	tailStart = pos

	constraintCount, pos, err := readArrayCount(data, pos, "constraintCount")
	if err != nil {
		return 0, 0, nil, nil, 0, err
	}

	constraints, pos, err = parseConstraintRecords(data, pos, int(constraintCount))
	if err != nil {
		return 0, 0, nil, nil, 0, fmt.Errorf("%w: constraint record array: %w", ErrSchemaTailNotUnderstood, err)
	}

	if remaining := len(data) - pos; remaining != systemIndexRootsSize {
		return 0, 0, nil, nil, 0, fmt.Errorf(
			"%w: %d bytes remain after %d index and %d constraint record(s), want the %d-byte root/blobroot trailer",
			ErrSchemaTailNotUnderstood, remaining, rawCount, constraintCount, systemIndexRootsSize,
		)
	}

	return colsEnd, rawCount, records, constraints, tailStart, nil
}

// readArrayCount reads one of the schema tail's two int32 record counts and
// bounds it against the bytes actually left, so a corrupt count cannot ask for
// a huge slice before a single record has been read.
func readArrayCount(data []byte, pos int, field string) (int32, int, error) {
	if pos+4 > len(data) {
		return 0, 0, fmt.Errorf("%w: no room for %s at offset %d", ErrBadSchema, field, pos)
	}

	count := int32(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4

	if count < 0 || int64(count) > int64(len(data)-pos) {
		return 0, 0, fmt.Errorf("%w: %s %d exceeds %d remaining bytes", ErrBadSchema, field, count, len(data)-pos)
	}

	return count, pos, nil
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

// parseIndexRecord parses one index record: the header, then one entry per
// covered column. The two flag bytes after each column name and the
// coveredColumnCount above 1 are what Constraints.abs's CIdxDesc, CIdxNoCase
// and CIdxMulti pin; everything a covered-column entry holds is read, so a
// multi-column or descending index is now understood rather than refused.
func parseIndexRecord(data []byte, pos int) (indexRecord, int, error) {
	rec := indexRecord{}

	var err error

	rec.name, pos, err = readPascalString(data, pos)
	if err != nil {
		return indexRecord{}, 0, fmt.Errorf("name: %w", err)
	}

	rec.objectID, pos, err = readUint32(data, pos, "objectId")
	if err != nil {
		return indexRecord{}, 0, err
	}

	pos, err = parseIndexRecordFlags(&rec, data, pos)
	if err != nil {
		return indexRecord{}, 0, err
	}

	coveredCount, pos, err := readUint32(data, pos, "coveredColumnCount")
	if err != nil {
		return indexRecord{}, 0, err
	}

	if coveredCount == 0 || coveredCount > maxSchemaColumns || int64(coveredCount) > int64(len(data)-pos) {
		return indexRecord{}, 0, fmt.Errorf("coveredColumnCount %d exceeds %d remaining bytes", coveredCount, len(data)-pos)
	}

	rootPage, pos, err := readUint32(data, pos, "root page")
	if err != nil {
		return indexRecord{}, 0, err
	}

	rec.rootPageNo = int32(rootPage)
	rec.columns = make([]indexColumn, 0, coveredCount)

	for i := range int(coveredCount) {
		var col indexColumn

		col, pos, err = parseIndexColumn(data, pos)
		if err != nil {
			return indexRecord{}, 0, fmt.Errorf("covered column %d: %w", i, err)
		}

		rec.columns = append(rec.columns, col)
	}

	return rec, pos, nil
}

// parseIndexRecordFlags reads the three flag bytes between an index record's
// objectId and its coveredColumnCount. The first is zero everywhere in the
// corpus and is refused if it is not; the other two are the UNIQUE and PRIMARY
// booleans.
func parseIndexRecordFlags(rec *indexRecord, data []byte, pos int) (int, error) {
	if pos+indexRecordFlagsSize > len(data) {
		return 0, errors.New("truncated flags")
	}

	if data[pos] != 0 {
		return 0, fmt.Errorf("index flag byte 0 = %#x, want 0", data[pos])
	}

	var err error

	rec.unique, err = indexFlagBool(data[pos+1], "UNIQUE")
	if err != nil {
		return 0, err
	}

	rec.primary, err = indexFlagBool(data[pos+2], "PRIMARY")
	if err != nil {
		return 0, err
	}

	return pos + indexRecordFlagsSize, nil
}

// parseIndexColumn reads one covered-column entry: the column name, the DESC
// and NOCASE flags, and the maximum indexed size.
func parseIndexColumn(data []byte, pos int) (indexColumn, int, error) {
	name, pos, err := readPascalString(data, pos)
	if err != nil {
		return indexColumn{}, 0, fmt.Errorf("name: %w", err)
	}

	if pos+2 > len(data) {
		return indexColumn{}, 0, errors.New("truncated column flags")
	}

	descending, err := indexFlagBool(data[pos], "DESC")
	if err != nil {
		return indexColumn{}, 0, err
	}

	caseInsensitive, err := indexFlagBool(data[pos+1], "NOCASE")
	if err != nil {
		return indexColumn{}, 0, err
	}

	pos += 2

	maxIndexedSize, pos, err := readUint32(data, pos, "maximum indexed size")
	if err != nil {
		return indexColumn{}, 0, err
	}

	if maxIndexedSize != indexColumnMaxIndexedSize {
		return indexColumn{}, 0, fmt.Errorf("maximum indexed size = %#x, want %#x", maxIndexedSize, indexColumnMaxIndexedSize)
	}

	return indexColumn{
		name:            name,
		descending:      descending,
		caseInsensitive: caseInsensitive,
		maxIndexedSize:  maxIndexedSize,
	}, pos, nil
}

// indexFlagBool decodes one of the format's 0x00/0xFF ByteBools, naming the
// flag in its error. Any other value is a byte this package has no evidence
// for, and reading it as "true" would be a guess.
func indexFlagBool(b byte, name string) (bool, error) {
	switch b {
	case 0:
		return false, nil
	case indexFlagTrue:
		return true, nil
	default:
		return false, fmt.Errorf("%s flag = %#x, want 0x00 or 0xFF", name, b)
	}
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

// serializeIndexArray builds the schema stream's index-definition array: the
// int32 count and that many records. It is the index-side mirror of
// serializeConstraintArray, and CreateTable uses it for a table whose PRIMARY
// KEY or UNIQUE constraint brings an index with it.
func serializeIndexArray(records []indexRecord) ([]byte, error) {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(records))) //nolint:gosec // bounded by maxSchemaColumns upstream

	for _, rec := range records {
		raw, err := serializeIndexRecord(rec)
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", rec.name, err)
		}

		out = append(out, raw...)
	}

	return out, nil
}

// serializeIndexRecord builds one index record, the mirror of parseIndexRecord:
// a Pascal name, the object id, the reserved/UNIQUE/PRIMARY flag bytes, the
// covered-column count, the index's own root page, and one entry per covered
// column. Each column entry is a Pascal name, its DESC and NOCASE flags, and
// the constant maximum indexed size.
//
// The bytes for a plain index are the same ones this function wrote before
// covered-column entries were decoded, which is what keeps
// TestCreateIndexMatchesEngineByteForByte passing unchanged: what used to be
// "2 reserved bytes plus a 0x14 terminator" is the ASC/CASE/20 entry the
// engine writes for an ascending, case-sensitive index. Constraints.abs's
// CPkMulti and CIdxMulti pin the repeated-column form byte for byte; supporting
// it here is independent of the occupied encoder, whose all-Int32 form is
// pinned separately by MultiKeys.abs.
func serializeIndexRecord(rec indexRecord) ([]byte, error) {
	rawName, err := encodePascalName(rec.name)
	if err != nil {
		return nil, err
	}

	if len(rec.columns) == 0 || len(rec.columns) > maxSchemaColumns {
		return nil, fmt.Errorf("%w: index %q covers %d columns", ErrValueRange, rec.name, len(rec.columns))
	}

	rawColumns := make([][]byte, len(rec.columns))
	size := 1 + len(rawName) + 4 + indexRecordFlagsSize + 4 + 4

	for i, col := range rec.columns {
		rawColumns[i], err = encodePascalName(col.name)
		if err != nil {
			return nil, fmt.Errorf("covered column %d: %w", i, err)
		}

		size += 1 + len(rawColumns[i]) + 2 + 4
	}

	out := make([]byte, 0, size)

	out = append(out, byte(len(rawName))) //nolint:gosec // checked in encodePascalName
	out = append(out, rawName...)

	var buf4 [4]byte

	binary.LittleEndian.PutUint32(buf4[:], rec.objectID)
	out = append(out, buf4[:]...)

	out = append(out, 0, indexFlagByte(rec.unique), indexFlagByte(rec.primary))

	binary.LittleEndian.PutUint32(buf4[:], uint32(len(rec.columns))) //nolint:gosec // bounded above
	out = append(out, buf4[:]...)

	binary.LittleEndian.PutUint32(buf4[:], uint32(rec.rootPageNo))
	out = append(out, buf4[:]...)

	for i, col := range rec.columns {
		rawColumn := rawColumns[i]
		out = append(out, byte(len(rawColumn))) //nolint:gosec // checked in encodePascalName
		out = append(out, rawColumn...)
		out = append(out, indexFlagByte(col.descending), indexFlagByte(col.caseInsensitive))

		binary.LittleEndian.PutUint32(buf4[:], indexColumnMaxIndexedSize)
		out = append(out, buf4[:]...)
	}

	return out, nil
}

// indexFlagByte spells a ByteBool the way an index record does: 0xFF when set
// and 0x00 when not, the encoding DESC, NOCASE, UNIQUE and PRIMARY all share.
func indexFlagByte(set bool) byte {
	if set {
		return indexFlagTrue
	}

	return 0
}

// encodePascalName Windows-1252 encodes a name for a Pascal string field, the
// same encoding column, index, table and constraint names all use. It is the
// one place a name a caller chose is checked against what a length byte can
// describe, which is why every serializer here goes through it.
func encodePascalName(name string) ([]byte, error) {
	raw, err := encodeOptionalPascalName(name)
	if err != nil {
		return nil, err
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: 0-byte name", ErrValueRange)
	}

	return raw, nil
}

// encodeOptionalPascalName is encodePascalName for the one field the engine
// leaves empty. A UNIQUE constraint record that CREATE UNIQUE INDEX wrote
// carries an empty table name -- testdata/Keys-uniqidx.abs holds
// "01 00 00 00 00" where the same field of a CREATE TABLE ... PRIMARY KEY
// record holds "05 00 00 00 04 'K' 'e' 'y' 's'" -- so refusing an empty name
// everywhere made that record unwritable. Only the key-shaped body's table
// name is ever empty; every other name goes through encodePascalName.
func encodeOptionalPascalName(name string) ([]byte, error) {
	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(name))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrStringEncoding, name, err)
	}

	if len(raw) > 255 {
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
// entries a single-page B-tree index over one or more Int32 columns holds:
// schema-order five-byte components, then the row's data page and slot.
func (db *File) buildIndexLeafEntries(t *Table, colIdxs []int) ([]BTreeEntry, error) {
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

		key := indexKeyOf(rec, colIdxs)

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
		return compareCompoundInt32Keys(entries[i].Key, entries[j].Key, len(colIdxs)) < 0
	})

	return entries, nil
}

// writeIndexLeafOfSize writes a single B-tree root/leaf page holding entries:
// IsRoot=true, IsLeaf=true, no siblings, HasKeys=true, HasSuffixes=false, keyed
// by the explicit measured width and followed by one reference-suffixed entry
// per row. CreateTable uses it for empty keys, and CreateIndex for occupied
// entries after buildIndexLeafEntries has encoded them.
func (db *File) writeIndexLeafOfSize(w *pageEdit, pageNo, keySize int, entries []BTreeEntry) error {
	buf, err := w.load(pageNo)
	if err != nil {
		return err
	}

	stride := keySize + leafEntrySuffixSize
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
	binary.LittleEndian.PutUint16(h[12:14], uint16(keySize))      //nolint:gosec // validated by the caller's schema plan
	binary.LittleEndian.PutUint16(h[14:16], uint16(len(entries))) //nolint:gosec // bounded by the capacity check above
	binary.LittleEndian.PutUint16(h[16:18], 0)                    // PagePrefixSize

	for i, e := range entries {
		off := btreeHeaderSize + i*stride
		copy(buf.payload[off:off+keySize], e.Key)
		binary.LittleEndian.PutUint32(buf.payload[off+keySize:off+keySize+4], uint32(e.PageNo))
		binary.LittleEndian.PutUint16(buf.payload[off+keySize+4:off+keySize+6], e.ItemNo)
	}

	buf.dirty = true

	return nil
}
