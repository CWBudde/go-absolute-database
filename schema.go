package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
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

// String returns the field type name.
func (ft FieldType) String() string {
	switch ft {
	case FieldUnknown:
		return "Unknown"
	case FieldChar:
		return "Char"
	case FieldString:
		return "String"
	case FieldWideChar:
		return "WideChar"
	case FieldWideString:
		return "WideString"
	case FieldShortInt:
		return "ShortInt"
	case FieldSmallInt:
		return "SmallInt"
	case FieldInteger:
		return "Integer"
	case FieldLargeInt:
		return "LargeInt"
	case FieldByte:
		return "Byte"
	case FieldWord:
		return "Word"
	case FieldCardinal:
		return "Cardinal"
	case FieldAutoInc:
		return "AutoInc"
	case FieldSingle:
		return "Single"
	case FieldDouble:
		return "Double"
	case FieldExtended:
		return "Extended"
	case FieldBoolean:
		return "Boolean"
	case FieldCurrency:
		return "Currency"
	case FieldDate:
		return "Date"
	case FieldTime:
		return "Time"
	case FieldDateTime:
		return "DateTime"
	case FieldTimeStamp:
		return "TimeStamp"
	case FieldBytes:
		return "Bytes"
	case FieldVarBytes:
		return "VarBytes"
	case FieldBLOB:
		return "BLOB"
	case FieldGraphic:
		return "Graphic"
	case FieldMemo:
		return "Memo"
	case FieldFmtMemo:
		return "FmtMemo"
	case FieldWideMemo:
		return "WideMemo"
	case FieldGUID:
		return "GUID"
	default:
		return fmt.Sprintf("FieldType(%d)", int(ft))
	}
}

// Column describes a single column in a table.
type Column struct {
	Name      string        // column name
	ID        uint32        // internal column ID
	BaseType  BaseFieldType // low-level storage type
	FieldType FieldType     // high-level field type
	Size      uint32        // max size for variable-length types (string length, etc.)
	Position  int           // 0-based position in the column list
}

// IsBLOB returns true if this column stores BLOB data (Memo, Graphic, etc.).
func (c Column) IsBLOB() bool {
	return c.BaseType == BftBlob || c.BaseType == BftClob || c.BaseType == BftWideClob
}

// TableSchema holds the parsed schema for one table.
type TableSchema struct {
	Columns []Column
}

const (
	// internalFileHeaderSize is the size of the TABSInternalFileHeader that
	// prefixes an internal file (such as the schema) stored in a page.
	internalFileHeaderSize = 10

	// minColumnDefSize is the smallest number of bytes a column definition can
	// occupy: an empty name (1) + ID (4) + types (2) + size (4) + flags (1) +
	// the 4-byte terminator. It bounds the column count against the amount of
	// data actually present, so a truncated blob cannot request a huge slice.
	minColumnDefSize = 16

	// maxSchemaColumns is an absolute ceiling on the column count.
	maxSchemaColumns = 65000
)

var (
	ErrNoSchema    = errors.New("absdb: no schema page found")
	ErrBadSchema   = errors.New("absdb: malformed schema data")
	ErrCompression = errors.New("absdb: decompression failed")
)

// Schema reads and parses the table schema from the database file.
// For single-table databases, this returns the schema of the only table.
func (db *File) Schema() (*TableSchema, error) {
	// Find the schema page (type 8).
	schemaPageNo, err := db.findPageByType(PageTypeSchema)
	if err != nil {
		return nil, err
	}

	if schemaPageNo < 0 {
		return nil, ErrNoSchema
	}

	page, err := db.ReadPage(schemaPageNo)
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

	return parseSchema(decompressed)
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
		result, err := inflateLimited(compressed, decompressedSize)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCompression, err)
		}

		return result, nil
	default:
		return nil, fmt.Errorf("%w: unsupported compression algorithm %d", ErrCompression, compressionAlgo)
	}
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

	// Flags (1 byte).
	if pos >= len(data) {
		return Column{}, 0, errors.New("truncated flags")
	}

	pos++ // skip flags byte

	// BLOB types have 6 extra bytes of BLOB descriptor info.
	if baseType == BftBlob || baseType == BftClob || baseType == BftWideClob {
		if pos+6 > len(data) {
			return Column{}, 0, errors.New("truncated BLOB info")
		}

		pos += 6
	}

	// After the flags (and optional BLOB info), there is variable-length padding
	// (zeros followed by 0xFF bytes), terminated by a 4-byte sequence:
	//   0x7F 0x00 <byte> 0xFF
	// where <byte> is either the baseType echo or 0x00 (varies by version).
	found := false

	for i := pos; i+3 < len(data); i++ {
		if data[i] == 0x7F && data[i+1] == 0x00 && data[i+3] == 0xFF {
			mid := data[i+2]
			if mid == byte(baseType) || mid == 0x00 {
				pos = i + 4
				found = true

				break
			}
		}
	}

	if !found {
		return Column{}, 0, errors.New("column terminator not found")
	}

	col := Column{
		Name:      name,
		ID:        colID,
		BaseType:  baseType,
		FieldType: advType,
		Size:      size,
		Position:  index,
	}

	return col, pos, nil
}
