package absdb

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// decSelfTestPlain is DEC's GetTestVector: the 32-byte plaintext every DEC
// cipher self-test encrypts.
var decSelfTestPlain = []byte{
	0x30, 0x44, 0xED, 0x6E, 0x45, 0xA4, 0x96, 0xF5,
	0xF6, 0x35, 0xA2, 0xEB, 0x3D, 0x1A, 0x5D, 0xD6,
	0xCB, 0x1D, 0x09, 0x82, 0x2D, 0xBD, 0xF5, 0x60,
	0xC2, 0xB8, 0x58, 0xA1, 0x91, 0xF9, 0x81, 0xB1,
}

// encryptCTSNoIV reproduces DEC's TCipher.SelfTest: cmCTS with IVector = nil,
// so InitEnd seeds the feedback register with E(0xFF...) rather than 0xFF...
// itself. It is only used to check cipher implementations against DEC's own
// published self-test vectors; real .abs pages are decrypted by decryptCTS,
// which gets an explicit all-0xFF vector.
func encryptCTSNoIV(block interface {
	BlockSize() int
	Encrypt(dst, src []byte)
}, src []byte,
) []byte {
	bs := block.BlockSize()

	feedback := bytes.Repeat([]byte{ivFillByte}, bs)
	block.Encrypt(feedback, feedback)

	dst := make([]byte, len(src))
	tmp := make([]byte, bs)

	for i := 0; i+bs <= len(src); i += bs {
		for j := range bs {
			tmp[j] = src[i+j] ^ feedback[j]
		}

		block.Encrypt(tmp, tmp)
		copy(dst[i:], tmp)

		for j := range bs {
			feedback[j] = tmp[j] ^ feedback[j]
		}
	}

	return dst
}

// TestDECTwofishSelfTest is the anchor for the whole Twofish implementation: it
// reproduces the self-test vector compiled into DEC's own TCipher_Twofish, so
// the deviation from reference Twofish documented in twofish.go is verified
// against the reference implementation rather than assumed.
//
// DEC's SelfTest keys the cipher with the class name, "TCipher_Twofish", which
// is 15 bytes and is zero-padded to 16 by SetupKey's Move into a zeroed buffer.
func TestDECTwofishSelfTest(t *testing.T) {
	key := append([]byte("TCipher_Twofish"), 0)

	c, err := newTwofish(key)
	if err != nil {
		t.Fatalf("newTwofish: %v", err)
	}

	want, err := hex.DecodeString(
		"a5535703ef3348799f22b4549705841987bd831c4dae1213607c7cd198450219",
	)
	if err != nil {
		t.Fatal(err)
	}

	got := encryptCTSNoIV(c, decSelfTestPlain)
	if !bytes.Equal(got, want) {
		t.Errorf("DEC self-test:\n got %x\nwant %x", got, want)
	}
}

// TestTwofishNotReference records that this cipher is deliberately incompatible
// with reference Twofish. The reference answer for an all-zero 128-bit key and
// an all-zero block is 9f589f5cf6122c32b6bfec2f2ae8c35a; DEC's key-schedule
// typo makes it something else. If this ever starts matching, the deviation has
// been "fixed" and every encrypted .abs file will stop decrypting.
func TestTwofishNotReference(t *testing.T) {
	c, err := newTwofish(make([]byte, 16))
	if err != nil {
		t.Fatalf("newTwofish: %v", err)
	}

	out := make([]byte, twofishBlockSize)
	c.Encrypt(out, make([]byte, twofishBlockSize))

	const reference = "9f589f5cf6122c32b6bfec2f2ae8c35a"

	if hex.EncodeToString(out) == reference {
		t.Error("cipher matches reference Twofish; DEC's key-schedule deviation was lost")
	}
}

// TestTwofishRoundTrip checks Decrypt is the exact inverse of Encrypt for both
// supported key lengths, including in-place operation.
func TestTwofishRoundTrip(t *testing.T) {
	for _, keyLen := range []int{16, 32} {
		key := make([]byte, keyLen)
		for i := range key {
			key[i] = byte(i * 7)
		}

		c, err := newTwofish(key)
		if err != nil {
			t.Fatalf("newTwofish(%d): %v", keyLen, err)
		}

		if c.BlockSize() != twofishBlockSize {
			t.Errorf("BlockSize = %d, want %d", c.BlockSize(), twofishBlockSize)
		}

		plain := make([]byte, twofishBlockSize)
		for i := range plain {
			plain[i] = byte(0xA0 + i)
		}

		cipherText := make([]byte, twofishBlockSize)
		c.Encrypt(cipherText, plain)

		if bytes.Equal(cipherText, plain) {
			t.Errorf("keyLen %d: ciphertext equals plaintext", keyLen)
		}

		back := make([]byte, twofishBlockSize)
		c.Decrypt(back, cipherText)

		if !bytes.Equal(back, plain) {
			t.Errorf("keyLen %d: round trip gave %x, want %x", keyLen, back, plain)
		}

		// dst may alias src.
		inPlace := append([]byte(nil), plain...)
		c.Encrypt(inPlace, inPlace)
		c.Decrypt(inPlace, inPlace)

		if !bytes.Equal(inPlace, plain) {
			t.Errorf("keyLen %d: in-place round trip gave %x, want %x", keyLen, inPlace, plain)
		}
	}
}

// TestTwofishKeySize pins the supported key lengths. 24-byte keys are rejected
// on purpose: deriveKey only ever produces a 16- or 32-byte digest, so the
// 192-bit schedule would be unreachable and untestable code.
func TestTwofishKeySize(t *testing.T) {
	for _, n := range []int{0, 1, 8, 15, 17, 24, 31, 33, 64} {
		if _, err := newTwofish(make([]byte, n)); !errors.Is(err, ErrTwofishKeySize) {
			t.Errorf("newTwofish(%d bytes) error = %v, want ErrTwofishKeySize", n, err)
		}
	}

	for _, n := range []int{16, 32} {
		if _, err := newTwofish(make([]byte, n)); err != nil {
			t.Errorf("newTwofish(%d bytes) = %v, want success", n, err)
		}
	}
}

// TestTwofishTablesSelfConsistent checks the derived MDS table against a direct
// recomputation, so a corrupted q permutation cannot silently pass.
func TestTwofishTablesSelfConsistent(t *testing.T) {
	finalQ := [4]int{1, 0, 1, 0}

	for col := range 4 {
		for z := range 256 {
			want := twofishMDSColumn(twofishQ[finalQ[col]][z], col)
			if twofishMDS[col][z] != want {
				t.Fatalf("twofishMDS[%d][%d] = %#08x, want %#08x",
					col, z, twofishMDS[col][z], want)
			}
		}
	}

	// The q permutations must be permutations.
	for q := range 2 {
		var seen [256]bool

		for _, v := range twofishQ[q] {
			if seen[v] {
				t.Fatalf("q%d is not a permutation: %#02x repeats", q, v)
			}

			seen[v] = true
		}
	}
}
