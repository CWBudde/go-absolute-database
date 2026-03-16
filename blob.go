package absdb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// PageTypeBlob is the page type for BLOB data storage.
	PageTypeBlob = 11

	// blobPageHeaderSize is the size of the BLOB page header (3 × int64).
	blobPageHeaderSize = 24
)

var (
	ErrBlobNotFound = fmt.Errorf("absdb: BLOB data not found")
	ErrBlobTrunc    = fmt.Errorf("absdb: BLOB data truncated")
)

// blobPageHeader is the on-disk header at the start of a BLOB page's data area.
// Fields are int64 (8-byte aligned), despite the C++ headers declaring a packed struct.
type blobPageHeader struct {
	ItemCount        int64 // number of items (usually 1)
	CompressedSize   int64 // compressed data size in bytes
	UncompressedSize int64 // uncompressed data size in bytes
}

// readBlobPageHeader reads the BLOB page header from page data.
func readBlobPageHeader(data []byte) (blobPageHeader, error) {
	if len(data) < blobPageHeaderSize {
		return blobPageHeader{}, ErrBlobTrunc
	}

	return blobPageHeader{
		ItemCount:        int64(binary.LittleEndian.Uint64(data[0:8])),
		CompressedSize:   int64(binary.LittleEndian.Uint64(data[8:16])),
		UncompressedSize: int64(binary.LittleEndian.Uint64(data[16:24])),
	}, nil
}

// BlobRef is a reference to BLOB data stored in a record field.
type BlobRef struct {
	PageNo int32  // page number where BLOB data starts
	ItemNo uint16 // item number within the page (usually 0)
}

// readBlobRef extracts a BLOB reference from 6 raw bytes.
func readBlobRef(data []byte) BlobRef {
	return BlobRef{
		PageNo: int32(binary.LittleEndian.Uint32(data[0:4])),
		ItemNo: binary.LittleEndian.Uint16(data[4:6]),
	}
}

// IsNull returns true if the BLOB reference points to no data.
func (ref BlobRef) IsNull() bool {
	return ref.PageNo == 0 && ref.ItemNo == 0
}

// BlobRef returns the BLOB reference for a BLOB/Memo/Graphic column.
func (rec Record) BlobRef(col int) BlobRef {
	off := rec.reader.fieldOffsets[col]

	return readBlobRef(rec.fieldData[off : off+6])
}

// ReadBlob reads the BLOB data for the given reference from the database.
// Returns the raw (decompressed) bytes. For Memo fields, convert to string.
func (db *File) ReadBlob(ref BlobRef) ([]byte, error) {
	if ref.IsNull() {
		return nil, nil
	}

	pageNo := int(ref.PageNo)
	if pageNo < 0 || pageNo >= db.PageCount() {
		return nil, fmt.Errorf("%w: page %d out of range", ErrBlobNotFound, pageNo)
	}

	page, err := db.ReadPage(pageNo)
	if err != nil {
		return nil, err
	}

	if page.Header == nil || page.Header.PageType != PageTypeBlob {
		return nil, fmt.Errorf("%w: page %d is type %d, not BLOB",
			ErrBlobNotFound, pageNo, page.Header.PageType)
	}

	data := page.PageData()

	hdr, err := readBlobPageHeader(data)
	if err != nil {
		return nil, err
	}

	if hdr.ItemCount < 1 {
		return nil, fmt.Errorf("%w: page %d has no items", ErrBlobNotFound, pageNo)
	}

	compressedData := data[blobPageHeaderSize:]

	// Handle multi-page BLOBs: if compressed size exceeds the single page,
	// follow the page chain (NextPageNo) if available.
	if int64(len(compressedData)) < hdr.CompressedSize {
		if page.Header != nil && page.Header.NextPageNo >= 0 {
			compressedData, err = db.readBlobChain(page, hdr.CompressedSize)
			if err != nil {
				return nil, err
			}
		} else {
			// Single-page BLOB where the header size exceeds the available
			// page data. This happens when the size field includes padding.
			// Return what we have — the trailing bytes are typically zeros.
			// Leave compressedData as-is (full remaining page data).
		}
	} else {
		compressedData = compressedData[:hdr.CompressedSize]
	}

	// If sizes match, data is uncompressed.
	if hdr.CompressedSize == hdr.UncompressedSize {
		result := make([]byte, len(compressedData))
		copy(result, compressedData)

		return result, nil
	}

	// Decompress with zlib.
	return decompressBlob(compressedData, hdr.UncompressedSize)
}

// readBlobChain reads BLOB data that spans multiple pages via NextPageNo chaining.
func (db *File) readBlobChain(firstPage Page, totalSize int64) ([]byte, error) {
	result := make([]byte, 0, totalSize)

	// First page: data starts after header.
	firstData := firstPage.PageData()[blobPageHeaderSize:]
	result = append(result, firstData...)

	// Follow chain.
	nextPageNo := int(firstPage.Header.NextPageNo)

	for int64(len(result)) < totalSize && nextPageNo >= 0 {
		page, err := db.ReadPage(nextPageNo)
		if err != nil {
			return nil, fmt.Errorf("absdb: reading BLOB chain page %d: %w", nextPageNo, err)
		}

		pageData := page.PageData()
		result = append(result, pageData...)
		nextPageNo = int(page.Header.NextPageNo)
	}

	if int64(len(result)) < totalSize {
		return nil, fmt.Errorf("%w: expected %d bytes, got %d", ErrBlobTrunc, totalSize, len(result))
	}

	return result[:totalSize], nil
}

// decompressBlob decompresses zlib-compressed BLOB data.
func decompressBlob(compressed []byte, _ int64) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("absdb: BLOB decompression: %w", err)
	}
	defer r.Close()

	result, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("absdb: BLOB decompression: %w", err)
	}

	return result, nil
}

// Blob reads the BLOB data for the given column from the database.
// Returns the raw bytes. Returns nil for NULL BLOBs.
func (rec Record) Blob(col int) ([]byte, error) {
	if rec.IsNull(col) {
		return nil, nil
	}

	ref := rec.BlobRef(col)
	if ref.IsNull() {
		return nil, nil
	}

	return rec.reader.db.ReadBlob(ref)
}

// Memo reads a Memo (text BLOB) column and returns it as a string.
// Returns empty string for NULL values.
func (rec Record) Memo(col int) (string, error) {
	data, err := rec.Blob(col)
	if err != nil {
		return "", err
	}

	if data == nil {
		return "", nil
	}

	return string(data), nil
}
