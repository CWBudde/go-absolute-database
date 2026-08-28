package absdb

import (
	"encoding/binary"
	"math/bits"
)

// ripemd256Sum computes the RIPEMD-256 hash of data, returning a 32-byte digest.
//
// RIPEMD-256 is the 256-bit extension of RIPEMD-128. It uses the identical
// message schedule, rotation amounts, round functions and round constants; the
// only differences are the doubled state (eight chaining words instead of
// four), the exchange of one working word between the two lines after every
// round, and the absence of the cross-line mixing at the end of a block. The
// schedule and rotation tables in ripemd128.go are therefore used verbatim.
func ripemd256Sum(data []byte) [32]byte {
	h := [8]uint32{
		0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476,
		0x76543210, 0xFEDCBA98, 0x89ABCDEF, 0x01234567,
	}

	padded := ripemd256Pad(data)

	for i := 0; i < len(padded); i += 64 {
		var x [16]uint32
		for j := range 16 {
			x[j] = binary.LittleEndian.Uint32(padded[i+j*4 : i+j*4+4])
		}

		ripemd256Block(&h, x)
	}

	var digest [32]byte
	for i, v := range h {
		binary.LittleEndian.PutUint32(digest[i*4:i*4+4], v)
	}

	return digest
}

// ripemd256Pad applies MD-strengthening padding to the input.
func ripemd256Pad(data []byte) []byte {
	msgLen := len(data)
	bitLen := uint64(msgLen) * 8

	padLen := 64 - ((msgLen + 9) % 64)
	if padLen == 64 {
		padLen = 0
	}

	buf := make([]byte, msgLen+1+padLen+8)
	copy(buf, data)
	buf[msgLen] = 0x80
	binary.LittleEndian.PutUint64(buf[len(buf)-8:], bitLen)

	return buf
}

// ripemd256Block processes one 512-bit block, updating the chaining state in place.
func ripemd256Block(h *[8]uint32, x [16]uint32) {
	al, bl, cl, dl := h[0], h[1], h[2], h[3]
	ar, br, cr, dr := h[4], h[5], h[6], h[7]

	for j := range 64 {
		fl, fr, kl, kr := ripemd256Round(j, bl, cl, dl, br, cr, dr)

		tl := bits.RotateLeft32(al+fl+x[ripemd128RL[j]]+kl, ripemd128SL[j])
		al, dl, cl, bl = dl, cl, bl, tl

		tr := bits.RotateLeft32(ar+fr+x[ripemd128RR[j]]+kr, ripemd128SR[j])
		ar, dr, cr, br = dr, cr, br, tr

		// After every round one working word is swapped between the lines.
		switch j {
		case 15:
			al, ar = ar, al
		case 31:
			bl, br = br, bl
		case 47:
			cl, cr = cr, cl
		case 63:
			dl, dr = dr, dl
		}
	}

	h[0] += al
	h[1] += bl
	h[2] += cl
	h[3] += dl
	h[4] += ar
	h[5] += br
	h[6] += cr
	h[7] += dr
}

// ripemd256Round returns the round function results and round constants for
// step j of both lines. The right line runs the round functions in reverse
// order, which is why its cases are mirrored.
func ripemd256Round(j int, bl, cl, dl, br, cr, dr uint32) (fl, fr, kl, kr uint32) {
	switch j >> 4 {
	case 0:
		return bl ^ cl ^ dl, br&dr | cr&^dr, 0x00000000, 0x50A28BE6
	case 1:
		return bl&cl | ^bl&dl, (br | ^cr) ^ dr, 0x5A827999, 0x5C4DD124
	case 2:
		return (bl | ^cl) ^ dl, br&cr | ^br&dr, 0x6ED9EBA1, 0x6D703EF3
	default:
		return bl&dl | cl&^dl, br ^ cr ^ dr, 0x8F1BBCDC, 0x00000000
	}
}
