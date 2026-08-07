# WASIX and wasmer

## Summary

`--profile wasix` targets wasmer alone. Its socket half ships and is proven by
running: a raptormark module instantiates under stock `wasmer run --net`,
accepts inbound connections, dials out, carries datagrams, honours `O_NONBLOCK`
and epolls a socket. Its loader half is built and gated but deferred behind a
PIC Rust standard library. The measured ABI record, with its probes, is
`.agents/docs/WASIX_ABI.md`; this document is the durable summary and the
reasoning around it.

## Key Facts

- **A host ABI is not portable between hosts just because the names match.**
  Every WASIX trap found produced a module whose import section was perfect and
  whose behaviour was wrong.
- The Wasm 2.0 rule is **SCOPED to the shipping artifact**. Its rationale names
  released `containerd-shim-wasmedge` and `wasmtime`; an opt-in profile
  targeting an advanced runtime is not bound by it.
- `dlopen` is an ordinary WASIX SYSCALL imported from `wasix_32v1`, not a
  host-side facility reachable only from `wasixcc`-compiled C. No 4 GB
  toolchain is on the critical path.
- `dylink.0` on the MAIN module is what switches wasmer onto the
  dynamic-linking instantiation path.
- WASIX loads a real raptormark side module (`rc=0`). The two things it needed
  were elfconv patch 0067 (`--suspend-via-call`) and `-Wl,--shared-memory` with
  the `wasm32-wasip1-threads` libc.
- The supervisor half is blocked on a PIC Rust std, which needs nightly +
  `rust-src` + `-Z build-std`. Nothing ELSE blocks it -- not exception handling,
  not the threads proposal for the lifted objects, not `wasixcc`.
- Of the 139 functions in `wasix_32v1` there is **not one** `shm`, `mmap` or
  memory-sharing call.

## Details

### The measured socket ABI, and its five silent traps

Every claim was measured against wasmer 7.3.0 BEFORE a line of the backend was
written, and every one contradicts what reading the syscall sources alone
suggests.

| measured | why it matters |
|---|---|
| `poll_oneoff` clock timeout **0 means wait forever**; 1 is the immediate probe | `net::wasmedge::ready` uses 0 deliberately. Copying it hangs the guest on its first `epoll_pwait`, with no error and nothing to grep for |
| the address port is **little-endian written, big-endian read back** | `read_ip_port` uses `from_ne_bytes`, `write_ip_port` uses `to_be_bytes`. A symmetric codec is wrong in exactly one direction, and a byte-swapped port is still a valid port that binds somewhere nobody is looking |
| `__wasi_addr_port_t` has a `_padding` byte at offset 1 | the ABI doc says "octs immediately after the tag"; every field is one byte further along than that reads |
| `wasix_32v1.sock_accept` **is** `sock_accept_v2` | the same import name binds to a different function per namespace: 3 params in preview1, 4 here |
| `--net` is required and its absence is quiet | `sock_open` SUCCEEDS and `sock_bind` returns errno 58. Nothing complains until the first bind, and what the guest logs is a bind failure on a perfectly fine address |

Two findings went the other way and saved work: preview1's `poll_oneoff` DOES
observe a `wasix_32v1` socket fd, so `fd_close`, `poll_oneoff` and
`fd_fdstat_set_flags` stay preview1 imports and cost nothing new; and
`fd_fdstat_set_flags(fd, 4)` really does make a WASIX socket non-blocking.

An enum's numbering is not shared either: WASIX has `Stream=1 Dgram=2`,
WasmEdge has them swapped.

Also durable: `sock_get_opt_size(LastError)` is `SO_ERROR` in WASI numbering,
so it is translated rather than passed through -- libpq queries it on every
connection -- and the in-progress family becomes 0 rather than EINPROGRESS,
because SO_ERROR asks how a connect ENDED and one still running has not.
`TCP_NODELAY` maps onto `Sockoption::NoDelay` through `sock_set_opt_flag`, which
makes `wasix` the first profile with both real egress and a working
`TCP_NODELAY`.

**Only the arity mistake fails loudly.** Measure a signature with a
deliberately wrong import type -- wasmer answers with the real one -- but read the
ORDER from the syscall's source. Arity does not give order, and this tree has
paid for inferring one twice.

### How the signature was measured

A `.wat` importing `wasix_32v1.dlopen` with a deliberately wrong type gets:

```
incompatible import type.
Expected Function(FunctionType { params: [I32], results: [I32] })      <- what MY module declared
but received Function(FunctionType { params: [I32 x8], results: [I32] })  <- what WASMER PROVIDES
```

The wording is inverted from the intuitive reading: "Expected" is the module's
declaration, "received" is the host's.

| import | signature |
|---|---|
| `wasix_32v1.dlopen` | `(i32 x8) -> i32`: path, path_len, flags, **err_buf, err_buf_len**, ld_library_path, ld_library_path_len, out_handle |
| `wasix_32v1.dlsym` | `(i32 x6) -> i32`: handle, name, name_len, err_buf, err_buf_len, out_ptr |
| `dlclose` | NOT PROVIDED |
| `dlerror` | NOT PROVIDED -- errors come back through `err_buf` |

### The main-module shape, rung by rung

| rung | module | result |
|---|---|---|
| 1 | plain instance, calls `dlopen` | errno 79 |
| 2 | + shared memory + `__tls_base` export | errno 79 -- so those are not the first gate |
| 3 | + `dylink.0` custom section | **79 is GONE**; now a Linker error demanding the PIC import set |
| 4 | + the PIC imports | `env.memory` type mismatch: wasmer provides `minimum: 129, maximum: 65536, shared: true` |
| 5 | + `(memory 1 65536 shared)` | instantiates |
| 6 | + `malloc`, `free`, `__wasm_call_ctors`, `__wasm_apply_data_relocs` | still 79 |

The required set for a wasix main module:

```
env.memory                     shared, 129..65536 pages
env.__indirect_function_table
env.__stack_pointer  env.__memory_base  env.__table_base
export __tls_base
```

That is very nearly what `sideLinkArgs` already produces for side modules, so
the link recipe is "link the supervisor the way we link a side module, plus
shared memory" rather than anything novel.

### How WASIX finally loaded our side module

```
$ wasmer run --volume libs:/libs ourlib.wat
failed to load module: Expected import to be a function: 'env'.__ecv_unwinding
```

WASIX parsed our `dylink.0`, accepted the memory and table, resolved the
imports, and refused for exactly one reason: its linker requires every `env`
import to be a FUNCTION, and `__ecv_unwinding` is a wasm GLOBAL. It comes from
`runtime/cshim/ecv_globals.c`; elfconv patch 0059 introduced it to replace an
`_ecv_suspended()` CALL with a global read. So the one thing between raptormark
and WASIX dynamic linking was a performance optimization.

`patches/0067-suspend-check-can-go-back-to-a-call.patch` restores the pre-0059
form under `ECV_SUSPEND_VIA_CALL=1`, plumbed as
`translate.Options.SuspendViaCall` -> `translate-one --suspend-via-call`. It is
in `TranslateID`, so an object lifted the call way cannot be served for a build
that wanted the global way. **Patch 0067 is INERT BY DEFAULT** -- on a base with
it applied, a default lift still emits the `env.__ecv_unwinding` global import.

The second requirement was `-Wl,--shared-memory` with the
`wasm32-wasip1-threads` libc. ⚠️ The lifted object does NOT need `-matomics`;
wasm-ld linked it under `--shared-memory` without complaint, which is the
opposite of the usual expectation and removes a codegen-wide change from the
critical path.

### The supervisor wall

| step | result |
|---|---|
| Rust with `-Crelocation-model=pic` | builds; our crate's objects leave the PIC relocation errors |
| a PIC-safe globals shim (`runtime/cshim/ecv_globals_pic.c`) | compiles `-fPIC` |
| supervisor link with imported SHARED memory + imported table | links, 2,479,457 bytes |
| `dylink.0` injected after the header | wasmer switches to the dynamic-linking path |
| **`Linker error: Main module is missing a required import: __stack_pointer`** | **the wall** |

A non-PIC link DEFINES that global; only a PIC/PIE link imports it. `--pie` /
`-shared` need every object to carry PIC relocations -- including the Rust
STANDARD LIBRARY, which ships precompiled and does not.
`-Crelocation-model=pic` reaches our crate and stops there.

**A WASM GLOBAL CANNOT BE COMPILED `-fPIC`.** `ecv_globals.c` exists to define
`address_space(1)` globals, which is the whole reason it is C; under `-fPIC`
clang tries to relocate one through `__memory_base` and the backend gives up
with `Cannot select: WebAssemblyISD::GLOBAL_GET`. `ecv_globals_pic.c` provides
the same accessors over ordinary statics, which is not a weakening -- `context.rs`
already does exactly this for the host build, because the scheduler is
cooperative and switches only at a block, a yield or an exit.

⚠️ `ecv_globals_pic.c` is ONLY correct alongside patch 0067. With the global
form, `--allow-undefined` resolves the suspend read to zero and the guest never
suspends -- a HANG, not a link error.

### The process models, and why ecvisor cannot use them

| WASIX call | verdict |
|---|---|
| `proc_fork` | works, given three requirements -- and ecvisor cannot use it |
| `proc_spawn2` | works, needs nothing, **is not a fork** |
| `proc_exec*` | same family as spawn; ecvisor's `execve` switches images in-instance |
| `stack_checkpoint` / `_restore` | asyncify, same requirements as `proc_fork` |

`proc_fork` was driven to a working fork (exit 202, child pid 2) through three
requirements, found one at a time because each produced a DIFFERENT failure: an
exported mutable `__stack_pointer` global (absent -> exit 79 `Unknown`), the five
Binaryen `asyncify_*` exports (absent -> exit 45 `Noexec`), and a `shared` linear
memory (absent -> `The memory is not shared`).

Ecvisor cannot use it, and the reason is none of the three. Those are costs.
The wall is that ecvisor multiplexes every guest process inside ONE Wasm
instance while `proc_fork` forks the INSTANCE: a guest `fork(2)` routed to it
would duplicate the whole supervisor -- every process in the arena, the fd table,
the run queue -- and yield two supervisors each believing it owns all of them.
`sys_clone` already does the thing that was actually asked for, and its unit is
a bounded dirty set (median 2 MiB on PostgreSQL) rather than the 384 MiB arena.

`proc_spawn2` needs none of `proc_fork`'s requirements because
`proc_spawn3_impl` never calls `unwind`. A WASIX guest instantiated and ran a
second wasm module: `spawn.wat` spawns `child.wasm`, the child printed
`CHILD-RAN` from its own stdout. That makes **`MULTIMODULE.md` §4's first
bullet false under WASIX** -- "a wasm module cannot instantiate another wasm
module" is true of preview1 only.

It still does not unblock §2, and the reason is memory rather than loading. A
spawned module gets a FRESH linear memory and starts at `_start`, while §2's
program modules must import the SUPERVISOR's memory. And `wasix_32v1` has no
memory-sharing call at all -- two WASIX processes share a filesystem, pipes and
sockets, and not one byte of memory. That rules out the process-per-instance
architecture in which `proc_fork` WOULD be correct: no POSIX `shm`, no
`MAP_SHARED`, no SysV segments, therefore no PostgreSQL. Ecvisor's
single-linear-memory design does not have this problem, which is now an argued
property rather than an accident.

Two secondary findings: `proc_fork` calls `ensure_static_module()` and returns
`Notsup` for any dynamically linked module, so it is mutually exclusive with
`--side-out` in wasmer independently of anything this tree does; and the
missing-asyncify failure is a process death with nothing written to `pid_ptr`
and the explaining WARN only under `RUST_LOG`, so a guest cannot detect it and
fall back.

### The three runtimes spell the directory flag three ways

```
wasmedge --dir    GUEST:HOST
wasmtime --dir    HOST::GUEST
wasmer   --volume HOST:GUEST      (the wasmtime order with the wasmedge separator)
```

`--mapdir GUEST:HOST` still runs under wasmer 7.3.0 but is deprecated and warns
that it goes in the next major. Measured with a `path_open` probe rather than
read off `--help`, because `fd_prestat_dir_name` answers `/` for the first
preopen whichever order it was given and cannot tell the two apart.

## Files

- `.agents/docs/WASIX_ABI.md`: the measured ABI record for both the socket and the process halves, with the probes.
- `.agents-workspace/wasmer/`: the probe harness -- `fork.wat`, `fork_stub.wat`, `fork_shared.wat`, `spawn.wat`, `mkchild.py`, and `src/` holding v7.3.0 `proc_fork.rs` / `syscalls/mod.rs` so the citations survive without network. Moved here from `.agents-workspace/tmp/`, which is wiped without warning.
- `runtime/src/net/wasix.rs`, `wasix_addr.rs`: the fourth `NetBackend`.
- `runtime/src/net/poll1.rs`: the shared `poll_oneoff` subscription and event codec, pure and uncfg'd, with byte-offset tests. `clock_sub` passes the timeout through verbatim and refuses to default it -- that is precisely where preview1 and WASIX disagree, and a helpful default there would bury a hang.
- `runtime/src/loader/wasix.rs`: the loader backend.
- `runtime/wasix/BUILD.bazel`, `runtime/cshim/ecv_globals_pic.c`.
- `patches/0067-suspend-check-can-go-back-to-a-call.patch`.

## Test Coverage

- `e2e/wasixnet_test.go`, gated by `RAPTORMARK_E2E_WASMER=1`: inbound, outbound, datagrams, non-blocking semantics, and `epollSocketGuestSrc`. It takes its wasmer argv FROM `pipeline.RuntimeArgsForTest` rather than spelling it itself, so `--volume` cannot regress to `--mapdir` and `--net` cannot be lost while both tests stay green.
- `//runtime:profile_exclusion_test`: each profile carries exactly one net backend AND carries its own.
- `//runtime:loader_exclusion_test`: four archives; `hosted` and `wasix` carry only their own backend, because the two are ALTERNATIVES and a build carrying both would import from two engines at once.
- Final gate for the socket work, run alone: **117 pass / 30 skip / 0 fail** (353 s); Go 13 packages; Rust 301 tests with all four `net-*` features compiling for `wasm32-wasip1`; Bazel 13/13 with 0 skipped.

## Pitfalls

- **Strings tell you what a binary CAN say, never what it does.** Two errors in
  one session came from reading a LIST of names and inferring behaviour: an
  ORDER of checks out of an error-variant list, and a MEANING out of an errno
  name's neighbours. Both were settled in one run each by calling the thing.
- **errno 79 is `unknown`, the catch-all at the end of the enum**, not "not a
  dynamically-linked instance". Anchored on `notcapable` = 76, which matches
  preview1. The discriminating evidence was available before the wrong claim
  was written: `dlsym` with a bogus handle ALSO returns 79 and `err_buf` comes
  back empty in both cases. A code that appears for two unrelated failures and
  carries no message is a catch-all, not a diagnosis.
- **The `dlopen` argument order was guessed and was wrong.** `err_buf` comes
  BEFORE `ld_library_path`; with them swapped wasmer wrote every diagnostic to
  address 0, and the empty buffer was read as "rejected before doing any work".
  With the order corrected the very first run said
  `Failed to find shared library libc.so` -- the linker had been working the
  whole time. Two hours of ladder, one file of ground truth.
- **`profile_exclusion_test` was passing vacuously**, and only a neutralization
  showed it. It is entirely ABSENCE assertions, and `ecvisor::net::loopback`
  leaves no symbols in any archive because it inlines away at `-Clto=fat`. So
  "loopback is not in the wasix archive" was satisfied by an archive that had
  compiled loopback in -- exactly what forgetting one of the two `any(...)`
  lists in `net/mod.rs` produces, since `net-loopback` is a LABEL rather than a
  `cfg`. Proven by pointing `//runtime/wasix` at `net-loopback`: all four
  `foreign-backends=0` lines still printed `ok`. A positive control was added.
- **`--profile wasix` implies `--suspend-via-call` only with `--side-out`.** A
  flat link pulls in `ecv_globals.o`, which DEFINES `__ecv_unwinding`; measured,
  a flat wasix module has 33 imports and no `env.*` at all. Implying it
  unconditionally gave every socket-only wasmer build a cold object cache and
  hours of re-translation to remove an import that build never had.
- **`NetBackend::ready` had NO end-to-end coverage in this tree, from any
  backend**, until `epollSocketGuestSrc`. Nothing in `e2e/` epolled a SOCKET:
  `timers_test.go` epolls an eventfd, and every socket test blocks in
  `accept`/`connect`, which the scheduler serves through `wait`. That matters
  most here, because `ready` is where the WASIX zero-timeout trap lives. It
  remains uncovered on the SHIPPING profile.
- **`std::env::var` after a fork can hit `lazy_lock::panic_poisoned`**, whose
  panic handler re-reads the environment and loops forever.
  `diag::tests::env_is_read_only_from_startup_paths` caught the new backend
  doing it inside a `dlopen` syscall, which is precisely a post-fork path -- a
  guard written for a different bug, stated as a SCAN rather than as advice.
- **Two ABIs sharing a name share nothing else.** WASIX `Noexec` is 45, Linux
  `ENOEXEC` is 8. Reading one as evidence of the other is available at every
  step.
- **Stage a probe so each missing piece fails DIFFERENTLY.** `proc_fork`'s three
  requirements are trustworthy because the failures were distinguishable, not
  because the list is short. And a probe that always fails cannot attribute its
  own failure -- `fork.wat` exits 45 blaming a missing asyncify export, but
  "proc_fork always fails" gives the same 45, so `fork_stub.wat` exists solely
  to move the outcome to a different diagnostic.
- **Pick the observation the claim needs, not the one the API returns.** For
  `proc_spawn` an errno of Success proves the call was accepted; the CHILD'S OWN
  STDOUT proves a second instance ran guest code.
- **External DNS is undemonstrated.** UDP works over loopback; whether
  `wasmer run --net` permits outbound UDP to port 53 is untested, and `--net`
  takes filter rules. ❌ Do not close that gap by wiring in `net::dns` -- that
  module exists because a browser has no UDP at all, and here it would replace a
  working transport with a synthesised answer.
- See [[wasm-runtime-and-oci-compatibility]] for the Wasm 2.0 rule this profile
  is deliberately outside, [[dynamic-side-module-loading]] for the loader seam
  the wasix backend plugs into, and [[web-embedder-and-browser-networking]] for
  the other non-default net backend.
