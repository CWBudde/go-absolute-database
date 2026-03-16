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

// Page type constants from the TABSDiskPageHeader.PageType field.
const (
	PageTypeSystemDir = 2  // System directory
	PageTypeFileHdr   = 3  // File header (page 0)
	PageTypeSchema    = 8  // Schema metadata (zlib-compressed column defs)
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
}

// Open opens an Absolute Database file for reading.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("absdb: %w", err)
	}

	db := &File{f: f}

	err = db.parseHeader()
	if err != nil {
		f.Close()
		return nil, err
	}

	return db, nil
}

// Close closes the database file.
func (db *File) Close() error {
	return db.f.Close()
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
func (db *File) ReadPage(n int) (Page, error) {
	if n < 0 || n >= db.PageCount() {
		return Page{}, ErrPageOutOfRange
	}

	data := make([]byte, db.pageSize)
	offset := int64(n) * int64(db.pageSize)

	_, err := db.f.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return Page{}, fmt.Errorf("absdb: reading page %d: %w", n, err)
	}

	page := Page{
		Number: n,
		Data:   data,
	}
	page.Header = parseDiskPageHeader(data)

	return page, nil
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

	db.pagesInExtent = binary.LittleEndian.Uint16(buf[28:30])
	db.totalPageCount = int32(binary.LittleEndian.Uint32(buf[30:34]))
	db.lastUsedPageNo = int32(binary.LittleEndian.Uint32(buf[34:38]))
	db.state = int32(binary.LittleEndian.Uint32(buf[38:42]))
	db.writeChangeState = buf[42]
	db.encrypted = buf[43] != 0

	if db.size < int64(db.pageSize) {
		return ErrTruncated
	}

	return nil
}

// Page represents a single page read from the database file.
type Page struct {
	Number int
	Data   []byte
	Header *DiskPageHeader // nil if no ABSP marker found
}

// IsEmpty returns true if the page contains only zero bytes.
func (p Page) IsEmpty() bool {
	for _, b := range p.Data {
		if b != 0 {
			return false
		}
	}

	return true
}

// PageData returns the usable data portion of the page (after the disk page header).
func (p Page) PageData() []byte {
	if len(p.Data) <= pageDataOffset {
		return nil
	}

	return p.Data[pageDataOffset:]
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
