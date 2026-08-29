package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// TestWriterMaintainsAnEncryptedIndex shows index maintenance is not a
// plaintext-only capability: Employees-Rijndael_128.abs carries a user index in
// an encrypted file, and its index page is written back through the same
// encrypting path a record page is.
//
// It asserts through the rebuild oracle rather than against a fixture, because
// no engine-made pair exists for a write to an encrypted indexed table.
func TestWriterMaintainsAnEncryptedIndex(t *testing.T) {
	path := writableCopy(t, "Employees-Rijndael_128.abs")

	db, err := OpenForWriteWithPassword(path, testPassword)
	if err != nil {
		t.Fatalf("OpenForWriteWithPassword: %v", err)
	}

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id, _ := firstRecord(t, db)

	if err := w.Delete(id); err != nil {
		t.Fatalf("Delete on an encrypted indexed table: %v", err)
	}

	if _, err := w.Insert([]any{int32(9), "Nine", 9.0, true}); err != nil {
		t.Fatalf("Insert on an encrypted indexed table: %v", err)
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	db.Close()

	requireIndexesMatchRebuild(t, path, testPassword)
}

// TestWriterRefusesAnIndexItCannotMaintain pins what stays refused now that
// single-page Int32 indexes are maintained. Each case is a shape no fixture
// captures, so refusing is the only honest answer: guessing at it would produce
// a file that reads back correctly through this package and is not what the
// engine writes.
//
// The first two poke a copy of Writes-idx.abs rather than needing a fixture of
// their own, which is safe because a page checksum is only an encryption
// marker here, never verified on read.
func TestWriterRefusesAnIndexItCannotMaintain(t *testing.T) {
	t.Run("tree deeper than one page", func(t *testing.T) {
		// Clearing IsLeaf turns the root into an internal node, which is what
		// a split would have produced.
		path := pokeIndexLeaf(t, "Writes-idx.abs", func(payload []byte) {
			payload[1] = 0
		})

		requireWriteRefusal(t, path, ErrIndexNotMaintained)
	})

	t.Run("leaf with no room for another entry", func(t *testing.T) {
		// EntryCount raised to exactly what the page holds, so the next insert
		// is the one the engine would split on.
		path := pokeIndexLeaf(t, "Writes-idx.abs", func(payload []byte) {
			capacity := (len(payload) - btreeHeaderSize) / (indexKeySize + leafEntrySuffixSize)
			binary.LittleEndian.PutUint16(payload[14:16], uint16(capacity))
		})

		db, err := OpenForWrite(path)
		if err != nil {
			t.Fatalf("OpenForWrite: %v", err)
		}

		defer db.Close()

		w, err := db.OpenTableWriter()
		if err != nil {
			t.Fatalf("OpenTableWriter: %v", err)
		}

		if _, err := w.Insert([]any{int32(9), "Nine", 9.0, true}); !errors.Is(err, ErrIndexTooManyRows) {
			t.Errorf("Insert into a full index leaf: got %v, want ErrIndexTooManyRows", err)
		}
	})

	t.Run("index shape the leaf writer cannot reproduce", func(t *testing.T) {
		// The two refusals maintainableIndexColumn adds on top of the leaf
		// shape, checked against hand-built records because no committed
		// fixture reaches them: the private fixtures that carry these index
		// shapes all declare a key constraint too, and the constraint gate
		// stops their writes first (see TestWriterRefusesAConstrainedTable).
		//
		// A PRIMARY or UNIQUE flag used to be the third. It is not a refusal
		// any more -- the Keys*.abs fixtures show the engine splicing such a
		// leaf exactly as it splices a plain one -- so what the flag selects
		// now is the duplicate check, which TestKeyIndexRefusesADuplicate
		// covers.
		for _, c := range []struct {
			name string
			rec  indexRecord
			want error
		}{
			{
				name: "more than one column",
				rec:  indexRecord{name: "RecIdx", columns: []indexColumn{{name: "A"}, {name: "B"}}},
				want: ErrMultiColumnIndex,
			},
			{
				name: "DESC or NOCASE column",
				rec:  indexRecord{name: "d", columns: []indexColumn{{name: "A", descending: true}}},
				want: ErrIndexNotMaintained,
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				if _, err := maintainableIndexColumn(c.rec); !errors.Is(err, c.want) {
					t.Errorf("maintainableIndexColumn(%q) = %v, want %v", c.rec.name, err, c.want)
				}
			})
		}
	})
}

// TestWriterRefusesAConstrainedTable pins the gate that sits in front of index
// maintenance: a table declaring a constraint this package does not check
// refuses every write, because a write that ignores one leaves the file
// holding a row the engine would have rejected.
//
// What is refused has narrowed twice. NOT NULL and MINVALUE/MAXVALUE are
// checked (writer_constraint.go), so CNotNull and CMinMax accept writes and
// are tested for what they reject in TestWriterChecksConstraints; a PRIMARY
// KEY or UNIQUE clause over a single Int32 column is enforced by its own index
// now (checkKeyIndexes), so CPk and CUnique accept writes too.
//
// What is left refuses because of the index rather than the record, and that
// is the point of the two remaining cases: CBoth's UNIQUE is on a VARCHAR
// column and CPkMulti's key covers two, so neither index is one this package
// can splice. The refusal now comes from index resolution, which runs first --
// a constraint whose index cannot be maintained is not separately reported as
// unchecked, because the index is what would have checked it.
//
// Constraints.abs isolates one clause per table, so each case names exactly
// which one stopped the write. CNone is the control: the same file, the same
// writer, no constraint, and the write is not refused.
func TestWriterRefusesAConstrainedTable(t *testing.T) {
	for _, c := range []struct {
		table string
		want  error
	}{
		{"CNone", nil},
		{"CDefault", nil},
		{"CNotNull", nil},
		{"CMinMax", nil},
		{"CPk", nil},
		{"CUnique", nil},
		{"CBoth", ErrIndexNotMaintained},
		{"CPkMulti", ErrMultiColumnIndex},
	} {
		t.Run(c.table, func(t *testing.T) {
			path := writableCopy(t, "Constraints.abs")

			db, err := OpenForWrite(path)
			if err != nil {
				t.Fatalf("OpenForWrite: %v", err)
			}

			defer db.Close()

			tbl, err := db.Table(c.table)
			if err != nil {
				t.Fatalf("Table(%q): %v", c.table, err)
			}

			w, err := tbl.OpenWriter()
			if err != nil {
				t.Fatalf("OpenWriter(%q): %v", c.table, err)
			}

			_, err = w.maintainedIndexes()

			switch {
			case c.want == nil && err != nil:
				t.Errorf("writing %q = %v, want it to be allowed", c.table, err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("writing %q = %v, want %v", c.table, err, c.want)
			}
		})
	}
}

// requireWriteRefusal fails unless insert, delete and an update of the first
// column are all refused with want.
//
// Update is included deliberately. It used to go through on a table like this
// and leave the index describing a key the row no longer had; refusing it is
// the behaviour change index maintenance brought with it.
func requireWriteRefusal(t *testing.T, path string, want error) {
	t.Helper()

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	id, _ := firstRecord(t, db)

	if err := w.Delete(id); !errors.Is(err, want) {
		t.Errorf("Delete: got %v, want %v", err, want)
	}

	// All-NULL, one per column, so that the refusal under test is reached
	// rather than a column-count error: these fixtures range from four columns
	// to thirty-six.
	if _, err := w.Insert(make([]any, len(w.Schema().Columns))); !errors.Is(err, want) {
		t.Errorf("Insert: got %v, want %v", err, want)
	}

	if err := w.UpdateColumn(id, 0, int32(9)); !errors.Is(err, want) {
		t.Errorf("UpdateColumn: got %v, want %v", err, want)
	}
}

// pokeIndexLeaf copies a fixture and rewrites its user index leaf through edit,
// returning the copy's path. It finds the leaf rather than hardcoding a page
// number, so it keeps working if a fixture is ever regenerated.
func pokeIndexLeaf(t *testing.T, fixture string, edit func(payload []byte)) string {
	t.Helper()

	path := writableCopy(t, fixture)

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}

	user := ir.UserIndexes()
	if len(user) != 1 {
		t.Fatalf("%s has %d user indexes, want 1", fixture, len(user))
	}

	buf, err := db.bufferPage(user[0].RootPageNo)
	if err != nil {
		t.Fatalf("bufferPage(%d): %v", user[0].RootPageNo, err)
	}

	edit(buf.payload)

	buf.dirty = true
	buf.stateBump = 0

	if err := db.writePageBuf(buf); err != nil {
		t.Fatalf("writePageBuf: %v", err)
	}

	db.Close()

	return path
}

// requireIndexesMatchRebuild is the oracle that reaches past the fixtures:
// every maintained index must hold exactly the entries CREATE INDEX would build
// from the table's rows as they now stand.
//
// It compares entries rather than page bytes, because a maintained leaf is
// deliberately not byte-identical to a rebuilt one: a removal leaves the slot it
// vacated untouched, so the bytes past EntryCount are stale by design.
func requireIndexesMatchRebuild(t *testing.T, path, password string) {
	t.Helper()

	db, err := OpenForWriteWithPassword(path, password)
	if err != nil {
		t.Fatalf("reopening %s: %v", path, err)
	}

	defer db.Close()

	table, err := db.Table("")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}

	w, err := table.OpenWriter()
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	defer w.Close()

	indexes, err := w.maintainedIndexes()
	if err != nil {
		t.Fatalf("maintainedIndexes: %v", err)
	}

	if len(indexes) == 0 {
		t.Fatal("no maintained index to check")
	}

	for _, idx := range indexes {
		page, err := db.ReadPage(idx.rootPageNo)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", idx.rootPageNo, err)
		}

		hdr, err := parseBTreeHeader(page.PageData())
		if err != nil {
			t.Fatalf("parseBTreeHeader: %v", err)
		}

		got, err := readBTreeEntries(page.PageData(), hdr)
		if err != nil {
			t.Fatalf("readBTreeEntries: %v", err)
		}

		want, err := db.buildIndexLeafEntries(table, idx.colIdx)
		if err != nil {
			t.Fatalf("buildIndexLeafEntries: %v", err)
		}

		if len(got) != len(want) {
			t.Fatalf("index %q holds %d entries, a rebuild from the rows gives %d",
				idx.name, len(got), len(want))
		}

		for i := range got {
			if !bytes.Equal(got[i].Key, want[i].Key) || got[i].PageNo != want[i].PageNo || got[i].ItemNo != want[i].ItemNo {
				t.Errorf("index %q entry %d is key %x -> page %d slot %d, a rebuild gives key %x -> page %d slot %d",
					idx.name, i, got[i].Key, got[i].PageNo, got[i].ItemNo,
					want[i].Key, want[i].PageNo, want[i].ItemNo)
			}
		}
	}
}

// TestWriterIndexMatchesRebuildAfterEditSequences is the coverage the four
// engine fixtures cannot give: they pin one splice each, at one position each.
// This drives long mixed sequences of inserts, deletes and key-moving updates
// through the leaf and requires the result, after every transaction, to hold
// exactly the entries CREATE INDEX would build from the rows as they then
// stand.
//
// The oracle is worth more than its brevity suggests. buildIndexLeafEntries is
// itself pinned byte for byte against Writes-idx.abs by
// TestCreateIndexMatchesEngineByteForByte, so agreeing with it is agreeing with
// the engine's own idea of what the leaf should contain -- reached by splicing
// rather than by rebuilding.
//
// The sequences are fixed rather than random, so a failure names one case
// instead of one seed.
func TestWriterIndexMatchesRebuildAfterEditSequences(t *testing.T) {
	type edit struct {
		op  string
		key int32
		to  int32
	}

	cases := []struct {
		name  string
		edits []edit
	}{
		{
			// Insert at the front, the middle and the back of the array in one
			// transaction, so every shift distance is exercised at once.
			name: "insert around the existing keys",
			edits: []edit{
				{op: "insert", key: 0},
				{op: "insert", key: 2},
				{op: "insert", key: 7},
			},
		},
		{
			name: "delete every row, then refill",
			edits: []edit{
				{op: "delete", key: 1},
				{op: "delete", key: 2},
				{op: "delete", key: 3},
				{op: "insert", key: 5},
				{op: "insert", key: 4},
			},
		},
		{
			// Moving a key forwards and backwards past its neighbours, which is
			// the removal and reinsertion Writes-idx-upd.abs pins once.
			name: "move keys across each other",
			edits: []edit{
				{op: "update", key: 1, to: 99},
				{op: "update", key: 3, to: 2},
				{op: "update", key: 99, to: 0},
			},
		},
		{
			name: "delete and reinsert the same key repeatedly",
			edits: []edit{
				{op: "delete", key: 2},
				{op: "insert", key: 2},
				{op: "delete", key: 2},
				{op: "insert", key: 2},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writableCopy(t, "Writes-idx.abs")

			for _, e := range c.edits {
				applyEdit(t, path, e.op, e.key, e.to)

				// After every single transaction, not just at the end: a leaf
				// that only agrees once the whole sequence has run would hide
				// an error that a later edit happens to undo.
				requireIndexesMatchRebuild(t, path, "")
			}
		})
	}
}

// applyEdit opens path, runs one edit as its own transaction and commits.
func applyEdit(t *testing.T, path, op string, key, to int32) {
	t.Helper()

	db, err := OpenForWrite(path)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	defer db.Close()

	w, err := db.OpenTableWriter()
	if err != nil {
		t.Fatalf("OpenTableWriter: %v", err)
	}

	switch op {
	case "insert":
		if _, err := w.Insert([]any{key, "row", 1.0, true}); err != nil {
			t.Fatalf("Insert(%d): %v", key, err)
		}
	case "delete":
		if err := w.Delete(recordWithKey(t, db, key)); err != nil {
			t.Fatalf("Delete(%d): %v", key, err)
		}
	case "update":
		if err := w.UpdateColumn(recordWithKey(t, db, key), 0, to); err != nil {
			t.Fatalf("UpdateColumn(%d -> %d): %v", key, to, err)
		}
	default:
		t.Fatalf("unknown edit %q", op)
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
