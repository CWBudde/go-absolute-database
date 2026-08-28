package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Encoding is the inverse of the decode path in reader.go: it turns Go values
// into the exact bytes a record slot holds. Every convention here was read off
// real .abs files rather than inferred:
//
//   - Null flags are the same bitmap Record.IsNull reads, and the spare high
//     bits of the last flag byte are set — Employees-*.abs (4 columns, one
//     flag byte, 0xf0), RCFQ0011.abs (7 columns, 0x80) and RCON0011.abs
//     (36 columns, five flag bytes ending 0xf0) all show it, and
//     Reader.validateLayout already rejects a file that lacks it.
//   - A NULL column's field bytes are zero in every fixture, so writing a NULL
//     zeroes the field as well as setting the bit.
//   - Char/Varchar fields hold Size+1 bytes: the Windows-1252 text, a NUL
//     terminator, then padding. Fresh records written by the Absolute Database
//     Manager zero-pad (Employees-*.abs), but records the engine has updated in
//     place keep whatever the record buffer held before (RCFQ0011.abs stores
//     "Nacht\x00\x46\x40…", the tail of a Double that shared the buffer).
//     Nothing can reconstruct that stale padding from a Go value, so the
//     encoder always zero-pads; the reader stops at the terminator either way.
//   - Booleans are a WordBool: 0x0001 for true, 0x0000 for false. No fixture
//     holds any other bit pattern.
//
// BLOB, CLOB and WideCLOB values are out of scope: storing one means allocating
// BLOB pages and maintaining their chain, which this package does not do yet.
// Such a column accepts nil — a NULL reference is a complete record — and
// rejects everything else rather than being half-written.
var (
	// ErrRecordSize is returned when the record buffer handed to encodeInto is
	// not exactly Reader.recordSize bytes long.
	ErrRecordSize = errors.New("absdb: record buffer has the wrong size")

	// ErrValueCount is returned when encodeRecord is given a number of values
	// that does not match the table's column count.
	ErrValueCount = errors.New("absdb: wrong number of values for the table's columns")

	// ErrColumnRange is returned for a column index outside the schema.
	ErrColumnRange = errors.New("absdb: column index out of range")

	// ErrValueType is returned when the Go type of a value cannot be stored in
	// the column at all — a string into an INTEGER column, say.
	ErrValueType = errors.New("absdb: value type does not fit the column")

	// ErrValueRange is returned when the value's type is right but the value
	// itself does not fit: outside the column's numeric range, or a string
	// longer than the column holds.
	ErrValueRange = errors.New("absdb: value does not fit the column")

	// ErrStringEncoding is returned for a string the column's character set
	// cannot represent, and for one containing a NUL, which would read back
	// truncated at the terminator.
	ErrStringEncoding = errors.New("absdb: string cannot be stored in the column's character set")

	// ErrBlobWrite is returned for a BLOB, CLOB or WideCLOB column. Writing one
	// needs BLOB page allocation, which is not implemented; this is a scope
	// boundary, not an oversight.
	ErrBlobWrite = errors.New("absdb: BLOB and CLOB columns cannot be written yet")

	// ErrColumnNotWritable is returned for a column whose storage this package
	// can read but not write: Extended (80-bit float, which Go has no type
	// for) and VarBytes (no fixture contains one, so its length prefix is
	// unverified and writing it would be guesswork).
	ErrColumnNotWritable = errors.New("absdb: column type cannot be written yet")
)

// daysToUnixEpoch is the number of days from 0001-01-01, which is Delphi date
// 1, to 1970-01-01. It converts a Go time to a Delphi date without going
// through time.Duration, whose int64 nanoseconds overflow outside 1678..2262.
const daysToUnixEpoch = 719162

// secondsPerDay is the divisor that turns a midnight Unix timestamp into a day
// number; millisPerDay bounds the time-of-day half of a DateTime.
const (
	secondsPerDay = 86400
	millisPerDay  = secondsPerDay * 1000
)

// encodeRecord fills a fresh recordSize-byte slot from values, one per column.
// The slot starts zeroed, so every field is zero-padded and every NULL column
// reads back as zero bytes — the shape a record written by the Absolute
// Database Manager has.
func (r *Reader) encodeRecord(values []any) ([]byte, error) {
	err := r.checkLayout()
	if err != nil {
		return nil, err
	}

	if len(values) != len(r.schema.Columns) {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrValueCount, len(values), len(r.schema.Columns))
	}

	rec := make([]byte, r.recordSize)
	r.setSpareNullFlags(rec)

	for col, v := range values {
		err = r.encodeInto(rec, col, v)
		if err != nil {
			return nil, err
		}
	}

	return rec, nil
}

// encodeInto writes v into column col of the record slot rec, which is exactly
// r.recordSize bytes. A nil v sets the column's null flag and zeroes the
// column's field bytes. Every byte of the column's field is written, so the
// result does not depend on what the buffer held before; no byte outside the
// field and the column's own null-flag bit is touched.
//
// A BLOB, CLOB or WideCLOB column takes nil and nothing else. Writing NULL is
// a complete operation — it zeroes the 6-byte reference and sets the flag, so a
// table with a Memo column is still insertable — but it does not free the BLOB
// pages an existing reference pointed at, which leaks them. Any other value is
// rejected with ErrBlobWrite: storing one means allocating BLOB pages and
// linking them into the file's BLOB chain, which is outside this phase's scope.
func (r *Reader) encodeInto(rec []byte, col int, v any) error {
	err := r.checkLayout()
	if err != nil {
		return err
	}

	if len(rec) != r.recordSize {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrRecordSize, len(rec), r.recordSize)
	}

	if col < 0 || col >= len(r.schema.Columns) {
		return fmt.Errorf("%w: %d not in [0,%d)", ErrColumnRange, col, len(r.schema.Columns))
	}

	c := r.schema.Columns[col]

	off := r.nullFlagBytes + r.fieldOffsets[col]

	size := r.fieldStoreSizes[col]
	if size <= 0 || off < 0 || off+size > len(rec) {
		return fmt.Errorf("%w: column %d has no field bytes in a %d-byte record",
			ErrBadLayout, col, r.recordSize)
	}

	field := rec[off : off+size]

	if v == nil {
		clear(field)
		setNullFlag(rec[:r.nullFlagBytes], col, true)

		return nil
	}

	if c.IsBLOB() {
		return fmt.Errorf("%w: column %d (%q, %s)", ErrBlobWrite, col, c.Name, c.FieldType)
	}

	err = encodeField(c, field, v)
	if err != nil {
		return fmt.Errorf("column %d (%q, %s): %w", col, c.Name, c.FieldType, err)
	}

	setNullFlag(rec[:r.nullFlagBytes], col, false)

	return nil
}

// checkLayout reports whether the Reader carries a usable record layout.
func (r *Reader) checkLayout() error {
	if r == nil || r.schema == nil {
		return ErrNoSchema
	}

	cols := len(r.schema.Columns)
	if r.recordSize <= 0 || len(r.fieldOffsets) != cols || len(r.fieldStoreSizes) != cols {
		return fmt.Errorf("%w: no record layout for %d columns", ErrBadLayout, cols)
	}

	return nil
}

// setSpareNullFlags sets the null-flag bits past the last column. The engine
// sets them in every record, and Reader.validateLayout rejects a file whose
// first record does not, so a record written without them would not read back.
func (r *Reader) setSpareNullFlags(rec []byte) {
	spare := r.nullFlagBytes*8 - len(r.schema.Columns)
	if spare <= 0 || r.nullFlagBytes > len(rec) {
		return
	}

	rec[r.nullFlagBytes-1] |= byte(0xff) << uint(8-spare)
}

// setNullFlag sets or clears the null bit of one column, using the same
// convention Record.IsNull reads: bit set means NULL.
func setNullFlag(flags []byte, col int, null bool) {
	byteIdx := col / 8
	if byteIdx < 0 || byteIdx >= len(flags) {
		return
	}

	mask := byte(1) << uint(col%8)

	if null {
		flags[byteIdx] |= mask
	} else {
		flags[byteIdx] &^= mask
	}
}

// --- per-type encoders ---

// encodeField writes a non-nil value into the column's field bytes. field is
// exactly fieldStoreSize(c) bytes and is fully overwritten.
func encodeField(c Column, field []byte, v any) error {
	switch c.BaseType {
	case BftCurrency:
		return encodeCurrency(field, v)
	case BftSingle, BftDouble:
		return encodeFloat(field, v)
	case BftLogical:
		return encodeBool(field, v)
	case BftChar, BftVarchar:
		return encodeANSI(field, v)
	case BftWideChar, BftWideVarchar:
		return encodeUTF16(field, v)
	case BftDate, BftTime, BftDateTime:
		return encodeTime(c.BaseType, field, v)
	case BftBytes:
		return encodeBytes(field, v)
	}

	width, signed, ok := integerStorage(c)
	if !ok {
		return fmt.Errorf("%w: %s", ErrColumnNotWritable, c.FieldType)
	}

	return encodeInteger(field, v, width, signed)
}

// encodeInteger writes an integer value in width little-endian bytes, rejecting
// anything outside the column's range rather than truncating it.
func encodeInteger(field []byte, v any, width int, signed bool) error {
	n, isInt, inRange := integerValue(v)
	if !isInt {
		return fmt.Errorf("%w: %T is not an integer", ErrValueType, v)
	}

	if !inRange {
		return fmt.Errorf("%w: %v exceeds the int64 range", ErrValueRange, v)
	}

	low, high := integerBounds(width, signed)
	if n < low || n > high {
		return fmt.Errorf("%w: %d outside [%d,%d]", ErrValueRange, n, low, high)
	}

	if width > len(field) {
		return fmt.Errorf("%w: %d-byte value in a %d-byte field", ErrValueRange, width, len(field))
	}

	clear(field)
	putUintLE(field[:width], uint64(n))

	return nil
}

// encodeCurrency writes a Delphi Currency, an int64 scaled by 10000. An
// integer value is the raw scaled value, matching Record.Int64; a float value
// is the decimal amount, matching Record.Float, and must be an exact multiple
// of 1/10000.
func encodeCurrency(field []byte, v any) error {
	if _, isInt, _ := integerValue(v); isInt {
		return encodeInteger(field, v, 8, true)
	}

	f, ok := floatValue(v)
	if !ok {
		return fmt.Errorf("%w: %T is neither an integer nor a float", ErrValueType, v)
	}

	scaled := f * currencyScale

	rounded := math.Round(scaled)
	if math.IsNaN(scaled) || math.Abs(scaled) >= math.MaxInt64 {
		return fmt.Errorf("%w: %v outside the Currency range", ErrValueRange, f)
	}

	// Rounding only absorbs the error of the multiplication itself; a value
	// with a fifth decimal is refused rather than quietly rounded away.
	if math.Abs(scaled-rounded) > 1e-6 {
		return fmt.Errorf("%w: %v is not a multiple of 0.0001", ErrValueRange, f)
	}

	return encodeInteger(field, int64(rounded), 8, true)
}

// encodeFloat writes a Single (4 bytes) or Double (8 bytes) IEEE-754 value. A
// value that a Single cannot hold exactly is refused rather than rounded.
func encodeFloat(field []byte, v any) error {
	f, ok := floatValue(v)
	if !ok {
		return fmt.Errorf("%w: %T is not a number", ErrValueType, v)
	}

	switch len(field) {
	case 4:
		if !math.IsNaN(f) && float64(float32(f)) != f {
			return fmt.Errorf("%w: %v is not exactly representable as a Single", ErrValueRange, f)
		}

		binary.LittleEndian.PutUint32(field, math.Float32bits(float32(f)))
	case 8:
		binary.LittleEndian.PutUint64(field, math.Float64bits(f))
	default:
		return fmt.Errorf("%w: %d-byte floating-point field", ErrBadLayout, len(field))
	}

	return nil
}

// encodeBool writes a WordBool: 0x0001 for true, 0x0000 for false.
func encodeBool(field []byte, v any) error {
	b, ok := v.(bool)
	if !ok {
		return fmt.Errorf("%w: %T is not a bool", ErrValueType, v)
	}

	if len(field) < 2 {
		return fmt.Errorf("%w: %d-byte WordBool field", ErrBadLayout, len(field))
	}

	clear(field)

	if b {
		field[0] = 1
	}

	return nil
}

// encodeANSI writes a Windows-1252 string with a NUL terminator, zero-padded to
// the width of the field. The field holds Size+1 bytes, so the text itself may
// be at most len(field)-1 bytes once encoded.
func encodeANSI(field []byte, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%w: %T is not a string", ErrValueType, v)
	}

	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(s))
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrStringEncoding, s, err)
	}

	if bytes.IndexByte(raw, 0) >= 0 {
		return fmt.Errorf("%w: %q contains a NUL", ErrStringEncoding, s)
	}

	if len(raw) >= len(field) {
		return fmt.Errorf("%w: %d bytes of text in a %d-character column",
			ErrValueRange, len(raw), len(field)-1)
	}

	clear(field)
	copy(field, raw)

	return nil
}

// encodeUTF16 writes a UTF-16LE string terminated by a 0x0000 code unit,
// zero-padded to the width of the field. The field holds (Size+1)*2 bytes, so
// the text may be at most len(field)/2-1 code units — a character outside the
// BMP costs two of them, exactly as it does on the read side.
func encodeUTF16(field []byte, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%w: %T is not a string", ErrValueType, v)
	}

	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %q is not valid UTF-8", ErrStringEncoding, s)
	}

	units := utf16.Encode([]rune(s))
	if slices.Contains(units, 0) {
		return fmt.Errorf("%w: %q contains a NUL", ErrStringEncoding, s)
	}

	if (len(units)+1)*2 > len(field) {
		return fmt.Errorf("%w: %d code units in a %d-character column",
			ErrValueRange, len(units), len(field)/2-1)
	}

	clear(field)

	for i, u := range units {
		binary.LittleEndian.PutUint16(field[i*2:i*2+2], u)
	}

	return nil
}

// encodeTime writes a Date (int32 days), a Time (int32 milliseconds since
// midnight) or a DateTime (both, date first). The value is read as a naive
// wall-clock time, the way a Delphi TDateTime carries no zone; Record.Time
// hands back UTC, so a value read and written back is unchanged.
//
// A Date column refuses a value with a non-zero time of day and a Time column
// refuses one whose date is not 0001-01-01, the date Record.Time reports for
// them: dropping the other half silently would lose information.
func encodeTime(base BaseFieldType, field []byte, v any) error {
	t, ok := v.(time.Time)
	if !ok {
		return fmt.Errorf("%w: %T is not a time.Time", ErrValueType, v)
	}

	days, err := delphiDate(t)
	if err != nil {
		return err
	}

	ms, err := delphiTimeOfDay(t)
	if err != nil {
		return err
	}

	switch base {
	case BftDate:
		if ms != 0 {
			return fmt.Errorf("%w: %v has a time of day, which a Date column cannot store", ErrValueRange, t)
		}

		return encodeInteger(field, int64(days), 4, true)
	case BftTime:
		if days != 1 {
			return fmt.Errorf("%w: %v is not on 0001-01-01, which a Time column cannot store", ErrValueRange, t)
		}

		return encodeInteger(field, int64(ms), 4, true)
	default:
		if len(field) < 8 {
			return fmt.Errorf("%w: %d-byte DateTime field", ErrBadLayout, len(field))
		}

		err = encodeInteger(field[0:4], int64(days), 4, true)
		if err != nil {
			return err
		}

		return encodeInteger(field[4:8], int64(ms), 4, true)
	}
}

// encodeBytes writes a fixed-width Bytes column: the value is copied in and the
// rest of the field is zeroed, so Record.Bytes reads the same field back.
func encodeBytes(field []byte, v any) error {
	b, ok := v.([]byte)
	if !ok {
		return fmt.Errorf("%w: %T is not a []byte", ErrValueType, v)
	}

	if len(b) > len(field) {
		return fmt.Errorf("%w: %d bytes in a %d-byte column", ErrValueRange, len(b), len(field))
	}

	clear(field)
	copy(field, b)

	return nil
}

// --- value conversion ---

// integerValue reports v as an int64. isInt is false for a value that is not a
// Go integer at all; inRange is false for the one integer that has no int64
// form, a uint or uint64 above MaxInt64.
func integerValue(v any) (n int64, isInt, inRange bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true, true
	case int8:
		return int64(x), true, true
	case int16:
		return int64(x), true, true
	case int32:
		return int64(x), true, true
	case int64:
		return x, true, true
	case uint8:
		return int64(x), true, true
	case uint16:
		return int64(x), true, true
	case uint32:
		return int64(x), true, true
	case uint:
		return unsignedValue(uint64(x))
	case uint64:
		return unsignedValue(x)
	case uintptr:
		return unsignedValue(uint64(x))
	default:
		return 0, false, false
	}
}

// unsignedValue narrows a uint64 to an int64, reporting the values that do not
// fit rather than wrapping them into a negative number.
func unsignedValue(x uint64) (n int64, isInt, inRange bool) {
	if x > math.MaxInt64 {
		return 0, true, false
	}

	return int64(x), true, true
}

// floatValue reports v as a float64. Integers are accepted as the obvious
// widening; one too large to survive the conversion is not.
func floatValue(v any) (float64, bool) {
	switch x := v.(type) {
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}

	n, isInt, inRange := integerValue(v)
	if !isInt || !inRange {
		return 0, false
	}

	f := float64(n)
	if int64(f) != n {
		return 0, false
	}

	return f, true
}

// integerBounds returns the inclusive range a width-byte integer column holds.
func integerBounds(width int, signed bool) (low, high int64) {
	bits := uint(width) * 8

	if !signed {
		if bits >= 64 {
			return 0, math.MaxInt64
		}

		return 0, int64(1)<<bits - 1
	}

	if bits == 0 || bits >= 64 {
		return math.MinInt64, math.MaxInt64
	}

	return -1 << (bits - 1), 1<<(bits-1) - 1
}

// putUintLE writes v into dst in little-endian order, one byte at a time, so
// that any width from 1 to 8 bytes works.
func putUintLE(dst []byte, v uint64) {
	for i := range dst {
		dst[i] = byte(v)
		v >>= 8
	}
}

// delphiDate converts the date part of t to a Delphi date integer, the inverse
// of delphiDateToTime. It counts whole days rather than subtracting times, so
// it stays exact outside the range a time.Duration can express.
func delphiDate(t time.Time) (int32, error) {
	year, month, day := t.Date()
	midnight := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	days := midnight.Unix()/secondsPerDay + daysToUnixEpoch + 1

	if days < math.MinInt32 || days > math.MaxInt32 {
		return 0, fmt.Errorf("%w: %v is outside the Delphi date range", ErrValueRange, t)
	}

	return int32(days), nil
}

// delphiTimeOfDay converts the time-of-day part of t to milliseconds since
// midnight, the inverse of delphiTimeToTime. A sub-millisecond remainder is
// refused rather than dropped.
func delphiTimeOfDay(t time.Time) (int32, error) {
	ns := int64(t.Hour())*int64(time.Hour) +
		int64(t.Minute())*int64(time.Minute) +
		int64(t.Second())*int64(time.Second) +
		int64(t.Nanosecond())

	if ns%int64(time.Millisecond) != 0 {
		return 0, fmt.Errorf("%w: %v has sub-millisecond precision", ErrValueRange, t)
	}

	ms := ns / int64(time.Millisecond)

	// A time of day is always inside a single day, but the bound is stated
	// rather than assumed: it is what makes the narrowing below lossless.
	if ms < 0 || ms >= millisPerDay {
		return 0, fmt.Errorf("%w: %v is not a time of day", ErrValueRange, t)
	}

	return int32(ms), nil
}
