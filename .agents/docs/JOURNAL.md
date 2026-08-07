## LTM Consolidation Record

The journal has been audited entry by entry against `.agents/docs/LTM/` and
`.agents/docs/TODO.md`. Every removed substantive entry has its durable
design, implementation, testing, pitfall, or follow-up information represented
in those destinations. Transient chronology, superseded measurements, and
investigative output without durable guidance were not retained.

### Journal sections to source LTM documents

| Journal section or section group | LTM document |
|----------------------------------|--------------|
| Recovery evidence, reconstructed components, builder provenance, image tags, patch-series health, and Docker preservation | `recovery-and-builder-provenance.md` |
| Image discovery, executable closure, symlinks, rootfs reconstruction, RFS modes, canonical paths, and plugin discovery | `image-discovery-and-rootfs.md` |
| Fuser reconstruction, relocation families, RELR, TLS/TLSDESC, GNU IFUNC, and resolver evaluation | `fusing-relocations-and-ifunc.md` |
| Function extents, `.eh_frame`, init/fini, computed code pointers, trace boundaries, and `.ecv.funcs` limits | `function-boundary-recovery.md` |
| Runtime custom-section consumers, reconstructed producers, dynamic symbols, TLS, musl TP, and producer/consumer gaps | `runtime-metadata-producers.md` |
| Deterministic lifting, preparation, namespacing, partitioning, object identity, manifests, library caching, and flat/side-module links | `translation-linking-and-object-cache.md` |
| Futexes, TLS/TSD, pthread identity, shared-VM threads, fork replay, signals, arena switching, shared windows, and bounded snapshots | `ecvisor-process-and-thread-model.md` |
| PostgreSQL bring-up, planner/data-path rungs, bounded concurrency, guest AF_UNIX, shared files, and multi-client results | `postgres-and-guest-concurrency.md` |
| RFS/VFS, syscalls, timed waits, socket semantics, errno mapping, guest AF_UNIX, host-neutral networking, and nginx serving | `ecvisor-vfs-syscalls-and-networking.md` |
| Proposal-free suspension, Wasm 2.0, OCI/runwasi behavior, portable profiles, multi-module constraints, and vDSO limits | `wasm-runtime-and-oci-compatibility.md` |
| Lifter hot spots, BTI indexing, merged-pass costs, closure timings, cache measurements, clock experiments, and measurement method | `performance-investigation.md` |
| Host/E2E gates, fixture controls, neutralization, false controls, artifact freshness, browser witnesses, and expressible checks | `testing-and-regression-method.md` |
| Document roles, memory workflows, language gates, web tooling, Vitest boundaries, and quality-gate drift | `agent-harness-and-quality-gate.md` |
| Hot-path tracing cost, opt-in design, patch 0060, and real-interpreter pricing | `hot-path-cost-and-opt-in-design.md` |
| AArch64 decoder patches 0050-0065, BTI/PAC handling, undecoded inventories, and native-oracle guards | `aarch64-lifter-and-coverage.md` |
| Python, Redis, cryptography, plugin loading, musl threading, and interpreter bring-up | `python-redis-and-cryptography-bringup.md` |
| Ruby PAC jump-table recovery, patch 0062, and optional YJIT scope | `ruby-jit-and-jump-table-bringup.md` |
| Node and browser embedding, re-entrant execution, DNS, relay, service-worker inbound, HTTP framing, and nginx in a tab | `web-embedder-and-browser-networking.md` |

Open and unresolved work extracted from these entries is maintained in
`.agents/docs/TODO.md`.

### Synthesis documents

| Synthesis document | Source LTM documents |
|--------------------|----------------------|
| `build-pipeline-synthesis.md` | `recovery-and-builder-provenance.md`, `image-discovery-and-rootfs.md`, `fusing-relocations-and-ifunc.md`, `function-boundary-recovery.md`, `runtime-metadata-producers.md`, `translation-linking-and-object-cache.md` |
| `ecvisor-runtime-synthesis.md` | `ecvisor-process-and-thread-model.md`, `ecvisor-vfs-syscalls-and-networking.md`, `wasm-runtime-and-oci-compatibility.md` |
| `engineering-practice-synthesis.md` | `performance-investigation.md`, `testing-and-regression-method.md`, `agent-harness-and-quality-gate.md`, `hot-path-cost-and-opt-in-design.md` |
| `target-enablement-synthesis.md` | `aarch64-lifter-and-coverage.md`, `python-redis-and-cryptography-bringup.md`, `ruby-jit-and-jump-table-bringup.md` |

### Intentionally standalone source documents

| Document | Reason |
|----------|--------|
| `postgres-and-guest-concurrency.md` | Workload-specific integration knowledge spans build and runtime boundaries and is most useful end to end. |
| `web-embedder-and-browser-networking.md` | Host, runtime, transport, browser, and workload constraints form one end-to-end reference not yet represented by a synthesis document. |

See `.agents/docs/LTM/INDEX.md` for the complete long-term memory index.

# 2026-08-21 -- Unit tests for the four uncovered modules

`load.ts`, `netv1.ts`, `files.ts` and `sockets.ts` were the four modules with no
unit coverage, filed as open in the previous close. **59 -> 97 tests**, all in
~290 ms, all neutralized.

## What each one is actually guarding

⚠️ **`load.ts` -- the cache whose failure mode is documented as SILENT.** The
original bug (a `WebAssembly.Module` into IndexedDB, which Chromium refuses)
reported `cached: true` while re-downloading every time, and was caught only
because a test PRINTED hit-or-miss rather than asserting the page still worked.
So every test here asserts on CACHE TRAFFIC, not on the module coming back: a
load that ignored the cache entirely passes any test that only checks the result.
The decisive one is `assert.equal(fetchMock.mock.calls.length, 1)` after a hit --
`cached: true` is a claim about the network, so the network is what gets counted.
Also covered, because all three are ordinary rather than exotic: no Cache API at
all (insecure origin), `open` throwing, and `put` rejecting on quota -- a 120 MB
artifact on a phone. Any of them propagating takes the page down for a reason
that has nothing to do with the guest.

⚠️ **`netv1.ts` -- the reverse lookup that every browser transport depends on.**
`connect(2)` carries an address and nothing else; the name was consumed by the
guest's resolver and thrown away. Neutralized by dropping the lookup: the backend
receives `240.0.0.1` instead of `upstream.test`, which is unroutable by design, so
the failure is a dial into nothing. Also pinned: a synthetic address the pool
never minted is NOT rewritten (reversing it would be inventing a name), and an
errno comes back with the out-parameters untouched -- writing a handle on failure
is invisible until a stale value is read as a live socket.

⚠️ **`files.ts` -- basename matching, which is deliberately not a wildcard.**
`/rootfs.img`, `rootfs.img` and `./rootfs.img` are one request, because Rust's std
strips the preopen prefix before the call. The half that matters is that an
UNPUBLISHED name still gets ENOENT: "return the one file we have" satisfies every
positive assertion and hands the sidecar to any stray open. Also: two descriptors
on one file must have INDEPENDENT positions -- a per-file cursor reads correctly
for the single-open case every fixture exercises and silently returns the wrong
half the moment anything opens it twice.

⚠️ **`sockets.ts` -- one `fd_read` serving two worlds.** A guest reads its sidecar
and its sockets through the same import, separated only by handle range; routing
either way wrongly looks like the other subsystem failing. And a short read must
be reported as SHORT: `recv` returning less than the iovecs hold is the normal
case on a stream, and reporting the requested length has the guest read
uninitialised memory as data.

## Neutralization

One core claim per file, each firing on its own assertion:

| break | diagnostic |
| --- | --- |
| cache never consults its store | `a hit must not go to the network` |
| descriptors share one position | `the second descriptor starts at zero` |
| reverse lookup dropped | `expected 'upstream.test'` |
| `nread` = requested, not received | `seven bytes arrived, not the nine the buffers could hold` |

## Findings

* **A silent failure needs a test that watches the MECHANISM, not the outcome.**
  Every observable of the original cache bug was correct -- the page loaded, the
  module ran, the flag said cached. Only the fetch count disagreed. Where the
  failure mode is "works, but pointlessly", the assertion has to be on the work
  that was supposed to be avoided.
* **`new Request('/m.wasm')` throws outside a browser** -- no document base to
  resolve against. That is the test environment, not a defect in `load.ts`, which
  is handed `./public/nginx.wasm` by a page that HAS a base. Recorded in the
  fixture so the next reader does not "fix" the module.
* **The neutralization workflow trips the staleness guard every time.** Copy,
  break, restore: the restore moves the mtime, the bundle looks stale, the suite
  refuses. Fifth and sixth firing today. It is a true positive with identical
  content and the fix is a 4 ms rebuild, so it stays -- but the friction is real
  and now written down rather than rediscovered.

## Gates

Rust untouched (170 host tests). Go: gofmt/build/vet/test green.
Web: `typecheck`, `lint` (warnings denied), `format:check` over 48 files,
**97 vitest tests in 14 files, ~290 ms**.
**E2E on `:listenfix`: 127 defined, 127 run, 120 pass / 7 skip / 0 fail, 268 s.**

## 2026-08-21 -- `mmap` length validation: EINVAL for zero, ENOMEM for an overflowing page-align

Closes the "Correctness" TODO item of 2026-08-18. Both divergences were SILENT
successes, which is why the `fatal!` audit passed over them.

* **`mmap(NULL, 0, ...)` returned an ADDRESS.** `(0 + GUEST_PAGE_MASK) & !MASK`
  is 0, `mmap_reserve(0, ...)` succeeds, and the guest is handed the current bump
  as a mapping of nothing -- the next reservation then hands out the same
  address. `do_mmap` opens with `if (!len) return -EINVAL`.
* **CORRECTION to the TODO's own description of the overflow.** It read "a length
  so large that page-aligning it overflows"; the estimate in the item implied a
  colossal request becoming a *small* one. It does not. A wrapped
  `len + 0xffff` is always below `0x10000`, and `& !0xffff` clears it, so EVERY
  overflowing length rounds to exactly **0**. The neutralization printed
  `left: Ok(0)` for `u64::MAX`, which is what showed this. Consequence: the two
  bugs converge on the same zero-byte mapping, and the zero-length guard does
  NOT subsume the overflow one, because the second zero appears *after* the
  rounding. They are independent checks and the tests assert them separately.
* **Where the fix lives is forced by a cfg, not by taste.** `sys` is
  `#[cfg(target_arch = "wasm32")]` (lib.rs), so nothing in `sys.rs` is reachable
  from `cargo test` -- a unit test written there would never run. The rules went
  into `arena::mmap_round_len` with `GUEST_PAGE_MASK` moved beside them, which is
  the seam `arena::madvise_zeroes` already established for exactly this reason.
  `MmapLenError` is a variant rather than a raw errno so the errno numbers stay
  in `sys` where every other one is.
* **Ordering.** The check is the first thing in the `NR_MMAP` arm, ahead of the
  MAP_FIXED branch, because Linux checks `!len` ahead of everything -- a fixed
  mapping of zero bytes is refused too. It also subsumes the MAP_FIXED path's
  own `(len + 0xfff) & !0xfff`: if `len + 0xffff` does not overflow then
  `len + 0xfff` cannot. That inner binding was renamed `fixed_len`, because a
  shadow of `map_len` rounding to a *different* granule (4 KiB vs 64 KiB, and
  deliberately so -- rounding a fixed mapping up would zero up to 60 KiB of the
  guest's own data) is a trap.
* **The native expectation was measured, not assumed.** This host is aarch64 on
  Linux 6.17, so the two raw syscalls were run directly: `zerolen` EINVAL(22),
  `hugelen` (`SIZE_MAX`) ENOMEM(12). That is the same architecture the e2e guest
  targets, which is worth more than the usual "the kernel source says so".
* **Neutralization** (all three, by breaking the code, never by a compile error):
  removing the zero guard -> `left: Ok(0) right: Err(Zero)`; replacing
  `checked_add` with the original unchecked `wrapping_add` ->
  `left: Ok(0) right: Err(Overflow)`; rounding down instead of up ->
  `left: Ok(0) right: Ok(65536)`.
* **`SIZE_MAX` specifically, and a raw syscall.** Only a length in the top 4 KiB
  overflows for *both* a 64 KiB align (ecvisor) and a 4 KiB one (Linux); a merely
  enormous length is ENOMEM natively too, but by address-space exhaustion, which
  is a different claim. And musl's `mmap` rejects `len >= PTRDIFF_MAX` in libc
  without issuing the syscall, so the library-level version of this case would
  test musl -- the same trap `badoff` documented for glibc's offset check.

## Gates

Rust: `cargo fmt --check` clean, **175 host tests pass** (170 baseline + 3 new
here + 2 from a concurrent session's `context.rs` work), `cargo check
--target wasm32-wasip1` clean. Go: `gofmt -l` silent, `build`, `vet`,
`go test ./e2e/` (skips without `RAPTORMARK_E2E`) green.
**The two new e2e cases are UNRUN** -- no Docker/builder in this session.

# 2026-08-21 -- Timing a guest benchmark from the HOST (`--stamp`)

`TestClockBenchIsolatesTheHostClockRead` timed both of its loops with
`clock_gettime` -- the clock it exists to measure. That is safe against a clock
running at the wrong RATE (both ends of the bracket scale, the ratio survives)
and useless against a clock that advances per READ: two reads bracketing 200 000
`getpid` calls land in the same tick and the control loop reports **0 ns/call**.
The TODO entry that named this also named the fix: `bin/run.ts` already knows how
to time something from outside, and that is what `HOST-AFTER-STEP-MS` is.

## The mechanism

`web/src/stamp.ts` + `--stamp PREFIX` in `web/bin/run.ts`. The guest prints
`BENCH-MARK <NAME>` on its own line and flushes; `fd_write` is a **synchronous**
import, so the host's `onOutput` runs at the instant the guest reached that
line, inside the same slice, before the guest gets control back. The host reads
`performance.now()` there and emits `HOST-STAMP-<NAME>-US: <n>` on stderr.

* **Microseconds, not milliseconds.** The consumer divides by an iteration
  count: at ms resolution a 20 ms loop of 200 000 calls quantises to 5 ns/call.
* **Sample the clock before any work** -- splitting, matching and emitting all
  happen after the guest got there and belong to neither interval.
* **A line completed in this chunk is stamped by the chunk that finished it.**
  ecvisor writes when it has bytes, not on line boundaries.
* **Stamping runs even under `--quiet`, and before the echo**, so the cost of
  writing to a pipe is not folded into the interval.
* **The in-guest numbers were KEPT.** They are printed beside the host ones as a
  cross-check, because the disagreement between the two is the diagnosis: a
  control loop reading 0 ns/call from inside and 134 ns/call from outside names
  the defect exactly. `hostNsPerCall` refuses a zero span instead of dividing it.
* **Still an instrument.** No threshold was added, and none should be: a
  machine-dependent number with a pass/fail line is worse than an instrument.

## The differential, and what it does NOT cover

WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A host-timed number that
was really the guest clock in disguise would collapse to zero alongside it. So a
throwaway `wasm32-wasip1` probe (`.agents-workspace/tmp/stampprobe`, hand-written
WASI calls, not a lifted artifact) times 200 000 host round trips with a CACHED
clock -- one read, never advanced -- and prints the markers. One run, both
numbers:

```
HOST-STAMP-GETPID-BEGIN-US: 68567
BENCH-MARK GETPID-BEGIN
HOST-STAMP-GETPID-END-US: 95503
BENCH-MARK GETPID-END
BENCH-GETPID-NS 0
HOST-EXIT: 0
```

Guest: 0 ns/call. Host: 26 936 us / 200 000 = **134 ns/call**. That capture is
pinned verbatim by `TestHostStampLinesParseAsClockValues` (no Docker, no node),
which runs `hostNsPerCall` over it.

⚠️ **`RAPTORMARK_CLOCK_BENCH=1` was never run.** No builder tag was supplied and
`raptormark-builder:latest` is not the newest builder. So the mechanism is
verified against a real wasm module through the real `run.ts` path, and the
*bench* -- lifted guest, `--reentrant`, ecvisor's `fd_write` -- is unrun.

## Neutralization

Every check was broken deliberately and observed to fail with the intended
diagnostic; no compile errors were accepted as neutralization.

* prefix matched with a bare `startsWith` -> `BENCH-MARKED X` stamped as `X`;
  "the prefix must be a whole token" fails.
* stamp taken at the FIRST chunk of a line -> split-marker test reads 100, wants
  140.
* one shared line buffer for all fds -> stdout/stderr test fails.
* partial lines dropped -> split-marker test fails.
* `hostNsPerCall` without the us->ns conversion -> "host-timed = 0 ns/call,
  want 134", which is the original defect reproduced by the guard.
* `hostNsPerCall` building the name without `-US` -> "no HOST-STAMP-GETPID-BEGIN
  line in the output", printing the line it could not find.

## Gates

Go: `gofmt -l e2e/clock_test.go` silent, `go build ./...`, `go vet ./...`,
`go test ./...` all green (`e2e` skips the E2E cases without `RAPTORMARK_E2E`).
Web: `oxfmt --check` clean over 50 files, `tsc --noEmit` clean,
`oxlint --deny-warnings` clean, `vitest run` **104 tests in 15 files**, of which
7 are the new `src/stamp.test.ts`.

---

## 2026-08-21 -- Default signal actions: a ruling, and the three places that act on it

Closes the TODO item "Model default signal actions. SIGKILL, SIGSTOP, and
unhandled terminating signals do not perform their default action even though
posting reports success." Entirely in `runtime/src/context.rs`; `sys.rs`,
`entry.rs` and `intrinsics.rs` are untouched.

### What the code did before

`sys_kill` -> `post_signal` (group queue) or `post_signal_to_thread` (task
queue): set a pending bit, wake an interruptible waiter, `wake_pollers`, return
0 to the guest. `deliver_pending_signals(wait_mask)` then ran at a boundary and
did this with the disposition:

* handler installed -> run it as a nested guest call, consume the bit;
* SIG_IGN -> consume and discard;
* **SIG_DFL -> `continue`.** `// We don't model default actions`.

So a `kill()` with no handler installed reported success and then nothing ever
consumed the bit. The seven boundaries are `do_sleep` (twice), `sys_ppoll`,
`sys_epoll_pwait`, `sys_futex` (resume only), `sys_rt_sigsuspend`, and
`__ecv_warning`'s SIGILL path -- the list in TODO.md, verified rather than
trusted, plus the second `do_sleep` site.

`rt_sigaction` accepts and records a disposition for **any** signal 1..=64,
including SIGKILL and SIGSTOP, where Linux answers EINVAL. `rt_sigprocmask`
will likewise block them.

Exit today is `Pending::Exit(code)` + `suspended`, applied by
`retire_after_suspend` for the CURRENT process: leader -> `Zombie(code)`, other
group members -> `Dead`, `close_all_fds`, `shm_drop_process` per member,
`wake_waiter(ppid)`, `post_signal(ppid, SIGCHLD)`, and `exit_code = code` when
`tgid == 1`. All of it reads the LIVE tables (`self.fds`, not `procs[i].fds`),
which is the constraint that shaped the whole design.

### The ruling

**Term and Core terminate; Ign is left pending; Stop is not modelled; SIGKILL
is unconditional.**

* **Core == Term.** No dump is written, and that is not a shortcut: it is what
  Linux does under the container default `RLIMIT_CORE=0`. `WCOREDUMP` is
  correspondingly not claimed.
* **Ign stays pending rather than being consumed.** Clearing the bit is the
  literal reading and is the one change here that would break a guest that works
  today -- `fd_ready` reports a signalfd readable out of `signals.pending`, and
  PostgreSQL's latch is a signalfd over a SIG_DFL signal. A bit left set is
  observationally identical to Ign for everything but `sigpending(2)`.
* **Stop is NOT implemented, deliberately.** SIGSTOP/SIGTSTP/SIGTTIN/SIGTTOU
  leave the bit pending and do nothing. Stopping is only meaningful with a way
  back and a way to observe it -- SIGCONT to a stopped task, `wait4(WUNTRACED)`,
  `WIFSTOPPED`, SIGCHLD-on-stop -- and for the three terminal signals also a
  controlling terminal and process groups, which this runtime does not have
  (`kill` with `pid <= 0` is ESRCH by construction). It further needs a fifth
  process state that the idle path must not count as a wake source and the
  deadlock detector must not count as a hang, i.e. a scheduler change. Rounding
  Stop up to Term would be strictly worse than the nothing we do now: SIGTSTP is
  sent by things that expect the process back.
* **SIGKILL and SIGSTOP have no disposition.** `rt_sigaction` still accepts one
  (that is a `sys.rs` fix, left to its owner), so the refusal happens where the
  disposition is CONSUMED. This is not cosmetic: SIG_IGN for SIGKILL would reach
  the SIG_IGN arm, `consume_signal` would clear the pending bit, and the
  termination check reads that same bit -- an unkillable process from two lines
  of ordinary guest setup.

### The three places that act on it

`terminating_signal(group_pending, task_pending, blocked, actions)` is the whole
ruling as one free function, taking plain words so it is reachable from
`cargo test` (`sys` and `entry` are `wasm32`-only). `default_action(sig)` is
`signal(7)` as a table. Three callers, all the cooperative equivalent of Linux
performing the action in the target's own context on its way back to userspace:

1. **`deliver_pending_signals`**, after the handler loop -> `arm_signal_exit`,
   which sets the same `Pending::Exit(128 + signo)` + `suspended` that
   `sys_exit` sets, so the teardown is the existing one and not a second one
   written for signals. ⚠️ It does **not** consume the bit and does **not**
   increment the returned handler count. Both are load-bearing: `rt_sigsuspend`
   reports EINTR on a non-zero count, and `__ecv_warning` treats zero as "SIGILL
   reached no handler" and raises the `fatal!` that names the undecoded
   ENCODING -- counting a default-Term SIGILL as delivered would swallow that
   diagnostic and turn a lifter defect into a guest that quietly exits 132.
2. **`retire_after_suspend`, the `Pending::Block` arm** -- the kernel's
   `signal_pending()` test at the top of an interruptible sleep. Here it is
   load-bearing in a way it is not there: a parked task is re-entered only when
   something wakes it, so a process that parks after taking a fatal signal never
   dies at all. It is also the recovery for `ppoll`/`epoll_pwait`/`rt_sigsuspend`,
   which call `deliver_pending_signals` and then `block_current`, overwriting
   `Pending::Exit` -- hence the non-consuming arming, re-derived here rather than
   defended at each `sys.rs` call site where the next boundary added would have
   to remember.
3. **`pick_next`, immediately after `load_current`** -- the kill point. The only
   place every about-to-run task passes through. It must be AFTER the switch
   (the group's arena, fds and disposition table are live only then, and
   `exit_group_current` tears down the LIVE ones), and it is what covers a task
   the signal woke out of a wait whose syscall-resume never runs.

Supporting changes: the `Pending::Exit` arm was extracted verbatim into
`exit_group_current(code)` so 2 and 3 can reuse it; `wake_task_for_signal` and
`post_signal` gained a SIGKILL path that wakes every blocked member of the
group regardless of `signal_interruptible` and regardless of the task's blocked
mask -- otherwise `kill -9` works on an idle process and not on a stuck one,
which is the opposite of what it is for. Cutting those waits short is safe
*because* `pick_next` retires the task before it re-enters the syscall.

### Known deviations, stated rather than papered over

* **`wait4` still reports a normal exit.** `reap_zombie` returns a code and
  `sys_wait4` builds `(code & 0xff) << 8`, so `WIFSIGNALED` is false and the
  parent sees exit status `128 + signo`. That is the shell rendering, non-zero,
  and the signal number is recoverable -- but it is not the kernel encoding.
  Fixing it means giving `ProcStatus::Zombie` a way to say "killed", which is a
  change to the process model and to `sys_wait4`. Deliberately out of scope; new
  TODO entry.
* **Leg granularity.** A self-`raise` (`abort()` is `raise(SIGABRT)`) does not
  die at the `kill` return; it dies at the process's next suspension or next
  selection. Making it immediate means `post_signal` setting `suspended`, and
  `ecv_signal` is documented as callable only BETWEEN slices, where a stale
  `suspended` would fake a suspension that did not happen. Not done.
* **`ppoll` does not install its temporary sigmask**, so a signal unblocked only
  for the wait is not seen by the check, which reads `task_signals.blocked`.
  `rt_sigsuspend` does install it and is therefore exact. Safe direction.
* **A guest with no syscalls is still uninterruptible**, unchanged.

### Neutralization

Nine breaks, each reverted after measuring. Every one produced an assertion
failure with the intended diagnostic; no compile error was accepted.

| break | test | printed |
| --- | --- | --- |
| boundary does not arm the exit | `an_uncaught_sigterm_at_a_delivery_boundary_arms_the_exit` | `expected Exit(143), got None` |
| arming consumes the bit | `arming_the_exit_leaves_the_signal_pending_for_the_pre_block_check` | `the bit was consumed; a boundary that blocks afterwards would lose the ruling` |
| Block arm allowed to park | `a_terminating_signal_forbids_the_process_from_parking` | `left: Blocked, right: Zombie(143)` |
| `pick_next` kill point removed | `a_process_with_a_terminating_signal_is_retired_instead_of_run` | `the process was handed the CPU with a terminating signal due` |
| ...and its module status | `init_killed_by_a_signal_exits_the_module_with_128_plus_the_signal` | `left: Ready, right: Exited(143)` |
| SIGKILL wake removed | `sigkill_wakes_a_task_parked_in_a_wait_no_other_signal_interrupts` | `kill -9 left the target parked on a pipe read nothing will ever satisfy` |
| Stop rounded up to Term | `a_stop_signal_is_not_faked_as_a_termination` | `signal 19 was rounded up to Term` |
| ...at the scheduler | `a_stop_signal_neither_stops_nor_kills_the_process` | `left: Exited(147), right: Ready` |
| default-Ign terminates | `a_default_ignored_signal_is_not_a_termination` | `left: Some(17), right: None` |
| ...at the block check | `an_ignored_signal_does_not_forbid_parking` | `left: Zombie(145), right: Blocked` |
| SIGKILL made catchable | `sigkill_is_neither_catchable_nor_blockable` | `a handler plus a full blocked mask must not make a task unkillable` |
| SIGKILL left in the deliverable set | `a_sigkill_the_guest_tried_to_ignore_still_kills` | `SIGKILL was ignorable: expected Exit(137), got None` |

⚠️ One check was written, found unfalsifiable, and **removed**. The first draft
OR'd SIGKILL back into `deliverable` so the loop would visit it, then forced its
handler to SIG_DFL inside the loop -- which made the visit a `continue`. No
break of that line could fail any test, because the ruling is taken from the
pending bits after the loop. It was replaced by subtracting SIGKILL and SIGSTOP
from `deliverable` outright, which IS falsifiable (row 12 above) and is the
honest statement: those two have no disposition to run.

### Gates

`cargo fmt --check` clean; `cargo test` **197 passed, 0 failed** (was 175, so
+22: 11 pure `default_action_tests`, 11 `sched_tests`); `cargo check --target
wasm32-wasip1` clean. No E2E run -- nothing here is reachable without a lifted
guest, and no builder tag was supplied.

## 2026-08-21 -- TODO sweep: what the list actually contained

Ran `tackle-todos` across the whole tree. Recording the SHAPE of the backlog,
because the four individual entries above say what was fixed and none of them
says what the sweep found out about the list itself.

**Source-code markers are empty.** A repo-wide grep for `TODO`/`FIXME`/`HACK`
over `internal/ runtime/src/ cmd/ e2e/ tools/ builder/ patches/` returns five
hits, and not one is a work item: two are vendored QEMU (`sve.decode`), two are
upstream elfconv context lines living inside `patches/0032` and `patches/0048`
hunks, and one is prose in `builder/ecv-prepare.cpp` referring to "TODO #34".
Every other `TODO` in a `.go` or `.rs` file -- 14 of them -- is a prose
cross-reference pointing AT `.agents/docs/TODO.md`. So this tree keeps its work
list in one place and the grep half of that skill has nothing to find here.
Do not re-run it expecting a yield.

**Two of 70 open entries were already closed by the tree.** Both were closed
here by re-verification rather than by work:

  - Unhandled syscalls 179/216/233/283. All four have real handlers in
    `sys.rs` -- and each carries a comment stating why its answer is the honest
    one, which is precisely the "decide per syscall whether ENOSYS is the honest
    answer" the entry asked for.
  - Focused unit coverage for `load.ts`/`netv1.ts`/`files.ts`/`sockets.ts`. All
    four `.test.ts` files exist (933 lines with `node/sockets.test.ts`).
    ⚠️ Closed as "there is no focused coverage", which is what the entry
    claimed. NOT audited for whether `load.ts`'s silent failure mode is among
    the cases covered.

That is a 3% stale rate on a list whose header already warns that an entry is a
hypothesis until checked. Cheap to confirm, and worth doing before dispatching.

**The remaining 68 partition sharply, and the sweepable slice is thin.** Roughly
7 are decisions reserved to the user (bounded-snapshot default, STABLE_SPLIT
default, shared-name default, inline-CH keep/drop, adopting 0062/0063,
`--enable-all`, README's JIT line); roughly 12 need a cold translate and a
side-built builder image (every instruction-coverage family, the codegen wall,
prune-to-reachable-closure, the ld.so hook cluster); and FIVE were host-side and
fixable without Docker. Four were done (A/B/D/E above); the fifth -- recording
three "honest limits" in README -- was left open by direction.

**Two entries were WRONG in a way that only implementing them revealed**, which
is the finding most worth carrying forward:

  - The `mmap` entry described the overflow as producing a small mapping. Every
    overflowing length rounds to exactly **0**, so the `len == 0` guard does not
    subsume it -- they are independent checks -- and its suggested placement
    ("beside the offset check") sits after the MAP_FIXED branch, where it would
    have left MAP_FIXED still diverging.
  - The signal-boundary list repeated in TODO.md and CLAUDE.md is incomplete:
    `do_sleep` has TWO call sites, so there are seven, not six.

Both entries were written by someone who had read the code. Reading it was not
enough; the arithmetic and the call-site count only came out under an edit.

**A conditional entry that is not yet a task**, left open deliberately: narrowing
`Service-Worker-Allowed: /` is gated on `internal/serve` becoming
general-purpose, and it has not.

## 2026-08-21 -- `bbmiss insn=` dropped, and the producer-level proof it had to be

This one had no journal entry -- the change landed and only `TODO.md` recorded
it. Writing it down because the AUDIT is the durable part, not the deletion.

`bbmiss` printed `insn=0x{iw:08x}` from `self.arena.slice(bb_vma, 4)` in
`ContextInner::block_address`. It was always `0x00000000`, which is a valid
aarch64 word (`udf #0`), so the field read as an answer rather than as an
absence. That is the whole defect: a fabricated encoding is worse than no
encoding.

**The entry was confirmed at the PRODUCER rather than by reading the runtime**,
which is what makes the ruling durable. Re-verified independently here:

  - the arena is filled from exactly one place, `Arena::load_data_sections`
    (`runtime/src/arena.rs`), which copies out of `EcvProgram`'s `data_*`
    tables;
  - those tables are built by `MainLifter::WrapImpl::SetDataSections`
    (`third_party/elfconv/lifter/MainLifter.cpp:213`), whose loop OPENS with
    `continue` on `SEC_TYPE_CODE || SEC_TYPE_UNKNOWN`;
  - `SEC_TYPE_CODE` is assigned to any section carrying bfd's `SEC_CODE`
    (`third_party/elfconv/lifter/Binary/Loader.cpp:371`), so `.text`, `.plt`,
    `.init` and `.fini` are structurally excluded;
  - `grep -rlE 'SetDataSections|SEC_TYPE_CODE' patches/` is EMPTY, so the fork
    does not change either site;
  - none of the ten `.ecv.*` sections carries text -- they are metadata tables --
    and `execmap.rs` holds path -> program-hash, not bytes.

So guest `.text` is not merely absent from the arena today, it is excluded by
construction at the lifter. **Do not re-derive this by reading `context.rs`;
the answer is not there.** If a future diagnostic wants a real encoding it needs
a new producer, not a different read.

The format string moved out into a pure `bbmiss_message(...)` (`context.rs:1746`)
so it is host-testable at all -- `block_address` needs a live `ContextInner` with
a 384 MiB arena and real lifted function pointers, and `fatal!`s on the
no-catch-all path, so the line was otherwise reachable only from E2E. This
mirrors the existing `diag::undecoded_message` precedent, which documents the
same removal for the same reason. Guarded by
`does_not_claim_an_encoding_it_cannot_read` (`context.rs:1783`); neutralized by
re-adding the field, which compiled cleanly and made the guard fire. No consumer
anywhere parses `insn=` out of a `bbmiss` line.

## 2026-08-21 -- Sweep close: combined gate, and what is left unrun

Ties off the five entries above. Findings about the BACKLOG are in
`2026-08-21 -- TODO sweep: what the list actually contained`; this is the state
of the tree afterwards.

**Changed**: `runtime/src/arena.rs`, `runtime/src/context.rs`,
`runtime/src/sys.rs`, `e2e/clock_test.go`, `e2e/mmapfail_test.go`,
`web/bin/run.ts`, `web/README.md`, and new `web/src/stamp.ts` +
`web/src/stamp.test.ts`. Nothing committed.

**Combined gate, run on the merged tree rather than trusted from four separate
reports** -- four agents edited concurrently, and one of them transiently saw
another's in-flight test fail:

```
gofmt -l ./cmd ./internal ./e2e ./tools   silent
go build ./... && go vet ./...            clean
go test ./...                             10 packages ok
cargo fmt --check                         clean
cargo test                                197 passed; 0 failed   (170 at HEAD)
cargo check --target wasm32-wasip1        clean
vitest run                                104 passed / 15 files
```

The 5 remaining `wasm32` warnings (`intrinsics.rs:10`, `context.rs:2527`,
`context.rs:5495`, `diag.rs:130`, `sys.rs:428`) were checked against the diff
hunks and all fall OUTSIDE them. Pre-existing, not introduced here.

⚠️ **UNRUN, and this is the honest limit of the above.** No builder tag was
supplied, so `RAPTORMARK_E2E=1` was never run and neither was
`RAPTORMARK_CLOCK_BENCH=1`. Specifically unverified:

  - the two new `e2e/mmapfail_test.go` cases (`zerolen`, `hugelen`) against the
    native baseline -- though the two raw syscalls WERE run directly on this
    host (aarch64, Linux 6.17, the guest's own architecture) and answered
    EINVAL and ENOMEM;
  - the reworked `e2e/clock_test.go` bench against a real lifted guest. Its
    differential was taken on a purpose-built `wasm32-wasip1` probe with a
    frozen clock, exercising `run.ts -> host.ts -> preview1.fd_write`, NOT
    ecvisor's own `sys_write`;
  - every signal-action path, none of which is reachable without a lifted guest.

The host gates cannot see any of this. Per CLAUDE.md the fast suite is ~20
minutes on a warm cache, not the 60-minute ceiling its `-timeout` suggests, so
this is worth running before any of it is believed.

**Two follow-ups filed** from the signal ruling, both deliberately out of scope
because both are process-model changes rather than signal changes: `wait4` still
reports `WIFEXITED` with status `128+signo` instead of `WIFSIGNALED`, and
`rt_sigaction`/`rt_sigprocmask` still accept `SIGKILL`/`SIGSTOP` where Linux
answers EINVAL (behaviour is already correct; only the errno is wrong).

## 2026-08-21 -- The sweep's E2E gate, and the two things running it taught

The four changes above were host-gate-green but E2E-unrun. Ran the fast suite.

**The builder had to be side-built first, and this is the reusable part.** Every
existing builder bakes `libecvisor.a` from `runtime/`, and three of the four
changes ARE `runtime/`. Running against `:listenfix` -- the newest image, built
the same day -- would have exercised the OLD runtime while every surface signal
said otherwise. That is the 2026-08-18 void gate exactly.

Recipe as executed, layering onto the existing patched base so no patch series
is re-applied and no `BaseID` moves:

```
raptormark build-tools --base raptormark-elfconv-base-patched:sisd0065
docker build -f builder/Dockerfile \
  --build-arg ELFCONV_BASE=raptormark-elfconv-base-patched:sisd0065 \
  --build-arg BASE_ID=sha256:722e7555...  --build-arg TRANSLATE_SH=752b4e33... \
  -t raptormark-builder:sweep0821 .
```

Checked BEFORE building, per CLAUDE.md: `TranslateSH` hashes exactly eight files
(three `internal/builder/*.go`, five `builder/*.cpp|h`) and none of this sweep's
files is among them, so the 6.1 GB object cache stayed valid. Checked after, on
the ARTIFACT rather than the labels: labels identical to `:listenfix`,
`sha256sum /opt/ecvisor/libecvisor.a` **bc57bed -> bd4f5ce**, i.e. differing.

**Result: 99 pass / 0 fail / 29 skip in 231 s** on a warm cache.

### Finding 1 -- the E2E neutralization, which is stronger than the unit one

Ran `TestMmapRefusalsDoNotKillTheModule` against `:listenfix` (old runtime) and
`:sweep0821` (new), one variable. The object was **served from the cache in 1 ms
under BOTH** -- same id `mmapfail_86f24aca9ca2` -- so only `libecvisor.a`
differed. Old runtime:

```
mmapfail: zerolen rc=ok errno=0
FAIL a zero-length mmap fails (errno=0)
mmapfail: hugelen rc=ok errno=0
FAIL an mmap length that overflows page alignment fails (errno=0)
```

`rc=ok` IS the divergence -- a successful mapping of nothing. This proves what a
green run alone cannot: the two new cases execute, and they observe the fix
rather than passing for an unrelated reason. It also confirms `TranslateSH`'s
design empirically -- a `runtime/`-only change does not move the object key, so
the same cached object linked two different runtimes.

### Finding 2 -- ⚠️ 29 SKIPS, and the count is the signal

`CLAUDE.md` records "81 pass / 4 skip" (2026-08-17) and elsewhere 85/4/0 and
90/4/0. **29 is not 4**, and a 0-fail headline over 29 skips is how a suite
certifies work it never ran. The breakdown: 12 browser
(`RAPTORMARK_E2E_BROWSER=1`, needs Playwright), 3 `RAPTORMARK_E2E_SLOW=1`, 1
containerd, 1 clock-bench, 2 expensive fixtures -- **and 10 on "node not on
PATH"**.

Those 10 included `TestGuestTimersSurviveAWallClockStep`, the clock guard, and
every test touching `web/bin/run.ts` -- so the FIRST run left the `--stamp`
change the least-verified of the four while reporting green.

**Node was installed the whole time.** `mise ls` shows node 26.5.1; it is simply
not active in this directory, so `which node` fails in a non-interactive shell
and the suite's gate reads that as absent. The suite already has the escape
hatch -- `RAPTORMARK_NODE=<path>`:

```
RAPTORMARK_NODE=/home/moriyoshi/.local/share/mise/installs/node/26.5.1/bin/node
```

⚠️ **Pass it on every E2E run on this machine**, or ten tests silently vanish.
`mise which node` does NOT resolve it either (it errors "not currently active"),
so the path has to come from `~/.local/share/mise/installs/node/*/bin/node`.

### Also: QUALITY_GATE.md's object-cache path does not exist here

§4 shows `RAPTORMARK_OBJECT_CACHE=/var/cache/raptormark` in both commands.
There is no such directory on this machine. The real warm cache is
`.agents-workspace/objcache` (6.1 GB). Following the documented command
literally means a cold run -- hours -- for a change that cannot invalidate a
single object.

### The re-run with node, and the bench closed

`RAPTORMARK_NODE` set to the mise path: **109 pass / 0 fail / 19 skip, 247 s**.
The 10 recovered tests all pass, including `TestGuestTimersSurviveAWallClockStep`
(the clock guard) and the nine Node-host / re-entrant tests -- so `--stamp`'s
edits to `web/bin/run.ts` did not disturb the host driver. The remaining 19 skips
are Playwright, `_SLOW`, containerd and the two expensive fixtures: gates that
need software or hours this machine was not asked for, not gaps that hid work.

The bench itself was then run directly, closing the last "unrun" caveat --
against a REAL lifted guest, not the proxy:

```
HOST-TIMED:  clock_gettime 210 ns/call, getpid 38 ns/call, host clock read ~172 ns (5.5x)
GUEST-TIMED: clock_gettime 207 ns/call, getpid 38 ns/call
```

⚠️ **The two AGREEING is the expected result and is not evidence the fix works.**
On the shipping runtime the guest clock does advance per read, so both methods
must agree; the change earns its keep only when they DISAGREE, which is the
frozen-clock probe recorded in the `--stamp` entry above (0 vs 134 ns/call). What
this run does prove is the weaker and still necessary thing -- the host-timed
path works end to end against a lifted guest and does not report a zero span.

**Final state of the sweep**: host gates green (10 Go packages, 197 Rust tests,
104 vitest), E2E 109/0/19, and the mmap fix additionally neutralized at the E2E
level against the previous builder. The signal-action paths remain covered only
by their 22 unit tests -- no E2E guest in this tree kills a process with a
default-action signal, so nothing in the suite exercises them.

### Both doc defects FIXED the same day, in both files

The two findings above were recorded as defects before they were repaired; this
closes them so nobody re-opens what is already done.

`QUALITY_GATE.md` §4 now carries working values in both commands --
`RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache"` and a
`RAPTORMARK_NODE` glob over the mise install path -- each annotated with the
reason it matters, which is that BOTH failed silently: a wrong cache path is
created empty rather than erroring, and a missing node skips ten tests while the
suite still reports a clean pass. The stale "same four skips" baseline was
corrected in place with the 2026-08-21 reading (247 s, 109/0/19) and the full
19-skip breakdown, plus the 99/0/29 no-node figure as the worked example of a
pass count RISING while coverage falls.

`README.md` carried the identical `/var/cache/raptormark` in both its E2E
commands and was fixed too. Deliberately NOT by duplicating the machine-specific
values -- README gets the portable path plus a pointer to QUALITY_GATE §4 as the
single place that records working env values and expected pass/skip counts.
Copying them into two files is how the test-count drift in `AGENTS.md` happened,
and a second copy of a number is worse than no copy: two files that disagree
cannot spot the DROP the number exists to spot.

## 2026-08-21 -- "Decouple the registry index from the object" is PARTLY closed, and the blocker is not what the code says

Audited the TODO entry against the tree. Two caches were being conflated, and the
recorded reason for not fixing the second one is wrong on both halves.

**The two layers are genuinely different things.**

`internal/builder/partcache.go` (`ECV_PART_CACHE`) keys one compiled bitcode
PARTITION on its bytes under a compiler salt. `TestPartCacheKeyFollowsContentNotPath`
proves the key follows content, and its comment claims that "is what makes a
shifted program index reuse every unchanged partition". That claim is true, but
the test is not what makes it true -- content addressing alone is not enough,
because a shifted index used to change every partition's BYTES. Two fixes in
`builder/ecv-partition.h` are what closed it: the dead-declaration sweep (an index
shift renames `ecv_program_name_<i>`, which `CloneModule` reproduced into all 80
partitions as an unused declaration -- "55,657 identical IR lines, 6 differing")
and the sort-by-name canonicalisation. So the codegen half IS closed, and it is
the expensive half.

`internal/translate.ObjectKey` (`RAPTORMARK_OBJECT_CACHE`) keys a whole program's
OBJECT, and it still misses. `link.Program.Symbol()` is `ecv_program_<Index>`, so
`Request.Keep` and the generated fragment both carry the index and both are direct
inputs to the key. Confirmed by construction rather than by reading: same ELF,
same `ModuleID`, ecvisor runtime, index 0 keys `1ff47098...` and index 1 keys
`f107d4bf...`; `--runtime upstream` keys `16369a38...` at BOTH indices because it
has neither `Keep` nor a fragment. `FragmentC` differs in exactly four lines, all
index-derived. `TestObjectKeyCoversEverythingBakedIntoTheObject` already asserts
this miss, so it is a live guard on the open state, not an oversight.

Also worth separating: the name the RUNTIME reports is ALREADY content-addressed.
`EcvProgram.name` is `Program.Name` is `translate.ModuleID`, ELF basename plus
content hash. Only the C symbol identifier is index-derived.

**CORRECTION to `internal/translate/translate.go`.** Its `ObjectKey` comment said
the rename was blocked because "ecv_program_<i> is the recovered contract, see
builder/translate-one.sh". That script is not in the tree -- translate-one is
`internal/builder/translateone.go` -- and it is not a runtime contract:
`runtime/src/abi.rs` reads `ecv_programs`, `ecv_program_count` and
`ecv_program_size` and never a per-program symbol name. `ecv_program_<i>` is a
build-time linkage handle with four in-repo consumers (RegistryC's extern,
translate-one's `--keep`, `builder.sideLinkArgs`, `e2e/testdata/embedder.mjs`).
The comment now says that, and names the real blocker.

**The real blocker is the object cache, and the invalidation would be CORRECT.**
Renaming the descriptor changes the object's exported symbol, so the bytes change
and every cached ecvisor object must miss once. `internal/link` is deliberately
absent from `builder.translateSources`, so this does NOT move `TranslateID`; the
miss arrives through `Keep` and the fragment text only. That makes it a cost
decision (hours of re-translation against a warm cache), not a refactor, so it was
reported rather than done. No code change was made to the naming.

**Found on the way, not fixed.** `builder.sideLinkArgs` derives a side module's
`--export=ecv_program_<n>` from the object's POSITION in `--objs`, not from the
manifest. Every caller happens to pass objects in registry order, so it works,
but the assumption has no test and no stated invariant -- and content-addressed
naming would force it into the open, because the symbol would then have to come
from `programs.json` and not every caller writes one.

The cost of a shift is still UNMEASURED. `.agents-workspace/drivers/idxshift`
was written for exactly this and there is no record of it having been run.

## 2026-08-21 -- The shared-`munmap` leak had an over-free hiding behind it

The TODO entry "`munmap` of part of a shared region is ignored" describes a
deliberate trade: match a shared region by its START only, so a partial unmap
never splits it, and accept the leak because leaking beats recycling memory a
guest still maps. The audit found that the start match does not implement that
trade. It implements HALF of it.

`NR_MUNMAP` (`runtime/src/sys.rs`) read `state.arg(0)` and never `state.arg(1)`
on the shared path:

    if let Some(i) = ctx.shm_seg_at(addr) {   // vma_start == addr, length ignored
        ctx.shared_segments[i].mappers.retain(|&p| p != pid);
        ctx.shm_try_reclaim(i);
    }

So a TAIL unmap is ignored (it names no region's start) -- that is the leak the
entry documents -- and a HEAD unmap of ANY length was taken for a full detach.
`munmap(region, 4096)` on a 16 MiB region dropped the caller's claim, and if the
caller was the last mapper `shm_try_reclaim` released all 16 MiB to
`ShmWindow` while the caller still had 16 MiB - 4096 mapped. The next
reservation is then handed an address inside a live region: in the regression
test's arithmetic, a 4 MiB request lands at 0x15c00000 inside a still-mapped
0x15000000..0x16000000. That is the silent-corruption direction the comment
above the arm claims to avoid.

Note the precondition, which is why it has never been observed: reclamation
requires `mappers` to be EMPTY, so the over-free only bites when the process
doing the partial unmap is the region's last mapper. A second process holding
the region keeps it alive and the head unmap merely leaks a claim.

**Fixed, contained.** `SharedSeg::unmap_is_whole(addr, len)` in
`runtime/src/arena.rs` -- `addr == vma_start && len >= self.len` -- and the arm
branches on it, rounding `len` through the existing `mmap_round_len` so an
unmap of the length the guest asked for still matches the region that length
created (a 1000-byte `MAP_SHARED` registers 64 KiB). A partial head unmap now
takes the same path as a partial tail unmap: ignored, logged, leaked. No change
to `ShmWindow`, `adopt_shared_from`, `reset`, the snapshot path, or the shape of
`SharedSeg`. The private arm is unchanged except that its `len` now comes from
`mmap_round_len` rather than an open-coded round that could wrap.

**Splitting was scoped and deliberately NOT built.** It is not one change to the
allocator; it is a change to three identities at once. `SharedSeg.mappers` is a
set of pids with no extents, so after a split nothing can say which process
holds which half. `shm_files` keys a POSIX region by its start VMA -- that is
the identity a second `shm_open` matches on -- and `ShmSeg.vma` keys a SysV
segment by its start VMA, which is how `shmdt` and `shmctl` find the shmid. A
split invalidates both keys. `ShmWindow::release` and `adopt_shared_from` would
both cope with a sub-range unchanged, so the allocator is NOT the obstacle; the
lifetime bookkeeping is. Against that, reachability is low: every PostgreSQL DSM
backend detaches whole mappings, the recovered fixture pins
`dynamic_shared_memory_type=sysv` (which goes through `shmdt`, exact-start by
construction), and no e2e guest performs a partial unmap.

**Tests** (`runtime/src/arena.rs`, host, no Docker):
`only_a_whole_unmap_gives_up_a_shared_region` pins the predicate in both
directions including the rounding; `a_partial_unmap_does_not_recycle_the_shared_window`
mirrors the arm over a real `ShmWindow` and asserts on the recycled ADDRESS, not
on a boolean, because the arm itself lives in the wasm-only `sys` and cannot be
called from a host test.

**Neutralization** (three variants, all behavioural, none a compile error).
Restoring the predicate to `addr == self.vma_start`: the first test fails with
"a HEAD unmap must not give up the 12 MiB the caller still maps", the second
with "a partial unmap dropped the caller's claim on the whole region"
(`[] != [11]`). With that assertion disabled it fails on the bump
(`369098752 != 352321536`); with the window assertions disabled too it fails
with "the next reservation landed at 0x15c00000, inside the live region
0x15000000..0x16000000". Worth recording that `w.free.is_empty()` alone does NOT
fire -- `release` absorbs the freed range straight back into the bump -- so the
`w.top` assertion beside it is load-bearing.

**Found on the way, not fixed.** (1) `NR_MREMAP` does not consult
`shared_segments` at all: a shrink calls `mmap_release` on the shared range
(harmless -- no exact match in `mmap_live`, so it is a no-op), but a GROW with
MREMAP_MAYMOVE copies the bytes into a PRIVATE mapping and returns it, silently
un-sharing a region while the old one stays registered. (2) Several comments in
`arena.rs`/`context.rs` cite `.agents/docs/SHAREDMEM.md`, which does not exist in
this tree.

## 2026-08-21 -- Second sweep pass: the browser suite, and two prices measured

Continuation of the TODO sweep. Four more entries closed by verification, two
priced by measurement, and the E2E gate taken from 99/0/29 to **121 pass / 0
fail / 7 skip**.

### The browser suite has never run in this tree, and two things blocked it

Chromium is now GREEN -- all 14 specs. Getting there needed no code change and
two fixes worth recording, because both would recur:

1. **`web/dist/raptormark.js` was STALE**, built 17:48:06 against sources
   changed 18:15:39 by this sweep's own `--stamp` work. **The browser runs the
   BUNDLE, not the TypeScript the Node host reads**, so a `web/` change reaches
   the Node tests and not the browser ones. `browser_test.go` catches this
   explicitly and names the fix (`npm run build`); it is a good guard and it
   fired correctly. ⚠️ Any change under `web/src` needs the bundle rebuilt
   before the browser suite means anything.
2. **`npx` is not on the non-interactive `PATH`.** `RAPTORMARK_NODE` points the
   Go side at the node binary but the specs shell out to `npx` for Playwright,
   so the fix is to put mise's node `bin` on `PATH`, not just to set the var.

### WebKit: downloaded, unlaunchable, and it inverts the gate

`playwright.config.ts` already takes `RAPTORMARK_BROWSERS=firefox,webkit` with
no edit. WebKit IS in `~/.cache/ms-playwright`, and every webkit spec still
fails at **0-1 ms**: `Host system is missing dependencies to run browsers`
(`libavif16`, `gstreamer1.0-libav`). It needs
`sudo npx playwright install-deps webkit` -- root, not run.

⚠️ Passing `RAPTORMARK_BROWSERS=webkit` here turns 121/0/7 into **110/11/7**,
and all eleven failures are that one message. A reader seeing 11 red would
reasonably hunt for a raptormark defect; there is none.

### The index-shift price, from a driver written 10 days ago and never run

`.agents-workspace/drivers/idxshift` exists precisely to price "decouple the
registry index from the object" and had never been executed. On
`busybox-musl.fused`:

| | wall | codegen | partitions from cache |
|---|---|---|---|
| index 0 (cold) | 19 s | 12.2 s | 0/80 |
| index 1 (shifted) | **7 s** | **0.2 s** | **76/80** |

**The partition cache already absorbs the expensive half.** The remaining ~7 s
is serial-pass time. The rename that would remove it invalidates every cached
ecvisor object -- hours -- because `ecv_program_<i>` is in the object's symbol
table and therefore in `ObjectKey`. Priced and DECLINED.

⚠️ **The first reading was wrong and inverted the conclusion**: without
`RAPTORMARK_PART_CACHE` the driver reports 0/80 and 20 s for the shifted index,
which prices a configuration nobody runs. Set the partition cache, and read the
hit rate rather than the driver's `stable-split:` line -- that line prints the
HOST `ECV_STABLE_SPLIT`, which is empty even when the pipeline has it, because
`RAPTORMARK_STABLE_SPLIT` is translated inside the container.

### A checkbox reported ticked was not ticked

The `bbmiss insn=` entry was reported closed on the day and the box was still
`[ ]` -- lost when four agents wrote `TODO.md` concurrently. The CODE had
landed. **Verify a checkbox against the tree, not against a report.** An audit
of the other five entries from that batch found them correctly marked, so this
was one lost edit rather than a systematic failure.

Separately, nine entries carried a DONE/CLOSED marker in the BODY with an
unticked box. Four were genuinely finished and are now closed with evidence
(the exec-map fallback, the postgres milestone ladder, signal-handler delivery
boundaries, and `FNMUL`); the other five are legitimately half-done and stay
open. An entry that says "DONE" inside an open box is the most expensive kind of
stale, because it reads as work remaining and costs a re-derivation to disprove.

## 2026-08-21 -- Sweep close: SIGKILL/SIGSTOP admission, and the final gate

Last item of the sweep. `rt_sigaction` and `rt_sigprocmask` accepted SIGKILL and
SIGSTOP; `context.rs` already refused the disposition where it is CONSUMED, so
behaviour was correct and only the error codes and round-trips were wrong.

**The detail the TODO entry did not carry, and it matters.** Linux's
`do_sigaction` conditions the refusal on `act`:
`!valid_signal(sig) || sig < 1 || (act && sig_kernel_only(sig))`. So
`sigaction(SIGKILL, NULL, &old)` **SUCCEEDS** and reports SIG_DFL. The tempting
rule "these two are always EINVAL" is wrong and would break the
`for (sig = 1; sig <= NSIG; sig++) sigaction(sig, NULL, &old)` sweep programs
use to snapshot inherited dispositions. Ruling: refuse only when INSTALLING.

Two more facts from the same read, both fixed: the kernel copies `oldact`/
`oldset` out only on SUCCESS, and both functions here wrote the old value first
and then returned EINVAL. And `rt_sigprocmask` drops the two bits from the
REQUEST before applying `how`, which is where `sigdelsetmask` sits -- masking
the result instead is observably different, and exactly one test separates the
two implementations.

⚠️ **A wasm32 trap worth remembering**: `usize` is 32-bit there, so a
`sig as usize` range check folds `0x1_0000_0009` onto SIGKILL. The helper takes
the raw `u64` so the SIGNATURE prevents it rather than the caller. Guarded by
`a_signal_number_above_32_bits_is_not_truncated_into_range`.

Both helpers live in `context.rs`, not `sys.rs`, for the reason this sweep hit
three times: **`lib.rs` gates `mod sys` to `#[cfg(target_arch = "wasm32")]`, so
a `#[test]` in `sys.rs` never runs.** `arena::madvise_zeroes` was the precedent;
`arena::mmap_round_len` and now these two follow it. Anyone adding a syscall
decision should extract the pure part on sight.

Stated plainly and NOT covered: the two `oldact`/`oldset` ordering changes are
in `sys.rs`, argued in comments and unverified by the host gate. Left open in
TODO: `sys_rt_sigsuspend` and the `blocked |= act.mask` in handler delivery
install masks verbatim, same class, harmless today because the consumption-site
net catches them.

### Final gate

Builder rebuilt to `raptormark-builder:sweep0821c` (labels identical,
`libecvisor.a` 9d75fb1 -> 08dbdac), browser bundle rebuilt:

```
cargo fmt --check / cargo check --target wasm32-wasip1   clean
cargo test                                               208 passed; 0 failed
gofmt / go build / go vet / go test ./...                clean
E2E (browser on, chromium)                121 pass / 0 fail / 7 skip, 270 s
```

**99/0/29 at the start of the sweep, 121/0/7 at the end** -- and the 22 extra
passes are almost entirely coverage that already existed and was being skipped,
not new tests. That is the sweep's most transferable result: the largest single
gain came from making the suite RUN, not from writing anything.

Remaining 7 skips are all deliberately gated: 3 `RAPTORMARK_E2E_SLOW`, 1
containerd, 1 clock-bench instrument, 2 expensive fixtures.


## 2026-08-21 -- an executed-set census for undecoded instructions (`RAPTORMARK_ECV_UNDEC_CENSUS`)

**Why.** TODO's `## Instruction coverage after patches 0063/0064` says site count
is the wrong ranking for capability: 0063 cleared the largest family (706 `tbl`)
and moved nothing observable, 0064 cleared 11 sites and unblocked the postgres
planner. The missing half is reachability -- which undecoded sites a real
workload actually EXECUTES. Every lifter patch costs a `BaseID` change and a cold
6.2 GB object cache, so choosing off the PRESENT set is expensive to get wrong.

**The mechanism was already 90% there.** `__ecv_warning` posts a thread-directed
SIGILL and calls `deliver_pending_signals`. If a handler runs it logs and
RETURNS -- execution already continues past the undecoded instruction, which is
what makes postgres's ARMv8-CRC32C probe work. Only the no-handler branch
aborts. Census mode changes that one branch: record and return.

**⚠️ UNSOUND, and it must never be sold as anything else.** Returning skips the
instruction's effect, so everything after the first skip is garbage. A crash,
a hang or wrong output under this mode is evidence about NOTHING except the site
list. It is a census instrument, never a way to "get further". The runtime says
so in four places: a four-line banner at arm time, every `addr=` line
(`SKIPPED-UNSOUND`), the `diag::set_undec_census` doc comment, and the comment
in `__ecv_warning` itself.

**Where each piece lives, and why.** `lib.rs` gates `mod intrinsics` AND
`mod sys` to `#[cfg(target_arch = "wasm32")]`, so a `#[test]` in either never
runs -- the trap that already cost `arena::madvise_zeroes`,
`arena::mmap_round_len`, `context::bbmiss_message` and `diag::undecoded_message`
their first homes. So the gate, the dedupe table, the banner text and the
messages are all in `diag.rs` (host-compiled); `intrinsics.rs` keeps only the
branch.

* `diag::census_on(raw)` -- the gate as a pure function of the stored byte, so
  "uninitialised (0) is OFF" is testable rather than a property of when
  `init_diag_flags` ran. This is the load-bearing test: a census that defaulted
  ON would convert the runtime's one LOUD failure into silent garbage.
* `diag::undecoded_disposition() -> Undecoded::{Fatal,Census}` -- the decision
  `__ecv_warning` consults. A two-armed enum exists purely so a host test can
  stand next to a wasm-only branch.
* `diag::CensusTable::note` -- dedupe by unique ADDRESS, cap 4096 unique keys,
  pure over an explicit table. Follows `context::bbmiss_first_time`: a const
  `Mutex::new` and no `LazyLock` (a lazy initialiser on the fork path is what
  caused the post-fork `panic_poisoned` infinite loop), and a cap on unique keys
  rather than occurrences ("an instrument verbose enough to change the outcome
  has stopped measuring the thing").
* One addition beyond `bbmiss`: `Census::Truncated`, logged exactly once if the
  cap clips the list. A silently truncated census reads as a COMPLETE one, which
  is the exact false-completeness this whole exercise exists to avoid.

**One behavioural detail that is not obvious.** The census branch calls
`consume_signal(1 << (SIGILL-1))` before returning. The SIGILL posted at the top
of `__ecv_warning` found no handler and is still pending; leaving it queued lets
a handler installed LATER receive a trap this thread never took -- and postgres's
SIGILL handler `siglongjmp`s, so a stale one does not stay a diagnostic.

**Reading the census.** `grep '\[undec_census\] '` on stderr; the site list is
`grep -o 'addr=0x[0-9a-f]*'`. Bare hex, pasteable into
`llvm-objdump --start-address=`. No `insn=` field, for the reason recorded on
`undecoded_message` and `bbmiss_message`: the arena holds no guest `.text`, so
any run-time encoding read is a fabricated `0x00000000` -- a valid `udf #0` that
reads as an answer.

### Gate

```
cargo fmt --check                              clean
cargo test                                     220 passed; 0 failed   (208 before)
cargo check --target wasm32-wasip1             clean
```

13 neutralizations, all confirmed to FAIL with the intended diagnostic and none
a compile error: gate default (`raw == 2` -> `raw != 1`), disposition ignoring
the gate, dedupe removed, cap removed, truncation announced repeatedly,
truncation text hardcoding the cap, decimal instead of hex in the line, a
re-added `insn=0x00000000`, banner emitted unconditionally, banner losing the
word UNSOUND, `fatal!` deleted from `__ecv_warning`, the census branch bypassing
`undecoded_disposition`, and the dedupe call removed from `__ecv_warning`.

⚠️ Not run, deliberately: the E2E suite and any image build. This change is
`runtime/`-only, so it relinks rather than re-lifts.

⚠️ Noted while diffing: `runtime/src/intrinsics.rs` already carried an
UNCOMMITTED correction from an earlier session (the "CORRECTED 2026-08-21" block
about the runtime being unable to read guest `.text`), which HEAD 8286235 does
not have. It was left alone. A `git diff` on that file is therefore two sessions'
work, not one.

## 2026-08-21 -- E2E guard for the undecoded-instruction census, and the armed-SIGILL exit it found

`e2e/undeccensus_test.go`. One guest, one lift, two runs of the SAME module
differing only in whether `RAPTORMARK_ECV_UNDEC_CENSUS=1` is passed to wasmedge
with `--env`.

### The fixture

`isb sy` (`0xd5033fdf`), emitted as `.inst` inside an ASM loop so there is
exactly ONE static site executed 64 times. The loop is asm and not C because a C
`for` around an `asm volatile` can be unrolled, and several static sites are
several addresses the census is entitled to report several times -- an unrolled
loop would turn the dedupe assertion green by removing the thing it tests.
`soleEncodingAddrInSymbol` re-checks "exactly one occurrence inside `isb_loop`"
against the built ELF, and the address it returns (`0x40076c`) is what both arms
assert against, so neither run supplies its own expected value.

The loop contains no syscall, and an `add` immediately after the `.inst` counts
arrivals at the instruction AFTER the skipped one. `UNDEC-AFTER iters=64` is
therefore independent evidence that the one site executed 64 times -- without
it, "one census line" is equally consistent with a loop that ran once.

Verified `0xd5033fdf` disassembles as `isb` with objdump on the builder image
before trusting it (`400720 <isb_loop>: ... 40072c: d5033fdf isb`).

### Results on `raptormark-builder:census`

* **default (unset)**: exit 1, `[ecvisor] fatal: undecoded instruction at
  0x40076c: ... not a missing basic block ...`, `UNDEC-BEFORE` printed and
  `UNDEC-AFTER` not, and NOT one `[undec_census]` line. This arm is also the
  proof that `isb` is genuinely undecoded through `patches/0065` -- it is an
  assertion, not a check taken once.
* **armed**: the 4 banner lines, then exactly ONE
  `[undec_census] pid=1 addr=0x40076c SKIPPED-UNSOUND: ...` for 64 executions,
  then `UNDEC-AFTER iters=64`. No fatal, no `census TRUNCATED`.

### ⚠️ FINDING: an armed run does not survive to the end of the workload

The armed run exits **132**, and `UNDEC-AFTER2` never prints. The census returns,
but the process is already condemned when it does:

* `__ecv_warning` posts SIGILL and calls `deliver_pending_signals`, which ends
  with `if let Some(sig) = self.pending_termination() { self.arm_signal_exit(sig) }`.
  So before it returns 0, SIGILL's default action has set
  `Pending::Exit(128 + SIGILL)` and `suspended = true`.
* The census branch consumes the pending signal BIT (`consume_signal`) and
  returns. Nothing clears the armed exit.
* The svc trampoline picks `suspended` up at the guest's NEXT SYSCALL. That
  syscall COMPLETES (`UNDEC-AFTER` is printed) and then the leg unwinds. Exit
  132, no diagnostic.

`arm_signal_exit`'s own doc calls this "normally unreachable rubble behind a
process that is already dying", which was true while `fatal!` was the only
disposition. The census branch is the path that reaches the rubble. The
consequence is that ONE armed run enumerates the sites executed between the first
skip and the next syscall -- **not** "every undecoded site a workload executes",
which is what the mode's own comment and TODO.md both claim. On a real workload
that is close to one site per run, so the planned postgres census would have
reported a single address and been read as a complete list.

Asserted rather than tolerated (`census` steps 6 and 7): the test requires
`exit status 132` and the absence of `UNDEC-AFTER2`, with failure text naming the
fix (clear `pending`/`suspended` in the census arm, or suppress the default
action when the disposition is `Census`) so that fixing the runtime fails here
loudly instead of silently.

### Neutralization

`raptormark-builder:sweep0821c` -- identical `raptormark.base_id`
(`sha256:722e7555…`) and `raptormark.translate_sh` (`752b4e33…`), so the object
cache served the SAME translated object to both, and only `libecvisor.a` differs.

* `default` still **PASSES** there -- the default path is unchanged, and the
  guest reaches a real undecoded site on both runtimes.
* `census` **FAILS** with 8 assertion failures: `UNDEC-AFTER iters=64` absent, no
  `addr=0x40076c SKIPPED-UNSOUND`, address reported 0 times not 1, all three
  banner substrings missing, the forbidden fatal text present, and `exit status
  1` instead of 132. The module printed
  `[ecvisor] fatal: undecoded instruction at 0x40076c: ...` with the variable
  passed. Not a compile error and not a skip.

### Gate

`gofmt -l` clean, `go build ./...`, `go vet ./...`, `go test ./...` all green;
the new test passes on `:census` (3.2 s warm) and fails on `:sweep0821c`. Gated
on `RAPTORMARK_E2E` alone with no second opt-in -- one small static guest, one
cached lift, two runs.

## 2026-08-21 -- The armed-SIGILL exit is fixed by DECIDING before POSTING, and the census now spans syscalls

Fixes the defect the entry above found and asserted. `RAPTORMARK_ECV_UNDEC_CENSUS`
now enumerates every undecoded site a workload executes in ONE run, which is what
it always claimed to do and never did.

### The sequence, read off the code before changing anything

1. Lifted code reaches an undecoded instruction -> `__ecv_warning(.., addr, ctx)`.
2. `post_signal_to_thread(pid, SIGILL)` sets the thread's pending bit.
3. `deliver_pending_signals(blocked)`:
   * `deliverable_set` keeps SIGILL (pending, not in the mask);
   * `act.handler == 0` is SIG_DFL, so the loop `continue`s and LEAVES the bit
     pending. `ran` stays 0.
   * after the loop, `pending_termination()` -> `terminating_signal` finds
     SIGILL pending, unblocked, SIG_DFL, `default_action` = Core -> `Some(4)`;
   * `arm_signal_exit(4)` sets `pending = Pending::Exit(132)` and
     `suspended = true`. **This happens BEFORE the function returns 0.**
4. `__ecv_warning` reads `undecoded_disposition()`. `Fatal` aborts, so the armed
   exit is the "normally unreachable rubble" `arm_signal_exit`'s doc describes.
   `Census` consumes the signal BIT and RETURNS -- and nothing clears the exit.
5. The guest's next `svc`: `__remill_syscall_tranpoline_call` runs `sys::svc`
   (the syscall COMPLETES -- this is why `UNDEC-AFTER` printed), then sees
   `ctx.suspended`, clears it, `on_suspend()`, `set_unwinding(true)`.
6. The leg unwinds to `entry.rs`, `retire_after_suspend()` takes the
   `Pending::Exit(132)` arm, `exit_group_current(132)`. Exit 132, no diagnostic.

### The fix, and why this shape

`__ecv_warning` now asks whether a handler would take the signal BEFORE it posts
one, and in census mode with no handler it **never posts at all**:

```rust
if matches!(diag::undecoded_disposition(), Undecoded::Census)
    && !(*ctx).delivers_to_handler(context::SIGILL) { ..log..; return; }
```

New in `context.rs`: `pub fn signal_reaches_handler(sig, blocked, actions)` --
the mirror of `terminating_signal`, applying exactly the two tests the delivery
loop applies before its handler arm (`deliverable_set`'s mask subtraction, then
`handler > 1`, i.e. neither SIG_DFL nor SIG_IGN), plus the SIGKILL/SIGSTOP
refusal every reader of a disposition owes -- and the method
`EcvContext::delivers_to_handler(sig)` over the live tables.

**Rejected: post as before, then clear `pending`/`suspended` in the census arm.**
It has to CANCEL a termination, and at that point it cannot tell the one it just
armed from one that was already due -- a real `kill`, a SIGKILL, or a
`pending_termination` naming some other signal entirely. Cancelling a real
termination is a far worse defect than the one being fixed. Nothing in the new
shape cancels anything, and `arm_signal_exit` keeps its contract: what it arms
stays armed.

The three invariants hold, by construction rather than by care:

* **Census OFF is byte-for-byte today.** The gate is the first term of the `&&`;
  with it false the function runs the old code unchanged and unhandled is
  `fatal!`.
* **A guest WITH a SIGILL handler still gets the signal.** `delivers_to_handler`
  true takes the post-and-deliver path, censused or not. PostgreSQL probes for
  ARMv8 CRC32C by executing it under a handler that `siglongjmp`s, so this is not
  a diagnostic it could afford to lose -- it is how it decides what its CPU does.
* **No `Pending::Exit` is cancelled.** Nothing is cleared anywhere.

The `fatal!` after `deliver_pending_signals` became UNCONDITIONAL. Reaching it
means the disposition is `Fatal` or a handler was expected, and an expected
handler increments `ran` (the only path to its arm consumes the bit and calls
`run_signal_handler`). Re-testing the disposition there would put the census back
BEHIND the arming, which is the bug.

### Host tests (5 new, `cargo test` 220 -> 225)

In `context::sched_tests`, because `intrinsics` is wasm-only:

* `posting_an_unhandled_sigill_condemns_the_process_before_delivery_returns` --
  the premise, stated as a fact: `ran == 0` AND `Pending::Exit(132)` AND
  `suspended`, all from one `deliver_pending_signals`.
* `delivers_to_handler_is_true_only_for_a_real_unblocked_handler` -- SIG_DFL no,
  SIG_IGN no, handler address yes, blocked no.
* `the_handlerless_shapes_run_nothing_but_do_not_all_arm_an_exit` -- **why the
  predicate cannot be replaced by reading the return value.** All three
  handlerless shapes give `ran == 0`; only unblocked SIG_DFL arms an exit. A
  census that repaired the damage afterwards would be guessing which it was in.
* `no_handler_can_be_reached_for_sigkill_or_sigstop`, and the counter-check that
  SIGILL still can.
* `an_out_of_range_signal_reaches_no_handler_instead_of_indexing`.

`diag::undec_census_tests::ecv_warning_still_aborts_and_routes_the_census_through_the_gate`
gained three ORDER assertions over the comment-stripped source: the positions of
`undecoded_disposition`, `delivers_to_handler` and the census `return;` must all
precede `post_signal_to_thread`.

### The E2E guard, inverted and strengthened

`e2e/undeccensus_test.go`. The old assertions (`exit status 132`, `UNDEC-AFTER2`
absent) are now their opposites. The guest gained a SECOND undecoded instruction
in its own `noinline` asm loop -- `0x6e3b2fff uqsub v31.16b, v31.16b, v27.16b`,
listed under `## Instruction coverage after patches 0063/0064` in `json_lex` --
with the `printf`/`fflush` that used to end the run sitting BETWEEN the two
sites. Two distinct sites separated by a syscall is precisely the property the
defect removed and the property one site cannot see.

`uqsub` is harmless to skip for the same reason `isb` is: it writes `v31` and
reads `v27`, both in the asm block's clobber list, so no live value depends on
the write that never happens.

`main` now takes `argc`: one extra argument makes the guest skip `isb_loop`.
That is what the new `defaultSecond` subtest needs -- with the census OFF the
guest dies at the FIRST site, so the second site needs its own run to prove it is
genuinely undecoded by the same method as the first (die naming its address).
Without it, "the census reported two addresses" would be satisfied just as well
by a decoder that had learned `uqsub`. ecvisor hands a module's wasmedge
arguments to the guest as argv when there is no boot record (`entry.rs`) and
passes NO environment through, so `argc` is the only switch available.

**Two extra arguments select a SIGILL-handler mode**, for the new
`handlerDefault` / `handlerCensus` pair -- the invariant the fix must not
change, and until now it had NO e2e coverage at all. `e2e/crc32_test.go` reads
as if it covered it and does not: `patches/0027` taught the lifter `crc32c*`, so
that guest has executed a DECODED instruction ever since and never reaches
`__ecv_warning`. The new arms install a `signal(SIGILL, ..)` handler, take ONE
undecoded site under it, and require `UNDEC-HANDLER caught=1 stepped=1`, a clean
exit, no fatal, and -- with the census armed -- **no census line for that
address**. Note the guest reports `caught=0` on real hardware: `isb` is legal
there, and it is only a lifter without the decoder that turns it into SIGILL,
which is the situation postgres's probe is written for. The handler RETURNS
rather than `siglongjmp`ing, which is the weaker of the two shapes; the longjmp
variant is still uncovered.

### Results on `raptormark-builder:census2`

```
[undec_census] ⚠️  UNDECODED-INSTRUCTION CENSUS IS ON (RAPTORMARK_ECV_UNDEC_CENSUS).
[undec_census] ⚠️  THIS RUN IS UNSOUND. ...
UNDEC-BEFORE
[undec_census] pid=1 addr=0x40076c SKIPPED-UNSOUND: ...
UNDEC-AFTER iters=64
UNDEC-AFTER2 iters2=64
[undec_census] pid=1 addr=0x40078c SKIPPED-UNSOUND: ...
UNDEC-AFTER3
```

Exit 0. Exactly two `SKIPPED-UNSOUND` lines, one per address, after 64 executions
each -- so nothing in glibc's `printf`, `fflush` or `exit` path adds a third, and
the strict count is a real bound rather than an approximation. `default` and
`defaultSecond` both die with `[ecvisor] fatal: undecoded instruction at
0x40076c` / `0x40078c`.

### Neutralization

`raptormark-builder:census` -- the pre-fix runtime, identical
`raptormark.base_id` (`sha256:722e7555…`) and `raptormark.translate_sh`
(`752b4e33…`); the object cache reported "served from the object cache (1ms)" on
both, so the SAME translated object ran under both `libecvisor.a`.

* `default` **PASSES**, `defaultSecond` **PASSES** -- the default path is
  untouched, and both instructions really are undecoded on this base. (The
  handler arms also pass there: `:census` posts BEFORE deciding, so a guest with
  a handler still received the signal. Their neutralization is separate, below.)
* `census` **FAILS**, 6 assertions:
  * did not reach `UNDEC-AFTER2 iters2=64`
  * did not reach `UNDEC-AFTER3`
  * no census line for `0x40078c`
  * `0x40078c` reported 0 times after 64 executions, want 1
  * 1 census line, want exactly 2
  * `exit status 132`, want a clean exit

  Not a compile error and not a skip: the module printed the banner, one line for
  `0x40076c`, `UNDEC-AFTER iters=64`, and stopped.

**The handler guard has its own image.** `raptormark-builder:census2neut`
(`libecvisor.a` `8a0e85f6…`) is `:census2` with `&& !(*ctx).delivers_to_handler(
crate::context::SIGILL)` deleted, so the census arm swallows a SIGILL the guest
had a handler for. `handlerCensus` is the ONLY subtest that fails there, with
exactly the two intended messages:

* `the guest's SIGILL handler did not run exactly once for its one undecoded site`
* `the census recorded 0x400800 although the guest has a SIGILL handler for it`

`handlerDefault` still passes there, which is the control: the census being off
is what makes the deleted term irrelevant.

Three host neutralizations as well, all source edits that compile:

* `signal_reaches_handler`'s `handler > 1` -> `!= 0`: two tests fail, with
  `SIG_IGN (1) discards the signal; no guest code runs`.
* dropping its blocked-mask test: `a blocked signal is subtracted by
  `deliverable_set` and reaches no handler, however it is installed`.
* moving the census arm back inside `if deliver_pending_signals(..) == 0 {`:
  `the disposition is read AFTER the SIGILL is posted. By then
  `deliver_pending_signals` has already armed `Pending::Exit(128 + SIGILL)` ...`

### Image

Layered onto the existing patched base, `build-tools` first:

```
raptormark build-tools --base raptormark-elfconv-base-patched:sisd0065
docker build -f builder/Dockerfile \
  --build-arg ELFCONV_BASE=raptormark-elfconv-base-patched:sisd0065 \
  --build-arg BASE_ID=sha256:722e7555ae2251dfc3a6d2f8d01ef0c7ff447ab12d3edb5538ca5b75c4745e9b \
  --build-arg TRANSLATE_SH=752b4e33108de78b65e773025e49c3106ba6fe611d74650801479a1a334676e9 \
  -t raptormark-builder:census2 .
```

Both labels identical to `:census`; `/opt/ecvisor/libecvisor.a`
`f2dc9d91…` (`:census`) -> `bb2ba73b…` (`:census2`), so the change is in the
artifact and not only in the source.

### Gate

`gofmt -l` clean, `go build ./...`, `go vet ./...`, `go test ./...` green;
`cargo fmt --check`, `cargo test` **225 passed**, `cargo check --target
wasm32-wasip1` clean; `TestUndecodedCensusFiresAndStaysOff` (5 subtests) passes
on `:census2` and fails on `:census` and on `:census2neut`. The FULL fast e2e
suite on `:census2`: **233 s, 100 pass / 29 skip / 0 fail** -- the 29 are the
browser, node-host, containerd and fixture-gated arms, unrelated to this change.

⚠️ 233 s is well under the 1186 s recorded on 2026-08-17. The object cache was
warm for every lift here; do not read this as the suite getting cheaper.

## 2026-08-21 -- The census meets real workloads, and two things the tree did not know

### python:3-slim executes ZERO undecoded instructions

Module relinked onto `raptormark-builder:census2`, run over all three pybench
workloads with `RAPTORMARK_ECV_UNDEC_CENSUS=1`:

| workload | census lines | exit | output |
|---|---|---|---|
| startup | **0** | 0 | `STARTUP-OK` |
| realistic | **0** | 0 | `REALISTIC-OK 1826320 300 0` |
| callheavy | **0** | 0 | `CALLHEAVY-OK 32851` |

The banner fired on all three (it is emitted at ARM time, not at first skip),
and not one `addr=` line. Output is byte-identical to the census-OFF baseline,
which is the control that makes this a measurement rather than an absence:
`REALISTIC-OK 1826320 300 0` on both.

Two conclusions, and the second is the useful one:

1. **A real workload is a silent control.** The instrument does not fire
   spuriously and does not perturb a guest it has nothing to say about.
2. **Python cannot validate the census, and neither can it justify a lifter
   patch.** On a base carrying patches through 0065, python's undecoded
   *executed* set is empty. Whatever `st1`/`sli`/`fcvt` sites the static
   inventory finds in an image, python reaches none of them on these three
   paths. ⚠️ Per-run and per-input, as always -- this is a lower bound.

### ⚠️ The pgcl4 closure's objects are ORPHANED by a ModuleID change

`.agents-workspace/tmp/dup/pgcl4` still holds 1.7 GB of translated objects, four
fused ELFs, a 471 MB rootfs and `go.sh` -- about 30 minutes of translation,
survived from 2026-08-17. **Today's tooling cannot link them.**

`ModuleID` is `basename + sha256(elf)[:12]` and folds in nothing else. The fused
ELFs are untouched (mtime `Aug 17 13:52`) and hash to `2789286606bb` (postgres)
and `687c3e70f865` (dash). The objects beside them are named `9047f5810319` and
`e747123245aa`. So the derivation CHANGED after they were written -- `lift`'s own
header records the direction ("pgmulti still folds in `TranslateID`, which is no
longer how objects are named"), and `relink.log` shows the same driver producing
the old names on 2026-08-17.

`lift -link-only` therefore fails with four `clang: error: no such file or
directory: /out/..._2789286606bb.o`, naming objects that were never written.

⚠️ **Do not read that error as a missing translation.** It is a naming
mismatch over objects that are present, complete, and 1.7 GB. Anyone reaching
for a surviving closure to save a re-translation will hit this; the objects are
only reusable by a driver of their own vintage.

### Method note that saved a wrong sidecar

Building the pybench sidecar with `pgrootfs -prog <path>=<elf>` produced an exec
map naming `python_glibc_fused_a7f81a1b25c2` while the link produced
`..._e5234af7e1cf`. Cause: `-prog` derives the id itself and its `-builder`
flag **defaults to `raptormark-builder:gate2`**, which was not the builder in
use. That is cause 3 of the exec-map entry in TODO.md ("the sidecar generated
with a DIFFERENT builder tag"), reproduced by accident.

The fix is the one that entry already records: `-manifest <linkdir> -map
path=INDEX`, which derives nothing and reads `programs.json` the link wrote.
Rebuilt that way, the sidecar names `..._e5234af7e1cf` and matches. ❌ Prefer
`-manifest`/`-map` over `-prog` always; `-prog` is a second derivation and this
tree has now had five incidents from exactly that shape.

## 2026-08-22 -- postgres executes ZERO undecoded instructions, and what that decides

The reachability question TODO.md calls "the missing half" now has an answer on
the workload the inventory was taken from.

Four-program closure (postgres, initdb, dash, psql) relinked onto
`raptormark-builder:census2`, sidecar built from the link's own `programs.json`,
run through the real `boot.sh`: initdb, postmaster, then SQL.

**Census OFF -- the run COMPLETES, and completes CORRECTLY:**

```
CREATE TABLE / INSERT 0 3 / UPDATE 1 / DELETE 1
 id |     s      | length          rows | sum
  1 | raptormark |     10             2 |   4
  3 | WASM       |      4
count(pg_database)=3   count(pg_authid)=16
checkpoint complete ... lsn=0/1511820
BOOT: DONE
```

The values are RIGHT, not merely present: `WASM` is uppercased by the UPDATE,
`sum=4` is 1+3 after the DELETE removed id 2. `pg_database` and `pg_authid` are
seq scans over REAL catalog relations, which is the bar TODO.md sets for a
postgres validation (a constant-folded query never enters the planner).

**Not one `__ecv_warning`. Zero undecoded instructions executed.**

### What this decides

| workload | undecoded sites EXECUTED |
|---|---|
| python:3-slim, three workloads | **0** |
| postgres: initdb + postmaster + DDL/DML/aggregates/catalog seqscans | **0** |

The static inventory for this closure lists **2,244 real sites**, led by `st1`
686, `sli` 574, `fcvt` 212 -- **1,472 sites in the top three families alone,
reached by NEITHER workload.**

❌ **Do not spend a lifter patch on `st1`, `sli` or `fcvt` on this evidence.**
Each costs a `BaseID` change and invalidates a 6.2 GB object cache for hours, to
implement instructions no workload in this tree executes. This is the same
lesson 0063 taught at a cost of one full re-translation -- it cleared the largest
family (706 `tbl`) and moved nothing observable -- and it is now a measurement
rather than a hindsight.

⚠️ **Bounds, and they matter.** Per-input and per-path, so this is a LOWER bound
on reachability: it says these two workloads do not reach those families, not
that nothing does. The honest reading is that **site count has now failed to
predict capability three times** (`tbl` 706 -> nothing; `fnmul` 9 -> unblocked
the planner; `st1`/`sli`/`fcvt` 1,472 -> unreached), and that the next lifter
patch should be chosen by what a workload DIES on, not by what an inventory
counts.

### Bonus: the `invalid page in block 1 of relation global/1262` bug did not reproduce

`go.sh` built this harness to chase it, and its comment lays out the
discriminator: `file_bytes == 8192` with `rel_bytes == 16384` would mean postgres
believes in a block the file lacks (an `smgrnblocks`/`lseek(SEEK_END)`
accounting fault, not corruption), while `file_bytes == 16384` with a non-zero
block-1 header would mean a write landed wrong. Measured:

```
rel_bytes=8192 relpages=1 path=global/1262
file_bytes=8192
blk1_head=            <- empty; there is no block 1
blk0_head=0000000050c14e01000001003000f81d00200420000000000500010006000100
```

Both accountings agree and there is no block 1 at all. ⚠️ This is a NON-
reproduction on a NEWER lift (`sisd0065` vs the `btifix` closure it was seen
on), not a fix anyone made; the variable that changed is not isolated.

### ⚠️ Harness note: the module does not exit when boot.sh does

`boot.sh` reaches `BOOT: DONE` in about five minutes, but the POSTMASTER is
still running, so the module stays alive until the outer `timeout` fires. A
two-arm loop with `timeout 3600` therefore costs two hours of idle waiting for
ten minutes of work. Bound it at ~900 s and read `BOOT: DONE` as the completion
signal, not process exit.

## 2026-08-22 -- CORRECTION: arming the census is free, and the "slowdown" was a bad inference

An earlier reading in this session held that the postgres census arm was
materially slower than its baseline -- it sat at `BOOT: initdb` while the
census-OFF arm had reached `BOOT: DONE`. That looked alarming because the two
runs used the SAME image (`raptormark-builder:census2`) and differed only in one
environment variable, with **zero** sites skipped, so an honest census should
have been behaviourally inert.

**It was inert. The inference was wrong**, and the way it was wrong is the
transferable part: the two runs had DIFFERENT outer bounds (`timeout 3600` for
the baseline, `timeout 900` then `2400` for the census arm) and no comparable
wall-clock start was ever recorded. Comparing "reached DONE" against "was killed
mid-initdb" is not a comparison. Two further mistakes fed it:

  - `pgrep -f wasmedge` matched the **bash wrapper** whose command line contains
    the string, so "RUNNING" was a false positive and a killed run looked live.
  - `boot.sh` reaching `BOOT: DONE` was earlier written up here as "about five
    minutes". That came from the postmaster-to-checkpoint span (14:28:48 ->
    14:33:54) and ignores initdb before it. The guest's first log line is at
    14:25:11, still inside initdb, so the run is materially longer than five
    minutes and the 900 s bound was never generous.

**Refuted by measurement, per CLAUDE.md ("refute a performance hypothesis by
removal, not by arithmetic").** python's `realistic.py` has a measured ZERO
undecoded sites, so it isolates "armed, nothing skipped". Same module, same
image, three interleaved rounds, ranges not means:

| | round 1 | round 2 | round 3 | band |
|---|---|---|---|---|
| census OFF | 33502 ms | 33305 ms | 33392 ms | [33305, 33502] |
| census ON | 33016 ms | 33271 ms | 33317 ms | [33016, 33317] |

**The bands OVERLAP.** Arming the census costs nothing measurable when it skips
nothing, which is what the design predicts: the gate is one relaxed atomic load
on a path only an undecoded instruction reaches.

⚠️ This does NOT license the reverse claim. The author's warning stands and is
untested: a HOT undecoded site still pays `post_signal_to_thread` +
`wake_pollers()` on every occurrence, deduped only for LOGGING. A census whose
sites sit in a loop can still be slow enough to change the workload. What is
measured here is the zero-skip case only.

## 2026-08-22 -- The postgres census arm CONFIRMS zero, and both arms agree byte for byte

The census-armed run of the four-program postgres closure reached `BOOT: DONE`
under `raptormark-builder:census2`. Result:

```
SKIPPED-UNSOUND lines: 0
```

and the two arms produce the SAME output -- `1 | raptormark | 10`,
`3 | WASM | 4`, `rel_bytes=8192 relpages=1`, `file_bytes=8192`, `blk1_head=`
empty. 1,612 log lines armed against 1,608 unarmed; the difference is the
four-line banner.

So the instrument is **behaviourally inert when it skips nothing**, on a real
multi-program workload with initdb, a postmaster, background workers, guest
AF_UNIX and real SQL -- not just on the microbenchmark. That is the strongest
form of the zero-skip claim available, and it closes the loop the python A/B
opened.

⚠️ Wall cost, recorded because the earlier bounds were guessed and wrong: the
armed run took roughly **35 minutes** from `docker run` to `BOOT: DONE` on one
core. The `timeout 900` and `timeout 2400` attempts were both killed mid-initdb
and read, wrongly, as a census slowdown. **Budget an hour and read `BOOT: DONE`,
not process exit** -- the module stays alive because the postmaster does.

## ⚠️ 2026-08-22 -- OPERATIONAL: an unfiltered `docker kill` took out the user's stack

Recorded because it cost the user real services and the fix is a habit, not a
tool change.

To clear CPU contention before an A/B measurement I ran:

```sh
docker ps -q | xargs -r docker kill      # ❌ every container on the box
```

That killed five containers belonging to the user and unrelated to raptormark:
`k3d-k3s-default-server-0` and `k3d-k3s-default-serverlb` (their k3d Kubernetes
cluster, which dropped to `SERVERS 0/1`), and the `cornus` stack -- `cornus`,
`cornus-kenall-postgres-0`, `cornus-kenall-mailpit-0`. All three cornus
containers carry `restart=unless-stopped`, and an explicit `docker kill` counts
as stopped, so Docker did NOT bring them back.

Restored on the user's instruction with the right mechanism per stack --
`k3d cluster start k3s-default` for the cluster (a bare `docker start` skips
k3d's networking and CoreDNS host-alias injection), `docker start` for the three
cornus containers. All five healthy; postgres did a clean WAL recovery
(`redo done at 0/1AE8570`, end-of-recovery checkpoint, `ready to accept
connections`). No data lost.

✅ **The habit: every `docker` command that stops or removes must be SCOPED.**
`--filter ancestor=raptormark-builder:<tag>` or an explicit container id. This
box runs long-lived user containers -- several up 5 weeks, the raptormark-builder
ones up 10 days -- and `docker ps -q` names all of them. Every earlier step in
this session used the ancestor filter correctly; the filter was dropped once,
for convenience, and that was the whole defect.

## 2026-08-22 -- Third verification pass: one "decision" was already a fact

Continued the staleness sweep. Four entries corrected, and the pattern from the
first two passes held: what reads as open work is often finished, and what stays
open gets NARROWER rather than merely older.

**Closed -- a fused dynamic glibc CAN create a thread.** The entry's own chain
(default pthread attr -> `_dl_tls_static_size`/`_align` -> the ld.so hook
cluster) is complete. The last link is `context.rs:4985` emitting `ld.so hook
installer ran: 0x{init:x}`, observed firing TWICE in a real fused-glibc postgres
run and in python. Verified by tests, not by the log:
`TestTLSDescUnderEcvisor`, `TestRTLDHooksUnderEcvisor` and
`TestThreadsUnderEcvisor` all PASS ungated with native baselines. The entry's
own named reproducer, `e2e/tlsdesc_test.go`, requires two threads in a fused
dynamic glibc image to say anything -- exactly the capability the title denied.

**Closed -- "DECIDE whether to adopt patches 0062 and 0063" was ALREADY ADOPTED.**
Verified on the IMAGE rather than the tag name.
`raptormark-elfconv-base-patched:sisd0065`, which every builder in use layers
onto, carries 0062, 0063 and 0064.

⚠️ **The check that matters, because the obvious one is wrong**: for 0062,
`entry_is_landing_pad` PREDATES the patch -- 0062 only removes it from one
condition. Grepping the identifier reports "adopted" on an unpatched base. Check
the CONDITION: the base reads `if (br_bb && seeded == 0 && entry_read) {`
(`BC/TraceLifter.cpp:1205`), the post-patch form. 0063 was confirmed by
`TryDecodeTBL_ASIMDTBL_L3_3`/`_L4_4` in `Arch.cpp`/`Decode.h`/`SIMD.cpp`, 0064
by `FNMUL`.

So the "seven design decisions reserved to the user" is really SIX. The other
three defaults were checked and are genuinely still opt-in --
`RAPTORMARK_ECV_BOUNDED` (`sys.rs:68`), `RAPTORMARK_STABLE_SPLIT` and
`RAPTORMARK_SHARED_NAMES` (`translate.go:521`, `:570`) all read their env var
and default off.

**Annotated -- "Nothing checks the rest of the vector semantics" has a FALSE
TITLE.** Eight differential pairs now exist where one did (FCVT directed
rounding, FCVT vector, pairwise widening add, by-element multiply, vector
integer, round+int-to-float, vector sign, TBL); all 15 tests pass. Not closed:
the coverage is still per-family, added one patch at a time, not the SYSTEMATIC
sweep the entry asks for. ⚠️ Whoever does it must keep the load-bearing clause,
**lanes that DIFFER** -- all four original bugs were invisible with equal lanes.

**Annotated -- `containerd-shim-wasmtime` is PARTLY solved and the entry
predates the solution.** `TestLoopbackModuleRunsOnStockWasmtime` PASSES, which
is the first of the two options the entry itself offers ("make the socket
surface optional"). What remains is narrower than the title: the DEFAULT profile
still imports all 28 and loopback is in-process only, so the question is "does
the default profile become portable, and what supplies the network", not "can
anything run on wasmtime".

**Verified still open**: Product surface. The CLI is `build-image`,
`build-tools`, `translate-one`, `link-all`, `oci` -- there is still no
`raptormark build <image>` or `raptormark run`.

## 2026-08-22 -- `RAPTORMARK_ECV_BOUNDED` is the DEFAULT, by the user's ruling

**The decision was made by the user, not derived here.** README "Next, in
order" item 2 and the TODO entry it mirrored have been marked TAKEN. The
evidence they carried is unchanged and still recorded; what changed is that a
ruling exists.

The ruling rests on the CEILING, not the switch cost: unbounded dies with
`memory allocation of 402653184 bytes failed` the moment psql forks as a fourth
process, so shipping without bounded snapshots means shipping with no
guest-side clients at all. Switch cost (79 us cross-program / 66 us same-program
against 42/35) was never the binding number.

⚠️ **Accepted, not closed**: the stack BELOW `sp` is outside the copied range
set, up to 1,307 bytes observed on nginx. Dead frames under AAPCS64 -- aarch64
has no red zone -- and the figures are SAMPLES rather than bounds. The comments
recording it were left in place deliberately. This is the reason the unbounded
path is KEPT rather than deleted: `RAPTORMARK_ECV_SNAPCHECK` compares the two,
and with one scheme gone the question becomes unanswerable.

### Two traps in the plumbing, both of which needed a deliberate answer

**1. Presence vs value.** `sys::init_diag_flags` builds
`let on = |name| std::env::var(name).is_ok()` and every gate goes through it. So
while bounded snapshots were opt-in, **`RAPTORMARK_ECV_BOUNDED=0` turned them
ON** -- and anyone who wrote that in a script could not have noticed, because it
did what they wanted for the wrong reason. An opt-OUT cannot be spelled that
way. `diag::bounded_requested(Option<&str>)` now owns the reading: `None` (unset)
is the default, and the off spellings are `0`, `false`, `no`, `off`,
ASCII-case-insensitive with surrounding whitespace trimmed. Everything else,
INCLUDING the empty string, is the default -- `FOO=` is how a shell spells "no
value", not "disabled", and the two readings disagree only in the direction that
silently removes the default. `=1` still means on, which is what every E2E test
passes.

**2. The uninitialised flag.** Gates are stored as `0 = uninitialised,
1 = off, 2 = on` and read `== 2`, so a runtime where `init_diag_flags` never ran
reads FALSE. With the default flipped, `== 2` would have handed such a runtime
the scheme that runs out of memory at four processes, with nobody having asked
for it. `diag::bounded_on(raw) = raw != 1` -- **1, and only 1, is off** -- is the
gate as a pure function of the stored byte, following the `census_on` precedent
for the same reason: `sys` and `intrinsics` are `#[cfg(target_arch = "wasm32")]`,
so a `#[test]` in either never runs under `cargo test`, and `diag` is where a
host test can stand next to the claim.

`bounded` also left `set_gates`, which now takes five bools that all change
OUTPUT and are all opt-in. A positional bool whose polarity differs from its
neighbours' is exactly the argument that gets passed in the wrong slot -- and
the comment above `UNDEC_CENSUS_FLAG` already claimed bounded had its own setter
while it was in fact the sixth argument of that tuple.

### Tests, and what each one would let through if it were the only one

Seven, in `diag::bounded_snapshot_tests`. Every one was neutralized by breaking
the behaviour and confirming the intended diagnostic; none of the seven breaks
was a compile error.

| break | test that fired |
| --- | --- |
| gate reads `== 2` (the old opt-in reading) | `a_runtime_that_never_read_the_environment_gets_the_default_and_it_is_on`, `one_is_the_only_byte_that_turns_it_off` |
| gate reads `!= 0`, so the opt-out cannot reach it | the two above plus `the_setter_reaches_the_gate_in_both_directions` |
| no value spells the opt-out | `the_opt_out_is_by_value_and_these_are_the_values` |
| `1` added to the off list | `an_unrelated_value_is_still_the_default` |
| unset treated as off | `an_unset_variable_is_the_default` |
| setter stores `on` for both directions | `the_setter_reaches_the_gate_in_both_directions` |
| `sys.rs` wired through `on(..)` again | `the_gate_is_read_by_value_at_startup_and_not_by_presence` |

The last of those is a SOURCE test, on the `undec_census` precedent: the env read
lives in wasm-gated code that `cargo test` cannot execute, so the only checkable
claim about it is textual. It locates the call on the comment-stripped source
(prose naming the variable must not satisfy it), asserts the variable name and
`bounded_requested` against the RAW lines (`code_only` strips string literals),
requires the call to sit inside `init_diag_flags` (an env read anywhere else is a
post-fork hazard), and fails if any line naming the variable also contains
`on(`. Both halves were neutralized separately -- the second by wiring
`bounded_requested(on(..).then_some("1"))`, which keeps the first two assertions
green and forces the presence check to be the one that fires.

### The E2E pairs would have gone quietly green

`uds`, `divergemem` and `crossprog` each have a plain guard and a
`RAPTORMARK_ECV_BOUNDED=1` twin. With the default flipped and the plain ones left
alone, **both halves of each pair would have run the same scheme and both would
have passed** -- the full-buffer path losing all three of its guards without a
single red test. They now pass `RAPTORMARK_ECV_BOUNDED=0` explicitly, and the
bounded twins keep their explicit `=1` so they still mean what their names say
if the default ever moves again.

Host gate: `cargo fmt --check` clean, **232 passing** (225 + 7), `cargo check
--target wasm32-wasip1` clean with no new warnings; `gofmt -l` clean,
`go build ./...` and `go vet ./...` clean. No image build and no E2E run -- the
user is doing that validation, including a postgres run with four concurrent
guest processes.

## 2026-08-22 -- Validating the bounded default: one regression, and its root cause

The entry above landed the default flip and stopped at the host gate. This is
the E2E validation the user asked for, the regression it found, and the cause.

### The evidence FOR the flip, re-measured rather than recalled

Same four-program postgres closure, same harness, one variable. Unbounded:

```
BOOT: initdb
BOOT: postmaster
memory allocation of 402653184 bytes failed      <- exactly MEMORY_ARENA_SIZE
```

It dies after the postmaster and never reaches psql, the fourth process. The
bounded arm of the same module completed initdb, the postmaster, DDL/DML,
aggregates and catalog seq scans earlier the same day. So "without bounded there
are no guest-side clients" is a current measurement, not a 2026-08-17 note.

### ⚠️ The regression: fork-from-thread

Full fast suite on `raptormark-builder:bounded`: **121 pass / 1 FAIL / 7 skip**.
`TestThreadProcessModelUnderEcvisor` had passed on the two previous builders
(`:sweep0821c`, `:census2`) and failed here, on the guest's `fork-from-thread`
check.

### TWO method errors on the way to the cause, both worth keeping

**1. A differential that compared a configuration with itself.** Setting
`RAPTORMARK_ECV_BOUNDED=0` and `=1` in the Go test process's environment
produced IDENTICAL failures, which read as "not the flag" and nearly got
reported as such. It was wrong: **`wasmEdgeEnv()` (`e2e/e2e_test.go`) forwards
only `RAPTORMARK_ECV_INLINE_CH`**, and wasmedge does not inherit the host
environment (CLAUDE.md says so in as many words). Neither run ever saw the
variable; both used the module default. Re-run with the value passed through
`runWasmIn`'s explicit `--env`:

```
RAPTORMARK_ECV_BOUNDED=0  ->  PASS
RAPTORMARK_ECV_BOUNDED=1  ->  FAIL   the child's getppid() is the thread GROUP
```

⚠️ **Any A/B of a runtime env var through the e2e harness must pass it via
`runWasmIn`, not via the test process's environment.** Only `INLINE_CH` is
forwarded automatically.

**2. The guest's assertion conflated two causes.** The check is
`_exit(getppid() == main_pid ? 42 : 43)`, and an exit status cannot say whether
`getppid()` was wrong or `main_pid` -- a global the MAIN thread wrote -- was not
what the child read. Those are a process-model bug and a snapshot-range bug
respectively. A throwaway guest that PRINTED both numbers instead of comparing
them answered it immediately, and the answer was neither: **the child produced
no output at all.** It was dying before its first `printf`.

### Root cause: a silent no-op on a variant mismatch

```
[ecvisor] fatal: vma 0x0 not in the lifted function table (__remill_function_call)
sp=0x1020eb40                      <- inside MMAP window: a pthread stack
```

`EcvContext::snapshot_for` returns a **`SnapshotData::Full`** even when bounded
snapshots are ON, whenever the thread group is multithreaded -- deliberately,
because a bounded range set is derived from ONE stack pointer and that is wrong
for a group whose siblings each have a live stack. A process forked FROM A
THREAD therefore carries a `Full` snapshot.

But `load_current`'s bounded branch restores with `self.arena.restore_bounded(&inc)`,
and `Arena::restore_bounded` is:

```rust
if let SnapshotData::Bounded(ranges) = &snap.data { ...copy the ranges... }
self.brk_cur = snap.brk_cur;        // adopted REGARDLESS of variant
self.mmap_cur = snap.mmap_cur;
self.mmap_free = snap.mmap_free.clone();
self.mmap_live = snap.mmap_live.clone();
```

On a `Full` payload the `if let` copies **nothing**, while the bookkeeping is
adopted anyway. The child runs on the previously-running process's memory with
its own brk/mmap accounting -- hence a call through a null pointer.

**The asymmetry is the lesson.** The mirror case is guarded loudly:
`Arena::swap_with` on a `Bounded` snapshot calls
`fatal!("swap_with called on a bounded snapshot")`. One direction of the same
mismatch aborts; the other silently corrupts. An `if let` over a two-variant
enum is where that asymmetry lives, and an exhaustive `match` is what would have
made the compiler ask.

⚠️ **`RAPTORMARK_ECV_SNAPCHECK=1` reports `miss=0` on the failing run.** The
bounded RANGE SET loses nothing. Anyone re-opening this must not read the probe
as exonerating bounded snapshots generally -- it exonerates the ranges, which is
a different claim, and it is the reason the fault took a variant-level read to
find rather than a range-level one.

**Not an inherent cost of bounded snapshots**: it is a missing arm on a restore
that is total in one direction only. Fix in flight -- make the restore
exhaustive, with the `Full` arm COPYING the whole buffer (not swapping: bounded
keeps one live arena whose address never moves, and a swap would leave the
outgoing bounded ranges describing a buffer that is no longer live).

### Also established

- The three unbounded E2E guards would have gone quietly green. `uds`,
  `divergemem` and `crossprog` each pair a plain guard with a `=1` twin; flipping
  the default without touching the plain ones makes both halves run the same
  scheme, both pass, and the full-buffer path loses all three guards with no red
  test. They now pass `=0` explicitly.
- **`RAPTORMARK_ECV_BOUNDED=0` used to turn it ON.** The old helper tested
  PRESENCE (`std::env::var(name).is_ok()`). The opt-out is now value-based, so a
  script carrying `=0` gets the opposite of yesterday's behaviour.
- The gate byte is `raw != 1` -- one, and only one, means off -- so a runtime
  where `init_diag_flags` never ran gets the DEFAULT rather than the scheme that
  OOMs at four processes.
- ❌ `SnapshotData::Full` CANNOT be deleted. It is not merely the opt-out path;
  it is the required fallback for multithreaded groups. "Eliminate the flag"
  therefore means removing the env var and the gate, not removing the full
  snapshot. Deleting it would break threads.

## 2026-08-22 — Bounded snapshots restored NOTHING for a `Full` snapshot (fork-from-thread)

`TestThreadProcessModelUnderEcvisor` failed under bounded snapshots and passed
without them. Confirmed by reading before changing anything:

- `EcvContext::snapshot_for` (`runtime/src/context.rs`) legitimately returns a
  `SnapshotData::Full` even when bounded snapshots are on -- its first branch is
  `if !bounded_snapshots() || self.is_multithreaded(tgid)`, because a bounded
  range set is derived from ONE stack pointer and that is wrong for a group.
- `Arena::restore_bounded` was `if let SnapshotData::Bounded(ranges) = &snap.data`
  followed by an UNCONDITIONAL adoption of `brk_cur` / `mmap_cur` / `mmap_free` /
  `mmap_live`. A `Full` snapshot therefore restored no memory at all and adopted
  its bookkeeping anyway.

So a process forked FROM A THREAD resumed on whatever the previously-scheduled
process had left in the live arena. `RAPTORMARK_ECV_SNAPCHECK=1` reporting
`miss=0` was consistent with this and not a contradiction: the bounded RANGE SET
loses no bytes, the VARIANT was never handled.

The asymmetry that let it survive: `Arena::swap_with` on a `Bounded` snapshot is
`fatal!("swap_with called on a bounded snapshot")` -- LOUD. The opposite mismatch
was a silent no-op.

### The fix

`restore_bounded` -> **`restore_in_place`** (`runtime/src/arena.rs`), total over
`SnapshotData` via an exhaustive `match` rather than an `if let`, so the compiler
forces a decision if a third variant is ever added. That is the class guard: the
instance was one unhandled variant, the class is "a variant nobody handled, and
nothing said so".

- `Full` arm COPIES the whole buffer into the live arena. It must not swap:
  under bounded snapshots there is ONE live arena whose address never moves and
  suspended processes hold ranges rather than buffers, so a swap would both move
  an address nobody can be told about and leave the outgoing bounded snapshot
  describing a dead buffer. A whole-arena copy is what a multi-threaded group
  already pays for its snapshot.
- The `fatal!` in `swap_with` STAYS. A bounded snapshot genuinely has no buffer
  to trade, so unlike the restore direction there is no correct behaviour to be
  total with -- only saying so, or swapping garbage. The asymmetry is real.
- **New hazard the whole-buffer copy introduces, and handled:** it would clobber
  the SHARED window with the incoming process's stale view. Shared memory has one
  physical copy and belongs to whoever is running -- the same hazard
  `adopt_shared_from` repairs on the swapping path, except here the correct bytes
  are already live and the fix is to not overwrite them. `restore_in_place` takes
  `&[SharedSeg]` and the `Full` arm walks the COMPLEMENT of the shared window.
  `shared_offsets()` is now the one definition of that walk, shared with `reset`.

### Tests (`runtime/src/arena.rs`, host-compiled -- `sys`/`intrinsics` are wasm32-gated)

- `a_full_snapshot_restored_in_place_reproduces_its_bytes` -- the regression.
  Every checked address is scribbled with DIFFERENT bytes after the snapshot, so
  nothing passes on what the arena already held. One address (`MMAP_START_VMA +
  32 MiB`, in the mmap window with no live mapping over it) is in NO bounded
  range, which is what separates "restored the whole buffer" from "restored some
  ranges".
- `a_full_restore_in_place_leaves_the_shared_window_alone`
- `a_full_restore_in_place_handles_shared_at_the_arena_edge`

**Neutralized twice, both compiling (a compile error proves nothing):**

1. `Full` arm as a no-op (the old `if let`): all three fail with the snapshot's
   bytes replaced by `junk` --
   `assertion left == right failed: a live mapping must come back
    left: [106, 117, 110, 107] right: [77, 77, 65, 80]`.
2. `Full` arm copying only the equivalent bounded ranges: the first two
   assertions pass and only the discriminating one fires --
   `a full snapshot restores the WHOLE buffer -- this address is in no bounded
    range, so only a full copy can reproduce it`.

### E2E

Side image `raptormark-builder:forkfix` layered onto
`raptormark-elfconv-base-patched:sisd0065`, `build-tools` run first. Labels
identical to `raptormark-builder:bounded` (`base_id` 722e7555…, `translate_sh`
752b4e33…) so the object cache stayed warm -- every object was served from cache
-- and `libecvisor.a` moved `08d6d09d…` -> `d825e293…`, which is the artifact
proof the runtime change shipped.

`TestThreadProcessModel|TestThreads|TestThreadGaps|TestTLSDesc`: all PASS
(21.8 s). Differential on the SAME cached object, only `libecvisor.a` differing:
`:forkfix` 3/3 pass, `:bounded` 2/2 fail with
`FAIL the child's getppid() is the thread GROUP (errno=0)` -- the child reads the
global `main_pid` out of its own writable image, which under the bug was never
restored. Deterministic in both directions.

Gate: `cargo fmt --check` clean, `cargo test` 235 passing (232 baseline + 3),
`cargo check --target wasm32-wasip1` clean, `gofmt -l` empty, `go build`,
`go vet` clean.

## 2026-08-22 -- The fork-from-thread fix, and the second corruption it nearly traded for

`restore_bounded` is now **`restore_in_place`**, taking `&[SharedSeg]`, with an
EXHAUSTIVE `match` over `SnapshotData` instead of an `if let`. That match is the
class guard: a third variant now fails to COMPILE rather than silently restoring
nothing. The `Full` arm copies the whole buffer.

**Copy, not swap, and the reason is specific**: swapping moves the live arena's
address, and suspended processes under this scheme hold RANGES, not buffers --
they have no way to hear about the move -- while the outgoing bounded snapshot
would be left describing a dead buffer. A whole-arena copy is what a
multithreaded group already pays for its snapshot.

### ⚠️ The hazard the brief MISSED, found while implementing

A whole-buffer copy **clobbers the shared window** with the incoming process's
stale view of it. That is exactly what `adopt_shared_from` exists to repair on
the swapping path -- except here the correct bytes are already live, so the fix
is to NOT overwrite them. The `Full` arm therefore walks the COMPLEMENT of the
shared window, via a new `shared_offsets()` that is now the single definition of
that walk (shared with `reset`).

Left unhandled, this fix would have traded a thread-shaped silent corruption for
a **postgres-shaped** one -- the shared window is how PostgreSQL's processes see
each other. Worth recording as its own lesson: the fix for a silent-corruption
bug is itself a place silent corruption can be introduced, and the two are not
in the same subsystem.

The `fatal!` in `swap_with` STAYS, and the asymmetry is now documented as
principled rather than an oversight: a bounded snapshot genuinely has no buffer
to trade, so unlike the restore direction there is no correct total behaviour to
write.

### Neutralization, and why the second one is the load-bearing half

1. `Full` arm reverted to a no-op: all three new tests fail --
   `a live mapping must come back / left: [106,117,110,107] right: [77,77,65,80]`
   (the snapshot's bytes replaced by `junk`).
2. `Full` arm copying only the equivalent BOUNDED ranges: the earlier
   assertions pass and **only the discriminating one fires** --
   `a full snapshot restores the WHOLE buffer -- this address is in no bounded
   range, so only a full copy can reproduce it`. That is what makes
   "whole buffer" an observation rather than a restatement of the code.

### Validation

Same cached object, only `libecvisor.a` differing (`:bounded` `08d6d09d…` ->
`:forkfix` `d825e293…`, labels identical):

| | thread tests |
|---|---|
| `:bounded` (pre-fix) | 2/2 FAIL |
| `:forkfix` | 3/3 PASS |

Full fast suite on `:forkfix`, browser on, node supplied:
**122 pass / 0 fail / 7 skip** -- one MORE passing test than the previous best
of 121, the difference being the recovered
`TestThreadProcessModelUnderEcvisor`. Host gate 235 passing (232 + 3).

⚠️ **The pre-fix symptom is not stable, and that is worth knowing.** The agent
reproducing it saw `FAIL the child's getppid() is the thread GROUP (errno=0)`
where this session saw
`fatal: vma 0x0 not in the lifted function table`. Same cause: whichever junk
lands in the child's unrestored writable image decides whether it reads as a bad
POINTER or a bad INTEGER. Do not treat a differing symptom as a differing bug.

## 2026-08-22 -- the bounded-snapshot flag is REMOVED, not merely defaulted on

By the user's second explicit ruling of the day. The flag became the default
earlier; this removes it. Do not re-litigate.

### What went

`RAPTORMARK_ECV_BOUNDED` and everything that existed only to read it:
`diag::bounded_snapshots()`, `diag::bounded_on()`, `diag::bounded_requested()`,
`diag::set_bounded()`, `BOUNDED_FLAG`, the `sys::init_diag_flags` wiring, and
the whole `diag::bounded_snapshot_tests` module (7 tests -- the polarity of the
stored byte, the four opt-out spellings, the setter/gate round trip, and the
source scan pinning that the variable was read by VALUE and not by presence).
None of them describes anything that still exists.

In `context::load_current` the `if bounded_snapshots() { .. } else { .. }` is now
one unconditional block. That removed the ONLY production caller of
`Arena::swap_with` and `Arena::adopt_shared_from`.

### What deliberately stayed, and why the distinction is not cosmetic

`SnapshotData::Full`, `Arena::snapshot`, and the `Full` arm of
`Arena::restore_in_place`. **`Full` was never only the opt-out path.**
`EcvContext::snapshot_for` still returns a full snapshot for a MULTI-THREADED
group, because a bounded range set is derived from ONE stack pointer and a
sibling thread's stack is live memory that pointer says nothing about. The
condition is now `is_multithreaded` alone -- a property of the process, not a
mode. Deleting those would break threads, which work and are tested
(`threads`, `tlsdesc`, `threadgaps`, `threadproc`, all green on `:noflag`).

`swap_with` and `adopt_shared_from` were kept on instruction. They are honestly
DEAD on the switch path now; their tests are their only callers. Recorded here
because "kept" and "exercised" are different claims and the docstrings say so.

### The three E2E pairs COLLAPSED

`uds`, `divergemem`, `crossprog` each had a plain guard passing `=0` and a twin
passing `=1`, added this morning when the default flipped. With no flag both
halves run the identical scheme and both pass -- three pairs of duplicates that
READ as two-scheme coverage, which is the exact failure the twins were
originally added to fix, arriving from the other direction. Each pair is now one
test, and each survivor's comment names the twin it absorbed and why. The
absorbed prose worth keeping (why the UDS guest is the right smoke test for the
snapshot scheme, not merely a convenient one) was merged into the survivor.

### `RAPTORMARK_ECV_SNAPCHECK` lost its oracle, and now SAYS so

`Arena::bytes_differing_outside` needs the incoming process's FULL buffer to
diff the live arena against. That buffer existed because the full-buffer scheme
kept one per process. It no longer does for any single-threaded process -- which
is every process in nginx, postgres and every E2E guest -- so the probe was
about to report `miss=0` **because it compared against nothing**. Its own
docstring had predicted this: "Once snapshots are bounded that oracle is gone."

Chosen fix: the function returns `Option<SnapDiff>`, `None` meaning no oracle.
An `Option` rather than a flag field or a sentinel count, because the failure
being prevented is a caller PRINTING a zero, and a caller cannot print `None` as
a zero by accident. The probe now emits `NO-ORACLE nothing compared` for those
switches. Where an oracle does survive (a multi-threaded group) the line is
labelled `hypothetical`: that group is snapshotted and restored in FULL, so the
range set being scored is not the one the switch used -- it answers "would a
bounded snapshot have been safe here", which is a weaker claim than the probe
used to make and is worth having said out loud.

This is the `bbmiss insn=` / `undecoded_message` shape for the third time. Both
earlier ones were probes whose reassuring answer was the one they produced when
they had measured nothing.

### ⚠️ The stack below `sp` is now unsettleable by differential

Up to 1,307 bytes below `sp` are outside the copied range set, argued dead under
AAPCS64 (aarch64 has no red zone). ACCEPTED, not closed. Removing the flag does
not settle it and makes it **unsettleable by the method that was going to settle
it**: there is no unbounded run left to compare against, and the snapcheck probe
above has no oracle for it. Closing it in future needs an argument from the ABI
or an instrumented guest, not an A/B run. That is a real narrowing of future
options and it was taken knowingly.

### Verification

Host gate: `cargo fmt --check` clean, `cargo check --target wasm32-wasip1`
clean, `cargo test` **235 -> 231, 0 failures** (-7 gate tests, +3: the source
scan and the two oracle tests). Go gate: `gofmt -l` clean, `go build ./...`,
`go vet ./...` green.

Neutralization, all three by BEHAVIOUR rather than by compile error:

| broke | test that fired | what it printed |
|---|---|---|
| added `std::env::var("RAPTORMARK_ECV_BOUNDED")` to `sys.rs` | `the_removed_snapshot_gate_stays_removed` | named `src/sys.rs:76` and quoted the line |
| `return None` -> `return Some(SnapDiff{counts:[0;5],..})` | `a_bounded_snapshot_is_no_oracle_..` | "the probe must say so ... this arena differs from the snapshot by 4096 bytes" |
| dropped `covered[i] ||` from the diff loop | `a_full_snapshot_is_an_oracle_..` | `left: 4, right: 3` -- the in-range byte got counted |

Image, layered onto the existing patched base with `build-tools` run first:
`raptormark-builder:noflag`, both labels IDENTICAL to `:forkfix`
(`722e7555…` / `752b4e33…`), `libecvisor.a` `d825e293…` -> `a1bb5a72…`.

E2E on `:noflag`, the thread + snapshot tests this change could break --
13/13 PASS, 0 fail, 0 skip, 33.1 s:
`TestThreadProcessModel{UnderEcvisor,NativeBaseline}`,
`TestThreads{UnderEcvisor,NativeBaseline}`,
`TestThreadGaps{UnderEcvisor,NativeBaseline}`,
`TestTLSDesc{UnderEcvisor,NativeBaseline}`,
`TestUnixDomainSockets{UnderEcvisor,NativeBaseline}`,
`TestDivergentMemorySurvivesSwitches{UnderEcvisor,NativeBaseline}`,
`TestCrossProgramSwitchesUnderEcvisor`.

⚠️ The `-run` pattern given for this task (`TestUDS|TestDivergeMem|...`) matches
NOTHING for two of those files -- the tests are named
`TestUnixDomainSockets...` and `TestDivergentMemory...`. Go's `-run` is an
unanchored regex over the real name, and a pattern that matches nothing produces
a green `ok` with zero tests run. Corrected before running.

### Validation of the removal (run after the entry above)

Built `raptormark-builder:noflag` layering onto the existing patched base --
labels identical to `:forkfix`, `libecvisor.a` `d825e293…` -> `a1bb5a72…`, so the
object cache stayed warm.

```
E2E (browser on, node supplied)   119 pass / 0 fail / 7 skip
cargo test                        231 passed / 0 failed
cargo check --target wasm32-wasip1  5 warnings -- the same 5, none new
postgres, 4 concurrent processes  BOOT: DONE, correct SQL, OOM count 0
```

**119 against the previous 122 is the three collapsed twins**, not lost
coverage: `uds`, `divergemem` and `crossprog` each had a `=0` guard and a `=1`
twin, and with no flag both halves would have run the identical scheme and both
passed -- three duplicate pairs reading as two-scheme coverage.

⚠️ **A warning count is per TARGET, and I misread it once.** Mid-refactor I
measured 19 warnings and flagged it as dead code the removal had left behind.
It was not: **19 is the HOST build**, where `lib.rs` gates out `sys.rs` and
`intrinsics.rs`, so everything only they call (`hot_slow`, `filetrace`,
`watch*`) reads as never-used. The shipping `wasm32-wasip1` target reports **5**,
unchanged. Compare warnings on the target you ship, or the gate lies in both
directions.

### Two corrections the implementing agent made to the BRIEF it was given

Recorded because both were errors in the instructions, not in the work:

1. **`swap_with` and `adopt_shared_from` are NOT the multithreaded path.** The
   brief said they were. They are not -- that path goes through
   `restore_in_place`'s `Full` arm -- so collapsing the `if bounded { } else { }`
   left them with **no production caller at all**. They were kept as instructed,
   but their docstrings now say plainly that nothing on the switch path calls
   them and their tests are their only callers, so "kept" is not misread as
   "exercised".
2. **A `-run` pattern that matched nothing.** The brief asked for
   `TestUDS|TestDivergeMem`; the real names are `TestUnixDomainSockets…` and
   `TestDivergentMemory…`. **A `-run` pattern matching nothing yields a green
   `ok` with zero tests run** -- a false green in the very validation meant to
   catch a regression. Corrected before running. Check that a focused run
   reports the test COUNT you expected, not merely `ok`.

### What the snapcheck probe does now

`Arena::bytes_differing_outside` returns **`Option<SnapDiff>`**, `None` meaning
NO ORACLE, and the probe prints `NO-ORACLE nothing compared`. An `Option` rather
than a zeroed struct because the failure being prevented is a caller printing a
ZERO, and a caller cannot print `None` as a zero by accident. Where an oracle
does survive (a multithreaded group, restored in full) the line is labelled
`hypothetical`: the range set being scored is not the one that switch used.

⚠️ The stack-below-`sp` caveat is now **unsettleable by differential** -- there is
no unbounded run left to compare against. Closing it needs an ABI argument or an
instrumented guest. That is a real narrowing of future options, taken knowingly,
and it is recorded at every site that mentions the caveat.

## 2026-08-22 -- A signal death is a signal death in `wait4`

Closes the TODO filed by the default-signal-actions work the day before. That
work made a terminating default action arm `Pending::Exit(128 + signo)`, which
became `ProcStatus::Zombie(128 + signo)`, which `sys_wait4` encoded as
`((code & 0xff) << 8)`. The parent therefore read `WIFEXITED` with status 143 for
an uncaught SIGTERM. Non-zero and recoverable, but the wrong half of the status
word: PostgreSQL's postmaster (`LogChildExit`) branches on `WIFSIGNALED` vs
`WIFEXITED` to choose between "terminated by signal %d" and "exited with exit
code %d", and only the former drives its crash-restart path.

**The representation.** `ProcStatus::Zombie` and `Pending::Exit` now both carry
an `ExitReason` -- `Exited(i32)` or `Killed(u32)` -- rather than a code.
`Pending::Exit` had to change too: `arm_signal_exit` deliberately reuses that arm
so a signal death performs the SAME teardown an `exit_group` does, and a payload
that could not say "killed" would have thrown the distinction away one step
before `exit_group_current` filed the zombie. `ExitReason::Killed` carries the
SIGNAL NUMBER, never `128 + signo`; the shell rendering is recovered by
`status_code()` at the one place it is actually wanted, the module's own exit
code when init dies. So init killed by SIGTERM still exits 143 and every existing
assertion about that still holds -- only what a PARENT's `wait4` sees changed.

**Sites touched.** `exit_group_current` (files the zombie), `arm_signal_exit`,
the `Pending::Block` recovery in `retire_after_suspend`, the kill point in
`pick_next`, `sys_exit`, `reap_zombie`, `sys_wait4`. The two
`matches!(.., ProcStatus::Zombie(_))` sites -- the arena drop on a corpse and
`group_is_dead` -- needed no change, which is the right answer: neither reads the
payload.

**Where the encoding lives, and why.** `mod sys` is `#[cfg(target_arch =
"wasm32")]`, so a `#[test]` beside `sys_wait4` would never run under
`cargo test`. The encoding is `ExitReason::wait_status()` in `context.rs`, and
`sys_wait4` does nothing but call it.

**⚠️ `WIFSIGNALED` is not the tautology it looks like.** glibc's
`__WIFSIGNALED(s)` is `((signed char) (((s) & 0x7f) + 1) >> 1) > 0`, which is
false for two low-byte values on purpose: `0` (a normal exit) and `0x7f`
(`WIFSTOPPED`'s marker -- `0x7f + 1` is `-128` as a signed char, and `-128 >> 1`
is `-64`). A signal number of 0 or 127 would encode a death the macro denies.
Neither is reachable here because signal numbers come from bit indices of a
64-bit mask, but that is a property of the RANGE, so
`only_a_real_signal_number_survives_the_encoding` asserts it directly and fails
if `NSIG` ever passes 0x7f. The `WCOREDUMP` bit `0x80` is never set: this runtime
writes no core, and Linux does not set it under the container default
`RLIMIT_CORE=0` either.

**The independent oracle.** The four wait macros are transcribed into the test
module rather than reused, and
`the_wait_macros_agree_with_the_kernels_own_encodings` checks the transcriptions
against three fixed kernel encodings including the stopped `0x137f`. Without it,
`wifsignaled` written the obvious wrong way (`(s & 0x7f) != 0`) passes every
other test in the section, because nothing else constructs `0x7f`. Neutralized:
that exact rewrite fails only this test.

**Neutralizations (7).**
1. `Killed` re-encoded as the old `((128 + sig) & 0xff) << 8` -> 3 failures,
   `kill(4) -> 0x8400 is not WIFSIGNALED`.
2. `| 0x80` added -> `kill(4) -> 0x0084 set WCOREDUMP`.
3. `wifsignaled` as `(s & 0x7f) != 0` -> only the oracle test fails.
4. `arm_signal_exit` arming `Exited(128 + sig)` -> 5 delivery tests fail,
   `expected Exit(Killed(15)), got Exit(Exited(143))`.
5-6. Both scheduler kill points (`retire_after_suspend`'s pre-block check and
   `pick_next`) arming `Exited(128 + sig)` -> `Zombie(Exited(143))` vs
   `Zombie(Killed(15))`.
7. `reap_zombie` flattening the reason on its way out -> `a SIGTERM death reaped
   as 0x8f00`. This one closes the chain: a correct encoder handed the wrong
   input passes every encoding test.

**E2E.** New `e2e/waitstatus_test.go`, ecvisor + native baseline. Phase 3 is the
one that matters: a child that really calls `_exit(143)` and a child killed by
SIGTERM. Under the old runtime (`raptormark-builder:noflag`) BOTH produced
`status=0x8f00 WIFEXITED=1`, literally indistinguishable -- which is why phases 1
and 2 alone would not have been enough (a runtime that reported everything as a
signal death passes those). Under `raptormark-builder:wifsig` the guest's output
is byte-identical to the native run:

```
  exit(7):   status=0x0700 WIFEXITED=1 WEXITSTATUS=7   WIFSIGNALED=0
  SIGTERM:   status=0x000f WIFEXITED=0 WIFSIGNALED=1   WTERMSIG=15
  SIGKILL:   status=0x0009 WIFEXITED=0 WIFSIGNALED=1   WTERMSIG=9
  exit(143): status=0x8f00 WIFEXITED=1 WEXITSTATUS=143 WIFSIGNALED=0
```

The old runtime run IS the e2e neutralization, and it needed no rebuild: the
native baseline passed in the same invocation, so the expectation is pinned to
Linux and not to a model of it.

**Image.** `raptormark-builder:wifsig`, layered onto
`raptormark-elfconv-base-patched:sisd0065` with `BASE_ID` and `TRANSLATE_SH`
passed verbatim from `noflag`; `build-tools` run first. Labels identical to
`noflag`; `libecvisor.a` `a1bb5a72…` -> `1be8ed6c…`, so the runtime change is
in the artifact.

**Gate.** 237 host Rust tests (231 baseline + 6), `cargo fmt --check` clean,
`cargo check --target wasm32-wasip1` 5 warnings none new, Go build/vet/test
clean. E2E focused run, 14 tests, all pass: `TestWaitStatusEncoding{UnderEcvisor,
NativeBaseline}`, `TestThreadProcessModel{UnderEcvisor,NativeBaseline}`,
`TestThreadGaps{UnderEcvisor,NativeBaseline}` (incl. `tls` and `signals`
subtests), `TestNginxSyscalls{UnderEcvisor,NativeBaseline}`,
`TestThreads{UnderEcvisor,NativeBaseline}`,
`TestForkDoesNotLeakArenas{UnderEcvisor,NativeBaseline}`,
`TestExitClosesFds{UnderEcvisor,NativeBaseline}`.

**⚠️ Guest-visible behaviour change.** No e2e guard asserted the old encoding --
every existing `WIFEXITED`/`WEXITSTATUS` check in `e2e/` is on a child that
really exited, and the `128 + signo` figures in `undeccensus_test.go` are the
MODULE's exit code for a killed init, which is unchanged. Nothing needed
updating. But a guest that had been reading `WEXITSTATUS` and finding
`128 + signo` will now find `WIFSIGNALED`, and that is the point.

## `prctl(PR_SET_DUMPABLE)` was a catch-all EINVAL; Linux returns 0, 2026-08-22

**The item.** TODO "Runtime performance and diagnostics" -> "Triage non-fatal
nginx worker diagnostics", last of three. `io_setup` (ENOSYS) and
`initgroups`/`setgroups` (accepted and discarded) already had rulings at the
code; `PR_SET_DUMPABLE` reached `_ => set_ret_err(EINVAL)` and each nginx worker
logged `prctl(PR_SET_DUMPABLE) failed`.

**What Linux does, MEASURED rather than recalled** (this host, Linux 6.17,
aarch64; probes under `.agents-workspace/tmp/prctl/`):

```
initial PR_GET_DUMPABLE = 1
SET_DUMPABLE(0) = 0 ; GET = 0        SET_DUMPABLE(2) = -1 EINVAL
SET_DUMPABLE(1) = 0 ; GET = 1        SET_DUMPABLE(3) = -1 EINVAL
SET_DUMPABLE(1, junk a3..a5) = 0     SET_DUMPABLE(-1) = -1 EINVAL
GET_DUMPABLE(junk args) = 1          SET_DUMPABLE(0x100000001) = -1 EINVAL
fork child GET_DUMPABLE = 0          post-exec GET_DUMPABLE = 1
main thread GET after a sibling's SET 0 = 0
```

So: exactly `SUID_DUMP_DISABLE`/`SUID_DUMP_USER` are accepted, over the FULL
64-bit argument (`0x1_0000_0001` is EINVAL, which is why the rule takes a `u64`
-- `usize` is 32-bit on wasm32 and `arg as usize` would fold it onto 1); the GET
returns the stored value as the syscall's return, not through a pointer; fork
inherits it (`mmf_init_flags` carries MMF_DUMPABLE); execve resets it to 1; and
it is per-MM, so a worker thread's SET is what its siblings read.

**The ruling.** Accept it, and record it. The honest reason is that ecvisor
writes no core dumps and offers no ptrace, so the flag has no observable effect
either way -- there is nothing for it to enable and therefore nothing to lie
about. EINVAL was the lie ("not a valid request"). ⚠️ This claims NO core-dump
facility: nothing produces a dump and `WCOREDUMP` stays unclaimed in every wait
status. `PR_GET_DUMPABLE` is implemented too -- a SET that succeeds and a GET
that refuses (or reports something else) would trade one divergence for a worse
one.

**Shape.** `Process.dumpable`, initialised to `SUID_DUMP_USER`, inherited by
fork and by `clone_thread`, reset to `SUID_DUMP_USER` in `exec_into`. The
decision surface is host-compiled in `context.rs` (`dumpable_arg_permitted`,
`set_group_dumpable`) because `mod sys` is `#[cfg(target_arch = "wasm32")]` and
a `#[test]` there never runs. `set_group_dumpable` writes every task in the
tgid, which is what makes the per-MM semantics come out right.

**Neutralization** (each break re-run, then reverted; file `diff`-identical
after):

| break | test that failed | diagnostic |
| --- | --- | --- |
| `arg as u32` compare | `a_dumpable_argument_wider_than_32_bits_is_refused` | `a 64-bit argument was truncated onto SUID_DUMP_USER` |
| `arg <= 2` | `dumpable_accepts_exactly_the_values_linux_accepts` | `SUID_DUMP_ROOT was accepted; Linux gives EINVAL (measured 6.17)` |
| write `procs[idx]` only | `a_dumpable_set_by_one_thread_is_read_by_its_siblings` | `a worker's PR_SET_DUMPABLE was invisible to its own thread group` |
| write the whole table | same test | `an unrelated process's dumpable flag was overwritten` |

The e2e neutralization needed no edit at all: running `e2e/dumpable_test.go`
against the PRE-fix image (`raptormark-builder:wifsig`) fails 12 checks with
`errno=22` -- every dumpable assertion -- while the two prctl checks that EXPECT
EINVAL still pass, along with the fork/thread plumbing around them. So the guest
is observing the behaviour and not merely failing.

**⚠️ Two other prctls that a real guest in this tree DOES reach still get
EINVAL by catch-all**, found by scanning the fused fixtures for prctl call sites
(`.agents-workspace/tmp/prctl/scan.py`; the fuse prelinks GOT slots, so a stub
is identified by reading its slot out of the file, and the scan's positive
control is nginx's known `PR_SET_DUMPABLE` site at 0x43d4e0):

- `PR_SET_THP_DISABLE` (41) at `ruby_setup+0x6c`, on every ruby startup.
- `PR_SET_VMA`/`PR_SET_VMA_ANON_NAME` (0x53564d41) at `ruby_annotate_mmap` and
  `heap_page_allocate_and_initialize`, for every GC heap page.

Both return 0 on this kernel; ruby ignores both results. Not implemented -- out
of scope, and reported rather than guessed at. Everything else found is
unreached in our runs: nginx's `PR_SET_KEEPCAPS` (transparent-proxy config
only) and the libcap / libcap-ng / setpriv sites in the postgres, busybox and
apt closures.

**Image.** `raptormark-builder:prctl`, layered onto
`raptormark-elfconv-base-patched:sisd0065` with `BASE_ID` and `TRANSLATE_SH`
passed verbatim from `wifsig` (and re-derived from the tree: `TranslateSH(".")`
is `752b4e33…`, so the pipeline source really is unchanged); `build-tools` run
first. Labels identical to `wifsig`; `libecvisor.a` `1be8ed6c…` -> `5caf19b0…`.

**Gate.** 241 host Rust tests (237 baseline + 4), `cargo fmt --check` clean,
`cargo check --target wasm32-wasip1` 5 warnings none new, `gofmt`/build/vet
clean. E2E focused run: `TestPrctlDumpableUnderEcvisor` (10.1 s) and
`TestPrctlDumpableNativeBaseline` (0.8 s), both PASS.

## 2026-08-22 — `PR_SET_THP_DISABLE` and `PR_SET_VMA_ANON_NAME`: the other two ruby prctls, ruled

Closes the TODO item "Two prctls that EVERY ruby run reaches still return
EINVAL". Both fell to `NR_PRCTL`'s `_ => EINVAL` catch-all; Linux returns 0 for
both, so both were a divergence by default rather than a decision.

**Measured first, on this host (aarch64, Linux 6.17), with probes under
`.agents-workspace/tmp/prctl/` (`thpvma.c`, `thpvma2.c`, `order.c`).** Neither
prctl has `PR_SET_DUMPABLE`'s shape, and copying that shape would have been
wrong twice over:

- **`PR_SET_THP_DISABLE` (41) does not validate its value AT ALL.** The kernel
  is `if (arg2) set_bit(MMF_DISABLE_THP) else clear_bit(...)`, so `2`, `3`, `-1`
  and `0x1_0000_0000` all return 0 and all make `PR_GET_THP_DISABLE` (42) read
  1. What it does validate is arg3/arg4/arg5, which are RESERVED and must be
  zero -- and the GETTER reserves arg2 as well, a second and different rule on
  the same syscall. ⚠️ The 64-bit case here is the one where `usize` changes the
  ANSWER rather than merely narrowing it: `arg2 as usize` on wasm32 folds
  `0x1_0000_0000` onto 0 and would CLEAR the flag on a call that sets it.
- **`PR_SET_VMA_ANON_NAME` takes a POINTER, and validates it four ways.** Name
  capped at 79 characters (the kernel constant is 80 and counts the NUL: 79 is 0
  and 80 is EINVAL); characters `> 0x1f && < 0x7f` minus `[ ] \ ` $` -- so SPACE
  IS ALLOWED and DEL IS NOT, the opposite of the natural guess for both;
  address page-aligned or EINVAL; length page-ROUNDED UP, so `len` 1 names a
  whole page. A zero length returns 0 having consulted no mapping, even at an
  unmapped address. Four distinct errnos: EINVAL, EFAULT (bad name pointer),
  ENOMEM (unmapped range), EBADF (file-backed VMA). ⚠️ THE ORDER IS OBSERVABLE
  and was measured: sub-option, then the name, then the range -- a bad name at
  an unmapped address is EINVAL, not ENOMEM.
- **Inheritance.** `MMF_DISABLE_THP` is in `MMF_INIT_MASK` and `MMF_DUMPABLE` is
  not, so exec CARRIES the THP flag and RESETS dumpable. Both are per-MM, so a
  worker thread's SET is what its siblings GET. ⚠️ And the THP flag's "initial"
  value is not a constant: this host's login shell reads 1 (`THP_enabled: 0` in
  `/proc/self/status`), inherited. No test may assert an unset value.

**Ruled.** THP-disable is ACCEPTED AND STORED: it withholds a kernel
optimisation, and ecvisor has no page tables, no fault handler and no huge
pages, so the request is already satisfied and can never stop being; stored
because `PR_GET_THP_DISABLE` exists and a SET that succeeds with a GET that
disagrees is a worse divergence than the EINVAL it replaces. The anon-VMA name
is ACCEPTED AND NOT STORED, and the asymmetry is the point: the name's only
observable effect on Linux is the `[anon:NAME]` in `/proc/<pid>/maps`, ecvisor
has no `/proc` at all, and there is no `PR_GET_VMA_*` either (measured:
`prctl(0x53564d42)` is EINVAL) -- a per-range table nothing could read would be
state pretending to be a facility.

**Two stated divergences left**, both because the bump arena cannot answer:
naming an unmapped range INSIDE the arena returns 0 where Linux gives ENOMEM
(the arena cannot tell a live mapping from a hole -- the limit `NR_MREMAP`
already states), and there are no file-backed mappings for EBADF to
distinguish. Out-of-arena ranges DO get ENOMEM, because everything a guest can
address is the arena, so that one is honestly answerable.

**Ruby's call sites, read off `ruby-glibc.fused` rather than assumed.**
`ruby_setup+0x6c` (0x731b8c) reaches prctl with `w1=1, w4=0` and x2/x3 already
zero -- the branch into it is `cbz x2, 731b8c` and x3 is set by `mov x3,#0` --
so the call really is `prctl(41, 1, 0, 0, 0)` and lands on the accepting side of
the reserved-argument rule. `heap_page_allocate_and_initialize` passes the name
`"Ruby:GC:default:heap_page_body_allocate"` over a 128 KiB range.

**Neutralization.** 9 host tests, every one broken deliberately and confirmed to
fail with the intended diagnostic. ⚠️ TWO of them did not fire on the first
attempt and both are recorded at the site:

- `a_thp_disable_set_by_one_thread_is_read_by_its_siblings` asserted the
  dumpable flag was untouched -- against `SUID_DUMP_USER`, which is 1, the same
  1 the THP write stores. A group write that clobbered `dumpable` passed. Fixed
  by moving dumpable off that value first.
- `a_range_four_gib_long_is_not_inside_a_384_mib_arena` cannot observe the
  wasm32 half of its own claim: `usize` is 64-BIT ON THE HOST, so re-writing
  `range_in_arena` in `usize` passed. The test was reworded to claim only the
  width property it can see (neutralized with `as u32 as u64`), and the limit is
  stated at the test -- `context::dumpable_arg_permitted`'s 64-bit test has the
  same one.

**Cross-builder run, the strongest neutralization available.** The new
`e2e/prctlmm_test.go` against the PRE-FIX `raptormark-builder:prctl`: 34 guest
checks fail with errno 22 and `TestPrctlMMNativeBaseline` still passes. Against
`raptormark-builder:rbprctl` both pass. Note what the cross-builder run does NOT
neutralize: every assertion that EXPECTS EINVAL passed pre-fix too, because the
catch-all also answered EINVAL -- those are carried by the arena host tests and
by the native baseline.

**Image.** `raptormark-builder:rbprctl`, layered onto
`raptormark-elfconv-base-patched:sisd0065` with `BASE_ID` and `TRANSLATE_SH`
copied verbatim from `:prctl`. Labels identical; `libecvisor.a`
`683ee3f8dd8b8835…` vs prctl's `5caf19b0a3d0d6d8…`, so the change is in the
ARTIFACT and the object cache stayed warm (the guest was served from cache under
`:prctl` and translated in 8 s under `:rbprctl`).

**Gate.** 250 host Rust tests (241 baseline + 9), `cargo fmt --check` clean,
`cargo check --target wasm32-wasip1` 5 warnings none new, `gofmt`/build/vet
clean. E2E focused runs, all PASS: `TestPrctlMMUnderEcvisor` (10.2 s),
`TestPrctlMMNativeBaseline` (0.9 s), plus `TestPrctlDumpable*`, `TestMmap*` and
`TestThreads*` re-run against `:rbprctl` to cover the `set_group_dumpable`
refactor onto the shared `for_thread_group` helper.

## 2026-08-22 -- Ruby's 384 MiB startup mapping is `Init_default_shapes`' redblack cache, and the size collision with `MEMORY_ARENA_SIZE` is arithmetic

**Question.** `[ecv] mmap region exhausted (want 402653184 bytes, bump
0x115a0000, 0 hole(s), shm_top 0x16000000) -> ENOMEM` on every ruby run.
402653184 is also exactly `MEMORY_ARENA_SIZE` (`runtime/src/arena.rs:37`,
`384 * 1024 * 1024`). Establish what ruby is asking for before someone connects
the two numbers.

**Answer.** `Init_default_shapes` (`shape.c:1218`), the Ruby 3.4 object-shape
tree initialiser, called from `rb_call_inits` -> `ruby_setup` -> `ruby_init`.
It makes TWO anonymous `PROT_READ|PROT_WRITE` mappings back to back:

| # | bytes | composition | on failure |
|---|---|---|---|
| 1 | 20971520 (20 MiB) | `SHAPE_BUFFER_SIZE` `0x80000` x `sizeof(rb_shape_t)` 40 | `rb_memerror()` -- **FATAL** |
| 2 | **402653184** (384 MiB) | `REDBLACK_CACHE_SIZE` `0x1000000` x `sizeof(redblack_node_t)` 24 | disables the ancestor cache -- **non-fatal** |

`REDBLACK_CACHE_SIZE` is `(SHAPE_BUFFER_SIZE * 32)` = `0x80000 * 32` =
16777216. So `16777216 x 24 = 402653184`. Every factor is a COMPILE-TIME
constant: no `sysconf(_SC_PAGESIZE)`, no `RUBY_GC_HEAP_INIT_SLOTS`, no
`RUBY_FIBER_*`, no env var of any kind. It is 384 MiB on any host, at any page
size.

**Not the GC heap and not the fiber pool.** Both were plausible and both were
checked, not assumed. The fiber pool is a DIFFERENT mapping in the same startup:
`fiber_pool_allocate_memory` (`cont.c:484`) <- `fiber_pool_expand`
(`cont.c:519`) <- `Init_Cont` (`cont.c:3435`), 21233664 bytes
(`FIBER_POOL_INITIAL_SIZE` 32 x a 663552-byte stride = a 640 KiB machine+VM
stack rounded to `((640K/4096)+1)*4096` plus one guard page), with
`MAP_STACK` in its flags. It succeeds. Naming the fiber pool would have been
wrong by a factor of 19.

**How.** The host is aarch64 and `ruby:3-slim` (ruby 3.4.10, glibc, aarch64) is
present locally, so ground truth was one native run away -- no lift needed.
`strace -e trace=mmap` gave the sizes; this build of strace has no `-k`, so an
`LD_PRELOAD` shim over `mmap` with `backtrace()`+`dladdr()`
(`.agents-workspace/tmp/rbmmap/shim.c`) gave the callers. `libruby.so.3.4.10`
ships **unstripped with `debug_info` and `.debug_macro`**, so `addr2line` named
the frames and `readelf --debug-dump=macro` yields the constants from the
SHIPPED BINARY rather than from memory:

    macro : SHAPE_BUFFER_SIZE 0x80000
    macro : REDBLACK_CACHE_SIZE (SHAPE_BUFFER_SIZE * 32)
    macro : ANCESTOR_CACHE_THRESHOLD 10

and `DW_AT_byte_size : 24` on `struct redblack_node`. `ruby-3.4.10.tar.gz`
`shape.c` was then fetched to read the fallback in source.

**Why no `0x18000000` literal exists** -- this CONFIRMS and explains the earlier
static scan. In `Init_default_shapes` at `.text+0x282fac` (and identically in
`.agents-workspace/fixtures/ruby-glibc.fused` at `0x882fac`, +0x600000):

    mov x1, #0x18                  // 24 = sizeof(redblack_node_t)
    add x0, x1, #0xfff, lsl #12    // 24 + 0xfff000
    add x0, x0, #0xfe8             // = 0x1000000 = 16777216
    bl  rb_size_mul_or_raise       // product computed AT RUN TIME
    ...
    bl  mmap

The count is synthesized from the element size by two `add`s (register reuse),
and the multiply is an overflow-checked call. Neither 402653184 nor 16777216
appears as an immediate. The three `0x18000000` occurrences in the fused ELF
really are VALUE flag masks.

**The ENOMEM is harmless, and the fallback is named.** `shape.c:1252`:

    if (GET_SHAPE_TREE()->shape_cache == MAP_FAILED) {
        GET_SHAPE_TREE()->shape_cache = 0;
        GET_SHAPE_TREE()->cache_size = REDBLACK_CACHE_SIZE;
    }

Confirmed in the disassembly at `+0x283170`: `mov w0, #0x1000000`,
`str xzr, [x5, #24]`, `str w0, [x5, #32]`, then `b` back onto the normal path.
Presetting `cache_size` to the cache's own capacity makes `redblack_new`
(`shape.c:141`) return `LEAF` forever, so `shape->ancestor_index` stays NULL for
every shape and `rb_shape_get_iv_index` always falls through to
`shape_get_iv_index` -- a linear walk up the shape parent chain instead of an
O(log n) redblack lookup. Correctness is unaffected.

**Cost of the fallback, measured.** An `LD_PRELOAD` shim that fails EXACTLY
`len == 402653184` (`.agents-workspace/tmp/rbmmap/fail.c`), native, ruby:3-slim:

- 500-ivar objects, 100 distinct leaf shapes, reading the ivar nearest the root:
  **0.009 / 0.009 / 0.009 s with the cache vs 0.024 / 0.024 / 0.024 s without**
  -- 2.7x, tight spread on three runs each.
- 14-ivar objects (the first version of the bench): **0.018 vs 0.017 s** -- no
  observable difference.

That difference is the neutralization. The shallow bench is reported because it
is the honest scale statement: the cost is bounded to instance-variable reads on
objects with >= `ANCESTOR_CACHE_THRESHOLD` (10) ivars that miss the inline
cache, and it scales with shape-chain DEPTH. A Rails-shaped object graph could
notice; a script cannot. Ruby ran to completion and produced the correct sum in
every no-cache run, which is the ENOMEM being handled, observed with no ecvisor
in the picture at all.

**Confirmed under ecvisor too.** `raptormark-builder:embed` +
`.agents-workspace/fixtures/rbbench/ruby-default.wasm` with
`RAPTORMARK_ECV_DEBUG=1`:

    [mmap] pid=1 0x101a0000..0x115a0000 len=20971520 fd=-1 flags=0x22
    [ecv]  pid=1 mmap region exhausted (want 402653184 bytes, ...) -> ENOMEM

Same two calls, same order, same sizes, same flags (`0x22` =
`MAP_PRIVATE|MAP_ANONYMOUS`) as the native strace. The FATAL one (20 MiB
`shape_list`) SUCCEEDS; only the non-fatal one is refused, and ~70 more
`[bbmiss]` events follow before the unrelated `rb_method_definition_set` crash
at guest `0x918724`.

**Why the collision is only a collision -- and what it implies.** The 384 MiB is
fixed inside ruby's own binary and reproduces identically on a native aarch64
Linux host with no ecvisor present; nothing ecvisor does can move it. But the
corollary is worth stating: ruby asks for a mapping exactly the size of the
WHOLE arena, so under a 384 MiB `MEMORY_ARENA_SIZE` this request can never
succeed no matter how early it runs. Natively it is nearly free -- the kernel
commits lazily, and `/proc/self/smaps` shows the mapping (named
`[anon:Ruby:Init_default_shapes:shape_cache]`, courtesy of the
`PR_SET_VMA_ANON_NAME` support added earlier today) resident at **28 kB of
384 MiB**. Under ecvisor a mapping is address space in a fixed linear memory,
so a lazily-committed reservation and a real allocation cost the same. That
asymmetry, not the numeric coincidence, is the durable finding.

**No code changed.** Making the mapping succeed is a separate decision nobody
has taken, and on this evidence it buys an ivar-lookup optimisation for
wide-object workloads at the price of the entire arena.

## 2026-08-22 -- What a JIT guest does under ecvisor: YJIT dies TWICE, both times loudly, and NEITHER death is about emitting code

**Question.** README's scope argument is "a runtime that emits aarch64 as it
runs has no machine code to lift ahead of time". `ruby:3-slim` has YJIT
compiled in and off by default, so one fused artifact sits on either side of
that line depending on argv. Nobody had observed what actually happens. The
2026-08-19 `--yjit` sidecar died in `rb_method_definition_set` -- before YJIT
could matter -- and the TODO explicitly forbade citing it. Patch 0062 fixed
that crash, so the question became answerable.

**Answer, in one line.** YJIT never emits a single byte of machine code, and
the reason is *not* the one README implies. There are two independent walls,
both fatal, both loud, and the second one is a plain `ENOMEM` that a native
kernel would also produce if it had the same address space.

### Wall 1 -- `--yjit` cannot even be PARSED: an undecoded NEON `orr` in `proc_options`

    [ecv] pid=1 undecoded instruction at 0x87ab18 -> SIGILL delivered
    ruby: [BUG] Illegal instruction at 0x0000000000000000
    exit=127

`0x87ab18` in `.agents-workspace/fixtures/ruby-glibc.fused` is

    87aac8: adrp x0, ad9000 / add x0, x0, #0xfd0   ; -> "yjit"
    87aad4: mov  x2, #0x4
    87aad8: bl   654d40                            ; strncmp(arg, "yjit", 4)
    87aadc: cbnz w0, 87aaa0
    87aae0: ldrb w28, [x27, #5]                    ; terminator after "--yjit"
    ...
    87ab10: ldr  d28, [x19, #80]
    87ab18: 0f04141c  orr v28.2s, #0x80            ; <-- UNDECODED
    87ab1c: str  d28, [x19, #80]

The string at `0xad9fd0` is verified to be `"yjit"` (`objdump -s`), and the
immediate is verified to be the right one from ruby's own source: `ruby.c`'s
`EACH_FEATURES` list makes `feature_yjit` the **8th** entry (gems,
error_highlight, did_you_mean, syntax_suggest, rubyopt,
frozen_string_literal, rjit, yjit), so `FEATURE_BIT(yjit) == 1U << 7 == 0x80`.
`ruby_features_t` is `{ unsigned mask; unsigned set; }`, and `FEATURE_SET`
ORs the bit into BOTH words; GCC vectorised that into one 8-byte load, one
`ORR (vector, immediate)` over two 32-bit lanes, and one store. So the trapping
instruction IS `FEATURE_SET(opt->features, FEATURE_BIT(yjit))`.

`0f04141c` decodes as `ORR <Vd>.2S, #0x80` -- Q=0, op=0, cmode=0b0001, abc=0b100,
defgh=0 -- i.e. the `ORR_ASIMDIMM_L_sl` form. In the pinned remill,
`TryDecodeORR_ASIMDIMM_L_HL` and `TryDecodeORR_ASIMDIMM_L_SL`
(`Decode.cpp:17329`, `:17367`) are both stubs that `return false`. This family
does NOT appear in `.agents-workspace/fixtures/undecoded/postgres-families.txt`
-- postgres never executes it -- so it is a genuinely new site.

⚠️ **Every `--yjit*` spelling hits the same instruction.** `--yjit-exec-mem-size=8`
was tried specifically because the disassembly branches away at
`cmp w28, #0x2d` for a `-` suffix; it traps at `0x87ab18` all the same, because
the suboption path still sets the feature bit. `--jit` is the same bit
(`DEFINE_FEATURE(jit) = feature_yjit`). **argv cannot arm YJIT under ecvisor at
all today.**

**The differential that makes this a claim and not a story.** The SAME command
line minus `--yjit` runs to completion and prints the native checksum:

| arm | argv | result |
|---|---|---|
| off | `ruby --disable-gems -I ... /yjit.rb` | `YJIT-ENABLED false`, `WORK-OK 33825`, `PROBE-END`, **29.220 s** |
| on | `ruby --yjit --disable-gems -I ... /yjit.rb` | SIGILL at `0x87ab18` |
| on | `ruby --yjit-exec-mem-size=8 --disable-gems -I ... /yjit.rb` | SIGILL at `0x87ab18` |

### Wall 2 -- armed WITHOUT argv, YJIT dies on a 128 MiB PROT_NONE RESERVATION

Two argv-free routes exist and both bypass `proc_options`:
`RubyVM::YJIT.enable` (Ruby 3.3+, verified natively to arm the identical
compiler and produce identical stats to `--yjit`) and `RUBY_YJIT_ENABLE=1`,
which is handled in `ruby_opt_init` (`ruby.c:2340`) and whose codegen does not
use the vector `orr`. Both get through, and both then die here:

    [ecv] pid=1 mmap region exhausted (want 134217728 bytes, bump 0x13020000,
          1 hole(s), shm_top 0x16000000) -> ENOMEM      x4
    ruby: yjit: mmap:: Cannot allocate memory
    <internal:yjit>:51: [BUG] mmap failed
    exit=127

with a Ruby-level backtrace naming `enable`. `rb_yjit_reserve_addr_space`
(`yjit.c`) probes downward from its own address with
`MAP_FIXED_NOREPLACE`, then falls back to a hintless `mmap(NULL, ...)`. The
symbol is at guest `0x9d5400`, so the `req_addr -= 4 MiB` walk can take exactly
three steps before wrapping below zero and failing `req_addr < probe_region_end`
-- **3 probes + 1 fallback = the 4 ENOMEM lines observed.** The count is a
prediction that matched, not a tally after the fact.

**Why the reservation can never succeed.** `MMAP_START_VMA 0x1000_0000` ..
`MMAP_END_VMA 0x1600_0000` (`runtime/src/arena.rs:40-41`) is a **96 MiB**
private-mmap window, and shared segments are carved down from the same top.
YJIT's default `--yjit-exec-mem-size` is **128 MiB**. 128 > 96, so this request
fails no matter how little the guest has allocated -- it is not pressure, it is
arithmetic. This is the third instance of the asymmetry recorded earlier today
for `Init_default_shapes`' 384 MiB redblack cache: natively a `PROT_NONE`
reservation is nearly free and lazily committed, under ecvisor a mapping IS
address space in a fixed linear memory, so a reservation costs exactly what an
allocation costs.

**Neutralized against a native oracle.** An `LD_PRELOAD` shim that refuses
exactly `len == 134217728 && fd == -1 && prot == PROT_NONE`
(`.agents-workspace/fixtures/rbbench/yjit-2026-08-22/failyjit.c`), native
`ruby:3-slim`, no ecvisor anywhere, produces the **same two lines and the same
Ruby backtrace** and aborts (exit 134 vs ecvisor's 127). So the loudness is
RUBY's, not ecvisor's. Ruby's source even intends a quiet
`exit(EXIT_FAILURE)` when `errno == ENOMEM`, but `perror()` runs first and
clobbers `errno`, so the `rb_bug("mmap failed")` arm is taken -- natively too.
Had this not reproduced natively, "ecvisor makes JIT death loud" would have
been the wrong conclusion.

### What this does and does not establish

- ❌ It is **NOT** a refused `PROT_EXEC`. `NR_MPROTECT => state.set_ret(0)`
  (`runtime/src/sys.rs:1624`) is unconditional, so YJIT's W^X toggling would
  have silently SUCCEEDED. Natively the probe issues **47** `mprotect(...,
  PROT_READ|PROT_EXEC)` calls against one 12288-byte region; under ecvisor every
  one of them would have returned 0. This is read from the runtime, not
  observed, because the reservation dies first.
- ❌ It is **NOT** a jump into unlifted bytes. Nothing ever reached
  `__remill_function_call` or "vma not in the lifted function table".
- ❌ YJIT does **NOT** detect anything and disable itself gracefully. Both walls
  are fatal.
- ✅ Both failures are **LOUD**: exit 127, ruby's own `[BUG]` report, and in the
  argv case a SIGILL that ruby's handler prints. A silent
  fall-back-to-interpreter -- the outcome that would have been most misleading
  for an operator -- does not happen.
- ⚠️ **The original question is still only half answered.** "What does a guest
  do when it EMITS machine code" remains untested, because ecvisor refuses the
  memory before any code exists. Do not upgrade this entry into evidence that
  lifting-time scope is what stops JIT; on this evidence the AOT boundary has
  not yet been reached.

### Native baseline, measured (not recalled)

`ruby:3-slim`, aarch64, this host, `fib(20) x 5`:

| arm | `RubyVM::YJIT.enabled?` | work | YJIT stats |
|---|---|---|---|
| no flag | false | 33825 / 0.003 s | n/a |
| `--yjit` | **true** | 33825 / 0.004 s | 1 iseq, 11 blocks, region 12288 B, inline 848 B |
| `--yjit --yjit-call-threshold=2` | **true** | 33825 / 0.002 s | 3 iseq, 15 blocks, region 12288 B, inline 1408 B |
| `RubyVM::YJIT.enable` from Ruby | **true** | 33825 / 0.002 s | identical to `--yjit` |

`strace` of the armed run: one
`mmap(<hint>, 134217728, PROT_NONE, MAP_PRIVATE|MAP_ANONYMOUS|MAP_FIXED_NOREPLACE)`
after **144** `EEXIST` hint probes (145 `MAP_FIXED_NOREPLACE` calls in all,
the last one succeeding), then **119** `mprotect` calls against that region --
**47** of them `PROT_READ|PROT_EXEC` -- toggling W^X over the 12 KiB used. That
12 KiB is the whole point: YJIT reserves 128 MiB and commits a rounding error.

⚠️ Reporting `RubyVM::YJIT.enabled?` from inside the guest is what makes the
interpreter arm evidence at all. The flag on a command line proves nothing;
under ecvisor the flag never even reached the option table.

### Incidental, and it will cost the next session an hour if it is not recorded

⚠️ **Ruby under ecvisor requires `--disable-gems`.** Without it,
`ruby -I ... /startup.rb` dies at

    *** longjmp causes uninitialized stack frame ***: terminated
    [ecvisor] fatal: guest trap or undecodable instruction at PC 0x1621ae0
              (__remill_error) lr=0x1621ac0

which is glibc's `__longjmp_chk` -> `__fortify_fail` -> abort while RubyGems
loads. The preserved 2026-08-19 sidecars all carry
`--disable-gems -I /usr/local/lib/ruby/3.4.0 -I .../aarch64-linux` and never say
why, so a sidecar built the obvious way (`ruby /script.rb`) fails in a way that
reads exactly like a runtime regression. It cost an hour and a wrong hypothesis
("the new builder broke ruby") this session, refuted by running the new sidecar
against the KNOWN-GOOD 2026-08-19 module and seeing it fail there too.

### Artifacts and how to re-run

`.agents-workspace/fixtures/rbbench/`:

- `ruby-rbprctl.wasm` -- ruby lifted 2026-08-22 with
  `raptormark-builder:rbprctl` (9m47s cold, 51.97 MiB module, program id
  `ruby_glibc_fused_3f3a7e07998d`).
- `yjit-2026-08-22/` -- the sidecars (`p-off`, `p-on`, `p-on2`, `p-enable`,
  `p-env`, `p-mem8`, `p-gems`), both probe scripts, the native `failyjit.c`
  oracle, and `rfsdump.py`, which decodes an rfs image's boot record and exec
  map (that is how the missing `--disable-gems` was found).

```
docker run --rm -v "$PWD:/w" --entrypoint bash raptormark-builder:rbprctl --login -c \
  'wasmedge --enable-all --dir /:/w --env RAPTORMARK_ROOTFS=/yjit-2026-08-22/p-enable.img \
     --env RAPTORMARK_ECV_DEBUG=1 /w/ruby-rbprctl.wasm'
```

⚠️ The 2026-08-19 sidecars name module id `ruby_glibc_fused_2d9985623f89` while
both modules in this directory are `..._3f3a7e07998d`. They still run, because
a one-program registry falls back to index 0 -- which is exactly the silent
drift the `-manifest`/`-map` path exists to prevent. Build new sidecars with
`-manifest`, never `-prog`.

**No code changed.** Making either wall passable is a scope decision for the
operator: wall 1 is a lifter patch (`ORR_ASIMDIMM_L_HL`/`_L_SL`), wall 2 is an
arena-size or executable-mapping decision that README explicitly declines.

## ⚠️ 2026-08-23 -- OPERATIONAL: `timeout` on `docker run` does not stop the CONTAINER

Found because the user asked what the running wasmedge instances were doing.
Answer: three postgres modules from the previous afternoon, still spinning after
**10, 11 and 12 hours**, at ~6% CPU and ~8 GB RSS between them.

**The mechanism, and it defeats a precaution that looks sufficient.** This tree
already records that a postgres module does NOT exit when `boot.sh` finishes --
the postmaster keeps running, so `BOOT: DONE` is the completion signal and the
process never returns. The remedy applied was

```sh
timeout 3600 docker run --rm ... wasmedge ... /out/pg.wasm > boot.log 2>&1
```

`timeout` kills the **docker CLI client**. The container is a child of the
daemon, not of the client, so it **detaches and runs on** -- `--rm` does not help
either, since nothing has asked the container to stop. Every "completed"
postgres run in this session left a live postmaster behind.

**The tell was visible and was misread.** `uptime` reported load average 19 on a
20-core box hours earlier. That was checked with `ps --sort=-pcpu`, which showed
one process at 99.9% and everything else near zero, and was written off as "not
my work". Three processes at 6.1/6.3/6.9% do not appear at the top of that sort.
⚠️ **A load average that does not match the top of `--sort=-pcpu` means the cost
is SPREAD, not absent.** Sum the column, or look at RSS.

**Second tell, stronger, and also missed**: the three harness background tasks
for those runs never completed. They finished within seconds of `docker stop`,
which is proof they had been blocked on the container the entire time. Results
were reported anyway because the log FILES were read directly -- correct
conclusions, reached while claiming a run had "finished" when only its output
had. ⚠️ If a backgrounded command has produced its expected output but its task
has not notified, the process is still alive. That mismatch is the signal.

✅ **The fix, and it is what the locale agent was already doing**: give the
container a name and stop it explicitly.

```sh
docker run --rm --name pgrun-<tag> ... &
# ... wait for BOOT: DONE in the log ...
docker stop -t 5 pgrun-<tag>
```

`--name` also makes cleanup scopeable to one container, which matters here for a
reason recorded on 2026-08-22: an unfiltered `docker ps -q | xargs docker kill`
took out the user's k3d cluster and application stack. Scope by NAME or by
explicit id; never by "everything running".

## 2026-08-23 -- `/usr/bin/locale` as a fifth postgres program: the program works, the shared window does not

Acting on the TODO item "`/usr/bin/locale` is not in the exec map". Work dir
`.agents-workspace/tmp/pgloc5/`, builder `raptormark-builder:rbprctl`, object
cache `.agents-workspace/objcache`.

### The closure-wide layout survives five programs, and it is free

`closurefuse` was first re-run on the ORIGINAL four programs to recover the
options the 2026-08-17 `pgcl4` fuse used, because they were recorded nowhere.
They are

```
-libdirs /usr/lib/postgresql/17/lib
-extra   /usr/lib/postgresql/17/lib/dict_snowball.so,/usr/lib/postgresql/17/lib/plpgsql.so
```

and the proof is that the four fused images came out byte-identical
(`sha256` `2789286606bb` postgres, `87cb38c0a0ad` initdb, `687c3e70f865` dash,
`ee75eb5a553d` psql -- the same ids `pgcensus2/programs.json` carries). Method
note: which plugins were fused was recovered by taking each candidate `.so`'s
first `./build/...` source-path string and grepping the fused image for it.
`amcheck.so` and `test_decoding.so` are FALSE POSITIVES on that test -- their
first such string is `./build/../src/include/access/tupmacs.h`, which the
postgres binary already contains. Check the marker against the BASE binary too.

Adding `/usr/bin/locale` as a fifth entry changes nothing about the layout:

```
SHARED layout: 36 libraries, top 0x7e40248 (ceiling 0x0A000000, 78.9% used)
```

-- identical line, identical `RAPTORMARK_LIB_RANGES`, and the four existing
images still hash the same. `locale` `DT_NEEDED`s only `libc.so.6` and
`ld-linux-aarch64.so.1`, both already in the closure, so it contributes no new
library and consumes no ceiling. **The feared cold re-translation of all five did
not happen**: programs 0-3 were cache hits at 0s and only
`usr_bin_locale.fused` (2,873,808 bytes) translated, 15m30s, on a host at load
average ~139 across 20 cores. `link-all` 5 programs: 1m51s, module 372.76 MiB
(361.03 MiB for four).

⚠️ Appending is what made this cheap. Program INDEX is part of the object cache
key, so `locale` had to go LAST.

### The warning is gone, and the program really runs

initdb's `performing post-bootstrap initialization` no longer emits
`sh: 1: locale: Exec format error` or
`WARNING: no usable system locales were found`. The mechanism was never in
doubt: `sys_execve` answers ENOEXEC for a path that resolves in the VFS but has
no program (`runtime/src/sys.rs:5243`).

That the warning vanished because `locale` WORKS -- not because nothing asked
for it -- is shown directly. A `boot5.sh` probe (boot.sh verbatim plus a locale
section) run against the five-program module prints

```
C
C.utf8
POSIX
en_US.utf8
locale-abs-rc=0
```

which is exactly what `locale -a` reports natively on `postgres:17`.

### ...and then the postmaster cannot get its shared memory

```
FATAL:  could not map anonymous shared memory: Cannot allocate memory
HINT:   ... request ... (currently 78618624 bytes) ...
BOOT: psql never connected after 41 tries
```

The two runs are byte-identical for 866 lines; the divergence is the `locale`
process starting, and after that the only difference is this FATAL.

**Root cause, from a debug-enabled A/B (`RAPTORMARK_ECV_DEBUG=1` on BOTH sides;
there was no debug-enabled 4-program baseline in the tree and one had to be
made).** `shm_try_reclaim` (`runtime/src/context.rs:4316`) refuses to reclaim a
`SharedKind::File` segment while its backing path still resolves in the VFS.
That rule is written for POSIX `/dev/shm/PostgreSQL.<n>`, which the guest
unlinks. glibc's locale and iconv loaders map ORDINARY, PERMANENT rootfs files
with `MAP_SHARED`:

```
pid=16 shm map "/usr/lib/locale/locale-archive"                    -> new 0x10ff0000 (3080192 bytes)
pid=16 shm map "/etc/locale.alias"                                 -> new 0x10fe0000 (65536 bytes)
pid=14 shm map ".../gconv/gconv-modules.cache"                     -> new 0x10fd0000 (65536 bytes)
```

Nothing ever unlinks those, so they are never reclaimed and they pin the shared
window's bottom for the life of the module. At the identical point in the two
runs -- the last reclaim by pid 14, immediately before `BOOT: postmaster`:

```
4 programs: [shm] pid=14 reclaimed File 0x112e0000..0x113e0000; shm_top 0x16000000, 0 hole(s)
5 programs: [shm] pid=14 reclaimed File 0x112e0000..0x113e0000; shm_top 0x10fd0000, 1 hole(s)
```

The private mmap region is `[MMAP_START_VMA, shm_window.top)`, so it shrinks
from 0x6000000 (96 MiB) to 0xfd0000 (15.8 MiB), and

```
[ecv] pid=17 mmap region exhausted (want 78618624 bytes, bump 0x10000000, 0 hole(s), shm_top 0x10fd0000) -> ENOMEM
```

3,211,264 bytes leaked; 78,618,624 wanted. **This is a reclaim defect, not an
address-space budget.**

⚠️ **CORRECTION to a reading that was nearly published.** The two lines
`mmap region exhausted (want 150994944 / 149897216 bytes ...)` at pid 8 look
like evidence and are not: they appear in BOTH debug runs (line 1436/1437 of
each) and are initdb's `test_config_settings` walking `shared_buffers`
downward, which EXPECTS failures. They are also absent from every earlier
4-program log for the trivial reason that `ecv_debug!` was off. **"0 occurrences
in the working run" means NOT LOGGED, not DID NOT HAPPEN** -- an ungated line
and a gated one cannot be compared across runs with different gates.

### The fifth program is not the problem

Same five-program module, same rootfs, one variable: `/usr/bin` and `/bin`
removed from the boot PATH, so postgres's `popen("locale -a")` cannot find the
program (`sh: 1: locale: not found`). That run completes end to end and
reproduces every BEFORE value. In the same run the boot script's own
`/usr/bin/locale -a`, called by ABSOLUTE path, still returns the four locales
with rc=0. So the five-program module is sound and `locale` is sound; the
blocker is what glibc's locale path does to the shared window.

### Dead end: the `RAPTORMARK_ECV_NO_FILE_SHM` bisect switch

It does not yield a usable configuration. With it, initdb's probe degrades to
`max_connections 25` / `shared_buffers 400kB` and the bootstrap dies with
`FATAL: could not map shared memory segment "/PostgreSQL.2040785318": No such
device`. It did NOT drop postgres to sysv DSM as its comment predicts --
`selecting dynamic shared memory implementation ... posix` still.

### Values, for the record

Three runs agree exactly (4-program relinked on rbprctl; the same with
`RAPTORMARK_ECV_DEBUG=1`; the 5-program no-PATH control), and all three agree
with the historical `pgcensus2/boot.noflag`:

```
 1 | raptormark |     10
 3 | WASM       |      4
rows=2 sum=4      count(pg_database)=3      count(pg_authid)=16
rel_bytes=8192 relpages=1 path=global/1262   file_bytes=8192   blk1_head=<empty>
collations=876    libc_collations=C POSIX
```

`collations=876` is new information: the ICU import already works. The libc side
contributes exactly nothing without `locale`, which is what the item was about.

The five-program run produced NO SQL at all -- the postmaster never started --
so there is no result diff to make. The item stays OPEN.

## 2026-08-23 -- file-backed `MAP_SHARED`: the name is not the rule, WRITABILITY is

Fixes the blocker recorded the same day under "`/usr/bin/locale` as a fifth
postgres program". `shm_try_reclaim`'s `SharedKind::File` arm refused to reclaim
a region while its backing path still resolved in the VFS. That is correct for
POSIX shm and wrong for an ordinary read-only rootfs file, and glibc maps three
of those `MAP_SHARED` and never unlinks them.

### What the rule was actually protecting, and what it was not

The premise is: a later `mmap` of the same NAME must find the bytes an earlier
mapper wrote through the region -- and this runtime never writes a `MAP_SHARED`
mapping back to the file (there is no `msync` and the file arm copies in once, at
first map), so those bytes exist ONLY in the region. Recycling it loses them.

That premise needs bytes to have been WRITTEN. A `PROT_READ` mapping cannot have
written any, so for a read-only region the premise is FALSE, not merely unlikely.
The new rule is one line:

```rust
pub fn shm_file_reclaimable(writable: bool, path_exists: bool) -> bool {
    !(writable && path_exists)
}
```

`writable` is a property of the REGION's history, accumulated and never cleared:
`NR_MMAP` ORs `prot & PROT_WRITE` in on the reuse path as well as at creation,
and `NR_MPROTECT` -- previously a bare `set_ret(0)` -- marks every region its
range OVERLAPS. `prot` (arg 2) had never been read by `NR_MMAP` at all.

### Why the other two discriminators are worse

* **Path prefix `/dev/shm/...`** -- the TODO entry's suggested fix, and the one
  with a concrete counterexample: PostgreSQL's `mmap` DSM backend
  (`dynamic_shared_memory_type=mmap`) maps `$PGDATA/pg_dynshmem/...` `MAP_SHARED`
  with exactly the cross-process expectations the `posix` backend has, and the
  prefix does not cover it. Same class of proxy as `entry_is_landing_pad`.
* **Backing store (the file is in the read-only rfs lower layer)** -- answers a
  question nobody asked. The file changing is not the hazard; the REGION
  diverging from the file is, and it diverges the moment a writable mapping
  stores into it, image-backed or not. It would also need a new `Vfs` API to
  report which layer served a path.

### Evidence

`shm_files` became a named `ShmFile { path, vma, len, writable }` (7 call sites).
Six host tests in `context::shm_file_reclaim_tests`, built the way `NR_MMAP`
builds a region and with the backing file CREATED so the path resolves -- without
that the old rule would reclaim them too and every test would pass while
observing nothing. Neutralized three ways, each restored afterwards:

| predicate | failing tests | diagnostic |
| --- | --- | --- |
| `!path_exists` (the old rule) | 4 | `the window stayed at 0x15cf0000 instead of 0x16000000` |
| `!writable` | 2 | `an unlinked POSIX shm region leaked` |
| `true` | 2 | `a writable POSIX shm region was recycled while its name still resolved` |

e2e: `TestSharedFileMappings*` appended to `e2e/shmreclaim_test.go` (whose
existing guard is anonymous-only and does not reach this arm). Two phases,
neutralized independently against REAL images rather than against a source edit:

* phase A pins 3 x 8 MiB read-only file mappings in a child that `_exit`s without
  unmapping, leaves the files in place, then probes the largest private mapping.
  `private-mmap-max=88` (MiB) fixed; **72** against `raptormark-builder:rbprctl`,
  which still has the path-only rule -- below the 80 MiB the guest requires.
* phase B writes a marker through a writable `MAP_SHARED` region, unmaps it as
  the last mapper with the path still present, and re-maps by name. Fixed: marker
  intact. Against a deliberately-broken side image (`shmneut`, predicate forced
  to `true`): `FAIL marker survived the remap (head)` / `(tail)`. ⚠️
  `raptormark-builder:shmneut` exists on this host and is DELIBERATELY WRONG --
  it is a neutralization artifact, not a builder. Do not lift with it.

⚠️ A native baseline runs the same guest under Linux for both phases. Phase B
passes there through write-back, which is a DIFFERENT mechanism reaching the same
observable -- and the observable is what a guest is entitled to.

### `shm_top` before and after, and the run

Five-program module RELINKED only (`lift -link-only`, 1m59s -- the lifted `.o`
depends on the fused ELF alone), builder `raptormark-builder:shmfix`. Labels
identical to `rbprctl`; `sha256sum /opt/ecvisor/libecvisor.a` differs
(`683ee3f8...` -> `bc4a0e15...`), which is the artifact-level proof the runtime
change shipped.

Last `[shm]` line before `BOOT: postmaster`, which is the direct observation:

```
BEFORE  [shm] pid=14 reclaimed File 0x112e0000..0x113e0000; shm_top 0x10fd0000, 1 hole(s)
AFTER   [shm] pid=14 reclaimed File 0x112d0000..0x112e0000; shm_top 0x16000000, 0 hole(s)
```

The three glibc regions are now reclaimed the moment their mapper exits, e.g.

```
[ecv] pid=16 shm map "/usr/lib/locale/locale-archive" -> new 0x10ff0000 (3080192 bytes)
[shm] pid=16 reclaimed File 0x10ff0000..0x112e0000 (3080192 bytes); shm_top 0x112e0000
```

while the postmaster's DSM segment is held and REUSED by name across a dozen
backends -- `pid=23/24/25/... shm map "/dev/shm/PostgreSQL.1985427006" ->
existing 0x112e0000` -- which is the case the rule exists for, still working.

The run reaches `BOOT: DONE` (30 min, `RAPTORMARK_ECV_DEBUG=1`) and reproduces
`1 | raptormark | 10`, `3 | WASM | 4`, `rows=2 sum=4`, `pg_database=3`,
`pg_authid=16`.

⚠️ **Two values deliberately DIFFER from the 4-program baseline, and they are
the point rather than a regression.** With `locale -a` working, initdb finds
system locales, so `collations=879` (not 876) and
`libc_collations=C C.utf8 POSIX en_US en_US.utf8` (not `C POSIX`). The delta is
exactly the three libc collations the four locales `locale -a` reports imply, and
`locale-path-rc` / `locale-popen-rc` went 126 -> 0 in the same run. An AFTER run
that reproduced 876 / `C POSIX` would have meant locale was still broken.

### Leftovers on this host

* `raptormark-builder:shmfix` -- the real fix. Use this one.
* `raptormark-builder:shmneut` -- ❌ predicate forced to `true`; built ONLY to
  neutralize the e2e guard's phase B. Never lift with it.
* `.agents-workspace/tmp/pgloc5/pg.shmfix.wasm`, `boot.shmfix.debug`,
  `shmfix.clean`.
