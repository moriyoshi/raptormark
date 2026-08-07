# Quality gate

What to run before declaring a change complete. Everything here is cheap enough
to run every time except section 4, which is opt-in and named explicitly.

A gate is not a formality. Section 5 is the part that actually catches things.

## 1. The Go gate

Applies to every change under `cmd/`, `internal/`, or `e2e/`. This applies to
subagents too.

```sh
gofmt -l <changed files>      # must print nothing
go build ./... raptormark/tools/decode-oracle/...
go vet   ./... raptormark/tools/decode-oracle/...
go test  ./... raptormark/tools/decode-oracle/...   # or focused: go test ./internal/fuse/
```

⚠️ **Name both patterns.** `tools/decode-oracle` is a separate module (licensing;
see below), and **a workspace does not make `./...` recursive across modules** --
measured, not assumed: `go list ./...` from the root returns zero of its
packages even with `go.work` in place, because a relative pattern resolves
against the module you are standing in. Bare `./...` silently skips 36 tests.

❌ Not `go test all`. In workspace mode that also pulls in dependencies -- 152
packages here, kong included -- so it runs third-party tests that can fail for
reasons that are not yours.

`go.work` is what makes the second pattern resolvable at all;
`internal/builder`'s `TestWorkspaceCoversEveryModule` fails if a module in the
tree is missing from it.

Go 1.26 is on `PATH` via mise. Fix violations and re-run until clean. Do not
declare a change complete with a failing build, vet, or test.

`go test ./...` must stay runnable without Docker, root, or network. The `e2e/`
package is env-gated rather than build-tagged, so it compiles and vets on this
path and skips cleanly.

## 2. The Rust gate

Applies to every change under `runtime/`.

```sh
cargo fmt --manifest-path runtime/Cargo.toml --check
cargo test --manifest-path runtime/Cargo.toml
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1
# The other network profiles. The runtime has a `[features]` section selecting
# its NetBackend, and a change can compile under one feature set and not
# another: each profile compiles a DIFFERENT backend module, and only
# `net-wasmedge` and `net-browser` carry `extern` blocks at all.
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1 \
  --no-default-features --features net-loopback
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1 \
  --no-default-features --features net-browser
```

`runtime/` ships as a `wasm32-wasip1` staticlib; `Cargo.toml` also declares
`rlib` for host-side pure-module tests.

**CORRECTED 2026-08-13: both of the caveats that used to live here are gone, and
all three commands are expected to be GREEN.** Measured on this date with rustc
1.97.1: `cargo test` compiles and runs **47 tests, 0 failures**, and
`cargo check --target wasm32-wasip1` reports **zero errors**. Treat any failure
in either as a real regression.

**Re-measured 2026-08-15: 81 tests, 0 failures.**

**Re-measured 2026-08-17: 79 tests, 0 failures** (`cargo test --manifest-path
runtime/Cargo.toml`, lib target; doc-tests are 0).

**Re-measured 2026-08-19: 147 tests, 0 failures.** The rise from 133 is the
backend seam (`runtime/src/net`): the sockaddr codec and the loopback backend
are the first socket code in this tree reachable from `cargo test` at all, since
`sys.rs` is gated to `wasm32` and always has been.

**Re-measured 2026-08-20: 166 tests, 0 failures.** Twelve of the additions are
the DNS wire format (`net::dns`), which is pure and therefore fully host-tested;
the other seven are scheduler OUTCOMES. Before `schedule_after_suspend` was split, its idle path called a wasm
import through a gated module and its terminal states called
`std::process::exit` -- which ends the test binary rather than returning
something to assert on, so "a hang is a deadlock, not a clean exit" could not be
stated as a test at all.

**Re-measured 2026-08-21: 169 tests, 0 failures.** The three additions are the
clock split: the clock-id -> timebase mapping, the deadline rebase, and the one
that pins the defect itself -- `mono_nanos()` reading time since BOOT rather
than since the epoch, which is the only host assertion that fires when the
monotonic clock is pointed back at the wall clock. The behavioural half is not
here and cannot be: it needs a clock that STEPS, which lives in
`e2e/clock_test.go` behind the Node host.

**Re-measured 2026-08-22: 235 tests before, 231 after, 0 failures — a
deliberate DROP of 4, which is what this figure exists to make visible.** The
bounded-snapshot environment gate was removed entirely by the user's ruling, and
with it the seven host tests in `diag::bounded_snapshot_tests` that existed only
to pin that gate's polarity, its opt-out spelling, and its wiring in
`sys::init_diag_flags`. There is no gate left for them to describe. Three tests
replaced them: `diag::tests::the_removed_snapshot_gate_stays_removed` (a raw
source scan over `runtime/src` that fails if the variable name reappears) and
two in `arena::tests` that pin the new `Option<SnapDiff>` return of
`bytes_differing_outside` — one that a bounded snapshot yields `None` because
there is no oracle, one that a full snapshot yields a real comparison. Net
−7 + 3 = −4. ⚠️ Wall time went 0.67 s → 7.1 s: the oracle test runs the probe's
real O(384 MiB) loop, which is the only way to observe that the comparison
happens at all.

The pre-session baseline was **76**, established by counting rather than by
arithmetic: at commit `980c1e7`, which predates that session's runtime change
(`grep -c mkdir_with_mode` = 0), `runtime/src` carries 76 `#[test]` attributes
and `vfs/mod.rs` carries 12. It now carries 79 and 15 -- exactly the three
guards that session added and kept. A fourth was added and WITHDRAWN within the
session, which is what made a first attempt at this tally read "net +2, baseline
77"; both figures were wrong and are corrected here.

So the unexplained gap against the 08-15 reading of 81 is **five tests, not
four**, and it is NOT reconciled -- either other work removed them in between, or
the two readings counted different things. Recorded rather than smoothed over:
this is precisely the drop the number exists to make visible, and overwriting it
with a fresh count would have destroyed the only signal it carries. Treat it as a
question, not as a failure.

`AGENTS.md` deliberately no longer carries its own copy of this figure. It used
to say "47 tests, verified 2026-08-13" long after this file had re-measured, and
two disagreeing counts cannot detect a drop in either direction.

The count is not a target and should not be pinned here as one -- it is recorded
only so that a large DROP is visible. What is worth knowing is which files it
can and cannot reach:
`lib.rs` gates `entry`, `intrinsics` and `sys` to `#[cfg(target_arch =
"wasm32")]`, so **no host test can execute anything in `intrinsics.rs`** -- the
per-guest-call hooks, the remill intrinsics, the memory accessors. A green
`cargo test` says nothing about that file. Only the env-gated E2E suite covers
it, which is why a change there is not done until E2E has run. Found the hard way
on 2026-08-15: splitting `_ecv_func_epilogue` left a duplicate
`call_history.pop()` that all 81 tests were structurally incapable of seeing.

The two obsolete caveats, recorded so a stale memory of them does not get
re-applied:

- *"The host test build is broken -- `src/context.rs` refers to `crate::sys`
  while `lib.rs` gates `mod sys` to wasm32."* It builds. The host-side tests in
  `arena`, `vfs`, `boot` and `execmap` all run.
- *"The shipping-target check has a known non-green baseline of three
  `dangerous_implicit_autorefs` errors in `src/intrinsics.rs`."* There are none.

This mattered more than a stale note usually does, in both directions. While the
host build was believed broken, nothing in `runtime/` was unit-tested at all --
including the arena layout assertions, which were being written and edited
without ever executing. And a documented "known non-green baseline" invites
comparing against a failure, which is one short step from accepting a new one.
Re-check the claim before trusting it again; that is what turned it up.

## 3. Documentation style

Repo-authored docs only (`AGENTS.md`, `README.md`, `.agents/docs/**`):

```sh
rg -n '（|）|：' --glob '*.md' .                       # must find nothing
rg -n --pcre2 '[\x{3099}\x{309A}]' --glob '*.md' .   # decomposed kana: must find nothing
```

Half-width parentheses and colons; CJK in NFKC normal form. Decomposed kana is
visually identical to composed kana in every editor and terminal, so scanning is
the only way to catch it.

## 4. End to end (opt-in)

Not part of the default gate. Run it when a change touches the pipeline itself —
discovery, fusing, translation, linking, the builder image, or the elfconv patch
series.

```sh
RAPTORMARK_E2E=1 \
  RAPTORMARK_BUILDER=raptormark-builder:<tag> \
  RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache" \
  RAPTORMARK_NODE="$(ls -d ~/.local/share/mise/installs/node/*/bin/node | head -1)" \
  go test ./e2e/ -v -timeout 60m

RAPTORMARK_E2E=1 RAPTORMARK_E2E_SLOW=1 \
  RAPTORMARK_BUILDER=raptormark-builder:<tag> \
  RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache" \
  RAPTORMARK_NODE="$(ls -d ~/.local/share/mise/installs/node/*/bin/node | head -1)" \
  go test ./e2e/ -v -timeout 90m
```

⚠️ **Both paths above were wrong until 2026-08-21, and each failed silently
rather than loudly.** They are corrected here from a real run; do not "tidy" them
back to something that looks more canonical.

- `RAPTORMARK_OBJECT_CACHE` used to read `/var/cache/raptormark`, **which does
  not exist on this machine**. Following the command literally does not error --
  the cache is simply created empty, so the run goes cold and costs hours for a
  change that cannot invalidate a single object. The warm cache is
  `.agents-workspace/objcache` (6.1 GB as of 2026-08-21).
- `RAPTORMARK_NODE` was absent entirely, and node is not on the non-interactive
  `PATH` here. Ten tests gate on it and SKIP when it is missing --
  `TestGuestTimersSurviveAWallClockStep` plus the nine Node-host and re-entrant
  tests -- so the suite reports a clean pass having never run them. Node IS
  installed (mise, 26.5.1); it is just not active in this directory, and note
  that `mise which node` does NOT resolve it either -- it errors with "not
  currently active", which is why the command above globs the install path
  directly.

**`-timeout` is a ceiling, not an estimate.** Measured 2026-08-17 on a warm
object cache, `raptormark-builder:scmfix`: the fast form above is **1186 s
(19m46s), 81 pass / 4 skip / 0 fail** — a third of its own timeout. The four
skips are the three `RAPTORMARK_E2E_SLOW` fixtures plus the containerd test.
Recorded because the 60m figure reads as the cost and makes the suite look too
expensive to run routinely, which is exactly backwards: it is the only gate that
observes runtime BEHAVIOUR, and the host gates cannot see a regression in it.
(The first command above also used to be shown bare, contradicting the two
bullets directly below it.)

**Re-measured 2026-08-18, `raptormark-builder:embed`: 192 s, 90 pass / 4 skip /
0 fail.** The suite grew 81 -> 94 tests over the day; the drop in wall-clock
against the 08-17 reading is a warmer object cache, not fewer tests. Same four
skips. The number is here for spotting a large DROP in the pass count -- a suite
that suddenly reports 60 has stopped building something -- and for nothing else.

**Re-measured 2026-08-21, `raptormark-builder:sweep0821`: 247 s, 109 pass / 0
fail / 19 skip**, with both env vars above set.

⚠️ **"4 skips" is no longer the healthy baseline, and the SKIP count now needs
reading as carefully as the pass count.** 19 is normal on a machine without
Playwright: 12 browser (`RAPTORMARK_E2E_BROWSER=1`), 3 `RAPTORMARK_E2E_SLOW`, 1
containerd, 1 clock-bench (`RAPTORMARK_CLOCK_BENCH=1`, an instrument that does
not assert), 2 expensive fixtures. The browser suite arrived after the 08-18
reading and accounts for the jump.

The same run WITHOUT `RAPTORMARK_NODE` reports **99 pass / 0 fail / 29 skip** --
still "0 fail", ten tests never executed. That is the failure mode this section
exists to prevent, so check the skip count against the breakdown above before
believing a green run: a pass total can go UP while coverage goes down.

- Always pass an explicit `RAPTORMARK_BUILDER`. `raptormark-builder:latest` is
  stale relative to the patched builders, and a stale default fails deep inside
  elflift with an error that reads like a defect in the input.
- Always pass `RAPTORMARK_OBJECT_CACHE`. Without it every run re-translates
  from scratch; with it, a change confined to `runtime/` costs only the link.
- `RAPTORMARK_E2E_SLOW=1` adds the fused-guest lifts (~30 minutes each). Budget
  for it, and do not extrapolate from one closure to another — cost follows the
  largest single function, not total volume.

Runtime scheduling or socket changes should include the focused guards in
`e2e/timers_test.go` and `e2e/nonblock_test.go`. The former observes finite,
zero, and ready `epoll_wait`; the latter compares non-blocking socket and
connect behavior with a native Linux baseline.

Changes to guest process memory, AF_UNIX, or shared regular files should include
the relevant guards in `e2e/divergemem_test.go`, `e2e/crossprog_test.go`,
`e2e/uds_test.go`, and `e2e/sharedfile_test.go`. A fork-based guard must make
the parties' state differ; inherited agreement proves nothing about restoration
or sharing.

Memory-syscall changes should include `e2e/mmadvise_test.go`, whose native
baseline observes contents after `MADV_DONTNEED` and writes the whole extent
returned by `mremap`. Side-module changes should include
`e2e/sidemodule_test.go`, `e2e/embedder_test.go`, and
`e2e/imports_test.go`: together they check the link contract, run two modules
through reserve/place/relocate/register/start, and bound what a host must supply.

Lifter patches need a fixture that reaches the changed decoder or discovery
branch. `e2e/pacjumptable_test.go` uses assembly because C did not preserve the
literal `paciasp` entry; `e2e/tbltable_test.go` pins an encoding seen in the
real corpus and compares with a native oracle. A full E2E pass with zero affected
functions is a regression control, not direct validation of the patch.

### The decode differential (opt-in, no Docker)

`tools/decode-oracle` is a **separate Go module**. Section 1's command reaches
it by module path, so a normal gate covers it; you only need the module-local
form when working inside it, or to check that it still stands alone:

```sh
cd tools/decode-oracle && gofmt -l . && go build ./... && go vet ./... && go test ./...
```

That covers both front ends: `internal/decode` (the tables and the parser) and
`internal/mcp` (the MCP stdio server, tested at the PROTOCOL level over pipes --
framing, notification handling, and the tool-error vs protocol-error
distinction, none of which a test that calls Go functions can see).

It embeds QEMU's decodetree tables (LGPL-2.1-or-later) and is kept out of the
main module so that obligation never reaches a pipeline that links Apache-2.0
lifter code. ❌ Do not fold it back in.

Its correctness check is a differential against an independent disassembler over
real fused binaries, gated on an env var because the fixtures live under
gitignored `.agents-workspace/`. It needs neither Docker nor network, only
`objdump` and an aarch64 fixture.

```sh
cd tools/decode-oracle
RAPTORMARK_DECODE_CORPUS=$PWD/../../.agents-workspace/fixtures/postgres-glibc.fused \
  go test ./internal/decode/ -run TestCorpusAgreesWithObjdump -v
```

The path must be **absolute** — `go test` runs each package in its own source
directory. Run it against one glibc and one musl fixture after re-vendoring
`third_party/qemu-decode/`, since the two share almost no assumptions and musl
ships no SVE at all -- glibc's SVE ifunc variants were 100% of the gap before
`sve.decode` was vendored, and busybox-musl could not have shown it. The
measured baselines at QEMU v11.1.0 are in the test's own doc comment; the
assertion floor is 99.9%, deliberately below them, so it catches a pin that has
lost whole families rather than freezing a number that moves legitimately with
binutils.

## 5. Neutralize every new check

**Before trusting any check — a new test, a `grep`, a differential probe, a
built artifact — ask what a PASS would look like if the claim were false.** If
there is no answer, the check proves nothing yet and must not be cited as
evidence.

Then neutralize it: deliberately break the fix and confirm the test fails with
the diagnostic you intended.

- ❌ A COMPILE error is not a valid neutralization. It proves the test names a
  symbol, not that it observes a behavior.
- Every regression guard in this tree was checked this way, and it has now caught
  **three** guards whose assertions were mis-specified and would never have
  fired. Budget for it.
- Two of the three shared one shape, so it is worth naming: the test's COMMENT
  described the right mechanism while its ASSERTION ran through a different code
  path. The decode oracle's SRI cases covered only two of four lane widths, so
  substituting `rsub_64` for `rsub_32` failed nothing behavioural; its `ld1b`
  cases claimed to exercise `!function=msz_dtype` but single-register loads take
  `dtype` as a plain field, so substituting identity failed nothing behavioural
  either. **A comment is not a check**, and only breaking the mechanism tells
  them apart.
- When a helper is a table or a family, cover the cases that DIFFER. `msz=0` maps
  to `dtype=0` under both the correct lookup and identity; a test with only that
  case passes against a broken implementation.
- Rebuild the tool after changing the library. A stale binary produces a "the fix
  didn't work" conclusion that is purely an unrebuilt binary.

## 6. Before you report

- Did you re-verify any `.agents/docs/TODO.md` entry you acted on against the
  actual tree, rather than trusting its text?
- If the change is durable knowledge, is it in `.agents/docs/JOURNAL.md`
  (appended, never edited in place)?
- If the change alters what `README.md` claims — especially the Status section —
  does the claim now name the evidence that supports it?
- Report failures as failures, with the output. A skipped step is a skipped step.
