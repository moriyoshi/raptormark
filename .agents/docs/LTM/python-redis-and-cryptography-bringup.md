# Python, Redis, and Cryptography Bring-up

## Summary

Redis on Alpine and Python on Debian exposed complementary loader-substitute, thread, plugin, and instruction gaps. Python and cryptography now run representative workloads; Redis reaches real shared-VM thread creation but remains blocked by musl TLS bookkeeping that ld.so would normally initialize.

## Key Facts

- `python:3-slim` evaluates Python, imports pure Python and C extension modules, and exits normally.
- `cryptography` 50.0.0 passes SHA-256, Ed25519 sign/verify, and AES-GCM round trips.
- `redis:7-alpine` remains blocked on musl `libc.tls_size`, `tls_align`, `tls_head`, and `tls_cnt`.
- Fused dynamic glibc never runs the ld.so path that consumes auxv into `GLRO`; required fields must be seeded directly.
- Runtime `dlopen` already returns a sentinel and resolves `dlsym` from `.ecv.dlsyms`; the missing work was admitting optional plugin objects into the fused closure.
- A static upstream CPython example does not exercise the dynamic loader, shared-library fuser, plugin model, or ecvisor process model.

## Details

### Redis and musl

Musl provided almost no `.eh_frame`, so Redis first required additional function starts recovered from stored code pointers and relative relocations. Its initial thread record also needed `prev` and `next` closed onto itself. `CLONE_THREAD` then made shared-VM pthread creation real, and `.ecv.musltp` gained the derived `libc.can_do_threads` address.

Redis now reaches musl `__copy_tls`, where zeroed `libc.tls_*` fields yield `new = -sizeof(struct pthread)`, a null thread pointer, and invalid SETTID addresses. Ecvisor already owns the static TLS geometry in `.ecv.tls`; the remaining task is to construct musl's `struct tls_module` list and derive the relevant `struct __libc` offsets without relying on private ABI layouts.

### Python loader state

Python first stopped because `_rtld_global_ro._dl_minsigstacksize` was zero. Adding `AT_MINSIGSTKSZ` did not help: a fused dynamic glibc does not enter `dl_main`, while `_dl_aux_init` belongs to the static path. The field offset is decoded from register-chained access patterns in exported `__getpagesize` and `__sysconf`, confirmed by adjacency to `_dl_pagesize`, emitted as word 9 of `.ecv.stacklists`, and written only when the surrounding state is plausible.

The old recursion alarm also mistook an interpreter stack for runaway recursion. The default is now 16,384 frames, configurable with `RAPTORMARK_ECV_MAXDEPTH`; Python's import path measured a peak of 237.

### Plugins and cryptography

`fuse.Options.Extra` and the runtime pseudo-`dlopen` mechanism already existed but had no production caller. Extra plugins are now walked independently after the mandatory closure. A plugin with a missing dependency is rolled back completely and recorded in `Report.SkippedExtras`; partial admission would be worse than absence because `dlopen` returns a sentinel regardless.

`image.PluginDirs` names Python `lib-dynload`, site-packages, and OpenSSL plugin directories. It deliberately avoids scanning every unreferenced shared object, which would add roughly 250 gconv and NSS objects. On Python, 77 offered extensions produced 95 fused libraries and skipped `_tkinter`, whose Tk dependency is absent from the source image too.

The cryptography image statically links OpenSSL inside `_rust.abi3.so`. Its successful probes cover the Python plugin path, hundreds of constructors, vector instruction additions, hashing against a host oracle, asymmetric signing, and authenticated encryption. A full dynamically linked OpenSSL TLS 1.3 handshake remains limited by broader SIMD coverage.

## Files

- `internal/fuse/`: Optional plugin loading, rollback, and metadata production.
- `internal/image/`: Named plugin-directory discovery.
- `runtime/src/context.rs`: Loader-state seeding, thread model, and recursion diagnostics.
- `runtime/src/sys.rs`: Pseudo-`dlopen` and dynamic-symbol lookup.
- `e2e/`: Python, minimum-signal-stack, thread, and vector-instruction guards.

## Test Coverage

The glibc 2.36 unit fixtures pin instruction decodes while `e2e/minsigstksz_test.go` validates the same derivation against glibc 2.41. Python was run through `json`, `math`, `hashlib`, and `base64`; cryptography was checked with independent host oracles and multiple algorithm families. Optional-plugin rollback tests were neutralized against aliased dedup maps.

## Pitfalls

- An auxv entry does not initialize glibc loader globals in a fused dynamic image.
- A successful sentinel `dlopen` with missing symbols can resemble a malformed extension rather than an absent object.
- Plugin-heavy closures can exceed the closure-wide shared-layout region and silently fall back to per-image packing.
- Redis's `bioInit` prints `strerror(errno)` even though `pthread_create` returns its error directly; the printed errno is not reliable evidence.
- Upstream static-binary claims and performance numbers are not comparable to fused dynamic container closures.
