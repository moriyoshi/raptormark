# Ecvisor Process and Thread Model

## Summary

Ecvisor multiplexes guest processes and threads inside one Wasm instance by saving and restoring process state around cooperative suspension. Correct behavior depends on explicit TLS, thread identity, futex, fork, and scheduler models rather than host-kernel inheritance.

## Key Facts

- Guest suspension now uses an ordinary early return and requires no Wasm exception-handling proposal.
- Futex waits track blocked state and deadlines; an all-blocked process set is a deadlock, not successful exit.
- `FUTEX_WAIT` timeouts are relative; `FUTEX_WAIT_BITSET` timeouts are absolute.
- musl's initial TID cannot be zero, and its thread-pointer layout is conveyed through `.ecv.musltp`.
- IFUNC/TLS correctness must be established before blaming pthread or atomic implementations.
- Forked workers need independent replayable process state while sharing the module's lifted code.
- Process arenas are swapped rather than copied; bounded snapshots are an opt-in alternative for high concurrency.

## Details

The OpenSSL deadlock investigation first exposed missing futex timeout semantics and a scheduler path that exited zero when all processes were blocked. Tracing the actual guest call history then showed that the apparent condvar problem originated in incorrect initialization caused by upstream fused-symbol handling, not by a generic pthread defect. This is the model for runtime diagnosis: identify the exact wait object and calling statement before naming the subsystem.

TLS, thread-specific data, atomic operations, and thread identity were separately probed. The musl path required a derived thread-pointer layout and a nonzero initial TID. Those details are image/runtime ABI, not interchangeable with glibc assumptions.

Fork and worker scheduling save registers, stacks, memory arenas, and dispatch-related state. Rebuilding function dispatch tables was first removed from the switch path, then the two 384 MiB arena copies were eliminated by swapping the live and suspended buffers. The measured switch fell from about 37 ms to 0.003 ms.

Shared mappings cannot travel with a private arena. Each shared segment records
its mapper pid set, and reclamation depends on its kind: anonymous mappings die
with the last mapper, named file-backed mappings also require unlink, and SysV
segments require `IPC_RMID` plus the last detach. `ShmWindow` allocates best-fit
from coalesced holes and absorbs free space back into its bump frontier.

Bounded snapshots -- the ONLY scheme since 2026-08-22, when the environment gate
that used to select between the two was removed outright -- store only
writable PT_LOAD ranges, used brk and
private mmap extents, live stack, and TLS. The real PostgreSQL dirty set was a
median 2 MiB and maximum 6 MiB out of 384 MiB. Allocation must zero-fill,
cross-program switches must re-materialize the correct image, and TLS extent
must come from `.ecv.tls` because fused multi-image binaries expose no useful
PT_TLS header. `materialized_prog` must be updated through one image-loading
path; a partially maintained tracker silently restores the wrong program.

⚠️ **A multi-threaded group is the exception, and it is a property of the
process rather than a mode.** A bounded range set is derived from ONE stack
pointer; a sibling thread's stack is live memory that pointer says nothing
about, so `EcvContext::snapshot_for` still returns `SnapshotData::Full` when
`is_multithreaded` is true. This is the only reason that variant, `Arena::snapshot`
and the `Full` arm of `Arena::restore_in_place` exist -- and the restore must be
EXHAUSTIVE over both variants. While it was an `if let SnapshotData::Bounded(..)`
a process forked from a thread carried a `Full` snapshot, had NO memory restored,
adopted the snapshot's brk/mmap bookkeeping anyway, and ran on whatever the
previously scheduled process had left in the live arena.

Removing the gate also removed the validation oracle. `RAPTORMARK_ECV_SNAPCHECK`
diffed the live arena against the incoming process's full buffer, which a
single-threaded process no longer has, so `Arena::bytes_differing_outside`
returns `Option<SnapDiff>` and the probe prints `NO-ORACLE` for those switches
rather than `miss=0`. The stack BELOW `sp` (up to 1,307 bytes observed on nginx,
argued dead under AAPCS64 because aarch64 has no red zone) was ACCEPTED rather
than closed, and is now unsettleable by differential: there is no unbounded run
left to compare against.

## Files

- `runtime/src/context.rs`: Process context and switching.
- `runtime/src/sys.rs`: Syscall and futex behavior.
- `runtime/src/`: Scheduler, TLS, thread, and fork support.
- `internal/fuse/`: TLS and musl thread-pointer metadata producers.

## Test Coverage

Host Rust tests cover the pure runtime modules and currently run 47 tests. The
shipping configuration is additionally checked for `wasm32-wasip1`, while
behavior remains covered by env-gated E2E tests including OpenSSL, multi-worker
nginx, fork reclamation, divergent memory, bounded snapshots, and PostgreSQL
concurrency.

## Pitfalls

- A blocked scheduler with live processes must not report success.
- Static call-graph names can misidentify stripped shared helpers; use runtime call history.
- A cost observed inside `load_current` is not localized until the individual statement is timed or removed.
- A shared mapping allocated from a process-private arena will overwrite another process after a switch.
- Bounded snapshots are the only process-switch scheme; the runtime flag and unbounded path were removed.

## Consolidated Update: Shared-VM Threads

`CLONE_THREAD` creates a sibling with the group's arena, fd table, cwd, and signal state. The sibling has its own pid/tid and thread pointer but the leader's tgid. It starts at the guest-provided child stack with an empty replay stack. Same-tgid switches skip arena swapping; `exit` retires one task, while `exit_group` retires the group. `CLONE_CHILD_CLEARTID`, `getpid` versus `gettid`, and leader-only `wait4` semantics are required for pthread join and reaping.

Bounded snapshots opt out for multithreaded groups because one stack pointer cannot bound every live thread stack. Guest pointer bounds are checked in 64-bit arithmetic before wasm32 truncation.

An exec from any thread now retires the old group and transfers leader identity to the caller. Signal posting resolves the group's single shared signal table. Fork-from-thread is covered by `e2e/threadproc_test.go`; exec from a non-leader remains unverified, and signal handlers still run only from `ppoll` or `epoll_pwait`.

The current Rust host suite has 58 tests, plus the `wasm32-wasip1` check and behavioral E2E coverage.

## Consolidated Update: PID-Aware Watchpoints

`RAPTORMARK_ECV_WATCH` must not compare live-arena samples across process owners. `WATCH_PREV` records the PID and `diag::watch_verdict` treats a first sample, owner change, or length change as a baseline; only changed bytes from the same owner fire. Guards cover both suppression across owners and detection within one owner.

## Consolidated Update: Re-entrant Scheduling and Host Signals

Blocking and returning hosts execute the same scheduler slices; returning hosts wait outside Wasm and call `ecv_run_slice`. Fork-by-replay works under this host: nginx forked workers, replaced one, and gracefully reloaded.

`ecv_signal` queues signals between slices. Default terminating and core actions retire the process, default-ignored signals do not prevent blocking, stop actions remain deliberately unmodelled, and SIGKILL is unconditional.

## Consolidated Update: Signals, Shared Unmaps, and Bounded Snapshots

Signal disposition is decided at delivery boundaries before a process can park. Term and Core terminate; default Ign remains non-terminating; Stop is not approximated as death; and SIGKILL cannot be caught, ignored, or blocked. Installing a SIGKILL or SIGSTOP action returns `EINVAL`, but querying the old action succeeds as on Linux. Requested masks drop those two bits before applying `how`.

A shared segment is released only when `munmap` covers its entire rounded extent. The old start-address-only predicate over-freed on a head unmap and could recycle live memory. Partial unmaps remain unsupported rather than pretending the mapper set tracks extents; shared `mremap` growth is also unresolved and remains in TODO.

Bounded snapshots became the default and the `RAPTORMARK_ECV_BOUNDED` switch was then removed. `SnapshotData::Full` remains as the required fallback for multithreaded groups, whose live stacks cannot be bounded by one stack pointer. `Arena::restore_in_place` (⚠️ NOT `restore_bounded`, which an earlier version of this sentence named and which has never existed) must dispatch `Full` to a full restore; treating it as an empty range restored nothing for fork-from-thread, while still adopting that process's brk/mmap bookkeeping, so a process forked from a thread ran on whatever the previously-scheduled process had left in the live arena and died calling through a null pointer. Its `match` over `SnapshotData` is deliberately TOTAL and exhaustive so it stays that way; the earlier `if let SnapshotData::Bounded(..)` is what shipped the corruption. Compare `swap_with`, whose opposite mismatch is a `fatal!` -- that half was loud, this half was silent, and only the silent half shipped a bug. The `Full` arm must also SKIP the live shared segments, because shared memory has one physical copy belonging to whoever is running and the incoming process's saved buffer holds a stale view of it; the `Bounded` arm needs no such care because `bounded_ranges` never covers the shared window. Restore now also clears memory outside the incoming ranges so switching from a larger snapshot cannot leak stale bytes into a smaller one. `RAPTORMARK_ECV_SNAPCHECK` is now a structural range checker, not an unavailable bounded-versus-full differential.
