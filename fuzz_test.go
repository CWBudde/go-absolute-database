package absdb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// Fuzz targets for the read path.
//
// The policy CLAUDE.md states is: no panics, no unbounded allocation, no hangs
// on arbitrary input. These targets are what keeps it true — several bugs of
// exactly that shape were fixed in the commits this suite accompanies.
//
// The corpora are seeded from testdata/*.abs when present and always from
// small synthetic files, so a fresh clone still fuzzes something real.

// fuzzSpecs returns the synthetic files used to seed every corpus. They use a
// small page size so a whole file is a couple of kilobytes, which is what the
// mutator works best with.
func fuzzSpecs() []synthSpec {
	twoInts := []synthColumn{
		{"ID", BftInt32, FieldAutoInc, 0},
		{"Value", BftInt32, FieldInteger, 0},
	}

	mixed := []synthColumn{
		{"ID", BftInt32, FieldAutoInc, 0},
		{"Name", BftVarchar, FieldString, 7},
		{"Ratio", BftDouble, FieldDouble, 0},
		{"Note", BftBlob, FieldMemo, 0},
	}

	rows := []synthRow{
		{values: [][]byte{synthInt32(1), synthInt32(42)}},
		{values: [][]byte{synthInt32(2), nil}},
	}

	return []synthSpec{
		{columns: twoInts, rows: rows, compress: true, pageSize: 512},
		{columns: twoInts, rows: rows, compress: false, pageSize: 512},
		{columns: mixed, compress: true, pageSize: 1024, blobs: []synthBlob{{data: []byte("memo")}}},
	}
}

// maxSeedBytes keeps the giant fixtures out of the corpus. The mutator spends
// its whole budget on a 300 KiB input for no extra coverage: the structures
// that matter all appear in the smaller files too.
const maxSeedBytes = 64 << 10

// seedFromFixtures adds every testdata/*.abs below maxSeedBytes to the corpus,
// or nothing at all when the directory is absent.
func seedFromFixtures(f *testing.F) {
	f.Helper()

	matches, err := filepath.Glob(filepath.Join("testdata", "*.abs"))
	if err != nil {
		return
	}

	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxSeedBytes {
			continue
		}

		f.Add(data)
	}
}

// Work budgets for exerciseFile. A mutated header can declare tens of
// thousands of columns and thousands of rows, and the full accessor sweep over
// that product takes minutes — which starves the fuzzer instead of finding
// bugs. The caps keep every input cheap; the paths themselves are still
// reached, just not repeated pointlessly.
const (
	maxFuzzRows   = 64
	maxFuzzCols   = 48
	maxFuzzBlobs  = 8
	maxFuzzTables = 8
)

// exerciseFile drives every read path a caller can reach from an open file.
// It ignores errors on purpose: an error is a correct outcome for garbage
// input, a panic or a runaway allocation is not.
func exerciseFile(db *File) {
	_, _ = db.ScanPages()
	_, _ = db.Schema()

	exerciseCatalog(db)
	exerciseRecords(db)
	exerciseIndexes(db)
}

// exerciseCatalog walks the table catalog and opens each table it names. A
// crafted catalog can claim any page numbers it likes, so every handle it
// yields has to survive being used, not merely being built.
func exerciseCatalog(db *File) {
	tables, err := db.Tables()
	if err != nil {
		return
	}

	for i, info := range tables {
		if i >= maxFuzzTables {
			return
		}

		tbl, err := db.Table(info.Name)
		if err != nil {
			continue
		}

		_, _ = tbl.Schema()
		_, _ = tbl.OpenIndex()

		reader, err := tbl.Open()
		if err != nil {
			continue
		}

		for rows := 0; reader.Next() && rows < maxFuzzRows; rows++ {
			_ = reader.Record()
		}
	}
}

func exerciseRecords(db *File) {
	reader, err := db.OpenTable()
	if err != nil {
		return
	}

	cols := min(len(reader.Schema().Columns), maxFuzzCols)
	blobs := 0
	rows := 0

	for reader.Next() {
		rec := reader.Record()

		for col := range cols {
			_ = rec.IsNull(col)
			_ = rec.Int64(col)
			_ = rec.Float(col)
			_ = rec.String(col)
			_ = rec.Bool(col)
			_ = rec.Time(col)
			_ = rec.Bytes(col)
			_ = rec.BlobRef(col)

			if blobs < maxFuzzBlobs {
				_, _ = rec.Blob(col)
				blobs++
			}
		}

		rows++
		if rows >= maxFuzzRows {
			break
		}
	}

	_ = reader.Err()
}

func exerciseIndexes(db *File) {
	ir, err := db.OpenIndex()
	if err != nil {
		return
	}

	for _, idx := range ir.Indexes() {
		_, _ = ir.ScanIndex(idx.RootPageNo)
	}

	// Only reaching the descent matters here; either outcome is fine.
	if _, _, err := ir.FindByPrimaryKey(1); err != nil {
		_ = err
	}

	if _, _, err := ir.FindByStringKey("x"); err != nil {
		_ = err
	}
}

// FuzzOpen drives the whole read path — header, pages, schema, records,
// indexes and BLOBs — over arbitrary bytes presented as an .abs file.
//
// A note for anyone tempted to optimise it, because this cost a session once:
// the coordinator's live "execs" and "execs/sec" only advance when a worker
// returns from a batch, so on a target that rarely finds anything interesting
// they read 0/sec for minutes at a time and the total they end on understates
// the run by an order of magnitude. That display is what "FuzzOpen manages only
// a few hundred executions per minute" was read off. Measured with a fixed
// budget instead — go test -fuzz FuzzOpen -fuzztime=3000x — the target runs at
// roughly 2700 executions per second, and did so before the decompression
// bounds were tightened as well.
func FuzzOpen(f *testing.F) {
	for _, spec := range fuzzSpecs() {
		f.Add(buildSynthetic(f, spec))
	}

	seedFromFixtures(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.abs")

		err := os.WriteFile(path, data, 0o600)
		if err != nil {
			t.Skip()
		}

		db, err := Open(path)
		if err != nil {
			return
		}

		defer db.Close()

		exerciseFile(db)
	})
}

// FuzzParseSchema targets the schema decoder directly: the internal file
// header, the zlib stream behind it and the column definitions inside. Feeding
// it the decompressed blob as well as the wrapped page reaches parseSchema
// without having to get past zlib first.
func FuzzParseSchema(f *testing.F) {
	for _, spec := range fuzzSpecs() {
		blob := encodeSchemaBlob(spec.columns)
		f.Add(blob)
		f.Add(encodeInternalFile(f, blob, false))
		f.Add(encodeInternalFile(f, blob, true))
	}

	seedFromFixtureSchemas(f)

	f.Fuzz(func(_ *testing.T, data []byte) {
		if decompressed, err := decompressInternalFile(data); err == nil {
			_, _ = parseSchema(decompressed)
		}

		_, _ = parseSchema(data)
	})
}

// FuzzParseSchemaTail targets the schema stream's two record arrays: the index
// definitions (ddl_index.go) and the constraint records (ddl_constraint.go).
// Both are length-prefixed strings and counts read straight off disk, so this
// is where a malformed stream would reach a bad slice expression or an
// unbounded allocation. It seeds from the same corpus as FuzzParseSchema, but
// feeds the decompressed stream rather than the page payload, because that is
// what parseSchemaTail is handed in production.
func FuzzParseSchemaTail(f *testing.F) {
	for _, spec := range fuzzSpecs() {
		f.Add(encodeSchemaBlob(spec.columns))
	}

	seedFromFixtureStreams(f)

	f.Fuzz(func(_ *testing.T, data []byte) {
		// Nothing is asserted about the results: the target is that no input
		// panics, allocates unboundedly, or slices out of range.
		_, _, _, _, _, _ = parseSchemaTail(data)
	})
}

// seedFromFixtureStreams adds every fixture's decompressed column-definition
// stream, which is exactly what parseSchemaTail reads.
func seedFromFixtureStreams(f *testing.F) {
	f.Helper()

	matches, err := filepath.Glob(filepath.Join("testdata", "*.abs"))
	if err != nil {
		return
	}

	for _, path := range matches {
		db, err := Open(path)
		if err != nil {
			continue
		}

		tables, err := db.Tables()
		if err == nil {
			for _, info := range tables {
				if raw, err := db.readSchemaStream(info.SchemaPageNo); err == nil {
					f.Add(raw)
				}
			}
		}

		db.Close()
	}
}

// seedFromFixtureSchemas adds the raw schema page payload of every fixture,
// which is the exact byte range decompressInternalFile sees in production.
func seedFromFixtureSchemas(f *testing.F) {
	f.Helper()

	matches, err := filepath.Glob(filepath.Join("testdata", "*.abs"))
	if err != nil {
		return
	}

	for _, path := range matches {
		db, err := Open(path)
		if err != nil {
			continue
		}

		pageNo, err := db.findPageByType(PageTypeSchema)
		if err == nil && pageNo >= 0 {
			if page, err := db.ReadPage(pageNo); err == nil {
				f.Add(page.PageData())
			}
		}

		db.Close()
	}
}

// FuzzReadBlob targets the BLOB reader. The input replaces the payload of a
// synthetic file's BLOB page, so the declared item count and the two sizes in
// the 24-byte header — the fields that drive every allocation and every slice
// expression in blob.go — are under the fuzzer's control while the rest of the
// file stays well formed.
func FuzzReadBlob(f *testing.F) {
	for _, b := range []synthBlob{
		{data: []byte("memo")},
		{data: []byte("a longer memo that still fits on one page")},
		{data: make([]byte, 200), compress: true},
	} {
		f.Add(encodeBlobPage(f, b))
	}

	seedFromFixtureBlobs(f)

	template, blobPageNo := blobFuzzTemplate(f)
	payloadLen := blobFuzzPageSize - diskPageHeaderSize

	f.Fuzz(func(t *testing.T, payload []byte) {
		data := make([]byte, len(template))
		copy(data, template)

		start := blobPageNo*blobFuzzPageSize + pageDataOffset
		copy(data[start:start+payloadLen], make([]byte, payloadLen))
		copy(data[start:start+payloadLen], payload)

		path := filepath.Join(t.TempDir(), "blob.abs")

		err := os.WriteFile(path, data, 0o600)
		if err != nil {
			t.Skip()
		}

		db, err := Open(path)
		if err != nil {
			return
		}

		defer db.Close()

		_, _ = db.ReadBlob(BlobRef{PageNo: int32(blobPageNo)})
		_, _ = db.ReadBlob(BlobRef{PageNo: int32(blobPageNo), ItemNo: 1})
	})
}

// blobFuzzPageSize is the page size of the file FuzzReadBlob patches. It is
// deliberately small so an input covering a whole page payload stays short.
const blobFuzzPageSize = 1024

// blobFuzzTemplate builds the well-formed file whose BLOB page FuzzReadBlob
// overwrites, and returns it together with that page's number.
func blobFuzzTemplate(f *testing.F) ([]byte, int) {
	f.Helper()

	spec := synthSpec{
		columns:  []synthColumn{{"ID", BftInt32, FieldAutoInc, 0}, {"Note", BftBlob, FieldMemo, 0}},
		rows:     []synthRow{{values: [][]byte{synthInt32(1), make([]byte, blobRefSize)}}},
		blobs:    []synthBlob{{data: []byte("seed")}},
		compress: true,
		pageSize: blobFuzzPageSize,
	}

	data := buildSynthetic(f, spec)

	// The BLOB page is the last page: file header, schema, data, index, BLOB.
	pageCount := (len(data) - diskPageHeaderOffset) / blobFuzzPageSize

	return data, pageCount - 1
}

// seedFromFixtureBlobs adds the payload of every type-11 page found in the
// fixtures, which is what a real BLOB page looks like.
func seedFromFixtureBlobs(f *testing.F) {
	f.Helper()

	matches, err := filepath.Glob(filepath.Join("testdata", "*.abs"))
	if err != nil {
		return
	}

	const maxSeeds = 8

	seeds := 0

	for _, path := range matches {
		db, err := Open(path)
		if err != nil {
			continue
		}

		for i := range db.PageCount() {
			if seeds >= maxSeeds {
				break
			}

			page, err := db.ReadPage(i)
			if err != nil || page.Header == nil || page.Header.PageType != PageTypeBlob {
				continue
			}

			f.Add(page.PageData())

			seeds++
		}

		db.Close()

		if seeds >= maxSeeds {
			return
		}
	}
}

// FuzzParseTableList targets the table catalog decoder directly, without having
// to get a well-formed page and internal file header past the reader first.
func FuzzParseTableList(f *testing.F) {
	f.Add([]byte(nil))
	f.Add(make([]byte, tableListEntrySize))
	f.Add(make([]byte, 3*tableListEntrySize))

	entry := make([]byte, tableListEntrySize)
	entry[0] = 5
	copy(entry[1:], "Alpha")
	binary.LittleEndian.PutUint32(entry[tableNameFieldSize:], 1)
	f.Add(entry)

	f.Fuzz(func(_ *testing.T, data []byte) {
		tables, err := parseTableList(data)
		if err != nil {
			return
		}

		for _, info := range tables {
			_ = info.Name
		}
	})
}
