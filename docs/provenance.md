# Provenance and repository hygiene

## Legal basis

Reverse engineering for interoperability is permitted under EU Directive 2009/24/EC Art. 6, and
Art. 8 voids contractual terms that purport to forbid it, so the ComponentAce EULA's
anti-reverse-engineering clause is very likely unenforceable here.

**Redistribution is a separate matter that Art. 6 does not cover.** Accordingly:

- `legacy/` — the ComponentAce SDK, binaries, help files and EULAs — is gitignored and has never
  been committed. EULA §7 forbids distributing "the Software or any of its parts". Ideally it
  moves outside the repository tree so that no ignore rule is load-bearing.
- DEC's `Cipher.pas`/`Cipher1.pas` must not be vendored. The ciphers are reimplemented in Go
  from what those files describe; see [format/encryption.md](format/encryption.md).
- Before any public release, form a deliberate view on the EULA clause "You may not develop a
  component, a library or a developer's toolkit using this Software", which Art. 8 does **not**
  neutralise, and add a `NOTICE` attributing the format to ComponentAce.

## What must never be committed

`testdata/` is gitignored except for an explicit filename allowlist in `.gitignore`. Never widen
it with a glob — a glob could catch a customer file.

The 20 customer fixtures derived from SoundPlan project files contain German street addresses
and real project data. They are **irreplaceable**: not in git, not recoverable. Nothing may
write to, move, rename or delete anything under `testdata/` or `legacy/`; a test that needs to
modify a fixture copies it into `t.TempDir()` first.

Extracted BLOBs are customer payload and are written `0o600`.

## Committed fixtures

The 40 committed `.abs` files were produced by this project under the ComponentAce DB Manager,
with invented table names and invented values. Before staging a new one, run the content scan
`testdata/README.md` documents and confirm that only the file signature, `ABSP`, the invented
names and the invented values appear.
