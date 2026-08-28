package absdb

import (
	"errors"
	"math/bits"
)

// Rijndael as ComponentAce's ABSCipher.pas implements it.
//
// ABSCipher is a fork of Delphi Encryption Compendium (DEC) 3.0, and this is
// DEC's TCipher_Rijndael: a 128-bit-block Rijndael with 10, 12 or 14 rounds for
// 128-, 192- and 256-bit keys. The round function — SubBytes, ShiftRows,
// MixColumns over GF(2^8) modulo x^8+x^4+x^3+x+1 ($11B), and their inverses —
// is plain AES, and so is the block layout: four little-endian words, byte 0 of
// word 0 first.
//
// The key schedule is not. DEC's TCipher_Rijndael.Init builds its own expansion
// and it agrees with the AES key schedule for 128- and 192-bit keys but not for
// 256-bit ones, where the two diverge at expanded word 12. For a 14-round key
// DEC first chains all eight key words (K[i] ^= K[i-1] for i = 1..7), then XORs
// SubWord(K[3]) into the already-chained K[4], then re-chains K[5..7] on top of
// that. AES-256 instead computes w[i] = w[i-8] xor SubWord(w[i-1]) at the
// halfway point, with no such double chaining. This is a genuine deviation, of
// the same family as the shr/shl typo in DEC's Twofish (see twofish.go), and it
// is why crypto/aes reads Rijndael-128 .abs files correctly but not
// Rijndael-256 ones.
//
// The deviation must never be "fixed": every Rijndael-256 .abs file in
// existence was written with it, and removing it makes all of them
// undecryptable. Three tests pin the construction. TestDECRijndaelSelfTest
// reproduces the self-test vector compiled into DEC's own TCipher_Rijndael (a
// 16-byte key, so the 10-round path). TestRijndaelMatchesAES checks the 16- and
// 24-byte key paths against crypto/aes, which is the strongest available check
// on the round function and on the part of the schedule that does not deviate.
// TestRijndael256NotAES asserts that the 32-byte path still differs from
// crypto/aes, and TestRijndael256ControlBlockCRC decrypts a real file's
// ControlBlock and checks its stored CRC.
//
// The only literal in this file is DEC's 30-byte round-constant stream. The
// S-box is derived from its algebraic definition, and the inverse S-box, the
// encryption and decryption round tables and the InvMixColumns table the
// decryption schedule uses are in turn derived from that, all at package
// initialisation.
const rijndaelBlockSize = 16

// rijndaelMaxRounds is the round count for a 256-bit key, and so the size of
// the round-key arrays. Shorter keys use fewer rounds and leave the tail
// unused.
const rijndaelMaxRounds = 14

// rijndaelPolynomial is the AES field polynomial x^8+x^4+x^3+x+1. Note that it
// is not Square's $1F5, even though DEC ships both ciphers side by side.
const rijndaelPolynomial = 0x11B

// ErrRijndaelKeySize indicates a key length this cipher does not accept.
var ErrRijndaelKeySize = errors.New("absdb: rijndael key must be 16, 24 or 32 bytes")

// rijndaelCipher implements cipher.Block for DEC's Rijndael. enc holds the
// round keys in encryption order; dec holds them reversed, with InvMixColumns
// folded into every key but the first and the last, so that decryption can use
// the equivalent inverse cipher and share the round structure with encryption.
type rijndaelCipher struct {
	enc    [rijndaelMaxRounds + 1][4]uint32
	dec    [rijndaelMaxRounds + 1][4]uint32
	rounds int
}

func (c *rijndaelCipher) BlockSize() int { return rijndaelBlockSize }

// Encrypt encrypts one 16-byte block. dst and src may be the same slice.
func (c *rijndaelCipher) Encrypt(dst, src []byte) {
	rijndaelTransform(dst, src, &c.enc, c.rounds, &rijndaelTE, &rijndaelSE, 1)
}

// Decrypt decrypts one 16-byte block. dst and src may be the same slice.
func (c *rijndaelCipher) Decrypt(dst, src []byte) {
	rijndaelTransform(dst, src, &c.dec, c.rounds, &rijndaelTD, &rijndaelSD, 3)
}

// rijndaelSE is the AES S-box. It is derived rather than transcribed: the
// S-box is the multiplicative inverse in GF(2^8) modulo rijndaelPolynomial
// (with zero mapped to zero) followed by the standard affine transform, which
// buildRijndaelSE computes directly. Deriving it keeps this file free of a
// 256-entry literal and makes the construction auditable; the result is pinned
// end to end by TestDECRijndaelSelfTest, and TestRijndaelSBox checks the
// published spot values.
var rijndaelSE = buildRijndaelSE()

// rijndaelRcon is DEC's RND_Data, the round-constant byte stream the key
// schedule consumes one byte per expansion step. It is the ordinary AES Rcon
// sequence 1, 2, 4, ... doubled in GF(2^8), carried far enough for every key
// length.
var rijndaelRcon = [30]byte{
	0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80, 0x1B, 0x36,
	0x6C, 0xD8, 0xAB, 0x4D, 0x9A, 0x2F, 0x5E, 0xBC, 0x63, 0xC6,
	0x97, 0x35, 0x6A, 0xD4, 0xB3, 0x7D, 0xFA, 0xEF, 0xC5, 0x91,
}

// rijndaelMixColumn is the first column of the MixColumns matrix, {2,3,1,1}
// read as the coefficients that byte 0 of the input contributes to bytes 0..3
// of the output.
var rijndaelMixColumn = [4]byte{2, 1, 1, 3}

// rijndaelInvMixColumn is the same for the inverse matrix: {14,11,13,9}
// reordered the same way.
var rijndaelInvMixColumn = [4]byte{14, 9, 13, 11}

// rijndaelSD is the inverse of rijndaelSE, used by the last round of
// decryption.
var rijndaelSD = buildRijndaelSD()

// rijndaelTE folds the S-box into MixColumns for encryption. Column j is
// column 0 rotated left by 8j bits, matching DEC's Rijndael_T[0..3].
var rijndaelTE = buildRijndaelTable(&rijndaelSE, rijndaelMixColumn)

// rijndaelTD folds the inverse S-box into InvMixColumns for decryption,
// matching DEC's Rijndael_T[4..7].
var rijndaelTD = buildRijndaelTable(&rijndaelSD, rijndaelInvMixColumn)

// rijndaelKeyTable is DEC's Rijndael_Key: InvMixColumns applied to the identity
// permutation instead of to the inverse S-box. The decryption schedule is built
// with it.
var rijndaelKeyTable = buildRijndaelKeyTable()

// rijndaelGFMult multiplies a by b in GF(2^8) modulo rijndaelPolynomial.
func rijndaelGFMult(a, b byte) byte {
	product := uint32(0)
	x := uint32(a)

	for shift := range 8 {
		if b>>shift&1 != 0 {
			product ^= x
		}

		x <<= 1
		if x&0x100 != 0 {
			x ^= rijndaelPolynomial
		}
	}

	return lowByte(product)
}

// rijndaelColumn multiplies v by each of coeffs and packs the four products
// into a little-endian word: one column of a MixColumns matrix applied to v.
func rijndaelColumn(v byte, coeffs [4]byte) uint32 {
	var w uint32

	for i, c := range coeffs {
		w |= uint32(rijndaelGFMult(v, c)) << (8 * i)
	}

	return w
}

// rijndaelGFInverse returns the multiplicative inverse of a in GF(2^8) modulo
// rijndaelPolynomial. Zero has no inverse and maps to zero, as the S-box
// definition requires.
func rijndaelGFInverse(a byte) byte {
	var candidate byte

	for range 256 {
		if rijndaelGFMult(a, candidate) == 1 {
			return candidate
		}

		candidate++
	}

	return 0
}

// buildRijndaelSE derives the AES S-box: invert in GF(2^8), then apply the
// affine transform b = s xor rotl(s,1) xor rotl(s,2) xor rotl(s,3) xor
// rotl(s,4) xor 0x63, where rotl rotates the eight bits of the byte.
func buildRijndaelSE() [256]byte {
	var (
		table [256]byte
		x     byte
	)

	for i := range table {
		s := uint32(rijndaelGFInverse(x))
		b := s

		for shift := 1; shift <= 4; shift++ {
			b ^= s<<shift | s>>(8-shift)
		}

		table[i] = lowByte(b) ^ 0x63
		x++
	}

	return table
}

// buildRijndaelSD inverts the S-box permutation.
func buildRijndaelSD() [256]byte {
	var (
		table [256]byte
		index byte
	)

	for _, v := range rijndaelSE {
		table[v] = index
		index++
	}

	return table
}

// buildRijndaelTable folds an S-box into a MixColumns matrix, producing the
// four rotated columns a round consumes.
func buildRijndaelTable(sbox *[256]byte, coeffs [4]byte) [4][256]uint32 {
	var table [4][256]uint32

	for x, s := range sbox {
		w := rijndaelColumn(s, coeffs)
		for j := range 4 {
			table[j][x] = bits.RotateLeft32(w, 8*j)
		}
	}

	return table
}

// buildRijndaelKeyTable builds the InvMixColumns table used to convert an
// encryption round key into an equivalent-inverse-cipher decryption round key.
func buildRijndaelKeyTable() [256]uint32 {
	var (
		table [256]uint32
		x     byte
	)

	for i := range table {
		table[i] = rijndaelColumn(x, rijndaelInvMixColumn)
		x++
	}

	return table
}

// rijndaelRoundCount maps a key length to DEC's round count: 10 up to 16 bytes,
// 12 up to 24, 14 beyond. Unlike DEC, which zero-pads any length, this rejects
// everything but the three standard sizes.
func rijndaelRoundCount(keyLen int) (int, error) {
	switch keyLen {
	case 16:
		return 10, nil
	case 24:
		return 12, nil
	case 32:
		return 14, nil
	default:
		return 0, ErrRijndaelKeySize
	}
}

// rijndaelSubWord applies the S-box to each of the four bytes of w.
func rijndaelSubWord(w uint32) uint32 {
	return uint32(rijndaelSE[lowByte(w)]) |
		uint32(rijndaelSE[lowByte(w>>8)])<<8 |
		uint32(rijndaelSE[lowByte(w>>16)])<<16 |
		uint32(rijndaelSE[lowByte(w>>24)])<<24
}

// rijndaelExpandKey reproduces DEC's BuildEncodeKey. The key is zero-filled to
// 32 bytes and read as eight little-endian words, of which the first nk =
// rounds-6 are live. Each expansion step folds the g-function
// (RotWord + SubWord + Rcon) into K[0] and then chains the remaining live words
// through each other; for 14 rounds DEC chains all eight words first and only
// afterwards mixes SubWord(K[3]) into K[4] and re-chains K[5..7], which is what
// makes the 256-bit schedule differ from AES-256. The expanded words are
// emitted in order until (rounds+1)*4 of them exist.
func rijndaelExpandKey(key []byte, rounds int) []uint32 {
	var buf [32]byte

	copy(buf[:], key)

	var k [8]uint32

	for i := range k {
		k[i] = uint32(buf[4*i]) | uint32(buf[4*i+1])<<8 |
			uint32(buf[4*i+2])<<16 | uint32(buf[4*i+3])<<24
	}

	nk := rounds - 6
	expanded := make([]uint32, 0, (rounds+1)*4)
	total := (rounds + 1) * 4

	emit := func() {
		for i := 0; i < nk && len(expanded) < total; i++ {
			expanded = append(expanded, k[i])
		}
	}

	emit()

	for step := 0; len(expanded) < total; step++ {
		k[0] ^= rijndaelSubWord(bits.RotateLeft32(k[nk-1], -8)) ^ uint32(rijndaelRcon[step])

		if rounds == rijndaelMaxRounds {
			for i := 1; i <= 7; i++ {
				k[i] ^= k[i-1]
			}

			k[4] ^= rijndaelSubWord(k[3])

			for i := 5; i <= 7; i++ {
				k[i] ^= k[i-1]
			}
		} else {
			for i := 1; i < nk; i++ {
				k[i] ^= k[i-1]
			}
		}

		emit()
	}

	return expanded
}

// rijndaelInvMix applies InvMixColumns to one round-key word, as DEC's
// BuildDecodeKey does.
func rijndaelInvMix(w uint32) uint32 {
	return rijndaelKeyTable[lowByte(w)] ^
		bits.RotateLeft32(rijndaelKeyTable[lowByte(w>>8)], 8) ^
		bits.RotateLeft32(rijndaelKeyTable[lowByte(w>>16)], 16) ^
		bits.RotateLeft32(rijndaelKeyTable[lowByte(w>>24)], 24)
}

// newRijndael builds the cipher from a 16-, 24- or 32-byte key, expanding it
// into the encryption round keys and the reversed, InvMixColumns-folded
// decryption round keys.
func newRijndael(key []byte) (*rijndaelCipher, error) {
	rounds, err := rijndaelRoundCount(len(key))
	if err != nil {
		return nil, err
	}

	c := &rijndaelCipher{rounds: rounds}
	expanded := rijndaelExpandKey(key, rounds)

	for r := range rounds + 1 {
		copy(c.enc[r][:], expanded[4*r:4*r+4])

		// The decryption schedule is the encryption one read backwards, with
		// InvMixColumns folded into every key except the outermost two.
		src := expanded[4*(rounds-r) : 4*(rounds-r)+4]
		for i, w := range src {
			if r > 0 && r < rounds {
				w = rijndaelInvMix(w)
			}

			c.dec[r][i] = w
		}
	}

	return c, nil
}

// rijndaelTransform runs the shared round structure: an initial key addition,
// rounds-1 table-driven rounds, then a final round that substitutes and shifts
// without mixing. Encryption and decryption differ only in the schedule, the
// round table, the S-box, and step — the direction in which ShiftRows walks the
// state words, 1 forwards and 3 (that is, -1 modulo 4) backwards.
func rijndaelTransform(dst, src []byte, keys *[rijndaelMaxRounds + 1][4]uint32, rounds int, table *[4][256]uint32, sbox *[256]byte, step int) {
	w := loadWordsLE(src)

	var state [4]uint32

	for i := range state {
		state[i] = w[i] ^ keys[0][i]
	}

	for r := 1; r < rounds; r++ {
		var next [4]uint32

		for j := range next {
			next[j] = table[0][lowByte(state[j])] ^
				table[1][lowByte(state[(j+step)%4]>>8)] ^
				table[2][lowByte(state[(j+2*step)%4]>>16)] ^
				table[3][lowByte(state[(j+3*step)%4]>>24)] ^
				keys[r][j]
		}

		state = next
	}

	var out [4]uint32

	for j := range out {
		out[j] = uint32(sbox[lowByte(state[j])]) |
			uint32(sbox[lowByte(state[(j+step)%4]>>8)])<<8 |
			uint32(sbox[lowByte(state[(j+2*step)%4]>>16)])<<16 |
			uint32(sbox[lowByte(state[(j+3*step)%4]>>24)])<<24
		out[j] ^= keys[rounds][j]
	}

	storeWordsLE(dst, out)
}
