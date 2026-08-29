package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

const (
	// tableListEntrySize is the size of one TABSTableListItem in the catalog.
	// It was measured, not guessed: the catalog's internal file is exactly this
	// long in every single-table fixture, and its layout below accounts for all
	// 272 bytes.
	tableListEntrySize = 272

	// tableNameFieldSize is the width of an entry's name field. It is a Delphi
	// ShortString: one length byte followed by 255 bytes of storage, of which
	// only the first Length are meaningful. The rest is whatever the engine's
	// record buffer happened to hold and must not be read.
	tableNameFieldSize = 256

	// maxCatalogPages bounds the page chain the catalog may span. The catalog
	// is one internal file like any other, so a corrupted NextPageNo could
	// otherwise walk the whole file.
	maxCatalogPages = 64

	// maxCatalogTables bounds the entry count parsed out of one catalog, so a
	// corrupted length field cannot make Tables allocate without limit.
	maxCatalogTables = 4096
)

var (
	// ErrNoCatalog is returned when the file holds no table catalog page.
	ErrNoCatalog = errors.New("absdb: no table catalog page found")

	// ErrBadCatalog is returned when the catalog page cannot be parsed.
	ErrBadCatalog = errors.New("absdb: malformed table catalog")

	// ErrNoSuchTable is returned when a name matches no catalog entry.
	ErrNoSuchTable = errors.New("absdb: no such table")

	// ErrAmbiguousTable is returned when a table is selected by the empty name
	// but the database holds more than one, so there is no "the" table.
	ErrAmbiguousTable = errors.New("absdb: database holds more than one table")
)

// TableInfo is one entry of the database's table catalog.
//
// The catalog lives in the type-6 system internal file, a plain array of
// 272-byte TABSTableListItem records with no count field of its own: the
// internal file header's decompressed length divides by the entry size to give
// the number of tables.
type TableInfo struct {
	// Name is the table's name, Windows-1252 decoded.
	//
	// The SoundPlan fixtures store the file's own name here, extension and all
	// ("RCON0011.abs"), because each of their tables lives in its own database.
	// That is what the engine was given, not something this package adds.
	Name string

	// ID is the table's identifier, and is what ABSP.ObjectID holds on each of
	// its data pages. It is the only field that partitions pages by table:
	// schema, table-info, index and BLOB pages all carry ObjectID 0xFFFFFFFF.
	ID int

	// SchemaPageNo is the page holding this table's compressed column
	// definitions (page type 8).
	SchemaPageNo int

	// InfoPageNo is the page holding this table's record and change counters
	// (page type 9).
	InfoPageNo int

	// systemPageNo is the fourth int32 of the entry. It points at a type-7
	// system internal file in every fixture, and its role is unidentified, so
	// it is not exported.
	systemPageNo int
}

// Tables returns the database's tables in catalog order.
func (db *File) Tables() ([]TableInfo, error) {
	raw, err := db.readCatalog()
	if err != nil {
		return nil, err
	}

	return parseTableList(raw)
}

// readCatalog returns the decompressed contents of the catalog internal file,
// following its page chain when it does not fit in one page.
func (db *File) readCatalog() ([]byte, error) {
	pageNo, err := db.findPageByType(PageTypeTableList)
	if err != nil {
		return nil, err
	}

	if pageNo < 0 {
		return nil, ErrNoCatalog
	}

	data, err := db.readInternalFilePages(pageNo)
	if err != nil {
		return nil, err
	}

	raw, err := decompressInternalFile(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCatalog, err)
	}

	return raw, nil
}

// readInternalFilePages concatenates the payloads of an internal file's page
// chain, starting at first. One page is the common case; the chain exists
// because an internal file larger than a payload is continued on the page its
// NextPageNo names.
func (db *File) readInternalFilePages(first int) ([]byte, error) {
	page, err := db.ReadPage(first)
	if err != nil {
		return nil, err
	}

	data := page.PageData()
	if len(data) < internalFileHeaderSize {
		return nil, ErrBadCatalog
	}

	// The header's own lengths say how much of the chain is worth reading, so
	// a chain is followed only while bytes are still missing.
	want := int64(data[0]) + int64(binary.LittleEndian.Uint32(data[1:5]))

	out := append([]byte(nil), data...)

	visited := map[int]bool{first: true}
	next := int(nextPageNo(page))

	for int64(len(out)) < want && next >= 0 {
		if visited[next] {
			return nil, fmt.Errorf("%w: page %d visited twice", ErrBadCatalog, next)
		}

		if len(visited) >= maxCatalogPages {
			return nil, fmt.Errorf("%w: chain longer than %d pages", ErrBadCatalog, maxCatalogPages)
		}

		visited[next] = true

		page, err = db.ReadPage(next)
		if err != nil {
			return nil, err
		}

		out = append(out, page.PageData()...)
		next = int(nextPageNo(page))
	}

	return out, nil
}

// internalFilePageChain walks a page chain the same way internalFilePages
// does, but through a pageEdit's buffers so a link this edit set but has not
// flushed to disk yet is still followed.
func (db *File) internalFilePageChain(w *pageEdit, first int) ([]int, error) {
	pages := []int{first}
	visited := map[int]bool{first: true}

	for {
		buf, err := w.load(pages[len(pages)-1])
		if err != nil {
			return nil, err
		}

		next := int32(binary.LittleEndian.Uint32(buf.raw[diskPageHeaderOffset+10 : diskPageHeaderOffset+14]))
		if next < 0 {
			return pages, nil
		}

		if visited[int(next)] {
			return nil, fmt.Errorf("absdb: internal file at page %d visits page %d twice", first, next)
		}

		if len(pages) >= maxCatalogPages {
			return nil, fmt.Errorf("absdb: internal file at page %d is longer than %d pages", first, maxCatalogPages)
		}

		visited[int(next)] = true
		pages = append(pages, int(next))
	}
}

// writeInternalFilePages writes a complete internal file across a page chain
// starting at first, the mirror of readInternalFilePages. The chain already
// linked from first is followed, and grown or shrunk to fit data via
// resizeChain when its length no longer matches -- a caller writing a fresh
// internal file passes a chain resizeChain (or allocatePages) has already
// sized correctly, so neither path runs in that case.
//
// The chain's page type and owner are taken from the first page's own ABSP
// header, so first must already carry one -- allocatePages gives a newly
// allocated page one before this is ever called on it.
func (db *File) writeInternalFilePages(w *pageEdit, first int, data []byte) error {
	head, err := w.load(first)
	if err != nil {
		return err
	}

	pageType := binary.LittleEndian.Uint16(head.raw[diskPageHeaderOffset+8 : diskPageHeaderOffset+10])
	objectID := int32(binary.LittleEndian.Uint32(head.raw[diskPageHeaderOffset+22 : diskPageHeaderOffset+26]))

	capacity := len(head.payload)
	if capacity <= 0 {
		return fmt.Errorf("%w: page %d has no payload capacity", ErrBadCatalog, first)
	}

	pages, err := db.internalFilePageChain(w, first)
	if err != nil {
		return err
	}

	need := max((len(data)+capacity-1)/capacity, 1)

	pages, err = db.resizeChain(w, pages, need, pageType, objectID)
	if err != nil {
		return err
	}

	off := 0

	for _, no := range pages {
		buf, err := w.load(no)
		if err != nil {
			return err
		}

		n := min(capacity, len(data)-off)

		copy(buf.payload[:n], data[off:off+n])

		off += n
		buf.dirty = true
	}

	return nil
}

// appendCatalogEntry adds one entry to the table catalog's internal file,
// growing its stored/decompressed length by tableListEntrySize -- the mirror
// of removeCatalogEntry, which shrinks it the same way. Like that function, it
// only supports a catalog stored uncompressed on a single page, which is the
// only shape any fixture carries.
func appendCatalogEntry(payload []byte, info TableInfo) error {
	if len(payload) < internalFileHeaderSize {
		return fmt.Errorf("%w: catalog page is %d bytes", ErrCatalogNotWritable, len(payload))
	}

	headerSize := int(payload[0])
	stored := int(int32(binary.LittleEndian.Uint32(payload[1:5])))
	decompressed := int(int32(binary.LittleEndian.Uint32(payload[5:9])))
	algorithm := payload[9]

	switch {
	case algorithm != 0 || stored != decompressed:
		return fmt.Errorf("%w: it is compressed with algorithm %d", ErrCatalogNotWritable, algorithm)
	case headerSize < internalFileHeaderSize || stored < 0 || stored%tableListEntrySize != 0:
		return fmt.Errorf("%w: %d bytes is not a whole number of entries", ErrCatalogNotWritable, stored)
	case headerSize+stored+tableListEntrySize > len(payload):
		return fmt.Errorf("%w: no room for another %d-byte entry", ErrCatalogNotWritable, tableListEntrySize)
	}

	entry := payload[headerSize+stored : headerSize+stored+tableListEntrySize]

	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(info.Name))
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrStringEncoding, info.Name, err)
	}

	if len(raw) >= tableNameFieldSize {
		return fmt.Errorf("%w: %d bytes of name in a %d-byte field", ErrValueRange, len(raw), tableNameFieldSize-1)
	}

	entry[0] = byte(len(raw)) //nolint:gosec // checked above: len(raw) < tableNameFieldSize (256)
	copy(entry[1:], raw)

	fields := entry[tableNameFieldSize:]
	binary.LittleEndian.PutUint32(fields[0:4], uint32(int32(info.ID)))             //nolint:gosec // small object ids
	binary.LittleEndian.PutUint32(fields[4:8], uint32(int32(info.SchemaPageNo)))   //nolint:gosec // page numbers
	binary.LittleEndian.PutUint32(fields[8:12], uint32(int32(info.InfoPageNo)))    //nolint:gosec // page numbers
	binary.LittleEndian.PutUint32(fields[12:16], uint32(int32(info.systemPageNo))) //nolint:gosec // page numbers

	newLen := uint32(stored + tableListEntrySize) //nolint:gosec // bounded by the page-capacity check above

	binary.LittleEndian.PutUint32(payload[1:5], newLen)
	binary.LittleEndian.PutUint32(payload[5:9], newLen)

	return nil
}

// nextPageNo returns a page's chain successor, or -1 when it has no header.
func nextPageNo(page Page) int32 {
	if page.Header == nil {
		return -1
	}

	return page.Header.NextPageNo
}

// parseTableList splits the catalog's decompressed bytes into entries.
func parseTableList(raw []byte) ([]TableInfo, error) {
	if len(raw)%tableListEntrySize != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a whole number of %d-byte entries",
			ErrBadCatalog, len(raw), tableListEntrySize)
	}

	count := len(raw) / tableListEntrySize
	if count > maxCatalogTables {
		return nil, fmt.Errorf("%w: %d entries", ErrBadCatalog, count)
	}

	tables := make([]TableInfo, 0, count)

	for i := range count {
		entry := raw[i*tableListEntrySize : (i+1)*tableListEntrySize]

		// The length is one byte and the field holds 255, so a name can never
		// run past its own field and needs no bounds check.
		nameLen := int(entry[0])
		fields := entry[tableNameFieldSize:]

		tables = append(tables, TableInfo{
			Name:         decodeANSI(entry[1 : 1+nameLen]),
			ID:           int(int32(binary.LittleEndian.Uint32(fields[0:4]))),
			SchemaPageNo: int(int32(binary.LittleEndian.Uint32(fields[4:8]))),
			InfoPageNo:   int(int32(binary.LittleEndian.Uint32(fields[8:12]))),
			systemPageNo: int(int32(binary.LittleEndian.Uint32(fields[12:16]))),
		})
	}

	return tables, nil
}

// anyTableID is the ID a Table carries when the file has no catalog to say
// otherwise. It matches every page, which is exactly the behaviour this
// package had before the catalog was parsed at all.
const anyTableID = -1

// Table is a handle to one table of a database. It is the scope every read and
// write is performed in: a Reader built from it sees only its own data pages,
// and a TableWriter built from it advances only its own counters.
type Table struct {
	db   *File
	info TableInfo

	// sole is set when this is the file's only table, which makes every page
	// in the file unambiguously its own — including the ones whose ABSP header
	// records no owner.
	sole bool

	// unlisted is set when the file has no usable catalog. The handle then
	// behaves as this package did before Tables existed: the first schema page
	// is the schema, the first table-info page holds the counters, and every
	// data page belongs to the table. Synthetic and fuzzed files reach this
	// path; no real one does.
	unlisted bool
}

// Table returns a handle to the named table. Names are matched without regard
// to case, as the engine's own SQL does.
//
// An empty name selects the database's only table. It reports ErrAmbiguousTable
// when there is more than one, which is the case this package used to get
// silently wrong: it read every data page in the file through the first table's
// schema, so a second table's rows came back as the first table's garbage.
func (db *File) Table(name string) (*Table, error) {
	tables, err := db.Tables()
	if err != nil {
		// A file with no readable catalog is still worth reading, so long as
		// the caller is not asking for a table by name — there is nothing to
		// match a name against.
		if name == "" && (errors.Is(err, ErrNoCatalog) || errors.Is(err, ErrBadCatalog)) {
			return &Table{
				db:       db,
				info:     TableInfo{ID: anyTableID, SchemaPageNo: -1, InfoPageNo: -1},
				unlisted: true,
			}, nil
		}

		return nil, err
	}

	if name == "" {
		switch len(tables) {
		case 0:
			return nil, fmt.Errorf("%w: the catalog is empty", ErrNoSuchTable)
		case 1:
			return &Table{db: db, info: tables[0], sole: true}, nil
		default:
			return nil, fmt.Errorf("%w: %d tables, name one of them", ErrAmbiguousTable, len(tables))
		}
	}

	for _, t := range tables {
		if strings.EqualFold(t.Name, name) {
			return &Table{db: db, info: t, sole: len(tables) == 1}, nil
		}
	}

	return nil, fmt.Errorf("%w: %q", ErrNoSuchTable, name)
}

// Info returns the catalog entry this handle was built from.
func (t *Table) Info() TableInfo {
	return t.info
}

// Name returns the table's name, which is empty for a file with no catalog.
func (t *Table) Name() string {
	return t.info.Name
}

// owns reports whether a page belongs to this table. Only data pages carry an
// ObjectID; every other page type carries 0xFFFFFFFF, so this answers the
// question for data pages alone and callers must not ask it of others.
func (t *Table) owns(page Page) bool {
	if page.Header == nil {
		return false
	}

	if t.info.ID == anyTableID {
		return true
	}

	return int(page.Header.ObjectID) == t.info.ID
}

// dataPages returns this table's data pages, in file order.
func (t *Table) dataPages() ([]int, error) {
	var pages []int

	for i := range t.db.PageCount() {
		page, err := t.db.ReadPage(i)
		if err != nil {
			return nil, err
		}

		// A freed page keeps its type and its ObjectID, so a table created
		// after a DROP that reuses the dropped table's ID would otherwise
		// inherit its rows.
		if page.Header != nil && page.Header.PageType == PageTypeData && t.owns(page) && !page.Freed() {
			pages = append(pages, i)
		}
	}

	return pages, nil
}

// infoPageNo resolves the page holding this table's record and change
// counters, or -1 when the file has none. A file without a catalog falls back
// to the first table-info page, as this package did before.
func (t *Table) infoPageNo() (int, error) {
	if !t.unlisted {
		return t.info.InfoPageNo, nil
	}

	return t.db.findPageByType(PageTypeTableInfo)
}
