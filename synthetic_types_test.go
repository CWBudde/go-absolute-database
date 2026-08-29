package absdb

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// This file covers the field types the fixture corpus does not reach.
//
// Only six base types occur in customer data -- Int32, Varchar, Double,
// Logical, Blob and Clob -- so the rest of the reader's type switches are
// exercised nowhere by a real file. The synthetic builders in
// synthetic_test.go assemble a valid .abs layout in memory, which lets every
// remaining type be read through the whole path a real file takes: page model,
// schema page, record layout, null flags and the typed accessors.
//
// What this is not is corpus evidence. The bytes here are this package's own
// idea of how the engine stores each type, so a test passing proves the
// reader agrees with the encoder, not that either agrees with the engine.
// docs/open-questions.md tracks that gap; closing it needs a file the engine
// itself wrote.

// synthUint16 encodes a Word column's two bytes.
func synthUint16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)

	return b
}

// synthUint32 encodes a Cardinal column's four bytes.
func synthUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)

	return b
}

// synthCurrency encodes a Currency column, which stores a Delphi Currency:
// an int64 scaled by 10000, not a float.
func synthCurrency(v float64) []byte {
	return synthInt64(int64(math.Round(v * currencyScale)))
}

// synthDate encodes a Date column: an int32 count of days with 0001-01-01 as
// day 1.
func synthDate(t testing.TB, tm time.Time) []byte {
	t.Helper()

	days, err := delphiDate(tm)
	if err != nil {
		t.Fatalf("delphiDate(%v): %v", tm, err)
	}

	return synthInt32(days)
}

// synthTimeOfDay encodes a Time column: an int32 count of milliseconds since
// midnight.
func synthTimeOfDay(t testing.TB, tm time.Time) []byte {
	t.Helper()

	ms, err := delphiTimeOfDay(tm)
	if err != nil {
		t.Fatalf("delphiTimeOfDay(%v): %v", tm, err)
	}

	return synthInt32(ms)
}

// synthDateTime encodes a DateTime column, which is the two int32s of
// TABSDateTime side by side: the date first, then the time.
func synthDateTime(t testing.TB, tm time.Time) []byte {
	t.Helper()

	return append(synthDate(t, tm), synthTimeOfDay(t, tm)...)
}

// synthBytes encodes a fixed-width Bytes column, zero padded.
func synthBytes(b []byte, size uint32) []byte {
	out := make([]byte, size)
	copy(out, b)

	return out
}

// typeCase is one column of the wide table below, paired with the value it
// should read back as through its accessor.
type typeCase struct {
	column synthColumn
	stored []byte
	check  func(t *testing.T, rec Record, col int)
}

// TestSyntheticEveryReadableType reads one row holding every type the reader
// decodes, through the real page and record path rather than through the
// accessors alone.
func TestSyntheticEveryReadableType(t *testing.T) {
	date := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	stamp := time.Date(2019, 3, 7, 1, 2, 3, 0, time.UTC)
	guid := []byte{
		0x40, 0xfc, 0x29, 0x6b, 0x47, 0xca, 0x67, 0x10,
		0xb3, 0x1d, 0x00, 0xdd, 0x01, 0x06, 0x62, 0xda,
	}

	cases := []typeCase{
		{
			column: synthColumn{"FShortInt", BftInt8, FieldShortInt, 0},
			stored: []byte{0xAA}, // -86
			check:  wantInt64(-86),
		},
		{
			column: synthColumn{"FByte", BftUint8, FieldByte, 0},
			stored: []byte{0xB7}, // 183
			check:  wantInt64(183),
		},
		{
			column: synthColumn{"FSmallInt", BftInt16, FieldSmallInt, 0},
			stored: synthInt16(-32768),
			check:  wantInt64(-32768),
		},
		{
			column: synthColumn{"FWord", BftUint16, FieldWord, 0},
			stored: synthUint16(65535),
			check:  wantInt64(65535),
		},
		{
			column: synthColumn{"FInteger", BftInt32, FieldInteger, 0},
			stored: synthInt32(-2147483648),
			check:  wantInt64(-2147483648),
		},
		{
			column: synthColumn{"FCardinal", BftUint32, FieldCardinal, 0},
			stored: synthUint32(4294967295),
			check:  wantInt64(4294967295),
		},
		{
			column: synthColumn{"FLargeInt", BftInt64, FieldLargeInt, 0},
			stored: synthInt64(math.MaxInt64),
			check:  wantInt64(math.MaxInt64),
		},
		{
			// Currency reads two ways: Int64 gives the raw scaled value and
			// Float divides it, which is the contract Record.Int64 documents.
			column: synthColumn{"FCurrency", BftCurrency, FieldCurrency, 0},
			stored: synthCurrency(8765.4321),
			check: func(t *testing.T, rec Record, col int) {
				t.Helper()

				if got, want := rec.Int64(col), int64(87654321); got != want {
					t.Errorf("Int64() = %d, want %d", got, want)
				}

				if got, want := rec.Float(col), 8765.4321; math.Abs(got-want) > 1e-9 {
					t.Errorf("Float() = %v, want %v", got, want)
				}
			},
		},
		{
			column: synthColumn{"FSingle", BftSingle, FieldSingle, 0},
			stored: synthSingle(1.5),
			check:  wantFloat(1.5),
		},
		{
			column: synthColumn{"FDouble", BftDouble, FieldDouble, 0},
			stored: synthDouble(2.718281828459045),
			check:  wantFloat(2.718281828459045),
		},
		{
			column: synthColumn{"FBoolean", BftLogical, FieldBoolean, 0},
			stored: synthBool(true),
			check: func(t *testing.T, rec Record, col int) {
				t.Helper()

				if !rec.Bool(col) {
					t.Error("Bool() = false, want true")
				}
			},
		},
		{
			column: synthColumn{"FChar", BftChar, FieldChar, 6},
			stored: synthString(t, "Straße", 6),
			check:  wantString("Straße"),
		},
		{
			column: synthColumn{"FVarchar", BftVarchar, FieldString, 12},
			stored: synthString(t, "Grüße", 12),
			check:  wantString("Grüße"),
		},
		{
			column: synthColumn{"FWideChar", BftWideChar, FieldWideChar, 4},
			stored: synthWideString("Tag", 4),
			check:  wantString("Tag"),
		},
		{
			column: synthColumn{"FWideString", BftWideVarchar, FieldWideString, 8},
			stored: synthWideString("Nachtzug", 8),
			check:  wantString("Nachtzug"),
		},
		{
			column: synthColumn{"FDate", BftDate, FieldDate, 0},
			stored: synthDate(t, date),
			check:  wantTime(date),
		},
		{
			column: synthColumn{"FTime", BftTime, FieldTime, 0},
			stored: synthTimeOfDay(t, stamp),
			check:  wantTime(time.Date(1, 1, 1, 1, 2, 3, 0, time.UTC)),
		},
		{
			column: synthColumn{"FDateTime", BftDateTime, FieldDateTime, 0},
			stored: synthDateTime(t, stamp),
			check:  wantTime(stamp),
		},
		{
			column: synthColumn{"FBytes", BftBytes, FieldBytes, 8},
			stored: synthBytes([]byte{0xDE, 0xAD, 0xC0, 0xDE, 0, 0x11, 0x22, 0x33}, 8),
			check: func(t *testing.T, rec Record, col int) {
				t.Helper()

				want := []byte{0xDE, 0xAD, 0xC0, 0xDE, 0, 0x11, 0x22, 0x33}
				if got := rec.Bytes(col); !bytes.Equal(got, want) {
					t.Errorf("Bytes() = % x, want % x", got, want)
				}
			},
		},
		{
			// A GUID column and the Bytes column above share a base type, so
			// this only reads as a GUID because of its FieldType.
			column: synthColumn{"FGUID", BftBytes, FieldGUID, guidSize},
			stored: guid,
			check: func(t *testing.T, rec Record, col int) {
				t.Helper()

				want := "6b29fc40-ca47-1067-b31d-00dd010662da"
				if got := rec.GUID(col).String(); got != want {
					t.Errorf("GUID() = %q, want %q", got, want)
				}
			},
		},
	}

	cols := make([]synthColumn, 0, len(cases))
	values := make([][]byte, 0, len(cases))
	nulls := make([][]byte, 0, len(cases))

	for _, c := range cases {
		cols = append(cols, c.column)
		values = append(values, c.stored)
		nulls = append(nulls, nil)
	}

	spec := synthSpec{
		columns: cols,
		// The second row is entirely NULL, so every accessor is also exercised
		// against a column whose null-flag bit is set.
		rows: []synthRow{{values: values}, {values: nulls}},
	}

	db, err := Open(writeSynthetic(t, spec))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	if !reader.Next() {
		t.Fatalf("Next: no first row (%v)", reader.Err())
	}

	rec := reader.Record()

	for i, c := range cases {
		t.Run(c.column.name, func(t *testing.T) {
			if rec.IsNull(i) {
				t.Fatalf("column %d reads as NULL", i)
			}

			c.check(t, rec, i)
		})
	}

	if !reader.Next() {
		t.Fatalf("Next: no second row (%v)", reader.Err())
	}

	null := reader.Record()

	for i, c := range cases {
		if !null.IsNull(i) {
			t.Errorf("column %q of the all-NULL row: IsNull() = false, want true", c.column.name)
		}
	}
}

func wantInt64(want int64) func(*testing.T, Record, int) {
	return func(t *testing.T, rec Record, col int) {
		t.Helper()

		if got := rec.Int64(col); got != want {
			t.Errorf("Int64() = %d, want %d", got, want)
		}
	}
}

func wantFloat(want float64) func(*testing.T, Record, int) {
	return func(t *testing.T, rec Record, col int) {
		t.Helper()

		if got := rec.Float(col); got != want {
			t.Errorf("Float() = %v, want %v", got, want)
		}
	}
}

func wantString(want string) func(*testing.T, Record, int) {
	return func(t *testing.T, rec Record, col int) {
		t.Helper()

		if got := rec.String(col); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func wantTime(want time.Time) func(*testing.T, Record, int) {
	return func(t *testing.T, rec Record, col int) {
		t.Helper()

		if got := rec.Time(col); !got.Equal(want) {
			t.Errorf("Time() = %v, want %v", got, want)
		}
	}
}
