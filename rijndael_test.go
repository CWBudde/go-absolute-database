package absdb

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"testing"
)

// TestDECRijndaelSelfTest is the anchor for the whole Rijndael implementation:
// it reproduces the self-test vector compiled into DEC's own
// TCipher_Rijndael, recovered from VMT slot 10 of that class in
// legacy/Utils/Bin/DBImportExport.exe and corroborated by
// TCipher_Rijndael.TestVector in DEC's Cipher1.pas.
//
// DEC's SelfTest keys the cipher with the class name, "TCipher_Rijndael",
// which is exactly 16 bytes, so this vector exercises the 10-round path.
func TestDECRijndaelSelfTest(t *testing.T) {
	c, err := newRijndael([]byte("TCipher_Rijndael"))
	if err != nil {
		t.Fatalf("newRijndael: %v", err)
	}

	want, err := hex.DecodeString(
		"946d2b5ee0ad1b5ca523a513958b3d2d9387f3374551f6589be7901b3687f9a9",
	)
	if err != nil {
		t.Fatal(err)
	}

	got := encryptCTSNoIV(c, decSelfTestPlain)
	if !bytes.Equal(got, want) {
		t.Errorf("DEC self-test:\n got %x\nwant %x", got, want)
	}
}

// TestRijndaelSBox checks the derived S-box against its defining properties and
// against published spot values, so that a mistake in buildRijndaelSE is
// reported here rather than only as an opaque self-test mismatch.
func TestRijndaelSBox(t *testing.T) {
	var seen [256]bool

	for _, v := range rijndaelSE {
		if seen[v] {
			t.Fatalf("S-box is not a permutation: %#02x occurs twice", v)
		}

		seen[v] = true
	}

	spot := map[byte]byte{0x00: 0x63, 0x01: 0x7c, 0x53: 0xed, 0x7f: 0xd2, 0xff: 0x16}
	for in, want := range spot {
		if got := rijndaelSE[in]; got != want {
			t.Errorf("S-box[%#02x] = %#02x, want %#02x", in, got, want)
		}
	}

	for i := range 256 {
		if rijndaelSD[rijndaelSE[i]] != byte(i) {
			t.Fatalf("inverse S-box does not invert at %#02x", i)
		}
	}
}

// TestRijndaelMatchesAES checks the two key lengths where DEC's schedule and
// the AES key schedule provably coincide. Agreement with crypto/aes on random
// blocks in both directions is the strongest available check on the round
// function — SubBytes, ShiftRows, MixColumns and their inverses — and on the
// non-deviating part of the expansion.
func TestRijndaelMatchesAES(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for _, keyLen := range []int{16, 24} {
		key := make([]byte, keyLen)
		if _, err := rng.Read(key); err != nil {
			t.Fatal(err)
		}

		mine, err := newRijndael(key)
		if err != nil {
			t.Fatalf("newRijndael(%d): %v", keyLen, err)
		}

		ref, err := aes.NewCipher(key)
		if err != nil {
			t.Fatalf("aes.NewCipher(%d): %v", keyLen, err)
		}

		for range 64 {
			block := make([]byte, rijndaelBlockSize)
			if _, err := rng.Read(block); err != nil {
				t.Fatal(err)
			}

			gotEnc := make([]byte, rijndaelBlockSize)
			wantEnc := make([]byte, rijndaelBlockSize)

			mine.Encrypt(gotEnc, block)
			ref.Encrypt(wantEnc, block)

			if !bytes.Equal(gotEnc, wantEnc) {
				t.Fatalf("key %d bytes, Encrypt(%x):\n got %x\nwant %x", keyLen, block, gotEnc, wantEnc)
			}

			gotDec := make([]byte, rijndaelBlockSize)
			wantDec := make([]byte, rijndaelBlockSize)

			mine.Decrypt(gotDec, block)
			ref.Decrypt(wantDec, block)

			if !bytes.Equal(gotDec, wantDec) {
				t.Fatalf("key %d bytes, Decrypt(%x):\n got %x\nwant %x", keyLen, block, gotDec, wantDec)
			}
		}
	}
}

// TestRijndael256NotAES records that the 256-bit key path is deliberately
// incompatible with AES-256: DEC's schedule chains all eight key words before
// mixing SubWord(K[3]) into K[4], which AES does not do. If this ever starts
// matching, the deviation has been "fixed" and every Rijndael-256 .abs file
// will stop decrypting.
func TestRijndael256NotAES(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	mine, err := newRijndael(key)
	if err != nil {
		t.Fatalf("newRijndael: %v", err)
	}

	ref, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}

	plain := make([]byte, rijndaelBlockSize)
	got := make([]byte, rijndaelBlockSize)
	want := make([]byte, rijndaelBlockSize)

	mine.Encrypt(got, plain)
	ref.Encrypt(want, plain)

	if bytes.Equal(got, want) {
		t.Error("cipher matches AES-256; DEC's key-schedule deviation was lost")
	}
}

// TestRijndael256ControlBlockCRC is the end-to-end proof that DEC's deviating
// 256-bit schedule is the one real files were written with: it decrypts the
// ControlBlock of a genuine Rijndael-256 database and checks the result against
// the CRC the file itself stores.
func TestRijndael256ControlBlockCRC(t *testing.T) {
	path := requireFixture(t, "Employees-Rijndael_256.abs")

	page0, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	ch := parseCryptoHeader(page0)
	if ch == nil {
		t.Fatal("parseCryptoHeader returned nil")
	}

	if ch.Algorithm != CryptoRijndael256 {
		t.Fatalf("Algorithm = %v, want %v", ch.Algorithm, CryptoRijndael256)
	}

	const wantCRC = 0x3264440f

	if ch.ControlCRC != wantCRC {
		t.Fatalf("ControlCRC = %#08x, want %#08x", ch.ControlCRC, uint32(wantCRC))
	}

	key := ripemd256Sum([]byte("Bla"))

	c, err := newRijndael(key[:32])
	if err != nil {
		t.Fatalf("newRijndael: %v", err)
	}

	control := make([]byte, controlBlockSize)
	decryptCTS(c, control, ch.ControlBlock[:])

	if got := absCRC32(control); got != ch.ControlCRC {
		t.Errorf("absCRC32(ControlBlock) = %#08x, want %#08x", got, ch.ControlCRC)
	}
}

// TestRijndaelRoundTrip checks Decrypt is the exact inverse of Encrypt for all
// three key lengths, both out of place and in place.
func TestRijndaelRoundTrip(t *testing.T) {
	for _, keyLen := range []int{16, 24, 32} {
		key := make([]byte, keyLen)
		for i := range key {
			key[i] = byte(i * 11)
		}

		c, err := newRijndael(key)
		if err != nil {
			t.Fatalf("newRijndael(%d): %v", keyLen, err)
		}

		if c.BlockSize() != rijndaelBlockSize {
			t.Errorf("BlockSize = %d, want %d", c.BlockSize(), rijndaelBlockSize)
		}

		plain := make([]byte, rijndaelBlockSize)
		for i := range plain {
			plain[i] = byte(0xA0 + i)
		}

		cipherText := make([]byte, rijndaelBlockSize)
		c.Encrypt(cipherText, plain)

		if bytes.Equal(cipherText, plain) {
			t.Errorf("key %d bytes: Encrypt was a no-op", keyLen)
		}

		back := make([]byte, rijndaelBlockSize)
		c.Decrypt(back, cipherText)

		if !bytes.Equal(back, plain) {
			t.Errorf("key %d bytes: round trip:\n got %x\nwant %x", keyLen, back, plain)
		}

		// In place, both directions.
		inPlace := append([]byte(nil), plain...)
		c.Encrypt(inPlace, inPlace)

		if !bytes.Equal(inPlace, cipherText) {
			t.Errorf("key %d bytes: in-place Encrypt:\n got %x\nwant %x", keyLen, inPlace, cipherText)
		}

		c.Decrypt(inPlace, inPlace)

		if !bytes.Equal(inPlace, plain) {
			t.Errorf("key %d bytes: in-place Decrypt:\n got %x\nwant %x", keyLen, inPlace, plain)
		}
	}
}

// TestRijndaelKeySizeRejected checks that only the three standard key lengths
// are accepted.
func TestRijndaelKeySizeRejected(t *testing.T) {
	for _, keyLen := range []int{0, 1, 8, 15, 17, 23, 25, 31, 33, 64} {
		if _, err := newRijndael(make([]byte, keyLen)); !errors.Is(err, ErrRijndaelKeySize) {
			t.Errorf("newRijndael(%d bytes) error = %v, want ErrRijndaelKeySize", keyLen, err)
		}
	}
}
