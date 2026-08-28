package absdb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des" //nolint:gosec // DES is required to read files written with it.
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/blowfish" //nolint:staticcheck // deprecated, but required to read files written with it
)

// CryptoAlgorithm identifies the encryption algorithm (TABSCryptoAlgorithm).
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

// String returns the algorithm name as used by the Absolute Database UI.
func (a CryptoAlgorithm) String() string {
	names := [...]string{
		"Rijndael-128", "Rijndael-256", "DES-Single", "DES-Triple",
		"Blowfish", "Twofish-128", "Twofish-256", "Square",
	}

	if int(a) >= len(names) {
		return fmt.Sprintf("CryptoAlgorithm(%d)", byte(a))
	}

	return names[a]
}

const (
	// cryptoHeaderOffset is where TABSCryptoHeader starts (after TABSDBHeader).
	cryptoHeaderOffset = 76

	// cryptoHeaderSize is the packed size of TABSCryptoHeader (280 bytes).
	cryptoHeaderSize = 280

	// controlBlockSize is the size of the ControlBlock within the CryptoHeader.
	controlBlockSize = 256

	// cryptoModeCTS is the only TCipherMode observed in real files (DEC cmCTS).
	cryptoModeCTS = 0

	// ivFillByte is the value the initial cipher feedback register is filled
	// with (DEC's InitEnd). It is 0xFF, not zero, and is reset for every page.
	ivFillByte = 0xFF
)

// ErrWrongPassword indicates the provided password does not match.
var ErrWrongPassword = errors.New("absdb: incorrect password")

// ErrUnsupportedCipher indicates an unsupported encryption algorithm.
var ErrUnsupportedCipher = errors.New("absdb: unsupported encryption algorithm")

// ErrUnsupportedCipherMode indicates an unsupported TCipherMode.
var ErrUnsupportedCipherMode = errors.New("absdb: unsupported cipher mode")

// ErrNoPassword indicates that an encrypted database was accessed without a password.
var ErrNoPassword = errors.New("absdb: database is encrypted but no password was supplied")

// CryptoHeader holds the parsed TABSCryptoHeader from page 0.
type CryptoHeader struct {
	HeaderSize   int16
	Algorithm    CryptoAlgorithm
	Mode         byte
	ControlBlock [controlBlockSize]byte
	ControlCRC   uint32
}

// CryptoHeader returns the parsed crypto header, or nil if the file is not encrypted.
func (db *File) CryptoHeader() *CryptoHeader {
	return db.cryptoHeader
}

// parseCryptoHeader reads the TABSCryptoHeader from the first page.
func parseCryptoHeader(page0 []byte) *CryptoHeader {
	off := cryptoHeaderOffset
	if len(page0) < off+cryptoHeaderSize {
		return nil
	}

	ch := &CryptoHeader{
		HeaderSize: int16(binary.LittleEndian.Uint16(page0[off : off+2])),
		Algorithm:  CryptoAlgorithm(page0[off+2]),
		Mode:       page0[off+3],
	}
	copy(ch.ControlBlock[:], page0[off+4:off+4+controlBlockSize])
	ch.ControlCRC = binary.LittleEndian.Uint32(page0[off+4+controlBlockSize : off+4+controlBlockSize+4])

	return ch
}

// keyHash selects the password hash used to derive the key for an algorithm.
type keyHash int

const (
	hashRipeMD128 keyHash = iota
	hashRipeMD256
)

// cipherSpec describes how to turn a password into a working block cipher.
type cipherSpec struct {
	hash    keyHash
	keySize int
	// newBlock builds the block cipher; nil means the algorithm is known but
	// not implementable with the ciphers available to this package.
	newBlock func(key []byte) (cipher.Block, error)
}

// cipherSpecs maps TABSCryptoAlgorithm to its key derivation and cipher.
//
// The hash choice per algorithm was recovered from ABSDiskEngine.dcu; it is not
// derivable from the public DEC headers. Blowfish using RIPEMD-256 rather than
// RIPEMD-128 is the non-obvious entry.
var cipherSpecs = map[CryptoAlgorithm]cipherSpec{
	CryptoRijndael128: {hashRipeMD128, 16, newAESCipher},
	CryptoRijndael256: {hashRipeMD256, 32, newAESCipher},
	CryptoDESSingle:   {hashRipeMD128, 8, newDESCipher},
	CryptoDESTriple:   {hashRipeMD128, 16, newTripleDESCipher},
	CryptoBlowfish:    {hashRipeMD256, 32, newBlowfishCipher},
	CryptoTwofish128:  {hashRipeMD128, 16, nil},
	CryptoTwofish256:  {hashRipeMD256, 32, nil},
	CryptoSquare:      {hashRipeMD128, 16, nil},
}

func newAESCipher(key []byte) (cipher.Block, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: aes: %w", err)
	}

	return block, nil
}

func newDESCipher(key []byte) (cipher.Block, error) {
	block, err := des.NewCipher(key) //nolint:gosec // reading legacy files
	if err != nil {
		return nil, fmt.Errorf("absdb: des: %w", err)
	}

	return block, nil
}

// newTripleDESCipher builds two-key Triple DES (K1, K2, K1) from the 16-byte
// digest, which is how a 16-byte key is fed to a 24-byte 3DES key schedule.
//
// Untested: no encrypted fixture uses DES-Triple.
func newTripleDESCipher(key []byte) (cipher.Block, error) {
	full := make([]byte, 0, 24)
	full = append(full, key...)
	full = append(full, key[:8]...)

	block, err := des.NewTripleDESCipher(full) //nolint:gosec // reading legacy files
	if err != nil {
		return nil, fmt.Errorf("absdb: 3des: %w", err)
	}

	return block, nil
}

func newBlowfishCipher(key []byte) (cipher.Block, error) {
	block, err := blowfish.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: blowfish: %w", err)
	}

	return block, nil
}

// deriveKey hashes the password to produce the encryption key for algo.
//
// The password is hashed as raw AnsiString (Windows-1252) bytes and the digest
// is truncated to the cipher's key size. Returns nil for an unknown algorithm.
func deriveKey(algo CryptoAlgorithm, password string) []byte {
	spec, ok := cipherSpecs[algo]
	if !ok {
		return nil
	}

	if spec.hash == hashRipeMD256 {
		digest := ripemd256Sum([]byte(password))

		return digest[:spec.keySize]
	}

	digest := ripemd128Sum([]byte(password))

	return digest[:spec.keySize]
}

// newCipherBlock builds the block cipher for algo from an already derived key.
func newCipherBlock(algo CryptoAlgorithm, key []byte) (cipher.Block, error) {
	spec, ok := cipherSpecs[algo]
	if !ok || spec.newBlock == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCipher, algo)
	}

	if len(key) != spec.keySize {
		return nil, fmt.Errorf("absdb: %s needs a %d-byte key, got %d", algo, spec.keySize, len(key))
	}

	return spec.newBlock(key)
}

// decryptCTS decrypts src into dst using DEC's cmCTS mode.
//
// Despite the name, this is not NIST ciphertext stealing. DEC's CTS is CBC with
// an XOR-accumulated feedback register:
//
//	full block i:      P_i = D(C_i) XOR F_i,  F_{i+1} = C_i XOR F_i
//	trailing partial:  P   = C XOR E(F)
//
// The feedback register starts as ivFillByte repeated to the block size and is
// reset for every unit of encryption (every page, and the ControlBlock).
//
// dst may alias src exactly, which is how pages are decrypted in place.
func decryptCTS(block cipher.Block, dst, src []byte) {
	blockSize := block.BlockSize()
	feedback := bytes.Repeat([]byte{ivFillByte}, blockSize)
	ciphertext := make([]byte, blockSize)
	plaintext := make([]byte, blockSize)

	full := len(src) - len(src)%blockSize

	for i := 0; i < full; i += blockSize {
		// src and dst may alias, so keep C_i before it is overwritten.
		copy(ciphertext, src[i:i+blockSize])
		block.Decrypt(plaintext, ciphertext)

		for j := range blockSize {
			dst[i+j] = plaintext[j] ^ feedback[j]
			feedback[j] ^= ciphertext[j]
		}
	}

	// The trailing partial block is masked with the encrypted feedback register.
	if rest := len(src) - full; rest > 0 {
		block.Encrypt(plaintext, feedback)

		for j := range rest {
			dst[full+j] = src[full+j] ^ plaintext[j]
		}
	}
}

// checkPassword decrypts the ControlBlock and compares its CRC with the stored
// one. It returns nil if the password is correct, ErrWrongPassword if it is
// not, and ErrUnsupportedCipher or ErrUnsupportedCipherMode if the file cannot
// be decrypted at all.
func (db *File) checkPassword(password string) error {
	if !db.encrypted || db.cryptoHeader == nil {
		return nil
	}

	if db.cryptoHeader.Mode != cryptoModeCTS {
		return fmt.Errorf("%w: %d", ErrUnsupportedCipherMode, db.cryptoHeader.Mode)
	}

	key := deriveKey(db.cryptoHeader.Algorithm, password)

	block, err := newCipherBlock(db.cryptoHeader.Algorithm, key)
	if err != nil {
		return err
	}

	control := make([]byte, controlBlockSize)
	decryptCTS(block, control, db.cryptoHeader.ControlBlock[:])

	if absCRC32(control) != db.cryptoHeader.ControlCRC {
		return ErrWrongPassword
	}

	return nil
}

// VerifyPassword reports whether the given password is correct for this
// encrypted database. It returns false for unencrypted files and for files
// whose cipher this package cannot use; Unlock distinguishes those cases.
func (db *File) VerifyPassword(password string) bool {
	if !db.encrypted || db.cryptoHeader == nil {
		return false
	}

	return db.checkPassword(password) == nil
}

// Unlock verifies the password and installs the derived key, so that
// subsequent page reads are decrypted. It is a no-op for unencrypted files.
func (db *File) Unlock(password string) error {
	if !db.encrypted || db.cryptoHeader == nil {
		return nil
	}

	err := db.checkPassword(password)
	if err != nil {
		return err
	}

	db.decryptionKey = deriveKey(db.cryptoHeader.Algorithm, password)

	return nil
}

// decryptPayload decrypts a page payload in place using the file's derived key.
//
// The unit of encryption is the whole page payload — pageSize-diskPageHeaderSize
// bytes, spanning from this block into the first diskPageHeaderOffset bytes of
// the next one — not just the tail of the block the page starts in. For the
// usual 4096-byte page that is 4056 bytes, which for a 16-byte cipher is 253
// full blocks plus an 8-byte partial block.
func (db *File) decryptPayload(payload []byte) error {
	if db.cryptoHeader == nil {
		return ErrNoPassword
	}

	if db.cryptoHeader.Mode != cryptoModeCTS {
		return fmt.Errorf("%w: %d", ErrUnsupportedCipherMode, db.cryptoHeader.Mode)
	}

	block, err := newCipherBlock(db.cryptoHeader.Algorithm, db.decryptionKey)
	if err != nil {
		return err
	}

	decryptCTS(block, payload, payload)

	return nil
}
