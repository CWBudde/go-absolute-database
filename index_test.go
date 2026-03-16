package absdb

import (
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
	if err != ErrKeyNotFound {
		t.Errorf("FindByPrimaryKey(999): expected ErrKeyNotFound, got %v", err)
	}
}

func TestTS03StringIndexLookup(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	ir, err := db.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}

	// Look up "EC / IC" by name in the secondary index.
	pageNo, itemNo, err := ir.FindByStringKey("EC / IC")
	if err != nil {
		t.Fatalf("FindByStringKey: %v", err)
	}

	t.Logf("Found 'EC / IC' at page=%d, item=%d", pageNo, itemNo)

	if pageNo != 13 || itemNo != 2 {
		t.Errorf("expected page=13, item=2, got page=%d, item=%d", pageNo, itemNo)
	}

	// Non-existent value.
	_, _, err = ir.FindByStringKey("Nonexistent Train Type")
	if err != ErrKeyNotFound {
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
