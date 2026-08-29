package absdb

import (
	"bytes"
	"testing"
)

// Constraints.abs's column-shaped tables, and the columns DBManager created
// them with. Both are "A INTEGER, B VARCHAR(10)" -- CNotNull adds NOT NULL to
// A, CMinMax adds MINVALUE 0 MAXVALUE 99 -- which is what makes them a
// difference of one constraint record and nothing else.
// An Integer column's Size is 0, not 4: the engine sizes a fixed-width column
// from its base type and leaves the declared size at zero, which is what
// Constraints.abs's own column definitions carry.
var constraintTestColumns = []Column{
	{Name: "A", BaseType: BftInt32, FieldType: FieldInteger},
	{Name: "B", BaseType: BftVarchar, FieldType: FieldString, Size: 10},
}

// TestCreateTableWritesTheEngineSchemaStream is the oracle for writing a
// constraint record in place, as opposed to writing the record itself
// (TestConstraintRecordsReserializeByteForByte). It replays the CREATE TABLE
// statements that produced Constraints.abs into a database this package
// created, and requires the resulting schema stream to be the engine's own,
// byte for byte.
//
// The object ids are part of that claim, which is why the replay does not start
// at the table under test. Constraints.abs hands out ids in catalog order, so
// reaching CNotNull's means creating CNone first, and reaching CMinMax's means
// accounting for the three tables in between that this package cannot create:
// CPk and CUnique take five ids each (a table, two columns, an index and a
// constraint) and CDefault takes three. Tables of the same shape stand in for
// them -- they exist to consume ids, nothing else -- and the fact that CMinMax
// then lands on the engine's own bytes is the check that the id arithmetic in
// ddl_constraint.go's file comment is right.
//
// Only the trailing eight bytes are excluded: they are the record-page and BLOB
// index root page numbers, which depend on where in the file the table landed
// and not on what it declares.
func TestCreateTableWritesTheEngineSchemaStream(t *testing.T) {
	fixture := openFixture(t, constraintsFixture)

	db, err := CreateDatabase(newDatabasePath(t, "replay.abs"), CreateDatabaseOptions{})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	defer db.Close()

	// The replay, in catalog order. Every step is either a table under test or
	// a stand-in consuming exactly the ids the engine's own table consumed.
	for _, step := range []struct {
		table   string
		columns []Column
		check   bool
	}{
		{table: "CNone", columns: constraintTestColumns, check: true},
		{table: "CNotNull", columns: constraintTestColumns, check: true},
		{table: "FillerForCPk", columns: fillerColumns(4)},
		{table: "FillerForCUnique", columns: fillerColumns(4)},
		{table: "FillerForCDefault", columns: fillerColumns(2)},
		{table: "CMinMax", columns: constraintTestColumns, check: true},
	} {
		var constraints []constraintRecord
		if step.check {
			constraints = constraintsOf(t, fixture, step.table)
		}

		if err := db.createTable(step.table, step.columns, constraints); err != nil {
			t.Fatalf("createTable(%q): %v", step.table, err)
		}

		if !step.check {
			continue
		}

		t.Run(step.table, func(t *testing.T) {
			want := schemaStreamOf(t, fixture, step.table)
			got := schemaStreamOf(t, db, step.table)

			// systemIndexRootsSize is the root/blobroot pair; everything before
			// it is the columns, the index array and the constraint array.
			if len(got) != len(want) {
				t.Fatalf("%s: schema stream is %d bytes, want %d", step.table, len(got), len(want))
			}

			end := len(want) - systemIndexRootsSize
			if !bytes.Equal(got[:end], want[:end]) {
				t.Errorf("%s schema stream:\n got % x\nwant % x", step.table, got[:end], want[:end])
			}
		})
	}
}

// TestCompactDatabaseRebuildsColumnConstraints runs the whole operation:
// a database carrying a NOT NULL and a MINVALUE/MAXVALUE pair used to refuse
// compaction outright with ErrConstraintsNotRebuilt, and now compacts with the
// records intact.
//
// The source is built here rather than taken from Constraints.abs because
// whole-file compaction stops at the first table it cannot rebuild, and that
// file's third table is CPk. What the source is does not weaken the test: the
// records it carries are the engine's, read out of Constraints.abs and written
// through the path TestCreateTableWritesTheEngineSchemaStream holds to the
// engine's bytes.
func TestCompactDatabaseRebuildsColumnConstraints(t *testing.T) {
	src := constrainedSourceDatabase(t)
	dst := newDatabasePath(t, "compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	before := openTestFileAt(t, src)
	after := openTestFileAt(t, dst)

	for _, table := range []string{"CNotNull", "CMinMax"} {
		t.Run(table, func(t *testing.T) {
			want := constraintsOf(t, before, table)
			got := constraintsOf(t, after, table)

			if len(got) != len(want) {
				t.Fatalf("%s carries %d constraint records after compaction, want %d",
					table, len(got), len(want))
			}

			for i := range want {
				// The ids are the one thing that may differ: a compaction
				// reallocates every object id in the file. The record's name,
				// the column it covers and its bounds must not.
				w, g := want[i], got[i]
				w.objectID, g.objectID, w.ownerID, g.ownerID = 0, 0, 0, 0

				if summary(g) != summary(w) {
					t.Errorf("%s constraint %d:\n got %s\nwant %s", table, i, summary(g), summary(w))
				}
			}
		})
	}

	// Nullability is read out of the constraint array, so this is the same
	// finding from the API side: a record written where the reader does not
	// look would leave the column reported as nullable.
	schema := schemaOfTable(t, after, "CNotNull")
	if notNull, known := schema.Columns[0].NotNull(); !known || !notNull {
		t.Errorf("CNotNull.A after compaction: NotNull = %t, known = %t, want true/true", notNull, known)
	}
}

// TestCompactedConstraintsAreEnforced closes the loop: a rebuilt table's
// records have to be the working kind, not decoration. The copy is opened for
// writing and asked to break each clause, which runs the same resolver a write
// against the original would.
func TestCompactedConstraintsAreEnforced(t *testing.T) {
	dst := newDatabasePath(t, "compacted-enforced.abs")

	if err := CompactDatabase(constrainedSourceDatabase(t), dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	db, err := OpenForWrite(dst)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	for _, c := range []struct {
		name   string
		table  string
		values []any
		want   error
	}{
		{"NOT NULL still rejects a NULL", "CNotNull", []any{nil, "x"}, ErrNotNullViolated},
		{"NOT NULL still admits a value", "CNotNull", []any{int32(1), "x"}, nil},
		{"MAXVALUE still rejects one above", "CMinMax", []any{int32(100), "x"}, ErrCheckViolated},
		{"MAXVALUE still admits the bound", "CMinMax", []any{int32(99), "x"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			tbl, err := db.Table(c.table)
			if err != nil {
				t.Fatalf("Table(%q): %v", c.table, err)
			}

			w, err := tbl.OpenWriter()
			if err != nil {
				t.Fatalf("OpenWriter(%q): %v", c.table, err)
			}

			defer w.Close()

			_, err = w.Insert(c.values)
			requireConstraintOutcome(t, err, c.want)
		})
	}
}

// constrainedSourceDatabase builds a two-table database carrying the constraint
// records DBManager wrote into Constraints.abs, with a row in each table so the
// compaction has something to copy as well as something to declare.
func constrainedSourceDatabase(t *testing.T) string {
	t.Helper()

	fixture := openFixture(t, constraintsFixture)
	path := newDatabasePath(t, "constrained.abs")

	db, err := CreateDatabase(path, CreateDatabaseOptions{})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	for _, table := range []string{"CNotNull", "CMinMax"} {
		if err := db.createTable(table, constraintTestColumns, constraintsOf(t, fixture, table)); err != nil {
			t.Fatalf("createTable(%q): %v", table, err)
		}

		insertOneRow(t, db, table, []any{int32(5), "five"})
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return path
}

// insertOneRow inserts and commits a single row, failing the test on anything
// that goes wrong -- a helper for building a source database, not for testing
// the writer.
func insertOneRow(t *testing.T, db *File, table string, values []any) {
	t.Helper()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	w, err := tbl.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter(%q): %v", table, err)
	}

	defer w.Close()

	if _, err := w.Insert(values); err != nil {
		t.Fatalf("Insert into %q: %v", table, err)
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit on %q: %v", table, err)
	}
}

// fillerColumns returns n plain integer columns, for a stand-in table whose
// only job is to consume object ids.
func fillerColumns(n int) []Column {
	columns := make([]Column, n)
	for i := range columns {
		columns[i] = Column{Name: string(rune('A' + i)), BaseType: BftInt32, FieldType: FieldInteger}
	}

	return columns
}
