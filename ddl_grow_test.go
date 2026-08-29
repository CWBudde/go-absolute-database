package absdb

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// growthCases are the three fixture pairs the growth rule was measured on, each
// a database the engine produced and the database the engine produced from it
// with exactly one statement. See the ddl_grow.go file comment for the rule.
//
// Each pins a different part of it:
//
//   - a one-page request on a 30-page, extent-8 file with no free page at all
//     grows it by 8: growth rounds up to a whole extent rather than appending
//     the pages asked for;
//   - a five-page request on the same file grows it by the same 8: the step is
//     not per page, one page and five cost the same;
//   - a six-page request on a 2048-byte, extent-4 file with one free page grows
//     it by 8 -- two extents of 4, not one of 8. This is the row that makes the
//     rule a rule: the step is PageCountInExtent, and a request bigger than one
//     extent is covered by a single extension of as many extents as it takes,
//     not by a loop of single-extent ones.
var growthCases = []struct {
	name string
	base string
	want string
	// need is how many pages the statement allocates, and shortfall how many
	// of them the base file had no room for.
	statement string
	need      int
	shortfall int
	// allocated lists the pages the statement takes out of the grown file, in
	// ascending order. Everything past the last of them is appended-and-unused.
	allocated []int
}{
	{
		name:      "one extent covers a one-page request",
		base:      "MultiTable-createidx.abs",
		want:      "MultiTable-createidxgrow.abs",
		statement: "CREATE INDEX IdxDeltaY ON Delta (Y)",
		need:      1,
		shortfall: 1,
		allocated: []int{30},
	},
	{
		name:      "one extent covers a five-page request",
		base:      "MultiTable-createidx.abs",
		want:      "MultiTable-createidxtab.abs",
		statement: "CREATE TABLE Epsilon (X INTEGER, Y INTEGER)",
		need:      5,
		shortfall: 5,
		allocated: []int{30, 31, 32, 33, 34},
	},
	{
		name:      "two extents cover a six-page request",
		base:      "Empty-p2048-e4.abs",
		want:      "Empty-p2048-e4-grow.abs",
		statement: "CREATE TABLE T1 (X INTEGER, Y INTEGER)",
		need:      6,
		shortfall: 5,
		allocated: []int{5, 6, 7, 8, 9, 10},
	},
}

// TestExtendFileMatchesEngineGrowth holds the growth step itself to the file
// the engine wrote, on all three pairs -- including the CREATE INDEX one, whose
// statement indexes a VARCHAR column and so cannot be replayed through
// CreateIndex yet (it refuses any key that is not an Int32).
//
// Growth is asserted in isolation rather than as part of a statement, which is
// what lets it be asserted exactly: extendFile alone must take the file to the
// engine's own length and page count, must leave every appended byte zero, and
// must write nothing else at all. The last part is the strongest claim here and
// the easiest to get wrong -- an implementation that stamped ABSP headers on the
// new pages, or that recorded the new extents in the Extent Allocation Map,
// would still read back correctly and would not be what the engine wrote.
func TestExtendFileMatchesEngineGrowth(t *testing.T) {
	for _, c := range growthCases {
		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(requireFixture(t, c.want))
			if err != nil {
				t.Fatalf("reading %s: %v", c.want, err)
			}

			base, err := os.ReadFile(requireFixture(t, c.base))
			if err != nil {
				t.Fatalf("reading %s: %v", c.base, err)
			}

			path := writableCopy(t, c.base)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			beforeCount := db.PageCount()
			pageSize := db.PageSize()
			perExtent := int(db.pagesInExtent)

			// The "free" column of the table in ddl_grow.go: how much of the
			// statement's need the base file could already meet.
			if free := countFreePages(t, db); free != c.need-c.shortfall {
				t.Fatalf("%s has %d free pages, the measurement says %d", c.base, free, c.need-c.shortfall)
			}

			if err := db.extendFile(c.shortfall); err != nil {
				t.Fatalf("extendFile(%d): %v", c.shortfall, err)
			}

			afterCount := db.PageCount()

			db.Close()

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading result: %v", err)
			}

			// The page count the engine ended at, and the whole number of
			// extents it takes to get there.
			wantCount := (len(want) - diskPageHeaderOffset) / pageSize
			if afterCount != wantCount {
				t.Errorf("grew to %d pages, the engine grew to %d", afterCount, wantCount)
			}

			extents := (c.shortfall + perExtent - 1) / perExtent
			if grew := afterCount - beforeCount; grew != extents*perExtent {
				t.Errorf("grew by %d pages, want %d extents of %d", grew, extents, perExtent)
			}

			if len(got) != len(want) {
				t.Fatalf("the grown file is %d bytes, the engine's is %d", len(got), len(want))
			}

			// Everything the base file already covered is untouched except
			// TotalPageCount, the one header field growth owns.
			for i := range base {
				if i >= totalPageCountOffset && i < totalPageCountOffset+4 {
					continue
				}

				if got[i] != base[i] {
					t.Fatalf("growth changed byte %d of the existing file: %02x -> %02x", i, base[i], got[i])
				}
			}

			// Everything past it is zero: no ABSP header, no allocation-map
			// entry, nothing.
			for i := len(base); i < len(got); i++ {
				if got[i] != 0 {
					t.Fatalf("appended byte %d is %02x, want an all-zero page", i, got[i])
				}
			}
		})
	}
}

// TestGrowthMatchesEngineByteForByte replays the whole statement, not just its
// growth step, and requires the result to be the file the engine wrote. It is
// TestCreateTableMatchesEngineByteForByte's standard applied to the two pairs
// whose statement this package can execute; the third pair's CREATE INDEX
// indexes a VARCHAR column, which CreateIndex refuses, and is covered by
// TestExtendFileMatchesEngineGrowth instead.
//
// The State exclusion is exactly the pages the statement allocates, and nothing
// else. A newly allocated page's ABSP State is seeded randomly by the engine
// and cannot be reproduced (FINDING 1, see newPageState); every other byte of
// those same pages is compared, and so is every byte of every other page --
// including the appended-but-unallocated ones at the end of the new extent,
// which is what pins them as zero-filled and header-free.
func TestGrowthMatchesEngineByteForByte(t *testing.T) {
	run := map[string]func(t *testing.T, db *File){
		"MultiTable-createidxtab.abs": func(t *testing.T, db *File) {
			t.Helper()

			err := db.CreateTable("Epsilon", []Column{
				{Name: "X", BaseType: BftInt32, FieldType: FieldInteger},
				{Name: "Y", BaseType: BftInt32, FieldType: FieldInteger},
			})
			if err != nil {
				t.Fatalf("CreateTable: %v", err)
			}
		},
		"Empty-p2048-e4-grow.abs": func(t *testing.T, db *File) {
			t.Helper()

			err := db.CreateTable("T1", []Column{
				{Name: "X", BaseType: BftInt32, FieldType: FieldInteger},
				{Name: "Y", BaseType: BftInt32, FieldType: FieldInteger},
			})
			if err != nil {
				t.Fatalf("CreateTable: %v", err)
			}
		},
	}

	for _, c := range growthCases {
		statement, ok := run[c.want]
		if !ok {
			continue
		}

		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(requireFixture(t, c.want))
			if err != nil {
				t.Fatalf("reading %s: %v", c.want, err)
			}

			path := writableCopy(t, c.base)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			statement(t, db)

			pageSize := db.PageSize()

			db.Close()

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading result: %v", err)
			}

			excluded := make(map[int]bool, len(c.allocated)*4)

			for _, no := range c.allocated {
				start := no*pageSize + pageStateOffset
				for i := range 4 {
					excluded[start+i] = true
				}
			}

			reportPageByteDifferences(t, got, want, c.statement, excluded, pageSize)
		})
	}
}

// TestEngineGrowthDoesNotWriteTheExtentMap pins, from the engine's own two
// files, the claim ddl_grow.go rests the "do not touch page 1" rule on: page 1
// of MultiTable-createidx.abs and of the file the engine grew from it are
// byte-identical, State word included.
//
// A page the engine writes always comes back with a moved State, so an
// unchanged one is evidence the page was not written. The extents growth
// appends are free, their two-bit EAM entries are already zero, and there is
// therefore nothing for it to record. The statement's own allocation still
// updates page 1 like any other allocation (updateExtentMap) -- here it did not,
// because taking page 30 leaves the extent it lands in partial, which is what it
// already was.
func TestEngineGrowthDoesNotWriteTheExtentMap(t *testing.T) {
	before, err := os.ReadFile(requireFixture(t, "MultiTable-createidx.abs"))
	if err != nil {
		t.Fatalf("reading MultiTable-createidx.abs: %v", err)
	}

	after, err := os.ReadFile(requireFixture(t, "MultiTable-createidxgrow.abs"))
	if err != nil {
		t.Fatalf("reading MultiTable-createidxgrow.abs: %v", err)
	}

	db := openTestFile(t, "MultiTable-createidx.abs")
	pageSize := db.PageSize()

	db.Close()

	// The whole block page 1 starts in, its ABSP header and State included.
	start := eamPageNo * pageSize
	if !bytes.Equal(before[start:start+pageSize], after[start:start+pageSize]) {
		t.Error("the engine rewrote page 1 across a growth; growth must not touch the Extent Allocation Map")
	}
}

// TestGrowthAppendsWholeExtentsOfZeroPages reads a grown file back as a file
// rather than as bytes, so a failure says which invariant broke rather than
// which offset differs. Byte identity already implies all of this, but only for
// the exact statements the fixtures cover.
func TestGrowthAppendsWholeExtentsOfZeroPages(t *testing.T) {
	path := writableCopy(t, "MultiTable-createidx.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	beforeCount := db.PageCount()
	perExtent := int(db.pagesInExtent)

	requireNoFreePage(t, db)

	err = db.CreateTable("Epsilon", []Column{
		{Name: "X", BaseType: BftInt32, FieldType: FieldInteger},
		{Name: "Y", BaseType: BftInt32, FieldType: FieldInteger},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	grew := db.PageCount() - beforeCount

	switch {
	case grew <= 0:
		t.Fatalf("the file did not grow: %d pages before, %d after", beforeCount, db.PageCount())
	case grew%perExtent != 0:
		t.Errorf("the file grew by %d pages, which is not a whole number of %d-page extents", grew, perExtent)
	case grew != perExtent:
		t.Errorf("a five-page shortfall grew the file by %d, want one extent of %d", grew, perExtent)
	}

	// Five of the eight appended pages were taken by the new table; the other
	// three are untouched: no ABSP header, not one non-zero byte.
	for no := beforeCount + 5; no < db.PageCount(); no++ {
		page, err := db.ReadPage(no)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", no, err)
		}

		if page.Header != nil {
			t.Errorf("appended page %d carries an ABSP header; growth must not stamp one", no)
		}

		if i := bytes.IndexFunc(page.Data, func(r rune) bool { return r != 0 }); i >= 0 {
			t.Errorf("appended page %d is not zero-filled: byte %d is %02x", no, i, page.Data[i])
		}
	}
}

// TestGrowthLetsATableWriterInsertIntoAFullFile is the other half of the
// pageLoader seam: growth has to reach the record writer, not just the schema
// operations, because both allocate through db.allocatePages.
//
// MultiTable-createidx.abs has no free page at all, and its Delta table has no
// data page, so the first insert has to call growTable, which has to grow the
// file. Before this the insert failed with ErrOutOfSpace.
func TestGrowthLetsATableWriterInsertIntoAFullFile(t *testing.T) {
	path := writableCopy(t, "MultiTable-createidx.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	requireNoFreePage(t, db)

	before := db.PageCount()

	table, err := db.Table("Delta")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	w, err := table.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	id, err := w.Insert([]any{int32(7), "grown"})
	if err != nil {
		t.Fatalf("Insert into a file with no free page: %v", err)
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if db.PageCount() <= before {
		t.Errorf("the insert did not grow the file: %d pages before, %d after", before, db.PageCount())
	}

	if id.PageNo < before {
		t.Errorf("the new record landed on page %d, which existed before the growth", id.PageNo)
	}

	rows := 0

	r, err := table.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for r.Next() {
		rows++
	}

	if err := r.Err(); err != nil {
		t.Fatalf("reading Delta back: %v", err)
	}

	if rows != 1 {
		t.Errorf("Delta holds %d rows after the insert, want 1", rows)
	}
}

// countFreePages returns how many of the file's pages its Page Free Space map
// marks free.
func countFreePages(t *testing.T, db *File) int {
	t.Helper()

	pfs, _ := allocationMaps(t, db)
	free := 0

	for no := range db.PageCount() {
		if !pfsAllocated(pfs, no) {
			free++
		}
	}

	return free
}

// requireNoFreePage skips unless the file's Page Free Space map marks every
// page allocated -- the precondition the growth tests need and the reason
// MultiTable-createidx.abs is their base.
func requireNoFreePage(t *testing.T, db *File) {
	t.Helper()

	pfs, _ := allocationMaps(t, db)

	for no := range db.PageCount() {
		if !pfsAllocated(pfs, no) {
			t.Fatalf("the fixture has free page %d; this test needs a file with none", no)
		}
	}
}

// TestGrowthRefusesPastTheAllocationMap pins the ceiling. The Page Free Space
// map is a single page's payload -- 4056 bytes, 32448 pages at a 4096-byte page
// size -- and markPagesAllocated indexes it without a range check of its own, so
// growth past it would write over whatever follows page 0's payload rather than
// failing. The engine spills the map onto further pages there
// (PfsPageNoForPageNo, EamPageNoForPageNo); this package has no fixture for that
// -- the whole corpus tops out at 78 pages -- and refuses.
//
// The setup is synthetic on purpose: TotalPageCount is poked on the open handle
// rather than in the file, because a file actually 32448 pages long would be
// 133 MB of fixture. extendFile checks the ceiling before it touches the disk,
// which is what makes that legitimate, and the test asserts the disk was in fact
// not touched.
func TestGrowthRefusesPastTheAllocationMap(t *testing.T) {
	path := writableCopy(t, "MultiTable.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	limit := db.mappablePages()
	if limit <= db.PageCount() {
		t.Fatalf("the fixture already claims %d pages, at or past the %d-page ceiling", db.PageCount(), limit)
	}

	sizeBefore := growFileSize(t, path)

	// One page short of the ceiling: a single extent of growth crosses it.
	db.totalPageCount = int32(limit - 1)

	err = db.extendFile(1)
	if !errors.Is(err, ErrDatabaseTooLarge) {
		t.Errorf("extending past the allocation map: %v, want ErrDatabaseTooLarge", err)
	}

	if got := growFileSize(t, path); got != sizeBefore {
		t.Errorf("a refused growth changed the file's length from %d to %d", sizeBefore, got)
	}

	// Exactly at the ceiling is refused too: any growth is at least one extent,
	// and the file is already as long as the map can describe.
	db.totalPageCount = int32(limit)

	if err := db.extendFile(1); !errors.Is(err, ErrDatabaseTooLarge) {
		t.Errorf("extending a file already at the ceiling: %v, want ErrDatabaseTooLarge", err)
	}
}

// TestFindFreePagesStopsAtTheMapsEnd is the unit-level half of the same bound.
// pfsAllocated reports a page past the end of the map as free, so a scan that
// stopped only at the page count would hand markPagesAllocated a page number it
// would then write outside the payload. The parser is fuzzed against arbitrary
// bytes, so this has to hold for a map far smaller than the page count claims.
func TestFindFreePagesStopsAtTheMapsEnd(t *testing.T) {
	// One byte of map: pages 0..7, of which 0 and 1 are marked allocated.
	got := findFreePages([]byte{0b0000_0011}, 1000, 20)

	want := []int{2, 3, 4, 5, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("findFreePages returned %v, want %v", got, want)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("findFreePages returned %v, want %v", got, want)
		}
	}
}

// TestExtendFileRefusesOnAReadOnlyHandle keeps growth behind the same
// OpenForWrite gate every other write path checks.
func TestExtendFileRefusesOnAReadOnlyHandle(t *testing.T) {
	db := openTestFile(t, "MultiTable.abs")
	defer db.Close()

	if err := db.extendFile(1); !errors.Is(err, ErrReadOnly) {
		t.Errorf("extendFile on a read-only handle: %v, want ErrReadOnly", err)
	}
}

// TestSystemFilePageCountFollowsThePageSize pins the one thing growth's second
// CREATE TABLE fixture forced out of hiding: the number of pages a fresh
// table's system internal file occupies is a function of the page size, not the
// constant 2 the 4096-byte fixtures show.
func TestSystemFilePageCountFollowsThePageSize(t *testing.T) {
	for _, c := range []struct {
		fixture string
		want    int
	}{
		{"MultiTable.abs", 2},     // 4096-byte pages, 4056 of payload
		{"Empty-p2048-e4.abs", 3}, // 2048-byte pages, 2008 of payload
	} {
		t.Run(c.fixture, func(t *testing.T) {
			db := openTestFile(t, c.fixture)
			defer db.Close()

			if got := db.systemFilePageCount(); got != c.want {
				t.Errorf("systemFilePageCount at page size %d is %d, want %d", db.PageSize(), got, c.want)
			}
		})
	}
}

// growFileSize returns the size of a file on disk.
func growFileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return info.Size()
}
