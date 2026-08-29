# Encryption

`ABSCipher` is **DEC — the Delphi Encryption Compendium** (Hagen Reddmann), which is open
source. The class tree in `ABSCipher.hpp` (`TCipher_Rijndael`, `TCipher_Blowfish`,
`TCipher_1DES`, `THash_RipeMD128`, `THash_RipeMD256`,
`TCipherMode { cmCTS, cmCBC, cmCFB, cmOFB, cmECB, ... }`) identifies it unambiguously, so the
algorithms come from public source rather than from decompilation.

## `TABSCryptoHeader` — file offset 76, always plaintext

| Offset | Size | Type      | Field              | Notes                              |
| ------ | ---- | --------- | ------------------ | ---------------------------------- |
| 76     | 2    | int16 LE  | `CryptoHeaderSize` | 280                                |
| 78     | 1    | byte      | `CryptoAlgorithm`  | `TABSCryptoAlgorithm`, 0..7        |
| 79     | 1    | byte      | `CryptoMode`       | 0 = `cmCTS`                        |
| 80     | 256  | byte[256] | `ControlBlock`     | encrypted; used to verify password |
| 336    | 4    | uint32 LE | `ControlBlockCRC`  | checksum of the decrypted block    |

`TABSDBHeader.Encrypted` at offset 43 is `0xFF` when the database is encrypted.

## The scheme

1. **Key** = `RIPEMD-128(password)` or `RIPEMD-256(password)` over the raw AnsiString bytes,
   truncated to the cipher's key size.
2. **IV / initial feedback** = `0xFF` repeated to the block size (DEC `InitEnd`), reset for
   every page. Not a zero IV.
3. **Mode** = DEC `cmCTS`: `P_i = D(C_i) XOR F_i`, `F_{i+1} = C_i XOR F_i`; trailing partial
   block `P = C XOR E(F)`.
4. **Encrypted region** = the whole page payload, `PageSize - 40` bytes, spanning from
   `[420, PageSize)` of this block into `[0, 380)` of the next. Page 0 is never encrypted, and
   every page's `ABSP` header stays in the clear.
5. **Is this page encrypted?** `ABSP.CRC32 != 0`.
6. **Password verification** = decrypt `ControlBlock`, compute CRC32 with the reflected IEEE
   polynomial `0xEDB88320`, **init 0, no final XOR** — not Go's `crc32.ChecksumIEEE`, which
   inverts both ends — and compare against `ControlBlockCRC`.

## Algorithm → hash mapping

| Value | Cipher       | Hash           | Key bytes |
| ----- | ------------ | -------------- | --------- |
| 0     | Rijndael-128 | RIPEMD-128     | 16        |
| 1     | Rijndael-256 | **RIPEMD-256** | 32        |
| 2     | DES-Single   | RIPEMD-128     | 8         |
| 3     | DES-Triple   | RIPEMD-128     | 16        |
| 4     | Blowfish     | **RIPEMD-256** | 32        |
| 5     | Twofish-128  | RIPEMD-128     | 16        |
| 6     | Twofish-256  | **RIPEMD-256** | 32        |
| 7     | Square       | RIPEMD-128     | 16        |

Blowfish using RIPEMD-**256** is the non-obvious entry.

## DEC's ciphers are not the published ciphers

`ABSCipher.pas` is a fork of DEC 3.0, and **three of its eight ciphers deviate from the
algorithms they are named after**. Each deviation is reproduced deliberately and pinned by
DEC's own compiled-in self-test vector. **Do not "fix" any of them** — every affected file
would stop decrypting.

### Twofish — a `shr`/`shl` typo in the key schedule

The two q permutations and the MDS table are byte-for-byte the published ones, but the subkey
loop reads

```pascal
SubKey[I shl 1] := A + B;
B := A + B shr 1;
SubKey[I shl 1 + 1] := ROL(B, 9);
```

where the specification calls for `K_{2i+1} = ROL(A + 2B, 9)`. Delphi binds `shr` tighter than
`+`, so this computes `A + (B shr 1)`, which changes every odd subkey.
`golang.org/x/crypto/twofish` therefore cannot read these files.

### Rijndael-256 — a key schedule that is not AES-256

Diffing `TCipher_Rijndael.Init` against the standard AES expansion, word by word:

```
128-bit key (10 rounds)  schedules identical
192-bit key (12 rounds)  schedules identical
256-bit key (14 rounds)  diverge at expanded word 12 -- DEC 0x516823b3, AES 0xcda85116
```

The `FRounds = 14` branch chains all eight key words (`for I := 1 to 7 do K[I] := K[I] xor
K[I-1]`), _then_ XORs `SubWord(K[3])` into the already-chained `K[4]`, _then_ re-chains
`K[5..7]`. Standard AES-256 applies `SubWord` to `w[i-1]` and XORs with `w[i-8]` without that
extra chaining. The round function itself is ordinary AES.

So `crypto/aes` is **correct for `Rijndael_128` and wrong for `Rijndael_256`**.
`CryptoRijndael128` uses `crypto/aes`, which is byte-identical there and both faster and
constant-time; `rijndael.go` implements DEC's schedule for the 256-bit case.

### DES-Triple — `TCipher_3TDES`, plus a swap typo

Not `TCipher_3DES`, and not two-key EDE over an 8-byte block:

- **24-byte block**, not 8 (`GetContext` overrides `ABufSize := 24`).
- Keyed with the 16-byte RIPEMD-128 digest **zero-extended to 24**, so the third DES key is all
  zeros.
- EDE across three 8-byte sub-blocks with a word swap between stages, and that swap contains a
  typo:

```pascal
T := PIntArray(Data)[3]; PIntArray(Data)[3] := PIntArray(Data)[4]; PIntArray(Data)[3] := T;
```

It assigns to word 3 twice and never to word 4, so only words 1 and 2 are actually exchanged.
Reproducing the typo matches the stored ControlBlock CRC; correcting the swap does not.

### Square — no deviation

DEC's `TCipher_Square` is faithful to the 1997 Daemen/Knudsen/Rijmen cipher: the S-box, the
diffusion matrix (the circulant of `2,1,1,3` over GF(2^8) modulo `$1F5` — not AES's `$11B`)
and the key schedule all match the specification. It is in-tree only because no Go
implementation of Square exists anywhere.

## Self-test vectors

DEC compiles a 32-byte self-test vector into each cipher class. `TCipher.SelfTest` keys the
cipher with the **class-name string**, zero-padded to `KeySize`, and encrypts DEC's 32-byte
`GetTestVector` plaintext in `cmCTS` with `IVector = nil`, so the feedback register starts at
`E(0xFF...)` rather than `0xFF...`:

```
plaintext 3044ED6E45A496F5F635A2EB3D1A5DD6CB1D09822DBDF560C2B858A191F981B1

TCipher_Rijndael  946D2B5EE0AD1B5CA523A513958B3D2D9387F3374551F6589BE7901B3687F9A9
TCipher_Twofish   A5535703EF3348799F22B4549705841987BD831C4DAE1213607C7CD198450219
TCipher_3TDES     0B12E48BD9CD08BFCAAE3E5FF6FE13CD3F706ECD53563F5A800F1B1EFB9A5796
TCipher_Square    439CA6C467E82E472295668506396AC9182120F74436F1617D1490B1A96856C7
```

**These are not in `ABSCipher.dcu`.** ComponentAce replaced every DEC `TestVector` asm stub
with one that raises `"TCipher_Square.TestVector not implemented 27"`, so byte-scanning the
Rio-era `.dcu`, `.bpl` or `DBManager.exe` returns nothing. The vectors survive in DEC 3.0's own
`Cipher.pas`/`Cipher1.pas`, and in the 2011-vintage `legacy/Utils/Bin/DBImportExport.exe`,
where VMT slot 10 of each cipher class still points at the original constant (Square at file
offset `0xD78B6`, Twofish at `0xD687A`). The two sources agree byte for byte, and the Blowfish
vector reproduces with stdlib Blowfish, which is what proves the harness rather than the
ciphers.

## Writing encrypted files

A modified page is re-checksummed over the new plaintext and re-encrypted. `writePageBuf`
refuses the one payload in 2^32 whose checksum comes out zero, because a zero `ABSP.CRC32`
means "this page is in the clear".

**Creating** an encrypted database is refused (`ErrEncryptionUnsupported`): the 260 bytes of
key material at header offsets 80..339 are located but undecoded, and a guess produces a file
the engine will not open.
