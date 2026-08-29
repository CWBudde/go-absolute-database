package absdb

import (
	"testing"
	"time"
)

// Types.abs is the answer to docs/open-questions.md's standing request for "a
// round-trip against the Delphi engine: create a table with known values in
// every type". Its eight tables were written by DBManager from one SQL script,
// so unlike synthetic_types_test.go's hand-built records these bytes are the
// engine's own.
//
// Every column of an unknown stored width is followed by a LargeInt sentinel
// carrying a distinctive eight-byte pattern, so a width this package gets
// wrong shows up as a sentinel that no longer reads. That is what caught the
// three defects this fixture found: BYTES and VARBYTES store one byte more
// than their declared size, Currency stores a double rather than a scaled
// int64, and a column with real AUTOINC options could not be parsed at all.

// Sentinel values, little-endian 0x0102030405060708 and friends.
const (
	sentinelA int64 = 72623859790382856
	sentinelB int64 = 1230066625199609624
	sentinelC int64 = 2387509390608836392
	sentinelD int64 = 3544952156018063160
)

// TestTypesFixtureColumnTypes records what the engine actually wrote for each
// column: its two type bytes and its declared size. GUID is the reason this
// test exists -- it is a Char column of 38 characters, not the 16-byte array
// the SDK's TABSGuid typedef suggests -- and TimeStamp is the second, sharing
// BftDateTime's base type without sharing its layout.
func TestTypesFixtureColumnTypes(t *testing.T) {
	db := openFixture(t, "Types.abs")

	for _, tt := range []struct {
		table   string
		columns []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}
	}{
		{"TInt", []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}{
			{"N1", BftInt8, FieldShortInt, 0},
			{"N2", BftUint8, FieldByte, 0},
			{"N3", BftInt16, FieldSmallInt, 0},
			{"N4", BftUint16, FieldWord, 0},
			{"N5", BftInt32, FieldInteger, 0},
			{"N6", BftUint32, FieldCardinal, 0},
			{"N7", BftInt64, FieldLargeInt, 0},
			{"N8", BftLogical, FieldBoolean, 0},
		}},
		{"TReal", []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}{
			{"R1", BftSingle, FieldSingle, 0},
			{"R2", BftDouble, FieldDouble, 0},
			{"R3", BftExtended, FieldExtended, 0},
			{"R4", BftCurrency, FieldCurrency, 0},
		}},
		{"TChr", []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}{
			{"C1", BftChar, FieldChar, 10},
			{"C2", BftVarchar, FieldString, 12},
			// VARCHAR is an alias of STRING, not a distinct storage.
			{"C3", BftVarchar, FieldString, 14},
			{"C4", BftWideChar, FieldWideChar, 6},
			{"C5", BftWideVarchar, FieldWideString, 8},
		}},
		{"TTime", []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}{
			{"D1", BftDate, FieldDate, 0},
			{"T1", BftTime, FieldTime, 0},
			{"DT1", BftDateTime, FieldDateTime, 0},
			// A TimeStamp is a DateTime as far as the base type goes; only
			// the advanced type tells them apart, and their layouts differ.
			{"TS1", BftDateTime, FieldTimeStamp, 0},
		}},
		{"TBin", []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}{
			{"B1", BftBytes, FieldBytes, 8},
			{"V1", BftVarBytes, FieldVarBytes, 8},
		}},
		{"TGuid", []struct {
			name string
			base BaseFieldType
			ft   FieldType
			size uint32
		}{
			{"V", BftInt16, FieldSmallInt, 0},
			// The finding: a GUID is stored as its 38-character text.
			{"G", BftChar, FieldGUID, guidTextSize},
			{"B", BftBytes, FieldBytes, 16},
		}},
	} {
		t.Run(tt.table, func(t *testing.T) {
			schema := schemaOfTable(t, db, tt.table)

			byName := make(map[string]Column, len(schema.Columns))
			for _, c := range schema.Columns {
				byName[c.Name] = c
			}

			for _, want := range tt.columns {
				got, ok := byName[want.name]
				if !ok {
					t.Errorf("column %q missing", want.name)

					continue
				}

				if got.BaseType != want.base || got.FieldType != want.ft || got.Size != want.size {
					t.Errorf("column %q = base %d / %s / size %d, want base %d / %s / size %d",
						want.name, got.BaseType, got.FieldType, got.Size,
						want.base, want.ft, want.size)
				}
			}
		})
	}
}

// TestTypesFixtureSentinels is the layout check. Every sentinel is a LargeInt
// with a known value, so one that reads wrongly means the width of the column
// in front of it is wrong -- which is exactly how BYTES and VARBYTES were
// found to store one byte more than they declare.
func TestTypesFixtureSentinels(t *testing.T) {
	db := openFixture(t, "Types.abs")

	for _, tt := range []struct {
		table string
		want  map[string]int64
	}{
		{"TInt", map[string]int64{"S1": sentinelA}},
		{"TReal", map[string]int64{"S1": sentinelA, "S2": sentinelB, "S3": sentinelC, "S4": sentinelD}},
		{"TTime", map[string]int64{"S1": sentinelA, "S2": sentinelB, "S3": sentinelC, "S4": sentinelD}},
		{"TBin", map[string]int64{"S1": sentinelA, "S2": sentinelB}},
		{"TGuid", map[string]int64{"S1": sentinelA}},
	} {
		t.Run(tt.table, func(t *testing.T) {
			reader := readerOfTable(t, db, tt.table)
			schema := schemaOfTable(t, db, tt.table)

			rows := 0

			for reader.Next() {
				rec := reader.Record()
				rows++

				for i, c := range schema.Columns {
					want, ok := tt.want[c.Name]
					if !ok || rec.IsNull(i) {
						continue
					}

					if got := rec.Int64(i); got != want {
						t.Errorf("row %d column %q = %d, want %d — the column before it is the wrong width",
							rows, c.Name, got, want)
					}
				}
			}

			if err := reader.Err(); err != nil {
				t.Fatalf("iteration: %v", err)
			}

			if rows == 0 {
				t.Fatal("no rows")
			}
		})
	}
}

// TestTypesFixtureValues reads each column through its accessor and checks the
// value the SQL script inserted.
func TestTypesFixtureValues(t *testing.T) {
	db := openFixture(t, "Types.abs")

	t.Run("integers", func(t *testing.T) {
		rec := firstRow(t, db, "TInt")

		for i, want := range []int64{-86, 183, 4660, 48879, 305419896, 3735928559, 81985529216486895} {
			if got := rec.Int64(i); got != want {
				t.Errorf("column %d = %d, want %d", i, got, want)
			}
		}

		if !rec.Bool(7) {
			t.Error("N8 = false, want true")
		}
	})

	t.Run("reals", func(t *testing.T) {
		rec := firstRow(t, db, "TReal")

		if got := rec.Float(0); got != 1.5 {
			t.Errorf("Single = %v, want 1.5", got)
		}

		if got := rec.Float(2); got != 2.718281828459045 {
			t.Errorf("Double = %v, want 2.718281828459045", got)
		}

		// The Currency finding: the engine stores an IEEE-754 double, so this
		// reads exactly. Under the old scaled-int64 model it read 4.67e+14.
		if got := rec.Float(6); got != 8765.4321 {
			t.Errorf("Currency = %v, want 8765.4321", got)
		}

		// Extended is the engine's x87 80-bit float, rounded to float64 on
		// the way out. The stored bytes are 00 40 a5 bf dc bc 1b cf ff 3f,
		// and the ten they occupy are what the sentinel after them proves in
		// TestTypesFixtureSentinels.
		if got := rec.Float(4); got != 1.6180339887498949 {
			t.Errorf("Extended = %v, want 1.6180339887498949", got)
		}
	})

	t.Run("strings", func(t *testing.T) {
		rec := firstRow(t, db, "TChr")

		for i, want := range map[int]string{
			0: "CHAR-AAAA", 2: "STRING-BBBB", 4: "VARCHAR-CCCCC",
			6: "WCH-D", 8: "WSTR-EE",
		} {
			if got := rec.String(i); got != want {
				t.Errorf("column %d = %q, want %q", i, got, want)
			}
		}
	})

	t.Run("date and time", func(t *testing.T) {
		rec := firstRow(t, db, "TTime")

		if got, want := rec.Time(0), time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("Date = %v, want %v", got, want)
		}

		if got, want := rec.Time(2), time.Date(1, 1, 1, 13, 45, 56, 0, time.UTC); !got.Equal(want) {
			t.Errorf("Time = %v, want %v", got, want)
		}

		if got, want := rec.Time(4), time.Date(2019, 3, 7, 1, 2, 3, 0, time.UTC); !got.Equal(want) {
			t.Errorf("DateTime = %v, want %v", got, want)
		}

		// A TimeStamp holds the same instant in an undecoded layout, so it
		// deliberately reads as the zero time rather than as a wrong one.
		if got := rec.Time(6); !got.IsZero() {
			t.Errorf("TimeStamp = %v, want the zero time", got)
		}
	})

	t.Run("guid", func(t *testing.T) {
		const want = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

		reader := readerOfTable(t, db, "TGuid")

		seen := 0

		for reader.Next() {
			rec := reader.Record()

			// Row 3 stores a NULL GUID; the other two store the same value,
			// one braced and one bare, and both must parse to it.
			if rec.IsNull(1) {
				if got := rec.GUID(1); got != (GUID{}) {
					t.Errorf("NULL GUID = %q, want the zero GUID", got)
				}

				continue
			}

			seen++

			if got := rec.GUID(1).String(); got != want {
				t.Errorf("GUID = %q, want %q", got, want)
			}
		}

		if seen != 2 {
			t.Errorf("read %d non-NULL GUIDs, want 2", seen)
		}
	})
}

// TestTypesFixtureNullability checks Column.NotNull against the table whose
// columns differ only in their NULL clause.
func TestTypesFixtureNullability(t *testing.T) {
	db := openFixture(t, "Types.abs")
	schema := schemaOfTable(t, db, "TNull")

	want := map[string]bool{
		"F1": false, // no clause
		"F2": true,  // NOT NULL
		"F3": false, // explicit NULL
		"F4": false,
		"F5": true,
		"F6": true,
		"S1": false,
	}

	for _, c := range schema.Columns {
		notNull, known := c.NotNull()
		if !known {
			t.Errorf("column %q: NotNull() known = false, want true", c.Name)

			continue
		}

		if notNull != want[c.Name] {
			t.Errorf("column %q: NotNull() = %v, want %v", c.Name, notNull, want[c.Name])
		}
	}
}

// TestTypesFixtureAutoIncOptions is what settles the autoinc block. TAutoInc
// declares INCREMENT 5 INITIALVALUE 100 MINVALUE 10 MAXVALUE 999 NOCYCLED, and
// it is the only column anywhere that stores anything but the engine's
// defaults. Before the block was decoded this table could not be parsed at
// all: the old parser scanned for a 0x7F 0x00 pair that is only there because
// every other column's MaxValue is High(Int64).
func TestTypesFixtureAutoIncOptions(t *testing.T) {
	db := openFixture(t, "Types.abs")
	schema := schemaOfTable(t, db, "TAutoInc")

	a := schema.Columns[0]
	if a.Name != "A" {
		t.Fatalf("first column is %q, want A", a.Name)
	}

	if !a.autoInc.known {
		t.Fatal("column A: autoinc block not read")
	}

	for _, tt := range []struct {
		field string
		got   int64
		want  int64
	}{
		{"increment", a.autoInc.increment, 5},
		{"initialValue", a.autoInc.initialValue, 100},
		{"minValue", a.autoInc.minValue, 10},
		{"maxValue", a.autoInc.maxValue, 999},
	} {
		if tt.got != tt.want {
			t.Errorf("column A %s = %d, want %d", tt.field, tt.got, tt.want)
		}
	}

	if a.autoInc.cycled {
		t.Error("column A cycled = true, want false (NOCYCLED)")
	}

	if a.autoInc.engineDefault() {
		t.Error("column A engineDefault() = true, want false")
	}

	// The engine numbered the two rows from INITIALVALUE by INCREMENT, which
	// is independent corroboration that those are the fields decoded.
	reader := readerOfTable(t, db, "TAutoInc")

	var got []int64

	for reader.Next() {
		got = append(got, reader.Record().Int64(0))
	}

	if len(got) != 2 || got[0] != 105 || got[1] != 110 {
		t.Errorf("AUTOINC values = %v, want [105 110]", got)
	}

	// The second column has no AUTOINC clause and must carry the defaults.
	if b := schema.Columns[1]; !b.autoInc.engineDefault() {
		t.Errorf("column %q: engineDefault() = false, want true", b.Name)
	}
}

// TestTypesFixtureRefusesUnwritableColumns pins the two refusals the fixture
// forced, so that neither can be relaxed without a fixture that justifies it.
func TestTypesFixtureRefusesUnwritableColumns(t *testing.T) {
	db := openFixture(t, "Types.abs")

	t.Run("autoinc options", func(t *testing.T) {
		schema := schemaOfTable(t, db, "TAutoInc")

		_, err := serializeColumnDef(schema.Columns[0])
		if err == nil {
			t.Fatal("serializeColumnDef accepted a column with AUTOINC options")
		}

		// The type check fires first for an AutoInc column, which is itself
		// correct; what matters is that it is refused rather than written
		// with the options silently reset.
		t.Logf("refused with: %v", err)
	})

	t.Run("timestamp", func(t *testing.T) {
		schema := schemaOfTable(t, db, "TTime")
		reader := readerOfTable(t, db, "TTime")

		if !reader.Next() {
			t.Fatalf("no rows: %v", reader.Err())
		}

		var ts int

		for i, c := range schema.Columns {
			if c.FieldType == FieldTimeStamp {
				ts = i
			}
		}

		if _, err := decodeColumnValue(schema.Columns[ts], reader.Record(), ts); err == nil {
			t.Error("decodeColumnValue accepted a TimeStamp column")
		}
	})
}

// --- helpers ---

func schemaOfTable(t *testing.T, db *File, name string) *TableSchema {
	t.Helper()

	tbl, err := db.Table(name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}

	schema, err := tbl.Schema()
	if err != nil {
		t.Fatalf("Schema(%q): %v", name, err)
	}

	return schema
}

func readerOfTable(t *testing.T, db *File, name string) *Reader {
	t.Helper()

	tbl, err := db.Table(name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}

	reader, err := tbl.Open()
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}

	return reader
}

func firstRow(t *testing.T, db *File, name string) Record {
	t.Helper()

	reader := readerOfTable(t, db, name)
	if !reader.Next() {
		t.Fatalf("table %q has no rows: %v", name, reader.Err())
	}

	return reader.Record()
}
