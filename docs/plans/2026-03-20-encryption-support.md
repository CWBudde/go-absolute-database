# Phase 6: Encryption Support Implementation Plan

> **Superseded — do not implement from this document.** Phase 6 was completed on
> 2026-08-28; `PLAN.md` §"Encryption" is the current reference. This plan is kept for the
> binary-format tables below, which are still accurate. Its _architecture_ paragraph is
> not: the ABSP header is **not** encrypted, the IV is **0xFF** repeated rather than zero,
> and DEC's `cmCTS` is an XOR-accumulated feedback mode that does not degenerate to CBC.
> Twofish additionally needs `twofish.go` rather than `golang.org/x/crypto/twofish`.

**Goal:** Decrypt encrypted `.abs` files given a password, starting with AES-128 (Rijndael-128).

**Architecture:** Encryption operates at the page level. Page 0 (header) is never encrypted; all other non-empty pages are fully encrypted (including their ABSP header). The password is hashed with RIPEMD-128 to derive a 16-byte AES key. Each page is decrypted independently using AES-128-CTS mode with a zero IV (which degenerates to CBC for block-aligned data). A 256-byte ControlBlock in the CryptoHeader allows password verification before attempting page decryption.

**Tech Stack:** Go stdlib `crypto/aes`, `crypto/cipher` for AES-CBC. Custom RIPEMD-128 implementation (not in stdlib or `x/crypto`). No new external dependencies for core crypto.

---

## Binary Format Reference

### CryptoHeader (offset 76 in page 0, 280 bytes, packed)

| Offset | Size | Type      | Field                                             |
| ------ | ---- | --------- | ------------------------------------------------- |
| 0      | 2    | int16 LE  | CryptoHeaderSize (always 280)                     |
| 2      | 1    | byte      | CryptoAlgorithm (enum, see below)                 |
| 3      | 1    | byte      | CryptoMode (0=CTS, 1=CBC, ...)                    |
| 4      | 256  | bytes     | ControlBlock (encrypted verification block)       |
| 260    | 4    | uint32 LE | ControlBlockCRC (CRC32 of decrypted ControlBlock) |
| 264    | 16   | bytes     | Reserved                                          |

### CryptoAlgorithm enum (TABSCryptoAlgorithm)

| Value | Name         | Key bits | Hash for key derivation |
| ----- | ------------ | -------- | ----------------------- |
| 0     | Rijndael-128 | 128      | RIPEMD-128 (16 bytes)   |
| 1     | Rijndael-256 | 256      | RIPEMD-256 (32 bytes)   |
| 2     | DES-Single   | 64       | RIPEMD-128 (truncated)  |
| 3     | DES-Triple   | 192      | RIPEMD-128 (expanded?)  |
| 4     | Blowfish     | 448      | RIPEMD-128              |
| 5     | Twofish-128  | 128      | RIPEMD-128              |
| 6     | Twofish-256  | 256      | RIPEMD-256              |
| 7     | Square       | 128      | RIPEMD-128              |

### Encryption scheme (DEC library — Delphi Encryption Compendium)

1. Password (AnsiString raw bytes) → hash with RIPEMD-128 → 16-byte key
2. Cipher mode: CTS (Cipher Text Stealing). For block-aligned data (4096-byte pages), CTS = CBC with zero IV.
3. Each page decrypted independently with same key and zero IV.
4. Page 0 is NEVER encrypted. Empty (all-zero) pages are not encrypted.
5. ControlBlock: encrypted with the same key/mode. Decrypt it, compute CRC32, compare with ControlBlockCRC to verify password.

### DEC CTS Mode Details

DEC's CTS mode processes data in blocks. For encryption:

- Block 0: C₀ = E(P₀ ⊕ IV), feedback = C₀
- Block i: Cᵢ = E(Pᵢ ⊕ Cᵢ₋₁), feedback = Cᵢ
- Last partial block: steal bytes from previous ciphertext (not applicable for full pages)

For decryption:

- Block 0: P₀ = D(C₀) ⊕ IV
- Block i: Pᵢ = D(Cᵢ) ⊕ Cᵢ₋₁

With IV = all zeros and block-aligned data, this is standard CBC.

---

## Task 1: RIPEMD-128 Hash Implementation

**Files:**

- Create: `ripemd128.go`
- Create: `ripemd128_test.go`

RIPEMD-128 is not available in Go's stdlib or `x/crypto`. We need a minimal implementation that produces 16-byte digests. Reference: the original RIPEMD-128 paper and test vectors from https://homes.esat.kuleuven.be/~bosMDselaers/ripemd160/rmd128.txt

**Step 1: Write the failing test with known test vectors**

```go
// ripemd128_test.go
package absdb

import (
    "encoding/hex"
    "testing"
)

func TestRIPEMD128(t *testing.T) {
    // Official test vectors from RIPEMD-128 specification
    tests := []struct {
        input string
        hash  string
    }{
        {"", "cdf26213a150dc3ecb610f18f6b38b46"},
        {"a", "86be7afa339d0fc7cfc785e72f578d33"},
        {"abc", "c14a1219c3965ef04b3c723bc743b23b"},
        {"message digest", "9e327b3d6e523062afc1132d7df9d1b8"},
        {"abcdefghijklmnopqrstuvwxyz", "fd2aa607f71dc8f510714922b371834e"},
    }
    for _, tt := range tests {
        t.Run(tt.input, func(t *testing.T) {
            got := ripemd128Sum([]byte(tt.input))
            want, _ := hex.DecodeString(tt.hash)
            if !bytes.Equal(got[:], want) {
                t.Errorf("ripemd128(%q) = %x, want %s", tt.input, got, tt.hash)
            }
        })
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -run TestRIPEMD128 -v ./...`
Expected: FAIL — `ripemd128Sum` undefined

**Step 3: Implement RIPEMD-128**

Implement `func ripemd128Sum(data []byte) [16]byte` in `ripemd128.go`.

The algorithm is well-specified in the RIPEMD-128 paper. It processes 512-bit (64-byte) blocks using 4 rounds of 16 operations each, with two parallel lines of computation. Use the reference implementation constants (K values, rotation amounts, message word permutations).

Key implementation notes:

- MD-strengthening padding (same as MD4/MD5): append 0x80, pad to 56 mod 64, append 64-bit LE length
- Four 32-bit state words initialized to: h0=0x67452301, h1=0xefcdab89, h2=0x98badcfe, h3=0x10325476
- Two parallel rounds (left line and right line) with different constants
- Final combination: t = h1 + cl + dr; h1 = h2 + dl + ar; h2 = h3 + al + br; h3 = h0 + bl + cr; h0 = t

**Step 4: Run test to verify it passes**

Run: `go test -run TestRIPEMD128 -v ./...`
Expected: PASS

**Step 5: Commit**

```bash
git add ripemd128.go ripemd128_test.go
git commit -m "feat: implement RIPEMD-128 hash for encryption key derivation"
```

---

## Task 2: CryptoHeader Parsing

**Files:**

- Modify: `absdb.go` — add CryptoHeader struct and parsing to `File`
- Create: `crypto.go` — CryptoAlgorithm enum and CryptoHeader type
- Modify: `absdb_test.go` — test CryptoHeader parsing

**Step 1: Write the failing test**

```go
// In absdb_test.go
func TestCryptoHeaderEncryptedFile(t *testing.T) {
    db := openTestFile(t, "Addresses.abs")

    if !db.Encrypted() {
        t.Fatal("expected Addresses.abs to be encrypted")
    }

    ch := db.CryptoHeader()
    if ch == nil {
        t.Fatal("expected non-nil CryptoHeader for encrypted file")
    }

    if ch.Algorithm != CryptoRijndael128 {
        t.Errorf("algorithm = %d, want CryptoRijndael128 (0)", ch.Algorithm)
    }

    if ch.HeaderSize != 280 {
        t.Errorf("header size = %d, want 280", ch.HeaderSize)
    }
}

func TestCryptoHeaderUnencryptedFile(t *testing.T) {
    db := openTestFile(t, "TS03.abs")

    if db.Encrypted() {
        t.Fatal("TS03.abs should not be encrypted")
    }

    ch := db.CryptoHeader()
    if ch != nil {
        t.Error("expected nil CryptoHeader for unencrypted file")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -run TestCryptoHeader -v ./...`
Expected: FAIL — `CryptoHeader()` undefined

**Step 3: Implement CryptoHeader parsing**

In `crypto.go`:

```go
package absdb

// CryptoAlgorithm identifies the encryption algorithm.
type CryptoAlgorithm byte

const (
    CryptoRijndael128 CryptoAlgorithm = 0
    CryptoRijndael256 CryptoAlgorithm = 1
    CryptoDESSingle   CryptoAlgorithm = 2
    CryptoDESTriple   CryptoAlgorithm = 3
    CryptoBlowfish    CryptoAlgorithm = 4
    CryptoTwofish128  CryptoAlgorithm = 5
    CryptoTwofish256  CryptoAlgorithm = 6
    CryptoSquare      CryptoAlgorithm = 7
)

// cryptoHeaderOffset is where TABSCryptoHeader starts (immediately after TABSDBHeader).
const cryptoHeaderOffset = 76

// cryptoHeaderSize is the packed size of TABSCryptoHeader (280 bytes).
const cryptoHeaderSize = 280

// CryptoHeader holds the parsed TABSCryptoHeader from page 0.
type CryptoHeader struct {
    HeaderSize   int16
    Algorithm    CryptoAlgorithm
    Mode         byte
    ControlBlock [256]byte
    ControlCRC   uint32
}
```

In `absdb.go`, add to File struct:

```go
cryptoHeader *CryptoHeader // nil if not encrypted
```

Add `CryptoHeader()` accessor and parse in `parseHeader()` when `encrypted == true`.

**Step 4: Run test to verify it passes**

Run: `go test -run TestCryptoHeader -v ./...`
Expected: PASS

**Step 5: Commit**

```bash
git add crypto.go absdb.go absdb_test.go
git commit -m "feat: parse CryptoHeader from encrypted database files"
```

---

## Task 3: Password Verification via ControlBlock

**Files:**

- Modify: `crypto.go` — add password verification function
- Create: `crypto_test.go` — test password verification

**Step 1: Write the failing test**

```go
// crypto_test.go
package absdb

import "testing"

func TestVerifyPassword(t *testing.T) {
    db := openTestFile(t, "Addresses.abs")

    // The file was encrypted with password "bla" or "Bla"
    // Try both — one must work
    blaOK := db.VerifyPassword("bla")
    BlaOK := db.VerifyPassword("Bla")

    if !blaOK && !BlaOK {
        t.Fatal("neither 'bla' nor 'Bla' verified as correct password")
    }

    // Log which one worked
    if blaOK {
        t.Log("password is 'bla'")
    } else {
        t.Log("password is 'Bla'")
    }

    // Wrong password must fail
    if db.VerifyPassword("wrong") {
        t.Error("wrong password should not verify")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -run TestVerifyPassword -v ./...`
Expected: FAIL — `VerifyPassword` undefined

**Step 3: Implement password verification**

The verification algorithm:

1. Derive key from password using RIPEMD-128
2. Decrypt the 256-byte ControlBlock using AES-128-CBC with zero IV
3. Compute CRC32 of the decrypted result
4. Compare with stored ControlBlockCRC

In `crypto.go`:

```go
func (db *File) VerifyPassword(password string) bool {
    if !db.encrypted || db.cryptoHeader == nil {
        return false
    }
    key := deriveKey(db.cryptoHeader.Algorithm, password)
    decrypted, err := decryptCBC(key, db.cryptoHeader.ControlBlock[:])
    if err != nil {
        return false
    }
    crc := crc32.ChecksumIEEE(decrypted)
    return crc == db.cryptoHeader.ControlCRC
}

func deriveKey(algo CryptoAlgorithm, password string) []byte {
    h := ripemd128Sum([]byte(password))
    return h[:]
}

func decryptCBC(key, ciphertext []byte) ([]byte, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, err
    }
    iv := make([]byte, aes.BlockSize) // zero IV
    mode := cipher.NewCBCDecrypter(block, iv)
    plaintext := make([]byte, len(ciphertext))
    mode.CryptBlocks(plaintext, ciphertext)
    return plaintext, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test -run TestVerifyPassword -v ./...`
Expected: PASS — one of the passwords verifies

**Step 5: Commit**

```bash
git add crypto.go crypto_test.go
git commit -m "feat: verify password against encrypted database ControlBlock"
```

---

## Task 4: OpenWithPassword API and Transparent Page Decryption

**Files:**

- Modify: `absdb.go` — add `OpenWithPassword`, modify `ReadPage` to decrypt
- Modify: `crypto.go` — add page decryption
- Modify: `crypto_test.go` — test reading encrypted pages

**Step 1: Write the failing test**

```go
// In crypto_test.go
func TestOpenWithPassword(t *testing.T) {
    path := testdataPath("Addresses.abs")

    // Opening without password should fail on encrypted file
    db, err := Open(path)
    if err != nil {
        t.Fatalf("Open should succeed even for encrypted files: %v", err)
    }
    db.Close()

    // OpenWithPassword with correct password
    db, err = OpenWithPassword(path, "bla") // or "Bla" — adjust after Task 3
    if err != nil {
        t.Fatalf("OpenWithPassword: %v", err)
    }
    defer db.Close()

    // Should be able to read schema
    schema, err := db.Schema()
    if err != nil {
        t.Fatalf("Schema: %v", err)
    }

    // Addresses.abs has 12 columns (known from unencrypted version)
    if len(schema.Columns) == 0 {
        t.Error("expected columns in schema")
    }
    t.Logf("schema: %d columns", len(schema.Columns))
    for _, col := range schema.Columns {
        t.Logf("  %s (%v, size=%d)", col.Name, col.Type, col.Size)
    }
}

func TestOpenWithWrongPassword(t *testing.T) {
    path := testdataPath("Addresses.abs")
    _, err := OpenWithPassword(path, "wrong")
    if err == nil {
        t.Fatal("expected error for wrong password")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -run TestOpenWith -v ./...`
Expected: FAIL — `OpenWithPassword` undefined

**Step 3: Implement OpenWithPassword and page decryption**

In `absdb.go`:

```go
var ErrWrongPassword = errors.New("absdb: incorrect password")

func OpenWithPassword(path, password string) (*File, error) {
    db, err := Open(path)
    if err != nil {
        return nil, err
    }
    if !db.encrypted {
        return db, nil // not encrypted, password ignored
    }
    if !db.VerifyPassword(password) {
        db.Close()
        return nil, ErrWrongPassword
    }
    db.decryptionKey = deriveKey(db.cryptoHeader.Algorithm, password)
    return db, nil
}
```

Add to File struct:

```go
decryptionKey []byte // nil if not encrypted or no password provided
```

Modify `ReadPage` to decrypt non-zero, non-empty pages:

```go
func (db *File) ReadPage(n int) (Page, error) {
    // ... existing read logic ...

    // Decrypt if needed (page 0 is never encrypted, empty pages are not encrypted)
    if n > 0 && db.decryptionKey != nil && !page.IsEmpty() {
        decrypted, err := decryptCBC(db.decryptionKey, data)
        if err != nil {
            return Page{}, fmt.Errorf("absdb: decrypting page %d: %w", n, err)
        }
        page.Data = decrypted
    }

    // Parse header AFTER decryption
    page.Header = parseDiskPageHeader(page.Data)
    return page, nil
}
```

**Important:** Move the `parseDiskPageHeader` call to AFTER decryption, since the ABSP header is encrypted.

**Step 4: Run test to verify it passes**

Run: `go test -run TestOpenWith -v ./...`
Expected: PASS — schema readable from encrypted file

**Step 5: Commit**

```bash
git add absdb.go crypto.go crypto_test.go
git commit -m "feat: add OpenWithPassword with transparent page decryption"
```

---

## Task 5: Full Integration Test — Read Records from Encrypted File

**Files:**

- Modify: `crypto_test.go` — test reading actual records

**Step 1: Write the test**

```go
func TestReadRecordsFromEncryptedFile(t *testing.T) {
    path := testdataPath("Addresses.abs")
    db, err := OpenWithPassword(path, "bla") // adjust password after Task 3
    if err != nil {
        t.Fatalf("OpenWithPassword: %v", err)
    }
    defer db.Close()

    reader, err := db.NewReader()
    if err != nil {
        t.Fatalf("NewReader: %v", err)
    }

    count := 0
    for reader.Next() {
        rec := reader.Record()
        // Just verify we can read without panic/error
        _ = rec
        count++
    }
    if err := reader.Err(); err != nil {
        t.Fatalf("reader error: %v", err)
    }

    t.Logf("read %d records from encrypted Addresses.abs", count)
    if count == 0 {
        t.Error("expected at least one record")
    }
}
```

**Step 2: Run test**

Run: `go test -run TestReadRecordsFromEncrypted -v ./...`
Expected: PASS — records readable

**Step 3: Commit**

```bash
git add crypto_test.go
git commit -m "test: verify record reading from encrypted database"
```

---

## Task 6: Error Handling for Encrypted Files Without Password

**Files:**

- Modify: `absdb.go` or `schema.go` — return clear error when reading encrypted file without password
- Modify: `crypto_test.go` — test this case

**Step 1: Write the test**

```go
func TestEncryptedFileWithoutPassword(t *testing.T) {
    db := openTestFile(t, "Addresses.abs")

    // Reading pages from encrypted file without password should return
    // encrypted data (no ABSP headers will parse)
    page, err := db.ReadPage(3) // page 3 has data
    if err != nil {
        t.Fatalf("ReadPage: %v", err)
    }

    // Without decryption key, ABSP header won't be found
    if page.Header != nil {
        t.Error("expected nil header for encrypted page without password")
    }
}
```

**Step 2: Run test, verify pass**

Run: `go test -run TestEncryptedFileWithoutPassword -v ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add crypto_test.go
git commit -m "test: verify encrypted file behavior without password"
```

---

## Task 7: Update PLAN.md

**Files:**

- Modify: `PLAN.md` — mark Phase 6 steps as complete

Mark completed:

- [x] Detect encrypted files (header flag — byte 43)
- [x] Parse CryptoHeader (algorithm, mode, ControlBlock)
- [x] Implement AES-128 decryption (Rijndael-128 with RIPEMD-128 key derivation)
- [x] Key derivation from password (RIPEMD-128 hash)
- [x] Integrate decryption into page reading (transparent to API consumers)
- [x] Add `OpenWithPassword(path, password string) (*File, error)`

Still TODO:

- [ ] AES-256 (Rijndael-256) — needs RIPEMD-256 and test data
- [ ] Blowfish, Twofish, DES, Triple DES, Square — lower priority, need test data

**Commit:**

```bash
git add PLAN.md
git commit -m "docs: update PLAN.md with Phase 6 encryption progress"
```

---

## Notes

- **RIPEMD-128 is critical path** — without it, nothing else works. The implementation must match the official test vectors exactly.
- **CTS vs CBC**: For block-aligned data (which all pages are), CTS mode degenerates to standard CBC with zero IV. We use `crypto/cipher.NewCBCDecrypter` directly.
- **No new dependencies** for core crypto — `crypto/aes` and `crypto/cipher` are stdlib. RIPEMD-128 is implemented in-tree.
- **Password is case-sensitive** — Delphi AnsiString preserves case. Task 3 will determine whether the actual password is "bla" or "Bla".
- **CRC32 for ControlBlock verification** — use `hash/crc32` with IEEE polynomial (standard CRC-32).
