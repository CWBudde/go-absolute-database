package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
)

// Write support: record insert, update and delete within existing data pages.
//
// The record layout Reader derives (see its doc comment) is what makes writing
// tractable: a data page is an occupancy bitmap followed by fixed-size record
// slots, so an update overwrites a slot in place, a delete clears one bitmap
// bit, and an insert sets a clear bit and fills the slot behind it. None of the
// three moves any other record, and the free list is the bitmap itself.
//
// All changes are buffered in memory until Commit. Rollback is therefore exact
// rather than compensating: nothing has been written yet, so discarding the
// buffers restores the on-disk file. Commit itself is NOT crash-atomic — it
// writes the modified pages and fsyncs, and a crash midway leaves some of them
// written. The engine's own journalling is not reproduced here.

var (
	// ErrReadOnly is returned by every write operation on a database that was
	// opened with Open rather than OpenForWrite.
	ErrReadOnly = errors.New("absdb: database is open read-only")

	// ErrWriterClosed is returned by a TableWriter that has already been
	// committed, rolled back or closed.
	ErrWriterClosed = errors.New("absdb: writer is closed")

	// ErrNoRecord reports a RecordID that does not address an occupied slot of
	// a data page of this table.
	ErrNoRecord = errors.New("absdb: no such record")

	// ErrSlotOccupied reports an insert into a slot that already holds a record.
	ErrSlotOccupied = errors.New("absdb: record slot is occupied")

	// ErrTableFull reports that every slot of every existing data page is
	// occupied. Growing the table by allocating a further data page is not
	// implemented yet, so this is a hard limit rather than a transient state.
	ErrTableFull = errors.New("absdb: no free record slot in any data page")

	// ErrIndexNotMaintained reports a write that would leave one of the table's
	// indexes out of step with its records. Inserting or deleting a record
	// changes the set of keys an index must hold, and this package cannot
	// update a B-tree yet, so it refuses the write instead of silently leaving
	// a lookup structure that no longer describes the data.
	ErrIndexNotMaintained = errors.New("absdb: table has an index this package cannot maintain")

	// ErrBlobReferenceLost reports an update that would overwrite a column
	// still holding a BLOB reference. The BLOB pages it points at would stay
	// allocated with nothing referring to them, and this package cannot free
	// them yet.
	ErrBlobReferenceLost = errors.New("absdb: update would drop a BLOB reference")

	// ErrBookkeepingMismatch reports that the engine's own record counters do
	// not agree with the records actually stored, so a write cannot bring them
	// forward without guessing.
	ErrBookkeepingMismatch = errors.New("absdb: stored record counts do not match the records on the page")
)

// The bookkeeping an insert or a delete has to keep in step, established by
// diffing vendor-produced files that differ by exactly one SQL statement (see
// PLAN.md, Phase 7). Besides the record and its occupancy bit, the engine
// updates, on every write:
//
//   - the record count of the affected data page, held in its entry of the
//     internal record-page index;
//   - the table's record count and a separate change counter, both in the
//     table info page;
//   - the State counter in the ABSP header of every page it writes, +1 each;
//   - the State field of the database header, +1 per committed transaction.
//
// Reproducing all four is what makes a write byte-identical to the engine's.
const (
	// recordPageEntrySize is the on-disk size of one entry of the internal
	// record-page index: a 4-byte data page number and a 2-byte record count.
	// It is deliberately not entryStride: that index holds a count per page,
	// not the 6-byte record reference a user index leaf holds.
	recordPageEntrySize = systemKeySize + 2

	// tableInfoTrailerSize is the size of the two counters at the end of a
	// table info internal file, which is where they live. The structure is
	//
	//	int32 ColumnCount, ColumnCount * 8 bytes, int32 Changes, int32 Records
	//
	// so their position depends on how many columns the table has. Every
	// fixture obeys that shape, across format versions 5.13, 7.61 and 7.94.
	//
	// The first version of this code used fixed offsets 46 and 50, which are
	// the right answer for a four-column table and only for a four-column
	// table. Every write fixture has four columns, so the byte-identity tests
	// could not see it; MultiTable.abs, whose tables have two and three, is
	// what exposed it. It also retires a conclusion drawn from the same bug —
	// that 7.61 files leave their record count at zero. They do not: read at
	// the right offset, RCON0011 says 300 and RCFQ0011 says 600, matching
	// their rows exactly.
	tableInfoTrailerSize = 8

	// tableInfoChangesFromEnd and tableInfoCountFromEnd are the two counters'
	// offsets back from the end of the structure. The changes counter advances
	// on every write to the table, an update included; the record count is
	// raised by an insert, lowered by a delete and left alone by an update.
	tableInfoChangesFromEnd = 8
	tableInfoCountFromEnd   = 4

	// pageStateOffset is the offset of the State counter within a page block:
	// four bytes behind the ABSP marker.
	pageStateOffset = diskPageHeaderOffset + 4

	// pageCRCOffset is the offset of the payload checksum within a page block.
	pageCRCOffset = diskPageHeaderOffset + 14

	// fileStateOffset is the offset of State in the database header.
	fileStateOffset = 38
)

// OpenForWrite opens an Absolute Database file for reading and writing.
//
// Nothing is written until a TableWriter is committed. Opening for write on its
// own does not modify the file.
func OpenForWrite(path string) (*File, error) {
	return openDB(path, os.O_RDWR)
}

// OpenForWriteWithPassword opens an encrypted file for reading and writing.
// It reports the same errors as OpenWithPassword.
func OpenForWriteWithPassword(path, password string) (*File, error) {
	db, err := OpenForWrite(path)
	if err != nil {
		return nil, err
	}

	err = db.Unlock(password)
	if err != nil {
		db.Close()

		return nil, err
	}

	return db, nil
}

// Writable reports whether the file was opened for writing.
func (db *File) Writable() bool {
	return db.writable
}

// RecordID addresses a single record slot: the data page it lives on and its
// slot index within that page. It is stable as long as the record is not
// deleted, because no write operation moves a record between slots.
type RecordID struct {
	PageNo int
	Slot   int
}

// RecordID returns the identity of the record the Reader is currently
// positioned on, for handing to a TableWriter. The second result is false
// before the first Next and after Next has reported false.
//
// It lives here rather than in reader.go because addressing a record only
// matters when something is going to write to it.
func (r *Reader) RecordID() (RecordID, bool) {
	if !r.started || r.pageIdx >= len(r.dataPages) {
		return RecordID{}, false
	}

	if _, ok := r.recordStart(r.recordIdx); !ok {
		return RecordID{}, false
	}

	return RecordID{PageNo: r.dataPages[r.pageIdx], Slot: r.recordIdx}, true
}

// pageWriteBuf is a page held in memory for modification: the whole block the
// page starts in plus the leading diskPageHeaderOffset bytes of the following
// block, which together are exactly what ReadPage reads. payload points into
// raw, so mutating a record mutates the buffer that will be written back.
type pageWriteBuf struct {
	number int
	raw    []byte
	// payload is the page's usable data area, in the clear even for an
	// encrypted page: ReadPage has already decrypted it.
	payload []byte
	// encrypted records whether this page's payload is stored encrypted, taken
	// from its ABSP checksum being non-zero. A page written back must be put
	// back into the state it was found in, not into whatever the file header
	// says: an encrypted database still holds a few pages in the clear.
	encrypted bool
	// dirty marks a page that was actually modified. Pages are buffered for
	// reading too, and a page that was only read must not be rewritten — doing
	// so would advance its State counter for a transaction that changed
	// nothing.
	dirty bool
	// stateBump is how far this page's State counter advances when the page is
	// written. It is 1 for an ordinary page, because the engine counts writes.
	// The allocation maps count the bits they record instead, and a page that
	// has just been freed carries pageStateFree as a marker rather than a
	// counter and must not be advanced at all, so both set this themselves.
	stateBump int
}

// TableWriter modifies the records of a table. Changes are buffered until
// Commit; see the file comment for what Commit does and does not guarantee.
//
// A TableWriter holds its own view of the pages it touches, so a Reader opened
// on the same File keeps seeing the committed state until the writer commits.
type TableWriter struct {
	db    *File
	r     *Reader
	pages map[int]*pageWriteBuf
	order []int // page numbers in first-touch order, so commits are deterministic
	// delta counts records added (positive) or removed (negative) per data
	// page, so that Commit can bring the engine's counters back in step.
	delta map[int]int
	// touched counts records modified in any way — inserted, updated or
	// deleted. The engine's change counter advances by the number of records a
	// statement affected, not by one per transaction: a two-row UPDATE moves it
	// by two while the State counters still move by one.
	touched int
	closed  bool
}

// OpenTableWriter opens the database's only table for modification. It fails
// with ErrReadOnly unless the file was opened with OpenForWrite, and reports
// ErrAmbiguousTable when the file holds more than one table.
func (db *File) OpenTableWriter() (*TableWriter, error) {
	t, err := db.Table("")
	if err != nil {
		return nil, err
	}

	return t.OpenWriter()
}

// OpenWriter opens this table for modification. It fails with ErrReadOnly
// unless the file was opened with OpenForWrite.
func (t *Table) OpenWriter() (*TableWriter, error) {
	if !t.db.writable {
		return nil, ErrReadOnly
	}

	r, err := t.Open()
	if err != nil {
		return nil, err
	}

	return &TableWriter{
		db:    t.db,
		r:     r,
		pages: make(map[int]*pageWriteBuf),
		delta: make(map[int]int),
	}, nil
}

// Schema returns the schema of the table being written.
func (w *TableWriter) Schema() *TableSchema {
	return w.r.Schema()
}

// Update overwrites the record at id with values, one per column. A nil value
// writes a NULL. The record must exist; Update never creates one.
//
// Unlike Insert and Delete, Update does not refuse a table that has an index:
// an update changes no key as long as it leaves the indexed columns alone, and
// the index format does not record which columns it covers, so this package
// cannot tell the two cases apart. Changing an indexed column through Update
// leaves that index pointing at the old key. Until index maintenance exists,
// avoiding that is the caller's responsibility.
func (w *TableWriter) Update(id RecordID, values []any) error {
	rec, err := w.r.encodeRecord(values)
	if err != nil {
		return err
	}

	buf, start, err := w.slot(id, true)
	if err != nil {
		return err
	}

	return w.storeRecord(buf, start, rec)
}

// UpdateColumn overwrites a single column of an existing record, leaving every
// other column byte-for-byte as it was. It is the narrowest write this package
// offers, and the only one that cannot disturb a column it was not asked about.
//
// It carries the same index caveat as Update.
func (w *TableWriter) UpdateColumn(id RecordID, col int, value any) error {
	buf, start, err := w.slot(id, true)
	if err != nil {
		return err
	}

	rec := make([]byte, w.r.recordSize)
	copy(rec, buf.payload[start:start+w.r.recordSize])

	err = w.r.encodeInto(rec, col, value)
	if err != nil {
		return err
	}

	return w.storeRecord(buf, start, rec)
}

// Insert stores a new record in the first free slot of the first data page that
// has one, and returns the slot it used. It reports ErrTableFull when every
// slot of every data page is occupied.
func (w *TableWriter) Insert(values []any) (RecordID, error) {
	rec, err := w.r.encodeRecord(values)
	if err != nil {
		return RecordID{}, err
	}

	err = w.checkIndexes()
	if err != nil {
		return RecordID{}, err
	}

	id, err := w.freeSlot()
	if err != nil {
		return RecordID{}, err
	}

	buf, start, err := w.slot(id, false)
	if err != nil {
		return RecordID{}, err
	}

	if bitSet(buf.payload, id.Slot, w.r.bitmapBytes) {
		return RecordID{}, fmt.Errorf("%w: page %d slot %d", ErrSlotOccupied, id.PageNo, id.Slot)
	}

	copy(buf.payload[start:start+w.r.recordSize], rec)
	setBit(buf.payload, id.Slot, w.r.bitmapBytes)

	buf.dirty = true
	w.delta[id.PageNo]++
	w.touched++

	return id, nil
}

// Delete removes the record at id by clearing its occupancy bit. The record's
// bytes are left in place, which is what the engine itself does: the slot is
// free, and the next insert overwrites as much of it as the new record needs.
func (w *TableWriter) Delete(id RecordID) error {
	err := w.checkIndexes()
	if err != nil {
		return err
	}

	buf, _, err := w.slot(id, true)
	if err != nil {
		return err
	}

	clearBit(buf.payload, id.Slot, w.r.bitmapBytes)

	buf.dirty = true
	w.delta[id.PageNo]--
	w.touched++

	return nil
}

// Record returns the current bytes of a record as this writer sees them,
// including changes buffered but not yet committed.
func (w *TableWriter) Record(id RecordID) (Record, error) {
	buf, start, err := w.slot(id, true)
	if err != nil {
		return Record{}, err
	}

	fieldStart := start + w.r.nullFlagBytes

	return Record{
		reader:    w.r,
		nullFlags: buf.payload[start:fieldStart],
		fieldData: buf.payload[fieldStart : fieldStart+w.r.fieldDataSize],
	}, nil
}

// Commit writes every modified page back and flushes the file. The writer is
// closed afterwards, whether or not the write succeeded: a failed commit may
// have written some pages already, so continuing to use the buffers would build
// on a state that is no longer known.
func (w *TableWriter) Commit() error {
	if w.closed {
		return ErrWriterClosed
	}

	w.closed = true

	if !w.modified() {
		return nil
	}

	err := w.updateTableInfo()
	if err != nil {
		return err
	}

	// The order slice may have grown while the counters were updated. Every
	// page that is written gets its State counter advanced, exactly as the
	// engine does.
	err = w.db.flushPages(w.order, w.pages)
	if err != nil {
		return err
	}

	err = w.db.bumpFileState()
	if err != nil {
		return err
	}

	err = w.db.f.Sync()
	if err != nil {
		return fmt.Errorf("absdb: flushing writes: %w", err)
	}

	return nil
}

// Rollback discards every buffered change. Because nothing is written before
// Commit, the file is left exactly as it was.
func (w *TableWriter) Rollback() {
	w.closed = true
	w.pages = nil
	w.order = nil
	w.delta = nil
}

// Close rolls back any uncommitted changes. It is safe to call after Commit,
// which makes `defer w.Close()` the correct idiom next to an explicit Commit.
func (w *TableWriter) Close() error {
	if !w.closed {
		w.Rollback()
	}

	return nil
}

// modified reports whether anything was actually changed.
func (w *TableWriter) modified() bool {
	for _, buf := range w.pages {
		if buf.dirty {
			return true
		}
	}

	return false
}

// storeRecord writes an encoded record over an existing one, after checking
// that doing so does not strand a BLOB.
func (w *TableWriter) storeRecord(buf *pageWriteBuf, start int, rec []byte) error {
	if len(rec) != w.r.recordSize {
		return fmt.Errorf("%w: record is %d bytes, want %d", ErrRecordSize, len(rec), w.r.recordSize)
	}

	old := buf.payload[start : start+w.r.recordSize]

	err := w.checkBlobReferences(old, rec)
	if err != nil {
		return err
	}

	copy(old, rec)

	buf.dirty = true
	w.touched++

	return nil
}

// checkBlobReferences refuses an update that would overwrite a live BLOB
// reference. That reference is the only thing pointing at the BLOB's pages, and
// nothing here can free them, so losing it would leak them silently.
func (w *TableWriter) checkBlobReferences(old, rec []byte) error {
	for i, c := range w.r.schema.Columns {
		if !c.IsBLOB() {
			continue
		}

		from := w.r.nullFlagBytes + w.r.fieldOffsets[i]
		to := from + w.r.fieldStoreSizes[i]

		if slices.Equal(old[from:to], rec[from:to]) || isZero(old[from:to]) {
			continue
		}

		return fmt.Errorf("%w: column %d (%s)", ErrBlobReferenceLost, i, c.Name)
	}

	return nil
}

// slot resolves a RecordID to its page buffer and the payload offset of its
// record. With mustExist set it also insists the slot is currently occupied,
// which is what every operation but an insert wants.
func (w *TableWriter) slot(id RecordID, mustExist bool) (*pageWriteBuf, int, error) {
	if w.closed {
		return nil, 0, ErrWriterClosed
	}

	if id.Slot < 0 || id.Slot >= w.r.recordsPerPage {
		return nil, 0, fmt.Errorf("%w: page %d slot %d", ErrNoRecord, id.PageNo, id.Slot)
	}

	buf, err := w.dataPage(id.PageNo)
	if err != nil {
		return nil, 0, err
	}

	start := w.r.bitmapBytes + id.Slot*w.r.recordSize
	if start+w.r.recordSize > len(buf.payload) {
		return nil, 0, fmt.Errorf("%w: page %d slot %d", ErrNoRecord, id.PageNo, id.Slot)
	}

	if mustExist && !bitSet(buf.payload, id.Slot, w.r.bitmapBytes) {
		return nil, 0, fmt.Errorf("%w: page %d slot %d", ErrNoRecord, id.PageNo, id.Slot)
	}

	return buf, start, nil
}

// dataPage returns the buffered copy of a data page. Only pages that belong to
// this table are accepted, so a stray page number cannot be written through a
// table writer.
func (w *TableWriter) dataPage(no int) (*pageWriteBuf, error) {
	if !w.isDataPage(no) {
		return nil, fmt.Errorf("%w: page %d is not a data page of this table", ErrNoRecord, no)
	}

	return w.loadPage(no)
}

// loadPage buffers any page of the file, data page or not, reading it on first
// use. The bookkeeping pages a write has to touch are not data pages, which is
// why this exists alongside dataPage.
func (w *TableWriter) loadPage(no int) (*pageWriteBuf, error) {
	if buf, ok := w.pages[no]; ok {
		return buf, nil
	}

	buf, err := w.db.bufferPage(no)
	if err != nil {
		return nil, err
	}

	w.pages[no] = buf
	w.order = append(w.order, no)

	return buf, nil
}

// bufferPage reads one page into a buffer that can be modified and written
// back. It is the shared half of buffering: TableWriter and the schema
// operations in ddl.go keep their own page sets but fill them the same way.
func (db *File) bufferPage(no int) (*pageWriteBuf, error) {
	page, err := db.ReadPage(no)
	if err != nil {
		return nil, err
	}

	if page.Header == nil {
		return nil, fmt.Errorf("absdb: page %d has no ABSP header", no)
	}

	return &pageWriteBuf{
		number:    no,
		raw:       page.raw,
		payload:   page.Payload,
		encrypted: page.Header.CRC32 != 0,
		stateBump: 1,
	}, nil
}

func (w *TableWriter) isDataPage(no int) bool {
	return slices.Contains(w.r.dataPages, no)
}

// freeSlot finds the first unoccupied slot, scanning data pages in file order
// and slots in bitmap order — the same order the engine fills them, which is
// what lets an insert reproduce the engine's own output byte for byte.
func (w *TableWriter) freeSlot() (RecordID, error) {
	for _, no := range w.r.dataPages {
		buf, err := w.dataPage(no)
		if err != nil {
			return RecordID{}, err
		}

		for slot := range w.r.recordsPerPage {
			if bitSet(buf.payload, slot, w.r.bitmapBytes) {
				continue
			}

			if w.r.bitmapBytes+(slot+1)*w.r.recordSize <= len(buf.payload) {
				return RecordID{PageNo: no, Slot: slot}, nil
			}
		}
	}

	return RecordID{}, ErrTableFull
}

// checkIndexes refuses writes that would change the set of keys an index has to
// hold. An update leaves that set alone as long as it does not change an
// indexed column, which is why Update and UpdateColumn do not call this.
func (w *TableWriter) checkIndexes() error {
	ir, err := w.r.table.OpenIndex()
	if err != nil {
		if errors.Is(err, ErrNoIndex) {
			return nil
		}

		return err
	}

	if len(ir.UserIndexes()) > 0 {
		return ErrIndexNotMaintained
	}

	return nil
}

// updateTableInfo brings the engine's own counters back in step with what this
// writer changed: the per-page record counts in the internal record-page index,
// and the record count and change counter in the table info page.
//
// The counters are only touched when they currently agree with what is actually
// stored. That is not defensiveness for its own sake: the record count is
// maintained in the 7.94 files but left at zero in the 7.61 ones, so a writer
// that updated it unconditionally would invent a count for files whose engine
// never kept one.
func (w *TableWriter) updateTableInfo() error {
	total, err := w.updatePageCounts()
	if err != nil {
		return err
	}

	return w.updateCounters(total)
}

// updatePageCounts rewrites the per-page record counts of the internal
// record-page index, and returns the table's record count as that index
// described it before the change.
func (w *TableWriter) updatePageCounts() (int, error) {
	pageNo, counts, err := w.recordPageIndex()
	if err != nil {
		return 0, err
	}

	total := 0
	for _, count := range counts {
		total += count
	}

	if len(w.delta) == 0 {
		return total, nil
	}

	buf, err := w.loadPage(pageNo)
	if err != nil {
		return 0, err
	}

	for i := range int(binary.LittleEndian.Uint16(buf.payload[14:16])) {
		off := btreeHeaderSize + i*recordPageEntrySize

		entry := int(int32(binary.LittleEndian.Uint32(buf.payload[off : off+4])))
		if w.delta[entry] == 0 {
			continue
		}

		occupied, err := w.occupiedSlots(entry)
		if err != nil {
			return 0, err
		}

		stored := int(binary.LittleEndian.Uint16(buf.payload[off+4 : off+6]))
		if stored != occupied-w.delta[entry] {
			return 0, fmt.Errorf("%w: index says page %d holds %d records, it held %d",
				ErrBookkeepingMismatch, entry, stored, occupied-w.delta[entry])
		}

		if occupied < 0 || occupied > math.MaxUint16 {
			return 0, fmt.Errorf("%w: page %d holds %d records, more than the count field can hold",
				ErrBookkeepingMismatch, entry, occupied)
		}

		binary.LittleEndian.PutUint16(buf.payload[off+4:off+6], uint16(occupied))

		buf.dirty = true
	}

	return total, nil
}

// updateCounters advances the two counters in the table info page: the record
// count, which only an insert or a delete moves, and the change counter, which
// advances by the number of records this transaction touched.
func (w *TableWriter) updateCounters(before int) error {
	no, err := w.r.table.infoPageNo()
	if err != nil || no < 0 {
		return err
	}

	buf, err := w.loadPage(no)
	if err != nil {
		return err
	}

	changeOff, countOff, err := tableInfoOffsets(buf.payload)
	if err != nil {
		return err
	}

	count := int(int32(binary.LittleEndian.Uint32(buf.payload[countOff : countOff+4])))
	if count != before {
		// This file's engine does not maintain the counter. Leave it as it is
		// rather than inventing a value for it.
		return nil
	}

	added := 0
	for _, delta := range w.delta {
		added += delta
	}

	total := before + added
	if total < 0 || total > math.MaxInt32 {
		return fmt.Errorf("%w: table would hold %d records", ErrBookkeepingMismatch, total)
	}

	binary.LittleEndian.PutUint32(
		buf.payload[countOff:countOff+4], uint32(total),
	)

	if w.touched < 0 || w.touched > math.MaxInt32 {
		return fmt.Errorf("%w: %d records touched", ErrBookkeepingMismatch, w.touched)
	}

	changes := binary.LittleEndian.Uint32(buf.payload[changeOff : changeOff+4])
	binary.LittleEndian.PutUint32(
		buf.payload[changeOff:changeOff+4], changes+uint32(w.touched),
	)

	buf.dirty = true

	return nil
}

// tableInfoOffsets locates the two counters inside a table info page's payload.
//
// They sit at the end of the internal file rather than at a fixed offset,
// because the structure in front of them is eight bytes per column, so their
// position depends on the table's width. The internal file header's own length
// field is what says where the end is.
func tableInfoOffsets(payload []byte) (changeOff, countOff int, err error) {
	if len(payload) < internalFileHeaderSize {
		return 0, 0, fmt.Errorf("%w: table info page is %d bytes", ErrBookkeepingMismatch, len(payload))
	}

	hdrSize := int(payload[0])
	stored := int(binary.LittleEndian.Uint32(payload[1:5]))

	if hdrSize < internalFileHeaderSize || stored < tableInfoTrailerSize {
		return 0, 0, fmt.Errorf("%w: table info file declares %d bytes behind a %d-byte header",
			ErrBookkeepingMismatch, stored, hdrSize)
	}

	end := hdrSize + stored
	if end > len(payload) {
		return 0, 0, fmt.Errorf("%w: table info file ends at %d, past the %d-byte payload",
			ErrBookkeepingMismatch, end, len(payload))
	}

	return end - tableInfoChangesFromEnd, end - tableInfoCountFromEnd, nil
}

// recordPageIndex locates the engine's internal index over this table's data
// pages and returns its page number together with its entries, keyed by data
// page number.
//
// It is identified by what its keys are rather than by its position: a system
// index (systemKeySize keys) whose keys are exactly this table's data pages. A
// file can hold a second system index over its BLOB pages, which this must not
// pick.
func (w *TableWriter) recordPageIndex() (int, map[int]int, error) {
	ir, err := w.r.table.OpenIndex()
	if err != nil {
		return 0, nil, err
	}

	for _, idx := range ir.Indexes() {
		if !idx.IsInternal {
			continue
		}

		entries, ok, err := w.readRecordPageEntries(idx.RootPageNo)
		if err != nil {
			return 0, nil, err
		}

		if ok {
			return idx.RootPageNo, entries, nil
		}
	}

	return 0, nil, fmt.Errorf("%w: no record-page index to update", ErrIndexNotMaintained)
}

// readRecordPageEntries reads one system index page as a record-page index. The
// second result is false when the page's keys are not this table's data pages,
// which is how the BLOB page index is told apart from the record page index.
func (w *TableWriter) readRecordPageEntries(pageNo int) (map[int]int, bool, error) {
	page, err := w.db.ReadPage(pageNo)
	if err != nil {
		return nil, false, err
	}

	data := page.PageData()

	hdr, err := parseBTreeHeader(data)
	if err != nil {
		return nil, false, err
	}

	if !hdr.IsLeaf || int(hdr.KeyPrefixSize) != systemKeySize {
		return nil, false, nil
	}

	entries := make(map[int]int, hdr.EntryCount)

	for i := range int(hdr.EntryCount) {
		off := btreeHeaderSize + i*recordPageEntrySize
		if off+recordPageEntrySize > len(data) {
			return nil, false, nil
		}

		key := int(int32(binary.LittleEndian.Uint32(data[off : off+4])))
		if !w.isDataPage(key) {
			return nil, false, nil
		}

		entries[key] = int(binary.LittleEndian.Uint16(data[off+4 : off+6]))
	}

	if len(entries) != len(w.r.dataPages) {
		return nil, false, nil
	}

	return entries, true, nil
}

// occupiedSlots counts the records currently on a data page, as this writer
// sees it: buffered changes included.
func (w *TableWriter) occupiedSlots(pageNo int) (int, error) {
	buf, err := w.dataPage(pageNo)
	if err != nil {
		return 0, err
	}

	count := 0

	for slot := range w.r.recordsPerPage {
		if bitSet(buf.payload, slot, w.r.bitmapBytes) {
			count++
		}
	}

	return count, nil
}

// writePageBuf writes a modified page back to disk.
//
// It writes exactly pageSize bytes starting at the page's ABSP header, which is
// the smallest contiguous run covering everything that can have changed: the
// header holds the payload checksum, and the payload runs from just behind the
// header into the leading diskPageHeaderOffset bytes of the following block.
// The bytes before the header belong to the previous page and are not touched.
func (db *File) writePageBuf(p *pageWriteBuf) error {
	if !db.writable {
		return ErrReadOnly
	}

	if p.encrypted {
		crc := absCRC32(p.payload)

		// ReadPage treats a zero ABSP checksum as "this page is in the clear",
		// so a page whose new payload happens to check to zero would be written
		// encrypted and read back as ciphertext. The format has no spare bit to
		// tell the two apart, so refuse the write rather than produce a file
		// that cannot be read back. One payload in 2^32.
		if crc == 0 {
			return fmt.Errorf("absdb: page %d: payload checksum is zero, which would read back as unencrypted", p.number)
		}

		binary.LittleEndian.PutUint32(p.raw[pageCRCOffset:pageCRCOffset+4], crc)

		err := db.encryptPayload(p.payload)
		if err != nil {
			return fmt.Errorf("absdb: encrypting page %d: %w", p.number, err)
		}
	}

	offset := int64(p.number)*int64(db.pageSize) + diskPageHeaderOffset

	_, err := db.f.WriteAt(p.raw[diskPageHeaderOffset:diskPageHeaderOffset+int(db.pageSize)], offset)
	if err != nil {
		return fmt.Errorf("absdb: writing page %d: %w", p.number, err)
	}

	return nil
}

// bumpFileState advances the database header's State counter, which the engine
// increments once per committed transaction.
func (db *File) bumpFileState() error {
	db.state++

	var buf [4]byte

	binary.LittleEndian.PutUint32(buf[:], uint32(db.state))

	_, err := db.f.WriteAt(buf[:], fileStateOffset)
	if err != nil {
		return fmt.Errorf("absdb: writing database state: %w", err)
	}

	return nil
}

// flushPages writes back every dirty page of a page set, in the order the set
// recorded, advancing each one's State counter as the engine does.
func (db *File) flushPages(order []int, pages map[int]*pageWriteBuf) error {
	for _, no := range order {
		buf := pages[no]
		if !buf.dirty {
			continue
		}

		bumpPageState(buf)

		err := db.writePageBuf(buf)
		if err != nil {
			return err
		}
	}

	return nil
}

// bumpPageState advances a page's State counter by the number of writes it
// stands for: once for a page the engine rewrote, more for an allocation map
// that records several bits at once, and not at all for a page whose State has
// just been set to pageStateFree, where the field is a marker and not a count.
func bumpPageState(p *pageWriteBuf) {
	if p.stateBump <= 0 {
		return
	}

	state := binary.LittleEndian.Uint32(p.raw[pageStateOffset : pageStateOffset+4])
	binary.LittleEndian.PutUint32(p.raw[pageStateOffset:pageStateOffset+4], state+uint32(p.stateBump)) //nolint:gosec // stateBump counts pages, checked positive above
}

// setBit sets the given bit of the occupancy bitmap at the start of data.
func setBit(data []byte, bit, bitmapBytes int) {
	byteIdx := bit / 8
	if bit < 0 || byteIdx >= bitmapBytes || byteIdx >= len(data) {
		return
	}

	data[byteIdx] |= 1 << uint(bit%8)
}

// clearBit clears the given bit of the occupancy bitmap at the start of data.
func clearBit(data []byte, bit, bitmapBytes int) {
	byteIdx := bit / 8
	if bit < 0 || byteIdx >= bitmapBytes || byteIdx >= len(data) {
		return
	}

	data[byteIdx] &^= 1 << uint(bit%8)
}

// isZero reports whether every byte of data is zero.
func isZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}

	return true
}
