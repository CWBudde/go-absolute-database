package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

const (
	// btreeHeaderSize is the packed size of TABSBTreePageHeader.
	btreeHeaderSize = 18
)

var (
	ErrNoIndex     = errors.New("absdb: no index found")
	ErrKeyNotFound = errors.New("absdb: key not found")
)

// BTreePageHeader is the on-disk header at the start of every index page body.
type BTreePageHeader struct {
	IsRoot         bool
	IsLeaf         bool
	LeftPageNo     int32 // left sibling page (-1 = none)
	RightPageNo    int32 // right sibling page (-1 = none)
	HasKeys        bool
	HasSuffixes    bool
	KeyPrefixSize  uint16 // key size per entry (short key)
	EntryCount     uint16 // number of entries on this page
	PagePrefixSize uint16 // page-level prefix size
}

// BTreeEntry is a single entry in an index page.
type BTreeEntry struct {
	Key    []byte // key bytes (KeyPrefixSize bytes)
	PageNo int32  // referenced page number
	ItemNo uint16 // referenced item number within the page
}

// RecordID returns the entry's reference as a (PageNo, ItemNo) pair.
func (e BTreeEntry) RecordID() (int32, uint16) {
	return e.PageNo, e.ItemNo
}

// IndexInfo describes a discovered index.
type IndexInfo struct {
	RootPageNo int  // root page of the B-tree
	KeySize    int  // key size in bytes
	EntryCount int  // total entries (for root-only trees)
	IsInternal bool // true for system indexes (RecordPage, BlobPage)
}

// parseBTreeHeader reads the TABSBTreePageHeader from index page data.
func parseBTreeHeader(data []byte) (*BTreePageHeader, error) {
	if len(data) < btreeHeaderSize {
		return nil, fmt.Errorf("absdb: index page too short (%d bytes)", len(data))
	}

	return &BTreePageHeader{
		IsRoot:         data[0] != 0,
		IsLeaf:         data[1] != 0,
		LeftPageNo:     int32(binary.LittleEndian.Uint32(data[2:6])),
		RightPageNo:    int32(binary.LittleEndian.Uint32(data[6:10])),
		HasKeys:        data[10] != 0,
		HasSuffixes:    data[11] != 0,
		KeyPrefixSize:  binary.LittleEndian.Uint16(data[12:14]),
		EntryCount:     binary.LittleEndian.Uint16(data[14:16]),
		PagePrefixSize: binary.LittleEndian.Uint16(data[16:18]),
	}, nil
}

// readBTreeEntries reads all entries from an index page.
func readBTreeEntries(data []byte, hdr *BTreePageHeader) []BTreeEntry {
	keySize := int(hdr.KeyPrefixSize)
	entrySize := keySize + 6 // key + PageItemID (4 + 2)
	entries := make([]BTreeEntry, 0, hdr.EntryCount)

	for i := range int(hdr.EntryCount) {
		off := btreeHeaderSize + i*entrySize
		if off+entrySize > len(data) {
			break
		}

		key := make([]byte, keySize)
		copy(key, data[off:off+keySize])

		pageNo := int32(binary.LittleEndian.Uint32(data[off+keySize : off+keySize+4]))
		itemNo := binary.LittleEndian.Uint16(data[off+keySize+4 : off+keySize+6])

		entries = append(entries, BTreeEntry{
			Key:    key,
			PageNo: pageNo,
			ItemNo: itemNo,
		})
	}

	return entries
}

// IndexReader provides index-based lookups on a table.
type IndexReader struct {
	db      *File
	indexes []indexRoot
}

// indexRoot tracks a discovered index root page.
type indexRoot struct {
	pageNo  int
	header  *BTreePageHeader
	keySize int
}

// OpenIndex creates an IndexReader by scanning all index pages.
func (db *File) OpenIndex() (*IndexReader, error) {
	var roots []indexRoot

	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil {
			return nil, err
		}

		if page.Header == nil || page.Header.PageType != PageTypeIndex {
			continue
		}

		d := page.PageData()
		hdr, err := parseBTreeHeader(d)
		if err != nil {
			continue
		}

		if hdr.IsRoot {
			roots = append(roots, indexRoot{
				pageNo:  i,
				header:  hdr,
				keySize: int(hdr.KeyPrefixSize),
			})
		}
	}

	// Sort by page number for deterministic order.
	sort.Slice(roots, func(i, j int) bool {
		return roots[i].pageNo < roots[j].pageNo
	})

	return &IndexReader{db: db, indexes: roots}, nil
}

// Indexes returns information about all discovered indexes.
func (ir *IndexReader) Indexes() []IndexInfo {
	result := make([]IndexInfo, len(ir.indexes))

	for i, root := range ir.indexes {
		result[i] = IndexInfo{
			RootPageNo: root.pageNo,
			KeySize:    root.keySize,
			EntryCount: int(root.header.EntryCount),
			IsInternal: root.keySize == 4, // 4-byte keys = system page indexes
		}
	}

	return result
}

// PrimaryKeyIndex returns the root page info for the primary key index.
// The primary key index has 5-byte keys (1 null flag + 4-byte int32).
func (ir *IndexReader) PrimaryKeyIndex() (*indexRoot, error) {
	for i := range ir.indexes {
		if ir.indexes[i].keySize == 5 {
			return &ir.indexes[i], nil
		}
	}

	return nil, ErrNoIndex
}

// SecondaryIndexes returns root pages for non-system, non-primary indexes.
func (ir *IndexReader) SecondaryIndexes() []IndexInfo {
	var result []IndexInfo

	for _, root := range ir.indexes {
		if root.keySize != 4 && root.keySize != 5 { // not system, not primary
			result = append(result, IndexInfo{
				RootPageNo: root.pageNo,
				KeySize:    root.keySize,
				EntryCount: int(root.header.EntryCount),
			})
		}
	}

	return result
}

// FindByPrimaryKey looks up a record by its primary key (AutoInc/RecNo value).
// Returns the data page number and item number, or ErrKeyNotFound.
func (ir *IndexReader) FindByPrimaryKey(key int32) (dataPageNo int32, itemNo uint16, err error) {
	root, err := ir.PrimaryKeyIndex()
	if err != nil {
		return 0, 0, err
	}

	// Build the 5-byte search key: [00] + int32 LE.
	searchKey := make([]byte, 5)
	binary.LittleEndian.PutUint32(searchKey[1:], uint32(key))

	entry, err := ir.searchBTree(root.pageNo, searchKey)
	if err != nil {
		return 0, 0, err
	}

	return entry.PageNo, entry.ItemNo, nil
}

// FindByStringKey searches a secondary string index for the given value.
// Uses the first secondary index found with matching key size.
func (ir *IndexReader) FindByStringKey(value string) (dataPageNo int32, itemNo uint16, err error) {
	secondaries := ir.SecondaryIndexes()
	if len(secondaries) == 0 {
		return 0, 0, ErrNoIndex
	}

	idx := secondaries[0]
	searchKey := makeStringKey(value, idx.KeySize)

	entry, err := ir.searchBTreeString(idx.RootPageNo, searchKey)
	if err != nil {
		return 0, 0, err
	}

	return entry.PageNo, entry.ItemNo, nil
}

// ScanIndex reads all entries from the specified index root page,
// following the B-tree leaf chain for multi-page indexes.
func (ir *IndexReader) ScanIndex(rootPageNo int) ([]BTreeEntry, error) {
	return ir.scanLeaves(rootPageNo)
}

// searchBTree performs a B-tree search starting from the given root page.
func (ir *IndexReader) searchBTree(rootPageNo int, searchKey []byte) (BTreeEntry, error) {
	pageNo := rootPageNo

	for {
		page, err := ir.db.ReadPage(pageNo)
		if err != nil {
			return BTreeEntry{}, err
		}

		d := page.PageData()
		hdr, err := parseBTreeHeader(d)
		if err != nil {
			return BTreeEntry{}, err
		}

		entries := readBTreeEntries(d, hdr)

		if hdr.IsLeaf {
			// Binary search in leaf entries.
			idx := sort.Search(len(entries), func(i int) bool {
				return bytes.Compare(entries[i].Key, searchKey) >= 0
			})

			if idx < len(entries) && bytes.Equal(entries[idx].Key, searchKey) {
				return entries[idx], nil
			}

			return BTreeEntry{}, ErrKeyNotFound
		}

		// Internal node: find the child to descend into.
		// Keys in internal nodes act as separators. Find the rightmost key <= searchKey.
		childIdx := sort.Search(len(entries), func(i int) bool {
			return bytes.Compare(entries[i].Key, searchKey) > 0
		})

		if childIdx == 0 {
			// Search key is less than all keys — go to left subtree.
			// The leftmost child is referenced by entries[0].
			pageNo = int(entries[0].PageNo)
		} else {
			pageNo = int(entries[childIdx-1].PageNo)
		}
	}
}

// scanLeaves reads all leaf entries starting from the root, following the
// leaf chain via RightPageNo links.
func (ir *IndexReader) scanLeaves(rootPageNo int) ([]BTreeEntry, error) {
	// First, find the leftmost leaf.
	leafPageNo, err := ir.findLeftmostLeaf(rootPageNo)
	if err != nil {
		return nil, err
	}

	// Then scan all leaves via the right-page chain.
	var allEntries []BTreeEntry
	pageNo := leafPageNo

	for pageNo >= 0 {
		page, err := ir.db.ReadPage(pageNo)
		if err != nil {
			return nil, err
		}

		d := page.PageData()
		hdr, err := parseBTreeHeader(d)
		if err != nil {
			return nil, err
		}

		entries := readBTreeEntries(d, hdr)
		allEntries = append(allEntries, entries...)

		pageNo = int(hdr.RightPageNo)
	}

	return allEntries, nil
}

// findLeftmostLeaf descends the tree always taking the leftmost child.
func (ir *IndexReader) findLeftmostLeaf(pageNo int) (int, error) {
	for {
		page, err := ir.db.ReadPage(pageNo)
		if err != nil {
			return 0, err
		}

		d := page.PageData()
		hdr, err := parseBTreeHeader(d)
		if err != nil {
			return 0, err
		}

		if hdr.IsLeaf {
			return pageNo, nil
		}

		entries := readBTreeEntries(d, hdr)
		if len(entries) == 0 {
			return 0, fmt.Errorf("absdb: empty internal node at page %d", pageNo)
		}

		pageNo = int(entries[0].PageNo)
	}
}

// searchBTreeString performs a B-tree search for string keys.
// String keys have garbage bytes after the null terminator, so we compare
// only up to the null terminator in the string portion (after byte 0).
func (ir *IndexReader) searchBTreeString(rootPageNo int, searchKey []byte) (BTreeEntry, error) {
	pageNo := rootPageNo

	for {
		page, err := ir.db.ReadPage(pageNo)
		if err != nil {
			return BTreeEntry{}, err
		}

		d := page.PageData()
		hdr, err := parseBTreeHeader(d)
		if err != nil {
			return BTreeEntry{}, err
		}

		entries := readBTreeEntries(d, hdr)

		if hdr.IsLeaf {
			for _, e := range entries {
				if compareStringKeys(e.Key, searchKey) == 0 {
					return e, nil
				}
			}

			return BTreeEntry{}, ErrKeyNotFound
		}

		// Internal node.
		childIdx := sort.Search(len(entries), func(i int) bool {
			return compareStringKeys(entries[i].Key, searchKey) > 0
		})

		if childIdx == 0 {
			pageNo = int(entries[0].PageNo)
		} else {
			pageNo = int(entries[childIdx-1].PageNo)
		}
	}
}

// compareStringKeys compares two string index keys.
// Keys have format: [null_flag] + null-terminated string + garbage.
// We compare the null flag byte, then the string up to the first null terminator.
func compareStringKeys(a, b []byte) int {
	if len(a) == 0 || len(b) == 0 {
		return bytes.Compare(a, b)
	}

	// Compare null flag byte.
	if a[0] != b[0] {
		if a[0] < b[0] {
			return -1
		}

		return 1
	}

	// Extract strings (up to null terminator).
	strA := extractNullTerminated(a[1:])
	strB := extractNullTerminated(b[1:])

	return bytes.Compare(strA, strB)
}

// extractNullTerminated returns the bytes up to (not including) the first null byte.
func extractNullTerminated(data []byte) []byte {
	for i, b := range data {
		if b == 0 {
			return data[:i]
		}
	}

	return data
}

// makeStringKey creates a search key for string indexes.
// The key format is: [00] + Windows-1252 string padded/truncated to keySize-1 bytes.
func makeStringKey(value string, keySize int) []byte {
	key := make([]byte, keySize)
	// First byte is a null flag (0 = not null).
	key[0] = 0
	// Copy the string value, truncating if needed.
	n := copy(key[1:], []byte(value))
	// Null-terminate and zero-pad the rest.
	for i := n + 1; i < keySize; i++ {
		key[i] = 0
	}

	return key
}
