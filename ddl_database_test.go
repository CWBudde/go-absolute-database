package absdb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pageStateExclusions is the set of byte offsets a byte-identity test is
// allowed to skip: the four-byte State word of each named page. A newly
// allocated page's State is a random seed the engine picks and no writer can
// reproduce (FINDING 1, ddl.go), and it is the only such value in the format,
// so every exclusion in this package is built here and nowhere else.
func pageStateExclusions(pages []int, pageSize int) map[int]bool {
	excluded := make(map[int]bool, len(pages)*4)

	for _, no := range pages {
		start := no*pageSize + pageStateOffset
		for i := range 4 {
			excluded[start+i] = true
		}
	}

	return excluded
}

// newDatabasePath returns a path in a fresh temporary directory for a database
// that does not exist yet. CreateDatabase creates its file exclusively, so it
// must never be handed a path under testdata/ -- everything here writes into
// t.TempDir().
func newDatabasePath(t *testing.T, name string) string {
	t.Helper()

	return filepath.Join(t.TempDir(), name)
}

// TestCreateDatabaseMatchesEngineByteForByte holds CreateDatabase to the same
// standard as every other write in this package: reproduce, byte for byte, the
// file the engine itself wrote.
//
// It excludes exactly twelve bytes -- the State words of pages 2, 3 and 4, the
// three the engine seeds randomly (see ddl_database.go). Pages 0 and 1 are
// deliberately not excluded: their States are counters, one per Page Free Space
// bit and one per Extent Allocation Map entry changed, and reproducing them is
// most of what this test is for. Excluding them would hide the difference
// between allocating the five pages one at a time and allocating them in one
// batch, which is the only thing the 2048-byte case measures that the 4096-byte
// one does not.
//
// The third case adds nothing to the geometry and everything to the claim that
// Max Connections is not a header field: Empty-mc100.abs and Empty.abs have
// identical headers and differ only in the Size field of page 3's internal
// file, so a CreateDatabase that put the value anywhere else would fail here
// and nowhere else.
func TestCreateDatabaseMatchesEngineByteForByte(t *testing.T) {
	for _, c := range []struct {
		fixture  string
		settings string
		opts     CreateDatabaseOptions
	}{
		{
			fixture:  "Empty.abs",
			settings: "CREATE DATABASE (defaults)",
		},
		{
			fixture:  "Empty-p2048-e4.abs",
			settings: "CREATE DATABASE (PageSize 2048, PageCountInExtent 4)",
			opts:     CreateDatabaseOptions{PageSize: 2048, PageCountInExtent: 4},
		},
		{
			fixture:  "Empty-mc100.abs",
			settings: "CREATE DATABASE (Max Connections 100)",
			opts:     CreateDatabaseOptions{MaxConnections: 100},
		},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			want, err := os.ReadFile(requireFixture(t, c.fixture))
			if err != nil {
				t.Fatalf("reading %s: %v", c.fixture, err)
			}

			path := newDatabasePath(t, "created.abs")

			db, err := CreateDatabase(path, c.opts)
			if err != nil {
				t.Fatalf("CreateDatabase: %v", err)
			}

			pageSize := db.PageSize()

			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading result: %v", err)
			}

			excluded := pageStateExclusions(
				[]int{systemFileDirPageNo, connectionTablePageNo, freshCatalogPageNo}, pageSize,
			)

			reportPageByteDifferences(t, got, want, c.settings, excluded, pageSize)
		})
	}
}

// TestCreateDatabaseThenCreateTableMatchesTheEngine chains the two halves of
// Phase 8's file lifecycle: a database created from nothing, then a table
// created in it, held against the engine's own file for the same two steps.
//
// Empty-p2048-e4-grow.abs is a CREATE TABLE on Empty-p2048-e4.abs, so
// ddl_grow_test.go already pins the second half starting from the engine's own
// fresh database. Starting from this package's instead is what makes the two
// halves meet: nine of the fourteen pages are written by CreateDatabase and
// CreateTable in turn, and a fresh database that was subtly wrong in a way
// Empty-p2048-e4.abs alone could not show -- a State counter that happened to
// match at six pages, say -- would surface here.
func TestCreateDatabaseThenCreateTableMatchesTheEngine(t *testing.T) {
	want, err := os.ReadFile(requireFixture(t, "Empty-p2048-e4-grow.abs"))
	if err != nil {
		t.Fatalf("reading Empty-p2048-e4-grow.abs: %v", err)
	}

	path := newDatabasePath(t, "created.abs")

	db, err := CreateDatabase(path, CreateDatabaseOptions{PageSize: 2048, PageCountInExtent: 4})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	err = db.CreateTable("T1", []Column{
		{Name: "X", BaseType: BftInt32, FieldType: FieldInteger},
		{Name: "Y", BaseType: BftInt32, FieldType: FieldInteger},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	pageSize := db.PageSize()

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	// The three pages CreateDatabase seeds, then the six CreateTable allocates
	// (three for the system internal file at a 2048-byte page size, plus
	// schema, info and index).
	excluded := pageStateExclusions([]int{2, 3, 4, 5, 6, 7, 8, 9, 10}, pageSize)

	reportPageByteDifferences(t,
		got, want, "CREATE DATABASE 2048/4 + CREATE TABLE T1 (X INTEGER, Y INTEGER)", excluded, pageSize)
}

// TestEngineFreshDatabaseIsSixPages pins the shape ddl_database.go's comment
// rests on, straight off the engine's own fixtures, so the reasoning cannot go
// stale. It asserts nothing about this package's writes.
func TestEngineFreshDatabaseIsSixPages(t *testing.T) {
	for _, fixture := range []string{"Empty.abs", "Empty-p2048-e4.abs", "Empty-mc100.abs"} {
		t.Run(fixture, func(t *testing.T) {
			db := openTestFile(t, fixture)

			if db.PageCount() != freshDatabasePageCount {
				t.Errorf("%s has %d pages, want %d", fixture, db.PageCount(), freshDatabasePageCount)
			}

			if db.lastUsedPageNo != freshLastUsedPageNo {
				t.Errorf("LastUsedPageNo is %d, want %d", db.lastUsedPageNo, freshLastUsedPageNo)
			}

			if db.state != freshDatabaseState {
				t.Errorf("State is %d, want %d", db.state, freshDatabaseState)
			}

			if db.lastObjectID != 0 {
				t.Errorf("LastObjectID is %d, want 0", db.lastObjectID)
			}

			wantTypes := []uint16{
				PageTypeFileHdr, PageTypeSystemDir,
				pageTypeSystemFileDir, pageTypeConnectionTable, PageTypeTableList,
			}

			for no, wantType := range wantTypes {
				page, err := db.ReadPage(no)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", no, err)
				}

				if page.Header == nil || page.Header.PageType != wantType {
					t.Errorf("page %d is type %v, want %d", no, page.Header, wantType)
				}
			}

			// The sixth page exists and is free: a fresh database is not
			// rounded up to a whole extent, and it is not five pages either.
			last, err := db.ReadPage(freshDatabasePageCount - 1)
			if err != nil {
				t.Fatalf("ReadPage(%d): %v", freshDatabasePageCount-1, err)
			}

			if !last.IsEmpty() {
				t.Errorf("page %d is not empty; a fresh database ends in one free page",
					freshDatabasePageCount-1)
			}
		})
	}
}

// TestSystemFileDirectoryIsTheSameInEveryDatabase pins the claim that page 2 is
// a constant: three databases made at different times, one of them by the
// engine's own compaction, all carry the same twenty bytes -- and so does what
// buildSystemDirectory writes.
func TestSystemFileDirectoryIsTheSameInEveryDatabase(t *testing.T) {
	want, err := compressInternalFile(
		buildSystemDirectory(connectionTablePageNo, freshCatalogPageNo), 0,
	)
	if err != nil {
		t.Fatalf("building the system directory: %v", err)
	}

	for _, fixture := range []string{
		"Empty.abs", "MultiTable-createidx.abs", "MultiTable-dropcompact.abs",
	} {
		t.Run(fixture, func(t *testing.T) {
			db := openTestFile(t, fixture)

			page, err := db.ReadPage(systemFileDirPageNo)
			if err != nil {
				t.Fatalf("ReadPage(%d): %v", systemFileDirPageNo, err)
			}

			if page.Header == nil || page.Header.PageType != pageTypeSystemFileDir {
				t.Fatalf("page %d is not the system file directory", systemFileDirPageNo)
			}

			if got := page.PageData()[:len(want)]; !bytes.Equal(got, want) {
				t.Errorf("page %d holds % x, want % x", systemFileDirPageNo, got, want)
			}
		})
	}
}

// TestCreateDatabaseReadsBack is the complement to byte identity: a database
// created from nothing has to work as a database. It holds no tables, it is
// writable, a table can be created in it, and rows can be inserted and read
// back out.
func TestCreateDatabaseReadsBack(t *testing.T) {
	path := newDatabasePath(t, "created.abs")

	db, err := CreateDatabase(path, CreateDatabaseOptions{})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	defer db.Close()

	if !db.Writable() {
		t.Error("CreateDatabase returned a read-only handle")
	}

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}

	if len(tables) != 0 {
		t.Errorf("a fresh database holds %d tables, want none", len(tables))
	}

	err = db.CreateTable("T", []Column{
		{Name: "X", BaseType: BftInt32, FieldType: FieldInteger},
		{Name: "S", BaseType: BftVarchar, FieldType: FieldString, Size: 8},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	table, err := db.Table("T")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	writer, err := table.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	if _, err := writer.Insert([]any{int32(7), "seven"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := writer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rows := captureRows(t, path, "T")
	if len(rows) != 1 {
		t.Fatalf("read back %d rows, want 1", len(rows))
	}

	if got := rows[0]; len(got) != 2 || got[0] != int64(7) || got[1] != "seven" {
		t.Errorf("read back %v, want [7 seven]", got)
	}
}

// TestCreateDatabaseRefusals covers every request CreateDatabase declines, so
// that each stays a named error rather than becoming a file nobody can open.
func TestCreateDatabaseRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		opts CreateDatabaseOptions
		want error
	}{
		{
			name: "encrypted",
			opts: CreateDatabaseOptions{Encrypted: true},
			want: ErrEncryptionUnsupported,
		},
		{
			name: "page size below the disk page header",
			opts: CreateDatabaseOptions{PageSize: 256},
			want: ErrBadGeometry,
		},
		{
			name: "page size past the header field",
			opts: CreateDatabaseOptions{PageSize: 1 << 17},
			want: ErrBadGeometry,
		},
		{
			name: "negative extent",
			opts: CreateDatabaseOptions{PageCountInExtent: -1},
			want: ErrBadGeometry,
		},
		{
			name: "negative connection count",
			opts: CreateDatabaseOptions{MaxConnections: -1},
			want: ErrBadGeometry,
		},
		{
			name: "connection table larger than a page",
			opts: CreateDatabaseOptions{PageSize: 1024, MaxConnections: 4000},
			want: ErrBadGeometry,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := newDatabasePath(t, "refused.abs")

			db, err := CreateDatabase(path, c.opts)
			if err == nil {
				db.Close()
				t.Fatalf("CreateDatabase(%+v) succeeded, want %v", c.opts, c.want)
			}

			if !errors.Is(err, c.want) {
				t.Errorf("CreateDatabase(%+v) = %v, want %v", c.opts, err, c.want)
			}

			// A refused request must leave nothing behind.
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused CreateDatabase left a file at %s", path)
			}
		})
	}
}

// TestCreateDatabaseRefusesAnExistingPath is the refusal that protects data
// rather than correctness: CreateDatabase opens its file exclusively, so it can
// never overwrite a database that is already there.
func TestCreateDatabaseRefusesAnExistingPath(t *testing.T) {
	path := newDatabasePath(t, "exists.abs")

	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("writing the existing file: %v", err)
	}

	db, err := CreateDatabase(path, CreateDatabaseOptions{})
	if err == nil {
		db.Close()
		t.Fatal("CreateDatabase overwrote an existing file")
	}

	if !errors.Is(err, os.ErrExist) {
		t.Errorf("CreateDatabase on an existing path = %v, want an os.ErrExist", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back the existing file: %v", err)
	}

	if string(data) != "not a database" {
		t.Errorf("the existing file was modified: %q", data)
	}
}

// TestBuildZeroInternalFileLeavesDecompressedSizeAtZero pins the one asymmetry
// the connection table and a fresh table's system internal file share, and that
// compressInternalFile does not have: Size is written, DecompressedSize is not.
// Both fixtures that measured it are checked against the builder here.
func TestBuildZeroInternalFileLeavesDecompressedSizeAtZero(t *testing.T) {
	got := buildZeroInternalFile(defaultMaxConnections)

	if len(got) != internalFileHeaderSize+defaultMaxConnections {
		t.Fatalf("built %d bytes, want %d", len(got), internalFileHeaderSize+defaultMaxConnections)
	}

	// Header: size 10, Size = 500, DecompressedSize = 0, algorithm 0.
	want := []byte{internalFileHeaderSize, 0xF4, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0}
	if !bytes.Equal(got[:internalFileHeaderSize], want) {
		t.Errorf("header is % x, want % x", got[:internalFileHeaderSize], want)
	}

	if !isZero(got[internalFileHeaderSize:]) {
		t.Error("the connection table's payload is not all zero")
	}

	db := openTestFile(t, "Empty.abs")

	page, err := db.ReadPage(connectionTablePageNo)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", connectionTablePageNo, err)
	}

	if fixture := page.PageData()[:internalFileHeaderSize]; !bytes.Equal(fixture, want) {
		t.Errorf("Empty.abs page %d header is % x, want % x", connectionTablePageNo, fixture, want)
	}
}
