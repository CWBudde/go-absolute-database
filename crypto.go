package absdb

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"hash/crc32"
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

const (
	// cryptoHeaderOffset is where TABSCryptoHeader starts (after TABSDBHeader).
	cryptoHeaderOffset = 76

	// cryptoHeaderSize is the packed size of TABSCryptoHeader (280 bytes).
	cryptoHeaderSize = 280

	// controlBlockSize is the size of the ControlBlock within the CryptoHeader.
	controlBlockSize = 256
)

// ErrWrongPassword indicates the provided password does not match.
var ErrWrongPassword = errors.New("absdb: incorrect password")

// ErrUnsupportedCipher indicates an unsupported encryption algorithm.
var ErrUnsupportedCipher = errors.New("absdb: unsupported encryption algorithm")

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

// VerifyPassword checks whether the given password is correct for this encrypted database.
// It decrypts the ControlBlock and compares its CRC32 with the stored value.
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

// deriveKey hashes the password using RIPEMD-128 to produce the encryption key.
func deriveKey(algo CryptoAlgorithm, password string) []byte {
	// DEC library: password bytes → RIPEMD-128 hash → 16-byte key
	// For AES-256/Twofish-256: would need RIPEMD-256 (32 bytes) — not yet implemented.
	h := ripemd128Sum([]byte(password))
	return h[:]
}

// decryptCBC decrypts ciphertext using AES-CBC with a zero IV.
// Used for both ControlBlock verification and page decryption.
func decryptCBC(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("absdb: ciphertext not block-aligned")
	}

	iv := make([]byte, aes.BlockSize) // zero IV
	mode := cipher.NewCBCDecrypter(block, iv)

	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	return plaintext, nil
}
