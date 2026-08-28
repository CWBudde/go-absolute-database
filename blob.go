package absdb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// PageTypeBlob is the page type for BLOB data storage.
	PageTypeBlob = 11

	// blobPageHeaderSize is the size of the BLOB page header (3 × int64).
	blobPageHeaderSize = 24

	// blobRefSize is the size of a BLOB reference as stored in a record field:
	// PageNo (int32) followed by ItemNo (uint16).
	blobRefSize = 6

	// maxBlobChainPages bounds how many pages a single BLOB may span. Nothing
	// on disk constrains NextPageNo, so without this an unlucky or hostile
	// chain would be walked until the process runs out of memory.
	maxBlobChainPages = 65536

	// blobAllocChunk caps the capacity reserved up front for a chained BLOB.
	// The declared total size comes straight off disk, so the buffer is grown
	// as pages actually arrive instead of being trusted in one allocation.
	blobAllocChunk = 1 << 20
)

const (
	// maxDecompressedSize is the hard ceiling on the output of a single zlib
	// stream, whatever the on-disk header declares.
	maxDecompressedSize = 256 << 20

	// maxCompressionRatio bounds the expansion factor of a zlib stream, so a
	// 4 KiB page can never inflate to more than 4 MiB.
	maxCompressionRatio = 1000

	// minDecompressAllowance keeps very small streams decompressible, which
	// maxCompressionRatio alone would not allow.
	minDecompressAllowance = 1 << 20

	// decompressSlack tolerates a declared size slightly below what the stream
	// actually yields (trailing padding).
	decompressSlack = 4096
)

var (
	ErrBlobNotFound = errors.New("absdb: BLOB data not found")
	ErrBlobTrunc    = errors.New("absdb: BLOB data truncated")

	// ErrBlobSize reports a BLOB header whose declared sizes are negative or
	// larger than the file could possibly hold.
	ErrBlobSize = errors.New("absdb: BLOB size out of range")

	// ErrBlobChain reports a malformed BLOB page chain: a cycle, a page
	// without a disk page header, or a chain too long to be plausible.
	ErrBlobChain = errors.New("absdb: malformed BLOB page chain")
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
// Short input yields a null reference rather than a panic.
func readBlobRef(data []byte) BlobRef {
	if len(data) < blobRefSize {
		return BlobRef{}
	}

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
// It returns a null reference when col is out of range or the record's field
// data is too short to hold the reference.
func (rec Record) BlobRef(col int) BlobRef {
	if rec.reader == nil || col < 0 || col >= len(rec.reader.fieldOffsets) {
		return BlobRef{}
	}

	off := rec.reader.fieldOffsets[col]
	if off < 0 || off > len(rec.fieldData)-blobRefSize {
		return BlobRef{}
	}

	return readBlobRef(rec.fieldData[off : off+blobRefSize])
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

	if page.Header == nil {
		return nil, fmt.Errorf("%w: page %d has no disk page header", ErrBlobNotFound, pageNo)
	}

	if page.Header.PageType != PageTypeBlob {
		return nil, fmt.Errorf("%w: page %d is type %d, not BLOB",
			ErrBlobNotFound, pageNo, page.Header.PageType)
	}

	hdr, err := readBlobPageHeader(page.PageData())
	if err != nil {
		return nil, err
	}

	if hdr.ItemCount < 1 {
		return nil, fmt.Errorf("%w: page %d has no items", ErrBlobNotFound, pageNo)
	}

	err = db.validateBlobHeader(hdr, pageNo)
	if err != nil {
		return nil, err
	}

	compressedData, err := db.blobPayload(page, hdr)
	if err != nil {
		return nil, err
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

// validateBlobHeader rejects declared sizes that are negative or larger than
// the file could possibly hold, before they reach a slice expression or an
// allocation. Both sizes are raw int64 values straight off disk.
func (db *File) validateBlobHeader(hdr blobPageHeader, pageNo int) error {
	maxOnDisk := int64(db.PageCount()) * int64(db.payloadSize())

	if hdr.CompressedSize < 0 || hdr.CompressedSize > maxOnDisk {
		return fmt.Errorf("%w: page %d declares compressed size %d, file holds at most %d",
			ErrBlobSize, pageNo, hdr.CompressedSize, maxOnDisk)
	}

	if hdr.UncompressedSize < 0 || hdr.UncompressedSize > maxDecompressedSize {
		return fmt.Errorf("%w: page %d declares uncompressed size %d, limit is %d",
			ErrBlobSize, pageNo, hdr.UncompressedSize, maxDecompressedSize)
	}

	return nil
}

// blobPayload returns the stored BLOB bytes of a page, following the
// NextPageNo chain when the declared size does not fit into a single page.
// hdr must already have passed validateBlobHeader.
func (db *File) blobPayload(page Page, hdr blobPageHeader) ([]byte, error) {
	data := page.PageData()
	if len(data) < blobPageHeaderSize {
		return nil, ErrBlobTrunc
	}

	payload := data[blobPageHeaderSize:]
	if int64(len(payload)) >= hdr.CompressedSize {
		return payload[:hdr.CompressedSize], nil
	}

	// Handle multi-page BLOBs: if the compressed size exceeds the single page,
	// follow the page chain (NextPageNo) if available.
	if page.Header != nil && page.Header.NextPageNo >= 0 {
		return db.readBlobChain(page, hdr.CompressedSize)
	}

	// Single-page BLOB where the header size exceeds the available page data
	// and nothing is chained. This happens when the size field includes
	// padding. Return what we have — the trailing bytes are typically zeros.
	return payload, nil
}

// readBlobChain reads BLOB data that spans multiple pages via NextPageNo
// chaining. The chain is guarded against cycles and against implausible
// length; no fixture chains, so the traversal itself is unverified.
func (db *File) readBlobChain(firstPage Page, totalSize int64) ([]byte, error) {
	if firstPage.Header == nil {
		return nil, fmt.Errorf("%w: page %d has no disk page header",
			ErrBlobChain, firstPage.Number)
	}

	if totalSize < 0 {
		return nil, fmt.Errorf("%w: negative chain size %d", ErrBlobSize, totalSize)
	}

	firstData := firstPage.PageData()
	if len(firstData) < blobPageHeaderSize {
		return nil, ErrBlobTrunc
	}

	// First page: data starts after header.
	result := make([]byte, 0, min(totalSize, int64(blobAllocChunk)))
	result = append(result, firstData[blobPageHeaderSize:]...)

	// Follow chain. Every page number is recorded so that a self-referential
	// or looping NextPageNo terminates instead of growing result forever.
	visited := map[int]bool{firstPage.Number: true}
	nextPageNo := int(firstPage.Header.NextPageNo)

	for int64(len(result)) < totalSize && nextPageNo >= 0 {
		if visited[nextPageNo] {
			return nil, fmt.Errorf("%w: page %d visited twice", ErrBlobChain, nextPageNo)
		}

		if len(visited) >= maxBlobChainPages {
			return nil, fmt.Errorf("%w: longer than %d pages", ErrBlobChain, maxBlobChainPages)
		}

		visited[nextPageNo] = true

		page, err := db.ReadPage(nextPageNo)
		if err != nil {
			return nil, fmt.Errorf("absdb: reading BLOB chain page %d: %w", nextPageNo, err)
		}

		if page.Header == nil {
			return nil, fmt.Errorf("%w: page %d has no disk page header", ErrBlobChain, nextPageNo)
		}

		result = append(result, page.PageData()...)
		nextPageNo = int(page.Header.NextPageNo)
	}

	if int64(len(result)) < totalSize {
		return nil, fmt.Errorf("%w: expected %d bytes, got %d", ErrBlobTrunc, totalSize, len(result))
	}

	return result[:totalSize], nil
}

// decompressBlob decompresses zlib-compressed BLOB data. uncompressedSize is
// the size declared in the BLOB page header; it bounds the output so a crafted
// stream cannot expand without limit.
func decompressBlob(compressed []byte, uncompressedSize int64) ([]byte, error) {
	result, err := inflateLimited(compressed, uncompressedSize)
	if err != nil {
		return nil, fmt.Errorf("absdb: BLOB decompression: %w", err)
	}

	return result, nil
}

// inflateLimit returns the maximum number of bytes a zlib stream of the given
// compressed length may legitimately produce. The declared size is honoured,
// but only within a ceiling derived from the input length, so that a bogus
// declaration cannot itself become the bomb.
func inflateLimit(compressedLen int, declared int64) (int64, error) {
	if declared < 0 {
		return 0, fmt.Errorf("negative uncompressed size %d", declared)
	}

	ceiling := min(max(int64(compressedLen)*maxCompressionRatio, minDecompressAllowance),
		maxDecompressedSize)

	if declared > ceiling {
		return 0, fmt.Errorf("declared uncompressed size %d exceeds limit %d", declared, ceiling)
	}

	return min(declared+decompressSlack, ceiling), nil
}

// inflateLimited zlib-decompresses compressed, producing at most as many bytes
// as the declared uncompressed size allows. A stream that yields more is
// rejected rather than read to the end.
func inflateLimited(compressed []byte, declared int64) ([]byte, error) {
	limit, err := inflateLimit(len(compressed), declared)
	if err != nil {
		return nil, err
	}

	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("zlib: %w", err)
	}
	defer r.Close()

	result, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("zlib: %w", err)
	}

	if int64(len(result)) > limit {
		return nil, fmt.Errorf("stream yields more than %d bytes, declared %d", limit, declared)
	}

	return result, nil
}

// Blob reads the BLOB data for the given column from the database.
// Returns the raw bytes. Returns nil for NULL BLOBs.
func (rec Record) Blob(col int) ([]byte, error) {
	if rec.reader == nil || rec.reader.db == nil {
		return nil, ErrBlobNotFound
	}

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
