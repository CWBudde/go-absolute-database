package absdb

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// schemaStreamOf returns the decompressed column-definition stream of a
// table's schema page, for comparing CREATE/DROP INDEX's output against the
// engine's own stream byte for byte.
func schemaStreamOf(t *testing.T, db *File, table string) []byte {
	t.Helper()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	no, err := tbl.schemaPageNo()
	if err != nil {
		t.Fatalf("schemaPageNo: %v", err)
	}

	raw, err := db.readSchemaStream(no)
	if err != nil {
		t.Fatalf("readSchemaStream: %v", err)
	}

	return raw
}

// schemaPageBytesOf returns the raw compressed page bytes holding a table's
// schema, for a re-compression byte check.
func schemaPageBytesOf(t *testing.T, path string, db *File, table string) []byte {
	t.Helper()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	no, err := tbl.schemaPageNo()
	if err != nil {
		t.Fatalf("schemaPageNo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	start := no * db.PageSize()

	return data[start+pageDataOffset : start+db.PageSize()]
}

// TestCreateIndexReproducesTheEngineStream is the strongest check available:
// Writes-idx.abs is the engine's own output for exactly
// "CREATE INDEX IdxId ON Writes (Id)" against Writes.abs, so both the
// decompressed schema stream and the recompressed page bytes must match it
// exactly.
func TestCreateIndexReproducesTheEngineStream(t *testing.T) {
	wantPath := requireFixture(t, "Writes-idx.abs")

	want, err := Open(wantPath)
	if err != nil {
		t.Fatalf("Open(Writes-idx.abs): %v", err)
	}
	defer want.Close()

	wantStream := schemaStreamOf(t, want, "Writes")
	wantPageBytes := schemaPageBytesOf(t, wantPath, want, "Writes")

	path := writableCopy(t, "Writes.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}
	defer db.Close()

	if err := db.CreateIndex("Writes", "IdxId", "Id"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	gotStream := schemaStreamOf(t, db, "Writes")
	if !bytes.Equal(gotStream, wantStream) {
		t.Errorf("decompressed schema stream differs from Writes-idx.abs's:\ngot:  %x\nwant: %x", gotStream, wantStream)
	}

	gotPageBytes := schemaPageBytesOf(t, path, db, "Writes")
	if !bytes.Equal(gotPageBytes, wantPageBytes) {
		t.Errorf("recompressed schema page bytes differ from Writes-idx.abs's:\ngot:  %x\nwant: %x", gotPageBytes, wantPageBytes)
	}
}

// TestDropIndexReproducesTheEngineStream is TestCreateIndexReproducesTheEngineStream
// in reverse: dropping IdxId from Writes-idx.abs must return the stream to
// exactly what Writes.abs (no index) carries.
func TestDropIndexReproducesTheEngineStream(t *testing.T) {
	wantPath := requireFixture(t, "Writes.abs")

	want, err := Open(wantPath)
	if err != nil {
		t.Fatalf("Open(Writes.abs): %v", err)
	}
	defer want.Close()

	wantStream := schemaStreamOf(t, want, "Writes")

	path := writableCopy(t, "Writes-idx.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}
	defer db.Close()

	if err := db.DropIndex("Writes", "IdxId"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}

	gotStream := schemaStreamOf(t, db, "Writes")
	if !bytes.Equal(gotStream, wantStream) {
		t.Errorf("decompressed schema stream differs from Writes.abs's:\ngot:  %x\nwant: %x", gotStream, wantStream)
	}
}

// TestCreateIndexMatchesEngineByteForByte holds CREATE INDEX to the same
// whole-file standard as TestCreateTableMatchesEngineByteForByte, but pins it
// harder: it excludes the 4-byte ABSP State word of EVERY page in the file,
// not just the newly allocated one, and requires zero bytes to differ beyond
// that.
//
// The wider exclusion is what a direct diff of Writes.abs against
// Writes-idx.abs earns. Masked the same way, the two engine-written files
// differ by exactly 77 bytes on exactly three pages: page 0 (LastUsedPageNo,
// the file's own State, LastObjectID, and the PFS byte carrying page 11's
// bit), page 7 (the schema stream -- already pinned exactly by
// TestCreateIndexReproducesTheEngineStream) and page 11 (the newly allocated
// index page). Every other page's payload is byte-identical between the two
// files even though several of them -- pages 2, 3, 4, 9 and 10 -- carry a
// different State. An untouched page cannot legitimately have a different
// State, so the engine evidently reseeds the State of every page it rewrites
// for CREATE INDEX, rather than incrementing it, and it rewrites the whole
// file to do so. (CREATE TABLE behaves differently: docs/writing.md and
// TestCreateTableMatchesEngineByteForByte's own comment record existing
// pages' States incrementing there -- +1 on page 4, +5 on page 0 -- with only
// the newly allocated pages reseeded. Different operations, different
// State-update behaviour.)
//
// So excluding every page's State word and requiring zero further
// differences is not a weaker check than the single-page exclusion -- it is
// the same content requirement (this package need not reproduce a State
// value it cannot predict, new page or old) applied honestly to the whole
// file, including the five pages CreateIndex never touches at all.
func TestCreateIndexMatchesEngineByteForByte(t *testing.T) {
	want, err := os.ReadFile(requireFixture(t, "Writes-idx.abs"))
	if err != nil {
		t.Fatalf("reading Writes-idx.abs: %v", err)
	}

	path := writableCopy(t, "Writes.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	if err := db.CreateIndex("Writes", "IdxId", "Id"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	pageSize := db.PageSize()
	pageCount := db.PageCount()

	db.Close()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	excluded := make(map[int]bool, pageCount*4)

	for no := range pageCount {
		start := no*pageSize + pageStateOffset
		for i := range 4 {
			excluded[start+i] = true
		}
	}

	reportByteDifferencesExcept(t, got, want, "CREATE INDEX IdxId ON Writes (Id)", excluded)
}

// TestCreateIndexReadsBack checks the index works as an index, not just that
// its bytes look right: it is visible through OpenIndex, and every row of the
// table is found by FindByPrimaryKey through it.
func TestCreateIndexReadsBack(t *testing.T) {
	path := writableCopy(t, "Writes.abs")

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}
	defer db.Close()

	if err := db.CreateIndex("Writes", "IdxId", "Id"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	table, err := db.Table("Writes")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	ir, err := table.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}

	// The new index's key is [null flag byte]+int32, the same shape as
	// primaryKeySize, so IndexReader classifies it as the primary key index
	// (index.go's keySize heuristic) rather than a secondary one.
	userIndexes := ir.UserIndexes()
	if len(userIndexes) == 0 {
		t.Fatal("OpenIndex reports no user index after CreateIndex")
	}

	r, err := table.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	found := 0

	for r.Next() {
		rec := r.Record()
		id := rec.Int(0) // Id is column 0

		entry, err := ir.searchTree(userIndexes[0].RootPageNo, primaryKeyOf(id), compareInt32Keys)
		if err != nil {
			t.Errorf("row Id=%d not found through the new index: %v", id, err)
			continue
		}

		wantID, ok := r.RecordID()
		if !ok {
			t.Fatalf("RecordID unavailable for Id=%d", id)
		}

		if int(entry.PageNo) != wantID.PageNo || int(entry.ItemNo) != wantID.Slot {
			t.Errorf("row Id=%d: index points at page %d slot %d, want page %d slot %d",
				id, entry.PageNo, entry.ItemNo, wantID.PageNo, wantID.Slot)
		}

		found++
	}

	if err := r.Err(); err != nil {
		t.Fatalf("reading rows: %v", err)
	}

	if found == 0 {
		t.Fatal("no rows read from Writes")
	}
}

// primaryKeyOf builds an int32 index search key the way FindByPrimaryKey does.
func primaryKeyOf(v int32) []byte {
	key := make([]byte, primaryKeySize)

	buf := key[1:]
	for i := range 4 {
		buf[i] = byte(uint32(v) >> (8 * i))
	}

	return key
}

// TestIndexRefusals checks that every boundary CreateIndex and DropIndex
// document is an error rather than a silently wrong file.
func TestIndexRefusals(t *testing.T) {
	t.Run("CreateIndex read-only", func(t *testing.T) {
		db := openTestFile(t, "Writes.abs")
		defer db.Close()

		if err := db.CreateIndex("Writes", "IdxId", "Id"); !errors.Is(err, ErrReadOnly) {
			t.Errorf("CreateIndex on a read-only file: %v, want ErrReadOnly", err)
		}
	})

	t.Run("DropIndex read-only", func(t *testing.T) {
		db := openTestFile(t, "Writes-idx.abs")
		defer db.Close()

		if err := db.DropIndex("Writes", "IdxId"); !errors.Is(err, ErrReadOnly) {
			t.Errorf("DropIndex on a read-only file: %v, want ErrReadOnly", err)
		}
	})

	t.Run("CreateIndex on an unknown table", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		if err := db.CreateIndex("Nowhere", "IdxId", "Id"); !errors.Is(err, ErrNoSuchTable) {
			t.Errorf("CreateIndex on an unknown table: %v, want ErrNoSuchTable", err)
		}
	})

	t.Run("DropIndex on an unknown table", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes-idx.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		if err := db.DropIndex("Nowhere", "IdxId"); !errors.Is(err, ErrNoSuchTable) {
			t.Errorf("DropIndex on an unknown table: %v, want ErrNoSuchTable", err)
		}
	})

	t.Run("CreateIndex with a name that already exists", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes-idx.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		if err := db.CreateIndex("Writes", "IdxId", "Id"); !errors.Is(err, ErrIndexExists) {
			t.Errorf("CreateIndex with an existing name: %v, want ErrIndexExists", err)
		}
	})

	t.Run("DropIndex on a name that does not exist", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		if err := db.DropIndex("Writes", "Nowhere"); !errors.Is(err, ErrNoSuchIndex) {
			t.Errorf("DropIndex of an unknown index: %v, want ErrNoSuchIndex", err)
		}
	})

	t.Run("CreateIndex on a column that does not exist", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		if err := db.CreateIndex("Writes", "IdxNowhere", "Nowhere"); !errors.Is(err, ErrNoSuchColumn) {
			t.Errorf("CreateIndex on an unknown column: %v, want ErrNoSuchColumn", err)
		}
	})

	t.Run("CreateIndex on a column CreateIndex has no corpus evidence for", func(t *testing.T) {
		db, err := OpenForWrite(writableCopy(t, "Writes.abs"))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		// Name is a Varchar/String column, not the only supported Int32/Integer
		// shape (see ErrUnsupportedIndexColumn).
		if err := db.CreateIndex("Writes", "IdxName", "Name"); !errors.Is(err, ErrUnsupportedIndexColumn) {
			t.Errorf("CreateIndex on a String column: %v, want ErrUnsupportedIndexColumn", err)
		}
	})

	t.Run("CreateIndex on a table whose schema stream carries constraints", func(t *testing.T) {
		// This used to be an ErrSchemaTailNotUnderstood refusal, and it covered
		// every indexed private fixture. Decoding the constraint array
		// (ddl_constraint.go) retired it: RCFQ0011.abs carries six NOT NULL
		// records and a PRIMARY KEY, and all of them now parse.
		//
		// What refuses this particular table now is its 600 rows, which is a
		// different and much narrower thing -- and the point of the assertion:
		// the tail is no longer what stands in the way.
		// TestCreateIndexPreservesConstraintRecords is where the operation is
		// carried through on a constrained table and the records checked byte
		// for byte.
		db, err := OpenForWrite(writableCopy(t, requireFixtureName(t, "RCFQ0011.abs")))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		err = db.CreateIndex("RCFQ0011.abs", "IdxScratch", "RecNo")
		if errors.Is(err, ErrSchemaTailNotUnderstood) {
			t.Errorf("CreateIndex on a table with constraint records: %v, want the tail to be read", err)
		}

		if !errors.Is(err, ErrIndexTooManyRows) {
			t.Errorf("CreateIndex over 600 rows: %v, want ErrIndexTooManyRows", err)
		}

		_, _, records, constraints, _, err := parseSchemaTail(schemaStreamOf(t, db, "RCFQ0011.abs"))
		if err != nil {
			t.Fatalf("parseSchemaTail: %v", err)
		}

		if len(records) != 1 || len(constraints) != 7 {
			t.Errorf("got %d index and %d constraint record(s), want 1 and 7", len(records), len(constraints))
		}
	})

	t.Run("DropIndex of the index behind a PRIMARY KEY", func(t *testing.T) {
		// RCFQ0011.abs's one index is "p", the index its PRIMARY KEY
		// constraint is built on. Dropping it would leave the constraint
		// naming an index the file no longer has.
		db, err := OpenForWrite(writableCopy(t, requireFixtureName(t, "RCFQ0011.abs")))
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}
		defer db.Close()

		if err := db.DropIndex("RCFQ0011.abs", "p"); !errors.Is(err, ErrIndexBacksConstraint) {
			t.Errorf("DropIndex of a primary key's index: %v, want ErrIndexBacksConstraint", err)
		}

		if err := db.DropIndex("RCFQ0011.abs", "FrqNo"); !errors.Is(err, ErrNoSuchIndex) {
			t.Errorf("DropIndex of a name that is a column, not an index: %v, want ErrNoSuchIndex", err)
		}
	})

	t.Run("nothing is written when CreateIndex is refused", func(t *testing.T) {
		path := writableCopy(t, "Writes.abs")
		before := fileDigest(t, path)

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		_ = db.CreateIndex("Nowhere", "IdxId", "Id")
		_ = db.CreateIndex("Writes", "IdxId", "Nowhere")

		db.Close()

		if fileDigest(t, path) != before {
			t.Error("a refused CreateIndex changed the file")
		}
	})
}

// TestCreateThenDropIndexRestoresTheStream round-trips CreateIndex and
// DropIndex on Writes.abs and requires the schema stream to return to exactly
// what it started as.
func TestCreateThenDropIndexRestoresTheStream(t *testing.T) {
	path := writableCopy(t, "Writes.abs")

	before, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	beforeStream := schemaStreamOf(t, before, "Writes")
	before.Close()

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}
	defer db.Close()

	if err := db.CreateIndex("Writes", "IdxScratch", "Id"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	if err := db.DropIndex("Writes", "IdxScratch"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}

	afterStream := schemaStreamOf(t, db, "Writes")
	if !bytes.Equal(afterStream, beforeStream) {
		t.Errorf("schema stream after create-then-drop differs from the original:\ngot:  %x\nwant: %x", afterStream, beforeStream)
	}
}
