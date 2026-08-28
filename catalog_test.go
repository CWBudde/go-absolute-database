package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCatalogAcrossFixtures reads the table catalog of every fixture and checks
// it against what the rest of the file says. It is the broadest evidence that
// the 272-byte entry layout is right: the two page numbers an entry carries
// have to land on pages of the type they claim, in files spanning format
// versions 5.13, 7.61 and 7.94.
func TestCatalogAcrossFixtures(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			if len(tables) == 0 {
				t.Fatal("catalog is empty")
			}

			seen := make(map[int]bool, len(tables))

			for _, info := range tables {
				if info.Name == "" {
					t.Errorf("table %d has an empty name", info.ID)
				}

				if seen[info.ID] {
					t.Errorf("table ID %d appears twice", info.ID)
				}

				seen[info.ID] = true

				checkPageType(t, db, info.SchemaPageNo, PageTypeSchema, "schema")
				checkPageType(t, db, info.InfoPageNo, PageTypeTableInfo, "table info")
			}
		})
	}
}

// checkPageType asserts that a page number named by the catalog really holds a
// page of the type the field promises.
func checkPageType(t *testing.T, db *File, pageNo int, want uint16, what string) {
	t.Helper()

	page, err := db.ReadPage(pageNo)
	if err != nil {
		t.Fatalf("reading %s page %d: %v", what, pageNo, err)
	}

	if page.Header == nil {
		t.Fatalf("%s page %d has no disk page header", what, pageNo)
	}

	if page.Header.PageType != want {
		t.Errorf("%s page %d has type %d, want %d", what, pageNo, page.Header.PageType, want)
	}
}

// TestCatalogIDMatchesDataPageOwner checks the one field that partitions pages
// by table: every data page's ABSP ObjectID must name a table the catalog
// lists. Nothing else in the file records ownership, so if this ever fails the
// per-table Reader is reading someone else's rows.
func TestCatalogIDMatchesDataPageOwner(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			known := make(map[int]bool, len(tables))
			for _, info := range tables {
				known[info.ID] = true
			}

			for i := range db.PageCount() {
				page, err := db.ReadPage(i)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", i, err)
				}

				if page.Header == nil || page.Header.PageType != PageTypeData {
					continue
				}

				owner := int(page.Header.ObjectID)
				if known[owner] {
					continue
				}

				// A data page whose owner the catalog does not list is only
				// legal when the engine has freed it: DROP TABLE tombstones
				// the dropped table's pages rather than erasing them, so its
				// rows and its ObjectID are still on disk.
				if !page.Freed() {
					t.Errorf("live data page %d is owned by table %d, which the catalog does not list", i, owner)
				}
			}
		})
	}
}

// TestTableByName checks name lookup, including that it ignores case the way
// the engine's own SQL does.
func TestTableByName(t *testing.T) {
	db := openFixture(t, "Writes.abs")
	defer db.Close()

	for _, name := range []string{"Writes", "writes", "WRITES"} {
		tbl, err := db.Table(name)
		if err != nil {
			t.Fatalf("Table(%q): %v", name, err)
		}

		if tbl.Name() != "Writes" {
			t.Errorf("Table(%q).Name() = %q, want %q", name, tbl.Name(), "Writes")
		}
	}

	_, err := db.Table("NoSuchTable")
	if !errors.Is(err, ErrNoSuchTable) {
		t.Errorf("Table(unknown) error = %v, want ErrNoSuchTable", err)
	}
}

// TestTableEmptyNameSelectsTheOnlyTable pins the convenience form the no-argument
// methods rest on.
func TestTableEmptyNameSelectsTheOnlyTable(t *testing.T) {
	db := openFixture(t, "Writes.abs")
	defer db.Close()

	tbl, err := db.Table("")
	if err != nil {
		t.Fatalf("Table(\"\"): %v", err)
	}

	if tbl.Name() != "Writes" {
		t.Errorf("Name() = %q, want %q", tbl.Name(), "Writes")
	}

	if !tbl.sole {
		t.Error("a single-table file should yield a sole table handle")
	}
}

// TestTableScopedReadMatchesUnscoped checks that going through the handle reads
// the same rows as the no-argument form, so the refactor changed no result for
// the single-table files that are all this package could read before.
func TestTableScopedReadMatchesUnscoped(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			if len(tables) != 1 {
				t.Skipf("%d tables; the no-argument form has no single answer to compare against", len(tables))
			}

			viaFile, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable: %v", err)
			}

			tbl, err := db.Table("")
			if err != nil {
				t.Fatalf("Table(\"\"): %v", err)
			}

			viaTable, err := tbl.Open()
			if err != nil {
				t.Fatalf("Table.Open: %v", err)
			}

			for {
				a, b := viaFile.Next(), viaTable.Next()
				if a != b {
					t.Fatalf("Next disagreed: file=%v table=%v", a, b)
				}

				if !a {
					break
				}

				ra, rb := viaFile.Record(), viaTable.Record()
				if !bytes.Equal(ra.nullFlags, rb.nullFlags) || !bytes.Equal(ra.fieldData, rb.fieldData) {
					t.Fatal("the two readers returned different record bytes")
				}
			}
		})
	}
}

// TestParseTableListRejectsMalformed pins the bounds that keep a corrupt
// catalog from turning into a bad allocation or an out-of-range read.
func TestParseTableListRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{"partial entry", make([]byte, tableListEntrySize+1)},
		{"one byte", make([]byte, 1)},
		{"entry and a half", make([]byte, tableListEntrySize+tableListEntrySize/2)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTableList(tc.raw)
			if !errors.Is(err, ErrBadCatalog) {
				t.Errorf("parseTableList error = %v, want ErrBadCatalog", err)
			}
		})
	}

	// A name that fills the field is legal: the length is one byte and the
	// field holds 255, so it can never run past its own entry.
	full := make([]byte, tableListEntrySize)
	full[0] = 255

	for i := 1; i < tableNameFieldSize; i++ {
		full[i] = 'x'
	}

	longest, err := parseTableList(full)
	if err != nil {
		t.Fatalf("parseTableList with a 255-byte name: %v", err)
	}

	if len(longest[0].Name) != 255 {
		t.Errorf("name length = %d, want 255", len(longest[0].Name))
	}

	// An empty catalog is well formed and holds no tables.
	tables, err := parseTableList(nil)
	if err != nil {
		t.Fatalf("parseTableList(nil): %v", err)
	}

	if len(tables) != 0 {
		t.Errorf("parseTableList(nil) returned %d tables, want 0", len(tables))
	}
}

// TestParseTableListFields checks the field offsets against a hand-built entry,
// so a change to the layout constants cannot pass unnoticed.
func TestParseTableListFields(t *testing.T) {
	raw := make([]byte, 2*tableListEntrySize)

	for i, spec := range []struct {
		name   string
		fields [4]int32
	}{
		{"Alpha", [4]int32{1, 7, 8, 5}},
		{"Beta", [4]int32{2, 11, 12, 5}},
	} {
		e := raw[i*tableListEntrySize:]
		e[0] = byte(len(spec.name))
		copy(e[1:], spec.name)

		for j, v := range spec.fields {
			binary.LittleEndian.PutUint32(e[tableNameFieldSize+4*j:], uint32(v))
		}
	}

	tables, err := parseTableList(raw)
	if err != nil {
		t.Fatalf("parseTableList: %v", err)
	}

	want := []TableInfo{
		{Name: "Alpha", ID: 1, SchemaPageNo: 7, InfoPageNo: 8, systemPageNo: 5},
		{Name: "Beta", ID: 2, SchemaPageNo: 11, InfoPageNo: 12, systemPageNo: 5},
	}

	if len(tables) != len(want) {
		t.Fatalf("got %d tables, want %d", len(tables), len(want))
	}

	for i := range want {
		if tables[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, tables[i], want[i])
		}
	}
}

// TestCatalogNameIsWindows1252 checks that a name with a high byte decodes
// rather than coming back as raw bytes.
func TestCatalogNameIsWindows1252(t *testing.T) {
	raw := make([]byte, tableListEntrySize)
	name := []byte{0x4D, 0xFC, 0x6C, 0x6C} // "Müll" in Windows-1252
	raw[0] = byte(len(name))
	copy(raw[1:], name)

	tables, err := parseTableList(raw)
	if err != nil {
		t.Fatalf("parseTableList: %v", err)
	}

	if tables[0].Name != "Müll" {
		t.Errorf("Name = %q, want %q", tables[0].Name, "Müll")
	}
}

// TestUnlistedTableFallsBackToPageScan checks that a file with no catalog page
// still reads. Synthetic fixtures are built without one, and so is anything the
// fuzzer produces; before Tables existed that was the only behaviour there was.
func TestUnlistedTableFallsBackToPageScan(t *testing.T) {
	path := writeSynthetic(t, synthSpec{
		columns: []synthColumn{
			{"Id", BftInt32, FieldInteger, 0},
			{"Name", BftVarchar, FieldString, 8},
		},
		rows: []synthRow{
			{values: [][]byte{synthInt32(1), synthString(t, "one", 8)}},
		},
	})

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.Tables(); !errors.Is(err, ErrNoCatalog) {
		t.Fatalf("Tables error = %v, want ErrNoCatalog", err)
	}

	tbl, err := db.Table("")
	if err != nil {
		t.Fatalf("Table(\"\"): %v", err)
	}

	if !tbl.unlisted {
		t.Error("a file with no catalog should yield an unlisted table handle")
	}

	if _, err := tbl.Schema(); err != nil {
		t.Errorf("Schema on an unlisted table: %v", err)
	}

	// Asking for a table by name has nothing to match against and must say so
	// rather than quietly handing back the only table there is.
	_, err = db.Table("Anything")
	if !errors.Is(err, ErrNoCatalog) {
		t.Errorf("Table(name) on a catalog-less file = %v, want ErrNoCatalog", err)
	}
}

// TestTableInfoNamesInFixturesAreNotPaths guards the observation that the
// SoundPlan fixtures store a file name in the table name field. It is a quirk
// of how those databases were made, not something this package should paper
// over, so the test records it rather than correcting it.
func TestTableInfoNamesInFixturesAreNotPaths(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db := openFixture(t, name)
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			for _, info := range tables {
				if strings.ContainsAny(info.Name, `/\`) {
					t.Errorf("table name %q contains a path separator", info.Name)
				}
			}
		})
	}
}

// multiTableFixture is the name of the three-table fixture. Every other file in
// testdata holds exactly one table, so this is the only one that exercises the
// case the catalog exists for.
const multiTableFixture = "MultiTable.abs"

// TestMultiTableCatalog pins the catalog of the three-table fixture exactly.
// The IDs are the point: they are 1, 4 and 8, not 1, 2 and 3, so anything that
// assumes a table's ID is its position in the list reads the wrong pages.
func TestMultiTableCatalog(t *testing.T) {
	db := openFixture(t, multiTableFixture)
	defer db.Close()

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}

	want := []TableInfo{
		{Name: "Alpha", ID: 1, SchemaPageNo: 7, InfoPageNo: 8, systemPageNo: 5},
		{Name: "Beta", ID: 4, SchemaPageNo: 13, InfoPageNo: 14, systemPageNo: 11},
		{Name: "Gamma", ID: 8, SchemaPageNo: 19, InfoPageNo: 20, systemPageNo: 17},
	}

	if len(tables) != len(want) {
		t.Fatalf("got %d tables %+v, want %d", len(tables), tables, len(want))
	}

	for i := range want {
		if tables[i] != want[i] {
			t.Errorf("table %d = %+v, want %+v", i, tables[i], want[i])
		}
	}
}

// TestMultiTableReadsEachTableSeparately is the assertion Phase 5e exists for.
// Before the catalog was parsed, OpenTable read every data page in the file
// through the first table's schema: this fixture came back as six rows instead
// of Alpha's two, four of them decoded from other tables' bytes, with no error.
func TestMultiTableReadsEachTableSeparately(t *testing.T) {
	db := openFixture(t, multiTableFixture)
	defer db.Close()

	tests := []struct {
		table   string
		columns []string
		rows    [][]any
	}{
		{
			table:   "Alpha",
			columns: []string{"Id", "Name"},
			rows:    [][]any{{int64(1), "one"}, {int64(2), "two"}},
		},
		{
			table:   "Beta",
			columns: []string{"Code", "Amount", "Flag"},
			rows: [][]any{
				{"aa", 1.5, true},
				{"bb", 2.5, false},
				{"cc", 3.5, true},
			},
		},
		{
			table:   "Gamma",
			columns: []string{"K", "V"},
			rows:    [][]any{{int64(10), int64(100)}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.table, func(t *testing.T) {
			tbl, err := db.Table(tc.table)
			if err != nil {
				t.Fatalf("Table(%q): %v", tc.table, err)
			}

			schema, err := tbl.Schema()
			if err != nil {
				t.Fatalf("Schema: %v", err)
			}

			if len(schema.Columns) != len(tc.columns) {
				t.Fatalf("%d columns, want %d", len(schema.Columns), len(tc.columns))
			}

			for i, name := range tc.columns {
				if schema.Columns[i].Name != name {
					t.Errorf("column %d is %q, want %q", i, schema.Columns[i].Name, name)
				}
			}

			reader, err := tbl.Open()
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			checkRows(t, reader, tc.rows)
		})
	}
}

// checkRows walks a reader and compares each row against want, one value at a
// time so that a mismatch names the column it happened in.
func checkRows(t *testing.T, reader *Reader, want [][]any) {
	t.Helper()

	row := 0

	for reader.Next() {
		if row >= len(want) {
			t.Fatalf("reader produced more than the expected %d rows", len(want))
		}

		rec := reader.Record()

		for col, expected := range want[row] {
			var got any

			switch expected.(type) {
			case int64:
				got = rec.Int64(col)
			case string:
				got = rec.String(col)
			case float64:
				got = rec.Float(col)
			case bool:
				got = rec.Bool(col)
			}

			if got != expected {
				t.Errorf("row %d column %d = %v (%T), want %v (%T)", row, col, got, got, expected, expected)
			}
		}

		row++
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	if row != len(want) {
		t.Errorf("reader produced %d rows, want %d", row, len(want))
	}
}

// TestMultiTableDataPagesArePartitioned checks the partition directly. Each
// table owns exactly one data page here, and no page may appear twice.
func TestMultiTableDataPagesArePartitioned(t *testing.T) {
	db := openFixture(t, multiTableFixture)
	defer db.Close()

	want := map[string][]int{
		"Alpha": {10},
		"Beta":  {16},
		"Gamma": {22},
	}

	for name, wantPages := range want {
		tbl, err := db.Table(name)
		if err != nil {
			t.Fatalf("Table(%q): %v", name, err)
		}

		got, err := tbl.dataPages()
		if err != nil {
			t.Fatalf("%s: dataPages: %v", name, err)
		}

		if len(got) != len(wantPages) {
			t.Errorf("%s owns pages %v, want %v", name, got, wantPages)

			continue
		}

		for i := range wantPages {
			if got[i] != wantPages[i] {
				t.Errorf("%s owns pages %v, want %v", name, got, wantPages)

				break
			}
		}
	}
}

// TestMultiTableIndexAttribution checks that the one user index in the file is
// attributed to the one table it covers. Index pages carry no ObjectID, so the
// only thing that ties this index to Alpha is that its leaf entries point at
// Alpha's data page.
func TestMultiTableIndexAttribution(t *testing.T) {
	db := openFixture(t, multiTableFixture)
	defer db.Close()

	want := map[string]int{"Alpha": 1, "Beta": 0, "Gamma": 0}

	for name, wantCount := range want {
		tbl, err := db.Table(name)
		if err != nil {
			t.Fatalf("Table(%q): %v", name, err)
		}

		ir, err := tbl.OpenIndex()
		if err != nil {
			t.Fatalf("%s: OpenIndex: %v", name, err)
		}

		if got := len(ir.UserIndexes()); got != wantCount {
			t.Errorf("%s has %d user indexes, want %d", name, got, wantCount)
		}
	}
}

// TestDroppedTableIsGoneButItsPagesRemain pins what DROP TABLE does. The
// engine shrinks the catalog and tombstones the dropped table's pages by
// setting their State to pageStateFree; it erases nothing. Both halves matter:
// a parser that ignored the catalog's length field would report the dropped
// table twice, because the entry it was overwritten with is still there a
// second time further down.
func TestDroppedTableIsGoneButItsPagesRemain(t *testing.T) {
	db := openFixture(t, "MultiTable-drop.abs")
	defer db.Close()

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}

	names := make([]string, 0, len(tables))
	for _, info := range tables {
		names = append(names, info.Name)
	}

	if len(names) != 2 || names[0] != "Alpha" || names[1] != "Gamma" {
		t.Fatalf("catalog lists %v, want [Alpha Gamma]", names)
	}

	if _, err := db.Table("Beta"); !errors.Is(err, ErrNoSuchTable) {
		t.Errorf("Table(\"Beta\") after DROP = %v, want ErrNoSuchTable", err)
	}

	// The dropped table's data page is still on disk, still typed as data and
	// still owned by table 4. Only its State says it is gone.
	page, err := db.ReadPage(16)
	if err != nil {
		t.Fatalf("ReadPage(16): %v", err)
	}

	if page.Header.PageType != PageTypeData || page.Header.ObjectID != 4 {
		t.Errorf("page 16 is type %d owned by %d, want type %d owned by 4",
			page.Header.PageType, page.Header.ObjectID, PageTypeData)
	}

	if !page.Freed() {
		t.Error("page 16 should be marked freed after DROP TABLE")
	}

	// The surviving tables read exactly as they did before the drop.
	for _, tc := range []struct {
		table string
		rows  [][]any
	}{
		{"Alpha", [][]any{{int64(1), "one"}, {int64(2), "two"}}},
		{"Gamma", [][]any{{int64(10), int64(100)}}},
	} {
		tbl, err := db.Table(tc.table)
		if err != nil {
			t.Fatalf("Table(%q): %v", tc.table, err)
		}

		reader, err := tbl.Open()
		if err != nil {
			t.Fatalf("%s: Open: %v", tc.table, err)
		}

		checkRows(t, reader, tc.rows)
	}
}

// syntheticCatalogFile builds a file whose only real content is a table
// catalog holding the named tables, split across as many pages as it needs.
// No fixture has a catalog longer than one page — that takes fifteen tables at
// a 4 KiB page size — so this is the only way the chain gets exercised.
func syntheticCatalogFile(t *testing.T, pageSize int, names []string) string {
	t.Helper()

	raw := make([]byte, 0, len(names)*tableListEntrySize)

	for i, name := range names {
		entry := make([]byte, tableListEntrySize)
		entry[0] = byte(len(name))
		copy(entry[1:], name)
		binary.LittleEndian.PutUint32(entry[tableNameFieldSize:], uint32(i+1))
		raw = append(raw, entry...)
	}

	file := make([]byte, internalFileHeaderSize+len(raw))
	file[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(file[1:5], uint32(len(raw)))
	binary.LittleEndian.PutUint32(file[5:9], uint32(len(raw)))
	copy(file[internalFileHeaderSize:], raw)

	payloadLen := pageSize - diskPageHeaderSize

	pages := []synthPage{{pageType: PageTypeFileHdr, objectID: -1, nextPage: -1}}

	for len(file) > 0 {
		chunk := min(len(file), payloadLen)

		next := int32(-1)
		if len(file) > chunk {
			next = int32(len(pages) + 1)
		}

		pages = append(pages, synthPage{
			pageType: PageTypeTableList,
			objectID: -1,
			nextPage: next,
			payload:  file[:chunk],
		})

		file = file[chunk:]
	}

	path := filepath.Join(t.TempDir(), "catalog.abs")

	err := os.WriteFile(path, assembleFile(t, pageSize, pages), 0o600)
	if err != nil {
		t.Fatalf("writing synthetic catalog: %v", err)
	}

	return path
}

// TestCatalogSpanningSeveralPages checks that a catalog too large for one page
// is followed across its chain. Fifteen 272-byte entries no longer fit in a
// 4056-byte payload, and the engine continues an internal file on the page its
// NextPageNo names, exactly as it does for the type-7 files.
func TestCatalogSpanningSeveralPages(t *testing.T) {
	names := make([]string, 0, 40)
	for i := range 40 {
		names = append(names, "T"+strconv.Itoa(i))
	}

	// A small page size makes the chain several pages long rather than two.
	for _, pageSize := range []int{512, 4096} {
		t.Run(strconv.Itoa(pageSize), func(t *testing.T) {
			db, err := Open(syntheticCatalogFile(t, pageSize, names))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer db.Close()

			tables, err := db.Tables()
			if err != nil {
				t.Fatalf("Tables: %v", err)
			}

			if len(tables) != len(names) {
				t.Fatalf("got %d tables, want %d", len(tables), len(names))
			}

			for i, info := range tables {
				if info.Name != names[i] || info.ID != i+1 {
					t.Errorf("entry %d = %+v, want name %q id %d", i, info, names[i], i+1)
				}
			}

			// Naming one of the later tables proves the chain was read, not
			// just its first page.
			tbl, err := db.Table("T39")
			if err != nil {
				t.Errorf("Table(T39): %v", err)
			} else if tbl.Info().ID != 40 {
				t.Errorf("T39 has ID %d, want 40", tbl.Info().ID)
			}
		})
	}
}

// TestCatalogChainTerminates checks the two guards that keep a corrupted
// NextPageNo from walking the file forever: a page may not appear twice, and
// the chain may not outrun maxCatalogPages.
func TestCatalogChainTerminates(t *testing.T) {
	// A catalog that claims far more bytes than one page holds, on a page that
	// names itself as its own successor.
	payload := make([]byte, 64)
	payload[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(payload[1:5], 10*tableListEntrySize)

	pages := []synthPage{
		{pageType: PageTypeFileHdr, objectID: -1, nextPage: -1},
		{pageType: PageTypeTableList, objectID: -1, nextPage: 1, payload: payload},
	}

	path := filepath.Join(t.TempDir(), "loop.abs")

	err := os.WriteFile(path, assembleFile(t, 512, pages), 0o600)
	if err != nil {
		t.Fatalf("writing: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, err = db.Tables()
	if !errors.Is(err, ErrBadCatalog) {
		t.Errorf("Tables over a self-referential chain = %v, want ErrBadCatalog", err)
	}
}
