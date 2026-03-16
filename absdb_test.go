package absdb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testdataPath(name string) string {
	return filepath.Join("testdata", name)
}

func openTestFile(t *testing.T, name string) *File {
	t.Helper()

	path := testdataPath(name)

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

	if v := db.Version(); v != 7.1 {
		t.Errorf("Version() = %v, want 7.1", v)
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
