## LTM Consolidation Record

The journal has been audited entry by entry against `.agents/docs/LTM/` and
`.agents/docs/TODO.md`. Every removed substantive entry has its durable design,
implementation, testing, pitfall, or follow-up information represented in those
destinations. Transient chronology, superseded measurements, and investigative
output without durable guidance were not retained.

This record supersedes and absorbs the earlier `## Deep Sleep Consolidation
Record`, which duplicated the synthesis mapping below. Later `deep-sleep` passes
update the synthesis and standalone tables here rather than appending a second
record section -- a separate record would recreate exactly the duplication this
one was written to remove.

### Journal sections to source LTM documents

| Journal section or section group | LTM document |
|----------------------------------|--------------|
| AArch64 decoder patches 0050-0065, BTI/PAC handling, undecoded inventories, honest diagnostics, the executed-set census, and python/postgres reachability | `aarch64-lifter-and-coverage.md` |
| Bazel owning the builder image contents, the zero-`RUN` Dockerfile, tool and C-shim byte-identity, `raptormark bazel`'s three sharp edges, cache identity moving with the flags, and the hermetic SDK differential | `bazel-build-and-hermetic-sdk.md` |
| Bounded-snapshot adoption and removal of its flag, full-snapshot fallback, fork-from-thread restoration, process switching, signals, shared unmaps, futexes, TLS/TSD, and PID-aware watchpoints | `ecvisor-process-and-thread-model.md` |
| Browser and Node embedding, re-entrant execution, focused web unit coverage, browser matrix, DNS, relay, service-worker inbound, HTTP framing, and nginx in a tab | `web-embedder-and-browser-networking.md` |
| Browser timing, host-side stamps, lifter hot spots, BTI indexing, merged-pass costs, registry-index pricing, closure timings, cache measurements, and measurement method | `performance-investigation.md` |
| Builder provenance, reconstructed components, image tags, patch-series health, Docker preservation, and the post-2026-08-23 image inventory | `recovery-and-builder-provenance.md` |
| Container discovery, executable closure, symlinks, rootfs reconstruction, RFS modes, canonical paths, and plugin discovery | `image-discovery-and-rootfs.md` |
| Deterministic lifting, preparation, namespacing, partitioning, registry-index identity, object identity, manifests, library caching, flat/side-module links, the stable-split default, and profile-aware linking | `translation-linking-and-object-cache.md` |
| Document roles, memory workflows, language gates, web tooling, environment requirements, process-scope safety, quality-gate drift, and the two-pattern Go gate | `agent-harness-and-quality-gate.md` |
| Dynamic side-module loading Phases 0-5: the plugin band, per-unit fusing, the dlopen map, plugin discovery and its JIT exclusion, handle-aware `dlsym`, per-process loaded-unit state, the loader seam and its backends, park/wake, mid-run `dlopen` and `execve` loads, and `patches/0066` | `dynamic-side-module-loading.md` |
| Function extents, `.eh_frame`, init/fini, computed code pointers, trace boundaries, and `.ecv.funcs` limits | `function-boundary-recovery.md` |
| Fuser reconstruction, relocation families, RELR, TLS/TLSDESC, GNU IFUNC, and resolver evaluation | `fusing-relocations-and-ifunc.md` |
| Host and E2E gates, mechanism-focused witnesses, fixture controls, neutralization (including neutralizations that did not neutralize), false controls, uniform fixtures, unlisted Bazel sources, artifact freshness, skip counts, browser witnesses, and cross-builder differentials | `testing-and-regression-method.md` |
| Hot-path tracing cost, opt-in design, patch 0060, and real-interpreter pricing | `hot-path-cost-and-opt-in-design.md` |
| PostgreSQL bring-up, planner/data-path rungs, bounded concurrency, guest AF_UNIX, shared files, locale closure, file-backed shared-map reclamation, and multi-client results | `postgres-and-guest-concurrency.md` |
| Proposal-free suspension, Wasm 2.0 and the SCOPED reading of that rule, OCI/runwasi behavior, portable profiles, multi-module constraints, and vDSO limits | `wasm-runtime-and-oci-compatibility.md` |
| Python, Redis, cryptography, plugin loading, musl threading, and interpreter bring-up | `python-redis-and-cryptography-bringup.md` |
| RFS/VFS, memory and process syscalls, timed waits, the monotonic guest clock, socket semantics, errno mapping, guest AF_UNIX, shared mappings, host-neutral networking, nginx serving, and the ruled worker diagnostics | `ecvisor-vfs-syscalls-and-networking.md` |
| Ruby PAC jump-table recovery, startup mappings and prctls, patch 0062, and YJIT's measured pre-codegen walls | `ruby-jit-and-jump-table-bringup.md` |
| Runtime custom-section consumers, reconstructed producers, dynamic symbols, TLS, musl TP, the fused-glibc threading chain, exec-map identity drift, and producer/consumer gaps | `runtime-metadata-producers.md` |
| The end-to-end driver: `raptormark build` and `raptormark run`, the three runtimes' directory flags, and the defects a one-program fixture cannot express | `pipeline-driver-and-cli.md` |
| WASIX and wasmer: the measured dl and socket ABI, `--profile wasix` egress, the deferred loader half and its PIC-Rust-std wall, and the WASIX process models | `wasix-and-wasmer.md` |

Open and unresolved work extracted from these entries is maintained in
`.agents/docs/TODO.md`.

### Synthesis documents

| Synthesis document | Source LTM documents |
|--------------------|----------------------|
| `build-pipeline-synthesis.md` | `recovery-and-builder-provenance.md`\*, `image-discovery-and-rootfs.md`, `fusing-relocations-and-ifunc.md`, `function-boundary-recovery.md`, `runtime-metadata-producers.md`, `translation-linking-and-object-cache.md` |
| `build-system-and-driver-synthesis.md` | `bazel-build-and-hermetic-sdk.md`, `pipeline-driver-and-cli.md`, `recovery-and-builder-provenance.md`\* |
| `deployment-targets-and-profiles-synthesis.md` | `wasix-and-wasmer.md`, `wasm-runtime-and-oci-compatibility.md`\*, `web-embedder-and-browser-networking.md` |
| `ecvisor-runtime-synthesis.md` | `ecvisor-process-and-thread-model.md`, `ecvisor-vfs-syscalls-and-networking.md`, `wasm-runtime-and-oci-compatibility.md`\* |
| `engineering-practice-synthesis.md` | `performance-investigation.md`, `testing-and-regression-method.md`, `agent-harness-and-quality-gate.md`, `hot-path-cost-and-opt-in-design.md` |
| `target-enablement-synthesis.md` | `aarch64-lifter-and-coverage.md`, `python-redis-and-cryptography-bringup.md`, `ruby-jit-and-jump-table-bringup.md` |

\* Two source documents deliberately feed two syntheses each, because each is
read for a different question. `recovery-and-builder-provenance.md` supplies
what the pipeline INHERITED to `build-pipeline-synthesis.md` and image IDENTITY
to `build-system-and-driver-synthesis.md`;
`wasm-runtime-and-oci-compatibility.md` supplies the SUSPENSION model to
`ecvisor-runtime-synthesis.md` and the TARGET constraint to
`deployment-targets-and-profiles-synthesis.md`. Neither synthesis owns its
shared source, and no source document was moved or duplicated to avoid this.

### Intentionally standalone source documents

| Document | Reason |
|----------|--------|
| `dynamic-side-module-loading.md` | Already an end-to-end narrative crossing fuse, link, runtime, loader backends and the lifter. A synthesis over it would restate it rather than compress it; it supplies the `load-*` half to `deployment-targets-and-profiles-synthesis.md` by reference. |
| `postgres-and-guest-concurrency.md` | Workload-specific integration knowledge spans build and runtime boundaries and is most useful end to end. |

See `.agents/docs/LTM/INDEX.md` for the complete long-term memory index, which
also points at `.agents/docs/WASIX_ABI.md`, `.agents/docs/MULTIMODULE.md` and
`.agents/docs/QUALITY_GATE.md`.

## 2026-08-25 -- The TODO sweep, and a defect a safety property found

Four TODO entries re-verified against the tree rather than taken at their word,
per the rule at the top of `TODO.md`. Three were stale and are closed; the
fourth turned into code.

### Closed on evidence

* **"No end-to-end driver"** -- false since 2026-08-24. `pipeline.Build` and
  `pipeline.Run` are registered in `cmd/raptormark/main.go:28-29`.
* **"`builder/_tools/raptormark-builder-tools` is a PREBUILT binary"** -- closed
  STRUCTURALLY, which is what the entry said it was waiting for. It had
  considered and rejected two fixes and settled for "one forgotten command
  rather than one forgotten thought"; a third it did not consider -- moving the
  build into the dependency graph -- made the hazard unrepresentable.
  `grep -rn _tools` finds no remaining reader of the path. ⚠️ The stale binary
  is still on disk, read by nothing; left in place as a pre-existing user file.
* **Phase 4 of the dynamic side-module plan** -- its last open bullet named the
  wrong file. The mid-run §8 sequence was delivered in a SECOND harness,
  `e2e/testdata/hostedembedder.mjs`, not by extending `embedder.mjs`. Verified
  by reading it: `dylink.0` MEM_INFO at `:82` (and `:233` REFUSES a flat-linked
  module), `__wasm_apply_data_relocs` at `:302`, `ecv_side_loaded` at `:347`,
  all between `ecv_run_slice` calls. `embedder.mjs` was deliberately NOT
  extended -- it is the up-front path's only witness, and merging the two would
  cost that.

### `NetBackend::ready` on all four profiles, and what the move turned up

`epollSocketGuestSrc` MOVED to `e2e/epollsock_test.go` -- moved, not copied,
because a second caller is what should move a fixture. Three tests joined the
wasix one: wasmedge (the shipping profile), loopback under stock wasmtime, and
browser under Node with `--reentrant --net-v1`.

❗ **The loopback run failed, and not in `ready`.**
`FAIL getsockname gave a real port (errno=0)`. `net::loopback::bind` stored the
address verbatim, so `bind(port 0)` stayed bound to port 0.

The guest binds port 0 only so two profiles' runs cannot collide on a fixed
port. **The defect was found by the fixture's SAFETY property, not by its
subject** -- every earlier loopback test named a port, which is why nothing had
noticed in the life of the backend.

It was silent in both directions:

* `find_listener` matches 0 against 0 as happily as any other number, so
  connections still worked and only the advertised port was wrong -- which
  breaks the ordinary ephemeral-server shape (bind 0, ask what you got,
  advertise it) with no error anywhere.
* Two sockets binding 0 both became "bound to 0" and `find_listener` returned
  the FIRST for both: a silent cross-connect between unrelated servers.

Fixed by assigning from Linux's default ephemeral range (32768..=60999), lowest
free first. Deliberately deterministic rather than imitating a kernel's
randomisation: this backend has no entropy source, and determinism is what a
test can assert on.

`port_taken` keys on the PORT ALONE, deliberately, because it has to agree with
what `find_listener` and `find_bound_dgram` would match -- `find_listener`
treats an all-zero bind address as a wildcard and `find_bound_dgram` ignores the
address entirely. A stricter check would hand out a port those two then consider
taken.

Three host tests guard it. The neutralization (`addr.port = 0`) fired on two
with the intended diagnostics, not a compile error;
`an_explicit_port_is_left_alone` stayed green throughout and is the control that
refuses a fix which assigns unconditionally.

### The artifact differential, which is the part worth keeping

Side-built `raptormark-builder:lbport` onto the existing
`raptormark-elfconv-base-patched:wasix`, `BASE_ID` and `TRANSLATE_SH` passed
verbatim and both labels verified identical to `raptormark-builder:wasixnet`.

| archive | before | after |
|---|---|---|
| `loopback/libecvisor.a` | `2617a772...` | `2fe09dab...` |
| `libecvisor.a` (SHIPPING) | `47203e76...` | `47203e76...` |

The second row is a control that cost nothing to obtain: the shipping profile is
provably untouched. Every lift in the re-run was served from the object cache,
which is the `runtime/`-only reuse property working exactly as designed.

Gates: Go build/vet/test both patterns, `cargo fmt --check`, `cargo test` (304
pass), `cargo check --target wasm32-wasip1`, and
`raptormark bazel test //...` 13/13.

### Sweep, second batch: three re-verified, one self-correction

* **Plugin band / fused region.** The exclusion list the entry asked for is
  BUILT (`internal/image/plugins.go`) and excludes by **dependency, not name** --
  `jitSonamePrefix = "libLLVM"` -- because `llvmjit.so` is the only one of the
  79 that names libLLVM today and a name check would silently stop working for
  the next image. It deliberately did NOT use the requested
  `Report.SkippedExtras` name: a `Skipped` field existed briefly, was always
  nil, and `fuse.Fuse` ERRORS on an unsatisfiable plugin rather than skipping
  it, so the field would have claimed something untrue. What is left is only
  HEADROOM -- 89.7% of the region, ~16 MiB spare, and both other packing options
  are already spent.
* **`.ecv.funcs` re-measured.** Still emitted, still `SHF_ALLOC`, still no
  consumer in `runtime/`, `patches/` or `third_party/elfconv/`. On
  `python-glibc.fused` 186,144 B and on `nginx-alpine.fused` 322,272 B -- the
  largest `.ecv.*` section on both. ⚠️ `postgres-glibc.fused` predates the
  section, so the 942 KB figure could NOT be re-confirmed and is left attributed
  to its original measurement rather than restated as current.
  Not dropped in the sweep: the entry poses a choice between dropping and
  repurposing, and only one of those is reversible.
* **❗ Self-correction, recorded because it is the exact failure this file warns
  about.** I first wrote that `libcache_test.go` "does not read the section but
  WOULD fail without it" -- from the grep hit alone. Reading it shows the
  opposite: it SYNTHESISES an ELF with a section it happens to name
  `.ecv.funcs` and never touches the producer. Dropping the producer leaves it
  green. The claim was corrected in place before it could become the reason
  nobody dropped the section.
* **Shared-name path.** Re-verified unchanged and still accurately blocked:
  `RAPTORMARK_SHARED_NAMES` is gated in `experimental.go:68`, and
  `e2e_test.go:358` states the blocking invariant in the entry's own terms --
  the two objects' exported sets must be **DISJOINT**, which sharing inverts.
* **`QUALITY_GATE.md` §1 re-measured**: 301 -> **304** host tests (+3, the
  loopback ephemeral-port work), and the naive-grep delta re-checked at
  308 attributes vs 304 run -- the same four-comment gap that has held across
  every reading.

### Sweep, third batch: the arena entries, all four accurate

Four entries whose last verification stamp was 2026-08-11 re-checked against
`runtime/src/arena.rs` and `runtime/src/context.rs`. **None had drifted**, which
is worth recording as plainly as a correction would be -- the file warns that
much of this tree was reconstructed rather than recovered, so "still true" is a
measurement too.

* **The 96 MiB overlap is structural, not incidental.** `MEMORY_ARENA_SIZE` is
  384 MiB (`:37`); `MMAP_START_VMA = 0x1000_0000` and `MMAP_END_VMA =
  0x1600_0000` (`:40-41`) bound a 0x600_0000 span that `ShmWindow` carves
  DOWNWARD while `Arena::mmap_cur` bumps UPWARD into the same range. The two
  allocators share one span by construction and neither reserves anything for
  the other, so a large `MAP_SHARED` starving malloc's mmap fallback is the
  design working as written.
* **The shared-window floor is one line**, and it is exactly the claim:
  `context.rs:4461` is `self.shm_window.reserve(len, self.arena.mmap_cur)`, with
  `self.arena` the RUNNING process's. `ShmWindow::reserve` refuses only
  `at < floor` (`arena.rs:318`) -- that is the check a cross-process high-water
  mark would have to replace.
* **The 2026-08-21 partial-`munmap` fix is real.** `SharedSeg::unmap_is_whole`
  at `:194`, host tests at `:2647-2655` asserting all three arms (exact-length
  whole, over-length whole, short NOT whole). The head case does join the tail
  case and leak rather than release. The leak itself stays open, and the
  three-structure argument for why a split cannot be expressed is unchanged.
* **`fmov <Vd>.2S, #imm` is still a stub**, checked in the patch rather than in
  a built image: `patches/0010-fmov-vector-immediate-d2.patch` is the only patch
  naming `FMOV_ASIMDIMM`, and its own diff context leaves
  `TryDecodeFMOV_ASIMDIMM_S_S` as the generated `return false` while implementing
  only `_D2_D`. The patch is named `d2` for that reason.

### Sweep, fourth batch: a bare `wasmedge` is only a PARTIAL proposal gate

Two TODO entries -- the `--enable-all` decision and its 2026-08-22 header --
both rest on the same unstated assumption: that dropping `--enable-all` from the
e2e harness would restore a runtime no-proposal guard. **Measured 2026-08-25,
and it would restore part of one.**

Four minimal hand-authored modules under wasmedge 0.17.1, each differing from
its control in exactly ONE byte:

| module | bare `wasmedge` | `--enable-all` |
|---|---|---|
| `return_call` (tail calls, post-2.0) | **ACCEPTED** | accepted |
| shared memory (threads, post-2.0) | **REJECTED** | accepted |
| `call` (same module, 2.0) | accepted | accepted |
| unshared memory (same module, 2.0) | accepted | accepted |

The rejection is at PARSE time, before validation:

```
[error] loading failed: malformed limits flags, Code: 0x11d
[error]     At AST node: limit
[error]     At AST node: memory type
```

⚠️ **The two 2.0 rows are not padding.** Without them the table reads as "bare
wasmedge rejects small hand-written modules", which is a completely different
and wrong conclusion. The one-byte deltas -- opcode `0x12` -> `0x10`, limits
flag `0x03` -> `0x01` -- are what make each pair differ in the proposal and in
nothing else.

❗ **A first attempt at this control proved nothing and looked like it worked.**
The initial modules had no exported `_start`, so wasmedge answered
`A function name is required when reactor mode is enabled` and `rc=1` for
BOTH -- a non-zero exit that reads exactly like a proposal rejection and never
reached the parser at all. The tail-call row would have been recorded as
REJECTED, which is the opposite of the truth.

So the open decision is now "is a gate that catches THREADS but not TAIL CALLS
worth the change?" -- not nothing, since threads/shared memory is precisely what
the `wasix` side-module work needed and so the likeliest to creep in, but it
cannot be called "the runtime no-proposal guard". `wasm-opt`'s flag list
(`TestWasmOptEnablesNoProposal`) stays the authority: it is the only check that
is not one engine's opinion of one release.

Generator, since `.agents-workspace/tmp/` is disposable:

```python
b  = b'\x00asm\x01\x00\x00\x00'
b += bytes([0x01,0x04,0x01,0x60,0x00,0x00])          # type () -> ()
b += bytes([0x03,0x03,0x02,0x00,0x00])               # 2 funcs
b += bytes([0x07,0x0a,0x01,0x06]) + b'_start' + bytes([0x00,0x00])
b += bytes([0x0a,0x09,0x02, 0x04,0x00,0x12,0x01,0x0b, 0x02,0x00,0x0b])
# 0x12 = return_call (post-2.0); 0x10 = call (2.0). For the memory pair, swap
# the code section for `05 04 01 <flag> 01 01` with flag 0x03 shared / 0x01 not.
```

### Sweep, fifth batch: the runwasi shim INTERPRETS, and the compiler is not in it

The wasmedge AOT entry left two consequences open. Consequence (2) -- what the
containerd/runwasi shim actually does -- is now answered, and answered at the
EXACT version `e2e/containerd_test.go` pins (`containerd-shim-wasmedge/v0.6.1`),
not at `main`:

```toml
wasmedge-sdk = { version = "0.14.0", default-features = false }
default = ["standalone", "static", "plugin"]
```

❗ **There is no `aot` feature, so this is not a runtime configuration choice --
the compiler is not compiled into the shim binary.** That is a stronger claim
than "it defaults to the interpreter": an absent feature cannot be turned on by
a flag or an env var. `instance.rs` agrees and adds nothing -- default
`ConfigBuilder`, a `Vm`, and `vm.run_func(...)`, with no `Compiler`, no
`CompilerOutputFormat` and no compile step anywhere.

Two independent lines of evidence, and the Cargo.toml is the load-bearing one:
the source could in principle be read wrong, but a feature that is not enabled
cannot produce code that is not linked.

The entry framed this as a fork in the road, so it is worth saying which branch
we are on:

* ✅ **Our measurements DO describe the shipping configuration.** Every wasmedge
  number in this tree is an interpreter number and so is production. Nothing
  already recorded needs re-qualifying as "not the shipping case", which was the
  worse of the two outcomes.
* ❗ **And every guest ships ~35x slower than it needs to** -- 4,673 ms
  interpreted against 133 ms AOT on the measured call-heavy loop. That is now
  stated headroom rather than an open question, and it is the largest
  single-number performance fact in this tree that nothing is acting on.

⚠️ **READ, not RUN.** This is the shim's build configuration and source at the
pinned tag. No run was timed through containerd; the empirical confirmation is
the env-gated `RAPTORMARK_E2E_CONTAINERD` suite with a timing guest, which pulls
ubuntu and shim releases from GitHub and is not sweep work. Recorded as such
rather than as a measurement, because the distinction is exactly the one this
entry was created to make.

Consequence (1) is unchanged and still open.

### Sweep, sixth batch: the TLS refusal, and the coincidence that is real

* **Dynamic TLS for a dlopen'd unit** -- re-verified in the code, not just in
  the measurement. `context.rs:8704` returns `"the unit has its own TLS, which
  dynamic loading does not support yet"` after a
  `find_data_section(b".ecv.tls")` hit, with the reason at `:8690`: `setup_tls`
  lays out the STATIC block at bring-up and nothing extends it.
  ⚠️ **The refusal is what makes deferring this safe.** It is a named error, not
  a silent partial load, so a guest that ever does need dynamic TLS says so
  rather than resolving a symbol into a slot nobody allocated. Combined with the
  earlier measurement -- 1 of 159 real plugins, and that one is CPython's own
  test fixture -- deferral is justified twice over.
  ❗ Checked separately that `ensure_unit_code` does NOT carry the same refusal
  (only a range check at `:8756`). That split is deliberate: `sys_execve` used to
  inherit it and returned ENOEXEC for every fused DYNAMIC program, a defect no
  `gcc -static` test can see. Re-merging the two would bring it back.

* **Speculative reservations (⭐)** -- arithmetic re-checked independently, and
  it all holds:

  ```
  MEMORY_ARENA_SIZE 384 MiB = 402,653,184
  ruby (0x80000 * 32) * 24  = 402,653,184   <- EQUAL, and unrelated
  MMAP_END - MMAP_START     = 96 MiB        <- YJIT's 128 MiB cannot fit
  ```

  ❗ **The coincidence the entry warns about is REAL**, which is what makes the
  warning worth keeping. Two independently derived constants land on the same
  402,653,184 and nothing in ruby reads it from us. An investigator who checks
  only that the numbers match concludes ecvisor caused it and is wrong;
  recomputing ruby's expression from its own headers is what separates them, and
  it is one line.

  ⚠️ Left OPEN deliberately. It is explicitly "a decision and not work" -- grow
  the arena, split the windows, make reservations lazy, or accept the ceiling --
  and every option is a trade nobody has been asked to make. Raised to the user
  rather than swept.

### Sweep, seventh batch and close

* **`__remill_error` vs `__ecv_warning`** -- re-verified with line numbers so the
  next check is a read rather than a search. `__remill_error` is
  `intrinsics.rs:277`, ending in `fatal!` at `:290`; `__ecv_warning` is `:294`
  and posts SIGILL with a documented no-handler fallback. 17 lines apart, not
  ten; otherwise exactly as the entry describes.
  ⚠️ **The entry's caution is well-founded and should not be sanded off.** The
  asymmetry IS the argument: `__ecv_warning`'s guest WANTED the signal --
  PostgreSQL probes for ARMv8 CRC32 by executing the instruction under a SIGILL
  handler, so aborting removed a recovery it had already arranged -- and nothing
  has yet shown a guest that wants SIGTRAP from `brk`, while
  `__remill_error`'s own comment records the incident that made it loud.

### Sweep close: what it produced, and where the cheap yield ended

Seven batches. TODO went from 62 open / 3 closed to **59 open / 6 closed**, but
the count understates it: several entries that stay open are now open for a
NARROWER and better-stated reason than when the sweep started.

| outcome | count |
|---|---|
| closed on evidence | 6 |
| re-verified unchanged, with the check recorded | 13 |
| defect found and fixed | 1 (`net::loopback` ephemeral ports) |
| open decision materially sharpened | 2 (`--enable-all`, `.ecv.funcs`) |
| escalated to the user as a decision | 1 (speculative reservations) |
| of my own claims caught before landing | 2 |

❗ **The two self-corrections are the part worth carrying forward**, because both
were the same mistake: asserting from a grep hit or a non-zero exit code without
reading what produced it. Once it was `libcache_test.go` "would fail without
`.ecv.funcs`" (it synthesises its own section and never reads the producer);
once it was a proposal control that returned `rc=1` for both arms because
neither module had an exported `_start`, so nothing ever reached the parser.
Both would have been recorded as findings.

**Where the cheap yield ended.** What remains divides three ways and none of it
is sweep work: decisions the user has to make (the address budget); measurements
that need a build or an e2e run (the containerd timing, the `--enable-all`
change, closure re-pricing); and lifter coverage that needs a ~30-minute lift per
answer. The sweep was worth running for as long as an entry could be settled by
reading the tree, and that point has now been reached.

## 2026-08-25 -- Post-sweep: the neutralization image, and a fixture that is really gone

`raptormark-builder:wasixneut` deleted on the user's instruction. Targeted
`docker rmi`, **not** a prune: verified first that `wasixneut` was the ONLY tag
on image id `7e3155bbdf4e` so nothing else lost a name, and confirmed afterwards
that every other builder and the patched base survive. That distinction is the
whole of the Docker rule -- the images are the only copies of some things, and a
prune cannot be aimed.

### `raptormark-tmp-ossldgst:latest` is gone, and I got the reason wrong first

Found while reconciling the skip count of a green 130-pass e2e run. Three
OpenSSL tests cannot run here: the fast discovery-and-fuse guard plus the two
`RAPTORMARK_E2E_SLOW` ones, all behind the same `requireFixture`.

❗ **My first write-up said "the note explaining why it cannot be rebuilt is
absent too". That was wrong, and the mistake was method: I searched the docs and
not the source.** It is four lines above the constant
(`e2e/e2e_test.go:681-684`) -- a surviving PRE-WIPE Debian image whose entrypoint
hashes a file with the distro openssl over a stripped libcrypto, "the closest
thing the project has to a record of what already worked".

That also corrects what "unreproducible" means here. It is **not** "we lost the
recipe" -- the recipe is ordinary. What cannot be rebuilt is its VALUE: it
records the pre-2026-08-01 state, and a fresh `debian` + `openssl` today yields a
different and probably harder image. The rebuild would pass and would no longer
be evidence.

**The image is genuinely gone, not merely untagged**, which was the cheaper
explanation and had to be ruled out: 64 images predate the wipe and most are
`<none>:<none>`. All 57 distinct pre-wipe ids inspected for `openssl`/`dgst` in
`Config.Entrypoint`/`Cmd` -- zero hits, against a control that finds 2
`postgres` entrypoints in the same sweep.

⚠️ **The first version of that scan matched `sha256` and "hit" every image**,
because the image Id contains it. A filter that cannot fail is not a search, and
it produced a list that looked like ten findings. Same failure as the `_start`
control earlier today: a check that returns a positive for structural reasons,
believed because the output was non-empty.

### The dangling-pointer problem is general

13 code comments across Go, Rust and shell cite `.agents/docs/JOURNAL.md` for
facts the consolidation moved into `LTM/`. Spot-checked `socketpair`, `162 MB`
and `mapdir`: all absent from `JOURNAL.md`, all present in `LTM/`. The one quoted
BY SECTION NAME, `"nginx was a bad baseline"`, survives as
`LTM/function-boundary-recovery.md:50` under different wording -- repointable,
not lost.

Only `requireFixture`'s was repointed, because each of the 13 needs its target
checked individually and a bulk rewrite would just move the dangling somewhere
harder to notice.

## 2026-08-25 -- The OpenSSL fixture, rewritten to be buildable

The pre-wipe `raptormark-tmp-ossldgst:latest` is gone and not recoverable, so
the three tests behind it were rewritten against a fixture the suite builds
itself: `raptormark-e2e-ossldgst:v1`, from a digest-pinned `debian` base, with
`requireFixture` now BUILDING it and `t.Fatal`ing on failure rather than
skipping.

**Why the skip had to go.** When the old image was lost, three tests went
permanently quiet and reported it as a SKIP. That is `0 fail` with coverage
silently gone -- the exact shape `QUALITY_GATE.md` §5 warns about, arriving
through the fixture rather than through an env var.

### ❗ The property that did NOT survive, and why it is written in the code

The old image was PRE-WIPE: "the closest thing the project has to a record of
what already worked". That let a failure be read as *we regressed* rather than
*this input is new and hard* -- a distinction worth a lot, since a freshly
pulled image can be harder than anything the pipeline ever targeted.

The replacement is built from today's Debian and **cannot make that
distinction**. It is stated on `osslFixture` rather than in a doc, because the
cheap mistake here is to keep the old sentence and let a reproducible fixture
quietly claim to be historical evidence.

### ❗ The neutralization found a defect in my own recipe

The first Dockerfile ran `strip --strip-all` on libcrypto and installed binutils
to do it, assuming the fixture had to arrange stripping. **Rebuilding with that
line removed PASSED**, which is how the assumption was caught: Debian already
ships its libraries stripped. Measured -- libcrypto.so.3 is 6,302,952 bytes with
`.symtab` absent BOTH ways, byte-identical. The strip was a no-op that cost an
apt install, an apt purge, and two stray scripts the purge left behind (132 vs
130 discovered scripts).

Removed. `assertFixtureIsHard` now VERIFIES the property rather than the recipe
pretending to cause it -- which is strictly better, because it also fires if
Debian ever stops shipping stripped or the digest is repinned.

A second neutralization inverted all three of its arms; each fired with its
intended diagnostic, behaviourally rather than as a compile error.

⚠️ **RELR is GLIBC's here, not libcrypto's.** libcrypto carries only
`.rela.dyn`; `libc.so.6` is what has `.relr.dyn`. Pinned in the comment because
`assertNoUnrelocatedPointers` guards the RELR defect and a reader checking
libcrypto would find none and conclude the rewrite lost the property.

### Dangling pointers: 4 of 13 fixed, and one that could not be

`e2e/e2e_test.go`'s four references to `.agents/docs/JOURNAL.md` were repointed
after locating each target individually. Three moved to `LTM/`. The fourth could
not: **`__wrap_main`, cited by `requireE2E`, appears in neither `JOURNAL.md` nor
`LTM/`** -- the specific symptom is lost and only the general lesson survives, in
`AGENTS.md`. That citation now says so at the point of citation rather than being
deleted, because a deleted pointer looks like the detail was never there.

That one case is also the argument against bulk-rewriting the remaining 9: a
sweep would have pointed it at a document that does not contain it.

## 2026-08-25 -- The citation sweep, and `_recovery/` is not there

All 13 code citations of `.agents/docs/JOURNAL.md` resolved individually. No
live citation points at that file any more.

| outcome | n |
|---|---|
| repointed into `LTM/` | 6 |
| target gone from JOURNAL **and** LTM | 4 |
| the claim itself was STALE | 1 |
| not citations (skip messages) | 2 |

For the four whose target is gone, the citation says so AT THE CITATION rather
than being deleted -- a deleted pointer looks like the detail was never there.
Three of them already carried the substance in the surrounding comment (the
malloc doubling ladder, the nginx `setuid(101) failed (38)` symptom, the
"39,893 lines" instrument story), so those comments are now the only record and
are marked not to be trimmed.

### ❗ One "dangling pointer" was a false CLAIM

`e2e/net_test.go` said the ecvisor half of `TestNetGuestsNativeContract` could
not be built because "socketpair/sendmsg/recvmsg are absent ... the ecvisor side
is added when they land". **They landed.** All three are dispatched
(`sys.rs:697-699`) and implemented, not stubbed (`sys_socketpair` :4653,
`sys_sendmsg` :4714, `sys_recvmsg` :4812), and `uds_test.go` exercises guest
AF_UNIX end to end.

The comment now says UNBUILT rather than BLOCKED. The old wording read as a
standing impossibility, which is the kind of sentence that stops anyone trying
for years. A TODO entry records what building it actually needs: those guests
bind FIXED ports and talk to in-process Go peers, so the port and peer
arrangement has to be rethought, not just lifted.

⚠️ Worth generalising: a stale "we can't yet" comment is more expensive than a
stale pointer. A dangling pointer wastes a search; a false blocker closes off
work.

### ❗ `_recovery/` does not exist

`AGENTS.md:39` -- "untracked evidence from the 2026-08-01 rebuild, kept on disk
deliberately. Read it; never clean it up." `LTM/recovery-and-builder-
provenance.md:19` -- "The recovery evidence lives under `_recovery/`." It is not
on disk.

**Established**: absent; `/_recovery/` is in `.gitignore:3`; never tracked, so
git cannot date or explain the loss. ❌ **Not established**: that anything
deleted it. It may never have existed on this machine, and saying otherwise
would be inventing a cause for a fact that has none available.

❗ **The actionable part is that its absence is INVISIBLE.** All four references
-- `.gitignore`, `.bazelignore`, `BUILD.bazel`'s `gazelle:exclude`, and a
`filepath.SkipDir` arm in `internal/builder/workspace_test.go:77` -- are
EXCLUSIONS. Every one tells a tool to ignore it; not one requires it to exist.
A directory two documents call load-bearing evidence is protected by a rule and
guarded by nothing.

Same shape as the lost OpenSSL fixture, one level up, and that is now twice in
one day: **a rule that says "do not delete this" is not a guard.** The fixture
at least announced itself through a skip. This announces itself through nothing.

## 2026-08-25 -- `_recovery/` confirmed gone; documents corrected

The user confirmed no copy survives. Everything that described it in the present
tense has been corrected rather than left to mislead: `AGENTS.md` (the "read it;
never clean it up" rule), `LTM/recovery-and-builder-provenance.md` in three
places -- including the sentence that called the evidence "preserved" -- and
`LTM/agent-harness-and-quality-gate.md`.

**What is recoverable about its contents is two names**, from surviving
references: `_recovery/RECOVERY.md` and `_recovery/reference/`.

⚠️ **`_recovery/RECOVERY.md` is NOT the recovery journal**, and conflating them
is the easy mistake: the journal was `RECOVERY.md` at the repo ROOT until
2026-08-09 and is now `.agents/docs/JOURNAL.md`. That one survived. The
`_recovery/` file kept the same name and did not. Anyone who notices only that
"a RECOVERY.md exists" will conclude the evidence survived.

What the evidence ESTABLISHED does survive in
`LTM/recovery-and-builder-provenance.md` -- which components are recovered
versus reconstructed, and how the patched elfconv tree was validated. What is
gone is the ability to re-read it and check a NEW question against it.

### The lesson, which is the only part that can still prevent something

All four references to `_recovery/` -- `.gitignore`, `.bazelignore`,
`BUILD.bazel`'s `gazelle:exclude`, and a `filepath.SkipDir` arm in
`internal/builder/workspace_test.go` -- are EXCLUSIONS. Every one told a tool to
ignore it; **not one required it to exist.** Every gate stayed green while it
vanished, and nothing ever reported it.

Twice in one week now, the other being `raptormark-tmp-ossldgst:latest`. That
one at least announced itself as a skip; this announced itself through nothing.

❗ **A "do not delete this" rule in a document is not a guard.** A new TODO entry
asks for real checks on what is still irreplaceable -- the pinned elfconv base
and patched builders, `.agents-workspace/fixtures/`,
`.agents-workspace/drivers/` -- with the explicit constraint that a check which
SKIPS when the thing is missing is not a guard, since that is precisely how the
OpenSSL fixture went quiet for an unknown length of time.

The exclusions are left in place: they cost nothing and are what a re-derived
`_recovery/` would need.

## 2026-08-25 -- `raptormark preserve`: turning the rule into a guard

Built the follow-up the two losses opened. `internal/preserve` +
`.agents/preserve.json`, wired as `raptormark preserve snapshot|check`, 9 host
tests, Bazel 14/14.

### The design decision, because the obvious build is wrong

A list of things that must exist, failing when one does not, is the natural
implementation and it would be deleted within a week: on a fresh clone NOTHING
is present, so it fails for everyone who has lost nothing. A check that cries
wolf gets switched off, and then there is no check -- which is where this
started.

What was missing is not detection of ABSENCE but of DISAPPEARANCE, and those
differ by a baseline:

| state | answer |
|---|---|
| recorded and present | ok |
| recorded and MISSING | ❗ non-zero exit |
| not recorded | "nothing is known" -- not an alarm, not an all-clear |

⚠️ The manifest lives under `.agents/`, NOT `.agents-workspace/` (documented
disposable, and one of the things being guarded), and is meant to be committed.
A manifest kept beside what it guards dies with it -- precisely how the
`_recovery/` references failed: every one was in a file perfectly happy in its
absence.

❗ `snapshot` REFUSES to record something already missing. A manifest listing a
lost thing fails forever, and a check that can never pass is one somebody
switches off. That refusal is a test.

### Proven against the actual losses

Given a manifest naming `_recovery/` and `raptormark-tmp-ossldgst:latest`,
`preserve check` reports both with their notes and exits 1. That is the
counterfactual run for real: had this existed, neither loss would have been
silent.

Baseline recorded here: the patched base, `raptormark-builder:lbport`, and
`.agents-workspace/{fixtures,drivers,objcache}`.

### ❗ A neutralization that did not neutralize, again

The unknown-kind guard "passed" under neutralization, which reads exactly like
an assertion observing nothing -- and I was one step from recording it as
vacuous. It was the NEUTRALIZATION that was broken: the injected
`s.Present = true` sat one line ABOVE the original `s.Present = false`, which
overwrote it on the next statement. Re-run against the real assignment, the
guard fired with its intended message.

Third time this week that a check-of-a-check was the thing at fault (the `_start`
proposal control, the `sha256` image filter, now this). The pattern is
consistent: **when a probe returns the comfortable answer, suspect the probe.**

### What this does NOT solve

Nobody is obliged to run it. It is a command, not a gate. Wiring it into the e2e
suite was considered and NOT taken: a fresh machine legitimately has none of
these, so failing there would be wrong, and gating on "manifest exists"
reintroduces exactly the skip-shaped silence that hid the OpenSSL fixture. The
honest claim is narrow -- an undetectable loss is now a detectable one, and
someone still has to look.

## 2026-08-25 -- The ecvisor half of the net pair, finally built

`e2e/netfork_test.go`: `TestNetForkServerUnderEcvisor` (shipping profile,
wasmedge) and `TestNetForkServerUnderLoopback` (in-process network, stock
wasmtime). Both green alongside the native baseline that runs the same source on
this host, which is what makes it a differential rather than an assertion.

This existed only because the previous entry found the blocker was gone. The
pair had a trusted native half and nothing checking that ecvisor reproduced it,
guarded by a comment asserting syscalls that had since landed.

### What it covers that nothing else did

Sockets AND fork in one process, with the two ends of a connection on either
side of the fork. `uds_test.go` is AF_UNIX; the socket tests do not fork; the
fork tests do not open sockets. **Interactions are what a suite of
single-subsystem tests cannot reach**, and this is one.

⚠️ The loopback run was the one expected to be able to fail legitimately:
`net::loopback` keeps its sockets in a `Vec` inside the runtime, so "the child
connects to the port the parent bound" is a question about that table's
interaction with ecvisor's fork -- something the wasmedge run cannot answer,
because there the host kernel owns the sockets and fork is not involved in
reaching them. It passes.

It also exercises the same-day ephemeral-port fix THROUGH a fork. Before that
fix the child would have dialled port 0, `find_listener` would have matched it,
and the guest would have PASSED while every real server advertised port 0.

### Only one of the three guests, and the reason is a fixture problem

`netServerSrc` binds a fixed 47826 and `netClientSrc` dials a fixed 47825, each
expecting an in-process Go peer. Lifting them as they stand would add two more
fixed-port guests to a suite where `AGENTS.md` already documents what fixed
ports cost. They need `netForkServerSrc`'s arrangement -- bind 0, report back --
which changes the NATIVE test too, so it is a rewrite of the pair rather than an
addition. Recorded as open rather than done quietly.

### The assertion was weak and was tightened after it passed

Neutralized by making the guest exit 0 while printing nothing; the banner check
fired, which also measured that neither wasmedge nor wasmtime emits "ok" itself.
The assertion was still tightened from `"ok"` to `"ok\n"` afterwards: bare "ok"
is a substring of too much, and "no current runtime emits it" is a property of
two releases, not a guarantee. ⚠️ Passing a neutralization is not a reason to
stop improving an assertion that is weak on its face.

### ❗ Correction, same day: the fixed ports were deliberate

I wrote that the other two `net_test.go` guests were blocked by "a FIXTURE
problem, not a runtime one", implying their fixed ports were careless and the
fix was to rewrite them like `netForkServerSrc`. **Wrong, and wrong in the
direction that matters** -- it would have become the basis for someone
overriding a decision without knowing one had been made.

`net_test.go:39-42` states it plainly: the ports are the ORIGINALS' and are
fixed rather than ephemeral "because the guest's whole job is to be found at a
known address by a peer it does not coordinate with". A server binding a known
port is what a server does.

I still do not find that decisive -- the non-coordinating peer here is the
harness, which can coordinate -- but the difference between "nobody thought
about this" and "somebody decided this and said why" is the difference between a
cleanup and an override.

It also missed the real obstacle. A LIFTED guest runs inside a container, so its
loopback is not the host's unless the container gets `--network host` (what
`wasixnet_test.go` needs) -- and that shares the host port space, which is
exactly when a FIXED port bites. So the options are: rewrite the pair to
coordinate a port, changing the trusted NATIVE half whose entire value is being
trusted; or add two more fixed-port guests to a suite that documents them as a
hazard. Neither is free, and it is not a sweep's call.

⚠️ I found this by READING the guests before editing them, which I only did
because the plan was to rewrite them. Had the task been smaller I would have
acted on the summary I had already written. **A rationale three lines above the
thing you are about to change is easy to miss precisely when you have already
decided what to do.**

## 2026-08-25 -- Sweeping for stale BLOCKERS, and an unguarded third copy

Today's most expensive finding was that a false blocker closes off work in a way
a stale pointer does not, so the obvious follow-up was to look for more of them:
a scan of every comment in Go and Rust for forward-looking capability claims
("not yet supported", "when X lands", "no consumer").

Most hits were false positives -- `blocked on` in `context.rs` and `sys.rs` is a
process blocked on a syscall, not a claim. One was real, and it was worse than
the one that started this.

### `internal/rootfs/boot.go` said the dlopen map had no consumer

Dated 2026-08-23: "THIS IS A PRODUCER WITH NO CONSUMER. The runtime does not
read it yet -- that is Phase 2 of the dynamic side-module loading work", and
"TestDlPathAgrees pins the two that exist today".

**Both halves went stale together, and the second is the damaging one.** The
runtime side landed -- `runtime/src/dlmap.rs:27` defines `DL_PATH` and
`context.rs:2479` reads it from the VFS at boot -- so a THIRD copy of the string
appeared, in a different language, guarded by nothing. The same is true of
`EXEC_PATH`: three copies, Go pair pinned, Rust unpinned.

⚠️ **The comment was ACCURATE WHEN WRITTEN**, which is exactly what made it
dangerous. It said "the two that exist today" and nobody re-read it on the day a
third arrived. A dated status claim ages into a false one without anyone
touching it.

### What drift would have looked like

Nothing fails to build and no test fails. `Vfs::read` returns None for a path
nothing wrote, `DlMap::load` gets `None`, and every guest `dlopen` fails with
"cannot open shared object file".

❗ **`AGENTS.md` already attributes that exact symptom to a DIFFERENT cause** -- a
`RAPTORMARK_ROOTFS` naming a host path instead of a guest one. So the diagnosis
would be led straight to the wrong subsystem, which is worse than an
unexplained failure.

### The guard

`internal/rootfs/runtimeagree_test.go` scans the Rust source and ties all four
definitions to one value, following `//runtime:cshim_equivalence_test`'s
approach: there is no build step that could hand a Rust `const` to Go, so a
transcription check is the available tool.

❌ It FAILS rather than skipping when the constant cannot be found. A skip would
recreate the silence this tree paid for twice this week.

Neutralized both ways, both firing with their intended diagnostics: the VALUE
changed (reports the drift and what it would cost), and the constant RENAMED so
the pattern matches nothing (reports that a pattern matching nothing passes
vacuously). The second case is the one that matters -- a regex guard whose
pattern stops matching is green forever.

### Generalising it: every value crossing the Go/Rust boundary

`AGENTS.md` calls `internal/` and `runtime/` a producer/consumer pair "and it is
not symmetric", so the `DL_PATH` gap is a shape rather than an incident. Swept
the whole boundary.

**Section names — clean, and clean in the direction that matters.** Diffing the
`.ecv.*` names Go EMITS against those Rust CONSUMES leaves two Go-only entries
and **zero Rust-only**. Zero is the important number: a consumer with no producer
is this tree's recurring defect, found one at a time behind each other.

Of the two Go-only:

* `.ecv.funcs` — known, documented, genuinely unconsumed.
* `.ecv.tlsdesc` — ⚠️ **NOT a gap, and checked rather than assumed.** It is
  `SHF_ALLOC|SHF_EXECINSTR` and holds two aarch64 instructions
  (`ldr x0,[x0,#8]; ret`); `applyTLSDescFixups` patches descriptors to point at
  it, so its consumer is the GUEST'S OWN CODE via `blr`, not the runtime. A
  name-only diff would have filed this as a second `.ecv.funcs`.

**The rfs MAGIC was pinned only against a hand-copy.** `rfs.go` says "Keep the
two in lockstep -- the reader is the specification", and `rfs_test.go` implements
a reader "transcribed from runtime/src/vfs/rfs.rs". The transcription is a
genuinely useful independent implementation, but it is a SNAPSHOT: if the real
reader's magic changed, the Go test would keep passing against its own copy while
every sidecar was rejected at boot. Now scanned from the real source.

`TestRuntimeAgreesOnTheSidecarABI` covers all three, and each case carries what
DRIFT WOULD COST, because they fail in visibly different ways:

| constant | cost of drift |
|---|---|
| `EXEC_PATH` | every execve falls back to program 0 — the silent wrong-program run behind four recorded incidents |
| `DL_PATH` | every dlopen fails with a message AGENTS.md attributes to a bad `RAPTORMARK_ROOTFS` |
| `MAGIC` | sidecar rejected at boot — the loudest, and the only one that fails immediately |

⚠️ One regex covers both Rust const forms on purpose. Two patterns would be two
ways to stop matching, and a pattern that stops matching passes vacuously --
which is why `rustConst` FATALS when it finds nothing rather than returning "".
Neutralized three ways in total: value drift, constant renamed, and now the
MAGIC case; all three fire with their intended diagnostics.

### The run states crossed THREE languages and were tied in none

`runtime/src/entry.rs:549-551` defines `ECV_IDLE`/`ECV_PREEMPTED`/`ECV_EXITED` as
`ecv_run_slice`'s return codes; `e2e/testdata/hostedembedder.mjs:330` hard-codes
the same three numbers to interpret them. Nothing compared the two.

❗ **Drift here produces a CLEAN EXIT 0 with the guest's work undone** -- the
embedder reads PREEMPTED as EXITED and stops early, or IDLE as PREEMPTED and
spins. That exact symptom has already been paid for once from a different cause
(a wake that set `Runnable` without enqueueing), and it took a run to find
because nothing reports it.

⚠️ And the numbers are not obviously right: the embedder's own comment warns that
"ECV_IDLE IS 0 AND ECV_PREEMPTED IS 1, which reads backwards if you expect the
busy state to be the falsy one". A pairing that reads backwards is one somebody
eventually "corrects".

`e2e/abiagree_test.go` ties them, and **is deliberately NOT gated on
`RAPTORMARK_E2E`**. Every other test in that package skips without Docker; this
one reads two source files and needs nothing, so gating it would make the guard
absent exactly when the cheap gate runs. Twice this week a check announced its
own absence as a skip and nobody read it. `AGENTS.md` requires `go test ./...` to
need no Docker, root or network -- honoured by needing none, not by skipping.

Three neutralizations, three distinct diagnostics:

| neutralization | what fired |
|---|---|
| renumber `ECV_PREEMPTED` in Rust | "is 7 in entry.rs and 1 in the embedder" |
| collapse `ECV_EXITED` onto `ECV_IDLE` | "two run states are indistinguishable" |
| break the regex | "matched nothing ... passes vacuously" |

The third is the one that matters and is now standard for every source-scanning
guard in this tree: `scanConsts` FATALS on an empty result, because a pattern
that stops matching is green forever.

### The rfs header layout, and one surface deliberately left unguarded

**Guarded: the header offsets.** The Go writer puts ten fields at fixed offsets
and the Rust reader picks six back out with `u32le(&data, N)`; neither side NAMES
them, so a field that MOVES is caught by nothing -- the reader keeps reading the
old offset and gets whatever now lives there.

❗ That is worse than a parse failure. Reading `dirent_off` where `name_off` was
expected yields a plausible number, and the guest sees a VFS whose files have the
WRONG CONTENTS rather than a sidecar that fails to load.

⚠️ The guard deliberately does NOT map Rust field names to Go ones -- that
correspondence would itself be a transcription, and a wrong transcription passes.
It asserts the weaker, checkable thing: **every offset the reader reads is an
offset the writer writes** (measured: 6 within 10). Move a Go field and some
reader offset stops being in the written set.

Neutralized both: moving `inodeOff` 24 -> 28 reports "reads the header at offset
24, but ... writes no field there"; changing the reader's minimum to 96 reports
the length mismatch.

**NOT guarded, on purpose: the `ECV_REG_*` codes.** Both embedders carry a
`-1..-5` table, so it looked like the same shape. It is not: the codes are used
only to make a diagnostic READABLE, and the behavioural check is `rc !== 0`,
which no renumbering can break. A guard there would pin message wording rather
than correctness. The two embedders already word `-3` differently and both are
right.
⚠️ Recorded because "these constants appear twice" is not sufficient reason to
tie them; what matters is whether anything BEHAVES differently when they drift.

### ❗ My own guard broke the Bazel gate, and the fix was better than the guard

`e2e/abiagree_test.go` passed under `go test` and **FAILED under
`bazel test //e2e:e2e_test`**: `reading ../runtime/src/entry.rs: no such file or
directory`. Bazel runs tests in a sandbox containing only DECLARED inputs, which
is the same reason `//internal/builder` and `//internal/rootfs` are tagged
`manual`.

Found only because the Bazel arithmetic was being reconciled for an unrelated
reason -- 16 declared test targets minus 2 `manual` = 14, matching what Bazel
reports. ⚠️ **A new test that passes `go test` and fails `bazel test` is exactly
the drift `AGENTS.md` warns about**, and I would have shipped it.

Three options, and the tempting one is wrong:

* ❌ Skip when the file is not found. This is the antipattern the guard exists to
  avoid -- it would go quiet precisely in the environment nobody watches.
* ❌ Tag the package `manual`. Loses the Bazel COMPILE coverage of every e2e
  test, to fix one file.
* ✅ DECLARE THE INPUT. `//runtime:srcs` already globs every `.rs`; adding it to
  the e2e target's `data` makes the guard run in BOTH environments. A declared
  input is strictly better than opting out of the gate.

`runtimeSrc` tries the `go test` path and the runfiles path, and **FATALS** if
neither resolves, with the reason spelled out at the call site.

✅ Verified the Bazel path reads the REAL file rather than one that merely
exists: renumbering `ECV_EXITED` to 9 in `entry.rs` makes the SANDBOXED run fail
with "is 9 in runtime/src/entry.rs and 2 in the embedder". Without that check, a
data dep that staged the wrong tree would have looked identical to a working one.

⚠️ `internal/rootfs/runtimeagree_test.go` needs no equivalent: that package is
already `manual` for this very reason, so its relative-path reads are the
established arrangement rather than a new liberty.

### Session close: the suite, and two vacuities caught in my own work

**Final e2e reading: 423 s, 134 pass / 0 fail / 19 skip** on
`raptormark-builder:lbport`, with wasmer and Node enabled. Every delta since the
first run today is attributable to a named test -- 130 -> 131 was the OpenSSL
guard going SKIP -> PASS once its fixture became buildable, and 131 -> 134 is the
two netfork tests plus the ungated ABI guard.

Two things caught in my OWN work by asking it the question this file asks of
everything else -- **what would this actually fire on?**

* **`"recorded": "unset"` in the preserve manifest.** A field carrying a value
  that means nothing is worse than no field, because it looks answered. The CLI
  now stamps a real timestamp; the PACKAGE stays clock-free so its tests remain
  deterministic. The split matters: testability was the reason the field was
  caller-supplied, and that reason does not extend to the caller.
* **❗ The manifest listed ITSELF, and that entry can never fire.** If
  `.agents/preserve.json` is deleted, `Load` reports not-recorded and `check`
  prints "nothing recorded" -- the entry that would have complained went with the
  file. It reads as thorough and protects nothing. Removed within the hour.
  What actually protects the manifest is that it is COMMITTED: `git status`
  shows its deletion. **A guard has to live outside the thing it guards**, which
  is the same property the manifest gives everything else, applied one level up.

⚠️ Both are the same error as the ones found in the tree today, made by the
person who had just spent the day finding them. That is worth recording plainly:
the discipline is not a state you reach, it is a question you keep asking.

## 2026-08-25 -- The net guests rewritten to coordinate, on the user's decision

The fixed ports are gone. `netServerSrc` binds 0 and announces `PORT <n>` on
stdout; `netClientSrc` takes the port on argv and refuses to default.

❗ **The blocker was the HARNESS, not the guests.** `runGuest` buffered stdout and
returned it only after `Wait()`, so an ephemeral port could not reach the peer in
time -- the peer would have waited for a process that was itself blocked in
`accept()` waiting for the peer. The fixed ports were that limit's CONSEQUENCE.
`streamPeer` now hands the peer a `nextLine` that reads stdout while the guest
runs, and the whole obstacle dissolves.

That is worth generalising: **a constraint documented with a good reason can
still be downstream of an unexamined limitation somewhere else.** The stated
rationale -- "a server's job is to be found at a known address by a peer it does
not coordinate with" -- was true and was not the reason the ports could not move.

### What the ecvisor halves buy

| test | covers |
|---|---|
| `TestNetServerUnderEcvisor` | lifted guest LISTENS, host peer connects |
| `TestNetClientUnderEcvisor` | lifted guest DIALS OUT to a host listener |

`TestNetClientUnderEcvisor` is the only thing exercising `sock_connect` against a
listener **ecvisor does not own** -- every other socket test binds and accepts,
or connects to something inside the same guest. Both use `--network host`, which
is safe only BECAUSE nothing is fixed any more.

### ❗ Two harness defects, both invisible to review and found by running it

* **`io.Pipe` is SYNCHRONOUS.** Teeing stdout through it made every guest write
  block until someone read. The forkserver's peer reads nothing, so the guest
  deadlocked on its first write and the suite HUNG rather than failed -- the
  failure shape that carries no diagnostic. Fixed with a drained `io.TeeReader`
  and a non-blocking send.
* **The runtime shares the guest's stdout.** `wasmedge --enable-all` opens with
  "component model is enabled, this is experimental", which the first version
  took for the port announcement. `portFromStream` skips chatter, BOUNDED at 20
  lines: scanning until found would turn a guest that never announces into a
  hang.
  ⚠️ `assertForkServerOK` had already anticipated exactly this hazard for its
  banner. Anticipating it in one place did not stop me writing it in another.

### The trusted half stayed trusted

The NATIVE baseline was re-run at every step and stayed green, including after
`runGuest` was refactored to share `streamPeer`. Its assertions were neutralized
again afterwards -- wrong reply, and a missing `PORT` line -- because a rewrite
that quietly weakened the half whose entire value is being trusted would have
been the real cost of this change, and "it still passes" does not distinguish
that from "it still checks".

### ❗ The rewrite SPENT reconstruction evidence that cannot be renewed

`net_test.go`'s package doc recorded something easy to miss while editing the
guests: all three were **reconstructed by disassembling** the lost
`raptormark-test-net*` fixture images, and equivalence was then established --
each compiled and run against the same peer as its original, matching on stdout,
exit status **and syscall sequence**.

Changing `netserver` and `netclient` breaks that. The current sources are no
longer the verified reconstructions: `netserver` gained a `getsockname` and a
`printf` before its first `accept`, `netclient` reads a port from argv, and both
syscall sequences now differ from the originals' by construction.

⚠️ **And it cannot be re-established.** The fixture images are unreproducible
artifacts of the lost tree -- the same category as the two things confirmed gone
this week. The fidelity evidence was SPENT, not renewed.

That is a real cost of the change and is now stated in the package doc rather
than left for someone to infer from a date. `netforkserver` is untouched and its
equivalence still stands, which is part of why it was the one lifted first.

⚠️ Worth generalising: **"this source was verified against something that no
longer exists" is a property an edit destroys silently.** Nothing in the build,
the tests or the review would have flagged it -- the guests still compile, still
pass, and still do what the tests say. The only witness was a paragraph of prose
three screens above the code being changed, and it was found by re-reading the
doc for an unrelated reason (its stale port claim).

The stale claims themselves were corrected in the same pass: the fixed-port
paragraph, and two subtest comments still naming 47825/47826.

### The last unspent reconstruction, turned from prose into a guard

Having just SPENT the reconstruction evidence for two guests, the obvious
question was what protects the third. Answer, until today: a paragraph in
`net_test.go`'s package doc, three screens above the code.

That is the shape this tree paid for twice this week -- `_recovery/` and the
OpenSSL fixture were both protected by prose and both went silently.

**Measured before building.** `strace` is on the host and the native guests run
there, so the recorded sequence is directly checkable. It matches EXACTLY, both
processes:

```
parent: socket bind listen getsockname clone accept read write wait4 close close write
child:  socket connect write read close
```

`TestNetForkServerMatchesItsRecordedSyscallSequence` now enforces it.

Design points worth keeping:

* **The expectation is TRANSCRIBED from the package doc, not derived from a
  run.** Deriving it would make this a change-detector that ratifies whatever
  the guest currently does -- the opposite of evidence.
* **Exactly two traced pids is asserted.** Fewer means the fork did not happen or
  strace did not follow it, either of which would leave one sequence compared and
  the other silently unchecked.
* **`accept4` -> `accept` and `clone3` -> `clone` are folded, nothing else.** Only
  where the kernel offers two spellings of one operation; folding more would let
  a behavioural change hide behind a rename.
* ⚠️ **It SKIPS when strace is absent or ptrace is restricted**, which is the
  antipattern this week has been about. Taken deliberately: strace absence is an
  environment fact, not a defect. The skip message therefore says what COVERAGE
  IS LOST, not just why it skipped -- "an edit to that guest will go unnoticed on
  this machine".
* ⚠️ It compares against the RECORDED sequence, not the original binary, which is
  gone. It proves the guest still does what the reconstruction was verified to
  do; it cannot re-verify the reconstruction.

Neutralized by adding one `close()` to the guest: the guard fails, prints both
sequences, and names what an intentional change would be spending.

### ❗ The net rewrite broke `TestWasixProfileServesTheSocketABI`, and why the miss happened

The full suite caught it: 135 pass / **1 fail** / 19 skip, the failure being both
subtests of `TestWasixProfileServesTheSocketABI` timing out on port 47825.

**The miss was specific and worth naming.** Before the rewrite I checked the
blast radius of the HARNESS -- every caller of `runGuest` -- and found it
contained to two files. I did not check the blast radius of the GUEST SOURCES.
`wasixnet_test.go` compiles both `netServerSrc` and `netClientSrc`.

❗ **And its fixed port was load-bearing for a DIFFERENT reason than the native
test's.** Its comment says so directly: "THE PORT IS THE ASSERTION. The guest
binds 47826; if the encode side byte-swapped it, the bind would SUCCEED on 26810
and this dial would time out."

**`bind(0)` has no port to encode.** Converting that test to the coordinating
form would have left it green while testing nothing on the encode side of the
address codec -- the precise silent vacuity this whole week has been about, and I
would have introduced it while writing guards against it.

The fix keeps both modes rather than choosing: `netServerSrc` takes an OPTIONAL
port. Absent means bind 0 and announce (coordination, no collisions); present
means bind exactly that (the only way to exercise encode). `netClientSrc`
already took argv, so `wasixnet_test.go` passes 47825 and keeps its known
number, because a byte-swap sending the guest to 26810 is only legible against
one.

⚠️ **Third time today a rationale sat beside the code I was changing and I did
not read it** -- the fixed-port paragraph, the reconstruction-equivalence
paragraph, and now this. The pattern is consistent and worth stating as a rule:
**I read the file I am editing, not the files that depend on it.** Grepping for
callers of a FUNCTION is habit; grepping for users of a DATA CONSTANT is not,
and a test fixture is data.

Also hit the backtick trap a second time: `wasixnet_test.go` in backticks inside
a backtick-delimited Go raw string terminated the C source. The compiler caught
it both times, which is the only reason it costs a minute rather than a session.

## 2026-08-25 -- The one hazard AGENTS.md bolds and nothing checked

`AGENTS.md` states the rule and its consequence in the same breath: a new `.go`
file must be added to its package's Bazel `srcs`, and "`gofmt`/`go build`/
`go vet`/`go test` all stay green when you forget; `bazel test //...` reports
'N tests pass and M were SKIPPED', which reads like caching and is not."

It has been paid for once -- 2026-08-24, when the omission broke
`//cmd/raptormark:builder_tools_linux_arm64` and therefore `//builder:stage`, so
**the builder image could not have been built** while every Go gate was green.

Nothing checked it. `internal/builder/bazelsrcs_test.go` now does.

**Swept before building it**: every package with Go files and a `BUILD.bazel` was
already complete, including the six files added today. So this guards a
discipline that is currently holding, which is the right time to add one -- a
guard introduced alongside a violation cannot tell you whether it works.

Design notes:

* It compares against filenames listed ANYWHERE in the `BUILD.bazel`, not
  per-rule. Which rule a file belongs to is rules_go's business and it says so
  loudly; a file in NO rule is the silent case. Checking the weaker property
  keeps this from becoming a second, worse copy of the build graph.
* ❌ No exclusion list for generated files or `//go:build ignore` scripts,
  because this tree has none. Adding one before it is needed is a hole waiting
  for a file to fall through.
* ❗ **A positive control on the walk**: fewer than 5 packages examined is a
  FATAL. Finding nothing is the only way this test can be wrong while looking
  right, and that is exactly the failure it exists to prevent elsewhere.

Neutralized both ways: dropping `netsyscall_test.go` from `e2e/BUILD.bazel`
names the file and quotes what it cost last time; pointing the walk at a
filename that does not exist reports "only 0 packages ... the comparison is
vacuous".

⚠️ Worth noting what it does NOT cover: the `deps` half of the same rule. That
one at least fails the Bazel BUILD rather than skipping, so it is loud -- and
`AGENTS.md` now says which half is guarded and which is still yours to remember,
rather than leaving the reader to assume the whole rule is covered.

## 2026-08-25 -- Auditing the adopt/publish invariant: no fourth hole, one misleading comment

`TODO.md` records the `call_history` adopt/publish invariant as having no
enforcement, three holes found in three places, and this warning: "A green suite
is not evidence there is no fourth."

The structural fix it proposes is explicitly conditional -- "if this feature is
kept" -- and whether to keep the opt-in is a separate open DECISION, so that was
not done. The AUDIT is decision-free and useful either way, so that was.

**All 8 mutation sites and all 10 bracket sites enumerated and matched.** The
coverage argument turns out to be simpler than the site list suggests:

* `intrinsics.rs:486-488` adopts before `sys::svc` and publishes after, and
  **that is what covers every context switch** -- suspension is decided INSIDE
  `svc`, so the adopt necessarily precedes the `mem::take` at `context.rs:3263`.
  Its own comment names hole #3: adopting only in the `_ic` variants left fork's
  snapshot short by every inline-pushed frame, and "a forking guest produced NO
  output and exited 0".
* `entry.rs:408` publishes before entering lifted code, covering `load_current`
  swapping a different vector in.
* `entry.rs:459` adopts before the replay pop (hole #1); `entry.rs:188`
  publishes during bring-up (hole #3's other half).

### ❗ The finding was a comment, and it is the shape that matters

`_ecv_save_call_history` carried: "adopt them before adding ours, and republish
afterwards in case the push moved the buffer." **It does neither.** That text
describes `_ecv_save_call_history_ic`, which brackets a call to it; the comment
stayed behind when the two entry points were split.

The code is correct -- a module built without the fast path leaves
`ecv_ch_built` at 0, so both bracket calls would be no-ops anyway. But an auditor
reading the plain function sees the invariant apparently honoured there and
stops.

⚠️ **That is exactly how this entry says holes two and three survived the
first**: "the design was declared complete after the first". A comment asserting
an invariant is upheld where it is not does not just fail to help -- it actively
terminates the search. Corrected in place, including why the plain entry point
genuinely needs no brackets.

### What the audit does not establish

The `resume_scheduling()` path -- a fresh program returning without exiting --
was traced least conclusively: its balance rests on inline epilogues having
decremented the global symmetrically, which is an argument rather than an
observation. Stated in the TODO rather than rounded up to "audited clean".

And the entry's own point stands unchanged: **a completed audit is not
enforcement**, and the next edit reopens the question.

### The audit's own caveat, resolved -- and the hedge named the wrong risk

The audit above recorded that `resume_scheduling()` "was traced least
conclusively" and that its balance rested on inline epilogues decrementing the
global symmetrically. Finishing the trace rather than leaving the hedge:

**That path never takes the vector at all.** `save_current` holds the
`mem::take` at `context.rs:3263` -- the only switch-out mutation -- and has
exactly ONE production caller, `retire_after_suspend`, which itself has exactly
one, `schedule_after_suspend`, reached only from `entry.rs`'s `if suspended`
branch. Suspension happens inside `sys::svc`, which the trampoline brackets.

So the adopt precedes every switch-out **by control flow, not by convention** --
which is a materially stronger statement than "every site I checked had one".
The plain leg-return path reaches `pick_next` -> `load_current`, which LOADS and
never takes, followed by `entry.rs:408`'s publish.

⚠️ **The instructive part is that the hedge named the wrong risk.** Whether the
inline epilogues balance is irrelevant on a path that never reads the length. An
honest "I am less sure about X" is worth recording, but it is not the same as
knowing what X is -- and finishing the trace replaced a vague doubt with a fact
that happens to be reassuring for an entirely different reason.

## 2026-08-25 -- `.ecv.funcs` removed: redundant by construction, measured in seconds

Three sessions of this entry proposed expensive futures -- a reachable-subset
artefact, a corrective lifter patch, a keep-list -- and nobody first asked
whether the table carried any information at all.

Decoding it from three fused fixtures and diffing against the `.symtab` the
lifter actually reads:

| fixture | `.ecv.funcs` | symtab FUNC | sizes disagree | ecv-only |
|---|---|---|---|---|
| `python-glibc.fused` | 11,666 | 11,667 | **0** | **0** |
| `nginx-alpine.fused` (musl) | 20,142 | 20,142 | **0** | **0** |
| `ruby-glibc.fused` | 16,740 | 16,741 | **0** | **0** |

Never a superset, never a disagreement. ❗ **And the reason is in the tree's own
history**: `funcRangesOf` fed both tables, unified precisely because they had
once disagreed by 2,531 functions on a fused bash. Fixing that divergence is
what made the second copy redundant -- the entry recorded the fix and never drew
the consequence.

⚠️ **Checked on both libcs**, per this file's own rule that a technique valid on
Debian can be invalid on Alpine. ⚠️ And checked that the comparison was not
vacuous on ifuncs: `funcTable` deliberately kept `STT_GNU_IFUNC` resolvers, and a
`FUNC`-filtered symtab scan would have missed them -- all three images have ZERO
`IFUNC` symbols, so there were none to miss.

### The measurement cost seconds and needed no lift

I had proposed running the undecoded-site census, a ~30-minute job, to answer
this. The census measures EXECUTED undecoded sites, which catches a boundary
over-claim only if the mis-lifted data both decodes badly and gets executed. The
symtab diff answers the question directly. **I was one step from spending half
an hour to get a worse answer**, and the better check was available the whole
time this entry was open.

### Result

Removed `funcTable`, its `addTable` call, `funcs_test.go` and its `BUILD.bazel`
entry. `funcRangesOf` and `symbolsOf` stay -- the merged `.symtab` is built from
them and is now their sole consumer, which their docs now say instead of naming
a section that no longer exists.

Verified on the ARTIFACT: a real OpenSSL fuse emits 7 `.ecv.*` sections and no
`.ecv.funcs`, and the image went **11,160,720 -> 10,828,272 bytes -- 332 KB
reclaimed** from a region already at 89.7% on postgres.
`assertNoRedundantFuncTable` guards it, with a positive control requiring the
other metadata sections to be present, because an absence check passes trivially
on a truncated read. Both arms neutralized.

## 2026-08-25 -- The ⭐ address-budget decision, settled by moving one boundary

The mmap window went **96 -> 160 MiB and the arena did not grow**. Two
constants, `BRK_END_VMA` and `MMAP_START_VMA`, both `0x1000_0000` ->
`0x0C00_0000`.

This entry carried four options for three sessions -- grow the arena, split the
window, make reservations lazy, accept the ceiling -- every one expensive. None
noticed that the starved 96 MiB window has a 96 MiB NEIGHBOUR.

### The measurement, and why it needed building

`Arena.brk_cur` had been tracked and never reported. Three high-water counters
(`diag::note_address_use`, fed from the arena's own mutators so a transient peak
cannot be missed) and one side-built image later, harvested from **104 guest
runs**:

| region | size | max observed | used |
|---|---|---|---|
| brk | 96 MiB | **1,164 KiB** | **1.2%** |
| private mmap | 96 MiB | **90,112 KiB** | **91.7%** |

103 of 104 runs sat at 136 KiB of brk. The plausibility argument became a
number.

### ❗ The datum that looked like caution was the argument for going further

`arena.rs`'s own doc records a real initdb backend with its break ~88 MiB into
brk. That reads as "postgres needs the 96 MiB" -- and postgres is precisely the
guest the 104-run sweep could not measure, so it looked like a reason to stop.

`sys.rs` settled it the other way. `NR_BRK` returns the break UNCHANGED when a
request leaves the region -- Linux's failure convention -- glibc turns that into
ENOMEM, and malloc switches to mmap. The comment even names the case: that
backend "wanted ~190 MiB, which is an ordinary condition with an ordinary
fallback".

**So postgres already overflows brk and already falls back to mmap.** Its demand
could not be met by ANY brk size we could give it, which makes brk size nearly
irrelevant to it and mmap size exactly what it needs. The fact that looked like
a wall was the reason the wall was in the wrong place.

⚠️ Worth generalising: **a number that contradicts your plan deserves the same
scrutiny as one that confirms it.** Had I taken the 88 MiB at face value I would
have shrunk brk to 64 MiB "to be safe" and got a third of the benefit for the
same risk.

### Verification

* `the_arena_layout_is_consistent` asserts `BRK_END_VMA == MMAP_START_VMA`.
  Neutralized by moving one constant alone: fails with "the mmap window must
  start where brk ends, with no gap to lose". A half-change cannot ship.
* E2E **137 pass / 0 fail / 19 skip**, 398 s -- identical to the pre-change run
  and back to the warm baseline. Includes
  `TestSharedFileMappingsDoNotPinTheWindowUnderEcvisor`, which peaked at 88 MiB
  in this window.

### What is NOT settled

This relieves the starvation; it does not make reservations lazy. Ruby's shape
cache still asks for 402,653,184 bytes and still cannot be served -- 160 MiB is
not 384. ❗ **YJIT's 128 MiB now FITS where it never could**, which is worth
re-testing and was not part of this change's claim. Real postgres remains
unmeasured; the instrumentation to do it is now in the tree.

## 2026-08-25 -- YJIT tested on the rebalanced layout: still blocked, and the bar was never 128 MiB

I said the rebalance meant "YJIT's 128 MiB now FITS where it never could".
**Tested, and it does not.** Same `[BUG] mmap failed`, same exit.

The instrument built earlier said why in one line:

```
mmap region exhausted (want 134217728, bump 0xf020000, 0 hole(s),
                       shm_top 0x16000000) -> ENOMEM
address budget -> brk 1728 KiB of 32 MiB | private mmap 49280 KiB
```

| | |
|---|---|
| window | 160 MiB |
| **ruby consumes before YJIT asks** | **48 MiB**, 0 holes, all live |
| contiguous left | 111.9 MiB |
| YJIT wants | 128 MiB |
| short by | **16.1 MiB** |

❗ **The requirement was never 128 MiB -- it is 176.** `TODO.md`'s own table says
"YJIT | 128 MiB into a 96 MiB window", which reads as though 128 were the bar.
Ruby's 48 MiB of startup mappings is not in that number and never was. Both the
entry and my claim inherited the same omission.

The rebalance did move it: short by **80 MiB** before, **16 MiB** now.

### ⚠️ The method failure is the point

The claim came from arithmetic -- 128 < 160, therefore it fits -- on a day spent
writing that this tree refutes by REMOVAL, not by reasoning. The number was
right and the model was wrong, because the model had one term missing.

**A control was what made the test worth anything.** Reproducing the failure on
the OLD module first (`ruby-rbprctl.wasm`, same sidecar, same env) established
that the probe reaches wall 2 at all. Without it, the new module's identical
failure could have been a mis-linked module, a wrong sidecar, or a probe that
never armed YJIT.

### What would clear it

The window needs 176 MiB against ruby's startup, so ~16 MiB more. Available
without growing the arena: brk is using 1,728 KiB of its remaining 32 MiB, so
`MMAP_START_VMA` could drop to `0x0B000000` (brk 16 MiB, window 176 MiB) --
EXACTLY the requirement, with no margin, which is not a number to design to.
❌ Not done: two constants moved on measurement is a rebalance; a third moved to
hit an exact figure with zero headroom is a guess wearing a measurement's
clothes. Whether YJIT is worth that margin is a decision, and wall 1 -- the
undecoded `orr v28.2s, #0x80` that stops argv arming YJIT at all -- is still
there regardless.

## 2026-08-26 -- "Does a memory mapper help?" -- costed, and it splits in two

Asked of the ⭐ address-budget decision. The entry's option 3, "make reservations
lazy", IS a mapper. Investigating it showed the word conflates two changes whose
costs differ by about nine orders of magnitude, and the cheap one was never
listed.

### 1. Full mapper: translate every access

✅ Fixes all four faces. The prize is in the entry's own measurement: ruby's
shape cache is **RSS 28 kB against a 384 MiB mapping, 14,000:1**.

❌ **There is no seam in a shipping build.** `TranslateVMA` exists in
`third_party/elfconv/runtime/Runtime.cpp`, but only under `MEMORY_INSTRUMENT`.
`VmIntrinsics.cpp` says outright that "`__remill_(read | write)_memory_*`
functions are not used for the optimization" and aborts if one is called.
Lifted code emits `arena_ptr + addr` inline, folded into the wasm load/store
offset.

⚠️ `__ecv_wild_store` is the same fact from the other side. `TODO.md` lists it as
a live abort site for "store outside the arena", which reads as though stores
were checked. **It has no caller anywhere** -- not in `patches/`, not in
`third_party/elfconv/`, not in `runtime/`. There is no per-access check to
extend.

❌ **And no cheap partial version.** Virtualising only `PROT_NONE` reservations
and faulting pages in on first touch needs first-touch detection; wasm gives a
guest module no page-fault hook, and a store past the arena traps at the ENGINE,
uncatchable from inside.

### 2. Better placement inside the window

Already exists -- `Arena.mmap_free` reuses and coalesces holes -- and is not the
problem. The YJIT failure reports `0 hole(s)`.

### 3. ❗ Dynamic region boundaries -- no indirection at all

`MMAP_START_VMA` / `BRK_END_VMA` appear NOWHERE outside `runtime/`. Not the
lifter, fuser, linker or image. They are pure ecvisor policy: where it starts
handing out addresses on `mmap`. Everything stays identity-mapped, so no access
changes. The boundary moved as a compile-time constant on 2026-08-25 could be a
RUNTIME value chosen per guest.

⚠️ **`BRK_START_VMA` is the opposite and the distinction is load-bearing.**
`internal/fuse/layout.go:45` uses it as the ceiling for the fused closure layout,
guarded cross-language by `TestBrkStartMatchesTheRuntime`. It is baked into the
image and cannot move at run time. Three constants sit in the same block of
`arena.rs`; two are policy and one is ABI.

### The comparison

**A mapper prices the fix per MEMORY ACCESS. Dynamic boundaries price it per
MMAP.** Same symptom, ~10^9 difference in how often you pay.

Neither is done. Option 3 is still a real design -- who chooses the sizes, and
what happens when the choice is wrong -- but it is the first thing in this
decision with a costed alternative to touching the hot path, and it exists only
because the question was asked in terms of indirection rather than in terms of
the four options already written down.

---

## 2026-08-26 — symbol versioning and weak/strong precedence, priced

Two gaps the dynamic-side-module plan flagged as "silent correctness gaps" and
recommended TODO entries for. Neither had one. Both are now measured rather than
restated, and the measurement changes what each is worth doing about.

`internal/fuse.globalSymbols` keys on the BARE symbol name and takes the first
definition. `fuse.go:463` loads symbols via `f.DynamicSymbols()`, `.gnu.version*`
is never consulted, and `ST_BIND`/`st_other` are never read.

### The probe

`.agents-workspace/drivers/symver` transcribes `globalSymbols`' admission rule
exactly -- defined, named, first-wins, over `DynamicSymbols()`, in command-line
then `.dynsym` order -- and reports only cases where the omission changes which
IMPLEMENTATION gets bound. Run over aarch64 glibc, `libcrypto.so.3` and
`libruby.so.3.4.10`: 10,575 distinct defined names.

| | count |
|---|---|
| names defined more than once | 196 |
| ...at more than one VERSION | 196 |
| ...at more than one ADDRESS | **5** |
| weak-first-beats-later-global | **0** |

**191 of the 196 cost nothing.** They are one implementation wearing two version
labels -- glibc's 2.17/2.34 re-versioning -- so collapsing them is harmless. The
framing "196 collisions" would have been true and useless.

### `glob` is the one that is actually wrong

The five that diverge, with the first-wins choice:

| name | bound | other |
|---|---|---|
| `fmemopen` | 2.22 ✅ | 2.17 |
| `glob` | **2.17 ❌** | 2.27 |
| `glob64` | 2.27 ✅ | 2.17 |
| `pthread_kill` | 2.34 ✅ | 2.17 |
| `quick_exit` | 2.24 ✅ | 2.17 |

❗ `glob` and `glob64` are ALIASES -- the same two addresses -- and `.dynsym`
order gives them DIFFERENT implementations in one fused image. `glob()` gets
glibc's pre-2.27 compat entry, `glob64()` the current one. glibc 2.27 changed
`gl_readdir`'s return layout, so a caller passing `GLOB_ALTDIRFUNC` reads the
wrong struct; without that flag they are believed equivalent, which is why
nothing has noticed. It makes an unusually good regression subject: assert the
two bind the SAME address, which fails today for the right reason.

### ❗ The zero was nearly a lie, and neutralization caught it

The first version of the probe compared the raw `s.Value` and reported **0
weak-before-strong on a witness pair built to contain exactly one**. Both
definitions sat at 0x578: two identically-shaped `.so` files lay out
identically. `globalSymbols` stores `o.addr(s.Value)`, so definitions in
different objects can never share an address. Keyed on (object, value) the
counter reads 1 in weak-first order and 0 in strong-first order.

Without that check, "0 weak/strong cases" would have been reported from a branch
that could not fire -- the exact vacuous-pass shape `AGENTS.md` keeps flagging,
in the one number the probe exists to produce.

⚠️ **What is NOT covered**, since the counts are small enough to read as a clean
bill: three objects, one libc, glibc only. musl has no symbol versioning so it
cannot be affected, but the postgres closure and its 79 extensions -- many
defining the same names, which is exactly where weak/strong would bite -- were
not measured.

**Recommendation, recorded in TODO.md.** Versioning: worth fixing, and the data
is already in hand since `debug/elf` populates `Symbol.Version`. Weak/strong: do
NOT fix speculatively; re-measure over postgres and musl first, and if it is
still zero, record the omission as deliberate and measured rather than adding an
unexercised precedence rule.

### Correction: `internal/fuse/unit.go` said the unit-lift crash was unexplained

Its comment block read "something ELSE also stops a unit lifting. That cause is
not yet known" -- written 2026-08-23, still there 2026-08-26, by which time units
had been lifting for three days. Corrected in place, naming all three causes
(`patches/0066`, the zero-sized `SHN_ABS` `_start`, and a unit's absent entry
function) with a pointer to the LTM document. A stale claim in CODE reads as an
open blocker; this one sat directly above the code that works.

## 2026-08-26 (later) — the same two gaps, measured on REAL closures

The entry above ended with a caveat: three objects, one libc, and neither the
postgres closure nor musl. Both are now measured, and the caveat was worth
paying off -- the numbers moved in both directions.

### Method, and a wrong number worth recording

`.agents-workspace/drivers/closure.sh` resolves a transitive `DT_NEEDED` closure
inside an exported rootfs. ❗ **This matters more than it looks.** The first
postgres run fed the probe every `.so` in the lib directory -- 218 objects -- and
reported **104,978** address-divergent names out of 106,769. That is not a
finding, it is a broken input set: it loads mutually-exclusive alternatives (two
ICU majors, LLVM, unrelated libraries) that no fuse would ever see together. The
real closure is **89 objects**: the `postgres` binary's transitive needs, plus
all 79 extensions and each of their own closures.

⚠️ A probe fed an over-approximated closure reports a catastrophe and looks
rigorous doing it. The realistic set gives 99 raw, 67 real.

### Categorising, because the raw count overstates by ~1.5x

The 99 raw divergences on the postgres closure break down as:

| kind | count | is it a defect? |
|---|---|---|
| `SHN_ABS` version nodes (`GLIBC_2.17`, `OPENSSL_3.0.0`) | 21 | no -- not implementations |
| C++ vague linkage (`_Z…` typeinfo, templates) | 8 | no -- collapsing them IS the ODR |
| linker bounds (`__bss_start`, `_edata`, `_end`) | 3 | no -- per-object by nature |
| **real** | **67** | yes |

⚠️ The version nodes are genuine `.dynsym` entries and `globalSymbols` ADMITS
them -- it filters only `SHN_UNDEF`, not `SHN_ABS`. Harmless, but they sit in
the fuser's symbol map and they inflate any naive count.

### What the 67 actually are

**64 libc/libm, 3 postgres.** And 32 of the 64 bind `GLIBC_2.17`, the OLDEST
compat implementation: `exp`, `expf`, `exp10`, `fmod`, `hypot`, `log2f`, `modf`,
`frexp`, `ldexp`, `scalbn`, `copysign`, `finite`, `glob`. A guest compiled
against modern headers expects the current one and gets glibc's SVID-era
wrapper.

⚠️ **Do not upgrade this to "wrong math".** The pairs agree on ordinary inputs
and differ at edge cases -- overflow/underflow errno, `matherr`. Real, low
severity, and now written down with its severity attached rather than as a
number that invites panic.

The 3 postgres ones are the defect the per-unit path exists for, and it is
already fixed: `Pg_magic_func` has **79** definitions, `_PG_init` **15**,
`_PG_output_plugin_init` **2**. Flat, first-wins binds `amcheck.so`'s magic and
`auth_delay.so`'s init for all of them.

❗ **CORRECTION to TODO.md's framing of that defect.** It says every extension
defines "`Pg_magic_func`, `_PG_init` and `pg_finfo_*`". The first two collide;
`pg_finfo_*` **does not and never did** -- those names embed the SQL function
name (`pg_finfo_hstore_in`), so they are unique per extension. Exactly three
names collide, not a class of them.

### musl: clean, and structurally so

nginx:alpine base closure (3 objects, 7,498 names): **0** versioned, **0** real
divergences, **0** weak/strong. musl has no symbol versioning at all.

### ❗ A NEW instance of "discovery finds more than you planted"

nginx:alpine ships **debug AND release variants of all 7 modules** --
`ngx_http_js_module.so` and `ngx_http_js_module-debug.so`, and so on. Fusing all
14 as flat `Options.Extra` plugins produces **940 real divergences**, because
each pair defines an identical symbol set and the JS ones statically embed
QuickJS four times over. First-wins would silently mix debug and release code in
one image.

✅ Not a new defect -- the per-unit path is the default for `raptormark build`
and gives each variant its own `.ecv.dlsyms`. But it is a second measured
instance of the hazard `AGENTS.md` records for the OpenSSL case, and a sharper
one: there the surprise was the COUNT, here it is that the extras are
near-duplicates of each other.

### Weak/strong precedence: settled, at zero

| closure | objects | names | weak-first |
|---|---|---|---|
| glibc + libcrypto + libruby | 3 | 10,575 | 0 |
| postgres:17 real closure + 79 extensions | 89 | 76,458 | 0 |
| nginx:alpine (musl) base | 3 | 7,498 | 0 |
| nginx:alpine + 14 module variants | 18 | 19,287 | 0 |

Four closures, two libcs, ~113,000 name-definitions, zero witnesses, with the
detector proven live by a witness pair. **Recommendation recorded in TODO.md: do
NOT implement precedence.** An untriggered precedence rule is code no test can
distinguish from its opposite.

⚠️ Independent of precedence and still open: `globalSymbols` and `UnitExports`
disagree about `STB_LOCAL` -- the latter filters it, the former does not.

## 2026-08-26 (later still) — a near-miss: the `STB_LOCAL` "inconsistency" was correct

The two entries above each closed noting that `globalSymbols` and `UnitExports`
disagree about `STB_LOCAL` -- the latter filters it, the former does not -- and
called it a real inconsistency worth fixing in whichever change next touched
either function.

❌ **It is not a defect, and acting on it would have caused one.**

They read DIFFERENT TABLES, and each is right for the table it reads:

| | table | named defined `STB_LOCAL` entries |
|---|---|---|
| `globalSymbols` | `.dynsym` (`f.DynamicSymbols()`) | **0** across all 89 postgres-closure objects |
| `UnitExports` | the synthesized `.symtab` (`f.Symbols()`) | **2,110** in `bash-glibc.fused` |

`.dynsym`'s only defined locals are section symbols. Their `st_name` is 0, so
`debug/elf` reports `Name == ""` and `globalSymbols`' existing `s.Name == ""`
check already drops them -- verified by reading every one of the 89 objects and
finding zero named cases. Adding a bind filter there would be dead code.

The synthesized `.symtab` is the opposite: `emit` deliberately writes named
`STB_LOCAL` `_ecv_fde_<addr>` symbols for function boundaries recovered from
`.eh_frame` (`fuse.go:1213`). Without the filter, `UnitExports` would report
2,110 boundary symbols as exports of the unit.

### What actually went wrong, twice

`UnitExports`' own doc comment said it "Mirrors `globalSymbols`' admission rule
exactly -- defined, named, and first-wins". That is false: it adds a bind
filter. The comment is what made the divergence read as drift both times it was
written down. Corrected in place, with the measurement and an explicit "do not
reconcile this".

❗ **The rule that caught it is the one already in `AGENTS.md`**: "These
constants appear twice is not sufficient reason to tie them. What matters is
whether anything BEHAVES differently." Two sites looking inconsistent is a
prompt to measure, not a defect. Had this been "fixed" for symmetry, the
outcome was either dead code in `globalSymbols` or a `UnitExports` that leaks
every FDE boundary symbol -- and the second would have been silent.

⚠️ Filed as a near-miss rather than a no-op because it was recorded as an open
defect twice in one day before anyone checked it. A note repeated is not a note
verified.

## 2026-08-26 (final) — the versioning defect is LIVE, and it is three symbols

The entry above priced the versioning gap at 67 real divergences on the
postgres:17 closure, 32 of them binding `GLIBC_2.17`. That was still one step
short of the number that decides anything.

**A wrong binding costs nothing unless something references the name across an
object boundary at a DIFFERENT version.** Cross-checking every `UND` reference
in the closure against what first-wins binds narrows 32 to **3**:

| symbol | referenced as | bound to | by |
|---|---|---|---|
| `exp` | `GLIBC_2.29` | **`GLIBC_2.17`** | `postgres`, `libLLVM`, `libz3` |
| `fmod` | `GLIBC_2.38` | **`GLIBC_2.17`** | `postgres`, `libLLVM` |
| `log2f` | `GLIBC_2.27` | **`GLIBC_2.17`** | `libLLVM` |

❗ `postgres` itself imports `exp@GLIBC_2.29` and `fmod@GLIBC_2.38`, and the
fused image hands it glibc's SVID-era compat wrapper. **This is live in the
shipping pipeline.** It had been recorded as a theoretical gap for as long as
the note existed.

The remaining 29 that bind 2.17 have no cross-object reference wanting a newer
version. `glob` is one of them -- latent in this closure, and still the best
test subject, because `glob` and `glob64` are aliases that first-wins gives
different implementations.

### Severity did not move, and that is deliberate

All three are libm pairs where `@GLIBC_2.17` is the SVID wrapper
(`_LIB_VERSION`, `matherr`) around the same kernel. They agree on ordinary
inputs and differ at edge cases -- overflow/underflow errno. ⚠️ What is provably
wrong is WHICH implementation runs, not the value it returns for normal
arguments. Recording it as "postgres computes exp() wrong" would be false and
would buy an urgent-looking bug report at the cost of being believed next time.

### The narrowing, as a chain

Worth keeping because every step removed roughly an order of magnitude, and
stopping at any of them would have produced a defensible-sounding wrong number:

| step | count |
|---|---|
| every `.so` in the lib directory (WRONG input set) | 104,978 |
| real `DT_NEEDED` closure, raw address divergence | 99 |
| minus `SHN_ABS` version nodes, C++ vague, linker bounds | 67 |
| of those, binding the OLDEST implementation | 32 |
| of those, actually referenced at a newer version | **3** |

Each row is a number someone could have reported. Only the last one is a defect.

## 2026-08-26 (fix) — a default version now outranks a compat one

The three entries above measured the versioning gap down to three actively
mis-bound symbols. This is the fix.

### The rule

`globalSymbols` stays first-wins, with ONE exception: a held definition that
came from a NON-DEFAULT (hidden) version is displaced by a later default one.
Versions are still discarded as keys -- `exp@GLIBC_2.17` and `exp@GLIBC_2.29`
remain one entry, because binding is eager and there is no runtime linker to
consult a version table. All that changed is refusing to PREFER the compat body.

❗ This reads the ELF rule exactly rather than guessing from version strings. A
version index carries a hidden bit; the definition without it is the default --
`readelf`'s `foo@@VER` against a compat `foo@VER`. `debug/elf` exposes it as
`VersionIndex.IsHidden()`, which Go 1.26 has and this tree is on.

⚠️ Unversioned symbols count as DEFAULT. That is what bounds the change.

### Measured effect, on the real closure rather than asserted

| closure | real divergences | RE-BOUND |
|---|---|---|
| postgres:17 (89 objects) | 67 | **18** |
| nginx:alpine + 14 modules (musl) | 940 | **0** |
| postgres's 79 extensions alone | 3 | **0** |

All 18 are compat -> default moves inside libc/libm. All three actively-wrong
symbols are fixed (`exp`, `fmod`, `log2f`), `glob` is fixed, and 10 of the 18
are the `totalorder*` family -- a real glibc 2.31 signature change, not a
re-versioning.

✅ **The two zeros are the important half.** musl has no symbol versioning and
neither do the plugins, so the rule degenerates to the old first-wins there. The
per-unit plugin path -- where `Pg_magic_func` has 79 definitions -- is provably
untouched by a libc fix. That was the risk worth checking, and it was checked on
data rather than argued from the code.

### The ifunc hazard, which is the one way this could have gone wrong silently

An earlier COMPAT definition may be an `STT_GNU_IFUNC` while the default is not.
If the ifunc mark survived the displacement, the runtime would treat a plain
implementation address as a RESOLVER -- calling it returns a function pointer
and does nothing, the silent no-op `sttGNUIFunc`'s own comment warns about. The
fix clears the mark, and the reverse case (default IS the ifunc) keeps it. Both
have tests.

### Neutralization, twice

`internal/fuse/symver_test.go`, six tests, all transcribed from `readelf` output
rather than derived from a run.

1. **Revert to plain first-wins** -- five fail with their intended diagnostics
   (`glob` and `glob64` resolving differently; `exp`/`log2f`/`fmod` binding the
   SVID body). ❗ Both CONTROLS still pass, which is what shows they are not
   just restating the subject: unversioned first-wins, and interposition
   between two defaults.
2. **Remove only the `delete(ifuncs, ...)`** -- exactly the ifunc test fails,
   with its own message. The first neutralization could not prove that branch,
   because the address assertion short-circuited ahead of it.

### ⚠️ Cost

This changes the fused ELF, so **every cached translated object for a glibc
image is invalidated**. musl images are byte-identical and their objects
survive. No E2E has been run against it; the gates run were Go (both module
patterns) and Bazel 14/14 -- `//internal/fuse:fuse_test` picks the new file up
through its `BUILD.bazel` srcs entry.

## 2026-08-26 — E2E for the versioning fix, and a green run that was worth less

`raptormark-builder:rebal`, warm object cache: **137 pass / 0 fail / 19 skip in
396 s**. Identical in both counts to the last recorded reading (407 s,
2026-08-25). The fuse change re-binds 18 symbols in every glibc image and
nothing moved.

The tests that most exercise the changed path all passed on freshly
re-translated glibc fixtures: `TestPostgresStyleDlopenResolvesPerUnit`,
`TestHostedLoaderServesADlopenMidRun`, `TestFusedDynamicProgram`,
`TestMinsigstacksizeIsLocatedInARealGlibc`, and `TestNginxAlpineFuseHandlesMusl`
for the untouched musl side.

### ❗ The first attempt was green and should not have been reported

It read **121 pass / 0 fail / 35 skip in 2460 s** -- and it was run without
`RAPTORMARK_NODE` and without `RAPTORMARK_E2E_WASMER=1`, so the node host suite
and the wasmer suite never executed. Sixteen tests silently absent, zero
failures, exit 0.

`QUALITY_GATE.md` §5 already carries the rule that catches this -- "a pass total
can go UP while coverage goes down", check the skip count against the breakdown
-- and 19 is the healthy number on this machine. 35 means two suites are
missing. Re-run with the full env; the counts then matched the series exactly.

⚠️ Worth keeping because the failure had no symptom. The run took 41 minutes and
ended in `ok`. Nothing about it looked partial except a number in a column
nobody has to read.

### An attribution I got wrong, and the correction

Comparing 137 against the **134 / 19** row, the +3 looked attributable to the
three net tests added this session. It was not a delta at all: **137 / 0 / 19
was already recorded** on 2026-08-25 after the net-guest rewrite, with those
same three named. The comparison was against a stale row two entries up. The
real result is "identical to baseline", which is the stronger claim anyway.

### ⚠️ What the green does NOT cover

Nothing in the suite calls `exp`, `fmod`, `glob` or `totalorder*` through a
fused glibc, and the two heaviest glibc closures
(`TestOpenSSLFixtureEndToEnd`, `TestSharedNamesReuseAcrossAClosure`) are
`RAPTORMARK_E2E_SLOW`-gated and skipped.

So the suite establishes that the fix BREAKS nothing across 137 tests including
a full cold re-translation. It does NOT establish that the 18 re-bound symbols
resolve to the right bodies at run time. That claim rests on
`internal/fuse/symver_test.go` (six tests, neutralized twice) and on
`.agents-workspace/drivers/symver` measuring the real 89-object closure. An
`RAPTORMARK_E2E_SLOW=1` run would narrow the gap; a guest that actually calls
`exp()` and checks the result would close it.

## `preserve check` was reporting a clean all-clear over four unguarded things, 2026-08-26

Picked up the `patches/0067` TODO entry (recorded 2026-08-24 as UNTRACKED) and
found it **already closed**: `git ls-files patches/` lists it, `git log`
attributes it to `bcad8a3`, and nothing under `patches/` is untracked. The user
committed it. Entry marked closed with the failure shape kept, because
`patches/*.patch` is a GLOB and a missing file still subtracts silently.

That led to the manifest that is supposed to catch exactly this class of loss.

### The finding

`raptormark preserve check` printed **`ok: all 5 recorded entries are present`**
while none of these were recorded at all:

| unguarded | why it matters |
|---|---|
| `raptormark-elfconv-base:8bfe80860118` | the CLEAN PIN every patched base is built from |
| `raptormark-builder:rebal` | the builder every current command names |
| `raptormark-builder:fsync` | the last pre-wipe builder, genuinely unrebuildable |
| `.agents-workspace/{wasmer,multimodule-poc}` | probe harnesses cited BY PATH from `WASIX_ABI.md`, `LTM/wasix-and-wasmer.md`, `MULTIMODULE.md` |

❗ **The sharpest one is the pin, and the reason is not "it is irreplaceable".**
It is rebuildable -- `third_party/elfconv` is pinned clean and its tag is
literally the submodule HEAD (verified: both `8bfe80860118`). The cost is that a
rebuild yields a **different image id**, so `BASE_ID` changes and all 7.7 GB of
`.agents-workspace/objcache` misses on every entry.

`objcache` **was** in the manifest. The identity it is keyed on was not. So
losing the pin does not delete the cache -- it makes every entry in it
permanently dead, while `check` goes on reporting the cache present. A guard
that watches the asset but not the thing the asset's validity depends on is the
`_recovery/` shape one level up: every reference to the pin in this tree
(`buildimage.go`, `hermetic_differential.sh`) CONSUMES it; none requires it.

### What was done

Six `preserve snapshot --add` entries (5 → 10), plus a note correction:
`lbport` still described itself as "Newest builder (2026-08-25)", false since
`rebal` was built on 2026-08-26. Corrected rather than left, because a manifest
whose notes drift gets read as a directory of what to use.

⚠️ **`rebal` and `lbport` are recorded with notes that say they are
REPRODUCIBLE**, because they are -- both layer onto the recorded patched base.
Overstating an entry's stakes is how the notes stop being believed.

### Neutralized, both arms, and not by compile error

Against a copy of the manifest in an empty root: 5 path entries report
`MISSING`, `rc=1`, both new paths named. Against a hand-edited copy with one
image renamed absent and one recorded id zeroed: `MISSING image` (`rc=1`) and
`CHANGED image` printed with recorded vs live ids. The real manifest then
re-checked `ok: all 10`, with all five image ids verified equal to live.

The path arm's neutralization is also the demonstration that image entries are
root-independent: they stayed present in the empty root, correctly, because
Docker is global.

## `raptormark build postgres:17` has already lost its shared library layout, 2026-08-26

Continuing the TODO sweep. Audited every code citation in `TODO.md` first --
78 file paths, 116 symbols, 30 `file:line` references -- and it is CLEAN: the
three files that do not exist (`internal/fuse/funcs_test.go`,
`internal/translate/runall.go`, `instance.rs`) are cited as deleted or belong to
the upstream containerd shim, every unresolved symbol is guest-side or external,
and every line number is in bounds. Spot-checked the ⭐ lifter target: both
`TryDecodeORR_ASIMDIMM_L_HL`/`_L_SL` are at exactly the cited `Decode.cpp:17329`
and `:17367`, both bare `return false`. **TODO.md's citations do not drift the
way LTM's did.**

Then re-verified the one entry that states a number with no guard behind it.

### The finding

`.agents/docs/TODO.md` records postgres + initdb + 78 extensions planning SHARED
at **89.7%** of the 156 MiB fused region, "a pass with ~16 MiB to spare". It does
not pass. Measured with a new driver, `.agents-workspace/drivers/headroom`:

| run | programs | libs | plugins | top | result |
|---|---|---|---|---|---|
| `-max 2` | 2 | 36 | 81 | `0x9020010` | 89.8%, 15.9 MiB free |
| full, `-plugins=none` | 71 | 49 | 0 | `0x9a41640` | 96.3%, 5.7 MiB free |
| **full (what `build` does)** | 71 | 51 | 81 | needs `0xb020010` | ❌ **OVER by 16.1 MiB** |

The recorded figure reproduces **exactly** -- at two programs. `raptormark build`
fuses the whole closure, which is 71.

❗ **So nothing regressed. The number was never describing a build.** That is the
more useful conclusion: a headroom percentage is meaningless without the program
count it was taken at, and this one had been reporting a pass for a
configuration the pipeline does not use.

### Cause, fully accounted for

`shared min` is `0xe00000` in all three runs, so `exeTop` never moves --
postgres is the largest executable and is present in every run. The 69
non-entry programs contribute **15 additional distinct libraries**; at `libAlign`
2 MiB that is ~32 MiB, and the two tops differ by **32.0 MiB** exactly.

⚠️ **It is not the plugins.** With discovery off the library band alone is at
96.3% -- room for about two more libraries.

### Why no test caught it, and why both reasons are defensible

`e2e/pipeline_test.go` holds the only `SharedLayout` assertion. It runs
`pgExtFixture` (debian + 2 extensions), not `postgres:17`, because a real
postgres build is hours; and it only `t.Logf`s the layout status, because an
overflow IS a legitimate degradation and failing on it would make a large image
unbuildable in exchange for an optimization. Each choice is right on its own.
Together they mean the flagship closure's cost regression is invisible -- the
`_recovery/` shape again, where every reference tolerates absence.

### The driver

`headroom` mirrors `pipeline.build` stages 1-2 and stops before translation, so
it costs an image export and seconds of Go rather than a lift. Controls: it
refuses to report on an EMPTY closure (a percentage over nothing is a clean
wrong all-clear), it refuses a returned layout whose top exceeds its duplicated
copy of `brkStartVMA` (the planner cannot accept one, so a report is evidence
the copy still matches), and `-plugins=none` must move the number -- it does,
96.3% against overflow.

⚠️ `-max` exists ONLY to reproduce a narrower recorded run. It is not a build
knob; `raptormark build` defaults to 10000, i.e. everything.

### Not taken

The two remaining fixes are both expensive and both decisions: growing the region
moves `brkStartVMA`, which is guest ABI duplicated into `runtime/src/arena.rs`;
shrinking `libAlign` moves every library base and re-lifts every cached object.
Neither belongs inside a sweep. What is no longer in question is whether the
cliff matters yet.

## The headroom survey, and a library search list that was a subset of the images', 2026-08-26

Follow-on from the postgres headroom finding. Two results and one stale citation.

### 1. Nine images surveyed: only postgres is anywhere near the ceiling

`raptormark build` fuses everything into `[0x400000, 0xa000000)` = 156 MiB.
Measured with `.agents-workspace/drivers/headroom` (plan only, no translation):

| image | programs | libs | plugins | used | headroom |
|---|---|---|---|---|---|
| **postgres:18** | 71 | 53 | 83 | ❌ needs `0xb690010` | **over by 22.6 MiB** |
| **postgres:17** | 71 | 51 | 81 | ❌ needs `0xb020010` | **over by 16.1 MiB** |
| node:22-slim | 3 | 7 | 0 | 73.2% | 41.7 MiB |
| php:8.3-cli | 6 | 42 | 3 | 69.8% | 47.1 MiB |
| python:3-slim | 1 | 19 | 80 | 43.4% | 88.4 MiB |
| ruby:3-slim | 2 | 10 | 3 | 21.3% | 122.7 MiB |
| nginx:latest | 6 | 10 | 3 | 18.5% | 127.1 MiB |
| redis:7-alpine | 3 | 4 | 5 | 11.4% | 138.2 MiB |
| nginx:alpine | 3 | 5 | 5 | 11.0% | 138.8 MiB |
| debian:trixie-slim | 1 | 6 | 3 | 12.1% | 137.1 MiB |

❗ **This is one workload, not a fleet-wide squeeze.** postgres:18 wants 179 MiB;
node, the runner-up, uses 114 MiB, and everything else fits in a quarter of the
region. A region sized for postgres (>= 179 MiB) leaves every other image with
more slack than it has now. That reframes the open decision: it is not "how much
does the fleet need", it is "do we size for postgres".

### 2. python:3-slim and ruby:3-slim could not be fused AT ALL

Two of the nine did not overflow -- they failed to resolve:

```
fuse: cannot find libpython3.14.so.1.0 in [.../lib .../usr/lib
      .../lib/aarch64-linux-gnu .../usr/lib/aarch64-linux-gnu]
```

`pipeline.libraryPaths` listed four directories. `cat /etc/ld.so.conf.d/*.conf`
inside `python:3-slim` names four, and **two were missing**: `libc.conf` is
exactly `/usr/local/lib`, and `aarch64-linux-gnu.conf` adds
`/usr/local/lib/aarch64-linux-gnu`. Both images keep their PRINCIPAL library
there. Both are in README's image survey.

❗ **The list looked complete and carried a comment arguing it was.** "not a
guess: a Debian rootfs is usr-merged..." -- true, and about the four that were
present. The two that were absent were never argued about. This was found by
running nine images through the planner, not by reading the function.

✅ **Fixed by APPENDING**, which is the whole of why it was safe to ship without
re-fusing anything: `fuse.findLib` takes the first match over an ordered list, so
an entry at the end cannot move a name that already resolved -- only one that
resolved nowhere. Putting `/usr/local` first would match Debian's ld.so.conf
order and silently re-point every name present in both places.

✅ Guarded by `TestLibraryPathsCoverWhatTheImageItselfDeclares`, wanted set
transcribed from the rootfs rather than read back from the function. Both arms
neutralized, neither by compile error: dropping the entries fires the
missing-directory arm naming both; moving them to the front fires the ordering
arm naming the indices. Verified by effect too -- both images now plan.

⚠️ Still hardcoded. Nothing reads the rootfs's own `/etc/ld.so.conf` and the
fuser honours neither `DT_RUNPATH` nor `DT_RPATH`. Six more copies of the
four-path list sit inline in `e2e/` and were deliberately left: their fixtures
keep nothing in `/usr/local`, so nothing behaves differently.

### 3. A blind spot in yesterday's citation audit: IMAGES

The TODO.md audit checked file paths, symbols and line numbers. It did not check
image tags, and there is a stale one: the entry re-verified 2026-08-22 states
"The images exist on this machine" and lists four `llvm22` tags. **All four are
absent** -- the 2026-08-23 mass removal took the whole line, one day after the
note was written. The `--llvm 22` flag and the README section are unchanged, so
the LINE is still first-class in the code; nothing on this machine can run it.

Auditing every `raptormark-*` tag cited across `AGENTS.md`, `README.md` and
`.agents/docs/**` turns up 17 absent names. Most are correct: historical records
of which builder a measurement used, two documented as deliberately deleted, and
`:latest`/`:foo` cited as hazards or examples. The `llvm22` group is the only one
asserting present-tense existence.

## A plan is not a build: `raptormark build python:3-slim` still fails, 2026-08-26

Added a `-fuse` mode to `.agents-workspace/drivers/headroom` -- after planning,
actually fuse every program the way `pipeline.build` does (entry via
`FuseWithUnits` with its units, others via `Fuse` without `Extra`). It found a
defect on its first run, which is the point of writing it: the `/usr/local/lib`
fix had been verified only as far as "the layout planned", and I was about to
report that as "the image builds". Those are different claims.

  ruby:3-slim     ✅ fuses -- 2 programs, 13,266,624 bytes
  python:3-slim   ❌ fails

```
fuse: cannot satisfy dlopen'd plugin .../_tkinter.cpython-314-aarch64-linux-gnu.so:
fuse: cannot find libtk8.6.so in [...]
```

### The module is broken in the source image

Verified rather than inferred: no `libtk*` or `libtcl*` anywhere in the rootfs,
and `import tkinter` in the stock container raises the identical
`libtk8.6.so: cannot open shared object file`. Debian ships the extension
without its dependency. raptormark refuses to build the whole image over a
module the guest could never have loaded.

### The handling exists, and the production path bypasses it

`fuse.load` ALREADY skips an unsatisfiable plugin into `SkippedExtra`, and that
type's doc names `_tkinter` as "the case that forced this". Then:

| entry point | unsatisfiable plugin |
|---|---|
| `fuse.FuseClosure` | degrades, reports via `Report.SkippedExtras` |
| `fuse.Fuse` | **fatal error** |
| `fuse.FuseWithUnits` | **fatal error** |
| `internal/pipeline.build` | calls the bottom two, **never `FuseClosure`** |

❗ So `internal/image/plugins.go` carried a ⚠️ note saying `FuseClosure` reports
these "through `Report.SkippedExtras`, which is a different and equally visible
list". True about `FuseClosure`; false as protection for a build. **Third
lying comment this session** -- after `_ecv_save_call_history` describing its
`_ic` sibling's bracketing, and `UnitExports` claiming to mirror
`globalSymbols`' admission rule. All three read as reassurance at exactly the
point an auditor stops. Corrected in place.

### Not fixed, because the placement is a policy decision

The asymmetry `Fuse` documents is RIGHT for an explicit `Options.Extra`:
somebody named that plugin, and a silently absent dlopen'd module is the failure
`Extra` exists to prevent. Under `--plugins auto` nobody named it -- discovery
walked a directory. That distinction is what a fix turns on, and the two
placements (exclude at discovery, which needs `image.Plugins` to resolve
`DT_NEEDED`; or thread `SkippedExtra` out of the two fusers) have different
costs. Recorded in TODO.md with both.

⚠️ **And the guard must distinguish "the image does not contain this dependency"
from "our search list is too narrow."** The second is precisely what the
`/usr/local/lib` defect was, hours earlier. A discovery-time exclusion built
without that distinction would have silently dropped `libpython3.14.so.1.0`'s
dependents and reported a clean build.

## `raptormark build <image>` failed at discovery for almost every real image, 2026-08-26

Found by giving `.agents-workspace/drivers/headroom` a `-fuse` mode and then
noticing what it did NOT print.

### The tell was an absence

postgres fused all 71 programs and **zero units**, silently. The
entry-carries-units branch is `guest == entry`; an entry that is not in the
closure simply never matches, and "no units" is indistinguishable from "this
image has none". The driver was missing the containment check `pipeline.build`
has. Adding it produced:

```
the entry /usr/local/bin/docker-entrypoint.sh is not in the closure (71 programs)
```

Then against the REAL command, which is what settles it:

```
$ raptormark build nginx:alpine --out … --builder raptormark-builder:rebal
discovery: exporting nginx:alpine ...
build <image>: the entry /docker-entrypoint.sh is not in the closure
(3 programs). …                                                    [exit 1]
```

### Two halves of discovery disagreeing, each right on its own

`image.EntrypointSeeds` deliberately resolves SCRIPTS as well as programs --
`resolveProgram` returns `inv.Scripts[p]` -- because `image.Closure` seeds FROM a
script, scanning it for the bare words that name real programs. So a script seed
is CORRECT, and a script is by construction never a MEMBER of the closure.
`pipeline.build` took `seeds[0]` and required membership.

Seeds are ENTRYPOINT then CMD, so the fix is "the first seed that is in the
closure" -- literally "the program the entrypoint script runs". Measured over
seven images, `seeds[1]` is the intended program in six:

| image | seeds[0] | seeds[1] |
|---|---|---|
| nginx:alpine, nginx:latest | `docker-entrypoint.sh` ❌ | `/usr/sbin/nginx` ✅ |
| postgres:17 | `docker-entrypoint.sh` ❌ | `…/bin/postgres` ✅ |
| redis:7-alpine | `docker-entrypoint.sh` ❌ | `…/redis-server` ✅ |
| node:22-slim | `docker-entrypoint.sh` ❌ | `…/node` ✅ |
| php:8.3-cli | `docker-php-entrypoint` ❌ | `…/php` ✅ |
| ruby:3-slim | `…/irb` ❌ | *(none)* |

### Why the suite never saw it

`e2e/pipeline_test.go` builds `pgExtFixture`, whose Dockerfile is
`CMD ["/usr/bin/pgdlhost"]` with **no ENTRYPOINT** -- the one shape where
`seeds[0]` is a program. Every real image in the survey has a script ENTRYPOINT.
A fixture chosen to be small and controllable was also, accidentally, chosen to
avoid the defect.

### The fix, and what it refuses

`entryFromSeeds` extracted from `build` so the rule is testable without an image
or builder, the way `suspendViaCallFor` already is.

✅ **Strictly widening**: when `seeds[0]` is in the closure it is still chosen
(first match), so nothing that built before builds differently -- only inputs
that hard-failed can move.

✅ **Still refuses ruby:3-slim**, whose only seed is the `irb` SCRIPT. Falling
through to `closure[0]` would make every image "work" and is exactly the guess
`build.go`'s own comment rejects: right most of the time, which is the worst
kind. The refusal names `--entry`.

Both arms neutralized, neither by compile error: reverting to `seeds[0]` fails
the four script cases while the two program-CMD controls keep passing; falling
through to `closure[0]` fires the refusal test.

✅ **Verified by effect**: `raptormark build nginx:alpine` now reports
`entry /usr/sbin/nginx`, `5 plugin(s)`, `fuse: 8 image(s)` -- 3 programs and 5
units -- and proceeds into translation.

❗ **The units are the part worth keeping.** While the entry sat outside the
closure, EVERY image's plugin units were silently skipped too. The defect
presented as a hard failure at discovery; had discovery been more lenient it
would have presented as builds that quietly contained no dlopen-able modules.

### Also measured, not fixed

`node:22-slim` fuses 2 of 3 programs and then fails on `/usr/local/bin/node`
with **29 unhandled `R_AARCH64_COPY` relocations** (`fuse: … need a policy
decision`). Copy relocations appear NOWHERE in TODO.md, LTM or
`internal/fuse/*.go` outside the generic `UnsupportedError`, so this is a new
gap, not a known one. Recorded rather than taken: handling them means copying a
library's initialised data into the executable's `.bss` slot AND re-pointing the
library's own references at that copy, which is a fuser design decision.

### The corrected survey: units materialize everywhere, 2026-08-26

Re-ran `-fuse` after the entry fix, with the driver now calling
`pipeline.EntryFromSeeds` rather than a copy (exported for the same reason
`BuildForTest` is -- every second copy in this area has already been wrong once).

| image | entry chosen | programs fused | units |
|---|---|---|---|
| nginx:alpine | `/usr/sbin/nginx` | ✅ 3/3 | **+5**, 145,168 B |
| redis:7-alpine | `/usr/local/bin/redis-server` | ✅ 3/3 | **+5**, 142,944 B |
| nginx:latest | `/usr/sbin/nginx` | ✅ 6/6 | **+3**, 165,136 B |
| php:8.3-cli | `/usr/local/bin/php` | ✅ 6/6 | **+3**, 165,136 B |
| debian:trixie-slim | `/usr/bin/bash` | ✅ 1/1 | +3 (unchanged -- it always worked) |
| python:3-slim | `/usr/local/bin/python3.14` | ❌ `_tkinter` | -- |
| node:22-slim | `/usr/local/bin/node` | ❌ 29 `R_AARCH64_COPY` | -- |
| ruby:3-slim | -- | ❌ no program seed | -- |

❗ **Every `+N unit(s)` in that column is new.** Before the fix only
`debian:trixie-slim` produced units, because it is the only surveyed image whose
CMD is a bare program. The entry defect was silently disabling the entire
per-unit plugin path -- the thing Phase 1-4 of the side-module work was built to
provide -- on every image with a `docker-entrypoint.sh`.

⚠️ **My own earlier totals in this JOURNAL were taken without units** and
therefore understate. Corrected above rather than edited in place.

### Where each image now stands

Three distinct blockers, none of them the same bug:

  python:3-slim  a plugin whose dependency the IMAGE lacks (`libtk8.6.so`)
  node:22-slim   a relocation class the FUSER does not handle (COPY)
  ruby:3-slim    an image that names no program seed at all (CMD `irb`)
  postgres:17/18 fuses, but over the region ceiling, so no shared layout

⚠️ Worth stating plainly: "fuses" is not "runs". None of this touches
translation, linking, or the runtime -- it is the last host-side step, and it is
the cheapest place to find these. The e2e suite remains the authority for whether
anything executes.

### postgres:17 with the corrected entry

```
programs   : 71, entry /usr/lib/postgresql/17/bin/postgres
plugins    : 81 discovered, 1 excluded
❌ NO SHARED LAYOUT -- needs 0xb020010, region ends 0xa000000
  /usr/lib/postgresql/17/bin/postgres   68,989,824 bytes + 81 unit(s), 4,278,512 bytes
  TOTAL                                410,838,064 bytes across 71 program(s)
```

✅ **It fuses COMPLETELY, all 71 programs and all 81 units.** So postgres's
remaining blocker at this stage is purely the region overflow, which is a COST
(every program packs independently, no cross-program library sharing) and not a
correctness failure. That is a meaningfully better position than the other three
blockers, and worth separating from them when the region decision is taken.

⚠️ The 81 units are 4.28 MB against a 69 MB entry image -- 6%. The per-unit path
is cheap here; what is expensive is the 71 unshared programs at 411 MB total.

## Shell scripts are parsed now, and the closure was missing programs the entrypoint runs, 2026-08-26

`internal/image/shellscan.go`, using `mvdan.cc/sh/v3/syntax` -- the tree's second
third-party Go dependency after kong.

### What was broken

`bareWords` was the only mechanism a script had. The other extractor,
`absolutePaths`, requires a NUL terminator because it was written for a binary's
string table -- and a shell script is text, so **no literal absolute path in a
script was ever followed.**

`nginx:alpine` reaches its four `/docker-entrypoint.d/` scripts only through
`$f`, whose value comes from `find`. Nothing names them literally. The one
literal available is the DIRECTORY, and it appears as an ARGUMENT to `find`, not
in command position.

### The result, measured

| image | before | after |
|---|---|---|
| nginx:latest | 6 | **29** |
| nginx:alpine | 3 | **6** |
| redis:7-alpine, php:8.3-cli, debian:trixie-slim, postgres:17 | — | unchanged |

❗ **The additions are real, and I checked rather than assumed.** `grep` over
nginx:latest's `/docker-entrypoint.d/*.sh` counts `mountpoint` 9x, `envsubst`
6x, `basename` 3x, `dirname` 2x, one each of `sha1sum`, `seq`, `md5sum`,
`getconf`, `dpkg-query`. nginx:alpine's three are `envsubst` (`20-…:53`),
`getconf _NPROCESSORS_ONLN` (`30-…:168`), `apk manifest nginx` (`10-…:50`).
**Every one of them was an `execve` with no exec-map entry** -- the silent
fallback to program 0 that `execmap.rs` records as causing four incidents.

I had expected the growth to be mostly bare-word noise amplified by reaching
more scripts. On these two images it is overwhelmingly genuine: Debian's nginx
entrypoint chain really does run ~23 separate coreutils binaries.

### Why the parser earns its dependency

Word boundaries, and one case decides it:

```
PATH=/usr/local/bin:/usr/bin      ONE word, and it does not start with '/'
```

A regex finds `/usr/bin` there and offers it for directory fan-out, which admits
every executable in the image while still looking like it worked. Also: comments
are not words, and `case "$f" in *.sh)` is a pattern rather than a path.
`syntax.Word.Lit` alone is insufficient -- it returns "" for
`"/docker-entrypoint.d/"`, the exact word this exists to read -- so `wordLiteral`
handles quoted literal runs itself.

### ❗ Neutralization found a defect in my own test

Three neutralizations, none by compile error. The third -- stub `scanShell` to
always error -- was predicted to fail the traversal tests **while the widening
test still passed**, because that is the case where the `bareWords` fallback is
supposed to carry the closure unharmed.

It failed instead. The first version of `TestShellScanOnlyEverWidensTheClosure`
computed its expectation by running `bareWords` over EVERY script in the
inventory, including the four that only the directory fan-out can reach. So it
was re-asserting reachability -- duplicating the test above it -- rather than the
widening property it was named for.

Rewritten against `bareWordClosure`, an independent reimplementation of the
pre-change algorithm kept in the test file. ⚠️ A deliberate second copy, and the
only one here that should exist: its purpose is to keep behaving the way
production no longer does, so "we only added" is something a test computes rather
than something a comment claims.

**The prediction is what caught it.** A neutralization run without one would have
shown three red tests and looked like success.

### Costs, and what is untouched

⚠️ nginx:latest goes 34.9 MB -> 100.9 MB of fused images and 6 -> 29 programs:
23 more full translations, minutes to hours each. Region use 18.5% -> 23.6%. The
object cache goes cold for any image whose closure changed.

⚠️ **Widening amplifies the existing over-approximation** -- each newly-reachable
script is fed to `bareWords` too. The narrowing half (command-position
extraction) is what would pay that back, and is out of scope by decision.

⚠️ **postgres is unchanged at 71 and still 16.1 MiB over the ceiling.** Its
entrypoint does not use the fan-out idiom, so the region decision is untouched.

Not handled, and none of it silent -- each simply yields nothing: `${VAR}` /
`$(cmd)` in a path, relative paths outside `.`/`source`/`exec`, `zsh`, and any
directory named only through an expansion.

## Unresolved-reference reporting: answering "should we emulate sed?" with evidence instead of a sample, 2026-08-27

Asked 2026-08-26: should the shell scanner emulate sed, awk, printf so it can
resolve computed paths? Measured six images first -- every entrypoint and
`docker-entrypoint.d` script, 1,223 lines:

| substitution | n | what it produces |
|---|---|---|
| `basename "$0"` | 6 | `ME=`, a log prefix |
| `id -u` | 5 | a uid for a permission test |
| `dirname "$relative_path"` | 4 | a `mkdir -p` subdir |
| `awk … /etc/resolv.conf` | 4 | resolver list; env var names |
| `printf '${%s} '` | 3 | an envsubst filter string |
| `date`, `mktemp`, `dpkg-query`, `apk` | 9 | timestamps, temp files, a checksum |

**37 substitutions, none producing a path to an executable.** The `$( grep … )`
hits that looked like command position were heredoc bodies feeding `while read`.

❗ **And the ceiling is not effort.** Those awk calls read `/etc/resolv.conf` and
`ENVIRON` -- inputs that do not exist at build time. raptormark is AOT: a perfect
`sed` still cannot resolve a path that depends on a file the operator mounts at
`docker run`. The two dynamic-exec patterns that DO matter were already handled
without emulating anything -- `"$f"` from a directory loop (the fan-out) and
`exec "$@"` (entry selection from CMD).

Six images is a sample, not a census. So instead of picking a tool from it, the
scanner now REPORTS what it could not resolve.

### What it collects

`image.Closure` returns `[]image.Unresolved` alongside the closure (matching
`image.Plugins`' shape; 12 call sites take `_`). Each entry carries the reference
as written, the script, a kind -- `command` for a computed EXEC target, `path`
for a path-shaped argument -- and **`Via`, the commands inside any `$(…)`**.
`Via` is the field the question turns on: if a future image's unresolved exec
target reports `via sed`, that is the evidence, and until one does there is none.

`raptormark build` prints a capped summary; `pipeline.Result.Unresolved` carries
the full list; the `headroom` driver prints everything plus a `$(...)` histogram.

### ⚠️ The first version was correct and unusable

Running it is what showed this. nginx:latest reported **25 references of which
ONE mattered** -- the rest were `entrypoint_log` arguments like
`"$ME: info: /$DEFAULT_CONF_FILE is not a file or does not exist"`, messages that
contain a slash because they quote a filename. Exactly the "diagnostic whose
first line is always noise" failure my own comment on the positional-parameter
exclusion had warned about, three functions higher up.

A literal-whitespace filter now drops them -- **except when the word contains a
`$(…)`**. That exception is load-bearing: the awk substitution that answers the
whole question is full of spaces and would have been filtered away with the
noise. nginx:latest is now 13 entries.

### ❗ A positive control caught a real gap

`exec "$CMD"` reported NOTHING, while `exec "$@"` correctly stayed silent.
Command position is the literal `exec`, so the target sits in ARGUMENT position,
and the path-shaped rule missed it -- `"$CMD"` has no literal separator. Without
the control asserting that the same scanner DOES fire on a non-positional
expansion, the exclusion would have been indistinguishable from a reporter that
never fires, and every "no unresolved references" would have been meaningless.

### First readings

```
nginx:latest   13 refs,  1 computed exec ("$f", already covered by the fan-out)
               commands inside $(...): awk=1     <- builds an env filter, not a path
postgres:17    10 refs,  2 computed exec:
                 "$f"                  over /docker-entrypoint-initdb.d (empty in the base image)
                 "${query_runner[@]}"  a bash array whose value psql is already in the closure
```

So on the two largest images measured, the answer to "which tool would an
emulator have to be" is still **none** -- but it is now collected rather than
argued, and the next image will say so on its own.

## E2E green, and the copy-relocation gap re-scoped by one column, 2026-08-27

### The suite

`137 pass / 0 fail / 19 skip in 399 s` -- identical in every count to the last
recorded reading, on a warm cache with 88 object-cache hits. The tests that
exercise what changed all pass: `TestBuildCommandDrivesTheWholePipeline`
(21.86 s, the whole `raptormark build` driver including entry selection),
`TestPostgresStyleDlopenResolvesPerUnit` (11.03 s, the per-unit plugin path the
entry fix un-broke), `TestOpenSSLFixtureDiscoverAndFuse`,
`TestNginxAlpineFuseHandlesMusl`, `TestFusedDynamicProgram`.

⚠️ **What it does NOT establish.** Nothing in the suite builds a stock nginx, so
the 23 newly-added programs are not shown to RUN. That is the same blind spot
that let the closure be wrong for so long. The claim rests on the unit tests, the
transcribed fixtures, and each addition having been verified as genuinely invoked
by grepping the scripts.

### The copy-relocation entry was priced wrong, in both directions

Recorded 2026-08-26 as a new fuser gap after node:22-slim failed to fuse with 29
unhandled `R_AARCH64_COPY`. Investigating it moved the entry twice.

**It is CHEAPER than recorded.** node defines each copy-reloc symbol in its OWN
`.dynsym` -- measured, `in6addr_any` 16 bytes, `_ZSt7nothrow` 1,
`_ZTVN10__cxxabiv117__class_type_infoE` 88, each at the relocation's own offset.
`globalSymbols` is first-wins with the executable at `objs[0]`, so **the
re-pointing half this entry called a design decision is already done**. What is
missing is only the memcpy from the library's definition.

⚠️ It must be DEFERRED, like `ifuncFixup`: the targets sit in `.data.rel.ro`,
they are C++ vtables full of pointers, and the executable does not relocate them
itself (measured -- only 3 unrelated `ABS64` touch that range). The pointers
become correct when the SOURCE library is relocated.

**And it MATTERS LESS than recorded, which is the finding worth keeping:**

| image | ELF type | COPY relocs |
|---|---|---|
| node:22-slim | **ET_EXEC** | **29** |
| php, postgres, redis, nginx, ruby, python | ET_DYN (PIE) | **0** |

❗ The zeros are STRUCTURAL. A copy relocation exists only because a **non-PIE**
executable references data in a shared library; a PIE reaches it through the GOT
and the linker allocates no copy. So this is not "node is first and the rest will
follow" -- node is the only non-PIE binary in the survey, and toolchains have
defaulted to PIE for years.

**The count was the wrong summary.** "1 of 7 images" describes the sample; "the
non-PIE case" predicts the next one. Not taken, on priority rather than
difficulty: one image, for a case the ecosystem is leaving, against a change
whose failure mode is a C++ program crashing deep in its runtime.

✅ One correction to my own earlier reasoning: I had implied node's 122 MB made
this expensive to verify. It does not. A minimal non-PIE C++ guest referencing a
vtable from a shared library reproduces the relocation in seconds, so the
verification ceiling I assumed was never there.

## Auditing the "guarded by X" claim class, 2026-08-27

Four times this session a comment named a safety net that was not on the path it
appeared to protect (`_ecv_save_call_history`, `UnitExports`, `plugins.go`'s
`Report.SkippedExtras`, and LTM repeating the last). Rather than keep hitting
that class by accident, audited it: extract every `Test[A-Z]\w{4,}` mentioned
anywhere in `*.go`, `*.rs`, `*.md`, `*.sh` and check it is defined.

⚠️ **The first pattern I wrote returned ZERO and I nearly reported it as a clean
result.** A regex with an alternation and a lazy `[^.]{0,80}?` matched nothing at
all. Replaced with a dumb, checkable version plus a positive control -- assert a
name that must exist is found -- which is the only reason the second run can be
believed. 540 names, 15 undefined.

### One genuinely missing guard

`internal/translate/translate.go`'s `Request.Jobs` says **"TestObjectKeyIgnoresJobs
holds this."** Nothing in the package so much as mentioned `Jobs`.

The invariant held by construction -- `ObjectKey` reads `TranslateID`,
`ModuleID`, `Keep` and the fragment, never `Jobs` -- so nothing was broken. What
was missing is the thing that would NOTICE if it broke, and the cost if it did is
the quiet kind: `Jobs` caps concurrent codegen and cannot change a byte of the
object, so putting it in the key makes every build with a different `--jobs` miss
a cache that costs HOURS to refill. The symptom is a slow build, not a failure.

✅ Written, with a positive control (changing `Keep` must still move the key, or
the equality assertions would pass against a key that ignores everything).
Neutralized by adding `jobs=%d` to the key: fires naming both values.

### Five stale names, none of them a missing guard

| claim site | named | actual |
|---|---|---|
| `rootfs/boot.go` | `…OnTheSidecarMapPaths` | `TestRuntimeAgreesOnTheSidecarABI` |
| `builder/translateone.go` | `…PICObjectsStillLinkFlat` | `TestCompileIREmitsPIC` |
| `e2e/hostedload_test.go` | `TestEmbedderPlacesSideModules` | `TestEmbedderRunsTheSideModule` |
| `relay/relay_test.go` | `…MatchesALiveHandshake` | `TestAcceptKeyMatchesTheRFCVector` |
| `e2e/syscalls_test.go` | `TestEcvisorRuntime` | `TestEcvisorSingleProgram` |

❗ **A wrong name reads as an absent guard.** `boot.go`'s is the instructive one:
it claims three copies of `DlPath` are tied to one value, and resolving whether
that was true took reading three files. Grepping the name is how anyone verifies
such a claim, so a name that finds nothing is indistinguishable from no guard --
and this comment is itself the CORRECTION of an earlier stale claim in the same
place, whose lesson it records.

⚠️ `relay_test.go`'s was a doc comment that no longer named its own function, so
`go doc` would not associate them either.

⚠️ **Writing the correction re-introduced the problem**, briefly: my first fix
spelled the dead name in the warning text, which kept it alive in the audit.
Rephrased so the warning survives without the identifier (`a …MapPaths variant
that has never existed`).

### The nine that remain are correct

Historical ("It absorbed X", "REPLACES X"), the three `…UnderBoundedSnapshots`
names belonging to a design that is still an open TODO, and doc-comment headers
in decode-oracle listing intended coverage. Left as they are.

### Extending the audit to Bazel targets: a second half-built guard, 2026-08-27

Same method, different claim class: every `//pkg:target` named in a `.md`, `.go`,
`.sh` or `.bzl`, checked against the `name = "..."` rules in that package's
`BUILD.bazel`. 27 labels, positive control on `//builder:stage`.

Most misses were regex artifacts and I say so rather than counting them as
findings: `.bzl` load labels are files not rules, `//visibility:public` and
`//command_line_option:platforms` are Bazel built-ins, `//go:build` is a Go
directive, `@rules_rust//rust:defs.bzl` loses its repo prefix to the pattern, and
several had a trailing period from prose.

**One real: `//runtime:cargo_lock_agrees_test` did not exist.**

`bazel/crates.bzl` fetches ecvisor's two crate dependencies by URL and sha256
instead of running cargo, and its docstring says the hashes "are not a second
source of truth to keep in sync -- they are the same numbers, and
`//runtime:cargo_lock_agrees_test` fails if they ever stop being."

❗ **And the scaffolding for it was already there.** `crates.bzl` exports
`CRATE_CHECKSUMS` with the comment "Exposed so a test can assert these agree with
runtime/Cargo.lock rather than trusting that whoever bumped one remembered the
other" -- and **nothing consumed it**. Someone built the hook, wrote the claim in
the present tense, and never landed the test. So the two files were a second
source of truth kept in step by a sentence.

**What drift costs.** `cargo` reads `Cargo.lock`; Bazel reads `crates.bzl`. A
`cargo update` moves one and not the other, and then the Rust gate tests one
version of miniz_oxide while the SHIPPED archive contains a different one --
both green. The "two gates where one of them lies" shape, and nothing else in the
tree reads both files.

✅ Written as `//runtime:cargo_lock_agrees_test`, the name the docstring already
claimed. A genrule writes out the SAME Starlark constant the fetch uses, so the
script compares Cargo.lock against what Bazel actually fetches rather than
against a transcription -- which is also what finally gives `CRATE_CHECKSUMS` a
consumer.

✅ **Neutralized three ways**, the third being the one that matters:
  * a changed sha256 in `crates.bzl` -> names the crate and prints both values;
  * a VERSION bumped on one side only -> same, a separate arm;
  * the `checksum` field renamed in `Cargo.lock` -> **"extracted ZERO
    checksummed packages ... would compare two empty sets and pass while
    checking nothing."** Without that arm this test is worthless the day
    Cargo.lock's format shifts, because two empty sets compare equal.

`bazel test //...` is now 15 tests, all passing.

### Third claim class: environment switches, 2026-08-27

Same method again: every `RAPTORMARK_*` / `ECV_*` in a `.md` versus every one the
code reads, both directions, positive control on `RAPTORMARK_BUILDER`.

**Documented-but-unread: clean.** The single hit, `ECV_REG_`, is a prefix in
prose, not a variable.

**Read-but-undocumented: 27, and none of them a defect.** Most are e2e gates
whose `t.Skip("set X=1: …")` message IS the documentation, and it appears exactly
when needed. Two that looked like unguarded build knobs are not:
`RAPTORMARK_SHARED_UNITS` and `RAPTORMARK_SIDE_DIR` are read by `runtime/src/*.rs`
at guest run time, so they cannot affect an emitted object;
`RAPTORMARK_EHFRAME_LIB` is a fixture path in `ehframe_test.go`.

**The cache-identity question came back clean too**, which is the one that
matters -- a byte-affecting switch outside `ExperimentalSettings()` means the
object cache serves an object for a build it does not describe. `internal/translate`
reads exactly ELEVEN `RAPTORMARK_*` switches: the 6 in `experimentalVars`, and 5
excluded -- **exactly the five `experimental.go`'s "What is NOT here, and why"
comment names.** The comment is accurate.

### The gap the existing guards left, and it is one direction only

`experimental.go` is already carefully guarded, including against the trap I
would have flagged: `byteAffecting` in the test is named INDEPENDENTLY of
`experimentalVars`, with a comment explaining that iterating the list under test
means deleting an entry silently stops checking it.

What neither test can see is a switch that is READ and appears in NEITHER list.
`TestTheIdentityListIsExactlyThese` compares two hand-maintained lists; adding
`os.Getenv("RAPTORMARK_SOMETHING")` to the package touches neither.

❗ **And doing nothing is not neutral there.** An unclassified switch is silently
OUT of the object identity, so two builds that differ collide on one key.

✅ `TestEveryEnvSwitchIsClassified` scans the package's own non-test sources and
requires every `RAPTORMARK_*` read to be in `experimentalVars` or in a
`nonByteAffecting` list TRANSCRIBED from the comment -- so the comment is checked
rather than parsed. Both directions: a read with no classification, and a
classification nothing reads (a misspelled `experimentalVars` entry contributes
nothing to the key while looking like it does).

⚠️ Declared `:gosrcs` as `data` rather than tagging the test `manual`, which is
what `//internal/rootfs` and `//internal/builder` had to do. Opting a package out
of `bazel test` to satisfy one file loses the compile coverage of every test
beside it. Verified under both `go test` and `bazel test`.

✅ Neutralized three ways: a new unclassified switch, a list entry nothing reads,
and the regex no longer matching -- the last producing "found NO os.Getenv …
would pass while checking nothing", which is the arm that keeps the other two
honest.

### Where three audits leave this

| class | checked | stale | real defect |
|---|---|---|---|
| test names in comments | 540 | 5 | 1 (`Jobs` unguarded) |
| Bazel targets | 27 | 0 | 1 (`cargo_lock_agrees_test` absent) |
| env switches | 65 | 0 | 0, plus one gap now closed |

❗ The two real defects share a shape and it is not carelessness: **the claim and
the scaffolding land, the test does not.** `crates.bzl` exported `CRATE_CHECKSUMS`
"so a test can assert these agree" and nothing consumed it; `Request.Jobs` said
"TestObjectKeyIgnoresJobs holds this" and no such test existed. Both were written
by someone who knew exactly what was needed.

## Narrowing Ruby's `--disable-gems` longjmp abort, statically, 2026-08-27

Picked up "Ruby under ecvisor needs `--disable-gems`, and nothing said so",
whose last line was "Not diagnosed further". Four things settled without running
anything, and one of them the entry asks as an open question.

**The entry's own open question was already answered in the code.** It asks
"whether it is the machine stack bounds `__longjmp_chk` consults";
`runtime/src/sys.rs`'s `NR_SIGALTSTACK` comment records the probe: reporting the
WHOLE ARENA as `ss_sp`/`ss_size` -- which admits any in-arena target -- and
`____longjmp_chk` still refused, "so the SP glibc recovers from the jmp_buf is
outside the arena altogether." That is a much sharper statement than the entry
carries, and it was sitting in the syscall it concerns.

**The mechanism, from the fixture rather than from memory.** Disassembling
`__sigsetjmp` in `.agents-workspace/fixtures/ruby-glibc.fused`:

```
1636660: mov x4, sp
1636668: ldr x2, [x2, #3632]     ; &__pointer_chk_guard, via the GOT
163666c: ldr x3, [x2]            ; the guard VALUE
1636670: eor x5, x4, x3          ; SP ^ guard
1636674: str x5, [x0, #104]      ; jmp_buf[104]
```

Both LR and SP are mangled. A wrong guard at demangle time therefore produces an
arbitrary SP -- precisely the symptom above.

❌ **And the hypothesis I would have bet on is REFUTED.** The GOT slot at
`0x17afe30` holds `0x183fb58`, the single `__pointer_chk_guard` in the image, and
there is only one (`libc.so.6` defines it; `ld-linux-aarch64.so.1` defines none).
So this is NOT `globalSymbols` first-wins binding the wrong copy -- the class of
defect fixed yesterday, and the reason it was worth testing first. Recording the
refutation because the next person will have the same prior.

The guard is zero in the image, and ecvisor DOES supply `AT_RANDOM` (16 bytes of
`0x01`), so startup writes a non-zero constant over it. A guard that stayed zero
would round-trip harmlessly; one that CHANGES gives `SP ^ 0x0101010101010101`,
which lands far outside a 96 MiB arena.

⭐ Next step is dynamic and named in the entry: read the guard at both points. And
a second suspect nothing in this tree has looked at -- this glibc's setjmp
contains a **GCS** path (`chkfeat`, `mrs x2, gcspr_el0`, ARMv9.4). A lifter that
treats `chkfeat` as a NOP leaves `x16 = 1`, takes the branch and skips GCS,
which is correct BY LUCK rather than by handling.

⚠️ **None of this is a diagnosis and the entry says so.** Four facts and two named
hypotheses. What it buys is that the next attempt starts from the disassembly
instead of from the stack-bounds theory the code had already refuted.

## Status of the sweep

The audit method is mined out for cheap classes -- test names (1 defect), Bazel
targets (1 defect), env switches (0). What remains in TODO.md is mostly not
"unstarted work": of 62 open entries, the ones I have read individually are
dominated by (a) decisions that are the operator's, (b) deliberate non-fixes with
the cost already analysed, and (c) work gated on hours of translation. A keyword
classifier put 33 in an "actionable" bucket and I do not trust it -- several I had
already read and they are (b).

## The fused region grown to fit postgres, 2026-08-27

Operator decision: "grow the region to fit postgres." Done -- `BRK_START_VMA` /
`brkStartVMA` `0x0A000000 -> 0x0C000000`, fused region **156 -> 188 MiB**. Both
postgres versions now plan a shared layout: `:17` at 91.6% (15.9 MiB spare),
`:18` at 95.0% (9.4 MiB).

### ❗ The arena did NOT grow, and the reason is in the code

The obvious reading of "grow the region" is to make the arena bigger. That would
have been wrong, and `runtime/src/arena.rs` says why in a CEILING note:

> arena_size × (live + suspended) < 4 GiB … 384 MiB allows 10 buffers where 512
> allowed 7, and postgres needs 7 concurrently to serve one guest-side psql

Each suspended process owns a full-size arena buffer on a wasm32 4 GiB budget.
Growing the arena to give postgres a bigger fused region would have spent exactly
the process headroom postgres needs -- **the change would have paid for itself
out of its own beneficiary.**

The same file records where the space actually was:

  brk  32 -> 8 MiB   max high-water over 104 runs was 1,164 KiB, 103 at 136 KiB;
                     8 MiB is still 7x the largest ever seen, and overflow is
                     GRACEFUL (NR_BRK returns the break unchanged -> ENOMEM ->
                     malloc uses mmap, the path initdb already takes)
  mmap 160 -> 152    peak private mmap measured 88 MiB; still 1.7x, and well
                     above the 96 MiB that prompted the 2026-08-25 rebalance

### Cost: none to the object cache, and that is checkable rather than hoped

`brkStartVMA` is used ONLY as a refusal test (`if next > brkStartVMA`), so
RAISING it cannot move a library base. Verified on nginx:latest -- its band still
starts at `0x800000`. `BRK_END_VMA`/`MMAP_START_VMA` are pure ecvisor policy and,
per a JOURNAL entry from the rebalance, "appear NOWHERE outside `runtime/`".

The rebuilt builder (`raptormark-builder:brkgrow`) carries **identical**
`raptormark.base_id` and `raptormark.translate_sh` labels and a **different**
`libecvisor.a` hash -- both checked, and the second on the ARTIFACT rather than
the labels, which is what AGENTS.md insists on.

⚠️ **`raptormark build-image --skip-base` tried to REBUILD the patched base** and
failed pulling `raptormark-elfconv-base:wasix`, which does not exist. That
failure was fortunate: a rebuilt patched base gets a new id, `BASE_ID` moves, and
every cached object dies. `--skip-base` skips the UNPATCHED base only. The manual
recipe in AGENTS.md -- stage, then `docker build` against the existing
`raptormark-elfconv-base-patched:wasix` with both labels passed verbatim -- is
the one that layers rather than rebuilds.

### ✅ A guard fired on me, correctly

The first re-measurement after the change printed, instead of numbers:

> the planner ACCEPTED a layout whose top is 0xb020010, above this driver's copy
> of brkStartVMA (0xa000000). The copy is stale -- re-read internal/fuse/layout.go

That is the assertion I put in `headroom` yesterday against its own duplicated
constant. Without it the driver would have reported postgres at 110% of a region
it now fits inside.

### ⚠️ And a gap this exposed

`TestBrkStartMatchesTheRuntime` compares Go source to Rust **source**, not to the
runtime baked into the builder image. A stale builder plus a new fuser passes the
guard and produces a module whose libraries sit exactly where the heap starts.
The two halves must ship together and nothing enforces it. Recorded in TODO.md.

⚠️ postgres:18 is at 95% -- four more 2 MiB-aligned libraries and it falls back
again. The next lever is `libAlign`, which moves every base and re-lifts
everything.

### E2E on the grown region

`137 pass / 0 fail / 19 skip in 411 s` -- identical in every count to the last
recorded reading, against `raptormark-builder:brkgrow`.

✅ **88 object-cache hits, 1 miss.** The prediction that raising a refusal test
cannot move a library base held on the artifacts, not just in the argument.

The memory-pressure tests are the ones that matter here and all pass:
`TestForkDoesNotLeakArenasUnderEcvisor`, `TestDivergentMemorySurvivesSwitchesUnderEcvisor`,
`TestCrossProgramSwitchesUnderEcvisor`, `TestMmapRefusalsDoNotKillTheModule`,
`TestThreadGapsUnderEcvisor`, `TestMuslThreadUnderEcvisor`, plus
`TestBuildCommandDrivesTheWholePipeline` and `TestPostgresStyleDlopenResolvesPerUnit`.

⚠️ **What it does NOT establish.** No e2e fixture is large enough to have
overflowed the OLD ceiling, so nothing here exercises a closure that newly fits
-- which is the entire point of the change. The suite proves the smaller brk and
mmap regions break nothing; it does not prove a fused postgres runs. That needs a
`raptormark build postgres:17` and hours of translation, and it is the honest
next step rather than something this run covered.

⚠️ `raptormark-builder:rebal` is now a HAZARD as well as a record: its runtime
still has `BRK_START_VMA = 0x0A000000`, so pairing it with a current host fuser
produces a module whose libraries can sit where the heap starts, and the
source-to-source guard cannot see it. Both builders are in `.agents/preserve.json`
with notes saying which is which.

## `raptormark build postgres:17` end to end: 151 of 152, blocked on a Go binary, 2026-08-27

Operator asked for the full build. **It FAILED**, after ~7 hours, at image
**152/152**. Reporting the outcome first because the earlier lines of this
session's log look like success and are not.

```
translate: [152/152] usr_local_bin_gosu.fused
F InstructionLifter.cpp:904] Expected that a memory operand should be represented
  by machine word type. Argument type is i32 and word type is i64 at 0x7d764
translate-one: elflift: signal: aborted (core dumped)          [exit 1]
```

⚠️ **The background task reported exit 0 and that was my shell, not the build.**
The command was `raptormark build … > log 2>&1; echo exit=$?; date`, so the task
status came from `date`. The real status, `exit=1`, was in the task's own output.
A trailing command in a backgrounded pipeline launders the exit code.

### ✅ The region change is exonerated, by construction and by measurement

`gosu` is a **statically linked ET_EXEC Go binary**. Its `.text` sits at
`0x11000` in the fused image -- byte-for-byte the same address as in the original
-- because a non-PIE static executable keeps its own vaddrs and has no libraries
for a Layout to place. `0x7d764` is inside that `.text`. The shared layout never
touched it, so this failure is independent of `BRK_START_VMA` moving.

### The actual defect, fully localised

```
7d764:  0ddf8402   ld1 {v2.d}[0], [x0], #8        inside aeshashbody
```

**Real code, not data.** That matters: 25 of the 29 `[ecv-undecoded]` lines in
the build carry `enc=0x00000000`, which is the signature of over-claimed function
boundaries walking into padding -- and this is NOT one of them. It is a genuine
lifter gap on a NEON single-structure load with post-index immediate.

`Decode.cpp` has `TryDecodeST1_ASISDLSEP_*` (stores) and no `patches/*.patch`
mentions LD1 at all. `aeshashbody` is Go's AES-accelerated map hash, so this is
reachable from **any** Go binary that uses maps.

❗ **gosu has never been translated in this tree** -- zero objects in the cache
before today. This is the first Go binary the pipeline has attempted, and it is
the first thing it hit. Worth stating as a capability limit rather than a
postgres detail: **raptormark cannot currently translate Go binaries.**

### What survived

**151 of 152 objects were produced and are cached** (5.7 GB under
`.agents-workspace/pgbuild/out`, kept there rather than in `tmp/` precisely
because it is seven hours of work). A rerun re-translates none of them.

### ⚠️ The fork, and it is expensive either way

Fixing the lifter is an `elfconv` patch, which changes `BASE_ID`, which
invalidates **every cached object** -- including the 151 just built. So the fix
costs the seven hours again. Excluding `gosu` instead needs a CLI surface that
does not exist (`ClosureOptions.Exclude` is in the library, unexposed) and yields
a module whose guest cannot exec what its own entrypoint runs.

## Four lifter patches, 0068-0071, and a latent silent bug found by testing them, 2026-08-27

Operator chose to batch the lifter targets so one `BASE_ID` change buys several
capabilities. Three of the four planned landed; the fourth was assessed and
deliberately left; and validating them turned up a fifth defect that had been in
the tree since patch 0010.

| patch | instruction | witness |
|---|---|---|
| 0068 | `ld1 {Vt.d}[i], [Xn], #imm` operand order | postgres:17's `gosu` aborted the lifter |
| 0069 | `orr` ASIMD immediate | ruby `--yjit*` SIGILLs |
| 0070 | `fmov <Vd>.2S, #imm` | killed a test guest |
| 0071 | `fmov <Vd>.2D, #imm` wrong constant | **found while testing 0070** |

### 0068: an operand-order mismatch, not a missing instruction

The eight `TryDecodeLD1_ASISDLSOP_*` decoders emit (vector, index, memory) while
`LD1_LANE_*` is declared `(V dst_vec, M src_mem, I32 index_imm)`. The memory
operand lands on the `I32` parameter and remill aborts.

❗ **The four NON-post-index forms in the same family already emit the right
order and share the same semantics**, which is what identifies the post-index
eight as the outliers rather than the semantics as wrong. ST1 is untouched: its
semantics genuinely differ -- `(src, I32 index, M dst_mem)` -- and its decoders
already match. A classifier I wrote flagged all 24 as wrong; checking ST1's
signature is what stopped 12 false positives from being "fixed".

### ⚠️ Two things I got wrong, both caught by checking rather than by luck

**A STUB/REAL classifier that reported ABSENT as REAL.** `awk` returning an empty
body fell through to the `*)` arm. It told me decoders existed that were defined
in another file entirely. The conclusion happened to survive -- the decoder IS
real, in `Arch.cpp` -- but the evidence for it was worthless.

**Hand-editing a patch file.** Correcting 0070 by editing the diff text broke the
hunk offsets and the base build failed applying it. Patches must be REGENERATED
from a source tree, and the tree has to be the one with the earlier patches
already applied -- 0010 rewrote the region 0070 touches.

### ❗ 0071: the bug that testing found, which no diagnostic would have

Patch 0070's first version copied `data.imm8.uimm` from the 2D form. That is the
SCALAR FMOV immediate field; the vector ASIMDIMM format splits abcdefgh across
bits [18:16] and [9:5]. It **decoded cleanly and produced 2.0 for
`fmov v1.2s, #1.0`**.

So I extended the guest to exercise the 2D form as well -- and found the same
mistake in patch 0010, live in the tree since it landed:

```
native  : ORR=OK(00000080) FMOV2S=OK(1.0) FMOV2D=OK(1.0)
ecvisor : ORR=OK(00000080) FMOV2S=OK(1.0) FMOV2D=BAD(2.0)     <- before 0071
ecvisor : ORR=OK(00000080) FMOV2S=OK(1.0) FMOV2D=OK(1.0)      <- after
```

⚠️ **Nothing in this tree could have reported it.** It is not undecoded, so no
`[ecv-undecoded]` line; it does not trap, so no `_ecv_unreached`; it lifts and
runs and returns a floating-point constant that is off by one exponent bit. The
only reason it surfaced is that the validation guest COMPARES AGAINST NATIVE
rather than checking that translation succeeded.

### The fourth target, assessed and not taken

The by-element 2S multiply needs a new mixed-arrangement template
`(v2f, v2f, v4f, index)` and new ISELs for FMUL and FMLA in both plain and FPSR
variants -- eight ISELs for FMUL alone -- where the other three each mirrored an
existing sibling exactly. And it has **no witness**: its current behaviour is a
LOUD refusal (`ByElementIndexFits` returns false), not a silent wrong value.
Every other target here has an observed failure. Left until something hits it,
on this tree's own rule that execution predicts capability and site counts do
not.

### ⚠️ A labelling mistake of mine

`raptormark-builder:brkgrow` was built with `--translate-sh <pinned>` to preserve
the object cache. That was WRONG: `translate_sh` hashes
`internal/builder/translateone.go`, which I had edited hours earlier in the
guard-name audit -- a comment change, but the hash is over contents. So brkgrow
carries a pipeline identity it had not earned, from the flag AGENTS.md calls
"unsafe unless you are certain the objects are unchanged". I was not certain; I
had forgotten my own edit. Harmless in fact (the objects are byte-identical to
what the real pipeline emits, since only a comment moved) and now moot, since
`lifter4` computed the hash fresh and the new `BASE_ID` invalidates everything
regardless. Recorded because the override's danger is exactly this: it is easy to
be sure and wrong.

### postgres:17 built end to end, 7h25m, and what running it revealed

```
BUILD_EXIT=0     08:17:41Z -> 15:42:55Z
app.wasm     1,653,444,985 bytes (1.65 GB)
rootfs.img     165,032,853 bytes
71 programs, 81 units, 152 images translated
```

✅ **Zero fallback lines** -- the grown region held, postgres planned SHARED.
✅ **Zero `[ecv-undecoded]` encodings** -- patches 0068-0071 cleared everything
the whole closure executes, including the `enc=0x00000000` runs that the failed
build reported.

And it RUNS. ecvisor boots, TLS and pthread bring-up complete, the ld.so hooks
install, and **77 `_dl_init` constructors run** -- a fully initialised glibc
dynamic-linking environment inside wasm.

### ❗ But it runs APT

```
WARNING: docker-entrypoint.sh does not have a stable CLI interface. ...
E: Invalid operation postgres
```

That is apt's error text, and apt is program 0. It is the program-0 fallback
`execmap.rs` documents -- "the guest ran the WRONG PROGRAM under the right argv"
-- which that file records as having caused four incidents.

**Cause:** `internal/pipeline.build` writes the boot record's argv as
`cfg.Entrypoint + cfg.Cmd`, i.e. `["docker-entrypoint.sh", "postgres"]`. A bare
script name: not PATH-resolved, not a registry program, so the exec map cannot
place it.

❗ **It is the same defect as the discovery-side entry bug fixed the same day,
one layer down.** `EntryFromSeeds` now resolves the closure's entry to a real
program. The boot record still takes the image config verbatim. The pipeline
computes a correct `entry` and then does not use it there.

⚠️ Filed rather than patched because the right answer is a design choice: point
argv[0] at the resolved PROGRAM (runs postgres, skips the entrypoint script's
setup -- not what `docker run` does) or at the script's resolved absolute PATH
(faithful, if the boot path does shebang resolution the way execve does, which is
unverified). Recorded in TODO.md with both.

✅ The good news on cost: the sidecar is host-side and independent of the 1.65 GB
module, so iterating on this needs a rootfs export and `rootfs.Build`, not
another 7h25m.

### CORRECTION 2026-08-28: the boot argv is resolved against the CWD, and scripts cannot run at all

The operator asked whether the entrypoint is resolved based on the working
directory. It is, and the question cracked the diagnosis open -- the entry above
said "not PATH-resolved", which was directionally right and named the wrong
mechanism, and stopped one step short of the real finding.

**What actually happens.** `entry.rs` calls
`programs.resolve(&vfs, &cwd, argv[0])`; `Programs::hash_for` tries an exact
exec-map key and then falls back to `vfs.resolve(cwd, path)`. That is
CWD-relative. There is no PATH lookup anywhere on the path. Docker resolves a
bare exec-form entrypoint with `execvp` -- PATH -- so the runtime implements a
different rule than the images are written against.

**It is not a postgres problem.** Predicted from the mechanism and confirmed
across the survey:

| image | entrypoint | cwd | resolves? |
|---|---|---|---|
| postgres:17/18, node:22-slim, php:8.3-cli | bare, in `/usr/local/bin` | `/` | ❌ |
| redis:7-alpine | bare | **`/data`** | ❌ (`/data/docker-entrypoint.sh`) |
| nginx:latest/alpine | `/docker-entrypoint.sh` | `/` | ✅ |

5 of 7. nginx works only because its Dockerfile writes the path with a leading
slash -- which is why nothing had caught this.

**And the bigger half, which the corrected mechanism led straight to.** Fixing
the path would not be enough: `boot` resolves registry PROGRAMS and a script is
not one, and `execve` returns ENOEXEC for scripts because `sys.rs:5505` says
"shebang support is a later addition".

❗ **So `internal/image/image.go`'s `Script` doc is false** -- "the runtime's
shebang handling execs the *interpreter* and feeds it the script file". The
runtime states it has none. **Fifth lying comment this session, and the most
load-bearing**: the whole scripts-as-exec-targets design rests on it.

⚠️ **This reframes today's closure work.** Discovery walks scripts and pulls in
what they invoke -- correct, and the shell parsing made it much better. But the
guest cannot EXECUTE any of those scripts, so `raptormark run` cannot reproduce
`docker run` for any image whose entrypoint is a script. Which is nearly all of
them.

⚠️ And it recontextualises the earlier claim that the build "runs". It boots,
initialises glibc, runs 77 constructors and executes a real guest program -- all
true and all still meaningful. It does not run postgres, and could not have.

## Shebang support, 2026-08-28

Operator: "implement shebang support". Done, on both paths, plus the host-side
half without which it does not help the motivating case.

### Two halves, and only both together fix anything

**1. `runtime/src/shebang.rs`** -- parse a `#!` line, rewrite argv Linux-style,
wired into `sys_execve` and into `entry.rs`'s boot resolution. Bounded at
`MAX_DEPTH` = 4 (`BINPRM_MAX_RECURSION`) with `ELOOP`, and the line read from at
most `MAX_LINE` = 256 bytes (`BINPRM_BUF_SIZE`).

**2. `image.ResolveExecPath`, used for the boot argv.** ❗ **Shebang support
ALONE does not fix postgres**, and noticing that before rebuilding saved a cycle:
the walk only starts once the file can be READ, and `vfs.read("/",
"docker-entrypoint.sh")` still looks for `/docker-entrypoint.sh`. The bare name
has to be resolved against PATH first, and that belongs host-side where discovery
already does it -- the runtime has neither the inventory nor the image
environment.

### Fidelity decisions, each one a place to be subtly wrong

  * the interpreter's optional argument is **ONE string, not fields**:
    `#!/bin/sh -e -x` gives `/bin/sh` the single argument `"-e -x"`. Splitting it
    would make scripts work here that do not work on Linux.
  * the **script path is the caller's spelling**, not canonicalised, so `$0`
    inside the script matches what was executed.
  * the caller's `argv[0]` is **discarded**; the interpreter takes its place.
  * **CRLF is deliberately not accommodated.** Linux does not strip the `\r`, so
    a CRLF script names an interpreter ending in `\r` and fails. Accepting it
    would make this runtime succeed where the kernel fails -- the harder
    difference to debug.
  * a long line is **truncated**, not rejected, because that is what the kernel
    does with `BINPRM_BUF_SIZE`.

### ⚠️ A check of mine that lied, and the compile error it hid

`cargo check --target wasm32-wasip1 | grep -E "^error"; echo "WASM CHECK OK"` --
the echo runs unconditionally. It printed OK over a real `E0308`. The host build
had passed because the failing arm is behind a `cfg` only the wasm profile
compiles, so the error existed only on the SHIPPING target and my check reported
the opposite. Rewritten as an `if`, and all four net profiles now checked
explicitly.

### Verification

312 Rust tests (8 new parser cases), `cargo fmt`, wasm32 clean, all four net
profiles compile, Go gates green on both module patterns.

Neutralized three ways, none by compile error: `ResolveExecPath` stops doing PATH
lookup (fires with the program-0 explanation); the shebang argument splits on
whitespace (fires naming `-e` vs `-e -x`); `rewrite_argv` drops the script path
(fires on two tests).

✅ `internal/image/image.go`'s `Script` doc is now TRUE, and is dated so it does
not read as though it always was.

### Cost

`base_id` and `translate_sh` are unchanged from `:lifter4`, so all 152 objects
hit the cache: the rebuild reached 152/152 in under 2.5 minutes against 7h25m
cold. Only `libecvisor.a` differs, which is the link.

### ⚠️ I hit the stale-binary trap twice in one day, and the repo has a tool for it

`raptormark run` still printed apt's `E: Invalid operation postgres` after the
argv fix. The fix had never run: `.agents-workspace/drivers/raptormark` was from
`2026-08-27 09:31` and `internal/pipeline/build.go` from `2026-08-28 07:20`. I
rebuilt the BUILDER IMAGE and forgot the DRIVER BINARY -- the same mistake as the
first postgres launch, which I had caught, reported, and then repeated.

`AGENTS.md` names it ("Rebuild the tool after changing the library. A stale
binary produces a 'the fix didn't work' conclusion that is purely an unrebuilt
binary") and `.agents-workspace/drivers/rebuild-drivers.sh -check` reports it in
one line. Running that BEFORE any driver-dependent run is the habit; I had been
rebuilding by hand and getting it right only when I remembered.

Running it also surfaced that **my `image.Closure` signature change broke two
drivers**, `discover` and `survey`, which is not something `go build ./...`
reports -- `.agents-workspace/` is outside the module patterns the gate names.
Both fixed. `parcap` remains broken on `translate.RunAll`, deleted 2026-08-18;
that one predates today and is a scratch driver with no current user.

⚠️ The general point: `.agents-workspace/drivers` is a PRESERVED asset
(`preserve.json`) and it is compiled against `internal/`, but nothing in the
standard gate compiles it. `rebuild-drivers.sh` is the only thing that does.

### Shebang support verified end to end, and postgres now reaches bash

```
[ecvisor] pid=1 boot argv: ["/usr/local/bin/docker-entrypoint.sh", "postgres"]
[ecvisor] pid=1 boot argv after #!: ["/usr/bin/env", "bash", "/usr/local/bin/docker-entrypoint.sh", "postgres"]
```

Both halves visible in one log line each: the host-side PATH resolution turned
the bare `docker-entrypoint.sh` into its real path, and the `#!` rewrite produced
exactly Linux's form -- interpreter, its argument, the script as the caller
spelled it, then the remaining argv.

Two program bring-ups follow: `env` runs and execs `bash`. postgres:17 has gone
from **running apt** to **running its own entrypoint through env and bash**.

### ❗ And the next blocker is one this session already narrowed

```
*** longjmp causes uninitialized stack frame ***: terminated
[ecvisor] fatal: guest trap ... at PC 0x2021ae0 (__remill_error)
```

That is the `__longjmp_chk` -> `__fortify_fail` abort investigated this morning
under "Ruby needs --disable-gems". It now has a **second witness, and a much
smaller one: BASH**, not RubyGems. So it is not a Ruby quirk -- it is the general
`setjmp`/`longjmp` path -- and it sits on the critical path for the flagship
image rather than behind one interpreter's opt-in flag.

What the morning's static work already established, and which the next attempt
should start from rather than rediscover: it is NOT the stack bounds (widening
`ss_sp`/`ss_size` to the whole arena still refused); `__sigsetjmp` mangles both LR
and SP by XOR with `*__pointer_chk_guard`; the GOT slot resolves correctly to the
single definition, so the fuser is exonerated; and the remaining hypothesis is
that the guard's VALUE differs between the setjmp and the longjmp. A bash
reproducer makes reading it at both points tractable in a way `ruby --yjit` did
not.

## setjmp/longjmp: reproduced in 7 lines and localised to the mask-restore path, 2026-08-28

Operator: "fix setjmp/longjmp if possible". NOT fixed. But it went from a
1.65 GB postgres module and a three-hypothesis guess to a 7-line reproducer and
one remaining suspect, with six hypotheses killed by measurement -- including two
of my own from this morning.

### The reproducer

```c
static jmp_buf jb;
static void inner(void) { longjmp(jb, 42); }
int main(void) { if (setjmp(jb) == 0) inner(); return 0; }
```

`gcc -static -O2`. Fails under ecvisor, passes natively.

### What it is NOT, each refuted by a run rather than by argument

| hypothesis | test | verdict |
|---|---|---|
| the stack canary | `-fstack-protector-all`, no longjmp | ✅ `CANARY OK` -- not it |
| the pointer guard is unstable | dump `jb` after setjmp and again before longjmp | identical; demangling by `0x0101…` gives a real text address and the frame pointer -- **setjmp is correct** |
| `ldp`/`ldr`/`eor` mis-lifted | inline-asm replica of `__longjmp`'s exact sequence | ✅ correct under ecvisor |
| callee-saved regs clobbered by syscalls | seed x19, raw `svc` rt_sigprocmask, read back | ✅ `PRESERVED` |
| `sigprocmask` corrupts the jmp_buf | dump `jb` either side of the real call | identical |
| the fuser bound the wrong guard (this morning's prior) | GOT slot + symbol count | single definition, slot correct |

### What it IS

```
_longjmp            (no mask)             OK
siglongjmp(mask=0)  (no mask restore)     OK
siglongjmp(mask=1)  (restores the mask)   FAILS
```

❗ **Only the mask-restoring path.** `__libc_siglongjmp` holds the jmp_buf in
**x19** across a call to `sigprocmask`, then takes a BACKWARD branch to the
`bl __longjmp` site and passes it as `mov x0, x19`.

The failure signature says x0 arrives as zero: `__longjmp` branches to exactly
`0x0101010101010101`, which is the pointer guard, i.e. `0 ^ guard` -- so
`ldp x29, x4, [x0, #80]` read zeros, which is what `[0 + 80]` gives in an
identity-mapped arena.

⭐ **The remaining suspect, and the next test.** x19 (or x0) does not survive
`__libc_siglongjmp`'s backward branch to the call site after `sigprocmask`
returns. A guest replicating that CFG shape -- seed x19, `bl` something that
syscalls, backward branch, read x19 -- confirms or kills it in one run. If
confirmed it is a lifter defect, not a runtime one, and the fix is an elfconv
patch.

⚠️ Not a runtime fix, so no `runtime/` change was made and nothing was rebuilt on
a guess. The morning's static work stands corrected in one place: I had named the
guard's stability as the leading hypothesis, and it is measurably stable.

### Continued: the causal chain, complete

The CFG hypothesis is also refuted -- x19 survives `bl` + backward branch in a
replica (`PRESERVED` under ecvisor). A syscall trace gave the real sequence:

```
trying siglongjmp(mask=1)
svc 135 args=(0x2, 0x494250, 0x0, 0x8)   rt_sigprocmask(SIG_SETMASK, &saved_mask)
bb=0x40057c                              __longjmp branches -- CORRECT address
svc 135 args=(0x2, 0xb9,     0x0, 0x8)   set = 185
bb=0x101010101010101                     fatal
```

**`0x40057c` is `cbz w0, 4005c4`, the instruction immediately after
`bl __sigsetjmp`.** So `__longjmp` returned to exactly the right place. The
`cbz` then TOOK the branch, so `w0` was **0** at that point -- and `__longjmp`'s
tail is `cmp x1,#0; mov x0,#1; csel x0,x1,x0,ne`, which can only produce `val` or
`1`. **Never 0.**

So main re-entered its `sigsetjmp(...) == 0` body and called `siglongjmp` a
SECOND time. By then x19 = 1, proved arithmetically: `__libc_siglongjmp` computes
`add x1, x19, #0xb8`, and the traced `set` is `0xb9` = 1 + 184. A jmp_buf pointer
of 1 makes `ldp x29, x4, [x0, #80]` read arena[81..96] -- zeros -- so
`eor x30, x4, x3` yields the guard, and the branch target is
`0x0101010101010101`.

Every step from "w0 is 0" to the fatal is now accounted for. **The one remaining
unknown is why `w0` is 0** at the return, on the path where `sigprocmask` ran
first and only there.

⭐ **The next test is one line of instrumentation**: read `w0` at `0x40057c`. A
guest cannot easily do that, but ecvisor can -- `RAPTORMARK_ECV_COUNTRET` and the
`dtrace` machinery already hook on return addresses, and the same hook can print
`x0`. That distinguishes the two remaining candidates: the `svc` trampoline not
restoring `x0` on this path, versus `br x30` not carrying it.

⚠️ Six hypotheses have now been killed by measurement here. Recording that the
ones that FELT most likely -- the pointer guard, the callee-saved clobber, the
backward-branch CFG -- were all wrong, and the answer came from a syscall trace
rather than from reading code.

### CORRECTION: `w0` is NOT 0. The state that is wrong is CALLEE-SAVED.

The previous entry concluded "`w0` is 0 at the return". **That was wrong, and a
guest that simply prints the value disproved it:**

```
pass 1: sigsetjmp returned 0
pass 2: sigsetjmp returned 9      <- w0 is CORRECT
[ecvisor] fatal: _ecv_unreached hit. value: 0x101010101010101
```

`__longjmp` returns to the right address WITH the right value. The program then
prints, and calls `siglongjmp` AGAIN -- from inside its own `if (v == 0)` body,
with `v` demonstrably 9.

`main`'s disassembly says why. `v` lives in **w20**:

```
400538: bl  __sigsetjmp
40053c: ...                       <- longjmp returns here
400544: mov w20, w0               <- v = 9
400560: bl  _IO_printf            <- prints "9"
400570: bl  _IO_fflush
400574: cbnz w20, <ok path>       <- did NOT branch, so w20 read as 0
```

So w20 is set to 9, printf proves it, and the `cbnz` a few instructions later
reads 0.

⚠️ **A bisect that moved the check before the calls is NOT clean evidence** and I
am recording it as such: it made `siglongjmp` the function's last statement, so
GCC tail-called it, which changes the frame and the callee-saved restore. Two
variables moved at once. The result (fails earlier) therefore says nothing about
whether the calls matter.

### Where it stands

**The live statement:** after `__longjmp`'s `br x30` dispatches back into `main`
(a `bbmiss` -> catchall lookup), the CALLEE-SAVED register state is not what the
guest wrote. That is an indirect-branch / register-writeback question at the
lifter-runtime interface, not a syscall, not arithmetic, and not the pointer
guard.

Everything downstream follows from it: the re-entered branch calls `siglongjmp`
a second time, x19 is then 1 (restored from `jb[0]`, and the traced
`rt_sigprocmask` set of `0xb9` = 1 + `#0xb8` proves it), so `__longjmp` reads a
null jmp_buf and branches to the guard.

⭐ **Next**: a guest that sets a callee-saved register, longjmps, and reads it
back WITHOUT any intervening call and WITHOUT letting the longjmp become a tail
call. That isolates "the dispatch loses callee-saved state" from "a call between
the two loses it", which the flawed bisect above conflated.

⚠️ Seven hypotheses killed by measurement now, including two of my own
conclusions from earlier in this same investigation. The pattern worth keeping:
every time I reasoned from a disassembly instead of printing a value, I was
wrong; every time I printed the value, it moved.

### Isolated: the register state is lost AT THE DISPATCH, not across calls

Two runs settle what the flawed bisect could not.

**Calls are exonerated.** A program with no `setjmp` at all that keeps a value in
a callee-saved register across `printf`+`fflush`:

```
native : callee-saved across a call: want 9 got 9  OK
ecvisor: callee-saved across a call: want 9 got 9  OK
```

**The dispatch is not.** `nocall.c` -- 12 lines, the jump in a `noinline` helper
so `main` cannot tail-call it, and `v` tested with NO call between the longjmp
landing and the read:

```
native : NOCALL OK v=9
ecvisor: [ecvisor] fatal: _ecv_unreached hit. value: 0x101010101010101
```

### The mechanism, as precisely as the evidence supports

`__longjmp`'s `br x30` is an indirect branch whose target is not statically
known, so it resolves through the `bbmiss` -> catchall dispatch back into
`main`'s lifted body. `main` then executes, at the landing site,

```
40053c:  adrp x19, ...     \
400540:  add  x19, ...      |  reached by DISPATCH
400544:  mov  w20, w0       /  v = 9   (printf proves w0 is 9 here)
...
400574:  cbnz w20, <ok>        reads 0
```

The write happens in the dispatched block; the read happens in a later block; the
value does not survive between them. With the calls exonerated, what is left is
the register-state handoff at the catchall re-entry -- a lifter/runtime interface
question, not a syscall, not arithmetic, and not the pointer guard.

⚠️ **This is where the evidence stops.** Whether the defect is a missing
writeback before the dispatch, a stale reload after it, or something about how
the catchall block is generated, is not established, and the next step is to read
the generated dispatch rather than to guess -- the last several hypotheses here
died precisely because they were reasoned rather than measured.

⚠️ **Cost note for whoever takes it**: the fix is an `elfconv` patch, which moves
`BASE_ID` and re-translates everything -- 7h25m for postgres:17, plus the object
cache. Worth batching with the by-element 2S multiply (the fourth lifter target,
still unwitnessed) if that ever gets a witness.

### The reproducers, all `gcc -static -O2 -fno-stack-protector -U_FORTIFY_SOURCE`

  ljmp/nocall.c   12 lines, minimal    -> fatal
  ljmp/retval.c   prints the value     -> shows w0 is CORRECT (9), then loops
  ljmp/variants.c _longjmp vs mask 0/1 -> only mask=1 fails
  ljmp/csave.c    no setjmp            -> passes, exonerating calls

### A JMP diagnostic, and two more of my own readings corrected

Added a gated register dump to `__remill_jump` (`runtime/src/intrinsics.rs`).
That function is where every `longjmp` lands -- a `br` to a target elflift did not
discover for the branching function -- and nothing could see it: the existing
report prints registers only on a MISS, and a longjmp is a HIT.

⚠️ **Two mistakes writing it, both caught by the tree's own guards.** I used
`eprintln!`, and `diag`'s `only_the_fatal_path_prints_without_going_through_the_sink`
failed (8 sites -> 9): raw printing is reserved for the fatal path. Switched to
`ecv_debug!`. And I again wrote `cargo check … | grep error; echo OK`, whose echo
runs unconditionally -- the same lying check I had criticised myself for one
entry earlier. Both checks are now `if`-guarded.

### What the instrument shows, and what it corrects

```
[bbmiss] fn=0x405540 bb=0x400538
[ecv] JMP t_vma=0x400538 lr=0x400538 x0=0x9  x19=0x1 x20=0x17fffd98 sp=0x17fffbd0
[ecv] JMP t_vma=0x41de40 lr=0x40c6a0 x0=0x458ab8 x19=0x1 x20=0x491000 sp=0x17fffb90
[bbmiss] fn=0x405540 bb=0x101010101010101
[ecv] JMP t_vma=0x101010101010101 lr=0x101010101010101 x0=0x17fffd98 x19=0x0 x20=0x0 sp=0x101010101010101
```

❌ **The landing is CORRECT.** `bl __sigsetjmp` is at `0x400534`; the target
`0x400538` is the `cbz w0` immediately after it, and **x0 = 9**. So the previous
entry's "the callee-saved state is lost at the dispatch" is ALSO not established
-- x19/x20 at the landing are `jb[0]`/`jb[1]`, exactly what `__longjmp`'s
`ldp x19, x20, [x0]` is supposed to restore.

⭐ **What is new: at the fatal, `sp` IS the pointer guard** --
`sp=0x0101010101010101`. `__longjmp` sets `mov sp, x5` where `x5 = jb[13] ^
guard`, so a guard-valued SP means `jb[13]` read as **zero**. The second longjmp
read a ZEROED jmp_buf, which is the same `0 ^ guard` signature as the branch
target and confirms the two share one cause: by then the buffer, not the
restore, is empty.

⚠️ So the open question has MOVED again: not "why is the restore wrong" but
**"why does a second longjmp happen at all, and why is the buffer zero by then"**.
`puts` did run before the fatal (`lr=0x40c6a0` is inside it), so main reached its
output path; its text is simply lost in the unflushed buffer when the process
dies.

⚠️ **Three of my own conclusions have now been overturned by the next
measurement** in this one investigation -- "w0 is 0", "callee-saved state is lost
at the dispatch", and before those the pointer-guard hypothesis. Each felt solid
and each came from reading a disassembly instead of printing a value. The
diagnostic added here is the durable part: it makes the register state at every
indirect branch observable, which is what every one of those mistakes lacked.

## 2026-09-01 setjmp/longjmp: root cause found, and it is not the mask

**CORRECTION to every earlier entry in this investigation that framed the defect
as "only the mask-restoring `siglongjmp` fails".** That correlation was an
artifact of the reproducer. `variants.c` runs `_longjmp`, `siglongjmp(0)` and
`siglongjmp(1)` in that order, and only the LAST one is followed by `main`
returning. The mask was never the variable.

Two new reproducers separate the two, and they invert the old reading:

| repro | mask | does the jumped-to code RETURN? | result |
|---|---|---|---|
| `mask0.c` | 0 -- the "working" form | yes (`return 0`) | ❌ FAILS |
| `noret.c` | 1 -- the "failing" form | no (`_exit(0)`) | ✅ PASSES |

`mask0` prints `MASK0 OK` and only THEN dies, which is the whole story: the
landing is correct, `main` runs to completion, and the fault is in what happens
when it RETURNS.

### The mechanism, exactly

`__remill_jump` implements an indirect branch as a NESTED CALL --
`f(arena_ptr, state, t_vma, ctx)` -- and then returns. `elflift` emits it via
`AddTerminatingTailCall` (`TraceLifter.cpp:987`), which on wasm32 without the
tail-call proposal is a plain call followed by `ret void`.

So `__longjmp`'s `br x30` RETURNS. Disassembling the caller shows what it
returns into (`__libc_siglongjmp`, 0x4054c0, glibc static aarch64):

```
4054dc: cbnz w0, 4054f0        ; if mask_was_saved -> restore block
4054e0: cmp  w20, #0
4054e8: csinc w1, w20, wzr, ne
4054ec: bl   __longjmp         ; NEVER returns natively
4054f0: add  x1, x19, #0xb8    ; <- FALL-THROUGH, reachable natively ONLY via the cbnz
4054fc: bl   __sigprocmask
405500: b    4054e0            ; <- straight back to the bl __longjmp
```

The fall-through target of a call that never returns is the mask-restore block,
and it branches back to the call. That is the loop, and it accounts for both
unexplained observations: the second `rt_sigprocmask(set=0xb9)` is
`add x1, x19, #0xb8` with `x19 == 1` (whatever `main` left), and the fatal
`sp = 0x0101010101010101` is `0 ^ pointer_guard` -- the second `__longjmp`
reading a jmp_buf that is by then zero.

### Why a tail call is fine and this is not

The invariant is **host frames popped == guest frames abandoned**. A tail-call
`br` abandons one frame and pops one (`__remill_jump` returns, the lifted
function's `ret void` pops it) -- correct, which is why every other `br` works.
A longjmp abandons every frame between the jump site and the `setjmp` frame, and
still pops one. Nothing in the nested-call dispatch can express that, so this is
not a lifting bug in any instruction; `probe.c` was right that `ldp`/`ldr`/`eor`
lift fine.

### The fix is runtime-only, and the machinery already exists

`context.rs`'s `Replay { cur, remaining, resuming }` is precisely the shape
needed: `cur = (containing_func(t_vma), t_vma)`, `remaining = call_history[..d]`
where `d` is the depth of the frame containing `t_vma`, `resuming: false`.
Patch 0026 already skips `_ecv_func_epilogue` on the unwind path specifically so
that `call_history` survives for replay, and the suspend check it emits after
every direct and indirect call is what carries the unwind up -- including the
one right after `bl __longjmp`, which is what makes the guest SKIP the
fall-through block instead of executing it.

So `__remill_jump` can detect "the target lies inside a function already on
`call_history`" -> non-local jump -> set the replay, `set_unwinding(true)`, and
return WITHOUT calling `f`. No lifter change, so no `BASE_ID` move and no
re-translation.

## 2026-09-01 setjmp/longjmp: FIXED, runtime-only

`__remill_jump` now classifies its target before dispatching
(`context::nonlocal_jump_depth`):

* target is a function ENTRY -> **tail call**. Unchanged: it abandons one guest
  frame and the nested call pops one, which is why every other `br` has always
  worked. The entry check runs FIRST, so a self-recursive tail call -- whose own
  entry IS on the history -- cannot be misread.
* target is INSIDE a function already on `call_history` -> **non-local jump**.
  `begin_nonlocal_jump` records a `Replay { cur: (fn, t_vma), remaining:
  call_history[..d], resuming: false }`, truncates `call_history` to `d` so the
  two stay the same length, and sets `longjmp_pending`; `__remill_jump` then
  raises `unwinding` and RETURNS WITHOUT CALLING. Every frame returns off its
  suspend check -- including the one right after `bl __longjmp`, which is what
  makes the guest skip the mask-restore block instead of falling into it.
* otherwise -> unchanged dispatch.

`entry.rs`'s leg loop re-enters the process on `longjmp_pending` **before** the
`suspended` arm, deliberately NOT through `schedule_after_suspend`: a longjmp is
not a yield point, and `retire_after_suspend` would read it as a process that
yielded with nothing pending.

❗ **No lifter change**, so `BASE_ID` and `TranslateSH` are untouched and every
cached object stays valid. Verified on the images: `raptormark-builder:ljfix`
carries the same two labels as `:jmpdiag` and a DIFFERENT
`/opt/ecvisor/libecvisor.a` (`bbe2df5a…` vs `a5d99bc9…`).

### Result

All five reproducers pass, where three failed before:

    nocall   NOCALL OK v=9                                    rc=0
    mask0    MASK0 OK                                         rc=0
    noret    MASK1 NORETURN OK                                rc=0
    retval   pass 1: 0 / pass 2: 9 / OK v=9                   rc=0
    variants _LONGJMP OK / SIGLONGJMP0 OK / SIGLONGJMP1 OK    rc=0

### Neutralized, three ways

* **In the module.** `nonlocal_jump_depth` made to return `None` (compiles and
  runs -- not a compile error), rebuilt as `raptormark-builder:ljneut`:
  `nocall`, `mask0` and `variants` all return to
  `_ecv_unreached hit. value: 0x101010101010101`, the identical diagnostic.
  Reverting rebuilt a BYTE-IDENTICAL `libecvisor.a`, which is what proves the
  revert was exact rather than merely green.
* **On the host**, `runtime/src/context.rs`'s `nonlocal_jump_tests`: dropping
  the entry check fails only `a_branch_to_a_function_entry_is_a_tail_call`;
  `rposition` -> `position` fails only the recursion test; dropping the
  `truncate` fails only the two lockstep tests.

### What this does NOT fix

⚠️ **Recursion is a guess.** `nonlocal_jump_depth` picks the INNERMOST frame of
the target function. Disambiguating needs a per-frame guest SP, and
`call_history` entries are written by the INLINED fast path (elfconv patch 0060)
straight into a wasm global as (func, ret) pairs -- widening them is a lifter
change. A `longjmp` out of a recursive function into an OUTER activation of
itself will still land in the wrong one. Recorded in
`recursion_resolves_to_the_innermost_matching_frame`, which exists so a later
fix has to come and change it deliberately.

## 2026-09-01 postgres:17 runs past the longjmp; the next blocker is `fchdir`

Relinked `postgres:17` against the fixed runtime -- **runtime-only, so all 152
objects were cache hits in ~90 s**, which is the labels-preserved claim actually
paying off rather than being asserted. Output
`.agents-workspace/pgbuild/out4/` (app.wasm 1,653,452,176 B, rootfs.img
165,032,858 B, 71 programs / 81 units).

**Zero longjmp failures.** The guest now boots, runs its `_dl_init`
constructors, execs through `env` and `bash`, forks, and reaches
`docker-entrypoint.sh`'s `find` at pid 7/8 -- where it stops on something else:

    find: Failed to change directory: Function not implemented
    find: Failed to restore initial working directory: Function not implemented

⚠️ **Measured, not inferred, and the first attempt at inferring it was not
admissible.** The first run showed no `ENOSYS syscall` line, which looked like
evidence that no syscall was missing -- but `ecv_debug!` is gated on
`RAPTORMARK_ECV_DEBUG`, which that run did not set, so the absence proved
nothing. (The `[ecvisor]` ifunc/`_dl_init` lines that made the gate look on come
from elsewhere.) Re-run with `-e RAPTORMARK_ECV_DEBUG=1` -- `wasmedge` inherits
no host environment -- gives:

    8 x ENOSYS syscall 43   (statfs)
    2 x ENOSYS syscall 50   (fchdir)

Two `fchdir` calls for `find`'s two messages. aarch64 has `chdir` (49)
implemented in `sys.rs` and `fchdir` (50) absent.

Note `resolve_arg` also refuses a relative path against a non-`AT_FDCWD` dirfd
("M1 supports AT_FDCWD and absolute paths only"). That did NOT fire here, but
`find`'s traversal is the classic caller of it, so it is likely the blocker
immediately after `fchdir`.

## 2026-09-01 Directory descriptors: `fchdir`, and relative paths against a dirfd

Both blockers found after the longjmp fix turned out to have ONE root:
**`OpenFile::Dir` did not record which directory it was.** With no path on the
descriptor, `fchdir` is unimplementable and `resolve_arg` can only refuse.

Fixed by adding `path: Vec<u8>` to `OpenFile::Dir` (set from `Vfs::resolve`'s
canonical result at the one construction site), then:

* `EcvContext::dir_fd_path(dirfd)` -- the directory an fd is open on;
* `EcvContext::resolve_base(dirfd, path)` -- cwd for an absolute path or
  `AT_FDCWD`, otherwise the dirfd's directory;
* `sys_fchdir` (syscall 50), which previously fell through to the ENOSYS
  catch-all.

### ❗ The refusal was in FOUR places, not one

`grep` for the guard found `resolve_arg` **and** `sys_unlinkat` (ENOENT),
`sys_readlinkat` (EINVAL), `sys_mkdirat` (EINVAL) -- each with its own copy and
its own errno. Fixing `resolve_arg` alone would have left three, and the next
symptom would have read as an unrelated bug. All four now go through
`resolve_base`, each keeping its original errno for the case it still cannot
serve (an unusable dirfd).

### ⚠️ A test module that compiled NOWHERE

The first attempt put host tests in `sys.rs`. They ran as "0 tests, 320 filtered
out" -- because **`mod sys` is `#[cfg(target_arch = "wasm32")]`**, so that file
is not compiled on the host at all and `cargo test` can never reach it. The
module was syntactically fine, committed nothing, and looked like coverage. That
is why `sys.rs` has no tests and why the DECISION was moved into `context.rs`,
which is host-compiled. Worth remembering as a general shape: a green
`cargo test` says nothing about a file the host build does not include.

### ⚠️ A guard I documented as load-bearing was not

`dir_fd_path` originally carried `if dirfd < 0 { return None }` with a comment
saying "only the explicit `< 0` guard stops" a negative fd becoming a huge
index. Neutralizing it changed NO test and NO behaviour: `self.fds` is a `Vec`
and `Vec::get` already returns `None` for an out-of-range index. The guard is
gone and the comment corrected, rather than left as defence nothing observes.
Found only because the neutralization pass was run on a guard I was confident
about.

### Measured

`.agents-workspace/dirfd/dirwalk.c` -- needs no rfs sidecar (it builds its tree
in the tmpfs upper), so it lifts in seconds instead of relinking 1.65 GB:

| runtime | result |
|---|---|
| native, inside the builder image | `DIRWALK OK` |
| `raptormark-builder:ljfix` (before) | `DIRWALK FAILED 7` |
| `raptormark-builder:dirfd` (after) | `DIRWALK OK` |

7 -> 0, matching native line for line across `openat`, `fchdir`, `mkdirat`,
`unlinkat` and cwd-relative resolution. ⚠️ Its `unlinked name is gone` line
passes in the BEFORE column for the wrong reason -- `openat` had already failed
-- so it is evidence only when the lines above it are `ok`.

Host tests: `context::resolve_base_tests`, 7 of them, neutralized two ways that
fail (cwd fallback for a non-directory dirfd; ignoring the dirfd entirely) and
one that does not, which is the guard finding above.

## 2026-09-01 postgres:17 past `find`; the next wall is Go's 512 MiB reservation

Relinked with the dirfd fix (`raptormark-builder:dirfd`,
`.agents-workspace/pgbuild/out5`). The `find:` errors are GONE -- zero
occurrences, where the previous run had two -- and `ENOSYS syscall 50` is gone
with them. Only `statfs` (43) remains, 12 calls, and nothing has been shown to
fail on it.

The run now gets much further: 11 processes, 10 of them run and exit, and the
entrypoint reaches its final `exec gosu postgres ...`, which REPLACES pid 1.
`gosu` is a Go binary (`GOSU_VERSION=1.19` is in the boot env), and the Go
runtime's `mallocinit` asks for its arenas in a recognisable ladder before
dying:

```text
[mmap] pid=1 0xc800000..0xc840000  len=262144    flags=0x22
[mmap] pid=1 0xc840000..0xc860000  len=131072    flags=0x22
[mmap] pid=1 0xc860000..0xc960000  len=1048576   flags=0x22
[mmap] pid=1 0xc960000..0xd160000  len=8388608   flags=0x22
[mmap] pid=1 0xd160000..0x11160000 len=67108864  flags=0x22
[ecv]  pid=1 mmap region exhausted (want 536870912 bytes, bump 0x11160000, 0 hole(s)) -> ENOMEM
fatal error: failed to reserve page summary memory
```

### ❗ Growing the arena CANNOT fix this, and the arithmetic says so

| | now | to fit Go |
|---|---|---|
| mmap region | 152 MiB (73 used) | 585 MiB |
| arena | 384 MiB | 817 MiB |
| process buffers under the 4 GiB wasm32 ceiling | 10 | **5** |

postgres needs **7** process buffers for one `psql`. So growing the region to
satisfy `gosu` takes the process ceiling below what postgres itself needs -- it
trades one failure for another rather than removing one. This is the first time
the two constraints have been shown to be in direct conflict, and it is why
"just grow it again" is not the follow-up to the 2026-08-2x growth.

### What the shape of a real fix looks like

Go's 512 MiB is `sysReserve` -- **PROT_NONE address space that is never
touched**. Under the identity map, VMA *is* linear-memory offset, so a
reservation costs arena. But because it is never written, it also never diverges
between processes, so the promising direction is a reservation region that is
SHARED rather than duplicated per process buffer -- paying the 512 MiB once
against the 4 GiB ceiling instead of once per process. Not attempted; recorded
because the arithmetic above rules out the obvious alternative.

⚠️ Also: after the fatal, pid 1 blocks on `Sleep` with every other process
`Dead` and the scheduler idles forever rather than exiting, so the run ends on
the harness timeout (rc=124). A guest that dies should not leave the module
spinning; that is a separate defect from the reservation.

**CORRECTION to the paragraph above** (same day, before it was acted on). It
said pid 1 "blocks on `Sleep` ... and the scheduler idles forever", implying a
spin. Counted: there are exactly **two** `block on Sleep` lines and two
`IDLE (runq empty)` lines, after which the log is silent until the 500 s
timeout. That is a process PARKED, not a scheduler spinning, and the last guest
output is `runtime stack:` -- Go's traceback stopping after its first line. Why
it parks is not established, and the TODO entry now says so rather than naming a
cause.

## 2026-09-01 gosu standalone: a 90-second reproducer, and a second Go defect

`gosu` extracted from `postgres:17` and lifted on its own reproduces the whole
tail of the postgres run in 90 s instead of ~8 minutes. Kept, with the recipe,
in `.agents-workspace/govm/`.

### What happens after the reservation fails -- measured, not inferred

With `RAPTORMARK_ECV_TRACE=sched,svctramp`:

1. `mmap region exhausted (want 536870912) -> ENOMEM`;
2. 3 x `write` (nr=64) -- the fatal message;
3. 2 x `nanosleep` (nr=101), 1 ms each, each a clean block/IDLE/resume;
4. 1 x `write` of `"\nruntime stack:\n"`;
5. then **zero syscalls and zero scheduler lines for ~85 s**, to the timeout.

`svctramp` fires on every syscall and `sched` on every idle, so zero of both
while the module runs puts the guest inside lifted code. It is an infinite loop
in Go's traceback. Why is not established.

⚠️ **I characterised this wrongly TWICE before running the syscall trace** --
first as the scheduler "spinning" (it was idle), then as a process "parked in a
long sleep" (the sleeps are 1 ms and there are two). Both readings came off the
`[sched]` lines alone. The sched log shows what the SCHEDULER did; it is silent
about a guest that never yields, which is exactly the case here.

### A second, unrelated Go defect: the lifter aborts on `stlxrb ..., wzr, ...`

A hello-world built with the tree's own Go (1.26.5) does not lift at all:

```text
Check failed: op_type == arg_type ... Lifted operand (READ_OP (REG_32 WZR)) to
STLXRB_SR32_LDSTEXCL does not have the correct type. Expected i8 but got i32.
arg_num: 2, address: 162792
```

❗ **The obvious explanation is wrong.** It is not "newer Go emits new
instructions": both binaries carry `stlxrb`/`ldaxrb` (48 in gosu, 66 in the new
one) and LSE atomics (285 vs 205). The discriminator is the ZERO REGISTER as the
byte operand -- `stlxrb Ws, wzr, [Xn]`, encoding `0x081bfc1f`: gosu has 0 of
them, the new binary has 3, and the assertion's `address: 162792` is `0x27BE8`,
the first one exactly. So a normal register is narrowed to i8 somewhere the WZR
path is not.

That is a real encoding for a lifter test, as AGENTS.md asks. Not fixed: it is a
patch to `third_party/elfconv`, which moves `BASE_ID` and costs a full
re-translation, and nothing currently in the postgres closure needs it.

## 2026-09-01 Where Go's traceback stops: a function name of length -32

Follow-up to the entry above, which left "why it loops" open. Armed the existing
differential call tracer over gosu's whole text
(`RAPTORMARK_ECV_DTRACE_LO=0x11000 RAPTORMARK_ECV_DTRACE_HI=0xba8f4`) and
resolved the frames against gosu's symbol table (it is not stripped).

**It is not a call-heavy loop**: 395 traced calls in 25 s, then nothing. The
final chain, with `x0..x2` being the CALLEE's arguments (`intrinsics.rs:738`):

```text
traceback2             -> printFuncName(0x11f589, 0xffffffffffffffe0, 0x44d)
printFuncName          -> funcNamePiecesForPrint(0x11f589, 0xffffffffffffffe0, 0x44d)
funcNamePiecesForPrint -> ...(0x5b, 0xffffffffffffffe0, 0x44d)
```

A Go string is `(ptr, len)`. **The function-name string has length -32**
(`0xffffffffffffffe0`), and `runtime.findnull` -- which returns the length of a
NUL-terminated name -- returned exactly that value immediately before. A print
loop over a negative length does not terminate, which matches the observed
silence: no syscalls, no new basic blocks, no scheduler activity.

The two 1 ms sleeps seen earlier are now also placed: `runtime.freezetheworld`
is at `0x4bbb0` and the replayed PCs were `0x4bc68`/`0x4bc78`, inside it. That
is its `usleep(1000)`, behaving correctly.

### ⚠️ What is measured and what is not

MEASURED: the chain above, the -32, and that `findnull` produced it.
NOT ESTABLISHED: why `findnull` returns -32. It scans for a NUL byte and cannot
return a negative length for well-formed input, so either the name pointer
(`0x11f589`, in rodata) is wrong, or the region it scans carries no NUL and the
scan wrapped. Distinguishing them means dumping guest memory at `0x11f589` and
checking it against the `funcnametab` in the ELF -- a bounded next step, not
done here.

If the second is true it would mean Go's pclntab name table is not correctly
materialised in the lifted image, which would make EVERY Go panic hang rather
than print -- worth knowing independently of the 512 MiB reservation, since that
reservation is only how this particular guest got to a panic.

## 2026-09-01 The -32 comes out of `bytealg.IndexByteString`, not a bad pclntab

**CORRECTION to the entry above.** It proposed that Go's `.gopclntab` name table
might not be materialised in the lifted image, and said that if so "EVERY Go
panic hangs". **Measured, and it is false.** The table is fine:

* `0x11f589` is inside `.gopclntab` (vma `0x1128a0`, size `0x7f1d8`), which sits
  in a `PT_LOAD` (`vaddr=0xc0000`, `filesz=memsz=0xd1a78`, RO);
* the ELF holds `"runtime.throw\0"` there, NUL at offset 13;
* and the GUEST holds the same bytes. Dumped with
  `RAPTORMARK_ECV_DTRACE_DUMPX0=30` at the call site:

```text
[dtrace] CALL ret=0x6b0b8 (from fn 0x6b020) x0=0x11f589 x1=0xffffffffffffffe0
[dtrace] DUMPX0 @0x11f589 [30]: 72756e74696d652e7468726f77006f732e72756e74696d655f6265666f72
```

`72756e74696d652e7468726f7700` is exactly `runtime.throw\0`. **The bytes are
right and the LENGTH is wrong** -- so the fault is in whatever computes it.

⚠️ Two of my readings of this log were wrong before this one, both from not
checking what a tracer field means. `callsite ret=X (in fn Y) x0=Z` is the
EPILOGUE hook (`intrinsics.rs:866`): `Y` is the CALLER and `Z` is the CALLEE's
return value -- not, as I first read it, the function that returned Z.

### Localised

`runtime.findnull` reaches the scan through, at `0x649e4`:

```text
649e4: 97feb627  bl  12280 <internal/bytealg.IndexByteString.abi0>
649e8: f94013e0  ldr x0, [sp, #32]      ; abi0 returns on the STACK
649ec: b100041f  cmn x0, #0x1           ; compare against -1
```

which matches the traced `callsite ret=0x649e8` exactly. `IndexByteString.abi0`
is hand-written NEON. For `"runtime.throw\0"` it must return 13; the length that
reaches `printFuncName` is `-32` (`-0x20`), and note it is NOT `-1`, the
"not found" answer.

### Ruled out

`RAPTORMARK_ECV_UNDEC_CENSUS=1` reports **no `addr=` lines** -- no undecoded
instruction was executed. So this is not the skip-an-instruction failure mode;
every instruction decoded, and one of them is presumably lifted with wrong
semantics.

### Not established, and the bounded next step

Whether `IndexByteString.abi0` computes the wrong index, or whether `[sp,#32]`
is never written and `findnull` reads stale stack. Distinguishing them is the
differential-trace workflow the tracer was built for (`intrinsics.rs` names
`nat_trace.py`): trace `0x12280` under ecvisor and under native gdb with the
same input and diff the first divergence.

⚠️ This matters well beyond gosu -- `IndexByteString` is on the path of ordinary
Go string handling, not just panics. If it is wrong, Go guests are unreliable in
a way that has nothing to do with the 512 MiB reservation.

## 2026-09-01 ROOT CAUSE: `ld1 {v..-v..}, [xN], #imm` never writes back

The -32 is not a Go problem and not a pclntab problem. Predicted from the
arithmetic, then measured.

`internal/bytealg.indexbytebody` computes its return value FROM the advanced
pointer, so the whole result depends on the post-index writeback:

```text
12184: and x3, x0, #0xffffffffffffffe0   ; align down (the mask IS -32)
121d4: ld1 {v1.16b-v2.16b}, [x3], #32
12230: sub x3, x3, #0x20
12238: add x0, x3, x6, lsr #1
1223c: sub x0, x0, x11                   ; index = advanced - original
```

Solving `x0 = (x3 - 32) + idx - x11 = -32` for a 32-byte-aligned pointer with a
match at index 0 requires `x3` never to have been incremented. ❗ And the two
store paths (`12240`, `1224c`) can only write a non-negative index or `-1`, so
`-32` was already proof the pointer arithmetic was wrong rather than the search.

`.agents-workspace/govm/ld1wb.c` tests it directly:

| form | native | ecvisor |
|---|---|---|
| `ld1 {v1.16b}, [x], #16` | +16 | +16 ok |
| `ld1 {v1.16b-v2.16b}, [x], #32` | +32 | **+0** |
| `ld1 {v1.16b-v3.16b}, [x], #48` | +48 | **+0** |
| `ld1 {v1.16b-v4.16b}, [x], #64` | +64 | **+0** |
| 2-reg loads `v2` | yes | **no** |

**Only the single-register form is correct.** The LD1 multiple-structures
post-index forms neither advance the base register nor load the second and later
vectors.

### Why nothing caught this

It is the SILENT failure mode this tree keeps paying for. The instructions
DECODE -- `RAPTORMARK_ECV_UNDEC_CENSUS=1` reports no executed-undecoded
instruction in gosu -- so there is no `_ecv_unreached`, no census line, no
warning. The lift succeeds, the module runs, and a pointer is quietly 32 bytes
short. Patch 0068 fixed the operand ORDER of the LD1 SINGLE-ELEMENT post-index
forms; this is the MULTIPLE-STRUCTURES family, a different set of decoders,
and it was not covered.

### Scope, which is wider than Go

`ld1 {v..-v..}` multi-register is ordinary optimised-string-function material.
Go reaches it through `bytealg`, so every `IndexByte`/`strings.Index`/`findnull`
on a Go guest is affected -- not just panics. ⚠️ Not yet checked: whether the
glibc/musl string routines in the existing fixtures use these forms. If they do,
they have been wrong all along and the E2E suite does not notice; if they do
not, that is why the suite stayed green. **That check should come before any
patch**, because it decides whether this is a Go-only defect or a general one.

### Cost

A fix is a patch to `third_party/elfconv`, which moves `BASE_ID` and invalidates
every cached object. But it can be VALIDATED cheaply on a side-tagged base:
`ld1wb.c` lifts in seconds, so the patch can be proven without re-translating
postgres. Batching it with the `stlxrb ..., wzr` defect is the obvious move --
one `BASE_ID` move buying both, which is the choice made earlier this session
for the previous lifter round.

**Scope settled** (the check the entry above said must come first). Counting
multi-register LD1/ST1 with post-index writeback:

| binary | count |
|---|---|
| `gosu` (Go) | 14 |
| glibc, from `postgres:17` | **0** |
| musl, from `nginx:alpine` | **0** |

Neither libc uses the form. So this is a **Go-guest defect**, not a general one,
and that is exactly why the E2E suite stayed green through it: no C guest in the
fixture set executes a single one of these instructions. The suite is not weak
here, it simply has no Go guest -- which is itself the gap worth closing, and a
cheaper one than the patch.

## 2026-09-01 The full LD1/ST1 multiple-structures matrix (and a correction)

`ld1wb.c` was too coarse. `.agents-workspace/govm/ld1mat.c` walks the whole
family -- 1 to 4 registers, load and store, with and without post-index -- and
checks the DATA in every destination vector separately from the base-register
advance. Native: all 36 cases ok. Under ecvisor:

| LD1 form | data | writeback |
|---|---|---|
| 1-reg, no writeback | ok | -- |
| 1-reg, post-index | ok | ok |
| 2-reg, no writeback | ok | -- |
| **2-reg, post-index** | **ok** | **MISSING** |
| 3-reg, either form | all zero | missing |
| 4-reg, either form | all zero | missing |

| ST1 form | result |
|---|---|
| 1-reg, no writeback | ok |
| **1-reg, post-index** (`0x4c9f7314`) | **UNDECODED -- fatal** |

(The ST1 run stops at the first undecoded instruction, so the 2/3/4-register ST1
forms are still unmeasured.)

### CORRECTION

The entry above says the multi-register post-index forms "neither advance the
base register nor load the 2nd+ vectors". **The second half is wrong for the
2-register form**: its data is loaded correctly and only the writeback is
missing. The all-zero data belongs to the 3- and 4-register forms, which are
broken with or without post-index. `ld1wb.c`'s fifth check claimed otherwise;
it combined the load and a lane read in one asm block with overlapping
constraints, and `ld1mat.c` -- which reads each vector in its own output
operand -- is the one to believe.

This does not change the root cause. gosu's `indexbytebody` uses
`ld1 {v1.16b-v2.16b}, [x3], #32`, the 2-register post-index form, and the
missing WRITEBACK is exactly what makes `x3` short by 32 and the returned index
`-32`.

### Scope, re-checked against the sharper matrix

glibc's ONE multi-register instruction is `ld1 {v1.16b-v2.16b}, [x1]` at
`0xa41f0` -- 2-register, NO writeback, which is a form that WORKS. musl has
none. So the Go-only conclusion holds, and now for a specific reason rather than
a count: the single form libc uses is the one form of the family that is lifted
correctly.

### ⚠️ Method note

The first matrix run printed NOTHING and looked like "it died before running".
It had not: `printf` to a pipe is fully buffered and the undecoded-instruction
fatal discarded the buffer. `ld1mat.c` now calls
`setvbuf(stdout, NULL, _IONBF, 0)`. Absence of output was not evidence -- the
third time that shape has cost time in this session.

## 2026-09-01 LD1/ST1 multiple structures: the complete map, from the decoder source

Iterating run-by-run was going to take one lift per undecoded instruction, so
the remaining cells were settled from the source instead. In
`backend/remill/lib/Arch/AArch64/Semantics/DATAXFER.cpp`, a form is implemented
iff it has per-arrangement entries (`_16B`, `_8B`, `_2D`, ...); a bare name is
decoded-but-unimplemented. Counting them:

| form | sem entries | measured behaviour |
|---|---|---|
| `LD1_ASISDLSE_R1_1V`  (1-reg, no wb) | 8 | correct |
| `LD1_ASISDLSE_R2_2V`  (2-reg, no wb) | 16 | correct |
| `LD1_ASISDLSE_R3_3V`  (3-reg, no wb) | 8 | **data all ZERO** |
| `LD1_ASISDLSE_R4_4V`  (4-reg, no wb) | 8 | **data all ZERO** |
| `LD1_ASISDLSEP_I1_I1` (1-reg, imm wb) | 8 | correct |
| `LD1_ASISDLSEP_I2_I2` (2-reg, imm wb) | 8 | data ok, **WRITEBACK MISSING** |
| `LD1_ASISDLSEP_I3_I3` (3-reg, imm wb) | 8 | **data ZERO + no writeback** |
| `LD1_ASISDLSEP_I4_I4` (4-reg, imm wb) | 8 | **data ZERO + no writeback** |
| `ST1_ASISDLSE_R1_1V`  (1-reg, no wb) | 8 | correct |
| `ST1_ASISDLSE_R2_2V`  (2-reg, no wb) | 8 | correct |
| `ST1_ASISDLSE_R3_3V`  (3-reg, no wb) | **0** | **UNDECODED** (`0x4c00600a`) |
| `ST1_ASISDLSE_R4_4V`  (4-reg, no wb) | **0** | undecoded (predicted) |
| `ST1_ASISDLSEP_I1_I1` (1-reg, imm wb) | **0** | **UNDECODED** (`0x4c9f7314`) |
| `ST1_ASISDLSEP_I2_I2` (2-reg, imm wb) | 8 | correct, writeback included |
| `ST1_ASISDLSEP_I3_I3` (3-reg, imm wb) | **0** | **UNDECODED** (`0x4c9f62aa`) |
| `ST1_ASISDLSEP_I4_I4` (4-reg, imm wb) | **0** | undecoded (predicted) |
| `LD1/ST1_ASISDLSEP_R*_R*` (REGISTER wb, `[x], xM`) | **0** | undecoded (predicted, untested) |

Every one of the three encodings measured as undecoded has `sem entries = 0`,
and every form measured as working has entries -- so the source rule predicts
the measurements rather than merely agreeing with them. The remaining
`sem entries = 0` rows are therefore predictions, and are labelled as such.

### Two distinct defects, which want different fixes

1. **Missing semantics** (`sem entries = 0`): five ST1 forms and the whole
   register-post-index family. These fail LOUDLY at run time.
2. **Wrong semantics** (entries present, behaviour wrong): `LD1 I2` loses its
   writeback; `LD1 R3/R4/I3/I4` load nothing. These fail SILENTLY, and one of
   them is what hangs every Go guest.

⚠️ Note the asymmetry that makes guessing dangerous: `ST1_ASISDLSEP_I2_I2`
handles its writeback CORRECTLY while `LD1_ASISDLSEP_I2_I2`, the same shape on
the load side, does not.

Reproducers, all preserved in `.agents-workspace/govm/`: `ld1mat.c` (the 36-case
matrix), `st1rest.c` and `st1last.c` (the ST1 cells `ld1mat` cannot reach,
because it dies at the first undecoded instruction), `ld1wb.c` (kept, but see
the correction above -- its 5th check is unsound).

## 2026-09-01 CORRECTION: the "sem entries" table counted COMMENTED-OUT code

The table in the entry above is wrong in its middle column. Its counts came from
a grep for `<NAME>_16B`, which matches `// DEF_ISEL(LD1_ASISDLSEP_I2_I2_16B)`
just as happily as a live one. ❗ Every "8" it reported for a broken LD1 form was
eight COMMENTED-OUT lines. The conclusion it supported -- which forms are
implemented -- was right only by luck for the rows that were measured.

Re-extracted, anchoring on `^DEF_ISEL(` versus `^// *DEF_ISEL(`:

| state | forms |
|---|---|
| **ACTIVE** | `LD1_ASISDLSE_R1_1V`, `LD1_ASISDLSE_R2_2V`, `LD1_ASISDLSEP_I1_I1`, `ST1_ASISDLSE_R1_1V`, `ST1_ASISDLSE_R2_2V`, `ST1_ASISDLSEP_I2_I2` |
| **COMMENTED OUT** (code present, disabled) | `LD1_ASISDLSE_R3_3V`, `R4_4V`, `LD1_ASISDLSEP_I2_I2`, `I3_I3`, `I4_I4` |
| **NEVER WRITTEN** (no DEF_ISEL at all) | `ST1_ASISDLSE_R3_3V`, `R4_4V`, `ST1_ASISDLSEP_I1_I1`, `I3_I3`, `I4_I4`, and every `*_ASISDLSEP_R*_R*` (register post-index, `[x], xM`) |

That three-way split explains the two different failure modes, which the earlier
two-way split did not:

* **NEVER WRITTEN -> fatal `undecoded instruction`.** All three ST1 encodings
  measured as undecoded are in this row.
* **COMMENTED OUT -> silent wrong answer.** `ld1 {v-v},[x],#32` loads its data
  correctly and simply does not write back, which is exactly what you get if the
  decoder falls back to the ACTIVE no-writeback `LD1_ASISDLSE_R2_2V` for it. The
  3- and 4-register LD1s silently do nothing at all.

### ❗ The disabled code cannot just be uncommented

The commented pair-postindex macro is written against the OLD signature:

```c
// DEF_SEM(LD1_PAIR_POSTINDEX_##elem_size, VI128 dst1, VI128 dst2, S src,
//         R64 addr_reg, ADDR next_addr) {
//     LD1_PAIR_##elem_size(rt_m, state, dst1, dst2, src);
//     Write(addr_reg, Read(next_addr));   // explicit writeback
//   }
```

while the ACTIVE single-register one next to it uses the current convention and
performs no explicit writeback at all:

```c
DEF_SEM_T_RUN(LD1_SINGLE_POSTINDEX_##elem_size, S src) {
    return LD1_SINGLE_##elem_size(arena_ptr, rt_m, src);
}
```

So a patch has to port the disabled forms to `DEF_SEM_T_RUN` and to whatever now
performs the base-register update -- not restore two lines. Anyone estimating
this from "the code is already there, just commented" will be wrong.

⚠️ Method: this is the second time in two hours that a grep matching COMMENTED
text produced a confident wrong table. Anchor on `^DEF_ISEL(` -- or on whatever
makes the line live -- not on the symbol name.

## 2026-09-01 CORRECTION: the LD1 post-index forms are NO-OPS, not writeback bugs

`ld1mat.c` reported that `ld1 {v-v},[x],#32` loads its data correctly and only
loses the writeback. **That was a flaw in my test.** For each register count it
ran the no-writeback case first and the post-index case second, USING THE SAME
VECTOR REGISTERS -- so the first case left exactly the values the second
expected, and a no-op read as success.

`ld1chk.c` (preserved) removes the ambiguity: it `movi`s both vectors to zero
immediately before, and loads from `buf+64` so no earlier case could leave the
expected bytes. Native passes; under ecvisor all three checks fail:

```text
post-index vec 0 loaded   got 0x0000000000000000 want 0x4746454443424140
post-index vec 1 loaded   got 0x0000000000000000 want 0x5756555453525150
post-index writeback      got 0x0 want 0x20
```

**The instruction does nothing at all.** That is consistent with the source in a
way "writeback only" never was: `LD1_ASISDLSEP_I2_I2_*` has no live `DEF_ISEL`.

### The rule, now that the evidence agrees

| source state | behaviour |
|---|---|
| `DEF_ISEL` ACTIVE | correct |
| `DEF_ISEL` COMMENTED OUT (but `TryDecode*` exists) | **silent NO-OP** |
| no `DEF_ISEL` and no `TryDecode*` | **fatal `undecoded instruction`** |

So: `LD1_ASISDLSE_R3_3V`, `R4_4V`, `LD1_ASISDLSEP_I2_I2`, `I3_I3`, `I4_I4` are
silent no-ops; the five ST1 forms are fatal. Nothing is merely "missing its
writeback".

This makes the Go failure simpler than the earlier account: `indexbytebody`'s
`ld1` neither advances `x3` nor loads `v1`/`v2`, so the compare runs on stale
registers and reports a hit at index 0 of a pointer that never moved -- hence
`-32`.

⚠️ Method: reusing a register between a control case and the case under test
makes a no-op indistinguishable from success. The earlier matrix looked more
precise than it was BECAUSE it was finer-grained -- more rows, each less sound.

## 2026-09-01 patches/0072: LD1 pair post-index restored; Go tracebacks work

Wrote and validated `patches/0072-ld1-pair-postindex-semantics.patch`.

**The fix.** `LD1_ASISDLSEP_I2_I2_*` had no live `DEF_ISEL`. Its DECODER was
fine and symmetric with the store side, so the instruction decoded and lifted to
nothing. The patch adds the eight ISELs in BOTH arms of the
`#if defined(__x86_64__)` split (the branches name their pair helpers
differently -- `LD1_PAIR_8_128` vs `LD1_PAIR_8` -- so the ISELs cannot be
shared), delegating to the already-working pair load exactly as
`ST1_PAIR_POSTINDEX` delegates to `ST1_PAIR`.

⚠️ The dead code that was already there could NOT be uncommented: it used the
older `DEF_SEM(..., R64 addr_reg, ADDR next_addr)` signature with an explicit
`Write(addr_reg, Read(next_addr))`, which under the current convention -- where
`AddPostIndexMemOp` carries the writeback on the memory operand -- would have
updated the base register TWICE.

**Validated on a side build**, `raptormark-builder:ld1fix`, leaving
`raptormark-builder:latest` and every existing tag alone:

| reproducer | before (`:dirfd`) | after (`:ld1fix`) |
|---|---|---|
| `ld1chk.c` (2-reg post-index, zeroed vectors) | 3 FAIL | **OK** |
| `ld1wb.c` | 4 FAIL | 2 FAIL (3-reg, 4-reg only) |
| `ld1mat.c` | 2/3/4-reg rows FAIL | 2-reg rows PASS |

### ⭐ End to end: Go now produces a real traceback and EXITS

`gosu` under `:ld1fix` (rc=2, where before it hung to a 90 s timeout):

```text
fatal error: failed to reserve page summary memory
runtime stack:
runtime.throw({0xeb29f?, 0x23680?})
  runtime/panic.go:1101 +0x38
runtime.(*pageAlloc).sysInit(0x1b8248, 0x38?)
  runtime/mpagealloc_64bit.go:81 +0x11c
runtime.(*mheap).init(0x1b8240)
  runtime/mheap.go:775 +0x1d8
runtime.mallocinit()
  runtime/malloc.go:461 +0xd4
runtime.schedinit()
  runtime/proc.go:836 +0xe8
runtime.rt0_go()
  runtime/asm_arm64.s:86 +0xa4
```

Symbol names, files and line numbers all resolve -- which is the direct proof
that `findnull`/`IndexByte` now returns real lengths. It also CONFIRMS, from the
guest's own stack rather than by inference from an mmap ladder, that the
remaining blocker is `pageAlloc.sysInit` reserving 512 MiB.

### Deliberately NOT in this patch

* **LD1 3- and 4-register** (`R3_3V`, `R4_4V`, `I3_I3`, `I4_I4`): still silent
  no-ops. They need `TTriple`/`TQuad` return types that do not exist beside
  `TPair`, plus framework support for writing 3-4 destination registers. Bigger
  and riskier than restoring an alias.
* **Five ST1 forms** with no `TryDecode*` at all (`ST1_ASISDLSE_R3_3V`, `R4_4V`,
  `ST1_ASISDLSEP_I1_I1`, `I3_I3`, `I4_I4`) and the whole register-post-index
  family. These fail LOUDLY, so they are far less dangerous than what was fixed.
* **`stlxrb Ws, wzr, [Xn]`**, which still blocks lifting Go 1.26 binaries.

⚠️ `BASE_ID` moved, so every cached object is invalid under `:ld1fix`. Existing
tags and their caches are untouched; nothing needs re-translating unless you
choose to move to this builder.

## 2026-09-01 patches/0073 and 0074: the LD1/ST1 family is complete

### 0073 -- LD1 with 3 and 4 destination registers

❗ **The framework change I predicted was not needed.** I had written that these
"need `TTriple`/`TQuad` return types that do not exist, plus framework support
for writing 3-4 destination registers", and estimated them as materially bigger.
The second half was wrong: `VroInstructionLifter` already handles an N-valued
return generically --

```cpp
if (auto struct_ty = dyn_cast<StructType>(sema_inst->getType()))
  CHECK(struct_ty->getNumElements() == write_regs.size());
```

-- and then indexes the result once per destination. `TPair` is not
special-cased anywhere; it is a two-field packed struct and nothing more. So
adding `TTriple`/`TQuad` beside it WAS the whole job.

Defined once, outside the `#if defined(__x86_64__)` split the pair forms live
in: the x86 pair path returns an `_ecv_u<W>v2_t` vector via reinterpret_cast and
there is no v3/v4 type to mirror it with, while a plain struct suits both arms.

### 0074 -- ST1 with 1 (post-index), 3 and 4 registers

These differed from the LD1 gaps in kind, not degree: no `DEF_ISEL` **and** no
real decoder. `Decode.cpp` carried `return false` stubs, so the decoder REFUSED
and the instruction was fatal rather than a silent no-op -- the better failure,
and why they ranked below 0072 despite being more numerous. Stubs removed from
`Decode.cpp`, real decoders added to `Arch.cpp` (the convention the working
forms follow), semantics as `DEF_SEM_VOID_RUN` -- stores need no multi-value
return, so no `TTriple`/`TQuad` is involved on this side.

### Result: every reproducer passes

Against `raptormark-builder:simd` (patches 0072+0073+0074):

| reproducer | before this session | now |
|---|---|---|
| `ld1chk.c` | 3 FAIL | **OK** |
| `ld1wb.c` | 4 FAIL | **OK** |
| `ld1all.c` | 4 FAIL | **OK** |
| `st1rest.c` | fatal, undecoded | **OK** |
| `st1last.c` | fatal, undecoded | **OK** |
| `ld1mat.c` (36 cases) | fatal, undecoded | **OK** |

`ld1mat.c` completing is the one that matters: it is the full matrix, and it now
runs to the end instead of stopping at the first undecoded ST1.

`gosu` still prints a fully symbolised traceback and exits rc=2, unchanged from
0072 -- as expected, since its hot path is the 2-register form.

### What is still not covered

The `*_ASISDLSEP_R*_R*` register-post-index family (`[Xn], Xm`) for both LD1 and
ST1. It has no decoder either, so it fails loudly, and nothing measured in any
fixture uses it. `stlxrb Ws, wzr, [Xn]` is also still open and still blocks
lifting Go 1.26 binaries.

## 2026-09-01 patches/0075: a zero-register read must take the semantics' type

Not a `STLXRB` bug at all -- a general one in `LiftRegisterOperand`, which
special-cases `XZR`/`WZR` with an EARLY RETURN, before the size adjustment that
every real register falls through to:

```cpp
if (31 == op.reg.number) {
  if ("XZR" == op.reg.name) return ConstantInt::get(getInt64Ty(context), 0);
  else if ("WZR" == op.reg.name) return ConstantInt::get(getInt32Ty(context), 0);
}
auto val = LoadRegValue(...);
...
} else if (val_size > arg_size) {
  val = new llvm::TruncInst(val, arg_type, ...);   // real registers only
```

`STLXRB` is `STLXR<R8, M8W>` and its decoder adds `kRegW` for Rt, so:

    stlxrb w27, w5,  [x0]   i32 loaded, TRUNCATED to i8   -- fine
    stlxrb w27, wzr, [x0]   i32 constant, never truncated -- FATAL

which is the whole of "Expected i8 but got i32". The fix takes `arg_type` when
it is an integer; zero is zero at every width, and for i64/i32 parameters it
reproduces the old constant exactly, so the wide cases cannot change.

⚠️ **Wider than the one instruction.** Every byte- and halfword-width semantic
that can take the zero register had the same hole -- `STXRB`, `STLXRH`, `STXRH`
and the plain `STRB`/`STRH`/`STLRB`/`STLRH` family among them.

### Measured

| binary | before (`:simd`) | after (`:zreg`) |
|---|---|---|
| `gohello` (Go 1.26, 3x `stlxrb ..., wzr`) | elflift ABORTS | **lifts, 5.41 MiB** |
| `ld1mat` / `st1last` / `ld1chk` | OK | **OK** (no regression) |
| `gosu` (Go 1.24) | traceback + rc=2 | **unchanged** |

### The next Go 1.26 gap, not taken

`gohello` now lifts and runs, and stops on a DIFFERENT instruction:

```text
1ef44: 4d60e940  ld4r {v0.4s-v3.4s}, [x10]   in internal/chacha8rand.block
```

`LD4R` is load-and-REPLICATE -- a separate family from the multiple-structures
forms 0072-0074 covered, needing its own semantics. Both binaries carry four
`ld[1234]r`; gosu never reaches its own, because it dies in `mallocinit` first,
which is why the postgres path is unaffected.

Stopping here rather than chasing it: each fix has revealed the next
instruction, and how far to walk that chain is a scoping decision, not a
technical one.

## 2026-09-01 E2E gate for the lifter patches

`raptormark-builder:simd` (patches 0072+0073+0074), cold object cache because
`BASE_ID` moved:

```text
PASS 121  FAIL 0  SKIP 35   ok  raptormark/e2e  3009.972s
```

Identical to the pre-lifter baseline taken earlier the same day on
`:dirfd` -- same pass count, same skip count, and a set difference of the
passing test NAMES is EMPTY, so nothing was lost and nothing merely traded one
failure for another. ⚠️ Comparing counts alone would not have shown that; the
name-set comparison is what makes "nothing lost" a measurement rather than an
inference from two equal integers.

Patch 0075 is gated separately, on `:zreg`. Its risk profile is different from
0072-0074, which only ADD previously-absent ISELs: 0075 changes a path taken by
every `XZR`/`WZR` READ, which is one of the most common operands there is. The
argument that it is safe is that for every case the old code did NOT crash on,
`ConstantInt::get(arg_type, 0)` produces the identical constant -- i64 zero for
an i64 parameter, i32 zero for an i32 one -- so only the previously-fatal
narrower cases can behave differently. That is an argument, not a measurement,
which is why the run is being done anyway.

## 2026-09-01 patches/0076 and 0077: the replicate family, and Go 1.26 works

`LD<n>R` loads N consecutive elements and REPLICATES each across its own
destination register -- a different family from the multiple-structures forms of
0072-0074, which spread one contiguous block across the registers. Only
`LD1R_ASISDLSO_R1` had ever been implemented; every other form was a
`return false` stub in `Decode.cpp`, i.e. fatal rather than silent.

* **0076** -- the plain forms `LD2R/LD3R/LD4R_ASISDLSO_R<n>`. The multi-value
  returns reuse `TPair` and the `TTriple`/`TQuad` added in 0073.
* **0077** -- the post-index forms `LD<n>R_ASISDLSOP_R<n>_I`, all four including
  `LD1R`'s, which had never been implemented either. Semantics are SHARED with
  the plain forms: the base-register update rides on the memory operand.

⚠️ In both, the memory operand keeps the SINGLE-element size while the advance
is `num_regs * elem_bytes`. `ld4r {v4.4s-v7.4s}, [x0], #16` reads four 4-byte
elements and advances 16; passing the total as the access size would give the
operand a type the single-element semantics does not take.

### ❗ The census beat chasing fatals one at a time

After 0076 the Go binary moved four instructions and died again. Instead of
another rebuild per address, `RAPTORMARK_ECV_UNDEC_CENSUS=1` -- which SKIPS
undecoded instructions and reports them all, unsound but with trustworthy
`addr=` lines -- showed the whole remaining set at once: exactly two, both
`ld4r ..., [x0], #16`. One patch closed both.

### Result: Go 1.26 lifts, runs, and reports

`gohello` (Go 1.26.5) under `:gofull`:

```text
fatal error: failed to reserve page summary memory
runtime stack:
runtime.throw({0xdc540?, 0x20000000?})
  .../go/1.26.5/src/runtime/panic.go:1229 +0x38
runtime.(*pageAlloc).sysInit(0x1a8248, 0x40?)
  .../go/1.26.5/src/runtime/mpagealloc_64bit.go:81 +0x118
```

Full symbolisation with source paths and line numbers. It stops at the same
512 MiB page-summary reservation `gosu` does -- an ARCHITECTURAL limit, not a
lifting gap. At the instruction level Go 1.26 is now fully served.

`ldnr.c` (preserved, 19 checks) covers `ld2r`/`ld3r`/`ld4r` in `.8h`/`.4s`/`.2d`,
the upper lanes -- which a partial replicate would fail -- and the post-index
data plus writeback. Native and `:gofull` both print `LDNR OK`; `:zreg` (before
0076) dies undecoded.

### Still not covered

The register-offset forms for both families: `*_ASISDLSOP_RX<n>_R` (replicate)
and `*_ASISDLSEP_R*_R*` (multiple structures), i.e. `[Xn], Xm`. Both keep their
stubs, both fail loudly, and nothing measured in any fixture executes one.

**E2E gate for 0075** (`raptormark-builder:zreg`, cold cache):

```text
PASS 121  FAIL 0  SKIP 35   ok  raptormark/e2e  2866.744s
```

Same counts as the pre-lifter baseline, and the set difference of passing test
NAMES against it is empty. That mattered more here than for 0072-0074: those
only ADD previously-absent ISELs, whereas 0075 changes a path taken by every
`XZR`/`WZR` read. The reasoning said it could only affect previously-fatal
cases; the run confirms it.

**Final E2E gate** (`raptormark-builder:gofull` = patches 0072-0077, cold cache):

```text
PASS 121  FAIL 0  SKIP 35   ok  raptormark/e2e  2919.623s
```

Identical to the pre-lifter baseline, by NAME-SET comparison and not merely by
count. Three gates were run across the series -- `:simd` (0072-0074), `:zreg`
(+0075) and `:gofull` (+0076/0077) -- all 121/0/35 and all losing nothing.

## 2026-09-01 Ruby: the `--disable-gems` requirement was the longjmp defect

Re-tested the 2026-08-22 finding that ruby needs `--disable-gems`, because its
symptom -- `*** longjmp causes uninitialized stack frame ***` while RubyGems
loads -- is a longjmp failure and setjmp/longjmp was fixed earlier today.

Relinked `.agents-workspace/fixtures/ruby-glibc.fused` with
`raptormark-builder:dirfd` (~8 min; the object was NOT cached, so this was a
full re-translation, not the relink I first estimated) and ran it against
`yjit-2026-08-22/p-gems.img`, the existing sidecar whose argv OMITS
`--disable-gems`.

**Controlled comparison** -- same fused ELF, same sidecar, same host, only the
linked runtime differs:

| module | result |
|---|---|
| `ruby-rbprctl.wasm` (2026-08-22, pre-fix) | `*** longjmp causes uninitialized stack frame ***`, rc=1 |
| rebuilt with `:dirfd` (post-fix) | **no longjmp message**, runs much further, rc=134 |

So the `--disable-gems` requirement WAS the setjmp/longjmp defect, and it is
gone. ⚠️ That does NOT mean ruby loads RubyGems: it now fails differently,

```text
execution failed: out of bounds memory access, Code: 0x408
  Accessing offset from: 0x25fc084c to: 0x25fc084f , Out of boundary: 0x1a03ffff
  In instruction: i32.load (0x1e)
```

a wild read far past the arena, ~60 frames deep. That is a NEW blocker, not the
old one wearing a different face -- the old one was glibc's `__longjmp_chk`
FORTIFY check firing, which no longer fires at all.

❗ Worth noting how close this came to being missed: the TODO entry was a
workaround note ("ruby needs `--disable-gems`"), not a bug report, and nothing
connected it to the longjmp work. It was found by re-reading open entries for
symptoms the day's fixes might have touched. The repo's own rule -- re-verify
every TODO entry against the tree before acting -- is what surfaced it.

## 2026-09-01 Ruby's new blocker, localised: a NameError whose MESSAGE crashes

Resolved the wasm frame indices against the module's name section
(`.agents-workspace/drivers/wasmnames.py`). Innermost-outward:

```text
rb_id_table_lookup                  <- the out-of-bounds i32.load
callable_method_entry_or_negative
rb_vm_search_method_slowpath
gccct_method_search_slowpath
rb_funcallv -> rb_inspect -> rb_protect
name_err_mesg_to_str                <- building a NameError's message
exc_to_s -> rb_String -> rb_check_string_type -> rb_check_convert_type_with_id
rb_get_detailed_message
rb_ec_error_print_detailed
error_handle
ruby_options
```

Two facts change the shape of this from "ruby crashes loading RubyGems":

1. **Ruby is already handling an exception.** `error_handle` /
   `rb_ec_error_print_detailed` sit near the top, so RubyGems raised a
   **NameError** and the crash is in FORMATTING it. Whatever the NameError is,
   it is the real defect; the OOB is downstream of it.
2. **Nothing was printed.** The process died before the message reached stderr,
   which is why the failure looks like a bare wasm trap.

That is the same shape as the Go traceback hang chased earlier today: the
visible crash was in the REPORTING path, and the thing being reported was the
actual problem. Worth treating as a pattern -- when a guest dies inside its own
error printer, find what it was trying to say first.

### ❗ Ruled out: this is not the longjmp fix misfiring

`fn_plt -> __remill_jump` appears repeatedly in the stack, and `__remill_jump`
is exactly what 2026-09-01's longjmp fix modified -- so it had to be excluded.
Control: the same rebuilt module with `--disable-gems` (`startup.img`) prints
`STARTUP-OK` and exits 0. Ruby's working path is unaffected, and a PLT thunk's
`br` is classified as a tail call (target IS a function entry, which
`nonlocal_jump_depth` checks FIRST) exactly as before.

### Not diagnosed

Why a NameError is raised during RubyGems load, and whether the OOB in
`rb_id_table_lookup` is a consequence of the same corruption or an independent
lifting defect. Both need real Ruby-internals work.

## 2026-09-01 RubyGems: a corrupted VALUE at `require "rbconfig"`

Named the exception without going through Ruby's error printer, which is the
part that crashes. Trick: boot with `--disable-gems` so ruby starts cleanly,
then `require "rubygems"` inside a `rescue`, printing `e.class` and `e.name`
(plain accessors) before `e.message` (which crashes). Sidecar built with
`pgrootfs`; probe kept in `.agents-workspace/rubygems/`.

```text
class=NoMethodError
name=:to_s
recv-failed NoMethodError
bt=/usr/local/lib/ruby/3.4.0/rubygems.rb:9:in 'Kernel#require'
```

The probe run exits **rc=0** -- the crash is entirely in the reporting path.

`rubygems.rb:9` is `require "rbconfig"`, and the filetrace shows `rbconfig.rb`
IS read (14,295 bytes), so nothing is missing from the image.

❗ **The receiver is a corrupted Ruby VALUE.** `.class` on it also raises
`NoMethodError`, which no valid object of any class can do. This is not a
missing method.

### CORRECTION to the previous entry

It left open "whether the OOB is a consequence of the same corruption or an
independent lifting defect". It is a consequence, and the chain is now
end-to-end:

1. a corrupted VALUE appears during `require "rbconfig"`;
2. calling `to_s` on it raises `NoMethodError`;
3. Ruby describes the error -- `name_err_mesg_to_str` -> `rb_inspect` -> method
   lookup -> `rb_id_table_lookup` -- following the garbage class pointer;
4. that is the out-of-bounds `i32.load`.

One root cause, two symptoms. ⚠️ Still not diagnosed: WHERE the VALUE is
corrupted, which needs Ruby-VM-level work.

### Method note

Three probes, each cheap, each killing a hypothesis: ENOSYS census (identical in
the working and failing runs -- not a missing syscall), sidecar name-table dump
(`rubygems.rb` and `rubygems/` ARE present -- not a missing file), filetrace
(both files read successfully -- not a failed load). Only then was it worth
building a custom sidecar.

## 2026-09-01 RubyGems, narrowed: it is the BOOT-TIME load, and the fixture lacks every .so

Three more probes, each refuting a hypothesis.

**Not frozen string literals, and not `rbconfig`.** A probe doing
`require "rbconfig"` at top level, plus `"rbconfig".dup`, a dynamically built
string, `.freeze.to_s` and `.inspect`, passes ALL SIX checks. The literal is
fine and so is the file.

**Not nesting.** `/nested.rb` containing `require "rbconfig"`, with and without
`# frozen_string_literal: true`, required from the probe: both OK. A require
executed from inside another require is not the trigger.

**It is the BOOT-TIME load.** The original failure comes from a sidecar whose
argv omits `--disable-gems`, so ruby loads RubyGems during `ruby_options` --
the backtrace's outermost frames were `ruby_options -> error_handle`. Loading
it AFTER boot behaves completely differently: with `rbconfig` pre-loaded,
`require "rubygems"` reaches **rubygems.rb:1415** and fails on something else
entirely.

### ❗ And that something else is a FIXTURE gap, not a defect

```text
rubygems: LoadError msg=cannot load such file -- monitor.so
  /usr/local/lib/ruby/3.4.0/monitor.rb:10:in 'Kernel#require'
```

`.agents-workspace/fixtures/rbbench/root` contains **zero `.so` files**;
`ruby:3-slim` ships 18 native extensions under
`/usr/local/lib/ruby/3.4.0/aarch64-linux/`, `monitor.so` among them. So the
fixture root was built without native extensions and **cannot exercise RubyGems
to completion no matter what the runtime does**. Anyone measuring RubyGems on
this fixture needs to rebuild the root first.

### Where that leaves the real defect

Narrowed to: **loading RubyGems during `ruby_options` (boot) corrupts a VALUE**,
where loading it post-boot does not. The same `require "rbconfig"` succeeds at
top level and fails at rubygems.rb:9 during boot, so the difference is VM state
at the time, not the require itself.

⚠️ Still Ruby-VM-level work, and now bounded to a specific window: between
`ruby_options` starting and rubygems.rb:9 executing.

## 2026-09-01 ⭐ RubyGems: the corruption is caused by the GARBAGE COLLECTOR

Bisected `rubygems.rb` by truncation, then found the real variable.

**Bisect.** First 9 lines OK; first 13/15/17 OK (`module Gem` defined); first 18
CRASHES. Line 18 is `require_relative "rubygems/defaults"`.

**But no single file is at fault.** `require "rubygems/defaults"` ALONE is fine,
as are `rubygems/errors` and `rubygems/deprecate`. So it is cumulative -- a
threshold reached after enough code loads, which is the shape of a GC trigger.

**Confirmed.** `gcprobe.rb` (preserved) requires rubygems with and without
`GC.disable`:

| | result |
|---|---|
| GC enabled | `NoMethodError name=:to_s` -- the corrupted VALUE |
| **GC disabled** | **`LoadError`** -- no corruption; reaches the fixture's missing `monitor.so` |

Same binary, same sidecar, same load order. **The garbage collector corrupts
objects under ecvisor.**

### Hypothesis for WHY, clearly labelled as one

MEASURED: GC causes it. NOT MEASURED: the mechanism. The most likely candidate
is register scanning. Ruby's `rb_gc_mark_machine_context` spills callee-saved
registers with `setjmp` and then scans the resulting buffer conservatively:

```c
jmp_buf save_regs_gc_mark;
setjmp(save_regs_gc_mark);
mark_locations_array((VALUE *)save_regs_gc_mark, ...);
```

Under ecvisor the guest's registers live in the `State` struct, not in host
registers, so if that `setjmp` does not spill the values the GC expects, live
objects held ONLY in registers are invisible to the mark phase, get collected,
and a later use finds a freed VALUE. That matches every observation: a valid
object reference turning into something whose `.class` itself raises.

⚠️ It is a hypothesis. Confirming it means checking what ecvisor's `setjmp`
writes into `jmp_buf` against what Ruby's marker reads out of it.

### Why this matters beyond Ruby

Any guest with a conservative, stack-and-register-scanning GC has the same
exposure -- this is not a RubyGems bug and not specific to gems. It is only
visible via gems because that is the first workload that allocates enough to
trigger a collection.

### Bounded workaround for anyone benchmarking Ruby now

`GC.disable` gets RubyGems past the corruption. Not a fix, and not usable for
real workloads, but it separates GC bugs from everything else while measuring.

## 2026-09-01 GC mechanism: register spilling is FINE; `pthread_getattr_np` is not

Two probes against the hypothesis in the previous entry.

### ❌ REFUTED: "setjmp does not spill live registers"

`gcregs.c` (preserved) parks magic values in x19/x20, calls `setjmp`, and scans
the `jmp_buf` for them -- which is exactly what
`rb_gc_mark_machine_context` does with the buffer it fills. Native and ecvisor
agree:

```text
jmp_buf bytes: 312
x19 magic in jmp_buf: YES
x20 magic in jmp_buf: YES
GCREGS OK
```

So ecvisor's lifted `__sigsetjmp` DOES write the current callee-saved registers
into the buffer, and a conservative scan of it would find them. The mechanism I
proposed for the GC corruption is wrong. Recording it as refuted rather than
quietly dropping it -- it is the obvious guess and someone will make it again.

### ⭐ NEW: `pthread_getattr_np` fails, so the stack BOUNDS are unavailable

`stackb.c` (preserved) asks for the machine-stack bounds and checks the current
SP lies inside them:

| | result |
|---|---|
| native | `[0xffffcab28000,0xffffcb328000)`, sp inside -> `STACKB OK` |
| **ecvisor** | **`pthread_getattr_np FAILED rc=2`** (ENOENT) |

glibc implements `pthread_getattr_np` for the MAIN thread by reading
`/proc/self/maps`, and **ecvisor synthesises no `/proc` entries at all** -- a
grep for `"/proc` across `runtime/src` finds only a comment.

That is a concrete, fixable gap, and it is the machinery a conservative
collector uses to decide WHAT RANGE to scan. Ruby's `ruby_init_stack` /
`rb_gc_mark_machine_context` mark conservatively over
`[stack_start, stack_end)`; if those bounds come from a failed query and a
fallback guess, live references on the real stack can fall outside the scanned
window and their objects are collected while in use -- which is the observed
symptom.

⚠️ **Still a hypothesis, and the weak link is explicit**: MEASURED that
`pthread_getattr_np` fails, that no `/proc` exists, and that `GC.disable` avoids
the corruption. NOT measured that Ruby's fallback bounds are actually wrong, nor
that the missed references are the ones that matter. Confirm by instrumenting
what Ruby computes for `stack_start`/`stack_end` under ecvisor and comparing
against the real stack extent -- ecvisor knows the true bounds
(`arena::STACK_TOP_VMA` and the process's stack allocation).

### Worth fixing regardless of Ruby

A guest asking for its own stack bounds and being told ENOENT is wrong on its
own terms; `pthread_getattr_np` is ordinary glibc API. Whether or not it turns
out to be the GC mechanism, synthesising `/proc/self/maps` -- or special-casing
this query -- removes a whole class of "the guest cannot find out where its
stack is" failures.

## 2026-09-01 CORRECTION: "the GC corrupts objects" is too broad, and /proc did not fix it

Two more results, both negative for the hypotheses and one positive for the tree.

### ✅ Implemented: synthetic `/proc/self/maps` (runtime-only)

`sys_openat` now synthesises it, mirroring the `/dev/urandom` precedent, with
lines for the image, brk, mmap window and stack taken from `arena.rs`.
Measured with `stackb.c`:

| | before | after |
|---|---|---|
| `pthread_getattr_np` | **FAILED rc=2** | ok |
| reported stack | -- | `[0x16000000,0x18000000)`, sp inside |

A real fix: a guest can now discover where its own stack is. Built as
`raptormark-builder:procmaps`, runtime-only, so `BASE_ID` is unchanged and the
cached ruby object relinked in seconds.

### ❌ REFUTED: it is not the missing stack bounds

With `/proc/self/maps` present, ruby STILL raises `NoMethodError name=:to_s`
loading RubyGems with GC enabled. So the missing bounds were not the mechanism
-- most likely because Ruby's main thread gets its bounds from
`ruby_init_stack` recording a local's address, not from `/proc` at all.

### ❌ CORRECTION: basic GC is CLEAN

The previous entry's headline -- "the GARBAGE COLLECTOR corrupts objects under
ecvisor" -- is **too broad**. `gcmin.rb` (preserved) allocates 2000 strings,
runs `GC.start` three times, and verifies every one, plus holds an object across
a GC inside a C frame:

```text
allocated=2000
round=0 corrupted=0
round=1 corrupted=0
round=2 corrupted=0
held_len=18
GCMIN-DONE
```

Nothing is corrupted. So the GC is not broken in general.

### What is actually established

* RubyGems load corrupts a VALUE (`NoMethodError` on `:to_s`) -- MEASURED;
* `GC.disable` avoids it -- MEASURED;
* plain allocation + collection is clean -- MEASURED;
* `setjmp` spills callee-saved registers correctly -- MEASURED;
* the stack bounds were unavailable, now are, and it changed nothing -- MEASURED.

So the corruption needs GC **and** something RubyGems does that a simple
allocation loop does not. ⚠️ A plausible reframing, NOT yet tested: the
underlying fault may not be in the GC at all -- `GC.disable` also stops objects
being freed and reused, so it would equally mask a use-after-free or a stray
write coming from somewhere else. "GC is the trigger" and "GC is the bug" are
different claims and only the first is supported.

## 2026-09-02 ⭐ Ruby: PROVEN premature free -- `T_NONE` where a live object should be

`heapchk.rb` (preserved) replays rubygems.rb's first 18 lines step by step,
calling `GC.verify_internal_consistency` -- Ruby's own heap walker -- between
each:

```text
start: heap OK
rbconfig: loaded              -> after rbconfig: heap OK
                                 after module Gem: heap OK
compatibility: loaded         -> after compatibility: heap OK
defaults: RAISED NoMethodError name=:to_s
check_rvalue_consistency: T_NONE is T_NONE.
[BUG] objspace/memsize_of(): unknown data type 0x0(0x000000000f856740)
```

**The heap is verifiably clean right up to the failing require**, and then
Ruby's own checker reports `T_NONE` -- an EMPTY slot -- where a referenced
object should be, at `0x0f856740` (inside the mmap window,
`0x0C800000..0x16000000`).

That settles the shape: **an object is freed while still referenced**. Not a
mysterious "corrupted VALUE", not a lifting bug in some instruction -- a live
object collected out from under a reference.

### What this rules in and out

* ❌ NOT a general GC bug -- `gcmin.rb` collects 2000 objects three times with
  zero corruption. Objects held in a Ruby ARRAY (marked precisely off the VM
  stack) are safe.
* ✅ The failing references are ones the collector must find CONSERVATIVELY.

### Hypothesis, and why the earlier `setjmp` result does not refute it

⚠️ NOT MEASURED. A conservative collector finds C-frame references by scanning
the machine stack and the spilled registers. `gcregs.c` showed `setjmp` fills
`jmp_buf` correctly, so callee-saved GUEST registers are visible -- but that is
only part of what native code guarantees. Natively, a caller-saved register
holding a VALUE is spilled to the machine stack by the compiler and found there.
Under ecvisor the lifted code keeps guest values in WASM locals, which are not
addressable memory at all: if the lifter elides a guest stack store it can prove
dead, the value is invisible to any scan of guest memory, however correct that
scan is.

That would explain every observation, including why `setjmp` looking right did
not help. Confirming it means checking whether the lifted code actually performs
the guest's register spills around allocation sites -- a `VroInstructionLifter`
question, not a GC one.

### ⚠️ Consequence if that is right

It is not fixable by teaching ecvisor about Ruby. ANY conservative
stack-scanning collector -- Ruby, Perl, Boehm-GC consumers, some JVMs' native
paths -- would be exposed, and the fix would have to be in how the lifter treats
guest stack stores near calls. That is a significant design question and is
recorded, not attempted.

**❌ REFUTED, immediately: "wasm locals are invisible to a conservative scan".**
`consgc.c` (preserved) models Ruby's root scan faithfully -- `setjmp` to spill
callee-saved registers, scan that buffer, then scan the machine stack from the
current SP to a recorded start -- with a magic value live across the scan in a
caller frame. Native and ecvisor give the IDENTICAL answer:

```text
value survived   : yes
found in registers: YES
found on stack    : no
reachable by a conservative scan: YES
```

So a live value held in a register IS reachable by exactly the mechanism Ruby
uses. The hypothesis in the entry above is wrong.

### Status of this investigation, honestly

SEVEN hypotheses have now been refuted by measurement: frozen string literals,
require nesting, boot-time vs post-boot, `setjmp` register spilling, missing
machine-stack bounds, a general GC defect, and conservative-scan blindness to
wasm locals. What survives is narrow and solid:

* RubyGems' load frees an object that is still referenced -- PROVEN by Ruby's
  own `GC.verify_internal_consistency` reporting `T_NONE`;
* it needs a collection to happen (`GC.disable` avoids it);
* plain allocation and collection are clean;
* every root-finding mechanism tested in isolation works.

⚠️ **Guessing at the mechanism has stopped paying.** The next step is a
different KIND of work, not another hypothesis: identify WHICH object is freed
and what referenced it. `RAPTORMARK_ECV_WATCH`/`WATCHLEN` can watch a guest
address, and Ruby's own `GC.stress`, `ObjectSpace` and `RUBY_GC_DEBUG` can
narrow the allocation site. Whoever picks this up should instrument, not
theorise -- the cheap theories are exhausted and each cost a build-and-run
cycle.

**E2E gate for the synthetic `/proc/self/maps`** (`:procmaps`, warm cache since
the change is runtime-only and `BASE_ID` did not move):

```text
PASS 121  FAIL 0  SKIP 35   ok  raptormark/e2e  369.240s
```

Identical to the baseline by name-set comparison. The change is additive -- a
new synthetic path in `sys_openat` -- so nothing else could reach it, and the
run confirms that rather than assuming it.
