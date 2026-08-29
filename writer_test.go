package absdb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writableCopy copies a fixture into the test's temporary directory and returns
// the copy's path.
//
// Every write test works on a copy. The fixtures in testdata/ are read-only
// ground truth — most of them are private files that exist nowhere else — so no
// test is ever allowed to open one for writing.
func writableCopy(t *testing.T, fixture string) string {
	t.Helper()

	src := requireFixture(t, fixture)

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", fixture, err)
	}

	dst := filepath.Join(t.TempDir(), fixture)

	err = os.WriteFile(dst, data, 0o600)
	if err != nil {
		t.Fatalf("writing copy of %s: %v", fixture, err)
	}

	return dst
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return sha256.Sum256(data)
}

// firstRecord returns the identity and bytes of the first record of the table.
func firstRecord(t *testing.T, db *File) (RecordID, []byte) {
	t.Helper()

	r, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	if !r.Next() {
		t.Fatalf("fixture has no records: %v", r.Err())
	}

	id, ok := r.RecordID()
	if !ok {
		t.Fatal("RecordID reported no record while positioned on one")
	}

	start, ok := r.recordStart(r.recordIdx)
	if !ok {
		t.Fatal("recordStart reported no record while positioned on one")
	}

	rec := make([]byte, r.recordSize)
	copy(rec, r.pageData[start:start+r.recordSize])

	return id, rec
}

func TestOpenForWriteReportsWritable(t *testing.T) {
	path := writableCopy(t, "Employees-Rijndael_128.abs")

	ro, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer ro.Close()

	if ro.Writable() {
		t.Error("a file opened with Open reports itself writable")
	}

	rw, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer rw.Close()

	if !rw.Writable() {
		t.Error("a file opened with OpenForWrite reports itself read-only")
	}
}

// TestOpenForWriteDoesNotModifyFile pins that acquiring a write handle, and even
// opening a table writer on it, leaves the file untouched. Only Commit writes.
func TestOpenForWriteDoesNotModifyFile(t *testing.T) {
	path := writableCopy(t, "Employees-Rijndael_128.abs")
	before := fileDigest(t, path)

	db, err := OpenForWriteWithPassword(path, testPassword)
	if err != nil {
		t.Fatalf("OpenForWriteWithPassword: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id, _ := firstRecord(t, db)

	err = w.UpdateColumn(id, 0, int32(7))
	if err != nil {
		t.Fatalf("UpdateColumn: %v", err)
	}

	w.Close()
	db.Close()

	if fileDigest(t, path) != before {
		t.Error("file changed although the writer was never committed")
	}
}

func TestTableWriterRequiresWritableFile(t *testing.T) {
	path := writableCopy(t, "Employees-Rijndael_128.abs")

	db, err := OpenWithPassword(path, testPassword)
	if err != nil {
		t.Fatalf("OpenWithPassword: %v", err)
	}

	defer db.Close()

	_, err = db.OpenTableWriter()
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("OpenTableWriter on a read-only handle: got %v, want ErrReadOnly", err)
	}
}

// TestWriterUpdateRoundTrip changes one record, commits, and reads the file back
// from scratch. It also re-verifies every page checksum, because a page written
// back to an encrypted file has to be re-encrypted and re-checksummed to stay
// readable at all.
func TestWriterUpdateRoundTrip(t *testing.T) {
	for _, fixture := range employeeFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := writableCopy(t, fixture.name)

			db, err := OpenForWriteWithPassword(path, testPassword)
			if err != nil {
				t.Fatalf("OpenForWriteWithPassword: %v", err)
			}

			w, err := db.OpenTableWriter()
			if err != nil {
				t.Fatalf("OpenTableWriter: %v", err)
			}

			id, original := firstRecord(t, db)

			// Change one column and leave every other byte alone, so the
			// read-back can attribute any difference it finds.
			changed := make([]byte, len(original))
			copy(changed, original)
			changed[1] = 42

			err = w.UpdateColumn(id, 0, int32(42))
			if err != nil {
				t.Fatalf("UpdateColumn: %v", err)
			}

			err = w.Commit()
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}

			db.Close()

			reopened, err := OpenWithPassword(path, testPassword)
			if err != nil {
				t.Fatalf("reopening after commit: %v", err)
			}

			defer reopened.Close()

			_, got := firstRecord(t, reopened)
			if !bytes.Equal(got, changed) {
				t.Errorf("record after commit:\n got %x\nwant %x", got, changed)
			}

			checkPageChecksums(t, reopened)
		})
	}
}

// TestWriterRollbackLeavesFileUnchanged asserts the property that makes rollback
// exact rather than compensating: nothing reaches the file before Commit.
func TestWriterRollbackLeavesFileUnchanged(t *testing.T) {
	path := writableCopy(t, "Employees-Square.abs")
	before := fileDigest(t, path)

	db, err := OpenForWriteWithPassword(path, testPassword)
	if err != nil {
		t.Fatalf("OpenForWriteWithPassword: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id, _ := firstRecord(t, db)

	err = w.UpdateColumn(id, 0, int32(999))
	if err != nil {
		t.Fatalf("UpdateColumn: %v", err)
	}

	w.Rollback()
	db.Close()

	if fileDigest(t, path) != before {
		t.Error("file changed after Rollback")
	}
}

func TestWriterRejectsUseAfterClose(t *testing.T) {
	path := writableCopy(t, "Employees-Rijndael_128.abs")

	db, err := OpenForWriteWithPassword(path, testPassword)
	if err != nil {
		t.Fatalf("OpenForWriteWithPassword: %v", err)
	}

	defer db.Close()

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id, _ := firstRecord(t, db)

	w.Rollback()

	if err := w.UpdateColumn(id, 0, int32(1)); !errors.Is(err, ErrWriterClosed) {
		t.Errorf("UpdateColumn after Rollback: got %v, want ErrWriterClosed", err)
	}

	if err := w.Commit(); !errors.Is(err, ErrWriterClosed) {
		t.Errorf("Commit after Rollback: got %v, want ErrWriterClosed", err)
	}
}

func TestWriterRejectsUnknownRecord(t *testing.T) {
	path := writableCopy(t, "Employees-Rijndael_128.abs")

	db, err := OpenForWriteWithPassword(path, testPassword)
	if err != nil {
		t.Fatalf("OpenForWriteWithPassword: %v", err)
	}

	defer db.Close()

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id, _ := firstRecord(t, db)

	cases := map[string]RecordID{
		"negative slot":     {PageNo: id.PageNo, Slot: -1},
		"slot past the end": {PageNo: id.PageNo, Slot: 1 << 20},
		"unoccupied slot":   {PageNo: id.PageNo, Slot: 100},
		"page 0":            {PageNo: 0, Slot: 0},
		"non-existent page": {PageNo: 1 << 20, Slot: 0},
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if err := w.UpdateColumn(bad, 0, int32(1)); !errors.Is(err, ErrNoRecord) {
				t.Errorf("UpdateColumn(%+v): got %v, want ErrNoRecord", bad, err)
			}
		})
	}
}

func TestSetAndClearBit(t *testing.T) {
	data := make([]byte, 4)

	setBit(data, 3, len(data))
	setBit(data, 8, len(data))

	if data[0] != 0x08 || data[1] != 0x01 {
		t.Errorf("after setBit: %x", data)
	}

	clearBit(data, 3, len(data))

	if data[0] != 0 || data[1] != 0x01 {
		t.Errorf("after clearBit: %x", data)
	}

	// Out-of-range bits are ignored rather than panicking, matching bitSet.
	setBit(data, -1, len(data))
	setBit(data, 1<<20, len(data))
	clearBit(data, -1, len(data))
	clearBit(data, 1<<20, len(data))

	if !bytes.Equal(data, []byte{0x00, 0x01, 0x00, 0x00}) {
		t.Errorf("out-of-range bit operations changed the bitmap: %x", data)
	}
}

// recordWithKey returns the identity of the record whose first column holds
// key. The Writes fixtures use a plain integer first column, so this is enough
// to address a row by hand.
func recordWithKey(t *testing.T, db *File, key int32) RecordID {
	t.Helper()

	r, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	for r.Next() {
		if r.Record().Int(0) != key {
			continue
		}

		id, ok := r.RecordID()
		if !ok {
			t.Fatal("RecordID reported no record while positioned on one")
		}

		return id
	}

	t.Fatalf("no record with Id=%d: %v", key, r.Err())

	return RecordID{}
}

// TestWriterMatchesEngineByteForByte is the test the whole write path exists to
// pass. Each case takes a database the engine produced, applies through this
// package the single SQL statement the engine was given, and requires the
// result to be byte-identical to the file the engine wrote.
//
// Byte identity is a far stronger claim than "reads back correctly": it covers
// the record bytes, the occupancy bitmap, the per-page record count in the
// internal record-page index, both counters in the table info page, the State
// counter of every written page and the State field of the database header. A
// writer that skipped any of those would still round-trip through this
// package's own reader and would still be wrong.
func TestWriterMatchesEngineByteForByte(t *testing.T) {
	cases := []struct {
		name      string
		base      string
		want      string
		statement string
		apply     func(t *testing.T, db *File, w *TableWriter)
	}{
		{
			name:      "insert",
			base:      "Writes.abs",
			want:      "Writes-ins1.abs",
			statement: "INSERT INTO Writes VALUES (4, 'Alan', 555.5, True)",
			apply: func(t *testing.T, _ *File, w *TableWriter) {
				t.Helper()

				_, err := w.Insert([]any{int32(4), "Alan", 555.5, true})
				if err != nil {
					t.Fatalf("Insert: %v", err)
				}
			},
		},
		{
			name:      "second insert",
			base:      "Writes-ins1.abs",
			want:      "Writes-ins2.abs",
			statement: "INSERT INTO Writes VALUES (5, 'Emmy', 777.25, False)",
			apply: func(t *testing.T, _ *File, w *TableWriter) {
				t.Helper()

				_, err := w.Insert([]any{int32(5), "Emmy", 777.25, false})
				if err != nil {
					t.Fatalf("Insert: %v", err)
				}
			},
		},
		{
			name:      "update a float column",
			base:      "Writes.abs",
			want:      "Writes-upd.abs",
			statement: "UPDATE Writes SET Salary = 1.5 WHERE Id = 2",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				err := w.UpdateColumn(recordWithKey(t, db, 2), 2, 1.5)
				if err != nil {
					t.Fatalf("UpdateColumn: %v", err)
				}
			},
		},
		{
			name:      "update a string column",
			base:      "Writes.abs",
			want:      "Writes-updname.abs",
			statement: "UPDATE Writes SET Name = 'Grazia' WHERE Id = 2",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				err := w.UpdateColumn(recordWithKey(t, db, 2), 1, "Grazia")
				if err != nil {
					t.Fatalf("UpdateColumn: %v", err)
				}
			},
		},
		{
			name:      "delete",
			base:      "Writes.abs",
			want:      "Writes-del.abs",
			statement: "DELETE FROM Writes WHERE Id = 2",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				err := w.Delete(recordWithKey(t, db, 2))
				if err != nil {
					t.Fatalf("Delete: %v", err)
				}
			},
		},
		{
			// Two rows in one transaction. This is the case that separates
			// the change counter, which moves by the number of records
			// touched, from the State counters, which move by one.
			name:      "update two rows in one transaction",
			base:      "Writes.abs",
			want:      "Writes-upd2.abs",
			statement: "UPDATE Writes SET Salary = 3.25 WHERE Id < 3",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				for _, key := range []int32{1, 2} {
					err := w.UpdateColumn(recordWithKey(t, db, key), 2, 3.25)
					if err != nil {
						t.Fatalf("UpdateColumn(Id=%d): %v", key, err)
					}
				}
			},
		},
		{
			name:      "delete two rows in one transaction",
			base:      "Writes.abs",
			want:      "Writes-del2.abs",
			statement: "DELETE FROM Writes WHERE Id < 3",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				for _, key := range []int32{1, 2} {
					err := w.Delete(recordWithKey(t, db, key))
					if err != nil {
						t.Fatalf("Delete(Id=%d): %v", key, err)
					}
				}
			},
		},
		{
			// The four indexed cases below are the same statements against
			// Writes-idx.abs, which carries IdxId over Id. Each pins one of the
			// leaf splices in writer_index.go against the engine's own bytes.
			//
			// Byte identity is the right bar here, with no State exclusion:
			// maintenance allocates nothing, so every page it writes already
			// existed and carries a counter rather than a fresh random seed.
			name:      "insert with an index, key sorts last",
			base:      "Writes-idx.abs",
			want:      "Writes-idx-ins.abs",
			statement: "INSERT INTO Writes VALUES (4, 'Alan', 555.5, True) -- indexed",
			apply: func(t *testing.T, _ *File, w *TableWriter) {
				t.Helper()

				_, err := w.Insert([]any{int32(4), "Alan", 555.5, true})
				if err != nil {
					t.Fatalf("Insert: %v", err)
				}
			},
		},
		{
			// Id=0 sorts before every stored key, so the whole entry array
			// shifts up by one stride. Writes-idx-ins.abs only ever appends,
			// and so cannot tell a sorted insert from an append.
			name:      "insert with an index, key sorts first",
			base:      "Writes-idx.abs",
			want:      "Writes-idx-ins0.abs",
			statement: "INSERT INTO Writes VALUES (0, 'Zero', 1.0, True) -- indexed",
			apply: func(t *testing.T, _ *File, w *TableWriter) {
				t.Helper()

				_, err := w.Insert([]any{int32(0), "Zero", 1.0, true})
				if err != nil {
					t.Fatalf("Insert: %v", err)
				}
			},
		},
		{
			// Removing the middle of three entries: the tail shifts down and
			// the slot it vacates keeps its old bytes. This is the case that
			// proves the leaf tail is not cleared.
			name:      "delete with an index",
			base:      "Writes-idx.abs",
			want:      "Writes-idx-del.abs",
			statement: "DELETE FROM Writes WHERE Id = 2 -- indexed",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				err := w.Delete(recordWithKey(t, db, 2))
				if err != nil {
					t.Fatalf("Delete: %v", err)
				}
			},
		},
		{
			// Moving an indexed column's value. The engine removes the entry
			// and reinserts it in sorted position, leaving EntryCount alone --
			// keys [1,2,3] become [1,3,9], not [1,9,3].
			name:      "update an indexed column",
			base:      "Writes-idx.abs",
			want:      "Writes-idx-upd.abs",
			statement: "UPDATE Writes SET Id = 9 WHERE Id = 2",
			apply: func(t *testing.T, db *File, w *TableWriter) {
				t.Helper()

				err := w.UpdateColumn(recordWithKey(t, db, 2), 0, int32(9))
				if err != nil {
					t.Fatalf("UpdateColumn: %v", err)
				}
			},
		},
		{
			// The engine reuses the slot the delete freed rather than
			// appending, which is why freeSlot scans bitmap order.
			name:      "insert into a deleted slot",
			base:      "Writes-del.abs",
			want:      "Writes-delins.abs",
			statement: "INSERT INTO Writes VALUES (6, 'Rosa', 42.0, True)",
			apply: func(t *testing.T, _ *File, w *TableWriter) {
				t.Helper()

				_, err := w.Insert([]any{int32(6), "Rosa", 42.0, true})
				if err != nil {
					t.Fatalf("Insert: %v", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(requireFixture(t, c.want))
			if err != nil {
				t.Fatalf("reading %s: %v", c.want, err)
			}

			path := writableCopy(t, c.base)

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			w, err := db.OpenTableWriter()
			if err != nil {
				t.Fatalf("OpenTableWriter: %v", err)
			}

			c.apply(t, db, w)

			err = w.Commit()
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}

			db.Close()

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading result: %v", err)
			}

			reportByteDifferences(t, got, want, c.statement)
		})
	}
}

// reportByteDifferences fails with the first few differing byte offsets, which
// is what makes a mismatch diagnosable: the offset says which structure was not
// kept in step.
func reportByteDifferences(t *testing.T, got, want []byte, statement string) {
	t.Helper()

	if bytes.Equal(got, want) {
		return
	}

	if len(got) != len(want) {
		t.Fatalf("%s: wrote %d bytes, the engine wrote %d", statement, len(got), len(want))
	}

	differing := 0

	for i := range got {
		if got[i] == want[i] {
			continue
		}

		if differing < 8 {
			t.Errorf("%s: byte %d (page %d offset 0x%x): wrote %02x, engine wrote %02x",
				statement, i, i/4096, i%4096, got[i], want[i])
		}

		differing++
	}

	t.Errorf("%s: %d bytes differ from the file the engine wrote", statement, differing)
}

// TestWriterInsertAndDeleteRoundTrip exercises the record-level operations on
// the unencrypted fixture, which has no user index and so accepts them.
func TestWriterInsertAndDeleteRoundTrip(t *testing.T) {
	path := writableCopy(t, "Writes.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	err = w.Delete(recordWithKey(t, db, 1))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = w.Insert([]any{int32(10), "Emmy", 3.5, false})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	_, err = w.Insert([]any{int32(11), nil, nil, nil})
	if err != nil {
		t.Fatalf("Insert with NULLs: %v", err)
	}

	err = w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}

	defer reopened.Close()

	r, err := reopened.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	type row struct {
		id     int32
		name   string
		salary float64
		null   bool
	}

	var got []row

	for r.Next() {
		rec := r.Record()
		got = append(got, row{rec.Int(0), rec.String(1), rec.Float(2), rec.IsNull(1)})
	}

	want := []row{
		{10, "Emmy", 3.5, false},
		{2, "Grace", 2345.75, false},
		{3, "Kurt", 999.25, false},
		{11, "", 0, true},
	}

	// Salary is not compared for Kurt and the NULL row; only the fields the
	// test set are listed above.
	want[2].salary = 999.25

	if len(got) != len(want) {
		t.Fatalf("read back %d rows, want %d: %+v", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestWriterRefusesToLoseABlobReference pins the scope boundary around BLOBs:
// an update may not drop a reference this package cannot free.
func TestWriterRefusesToLoseABlobReference(t *testing.T) {
	path := writableCopy(t, "RPDG0011.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	blob := -1

	for i, c := range w.Schema().Columns {
		if c.IsBLOB() {
			blob = i

			break
		}
	}

	if blob < 0 {
		t.Skip("fixture has no BLOB column")
	}

	r, err := db.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	for r.Next() {
		if r.Record().IsNull(blob) {
			continue
		}

		id, _ := r.RecordID()

		err = w.UpdateColumn(id, blob, nil)
		if !errors.Is(err, ErrBlobReferenceLost) {
			t.Errorf("clearing a live BLOB reference: got %v, want ErrBlobReferenceLost", err)
		}

		return
	}

	t.Skip("fixture has no non-NULL BLOB value")
}

// TestWriterUpdateWholeRecord covers Update, which replaces every column at
// once, and Record, which shows the writer's own view including changes that
// have not been committed yet.
func TestWriterUpdateWholeRecord(t *testing.T) {
	path := writableCopy(t, "Writes.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id := recordWithKey(t, db, 3)

	err = w.Update(id, []any{int32(30), "Emmy", 12.5, false})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Before the commit the writer sees the new record and the file does not.
	buffered, err := w.Record(id)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if got := buffered.String(1); got != "Emmy" {
		t.Errorf("buffered record: Name is %q, want %q", got, "Emmy")
	}

	onDisk, err := Open(path)
	if err != nil {
		t.Fatalf("opening the file alongside the writer: %v", err)
	}

	r, err := onDisk.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	for r.Next() {
		if r.Record().Int(0) == 30 {
			t.Error("an uncommitted update is visible in the file")
		}
	}

	onDisk.Close()

	err = w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}

	defer reopened.Close()

	after, err := reopened.OpenTable()
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}

	found := false

	for after.Next() {
		rec := after.Record()
		if rec.Int(0) != 30 {
			continue
		}

		found = true

		if rec.String(1) != "Emmy" || rec.Float(2) != 12.5 || rec.Bool(3) {
			t.Errorf("record after commit: %d %q %v %v",
				rec.Int(0), rec.String(1), rec.Float(2), rec.Bool(3))
		}
	}

	if !found {
		t.Error("the updated record is not in the file after Commit")
	}
}

// TestWriterOnMultiTableFileTouchesOnlyItsOwnPages checks that a write through
// one table's handle stays inside that table.
//
// There is no engine-produced file to diff against for a multi-table write, so
// this asserts the next strongest thing that can be checked without one: which
// pages moved. Updating a row of Beta may touch Beta's data page, Beta's
// record-page index, Beta's table-info page and the file header, and nothing
// belonging to Alpha or Gamma. Before the catalog existed the writer had no
// notion of which table it was writing, and advanced whichever table-info page
// came first in the file — Alpha's.
func TestWriterOnMultiTableFileTouchesOnlyItsOwnPages(t *testing.T) {
	path := writableCopy(t, "MultiTable.abs")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	beta, err := db.Table("Beta")
	if err != nil {
		t.Fatalf("Table(Beta): %v", err)
	}

	w, err := beta.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	reader, err := beta.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !reader.Next() {
		t.Fatal("Beta has no rows")
	}

	id, ok := reader.RecordID()
	if !ok {
		t.Fatal("Beta's first row has no record ID")
	}

	err = w.UpdateColumn(id, 1, 99.5)
	if err != nil {
		t.Fatalf("UpdateColumn: %v", err)
	}

	err = w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading the file: %v", err)
	}

	changed := changedPages(t, before, after, db.PageSize())
	t.Logf("updating one Beta row changed pages %v", changed)

	// Beta's run is pages 11..16; the file header is page 0.
	allowed := map[int]bool{0: true, 14: true, 15: true, 16: true}
	for _, p := range changed {
		if !allowed[p] {
			t.Errorf("writing Beta changed page %d, which is not Beta's", p)
		}
	}

	if !slices.Contains(changed, 16) {
		t.Error("Beta's data page 16 was not written")
	}

	// The other two tables must read exactly as they did.
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer db.Close()

	for _, tc := range []struct {
		table string
		rows  [][]any
	}{
		{"Alpha", [][]any{{int64(1), "one"}, {int64(2), "two"}}},
		{"Gamma", [][]any{{int64(10), int64(100)}}},
		{"Beta", [][]any{{"aa", 99.5, true}, {"bb", 2.5, false}, {"cc", 3.5, true}}},
	} {
		tbl, err := db.Table(tc.table)
		if err != nil {
			t.Fatalf("Table(%q): %v", tc.table, err)
		}

		r, err := tbl.Open()
		if err != nil {
			t.Fatalf("%s: Open: %v", tc.table, err)
		}

		checkRows(t, r, tc.rows)
	}
}

// changedPages returns the page numbers whose bytes differ between two
// versions of the same file.
func changedPages(t *testing.T, before, after []byte, pageSize int) []int {
	t.Helper()

	if len(before) != len(after) {
		t.Fatalf("file length changed from %d to %d", len(before), len(after))
	}

	seen := map[int]bool{}

	var pages []int

	for i := range before {
		if before[i] == after[i] {
			continue
		}

		page := i / pageSize
		if !seen[page] {
			seen[page] = true

			pages = append(pages, page)
		}
	}

	return pages
}

// TestTableInfoCountMatchesRows checks the located record count against the
// rows actually present, for every table of every fixture.
//
// It is the evidence that the counters sit at the end of the table info
// structure rather than at a fixed offset. Read at fixed offsets 46 and 50 —
// correct only for a four-column table — most of these files appear to keep no
// record count at all, and the ones that do are exactly the four-column ones.
func TestTableInfoCountMatchesRows(t *testing.T) {
	checked := 0

	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)
			defer db.Close()

			for _, tbl := range fixtureTables(t, db) {
				infoPage, err := tbl.infoPageNo()
				if err != nil || infoPage < 0 {
					t.Fatalf("%s: infoPageNo: %v", tbl.Name(), err)
				}

				page, err := db.ReadPage(infoPage)
				if err != nil {
					t.Fatalf("%s: ReadPage(%d): %v", tbl.Name(), infoPage, err)
				}

				payload := page.PageData()

				_, countOff, err := tableInfoOffsets(payload)
				if err != nil {
					t.Fatalf("%s: tableInfoOffsets: %v", tbl.Name(), err)
				}

				stored := int(int32(binary.LittleEndian.Uint32(payload[countOff : countOff+4])))

				reader, err := tbl.Open()
				if err != nil {
					t.Fatalf("%s: Open: %v", tbl.Name(), err)
				}

				rows := 0
				for reader.Next() {
					rows++
				}

				if err := reader.Err(); err != nil {
					t.Fatalf("%s: iterating: %v", tbl.Name(), err)
				}

				if stored != rows {
					t.Errorf("%s: table info says %d records, the reader finds %d", tbl.Name(), stored, rows)
				}

				checked++
			}
		})
	}

	if checked == 0 {
		t.Skip("no fixtures present (testdata/ is not committed)")
	}
}

// TestTableInfoOffsetsFollowColumnCount pins the shape the offsets are derived
// from: int32 ColumnCount, eight bytes per column, then the two counters.
func TestTableInfoOffsetsFollowColumnCount(t *testing.T) {
	for _, cols := range []int{1, 2, 4, 19, 36} {
		stored := 4 + 8*cols + tableInfoTrailerSize

		payload := make([]byte, internalFileHeaderSize+stored)
		payload[0] = internalFileHeaderSize
		binary.LittleEndian.PutUint32(payload[1:5], uint32(stored))
		binary.LittleEndian.PutUint32(payload[5:9], uint32(stored))

		changeOff, countOff, err := tableInfoOffsets(payload)
		if err != nil {
			t.Fatalf("%d columns: %v", cols, err)
		}

		wantChange := internalFileHeaderSize + 4 + 8*cols
		if changeOff != wantChange || countOff != wantChange+4 {
			t.Errorf("%d columns: offsets %d/%d, want %d/%d",
				cols, changeOff, countOff, wantChange, wantChange+4)
		}
	}

	// A four-column table is where the old fixed constants happened to be
	// right, and every write fixture has four columns. Keeping it explicit
	// says why the byte-identity tests could not see the bug.
	payload := make([]byte, internalFileHeaderSize+44)
	payload[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(payload[1:5], 44)

	changeOff, countOff, err := tableInfoOffsets(payload)
	if err != nil {
		t.Fatalf("four columns: %v", err)
	}

	if changeOff != 46 || countOff != 50 {
		t.Errorf("four-column offsets are %d/%d, want 46/50", changeOff, countOff)
	}
}

// TestTableInfoOffsetsRejectMalformed checks that a corrupt header cannot make
// the writer index outside the page.
func TestTableInfoOffsetsRejectMalformed(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"too short for a header", make([]byte, internalFileHeaderSize-1)},
		{
			"no room for the counters",
			func() []byte {
				p := make([]byte, 64)
				p[0] = internalFileHeaderSize
				binary.LittleEndian.PutUint32(p[1:5], 4)

				return p
			}(),
		},
		{
			"declared length runs past the payload",
			func() []byte {
				p := make([]byte, 64)
				p[0] = internalFileHeaderSize
				binary.LittleEndian.PutUint32(p[1:5], 1<<20)

				return p
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tableInfoOffsets(tc.payload)
			if !errors.Is(err, ErrBookkeepingMismatch) {
				t.Errorf("error = %v, want ErrBookkeepingMismatch", err)
			}
		})
	}
}
