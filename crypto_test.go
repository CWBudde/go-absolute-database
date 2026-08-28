package absdb

import (
	"bytes"
	"errors"
	"testing"
)

// The three encrypted fixtures (Addresses-Rijndael_128.abs,
// Addresses-Blowfish.abs, Addresses-DES_Single.abs) are encrypted copies of
// Addresses.abs, which holds an *empty* table. They therefore validate the
// crypto header, key derivation, the cmCTS mode and full-page decryption
// byte-for-byte, but they can never validate the decryption of encrypted
// *records* — there are none. A fixture with rows and a known password is
// still missing.

// testPassword is the password of all three encrypted fixtures (capital B).
const testPassword = "Bla"

// encryptedFixtures lists the encrypted fixtures with their expected algorithm
// and the CRC32 of their decrypted ControlBlock.
var encryptedFixtures = []struct {
	name       string
	algorithm  CryptoAlgorithm
	controlCRC uint32
}{
	{"Addresses-Rijndael_128.abs", CryptoRijndael128, 0xffac4819},
	{"Addresses-DES_Single.abs", CryptoDESSingle, 0xd9f59b5b},
	{"Addresses-Blowfish.abs", CryptoBlowfish, 0x5a2f73e6},
}

func TestCryptoHeaderEncryptedFile(t *testing.T) {
	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db := openTestFile(t, fixture.name)

			if !db.Encrypted() {
				t.Fatalf("expected %s to be encrypted", fixture.name)
			}

			ch := db.CryptoHeader()
			if ch == nil {
				t.Fatal("expected non-nil CryptoHeader for encrypted file")
			}

			if ch.Algorithm != fixture.algorithm {
				t.Errorf("algorithm = %v, want %v", ch.Algorithm, fixture.algorithm)
			}

			if ch.Mode != cryptoModeCTS {
				t.Errorf("mode = %d, want cmCTS (%d)", ch.Mode, cryptoModeCTS)
			}

			if ch.HeaderSize != cryptoHeaderSize {
				t.Errorf("header size = %d, want %d", ch.HeaderSize, cryptoHeaderSize)
			}

			if ch.ControlCRC != fixture.controlCRC {
				t.Errorf("ControlBlockCRC = %#08x, want %#08x", ch.ControlCRC, fixture.controlCRC)
			}
		})
	}
}

func TestCryptoHeaderUnencryptedFile(t *testing.T) {
	for _, name := range []string{"TS03.abs", "Addresses.abs"} {
		t.Run(name, func(t *testing.T) {
			db := openTestFile(t, name)

			if db.Encrypted() {
				t.Fatalf("%s should not be encrypted", name)
			}

			if db.CryptoHeader() != nil {
				t.Error("expected nil CryptoHeader for unencrypted file")
			}
		})
	}
}

func TestVerifyPassword(t *testing.T) {
	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db := openTestFile(t, fixture.name)

			if !db.VerifyPassword(testPassword) {
				t.Errorf("VerifyPassword(%q) = false, want true", testPassword)
			}

			// The password is case sensitive: "bla" must be rejected.
			for _, wrong := range []string{"bla", "BLA", "wrong", "", "Bla "} {
				if db.VerifyPassword(wrong) {
					t.Errorf("VerifyPassword(%q) = true, want false", wrong)
				}
			}
		})
	}
}

// TestDecryptedPagesMatchPlaintext is the decisive test: every page of every
// encrypted fixture must decrypt byte-identically to the corresponding page of
// the unencrypted Addresses.abs, over the full payload — including the
// trailing partial cipher block.
func TestDecryptedPagesMatchPlaintext(t *testing.T) {
	plain := openTestFile(t, "Addresses.abs")

	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db, err := OpenWithPassword(testdataPath(fixture.name), testPassword)
			if err != nil {
				t.Fatalf("OpenWithPassword: %v", err)
			}
			defer db.Close()

			if db.PageCount() != plain.PageCount() {
				t.Fatalf("page count = %d, want %d", db.PageCount(), plain.PageCount())
			}

			matched := 0

			for i := range db.PageCount() {
				want, err := plain.ReadPage(i)
				if err != nil {
					t.Fatalf("plain ReadPage(%d): %v", i, err)
				}

				got, err := db.ReadPage(i)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", i, err)
				}

				if len(got.Payload) != db.payloadSize() {
					t.Fatalf("page %d payload = %d bytes, want %d",
						i, len(got.Payload), db.payloadSize())
				}

				if !bytes.Equal(got.Payload, want.Payload) {
					t.Errorf("page %d: payload differs at byte %d",
						i, firstDiff(got.Payload, want.Payload))

					continue
				}

				matched++
			}

			t.Logf("%s: %d/%d pages byte-identical", fixture.name, matched, db.PageCount())

			if matched != db.PageCount() {
				t.Errorf("%d/%d pages byte-identical", matched, db.PageCount())
			}
		})
	}
}

// firstDiff returns the index of the first differing byte, or -1.
func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	if len(a) != len(b) {
		return min(len(a), len(b))
	}

	return -1
}

func TestOpenWithPassword(t *testing.T) {
	plain := openTestFile(t, "Addresses.abs")

	wantSchema, err := plain.Schema()
	if err != nil {
		t.Fatalf("Schema of plaintext fixture: %v", err)
	}

	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db, err := OpenWithPassword(testdataPath(fixture.name), testPassword)
			if err != nil {
				t.Fatalf("OpenWithPassword: %v", err)
			}
			defer db.Close()

			schema, err := db.Schema()
			if err != nil {
				t.Fatalf("Schema: %v", err)
			}

			if len(schema.Columns) != len(wantSchema.Columns) {
				t.Fatalf("got %d columns, want %d", len(schema.Columns), len(wantSchema.Columns))
			}

			for i, col := range schema.Columns {
				if col.Name != wantSchema.Columns[i].Name {
					t.Errorf("column %d = %q, want %q", i, col.Name, wantSchema.Columns[i].Name)
				}
			}
		})
	}
}

func TestOpenWithWrongPassword(t *testing.T) {
	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := requireFixture(t, fixture.name)

			for _, wrong := range []string{"bla", "wrong", ""} {
				_, err := OpenWithPassword(path, wrong)
				if !errors.Is(err, ErrWrongPassword) {
					t.Errorf("OpenWithPassword(%q) error = %v, want ErrWrongPassword", wrong, err)
				}
			}
		})
	}
}

// TestUnlock covers the in-place variant used by the command line tool.
func TestUnlock(t *testing.T) {
	db := openTestFile(t, "Addresses-Blowfish.abs")

	err := db.Unlock("bla")
	if !errors.Is(err, ErrWrongPassword) {
		t.Errorf("Unlock(%q) = %v, want ErrWrongPassword", "bla", err)
	}

	err = db.Unlock(testPassword)
	if err != nil {
		t.Fatalf("Unlock(%q): %v", testPassword, err)
	}

	page, err := db.ReadPage(1)
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}

	if page.Header == nil {
		t.Fatal("expected a parsed ABSP header after Unlock")
	}
}

// TestUnlockUnsupportedCipher documents the algorithms this package knows about
// but cannot decrypt.
func TestUnlockUnsupportedCipher(t *testing.T) {
	for _, algo := range []CryptoAlgorithm{CryptoTwofish128, CryptoTwofish256, CryptoSquare} {
		key := deriveKey(algo, testPassword)
		if key == nil {
			t.Errorf("deriveKey(%v) = nil, want a derived key", algo)
		}

		_, err := newCipherBlock(algo, key)
		if !errors.Is(err, ErrUnsupportedCipher) {
			t.Errorf("newCipherBlock(%v) error = %v, want ErrUnsupportedCipher", algo, err)
		}
	}
}

// TestKeySizes pins the algorithm-to-hash mapping recovered from the DCU.
func TestKeySizes(t *testing.T) {
	tests := []struct {
		algo CryptoAlgorithm
		size int
		hash keyHash
	}{
		{CryptoRijndael128, 16, hashRipeMD128},
		{CryptoRijndael256, 32, hashRipeMD256},
		{CryptoDESSingle, 8, hashRipeMD128},
		{CryptoDESTriple, 16, hashRipeMD128},
		{CryptoBlowfish, 32, hashRipeMD256},
		{CryptoTwofish128, 16, hashRipeMD128},
		{CryptoTwofish256, 32, hashRipeMD256},
		{CryptoSquare, 16, hashRipeMD128},
	}

	for _, tt := range tests {
		key := deriveKey(tt.algo, testPassword)
		if len(key) != tt.size {
			t.Errorf("deriveKey(%v) = %d bytes, want %d", tt.algo, len(key), tt.size)
		}

		var want []byte

		if tt.hash == hashRipeMD256 {
			digest := ripemd256Sum([]byte(testPassword))
			want = digest[:tt.size]
		} else {
			digest := ripemd128Sum([]byte(testPassword))
			want = digest[:tt.size]
		}

		if !bytes.Equal(key, want) {
			t.Errorf("deriveKey(%v) = %x, want %x", tt.algo, key, want)
		}
	}
}

// TestEncryptedFileWithoutPassword checks that an encrypted page read without a
// password yields no usable page header rather than silent garbage.
func TestEncryptedFileWithoutPassword(t *testing.T) {
	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db := openTestFile(t, fixture.name)

			// Page 1 is encrypted; its ABSP header stays in the clear, but the
			// payload is ciphertext, so the schema cannot be parsed.
			_, err := db.Schema()
			if err == nil {
				t.Error("expected an error reading the schema without a password")
			}
		})
	}
}
