package absdb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestTS03Schema(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema(): %v", err)
	}

	// TS03 has 9 columns in its schema definition.
	if len(schema.Columns) != 9 {
		t.Fatalf("len(Columns) = %d, want 9", len(schema.Columns))
	}

	// Verify known columns.
	expected := []struct {
		name      string
		fieldType FieldType
		baseType  BaseFieldType
		size      uint32
	}{
		{"ZugArt", FieldAutoInc, BftInt32, 0},
		{"Name", FieldString, BftVarchar, 40},
		{"SBA", FieldDouble, BftDouble, 0},
		{"Vmax", FieldDouble, BftDouble, 0},
		{"LZug", FieldDouble, BftDouble, 0},
		{"DFz", FieldDouble, BftDouble, 0},
		{"DAo", FieldDouble, BftDouble, 0},
		{"Kommentar", FieldMemo, BftClob, 0},
		{"Graphic", FieldGraphic, BftBlob, 0},
	}

	for i, want := range expected {
		col := schema.Columns[i]
		if col.Name != want.name {
			t.Errorf("col %d: Name = %q, want %q", i, col.Name, want.name)
		}

		if col.FieldType != want.fieldType {
			t.Errorf("col %d %q: FieldType = %v, want %v", i, col.Name, col.FieldType, want.fieldType)
		}

		if col.BaseType != want.baseType {
			t.Errorf("col %d %q: BaseType = %d, want %d", i, col.Name, col.BaseType, want.baseType)
		}

		if col.Size != want.size {
			t.Errorf("col %d %q: Size = %d, want %d", i, col.Name, col.Size, want.size)
		}

		if col.Position != i {
			t.Errorf("col %d %q: Position = %d, want %d", i, col.Name, col.Position, i)
		}
	}
}

func TestAddressesSchema(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema(): %v", err)
	}

	// Log all columns for debugging.
	for i, col := range schema.Columns {
		t.Logf("Col %2d: %-20s type=%-12s base=%2d size=%d",
			i, col.Name, col.FieldType, col.BaseType, col.Size)
	}

	// Verify first few known columns.
	if schema.Columns[0].Name != "Eintrag" {
		t.Errorf("col 0 Name = %q, want Eintrag", schema.Columns[0].Name)
	}

	if schema.Columns[0].FieldType != FieldAutoInc {
		t.Errorf("col 0 FieldType = %v, want AutoInc", schema.Columns[0].FieldType)
	}

	if schema.Columns[1].Name != "Company" {
		t.Errorf("col 1 Name = %q, want Company", schema.Columns[1].Name)
	}

	if schema.Columns[1].FieldType != FieldString {
		t.Errorf("col 1 FieldType = %v, want String", schema.Columns[1].FieldType)
	}

	if schema.Columns[1].Size != 128 {
		t.Errorf("col 1 Size = %d, want 128", schema.Columns[1].Size)
	}
}

func TestRREC0011Schema(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema(): %v", err)
	}

	// Log all columns.
	for i, col := range schema.Columns {
		t.Logf("Col %2d: %-20s type=%-12s base=%2d size=%d",
			i, col.Name, col.FieldType, col.BaseType, col.Size)
	}

	if len(schema.Columns) == 0 {
		t.Fatal("expected at least 1 column")
	}
}

func TestColumnIsBLOB(t *testing.T) {
	tests := []struct {
		baseType BaseFieldType
		want     bool
	}{
		{BftBlob, true},
		{BftClob, true},
		{BftWideClob, true},
		{BftVarchar, false},
		{BftInt32, false},
		{BftDouble, false},
	}

	for _, tt := range tests {
		col := Column{BaseType: tt.baseType}
		if got := col.IsBLOB(); got != tt.want {
			t.Errorf("IsBLOB() for baseType %d = %v, want %v", tt.baseType, got, tt.want)
		}
	}
}

func TestFieldTypeString(t *testing.T) {
	tests := []struct {
		ft   FieldType
		want string
	}{
		{FieldString, "String"},
		{FieldInteger, "Integer"},
		{FieldDouble, "Double"},
		{FieldAutoInc, "AutoInc"},
		{FieldMemo, "Memo"},
		{FieldBLOB, "BLOB"},
		{FieldGUID, "GUID"},
	}

	for _, tt := range tests {
		if got := tt.ft.String(); got != tt.want {
			t.Errorf("FieldType(%d).String() = %q, want %q", int(tt.ft), got, tt.want)
		}
	}
}

// --- internal file / schema blob hardening -------------------------------

// internalFile builds a TABSInternalFileHeader followed by payload.
func internalFile(algo byte, declared uint32, payload []byte) []byte {
	buf := make([]byte, internalFileHeaderSize, internalFileHeaderSize+len(payload))
	buf[0] = internalFileHeaderSize

	binary.LittleEndian.PutUint32(buf[1:5], uint32(len(payload)))
	binary.LittleEndian.PutUint32(buf[5:9], declared)

	buf[9] = algo

	return append(buf, payload...)
}

func zlibBytes(t *testing.T, data []byte) []byte {
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

func TestDecompressInternalFileRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("schema blob "), 100)

	got, err := decompressInternalFile(internalFile(1, uint32(len(payload)), zlibBytes(t, payload)))
	if err != nil {
		t.Fatalf("decompressInternalFile(): %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("round trip returned %d bytes, want %d", len(got), len(payload))
	}

	// Algorithm 0 stores the payload verbatim.
	got, err = decompressInternalFile(internalFile(0, uint32(len(payload)), payload))
	if err != nil {
		t.Fatalf("decompressInternalFile(uncompressed): %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Error("uncompressed payload did not round trip")
	}
}

// TestDecompressInternalFileBadHeader covers the lengths read straight off
// disk. A compressed size larger than the data, or a header size below the
// fixed 10 bytes, must be rejected before the slice expression.
func TestDecompressInternalFileBadHeader(t *testing.T) {
	tests := []struct {
		name  string
		build func() []byte
	}{
		{"too short", func() []byte { return make([]byte, internalFileHeaderSize-1) }},
		{"header size zero", func() []byte {
			data := internalFile(0, 4, []byte("abcd"))
			data[0] = 0

			return data
		}},
		{"header size beyond data", func() []byte {
			data := internalFile(0, 4, []byte("abcd"))
			data[0] = 255

			return data
		}},
		{"compressed size beyond data", func() []byte {
			data := internalFile(0, 4, []byte("abcd"))
			binary.LittleEndian.PutUint32(data[1:5], 1<<20)

			return data
		}},
		{"compressed size is uint32 max", func() []byte {
			data := internalFile(0, 4, []byte("abcd"))
			binary.LittleEndian.PutUint32(data[1:5], math.MaxUint32)

			return data
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decompressInternalFile(tt.build())
			if !errors.Is(err, ErrBadSchema) {
				t.Errorf("error = %v, want ErrBadSchema", err)
			}
		})
	}
}

func TestDecompressInternalFileUnsupportedAlgorithm(t *testing.T) {
	_, err := decompressInternalFile(internalFile(9, 4, []byte("abcd")))
	if !errors.Is(err, ErrCompression) {
		t.Errorf("error = %v, want ErrCompression", err)
	}
}

// TestDecompressInternalFileZlibBomb: the schema path used io.ReadAll with no
// limit, so a page-sized stream could expand to hundreds of megabytes. The
// declared decompressed size was parsed and thrown away; now it bounds the
// output.
func TestDecompressInternalFileZlibBomb(t *testing.T) {
	stream := zlibBytes(t, make([]byte, 2<<20))

	tests := []struct {
		name     string
		declared uint32
	}{
		{"understated", 64},
		{"overstated ratio", 64 << 20},
		{"overstated ceiling", 1 << 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decompressInternalFile(internalFile(1, tt.declared, stream))
			if err == nil {
				t.Fatalf("decompressInternalFile() returned %d bytes, want an error", len(got))
			}

			if !errors.Is(err, ErrCompression) {
				t.Errorf("error = %v, want ErrCompression", err)
			}
		})
	}
}

// TestParseSchemaColumnCountBounds: the column count is a uint32 off disk and
// used to reach make() bounded only by 65000, so four bytes of input could ask
// for 65000 Column values.
func TestParseSchemaColumnCountBounds(t *testing.T) {
	tests := []struct {
		name  string
		count uint32
		extra int
	}{
		{"uint32 max", math.MaxUint32, 0},
		{"above the ceiling", maxSchemaColumns + 1, 0},
		{"more columns than bytes", 1000, 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 4+tt.extra)
			binary.LittleEndian.PutUint32(data[0:4], tt.count)

			_, err := parseSchema(data)
			if !errors.Is(err, ErrBadSchema) {
				t.Errorf("error = %v, want ErrBadSchema", err)
			}
		})
	}
}

func TestParseSchemaTruncated(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		_, err := parseSchema(make([]byte, n))
		if !errors.Is(err, ErrBadSchema) {
			t.Errorf("parseSchema(%d bytes) error = %v, want ErrBadSchema", n, err)
		}
	}

	// One column announced, but no terminator anywhere in the blob.
	data := make([]byte, 4+64)
	binary.LittleEndian.PutUint32(data[0:4], 1)

	_, err := parseSchema(data)
	if !errors.Is(err, ErrBadSchema) {
		t.Errorf("error = %v, want ErrBadSchema", err)
	}
}

// TestParseSchemaArbitraryInput is a cheap fuzz-style smoke test: no byte
// sequence may panic the schema parser.
func TestParseSchemaArbitraryInput(t *testing.T) {
	seed := []byte{
		0x02, 0x00, 0x00, 0x00, 0x03, 'a', 'b', 'c', 0x01, 0x00, 0x00, 0x00,
		0x12, 0x21, 0xFF, 0xFF, 0xFF, 0x7F, 0x00, 0x12, 0xFF, 0x00, 0x7F, 0x00,
	}

	for i := range seed {
		for _, b := range []byte{0x00, 0x7F, 0xFF} {
			mutated := bytes.Clone(seed)
			mutated[i] = b

			_, _ = parseSchema(mutated)
			_, _ = decompressInternalFile(mutated)
		}
	}
}
