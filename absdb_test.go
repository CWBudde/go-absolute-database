package absdb

import (
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
	// Create a temporary file with invalid content.
	tmp := filepath.Join(t.TempDir(), "bad.abs")
	if err := os.WriteFile(tmp, []byte("not a database file!!"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(tmp)
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
	// Write valid magic but truncate before page size.
	data := make([]byte, 20)
	copy(data, Magic[:])
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(tmp)
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
	if m := db.Mode(); m != 'L' {
		t.Errorf("Mode() = %c, want L", m)
	}
	if tc := db.TotalColumnCount(); tc != 14 {
		t.Errorf("TotalColumnCount() = %d, want 14", tc)
	}
	if uc := db.UserColumnCount(); uc != 13 {
		t.Errorf("UserColumnCount() = %d, want 13", uc)
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
	if uc := db.UserColumnCount(); uc != 12 {
		t.Errorf("UserColumnCount() = %d, want 12", uc)
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
	if uc := db.UserColumnCount(); uc != 12 {
		t.Errorf("UserColumnCount() = %d, want 12", uc)
	}
}

func TestPageCount(t *testing.T) {
	db := openTestFile(t, "TS03.abs")
	pc := db.PageCount()
	// 57724 bytes / 4096 = 14 pages (with remainder, so 14)
	if pc != 14 {
		t.Errorf("PageCount() = %d, want 14", pc)
	}
}

func TestReadPage(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	// Page 0 should be readable and contain the magic.
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
	// Verify magic in raw data.
	var magic [16]byte
	copy(magic[:], p.Data[:16])
	if magic != Magic {
		t.Error("page 0 does not contain magic bytes")
	}
}

func TestReadPageOutOfRange(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	_, err := db.ReadPage(-1)
	if err != ErrPageOutOfRange {
		t.Errorf("ReadPage(-1) error = %v, want ErrPageOutOfRange", err)
	}

	_, err = db.ReadPage(db.PageCount())
	if err != ErrPageOutOfRange {
		t.Errorf("ReadPage(PageCount) error = %v, want ErrPageOutOfRange", err)
	}
}

func TestPage0ABSP(t *testing.T) {
	db := openTestFile(t, "TS03.abs")
	p, err := db.ReadPage(0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ABSP == nil {
		t.Fatal("page 0 should have ABSP marker")
	}
	if p.ABSP.Offset != page0ABSPOffset {
		t.Errorf("ABSP offset = 0x%X, want 0x%X", p.ABSP.Offset, page0ABSPOffset)
	}
	// On page 0, the checksum field equals the total column count (14).
	if p.ABSP.Checksum != 14 {
		t.Errorf("ABSP.Checksum = %d, want 14 (column count on page 0)", p.ABSP.Checksum)
	}
	if p.ABSP.PageType != 3 {
		t.Errorf("ABSP.PageType = %d, want 3", p.ABSP.PageType)
	}
}

func TestClassifyPages(t *testing.T) {
	db := openTestFile(t, "TS03.abs")
	summaries, err := db.ScanPages()
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != db.PageCount() {
		t.Fatalf("got %d summaries, want %d", len(summaries), db.PageCount())
	}

	// Page 0 should be file-header.
	if summaries[0].Classification != PageFileHeader {
		t.Errorf("page 0 classification = %v, want file-header", summaries[0].Classification)
	}

	// Count page types for debugging.
	counts := map[PageClassification]int{}
	for _, s := range summaries {
		counts[s.Classification]++
	}
	t.Logf("Page classifications: %v", counts)

	// Log ABSP pages with their types.
	for _, s := range summaries {
		if s.ABSP != nil {
			t.Logf("Page %2d: ABSP at 0x%03X, checksum=0x%08X, type=%d, class=%v",
				s.Number, s.ABSP.Offset, s.ABSP.Checksum, s.ABSP.PageType, s.Classification)
		} else {
			t.Logf("Page %2d: %v", s.Number, s.Classification)
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

func TestPageClassificationString(t *testing.T) {
	tests := []struct {
		c    PageClassification
		want string
	}{
		{PageEmpty, "empty"},
		{PageFileHeader, "file-header"},
		{PageABSP, "absp"},
		{PageUnknown, "unknown"},
	}
	for _, tt := range tests {
		if got := tt.c.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", int(tt.c), got, tt.want)
		}
	}
}
