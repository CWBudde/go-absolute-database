package absdb

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTS03IndexDiscovery(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	indexes := ir.Indexes()
	t.Logf("Found %d indexes", len(indexes))

	for i, idx := range indexes {
		t.Logf("  Index %d: root=page %d, keySize=%d, entries=%d, internal=%v",
			i, idx.RootPageNo, idx.KeySize, idx.EntryCount, idx.IsInternal)
	}

	// TS03 should have 4 index root pages.
	if len(indexes) != 4 {
		t.Errorf("expected 4 indexes, got %d", len(indexes))
	}

	// Pages 9,10 are system (4-byte keys), 11 is primary (5-byte), 12 is secondary.
	secondaries := ir.SecondaryIndexes()
	if len(secondaries) != 1 {
		t.Errorf("expected 1 secondary index, got %d", len(secondaries))
	}

	if secondaries[0].RootPageNo != 12 {
		t.Errorf("secondary index root page = %d, want 12", secondaries[0].RootPageNo)
	}
}

// TestConstraintsIndexMetadata joins the committed fixture's schema records
// to its empty B-tree roots. Empty leaves cannot identify their owner through
// record references, so this also pins that Table.OpenIndex uses the schema's
// root page number as the authority in a multi-table database.
func TestConstraintsIndexMetadata(t *testing.T) {
	db := openTestFile(t, constraintsFixture)

	tests := []struct {
		table   string
		name    string
		columns []string
		primary bool
	}{
		{table: "CBoth", name: "C_Unique$B", columns: []string{"B"}, primary: false},
		{table: "CIdxMulti", name: "IdxMulti", columns: []string{"A", "B"}, primary: false},
		{table: "CPkMulti", name: "C_PK$A$B", columns: []string{"A", "B"}, primary: true},
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			table, err := db.Table(tt.table)
			if err != nil {
				t.Fatal(err)
			}

			ir, err := table.OpenIndex()
			if err != nil {
				t.Fatal(err)
			}

			user := ir.UserIndexes()
			if len(user) != 1 {
				t.Fatalf("UserIndexes() = %d, want 1", len(user))
			}

			if got := user[0]; got.Name != tt.name || !slices.Equal(got.Columns, tt.columns) {
				t.Errorf("index metadata = %q %v, want %q %v",
					got.Name, got.Columns, tt.name, tt.columns)
			}

			primary, err := ir.PrimaryKeyIndex()
			if tt.primary {
				if err != nil {
					t.Fatalf("PrimaryKeyIndex(): %v", err)
				}

				if primary.RootPageNo != user[0].RootPageNo {
					t.Errorf("primary root = %d, want %d", primary.RootPageNo, user[0].RootPageNo)
				}
			} else if !errors.Is(err, ErrNoIndex) {
				t.Errorf("PrimaryKeyIndex() error = %v, want ErrNoIndex", err)
			}
		})
	}
}

func TestFindByStringKeySelectsTheCoveredColumn(t *testing.T) {
	db := openTestFile(t, constraintsFixture)

	table, err := db.Table("CBoth")
	if err != nil {
		t.Fatal(err)
	}

	ir, err := table.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	// CBoth's only index covers B. The empty leaf cannot find a value, but it
	// distinguishes selecting that index from finding no index for A.
	_, _, err = ir.FindByStringKey("b", "value")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("FindByStringKey(B) error = %v, want ErrKeyNotFound", err)
	}

	_, _, err = ir.FindByStringKey("A", "value")
	if !errors.Is(err, ErrNoIndex) {
		t.Errorf("FindByStringKey(A) error = %v, want ErrNoIndex", err)
	}

	multi, err := db.Table("CIdxMulti")
	if err != nil {
		t.Fatal(err)
	}

	ir, err = multi.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = ir.FindByStringKey("B", "value")
	if !errors.Is(err, ErrNoIndex) {
		t.Errorf("FindByStringKey through compound index error = %v, want ErrNoIndex", err)
	}
}

func TestTS03PrimaryKeyLookup(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	// Look up primary key values 1 through 18.
	for key := int32(1); key <= 18; key++ {
		pageNo, itemNo, err := ir.FindByPrimaryKey(key)
		if err != nil {
			t.Fatalf("FindByPrimaryKey(%d): %v", key, err)
		}

		if pageNo != 13 {
			t.Errorf("key %d: pageNo=%d, want 13", key, pageNo)
		}

		expectedItem := uint16(key - 1) // 0-based
		if itemNo != expectedItem {
			t.Errorf("key %d: itemNo=%d, want %d", key, itemNo, expectedItem)
		}
	}

	// Look up a non-existent key.
	_, _, err = ir.FindByPrimaryKey(999)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("FindByPrimaryKey(999): expected ErrKeyNotFound, got %v", err)
	}
}

func TestTS03StringIndexLookup(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	table, err := db.Table("")
	if err != nil {
		t.Fatal(err)
	}

	ir, err := table.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	// Look up "EC / IC" by name in the secondary index.
	pageNo, itemNo, err := ir.FindByStringKey("Name", "EC / IC")
	if err != nil {
		t.Fatalf("FindByStringKey: %v", err)
	}

	t.Logf("Found 'EC / IC' at page=%d, item=%d", pageNo, itemNo)

	if pageNo != 13 || itemNo != 2 {
		t.Errorf("expected page=13, item=2, got page=%d, item=%d", pageNo, itemNo)
	}

	// Non-existent value.
	_, _, err = ir.FindByStringKey("Name", "Nonexistent Train Type")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for non-existent key, got %v", err)
	}
}

func TestTS03IndexScan(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	// Scan the primary key index (page 11).
	entries, err := ir.ScanIndex(11)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 18 {
		t.Errorf("primary index scan: got %d entries, want 18", len(entries))
	}

	// Verify entries are sequential.
	for i, e := range entries {
		expectedKey := int32(i + 1)

		if len(e.Key) >= 5 {
			actualKey := int32(e.Key[1]) | int32(e.Key[2])<<8 | int32(e.Key[3])<<16 | int32(e.Key[4])<<24
			if actualKey != expectedKey {
				t.Errorf("entry %d: key=%d, want %d", i, actualKey, expectedKey)
			}
		}

		if e.PageNo != 13 {
			t.Errorf("entry %d: pageNo=%d, want 13", i, e.PageNo)
		}
	}

	// Scan the secondary index (page 12) - sorted by name.
	entries, err = ir.ScanIndex(12)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 18 {
		t.Errorf("secondary index scan: got %d entries, want 18", len(entries))
	}

	// First entry should be alphabetically first train name.
	if len(entries) > 0 {
		// Key starts with null flag byte, then the string.
		firstName := extractStringFromKey(entries[0].Key)
		t.Logf("First entry in name index: %q", firstName)
	}

	// Log all entries in name-sorted order.
	for i, e := range entries {
		name := extractStringFromKey(e.Key)
		t.Logf("  [%2d] %q -> page=%d, item=%d", i, name, e.PageNo, e.ItemNo)
	}
}

func TestRPDG0011PrimaryKeyLookup(t *testing.T) {
	db := openTestFile(t, "RPDG0011.abs")

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	// Look up primary key 1.
	pageNo, itemNo, err := ir.FindByPrimaryKey(1)
	if err != nil {
		t.Fatalf("FindByPrimaryKey(1): %v", err)
	}

	t.Logf("RPDG0011 key=1: page=%d, item=%d", pageNo, itemNo)

	if pageNo != 15 { // RPDG0011 data page is 15
		t.Errorf("expected page 15, got %d", pageNo)
	}
}

func TestBTreePageHeader(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	page, err := db.ReadPage(11) // Primary key index
	if err != nil {
		t.Fatal(err)
	}

	hdr, err := parseBTreeHeader(page.PageData())
	if err != nil {
		t.Fatal(err)
	}

	if !hdr.IsRoot {
		t.Error("expected root page")
	}

	if !hdr.IsLeaf {
		t.Error("expected leaf page")
	}

	if hdr.EntryCount != 18 {
		t.Errorf("EntryCount = %d, want 18", hdr.EntryCount)
	}

	if hdr.KeyPrefixSize != 5 {
		t.Errorf("KeyPrefixSize = %d, want 5", hdr.KeyPrefixSize)
	}
}

// extractStringFromKey extracts the null-terminated string from an index key.
// The first byte is a null flag (skip it).
func extractStringFromKey(key []byte) string {
	if len(key) <= 1 {
		return ""
	}

	data := key[1:] // skip null flag byte

	end := 0
	for end < len(data) && data[end] != 0 {
		end++
	}

	return string(data[:end])
}

// --- Ground truth from real fixtures -----------------------------------------

// TestUserIndexLeafScanCounts checks that scanning the leaf chain of every user
// index of a fixture yields exactly one entry per row of the table.
func TestUserIndexLeafScanCounts(t *testing.T) {
	tests := []struct {
		file string
		rows int
	}{
		{"TS03.abs", 18},
		{"RREC0011.abs", 30},
		{"RCON0011.abs", 300},
		{"RCFQ0011.abs", 600},
		{"RMPA0011.abs", 600},
		{"RR240011.abs", 30},
		{"RPDG0011.abs", 30},
		{"RFRQ0011.abs", 60},
		{"RGRP0011.abs", 30},
		{"RMND0011.abs", 10},
		{"RRAI0011.abs", 5},
		{"RRAD0011.abs", 20},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			db := openTestFile(t, tt.file)

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatal(err)
			}

			userIndexes := ir.UserIndexes()
			if len(userIndexes) == 0 {
				t.Fatalf("%s: no user indexes found", tt.file)
			}

			for _, idx := range userIndexes {
				entries, err := ir.ScanIndex(idx.RootPageNo)
				if err != nil {
					t.Fatalf("ScanIndex(%d): %v", idx.RootPageNo, err)
				}

				if len(entries) != tt.rows {
					t.Errorf("index root %d (keySize %d): scanned %d entries, want %d",
						idx.RootPageNo, idx.KeySize, len(entries), tt.rows)
				}
			}
		})
	}
}

// TestIndexKeySizes pins the key sizes discovered per fixture, in root page order.
func TestIndexKeySizes(t *testing.T) {
	tests := []struct {
		file     string
		keySizes []int
	}{
		{"TS03.abs", []int{4, 4, 5, 23}},
		{"RREC0011.abs", []int{4, 10}},
		{"RCON0011.abs", []int{4, 15, 10}},
		{"RCFQ0011.abs", []int{4, 5}},
		{"RMPA0011.abs", []int{4, 5, 10}},
		{"RR240011.abs", []int{4, 5, 10}},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			db := openTestFile(t, tt.file)

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatal(err)
			}

			indexes := ir.Indexes()

			got := make([]int, len(indexes))
			for i, idx := range indexes {
				got[i] = idx.KeySize
			}

			if !slices.Equal(got, tt.keySizes) {
				t.Fatalf("key sizes = %v, want %v", got, tt.keySizes)
			}

			// Every 4-byte-key index is a system index; nothing else is.
			for _, idx := range indexes {
				if idx.IsInternal != (idx.KeySize == 4) {
					t.Errorf("root %d: IsInternal=%v for keySize %d",
						idx.RootPageNo, idx.IsInternal, idx.KeySize)
				}
			}

			// UserIndexes is exactly Indexes minus the system indexes, and
			// SecondaryIndexes is UserIndexes minus the primary key index.
			wantUser := 0

			for _, k := range tt.keySizes {
				if k != 4 {
					wantUser++
				}
			}

			if len(ir.UserIndexes()) != wantUser {
				t.Errorf("UserIndexes() = %d, want %d", len(ir.UserIndexes()), wantUser)
			}

			wantSecondary := wantUser

			_, err = ir.PrimaryKeyIndex()
			if err == nil {
				wantSecondary--
			}

			if len(ir.SecondaryIndexes()) != wantSecondary {
				t.Errorf("SecondaryIndexes() = %d, want %d", len(ir.SecondaryIndexes()), wantSecondary)
			}
		})
	}
}

// TestPrimaryKeyIndexAbsent covers the tables whose primary key is composite:
// they carry no 5-byte-key index at all.
func TestPrimaryKeyIndexAbsent(t *testing.T) {
	for _, name := range []string{"RREC0011.abs", "RCON0011.abs"} {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatal(err)
			}

			_, err = ir.PrimaryKeyIndex()
			if !errors.Is(err, ErrNoIndex) {
				t.Errorf("PrimaryKeyIndex() error = %v, want ErrNoIndex", err)
			}
		})
	}
}

// TestFindByPrimaryKeyRoundTrip walks the leaf chain of the primary key index
// and looks every key up again through the tree. Both fixtures have multi-level
// primary indexes, so this exercises the internal node entry stride and the
// little-endian key ordering.
func TestFindByPrimaryKeyRoundTrip(t *testing.T) {
	for _, name := range []string{"RCFQ0011.abs", "RMPA0011.abs"} {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			ir, err := db.OpenIndex()
			if err != nil {
				t.Fatal(err)
			}

			pk, err := ir.PrimaryKeyIndex()
			if err != nil {
				t.Fatal(err)
			}

			entries, err := ir.ScanIndex(pk.RootPageNo)
			if err != nil {
				t.Fatal(err)
			}

			if len(entries) != 600 {
				t.Fatalf("primary index scan: %d entries, want 600", len(entries))
			}

			for _, e := range entries {
				key := int32(binary.LittleEndian.Uint32(e.Key[1:5]))

				pageNo, itemNo, err := ir.FindByPrimaryKey(key)
				if err != nil {
					t.Fatalf("FindByPrimaryKey(%d): %v", key, err)
				}

				if pageNo != e.PageNo || itemNo != e.ItemNo {
					t.Errorf("FindByPrimaryKey(%d) = (%d, %d), leaf scan says (%d, %d)",
						key, pageNo, itemNo, e.PageNo, e.ItemNo)
				}
			}
		})
	}
}

// --- Malformed index handling -------------------------------------------------

// craftedIndexPage describes one index page of a synthetic .abs file.
type craftedIndexPage struct {
	isRoot     bool
	isLeaf     bool
	rightPage  int32
	keySize    uint16
	entryCount uint16
	entries    []byte // raw entry bytes placed behind the B-tree header
}

// buildIndexFile writes a minimal .abs file whose pages 1..n are the given
// index pages. Page 0 holds only the database header.
func buildIndexFile(t *testing.T, pages []craftedIndexPage) string {
	t.Helper()

	const pageSize = 4096

	pageCount := len(pages) + 1
	buf := make([]byte, pageCount*pageSize+diskPageHeaderOffset)

	copy(buf, Magic[:])
	binary.LittleEndian.PutUint16(buf[16:18], dbHeaderSize)
	binary.LittleEndian.PutUint64(buf[18:26], math.Float64bits(7.61))
	binary.LittleEndian.PutUint16(buf[26:28], pageSize)
	binary.LittleEndian.PutUint16(buf[28:30], 1)
	binary.LittleEndian.PutUint32(buf[30:34], uint32(pageCount))
	binary.LittleEndian.PutUint32(buf[34:38], uint32(pageCount-1))

	for i, spec := range pages {
		pageNo := i + 1

		// ABSP disk page header.
		h := pageNo*pageSize + diskPageHeaderOffset
		copy(buf[h:h+4], "ABSP")
		binary.LittleEndian.PutUint16(buf[h+8:h+10], PageTypeIndex)
		binary.LittleEndian.PutUint32(buf[h+10:h+14], math.MaxUint32) // NextPageNo = -1

		// B-tree page header at the start of the payload.
		p := pageNo*pageSize + pageDataOffset
		if spec.isRoot {
			buf[p] = 1
		}

		if spec.isLeaf {
			buf[p+1] = 1
		}

		binary.LittleEndian.PutUint32(buf[p+2:p+6], math.MaxUint32) // LeftPageNo = -1
		binary.LittleEndian.PutUint32(buf[p+6:p+10], uint32(spec.rightPage))
		buf[p+10] = 1 // HasKeys
		binary.LittleEndian.PutUint16(buf[p+12:p+14], spec.keySize)
		binary.LittleEndian.PutUint16(buf[p+14:p+16], spec.entryCount)
		copy(buf[p+btreeHeaderSize:], spec.entries)
	}

	path := filepath.Join(t.TempDir(), "crafted.abs")

	err := os.WriteFile(path, buf, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// openCraftedIndex opens a synthetic file and returns its IndexReader.
func openCraftedIndex(t *testing.T, pages []craftedIndexPage) *IndexReader {
	t.Helper()

	db, err := Open(buildIndexFile(t, pages))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	return ir
}

// TestCyclicLeafChain makes sure a leaf whose RightPageNo points at itself is
// reported as malformed instead of scanned forever.
func TestCyclicLeafChain(t *testing.T) {
	ir := openCraftedIndex(t, []craftedIndexPage{{
		isRoot:     true,
		isLeaf:     true,
		rightPage:  1, // points at itself
		keySize:    5,
		entryCount: 1,
		entries:    make([]byte, 11),
	}})

	_, err := ir.ScanIndex(1)
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("ScanIndex on cyclic leaf chain: error = %v, want ErrMalformedIndex", err)
	}
}

// TestCyclicInternalNode makes sure a descent through a self-referential child
// pointer is bounded instead of looping forever.
func TestCyclicInternalNode(t *testing.T) {
	// One entry: a 5-byte key followed by a child pointer back to this page.
	entry := make([]byte, 9)
	binary.LittleEndian.PutUint32(entry[5:9], 1)

	ir := openCraftedIndex(t, []craftedIndexPage{{
		isRoot:     true,
		isLeaf:     false,
		rightPage:  -1,
		keySize:    5,
		entryCount: 1,
		entries:    entry,
	}})

	_, _, err := ir.FindByPrimaryKey(1)
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("FindByPrimaryKey on cyclic tree: error = %v, want ErrMalformedIndex", err)
	}

	_, err = ir.ScanIndex(1)
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("ScanIndex on cyclic tree: error = %v, want ErrMalformedIndex", err)
	}
}

// TestEmptyInternalNode covers an internal node without entries: there is no
// child to descend into, which must be an error rather than a panic.
func TestEmptyInternalNode(t *testing.T) {
	ir := openCraftedIndex(t, []craftedIndexPage{{
		isRoot:     true,
		isLeaf:     false,
		rightPage:  -1,
		keySize:    5,
		entryCount: 0,
	}})

	_, err := ir.ScanIndex(1)
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("ScanIndex on empty internal node: error = %v, want ErrMalformedIndex", err)
	}

	_, _, err = ir.FindByPrimaryKey(1)
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("FindByPrimaryKey on empty internal node: error = %v, want ErrMalformedIndex", err)
	}
}

// TestZeroKeyPrefixSize covers an index page claiming zero-length keys, which
// must not reach the key builders with an empty slice.
func TestZeroKeyPrefixSize(t *testing.T) {
	ir := openCraftedIndex(t, []craftedIndexPage{{
		isRoot:     true,
		isLeaf:     true,
		rightPage:  -1,
		keySize:    0,
		entryCount: 3,
	}})
	ir.indexes[0].columns = []string{"Name"}

	_, err := ir.ScanIndex(1)
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("ScanIndex with zero key size: error = %v, want ErrMalformedIndex", err)
	}

	// Give the crafted root a covered column so FindByStringKey reaches the key
	// builder and rejects its zero key size.
	_, _, err = ir.FindByStringKey("Name", "anything")
	if !errors.Is(err, ErrMalformedIndex) {
		t.Fatalf("FindByStringKey with zero key size: error = %v, want ErrMalformedIndex", err)
	}
}

// TestEntryCountBeyondPageCapacity makes sure a bogus EntryCount cannot drive
// the allocation: only the entries the page can actually hold are read.
func TestEntryCountBeyondPageCapacity(t *testing.T) {
	ir := openCraftedIndex(t, []craftedIndexPage{{
		isRoot:     true,
		isLeaf:     true,
		rightPage:  -1,
		keySize:    5,
		entryCount: 65535, // page holds (4056-18)/11 = 367 entries
	}})

	entries, err := ir.ScanIndex(1)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != (4056-btreeHeaderSize)/11 {
		t.Fatalf("read %d entries, want %d", len(entries), (4056-btreeHeaderSize)/11)
	}
}
