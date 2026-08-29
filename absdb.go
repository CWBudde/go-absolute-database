// Package absdb reads ComponentAce Absolute Database (.abs) files.
//
// The binary format is reverse-engineered from real .abs files and
// the C++ header files shipped with the Absolute Database SDK.
package absdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Magic is the 16-byte file signature: "ABS0LUTEDATABASE" (note: zero, not letter O).
var Magic = [16]byte{
	'A', 'B', 'S', '0', 'L', 'U', 'T', 'E',
	'D', 'A', 'T', 'A', 'B', 'A', 'S', 'E',
}

const (
	// diskPageHeaderOffset is the fixed byte offset of the TABSDiskPageHeader
	// ("ABSP" marker) within every page.
	diskPageHeaderOffset = 0x17C

	// diskPageHeaderSize is the packed size of TABSDiskPageHeader (40 bytes).
	diskPageHeaderSize = 40

	// pageDataOffset is where usable page data begins (after the disk page header).
	pageDataOffset = diskPageHeaderOffset + diskPageHeaderSize // 0x1A4

	// dbHeaderSize is the packed size of TABSDBHeader (76 bytes).
	dbHeaderSize = 76
)

// The payload model.
//
// Each pageSize-byte block on disk carries the 40-byte TABSDiskPageHeader at
// block offset diskPageHeaderOffset. The payload of page N is the contiguous
// run of pageSize-diskPageHeaderSize bytes starting right after page N's own
// header and ending right before page N+1's header:
//
//	file[N*pageSize+pageDataOffset : (N+1)*pageSize+diskPageHeaderOffset]
//
// For the usual 4096-byte page that is 4056 bytes: 3676 bytes from block N plus
// the first 380 bytes of block N+1. This is why every .abs file is exactly
// pageCount*pageSize+diskPageHeaderOffset bytes long — the trailing
// diskPageHeaderOffset bytes complete the last page's payload.

// pageStateFree is the State an ABSP header carries once the engine has
// released its page. DROP TABLE tombstones the dropped table's whole run of
// pages this way rather than erasing them: they keep their type, their
// ObjectID and their contents, and only this field says they are gone.
// Measured on testdata/MultiTable-drop.abs, where all six of the dropped
// table's pages carry it and every surviving page carries a plain counter.
const pageStateFree = math.MaxInt32

// Page type constants from the TABSDiskPageHeader.PageType field.
const (
	PageTypeSystemDir = 2  // System directory
	PageTypeFileHdr   = 3  // File header (page 0)
	PageTypeTableList = 6  // Table catalog (uncompressed internal file)
	PageTypeSystem    = 7  // A table's "system" internal file; role unidentified, see TableInfo.systemPageNo
	PageTypeSchema    = 8  // Schema metadata (zlib-compressed column defs)
	PageTypeTableInfo = 9  // Table info (record counts)
	PageTypeData      = 10 // Data page (row storage)
	PageTypeIndex     = 12 // B-tree index page
)

var (
	ErrNotABS         = errors.New("absdb: not an Absolute Database file")
	ErrTruncated      = errors.New("absdb: file is truncated")
	ErrPageOutOfRange = errors.New("absdb: page number out of range")
)

// File represents an opened Absolute Database file.
type File struct {
	f    *os.File
	size int64

	// writable is true when the file was opened with OpenForWrite. Every write
	// path checks it, so a read-only handle can never reach a WriteAt.
	writable bool

	// Parsed from TABSDBHeader.
	headerSize       int16
	version          float64
	pageSize         uint16
	pagesInExtent    uint16
	totalPageCount   int32
	lastUsedPageNo   int32
	state            int32
	writeChangeState byte
	encrypted        bool

	// lastObjectID is TABSDBHeader.LastObjectID, at header offset 376: the last
	// object id the engine has handed out. It is not part of the documented
	// 76-byte header (dbHeaderSize) -- it sits in the header's otherwise
	// unaccounted tail, 4 bytes ahead of page 0's own ABSP marker at
	// diskPageHeaderOffset. See PLAN.md Phase 8 for how CREATE TABLE moves it.
	lastObjectID int32

	// Parsed from TABSCryptoHeader (nil if not encrypted).
	cryptoHeader *CryptoHeader

	// Derived from password; nil if not encrypted or no password provided.
	decryptionKey []byte

	// randPageState supplies a newly allocated page's initial ABSP State. The
	// engine seeds it with a random, unreproducible 30-bit value (see the
	// CREATE TABLE analysis in ddl_create.go); this field exists so a test can
	// pin the value and assert every other byte of an allocation. nil selects
	// the default source, newPageState's math/rand/v2 generator.
	randPageState func() uint32
}

// Open opens an Absolute Database file for reading.
func Open(path string) (*File, error) {
	return openDB(path, os.O_RDONLY)
}

// openDB opens the file with the given access mode and parses its header. It
// backs both Open and OpenForWrite so that the two cannot drift apart.
func openDB(path string, flag int) (*File, error) {
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, fmt.Errorf("absdb: %w", err)
	}

	db := &File{f: f, writable: flag&os.O_RDWR != 0}

	err = db.parseHeader()
	if err != nil {
		f.Close()
		return nil, err
	}

	return db, nil
}

// OpenWithPassword opens an encrypted Absolute Database file.
// If the file is not encrypted, the password is ignored.
//
// It returns ErrWrongPassword when the password does not match and
// ErrUnsupportedCipher when the file uses a cipher this package cannot decrypt;
// the two are distinct because a caller can do nothing about the latter.
func OpenWithPassword(path, password string) (*File, error) {
	db, err := Open(path)
	if err != nil {
		return nil, err
	}

	err = db.Unlock(password)
	if err != nil {
		db.Close()

		return nil, err
	}

	return db, nil
}

// Close closes the database file.
func (db *File) Close() error {
	if err := db.f.Close(); err != nil {
		return fmt.Errorf("absdb: closing file: %w", err)
	}

	return nil
}

// Version returns the database engine version (e.g. 5.13, 7.10, 7.61).
func (db *File) Version() float64 {
	return db.version
}

// PageSize returns the page size in bytes.
func (db *File) PageSize() int {
	return int(db.pageSize)
}

// PageCount returns the total number of pages in the file.
func (db *File) PageCount() int {
	return int(db.totalPageCount)
}

// Encrypted returns true if the database is encrypted.
func (db *File) Encrypted() bool {
	return db.encrypted
}

// ReadPage reads a single page by its zero-based page number.
//
// A page spans pageSize+diskPageHeaderOffset bytes on disk: the whole block the
// page starts in plus the leading diskPageHeaderOffset bytes of the next block,
// which still belong to this page's payload. Both are fetched with one ReadAt.
func (db *File) ReadPage(n int) (Page, error) {
	if n < 0 || n >= db.PageCount() {
		return Page{}, ErrPageOutOfRange
	}

	buf := make([]byte, db.pageReadSize())
	offset := int64(n) * int64(db.pageSize)

	read, err := db.f.ReadAt(buf, offset)
	if read < len(buf) {
		if err == nil || errors.Is(err, io.EOF) {
			return Page{}, fmt.Errorf("absdb: reading page %d: %w", n, ErrTruncated)
		}

		return Page{}, fmt.Errorf("absdb: reading page %d: %w", n, err)
	}

	// Data and Payload share the same backing array; they overlap in
	// [pageDataOffset, pageSize). Decrypting the payload in place is therefore
	// visible through Data as well, which is intended.
	page := Page{
		Number:  n,
		Data:    buf[:db.pageSize],
		Payload: buf[pageDataOffset : pageDataOffset+db.payloadSize()],
		raw:     buf,
	}

	// The ABSP header stays in the clear even on encrypted pages, so it is
	// parsed from the raw block before the payload is decrypted.
	page.Header = parseDiskPageHeader(page.Data)

	// Page 0 is never encrypted; a zero CRC32 marks an unencrypted page.
	if n > 0 && db.decryptionKey != nil && page.Header != nil && page.Header.CRC32 != 0 {
		err = db.decryptPayload(page.Payload)
		if err != nil {
			return Page{}, fmt.Errorf("absdb: decrypting page %d: %w", n, err)
		}
	}

	return page, nil
}

// Page represents a single page read from the database file.
//
// Data is the raw pageSize-byte block the page starts in, kept so that the ABSP
// header at diskPageHeaderOffset can be located at its documented offset.
// Payload is the page's usable data area: pageSize-diskPageHeaderSize bytes
// running from pageDataOffset in this block into the first
// diskPageHeaderOffset bytes of the next block. Data and Payload overlap and
// share one backing array.
type Page struct {
	Number  int
	Data    []byte
	Payload []byte
	Header  *DiskPageHeader // nil if no ABSP marker found

	// raw is the whole buffer Data and Payload are cut from: the page's own
	// block followed by the leading diskPageHeaderOffset bytes of the next one.
	// The write path needs it, because writing a page back means writing the
	// ABSP header and the payload, which are contiguous only in this buffer.
	raw []byte
}

// IsEmpty returns true if the page's block (Data, the raw pageSize bytes this
// page starts in, header included) contains only zero bytes. It deliberately
// covers the block rather than the payload: an all-zero block carries no ABSP
// header and is therefore never encrypted.
func (p Page) IsEmpty() bool {
	for _, b := range p.Data {
		if b != 0 {
			return false
		}
	}

	return true
}

// PageData returns the usable data portion of the page: everything after this
// page's disk page header, continuing into the next block up to that block's
// header. For a 4096-byte page this is 4056 bytes.
func (p Page) PageData() []byte {
	return p.Payload
}

// DiskPageHeader is the 40-byte TABSDiskPageHeader found at offset 0x17C in every page.
type DiskPageHeader struct {
	State      int32  // page state
	PageType   uint16 // page type (see PageType* constants)
	NextPageNo int32  // next page in chain (-1 = none)
	CRC32      uint32 // CRC32 checksum
	CRCType    byte
	HashType   byte
	CipherType byte
	MACType    byte
	ObjectID   int32  // table/object this page belongs to (-1 = system)
	RecPageNo  int32  // record ID: page number
	RecItemNo  uint16 // record ID: item number within page
}

// parseDiskPageHeader reads the TABSDiskPageHeader from the fixed offset.
func parseDiskPageHeader(data []byte) *DiskPageHeader {
	if len(data) < diskPageHeaderOffset+diskPageHeaderSize {
		return nil
	}

	off := diskPageHeaderOffset
	if data[off] != 'A' || data[off+1] != 'B' || data[off+2] != 'S' || data[off+3] != 'P' {
		return nil
	}

	return &DiskPageHeader{
		State:      int32(binary.LittleEndian.Uint32(data[off+4 : off+8])),
		PageType:   binary.LittleEndian.Uint16(data[off+8 : off+10]),
		NextPageNo: int32(binary.LittleEndian.Uint32(data[off+10 : off+14])),
		CRC32:      binary.LittleEndian.Uint32(data[off+14 : off+18]),
		CRCType:    data[off+18],
		HashType:   data[off+19],
		CipherType: data[off+20],
		MACType:    data[off+21],
		ObjectID:   int32(binary.LittleEndian.Uint32(data[off+22 : off+26])),
		RecPageNo:  int32(binary.LittleEndian.Uint32(data[off+26 : off+30])),
		RecItemNo:  binary.LittleEndian.Uint16(data[off+30 : off+32]),
	}
}

// Freed reports whether the engine has released this page. A freed page keeps
// its type, its owner and its old contents, so nothing but this distinguishes
// a dropped table's data page from a live one.
func (p Page) Freed() bool {
	return p.Header != nil && p.Header.State == pageStateFree
}

// ScanPages reads all pages and returns their disk page headers.
func (db *File) ScanPages() ([]PageSummary, error) {
	count := db.PageCount()
	summaries := make([]PageSummary, count)

	for i := range count {
		page, err := db.ReadPage(i)
		if err != nil {
			return nil, err
		}

		summaries[i] = PageSummary{
			Number: i,
			Empty:  page.IsEmpty(),
			Header: page.Header,
		}
	}

	return summaries, nil
}

// PageSummary is a lightweight summary of a scanned page.
type PageSummary struct {
	Number int
	Empty  bool
	Header *DiskPageHeader
}

func (db *File) parseHeader() error {
	info, err := db.f.Stat()
	if err != nil {
		return fmt.Errorf("absdb: %w", err)
	}

	db.size = info.Size()

	if db.size < dbHeaderSize {
		return ErrTruncated
	}

	buf := make([]byte, dbHeaderSize)

	_, err = db.f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("absdb: reading header: %w", err)
	}

	// Validate magic.
	var magic [16]byte
	copy(magic[:], buf[:16])

	if magic != Magic {
		return ErrNotABS
	}

	// TABSDBHeader layout (packed):
	//   Signature[16]        offset 0
	//   HeaderSize  int16    offset 16
	//   Version     float64  offset 18
	//   PageSize    uint16   offset 26
	//   PageCountInExtent uint16 offset 28
	//   TotalPageCount int32 offset 30
	//   LastUsedPageNo int32 offset 34
	//   State       int32    offset 38
	//   WriteChangesState byte offset 42
	//   Encrypted   bytebool offset 43
	//   Reserved[32]         offset 44

	db.headerSize = int16(binary.LittleEndian.Uint16(buf[16:18]))
	db.version = math.Float64frombits(binary.LittleEndian.Uint64(buf[18:26]))

	db.pageSize = binary.LittleEndian.Uint16(buf[26:28])
	if db.pageSize == 0 {
		return errors.New("absdb: invalid page size 0")
	}

	// A page must at least be able to hold its own disk page header, otherwise
	// the payload model is not well defined. Real files use 4096, so this only
	// rejects nonsense. pageSize is a uint16 and therefore bounded above.
	if int(db.pageSize) < pageDataOffset {
		return fmt.Errorf("absdb: invalid page size %d", db.pageSize)
	}

	db.pagesInExtent = binary.LittleEndian.Uint16(buf[28:30])
	db.totalPageCount = int32(binary.LittleEndian.Uint32(buf[30:34]))
	db.lastUsedPageNo = int32(binary.LittleEndian.Uint32(buf[34:38]))
	db.state = int32(binary.LittleEndian.Uint32(buf[38:42]))
	db.writeChangeState = buf[42]
	db.encrypted = buf[43] != 0

	if db.size < int64(db.pageSize) {
		return ErrTruncated
	}

	if db.totalPageCount < 0 {
		return fmt.Errorf("absdb: invalid total page count %d", db.totalPageCount)
	}

	// The file must be large enough to hold every announced page, including the
	// trailing diskPageHeaderOffset bytes that complete the last page's payload.
	// A longer file is tolerated; a shorter one cannot be trusted and would
	// otherwise make ScanPages allocate for pages that do not exist.
	if int64(db.totalPageCount)*int64(db.pageSize)+diskPageHeaderOffset > db.size {
		return ErrTruncated
	}

	db.readLastObjectID()

	// Parse CryptoHeader if encrypted.
	if db.encrypted {
		page0 := make([]byte, db.pageSize)
		if _, err := db.f.ReadAt(page0, 0); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("absdb: reading page 0 for crypto header: %w", err)
		}

		db.cryptoHeader = parseCryptoHeader(page0)
	}

	return nil
}

// readLastObjectID reads LastObjectID, which lives outside the 76-byte buffer
// parseHeader already read, in the header's otherwise-reserved tail (see the
// File.lastObjectID doc comment). It is read best-effort: a file too short to
// carry it is still readable for everything else, and this field only
// matters to writers.
func (db *File) readLastObjectID() {
	if db.size < lastObjectIDOffset+4 {
		return
	}

	var objBuf [4]byte

	if _, err := db.f.ReadAt(objBuf[:], lastObjectIDOffset); err == nil {
		db.lastObjectID = int32(binary.LittleEndian.Uint32(objBuf[:]))
	}
}

// payloadSize returns the number of usable payload bytes carried by a single
// page: pageSize minus the disk page header. See "The payload model" above.
func (db *File) payloadSize() int {
	return int(db.pageSize) - diskPageHeaderSize
}

// pageReadSize returns the number of bytes that must be read from disk to cover
// one full page: the block itself plus the leading part of the following block
// that still belongs to this page's payload.
func (db *File) pageReadSize() int {
	return int(db.pageSize) + diskPageHeaderOffset
}

// findPageByType returns the first page with the given type, or -1.
func (db *File) findPageByType(pageType uint16) (int, error) {
	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil {
			return -1, err
		}

		if page.Header != nil && page.Header.PageType == pageType {
			return i, nil
		}
	}

	return -1, nil
}
