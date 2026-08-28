package absdb

import (
	"encoding/binary"
	"testing"
)

// internalFilePage is one zlib-compressed internal file (schema, table info or
// catalog) found in a fixture, together with the header fields that describe
// it.
type internalFilePage struct {
	fixture string
	pageNo  int

	// data is the whole page payload, starting at the TABSInternalFileHeader.
	data []byte

	compressed   int64 // FileSize: the length of the compressed stream
	decompressed int64 // DecompressedSize
}

// stream returns the raw zlib stream the page carries, or nil when the header
// points outside the page.
func (p internalFilePage) stream() []byte {
	hdrSize := int64(p.data[0])
	if hdrSize < internalFileHeaderSize || hdrSize+p.compressed > int64(len(p.data)) {
		return nil
	}

	return p.data[hdrSize : hdrSize+p.compressed]
}

// eachCompressedInternalFile calls fn for every zlib-compressed internal file
// in every fixture in testdata/, in fixture then page order.
//
// Two tests depend on enumerating exactly this set of pages —
// TestInternalFileInflateBoundsFitEveryFixture, which checks the inflate bounds
// clear every real stream, and TestZlib1ReproducesEveryCorpusStream, which
// re-compresses each one — so the enumeration lives here rather than in either
// of them, where the two copies could drift apart.
//
// It skips the test when testdata/ holds no fixtures: that directory is not
// committed, so a fresh clone has nothing to walk.
func eachCompressedInternalFile(t *testing.T, fn func(internalFilePage)) {
	t.Helper()

	for _, name := range fixtureNames(t) {
		db, err := Open(testdataPath(name))
		if err != nil {
			continue
		}

		eachCompressedInternalFileIn(db, name, fn)

		db.Close()
	}
}

// eachCompressedInternalFileIn is eachCompressedInternalFile's per-fixture half.
func eachCompressedInternalFileIn(db *File, name string, fn func(internalFilePage)) {
	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil || page.Header == nil {
			continue
		}

		switch page.Header.PageType {
		case PageTypeSchema, PageTypeTableInfo, PageTypeTableList:
		default:
			continue
		}

		// data[9] is the TABSInternalFileHeader's CompressionAlgorithm; 1 is
		// zlib, 0 means the file is stored uncompressed.
		data := page.PageData()
		if len(data) < internalFileHeaderSize || data[9] != 1 {
			continue
		}

		compressed := int64(binary.LittleEndian.Uint32(data[1:5]))
		if compressed == 0 {
			continue
		}

		fn(internalFilePage{
			fixture:      name,
			pageNo:       i,
			data:         data,
			compressed:   compressed,
			decompressed: int64(binary.LittleEndian.Uint32(data[5:9])),
		})
	}
}
