# Runtime Metadata Producers

## Summary

The recovered Rust runtime consumes custom ELF/Wasm data sections that the reconstructed Go pipeline must produce. This producer/consumer relationship is intentionally asymmetric: a runtime consumer is evidence of a requirement, not evidence that a producer exists.

## Key Facts

- Runtime tables are found through `find_data_section`.
- Known produced sections include `.ecv.tls`, `.ecv.irela`, `.ecv.early`, `.ecv.init`, `.ecv.stacklists`, `.ecv.dlsyms`, and `.ecv.musltp`.
- `internal/link` also emits the C registry and `/.raptormark/exec` map.
- Table layouts must be derived from the runtime parser or ABI declaration, not reconstructed by resemblance.
- IFUNC-aware symbol resolution applies to `.ecv.dlsyms` too.

## Details

The metadata gaps appeared sequentially because each missing producer was hidden behind the preceding startup failure. TLS layout, deferred IRELATIVE relocations, early relocations, init ordering, stack-list information, dynamic-symbol interception, and musl thread-pointer layout were all live runtime contracts before the Go side had emitters.

`runtime/src/abi.rs` is authoritative for `EcvProgram` field order and types. `runtime/src/execmap.rs` specifies the exec map as `RMEXEC01`, a little-endian count, and length-prefixed path/hash pairs. Generation rejects empty or duplicate paths and references to unknown programs because the runtime otherwise drops or misroutes them silently.

The durable audit rule is straightforward: search runtime consumers whenever startup exposes a new gap, then prove a producer exists and is wired into the final linked artifact. A matching type or helper that is never invoked is not enough.

## Files

- `internal/fuse/`: Producers tied to fused ELF state.
- `internal/link/`: Registry, exec map, and link-time metadata.
- `runtime/src/abi.rs`: Registry ABI authority.
- `runtime/src/execmap.rs`: Exec map parser and behavior.
- `runtime/src/`: Custom-section consumers.

## Test Coverage

Go-generated registry and exec-map bytes have been round-tripped through the Rust parsers. E2E fixtures validate startup tables on both glibc and musl and exercise `execve`, dynamic symbol interception, and thread-pointer setup.

## Pitfalls

- Assume a newly discovered consumer has no producer until proven otherwise.
- Verify the section reaches the final module; testing a standalone encoder is insufficient.
- A resolver address in an export table is worse than a missing symbol because it can fail silently.

## Consolidated Update: Loader-State Producers

`.ecv.musltp` now also identifies musl's `libc.can_do_threads`, derived from the exported `pthread_create` instruction sequence rather than a private struct layout. Runtime bring-up closes the initial musl thread's `prev` and `next` links only when they are null.

`.ecv.stacklists` word 9 identifies glibc's `_rtld_global_ro._dl_minsigstacksize`. Its offset is decoded from register-chained accesses in exported `__getpagesize` and `__sysconf`, confirmed relative to `_dl_pagesize`, and rechecked before the runtime writes 5120. Adding `AT_MINSIGSTKSZ` alone cannot initialize this field because a fused dynamic glibc never enters the ld.so auxv-consumption path.

Musl's `libc.tls_size`, `tls_align`, `tls_head`, and `tls_cnt` remain without producers. They are the current Redis thread blocker.
