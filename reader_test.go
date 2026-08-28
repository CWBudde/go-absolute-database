package absdb

import (
	"encoding/binary"
	"math"
	"testing"
)

// fixtureRowCounts is the verified number of rows in every test fixture,
// measured from the B-tree leaf entries independently of the record decoder.
var fixtureRowCounts = map[string]int{
	"TS03.abs":      18,
	"RREC0011.abs":  30,
	"RCON0011.abs":  300,
	"RCFQ0011.abs":  600,
	"RMPA0011.abs":  600,
	"RR240011.abs":  30,
	"RPDG0011.abs":  30,
	"RFRQ0011.abs":  60,
	"RGRP0011.abs":  30,
	"RMND0011.abs":  10,
	"RRAI0011.abs":  5,
	"RRAI0012.abs":  5,
	"RRAI0023.abs":  5,
	"RRAD0011.abs":  20,
	"RRAD0012.abs":  20,
	"RRAD0023.abs":  20,
	"Addresses.abs": 0,
}

func TestFixtureRowCounts(t *testing.T) {
	for name, want := range fixtureRowCounts {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable(): %v", err)
			}

			got := 0
			for reader.Next() {
				reader.Record()

				got++
			}

			if err := reader.Err(); err != nil {
				t.Fatalf("iteration error: %v", err)
			}

			if got != want {
				t.Errorf("row count = %d, want %d", got, want)
			}
		})
	}
}

func TestRecordLayoutDerivation(t *testing.T) {
	// Verified layout per fixture: columns, null-flag bytes, field data size,
	// record size, record slots per page and occupancy bitmap bytes.
	tests := []struct {
		file           string
		columns        int
		nullFlagBytes  int
		fieldDataSize  int
		recordSize     int
		recordsPerPage int
		bitmapBytes    int
	}{
		{"TS03.abs", 9, 2, 97, 99, 40, 5},
		{"RREC0011.abs", 20, 3, 173, 176, 23, 3},
		{"RCON0011.abs", 36, 5, 318, 323, 12, 2},
		{"RCFQ0011.abs", 7, 1, 46, 47, 86, 11},
		{"RMPA0011.abs", 31, 4, 282, 286, 14, 2},
		{"RR240011.abs", 27, 4, 204, 208, 19, 3},
		{"RRAD0011.abs", 12, 2, 135, 137, 29, 4},
		{"RRAI0011.abs", 12, 2, 121, 123, 32, 4},
		{"RPDG0011.abs", 5, 1, 24, 25, 161, 21},
		{"RFRQ0011.abs", 5, 1, 36, 37, 109, 14},
		{"RGRP0011.abs", 6, 1, 69, 70, 57, 8},
		{"RMND0011.abs", 3, 1, 16, 17, 236, 30},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			db := openTestFile(t, tt.file)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable(): %v", err)
			}

			checks := []struct {
				name string
				got  int
				want int
			}{
				{"columns", len(reader.Schema().Columns), tt.columns},
				{"nullFlagBytes", reader.nullFlagBytes, tt.nullFlagBytes},
				{"fieldDataSize", reader.fieldDataSize, tt.fieldDataSize},
				{"recordSize", reader.recordSize, tt.recordSize},
				{"recordsPerPage", reader.recordsPerPage, tt.recordsPerPage},
				{"bitmapBytes", reader.bitmapBytes, tt.bitmapBytes},
			}

			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
				}
			}
		})
	}
}

func TestRecordsPerPageFixedPoint(t *testing.T) {
	tests := []struct {
		payload    int
		recordSize int
		want       int
	}{
		{4056, 25, 161}, // bitmap grows past the naive 162 slots
		{4056, 47, 86},
		{4056, 208, 19},
		{4056, 17, 236},
		{4056, 0, 0},  // no columns: no division by zero
		{0, 47, 0},    // empty payload
		{4056, -1, 0}, // nonsense record size
		{10, 4096, 0}, // record larger than the payload
	}

	for _, tt := range tests {
		got := recordsPerPage(tt.payload, tt.recordSize)
		if got != tt.want {
			t.Errorf("recordsPerPage(%d, %d) = %d, want %d", tt.payload, tt.recordSize, got, tt.want)
		}

		if got > 0 && (got+7)/8+got*tt.recordSize > tt.payload {
			t.Errorf("recordsPerPage(%d, %d) = %d does not fit", tt.payload, tt.recordSize, got)
		}
	}
}

func TestRCFQ0011Values(t *testing.T) {
	// 7 cols: FrqNo AutoInc, SrcNo Integer, RecNo Integer, Floor Integer,
	// ZBname String[15], SType String[5], FOT500 Double.
	want := []struct {
		frqNo  uint32
		srcNo  int32
		recNo  int32
		floor  int32
		zbName string
		sType  string
		fot500 float64
	}{
		{1, 1, 10, 1, "Tag", "Rail", 44.90930938720703},
		{2, 1, 10, 1, "Nacht", "Rail", 48.84680938720703},
		{3, 1, 10, 2, "Tag", "Rail", 46.7508544921875},
	}

	db := openTestFile(t, "RCFQ0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	for i, w := range want {
		if !reader.Next() {
			t.Fatalf("row %d: Next() = false, want a row (err %v)", i, reader.Err())
		}

		rec := reader.Record()

		if got := rec.Uint32(0); got != w.frqNo {
			t.Errorf("row %d FrqNo = %d, want %d", i, got, w.frqNo)
		}

		if got := rec.Int(1); got != w.srcNo {
			t.Errorf("row %d SrcNo = %d, want %d", i, got, w.srcNo)
		}

		if got := rec.Int(2); got != w.recNo {
			t.Errorf("row %d RecNo = %d, want %d", i, got, w.recNo)
		}

		if got := rec.Int(3); got != w.floor {
			t.Errorf("row %d Floor = %d, want %d", i, got, w.floor)
		}

		if got := rec.String(4); got != w.zbName {
			t.Errorf("row %d ZBname = %q, want %q", i, got, w.zbName)
		}

		if got := rec.String(5); got != w.sType {
			t.Errorf("row %d SType = %q, want %q", i, got, w.sType)
		}

		if got := rec.Float(6); got != w.fot500 {
			t.Errorf("row %d FOT500 = %v, want %v", i, got, w.fot500)
		}
	}
}

func TestRREC0011Row0(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	if !reader.Next() {
		t.Fatalf("expected a first row, err %v", reader.Err())
	}

	rec := reader.Record()

	// Null flags 08 00 f9: columns 3, 16 and 19 are NULL, everything else set.
	for col := range reader.Schema().Columns {
		wantNull := col == 3 || col == 16 || col == 19
		if got := rec.IsNull(col); got != wantNull {
			t.Errorf("IsNull(%d) = %v, want %v", col, got, wantNull)
		}
	}

	ints := map[int]int32{0: 10, 1: 1, 2: 65704, 7: 1, 8: 443}
	for col, want := range ints {
		if got := rec.Int(col); got != want {
			t.Errorf("col %d = %d, want %d", col, got, want)
		}
	}

	strs := map[int]string{4: "0000", 5: "Hauptstraße 4", 9: "S"}
	for col, want := range strs {
		if got := rec.String(col); got != want {
			t.Errorf("col %d = %q, want %q", col, got, want)
		}
	}

	if rec.Bool(6) {
		t.Error("Marke = true, want false")
	}

	floats := map[int]float64{
		10: 7694.167933162916,
		11: 6802.106603169757,
		12: 247.1168670654297,
		13: 245.24557879277586,
		14: 72.0,
		15: 48.692405700683594,
		17: 62.0,
		18: 52.442867279052734,
	}

	for col, want := range floats {
		if got := rec.Float(col); got != want {
			t.Errorf("col %d = %v, want %v", col, got, want)
		}
	}
}

func TestTS03ReadRecords(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	var records []Record
	for reader.Next() {
		records = append(records, reader.Record())
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}

	if len(records) == 0 {
		t.Fatal("expected at least 1 record")
	}

	// Column 0: ZugArt (AutoInc) - should be 1.
	rec := records[0]
	if autoInc := rec.Uint32(0); autoInc != 1 {
		t.Errorf("record 0 ZugArt = %d, want 1", autoInc)
	}

	// Column 1: Name (String/40) - first train type name.
	if name := rec.String(1); name == "" {
		t.Error("record 0 Name is empty")
	}

	// Column 2: SBA (Double) - should be a reasonable float.
	if sba := rec.Float(2); math.IsNaN(sba) || math.IsInf(sba, 0) {
		t.Errorf("record 0 SBA = %f, expected a finite number", sba)
	}
}

func TestRREC0011ReadRecords(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	var records []Record
	for reader.Next() {
		records = append(records, reader.Record())
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}

	if len(records) == 0 {
		t.Fatal("expected at least 1 record")
	}

	// Column 5: Name (String/30), columns 10/11: X/Y coordinates (Double).
	rec := records[0]
	if name := rec.String(5); name == "" {
		t.Error("record 0 Name is empty")
	}

	x := rec.Float(10)
	y := rec.Float(11)

	if x < 100 || y < 100 {
		t.Errorf("record 0 X=%.2f, Y=%.2f: expected reasonable coordinates", x, y)
	}
}

// TestAddressesNoData checks that a table without data pages opens cleanly and
// yields an iterator with no rows, rather than failing with ErrNoData.
func TestAddressesNoData(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	if reader.Next() {
		t.Error("Next() = true on an empty table, want false")
	}

	if err := reader.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}

	// Record on an exhausted iterator must be safe and report every column null.
	rec := reader.Record()
	for col := range reader.Schema().Columns {
		if !rec.IsNull(col) {
			t.Errorf("col %d: IsNull() = false on an empty iterator", col)
		}
	}
}

// TestRecordIsIdempotent checks that Record has no side effects: calling it
// twice per Next must return the same record, and iteration must not skip rows.
func TestRecordIsIdempotent(t *testing.T) {
	db := openTestFile(t, "RCFQ0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	count := 0

	for reader.Next() {
		first := reader.Record()
		second := reader.Record()

		if first.Uint32(0) != second.Uint32(0) {
			t.Fatalf("row %d: Record() returned %d then %d", count, first.Uint32(0), second.Uint32(0))
		}

		count++
	}

	if want := fixtureRowCounts["RCFQ0011.abs"]; count != want {
		t.Errorf("row count with two Record() calls per row = %d, want %d", count, want)
	}
}

// TestAccessorsNeverPanic checks that out-of-range column indexes and truncated
// field data yield zero values instead of panicking.
func TestAccessorsNeverPanic(t *testing.T) {
	db := openTestFile(t, "RREC0011.abs")

	reader, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable(): %v", err)
	}

	if !reader.Next() {
		t.Fatalf("expected a first row, err %v", reader.Err())
	}

	full := reader.Record()

	// A record whose field data has been cut short mid-field.
	truncated := Record{
		reader:    reader,
		nullFlags: full.nullFlags,
		fieldData: full.fieldData[:3],
	}

	cols := []int{-1, 0, 5, len(reader.Schema().Columns), 1 << 20}
	for _, rec := range []Record{full, truncated, {reader: reader}} {
		for _, col := range cols {
			rec.Int(col)
			rec.Int16(col)
			rec.Int64(col)
			rec.Uint16(col)
			rec.Uint32(col)
			rec.Float(col)
			rec.String(col)
			rec.Bool(col)
			rec.Time(col)
			rec.Bytes(col)
			rec.IsNull(col)
		}
	}
}

// TestNarrowAndWideAccessors covers the field types no fixture contains:
// Single, SmallInt, Word, LargeInt, Currency and WideString. The record bytes
// are built by hand, so this verifies the decoders by construction only.
func TestNarrowAndWideAccessors(t *testing.T) {
	cols := []Column{
		{Name: "S", BaseType: BftSingle, FieldType: FieldSingle},
		{Name: "SI", BaseType: BftInt16, FieldType: FieldSmallInt},
		{Name: "W", BaseType: BftUint16, FieldType: FieldWord},
		{Name: "L", BaseType: BftInt64, FieldType: FieldLargeInt},
		{Name: "C", BaseType: BftCurrency, FieldType: FieldCurrency},
		{Name: "WS", BaseType: BftWideVarchar, FieldType: FieldWideString, Size: 8},
	}

	reader := &Reader{schema: &TableSchema{Columns: cols}}
	reader.computeRecordLayout()

	if want := 4 + 2 + 2 + 8 + 8 + 18; reader.fieldDataSize != want {
		t.Fatalf("fieldDataSize = %d, want %d", reader.fieldDataSize, want)
	}

	data := make([]byte, reader.fieldDataSize)
	binary.LittleEndian.PutUint32(data[0:4], math.Float32bits(1.5))
	binary.LittleEndian.PutUint16(data[4:6], uint16(0xfffe))       // int16 -2
	binary.LittleEndian.PutUint16(data[6:8], 65535)                // word
	binary.LittleEndian.PutUint64(data[8:16], ^uint64(4))          // int64 -5
	binary.LittleEndian.PutUint64(data[16:24], uint64(12_345_678)) // currency 1234.5678
	for i, u := range []uint16{'H', 'a', 'l', 'l', 'o', 0} {
		binary.LittleEndian.PutUint16(data[24+2*i:26+2*i], u)
	}

	rec := Record{reader: reader, nullFlags: make([]byte, reader.nullFlagBytes), fieldData: data}

	if got := rec.Float(0); got != 1.5 {
		t.Errorf("Single Float = %v, want 1.5", got)
	}

	if got := rec.Int16(1); got != -2 {
		t.Errorf("SmallInt Int16 = %d, want -2", got)
	}

	if got := rec.Int(1); got != -2 {
		t.Errorf("SmallInt Int = %d, want -2 (sign extended)", got)
	}

	if got := rec.Uint16(2); got != 65535 {
		t.Errorf("Word Uint16 = %d, want 65535", got)
	}

	if got := rec.Int(2); got != 65535 {
		t.Errorf("Word Int = %d, want 65535 (zero extended)", got)
	}

	if got := rec.Int64(3); got != -5 {
		t.Errorf("LargeInt Int64 = %d, want -5", got)
	}

	if got := rec.Float(4); got != 1234.5678 {
		t.Errorf("Currency Float = %v, want 1234.5678", got)
	}

	if got := rec.String(5); got != "Hallo" {
		t.Errorf("WideString String = %q, want %q", got, "Hallo")
	}
}

func TestRRAIEmissionFiles(t *testing.T) {
	files := []string{
		"RRAI0011.abs",
		"RRAI0012.abs",
		"RRAI0023.abs",
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable(): %v", err)
			}

			if !reader.Next() {
				t.Fatal("expected at least one record")
			}

			rec := reader.Record()
			if err := reader.Err(); err != nil {
				t.Fatalf("iteration error: %v", err)
			}

			if got := rec.Uint32(0); got != 1 {
				t.Fatalf("IDX = %d, want 1", got)
			}

			if got := rec.Int(1); got <= 0 {
				t.Fatalf("ObjID = %d, want > 0", got)
			}

			if got := rec.String(2); got == "" {
				t.Fatal("Railname is empty")
			}

			for _, col := range []int{3, 4, 10, 11} {
				v := rec.Float(col)
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("col %d = %v, want finite", col, v)
				}
			}
		})
	}
}

func TestRRADEmissionFiles(t *testing.T) {
	files := []string{
		"RRAD0011.abs",
		"RRAD0012.abs",
		"RRAD0023.abs",
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable(): %v", err)
			}

			if !reader.Next() {
				t.Fatal("expected at least one record")
			}

			rec := reader.Record()
			if err := reader.Err(); err != nil {
				t.Fatalf("iteration error: %v", err)
			}

			if got := rec.Uint32(0); got != 1 {
				t.Fatalf("No = %d, want 1", got)
			}

			if got := rec.Int(1); got <= 0 {
				t.Fatalf("IDX = %d, want > 0", got)
			}

			if got := rec.String(2); got == "" {
				t.Fatal("Trainname is empty")
			}

			if rec.Bool(9) {
				t.Fatal("Max = true, want false for first record")
			}

			for _, col := range []int{3, 4, 6, 10, 11} {
				v := rec.Float(col)
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("col %d = %v, want finite", col, v)
				}
			}
		})
	}
}
