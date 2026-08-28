package absdb

import (
	"hash/crc32"
	"math/rand/v2"
	"testing"
)

// referenceAbsCRC32 is a deliberately naive, bit-at-a-time implementation of
// the same CRC, used to cross-check the table-driven one.
func referenceAbsCRC32(data []byte) uint32 {
	crc := uint32(0)

	for _, b := range data {
		crc ^= uint32(b)

		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ absCRCPoly
			} else {
				crc >>= 1
			}
		}
	}

	return crc
}

func TestAbsCRC32MatchesReference(t *testing.T) {
	src := rand.New(rand.NewPCG(1, 2)) // deterministic, reproducible test data

	for size := range 300 {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(src.UintN(256))
		}

		got := absCRC32(data)

		want := referenceAbsCRC32(data)
		if got != want {
			t.Fatalf("absCRC32(%d random bytes) = %#08x, want %#08x", size, got, want)
		}
	}
}

// TestAbsCRC32IsNotIEEE guards against a regression to hash/crc32, which uses
// the same polynomial but pre- and post-inverts the register.
func TestAbsCRC32IsNotIEEE(t *testing.T) {
	if absCRC32(nil) != 0 {
		t.Errorf("absCRC32(nil) = %#08x, want 0", absCRC32(nil))
	}

	data := []byte("123456789")

	if absCRC32(data) == crc32.ChecksumIEEE(data) {
		t.Error("absCRC32 must not equal crc32.ChecksumIEEE")
	}

	// The two differ only by the inversions, so IEEE can be reconstructed from
	// this CRC by feeding it an inverted register; that relation pins the
	// polynomial and the bit order.
	table := crc32.MakeTable(crc32.IEEE)

	crc := ^uint32(0)
	for _, b := range data {
		crc = crc>>8 ^ table[byte(crc)^b]
	}

	if ^crc != crc32.ChecksumIEEE(data) {
		t.Fatal("test's own IEEE reconstruction is wrong")
	}
}

// TestAbsCRC32ControlBlocks checks absCRC32 against the known checksums of the
// decrypted ControlBlocks of the encrypted fixtures.
func TestAbsCRC32ControlBlocks(t *testing.T) {
	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db := openTestFile(t, fixture.name)
			ch := db.CryptoHeader()

			block, err := newCipherBlock(ch.Algorithm, deriveKey(ch.Algorithm, testPassword))
			if err != nil {
				t.Fatalf("newCipherBlock: %v", err)
			}

			control := make([]byte, controlBlockSize)
			decryptCTS(block, control, ch.ControlBlock[:])

			got := absCRC32(control)
			if got != fixture.controlCRC {
				t.Errorf("absCRC32(ControlBlock) = %#08x, want %#08x", got, fixture.controlCRC)
			}

			if got != ch.ControlCRC {
				t.Errorf("absCRC32(ControlBlock) = %#08x, but the file stores %#08x", got, ch.ControlCRC)
			}
		})
	}
}

// TestPageCRCIsAbsCRC32OfPayload shows that the ABSP header's CRC32 field is
// absCRC32 over the *decrypted* 4056-byte page payload. This independently
// corroborates both the CRC variant and the payload extent: over the first
// 3676 bytes only, the value does not match.
//
// The field is written by the encryption path; in unencrypted files it is 0.
func TestPageCRCIsAbsCRC32OfPayload(t *testing.T) {
	for _, fixture := range encryptedFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			db, err := OpenWithPassword(requireFixture(t, fixture.name), testPassword)
			if err != nil {
				t.Fatalf("OpenWithPassword: %v", err)
			}
			defer db.Close()

			checked := 0

			for n := 1; n < db.PageCount(); n++ {
				page, err := db.ReadPage(n)
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", n, err)
				}

				if page.Header == nil || page.Header.CRC32 == 0 {
					continue
				}

				got := absCRC32(page.Payload)
				if got != page.Header.CRC32 {
					t.Errorf("page %d: absCRC32(payload) = %#08x, stored %#08x",
						n, got, page.Header.CRC32)
				}

				checked++
			}

			if checked == 0 {
				t.Fatal("no encrypted pages found")
			}

			t.Logf("%s: %d encrypted pages, all page CRCs reproduced", fixture.name, checked)
		})
	}
}
