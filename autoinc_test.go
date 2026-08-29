package absdb

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// What the Auto*.abs fixtures settle about an AUTOINC column's counter.
//
// The counter is not derived from the column's rows. It is stored, one int64
// per column, in the table info file's per-column array -- the array
// buildTableInfoFile writes as zeroes, which is why it read as padding for so
// long: every slot of it is zero in a table with no AUTOINC column.
//
// writer_autoinc.go's comment states the rule these tests hold the writer to.

const (
	autoInsExpFixture     = "Auto-insexp.abs"
	autoDelFixture        = "Auto-del.abs"
	autoUpdFixture        = "Auto-upd.abs"
	autoUpdCompactFixture = "Auto-updcompact.abs"
)

// TestAutoIncCounterLivesInTheTableInfoFile is the finding itself, read off the
// two fixtures that were once taken as evidence that no counter is stored.
//
// The Keys pair is the control: its key is a plain INTEGER, so every slot of
// the same array stays zero across the same statement.
func TestAutoIncCounterLivesInTheTableInfoFile(t *testing.T) {
	for _, c := range []struct {
		fixture string
		table   string
		want    []int64
	}{
		{autoFixture, "Auto", []int64{3, 0}},
		{"Auto-ins.abs", "Auto", []int64{4, 0}},
		{keysFixture, "Keys", []int64{0, 0, 0}},
		{"Keys-ins.abs", "Keys", []int64{0, 0, 0}},

		// The two files where the counter and the column part company, pinned
		// exactly because the corpus check below can only skip them.
		{autoDelFixture, "Auto", []int64{3, 0}}, // rows 1, 2 -- a delete frees nothing
		{autoUpdFixture, "Auto", []int64{3, 0}}, // rows 20, 2, 3 -- the counter is *below* the maximum
	} {
		t.Run(c.fixture, func(t *testing.T) {
			db := openFixture(t, c.fixture)

			if got := tableInfoCounters(t, db, c.table); !equalInt64(got, c.want) {
				t.Errorf("%s counters = %v, want %v", c.fixture, got, c.want)
			}
		})
	}
}

// countersPartedFromTheColumn names the two fixtures where a delete or an
// update has left the counter and the column's maximum disagreeing. Both were
// generated for exactly that, and TestAutoIncCounterLivesInTheTableInfoFile
// pins their values; the corpus check skips them rather than weakening.
//
// Auto-upd.abs is the interesting direction. Its counter is *below* the
// maximum -- 3, with a row numbered 20 -- so the engine will hand out 4 .. 20
// and then collide with a row it wrote itself, on a PRIMARY KEY column, and
// refuse its own insert. That is the engine's behaviour and this package
// reproduces it rather than repairing it: raising the counter to the maximum
// would be a write the engine never makes, and it is the reason "the counter
// is the column's maximum" is not stated as an invariant anywhere.
var countersPartedFromTheColumn = map[string]bool{
	autoDelFixture: true,
	autoUpdFixture: true,
}

// TestAutoIncCounterHoldsTheColumnMaximum is the corpus-wide cross-check, and
// it is what makes the reading more than an interpretation of two files: every
// AUTOINC column in the corpus carries its column's maximum, and no column of
// any other kind anywhere carries a non-zero counter.
func TestAutoIncCounterHoldsTheColumnMaximum(t *testing.T) {
	checked := 0

	for _, name := range fixtureNames(t) {
		if countersPartedFromTheColumn[name] {
			continue
		}

		db, err := Open(filepath.Join("testdata", name))
		if err != nil {
			continue
		}

		if db.Encrypted() {
			db.Close()

			db, err = OpenWithPassword(filepath.Join("testdata", name), fixturePassword)
			if err != nil {
				continue
			}
		}

		checked += checkFixtureCounters(t, db, name)

		db.Close()
	}

	if checked == 0 {
		t.Fatal("no AUTOINC column found anywhere in the corpus")
	}

	t.Logf("checked %d AUTOINC columns", checked)
}

// checkFixtureCounters asserts one file's counters and returns how many AUTOINC
// columns it held.
func checkFixtureCounters(t *testing.T, db *File, name string) int {
	t.Helper()

	infos, err := db.Tables()
	if err != nil {
		return 0
	}

	found := 0

	for _, info := range infos {
		table, err := db.Table(info.Name)
		if err != nil {
			continue
		}

		schema, err := table.Schema()
		if err != nil {
			continue
		}

		counters := tableInfoCounters(t, db, info.Name)
		if len(counters) != len(schema.Columns) {
			t.Errorf("%s: %s has %d counters and %d columns",
				name, info.Name, len(counters), len(schema.Columns))

			continue
		}

		for i, col := range schema.Columns {
			if !col.IsAutoInc() {
				if counters[i] != 0 {
					t.Errorf("%s: %s.%s is not AUTOINC and carries counter %d",
						name, info.Name, col.Name, counters[i])
				}

				continue
			}

			found++

			if got, most := counters[i], columnMaximum(t, table, i); got != most {
				t.Errorf("%s: %s.%s counter = %d, column maximum = %d",
					name, info.Name, col.Name, got, most)
			}
		}
	}

	return found
}

// TestAutoIncWritesMatchEngineByteForByte is the gate. Seven statements against
// an AUTOINC primary key, each reproducing the file DBManager wrote for it,
// with no State exclusion: none of these allocates a page.
//
// Three of them pass nil for the key, which is this package's way of writing
// the engine's own "INSERT INTO Auto (Name) VALUES (...)" -- the case that
// makes the counter the source of the value rather than a record of it.
func TestAutoIncWritesMatchEngineByteForByte(t *testing.T) {
	for _, c := range []struct {
		name      string
		base      string
		want      string
		statement string
		apply     func(*testing.T, *File, *TableWriter)
	}{
		{
			name:      "the engine numbers the row",
			base:      autoFixture,
			want:      "Auto-ins.abs",
			statement: "INSERT INTO Auto (Name) VALUES ('Alan')",
			apply:     insertRow(nil, "Alan"),
		},
		{
			name:      "an explicit value above the counter",
			base:      autoFixture,
			want:      autoInsExpFixture,
			statement: "INSERT INTO Auto (Id, Name) VALUES (10, 'Zoe')",
			apply:     insertRow(int32(10), "Zoe"),
		},
		{
			// The counter, not the column, is what the next value comes from:
			// the rows are 1, 2, 3, 10 and the engine picks 11.
			name:      "the next value follows the raised counter",
			base:      autoInsExpFixture,
			want:      "Auto-insnext.abs",
			statement: "INSERT INTO Auto (Name) VALUES ('Next')",
			apply:     insertRow(nil, "Next"),
		},
		{
			name:      "an explicit value below the counter",
			base:      autoInsExpFixture,
			want:      "Auto-inslow.abs",
			statement: "INSERT INTO Auto (Id, Name) VALUES (5, 'Low')",
			apply:     insertRow(int32(5), "Low"),
		},
		{
			name:      "a delete does not lower the counter",
			base:      autoFixture,
			want:      autoDelFixture,
			statement: "DELETE FROM Auto WHERE Id = 3",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				if err := w.Delete(recordWithKey(t, db, 3)); err != nil {
					t.Fatalf("Delete: %v", err)
				}
			},
		},
		{
			// The freed value is not reissued: rows 1 and 2 with a counter of
			// 3 give 4, not the 3 the delete made available.
			name:      "the value a delete freed is not reissued",
			base:      autoDelFixture,
			want:      "Auto-delins.abs",
			statement: "INSERT INTO Auto (Name) VALUES ('After')",
			apply:     insertRow(nil, "After"),
		},
		{
			// The case a writer that recomputed the counter from the rows
			// would fail: the column's maximum becomes 20 and the counter
			// stays at 3.
			name:      "an update does not raise the counter",
			base:      autoFixture,
			want:      autoUpdFixture,
			statement: "UPDATE Auto SET Id = 20 WHERE Id = 1",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				if err := w.UpdateColumn(recordWithKey(t, db, 1), 0, int32(20)); err != nil {
					t.Fatalf("UpdateColumn: %v", err)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireEngineBytes(t, c.base, c.want, c.statement, c.apply)
		})
	}
}

// TestCompactedAutoIncCounterFollowsTheRows pins what compaction does to a
// counter, on the one file where rebuilding it and carrying it forward give
// different answers: Auto-upd.abs holds a row numbered 20 and a counter of 3.
//
// The engine rebuilds, and this package reaches the same answer for the same
// reason -- copyTableRows re-inserts every row, and an insert raises the
// counter -- rather than by copying the counter across.
func TestCompactedAutoIncCounterFollowsTheRows(t *testing.T) {
	src := writableCopy(t, autoUpdFixture)
	dst := filepath.Join(t.TempDir(), "compacted.abs")

	if err := CompactDatabase(src, dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	db, err := Open(dst)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	if got := tableInfoCounters(t, db, "Auto"); !equalInt64(got, []int64{20, 0}) {
		t.Errorf("compacted counters = %v, want [20 0]", got)
	}
}

// TestCreateAutoIncTableMatchesEngineByteForByte rebuilds the whole of
// Auto.abs from Empty.abs: the CREATE TABLE, its PRIMARY KEY and backing index,
// and the three rows the engine numbered itself. Only page State words are
// excluded.
//
// This is what pins the AUTOINC column definition. The bytes go through
// internal/zlib1 into a compressed schema stream, so a serializer that had the
// field-type byte, the size or the autoinc block wrong could not reproduce it;
// and because the three inserts pass nil, the row values 1, 2 and 3 are this
// package's assignments rather than constants copied out of the fixture.
func TestCreateAutoIncTableMatchesEngineByteForByte(t *testing.T) {
	want, err := os.ReadFile(requireFixture(t, autoFixture))
	if err != nil {
		t.Fatalf("reading %s: %v", autoFixture, err)
	}

	fixture := openFixture(t, autoFixture)
	constraints := constraintsOf(t, fixture, "Auto")
	fixture.Close()

	path := writableCopy(t, "Empty.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	columns := []Column{
		{Name: "Id", BaseType: BftInt32, FieldType: FieldAutoInc},
		{Name: "Name", BaseType: BftVarchar, FieldType: FieldString, Size: 20},
	}

	if err := db.createTable("Auto", columns, constraints); err != nil {
		t.Fatalf("createTable: %v", err)
	}

	for _, name := range []string{"Ada", "Grace", "Edsger"} {
		insertOneRow(t, db, "Auto", []any{nil, name})
	}

	pageSize, pageCount := db.PageSize(), db.PageCount()

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	pages := make([]int, pageCount)
	for no := range pageCount {
		pages[no] = no
	}

	reportByteDifferencesExcept(t, got, want,
		"CREATE TABLE Auto (Id AUTOINC PRIMARY KEY, Name VARCHAR(20)) + three INSERTs",
		pageStateExclusions(pages, pageSize))
}

// TestCompactAutoIncMatchesEngineByteForByte is the compaction half, against
// the file DBManager's own Database -> Compact Database wrote for the same
// source. It is the case that separates rebuilding the counter from carrying it
// forward: the source's counter is 3 and its highest row is 20.
func TestCompactAutoIncMatchesEngineByteForByte(t *testing.T) {
	want, err := os.ReadFile(requireFixture(t, autoUpdCompactFixture))
	if err != nil {
		t.Fatalf("reading %s: %v", autoUpdCompactFixture, err)
	}

	dst := newDatabasePath(t, "auto-compacted.abs")

	if err := CompactDatabase(requireFixture(t, autoUpdFixture), dst); err != nil {
		t.Fatalf("CompactDatabase: %v", err)
	}

	db, err := Open(dst)
	if err != nil {
		t.Fatalf("Open(%q): %v", dst, err)
	}

	pageSize, pageCount := db.PageSize(), db.PageCount()

	db.Close()

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	// Every page but the two allocation maps, whose States are counters rather
	// than seeds -- the same exclusion TestCompactDatabaseMatchesEngineByteForByte
	// makes, expressed the same way.
	pages := make([]int, 0, pageCount)
	for no := 2; no < pageCount; no++ {
		pages = append(pages, no)
	}

	reportPageByteDifferences(t, got, want, "COMPACT DATABASE",
		pageStateExclusions(pages, pageSize), pageSize)
}

// TestAutoIncPrimaryKeyRefusesADuplicate covers the refusals an AUTOINC key
// shares with an Int32 one, checked here because a value this package assigns
// must be one the key would accept. The counter is what makes the assigned
// value safe: Auto-upd.abs's row 20 is above the counter, so nothing assigned
// can collide with it until the counter climbs there.
func TestAutoIncPrimaryKeyRefusesADuplicate(t *testing.T) {
	for _, c := range []struct {
		name  string
		apply func(*TableWriter) error
		want  error
	}{
		{
			name: "an explicit duplicate",
			want: ErrDuplicateKey,
			apply: func(w *TableWriter) error {
				_, err := w.Insert([]any{int32(2), "Dup"})

				return err
			},
		},
		{
			name: "an assigned value is not a duplicate",
			want: nil,
			apply: func(w *TableWriter) error {
				_, err := w.Insert([]any{nil, "Fine"})

				return err
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := writableCopy(t, autoFixture)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			defer db.Close()

			w, err := db.OpenTableWriter()
			if err != nil {
				t.Fatalf("OpenTableWriter: %v", err)
			}

			defer w.Close()

			if err := c.apply(w); !errors.Is(err, c.want) {
				t.Errorf("Insert = %v, want %v", err, c.want)
			}
		})
	}
}

// insertRow builds the apply function for a two-column Auto row.
func insertRow(id any, name string) func(*testing.T, *File, *TableWriter) {
	return func(t *testing.T, _ *File, w *TableWriter) {
		t.Helper()

		if _, err := w.Insert([]any{id, name}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
}

// tableInfoCounters reads one table's per-column counter array.
func tableInfoCounters(t *testing.T, db *File, name string) []int64 {
	t.Helper()

	table, err := db.Table(name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}

	no, err := table.infoPageNo()
	if err != nil {
		t.Fatalf("%s: infoPageNo: %v", name, err)
	}

	page, err := db.ReadPage(no)
	if err != nil {
		t.Fatalf("%s: ReadPage(%d): %v", name, no, err)
	}

	schema, err := table.Schema()
	if err != nil {
		t.Fatalf("%s: Schema: %v", name, err)
	}

	base, err := autoIncCounterBase(page.Payload, len(schema.Columns))
	if err != nil {
		t.Fatalf("%s: autoIncCounterBase: %v", name, err)
	}

	out := make([]int64, len(schema.Columns))
	for i := range out {
		off := base + i*tableInfoCounterFields
		out[i] = int64(binary.LittleEndian.Uint64(page.Payload[off : off+8]))
	}

	return out
}

// columnMaximum returns the largest value one integer column holds, or 0 when
// the table has no row with a value in it -- which is the counter a table that
// has never been inserted into carries.
func columnMaximum(t *testing.T, table *Table, col int) int64 {
	t.Helper()

	r, err := table.Open()
	if err != nil {
		t.Fatalf("%s: Open: %v", table.Name(), err)
	}

	var most int64

	for r.Next() {
		rec := r.Record()
		if rec.IsNull(col) {
			continue
		}

		if v := int64(rec.Int(col)); v > most {
			most = v
		}
	}

	return most
}

// equalInt64 is equalInt32 for the counter array.
func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
