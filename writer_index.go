package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Index maintenance: keeping a table's user indexes in step with its records.
//
// Until this existed, insert and delete were refused outright on any table
// carrying a user index (ErrIndexNotMaintained), and Update was worse -- it
// went through and left the index describing a key the row no longer has. Four
// engine-made fixtures pin what the engine does instead, each one statement
// away from Writes-idx.abs, whose IdxId keys the four-column Writes table on
// Id:
//
//	Writes-idx-ins.abs   INSERT ... (4,...)   key sorts last   append
//	Writes-idx-ins0.abs  INSERT ... (0,...)   key sorts first  shift the array up
//	Writes-idx-del.abs   DELETE WHERE Id = 2  middle entry     shift the tail down
//	Writes-idx-upd.abs   UPDATE SET Id = 9    indexed column   remove, then insert
//
// A leaf is a sorted array of fixed-stride entries behind an 18-byte header, so
// all four reduce to the same two primitives: splice an entry in at its sorted
// position, or splice one out. Two details are not guessable and come straight
// off those files:
//
//   - a removal does NOT clear the slot it vacates. Writes-idx-del.abs shifts
//     entry 2 down onto entry 1 and drops EntryCount to 2, leaving the old
//     entry 2's eleven bytes exactly where they were. Clearing that tail is the
//     obvious thing to do and misses byte identity by eleven bytes.
//   - an update of an indexed column is a removal followed by a sorted
//     insertion, not an in-place patch of the key: Writes-idx-upd.abs turns the
//     keys [1,2,3] into [1,3,9] with EntryCount unchanged, which an in-place
//     patch would have left as [1,9,3].
//
// What is still refused, because no fixture pins it and this package does not
// guess at B-tree shape:
//
//   - an index that is not a single root-and-leaf page, that is one deep enough
//     to have split (ErrIndexNotMaintained);
//   - an insert into a leaf with no room for another entry, which is where the
//     engine would split (ErrIndexTooManyRows, the same error CreateIndex
//     raises for a table too large to index in the first place);
//   - a key that is not the 1-null-flag-byte-plus-int32 shape CreateIndex
//     builds, an index over more than one column, a DESC or NOCASE column, an
//     index enforcing a PRIMARY KEY or UNIQUE constraint, and a table whose
//     schema tail does not parse at all (all ErrIndexNotMaintained). See
//     maintainableIndexColumn for what each of those would get wrong;
//   - a table declaring a PRIMARY KEY or UNIQUE constraint, indexed or not
//     (ErrConstraintsNotEnforced). NOT NULL and MINVALUE/MAXVALUE are checked
//     now (writer_constraint.go); a key is not, because checking it without
//     being able to maintain the index that implements it would buy nothing.

// maintainedIndex is one user index this writer keeps in step with the records:
// which page its single leaf is, and which column it keys on.
//
// The covered column comes from the schema stream's index-definition array, not
// from the leaf. That is what makes maintenance possible at all: the B-tree
// itself does not record which column it covers, which is the reason Phase 7
// gave for leaving Update unguarded, and Phase 8's parseIndexRecord retired it.
type maintainedIndex struct {
	name       string
	rootPageNo int
	colIdx     int
}

// maintainedIndexes resolves the table's user indexes once per writer and
// caches the result, error included: a table this package will not index is
// refused the same way on every write, not just the first.
func (w *TableWriter) maintainedIndexes() ([]maintainedIndex, error) {
	if !w.indexesResolved {
		w.indexesResolved = true
		w.indexes, w.indexesErr = w.resolveIndexes()
	}

	return w.indexes, w.indexesErr
}

// resolveIndexes pairs every user index found on this table's pages with its
// definition in the table's schema stream, and refuses the write unless all of
// them are of the one shape this package maintains.
//
// The pairing is in that direction on purpose. Trusting the schema stream alone
// would miss an index whose record this package cannot parse; trusting the
// pages alone would leave the covered column unknown. Requiring every index the
// pages show to be named in the schema means an index this package cannot
// account for stops the write instead of being silently left stale.
func (w *TableWriter) resolveIndexes() ([]maintainedIndex, error) {
	// The schema tail is read first because the constraint array gates the
	// write whether or not the table has an index, while the index records
	// only matter if it has one. A tail that does not parse says nothing
	// either way, so it is carried and only raised below, where an index makes
	// it decisive -- refusing every unparsed tail here would newly refuse
	// writes to the unindexed private files that have always accepted them.
	records, constraints, tailErr := w.tableSchemaTail()
	if tailErr == nil {
		checks, err := newConstraintChecks(constraints, w.r.Schema(), w.r.table.Name())
		if err != nil {
			return nil, err
		}

		w.checks = checks
	}

	ir, err := w.r.table.OpenIndex()
	if err != nil {
		if errors.Is(err, ErrNoIndex) {
			return nil, nil
		}

		return nil, err
	}

	user := ir.UserIndexes()
	if len(user) == 0 {
		return nil, nil
	}

	if tailErr != nil {
		return nil, tailErr
	}

	byRoot := make(map[int]indexRecord, len(records))
	for _, rec := range records {
		byRoot[int(rec.rootPageNo)] = rec
	}

	indexes := make([]maintainedIndex, 0, len(user))

	for _, info := range user {
		idx, err := w.describeIndex(info, byRoot)
		if err != nil {
			return nil, err
		}

		indexes = append(indexes, idx)
	}

	return indexes, nil
}

// tableSchemaTail reads this table's index and constraint definitions out of
// its column-definition stream, mapping every failure to ErrIndexNotMaintained:
// a schema this package cannot read is a reason to refuse the write, not to
// fail it with an error about schemas.
func (w *TableWriter) tableSchemaTail() ([]indexRecord, []constraintRecord, error) {
	schemaPageNo, err := w.r.table.schemaPageNo()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrIndexNotMaintained, err)
	}

	raw, err := w.db.readSchemaStream(schemaPageNo)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrIndexNotMaintained, err)
	}

	// parseSchemaTail also returns the splice points CREATE INDEX and DROP
	// INDEX need; a write needs only the two record arrays.
	_, _, records, constraints, _, err := parseSchemaTail(raw) //nolint:dogsled // three of the six results belong to the splicing callers
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrIndexNotMaintained, err)
	}

	return records, constraints, nil
}

// maintainableIndexColumn returns the one column an index keys on, refusing
// every index shape whose leaf this package cannot reproduce.
//
// Reading the schema tail stopped being the gate here once constraint records
// were decoded, so these are the refusals that carry the weight now, and each
// one names a leaf this package would order differently than the engine did:
//
//   - a multi-column index concatenates its columns into one key;
//   - a DESC column sorts the other way, and compareInt32Keys does not;
//   - a NOCASE column compares case-folded, which no key this package builds
//     does;
//   - a UNIQUE or PRIMARY index rejects a duplicate key. A duplicate scan
//     would be easy enough, but no fixture shows the engine inserting into
//     such an index -- all four Writes-idx* files carry a plain one -- so the
//     leaf splice would be the only write in this package with no byte
//     identity behind it. The constraint and its index have to lift together;
//     see writer_constraint.go.
func maintainableIndexColumn(rec indexRecord) (indexColumn, error) {
	if rec.unique || rec.primary {
		return indexColumn{}, fmt.Errorf("%w: index %q enforces a PRIMARY KEY or UNIQUE constraint this package does not check",
			ErrIndexNotMaintained, rec.name)
	}

	col, ok := rec.singleColumn()
	if !ok {
		return indexColumn{}, fmt.Errorf("%w: %w: index %q covers %d columns",
			ErrIndexNotMaintained, ErrMultiColumnIndex, rec.name, len(rec.columns))
	}

	if col.descending || col.caseInsensitive {
		return indexColumn{}, fmt.Errorf("%w: index %q keys %q with descending=%t, case-insensitive=%t",
			ErrIndexNotMaintained, rec.name, col.name, col.descending, col.caseInsensitive)
	}

	return col, nil
}

// describeIndex checks one discovered index against its schema record and the
// column it keys on, and returns what maintenance needs. Every refusal here is
// a shape this package will not guess at, checked before any record is written
// so that a refused write leaves nothing behind.
func (w *TableWriter) describeIndex(info IndexInfo, byRoot map[int]indexRecord) (maintainedIndex, error) {
	rec, ok := byRoot[info.RootPageNo]
	if !ok {
		return maintainedIndex{}, fmt.Errorf("%w: an index rooted at page %d is not named in %q's schema",
			ErrIndexNotMaintained, info.RootPageNo, w.r.table.Name())
	}

	if info.KeySize != indexKeySize {
		return maintainedIndex{}, fmt.Errorf("%w: index %q has %d-byte keys, only %d-byte Int32 keys are maintained",
			ErrIndexNotMaintained, rec.name, info.KeySize, indexKeySize)
	}

	col, err := maintainableIndexColumn(rec)
	if err != nil {
		return maintainedIndex{}, err
	}

	schema := w.r.Schema()

	colIdx, err := findColumnIndex(schema, col.name)
	if err != nil {
		return maintainedIndex{}, fmt.Errorf("%w: index %q: %w", ErrIndexNotMaintained, rec.name, err)
	}

	if col := schema.Columns[colIdx]; col.BaseType != BftInt32 || col.FieldType != FieldInteger {
		return maintainedIndex{}, fmt.Errorf("%w: index %q keys %q, which is base type %d / field type %s",
			ErrIndexNotMaintained, rec.name, col.Name, col.BaseType, col.FieldType)
	}

	idx := maintainedIndex{name: rec.name, rootPageNo: info.RootPageNo, colIdx: colIdx}

	// Load the leaf now rather than at the first mutation, so that a tree this
	// package cannot edit is refused before a record has been written.
	if _, err := w.indexLeaf(idx); err != nil {
		return maintainedIndex{}, err
	}

	return idx, nil
}

// indexLeaf is one index's root page, buffered for modification and checked to
// be the single root-and-leaf page CreateIndex writes.
//
// EntryCount is read from the payload on demand rather than cached, because an
// update mutates the same leaf twice -- a removal then an insertion -- and the
// second has to see what the first left behind.
type indexLeaf struct {
	buf    *pageWriteBuf
	stride int
}

// indexLeaf buffers and validates the leaf page of one maintained index.
func (w *TableWriter) indexLeaf(idx maintainedIndex) (indexLeaf, error) {
	buf, err := w.loadPage(idx.rootPageNo)
	if err != nil {
		return indexLeaf{}, err
	}

	hdr, err := parseBTreeHeader(buf.payload)
	if err != nil {
		return indexLeaf{}, fmt.Errorf("%w: index %q root page %d: %w",
			ErrIndexNotMaintained, idx.name, idx.rootPageNo, err)
	}

	if !hdr.IsRoot || !hdr.IsLeaf {
		return indexLeaf{}, fmt.Errorf("%w: index %q is deeper than one page (root=%t leaf=%t)",
			ErrIndexNotMaintained, idx.name, hdr.IsRoot, hdr.IsLeaf)
	}

	if int(hdr.KeyPrefixSize) != indexKeySize {
		return indexLeaf{}, fmt.Errorf("%w: index %q leaf keys are %d bytes, want %d",
			ErrIndexNotMaintained, idx.name, hdr.KeyPrefixSize, indexKeySize)
	}

	leaf := indexLeaf{buf: buf, stride: indexKeySize + leafEntrySuffixSize}

	if end := leaf.end(); end > len(buf.payload) {
		return indexLeaf{}, fmt.Errorf("%w: index %q claims %d entries, which need %d bytes of a %d-byte page",
			ErrBookkeepingMismatch, idx.name, leaf.count(), end, len(buf.payload))
	}

	return leaf, nil
}

// count is the leaf's current entry count, read from the B-tree page header.
func (l indexLeaf) count() int {
	return int(binary.LittleEndian.Uint16(l.buf.payload[14:16]))
}

// end is the offset just past the last entry.
func (l indexLeaf) end() int {
	return btreeHeaderSize + l.count()*l.stride
}

// offset is where entry i starts.
func (l indexLeaf) offset(i int) int {
	return btreeHeaderSize + i*l.stride
}

// key returns entry i's key, aliasing the page buffer.
func (l indexLeaf) key(i int) []byte {
	off := l.offset(i)

	return l.buf.payload[off : off+indexKeySize]
}

// ref returns the row entry i points at.
func (l indexLeaf) ref(i int) RecordID {
	off := l.offset(i) + indexKeySize

	return RecordID{
		PageNo: int(int32(binary.LittleEndian.Uint32(l.buf.payload[off : off+4]))),
		Slot:   int(binary.LittleEndian.Uint16(l.buf.payload[off+4 : off+6])),
	}
}

// setCount writes the leaf's entry count back and marks the page modified.
func (l indexLeaf) setCount(n int) {
	binary.LittleEndian.PutUint16(l.buf.payload[14:16], uint16(n)) //nolint:gosec // bounded by room, checked before every insert

	l.buf.dirty = true
}

// room reports whether one more entry fits, which is the boundary at which the
// engine would split the leaf into a deeper tree. No fixture captures a split,
// so this refuses instead.
func (l indexLeaf) room() error {
	if l.end()+l.stride > len(l.buf.payload) {
		return fmt.Errorf("%w: index leaf page %d already holds %d entries",
			ErrIndexTooManyRows, l.buf.number, l.count())
	}

	return nil
}

// insert splices one entry into the leaf at its sorted position, shifting
// everything at or after that position up by one stride.
//
// A key equal to one already stored is placed after the whole run of equals,
// which is the position an append would give it. No fixture has duplicate keys
// -- every index in the corpus is over a unique column -- so this is the
// convention, not a measured fact.
func (l indexLeaf) insert(key []byte, id RecordID) error {
	if err := l.room(); err != nil {
		return err
	}

	if id.PageNo < 0 || id.PageNo > math.MaxInt32 {
		return fmt.Errorf("%w: page number %d", ErrBookkeepingMismatch, id.PageNo)
	}

	if id.Slot < 0 || id.Slot > math.MaxUint16 {
		return fmt.Errorf("%w: slot number %d", ErrBookkeepingMismatch, id.Slot)
	}

	count := l.count()

	pos := count

	for i := range count {
		if compareInt32Keys(l.key(i), key) > 0 {
			pos = i

			break
		}
	}

	off, end := l.offset(pos), l.end()

	// copy is a memmove, so the overlap with the shifted region is safe.
	copy(l.buf.payload[off+l.stride:end+l.stride], l.buf.payload[off:end])

	copy(l.buf.payload[off:off+indexKeySize], key)
	binary.LittleEndian.PutUint32(l.buf.payload[off+indexKeySize:off+indexKeySize+4], uint32(id.PageNo))
	binary.LittleEndian.PutUint16(l.buf.payload[off+indexKeySize+4:off+indexKeySize+6], uint16(id.Slot))

	l.setCount(count + 1)

	return nil
}

// remove splices out the entry pointing at id, shifting the entries behind it
// down by one stride.
//
// The stride the shift frees at the end is deliberately left as it was.
// Writes-idx-del.abs is the evidence: the engine drops EntryCount and shifts,
// and the vacated bytes still hold the entry that used to be last. Clearing
// them would read back identically through this package and differ from the
// engine's file, which is exactly the failure the byte-identity tests exist to
// catch.
func (l indexLeaf) remove(id RecordID) error {
	count := l.count()

	pos := -1

	for i := range count {
		if l.ref(i) == id {
			pos = i

			break
		}
	}

	if pos < 0 {
		return fmt.Errorf("%w: index leaf page %d has no entry for page %d slot %d",
			ErrBookkeepingMismatch, l.buf.number, id.PageNo, id.Slot)
	}

	off, end := l.offset(pos), l.end()

	copy(l.buf.payload[off:end-l.stride], l.buf.payload[off+l.stride:end])

	l.setCount(count - 1)

	return nil
}

// indexKeyFor builds the leaf key for one record's indexed column, in the same
// [null flag byte][int32 little-endian] shape buildIndexLeafEntries writes when
// CREATE INDEX builds the whole leaf. Sharing that shape is what lets a
// maintained leaf be compared against a rebuilt one.
func (w *TableWriter) indexKeyFor(id RecordID, colIdx int) ([]byte, error) {
	rec, err := w.Record(id)
	if err != nil {
		return nil, err
	}

	key := make([]byte, indexKeySize)

	if rec.IsNull(colIdx) {
		key[0] = 1
	} else {
		binary.LittleEndian.PutUint32(key[1:], uint32(rec.Int(colIdx)))
	}

	return key, nil
}

// indexRoom refuses an insert no index leaf has room for, before the record
// itself is written. Checking first is what keeps a refused insert from
// leaving a row behind that no index describes.
func (w *TableWriter) indexRoom(indexes []maintainedIndex) error {
	for _, idx := range indexes {
		leaf, err := w.indexLeaf(idx)
		if err != nil {
			return err
		}

		if err := leaf.room(); err != nil {
			return err
		}
	}

	return nil
}

// indexInsert adds the record at id to every maintained index.
func (w *TableWriter) indexInsert(indexes []maintainedIndex, id RecordID) error {
	for _, idx := range indexes {
		key, err := w.indexKeyFor(id, idx.colIdx)
		if err != nil {
			return err
		}

		leaf, err := w.indexLeaf(idx)
		if err != nil {
			return err
		}

		if err := leaf.insert(key, id); err != nil {
			return err
		}
	}

	return nil
}

// indexRemove drops the record at id from every maintained index.
func (w *TableWriter) indexRemove(indexes []maintainedIndex, id RecordID) error {
	for _, idx := range indexes {
		leaf, err := w.indexLeaf(idx)
		if err != nil {
			return err
		}

		if err := leaf.remove(id); err != nil {
			return err
		}
	}

	return nil
}

// storeRecordReindexing overwrites a record and brings the table's indexes
// forward with it: the keys are read before the write and again after, and only
// an index whose key actually moved is touched.
//
// It is the shared body of Update and UpdateColumn, and the only place either
// of them writes a record. storeRecord itself stays index-blind: it takes a
// buffer and an offset, not a RecordID, so it has nothing to write an index
// entry with. Keeping the split means an index entry is only ever moved from a
// caller that knows which row it is moving.
func (w *TableWriter) storeRecordReindexing(id RecordID, buf *pageWriteBuf, start int, rec []byte) error {
	err := w.validateStore(buf, start, rec)
	if err != nil {
		return err
	}

	indexes, err := w.maintainedIndexes()
	if err != nil {
		return err
	}

	err = w.checkConstraints(rec)
	if err != nil {
		return err
	}

	before, err := w.indexKeys(indexes, id)
	if err != nil {
		return err
	}

	err = w.storeRecord(buf, start, rec)
	if err != nil {
		return err
	}

	return w.indexReplace(indexes, id, before)
}

// indexKeys captures the key each maintained index currently holds for the
// record at id, so that an update can tell which of them it moved.
func (w *TableWriter) indexKeys(indexes []maintainedIndex, id RecordID) ([][]byte, error) {
	if len(indexes) == 0 {
		return nil, nil
	}

	keys := make([][]byte, len(indexes))

	for i, idx := range indexes {
		key, err := w.indexKeyFor(id, idx.colIdx)
		if err != nil {
			return nil, err
		}

		keys[i] = key
	}

	return keys, nil
}

// indexReplace brings every maintained index forward after the record at id was
// overwritten, given the keys it held beforehand.
//
// An index whose key did not move is left untouched, which is what keeps an
// ordinary update byte-identical to the engine's: the engine writes the index
// page only when the key actually changed, and writing it anyway would advance
// its State counter for a page whose contents did not change.
func (w *TableWriter) indexReplace(indexes []maintainedIndex, id RecordID, before [][]byte) error {
	for i, idx := range indexes {
		after, err := w.indexKeyFor(id, idx.colIdx)
		if err != nil {
			return err
		}

		if bytes.Equal(before[i], after) {
			continue
		}

		leaf, err := w.indexLeaf(idx)
		if err != nil {
			return err
		}

		if err := leaf.remove(id); err != nil {
			return err
		}

		if err := leaf.insert(after, id); err != nil {
			return err
		}
	}

	return nil
}
