package deccrypto

import (
	"bytes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"testing"
)

const tripleDESSelfTestKey = "TCipher_3TDES"

// decSelfTestVector3TDES is the self-test vector compiled into DEC's
// TCipher_3TDES. It was read out of VMT slot 10 in
// <sdk>/Utils/Bin/DBImportExport.exe at file offset 0xD6596 and corroborated
// by TCipher_3TDES.TestVector in DEC's Cipher1.pas.
const decSelfTestVector3TDES = "0b12e48bd9cd08bfcaae3e5ff6fe13cd" +
	"3f706ecd53563f5a800f1b1efb9a5796"

// encryptCTSPartial reproduces DEC's cmCTS encryption with IVector = nil, the
// mode TCipher.SelfTest runs in: InitEnd seeds the feedback register with
// E(0xFF...) rather than 0xFF... itself, whole blocks are C = E(P XOR F) with
// F' = C XOR F, and a trailing partial block is C = P XOR E(F).
//
// twofish_test.go's encryptCTSNoIV drops the trailing partial block, which is
// harmless for a 16-byte block over DEC's 32-byte test vector but not for the
// 24-byte block of TCipher_3TDES, where 32 bytes is one full block plus an
// 8-byte remainder. Hence this second helper rather than a change over there.
func encryptCTSPartial(block cipher.Block, src []byte) []byte {
	bs := block.BlockSize()

	feedback := bytes.Repeat([]byte{0xFF}, bs)
	block.Encrypt(feedback, feedback)

	dst := make([]byte, len(src))
	tmp := make([]byte, bs)
	full := len(src) - len(src)%bs

	for i := 0; i < full; i += bs {
		for j := range bs {
			tmp[j] = src[i+j] ^ feedback[j]
		}

		block.Encrypt(tmp, tmp)
		copy(dst[i:], tmp)

		for j := range bs {
			feedback[j] ^= tmp[j]
		}
	}

	if rest := len(src) - full; rest > 0 {
		block.Encrypt(tmp, feedback)

		for j := range rest {
			dst[full+j] = src[full+j] ^ tmp[j]
		}
	}

	return dst
}

// TestDECTripleDESSelfTest reproduces the self-test vector compiled into DEC's
// own TCipher_3TDES, so the port is checked against DEC rather than only
// against one .abs file.
//
// DEC's SelfTest keys the cipher with the class name, "TCipher_3TDES", which
// is 13 bytes and is zero-padded to the 24-byte KeySize by Init's Move into a
// zeroed buffer, and encrypts the 32-byte GetTestVector plaintext in cmCTS
// with IVector = nil.
func TestDECTripleDESSelfTest(t *testing.T) {
	key := make([]byte, tripleDESKeySize)
	copy(key, tripleDESSelfTestKey)

	c, err := NewTripleDES(key)
	if err != nil {
		t.Fatalf("NewTripleDES: %v", err)
	}

	want, err := hex.DecodeString(decSelfTestVector3TDES)
	if err != nil {
		t.Fatal(err)
	}

	got := encryptCTSPartial(c, decSelfTestPlain)
	if !bytes.Equal(got, want) {
		t.Errorf("DEC self-test:\n got %x\nwant %x", got, want)
	}
}

// TestTripleDESRoundTrip checks Decrypt is the exact inverse of Encrypt, both
// out of place and in place, for both accepted key lengths.
func TestTripleDESRoundTrip(t *testing.T) {
	for _, keyLen := range []int{tripleDESDerivedKeySize, tripleDESKeySize} {
		key := make([]byte, keyLen)
		for i := range key {
			key[i] = byte(i*7 + 3)
		}

		c, err := NewTripleDES(key)
		if err != nil {
			t.Fatalf("NewTripleDES(%d): %v", keyLen, err)
		}

		if c.BlockSize() != tripleDESBlockSize {
			t.Errorf("BlockSize = %d, want %d", c.BlockSize(), tripleDESBlockSize)
		}

		plain := make([]byte, tripleDESBlockSize)
		for i := range plain {
			plain[i] = byte(0xA0 + i)
		}

		cipherText := make([]byte, tripleDESBlockSize)
		c.Encrypt(cipherText, plain)

		if bytes.Equal(cipherText, plain) {
			t.Errorf("keyLen %d: ciphertext equals plaintext", keyLen)
		}

		back := make([]byte, tripleDESBlockSize)
		c.Decrypt(back, cipherText)

		if !bytes.Equal(back, plain) {
			t.Errorf("keyLen %d: round trip gave %x, want %x", keyLen, back, plain)
		}

		inPlace := append([]byte(nil), plain...)
		c.Encrypt(inPlace, inPlace)
		c.Decrypt(inPlace, inPlace)

		if !bytes.Equal(inPlace, plain) {
			t.Errorf("keyLen %d: in-place round trip gave %x, want %x",
				keyLen, inPlace, plain)
		}
	}
}

// TestTripleDESKeySize pins the accepted key lengths: the 16-byte RIPEMD-128
// digest the .abs derivation supplies, and the full 24-byte DEC KeySize the
// self-test needs.
func TestTripleDESKeySize(t *testing.T) {
	for _, n := range []int{0, 1, 8, 15, 17, 23, 25, 32, 48} {
		if _, err := NewTripleDES(make([]byte, n)); !errors.Is(err, ErrTripleDESKeySize) {
			t.Errorf("NewTripleDES(%d bytes) error = %v, want ErrTripleDESKeySize", n, err)
		}
	}

	for _, n := range []int{tripleDESDerivedKeySize, tripleDESKeySize} {
		if _, err := NewTripleDES(make([]byte, n)); err != nil {
			t.Errorf("NewTripleDES(%d bytes) = %v, want success", n, err)
		}
	}
}
