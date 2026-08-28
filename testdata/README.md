# testdata

Almost everything in this directory is **deliberately not committed**. The `.abs`
fixtures the parser is developed against are real customer project data, and
`.gitignore` excludes `testdata/*` for that reason. A fresh clone therefore has almost
no fixtures, and every test that needs one skips (see `requireFixture` in
`absdb_test.go`). A green CI run does **not** mean the parser was validated against real
files — run `just test` locally for that.

## The two committed fixtures

`Employees-Twofish_128.abs` and `Employees-Twofish_256.abs` are the exception. They are
committed because they are the only fixtures CI can see: without them the encryption
code is never exercised on a runner at all.

|          |                                                                                         |
| -------- | --------------------------------------------------------------------------------------- |
| Password | `Bla`                                                                                   |
| Table    | `Employees` — `Id` INTEGER, `Name` VARCHAR(20), `Salary` FLOAT, `Active` BOOLEAN        |
| Rows     | `(1, 'Ada', 1234.5, True)`, `(2, 'Grace', 2345.75, False)`, `(3, 'Kurt', 999.25, True)` |
| Index    | `IdxId` on `Id`                                                                         |
| Origin   | Created 2026-08-28 with the ComponentAce Absolute Database Manager                      |

They contain no customer data — the schema and all three rows are invented — and no
vendor material: the only identifiable byte sequences in them are the format's own
`ABS0LUTEDATABASE` and `ABSP` magic. The Absolute Database licence restricts
distributing "the Software or any of its parts"; these are output files produced by the
Software, not parts of it, and carry no registration or access codes.

They are also the only encrypted fixtures with any rows. The three `Addresses-*`
fixtures are encrypted copies of an _empty_ table, so before these existed, encrypted
_record_ decryption had never been validated for any cipher.

Because they have no plaintext twin, decryption is checked against each page's `ABSP`
checksum, which covers the decrypted payload and so is an oracle needing no twin, and
against the B-tree leaf scan in `oracle_test.go`.

## Regenerating them

They can be recreated on Linux by driving `DBManager.exe` from the SDK under Wine with a
virtual desktop; the SQL tab accepts the `CREATE TABLE` / `INSERT` / `CREATE INDEX`
statements above directly.
