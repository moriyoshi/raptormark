# Documents for both humans and coding agents

* [README.md](./README.md) ... goal, the four pipeline stages, the ecvisor runtime, repository layout, building and running, and the honest status. This is the canonical architecture document; there is no separate `ARCHITECTURE.md`.

# Documents for coding agents

* [./.agents/docs/JOURNAL.md](./.agents/docs/JOURNAL.md) ... unconsolidated findings and consolidation records. Ordinary work is append-only; `reconcile-journal-ltm` may remove entries only after auditing their durable knowledge into LTM or TODO.
* [./.agents/docs/LTM/INDEX.md](./.agents/docs/LTM/INDEX.md) ... long-term memory index for durable project knowledge under `./.agents/docs/LTM/`. Synthesis documents orient; source topic documents hold the detail. ❗ **LTM drifts, and the same re-verification rule as TODO.md applies to it** — measured 2026-08-25, when an audit of every code artifact LTM names found eight stale claims, including one document instructing a side build to run a command that is now obsolete and fails by design, one calling a `wasm-opt` pass a "measured pure round-trip" that a dated correction had already refuted, and two naming Rust symbols (`ExitReason::Code`, `restore_bounded`) that have never existed. **A grep proves a marker is present, never that the claim around it is current**, so an LTM statement that names a file, function, symbol or flag must be checked against the tree before you act on it.
* [./.agents/docs/TODO.md](./.agents/docs/TODO.md) ... open to-do items extracted from JOURNAL.md during `good-sleep` consolidation. Check and update this file when picking up or finishing work. **Every entry must be re-verified against the tree before acting on it** — much of this tree was reconstructed rather than recovered, so an entry may describe an intent the code never reached, or a gap that has since been closed by another session.
* [./.agents/docs/QUALITY_GATE.md](./.agents/docs/QUALITY_GATE.md) ... the standard verification gate to run before declaring a change complete: the Go gate, the Rust gate, the Bazel gate, and how to run the env-gated E2E suite.

# Rules and protocols

## General

* raptormark translates an aarch64 Linux container image ahead of time into a single WebAssembly module, supervised by ecvisor (a Rust `wasm32-wasip1` staticlib) instead of a kernel. Read `./README.md` before changing stage boundaries.
* The pipeline is four stages: discovery (`internal/image`) -> fusing (`internal/fuse`) -> translation (`raptormark translate-one`, driven by `internal/translate`) -> linking (`internal/link` + `link-all`). Stages 3 and 4 run *inside* the builder Docker image; stages 1 and 2 run on the host.
* **`raptormark build <image> --out DIR --builder <img>` runs all four stages** (`internal/pipeline`), and `raptormark run <module>` executes the result. Added 2026-08-24; before that the pipeline was only ever strung together by the `e2e/` suite, which is why `image.Plugins` sat with "no production caller" for three sessions — there was no command for one to live in.
  * `build` discovers plugins by default (`--plugins auto`) and fuses each as its own dlopen-able unit. ⚠️ Discovery finds **more than you planted**: on a `postgres:17`-derived fixture, 5 plugins, 3 of them the base image's own OpenSSL modules. A unit count is not a plugin count.
  * `run` exists because one combination has to be right and blames the guest when it is not: the sidecar must be inside the preopened directory and `RAPTORMARK_ROOTFS` must be the **guest** path. Otherwise ecvisor reports the rootfs "set but unreadable", runs with no exec map and no dlopen map, and every `execve` falls back to program 0 while every `dlopen` fails with "cannot open shared object file".
  * ⚠️ **The three runtimes spell the directory flag three different ways**, and none fails loudly when swapped — `wasmedge --dir GUEST:HOST`, `wasmtime --dir HOST::GUEST`, `wasmer --volume HOST:GUEST` (the wasmtime order with the wasmedge separator; its `--mapdir GUEST:HOST` is deprecated as of 7.3.0). `runtimeArgs` in `internal/pipeline/run.go` is the one place that knows.
  * ⚠️ **`--runtime wasmer` always passes `--net`, and must.** Without it a WASIX guest's `sock_open` SUCCEEDS and its `sock_bind` returns errno 58, so nothing reports a problem until the first bind and what the guest logs is a bind failure on an address that is perfectly fine.
* The CLI uses `github.com/alecthomas/kong`. Keep new commands consistent with the existing kong layout in `cmd/raptormark/`.
* ❗ **Adding a Go package means adding a `BUILD.bazel` AND a `deps` entry**, and a new FILE in an existing package means adding it to that package's `srcs` — they are listed explicitly. ✅ **The `srcs` half is now GUARDED**: `internal/builder`'s `TestEveryGoFileIsInItsBazelSrcs` compares every package's `.go` files against what its `BUILD.bazel` lists, and fails naming the file. The `deps` half is still yours to remember — a missing dependency at least fails the Bazel build rather than skipping silently. `gofmt`/`go build`/`go vet`/`go test` all stay green when you forget; `bazel test //...` reports "N tests pass and M were SKIPPED", which reads like caching and is not. Measured 2026-08-24: the omission broke `//cmd/raptormark:builder_tools_linux_arm64`, which `//builder:stage` depends on, so **the builder image could not have been built**.
* **`internal/` and `runtime/` are a producer/consumer pair, and it is not symmetric.** `runtime/` was recovered wholesale from a Docker layer; `internal/` was rewritten by hand. Every table the runtime reads through `find_data_section` therefore had a live consumer and, for a while, no producer — `.ecv.tls`, `.ecv.irela`, `.ecv.early`, `.ecv.init`, `.ecv.stacklists`, `.ecv.dlsyms` were each found one at a time, hidden behind the last. If you find a new consumer in `runtime/`, assume nothing emits it until you have proven otherwise.
* ❌ Do not modify `third_party/elfconv`. It is a submodule pinned clean at its upstream commit; the fork lives as an ordered patch series in `patches/`, applied inside the builder image. A change to the lifter is a new or edited patch file, never a working-tree edit.
* **`raptormark-builder:latest` is not the newest builder.** Side builds are tagged and `latest` is left alone. Pass an explicit `RAPTORMARK_BUILDER=raptormark-builder:<tag>`; a stale default fails deep inside elflift with an error that reads like a defect in the input.

## File Management

* When you'd make summary documents for your work, be sure to write them under `./.agents/docs`, not under `/tmp`.
* Temporary files should be created under `./.agents-workspace/tmp`, not under `/tmp`.
* **`./.agents-workspace/tmp` is DISPOSABLE and gets wiped without warning.** Anything you would be sorry to lose does not belong in it. Durable agent assets live in named siblings, all of them gitignored:
  * `./.agents-workspace/drivers/` — scratch driver SOURCES (`<name>/main.go`) and `rebuild-drivers.sh`, which builds them all. Their binaries build beside them.
  * `./.agents-workspace/fixtures/` — fused ELF fixtures and closure fuses. Expensive to regenerate (`postgres-glibc.fused` needs an image export plus a fuse) and referenced by every build-speed probe.
  * This rule was written after a `tmp` cleanup removed `rebuild-drivers.sh` and 20 driver binaries that `.agents/docs` names directly. The sources survived by luck. If a cleanup can break the next session, the file was in the wrong place — move it rather than asking for the cleanup to be more careful.
* ❌ Do not build binaries into the version-controlled tree (e.g. `go build -o raptormark ./cmd/raptormark` at the repo root). Output under `./.agents-workspace/` — `tmp/` for one-off builds, `drivers/` for anything `rebuild-drivers.sh` maintains.
* ❌ Never delete user files without permission. Only safe to delete: files YOU created in THIS session that are in `./.agents-workspace/tmp/`. Always ask first if unsure. Assume all pre-existing files belong to the user.
* ❗ **`_recovery/` IS GONE.** It held untracked evidence from the 2026-08-01 rebuild — `_recovery/RECOVERY.md` and `_recovery/reference/` — and this line used to read "kept on disk deliberately. Read it; never clean it up." Confirmed absent and unrecoverable by the user on 2026-08-25. It was gitignored and never tracked, so **git cannot date or explain the loss and neither can anyone else**; do not go looking for it, and do not read a reference to it in an older document as evidence it survived.
  * ⚠️ **The lesson is worth more than the rule was: a "never delete this" instruction is not a guard.** Every reference to `_recovery/` in code and build config is an EXCLUSION — `.gitignore`, `.bazelignore`, `BUILD.bazel`'s `gazelle:exclude`, and a `filepath.SkipDir` arm in `internal/builder/workspace_test.go`. Each tells a tool to ignore it; not one required it to exist, so every gate stayed green as it vanished and nothing ever reported it. The same shape lost `raptormark-tmp-ossldgst:latest` the same week. **If something is irreplaceable, give it a check that fails when it is missing** — a rule in a document protects nothing.
  * Those exclusions are deliberately LEFT IN PLACE. They cost nothing, and they are what a restored or re-derived `_recovery/` would need.
  * ✅ **There is now a real guard: `raptormark preserve`.** It records what is present into `.agents/preserve.json` (committed, so it outlives what it guards) and `raptormark preserve check` exits non-zero when something recorded has DISAPPEARED. Run it before and after anything that deletes — a Docker cleanup, a `.agents-workspace` sweep, a branch switch that touches untracked files.
    * ⚠️ It checks **disappearance, not absence**, and the distinction is the whole design. A check for absence fails on every fresh clone, and a check that cries wolf gets deleted — leaving no check, which is where this started. Never recorded means "nothing is known", reported as such rather than as an all-clear.
    * ❗ `preserve snapshot` REFUSES to record something already missing. A manifest listing a thing that is gone fails forever, and a check that can never pass is one somebody switches off.
    * If a deletion was deliberate, re-run `snapshot`: the manifest is committed, so the change shows up in a diff a reviewer can see.

## Do not prune Docker

The raptormark images and the reclaimable BuildKit cache are the only copies of things that exist nowhere else. ❌ Avoid `docker system prune` and `docker buildx prune`. The pinned elfconv base and the patched builders are not reproducible from this tree alone. See `.agents/docs/LTM/recovery-and-builder-provenance.md` for the durable provenance and preservation rules.

## Building

* Go 1.26 and cargo are on `PATH` (Go via mise, cargo via rustup). No extra setup is needed.
* Run `gofmt -w` on every Go file you change before running `go build`, `go vet`, or `go test`, and before reporting a change as done. Likewise `cargo fmt` for every Rust file you change under `runtime/`.
* The standard local gate for any Go change you make — this applies to subagents too:
  ```
  gofmt -l <changed files>      # must print nothing
  go build ./... raptormark/tools/decode-oracle/...
  go vet   ./... raptormark/tools/decode-oracle/...
  go test  ./... raptormark/tools/decode-oracle/...   # or focused: go test ./internal/<pkg>/
  ```
  ⚠️ **NAME BOTH PATTERNS.** `tools/decode-oracle` is a separate module, and a
  workspace does NOT make `./...` recursive across modules — a relative pattern
  resolves against the module you are standing in, so bare `./...` silently
  skips 36 tests. This file showed the short form until 2026-08-24 and an agent
  followed it for a whole session; the short form is green, so nothing objects.
  `go test` prints only what it ran. `.agents/docs/QUALITY_GATE.md` §1 has the
  measurement and the reason.
* The standard local gate for any change to a **BUILD.bazel, a `.bzl`, or anything that reaches the builder image** — the LLVM tools, the C shims, the ecvisor profiles:
  ```
  raptormark bazel --image raptormark-elfconv-base-patched:<tag> test //...
  ```
  It is fast (seconds, warm) and three of its tests are the ones that make moving a build recipe safe:
  * `//builder:tools_equivalence_test` — the four LLVM tools byte-identical to the recipe `builder/Dockerfile` used to carry.
  * `//runtime:cshim_equivalence_test` — both C shims, the same way.
  * `//runtime:profile_exclusion_test` — each ecvisor profile carries exactly one network backend, **and carries its own**. ❗ That second half was added 2026-08-24 and the test was weaker than it read without it: every assertion is an ABSENCE, and `ecvisor::net::loopback` leaves no symbols anywhere (pure Rust, inlined away at `-Clto=fat`), so "loopback is not in this archive" was satisfied by an archive that had compiled loopback in. Measured by pointing `//runtime/wasix` at `net-loopback`: all four `foreign-backends=0` lines still printed `ok`.
  * ⚠️ **Adding a `net-*` backend means editing FOUR places, and the compiler catches none of them.** `net-loopback` is a LABEL, not a `cfg`: `runtime/src/net/mod.rs` selects loopback by the ABSENCE of every other backend, from **two separate `any(...)` lists**. Miss one and loopback is compiled in alongside the real backend. The other two are the `PROFILES` list in `profile_exclusion_test.sh` and a `cargo check --features net-<name>` line in `QUALITY_GATE.md` §2 — a backend with no line there can fail to compile while every other gate stays green.
  * `//runtime:loader_exclusion_test` — the SHIPPING archive carries no host-aided loader, and the two loader archives (`hosted`, `wasix`) carry only their own backend. ⚠️ `--profile hosted` (loopback + `load-hosted`) imports `env.ecv_host_load_side`, which **no stock runwasi shim supplies**, so such a module fails to INSTANTIATE before running an instruction. It is never the default; this is what keeps it that way.

  ⚠️ `bazel test //...` does **not** run the Go tests: `//internal/builder` and `//internal/rootfs` are tagged `manual` because they read the repo tree by relative path and shell out to `go`, neither of which exists in Bazel's sandbox. `go test ./...` is still the authority for Go, and it passes. Two gates where one of them lies is worse than one gate.
* The standard local gate for any Rust change under `runtime/`:
  ```
  cargo fmt --manifest-path runtime/Cargo.toml --check
  cargo test --manifest-path runtime/Cargo.toml
  cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1
  ```
  ⚠️ `cargo` remains the Rust gate even though Bazel now BUILDS the three shipped archives. `runtime/Cargo.toml` is still what `cargo test` reads, so `[profile.release]` there and `RELEASE_PROFILE` in `//bazel:ecvisor.bzl` have to be changed together — rules_rust does not read Cargo.toml.
  All three commands are expected to be green. Treat any failure as a
  regression; runtime behavior still requires the E2E suite. The current host
  test count, and what it can and cannot reach, live in ONE place —
  `.agents/docs/QUALITY_GATE.md`. This file used to pin its own copy ("47 tests,
  verified 2026-08-13"); QUALITY_GATE re-measured twice and the two drifted, so
  the number is not repeated here. A count is only useful for spotting a large
  DROP, and it cannot do that from two files that disagree.
* Fix violations and re-run until clean. Do not declare a change complete with failing build, vet, or tests.
* **If only `runtime/` changed, relink — do not re-lift.** The lifted `.o` depends on the fused ELF alone. Rebuilding the image and running `link-all` turns a 30-minute cycle into a 5-minute one.
* **Rebuild the tool after changing the library.** A stale binary in `.agents-workspace/` produces a "the fix didn't work" conclusion that is purely an unrebuilt binary.
* Set `RAPTORMARK_OBJECT_CACHE=<dir>` for any pipeline run. Objects are content-addressed over the base image digest, the translation pipeline's own sources, the ELF, and the codegen options, so a change confined to `runtime/` reuses every translated object.
* **A side-built image must LAYER ONTO the existing patched base, not rebuild one.** `raptormark build-image` derives the patched base from `--tag`, so `--tag foo` builds `raptormark-elfconv-base-patched:foo`. If the BuildKit layer cache has been evicted, re-applying the patch series yields a different image id, `BASE_ID` changes, and every cached translated object is invalidated — hours of re-translation to pick up a change that cannot alter a single object. For a `runtime/`-only change, run `docker build` against the existing `raptormark-elfconv-base-patched:<tag>` and pass `BASE_ID` and `TRANSLATE_SH` verbatim from the image you are layering on. Verify BEFORE building: the two labels
  ```
  docker image inspect --format '{{index .Config.Labels "raptormark.base_id"}} {{index .Config.Labels "raptormark.translate_sh"}}' raptormark-builder:<tag>
  ```
  must come out identical on the new image, and `runtime/` is deliberately absent from `TranslateSH` (`internal/builder/toolsid.go`) so it will — unless something else in the tree changed too, in which case the reuse assumption is void and you want to know that before the build, not after.

  **The side-build recipe as of 2026-08-23** — two steps, because the image's contents come from Bazel and its context is the staged tree:
  ```
  raptormark bazel --image raptormark-elfconv-base-patched:<tag> build //builder:stage
  # copy //builder:stage out to builder/_stage (build-image does this for you), then:
  docker build -t raptormark-builder:<newtag> -f builder/Dockerfile \
    --build-arg ELFCONV_BASE=raptormark-elfconv-base-patched:<tag> \
    --build-arg BASE_ID=<verbatim> --build-arg TRANSLATE_SH=<verbatim> \
    builder/_stage
  ```
  ⚠️ **The context is `builder/_stage`, not the repo root.** The Dockerfile is `COPY . /`, so passing `.` would copy the repository into the image.

  ⚠️ **`bazel/llvm_tool.bzl`, `builder/BUILD.bazel` and `bazel/sdk.bzl` are now in `TranslateSH`.** They carry the tools' compile flags, component lists and choice of LLVM — the caveat `toolsid.go` used to record about the Dockerfile, which now applies to them. Editing any of them invalidates every cached object, including edits to the staging genrule that cannot change a tool. That over-invalidation is deliberate: under-invalidating serves a stale object for a pipeline that no longer produces it.
* **✅ FIXED 2026-08-23: the pipeline binary can no longer be stale.** This entry used to read "❌ A raw `docker build` does NOT rebuild the pipeline binary" and tell you to run `raptormark build-tools` first, every time. `builder/Dockerfile` no longer COPYs a prebuilt binary — it is `COPY . /` over `//builder:stage`, which **depends on** `//cmd/raptormark:builder_tools_linux_arm64`, so Bazel cannot assemble the image contents without building it.
  * `raptormark build-tools` is **obsolete and now fails with instructions** rather than rebuilding a file nothing reads. A command that looks like it worked is the same failure it was written to prevent.
  * The 2026-08-18 void gate this entry recorded is worth keeping: a `-fPIC` change to `internal/builder/translateone.go` never reached the image, and everything a careful check looks at said it had — `raptormark.translate_sh` moved (it hashes the pipeline's SOURCE, which really had changed), the object cache went cold and re-translated for 20 minutes, and `libecvisor.a` differed. All three are downstream of *the source changed*; none of *the binary changed*. It also poisoned 45 cached objects under a key that claimed the new pipeline. **That class of mistake is now unrepresentable, not merely documented.**
* **The image contents are built by Bazel, inside the base image.** `builder/Dockerfile` has **no `RUN` lines at all** — `TestDockerfileBuildsNothing` fails if one appears. Everything the image gains is a Bazel target:
  ```
  //builder            ecv-prepare, ecv-split, namespace-object, ecv-promote
  //runtime            the C shims, and ecvisor with net-wasmedge
  //runtime/loopback   ecvisor with net-loopback
  //runtime/browser    ecvisor with net-browser
  //cmd/raptormark     raptormark, and the cross-built pipeline binary
  //builder:stage      all of the above, at their image paths
  ```
  Bazel runs **inside** the elfconv base image, because the LLVM tools must link against the LLVM that built `elflift`:
  ```
  raptormark bazel --image raptormark-elfconv-base-patched:<tag> build //builder:stage
  raptormark bazel --image raptormark-elfconv-base-patched:<tag> test //...
  ```
  Bazel is mounted in from the host (the image has none): put `bazelisk`/`bazel` on `PATH`, or set `RAPTORMARK_BAZEL`.
* **`--config=hermetic` builds the same targets with no Docker at all**, downloading LLVM 16.0.6 and wasi-sdk 24.0 by sha256:
  ```
  bazel build --config=hermetic //builder:stage      # runs on a bare host
  ```
  Measured 2026-08-23 (`builder/hermetic_differential.sh`, which is how to re-measure it):
  * 6 of the 10 staged artifacts are **byte-identical** to the in-image build — all three ecvisor archives, both C shims, and the Go pipeline binary — because rules_rust, rules_go and the wasi-sdk are the same in both modes.
  * The 4 LLVM tools are **not**, and not from drift: Debian's `llvm-config --libs` returns `-lLLVM-16` (one shared library, 88 KB tool), upstream's returns the static component list (13.7 MB tool).
  * Their emitted bitcode differs by **36 bytes** — the embedded LLVM version string, which upstream carries the git hash in. The disassembled IR is identical.
  * ✅ **The emitted objects are byte-identical.** That is the one that decides: an object cached under one toolchain is correct under the other.
  * ⚠️ The **partition cache** misses across a switch — `partcache.go` keys on bitcode bytes, and those 36 bytes are in them. A cold cache, not a wrong answer.
  * ❌ **Hermetic tools built on a newer host cannot run inside the image** (`GLIBC_2.38 not found`). Use hermetic for host-side work and CI; stage into the image only from a host whose glibc is no newer than the image's.
  * ❌ **x86_64 hermetic does not work and fails saying why.** LLVM publishes no `clang+llvm` release for 16.0.6 on x86_64 (16.0.4 is the newest 16.x that has one), and substituting a different patch version would change what this mode measures.
* **❌ Do not run `bazel` directly against this tree.** The base image's entrypoint is `["/bin/bash","--login","-c"]`, so a normal argv is silently truncated to its first word and bazel prints usage and exits 0; `$WASI_SDK_PATH` is under `/root` at mode 700, so a non-root uid reports the compiler as absent; and running as root leaves root-owned `BUILD.bazel` and `MODULE.bazel.lock` files in your tree. `raptormark bazel` handles all three, including chowning them back. Every one of these was found by hitting it.
* **Confirm a rebuilt image actually contains your change**, and confirm it on the ARTIFACT, not the labels. `sha256sum /opt/ecvisor/libecvisor.a` inside the old and new images must differ — that covers a `runtime/` change, because libecvisor.a is built in the image. It does NOT cover a change to `internal/builder`, whose only witness is the emitted object: translate something and look at it (`llvm-objdump -r`, `llvm-nm`) before believing the change is in. An identical hash means a cached layer shipped the old runtime, and the run that follows will "fail to reproduce the fix" for a reason that has nothing to do with the fix.
* **Build costs are wildly non-uniform — do not extrapolate.** Cost follows the largest single function, not total volume: the OpenSSL closure translates in 28 minutes, the smaller nginx closure took 6.5 hours.

## Testing

* Make sure that regression tests are ready for your fix.
* ❗ **Run ONE `go test ./e2e/` at a time, and check for a second one before debugging a red suite.** Several guests bind FIXED ports (39117, 47825, 47826) and the wasmer tests use `docker --network host`, so two overlapping invocations share a port space. The first guest's `bind` then returns EADDRINUSE and every later check in it fails too — the output is a wall of unrelated-looking errnos (`accept should be EAGAIN (errno=5)`, `recv should be EAGAIN (errno=88)`) that reads exactly like a broken backend, with the one line that matters at the top. Measured 2026-08-24: a test that passed alone both before and after was reported as a failure by a concurrent run. `ps aux | grep e2e.test` is cheaper than the investigation.
* **Audit the code before you write the test for it.** Read what the code actually does and state it, then write the test against that. A test written from your model of the fix inherits that model's blind spots, and a test that passes for the wrong reason is worse than no test: it certifies the one behavior nobody checked, and it does so in green.
* Before trusting any check — a new test, a `grep`, a differential probe, a built artifact — ask **what would a PASS look like if this claim were false?** If there is no answer, the check proves nothing yet and must not be cited as evidence. Then neutralize it: deliberately break the fix and confirm the test fails with the diagnostic you intended. ❌ A COMPILE error is not a valid neutralization; it proves the test names a symbol, not that it observes a behavior. Every regression guard in this tree was checked this way, and it caught one guard whose assertion was mis-specified and would never have fired.
* ❗ **A value that crosses a language boundary needs a guard, and the guard is a SOURCE SCAN.** `internal/` and `runtime/` are a producer/consumer pair compiled by different toolchains, one of them inside a container, so no build step can share a constant between them — a scan of the other side's source is the available tool, the same way `//runtime:cshim_equivalence_test` compares against a transcription. Seven such guards exist (`internal/rootfs/runtimeagree_test.go`, `e2e/abiagree_test.go`); add to them rather than inventing a scheme.
  * ⚠️ **The scan must FATAL when its pattern matches nothing.** A regex guard whose pattern stops matching is green forever, which makes this whole class of test worth distrusting unless it refuses an empty result. Neutralize a new one THREE ways: value drift, the constant renamed, and the pattern broken.
  * ⚠️ **Reading a repo file makes a test pass `go test` and FAIL `bazel test`**, which is the drift this file warns about elsewhere. Bazel sandboxes tests with only DECLARED inputs. ✅ Declare the input (`//runtime:srcs` globs every `.rs`) rather than tagging the package `manual` — opting out of the gate to fix one file loses the compile coverage of every test beside it. ❌ Never resolve it by SKIPPING when the file is absent: the guard then goes quiet in exactly the environment nobody watches.
  * ❌ **"These constants appear twice" is not sufficient reason to tie them.** What matters is whether anything BEHAVES differently when they drift. The `ECV_REG_*` codes appear in both embedders and are deliberately unguarded: they only make a diagnostic readable, and the behavioural check is `rc !== 0`, which no renumbering can break.
* **Over-claiming is as harmful as missing.** A missed function boundary gives `_ecv_unreached`, which is loud. Claiming too much silently hides every real boundary inside the range and has the lifter disassemble data as code. Assertions must be bounded by something that genuinely bounds them.
* **glibc and musl share almost no assumptions** — `.eh_frame` volume, the existence of `.init`/`.fini`, whether libc and the interpreter are the same file, symbol versioning. A technique validated on Debian can recover nothing on Alpine, and a check valid on musl can be invalid on glibc. Fixtures need one of each.
* **Synthetic test data that "looks random" usually is not.** Use a real PRNG; an arithmetic byte pattern silently inverted the meaning of an incompressible-file test.
* Host-side unit tests (`go test ./...`) need neither Docker nor wasm. Keep it that way: do not add a test to the default path that requires Docker, root, or network.
* End-to-end coverage lives in `e2e/` and is **env-gated, not build-tagged**, so it still compiles and vets normally and skips cleanly without Docker:
  ```
  RAPTORMARK_E2E=1 RAPTORMARK_BUILDER=raptormark-builder:<tag> \
    RAPTORMARK_OBJECT_CACHE=<dir> go test ./e2e/ -v -timeout 60m

  RAPTORMARK_E2E=1 RAPTORMARK_E2E_SLOW=1 RAPTORMARK_BUILDER=raptormark-builder:<tag> \
    RAPTORMARK_OBJECT_CACHE=<dir> go test ./e2e/ -v -timeout 90m   # + the ~30-min fused lifts
  ```
  ❌ Do not run either bare. `RAPTORMARK_BUILDER` is required for the same
  reason as everywhere else in this file — `raptormark-builder:latest` is not
  the newest builder — and without `RAPTORMARK_OBJECT_CACHE` every run
  re-translates from scratch. This file previously showed both commands without
  them, which contradicts `.agents/docs/QUALITY_GATE.md` §5 and the
  "not the newest builder" rule above it.
  **`-timeout` is a CEILING, not an estimate.** Measured 2026-08-17 on a warm
  object cache: the fast suite is **1186 s (19m46s), 81 pass / 4 skip / 0 fail**
  — a third of its own timeout. Reading the timeout as the cost is what makes it
  feel too expensive to run before declaring a change done, and it is the check
  that catches a runtime regression the host gates cannot see. Only
  `RAPTORMARK_E2E_SLOW=1` adds the ~30-minute fused lifts.

## Measuring

* **A cost inside a function is not a cost of the function you suspect.** Localise to the *statement* before naming a cause.
* **Refute a performance hypothesis by removal, not by arithmetic.** A consistent rate proves nothing. Skip the suspect, re-measure, and keep the change only if the number moves.
* **Variance is a measurement, not noise to average away.** Three rounds of the same bench read 26.9 / 48.2 / 10.1 s; the spread was the signal. Do not report a mean, and do not re-run until a clean number appears.
* **Measuring only completed outputs hides the slow ones.** Listing `*.o` made a pathological split look balanced, because the two worst partitions had produced no `.o` yet. Measure the inputs, or `ps`.
* **Timestamp the log you already have before adding instrumentation.** The runtime's trace carries no clock; piping stderr through a host-side timestamper turns it into a profile with no rebuild.
* **`readelf -SW` column positions shift** between `[ 9]` and `[10]` — the former splits into two tokens. Index relative to a known token, never absolutely.
* **Instruction masks derived by hand are wrong more often than not.** Verify a mask against a real encoding taken from `objdump`, and put that encoding in a test.

## Git Workflow

* ❌ Do not run `git checkout` or `git restore` against the working tree — another agent may be working concurrently in the same directory. A `git status` diff is not necessarily yours; check before assuming. This matters more than it sounds: the builder image build copies whatever is on disk, so an image built at time T bakes in another session's uncommitted edits.
* ❌ Never make discretionary commits. Commit or push only when the user explicitly asks.

## Documentation

* Try to write your work summary to one of the existing documents under `./.agents/docs`.
* ❌ Avoid editing any existing sections of `JOURNAL.md`. Append new entries to the end. Where a later finding contradicts an earlier claim, mark it **CORRECTION** or **SUPERSEDED** at the point it applies rather than rewriting the original, so the evidence trail stays intact. (The sole exception is the `reconcile-journal-ltm` skill, which may remove entries already consolidated into `.agents/docs/LTM/` per the canonical `## LTM Consolidation Record`.)
* ❌ For repo-authored documentation only (e.g. `AGENTS.md`, `README.md`, `.agents/docs/**`), never use full-width parentheses (`（` `）`). Use half-width parentheses (`(` `)`) with a half-width space before/after when adjacent to a non-whitespace character.
* ❌ For repo-authored documentation only, never use full-width colons (`：`). Use a half-width colon followed by a half-width space.
* ❌ Never emit decomposed Japanese. Always write CJK text in NFKC normal form, so voiced hiragana and katakana are the precomposed single code points (`が` U+304C, `ぱ` U+3071) — never a base kana followed by a combining voiced/semi-voiced sound mark (U+3099 / U+309A). Decomposed kana is visually identical to composed kana in every editor and terminal, so it can only be caught by scanning:

  ```sh
  rg -n --pcre2 '[\x{3099}\x{309A}]' --glob '*.md' .
  ```

## Shell Pitfalls (prezto defaults)

The user's shell uses prezto, which sets aliases and options that break non-interactive scripts:

* ❌ `cp src dst` prompts interactively when `dst` exists (prezto aliases `cp` to `cp -i`). Always `rm -f dst` before `cp`. Also kill any process using the destination file first.
* ❌ `cat > file <<'EOF'` and `echo > file` fail with `file exists` when the target exists (prezto enables `NO_CLOBBER`). Workaround: `rm -f file` before writing, or use `tee` / `/bin/cat`.
* ❌ `rm file` prompts for confirmation on some files (prezto aliases `rm` to `rm -i`). Always use `rm -f` for non-interactive deletion.

## Runtime Pitfalls

* ❗ **A HOST ABI IS NOT PORTABLE BETWEEN HOSTS JUST BECAUSE THE NAMES MATCH.** Every WASIX socket trap found on 2026-08-24 produced a module whose import section was perfect and whose behaviour was wrong, and all five were found by MEASURING wasmer before writing the backend. `.agents/docs/WASIX_ABI.md` is the measured record; the shape of the mistakes generalises:
  * a `poll_oneoff` clock timeout of **0 means "return now" to preview1 and "wait forever" to WASIX** — copying `net::wasmedge::ready` hangs the guest on its first `epoll_pwait`, with no error and nothing to grep for;
  * the WASIX address port is **little-endian written and big-endian read back** (`from_ne_bytes` one way, `to_be_bytes` the other) — a symmetric codec is wrong in exactly one direction, and a byte-swapped port is still a valid port that binds successfully somewhere nobody is looking;
  * a struct with a `_padding` byte puts every field one further along than the field list reads;
  * **the same import name can be bound to different functions in different namespaces** — `wasix_32v1.sock_accept` is `sock_accept_v2` and takes four parameters where preview1's takes three;
  * an enum's numbering is not shared: WASIX has `Stream=1 Dgram=2`, WasmEdge has them swapped.
  * ⚠️ Only the arity mistake fails loudly. Measure the signature with a deliberately wrong import type, and read the ORDER from the syscall's source — arity does not give order, and this tree has paid for inferring one twice.
* **An `STT_GNU_IFUNC` symbol's value is its resolver, not its implementation.** Any code path that consumes a symbol value has to handle it. Binding a resolver is worse than failing: it is not a crash, it is a function that returns a pointer and does nothing.
* **`debug/elf` has no `SHT_RELR` constant, even in Go 1.26.** When adding relocation handling, enumerate the section types present in a real fixture rather than the ones the standard library has names for.
* **A missing intrinsic can link to a silent stub.** `link-all` passes `--allow-undefined` and remill ships no-op defaults for its own intrinsics, which once turned every guest `brk` into a no-op. Check with `llvm-nm` on the lifted object: an intrinsic the runtime implements should be `U` there, not `T`.
* **The SHIPPING module must stay within Wasm 2.0.** No proposal beyond it — the released `containerd-shim-wasmedge` and `wasmtime` both rejected earlier modules that needed exception handling, and enabling it shim-side is not on the table. `TestWasmOptEnablesNoProposal` guards this.
  * ⚠️ **SCOPED TO THE SHIPPING ARTIFACT, and the word matters.** The rationale names specific deployment targets; it does not bind a profile that targets something else. An opt-in `--profile <engine>` artifact for an ADVANCED runtime -- wasmer being the case in hand -- may use proposals beyond 2.0, because nothing loads it through a stock runwasi shim. It must never be the default and never `TestWasmOptEnablesNoProposal`'s business.
  * ❗ This was misread once, on 2026-08-24: the rule stated absolutely was applied absolutely, and a whole phase (`load-wasix`, which needs a shared memory -- the threads proposal) was written off as blocked. It was not. If a constraint's justification names a target, check whether your artifact has that target before treating it as a wall.
* **Blocking semantics follow the guest descriptor.** A non-blocking socket operation returns `EAGAIN` or `EINPROGRESS`; only a blocking descriptor parks the process. Timed waits keep one absolute deadline across spurious readiness wakes, and the idle scheduler must poll socket readiness and the earliest deadline together so neither starves the other.
* **Do not collapse host socket errors into one Linux errno.** Map WASI errors by operation and preserve resumable states such as `EAGAIN`, `ALREADY`, and `INPROGRESS`. A generic `EIO` or `ECONNREFUSED` can turn an ordinary asynchronous transition into a false permanent failure.
* **`wasmedge` does not inherit the host environment**; diagnostics need explicit `--env`. And a backgrounded command must *be* the tracked process — wrapping it in `nohup` returns immediately and the child is killed.
