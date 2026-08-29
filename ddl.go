package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
)

// Write support: schema operations.
//
// Of the schema operations Phase 8 lists, only DROP TABLE is here, and the
// reason is the compressor rather than the format. A table's column
// definitions live in a zlib-compressed internal file, and the engine's zlib
// is the C library at level 1: all 34 compressed internal files in the corpus
// are reproduced byte for byte by zlib.compress(data, 1), and by no other
// level. Go's compress/zlib reproduces none of them at any level, because its
// level-1 encoder is not zlib's. So CREATE TABLE, ALTER TABLE and CREATE and
// DROP INDEX — every one of which rewrites that stream — cannot yet be
// byte-identical to the engine's own output, which is the standard the write
// path is held to. DROP TABLE is the one schema operation that never touches a
// compressed stream: the table catalog it edits is stored uncompressed.
//
// What the engine does for a DROP, established by diffing files that differ by
// exactly one statement (testdata/MultiTable-drop*.abs):
//
//   - every page the table owns is tombstoned, its ABSP State set to
//     pageStateFree. The pages keep their type, their owner and their contents;
//     nothing is erased;
//   - the catalog entry is removed by shifting the entries behind it down and
//     shortening the internal file by one entry. The bytes past the new end are
//     left alone, so the last entry stays behind as a stale duplicate;
//   - the Page Free Space map on page 0 loses one bit per freed page, and the
//     Extent Allocation Map on page 1 downgrades any extent that was full;
//   - LastUsedPageNo in the database header follows the highest page still
//     allocated, and State advances by one for the transaction.
//
// The file is never shortened here. The engine does shorten it, but the only
// evidence for when is a file whose last table was dropped, which is why
// dropping the last table is refused rather than guessed at.

const (
	// pfsPageNo is the page whose payload holds the Page Free Space map: one
	// bit per page of the file, least significant bit first, set while the page
	// is allocated. The name is the engine's own — ABSDiskEngine.dcu exports
	// GetPageUsageFromPFS, ABS_PAGE_IS_FREE and ABS_PAGE_IS_FULL — and the map
	// agrees with the ABSP State of every page in every fixture, which is what
	// TestAllocationMapsDescribeEveryFixture asserts.
	pfsPageNo = 0

	// eamPageNo is the page whose payload holds the Extent Allocation Map: two
	// bits per extent of pagesInExtent pages, again least significant first.
	eamPageNo = 1

	// The three values an EAM entry takes, named after ABS_EXTENT_IS_FREE,
	// ABS_EXTENT_IS_PARTIAL_USED and ABS_EXTENT_IS_FULL in the same unit. The
	// fourth two-bit value does not occur.
	extentFree    byte = 0
	extentPartial byte = 1
	extentFull    byte = 3

	// eamBitsPerExtent is the width of one EAM entry.
	eamBitsPerExtent = 2

	// lastUsedPageOffset is the offset of LastUsedPageNo in the database
	// header: the highest page number still allocated.
	lastUsedPageOffset = 34

	// systemIndexRootsSize is the size of the two page numbers an internal file
	// of column definitions ends with, described at systemIndexRoots.
	systemIndexRootsSize = 8

	// systemOwner marks a page that belongs to the database rather than to any
	// table. It is distinct from anyTableID, which means "every table".
	systemOwner = -2

	// lastObjectIDOffset is the offset of LastObjectID in the database header;
	// see File.lastObjectID.
	lastObjectIDOffset = 376

	// noPageNo is -1 reinterpreted as the uint32 binary.LittleEndian.PutUint32
	// takes, for the int32 page-reference fields (NextPageNo, RecPageNo, a
	// B-tree node's sibling pointers) that mean "none" at that value. A typed
	// negative constant cannot convert straight to uint32 even through int32,
	// so this is spelled as the bit pattern instead.
	noPageNo uint32 = 0xFFFFFFFF
)

var (
	// ErrPageUnattributed reports that the file holds an allocated page this
	// package cannot assign to a table or to the database itself. A schema
	// operation refuses rather than proceed, because a page it cannot name is a
	// page it might leave allocated with nothing referring to it.
	ErrPageUnattributed = errors.New("absdb: file holds a page that belongs to no table")

	// ErrTableHasBlobPages reports a drop of a table that owns BLOB pages. They
	// are reachable only through the table's BLOB page index, which names the
	// pages a BLOB starts on and not the ones it continues on, so freeing what
	// it lists would leak the rest.
	ErrTableHasBlobPages = errors.New("absdb: table owns BLOB pages this package cannot free")

	// ErrLastTable reports a drop of the database's only table. The engine
	// shortens the file when the catalog empties, and no fixture pins the rule
	// it shortens by, so this package refuses instead of writing a file that
	// would differ from the engine's.
	ErrLastTable = errors.New("absdb: cannot drop the database's only table")

	// ErrCatalogNotWritable reports a table catalog that cannot be rewritten in
	// place: one that is compressed, or one that spans more than a single page.
	// Neither occurs in any fixture.
	ErrCatalogNotWritable = errors.New("absdb: table catalog cannot be rewritten in place")
)

// databasePageTypes are the page types belonging to the database rather than to
// a table: the file header, the system directory, the table catalog, and the
// two system internal files at types 4 and 5, which every fixture carries
// exactly once and which no observed write modifies.
var databasePageTypes = map[uint16]bool{
	PageTypeFileHdr:         true,
	PageTypeSystemDir:       true,
	pageTypeSystemFileDir:   true,
	pageTypeConnectionTable: true,
	PageTypeTableList:       true,
}

// DropTable removes a table and every page it owns from the database.
//
// It reproduces what the engine's own DROP TABLE writes, byte for byte; see the
// file comment for what that is. It fails with ErrReadOnly unless the file was
// opened with OpenForWrite, and refuses rather than guess when the table owns
// BLOB pages, when it is the database's only table, or when the file holds a
// page that cannot be attributed to a table.
//
// Like every write here, the change is not crash-atomic: a crash midway leaves
// some pages written.
func (db *File) DropTable(name string) error {
	if !db.writable {
		return ErrReadOnly
	}

	tables, err := db.Tables()
	if err != nil {
		return err
	}

	position := -1

	for i, t := range tables {
		if strings.EqualFold(t.Name, name) {
			position = i

			break
		}
	}

	if position < 0 {
		return fmt.Errorf("%w: %q", ErrNoSuchTable, name)
	}

	owned, err := db.ownedPages(tables)
	if err != nil {
		return err
	}

	// The specific reason first: a table with BLOB pages is refused for what it
	// holds, and the pages it holds are also the ones nothing else can name.
	if owned.blobIndexRoots[tables[position].ID] >= 0 {
		return fmt.Errorf("%w: %q", ErrTableHasBlobPages, tables[position].Name)
	}

	if len(owned.unattributed) > 0 {
		return fmt.Errorf("%w: pages %v", ErrPageUnattributed, owned.unattributed)
	}

	if len(tables) == 1 {
		return fmt.Errorf("%w: %q", ErrLastTable, tables[0].Name)
	}

	return db.applyDrop(position, tables[position], owned.of(tables[position].ID))
}

// applyDrop performs the drop: tombstone, catalog, allocation maps, header.
func (db *File) applyDrop(position int, info TableInfo, pages []int) error {
	w := newPageEdit(db)

	for _, no := range pages {
		buf, err := w.load(no)
		if err != nil {
			return err
		}

		// A freed page's State is a marker rather than a counter, so it is set
		// outright and must not then be advanced by the write.
		binary.LittleEndian.PutUint32(buf.raw[pageStateOffset:pageStateOffset+4], uint32(pageStateFree))

		buf.stateBump = 0
		buf.dirty = true
	}

	err := db.dropCatalogEntry(w, position)
	if err != nil {
		return err
	}

	lastUsed, err := db.releasePages(w, pages)
	if err != nil {
		return err
	}

	err = db.flushPages(w.order, w.pages)
	if err != nil {
		return err
	}

	err = db.setLastUsedPageNo(lastUsed)
	if err != nil {
		return err
	}

	err = db.bumpFileState()
	if err != nil {
		return err
	}

	if err := db.f.Sync(); err != nil {
		return fmt.Errorf("absdb: flushing drop of %q: %w", info.Name, err)
	}

	return nil
}

// dropCatalogEntry removes one entry from the table catalog.
//
// The entries behind it move down and the internal file's two length fields
// lose one entry, which leaves the bytes of the old last entry in place past
// the new end. That is not tidiness lost: it is what the engine writes, and a
// parser that ignores the length field sees the last table twice because of it.
func (db *File) dropCatalogEntry(w *pageEdit, position int) error {
	pageNo, err := db.findPageByType(PageTypeTableList)
	if err != nil {
		return err
	}

	if pageNo < 0 {
		return ErrNoCatalog
	}

	buf, err := w.load(pageNo)
	if err != nil {
		return err
	}

	err = removeCatalogEntry(buf.payload, position)
	if err != nil {
		return err
	}

	buf.dirty = true

	return nil
}

// removeCatalogEntry edits the catalog's internal file in place: it moves the
// entries behind position down and shortens the file by one entry.
//
// The bytes past the new end are deliberately left as they were. That is what
// the engine writes, and it is why a parser that ignores the length field
// reports the last surviving table twice.
func removeCatalogEntry(payload []byte, position int) error {
	if len(payload) < internalFileHeaderSize {
		return fmt.Errorf("%w: catalog page is %d bytes", ErrCatalogNotWritable, len(payload))
	}

	headerSize := int(payload[0])
	stored := int(int32(binary.LittleEndian.Uint32(payload[1:5])))
	decompressed := int(int32(binary.LittleEndian.Uint32(payload[5:9])))
	algorithm := payload[9]

	switch {
	case algorithm != 0 || stored != decompressed:
		return fmt.Errorf("%w: it is compressed with algorithm %d", ErrCatalogNotWritable, algorithm)
	case headerSize < internalFileHeaderSize || stored <= 0 || stored%tableListEntrySize != 0:
		return fmt.Errorf("%w: %d bytes is not a whole number of entries", ErrCatalogNotWritable, stored)
	case headerSize+stored > len(payload):
		return fmt.Errorf("%w: it spans more than one page", ErrCatalogNotWritable)
	case position < 0 || position >= stored/tableListEntrySize:
		return fmt.Errorf("%w: no entry %d among %d", ErrNoSuchTable, position, stored/tableListEntrySize)
	}

	entries := payload[headerSize : headerSize+stored]
	copy(entries[position*tableListEntrySize:], entries[(position+1)*tableListEntrySize:])

	remaining := uint32(stored - tableListEntrySize) //nolint:gosec // stored is a positive multiple of the entry size

	binary.LittleEndian.PutUint32(payload[1:5], remaining)
	binary.LittleEndian.PutUint32(payload[5:9], remaining)

	return nil
}

// releasePages clears the allocation maps for a set of pages and returns the
// highest page still allocated afterwards.
//
// The two maps are kept differently, and the difference is measured rather than
// assumed. The Page Free Space map loses exactly one bit per page, and page 0's
// State counter advances once per bit. The Extent Allocation Map only ever
// downgrades an extent that was full to partially used: freeing the last page
// of an already-partial extent leaves the entry alone and page 1 unwritten,
// which is why dropping three tables in a row advances page 1's State only
// three times and not nine.
func (db *File) releasePages(w *pageEdit, pages []int) (int, error) {
	pfs, err := w.load(pfsPageNo)
	if err != nil {
		return 0, err
	}

	for _, no := range pages {
		if !pfsAllocated(pfs.payload, no) {
			return 0, fmt.Errorf("absdb: page %d is already free", no)
		}

		pfs.payload[no/8] &^= 1 << (no % 8)
	}

	pfs.stateBump = len(pages)
	pfs.dirty = len(pages) > 0

	eam, err := w.load(eamPageNo)
	if err != nil {
		return 0, err
	}

	perExtent := int(db.pagesInExtent)
	if perExtent <= 0 {
		return 0, fmt.Errorf("absdb: invalid extent size %d", perExtent)
	}

	changed := 0
	seen := make(map[int]bool, len(pages))

	for _, no := range pages {
		extent := no / perExtent
		if seen[extent] {
			continue
		}

		seen[extent] = true

		if extentState(eam.payload, extent) != extentFull {
			continue
		}

		setExtentState(eam.payload, extent, extentPartial)

		changed++
	}

	eam.stateBump = changed
	eam.dirty = changed > 0

	last := -1

	for no := range db.PageCount() {
		if pfsAllocated(pfs.payload, no) {
			last = no
		}
	}

	return last, nil
}

// setLastUsedPageNo writes the database header's LastUsedPageNo. The field sits
// in front of the page's ABSP header, which writePageBuf does not cover, so it
// is written on its own the way the State counter is.
func (db *File) setLastUsedPageNo(last int) error {
	if int32(last) == db.lastUsedPageNo { //nolint:gosec // last is a page number, bounded by PageCount
		return nil
	}

	db.lastUsedPageNo = int32(last) //nolint:gosec // as above

	var buf [4]byte

	binary.LittleEndian.PutUint32(buf[:], uint32(db.lastUsedPageNo))

	_, err := db.f.WriteAt(buf[:], lastUsedPageOffset)
	if err != nil {
		return fmt.Errorf("absdb: writing last used page number: %w", err)
	}

	return nil
}

// setLastObjectID writes the database header's LastObjectID, the mirror of
// setLastUsedPageNo for the other header field a schema operation that
// allocates anything has to move forward.
func (db *File) setLastObjectID(id int32) error {
	if id == db.lastObjectID {
		return nil
	}

	db.lastObjectID = id

	var buf [4]byte

	binary.LittleEndian.PutUint32(buf[:], uint32(id))

	_, err := db.f.WriteAt(buf[:], lastObjectIDOffset)
	if err != nil {
		return fmt.Errorf("absdb: writing last object id: %w", err)
	}

	return nil
}

// ErrOutOfSpace reports an allocation that found too few free pages even after
// the file grew to make room for it. Running out of free pages is no longer a
// refusal on its own -- allocatePages extends the file by whole extents (see
// ddl_grow.go) -- so reaching this means the allocation maps and the file
// disagree about what is free, not that the database is full. Growth that would
// take the file past what those maps can describe is refused earlier, with
// ErrDatabaseTooLarge.
var ErrOutOfSpace = errors.New("absdb: not enough free pages")

// newPageState returns the initial ABSP State a freshly allocated page is
// stamped with. FINDING 1 of the CREATE TABLE analysis this package was built
// from: across every fixture, a live page's State is uniformly distributed in
// [0, 2^30), and byte-identical pages allocated at different times carry
// different States. The engine seeds it randomly and counts up from there, so
// this package cannot reproduce the value -- it can only vary it the way the
// engine does. db.randPageState lets a test pin it instead.
func (db *File) newPageState() uint32 {
	if db.randPageState != nil {
		return db.randPageState()
	}

	return seedPageState()
}

// seedPageState is newPageState without a File to ask, for CreateDatabase,
// which stamps its pages before any File exists to hold them.
func seedPageState() uint32 {
	//nolint:gosec // not a security context: the engine's own seed is
	// unreproducible regardless (see newPageState), so math/rand/v2 only needs
	// to vary from run to run, and unlike crypto/rand it cannot fail.
	return rand.N(uint32(1) << 30)
}

// initPage stamps a freshly allocated page with a complete ABSP header: the
// marker, a random State (newPageState), the page's type and owner, and no
// chain link. Every fixture's newly allocated pages carry ObjectID 0xFFFFFFFF
// except data pages, which this package does not allocate yet -- CREATE TABLE
// writes no data page, matching PLAN.md.
//
// The payload is left zeroed, which is what an unallocated page already is;
// initPage does not need to clear it, only the caller's later write does.
func (db *File) initPage(buf *pageWriteBuf, pageType uint16, objectID int32) {
	writeDiskPageHeader(buf.raw[diskPageHeaderOffset:diskPageHeaderOffset+diskPageHeaderSize],
		db.newPageState(), pageType, objectID)

	buf.stateBump = 0 // State was just set outright, like a tombstoned page's.
	buf.dirty = true
}

// writeDiskPageHeader fills h, the diskPageHeaderSize bytes at
// diskPageHeaderOffset of a page's block, with a complete ABSP header. It is
// the one place the header's layout is written, shared by initPage -- which
// stamps a page allocated out of an existing file -- and CreateDatabase, which
// stamps the five pages of a file that did not exist a moment ago.
func writeDiskPageHeader(h []byte, state uint32, pageType uint16, objectID int32) {
	clear(h)
	copy(h[0:4], "ABSP")
	binary.LittleEndian.PutUint32(h[4:8], state)
	binary.LittleEndian.PutUint16(h[8:10], pageType)
	binary.LittleEndian.PutUint32(h[10:14], noPageNo) // NextPageNo: no chain yet
	// CRC32, CRCType, HashType, CipherType, MACType are already zero from clear(h).
	binary.LittleEndian.PutUint32(h[22:26], uint32(objectID))
	binary.LittleEndian.PutUint32(h[26:30], noPageNo) // RecPageNo: not a record

	// RecItemNo and the 8 reserved trailing bytes are already zero.
}

// computeExtentState derives an Extent Allocation Map entry straight from the
// Page Free Space map. It is what allocation needs and releasePages does not:
// releasePages only ever downgrades an extent it already knows was full, but
// a freshly allocated extent can move free -> partial, free -> full or
// partial -> full depending on how many of its pages a single call just took
// -- measured on CREATE TABLE Delta, whose five new pages leave their shared
// extent partial, not full, because three of its eight pages stay free.
//
// An extent that runs off the end of the file counts its missing pages as
// free, so a trailing extent is never full until the file is long enough to
// hold all of it. That is the engine's own convention, not a choice: page 1 of
// MultiTable-createidx.abs calls extent 3 partial although every page of it the
// 30-page file actually has (24 through 29) is allocated, and page 1 of
// Empty-p2048-e4.abs calls extent 1 partial although its only existing page is
// allocated too. It matters as soon as the file can grow: an allocation that
// filled the last extent of a short file would otherwise write "full", and the
// growth that followed would have to write it back to "partial", advancing
// page 1's State twice for a change the engine never made.
func computeExtentState(pfs []byte, extent, perExtent, pageCount int) byte {
	allocated := 0

	for no := extent * perExtent; no < (extent+1)*perExtent; no++ {
		if no < pageCount && pfsAllocated(pfs, no) {
			allocated++
		}
	}

	switch allocated {
	case 0:
		return extentFree
	case perExtent:
		return extentFull
	default:
		return extentPartial
	}
}

// pageLoader is the shared capability pageEdit and TableWriter both offer: a
// page buffered for modification, read and cached on first use. allocatePages
// works over either, which is what lets a TableWriter grow its own table's
// data pages (see growTable) through the same allocator the DDL operations in
// this file use, without either side depending on the other's page set.
type pageLoader interface {
	load(no int) (*pageWriteBuf, error)
	loadFresh(no int) (*pageWriteBuf, error)
}

// allocatePages reserves n free pages, lowest page number first, and returns
// their numbers in ascending order. Each page is stamped with a fresh ABSP
// header via initPage; the caller links any chain and writes the payload.
//
// It may be called more than once against the same pageLoader -- CREATE TABLE
// calls it four times for four different page types -- and the Page Free
// Space and Extent Allocation Map State bumps accumulate correctly across
// those calls rather than being overwritten by the last one, because both
// pages are cached by the loader and the bump counters are reset only on a
// page's first touch.
//
// PLAN.md records the engine allocating the same way: a table created after a
// drop takes the freed pages before any higher one, and CREATE TABLE Delta on
// MultiTable.abs took pages 24-28, the file's five lowest free pages.
func (db *File) allocatePages(w pageLoader, n int, pageType uint16, objectID int32) ([]int, error) {
	if n <= 0 {
		return nil, fmt.Errorf("absdb: cannot allocate %d pages", n)
	}

	pfs, err := w.load(pfsPageNo)
	if err != nil {
		return nil, err
	}

	newPages := findFreePages(pfs.payload, db.PageCount(), n)
	if len(newPages) < n {
		// The file is short of pages, so it grows -- by whole extents, in one
		// extension sized to cover the whole request. See ddl_grow.go for the
		// rule and the fixtures that measured it. Page 0 stays valid across
		// this: extending the file appends zeroed pages and moves one header
		// field, and touches neither allocation map.
		if err := db.extendFile(n - len(newPages)); err != nil {
			return nil, err
		}

		newPages = findFreePages(pfs.payload, db.PageCount(), n)
		if len(newPages) < n {
			return nil, fmt.Errorf("%w: %d free after growing the file, need %d", ErrOutOfSpace, len(newPages), n)
		}
	}

	markPagesAllocated(pfs, newPages)

	eam, err := w.load(eamPageNo)
	if err != nil {
		return nil, err
	}

	perExtent := int(db.pagesInExtent)
	if perExtent <= 0 {
		return nil, fmt.Errorf("absdb: invalid extent size %d", perExtent)
	}

	updateExtentMap(pfs.payload, eam, newPages, perExtent, db.PageCount())

	if err := db.initNewPages(w, newPages, pageType, objectID); err != nil {
		return nil, err
	}

	highest := max(newPages[len(newPages)-1], int(db.lastUsedPageNo))

	if err := db.setLastUsedPageNo(highest); err != nil {
		return nil, err
	}

	return newPages, nil
}

// findFreePages scans the Page Free Space map for the n lowest-numbered pages
// it marks free, in ascending order. It may return fewer than n.
//
// The scan stops at the last page the map itself has a bit for as well as at
// the end of the file, and that second bound is load-bearing: pfsAllocated
// reports a page past the end of the map as free, and markPagesAllocated writes
// pfs[no/8] with no range check of its own, so without this a map too small for
// the file it describes would be written past its end. extendFile refuses to
// cross the same ceiling first (ErrDatabaseTooLarge), so in practice this only
// ever binds on a malformed file.
func findFreePages(pfs []byte, pageCount, n int) []int {
	var free []int

	pageCount = min(pageCount, len(pfs)*8)

	for no := 0; no < pageCount && len(free) < n; no++ {
		if !pfsAllocated(pfs, no) {
			free = append(free, no)
		}
	}

	return free
}

// markPagesAllocated sets each page's Page Free Space bit and accumulates the
// State bump across however many times allocatePages touches this same
// buffered page within one pageLoader (see allocatePages' doc comment).
func markPagesAllocated(pfs *pageWriteBuf, newPages []int) {
	for _, no := range newPages {
		pfs.payload[no/8] |= 1 << (no % 8)
	}

	if !pfs.dirty {
		pfs.stateBump = 0
	}

	pfs.stateBump += len(newPages)
	pfs.dirty = true
}

// updateExtentMap recomputes and, where it changed, rewrites the Extent
// Allocation Map entry of every extent newPages touched, accumulating the
// State bump the same way markPagesAllocated does for the PFS.
func updateExtentMap(pfs []byte, eam *pageWriteBuf, newPages []int, perExtent, pageCount int) {
	if !eam.dirty {
		eam.stateBump = 0
	}

	changed := 0
	seenExtent := make(map[int]bool, len(newPages))

	for _, no := range newPages {
		extent := no / perExtent
		if seenExtent[extent] {
			continue
		}

		seenExtent[extent] = true

		state := computeExtentState(pfs, extent, perExtent, pageCount)
		if extentState(eam.payload, extent) == state {
			continue
		}

		setExtentState(eam.payload, extent, state)

		changed++
	}

	eam.stateBump += changed
	if changed > 0 {
		eam.dirty = true
	}
}

// initNewPages gives every newly allocated page a fresh ABSP header.
func (db *File) initNewPages(w pageLoader, newPages []int, pageType uint16, objectID int32) error {
	for _, no := range newPages {
		buf, err := w.loadFresh(no)
		if err != nil {
			return err
		}

		db.initPage(buf, pageType, objectID)
	}

	return nil
}

// linkChain sets a page's NextPageNo, the way allocatePages leaves it at -1
// for a page nothing has chained yet.
func (db *File) linkChain(w *pageEdit, no, next int) error {
	buf, err := w.load(no)
	if err != nil {
		return err
	}

	binary.LittleEndian.PutUint32(buf.raw[diskPageHeaderOffset+10:diskPageHeaderOffset+14], uint32(int32(next))) //nolint:gosec // next is a page number or -1
	buf.dirty = true

	return nil
}

// freeChainPages tombstones pages a chain no longer needs and returns them to
// the allocation maps, exactly the way applyDrop frees a whole table's pages.
func (db *File) freeChainPages(w *pageEdit, pages []int) error {
	for _, no := range pages {
		buf, err := w.load(no)
		if err != nil {
			return err
		}

		binary.LittleEndian.PutUint32(buf.raw[pageStateOffset:pageStateOffset+4], uint32(pageStateFree))
		buf.stateBump = 0
		buf.dirty = true
	}

	last, err := db.releasePages(w, pages)
	if err != nil {
		return err
	}

	return db.setLastUsedPageNo(last)
}

// resizeChain grows or shrinks a page chain to exactly n pages, allocating new
// pages (lowest free page first, like allocatePages) or freeing trailing ones,
// and keeps every NextPageNo link in step.
func (db *File) resizeChain(w *pageEdit, pages []int, n int, pageType uint16, objectID int32) ([]int, error) {
	for len(pages) < n {
		added, err := db.allocatePages(w, 1, pageType, objectID)
		if err != nil {
			return nil, err
		}

		if err := db.linkChain(w, pages[len(pages)-1], added[0]); err != nil {
			return nil, err
		}

		pages = append(pages, added[0])
	}

	if len(pages) > n {
		freed := pages[n:]
		pages = pages[:n]

		if err := db.linkChain(w, pages[len(pages)-1], -1); err != nil {
			return nil, err
		}

		if err := db.freeChainPages(w, freed); err != nil {
			return nil, err
		}
	}

	return pages, nil
}

// pfsAllocated reports whether the Page Free Space map marks a page allocated.
func pfsAllocated(pfs []byte, pageNo int) bool {
	if pageNo < 0 || pageNo/8 >= len(pfs) {
		return false
	}

	return pfs[pageNo/8]&(1<<(pageNo%8)) != 0
}

// extentState reads one Extent Allocation Map entry.
func extentState(eam []byte, extent int) byte {
	bit := extent * eamBitsPerExtent
	if extent < 0 || bit/8 >= len(eam) {
		return extentFree
	}

	return (eam[bit/8] >> (bit % 8)) & 0x3
}

// setExtentState writes one Extent Allocation Map entry.
func setExtentState(eam []byte, extent int, state byte) {
	bit := extent * eamBitsPerExtent
	if extent < 0 || bit/8 >= len(eam) {
		return
	}

	eam[bit/8] = eam[bit/8]&^(0x3<<(bit%8)) | state<<(bit%8)
}

// pageOwnership is the result of attributing every allocated page of a file.
type pageOwnership struct {
	// owner maps a page number to the ID of the table that owns it, or to
	// systemOwner for the pages the database itself owns.
	owner map[int]int

	// unattributed lists the allocated pages that belong to nothing this
	// package can name. BLOB pages are the ordinary case: they carry no owner
	// in their ABSP header and their table's BLOB page index does not list all
	// of them.
	unattributed []int

	// blobIndexRoots maps a table ID to the root page of its BLOB page index,
	// or to -1 when it has none.
	blobIndexRoots map[int]int
}

// of returns the pages owned by one table, in ascending order.
func (o pageOwnership) of(tableID int) []int {
	var pages []int

	for no, owner := range o.owner {
		if owner == tableID {
			pages = append(pages, no)
		}
	}

	sort.Ints(pages)

	return pages
}

// ownedPages attributes every allocated page of the file to a table or to the
// database. It is what makes a drop safe to perform: a page nothing claims is a
// page that would be left allocated with nothing pointing at it, so the callers
// refuse rather than proceed.
func (db *File) ownedPages(tables []TableInfo) (pageOwnership, error) {
	out := pageOwnership{
		owner:          make(map[int]int),
		blobIndexRoots: make(map[int]int, len(tables)),
	}

	for _, info := range tables {
		pages, blobRoot, err := db.tablePages(info)
		if err != nil {
			return pageOwnership{}, err
		}

		out.blobIndexRoots[info.ID] = blobRoot

		for _, no := range pages {
			if prev, ok := out.owner[no]; ok && prev != info.ID {
				return pageOwnership{}, fmt.Errorf("absdb: page %d is claimed by two tables", no)
			}

			out.owner[no] = info.ID
		}
	}

	for no := range db.PageCount() {
		page, err := db.ReadPage(no)
		if err != nil {
			return pageOwnership{}, err
		}

		if page.Header == nil || page.Freed() {
			continue
		}

		if databasePageTypes[page.Header.PageType] {
			out.owner[no] = systemOwner

			continue
		}

		if _, ok := out.owner[no]; !ok {
			out.unattributed = append(out.unattributed, no)
		}
	}

	return out, nil
}

// tablePages lists the pages one table owns, and the root of its BLOB page
// index, which is -1 when it has none.
//
// Four of the five kinds are named outright: the catalog entry gives the
// table's system internal file, its column definitions and its counters, the
// ABSP ObjectID gives its data pages, and the end of the column definitions
// gives the root of the index over those data pages. Only a user index has to
// be found by what it points at, which is what OpenIndex already does for
// reading.
func (db *File) tablePages(info TableInfo) ([]int, int, error) {
	seen := make(map[int]bool)

	err := db.addInternalFiles(seen, info)
	if err != nil {
		return nil, 0, err
	}

	recordIndexRoot, blobIndexRoot, err := db.systemIndexRoots(info)
	if err != nil {
		return nil, 0, err
	}

	err = db.addIndexTrees(seen, info, recordIndexRoot, blobIndexRoot)
	if err != nil {
		return nil, 0, err
	}

	err = db.addDataPages(seen, info)
	if err != nil {
		return nil, 0, err
	}

	pages := make([]int, 0, len(seen))
	for no := range seen {
		pages = append(pages, no)
	}

	sort.Ints(pages)

	return pages, blobIndexRoot, nil
}

// addInternalFiles adds the three internal files the catalog entry names: the
// table's system file, its column definitions and its counters, each of which
// may span a chain of pages.
func (db *File) addInternalFiles(seen map[int]bool, info TableInfo) error {
	for _, start := range []int{info.systemPageNo, info.SchemaPageNo, info.InfoPageNo} {
		if start < 0 {
			continue
		}

		chain, err := db.internalFilePages(start)
		if err != nil {
			return err
		}

		for _, no := range chain {
			seen[no] = true
		}
	}

	return nil
}

// addIndexTrees adds every page of the table's index trees: the record page
// index, whose root the column definitions name outright, and any user index,
// which has to be recognised by the data pages its leaves point at. The BLOB
// page index is deliberately left out — a table that has one is refused before
// this is reached, so adding its pages would only hide that.
func (db *File) addIndexTrees(seen map[int]bool, info TableInfo, recordIndexRoot, blobIndexRoot int) error {
	roots := []int{recordIndexRoot}

	table, err := db.Table(info.Name)
	if err != nil {
		return err
	}

	reader, err := table.OpenIndex()

	switch {
	case err == nil:
		for _, idx := range reader.Indexes() {
			roots = append(roots, idx.RootPageNo)
		}
	case !errors.Is(err, ErrNoIndex):
		return err
	}

	for _, root := range roots {
		if root < 0 || root == blobIndexRoot {
			continue
		}

		tree, err := db.indexTreePages(root)
		if err != nil {
			return err
		}

		for _, no := range tree {
			seen[no] = true
		}
	}

	return nil
}

// addDataPages adds the table's data pages, which are the only pages whose ABSP
// header names their owner.
func (db *File) addDataPages(seen map[int]bool, info TableInfo) error {
	for no := range db.PageCount() {
		page, err := db.ReadPage(no)
		if err != nil {
			return err
		}

		if page.Header != nil && page.Header.PageType == PageTypeData &&
			!page.Freed() && int(page.Header.ObjectID) == info.ID {
			seen[no] = true
		}
	}

	return nil
}

// systemIndexRoots returns the two page numbers a table's column definitions
// end with: the root of the index over its data pages, and the root of the
// index over its BLOB pages, which is -1 when the table stores no BLOBs.
//
// That the second is a BLOB index and not something else is what the corpus
// says: it is -1 for every table without a BLOB column and a real page number
// for exactly the three fixtures that have one.
func (db *File) systemIndexRoots(info TableInfo) (recordIndex, blobIndex int, err error) {
	table, err := db.Table(info.Name)
	if err != nil {
		return 0, 0, err
	}

	pageNo, err := table.schemaPageNo()
	if err != nil {
		return 0, 0, err
	}

	page, err := db.ReadPage(pageNo)
	if err != nil {
		return 0, 0, err
	}

	raw, err := decompressInternalFile(page.PageData())
	if err != nil {
		return 0, 0, err
	}

	if len(raw) < systemIndexRootsSize {
		return 0, 0, fmt.Errorf("%w: %d bytes hold no index roots", ErrBadSchema, len(raw))
	}

	tail := raw[len(raw)-systemIndexRootsSize:]

	return int(int32(binary.LittleEndian.Uint32(tail[0:4]))),
		int(int32(binary.LittleEndian.Uint32(tail[4:8]))), nil
}

// internalFilePages returns the pages an internal file occupies, following the
// chain its ABSP headers describe. readInternalFilePages returns the bytes; a
// schema operation needs the page numbers themselves.
func (db *File) internalFilePages(first int) ([]int, error) {
	pages := []int{first}
	visited := map[int]bool{first: true}

	for {
		page, err := db.ReadPage(pages[len(pages)-1])
		if err != nil {
			return nil, err
		}

		next := int(nextPageNo(page))
		if next < 0 {
			return pages, nil
		}

		if visited[next] {
			return nil, fmt.Errorf("absdb: internal file at page %d visits page %d twice", first, next)
		}

		if len(pages) >= maxCatalogPages {
			return nil, fmt.Errorf("absdb: internal file at page %d is longer than %d pages", first, maxCatalogPages)
		}

		visited[next] = true
		pages = append(pages, next)
	}
}

// indexTreePages returns every page of the B-tree rooted at root, walking the
// child pointers of its internal nodes. The leaf chain is not followed: every
// leaf hangs off some internal node, so the descent already reaches them all.
func (db *File) indexTreePages(root int) ([]int, error) {
	var pages []int

	visited := make(map[int]bool)
	queue := []int{root}

	for len(queue) > 0 {
		no := queue[0]
		queue = queue[1:]

		if visited[no] {
			continue
		}

		visited[no] = true

		page, err := db.ReadPage(no)
		if err != nil {
			return nil, err
		}

		if page.Header == nil || page.Header.PageType != PageTypeIndex {
			return nil, fmt.Errorf("%w: page %d in a tree rooted at %d is not an index page",
				ErrMalformedIndex, no, root)
		}

		pages = append(pages, no)

		if len(pages) > db.PageCount() {
			return nil, fmt.Errorf("%w: tree rooted at %d has more pages than the file", ErrMalformedIndex, root)
		}

		data := page.PageData()

		header, err := parseBTreeHeader(data)
		if err != nil {
			return nil, err
		}

		if header.IsLeaf {
			continue
		}

		entries, err := readBTreeEntries(data, header)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			queue = append(queue, int(entry.PageNo))
		}
	}

	sort.Ints(pages)

	return pages, nil
}

// pageEdit is a set of pages held for modification, the schema operations'
// equivalent of what a TableWriter keeps for records. It is deliberately not a
// TableWriter: a schema operation writes pages no table owns.
type pageEdit struct {
	db    *File
	pages map[int]*pageWriteBuf
	order []int
}

func newPageEdit(db *File) *pageEdit {
	return &pageEdit{db: db, pages: make(map[int]*pageWriteBuf)}
}

// load buffers a page, reading it on first use.
func (w *pageEdit) load(no int) (*pageWriteBuf, error) {
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

// loadFresh is load's counterpart for a page allocatePages is about to
// initialize, which may not carry an ABSP header yet. See bufferPageFresh.
func (w *pageEdit) loadFresh(no int) (*pageWriteBuf, error) {
	if buf, ok := w.pages[no]; ok {
		return buf, nil
	}

	buf, err := w.db.bufferPageFresh(no)
	if err != nil {
		return nil, err
	}

	w.pages[no] = buf
	w.order = append(w.order, no)

	return buf, nil
}
