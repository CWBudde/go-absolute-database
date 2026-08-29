package absdb

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// Types2.abs is the second field-type fixture, written by DBManager for the
// two questions Types.abs could not settle: what a TIMESTAMP column stores,
// and why a BYTES one takes no SQL literal.
//
// Types.abs held one instant, which cannot settle a layout. This one holds
// eleven that differ in one field each, every one beside a DATETIME column
// carrying the same instant as a control, so a field the TimeStamp drops
// shows up as two rows whose TimeStamp bytes are identical and whose DateTime
// bytes are not.

// TestTypes2FixtureTimeStampLayout is the decisive test. Rows 1, 2 and 3 hold
// 01:02:03, 01:02:04 and 01:03:03 and store byte-identical TimeStamps, so the
// engine keeps no minutes and no seconds; rows 4 to 7 each move one of the
// four fields it does keep. The raw bytes are asserted beside the decoded
// value, because it is the bytes that carry the finding.
func TestTypes2FixtureTimeStampLayout(t *testing.T) {
	db := openFixture(t, "Types2.abs")

	want := map[int64]struct {
		raw   string
		stamp time.Time
		date  time.Time
	}{
		1:  {"e30703000700 0100", time.Date(2019, 3, 7, 1, 0, 0, 0, time.UTC), time.Date(2019, 3, 7, 1, 2, 3, 0, time.UTC)},
		2:  {"e30703000700 0100", time.Date(2019, 3, 7, 1, 0, 0, 0, time.UTC), time.Date(2019, 3, 7, 1, 2, 4, 0, time.UTC)},
		3:  {"e30703000700 0100", time.Date(2019, 3, 7, 1, 0, 0, 0, time.UTC), time.Date(2019, 3, 7, 1, 3, 3, 0, time.UTC)},
		4:  {"e30703000700 0200", time.Date(2019, 3, 7, 2, 0, 0, 0, time.UTC), time.Date(2019, 3, 7, 2, 2, 3, 0, time.UTC)},
		5:  {"e30703000800 0100", time.Date(2019, 3, 8, 1, 0, 0, 0, time.UTC), time.Date(2019, 3, 8, 1, 2, 3, 0, time.UTC)},
		6:  {"e30704000700 0100", time.Date(2019, 4, 7, 1, 0, 0, 0, time.UTC), time.Date(2019, 4, 7, 1, 2, 3, 0, time.UTC)},
		7:  {"e40703000700 0100", time.Date(2020, 3, 7, 1, 0, 0, 0, time.UTC), time.Date(2020, 3, 7, 1, 2, 3, 0, time.UTC)},
		8:  {"e30703000700 0000", time.Date(2019, 3, 7, 0, 0, 0, 0, time.UTC), time.Date(2019, 3, 7, 0, 0, 0, 0, time.UTC)},
		9:  {"e3070c001f00 1700", time.Date(2019, 12, 31, 23, 0, 0, 0, time.UTC), time.Date(2019, 12, 31, 23, 59, 58, 0, time.UTC)},
		10: {"6c0701000100 0000", time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)},
		// The NULL row, whose eight bytes are zero. The TimeStamp decodes to
		// the zero time, because a zero year is not a date; the DateTime
		// beside it decodes to Delphi day zero, which is what that column has
		// always done for zero bytes. Neither is reached without IsNull
		// having said the field is absent.
		12: {"000000000000 0000", time.Time{}, time.Date(0, 12, 31, 0, 0, 0, 0, time.UTC)},
	}

	reader := readerOfTable(t, db, "TStamp")

	seen := 0

	for reader.Next() {
		rec := reader.Record()

		v := rec.Int64(0)

		tt, ok := want[v]
		if !ok {
			t.Errorf("unexpected row V = %d", v)

			continue
		}

		seen++

		raw, err := hex.DecodeString(strippedHex(tt.raw))
		if err != nil {
			t.Fatalf("row %d: bad expectation: %v", v, err)
		}

		if got := rec.field(1); !bytes.Equal(got, raw) {
			t.Errorf("row %d: TimeStamp bytes = % x, want % x", v, got, raw)
		}

		if got := rec.Time(1); !got.Equal(tt.stamp) {
			t.Errorf("row %d: TimeStamp = %v, want %v", v, got, tt.stamp)
		}

		if got := rec.Time(3); !got.Equal(tt.date) {
			t.Errorf("row %d: DateTime control = %v, want %v", v, got, tt.date)
		}

		if got := rec.Int64(2); got != sentinelA {
			t.Errorf("row %d: sentinel after the TimeStamp = %d, want %d", v, got, sentinelA)
		}

		if got := rec.Int64(4); got != sentinelB {
			t.Errorf("row %d: sentinel after the DateTime = %d, want %d", v, got, sentinelB)
		}

		if null := rec.IsNull(1); null != (v == 12) {
			t.Errorf("row %d: IsNull(TimeStamp) = %v", v, null)
		}
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}

	// Eleven of the twelve statements ran. Row 11 held
	// '2019-03-07 01:02:03.456' and the engine rejected the literal outright,
	// which is the only thing the fixture says about fractional seconds.
	if seen != len(want) {
		t.Errorf("read %d rows, want %d", seen, len(want))
	}
}

// TestTypes2FixtureTimeStampEncodes runs the encoder against the engine's own
// bytes: every instant Record.Time reads out of the fixture must encode back
// to exactly the eight bytes the engine wrote. That is the byte-level oracle
// for the write path, and it is what says the encoder is not merely the
// inverse of this package's own decoder.
func TestTypes2FixtureTimeStampEncodes(t *testing.T) {
	db := openFixture(t, "Types2.abs")

	reader := readerOfTable(t, db, "TStamp")
	schema := schemaOfTable(t, db, "TStamp")

	stamp := schema.Columns[1]
	if stamp.FieldType != FieldTimeStamp {
		t.Fatalf("column 1 is %s, want a TimeStamp", stamp.FieldType)
	}

	rows := 0

	for reader.Next() {
		rec := reader.Record()
		rows++

		field := make([]byte, len(rec.field(1)))
		if err := encodeField(stamp, field, rec.Time(1)); err != nil {
			t.Fatalf("row %d: encodeField: %v", rec.Int64(0), err)
		}

		if !bytes.Equal(field, rec.field(1)) {
			t.Errorf("row %d: re-encoded % x, engine wrote % x", rec.Int64(0), field, rec.field(1))
		}
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}

	if rows == 0 {
		t.Fatal("no rows")
	}
}

// TestTypes2FixtureBinaryColumnsStayNull records the negative result, which is
// as much of a finding as the TimeStamp is. Six ways of writing a BYTES(8)
// value and five of writing a VARBYTES(8) one were all rejected by the
// engine, so every B column here is NULL while its sentinel still reads --
// the widths are pinned, the contents are unreachable.
//
// Two rows keep that from being a story about the statements. Row 8 of TBin2
// was inserted as row 7 and renumbered by an UPDATE, so UPDATE reaches this
// table; and TBlob2 holds the same MIMETOBIN payload the BYTES columns
// refused, which says MIMETOBIN builds a BLOB value -- the parser's node type
// is TABSExprNodeBlob -- assignable to a BLOB column and not to a fixed one.
func TestTypes2FixtureBinaryColumnsStayNull(t *testing.T) {
	db := openFixture(t, "Types2.abs")

	for _, tt := range []struct {
		table    string
		sentinel int64
		rows     []int64
	}{
		// 1..4 are MIMETOBIN of 8, 9, 7 and 4 bytes; 5 is a plain string; 6
		// is a CAST. 8 is the control row, renumbered from 7 by an UPDATE.
		{"TBin2", sentinelA, []int64{1, 2, 3, 4, 5, 6, 8}},
		{"TVar2", sentinelB, []int64{1, 2, 3, 4, 5}},
	} {
		t.Run(tt.table, func(t *testing.T) {
			reader := readerOfTable(t, db, tt.table)

			var got []int64

			for reader.Next() {
				rec := reader.Record()
				got = append(got, rec.Int64(0))

				if !rec.IsNull(1) {
					t.Errorf("row %d: the binary column is not NULL — the engine took a literal after all",
						rec.Int64(0))
				}

				if s := rec.Int64(2); s != tt.sentinel {
					t.Errorf("row %d: sentinel = %d, want %d — the binary column is the wrong width",
						rec.Int64(0), s, tt.sentinel)
				}
			}

			if err := reader.Err(); err != nil {
				t.Fatalf("iteration: %v", err)
			}

			if len(got) != len(tt.rows) {
				t.Fatalf("rows = %v, want %v", got, tt.rows)
			}

			for i, want := range tt.rows {
				if got[i] != want {
					t.Errorf("rows = %v, want %v", got, tt.rows)

					break
				}
			}
		})
	}

	t.Run("TBlob2", func(t *testing.T) {
		want := []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}

		reader := readerOfTable(t, db, "TBlob2")
		if !reader.Next() {
			t.Fatalf("no rows: %v", reader.Err())
		}

		got, err := reader.Record().Blob(1)
		if err != nil {
			t.Fatalf("Blob(): %v", err)
		}

		if !bytes.Equal(got, want) {
			t.Errorf("BLOB = % x, want % x", got, want)
		}
	})
}

// strippedHex drops the spaces the expectations above use to mark the field
// boundary inside a TimeStamp -- the six bytes of year, month and day, then
// the hour's two.
func strippedHex(s string) string {
	out := make([]byte, 0, len(s))

	for i := range len(s) {
		if s[i] != ' ' {
			out = append(out, s[i])
		}
	}

	return string(out)
}
