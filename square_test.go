package absdb

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/bits"
	"testing"
)

// TestDECSquareSelfTest is the anchor for the whole Square implementation: it
// reproduces the self-test vector compiled into DEC's own TCipher_Square, so
// the port is verified against the reference implementation rather than
// assumed.
//
// DEC's SelfTest keys the cipher with the class name, "TCipher_Square", which
// is 14 bytes and is zero-padded to 16 by SetupKey's Move into a zeroed buffer.
//
// The expected ciphertext was recovered twice independently: from
// TCipher_Square.TestVector in DEC 3.0's Cipher.pas, and from VMT slot 10 of
// TCipher_Square in <sdk>/Utils/Bin/DBImportExport.exe at file offset
// 0xD78B6. It is not in the shipped ABSCipher.dcu because the Rio-era ABSCipher
// replaced DEC's TestVector stubs with ones that raise "not implemented".
func TestDECSquareSelfTest(t *testing.T) {
	key := append([]byte("TCipher_Square"), 0, 0)

	c, err := newSquare(key)
	if err != nil {
		t.Fatalf("newSquare: %v", err)
	}

	want, err := hex.DecodeString(
		"439ca6c467e82e472295668506396ac9182120f74436f1617d1490b1a96856c7",
	)
	if err != nil {
		t.Fatal(err)
	}

	got := encryptCTSNoIV(c, decSelfTestPlain)
	if !bytes.Equal(got, want) {
		t.Errorf("DEC self-test:\n got %x\nwant %x", got, want)
	}
}

// TestSquareRoundTrip checks Decrypt is the exact inverse of Encrypt, including
// in-place operation, and that the block size is what the format expects.
func TestSquareRoundTrip(t *testing.T) {
	key := make([]byte, squareKeySize)
	for i := range key {
		key[i] = byte(i * 7)
	}

	c, err := newSquare(key)
	if err != nil {
		t.Fatalf("newSquare: %v", err)
	}

	if c.BlockSize() != squareBlockSize {
		t.Errorf("BlockSize = %d, want %d", c.BlockSize(), squareBlockSize)
	}

	plain := make([]byte, squareBlockSize)
	for i := range plain {
		plain[i] = byte(0xA0 + i)
	}

	cipherText := make([]byte, squareBlockSize)
	c.Encrypt(cipherText, plain)

	if bytes.Equal(cipherText, plain) {
		t.Error("ciphertext equals plaintext")
	}

	back := make([]byte, squareBlockSize)
	c.Decrypt(back, cipherText)

	if !bytes.Equal(back, plain) {
		t.Errorf("round trip gave %x, want %x", back, plain)
	}

	// dst may alias src.
	inPlace := append([]byte(nil), plain...)
	c.Encrypt(inPlace, inPlace)

	if !bytes.Equal(inPlace, cipherText) {
		t.Errorf("in-place encrypt gave %x, want %x", inPlace, cipherText)
	}

	c.Decrypt(inPlace, inPlace)

	if !bytes.Equal(inPlace, plain) {
		t.Errorf("in-place round trip gave %x, want %x", inPlace, plain)
	}
}

// TestSquareKeySize pins the single supported key length. Square is defined
// only for a 128-bit key, and deriveKey never produces anything else for it.
func TestSquareKeySize(t *testing.T) {
	for _, n := range []int{0, 1, 8, 15, 17, 24, 31, 32, 33, 64} {
		if _, err := newSquare(make([]byte, n)); !errors.Is(err, ErrSquareKeySize) {
			t.Errorf("newSquare(%d bytes) error = %v, want ErrSquareKeySize", n, err)
		}
	}

	if _, err := newSquare(make([]byte, squareKeySize)); err != nil {
		t.Errorf("newSquare(%d bytes) = %v, want success", squareKeySize, err)
	}
}

// TestSquareTablesSelfConsistent checks every derived table against the one
// literal it is built from, so a corrupted S-box or a wrong field polynomial
// cannot slip through silently.
func TestSquareTablesSelfConsistent(t *testing.T) {
	// The S-box must be a permutation, and squareSD its exact inverse.
	var seen [256]bool

	for _, v := range squareSE {
		if seen[v] {
			t.Fatalf("squareSE is not a permutation: %#02x repeats", v)
		}

		seen[v] = true
	}

	var index byte

	for _, v := range squareSE {
		if squareSD[v] != index {
			t.Fatalf("squareSD[%#02x] = %#02x, want %#02x", v, squareSD[v], index)
		}

		index++
	}

	// theta times its derived inverse must be the identity over the field.
	for i := range 4 {
		for j := range 4 {
			var sum byte

			for k := range 4 {
				sum ^= squareGFMult(squareDiffusion[(k-i+4)%4], squareInvDiffusion[(j-k+4)%4])
			}

			want := byte(0)
			if i == j {
				want = 1
			}

			if sum != want {
				t.Fatalf("theta*theta^-1 [%d][%d] = %#02x, want %#02x", i, j, sum, want)
			}
		}
	}

	// The inverse coefficients ABSCipher.dcu's decryption table is built from.
	if squareInvDiffusion != [4]byte{0x0E, 0x09, 0x0D, 0x0B} {
		t.Errorf("squareInvDiffusion = %#02x, want {0x0E, 0x09, 0x0D, 0x0B}", squareInvDiffusion)
	}

	// Columns 1..3 of each round table are rotations of column 0.
	tables := map[string]*[4][256]uint32{"squareTE": &squareTE, "squareTD": &squareTD}

	for name, table := range tables {
		for j := 1; j < 4; j++ {
			for x := range 256 {
				want := bits.RotateLeft32(table[0][x], 8*j)
				if table[j][x] != want {
					t.Fatalf("%s[%d][%d] = %#08x, want %#08x", name, j, x, table[j][x], want)
				}
			}
		}
	}

	// PHI is the same construction as squareTE with the identity S-box.
	var v byte

	for i := range squarePHI {
		want := squareColumn(v, squareDiffusion)
		if squarePHI[i] != want {
			t.Fatalf("squarePHI[%d] = %#08x, want %#08x", i, squarePHI[i], want)
		}

		v++
	}
}
