package absdb

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

// TestDropTableMatchesEngineByteForByte holds DROP TABLE to the same standard as
// the record writes: each case takes a database the engine produced, runs the
// single statement the engine was given, and requires the result to be
// byte-identical to the file the engine wrote.
//
// The four cases are not variations on one theme. Dropping the first entry
// rewrites every catalog entry behind it; dropping the last rewrites none;
// dropping the middle one is what the original fixture covers. The fourth drops
// a table with no rows, which owns no data page at all — the case where the
// table's index page can only be found by reading it out of the column
// definitions, because there are no rows for it to point at.
func TestDropTableMatchesEngineByteForByte(t *testing.T) {
	cases := []struct {
		name      string
		base      string
		want      string
		table     string
		statement string
	}{
		{
			name:      "drop the first table",
			base:      "MultiTable.abs",
			want:      "MultiTable-dropfirst.abs",
			table:     "Alpha",
			statement: "DROP TABLE Alpha",
		},
		{
			name:      "drop a table in the middle",
			base:      "MultiTable.abs",
			want:      "MultiTable-drop.abs",
			table:     "Beta",
			statement: "DROP TABLE Beta",
		},
		{
			name:      "drop the last table",
			base:      "MultiTable.abs",
			want:      "MultiTable-droplast.abs",
			table:     "Gamma",
			statement: "DROP TABLE Gamma",
		},
		{
			name:      "drop a table with no rows",
			base:      "MultiTable-create.abs",
			want:      "MultiTable-createdrop.abs",
			table:     "Delta",
			statement: "DROP TABLE Delta",
		},
	}

	for _, c := range cases {
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

			err = db.DropTable(c.table)
			if err != nil {
				t.Fatalf("DropTable(%q): %v", c.table, err)
			}

			db.Close()

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading result: %v", err)
			}

			reportByteDifferences(t, got, want, c.statement)
		})
	}
}

// TestDropTableReadsBack is the complement to byte identity: the surviving
// tables must still be readable, and the dropped one must be gone. Byte
// identity alone would not catch a reader that trusted a stale catalog entry,
// because the engine leaves one behind past the end of the catalog on purpose.
func TestDropTableReadsBack(t *testing.T) {
	path := writableCopy(t, "MultiTable.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	err = db.DropTable("Beta")
	if err != nil {
		t.Fatalf("DropTable: %v", err)
	}

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}

	if len(tables) != 2 || tables[0].Name != "Alpha" || tables[1].Name != "Gamma" {
		t.Fatalf("after the drop the catalog holds %v, want Alpha and Gamma", tables)
	}

	_, err = db.Table("Beta")
	if !errors.Is(err, ErrNoSuchTable) {
		t.Errorf("Table(\"Beta\") after the drop: %v, want ErrNoSuchTable", err)
	}

	for name, wantRows := range map[string]int{"Alpha": 2, "Gamma": 1} {
		table, err := db.Table(name)
		if err != nil {
			t.Fatalf("Table(%q): %v", name, err)
		}

		r, err := table.Open()
		if err != nil {
			t.Fatalf("Open(%q): %v", name, err)
		}

		rows := 0
		for r.Next() {
			rows++
		}

		if err := r.Err(); err != nil {
			t.Fatalf("reading %q: %v", name, err)
		}

		if rows != wantRows {
			t.Errorf("%s holds %d rows after the drop, want %d", name, rows, wantRows)
		}
	}
}

// TestDropTableRefusals checks that each boundary is an error rather than a
// silently wrong file.
func TestDropTableRefusals(t *testing.T) {
	t.Run("read-only", func(t *testing.T) {
		db := openTestFile(t, "MultiTable.abs")
		defer db.Close()

		if err := db.DropTable("Beta"); !errors.Is(err, ErrReadOnly) {
			t.Errorf("DropTable on a read-only file: %v, want ErrReadOnly", err)
		}
	})

	t.Run("no such table", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "MultiTable.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropTable("Nowhere"); !errors.Is(err, ErrNoSuchTable) {
			t.Errorf("DropTable of an unknown table: %v, want ErrNoSuchTable", err)
		}
	})

	t.Run("the only table", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropTable("Writes"); !errors.Is(err, ErrLastTable) {
			t.Errorf("DropTable of the only table: %v, want ErrLastTable", err)
		}
	})

	t.Run("a table with BLOB pages", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "RPDG0011.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropTable("RPDG0011.abs"); !errors.Is(err, ErrTableHasBlobPages) {
			t.Errorf("DropTable of a table with BLOBs: %v, want ErrTableHasBlobPages", err)
		}
	})

	t.Run("nothing is written when the drop is refused", func(t *testing.T) {
		path := writableCopy(t, "MultiTable.abs")
		before := fileDigest(t, path)

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		for _, name := range []string{"Nowhere", ""} {
			_ = db.DropTable(name)
		}

		db.Close()

		if fileDigest(t, path) != before {
			t.Error("a refused drop changed the file")
		}
	})
}

// TestAllocationMapsDescribeEveryFixture is what makes the allocation model a
// measurement rather than a reading of two files. The Page Free Space map on
// page 0 and the Extent Allocation Map on page 1 are checked against the page
// states they are supposed to summarise, in every fixture present.
//
// The two maps are asserted differently, and the asymmetry is the finding. A
// PFS bit is exact: it is set for exactly the pages that carry an ABSP header
// and are not tombstoned. An EAM entry is not, because the engine only ever
// downgrades one: freeing pages turns a full extent into a partial one and
// never turns a partial extent back into a free one, so "partial" claims
// nothing at all and only "full" and "free" can be checked.
func TestAllocationMapsDescribeEveryFixture(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db, err := Open(testdataPath(name))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			defer db.Close()

			if db.encrypted {
				t.Skip("encrypted: its allocation maps are ciphertext until Unlock")
			}

			pfs, eam := allocationMaps(t, db)

			allocated := make([]bool, db.PageCount())
			last := -1

			for no := range db.PageCount() {
				page, err := db.ReadPage(no)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", no, err)
				}

				allocated[no] = page.Header != nil && !page.Freed()
				if allocated[no] {
					last = no
				}

				if got := pfsAllocated(pfs, no); got != allocated[no] {
					t.Errorf("PFS says page %d allocated=%v, its ABSP header says %v", no, got, allocated[no])
				}
			}

			if int(db.lastUsedPageNo) != last {
				t.Errorf("header says the last used page is %d, the PFS says %d", db.lastUsedPageNo, last)
			}

			checkExtents(t, db, eam, allocated)
		})
	}
}

// checkExtents asserts the one direction of the Extent Allocation Map that is
// exact: an extent it calls full holds no free page, and one it calls free
// holds no allocated page.
func checkExtents(t *testing.T, db *File, eam []byte, allocated []bool) {
	t.Helper()

	perExtent := int(db.pagesInExtent)
	if perExtent <= 0 {
		t.Fatalf("file declares %d pages per extent", perExtent)
	}

	for extent := 0; extent*perExtent < len(allocated); extent++ {
		free, used := 0, 0

		for no := extent * perExtent; no < (extent+1)*perExtent && no < len(allocated); no++ {
			if allocated[no] {
				used++
			} else {
				free++
			}
		}

		// A trailing extent the file does not reach is short; its missing
		// pages count as free, which is why a file whose last page is
		// allocated still has a partial last extent.
		if (extent+1)*perExtent > len(allocated) {
			free += (extent+1)*perExtent - len(allocated)
		}

		switch extentState(eam, extent) {
		case extentFull:
			if free != 0 {
				t.Errorf("extent %d is marked full but holds %d free pages", extent, free)
			}
		case extentFree:
			if used != 0 {
				t.Errorf("extent %d is marked free but holds %d allocated pages", extent, used)
			}
		}
	}
}

// allocationMaps returns the payloads of the two allocation map pages.
func allocationMaps(t *testing.T, db *File) (pfs, eam []byte) {
	t.Helper()

	for _, no := range []int{pfsPageNo, eamPageNo} {
		page, err := db.ReadPage(no)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", no, err)
		}

		if no == pfsPageNo {
			pfs = page.PageData()
		} else {
			eam = page.PageData()
		}
	}

	return pfs, eam
}

// TestSystemIndexRootsNameTheBlobIndex pins the reading of the two page numbers
// a table's column definitions end with. The first is the index over its data
// pages, which every table has. The second is the index over its BLOB pages,
// and the evidence that it is one is that it is -1 for exactly the tables whose
// schema declares no BLOB column.
func TestSystemIndexRootsNameTheBlobIndex(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				t.Skipf("no readable catalog: %v", err)
			}

			for _, info := range tables {
				recordIndex, blobIndex, err := db.systemIndexRoots(info)
				if err != nil {
					t.Fatalf("systemIndexRoots(%q): %v", info.Name, err)
				}

				page, err := db.ReadPage(recordIndex)
				if err != nil || page.Header == nil || page.Header.PageType != PageTypeIndex {
					t.Errorf("%s: record page index at %d is not an index page", info.Name, recordIndex)
				}

				table, err := db.Table(info.Name)
				if err != nil {
					t.Fatalf("Table(%q): %v", info.Name, err)
				}

				schema, err := table.Schema()
				if err != nil {
					t.Fatalf("Schema(%q): %v", info.Name, err)
				}

				wantBlobIndex := false

				for _, c := range schema.Columns {
					if c.IsBLOB() {
						wantBlobIndex = true
					}
				}

				if got := blobIndex >= 0; got != wantBlobIndex {
					t.Errorf("%s: has a BLOB page index = %v (root %d), has a BLOB column = %v",
						info.Name, got, blobIndex, wantBlobIndex)
				}
			}
		})
	}
}

// TestExtentStateRoundTrip checks the two-bit accessors on their own, so that a
// failure in the fixture tests points at the model and not at the bit twiddling.
func TestExtentStateRoundTrip(t *testing.T) {
	eam := make([]byte, 4)

	for extent := range 16 {
		for _, state := range []byte{extentFree, extentPartial, extentFull} {
			setExtentState(eam, extent, state)

			if got := extentState(eam, extent); got != state {
				t.Fatalf("extent %d: set %d, read back %d", extent, state, got)
			}
		}
	}

	// Writing one entry must leave its neighbours alone.
	for extent := range 16 {
		setExtentState(eam, extent, extentFull)
	}

	setExtentState(eam, 5, extentFree)

	for extent := range 16 {
		want := extentFull
		if extent == 5 {
			want = extentFree
		}

		if got := extentState(eam, extent); got != want {
			t.Errorf("extent %d is %d after clearing extent 5, want %d", extent, got, want)
		}
	}

	// Out-of-range accesses must not panic or write past the buffer.
	setExtentState(eam, -1, extentFull)
	setExtentState(eam, 1000, extentFull)

	if got := extentState(eam, 1000); got != extentFree {
		t.Errorf("extent past the map reads as %d, want %d", got, extentFree)
	}
}

// TestPfsAllocatedBounds keeps the map accessor from reading past its page.
func TestPfsAllocatedBounds(t *testing.T) {
	pfs := []byte{0xFF}

	for _, no := range []int{-1, 8, 1 << 20} {
		if pfsAllocated(pfs, no) {
			t.Errorf("page %d reads as allocated from a one-byte map", no)
		}
	}

	for no := range 8 {
		if !pfsAllocated(pfs, no) {
			t.Errorf("page %d reads as free from a full map byte", no)
		}
	}
}

// TestDropTableKeepsHeaderCountersInStep checks the two header fields a drop
// moves, against the file on disk rather than against the in-memory copy.
func TestDropTableKeepsHeaderCountersInStep(t *testing.T) {
	path := writableCopy(t, "MultiTable.abs")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	err = db.DropTable("Alpha")
	if err != nil {
		t.Fatalf("DropTable: %v", err)
	}

	db.Close()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the result: %v", err)
	}

	field := func(data []byte, off int) int32 {
		return int32(binary.LittleEndian.Uint32(data[off : off+4]))
	}

	// Alpha owned the file's highest page, so the last used page number has to
	// fall back to the highest one still allocated.
	if got, want := field(after, lastUsedPageOffset), field(before, lastUsedPageOffset)-1; got != want {
		t.Errorf("last used page is %d after the drop, want %d", got, want)
	}

	if got, want := field(after, fileStateOffset), field(before, fileStateOffset)+1; got != want {
		t.Errorf("database State is %d after the drop, want %d", got, want)
	}
}

// makeCatalogPayload builds an uncompressed catalog internal file holding n
// entries, each filled with its own index so that a shift is visible.
func makeCatalogPayload(n int) []byte {
	payload := make([]byte, internalFileHeaderSize+n*tableListEntrySize)

	payload[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(payload[1:5], uint32(n*tableListEntrySize))
	binary.LittleEndian.PutUint32(payload[5:9], uint32(n*tableListEntrySize))

	for i := range n {
		for j := range tableListEntrySize {
			payload[internalFileHeaderSize+i*tableListEntrySize+j] = byte(i + 1)
		}
	}

	return payload
}

// TestRemoveCatalogEntry pins the catalog edit on its own, including the part
// that looks like a bug and is not: the bytes past the new end keep the old
// last entry.
func TestRemoveCatalogEntry(t *testing.T) {
	payload := makeCatalogPayload(3)

	err := removeCatalogEntry(payload, 1)
	if err != nil {
		t.Fatalf("removeCatalogEntry: %v", err)
	}

	if got := binary.LittleEndian.Uint32(payload[1:5]); got != 2*tableListEntrySize {
		t.Errorf("stored length is %d, want %d", got, 2*tableListEntrySize)
	}

	if got := binary.LittleEndian.Uint32(payload[5:9]); got != 2*tableListEntrySize {
		t.Errorf("decompressed length is %d, want %d", got, 2*tableListEntrySize)
	}

	entry := func(i int) byte {
		return payload[internalFileHeaderSize+i*tableListEntrySize]
	}

	if entry(0) != 1 || entry(1) != 3 {
		t.Errorf("after removing entry 1 the catalog reads %d, %d; want 1, 3", entry(0), entry(1))
	}

	// The third slot is past the new end and must be left as it was, because
	// that is what the engine writes.
	if entry(2) != 3 {
		t.Errorf("the stale slot past the end reads %d, want the old last entry 3", entry(2))
	}
}

// TestRemoveCatalogEntryRefusals covers the shapes no fixture has, so that a
// file that ever grows one is refused rather than corrupted.
func TestRemoveCatalogEntryRefusals(t *testing.T) {
	compressed := makeCatalogPayload(2)
	compressed[9] = 1

	mismatched := makeCatalogPayload(2)
	binary.LittleEndian.PutUint32(mismatched[5:9], 4*tableListEntrySize)

	ragged := makeCatalogPayload(2)
	binary.LittleEndian.PutUint32(ragged[1:5], 2*tableListEntrySize+7)
	binary.LittleEndian.PutUint32(ragged[5:9], 2*tableListEntrySize+7)

	chained := makeCatalogPayload(2)
	binary.LittleEndian.PutUint32(chained[1:5], 4*tableListEntrySize)
	binary.LittleEndian.PutUint32(chained[5:9], 4*tableListEntrySize)

	empty := makeCatalogPayload(0)

	cases := []struct {
		name     string
		payload  []byte
		position int
		want     error
	}{
		{"too short to hold a header", make([]byte, 4), 0, ErrCatalogNotWritable},
		{"compressed", compressed, 0, ErrCatalogNotWritable},
		{"lengths disagree", mismatched, 0, ErrCatalogNotWritable},
		{"not a whole number of entries", ragged, 0, ErrCatalogNotWritable},
		{"longer than the page", chained, 0, ErrCatalogNotWritable},
		{"no entries at all", empty, 0, ErrCatalogNotWritable},
		{"entry past the end", makeCatalogPayload(2), 2, ErrNoSuchTable},
		{"negative entry", makeCatalogPayload(2), -1, ErrNoSuchTable},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := removeCatalogEntry(c.payload, c.position); !errors.Is(err, c.want) {
				t.Errorf("removeCatalogEntry: %v, want %v", err, c.want)
			}
		})
	}
}

// TestDropTableOnACatalogLessFile checks the one malformed shape the read path
// tolerates and a schema operation must not: a file with no table catalog.
// Table("") still opens such a file, because there is only one table to mean,
// but a drop has nothing to remove an entry from.
func TestDropTableOnACatalogLessFile(t *testing.T) {
	path := writeSynthetic(t, synthSpec{
		columns: []synthColumn{{name: "Id", base: BftInt32, field: FieldInteger}},
		rows:    []synthRow{{values: [][]byte{synthInt32(1)}}},
	})

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	if _, err := db.Table(""); err != nil {
		t.Fatalf("the synthetic file should still open for reading: %v", err)
	}

	if err := db.DropTable("Anything"); !errors.Is(err, ErrNoCatalog) {
		t.Errorf("DropTable on a file with no catalog: %v, want ErrNoCatalog", err)
	}
}

// TestDropTableTwiceKeepsTheExtentMapDowngradeOnly guards the one rule in the
// allocation model that a straightforward reimplementation would get wrong: the
// Extent Allocation Map is only ever downgraded, so a second drop that frees
// pages in extents already marked partial writes page 1 not at all.
//
// The numbers are the engine's. Dropping all three tables of MultiTable.abs in
// one script leaves page 1's State at 9, three above the 6 it starts at, and
// page 0's at 43 — one per page freed, 7 + 6 + 6. Two drops therefore have to
// land on 9 and 37, and an implementation that recomputed the map from the PFS
// instead of downgrading it would write page 1 three times and land on 11.
func TestDropTableTwiceKeepsTheExtentMapDowngradeOnly(t *testing.T) {
	path := writableCopy(t, "MultiTable.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	for _, name := range []string{"Alpha", "Beta"} {
		if err := db.DropTable(name); err != nil {
			t.Fatalf("DropTable(%q): %v", name, err)
		}
	}

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}

	if len(tables) != 1 || tables[0].Name != "Gamma" {
		t.Fatalf("after two drops the catalog holds %v, want Gamma alone", tables)
	}

	for _, c := range []struct {
		pageNo int
		want   int32
	}{{pfsPageNo, 37}, {eamPageNo, 9}} {
		page, err := db.ReadPage(c.pageNo)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", c.pageNo, err)
		}

		if page.Header.State != c.want {
			t.Errorf("page %d State is %d after two drops, want %d", c.pageNo, page.Header.State, c.want)
		}
	}

	db.Close()
}
