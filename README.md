# Raptormark

<img src="./assets/raptormark-logo-art-and-wordmark.svg" />

Ahead-of-time translation of **aarch64 Linux container images into a single
WebAssembly module**, with a purpose-built supervisor runtime instead of a
kernel.

raptormark takes an OCI image, works out which binaries its entrypoint can
actually reach, statically links each one against its shared libraries, lifts
the resulting machine code to LLVM IR with a patched
[elfconv](https://github.com/yomaytk/elfconv), and emits one WASI module that
carries the whole container: the programs, a compressed read-only filesystem,
and the container's personality (argv, env, cwd, uid/gid). The module is
verified on WasmEdge directly and through released containerd/runwasi. The
default profile imports WasmEdge's non-standard preview1 socket extensions, so
wasmtime cannot run it. Two further profiles drop them: `--profile loopback`
runs on stock wasmtime with an in-process network, and `--profile browser` runs
**in a browser tab** over a purpose-built import module, with the guest handing
control back to the page instead of blocking it. Neither has external network
egress yet. A fourth, `--profile hosted`, is loopback plus a host-aided loader:
the guest's `dlopen` and `execve` can be served **while it runs**, by a host that
instantiates the needed side module in response. It imports one function no
stock shim supplies, so it is deliberately not the default. A fifth,
`--profile wasix`, targets **wasmer**: it speaks WASIX sockets from the
`wasix_32v1` namespace, which makes it the only profile besides the default with
**real external network egress** — a guest listens and something outside
connects, or the guest dials out, both verified against a real wasmer. wasmer's
WASIX runtime is also itself a loader; that half is unfinished, so its side
modules load under `dlopen` today and its supervisor does not yet (see the
honest status below).

There is no emulator and no interpreter in the output. Guest machine code
becomes wasm functions; guest syscalls become Rust.

## Goal

Run an unmodified `linux/arm64` container image on a wasm runtime, at native
wasm speed, with no source, no recompilation, and no cooperation from the image
itself.

This is bounded by what can be known ahead of time. A survey of eight official
arm64 images:

| image | programs | libs | lift MiB | required strategy |
|---|---|---|---|---|
| `postgres:17` | 71 | 49 | 88.1 | compiled — liftable |
| `redis:7-alpine` | 3 | 4 | 22.7 | compiled — liftable |
| `nginx:alpine` | 3 | 5 | 10.3 | compiled — liftable |
| `python:3-slim` | 1 | 4 | 7.9 | interpreted |
| `ruby:3-slim` | 2 | 8 | 24.0 | interpreted by default; optional YJIT |
| `php:8.3-cli` | 6 | 42 | 48.7 | interpreted + 2 native extensions |
| `node:22-slim` | 3 | 7 | 121.4 | **JIT** |
| `eclipse-temurin:21-jre-alpine` | 7 | 10 | 10.8 | **JIT** |

Compiled and interpreted images are in scope. JIT images are not a coverage gap
to be closed: a runtime that emits aarch64 as it runs has no machine code to
lift ahead of time.

That boundary is about the guest's execution mode, not only its image name.
`ruby:3-slim` has YJIT compiled in but disabled by default: interpreter execution
is in scope, while a run started with `--yjit` crosses the AOT boundary.

The reference target is the official **postgres** image; `openssl` and
`nginx:alpine` are the working fixtures today.

## Architecture

Four stages, three of which are Go code in `internal/` driving tools that live
inside a builder Docker image. `internal/pipeline` strings all four together
behind `raptormark build`; each is also reachable on its own.

```mermaid
flowchart TB
  subgraph host["Host — Go, internal/"]
    DRV["internal/pipeline<br/>raptormark build / run"]
    IMG["internal/image<br/>entrypoint closure:<br/>which ELFs can run?"]
    FUSE["internal/fuse<br/>build-time dynamic linker"]
    RFS["internal/rootfs<br/>rfs sidecar + boot record"]
    TR["internal/translate<br/>drives translate-one,<br/>content-addressed object cache"]
    LNK["internal/link<br/>generates registry.c<br/>(EcvProgram ABI)"]
  end

  subgraph builder["Builder image — builder/"]
    LIFT["elflift<br/>(patched elfconv)<br/>ELF → LLVM IR"]
    PROMO["ecv-promote<br/>namespace-object"]
    CG["ecv-split / llvm-split + clang<br/>parallel wasm codegen"]
    LA["link-all<br/>+ libecvisor.a"]
  end

  OCI[("OCI image<br/>linux/arm64")] --> DRV
  DRV --> IMG
  IMG -->|"one program per<br/>reachable binary"| FUSE
  IMG --> RFS
  FUSE -->|"fused ET_EXEC:<br/>exe + libs, one address space"| TR
  TR --> LIFT --> PROMO --> CG
  CG -->|"program_i.o"| LNK
  LNK --> LA
  TR -.->|"per-program C fragment"| LNK
  LA --> WASM[["module.wasm"]]
  RFS --> SIDE[["rootfs.img"]]
  WASM --> RUN{{"WasmEdge / containerd-runwasi"}}
  SIDE --> RUN
```

### 1. Discovery — `internal/image`

Reads the image config, exports its filesystem, and computes the closure of
aarch64 executables reachable from the entrypoint by `exec`. Only that closure
becomes programs; the rest of the filesystem is payload.

Packaging follows the **full-fuse execve model**: every unit is a self-contained
program with its own entry in the exec map, so an `execve` inside the guest is a
lookup in the registry rather than a load.

### 2. Fusing — `internal/fuse`

`elflift` cannot lift a dynamic executable. `internal/fuse` is a build-time
dynamic linker that removes the need for one: it maps an executable and its
`DT_NEEDED` closure into a single address space at distinct bases, merges
sections (`.text`, `.got.l0`, `.data.l1`, …), lays out static TLS, resolves
every relocation eagerly — including RELR and IFUNC, whose resolvers are
*interpreted* at build time — and emits one `ET_EXEC` image with synthetic
program headers and a merged symbol table.

Binding is eager and no runtime loader is involved: a PLT stub lifts as ordinary
code that loads a pre-resolved GOT slot and branches indirectly.

It also emits the bring-up tables the supervisor needs, since a fused image has
no `ld.so` to run them: `.ecv.init` (constructors), `.ecv.early`
(`__libc_early_init`), `.ecv.stacklists` (`_rtld_global` thread lists),
`.ecv.irela` (deferred ifuncs) and `.ecv.dlsyms` (for intercepted `dlopen`/`dlsym`).

`.ecv.dlsyms` is emitted **per unit**, which is what makes `dlsym` handle-scoped.
It used to be one flat table for the whole image, and every postgres extension
defines `Pg_magic_func`, `_PG_init` and `pg_finfo_*` — so a second module
collided with the first and first-wins bound the wrong one, silently. A plugin
can now be fused as its own unit against the closure's shared layout, so its
cross-references still resolve statically at fuse time while its data, ifuncs and
constructors wait until it is opened.

Function boundaries drive lift quality, so they are recovered from three
independent sources: the symbol table, `.eh_frame` FDEs (glibc), and computed
code pointers (musl, which ships almost no unwind data).

### 3. Translation — `raptormark translate-one`, driven by `internal/translate`

Inside the builder image, per program:

```
ELF → elflift → .bc → ecv-promote → namespace-object → internalize/globaldce
    → ecv-split / llvm-split → clang -O3 (parallel) → program_i.o
```

`namespace-object` tags every local symbol with the module id so N programs can
share one link. `internalize` leaves exactly one exported symbol per program:
its `EcvProgram` descriptor.

The content-stable partitioner `ecv-split` is the default since 2026-08-23;
`RAPTORMARK_NO_STABLE_SPLIT` restores `llvm-split`. With a
closure-wide library layout, address-derived shared names, and
`RAPTORMARK_LIB_RANGES`, each reusable partition belongs to one library rather
than to an arbitrary fixed bucket. On the measured PostgreSQL closure this cut
the marginal cost of initdb + dash after postgres from 19m11s to 5m49s. The
shared-name path is still opt-in. All of these byte-affecting
settings are included in `TranslateID`, and therefore in `ObjectKey`, so
differently configured translations cannot collide in the object cache.

Objects are cached by content-addressed key (`ObjectKey`) over the patched
base image's digest, a hash of the translation pipeline's own sources
(`internal/builder.TranslateSH`), the ELF, and the codegen options — so a change
confined to `runtime/` reuses every translated object and costs only the final
link. Enable with `RAPTORMARK_OBJECT_CACHE=<dir>`.

Per-library lift caching is automatic when library ranges and a persistent
partition or object cache are available. `RAPTORMARK_LIB_CACHE` overrides its
location; `RAPTORMARK_NO_LIB_CACHE` disables it for differential checks.

### 4. Linking — `internal/link` + `link-all`

`internal/link` generates the C registry that binds each program's renamed
`_ecv*` symbols into an `EcvProgram` descriptor array. `link-all` compiles it
and links the N objects with `libecvisor.a` into one module.

`link.WriteLinkInputs` writes the registry and `programs.json` manifest from one
program list. Sidecar generation consumes that manifest instead of deriving
module identities a second time.

The same `-fPIC` objects support `link-all --side-out <dir>`, which keeps the
flat module and additionally emits one side module per program plus
`supervisor.wasm`. The protocol runs in the development embedder; no shipping
host owns it yet.

### The runtime — `runtime/` (ecvisor)

ecvisor is the project's novel component: a ~7.7k-line Rust `wasm32-wasip1`
staticlib that replaces upstream elfconv's entire C++ runtime. The lifted code
treats its former `RuntimeManager*` as an opaque pointer into ecvisor's own
context; no C++ object ever exists in the module.

```mermaid
flowchart TB
  START["_start → __main_argc_argv<br/>(entry.rs)"]
  START --> BOOT["boot.rs<br/>argv / env / cwd / uid / gid<br/>from the sidecar"]
  BOOT --> MOUNT["vfs/mod.rs — overlay<br/>tmpfs (upper) over rfs (lower)"]
  MOUNT --> SEL["execmap.rs<br/>guest path → program index"]
  SEL --> SCHED["cooperative scheduler<br/>+ replay-based fork/resume"]

  SCHED --> GUEST["lifted guest code<br/>(one wasm fn per guest fn)"]
  GUEST -->|"_ecv_* intrinsics"| INTR["intrinsics.rs<br/>remill semantics, dispatch tables"]
  GUEST -->|"svc #0"| SYS["sys.rs<br/>Linux aarch64 syscalls"]
  SYS --> MOUNT
  SYS --> ARENA["arena.rs<br/>flat guest address space,<br/>brk / mmap"]
  GUEST --> ARENA
  INTR --> ARENA
```

Interception is layered — L0 instruction semantics, L1 syscalls, L2 libc, L3
whole libraries — which is what keeps the same conversion core aimed at
different hosts. The server profile (containerd/runwasi + wasmedge) intercepts
at L1 in `sys.rs`; constrained hosts would have to intercept higher, because
every un-lifted library is bundle bytes not shipped.

The VFS is pluggable behind one coordinator that owns path normalization and
cross-layer symlink resolution — squashfs, passthrough or network backends slot
in without touching the syscall layer.

Blocking is part of the guest ABI, not an implementation detail. A guest
descriptor marked non-blocking receives `EAGAIN`/`EINPROGRESS`; a blocking
descriptor suspends its process until host readiness or an absolute deadline.
The idle scheduler polls socket readiness and the earliest deadline together,
so neither wake source can starve the other. This is what lets nginx return to
`epoll_pwait`, run its timer wheel, and keep other connections serviceable.

Process arenas are snapshotted in BOUNDED form, unconditionally (since
2026-08-22): a suspended process stores only its writable image, brk, private
mmap, live stack, and TLS ranges — median 2 MiB, max 6 MiB of a 384 MiB arena
over 102 measured switches — instead of a full-size copy. Allocation zero-fills
and a cross-program switch re-materialises the correct image. There is no
environment variable and no full-buffer alternative: the scheme was opt-in, then
the default, and the gate was removed entirely. A **multi-threaded group** is the
one exception and it is not a mode — a bounded range set derives from one stack
pointer, which says nothing about a sibling thread's stack, so such a group still
takes a full snapshot. Named AF_UNIX sockets
and shared regular-file contents live inside ecvisor, which is what allows
PostgreSQL processes in one module to communicate and observe the same files.

### The scheduler — `runtime/src/entry.rs` + `runtime/src/context.rs`

There is no kernel to preempt anything and no host thread to park, so the
supervisor multiplexes every guest process onto the single wasm stack itself.
The scheduler is **cooperative and single-threaded**: control changes hands only
where a guest process asks it to — a syscall that would block, `sched_yield`,
`clone`/`fork`, `execve`, `exit`/`exit_group`. Nothing is time-sliced; a guest
that computes forever holds the module forever, and that is deliberate, because
there is no interrupt to build preemption on.

**A leg, not a coroutine.** The main loop in `entry.rs` runs one *leg* of the
current process: it calls straight into lifted code and waits for an ordinary
return. There is no asyncify and no exception handling (both are foreclosed —
asyncify is mutually exclusive with the `--fork_emulation` codegen the fork
model needs, and the emitted module must stay inside Wasm 2.0). A blocking
syscall therefore does not unwind a saved stack; it *returns* one:

1. the handler sets `ctx.suspended` and records why (`Pending::{Yield, Block,
   Exit, ExitThread}`, plus a `BlockedOn` for a block);
2. the svc trampoline captures the process's replay state and raises the
   `unwinding` flag — a wasm global, not a context field, because the lifted
   code tests it after every syscall *and* every call: 33,540 sites in the
   linked bash fixture alone, one `global.get` each instead of one call to
   `_ecv_suspended` (elfconv patch 0059);
3. every lifted frame between the syscall and the scheduler sees the flag and
   returns immediately, so the leg comes back by plain return.

The native frames are thrown away, and that is the point: a process's whole
durable state lives in the arena and in its saved `State`, never in wasm locals,
so a suspended process costs no stack.

**Resume is replay, not rewind.** Because the frames are gone, the scheduler
rebuilds them by re-entering each one, innermost first, using the call history
the `fork_emulation` codegen maintains. A suspended process carries a `Replay {
cur, remaining, resuming }`: `cur` is the frame to enter now — a lifted function
plus a mid-body pc, reached through the block-address map — and `remaining` is
the outer frames. Re-entering the innermost frame at its post-SVC pc does *not*
re-execute the `svc`, so the scheduler first drives the syscall handler directly
with `ctx.resuming = true` to finish the call (reap the result, set the return
register, or re-block if it still would). As each reconstructed frame returns,
the scheduler pops the call-history entry the skipped epilogue would have popped
and advances to the next-outer frame at its post-call pc, until it is below the
outermost recorded frame. One mechanism serves every entry: a fork child, a
`clone`d thread, and any process that blocked in a syscall all resume this way;
only a fresh or just-`execve`'d process (`replay == None`) enters at its program
entry instead.

**Process states and their wake sources.** A process is `Runnable`, `Blocked`,
`Zombie(code)` (exited, awaiting `wait4`) or `Dead` (reaped, or a retired
thread — nothing can `wait4` a thread). `Blocked` carries what it is waiting on,
and each reason names exactly one thing that can end it:

| `BlockedOn` | woken by |
|---|---|
| `Wait` | a child of this process becoming a zombie |
| `PipeRead(i)` | a write or a last-writer close on that pipe (another guest process) |
| `UnixAccept { listener }` | another guest process's `connect` to that named AF_UNIX socket |
| `Futex { uaddr }` | a `FUTEX_WAKE` on the same word of a shared segment |
| `Socket { host_fd, write }` | the **host**: readiness observed by the idle poll, or by a running process |
| `Sleep` | the **clock** (its absolute deadline), or a posted signal (→ `EINTR` + remaining time) |
| `Poll` | anything that can change readiness — a pipe write, a posted signal — after which the waiter re-scans its interest list |

Only the socket and clock cases come from outside the guest; everything else is
posted synchronously by another cooperatively-scheduled process, which is why a
missing wake there is a hard deadlock rather than a slow path. Over-waking is
safe by construction — a woken process re-checks its condition and re-blocks —
so `Poll` waiters are woken as a class rather than tracked per fd.

**The idle path.** When the run queue drains with nothing runnable, the
scheduler is not finished; it is idle, and there are exactly two wake sources
left. Servicing either alone starves the other, so they are serviced by one
wait: if anything is parked on a socket, sleep once inside a host `poll_oneoff`
over those fds *bounded by the earliest guest deadline*, then sweep the clock.
With no socket waiters, sleep to the earliest deadline (capped at 5 s, so an
absurd guest timeout cannot wedge the module) and sweep. Only when nothing is
runnable, nothing waits on the host, and no deadline exists is the state
terminal — and even then, processes still `Blocked` is a **deadlock**, reported
with the full `(pid, blocked_on)` list and exit code 111, never a silent exit 0.
Otherwise the module exits with init's status.

Fairness among socket waiters is a rotation, not a queue order: N nginx workers
blocked in `accept` are N waiters on the *same* host fd, the host reports them
all ready at once, and collected in plain index order the first one wins every
time (measured: 100 requests at 25-way concurrency, all served by one of four
workers, on both libcs). A `wake_cursor` rotates both the idle poll's waiter
list and the wake order used when a running process observes readiness.

**Timeouts belong to one wait.** A deadline is cleared at the start of a *fresh*
syscall and preserved across a *resume*, because the wake that brought a process
back may have been spurious and its timeout is still running; a waker never
clears it. The scheduler sets `timed_out` when it releases a process because the
clock passed rather than because someone woke it, which is how the resuming
handler tells `ETIMEDOUT` from success.

**What a context switch moves.** `State` is copied out of and into the one live
register file. The fd table, cloexec/nonblock flags, cwd and signal dispositions
are *moved* by thread-group holder, so a descriptor one thread opens is the same
descriptor to the next thread scheduled; the signal mask and thread-directed
queue are per task and travel with the task alone. Arena buffers are traded
rather than copied, and skipped entirely between threads of one group (they are
the same address space) and when the same process is re-loaded. Dispatch tables
are rebuilt only when the incoming process runs a *different program*, since
they are a pure function of the program: rebuilding them per switch cost 164 ms
on nginx and was the real price of a switch — the 384 MiB arena restore, the
obvious suspect, was ~15 ms of it.

**Signals are delivered at boundaries, synchronously.** There is no
`rt_sigframe` and no asynchronous interruption of arbitrary guest code. Pending,
unblocked signals with an installed handler run at a delivery boundary (chiefly
`epoll_pwait` and the other waiting syscalls) as an ordinary guest call on the
interrupted process's own stack with a cloned `State`. That is the path that
makes PostgreSQL's postmaster advance: a child exits → `SIGCHLD` is posted and
the postmaster is woken from its epoll → the handler runs and `SetLatch()`s →
the latch signalfd becomes readable → the same `epoll_pwait` returns and the
main loop reaps.

**Exit is a scheduler operation.** `exit_group` (and `exit` from the last task
in a group) retires every member, leaves the leader as a `Zombie` carrying the
status, closes all descriptors — which is what gives a pipe reader EOF — drops
the group's shared mappings, wakes a parent parked in `wait4` and posts
`SIGCHLD`. `exit` from one thread of a live group retires that task only and
touches none of the group's shared state. Either way the module keeps running;
it exits when the scheduler has nothing left to run.

### Repository layout

| path | what it is |
|---|---|
| `internal/image` | entrypoint-closure discovery over an OCI image |
| `internal/fuse` | build-time dynamic linker (the hard part) |
| `internal/rootfs` | rfs sidecar writer + boot record |
| `internal/translate` | translate-one driver, translation identity, object cache |
| `internal/builder` | the in-image steps: `build-image`, `bazel`, `translate-one`, `link-all` |
| `internal/pipeline` | the host-side end-to-end driver: `build` and `run` |
| `internal/oci` | packs a module + sidecar into an importable OCI image |
| `internal/serve` | hosts the browser embedder and its artifacts |
| `internal/relay` | the WebSocket relay protocol for browser egress |
| `cmd/raptormark` | the CLI they hang off |
| `internal/link` | registry/fragment C generation, `EcvProgram` ABI |
| `runtime/` | ecvisor — the Rust supervisor (wasm32-wasip1 staticlib) |
| `builder/` | the LLVM companion passes, and the packaging-only Dockerfile |
| `bazel/` | the Bazel rules that build everything the image contains |
| `patches/` | the elfconv fork series (67 patches) |
| `third_party/elfconv` | upstream submodule, pinned clean at `8bfe808` |
| `e2e/` | end-to-end attestation of the whole pipeline |

The fork is kept as an ordered patch series applied inside the builder image,
not as a modified checkout, so the submodule stays clean at its pin and every
patch is individually reviewable.

### Two runtime models

Stage 4 can emit the guest in either of two shapes, and **both come out of one
translation**. Only the final link forks, because every object is compiled
`-fPIC` and a PIC object links flat with an unchanged import surface. Translation
is the expensive half of the pipeline — hours for a large closure — so asking for
the second shape costs about a second of extra linking, not a second pipeline.

```
                     one set of -fPIC objects
                                |
             +------------------+------------------+
             |                                     |
        flat link                     link-all --side-out
             |                                     |
       app.wasm                    supervisor.wasm + one
   (ecvisor linked in)             <program>.side.wasm each
             |                                     |
   a stock runwasi shim                  an embedder you own
```

| | **flat module** | **supervisor + side modules** |
|---|---|---|
| artifact | one `.wasm` with ecvisor linked in | `supervisor.wasm` plus one PIC side module per program |
| host | released `containerd-shim-wasmedge`, unchanged | a host that instantiates several modules and wires them together |
| WASI | supplied by the shim | supplied by that host — 28 imports, 11 of them WasmEdge socket extensions |
| relink after a runtime change | whole module | supervisor only |
| status | **what ships today** | built, tested, and run; no stock shim can load it |

#### Why a stock shim cannot run the split

A released runwasi shim loads a single module file and has no import map. The
side modules import `memory`, `__indirect_function_table`, `__stack_pointer`,
`__memory_base`, `__table_base` and the supervisor's intrinsics from `env`, and
nothing on that path supplies them. That is the whole obstacle — not the module
shape, which stays inside Wasm 2.0 and needs no proposal.

#### What an embedder has to do

The sequence is in `.agents/docs/MULTIMODULE.md` §8 and is executed on every E2E
run by `e2e/testdata/embedder.mjs`:

1. instantiate the supervisor with the WASI imports;
2. read each side module's `dylink.0` MEM_INFO;
3. call the supervisor's `ecv_reserve_side(size, align)` for its memory —
   **not** `memory.grow` and **not** `__heap_base`, both of which let the
   supervisor's own allocator hand the same bytes out again later;
4. take `__table_base` from the current table length and grow the table;
5. instantiate the side module against the supervisor's memory, table and
   shadow stack;
6. call `__wasm_apply_data_relocs()` then `__wasm_call_ctors()`;
7. read the exported `ecv_program_<i>` global — it holds the OFFSET from
   `__memory_base`, not an address;
8. `ecv_register_program(addr, sizeof(EcvProgram))`;
9. `_start()`.

⚠️ `e2e/testdata/embedder.mjs` runs under node and is a **development harness,
not a deployment target**: it cannot supply WasmEdge's socket extensions, and
`node:wasi` needs a private-symbol workaround to survive the guest growing linear
memory at all. It exists so the protocol is executed rather than described.

The same nine steps also run **mid-run**, in response to the guest rather than
before it — that is what dynamic side-module loading is
(`e2e/testdata/hostedembedder.mjs`). Only steps 1 and 9 differ: the host drives
`ecv_boot` and `ecv_run_slice` instead of `_start`, because `_start` returns only
when the guest is finished and so leaves no window in which to instantiate
anything; and registration lands on `push_late` rather than the frozen registry.
Two rules come with it. A load must be served BETWEEN slices, never from inside
the `ecv_host_load_side` import — a slice is on the wasm stack there, holding
`&mut` on the process table that `ecv_side_loaded` mutates. And a library unit
imports `GOT.mem._ecv_entry_func`, which a main program does not: it has no entry
function, so elflift emits none and the descriptor's `.entry_func` is an
undefined symbol. The flat link resolves it to 0 through `--allow-undefined`; a
host must supply the same 0.

#### What is settled and what is not

Settled: the protocol works, at real scale. CPython — a 36 MB side module needing
5.75 MB of memory and 12,115 table entries — runs through the sequence above and
prints the same checksum as the native interpreter and as the flat module.
Nothing in the placement turned out to be scale-sensitive.

Not settled: **the runtime cost of the split**. It is not a call-overhead
multiplier but the loss of inlining across a boundary that only exists once the
split does, and it cannot be measured in advance — wasmedge cannot instantiate
two modules, and V8 inlines the cross-module import away. The number appears
after a host is built, not before.

## Building and running

Build the toolchain image (base + fork + ecvisor + LLVM passes):

```sh
git submodule update --init
go run ./cmd/raptormark build-image                        # LLVM 16 line, tags :latest
go run ./cmd/raptormark build-image --llvm 22              # LLVM 22 line
go run ./cmd/raptormark build-image --skip-base --tag mytag
```

Since 2026-08-23 the image's contents are built by **Bazel**, not by `RUN` lines
in the Dockerfile. `builder/Dockerfile` has none left: it is `COPY . /` over
`//builder:stage`, which declares every file the image gains at the path it has
in the image. Bazel runs *inside* the elfconv base image, because the LLVM
companion tools must link against the same LLVM 16 that built `elflift`:

```sh
go run ./cmd/raptormark bazel --image raptormark-elfconv-base-patched:<tag> build //builder:stage
go run ./cmd/raptormark bazel --image raptormark-elfconv-base-patched:<tag> test //...
```

Bazel itself is mounted in from the host (the image has none), so put
`bazelisk` or `bazel` on `PATH`, or set `RAPTORMARK_BAZEL`.

The same targets also build with **no Docker at all**, against LLVM 16.0.6 and
wasi-sdk 24.0 that Bazel downloads and verifies by sha256:

```sh
bazel build --config=hermetic //builder:stage
```

The two modes are not assumed to agree — `builder/hermetic_differential.sh`
measures it. Six of the ten staged artifacts come out byte-identical; the four
LLVM tools do not, because Debian ships one shared `libLLVM` where upstream
ships static components; and the objects those tools emit **are** identical,
which is the property the object cache depends on.

That the move changed nothing is checked rather than asserted:
`//builder:tools_equivalence_test` and `//runtime:cshim_equivalence_test`
rebuild each artifact using a transcription of the `RUN` line that used to
build it and compare byte-for-byte.

`build-image` also cross-builds this same binary for the image's platform and
installs it as the entrypoint, so `translate-one` and `link-all` — which run
*inside* the image — are subcommands of the same binary:

```sh
go run ./cmd/raptormark --help
```

### Building and running an image

`raptormark build` runs all four stages and writes a module plus its sidecar;
`raptormark run` executes the pair:

```sh
go run ./cmd/raptormark build postgres:17   --out /tmp/pg --builder raptormark-builder:mytag   --object-cache "$PWD/.agents-workspace/objcache"

go run ./cmd/raptormark run /tmp/pg/app.wasm
```

⚠️ `--builder` is **required** and deliberately has no default:
`raptormark-builder:latest` is not necessarily the newest builder, and a stale
one fails deep inside elflift with an error that reads like a defect in the
input. `--object-cache` is not required and you want it anyway — without it
every run re-translates from scratch, which is hours on a real closure.

**The entry program is the first of the image's ENTRYPOINT/CMD seeds that is a
program in the closure**, which for a real image means "the program the
entrypoint script runs" — `postgres:17` resolves to
`/usr/lib/postgresql/17/bin/postgres`, not to its `docker-entrypoint.sh`. An
image that names no program seed at all (`ruby:3-slim`, whose only seed is the
`irb` script) is refused with a message naming `--entry`; pass the program the
script ultimately runs.

A build also prints any shell reference discovery could not turn into a path:

```
discovery: 13 unresolved shell reference(s) in 4 script(s), 1 of them a computed EXEC target
  command /docker-entrypoint.sh    "$f"
```

⚠️ **That is a diagnostic, not an error.** Most entries are benign — a computed
path often names a config file, and a `"$f"` from a `/docker-entrypoint.d` loop
is usually already covered, because such a directory is enumerated and its
scripts followed. It is printed so an image whose entrypoint computes a program
name says so, instead of the missing program surfacing much later as an `execve`
that silently falls back to program 0.

By default `build` also discovers dlopen-able plugins and fuses each as its own
unit (`--plugins none` turns that off). Discovery finds more than a build
plants: on a `postgres:17`-derived fixture, 5 plugins, 3 of them the base
image's own OpenSSL modules.

`run` exists because one combination has to be right and blames the guest when
it is not — the sidecar must sit in the preopened directory and
`RAPTORMARK_ROOTFS` must name the **guest** path. Get it wrong and ecvisor
reports the rootfs "set but unreadable", runs with no exec map and no dlopen
map, and every `execve` falls back to program 0 while every `dlopen` fails with
"cannot open shared object file".

### The end-to-end suite

It is the pipeline's attestation, not its driver. Env-gated rather than
build-tagged, so it still compiles and vets normally and skips cleanly on a
machine without Docker:

```sh
RAPTORMARK_E2E=1 RAPTORMARK_BUILDER=raptormark-builder:mytag \
  RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache" \
  go test ./e2e/ -v -timeout 60m

# including the ~30-minute fused-guest lifts
RAPTORMARK_E2E=1 RAPTORMARK_E2E_SLOW=1 \
  RAPTORMARK_BUILDER=raptormark-builder:mytag \
  RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache" \
  go test ./e2e/ -v -timeout 90m
```

The cache directory is created if absent, so a wrong path does not error — it
just makes every run cold. A third variable, `RAPTORMARK_NODE`, is needed
wherever node is not on `PATH`; without it ten tests skip and the suite still
reports a clean pass. See `.agents/docs/QUALITY_GATE.md` §5, which is the one
place that records the working values and the expected pass/skip counts.

Host-side Go tests (`go test ./...`) need neither Docker nor wasm. Ecvisor's
host `cargo test` and `cargo check --manifest-path runtime/Cargo.toml
--target wasm32-wasip1` are both expected to pass. The latter checks the
shipping configuration; runtime behavior still requires the E2E suite. See
`.agents/docs/QUALITY_GATE.md`.

### Packing an OCI image

`raptormark oci` turns the module and its sidecar into a `wasip1/wasm` image tar
that containerd, Docker and a registry all accept:

```sh
go run ./cmd/raptormark oci \
  --module out/nginx.wasm --sidecar out/rootfs.img \
  --from nginx:alpine \
  --ref ghcr.io/me/nginx-wasm:latest -o img.tar

ctr images import img.tar
ctr run --rm --runtime=io.containerd.wasmedge.v1 ghcr.io/me/nginx-wasm:latest x
```

`--from` lifts the personality (env, cwd, argv) off the source image; `--env`,
`--cwd`, `--user` and `--arg` override individual fields.

The image's `os` is `wasip1` by default, matching runwasi's own `oci-tar-builder`
and the demo images containerd is known to accept. Docker documents its Wasm
workloads as `--platform=wasi/wasm`, so `--os wasi` is there for that; the
architecture is always `wasm`, and that is the only field the shim actually
tests (`Arch::Wasm` in `containerd-shim-wasm`'s `client.rs`). Note that Docker's
Wasm support *is* runwasi — Docker Desktop installs `io.containerd.wasmedge.v1`
and friends — so there is one target here, not two. It does require the
containerd image store; with the classic overlay2 store `docker load` rejects
any non-linux image regardless of which `os` string it carries.

**The released `containerd-shim-wasmedge` runs these unmodified** — no fork, no
patched engine, no proposal flags. Measured against containerd 2.2.1 with
runwasi's own shims (`e2e/containerd_test.go` runs containerd in a container to
check): `ctr images import` accepts the tar, containerd records `wasip1/wasm`,
the shim logs *"found manifest with WASM OCI image format"* then *"no WASM layers
found in OCI image"* and takes the file-in-rootfs path, ecvisor reads the sidecar
and echoes the boot record, and the guest exits 0.

`containerd-shim-wasmtime` loads the default module but cannot run it, for an
unrelated reason: ecvisor imports WasmEdge's non-standard preview1 socket
extensions (`sock_open`, `sock_accept` — `runtime/src/net/wasmedge.rs`), which
wasmtime's WASI p1 does not provide. That dependency is old; it was just
invisible while wasmtime rejected the module at parse time.

**A `--profile loopback` module does run on stock wasmtime.** The socket calls
go through a `NetBackend` seam (`runtime/src/net`) whose implementation is
chosen at COMPILE time — a `cfg` type alias, deliberately not a `dyn` or an
enum, either of which would keep every backend live and put every import set in
the module. Linking against the loopback archive takes the probe guest from 28
imports to 15 and `wasmtime` 46.0.1 runs it with no flags
(`e2e/loopback_test.go`). Its network is in-process only, so this is a
portability proof and a browser stepping-stone rather than a way to run a
networked guest under wasmtime.

This is a constraint on the module, not luck. Until 2026-08-09 both shims
rejected every raptormark module with *"This instruction or syntax requires
enabling Exception Handling proposal"*, because `link-all` finalised with
`wasm-opt --translate-to-exnref`. Enabling it shim-side was never really on the
table: runwasi pins `wasmedge-sdk 0.14.0`, whose `CommonConfigOptions` has no
exception-handling field at all, so it meant forking the shim *and* migrating an
engine SDK, for a shim only we could run. Instead, suspension became a plain
return (elfconv patch 0026), and the emitted module now uses nothing beyond
Wasm 2.0. Keep it that way — `TestWasmOptEnablesNoProposal` guards it.

| `--format` | layers | what the shim must do |
|---|---|---|
| `rootfs` (default) | one ordinary layer holding module + `rootfs.img` | nothing — image handling is stock |
| `wasm-layers` | module as `…wasm.component.layer.v0+wasm`, sidecar as `…raptormark.rootfs.v1+rfs`, empty rootfs | accept the rootfs media type and write it out as a file — **no such shim exists or is planned** |

ecvisor loads the sidecar with `fs::read`, so it has to be a file the guest can
open. `rootfs` puts it at `/rootfs.img` and names it in `RAPTORMARK_ROOTFS`, so
the module finds it regardless of cwd. `wasm-layers` has no filesystem at all,
which is the extra work; `oci` warns when you ask for that combination.

## Status

Working today, verified end to end under wasmedge:

- Static aarch64 binaries → WASI module, both on elfconv's upstream C++ runtime
  and on ecvisor.
- Multiple programs linked into one module without symbol collision.
- **Dynamically linked guests.** A Debian aarch64 `openssl`, fused with its full
  shared-library closure and running on ecvisor with a real rootfs and a real
  argv, prints the correct version and exits 0. With the bring-up tables in
  place, provider-dependent commands (`list -providers`, `dgst`) work too.
- **Network servers on both libcs.** Alpine/musl and Bookworm/glibc nginx serve
  real HTTP requests with four workers. Timed `epoll_pwait`, non-blocking
  accept/send/recv, and blocking/non-blocking connect semantics have focused E2E
  guards with native Linux baselines where applicable.
- **PostgreSQL 17 with concurrent guest processes.** One module containing
  dash, initdb, postgres (with extensions), and psql completed initdb and served
  8 simultaneous clients over a guest AF_UNIX socket, with 8/8 clients exiting
  0 and 8/8 rows committed. The measured 2 -> 4 -> 8 ladder peaked at 948 MiB
  under bounded snapshots. **Bounded snapshots became the default on 2026-08-22
  and the flag was removed the same day** — see "Next, in order" below for the
  ruling; without them the full-buffer scheme dies with `memory allocation of
  402653184 bytes failed` the moment psql forks as a fourth process, so this
  result is not reachable on that path at all.
- **Cross-program library partition reuse (opt-in).** On the measured postgres,
  initdb, and dash closure, total translation fell from 66m43s to 46m21s and the
  marginal cost after the first program fell 3.3x. This path requires shared
  names and closure library ranges and is not yet a default build mode.
- Content-addressed object cache: a re-run of `TestUpstreamRuntime` went
  45.16 s → 1.23 s, with the assertion on the *served artifact*, not on the hit.

Not there yet:

- **Threads work; musl's thread bring-up does not.** `clone(CLONE_THREAD)` is
  implemented — a real shared-VM task in the caller's thread group, sharing one
  arena, one fd table, one cwd and one signal state, which the cooperative
  scheduler makes safe because it only switches at a block, a yield or an exit.
  A static-glibc guest creates threads, joins them, shares descriptors both ways
  and keeps separate thread pointers (`e2e/threads_test.go`).

  It does not yet unblock `redis:7-alpine`. A fused image never enters ld.so, so
  musl's `__init_tp` and `__init_tls` never run: `libc.can_do_threads` is now
  seeded from `.ecv.musltp`, but `libc.tls_size`/`tls_align`/`tls_head` are still
  zero, so `__copy_tls` hands `clone` a TLS pointer computed relative to zero.
  Everything before that works, including the config load and
  `Running mode=standalone`. See `.agents/docs/TODO.md`.
- **A fused glibc never reads its auxv.** `_dl_aux_init` runs only on glibc's
  static path and the shared path reads the auxv in `dl_main`, which lives in
  ld.so, which eager binding means the guest never enters. Every `GLRO` field
  that exists only to hold an auxv value therefore stays zero, and the fix for
  each is to write it directly from a prelinker table — `_dl_stack_*` and
  `_dl_minsigstacksize` are done, and any future one will be found the same way:
  as a guest that aborts somewhere unrelated-looking.

  `python:3-slim` used to stop on exactly that, aborting in
  `sysconf(_SC_MINSIGSTKSZ)` after all twelve of its constructors had run. It now
  runs Python, imports and third-party native extensions included:

  ```
  $ python3.14 -c 'print(json.dumps({...}))'
  {"py": "3.14.6", "sum": 5050, "sorted": ["a","k","m","o","p","r","t"], "cs": 3.5}

  $ python3.14 -c '... cryptography: Ed25519 sign+verify, AES-GCM ...'
  CRYPTO2-OK ed25519sig 64 aesgcm secret-payload
  ```

  `cryptography` 50.0.0 works, with SHA-256 matching the host byte for byte.
  Dynamically linked OpenSSL works too — python's `_ssl`/`_hashlib` pull in
  `libssl.so.3` and `libcrypto.so.3` as ordinary DT_NEEDED, and
  `_hashlib.openssl_sha256` matches the host oracle. A full TLS handshake does
  not yet: it reaches libcrypto's SIMD and stops on two unlifted instructions
  (`usubw`, `cmlt`); see `.agents/docs/TODO.md`.
- ✅ **There is an end-to-end driver now.** `raptormark build <image> --out DIR
  --builder <img>` runs all four stages — discovery, fuse, translate, link — and
  writes `app.wasm` plus its `rootfs.img`. Measured on a `postgres:17`-derived
  fixture: 1 program, 5 dlopen units, shared layout, and the module runs under
  wasmedge with the guest's own dlopen checks passing (`internal/pipeline`,
  `e2e/pipeline_test.go`).
  ⚠️ `--builder` is REQUIRED and deliberately has no default, for the reason
  AGENTS.md gives: a stale builder fails deep inside elflift with an error that
  reads like a defect in the input.

  `raptormark run <module>` executes the result — it finds the sidecar beside
  the module, preopens its directory and sets `RAPTORMARK_ROOTFS` to the GUEST
  path. That combination is the point of the command: get it wrong and ecvisor
  reports the rootfs "set but unreadable", runs with no exec map and no dlopen
  map, and then every `execve` falls back to program 0 and every `dlopen` fails
  with "cannot open shared object file" — both of which look like a defect in
  the guest. The three runtimes spell the directory flag three different ways,
  and none of them fails loudly when the order is swapped — the runtime opens
  *something* and the guest simply cannot find its sidecar:

  ```
  wasmedge --dir    GUEST:HOST      e.g. --dir    /:/out
  wasmtime --dir    HOST::GUEST     e.g. --dir    /out::/
  wasmer   --volume HOST:GUEST      e.g. --volume /out:/
  ```

  wasmer takes the wasmtime ORDER with the wasmedge SEPARATOR; its
  `--mapdir GUEST:HOST` still runs but is deprecated as of 7.3.0.
  `internal/pipeline`'s `runtimeArgs` is the one place that knows, and
  `e2e/wasixnet_test.go` drives a real wasmer through it rather than spelling
  the argv itself, so the two cannot drift apart while both stay green.
- ✅ **Plugin discovery is wired into that driver.** `raptormark build
  --plugins auto` (the default) calls `image.Plugins` and fuses each result as
  its own dlopen-able unit; `--plugins none` fuses only the closure's programs.
  ⚠️ Discovery finds more than a build plants: on that fixture, 5 plugins, of
  which 3 are `debian:trixie-slim`'s own OpenSSL modules. That is the policy
  working, not a bug, but it means a unit count is not a plugin count.
- ✅ **`--profile wasix` HAS REAL NETWORK EGRESS, and it is the only profile
  besides the default that does.** A raptormark module instantiates and runs
  under stock `wasmer run --net`, and its guest both accepts an inbound
  connection and dials out — the same two kernel-pinned guests `net_test.go`
  fixes against a real Linux, driven from the host
  (`e2e/wasixnet_test.go`). ⚠️ **`--net` is not optional and its absence is
  quiet**: without it `sock_open` SUCCEEDS and `sock_bind` returns errno 58, so
  the guest fails to bind an address that is perfectly fine. `raptormark run
  --runtime wasmer` passes it.
  * `TCP_NODELAY` works here and cannot be expressed under WasmEdge at all,
    which has no TCP option level — so nginx's `tcp_nodelay on` is inert on the
    shipping profile and live on this one.
  * The ABI is written down in `.agents/docs/WASIX_ABI.md`, measured rather than
    read, because three of its traps produce a module with a perfect import
    section that silently misbehaves: the address port is **little-endian going
    out and big-endian coming back**, a `poll_oneoff` clock timeout of **0 means
    wait forever** (1 is the immediate probe), and `wasix_32v1.sock_accept` is
    bound to `sock_accept_v2` and takes four parameters.
- **The wasix LOADER half is still unfinished.** WASIX loads a real raptormark
  side module (`rc=0`, a lifted guest built
  by `link-all --profile wasix --side-out`). Two things were needed and both are
  in: elfconv patch 0067 (`--suspend-via-call`) removes the
  `env.__ecv_unwinding` GLOBAL import, which WASIX's linker refuses because it
  requires every `env` import to be a function; and `-Wl,--shared-memory` with
  the `wasm32-wasip1-threads` libc gives the shared memory WASIX supplies.
  ⚠️ The SUPERVISOR is not there yet: wasmer needs the main module to IMPORT
  `__stack_pointer`, which only a PIC/PIE link does, and that needs every object
  to carry PIC relocations — including the precompiled Rust std. A PIC std needs
  nightly and `-Z build-std`. Nothing else blocks it: not exception handling,
  not the threads proposal for lifted objects, not `wasixcc`. Until it lands a
  guest's `dlopen` on this profile returns NULL with a real `dlerror`; a guest
  that does not `dlopen` is unaffected, which is why the socket half above ships
  independently of it.
  ⚠️ `--profile wasix` therefore implies `--suspend-via-call` only when
  `--side-out` is also given. A flat link DEFINES `__ecv_unwinding` rather than
  importing it (`llvm-nm` on `ecv_globals.o` says `D`, and a flat wasix module's
  33 imports contain no `env.*`), so implying it unconditionally would cost
  every socket-only build a cold object cache — it is part of `TranslateID` —
  to remove an import that build never had.
- **Dynamic side-module loading works, on a profile that is not the default.**
  A guest's `dlopen` — or its `execve` — can be served MID-RUN: the guest parks,
  the host instantiates the unit's side module in response, registers it, and
  wakes the guest, whose `dlsym` then resolves out of a unit that was not in the
  module a moment earlier (`e2e/hostedload_test.go`, `e2e/execload_test.go`).
  Handles are real and scoped, so postgres's 78 extensions no longer collide in
  one flat namespace, and `dlerror` carries a real message.
  `e2e/pipelinehosted_test.go` runs the whole combination through
  `raptormark build --profile hosted --side-out` on a dynamic, multi-program
  image: one load served per trigger, asserted as exactly two.
  ⚠️ It needs `link-all --profile hosted`, which imports `env.ecv_host_load_side`
  — a function **no stock runwasi shim supplies**, so such a module would fail to
  instantiate there. The shipping profile keeps the `preloaded` backend and its
  28 imports; `//runtime:loader_exclusion_test` is what keeps the two apart.
- **Wasmtime socket portability.** The DEFAULT module loads but does not
  instantiate: live ecvisor code imports WasmEdge's non-standard preview1
  socket functions, beginning with `sock_open`. `--profile loopback` removes
  them and runs on stock wasmtime, but gives the guest no external network, so
  the gap that remains is a wasmtime-compatible backend that has one.
  ⚠️ `--profile wasix` does NOT close this. It has real egress, but through
  `wasix_32v1`, which only wasmer supplies — so it moves the guest from one
  engine-specific socket ABI to another rather than to a portable one. What it
  settles is that the `NetBackend` seam can carry a foreign socket ABI at all;
  what wasmtime still needs is an ABI wasmtime has.
- **The browser has no egress.** `--profile browser` boots and runs a guest in a
  tab (`e2e/browser_test.go`, Chromium), with DNS answered in-runtime and
  addresses minted by the host. But a browser cannot open a TCP socket, so
  reaching the network needs a WebSocket relay: the client subprotocol exists
  (`web/src/browser/relay.ts`) and **no relay server does**. Only Chromium has
  been exercised; Firefox and WebKit need system libraries the harness cannot
  install.
- **`--format wasm-layers` has nothing to run it.** The default `rootfs` format
  works on stock shims; the wasm-OCI shape does not, and cannot, because ecvisor
  opens the sidecar with `fs::read` and that shape has no filesystem. It would
  take a shim that materialises the sidecar layer as a file, which nobody is
  going to build or accept upstream. The format stays for sidecar-less modules
  and for feeding other tooling; treat it as unrunnable otherwise.
- **Conversion cost.** The pathological 6.5-hour nginx conversion was reduced
  to about 16 minutes by the tail-call lifter fix. Library-scoped reuse makes
  later programs cheaper; cached work is now dominated by elflift and the
  remaining preparation rather than codegen.
- **Instruction coverage.** Each new image still surfaces aarch64 encodings the
  lifter cannot decode — a class of work, not a bug.
- **Size.** Lifting expands roughly `wasm ≈ 6.89 × .text + 2.37 MB`. Fine for a
  server host; too big for tightly bounded edge runtimes.

Next, in order:

1. **~~Decide whether content-stable shared partitioning becomes the default.~~
   DECIDED 2026-08-23: content-stable partitioning IS the default.** The
   precondition — byte-affecting switches keying correctly in the object cache
   (`internal/translate/experimental.go`, 2026-08-18) — was met, and the switch
   is now its inverse, `RAPTORMARK_NO_STABLE_SPLIT`. ⚠️ Host-gated only: no
   builder image on this host contains `ecv-prepare`, so the E2E evidence this
   change is supposed to rest on is owed the next time one is built. The
   shared-name half of the original item is untouched and still opt-in.

   * *Content-stable partitioning* (now the default; was `RAPTORMARK_STABLE_SPLIT`).
     The pass collapsing this list used to ask for is done: `ecv-prepare` merges
     `llvm-link`, internalize/globaldce and namespace-object (default since
     2026-08-13), and the default now folds the split in against a single parse.
     Measured on the 519 MB postgres module: **149 s of the 225 s split is
     parsing what the previous pass just wrote**, ~9% of that closure's
     translation.
   * *Per-library lift caching* (`RAPTORMARK_LIB_RANGES` plus a persistent cache).
     A partition-cache-warm translation is 68% elflift, most of it re-lifting
     libraries another program of the same closure already lifted; the cache is
     now automatic in that regime and keyed per library, so a closure pays once
     per distinct library. Trace-entry
     sharding is not the answer — sampled shards duplicated 80-84% of their
     emitted functions.

   Promoting stable partitioning changed cache identity and invalidated existing
   translated objects. The advice here used to be to batch that with pending
   lifter-patch adoption; by 2026-08-23 the `-fPIC` change had already landed
   unconditionally and no builder image on the host still matched the pipeline,
   so the objects the batching was protecting were unreachable either way.

2. ~~**Decide whether bounded arena snapshots become the default.**~~
   **DECIDED 2026-08-22, by the user: they are the ONLY scheme, and the flag was
   removed the same day.** The measurements this asked for were in — switch cost
   79 us cross-program and 66 us same-program against 42 us and 35 us unbounded,
   and nginx serving four workers and 100 requests under it byte-identically to
   the unbounded run — and what remained was a ruling, not a measurement. The
   ruling was made on the ceiling rather than on the cost: without bounded
   snapshots there are no guest-side clients at all, because the full-buffer
   scheme dies with `memory allocation of 402653184 bytes failed` the moment
   psql forks as a fourth process.

   The full-buffer path is gone from the switch path with it. `SnapshotData::Full`
   and `Arena::snapshot` stay, because a **multi-threaded group** must still take
   a full snapshot — a bounded range set derives from one stack pointer, and a
   sibling thread's stack is live memory that pointer says nothing about. That is
   a property of the process, decided by `is_multithreaded`, not a mode anyone can
   select. `Arena::swap_with` and `Arena::adopt_shared_from` are kept but are no
   longer on any switch path; their tests are their only callers.

   ⚠️ **What was accepted, not closed — and is now harder to close**: the stack
   BELOW `sp` is outside the copied range set (up to 1,307 bytes observed on
   nginx). Those are dead frames under AAPCS64 — aarch64 has no red zone — and
   the observations are samples rather than bounds. Removing the flag does not
   settle this, and it makes it **unsettleable by differential**: there is no
   unbounded run left to compare against, so `RAPTORMARK_ECV_SNAPCHECK` has lost
   its oracle for every single-threaded switch and now reports `NO-ORACLE`
   instead of a fabricated `miss=0`. Closing it in future needs an argument from
   the ABI or an instrumented guest, not an A/B run. That is a real narrowing of
   the options, taken knowingly. See `.agents/docs/TODO.md`.

3. **Resolve cross-program translation concurrency against the caches.** The
   old unused `RunAll` worker pool was deleted. Concurrent cold translations
   can compile the same libraries independently, defeating reuse; a future
   design must coordinate caches before turning idle cores into useful work.

The open-file-description gap that used to be item 4 is closed. A file offset
now lives in a context-global `file_offsets` table -- the level Linux calls a
`struct file` -- so `dup`, `dup2` and `fork` share one position while a second
`open` of the same path gets its own.

### Honest limits

Properties of the shipping runtime that are easy to meet by surprise. Each was
re-verified against the tree on 2026-08-21 and names the code that makes it
true, so a future reader can check rather than trust.

- **Guest `getrandom` and `/dev/urandom` are NOT cryptographic.** Both are
  served from a deterministic xorshift64\* PRNG (`runtime/src/context.rs`, see
  `rng_state` and `rand_bytes`). Any guest doing its own crypto — a TLS
  handshake, a session key, a password salt, a UUID it treats as unguessable —
  draws from it. This is the sharp one: it is not a browser limitation and it
  predates the browser work, it has simply never been stated where someone
  deploying raptormark would see it.
- **`sendmsg`/`recvmsg` on a host socket return `ENOSYS`** (`runtime/src/sys.rs`,
  `sys_sendmsg` / `sys_recvmsg`). Less exotic than it sounds: curl and some Go
  and Rust runtimes use them on ordinary TCP sockets, so a guest can meet this
  without doing anything unusual. The in-runtime AF_UNIX path is unaffected.
- **A compute-bound guest with no syscalls freezes its host thread.** This
  follows from the cooperative scheduler described above — nothing is
  time-sliced, and there is no interrupt to build preemption on. Stated again
  here because the consequence lands differently per host: under wasmedge it
  holds the module, and in a browser it freezes the page thread, i.e. the tab.

A fourth limit belongs in this list historically and is no longer true: guest
deadlines used to be measured against the WALL clock, so a timer followed NTP
steps, laptop suspend and browser tab throttling. Fixed 2026-08-21 — the runtime
now keeps a separate monotonic base, and `TestGuestTimersSurviveAWallClockStep`
steps the host clock by an hour to guard it.

## License

raptormark is licensed under the **Apache License, Version 2.0**. See
[LICENSE](./LICENSE) for the terms and [NOTICE](./NOTICE) for attribution of the
Apache-2.0 upstreams it builds on — elfconv, Remill, MyAOT and LLVM.

Two scope notes that `NOTICE` states in full and that are easy to get wrong:

* **`tools/decode-oracle/` is not covered by that grant.** It is a separate Go
  module, licensed LGPL-2.1-or-later, because it embeds QEMU's decodetree
  tables. It is a developer-only analysis tool that is never built into, linked
  with, or shipped alongside the pipeline; there is no `go.work`, and nothing in
  the root module imports it. Keeping it out is what leaves this project free to
  be Apache-2.0, so ❌ do not fold it back in for convenience.
* **A module raptormark emits is not covered by that grant either.** It carries
  code translated from whatever guest image was supplied, and those contents
  keep their own licences — commonly glibc or musl. Whoever distributes a
  translated module owns that obligation.
