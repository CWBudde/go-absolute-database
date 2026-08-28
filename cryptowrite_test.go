package absdb

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"
)

// allAlgorithms lists every TABSCryptoAlgorithm the format defines. The
// encrypt path has to work for all of them, not just for the ones a fixture
// happens to exercise.
var allAlgorithms = []CryptoAlgorithm{
	CryptoRijndael128, CryptoRijndael256, CryptoDESSingle, CryptoDESTriple,
	CryptoBlowfish, CryptoTwofish128, CryptoTwofish256, CryptoSquare,
}

// pagePayloadBytes is the payload length of a 4096-byte page: the size the
// encrypt path sees in practice, and one that leaves a trailing partial block
// for every block size the format uses.
const pagePayloadBytes = 4096 - diskPageHeaderSize

// TestEncryptPayloadRoundTripsFixtures is the decisive test for the encrypt
// path: every encrypted page of every Employees-* fixture must re-encrypt to
// the byte-identical ciphertext the ComponentAce engine wrote.
//
// The comparison is between two well-defined byte ranges:
//
//   - the ciphertext, read straight from the file on disk with os.ReadFile, at
//     file[n*pageSize+pageDataOffset : +payloadSize] — the payload of page n is
//     contiguous on disk even though it runs into the following block;
//   - the plaintext, which is Page.Payload as ReadPage returns it, since
//     ReadPage decrypts the payload in place.
//
// Encrypting the second must reproduce the first. Any deviation in encryptCTS
// — a mis-ordered feedback update, a wrong IV fill byte, a partial block
// handled as a full one — shows up here, because the ciphertext was produced by
// the vendor's own implementation and not by this package.
func TestEncryptPayloadRoundTripsFixtures(t *testing.T) {
	for _, fixture := range employeeFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := requireFixture(t, fixture.name)

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			db, err := OpenWithPassword(path, testPassword)
			if err != nil {
				t.Fatalf("OpenWithPassword: %v", err)
			}
			defer db.Close()

			checkControlBlockRoundTrip(t, db)

			encrypted := 0

			// Page 0 is never encrypted, so the scan starts at page 1.
			for n := 1; n < db.PageCount(); n++ {
				page, err := db.ReadPage(n)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", n, err)
				}

				// A zero CRC32 marks an unencrypted page; ReadPage leaves those
				// alone, so there is nothing to re-encrypt.
				if page.Header == nil || page.Header.CRC32 == 0 {
					continue
				}

				start := n*db.PageSize() + pageDataOffset

				end := start + db.payloadSize()
				if end > len(raw) {
					t.Fatalf("page %d: payload ends at %d, file is %d bytes", n, end, len(raw))
				}

				ciphertext := raw[start:end]

				if bytes.Equal(ciphertext, page.Payload) {
					t.Fatalf("page %d: on-disk bytes equal the decrypted payload, "+
						"so this page proves nothing", n)
				}

				got := slices.Clone(page.Payload)

				err = db.encryptPayload(got)
				if err != nil {
					t.Fatalf("encryptPayload(page %d): %v", n, err)
				}

				if !bytes.Equal(got, ciphertext) {
					t.Fatalf("page %d: re-encrypted payload differs from the on-disk "+
						"ciphertext at byte %d", n, firstDiff(got, ciphertext))
				}

				encrypted++
			}

			t.Logf("%s: %d/%d pages re-encrypted byte-identically",
				fixture.name, encrypted, db.PageCount())

			if encrypted == 0 {
				t.Errorf("no encrypted page found in %s, nothing was verified", fixture.name)
			}
		})
	}
}

// checkControlBlockRoundTrip re-encrypts the decrypted ControlBlock of an
// encrypted fixture. It is a second unit of real vendor ciphertext with a
// different length than a page payload — 256 bytes, which for the 24-byte
// DES-Triple block is ten full blocks plus a 16-byte partial one — so it
// exercises the trailing-partial path at another offset.
func checkControlBlockRoundTrip(t *testing.T, db *File) {
	t.Helper()

	ch := db.CryptoHeader()
	if ch == nil {
		t.Fatal("expected a crypto header")
	}

	block, err := newCipherBlock(ch.Algorithm, db.decryptionKey)
	if err != nil {
		t.Fatalf("newCipherBlock: %v", err)
	}

	control := make([]byte, controlBlockSize)
	decryptCTS(block, control, ch.ControlBlock[:])
	encryptCTS(block, control, control)

	if !bytes.Equal(control, ch.ControlBlock[:]) {
		t.Errorf("re-encrypted ControlBlock differs from the stored one at byte %d",
			firstDiff(control, ch.ControlBlock[:]))
	}
}

// TestEncryptCTSInvertsDecryptCTS checks the round trip for every cipher over
// lengths that hit both the full-block and the trailing-partial path, and
// checks that both directions tolerate dst aliasing src exactly, which is how
// pages are transformed in place.
func TestEncryptCTSInvertsDecryptCTS(t *testing.T) {
	for _, algo := range allAlgorithms {
		t.Run(algo.String(), func(t *testing.T) {
			block, err := newCipherBlock(algo, deriveKey(algo, testPassword))
			if err != nil {
				t.Fatalf("newCipherBlock: %v", err)
			}

			size := block.BlockSize()

			lengths := []int{
				0,                // nothing to do at all
				1,                // a partial block only
				size - 1,         // just short of one block
				size,             // exactly one block
				3 * size,         // an exact multiple, no partial block
				3*size + 1,       // full blocks plus a one-byte partial
				4*size + 5,       // full blocks plus a wider partial
				pagePayloadBytes, // the length a real 4096-byte page payload has
			}

			for _, length := range lengths {
				plain := patternBytes(length)

				cipherText := make([]byte, length)
				encryptCTS(block, cipherText, plain)

				roundTripped := make([]byte, length)
				decryptCTS(block, roundTripped, cipherText)

				if !bytes.Equal(roundTripped, plain) {
					t.Errorf("length %d: round trip differs at byte %d",
						length, firstDiff(roundTripped, plain))
				}

				// The same, with dst aliasing src exactly.
				inPlace := slices.Clone(plain)
				encryptCTS(block, inPlace, inPlace)

				if !bytes.Equal(inPlace, cipherText) {
					t.Errorf("length %d: in-place encryption differs from the "+
						"out-of-place result at byte %d", length, firstDiff(inPlace, cipherText))
				}

				decryptCTS(block, inPlace, inPlace)

				if !bytes.Equal(inPlace, plain) {
					t.Errorf("length %d: in-place round trip differs at byte %d",
						length, firstDiff(inPlace, plain))
				}
			}
		})
	}
}

// TestEncryptCTSPartialBlockIsSymmetric pins the one place where the two
// directions share an expression: a unit shorter than the block size is masked
// with E(F) in both, so encrypting and decrypting it produce the same bytes.
// Should encryptCTS ever grow its own partial-block handling, this fails.
func TestEncryptCTSPartialBlockIsSymmetric(t *testing.T) {
	for _, algo := range allAlgorithms {
		t.Run(algo.String(), func(t *testing.T) {
			block, err := newCipherBlock(algo, deriveKey(algo, testPassword))
			if err != nil {
				t.Fatalf("newCipherBlock: %v", err)
			}

			for length := 1; length < block.BlockSize(); length++ {
				plain := patternBytes(length)

				encrypted := make([]byte, length)
				encryptCTS(block, encrypted, plain)

				decrypted := make([]byte, length)
				decryptCTS(block, decrypted, plain)

				if !bytes.Equal(encrypted, decrypted) {
					t.Fatalf("length %d: encryptCTS and decryptCTS disagree at byte %d",
						length, firstDiff(encrypted, decrypted))
				}
			}
		})
	}
}

// TestEncryptPayloadErrors covers the guard clauses, which have to reject the
// same states decryptPayload rejects: a file that was never unlocked, a cipher
// mode other than cmCTS, and an algorithm byte outside the enum.
func TestEncryptPayloadErrors(t *testing.T) {
	key := deriveKey(CryptoBlowfish, testPassword)

	tests := []struct {
		name string
		db   *File
		want error
	}{
		{
			name: "no crypto header",
			db:   &File{},
			want: ErrNoPassword,
		},
		{
			name: "unsupported mode",
			db: &File{
				cryptoHeader:  &CryptoHeader{Algorithm: CryptoBlowfish, Mode: cryptoModeCTS + 1},
				decryptionKey: key,
			},
			want: ErrUnsupportedCipherMode,
		},
		{
			name: "algorithm outside the enum",
			db: &File{
				cryptoHeader:  &CryptoHeader{Algorithm: CryptoAlgorithm(99), Mode: cryptoModeCTS},
				decryptionKey: key,
			},
			want: ErrUnsupportedCipher,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := patternBytes(64)
			untouched := slices.Clone(payload)

			err := tt.db.encryptPayload(payload)
			if !errors.Is(err, tt.want) {
				t.Errorf("encryptPayload error = %v, want %v", err, tt.want)
			}

			if !bytes.Equal(payload, untouched) {
				t.Error("payload was modified even though encryptPayload failed")
			}
		})
	}
}

// patternBytes returns deterministic test data. A fixed pattern keeps a failure
// reproducible; the exact bytes do not matter, only that they are not uniform.
func patternBytes(n int) []byte {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte(i*31 + 7)
	}

	return buf
}
