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
	// newBlock builds the block cipher. Every algorithm the format defines now
	// has one, so no entry is nil; the field is still allowed to be nil, and
	// newCipherBlock still guards against it, so that a future algorithm can be
	// listed here with its key derivation before its cipher exists.
	newBlock func(key []byte) (cipher.Block, error)
}

// cipherSpecs maps TABSCryptoAlgorithm to its key derivation and cipher.
//
// The hash choice per algorithm was recovered from ABSDiskEngine.dcu; it is not
// derivable from the public DEC headers. Blowfish using RIPEMD-256 rather than
// RIPEMD-128 is the non-obvious entry.
var cipherSpecs = map[CryptoAlgorithm]cipherSpec{
	CryptoRijndael128: {hashRipeMD128, 16, newAESCipher},
	CryptoRijndael256: {hashRipeMD256, 32, newRijndaelCipher},
	CryptoDESSingle:   {hashRipeMD128, 8, newDESCipher},
	CryptoDESTriple:   {hashRipeMD128, 16, newTripleDESCipher},
	CryptoBlowfish:    {hashRipeMD256, 32, newBlowfishCipher},
	CryptoTwofish128:  {hashRipeMD128, 16, newTwofishCipher},
	CryptoTwofish256:  {hashRipeMD256, 32, newTwofishCipher},
	CryptoSquare:      {hashRipeMD128, 16, newSquareCipher},
}

func newAESCipher(key []byte) (cipher.Block, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: aes: %w", err)
	}

	return block, nil
}

// newRijndaelCipher builds DEC's Rijndael from the derived 32-byte key.
//
// crypto/aes is deliberately still used for CryptoRijndael128: DEC's key
// schedule coincides with AES for 128- and 192-bit keys, so the stdlib is
// byte-identical there and is both faster and constant-time. It diverges for
// 256-bit keys, which is why that one algorithm needs the in-tree
// implementation. See rijndael.go.
func newRijndaelCipher(key []byte) (cipher.Block, error) {
	block, err := newRijndael(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: rijndael: %w", err)
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

// newTripleDESCipher builds DEC's TCipher_3TDES from the derived key.
//
// This is not two-key EDE Triple DES over an 8-byte block, which is what the
// name suggests and what this package assumed before a fixture existed. It is
// DEC's TCipher_3TDES: a 24-byte block, keyed with the 16-byte RIPEMD-128
// digest zero-extended to 24 so the third DES key is all zeros. See tdes.go,
// including the swap typo that has to be reproduced.
func newTripleDESCipher(key []byte) (cipher.Block, error) {
	block, err := newTripleDES(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: 3tdes: %w", err)
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

// newTwofishCipher builds Twofish from the derived key. The key length selects
// the variant, exactly as in DEC: 16 bytes give Twofish-128, 32 bytes
// Twofish-256. DEC's TCipher_Twofish declares a KeySize of 32 for both, but it
// keys the cipher from the number of bytes actually passed to Init rather than
// zero-padding to KeySize — the same TCipher.InitBegin path that makes a
// 16-byte key produce plain AES-128 for Rijndael-128, which the
// Addresses-Rijndael_128.abs fixture confirms.
//
// This is *not* reference Twofish; see twofish.go for the one-line deviation in
// DEC's key schedule and why golang.org/x/crypto/twofish cannot be used here.
func newTwofishCipher(key []byte) (cipher.Block, error) {
	block, err := newTwofish(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: twofish: %w", err)
	}

	return block, nil
}

// newSquareCipher builds Square from the 16-byte RIPEMD-128 digest of the
// password, the only key size the algorithm allows and the one the
// algorithm-to-hash mapping supplies for CryptoSquare.
//
// Square is DEC-specific: golang.org/x/crypto has no Square at all, so unlike
// Blowfish or AES there was never an off-the-shelf implementation to reach for.
// See square.go for the port of DEC 3.0's TCipher_Square.
func newSquareCipher(key []byte) (cipher.Block, error) {
	block, err := newSquare(key)
	if err != nil {
		return nil, fmt.Errorf("absdb: square: %w", err)
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
