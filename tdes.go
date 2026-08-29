package absdb

import (
	"crypto/cipher"
	"crypto/des" //nolint:gosec // DES is required to read files written with it.
	"errors"
	"fmt"
)

// Triple DES as ComponentAce's ABSCipher.pas implements it.
//
// DEC 3.0 Part I is Copyright (c) Hagen Reddmann, on the terms "freeware, but
// this Copyright must be included". No DEC code is used here: what follows is
// an independent Go implementation. What is owed to DEC is the knowledge of
// where it deviates and the self-test vector that pins the deviation, and the
// notice is carried for those. See NOTICE.
//
// ABSCipher is a fork of Delphi Encryption Compendium (DEC) 3.0, and the
// algorithm behind TABSCryptoAlgorithm value 3 ("DES-Triple") is DEC's
// TCipher_3TDES — not TCipher_3DES. The two share a key schedule but not a
// block size: 3DES has the usual 8-byte block, while 3TDES declares a 24-byte
// one and runs each EDE stage over all three 8-byte sub-blocks, exchanging
// words between the stages so that the sub-blocks are not independent. This
// was verified against testdata/Employees-DES_Triple.abs; see
// TestTripleDESFixtureControlBlock.
//
// The key is the 16-byte RIPEMD-128 digest of the password, zero-extended to
// the 24-byte KeySize by DEC's Init (which moves Size bytes into a zeroed
// buffer). The third DES key is therefore always eight zero bytes, so the
// construction degenerates to two-key EDE — but as (K1, K2, 0), not the
// (K1, K2, K1) that a plain 3DES reading of the format would suggest.
//
// DEC's inter-stage word exchange contains a typo that must be reproduced.
// The Pascal reads, for each of the two exchanges:
//
//	T := PIntArray(Data)[3]; PIntArray(Data)[3] := PIntArray(Data)[4]; PIntArray(Data)[3] := T;
//
// It assigns to word 3 twice and never to word 4, so that half of the exchange
// is a no-op and only words 1 and 2 actually change places. This is deliberate
// bug-for-bug fidelity, exactly like the shr/shl typo in DEC's Twofish key
// schedule documented in twofish.go: "correcting" the swap makes real .abs
// files stop decrypting. TestTripleDESSwapTypoIsLoadBearing pins it.
//
// TestDECTripleDESSelfTest anchors the whole construction against DEC's own
// published self-test vector for TCipher_3TDES.
const tripleDESBlockSize = 24

// tripleDESKeySize is the KeySize DEC declares for TCipher_3TDES: three 8-byte
// DES keys.
const tripleDESKeySize = 24

// tripleDESDerivedKeySize is the key length the .abs key derivation actually
// supplies: the full RIPEMD-128 digest, which newTripleDES zero-extends to
// tripleDESKeySize.
const tripleDESDerivedKeySize = 16

// desBlockSize is the block size of the underlying DES primitive, and so the
// size of one of the three sub-blocks a 3TDES block is made of.
const desBlockSize = 8

// ErrTripleDESKeySize indicates a key length this cipher does not accept.
var ErrTripleDESKeySize = errors.New("absdb: 3TDES key must be 16 or 24 bytes")

// tripleDESCipher implements cipher.Block for DEC's TCipher_3TDES.
//
// keys holds the three DES key schedules K1, K2 and K3 in that order. Both
// directions of each are available from a single crypto/des cipher.Block, so
// the six schedules DEC's Init builds are three here.
type tripleDESCipher struct {
	keys [3]cipher.Block
}

// newTripleDES builds the cipher from either the 16-byte RIPEMD-128 digest the
// .abs key derivation produces, which is zero-extended to 24 bytes exactly as
// DEC's TCipher_3DES.Init does, or from a full 24-byte key.
func newTripleDES(key []byte) (*tripleDESCipher, error) {
	if len(key) != tripleDESDerivedKeySize && len(key) != tripleDESKeySize {
		return nil, ErrTripleDESKeySize
	}

	full := make([]byte, tripleDESKeySize)
	copy(full, key)

	c := new(tripleDESCipher)

	for i := range c.keys {
		block, err := des.NewCipher(full[i*desBlockSize : (i+1)*desBlockSize]) //nolint:gosec // reading legacy files
		if err != nil {
			return nil, fmt.Errorf("absdb: 3tdes: %w", err)
		}

		c.keys[i] = block
	}

	return c, nil
}

// BlockSize returns 24: DEC's TCipher_3TDES block, not DES's.
func (c *tripleDESCipher) BlockSize() int { return tripleDESBlockSize }

// Encrypt encrypts one 24-byte block. dst and src may be the same slice.
//
// The three EDE stages are E_K1, D_K2, E_K3, with DEC's word exchange after
// the first two.
func (c *tripleDESCipher) Encrypt(dst, src []byte) {
	var buf [tripleDESBlockSize]byte

	copy(buf[:], src)

	c.stage(buf[:], c.keys[0], true)
	tripleDESSwap(buf[:])
	c.stage(buf[:], c.keys[1], false)
	tripleDESSwap(buf[:])
	c.stage(buf[:], c.keys[2], true)

	copy(dst, buf[:])
}

// Decrypt decrypts one 24-byte block. dst and src may be the same slice.
//
// It mirrors Encrypt: D_K3, E_K2, D_K1, with the same word exchange between
// the stages. The exchange is its own inverse, so no separate undo is needed.
func (c *tripleDESCipher) Decrypt(dst, src []byte) {
	var buf [tripleDESBlockSize]byte

	copy(buf[:], src)

	c.stage(buf[:], c.keys[2], false)
	tripleDESSwap(buf[:])
	c.stage(buf[:], c.keys[1], true)
	tripleDESSwap(buf[:])
	c.stage(buf[:], c.keys[0], false)

	copy(dst, buf[:])
}

// stage applies one DES direction to each of the three 8-byte sub-blocks of
// buf in place. encrypt selects the direction, which is what DEC encodes in
// the MakeKey flag of each of the six schedules its Init lays out.
func (c *tripleDESCipher) stage(buf []byte, block cipher.Block, encrypt bool) {
	for i := 0; i < tripleDESBlockSize; i += desBlockSize {
		sub := buf[i : i+desBlockSize]
		if encrypt {
			block.Encrypt(sub, sub)
		} else {
			block.Decrypt(sub, sub)
		}
	}
}

// tripleDESSwap performs DEC's inter-stage word exchange on a 24-byte block.
//
// Words are little-endian 32-bit, so this exchanges bytes [4:8] with [8:12] —
// the tail of sub-block 0 with the head of sub-block 1. DEC's source also
// appears to exchange words 3 and 4, but that statement assigns to word 3
// twice and is a no-op; see the file's opening comment. Reproducing the typo
// is required, so this function must keep touching words 1 and 2 only.
func tripleDESSwap(buf []byte) {
	var tmp [4]byte

	copy(tmp[:], buf[4:8])
	copy(buf[4:8], buf[8:12])
	copy(buf[8:12], tmp[:])
}
