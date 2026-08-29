package absdb

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
)

// compactedPages are the sixteen pages a compaction of MultiTable-drop.abs
// allocates -- everything but the two allocation maps, whose States are
// counters rather than seeds. They are the only bytes the byte-identity test
// below excludes.
var compactedPages = []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}

// TestCompactDatabaseMatchesEngineByteForByte holds compaction to the same
// standard as CREATE TABLE: reproduce, byte for byte, the file the engine's own
// Database -> Compact Database wrote for the same database.
//
// It was not obvious that this bar was reachable. Compaction allocates every
// page of its output and renumbers every object, so both of the things that
// make ALTER TABLE fall back to semantic identity are present here in a
// stronger form. They turn out not to bite: the object ids come out identical
// because the replay hands them out in the engine's own order, so the only
// bytes that differ are the sixteen randomly seeded page State words. Pages 0
// and 1 are not excluded -- their States are the Page Free Space bit count (18)
// and the Extent Allocation Map change count (5), and reproducing both is a
// large part of what this test proves.
func TestCompactDatabaseMatchesEngineByteForByte(t *testing.T) {
	want, err := os.ReadFile(requireFixture(t, "MultiTable-dropcompact.abs"))
	if err != nil {
		t.Fatalf("reading MultiTable-dropcompact.abs: %v", err)
	}

	src := requireFixture(t, "MultiTable-drop.abs")
	dst := newDatabasePath(t, "compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	db, err := Open(dst)
	if err != nil {
		t.Fatalf("Open(%q): %v", dst, err)
	}

	pageSize := db.PageSize()

	db.Close()

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	reportPageByteDifferences(t, got, want, "COMPACT DATABASE",
		pageStateExclusions(compactedPages, pageSize), pageSize)
}

// TestCompactDatabaseMatchesEnginePageLayout reads the same two files back as
// files rather than as bytes, so that a future divergence says which structural
// property broke rather than which offset moved. Byte identity already implies
// all of it, but only for this one fixture pair and only while it holds.
//
// The page sequence is the part worth naming: five system pages, then per table
// its system internal file, schema, info and record index, then its user
// indexes, then its data. The index page landing ahead of the data page is what
// says the engine creates a table's indexes before it copies its rows.
func TestCompactDatabaseMatchesEnginePageLayout(t *testing.T) {
	src := requireFixture(t, "MultiTable-drop.abs")
	wantPath := requireFixture(t, "MultiTable-dropcompact.abs")
	dst := newDatabasePath(t, "compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	got, want := describeDatabase(t, dst), describeDatabase(t, wantPath)

	if got.pageCount != want.pageCount {
		t.Errorf("wrote %d pages, the engine wrote %d", got.pageCount, want.pageCount)
	}

	if got.lastUsedPageNo != want.lastUsedPageNo {
		t.Errorf("LastUsedPageNo is %d, the engine wrote %d", got.lastUsedPageNo, want.lastUsedPageNo)
	}

	if got.state != want.state {
		t.Errorf("file State is %d, the engine wrote %d", got.state, want.state)
	}

	if got.lastObjectID != want.lastObjectID {
		t.Errorf("LastObjectID is %d, the engine wrote %d", got.lastObjectID, want.lastObjectID)
	}

	if fmt.Sprint(got.pageTypes) != fmt.Sprint(want.pageTypes) {
		t.Errorf("page types are %v, the engine wrote %v", got.pageTypes, want.pageTypes)
	}

	if fmt.Sprint(got.pageOwners) != fmt.Sprint(want.pageOwners) {
		t.Errorf("page owners are %v, the engine wrote %v", got.pageOwners, want.pageOwners)
	}

	if got.freePages != 0 || want.freePages != 0 {
		t.Errorf("compaction left %d free pages, the engine left %d", got.freePages, want.freePages)
	}
}

// databaseShape is everything about a file's page layout that a compaction has
// to reproduce and that reading it back can see.
type databaseShape struct {
	pageCount      int
	lastUsedPageNo int32
	state          int32
	lastObjectID   int32
	pageTypes      []int
	pageOwners     []int32
	freePages      int
}

func describeDatabase(t *testing.T, path string) databaseShape {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	shape := databaseShape{
		pageCount:      db.PageCount(),
		lastUsedPageNo: db.lastUsedPageNo,
		state:          db.state,
		lastObjectID:   db.lastObjectID,
	}

	summaries, err := db.ScanPages()
	if err != nil {
		t.Fatalf("ScanPages(%q): %v", path, err)
	}

	for _, s := range summaries {
		if s.Header == nil {
			shape.pageTypes = append(shape.pageTypes, -1)
			shape.pageOwners = append(shape.pageOwners, 0)
			shape.freePages++

			continue
		}

		shape.pageTypes = append(shape.pageTypes, int(s.Header.PageType))
		shape.pageOwners = append(shape.pageOwners, s.Header.ObjectID)

		if s.Header.State == pageStateFree {
			shape.freePages++
		}
	}

	return shape
}

// TestCompactDatabaseMatchesEngineSemantically is the reading of the same
// result that names the property rather than the offset: the compacted database
// must be indistinguishable from the engine's own through every read path this
// package has -- the table list, each table's columns, every row of every table,
// and the contents of every user index.
//
// Object ids are excluded, and only object ids. They are the one thing a
// compaction is free to renumber, and the engine renumbers them too (Gamma goes
// from 8 to 5); that they happen to come out identical is asserted by the
// byte-identity test above, not assumed here.
func TestCompactDatabaseMatchesEngineSemantically(t *testing.T) {
	src := requireFixture(t, "MultiTable-drop.abs")
	wantPath := requireFixture(t, "MultiTable-dropcompact.abs")
	dst := newDatabasePath(t, "compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	const statement = "COMPACT DATABASE"

	compareTableNames(t, dst, wantPath, statement)

	for _, name := range tableNames(t, wantPath) {
		compareColumnsIgnoringIDs(t, dst, wantPath, name, statement)
		compareRows(t, dst, wantPath, name, statement)
		compareIndexes(t, dst, wantPath, name, statement)
	}
}

// compareIndexes requires a table's user indexes to agree in name, covered
// column and leaf contents. The leaf entries are compared whole -- key plus the
// record reference behind it -- rather than by key alone, because a compaction
// that lost a row would otherwise still look right.
func compareIndexes(t *testing.T, gotPath, wantPath, table, statement string) {
	t.Helper()

	got, want := captureIndexes(t, gotPath, table), captureIndexes(t, wantPath, table)

	if len(got) != len(want) {
		t.Fatalf("%s: %q has %d indexes %v, the engine wrote %d %v",
			statement, table, len(got), indexNames(got), len(want), indexNames(want))
	}

	for i := range got {
		g, w := got[i], want[i]

		switch {
		case g.name != w.name:
			t.Errorf("%s: %q index %d is %q, the engine wrote %q", statement, table, i, g.name, w.name)
		case g.column != w.column:
			t.Errorf("%s: %q index %q keys %q, the engine keys %q",
				statement, table, g.name, g.column, w.column)
		case fmt.Sprint(g.entries) != fmt.Sprint(w.entries):
			t.Errorf("%s: %q index %q holds %v, the engine holds %v",
				statement, table, g.name, g.entries, w.entries)
		}
	}
}

// capturedIndex is one user index as a semantic comparison sees it.
type capturedIndex struct {
	name    string
	column  string
	entries []BTreeEntry
}

func indexNames(indexes []capturedIndex) []string {
	names := make([]string, len(indexes))
	for i, idx := range indexes {
		names[i] = idx.name
	}

	return names
}

// captureIndexes reads a table's user indexes: their names and columns from the
// schema stream's index-definition array, their contents from the B-tree each
// one roots.
func captureIndexes(t *testing.T, path, table string) []capturedIndex {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	schemaPageNo, err := tbl.schemaPageNo()
	if err != nil {
		t.Fatalf("schemaPageNo(%q): %v", table, err)
	}

	raw, err := db.readSchemaStream(schemaPageNo)
	if err != nil {
		t.Fatalf("readSchemaStream(%q): %v", table, err)
	}

	records, _, err := schemaTailArrays(raw)
	if err != nil {
		t.Fatalf("parseSchemaTail(%q): %v", table, err)
	}

	if len(records) == 0 {
		return nil
	}

	reader, err := tbl.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex(%q): %v", table, err)
	}

	indexes := make([]capturedIndex, 0, len(records))

	for _, rec := range records {
		entries, err := reader.ScanIndex(int(rec.rootPageNo))
		if err != nil {
			t.Fatalf("ScanIndex(%d) on %q: %v", rec.rootPageNo, table, err)
		}

		column := ""
		if col, ok := rec.singleColumn(); ok {
			column = col.name
		}

		indexes = append(indexes, capturedIndex{name: rec.name, column: column, entries: entries})
	}

	return indexes
}

// TestEngineCompactionRebuildsTheDatabase pins the finding ddl_compact.go rests
// on, so it cannot quietly stop being true. It asserts nothing about this
// package's writes: every claim is about the two engine fixtures read side by
// side.
//
// If this test ever fails, "compaction is a rebuild into a new file" has to be
// revisited -- which is why it is written down as a test and not only as prose.
func TestEngineCompactionRebuildsTheDatabase(t *testing.T) {
	before := describeDatabase(t, requireFixture(t, "MultiTable-drop.abs"))
	after := describeDatabase(t, requireFixture(t, "MultiTable-dropcompact.abs"))

	// The source is what a DROP TABLE left behind: twelve pages free of thirty.
	if before.freePages != 12 {
		t.Errorf("MultiTable-drop.abs has %d free pages, want 12", before.freePages)
	}

	if after.freePages != 0 {
		t.Errorf("the compacted file has %d free pages, want none", after.freePages)
	}

	if after.pageCount >= before.pageCount {
		t.Errorf("compaction went from %d pages to %d; it must shorten the file",
			before.pageCount, after.pageCount)
	}

	if int(after.lastUsedPageNo)+1 != after.pageCount {
		t.Errorf("the compacted file is %d pages with LastUsedPageNo %d; it must end at the last page in use",
			after.pageCount, after.lastUsedPageNo)
	}

	// A continued transaction counter would have to go up. It goes down, which
	// is only possible for a file that was created rather than edited.
	if after.state >= before.state {
		t.Errorf("file State went %d -> %d; compaction must reset it, not continue it",
			before.state, after.state)
	}

	// Object ids are reallocated from scratch: the surviving table's id moves.
	if after.lastObjectID >= before.lastObjectID {
		t.Errorf("LastObjectID went %d -> %d; compaction must reallocate object ids",
			before.lastObjectID, after.lastObjectID)
	}

	srcIDs := tableIDsByName(t, requireFixture(t, "MultiTable-drop.abs"))
	dstIDs := tableIDsByName(t, requireFixture(t, "MultiTable-dropcompact.abs"))

	if dstIDs["Gamma"] == srcIDs["Gamma"] {
		t.Errorf("Gamma kept object id %d across a compaction", dstIDs["Gamma"])
	}
}

// tableIDsByName maps a file's table names to their object ids, which is what
// "compaction reallocates object ids" is asserted over.
func tableIDsByName(t *testing.T, path string) map[string]int {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables(%q): %v", path, err)
	}

	ids := make(map[string]int, len(tables))
	for _, tbl := range tables {
		ids[tbl.Name] = tbl.ID
	}

	return ids
}

// TestCompactDatabaseOfAnEmptyDatabaseIsAFreshOne is what pins
// shrinkToLastUsedPage's floor. A database with no tables uses five pages, and
// the engine's own Create Database still writes six; shortening to five would
// produce a shape no fixture shows. Compacting each of the three Empty fixtures
// must therefore reproduce it exactly, spare page and all.
func TestCompactDatabaseOfAnEmptyDatabaseIsAFreshOne(t *testing.T) {
	for _, fixture := range []string{"Empty.abs", "Empty-p2048-e4.abs", "Empty-mc100.abs"} {
		t.Run(fixture, func(t *testing.T) {
			wantPath := requireFixture(t, fixture)

			want, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatalf("reading %s: %v", fixture, err)
			}

			dst := newDatabasePath(t, "compacted.abs")

			if err := CompactDatabase(wantPath, dst); err != nil {
				t.Fatalf("CompactDatabase: %v", err)
			}

			db, err := Open(dst)
			if err != nil {
				t.Fatalf("Open(%q): %v", dst, err)
			}

			pageSize := db.PageSize()

			db.Close()

			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("reading result: %v", err)
			}

			excluded := pageStateExclusions(
				[]int{systemFileDirPageNo, connectionTablePageNo, freshCatalogPageNo}, pageSize,
			)

			reportPageByteDifferences(t, got, want, "COMPACT DATABASE (no tables)", excluded, pageSize)
		})
	}
}

// TestCompactDatabaseIsIdempotent compacts a file the engine already compacted.
// There is nothing left to reclaim, so the result must be the same file again:
// same length, same pages, same everything but the seeds.
func TestCompactDatabaseIsIdempotent(t *testing.T) {
	wantPath := requireFixture(t, "MultiTable-dropcompact.abs")

	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("reading MultiTable-dropcompact.abs: %v", err)
	}

	dst := newDatabasePath(t, "compacted-again.abs")

	if err := CompactDatabase(wantPath, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	db, err := Open(dst)
	if err != nil {
		t.Fatalf("Open(%q): %v", dst, err)
	}

	pageSize := db.PageSize()

	db.Close()

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	reportPageByteDifferences(t, got, want, "COMPACT DATABASE (already compact)",
		pageStateExclusions(compactedPages, pageSize), pageSize)
}

// TestCompactDatabaseLeavesTheSourceAlone states the property every test here
// depends on and none of them would otherwise check: the source is opened
// read-only and comes out of a compaction byte for byte as it went in.
func TestCompactDatabaseLeavesTheSourceAlone(t *testing.T) {
	src := writableCopy(t, "MultiTable-drop.abs")
	before := fileDigest(t, src)

	if err := CompactDatabase(src, newDatabasePath(t, "compacted.abs")); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	if fileDigest(t, src) != before {
		t.Error("CompactDatabase modified its source file")
	}
}

// TestCompactDatabaseRefusals covers every database the rebuild declines. Each
// one would otherwise be a compaction that silently returned less than it was
// given -- a lost index, a lost constraint, a column written by guesswork -- so
// each is checked to name what it refuses and to leave no file behind.
func TestCompactDatabaseRefusals(t *testing.T) {
	for _, c := range []struct {
		name    string
		fixture string
		want    error
	}{
		{
			name:    "an encrypted database",
			fixture: "Empty-encrypted.abs",
			want:    ErrEncryptionUnsupported,
		},
		{
			name:    "a column type CREATE TABLE cannot write",
			fixture: "MultiTable.abs",
			want:    ErrUnsupportedColumnType,
		},
		{
			name:    "a table carrying constraint records",
			fixture: "Constraints.abs",
			want:    ErrConstraintsNotRebuilt,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := requireFixture(t, c.fixture)
			dst := newDatabasePath(t, "refused.abs")

			err := CompactDatabase(src, dst)
			if err == nil {
				t.Fatalf("CompactDatabase(%s) succeeded, want %v", c.fixture, c.want)
			}

			if !errors.Is(err, c.want) {
				t.Errorf("CompactDatabase(%s) = %v, want %v", c.fixture, err, c.want)
			}

			if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused compaction left a file at %s", dst)
			}
		})
	}
}

// TestCompactDatabaseRefusesAnExistingDestination checks the other half of
// CreateDatabase's exclusive open: a compaction can no more overwrite a
// database than a creation can.
func TestCompactDatabaseRefusesAnExistingDestination(t *testing.T) {
	src := requireFixture(t, "MultiTable-drop.abs")
	dst := newDatabasePath(t, "exists.abs")

	if err := os.WriteFile(dst, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("writing the existing file: %v", err)
	}

	if err := CompactDatabase(src, dst); !errors.Is(err, os.ErrExist) {
		t.Errorf("CompactDatabase onto an existing path = %v, want an os.ErrExist", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading back the existing file: %v", err)
	}

	if string(data) != "keep me" {
		t.Errorf("the existing destination was modified: %q", data)
	}
}

// TestPlanCompactTableRefusalsPerTable runs the plan against one table of
// Constraints.abs at a time. Whole-file compaction stops at the first table it
// cannot rebuild, which is enough to be safe and not enough to show that each
// refusal is the right one; this is the matrix that shows it, one variation per
// table, and it is also what says the two tables the rebuild *can* handle are
// not refused by accident.
//
// CDefault is the one refusal that does not come from the constraint array:
// a DEFAULT lives in the column definition, and serializeColumnDef writes the
// absent marker unconditionally, so rebuilding that table would drop the
// clause instead of failing on it.
func TestPlanCompactTableRefusalsPerTable(t *testing.T) {
	db := openTestFile(t, "Constraints.abs")

	for _, c := range []struct {
		table string
		want  error
	}{
		{"CNone", nil},
		{"CIdxOne", nil},
		{"CDefault", ErrColumnDefault},
		{"CNotNull", ErrConstraintsNotRebuilt},
		{"CPk", ErrConstraintsNotRebuilt},
		{"CUnique", ErrConstraintsNotRebuilt},
		{"CMinMax", ErrConstraintsNotRebuilt},
		{"CBoth", ErrConstraintsNotRebuilt},
		{"CPkMulti", ErrConstraintsNotRebuilt},
		{"CIdxDesc", ErrIndexNotMaintained},
		{"CIdxNoCase", ErrIndexNotMaintained},
		{"CIdxMulti", ErrMultiColumnIndex},
	} {
		t.Run(c.table, func(t *testing.T) {
			tbl, err := db.Table(c.table)
			if err != nil {
				t.Fatalf("Table(%q): %v", c.table, err)
			}

			_, err = planCompactTable(db, tbl)

			switch {
			case c.want == nil && err != nil:
				t.Errorf("planCompactTable(%q) = %v, want it to be rebuildable", c.table, err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("planCompactTable(%q) = %v, want %v", c.table, err, c.want)
			}
		})
	}
}

// TestPlanCompactIndexesRefusesAStringKey is the one index refusal no fixture
// can reach through a whole-file compaction: CreateIndex builds only Int32
// keys, so the corpus holds no plain single-column index over a string that
// this package could also have created. The record is built by hand instead,
// because losing an index is not an acceptable outcome of a compaction and the
// refusal has to be pinned somewhere.
func TestPlanCompactIndexesRefusesAStringKey(t *testing.T) {
	schema := &TableSchema{Columns: []Column{
		{Name: "S", BaseType: BftVarchar, FieldType: FieldString, Size: 20},
	}}

	records := []indexRecord{{
		name:    "IdxS",
		columns: []indexColumn{{name: "S", maxIndexedSize: indexColumnMaxIndexedSize}},
	}}

	_, err := planCompactIndexes(schema, records)
	if !errors.Is(err, ErrUnsupportedIndexColumn) {
		t.Errorf("planCompactIndexes over a string column = %v, want %v", err, ErrUnsupportedIndexColumn)
	}
}

// TestMaxConnectionsReadsThePageThreeInternalFile checks the reverse of what
// CreateDatabase writes, since a compaction has to carry the setting across and
// no header field records it.
func TestMaxConnectionsReadsThePageThreeInternalFile(t *testing.T) {
	for _, c := range []struct {
		fixture string
		want    int
	}{
		{"Empty.abs", defaultMaxConnections},
		{"Empty-mc100.abs", 100},
		{"Empty-p2048-e4.abs", defaultMaxConnections},
		{"MultiTable-dropcompact.abs", defaultMaxConnections},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			db := openTestFile(t, c.fixture)

			got, err := db.maxConnections()
			if err != nil {
				t.Fatalf("maxConnections: %v", err)
			}

			if got != c.want {
				t.Errorf("%s reports %d connections, want %d", c.fixture, got, c.want)
			}
		})
	}
}

// TestCompactDatabasePreservesTheGeometry compacts the 2048-byte fixture and
// the 100-connection one, and requires the result to carry the same geometry.
// Page size and extent size are header fields a wrong implementation would
// obviously lose; the connection count is the one that could quietly fall back
// to the default, since it lives in a page nothing else reads.
func TestCompactDatabasePreservesTheGeometry(t *testing.T) {
	for _, fixture := range []string{"Empty-p2048-e4-grow.abs", "Empty-mc100.abs"} {
		t.Run(fixture, func(t *testing.T) {
			src := openTestFile(t, fixture)

			wantPageSize, wantExtent := src.PageSize(), src.pagesInExtent

			wantConnections, err := src.maxConnections()
			if err != nil {
				t.Fatalf("maxConnections: %v", err)
			}

			dst := newDatabasePath(t, "compacted.abs")

			if err := CompactDatabase(requireFixture(t, fixture), dst); err != nil {
				t.Fatalf("CompactDatabase: %v", err)
			}

			db, err := Open(dst)
			if err != nil {
				t.Fatalf("Open(%q): %v", dst, err)
			}

			defer db.Close()

			gotConnections, err := db.maxConnections()
			if err != nil {
				t.Fatalf("maxConnections: %v", err)
			}

			switch {
			case db.PageSize() != wantPageSize:
				t.Errorf("page size is %d, want %d", db.PageSize(), wantPageSize)
			case db.pagesInExtent != wantExtent:
				t.Errorf("extent is %d pages, want %d", db.pagesInExtent, wantExtent)
			case gotConnections != wantConnections:
				t.Errorf("connection table holds %d, want %d", gotConnections, wantConnections)
			}
		})
	}
}

// TestShrinkToLastUsedPageRefusesOnAReadOnlyHandle keeps the one destructive
// step of a compaction behind the same gate as every other write.
func TestShrinkToLastUsedPageRefusesOnAReadOnlyHandle(t *testing.T) {
	db := openTestFile(t, "MultiTable-drop.abs")

	if err := db.shrinkToLastUsedPage(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("shrinkToLastUsedPage on a read-only handle = %v, want %v", err, ErrReadOnly)
	}
}

// TestShrinkToLastUsedPageKeepsFreeTrailingPages checks the truncation in
// isolation, on a file whose trailing pages a DROP TABLE freed: it must remove
// exactly the run past LastUsedPageNo, leave every surviving page byte for byte
// as it was, and be a no-op the second time.
func TestShrinkToLastUsedPageKeepsFreeTrailingPages(t *testing.T) {
	path := writableCopy(t, "MultiTable-drop.abs")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	last := int(db.lastUsedPageNo)
	pageSize := db.PageSize()

	if err := db.shrinkToLastUsedPage(); err != nil {
		t.Fatalf("shrinkToLastUsedPage: %v", err)
	}

	if db.PageCount() != last+1 {
		t.Errorf("shortened to %d pages, want %d", db.PageCount(), last+1)
	}

	// A second call has nothing to do.
	if err := db.shrinkToLastUsedPage(); err != nil {
		t.Fatalf("second shrinkToLastUsedPage: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the shortened file: %v", err)
	}

	wantSize := (last+1)*pageSize + diskPageHeaderOffset
	if len(after) != wantSize {
		t.Fatalf("the shortened file is %d bytes, want %d", len(after), wantSize)
	}

	// Everything but TotalPageCount survives untouched.
	patched := append([]byte(nil), before[:wantSize]...)
	copy(patched[totalPageCountOffset:totalPageCountOffset+4],
		after[totalPageCountOffset:totalPageCountOffset+4])

	if !bytes.Equal(after, patched) {
		t.Error("shortening the file changed a byte other than TotalPageCount")
	}
}
