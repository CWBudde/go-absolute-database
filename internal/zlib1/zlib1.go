package zlib1

import "hash/adler32"

// zlibHeader is the two-byte zlib stream header deflateInit2 emits for
// compression level 1 with windowBits = 15 (deflate.c):
//
//	CMF = (Z_DEFLATED) | ((windowBits-8) << 4) = 0x78
//	FLG carries the level flags (0 for level < 2) plus the check bits that
//	make the 16-bit header a multiple of 31, giving 0x01.
//
// The Absolute Database engine's internal files all start with these two bytes.
var zlibHeader = [2]byte{0x78, 0x01}

// Compress returns src compressed as a zlib stream byte-identical to the C
// zlib library's output at compression level 1.
//
// It never fails and never panics: every input, including nil and inputs larger
// than the 32 KiB window, produces a complete stream.
func Compress(src []byte) []byte {
	s := newDeflateState(src)

	s.out = append(s.out, zlibHeader[0], zlibHeader[1])
	s.deflateFast()

	// The zlib trailer is the adler32 of the *uncompressed* input, big-endian.
	sum := adler32.Checksum(src)
	//nolint:gosec // splitting the checksum into four bytes; the truncation is the point.
	s.out = append(s.out, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))

	return s.out
}
