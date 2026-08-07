# decode-oracle

Names the aarch64 encodings elflift could not decode, and extracts their operand
fields, using QEMU's decodetree tables.

Two front ends over one library:

```sh
cd tools/decode-oracle
go run ./cmd/decode-report -enc 0x4c9f7000            # name encodings, with their fields
go run ./cmd/decode-report -log ../../.agents-workspace/fixtures/undecoded/undec4.log
go run ./cmd/decode-report -corpus ../../.agents-workspace/fixtures/postgres-glibc.fused
```

and `cmd/decode-mcp`, the same three capabilities over MCP's stdio transport --
because the tool's real consumer is a coding agent working through the coverage
debt, not a human at a prompt. Registered for this repo in `.mcp.json`:

```json
{"mcpServers": {"decode-oracle": {
  "command": "go", "args": ["run", "./cmd/decode-mcp"], "cwd": "tools/decode-oracle"
}}}
```

| MCP tool | CLI equivalent |
|---|---|
| `decode_encoding` | `-enc` |
| `decode_report` | `-log` |
| `decode_corpus` | `-corpus` |

Both front ends render `decode.LookupEncodings` and the same report writers, so
an answer cannot differ between them -- a wrong answer is wrong in both or
neither.

⚠️ **stdout belongs to the protocol.** MCP requires a stdio server to write
nothing to stdout that is not a valid message, and messages must not contain
embedded newlines. `internal/mcp` therefore never calls the decode package's
`os.Stdout`-writing entry points -- it renders into a buffer -- and marshals
with `json.Marshal`, never an indenting encoder. `TestNoEmbeddedNewlines` guards
this with a multi-line report payload, and fails if either rule is broken.

## What it is for

elfconv's aarch64 decoder is incomplete, and closing the gaps is raptormark's
dominant ongoing tax. elflift reports every rejected instruction as
`[ecv-undecoded] vma=... enc=... fn=...` (elfconv patch 0057), but that names
encodings, not instructions.

Two cheaper routes are recorded as dead ends in `.agents/docs/TODO.md`: matching
elfconv's stub decoders by **mnemonic** yields 83 false positives because
objdump prints aliases (`cmp` for SUBS, `mov` for ORR, `mov` for SVE SEL), and
hand-narrowing to encoding groups missed `usubw` and `cmlt` at half an hour per
retry. **The unit of truth is the raw encoding.**

So this matches encodings against QEMU's declarative tables and reports the
pattern, the resolved `fixedmask`/`fixedbits`, and every operand:

```
enc=0x4c9f7000  sites=112  funcs=31  first-vma=0x4a1234
  objdump: st1 {v0.16b}, [x0], #16
  ST_mult @ldst_mult &ldst_mult  (a64.decode:598)
    fixedmask=0xbf60f000 fixedbits=0x0c007000
    q=1 p=1 rm=31 sz=0 rn=0 rt=0 rpt=1 selem=1
```

## Two modes

| flag | what it does |
|---|---|
| `-log` | the worklist: group `[ecv-undecoded]` records by encoding, rank by site count, name each |
| `-corpus` | the differential: decode every executable word of a fused ELF with both the tables and objdump, report disagreement |

⚠️ Harvesting a log needs `RAPTORMARK_TRANSLATE_VERBOSE=1` **and a cold
`RAPTORMARK_OBJECT_CACHE`**. A cache hit means no translate ran, so the report
comes out empty for a reason that has nothing to do with this tool.

## Gate

The repo-root gate in `.agents/docs/QUALITY_GATE.md` covers this module by
naming its module path; bare `./...` from the root does **not**, because a
workspace does not make relative patterns recursive across modules. To gate it
on its own, or to confirm it still builds standalone (`GOWORK=off`):

```sh
cd tools/decode-oracle
gofmt -l .
go build ./...
go vet ./...
go test ./...
```

And after re-vendoring `third_party/qemu-decode/`, the differential against one
glibc and one musl fixture — they share almost no assumptions, and musl ships no
SVE at all:

```sh
RAPTORMARK_DECODE_CORPUS=$PWD/../../.agents-workspace/fixtures/postgres-glibc.fused \
  go test ./internal/decode/ -run TestCorpusAgreesWithObjdump -v
```

Measured at QEMU v11.1.0, binutils 2.42: 100.0000% (busybox-musl, bash-glibc),
99.9985% (aptget-glibc), 99.9990% (postgres-glibc). The residue is SME. The
assertion floor is 99.9%, deliberately below the measurements, so it catches a
pin that has lost whole families rather than freezing a number that moves with
binutils.

## Caveats worth knowing before reading a report

- **A mnemonic is not a decodetree pattern.** `st1` splits across `ST_mult` and
  `ST_single`; `usubw` 8 is one `USUBW` pattern that objdump renders as `usubw`
  4 + `usubw2` 4, discriminated by `q`. The report prints both views.
- **`enc=0x00000000` is padding lifted as code**, not an instruction. It
  dominates a raw grep. The report excludes it from the totals and reports it
  separately, because it doubles as a control: if it moves between two runs,
  function-boundary recovery moved and not just instruction coverage.
- **This names; it carries no semantics.** A named family with correct field
  extraction is still a patch someone has to write and test against a native
  oracle. `TODO.md`'s standing warning holds: do not respond by implementing a
  whole encoding group — `FRECPE`, `FRSQRTE`, `URECPE` and `URSQRTE` are
  reciprocal estimates whose results are hard to verify, and an unverifiable
  approximation in crypto arithmetic is worse than a loud `__ecv_warning`.

## Why this is a separate module

**Licensing, and it is the whole reason for the directory.**

The vendored tables (`third_party/qemu-decode/`) are **LGPL-2.1-or-later**.
raptormark's lifter lineage — `third_party/elfconv`, remill, LLVM — is
**Apache-2.0**, which is incompatible with **GPLv2 and LGPLv2.1 specifically**:
Apache's patent-termination and indemnity clauses read as "further restrictions"
under LGPLv2.1 §10. That conflict does **not** extend to LGPLv3, which was
written to be Apache-compatible, and the `-or-later` grant on both tables puts
v3 within reach. The door is narrow, not locked — do not read the incompatibility
as broader than it is.

raptormark itself has no declared licence yet, and embedding LGPL material in
`cmd/raptormark` would have silently constrained that decision, forcing either an
LGPLv3 election or a v2.1 conflict onto the pipeline before anyone chose. That is
what the split avoids.

Keeping the oracle in its own module means the obligation attaches only to a
developer-only analysis tool:

- It is **never built into, linked with, or shipped alongside** the pipeline.
- It reads a log or an ELF and prints a report. Nothing it touches reaches a
  translated object or `module.wasm`.
- A nested `go.mod` keeps it out of the parent module, so nothing in
  `cmd/raptormark` can import it even by accident. The repo-root gate reaches it
  by module path (`raptormark/tools/decode-oracle/...`) via `go.work`, which
  changes which packages a gate VISITS and not what anything links.

It is also deliberately **dependency-free** (stdlib only -- no kong, and no MCP
SDK), so the smallest possible surface carries the obligation and the module
builds offline forever. The MCP server is hand-rolled for that reason: the stdio
transport is newline-delimited JSON-RPC 2.0, and the slice needed to serve three
tools is `initialize`, `notifications/initialized`, `tools/list`, `tools/call`
and `ping`. That is the same argument the main module's `internal/oci/blob.go`
already makes for hand-rolling OCI JSON -- "what raptormark emits is a narrow
slice of the spec". That is a knowing departure from `AGENTS.md`'s "keep new commands
consistent with the kong layout" rule, which is about `cmd/raptormark/`.

❌ Do not fold this back into the main module for convenience.

## Licence

This module is **LGPL-2.1-or-later**, matching the tables it embeds. `LICENSE`
is that text; every `.go` file here carries a copyright line and an
`SPDX-License-Identifier` line.

That last claim is enforced by `TestEveryGoFileCarriesTheLicenceHeader`, which
walks from the **module root** rather than from its own package. It is a test
and not a note in this file because it is a claim that decays: it was true of
all 12 `.go` files when written and false an hour later, when the MCP server
arrived with five more.

Chosen over the alternative — electing LGPLv3 for the tables and keeping the Go
code Apache-2.0 — because `//go:embed` links the tables into the binary
statically, so a distributed binary is one combined work under either choice. A
single licence across the module removes the question instead of managing it,
and the module is dependency-free, so nothing else constrains the pick. Revisit
only if the decodetree parser is ever worth spinning out on its own; it is a
clean-room Go implementation of a grammar with no other Go implementation, so
that is not far-fetched.

**The obligation stops at this directory.** Nothing here is linked into
`cmd/raptormark`, and the root module's licence remains an open and
unconstrained decision — which was the point of the split.

Upstream material under `third_party/qemu-decode/` keeps its own file headers
and is never edited in place. See that directory's `PROVENANCE.md`.

Copyright is asserted as **The raptormark Authors**, matching the root `NOTICE`.
Same holder as the root module, different licence — which is what a per-module
licence means, and is entirely ordinary. The root `NOTICE` carries a scope note
saying this directory is not covered by its Apache-2.0 grant; keep the two in
agreement if either moves.


