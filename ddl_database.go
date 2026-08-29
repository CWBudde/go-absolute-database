package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// CREATE DATABASE -- writing an .abs file from nothing.
//
// Every other write in this package edits a file the engine made. This one has
// no base to start from, which makes it the tightest byte target in the corpus:
// a fresh database is almost entirely constant, so almost every byte of it is
// either measured or wrong. Five fixtures pin it, each a File -> Create
// Database in the DBManager with exactly one setting changed from the defaults:
//
//	Empty.abs                nothing -- 4096 / extent 8 / 500 connections
//	Empty-p2048-e4.abs       PageSize 2048, PageCountInExtent 4
//	Empty-mc100.abs          Max Connections 100
//	Empty-encrypted.abs      encrypted, Rijndael-128
//	Empty-p2048-e4-grow.abs  a CREATE TABLE on the 2048 one (ddl_grow.go)
//
// What a fresh database is
//
// Six pages, five allocated and one free, whatever the geometry -- both
// Empty.abs and Empty-p2048-e4.abs carry exactly six, so the count is not
// rounded up to a whole extent. LastUsedPageNo is 4, the header State is 1
// (one transaction: the creation itself) and LastObjectID is 0. The five
// allocated pages, in the order the engine allocates them:
//
//	page 0  type 3   the Page Free Space map
//	page 1  type 2   the Extent Allocation Map
//	page 2  type 4   the system file directory
//	page 3  type 5   the connection/lock table
//	page 4  type 6   the table catalog, empty
//
// The 380-byte header is nearly constant
//
// Empty.abs and Empty-p2048-e4.abs differ in the header by exactly two bytes,
// PageSize at offset 26 and PageCountInExtent at 28; nothing else in the header
// depends on the geometry. Empty.abs and Empty-mc100.abs do not differ in the
// header at all -- Max Connections is not a header field (see below). Only
// three fields are therefore variable at all, and everything else written here
// is a constant lifted straight off Empty.abs: the version (7.94), the
// documented 76-byte header size at offset 16, WriteChangesState 2 at offset
// 42, the crypto sub-header's own size (280) at offset 76, and a second size
// field of 20 at offset 356, which is exactly the gap left before LastObjectID
// at 376.
//
// The system pages
//
// An internal file opens with a ten-byte header: version, int32 Size, int32
// DecompressedSize, byte CompressionAlgorithm (schema.go).
//
//   - Page 2 is byte-identical in every database examined -- Empty.abs,
//     MultiTable-createidx.abs and MultiTable-dropcompact.abs all carry the
//     same twenty bytes, a ten-byte uncompressed internal file of two five-byte
//     entries: kind 2 -> page 3, kind 1 -> page 4. It is a fixed directory of
//     the two other system files and never changes.
//   - Page 3 is the connection/lock table: an internal file of MaxConnections
//     zero bytes. Empty-mc100.abs is the proof that this, and not any header
//     field, is where Max Connections lives: it differs from Empty.abs in
//     exactly four places -- the State words of pages 2, 3 and 4, which the
//     engine seeds randomly, and this file's Size field, 0x1F4 (500) -> 0x64
//     (100). Like the system internal file CREATE TABLE writes
//     (buildSystemInternalFile), it leaves DecompressedSize at 0 rather than
//     mirroring Size.
//   - Page 4 is the table catalog, a zero-length uncompressed internal file.
//
// The State counters
//
// Pages 0 and 1 carry counters rather than seeds, and the two geometries pin
// what they count. Page 0's State is 5 in both files: one bump per Page Free
// Space bit set. Page 1's State is 1 at 4096/extent 8 and 3 at 2048/extent 4:
// one bump per Extent Allocation Map entry that changed value. Three, not two,
// is what shows the five pages are allocated one at a time rather than in one
// batch -- extent 0 goes free -> partial at page 0 and partial -> full at page
// 3, and extent 1 goes free -> partial at page 4, which is three changes where
// a single batched allocation would record only two. Pages 2, 3 and 4 carry
// random seeds (newPageState, ddl.go) and are the only bytes of a fresh
// database this package cannot reproduce.
//
// Encryption is out of scope
//
// Empty-encrypted.abs locates the material and no more: offset 43 goes 0 ->
// 0xFF (not 1), and offsets 80..339 -- the crypto sub-header's 256-byte
// ControlBlock and its 4-byte CRC -- fill with 260 bytes that are all zero in
// an unencrypted file. What those 260 bytes are derived from is undecoded: the
// key derivation this package implements (crypto.go) turns a password into a
// working cipher, but nothing in the corpus says how the engine builds the
// control block it verifies a password against. Writing a guess would produce a
// file the engine refuses to open, so CreateDatabase refuses the request
// instead, with ErrEncryptionUnsupported.

const (
	// defaultPageSize, defaultPageCountInExtent and defaultMaxConnections are
	// the DBManager's own Create Database defaults, which is what Empty.abs
	// measures. They are also the values its dialog opens with
	// (legacy/Utils/Source/DBManager/uDatabase.dfm).
	defaultPageSize          = 4096
	defaultPageCountInExtent = 8
	defaultMaxConnections    = 500

	// createdVersion is the engine version stamped into a database this package
	// creates: 7.94, the version every DBManager-made fixture in the corpus
	// carries. The older 5.13 and 7.61 files are read, never written.
	createdVersion = 7.94

	// freshDatabasePageCount is how long a fresh database is: five allocated
	// pages and one free one. It is a constant and not a whole extent --
	// Empty.abs (extent 8) and Empty-p2048-e4.abs (extent 4) are both six pages
	// long.
	freshDatabasePageCount = 6

	// freshLastUsedPageNo and freshDatabaseState are the two header counters a
	// fresh database opens with: the highest allocated page, and one
	// transaction for the creation itself.
	freshLastUsedPageNo = 4
	freshDatabaseState  = 1

	// freshWriteChangesState is WriteChangesState at header offset 42. It is 2
	// in every fixture in the corpus, Empty.abs included, and nothing observed
	// ever moves it.
	freshWriteChangesState = 2

	// Header offsets parseHeader reads and this file is the only writer of.
	// TotalPageCount (30), LastUsedPageNo (34), State (38) and LastObjectID
	// (376) already have their own constants, in ddl_grow.go, ddl.go and
	// writer.go.
	headerSizeOffset    = 16
	versionOffset       = 18
	pageSizeOffset      = 26
	pagesInExtentOffset = 28
	writeChangesOffset  = 42

	// trailingHeaderSizeOffset and trailingHeaderSize are the third size field
	// of the database header, after the documented 76-byte one at offset 16 and
	// the crypto sub-header's 280 at offset 76. What the 20 bytes it introduces
	// hold is unidentified: they are zero in every fixture, and 356+20 is
	// exactly lastObjectIDOffset, so the block accounts for the whole gap
	// between the crypto header and LastObjectID.
	trailingHeaderSizeOffset = 356
	trailingHeaderSize       = 20
)

// The two page types a fresh database's middle pages carry. Neither has an
// exported name, because neither is anything a reader has to identify: they are
// the database's own two system internal files, they occur exactly once each,
// and no observed write modifies either after creation.
const (
	// pageTypeSystemFileDir is page 2: a directory naming the other two system
	// files by kind.
	pageTypeSystemFileDir = 4

	// pageTypeConnectionTable is page 3: MaxConnections bytes of lock table.
	pageTypeConnectionTable = 5
)

const (
	// The page numbers a fresh database's five system pages take. pfsPageNo (0)
	// and eamPageNo (1) are named in ddl.go.
	systemFileDirPageNo   = 2
	connectionTablePageNo = 3
	freshCatalogPageNo    = 4

	// systemDirEntrySize is one entry of the page-2 directory: a kind byte and
	// an int32 page number.
	systemDirEntrySize = 5

	// The two kinds that directory names, in the order it lists them.
	systemDirKindConnections = 2
	systemDirKindCatalog     = 1
)

var (
	// ErrEncryptionUnsupported reports a request to create an encrypted
	// database, or to compact one. The 260 bytes of key material an encrypted
	// file carries at header offsets 80..339 are located but undecoded; see
	// this file's comment.
	ErrEncryptionUnsupported = errors.New("absdb: creating an encrypted database is not supported")

	// ErrBadGeometry reports a CreateDatabaseOptions this package will not
	// build a file from: a page size the format cannot express or that the
	// system pages do not fit in, an extent of no pages, or a connection count
	// out of range.
	ErrBadGeometry = errors.New("absdb: invalid database geometry")
)

// CreateDatabaseOptions is the geometry of a database CreateDatabase makes.
// The zero value selects the DBManager's own defaults -- 4096-byte pages, 8
// pages to an extent, 500 connections -- which is what Empty.abs carries.
type CreateDatabaseOptions struct {
	// PageSize is the size of one page in bytes. 4096 (Empty.abs) and 2048
	// (Empty-p2048-e4.abs) are the two values the fixtures pin; any other value
	// large enough to hold the system pages is accepted but unmeasured.
	PageSize int

	// PageCountInExtent is how many pages an extent groups, which is the unit
	// the file grows by (ddl_grow.go). 8 and 4 are the two the fixtures pin.
	PageCountInExtent int

	// MaxConnections sizes the connection/lock table on page 3. 500 and 100 are
	// the two the fixtures pin.
	MaxConnections int

	// Encrypted requests an encrypted database, which is refused with
	// ErrEncryptionUnsupported. The field exists so that asking is an explicit
	// refusal rather than a silently unencrypted file.
	Encrypted bool
}

// geometry is CreateDatabaseOptions with the defaults filled in and the values
// checked, so that everything past normalize can assume them.
type geometry struct {
	pageSize       int
	perExtent      int
	maxConnections int
}

// payloadSize is the usable bytes of one page at this geometry, the mirror of
// File.payloadSize for a file that does not exist yet.
func (g geometry) payloadSize() int {
	return g.pageSize - diskPageHeaderSize
}

// normalize fills in the defaults and refuses a geometry this package will not
// build a file from.
func (o CreateDatabaseOptions) normalize() (geometry, error) {
	g := geometry{
		pageSize:       orDefault(o.PageSize, defaultPageSize),
		perExtent:      orDefault(o.PageCountInExtent, defaultPageCountInExtent),
		maxConnections: orDefault(o.MaxConnections, defaultMaxConnections),
	}

	switch {
	case o.Encrypted:
		return geometry{}, ErrEncryptionUnsupported
	case g.pageSize < pageDataOffset || g.pageSize > math.MaxUint16:
		// The lower bound is parseHeader's own: a page has to be able to hold
		// its disk page header and leave payload behind it. The upper bound is
		// the header field's width.
		return geometry{}, fmt.Errorf("%w: page size %d is not in [%d,%d]",
			ErrBadGeometry, g.pageSize, pageDataOffset, math.MaxUint16)
	case g.perExtent < 1 || g.perExtent > math.MaxUint16:
		return geometry{}, fmt.Errorf("%w: %d pages to an extent", ErrBadGeometry, g.perExtent)
	case g.maxConnections < 1:
		return geometry{}, fmt.Errorf("%w: %d connections", ErrBadGeometry, g.maxConnections)
	case internalFileHeaderSize+g.maxConnections > g.payloadSize():
		// The engine would chain the connection table across pages; no fixture
		// shows it doing so, so the combination is refused rather than guessed
		// at.
		return geometry{}, fmt.Errorf("%w: a %d-connection table does not fit a %d-byte page",
			ErrBadGeometry, g.maxConnections, g.pageSize)
	}

	return g, nil
}

// orDefault returns fallback for an unset (zero) request, so that the zero
// value of CreateDatabaseOptions means "the DBManager's defaults". A negative
// request is passed through untouched, so it reaches normalize's own range
// checks and is refused rather than quietly turned into a default.
func orDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}

	return v
}

// CreateDatabase writes a new, empty Absolute Database file at path and returns
// it open for writing. The caller closes it.
//
// The file it writes is the one the DBManager's File -> Create Database
// produces for the same settings, byte for byte apart from the three randomly
// seeded page State words no writer can reproduce; see this file's comment for
// the layout and the fixtures behind every field of it.
//
// It refuses rather than guess when the geometry is one the layout does not fit
// (ErrBadGeometry) and when encryption is asked for (ErrEncryptionUnsupported).
// path must not already exist: the file is created exclusively, so
// CreateDatabase can never overwrite a database.
func CreateDatabase(path string, opts CreateDatabaseOptions) (*File, error) {
	g, err := opts.normalize()
	if err != nil {
		return nil, err
	}

	image, err := buildFreshDatabase(g)
	if err != nil {
		return nil, err
	}

	if err := writeNewFile(path, image); err != nil {
		return nil, err
	}

	return OpenForWrite(path)
}

// writeNewFile creates path exclusively and writes image to it, removing the
// file again if anything goes wrong -- a half-written database is not something
// to leave behind, and O_EXCL guarantees the file being removed is the one this
// call created.
func writeNewFile(path string, image []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("absdb: creating %q: %w", path, err)
	}

	err = writeAndSync(f, image)

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		os.Remove(path)

		return fmt.Errorf("absdb: writing %q: %w", path, err)
	}

	return nil
}

func writeAndSync(f *os.File, image []byte) error {
	if _, err := f.WriteAt(image, 0); err != nil {
		return fmt.Errorf("writing %d bytes: %w", len(image), err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("flushing: %w", err)
	}

	return nil
}

// buildFreshDatabase assembles the whole file in memory. It is one buffer
// rather than a sequence of writes because a fresh database is small, entirely
// determined by its geometry, and easiest to check that way: what this function
// returns is exactly what ends up on disk.
func buildFreshDatabase(g geometry) ([]byte, error) {
	image := make([]byte, freshDatabasePageCount*g.pageSize+diskPageHeaderOffset)

	writeFreshHeader(image, g)

	files, err := freshInternalFiles(g)
	if err != nil {
		return nil, err
	}

	pfsState, eamState := allocateFreshPages(image, g)

	stamps := []struct {
		no       int
		pageType uint16
		state    uint32
	}{
		{pfsPageNo, PageTypeFileHdr, pfsState},
		{eamPageNo, PageTypeSystemDir, eamState},
		{systemFileDirPageNo, pageTypeSystemFileDir, seedPageState()},
		{connectionTablePageNo, pageTypeConnectionTable, seedPageState()},
		{freshCatalogPageNo, PageTypeTableList, seedPageState()},
	}

	for _, s := range stamps {
		writeDiskPageHeader(pageBlockHeader(image, g.pageSize, s.no), s.state, s.pageType, -1)
	}

	for no, data := range files {
		copy(pagePayload(image, g.pageSize, no), data)
	}

	return image, nil
}

// freshInternalFiles builds the three system internal files, keyed by the page
// each one lives on. The Page Free Space and Extent Allocation Map pages carry
// bitmaps rather than internal files and are not among them.
func freshInternalFiles(g geometry) (map[int][]byte, error) {
	directory, err := compressInternalFile(
		buildSystemDirectory(connectionTablePageNo, freshCatalogPageNo), 0,
	)
	if err != nil {
		return nil, err
	}

	catalog, err := compressInternalFile(nil, 0)
	if err != nil {
		return nil, err
	}

	files := map[int][]byte{
		systemFileDirPageNo:   directory,
		connectionTablePageNo: buildZeroInternalFile(g.maxConnections),
		freshCatalogPageNo:    catalog,
	}

	for no, data := range files {
		if len(data) > g.payloadSize() {
			return nil, fmt.Errorf("%w: page %d's %d-byte system file does not fit a %d-byte page",
				ErrBadGeometry, no, len(data), g.pageSize)
		}
	}

	return files, nil
}

// writeFreshHeader writes the 380-byte database header. Every field it does not
// set is zero in Empty.abs and is left zero here: the 32 reserved bytes at 44,
// the whole crypto sub-header past its own size field, the 20 bytes at
// trailingHeaderSizeOffset, and LastObjectID.
func writeFreshHeader(image []byte, g geometry) {
	copy(image[:len(Magic)], Magic[:])

	binary.LittleEndian.PutUint16(image[headerSizeOffset:], dbHeaderSize)
	binary.LittleEndian.PutUint64(image[versionOffset:], math.Float64bits(createdVersion))
	binary.LittleEndian.PutUint16(image[pageSizeOffset:], uint16(g.pageSize))       //nolint:gosec // normalize bounds it by MaxUint16
	binary.LittleEndian.PutUint16(image[pagesInExtentOffset:], uint16(g.perExtent)) //nolint:gosec // as above
	binary.LittleEndian.PutUint32(image[totalPageCountOffset:], freshDatabasePageCount)
	binary.LittleEndian.PutUint32(image[lastUsedPageOffset:], freshLastUsedPageNo)
	binary.LittleEndian.PutUint32(image[fileStateOffset:], freshDatabaseState)

	image[writeChangesOffset] = freshWriteChangesState

	binary.LittleEndian.PutUint16(image[cryptoHeaderOffset:], cryptoHeaderSize)
	binary.LittleEndian.PutUint16(image[trailingHeaderSizeOffset:], trailingHeaderSize)
}

// allocateFreshPages marks the five system pages allocated in the two
// allocation maps and returns the State counter each map page ends up with.
//
// The pages are taken one at a time, which is not a detail: it is what makes
// page 1's State 3 rather than 2 at 2048/extent 4. See this file's comment.
func allocateFreshPages(image []byte, g geometry) (pfsState, eamState uint32) {
	pfs := pagePayload(image, g.pageSize, pfsPageNo)
	eam := pagePayload(image, g.pageSize, eamPageNo)

	for no := range freshLastUsedPageNo + 1 {
		pfs[no/8] |= 1 << (no % 8)
		pfsState++

		extent := no / g.perExtent

		state := computeExtentState(pfs, extent, g.perExtent, freshDatabasePageCount)
		if extentState(eam, extent) == state {
			continue
		}

		setExtentState(eam, extent, state)

		eamState++
	}

	return pfsState, eamState
}

// buildSystemDirectory builds page 2's ten-byte payload: two five-byte entries
// naming the connection table and the table catalog, in that order. Both the
// order and the two kind bytes are lifted off Empty.abs, and the same twenty
// bytes appear in every other database examined.
func buildSystemDirectory(connectionsPageNo, catalogPageNo int) []byte {
	out := make([]byte, 2*systemDirEntrySize)

	out[0] = systemDirKindConnections
	binary.LittleEndian.PutUint32(out[1:5], uint32(int32(connectionsPageNo))) //nolint:gosec // page numbers

	out[systemDirEntrySize] = systemDirKindCatalog
	binary.LittleEndian.PutUint32(out[systemDirEntrySize+1:], uint32(int32(catalogPageNo))) //nolint:gosec // page numbers

	return out
}

// buildZeroInternalFile builds an uncompressed internal file of size bytes,
// all of them zero, with DecompressedSize left at 0 rather than mirroring Size.
//
// That asymmetry is measured twice and in two places: page 3 of Empty.abs, the
// connection table, and page 24 of MultiTable-create.abs, the system internal
// file a fresh table gets (buildSystemInternalFile). decompressInternalFile
// does not read the field for algorithm 0, so the engine evidently leaves it
// alone for a file it never compresses -- unlike the catalog, which
// compressInternalFile does mirror because the engine does.
func buildZeroInternalFile(size int) []byte {
	out := make([]byte, internalFileHeaderSize+size)
	out[0] = internalFileHeaderSize
	binary.LittleEndian.PutUint32(out[1:5], uint32(size)) //nolint:gosec // callers bound size by a page payload
	// out[5:9] (DecompressedSize) and out[9] (algorithm 0) stay zero.

	return out
}

// pageBlockHeader returns the diskPageHeaderSize bytes of page no's ABSP header
// within a whole-file image.
func pageBlockHeader(image []byte, pageSize, no int) []byte {
	off := no*pageSize + diskPageHeaderOffset

	return image[off : off+diskPageHeaderSize]
}

// pagePayload returns page no's payload within a whole-file image: the run that
// starts after its own header and reaches into the next block, exactly as
// ReadPage cuts it. See "The payload model" in absdb.go.
func pagePayload(image []byte, pageSize, no int) []byte {
	off := no*pageSize + pageDataOffset

	return image[off : off+pageSize-diskPageHeaderSize]
}
