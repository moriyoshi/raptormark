# Build System and Driver Synthesis

## Summary

Two questions this answers that the stage documents do not: how a CORRECT
builder image comes into existence and how you know the one you have is it, and
how the four stages are driven from a single command. Bazel produces the image's
contents inside the base image; `raptormark build` and `raptormark run` drive
the pipeline and execute the result. Identity is the thread running through
both -- most of what goes wrong here is a stale artifact that looks current.

## Included Documents

| Document | Focus |
|----------|-------|
| [bazel-build-and-hermetic-sdk.md](./bazel-build-and-hermetic-sdk.md) | Bazel owning image contents, equivalence tests, cache identity, hermetic mode |
| [pipeline-driver-and-cli.md](./pipeline-driver-and-cli.md) | `raptormark build` / `run`, runtime flags, driver defects |
| [recovery-and-builder-provenance.md](./recovery-and-builder-provenance.md) | Image provenance, patch series, Docker preservation |

⚠️ `recovery-and-builder-provenance.md` also feeds
[build-pipeline-synthesis.md](./build-pipeline-synthesis.md), which reads it for
what the pipeline INHERITED from the recovery. This document reads it for image
IDENTITY. The dual membership is deliberate.

## Stable Knowledge

- `builder/Dockerfile` has **zero `RUN` lines**. It is `COPY . /` over
  `//builder:stage`, which declares every file the image gains at its image
  path. `TestDockerfileBuildsNothing` fails if a `RUN` appears.
- Bazel runs **inside** the elfconv base image, because the LLVM tools must link
  against the LLVM that built `elflift`.
- The pipeline binary **can no longer be stale**: `//builder:stage` depends on
  `//cmd/raptormark:builder_tools_linux_arm64`, so Bazel cannot assemble the
  image contents without building it.
- Byte-identity against the deleted `RUN` lines is a permanent test for the four
  LLVM tools and both C shims. Rust cannot be checked that way -- rules_rust and
  cargo differ in metadata hashing and codegen-unit naming -- and is guarded by
  the exclusion tests plus E2E instead.
- `bazel/llvm_tool.bzl`, `builder/BUILD.bazel` and `bazel/sdk.bzl` are in
  `translateSources`, so editing any of them invalidates every cached object.
  Over-invalidating costs CPU; under-invalidating serves a stale object for a
  pipeline that no longer produces it.
- `--config=hermetic` builds the same targets with no Docker. 6 of 10 staged
  artifacts are byte-identical; the 4 LLVM tools differ by LINKAGE MODEL, and
  **the objects they emit are byte-identical**, which is the one that decides.
- The driver is NOT a second pipeline: every step is the same library call
  `e2e/` already makes, in the same order.
- `raptormark run` exists because one combination has to be right and blames the
  guest when it is not: the sidecar inside the preopened directory, and
  `RAPTORMARK_ROOTFS` as the **guest** path.

## Operational Guidance

### The gates

```sh
raptormark bazel --image raptormark-elfconv-base-patched:<tag> test //...
raptormark bazel --image raptormark-elfconv-base-patched:<tag> build //builder:stage
bazel build --config=hermetic //builder:stage        # host-side, no Docker
```

❌ **Do not run `bazel` directly against this tree.** Three things bite and
`raptormark bazel` handles all three: the base entrypoint is
`["/bin/bash","--login","-c"]` so a normal argv is truncated to its first word
and bazel prints usage and **exits 0**; `$WASI_SDK_PATH` is under `/root` at
mode 700 so a non-root uid reports the compiler as absent; and running as root
leaves root-owned `BUILD.bazel` and `MODULE.bazel.lock` in the tree, which
`restoreOwnership` chowns back -- exactly those two names and no others.

### The side-build recipe

A side build must LAYER ONTO the existing patched base, never rebuild one. If
the BuildKit layer cache has been evicted, re-applying the patch series yields a
different image id, `BASE_ID` changes, and every cached object is invalidated.
Verify the two labels BEFORE building:

```sh
docker image inspect --format \
  '{{index .Config.Labels "raptormark.base_id"}} {{index .Config.Labels "raptormark.translate_sh"}}' \
  raptormark-builder:<tag>
```

```sh
raptormark bazel --image raptormark-elfconv-base-patched:<tag> build //builder:stage
docker build -t raptormark-builder:<newtag> -f builder/Dockerfile \
  --build-arg ELFCONV_BASE=raptormark-elfconv-base-patched:<tag> \
  --build-arg BASE_ID=<verbatim> --build-arg TRANSLATE_SH=<verbatim> \
  builder/_stage
```

⚠️ The context is `builder/_stage`, **not** the repo root -- the Dockerfile is
`COPY . /`, so passing `.` would copy the repository into the image.
`stageImageFiles` copies `//builder:stage` out of the `raptormark-bazel-cache`
volume, which is easy to miss by hand.

❌ **`raptormark build-tools` is obsolete and fails with instructions.** It used
to be a mandatory first step; it now rebuilds a file nothing reads, and a
command that looks like it worked is the same failure it was written to prevent.

### Confirming a rebuild took

On the **artifact**, not the labels. `sha256sum /opt/ecvisor/libecvisor.a`
covers a `runtime/` change. It does NOT cover a change to `internal/builder`,
whose only witness is the emitted object -- translate something and look at it
with `llvm-objdump -r` or `llvm-nm`. An identical hash means a cached layer
shipped the old runtime, and the next run will "fail to reproduce the fix" for
reasons unrelated to the fix.

⚠️ `llvm-nm` is not on `PATH` in the builder image; it is at
`/root/wasi-sdk-24.0-arm64-linux/bin/llvm-nm`. A `command not found` makes every
grep return 0, which reads as "the symbol is absent from both".

## Files

- `builder/Dockerfile`, `builder/BUILD.bazel`, `builder/hermetic_differential.sh`.
- `bazel/llvm_tool.bzl`, `bazel/sdk.bzl`, `bazel/wasm_transition.bzl`, `bazel/ecvisor.bzl`.
- `internal/builder/toolsid.go`: `translateSources`, the `TranslateSH` label.
- `internal/pipeline/build.go`, `run.go`: the driver and `runtimeArgs`.
- `cmd/raptormark/`: nine subcommands.

## Tests

- `//builder:tools_equivalence_test` -- image-mode only via
  `target_compatible_with`; Bazel reports it SKIPPED rather than omitting it.
  Its reference IS the historical Dockerfile recipe. ❌ Adding `--system-libs`
  to the reference would make it compare the rule against a copy of itself.
- `//runtime:cshim_equivalence_test`, `TestDockerfileBuildsNothing`.
- `internal/pipeline`: `TestOnlyTheEntryCarriesPlugins`,
  `TestRuntimeArgsSpellDirTheRightWayRound`, `TestSupervisorHonoursTheProfile`,
  `TestLinkRequestProfileReachesTheTool`.
- `e2e/pipeline_test.go`, `pipelinemulti_test.go`, `pipelinehosted_test.go`.

## Pitfalls

- ⚠️ `bazel test //...` does **not** run the Go tests: `//internal/builder` and
  `//internal/rootfs` are tagged `manual`. `go test` is the Go authority. Two
  gates where one of them lies is worse than one gate.
- ❗ **A new Go package needs a `BUILD.bazel` AND a `deps` entry; a new FILE in
  an existing package needs adding to `srcs`.** The Go gate stays green either
  way, and Bazel reports "N pass, M SKIPPED", which reads like caching. Measured
  twice: once it broke `builder_tools_linux_arm64` so the image could not have
  been built, and once six `e2e/*_test.go` files were compiled by nothing while
  Bazel reported 13/13.
- ❌ **Hermetic tools built on a newer host cannot run inside the image**
  (`GLIBC_2.38 not found`). Hermetic is for host-side work and CI.
- ⚠️ The PARTITION cache misses across a hermetic/image switch --
  `partcache.go` keys on bitcode bytes and the 36-byte LLVM version string is in
  them. A cold cache, not a wrong answer.
- ⚠️ **`build-image --tag X --base-tag Y --skip-base` does not work for a side
  build.** `--skip-base` skips the UNPATCHED base but still re-applies the patch
  series onto `raptormark-elfconv-base:Y`, which may not exist. It fails with a
  Docker Hub pull-access error that reads like authentication and is not.
- `raptormark-builder:latest` is **not** the newest builder; pass an explicit
  tag or fail deep inside elflift with an error that reads like bad input.
- ❌ Do not prune Docker. The patched bases and reclaimable BuildKit cache are
  the only copies of things that exist nowhere else, and a patched base rebuilt
  from cache is not guaranteed to reproduce its image id.
- **Build costs are wildly non-uniform.** Cost follows the largest single
  function, not total volume: the OpenSSL closure translates in 28 minutes, the
  smaller nginx closure took 6.5 hours. Do not extrapolate.
- **A degradation path must not render a programming error as a capacity
  limit** -- the fuse fallback once absorbed a wrong-argument failure and
  reported it as a closure that did not fit.
