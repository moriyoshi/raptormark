//! Diagnostic gates that are readable from every module, on every target.
//!
//! These live here rather than in `sys` for one structural reason: `sys` is
//! `#[cfg(target_arch = "wasm32")]` because it is built on WASI imports, while
//! `context` is not gated at all. `context` reads these flags in eleven places,
//! so the crate did not compile for the host at all and every host-side unit
//! test in it -- including the pure `vfs` and `boot` ones the `rlib` crate-type
//! exists to enable -- was unreachable. Nothing about a boolean flag needs wasm.
//!
//! The values are read from the environment ONCE at startup (`init_diag_flags`
//! in `sys`) into atomics, and only ever loaded afterwards. That is a fork-safety
//! requirement, not a micro-optimisation: `std::env::var` after a fork can hit
//! `lazy_lock::panic_poisoned`, whose panic handler re-reads the environment and
//! loops forever. A pre-initialised atomic touches no environment and cannot
//! poison. 0 = uninitialised, 1 = off, 2 = on.
//!
//! Every gate here is opt-in, so 0 reads as off. Where a gate's default matters
//! it is expressed once, as a pure function of the stored byte ([`census_on`]),
//! so it is a claim a host test can check rather than a property of whether
//! `sys::init_diag_flags` happened to have run.
//!
//! ⚠️ There is no gate here for bounded arena snapshots. They are the ONLY
//! snapshot scheme as of 2026-08-22 and the variable that used to select them
//! was removed; see [`snapcheck`] for what that cost the probe that validated
//! them, and `arena::SnapshotData` for the one case that still takes a full
//! snapshot (a multi-threaded group, decided by thread count and not by a flag).

use crate::trace::ecv_warn;
use core::sync::atomic::{AtomicU64, AtomicU8, Ordering};

static DEBUG_FLAG: AtomicU8 = AtomicU8::new(0);
static TRACE_FLAG: AtomicU8 = AtomicU8::new(0);
static LEGSP_FLAG: AtomicU8 = AtomicU8::new(0);
static SNAPSTAT_FLAG: AtomicU8 = AtomicU8::new(0);
static SNAPCHECK_FLAG: AtomicU8 = AtomicU8::new(0);
static FILETRACE_FLAG: AtomicU8 = AtomicU8::new(0);
static FDCHECK_FLAG: AtomicU8 = AtomicU8::new(0);
static TABLES_FLAG: AtomicU8 = AtomicU8::new(0);
static NONBLOCK_FLAG: AtomicU8 = AtomicU8::new(0);

// Call-site execution counter (RAPTORMARK_ECV_COUNTRET=<hex return address>).
// Counting one call site distinguishes "ran once, state corrupt" from "really
// ran N times", which end-state dumps cannot. 0 = off.
static COUNT_RET: AtomicU64 = AtomicU64::new(0);
static RET_HITS: AtomicU64 = AtomicU64::new(0);

// Guest call depth past which runaway recursion is reported
// (RAPTORMARK_ECV_MAXDEPTH), and the peak depth actually reached.
static MAX_DEPTH: AtomicU64 = AtomicU64::new(0);
static PEAK_DEPTH: AtomicU64 = AtomicU64::new(0);

// Default guest thread stack (RAPTORMARK_ECV_THREAD_STACK), seeded into libc
// because ld.so never ran to take it from RLIMIT_STACK.
static THREAD_STACK: AtomicU64 = AtomicU64::new(0);

/// ENOSYS and other advisory syscall diagnostics are logged only when
/// RAPTORMARK_ECV_DEBUG is set, keeping normal runs quiet (glibc probes
/// several unsupported syscalls at startup, all harmless).
pub(crate) fn debug_log() -> bool {
    DEBUG_FLAG.load(Ordering::Relaxed) == 2
}

/// Verbose per-syscall + hot-lookup VMA tracing, gated on RAPTORMARK_ECV_TRACE
/// (separate from RAPTORMARK_ECV_DEBUG so the ENOSYS advisory stays uncluttered).
/// Used to locate where a guest stalls (a syscall loop vs a pure lifted-code
/// spin). Diagnostic only.
pub(crate) fn trace_log() -> bool {
    TRACE_FLAG.load(Ordering::Relaxed) == 2
}

/// Wasm shadow-stack low-water probe gate (RAPTORMARK_ECV_LEGSP). Diagnostic.
#[inline]
pub(crate) fn legsp() -> bool {
    LEGSP_FLAG.load(Ordering::Relaxed) == 2
}

// "Some diagnostic that the PER-CALL hooks consult is enabled." One flag
// standing for five, so `_ecv_save_call_history` and `_ecv_func_epilogue` --
// which run on every guest BL and every return, 32,972 and 32,115 call sites in
// a linked bash module -- can test once and jump over the lot.
//
// It is a summary, never a source of truth: the cold path re-reads each
// individual gate, so a flag this misses can only cost the diagnostic, never
// change behaviour. Set once by `sys::init_diag_flags`, before any guest code or
// fork runs, and never changed after -- which is what makes it safe to treat as
// a constant on the hot path.
static HOT_SLOW: AtomicU8 = AtomicU8::new(0);

/// True when any per-call diagnostic is on. See [`HOT_SLOW`].
#[inline]
pub(crate) fn hot_slow() -> bool {
    HOT_SLOW.load(Ordering::Relaxed) == 2
}

/// Called once by `sys::init_diag_flags`, AFTER every individual gate is set.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_hot_slow(on: bool) {
    HOT_SLOW.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

/// Bounded-snapshot sizing probe (RAPTORMARK_ECV_SNAPSTAT). Reports, at each
/// context switch, how much of the 384 MiB arena a process could actually have
/// written -- the number that decides whether bounded snapshots are worth
/// building. Diagnostic only.
#[inline]
pub(crate) fn snapstat() -> bool {
    SNAPSTAT_FLAG.load(Ordering::Relaxed) == 2
}

/// Bounded-snapshot SAFETY probe (RAPTORMARK_ECV_SNAPCHECK). Verifies, at each
/// switch, that the bytes a bounded snapshot would NOT copy are already
/// identical between the two processes. O(arena) per switch; diagnostic only.
///
/// ⚠️ **It mostly has no oracle any more.** The comparison needs the incoming
/// process's FULL memory to compare the live arena against, and that existed
/// only because the full-buffer scheme kept one per process. With bounded
/// snapshots unconditional (2026-08-22) a single-threaded process stores ranges
/// and there is nothing to compare against, so the probe now says
/// `NO-ORACLE` for that switch instead of reporting `miss=0`. Reporting zero
/// would have been a fabricated pass -- it counts differences against a buffer
/// that does not exist -- and this tree has been bitten by exactly that shape
/// twice (`bbmiss insn=`, `undecoded_message`). See
/// `Arena::bytes_differing_outside`, which returns `Option<SnapDiff>` so the
/// distinction cannot be dropped by a caller.
#[inline]
pub(crate) fn snapcheck() -> bool {
    SNAPCHECK_FLAG.load(Ordering::Relaxed) == 2
}

/// NON-BLOCKING HOST (RAPTORMARK_ECV_NONBLOCK). Makes the in-process network
/// backend DECLINE to wait, so the scheduler returns `Idle` to its caller
/// instead of sleeping.
///
/// This is not a diagnostic switch -- it selects the shape a re-entrant host
/// needs. A browser cannot let the guest block at all: `Atomics.wait` is illegal
/// on the main thread, and in a worker a blocking host stalls the very event
/// loop that would deliver the readiness being waited for. The browser backend
/// will hard-code this behaviour; the flag lets the loopback profile take the
/// same path, which is what makes the re-entrant exports drivable before that
/// backend exists.
#[inline]
pub(crate) fn nonblocking() -> bool {
    NONBLOCK_FLAG.load(Ordering::Relaxed) == 2
}

/// Called once by `sys::init_diag_flags`.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_nonblocking(on: bool) {
    NONBLOCK_FLAG.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

/// Where a WASIX build looks for side modules, read once at startup.
///
/// ❗ IT MUST BE READ AT STARTUP, and the reason is not tidiness.
/// `std::env::var` after a fork can hit `lazy_lock::panic_poisoned`, whose panic
/// handler re-reads the environment and loops forever. `loader::wasix` called
/// `std::env::var` inside `request` -- i.e. inside a `dlopen` syscall, which is
/// exactly a post-fork path -- and `env_is_read_only_from_startup_paths` caught
/// it before it ever ran.
///
/// A `OnceLock` rather than an `AtomicU8`: the value is a path, and the flags
/// above are booleans. It is never written after `init_diag_flags`.
static SIDE_DIR: std::sync::OnceLock<String> = std::sync::OnceLock::new();

/// The side-module directory, or the default when unset.
///
/// The default is a path in the WASIX GUEST's filesystem -- wasmer maps host
/// directories in -- not in ecvisor's virtual rfs, which WASIX knows nothing
/// about.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn side_dir() -> &'static str {
    match SIDE_DIR.get() {
        Some(s) if !s.is_empty() => s.as_str(),
        _ => "/side",
    }
}

/// Called once by `sys::init_diag_flags`.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_side_dir(dir: String) {
    let _ = SIDE_DIR.set(dir);
}

pub fn count_ret() -> u64 {
    COUNT_RET.load(Ordering::Relaxed)
}

pub fn bump_ret_count() {
    RET_HITS.fetch_add(1, Ordering::Relaxed);
}

pub fn ret_hits() -> u64 {
    RET_HITS.load(Ordering::Relaxed)
}

/// In-memory FILE probe (RAPTORMARK_ECV_FILETRACE). Logs the lifecycle of the
/// shared `open_files` table -- every open, the reads served from it, and every
/// slot recycle -- with the pid, so a read can be traced back to the path the
/// descriptor actually names.
///
/// It exists for one question that end-state inspection cannot answer: a guest
/// that reads a FULL block from a one-block file did not read that file. The
/// table joins descriptors on path and recycles slots once `refs` hits zero, so
/// "which file did this index mean at this moment" is a property of history, not
/// of the final state. Diagnostic only.
#[inline]
pub(crate) fn filetrace() -> bool {
    FILETRACE_FLAG.load(Ordering::Relaxed) == 2
}

/// Called once by `sys::init_diag_flags`. Separate from `set_gates` on purpose:
/// that takes six positional bools already, and a seventh in the wrong position
/// enables the wrong gate silently.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_filetrace(on: bool) {
    FILETRACE_FLAG.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

/// `open_files` REFCOUNT probe (RAPTORMARK_ECV_FDCHECK). Recomputes what every
/// slot's count should be by counting the descriptors that actually reference
/// it, and reports any slot where the recorded count differs.
///
/// Runs after `openat`/`close`/`dup`/`dup3` AND at each context switch. The
/// syscall sites are the load-bearing ones, and that is a measured correction:
/// with the check at switches ALONE, a guest that dups, closes and churns
/// without yielding violates and restores the invariant between two switches,
/// and the probe stayed completely silent against a deliberately re-injected
/// `dup` bug. Moved to the syscalls, the same guest reports it on the FIRST dup.
///
/// This is the gate that would have caught the `dup` bug on its first guest.
/// The rule "a clone of an fd entry takes a reference" is expressed once, in
/// `EcvContext::retain_entry`, but nothing STRUCTURALLY prevents a fourth clone
/// site from forgetting to call it -- `OpenFile` is `Clone`, and `fork` needs it
/// to be. An invariant that cannot be enforced by the type system can still be
/// checked, and a count that is too LOW is the dangerous direction: it frees a
/// slot a live descriptor still names. Diagnostic only.
#[inline]
pub(crate) fn fdcheck() -> bool {
    FDCHECK_FLAG.load(Ordering::Relaxed) == 2
}

#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_fdcheck(on: bool) {
    FDCHECK_FLAG.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

/// Dispatch-table rebuild probe (RAPTORMARK_ECV_TABLES). Reports, at each
/// rebuild, the program, the table sizes in entries and BYTES, and how long the
/// rebuild took.
///
/// Its own gate rather than `debug_log` for a measurement reason: a rebuild
/// happens on every cross-program switch, and at 13.9 ms each it IS the switch,
/// so the probe has to be readable without the per-syscall volume that
/// `RAPTORMARK_ECV_DEBUG` and `_TRACE` bring with them. `[sched] switch` timing
/// is gated on `_TRACE` for exactly that reason and was unusable here.
#[inline]
pub(crate) fn tables() -> bool {
    TABLES_FLAG.load(Ordering::Relaxed) == 2
}

#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_tables(on: bool) {
    TABLES_FLAG.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

// ---------------------------------------------------------------------------
// Watchpoint and differential value tracer
// ---------------------------------------------------------------------------
//
// These moved here from `sys` so that every gate this runtime consults lives in
// ONE module, which is what `ecv_probe!` assumes: it expands `ecv_probe!(watch,
// ..)` to `if crate::diag::watch()`, so a probe whose gate lived elsewhere could
// not use it and kept a hand-written `if` and a raw `eprintln!`. `[watch]` and
// `[dtrace]` were the last two in that position.
//
// The split is deliberate and is not "everything to do with the probe". What
// belongs here is CONFIGURATION read once at startup; the watchpoint's previous
// window -- per-call mutable state, not a gate -- stays with the probe in
// `intrinsics`. Mixing the two here would undo the one property this module has:
// that nothing in it changes after `init_diag_flags`.

/// Watchpoint address (RAPTORMARK_ECV_WATCH=<hex vma>), 0 when unset. Sampled at
/// every guest call, so a change is attributed to the function that made the
/// call -- coarse, but enough to name the writer.
static WATCH_ADDR: AtomicU64 = AtomicU64::new(0);
/// Width of the watched window in bytes (RAPTORMARK_ECV_WATCHLEN, default 4).
/// Watching a whole object rather than one word shows the sequence of writes,
/// which is what distinguishes a bad value from a bad address.
static WATCH_LEN: AtomicU64 = AtomicU64::new(4);

/// True when the watchpoint is armed. The gate `ecv_probe!(watch, ..)` expands
/// to, so the category, the prefix and the environment variable are one name.
#[inline]
pub(crate) fn watch() -> bool {
    WATCH_ADDR.load(Ordering::Relaxed) != 0
}

#[inline]
pub(crate) fn watch_addr() -> u64 {
    WATCH_ADDR.load(Ordering::Relaxed)
}

#[inline]
pub(crate) fn watch_len() -> usize {
    WATCH_LEN.load(Ordering::Relaxed) as usize
}

/// What a watchpoint sample means, given the previous one.
///
/// # Why the previous sample's PID matters
///
/// The probe reads the LIVE arena, and under the full-snapshot scheme a context
/// switch swaps that buffer wholesale. So the same VMA holds a different
/// process's memory after a switch, and a byte-for-byte comparison across one
/// reports a change that never happened. During the double-free hunt this
/// attributed a write to dash's `__libc_early_init` that was only the swap --
/// the probe's output was not merely noisy, it named a culprit.
///
/// Recording the owning pid makes the two cases distinguishable: a differing pid
/// means the window is not comparable and the sample becomes a new baseline,
/// silently. Only a change WITHIN one process is a change.
///
/// A pure rule rather than state, which is why it sits here with the watchpoint's
/// address and length while `WATCH_PREV` stays in `intrinsics` -- this module's
/// invariant is that nothing in it CHANGES after `init_diag_flags`, and a
/// function that reads its arguments does not.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum WatchVerdict {
    /// Record `now` and say nothing: there is no comparable previous sample.
    Baseline,
    Unchanged,
    /// A real change within one process. Report it.
    Changed,
}

pub(crate) fn watch_verdict(prev: Option<(u32, &[u8])>, pid: u32, now: &[u8]) -> WatchVerdict {
    match prev {
        None => WatchVerdict::Baseline,
        // The arena swapped under us; these bytes belong to someone else.
        Some((owner, _)) if owner != pid => WatchVerdict::Baseline,
        // A window that changed size cannot be compared either. Not reachable
        // today -- the length is startup configuration -- but the alternative is
        // a zip() that silently compares a prefix.
        Some((_, was)) if was.len() != now.len() => WatchVerdict::Baseline,
        Some((_, was)) if was != now => WatchVerdict::Changed,
        Some(_) => WatchVerdict::Unchanged,
    }
}

/// Called once by `sys::init_diag_flags`. A zero length means "unset", not "watch
/// nothing", so it selects the default rather than disabling the window.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_watch(addr: u64, len: u64) {
    WATCH_ADDR.store(addr, Ordering::Relaxed);
    WATCH_LEN.store(if len == 0 { 4 } else { len }, Ordering::Relaxed);
}

// Differential value tracer: when RAPTORMARK_ECV_DTRACE_LO/_HI (hex guest VMAs)
// bound a non-empty range, the call prologue and epilogue log the arguments and
// the return value of every call SITE whose return address falls in [LO,HI).
// Keyed on the return address because the epilogue knows the call site, not the
// callee.
static DTRACE_LO: AtomicU64 = AtomicU64::new(0);
static DTRACE_HI: AtomicU64 = AtomicU64::new(0);
// Optional PID floor: only calls made while the current process's pid >= this
// are logged. Postgres runs its query in a forked backend (pid >= 8) while the
// postmaster and aux processes (pids 1-7) do all the boot noise, so
// RAPTORMARK_ECV_DTRACE_MINPID=8 isolates one query's call trace. 0 = no floor.
static DTRACE_MINPID: AtomicU64 = AtomicU64::new(0);
// Bytes to hexdump at a traced call's x1 argument, and at its x0 argument, so
// e.g. the source tuple and the destination Page handed to
// PageAddItemExtended can both be inspected byte-for-byte. 0 = no dump.
static DTRACE_DUMP: AtomicU64 = AtomicU64::new(0);
static DTRACE_DUMPX0: AtomicU64 = AtomicU64::new(0);
// Extra register context (RAPTORMARK_ECV_DTRACE_REGS): a traced call also prints
// x19-x28.
static DTRACE_REGS: AtomicU8 = AtomicU8::new(0);

/// True when the differential tracer is armed. Two relaxed loads, no env access.
#[inline]
pub(crate) fn dtrace() -> bool {
    DTRACE_HI.load(Ordering::Relaxed) > DTRACE_LO.load(Ordering::Relaxed)
}

/// The tracer's range [LO,HI), or None when unset or empty.
#[inline]
pub(crate) fn dtrace_range() -> Option<(u64, u64)> {
    let lo = DTRACE_LO.load(Ordering::Relaxed);
    let hi = DTRACE_HI.load(Ordering::Relaxed);
    (hi > lo).then_some((lo, hi))
}

/// PID floor for the tracer, 0 when unset.
#[inline]
pub(crate) fn dtrace_minpid() -> u64 {
    DTRACE_MINPID.load(Ordering::Relaxed)
}

/// Byte count to hexdump at a traced call's x1 pointer, 0 when unset.
#[inline]
pub(crate) fn dtrace_dump() -> u64 {
    DTRACE_DUMP.load(Ordering::Relaxed)
}

/// Byte count to hexdump at a traced call's x0 pointer, 0 when unset.
#[inline]
pub(crate) fn dtrace_dumpx0() -> u64 {
    DTRACE_DUMPX0.load(Ordering::Relaxed)
}

/// Whether a traced call also prints x19-x28.
///
/// The argument registers suffice when the value wanted IS an argument. Often it
/// is not: a diagnostic deep inside libc reports on a value the compiler parked
/// in a callee-saved register long before, and the only place the runtime can
/// observe it is the call to the reporting function. glibc's `_int_free` holds
/// `nextchunk` in x22 when it calls `malloc_printerr`, and `free` reaches
/// `_int_free` by TAIL call, so there is no argument-bearing call site at all.
///
/// ⚠️ THIS USED TO READ THE ENVIRONMENT ON EVERY TRACED CALL
/// (`std::env::var("RAPTORMARK_ECV_DTRACE_REGS").is_ok()`), which is the exact
/// hazard the note at the top of this file exists for: `std::env::var` after a
/// fork can hit `lazy_lock::panic_poisoned`, whose panic handler re-reads the
/// environment and loops forever. It sat behind the dtrace gate, so it could only
/// bite a run with the tracer armed -- which is precisely the run you would be
/// doing while chasing a fork hang.
#[inline]
pub(crate) fn dtrace_regs() -> bool {
    DTRACE_REGS.load(Ordering::Relaxed) == 2
}

/// Called once by `sys::init_diag_flags`, which is where env reading belongs:
/// `sys` is the wasm-gated module and this one must compile for the host.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_dtrace(lo: u64, hi: u64, minpid: u64, dump: u64, dumpx0: u64, regs: bool) {
    DTRACE_LO.store(lo, Ordering::Relaxed);
    DTRACE_HI.store(hi, Ordering::Relaxed);
    DTRACE_MINPID.store(minpid, Ordering::Relaxed);
    DTRACE_DUMP.store(dump, Ordering::Relaxed);
    DTRACE_DUMPX0.store(dumpx0, Ordering::Relaxed);
    DTRACE_REGS.store(if regs { 2 } else { 1 }, Ordering::Relaxed);
}

/// Called once by `sys::init_diag_flags`, before any guest code or fork.
/// `sys` is wasm-only, so the host build sees no caller.
///
/// Every flag here changes OUTPUT and is opt-in. The two that change BEHAVIOUR
/// -- [`set_bounded`] and [`set_undec_census`] -- have their own setters.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_gates(debug: bool, trace: bool, legsp: bool, snapstat: bool, snapcheck: bool) {
    let v = |on: bool| if on { 2 } else { 1 };
    DEBUG_FLAG.store(v(debug), Ordering::Relaxed);
    TRACE_FLAG.store(v(trace), Ordering::Relaxed);
    LEGSP_FLAG.store(v(legsp), Ordering::Relaxed);
    SNAPSTAT_FLAG.store(v(snapstat), Ordering::Relaxed);
    SNAPCHECK_FLAG.store(v(snapcheck), Ordering::Relaxed);
}

/// Called once by `sys::init_diag_flags`. Host build has no caller.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_count_ret(v: u64) {
    COUNT_RET.store(v, Ordering::Relaxed);
}

/// Guest call depth past which recursion is reported runaway. Overridable with
/// `RAPTORMARK_ECV_MAXDEPTH`; 0 selects the default.
///
/// It is a heuristic, and its premise is falsifiable: an INTERPRETER's stack is
/// as deep as the program it is running. The previous limit of 96 was chosen
/// when nginx's config parser was the deepest thing measured, and CPython
/// exceeded it while importing a module -- a guest doing exactly what it is
/// supposed to, killed by a diagnostic. `RAPTORMARK_ECV_DEBUG` reports the peak
/// depth a run reached, so this can stay a measurement.
#[inline]
pub(crate) fn max_depth() -> usize {
    let v = MAX_DEPTH.load(Ordering::Relaxed);
    if v == 0 {
        DEFAULT_MAX_DEPTH
    } else {
        v as usize
    }
}

/// The default, chosen against both ends rather than picked.
///
/// FLOOR: python:3-slim peaks at 237 lifted frames running
/// `print(__import__("json")...)`, measured with this reporting. The old 96
/// killed it. Two orders of magnitude of headroom over the deepest thing
/// actually measured is what keeps the next interpreter from re-opening this.
///
/// CEILING: the guest stack region is 32 MiB (`MMAP_END_VMA`..`STACK_TOP_VMA`),
/// so 16384 frames would have to average 2 KiB each to run off it first. Real
/// frames are tens to a few hundred bytes, so the report still arrives before
/// the wasm trap it exists to replace -- which is the whole point, because that
/// trap carries wasm function indices and no way back to a guest address.
pub(crate) const DEFAULT_MAX_DEPTH: usize = 16384;

/// The default stack a guest thread gets, in bytes. Overridable with
/// `RAPTORMARK_ECV_THREAD_STACK`; 0 selects the default.
///
/// WHY IT IS NOT THE PLATFORM DEFAULT. glibc takes this from RLIMIT_STACK, and
/// this runtime reports RLIM_INFINITY, for which glibc substitutes its
/// architecture default -- 32 MiB on aarch64. The guest's whole mmap window is
/// 96 MiB (`MMAP_START_VMA`..`MMAP_END_VMA`), so the platform answer would let a
/// process create TWO extra threads before the window was gone.
///
/// 1 MiB instead: ~90 threads in the same window, and 64x above aarch64's
/// PTHREAD_STACK_MIN of 16 KiB. A guest that genuinely needs deep thread stacks
/// raises it rather than discovering the ceiling as a corruption.
pub(crate) fn thread_stack_size() -> u64 {
    let v = THREAD_STACK.load(Ordering::Relaxed);
    if v == 0 {
        DEFAULT_THREAD_STACK
    } else {
        v
    }
}

pub(crate) const DEFAULT_THREAD_STACK: u64 = 1024 * 1024;

/// Called once by `sys::init_diag_flags`. Host build has no caller.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_thread_stack(v: u64) {
    THREAD_STACK.store(v, Ordering::Relaxed);
}

/// Called once by `sys::init_diag_flags`. Host build has no caller.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_max_depth(v: u64) {
    MAX_DEPTH.store(v, Ordering::Relaxed);
}

/// Records a guest call depth and returns the peak seen so far. Used to report
/// what a guest actually needs, so the limit above is a measurement rather than
/// a guess.
#[inline]
pub(crate) fn note_depth(d: usize) -> usize {
    let prev = PEAK_DEPTH.load(Ordering::Relaxed);
    if d as u64 > prev {
        PEAK_DEPTH.store(d as u64, Ordering::Relaxed);
        return d;
    }
    prev as usize
}

/// The deepest guest call stack seen this run.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn peak_depth() -> usize {
    PEAK_DEPTH.load(Ordering::Relaxed) as usize
}

// ---------------------------------------------------------------------------
// Address-budget high-water marks
// ---------------------------------------------------------------------------
//
// ❗ WHY THESE EXIST. The arena's 384 MiB is split into four fixed regions, and
// `.agents/docs/TODO.md` carries a ⭐ decision about one of them: the mmap window
// is 96 MiB shared between private `mmap` and `MAP_SHARED`, so a large
// `shared_buffers` starves malloc's fallback.
//
// The options considered for three sessions were grow the arena, split the
// window, make reservations lazy, or accept the ceiling -- all expensive. None
// noticed that the window has a 96 MiB NEIGHBOUR: the brk heap
// (`0x0A000000`-`0x10000000`). If real guests leave most of that dead, lowering
// `MMAP_START_VMA` is a CONSTANT change costing no linear memory and no
// memory-model change.
//
// ⚠️ That was an ARGUMENT -- glibc malloc sends large allocations to mmap and
// only small ones to brk -- and this file's own rule is to refute by removal
// rather than by reasoning. `brk_cur` and `mmap_cur` were tracked and NOTHING
// reported them, so the argument could not be checked. These make it a number.
//
// Peaks, not current values: the question is the WATERMARK a guest reaches, and
// a sample at exit would miss a spike that was freed.
static PEAK_BRK: AtomicU64 = AtomicU64::new(0);
static PEAK_MMAP: AtomicU64 = AtomicU64::new(0);
static PEAK_SHM: AtomicU64 = AtomicU64::new(0);

/// Records the brk break and the two mmap-window edges, in BYTES USED.
///
/// Called from the arena's own mutators rather than sampled, so a transient peak
/// cannot be missed. Three relaxed compare-and-stores on paths that already do
/// real work; `brk` and `mmap` are syscalls, not hot leaves.
#[inline]
pub(crate) fn note_address_use(brk_used: u64, mmap_used: u64, shm_used: u64) {
    for (cell, v) in [
        (&PEAK_BRK, brk_used),
        (&PEAK_MMAP, mmap_used),
        (&PEAK_SHM, shm_used),
    ] {
        if v > cell.load(Ordering::Relaxed) {
            cell.store(v, Ordering::Relaxed);
        }
    }
}

/// `(brk, private mmap, shared)` high-water marks in bytes.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn peak_address_use() -> (u64, u64, u64) {
    (
        PEAK_BRK.load(Ordering::Relaxed),
        PEAK_MMAP.load(Ordering::Relaxed),
        PEAK_SHM.load(Ordering::Relaxed),
    )
}

// ---------------------------------------------------------------------------
// Startup-read settings that are not logging gates
// ---------------------------------------------------------------------------
//
// The module's contract is "read from the environment ONCE, before any fork".
// That contract is about the ENVIRONMENT, not about logging, so a switch that
// happens not to print anything belongs here for exactly the same reason a gate
// does. The two below were the last `std::env::var` calls on post-fork paths:
// `RAPTORMARK_ECV_NO_FILE_SHM` sat in the `mmap` syscall handler and
// `RAPTORMARK_ECV_SCAN` in the deadlock reporter, both of which run only after
// the guest has been running -- and, in the deadlock reporter's case, only after
// every process is blocked, which requires at least one fork to have happened.
// Same defect class as `dtrace_regs` above, and the same fix.

/// `RAPTORMARK_ECV_NO_FILE_SHM`: make file-backed `MAP_SHARED` fail with ENODEV.
///
/// A bisect switch, not a diagnostic -- with it set, a guest that has a fallback
/// takes it (PostgreSQL drops from posix dynamic shared memory to sysv), which
/// turns "is the shm code responsible" into a measurement instead of an
/// argument. It prints nothing, which is why it was not swept up with the gates.
static NO_FILE_SHM: AtomicU8 = AtomicU8::new(0);

#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
#[inline]
pub(crate) fn no_file_shm() -> bool {
    NO_FILE_SHM.load(Ordering::Relaxed) == 2
}

/// Called once by `sys::init_diag_flags`. Host build has no caller.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_no_file_shm(on: bool) {
    NO_FILE_SHM.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

/// `RAPTORMARK_ECV_INLINE_CH=1`: run the inlined call history (elfconv patch
/// 0060) if the module was also built for it. Unlike every other flag here, the
/// value matters -- `=0` is off, not "set".
///
/// See `context::inline_call_history_enabled` for the other two conditions. It
/// is consulted once, from `EcvContext::new`, so the env read it replaces was
/// pre-fork and safe; it moved because that safety was a comment about call
/// order rather than a property of the code.
static INLINE_CH: AtomicU8 = AtomicU8::new(0);

#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
#[inline]
pub(crate) fn inline_ch() -> bool {
    INLINE_CH.load(Ordering::Relaxed) == 2
}

/// Called once by `sys::init_diag_flags`. Host build has no caller.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_inline_ch(on: bool) {
    INLINE_CH.store(if on { 2 } else { 1 }, Ordering::Relaxed);
}

/// `RAPTORMARK_ECV_SCAN=off:min:max[,off:min:max...]`, parsed at startup.
///
/// The deadlock reporter walks the mmap arena for every 8-byte-aligned address
/// whose `u32` at each `off` lies in `[min,max]`. Storing the PARSED clauses
/// rather than the spec string is what keeps the promise that nothing in this
/// module touches the environment or allocates after `init_diag_flags`.
///
/// `static mut` behind `addr_of` accessors, as `intrinsics::WATCH_PREV` does:
/// the runtime is a single-threaded cooperative scheduler, and this is written
/// exactly once, before any guest code runs.
static mut SCAN_CLAUSES: Vec<(u64, u32, u32)> = Vec::new();

/// The scan clauses, empty when `RAPTORMARK_ECV_SCAN` was unset or unparseable.
pub(crate) fn scan_clauses() -> &'static [(u64, u32, u32)] {
    unsafe { &*core::ptr::addr_of!(SCAN_CLAUSES) }
}

/// Called once by `sys::init_diag_flags`. Host build has no caller.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_scan(spec: &str) {
    unsafe { *core::ptr::addr_of_mut!(SCAN_CLAUSES) = parse_scan(spec) }
}

/// Parses the scan spec. Pure, so the grammar is testable on the host -- it was
/// previously inline in the deadlock handler, reachable only by deadlocking a
/// guest with the variable set.
///
/// A clause is exactly three decimal fields; anything else is dropped, and a
/// spec that yields no clause reads as "off". Dropped rather than rejected
/// because this is a diagnostic switch whose only failure mode is printing
/// nothing.
///
/// ⚠️ ONE BEHAVIOUR CHANGE from the version inlined in the deadlock handler,
/// made when this became testable. That one dropped unparseable FIELDS and then
/// counted what survived, so `foo:1:2:3` kept `1:2:3` and was accepted as a
/// clause the operator did not write -- a silent field shift. Here an
/// unparseable field drops the whole clause. Nothing else about the grammar
/// moved: fields are still trimmed and still decimal, and every previously
/// valid spec parses identically.
pub(crate) fn parse_scan(spec: &str) -> Vec<(u64, u32, u32)> {
    spec.split(',')
        .filter_map(|c| {
            let f: Vec<Option<u64>> = c.split(':').map(|s| s.trim().parse().ok()).collect();
            match f[..] {
                [Some(off), Some(lo), Some(hi)] => Some((off, lo as u32, hi as u32)),
                _ => None,
            }
        })
        .collect()
}

/// Current wasm linear memory, in MiB.
///
/// The exact number, not an estimate: `memory.size` is what the host has
/// actually handed the module. Reported at fork and exit because the question
/// "why did a 384 MiB arena allocation fail when four of them should fit in
/// 4 GiB" cannot be answered by reasoning about the arena alone -- the sidecar,
/// the tmpfs upper layer and the dispatch tables are all resident too.
#[cfg(target_arch = "wasm32")]
pub fn linear_memory_mib() -> usize {
    core::arch::wasm32::memory_size(0) * 64 / 1024
}

#[cfg(not(target_arch = "wasm32"))]
pub fn linear_memory_mib() -> usize {
    0
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::{Path, PathBuf};

    // --- The scan spec grammar -------------------------------------------
    //
    // Testable at all only because the parser moved here. In the deadlock
    // handler it was reachable only by deadlocking a guest with the variable
    // set, which is why a field-shift bug lived in it undetected.

    #[test]
    fn a_clause_is_three_decimal_fields() {
        assert_eq!(parse_scan("8:1:100"), vec![(8, 1, 100)]);
    }

    #[test]
    fn clauses_are_comma_separated_and_ordered() {
        assert_eq!(
            parse_scan("8:1:100,24:0:7,0:4096:4096"),
            vec![(8, 1, 100), (24, 0, 7), (0, 4096, 4096)]
        );
    }

    #[test]
    fn fields_may_be_padded() {
        assert_eq!(parse_scan(" 8 : 1 : 100 "), vec![(8, 1, 100)]);
    }

    #[test]
    fn an_unset_spec_scans_nothing() {
        // `init_diag_flags` passes `unwrap_or_default()`, so "unset" arrives
        // here as the empty string rather than as an absent call.
        assert!(parse_scan("").is_empty());
        assert!(parse_scan(",,").is_empty());
    }

    #[test]
    fn a_clause_with_the_wrong_field_count_is_dropped() {
        assert!(parse_scan("8:1").is_empty());
        assert!(parse_scan("8:1:100:7").is_empty());
        // ...and dropping one does not drop its neighbours.
        assert_eq!(parse_scan("8:1,24:0:7").len(), 1);
    }

    #[test]
    fn an_unparseable_field_drops_its_whole_clause() {
        // The regression this parser's move fixed. The inlined version dropped
        // unparseable FIELDS and counted the survivors, so this kept `1:2:3`
        // and scanned for a clause nobody asked for.
        assert!(parse_scan("foo:1:2:3").is_empty());
        assert!(parse_scan("0x18:1:2").is_empty());
    }

    /// The three settings that moved here store and load the state they claim
    /// to. Worth a test only because each is a `static` in a file with a dozen
    /// near-identical ones, and storing to the neighbour is a mistake that
    /// compiles, runs, and shows up as "the flag did nothing".
    ///
    /// One test rather than three: these are process-wide, and nothing else in
    /// the crate reads them (the host build of
    /// `context::inline_call_history_enabled` returns false from a `cfg`, not
    /// from `inline_ch`), so a single sequential test needs no lock.
    #[test]
    fn the_startup_settings_round_trip() {
        assert!(!no_file_shm(), "uninitialised must read as off");
        assert!(!inline_ch(), "uninitialised must read as off");
        assert!(scan_clauses().is_empty(), "uninitialised must read as off");

        set_no_file_shm(true);
        set_inline_ch(true);
        set_scan("16:2:9");
        assert!(no_file_shm());
        assert!(inline_ch());
        assert_eq!(scan_clauses(), [(16, 2, 9)]);

        set_no_file_shm(false);
        set_inline_ch(false);
        set_scan("");
        assert!(!no_file_shm());
        assert!(!inline_ch());
        assert!(scan_clauses().is_empty());
    }

    // --- The watchpoint's comparability rule --------------------------------

    #[test]
    fn the_first_sample_is_a_baseline() {
        assert_eq!(watch_verdict(None, 1, &[0, 0]), WatchVerdict::Baseline);
    }

    #[test]
    fn an_unchanged_window_reports_nothing() {
        assert_eq!(
            watch_verdict(Some((7, &[1, 2, 3])), 7, &[1, 2, 3]),
            WatchVerdict::Unchanged
        );
    }

    #[test]
    fn a_change_within_one_process_is_reported() {
        assert_eq!(
            watch_verdict(Some((7, &[1, 2, 3])), 7, &[1, 9, 3]),
            WatchVerdict::Changed
        );
    }

    /// ⚠️ THE BUG THIS RULE EXISTS FOR. The probe reads the LIVE arena, which a
    /// context switch swaps wholesale, so identical VMAs hold different
    /// processes' memory either side of one. Comparing across a switch reported a
    /// write that never happened -- and it named a culprit, attributing one to
    /// dash's `__libc_early_init` during the double-free hunt.
    ///
    /// Note the bytes DIFFER here: if the pid were ignored this returns Changed.
    #[test]
    fn a_different_owner_is_a_new_baseline_not_a_change() {
        assert_eq!(
            watch_verdict(Some((7, &[1, 2, 3])), 8, &[9, 9, 9]),
            WatchVerdict::Baseline
        );
    }

    /// ...and the pid alone does not license silence: same owner, same bytes as
    /// the OTHER process had, is still a real comparison.
    #[test]
    fn the_owner_check_does_not_swallow_a_real_change() {
        assert_eq!(
            watch_verdict(Some((8, &[9, 9, 9])), 8, &[9, 9, 8]),
            WatchVerdict::Changed
        );
    }

    /// A window that changed size cannot be compared. Unreachable today -- the
    /// length is startup configuration -- but the alternative to saying so is a
    /// `zip()` that silently compares a prefix and calls the rest unchanged.
    #[test]
    fn a_resized_window_is_a_baseline_rather_than_a_prefix_comparison() {
        assert_eq!(
            watch_verdict(Some((7, &[1, 2, 3, 4])), 7, &[1, 2, 3]),
            WatchVerdict::Baseline
        );
    }

    // --- No `env::var` outside startup ------------------------------------

    /// Every `.rs` under `src/`, so a new module is covered without editing
    /// this test. `cargo test` runs with the package root as CWD.
    fn sources() -> Vec<(String, String)> {
        fn walk(dir: &Path, out: &mut Vec<PathBuf>) {
            let mut entries: Vec<PathBuf> = std::fs::read_dir(dir)
                .unwrap_or_else(|e| panic!("reading {}: {e}", dir.display()))
                .map(|e| e.unwrap().path())
                .collect();
            entries.sort();
            for p in entries {
                if p.is_dir() {
                    walk(&p, out);
                } else if p.extension().is_some_and(|x| x == "rs") {
                    out.push(p);
                }
            }
        }
        let mut paths = Vec::new();
        walk(Path::new("src"), &mut paths);
        assert!(paths.len() > 5, "found only {} sources", paths.len());
        paths
            .into_iter()
            .map(|p| {
                let src = std::fs::read_to_string(&p).unwrap();
                (p.display().to_string(), src)
            })
            .collect()
    }

    /// Reduces a line to CODE: no `//` comment, no string literal.
    ///
    /// Both exclusions are load-bearing. Dropping comments keeps the many prose
    /// mentions of the forbidden call -- including the one at the top of this
    /// file -- from reading as calls. Dropping string literals is what lets the
    /// two tests below name the thing they forbid in their own failure
    /// messages: without it, this module is the loudest violator in the crate
    /// and both tests fail on themselves.
    /// It is per-FILE and not per-line because a Rust string literal may span
    /// lines: the message these tests print does, and its continuation line
    /// began outside any string when this was written line at a time, which put
    /// the forbidden text back into "code" on exactly one line of the crate.
    /// `pub(super)` so `undec_census_tests`, which polices two source spans of
    /// its own, strips comments with the SAME stripper rather than a second
    /// copy that could disagree about what counts as code.
    pub(super) fn code_only(src: &str) -> Vec<String> {
        let mut out = Vec::new();
        let mut in_str = false;
        for line in src.lines() {
            let mut kept = String::with_capacity(line.len());
            let mut escaped = false;
            let mut chars = line.chars().peekable();
            while let Some(c) = chars.next() {
                if in_str {
                    if escaped {
                        escaped = false;
                    } else if c == '\\' {
                        escaped = true;
                    } else if c == '"' {
                        in_str = false;
                    }
                    continue;
                }
                match c {
                    '"' => in_str = true,
                    '/' if chars.peek() == Some(&'/') => break,
                    _ => kept.push(c),
                }
            }
            out.push(kept);
        }
        out
    }

    /// The line range of a top-level `fn NAME`, as `[first, last]`. rustfmt
    /// puts a top-level closing brace at column 0 and nothing else, which is
    /// what makes this reliable without parsing Rust.
    fn fn_span(file: &str, src: &str, name: &str) -> (usize, usize) {
        let lines: Vec<&str> = src.lines().collect();
        let open = lines
            .iter()
            .position(|l| l.contains(&format!("fn {name}(")))
            .unwrap_or_else(|| panic!("{file}: no `fn {name}(` -- was it renamed? See below."));
        let close = lines[open..]
            .iter()
            .position(|l| *l == "}")
            .unwrap_or_else(|| panic!("{file}: `fn {name}` has no column-0 close"))
            + open;
        (open, close)
    }

    /// `std::env::var` must be read ONCE, at startup, before any guest runs or
    /// forks -- see the note at the top of this file. This is the tripwire for
    /// that rule, and it exists because the rule has been broken three times by
    /// three different mechanisms, each one invisible to a reader of the call
    /// site: `dtrace_regs` read it per traced call, the `mmap` handler read it
    /// per file-backed MAP_SHARED, and the deadlock reporter read it after
    /// every process was already blocked. None of them looked like a diagnostic
    /// gate, which is how each escaped the sweep that caught the others.
    ///
    /// A new env read is not necessarily wrong. It is a decision, and this test
    /// makes it one: move the read into `init_diag_flags` and expose the value
    /// from `diag`, or -- if it genuinely is startup-only -- widen the spans
    /// below and say why.
    #[test]
    fn env_is_read_only_from_startup_paths() {
        let mut seen_in = Vec::new();
        for (file, src) in sources() {
            let hits: Vec<usize> = code_only(&src)
                .iter()
                .enumerate()
                .filter(|(_, l)| l.contains("env::var"))
                .map(|(i, _)| i)
                .collect();
            if hits.is_empty() {
                continue;
            }
            let name = Path::new(&file).file_name().unwrap().to_str().unwrap();
            // The ONLY functions allowed to read the environment, all of which
            // run during boot, before any guest code and so before any fork.
            //
            // `boot_world` is what `__main_argc_argv` used to be: the boot half
            // was split out when the scheduler became re-entrant, so a host can
            // boot once and then ask for slices. The rule is unchanged -- this
            // is still the startup path -- but the span had to follow the name,
            // and this test firing on the rename is the guard working.
            let allowed: Vec<(usize, usize)> = match name {
                "sys.rs" => vec![fn_span(&file, &src, "init_diag_flags")],
                "entry.rs" => vec![
                    fn_span(&file, &src, "boot_world"),
                    fn_span(&file, &src, "load_sidecar"),
                ],
                _ => vec![],
            };
            for h in hits {
                assert!(
                    allowed.iter().any(|&(a, b)| h > a && h < b),
                    "{file}:{}: reads the environment outside a startup path.\n  {}\n\
                     `std::env::var` after a fork can hit `lazy_lock::panic_poisoned`, \
                     whose panic handler re-reads the environment and loops forever. \
                     Read it once in `sys::init_diag_flags` and expose it from `diag`.",
                    h + 1,
                    src.lines().nth(h).unwrap().trim()
                );
            }
            seen_in.push(name.to_string());
        }
        // Without this the test passes vacuously the day someone writes
        // `use std::env::var;` and calls a bare `var(..)`, or the day the two
        // startup readers are renamed -- both of which turn the scan above into
        // a loop over nothing.
        seen_in.sort();
        assert_eq!(
            seen_in,
            vec!["entry.rs".to_string(), "sys.rs".to_string()],
            "the probe no longer finds the reads it is supposed to police"
        );
    }

    /// Closes the alias route past the test above: an import makes the call
    /// sites read `var("X")`, which the `env::var` scan cannot see.
    #[test]
    fn the_environment_is_never_imported_under_a_shorter_name() {
        for (file, src) in sources() {
            for (i, code) in code_only(&src).iter().enumerate() {
                assert!(
                    !(code.contains("use std::env::") || code.contains("use core::env::")),
                    "{file}:{}: importing from `std::env` hides the reads that \
                     `env_is_read_only_from_startup_paths` polices. Spell them out.",
                    i + 1
                );
            }
        }
    }

    // --- The removed snapshot gate -------------------------------------------

    /// The bounded-snapshot environment gate is GONE, and this is what fails if
    /// it comes back. The name it had is assembled below rather than written
    /// anywhere, for the reason given at the end of this comment.
    ///
    /// Bounded arena snapshots were opt-in, then the default, and since
    /// 2026-08-22 they are the only scheme there is. A flag would not merely be
    /// redundant: the full-buffer path it used to select is no longer reachable
    /// code, so a reintroduced variable would either do nothing (an operator
    /// setting it gets silence, which is how the old `=0`-means-ON trap worked)
    /// or would restore a path that dies at four processes. Deciding to bring it
    /// back is allowed; doing it without noticing is what this prevents.
    ///
    /// ⚠️ Scanned on the RAW source, comments and string literals included --
    /// deliberately the opposite of `code_only`, which every other source scan
    /// here uses. The reintroduction this guards looks exactly like an
    /// `std::env::var(..)` whose argument is the removed name, and `code_only`
    /// strips string literals -- so a stripped scan would be blind to the one
    /// shape that matters, and a comment-stripped one would let the prose that
    /// documents the removal drift back into an instruction.
    ///
    /// The name is therefore assembled from fragments rather than written out,
    /// so that this test is not itself the violation it reports. That is the
    /// only reason for the `format!`.
    #[test]
    fn the_removed_snapshot_gate_stays_removed() {
        let gone = format!("RAPTORMARK_{}_{}", "ECV", "BOUNDED");
        let mut hits = Vec::new();
        for (file, src) in sources() {
            for (i, line) in src.lines().enumerate() {
                if line.contains(&gone) {
                    hits.push(format!("{file}:{}: {}", i + 1, line.trim()));
                }
            }
        }
        assert!(
            hits.is_empty(),
            "`{gone}` was removed on 2026-08-22 and must not come back by \
             accident. Bounded snapshots are the ONLY scheme; the full-buffer \
             path is gone, and a multi-threaded group takes a full snapshot \
             because `is_multithreaded` says so, not because of any \
             environment. Found:\n  {}",
            hits.join("\n  ")
        );
        // Without this the scan passes vacuously the day `sources()` stops
        // finding files -- and it is the only thing this test does, so a
        // vacuous pass is a total loss of coverage.
        assert!(
            sources().iter().any(|(_, s)| s.contains("RAPTORMARK_ECV_")),
            "the scan found no RAPTORMARK_ECV_ variables at all, so it is \
             looking at the wrong tree"
        );
    }

    // --- One sink -----------------------------------------------------------

    /// Every diagnostic goes through `trace::write_line`, except on the way out.
    ///
    /// This is the invariant the diagnostics refactor was for, and it was
    /// two-thirds true for a day: 27 unconditional notices in `context` and
    /// `execmap` kept a raw `eprintln!` on the reading that routing them through
    /// `trace` would make them conditional. It would not -- `ecv_warn!` is
    /// ungated and `write_line` is a plain `eprintln!` on every target -- but the
    /// reasoning was written while a `tracing` subscriber was still in the
    /// design, and it outlived it.
    ///
    /// The five functions below are the exception on purpose: they are the last
    /// words before an abort, and the one message you need should not depend on
    /// a layer that could itself be broken. Anything else printing directly is
    /// the same drift starting again.
    #[test]
    fn only_the_fatal_path_prints_without_going_through_the_sink() {
        let mut found = 0;
        for (file, src) in sources() {
            let hits: Vec<usize> = code_only(&src)
                .iter()
                .enumerate()
                .filter(|(_, l)| l.contains("eprintln!"))
                .map(|(i, _)| i)
                .collect();
            if hits.is_empty() {
                continue;
            }
            let name = Path::new(&file).file_name().unwrap().to_str().unwrap();
            let allowed: Vec<(usize, usize)> = match name {
                // The sink itself.
                "trace.rs" => vec![fn_span(&file, &src, "write_line")],
                // `fatal!`'s implementation, and the panic hook.
                "lib.rs" => vec![fn_span(&file, &src, "runtime_error")],
                "sys.rs" => vec![fn_span(&file, &src, "init_diag_flags")],
                // The guest-state dumps that run immediately before a `fatal!`.
                "intrinsics.rs" => vec![
                    fn_span(&file, &src, "report_runaway_recursion"),
                    fn_span(&file, &src, "__remill_function_call"),
                    fn_span(&file, &src, "__remill_jump"),
                ],
                _ => vec![],
            };
            for h in &hits {
                assert!(
                    allowed.iter().any(|&(a, b)| *h > a && *h < b),
                    "{file}:{}: prints without going through `trace::write_line`.\n  {}\n\
                     An unconditional notice converts to `ecv_warn!`, which is ungated \
                     on every target; a gated one to `ecv_debug!`/`ecv_trace!`/\
                     `ecv_probe!`. Raw printing is for the fatal path only.",
                    h + 1,
                    src.lines().nth(*h).unwrap().trim()
                );
            }
            found += hits.len();
        }
        // The scan must still be finding the known fatal-path sites; otherwise it
        // would pass just as well against a crate that had stopped printing.
        assert_eq!(found, 8, "the fatal-path sites this test polices moved");
    }
}

/// The fatal text for an undecoded instruction the guest cannot survive.
///
/// ⚠️ Lives HERE, not beside its only caller in `intrinsics.rs`, and that is the
/// point: `intrinsics` is `#[cfg(target_arch = "wasm32")]`, so a `#[cfg(test)]`
/// module inside it never compiles on the host and its tests never run. That was
/// tried first -- `cargo test` reported the same 131 tests as before the two new
/// ones were added, which is the only reason it was noticed.
///
/// # Why the wording is guarded
///
/// This message used to read "no lifted instruction at 0x...", which names a
/// missing BASIC BLOCK. The cause is a missing INSTRUCTION in the decoder. On
/// 2026-08-19 that cost about an hour: postgres stopped at 0x9bd3f4 while a
/// lifter patch that changes block discovery was under test, the wording matched
/// that patch's failure mode exactly, and it took disassembling the address by
/// hand to find `fnmul`.
///
/// # ⚠️ Why it does NOT print the encoding
///
/// It did, for one build. `EcvContext::undecoded_word` read the guest word from
/// the arena the way `block_address` does for its `bbmiss` line -- and printed
/// `0x00000000` for an address holding `1e688800 fnmul d0, d0, d8`. **The arena
/// does not contain guest .text**, so that read yields zeros. All 138 `bbmiss`
/// lines from a ruby run print `insn=0x00000000` for the same reason; the field
/// has never carried a real encoding.
///
/// A fabricated `0x00000000` is worse than no encoding at all: it is a valid
/// aarch64 word (`udf #0`), so it reads as a real answer and sends the next
/// reader looking for a UDF that is not there. The address plus a disassembler
/// is the honest instruction.
pub(crate) fn undecoded_message(addr: u64) -> String {
    format!(
        "undecoded instruction at 0x{addr:x}: the lifter emitted no semantics for \
         it, and the guest has no SIGILL handler to receive it. This is an \
         INSTRUCTION the decoder lacks, not a missing basic block -- disassemble \
         that address in the fused ELF for the encoding, then grep patches/ for it."
    )
}

#[cfg(test)]
mod undecoded_message_tests {
    use super::undecoded_message;

    #[test]
    fn says_instruction_not_block_and_says_where() {
        let m = undecoded_message(0x9bd3f4);
        assert!(
            m.contains("0x9bd3f4"),
            "the address is the actionable part: {m}"
        );
        assert!(
            m.contains("not a missing basic block"),
            "the message must say which of the two failures this is: {m}"
        );
        assert!(
            !m.contains("no lifted instruction"),
            "the old wording is the defect being fixed: {m}"
        );
    }

    /// ⚠️ Guards a REMOVAL. An earlier version printed an encoding read from the
    /// arena, which does not hold guest .text and so yielded `0x00000000` for a
    /// real `fnmul`. That is a valid aarch64 word, so it reads as an answer.
    /// The message must not carry a fabricated one.
    #[test]
    fn does_not_fabricate_an_encoding() {
        let m = undecoded_message(0x40053c);
        assert!(
            !m.contains("0x00000000"),
            "must not invent an encoding: {m}"
        );
        assert!(
            m.contains("disassemble"),
            "having dropped the encoding it must say how to get one: {m}"
        );
    }
}

// ---------------------------------------------------------------------------
// Undecoded-instruction CENSUS (RAPTORMARK_ECV_UNDEC_CENSUS)
// ---------------------------------------------------------------------------
//
// ⚠️ READ THIS BEFORE TOUCHING ANYTHING BELOW. ⚠️
//
// This mode makes `__ecv_warning` RETURN where it would otherwise `fatal!`, so
// the guest carries on with the undecoded instruction's effect NEVER APPLIED.
// It is unsound by construction and it is not a "get further" switch:
//
//   * A register or memory location the instruction should have written keeps
//     its old value. Everything computed from it afterwards is garbage.
//   * The garbage is SILENT. The guest can print a wrong answer, spin forever,
//     or die thousands of instructions later inside code that has nothing to do
//     with the site that was skipped.
//   * A crash under this mode is therefore NOT evidence about anything except
//     the site list. Never diagnose a second defect from a census run.
//
// What it IS for: answering "which undecoded instructions does a real workload
// actually EXECUTE", in ONE run instead of one run per site. The static
// inventory ranks by sites PRESENT -- and that ranking has already been wrong
// once, when patch 0063 cleared the largest family (706 `tbl`) and moved
// nothing observable while patch 0064 cleared 11 sites and unblocked the
// PostgreSQL planner. Every lifter patch costs a `BaseID` change and a cold
// object cache, so picking the next one off the EXECUTED set rather than the
// PRESENT set is worth an unsound instrument -- provided nobody mistakes its
// output for a working guest.
//
// Its own gate, not part of `set_gates`: every flag in that tuple changes
// OUTPUT, and this one changes BEHAVIOUR. `bounded_snapshots` is the only other
// flag of that kind, and it now has its own setter too, for the same reason --
// it was the sixth positional bool of `set_gates` while this comment already
// claimed otherwise.

/// 0 = uninitialised, 1 = off, 2 = on -- the same encoding as every other gate
/// in this module, and the reason [`census_on`] exists.
static UNDEC_CENSUS_FLAG: AtomicU8 = AtomicU8::new(0);

/// The greatest number of DISTINCT undecoded addresses one run reports.
///
/// Capping by unique KEY rather than by occurrence, exactly as
/// `context::bbmiss_first_time` does and for the same measured reason: an
/// occurrence cap lets one hot site crowd out every rare one, and removing the
/// cap entirely made a guest so slow it never reached the event the dump was
/// meant to explain. An instrument verbose enough to change the outcome has
/// stopped measuring the thing.
///
/// 4096 is above any plausible executed set: the whole fused PostgreSQL closure
/// held 2,950 real undecoded sites across 398 encodings after patch 0063, and
/// the executed subset is what this measures.
const UNDEC_CENSUS_CAP: usize = 4096;

/// The census banner, one line per element so each carries the `[undec_census]`
/// prefix and none of it can be lost to a grep that keeps only prefixed lines.
///
/// Emitted from [`set_undec_census`] rather than from the first skipped site,
/// because the warning has to reach an operator who runs the mode and sees the
/// guest come out CLEAN -- the outcome most likely to be believed.
pub(crate) const UNDEC_CENSUS_BANNER: [&str; 4] = [
    "⚠️  UNDECODED-INSTRUCTION CENSUS IS ON (RAPTORMARK_ECV_UNDEC_CENSUS).",
    "⚠️  THIS RUN IS UNSOUND. Undecoded instructions are SKIPPED, not executed,",
    "⚠️  so every result after the first skip is garbage -- wrong output, a hang",
    "⚠️  and a later crash are all expected. Trust the addr= lines. Nothing else.",
];

/// Printed once, if and only if the cap clipped the list.
///
/// A truncated census that says nothing reads as a COMPLETE census, which is
/// the precise failure this whole exercise exists to avoid: choosing the next
/// lifter patch off a list that silently omitted the site that mattered.
///
/// A function rather than a `const &str` so the number in the prose IS
/// [`UNDEC_CENSUS_CAP`]; a hardcoded copy drifts the day the cap is raised, and
/// it would drift in the one message whose whole job is to be believed.
pub(crate) fn undec_census_truncated_message() -> String {
    format!(
        "census TRUNCATED: more than {UNDEC_CENSUS_CAP} distinct undecoded \
         addresses were executed, so the list above is INCOMPLETE. Raise \
         UNDEC_CENSUS_CAP in runtime/src/diag.rs and re-run."
    )
}

/// The gate, as a pure function of the stored byte.
///
/// Factored out of [`undec_census`] so that "uninitialised means OFF" is a
/// claim a host test can check, instead of a property of whether
/// `sys::init_diag_flags` happened to have run. That claim is the load-bearing
/// one here: a census that defaulted ON would convert this runtime's one LOUD
/// failure -- `fatal!` on an undecoded instruction -- into silent garbage, on
/// every workload, with no operator having asked for it.
#[inline]
fn census_on(raw: u8) -> bool {
    raw == 2
}

/// True when the census is armed. Default OFF, in every sense: unset
/// environment, unset flag, and a runtime in which `init_diag_flags` never ran
/// all read false.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
#[inline]
pub(crate) fn undec_census() -> bool {
    census_on(UNDEC_CENSUS_FLAG.load(Ordering::Relaxed))
}

/// Called once by `sys::init_diag_flags`, before any guest code or fork.
///
/// Prints the banner as a side effect of ARMING, so there is no path that turns
/// the mode on without saying so.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn set_undec_census(on: bool) {
    UNDEC_CENSUS_FLAG.store(if on { 2 } else { 1 }, Ordering::Relaxed);
    if on {
        for l in UNDEC_CENSUS_BANNER {
            ecv_warn!(undec_census, "{l}");
        }
    }
}

/// What `__ecv_warning` must do when the SIGILL it posted found no handler.
///
/// A two-armed enum rather than a bare `if diag::undec_census()` at the call
/// site, for one reason: `intrinsics` is `#[cfg(target_arch = "wasm32")]`, so
/// the branch itself is unreachable from `cargo test` and "with the census off,
/// an undecoded instruction is still fatal" would otherwise be untestable on
/// the host. This is the closest a host test can stand to that branch.
#[derive(Debug, PartialEq, Eq, Clone, Copy)]
pub(crate) enum Undecoded {
    /// Abort with [`undecoded_message`]. The default, and the honest outcome.
    Fatal,
    /// Record the site and return, leaving the instruction unexecuted. UNSOUND.
    Census,
}

/// The disposition of an undecoded site nothing handled. See [`Undecoded`].
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
#[inline]
pub(crate) fn undecoded_disposition() -> Undecoded {
    if undec_census() {
        Undecoded::Census
    } else {
        Undecoded::Fatal
    }
}

/// What one executed undecoded address did to the census table.
#[derive(Debug, PartialEq, Eq, Clone, Copy)]
pub(crate) enum Census {
    /// First execution of this address in this run -- log it.
    New,
    /// Already recorded, or dropped after the cap -- say nothing.
    Seen,
    /// The cap has just clipped the list. Log [`UNDEC_CENSUS_TRUNCATED`], once.
    Truncated,
}

/// The set of addresses already reported, plus whether the cap has been
/// announced.
///
/// A struct rather than a bare `Vec` so both pieces sit behind one lock, and so
/// [`CensusTable::note`] is pure over an explicit table -- which is what makes
/// the dedupe, the cap and the once-only truncation notice testable without a
/// process-wide static that other tests would race.
pub(crate) struct CensusTable {
    seen: Vec<u64>,
    truncated: bool,
}

impl CensusTable {
    /// A const initialiser, which is the whole point -- see
    /// [`undec_census_note`].
    pub(crate) const EMPTY: CensusTable = CensusTable {
        seen: Vec::new(),
        truncated: false,
    };

    /// Records `addr` if it is new and the table has room.
    pub(crate) fn note(&mut self, addr: u64) -> Census {
        if self.seen.contains(&addr) {
            return Census::Seen;
        }
        if self.seen.len() >= UNDEC_CENSUS_CAP {
            if self.truncated {
                return Census::Seen;
            }
            self.truncated = true;
            return Census::Truncated;
        }
        self.seen.push(addr);
        Census::New
    }
}

/// Dedupes one executed undecoded address against the process-wide table.
///
/// ⚠️ `Mutex::new` is const, so this needs no `LazyLock` -- and that is not a
/// style preference. A lazy initialiser on the fork path is what caused the
/// post-fork `lazy_lock::panic_poisoned` infinite loop (see the note at the top
/// of this file and `sys.rs`); `context::bbmiss_first_time` is written this way
/// for the same reason. A poisoned lock reports `Seen`, i.e. loses a census
/// line, which is the correct direction to fail in for a diagnostic.
#[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
pub(crate) fn undec_census_note(addr: u64) -> Census {
    static TABLE: std::sync::Mutex<CensusTable> = std::sync::Mutex::new(CensusTable::EMPTY);
    let Ok(mut t) = TABLE.lock() else {
        return Census::Seen;
    };
    t.note(addr)
}

/// The `[undec_census]` line for one executed undecoded site.
///
/// Two things this has to get right. The address is printed as bare `0x...`
/// hex, which is what `llvm-objdump --start-address=` and a disassembler prompt
/// both take verbatim; and the line repeats the unsoundness, so a log pasted
/// into an issue three weeks later still carries the warning that the run it
/// came from computed nothing trustworthy.
///
/// ⚠️ No `insn=` field, for the reason spelled out on [`undecoded_message`] and
/// on `context::bbmiss_message`: the arena does not contain guest `.text`, so
/// any encoding read at run time is a fabricated `0x00000000`, which is a valid
/// aarch64 `udf #0` and reads as an answer.
pub(crate) fn undec_census_message(addr: u64) -> String {
    format!(
        "addr=0x{addr:x} SKIPPED-UNSOUND: undecoded instruction was EXECUTED and \
         stepped over, its effect never applied. Disassemble 0x{addr:x} in the \
         fused ELF for the encoding, then grep patches/ for it."
    )
}

#[cfg(test)]
mod undec_census_tests {
    use super::*;

    /// Serialises the tests that touch the process-wide flag. The pure tests
    /// below deliberately do not need it.
    static FLAG: std::sync::Mutex<()> = std::sync::Mutex::new(());

    // --- The gate ---------------------------------------------------------

    /// ⚠️ THE test. A census that defaulted on would replace this runtime's one
    /// loud failure with silent garbage on every workload.
    ///
    /// Stated against the stored byte rather than the atomic, so it covers the
    /// case no atomic test can: a runtime in which `sys::init_diag_flags` never
    /// ran at all, where the flag still holds its static initialiser 0.
    #[test]
    fn the_gate_is_off_unless_something_explicitly_turned_it_on() {
        assert!(
            !census_on(0),
            "0 is the static initialiser -- a runtime that never called \
             init_diag_flags must not census"
        );
        assert!(!census_on(1), "1 is `the environment variable was unset`");
        assert!(census_on(2), "2 is the only value that arms it");
        // Nothing else may arm it either: a `!= 1` gate would pass the two
        // asserts above and still turn 3, 42, ... on.
        for raw in [3u8, 42, 255] {
            assert!(!census_on(raw), "{raw} is not a value this module stores");
        }
    }

    /// The disposition `__ecv_warning` actually consults, with the flag off.
    #[test]
    fn an_unhandled_undecoded_site_is_fatal_by_default() {
        let _lock = FLAG.lock().unwrap_or_else(|e| e.into_inner());
        set_undec_census(false);
        assert_eq!(undecoded_disposition(), Undecoded::Fatal);
        assert!(!undec_census());
    }

    /// ... and only with the flag on does it become a census.
    #[test]
    fn arming_the_gate_is_what_switches_the_disposition() {
        let _lock = FLAG.lock().unwrap_or_else(|e| e.into_inner());
        set_undec_census(true);
        assert_eq!(undecoded_disposition(), Undecoded::Census);
        set_undec_census(false);
        assert_eq!(
            undecoded_disposition(),
            Undecoded::Fatal,
            "the gate must be able to go back off"
        );
    }

    // --- The banner -------------------------------------------------------

    /// The banner has to say the run is untrustworthy, in words, more than
    /// once. Asserting on the substance rather than on the exact prose.
    #[test]
    fn the_banner_says_the_run_is_unsound() {
        let all = UNDEC_CENSUS_BANNER.join("\n");
        assert!(
            all.contains("RAPTORMARK_ECV_UNDEC_CENSUS"),
            "it must name the variable that produced it: {all}"
        );
        assert!(
            all.contains("UNSOUND"),
            "the word, not a euphemism for it: {all}"
        );
        assert!(
            all.contains("SKIPPED") && all.contains("garbage"),
            "it must say WHAT is wrong (skipped) and WHAT that costs (garbage): {all}"
        );
        assert!(
            UNDEC_CENSUS_BANNER.len() > 1,
            "one line is not `loudly and more than once`"
        );
    }

    /// Arming the mode prints the banner; not arming it prints nothing.
    ///
    /// The negative half is the one that matters -- a banner is worthless if a
    /// normal run also emits it, and worse than worthless if an armed run does
    /// not.
    #[test]
    fn the_banner_is_emitted_exactly_when_the_mode_is_armed() {
        let _lock = FLAG.lock().unwrap_or_else(|e| e.into_inner());
        let off = crate::trace::tests::capture(|| set_undec_census(false));
        assert_eq!(off, "", "a normal run must be silent about the census");
        let on = crate::trace::tests::capture(|| set_undec_census(true));
        set_undec_census(false);
        assert!(on.contains("UNSOUND"), "{on}");
        assert_eq!(
            on.lines().count(),
            UNDEC_CENSUS_BANNER.len(),
            "every banner line must reach the sink: {on}"
        );
        for l in on.lines() {
            assert!(
                l.starts_with("[undec_census] "),
                "each line must carry the prefix so none is lost to a grep: {l}"
            );
        }
    }

    // --- Dedupe and cap ---------------------------------------------------

    #[test]
    fn an_address_is_reported_once_and_then_never_again() {
        let mut t = CensusTable::EMPTY;
        assert_eq!(t.note(0x9bd3f4), Census::New);
        assert_eq!(t.note(0x9bd3f4), Census::Seen);
        assert_eq!(t.note(0x9bd3f4), Census::Seen);
        // A different address is still new: the dedupe is per address, not a
        // one-shot latch.
        assert_eq!(t.note(0x40053c), Census::New);
        assert_eq!(t.note(0x40053c), Census::Seen);
        assert_eq!(t.note(0x9bd3f4), Census::Seen);
    }

    #[test]
    fn the_cap_holds_and_says_so_once() {
        let mut t = CensusTable::EMPTY;
        for i in 0..UNDEC_CENSUS_CAP as u64 {
            assert_eq!(t.note(0x400000 + i * 4), Census::New, "site {i}");
        }
        // The first address past the cap is dropped, and announced.
        assert_eq!(t.note(0xdead00), Census::Truncated);
        // ... exactly once, however many more arrive.
        assert_eq!(t.note(0xdead04), Census::Seen);
        assert_eq!(t.note(0xdead08), Census::Seen);
        assert_eq!(t.note(0xbeef00), Census::Seen);
        // And an address recorded BEFORE the cap still reads as seen, rather
        // than being re-reported by a table that had started dropping.
        assert_eq!(t.note(0x400000), Census::Seen);
    }

    #[test]
    fn the_truncation_notice_says_the_list_is_incomplete() {
        let m = undec_census_truncated_message();
        assert!(m.contains("INCOMPLETE"), "{m}");
        assert!(
            m.contains(&UNDEC_CENSUS_CAP.to_string()),
            "the notice must quote the CAP itself, not a copy of it that can \
             drift when the cap is raised: {m}"
        );
    }

    // --- The line ---------------------------------------------------------

    #[test]
    fn the_line_names_the_address_in_a_form_a_disassembler_takes() {
        let m = undec_census_message(0x9bd3f4);
        assert!(
            m.contains("0x9bd3f4"),
            "bare hex, pasteable into llvm-objdump --start-address=: {m}"
        );
        assert!(
            m.contains("addr="),
            "a stable key so the site list falls out of `grep -o`: {m}"
        );
        assert!(
            m.contains("UNSOUND"),
            "a pasted log must carry the warning with it: {m}"
        );
    }

    /// ⚠️ Guards the same REMOVAL as `undecoded_message` and
    /// `context::bbmiss_message`: the arena holds no guest `.text`, so any
    /// encoding printed here is a fabricated `udf #0` that reads as an answer.
    #[test]
    fn the_line_does_not_fabricate_an_encoding() {
        let m = undec_census_message(0x40053c);
        assert!(!m.contains("insn"), "{m}");
        assert!(!m.contains("0x00000000"), "{m}");
        assert!(
            m.contains("Disassemble"),
            "having no encoding it must say how to get one: {m}"
        );
    }

    // --- The call site ----------------------------------------------------

    /// `intrinsics` is wasm-only, so nothing above can observe that
    /// `__ecv_warning` still aborts. This reads the source instead.
    ///
    /// It polices two things at once: that the fatal path is still IN that
    /// function (a census that replaced it would be a silent, permanent
    /// downgrade of the runtime's loudest failure), and that the census branch
    /// goes through `undecoded_disposition`, which is the function every test
    /// above is about.
    #[test]
    fn ecv_warning_still_aborts_and_routes_the_census_through_the_gate() {
        let src = std::fs::read_to_string("src/intrinsics.rs").unwrap();
        let lines: Vec<&str> = src.lines().collect();
        let open = lines
            .iter()
            .position(|l| l.contains("fn __ecv_warning("))
            .expect("src/intrinsics.rs: no `fn __ecv_warning(` -- was it renamed?");
        let close = lines[open..].iter().position(|l| *l == "}").unwrap() + open;
        let body: String = lines[open..=close].join("\n");
        let code: Vec<String> = tests_code_only(&body);
        let has = |needle: &str| code.iter().any(|l| l.contains(needle));
        assert!(
            has("fatal!("),
            "__ecv_warning no longer aborts on an undecoded site. The census is \
             an instrument; it must not become the default outcome."
        );
        assert!(
            has("undecoded_disposition"),
            "the census branch must go through `diag::undecoded_disposition`, \
             which is the only part of this decision a host test can reach."
        );
        assert!(
            has("undec_census_note"),
            "a census that does not dedupe by address is the verbose instrument \
             `bbmiss_first_time` exists to avoid."
        );

        // --- The ORDER, which is the whole of the 2026-08-21 fix -------------
        //
        // `deliver_pending_signals` arms SIGILL's default action -- `Pending::
        // Exit(132)` plus `suspended` -- BEFORE it returns 0. So a census that
        // decides what to do only after the post inherits a condemned process
        // and the run ends at the guest's next syscall: one site per run, from
        // an instrument built to enumerate them all in one run. The three
        // positions below say "decide, then post", which is what makes the armed
        // exit unreachable rather than something to undo afterwards.
        //
        // These are positions in the COMMENT-STRIPPED source, so the prose above
        // the code -- which names every one of these symbols -- cannot satisfy
        // them.
        let at = |needle: &str| {
            code.iter()
                .position(|l| l.contains(needle))
                .unwrap_or_else(|| {
                    panic!("src/intrinsics.rs: `__ecv_warning` no longer calls `{needle}`")
                })
        };
        let post = at("post_signal_to_thread");
        assert!(
            at("undecoded_disposition") < post,
            "the disposition is read AFTER the SIGILL is posted. By then \
             `deliver_pending_signals` has already armed `Pending::Exit(128 + \
             SIGILL)`, and a census arm that returns leaves the guest to die at \
             its next syscall with no diagnostic."
        );
        assert!(
            at("delivers_to_handler") < post,
            "whether a guest handler exists must be settled BEFORE the post: \
             that is the case which still has to be posted and delivered \
             unchanged (postgres probes for ARMv8 CRC32C under a SIGILL handler \
             that siglongjmps), and it is the only thing separating it from the \
             census."
        );
        let first_return = code
            .iter()
            .position(|l| l.trim() == "return;")
            .expect("src/intrinsics.rs: `__ecv_warning` has no census early return");
        assert!(
            first_return < post,
            "the census arm returns from BELOW the post, so it is still the \
             post-then-repair shape. Nothing may cancel an armed exit -- one \
             armed for a real `kill` is indistinguishable from one armed here."
        );
    }

    /// Reuses the comment/string stripper the env and sink probes use, so a
    /// mention of `fatal!` in a doc comment cannot satisfy the test above.
    fn tests_code_only(src: &str) -> Vec<String> {
        super::tests::code_only(src)
    }

    /// The env var is read exactly where every other one is.
    ///
    /// The call site is located on the COMMENT-STRIPPED source -- the prose
    /// above that call names both the function and the variable, and a test
    /// satisfied by a comment is satisfied by nothing -- and then asserted
    /// against the RAW line, because `code_only` also strips string literals
    /// and the variable name is one.
    #[test]
    fn the_gate_is_wired_to_its_environment_variable_at_startup() {
        let src = std::fs::read_to_string("src/sys.rs").unwrap();
        let code = tests_code_only(&src);
        let lines: Vec<&str> = src.lines().collect();
        let at = code
            .iter()
            .position(|l| l.contains("set_undec_census"))
            .expect("src/sys.rs: nothing arms the census");
        assert!(
            lines[at].contains("RAPTORMARK_ECV_UNDEC_CENSUS"),
            "the census must be armed from its own variable and nothing else: {}",
            lines[at]
        );
        // And that call has to be inside the one function allowed to read the
        // environment, or it is an env read on a post-fork path.
        let open = code
            .iter()
            .position(|l| l.contains("fn init_diag_flags("))
            .unwrap();
        let close = code[open..].iter().position(|l| l == "}").unwrap() + open;
        assert!(
            at > open && at < close,
            "src/sys.rs:{}: the census is armed outside `init_diag_flags`",
            at + 1
        );
    }
}
