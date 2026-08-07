# AArch64 Lifter and Coverage

## Summary

Raptormark extends the recovered elfconv lifter through the ordered patch series rather than by modifying the pinned submodule. Coverage work is driven by decoded evidence from real fused images, with native-oracle E2E tests that distinguish instruction variants and deliberately neutralize each fix.

## Key Facts

- Patch 0050 indexes BTI landing pads without narrowing the original 512 KiB scan semantics.
- Patches 0051 and 0058 together cover register-register and by-element widening multiply families.
- Patches 0053-0056 add vector sign, pairwise widening, popcount correctness, and measured integer-vector operations.
- Patch 0057 reports every undecoded instruction as `[ecv-undecoded] vma=... enc=... fn=...` during lifting.
- `RAPTORMARK_TRANSLATE_VERBOSE=1` is required to relay successful container stderr, and a cold object cache is required to execute the lift.
- The latest cryptography inventory has 2,805 real sites across 474 encodings; the largest families are `st1` (736), `tbl` (706), and `sli` (535).

## Details

### Indexed BTI discovery

The old per-function loop searched forward from `trace_addr + 4` for `bti j` or `bti jc` until `bti c`, capped at 512 KiB. Bounding the loop by a synthetic function end is unsafe because an `_ecv_fde_*` boundary may split one real function. `AArch64TraceManager::CollectBTIJumpPads` instead queries sorted indexes built once over executable ranges, preserving the old answer and its cap.

Readable extent is the maximal gap-free run of executable ranges, not merely the containing section. A four-byte word is readable only when its start is before `end - 3`. The indexed result was byte-identical to the scan on busybox, bash, apt-get, and PostgreSQL, and was audited call-by-call on a branch-protected computed-goto fixture and PostgreSQL. It improved non-branch-protected lifts by 1.96-2.29x and left PostgreSQL unchanged, as predicted by its dense BTI population.

### Instruction patches

Patch 0051 implements the register-register `{S,U}MULL{2}`, `{S,U}MLAL{2}`, and `{S,U}MLSL{2}` forms missing beside the older `SMLAL` support. Patch 0058 completes the by-element forms. For 32-bit elements, the element register is the five-bit `M:Rm`; for 16-bit elements, `M` belongs to the index. The element operand uses the full 128-bit register even when the source arrangement is 64-bit.

Patch 0053 implements vector FNEG and FABS as sign-bit operations. Arithmetic negation is wrong for signed zero, signalling NaNs, payload preservation, and FPSR behavior. Patch 0054 adds `{S,U}ADDLP` and `{S,U}ADALP`; patch 0055 fixes the pre-existing 16-byte CNT implementation whose 64-bit SWAR masks cleared the upper eight lanes. Patch 0056 implements the measured plain-register NEG, ABS, MLA, MLS, and USUBL forms rather than an entire decoder group containing unverifiable reciprocal estimates.

### Coverage measurement

The undecoded report must be filtered rather than treated as a task list verbatim. `enc=0x00000000` is padding lifted as code and should be excluded; sites should be grouped by encoding or mnemonic family. A crash reveals the first gap on one path, not the dominant gap in the image: `usubw` accounted for only 8 of 2,805 remaining real sites after patch 0058.

The report also guards the scope of family claims. Patch 0051 made its motivating Redis path run but left 557 by-element multiply sites. Patch 0058 was accepted only after a cold-cache remeasurement reduced those sites to zero while leaving the padding count unchanged as a control.

## Files

- `patches/0050-*.patch` through `patches/0058-*.patch`: Lifter changes discussed here.
- `internal/builder/buildimage.go`: Patch-presence assertions used during builder creation.
- `internal/translate/`: Verbose container-output relay.
- `e2e/btiswitch_test.go`: Branch-protected computed-goto coverage.
- `e2e/mull_test.go`, `e2e/fsign_test.go`, `e2e/addlp_test.go`, `e2e/vecint_test.go`, `e2e/elemmul_test.go`: Native-oracle instruction guards.

## Test Coverage

Every instruction guard has a native aarch64 baseline and was run against either the pre-patch builder or a deliberately broken implementation. Inputs discriminate lane halves, signedness, accumulator behavior, register-number extension, signed zero, and NaN bit patterns. The BTI fixture asserts that branch-protected landing pads exist before trusting its execution result.

## Pitfalls

- Never edit `third_party/elfconv`; change the ordered patch series.
- Verify instruction masks and field extraction against real `objdump` encodings.
- A guest that runs proves only the paths it executed, not full binary coverage.
- Implementing a stub can expose a silent defect in the next instruction of the idiom.
- Generate each patch from a worktree whose HEAD contains its immediate predecessor, then inspect every `diff --git` line.

## Consolidated Update: PAC Jump Tables and the PostgreSQL Inventory

Patch 0062 removes a binary-wide PAC-entry proxy from the existing jump-table sweep: Ruby's PAC-signed `rb_method_definition_set` had no `bti j`, missed a compact table target, and re-entered through `__remill_jump`. An assembly guard reaches the literal `paciasp` entry.

A cold PostgreSQL inventory found 2,950 real undecoded sites across 398 encodings after excluding 8,451 `enc=0` padding reports. The largest families were `tbl` (706), `st1` (686), and `sli` (574), while planner-hot `fnmul` had only 9 sites: site count ranks coverage effort, not reachability. Patch 0063 reduced TBL 706 to zero and real sites to 2,244 with padding unchanged. Its generated decoder scaffold already existed as `return false` stubs; TBX remains separate because it reads the destination.

## Consolidated Update: Scalar Floating and SISD Coverage

Patch 0064 implemented FNMUL/FNMADD/FNMSUB and moved PostgreSQL into planner work despite covering only 11 sites. Patch 0065 implemented scalar SISD ADD/SUB and unblocked hash aggregation. The SISD result must also zero the upper 64-bit lane.

Patches 0062 through 0065 shared one shape: generated dispatch existed while leaf decoder bodies returned `false`. Presence in a query-related function is not execution; query shape correctly predicted the hash-aggregate stop.

## Consolidated Update: The Reachability Census, and Zero on Two Real Workloads

The two claims above -- "site count ranks coverage effort, not reachability" and "presence in a query-related function is not execution" -- are now measured rather than asserted. `RAPTORMARK_ECV_UNDEC_CENSUS=1` (2026-08-21/22) makes `__ecv_warning` record an undecoded site and CONTINUE instead of aborting, so one run enumerates every undecoded instruction a workload actually EXECUTES, rather than one per ~30-minute lift.

Measured on a base carrying patches 0062-0065: `python:3-slim` over three workloads reports **zero** executed undecoded sites, and a four-program PostgreSQL closure -- initdb, postmaster, DDL/DML, aggregates, and seq scans over real catalog relations -- also reports **zero**, completing with correct values. The static inventory for that same closure lists 2,244 sites, of which `st1` 686 + `sli` 574 + `fcvt` 212 = 1,472 are reached by neither workload. Site count has now failed to predict capability three times: `tbl` 706 moved nothing observable, `fnmul` 9 unblocked the planner, and these 1,472 are unexecuted. Choose the next patch from what a workload DIES on, then use the census to get the whole list instead of the first entry.

Operating notes. The census is UNSOUND by construction: a skipped instruction means every result after the first skip may be garbage, so only the `addr=` lines carry information, and a clean-looking run is the most dangerous outcome. It is behaviourally inert when it skips nothing -- verified by an interleaved A/B on python with overlapping bands, and by the PostgreSQL arms producing byte-identical output. A build older than the fix reports ONE site and stops: the unhandled SIGILL's default action arms `Pending::Exit` inside `deliver_pending_signals` before it returns, and the run then dies at the next syscall, so the correct shape decides the disposition BEFORE posting and never posts when the census will handle it. A hot undecoded site still pays signal posting per occurrence -- deduplication covers logging only -- so a site inside a loop can still perturb a workload; that case is unmeasured.
