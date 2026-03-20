package absdb

import (
	"encoding/binary"
	"errors"
	"math"
	"time"

	"golang.org/x/text/encoding/charmap"
)

var (
	ErrNoData     = errors.New("absdb: no data pages found")
	ErrNoMoreRows = errors.New("absdb: no more rows")
)

// Reader iterates over data records in a table.
type Reader struct {
	db     *File
	schema *TableSchema

	// Computed layout.
	nullFlagBytes   int // bytes of null flags per record
	extraBytes      int // version-dependent per-record metadata bytes
	fieldDataSize   int // total bytes for all field values
	recordSize      int // nullFlagBytes + extraBytes + fieldDataSize
	fieldOffsets    []int
	fieldStoreSizes []int

	// Page layout.
	pageHdrSize int // bytes of page header before first record

	// Data pages.
	dataPages []int // page numbers with type 10

	// Iteration state.
	pageIdx   int // current data page index
	pageData  []byte
	recordIdx int // current record within page
	maxRecs   int // max records per page
	started   bool
	err       error
}

// OpenTable creates a Reader for the table's data records.
func (db *File) OpenTable() (*Reader, error) {
	schema, err := db.Schema()
	if err != nil {
		return nil, err
	}

	// Find all data pages (type 10).
	var dataPages []int

	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil {
			return nil, err
		}

		if page.Header != nil && page.Header.PageType == PageTypeData {
			dataPages = append(dataPages, i)
		}
	}

	if len(dataPages) == 0 {
		return nil, ErrNoData
	}

	r := &Reader{
		db:        db,
		schema:    schema,
		dataPages: dataPages,
	}
	r.computeLayout()

	return r, nil
}

// Schema returns the table schema.
func (r *Reader) Schema() *TableSchema {
	return r.schema
}

// Next advances to the next record. Returns false when no more records.
func (r *Reader) Next() bool {
	if r.err != nil {
		return false
	}

	if !r.started {
		r.started = true

		r.pageIdx = 0

		err := r.loadPage()
		if err != nil {
			r.err = err
			return false
		}
	}

	for {
		if r.recordIdx < r.maxRecs {
			recStart := r.pageHeaderSize() + r.recordIdx*r.recordSize
			if recStart+r.recordSize <= len(r.pageData) && r.isRecordPresent(r.recordIdx) {
				return true
			}

			r.recordIdx++

			continue
		}

		// Move to next data page.
		r.pageIdx++
		if r.pageIdx >= len(r.dataPages) {
			return false
		}

		err := r.loadPage()
		if err != nil {
			r.err = err
			return false
		}
	}
}

// Err returns any error encountered during iteration.
func (r *Reader) Err() error {
	return r.err
}

// Record returns the current record.
func (r *Reader) Record() Record {
	recStart := r.pageHeaderSize() + r.recordIdx*r.recordSize
	fieldStart := recStart + r.nullFlagBytes + r.extraBytes
	r.recordIdx++ // advance for next call to Next()

	return Record{
		reader:    r,
		nullFlags: r.pageData[recStart : recStart+r.nullFlagBytes],
		fieldData: r.pageData[fieldStart : fieldStart+r.fieldDataSize],
	}
}

// Record represents a single data record.
type Record struct {
	reader    *Reader
	nullFlags []byte
	fieldData []byte
}

// IsNull returns true if the column at the given index is null.
func (rec Record) IsNull(col int) bool {
	if col < 0 || col >= len(rec.reader.schema.Columns) {
		return true
	}

	byteIdx := col / 8
	bitIdx := uint(col % 8)

	if byteIdx >= len(rec.nullFlags) {
		return true
	}
	// Bit = 1 means null.
	return rec.nullFlags[byteIdx]&(1<<bitIdx) != 0
}

// Int returns the int32 value of the column.
func (rec Record) Int(col int) int32 {
	off := rec.reader.fieldOffsets[col]
	return int32(binary.LittleEndian.Uint32(rec.fieldData[off : off+4]))
}

// Int64 returns the int64 value of the column.
func (rec Record) Int64(col int) int64 {
	off := rec.reader.fieldOffsets[col]
	return int64(binary.LittleEndian.Uint64(rec.fieldData[off : off+8]))
}

// Uint32 returns the uint32 value of the column.
func (rec Record) Uint32(col int) uint32 {
	off := rec.reader.fieldOffsets[col]
	return binary.LittleEndian.Uint32(rec.fieldData[off : off+4])
}

// Float returns the float64 value of the column.
func (rec Record) Float(col int) float64 {
	off := rec.reader.fieldOffsets[col]
	return math.Float64frombits(binary.LittleEndian.Uint64(rec.fieldData[off : off+8]))
}

// String returns the string value of the column, decoded from Windows-1252.
func (rec Record) String(col int) string {
	off := rec.reader.fieldOffsets[col]
	sz := rec.reader.fieldStoreSizes[col]
	raw := rec.fieldData[off : off+sz]

	// Find null terminator.
	end := 0
	for end < len(raw) && raw[end] != 0 {
		end++
	}

	if end == 0 {
		return ""
	}

	// Decode Windows-1252 to UTF-8.
	decoded, err := charmap.Windows1252.NewDecoder().Bytes(raw[:end])
	if err != nil {
		return string(raw[:end]) // fallback
	}

	return string(decoded)
}

// Bool returns the boolean value of the column.
func (rec Record) Bool(col int) bool {
	off := rec.reader.fieldOffsets[col]
	// WordBool: 2 bytes, non-zero = true.
	return binary.LittleEndian.Uint16(rec.fieldData[off:off+2]) != 0
}

// Time returns the time.Time value of a Date, Time, or DateTime column.
func (rec Record) Time(col int) time.Time {
	c := rec.reader.schema.Columns[col]
	off := rec.reader.fieldOffsets[col]

	switch c.BaseType {
	case BftDate:
		// Date stored as int32 (days since epoch).
		days := int32(binary.LittleEndian.Uint32(rec.fieldData[off : off+4]))
		return delphiDateToTime(days)
	case BftTime:
		// Time stored as int32 (milliseconds since midnight).
		ms := int32(binary.LittleEndian.Uint32(rec.fieldData[off : off+4]))
		return delphiTimeToTime(ms)
	case BftDateTime:
		// DateTime stored as Date(int32) + Time(int32).
		days := int32(binary.LittleEndian.Uint32(rec.fieldData[off : off+4]))
		ms := int32(binary.LittleEndian.Uint32(rec.fieldData[off+4 : off+8]))
		t := delphiDateToTime(days)

		return t.Add(time.Duration(ms) * time.Millisecond)
	default:
		return time.Time{}
	}
}

// Bytes returns the raw bytes for the column.
func (rec Record) Bytes(col int) []byte {
	off := rec.reader.fieldOffsets[col]
	sz := rec.reader.fieldStoreSizes[col]
	result := make([]byte, sz)
	copy(result, rec.fieldData[off:off+sz])

	return result
}

// --- internal helpers ---

func (r *Reader) computeLayout() {
	cols := r.schema.Columns

	// Compute field storage sizes and offsets.
	r.fieldOffsets = make([]int, len(cols))
	r.fieldStoreSizes = make([]int, len(cols))
	offset := 0

	for i, c := range cols {
		sz := r.fieldStoreSize(c)
		r.fieldOffsets[i] = offset
		r.fieldStoreSizes[i] = sz
		offset += sz
	}

	r.fieldDataSize = offset

	// Detect the actual record size and null flag bytes by scanning the first data page.
	// The record size is determined by the spacing between consecutive records.
	r.detectRecordLayout()
}

func (r *Reader) fieldStoreSize(c Column) int {
	switch c.BaseType {
	case BftInt8, BftUint8:
		return 1
	case BftInt16, BftUint16:
		return 2
	case BftInt32, BftUint32:
		return 4
	case BftInt64:
		return 8
	case BftSingle:
		return 4
	case BftDouble:
		return 8
	case BftExtended:
		return 10
	case BftCurrency:
		return 8
	case BftLogical:
		return 2 // WordBool
	case BftDate, BftTime:
		return 4
	case BftDateTime:
		return 8 // Date(4) + Time(4)
	case BftVarchar, BftChar:
		return int(c.Size) + 1
	case BftWideVarchar, BftWideChar:
		return (int(c.Size) + 1) * 2
	case BftBlob, BftClob, BftWideClob:
		return 6 // BLOB reference: PageNo(4) + ItemNo(2)
	case BftBytes:
		return int(c.Size)
	case BftVarBytes:
		return int(c.Size) + 2
	default:
		return int(c.Size)
	}
}

func (r *Reader) pageHeaderSize() int {
	return r.pageHdrSize
}

// detectRecordLayout scans the first data page to determine the record stride,
// null flag byte count, and extra per-record metadata bytes.
//
// Strategy: the first column is typically AutoInc or RecNo starting at 1.
// We scan for 01 00 00 00 in the page data, then validate by checking the
// second record at the computed stride also has a small positive value.
// The page header size varies because it contains a record occupancy bitmap
// whose size depends on max records per page.
func (r *Reader) detectRecordLayout() {
	if len(r.dataPages) == 0 {
		return
	}

	page, err := r.db.ReadPage(r.dataPages[0])
	if err != nil {
		return
	}

	d := page.PageData()
	if len(d) < 20 {
		return
	}

	expectedNullBytes := (len(r.schema.Columns) + 2 + 7) / 8
	bestScore := math.MinInt
	bestPageHdr := 0
	bestExtra := 0

	for pageHdr := 1; pageHdr <= 64 && pageHdr < len(d); pageHdr++ {
		if r.countPresentSlots(d, pageHdr) == 0 {
			continue
		}

		for extra := 0; extra <= 8; extra++ {
			recSize := expectedNullBytes + extra + r.fieldDataSize
			if recSize <= 0 || pageHdr+recSize > len(d) {
				continue
			}

			score := r.scoreLayoutCandidate(d, pageHdr, expectedNullBytes, extra, recSize)
			if score > bestScore {
				bestScore = score
				bestPageHdr = pageHdr
				bestExtra = extra
			}
		}
	}

	if bestPageHdr != 0 {
		r.nullFlagBytes = expectedNullBytes
		r.extraBytes = bestExtra
		r.recordSize = r.nullFlagBytes + r.extraBytes + r.fieldDataSize
		r.pageHdrSize = bestPageHdr
		return
	}

	r.nullFlagBytes = expectedNullBytes
	r.extraBytes = 0
	r.recordSize = r.nullFlagBytes + r.extraBytes + r.fieldDataSize
	r.pageHdrSize = 1
}

func (r *Reader) loadPage() error {
	if r.pageIdx >= len(r.dataPages) {
		return ErrNoMoreRows
	}

	page, err := r.db.ReadPage(r.dataPages[r.pageIdx])
	if err != nil {
		return err
	}

	r.pageData = page.PageData()
	r.recordIdx = 0

	// Calculate max records that fit in the page data area.
	usable := len(r.pageData) - r.pageHeaderSize()
	if usable > 0 && r.recordSize > 0 && r.pageHdrSize > 0 {
		r.maxRecs = r.pageHdrSize * 8
	} else {
		r.maxRecs = 0
	}

	return nil
}

func (r *Reader) isRecordPresent(slot int) bool {
	byteIdx := slot / 8
	if byteIdx < 0 || byteIdx >= r.pageHdrSize || byteIdx >= len(r.pageData) {
		return false
	}

	bitIdx := uint(slot % 8)
	return r.pageData[byteIdx]&(1<<bitIdx) != 0
}

func (r *Reader) countPresentSlots(pageData []byte, pageHdr int) int {
	if pageHdr <= 0 || pageHdr > len(pageData) {
		return 0
	}

	count := 0
	for slot := 0; slot < pageHdr*8; slot++ {
		if pageData[slot/8]&(1<<uint(slot%8)) != 0 {
			count++
		}
	}

	return count
}

func (r *Reader) scoreLayoutCandidate(pageData []byte, pageHdr, nullBytes, extra, recSize int) int {
	score := 0
	firstValues := make([]uint32, 0, 4)
	recordsScored := 0

	for slot := 0; slot < pageHdr*8 && recordsScored < 4; slot++ {
		if pageData[slot/8]&(1<<uint(slot%8)) == 0 {
			continue
		}

		recStart := pageHdr + slot*recSize
		if recStart+recSize > len(pageData) {
			return math.MinInt / 2
		}

		fieldStart := recStart + nullBytes + extra
		if fieldStart+r.fieldDataSize > len(pageData) {
			return math.MinInt / 2
		}

		recScore, firstValue := r.scoreRecordData(pageData[fieldStart : fieldStart+r.fieldDataSize])
		score += recScore
		firstValues = append(firstValues, firstValue)
		recordsScored++
	}

	if recordsScored == 0 {
		return math.MinInt / 2
	}

	score += recordsScored * 8
	for i := 1; i < len(firstValues); i++ {
		switch {
		case firstValues[i] == firstValues[i-1]+1:
			score += 10
		case firstValues[i] > firstValues[i-1]:
			score += 3
		default:
			score -= 8
		}
	}

	return score
}

func (r *Reader) scoreRecordData(fieldData []byte) (int, uint32) {
	score := 0
	firstValue := uint32(0)
	haveFirstValue := false

	for i, c := range r.schema.Columns {
		off := r.fieldOffsets[i]
		sz := r.fieldStoreSizes[i]
		raw := fieldData[off : off+sz]

		switch c.BaseType {
		case BftInt32, BftUint32:
			v := binary.LittleEndian.Uint32(raw)
			if !haveFirstValue {
				firstValue = v
				haveFirstValue = true
			}
			if v <= 1_000_000_000 {
				score += 3
			} else {
				score -= 8
			}
		case BftLogical:
			v := binary.LittleEndian.Uint16(raw)
			if v == 0 || v == 1 {
				score += 3
			} else {
				score -= 8
			}
		case BftVarchar, BftChar:
			end := 0
			for end < len(raw) && raw[end] != 0 {
				end++
			}

			if end == 0 {
				score--
				continue
			}

			printable := 0
			for _, b := range raw[:end] {
				if b >= 32 || b >= 0x80 {
					printable++
				}
			}
			if printable*100 >= end*85 {
				score += 6
			} else {
				score -= 8
			}
		case BftDouble:
			bits := binary.LittleEndian.Uint64(raw)
			v := math.Float64frombits(bits)
			switch {
			case bits == 0:
				score += 1
			case math.IsNaN(v) || math.IsInf(v, 0):
				score -= 10
			default:
				abs := math.Abs(v)
				if abs < 1e-100 || abs > 1e100 {
					score -= 3
				} else {
					score += 2
				}
			}
		case BftBlob, BftClob, BftWideClob:
			ref := readBlobRef(raw)
			switch {
			case ref.IsNull():
				score++
			case int(ref.PageNo) < 0 || int(ref.PageNo) >= r.db.PageCount():
				score -= 12
			default:
				page, err := r.db.ReadPage(int(ref.PageNo))
				if err != nil || page.Header == nil || page.Header.PageType != PageTypeBlob {
					score -= 12
				} else if ref.ItemNo <= 8 {
					score += 8
				} else {
					score += 4
				}
			}
		}
	}

	if !haveFirstValue {
		return math.MinInt / 2, 0
	}

	return score, firstValue
}

// delphiDateToTime converts a Delphi date integer to time.Time.
// Delphi dates: 1 = 0001-01-01. Go uses a different epoch.
func delphiDateToTime(days int32) time.Time {
	// Delphi epoch: day 1 = January 1, 0001.
	epoch := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	return epoch.AddDate(0, 0, int(days)-1)
}

// delphiTimeToTime converts Delphi time milliseconds to a time.Time with just the time component.
func delphiTimeToTime(ms int32) time.Time {
	return time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
}
