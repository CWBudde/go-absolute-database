package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf16"

	"golang.org/x/text/encoding/charmap"
)

var (
	ErrNoData     = errors.New("absdb: no data pages found")
	ErrNoMoreRows = errors.New("absdb: no more rows")
	ErrBadLayout  = errors.New("absdb: record layout does not match the data")
)

// currencyScale is the implied divisor of a Delphi Currency value, which is
// stored as an int64 with four decimal places.
const currencyScale = 10000

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

// OpenTable creates a Reader for the table's data records.
// A table without data pages yields a valid Reader whose Next reports no rows.
func (db *File) OpenTable() (*Reader, error) {
	schema, err := db.Schema()
	if err != nil {
		return nil, err
	}

	// Find all data pages (type 10).
	var dataPages []int

	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil {
			return nil, err
		}

		if page.Header != nil && page.Header.PageType == PageTypeData {
			dataPages = append(dataPages, i)
		}
	}

	r := &Reader{
		db:        db,
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
// Columns that do not store a plain integer return 0.
func (rec Record) Int(col int) int32 {
	v, _ := rec.intValue(col)

	return int32(v)
}

// Int16 returns the value of a SmallInt column. Columns that do not store a
// plain integer return 0.
func (rec Record) Int16(col int) int16 {
	v, _ := rec.intValue(col)

	return int16(v)
}

// Int64 returns the value of an integer column widened to int64. For a
// Currency column this is the raw value, scaled by 10000; use Float for the
// decimal value.
func (rec Record) Int64(col int) int64 {
	v, _ := rec.intValue(col)

	return v
}

// Uint16 returns the value of a Word column. Columns that do not store a plain
// integer return 0.
func (rec Record) Uint16(col int) uint16 {
	v, _ := rec.intValue(col)

	return uint16(v)
}

// Uint32 returns the value of an unsigned integer column. Narrower columns are
// zero-extended. Columns that do not store a plain integer return 0.
func (rec Record) Uint32(col int) uint32 {
	v, _ := rec.intValue(col)

	return uint32(v)
}

// Float returns the floating-point value of the column: Single is read as a
// 4-byte IEEE-754 value, Double as an 8-byte one and Currency as its scaled
// int64. Extended (80-bit) and all other columns return 0.
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
		v, _ := rec.intValue(col)

		return float64(v) / currencyScale
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
func (rec Record) Time(col int) time.Time {
	c, ok := rec.column(col)
	if !ok {
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
// columns. ok is false for columns that do not store a plain integer.
func (rec Record) intValue(col int) (int64, bool) {
	c, ok := rec.column(col)
	if !ok {
		return 0, false
	}

	width, signed, ok := integerStorage(c)
	if !ok {
		return 0, false
	}

	raw := rec.fieldPrefix(col, width)
	if raw == nil {
		return 0, false
	}

	var v uint64
	for i := width - 1; i >= 0; i-- {
		v = v<<8 | uint64(raw[i])
	}

	bits := uint(width) * 8
	if signed && width < 8 && v&(1<<(bits-1)) != 0 {
		v |= ^uint64(0) << bits
	}

	return int64(v), true
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
	case BftInt64, BftCurrency:
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

// fieldStoreSize returns the number of bytes a column occupies in the fixed
// part of a record.
func fieldStoreSize(c Column) int {
	switch c.BaseType {
	case BftInt8, BftUint8:
		return 1
	case BftInt16, BftUint16:
		return 2
	case BftInt32, BftUint32:
		return 4
	case BftInt64:
		return 8
	case BftSingle:
		return 4
	case BftDouble:
		return 8
	case BftExtended:
		return 10
	case BftCurrency:
		return 8
	case BftLogical:
		return 2 // WordBool
	case BftDate, BftTime:
		return 4
	case BftDateTime:
		return 8 // Date(4) + Time(4)
	case BftVarchar, BftChar:
		return int(c.Size) + 1
	case BftWideVarchar, BftWideChar:
		return (int(c.Size) + 1) * 2
	case BftBlob, BftClob, BftWideClob:
		return 6 // BLOB reference: PageNo(4) + ItemNo(2)
	case BftBytes:
		return int(c.Size)
	case BftVarBytes:
		return int(c.Size) + 2
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
