# Performance Investigation

## Summary

Raptormark performance is dominated by irregular critical paths: the largest lifted function, the largest codegen partition, and per-context-switch runtime work. Reliable progress came from statement-level timing, removal experiments, and treating variance as evidence.

## Key Facts

- Build cost does not scale with ELF or bitcode size alone.
- Namespace-before-split converted one dominant codegen partition into useful parallel work.
- Patch 0022 prevented a tail-form branch from becoming a huge inlined compile unit, reducing one nginx conversion from hours to minutes.
- Codegen parts are scheduled largest-first, but programs still use separate queues.
- Runtime-only changes should be benchmarked by relinking cached lifted objects.
- Arena swapping removed the two 384 MiB copies; bounded snapshots address concurrency capacity rather than that former copy cost.
- After library-scoped codegen reuse, elflift and serial whole-module passes dominate cached programs.

## Details

Measurements showed OpenSSL translating in tens of minutes while a smaller nginx closure took 6.5 hours. The difference was a pathological function/partition, not aggregate volume. A lifter patch treating the relevant branch as a tail call reduced the measured nginx path from 6h33m to 16m20s.

The object cache and largest-first scheduling reduce repeated and tail costs. A global queue across all programs remains desirable because per-program `-P $(nproc)` leaves cores idle at each program's tail. Reachability pruning before lifting is the largest projected multiplier, but must be implemented as a lifter keep-list rather than by hiding symbols.

Runtime throughput initially varied wildly between identical nginx runs. Timestamping existing scheduler trace first localized function-table rebuilding, then direct removal isolated the arena copies. Swapping buffers reduced the measured pair from about 37 ms to 0.003 ms and brought four-worker nginx to roughly 1.05-1.19 s for 200 requests. Bounded snapshots are a separate, opt-in capacity mechanism: PostgreSQL's measured dirty set was at most 6 MiB and eight concurrent clients peaked at 948 MiB.

### Current translation critical path

The lift was first made deterministic, then accelerated in the lifter itself.
`AArch64TraceManager` stored executable bytes in a per-byte hash map;
`patches/0038` replaced it with sorted section ranges and a cached hit, improving
the lift 2.8-3.0x with byte-identical IR. `patches/0040` added word reads and a
block-membership set for another 1.9x. The remaining BTI landing-pad scan was
measured at 17.6% of the lift and is still open.

Codegen remains tail-bound by individual functions, especially glibc
`__vfscanf_internal`. Partition byte size predicts cost poorly, and lowering the
optimization level made that function slower and much larger. Library-scoped
partition reuse therefore attacks repeated compilation rather than pretending a
scheduler can divide an indivisible function.

For a program whose libraries are already compiled, the measured phase ordering
is `elflift` 32%, `ecv-split` 24%, internalize 15%, `llvm-link` 13%,
namespace-object 9%, and codegen 6%. The next broad optimization is to collapse
the four parse-and-reserialize whole-module passes. Lifting each library once is
the next new mechanism to investigate.

## Files

- `internal/translate/`: Object cache and scheduling.
- `patches/`: Lifter performance fixes.
- `runtime/src/context.rs`: Process-state switching and table loading.
- `e2e/`: Repeatable closure and network benchmarks.

## Test Coverage

Performance changes are paired with correctness E2E runs. Regression guards were neutralized by temporarily breaking the behavioral fix and confirming the intended failure, not merely a compile error.

## Pitfalls

- Measure inputs or `ps`, not only completed objects.
- Refute a cost hypothesis by removing the suspected work and re-measuring.
- Localize to a statement before naming a cause.
- Report variance rather than averaging it away or selecting a clean run.
- Timestamp an existing trace before adding instrumentation.
- A count of entries passed to `Lift` is not a count of functions emitted by trace expansion.
- Recompute the phase ordering after an optimization; removing one bottleneck promotes the work behind it.

## Consolidated Update: Current Translation Critical Path

Patch 0050 replaced overlapping per-function BTI scans with one index while preserving the original 512 KiB answer. It improved busybox 2.29x, bash 1.96x, and apt-get 2.06x with byte-identical bitcode; branch-protected PostgreSQL was unchanged. The older 17.6% estimate predated patch 0040 and is superseded.

Whole-module preparation was mostly parse cost: an empty `opt` round trip took 4.999 s while `internalize,globaldce` took 5.057 s. Merged preparation reduced four passes from 18.45 s to 6.0 s. Library cost must be measured by IR volume, not symbol count: PostgreSQL is 77.3% library symbols but only 54.7% library IR.

A whole-run cache A/B initially read 671 s on and 592 s off, but the identical first-program codegen differed by 60 s. Phase-local measurement showed caching saved 17.13 s of lift and added 1.03 s of preparation. The contrary wall-clock explanation is superseded. `ecv-prepare-split`, at 17.32 s of a 57.09 s later-program translation, is now the largest term.

A pass/fail gate can detect a reuse regression but cannot localize it. Diff keys, merged modules, and partitions before running another expensive gate.

## Consolidated Update: Browser Runtime Measurements and Rejected Clock Designs

The measured Alpine nginx fixture translates in about 351 seconds; the remembered 6.5-hour figure has another scope. Guest monotonic time now has a monotonic source and a clock-step guard.

A vDSO was rejected structurally. A clock cache improved a microbenchmark from 208 to 75 ns per call but did not move nginx. The earlier 1,144,644-read count came from a broken listener loop; a healthy request makes six. Verify system health before optimizing a surprising measurement.
