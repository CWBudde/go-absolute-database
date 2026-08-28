package absdb

import (
	"errors"
	"math/bits"
)

// Twofish as ComponentAce's ABSCipher.pas implements it.
//
// ABSCipher is a fork of Delphi Encryption Compendium (DEC) 3.0, and its
// TCipher_Twofish is *not* interchangeable with reference Twofish. Everything
// but one line agrees with the specification — the q permutations and the MDS
// tables in ABSCipher.dcu are byte-for-byte the published ones, and the round
// function, the RS key mapping and the key-dependent S-box setup all follow the
// spec — but the subkey schedule reads
//
//	SubKey[I shl 1] := A + B;
//	B := A + B shr 1;
//	SubKey[I shl 1 + 1] := ROL(B, 9);
//
// where the specification calls for K_{2i+1} = ROL(A + 2B, 9). Delphi binds
// shr tighter than +, so that line computes A + (B shr 1): a shr/shl typo that
// makes every odd subkey differ from reference Twofish. It is preserved here
// because it is what wrote the files.
//
// TestDECTwofishSelfTest pins this against DEC's own published self-test
// vector, so the deviation is verified rather than assumed. golang.org/x/
// crypto/twofish cannot be used: it is (correctly) the reference cipher, and
// fails that vector.
//
// Only the 16- and 32-byte keys that deriveKey produces are supported. The
// 192-bit key schedule is deliberately absent: no .abs file can reach it, since
// the key is always a RIPEMD-128 or RIPEMD-256 digest.
const twofishBlockSize = 16

// ErrTwofishKeySize indicates a key length this package does not implement.
var ErrTwofishKeySize = errors.New("absdb: twofish key must be 16 or 32 bytes")

// twofishCipher implements cipher.Block for DEC's Twofish variant.
type twofishCipher struct {
	subKey [40]uint32
	box    [4][256]uint32
}

func (c *twofishCipher) BlockSize() int { return twofishBlockSize }

// twofishQ holds the two 8x8 permutations q0 and q1 from the Twofish
// specification. ABSCipher.dcu carries the same 512 bytes verbatim.
//
//nolint:dupl // the two q permutations are constant tables, not duplicated logic
var twofishQ = [2][256]byte{
	{
		0xa9, 0x67, 0xb3, 0xe8, 0x04, 0xfd, 0xa3, 0x76, 0x9a, 0x92, 0x80, 0x78, 0xe4, 0xdd, 0xd1, 0x38,
		0x0d, 0xc6, 0x35, 0x98, 0x18, 0xf7, 0xec, 0x6c, 0x43, 0x75, 0x37, 0x26, 0xfa, 0x13, 0x94, 0x48,
		0xf2, 0xd0, 0x8b, 0x30, 0x84, 0x54, 0xdf, 0x23, 0x19, 0x5b, 0x3d, 0x59, 0xf3, 0xae, 0xa2, 0x82,
		0x63, 0x01, 0x83, 0x2e, 0xd9, 0x51, 0x9b, 0x7c, 0xa6, 0xeb, 0xa5, 0xbe, 0x16, 0x0c, 0xe3, 0x61,
		0xc0, 0x8c, 0x3a, 0xf5, 0x73, 0x2c, 0x25, 0x0b, 0xbb, 0x4e, 0x89, 0x6b, 0x53, 0x6a, 0xb4, 0xf1,
		0xe1, 0xe6, 0xbd, 0x45, 0xe2, 0xf4, 0xb6, 0x66, 0xcc, 0x95, 0x03, 0x56, 0xd4, 0x1c, 0x1e, 0xd7,
		0xfb, 0xc3, 0x8e, 0xb5, 0xe9, 0xcf, 0xbf, 0xba, 0xea, 0x77, 0x39, 0xaf, 0x33, 0xc9, 0x62, 0x71,
		0x81, 0x79, 0x09, 0xad, 0x24, 0xcd, 0xf9, 0xd8, 0xe5, 0xc5, 0xb9, 0x4d, 0x44, 0x08, 0x86, 0xe7,
		0xa1, 0x1d, 0xaa, 0xed, 0x06, 0x70, 0xb2, 0xd2, 0x41, 0x7b, 0xa0, 0x11, 0x31, 0xc2, 0x27, 0x90,
		0x20, 0xf6, 0x60, 0xff, 0x96, 0x5c, 0xb1, 0xab, 0x9e, 0x9c, 0x52, 0x1b, 0x5f, 0x93, 0x0a, 0xef,
		0x91, 0x85, 0x49, 0xee, 0x2d, 0x4f, 0x8f, 0x3b, 0x47, 0x87, 0x6d, 0x46, 0xd6, 0x3e, 0x69, 0x64,
		0x2a, 0xce, 0xcb, 0x2f, 0xfc, 0x97, 0x05, 0x7a, 0xac, 0x7f, 0xd5, 0x1a, 0x4b, 0x0e, 0xa7, 0x5a,
		0x28, 0x14, 0x3f, 0x29, 0x88, 0x3c, 0x4c, 0x02, 0xb8, 0xda, 0xb0, 0x17, 0x55, 0x1f, 0x8a, 0x7d,
		0x57, 0xc7, 0x8d, 0x74, 0xb7, 0xc4, 0x9f, 0x72, 0x7e, 0x15, 0x22, 0x12, 0x58, 0x07, 0x99, 0x34,
		0x6e, 0x50, 0xde, 0x68, 0x65, 0xbc, 0xdb, 0xf8, 0xc8, 0xa8, 0x2b, 0x40, 0xdc, 0xfe, 0x32, 0xa4,
		0xca, 0x10, 0x21, 0xf0, 0xd3, 0x5d, 0x0f, 0x00, 0x6f, 0x9d, 0x36, 0x42, 0x4a, 0x5e, 0xc1, 0xe0,
	},
	{
		0x75, 0xf3, 0xc6, 0xf4, 0xdb, 0x7b, 0xfb, 0xc8, 0x4a, 0xd3, 0xe6, 0x6b, 0x45, 0x7d, 0xe8, 0x4b,
		0xd6, 0x32, 0xd8, 0xfd, 0x37, 0x71, 0xf1, 0xe1, 0x30, 0x0f, 0xf8, 0x1b, 0x87, 0xfa, 0x06, 0x3f,
		0x5e, 0xba, 0xae, 0x5b, 0x8a, 0x00, 0xbc, 0x9d, 0x6d, 0xc1, 0xb1, 0x0e, 0x80, 0x5d, 0xd2, 0xd5,
		0xa0, 0x84, 0x07, 0x14, 0xb5, 0x90, 0x2c, 0xa3, 0xb2, 0x73, 0x4c, 0x54, 0x92, 0x74, 0x36, 0x51,
		0x38, 0xb0, 0xbd, 0x5a, 0xfc, 0x60, 0x62, 0x96, 0x6c, 0x42, 0xf7, 0x10, 0x7c, 0x28, 0x27, 0x8c,
		0x13, 0x95, 0x9c, 0xc7, 0x24, 0x46, 0x3b, 0x70, 0xca, 0xe3, 0x85, 0xcb, 0x11, 0xd0, 0x93, 0xb8,
		0xa6, 0x83, 0x20, 0xff, 0x9f, 0x77, 0xc3, 0xcc, 0x03, 0x6f, 0x08, 0xbf, 0x40, 0xe7, 0x2b, 0xe2,
		0x79, 0x0c, 0xaa, 0x82, 0x41, 0x3a, 0xea, 0xb9, 0xe4, 0x9a, 0xa4, 0x97, 0x7e, 0xda, 0x7a, 0x17,
		0x66, 0x94, 0xa1, 0x1d, 0x3d, 0xf0, 0xde, 0xb3, 0x0b, 0x72, 0xa7, 0x1c, 0xef, 0xd1, 0x53, 0x3e,
		0x8f, 0x33, 0x26, 0x5f, 0xec, 0x76, 0x2a, 0x49, 0x81, 0x88, 0xee, 0x21, 0xc4, 0x1a, 0xeb, 0xd9,
		0xc5, 0x39, 0x99, 0xcd, 0xad, 0x31, 0x8b, 0x01, 0x18, 0x23, 0xdd, 0x1f, 0x4e, 0x2d, 0xf9, 0x48,
		0x4f, 0xf2, 0x65, 0x8e, 0x78, 0x5c, 0x58, 0x19, 0x8d, 0xe5, 0x98, 0x57, 0x67, 0x7f, 0x05, 0x64,
		0xaf, 0x63, 0xb6, 0xfe, 0xf5, 0xb7, 0x3c, 0xa5, 0xce, 0xe9, 0x68, 0x44, 0xe0, 0x4d, 0x43, 0x69,
		0x29, 0x2e, 0xac, 0x15, 0x59, 0xa8, 0x0a, 0x9e, 0x6e, 0x47, 0xdf, 0x34, 0x35, 0x6a, 0xcf, 0xdc,
		0x22, 0xc9, 0xc0, 0x9b, 0x89, 0xd4, 0xed, 0xab, 0x12, 0xa2, 0x0d, 0x52, 0xbb, 0x02, 0x2f, 0xa9,
		0xd7, 0x61, 0x1e, 0xb4, 0x50, 0x04, 0xf6, 0xc2, 0x16, 0x25, 0x86, 0x56, 0x55, 0x09, 0xbe, 0x91,
	},
}

// twofishMDSPolynomial is the MDS field polynomial x^8 + x^6 + x^5 + x^3 + 1.
const twofishMDSPolynomial = 0x169

// twofishRSPolynomial is the RS field polynomial x^8 + x^6 + x^3 + x^2 + 1.
// DEC spells it $014D inline in its RS routine.
const twofishRSPolynomial = 0x14d

// twofishMDS is the MDS table with the outermost q permutation folded in, which
// is how DEC stores it (as Twofish_Data): column c is built over q1 for the
// even columns and q0 for the odd ones. The same 4096 bytes appear verbatim in
// ABSCipher.dcu directly after the q permutations.
var twofishMDS = buildTwofishMDS()

func buildTwofishMDS() [4][256]uint32 {
	finalQ := [4]int{1, 0, 1, 0}

	var table [4][256]uint32

	for col := range 4 {
		for z := range 256 {
			table[col][z] = twofishMDSColumn(twofishQ[finalQ[col]][z], col)
		}
	}

	return table
}

// lowByte truncates v to its least significant byte. Every narrowing in this
// file is a deliberate byte extraction from a 32-bit cipher word.
func lowByte(v uint32) byte { return byte(v & 0xFF) }

// twofishGFMult multiplies a by b in GF(2^8) modulo p.
func twofishGFMult(a, b byte, p uint32) byte {
	bv := [2]uint32{0, uint32(b)}
	pv := [2]uint32{0, p}

	var result uint32

	for range 7 {
		result ^= bv[a&1]
		a >>= 1
		bv[1] = pv[bv[1]>>7] ^ (bv[1] << 1)
	}

	result ^= bv[a&1]

	return lowByte(result)
}

// twofishMDSColumn multiplies in by column col of the MDS matrix.
func twofishMDSColumn(in byte, col int) uint32 {
	mul01 := in
	mul5B := twofishGFMult(in, 0x5B, twofishMDSPolynomial)
	mulEF := twofishGFMult(in, 0xEF, twofishMDSPolynomial)

	switch col {
	case 0:
		return uint32(mul01) | uint32(mul5B)<<8 | uint32(mulEF)<<16 | uint32(mulEF)<<24
	case 1:
		return uint32(mulEF) | uint32(mulEF)<<8 | uint32(mul5B)<<16 | uint32(mul01)<<24
	case 2:
		return uint32(mul5B) | uint32(mulEF)<<8 | uint32(mul01)<<16 | uint32(mulEF)<<24
	default:
		return uint32(mul5B) | uint32(mul01)<<8 | uint32(mulEF)<<16 | uint32(mul5B)<<24
	}
}

// twofishRS maps a pair of key words through the Reed-Solomon code, producing
// one word of key material for the key-dependent S-boxes.
func twofishRS(k0, k1 uint32) uint32 {
	r := uint32(0)

	for i := range 2 {
		if i != 0 {
			r ^= k0
		} else {
			r ^= k1
		}

		for range 4 {
			b := r >> 24

			var g2 uint32
			if b&0x80 != 0 {
				g2 = (b<<1 ^ twofishRSPolynomial) & 0xFF
			} else {
				g2 = b << 1 & 0xFF
			}

			var g3 uint32
			if b&1 != 0 {
				g3 = (b>>1&0x7F ^ twofishRSPolynomial>>1) ^ g2
			} else {
				g3 = (b >> 1 & 0x7F) ^ g2
			}

			r = r<<8 ^ g3<<24 ^ g2<<16 ^ g3<<8 ^ b
		}
	}

	return r
}

// twofishF32 is the key-schedule h function: it runs x through the q
// permutations, xoring in the key words, then through the MDS table.
func twofishF32(x uint32, k []uint32, keyLen int) uint32 {
	a := lowByte(x)
	b := lowByte(x >> 8)
	c := lowByte(x >> 16)
	d := lowByte(x >> 24)

	if keyLen == 32 {
		a = twofishQ[1][a] ^ lowByte(k[3])
		b = twofishQ[0][b] ^ lowByte(k[3]>>8)
		c = twofishQ[0][c] ^ lowByte(k[3]>>16)
		d = twofishQ[1][d] ^ lowByte(k[3]>>24)

		// DEC guards this layer with "Size >= 24", so a 256-bit key runs it too.
		a = twofishQ[1][a] ^ lowByte(k[2])
		b = twofishQ[1][b] ^ lowByte(k[2]>>8)
		c = twofishQ[0][c] ^ lowByte(k[2]>>16)
		d = twofishQ[0][d] ^ lowByte(k[2]>>24)
	}

	a = twofishQ[0][a] ^ lowByte(k[1])
	b = twofishQ[1][b] ^ lowByte(k[1]>>8)
	c = twofishQ[0][c] ^ lowByte(k[1]>>16)
	d = twofishQ[1][d] ^ lowByte(k[1]>>24)

	a = twofishQ[0][a] ^ lowByte(k[0])
	b = twofishQ[0][b] ^ lowByte(k[0]>>8)
	c = twofishQ[1][c] ^ lowByte(k[0]>>16)
	d = twofishQ[1][d] ^ lowByte(k[0]>>24)

	return twofishMDS[0][a] ^ twofishMDS[1][b] ^ twofishMDS[2][c] ^ twofishMDS[3][d]
}

// newTwofish builds the cipher from a 16- or 32-byte key.
func newTwofish(key []byte) (*twofishCipher, error) {
	if len(key) != 16 && len(key) != 32 {
		return nil, ErrTwofishKeySize
	}

	var words [8]uint32

	for i := range len(key) / 4 {
		words[i] = uint32(key[4*i]) | uint32(key[4*i+1])<<8 |
			uint32(key[4*i+2])<<16 | uint32(key[4*i+3])<<24
	}

	var (
		even, odd [4]uint32
		boxKey    [4]uint32
	)

	pairs := len(key) / 8

	for i := range pairs {
		even[i] = words[2*i]
		odd[i] = words[2*i+1]
		boxKey[pairs-1-i] = twofishRS(even[i], odd[i])
	}

	c := new(twofishCipher)

	x := uint32(0)

	for i := range 20 {
		a := twofishF32(x, even[:], len(key))
		b := bits.RotateLeft32(twofishF32(x+0x01010101, odd[:], len(key)), 8)

		c.subKey[2*i] = a + b
		// DEC writes "A + B shr 1" where the specification calls for A + 2*B.
		c.subKey[2*i+1] = bits.RotateLeft32(a+(b>>1), 9)

		x += 0x02020202
	}

	if len(key) == 16 {
		c.setupBox128(boxKey)
	} else {
		c.setupBox256(boxKey)
	}

	return c, nil
}

// xorTable fills dst with src xored byte-wise by the low byte of value.
func xorTable(dst *[256]byte, src *[256]byte, value uint32) {
	v := lowByte(value)
	for i := range src {
		dst[i] = src[i] ^ v
	}
}

func loadWordsLE(src []byte) [4]uint32 {
	var w [4]uint32

	for i := range 4 {
		w[i] = uint32(src[4*i]) | uint32(src[4*i+1])<<8 |
			uint32(src[4*i+2])<<16 | uint32(src[4*i+3])<<24
	}

	return w
}

func storeWordsLE(dst []byte, w [4]uint32) {
	for i, v := range w {
		dst[4*i] = lowByte(v)
		dst[4*i+1] = lowByte(v >> 8)
		dst[4*i+2] = lowByte(v >> 16)
		dst[4*i+3] = lowByte(v >> 24)
	}
}

// Encrypt encrypts one 16-byte block. dst and src may be the same slice.
func (c *twofishCipher) Encrypt(dst, src []byte) {
	w := loadWordsLE(src)

	a := w[0] ^ c.subKey[0]
	b := w[1] ^ c.subKey[1]
	d0 := w[2] ^ c.subKey[2]
	e := w[3] ^ c.subKey[3]

	s := c.subKey[8:]

	for range 8 {
		x := c.g(a)
		y := c.gRot(b)
		e = bits.RotateLeft32(e, 1)
		d0 ^= x + y + s[0]
		e ^= x + y<<1 + s[1]
		d0 = bits.RotateLeft32(d0, -1)

		x = c.g(d0)
		y = c.gRot(e)
		b = bits.RotateLeft32(b, 1)
		a ^= x + y + s[2]
		b ^= x + y<<1 + s[3]
		a = bits.RotateLeft32(a, -1)

		s = s[4:]
	}

	storeWordsLE(dst, [4]uint32{
		d0 ^ c.subKey[4],
		e ^ c.subKey[5],
		a ^ c.subKey[6],
		b ^ c.subKey[7],
	})
}

// Decrypt decrypts one 16-byte block. dst and src may be the same slice.
func (c *twofishCipher) Decrypt(dst, src []byte) {
	w := loadWordsLE(src)

	d0 := w[0] ^ c.subKey[4]
	e := w[1] ^ c.subKey[5]
	a := w[2] ^ c.subKey[6]
	b := w[3] ^ c.subKey[7]

	base := 36

	for range 8 {
		s := c.subKey[base : base+4]

		x := c.g(d0)
		y := c.gRot(e)
		a = bits.RotateLeft32(a, 1)
		b ^= x + y<<1 + s[3]
		a ^= x + y + s[2]
		b = bits.RotateLeft32(b, -1)

		x = c.g(a)
		y = c.gRot(b)
		d0 = bits.RotateLeft32(d0, 1)
		e ^= x + y<<1 + s[1]
		d0 ^= x + y + s[0]
		e = bits.RotateLeft32(e, -1)

		base -= 4
	}

	storeWordsLE(dst, [4]uint32{
		a ^ c.subKey[0],
		b ^ c.subKey[1],
		d0 ^ c.subKey[2],
		e ^ c.subKey[3],
	})
}

// setupBox128 builds the key-dependent S-boxes for a 128-bit key.
func (c *twofishCipher) setupBox128(boxKey [4]uint32) {
	var l [256]byte

	steps := []struct {
		src   int // q permutation used for the first layer
		outer int // q permutation used for the second layer
	}{
		{0, 0}, {1, 0}, {0, 1}, {1, 1},
	}

	for col, st := range steps {
		xorTable(&l, &twofishQ[st.src], boxKey[1]>>(8*col))
		a := lowByte(boxKey[0] >> (8 * col))

		for i := range 256 {
			c.box[col][i] = twofishMDS[col][twofishQ[st.outer][l[i]]^a]
		}
	}
}

// setupBox256 builds the key-dependent S-boxes for a 256-bit key.
func (c *twofishCipher) setupBox256(boxKey [4]uint32) {
	var k, l [256]byte

	steps := []struct {
		first  int // q permutation applied to boxKey[3]'s layer
		second int // q permutation applied on top of it
		third  int // q permutation applied to boxKey[1]'s layer
		outer  int // q permutation applied to boxKey[0]'s layer
	}{
		{1, 1, 0, 0}, {0, 1, 1, 0}, {0, 0, 0, 1}, {1, 0, 1, 1},
	}

	for col, st := range steps {
		shift := 8 * col

		xorTable(&k, &twofishQ[st.first], boxKey[3]>>shift)

		for i := range 256 {
			l[i] = twofishQ[st.second][k[i]]
		}

		xorTable(&l, &l, boxKey[2]>>shift)

		a := lowByte(boxKey[0] >> shift)
		b := lowByte(boxKey[1] >> shift)

		for i := range 256 {
			c.box[col][i] = twofishMDS[col][twofishQ[st.outer][twofishQ[st.third][l[i]]^b]^a]
		}
	}
}

// g applies the key-dependent S-boxes and MDS matrix to a word.
func (c *twofishCipher) g(x uint32) uint32 {
	return c.box[0][lowByte(x)] ^ c.box[1][lowByte(x>>8)] ^
		c.box[2][lowByte(x>>16)] ^ c.box[3][lowByte(x>>24)]
}

// gRot is g applied to x rotated left by 8 bits, which is how the second half
// of each round consumes its input.
func (c *twofishCipher) gRot(x uint32) uint32 {
	return c.box[1][lowByte(x)] ^ c.box[2][lowByte(x>>8)] ^
		c.box[3][lowByte(x>>16)] ^ c.box[0][lowByte(x>>24)]
}
