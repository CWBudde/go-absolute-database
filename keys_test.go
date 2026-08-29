package absdb

import (
	"bytes"
	"encoding/binary"
	"os"
	"sort"
	"testing"
)

// What the Keys*.abs and Auto*.abs fixtures settle.
//
// Every write path in this package was held to a plain index, because all four
// Writes-idx* files carry one: a leaf splice on a UNIQUE or PRIMARY index had
// no byte identity behind it, so index maintenance refused such an index and
// the constraints built on it refused every write. These fixtures are the
// missing evidence, and the tests here read them before anything writes them.
//
// The table is Keys -- Id INTEGER PRIMARY KEY, Alt INTEGER, Name VARCHAR(20)
// -- with three rows, so its primary key's backing index is the
// single-column, ascending, case-sensitive, Int32 shape CREATE INDEX builds,
// and its leaf holds entries rather than being empty the way Constraints.abs's
// CPk is. Auto is the same idea over an AUTOINC column, which is what fifteen
// of the corpus's twenty-five key constraints actually are.
const (
	keysFixture         = "Keys.abs"
	keysUniqueIdxFixure = "Keys-uniqidx.abs"
	autoFixture         = "Auto.abs"
)

// TestKeysPrimaryKeyIndexRecord pins the index record a CREATE TABLE ...
// PRIMARY KEY clause writes: the third flag byte set, the second clear, one
// covered column, ascending and case-sensitive. Constraints.abs's CPk already
// showed those bytes; what is new is that the same record here belongs to an
// index whose leaf holds rows.
func TestKeysPrimaryKeyIndexRecord(t *testing.T) {
	db := openFixture(t, keysFixture)
	defer db.Close()

	_, _, records, constraints := tailOf(t, db, "Keys")

	if len(records) != 1 || len(constraints) != 1 {
		t.Fatalf("Keys carries %d index and %d constraint records, want 1 and 1", len(records), len(constraints))
	}

	rec := records[0]
	if rec.name != "C_PK$Id" || !rec.primary || rec.unique {
		// The engine's generated name, PRIMARY set, UNIQUE clear: a primary key
		// is not spelled as "unique and primary" by DBManager's CREATE TABLE,
		// even though a private fixture's own "p" index sets both.
		t.Errorf("index record: name=%q unique=%t primary=%t, want %q false true",
			rec.name, rec.unique, rec.primary, "C_PK$Id")
	}

	col, ok := rec.singleColumn()
	if !ok {
		t.Fatalf("index %q covers %d columns, want 1", rec.name, len(rec.columns))
	}

	if col.name != "Id" || col.descending || col.caseInsensitive || col.maxIndexedSize != indexColumnMaxIndexedSize {
		t.Errorf("covered column = %+v, want Id ascending case-sensitive max %d", col, indexColumnMaxIndexedSize)
	}

	// The constraint hangs off the index, not off the column: this is what the
	// checker has to resolve before it can call a key enforceable.
	if c := constraints[0]; c.kind != constraintPrimaryKey || c.ownerID != rec.objectID || c.index != rec.name {
		t.Errorf("constraint = %s %q owner=%d index=%q, want PRIMARY KEY owned by index %d %q",
			c.kind, c.name, c.ownerID, c.index, rec.objectID, rec.name)
	}
}

// TestKeysPrimaryKeyDoesNotImplyANotNullRecord records the negative that
// matters for the checker: a PRIMARY KEY column carries no NOT NULL constraint
// record, yet the engine still refuses a NULL in it ("Constraint 'C_PK$Id'
// violated. Value in field 'Id' cannot be null", native error 30330, and the
// file came back byte-identical). So a writer that only consults the constraint
// array would let a NULL primary key through.
func TestKeysPrimaryKeyDoesNotImplyANotNullRecord(t *testing.T) {
	db := openFixture(t, keysFixture)
	defer db.Close()

	for _, c := range constraintsOf(t, db, "Keys") {
		if c.kind == constraintNotNull {
			t.Errorf("Keys carries a NOT NULL record %q; the fixture was made without one", c.name)
		}
	}

	schema := schemaOfTable(t, db, "Keys")
	if notNull, known := schema.Columns[0].NotNull(); notNull || !known {
		t.Errorf("Keys.Id: NotNull = %t, known = %t, want false/true", notNull, known)
	}
}

// TestCreateUniqueIndexWritesAConstraintToo is the finding that decides what
// CreateUniqueIndex has to write. CREATE UNIQUE INDEX IdxAlt ON Keys (Alt) is
// not just an index record with a flag set: the engine also writes a UNIQUE
// constraint record named after the column, and hands out two object ids
// rather than one.
//
// That record's table name is empty, which nothing else in the corpus is --
// a CREATE TABLE ... PRIMARY KEY record names its table. Refusing an empty
// name is what made this record unwritable before encodeOptionalPascalName.
func TestCreateUniqueIndexWritesAConstraintToo(t *testing.T) {
	db := openFixture(t, keysUniqueIdxFixure)
	defer db.Close()

	_, _, records, constraints := tailOf(t, db, "Keys")

	if len(records) != 2 || len(constraints) != 2 {
		t.Fatalf("Keys carries %d index and %d constraint records, want 2 and 2", len(records), len(constraints))
	}

	idx := records[1]
	if idx.name != "IdxAlt" || !idx.unique || idx.primary {
		t.Errorf("index record: name=%q unique=%t primary=%t, want IdxAlt true false", idx.name, idx.unique, idx.primary)
	}

	con := constraints[1]
	if con.kind != constraintUnique || con.name != "C_Unique$Alt" || con.index != "IdxAlt" || con.ownerID != idx.objectID {
		t.Errorf("constraint = %s %q index=%q owner=%d, want UNIQUE C_Unique$Alt on IdxAlt owned by %d",
			con.kind, con.name, con.index, con.ownerID, idx.objectID)
	}

	if con.table != "" {
		t.Errorf("constraint %q names table %q, want the empty name the engine wrote", con.name, con.table)
	}

	// Two ids in sequence, the index's then the constraint's, the same order
	// CREATE TABLE hands them out in.
	if con.objectID != idx.objectID+1 {
		t.Errorf("object ids: index %d, constraint %d, want consecutive", idx.objectID, con.objectID)
	}
}

// TestKeyIndexLeafIsTheSameShapeAsAPlainOne is what lets index maintenance
// treat a key index like any other: the engine splices a key-enforcing leaf
// exactly as it splices a plain one. Each case is one statement away from
// Keys.abs, and the expected keys are the whole of the leaf's meaning.
//
// Keys-del is the case that would catch a "tidier" implementation: the engine
// drops the count to two and leaves the vacated third entry's bytes in place,
// which is the same behaviour Writes-idx-del.abs pins for a plain index.
func TestKeyIndexLeafIsTheSameShapeAsAPlainOne(t *testing.T) {
	for _, c := range []struct {
		fixture string
		keys    []int32
		trailer []int32 // keys still readable past EntryCount
	}{
		{fixture: "Keys.abs", keys: []int32{1, 2, 3}},
		{fixture: "Keys-ins.abs", keys: []int32{1, 2, 3, 4}},
		{fixture: "Keys-ins0.abs", keys: []int32{0, 1, 2, 3}},
		{fixture: "Keys-del.abs", keys: []int32{1, 3}, trailer: []int32{3}},
		{fixture: "Keys-upd.abs", keys: []int32{1, 3, 9}},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			db := openFixture(t, c.fixture)
			defer db.Close()

			keys, trailer := leafKeys(t, db, "Keys", 0)

			if !equalInt32(keys, c.keys) {
				t.Errorf("leaf keys = %v, want %v", keys, c.keys)
			}

			if len(c.trailer) > 0 && !equalInt32(trailer[:len(c.trailer)], c.trailer) {
				t.Errorf("bytes past EntryCount decode as %v, want the vacated entry %v", trailer, c.trailer)
			}
		})
	}
}

// TestUniqueIndexAdmitsANull is the one place a UNIQUE index and a PRIMARY KEY
// differ, and it is not guessable: the engine accepted INSERT INTO Keys VALUES
// (7, NULL, 'Nul1') into a table whose Alt column carries a UNIQUE index, and
// stored the entry with the null flag set, sorting ahead of every value. The
// same statement against the PRIMARY KEY column was refused.
func TestUniqueIndexAdmitsANull(t *testing.T) {
	db := openFixture(t, "Keys-uniqnull.abs")
	defer db.Close()

	page, count := leafPage(t, db, "Keys", 1)

	if count != 4 {
		t.Fatalf("IdxAlt holds %d entries, want 4", count)
	}

	if got := page[btreeHeaderSize]; got != 1 {
		t.Errorf("first entry's null flag = %d, want 1 (the NULL sorts first)", got)
	}
}

// TestAutoIncKeyIndexHasTheSameShape carries the survey's finding into a test.
// Fifteen of the corpus's twenty-five key constraints are backed by an index
// over an AUTOINC column, and this package refuses those on field type rather
// than on anything about the key. Auto.abs says the refusal is about the
// column, not the index: the record and the leaf are the Int32 shape exactly.
func TestAutoIncKeyIndexHasTheSameShape(t *testing.T) {
	db := openFixture(t, autoFixture)
	defer db.Close()

	records := indexRecordsOf(t, db, "Auto")

	if len(records) != 1 {
		t.Fatalf("Auto carries %d index records, want 1", len(records))
	}

	col, ok := records[0].singleColumn()
	if !ok || col.descending || col.caseInsensitive {
		t.Fatalf("index covers %+v, want one ascending case-sensitive column", records[0].columns)
	}

	schema := schemaOfTable(t, db, "Auto")
	if c := schema.Columns[0]; c.BaseType != BftInt32 || c.FieldType != FieldAutoInc {
		t.Errorf("Auto.Id is base type %d / %s, want %d / AutoInc", c.BaseType, c.FieldType, BftInt32)
	}

	keys, _ := leafKeys(t, db, "Auto", 0)
	if !equalInt32(keys, []int32{1, 2, 3}) {
		t.Errorf("leaf keys = %v, want [1 2 3]", keys)
	}
}

// TestAutoIncInsertWritesNoExtraPage is why Auto-ins.abs exists. An AUTOINC
// column's next value has to come from somewhere, and if the engine kept a
// counter of its own then inserting a row would touch a page an Int32-keyed
// insert does not -- which would be a reason to keep refusing an AutoInc
// index even after a key index is maintained. It does not: the two inserts
// touch the same page types, so the next value is evidently derived rather
// than stored.
func TestAutoIncInsertWritesNoExtraPage(t *testing.T) {
	auto := changedPageTypes(t, autoFixture, "Auto-ins.abs")
	keys := changedPageTypes(t, keysFixture, "Keys-ins.abs")

	if !equalStrings(auto, keys) {
		t.Errorf("an AUTOINC insert touches page types %v, an Int32 insert %v", auto, keys)
	}
}

// indexRecordsOf is tailOf reduced to the index array, the sibling of
// constraintsOf in ddl_constraint_write_test.go.
func indexRecordsOf(t *testing.T, db *File, name string) []indexRecord {
	t.Helper()

	_, _, records, _ := tailOf(t, db, name) //nolint:dogsled // tailOf returns four results and this caller needs one

	return records
}

// leafPage returns the payload and entry count of the n-th user index's root
// page, checking it is the single root-and-leaf page every fixture here has.
func leafPage(t *testing.T, db *File, table string, n int) ([]byte, int) {
	t.Helper()

	tbl, err := db.Table(table)
	if err != nil {
		t.Fatalf("Table(%q): %v", table, err)
	}

	ir, err := tbl.OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}

	user := ir.UserIndexes()
	if n >= len(user) {
		t.Fatalf("%s has %d user indexes, want more than %d", table, len(user), n)
	}

	page, err := db.ReadPage(user[n].RootPageNo)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", user[n].RootPageNo, err)
	}

	hdr, err := parseBTreeHeader(page.PageData())
	if err != nil {
		t.Fatalf("parseBTreeHeader: %v", err)
	}

	if !hdr.IsRoot || !hdr.IsLeaf || int(hdr.KeyPrefixSize) != indexKeySize {
		t.Fatalf("index %d: root=%t leaf=%t keySize=%d, want a single %d-byte-keyed leaf",
			n, hdr.IsRoot, hdr.IsLeaf, hdr.KeyPrefixSize, indexKeySize)
	}

	return page.PageData(), int(hdr.EntryCount)
}

// leafKeys decodes a leaf's live keys, and separately the keys still readable
// in the strides past EntryCount -- the bytes a removal deliberately leaves
// behind.
func leafKeys(t *testing.T, db *File, table string, n int) (live, trailer []int32) {
	t.Helper()

	payload, count := leafPage(t, db, table, n)
	stride := indexKeySize + leafEntrySuffixSize

	read := func(i int) int32 {
		off := btreeHeaderSize + i*stride

		return int32(binary.LittleEndian.Uint32(payload[off+1 : off+indexKeySize]))
	}

	for i := range count {
		live = append(live, read(i))
	}

	// One stride past the live entries is where a removal's vacated slot is;
	// anything further is untouched page fill.
	if btreeHeaderSize+(count+1)*stride <= len(payload) {
		trailer = append(trailer, read(count))
	}

	return live, trailer
}

// changedPageTypes reports the sorted, deduplicated page types whose bytes
// differ between two fixtures of the same size.
func changedPageTypes(t *testing.T, before, after string) []string {
	t.Helper()

	a, err := os.ReadFile(requireFixture(t, before))
	if err != nil {
		t.Fatalf("reading %s: %v", before, err)
	}

	b, err := os.ReadFile(requireFixture(t, after))
	if err != nil {
		t.Fatalf("reading %s: %v", after, err)
	}

	if len(a) != len(b) {
		t.Fatalf("%s is %d bytes and %s is %d; the pair is meant to be one statement apart",
			before, len(a), after, len(b))
	}

	db := openFixture(t, after)
	defer db.Close()

	size := db.PageSize()
	seen := map[string]bool{}

	for no := range db.PageCount() {
		start, end := no*size, min((no+1)*size, len(a))
		if bytes.Equal(a[start:end], b[start:end]) {
			continue
		}

		page, err := db.ReadPage(no)
		if err != nil || page.Header == nil {
			seen["unreadable"] = true

			continue
		}

		seen[pageTypeName(page.Header.PageType)] = true
	}

	types := make([]string, 0, len(seen))
	for k := range seen {
		types = append(types, k)
	}

	sort.Strings(types)

	return types
}

// pageTypeName names the page types these fixtures touch, so a mismatch reads
// as "data vs data and something else" rather than as two lists of numbers.
func pageTypeName(t uint16) string {
	switch int(t) {
	case PageTypeData:
		return "data"
	case PageTypeIndex:
		return "index"
	case PageTypeTableInfo:
		return "tableinfo"
	case PageTypeSystem:
		return "system"
	default:
		return "other"
	}
}

func equalInt32(a, b []int32) bool {
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

func equalStrings(a, b []string) bool {
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
