# Test strategy

## Fixtures

Fixtures live in `testdata/`, which is **gitignored except for an explicit filename
allowlist** in `.gitignore`. The allowlist uses the `testdata/*` form rather than `testdata/`,
because git cannot re-include a path whose parent directory is excluded.

Two populations:

- **Committed** — 40 `.abs` files this project produced under the ComponentAce DB Manager, plus
  `testdata/zlib1` golden vectors and `testdata/fuzz` corpora. Eight `Employees-*` (one per
  encryption algorithm, all with rows), fourteen `Writes*` (the record write path's ground
  truth, five of them carrying a user index), twelve `MultiTable*` (the catalog and the schema
  operations over it), five `Empty*` (fresh databases, one setting changed each), and
  `Constraints.abs` (twelve tables, each one clause from a control).
- **Not committed** — 20 real customer files. They are irreplaceable, are not in git, and must
  never be. Nothing in the test suite may write to, move, rename or delete anything under
  `testdata/`; a test that needs to modify a fixture copies it into `t.TempDir()` first.

Every fixture-backed test must `t.Skip` when its file is missing, so a fresh clone and CI still
run green on the synthetic, unit and committed-fixture tests alone.

## Oracles

In rough order of strength. Prefer these to constants read out of this package's own earlier
output, which pin behaviour but prove nothing about correctness.

1. **Byte identity against the engine.** For write paths, the strongest available: reproduce
   the file DBManager produced for the same statement. See [writing.md](writing.md).
2. **Index cross-check.** The B-tree leaf scan is an independent decoder of the same data the
   record reader produces. Assert not only row counts but the exact set of `(PageNo, ItemNo)`
   pairs, for every fixture and every user index against every other.
3. **Decryption equality.** An encrypted fixture must decrypt byte-identically to its plaintext
   twin; where there is no twin, against the `ABSP` page checksum, which covers the decrypted
   payload.
4. **Structural invariants.** Where the data has a known shape, assert it — a fixture whose
   BLOBs are float32 triplets must satisfy `len(data) % 12 == 0`.
5. **Round-trip against the Delphi engine.** The only way to validate the field types with zero
   corpus coverage. Requires Windows or Wine plus the Absolute Database Personal Edition.

## Fuzzing

`FuzzOpen`, `FuzzParseSchema` and `FuzzReadBlob`, with a per-target budget in CI and 30 s per
target from `just fuzz`. Finding no crash in a bounded run is an absence of evidence, not a
proof of safety.

The parser must handle arbitrary byte sequences without panicking or allocating unboundedly.
Both requirements are policy, not aspiration: every error path returns an error, and both zlib
readers are bounded by [source-specific expansion ceilings](format/internal-files.md#expansion-bounds).

## Generating fixtures

`testdata/README.md` carries the recipe: drive `legacy/Utils/Bin/DBManager.exe` under Wine on
`Xvfb`, with a virtual desktop set in the prefix registry. Use **`SQL → Execute SQL Script`**
for anything longer than one statement — it turns a fixture needing a dozen statements into a
single file dialog, which is what makes a _matrix_ fixture like `Constraints.abs` as cheap as a
one-statement diff.

Two rules that are not conveniences:

- Always work on copies in a scratch directory. DBManager's file dialog opens in the last-used
  directory, which is often `testdata/`.
- Prove each derived file actually differs from its parent (`cmp -l parent child | wc -l`). A
  silently-failed statement yields a byte-identical copy that looks like a valid fixture.

Before staging anything new, run the content scan `testdata/README.md` documents — `strings -n 4
-a` and `strings -e l`, over both ciphertext and decrypted payload — and confirm only the
signature, `ABSP`, the invented table names and the invented values appear.

## CI

`.github/workflows/ci.yml` runs build, `go vet`, `gofmt`, `go mod tidy -diff`, race tests,
`golangci-lint` and a fuzz budget per target.

**CI cannot validate the parser against the customer corpus.** Those fixtures are not in the
repository, so their tests skip on a runner; the workflow prints the skip list into the job
summary and uploads no coverage number, precisely so a green tick does not imply real-file
validation. That stays a local `just test` responsibility.
