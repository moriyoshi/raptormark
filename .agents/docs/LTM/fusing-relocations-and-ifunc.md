# Fusing, Relocations, and IFUNC

## Summary

`internal/fuse` combines an executable with its dynamic closure into one ELF before lifting. Correctness depends on applying every relocation family the loader would apply, resolving GNU IFUNCs to implementations, and emitting metadata for relocations that must be completed by ecvisor.

## Key Facts

- The fuser assigns disjoint virtual ranges, merges loadable sections, rewrites symbols, and resolves dynamic dependencies.
- Relative, symbol, TLS, IRELATIVE, and RELR relocations all occur in real distro images.
- Go's `debug/elf` has no `SHT_RELR` name; inspect real section types explicitly.
- `STT_GNU_IFUNC` values are resolver addresses, never implementation addresses.
- IFUNC resolution is required both while relocating and when producing `.ecv.dlsyms`.
- Resolver interpretation treats the AArch64 HINT space, including `bti c`, as no-op while keeping the mask narrow.
- AArch64 TLSDESC relocations require an emitted runtime stub; resolving only the descriptor address is insufficient.

## Details

The recovered proof-of-concept bitcode established the intended fused-address model. The Go implementation reconstructs that contract and was checked against real glibc inputs where all ordinary relocations resolved.

Some relocations can be fixed entirely at build time. Others depend on runtime thread-pointer state and are represented through metadata sections. RELR deserves special attention because a switch over only standard-library constants silently omits it. The fuser decodes its bitmap format and applies the encoded relative relocations.

IFUNC resolvers are evaluated with a deliberately small AArch64 interpreter. Binding the resolver itself is a silent semantic failure: calls enter a function that computes and returns a pointer instead of performing the requested operation. Every path that consumes a symbol value must therefore pass through the IFUNC-aware resolution logic.

PostgreSQL exposed `R_AARCH64_TLSDESC` after the ordinary TLS relocation set was
already green. The fuser resolves the descriptor into emitted data and a stub
whose runtime behavior computes the thread-relative address. This is another
producer/consumer boundary: accepting the relocation without emitting the shape
ecvisor executes can produce a valid-looking ELF that fails only at startup.

The resolver evaluator originally rejected BTI-enabled glibc because it recognized only the exact `NOP` word. Matching the HINT instruction class fixed the real OpenSSL fixture while retaining a negative test for nearby non-HINT encodings.

## Files

- `internal/fuse/`: ELF layout, dependency loading, relocation, symbols, IFUNC evaluation, and metadata emission.
- `runtime/src/`: Consumers of deferred relocation and TLS tables.
- `e2e/`: glibc and musl fixture coverage.

## Test Coverage

Unit tests cover relocation encodings, IFUNC instruction evaluation, and narrow instruction masks. Real OpenSSL discovery-and-fuse coverage detects both dynamic relocation and BTI resolver regressions. Slow E2E tests validate the resulting module.

## Pitfalls

- Enumerate relocation section types from real fixtures rather than from `debug/elf` names.
- Resolver values can reappear in newly added export or metadata paths.
- A technique validated on glibc may be irrelevant or invalid on musl.
- Rebuild any diagnostic binary after changing the fuse library.
