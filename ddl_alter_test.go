package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// captureRows decodes every row of a table into comparable Go values, using
// the same decodeRowValues this package's own AddColumn/DropColumn rely on to
// re-encode a row under a changed schema. It is the value-preservation oracle
// every test in this file that asserts "the row did not change" is built on.
func captureRows(t *testing.T, path, table string) [][]any {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	reader, err := tbl.Open()
	if err != nil {
		t.Fatalf("Open table %q: %v", table, err)
	}

	var rows [][]any

	for reader.Next() {
		values, err := decodeRowValues(reader.Schema().Columns, reader.Record())
		if err != nil {
			t.Fatalf("decoding row: %v", err)
		}

		rows = append(rows, values)
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iterating %q: %v", table, err)
	}

	return rows
}

// captureSchemaRaw returns the decompressed bytes of a table's column-
// definition internal file, tail included. TestAddThenDropColumnRestoresTheFile
// uses it as a stronger check than comparing decoded Column values: adding and
// then dropping the same column should restore these bytes exactly, because
// DropColumn removes precisely the span AddColumn appended.
func captureSchemaRaw(t *testing.T, path, table string) []byte {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	pageNo, err := tbl.schemaPageNo()
	if err != nil {
		t.Fatalf("schemaPageNo(%q): %v", table, err)
	}

	data, err := db.readInternalFilePages(pageNo)
	if err != nil {
		t.Fatalf("readInternalFilePages(%d): %v", pageNo, err)
	}

	raw, err := decompressInternalFile(data)
	if err != nil {
		t.Fatalf("decompressInternalFile: %v", err)
	}

	return raw
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return info.Size()
}

// TestAddColumnPreservesEveryRow checks, on Writes.abs and on every table of
// MultiTable.abs, that AddColumn leaves every pre-existing row's original
// values unchanged and reads back the new column as NULL. Values are compared
// against what was actually stored before the change, not against constants,
// so a decoder that is self-consistently wrong on both sides could not pass by
// accident.
func TestAddColumnPreservesEveryRow(t *testing.T) {
	cases := []struct{ fixture, table string }{
		{"Writes.abs", ""},
		{"MultiTable.abs", "Alpha"},
		{"MultiTable.abs", "Beta"},
		{"MultiTable.abs", "Gamma"},
	}

	for _, c := range cases {
		t.Run(c.fixture+"/"+c.table, func(t *testing.T) {
			path := writableCopy(t, c.fixture)
			before := captureRows(t, path, c.table)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			newCol := Column{Name: "AddedCol", BaseType: BftInt32, FieldType: FieldInteger}
			if err := db.AddColumn(c.table, newCol); err != nil {
				t.Fatalf("AddColumn: %v", err)
			}

			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			after := captureRows(t, path, c.table)
			checkColumnAdded(t, before, after)
		})
	}
}

// checkColumnAdded asserts the shape TestAddColumnPreservesEveryRow and
// TestAlterTableColumnCountBoundary both need: same row count, every old
// value unchanged, and the new trailing column reading back NULL.
func checkColumnAdded(t *testing.T, before, after [][]any) {
	t.Helper()

	if len(after) != len(before) {
		t.Fatalf("row count changed: got %d, want %d", len(after), len(before))
	}

	for i := range before {
		if len(after[i]) != len(before[i])+1 {
			t.Fatalf("row %d: %d columns, want %d", i, len(after[i]), len(before[i])+1)
		}

		for c := range before[i] {
			if !reflect.DeepEqual(after[i][c], before[i][c]) {
				t.Errorf("row %d col %d: got %#v, want %#v", i, c, after[i][c], before[i][c])
			}
		}

		if last := after[i][len(before[i])]; last != nil {
			t.Errorf("row %d: new column is %#v, want NULL", i, last)
		}
	}
}

// TestDropColumnPreservesRemainingValues is TestAddColumnPreservesEveryRow's
// mirror: every remaining column keeps its original value, in every row, on
// every table tested.
func TestDropColumnPreservesRemainingValues(t *testing.T) {
	cases := []struct {
		fixture, table, drop string
	}{
		{"Writes.abs", "", "Salary"},
		{"MultiTable.abs", "Alpha", "Name"}, // Alpha's Id is indexed, Name is not
		{"MultiTable.abs", "Beta", "Amount"},
		{"MultiTable.abs", "Gamma", "V"},
	}

	for _, c := range cases {
		t.Run(c.fixture+"/"+c.table+"/"+c.drop, func(t *testing.T) {
			path := writableCopy(t, c.fixture)
			before := captureRows(t, path, c.table)

			beforeSchema := captureSchemaColumns(t, path, c.table)
			dropIdx := columnIndex(t, beforeSchema, c.drop)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			if err := db.DropColumn(c.table, c.drop); err != nil {
				t.Fatalf("DropColumn: %v", err)
			}

			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			after := captureRows(t, path, c.table)
			checkColumnDropped(t, before, after, dropIdx)
		})
	}
}

func checkColumnDropped(t *testing.T, before, after [][]any, dropIdx int) {
	t.Helper()

	if len(after) != len(before) {
		t.Fatalf("row count changed: got %d, want %d", len(after), len(before))
	}

	for i := range before {
		if len(after[i]) != len(before[i])-1 {
			t.Fatalf("row %d: %d columns, want %d", i, len(after[i]), len(before[i])-1)
		}

		wantCol := 0

		for c := range before[i] {
			if c == dropIdx {
				continue
			}

			if !reflect.DeepEqual(after[i][wantCol], before[i][c]) {
				t.Errorf("row %d col %d (was col %d): got %#v, want %#v", i, wantCol, c, after[i][wantCol], before[i][c])
			}

			wantCol++
		}
	}
}

func captureSchemaColumns(t *testing.T, path, table string) []Column {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	schema, err := tbl.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	return schema.Columns
}

func columnIndex(t *testing.T, cols []Column, name string) int {
	t.Helper()

	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}

	t.Fatalf("column %q not found among %v", name, cols)

	return -1
}

// TestAlterTableKeepsTheIndexOracleInStep runs the record decoder against the
// independent B-tree leaf scan (crossCheckTable, oracle_test.go) after each
// operation, on the one table in the committed corpus that carries a real
// index: MultiTable.abs's Alpha, indexed by IdxAlphaId on Id. Neither
// AddColumn nor DropColumn moves a record to a different (page, slot), which
// is exactly the invariant that keeps a leaf entry's reference valid across a
// schema change without this package ever touching the index page -- this
// test is what makes that claim more than an assertion in a comment.
func TestAlterTableKeepsTheIndexOracleInStep(t *testing.T) {
	t.Run("after AddColumn", func(t *testing.T) {
		path := writableCopy(t, "MultiTable.abs")

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		if err := db.AddColumn("Alpha", Column{Name: "Z", BaseType: BftInt32, FieldType: FieldInteger}); err != nil {
			t.Fatalf("AddColumn: %v", err)
		}

		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		checked := crossCheckReopened(t, path, "Alpha")
		if checked == 0 {
			t.Fatal("no user indexes found; nothing cross-checked")
		}
	})

	t.Run("after DropColumn", func(t *testing.T) {
		path := writableCopy(t, "MultiTable.abs")

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		// Name is not covered by IdxAlphaId (which covers Id), so this is
		// the one column Alpha can lose without ErrColumnIndexed.
		if err := db.DropColumn("Alpha", "Name"); err != nil {
			t.Fatalf("DropColumn: %v", err)
		}

		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		checked := crossCheckReopened(t, path, "Alpha")
		if checked == 0 {
			t.Fatal("no user indexes found; nothing cross-checked")
		}
	})
}

// crossCheckReopened reopens path and runs the oracle's own crossCheckTable
// (oracle_test.go) against the named table.
func crossCheckReopened(t *testing.T, path, table string) int {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	return crossCheckTable(t, tbl)
}

// TestAddThenDropColumnRestoresTheFile adds a column and then drops it again,
// and checks what returns to its original state and what does not.
//
// What IS asserted to be identical: the file's length, every table's column-
// definition bytes (schema tail included -- DropColumn removes exactly the
// span AddColumn appended, so the round trip is not merely schema-equivalent
// but byte-identical), and every row's decoded values on the altered table.
//
// What is NOT asserted, and why: the database header's State counter (offset
// 38) and LastObjectID (offset 376) are expected to differ -- two ALTER TABLE
// statements each bump the transaction counter, and AddColumn consumes one
// object id that DropColumn has no reason to give back. Every page this
// round trip touched also carries an advanced ABSP State word, the same way
// DropTable's own file comment documents for a dropped table's pages. And if
// growing the schema's internal file needed a page beyond the one it already
// had, DropColumn's own shrink leaves that page tombstoned (its ABSP State
// set to pageStateFree) rather than restored to the all-zero state a page
// that was never allocated carries -- again the same asymmetry DropTable
// documents for the pages a drop frees. None of that is a defect in the round
// trip; it is what "not crash-atomic, not byte-identical to a file the
// engine never wrote either" already commits this package to elsewhere.
func TestAddThenDropColumnRestoresTheFile(t *testing.T) {
	path := writableCopy(t, "MultiTable.abs")

	beforeSize := fileSize(t, path)
	beforeRows := captureRows(t, path, "Gamma")
	beforeSchema := captureSchemaRaw(t, path, "Gamma")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.AddColumn("Gamma", Column{Name: "W", BaseType: BftInt32, FieldType: FieldInteger}); err != nil {
		t.Fatalf("AddColumn: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close after AddColumn: %v", err)
	}

	db2, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db2.DropColumn("Gamma", "W"); err != nil {
		t.Fatalf("DropColumn: %v", err)
	}

	if err := db2.Close(); err != nil {
		t.Fatalf("Close after DropColumn: %v", err)
	}

	if got := fileSize(t, path); got != beforeSize {
		t.Errorf("file size changed: got %d, want %d", got, beforeSize)
	}

	afterSchema := captureSchemaRaw(t, path, "Gamma")
	if !reflect.DeepEqual(beforeSchema, afterSchema) {
		t.Errorf("column-definition bytes did not round-trip:\nbefore: %x\nafter:  %x", beforeSchema, afterSchema)
	}

	afterRows := captureRows(t, path, "Gamma")
	if !reflect.DeepEqual(beforeRows, afterRows) {
		t.Errorf("row values did not round-trip:\nbefore: %#v\nafter:  %#v", beforeRows, afterRows)
	}
}

// TestAlterTableRefusals checks that each documented boundary is an error
// rather than a silently wrong file, in the shape of TestDropTableRefusals
// (ddl_test.go).
func TestAlterTableRefusals(t *testing.T) {
	t.Run("AddColumn read-only", func(t *testing.T) {
		db := openTestFile(t, "MultiTable.abs")
		defer db.Close()

		col := Column{Name: "Z", BaseType: BftInt32, FieldType: FieldInteger}
		if err := db.AddColumn("Alpha", col); !errors.Is(err, ErrReadOnly) {
			t.Errorf("AddColumn on a read-only file: %v, want ErrReadOnly", err)
		}
	})

	t.Run("DropColumn read-only", func(t *testing.T) {
		db := openTestFile(t, "MultiTable.abs")
		defer db.Close()

		if err := db.DropColumn("Alpha", "Name"); !errors.Is(err, ErrReadOnly) {
			t.Errorf("DropColumn on a read-only file: %v, want ErrReadOnly", err)
		}
	})

	t.Run("no such table", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "MultiTable.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		col := Column{Name: "Z", BaseType: BftInt32, FieldType: FieldInteger}
		if err := db.AddColumn("Nowhere", col); !errors.Is(err, ErrNoSuchTable) {
			t.Errorf("AddColumn on an unknown table: %v, want ErrNoSuchTable", err)
		}

		if err := db.DropColumn("Nowhere", "X"); !errors.Is(err, ErrNoSuchTable) {
			t.Errorf("DropColumn on an unknown table: %v, want ErrNoSuchTable", err)
		}
	})

	t.Run("column already exists", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "MultiTable.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		col := Column{Name: "Id", BaseType: BftInt32, FieldType: FieldInteger}
		if err := db.AddColumn("Alpha", col); !errors.Is(err, ErrColumnExists) {
			t.Errorf("AddColumn of an existing name: %v, want ErrColumnExists", err)
		}
	})

	t.Run("no such column", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "MultiTable.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropColumn("Gamma", "Nonexistent"); !errors.Is(err, ErrNoSuchColumn) {
			t.Errorf("DropColumn of an unknown column: %v, want ErrNoSuchColumn", err)
		}
	})

	t.Run("unsupported column type", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "MultiTable.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		col := Column{Name: "Z", BaseType: BftDouble, FieldType: FieldDouble}
		if err := db.AddColumn("Alpha", col); !errors.Is(err, ErrUnsupportedColumnType) {
			t.Errorf("AddColumn of an undocumented column type: %v, want ErrUnsupportedColumnType", err)
		}
	})

	t.Run("table with BLOB pages", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, requireFixtureName(t, "RPDG0011.abs")))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		col := Column{Name: "Z", BaseType: BftInt32, FieldType: FieldInteger}
		if err := db.AddColumn("RPDG0011.abs", col); !errors.Is(err, ErrTableHasBlobPages) {
			t.Errorf("AddColumn on a table with BLOBs: %v, want ErrTableHasBlobPages", err)
		}

		if err := db.DropColumn("RPDG0011.abs", "PDGNo"); !errors.Is(err, ErrTableHasBlobPages) {
			t.Errorf("DropColumn on a table with BLOBs: %v, want ErrTableHasBlobPages", err)
		}
	})

	t.Run("column covered by an index", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, requireFixtureName(t, "RRAD0011.abs")))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropColumn("RRAD0011.abs", "No"); !errors.Is(err, ErrColumnIndexed) {
			t.Errorf("DropColumn of an indexed column: %v, want ErrColumnIndexed", err)
		}
	})

	t.Run("column named by a constraint", func(t *testing.T) {
		// RCFQ0011.abs carries six NOT NULL constraints plus a PRIMARY KEY
		// (on FrqNo, which is also the table's one real index -- SrcNo is
		// NOT NULL but not indexed, so this exercises ErrColumnConstrained
		// on its own rather than being shadowed by ErrColumnIndexed).
		db, err := OpenForWrite(writableCopy(t, requireFixtureName(t, "RCFQ0011.abs")))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropColumn("RCFQ0011.abs", "SrcNo"); !errors.Is(err, ErrColumnConstrained) {
			t.Errorf("DropColumn of a NOT NULL column: %v, want ErrColumnConstrained", err)
		}
	})

	t.Run("only column", func(t *testing.T) {
		path := buildAlterFixture(t, 1, 1)

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		if err := db.DropColumn("", "C0"); !errors.Is(err, ErrLastColumn) {
			t.Errorf("DropColumn of a table's only column: %v, want ErrLastColumn", err)
		}
	})

	t.Run("record would not fit its page", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		col := Column{Name: "Huge", BaseType: BftVarchar, FieldType: FieldString, Size: 100000}
		if err := db.AddColumn("", col); !errors.Is(err, ErrRecordWontFit) {
			t.Errorf("AddColumn widening past the page: %v, want ErrRecordWontFit", err)
		}
	})

	t.Run("nothing is written when the alter is refused", func(t *testing.T) {
		path := writableCopy(t, "MultiTable.abs")
		before := fileDigest(t, path)

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		_ = db.AddColumn("Nowhere", Column{Name: "Z", BaseType: BftInt32, FieldType: FieldInteger})
		_ = db.AddColumn("Alpha", Column{Name: "Id", BaseType: BftInt32, FieldType: FieldInteger})
		_ = db.DropColumn("Gamma", "Nonexistent")

		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if fileDigest(t, path) != before {
			t.Error("a refused ALTER TABLE changed the file")
		}
	})
}

// requireFixtureName is requireFixture, returning the fixture's own name
// instead of its full path -- writableCopy takes a name, not a path.
func requireFixtureName(t *testing.T, name string) string {
	t.Helper()

	requireFixture(t, name)

	return name
}

// --- what the engine's ALTER TABLE actually does, and why this package does
// --- something else
//
// MultiTable-alteradd.abs and MultiTable-alterdrop.abs are the two fixtures
// PLAN.md long recorded as "never committed and could not be regenerated".
// They exist now, produced under DBManager against testdata/MultiTable.abs by
// exactly the statements the tests below name, and they reproduce the byte
// diffs PLAN.md measured in the round that first implemented ALTER TABLE
// (248 bytes for the ADD, 234 for the DROP).
//
// What they show is not a near-miss this package could close by fixing an
// accounting detail. The engine does not edit a table in place at all. It
// runs a four-statement sequence:
//
//	CREATE TABLE <temp> (the new column list)
//	copy every row across
//	rename <temp> to the original name
//	DROP TABLE the original
//
// Every counter in the file agrees with that reading, and nothing else
// explains all of them at once:
//
//   - the file header State advances 11 -> 15: four transactions, where
//     CREATE TABLE alone advances it by one.
//   - the table catalog page's State advances by three: the append, the
//     rename, and the drop's shift. The copy does not touch the catalog.
//   - Gamma keeps catalog position 2 -- it is the *drop* of the old entry
//     that shifts the new one down into place -- and the catalog carries a
//     stale fourth entry, already named Gamma, past its shortened length.
//     That is exactly the artifact dropCatalogEntry (ddl.go) documents.
//   - Gamma is a new object: table id 8 -> 12, its columns 9,10 -> 13,14,15,
//     and LastObjectID 11 -> 15. An in-place edit would have kept them.
//   - pages 17-22, the old table's, all carry pageStateFree. Pages 24-28 are
//     a fresh five-page table image in allocateTablePages' order (system,
//     system, schema, info, index), and page 29 is a new data page holding
//     the copied row.
//   - page 0's State advances by twelve (six pages allocated, six freed) and
//     page 1's by two (one extent each side), which is the PFS/EAM
//     accounting ddl.go already implements, applied to twelve bit changes.
//
// This package's AddColumn and DropColumn splice the schema stream and
// rewrite the records in place instead: three pages touched, thirty bytes,
// object ids preserved. The two are not reconcilable, and reproducing the
// engine's sequence was considered and deliberately rejected -- on a ground
// that has since expired. It needed six free pages, and at the time nothing
// in this package could grow a database. MultiTable.abs is the only file in
// the whole corpus that has six -- Writes.abs has three, every Employees-*.abs
// has two, and the customer fixtures have between none and five. An
// engine-faithful ALTER TABLE would have been byte-perfect on one fixture and
// refused on every other file this package exists to read. ddl_grow.go removed
// that constraint; the rebuild is now unblocked but still not done, and what
// it would have to reproduce is the whole four-transaction sequence, not just
// six pages.
//
// So byte identity is out of reach for these two operations, and is recorded
// as a measured divergence rather than an open question. What the fixtures
// can still hold ALTER TABLE to is that the two files mean the same thing,
// which is TestAlterTableMatchesEngineSemantically below, and that the
// engine's strategy is what this comment says it is, which is
// TestEngineAlterTableRebuildsTheTable.

// TestAlterTableMatchesEngineSemantically is what replaces byte identity for
// ALTER TABLE: run the statement through this package, and require the result
// to be indistinguishable from the engine's own output through every read
// path this package has -- the table list, the altered table's columns, and
// every row of every table, not just the altered one.
//
// Column ids are excluded, and only column ids. They are the one thing the
// two files genuinely disagree about, because the engine's rebuild allocates
// fresh object ids for a table it re-creates while the splice keeps the ones
// already there. Everything else -- names, types, sizes, positions, order,
// and all the data -- must match exactly.
func TestAlterTableMatchesEngineSemantically(t *testing.T) {
	cases := []struct {
		name      string
		want      string
		statement string
		table     string
		apply     func(db *File) error
	}{
		{
			name:      "ADD COLUMN",
			want:      "MultiTable-alteradd.abs",
			statement: "ALTER TABLE Gamma ADD (W INTEGER)",
			table:     "Gamma",
			apply: func(db *File) error {
				return db.AddColumn("Gamma", Column{Name: "W", BaseType: BftInt32, FieldType: FieldInteger})
			},
		},
		{
			name:      "DROP COLUMN",
			want:      "MultiTable-alterdrop.abs",
			statement: "ALTER TABLE Gamma DROP (V)",
			table:     "Gamma",
			apply: func(db *File) error {
				return db.DropColumn("Gamma", "V")
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantPath := requireFixture(t, c.want)
			path := writableCopy(t, "MultiTable.abs")

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			if err := c.apply(db); err != nil {
				t.Fatalf("%s: %v", c.statement, err)
			}

			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			compareTableNames(t, path, wantPath, c.statement)
			compareColumnsIgnoringIDs(t, path, wantPath, c.table, c.statement)

			// Every table, not only the altered one: the splice must leave
			// the file's other tables exactly as readable as the engine's
			// rebuild left them.
			for _, name := range []string{"Alpha", "Beta", "Gamma"} {
				compareRows(t, path, wantPath, name, c.statement)
			}
		})
	}
}

// compareTableNames requires two files to list the same tables in the same
// order. It compares names only: the engine's rebuild gives the altered table
// a new id and new pages, which is the divergence this test exists to permit.
func compareTableNames(t *testing.T, gotPath, wantPath, statement string) {
	t.Helper()

	got, want := tableNames(t, gotPath), tableNames(t, wantPath)

	if len(got) != len(want) {
		t.Fatalf("%s: wrote %d tables %v, the engine wrote %d %v", statement, len(got), got, len(want), want)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: table %d is %q, the engine wrote %q", statement, i, got[i], want[i])
		}
	}
}

func tableNames(t *testing.T, path string) []string {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables(%q): %v", path, err)
	}

	names := make([]string, len(tables))
	for i, tbl := range tables {
		names[i] = tbl.Name
	}

	return names
}

// compareColumnsIgnoringIDs requires a table's columns to agree in name,
// type, size and position. Column.ID is deliberately not compared; see this
// section's file comment for why the engine's differ.
func compareColumnsIgnoringIDs(t *testing.T, gotPath, wantPath, table, statement string) {
	t.Helper()

	got := captureSchemaColumns(t, gotPath, table)
	want := captureSchemaColumns(t, wantPath, table)

	if len(got) != len(want) {
		t.Fatalf("%s: %q has %d columns, the engine wrote %d", statement, table, len(got), len(want))
	}

	for i := range got {
		g, w := got[i], want[i]

		switch {
		case g.Name != w.Name:
			t.Errorf("%s: column %d is %q, the engine wrote %q", statement, i, g.Name, w.Name)
		case g.BaseType != w.BaseType || g.FieldType != w.FieldType:
			t.Errorf("%s: column %q is %d/%s, the engine wrote %d/%s",
				statement, g.Name, g.BaseType, g.FieldType, w.BaseType, w.FieldType)
		case g.Size != w.Size:
			t.Errorf("%s: column %q has size %d, the engine wrote %d", statement, g.Name, g.Size, w.Size)
		case g.Position != w.Position:
			t.Errorf("%s: column %q is at position %d, the engine wrote %d", statement, g.Name, g.Position, w.Position)
		}
	}
}

// compareRows requires a table to read back the same rows, in the same order,
// with the same values, from both files.
func compareRows(t *testing.T, gotPath, wantPath, table, statement string) {
	t.Helper()

	got := captureRows(t, gotPath, table)
	want := captureRows(t, wantPath, table)

	if len(got) != len(want) {
		t.Fatalf("%s: %q has %d rows, the engine wrote %d", statement, table, len(got), len(want))
	}

	for r := range got {
		if len(got[r]) != len(want[r]) {
			t.Errorf("%s: %q row %d has %d values, the engine wrote %d",
				statement, table, r, len(got[r]), len(want[r]))

			continue
		}

		for c := range got[r] {
			if fmt.Sprintf("%v", got[r][c]) != fmt.Sprintf("%v", want[r][c]) {
				t.Errorf("%s: %q row %d column %d is %v, the engine wrote %v",
					statement, table, r, c, got[r][c], want[r][c])
			}
		}
	}
}

// TestEngineAlterTableRebuildsTheTable pins the finding the section comment
// above rests on, so it cannot quietly stop being true. It asserts nothing
// about this package's own writes: every claim is about the two engine
// fixtures and MultiTable.abs, read side by side.
//
// If this test ever fails, the reasoning that byte identity is unreachable
// has to be revisited -- which is the point of writing it down as a test
// rather than only as prose.
func TestEngineAlterTableRebuildsTheTable(t *testing.T) {
	basePath := requireFixture(t, "MultiTable.abs")

	for _, c := range []struct {
		fixture     string
		wantColumns int
		wantLastObj int32
	}{
		{"MultiTable-alteradd.abs", 3, 15},
		{"MultiTable-alterdrop.abs", 1, 13},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			altered := requireFixture(t, c.fixture)

			baseInfo := tableInfoByName(t, basePath, "Gamma")
			newInfo := tableInfoByName(t, altered, "Gamma")

			// The table is a new object on new pages, not an edited one.
			if newInfo.ID == baseInfo.ID {
				t.Errorf("Gamma kept object id %d; the engine's ALTER re-creates the table", newInfo.ID)
			}

			if newInfo.SchemaPageNo == baseInfo.SchemaPageNo {
				t.Errorf("Gamma kept schema page %d; the engine's ALTER allocates a new one", newInfo.SchemaPageNo)
			}

			// ...but it keeps its name and its place in the catalog, because
			// the drop of the old entry shifts the new one down into it.
			if got := tableNames(t, altered); len(got) != 3 || got[2] != "Gamma" {
				t.Errorf("catalog is %v, want Gamma still third", got)
			}

			// The old table's pages are freed, not reused in place.
			for _, no := range []int{
				baseInfo.systemPageNo, baseInfo.SchemaPageNo, baseInfo.InfoPageNo,
			} {
				if !pageIsFreed(t, altered, no) {
					t.Errorf("page %d (one of old Gamma's) is not freed", no)
				}
			}

			if got := len(captureSchemaColumns(t, altered, "Gamma")); got != c.wantColumns {
				t.Errorf("Gamma has %d columns, want %d", got, c.wantColumns)
			}

			// LastObjectID moves by one table object plus one per column,
			// which is what CreateTable's own accounting would produce.
			if got := lastObjectID(t, altered); got != c.wantLastObj {
				t.Errorf("LastObjectID is %d, want %d", got, c.wantLastObj)
			}
		})
	}
}

func tableInfoByName(t *testing.T, path, name string) TableInfo {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables(%q): %v", path, err)
	}

	for _, tbl := range tables {
		if tbl.Name == name {
			return tbl
		}
	}

	t.Fatalf("%q has no table %q", path, name)

	return TableInfo{}
}

func pageIsFreed(t *testing.T, path string, no int) bool {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	page, err := db.ReadPage(no)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", no, err)
	}

	return page.Freed()
}

func lastObjectID(t *testing.T, path string) int32 {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	defer db.Close()

	return db.lastObjectID
}

// --- the column-count boundary ---
//
// nullFlagBytes = ceil(numColumns / 8), so it only widens when numColumns
// crosses a multiple of 8. Every fixture in the committed corpus has too few
// columns to cross one from a single ALTER TABLE, so this package builds its
// own synthetic single-table files (in the shape synthetic_test.go's helpers
// already assemble, plus the 16-byte schema tail this package's splice needs
// to find: indexCount=0, reserved=0, a record-index-root placeholder this
// package never reads, and blobIndexRoot=-1) to construct the boundary
// deliberately rather than hope the corpus happens to contain one.

// buildAlterFixture assembles a minimal single-table .abs file with numCols
// int32 columns and numRows rows of predictable values (100*row + col), and
// returns its path. It carries no table catalog, no index and no BLOB column
// -- Table("") resolves it through the same unlisted fallback
// TestSyntheticColumnCounts already exercises for reads.
func buildAlterFixture(t *testing.T, numCols, numRows int) string {
	t.Helper()

	cols := make([]synthColumn, numCols)
	for i := range cols {
		cols[i] = synthColumn{name: "C" + strconv.Itoa(i), base: BftInt32, field: FieldInteger}
	}

	rows := make([]synthRow, numRows)

	for r := range rows {
		values := make([][]byte, numCols)
		for c := range values {
			values[c] = synthInt32(int32(100*r + c))
		}

		rows[r] = synthRow{values: values}
	}

	payloadLen := synthPageSize - diskPageHeaderSize
	layout := computeSynthLayout(cols, payloadLen)

	if layout.recordsPerPage < numRows {
		t.Fatalf("test rows do not fit one page: %d rows, %d per page", numRows, layout.recordsPerPage)
	}

	blob := encodeSchemaBlob(cols)
	tail := make([]byte, systemIndexRootsSize+8)
	binary.LittleEndian.PutUint32(tail[8:12], 2)           // recordIndexRoot: a placeholder, never read by AddColumn/DropColumn
	binary.LittleEndian.PutUint32(tail[12:16], 0xFFFFFFFF) // blobIndexRoot: -1, no BLOB column
	blob = append(blob, tail...)

	pages := []synthPage{
		{pageType: PageTypeFileHdr, objectID: -1, nextPage: -1},
		{pageType: PageTypeSchema, objectID: 1, nextPage: -1, payload: encodeInternalFile(t, blob, true)},
		{pageType: PageTypeData, objectID: 1, nextPage: -1, payload: encodeDataPage(cols, rows, layout, payloadLen)},
	}

	path := filepath.Join(t.TempDir(), "alter-boundary.abs")
	if err := os.WriteFile(path, assembleFile(t, synthPageSize, pages), 0o600); err != nil {
		t.Fatalf("writing synthetic file: %v", err)
	}

	return path
}

// TestAlterTableColumnCountBoundary is the test that would have caught the
// Phase 5c "+2 fudge factor" bug: it builds tables straddling a multiple of 8
// on both sides of an ALTER TABLE, so nullFlagBytes changes and every field
// in every record moves, not just the field being added or removed. A test
// that never crossed the boundary would prove nothing about it.
func TestAlterTableColumnCountBoundary(t *testing.T) {
	for _, base := range []int{8, 16} {
		t.Run(fmt.Sprintf("AddColumn %d to %d", base, base+1), func(t *testing.T) {
			checkBoundaryAdd(t, base)
		})

		t.Run(fmt.Sprintf("DropColumn %d to %d", base+1, base), func(t *testing.T) {
			checkBoundaryDrop(t, base+1)
		})
	}
}

func checkBoundaryAdd(t *testing.T, numCols int) {
	t.Helper()

	path := buildAlterFixture(t, numCols, 3)
	before := captureRows(t, path, "")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.AddColumn("", Column{Name: "New", BaseType: BftInt32, FieldType: FieldInteger}); err != nil {
		t.Fatalf("AddColumn: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := captureRows(t, path, "")
	checkColumnAdded(t, before, after)
	checkNullFlagBytes(t, path, numCols+1)
}

func checkBoundaryDrop(t *testing.T, numCols int) {
	t.Helper()

	path := buildAlterFixture(t, numCols, 3)
	before := captureRows(t, path, "")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.DropColumn("", "C0"); err != nil {
		t.Fatalf("DropColumn: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := captureRows(t, path, "")
	checkColumnDropped(t, before, after, 0)
	checkNullFlagBytes(t, path, numCols-1)
}

// checkNullFlagBytes reopens path and asserts the derived record layout
// actually crossed the byte the test intends it to: without this, a bug that
// silently kept the OLD null-flag width would still pass the value checks
// above by coincidence whenever the two widths overlap enough columns.
func checkNullFlagBytes(t *testing.T, path string, wantCols int) {
	t.Helper()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	tbl, err := db.Table("")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	schema, err := tbl.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	if len(schema.Columns) != wantCols {
		t.Fatalf("column count = %d, want %d", len(schema.Columns), wantCols)
	}

	r := &Reader{db: db, schema: schema}
	r.computeLayout()

	want := (wantCols + 7) / 8
	if r.nullFlagBytes != want {
		t.Errorf("nullFlagBytes = %d, want %d (columns = %d)", r.nullFlagBytes, want, wantCols)
	}
}
