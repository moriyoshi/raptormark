# Ecvisor Runtime Synthesis

## Summary

Ecvisor supervises fused Linux userspace inside one Wasm module without a guest
kernel. Filesystem state, Linux-like syscalls, cooperative processes, networking,
and proposal-free control flow are implemented explicitly for both glibc and
musl.

## Included Documents

| Document | Focus |
|----------|-------|
| [ecvisor-process-and-thread-model.md](./ecvisor-process-and-thread-model.md) | Process state, arenas, fork, TLS, futexes, and scheduling |
| [ecvisor-vfs-syscalls-and-networking.md](./ecvisor-vfs-syscalls-and-networking.md) | RFS, timed waits, sockets, guest AF_UNIX, and shared files |
| [wasm-runtime-and-oci-compatibility.md](./wasm-runtime-and-oci-compatibility.md) | Wasm 2.0, OCI packaging, and released shim behavior |

## Stable Knowledge

- Ecvisor inherits no host kernel, filesystem, or environment. Guest-visible state comes from RFS, metadata, runtime configuration, and implemented syscalls.
- Suspension uses an ordinary early return. Exception handling and asyncify must not return to the emitted module.
- Arena buffers were once swapped rather than copied, which took the measured switch from about 37 ms to 0.003 ms. That path (`Arena::swap_with` + `adopt_shared_from`) is no longer on any switch; bounded snapshots superseded it. The functions are kept, but only their tests call them.
- Bounded snapshots -- writable loads, used brk/mmap/stack, and TLS -- are the ONLY scheme since 2026-08-22. They were opt-in, then the default, and then the environment gate was REMOVED; there is no full-buffer alternative to select. Allocation must zero-fill, and cross-program switches must materialize and record the correct image.
- The one remaining producer of a full snapshot is a **multi-threaded group**, decided by `is_multithreaded` and not by any flag: a bounded range set derives from one stack pointer, which says nothing about a sibling thread's live stack. `SnapshotData::Full` and `Arena::snapshot` therefore stay; deleting them breaks threads. `Arena::restore_in_place` must be EXHAUSTIVE over both variants -- an `if let ... Bounded` there restored no memory for a process forked from a thread and shipped a silent corruption.
- Removing the gate cost the validation oracle. `RAPTORMARK_ECV_SNAPCHECK` compared the live arena against the incoming process's full buffer, and single-threaded processes no longer have one; `Arena::bytes_differing_outside` returns `Option<SnapDiff>` and the probe prints `NO-ORACLE` rather than a fabricated `miss=0`. The stack-below-`sp` question (up to 1,307 bytes, argued dead under AAPCS64) is ACCEPTED, not closed, and is now unsettleable by differential because there is no other way to run.
- Shared mappings track mapper pid sets and kind-specific lifetime. Anonymous, named file-backed, and SysV mappings do not share one reclamation condition.
- File-backed shared mappings are reclaimed according to write history, not pathname. Read-only mappings can be recycled while their backing file exists; a region ever made writable retains its divergent bytes while the name resolves.
- Futex, epoll, and poll waits retain one absolute deadline across spurious wakes. An all-blocked live process set is a deadlock, not success.
- Blocking behavior follows the guest descriptor. Non-blocking socket operations return `EAGAIN` or `EINPROGRESS`; resumable host states must not collapse into permanent Linux errors.
- glibc and musl differ in TLS, thread pointer, TID, symbols, and page assumptions. `.ecv.musltp` carries musl's derived layout.
- Named AF_UNIX sockets live inside ecvisor and the guest VFS. PostgreSQL also requires `ppoll`, guest-local send/recv, and one shared regular-file buffer per path.
- Default Term and Core signal actions retire a process, default Ign does not prevent blocking, Stop remains deliberately unmodelled, and SIGKILL is unconditional. Parent-visible `wait4` state preserves signal death separately from `_exit(128 + signo)`.
- `mmap` rejects zero length with `EINVAL` and checked page-alignment overflow with `ENOMEM`. These are independent guards because every overflowing length rounds to zero.
- Released containerd/WasmEdge runs the proposal-free module. Wasmtime is blocked separately by unconditional WasmEdge socket imports.

## Operational Guidance

Diagnose from observed syscalls, scheduler traces, call history, and memory state.
Timestamp existing traces, then remove suspected work to test causality. Verify
Linux return conventions, preserve guest flags separately from non-blocking host
implementation details, map WASI errors by operation, and inspect lifted
intrinsics with `llvm-nm`.

Use focused guests and real applications. For snapshots, make process state
differ across switches. `RAPTORMARK_ECV_SNAPCHECK=1` now validates structural
range invariants; it reports `NO-ORACLE` for single-threaded bounded snapshots
because the selectable full-buffer comparison path no longer exists.

## Files

- `runtime/src/context.rs`: Process state, arenas, snapshots, and scheduling.
- `runtime/src/sys.rs`: Linux syscall surface and Wasm imports.
- `runtime/src/vfs/`: Guest VFS and socket nodes.
- `runtime/src/intrinsics.rs`: Remill/ecvisor intrinsic boundary.
- `internal/rootfs/`, `internal/fuse/`: RFS and metadata producers.

## Tests

The current host Rust count lives only in `.agents/docs/QUALITY_GATE.md`. Also
type-check the shipping target and use env-gated E2E coverage for behavior.
Timer, non-blocking, UDS, shared-file, signal-status, mmap, nginx, PostgreSQL,
and released-shim tests observe different boundaries.

```sh
cargo fmt --manifest-path runtime/Cargo.toml --check
cargo test --manifest-path runtime/Cargo.toml
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1
RAPTORMARK_E2E=1 RAPTORMARK_BUILDER=raptormark-builder:<tag> \
  RAPTORMARK_OBJECT_CACHE="$PWD/.agents-workspace/objcache" \
  go test ./e2e/ -v -timeout 60m
```

## Pitfalls

- A clean link can select a silent remill stub.
- Host non-blocking sockets do not imply non-blocking guest descriptors.
- Re-arming a relative timeout after a wake extends the deadline.
- A shared mapping in a private arena corrupts another process after a switch.
- A `SnapshotData::Full` value is not an empty bounded snapshot. Dispatch it
  to full restoration or fork-from-thread silently resumes with another
  process's memory.
- Partial `munmap` of shared mappings remains unsupported, and shared
  `mremap` growth can silently turn a mapping private; both remain open work.
- Direct WasmEdge execution does not prove released-shim compatibility.
## Refresh: Diagnostics, Memory Syscalls, and Split Hosts

A watchpoint sample belongs to its PID; a context switch installs another arena image and starts a new baseline. The memory-related syscall contracts are distinct: `membarrier` succeeds under cooperative single-thread execution, `sysinfo` reports granted memory, `MADV_DONTNEED` and `MADV_REMOVE` zero contents, and `mremap` moves growth rather than guessing adjacent space is free.

The side-module protocol has run, but no shipping host owns it. Node's WASI binding breaks after linear-memory growth and lacks WasmEdge sockets; WasmEdge cannot instantiate the module set. Multi-module support therefore includes an embedder, host imports, lifecycle, and lost cross-boundary AOT inlining.

## Refresh: The Address Budget, and Why a Reservation Is Not Free Here

Measured across three independent guests on 2026-08-22, and recorded together because they are one constraint wearing five hats.

**A mapping IS address space in a fixed linear memory, so a reservation costs exactly what an allocation costs.** Natively the opposite holds: Linux commits lazily, so reserving address space you never touch is nearly free. Ruby's shape cache is the measurement that makes the gap concrete -- `/proc/self/smaps` reports **RSS 28 kB against a 384 MiB mapping**, a ratio of about 14,000 to 1. Every runtime that speculatively reserves address space pays full price under ecvisor and nothing natively, and nothing in the guest can tell it has crossed that line.

The budget: `MEMORY_ARENA_SIZE` is 384 MiB, and the private mmap window is `MMAP_START_VMA 0x1000_0000 .. MMAP_END_VMA 0x1600_0000`, i.e. **96 MiB**, which the shared window also draws from. Four consequences observed the same day, none of them a bug in the guest:

- **Ruby's object-shape redblack cache asks for 402,653,184 bytes** -- `REDBLACK_CACHE_SIZE * sizeof(redblack_node_t)` = `(0x80000 * 32) * 24`, which is *exactly* `MEMORY_ARENA_SIZE`. It can never succeed at this arena size. Ruby degrades gracefully (`shape_cache = NULL`, `cache_size` preset so `redblack_new` returns `LEAF`), costing a measured **2.7x on ivar reads for objects with >= 10 ivars** and nothing at 14.
- **Ruby's OTHER startup mapping is fatal**: `SHAPE_BUFFER_SIZE * sizeof(rb_shape_t)` = 20,971,520, issued immediately before, calls `rb_memerror()` on failure. It succeeds today with roughly **74 MiB** of window left -- ruby boots on margin, and anything raising arena pressure earlier turns a working guest into one that cannot start.
- **YJIT reserves 128 MiB into a 96 MiB window.** Arithmetic, not pressure: `rb_yjit_reserve_addr_space` walks `MAP_FIXED_NOREPLACE` hints downward, fails, falls back hintless, and ruby aborts with its own `[BUG] mmap failed`. No decoder patch and no arena tuning short of resizing the window can change it.
- **PostgreSQL's shared window and the private mmap arena are the same 96 MiB**, so a large `MAP_SHARED` starves malloc's mmap fallback.

Read-only locale mappings once made the last case much worse: the path-exists
reclaim rule pinned 3,211,264 bytes and moved `shm_top` from `0x16000000` to
`0x10fd0000`. Tracking monotonic writability releases those mappings while
preserving named writable PostgreSQL DSM across backends.

**Operational reading.** ❌ Do not treat any one of these as a guest quirk. The pattern to expect on a NEW image is: a speculative reservation sized from a compile-time constant, succeeding natively and refused here, with the guest either degrading quietly (ruby's cache), dying loudly (YJIT), or dying at a point that looks unrelated (`rb_memerror`). ⚠️ When triaging a new guest's mmap failure, get the SIZE and its provenance before touching the arena -- ruby's 402,653,184 collides with `MEMORY_ARENA_SIZE` by coincidence, not because the guest read anything of ours, and reading it the other way sends the investigation into the runtime instead of into the guest's own arithmetic.
