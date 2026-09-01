package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
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
		{table: "CPk", columns: constraintTestColumns, check: true},
		{table: "CUnique", columns: constraintTestColumns, check: true},
		{table: "FillerForCDefault", columns: fillerColumns(2)},
		{table: "CMinMax", columns: constraintTestColumns, check: true},
		// CBoth consumes six ids (table, two columns, one index and two
		// constraints). A five-column filler consumes the same six without
		// asking this test to build its still-unsupported string UNIQUE key.
		{table: "FillerForCBoth", columns: fillerColumns(5), check: false},
		{table: "CPkMulti", columns: constraintTestColumns, check: true},
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
			requireStreamsEqual(t, step.table, got[:end], want[:end], indexRootOffsets(t, db, step.table))

			if step.table == "CPkMulti" {
				requireEmptyIndexRootEqual(t, fixture, db, step.table)
			}
		})
	}
}

// requireEmptyIndexRootEqual compares the whole B-tree payload of the one key
// index on table. Unlike a populated leaf, this shape is fully established by
// Constraints.abs: the compound KeyPrefixSize is 17 and every remaining byte
// is the same zero/flag/sentinel byte CreateTable writes.
func requireEmptyIndexRootEqual(t *testing.T, wantDB, gotDB *File, table string) {
	t.Helper()

	wantRecords := indexRecordsOf(t, wantDB, table)

	gotRecords := indexRecordsOf(t, gotDB, table)
	if len(wantRecords) != 1 || len(gotRecords) != 1 {
		t.Fatalf("%s index records: got %d, want %d", table, len(gotRecords), len(wantRecords))
	}

	want, err := wantDB.ReadPage(int(wantRecords[0].rootPageNo))
	if err != nil {
		t.Fatalf("read engine index root: %v", err)
	}

	got, err := gotDB.ReadPage(int(gotRecords[0].rootPageNo))
	if err != nil {
		t.Fatalf("read rebuilt index root: %v", err)
	}

	if !bytes.Equal(got.PageData(), want.PageData()) {
		t.Errorf("%s empty compound index root differs from the engine's", table)
	}
}

// indexRootOffsets is the byte range of every index record's rootPageNo in a
// table's schema stream, and the one thing the replay cannot reproduce: the
// page an index lands on depends on where in the file its table was created,
// not on what the table declares. The offset is computed from the record's own
// parsed span rather than searched for, so a record whose layout moved would
// exclude the wrong bytes and fail loudly instead of hiding a difference.
//
// It doubles as a check that the number written is the page actually
// allocated, which is what the exclusion would otherwise stop anyone noticing.
func indexRootOffsets(t *testing.T, db *File, table string) map[int]bool {
	t.Helper()

	raw, _, records, _ := tailOf(t, db, table)
	excluded := map[int]bool{}

	for _, rec := range records {
		name, err := encodePascalName(rec.name)
		if err != nil {
			t.Fatalf("%s: index %q: %v", table, rec.name, err)
		}

		// name byte + name + objectID + three flag bytes + coveredColumnCount.
		start := rec.start + 1 + len(name) + 4 + indexRecordFlagsSize + 4

		if got := int32(binary.LittleEndian.Uint32(raw[start : start+4])); got != rec.rootPageNo {
			t.Fatalf("%s: index %q root page reads %d at offset %d, parsed as %d",
				table, rec.name, got, start, rec.rootPageNo)
		}

		page, err := db.ReadPage(int(rec.rootPageNo))
		if err != nil || page.Header == nil || int(page.Header.PageType) != PageTypeIndex {
			t.Errorf("%s: index %q is rooted at page %d, which is not an index page (%v)",
				table, rec.name, rec.rootPageNo, err)
		}

		for i := range 4 {
			excluded[start+i] = true
		}
	}

	return excluded
}

// maxReportedStreamDifferences caps how many differing bytes are named before
// the count alone is reported, the same courtesy reportByteDifferences does
// for a whole file.
const maxReportedStreamDifferences = 8

// requireStreamsEqual compares two schema streams byte for byte, skipping the
// offsets a replay cannot reproduce and naming the first few that differ.
func requireStreamsEqual(t *testing.T, table string, got, want []byte, excluded map[int]bool) {
	t.Helper()

	differing := 0

	for i := range want {
		if excluded[i] || got[i] == want[i] {
			continue
		}

		differing++

		if differing <= maxReportedStreamDifferences {
			t.Errorf("%s schema stream: byte %d: wrote %#02x, the engine wrote %#02x",
				table, i, got[i], want[i])
		}
	}

	if differing > maxReportedStreamDifferences {
		t.Errorf("%s schema stream: %d bytes differ in all", table, differing)
	}
}

// TestCompactDatabaseRebuildsConstraints runs the whole operation: a database
// carrying a NOT NULL, a MINVALUE/MAXVALUE pair, a PRIMARY KEY and a UNIQUE
// used to refuse compaction outright with ErrConstraintsNotRebuilt, and now
// compacts with every record intact -- the key ones together with the index
// each is built on, which the rebuild creates rather than copies.
//
// The source is built here rather than taken from Constraints.abs because
// whole-file compaction stops at the first table it cannot rebuild, and that
// file's fifth table is CDefault. What the source is does not weaken the test: the
// records it carries are the engine's, read out of Constraints.abs and written
// through the path TestCreateTableWritesTheEngineSchemaStream holds to the
// engine's bytes.
func TestCompactDatabaseRebuildsConstraints(t *testing.T) {
	src := constrainedSourceDatabase(t)
	dst := newDatabasePath(t, "compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	before := openTestFileAt(t, src)
	after := openTestFileAt(t, dst)

	for _, table := range []string{"CNotNull", "CMinMax", "CPk", "CUnique"} {
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

// TestCompactDatabaseRebuildsEmptyCompoundShapes covers the only compound
// compaction the committed roots can prove: both a key-backed index and a plain
// index on rowless tables. Populated compound tables remain a planning error,
// before a destination is created.
func TestCompactDatabaseRebuildsEmptyCompoundShapes(t *testing.T) {
	fixture := openFixture(t, constraintsFixture)
	src := newDatabasePath(t, "compound-empty.abs")

	db, err := CreateDatabase(src, CreateDatabaseOptions{
		PageSize:          0,
		PageCountInExtent: 0,
		MaxConnections:    0,
		Encrypted:         false,
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	if err := db.createTable(
		"CPkMulti", constraintTestColumns, constraintsOf(t, fixture, "CPkMulti"),
	); err != nil {
		t.Fatalf("createTable(CPkMulti): %v", err)
	}

	if err := db.CreateTable("CIdxMulti", constraintTestColumns); err != nil {
		t.Fatalf("CreateTable(CIdxMulti): %v", err)
	}

	if err := db.CreateIndex("CIdxMulti", "IdxMulti", "A", "B"); err != nil {
		t.Fatalf("CreateIndex(CIdxMulti): %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close source: %v", err)
	}

	dst := newDatabasePath(t, "compound-empty-compacted.abs")
	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	after := openTestFileAt(t, dst)
	for _, table := range []string{"CPkMulti", "CIdxMulti"} {
		t.Run(table, func(t *testing.T) {
			requireEmptyIndexRootEqual(t, fixture, after, table)

			got := indexRecordsOf(t, after, table)
			if len(got) != 1 || len(got[0].columns) != 2 ||
				got[0].columns[0].name != "A" || got[0].columns[1].name != "B" {
				t.Errorf("rebuilt compound index = %+v", got)
			}
		})
	}

	wantConstraints := constraintsOf(t, fixture, "CPkMulti")

	gotConstraints := constraintsOf(t, after, "CPkMulti")
	if len(gotConstraints) != 1 || len(wantConstraints) != 1 {
		t.Fatalf("CPkMulti constraints after compaction = %d, want %d",
			len(gotConstraints), len(wantConstraints))
	}

	want, got := wantConstraints[0], gotConstraints[0]

	want.objectID, got.objectID, want.ownerID, got.ownerID = 0, 0, 0, 0
	for i := range want.columns {
		want.columns[i].objectID, got.columns[i].objectID = 0, 0
	}

	if summary(got) != summary(want) {
		t.Errorf("CPkMulti constraint after compaction:\n got %s\nwant %s", summary(got), summary(want))
	}
}

// TestCompactDatabaseRebuildsPopulatedCompoundIndex proves the replay path can
// create the empty ten-byte root first and then maintain it while rows are
// copied. The source is the official-engine MultiKeys fixture.
func TestCompactDatabaseRebuildsPopulatedCompoundIndex(t *testing.T) {
	src := requireFixture(t, "MultiKeys.abs")
	dst := newDatabasePath(t, "multikeys-compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	compareRows(t, dst, src, "MultiKeys", "COMPACT DATABASE with a populated compound index")

	got, want := captureIndexes(t, dst, "MultiKeys"), captureIndexes(t, src, "MultiKeys")
	if len(got) != 1 || len(want) != 1 || got[0].name != want[0].name || len(got[0].entries) != len(want[0].entries) {
		t.Fatalf("compacted indexes = %+v, source indexes = %+v", got, want)
	}

	for i := range got[0].entries {
		if !bytes.Equal(got[0].entries[i].Key, want[0].entries[i].Key) {
			t.Errorf("compacted key %d = %x, want %x", i, got[0].entries[i].Key, want[0].entries[i].Key)
		}
	}

	after := openTestFileAt(t, dst)

	table, err := after.Table("MultiKeys")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	if checked := crossCheckTable(t, table); checked != 1 {
		t.Errorf("cross-checked %d indexes, want 1", checked)
	}
}

// TestCompoundPrimaryKeySurvivesCreateAndCompact covers the key-backed path:
// CREATE TABLE builds a two-column primary index, inserts maintain it, and a
// compaction preserves both tuple uniqueness and PRIMARY's no-NULL rule.
func TestCompoundPrimaryKeySurvivesCreateAndCompact(t *testing.T) {
	src := newDatabasePath(t, "compound-primary.abs")

	db, err := CreateDatabase(src, CreateDatabaseOptions{})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	columns := []Column{
		{Name: "A", BaseType: BftInt32, FieldType: FieldInteger},
		{Name: "B", BaseType: BftInt32, FieldType: FieldInteger},
	}

	constraint := constraintRecord{
		kind: constraintPrimaryKey, name: "C_PK$A$B", table: "Pairs", index: "C_PK$A$B",
		columns: []constraintColumn{{name: "A"}, {name: "B"}},
	}
	if err := db.createTable("Pairs", columns, []constraintRecord{constraint}); err != nil {
		t.Fatalf("createTable: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	for _, row := range [][]any{{int32(1), int32(10)}, {int32(1), int32(20)}, {int32(2), int32(10)}} {
		if _, err := w.Insert(row); err != nil {
			t.Fatalf("Insert(%v): %v", row, err)
		}
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close source: %v", err)
	}

	dst := newDatabasePath(t, "compound-primary-compacted.abs")
	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	after, err := OpenForWrite(dst)
	if err != nil {
		t.Fatalf("OpenForWrite compacted database: %v", err)
	}
	defer after.Close()

	writer, err := after.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter compacted database: %v", err)
	}

	if _, err := writer.Insert([]any{int32(1), int32(10)}); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("duplicate compound primary key = %v, want ErrDuplicateKey", err)
	}

	if _, err := writer.Insert([]any{int32(1), nil}); !errors.Is(err, ErrNotNullViolated) {
		t.Errorf("NULL compound primary component = %v, want ErrNotNullViolated", err)
	}

	if _, err := writer.Insert([]any{int32(2), int32(20)}); err != nil {
		t.Errorf("distinct compound primary key: %v", err)
	}

	writer.Rollback()
}

// TestCompoundUniqueChecksTheWholeTuple ensures equality is over every
// component: repeating A is allowed until B repeats as well.
func TestCompoundUniqueChecksTheWholeTuple(t *testing.T) {
	path := newDatabasePath(t, "compound-unique.abs")

	db, err := CreateDatabase(path, CreateDatabaseOptions{})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	defer db.Close()

	columns := []Column{
		{Name: "A", BaseType: BftInt32, FieldType: FieldInteger},
		{Name: "B", BaseType: BftInt32, FieldType: FieldInteger},
	}

	constraint := constraintRecord{
		kind: constraintUnique, name: "C_Unique$A$B", table: "Pairs", index: "U_AB",
		columns: []constraintColumn{{name: "A"}, {name: "B"}},
	}
	if err := db.createTable("Pairs", columns, []constraintRecord{constraint}); err != nil {
		t.Fatalf("createTable: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}
	defer w.Rollback()

	for _, row := range [][]any{{int32(1), int32(10)}, {int32(1), int32(20)}} {
		if _, err := w.Insert(row); err != nil {
			t.Fatalf("Insert(%v): %v", row, err)
		}
	}

	if _, err := w.Insert([]any{int32(1), int32(10)}); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("duplicate tuple = %v, want ErrDuplicateKey", err)
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
		// The key constraints are the ones a rebuild used to refuse outright.
		// Their index has to come back enforcing, not merely present: the
		// source row has A = 5 in every table.
		{"PRIMARY KEY still rejects a duplicate", "CPk", []any{int32(5), "x"}, ErrDuplicateKey},
		{"PRIMARY KEY still rejects a NULL", "CPk", []any{nil, "x"}, ErrNotNullViolated},
		{"PRIMARY KEY still admits a new key", "CPk", []any{int32(6), "x"}, nil},
		{"UNIQUE still rejects a duplicate", "CUnique", []any{int32(5), "x"}, ErrDuplicateKey},
		{"UNIQUE still admits a NULL", "CUnique", []any{nil, "x"}, nil},
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

	for _, table := range []string{"CNotNull", "CMinMax", "CPk", "CUnique"} {
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
