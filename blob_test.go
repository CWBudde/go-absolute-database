package absdb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

// testPageSize is the page size of every fixture used by the crafted-input
// tests below. The offsets they patch assume it.
const testPageSize = 4096

// rpdgBlobSizes is the measured BLOB size of every type-11 page in
// RPDG0011.abs, read straight from the three int64 values at the start of each
// page payload. All 60 are stored uncompressed (CompressedSize ==
// UncompressedSize) and every size is a multiple of 12: the payload is a run of
// float32 triplets.
//
// Pages 66..69 hold 3816 bytes. That is more than the 3676-24 = 3652 bytes the
// old (wrong) page model made available, which is why those four BLOBs used to
// come back truncated, and less than the 4056-24 = 4032 bytes the corrected
// model provides.
var rpdgBlobSizes = map[int32]int{
	13: 2484, 14: 2484, 16: 2484, 17: 2484,
	18: 3156, 19: 3156, 20: 3156, 21: 3156,
	22: 3168, 23: 3168,
	24: 3132, 25: 3132,
	26: 2988, 27: 2988, 28: 2988, 29: 2988,
	30: 3072, 31: 3072, 32: 3072, 33: 3072,
	34: 2988, 35: 2988,
	36: 3012, 37: 3012, 38: 3012, 39: 3012,
	40: 2940, 41: 2940,
	42: 2880, 43: 2880, 44: 2880, 45: 2880, 46: 2880, 47: 2880,
	48: 3132, 49: 3132, 50: 3132, 51: 3132,
	52: 3072, 53: 3072, 54: 3072, 55: 3072, 56: 3072, 57: 3072,
	58: 3264, 59: 3264, 60: 3264, 61: 3264,
	62: 3456, 63: 3456, 64: 3456, 65: 3456,
	66: 3816, 67: 3816, 68: 3816, 69: 3816,
	70: 3132, 71: 3132, 72: 3132, 73: 3132,
}

// blobColumns returns the indexes of the reader's BLOB columns.
func blobColumns(r *Reader) []int {
	var cols []int

	for i, c := range r.Schema().Columns {
		if c.IsBLOB() {
			cols = append(cols, i)
		}
	}

	return cols
}

// TestRPDG0011ReadBlobs checks every BLOB of every record, not just the first
// one: the four BLOBs that the old page model truncated all sit on later
// records and slipped through a first-record-only assertion.
func TestRPDG0011ReadBlobs(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	blobCols := blobColumns(reader)
	if len(blobCols) == 0 {
		t.Fatal("expected BLOB columns in RPDG0011")
	}

	records := 0
	blobs := 0
	seenPages := map[int32]bool{}

	for reader.Next() {
		rec := reader.Record()
		records++

		for _, col := range blobCols {
			if rec.IsNull(col) {
				continue
			}

			ref := rec.BlobRef(col)
			if ref.IsNull() {
				continue
			}

			data, err := rec.Blob(col)
			if err != nil {
				t.Fatalf("record %d, col %d (%s): Blob(): %v",
					records, col, reader.Schema().Columns[col].Name, err)
			}

			want, ok := rpdgBlobSizes[ref.PageNo]
			if !ok {
				t.Errorf("record %d, col %d: unexpected BLOB page %d", records, col, ref.PageNo)

				continue
			}

			if len(data) != want {
				t.Errorf("record %d, col %d: BLOB on page %d is %d bytes, want %d",
					records, col, ref.PageNo, len(data), want)
			}

			if len(data)%12 != 0 {
				t.Errorf("record %d, col %d: BLOB on page %d is %d bytes, not a multiple of 12",
					records, col, ref.PageNo, len(data))
			}

			seenPages[ref.PageNo] = true
			blobs++
		}
	}

	err = reader.Err()
	if err != nil {
		t.Fatalf("iteration error: %v", err)
	}

	if records != 30 {
		t.Errorf("read %d records, want 30", records)
	}

	if blobs != len(rpdgBlobSizes) {
		t.Errorf("read %d BLOBs, want %d", blobs, len(rpdgBlobSizes))
	}

	for page := range rpdgBlobSizes {
		if !seenPages[page] {
			t.Errorf("BLOB page %d was never referenced by a record", page)
		}
	}
}

// TestRPDG0011LargestBlobsNotTruncated pins the regression the corrected page
// model fixes: the BLOBs on pages 66..69 are 3816 bytes. Under the old model
// PageData() stopped after 3676 bytes and these four came back as exactly
// 3652 bytes (3676 - the 24-byte BLOB page header).
func TestRPDG0011LargestBlobsNotTruncated(t *testing.T) {
	const (
		wantSize    = 3816
		oldTruncLen = 3652
	)

	db := openTestFile(t, "RPDG0011.abs")

	large := map[int32]bool{66: true, 67: true, 68: true, 69: true}
	found := 0

	for page := range large {
		data, err := db.ReadBlob(BlobRef{PageNo: page})
		if err != nil {
			t.Fatalf("ReadBlob(page %d): %v", page, err)
		}

		if len(data) == oldTruncLen {
			t.Errorf("page %d: BLOB is %d bytes — still truncated by the old page model",
				page, len(data))
		}

		if len(data) != wantSize {
			t.Errorf("page %d: BLOB is %d bytes, want %d", page, len(data), wantSize)
		}

		found++
	}

	if found != 4 {
		t.Errorf("checked %d pages, want 4", found)
	}
}

func TestBlobRefNull(t *testing.T) {
	ref := BlobRef{PageNo: 0, ItemNo: 0}
	if !ref.IsNull() {
		t.Error("zero BlobRef should be null")
	}

	ref = BlobRef{PageNo: 5, ItemNo: 0}
	if ref.IsNull() {
		t.Error("non-zero PageNo BlobRef should not be null")
	}
}

// TestBlobRefOutOfRange covers the bounds check on Record.BlobRef: an
// out-of-range column index used to index fieldOffsets directly and panic.
func TestBlobRefOutOfRange(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	if !reader.Next() {
		t.Fatal("no records")
	}

	rec := reader.Record()
	cols := len(reader.Schema().Columns)

	for _, col := range []int{-1, cols, cols + 1, 1 << 20} {
		if ref := rec.BlobRef(col); !ref.IsNull() {
			t.Errorf("BlobRef(%d) = %+v, want null ref", col, ref)
		}

		data, err := rec.Blob(col)
		if err != nil {
			t.Errorf("Blob(%d): unexpected error %v", col, err)
		}

		if data != nil {
			t.Errorf("Blob(%d) returned %d bytes, want nil", col, len(data))
		}
	}
}

// TestBlobRefShortFieldData covers the second half of the same bounds check: a
// field offset that lies inside the record but leaves fewer than 6 bytes.
func TestBlobRefShortFieldData(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	if !reader.Next() {
		t.Fatal("no records")
	}

	rec := reader.Record()
	blobCols := blobColumns(reader)

	if len(blobCols) == 0 {
		t.Fatal("expected BLOB columns")
	}

	col := blobCols[0]

	// Truncate the record's field data so the reference no longer fits.
	off := reader.fieldOffsets[col]
	rec.fieldData = rec.fieldData[:off+blobRefSize-1]

	if ref := rec.BlobRef(col); !ref.IsNull() {
		t.Errorf("BlobRef on short field data = %+v, want null ref", ref)
	}
}

func TestReadBlobNullRef(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	ref := BlobRef{PageNo: 0, ItemNo: 0}

	data, err := db.ReadBlob(ref)
	if err != nil {
		t.Fatalf("ReadBlob(null ref): %v", err)
	}

	if data != nil {
		t.Error("expected nil data for null ref")
	}
}

func TestReadBlobInvalidPage(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	for _, pageNo := range []int32{999, -1, -1 << 20} {
		_, err := db.ReadBlob(BlobRef{PageNo: pageNo})
		if err == nil {
			t.Errorf("ReadBlob(page %d): expected error", pageNo)
		}
	}
}

// TestReadBlobWrongPageType asks a data page for BLOB data.
func TestReadBlobWrongPageType(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	_, err := db.ReadBlob(BlobRef{PageNo: 1})
	if !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("ReadBlob(page 1) error = %v, want ErrBlobNotFound", err)
	}
}

func TestTS03NullBlobs(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	// TS03 has BLOB columns but all are NULL.
	for reader.Next() {
		rec := reader.Record()

		for i, c := range reader.Schema().Columns {
			if !c.IsBLOB() {
				continue
			}

			data, err := rec.Blob(i)
			if err != nil {
				t.Fatalf("unexpected error reading null BLOB: %v", err)
			}

			if data != nil {
				t.Errorf("col %d (%s): expected nil for null BLOB, got %d bytes",
					i, c.Name, len(data))
			}

			memo, err := rec.Memo(i)
			if err != nil {
				t.Fatalf("unexpected error reading null Memo: %v", err)
			}

			if memo != "" {
				t.Errorf("col %d (%s): expected empty Memo, got %q", i, c.Name, memo)
			}
		}
	}

	err = reader.Err()
	if err != nil {
		t.Fatal(err)
	}
}

// --- crafted-input tests -------------------------------------------------
//
// Each of these patches a copy of RPDG0011.abs in t.TempDir() and asks
// ReadBlob for page 13, the first type-11 page in the file. They all used to
// panic, hang, or allocate without bound.

// patchBlobHeader rewrites the three int64 values of a BLOB page header.
func patchBlobHeader(pageNo int, item, comp, uncomp int64) func([]byte) []byte {
	return func(data []byte) []byte {
		off := pageNo*testPageSize + pageDataOffset

		binary.LittleEndian.PutUint64(data[off:off+8], uint64(item))
		binary.LittleEndian.PutUint64(data[off+8:off+16], uint64(comp))
		binary.LittleEndian.PutUint64(data[off+16:off+24], uint64(uncomp))

		return data
	}
}

// patchNextPageNo rewrites NextPageNo in a page's ABSP disk page header.
func patchNextPageNo(data []byte, pageNo int, next int32) {
	off := pageNo*testPageSize + diskPageHeaderOffset + 10
	binary.LittleEndian.PutUint32(data[off:off+4], uint32(next))
}

// openCrafted copies RPDG0011.abs, applies patch and opens the result.
func openCrafted(t *testing.T, patch func([]byte) []byte) *File {
	t.Helper()

	path := craftFile(t, "RPDG0011.abs", patch)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(crafted): %v", err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// TestReadBlobNegativeCompressedSize: CompressedSize = -1 reached
// compressedData[:hdr.CompressedSize] and panicked with
// "slice bounds out of range [:-1]".
func TestReadBlobNegativeCompressedSize(t *testing.T) {
	for _, size := range []int64{-1, -4096, -1 << 40} {
		db := openCrafted(t, patchBlobHeader(13, 1, size, size))

		_, err := db.ReadBlob(BlobRef{PageNo: 13})
		if !errors.Is(err, ErrBlobSize) {
			t.Errorf("CompressedSize %d: error = %v, want ErrBlobSize", size, err)
		}
	}
}

// TestReadBlobNegativeUncompressedSize covers the other raw int64.
func TestReadBlobNegativeUncompressedSize(t *testing.T) {
	db := openCrafted(t, patchBlobHeader(13, 1, 2484, -1))

	_, err := db.ReadBlob(BlobRef{PageNo: 13})
	if !errors.Is(err, ErrBlobSize) {
		t.Errorf("error = %v, want ErrBlobSize", err)
	}
}

// TestReadBlobAbsurdCompressedSize: a declared size larger than the whole file
// used to reach make([]byte, 0, totalSize) in readBlobChain.
func TestReadBlobAbsurdCompressedSize(t *testing.T) {
	db := openCrafted(t, patchBlobHeader(13, 1, 1<<40, 1<<40))

	_, err := db.ReadBlob(BlobRef{PageNo: 13})
	if !errors.Is(err, ErrBlobSize) {
		t.Errorf("error = %v, want ErrBlobSize", err)
	}
}

// TestReadBlobNilPageHeader: the old guard tested page.Header == nil and then
// formatted page.Header.PageType inside that very branch — a nil dereference on
// exactly the input it was meant to reject.
func TestReadBlobNilPageHeader(t *testing.T) {
	db := openCrafted(t, func(data []byte) []byte {
		off := 13*testPageSize + diskPageHeaderOffset
		copy(data[off:off+4], "XXXX")

		return data
	})

	_, err := db.ReadBlob(BlobRef{PageNo: 13})
	if !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("error = %v, want ErrBlobNotFound", err)
	}
}

// TestReadBlobSelfReferentialChain: a page whose NextPageNo points at itself
// grew the chain buffer without bound. Run under the package timeout; the
// cycle must be detected on the first revisit.
func TestReadBlobSelfReferentialChain(t *testing.T) {
	db := openCrafted(t, func(data []byte) []byte {
		// Declare more than one page holds so the chain is entered, but stay
		// inside what the file could contain so the size check passes.
		data = patchBlobHeader(13, 1, 300000, 300000)(data)
		patchNextPageNo(data, 13, 13)

		return data
	})

	_, err := db.ReadBlob(BlobRef{PageNo: 13})
	if !errors.Is(err, ErrBlobChain) {
		t.Errorf("error = %v, want ErrBlobChain", err)
	}
}

// TestReadBlobTwoPageChain exercises the chain traversal itself. No fixture
// chains — every real BLOB page has NextPageNo == -1 — so this only shows the
// loop terminates and concatenates; it does not verify the on-disk format.
func TestReadBlobTwoPageChain(t *testing.T) {
	const totalSize = 4132 // one page payload after the header (4032) plus 100

	db := openCrafted(t, func(data []byte) []byte {
		data = patchBlobHeader(13, 1, totalSize, totalSize)(data)
		patchNextPageNo(data, 13, 14)

		return data
	})

	got, err := db.ReadBlob(BlobRef{PageNo: 13})
	if err != nil {
		t.Fatalf("ReadBlob(): %v", err)
	}

	if len(got) != totalSize {
		t.Errorf("len = %d, want %d", len(got), totalSize)
	}
}

// TestReadBlobChainBrokenLink stops the chain at a page without an ABSP header.
func TestReadBlobChainBrokenLink(t *testing.T) {
	db := openCrafted(t, func(data []byte) []byte {
		data = patchBlobHeader(13, 1, 300000, 300000)(data)
		patchNextPageNo(data, 13, 14)

		off := 14*testPageSize + diskPageHeaderOffset
		copy(data[off:off+4], "XXXX")

		return data
	})

	_, err := db.ReadBlob(BlobRef{PageNo: 13})
	if !errors.Is(err, ErrBlobChain) {
		t.Errorf("error = %v, want ErrBlobChain", err)
	}
}

// TestReadBlobZeroItemCount keeps the ItemCount guard covered.
func TestReadBlobZeroItemCount(t *testing.T) {
	db := openCrafted(t, patchBlobHeader(13, 0, 2484, 2484))

	_, err := db.ReadBlob(BlobRef{PageNo: 13})
	if !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("error = %v, want ErrBlobNotFound", err)
	}
}

// zlibCompress returns data as a zlib stream.
func zlibCompress(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := zlib.NewWriter(&buf)

	_, err := w.Write(data)
	if err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// patchBlobPayload writes a zlib stream into a BLOB page and declares it in the
// page header, so ReadBlob takes the decompression path.
func patchBlobPayload(pageNo int, stream []byte, declared int64) func([]byte) []byte {
	return func(data []byte) []byte {
		data = patchBlobHeader(pageNo, 1, int64(len(stream)), declared)(data)

		off := pageNo*testPageSize + pageDataOffset + blobPageHeaderSize
		copy(data[off:off+len(stream)], stream)

		return data
	}
}

// TestReadBlobDecompresses covers the compressed path end to end. No fixture
// stores a compressed BLOB, so this is the only exercise it gets.
func TestReadBlobDecompresses(t *testing.T) {
	payload := bytes.Repeat([]byte("absdb-blob-payload!"), 64)
	stream := zlibCompress(t, payload)

	db := openCrafted(t, patchBlobPayload(13, stream, int64(len(payload))))

	got, err := db.ReadBlob(BlobRef{PageNo: 13})
	if err != nil {
		t.Fatalf("ReadBlob(): %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("decompressed %d bytes, want %d identical bytes", len(got), len(payload))
	}
}

// TestReadBlobZlibBomb: a stream that inflates far beyond its declared size was
// read with io.ReadAll and no limit — a 64 KiB page expanded to 157 MiB.
func TestReadBlobZlibBomb(t *testing.T) {
	stream := zlibCompress(t, make([]byte, 2<<20))

	if len(stream) > 4032 {
		t.Fatalf("compressed bomb is %d bytes, does not fit a page payload", len(stream))
	}

	tests := []struct {
		name     string
		declared int64
	}{
		// Declared small, stream huge: caught while reading.
		{"understated", 100},
		// Declared huge: caught before a single byte is inflated, both by the
		// expansion-ratio ceiling and by maxDecompressedSize.
		{"overstated ratio", 64 << 20},
		{"overstated ceiling", 1 << 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openCrafted(t, patchBlobPayload(13, stream, tt.declared))

			got, err := db.ReadBlob(BlobRef{PageNo: 13})
			if err == nil {
				t.Fatalf("ReadBlob() returned %d bytes, want an error", len(got))
			}
		})
	}
}

// TestDecompressBlob unit-tests the decompressor directly: it had no coverage
// at all, because every BLOB in the corpus is stored uncompressed.
func TestDecompressBlob(t *testing.T) {
	payload := []byte("the quick brown fox jumps over the lazy dog, repeatedly")
	stream := zlibCompress(t, payload)

	got, err := decompressBlob(stream, int64(len(payload)))
	if err != nil {
		t.Fatalf("decompressBlob(): %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}

	// Not a zlib stream at all.
	_, err = decompressBlob([]byte("not zlib"), 8)
	if err == nil {
		t.Error("expected an error for non-zlib input")
	}

	// Truncated stream.
	_, err = decompressBlob(stream[:len(stream)/2], int64(len(payload)))
	if err == nil {
		t.Error("expected an error for a truncated stream")
	}

	// Negative declared size.
	_, err = decompressBlob(stream, -1)
	if err == nil {
		t.Error("expected an error for a negative declared size")
	}
}

func TestInflateLimit(t *testing.T) {
	tests := []struct {
		name          string
		bounds        inflateBounds
		compressedLen int
		declared      int64
		wantErr       bool
	}{
		{"blob: small stream gets the floor", blobInflateBounds, 10, minDecompressAllowance, false},
		{"blob: beyond the floor", blobInflateBounds, 10, minDecompressAllowance + 1, true},
		{"blob: ratio applies to large input", blobInflateBounds, 1 << 20, 8 << 20, false},
		{"blob: hard ceiling", blobInflateBounds, 1 << 20, maxDecompressedSize + 1, true},
		{"blob: negative", blobInflateBounds, 10, -1, true},
		{"blob: zero", blobInflateBounds, 10, 0, false},

		// The internal-file bounds are the tighter of the two: what a BLOB
		// page may declare, a schema page may not.
		{"internal: small stream gets the floor", internalFileInflateBounds, 10, minInternalAllowance, false},
		{"internal: beyond the floor", internalFileInflateBounds, 10, minInternalAllowance + 1, true},
		{"internal: ratio applies to large input", internalFileInflateBounds, 1 << 20, 8 << 20, false},
		{"internal: hard ceiling", internalFileInflateBounds, 1 << 20, maxInternalDecompressedSize + 1, true},
		{"internal: a page-sized stream stays under a megabyte", internalFileInflateBounds, 4096, 1 << 20, true},
		{"internal: negative", internalFileInflateBounds, 10, -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, err := inflateLimit(tt.compressedLen, tt.declared, tt.bounds)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("inflateLimit() = %d, want an error", limit)
				}

				return
			}

			if err != nil {
				t.Fatalf("inflateLimit(): %v", err)
			}

			if limit < tt.declared || limit > tt.bounds.hardMax {
				t.Errorf("limit = %d, out of range for declared %d", limit, tt.declared)
			}
		})
	}
}

// TestInternalFileInflateBoundsFitEveryFixture is what makes the tightened
// internal-file ratio a measurement rather than a guess: no schema, table info
// or catalog page in the corpus may come anywhere near the new ceiling. It also
// records the widest expansion actually observed, so a future tightening has a
// number to argue against.
func TestInternalFileInflateBoundsFitEveryFixture(t *testing.T) {
	var (
		worstRatio float64
		worstFile  string
		checked    int
	)

	eachCompressedInternalFile(t, func(p internalFilePage) {
		checked++

		if _, err := inflateLimit(int(p.compressed), p.decompressed, internalFileInflateBounds); err != nil {
			t.Errorf("%s page %d: %d -> %d rejected by the internal-file bounds: %v",
				p.fixture, p.pageNo, p.compressed, p.decompressed, err)
		}

		if r := float64(p.decompressed) / float64(p.compressed); r > worstRatio {
			worstRatio, worstFile = r, p.fixture
		}
	})

	if checked == 0 {
		t.Skip("no compressed internal files in the fixtures")
	}

	t.Logf("checked %d compressed internal files; widest expansion %.2fx (%s), ratio bound is %d",
		checked, worstRatio, worstFile, maxInternalCompressionRatio)

	if worstRatio > maxInternalCompressionRatio/4 {
		t.Errorf("widest expansion %.2fx leaves too little headroom under the bound of %d",
			worstRatio, maxInternalCompressionRatio)
	}
}

func TestReadBlobPageHeaderTruncated(t *testing.T) {
	for _, n := range []int{0, 1, blobPageHeaderSize - 1} {
		_, err := readBlobPageHeader(make([]byte, n))
		if !errors.Is(err, ErrBlobTrunc) {
			t.Errorf("readBlobPageHeader(%d bytes) error = %v, want ErrBlobTrunc", n, err)
		}
	}
}

func TestReadBlobRefShort(t *testing.T) {
	for _, n := range []int{0, 1, blobRefSize - 1} {
		if ref := readBlobRef(make([]byte, n)); !ref.IsNull() {
			t.Errorf("readBlobRef(%d bytes) = %+v, want null ref", n, ref)
		}
	}
}

// TestCraftedFixtureAssumptions guards the offsets the crafted tests patch.
func TestCraftedFixtureAssumptions(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	if db.PageSize() != testPageSize {
		t.Fatalf("PageSize() = %d, want %d", db.PageSize(), testPageSize)
	}

	raw, err := os.ReadFile(testdataPath("RPDG0011.abs"))
	if err != nil {
		t.Fatal(err)
	}

	off := 13*testPageSize + diskPageHeaderOffset
	if string(raw[off:off+4]) != "ABSP" {
		t.Fatalf("page 13 has no ABSP header at offset %d", off)
	}

	page, err := db.ReadPage(13)
	if err != nil {
		t.Fatal(err)
	}

	if page.Header == nil || page.Header.PageType != PageTypeBlob {
		t.Fatalf("page 13 is not a BLOB page")
	}

	if page.Header.NextPageNo != -1 {
		t.Errorf("page 13 NextPageNo = %d, want -1", page.Header.NextPageNo)
	}
}
