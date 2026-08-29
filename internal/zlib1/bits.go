// Package zlib1 is a port of the C zlib library's compression level 1, exact
// enough to reproduce its output byte for byte.
//
// The ComponentAce Absolute Database engine writes every compressed internal
// file (schema, table info, catalog) with C zlib at level 1, and this package's
// writes are held to byte-for-byte equality with what the engine produces.
// Go's compress/flate cannot be used for that: its level 1 is its own fast
// encoder, not zlib's deflate_fast, and it matches none of the engine's streams
// at any level.
//
// The code follows zlib 1.2.13's deflate.c and trees.c closely, function by
// function, and the comments name the zlib symbol each piece corresponds to.
// Read it side by side with those two files; deviations from them are bugs.
//
// The configuration reproduced here is zlib's configuration_table row for
// level 1 — {good_length 4, max_lazy 4, nice_length 8, max_chain 4,
// deflate_fast} — with the deflateInit defaults windowBits = 15 and
// memLevel = 8.
//
// # Attribution
//
// This package is an ALTERED version of zlib, plainly marked as such, as
// zlib's licence requires. It is a Go rewrite, it implements level 1 only, and
// it compresses but does not decompress. Any bug in it is this project's, not
// zlib's: report it here, never to the zlib authors.
//
//	zlib.h -- interface of the 'zlib' general purpose compression library
//	version 1.2.13, October 13th, 2022
//
//	Copyright (C) 1995-2022 Jean-loup Gailly and Mark Adler
//
//	This software is provided 'as-is', without any express or implied
//	warranty.  In no event will the authors be held liable for any damages
//	arising from the use of this software.
//
//	Permission is granted to anyone to use this software for any purpose,
//	including commercial applications, and to alter it and redistribute it
//	freely, subject to the following restrictions:
//
//	1. The origin of this software must not be misrepresented; you must not
//	   claim that you wrote the original software. If you use this software
//	   in a product, an acknowledgment in the product documentation would be
//	   appreciated but is not required.
//	2. Altered source versions must be plainly marked as such, and must not be
//	   misrepresented as being the original software.
//	3. This notice may not be removed or altered from any source distribution.
//
//	Jean-loup Gailly        Mark Adler
//	jloup@gzip.org          madler@alumni.caltech.edu
package zlib1

// bufSize is zlib's Buf_size (trees.c): the width in bits of the bit
// accumulator bi_buf. zlib uses a 16-bit accumulator and flushes it two bytes
// at a time, and the exact flush points are visible in the output only through
// bi_flush/bi_windup, so the width has to be reproduced rather than widened.
const bufSize = 16

// bitWriter is zlib's deflate_state bit-output half: bi_buf, bi_valid and the
// pending output buffer. Deflate streams are LSB-first, so send_bits appends
// each value's low bit first.
type bitWriter struct {
	out []byte // zlib's pending_buf, already flushed to the caller's output

	biBuf   uint16 // zlib's bi_buf: bit accumulator
	biValid int    // zlib's bi_valid: bits currently in biBuf
}

// putByte is zlib's put_byte macro.
func (w *bitWriter) putByte(b byte) {
	w.out = append(w.out, b)
}

// putShort is zlib's put_short macro: little-endian, as deflate's LEN/NLEN
// fields and the bi_buf spill both are.
func (w *bitWriter) putShort(v uint16) {
	//nolint:gosec // splitting a uint16 into its two bytes; the truncation is the point.
	w.out = append(w.out, byte(v), byte(v>>8))
}

// sendBits is zlib's send_bits (trees.c, the non-debug branch). value must fit
// in length bits and length must not exceed bufSize.
func (w *bitWriter) sendBits(value uint32, length int) {
	val := uint16(value) //nolint:gosec // send_bits' value always fits in length <= 16 bits; zlib casts to ush here too.

	if w.biValid > bufSize-length {
		w.biBuf |= val << w.biValid
		w.putShort(w.biBuf)
		// biValid is >= 1 whenever this branch is taken (length <= bufSize),
		// so the shift count stays below 16, exactly as in zlib.
		w.biBuf = val >> (bufSize - w.biValid)
		w.biValid += length - bufSize

		return
	}

	w.biBuf |= val << w.biValid
	w.biValid += length
}

// biFlush is zlib's bi_flush: flush whole bytes out of the accumulator without
// aligning to a byte boundary.
func (w *bitWriter) biFlush() {
	switch {
	case w.biValid == bufSize:
		w.putShort(w.biBuf)

		w.biBuf = 0
		w.biValid = 0
	case w.biValid >= 8:
		w.putByte(byte(w.biBuf)) //nolint:gosec // only the low eight bits are complete; bi_flush truncates here too.

		w.biBuf >>= 8
		w.biValid -= 8
	}
}

// biWindup is zlib's bi_windup: flush the accumulator and align to a byte
// boundary, zero-padding the last byte. This is what produces the trailing
// padding bits of a final block and the alignment before a stored block.
func (w *bitWriter) biWindup() {
	switch {
	case w.biValid > 8:
		w.putShort(w.biBuf)
	case w.biValid > 0:
		w.putByte(byte(w.biBuf)) //nolint:gosec // at most eight bits are pending; the high half is zero.
	}

	w.biBuf = 0
	w.biValid = 0
}

// biReverse is zlib's bi_reverse: reverse the low len bits of code. Huffman
// codes are assigned MSB-first but written LSB-first, so every code is stored
// pre-reversed.
func biReverse(code uint32, length int) uint32 {
	res := uint32(0)

	for ; length > 0; length-- {
		res |= code & 1
		code >>= 1
		res <<= 1
	}

	return res >> 1
}
