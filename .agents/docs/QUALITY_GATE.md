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

⚠️ And it is easy to drift back to the short form without noticing, because the
short form is green: an agent ran bare `./...` for a whole session on
2026-08-24 before catching it here. Re-running both patterns showed 13 packages
passing and nothing hidden -- but that was luck, not verification. The skipped
tests do not announce themselves; `go test` prints only what it ran.

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
# `net-wasmedge`, `net-browser` and `net-wasix` carry `extern` blocks at all.
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1 \
  --no-default-features --features net-loopback
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1 \
  --no-default-features --features net-browser
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1 \
  --no-default-features --features net-wasix
```

❗ **EVERY `net-*` FEATURE NEEDS A LINE HERE, and a missing one is invisible.**
`net-loopback` is a LABEL, not a `cfg` -- `runtime/src/net/mod.rs` selects
loopback by the ABSENCE of every other backend, from two separate `any(...)`
lists. A backend whose feature is not checked here can fail to compile while
every other command in this file stays green, and a backend added to one of
those two lists and not the other compiles loopback in ALONGSIDE itself.
`//runtime:profile_exclusion_test` catches the second; nothing but this catches
the first. `net-wasix` was added 2026-08-24 and the note above it named only two
backends at the time.

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

**Re-measured 2026-08-25: 304 tests, 0 failures, 7.08 s** (`cargo test
--manifest-path runtime/Cargo.toml`).

The readings are kept as a SERIES rather than folded into one total, because a
count is only useful for spotting a large DROP and it cannot do that from a
number nobody can attribute:

| reading | date | what accounts for the rise |
|---|---|---|
| 231 | 2026-08-22 | -- |
| 280 | 2026-08-24 | ⚠️ **not reconciled**; happened in sessions between |
| 288 | 2026-08-24 | +8, all dynamic side-module work (below) |
| 301 | 2026-08-25 | +13, the WASIX socket work -- 12 of them in `net::poll1` (6) and `net::wasix_addr` (6) |
| **304** | **2026-08-25** | +3, `net::loopback` ephemeral-port assignment (below) |

The +8 at 288, each pinning something no other gate can see:

| where | what it pins |
|---|---|
| `dlmap` | `hash_for` answers before a unit is registered, while `resolve` does not |
| `execmap` ×2 | an exec target registered AFTER the map still resolves; a deferred one is not a library unit |
| `context` ×5 | the pending token is stable and never a registry index; the host wake releases a parked guest AND enqueues it; a side-load waiter idles rather than deadlocking; a token maps back to its registry index; a duplicate late registration is refused |

The +13 at 301 is worth naming for the same reason: `net::poll1` and
`net::wasix_addr` are PURE and uncfg'd precisely so they can be host-tested at
all. Both socket backends previously hand-rolled the `poll_oneoff` buffers with
magic offsets and had **no host test whatever**, being wasm32-gated -- which is
where the little-endian/big-endian port trap and the `_padding` byte at offset 1
lived.

The +3 at 304 is one behaviour and two controls, which is the ratio this file
argues for: `binding_port_zero_assigns_a_real_one` is the subject;
`two_ephemeral_binds_do_not_collide` refuses a fix that returns any non-zero
number; `an_explicit_port_is_left_alone` refuses one that assigns
unconditionally. Neutralizing the fix (`addr.port = 0`) fails the first two with
their intended messages and leaves the third green -- a control that stays green
under the neutralization is doing its job, not sitting idle.

⚠️ **304 tests but 308 `#[test]` in a naive grep, and there is no discrepancy.**
`grep -rc "#\[test\]" runtime/src` counts four occurrences inside COMMENTS --
comments that exist to explain why `mod sys` cannot host a test, since it is
`#[cfg(target_arch = "wasm32")]`. Counting only bare attribute lines gives
exactly 304:

```sh
grep -rn "#\[test\]" runtime/src --include=*.rs | grep -cE ':\s*#\[test\]\s*$'
```

Worth stating because "308 attributes, 304 run" looks exactly like four silently
skipped tests, and chasing that is the kind of hour this file exists to save.
The four-comment delta has held across every reading since it was first noticed.

## 3. The Bazel gate

Run it for any change to a `BUILD.bazel`, a `.bzl`, or anything that reaches
the builder image — the LLVM companion tools, the C shims, the ecvisor
profiles.

```sh
raptormark bazel --image raptormark-elfconv-base-patched:<tag> test //...
```

Seconds, warm. Bazel runs INSIDE the base image because the LLVM tools must
link against the LLVM that built `elflift`; Bazel itself is mounted in from the
host, so put `bazelisk`/`bazel` on `PATH` or set `RAPTORMARK_BAZEL`.

The same targets build with **no Docker at all**:

```sh
bazel build --config=hermetic //builder:stage
```

which downloads LLVM 16.0.6 and wasi-sdk 24.0 by sha256. What that mode is and
is not equivalent to has been measured rather than assumed — run
`builder/hermetic_differential.sh` to re-measure it. Summary as of 2026-08-23:
6 of 10 staged artifacts byte-identical, the 4 LLVM tools not (different LLVM
linkage model), their emitted bitcode differing by the 36-byte version string,
and **the emitted objects identical**. The partition cache misses across a
switch; the object cache stays correct. Hermetic tools built on a newer host
cannot run inside the image (`GLIBC_2.38 not found`).

Three of its tests are what make moving a build recipe out of the Dockerfile
safe, and they are the reason this section exists:

| test | what it asserts |
|---|---|
| `//builder:tools_equivalence_test` | the four LLVM tools are byte-identical to the `RUN` line that used to build them |
| `//runtime:cshim_equivalence_test` | both C shims, the same way |
| `//runtime:profile_exclusion_test` | each of the FOUR net archives carries exactly one network backend, and carries its own |
| `//runtime:loader_exclusion_test` | the SHIPPING archive carries no host-aided loader, and the two loader archives (`hosted`, `wasix`) carry only their own |
| `//internal/pipeline:pipeline_test` | the end-to-end driver's pure logic (see the note below) |

⚠️ **Both exclusion rows are one revision behind twice over, so re-read them
rather than trusting a remembered scope.** `loader_exclusion_test` covered only
`hosted` until 2026-08-24 and now covers four archives; `profile_exclusion_test`
covered three net backends until `net-wasix` and now covers four.

❗ **AND `profile_exclusion_test` GAINED A POSITIVE CONTROL, without which it
was weaker than it read.** Every assertion in it is an ABSENCE, and
`ecvisor::net::loopback` leaves NO symbols in any archive (it is pure Rust and
inlines away at `-Clto=fat`), so "loopback is not in the wasix archive" was
satisfied by an archive that had compiled loopback in. Measured 2026-08-24 by
pointing `//runtime/wasix` at `net-loopback`: all four `foreign-backends=0` lines
still printed `ok`. Each backend that calls an import must now be FOUND in its
own archive -- 17 wasmedge, 16 browser, 19 wasix symbols -- which is what makes
the exclusions mean anything.

The first two work by transcribing the deleted `RUN` line and rebuilding from
it, so they fail if the compile flags in `//bazel:llvm_tool.bzl` or
`//bazel:wasi_object.bzl` ever drift. Verified 2026-08-23: 4/4 and 2/2
identical.

⚠️ **It does not run MOST of the Go tests.** `//internal/builder` and
`//internal/rootfs` are tagged `manual`: they read the repo tree by relative
path and shell out to `go`, neither of which exists in Bazel's sandbox. §1 is
still the authority for Go, and it passes. Two gates where one of them lies is
worse than one gate.

`//internal/pipeline:pipeline_test` is the exception and is NOT tagged
`manual`: its tests are pure functions of their arguments -- the refusal to
default `--builder`, the two path derivations, and the two runtimes' reversed
`--dir` spellings -- so they read nothing outside the sandbox and shell out to
nothing.

❗ **AND AFTER ADDING A FILE.** `srcs` are explicit everywhere, so a test file
that is not listed is not compiled -- not skipped, not failed, not reported.
Measured 2026-08-24: SIX files under `e2e/` were missing, including
`pgdlopen_test.go`, and `bazel test //...` said 13/13 the entire time. Listing
them produced an immediate `FAILED TO BUILD`. One line finds it:

```sh
for f in $(ls e2e/*_test.go | xargs -n1 basename); do
  grep -q "\"$f\"" e2e/BUILD.bazel || echo "MISSING FROM BUILD: $f"
done
```

❗ **RUN THIS GATE AFTER ADDING A GO PACKAGE, even one Bazel does not test.**
Measured 2026-08-24: adding `internal/pipeline` and importing it from
`cmd/raptormark` left `gofmt`, `go build`, `go vet` and `go test ./...` all
green while `bazel test //...` went from 12 passing to "3 tests pass and 9 were
SKIPPED". That reads like a caching artifact and is not: four `GoCompilePkg`
targets failed, one of them `//cmd/raptormark:builder_tools_linux_arm64`, which
`//builder:stage` depends on -- so the BUILDER IMAGE could not have been built.
A `BUILD.bazel` for the new package and a `deps` entry fixed it. Note also that
`srcs` are listed explicitly, so a new FILE in an existing package needs adding
too.

⚠️ **`//runtime:profile_exclusion_test` reads archives, not linked modules.**
It proves the wasmedge backend is absent from the loopback archive — a
precondition for "a loopback module imports no sockets", not that claim. Only
`wasm-ld` decides what becomes an import, so the claim itself belongs to §5.

## 4. Documentation style

Repo-authored docs only (`AGENTS.md`, `README.md`, `.agents/docs/**`):

```sh
rg -n '（|）|：' --glob '*.md' . | grep -v '^\./AGENTS\.md:'   # must find nothing
rg -n --pcre2 '[\x{3099}\x{309A}]' --glob '*.md' .            # decomposed kana: must find nothing
```

⚠️ **The first command needs that filter and used to be written without it, so
as documented it could never pass.** `AGENTS.md` states the rule by QUOTING the
forbidden characters, so it matches itself — two hits, permanently. An agent
running the unfiltered form gets a failure it cannot fix except by deleting the
rule's own examples. If a third hit ever appears in `AGENTS.md`, read it: only
the lines that DEFINE the rule are exempt.

Half-width parentheses and colons; CJK in NFKC normal form. Decomposed kana is
visually identical to composed kana in every editor and terminal, so scanning is
the only way to catch it.

### After a memory pass (`good-sleep`, `deep-sleep`, `reconcile-journal-ltm`)

Structural checks only — they say the memory documents are internally
consistent, never that their CLAIMS are current:

```sh
cd .agents/docs
for f in LTM/*.md; do b=$(basename $f); [ "$b" = INDEX.md ] && continue;
  grep -q "($b)" LTM/INDEX.md || echo "NOT INDEXED: $b"; done
grep -o '](\([^)]*\.md\))' LTM/INDEX.md | sed 's/](//;s/)//' \
  | while read p; do [ -f "LTM/$p" ] || echo "BROKEN LINK: $p"; done
grep -oh '\[\[[a-z0-9-]*\]\]' LTM/*.md | tr -d '[]' | sort -u \
  | while read n; do [ -f "LTM/$n.md" ] || echo "DANGLING: $n"; done
grep -c '^## ' JOURNAL.md      # exactly 1 after reconcile-journal-ltm
```

❗ **The check that actually matters is not scriptable in one line**: every
claim naming a file, function, symbol, flag or env var must be verified against
the tree. Run over LTM on 2026-08-25, that found eight stale claims where the
structural checks above found none — a marker grep passes on a stale sentence
and a current one alike. The mechanical half of it is worth keeping:

```sh
# every repo path, env var and Rust symbol LTM names, checked for existence
grep -oh '`[^`]*`' .agents/docs/LTM/*.md | tr -d '`' \
  | grep -E '^(internal|runtime|e2e|cmd|builder|bazel|patches|web|tools)/' \
  | sort -u | while read p; do [ -e "$p" ] || echo "MISSING PATH: $p"; done
```

Expect false positives (globs, `package.Symbol` forms, guest and wasmer
symbols) and read them rather than suppressing them; the two real defects that
run surfaced were both hiding among them.

## 5. End to end (opt-in)

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

⚠️ **`RAPTORMARK_E2E_WASMER=1` is a SEPARATE gate, and neither line above
includes it.** The six `TestWasix*` tests skip without it and the run still says
"0 fail" -- the skip-count trap below, one more time. It needs a wasmer
container, which is deliberately neither on the host nor in the builder image
(`.agents-workspace/wasmer/Dockerfile` builds `raptormark-wasmer:7.3.0`;
`RAPTORMARK_WASMER_IMAGE` overrides the tag).

```sh
RAPTORMARK_E2E=1 RAPTORMARK_E2E_WASMER=1 \
  RAPTORMARK_BUILDER=raptormark-builder:<tag> \
  RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache" \
  go test ./e2e/ -run TestWasix -v -timeout 40m   # ~24 s warm, 6 tests
```

Run it for any change to `runtime/src/net/`. The archive-level exclusion tests
cannot reach these: three of the four WASIX ABI traps produce a module with a
PERFECT import section and wrong behaviour, and one of them is a hang.

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

**Re-measured 2026-08-25, `raptormark-builder:lbport`: 453 s, 130 pass / 0 fail
/ 20 skip**, with `RAPTORMARK_NODE` **and** `RAPTORMARK_E2E_WASMER=1` set --
the first reading in this series to include the wasmer suite, which is what
accounts for most of 109 -> 130.

The 20 skips reconcile against the breakdown above with ONE addition: 12
browser, 3 `RAPTORMARK_E2E_SLOW`, 1 containerd, 1 clock-bench, 2 fixtures, and
**1 that is neither an env gate nor a missing tool** -- a fixture recorded as
`not present (it is unreproducible; see JOURNAL.md)`. Worth naming because it is
the only skip in the list that no environment variable can turn back on, so it
will not "come back" on a better-provisioned machine.

**Re-measured 2026-08-25 after the OpenSSL fixture rewrite,
`raptormark-builder:lbport`: 389 s, 131 pass / 0 fail / 19 skip**, same env.

❗ **The delta against the 453 s reading above is one test in EACH direction, and
both are the same test.** `TestOpenSSLFixtureDiscoverAndFuse` went SKIP -> PASS,
so pass +1 and skip -1. The skip that disappeared is the one called out above as
"neither an env gate nor a missing tool" -- the lost pre-wipe fixture, the only
skip in that list no environment variable could turn back on. The fixture is now
BUILT by the suite, so it cannot go quiet that way again.

That is what an attributable delta looks like, and it is the reason to record
these as a series: a pass count rising by one proves nothing on its own, but a
pass +1 with a skip -1 naming the same test is coverage genuinely recovered.

**Re-measured 2026-08-25 at end of session, `raptormark-builder:lbport`: 423 s,
134 pass / 0 fail / 19 skip**, same env.

The two deltas since the 453 s reading are each attributable to a NAMED test,
which is the only thing that makes a pass count worth recording:

| reading | pass / skip | what moved |
|---|---|---|
| 130 / 20 | — | baseline |
| 131 / 19 | +1 / −1 | `TestOpenSSLFixtureDiscoverAndFuse` SKIP -> PASS, once its fixture became buildable |
| **134 / 19** | +3 | `TestNetForkServerUnderEcvisor`, `TestNetForkServerUnderLoopback`, `TestRunStatesAgreeBetweenRuntimeAndEmbedder` |

⚠️ `TestRunStatesAgreeBetweenRuntimeAndEmbedder` is in `e2e/` but is **NOT
gated** on `RAPTORMARK_E2E` -- it reads two source files and needs no Docker, so
it also runs under a bare `go test ./...` and under `bazel test`. It is counted
here because it lives in the package, not because the suite is what exercises
it.

**Re-measured 2026-08-25 after the net-guest rewrite,
`raptormark-builder:lbport`: 407 s, 137 pass / 0 fail / 19 skip**, same env.

134 -> 137 is `TestNetServerUnderEcvisor`, `TestNetClientUnderEcvisor` and
`TestNetForkServerMatchesItsRecordedSyscallSequence`.

❗ **An intermediate run of this same suite reported 135 / 1 fail**, and it is the
reason to run the whole thing rather than the tests you changed. The rewrite of
`netServerSrc`/`netClientSrc` broke `TestWasixProfileServesTheSocketABI`, which
compiles the SAME guest sources and whose fixed port is load-bearing for a
different reason -- `bind(0)` has no port to encode, so the coordinating form
would have left its address-codec assertion green while testing nothing. Every
focused run of the tests I had touched was green throughout.

**Re-measured 2026-08-25 after the `.ecv.funcs` removal,
`raptormark-builder:lbport`: 2600 s, 137 pass / 0 fail / 19 skip**, same env.

**Re-measured 2026-08-26 after the symbol-versioning fix in `globalSymbols`,
`raptormark-builder:rebal`: 396 s, 137 pass / 0 fail / 19 skip**, same env.

❗ **IDENTICAL to the 407 s reading in both counts.** The fuse change re-binds 18
symbols in every glibc image, and nothing moved -- no test gained, none lost,
none went quiet. That is the result wanted from a change of this shape.

⚠️ **The FIRST attempt at this run is the one worth recording, because it was
green and worth less.** It reported **121 pass / 0 fail / 35 skip in 2460 s**,
with neither `RAPTORMARK_NODE` nor `RAPTORMARK_E2E_WASMER=1` set -- 16 tests
never executed, and the node and wasmer suites among them. It would have read as
a pass. This section's own rule caught it: check the skip count against the
breakdown before believing a green run. 19 is the healthy number on this
machine; 35 means two suites are missing.

The 2460 s -> 396 s difference between the two attempts is the object cache: the
first re-translated every glibc fixture after the fuse change, the second found
them warm. Same shape as the `.ecv.funcs` reading above.

⚠️ **What this run does NOT establish**, stated because the green is easy to
over-read: nothing in the suite calls `exp`, `fmod`, `glob` or `totalorder*`
through a fused glibc, and the two heaviest glibc closures
(`TestOpenSSLFixtureEndToEnd`, `TestSharedNamesReuseAcrossAClosure`) are
`RAPTORMARK_E2E_SLOW`-gated and skipped. The suite shows the fix BREAKS nothing.
It does not show the 18 re-bound symbols resolve to the right bodies at run
time; that claim rests on `internal/fuse/symver_test.go` and on
`.agents-workspace/drivers/symver` over the real closure.

❗ **Identical counts to the 407 s reading, at 6.4x the wall clock.** That is the
object cache going cold for the FUSED path only, and it is worth recording
because it is the shape of a legitimate slowdown:

| | |
|---|---|
| pipeline translate steps (fused images) | 18, all cold |
| `liftOne` cache HITS | **85** |
| `liftOne` real translations | 4 |

**85 of 89 lifts still hit cache.** Removing `.ecv.funcs` changed the fused ELF
bytes and nothing else, so only images that go through `fuse` re-translated;
plain-ELF lifts are keyed on ELF content that did not move. The cost lands
entirely in the fusing tests -- `TestBuildHostedProfileServesBothTriggers`
787 s, `TestBuildCommandDrivesTheWholePipeline` 417 s.

⚠️ A 6x slowdown with an unchanged pass count is the signature of a cache
invalidation, not a regression. Check the hit/miss split before reading one as
the other; the next warm run should return to ~400 s.

⚠️ **Do NOT read 453 s against the 2026-08-21 reading's 247 s as a slowdown, or
against the 08-17 reading's 1186 s as a speedup.** The env differs in both
directions -- this run added the wasmer suite and its container startups, and
still skipped the browser suite. Wall-clock across these readings is only
comparable when the gate set is identical, which no two of them are.

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

⚠️ **`compileGuest` builds with `gcc -static`, and that is an unstated
assumption of every test that uses it.** A static image carries no `.ecv.tls`,
so every execve test in this suite -- `crossprog`, `execthread`, `execload` --
exercised only programs without static TLS. That hid an ENOEXEC for every
DYNAMIC program for as long as the code existed (2026-08-24 JOURNAL). When a
test's subject could plausibly differ between a static and a dynamic guest, say
which one it is testing, and consider `buildPgDlopenFixture`-style Docker guests
instead.

### Measured 2026-08-24

**109 pass / 30 skip / 0 fail, 286 s** on `raptormark-builder:dupguard` with a
warm object cache. ⚠️ The wall time is not comparable to the "1186 s" CLAUDE.md
records for 2026-08-17: that run re-translated more. `-timeout` is a CEILING,
not an estimate, and reading it as the cost is what makes this suite feel too
expensive to run before declaring a change done.

**Re-measured 2026-08-24 (later the same day), `raptormark-builder:wasixnet`
with `RAPTORMARK_E2E_WASMER=1`: 117 pass / 30 skip / 0 fail, 353 s.** The +8 is
the six `TestWasix*` tests plus two subtests; the skip count is UNCHANGED at 30,
which is the half of this figure that matters — a new test that silently skipped
would show here as +0 pass and +1 skip, and "0 fail" would say nothing about it.

⚠️ The same suite reported one FAILURE when run concurrently with a second
`go test ./e2e/`. See §8: that is a fixed-port collision, not a regression, and
it is worth ruling out before debugging a red run.

Three of those tests are new and each covers a path nothing else reaches:

| test | what only it proves |
|---|---|
| `TestHostedLoaderServesADlopenMidRun` | a guest's `dlopen` parks and a host serves the load, mid-run |
| `TestHostedLoaderServesAnExecveMidRun` | the same for `execve` -- the seam's second trigger |
| `TestBuildCommandDrivesTheWholePipeline` | `raptormark build` end to end, and the ONLY production caller of discovery + fusing |

⚠️ **`TestBuildCommandDrivesTheWholePipeline` is the only test that exercises
the pipeline as a PRODUCT.** Every other test here assembles its own closure,
fuse, registry and maps -- deliberate, since each isolates one stage, but it
means a defect in how the stages are strung together is invisible to all of
them. Three were found the day it was written, two by its own unit tests before
it ran and one only by running it.

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

## 6. Neutralize every new check

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

## 7. Before you report

- Did you re-verify any `.agents/docs/TODO.md` entry you acted on against the
  actual tree, rather than trusting its text?
- If the change is durable knowledge, is it in `.agents/docs/JOURNAL.md`
  (appended, never edited in place)?
- If the change alters what `README.md` claims — especially the Status section —
  does the claim now name the evidence that supports it?
- Report failures as failures, with the output. A skipped step is a skipped step.

## 8. ❗ Run ONE `go test ./e2e/` at a time

Two concurrent invocations collide, and the collision does not look like one.

Several guests bind FIXED ports -- `nonblockGuestSrc` takes 39117, the socket
guests take 47825/47826 -- and the wasmer tests run with `docker --network
host`, so a container's guest shares the host's port space with whatever else
is running. A second suite in flight makes the first guest's `bind` return
EADDRINUSE, and because that guest keeps going, the output is a wall of
unrelated-looking errnos with the one that matters at the top:

```
FAIL bind (errno=98)                     <- the cause
FAIL listen (errno=22)
FAIL accept should be EAGAIN (errno=5)
FAIL recv should be EAGAIN (errno=88)    <- reads like a broken backend
```

Measured 2026-08-24: a full suite run concurrently with a `-run TestWasix` run
reported `TestWasixProfileHonoursNonblockingSemantics` as a FAILURE, and the
same test passed alone both before and after. **A red E2E run is worth checking
for a second `e2e.test` process before it is worth debugging** --
`ps aux | grep e2e.test`.

⚠️ This is not new and is not specific to the wasix profile: the same fixed
ports are used by `nodehost_test.go` and `net_test.go`. Go runs a package's
tests sequentially, so ONE invocation is always safe; it is only overlapping
invocations that break.
