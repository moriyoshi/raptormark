# Dynamic Side-Module Loading

## Summary

`dlopen` and `execve` are one seam. A dlopen-able plugin is fused as its own
ET_EXEC unit against the closure's fixed layout, published through a dlopen map
in the rfs sidecar, and made reachable through a compile-selected
`LoaderBackend`. This closed the PostgreSQL extension collision, in which 79
extensions each defining `Pg_magic_func` bound first-wins in one flat namespace,
and it added a host-driven path on which a unit is compiled, instantiated and
registered mid-run in response to the guest's own call.

## Key Facts

- A plugin's RELOCATED bytes are a function of the shared layout alone. That is
  measured, not assumed, and it is why a separately-fused unit needs no runtime
  symbol resolver.
- Plugins live in their own BAND above the library band, at their own
  `PT_LOAD` alignment. Putting them above rather than below leaves every library
  base byte-identical, so no cached object or partition is spent.
- The dlopen map is path -> content HASH, never path -> index. A lazily placed
  unit has no index until the host registers it.
- `dlsym` selects a unit by handle; `RTLD_DEFAULT` keeps global scope,
  `RTLD_NEXT` is refused, `RTLD_NOLOAD` never causes a load.
- Loaded-unit state is per PROCESS (thread groups share the leader's), and
  `dlerror` is per TASK. Both were global once, and neither failure was visible
  to any test.
- A unit carrying its own `PT_TLS` is REFUSED. `execve` must NOT inherit that
  refusal -- a fused dynamic program always carries `.ecv.tls`.
- Four loader backends exist: `preloaded` (default, ships), `hosted`, `wasix`,
  and the seam itself. Exactly one may be enabled; that is a `compile_error!`,
  not a precedence chain.

## Details

### Phase 0: the questions asked before anything was built

| question | answer |
|---|---|
| 0c -- are a plugin's relocated bytes a function of the shared layout alone? | **yes**, measured over real aarch64 ELFs (`.agents-workspace/drivers/plugunit`) |
| 0b static -- what does a raptormark side module import? | 20 names, all `env`; no exception handling |
| 0b runtime -- does a FOREIGN host load one? | **yes**, against its own memory and table, reaching no intrinsic during bring-up |
| 0a -- does WASIX accept a non-EH PIC side module? | eventually **yes**; see `wasix-and-wasmer.md` |
| 0d -- can a browser host serve a synchronous load? | **no** for a real program: Chromium refuses to compile a module over 8 MB synchronously and the CPython side module is 36.4 MB |

Three ways 0c could have passed while proving nothing were closed
deliberately: a closure that silently falls back to per-image packing (the
driver refuses a verdict unless `Report.Shared`), a comparison containing no
relocated word (two digests, one address-bearing and one content-only), and two
empty extractions (sections matched by ADDRESS, never by name, because `emit()`
suffixes a library's sections with its per-program slot index).

The obvious neutralization of the foreign-host probe is WRONG and that is the
useful part: placing a side module at a different `__memory_base` does not
corrupt it, because a PIC module's relocations are applied relative to the base
at load time. The neutralization that works is skipping
`__wasm_apply_data_relocs`.

### Phase 1: the fuse side

**The plugin band.** `libAlign` is `0x200000` and `fuse.go` records why -- "chosen
to match the recovered poc". It is an inherited constant, not a requirement:

| sample | max `PT_LOAD` `p_align` |
|---|---|
| 2,114 real aarch64 shared objects on a Debian host | `0x10000`, every one |
| all 79 postgres:17 extensions | `0x10000`, every one |

| 79 postgres extensions | size |
|---|---|
| real content | 7.8 MiB, median 66 KiB each |
| at libAlign 2 MiB | 158.0 MiB |
| at their own 64 KiB | 12.6 MiB |

158 MiB exceeds the 156 MiB fused region before one library is placed.
`Options.Extra` objects are marked `isPlugin` in `load()` and placed in a band
above the libraries; libraries they DT_NEEDED are NOT marked, because those are
ordinary shared libraries. `TestPluginsDoNotMoveAnyLibrary` pins the
no-library-moves property and is the more important of the two tests.

The band was necessary and NOT sufficient: with all 79 the closure still
overflows, and the residue is genuine CONTENT -- `llvmjit.so` brings
`libLLVM.so.19.1` (117.5 MiB) and `libz3.so.4` (25.6 MiB). With 78 the closure
plans SHARED at top `0x8f80010`, 89.7% of the region. At 89.7% there is little
headroom, so the next large closure needs a bigger region rather than better
packing.

**Per-unit fusing.** `fuse.FuseWithUnits` fuses the closure and emits each
`Options.Extra` plugin as its own ET_EXEC image at the base the plugin band
planned. Relocation must see EVERY object even though emission does not:
narrowing `globalSymbols` to the main closure leaves each plugin's libc
references unresolved, and an unresolved `GLOB_DAT` is written as **0** rather
than reported -- a plugin whose every libc call goes through a null pointer.

Measured on six real postgres:17 extensions that each define `Pg_magic_func`:

| | `.ecv.dlsyms` entries | `Pg_magic_func` |
|---|---|---|
| flat | 46,882 | present ONCE -- one of six wins, silently |
| main (split) | 46,568 | **absent** |
| 6 units | 7 / 2 / 175 / 24 / 66 / 47 | each has its own, at 6 distinct addresses |

A unit emits `.ecv.init` and `.ecv.dlsyms` only. `.ecv.early`,
`.ecv.stacklists` and `.ecv.musltp` describe the LIBC in the main image and are
applied once at bring-up; a unit restating them would re-run libc's early init
on every `dlopen`.

**The dlopen map.** `internal/link/dlmap.go`, placed at `/.raptormark/dlopen`.
Byte-identical to the exec map apart from the magic (`RMDLOP01` against
`RMEXEC01`) so one runtime reader serves both, pinned by
`TestDlMapEncodesLikeTheExecMap`. It is a SECOND map rather than more entries in
the first because the failure modes are opposite: an unknown execve path falls
back to program 0, an unknown dlopen path must simply FAIL.

Canonical paths matter more here than for execve. A guest names a plugin
through whatever path its own configuration holds -- postgres builds one from
`$libdir`, python from `sys.path` -- so the spellings are far more varied than
the handful libc spawns through, and a usr-merged Debian image resolves most of
them through at least one symlink.

**Discovery.** `internal/image/plugins.go` closes the gap between `PluginDirs`
(directories) and `fuse.Options.Extra` (files). On the real `postgres:17`
rootfs: 81 discovered, 1 excluded. The exclusion is by DEPENDENCY, not by name --
`llvmjit.so` is the only extension naming `libLLVM`, and it names it directly, so
a DT_NEEDED PREFIX rule is precise and keeps working at LLVM 20 where equality
would silently lapse. The reason to exclude is that raptormark is AOT and a
runtime-generated guest address reaches `func_at -> None -> fatal!`; the 143 MiB
is a consequence, not the argument.

### Phase 2: the runtime half

| | before | after |
|---|---|---|
| `dlopen(path)` | non-null SENTINEL `1` for EVERY path | resolves through the dlopen map; `idx + 1`, or NULL |
| `dlsym(h, name)` | handle IGNORED, one flat closure-wide table | handle selects the unit |
| `dlerror()` | `0` unconditionally | the real message, cleared on read |
| `dlclose(h)` | `0`, no bookkeeping | refcount, and a bogus handle reports failure |

Together those were why an absent plugin "loaded" successfully, had every symbol
resolve to NULL, and could not be diagnosed -- postgres reports it as
`incompatible library "...": missing magic block`, a version mismatch by
appearance and an absent object in fact.

Added: `runtime/src/dlmap.rs` (sharing `execmap::parse`, now taking the magic),
`EcvContext::dlsym_in`, `apply_ifuncs_in` / `apply_init_array_in`,
`ensure_unit_loaded`, and `arena::DLERROR_VMA`. Ifuncs run BEFORE constructors:
a constructor may call an ifunc'd libc function through a slot, and an unfilled
slot holds the RESOLVER, which returns a pointer and does nothing.
`DLERROR_VMA` sits in the measured gap between musl's TLS module list (ends
`THREAD_PTR - 0x900`) and glibc's `_dl_stack_*` bring-up (`0xff9a0`), guarded by
three `const` assertions that fail the BUILD if either neighbour grows into it.

`dlclose` never tears a unit down -- it decrements a refcount and never clears
`inited`, because re-initialising a unit's `.data` under a guest holding
pointers into it is silent corruption. `RTLD_NEXT` is refused rather than
answered from global scope: answering globally returns the SAME definition the
caller already has, which is the classic way an interposer recurses forever.

### Phase 3-4: the loader seam and its backends

`runtime/src/loader/` mirrors `runtime/src/net/` for the identical reason:
`wasm-ld` emits an import for every undefined symbol reachable from live code,
so a `dyn` trait object or an `enum` keeps every backend live and puts every
backend's imports in the module. `--gc-sections` cannot help -- it cannot prove a
vtable slot is never called. So `LoaderBackend` is a CONFORMANCE CONTRACT and
`type Loader` is a cfg-selected alias.

`LoadOutcome` is `Ready | Pending | Failed(&'static str)`, async-capable from
the first signature because it cannot be retrofitted without touching every
caller. `Pending` is handled LOUDLY under a backend that cannot park: a backend
returning it early means a backend and a runtime that disagree about what is
wired, not a plugin that could not be found.

`preloaded` is the default and not a consolation prize. The flat artifact
physically cannot gain code after link time -- every module in this tree declares
`__indirect_function_table` with `min == max` (bash 6,694, nginx 21,902,
postgres 155,275), it is not exported, and nothing calls `table.grow`. So on the
stock runwasi path "dynamic" can only mean deferring the merge of code already
linked in -- which still delivers the collision fix, on the artifact that ships,
with no new import.

`hosted` and `emscripten` COLLAPSE INTO ONE BACKEND on the Phase 0b evidence: a
side module imports 20 names all from `env`, instantiates against a host
supplying its own memory and table, and reaches no intrinsic during bring-up.
Conformance to Emscripten's `library_dylink.js` is NAMING, not ABI.

A `hosted` profile could never have been wasmedge-based. A mid-run load REQUIRES
the re-entrant surface -- the guest must yield so the host can instantiate
asynchronously and then call `ecv_side_loaded` -- and `_start` returns only when
the guest is finished. `linkall.go` puts that surface on non-default profiles
only, so the pairing is forced. `//runtime/hosted` is loopback-based, so an
embedder need not supply a socket ABI to exercise a loader.

**Park and wake.** `BlockedOn::SideLoad { unit }` takes its readiness from
outside, like `Socket` and unlike `PipeRead`. It is deliberately NOT in
`signal_interruptible`: native `dlopen` is not interruptible. A parked `dlopen`
sets NO return value; a parked `execve` returns without touching `state` so the
retry sees its original arguments.

**A unit has no index before it is loaded.** `ensure_unit_loaded` takes a
registry index and a lazily placed unit has none, because its `EcvProgram`
descriptor lives inside the side module. The index is a RESULT of the load.
`DlMap::hash_for`, `pending_units` with `PENDING_UNIT_BASE` (2^30) as a stable
opaque park token, and `ensure_unregistered_unit` close that: the host echoes
the token back and the guest re-resolves after the wake.

### The mid-run result

A guest calls `dlopen`, parks; the host compiles and instantiates the unit's
side module, registers it, calls `ecv_side_loaded`; the guest wakes and its
`dlsym` resolves out of a unit that was not in the module a moment earlier. Two
units, two distinct addresses, correct magics, `loads served: 2`, exit 0
(`e2e/hostedload_test.go` + `e2e/testdata/hostedembedder.mjs`). `execve` to a
deferred program works the same way (`e2e/execload_test.go`), and both triggers
serve on a DYNAMIC, MULTI-PROGRAM image built from the CLI
(`e2e/pipelinehosted_test.go`, which asserts `loads served: 2` EXACTLY rather
than as a floor, because a floor would pass if only the dlopen worked).

### patches/0066: the VRP crash was OUR bug

A unit image crashed elflift in `VirtualRegsOpt::GetRegValueFromCacheMap` with
`std::out_of_range`. `cur_r_inst_mp` is seeded from `t_bag->drvd_rmp` -- every
register there gets a PHI at the top of the block -- but the seeding loop filters
by `CheckPassedArgsRegs` while the STORE sites filter by `CheckStoreBeforeCall`:

| predicate | admits |
|---|---|
| `CheckPassedArgsRegs` (at the seed) | 0..8, or SP -- any reg kind |
| `CheckStoreBeforeCall` (at the stores) | General 0..30 or SP; Vector 8..15 |

So x9..x30 and v8..v15 could be stored before a call with no PHI.
`CheckStoreBeforeCall` does not exist upstream -- `patches/0007` added it so
glibc's hand-written setjmp reads live values -- and it widened the store sites
while leaving the seeding narrow. Latent since, for anyone, not unit-specific.

The fix is the UNION (`CheckPassedArgsRegs() || CheckStoreBeforeCall()`) and not
a replacement: neither predicate contains the other, so swapping them would drop
v0..v7 and lose PHIs that are wanted today.

Two failures hid behind it. A zero-sized `SHN_ABS` FUNC symbol dereferences null
in `TraceManager::SetELFData`, because BFD returns a NULL section for `SHN_ABS`;
`fuse.emit` synthesised `_start` at `e_entry` defaulting `shndx` to `SHN_ABS`,
and a unit's `e_entry` of 0 rebases to the image base, below every section. Fixed
by not emitting it -- a symbol at an address no section covers is not a function.
And a unit has no entry function at all, so `emit` now points `e_entry` at the
lowest-addressed sized FUNC symbol; choosing arbitrarily is sound only because a
unit has no exec-map path and its `entry_func` is never called.

## Files

- `internal/fuse/unit.go`: `FuseWithUnits`, `unitTables`, `ifuncsWithin`, `tlsdescsOf`.
- `internal/fuse/fuse.go`: the plugin band, `isPlugin`, `PlanLayout`.
- `internal/link/dlmap.go`: the dlopen map, `CheckDlMap`, `CanonicalDlEntries`.
- `internal/image/plugins.go`: `Plugins(root)`, `IsJITPlugin`.
- `runtime/src/dlmap.rs`: the sidecar reader, sharing `execmap::parse`.
- `runtime/src/loader/`: `mod.rs` (the contract and the `compile_error!`), `preloaded.rs`, `hosted.rs`, `wasix.rs`.
- `runtime/src/context.rs`: `dlsym_in`, `ensure_unit_code`, `ensure_unit_loaded`, `apply_load_outcome`, `note_side_loaded`, `units_owner`, `register_late_unit`.
- `patches/0066-drvd-rmp-covers-store-before-call.patch`.

## Test Coverage

- `e2e/pgdlopen_test.go`: `TestPostgresStyleDlopenResolvesPerUnit`. Two plugins defining `Pg_magic_func` and `_PG_init` with distinct magics; three discriminators, because any one could be right by luck -- each magic matches its own plugin, the two resolved addresses DIFFER, and `_PG_init` scopes the same way. A native baseline supplies the expected magics rather than asserting them from source.
- `e2e/hostedload_test.go`, `e2e/execload_test.go`, `e2e/pipelinehosted_test.go`: the mid-run triggers.
- `//runtime:loader_exclusion_test`: the SHIPPING archive carries no host-aided loader, and `hosted` / `wasix` carry only their own. Matches the MANGLED module path, not the import name.
- `context::dlopen_tests::execve_accepts_a_unit_with_static_tls_and_dlopen_does_not`.
- `internal/fuse.TestPluginsDoNotMoveAnyLibrary`, `internal/link.TestDlMapEncodesLikeTheExecMap`, `internal/pipeline.TestOnlyTheEntryCarriesPlugins`.

Neutralization is what proved each of these. Making `dlsym` ignore its handle
reproduces the postgres defect exactly -- ext_b returns ext_a's magic, both
handles resolve to one address, `_PG_init` collapses too, all three
discriminators firing. `NEUTRALIZE_REFUSE_ALL=1` makes the host refuse every
load, and `dlerror` then carries "the host refused to load this unit", which is
the shape the whole chain began from.

## Pitfalls

- **Seven defects, none caught by a failing test.** Six were found by reading,
  five of those in code written and gated green in the same session. Every one
  had the same shape: a claim about behaviour that no test could distinguish
  from its opposite.
  1. a weak test whose plugin paths sorted after the libraries;
  2. a weak test that compared COUNTS, not paths;
  3. the dlopen map resolving to an index at construction;
  4. dlopen units swept into the eager library merge by `library_indices`, which classifies a LIBRARY unit by ABSENCE from the exec map -- and a dlopen-able unit has no exec-map path either, so every unit was merged before the guest's first instruction while `inited` kept `dlopen` behaving correctly;
  5. the loaded-unit set global, so a second process's `dlopen` skipped the merge and read the plugin's `.data` as zeroes;
  6. `dlerror` global, so a process read another's failure;
  7. `loader_profiles` as a `filegroup`, caught by a positive control.
- **Three neutralizations also needed neutralizing.** One commented out a line a
  text grep still matched (`//#[link(wasm_import_module = "env")]` still
  contains the string); one edited an anchor that did not exist and the test
  kept passing; one tested a property that was never true. A neutralization
  needs the same "what would a pass look like if this were false?" as the check
  it tests.
- **`cargo test` DOES NOT COMPILE `sys.rs`.** `lib.rs` gates `entry`,
  `intrinsics` and `sys` on `#[cfg(target_arch = "wasm32")]`, so the whole
  syscall layer is invisible to `cargo build` and `cargo test`. Only
  `cargo check --target wasm32-wasip1` compiles it. Any claim that a change to
  `sys.rs` "builds" is worthless unless the wasm check ran.
- **A side-load waiter reads exactly like a deadlock.** No fd and no deadline
  satisfies every terminal condition. `has_side_load_waiters` must be asked at
  BOTH places `resume_scheduling` concludes deadlock -- guarding only the
  `WaitOutcome::TimedOut` one is guarding a branch a socket-free profile never
  takes.
- **A wake that sets `Runnable` without `run_queue.push_back` gives a CLEAN EXIT
  0 with the guest's work undone.** The host-side test asserted `status` and
  `blocked_on` -- exactly the two fields the buggy code wrote. A test written
  from a model of the fix inherits that model's blind spot; it now asserts
  `run_queue.contains`.
- **`Hosted::slot` indexed a Vec by the opaque token.** `Vec::resize(2^30)`
  quietly allocated ~1 GiB on the first call and trapped on the second, one
  whole successful load away from the cause. A `HashMap` now; the trait doc
  records that `idx` is opaque and sparse.
- **`ecv_side_loaded` was exported by no profile, so it did not exist.**
  `wasm-ld` never pulls an unreferenced member out of a static archive, and
  nothing inside the module references it. Every gate was green: the Rust
  compiles, `cargo test` passes, the archive genuinely contains the function.
  Second time this tree has hit that mechanism.
- **A library unit DOES import `GOT.mem._ecv_entry_func`.** Phase 0b's "no
  `GOT.mem` / `GOT.func`" was measured on a MAIN PROGRAM's side module. A
  library has no entry function, so elflift emits none and the descriptor
  fragment's `.entry_func` is left undefined; the flat link resolves it to 0
  through `--allow-undefined`, a PIC side link turns it into a GOT import.
- **`llvm-nm` is not on `PATH` in the builder image.** It is at
  `/root/wasi-sdk-24.0-arm64-linux/bin/llvm-nm`. A `command not found` makes a
  grep return 0 for every object, which reads as "the symbol is absent".
- **The elflift output flag is `--bc_out`, not `--bitcode_path`.** Using the
  wrong one writes nothing and produces a phantom failure. Check the flag
  against the source before reading an empty artifact as a failure.
- Dynamic TLS blocks nothing measurable: **1 of 159 real plugins** carries
  `PT_TLS`, and that one is `_ctypes_test.cpython-314-aarch64-linux-gnu.so`,
  CPython's own test fixture. See [[target-enablement-synthesis]].
- See [[translation-linking-and-object-cache]] for how a unit's object is keyed,
  [[image-discovery-and-rootfs]] for plugin discovery's place in stage 1, and
  [[wasix-and-wasmer]] for the fourth backend and why its loader half is
  deferred.
