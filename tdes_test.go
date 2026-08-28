package absdb

import (
	"bytes"
	"crypto/cipher"
	"crypto/des"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// tripleDESFixture is the real .abs file that pins this cipher, together with
// its password and the CRC of its decrypted ControlBlock.
const (
	tripleDESFixture     = "Employees-DES_Triple.abs"
	tripleDESPassword    = "Bla"
	tripleDESControlCRC  = 0xd1a0c93f
	tripleDESSelfTestKey = "TCipher_3TDES"
)

// decSelfTestVector3TDES is the self-test vector compiled into DEC's
// TCipher_3TDES. It was read out of VMT slot 10 in
// legacy/Utils/Bin/DBImportExport.exe at file offset 0xD6596 and corroborated
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

	feedback := bytes.Repeat([]byte{ivFillByte}, bs)
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

// tripleDESVariant is an independent second implementation of DEC's
// TCipher_3TDES, written straight from the Pascal, with the inter-stage word
// exchange parameterised.
//
// fixSwap = false reproduces DEC as shipped: only words 1 and 2 change places,
// because the statement that looks like it exchanges words 3 and 4 assigns to
// word 3 twice. fixSwap = true is the "corrected" cipher nobody should ever
// ship — it exists solely so a test can show that correcting the typo breaks
// real files. It is a cipher.Block so that decryptCTS can be run over a real
// ControlBlock with it.
type tripleDESVariant struct {
	keys    [3]cipher.Block
	fixSwap bool
}

// newTripleDESVariant builds the reference implementation from a key of the
// same lengths newTripleDES accepts.
func newTripleDESVariant(t *testing.T, key []byte, fixSwap bool) *tripleDESVariant {
	t.Helper()

	full := make([]byte, tripleDESKeySize)
	copy(full, key)

	v := &tripleDESVariant{fixSwap: fixSwap}

	for i := range v.keys {
		b, err := des.NewCipher(full[i*desBlockSize : (i+1)*desBlockSize])
		if err != nil {
			t.Fatalf("des.NewCipher: %v", err)
		}

		v.keys[i] = b
	}

	return v
}

func (v *tripleDESVariant) BlockSize() int { return tripleDESBlockSize }

// Encrypt runs DEC's Encode: E_K1, swap, D_K2, swap, E_K3.
func (v *tripleDESVariant) Encrypt(dst, src []byte) {
	v.run(dst, src, [3]bool{true, false, true}, [3]int{0, 1, 2})
}

// Decrypt runs DEC's Decode: D_K3, swap, E_K2, swap, D_K1.
func (v *tripleDESVariant) Decrypt(dst, src []byte) {
	v.run(dst, src, [3]bool{false, true, false}, [3]int{2, 1, 0})
}

// run applies the three EDE stages named by keyOrder in the directions given
// by encrypt, exchanging words between consecutive stages.
func (v *tripleDESVariant) run(dst, src []byte, encrypt [3]bool, keyOrder [3]int) {
	buf := make([]byte, tripleDESBlockSize)
	copy(buf, src)

	for stage := range 3 {
		b := v.keys[keyOrder[stage]]

		for i := 0; i < tripleDESBlockSize; i += desBlockSize {
			if encrypt[stage] {
				b.Encrypt(buf[i:i+desBlockSize], buf[i:i+desBlockSize])
			} else {
				b.Decrypt(buf[i:i+desBlockSize], buf[i:i+desBlockSize])
			}
		}

		if stage == 2 {
			break
		}

		v.swap(buf)
	}

	copy(dst, buf)
}

// swap performs the inter-stage word exchange, optionally including the pair
// DEC's typo leaves untouched.
func (v *tripleDESVariant) swap(buf []byte) {
	var tmp [4]byte

	copy(tmp[:], buf[4:8])
	copy(buf[4:8], buf[8:12])
	copy(buf[8:12], tmp[:])

	if v.fixSwap {
		copy(tmp[:], buf[12:16])
		copy(buf[12:16], buf[16:20])
		copy(buf[16:20], tmp[:])
	}
}

// tripleDESFixtureHeader parses the crypto header of the DES-Triple fixture,
// skipping when testdata/ is not present.
func tripleDESFixtureHeader(t *testing.T) *CryptoHeader {
	t.Helper()

	data, err := os.ReadFile(requireFixture(t, tripleDESFixture))
	if err != nil {
		t.Fatal(err)
	}

	ch := parseCryptoHeader(data)
	if ch == nil {
		t.Fatalf("%s: no crypto header", tripleDESFixture)
	}

	if ch.Algorithm != CryptoDESTriple {
		t.Fatalf("%s: algorithm = %d, want %d",
			tripleDESFixture, ch.Algorithm, CryptoDESTriple)
	}

	if ch.Mode != cryptoModeCTS {
		t.Fatalf("%s: mode = %d, want cmCTS", tripleDESFixture, ch.Mode)
	}

	return ch
}

// TestTripleDESFixtureControlBlock is the load-bearing test for this cipher: a
// real ComponentAce-written file encrypted with "DES-Triple" decrypts to a
// ControlBlock whose CRC matches the one stored beside it. It is what settled
// that the algorithm is TCipher_3TDES with a 24-byte block rather than plain
// 3DES with an 8-byte one.
//
// The 256-byte ControlBlock is ten full 24-byte blocks plus a 16-byte
// remainder, so this also exercises decryptCTS's trailing partial-block path.
func TestTripleDESFixtureControlBlock(t *testing.T) {
	ch := tripleDESFixtureHeader(t)

	if ch.ControlCRC != tripleDESControlCRC {
		t.Fatalf("stored ControlCRC = %#08x, want %#08x",
			ch.ControlCRC, uint32(tripleDESControlCRC))
	}

	digest := ripemd128Sum([]byte(tripleDESPassword))

	block, err := newTripleDES(digest[:])
	if err != nil {
		t.Fatalf("newTripleDES: %v", err)
	}

	control := make([]byte, controlBlockSize)
	decryptCTS(block, control, ch.ControlBlock[:])

	if got := absCRC32(control); got != ch.ControlCRC {
		t.Errorf("ControlBlock CRC = %#08x, want %#08x", got, ch.ControlCRC)
	}
}

// TestTripleDESSwapTypoIsLoadBearing documents that DEC's word-exchange typo
// must not be "fixed".
//
// DEC's Encode contains, twice:
//
//	T := PIntArray(Data)[3]; PIntArray(Data)[3] := PIntArray(Data)[4]; PIntArray(Data)[3] := T;
//
// which assigns to word 3 twice and never to word 4, making that half of the
// exchange a no-op. The obvious reading — that words 3 and 4 were meant to be
// exchanged as well — produces a cipher that does not decrypt real files. This
// test asserts exactly that: with the typo reproduced the fixture's
// ControlBlock CRC matches; with the swap "corrected" it does not.
//
// If this test ever fails because the corrected variant now matches, someone
// has changed tripleDESSwap. Change it back.
func TestTripleDESSwapTypoIsLoadBearing(t *testing.T) {
	ch := tripleDESFixtureHeader(t)
	digest := ripemd128Sum([]byte(tripleDESPassword))

	corrected := newTripleDESVariant(t, digest[:], true)

	control := make([]byte, controlBlockSize)
	decryptCTS(corrected, control, ch.ControlBlock[:])

	if absCRC32(control) == ch.ControlCRC {
		t.Error("the corrected word swap also decrypts the fixture; " +
			"the typo is no longer load-bearing and this test needs rewriting")
	}
}

// TestTripleDESMatchesReference checks the production implementation against
// the independent transcription in tripleDESVariant, and shows that the
// corrected swap really is a different cipher (so the test above is not merely
// passing because the two variants coincide).
func TestTripleDESMatchesReference(t *testing.T) {
	key := make([]byte, tripleDESKeySize)
	for i := range key {
		key[i] = byte(i*13 + 1)
	}

	plain := make([]byte, tripleDESBlockSize)
	for i := range plain {
		plain[i] = byte(0x5A + i)
	}

	c, err := newTripleDES(key)
	if err != nil {
		t.Fatalf("newTripleDES: %v", err)
	}

	got := make([]byte, tripleDESBlockSize)
	c.Encrypt(got, plain)

	want := make([]byte, tripleDESBlockSize)
	newTripleDESVariant(t, key, false).Encrypt(want, plain)

	if !bytes.Equal(got, want) {
		t.Errorf("Encrypt = %x, reference = %x", got, want)
	}

	fixed := make([]byte, tripleDESBlockSize)
	newTripleDESVariant(t, key, true).Encrypt(fixed, plain)

	if bytes.Equal(got, fixed) {
		t.Error("corrected swap gives the same ciphertext; the guard test is vacuous")
	}
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

	c, err := newTripleDES(key)
	if err != nil {
		t.Fatalf("newTripleDES: %v", err)
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

		c, err := newTripleDES(key)
		if err != nil {
			t.Fatalf("newTripleDES(%d): %v", keyLen, err)
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
		if _, err := newTripleDES(make([]byte, n)); !errors.Is(err, ErrTripleDESKeySize) {
			t.Errorf("newTripleDES(%d bytes) error = %v, want ErrTripleDESKeySize", n, err)
		}
	}

	for _, n := range []int{tripleDESDerivedKeySize, tripleDESKeySize} {
		if _, err := newTripleDES(make([]byte, n)); err != nil {
			t.Errorf("newTripleDES(%d bytes) = %v, want success", n, err)
		}
	}
}
