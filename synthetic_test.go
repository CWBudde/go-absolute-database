package absdb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// Synthetic .abs files.
//
// testdata/ holds real private project data and is not committed, so on a fresh clone
// every fixture-backed test skips and the parser would go completely
// unexercised. The builders below assemble valid .abs byte layouts in memory
// instead, so the read path — page model, schema page, record layout, data
// page bitmap, B-tree leaf, BLOB page — is covered without any fixture.
//
// The page model they implement is the one absdb.go documents:
//
//	page N's ABSP header = file[N*pageSize+380 : N*pageSize+420]
//	page N's payload     = file[N*pageSize+420 : (N+1)*pageSize+380]
//	file size            = pageCount*pageSize + 380
//
// Payloads therefore never overlap a neighbouring page's header, which is what
// makes the assembly below a simple concatenation.

const synthPageSize = 4096

// synthPage is one page to be assembled into a synthetic file.
type synthPage struct {
	pageType uint16
	objectID int32
	nextPage int32
	payload  []byte
}

// synthColumn describes one column of a synthetic table.
type synthColumn struct {
	name  string
	base  BaseFieldType
	field FieldType
	size  uint32
}

// synthRow holds the raw stored bytes per column. A nil entry marks the column
// NULL: its null-flag bit is set and its field bytes stay zero.
type synthRow struct {
	values [][]byte
}

// synthBlob is one BLOB stored on a page of its own.
type synthBlob struct {
	data     []byte
	compress bool
}

// synthSpec describes a whole synthetic database file.
type synthSpec struct {
	columns  []synthColumn
	rows     []synthRow
	blobs    []synthBlob
	compress bool // zlib-compress the schema's internal file
	pageSize int  // 0 means synthPageSize
}

// pageSizeOf returns the spec's page size, defaulting to synthPageSize.
func (s synthSpec) pageSizeOf() int {
	if s.pageSize == 0 {
		return synthPageSize
	}

	return s.pageSize
}

// --- page assembly ---

// assembleFile lays the pages out according to the payload model and returns
// the complete file bytes, header included.
func assembleFile(t testing.TB, pageSize int, pages []synthPage) []byte {
	t.Helper()

	payloadLen := pageSize - diskPageHeaderSize

	buf := make([]byte, len(pages)*pageSize+diskPageHeaderOffset)

	for i, p := range pages {
		if len(p.payload) > payloadLen {
			t.Fatalf("page %d: payload of %d bytes exceeds %d", i, len(p.payload), payloadLen)
		}

		hdr := i*pageSize + diskPageHeaderOffset
		copy(buf[hdr:], []byte("ABSP"))
		binary.LittleEndian.PutUint16(buf[hdr+8:hdr+10], p.pageType)
		binary.LittleEndian.PutUint32(buf[hdr+10:hdr+14], uint32(p.nextPage))
		// CRC32 stays 0: a non-zero CRC32 marks an encrypted page.
		binary.LittleEndian.PutUint32(buf[hdr+22:hdr+26], uint32(p.objectID))

		copy(buf[i*pageSize+pageDataOffset:], p.payload)
	}

	writeDBHeader(buf, pageSize, len(pages))

	return buf
}

// writeDBHeader fills the 76-byte TABSDBHeader at the start of page 0.
func writeDBHeader(buf []byte, pageSize, pageCount int) {
	copy(buf[0:16], Magic[:])
	binary.LittleEndian.PutUint16(buf[16:18], dbHeaderSize)
	binary.LittleEndian.PutUint64(buf[18:26], math.Float64bits(7.61))
	binary.LittleEndian.PutUint16(buf[26:28], uint16(pageSize))
	binary.LittleEndian.PutUint16(buf[28:30], 8)
	binary.LittleEndian.PutUint32(buf[30:34], uint32(pageCount))
	binary.LittleEndian.PutUint32(buf[34:38], uint32(pageCount-1))
}

// --- schema page ---

// encodeSchemaBlob encodes the decompressed schema: a column count followed by
// one definition per column, each carrying the autoinc block and the absent
// DEFAULT that close it (docs/format/schema.md). The block holds the engine's
// own defaults, so a synthetic file is bytes the engine could have written
// rather than a layout only this package's parser accepts.
func encodeSchemaBlob(cols []synthColumn) []byte {
	var buf bytes.Buffer

	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(cols)))

	for i, c := range cols {
		buf.WriteByte(byte(len(c.name)))
		buf.WriteString(c.name)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(i+1)) // column ID
		buf.WriteByte(byte(c.base))
		buf.WriteByte(byte(c.field))
		_ = binary.Write(&buf, binary.LittleEndian, c.size)

		if c.base == BftBlob || c.base == BftClob || c.base == BftWideClob {
			buf.Write(make([]byte, blobSettingsSize))
		}

		// AutoincIncrement 1, InitialValue 0, MinValue 0,
		// MaxValue High(Int64), Cycled False.
		_ = binary.Write(&buf, binary.LittleEndian, int64(1))
		_ = binary.Write(&buf, binary.LittleEndian, int64(0))
		_ = binary.Write(&buf, binary.LittleEndian, int64(0))
		_ = binary.Write(&buf, binary.LittleEndian, int64(math.MaxInt64))
		buf.WriteByte(0)

		// The DEFAULT typed value: the variant's type tag, then absent.
		buf.WriteByte(byte(c.base))
		buf.WriteByte(typedValueAbsent)
	}

	return buf.Bytes()
}

// encodeInternalFile wraps a blob in a TABSInternalFileHeader, optionally
// zlib-compressing it the way the real schema page does.
func encodeInternalFile(t testing.TB, blob []byte, compress bool) []byte {
	t.Helper()

	payload := blob
	algo := byte(0)

	if compress {
		var out bytes.Buffer

		w := zlib.NewWriter(&out)
		if _, err := w.Write(blob); err != nil {
			t.Fatalf("zlib write: %v", err)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("zlib close: %v", err)
		}

		payload = out.Bytes()
		algo = 1
	}

	out := make([]byte, internalFileHeaderSize+len(payload))
	out[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(out[1:5], uint32(len(payload)))
	binary.LittleEndian.PutUint32(out[5:9], uint32(len(blob)))
	out[9] = algo
	copy(out[internalFileHeaderSize:], payload)

	return out
}

// --- data pages ---

// synthLayout mirrors the layout the Reader derives from the schema. The
// builder must agree with it byte for byte, so the derivation is what the
// round-trip tests actually check.
type synthLayout struct {
	nullFlagBytes  int
	fieldDataSize  int
	recordSize     int
	recordsPerPage int
	bitmapBytes    int
	offsets        []int
	sizes          []int
}

func computeSynthLayout(cols []synthColumn, payloadLen int) synthLayout {
	l := synthLayout{
		nullFlagBytes: (len(cols) + 7) / 8,
		offsets:       make([]int, len(cols)),
		sizes:         make([]int, len(cols)),
	}

	offset := 0

	for i, c := range cols {
		size := fieldStoreSize(Column{BaseType: c.base, Size: c.size})
		l.offsets[i] = offset
		l.sizes[i] = size
		offset += size
	}

	l.fieldDataSize = offset
	l.recordSize = l.nullFlagBytes + l.fieldDataSize
	l.recordsPerPage = recordsPerPage(payloadLen, l.recordSize)
	l.bitmapBytes = (l.recordsPerPage + 7) / 8

	return l
}

// encodeDataPage lays out one data page payload: the occupancy bitmap followed
// by fixed-size record slots. Bit set = slot occupied; a set null-flag bit
// marks a NULL column, and the spare high bits of the last null-flag byte are
// set, which is the invariant validateLayout checks.
func encodeDataPage(cols []synthColumn, rows []synthRow, l synthLayout, payloadLen int) []byte {
	payload := make([]byte, payloadLen)

	spare := l.nullFlagBytes*8 - len(cols)

	for i, row := range rows {
		payload[i/8] |= 1 << uint(i%8)

		start := l.bitmapBytes + i*l.recordSize
		fieldStart := start + l.nullFlagBytes

		if spare > 0 {
			payload[fieldStart-1] |= byte(0xff) << uint(8-spare)
		}

		for c := range cols {
			value := row.values[c]
			if value == nil {
				payload[start+c/8] |= 1 << uint(c%8)

				continue
			}

			off := fieldStart + l.offsets[c]
			copy(payload[off:off+l.sizes[c]], value)
		}
	}

	return payload
}

// --- index pages ---

// encodeLeafPage builds one B-tree leaf page payload.
func encodeLeafPage(entries []BTreeEntry, keySize int, isRoot bool, rightPageNo int32) []byte {
	hdr := make([]byte, btreeHeaderSize)

	if isRoot {
		hdr[0] = 1
	}

	hdr[1] = 1 // IsLeaf
	binary.LittleEndian.PutUint32(hdr[2:6], math.MaxUint32)
	binary.LittleEndian.PutUint32(hdr[6:10], uint32(rightPageNo))
	hdr[10] = 1 // HasKeys
	binary.LittleEndian.PutUint16(hdr[12:14], uint16(keySize))
	binary.LittleEndian.PutUint16(hdr[14:16], uint16(len(entries)))

	buf := bytes.NewBuffer(hdr)

	for _, e := range entries {
		key := make([]byte, keySize)
		copy(key, e.Key)
		buf.Write(key)
		_ = binary.Write(buf, binary.LittleEndian, e.PageNo)
		_ = binary.Write(buf, binary.LittleEndian, e.ItemNo)
	}

	return buf.Bytes()
}

// --- BLOB pages ---

// encodeBlobPage builds a BLOB page payload: the 24-byte three-int64 header
// followed by the stored bytes. A compressed BLOB declares differing sizes,
// which is what makes the reader inflate it.
func encodeBlobPage(t testing.TB, b synthBlob) []byte {
	t.Helper()

	stored := b.data

	if b.compress {
		var out bytes.Buffer

		w := zlib.NewWriter(&out)
		if _, err := w.Write(b.data); err != nil {
			t.Fatalf("zlib write: %v", err)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("zlib close: %v", err)
		}

		stored = out.Bytes()
	}

	out := make([]byte, blobPageHeaderSize+len(stored))
	binary.LittleEndian.PutUint64(out[0:8], 1)
	binary.LittleEndian.PutUint64(out[8:16], uint64(len(stored)))
	binary.LittleEndian.PutUint64(out[16:24], uint64(len(b.data)))
	copy(out[blobPageHeaderSize:], stored)

	return out
}

// --- whole-file builder ---

// buildSynthetic assembles a complete synthetic file and returns its bytes.
//
// Page 0 is the file header, page 1 the schema, then as many data pages as the
// rows need, then as many index leaf pages as the entries need, then one page
// per BLOB. BLOB references in the rows are patched to the page each BLOB
// landed on, so a caller can leave them zero.
func buildSynthetic(t testing.TB, spec synthSpec) []byte {
	t.Helper()

	pageSize := spec.pageSizeOf()
	payloadLen := pageSize - diskPageHeaderSize
	layout := computeSynthLayout(spec.columns, payloadLen)

	if layout.recordsPerPage <= 0 {
		t.Fatalf("record of %d bytes does not fit a %d-byte page", layout.recordSize, payloadLen)
	}

	pages := make([]synthPage, 0, 4+len(spec.blobs))
	pages = append(
		pages,
		synthPage{pageType: PageTypeFileHdr, objectID: -1, nextPage: -1},
		synthPage{
			pageType: PageTypeSchema,
			objectID: 1,
			nextPage: -1,
			payload:  encodeInternalFile(t, encodeSchemaBlob(spec.columns), spec.compress),
		},
	)

	dataPages := chunkRows(spec.rows, layout.recordsPerPage)
	firstDataPage := len(pages)

	// BLOB pages come last; their page numbers are known up front because the
	// page count of every earlier section is known.
	entries := make([]BTreeEntry, 0, len(spec.rows))
	rowNo := 0

	for range dataPages {
		pages = append(pages, synthPage{pageType: PageTypeData, objectID: 1, nextPage: -1})
	}

	leafCapacity := (payloadLen - btreeHeaderSize) / (synthKeySize + leafEntrySuffixSize)
	indexPageCount := max(1, (len(spec.rows)+leafCapacity-1)/leafCapacity)
	firstBlobPage := len(pages) + indexPageCount

	for chunk, rows := range dataPages {
		pageNo := firstDataPage + chunk

		patched := patchBlobRefs(spec.columns, rows, layout, firstBlobPage)
		pages[pageNo].payload = encodeDataPage(spec.columns, patched, layout, payloadLen)

		for slot := range rows {
			key := make([]byte, synthKeySize)
			binary.LittleEndian.PutUint32(key[1:], uint32(rowNo+1))
			entries = append(entries, BTreeEntry{
				Key:    key,
				PageNo: int32(pageNo),
				ItemNo: uint16(slot),
			})
			rowNo++
		}
	}

	pages = append(pages, buildIndexPages(entries, len(pages), leafCapacity)...)

	for _, b := range spec.blobs {
		pages = append(pages, synthPage{
			pageType: PageTypeBlob,
			objectID: 1,
			nextPage: -1,
			payload:  encodeBlobPage(t, b),
		})
	}

	return assembleFile(t, pageSize, pages)
}

// synthKeySize is the key size the synthetic index uses: one null flag byte
// plus an int32 primary key, which is what primaryKeySize describes.
const synthKeySize = primaryKeySize

// chunkRows splits rows into groups of at most perPage. An empty table still
// yields one (empty) data page, mirroring what the engine writes.
func chunkRows(rows []synthRow, perPage int) [][]synthRow {
	if len(rows) == 0 {
		return [][]synthRow{nil}
	}

	var chunks [][]synthRow

	for start := 0; start < len(rows); start += perPage {
		chunks = append(chunks, rows[start:min(start+perPage, len(rows))])
	}

	return chunks
}

// buildIndexPages turns the leaf entries into a chain of leaf pages. The first
// page is the root; the rest hang off it via RightPageNo, which is the chain
// scanLeaves follows.
func buildIndexPages(entries []BTreeEntry, firstPageNo, capacity int) []synthPage {
	var chunks [][]BTreeEntry

	for start := 0; start < len(entries); start += capacity {
		chunks = append(chunks, entries[start:min(start+capacity, len(entries))])
	}

	if len(chunks) == 0 {
		chunks = [][]BTreeEntry{nil}
	}

	pages := make([]synthPage, 0, len(chunks))

	for i, chunk := range chunks {
		right := int32(-1)
		if i+1 < len(chunks) {
			right = int32(firstPageNo + i + 1)
		}

		pages = append(pages, synthPage{
			pageType: PageTypeIndex,
			objectID: 1,
			nextPage: -1,
			payload:  encodeLeafPage(chunk, synthKeySize, i == 0, right),
		})
	}

	return pages
}

// patchBlobRefs fills every BLOB column with the page number its BLOB landed
// on. BLOBs are handed out in row order, one page each.
func patchBlobRefs(cols []synthColumn, rows []synthRow, l synthLayout, firstBlobPage int) []synthRow {
	blobIdx := 0
	out := make([]synthRow, len(rows))

	for i, row := range rows {
		values := make([][]byte, len(row.values))
		copy(values, row.values)

		for c, col := range cols {
			if col.base != BftBlob && col.base != BftClob && col.base != BftWideClob {
				continue
			}

			if values[c] == nil {
				continue
			}

			ref := make([]byte, l.sizes[c])
			binary.LittleEndian.PutUint32(ref[0:4], uint32(firstBlobPage+blobIdx))
			values[c] = ref
			blobIdx++
		}

		out[i] = synthRow{values: values}
	}

	return out
}

// writeSynthetic writes a synthetic file into t.TempDir and returns its path.
func writeSynthetic(t testing.TB, spec synthSpec) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "synthetic.abs")

	err := os.WriteFile(path, buildSynthetic(t, spec), 0o600)
	if err != nil {
		t.Fatalf("writing synthetic file: %v", err)
	}

	return path
}

// --- value encoders ---

func synthInt32(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))

	return b
}

func synthInt16(v int16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, uint16(v))

	return b
}

func synthInt64(v int64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(v))

	return b
}

func synthDouble(v float64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, math.Float64bits(v))

	return b
}

func synthSingle(v float32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))

	return b
}

// synthExtended builds the x87 80-bit representation of a float64. Every
// float64 is exactly representable, so this is the inverse of
// extendedToFloat for the values it is given.
func synthExtended(v float64) []byte {
	b := make([]byte, 10)
	if v == 0 {
		return b
	}

	significand, exponent := math.Frexp(v)

	signExp := uint16(exponent - 1 + extendedExponentBias)

	if significand < 0 {
		significand = -significand
		signExp |= extendedSignBit
	}

	// Frexp yields a significand in [0.5, 1); scaling it by 2^64 puts its
	// leading bit in the explicit integer bit the format wants.
	binary.LittleEndian.PutUint64(b[0:8], uint64(math.Ldexp(significand, 64)))
	binary.LittleEndian.PutUint16(b[8:10], signExp)

	return b
}

func synthBool(v bool) []byte {
	if v {
		return []byte{1, 0}
	}

	return []byte{0, 0}
}

// synthString encodes a Windows-1252 string into a size+1 byte field, null
// terminated, which is how the engine stores Char/Varchar.
func synthString(t testing.TB, s string, size uint32) []byte {
	t.Helper()

	encoded, err := charmap.Windows1252.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encoding %q as Windows-1252: %v", s, err)
	}

	b := make([]byte, size+1)
	copy(b, encoded)

	return b
}

// synthWideString encodes a UTF-16LE string into a (size+1)*2 byte field.
func synthWideString(s string, size uint32) []byte {
	b := make([]byte, (size+1)*2)

	for i, r := range []rune(s) {
		if 2*i+1 >= len(b)-2 {
			break
		}

		binary.LittleEndian.PutUint16(b[2*i:2*i+2], uint16(r))
	}

	return b
}

// --- tests ---

// TestSyntheticRoundTrip builds a file with known values and reads them back.
// It is the only test in the suite that knows what the bytes mean without
// asking the reader, so it is the one that can catch a decoder that is
// self-consistently wrong.
func TestSyntheticRoundTrip(t *testing.T) {
	cols := []synthColumn{
		{"ID", BftInt32, FieldAutoInc, 0},
		{"Small", BftInt16, FieldSmallInt, 0},
		{"Big", BftInt64, FieldLargeInt, 0},
		{"Ratio", BftDouble, FieldDouble, 0},
		{"Approx", BftSingle, FieldSingle, 0},
		{"Name", BftVarchar, FieldString, 15},
		{"Wide", BftWideVarchar, FieldWideString, 8},
		{"Flag", BftLogical, FieldBoolean, 0},
	}

	rows := []synthRow{
		{values: [][]byte{
			synthInt32(1), synthInt16(-7), synthInt64(1 << 40),
			synthDouble(44.90930938720703), synthSingle(1.5),
			synthString(t, "Hauptstraße 4", 15), synthWideString("Grüße", 8),
			synthBool(true),
		}},
		{values: [][]byte{
			synthInt32(2), synthInt16(32767), synthInt64(-1),
			synthDouble(-0.5), synthSingle(-2.25),
			synthString(t, "Nacht", 15), synthWideString("Tag", 8),
			synthBool(false),
		}},
		// Third row leaves Name and Ratio NULL.
		{values: [][]byte{
			synthInt32(3), synthInt16(0), synthInt64(0),
			nil, synthSingle(0),
			nil, synthWideString("", 8),
			synthBool(false),
		}},
	}

	for _, compress := range []bool{false, true} {
		name := "uncompressed schema"
		if compress {
			name = "zlib schema"
		}

		t.Run(name, func(t *testing.T) {
			path := writeSynthetic(t, synthSpec{columns: cols, rows: rows, compress: compress})

			db, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			defer db.Close()

			checkSyntheticHeader(t, db)
			checkSyntheticSchema(t, db, cols)
			checkSyntheticRows(t, db)
		})
	}
}

func checkSyntheticHeader(t *testing.T, db *File) {
	t.Helper()

	if db.PageSize() != synthPageSize {
		t.Errorf("PageSize() = %d, want %d", db.PageSize(), synthPageSize)
	}

	if db.Version() != 7.61 {
		t.Errorf("Version() = %v, want 7.61", db.Version())
	}

	if db.Encrypted() {
		t.Error("Encrypted() = true, want false")
	}

	want := int64(db.PageCount())*int64(db.PageSize()) + diskPageHeaderOffset
	if db.size != want {
		t.Errorf("size = %d, want %d", db.size, want)
	}
}

func checkSyntheticSchema(t *testing.T, db *File, cols []synthColumn) {
	t.Helper()

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	if len(schema.Columns) != len(cols) {
		t.Fatalf("schema has %d columns, want %d", len(schema.Columns), len(cols))
	}

	for i, want := range cols {
		got := schema.Columns[i]

		switch {
		case got.Name != want.name:
			t.Errorf("column %d: Name = %q, want %q", i, got.Name, want.name)
		case got.BaseType != want.base:
			t.Errorf("column %d: BaseType = %d, want %d", i, got.BaseType, want.base)
		case got.FieldType != want.field:
			t.Errorf("column %d: FieldType = %v, want %v", i, got.FieldType, want.field)
		case got.Size != want.size:
			t.Errorf("column %d: Size = %d, want %d", i, got.Size, want.size)
		case got.Position != i:
			t.Errorf("column %d: Position = %d", i, got.Position)
		}
	}
}

func checkSyntheticRows(t *testing.T, db *File) {
	t.Helper()

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	type want struct {
		id     int32
		small  int16
		big    int64
		ratio  float64
		approx float64
		name   string
		wide   string
		flag   bool
		nulls  []int
	}

	wants := []want{
		{1, -7, 1 << 40, 44.90930938720703, 1.5, "Hauptstraße 4", "Grüße", true, nil},
		{2, 32767, -1, -0.5, -2.25, "Nacht", "Tag", false, nil},
		{3, 0, 0, 0, 0, "", "", false, []int{3, 5}},
	}

	idx := 0

	for reader.Next() {
		if idx >= len(wants) {
			t.Fatalf("reader yielded more than %d rows", len(wants))
		}

		rec := reader.Record()
		w := wants[idx]

		checkRowValues(t, rec, idx, w.id, w.small, w.big, w.ratio, w.approx, w.name, w.wide, w.flag)

		for col := range 8 {
			wantNull := false

			for _, n := range w.nulls {
				if n == col {
					wantNull = true
				}
			}

			if rec.IsNull(col) != wantNull {
				t.Errorf("row %d col %d: IsNull = %v, want %v", idx, col, rec.IsNull(col), wantNull)
			}
		}

		idx++
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	if idx != len(wants) {
		t.Errorf("read %d rows, want %d", idx, len(wants))
	}
}

func checkRowValues(
	t *testing.T, rec Record, row int,
	id int32, small int16, big int64, ratio, approx float64, name, wide string, flag bool,
) {
	t.Helper()

	if got := rec.Int(0); got != id {
		t.Errorf("row %d: Int(0) = %d, want %d", row, got, id)
	}

	if got := rec.Int16(1); got != small {
		t.Errorf("row %d: Int16(1) = %d, want %d", row, got, small)
	}

	if got := rec.Int64(2); got != big {
		t.Errorf("row %d: Int64(2) = %d, want %d", row, got, big)
	}

	if got := rec.Float(3); got != ratio {
		t.Errorf("row %d: Float(3) = %v, want %v", row, got, ratio)
	}

	if got := rec.Float(4); got != approx {
		t.Errorf("row %d: Float(4) = %v, want %v", row, got, approx)
	}

	if got := rec.String(5); got != name {
		t.Errorf("row %d: String(5) = %q, want %q", row, got, name)
	}

	if got := rec.String(6); got != wide {
		t.Errorf("row %d: String(6) = %q, want %q", row, got, wide)
	}

	if got := rec.Bool(7); got != flag {
		t.Errorf("row %d: Bool(7) = %v, want %v", row, got, flag)
	}
}

// TestSyntheticColumnCounts sweeps the column count across the byte boundaries
// of the null-flag prefix. numColumns mod 8 == 0 (no spare bits, so
// validateLayout's check does not apply) and == 7 (a single spare bit) are the
// two cases where the old "+2 fudge" search put the right layout outside the
// space it looked at.
func TestSyntheticColumnCounts(t *testing.T) {
	for numColumns := 1; numColumns <= 25; numColumns++ {
		t.Run(columnCountName(numColumns), func(t *testing.T) {
			cols := make([]synthColumn, numColumns)
			for i := range cols {
				cols[i] = synthColumn{name: "C" + strconv.Itoa(i), base: BftInt32, field: FieldInteger}
			}

			rows := make([]synthRow, 4)
			for r := range rows {
				values := make([][]byte, numColumns)
				for c := range values {
					values[c] = synthInt32(int32(100*r + c))
				}

				rows[r] = synthRow{values: values}
			}

			path := writeSynthetic(t, synthSpec{columns: cols, rows: rows, compress: true})

			db, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			defer db.Close()

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable (%d columns): %v", numColumns, err)
			}

			wantNullBytes := (numColumns + 7) / 8
			if reader.nullFlagBytes != wantNullBytes {
				t.Errorf("nullFlagBytes = %d, want %d", reader.nullFlagBytes, wantNullBytes)
			}

			if want := wantNullBytes + 4*numColumns; reader.recordSize != want {
				t.Errorf("recordSize = %d, want %d", reader.recordSize, want)
			}

			checkSweptRows(t, reader, numColumns, len(rows))
		})
	}
}

func checkSweptRows(t *testing.T, reader *Reader, numColumns, wantRows int) {
	t.Helper()

	row := 0

	for reader.Next() {
		rec := reader.Record()

		for c := range numColumns {
			if got, want := rec.Int(c), int32(100*row+c); got != want {
				t.Errorf("row %d col %d: Int = %d, want %d", row, c, got, want)
			}
		}

		row++
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	if row != wantRows {
		t.Errorf("read %d rows, want %d", row, wantRows)
	}
}

// TestSyntheticOracle runs the reader-versus-leaf-scan cross-check against a
// synthetic file, so the oracle itself keeps working on a fresh clone.
func TestSyntheticOracle(t *testing.T) {
	cols := []synthColumn{
		{"ID", BftInt32, FieldAutoInc, 0},
		{"Label", BftVarchar, FieldString, 7},
	}

	// Enough rows to spill over several data pages and several index leaves.
	const rowCount = 900

	rows := make([]synthRow, rowCount)
	for i := range rows {
		rows[i] = synthRow{values: [][]byte{
			synthInt32(int32(i + 1)),
			synthString(t, "row", 7),
		}}
	}

	path := writeSynthetic(t, synthSpec{columns: cols, rows: rows, compress: true})

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	readerIDs, err := readerRecordIDs(reader)
	if err != nil {
		t.Fatalf("iterating: %v", err)
	}

	if len(readerIDs) != rowCount {
		t.Errorf("reader yielded %d rows, want %d", len(readerIDs), rowCount)
	}

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}

	userIndexes := ir.UserIndexes()
	if len(userIndexes) != 1 {
		t.Fatalf("found %d user indexes, want 1", len(userIndexes))
	}

	leafIDs, err := leafRecordIDs(ir, userIndexes[0].RootPageNo)
	if err != nil {
		t.Fatalf("ScanIndex: %v", err)
	}

	if len(leafIDs) != len(readerIDs) {
		t.Errorf("leaf scan reports %d rows, reader %d", len(leafIDs), len(readerIDs))
	}

	onlyReader, onlyLeaf := diffIDs(sortIDs(readerIDs), sortIDs(leafIDs))
	reportDiff(t, "synthetic", onlyReader, onlyLeaf)
}

// TestSyntheticPrimaryKeyLookup exercises the B-tree descent against keys the
// test itself wrote, so a lookup that returns the wrong row is visible.
func TestSyntheticPrimaryKeyLookup(t *testing.T) {
	cols := []synthColumn{{"ID", BftInt32, FieldAutoInc, 0}}

	const rowCount = 50

	rows := make([]synthRow, rowCount)
	for i := range rows {
		rows[i] = synthRow{values: [][]byte{synthInt32(int32(i + 1))}}
	}

	path := writeSynthetic(t, synthSpec{columns: cols, rows: rows, compress: true})

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}

	for key := int32(1); key <= rowCount; key++ {
		pageNo, itemNo, err := ir.FindByPrimaryKey(key)
		if err != nil {
			t.Fatalf("FindByPrimaryKey(%d): %v", key, err)
		}

		if itemNo != uint16(key-1) {
			t.Errorf("FindByPrimaryKey(%d) = (page %d, item %d), want item %d",
				key, pageNo, itemNo, key-1)
		}
	}

	_, _, err = ir.FindByPrimaryKey(rowCount + 1)
	if err == nil {
		t.Error("FindByPrimaryKey past the last key succeeded, want ErrKeyNotFound")
	}
}

// TestSyntheticBlob covers both BLOB storage forms. The fixtures only ever
// store BLOBs uncompressed, so the compressed case is reached nowhere else.
func TestSyntheticBlob(t *testing.T) {
	cols := []synthColumn{
		{"ID", BftInt32, FieldAutoInc, 0},
		{"Note", BftBlob, FieldMemo, 0},
	}

	plain := []byte("a short uncompressed BLOB")
	compressible := bytes.Repeat([]byte("Hauptstrasse "), 200)

	rows := []synthRow{
		{values: [][]byte{synthInt32(1), make([]byte, blobRefSize)}},
		{values: [][]byte{synthInt32(2), make([]byte, blobRefSize)}},
		{values: [][]byte{synthInt32(3), nil}}, // NULL BLOB
	}

	spec := synthSpec{
		columns:  cols,
		rows:     rows,
		compress: true,
		blobs: []synthBlob{
			{data: plain},
			{data: compressible, compress: true},
		},
	}

	path := writeSynthetic(t, spec)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	wants := [][]byte{plain, compressible, nil}

	row := 0

	for reader.Next() {
		got, err := reader.Record().Blob(1)
		if err != nil {
			t.Fatalf("row %d: Blob: %v", row, err)
		}

		if !bytes.Equal(got, wants[row]) {
			t.Errorf("row %d: BLOB is %d bytes, want %d", row, len(got), len(wants[row]))
		}

		row++
	}

	if row != len(wants) {
		t.Errorf("read %d rows, want %d", row, len(wants))
	}
}

// TestSyntheticEmptyTable covers the shape the Addresses fixtures have: a
// well-formed file whose data page holds no occupied slot.
func TestSyntheticEmptyTable(t *testing.T) {
	cols := []synthColumn{{"ID", BftInt32, FieldAutoInc, 0}}

	path := writeSynthetic(t, synthSpec{columns: cols, compress: true})

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	if reader.Next() {
		t.Error("empty table yielded a row")
	}

	if err := reader.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}

	for _, idx := range ir.UserIndexes() {
		entries, err := ir.ScanIndex(idx.RootPageNo)
		if err != nil {
			t.Errorf("ScanIndex(%d): %v", idx.RootPageNo, err)
		}

		if len(entries) != 0 {
			t.Errorf("index root %d reports %d entries, want 0", idx.RootPageNo, len(entries))
		}
	}
}

// TestSyntheticBadLayoutDetected corrupts the spare null-flag bits of the
// first record. validateLayout exists to notice exactly that, so OpenTable
// must refuse the file rather than hand back plausible-looking garbage.
func TestSyntheticBadLayoutDetected(t *testing.T) {
	cols := make([]synthColumn, 7) // 7 columns: one spare bit in the flag byte
	for i := range cols {
		cols[i] = synthColumn{name: "C" + strconv.Itoa(i), base: BftInt32, field: FieldInteger}
	}

	rows := []synthRow{{values: make([][]byte, 7)}}
	for c := range rows[0].values {
		rows[0].values[c] = synthInt32(int32(c))
	}

	data := buildSynthetic(t, synthSpec{columns: cols, rows: rows, compress: true})

	// Page 2 is the data page; its payload starts at 2*pageSize+pageDataOffset,
	// and the occupancy bitmap precedes the first record's null-flag byte.
	layout := computeSynthLayout(cols, synthPageSize-diskPageHeaderSize)
	nullFlagOffset := 2*synthPageSize + pageDataOffset + layout.bitmapBytes
	data[nullFlagOffset] &^= 0x80

	path := filepath.Join(t.TempDir(), "badlayout.abs")

	err := os.WriteFile(path, data, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	_, err = db.OpenTable()
	if err == nil {
		t.Fatal("OpenTable accepted a record with a cleared spare null-flag bit")
	}
}

// TestOracleDiffDetectsMismatch proves the cross-check is not vacuous: fed two
// different sets, diffIDs must report the difference in both directions.
func TestOracleDiffDetectsMismatch(t *testing.T) {
	a := []recordID{{11, 0}, {11, 1}, {12, 0}}
	b := []recordID{{11, 1}, {12, 0}, {12, 5}}

	onlyA, onlyB := diffIDs(a, b)

	if len(onlyA) != 1 || onlyA[0] != (recordID{11, 0}) {
		t.Errorf("onlyA = %v, want [(page 11, item 0)]", onlyA)
	}

	if len(onlyB) != 1 || onlyB[0] != (recordID{12, 5}) {
		t.Errorf("onlyB = %v, want [(page 12, item 5)]", onlyB)
	}

	onlyA, onlyB = diffIDs(a, a)
	if len(onlyA) != 0 || len(onlyB) != 0 {
		t.Errorf("identical sets differ: %v / %v", onlyA, onlyB)
	}
}

// --- small helpers ---

func columnCountName(n int) string {
	return strconv.Itoa(n) + " columns"
}
