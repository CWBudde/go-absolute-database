# Provenance and repository hygiene

## Legal basis

Three things carry this work, and they are separate claims.

**The format is not protected by copyright.** In Case C-406/10, _SAS Institute v World
Programming_, the CJEU held that neither the functionality of a computer program nor the format
of the data files it uses in order to exploit certain of its functions constitutes a form of
expression protected by copyright. Everything in [format/](format/) is a description of facts
about `.abs` files, written in our own words: no vendor declaration is reproduced verbatim
anywhere in these documents.

**Studying and decompiling for interoperability is permitted.** EU Directive 2009/24/EC Art. 5(3)
(observation, study and testing) and Art. 6 (decompilation for interoperability) — in German law
§69d(3) and §69e UrhG — allow it, and Art. 8 / §69g(2) UrhG void contractual terms purporting to
forbid it. So the ComponentAce EULA's anti-reverse-engineering clause is very likely
unenforceable against this work.

**Redistribution is a separate matter that Art. 6 does not cover**, and the answer here is simply
that there is nothing of theirs to redistribute. See below.

Art. 6(2)(c) also limits passing decompilation results to others to what is necessary for the
interoperability of the independently created program. That is why the format reference is part
of this library's source tree, documenting the code that sits beside it, rather than published
as a standalone specification.

## What is not here

The ComponentAce SDK — the compiled units, the C++Builder headers, DBManager's Delphi source, the
help files, the sample binaries and the EULAs — **lives outside this repository**. By convention
it is a sibling directory, and the documents here refer to it as `<sdk>`:

```
Code/
├── go-absolute-database/     this repository
└── absolute-database-sdk/    <sdk>, extracted from the vendor's package, never committed
```

It has never been committed: `git log --all --diff-filter=A` over the whole history contains no
path under `legacy/`, and no `.dcu`, `.pas`, `.hpp`, `.chm`, `.bpl`, `.exe` or `.dll`. EULA §7
forbids distributing "the Software or any of its parts", and the way this repository complies is
by not containing any of them, rather than by relying on an ignore rule to hide them. The
`legacy/` line in `.gitignore` and the `legacy/**` line in `treefmt.toml` are kept purely as a
safety net against someone extracting an SDK into the tree; nothing may depend on them, and no
document may describe `<sdk>` as living inside the repository.

Anyone reproducing the fixture-generation or verification work in [testing.md](testing.md) needs
their own licensed copy of the SDK.

DEC's `Cipher.pas`/`Cipher1.pas` must not be vendored either. The ciphers are independent Go
implementations written from the published algorithm descriptions; what came from DEC is the
knowledge of where it deviates and the self-test vectors that pin those deviations. See
[format/encryption.md](format/encryption.md) and the [`NOTICE`](../NOTICE) file.

## Attribution

[`NOTICE`](../NOTICE) carries three things and must be kept current:

1. **zlib.** `internal/zlib1` is an altered port of zlib 1.2.13's `deflate.c` and `trees.c`. The
   zlib licence requires that altered versions be plainly marked as such and that the notice
   travel with the source, so the Gailly/Adler copyright appears both in `NOTICE` and in the
   package doc comment on `internal/zlib1`. If that package is ever rewritten from scratch, the
   notice still stays until the last line of zlib-derived structure is gone.
2. **DEC.** DEC 3.0 Part I is Copyright (c) Hagen Reddmann, on the terms "freeware, but this
   Copyright must be included". The notice is carried in `NOTICE` and at the top of `rijndael.go`,
   `twofish.go`, `tdes.go` and `square.go`.
3. **ComponentAce trademarks**, with an explicit statement that this project is independent and
   unaffiliated. `README.md` repeats it, because that is what a visitor reads first.

## The open question

EULA §4 says:

> You may not develop a component, a library or a developer's toolkit using this Software without
> the prior written permission of ComponentAce.

Art. 8 / §69g(2) does **not** neutralise this one: it voids terms contrary to Arts. 5(2)–(3) and
6, and a general covenant not to build a library is not within that scope. German trade-secret
law points the same way — §3(1) Nr. 2 GeschGehG permits reverse engineering of a lawfully
obtained product, but subject to any contractual restriction on acquiring the secret.

Whether that clause binds this project depends on a fact no document here settles: **which of the
seven EULA variants in the SDK package, if any, was actually accepted.** If no Absolute Database
licence was ever taken out — `.abs` files reaching us because customers' own installations wrote
them — then no contract was formed and the analysis reduces to copyright, where _SAS v WPL_
governs and the position is comfortable.

This is the one item on which a lawyer, or written permission from ComponentAce, is worth more
than further reasoning in this file. Record the answer here when it is known.

## What must never be committed

`testdata/` is gitignored except for an explicit filename allowlist in `.gitignore`. Never widen
it with a glob — a glob could catch a customer file.

The 20 customer fixtures derived from SoundPlan project files contain German street addresses
and real project data. They are **irreplaceable**: not in git, not recoverable. Nothing may
write to, move, rename or delete anything under `testdata/` or `<sdk>`; a test that needs to
modify a fixture copies it into `t.TempDir()` first.

Extracted BLOBs are customer payload and are written `0o600`.

## Committed fixtures

The 40 committed `.abs` files were produced by this project under the ComponentAce DB Manager,
with invented table names and invented values. Before staging a new one, run the content scan
`testdata/README.md` documents and confirm that only the file signature, `ABSP`, the invented
names and the invented values appear.
