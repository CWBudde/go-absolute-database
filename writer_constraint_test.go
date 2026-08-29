package absdb

import (
	"errors"
	"testing"
)

// TestWriterChecksConstraints is the behaviour the constraint gate narrowed to
// make room for: a table declaring a NOT NULL or a MINVALUE/MAXVALUE pair now
// accepts writes, and rejects exactly the rows the clause forbids.
//
// Constraints.abs is the fixture because it isolates one clause per table:
// CNotNull is "A INTEGER NOT NULL, B VARCHAR(10)" and CMinMax is the same two
// columns with "MINVALUE 0 MAXVALUE 99" on A. Both bounds are tested at the
// bound itself, not one past it, because an exclusive comparison would pass
// every test written one further out.
func TestWriterChecksConstraints(t *testing.T) {
	for _, c := range []struct {
		name   string
		table  string
		values []any
		want   error
	}{
		{"NOT NULL rejects a NULL", "CNotNull", []any{nil, "x"}, ErrNotNullViolated},
		{"NOT NULL admits a value", "CNotNull", []any{int32(1), "x"}, nil},
		{"NOT NULL ignores another column", "CNotNull", []any{int32(1), nil}, nil},
		{"MINVALUE rejects one below", "CMinMax", []any{int32(-1), "x"}, ErrCheckViolated},
		{"MINVALUE admits the bound", "CMinMax", []any{int32(0), "x"}, nil},
		{"MAXVALUE admits the bound", "CMinMax", []any{int32(99), "x"}, nil},
		{"MAXVALUE rejects one above", "CMinMax", []any{int32(100), "x"}, ErrCheckViolated},
		// The assumed rule, stated in writer_constraint.go and in
		// docs/open-questions.md: a NULL is not compared against a bound.
		{"a bound ignores a NULL", "CMinMax", []any{nil, "x"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := constrainedWriter(t, c.table)

			_, err := w.Insert(c.values)
			requireConstraintOutcome(t, err, c.want)
		})
	}
}

// TestWriterChecksConstraintsOnUpdate covers the other two write paths. Update
// and UpdateColumn share storeRecordReindexing, which is where the check sits,
// so an update that nulls a NOT NULL column has to be refused as firmly as an
// insert -- and refused before the record is stored, which is what the row
// read back afterwards checks.
func TestWriterChecksConstraintsOnUpdate(t *testing.T) {
	w := constrainedWriter(t, "CNotNull")

	id, err := w.Insert([]any{int32(7), "seven"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := w.Update(id, []any{nil, "seven"}); !errors.Is(err, ErrNotNullViolated) {
		t.Errorf("Update to NULL = %v, want ErrNotNullViolated", err)
	}

	if err := w.UpdateColumn(id, 0, nil); !errors.Is(err, ErrNotNullViolated) {
		t.Errorf("UpdateColumn to NULL = %v, want ErrNotNullViolated", err)
	}

	rec, err := w.Record(id)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if rec.IsNull(0) || rec.Int(0) != 7 {
		t.Errorf("after two refused updates A = %d (null=%t), want 7", rec.Int(0), rec.IsNull(0))
	}
}

// TestNewConstraintChecksRefusals covers what the resolver will not check.
// Each case is a record the schema tail could hold and this package cannot
// test, and every one of them has to come back as ErrConstraintsNotEnforced
// rather than as an empty check set -- a resolver that recorded what it
// understood and dropped the rest would let a write through under a clause
// nobody looked at.
func TestNewConstraintChecksRefusals(t *testing.T) {
	schema := &TableSchema{Columns: []Column{
		{Name: "A", BaseType: BftInt32, FieldType: FieldInteger, Size: 4},
		{Name: "S", BaseType: BftVarchar, FieldType: FieldString, Size: 10},
	}}

	for _, c := range []struct {
		name string
		rec  constraintRecord
	}{
		{
			name: "a PRIMARY KEY",
			rec: constraintRecord{
				kind: constraintPrimaryKey, name: "C_PK$A", table: "T", index: "C_PK$A",
				columns: []constraintColumn{{name: "A"}},
			},
		},
		{
			name: "a UNIQUE clause",
			rec: constraintRecord{
				kind: constraintUnique, name: "C_Unique$A", table: "T", index: "C_Unique$A",
				columns: []constraintColumn{{name: "A"}},
			},
		},
		{
			name: "a record naming a column the table does not have",
			rec: constraintRecord{
				kind: constraintNotNull, name: "$C_NotNull$T$Z", table: "T",
				columns: []constraintColumn{{name: "Z"}},
			},
		},
		{
			name: "bounds on a column that is not an integer",
			rec: constraintRecord{
				kind: constraintCheck, name: "$C_Check$T$S", table: "T",
				columns:  []constraintColumn{{name: "S"}},
				minValue: typedValue{baseType: BftVarchar, present: true, data: []byte("a")},
			},
		},
		{
			name: "a bound of the wrong base type",
			rec: constraintRecord{
				kind: constraintCheck, name: "$C_Check$T$A", table: "T",
				columns:  []constraintColumn{{name: "A"}},
				minValue: typedValue{baseType: BftInt64, present: true, data: make([]byte, 8)},
			},
		},
		{
			name: "a bound of the wrong width",
			rec: constraintRecord{
				kind: constraintCheck, name: "$C_Check$T$A", table: "T",
				columns:  []constraintColumn{{name: "A"}},
				maxValue: typedValue{baseType: BftInt32, present: true, data: []byte{1, 2}},
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := newConstraintChecks([]constraintRecord{c.rec}, schema, "T")
			if !errors.Is(err, ErrConstraintsNotEnforced) {
				t.Errorf("newConstraintChecks = %v, want ErrConstraintsNotEnforced", err)
			}
		})
	}
}

// TestBoundsCheckDecodesASignedBound pins the sign extension a narrow bound
// needs. Constraints.abs holds only "MINVALUE 0 MAXVALUE 99", both of which
// survive being read unsigned, so the negative case is built by hand.
func TestBoundsCheckDecodesASignedBound(t *testing.T) {
	schema := &TableSchema{Columns: []Column{
		{Name: "A", BaseType: BftInt16, FieldType: FieldSmallInt, Size: 2},
	}}

	rec := constraintRecord{
		kind: constraintCheck, name: "$C_Check$T$A", table: "T",
		columns:  []constraintColumn{{name: "A"}},
		minValue: typedValue{baseType: BftInt16, present: true, data: []byte{0xFF, 0xFF}}, // -1
		maxValue: typedValue{baseType: BftInt16, present: true, data: []byte{0x64, 0x00}}, // 100
	}

	checks, err := newConstraintChecks([]constraintRecord{rec}, schema, "T")
	if err != nil {
		t.Fatalf("newConstraintChecks: %v", err)
	}

	if len(checks.bounds) != 1 {
		t.Fatalf("resolved %d bounds checks, want 1", len(checks.bounds))
	}

	if got := checks.bounds[0]; got.min != -1 || got.max != 100 {
		t.Errorf("bounds = [%d, %d], want [-1, 100]", got.min, got.max)
	}
}

// constrainedWriter opens one table of a writable copy of Constraints.abs for
// writing. Every case needs its own copy, so a refused write cannot be
// mistaken for one an earlier case had already made.
func constrainedWriter(t *testing.T, table string) *TableWriter {
	t.Helper()

	path := writableCopy(t, constraintsFixture)

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	w, err := tbl.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter(%q): %v", table, err)
	}

	t.Cleanup(func() { w.Close() })

	return w
}

// requireConstraintOutcome fails unless err is want, treating a nil want as
// "the write went through".
func requireConstraintOutcome(t *testing.T, err, want error) {
	t.Helper()

	switch {
	case want == nil && err != nil:
		t.Errorf("write = %v, want it to be allowed", err)
	case want != nil && !errors.Is(err, want):
		t.Errorf("write = %v, want %v", err, want)
	}
}
