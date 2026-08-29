package absdb

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestConstraintRecordsReserializeByteForByte is the oracle for the constraint
// serializer. No fixture shows the engine adding a constraint to a table that
// did not have one, so byte identity is taken from the other direction: every
// record the corpus holds is parsed and written again, and the bytes have to
// come back the same.
//
// That is a real engine oracle rather than a round trip through this package's
// own model, because the comparison is against data[rec.start:rec.end] -- the
// bytes DBManager wrote -- and not against a re-parse. A serializer that
// agreed with parseConstraintRecord but disagreed with the engine (a size
// field that excluded the length byte, say, or a body count written as an
// int32 in a column-shaped record) fails here.
func TestConstraintRecordsReserializeByteForByte(t *testing.T) {
	kinds := map[constraintKind]int{}
	total := 0

	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			for _, info := range tables {
				raw, _, _, constraints := tailOf(t, db, info.Name)

				for i, rec := range constraints {
					kinds[rec.kind]++
					total++

					got, err := serializeConstraintRecord(rec)
					if err != nil {
						t.Errorf("%s.%s constraint %d (%s %q): %v", name, info.Name, i, rec.kind, rec.name, err)

						continue
					}

					if want := raw[rec.start:rec.end]; !bytes.Equal(got, want) {
						t.Errorf("%s.%s constraint %d (%s %q):\n got % x\nwant % x",
							name, info.Name, i, rec.kind, rec.name, got, want)
					}
				}
			}
		})
	}

	// A count in the log rather than an assertion: how many records the corpus
	// actually offers depends on which private fixtures are present, and a
	// fresh clone has only Constraints.abs and Types.abs. What must hold on
	// every clone is the kind coverage below.
	t.Logf("re-serialized %d constraint record(s): %v", total, kinds)
}

// TestConstraintRecordsReserializeEveryKind pins the coverage the test above
// cannot: Constraints.abs is committed, so all four kinds, both bodies, a
// multi-column key and a CHECK record's two bounds are re-serialized on every
// clone and in CI, not only on a machine holding the private corpus.
func TestConstraintRecordsReserializeEveryKind(t *testing.T) {
	db := openFixture(t, constraintsFixture)

	want := map[string]constraintKind{
		"CPk":      constraintPrimaryKey,
		"CUnique":  constraintUnique,
		"CNotNull": constraintNotNull,
		"CMinMax":  constraintCheck,
		"CPkMulti": constraintPrimaryKey,
	}

	for table, kind := range want {
		t.Run(table, func(t *testing.T) {
			raw, _, _, constraints := tailOf(t, db, table)

			if len(constraints) != 1 {
				t.Fatalf("%s carries %d constraint records, want 1", table, len(constraints))
			}

			rec := constraints[0]
			if rec.kind != kind {
				t.Fatalf("%s carries a %s record, want %s", table, rec.kind, kind)
			}

			got, err := serializeConstraintRecord(rec)
			if err != nil {
				t.Fatalf("serializeConstraintRecord: %v", err)
			}

			if w := raw[rec.start:rec.end]; !bytes.Equal(got, w) {
				t.Errorf("%s:\n got % x\nwant % x", table, got, w)
			}
		})
	}

	// CPkMulti is the only record anywhere covering more than one column, so
	// it is the only evidence that the covered-column loop repeats the object
	// id and the sized name rather than writing a name list.
	multi := constraintsOf(t, db, "CPkMulti")
	if len(multi) == 1 && len(multi[0].columns) != 2 {
		t.Errorf("CPkMulti covers %d columns, want 2", len(multi[0].columns))
	}
}

// TestConstraintArrayReserializesEveryTable widens the record-level check to
// the array: the int32 count and the records concatenated must reproduce the
// whole span between the index array and the trailer. That is what a rebuild
// writes, and it is the span parseSchemaTail's own length check anchors on.
func TestConstraintArrayReserializesEveryTable(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			for _, info := range tables {
				raw, tailStart, _, constraints := tailOf(t, db, info.Name)

				got, err := serializeConstraintArray(constraints)
				if err != nil {
					t.Errorf("%s.%s: %v", name, info.Name, err)

					continue
				}

				want := raw[tailStart : len(raw)-systemIndexRootsSize]
				if !bytes.Equal(got, want) {
					t.Errorf("%s.%s constraint array:\n got % x\nwant % x", name, info.Name, got, want)
				}
			}
		})
	}
}

// TestSerializeConstraintRecordRefusals covers the shapes the serializer will
// not write. Each is a record no parse can produce -- kind 1 is refused on the
// way in, and both bodies constrain their column count -- so these can only
// arise from a caller building one, which is exactly what CreateTable and the
// compaction rebuild do.
func TestSerializeConstraintRecordRefusals(t *testing.T) {
	for _, tt := range []struct {
		name string
		rec  constraintRecord
		want string
	}{
		{
			name: "unsupported kind",
			rec:  constraintRecord{kind: constraintKind(1), name: "C"},
			want: "unsupported constraint kind 1",
		},
		{
			name: "column-shaped without a column",
			rec:  constraintRecord{kind: constraintNotNull, name: "C", table: "T"},
			want: "0 covered columns",
		},
		{
			name: "column-shaped with two columns",
			rec: constraintRecord{
				kind: constraintNotNull, name: "C", table: "T",
				columns: []constraintColumn{{name: "A"}, {name: "B"}},
			},
			want: "2 covered columns",
		},
		{
			name: "key-shaped without an index",
			rec: constraintRecord{
				kind: constraintPrimaryKey, name: "C", table: "T",
				columns: []constraintColumn{{name: "A"}},
			},
			want: "index name",
		},
		{
			name: "empty name",
			rec:  constraintRecord{kind: constraintNotNull, table: "T", columns: []constraintColumn{{name: "A"}}},
			want: "name",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := serializeConstraintRecord(tt.rec)
			if err == nil {
				t.Fatalf("serialized %v without complaint", tt.rec.kind)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestSerializeTypedValueRefusesAnOversizeValue pins the one bound the writer
// adds that the reader does not need: the int32 size field is built from a
// slice length, so the length has to be checked before it is narrowed.
func TestSerializeTypedValueRefusesAnOversizeValue(t *testing.T) {
	rec := constraintRecord{
		kind: constraintCheck, name: "$C_Check$T$A", table: "T",
		columns:  []constraintColumn{{name: "A"}},
		minValue: typedValue{baseType: BftVarchar, present: true, data: make([]byte, maxTypedValueSize+1)},
	}

	_, err := serializeConstraintRecord(rec)
	if !errors.Is(err, ErrValueRange) {
		t.Fatalf("err = %v, want ErrValueRange", err)
	}
}

// TestConstraintRecordRoundTripsThroughTheParser closes the loop the other
// way: bytes this serializer writes for a record the corpus does not hold must
// parse back to the same record. It is the weaker of the two directions -- it
// would pass for a serializer and parser that agreed with each other and not
// with the engine -- so it covers only what the corpus leaves untested, an
// absent MINVALUE beside a present MAXVALUE.
func TestConstraintRecordRoundTripsThroughTheParser(t *testing.T) {
	want := constraintRecord{
		kind: constraintCheck, name: "$C_Check$T$A", objectID: 42, ownerID: 7, table: "T",
		columns:  []constraintColumn{{name: "A"}},
		minValue: typedValue{baseType: BftInt32},
		maxValue: typedValue{baseType: BftInt32, present: true, data: []byte{0x63, 0, 0, 0}},
	}

	raw, err := serializeConstraintRecord(want)
	if err != nil {
		t.Fatalf("serializeConstraintRecord: %v", err)
	}

	got, next, err := parseConstraintRecord(raw, 0)
	if err != nil {
		t.Fatalf("parseConstraintRecord: %v", err)
	}

	if next != len(raw) {
		t.Errorf("parsed %d of %d bytes", next, len(raw))
	}

	if summary(got) != summary(want) {
		t.Errorf("round trip:\n got %s\nwant %s", summary(got), summary(want))
	}
}

// constraintsOf is tailOf reduced to the constraint array, for a caller that
// wants neither the raw bytes nor the index records.
func constraintsOf(t *testing.T, db *File, name string) []constraintRecord {
	t.Helper()

	_, _, _, constraints := tailOf(t, db, name) //nolint:dogsled // tailOf returns four results and this caller needs one

	return constraints
}

// summary renders the fields of a constraint record that survive a round trip
// -- start and end are positions in a stream and are deliberately left out.
func summary(rec constraintRecord) string {
	cols := make([]string, 0, len(rec.columns))
	for _, c := range rec.columns {
		cols = append(cols, fmt.Sprintf("%s/%d", c.name, c.objectID))
	}

	return fmt.Sprintf("kind=%s name=%q id=%d owner=%d table=%q index=%q cols=[%s] min=%v/%t/%x max=%v/%t/%x",
		rec.kind, rec.name, rec.objectID, rec.ownerID, rec.table, rec.index, strings.Join(cols, ","),
		rec.minValue.baseType, rec.minValue.present, rec.minValue.data,
		rec.maxValue.baseType, rec.maxValue.present, rec.maxValue.data)
}
