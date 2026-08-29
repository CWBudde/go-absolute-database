package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// constraintsFixture is the database built for decoding the constraint record:
// twelve two-column tables, each one CREATE TABLE clause away from the control.
// See ddl_constraint.go for what each of them isolates.
const constraintsFixture = "Constraints.abs"

// TestConstraintRecordsDecodeTheConstraintsFixture pins the decoded content of
// every table in Constraints.abs, which is the whole evidence base for the
// layout ddl_constraint.go documents. If a change to the parser makes this
// test disagree, the parser is wrong -- these values were read off the bytes
// one clause at a time.
func TestConstraintRecordsDecodeTheConstraintsFixture(t *testing.T) {
	type wantIndex struct {
		name    string
		unique  bool
		primary bool
		columns []indexColumn
	}

	type wantConstraint struct {
		kind    constraintKind
		name    string
		table   string
		index   string
		columns []string
		minMax  string
	}

	asc := func(name string) indexColumn {
		return indexColumn{name: name, maxIndexedSize: indexColumnMaxIndexedSize}
	}

	tests := []struct {
		table       string
		indexes     []wantIndex
		constraints []wantConstraint
	}{
		{table: "CNone"},
		{
			table: "CNotNull",
			constraints: []wantConstraint{{
				kind: constraintNotNull, name: "$C_NotNull$CNotNull$A",
				table: "CNotNull", columns: []string{"A"},
			}},
		},
		{
			table:   "CPk",
			indexes: []wantIndex{{name: "C_PK$A", primary: true, columns: []indexColumn{asc("A")}}},
			constraints: []wantConstraint{{
				kind: constraintPrimaryKey, name: "C_PK$A",
				table: "CPk", index: "C_PK$A", columns: []string{"A"},
			}},
		},
		{
			table:   "CUnique",
			indexes: []wantIndex{{name: "C_Unique$A", unique: true, columns: []indexColumn{asc("A")}}},
			constraints: []wantConstraint{{
				kind: constraintUnique, name: "C_Unique$A",
				table: "CUnique", index: "C_Unique$A", columns: []string{"A"},
			}},
		},
		// CDefault carries no constraint record at all: DEFAULT lives in the
		// column definition, which is why findColumnTerminator had to learn to
		// read it before this table could be opened at all.
		{table: "CDefault"},
		{
			table: "CMinMax",
			constraints: []wantConstraint{{
				kind: constraintCheck, name: "$C_Check$CMinMax$A",
				table: "CMinMax", columns: []string{"A"},
				minMax: "min=00000000 max=63000000",
			}},
		},
		{
			table:   "CBoth",
			indexes: []wantIndex{{name: "C_Unique$B", unique: true, columns: []indexColumn{asc("B")}}},
			constraints: []wantConstraint{
				{
					kind: constraintNotNull, name: "$C_NotNull$CBoth$A",
					table: "CBoth", columns: []string{"A"},
				},
				{
					kind: constraintUnique, name: "C_Unique$B",
					table: "CBoth", index: "C_Unique$B", columns: []string{"B"},
				},
			},
		},
		{
			table:   "CPkMulti",
			indexes: []wantIndex{{name: "C_PK$A$B", primary: true, columns: []indexColumn{asc("A"), asc("B")}}},
			constraints: []wantConstraint{{
				kind: constraintPrimaryKey, name: "C_PK$A$B",
				table: "CPkMulti", index: "C_PK$A$B", columns: []string{"A", "B"},
			}},
		},
		{
			table:   "CIdxOne",
			indexes: []wantIndex{{name: "IdxOne", columns: []indexColumn{asc("A")}}},
		},
		{
			table: "CIdxDesc",
			indexes: []wantIndex{{name: "IdxDesc", columns: []indexColumn{
				{name: "A", descending: true, maxIndexedSize: indexColumnMaxIndexedSize},
			}}},
		},
		{
			table:   "CIdxMulti",
			indexes: []wantIndex{{name: "IdxMulti", columns: []indexColumn{asc("A"), asc("B")}}},
		},
		{
			table: "CIdxNoCase",
			indexes: []wantIndex{{name: "IdxNoCase", columns: []indexColumn{
				{name: "B", caseInsensitive: true, maxIndexedSize: indexColumnMaxIndexedSize},
			}}},
		},
	}

	db := openFixture(t, constraintsFixture)

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			_, _, records, constraints := tailOf(t, db, tt.table)

			if len(records) != len(tt.indexes) {
				t.Fatalf("index records: got %d, want %d", len(records), len(tt.indexes))
			}

			for i, want := range tt.indexes {
				got := records[i]
				if got.name != want.name || got.unique != want.unique || got.primary != want.primary {
					t.Errorf("index %d: got %q unique=%t primary=%t, want %q unique=%t primary=%t",
						i, got.name, got.unique, got.primary, want.name, want.unique, want.primary)
				}

				if len(got.columns) != len(want.columns) {
					t.Fatalf("index %d covers %d columns, want %d", i, len(got.columns), len(want.columns))
				}

				for j, wantCol := range want.columns {
					if got.columns[j] != wantCol {
						t.Errorf("index %d column %d: got %+v, want %+v", i, j, got.columns[j], wantCol)
					}
				}
			}

			if len(constraints) != len(tt.constraints) {
				t.Fatalf("constraint records: got %d, want %d", len(constraints), len(tt.constraints))
			}

			for i, want := range tt.constraints {
				checkConstraint(t, i, constraints[i], want.kind, want.name, want.table, want.index, want.columns, want.minMax)
			}
		})
	}
}

// checkConstraint compares one parsed record against what the fixture's DDL
// says it should hold.
func checkConstraint(t *testing.T, i int, got constraintRecord,
	kind constraintKind, name, table, index string, columns []string, minMax string,
) {
	t.Helper()

	if got.kind != kind || got.name != name || got.table != table || got.index != index {
		t.Errorf("constraint %d: got kind=%s name=%q table=%q index=%q, want kind=%s name=%q table=%q index=%q",
			i, got.kind, got.name, got.table, got.index, kind, name, table, index)
	}

	gotColumns := make([]string, 0, len(got.columns))
	for _, c := range got.columns {
		gotColumns = append(gotColumns, c.name)
	}

	if strings.Join(gotColumns, ",") != strings.Join(columns, ",") {
		t.Errorf("constraint %d columns: got %v, want %v", i, gotColumns, columns)
	}

	if minMax == "" {
		return
	}

	if bounds := fmt.Sprintf("min=%x max=%x", got.minValue.data, got.maxValue.data); bounds != minMax {
		t.Errorf("constraint %d bounds: got %s, want %s", i, bounds, minMax)
	}
}

// TestSchemaTailParsesEveryFixture is the measure of the whole exercise: every
// table in every fixture present must have a schema tail this package can read
// end to end.
//
// Before the constraint array was decoded, twenty private-fixture tables failed here:
// the thirteen databases Addresses, RCON0011, RCFQ0011, RMPA0011, RFRQ0011,
// RGRP0011, RMND0011, RPDG0011, RR240011, RRAD00*, RRAI00*, RREC0011 and TS03,
// three of them also present as encrypted copies of Addresses. Some failed on
// a constraint record, some on a multi-column index, and every one of them took
// CREATE INDEX, DROP INDEX, DROP COLUMN and index maintenance down with it.
// Constraints.abs itself contributed eight more, one of which (CDefault) could
// not even be opened.
//
// The cross-checks matter as much as the parse succeeding. Requiring every
// name an index or constraint record mentions to resolve to a real column of
// the table is what would catch a parse that landed on the trailer by luck
// rather than by reading the right fields.
func TestSchemaTailParsesEveryFixture(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			for _, info := range tables {
				checkFixtureTable(t, db, info.Name)
			}
		})
	}
}

// checkFixtureTable parses one table's schema tail and cross-checks it against
// the table's own columns and against the raw bytes it came from.
func checkFixtureTable(t *testing.T, db *File, name string) {
	t.Helper()

	table, err := db.Table(name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}

	schema, err := table.Schema()
	if err != nil {
		t.Fatalf("%s: Schema: %v", name, err)
	}

	raw, _, records, constraints := tailOf(t, db, name)

	for _, rec := range records {
		for _, col := range rec.columns {
			if _, err := findColumnIndex(schema, col.name); err != nil {
				t.Errorf("%s: index %q covers %q, which is not a column of the table", name, rec.name, col.name)
			}
		}
	}

	for _, c := range constraints {
		if !strings.EqualFold(c.table, name) {
			t.Errorf("%s: constraint %q names table %q", name, c.name, c.table)
		}

		for _, col := range c.columns {
			if _, err := findColumnIndex(schema, col.name); err != nil {
				t.Errorf("%s: constraint %q covers %q, which is not a column of the table", name, c.name, col.name)
			}
		}
	}

	// Reading the tail must not be able to change what the file means, so the
	// spans the parse reports have to add back up to the bytes it read.
	for _, rec := range records {
		if rec.start >= rec.end || rec.end > len(raw) {
			t.Errorf("%s: index record %q has span [%d,%d) in a %d-byte stream", name, rec.name, rec.start, rec.end, len(raw))
		}
	}

	for _, c := range constraints {
		if c.start >= c.end || c.end > len(raw) {
			t.Errorf("%s: constraint %q has span [%d,%d) in a %d-byte stream", name, c.name, c.start, c.end, len(raw))
		}
	}

	rows, err := table.Open()
	if err != nil {
		t.Fatalf("%s: Open: %v", name, err)
	}

	for rows.Next() { //revive:disable-line:empty-block // draining the reader is the check
	}

	if err := rows.Err(); err != nil {
		t.Errorf("%s: reading rows: %v", name, err)
	}
}

// tailOf reads and parses one table's schema stream, failing the test on any
// error. It returns the raw stream alongside the parse so a test can compare
// bytes as well as fields.
func tailOf(t *testing.T, db *File, name string) ([]byte, int, []indexRecord, []constraintRecord) {
	t.Helper()

	table, err := db.Table(name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}

	pageNo, err := table.schemaPageNo()
	if err != nil {
		t.Fatalf("%s: schemaPageNo: %v", name, err)
	}

	raw, err := db.readSchemaStream(pageNo)
	if err != nil {
		t.Fatalf("%s: readSchemaStream: %v", name, err)
	}

	_, _, records, constraints, tailStart, err := parseSchemaTail(raw)
	if err != nil {
		t.Fatalf("%s: parseSchemaTail: %v", name, err)
	}

	return raw, tailStart, records, constraints
}

// TestCreateIndexPreservesConstraintRecords is the safety property the whole
// change hangs on: a constrained table's constraint array must come back byte
// for byte across a schema-stream rewrite. CREATE INDEX splices a record into
// the index array, which sits immediately before it, so an off-by-one in the
// splice would show up here and nowhere else.
func TestCreateIndexPreservesConstraintRecords(t *testing.T) {
	path := writableCopy(t, constraintsFixture)

	before := constraintBytes(t, path, "CNotNull")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.CreateIndex("CNotNull", "IdxA", "A"); err != nil {
		t.Fatalf("CreateIndex on a table with a NOT NULL constraint: %v", err)
	}

	db.Close()

	after := constraintBytes(t, path, "CNotNull")
	if !bytes.Equal(before, after) {
		t.Errorf("constraint array changed across CreateIndex:\n before %x\n after  %x", before, after)
	}

	// The new index must be there and readable, and the constraint must still
	// decode -- byte equality alone would also be satisfied by a stream that no
	// longer parses.
	db = openTestFileAt(t, path)

	_, _, records, constraints := tailOf(t, db, "CNotNull")

	if len(records) != 1 || records[0].name != "IdxA" {
		t.Errorf("index records after CreateIndex: %+v", records)
	}

	if len(constraints) != 1 || constraints[0].kind != constraintNotNull {
		t.Errorf("constraint records after CreateIndex: %+v", constraints)
	}

	if err := db.DropIndex("CNotNull", "IdxA"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DropIndex on a read-only handle: %v, want ErrReadOnly", err)
	}
}

// TestDropIndexPreservesConstraintRecords is CreateIndex's mirror: removing a
// plain index from a constrained table must leave the constraint array alone.
func TestDropIndexPreservesConstraintRecords(t *testing.T) {
	path := writableCopy(t, constraintsFixture)

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.CreateIndex("CBoth", "IdxA", "A"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	db.Close()

	before := constraintBytes(t, path, "CBoth")

	db, err = OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.DropIndex("CBoth", "IdxA"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}

	db.Close()

	if after := constraintBytes(t, path, "CBoth"); !bytes.Equal(before, after) {
		t.Errorf("constraint array changed across DropIndex:\n before %x\n after  %x", before, after)
	}
}

// constraintBytes returns the raw bytes of a table's constraint array plus the
// trailer that follows it -- everything from the constraint count on.
func constraintBytes(t *testing.T, path, table string) []byte {
	t.Helper()

	db := openTestFileAt(t, path)

	raw, tailStart, _, _ := tailOf(t, db, table)

	return append([]byte(nil), raw[tailStart:]...)
}

// openTestFileAt opens a database by path (rather than by fixture name) and
// closes it when the test ends.
func openTestFileAt(t *testing.T, path string) *File {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// TestDropIndexRefusesAConstraintIndex covers the one refusal decoding the
// constraint array added rather than removed. The index behind a PRIMARY KEY or
// UNIQUE clause is not the user's to drop: DBManager drops the constraint and
// the index together, and nothing in the corpus says what a file with a
// constraint naming a missing index means.
func TestDropIndexRefusesAConstraintIndex(t *testing.T) {
	db, err := OpenForWrite(writableCopy(t, constraintsFixture))
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	for _, tt := range []struct{ table, index string }{
		{"CPk", "C_PK$A"},
		{"CUnique", "C_Unique$A"},
		{"CPkMulti", "C_PK$A$B"},
	} {
		if err := db.DropIndex(tt.table, tt.index); !errors.Is(err, ErrIndexBacksConstraint) {
			t.Errorf("DropIndex(%q, %q): %v, want ErrIndexBacksConstraint", tt.table, tt.index, err)
		}
	}
}

// TestDropColumnAsksAboutTheColumnItIsDropping is the precision the decode
// bought. A constraint or a multi-column index blocks the drop of a column it
// covers -- and, now that the records are read rather than text-scanned, only
// of a column it covers.
func TestDropColumnAsksAboutTheColumnItIsDropping(t *testing.T) {
	tests := []struct {
		name   string
		table  string
		column string
		want   error
	}{
		{"NOT NULL column", "CNotNull", "A", ErrColumnConstrained},
		{"CHECK column", "CMinMax", "A", ErrColumnConstrained},
		// A PRIMARY KEY is always backed by an index, so its columns are
		// caught by the index check first -- which is the same refusal, one
		// step earlier.
		{"second column of a multi-column key", "CPkMulti", "B", ErrColumnIndexed},
		{"second column of a multi-column index", "CIdxMulti", "B", ErrColumnIndexed},
		// The column no record mentions. Before the records were parsed the
		// text scan flagged "B" on any table whose tail happened to contain
		// the letter, and a table's unconstrained columns could not be
		// dropped either.
		{"unconstrained column of a constrained table", "CNotNull", "B", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := OpenForWrite(writableCopy(t, constraintsFixture))
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			defer db.Close()

			err = db.DropColumn(tt.table, tt.column)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("DropColumn(%q, %q): %v, want success", tt.table, tt.column, err)
				}

				schema, err := mustTable(t, db, tt.table).Schema()
				if err != nil {
					t.Fatalf("Schema after DropColumn: %v", err)
				}

				if _, err := findColumnIndex(schema, tt.column); err == nil {
					t.Errorf("column %q survived DropColumn", tt.column)
				}

				return
			}

			if !errors.Is(err, tt.want) {
				t.Errorf("DropColumn(%q, %q): %v, want %v", tt.table, tt.column, err, tt.want)
			}
		})
	}
}

// mustTable resolves a table by name, failing the test if it is missing.
func mustTable(t *testing.T, db *File, name string) *Table {
	t.Helper()

	table, err := db.Table(name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}

	return table
}

// TestSchemaTailRefusalsSurvive checks that decoding the array did not turn
// "refuse rather than guess" into "parse anything". Each case corrupts one
// field of a real constraint record in a way the corpus never shows.
func TestSchemaTailRefusalsSurvive(t *testing.T) {
	db := openFixture(t, constraintsFixture)

	raw, tailStart, _, constraints := tailOf(t, db, "CNotNull")

	if len(constraints) != 1 {
		t.Fatalf("CNotNull has %d constraint records, want 1", len(constraints))
	}

	rec := constraints[0]

	tests := []struct {
		name    string
		corrupt func(b []byte) []byte
	}{
		{"an unknown constraint kind", func(b []byte) []byte {
			b[rec.start] = 1

			return b
		}},
		{"a non-zero reserved field", func(b []byte) []byte {
			b[rec.start+1+1+len(rec.name)+4] = 1

			return b
		}},
		{"a count the record array cannot hold", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[tailStart:tailStart+4], 99)

			return b
		}},
		{"a trailing byte too many", func(b []byte) []byte {
			return append(b, 0)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broken := tt.corrupt(append([]byte(nil), raw...))

			_, _, _, _, _, err := parseSchemaTail(broken)
			if !errors.Is(err, ErrSchemaTailNotUnderstood) && !errors.Is(err, ErrBadSchema) {
				t.Errorf("parseSchemaTail on %s: %v, want a refusal", tt.name, err)
			}
		})
	}
}

// TestCreateIndexOnConstrainedPrivateFixtureTables is the payoff, measured on the
// files this package exists to read. Each of these carries NOT NULL records and
// a PRIMARY KEY, and every one of them refused CREATE INDEX outright until the
// constraint array was decoded.
//
// Three things are checked per fixture, and the third is the one that matters:
// the constraint array survives byte for byte, the rows read back unchanged,
// and the index the operation built agrees with the record reader row for row
// (crossCheckTable, the same oracle TestOracleReaderMatchesLeafScan applies to
// the fixtures as shipped).
func TestCreateIndexOnConstrainedPrivateFixtureTables(t *testing.T) {
	tests := []struct{ fixture, column string }{
		{"RRAI0011.abs", "ObjID"},
		{"RMND0011.abs", "SrcNo"},
		{"RGRP0011.abs", "RecNo"},
		{"RPDG0011.abs", "RecNo"},
		{"RRAD0011.abs", "IDX"},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			path := writableCopy(t, requireFixtureName(t, tt.fixture))
			table := tt.fixture

			before := constraintBytes(t, path, table)
			beforeRows := rowDigest(t, path, table)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			if err := db.CreateIndex(table, "IdxScratch", tt.column); err != nil {
				t.Fatalf("CreateIndex(%q, %q): %v", table, tt.column, err)
			}

			db.Close()

			if after := constraintBytes(t, path, table); !bytes.Equal(before, after) {
				t.Errorf("constraint array changed:\n before %x\n after  %x", before, after)
			}

			if after := rowDigest(t, path, table); after != beforeRows {
				t.Errorf("rows changed across CreateIndex: %q -> %q", beforeRows, after)
			}

			reopened := openTestFileAt(t, path)

			if crossCheckTable(t, mustTable(t, reopened, table)) == 0 {
				t.Error("no user index was cross-checked against the rows")
			}
		})
	}
}

// rowDigest renders every field of every row of a table, so a test can assert
// that a schema-stream edit left the data alone.
func rowDigest(t *testing.T, path, table string) string {
	t.Helper()

	db := openTestFileAt(t, path)

	reader, err := mustTable(t, db, table).Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var out strings.Builder

	for reader.Next() {
		record := reader.Record()
		for i := range reader.Schema().Columns {
			fmt.Fprintf(&out, "%q|", record.String(i))
		}

		out.WriteByte('\n')
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("reading rows: %v", err)
	}

	return out.String()
}
