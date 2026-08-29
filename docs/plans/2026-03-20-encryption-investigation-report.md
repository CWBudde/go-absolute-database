# Phase 6 Encryption Investigation Report

**Date:** 2026-03-20
**Status:** Superseded — kept as the record of how the format was worked out.
**Password used for all test files:** "Bla"

> **Resolved.** Encryption was solved on 2026-08-28;
> [`docs/format/encryption.md`](../format/encryption.md) is the current reference. This document is a snapshot of the investigation while it was still
> open, so its open questions and negative results are of historical interest only.
>
> Corrections to what is written below:
>
> - **"Key derivation unsolved"** — solved. `TCipher.InitKey` hashes the password and
>   truncates to `min(Hash.DigestKeySize, FKeySize)`, so the algorithm-to-hash mapping
>   alone fixes the key length.
> - **"What buffer region does ABSInternalEncryptBuffer operate on?"** — the page
>   payload: `pageSize - 40` bytes, running from this block into the first 380 bytes of
>   the next. The `ABSP` header itself stays in the clear.
> - **"Post-ABSP data is NOT encrypted for page types 5 and 7"** — an artifact of the old
>   page model. Encryption is not conditional on page type; a zero `ABSP.CRC32` marks a
>   page that was never encrypted.
> - The recorded `ABSP.CRC32` mismatch was the 3676-byte value. `ABSP.CRC32` is
>   `absCRC32` over the decrypted 4056-byte payload.
> - Resolving go-dede's fixup symbols was never needed; see
>   [`docs/format/encryption.md`](../format/encryption.md) §"DEC's ciphers are not the published
>   ciphers" for what settled it.

---

## What We Know (Confirmed)

### File Header (TABSDBHeader, offset 0, 76 bytes)

- Byte 43: `Encrypted` flag (`0x00` = no, `0xFF` = yes)
- Already parsed in `absdb.go`

### CryptoHeader (TABSCryptoHeader, offset 76, 280 bytes, packed)

| Offset | Size | Field                                      | Example (Blowfish) |
| ------ | ---- | ------------------------------------------ | ------------------ |
| 76     | 2    | CryptoHeaderSize (int16 LE) = 280          | `0x0118`           |
| 78     | 1    | CryptoAlgorithm (enum)                     | `0x04` = Blowfish  |
| 79     | 1    | CryptoMode                                 | `0x00`             |
| 80     | 256  | ControlBlock (encrypted verification data) | (random bytes)     |
| 336    | 4    | ControlBlockCRC (uint32 LE)                | `0x5A2F73E6`       |
| 340    | 16   | Reserved                                   | zeros              |

### CryptoAlgorithm Enum (TABSCryptoAlgorithm)

| Value | Name               | Block Size | DEC MaxKeySize |
| ----- | ------------------ | ---------- | -------------- |
| 0     | Rijndael-128 (AES) | 16 bytes   | 32 bytes       |
| 1     | Rijndael-256       | 16 bytes   | 32 bytes       |
| 2     | DES-Single         | 8 bytes    | 8 bytes        |
| 3     | DES-Triple         | 8 bytes    | 24 bytes       |
| 4     | Blowfish           | 8 bytes    | 56 bytes       |
| 5     | Twofish-128        | 16 bytes   | 32 bytes       |
| 6     | Twofish-256        | 16 bytes   | 32 bytes       |
| 7     | Square             | 16 bytes   | 16 bytes       |

### Per-Page ABSP Header (TABSDiskPageHeader)

- Located at offset `0x17C` (380 bytes) within every page
- 40 bytes total
- **Never encrypted** — ABSP signature always readable in the clear
- The `CipherType` byte (offset +20 within ABSP) is set to mark encrypted pages
- The `State` and `CRC32` fields may change during encryption (metadata update)

### Encryption Boundaries (Per Page)

Confirmed by byte-level comparison of encrypted vs unencrypted Addresses.abs:

```
Page layout (4096 bytes):
┌──────────────────────────────────────────┐
│ 0x000-0x17B  Pre-ABSP area  (380 bytes)  │ ← ENCRYPTED (plaintext = all zeros)
├──────────────────────────────────────────┤
│ 0x17C-0x1A3  ABSP header    (40 bytes)   │ ← NOT encrypted (clear text)
├──────────────────────────────────────────┤
│ 0x1A4-0xFFF  Page data      (3676 bytes) │ ← ENCRYPTED
└──────────────────────────────────────────┘

Combined encrypted region: 380 + 3676 = 4056 bytes
  = 507 × 8 (Blowfish/DES block-aligned)
  = 253 × 16 + 8 (AES: 253 full blocks + 8 byte remainder → needs CTS)
```

Page 0 is an exception: only the page data portion is encrypted; the DB header, CryptoHeader, and LockedBytes areas remain in the clear.

Empty (all-zero) pages are NOT encrypted.

### Cipher Mode

The DEC (Delphi Encryption Compendium) library's CTS mode is NOT standard CBC. From DEC v3 source code (`Cipher.pas`):

```pascal
// DEC CTS Encryption (per block):
D := S XOR Feedback;        // XOR plaintext with feedback
Encode(D);                   // Block cipher encrypt in-place
Feedback := D XOR Feedback;  // Feedback = ciphertext XOR old_feedback
```

Standard CBC uses `Feedback := Ciphertext`. DEC CTS uses `Feedback := Ciphertext XOR old_feedback` (accumulated XOR).

Decryption (derived):

```
P := Decrypt(C) XOR Feedback
Feedback_new := C XOR Feedback_old
```

### Per-Page Independence

Pages 10 and 11 (identical plaintext) produce **identical ciphertext** in all three encrypted test files. This confirms:

- Same key for all pages
- Same initial feedback/IV state for all pages
- No per-page nonce or counter

### Password Verification (Hypothesized)

The ControlBlock (256 bytes) is encrypted with the same cipher and key. The ControlBlockCRC is presumably the CRC32 of the decrypted ControlBlock. To verify a password:

1. Derive key from password
2. Decrypt ControlBlock
3. CRC32(decrypted) should equal ControlBlockCRC

---

## What We Tried (All Failed)

### Key Derivation Approaches

| Approach                       | Hash       | Key Size | Result |
| ------------------------------ | ---------- | -------- | ------ |
| RIPEMD-128(password)           | RIPEMD-128 | 16 bytes | ❌     |
| MD4(password)                  | MD4        | 16 bytes | ❌     |
| MD5(password)                  | MD5        | 16 bytes | ❌     |
| RIPEMD-128 + zero pad to 32    | RIPEMD-128 | 32 bytes | ❌     |
| RIPEMD-128 + zero pad to 56    | RIPEMD-128 | 56 bytes | ❌     |
| Raw password "Bla\0"           | none       | 4 bytes  | ❌     |
| Raw password padded to 8/16/56 | none       | various  | ❌     |
| Password repeated to 56 bytes  | none       | 56 bytes | ❌     |
| UTF-16LE password bytes        | RIPEMD-128 | 16 bytes | ❌     |
| Password with null terminator  | RIPEMD-128 | 16 bytes | ❌     |

### Cipher Mode Approaches

| Mode                   | Feedback Init                     | Result |
| ---------------------- | --------------------------------- | ------ |
| DEC CTS (XOR feedback) | Zero IV                           | ❌     |
| DEC CTS                | InitKey feedback (encrypt digest) | ❌     |
| Standard CBC           | Zero IV                           | ❌     |
| ECB                    | n/a                               | ❌     |
| CFB                    | Zero IV                           | ❌     |
| OFB                    | Zero IV                           | ❌     |

### Endianness

- Blowfish with LE word swap: ❌
- AES with LE word swap: ❌
- DEC source confirmed Blowfish uses `SwapInteger` (BSWAP) → standard big-endian processing

### RIPEMD-128 Implementation

- All 7 official test vectors pass ✓
- Implementation verified correct

---

## What Remains Unknown

### 1. Key Derivation Function

The DEC v3 `InitKey` method hashes the password with RIPEMD-128 and passes the digest to `Init`. But the ABSCipher.pas used by ComponentAce may use a **different key derivation**:

- Different hash (not RIPEMD-128)
- Salted hash
- Iterated/stretched hash (PBKDF-like)
- Custom key schedule modification
- Password preprocessed before hashing

### 2. Initial Feedback State

The DEC `InitKey` method encrypts the hash digest after `Init`, building up feedback state. The actual initial feedback for data encryption could be:

- Zero (if cipher is reinitialized)
- The accumulated feedback from encrypting the digest
- Something derived from the CryptoHeader or page metadata

### 3. Per-Page Feedback / IV

**Critical finding (session 3):** The recovered per-page feedback values (computed as `D_ECB(first_encrypted_block)` since plaintext is zeros) DIFFER between pages:

| Page | Feedback (D_ECB of first block) | Page Type |
| ---- | ------------------------------- | --------- |
| 3    | 403E0D74209EEE24                | 5         |
| 5    | 4811EDF2FE5B7C63                | 7         |
| 8    | A39C5485F8817E4B                | 9         |
| 9    | 82EF52F1EB1D9E59                | 12        |
| 10   | E13EEBE070BE9B97                | 12        |
| 11   | E13EEBE070BE9B97                | 12        |
| 12   | 4C54BCE2DEC45C9E                | 12        |

Pages 10+11 share the SAME feedback and have IDENTICAL pre-ABSP ciphertext (all 380 bytes). Their post-ABSP ciphertext DIFFERS because their post-ABSP plaintext differs by 1 byte.

The feedback is NOT: zero, RIPEMD-128 hash, InitKey accumulated state, page number, page type, or any ABSP header field tested. In the unencrypted file, pre-ABSP is confirmed all-zeros for all pages 1-12.

**Hypotheses:**

- The pre-ABSP area stores a per-page random nonce generated during encryption (not encrypted data)
- The encryption reorders processing (e.g., ABSP first, then pre-ABSP, then post-ABSP)
- The InternalWritePage method adds per-page metadata before calling ABSInternalEncryptBuffer

### 4. DEC v3 CTS vs ABSCipher CTS

We've verified that DEC v5 CTSx mode ("double CBC") is NOT what ABSCipher uses. We implemented DEC v3 CTS manually (XOR-accumulated feedback) using DEC v5's ECB block cipher. The block cipher output matches perfectly between Go, FPC, and the DCU analysis. But neither DEC v3 CTS nor DEC v5 CTSx, with any feedback state (zero, InitKey, sequential), reproduces the encrypted file output.

The ABSCipher.dcu CTS implementation (confirmed in the switch case bodies) matches the DEC v3 source pattern exactly. Yet something in the overall pipeline — the InitKey feedback preservation, the Done/Protect behavior, or the page-level orchestration — produces a different result than our standalone tests.

**Tested and failed:**

- DEC v3 CTS with zero feedback, encrypting individual pages
- DEC v3 CTS with InitKey-accumulated feedback
- DEC v3 CTS sequential (encrypting ALL pages in order, carrying feedback)
- DEC v3 CTS sequential (skipping empty pages)
- DEC v5 CTSx (all variations above)
- Page-number-based IV
- First-8-bytes-as-nonce theory

### 5. ControlBlock Content

We don't know what the original plaintext of the ControlBlock is (random bytes? known constant? hash?), or exactly how the CRC32 verification works.

---

## Test Files Available

| File                                  | Encrypted | Algorithm        | Password |
| ------------------------------------- | --------- | ---------------- | -------- |
| `testdata/Addresses.abs`              | No        | n/a              | n/a      |
| `testdata/Addresses-Rijndael_128.abs` | Yes       | Rijndael-128 (0) | "Bla"    |
| `testdata/Addresses-Blowfish.abs`     | Yes       | Blowfish (4)     | "Bla"    |
| `testdata/Addresses-DES_Single.abs`   | Yes       | DES-Single (2)   | "Bla"    |

Original unencrypted source: `../Aconiq/interoperability/Schienenprojekt - Schall 03/Addresses.abs`

---

## DCU Disassembly Analysis (via go-dede)

Used `go-dede decompile --asm` on `ABSCipher.dcu` (Delphi XE12) and `ABSDiskEngine.dcu`.

### Confirmed: InitKey uses RIPEMD-128

The `InitKey` procedure (identified by its 22-call, 220-line signature and virtual call pattern) matches the DEC v3 source exactly:

```pascal
procedure TCipher.InitKey(const Key: String; IVector: Pointer);
begin
  Hash.Init;                                    // call [vtable+0x20]
  Hash.Calc(PAnsiChar(Key)^, Length(Key));       // call [vtable+0x24]
  Hash.Done;                                    // call [vtable+0x28]
  I := Hash.DigestKeySize;                      // call [vtable+0x30]
  if I > FKeySize then I := FKeySize;           // cmp [Self+0x20]
  Init(Hash.DigestKey^, I, IVector);            // call [vtable+0x2C]
  EncodeBuffer(Hash.DigestKey^, ..., DigestKeySize);
  Done;                                         // call [vtable+0x38]
  SetFlag(0, True);                             // call [vtable+0x1C]
end;
```

### Confirmed: CTS mode matches DEC v3

The EncodeBuffer CTS case (extracted from the jump table case code) matches:

```pascal
cmCTS:
  XORBuffers(S, FFeedback, FBufSize, D);         // D = S XOR Feedback
  Encode(D);                                      // D = E(D)
  XORBuffers(D, FFeedback, FBufSize, FFeedback);  // Feedback = D XOR Feedback
```

Field offsets confirmed: `[Self+0x24]=FBufSize`, `[Self+0x2C]=FBuffer`, `[Self+0x30]=FVector`, `[Self+0x34]=FFeedback`.

### Confirmed: GetContext values

| Proc                        | BufSize | KeySize | UserSize | Cipher     |
| --------------------------- | ------- | ------- | -------- | ---------- |
| TCipher_Rijndael.GetContext | 16      | 32      | 480      | Rijndael   |
| TCipher_Blowfish.GetContext | 8       | 56      | 4168     | Blowfish   |
| TCipher_1DES.GetContext     | 8       | 8       | 256      | DES-Single |
| TCipher_3DES.GetContext     | 8       | 24      | 768      | DES-Triple |
| TCipher_Twofish.GetContext  | 16      | 32      | 4256     | Twofish    |
| TCipher_Square.GetContext   | 16      | 16      | 288      | Square     |

### Discovery: ABSInternalEncryptBuffer calls SetHashClass then InitKey again

From `ABSDiskEngine.dcu`, each case in the switch on CryptoAlgorithm does:

```
1. Create(Password, nil)          — calls InitKey with DEFAULT hash
2. SetHashClass(specific_hash)    — changes hash class (UNRESOLVED RELOCATION)
3. InitKey(Password, FVector)     — re-runs key derivation with NEW hash
4. EncodeBuffer(inBuf, inBuf, Size)
5. Free
```

The hash class set by SetHashClass is an unresolved DCU relocation — we cannot determine which of THash_RipeMD128, THash_RipeMD256, or THash_MD4 is used for each cipher algorithm.

### FreePascal Validation (DEC v5)

Compiled DEC v5 (docroger/DEC fork) with FreePascal 3.2.2 on Linux x86-64.

**ECB mode matches Go implementation perfectly:**

- `ECB Blowfish(key=RIPEMD128("Bla"), zeros)` = `F697BCDFC87C9166` ✓ (same in Go)
- `RIPEMD-128("Bla")` = `5F05EDCFA8677C4F72E831885C5A1FF9` ✓ (same in Go and PHP)

**DEC v5 CTSx mode does NOT match the encrypted files:**

- DEC v5 CTSx uses "double CBC" with non-trivial IV derivation when no IV is provided
- This is fundamentally different from DEC v3's CTS mode
- ComponentAce ABSCipher is based on DEC v3, not v5

---

## Code Written So Far

- `ripemd128.go` + `ripemd128_test.go` — RIPEMD-128 hash (7 test vectors pass)
- `crypto.go` — CryptoHeader parsing, CryptoAlgorithm enum, VerifyPassword stub, decryptCBC
- `crypto_test.go` — Test framework for password verification
- `absdb.go` — CryptoHeader field added to File struct, OpenWithPassword stub, ReadPage with decryption hook

---

## Recommended Next Steps

1. **Resolve the SetHashClass relocation** — The DCU fixup records should contain the target class reference for each SetHashClass call in ABSInternalEncryptBuffer. Enhancing go-dede's fixup resolution to show which hash class is used per cipher algorithm would immediately solve the mystery.

2. **Port DEC v3 to FPC** — DEC v3 source (luizvaz/DelphiEncryptionCompendium) uses x86 BASM which doesn't compile on x86-64 FPC. Either:
   - Install `fpc-i386` cross-compiler for 32-bit compilation
   - Port the x86 ASM blocks to pure Pascal (significant effort — 31 ASM blocks in Cipher.pas, 27 in DECUtil.pas, 54 in Hash.pas)

3. **Known-plaintext key recovery for DES** — For DES-Single (56-bit effective key), we have known plaintext/ciphertext pairs. A brute-force search of 2^56 DES keys is feasible with optimized code (days on a modern CPU, hours on GPU). The recovered key would reveal the exact key derivation regardless of which hash is used.

4. **Ask ComponentAce** — Contact the vendor for encryption documentation or ABSCipher.pas source code.

5. **Obtain ABSCipher.pas source** — Check if the registered Absolute Database SDK includes source code for ABSCipher.pas.

6. **Disassemble ABSCipher TCipher.Done and TCipher.Protect** — The go-dede DCU analysis confirmed InitKey calls Done after EncodeBuffer(digest). Whether Done zeros the feedback (DEC v3 behavior) or preserves it determines the cipher state for subsequent operations. Using go-dede's `--depth` flag from InitKey to trace Done→Protect would reveal whether FFeedback is zeroed.

7. **Trace InternalWritePage encryption flow** — The go-dede call-chain investigation found `proc_49` (encrypt-and-pack helper) and `proc_72` (InternalWritePage) but the exact page-level orchestration — how the cipher is initialized, whether feedback is reset per page, and what buffer is passed to ABSInternalEncryptBuffer — needs further tracing. The new `--depth` and intra-DCU annotation features in go-dede should help.
