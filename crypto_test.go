package absdb

import (
	"bytes"
	"errors"
	"testing"
)

// The three Addresses-* encrypted fixtures (Addresses-Rijndael_128.abs,
// Addresses-Blowfish.abs, Addresses-DES_Single.abs) are encrypted copies of
// Addresses.abs, which holds an *empty* table. They validate the crypto header,
// key derivation, the cmCTS mode and full-page decryption byte-for-byte against
// their plaintext twin, but they cannot validate the decryption of encrypted
// *records* — there are none.
//
// The eight Employees-* fixtures close that gap: one per algorithm, each
// carrying a real table with three rows, so TestEmployeeFixtures reads
// encrypted schema and encrypted records end to end. They have no plaintext
// twin, so instead of a byte comparison they are checked against the ABSP page
// checksum, which is absCRC32 over the *decrypted* payload and so is an
// independent oracle.

// testPassword is the password of every encrypted fixture (capital B).
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

// TestAllAlgorithmsSupported records the completeness milestone: all eight
// TABSCryptoAlgorithm values the format defines are implemented, Square — the
// last one — included. Every one derives a key and yields a working block.
func TestAllAlgorithmsSupported(t *testing.T) {
	algorithms := []CryptoAlgorithm{
		CryptoRijndael128, CryptoRijndael256, CryptoDESSingle, CryptoDESTriple,
		CryptoBlowfish, CryptoTwofish128, CryptoTwofish256, CryptoSquare,
	}

	for _, algo := range algorithms {
		key := deriveKey(algo, testPassword)
		if key == nil {
			t.Errorf("deriveKey(%v) = nil, want a derived key", algo)

			continue
		}

		block, err := newCipherBlock(algo, key)
		if err != nil {
			t.Errorf("newCipherBlock(%v) = %v, want a working block", algo, err)

			continue
		}

		if block.BlockSize() <= 0 {
			t.Errorf("newCipherBlock(%v) block size = %d, want a positive size",
				algo, block.BlockSize())
		}
	}
}

// TestUnlockUnsupportedCipher covers what is left of the ErrUnsupportedCipher
// path. With Square implemented, all eight defined algorithms can be decrypted,
// so the error is reserved for algorithm bytes outside the enum — a value read
// from a corrupt or future crypto header.
func TestUnlockUnsupportedCipher(t *testing.T) {
	for _, algo := range []CryptoAlgorithm{CryptoAlgorithm(8), CryptoAlgorithm(99), CryptoAlgorithm(255)} {
		if key := deriveKey(algo, testPassword); key != nil {
			t.Errorf("deriveKey(%v) = %x, want nil", algo, key)
		}

		_, err := newCipherBlock(algo, nil)
		if !errors.Is(err, ErrUnsupportedCipher) {
			t.Errorf("newCipherBlock(%v) error = %v, want ErrUnsupportedCipher", algo, err)
		}
	}
}

// TestCryptoAlgorithmString covers the names shown by the Absolute Database UI
// and the fallback for a value outside the enum.
func TestCryptoAlgorithmString(t *testing.T) {
	tests := []struct {
		algo CryptoAlgorithm
		want string
	}{
		{CryptoRijndael128, "Rijndael-128"},
		{CryptoRijndael256, "Rijndael-256"},
		{CryptoDESSingle, "DES-Single"},
		{CryptoDESTriple, "DES-Triple"},
		{CryptoBlowfish, "Blowfish"},
		{CryptoTwofish128, "Twofish-128"},
		{CryptoTwofish256, "Twofish-256"},
		{CryptoSquare, "Square"},
		{CryptoAlgorithm(8), "CryptoAlgorithm(8)"},
		{CryptoAlgorithm(255), "CryptoAlgorithm(255)"},
	}

	for _, tt := range tests {
		if got := tt.algo.String(); got != tt.want {
			t.Errorf("CryptoAlgorithm(%d).String() = %q, want %q", byte(tt.algo), got, tt.want)
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

// employeeFixtures are encrypted databases holding a real table, created with
// the ComponentAce Absolute Database Manager -- one per encryption algorithm.
// Unlike the Addresses-* fixtures they have no plaintext twin, so decryption is
// checked against each page's ABSP checksum instead.
//
// They are the whole reason two of these ciphers are correct: Rijndael-256 and
// DES-Triple were both implemented from inference and both silently wrong until
// a real file existed to test them against. There are eight, one per algorithm,
// so no cipher rests on inference or on an empty table any more.
var employeeFixtures = []struct {
	name      string
	algorithm CryptoAlgorithm
}{
	{"Employees-Rijndael_128.abs", CryptoRijndael128},
	{"Employees-Rijndael_256.abs", CryptoRijndael256},
	{"Employees-DES_Single.abs", CryptoDESSingle},
	{"Employees-DES_Triple.abs", CryptoDESTriple},
	{"Employees-Blowfish.abs", CryptoBlowfish},
	{"Employees-Twofish_128.abs", CryptoTwofish128},
	{"Employees-Twofish_256.abs", CryptoTwofish256},
	{"Employees-Square.abs", CryptoSquare},
}

// employeeRows is what the Absolute Database Manager shows for these
// fixtures. All of them hold the identical table; only the cipher differs.
var employeeRows = []struct {
	id     int32
	name   string
	salary float64
	active bool
}{
	{1, "Ada", 1234.5, true},
	{2, "Grace", 2345.75, false},
	{3, "Kurt", 999.25, true},
}

// TestEmployeeFixtures reads every algorithm's fixture end to end: header,
// password verification, per-page decryption, schema and record values. All
// eight defined algorithms are covered.
func TestEmployeeFixtures(t *testing.T) {
	for _, fixture := range employeeFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db, err := OpenWithPassword(requireFixture(t, fixture.name), testPassword)
			if err != nil {
				t.Fatalf("OpenWithPassword: %v", err)
			}
			defer db.Close()

			ch := db.CryptoHeader()
			if ch == nil {
				t.Fatal("expected a crypto header")
			}

			if ch.Algorithm != fixture.algorithm {
				t.Errorf("algorithm = %v, want %v", ch.Algorithm, fixture.algorithm)
			}

			checkPageChecksums(t, db)

			schema, err := db.Schema()
			if err != nil {
				t.Fatalf("Schema: %v", err)
			}

			wantColumns := []string{"Id", "Name", "Salary", "Active"}
			if len(schema.Columns) != len(wantColumns) {
				t.Fatalf("got %d columns, want %d", len(schema.Columns), len(wantColumns))
			}

			for i, want := range wantColumns {
				if schema.Columns[i].Name != want {
					t.Errorf("column %d = %q, want %q", i, schema.Columns[i].Name, want)
				}
			}

			reader, err := db.OpenTable()
			if err != nil {
				t.Fatalf("OpenTable: %v", err)
			}

			row := 0

			for reader.Next() {
				if row >= len(employeeRows) {
					t.Fatalf("more than %d rows", len(employeeRows))
				}

				want := employeeRows[row]
				rec := reader.Record()

				if got := rec.Int(0); got != want.id {
					t.Errorf("row %d Id = %d, want %d", row, got, want.id)
				}

				if got := rec.String(1); got != want.name {
					t.Errorf("row %d Name = %q, want %q", row, got, want.name)
				}

				if got := rec.Float(2); got != want.salary {
					t.Errorf("row %d Salary = %v, want %v", row, got, want.salary)
				}

				if got := rec.Bool(3); got != want.active {
					t.Errorf("row %d Active = %v, want %v", row, got, want.active)
				}

				row++
			}

			if err := reader.Err(); err != nil {
				t.Fatalf("iteration: %v", err)
			}

			if row != len(employeeRows) {
				t.Errorf("read %d rows, want %d", row, len(employeeRows))
			}
		})
	}
}

// TestEncryptedPageChecksums applies the ABSP checksum oracle to every
// encrypted fixture, Twofish and otherwise. It is independent of the plaintext
// twin used by TestDecryptedPagesMatchPlaintext.
func TestEncryptedPageChecksums(t *testing.T) {
	names := make([]string, 0, len(encryptedFixtures)+len(employeeFixtures))
	for _, f := range encryptedFixtures {
		names = append(names, f.name)
	}

	for _, f := range employeeFixtures {
		names = append(names, f.name)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			db, err := OpenWithPassword(requireFixture(t, name), testPassword)
			if err != nil {
				t.Fatalf("OpenWithPassword: %v", err)
			}
			defer db.Close()

			checkPageChecksums(t, db)
		})
	}
}

// TestEmployeeFixturesWrongPassword checks that every algorithm's key
// derivation rejects near misses rather than producing garbage.
func TestEmployeeFixturesWrongPassword(t *testing.T) {
	for _, fixture := range employeeFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := requireFixture(t, fixture.name)

			for _, wrong := range []string{"bla", "BLA", "Bl", "Bla ", "", "Blb"} {
				if _, err := OpenWithPassword(path, wrong); !errors.Is(err, ErrWrongPassword) {
					t.Errorf("OpenWithPassword(%q) error = %v, want ErrWrongPassword", wrong, err)
				}
			}

			db := openTestFile(t, fixture.name)
			if !db.VerifyPassword(testPassword) {
				t.Errorf("VerifyPassword(%q) = false, want true", testPassword)
			}
		})
	}
}

// checkPageChecksums verifies every decrypted page against its ABSP checksum.
// The checksum is stored in the clear but covers the decrypted payload, so it
// is an oracle for decryption that needs no plaintext twin.
func checkPageChecksums(t *testing.T, db *File) {
	t.Helper()

	verified := 0

	for i := range db.PageCount() {
		page, err := db.ReadPage(i)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", i, err)
		}

		if page.Header == nil || page.Header.CRC32 == 0 {
			continue
		}

		if got := absCRC32(page.Payload); got != page.Header.CRC32 {
			t.Errorf("page %d: payload CRC %#08x, want %#08x", i, got, page.Header.CRC32)
		}

		verified++
	}

	if verified == 0 {
		t.Error("no page carried a checksum; the oracle checked nothing")
	}

	t.Logf("%d pages verified against their ABSP checksum", verified)
}
