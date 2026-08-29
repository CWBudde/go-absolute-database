package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/cwbudde/go-absolute-database/internal/zlib1"
)

// BaseFieldType represents the low-level storage type (TABSBaseFieldType).
type BaseFieldType byte

const (
	BftUnknown     BaseFieldType = 0
	BftChar        BaseFieldType = 1
	BftWideChar    BaseFieldType = 2
	BftVarchar     BaseFieldType = 3
	BftWideVarchar BaseFieldType = 4
	BftInt8        BaseFieldType = 5
	BftInt16       BaseFieldType = 6
	BftInt32       BaseFieldType = 7
	BftInt64       BaseFieldType = 8
	BftUint8       BaseFieldType = 9
	BftUint16      BaseFieldType = 10
	BftUint32      BaseFieldType = 11
	BftSingle      BaseFieldType = 12
	BftDouble      BaseFieldType = 13
	BftExtended    BaseFieldType = 14
	BftDate        BaseFieldType = 15
	BftTime        BaseFieldType = 16
	BftDateTime    BaseFieldType = 17
	BftBlob        BaseFieldType = 18
	BftClob        BaseFieldType = 19
	BftWideClob    BaseFieldType = 20
	BftLogical     BaseFieldType = 21
	BftCurrency    BaseFieldType = 22
	BftBytes       BaseFieldType = 23
	BftVarBytes    BaseFieldType = 24
)

// FieldType represents the high-level field type (TABSAdvancedFieldType).
type FieldType byte

const (
	FieldUnknown       FieldType = 0
	FieldChar          FieldType = 1
	FieldString        FieldType = 2
	FieldWideChar      FieldType = 3
	FieldWideString    FieldType = 4
	FieldShortInt      FieldType = 5
	FieldSmallInt      FieldType = 6
	FieldInteger       FieldType = 7
	FieldLargeInt      FieldType = 8
	FieldByte          FieldType = 9
	FieldWord          FieldType = 10
	FieldCardinal      FieldType = 11
	FieldAutoInc       FieldType = 12
	FieldAutoIncInt8   FieldType = 13
	FieldAutoIncInt16  FieldType = 14
	FieldAutoIncInt32  FieldType = 15
	FieldAutoIncInt64  FieldType = 16
	FieldAutoIncUint8  FieldType = 17
	FieldAutoIncUint16 FieldType = 18
	FieldAutoIncUint32 FieldType = 19
	FieldSingle        FieldType = 20
	FieldDouble        FieldType = 21
	FieldExtended      FieldType = 22
	FieldBoolean       FieldType = 23
	FieldCurrency      FieldType = 24
	FieldDate          FieldType = 25
	FieldTime          FieldType = 26
	FieldDateTime      FieldType = 27
	FieldTimeStamp     FieldType = 28
	FieldBytes         FieldType = 29
	FieldVarBytes      FieldType = 30
	FieldBLOB          FieldType = 31
	FieldGraphic       FieldType = 32
	FieldMemo          FieldType = 33
	FieldFmtMemo       FieldType = 34
	FieldWideMemo      FieldType = 35
	FieldGUID          FieldType = 36
)

//go:generate stringer -type=FieldType

// fieldTypeNames maps each known field type to its name. This is a table
// rather than a switch because a 30-arm switch that only ever returns a
// constant carries no logic worth reading as control flow.
var fieldTypeNames = map[FieldType]string{
	FieldUnknown:    "Unknown",
	FieldChar:       "Char",
	FieldString:     "String",
	FieldWideChar:   "WideChar",
	FieldWideString: "WideString",
	FieldShortInt:   "ShortInt",
	FieldSmallInt:   "SmallInt",
	FieldInteger:    "Integer",
	FieldLargeInt:   "LargeInt",
	FieldByte:       "Byte",
	FieldWord:       "Word",
	FieldCardinal:   "Cardinal",
	FieldAutoInc:    "AutoInc",
	FieldSingle:     "Single",
	FieldDouble:     "Double",
	FieldExtended:   "Extended",
	FieldBoolean:    "Boolean",
	FieldCurrency:   "Currency",
	FieldDate:       "Date",
	FieldTime:       "Time",
	FieldDateTime:   "DateTime",
	FieldTimeStamp:  "TimeStamp",
	FieldBytes:      "Bytes",
	FieldVarBytes:   "VarBytes",
	FieldBLOB:       "BLOB",
	FieldGraphic:    "Graphic",
	FieldMemo:       "Memo",
	FieldFmtMemo:    "FmtMemo",
	FieldWideMemo:   "WideMemo",
	FieldGUID:       "GUID",
}

// String returns the field type name.
func (ft FieldType) String() string {
	if name, ok := fieldTypeNames[ft]; ok {
		return name
	}

	return fmt.Sprintf("FieldType(%d)", int(ft))
}

// Column describes a single column in a table.
type Column struct {
	Name      string        // column name
	ID        uint32        // internal column ID
	BaseType  BaseFieldType // low-level storage type
	FieldType FieldType     // high-level field type
	Size      uint32        // max size for variable-length types (string length, etc.)
	Position  int           // 0-based position in the column list

	// hasDefault records that this column's definition carries a present
	// DEFAULT value. The value itself is not decoded -- nothing here needs it
	// -- but its presence is what tells a caller that re-serializing this
	// column would drop a clause the engine wrote. It is unexported because a
	// caller building a Column for CreateTable has no default to declare;
	// only parseColumnDef ever sets it.
	hasDefault bool

	// autoInc carries TABSFieldDef's five autoinc parameters, which sit
	// between the column's size (or its BLOB settings) and its DEFAULT. Every
	// column in the corpus stores the engine's defaults there, so what this
	// exists for is to notice one that does not: serializeColumnDef writes the
	// defaults unconditionally and would silently drop a real INCREMENT.
	autoInc autoIncOptions

	// blob carries the BLOB compression settings, present only on a Blob,
	// Clob or WideClob column.
	blob blobSettings

	// notNull and nullabilityKnown carry what the table's constraint array
	// said about this column. Nullability is not in the column definition at
	// all -- CNone.A and CNotNull.A in Constraints.abs differ only by their
	// object id -- so it comes from the kind-3 constraint record, and only
	// Table.Schema, which has the whole stream, can fill it in. Both are
	// unexported so that a caller building a Column for CreateTable cannot
	// declare a constraint this package has no way to write, and so that
	// re-serializing a parsed column cannot lose one: it was never in the
	// bytes serializeColumnDef writes.
	notNull          bool
	nullabilityKnown bool
}

// autoIncOptions is the AUTOINC clause's five parameters as TABSFieldDef
// declares them. known distinguishes a column whose block was read from one
// built by a caller, whose zero value is not what the engine writes.
type autoIncOptions struct {
	increment    int64
	initialValue int64
	minValue     int64
	maxValue     int64
	cycled       bool
	known        bool
}

// engineDefault reports whether these are the values every column in the
// corpus carries, which are the only ones serializeColumnDef can write.
func (o autoIncOptions) engineDefault() bool {
	return !o.known ||
		(o.increment == 1 && o.initialValue == 0 && o.minValue == 0 &&
			o.maxValue == math.MaxInt64 && !o.cycled)
}

// blobSettings is a BLOB column's compression declaration.
type blobSettings struct {
	algorithm byte
	mode      byte
	blockSize uint32
	known     bool
}

// IsBLOB returns true if this column stores BLOB data (Memo, Graphic, etc.).
func (c Column) IsBLOB() bool {
	return c.BaseType == BftBlob || c.BaseType == BftClob || c.BaseType == BftWideClob
}

// IsAutoInc reports whether the engine numbers this column itself.
//
// The field types run from FieldAutoInc through FieldAutoIncUint32, one per
// integer width, and every AUTOINC column in the corpus is the first of them.
// The narrower ones are recognised here so that a writer can refuse them by
// name rather than mistake them for ordinary integers and leave the column's
// counter stale; see writer_autoinc.go.
func (c Column) IsAutoInc() bool {
	return c.FieldType >= FieldAutoInc && c.FieldType <= FieldAutoIncUint32
}

// NotNull reports whether a NOT NULL constraint record names this column.
//
// known is false when the table's constraint array was not read -- because the
// column came from parseSchema directly, or because the schema tail did not
// parse -- and it is the difference between "this column is nullable" and
// "this was never established". A caller that treats an unknown as nullable is
// making the guess this package otherwise refuses to make for it.
func (c Column) NotNull() (notNull, known bool) {
	return c.notNull, c.nullabilityKnown
}

// HasDefault reports whether this column's definition carries a DEFAULT value.
// The value itself is not decoded; what this answers is whether re-serializing
// the column would drop a clause the engine wrote, which is why CreateTable
// and compaction refuse a column that has one.
func (c Column) HasDefault() bool {
	return c.hasDefault
}

// TableSchema holds the parsed schema for one table.
type TableSchema struct {
	Columns []Column
}

const (
	// internalFileHeaderSize is the size of the TABSInternalFileHeader that
	// prefixes an internal file (such as the schema) stored in a page.
	internalFileHeaderSize = 10

	// autoIncBlockSize is the width of TABSFieldDef's five autoinc fields:
	// four int64s and a ByteBool. See docs/format/schema.md.
	autoIncBlockSize = 33

	// blobSettingsSize is the width of a BLOB column's compression
	// declaration: algorithm, mode and an int32 block size.
	blobSettingsSize = 6

	// minColumnDefSize is the smallest number of bytes a column definition can
	// occupy: an empty name (1) + ID (4) + types (2) + size (4) + the autoinc
	// block (33) + a DEFAULT that is a type tag and an absent marker (2). It
	// bounds the column count against the amount of data actually present, so
	// a truncated blob cannot request a huge slice.
	minColumnDefSize = 1 + 4 + 2 + 4 + autoIncBlockSize + 2

	// maxSchemaColumns is an absolute ceiling on the column count.
	maxSchemaColumns = 65000
)

var (
	ErrNoSchema    = errors.New("absdb: no schema page found")
	ErrBadSchema   = errors.New("absdb: malformed schema data")
	ErrCompression = errors.New("absdb: decompression failed")
)

// Schema reads and parses the schema of the database's only table. It reports
// ErrAmbiguousTable when the file holds more than one; use Table to name it.
func (db *File) Schema() (*TableSchema, error) {
	t, err := db.Table("")
	if err != nil {
		return nil, err
	}

	return t.Schema()
}

// Schema reads and parses this table's column definitions.
func (t *Table) Schema() (*TableSchema, error) {
	schemaPageNo, err := t.schemaPageNo()
	if err != nil {
		return nil, err
	}

	page, err := t.db.ReadPage(schemaPageNo)
	if err != nil {
		return nil, err
	}

	// Read the internal file header at the start of page data.
	data := page.PageData()
	if len(data) < internalFileHeaderSize {
		return nil, ErrBadSchema
	}

	decompressed, err := decompressInternalFile(data)
	if err != nil {
		return nil, err
	}

	schema, err := parseSchema(decompressed)
	if err != nil {
		return nil, err
	}

	applyNullability(schema, decompressed)

	return schema, nil
}

// applyNullability fills in each column's NotNull from the schema stream's
// constraint array.
//
// It is best-effort by design: a tail this package cannot read leaves every
// column's nullability simply unknown, because Schema has always succeeded on
// files whose tail parseSchemaTail refuses, and it must keep doing so. The
// alternative -- reporting every column of such a table as nullable -- would
// be indistinguishable from a table that really has no NOT NULL anywhere.
func applyNullability(schema *TableSchema, stream []byte) {
	constraints, ok := tailConstraints(stream)
	if !ok {
		return
	}

	for i := range schema.Columns {
		schema.Columns[i].nullabilityKnown = true
	}

	for _, rec := range constraints {
		if rec.kind != constraintNotNull {
			continue
		}

		for i := range schema.Columns {
			if rec.namesColumn(schema.Columns[i].Name) {
				schema.Columns[i].notNull = true
			}
		}
	}
}

// schemaPageNo resolves the page holding this table's column definitions. The
// catalog names it outright; a file without one falls back to the first schema
// page, which is what this package did before the catalog was parsed.
func (t *Table) schemaPageNo() (int, error) {
	if !t.unlisted {
		return t.info.SchemaPageNo, nil
	}

	no, err := t.db.findPageByType(PageTypeSchema)
	if err != nil {
		return 0, err
	}

	if no < 0 {
		return 0, ErrNoSchema
	}

	return no, nil
}

// decompressInternalFile reads the TABSInternalFileHeader and decompresses the payload.
// If the page chains to additional pages, those are not yet supported.
func decompressInternalFile(data []byte) ([]byte, error) {
	// TABSInternalFileHeader (packed):
	//   FileHeaderSize       byte    offset 0
	//   FileSize             int32   offset 1  (compressed size)
	//   DecompressedSize     int32   offset 5
	//   CompressionAlgorithm byte    offset 9
	if len(data) < internalFileHeaderSize {
		return nil, ErrBadSchema
	}

	// All three lengths are widened to int64 before any arithmetic: they come
	// straight off disk and must not be able to overflow or go negative on the
	// way to a slice expression.
	fileHdrSize := int64(data[0])
	compressedSize := int64(binary.LittleEndian.Uint32(data[1:5]))
	decompressedSize := int64(binary.LittleEndian.Uint32(data[5:9]))
	compressionAlgo := data[9]

	if fileHdrSize < internalFileHeaderSize || fileHdrSize+compressedSize > int64(len(data)) {
		return nil, fmt.Errorf("%w: internal file header invalid", ErrBadSchema)
	}

	compressed := data[fileHdrSize : fileHdrSize+compressedSize]

	switch compressionAlgo {
	case 0: // no compression
		result := make([]byte, len(compressed))
		copy(result, compressed)

		return result, nil
	case 1: // zlib
		// The declared decompressed size bounds the output: without it a
		// crafted page could inflate far beyond the file it came from.
		result, err := inflateLimited(compressed, decompressedSize, internalFileInflateBounds)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCompression, err)
		}

		return result, nil
	default:
		return nil, fmt.Errorf("%w: unsupported compression algorithm %d", ErrCompression, compressionAlgo)
	}
}

// compressInternalFile is decompressInternalFile's mirror: it prefixes raw
// with a TABSInternalFileHeader and appends the compressed (algorithm 1) or
// copied (algorithm 0) payload.
//
// Algorithm 1 uses internal/zlib1.Compress, a deflate encoder that reproduces
// the C zlib library at level 1 byte for byte -- the compressor the engine
// itself uses for every compressed internal file in the corpus (see
// TestZlib1ReproducesEveryCorpusStream and this file's own
// TestCompressInternalFileReproducesCorpus). Go's compress/zlib cannot stand
// in for it; see the file comment in ddl.go for why.
func compressInternalFile(raw []byte, algo byte) ([]byte, error) {
	var payload []byte

	switch algo {
	case 0: // no compression
		payload = raw
	case 1: // zlib
		payload = zlib1.Compress(raw)
	default:
		return nil, fmt.Errorf("%w: unsupported compression algorithm %d", ErrCompression, algo)
	}

	if len(payload) > math.MaxInt32 || len(raw) > math.MaxInt32 {
		return nil, fmt.Errorf("%w: internal file of %d bytes is too large to encode", ErrCompression, len(raw))
	}

	out := make([]byte, internalFileHeaderSize+len(payload))
	out[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(out[1:5], uint32(len(payload))) //nolint:gosec // bounded above
	binary.LittleEndian.PutUint32(out[5:9], uint32(len(raw)))     //nolint:gosec // bounded above
	out[9] = algo

	copy(out[internalFileHeaderSize:], payload)

	return out, nil
}

// parseSchema parses the decompressed schema blob into a TableSchema.
func parseSchema(data []byte) (*TableSchema, error) {
	if len(data) < 4 {
		return nil, ErrBadSchema
	}

	columnCount := int64(binary.LittleEndian.Uint32(data[0:4]))
	if columnCount > maxSchemaColumns {
		return nil, fmt.Errorf("%w: invalid column count %d", ErrBadSchema, columnCount)
	}

	// The blob must be long enough to hold that many definitions. Without this
	// a 4-byte input could ask for 65000 Column values.
	if columnCount > int64(len(data)/minColumnDefSize) {
		return nil, fmt.Errorf("%w: column count %d exceeds %d bytes of schema data",
			ErrBadSchema, columnCount, len(data))
	}

	columns := make([]Column, 0, columnCount)
	pos := 4

	for i := range int(columnCount) {
		col, nextPos, err := parseColumnDef(data, pos, i)
		if err != nil {
			return nil, fmt.Errorf("%w: column %d: %w", ErrBadSchema, i, err)
		}

		columns = append(columns, col)
		pos = nextPos
	}

	return &TableSchema{Columns: columns}, nil
}

// parseColumnDef parses a single column definition from the schema blob.
// Returns the column and the byte position of the next column.
func parseColumnDef(data []byte, pos int, index int) (Column, int, error) {
	if pos >= len(data) {
		return Column{}, 0, fmt.Errorf("unexpected end of data at position %d", pos)
	}

	// Name: Pascal-style length-prefixed string.
	nameLen := int(data[pos])

	pos++
	if pos+nameLen > len(data) {
		return Column{}, 0, errors.New("name extends beyond data")
	}

	// Column names are Windows-1252 like every other string field on disk;
	// a name such as "Größe" is mojibake if taken as raw bytes.
	name := decodeANSI(data[pos : pos+nameLen])
	pos += nameLen

	// Column ID (uint32).
	if pos+4 > len(data) {
		return Column{}, 0, errors.New("truncated column ID")
	}

	colID := binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	// Base field type (1 byte) + Advanced field type (1 byte).
	if pos+2 > len(data) {
		return Column{}, 0, errors.New("truncated field types")
	}

	baseType := BaseFieldType(data[pos])
	advType := FieldType(data[pos+1])
	pos += 2

	// Size (uint32).
	if pos+4 > len(data) {
		return Column{}, 0, errors.New("truncated size")
	}

	size := binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	// BLOB types carry their compression settings here, before the autoinc
	// block: TABSFieldDef declares BLOBCompressionAlgorithm,
	// BLOBCompressionMode and BLOBBlockSize in that order.
	var blob blobSettings

	if baseType == BftBlob || baseType == BftClob || baseType == BftWideClob {
		if pos+blobSettingsSize > len(data) {
			return Column{}, 0, errors.New("truncated BLOB settings")
		}

		blob = blobSettings{
			algorithm: data[pos],
			mode:      data[pos+1],
			blockSize: binary.LittleEndian.Uint32(data[pos+2 : pos+6]),
			known:     true,
		}
		pos += blobSettingsSize
	}

	if pos+autoIncBlockSize > len(data) {
		return Column{}, 0, errors.New("truncated autoinc options")
	}

	autoInc := autoIncOptions{
		increment:    int64(binary.LittleEndian.Uint64(data[pos : pos+8])),
		initialValue: int64(binary.LittleEndian.Uint64(data[pos+8 : pos+16])),
		minValue:     int64(binary.LittleEndian.Uint64(data[pos+16 : pos+24])),
		maxValue:     int64(binary.LittleEndian.Uint64(data[pos+24 : pos+32])),
		cycled:       data[pos+32] != 0,
		known:        true,
	}
	pos += autoIncBlockSize

	// The DEFAULT clause closes the definition as a typed value: a
	// TABSVariant type tag, then the present/absent flag and its payload.
	if pos >= len(data) {
		return Column{}, 0, errors.New("truncated default value tag")
	}

	pos++ // the variant's type tag, which echoes baseType or is 0x00

	pos, hasDefault, ok := columnDefaultEnd(data, pos)
	if !ok {
		return Column{}, 0, errors.New("malformed default value")
	}

	col := Column{
		Name:       name,
		ID:         colID,
		BaseType:   baseType,
		FieldType:  advType,
		Size:       size,
		Position:   index,
		hasDefault: hasDefault,
		autoInc:    autoInc,
		blob:       blob,
	}

	return col, pos, nil
}

// columnDefaultEnd returns the position just past the DEFAULT field at pos,
// whether that field held a value, and false when it does not read as a DEFAULT
// at all. It is readTypedValue's second half: the base type is the terminator's
// own middle byte, so only the present/absent flag and the counted payload are
// left here.
func columnDefaultEnd(data []byte, pos int) (end int, present, ok bool) {
	if pos >= len(data) {
		return 0, false, false
	}

	switch data[pos] {
	case typedValueAbsent:
		return pos + 1, false, true
	case typedValuePresent:
		if pos+5 > len(data) {
			return 0, false, false
		}

		size := int64(binary.LittleEndian.Uint32(data[pos+1 : pos+5]))

		end := int64(pos) + 5 + size
		if end > int64(len(data)) {
			return 0, false, false
		}

		return int(end), true, true
	default:
		return 0, false, false
	}
}
