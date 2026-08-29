package absdb

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// --- helpers ---

// newTestReader builds a Reader that carries only a record layout, which is all
// the encoder needs. It never touches a file.
func newTestReader(cols ...Column) *Reader {
	r := &Reader{schema: &TableSchema{Columns: cols}}
	r.computeRecordLayout()

	return r
}

// col is shorthand for a column definition in the synthetic tables below.
func col(name string, base BaseFieldType, ft FieldType, size uint32) Column {
	return Column{Name: name, BaseType: base, FieldType: ft, Size: size}
}

// recordOver wraps a freshly encoded slot in a Record so the existing readers
// can be pointed at it.
func recordOver(r *Reader, rec []byte) Record {
	return Record{
		reader:    r,
		nullFlags: rec[:r.nullFlagBytes],
		fieldData: rec[r.nullFlagBytes:],
	}
}

// slotBytes reassembles the record slot a Record was read from. nullFlags and
// fieldData are adjacent slices of the same page, so the slot is their
// concatenation.
func slotBytes(rec Record) []byte {
	out := make([]byte, 0, len(rec.nullFlags)+len(rec.fieldData))
	out = append(out, rec.nullFlags...)
	out = append(out, rec.fieldData...)

	return out
}

// --- the decisive round-trip test ---

// TestEncodeReproducesFixtureRecords reads every record of every fixture
// present, decodes each column with the existing Record accessors, re-encodes
// the values, and requires the result to be byte-identical to the slot the file
// holds.
//
// Two deliberate deviations from a literal byte comparison, both documented in
// encode.go and both properties of the files rather than of the encoder:
//
//   - BLOB, CLOB and WideCLOB columns are excluded. Their 6-byte reference
//     points at a BLOB page chain this package cannot allocate, so a non-NULL
//     one is copied through instead of encoded. A NULL one needs no exclusion:
//     writing nil is a complete operation. In practice only RPDG0011.abs has
//     non-NULL BLOB references; TS03.abs and Addresses.abs have Memo columns
//     that are NULL in every row and go through the encoder unaided.
//   - The padding a Char/Varchar field holds *after* its NUL terminator is
//     canonicalised to zero on both sides. Records the engine wrote fresh are
//     already zero-padded (all eight Employees-*.abs), but records it updated
//     in place keep whatever the record buffer held before — RCFQ0011.abs
//     stores "Nacht\x00\x46\x40..." where 46 40 is the tail of a Double that
//     shared the buffer. That stale padding is not a function of the value, so
//     nothing can reconstruct it. The test reports how many records needed no
//     canonicalisation at all.
func TestEncodeReproducesFixtureRecords(t *testing.T) {
	var totalRecords, totalFixtures, verbatimRecords, excludedColumns int

	for _, name := range roundTripFixtures(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)

			totalFixtures++

			for _, tbl := range fixtureTables(t, db) {
				reader, err := tbl.Open()
				if err != nil {
					t.Fatalf("%s: Open: %v", tbl.Name(), err)
				}

				records, verbatim, excluded := roundTripTable(t, reader)

				totalRecords += records
				verbatimRecords += verbatim
				excludedColumns += excluded

				t.Logf("%s/%s: %d records, %d byte-identical without canonicalisation, %d BLOB columns copied through",
					name, tbl.Name(), records, verbatim, excluded)
			}
		})
	}

	if totalRecords == 0 {
		t.Skip("no fixture with records present (testdata/ is not committed)")
	}

	t.Logf("compared %d records across %d fixtures; %d byte-identical with no canonicalisation, "+
		"%d BLOB/CLOB column values copied through",
		totalRecords, totalFixtures, verbatimRecords, excludedColumns)
}

// roundTripFixtures lists the fixtures the round-trip test walks: the eight
// committed Employees-* files by name, so their absence shows up as a skip,
// plus every other .abs file in testdata/, which on this machine is real
// private data that a fresh clone simply does not have.
func roundTripFixtures(t *testing.T) []string {
	t.Helper()

	names := make([]string, 0, len(employeeFixtures))
	seen := map[string]bool{}

	for _, f := range employeeFixtures {
		names = append(names, f.name)
		seen[f.name] = true
	}

	paths, err := filepath.Glob(filepath.Join("testdata", "*.abs"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}

	for _, p := range paths {
		base := filepath.Base(p)
		if !seen[base] {
			names = append(names, base)
		}
	}

	return names
}

// roundTripTable re-encodes every record of one table and compares it with the
// bytes on disk. It returns the number of records compared, how many of those
// matched without any canonicalisation, and how many column values had to be
// copied through instead of encoded.
func roundTripTable(t *testing.T, r *Reader) (records, verbatim, excluded int) {
	t.Helper()

	for r.Next() {
		rec := r.Record()
		want := slotBytes(rec)
		records++

		got, skipped, err := reencode(r, rec)
		if err != nil {
			t.Fatalf("record %d: %v", records, err)
		}

		excluded += skipped

		if bytes.Equal(got, want) {
			verbatim++
			continue
		}

		canonical := canonicalSlot(r, want)
		if !bytes.Equal(got, canonical) {
			t.Fatalf("record %d does not round-trip\n got %s\nwant %s",
				records, diffSummary(r, got, canonical), diffSummary(r, canonical, got))
		}
	}

	if err := r.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}

	return records, verbatim, excluded
}

// reencode rebuilds a record slot from the Go values the Record accessors
// report. It uses encodeRecord — the API the writer calls — whenever the record
// has no non-NULL BLOB reference, and falls back to a per-column build that
// copies those references through when it has.
func reencode(r *Reader, rec Record) (slot []byte, excluded int, err error) {
	values := make([]any, len(r.schema.Columns))
	passThrough := make([]bool, len(r.schema.Columns))

	for i, c := range r.schema.Columns {
		if rec.IsNull(i) {
			continue // values[i] stays nil, which encodes as NULL
		}

		v, ok := decodedValue(rec, c, i)
		if !ok {
			passThrough[i] = true
			excluded++

			continue
		}

		values[i] = v
	}

	if excluded == 0 {
		slot, err = r.encodeRecord(values)

		return slot, 0, err
	}

	slot = make([]byte, r.recordSize)
	r.setSpareNullFlags(slot)

	for i := range r.schema.Columns {
		if passThrough[i] {
			off := r.nullFlagBytes + r.fieldOffsets[i]
			copy(slot[off:off+r.fieldStoreSizes[i]], rec.field(i))
			setNullFlag(slot[:r.nullFlagBytes], i, false)

			continue
		}

		err = r.encodeInto(slot, i, values[i])
		if err != nil {
			return nil, excluded, err
		}
	}

	return slot, excluded, nil
}

// decodedValue reads one column with the accessor that matches its storage
// type. ok is false for a column the encoder does not support, which the caller
// copies through instead.
func decodedValue(rec Record, c Column, i int) (any, bool) {
	// A TimeStamp shares BftDateTime's base type but stores an undecoded
	// layout, so nothing here can rebuild it; it is copied through like a
	// BLOB reference.
	if c.FieldType == FieldTimeStamp {
		return nil, false
	}

	switch c.BaseType {
	case BftCurrency:
		return rec.Float(i), true
	case BftChar, BftVarchar, BftWideChar, BftWideVarchar:
		return rec.String(i), true
	case BftLogical:
		return rec.Bool(i), true
	case BftSingle, BftDouble:
		return rec.Float(i), true
	case BftDate, BftTime, BftDateTime:
		return rec.Time(i), true
	case BftBytes:
		return rec.Bytes(i), true
	case BftBlob, BftClob, BftWideClob, BftExtended, BftVarBytes:
		return nil, false
	}

	if _, _, ok := integerStorage(c); ok {
		return rec.Int64(i), true
	}

	return nil, false
}

// canonicalSlot zeroes the padding that follows a string field's terminator.
// The engine leaves whatever the record buffer held there; the encoder always
// zero-pads, and no value can tell the two apart.
func canonicalSlot(r *Reader, slot []byte) []byte {
	out := make([]byte, len(slot))
	copy(out, slot)

	for i, c := range r.schema.Columns {
		off := r.nullFlagBytes + r.fieldOffsets[i]

		size := r.fieldStoreSizes[i]
		if size <= 0 || off+size > len(out) {
			continue
		}

		field := out[off : off+size]

		switch c.BaseType {
		case BftChar, BftVarchar:
			clear(field[min(terminatedLen(field, 1)+1, len(field)):])
		case BftWideChar, BftWideVarchar:
			clear(field[min(terminatedLen(field, 2)+2, len(field)):])
		}
	}

	return out
}

// terminatedLen returns the byte offset of the first all-zero code unit of the
// given width, or len(field) when there is none.
func terminatedLen(field []byte, unit int) int {
	for i := 0; i+unit <= len(field); i += unit {
		if bytes.Equal(field[i:i+unit], make([]byte, unit)) {
			return i
		}
	}

	return len(field)
}

// diffSummary names the columns whose bytes differ, so a failure points at a
// column rather than at a wall of hex.
func diffSummary(r *Reader, got, want []byte) string {
	if !bytes.Equal(got[:r.nullFlagBytes], want[:r.nullFlagBytes]) {
		return fmt.Sprintf("null flags % x", got[:r.nullFlagBytes])
	}

	for i, c := range r.schema.Columns {
		off := r.nullFlagBytes + r.fieldOffsets[i]

		size := r.fieldStoreSizes[i]
		if size <= 0 || off+size > len(got) {
			continue
		}

		if !bytes.Equal(got[off:off+size], want[off:off+size]) {
			return fmt.Sprintf("column %d (%q, %s) = % x", i, c.Name, c.FieldType, got[off:off+size])
		}
	}

	return "identical"
}

// --- rejection rules ---

func TestEncodeRejectsOutOfRange(t *testing.T) {
	tests := []struct {
		name   string
		column Column
		value  any
		want   error
	}{
		{"int16 above range", col("A", BftInt16, FieldSmallInt, 0), 40000, ErrValueRange},
		{"int16 below range", col("A", BftInt16, FieldSmallInt, 0), -40000, ErrValueRange},
		{"uint8 negative", col("A", BftUint8, FieldByte, 0), -1, ErrValueRange},
		{"uint32 above range", col("A", BftUint32, FieldCardinal, 0), int64(1) << 33, ErrValueRange},
		{"uint64 above int64", col("A", BftInt64, FieldLargeInt, 0), uint64(math.MaxUint64), ErrValueRange},
		{"string into integer", col("A", BftInt32, FieldInteger, 0), "seven", ErrValueType},
		{"float into integer", col("A", BftInt32, FieldInteger, 0), 1.5, ErrValueType},
		{"integer into bool", col("A", BftLogical, FieldBoolean, 0), 1, ErrValueType},
		{"bytes into varchar", col("A", BftVarchar, FieldString, 5), []byte("ab"), ErrValueType},
		{"varchar too long", col("A", BftVarchar, FieldString, 5), "toolong", ErrValueRange},
		{"varchar exactly one over", col("A", BftVarchar, FieldString, 5), "abcdef", ErrValueRange},
		{"varchar outside windows-1252", col("A", BftVarchar, FieldString, 5), "\u2192", ErrStringEncoding},
		{"varchar with embedded NUL", col("A", BftVarchar, FieldString, 5), "a\x00b", ErrStringEncoding},
		{"widevarchar too long", col("A", BftWideVarchar, FieldWideString, 2), "abc", ErrValueRange},
		{"widevarchar astral overflow", col("A", BftWideVarchar, FieldWideString, 2), "\U0001F600x", ErrValueRange},
		{"widevarchar with embedded NUL", col("A", BftWideVarchar, FieldWideString, 5), "a\x00b", ErrStringEncoding},
		{"single not representable", col("A", BftSingle, FieldSingle, 0), 1e300, ErrValueRange},
		{"single from imprecise double", col("A", BftSingle, FieldSingle, 0), 0.1, ErrValueRange},
		{"string into double", col("A", BftDouble, FieldDouble, 0), "1.5", ErrValueType},
		{"currency from a string", col("A", BftCurrency, FieldCurrency, 0), "1.00", ErrValueType},
		{"bytes too long", col("A", BftBytes, FieldBytes, 4), []byte("abcde"), ErrValueRange},
		{"string into bytes", col("A", BftBytes, FieldBytes, 4), "abcd", ErrValueType},
		{"date with a time of day", col("A", BftDate, FieldDate, 0), time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC), ErrValueRange},
		{"time with a date", col("A", BftTime, FieldTime, 0), time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC), ErrValueRange},
		{"datetime with sub-millisecond precision", col("A", BftDateTime, FieldDateTime, 0), time.Date(2024, 3, 1, 12, 0, 0, 1, time.UTC), ErrValueRange},
		{"string into datetime", col("A", BftDateTime, FieldDateTime, 0), "2024-03-01", ErrValueType},
		{"blob column", col("A", BftBlob, FieldGraphic, 0), []byte("x"), ErrBlobWrite},
		{"clob column", col("A", BftClob, FieldMemo, 0), "text", ErrBlobWrite},
		{"wideclob column", col("A", BftWideClob, FieldWideMemo, 0), "text", ErrBlobWrite},
		{"extended column", col("A", BftExtended, FieldExtended, 0), 1.5, ErrColumnNotWritable},
		{"varbytes column", col("A", BftVarBytes, FieldVarBytes, 4), []byte("ab"), ErrColumnNotWritable},
		{"unknown column", col("A", BftUnknown, FieldUnknown, 4), 1, ErrColumnNotWritable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReader(tt.column)

			err := r.encodeInto(make([]byte, r.recordSize), 0, tt.value)
			if !errors.Is(err, tt.want) {
				t.Fatalf("encodeInto(%#v) error = %v, want %v", tt.value, err, tt.want)
			}
		})
	}
}

// TestEncodeRejectsBadCall covers the failure modes that are about the call
// rather than about the value.
func TestEncodeRejectsBadCall(t *testing.T) {
	r := newTestReader(
		col("A", BftInt32, FieldInteger, 0),
		col("B", BftVarchar, FieldString, 4),
	)

	rec := make([]byte, r.recordSize)

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"short buffer", func() error { return r.encodeInto(rec[:len(rec)-1], 0, 1) }, ErrRecordSize},
		{"long buffer", func() error { return r.encodeInto(make([]byte, len(rec)+1), 0, 1) }, ErrRecordSize},
		{"negative column", func() error { return r.encodeInto(rec, -1, 1) }, ErrColumnRange},
		{"column past the end", func() error { return r.encodeInto(rec, 2, 1) }, ErrColumnRange},
		{"too few values", func() error { _, err := r.encodeRecord([]any{1}); return err }, ErrValueCount},
		{"too many values", func() error { _, err := r.encodeRecord([]any{1, "a", 2}); return err }, ErrValueCount},
		{"no layout", func() error { return (&Reader{}).encodeInto(rec, 0, 1) }, ErrNoSchema},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// --- null flags ---

// TestEncodeNullFlags pins the null-flag convention the files use: bit set
// means NULL, and every bit past the last column is set. Reader.validateLayout
// rejects a file whose first record lacks those spare bits, so a record written
// without them would not read back.
func TestEncodeNullFlags(t *testing.T) {
	// Twelve columns need two flag bytes, leaving four spare bits.
	cols := make([]Column, 12)
	for i := range cols {
		cols[i] = col(fmt.Sprintf("C%d", i), BftInt32, FieldInteger, 0)
	}

	r := newTestReader(cols...)

	if r.nullFlagBytes != 2 {
		t.Fatalf("nullFlagBytes = %d, want 2", r.nullFlagBytes)
	}

	spareMask := byte(0xff) << uint(8-(r.nullFlagBytes*8-len(cols)))

	t.Run("all NULL", func(t *testing.T) {
		rec, err := r.encodeRecord(make([]any, len(cols)))
		if err != nil {
			t.Fatal(err)
		}

		if got := rec[:2]; !bytes.Equal(got, []byte{0xff, 0xff}) {
			t.Errorf("null flags = % x, want both bytes 0xff", got)
		}

		checkNulls(t, r, rec, func(int) bool { return true })
	})

	t.Run("none NULL", func(t *testing.T) {
		values := make([]any, len(cols))
		for i := range values {
			values[i] = i
		}

		rec, err := r.encodeRecord(values)
		if err != nil {
			t.Fatal(err)
		}

		want := []byte{0x00, spareMask}
		if got := rec[:2]; !bytes.Equal(got, want) {
			t.Errorf("null flags = % x, want % x", got, want)
		}

		checkNulls(t, r, rec, func(int) bool { return false })
	})

	t.Run("set and clear one column", func(t *testing.T) {
		values := make([]any, len(cols))
		for i := range values {
			values[i] = i
		}

		rec, err := r.encodeRecord(values)
		if err != nil {
			t.Fatal(err)
		}

		// Setting column 9 NULL must zero its field and leave the spare bits.
		err = r.encodeInto(rec, 9, nil)
		if err != nil {
			t.Fatal(err)
		}

		want := []byte{0x00, spareMask | 0x02}
		if got := rec[:2]; !bytes.Equal(got, want) {
			t.Errorf("after NULL: null flags = % x, want % x", got, want)
		}

		field := rec[r.nullFlagBytes+r.fieldOffsets[9]:][:4]
		if !bytes.Equal(field, []byte{0, 0, 0, 0}) {
			t.Errorf("NULL column field = % x, want all zero", field)
		}

		checkNulls(t, r, rec, func(i int) bool { return i == 9 })

		// Writing a value again must clear the bit and leave the spare bits.
		err = r.encodeInto(rec, 9, 42)
		if err != nil {
			t.Fatal(err)
		}

		want = []byte{0x00, spareMask}
		if got := rec[:2]; !bytes.Equal(got, want) {
			t.Errorf("after value: null flags = % x, want % x", got, want)
		}

		checkNulls(t, r, rec, func(int) bool { return false })
	})
}

// checkNulls asserts that Record.IsNull agrees with the encoder for every
// column, so the two directions cannot drift apart.
func checkNulls(t *testing.T, r *Reader, rec []byte, wantNull func(int) bool) {
	t.Helper()

	record := recordOver(r, rec)

	for i := range r.schema.Columns {
		if got := record.IsNull(i); got != wantNull(i) {
			t.Errorf("IsNull(%d) = %v, want %v", i, got, wantNull(i))
		}
	}
}

// --- synthetic round-trip for the types no fixture holds ---

// TestEncodeRoundTripsEveryWritableType encodes one value of every base type
// the encoder supports and reads it back with the Record accessors. No fixture
// on hand carries a Date, Time, DateTime, Single, Currency, Bytes or WideChar
// column with rows in it, so this is the only coverage those conversions get.
func TestEncodeRoundTripsEveryWritableType(t *testing.T) {
	cols := []Column{
		col("i8", BftInt8, FieldShortInt, 0),
		col("u8", BftUint8, FieldByte, 0),
		col("i16", BftInt16, FieldSmallInt, 0),
		col("u16", BftUint16, FieldWord, 0),
		col("i32", BftInt32, FieldInteger, 0),
		col("u32", BftUint32, FieldCardinal, 0),
		col("i64", BftInt64, FieldLargeInt, 0),
		col("cur", BftCurrency, FieldCurrency, 0),
		col("sng", BftSingle, FieldSingle, 0),
		col("dbl", BftDouble, FieldDouble, 0),
		col("bool", BftLogical, FieldBoolean, 0),
		col("chr", BftChar, FieldChar, 8),
		col("var", BftVarchar, FieldString, 8),
		col("wchr", BftWideChar, FieldWideChar, 8),
		col("wvar", BftWideVarchar, FieldWideString, 8),
		col("date", BftDate, FieldDate, 0),
		col("time", BftTime, FieldTime, 0),
		col("dt", BftDateTime, FieldDateTime, 0),
		col("bytes", BftBytes, FieldBytes, 4),
	}

	r := newTestReader(cols...)

	date := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	clock := time.Date(1, 1, 1, 13, 45, 30, int(500*time.Millisecond), time.UTC)
	stamp := time.Date(1899, 12, 30, 6, 0, 0, 0, time.UTC)

	values := []any{
		int8(-128), uint8(255), int16(-32768), uint16(65535),
		int32(-2147483648), uint32(4294967295), int64(-1),
		1234.5678, float32(0.5), 1234.5,
		true, "chars", "varchar", "wide", "widevar",
		date, clock, stamp,
		[]byte{1, 2, 3, 4},
	}

	rec, err := r.encodeRecord(values)
	if err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}

	got := recordOver(r, rec)

	// Column 7 is the Currency one, which stores a double and is checked
	// through Float below rather than as an integer.
	checkInts(t, got, []int64{-128, 255, -32768, 65535, -2147483648, 4294967295, -1})

	if v := got.Float(8); v != 0.5 {
		t.Errorf("Single = %v, want 0.5", v)
	}

	if v := got.Float(9); v != 1234.5 {
		t.Errorf("Double = %v, want 1234.5", v)
	}

	if !got.Bool(10) {
		t.Error("Boolean = false, want true")
	}

	for i, want := range map[int]string{11: "chars", 12: "varchar", 13: "wide", 14: "widevar"} {
		if v := got.String(i); v != want {
			t.Errorf("String(%d) = %q, want %q", i, v, want)
		}
	}

	for i, want := range map[int]time.Time{15: date, 16: clock, 17: stamp} {
		if v := got.Time(i); !v.Equal(want) {
			t.Errorf("Time(%d) = %v, want %v", i, v, want)
		}
	}

	// A Bytes column stores one byte more than its declared size, so the
	// value comes back with the trailing byte the field always carries.
	if v := got.Bytes(18); !bytes.Equal(v, []byte{1, 2, 3, 4, 0}) {
		t.Errorf("Bytes = % x, want 01 02 03 04 00", v)
	}

	// Currency stores a double, so it reads back through Float alone.
	if v := got.Float(7); v != 1234.5678 {
		t.Errorf("Currency = %v, want 1234.5678", v)
	}
}

// checkInts asserts the first len(want) columns read back as the given values.
func checkInts(t *testing.T, rec Record, want []int64) {
	t.Helper()

	for i, w := range want {
		if got := rec.Int64(i); got != w {
			t.Errorf("Int64(%d) = %d, want %d", i, got, w)
		}
	}
}

// TestEncodeCurrencyFromFloat pins the two ways a Currency column accepts a
// value: an integer is the raw scaled value Record.Int64 reports, a float is
// the decimal value Record.Float reports.
func TestEncodeCurrencyFromFloat(t *testing.T) {
	r := newTestReader(col("cur", BftCurrency, FieldCurrency, 0))

	for _, f := range []float64{0, 1.1, -1.1, 1234.5678, -0.0001, 99999999.9999} {
		rec, err := r.encodeRecord([]any{f})
		if err != nil {
			t.Fatalf("encodeRecord(%v): %v", f, err)
		}

		if got := recordOver(r, rec).Float(0); got != f {
			t.Errorf("Currency %v read back as %v", f, got)
		}
	}
}

// TestEncodeDelphiDateInverse checks delphiDate against delphiDateToTime over a
// wide span, including dates outside the range a time.Duration can express.
func TestEncodeDelphiDateInverse(t *testing.T) {
	for _, days := range []int32{1, 2, 100000, 693594, 719163, 730120, 3000000} {
		got, err := delphiDate(delphiDateToTime(days))
		if err != nil {
			t.Fatalf("delphiDate(day %d): %v", days, err)
		}

		if got != days {
			t.Errorf("delphiDate(delphiDateToTime(%d)) = %d", days, got)
		}
	}

	for _, ms := range []int32{0, 1, 1000, 43200000, 86399999} {
		got, err := delphiTimeOfDay(delphiTimeToTime(ms))
		if err != nil {
			t.Fatalf("delphiTimeOfDay(%d ms): %v", ms, err)
		}

		if got != ms {
			t.Errorf("delphiTimeOfDay(delphiTimeToTime(%d)) = %d", ms, got)
		}
	}
}

// TestEncodeRefusesTimeStamp pins the one refusal Types.abs forced. A
// TimeStamp column shares BftDateTime's base type but stores some other
// layout, so writing it as a DateTime would record a different instant.
func TestEncodeRefusesTimeStamp(t *testing.T) {
	stamp := col("ts", BftDateTime, FieldTimeStamp, 0)
	r := newTestReader(stamp, col("n", BftInt32, FieldInteger, 0))

	err := r.encodeInto(make([]byte, r.recordSize), 0, time.Unix(0, 0).UTC())
	if !errors.Is(err, ErrColumnNotWritable) {
		t.Errorf("encodeInto on a TimeStamp column: err = %v, want ErrColumnNotWritable", err)
	}

	rec := recordOver(r, make([]byte, r.recordSize))
	if _, err := decodeColumnValue(stamp, rec, 0); !errors.Is(err, ErrColumnNotWritable) {
		t.Errorf("decodeColumnValue on a TimeStamp column: err = %v, want ErrColumnNotWritable", err)
	}

	// A DateTime column of the same base type is still writable, which is what
	// says the refusal keys on FieldType and not on the storage.
	dt := newTestReader(col("dt", BftDateTime, FieldDateTime, 0))
	if err := dt.encodeInto(make([]byte, dt.recordSize), 0, time.Unix(0, 0).UTC()); err != nil {
		t.Errorf("encodeInto on a DateTime column: %v", err)
	}
}

// TestEncodeGUIDIsAString records that a GUID column is writable. It stores
// Char, so its text goes through the string path like any other fixed string
// and round-trips through the accessor.
func TestEncodeGUIDIsAString(t *testing.T) {
	const text = "{3F2504E0-4F89-11D3-9A0C-0305E82C3301}"

	r := newTestReader(col("g", BftChar, FieldGUID, guidTextSize))

	slot := make([]byte, r.recordSize)
	if err := r.encodeInto(slot, 0, text); err != nil {
		t.Fatalf("encodeInto on a GUID column: %v", err)
	}

	if got, want := recordOver(r, slot).GUID(0).String(), "3f2504e0-4f89-11d3-9a0c-0305e82c3301"; got != want {
		t.Errorf("GUID(0) = %q, want %q", got, want)
	}
}
