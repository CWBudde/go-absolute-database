package absdb

import (
	"bytes"
	"crypto/cipher"
	"fmt"
)

// encryptCTS encrypts src into dst using DEC's cmCTS mode. It is the exact
// inverse of decryptCTS.
//
// As on the decrypt side this is not NIST ciphertext stealing but CBC with an
// XOR-accumulated feedback register:
//
//	full block i:      C_i = E(P_i XOR F_i),  F_{i+1} = C_i XOR F_i
//	trailing partial:  C   = P XOR E(F)
//
// The trailing partial block is handled by the identical expression on both
// sides, because there the feedback register is used as a stream mask and
// XOR is its own inverse.
//
// The feedback register starts as ivFillByte repeated to the block size and is
// reset for every unit of encryption (every page, and the ControlBlock), so a
// page can be re-encrypted without knowing anything about its neighbours.
//
// dst may alias src exactly, which is how a page payload is encrypted in
// place; P_i is therefore XOR-folded into a scratch buffer before dst is
// written.
func encryptCTS(block cipher.Block, dst, src []byte) {
	blockSize := block.BlockSize()
	feedback := bytes.Repeat([]byte{ivFillByte}, blockSize)
	plaintext := make([]byte, blockSize)
	ciphertext := make([]byte, blockSize)

	full := len(src) - len(src)%blockSize

	for i := 0; i < full; i += blockSize {
		// src and dst may alias, so fold P_i XOR F_i into scratch space before
		// dst[i:] is overwritten.
		for j := range blockSize {
			plaintext[j] = src[i+j] ^ feedback[j]
		}

		block.Encrypt(ciphertext, plaintext)

		for j := range blockSize {
			dst[i+j] = ciphertext[j]
			feedback[j] ^= ciphertext[j]
		}
	}

	// The trailing partial block is masked with the encrypted feedback register.
	if rest := len(src) - full; rest > 0 {
		block.Encrypt(ciphertext, feedback)

		for j := range rest {
			dst[full+j] = src[full+j] ^ ciphertext[j]
		}
	}
}

// encryptPayload encrypts a page payload in place using the file's derived key.
//
// It is the inverse of decryptPayload and operates on the same unit: the whole
// page payload — pageSize-diskPageHeaderSize bytes, spanning from this block
// into the first diskPageHeaderOffset bytes of the next one. Feeding it a
// payload that decryptPayload produced reconstructs the original ciphertext
// byte for byte, which is what makes a page rewritable in place.
//
// The guards mirror decryptPayload exactly: a file with no crypto header was
// never unlocked, and a cipher mode other than cmCTS is one this package can
// neither read nor write.
func (db *File) encryptPayload(payload []byte) error {
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

	encryptCTS(block, payload, payload)

	return nil
}
