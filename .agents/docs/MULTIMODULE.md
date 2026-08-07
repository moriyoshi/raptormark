# Feasibility: the supervisor and the lifted programs as independent Wasm modules

Explored 2026-08-15. Two proposals were put:

1. **Supervisor as an independent Wasm module** — ship ecvisor as its own module
   rather than as `libecvisor.a` statically linked into every artifact.
2. **Lift executables as independent Wasm modules** — one module per program
   instead of N lifted objects linked into one module.

Short answer: **the module shapes are feasible today and were built and run;
the missing piece is not the toolchain, it is a host that instantiates more than
one module.** The shipping target (released `containerd-shim-wasmedge`) loads a
single module file and has no import map, so a multi-module artifact cannot run
there. The choice is therefore not "can we split the module" but "are we willing
to own the embedder".

**These are not two proposals. Proposal 1 is entailed by proposal 2.** The
moment programs become separate modules, something has to export the linear
memory, the indirect function table, the shadow-stack global and the ~100
intrinsics they import — and that something is ecvisor. There is no arrangement
in which programs are modules and the supervisor is not. So the supervisor split
should be costed as part of proposal 2, not weighed on its own merits; §3 below
records what it is worth *standing alone* only because that is the motivation
usually given for it, and the answer is "much less than you would think".

Everything below is measured on this tree unless marked otherwise.

## 1. The boundary that already exists

The lifted code and ecvisor are already coupled through a small, named, and
entirely function-shaped interface. Measured on a cached lifted object
(`llvm-nm --undefined-only` on `openssl_ecv.o`, 149 MB):

* ~100 undefined symbols, of which the live ones are the `__remill_*` semantics
  and memory helpers, five `_ecv_*` hooks (`_ecv_suspended`,
  `_ecv_save_call_history`, `_ecv_func_epilogue`,
  `_ecv_get_indirectbr_block_address`, `_ecv_unreached`), `__ecv_warning`, and
  libm plus `memset`. The remaining ones are dead x86/SPARC intrinsics remill
  declares unconditionally.
* Exactly one defined symbol needs to be visible outwards: the program's
  `EcvProgram` descriptor (`ecv_program_<i>`), out of 48,868 defined symbols.
* One non-function dependency: `__stack_pointer`, the shadow-stack global.

Two properties make this boundary unusually well-suited to a module split:

* **Guest memory is reached through a parameter, not through a module global.**
  `read_memory64(rt_m, arena_ptr, addr)` expands to `*(uint64_t *)(arena_ptr +
  addr)` (`third_party/elfconv` `backend/remill/include/remill/Arch/Runtime/
  Operators.h`; the `__remill_read_memory_*` calls are the `MEMORY_INSTRUMENT`
  debug branch, not the shipping one). `arena_ptr` is argument 0 of every
  `LiftedFunc`. So the hot path is unaffected by where the module's own data
  lives.
* **`--allow-undefined` already turns the ecvisor dependencies into imports.**
  `link-all` passes it as "transitional link-slack"; what it actually does is
  emit each unresolved symbol as an `env` import. This was observed
  accidentally: linking the openssl object against a stale `libecvisor.a`
  produced a module that wasmedge rejected with *"unknown import, When linking
  module: `env`, function name: `_ecv_suspended`"*.

Linking that same object standalone — `--no-entry --export=ecv_program_0
-nostdlib --allow-undefined` — takes **1 second** and produces a 125 MB module
with **17 `env` imports** and nothing else. A lifted program is already almost a
well-formed side module.

## 2. Programs as independent modules: built and run

`.agents-workspace/multimodule-poc/` holds a working two-module PoC that mirrors
the real ABI shape. `build-and-run.sh` builds both modules with the builder
image's own wasi-sdk 24 clang and runs them together.

* **Supervisor module** — ordinary, non-PIC. Owns and exports `memory` and
  `__indirect_function_table` (`--export-memory --export-table
  --growable-table`), and exports the intrinsics.
* **Program module** — imports `env.memory` and `env.__indirect_function_table`
  (`--import-memory --import-table`), imports the intrinsics from `env`, places
  its own data and table entries at a build-assigned base (`--global-base`,
  `--table-base`), and exports nothing but `__wasm_call_ctors`.
* **The import cycle is broken by registration, not by imports.** The supervisor
  never imports from a program. Each program module calls
  `env.ecv_register_program(entry, entry_pc)` from a constructor at
  instantiation. Instantiation order is supervisor first, then each program.
* **Result.** The supervisor calls the program's lifted function through the
  shared table; the program reads guest memory both by direct `arena + addr`
  arithmetic and through a supervisor intrinsic, and the two agree:
  `ecv_run -> 0x1234500000400100 MATCH`.
* **Feature level.** Both modules report `mutable-globals, sign-ext` — inside
  Wasm 2.0. Splitting costs no proposal, so `TestWasmOptEnablesNoProposal` and
  the stock-shim property are not put at risk by the module shape itself.

**Neutralized.** Rebuilt with `GLOBAL_BASE=1024` so the program's data collides
with the supervisor's, the run prints
`ecv_run -> 0x1234500000000001 MISMATCH`. That is the intended diagnostic and it
proves the two modules really do share one linear memory and that the
build-assigned layout is what keeps them apart. The parallel table-base
neutralization (`TABLE_BASE=0`) did **not** fire, because slot 0 of the
supervisor's table is unused — a weaker control, recorded as such.

### The shadow stack forces PIC

The one thing the non-PIC shape gets wrong is `__stack_pointer`, and it matters
more than it looks:

| build | `__stack_pointer` |
|---|---|
| non-PIC, `--import-memory --import-table` | **defines its own** `(global $__stack_pointer (mut i32) (i32.const 66560))` |
| `-fPIC` + `wasm-ld --experimental-pic -shared` | **imports** `env.__stack_pointer` as a mutable global |

A per-module shadow stack means N separate stacks in one linear memory. Against
this tree that is not cosmetic:

* `link-all` sets `-z stack-size=16777216` because entry.rs reserves a 512 KiB
  per-guest gap and guest recursion is deep. 16 MiB per program module, times
  71 programs for `postgres:17`, is 1.1 GiB of a 4 GiB wasm32 address space that
  already peaked at 4010 MiB on a real postgres run.
* `ecv_cur_sp()` (`/opt/ecvisor/ecv_sp.o`, wasm inline asm) probes *the
  supervisor's* `__stack_pointer`. With per-module stacks the
  `RAPTORMARK_ECV_LEGSP` low-water probe and the runaway-recursion guard would
  silently observe the wrong stack.

So the correct shape is PIC side modules, which also import `__memory_base` /
`__table_base` and export `__wasm_apply_data_relocs`, letting the loader assign
layout instead of the build. PIC builds and links cleanly on wasi-sdk 24 and
stays inside Wasm 2.0. Its code cost is confined to module-local data
references — `global.get $__memory_base` + `i32.add` per access — which for
lifted code means the cold tables (`_ecv_fun_vmas`, the block-address arrays,
`_ecv_data_sec_bytes`), not the guest loads and stores.

`drive-pic.mjs` runs that variant, and it works. The program module built with
`clang -fPIC` + `wasm-ld --experimental-pic -shared` imports exactly:

```
env.memory  env.__indirect_function_table  env.__stack_pointer (mut i32)
env.__memory_base  env.__table_base  + the intrinsics
```

and the supervisor exports every one of them, `__stack_pointer` included
(`wasm-ld --export=__stack_pointer` exports the mutable global; verified). The
run prints `ecv_run -> 0x1234500000400100 MATCH` and
`shadow stack shared: true (sp before 70688 / after 70688)` — the program
module's frame is pushed and popped on the *supervisor's* shadow stack, which is
the property the whole design turns on. Note the first PIC build imported no
`__stack_pointer` at all, because its one function was a leaf that never touched
the shadow stack; the guest was given a stack frame so the import would appear.

### What it would buy

* **The namespacing and whole-module passes exist only because of the single
  link.** `namespace-object` tags every local symbol with a module id, and
  `opt -passes=internalize,globaldce` reduces each program to one exported
  symbol, purely so N objects can share one link without colliding. Separate
  modules have separate symbol spaces and need neither. That is item 1 of the
  README's "Next, in order" — collapsing the serial whole-module passes over a
  500-600 MB module.
* **Per-program artifacts.** A program module is independently cacheable,
  shippable and diffable, and adding a program to an image stops touching the
  others' bytes.

### What it would not buy

* **Not memory.** Each program module's data still occupies the same shared
  linear memory it occupies today. The openssl program module alone declares 615
  pages (~40 MiB).
* **Not engine load time, on the evidence available.** wasmedge parsed and
  validated the whole 120 MB linked module in about 2 s before failing at the
  import-link stage. Lazy per-program instantiation is not obviously worth much
  here; it would need a real measurement against an AOT-compiled module before
  being claimed.

## 3. Supervisor as an independent module

### 3a. As a consequence of §2, which is the real case

The supervisor is the anchor of the multi-module design, not an optional second
step. What it has to become is a module that:

* **exports the memory and the table** (`--export-memory --export-table
  --growable-table`), or imports them from the embedder if the embedder is to
  own them;
* **exports `__stack_pointer`**, so every program module shares one shadow stack
  rather than reserving its own (see §2);
* **exports the intrinsic surface explicitly.** Today those symbols are resolved
  inside one link and `--gc-sections` drops the unused ones. Linked without any
  guest object, each one has to be an explicit `--export=` — which also makes it
  a GC root, so the supervisor module keeps intrinsics no program calls. About
  100 exports; `internal/link` is the natural place to generate the list, and it
  is the natural replacement for the work `RegistryC` does now.

Two things this does **not** cost, both worth knowing before pricing the work:

* **No change to how ecvisor is built.** As the non-PIC "main" module it stays an
  ordinary `cargo build --target wasm32-wasip1` staticlib; only the *program*
  modules need `-fPIC`. (Making ecvisor itself a PIC side module — so the
  embedder owns memory, table and stack pointer and no module is privileged —
  would mean building Rust `std` with `-Crelocation-model=pic` via `-Zbuild-std`,
  which is a real and separate cost. The demonstrated arrangement avoids it.)
* **No change to the exec map.** `Programs::load` builds `by_hash` from each
  registered program's name, so registration order is irrelevant and the
  path -> hash -> index resolution is untouched.

One thing it does cost, and it is the largest single code change implied by
either proposal: **the static registry has to become a dynamic one.**
`runtime/src/abi.rs::registry()` reads three link-time symbols — `ecv_programs`,
`ecv_program_count`, `ecv_program_size` — that `internal/link.RegistryC`
generates. A supervisor linked before the program modules exist cannot see their
descriptors, so the array must be populated at instantiation by each module
calling in (`ecv_register_program`, as in the PoC). Most of `RegistryC` goes
away. The `ecv_program_size` ABI-drift guard survives and gets *stronger*: today
one static value stands for N separately compiled objects, whereas each module
would present its own `sizeof(EcvProgram)` at registration.

### 3b. Standing alone, on build cost, it is not worth doing

This is the motivation usually given for proposal 1 in isolation: a change
confined to `runtime/` should not cost a re-link of a 100 MB artifact. Measured
on the largest cached fixture (openssl, 149 MB object, builder image, cold):

```
wasm-ld link (149 MB object + registry.o + libecvisor.a -> 127 MB module)   1 s
wasm-opt -g -O0 round trip (127 MB -> 120 MB)                              52 s
```

So the entire cost a supervisor split could remove is ~53 s, and **52 of those
53 seconds are wasm-opt, not the link**.

**CORRECTION 2026-08-15.** This section first said that wasm-opt "does nothing"
and that "its only effect is re-emitting the name section". That was wrong, and
the evidence for it was in the same measurement: the module went
**127,354,502 -> 120,298,637 bytes, a 5.5% shrink**. Binaryen does report
`warning: no passes specified, not doing any work`, and that line was read as
the whole story — but it re-encodes wasm-ld's padded LEBs on the way through, so
the 52 s buys 5.5% of module size. The lesson is the tree's own rule about
trusting a tool's self-report over the artifact it produced.

So it is a size/time trade, not a free deletion. It is now opt-in rather than
removed: `ECV_WASM_OPT=0` makes `finalise` rename the pre-module into place, for
the relink-heavy loop where a runtime-only change reuses every translated object
and the final link is the whole cycle. Do not use it for anything shipped or
size-measured. `TestWasmOptCanBeSkipped` and `TestWasmOptRunsByDefault` pin both
directions; all three failure modes were neutralized (skip unconditionally,
never skip, leave the pre-module behind).

The object cache already delivers the rest of the property: `ObjectKey` covers
the base image digest, `TranslateSH`, the ELF and the codegen options, but not
`libecvisor.a`, precisely so a runtime-only change reuses every translated
object.

So: worth doing as the anchor of §2, not worth doing for its own sake.

**And there is a third option that is neither: move the supervisor out of wasm
entirely**, into the embedder, with the lifted modules importing syscalls from
the host. Note this is the one arrangement in which the supervisor is *not* a
module — it dissolves rather than splits. It buys what the in-wasm supervisor
cannot:

* escape from the wasm32 4 GiB ceiling that the arena model is currently pressed
  against (384 MiB per suspended process; a measured postgres run hit 4010 MiB
  and then failed a 512 MiB request),
* one wasm instance per guest process, each with its own memory — which is what
  per-process arenas are emulating today by swapping buffers,
* native-speed VFS, scheduler and socket handling,
* real host threads instead of a cooperative scheduler.

It costs the single self-contained artifact and every property that follows from
it, including the stock-shim result the README leads with.

## 3c. The cooperative scheduler is where the split's cost lands

The supervisor's dispatch loop (`entry.rs`) is not broken by the split. It calls
a `LiftedFunc` pointer and gets a return; suspension has been a plain return
since elfconv patch 0026, so a cross-module leg behaves exactly like an
in-module one — the PoC calls into the program module through the shared table
and returns normally. Threads need nothing extra either: a `CLONE_THREAD` task
is an ordinary entry in the same run queue carrying the leader's `tgid`, and
`func_at` / `func_containing` resolve to function pointers, which are shared-table
indices in both worlds.

What the split *does* do is tax that loop's per-call bookkeeping, and it is the
hottest thing in the module. Counted on the linked bash fixture
(`.agents-workspace/tmp/warm-base/bash.wasm`, 7,567 functions):

| supervisor entry point | call sites |
|---|---|
| `_ecv_suspended` | 33,540 |
| `_ecv_save_call_history` | 32,972 |
| `_ecv_func_epilogue` | 32,115 |
| `__remill_function_call` | 908 |
| `__remill_jump` / `_ecv_get_indirectbr_block_address` | 570 each |
| `__remill_syscall_tranpoline_call` | 1 |

That is ~98,600 sites in 7,567 functions — about **13 per lifted function**, and
dynamically **three calls into ecvisor per guest BL**: push the call history,
pop it at the epilogue, test the suspend flag. All three exist to serve
replay-based resume; none of them is guest work.

Today those are direct calls to a local function. Across a module boundary they
become imported calls, which no engine inlines across instances. So the split's
real running cost lands on the scheduler's bookkeeping, not on guest memory
access — which stays `arena_ptr + addr` and is unaffected (§1).

**Partially quantified, 2026-08-15, by the phase 0 work** (details in the
journal). These are *intra-module* costs under wasmedge's interpreter, and a
cross-module call can only be more expensive than an intra-module one, so they
are a LOWER BOUND on what the split would pay:

| removed | call-heavy guest | realistic guest |
|---|---|---|
| one crossing (`_ecv_suspended` -> a wasm global) | −1.8% | −0.26%, **bands overlap** |
| both call-history hooks, entirely | −31% | −4.7% |

Two things follow. First, the crossings are NOT interchangeable: the cheap one is
worth ~2% and the two call-history ones are worth ~30% between them, because
their bodies do real work. Second, on a realistic workload the whole per-BL
apparatus is under 5%, so the split's penalty on *server-shaped* guests is
smaller than §3c originally implied — while on interpreter-shaped guests, which
the README puts in scope, it is large.

**Still unverified:** the cross-module multiplier itself. That needs an AOT
comparison (`wasmedge compile` on a single-module build against a split pair) and
should be measured before the split is committed to, because it is the one cost
that cannot be designed away afterwards.

### Two of the three crossings are removable now, in both worlds

This is worth doing whether or not the split ever happens:

* **`_ecv_suspended` should be a global, not a call.** Its whole body is
  `(*ctx).unwinding as u8`. If the supervisor exports a mutable `i32` global and
  the lifter emits `global.get` instead of a call, 33,540 call sites become
  33,540 global reads — cheaper than today in the single module, and *free*
  across a module boundary, because an imported global is read inline while an
  imported function is not. Changes `TraceLifter::Impl::AddSuspendCheck` (patch
  0026) and `intrinsics.rs`.
* **`_ecv_save_call_history` / `_ecv_func_epilogue` are a push and a pop wearing
  a lot of diagnostics.** Everything else in them — the recursion alarm, the
  call-site counter, `RAPTORMARK_ECV_WATCH`, the LEGSP probe, the dtrace hooks —
  is gated on flags read once at startup. Lowering the push/pop to inline stores
  into a ring buffer in the arena, with the diagnostics kept behind an
  out-of-line slow path, removes another ~65,000 crossings.

### The loop itself cannot be removed inside Wasm 2.0

Replay exists because wasm has no way to suspend a call stack. The alternatives
are all outside the current envelope:

* **asyncify** — tried and abandoned: mutually exclusive with elflift's
  `--fork_emulation`, and `call_indirect` returns trap entering the unwind state.
* **exception-handling unwind** — tried and abandoned: rejected by every
  released runwasi shim, and an EH unwind returns each function *before* its
  epilogue, so `__stack_pointer` leaked a frame per yield.
* **stack switching / JSPI** — post-Wasm 2.0, and not on the shims.
* **the threads proposal** — the only one that actually deletes the loop. With a
  `shared` memory and one host thread per guest task, a blocking syscall blocks
  its thread, and `call_history`, the suspend checks and the dispatch loop all
  go away — along with `--fork_emulation` in the lifter. It is beyond Wasm 2.0
  and off the stock shims, which makes it **the same decision as owning the
  embedder**. Note it only fixes *threads*: `fork` still needs separate address
  spaces, so arena swapping survives.

So the cooperative loop is a consequence of the Wasm 2.0 constraint, not a design
preference, and "overcome the loop" and "split the modules" are the same
decision arriving from two directions. The ordering that follows is: do the two
crossing removals first, because they pay in the current single-module artifact
and they shrink the split's main cost before it is ever paid.

## 4. The blocker, stated precisely

> ### ⚠️ AMENDMENT 2026-08-24: the first bullet is FALSE UNDER WASIX
>
> "A wasm module cannot instantiate another wasm module" is true of preview1 and
> **not** true of WASIX. `wasix_32v1.proc_spawn2` resolves a name against the
> guest's own filesystem and instantiates it. Measured, not read: a guest
> spawned a hand-built `child.wasm` which printed from its own stdout and exited
> 42, under a stock `wasmer run` -- no embedder, no CLI flag, and none of
> `proc_fork`'s asyncify / shared-memory / `__stack_pointer` requirements.
> `.agents/docs/WASIX_ABI.md`, "The process half", has the probe and the run.
>
> ❗ **It does not unblock §2, and the reason is memory, not loading.** A spawned
> module gets a FRESH linear memory and starts at `_start`. §2's program modules
> must import the SUPERVISOR's memory -- that is the entire design -- and WASIX
> has no way to express it: of the 139 functions in `wasix_32v1` there is not one
> `shm`, `mmap` or memory-sharing call. Two WASIX processes share a filesystem,
> pipes and sockets, and not one byte of memory.
>
> So what §4 loses is the *loader* half of the blocker, on one runtime. The
> shared-memory half is untouched and is the half §2 rests on. The rest of this
> section stands as written.

Nothing above runs on the shipping target, for one reason: **no loader.**

* A wasm module cannot instantiate another wasm module. There is no engine
  inside ecvisor, and WASI preview1 exposes no module-loading call. Riding extra
  modules in as sidecar files — which the rfs sidecar mechanism could trivially
  do — does not help, because ecvisor could read them and could not run them.
* `wasmedge`'s CLI has no `--preload` or module-registration option (checked:
  `wasmedge --help` offers `--reactor` and `--dir`, nothing else relevant).
  WasmEdge's C API does have `VMRegisterModuleFrom*`; the CLI does not surface
  it.
* `wasmtime run --preload NAME=MODULE_PATH` does exist, and would be the natural
  dev harness — except that its preloaded modules are instantiated *before* the
  main module, and here the direction is reversed: program modules import the
  supervisor's memory, so the supervisor must be instantiated first. The
  ordering the design needs requires an embedder, not a CLI flag. (And wasmtime
  cannot run ecvisor at all today, because of the WasmEdge-specific `sock_open`
  imports.)
* `containerd-shim-wasmedge` takes the file-in-rootfs path and runs `_start` on
  one module. Making it link a second module is strictly more shim work than
  making it materialise the sidecar layer as a file, which the README already
  records as something "nobody is going to build or accept upstream".

Build-time merging is not a way around this. wasm-ld *is* the merge tool, a
relocatable `.o` *is* a wasm module, and merging PIC side modules back into one
module means re-implementing relocation. Binaryen's `wasm-merge` is present in
the builder image but does not apply the dylink relocation scheme; it is not a
substitute for the linker.

## 5. Recommendation

There is one decision here, not two, because proposal 1 is proposal 2's anchor.

1. **Do not split the supervisor as a way of cutting build cost** — that is the
   one framing in which it stands alone, and it does not pay. Make the 52-second
   wasm-opt step skippable instead. **DONE 2026-08-15**: `ECV_WASM_OPT=0` renames
   the pre-module into place; default unchanged, so
   `TestWasmOptEnablesNoProposal` still guards the flags for every shipped build.
   It is a 52 s / 5.5% trade rather than the free win this section first claimed
   (see the correction in §3b), so it is a dev-loop switch, not a new default.
2. **Treat the multi-module design as a host decision, not a linker decision.**
   The module shapes work; what has to be decided first is whether raptormark is
   willing to own an embedder. Two coherent end states:
   * *Stay on stock shims.* Then the artifact stays one module, and the way to
     get the namespacing and whole-module passes out of the critical path is
     object-level, not module-level: localise symbols on the lifted `.o` rather
     than by an LLVM IR pass over a 500-600 MB module. Same payoff, no
     architectural change.
   * *Own the embedder.* Then the multi-module design opens up — and with it the
     supervisor module, the dynamic registry, and the PIC codegen flag, as one
     piece of work rather than three. Once the embedder is owned, the question
     immediately following is whether the supervisor should be in wasm at all,
     since the host is where the 4 GiB ceiling and the per-process arena model
     actually get fixed. This is a large change and forecloses the stock-shim
     story.

The one thing worth doing before either: confirm PIC codegen survives the actual
pipeline.

### ⚠️ AMENDMENT 2026-08-18: the two end states are NOT exclusive

The recommendation above says owning the embedder "forecloses the stock-shim
story". Measured, it need not: **one set of translated objects serves both, and
the fork is at LINK time.**

| the same `-fPIC` object, linked two ways | result |
|---|---|
| ordinary `wasm-ld` link (stock-shim path) | 231,716 B, **319 imports, no `__memory_base`** |
| the NON-PIC object, same ordinary link (control) | 231,731 B, **319 imports** |
| `--experimental-pic -shared` (embedder path) | a real side module; imports `__memory_base`, `__table_base` |

The stock-shim artifact is unchanged by compiling PIC: same import count, 15
bytes smaller. And PIC codegen itself costs nothing (see §6, 2026-08-18). So the
expensive half of the pipeline -- translation, minutes to hours per program --
is SHARED, and only the final link diverges, at ~1 s plus wasm-opt.

That the boundary is already import-shaped makes this cheaper still. A lifted
object's undefined symbols are exactly the supervisor surface --
`env._ecv_save_call_history`, `env._ecv_suspended`, `env._ecv_func_epilogue`,
`env.__remill_function_return`, `env._ecv_unreached`. The single-module link
resolves them against `libecvisor.a`; a split leaves them as imports. Nothing
about the objects has to change for one path or the other.

**What "both" does not make free**, and this is the real cost of the option:

* **The supervisor in two shapes.** Linked-in for one path, its own module
  exporting those intrinsics and importing the arena memory for the other. That
  is Rust-side work behind a cfg, not a link flag, and it is the bulk of it.
* **Two link paths to maintain and to TEST.** Ship one and exercise only it, and
  the other rots; the e2e suite would have to cover both artifacts.
* **The split path still pays the inlining loss** nobody can measure until it
  exists (§6). Keeping both is arguably the reason TO keep both: the stock-shim
  artifact stays fast, and the split is available where an embedder is owned.
* **One concrete toolchain bug on the split path.**
  `wasm-ld --experimental-pic -shared --export-all` CRASHES on a real lifted
  object (LLVM crash report, wasi-sdk 24). An explicit `--export=<sym>` links
  fine, so it is `--export-all` specifically -- but a link path that needs to
  root a program's whole surface will meet it.

⚠️ Scope of this measurement: ONE partition, linked both ways, and neither
artifact was RUN. "The same objects link both ways" is established. "The split
artifact runs" is not, and remains the embedder question.

## 5b. Implementation status, 2026-08-18

Both paths are being supported (decision taken). Progress, all verified against
real artifacts rather than probes -- see JOURNAL entries of this date.

| phase | state |
|---|---|
| **A** every object built `-fPIC`, so ONE object set serves both links | ✅ E2E 87/4/0, 44 cold translations, against an image whose objects were VERIFIED PIC (`raptormark-builder:picreal`). ⚠️ The first attempt was void -- a prebuilt tools binary, see JOURNAL |
| **B** `link-all --side-out` emits a PIC side module per program, plumbed through `translate.Link` | ✅ e2e `TestSideModulesAreBuiltAndCarryTheContract`: flat 4,892,759 B, side 3,245,930 B, dylink.0 443,113 B / 899 table entries; contract asserted both ways |
| **C1** the supervisor links and RUNS as its own module, exporting the contract | ✅ 2,154,872 B, 19 exports, reports `no entry program (registry is empty)`; flat path re-checked 86/4/0 |
| **C2** dynamic registry so the embedder can register program modules | ✅ `ecv_register_program(p, size)`, 6 refusal rules, 122 host tests |
| **C3** compiler-rt + libm for side modules | ✅ linked locally, +5,371 B/module; import surface is now exactly the contract |
| **C4** the embedder itself | ✅ **DECIDED: support BOTH paths** (user, 2026-08-18). The protocol is written down (§8) and RUN (§9): `e2e/testdata/embedder.mjs` places two side modules against a supervisor module and the lifted guest runs. Remaining work is engineering, not a decision -- real-program scale, and keeping the side path from rotting |

The boundary needed no design: a lifted object's undefined symbols already ARE
the supervisor surface (§1), the flat link resolves them against `libecvisor.a`,
and the side link leaves them as imports. Everything above is link flags plus one
three-line fix in `abi::registry`.

## 6. Cheap experiments

### Answered 2026-08-15

**Does `-fPIC` apply to `-x ir` input? YES.** This was the item flagged as the
first thing that could invalidate the whole plan. `compileIR` runs
`clang <target> -O1 --sysroot -x ir -c part.bc -o part.o`, and PIC-ness on wasm
is partly a module-level IR property — but the flag takes effect anyway on IR
that carries no `PIC Level` flag at all (confirmed absent with `grep`, as
elflift emits it). Adding `-fPIC` produced a module importing
`env.__memory_base` and `env.__table_base`; the control — the same IR without
the flag — failed to link with

```
wasm-ld: error: relocation R_WASM_MEMORY_ADDR_SLEB cannot be used
                against symbol `tbl`; recompile with -fPIC
```

So the codegen half of a program-module build is a one-flag change to
`compileIR`. `.agents-workspace/tmp/globalprobe/`.

**Can a wasm global carry the suspend flag? YES, on both LLVM lines.** See the
phase 0 plan; `__attribute__((address_space(1))) int` in C becomes a real
`(global (mut i32))`, a cross-TU read becomes `global.get`, an
`external addrspace(1)` load written in LLVM IR and compiled through the
pipeline's own `clang -x ir -c` resolves against it, `wasm-opt -g -O0` preserves
it, and setting it through the C accessor is observed by the IR-side read
(`before=0 after=1`). Neutralized by pointing the accessor at a decoy global:
`before=0 after=0`. The LLVM 16 line stays at `mutable-globals, sign-ext`.

Noted while measuring, and NOT introduced by any of this: the LLVM 22 line's
baseline feature set is already much wider — `nontrapping-float-to-int`,
`bulk-memory`, `reference-types`, `multivalue`, `bulk-memory-opt`,
`call-indirect-overlong`. That is clang-22's default on a trivial file, not a
measurement of the real pipeline's output, but the Wasm 2.0 claim is load-bearing
and the LLVM 22 line deserves its own check before it ships anything.

### Answered 2026-08-18

**PIC's real code cost on lifted code: ZERO, and negative where it matters.**
Measured on four partitions of a real bash lift, `clang -O1 --sysroot -x ir -c`
with and without `-fPIC`:

| part | memory relocs, no-PIC | no-PIC | PIC | delta |
|---|---|---|---|---|
| p0 | 16 x `MEMORY_ADDR_SLEB` | 241,249 B | 241,215 B | **-34 B** |
| p1 | none | 330,278 B | 330,343 B | +65 B |
| p2 | none | 396,020 B | 396,120 B | +100 B |
| p3 | none | 305,287 B | 305,387 B | +100 B |

Defined-function counts are identical in every case. p0's 16
`R_WASM_MEMORY_ADDR_SLEB` become **one** `R_WASM_MEMORY_ADDR_REL_SLEB` plus an
undefined `__memory_base`, so the only partition with memory references got
SMALLER under PIC. The ABI reasoning above is confirmed and now has a mechanism:
lifted code reaches guest memory through `arena_ptr`, so there is almost nothing
for PIC to relocate.

⚠️ **CORRECTION to the 2026-08-15 entry above.** It explains that `-fPIC` works
because the IR "carries no `PIC Level` flag at all (confirmed absent with
`grep`)". The real pipeline's IR **does** carry one --
`!{i32 8, !"PIC Level", i32 0}`, alongside `!"PIE Level", i32 2`. The flag says
NOT PIC and `-fPIC` overrides it regardless. Proved by rewriting the module flag
to `PIC Level 2` and recompiling: **byte-identical relocation counts** to just
passing `-fPIC`. The conclusion (a one-flag change to `compileIR`) stands; the
reason given for it did not, and it was measured on `globalprobe`'s hand-written
IR rather than on pipeline output.

⚠️ **And the first attempt at this measurement answered confidently and wrongly.**
p1, p2 and p3 have NO memory relocations at all, so `-fPIC` changed nothing about
them -- same size to within 100 bytes, same symbols, same relocation types. Read
alone that says "the flag does nothing", which is the opposite of the truth and
indistinguishable from "the flag is ignored". Only p0 had
`MEMORY_ADDR_SLEB` to rewrite. **Pick the partition by what it CONTAINS, not by
what it costs to compile.**

**Table scale is not a constraint.** Two engines, three sizes -- 48,868 (the
openssl object's symbol count), 59,000 (postgres's closure), and 1,000,000 as a
ceiling probe:

| engine | shape | result |
|---|---|---|
| node/V8 | table IMPORTED by a second module, top slot filled with a function exported by a THIRD | instantiate 0.78 / 0.94 / 17.16 ms, call through the top slot correct at all three |
| **wasmedge** | single module DEFINING the table, calling through the top slot | correct at all three |

The top slot is used deliberately rather than slot 0: an engine that quietly
clamped the table would still serve slot 0.

⚠️ What this does NOT answer, and cannot from a CLI: a table SHARED across N
modules on wasmedge. `wasmedge` registers one module (§4), so the cross-module
half was measured on node and only the size half on wasmedge. That is the
existing blocker, not a new one -- but it means "wasmedge handles a table this
size" and "wasmedge shares a table this size" are two claims and only the first
is measured.

Harnesses: `.agents-workspace/multimodule-poc/tablescale.mjs` (node,
cross-module) and `tablescale-wasmedge.mjs` (emits a module, run with
`wasmedge --reactor`).

### Attempted 2026-08-18, and it CANNOT be done as specified

**The cross-module call cost is not measurable with the tools on hand, and the
reason changes the question.**

Two engines, one workload -- the same `bench.o`, linked two ways, so the only
difference is whether the callee is in this module or imported:

| engine / mode | baseline | direct | indirect | import (same mod) | import (CROSS) |
|---|---|---|---|---|---|
| node/V8 | 123.4 ms | 128.7 | 128.7 | 128.8 | **128.6** |
| wasmedge, `--run-mode aot` | 133 ms | 136 | 134 | 134 | *not runnable* |
| wasmedge, interpreter | 4,673 ms | 5,223 | 5,332 | 5,164 | *not runnable* |

20,000,000 iterations; every shape returns the same value (2018687397), which is
what says the rows are comparable.

Three things fall out, and the third is the finding:

1. **wasmedge's default run mode is the INTERPRETER**, and a `wasmedge compile`
   artifact is NOT used unless `--run-mode aot` is passed. Measured: 35x. The
   first version of this experiment compiled AOT, ran it without the flag, got
   timings identical to the plain module, and would have reported interpreter
   numbers as AOT ones. ⚠️ Any wasmedge timing in this tree that did not pass
   `--run-mode aot` is an interpreter number.
2. **Under AOT, an intra-module call is FREE because it is inlined.** 136 vs 133
   ms over 20M calls is 0.15 ns/call, well under one cycle. V8 does the same
   (0.27 ns). The interpreter is where a call costs anything: ~27 ns direct,
   ~33 ns indirect.
3. **So the split's running cost is not a call-overhead multiplier at all -- it
   is the loss of inlining across the boundary.** That is a larger effect and a
   different shape from what §3c assumed, and neither engine can be asked about
   it: wasmedge cannot instantiate two modules (§4, re-verified against 0.17.1 --
   the CLI has `--dir`, `--env`, `--reactor` and no preload or register), and V8
   INLINES the cross-module import, returning 0.98x of a direct call, so its
   answer is "free" for a reason that will not hold on an engine compiling each
   module separately.

⚠️ **The first attempt reported a clean 0.98x and was measuring the inliner.**
Its baseline loop timed 0.0 ms for 20M iterations -- folded to a constant -- and
every shape landed within 2% of every other, including the cross-module one
coming out FASTER than a direct call. That is indistinguishable from "crossings
are free" unless the baseline is checked, and it wasn't. The rebuilt benchmark
gives the callee a dependent PRNG chain and gives the baseline the same work
inline, so a call is the only difference between rows -- and the answer did not
change, which is how it became a finding about inlining rather than a number.

**Consequence for §5.** "Measure this before committing to the split" is not
satisfiable. The main running cost of the split can only be known by building
it, because the thing it costs -- cross-boundary inlining -- is exactly what a
boundary removes. The decision is therefore more front-loaded than §5 says: it
cannot be de-risked by measurement first.

Harness: `.agents-workspace/multimodule-poc/callcost/` (`bench.c`, `provider.c`,
`drive.mjs`).

### Still open
* **`_ecv_suspended` as a global.** Mechanism proven (above); the change itself
  is phase 0 work, tracked in the plan rather than here.

## 7. Evidence log

All commands run against `raptormark-builder:latest` on 2026-08-15. `:latest` is
a stale builder (`/opt/ecvisor` dates from Jul 29 and has no `ecv_sp.o`); that
does not affect the timings or the module shapes, which depend on wasi-sdk 24,
wasm-ld and Binaryen, but it is why the linked probe fails at import-link rather
than running.

| claim | how |
|---|---|
| boundary is ~17 live imports, one export | `llvm-nm --undefined-only openssl_ecv.o`; standalone link with `--export=ecv_program_0` |
| link 1 s, wasm-opt 52 s | timed `clang -O3 ... -o probe.pre` then `wasm-opt -g -O0` in the builder image |
| 120 MB module parses in ~2 s | `wasmedge probe.wasm`, which reached the import-link error |
| `--allow-undefined` emits `env` imports | the same run's *"unknown import ... `env` ... `_ecv_suspended`"* |
| PIC side modules build and are Wasm 2.0 | `clang -fPIC` + `wasm-ld --experimental-pic -shared`, features section `mutable-globals, sign-ext` |
| PIC imports `__stack_pointer`, non-PIC defines it | `wasm-dis` of both builds of the same source |
| two modules share memory and a table and call across | `.agents-workspace/multimodule-poc/build-and-run.sh` |
| the layout is what keeps them apart | same script with `GLOBAL_BASE=1024` -> MISMATCH |
| the supervisor can export the shadow-stack global | `wasm-ld --export=__stack_pointer`, `wasm-dis` shows `(export "__stack_pointer" (global ...))` |
| PIC program module + shared shadow stack works end to end | `.agents-workspace/multimodule-poc/drive-pic.mjs`: MATCH, `shadow stack shared: true`, sp balanced 70688 -> 70688 |
| registration order does not matter | `runtime/src/execmap.rs` `Programs::load` keys `by_hash` on each program's name |
| ~3 supervisor crossings per guest BL, ~98.6k sites in bash | `wasm-dis .agents-workspace/tmp/warm-base/bash.wasm`, counting `call $<sym>` per intrinsic against 7,567 functions |
| wasmedge CLI cannot preload; wasmtime can | `wasmedge --help`, `wasmtime run --help` |
| wasm-ld has the layout flags | `wasm-ld --help`: `--global-base`, `--table-base`, `--import-memory`, `--import-table`, `--export-memory`, `--export-table`, `--growable-table`, `--experimental-pic`, `--shared` |

## 8. The embedder protocol, read off the artifacts (2026-08-18)

Everything below C4 is built. This is what an embedder has to do, and every line
came from inspecting a real supervisor module and a real side module rather than
from design.

### What each side says

A **side module** exports exactly three things:

    func   __wasm_call_ctors
    func   __wasm_apply_data_relocs
    global ecv_program_0            <- holds the ADDRESS of the descriptor

⚠️ **CORRECTION (§9, 2026-08-18)**: it holds the OFFSET from `__memory_base`,
not the address. Reading it as an address is the dangerous mistake rather than
the loud one -- the offset is a valid address inside the supervisor's own heap,
so registration succeeds and the supervisor reads a descriptor out of somebody
else's allocation. See §9.

and declares its needs in `dylink.0` MEM_INFO. Measured on a real one:
**443,939 bytes of memory at 16-byte alignment, 901 table entries.**

The **supervisor** exports 23: the 15 intrinsics, `memory`,
`__indirect_function_table`, `__stack_pointer`, `_start`,
`ecv_register_program`, `ecv_reserve_side`, and `__heap_base`/`__heap_end`/
`__data_end` for inspection.

⚠️ **CORRECTION (§9)**: the supervisor `link-all --side-out` now emits exports
**21**. The three `__heap_*`/`__data_end` symbols are NOT exported -- nothing
needs them, `ecv_reserve_side` is what placement goes through, and exporting the
heap bounds invites exactly the placement §8 argues against. `--growable-table`
is a required flag that this list did not mention; without it the embedder can
place zero side modules.

### The sequence

1. Instantiate the supervisor with the 28 WASI imports.
2. For each side module, read its `dylink.0` MEM_INFO.
3. `ecv_reserve_side(size, align)` -> `__memory_base`. **Not `memory.grow`, and
   not `__heap_base`** -- see below.
4. `__table_base` = current table length; grow the table by the declared count.
5. Instantiate the side module with `env` = `memory`,
   `__indirect_function_table`, `__memory_base`, `__table_base`,
   `__stack_pointer`, and the 15 intrinsics.
6. Call `__wasm_apply_data_relocs()`, then `__wasm_call_ctors()`.
7. Read the exported global `ecv_program_0` -> the descriptor address.
8. `ecv_register_program(addr, sizeof(EcvProgram))`. Non-zero is fatal and the
   code says which rule was broken.
9. `_start()`.

### ⚠️ Why the SUPERVISOR allocates the side module's memory

The obvious placements are both hazards. wasi-libc's dlmalloc grows the same
linear memory and tracks its own high-water mark, so a region the embedder takes
by calling `memory.grow` behind its back is a region dlmalloc never saw -- and a
later supervisor allocation can be handed out overlapping it. Placing at
`__heap_base` is worse: the 384 MiB arena is allocated from that heap.

The failure would be a side module's data quietly overwritten by the
supervisor's heap: silent, and arbitrarily long after placement. `ecv_reserve_side`
removes it by construction -- dlmalloc owns the region, so it cannot hand it out
twice. The allocation is deliberately leaked; a side module's data lives as long
as the process.

### Still open

**SUPERSEDED by §9, 2026-08-18.** The sequence has now been run, end to end, on
real artifacts. What this paragraph says about the shipping path is still true;
what it says about the protocol being unverified is not.

The sequence above has not been RUN. A host that can drive it needs the 28 WASI
imports including WasmEdge's 11 socket extensions, which rules out `node:wasi`
for anything that touches a socket, and the wasmedge CLI registers one module
(§4). So C4 remains what §5 said it was: a decision to own an embedder, now with
every other piece in place and the protocol written down.

⚠️ **SUPERSEDED. The decision was TAKEN on 2026-08-18: support both paths.** This
paragraph, and every later line in this file that calls C4 "a decision", is
stale. One `-fPIC` object set links both ways, so the two are not alternatives
and there was never a future to give up. What remains under C4 is engineering.

## 9. The protocol, RUN (2026-08-18)

`e2e/testdata/embedder.mjs` is a development embedder: ~250 lines of JavaScript
that drives the §8 sequence over the artifacts `link-all --side-out` produces.
`TestEmbedderRunsTheSideModule` runs it on every E2E pass.

    step1: supervisor instantiated, 21 exports
    step2[0]: needs mem=443173 align=2^4 table=900
    step3[0]: reserved at 16819216 (0x100a410)
    step4[0]: table base 130, grown to 1030
    step5[0]: instantiated
    step6[0]: relocs applied, ctors run
    step7[0]: ecv_program_0 at offset 345880 -> 17165096
    step8[0]: registered
    ... the same for program 1, placed at 17262432 ...
    step9: starting with 2 program(s) registered
    EMBEDDER-GUEST-OK
    EMBEDDER-SEQUENCE-COMPLETE exit=0

The guest is aarch64 code lifted by elflift, running inside a PIC side module
that was instantiated separately from the supervisor and placed by it. The SAME
translated objects, linked flat, print the same line under wasmedge -- so the
test is a differential over one object set rather than a claim about one path.

**Two programs, not one.** A single placement cannot show that two do not
overlap, which is the entire reason placement goes through `ecv_reserve_side`.
Measured: `[16819216, 17262389)` and `[17262432, 17705657)`, disjoint with a
43-byte gap -- dlmalloc's own header and alignment, which is what "the
supervisor's allocator owns the region" looks like from outside.

### What running it found that reading it could not

**1. The exported descriptor global is an OFFSET, not an address.** §8 said
address. The global section is 8 bytes -- one `i32.const 345880` -- and neither
`__wasm_apply_data_relocs` nor `__wasm_call_ctors` touches it: a wasm global's
initialiser is a const expression, and `__memory_base + off` is not one without
the extended-const proposal. The relocs fix up the module's DATA; nothing was
ever going to fix up this global. The embedder adds the base itself.

This is the failure mode worth dwelling on. 345880 is a perfectly good address
inside the supervisor's heap, so `ecv_register_program` accepts it and the
supervisor reads an `EcvProgram` out of another allocation. Confirmed by
mutation: the guest died with `null function or function signature mismatch`,
which points at the dispatch table and not at the descriptor.

**2. `node:wasi` ABORTS -- SIGABRT, no message, no stack -- on the first WASI
call after the guest grows linear memory.** The binding caches the backing store
when memory is bound; `memory.grow` detaches that ArrayBuffer; nothing re-reads
it. Reduced to 15 lines: `fprintf`, `malloc(384 MiB)`, `fprintf`, one ordinary
module, the documented `wasi.start()` path. Nothing to do with multi-module --
but every raptormark guest hits it, because ecvisor allocates a 384 MiB arena
during startup, and the crash carries no diagnostic at all.

The harness works around it by fishing node's private `kSetMemory` symbol off
the instance and re-binding whenever `memory.buffer` changes (one rebind per
growth, measured). ⚠️ **This is the clearest statement available of why node is
a development host and not a candidate embedder.**

**3. `-Wl,--growable-table` is load-bearing, and its absence is silent until
run time.** Verified by removal: a supervisor linked without it reaches step 4
and stops, because wasm-ld caps the exported table at its initial length. V8
reports only `failed to grow table by 900`, naming neither the flag nor the
module that lacks it, so the harness diagnoses it.

### Real scale: CPython through the same sequence (2026-08-19)

The harness had only ever run toy guests -- a `printf` and something smaller --
so "the protocol works" rested on a 443 KB module. Repeated with the real
`python:3-slim` side module:

| | hello-world | **CPython** |
|---|---|---|
| side module | 3.2 MB | **36.4 MB** |
| `dylink.0` memory | 443,939 B | **5,754,786 B** (13x) |
| `dylink.0` table | 901 entries | **12,115 entries** (13x) |

    [ecvisor] registry has: python_glibc_fused_e5234af7e1cf
    [ecvisor] ifunc: resolver 0xc9bee0 -> impl 0xca3080; GOT slot 0xb002c0 filled
    [ecvisor] __libc_early_init 0xd405a0 ran
    [ecvisor] _dl_tls_static bring-up: size=208 align=16
    [ecvisor] ran 12 _dl_init constructor(s)
    CALLHEAVY-OK 32851
    EMBEDDER-SEQUENCE-COMPLETE exit=0

`32851` is the checksum the native interpreter and the FLAT module both print, so
this is the same program computing the same answer by a different placement.
Everything a real guest needs crossed the boundary intact: ifunc resolution
through the GOT, `__libc_early_init`, static TLS geometry, and twelve
constructors -- none of which a hello-world exercises.

`ecv_reserve_side` handled a 5.75 MB request and the table grew by 12,115 entries
without special-casing. **Nothing in the protocol was scale-sensitive.**

### What this does and does not settle

Settled: the protocol is right. Reserve, place, relocate, register, start --
each step verified by removing it and watching the intended failure. Eight
mutations, eight caught.

NOT settled, and unchanged by any of this:

* **The shipping path still runs one module.** §4 stands. Node is a development
  host; it cannot supply WasmEdge's 11 socket imports (the harness stubs them to
  `ENOTSUP`) and it is not a deployment target.
* **The cost of the split is still unmeasurable.** §6's finding holds: V8 inlines
  cross-module imports, so the harness cannot price the boundary either.
* **C4 is a TASK, not a decision.** ⚠️ This bullet previously read "C4 remains a
  decision, not a task", which was wrong: the decision -- support both paths --
  was taken on 2026-08-18 and re-stated when this document drifted off it. What
  remains is writing the host, in a language of choice, against a sequence that
  has been executed. The three findings above are what that host would otherwise
  have discovered the hard way.

## 10. The protocol, run MID-RUN (2026-08-24)

§8 and §9 place every side module BEFORE `_start`. That is deferred
instantiation of a set fixed at link time: the guest never influences it. §10 is
the same nine steps, executed in response to the guest instead — its `dlopen` or
its `execve`. `e2e/testdata/hostedembedder.mjs`, proven by
`e2e/hostedload_test.go` and `e2e/execload_test.go`.

Only two of the nine steps change.

**Step 1 becomes a loop, not `_start`.** `_start` returns only when the guest is
finished, so a host driving it has no window in which to instantiate anything.
The host drives the re-entrant surface instead:

```
ecv_boot()                  once
ecv_run_slice(n) -> 1       progress; go again          (ECV_PREEMPTED)
                 -> 0       nothing runnable            (ECV_IDLE)
                 -> 2       exited; read ecv_exit_code  (ECV_EXITED)
```

⚠️ **`ECV_IDLE` is 0 and `ECV_PREEMPTED` is 1**, which reads backwards if zero
looks like success. A loop written `if (r !== 0) break` stops on the first
productive slice. That bug was live in a driver and PASSED, because its guest
exited through `proc_exit` before the loop's own condition decided anything.

**Step 8 lands on `push_late`.** The registry is frozen once `_start` has read
it; `ecv_register_program` now routes a frozen registration to a late hook
instead of returning `ECV_REG_FROZEN`.

### The three rules that are not in §8

**A load must be served BETWEEN slices.** `ecv_host_load_side` is called from
inside `ecv_run_slice`, so a slice is on the wasm stack, holding `&mut` on the
process table that `ecv_side_loaded` mutates. The import records the request and
returns 0 (PENDING); the queue is flushed after the slice returns. Same rule
`ecv_net_ready` and `ecv_signal` already carry.

**A library unit imports `GOT.mem._ecv_entry_func`; a main program does not.**
§8 measured "no GOT.mem / GOT.func" and that was true of a MAIN module. A library
has no entry function, so elflift emits no `_ecv_entry_func` and the descriptor
fragment's `.entry_func = &_ecv_entry_func` leaves an undefined data symbol.
Measured with `llvm-nm`:

```
unit_a: U _ecv_entry_func            (undefined)
main:   D _ecvmain_..._entry_func    (defined, namespaced)
```

The flat link resolves it to 0 via `--allow-undefined`; a host must supply the
same 0. Do so from an allowlist, not blanket — zeroing every GOT import silences
a genuinely missing symbol and turns it into a null dereference far away.

⚠️ `llvm-nm` is NOT on PATH in the builder image. The first attempt at that
measurement printed `command not found`, and the greps returned 0 for BOTH
objects — which reads exactly like "absent from both". It lives at
`/root/wasi-sdk-24.0-arm64-linux/bin/llvm-nm`.

**The unit is named by its CONTENT HASH, and the token is opaque.**
`ecv_host_load_side(token, name_ptr, name_len)` — note the argument order; `name`
is the hash, not a guest path. `token` is whatever the runtime chose and the host
must echo it back verbatim to `ecv_side_loaded`. For a unit with no registry
index yet — every unit on its first open, because the descriptor lives inside the
side module — it is a synthetic value at or above `PENDING_UNIT_BASE` (2^30).
**A host, and a backend, must never use it to index a dense array.**

### What running it found that reading it could not

Four runtime defects, each hidden behind the one before: a parked `dlopen`
reported as DEADLOCK on the branch a socket-free profile actually takes; a wake
that set `Runnable` without enqueuing, giving a clean exit 0 with the guest's
work undone; the same unit placed twice, once under its token and once under its
index; and a backend doing `Vec::resize(2^30)`, whose FIRST call quietly took
~1 GiB and whose second open trapped. Plus the exec map resolving paths to
indices at construction, which made `execve` to a deferred program ENOEXEC. The
2026-08-24 JOURNAL entries have the detail.
