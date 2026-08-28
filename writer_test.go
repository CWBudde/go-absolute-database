package absdb

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writableCopy copies a fixture into the test's temporary directory and returns
// the copy's path.
//
// Every write test works on a copy. The fixtures in testdata/ are read-only
// ground truth — most of them are customer files that exist nowhere else — so no
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

// TestWriterRefusesToStrandAnIndex pins the safety rule: both fixtures carry the
// IdxId index, and inserting or deleting a record would leave it describing a
// different set of rows than the table holds.
//
// Writes-idx.abs is the unencrypted half of the pair, so the refusal is shown
// not to depend on the file being encrypted. Its sibling Writes-idx-ins.abs is
// what the engine produced for the insert this test refuses, and is the ground
// truth for implementing index maintenance later.
func TestWriterRefusesToStrandAnIndex(t *testing.T) {
	for _, fixture := range []string{"Employees-Rijndael_128.abs", "Writes-idx.abs"} {
		t.Run(fixture, func(t *testing.T) {
			path := writableCopy(t, fixture)

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

			if err := w.Delete(id); !errors.Is(err, ErrIndexNotMaintained) {
				t.Errorf("Delete on an indexed table: got %v, want ErrIndexNotMaintained", err)
			}

			_, err = w.Insert([]any{int32(9), "Nine", 9.0, true})
			if !errors.Is(err, ErrIndexNotMaintained) {
				t.Errorf("Insert on an indexed table: got %v, want ErrIndexNotMaintained", err)
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
