package absdb

import (
	"bytes"
	"crypto/cipher"
	"crypto/des"
	"os"
	"testing"

	"github.com/cwbudde/go-absolute-database/internal/deccrypto"
	"github.com/cwbudde/go-absolute-database/internal/ripemd"
)

// tripleDESFixture is the real .abs file that pins this cipher, together with
// its password and the CRC of its decrypted ControlBlock.
const (
	tripleDESKeySize    = 24
	tripleDESBlockSize  = 24
	tripleDESFixture    = "Employees-DES_Triple.abs"
	tripleDESPassword   = "Bla"
	tripleDESControlCRC = 0xd1a0c93f
)

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
// same lengths deccrypto.NewTripleDES accepts.
func newTripleDESVariant(t *testing.T, key []byte, fixSwap bool) *tripleDESVariant {
	t.Helper()

	full := make([]byte, tripleDESKeySize)
	copy(full, key)

	v := &tripleDESVariant{fixSwap: fixSwap}

	for i := range v.keys {
		b, err := des.NewCipher(full[i*des.BlockSize : (i+1)*des.BlockSize])
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

		for i := 0; i < tripleDESBlockSize; i += des.BlockSize {
			if encrypt[stage] {
				b.Encrypt(buf[i:i+des.BlockSize], buf[i:i+des.BlockSize])
			} else {
				b.Decrypt(buf[i:i+des.BlockSize], buf[i:i+des.BlockSize])
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

	digest := ripemd.Sum128([]byte(tripleDESPassword))

	block, err := deccrypto.NewTripleDES(digest[:])
	if err != nil {
		t.Fatalf("deccrypto.NewTripleDES: %v", err)
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
	digest := ripemd.Sum128([]byte(tripleDESPassword))

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

	c, err := deccrypto.NewTripleDES(key)
	if err != nil {
		t.Fatalf("deccrypto.NewTripleDES: %v", err)
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
