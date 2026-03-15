// Package absdb reads ComponentAce Absolute Database (.abs) files.
//
// The binary format is reverse-engineered from real .abs files.
// There is no public specification.
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

// Header offsets and sizes.
const (
	headerMagicSize   = 16
	headerModeOffset  = 16
	headerVerOffset   = 18
	headerVerSize     = 8
	headerPageSzOff   = 26
	headerUnk28Off    = 28
	headerTotalColOff = 30
	headerUserColOff  = 34
	headerMinSize     = 38

	// ABSP marker location within page 0.
	page0ABSPOffset = 0x17C
)

var (
	ErrNotABS         = errors.New("absdb: not an Absolute Database file")
	ErrTruncated      = errors.New("absdb: file is truncated")
	ErrPageOutOfRange = errors.New("absdb: page number out of range")
)

// File represents an opened Absolute Database file.
type File struct {
	f         *os.File
	size      int64
	version   float64
	pageSize  uint16
	mode      byte
	unknown28 uint16
	totalCols uint16
	userCols  uint32
}

// Open opens an Absolute Database file for reading.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("absdb: %w", err)
	}

	db := &File{f: f}
	if err := db.parseHeader(); err != nil {
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
	return int(db.size / int64(db.pageSize))
}

// Mode returns the file mode byte (e.g. 'L' for local/single-user).
func (db *File) Mode() byte {
	return db.mode
}

// TotalColumnCount returns the total column count (user + internal).
func (db *File) TotalColumnCount() int {
	return int(db.totalCols)
}

// UserColumnCount returns the user-visible column count.
func (db *File) UserColumnCount() int {
	return int(db.userCols)
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
	page.ABSP = parseABSP(data)
	return page, nil
}

func (db *File) parseHeader() error {
	info, err := db.f.Stat()
	if err != nil {
		return fmt.Errorf("absdb: %w", err)
	}
	db.size = info.Size()

	if db.size < headerMinSize {
		return ErrTruncated
	}

	buf := make([]byte, 512) // read enough for the full header area
	n, err := db.f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return fmt.Errorf("absdb: reading header: %w", err)
	}
	if n < headerMinSize {
		return ErrTruncated
	}
	buf = buf[:n]

	// Validate magic.
	var magic [16]byte
	copy(magic[:], buf[:headerMagicSize])
	if magic != Magic {
		return ErrNotABS
	}

	// Mode flag.
	db.mode = buf[headerModeOffset]

	// Version (float64 LE).
	db.version = math.Float64frombits(binary.LittleEndian.Uint64(buf[headerVerOffset : headerVerOffset+headerVerSize]))

	// Page size (uint16 LE).
	db.pageSize = binary.LittleEndian.Uint16(buf[headerPageSzOff : headerPageSzOff+2])
	if db.pageSize == 0 {
		return fmt.Errorf("absdb: invalid page size 0")
	}

	// Unknown field at offset 28.
	db.unknown28 = binary.LittleEndian.Uint16(buf[headerUnk28Off : headerUnk28Off+2])

	// Total column count (uint16 at offset 30).
	db.totalCols = binary.LittleEndian.Uint16(buf[headerTotalColOff : headerTotalColOff+2])

	// User column count (uint32 at offset 34).
	if len(buf) >= headerUserColOff+4 {
		db.userCols = binary.LittleEndian.Uint32(buf[headerUserColOff : headerUserColOff+4])
	}

	// Validate file size is at least one page.
	if db.size < int64(db.pageSize) {
		return ErrTruncated
	}

	return nil
}

// Page represents a single page read from the database file.
type Page struct {
	Number int
	Data   []byte
	ABSP   *ABSPHeader // nil if no ABSP marker found on this page
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

// ABSPHeader represents the parsed ABSP marker found in page headers.
// Every page contains "ABSP" at offset 0x17C. The 4 bytes after the marker
// appear to be a CRC32 or checksum (on page 0, this value equals the total
// column count, likely by coincidence). The uint16 at ABSP+8 is the page type.
type ABSPHeader struct {
	Offset   int    // byte offset of the ABSP marker within the page
	Checksum uint32 // 4-byte value after marker (CRC32 or checksum)
	PageType uint16 // page type identifier at ABSP+8
}

// parseABSP scans a page for the ABSP marker and parses it.
// Returns nil if no marker is found.
func parseABSP(data []byte) *ABSPHeader {
	// Search for ABSP marker in the page.
	// It's typically at a fixed offset, but we search to be robust.
	for i := 0; i <= len(data)-10; i++ {
		if data[i] == 'A' && data[i+1] == 'B' && data[i+2] == 'S' && data[i+3] == 'P' {
			return &ABSPHeader{
				Offset:   i,
				Checksum: binary.LittleEndian.Uint32(data[i+4 : i+8]),
				PageType: binary.LittleEndian.Uint16(data[i+8 : i+10]),
			}
		}
	}
	return nil
}

// PageClassification describes the role of a page within the database.
type PageClassification int

const (
	PageEmpty      PageClassification = iota // All zeros
	PageFileHeader                           // Page 0: file header
	PageABSP                                 // Has ABSP marker (further classified by PageType)
	PageUnknown                              // Non-empty, no ABSP marker
)

// String returns a human-readable classification name.
func (c PageClassification) String() string {
	switch c {
	case PageEmpty:
		return "empty"
	case PageFileHeader:
		return "file-header"
	case PageABSP:
		return "absp"
	case PageUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("PageClassification(%d)", int(c))
	}
}

// ClassifyPage returns the classification for a page.
func ClassifyPage(p Page) PageClassification {
	if p.Number == 0 {
		return PageFileHeader
	}
	if p.IsEmpty() {
		return PageEmpty
	}
	if p.ABSP != nil {
		return PageABSP
	}
	return PageUnknown
}

// ScanPages reads all pages and returns their classifications.
func (db *File) ScanPages() ([]PageSummary, error) {
	count := db.PageCount()
	summaries := make([]PageSummary, count)

	for i := range count {
		page, err := db.ReadPage(i)
		if err != nil {
			return nil, err
		}
		summaries[i] = PageSummary{
			Number:         i,
			Classification: ClassifyPage(page),
			ABSP:           page.ABSP,
		}
	}
	return summaries, nil
}

// PageSummary is a lightweight summary of a page's classification.
type PageSummary struct {
	Number         int
	Classification PageClassification
	ABSP           *ABSPHeader
}
