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

	// leafEntrySuffixSize is the size of the reference stored after the key in
	// a leaf entry: PageNo int32 + ItemNo uint16.
	leafEntrySuffixSize = 6

	// internalEntrySuffixSize is the size of the reference stored after the key
	// in an internal node entry: the child PageNo int32. Internal nodes carry
	// no item number, so their entries are two bytes shorter than leaf entries.
	internalEntrySuffixSize = 4

	// systemKeySize is the key size of the engine's internal page indexes.
	// Their PageNo values are engine internals, not data page numbers.
	systemKeySize = 4

	// primaryKeySize is the key size of a single-column int32 primary key
	// index: one null flag byte plus the int32 value.
	primaryKeySize = 5

	// maxTreeDepth bounds a root-to-leaf descent. Real trees are two or three
	// levels deep; anything deeper means the page links form a cycle or the
	// file is corrupt.
	maxTreeDepth = 64
)

var (
	// ErrNoIndex is returned when the requested index does not exist.
	ErrNoIndex = errors.New("absdb: no index found")

	// ErrKeyNotFound is returned when a lookup finds no matching entry.
	ErrKeyNotFound = errors.New("absdb: key not found")

	// ErrMalformedIndex reports a structurally invalid index: a cyclic page
	// chain, a descent deeper than maxTreeDepth, an empty internal node, a
	// non-leaf page in the leaf chain or a zero-length key.
	ErrMalformedIndex = errors.New("absdb: malformed index")
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
	ItemNo uint16 // referenced item number within the page (leaf entries only)
}

// RecordID returns the entry's reference as a (PageNo, ItemNo) pair.
func (e BTreeEntry) RecordID() (int32, uint16) {
	return e.PageNo, e.ItemNo
}

// IndexInfo describes a discovered index.
type IndexInfo struct {
	RootPageNo int  // root page of the B-tree
	KeySize    int  // key size in bytes
	EntryCount int  // entries on the root page (whole tree only for root-only trees)
	IsInternal bool // true for system indexes (RecordPage, BlobPage)
}

// parseBTreeHeader reads the TABSBTreePageHeader from index page data.
func parseBTreeHeader(data []byte) (*BTreePageHeader, error) {
	if len(data) < btreeHeaderSize {
		return nil, fmt.Errorf("%w: index page too short (%d bytes)", ErrMalformedIndex, len(data))
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

// entryStride returns the on-disk size of one entry on the given node. Leaves
// store a full record reference (PageNo + ItemNo) behind the key, internal
// nodes only the child PageNo.
func entryStride(hdr *BTreePageHeader) int {
	if hdr.IsLeaf {
		return int(hdr.KeyPrefixSize) + leafEntrySuffixSize
	}

	return int(hdr.KeyPrefixSize) + internalEntrySuffixSize
}

// readBTreeEntries reads the entries of one index page.
//
// The number of entries read is bounded by what the page can physically hold,
// never by the untrusted EntryCount field alone, so a crafted page cannot force
// a large allocation.
func readBTreeEntries(data []byte, hdr *BTreePageHeader) ([]BTreeEntry, error) {
	if len(data) < btreeHeaderSize {
		return nil, fmt.Errorf("%w: index page too short (%d bytes)", ErrMalformedIndex, len(data))
	}

	keySize := int(hdr.KeyPrefixSize)
	if keySize == 0 {
		return nil, fmt.Errorf("%w: zero key size", ErrMalformedIndex)
	}

	stride := entryStride(hdr)

	count := int(hdr.EntryCount)
	if capacity := (len(data) - btreeHeaderSize) / stride; count > capacity {
		count = capacity
	}

	entries := make([]BTreeEntry, 0, count)

	for i := range count {
		off := btreeHeaderSize + i*stride

		key := make([]byte, keySize)
		copy(key, data[off:off+keySize])

		entry := BTreeEntry{
			Key:    key,
			PageNo: int32(binary.LittleEndian.Uint32(data[off+keySize : off+keySize+4])),
		}

		if hdr.IsLeaf {
			entry.ItemNo = binary.LittleEndian.Uint16(data[off+keySize+4 : off+keySize+6])
		}

		entries = append(entries, entry)
	}

	return entries, nil
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

// info converts a discovered root into its public description.
func (r indexRoot) info() IndexInfo {
	return IndexInfo{
		RootPageNo: r.pageNo,
		KeySize:    r.keySize,
		EntryCount: int(r.header.EntryCount),
		IsInternal: r.keySize == systemKeySize,
	}
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

// Indexes returns information about all discovered indexes, system indexes
// included. Use UserIndexes to get only the indexes over table rows.
func (ir *IndexReader) Indexes() []IndexInfo {
	result := make([]IndexInfo, len(ir.indexes))

	for i, root := range ir.indexes {
		result[i] = root.info()
	}

	return result
}

// UserIndexes returns every index defined over table rows, that is all
// discovered indexes except the engine's internal page indexes (systemKeySize
// keys). Only user index entries reference real rows: the PageNo of a system
// index entry is an engine-internal value, not a data page number.
func (ir *IndexReader) UserIndexes() []IndexInfo {
	var result []IndexInfo

	for _, root := range ir.indexes {
		if root.keySize == systemKeySize {
			continue
		}

		result = append(result, root.info())
	}

	return result
}

// PrimaryKeyIndex returns the user index over the primary key: the one with
// primaryKeySize keys (1 null flag byte + int32). Tables whose primary key is
// composite have no such index and yield ErrNoIndex.
func (ir *IndexReader) PrimaryKeyIndex() (IndexInfo, error) {
	for _, root := range ir.indexes {
		if root.keySize == primaryKeySize {
			return root.info(), nil
		}
	}

	return IndexInfo{}, ErrNoIndex
}

// SecondaryIndexes returns the user indexes other than the primary key index,
// that is UserIndexes minus the primaryKeySize entry.
func (ir *IndexReader) SecondaryIndexes() []IndexInfo {
	var result []IndexInfo

	for _, idx := range ir.UserIndexes() {
		if idx.KeySize == primaryKeySize {
			continue
		}

		result = append(result, idx)
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

	// Build the search key: [null flag 00] + int32 LE.
	searchKey := make([]byte, primaryKeySize)
	binary.LittleEndian.PutUint32(searchKey[1:], uint32(key))

	entry, err := ir.searchTree(root.RootPageNo, searchKey, compareInt32Keys)
	if err != nil {
		return 0, 0, err
	}

	return entry.PageNo, entry.ItemNo, nil
}

// FindByStringKey searches a secondary string index for the given value.
//
// Known limitation: index definitions are not parsed yet, so an index cannot be
// mapped to the column it covers. The lookup therefore always uses the first
// secondary index of the table and silently returns ErrKeyNotFound for values
// of any other indexed column.
func (ir *IndexReader) FindByStringKey(value string) (dataPageNo int32, itemNo uint16, err error) {
	secondaries := ir.SecondaryIndexes()
	if len(secondaries) == 0 {
		return 0, 0, ErrNoIndex
	}

	idx := secondaries[0]

	searchKey, err := makeStringKey(value, idx.KeySize)
	if err != nil {
		return 0, 0, err
	}

	entry, err := ir.searchTree(idx.RootPageNo, searchKey, compareStringKeys)
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

// indexPage reads one index page and parses its B-tree header.
func (ir *IndexReader) indexPage(pageNo int) ([]byte, *BTreePageHeader, error) {
	page, err := ir.db.ReadPage(pageNo)
	if err != nil {
		return nil, nil, err
	}

	d := page.PageData()

	hdr, err := parseBTreeHeader(d)
	if err != nil {
		return nil, nil, err
	}

	return d, hdr, nil
}

// searchTree descends from rootPageNo to the leaf that may hold searchKey and
// returns the entry whose key compares equal under cmp, or ErrKeyNotFound.
// The descent is bounded by maxTreeDepth so a cyclic child link terminates.
func (ir *IndexReader) searchTree(
	rootPageNo int,
	searchKey []byte,
	cmp func(a, b []byte) int,
) (BTreeEntry, error) {
	pageNo := rootPageNo

	for range maxTreeDepth {
		d, hdr, err := ir.indexPage(pageNo)
		if err != nil {
			return BTreeEntry{}, err
		}

		entries, err := readBTreeEntries(d, hdr)
		if err != nil {
			return BTreeEntry{}, err
		}

		if hdr.IsLeaf {
			return findInLeaf(entries, searchKey, cmp)
		}

		if len(entries) == 0 {
			return BTreeEntry{}, fmt.Errorf("%w: empty internal node at page %d", ErrMalformedIndex, pageNo)
		}

		pageNo = childPageNo(entries, searchKey, cmp)
	}

	return BTreeEntry{}, fmt.Errorf("%w: descent from page %d exceeded %d levels",
		ErrMalformedIndex, rootPageNo, maxTreeDepth)
}

// findInLeaf binary-searches the sorted leaf entries for searchKey.
func findInLeaf(entries []BTreeEntry, searchKey []byte, cmp func(a, b []byte) int) (BTreeEntry, error) {
	idx := sort.Search(len(entries), func(i int) bool {
		return cmp(entries[i].Key, searchKey) >= 0
	})

	if idx < len(entries) && cmp(entries[idx].Key, searchKey) == 0 {
		return entries[idx], nil
	}

	return BTreeEntry{}, ErrKeyNotFound
}

// childPageNo picks the child to descend into. Internal node keys act as
// separators, so the target child is the one behind the rightmost key that is
// not greater than searchKey; keys below the first separator live in the
// leftmost child.
func childPageNo(entries []BTreeEntry, searchKey []byte, cmp func(a, b []byte) int) int {
	idx := sort.Search(len(entries), func(i int) bool {
		return cmp(entries[i].Key, searchKey) > 0
	})

	if idx == 0 {
		return int(entries[0].PageNo)
	}

	return int(entries[idx-1].PageNo)
}

// scanLeaves reads all leaf entries starting from the root, following the
// leaf chain via RightPageNo links. A page that is revisited or that is not a
// leaf aborts the scan with ErrMalformedIndex instead of looping forever.
func (ir *IndexReader) scanLeaves(rootPageNo int) ([]BTreeEntry, error) {
	pageNo, err := ir.findLeftmostLeaf(rootPageNo)
	if err != nil {
		return nil, err
	}

	var allEntries []BTreeEntry

	visited := make(map[int]struct{})

	for pageNo >= 0 {
		if _, seen := visited[pageNo]; seen {
			return nil, fmt.Errorf("%w: leaf chain revisits page %d", ErrMalformedIndex, pageNo)
		}

		visited[pageNo] = struct{}{}

		d, hdr, err := ir.indexPage(pageNo)
		if err != nil {
			return nil, err
		}

		if !hdr.IsLeaf {
			return nil, fmt.Errorf("%w: page %d in leaf chain is not a leaf", ErrMalformedIndex, pageNo)
		}

		entries, err := readBTreeEntries(d, hdr)
		if err != nil {
			return nil, err
		}

		allEntries = append(allEntries, entries...)
		pageNo = int(hdr.RightPageNo)
	}

	return allEntries, nil
}

// findLeftmostLeaf descends the tree always taking the leftmost child. The
// descent is bounded by maxTreeDepth so a cyclic child link terminates.
func (ir *IndexReader) findLeftmostLeaf(pageNo int) (int, error) {
	start := pageNo

	for range maxTreeDepth {
		d, hdr, err := ir.indexPage(pageNo)
		if err != nil {
			return 0, err
		}

		if hdr.IsLeaf {
			return pageNo, nil
		}

		entries, err := readBTreeEntries(d, hdr)
		if err != nil {
			return 0, err
		}

		if len(entries) == 0 {
			return 0, fmt.Errorf("%w: empty internal node at page %d", ErrMalformedIndex, pageNo)
		}

		pageNo = int(entries[0].PageNo)
	}

	return 0, fmt.Errorf("%w: descent from page %d exceeded %d levels",
		ErrMalformedIndex, start, maxTreeDepth)
}

// compareInt32Keys compares two int32 index keys: a null flag byte followed by
// the value in little-endian byte order. Byte-wise comparison must not be used
// for these keys — the least significant byte comes first, so it orders 256
// before 2 — while the entries on a page are sorted by value.
func compareInt32Keys(a, b []byte) int {
	if len(a) < primaryKeySize || len(b) < primaryKeySize {
		return bytes.Compare(a, b)
	}

	if a[0] != b[0] {
		if a[0] < b[0] {
			return -1
		}

		return 1
	}

	av := int32(binary.LittleEndian.Uint32(a[1:primaryKeySize]))

	bv := int32(binary.LittleEndian.Uint32(b[1:primaryKeySize]))

	switch {
	case av < bv:
		return -1
	case av > bv:
		return 1
	default:
		return 0
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
func makeStringKey(value string, keySize int) ([]byte, error) {
	// One byte for the null flag plus at least one byte of string.
	if keySize < 2 {
		return nil, fmt.Errorf("%w: string key size %d too small", ErrMalformedIndex, keySize)
	}

	// The first byte stays 0 (the null flag: 0 = not null); the string is
	// copied behind it, truncated if it does not fit, and the rest stays zero,
	// which both terminates and pads the key.
	key := make([]byte, keySize)
	copy(key[1:], value)

	return key, nil
}

// OpenIndex creates an IndexReader over the indexes this table owns.
//
// Attribution is by evidence, not by ObjectID: an index page's ABSP header
// records no owner, so the only thing that ties an index to a table is where
// its entries point. A user index is this table's when its leaf entries name
// this table's data pages; the engine's internal record-page index is this
// table's when its keys are those pages.
//
// Two cases are therefore not returned for a multi-table database: an index
// whose leftmost leaf is empty, which offers no evidence either way, and the
// engine's BLOB page index, whose keys are BLOB pages and whose owner nothing
// in the file records. Neither arises for a single-table file, where every
// index in the file is returned because there is no other table it could
// belong to.
func (t *Table) OpenIndex() (*IndexReader, error) {
	ir, err := t.db.OpenIndex()
	if err != nil {
		return nil, err
	}

	if t.sole || t.unlisted {
		return ir, nil
	}

	pages, err := t.dataPages()
	if err != nil {
		return nil, err
	}

	own := make(map[int]bool, len(pages))
	for _, p := range pages {
		own[p] = true
	}

	var kept []indexRoot

	for _, root := range ir.indexes {
		ok, err := ir.rootReferences(root, own)
		if err != nil {
			return nil, err
		}

		if ok {
			kept = append(kept, root)
		}
	}

	return &IndexReader{db: t.db, indexes: kept}, nil
}

// rootReferences reports whether the leftmost leaf of an index tree points at
// pages in own. One leaf is enough: every entry of an index belongs to the same
// table, so the first one settles it without walking the whole tree.
func (ir *IndexReader) rootReferences(root indexRoot, own map[int]bool) (bool, error) {
	leafNo, err := ir.findLeftmostLeaf(root.pageNo)
	if err != nil {
		return false, err
	}

	data, hdr, err := ir.indexPage(leafNo)
	if err != nil {
		return false, err
	}

	entries, err := readBTreeEntries(data, hdr)
	if err != nil {
		return false, err
	}

	for _, e := range entries {
		// A system index keys pages by number and its PageNo is an internal
		// value; a user index does the reverse. Either field landing on one of
		// this table's data pages identifies the owner.
		if root.keySize == systemKeySize {
			if len(e.Key) >= 4 && own[int(int32(binary.LittleEndian.Uint32(e.Key[:4])))] {
				return true, nil
			}

			continue
		}

		if own[int(e.PageNo)] {
			return true, nil
		}
	}

	return false, nil
}
