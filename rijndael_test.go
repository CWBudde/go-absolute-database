package absdb

import (
	"os"
	"testing"

	"github.com/cwbudde/go-absolute-database/internal/deccrypto"
	"github.com/cwbudde/go-absolute-database/internal/ripemd"
)

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

	key := ripemd.Sum256([]byte("Bla"))

	c, err := deccrypto.NewRijndael(key[:32])
	if err != nil {
		t.Fatalf("deccrypto.NewRijndael: %v", err)
	}

	control := make([]byte, controlBlockSize)
	decryptCTS(c, control, ch.ControlBlock[:])

	if got := absCRC32(control); got != ch.ControlCRC {
		t.Errorf("absCRC32(ControlBlock) = %#08x, want %#08x", got, ch.ControlCRC)
	}
}
