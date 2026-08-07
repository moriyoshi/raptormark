# Documents for both humans and coding agents

* [README.md](./README.md) ... goal, the four pipeline stages, the ecvisor runtime, repository layout, building and running, and the honest status. This is the canonical architecture document; there is no separate `ARCHITECTURE.md`.

# Documents for coding agents

* [./.agents/docs/JOURNAL.md](./.agents/docs/JOURNAL.md) ... unconsolidated findings and consolidation records. Ordinary work is append-only; `reconcile-journal-ltm` may remove entries only after auditing their durable knowledge into LTM or TODO.
* [./.agents/docs/LTM/INDEX.md](./.agents/docs/LTM/INDEX.md) ... long-term memory index for durable project knowledge under `./.agents/docs/LTM/`.
* [./.agents/docs/TODO.md](./.agents/docs/TODO.md) ... open to-do items extracted from JOURNAL.md during `good-sleep` consolidation. Check and update this file when picking up or finishing work. **Every entry must be re-verified against the tree before acting on it** — much of this tree was reconstructed rather than recovered, so an entry may describe an intent the code never reached, or a gap that has since been closed by another session.
* [./.agents/docs/QUALITY_GATE.md](./.agents/docs/QUALITY_GATE.md) ... the standard verification gate to run before declaring a change complete: the Go gate, the Rust gate, and how to run the env-gated E2E suite.

# Rules and protocols

## General

* raptormark translates an aarch64 Linux container image ahead of time into a single WebAssembly module, supervised by ecvisor (a Rust `wasm32-wasip1` staticlib) instead of a kernel. Read `./README.md` before changing stage boundaries.
* The pipeline is four stages: discovery (`internal/image`) -> fusing (`internal/fuse`) -> translation (`raptormark translate-one`, driven by `internal/translate`) -> linking (`internal/link` + `link-all`). Stages 3 and 4 run *inside* the builder Docker image; stages 1 and 2 run on the host.
* The CLI uses `github.com/alecthomas/kong`. Keep new commands consistent with the existing kong layout in `cmd/raptormark/`.
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
* `_recovery/` is untracked evidence from the 2026-08-01 rebuild, kept on disk deliberately. Read it; never clean it up.

## Do not prune Docker

The raptormark images and the reclaimable BuildKit cache are the only copies of things that exist nowhere else. ❌ Avoid `docker system prune` and `docker buildx prune`. The pinned elfconv base and the patched builders are not reproducible from this tree alone. See `.agents/docs/LTM/recovery-and-builder-provenance.md` for the durable provenance and preservation rules.

## Building

* Go 1.26 and cargo are on `PATH` (Go via mise, cargo via rustup). No extra setup is needed.
* Run `gofmt -w` on every Go file you change before running `go build`, `go vet`, or `go test`, and before reporting a change as done. Likewise `cargo fmt` for every Rust file you change under `runtime/`.
* The standard local gate for any Go change you make — this applies to subagents too:
  ```
  gofmt -l <changed files>      # must print nothing
  go build ./...
  go vet ./...
  go test ./...                 # or a focused package: go test ./internal/<pkg>/
  ```
* The standard local gate for any Rust change under `runtime/`:
  ```
  cargo fmt --manifest-path runtime/Cargo.toml --check
  cargo test --manifest-path runtime/Cargo.toml
  cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1
  ```
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
* **❌ A raw `docker build` does NOT rebuild the pipeline binary.** `builder/Dockerfile` does `COPY builder/_tools/raptormark-builder-tools`, so the translate-one / link-all that runs inside the image is a **prebuilt host binary**. `raptormark build-image` rebuilds it; the side-build recipe above deliberately avoids `build-image`, and therefore skips it. Rebuild it first, every time:
  ```
  raptormark build-tools --base raptormark-elfconv-base-patched:<tag>
  ```
  This cost a whole void gate on 2026-08-18: a `-fPIC` change to `internal/builder/translateone.go` never reached the image, and everything a careful check looks at said it had — `raptormark.translate_sh` moved (it hashes the pipeline's SOURCE, which really had changed), the object cache went cold and re-translated for 20 minutes, and `libecvisor.a` differed. All three are downstream of *the source changed*; none of *the binary changed*. It also poisoned 45 cached objects under a key that claimed the new pipeline.
* **Confirm a rebuilt image actually contains your change**, and confirm it on the ARTIFACT, not the labels. `sha256sum /opt/ecvisor/libecvisor.a` inside the old and new images must differ — that covers a `runtime/` change, because libecvisor.a is built in the image. It does NOT cover a change to `internal/builder`, whose only witness is the emitted object: translate something and look at it (`llvm-objdump -r`, `llvm-nm`) before believing the change is in. An identical hash means a cached layer shipped the old runtime, and the run that follows will "fail to reproduce the fix" for a reason that has nothing to do with the fix.
* **Build costs are wildly non-uniform — do not extrapolate.** Cost follows the largest single function, not total volume: the OpenSSL closure translates in 28 minutes, the smaller nginx closure took 6.5 hours.

## Testing

* Make sure that regression tests are ready for your fix.
* **Audit the code before you write the test for it.** Read what the code actually does and state it, then write the test against that. A test written from your model of the fix inherits that model's blind spots, and a test that passes for the wrong reason is worse than no test: it certifies the one behavior nobody checked, and it does so in green.
* Before trusting any check — a new test, a `grep`, a differential probe, a built artifact — ask **what would a PASS look like if this claim were false?** If there is no answer, the check proves nothing yet and must not be cited as evidence. Then neutralize it: deliberately break the fix and confirm the test fails with the diagnostic you intended. ❌ A COMPILE error is not a valid neutralization; it proves the test names a symbol, not that it observes a behavior. Every regression guard in this tree was checked this way, and it caught one guard whose assertion was mis-specified and would never have fired.
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
  them, which contradicts `.agents/docs/QUALITY_GATE.md` §4 and the
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

* **An `STT_GNU_IFUNC` symbol's value is its resolver, not its implementation.** Any code path that consumes a symbol value has to handle it. Binding a resolver is worse than failing: it is not a crash, it is a function that returns a pointer and does nothing.
* **`debug/elf` has no `SHT_RELR` constant, even in Go 1.26.** When adding relocation handling, enumerate the section types present in a real fixture rather than the ones the standard library has names for.
* **A missing intrinsic can link to a silent stub.** `link-all` passes `--allow-undefined` and remill ships no-op defaults for its own intrinsics, which once turned every guest `brk` into a no-op. Check with `llvm-nm` on the lifted object: an intrinsic the runtime implements should be `U` there, not `T`.
* **The emitted module must stay within Wasm 2.0.** No proposal beyond it — the released `containerd-shim-wasmedge` and `wasmtime` both rejected earlier modules that needed exception handling, and enabling it shim-side is not on the table. `TestWasmOptEnablesNoProposal` guards this.
* **Blocking semantics follow the guest descriptor.** A non-blocking socket operation returns `EAGAIN` or `EINPROGRESS`; only a blocking descriptor parks the process. Timed waits keep one absolute deadline across spurious readiness wakes, and the idle scheduler must poll socket readiness and the earliest deadline together so neither starves the other.
* **Do not collapse host socket errors into one Linux errno.** Map WASI errors by operation and preserve resumable states such as `EAGAIN`, `ALREADY`, and `INPROGRESS`. A generic `EIO` or `ECONNREFUSED` can turn an ordinary asynchronous transition into a false permanent failure.
* **`wasmedge` does not inherit the host environment**; diagnostics need explicit `--env`. And a backgrounded command must *be* the tracked process — wrapping it in `nohup` returns immediately and the child is killed.
