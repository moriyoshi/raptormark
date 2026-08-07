# Bazel Build and the Hermetic SDK

## Summary

The builder image's contents are built by Bazel, and `builder/Dockerfile` has
**zero `RUN` lines** -- it is `COPY . /` over `//builder:stage`, which declares
every file the image gains at the path it has in the image. Bazel runs INSIDE
the elfconv base image because the LLVM tools must link against the LLVM that
built `elflift`. A second mode, `--config=hermetic`, builds the same targets on
a bare host with no Docker, and its equivalence to the image mode is measured
rather than asserted.

## Key Facts

- `TestDockerfileBuildsNothing` fails if a `RUN` line appears in
  `builder/Dockerfile`.
- The pipeline binary can no longer be stale: `//builder:stage` DEPENDS on
  `//cmd/raptormark:builder_tools_linux_arm64`, so Bazel cannot assemble the
  image contents without building it. `raptormark build-tools` is obsolete and
  fails with instructions rather than rebuilding a file nothing reads.
- Byte-identity against the deleted `RUN` lines is a PERMANENT test for the four
  LLVM tools and both C shims. Rust cannot be checked that way and is guarded by
  exclusion tests instead.
- `bazel/llvm_tool.bzl`, `builder/BUILD.bazel` and `bazel/sdk.bzl` are in
  `translateSources`, so editing any of them invalidates every cached object.
- Hermetic mode produces 6 of 10 staged artifacts byte-identical to the image
  build. The 4 LLVM tools differ by LINKAGE MODEL, and the objects they emit are
  byte-identical.
- ❌ Do not run `bazel` directly against this tree. Use `raptormark bazel`.

## Details

### What moved out of the Dockerfile

| was a RUN line | is now |
|---|---|
| 4 x `clang++ ... $(llvm-config ...)` | `//builder:{ecv-prepare,ecv-split,namespace-object,ecv-promote}` |
| 2 x wasi-sdk `clang -c` | `//runtime:{ecv_sp,ecv_globals}` |
| 3 x `cargo build --release` | `//runtime:ecvisor`, `//runtime/loopback:ecvisor`, `//runtime/browser:ecvisor` |
| `curl https://sh.rustup.rs \| sh` | `rules_rust`, Rust 1.88 pinned in MODULE.bazel |
| `COPY builder/_tools/raptormark-builder-tools` | `//cmd/raptormark:builder_tools_linux_arm64`, a DEPENDENCY of the stage |

`//runtime/hosted` and `//runtime/wasix` joined later, each staging its own
archive.

### The evidence, because moving a compile recipe is not free

**4/4 LLVM tools and 2/2 C shims byte-identical** against a transcription of
each deleted `RUN` line, measured in the base image. Both checks are permanent
tests -- `//builder:tools_equivalence_test`, `//runtime:cshim_equivalence_test` --
and both neutralized: flipping `-O2` to `-O1` in `//bazel:llvm_tool.bzl` fails
all four with the intended diagnostic.

⚠️ **Rust is the exception and cannot be checked this way.** rules_rust and
cargo differ in metadata hashing and codegen-unit naming, so `libecvisor.a` is
NOT byte-identical and no arrangement makes it so. What guards it is
`//runtime:profile_exclusion_test` and `//runtime:loader_exclusion_test`, plus
the E2E suite as the behavioural check.

⚠️ `cargo` remains the Rust gate even though Bazel now builds the shipped
archives. `runtime/Cargo.toml` is what `cargo test` reads, so `[profile.release]`
there and `RELEASE_PROFILE` in `//bazel:ecvisor.bzl` have to be changed together --
rules_rust does not read Cargo.toml.

### Running Bazel here, which is its own pile of sharp edges

`raptormark bazel --image <img> <args>` exists because three things bite:

1. the base image entrypoint is `["/bin/bash","--login","-c"]`, so a normal argv
   is truncated to its first word and bazel prints usage and **exits 0**;
2. `$WASI_SDK_PATH` is under `/root` at mode 700, so a non-root uid reports the
   compiler as absent -- it must run as root;
3. which means gazelle and bzlmod write **root-owned** `BUILD.bazel` and
   `MODULE.bazel.lock` into the tree. `restoreOwnership` chowns exactly those
   two names back, and only those, because `runtime/runtime` is pre-existing and
   root-owned and is not ours to take.

Bazel's output base is a named Docker volume: Bazel refuses an output base it
does not own, and a bind mount would put root-owned outputs in the source tree.
`stageImageFiles` copies `//builder:stage` out of the `raptormark-bazel-cache`
volume, which is easy to miss when doing the side build by hand.

### Things that were wrong on the first try, all found by running them

- **`bazel test //...` under `--platforms=wasm32_wasip1` cannot resolve a shell
  toolchain.** Forcing the platform on the command line retargets the LLVM tools
  and the `sh_test`s too. Fixed with a transition
  (`//bazel:wasm_transition.bzl`) so wasm-ness is a property of the target.
- **`-Clto=fat` in `rustc_flags` is rejected**: rules_rust passes
  `-Cembed-bitcode=no`, and setting the flag per-target would leave miniz_oxide
  and adler2 without bitcode anyway. The `//rust/settings:lto` build setting
  reaches the whole graph.
- **Three profiles with `crate_name = "ecvisor"` collide** on one
  `libecvisor.a`. They are three PACKAGES. Renaming the crate was rejected: it
  would change symbol mangling.
- **The 17 `sock_*` symbols are not host imports.** They are
  `ecvisor::net::wasmedge::sock_*`, ecvisor's own functions. A test grepping
  `sock_` would have passed for a reason adjacent to the claim.
- **Both equivalence tests originally exited 0 when the compiler was missing.**
  A "skip" that makes an absent toolchain indistinguishable from a pass. Now a
  hard failure -- the SDK repo rule already guarantees the toolchain exists.

### Cache identity moved with the flags

`bazel/llvm_tool.bzl`, `builder/BUILD.bazel` and `bazel/sdk.bzl` joined
`translateSources` because they carry the tools' compile flags, component lists
and choice of LLVM. `toolsid.go` used to carry a caveat that the Dockerfile held
those flags but was excluded because it ALSO held the runtime build; both halves
stopped being true at once.

`builder/BUILD.bazel` also holds the staging genrule, so editing that
invalidates every object including edits that cannot change a tool. That
over-invalidation is deliberate, on that file's own rule: over-invalidating
costs CPU while under-invalidating serves a stale object for a pipeline that no
longer produces it.

`//bazel:wasi_object.bzl`, `//bazel:ecvisor.bzl` and `//bazel:crates.bzl` stay
OUT: they build things that reach only the final link, the same reason
`runtime/` has never been in the list.

### The hermetic mode, and what it is equivalent to

`bazel build --config=hermetic //builder:stage` produces the complete builder
image contents on a bare host with no Docker, against LLVM 16.0.6 and wasi-sdk
24.0 downloaded and verified by sha256 (`283e9040...`, `ae6c1417...`).

`builder/hermetic_differential.sh` is how to re-measure it, and it answers three
separate questions rather than one blurred one:

| | result |
|---|---|
| staged artifacts byte-identical | **6 of 10** -- all 3 ecvisor archives, both C shims, the Go pipeline binary |
| the 4 LLVM tools | **differ**, and not by drift |
| the bitcode they emit | **differs by 36 bytes** |
| the object that bitcode compiles to | **byte-identical** |

The tools differ because the two LLVMs have different LINKAGE MODELS, not
different behaviour: Debian's `llvm-config --libs` returns `-lLLVM-16`, one
shared library, an 88 KB tool; upstream's returns the static component list,
a self-contained 13.7 MB tool. The 36 bytes are the embedded LLVM version string
at the tail of the identification block -- upstream carries the git hash, Debian
strips it. Disassembled IR is byte-identical once `llvm-dis`'s `ModuleID` line
is excluded; that line is the path llvm-dis READ, and an earlier comparison that
did not exclude it made everything look different for no reason.

Consequences, stated separately because they differ in kind:

- ✅ No correctness hazard for the OBJECT cache. Both tools emit the same `.o`.
- ⚠️ The PARTITION cache misses across a mode switch. `partcache.go` keys a
  partition on its bitcode BYTES and the version string is in them. A cold
  cache, not a wrong answer.
- ❌ **Hermetic tools built on a newer host cannot run inside the image**
  (`GLIBC_2.38 not found`). Hermetic is for host-side builds and CI; staging its
  output into the image needs a host whose glibc is no newer than the image's.

**`--system-libs` is the one recipe change, and it is safe by measurement.**
The upstream LLVM's static components need `-lz` and `-ltinfo`; the Dockerfile
recipe never passed it and got away with it because Debian's
`llvm-config --system-libs` prints nothing at all. `//builder:tools_equivalence_test`
proves the flag is inert on the image path rather than asserting it: an uncached
run after the change produced the same four hashes, and `bazel aquery` confirms
the flag really is on the image-mode command line. `-ltinfo` needed one more
thing -- the host has `libtinfo.so.6` but no `libtinfo.so` dev symlink, so the
repo rule makes that symlink inside the external repo and adds a `-L`. That is
the one place the mode is not hermetic, and it is a runtime library rather than
a dev package.

**x86_64 hermetic does not work and fails saying why.** LLVM publishes no
`clang+llvm` release for 16.0.6 on x86_64 (16.0.4 is the newest 16.x that has
one), and a patch-version swap would change what the mode measures. An x86_64
wasi-sdk entry was drafted and REMOVED: its hash could not be verified from this
host, and a plausible-looking unchecked hash reads as authority.

### rules_oci was asked for and not done

`oci_pull` would materialise the **4.7 GB local-only** base into Bazel's repo
cache and `oci_load` would re-tar it every build, to add ~20 MB of files that
Docker layers on for free. Worth revisiting only if the patched base is ever
published to a registry.

## Files

- `builder/Dockerfile`: `COPY . /` over `//builder:stage`, no `RUN` lines.
- `builder/BUILD.bazel`: the four LLVM tools, the staging genrule, `tools_equivalence_test`.
- `bazel/llvm_tool.bzl`, `bazel/sdk.bzl`, `bazel/wasm_transition.bzl`, `bazel/ecvisor.bzl`, `bazel/wasi_object.bzl`, `bazel/crates.bzl`.
- `builder/hermetic_differential.sh`: the mode comparison.
- `runtime/profile_exclusion_test.sh`, `runtime/loader_exclusion_test.sh`.
- `internal/builder/toolsid.go`: `translateSources`.

## Test Coverage

- `//builder:tools_equivalence_test` -- image-mode only (`target_compatible_with`), and Bazel reports it as SKIPPED rather than omitting it. Its reference IS the historical Dockerfile recipe, which cannot link against upstream LLVM; ❌ adding `--system-libs` to the reference would make it compare the rule against a copy of itself.
- `//runtime:cshim_equivalence_test`.
- `//runtime:profile_exclusion_test`, `//runtime:loader_exclusion_test`.
- `TestDockerfileBuildsNothing`.
- The standard gate: `raptormark bazel --image raptormark-elfconv-base-patched:<tag> test //...`. Fast (seconds, warm), currently 13 targets.

## Pitfalls

- ⚠️ `bazel test //...` does **not** run the Go tests: `//internal/builder` and
  `//internal/rootfs` are tagged `manual` because they read the repo tree by
  relative path and shell out to `go`. `go test` is still the authority for Go.
  Two gates where one of them lies is worse than one gate.
- ❗ **A new Go package needs a `BUILD.bazel` AND a `deps` entry, and a new FILE
  in an existing package needs adding to that package's `srcs`.** `gofmt`,
  `go build`, `go vet` and `go test` all stay green when you forget; Bazel
  reports "N tests pass and M were SKIPPED", which reads like caching. Measured:
  the omission broke `//cmd/raptormark:builder_tools_linux_arm64`, which
  `//builder:stage` depends on, so the builder image could not have been built.
  The one-line check is in [[testing-and-regression-method]].
- **A side-built image must LAYER ONTO the existing patched base, not rebuild
  one.** If the BuildKit layer cache has been evicted, re-applying the patch
  series yields a different image id, `BASE_ID` changes, and every cached
  translated object is invalidated. Verify the two labels BEFORE building.
- ⚠️ **`build-image --tag X --base-tag Y --skip-base` DOES NOT WORK for a side
  build.** `--skip-base` skips the UNPATCHED base but still re-applies the patch
  series onto `raptormark-elfconv-base:Y`, which may not exist -- only the
  patched image survived the recovery. It fails with a Docker Hub pull-access
  error that reads like an authentication problem and is not one.
- **Confirm a rebuilt image contains your change on the ARTIFACT, not the
  labels.** `sha256sum /opt/ecvisor/libecvisor.a` covers a `runtime/` change; a
  change to `internal/builder` has no witness but the emitted object.
- `RAPTORMARK_BAZEL_SDK=hermetic` was declared and failed saying it was not
  implemented, rather than silently falling back to the image toolchain -- which
  would have "proved" an equivalence it never tested. That is the pattern for
  any half-built mode.
- See [[recovery-and-builder-provenance]] for why the base image cannot be
  rebuilt from this tree, and [[translation-linking-and-object-cache]] for what
  `TranslateSH` membership costs.
