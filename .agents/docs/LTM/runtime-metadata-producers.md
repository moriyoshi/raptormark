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

## Consolidated Update: The Fused-Glibc Threading Chain

A fused DYNAMIC glibc image could not create a thread at all, and closing it
took three producers in sequence, each hidden behind the last. The shape
generalises: ld.so performs bring-up that a fused image never runs, so any state
it installs has to be reconstructed or invoked deliberately.

1. **The default pthread attr.** `EcvContext::apply_pthread_attr_default` gives
   glibc a default thread stack by calling glibc's OWN exported API at bring-up --
   `pthread_attr_init`, `pthread_attr_setstacksize`,
   `pthread_setattr_default_np` -- so it pokes no struct and needs no layout
   evidence, unlike the two musl seeds. It reads the value back through
   `pthread_getattr_default_np` and refuses to claim success unless glibc
   reports what it was given. Size is 1 MiB, `RAPTORMARK_ECV_THREAD_STACK`
   overrides.
2. **`_dl_tls_static_size` / `_dl_tls_static_align`**, words 10-11 of
   `.ecv.stacklists`, seeded from the image's own TLS geometry. The offsets come
   from `__pthread_get_minstack`, whose whole body is
   `roundup(GLRO(dl_tls_static_size), GLRO(dl_tls_static_align)) +
   GLRO(dl_pagesize) + PTHREAD_STACK_MIN`, with three conditions or nothing is
   emitted: the GOT slot must hold `&_rtld_global_ro`, there must be exactly ONE
   such `ldp`, and the neighbouring plain `ldr` must name the `_dl_pagesize`
   offset `__getpagesize` gives INDEPENDENTLY. Without them
   `__static_tls_align_m1` is `0 - 1`, all-ones, and `size &= ~m1` in
   `allocate_stack` makes ANY size zero.
3. **The ld.so hook cluster.** Six consecutive pointer slots immediately below
   `_rtld_global_ro` -- the `__rtld_malloc`/`calloc`/`realloc`/`free` family --
   called from 9, 53, 45, 4, 13 and 22 sites. **No relocation covers them**:
   `_rtld_global`'s file image holds six small integers and not one pointer, and
   ld.so's whole `.rela.dyn` is one GLOB_DAT. ld.so installs them at startup.
   ⚠️ Do NOT seed the six individually -- they are `attribute_hidden`, unnamed in
   dynsym, and installing `free` where `calloc` belongs is silent corruption.
   The tractable design, and what was implemented, is to identify
   `__rtld_malloc_init_stubs` STRUCTURALLY ("stores constant function addresses
   into >= 4 slots of the cluster, takes no arguments"), emit its entry VMA using
   the recovered function boundaries, and CALL it at bring-up the way
   `apply_early_init` calls `__libc_early_init`. One call restores the whole
   family instead of six guesses. `context.rs` logs
   `ld.so hook installer ran: 0x{init:x}`.

⚠️ Two hypotheses were refuted on the way and should not be restarted.
`_dl_rtld_lock_recursive` is NOT the null call: in glibc 2.41
`__rtld_lock_lock_recursive` compiles to a DIRECT `bl __pthread_mutex_lock`, so
`.ecv.stacklists` words 6 and 7 have no consumer to point at. And the fault was
not the default attr: a guest passing an EXPLICIT attr with its own stacksize
still asserted, which is what moved the search downstream.

Why it never bit before: fused glibc guests here are processes, not threads
(nginx, postgres), and python's dlopen path is INTERCEPTED by the runtime, so
real `_dl_open` never runs. `_dl_allocate_tls_storage` on the `pthread_create`
path is the first thing to need them. Every threading guard in this tree was
static glibc or fused musl until `e2e/tlsdesc_test.go`, which needs TWO THREADS
in a fused dynamic glibc image to say anything -- which is exactly the capability
that was missing.

⚠️ Each iteration here costs a COLD translate (~8-10 min): a producer change
alters the fused ELF, which changes the object-cache key. Batch producer changes
before re-running.

## Consolidated Update: Exec-Map Identity Drift

The exec map's failure mode is silence. A hash the runtime cannot match was
DROPPED without a diagnostic, so the guest ran the wrong program and the symptom
appeared wherever that program first disagreed with its argv -- seen as
`unrecognized configuration parameter "username"` (postgres running initdb's
flags) and as `dash: invalid argument: "/run-pg.sh"` carrying postgres's
`Try "%s --help"` format under dash's argv[0]. `execmap.rs` records four
separate incidents.

The three causes that have actually occurred, all avoidable:

1. a re-lift changed the module IDs and the sidecar was stale;
2. a non-canonical guest path -- `/bin/dash` on a usr-merged image, where the
   resolved path is `/usr/bin/dash`;
3. the sidecar generated with a DIFFERENT builder tag than the one whose objects
   are in the registry. ⚠️ Note the identity moves for reasons beyond the
   lifter: renaming a patch file from `0046-fcm` to `0047-fcm` changed the
   patched-base image content, hence its ID, hence every module ID.

Both halves of the fix are in. The runtime WARNS unconditionally when an
exec-map entry names a hash the registry does not contain, naming the path, the
missing hash and the registry's contents -- verified in both directions, a stale
id warns and a correct sidecar is silent. And the second derivation is gone: the
link writes `programs.json` and the sidecar build reads it, naming programs by
INDEX, so no hash is derived or typed twice. What remains is only the case a
manifest cannot help -- a sidecar built against a DIFFERENT module -- which is
what the warning exists for.

⚠️ Under a host-driven loader the warning's premise inverts: an unregistered
hash is the NORMAL state, so the text says "not registered yet" and explains
which build makes it suspicious. See [[dynamic-side-module-loading]].
