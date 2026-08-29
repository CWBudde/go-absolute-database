package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Database growth: what the engine does when an allocation finds no free page.
//
// Until this file existed every allocation refused with ErrOutOfSpace, which
// made CREATE TABLE unusable on almost every file in the corpus -- a customer
// fixture carries between zero and five free pages and a CREATE TABLE needs
// five or six of them. The engine grows the file instead, and
// ABSDiskEngine.hpp names the machinery it does it with:
// TABSDatabaseFreeSpaceManager.AddNewPageAndExtentFile, GetAddPagesStep /
// AddPagesStep, and TABSDiskPageManager.ExtendFile(PageCount).
//
// The rule, measured on fixtures that differ from their base by exactly one
// statement:
//
//	extend by ceil(shortfall / PageCountInExtent) whole extents
//
//	base                     geometry        free  need  short  grew by
//	MultiTable-createidx.abs 4096/extent 8   0     1     1      +8  (one extent)
//	MultiTable-createidx.abs 4096/extent 8   0     5     5      +8  (one extent)
//	Empty-p2048-e4.abs       2048/extent 4   1     6     5      +8  (two extents)
//
// The last row is what proves the step is PageCountInExtent rather than a
// constant 8, and that one request is covered by one extension of however many
// extents it takes rather than by a loop of single-extent ones. A fourth
// measurement, taken but not committed as a fixture (14 pages, extent 4, three
// free, six needed) grew by a single extent of 4, which pins the ceiling
// division from the other side.
//
// One thing the fixtures do NOT settle, and this file does not pretend they
// do: whether the engine sizes the extension against the whole statement or
// against each allocation the statement makes. CREATE TABLE does not ask for
// its five or six pages at once here -- allocateTablePages calls allocatePages
// once per page type -- so the growth happens per request, and a statement can
// extend the file more than once. Both accounts produce the same total on all
// four measurements above, including the 2048-byte one, where per-request
// growth adds one extent for the system file's pages and another for the index
// root. If a fixture is ever found that separates them, this is the paragraph
// that is wrong.
//
// What the growth itself writes, byte for byte
// (MultiTable-createidx.abs -> MultiTable-createidxgrow.abs):
//
//   - the file gets longer by extents*PageCountInExtent*PageSize bytes;
//   - the appended pages are pure zeros. No ABSP header is stamped on them:
//     pages 31..37 of the grown file hold no non-zero byte at all. A page gets
//     its header only when something allocates it, which is initPage's job;
//   - the database header's TotalPageCount at offset 30 moves to the new count.
//     That is the only field growth owns; LastUsedPageNo, LastObjectID, the
//     header State and the allocation maps all move for the *allocation* that
//     followed, not for the growth;
//   - page 1, the Extent Allocation Map, is not written at all -- its State is
//     unchanged across the growth. The extents that appear are free, and their
//     EAM entries are already zero, so there is nothing to record. Only the
//     allocation that then takes pages out of them touches page 1, through
//     updateExtentMap like any other allocation.
//
// Growth is bounded, and the bound is not cosmetic. markPagesAllocated indexes
// the Page Free Space payload as pfs[no/8] with no range check of its own; it
// is safe because nothing ever hands it a page number the map cannot describe.
// A single 4096-byte PFS page holds 4056 payload bytes = 32448 pages, and the
// engine has PfsPageNoForPageNo / EamPageNoForPageNo / IsPagePfsOrEam for the
// recurring PFS and EAM pages a larger file would need -- but no fixture
// exercises them and the largest file in the whole corpus is 78 pages, so a
// second PFS page is out of scope and growth past the first one is refused
// with ErrDatabaseTooLarge instead of guessed at.

const (
	// totalPageCountOffset is the offset of TotalPageCount in the database
	// header: the number of pages the file is long enough to hold. It is the
	// one header field growth owns.
	totalPageCountOffset = 30
)

// ErrDatabaseTooLarge reports growth that would take the file past what its
// first allocation map page can describe: more pages than the Page Free Space
// map on page 0 has bits for, or more extents than the Extent Allocation Map
// on page 1 has bit pairs for. The engine spills both maps onto further pages
// at that size; this package does not, because no fixture shows it doing so
// (the whole corpus tops out at 78 pages), and writing a second map page by
// guesswork would corrupt the first.
var ErrDatabaseTooLarge = errors.New("absdb: database cannot grow past its first allocation map page")

// mappablePages returns the highest number of pages this file's two allocation
// maps can describe between them, which is the ceiling growth refuses to cross.
//
// The Page Free Space map spends one bit per page and the Extent Allocation Map
// two bits per extent, so at every geometry in the corpus the PFS is the tighter
// of the two by a wide margin -- 32448 pages against the EAM's 129792 at
// 4096/8. Both are computed anyway, and the tighter one wins, because the
// relationship inverts for a hypothetical file with fewer than two pages per
// extent and a silently-wrong bound is exactly what this check exists to
// prevent.
func (db *File) mappablePages() int {
	payload := db.payloadSize()
	if payload <= 0 {
		return 0
	}

	perExtent := int(db.pagesInExtent)
	if perExtent <= 0 {
		return 0
	}

	pfsCapacity := payload * 8
	eamCapacity := payload * 8 / eamBitsPerExtent * perExtent

	return min(pfsCapacity, eamCapacity)
}

// setTotalPageCount writes the database header's TotalPageCount. Like
// setLastUsedPageNo and setLastObjectID it sits in front of page 0's own ABSP
// header, which writePageBuf does not cover, so it is written on its own; and
// like them it is a no-op when the field already holds the value, so that a
// caller cannot make the file look written-to by asking for what is already
// there.
func (db *File) setTotalPageCount(total int) error {
	if total < 0 || total > math.MaxInt32 {
		return fmt.Errorf("absdb: invalid total page count %d", total)
	}

	if int32(total) == db.totalPageCount {
		return nil
	}

	db.totalPageCount = int32(total)

	var buf [4]byte

	binary.LittleEndian.PutUint32(buf[:], uint32(db.totalPageCount))

	_, err := db.f.WriteAt(buf[:], totalPageCountOffset)
	if err != nil {
		return fmt.Errorf("absdb: writing total page count: %w", err)
	}

	return nil
}

// extendFile lengthens the database by whole extents until it holds at least
// shortfall more pages than it does now, and returns once the new pages are
// readable. See the file comment for the rule and the measurements behind it.
//
// The order of the three steps is not free. The file is lengthened first,
// db.size follows it, and only then does TotalPageCount move -- because every
// page a caller subsequently buffers goes through ReadPage, which rejects a
// page number at or past PageCount() with ErrPageOutOfRange and reports
// ErrTruncated when the block it wants runs off the end of the file. A page has
// to be both announced and present before anything can touch it, and announcing
// it first would open a window in which a read of it fails.
//
// The appended pages are left exactly as ftruncate leaves them, all zero: the
// engine stamps no ABSP header on a page it has only made room for (see the
// file comment). initPage writes the header when the page is allocated.
//
// Like setLastUsedPageNo, this writes the header field immediately rather than
// buffering it for the enclosing operation's flush. A rolled-back TableWriter
// therefore leaves the file longer than it found it, with the extra pages free
// -- which is a larger file, not a corrupt one.
func (db *File) extendFile(shortfall int) error {
	if !db.writable {
		return ErrReadOnly
	}

	if shortfall <= 0 {
		return fmt.Errorf("absdb: cannot extend the file by %d pages", shortfall)
	}

	perExtent := int(db.pagesInExtent)
	if perExtent <= 0 {
		return fmt.Errorf("absdb: invalid extent size %d", perExtent)
	}

	extents := (shortfall + perExtent - 1) / perExtent
	total := db.PageCount() + extents*perExtent

	if limit := db.mappablePages(); total > limit {
		return fmt.Errorf("%w: growing to %d pages, its maps describe %d",
			ErrDatabaseTooLarge, total, limit)
	}

	// The file spans every page's own block plus the leading
	// diskPageHeaderOffset bytes of one block past the last, which is where the
	// last page's payload ends. See the payload model in absdb.go.
	size := int64(total)*int64(db.pageSize) + diskPageHeaderOffset

	if err := db.f.Truncate(size); err != nil {
		return fmt.Errorf("absdb: extending the file to %d pages: %w", total, err)
	}

	db.size = size

	return db.setTotalPageCount(total)
}
