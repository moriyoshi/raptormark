# Translation, Linking, and Object Cache

## Summary

Stages 3 and 4 translate each fused ELF into a namespaced Wasm object and link all objects with ecvisor. Cache identity and symbol namespacing are correctness mechanisms as well as performance features.

## Key Facts

- Translation and final linking execute inside an explicitly selected builder image.
- `ModuleID` is `<sanitised ELF name>_<ELF SHA-256 prefix>` and deliberately excludes builder identity.
- `TranslateID` covers toolchain identity, options, and ELF content with domain-separated length-prefixed fields.
- Namespace symbols before splitting; library-scoped partitions make reusable libraries independent of the surrounding executable.
- Schedule codegen parts largest-first so the longest work starts early.
- A failed split must fail the build, never silently fall back to a multi-hour serial compile.
- Runtime-only changes require relinking, not re-lifting.
- Shared names and library-scoped partitioning remain opt-in through `ECV_SHARED_NAMES` and `ECV_LIB_RANGES`.

## Details

`internal/link` emits `EcvProgram` descriptors and the exec map. Its field mapping was checked against both `runtime/src/abi.rs` and real `llvm-nm` output. `internal/translate` derives identity from labels on the selected builder image and content-addresses translated objects.

Plain split codegen promotes cross-part locals to hidden global symbols, causing collisions when multiple programs are linked. Preserving locals fixes collisions on tiny modules but keeps the reachable graph in one huge part. The recovered intended design namespaces every symbol per program at bitcode level, then allows ordinary splitting and parallel codegen. Post-codegen wasm tools cannot reliably rename these symbols.

The object cache hashes the base image identity, translation pipeline sources, ELF content, and codegen options. It deliberately excludes runtime-only inputs because lifted objects do not depend on them. A prior incomplete key could serve incorrect objects; regression tests now perturb lifter and runtime inputs independently.

Deterministic lifted IR was a prerequisite for reuse. `patches/0029` replaces
pointer-ordered near-jump switch construction with stable ordering. `ModuleID`
then moved to ELF identity alone so the same library code can retain names across
builder revisions; builder and patch-series identity still belongs in the
translation/cache key, not in the symbol namespace.

`ecv-split` replaced plain `llvm-split` for content-stable output. It clusters
definitions with their blockaddress companions, promotes and namespaces symbols,
and names each output from its actual membership. A definition is shareable only
when everything it references is also shared; this prevents glue such as
`__remill_intrinsics` from acquiring a shared name while still referring to a
per-program state symbol.

Fixed-count hash buckets reused well only for similarly sized programs. Different
programs put the same library functions beside different co-tenants, changing the
partition hash even when all reusable definitions matched. The durable boundary
is the library: all 21 libraries shared by PostgreSQL and initdb produced
identical lifted symbol sets. `ECV_LIB_RANGES` supplies closure-fixed library
ranges, and library partitions assign sorted definitions round-robin by
`ordinal % nchunks` with a default chunk target of 125. Contiguous block chunks
were rejected by wall-clock evidence because adjacent functions can form a very
slow partition despite modest byte size.

The PostgreSQL closure improved from 66m43s to 46m21s; marginal cost after the
first program fell from 19m11s to 5m49s. initdb codegen CPU fell 42x and dash
160x. Call-graph sharding (`patches/0030`) remains inert: trace lifting caused
80-84% duplication between sampled shards, so arbitrary entry shards are not a
cache boundary. Lifting each library once is the successor investigation.

`link.WriteLinkInputs` writes the registry and `programs.json` manifest together.
Sidecar generation consumes that manifest instead of independently deriving
module identifiers, eliminating a second source of exec-map identity drift.

The earlier exception-handling design required `wasm-opt --translate-to-exnref`; suspension by ordinary return removed that lowering.

⚠️ **`wasm-opt -O0` is NOT a pure round-trip, and an earlier reading here said
it was.** Binaryen does report "no passes specified, not doing any work", but the
module still shrinks **5.5%** (127,354,502 -> 120,298,637 on the openssl fixture)
because it re-encodes wasm-ld's padded LEBs. So identity is already refuted BY
SIZE, and the pass is a size/time trade rather than a free deletion. The escape
hatch exists: `ECV_WASM_OPT=0` (`wasmOptEnabled`) skips it entirely and
`finalise` renames the pre-module into place. It is opt-in for that reason.
❌ If this is re-raised, the answer is the 5.5% measurement, not another
`wasm-objdump` structural comparison -- that comparison was asked for to confirm
an identity that does not hold.

## Files

- `internal/translate/`: Translation requests and cache identity.
- `internal/link/`: Registry, exec map, and final-link inputs.
- `cmd/raptormark/`: `translate-one` and `link-all` commands.
- `builder/`: Builder image support files.
- `e2e/`: Full pipeline driver and cache use.

## Test Coverage

Unit tests cover identity changes, registry ABI, exec-map encoding, defaults, and symbol visibility. E2E tests link one and multiple programs and check that program objects do not collide. Cache tests confirm lifter changes invalidate objects while runtime-only changes do not.

`e2e/sharednames_test.go` uses different programs from one closure, asserts
served partition artifacts and non-empty objects, then links and runs both. Its
reuse assertion was neutralized against a pre-fix builder, while a later builder
showed that the non-empty-object assertion is independent. The legacy naming
path remains byte-identical when the opt-in flags are absent.

## Pitfalls

- Always set `RAPTORMARK_OBJECT_CACHE` for pipeline runs.
- Rebuild the Go tool after changing its libraries.
- Measure split inputs or running processes; completed `.o` files hide the slowest partitions.
- Codegen cost follows the largest function or partition, not total input size.
- A stable name-to-bucket hash does not imply stable bucket membership when program sizes differ.
- Library reuse requires closure-fixed addresses as well as stable names; the same library at a different base is different lifted code.
- Do not use the dormant `-shard` path as a production optimization.

## Consolidated Update: Merged Preparation and Per-Library Lift Caching

`ecv-prepare` now combines linking, internalization, namespacing, and stable splitting in one parse. Shared headers keep the standalone and merged decisions identical. The four passes fell from 18.45 s to 6.0 s on bash, and a partition-cache-warm translation fell from 32.51 s to 20.18 s.

Patch 0052 adds `--lift_range`. Program and library range lifts compose because the runtime sorts and searches dispatch tables by VMA. `ecv-prepare --merge` handles the guarded and unguarded table lengths, drops duplicate external definitions, temporarily coalesces identical internal definitions, then restores local linkage.

The library cache is keyed per library over executable section address, size, bytes, in-range symbol boundaries, and lifter identity. Section names are excluded because `.text.l<N>` carries a per-program slot; non-executable per-program metadata is excluded because composition discards it. Deduplicating `llvm.ident` moved N-way versus single-merge partition identity from 0 of 126 to 124 of 126.

Per-library caching is the default when a persistent cache root and `RAPTORMARK_LIB_RANGES` exist. `libCacheHostDir` prefers `RAPTORMARK_LIB_CACHE`, then the partition-cache `lib-lifts` directory, then the object-cache equivalent. `RAPTORMARK_NO_LIB_CACHE` disables it; `ECV_NO_MERGED_PREPARE` disables merged preparation. The measured second-program lift fell from 27.98 s to 10.85 s and total translation from 80.11 s to 57.09 s.

Companion C++ sources that affect output now participate in `TranslateSH`. `TestTranslateSHIsStableAndSensitive` edits every listed source independently so a file cannot be listed but silently omitted from the hash.

## Consolidated Update: Flat and Side-Module Links

One `-fPIC` object set feeds both the flat link and `link-all --side-out`. The latter emits side modules and a standalone supervisor; the development embedder has run reserve, place, relocate, register, and start for two disjoint modules. The exported descriptor global is an offset, not an address, and `--growable-table` is required. Byte-affecting experimental switches join `TranslateID`, but the identity is trustworthy only when `raptormark build-tools` rebuilt the prebuilt binary. The remaining decision is whether to own a shipping embedder and both link paths.

## Consolidated Update: Side Modules at Interpreter Scale

The side-module protocol ran with a 36.4 MB CPython module needing 5,754,786 bytes of memory and 12,115 table entries. IFUNC, static TLS, early initialization, constructors, and output matched the flat and native paths.

Support for both link shapes is settled. Shipping still loads one module, the lost-inlining cost remains unmeasurable on current hosts, and both links must stay tested.

## Consolidated Update: Registry Index and Object Identity

The partition cache and whole-object cache answer different questions. Partition keys follow canonicalized bitcode content; removing dead indexed declarations and sorting by name lets an index shift reuse 76 of 80 busybox partitions. The whole object still intentionally misses because `ecv_program_<Index>`, `Request.Keep`, and the generated fragment are baked into its symbol contract and therefore into `ObjectKey`.

Measured shifted-index reuse reduced codegen to 0.2 s but left about 7 s of serial work. Decoupling the exported registry symbol would invalidate the existing ecvisor object cache and was declined. Program index remains part of the cache key, so append new closure programs last to preserve earlier objects. A changed `ModuleID` can orphan otherwise reusable objects even when their ELF inputs remain present.

## Consolidated Update: Stable Split as the Default, and Profile-Aware Linking

`ECV_STABLE_SPLIT` became the default and the switch inverted to
`ECV_NO_STABLE_SPLIT` (host side `RAPTORMARK_NO_STABLE_SPLIT`). The precondition
was byte-affecting switches reaching `TranslateID`, which removed a live footgun:
a `RAPTORMARK_STABLE_SPLIT` run against the shared cache used to poison it.

**Why the default key does not move.** `ExperimentalSettings` returns nil for a
clean environment either way, so `TranslateID`'s pinned literal in
`translate_test.go` is unchanged. What DOES invalidate every cached object is
`TranslateSH`: `stablesplit.go` is in `translateSources`, so editing it moves
the label. That is the intended mechanism -- the flip changes emitted bytes, and
objects cached under the old partitioner must miss rather than be served.
`TestStableSplitIsTheDefault` guards it, neutralized by reverting
`stableSplitEnabled` to the old semantics so the clean-environment case fails
behaviourally rather than as a compile error.

Two pricing mistakes preceded that promotion, both from the same corpus, and
both are the durable part. First: aggregating `*.timing.json` across BOTH
pipelines gave a figure for a path nobody runs. Second: even the corrected
figure described a collapse that already existed behind a flag. **Check
`phases` for `llvm-link`, and check whether the thing is already implemented,
before pricing it.**

Two more identity inputs joined later, both following `InlineCallHistory`
exactly. `translate.Options.SuspendViaCall` (`translate-one --suspend-via-call`,
elfconv patch 0067) is in `TranslateID`, so an object lifted with the suspend
check as a CALL cannot be served for a build that wanted the global read. And
`LinkRequest` gained a `Profile` field, because `supervisorLinkArgs` had ignored
`--profile` entirely: the flat module and the supervisor are linked by different
functions and only the flat one consulted the profile, so
`--side-out --profile hosted` produced a supervisor with no loader backend and
none of the exports a host drives.

⚠️ A DEFAULT change with a cache cost still needs E2E evidence on an image
carrying it, because byte identity cannot be the evidence when two partitioners
assign differently by construction. The stable-split flip owed that for a while
and it was discharged: full suite `ok` at 1655 s on `raptormark-builder:unitfix`,
after 104/0/30 on `:dynload`.
