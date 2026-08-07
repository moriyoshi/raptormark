# Build Pipeline Synthesis

## Summary

Raptormark translates an aarch64 Linux container image into one Wasm module
through discovery, fusing, translation, and linking. The pipeline crosses a
recovered runtime, reconstructed Go producers, and a patched lifter, so explicit
metadata contracts and cache identities are correctness boundaries.

## Included Documents

| Document | Focus |
|----------|-------|
| [recovery-and-builder-provenance.md](./recovery-and-builder-provenance.md) | Recovery evidence, builder identity, and patch provenance |
| [image-discovery-and-rootfs.md](./image-discovery-and-rootfs.md) | Container closure discovery and RFS production |
| [fusing-relocations-and-ifunc.md](./fusing-relocations-and-ifunc.md) | ELF layout, relocations, TLS/TLSDESC, and IFUNC |
| [function-boundary-recovery.md](./function-boundary-recovery.md) | Trustworthy function extents and `.ecv.funcs` limits |
| [runtime-metadata-producers.md](./runtime-metadata-producers.md) | Runtime custom-section producer/consumer contracts |
| [translation-linking-and-object-cache.md](./translation-linking-and-object-cache.md) | Deterministic lifting, library partitions, linking, and cache identity |

## Stable Knowledge

- The stages are discovery (`internal/image`), fusing (`internal/fuse`), translation (`internal/translate` plus `translate-one`), and linking (`internal/link` plus `link-all`). The first two run on the host; the latter two run in an explicitly tagged builder.
- A runtime consumer proves that a producer contract is required, not that reconstructed `internal/` emits it. Audit every table read through `find_data_section`.
- The fused ELF models the dynamic loader, including RELR, TLS/TLSDESC, IRELATIVE, and GNU IFUNC implementations. TLSDESC needs the emitted runtime stub shape, not only an address.
- Function boundaries come from sized symbols, `.eh_frame`, PLT ranges, init/fini entries, and computed pointers. Over-claiming silently hides real entries and decodes data as code.
- `.ecv.funcs` is unused and incomplete relative to elflift discovery. A restrictive per-program consumer would lose PLT/gap entries and make library output program-specific.
- Deterministic lifting (`patches/0029`) and namespacing feed `ecv-split`, which clusters related definitions and names outputs from actual membership.
- `ModuleID` follows ELF content. Builder and patch identity belongs in the translation cache key; runtime-only changes intentionally reuse lifted objects.
- Cross-program reuse needs stable names and closure-fixed library addresses. Library partitions use `ECV_LIB_RANGES`, round-robin ordinal assignment, and a default chunk target of 125. Shared names and library ranges remain opt-in.
- `link.WriteLinkInputs` emits the registry and `programs.json` together so sidecar generation does not derive program identity independently.

## Operational Guidance

Read `README.md`, select an explicit `RAPTORMARK_BUILDER`, and set
`RAPTORMARK_OBJECT_CACHE`. Diagnose forward through discovery/rootfs, relocation
and symbol values, boundaries, `.ecv.*` producers, namespaced partitions, then
registry and sidecar.

Keep `third_party/elfconv` clean and change the lifter through `patches/`.
`patches/0030` entry sharding is dormant and must not be used: sampled shards
duplicated 80-84% of their emitted functions. The durable reuse boundary is the
library; lifting each library once remains an open investigation. For shared-name
work, use differently sized programs from one closure because equal-size inputs
can hide unstable bucket membership.

## Files

- `internal/image/`, `internal/rootfs/`: Discovery and RFS production.
- `internal/fuse/`: Layout, relocations, symbols, boundaries, and metadata.
- `internal/translate/`, `internal/link/`: Translation identity and final inputs.
- `builder/ecv-split.cpp`: Content-stable and library-scoped partitioning.
- `runtime/`: Metadata consumers.
- `patches/`: Ordered lifter fork; `third_party/elfconv/` stays clean.

## Tests

Run the Go gate for host-side changes. The fast OpenSSL fixture covers dynamic
image discovery, BTI-enabled IFUNC, and fusing; the ecvisor slow arm exercises
the shipping split path. `e2e/sharednames_test.go` uses a different-size pair and
independently checks served artifacts, non-empty objects, linking, and execution.

```sh
gofmt -l <changed-go-files>
go build ./...
go vet ./...
go test ./...
RAPTORMARK_E2E=1 go test ./e2e/ -v -timeout 60m
```

## Pitfalls

- `raptormark-builder:latest` is stale; use an explicit side tag.
- Never bind an `STT_GNU_IFUNC` resolver as its implementation.
- Go's `debug/elf` has no `SHT_RELR` constant.
- Stable name-to-bucket hashing does not imply stable bucket membership.
- The same library at a different base is different lifted code.
- Never edit `third_party/elfconv` or prune irreplaceable Docker state.
## Refresh: Cached Preparation and Dual Links

Merged preparation now combines four whole-module operations in one parse, and per-library lifting is cached by executable bytes, addresses, in-range boundaries, and lifter identity. Runtime-only changes relink against cached objects. One `-fPIC` object set can also feed both the stock flat link and experimental side modules; `link-all --side-out` emits the supervisor artifact and side modules additively.

A side build must run `raptormark build-tools --base ...` before Docker copies the prebuilt pipeline binary. Source-derived labels, a cold cache, and a changed runtime archive do not prove the executable contains a source change. Validate runtime changes with the archive and builder-pipeline changes on emitted objects.
