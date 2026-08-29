package absdb

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/text/encoding/charmap"
)

var (
	ErrNoData     = errors.New("absdb: no data pages found")
	ErrNoMoreRows = errors.New("absdb: no more rows")
	ErrBadLayout  = errors.New("absdb: record layout does not match the data")
)

// Reader iterates over data records in a table.
//
// The record layout is derived from the schema, not searched for:
//
//	nullFlagBytes  = ceil(numColumns / 8)          // spare high bits are set
//	fieldDataSize  = sum(fieldStoreSize(col))
//	recordSize     = nullFlagBytes + fieldDataSize // no trailer, no padding
//	recordsPerPage = max n with ceil(n/8) + n*recordSize <= payload
//	bitmapBytes    = ceil(recordsPerPage / 8)
//
// A data page therefore starts with a bitmapBytes-long occupancy bitmap (bit
// set = slot occupied), followed by recordsPerPage fixed-size record slots.
type Reader struct {
	db     *File
	table  *Table
	schema *TableSchema

	// Record layout.
	nullFlagBytes   int // bytes of null flags in front of every record
	fieldDataSize   int // total bytes for all field values
	recordSize      int // nullFlagBytes + fieldDataSize
	fieldOffsets    []int
	fieldStoreSizes []int

	// Page layout.
	recordsPerPage int // record slots carried by one page payload
	bitmapBytes    int // occupancy bitmap in front of the first slot

	// Data pages.
	dataPages []int // page numbers with type 10

	// Iteration state.
	pageIdx   int // current data page index
	pageData  []byte
	recordIdx int // current record slot within the page
	started   bool
	err       error
}

// OpenTable creates a Reader over the database's only table. It reports
// ErrAmbiguousTable when the file holds more than one; use Table to name it.
func (db *File) OpenTable() (*Reader, error) {
	t, err := db.Table("")
	if err != nil {
		return nil, err
	}

	return t.Open()
}

// Open creates a Reader for this table's data records. A table without data
// pages yields a valid Reader whose Next reports no rows.
func (t *Table) Open() (*Reader, error) {
	schema, err := t.Schema()
	if err != nil {
		return nil, err
	}

	dataPages, err := t.dataPages()
	if err != nil {
		return nil, err
	}

	r := &Reader{
		db:        t.db,
		table:     t,
		schema:    schema,
		dataPages: dataPages,
	}
	r.computeLayout()

	err = r.validateLayout()
	if err != nil {
		return nil, err
	}

	return r, nil
}

// Schema returns the table schema.
func (r *Reader) Schema() *TableSchema {
	return r.schema
}

// Next advances to the next occupied record slot. Returns false when no more
// records are available. Record may be called any number of times per Next.
func (r *Reader) Next() bool {
	if r.err != nil || len(r.dataPages) == 0 || r.recordSize <= 0 {
		return false
	}

	if !r.started {
		r.started = true
		r.pageIdx = 0

		err := r.loadPage()
		if err != nil {
			r.err = err
			return false
		}
	} else {
		r.recordIdx++
	}

	return r.seekOccupied()
}

// Err returns any error encountered during iteration.
func (r *Reader) Err() error {
	return r.err
}

// Record returns the record the Reader is currently positioned on. It has no
// side effects: calling it twice for the same Next returns the same record.
// Before the first Next, or after Next reported false, it returns a record
// whose columns all read as NULL.
func (r *Reader) Record() Record {
	start, ok := r.recordStart(r.recordIdx)
	if !ok {
		return Record{reader: r}
	}

	fieldStart := start + r.nullFlagBytes

	return Record{
		reader:    r,
		nullFlags: r.pageData[start:fieldStart],
		fieldData: r.pageData[fieldStart : fieldStart+r.fieldDataSize],
	}
}

// Record represents a single data record.
type Record struct {
	reader    *Reader
	nullFlags []byte
	fieldData []byte
}

// IsNull returns true if the column at the given index is null.
func (rec Record) IsNull(col int) bool {
	if rec.reader == nil || rec.reader.schema == nil {
		return true
	}

	if col < 0 || col >= len(rec.reader.schema.Columns) {
		return true
	}

	byteIdx := col / 8
	if byteIdx >= len(rec.nullFlags) {
		return true
	}

	// Bit = 1 means null.
	return rec.nullFlags[byteIdx]&(1<<uint(col%8)) != 0
}

// Int returns the value of an integer column widened to int32. Narrower
// columns (Int8, Uint8, Int16, Uint16) are widened keeping their own sign.
// Columns that do not store a plain integer return 0, and so do values that do
// not fit an int32 — an Int64 column, or a Uint32 column above MaxInt32. Use
// Int64 to read those without loss.
func (rec Record) Int(col int) int32 {
	v := rec.intValue(col)
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0
	}

	return int32(v)
}

// Int16 returns the value of a SmallInt column. Columns that do not store a
// plain integer return 0, and so do values outside the int16 range — reading a
// wider column through Int16 yields 0 rather than its truncated low bytes.
func (rec Record) Int16(col int) int16 {
	v := rec.intValue(col)
	if v < math.MinInt16 || v > math.MaxInt16 {
		return 0
	}

	return int16(v)
}

// Int64 returns the value of an integer column widened to int64. A Currency
// column is not an integer column -- it stores a double -- and reads 0 here;
// use Float.
func (rec Record) Int64(col int) int64 {
	return rec.intValue(col)
}

// Uint16 returns the value of a Word column. Columns that do not store a plain
// integer return 0, and so do values outside the uint16 range — including the
// negative value of a signed column, which is not reinterpreted as a large
// unsigned one.
func (rec Record) Uint16(col int) uint16 {
	v := rec.intValue(col)
	if v < 0 || v > math.MaxUint16 {
		return 0
	}

	return uint16(v)
}

// Uint32 returns the value of an unsigned integer column. Narrower columns are
// zero-extended. Columns that do not store a plain integer return 0, and so do
// values outside the uint32 range — including the negative value of a signed
// column, which is not reinterpreted as a large unsigned one.
func (rec Record) Uint32(col int) uint32 {
	v := rec.intValue(col)
	if v < 0 || v > math.MaxUint32 {
		return 0
	}

	return uint32(v)
}

// Float returns the floating-point value of the column: Single is read as a
// 4-byte IEEE-754 value, Double and Currency as 8-byte ones. Extended
// (80-bit) and all other columns return 0.
func (rec Record) Float(col int) float64 {
	c, ok := rec.column(col)
	if !ok {
		return 0
	}

	switch c.BaseType {
	case BftSingle:
		if raw := rec.fieldPrefix(col, 4); raw != nil {
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(raw)))
		}
	case BftDouble:
		if raw := rec.fieldPrefix(col, 8); raw != nil {
			return math.Float64frombits(binary.LittleEndian.Uint64(raw))
		}
	case BftCurrency:
		// Currency is an IEEE-754 double on disk, not the scaled int64 a
		// Delphi Currency is in memory. Types.abs settles it: TReal.R4 holds
		// 8765.4321 as 4d 84 0d 4f b7 1e c1 40, which is that double exactly
		// and is nothing like the scaled 87654321. ABSTypes.hpp agrees --
		// TABSCurrency is a typedef of double.
		if raw := rec.fieldPrefix(col, 8); raw != nil {
			return math.Float64frombits(binary.LittleEndian.Uint64(raw))
		}
	}

	return 0
}

// String returns the string value of the column. Char and Varchar columns are
// decoded from Windows-1252, WideChar and WideVarchar from UTF-16LE; both stop
// at the first null terminator. Other columns return the empty string — BLOB
// and CLOB columns hold only a reference, use Memo to read their text.
func (rec Record) String(col int) string {
	c, ok := rec.column(col)
	if !ok {
		return ""
	}

	raw := rec.field(col)
	if len(raw) == 0 {
		return ""
	}

	switch c.BaseType {
	case BftChar, BftVarchar:
		return decodeANSI(raw)
	case BftWideChar, BftWideVarchar:
		return decodeUTF16(raw)
	default:
		return ""
	}
}

// Bool returns the boolean value of the column. WordBool is 2 bytes, non-zero
// meaning true.
func (rec Record) Bool(col int) bool {
	raw := rec.fieldPrefix(col, 2)
	if raw == nil {
		return false
	}

	return binary.LittleEndian.Uint16(raw) != 0
}

// Time returns the time.Time value of a Date, Time, or DateTime column.
// Any other column, and any truncated field, yields the zero time.
//
// A TimeStamp column reads as the zero time even though it shares
// BftDateTime's base type: Types.abs shows it stores some other layout --
// "2019-03-07 01:02:03" is e3 07 03 00 07 00 01 00, which reads as the
// numbers 2019, 3, 7, 1 rather than as a day count and a millisecond count --
// and returning a confidently wrong instant is worse than returning none.
func (rec Record) Time(col int) time.Time {
	c, ok := rec.column(col)
	if !ok || c.FieldType == FieldTimeStamp {
		return time.Time{}
	}

	switch c.BaseType {
	case BftDate:
		// Date stored as int32 (days since epoch).
		if raw := rec.fieldPrefix(col, 4); raw != nil {
			return delphiDateToTime(int32(binary.LittleEndian.Uint32(raw)))
		}
	case BftTime:
		// Time stored as int32 (milliseconds since midnight).
		if raw := rec.fieldPrefix(col, 4); raw != nil {
			return delphiTimeToTime(int32(binary.LittleEndian.Uint32(raw)))
		}
	case BftDateTime:
		// DateTime stored as Date(int32) + Time(int32).
		if raw := rec.fieldPrefix(col, 8); raw != nil {
			days := int32(binary.LittleEndian.Uint32(raw[0:4]))
			ms := int32(binary.LittleEndian.Uint32(raw[4:8]))

			return delphiDateToTime(days).Add(time.Duration(ms) * time.Millisecond)
		}
	}

	return time.Time{}
}

// Bytes returns a copy of the raw stored bytes for the column, or nil if the
// column index is out of range or its field is truncated.
func (rec Record) Bytes(col int) []byte {
	raw := rec.field(col)
	if raw == nil {
		return nil
	}

	result := make([]byte, len(raw))
	copy(result, raw)

	return result
}

// guidTextSize is the declared Size of a GUID column: the braced text form
// "{XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX}" is 38 characters.
const guidTextSize = 38

// GUID is a 128-bit globally unique identifier, held in the order it is
// printed: g[0] is the first hex pair of the first group.
//
// The engine does not store the Win32 GUID struct, whatever TABSGuid's
// typedef suggests. A GUID column is a fixed 38-character Char column and
// holds the value as text -- Types.abs's TGuid stores the bytes
// "{3F2504E0-4F89-11D3-9A0C-0305E82C3301}" followed by a NUL -- so there is
// no endianness to reverse and no struct to unpack.
type GUID [16]byte

// String formats the GUID canonically, as 8-4-4-4-12 lowercase hex digits
// with no braces. The zero GUID formats as all zeros.
func (g GUID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", g[0:4], g[4:6], g[6:8], g[8:10], g[10:16])
}

// ParseGUID reads the engine's stored text. Both forms the engine accepts are
// taken -- braced as DBManager writes it, and bare as it stores a bare literal
// -- and anything else reports false rather than a partly filled value.
func ParseGUID(s string) (GUID, bool) {
	var g GUID

	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")

	// 32 hex digits and the four dashes.
	if len(s) != 36 {
		return GUID{}, false
	}

	n := 0

	for i, group := range [5]int{4, 2, 2, 2, 6} {
		if i > 0 {
			if s[0] != '-' {
				return GUID{}, false
			}

			s = s[1:]
		}

		raw, err := hex.DecodeString(s[:group*2])
		if err != nil {
			return GUID{}, false
		}

		n += copy(g[n:], raw)
		s = s[group*2:]
	}

	return g, true
}

// GUID returns the value of a GUID column. Any other column, a NULL, and text
// that is not a GUID all yield the zero GUID, which is what every other typed
// accessor here does for a column it cannot read; use IsNull to tell a NULL
// apart from a zero value.
//
// This is the one accessor that dispatches on FieldType rather than BaseType,
// because it has to: a GUID column stores Char, so BaseType alone cannot tell
// it from any other fixed string. String reads the same column as its raw
// text, braces and all.
func (rec Record) GUID(col int) GUID {
	c, ok := rec.column(col)
	if !ok || c.FieldType != FieldGUID {
		return GUID{}
	}

	g, ok := ParseGUID(rec.String(col))
	if !ok {
		return GUID{}
	}

	return g
}

// --- record field access ---

// column returns the schema column at col, or false if col is out of range.
func (rec Record) column(col int) (Column, bool) {
	r := rec.reader
	if r == nil || r.schema == nil || col < 0 || col >= len(r.schema.Columns) {
		return Column{}, false
	}

	return r.schema.Columns[col], true
}

// field returns the stored bytes of the column, or nil if the column index is
// out of range or the field does not fit in the record's field data.
func (rec Record) field(col int) []byte {
	r := rec.reader
	if r == nil || col < 0 || col >= len(r.fieldOffsets) {
		return nil
	}

	off := r.fieldOffsets[col]

	size := r.fieldStoreSizes[col]
	if off < 0 || size <= 0 || off+size > len(rec.fieldData) {
		return nil
	}

	return rec.fieldData[off : off+size]
}

// fieldPrefix returns the first width bytes of the column's stored bytes, or
// nil if the column is out of range, truncated, or narrower than width.
func (rec Record) fieldPrefix(col, width int) []byte {
	raw := rec.field(col)
	if len(raw) < width {
		return nil
	}

	return raw[:width]
}

// intValue decodes an integer column into an int64, sign-extending signed
// columns. Columns that do not store a plain integer, and fields whose stored
// bytes are truncated, decode to 0 — the same value every accessor built on
// this one reports for them.
func (rec Record) intValue(col int) int64 {
	c, ok := rec.column(col)
	if !ok {
		return 0
	}

	width, signed, ok := integerStorage(c)
	if !ok {
		return 0
	}

	raw := rec.fieldPrefix(col, width)
	if raw == nil {
		return 0
	}

	var v uint64
	for i := width - 1; i >= 0; i-- {
		v = v<<8 | uint64(raw[i])
	}

	bits := uint(width) * 8
	if signed && width < 8 && v&(1<<(bits-1)) != 0 {
		v |= ^uint64(0) << bits
	}

	return int64(v)
}

// integerStorage reports the stored width in bytes and the signedness of an
// integer column. ok is false for columns that do not store a plain integer.
func integerStorage(c Column) (width int, signed, ok bool) {
	switch c.BaseType {
	case BftInt8:
		return 1, true, true
	case BftUint8:
		return 1, false, true
	case BftInt16:
		return 2, true, true
	case BftUint16:
		return 2, false, true
	case BftInt32:
		return 4, true, true
	case BftUint32:
		return 4, false, true
	case BftInt64:
		return 8, true, true
	default:
		return 0, false, false
	}
}

// decodeANSI decodes a null-terminated Windows-1252 string.
func decodeANSI(raw []byte) string {
	end := 0
	for end < len(raw) && raw[end] != 0 {
		end++
	}

	if end == 0 {
		return ""
	}

	decoded, err := charmap.Windows1252.NewDecoder().Bytes(raw[:end])
	if err != nil {
		return string(raw[:end]) // fallback
	}

	return string(decoded)
}

// decodeUTF16 decodes a UTF-16LE string terminated by a 0x0000 code unit.
func decodeUTF16(raw []byte) string {
	units := make([]uint16, 0, len(raw)/2)

	for i := 0; i+1 < len(raw); i += 2 {
		u := binary.LittleEndian.Uint16(raw[i : i+2])
		if u == 0 {
			break
		}

		units = append(units, u)
	}

	if len(units) == 0 {
		return ""
	}

	return string(utf16.Decode(units))
}

// --- layout derivation ---

// computeLayout derives the record and page layout from the schema alone.
func (r *Reader) computeLayout() {
	r.computeRecordLayout()
	r.recordsPerPage = recordsPerPage(r.db.payloadSize(), r.recordSize)
	r.bitmapBytes = (r.recordsPerPage + 7) / 8
}

// computeRecordLayout derives the field offsets, the null-flag prefix and the
// record size. It does not depend on the page size.
func (r *Reader) computeRecordLayout() {
	cols := r.schema.Columns

	r.fieldOffsets = make([]int, len(cols))
	r.fieldStoreSizes = make([]int, len(cols))

	offset := 0

	for i, c := range cols {
		size := fieldStoreSize(c)
		r.fieldOffsets[i] = offset
		r.fieldStoreSizes[i] = size
		offset += size
	}

	r.fieldDataSize = offset
	r.nullFlagBytes = (len(cols) + 7) / 8
	r.recordSize = r.nullFlagBytes + r.fieldDataSize
}

// recordsPerPage returns the largest n for which an n-bit occupancy bitmap plus
// n records of recordSize bytes still fit into payloadLen bytes.
func recordsPerPage(payloadLen, recordSize int) int {
	if recordSize <= 0 || payloadLen <= 0 {
		return 0
	}

	n := payloadLen / recordSize
	for n > 0 && (n+7)/8+n*recordSize > payloadLen {
		n--
	}

	return n
}

// fixedFieldStoreSize gives the number of bytes each fixed-width base type
// occupies in the fixed part of a record. Types whose stored size is derived
// from the column's declared Size are absent and handled by fieldStoreSize.
var fixedFieldStoreSize = map[BaseFieldType]int{
	BftInt8:     1,
	BftUint8:    1,
	BftInt16:    2,
	BftUint16:   2,
	BftInt32:    4,
	BftUint32:   4,
	BftInt64:    8,
	BftSingle:   4,
	BftDouble:   8,
	BftExtended: 10,
	BftCurrency: 8,
	BftLogical:  2, // WordBool
	BftDate:     4,
	BftTime:     4,
	BftDateTime: 8, // Date(4) + Time(4)
	BftBlob:     6, // BLOB reference: PageNo(4) + ItemNo(2)
	BftClob:     6,
	BftWideClob: 6,
}

// fieldStoreSize returns the number of bytes a column occupies in the fixed
// part of a record.
func fieldStoreSize(c Column) int {
	if n, ok := fixedFieldStoreSize[c.BaseType]; ok {
		return n
	}

	// The remaining types size themselves from the column's declared Size.
	switch c.BaseType {
	case BftVarchar, BftChar:
		return int(c.Size) + 1
	case BftWideVarchar, BftWideChar:
		return (int(c.Size) + 1) * 2
	// Both byte types store one byte more than their declared size, exactly
	// as Char and Varchar do. Types.abs pins it with sentinels: in TBin a
	// BYTES(8) and a VARBYTES(8) each occupy nine bytes, and in TGuid a
	// BYTES(16) occupies seventeen, which is what puts the LargeInt sentinel
	// after them where the raw page shows it. What the extra byte holds is not
	// established -- every byte column in the corpus is NULL.
	case BftVarBytes, BftBytes:
		return int(c.Size) + 1
	default:
		return int(c.Size)
	}
}

// validateLayout sanity-checks the derived layout against the first row. The
// engine sets every null-flag bit beyond the last column, so the spare high
// bits of the last null-flag byte must all be 1. This is a check, never a
// search: a mismatch means the layout does not describe this file.
func (r *Reader) validateLayout() error {
	spare := r.nullFlagBytes*8 - len(r.schema.Columns)
	if spare == 0 || len(r.dataPages) == 0 {
		return nil
	}

	found := r.Next()
	rec := r.Record()

	page, slot, err := -1, r.recordIdx, r.err
	if found {
		page = r.dataPages[r.pageIdx]
	}

	// Validating must not consume the first row.
	r.started, r.pageIdx, r.recordIdx, r.pageData, r.err = false, 0, 0, nil, nil

	if !found || len(rec.nullFlags) < r.nullFlagBytes {
		return err
	}

	got := rec.nullFlags[r.nullFlagBytes-1]

	want := byte(0xff) << uint(8-spare)
	if got&want != want {
		return fmt.Errorf("%w: page %d slot %d: last null-flag byte %#02x is missing spare bits %#02x "+
			"(%d columns, %d null-flag bytes, %d-byte records)",
			ErrBadLayout, page, slot, got, want,
			len(r.schema.Columns), r.nullFlagBytes, r.recordSize)
	}

	return nil
}

// --- iteration helpers ---

func (r *Reader) loadPage() error {
	page, err := r.db.ReadPage(r.dataPages[r.pageIdx])
	if err != nil {
		return err
	}

	r.pageData = page.PageData()
	r.recordIdx = 0

	return nil
}

// seekOccupied advances to the next occupied slot, crossing into further data
// pages as needed.
func (r *Reader) seekOccupied() bool {
	for {
		for ; r.recordIdx < r.recordsPerPage; r.recordIdx++ {
			if _, ok := r.recordStart(r.recordIdx); ok {
				return true
			}
		}

		r.pageIdx++
		if r.pageIdx >= len(r.dataPages) {
			return false
		}

		err := r.loadPage()
		if err != nil {
			r.err = err
			return false
		}
	}
}

// recordStart returns the payload offset of the given slot, or false if the
// slot is out of range, unoccupied, or would run past the end of the payload.
func (r *Reader) recordStart(slot int) (int, bool) {
	if slot < 0 || slot >= r.recordsPerPage || r.recordSize <= 0 {
		return 0, false
	}

	if !bitSet(r.pageData, slot, r.bitmapBytes) {
		return 0, false
	}

	start := r.bitmapBytes + slot*r.recordSize
	if start+r.recordSize > len(r.pageData) {
		return 0, false
	}

	return start, true
}

// bitSet reports whether the given bit of a little-endian bitmap of bitmapBytes
// bytes at the start of data is set.
func bitSet(data []byte, bit, bitmapBytes int) bool {
	byteIdx := bit / 8
	if bit < 0 || byteIdx >= bitmapBytes || byteIdx >= len(data) {
		return false
	}

	return data[byteIdx]&(1<<uint(bit%8)) != 0
}

// delphiDateToTime converts a Delphi date integer to time.Time.
// Delphi dates: 1 = 0001-01-01. Go uses a different epoch.
func delphiDateToTime(days int32) time.Time {
	// Delphi epoch: day 1 = January 1, 0001.
	epoch := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)

	return epoch.AddDate(0, 0, int(days)-1)
}

// delphiTimeToTime converts Delphi time milliseconds to a time.Time with just the time component.
func delphiTimeToTime(ms int32) time.Time {
	return time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
}
