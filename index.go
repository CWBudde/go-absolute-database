package absdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	RootPageNo int      // root page of the B-tree
	KeySize    int      // key size in bytes
	EntryCount int      // entries on the root page (whole tree only for root-only trees)
	IsInternal bool     // true for system indexes (RecordPage, BlobPage)
	Name       string   // name from the schema definition (empty for system indexes)
	Columns    []string // covered columns in comparison order
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
	name    string
	columns []string
	primary bool
}

// info converts a discovered root into its public description.
func (r *indexRoot) info() IndexInfo {
	return IndexInfo{
		RootPageNo: r.pageNo,
		KeySize:    r.keySize,
		EntryCount: int(r.header.EntryCount),
		IsInternal: r.keySize == systemKeySize,
		Name:       r.name,
		Columns:    append([]string(nil), r.columns...),
	}
}

// applyDefinition joins the schema stream's description of a user index to
// the B-tree root discovered from the page scan. Index pages do not carry a
// name or covered-column list themselves; the root page number is the field
// shared by both structures.
func (r *indexRoot) applyDefinition(rec indexRecord) {
	r.name = rec.name
	r.primary = rec.primary
	r.columns = make([]string, len(rec.columns))

	for i, col := range rec.columns {
		r.columns[i] = col.name
	}
}

// OpenIndex creates an IndexReader by scanning all index pages.
func (db *File) OpenIndex() (*IndexReader, error) {
	var roots []indexRoot
	var schemaPages []int

	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil {
			return nil, err
		}

		if page.Header == nil || page.Header.PageType != PageTypeIndex {
			if page.Header != nil && page.Header.PageType == PageTypeSchema {
				schemaPages = append(schemaPages, i)
			}

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
				name:    "",
				columns: nil,
				primary: false,
			})
		}
	}

	// Sort by page number for deterministic order.
	sort.Slice(roots, func(i, j int) bool {
		return roots[i].pageNo < roots[j].pageNo
	})

	// Metadata is best-effort at file scope. OpenIndex historically succeeds
	// on files whose schema tail is unknown, and a malformed definition must
	// not hide otherwise readable B-tree pages. Table.OpenIndex repeats the
	// join against its one authoritative schema and uses it for ownership.
	byPage := make(map[int]*indexRoot, len(roots))
	for i := range roots {
		byPage[roots[i].pageNo] = &roots[i]
	}

	for _, pageNo := range schemaPages {
		records, err := db.indexRecords(pageNo)
		if err != nil {
			continue
		}

		for _, rec := range records {
			if root := byPage[int(rec.rootPageNo)]; root != nil {
				root.applyDefinition(rec)
			}
		}
	}

	return &IndexReader{db: db, indexes: roots}, nil
}

// indexRecords reads the index-definition array from one table's schema
// stream. The caller supplies the schema head page so this works for both a
// named Table and the file-wide best-effort metadata pass above.
func (db *File) indexRecords(schemaPageNo int) ([]indexRecord, error) {
	raw, err := db.readSchemaStream(schemaPageNo)
	if err != nil {
		return nil, err
	}

	_, _, records, _, _, err := parseSchemaTail(raw) //nolint:dogsled // only index definitions belong here
	if err != nil {
		return nil, err
	}

	return records, nil
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

// PrimaryKeyIndex returns the user index whose schema definition marks it as
// the primary key. A file-scoped reader whose schema metadata could not be
// decoded falls back to the historical 5-byte-key heuristic.
func (ir *IndexReader) PrimaryKeyIndex() (IndexInfo, error) {
	for _, root := range ir.indexes {
		if root.primary {
			return root.info(), nil
		}
	}

	for _, root := range ir.indexes {
		if root.name == "" && root.keySize == primaryKeySize {
			return root.info(), nil
		}
	}

	return IndexInfo{}, ErrNoIndex
}

// SecondaryIndexes returns the user indexes other than the primary key index.
// Parsed definitions provide the distinction directly; roots without schema
// metadata retain the historical 5-byte-key heuristic.
func (ir *IndexReader) SecondaryIndexes() []IndexInfo {
	var result []IndexInfo

	for _, root := range ir.indexes {
		if root.keySize == systemKeySize || root.primary {
			continue
		}

		if root.name == "" && root.keySize == primaryKeySize {
			continue
		}

		result = append(result, root.info())
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

// FindByStringKey searches the single-column user index that covers column for
// value. Column names are matched case-insensitively, like table and column
// names elsewhere in the package. A compound index is not selected: building
// its search key needs the full component layout that PLAN.md still leaves to
// a row-bearing engine fixture.
func (ir *IndexReader) FindByStringKey(column, value string) (dataPageNo int32, itemNo uint16, err error) {
	var idx *indexRoot

	for i := range ir.indexes {
		root := &ir.indexes[i]
		if root.keySize == systemKeySize || len(root.columns) != 1 ||
			!strings.EqualFold(root.columns[0], column) {
			continue
		}

		idx = root

		break
	}

	if idx == nil {
		return 0, 0, ErrNoIndex
	}

	searchKey, err := makeStringKey(value, idx.keySize)
	if err != nil {
		return 0, 0, err
	}

	entry, err := ir.searchTree(idx.pageNo, searchKey, compareStringKeys)
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
//
// indexableKeyColumn reports whether an index over this column is one this
// package builds and maintains: a 5-byte Int32 key, ordered by
// compareInt32Keys.
//
// An AUTOINC column is included because its key is an Integer one byte for
// byte -- Auto.abs's index record and leaf are the Int32 shape exactly. What
// kept it out was not the key but the column's counter, which the engine keeps
// in the table info file and this package now maintains (writer_autoinc.go).
//
// The rule had four copies before this one: the writer's index maintenance,
// CREATE INDEX, CREATE TABLE's key constraints and the compaction rebuild. They
// have to agree -- a rebuild that builds an index the writer will not maintain
// produces a table nothing can insert into -- so they share this.
func indexableKeyColumn(col Column) bool {
	return col.BaseType == BftInt32 &&
		(col.FieldType == FieldInteger || col.FieldType == FieldAutoInc)
}

// A NULL key sorts before every value, which is the opposite of what comparing
// the flag byte as a number gives. No index in the corpus held one until
// testdata/Keys-uniqnull.abs, whose UNIQUE index stores the NULL entry ahead
// of 10, 20 and 30; before it, this ordered NULL last and nothing noticed.
func compareInt32Keys(a, b []byte) int {
	if len(a) < primaryKeySize || len(b) < primaryKeySize {
		return bytes.Compare(a, b)
	}

	if (a[0] == 0) != (b[0] == 0) {
		if a[0] != 0 {
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
// User indexes are also named by root page in this table's schema stream, so
// that definition is authoritative even when an empty leaf offers no record
// references. System indexes have no such definition and retain the evidence
// rule; in particular a BLOB page index can still be unattributable in a
// multi-table database. A single-table file returns every root because there
// is no other table it could belong to.
func (t *Table) OpenIndex() (*IndexReader, error) {
	ir, err := t.db.OpenIndex()
	if err != nil {
		return nil, err
	}

	// The table's own schema is the authority for its user indexes. Besides
	// supplying public metadata, this identifies an empty index whose leaf has
	// no record references from which ownership could be inferred.
	var definitions map[int]indexRecord

	schemaPageNo, schemaErr := t.schemaPageNo()
	if schemaErr == nil {
		records, recordsErr := t.db.indexRecords(schemaPageNo)
		if recordsErr == nil {
			definitions = make(map[int]indexRecord, len(records))
			for _, rec := range records {
				definitions[int(rec.rootPageNo)] = rec
			}

			for i := range ir.indexes {
				if rec, ok := definitions[ir.indexes[i].pageNo]; ok {
					ir.indexes[i].applyDefinition(rec)
				}
			}
		}
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
		if _, named := definitions[root.pageNo]; named {
			kept = append(kept, root)
			continue
		}

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
