package absdb

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func testdataPath(name string) string {
	return filepath.Join("testdata", name)
}

// requireFixture returns the path of a fixture in testdata/, skipping the test
// when it is not there.
//
// testdata/ mostly holds real customer data and is deliberately not committed,
// so on a fresh clone the fixtures are simply absent and the tests that need
// them have nothing to say. (The eight Employees-* fixtures are the exception:
// they were generated with the ComponentAce DB Manager and contain no customer
// data, but they live under the same ignored directory.) Every path that
// reaches a fixture — Open, ReadFile, OpenWithPassword — should go through
// here, so that a fresh clone skips cleanly instead of reporting dozens of
// failures. Any error other than a missing file is still a hard failure.
func requireFixture(t *testing.T, name string) string {
	t.Helper()

	path := testdataPath(name)

	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("fixture %s not present (testdata/ is not committed)", name)
	}

	return path
}

// openTestFile opens a fixture from testdata/, skipping when it is absent.
func openTestFile(t *testing.T, name string) *File {
	t.Helper()

	path := requireFixture(t, name)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

func TestOpenInvalidFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.abs")

	err := os.WriteFile(tmp, []byte("not a database file!!"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Open(tmp)
	if err == nil {
		t.Fatal("expected error for invalid file")
	}
}

func TestOpenNonExistent(t *testing.T) {
	_, err := Open("/nonexistent/path.abs")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestOpenTruncated(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "trunc.abs")
	data := make([]byte, 20)
	copy(data, Magic[:])

	err := os.WriteFile(tmp, data, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Open(tmp)
	if err == nil {
		t.Fatal("expected error for truncated file")
	}
}

func TestTS03Header(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	if v := db.Version(); v != 5.13 {
		t.Errorf("Version() = %v, want 5.13", v)
	}

	if ps := db.PageSize(); ps != 4096 {
		t.Errorf("PageSize() = %d, want 4096", ps)
	}

	if pc := db.PageCount(); pc != 14 {
		t.Errorf("PageCount() = %d, want 14", pc)
	}

	if db.Encrypted() {
		t.Error("Encrypted() = true, want false")
	}
}

func TestAddressesHeader(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	if v := db.Version(); v != 7.94 {
		t.Errorf("Version() = %v, want 7.94", v)
	}

	if ps := db.PageSize(); ps != 4096 {
		t.Errorf("PageSize() = %d, want 4096", ps)
	}
}

func TestRREC0011Header(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	if v := db.Version(); v != 7.61 {
		t.Errorf("Version() = %v, want 7.61", v)
	}

	if ps := db.PageSize(); ps != 4096 {
		t.Errorf("PageSize() = %d, want 4096", ps)
	}
}

func TestReadPage(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	p, err := db.ReadPage(0)
	if err != nil {
		t.Fatalf("ReadPage(0): %v", err)
	}

	if p.Number != 0 {
		t.Errorf("page.Number = %d, want 0", p.Number)
	}

	if len(p.Data) != 4096 {
		t.Errorf("len(page.Data) = %d, want 4096", len(p.Data))
	}

	var magic [16]byte
	copy(magic[:], p.Data[:16])

	if magic != Magic {
		t.Error("page 0 does not contain magic bytes")
	}
}

func TestReadPageOutOfRange(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	_, err := db.ReadPage(-1)
	if !errors.Is(err, ErrPageOutOfRange) {
		t.Errorf("ReadPage(-1) error = %v, want ErrPageOutOfRange", err)
	}

	_, err = db.ReadPage(db.PageCount())
	if !errors.Is(err, ErrPageOutOfRange) {
		t.Errorf("ReadPage(PageCount) error = %v, want ErrPageOutOfRange", err)
	}
}

func TestDiskPageHeader(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	p, err := db.ReadPage(0)
	if err != nil {
		t.Fatal(err)
	}

	if p.Header == nil {
		t.Fatal("page 0 should have disk page header")
	}

	if p.Header.PageType != PageTypeFileHdr {
		t.Errorf("PageType = %d, want %d (FileHdr)", p.Header.PageType, PageTypeFileHdr)
	}

	if p.Header.NextPageNo != -1 {
		t.Errorf("NextPageNo = %d, want -1", p.Header.NextPageNo)
	}
}

func TestDiskPageHeaderTypes(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	// Expected page types for TS03.abs.
	expectedTypes := map[int]uint16{
		0:  PageTypeFileHdr,
		1:  PageTypeSystemDir,
		7:  PageTypeSchema,
		13: PageTypeData,
		9:  PageTypeIndex,
	}

	for pageNo, wantType := range expectedTypes {
		p, err := db.ReadPage(pageNo)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", pageNo, err)
		}

		if p.Header == nil {
			t.Fatalf("page %d has no header", pageNo)
		}

		if p.Header.PageType != wantType {
			t.Errorf("page %d: PageType = %d, want %d", pageNo, p.Header.PageType, wantType)
		}
	}
}

func TestDataPageObjectID(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	p, err := db.ReadPage(13)
	if err != nil {
		t.Fatal(err)
	}

	if p.Header.ObjectID != 1 {
		t.Errorf("data page ObjectID = %d, want 1", p.Header.ObjectID)
	}
}

func TestLinkedPages(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	// Pages 5 and 6 are linked (both type 7).
	p5, err := db.ReadPage(5)
	if err != nil {
		t.Fatal(err)
	}

	if p5.Header.NextPageNo != 6 {
		t.Errorf("page 5 NextPageNo = %d, want 6", p5.Header.NextPageNo)
	}

	p6, err := db.ReadPage(6)
	if err != nil {
		t.Fatal(err)
	}

	if p6.Header.NextPageNo != -1 {
		t.Errorf("page 6 NextPageNo = %d, want -1", p6.Header.NextPageNo)
	}
}

func TestScanPages(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	summaries, err := db.ScanPages()
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != db.PageCount() {
		t.Fatalf("got %d summaries, want %d", len(summaries), db.PageCount())
	}

	for _, s := range summaries {
		if s.Header != nil {
			t.Logf("Page %2d: type=%2d, next=%3d, obj=%3d",
				s.Number, s.Header.PageType, s.Header.NextPageNo, s.Header.ObjectID)
		} else {
			t.Logf("Page %2d: no header", s.Number)
		}
	}
}

// craftFile copies a fixture, applies patch to the raw bytes and writes the
// result to a temporary file whose path is returned. Like openTestFile it
// skips when the fixture is absent, since testdata/ is not committed.
func craftFile(t *testing.T, fixture string, patch func([]byte) []byte) string {
	t.Helper()

	data, err := os.ReadFile(requireFixture(t, fixture))
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "crafted.abs")

	err = os.WriteFile(path, patch(data), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// putPageCount overwrites TotalPageCount at offset 30 of the TABSDBHeader.
func putPageCount(data []byte, count int32) []byte {
	binary.LittleEndian.PutUint32(data[30:34], uint32(count))

	return data
}

func TestPagePayloadSize(t *testing.T) {
	for _, name := range []string{"TS03.abs", "RREC0011.abs", "Addresses.abs"} {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			want := db.PageSize() - diskPageHeaderSize

			for n := range db.PageCount() {
				p, err := db.ReadPage(n)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", n, err)
				}

				if len(p.Data) != db.PageSize() {
					t.Fatalf("page %d: len(Data) = %d, want %d", n, len(p.Data), db.PageSize())
				}

				if got := len(p.PageData()); got != want {
					t.Fatalf("page %d: len(PageData()) = %d, want %d", n, got, want)
				}
			}
		})
	}
}

func TestReadLastPage(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	last := db.PageCount() - 1

	p, err := db.ReadPage(last)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", last, err)
	}

	if len(p.PageData()) != db.PageSize()-diskPageHeaderSize {
		t.Errorf("last page: len(PageData()) = %d, want %d",
			len(p.PageData()), db.PageSize()-diskPageHeaderSize)
	}
}

// TestFileSizeMatchesPageModel documents the invariant the payload model rests
// on: the file holds pageCount full pages plus the trailing bytes that complete
// the last page's payload.
func TestFileSizeMatchesPageModel(t *testing.T) {
	for _, name := range []string{"TS03.abs", "RREC0011.abs", "Addresses.abs"} {
		db := openTestFile(t, name)

		want := int64(db.PageCount())*int64(db.PageSize()) + diskPageHeaderOffset
		if db.size != want {
			t.Errorf("%s: size = %d, want %d", name, db.size, want)
		}
	}
}

func TestOpenAbsurdPageCount(t *testing.T) {
	tests := []struct {
		name  string
		count int32
	}{
		{"huge", 1 << 30},
		{"max", math.MaxInt32},
		{"negative", -1},
		{"min", math.MinInt32},
		{"one past end", 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := craftFile(t, "TS03.abs", func(data []byte) []byte {
				return putPageCount(data, tt.count)
			})

			db, err := Open(path)
			if err == nil {
				db.Close()
				t.Fatalf("Open with totalPageCount %d succeeded, want error", tt.count)
			}
		})
	}
}

// TestOpenAbsurdPageSize guards the payload slice in ReadPage: a page smaller
// than its own disk page header has no well-defined payload.
func TestOpenAbsurdPageSize(t *testing.T) {
	for _, ps := range []uint16{1, 16, 64, 379, 419} {
		path := craftFile(t, "TS03.abs", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[26:28], ps)

			return data
		})

		db, err := Open(path)
		if err == nil {
			db.Close()
			t.Errorf("Open with pageSize %d succeeded, want error", ps)
		}
	}
}

// TestOpenMissingTrailingPayload rejects a file that stops after the last full
// block: the final page's payload would be incomplete.
func TestOpenMissingTrailingPayload(t *testing.T) {
	path := craftFile(t, "TS03.abs", func(data []byte) []byte {
		return data[:len(data)-1]
	})

	_, err := Open(path)
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("Open truncated file error = %v, want ErrTruncated", err)
	}
}

func TestPageIsEmpty(t *testing.T) {
	p := Page{Data: make([]byte, 100)}
	if !p.IsEmpty() {
		t.Error("zero page should be empty")
	}

	p.Data[50] = 1
	if p.IsEmpty() {
		t.Error("non-zero page should not be empty")
	}
}
