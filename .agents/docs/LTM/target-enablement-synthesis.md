# Target Enablement Synthesis

## Summary

New guest targets become viable by closing a chain of discovery, loader-state,
runtime, boundary-recovery, and instruction-decoding gaps. Python,
cryptography, Redis, Ruby, and PostgreSQL show that one successful path is not
coverage: durable enablement combines image-wide inventories with workload
reachability and bounded regression controls.

## Included Documents

| Document | Focus |
|----------|-------|
| [aarch64-lifter-and-coverage.md](./aarch64-lifter-and-coverage.md) | Function-target discovery, decoder patches, inventories, and native-oracle guards |
| [python-redis-and-cryptography-bringup.md](./python-redis-and-cryptography-bringup.md) | Dynamic loader state, plugins, shared-VM threads, and SIMD coverage |
| [ruby-jit-and-jump-table-bringup.md](./ruby-jit-and-jump-table-bringup.md) | PAC jump-table recovery, cross-guest validation, and optional-JIT scope |

## Stable Knowledge

- A fused dynamic image does not run the original dynamic loader. Required glibc
  and musl state must be decoded from the actual libc and emitted by reconstructed
  producers.
- A runtime consumer or dormant option proves a mechanism exists, not that the
  production pipeline invokes or populates it.
- Function discovery must combine symbols, unwind data, relocated code pointers,
  BTI targets, and bounded table sweeps. Binary-wide branch-protection properties
  are not safe proxies for per-function coverage in mixed binaries.
- A crash reveals the first reachable gap. An undecoded inventory ranks
  coverage-per-effort, while workload tracing ranks what unblocks a target.
- `enc=0` reports are padding decoded as code and are controls, not instruction
  work. Cold before/after inventories should remove exactly the patched family
  while leaving padding fixed.
- Optional plugins must be admitted atomically with dependencies. A pseudo-
  `dlopen` sentinel plus missing symbols is worse than reporting the plugin
  absent.
- An image can cross the AOT/JIT scope boundary based on argv. Ruby has YJIT
  compiled in but disabled by default; the untested JIT path must not inherit
  evidence from interpreter execution.

## Operational Guidance

Start with the guest's own stderr and identify whether failure occurs in image
discovery, loader-state bring-up, runtime semantics, function discovery, or
instruction execution. Preserve cheap artifacts and use
`RAPTORMARK_TRANSLATE_VERBOSE=1` with a cold object cache when measuring decode
coverage.

Before implementing the instruction at a crash site, inventory the whole closure
and group real encodings by family. Pair that static view with a workload that
reaches meaningful paths: PostgreSQL should plan over a relation rather than only
run a constant-folded query. Validate a lifter patch on the motivating guest, a
native oracle, and at least one unaffected closure. Byte-identical unaffected
objects or modules are stronger than a broad green suite whose fixtures never
reach the changed branch.

## Files

- `patches/`: Ordered elfconv changes; never edit `third_party/elfconv`.
- `internal/image/`, `internal/fuse/`: Plugin discovery, closure admission,
  loader-state decoding, and metadata production.
- `runtime/src/context.rs`, `runtime/src/sys.rs`: Loader substitutes, threads,
  pseudo-`dlopen`, and execution diagnostics.
- `e2e/pacjumptable_test.go`, `e2e/tbltable_test.go`: Assembly and
  native-oracle lifter guards.
- `.agents-workspace/drivers/undecinv/`: Undecoded-family inventory tooling.

## Tests

Run host gates first, then native-oracle E2E tests against explicit before and
after builders with a persistent object cache. Confirm the fixture contains the
encoding or entry shape it claims to test.

```sh
gofmt -l <changed-go-files>
go build ./...
go vet ./...
go test ./...
cargo fmt --manifest-path runtime/Cargo.toml --check
cargo test --manifest-path runtime/Cargo.toml
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1
RAPTORMARK_E2E=1 RAPTORMARK_BUILDER=raptormark-builder:<tag> \
  RAPTORMARK_OBJECT_CACHE=<dir> go test ./e2e/ -v -timeout 60m
```

## Pitfalls

- Do not treat a static upstream guest result as evidence for a fused dynamic
  container image.
- Do not infer loader-state initialization from auxv alone.
- Do not rank workload importance by undecoded site count.
- A C fixture may compile away the exact PAC or instruction shape under test.
- A full E2E pass with zero affected functions is a regression control, not
  direct validation.
- Adopting a lifter patch changes `BaseID` and invalidates translated objects.
