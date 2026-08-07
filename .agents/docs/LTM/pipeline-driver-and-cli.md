# The Pipeline Driver and the CLI

## Summary

`raptormark build <image> --out DIR --builder <img>` runs all four stages --
discovery, fuse, translate, link -- and `raptormark run <module>` executes the
result. Before `internal/pipeline` existed the four stages were only ever strung
together by the `e2e/` suite, which is why `image.Plugins` sat with "no
production caller" for three sessions: there was no command for one to live in.

## Key Facts

- The driver is NOT a new pipeline. Every step is the same library call `e2e/`
  already makes, in the same order. A driver that reimplemented any of it would
  be a second thing to keep in step with the one that is actually exercised.
- `build` discovers plugins by default (`--plugins auto`) and fuses each as its
  own dlopen-able unit. Discovery finds MORE than you planted: on a
  `postgres:17`-derived fixture, 5 plugins, 3 of them the base image's own
  OpenSSL modules. A unit count is not a plugin count.
- `run` exists because one combination has to be right and blames the guest when
  it is not: the sidecar must be inside the preopened directory and
  `RAPTORMARK_ROOTFS` must be the **guest** path.
- **The three runtimes spell the directory flag three different ways**, and none
  fails loudly when swapped. `runtimeArgs` is the one place that knows.
- `--runtime wasmer` always passes `--net`, and must.
- Only the ENTRY program carries plugins. `fuse.Fuse` and `fuse.FuseWithUnits`
  ERROR on a plugin they cannot satisfy rather than skipping it.
- `pipeline.Result` carries `[]Artifact` (guest path, content hash, registry
  index, side-module path), not counts.

## Details

### What it does

```
$ raptormark build raptormark-tmp-pgdlopen:latest --out DIR --builder raptormark-builder:<tag>
discovery: 1 program(s), entry /usr/bin/pgdlhost
discovery: 5 plugin(s), 0 excluded
fuse: 6 image(s)
link: 6 object(s) -> DIR/app.wasm
  programs: 1, units: 5
```

`e2e/pipeline_test.go` guards it and asserts on the returned `Result` rather
than scraping the printed summary -- a test that parsed the formatting would keep
passing if the numbers went wrong.

`raptormark build --profile hosted --side-out` on a DYNAMIC, MULTI-PROGRAM
image, driven by `e2e/testdata/hostedembedder.mjs`, serves both mid-run triggers
on demand: a `dlopen` and an `execve`, `loads served: 2`, exit 0.

### The directory flags

```
wasmedge 0.17.1   --dir    GUEST:HOST     e.g. --dir /:/out
wasmtime 46.0.1   --dir    HOST::GUEST    e.g. --dir /out::/
wasmer   7.3.0    --volume HOST:GUEST     (the wasmtime order, the wasmedge separator)
```

Verified against the binaries, not assumed. Swapping them does not fail loudly:
the runtime opens *something* and the guest simply cannot find its sidecar, at
which point ecvisor reports the rootfs "set but unreadable", runs with NO exec
map and NO dlopen map, and every `execve` falls back to program 0 while every
`dlopen` fails with "cannot open shared object file" -- both of which read as a
defect in the guest.

`TestRuntimeArgsSpellDirTheRightWayRound` asserts both spellings AND that they
DIFFER, because a single shared spelling would be silently wrong for one of
them. Also pinned: wasmedge accepts `--env` only BEFORE the module path; after
it, the flag is handed to the GUEST and the variable is simply absent.

### Four defects the driver produced, and where each was caught

**Caught by tests before the code ran once:**

- **`sanitise` was not injective.** It mapped BOTH `/` and `.` to `_`, so
  `/a/b.c`, `/a.b/c`, `/a/b/c` and `/a.b.c` all became `a_b_c`. Two fused images
  written to one file: the second overwrites the first, the second translation
  runs on the wrong bytes, and the module ends up holding one program twice
  under two names with the exec map pointing both guest paths at it. Nothing
  fails; the wrong program runs. Now escape-then-map (`_` -> `__`, then `/` ->
  `_`), which cannot collide.
- **`guestPathOf` accepted a path outside the root.** `filepath.Rel` SUCCEEDS
  for one, returning a `../..` form, so the error check that looked sufficient
  was not: a stray host path became the guest path `/../../elsewhere/lib.so` and
  would have been written into the dlopen map as if it were real.

**Caught only by running it:**

- **A fallback that lied.** `PlanLayoutFor` was handed GUEST paths, so it failed
  with `no such file or directory` -- and the "fallback is not an error" branch,
  written for a legitimate overflow, swallowed it and reported "the closure did
  not fit one shared layout". Every image lost layout sharing, and the message
  named a capacity limit for what was a wrong argument. There is now a pre-check
  that stats every host path and fails loudly. The fallback is correct policy;
  what was wrong was its REACH.
- **`os.Exit` in a library function.** `Run` passed a guest's non-zero status
  through with `os.Exit(ee.ExitCode())`, which kills the process mid-call: a
  test cannot assert on what happened, `t.TempDir` cleanup never runs, and no
  caller other than `main` can use the function. Now a typed `GuestExitError`,
  unwrapped in `main` before the error message is printed -- because a guest
  exiting 3 is its ANSWER, not raptormark failing, and a script testing `$?` has
  to be able to tell them apart.

**Caught by reading afterwards, and a one-program fixture cannot express
either:**

- **Every plugin was being fused into every NON-ENTRY program.** `opts` carries
  every discovered plugin and only the entry is supposed to fuse them, but the
  non-entry branch passed the same `opts` to `fuse.Fuse`. On `postgres:17` all
  79 plugins would go into each of 71 programs, defeating the per-unit design.
  And it would not merely bloat: `fuse.Fuse` and `fuse.FuseWithUnits` ERROR on a
  plugin they cannot satisfy (unlike `fuse.FuseClosure`, which degrades and
  reports), so one program that cannot resolve one plugin fails the WHOLE build
  with a message naming a plugin that program had no business loading. Extracted
  as `optionsForNonEntryProgram` so the rule is testable.
- **`Result.Skipped` was always nil.** A field printed by `Run` with a ⚠️ prefix
  that nothing ever populated, because the two fuse entry points this driver
  uses ERROR rather than skip. Removed, with the reason recorded on the struct.
  Same defect `translate.LinkRequest.toolFlags` carries a comment about -- "a
  struct field that is never read is invisible in review precisely because it
  looks like it is doing something" -- reintroduced in a new file by someone who
  had read that comment three hours earlier.

### The bug the driver found in the runtime

Running the driver on a two-program image produced `FAIL execve: Exec format
error` on the DEFAULT profile, immediately after a successful `dlopen` in the
same guest. `sys_execve` called `ensure_unit_loaded`, which is `dlopen`'s WHOLE
operation: the loader seam, PLUS a refusal of any unit carrying its own
`.ecv.tls`, PLUS a merge into the running image, PLUS ifuncs and constructors.
Every one of those except the seam is wrong for `execve`, which REPLACES the
image -- `exec_into` rebuilds the dispatch tables and marks the process
not-started, so `setup_tls` runs fresh.

**A fused DYNAMIC program always carries `.ecv.tls`**, so this was ENOEXEC for
every realistic glibc program. Split into `ensure_unit_code` (the seam alone) and
`ensure_unit_loaded`.

⚠️ **Why 109 passing E2E tests missed it.** `e2e`'s `compileGuest` builds guests
with `gcc -static`, and a static image carries no `.ecv.tls`. Every execve test
in the suite exercised the one shape that happened to work. The suite was not
thin; it was UNIFORM, and the uniformity was invisible because nothing named it.
A fixture convention shared by every test in a family becomes an unstated
assumption of the whole family.

## Files

- `internal/pipeline/build.go`: `Build`, `Result`, `Artifact`, `sanitise`, `guestPathOf`, `optionsForNonEntryProgram`, `suspendViaCallFor`.
- `internal/pipeline/run.go`: `Run`, `runtimeArgs`, `GuestExitError`, `RuntimeArgsForTest`.
- `cmd/raptormark/`: nine subcommands -- `build`, `run`, `build-image`, `bazel`, `translate-one`, `link-all`, `oci`, `serve`, and the obsolete `build-tools`.

## Test Coverage

- `e2e/pipeline_test.go`, `e2e/pipelinemulti_test.go`, `e2e/pipelinehosted_test.go`.
- `internal/pipeline`: `TestOnlyTheEntryCarriesPlugins`, `TestRuntimeArgsSpellDirTheRightWayRound`, `TestSupervisorHonoursTheProfile`, `TestLinkRequestProfileReachesTheTool`.
- `RuntimeArgsForTest` is exported (following `BuildForTest`) so `e2e/wasixnet_test.go` takes its wasmer argv from the same source the unit test pins. Two strings proven against two different things, with nothing comparing them, is how `--volume` regresses to `--mapdir` while both tests stay green.

## Pitfalls

- ⚠️ **One new assertion in `TestOnlyTheEntryCarriesPlugins` CANNOT fail, and
  the test says so.** It asserts that deriving the non-entry options does not
  mutate the caller's; `optionsForNonEntryProgram` takes `fuse.Options` BY
  VALUE, so nothing it does can change the caller's length. Neutralization
  confirmed it: rewriting the body as `opts.Extra = opts.Extra[:0]` -- which
  looks exactly like an in-place truncation bug -- leaves the assertion green. It
  is kept as a guard against a future `*fuse.Options`, but the comment records
  that it proves nothing today, because the alternative is a green assertion a
  later reader cites as evidence.
- ⚠️ `go build` passed while Bazel could not build the image at all: the new
  package had no `BUILD.bazel` and `//cmd/raptormark:raptormark_lib` imports it.
  `bazel test //...` went from 12 passing to "3 tests pass and 9 were SKIPPED",
  which is easy to skim past as a caching artifact. Four `GoCompilePkg` targets
  had failed, including `builder_tools_linux_arm64`.
- **A degradation path must not render a programming error as a capacity
  limit.** That is the general form of the fallback defect above.
- **The `os.Exit` was found while checking that a neutralization failed for the
  RIGHT REASON.** The first pass showed `FAIL` and it would have been easy to
  record a successful neutralization; the message was missing, and that absence
  was the defect.
- **A README is not a set of independent claims.** Every individual edit to it
  was correct and verified; what went unchecked was whether the SURROUNDING text
  was still true. Adding a capability falsifies every sentence that assumed its
  absence, and those sentences are usually nowhere near the edit. The sharpest
  example: the patch count sat 300 lines from the patch that was added.
- See [[dynamic-side-module-loading]] for the units this driver produces,
  [[wasix-and-wasmer]] for `--profile wasix`, and
  [[image-discovery-and-rootfs]] for stages 1 and 2.
