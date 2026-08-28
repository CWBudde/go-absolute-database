# testdata

Almost everything in this directory is **deliberately not committed**. The `.abs`
fixtures the parser is developed against are real customer project data, and
`.gitignore` excludes `testdata/*` for that reason. A fresh clone therefore has only the
`Employees-*` fixtures below, and every test that needs one of the others skips (see
`requireFixture` in `absdb_test.go`). A green CI run does **not** mean the parser was
validated against real customer files — run `just test` locally for that.

## The committed fixtures

The seven `Employees-*.abs` files are the exception, one per encryption algorithm. They
are committed because they are the only fixtures CI can see: without them the encryption
code is never exercised on a runner at all.

All seven hold the identical database and differ only in the cipher:

|          |                                                                                         |
| -------- | --------------------------------------------------------------------------------------- |
| Password | `Bla`                                                                                   |
| Table    | `Employees` — `Id` INTEGER, `Name` VARCHAR(20), `Salary` FLOAT, `Active` BOOLEAN        |
| Rows     | `(1, 'Ada', 1234.5, True)`, `(2, 'Grace', 2345.75, False)`, `(3, 'Kurt', 999.25, True)` |
| Index    | `IdxId` on `Id`                                                                         |
| Origin   | Created 2026-08-28 with the ComponentAce Absolute Database Manager                      |

| File                         | `TABSCryptoAlgorithm` | Cipher                                        |
| ---------------------------- | --------------------- | --------------------------------------------- |
| `Employees-Rijndael_128.abs` | 0                     | AES-128 (DEC's schedule coincides with AES)   |
| `Employees-Rijndael_256.abs` | 1                     | DEC's Rijndael — **not** AES-256, see PLAN.md |
| `Employees-DES_Single.abs`   | 2                     | DES                                           |
| `Employees-DES_Triple.abs`   | 3                     | DEC's `TCipher_3TDES`, 24-byte block          |
| `Employees-Twofish_128.abs`  | 5                     | DEC's Twofish variant                         |
| `Employees-Twofish_256.abs`  | 6                     | DEC's Twofish variant                         |
| `Employees-Square.abs`       | 7                     | Square                                        |

Blowfish (4) has no `Employees-` fixture; it is covered only by the empty-table
`Addresses-Blowfish.abs`, which is not committed.

They contain no customer data — the schema and all three rows are invented — and no
vendor material. This was checked rather than assumed: scanning both the ciphertext and
the decrypted payload of every file finds no path, user name, vendor string or licence
code, only the format's own `ABS0LUTEDATABASE` and `ABSP` magic plus the invented table
and row values. (`Employees-Rijndael_256.abs` happens to contain the three bytes `Z:/`
at offset 40666, which looks like a Wine drive mapping but is coincidence inside
ciphertext: it does not appear in the decrypted plaintext.) The Absolute Database licence
restricts distributing "the Software or any of its parts"; these are output files
produced by the Software, not parts of it, and carry no registration or access codes.

## Why they matter

Three of them found real bugs that no amount of reading could have:

- `Employees-Rijndael_256.abs` proved that DEC's Rijndael key schedule diverges from AES
  for 256-bit keys, so the shipped `crypto/aes` implementation was silently wrong.
- `Employees-DES_Triple.abs` proved that `DES_Triple` is `TCipher_3TDES` with a **24-byte
  block**, not two-key EDE Triple DES over 8 bytes as had been inferred.
- `Employees-Square.abs` validated the Square port end to end.

They are also the only encrypted fixtures with any rows. The three `Addresses-*` fixtures
are encrypted copies of an _empty_ table, so before these existed, encrypted _record_
decryption had never been validated for any cipher.

Because they have no plaintext twin, decryption is checked against each page's `ABSP`
checksum, which covers the decrypted payload and so is an oracle needing no twin, and
against the B-tree leaf scan in `oracle_test.go`.

## Regenerating them

They can be recreated on Linux by driving `DBManager.exe` from the SDK under Wine with a
virtual desktop. Two things make it much easier than it first appears:

- `SQL → Execute SQL Script` runs a whole semicolon-separated `.sql` file in one pass, so
  the `CREATE TABLE` / `INSERT` / `CREATE INDEX` statements above need no per-statement
  typing.
- Clearing `HKCU\Software\ComponentAce\Absolute Database\DatabaseManager\History\File1..5`
  before launch stops DBManager reopening the last database and prompting for its
  password.

Verify the result by reading the file rather than by screenshot: byte 43 is the
`Encrypted` flag and byte 78 is the algorithm, which must match the table above.
