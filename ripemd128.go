package absdb

import (
	"encoding/binary"
	"math/bits"
)

// ripemd128 round constants.
var (
	// Left line: message word selection per round.
	ripemd128RL = [64]int{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
		3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
		1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
	}

	// Right line: message word selection per round.
	ripemd128RR = [64]int{
		5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
		6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
		15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
		8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
	}

	// Left line: rotation amounts.
	ripemd128SL = [64]int{
		11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
		7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
		11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
		11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
	}

	// Right line: rotation amounts.
	ripemd128SR = [64]int{
		8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
		9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
		9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
		15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
	}
)

// ripemd128Sum computes the RIPEMD-128 hash of data, returning a 16-byte digest.
func ripemd128Sum(data []byte) [16]byte {
	h0 := uint32(0x67452301)
	h1 := uint32(0xefcdab89)
	h2 := uint32(0x98badcfe)
	h3 := uint32(0x10325476)

	padded := ripemd128Pad(data)

	for i := 0; i < len(padded); i += 64 {
		var x [16]uint32
		for j := range 16 {
			x[j] = binary.LittleEndian.Uint32(padded[i+j*4 : i+j*4+4])
		}

		al, bl, cl, dl, ar, br, cr, dr := ripemd128Block(x, h0, h1, h2, h3)

		t := h1 + cl + dr
		h1 = h2 + dl + ar
		h2 = h3 + al + br
		h3 = h0 + bl + cr
		h0 = t
	}

	var digest [16]byte
	binary.LittleEndian.PutUint32(digest[0:4], h0)
	binary.LittleEndian.PutUint32(digest[4:8], h1)
	binary.LittleEndian.PutUint32(digest[8:12], h2)
	binary.LittleEndian.PutUint32(digest[12:16], h3)

	return digest
}

// ripemd128Pad applies MD-strengthening padding to the input.
func ripemd128Pad(data []byte) []byte {
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

// ripemd128Block processes one 512-bit block through both parallel lines.
func ripemd128Block(x [16]uint32, h0, h1, h2, h3 uint32) (al, bl, cl, dl, ar, br, cr, dr uint32) {
	al, bl, cl, dl = h0, h1, h2, h3
	ar, br, cr, dr = h0, h1, h2, h3

	for j := range 64 {
		var fl, fr, kl, kr uint32

		switch j >> 4 {
		case 0:
			fl = bl ^ cl ^ dl
			fr = br&dr | cr&^dr
			kl = 0x00000000
			kr = 0x50A28BE6
		case 1:
			fl = bl&cl | ^bl&dl
			fr = (br | ^cr) ^ dr
			kl = 0x5A827999
			kr = 0x5C4DD124
		case 2:
			fl = (bl | ^cl) ^ dl
			fr = br&cr | ^br&dr
			kl = 0x6ED9EBA1
			kr = 0x6D703EF3
		case 3:
			fl = bl&dl | cl&^dl
			fr = br ^ cr ^ dr
			kl = 0x8F1BBCDC
			kr = 0x00000000
		}

		tl := al + fl + x[ripemd128RL[j]] + kl
		tl = bits.RotateLeft32(tl, ripemd128SL[j])
		al, dl, cl, bl = dl, cl, bl, tl

		tr := ar + fr + x[ripemd128RR[j]] + kr
		tr = bits.RotateLeft32(tr, ripemd128SR[j])
		ar, dr, cr, br = dr, cr, br, tr
	}

	return al, bl, cl, dl, ar, br, cr, dr
}
