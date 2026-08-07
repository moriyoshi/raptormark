//! Supervisor context: the opaque pointer the lifted code threads through
//! every call (formerly upstream's `RuntimeManager`). Owns the arena, the
//! dispatch tables built from the `_ecv_*` symbol tables, the VFS, and the
//! per-process file-descriptor table / cwd / identity.

use crate::abi::{EcvProgram, LiftedFunc, State};
use crate::arena::{Arena, ArenaSnapshot, SharedKind, SharedSeg, ShmWindow};
use crate::execmap::Programs;
use crate::fatal;
// ⚠️ CORRECTION. This import used to omit `ecv_warn` deliberately, on the reasoning
// that -- because this module is NOT `#[cfg(target_arch = "wasm32")]` and so also
// compiles for the host, where `sys::init_diag_flags` never runs -- its GATED
// diagnostics convert losslessly (the `diag` atomics stay uninitialised, so every
// gate already reads false) while its UNCONDITIONAL ones would go "from guaranteed
// to subscriber-dependent".
//
// The first half is still right. The second half stopped being true when the
// `tracing` crate was dropped for the hand-rolled facility: there is no subscriber
// any more. `ecv_warn!` -> `trace::emit_plain` -> `trace::write_line` is a plain
// `eprintln!` on every target, gated by nothing, and `trace::line` renders
// `[{cat}] {args}` -- byte-identical to the `eprintln!("[ecvisor] ...")` it
// replaces. The 24 notices here and the 3 in `execmap` now go through it, so ONE
// sink carries every diagnostic this runtime emits except the fatal path.
//
// The rule that remains: an unconditional message must convert to `ecv_warn!`, never
// to `ecv_debug!`/`ecv_probe!`. Those are gated, and on the host their gates are
// permanently false.
use crate::trace::{ecv_debug, ecv_log, ecv_probe, ecv_trace, ecv_warn};
use crate::vfs::{NodeKind, Vfs};
use std::collections::VecDeque;

/// Exit code for a detected deadlock. Distinct from any guest exit status so a
/// hang is never mistaken for a clean run by a caller reading $?.
pub const EXIT_DEADLOCK: i32 = 111;

// --- The unwinding flag --------------------------------------------------
//
// "A return to the scheduler is in progress." Raised by the svc trampoline once
// it has consumed `EcvContext::suspended`, and cleared by the scheduler in
// entry.rs when the leg is back. While it is set, every lifted frame on the way
// out returns immediately.
//
// It lives in a WASM GLOBAL rather than on EcvContext, because the lifted code
// tests it after every syscall AND every lifted call -- 33,540 sites in the
// linked bash fixture alone (.agents/docs/MULTIMODULE.md 3c). As a field it cost
// a call to `_ecv_suspended` at each one; as a global the codegen reads it with
// a single `global.get` (elfconv patch 0059). Its meaning and lifetime are
// unchanged: there is exactly one EcvContext, so a field on it and a
// module-wide global are the same object.
//
// The definition is in runtime/cshim/ecv_globals.c, next to ecv_sp.c, because
// Rust cannot express a wasm global.

#[cfg(target_arch = "wasm32")]
extern "C" {
    fn ecv_get_unwinding() -> i32;
    fn ecv_set_unwinding(v: i32);
}

/// Reads the unwinding flag. `_ecv_suspended` is the same read, kept exported
/// for lifted objects built before patch 0059.
#[cfg(target_arch = "wasm32")]
pub fn unwinding() -> bool {
    unsafe { ecv_get_unwinding() != 0 }
}

#[cfg(target_arch = "wasm32")]
pub fn set_unwinding(v: bool) {
    unsafe { ecv_set_unwinding(v as i32) }
}

// The host build has no cshim to link against, so it gets a plain static. The
// runtime is single-threaded by construction (a cooperative scheduler that only
// switches at a block, a yield or an exit), so this is not a weakening.
#[cfg(not(target_arch = "wasm32"))]
static UNWINDING: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

#[cfg(not(target_arch = "wasm32"))]
pub fn unwinding() -> bool {
    UNWINDING.load(std::sync::atomic::Ordering::Relaxed)
}

#[cfg(not(target_arch = "wasm32"))]
pub fn set_unwinding(v: bool) {
    UNWINDING.store(v, std::sync::atomic::Ordering::Relaxed)
}

// --- The call history ----------------------------------------------------
//
// A stack of {func_vma, return_pc} frames: pushed at each lifted function's
// prologue, popped at its normal epilogue. It is the raw material for
// fork-by-replay -- context.rs rebuilds a child's stack from the parent's saved
// history rather than rewinding lifted glibc fork frames, which cannot be
// rewound.
//
// WHY IT IS NOT A `Vec`. It is written once per guest BL, and the lifted code
// can do that inline if the storage has a fixed address it can compute from
// (elfconv patch 0060). So the LIVE history is a flat buffer whose base, length
// and capacity live in wasm globals; `Process.call_history` stays an ordinary
// `Vec` snapshot, because a suspended process's history is touched only by the
// scheduler.
//
// The length is in the global and NOWHERE ELSE. Lifted code bumps it directly,
// so a second copy in Rust would be a second source of truth for the one number
// fork replay depends on.

/// One guest frame, exactly as the lifted code writes it: `fun` at +0, `ret` at
/// +8. This is an ABI type shared with elfconv patch 0060, pinned the same way
/// `State` is -- a layout change here corrupts the fork replay silently.
#[repr(C)]
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Frame {
    pub fun: u64,
    pub ret: u64,
}

const _: () = {
    use core::mem::{align_of, offset_of, size_of};
    assert!(size_of::<Frame>() == 16);
    assert!(align_of::<Frame>() == 8);
    assert!(offset_of!(Frame, fun) == 0);
    assert!(offset_of!(Frame, ret) == 8);
};

#[cfg(target_arch = "wasm32")]
extern "C" {
    fn ecv_ch_built() -> i32;
    fn ecv_get_ch_len() -> i32;
    fn ecv_set_ch_len(v: i32);
    fn ecv_set_ch_buf(base: i32, cap: i32);
}

#[cfg(target_arch = "wasm32")]
#[inline]
fn ch_len() -> usize {
    unsafe { ecv_get_ch_len() as usize }
}
#[cfg(target_arch = "wasm32")]
#[inline]
fn set_ch_len(v: usize) {
    unsafe { ecv_set_ch_len(v as i32) }
}
#[cfg(target_arch = "wasm32")]
fn set_ch_buf(base: *mut u8, cap: usize) {
    unsafe { ecv_set_ch_buf(base as usize as i32, cap as i32) }
}
// The host build has no shim to link against; the length is an ordinary static.
// Single-threaded by construction, as above.
#[cfg(not(target_arch = "wasm32"))]
static CH_LEN: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
#[cfg(not(target_arch = "wasm32"))]
#[inline]
fn ch_len() -> usize {
    CH_LEN.load(std::sync::atomic::Ordering::Relaxed)
}
#[cfg(not(target_arch = "wasm32"))]
#[inline]
fn set_ch_len(v: usize) {
    CH_LEN.store(v, std::sync::atomic::Ordering::Relaxed)
}
#[cfg(not(target_arch = "wasm32"))]
fn set_ch_buf(_base: *mut u8, _cap: usize) {}

/// Whether to let lifted code maintain the call history inline.
///
/// `RAPTORMARK_ECV_INLINE_CH=1`. Off by default and read exactly once, at
/// `EcvContext::new`, before any guest code runs -- like every other gate here.
///
/// Two independent opt-ins guard this feature, and that is deliberate. The
/// module has to be BUILT with `--inline-call-history` for the fast path to
/// exist at all, and ecvisor has to be RUN with this for the path to be taken.
/// Either one off means every guest BL calls in exactly as it always did. The
/// second gate is what makes the first safe to ship in the patch series: a
/// module carrying the inline code is not committed to running it, so a
/// suspected miscompile is one environment variable away from being ruled out
/// rather than a rebuild away.
// The call history is a plain `Vec<(u64, u64)>` and STAYS one. That is the whole
// design: the default path must be the code it always was, not a
// reimplementation that happens to be equivalent.
//
// MEASURED WHY. Replacing it with a hand-rolled flat buffer whose length lived
// behind the shim accessors cost the DEFAULT path 2.9%, and a second attempt
// that branched on an `inline` flag cost 6.5% -- paid by every guest, for a
// feature nobody had enabled. Forcing `#[inline(always)]` on the hot methods
// changed nothing, which refuted the obvious explanation. `Vec::push` on a
// preallocated vector is already about the minimum, and anything wrapped around
// it is not free.
//
// So when the inline path is OFF, nothing here runs and `Vec::push` / `pop` are
// reached exactly as before. When it is ON, the lifted code writes into the
// VECTOR'S OWN buffer and Rust reconciles only where control crosses between
// them.

/// The frame layout the lifted code writes: `func_vma` at +0, `return_pc` at +8.
///
/// An ABI shared with elfconv patch 0060, pinned the way `State` is. The element
/// type stays the ordinary tuple so that no call site has to change -- but a
/// tuple's layout is not guaranteed by the language, so it is asserted rather
/// than assumed.
const _: () = {
    use core::mem::{offset_of, size_of};
    assert!(size_of::<(u64, u64)>() == 16);
    assert!(offset_of!((u64, u64), 0) == 0);
    assert!(offset_of!((u64, u64), 1) == 8);
};

/// Whether the inlined call history (elfconv patch 0060) is LIVE.
///
/// Three things must hold, and the runtime expresses all three the same way --
/// by publishing a capacity of zero, which makes the lifted `len < cap` test
/// fail forever:
///
///   1. the module was BUILT with `--inline-call-history`;
///   2. ecvisor was RUN with `RAPTORMARK_ECV_INLINE_CH=1`;
///   3. no per-call diagnostic is armed.
///
/// (3) used to be a separate `__ecv_slow` global that the lifted code read at
/// every site. Folding it into the capacity removed a global read and a branch
/// per push AND per pop -- 4,111 push and 4,106 pop sites in a small guest.
///
/// (3) is also a CORRECTNESS condition, not just a speed one. With diagnostics
/// armed the slow arm must run so the dtrace and watch hooks fire; if the fast
/// path were still live it would bump the length behind Rust's back and those
/// hooks would see a history that never grew.
///
/// (2) is what makes (1) safe to ship: a module carrying the inline code is not
/// committed to running it, so a suspected miscompile is one environment
/// variable away from being ruled out rather than a rebuild away.
#[cfg(target_arch = "wasm32")]
pub fn inline_call_history_enabled() -> bool {
    // `init_diag_flags` runs before `EcvContext::new`, so both of these are
    // already final here. (2) is read from `diag` rather than from the
    // environment for the reason stated at the top of that module -- this
    // function's safety otherwise rested on the ordering comment above, and
    // nothing structural stops a later caller from a post-fork path.
    if !crate::diag::inline_ch() || crate::diag::hot_slow() {
        return false;
    }
    // The module must ALSO have been built for it. Without this the variable
    // alone turned on adopt/publish against lifted code that never maintains the
    // global, which truncated the history at every syscall and killed a forking
    // guest silently. Say so rather than ignoring it -- an operator who asked for
    // this deserves to know why it did nothing.
    if unsafe { ecv_ch_built() } == 0 {
        ecv_warn!(
            ecvisor,
            "RAPTORMARK_ECV_INLINE_CH=1 ignored: this module was not built \
             with --inline-call-history"
        );
        return false;
    }
    true
}

#[cfg(not(target_arch = "wasm32"))]
pub fn inline_call_history_enabled() -> bool {
    false
}

impl EcvContext {
    /// Hands the lifted code the vector's own buffer: base, capacity, depth.
    ///
    /// Must run before ANY transfer of control into lifted code that could
    /// follow something which moved the vector -- a context switch takes it and
    /// puts a different one back, and a push that reallocates moves it. Getting
    /// that wrong does not fault: it lets the guest write into a freed
    /// allocation. So rather than trusting a list of sites to stay complete, the
    /// scheduler calls this once per leg (entry.rs), which covers every switch
    /// by construction, and the intrinsics call it after a push that may have
    /// reallocated.
    ///
    /// No-op unless both opt-ins are on, so the default path never reaches the
    /// shim.
    #[inline]
    pub fn publish_call_history(&mut self) {
        if !self.ch_inline {
            return;
        }
        let len = self.call_history.len();
        let cap = self.call_history.capacity();
        set_ch_buf(self.call_history.as_mut_ptr() as *mut u8, cap);
        set_ch_len(len);
        self.ch_published = self.call_history.as_ptr();
    }

    /// Adopts the depth the lifted code left in the global.
    ///
    /// The inline fast path bumps `__ecv_ch_len` without telling Rust, so the
    /// vector's own length is stale by exactly the frames the guest pushed. This
    /// makes it true again before Rust touches the history.
    #[inline]
    pub fn adopt_call_history_depth(&mut self) {
        if !self.ch_inline {
            return;
        }
        // Catch the buffer moving without a republish. This never fired while
        // chasing the guest-stack corruption -- the cause was a MISSED ADOPT in
        // entry.rs's replay path, not a stale base -- but the hazard it guards
        // is real and one push path away, and it costs the default path nothing
        // because the whole function returns early when the gate is off.
        if self.call_history.as_ptr() != self.ch_published {
            crate::fatal!(
                "call history moved without republish: published {:?}, now {:?} (len {} cap {})",
                self.ch_published,
                self.call_history.as_ptr(),
                self.call_history.len(),
                self.call_history.capacity()
            );
        }
        // Clamped: a length past the capacity would be a corrupt global, and
        // `set_len` past the allocation is instant undefined behaviour. The
        // clamp turns a bad global into lost frames rather than a wild write.
        let n = ch_len().min(self.call_history.capacity());
        // Safe: frames in [0, n) were written by the lifted fast path through
        // the pointer published above, and `(u64, u64)` has no drop glue.
        unsafe { self.call_history.set_len(n) };
    }
}

/// Upper bound on a single idle sleep, so an implausible guest timeout cannot
/// wedge the module; the scheduler simply re-enters the wait.
const MAX_IDLE_SLEEP_NANOS: u128 = 5_000_000_000;

/// Wall-clock nanoseconds since the epoch: what the guest sees from
/// `clock_gettime(CLOCK_REALTIME)` and `gettimeofday`, and what file timestamps
/// are stamped with.
///
/// ⚠️ NOT A TIMEBASE FOR DEADLINES. A wall clock steps -- NTP corrects it, a
/// laptop suspends, a browser tab is backgrounded and its `Date.now()` jumps on
/// resume -- and a deadline measured against it moves with the step: forward
/// and the sleep ends early, backward and it overruns by the size of the jump.
/// Everything that waits uses `mono_nanos` and converts at arm time with
/// `to_mono`. This function used to serve both, which is the defect that split
/// them.
pub fn now_nanos() -> u128 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0)
}

/// The base `mono_nanos` counts from, captured on its first call.
///
/// `static mut` rather than a `OnceLock` because ecvisor is single-threaded by
/// construction -- no atomics, no wasm threads -- and the module must stay
/// inside Wasm 2.0, which has no threads proposal to lower an atomic onto. It
/// is the same idiom the boot state uses, reached only through `addr_of_mut!`.
static mut MONO_BASE: Option<std::time::Instant> = None;

/// The highest reading `mono_nanos` has returned. See there for why.
static mut MONO_LAST: u128 = 0;

/// Monotonic nanoseconds since the first call, which is during boot.
///
/// This is the timebase every deadline in the runtime is measured against. Two
/// properties matter and neither holds for `now_nanos`: it never runs backwards,
/// and it is unaffected by a wall-clock step. Starting near zero at boot also
/// matches what a guest expects of `CLOCK_MONOTONIC`, which Linux counts from
/// boot rather than from the epoch.
///
/// `Instant` is the same import on every target -- `clock_time_get` with the
/// monotonic clock id on `wasm32-wasip1`, which the module already imports for
/// `SystemTime` -- so this adds no import and no new host obligation.
///
/// ⚠️ `Instant::elapsed` PANICS if the host's monotonic clock runs backwards,
/// and `panic = "abort"` makes that a dead module rather than an error a guest
/// could survive. A host is not obliged to be right -- the reading comes from
/// whatever the embedder wired to `clock_time_get`, which in a browser is
/// `performance.now()` -- so this clamps instead of trusting. Freezing the clock
/// for the length of an anomaly is a bad outcome; aborting the module is a worse
/// one, and monotonicity is the property the whole function exists to provide.
pub fn mono_nanos() -> u128 {
    unsafe {
        let p = std::ptr::addr_of_mut!(MONO_BASE);
        let last = std::ptr::addr_of_mut!(MONO_LAST);
        let now = match *p {
            Some(base) => std::time::Instant::now()
                .checked_duration_since(base)
                .map_or(*last, |d| d.as_nanos()),
            None => {
                *p = Some(std::time::Instant::now());
                0
            }
        };
        *last = now.max(*last);
        *last
    }
}

/// Which of the runtime's two timebases a Linux clock id names.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ClockBase {
    /// Steps with the host's wall clock; counts from the epoch.
    Real,
    /// Never steps; counts from ecvisor's boot.
    Mono,
}

/// Maps an aarch64 Linux `clockid_t` onto a timebase, or `None` for the ids
/// Linux itself rejects with EINVAL.
///
/// ⚠️ THE WHOLE POINT IS THAT THIS IS NOT CONSTANT. `clock_gettime` used to
/// ignore its `clockid` argument outright and answer every clock from the wall
/// clock, so a guest asking for `CLOCK_MONOTONIC` got a value that steps -- and
/// a guest asking for `CLOCK_PROCESS_CPUTIME_ID` got about fifty-six years of
/// CPU time, because it got the epoch.
///
/// Two mappings are approximations and are called out rather than hidden:
///
///   * `CLOCK_PROCESS_CPUTIME_ID` / `CLOCK_THREAD_CPUTIME_ID` are served from
///     elapsed monotonic time, so an idle guest appears to burn CPU while it
///     waits. There is no CPU accounting in this runtime to do better with, and
///     elapsed-since-boot is both the right ORDER OF MAGNITUDE and correct under
///     differencing, which is what `clock()` and every profiler actually do.
///   * `CLOCK_TAI` is served as realtime, i.e. without the leap-second offset.
///     A guest that can tell the difference is measuring leap seconds, and this
///     runtime has no source for them.
///
/// `CLOCK_SGI_CYCLE` (10) is absent deliberately: Linux has no such clock on
/// aarch64 either, so EINVAL is the honest answer rather than a made-up one.
pub fn clock_base(clockid: u64) -> Option<ClockBase> {
    match clockid {
        // REALTIME, REALTIME_COARSE, REALTIME_ALARM, TAI.
        0 | 5 | 8 | 11 => Some(ClockBase::Real),
        // MONOTONIC, PROCESS_CPUTIME, THREAD_CPUTIME, MONOTONIC_RAW,
        // MONOTONIC_COARSE, BOOTTIME, BOOTTIME_ALARM.
        1 | 2 | 3 | 4 | 6 | 7 | 9 => Some(ClockBase::Mono),
        _ => None,
    }
}

/// Reads the clock a guest named, or `None` if it named one that does not exist.
pub fn clock_read(clockid: u64) -> Option<u128> {
    match clock_base(clockid)? {
        ClockBase::Real => Some(now_nanos()),
        ClockBase::Mono => Some(mono_nanos()),
    }
}

/// Re-expresses a deadline that was computed on one of the guest's clocks in the
/// monotonic base, by carrying over the INTERVAL rather than the instant.
///
/// A relative sleep needs no help: `deadline - clock_now` is the interval the
/// guest asked for whichever clock it named. An ABSOLUTE `CLOCK_REALTIME`
/// deadline is the case this exists for -- the instant it names is on the wall
/// clock and means nothing against a monotonic counter, so what survives the
/// conversion is how far away it was when the guest armed it.
///
/// ⚠️ THIS DELIBERATELY DOES NOT TRACK A LATER CLOCK STEP. Linux re-arms an
/// absolute `CLOCK_REALTIME` timer when the wall clock is set, so a guest that
/// asks to wake at 12:00 wakes at 12:00 even if the clock jumps at 11:59.
/// ecvisor freezes the interval at arm time instead, so that guest wakes after
/// the originally-remaining duration. The deviation is a deliberate trade: the
/// alternative is a deadline a backgrounded tab can push arbitrarily far into
/// the future or fire arbitrarily early, and no guest in this tree arms an
/// absolute realtime timer across a step. Revisit if one does.
///
/// A deadline already in the past saturates to `mono_now`, so it fires at once
/// rather than wrapping into a wait of nearly the age of the epoch.
pub fn to_mono(deadline: u128, clock_now: u128, mono_now: u128) -> u128 {
    mono_now.saturating_add(deadline.saturating_sub(clock_now))
}

/// glibc's `GLRO(dl_tls_static_size)` and `GLRO(dl_tls_static_align)` for a
/// fused image's own TLS modules.
///
/// The size covers the TCB and every static block above it -- the extent the
/// prelinker laid out, which is what a thread's TLS area has to hold -- rounded
/// up to the alignment. The align is the maximum any module declared, floored
/// at the TCB size, because glibc uses `align - 1` as a MASK: a zero would make
/// it all-ones and mask away whatever it was applied to, which is the exact
/// failure this seed exists to prevent.
pub fn glibc_tls_static_geometry(mods: &[MuslTlsModule]) -> (u64, u64) {
    let mut align = crate::arena::TCB_SIZE;
    let mut end = crate::arena::TCB_SIZE;
    for m in mods {
        align = align.max(m.align.max(1));
        end = end.max(m.offset.saturating_add(m.size));
    }
    // Round the extent up to the alignment, as glibc does when it computes the
    // static TLS area from the modules it loaded.
    let size = end.next_multiple_of(align);
    (size, align)
}

/// One entry of musl's `libc.tls_head` list, as the runtime builds it from
/// `.ecv.tls` + `.ecv.tlsalign`. Mirrors musl's `struct tls_module`, whose
/// layout is `{ next, image, len, size, align, offset }` -- 48 bytes, all
/// pointer-sized.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MuslTlsModule {
    pub image: u64,
    pub len: u64,
    pub size: u64,
    pub align: u64,
    pub offset: u64,
}

/// musl's `struct tls_module` in guest memory: six pointer-sized fields.
pub const MUSL_TLS_MODULE_SIZE: u64 = 48;

/// What `libc.tls_size`, `libc.tls_align` and `libc.tls_cnt` must be for the
/// module list `mods`, given `pthread_size` = the part of `struct pthread` that
/// sits below the thread pointer (`.ecv.musltp` word 1).
///
/// ⚠️ `tls_size` is the size of the WHOLE per-thread allocation -- struct
/// pthread, the TLS blocks, the DTV and alignment slack -- NOT the sum of the
/// module sizes. `__copy_tls` places the DTV at `mem + libc.tls_size` while
/// `pthread_create` mmaps that many bytes, so the two agree with each other
/// whatever the value is: over-estimating costs a slightly larger per-thread
/// mapping, under-estimating writes the DTV past the end of the allocation.
///
/// This deliberately OVER-ESTIMATES rather than reproducing musl's formula from
/// memory. The block extent already includes the 16-byte gap above the thread
/// pointer, because `.ecv.tls` offsets start at `TCB_SIZE`; adding a full
/// `align` of slack on top covers the alignment `__copy_tls` performs.
pub fn musl_tls_geometry(mods: &[MuslTlsModule], pthread_size: u64) -> (u64, u64, u64) {
    // musl aligns the pthread struct itself to tls_align, so a floor of the
    // TCB gap keeps the arithmetic sane for an image with no thread-locals or
    // with under-aligned ones.
    let mut align = crate::arena::TCB_SIZE;
    let mut end = 0u64;
    for m in mods {
        align = align.max(m.align.max(1));
        end = end.max(m.offset.saturating_add(m.size));
    }
    let dtv = (mods.len() as u64 + 1) * 8;
    let total = pthread_size
        .saturating_add(end)
        .saturating_add(dtv)
        .saturating_add(align);
    ((total + 15) & !15, align, mods.len() as u64)
}

/// The absolute deadline a `nanosleep` / `clock_nanosleep` request names, or
/// None when the timespec is malformed and the syscall must report EINVAL.
///
/// It lives here rather than in `sys.rs` for a reason that is not organisation:
/// `sys` is `#[cfg(target_arch = "wasm32")]`, so anything defined there is
/// invisible to `cargo test` on the host. This is the whole of the sleep that
/// can be wrong arithmetically, and it is worth a test, so it belongs on the
/// host-compiled side next to the clocks it is measured against.
///
/// ⚠️ `now` is a reading of the clock the GUEST named, not of the runtime's
/// timebase, and the deadline comes back on that same clock. `to_mono` is what
/// moves it onto the timebase the scheduler compares against; for a relative
/// request the two steps compose to `mono_now + interval`, which is why a
/// caller that gets this wrong still looks right on every relative sleep.
pub fn sleep_deadline(now: u128, abstime: bool, secs: u64, nsecs: u64) -> Option<u128> {
    // Linux validates the timespec before it validates anything else about the
    // wait, and a guest that passes tv_nsec >= 1e9 is expecting EINVAL, not a
    // sleep of some rounded interpretation of what it asked for.
    if nsecs >= 1_000_000_000 || (secs as i64) < 0 {
        return None;
    }
    let t = secs as u128 * 1_000_000_000 + nsecs as u128;
    // saturating_add: a guest may legitimately ask for a sleep longer than the
    // epoch (glibc's "sleep forever" is a very large tv_sec), and that must park
    // until something wakes it, not wrap into a deadline already in the past.
    Some(if abstime { t } else { now.saturating_add(t) })
}

/// Time left on a deadline as a (secs, nsecs) timespec, saturating at zero.
/// This is what an interrupted sleep hands back so the guest can resume the
/// REMAINDER -- glibc's `sleep()` loops on exactly this, so a wrong answer here
/// turns one interrupted sleep into a longer total sleep than was asked for.
pub fn remaining_timespec(deadline: u128, now: u128) -> (u64, u64) {
    let left = deadline.saturating_sub(now);
    ((left / 1_000_000_000) as u64, (left % 1_000_000_000) as u64)
}

/// How a thread group ended.
///
/// The distinction is the parent's, not the dying process's: Linux's wait status
/// word encodes "exited with code N" and "killed by signal N" in DIFFERENT bits,
/// and a parent is expected to tell them apart. PostgreSQL's postmaster does --
/// `LogChildExit` reports "terminated by signal %d" for `WIFSIGNALED` and
/// "exited with exit code %d" for `WIFEXITED`, and only the former drives its
/// crash-restart path.
///
/// The shell convention `128 + signo` is a rendering of a signal death for a
/// HUMAN, not the kernel's encoding, and collapsing the two into it here is
/// exactly the defect this type exists to remove: it left `WIFEXITED` true and
/// `WIFSIGNALED` false for every signal death in the system.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ExitReason {
    /// `exit` / `exit_group` with this code. Only the low byte survives into the
    /// wait status, which is Linux's rule and not a shortcut taken here.
    Exited(i32),
    /// Terminated by an uncaught signal's default action. Carries the SIGNAL
    /// NUMBER, never `128 + signo` -- see `wait_status`.
    Killed(u32),
}

impl ExitReason {
    /// The Linux wait status word, as `wait4` writes it through `wstatus`.
    ///
    /// The two encodings and the macros that read them:
    ///
    ///   - exited: `(code & 0xff) << 8`. The low 7 bits are zero, so
    ///     `WIFEXITED` (`(s & 0x7f) == 0`) is true and `WEXITSTATUS` is
    ///     `(s >> 8) & 0xff`.
    ///   - killed: the signal number in the low 7 bits. `WTERMSIG` is
    ///     `s & 0x7f` and `WIFSIGNALED` is `((signed char)((s & 0x7f) + 1) >> 1) > 0`.
    ///
    /// ⚠️ That `WIFSIGNALED` expression is not the tautology it looks like. It
    /// is false for TWO low-byte values on purpose: `0`, which is a normal exit,
    /// and `0x7f`, which is `WIFSTOPPED`'s marker -- `(0x7f + 1)` is `0x80`,
    /// which as a signed char is `-128`, and `-128 >> 1` is `-64`. So a signal
    /// number of 0 or 127 would encode a death that `WIFSIGNALED` denies. Signal
    /// numbers here are `1..=NSIG-1` (`NSIG` is 65), so neither is reachable,
    /// and the test `only_a_real_signal_number_survives_the_encoding` is what
    /// keeps it that way.
    ///
    /// ⚠️ Bit `0x80` -- `WCOREDUMP` -- is deliberately NEVER set. This runtime
    /// writes no core, and Linux does not set it either under the container
    /// default `RLIMIT_CORE=0`. Setting it would advertise a file that does not
    /// exist.
    pub fn wait_status(self) -> u32 {
        match self {
            ExitReason::Exited(code) => ((code & 0xff) << 8) as u32,
            ExitReason::Killed(sig) => sig & 0x7f,
        }
    }

    /// What a SHELL reports, and what the module exits with when init dies.
    ///
    /// Distinct from [`wait_status`](Self::wait_status) and not interchangeable
    /// with it: this is a single small integer for a human or a `$?`, while the
    /// wait status is a packed word for `waitpid`'s macros. Conflating them is
    /// the bug this type was introduced to fix.
    pub fn status_code(self) -> i32 {
        match self {
            ExitReason::Exited(code) => code,
            ExitReason::Killed(sig) => 128 + sig as i32,
        }
    }
}

/// A process's scheduling status.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ProcStatus {
    Runnable,
    Blocked, // suspended in wait4; woken when a child becomes a zombie
    /// Exited or killed; awaiting reap by the parent's `wait4`. Carries the
    /// REASON rather than a code, because that is the part `wait4` cannot
    /// reconstruct afterwards.
    Zombie(ExitReason),
    Dead, // reaped
}

/// What a suspended process is waiting on — set by the yielding syscall, acted
/// on by the scheduler.
#[derive(Debug)]
pub enum Pending {
    None,
    Yield,
    Block, // process suspended (wait4 / pipe read); Process.blocked_on says why
    /// `exit_group` (and `exit` from a single-threaded task): retire the whole
    /// thread group, close its descriptors and notify its parent.
    ///
    /// Carries an [`ExitReason`] rather than a code so that a terminating
    /// default action can reuse this arm -- the whole teardown -- WITHOUT losing
    /// the fact that it was a kill by the time the parent's `wait4` asks.
    Exit(ExitReason),
    /// `exit` from one thread of a multi-threaded group: retire this task only.
    /// The group's descriptors, memory and parent notification are untouched --
    /// getting that wrong closes the whole process's fds when a worker thread
    /// finishes.
    ExitThread(i32),
}

/// A process. The RUNNING process's arena/state/fds/cwd live in the EcvContext
/// fields (fixed addresses so asyncify-saved stacks stay valid); they are
/// snapshotted back into `arena`/`state`/`fds`/`cwd` (Some) when it is switched
/// out, and None while it is current.
pub struct Process {
    pub pid: u32,
    pub ppid: u32,
    /// Thread-group id: the pid of the group LEADER. `pid == tgid` for an
    /// ordinary process and for the leader of a threaded one; a `CLONE_THREAD`
    /// sibling carries the leader's tgid and its own `pid` as its tid.
    ///
    /// This is the only thing that distinguishes a thread from a process here,
    /// and everything threads share hangs off it: one arena, one fd table, one
    /// cwd, one signal state. `getpid` reports the tgid, `gettid` the pid.
    pub tgid: u32,
    pub status: ProcStatus,
    pub started: bool,   // false => fresh entry (init, or a just-execve'd process)
    pub prog_idx: usize, // the program this process runs (changes on execve)
    pub blocked_on: BlockedOn,
    pub arena: Option<ArenaSnapshot>,
    pub state: Option<Box<State>>,
    pub fds: Option<Vec<Option<OpenFile>>>,
    /// Per-fd close-on-exec flags, parallel to `fds` (a shorter/absent entry means
    /// not-cloexec). Snapshotted with `fds` so it travels through fork/execve.
    pub cloexec: Option<Vec<bool>>,
    /// Per-fd O_NONBLOCK flags, parallel to `fds`, snapshotted the same way.
    pub nonblock: Option<Vec<bool>>,
    pub cwd: Option<Vec<u8>>,
    pub signals: Option<SignalState>,
    /// This task's OWN signal state. Unlike `signals` it is not an Option and is
    /// never shared: it is saved and restored by task index, not by group table
    /// holder, which is the whole point of the split.
    pub task_signals: TaskSignals,
    /// Which units THIS process has dlopen'd, indexed by registry index.
    ///
    /// ❗ PER-PROCESS, and it has to be. `ensure_unit_loaded` writes the unit's
    /// data into the LIVE arena, and every process has its own arena buffer
    /// restored on each switch -- so a unit loaded by process A exists in A's
    /// memory and nowhere else. Held on the context instead, `inited` would be
    /// true for a process whose arena never received the data, its `dlopen`
    /// would short-circuit, and the plugin would read its own `.data` as
    /// zeroes: silent, and arbitrarily far from the dlopen that caused it.
    ///
    /// It is also the native semantics. `dlopen` affects the calling process
    /// only; `fork` carries the loaded set into the child with the address
    /// space; `execve` discards it with the image.
    ///
    /// Grown lazily rather than sized at construction, so a unit registered
    /// after `_start` needs no fix-up in every process.
    pub units: Vec<UnitLoad>,
    /// The message THIS TASK's next `dlerror` returns, and None once taken.
    ///
    /// POSIX says `dlerror` reports the error from the last dl-call and then
    /// clears, so a second call returns NULL. Before this the intercepted
    /// `dlerror` returned 0 unconditionally -- indistinguishable from "no
    /// error" -- so a guest whose `dlsym` returned NULL had no way to learn why,
    /// and postgres reported an absent object as a version mismatch.
    ///
    /// ❗ PER-TASK, not per-context, and glibc agrees: `dlerror` is per-thread
    /// there. Held on the context it was GLOBAL, so a process could read the
    /// message another process's failed `dlopen` had left behind -- a wrong
    /// diagnosis rather than corruption, but wrong in the one place a guest
    /// looks when it is already confused.
    ///
    /// ⚠️ RESIDUAL, not fixed here: `take_dlerror` writes the text to the single
    /// `arena::DLERROR_VMA`, and threads share one arena. Two threads that both
    /// call `dlerror` can therefore have the second overwrite the buffer the
    /// first is still holding a pointer into. The PENDING message is now
    /// per-task, which is the half that decides WHAT is reported; giving each
    /// thread its own buffer needs per-thread storage and is not worth it until
    /// something needs it.
    pub dlerror: Option<Vec<u8>>,
    /// Live call-history stack {func_vma, return_pc} snapshot (fork_emulation).
    /// Swapped with `EcvContext.call_history` on a context switch, like `fds`.
    pub call_history: Option<Vec<(u64, u64)>>,
    /// A suspended process's pending stack reconstruction (Some for a fork child
    /// and for any process blocked in a syscall; None for a fresh/not-yet-entered
    /// or execve'd process, which the scheduler enters at its program entry).
    pub replay: Option<Replay>,
    /// Absolute deadline (nanoseconds since the epoch) for a TIMED block, or
    /// None for an indefinite one. glibc implements `clock_nanosleep`,
    /// `sem_timedwait` and `pthread_cond_timedwait` as a futex wait with a
    /// timeout, so a runtime that parks such a wait forever hangs the guest on
    /// its first sleep -- with one process and nothing to post the futex, the
    /// wake can only ever come from the clock.
    pub deadline: Option<u128>,
    /// Set when the scheduler released this process because its `deadline`
    /// passed rather than because someone woke it, so the resuming syscall can
    /// report ETIMEDOUT instead of success.
    pub timed_out: bool,
    /// `CLONE_CHILD_CLEARTID` / `set_tid_address`: a guest address the kernel
    /// zeroes and futex-wakes when this task dies. 0 = none.
    ///
    /// It is what makes `pthread_join` terminate. musl points it at
    /// `__thread_list_lock` so the list unlock happens after the thread is
    /// truly gone; glibc points it at the joiner's `pd->tid`. Either way a
    /// runtime that retires a thread without this leaves the joiner parked.
    pub clear_child_tid: u64,
    /// `prctl(PR_SET_DUMPABLE)`'s stored value -- `SUID_DUMP_DISABLE` (0) or
    /// `SUID_DUMP_USER` (1); see [`dumpable_arg_permitted`].
    ///
    /// It is state we keep only so `PR_GET_DUMPABLE` can answer with what
    /// `PR_SET_DUMPABLE` stored. Nothing else reads it, and nothing else may:
    /// the flag decides whether a core dump is written and whether a same-uid
    /// process may `ptrace` this one, and ecvisor does neither. Recording it is
    /// what lets us accept the SET truthfully instead of claiming a core-dump
    /// facility we do not have.
    ///
    /// Per-TASK here, but per-MM in Linux -- so a SET writes it across the
    /// thread group (`set_group_dumpable`), which is what makes the two agree
    /// for a threaded guest. Verified on Linux 6.17: a `prctl(PR_SET_DUMPABLE,
    /// 0)` from a worker thread is visible to `PR_GET_DUMPABLE` on the main
    /// thread.
    pub dumpable: u64,
    /// `prctl(PR_SET_THP_DISABLE)`'s stored bit -- 0 or 1, never the raw
    /// argument; see [`thp_disable_value`].
    ///
    /// Like [`Self::dumpable`] this is kept ONLY so `PR_GET_THP_DISABLE` can
    /// answer with what the SET stored, and nothing else reads it. It cannot:
    /// the flag decides whether khugepaged and the fault handler may back this
    /// mm's anonymous VMAs with huge pages, and ecvisor's guest memory is one
    /// flat wasm linear-memory arena with no page tables, no fault handler and
    /// no huge pages to withhold.
    ///
    /// Per-TASK here, per-MM in Linux, so a SET writes the whole thread group
    /// (`set_group_thp_disable`) -- verified on Linux 6.17, a worker thread's
    /// `PR_SET_THP_DISABLE(1)` is visible to the main thread's GET.
    pub thp_disable: u64,
}

/// Linux's boot value: `MMF_DISABLE_THP` starts clear, so a process that has
/// never called `PR_SET_THP_DISABLE` reads 0.
///
/// ⚠️ On a real system this is INHERITED, not constant -- it survives both fork
/// and execve (`MMF_INIT_MASK`), so a login session that disabled THP hands 1 to
/// everything it starts. Measured on Linux 6.17: this host's shell reads 1 at
/// startup with `THP_enabled: 0` in `/proc/self/status`. 0 is right for the
/// guest's first process because nothing above it asked; it is NOT a claim that
/// THP is available.
pub const THP_NOT_DISABLED: u64 = 0;

/// The two values Linux's `PR_SET_DUMPABLE` accepts: `SUID_DUMP_DISABLE` and
/// `SUID_DUMP_USER` from `include/linux/sched/coredump.h`.
///
/// `SUID_DUMP_ROOT` (2) exists in the same enum but is not settable from
/// userspace -- the kernel sets it itself for a privileged binary -- so
/// `prctl(PR_SET_DUMPABLE, 2)` is EINVAL, not "root-only dumps".
pub const SUID_DUMP_DISABLE: u64 = 0;
pub const SUID_DUMP_USER: u64 = 1;

/// True for a `PR_SET_DUMPABLE` argument Linux would accept.
///
/// The kernel's rule (`kernel/sys.c`) is literally
/// `if (arg2 != SUID_DUMP_DISABLE && arg2 != SUID_DUMP_USER) return -EINVAL;`
/// -- an equality test against the two values, not a range or a truthiness
/// test. Measured on Linux 6.17: 0 and 1 return 0; 2, 3 and (unsigned long)-1
/// return EINVAL.
///
/// ⚠️ The argument is the raw `u64` syscall register, for the same reason
/// [`sigaction_permitted`] takes one: `usize` is 32-bit on wasm32, so
/// `arg as usize` would fold `0x1_0000_0001` onto 1 and accept a value Linux
/// rejects (measured: `prctl(PR_SET_DUMPABLE, 0x100000001)` is EINVAL).
pub fn dumpable_arg_permitted(arg: u64) -> bool {
    arg == SUID_DUMP_DISABLE || arg == SUID_DUMP_USER
}

/// Stores `value` as the dumpable flag of every task in `idx`'s thread group.
///
/// Linux keeps this flag in the `mm_struct`, which a `CLONE_THREAD` sibling
/// shares -- so one thread's `PR_SET_DUMPABLE` is what every other thread's
/// `PR_GET_DUMPABLE` reads. Writing only `procs[idx]` would give each thread a
/// private copy and answer the query with a value the group never agreed on.
///
/// Dead and zombie members are written too: they are cheap, and a member that
/// is Dead here can still be the entry a later lookup lands on.
///
/// Free function over the process table rather than a method, for the same
/// reason `group_member_where` is one -- an `EcvContext` cannot be built on the
/// host, and the per-MM rule is exactly the part worth a test.
pub fn set_group_dumpable(procs: &mut [Process], idx: usize, value: u64) {
    for_thread_group(procs, idx, |p| p.dumpable = value);
}

/// True for a `PR_SET_THP_DISABLE`'s arg3/arg4/arg5.
///
/// The kernel's rule is `if (arg3 || arg4 || arg5) return -EINVAL;` -- it
/// reserves the three trailing arguments and enforces the reservation, which is
/// why the SET has a distinct rule from the GET below. Measured on Linux 6.17:
/// `prctl(41, 1, 1, 0, 0)` is EINVAL, and the flag is NOT changed by the refused
/// call.
///
/// ⚠️ Worth knowing that this is not academic: ruby's `ruby_setup` reaches this
/// with `x2`/`x3`/`x4` all zero (read off `ruby-glibc.fused` at 0x731b8c -- the
/// compiler omitted `mov x2,#0` only because the branch into it is
/// `cbz x2, 731b8c`), so ruby's call is `prctl(41, 1, 0, 0, 0)` and lands on the
/// accepting side.
pub fn thp_disable_set_permitted(arg3: u64, arg4: u64, arg5: u64) -> bool {
    arg3 == 0 && arg4 == 0 && arg5 == 0
}

/// True for a `PR_GET_THP_DISABLE`'s arg2..arg5.
///
/// A DIFFERENT rule from the setter's, because the kernel writes a different
/// one: `if (arg2 || arg3 || arg4 || arg5) return -EINVAL;`. The getter reserves
/// arg2 as well, since it has no value to take. Measured: `prctl(42, 1, 0, 0,
/// 0)` is EINVAL where `prctl(41, 1, 0, 0, 0)` is 0.
pub fn thp_disable_get_permitted(arg2: u64, arg3: u64, arg4: u64, arg5: u64) -> bool {
    arg2 == 0 && arg3 == 0 && arg4 == 0 && arg5 == 0
}

/// The bit `PR_SET_THP_DISABLE(arg2)` stores.
///
/// ⚠️ NOT a validated value: unlike `PR_SET_DUMPABLE`, which accepts exactly 0
/// and 1, this one accepts ANY arg2 and stores its TRUTHINESS -- the kernel is
/// literally `if (arg2) set_bit(...) else clear_bit(...)`. Measured on Linux
/// 6.17: `prctl(41, 2)`, `prctl(41, 3)` and `prctl(41, -1)` all return 0 and all
/// make the GET read 1. Refusing them would be the invention here.
///
/// ⚠️ Takes the raw `u64`, and this is the case where it CHANGES THE ANSWER
/// rather than merely being tidy: `usize` is 32-bit on wasm32, so
/// `arg2 as usize` folds `0x1_0000_0000` onto 0 and would CLEAR the flag on a
/// call that sets it. Measured: `prctl(41, 0x100000000)` makes the GET read 1.
pub fn thp_disable_value(arg2: u64) -> u64 {
    (arg2 != 0) as u64
}

/// Stores `value` as the THP-disable bit of every task in `idx`'s thread group,
/// for the same per-MM reason as [`set_group_dumpable`]: `MMF_DISABLE_THP` lives
/// in the `mm_struct` a `CLONE_THREAD` sibling shares.
pub fn set_group_thp_disable(procs: &mut [Process], idx: usize, value: u64) {
    for_thread_group(procs, idx, |p| p.thp_disable = value);
}

/// Applies `f` to every task sharing `idx`'s thread group -- the write half of
/// "this field is per-MM, not per-task".
///
/// One helper rather than the loop written once per flag, because the rule it
/// encodes (which tasks count as the same mm, and that dead and zombie members
/// are written too -- they are cheap, and a member that is Dead here can still
/// be the entry a later lookup lands on) is the part worth getting right once.
fn for_thread_group(procs: &mut [Process], idx: usize, mut f: impl FnMut(&mut Process)) {
    let tg = procs[idx].tgid;
    for p in procs.iter_mut() {
        if p.tgid == tg {
            f(p);
        }
    }
}

/// A suspended process's stack-reconstruction state (elfconv's `fork_main` model,
/// runtime/Entry.cpp), used for EVERY resume now that asyncify is gone: a fork
/// child AND any process that suspended in a blocking syscall. The stack is
/// rebuilt by RE-ENTERING each frame: `cur` is the frame to enter now (innermost
/// first, at its post-syscall / post-call pc), and `remaining` is the outer frames
/// (a snapshot of `call_history`), consumed innermost-first as each re-entered
/// frame returns (its epilogue pops the live call_history in lockstep). See the
/// scheduler loop in entry.rs and .agents/docs/DYNLINK.md.
#[derive(Clone)]
pub struct Replay {
    /// (func_vma, pc) of the frame to (re-)enter now.
    pub cur: (u64, u64),
    /// Outer frames still to reconstruct; `pop()` yields the next (innermost-first).
    pub remaining: Vec<(u64, u64)>,
    /// True when `cur` is a syscall-resume point (the frame is re-executing the
    /// SVC it blocked on); the scheduler sets `ctx.resuming` from this so the
    /// handler takes its resume path. False for reconstructed outer frames (they
    /// resume at a post-call pc, not a syscall).
    pub resuming: bool,
}

/// `AT_FDCWD`: "resolve this relative path against the current directory".
///
/// Defined here rather than in `sys.rs` so [`EcvContext::resolve_base`] and its
/// tests can see it; `sys.rs` imports it.
pub const AT_FDCWD: i64 = -100;

/// A pipe: a shared byte buffer with reference counts for its open read/write
/// ends across ALL processes (fork duplicates the fds). Lives in the shared
/// EcvContext, referenced by OpenFile::Pipe fds.
pub struct Pipe {
    pub buf: std::collections::VecDeque<u8>,
    pub readers: u32,
    pub writers: u32,
    /// Descriptors in flight over this direction, sent by `sendmsg` with an
    /// `SCM_RIGHTS` control message and claimed by the matching `recvmsg`. One
    /// entry per sending message, in order, because that is the granularity the
    /// kernel attaches them at: rights ride along with a message, not with a
    /// byte offset. Always empty for a real pipe -- only a `SocketPair` can
    /// carry them, and only `sendmsg` puts them here.
    pub scm: std::collections::VecDeque<Vec<OpenFile>>,
}

/// One in-memory regular file, shared by every descriptor open on that path.
///
/// Reference-counted exactly like a pipe end, and for the same reason: `fork`
/// and `dup` duplicate descriptors, so an owner count kept in the descriptor
/// would be wrong the moment either happens. The flush to the tmpfs upper layer
/// happens when the LAST descriptor closes -- flushing per descriptor is what
/// let a stale copy overwrite a newer one.
pub struct MemFile {
    pub path: Vec<u8>,
    pub data: Vec<u8>,
    /// Open descriptors across all processes.
    pub refs: u32,
    pub dirty: bool,
}

/// The read/write offset of one open file description. See
/// `EcvContext::file_offsets` for why this is a table rather than a field of
/// `OpenFile::Mem`.
///
/// Refcounted by exactly the same rule as `MemFile` and the pipe ends, and by
/// the same three holders: this process's fd table, every other process's saved
/// one, and any SCM_RIGHTS batch queued on a pipe. The counts differ from
/// `MemFile`'s, though, and the difference IS the feature -- two `open`s of one
/// path join a single `MemFile` while taking two offsets, so a slot here counts
/// the descriptors that share a POSITION rather than the ones that share bytes.
pub struct FileOffset {
    pub pos: usize,
    /// Descriptors pointing at this description, across all processes.
    pub refs: u32,
}

/// A named AF_UNIX socket: the rendezvous point a `connect(path)` finds and a
/// `bind(path)` publishes.
///
/// WASI has no AF_UNIX. WasmEdge's socket extension knows exactly two families,
/// `WE_FAMILY_INET4` and `WE_FAMILY_INET6`, so there is no host object to bridge
/// to -- and there should not be: both endpoints of a guest UDS are guest
/// processes inside this one module, and the path they rendezvous on exists only
/// in the guest's rfs/tmpfs overlay. So this is implemented entirely in-runtime,
/// on top of the pipe table.
pub struct UnixListener {
    /// The absolute path this socket is bound to, or empty once the path has
    /// been unlinked. Unlinking does NOT kill the listener -- on Linux the
    /// socket keeps serving already-queued and already-accepted connections,
    /// and only new lookups fail -- so the name is cleared rather than the
    /// entry. PostgreSQL relies on this: the postmaster unlinks its socket file
    /// during shutdown while backends are still connected.
    pub path: Vec<u8>,
    /// Set by `listen`. A bound-but-not-listening socket must refuse with
    /// ECONNREFUSED, exactly as the kernel does.
    pub listening: bool,
    /// Completed connections waiting to be accepted, as the SERVER end's
    /// `(rx, tx)` pipe indices. The connector builds both ends and takes its own
    /// immediately, so a connect never blocks and the queue only ever holds the
    /// half the server has not claimed yet.
    ///
    /// The `backlog` argument is deliberately not enforced. Over-accepting is
    /// the benign direction (Linux itself queues beyond the backlog); the
    /// alternative -- refusing or parking the connector -- turns a capacity
    /// limit nothing here can actually hit into a new way for a guest to hang.
    pub pending: VecDeque<(usize, usize)>,
    /// Set when the bound descriptor is closed. The entry stays so indices
    /// remain stable, but it is invisible to `connect`.
    pub dead: bool,
}

/// A System V shared-memory segment (shmget/shmat/shmctl). In ecvisor's single-
/// address-space cooperative model a SysV segment is just a shared arena region
/// (like `mmap(MAP_SHARED|MAP_ANONYMOUS)`), registered in `shared_segments` so it
/// is exempt from the per-process arena restore and visible to every process.
/// PostgreSQL creates a tiny (~56-byte) segment as its postmaster interlock.
#[derive(Clone)]
pub struct ShmSeg {
    pub key: i32,
    pub shmid: i32,
    pub vma: u64,
    pub size: usize,
    pub cpid: u32,
    pub removed: bool, // IPC_RMID: destroyed once the last attach goes
}

// `nattch` is deliberately NOT a field here: the attach set lives on the
// segment's `SharedSeg.mappers`, and one truth is the point. A separate counter
// missed the two ways an attachment ends without a `shmdt` -- fork inherits an
// attachment, exit and execve drop every attachment a process held -- so it
// drifted upward and the segment could never reach nattch == 0.

/// Why a Blocked process is waiting (so the right event wakes it).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum BlockedOn {
    None,
    Wait,            // wait4: woken by a child exit
    PipeRead(usize), // read on an empty pipe: woken by a write/close on that pipe
    /// A socket op (accept/recv/connect/send) that would block. Unlike a pipe
    /// (woken synchronously by another guest process), a socket becomes ready
    /// from the HOST, so when only socket-blocked processes remain the scheduler
    /// sleeps in a host poll_oneoff over their fds (see schedule_after_suspend →
    /// poll_sockets_and_wake). `write` = readiness direction (send/connect vs
    /// recv/accept). See .agents/docs/NETWORKING.md.
    ///
    /// ⚠️ `poll` DISTINGUISHES WAITING ON A SET FROM WAITING ON ONE SOCKET, and
    /// the difference decides whether a SPURIOUS wake is safe.
    ///
    /// `poll_oneoff`/`epoll_pwait` wait on many sockets but this variant records
    /// only one of them, so readiness landing on any of the others must still
    /// wake the process -- and doing so is harmless, because on resume it
    /// re-scans its whole interest list and re-parks if it was wrong.
    ///
    /// A single-socket block (`connect`, `recv`, `send`, `accept`) has no such
    /// re-scan. Waking it spuriously makes it re-attempt an operation that has
    /// not become ready, and the connect state machine cannot tell that from a
    /// real wake -- measured: it turned "connect to a dead port must fail" into
    /// an `EAGAIN` leaking out of a BLOCKING connect, which is the same guard
    /// that caught the M5 readiness bug.
    Socket {
        h: crate::net::NetHandle,
        write: bool,
        /// True when the process is waiting on a SET of sockets.
        poll: bool,
    },
    /// A FUTEX_WAIT on the guest word at `uaddr` (a word INSIDE a shared
    /// segment): woken synchronously by another cooperatively-scheduled process
    /// doing FUTEX_WAKE on the same address (like a pipe read, not a host event).
    /// The shared segment gives every process the same physical bytes at `uaddr`,
    /// so the waker's write + wake reach the waiter. See .agents/docs/SHAREDMEM.md.
    Futex {
        uaddr: u64,
    },
    /// A blocking `accept` on a named AF_UNIX socket with an empty backlog,
    /// carrying the `unix_listeners` index. Woken synchronously by another
    /// guest process's `connect` (like a pipe read, not a host event) -- there
    /// is no host object here for `poll_sockets_and_wake` to watch, so if this
    /// is not woken from `connect` the whole guest deadlocks with the scheduler
    /// convinced everyone is waiting on the outside world.
    UnixAccept {
        listener: usize,
    },
    /// A timed sleep: `nanosleep` / `clock_nanosleep`. The ONLY thing that ends
    /// it on its own is the clock, so the waiter carries an absolute `deadline`
    /// and the scheduler's deadline sweep is its wake source. A posted signal
    /// also releases it (see `post_signal`), which is what makes an interrupted
    /// sleep return EINTR with the time remaining rather than sleeping through
    /// the handler.
    ///
    /// Distinct from `Poll` on purpose: a sleeper has no interest list to
    /// re-evaluate, so waking it for a pipe write or a readiness change would be
    /// pure spurious churn. It re-parks on its original deadline in that case,
    /// but the wake costs a full context switch to learn nothing.
    Sleep,
    /// Waiting for the HOST to make a dlopen'd/exec'd unit's code reachable.
    ///
    /// Like `Socket` and unlike `PipeRead`, readiness comes from OUTSIDE: no
    /// other guest process can supply it, so nothing in the guest can wake this.
    /// The host calls `ecv_side_loaded` between slices and that wakes the
    /// waiter -- the same shape as `ecv_net_ready`.
    ///
    /// ⚠️ A spurious wake is SAFE here, because the waiter re-asks the backend
    /// on resume and re-parks if the answer is still `Pending`. That is
    /// deliberate: the alternative is the single-socket rule, where a spurious
    /// wake makes a state machine re-attempt something that has not happened.
    SideLoad {
        unit: usize,
    },
    /// Blocked in `epoll_pwait` (or a blocking read on a signalfd) with nothing
    /// ready. Woken by any event that can change readiness -- a signal posted by
    /// `kill`, or a pipe write/close -- after which the waiter re-evaluates its
    /// interest list and re-blocks if the wakeup was spurious (the kernel permits
    /// spurious epoll wakeups, so "wake all pollers, let each re-check" is both
    /// correct and far less bookkeeping than tracking which fd woke whom).
    Poll,
}

/// One `epoll_ctl` interest-list entry: the watched fd, the requested event
/// mask, and the opaque `epoll_data` the guest gets back in ready events.
#[derive(Clone, Copy)]
pub struct EpollItem {
    pub fd: i32,
    pub events: u32,
    pub data: u64,
}

/// An open file descriptor.
#[derive(Clone)]
pub enum OpenFile {
    /// Stdio (0/1/2) passed through to the host WASI fd.
    Stdio(i32),
    /// A socket, identified by a backend-owned handle.
    ///
    /// The handle is opaque to the runtime: the `NetBackend` (`crate::net`)
    /// allocates and frees it, and all this layer does is COUNT references --
    /// `fork` copies the fd table by value, so two processes legitimately share
    /// one handle, and `close_fd_full` decides whether a close is the last one
    /// by scanning for equal handles.
    Socket { h: crate::net::NetHandle },
    /// One end of a pipe (index into the shared pipe table).
    Pipe { idx: usize, write: bool },
    /// A regular file materialized in memory (read from the VFS, or a new
    /// tmpfs file). Writes flush back to the tmpfs upper layer on close.
    /// A regular file open in memory. `file` indexes the CONTEXT-GLOBAL
    /// `open_files` table; only `pos` is per-descriptor.
    ///
    /// The contents used to live here, one copy per fd. That made two processes
    /// writing one file diverge, with the last close winning: PostgreSQL, once
    /// two backends could finally run at once, reported
    /// `unexpected data beyond EOF in block 0 of relation base/5/16384` -- one
    /// backend had extended a relation while the other held a shorter copy.
    /// Sharing by path is what Linux does by sharing an inode, and it follows
    /// the pipe table's shape, which already solves the same problem.
    ///
    /// `off` indexes the CONTEXT-GLOBAL `file_offsets` table, which is the open
    /// file description -- so `dup` and `fork` share the offset the way Linux
    /// does by sharing one `struct file`, while a second `open` of the same path
    /// joins this `file` and takes a NEW `off`. It used to be a `pos: usize`
    /// held here, which made every descriptor its own position: `dup2(fd, 5)`
    /// then reading from both re-read the same bytes, and a shell's `read` in a
    /// forked child left the parent's offset behind.
    Mem {
        file: usize,
        off: usize,
        writable: bool,
    },
    /// `/dev/null` (and `/dev/zero`): reads return the requested number of zero
    /// bytes for zero and EOF for null, writes are accepted and discarded.
    ///
    /// A distinct variant rather than an empty `Mem` file, because a writable
    /// `Mem` would accumulate everything written to it and flush a real file
    /// into the tmpfs upper layer on close -- a shell redirecting to /dev/null
    /// would then materialise its whole output.
    Null { zero: bool },
    /// An open directory, for getdents64 -- and for anything that asks "which
    /// directory is this fd?".
    Dir {
        entries: Vec<(Vec<u8>, NodeKind)>,
        pos: usize,
        /// The CANONICAL path this fd was opened on, as `Vfs::resolve` returned
        /// it.
        ///
        /// ❗ Added 2026-09-01, and it is the whole of two fixes rather than a
        /// convenience. A directory fd that does not remember its directory
        /// makes `fchdir` unimplementable and forces `resolve_arg` to refuse
        /// every relative path against a non-`AT_FDCWD` dirfd -- which is how
        /// `postgres:17` stopped, in `docker-entrypoint.sh`'s `find`:
        ///
        /// ```text
        /// find: Failed to change directory: Function not implemented
        /// find: Failed to restore initial working directory: ...
        /// ```
        ///
        /// Canonical, not as-spelled: it is used as a RESOLUTION BASE, so
        /// carrying `../x` here would re-resolve relative to wherever the caller
        /// happened to be at open time rather than to this directory.
        path: Vec<u8>,
    },
    /// A `signalfd`: readable whenever the owning process has a pending signal
    /// selected by `mask` (a sigset_t, bit `sig-1`). Reading consumes one pending
    /// signal and yields a 128-byte `signalfd_siginfo`. This is how PostgreSQL's
    /// latch layer turns an async SIGURG into a synchronously-pollable fd.
    SignalFd { mask: u64 },
    /// An `epoll` set: the interest list registered via `epoll_ctl`.
    Epoll { interest: Vec<EpollItem> },
    /// One end of a `socketpair(AF_UNIX, SOCK_STREAM, 0)`: two pipes, one per
    /// direction, so each end reads from `rx` and writes to `tx` and the peer
    /// has them the other way round. Modelling it on the existing pipe table is
    /// not a shortcut -- it inherits blocking, readiness, EOF-on-last-close and
    /// fork fd duplication already proven by `pipe2`, and the only thing a
    /// socketpair adds over two pipes is `SCM_RIGHTS`, which rides in
    /// `Pipe::scm`.
    ///
    /// This is nginx's master/worker channel: the master `sendmsg`s a command
    /// plus a listening fd, and the worker `recvmsg`s both.
    SocketPair { rx: usize, tx: usize },
    /// An AF_UNIX socket created by `socket(AF_UNIX, SOCK_STREAM, 0)` that is
    /// not yet a connected pair. `listener` is set once `bind` names it (an
    /// index into `EcvContext::unix_listeners`, stable for the life of the
    /// context).
    ///
    /// A CONNECTED endpoint is not this variant: `connect` and `accept` both
    /// hand back a `SocketPair`, which already has the whole data plane --
    /// blocking, readiness, EOF on last close, fork duplication, SCM_RIGHTS.
    /// The only thing a named socket adds over `socketpair` is the rendezvous
    /// by path, which is all this variant carries.
    UnixSocket { listener: Option<usize> },
    /// An `eventfd2` counter. Readable while `count > 0`; a read yields 8 bytes
    /// and drains it (all of it, or 1 when `EFD_SEMAPHORE`), a write adds.
    ///
    /// The counter lives in the fd rather than in a shared table, so a fork
    /// gives parent and child independent counters. That is wrong for a
    /// cross-process notification but right for the use that needs it: nginx
    /// creates its notify eventfd per worker, after the fork, and never shares
    /// one. A shared counter is the change to make if that stops being true.
    EventFd { count: u64, semaphore: bool },
}

impl OpenFile {
    /// The pipe-table ends this descriptor holds a reference on, as `(index,
    /// is_write_end)`. A `Pipe` holds one; a `SocketPair` holds two, because it
    /// reads from one direction and writes to the other; everything else holds
    /// none.
    ///
    /// This exists so the three places that refcount pipe ends -- `close`, `dup`,
    /// and the fork that clones the fd table -- cannot disagree about what a
    /// descriptor owns. They did not have to agree while only `Pipe` had ends;
    /// the moment a second kind has them, a site that forgets one leaks a
    /// reference (peer never sees EOF) or drops one it never took (peer sees EOF
    /// early). The fork site is the load-bearing one: nginx's master forks every
    /// worker while holding both ends of the channel.
    /// The shared in-memory file this descriptor references, if any. Paired
    /// with `pipe_ends`: both name a context-global object that the descriptor
    /// only borrows, and every site that duplicates or drops a descriptor must
    /// consult both.
    pub fn mem_file(&self) -> Option<usize> {
        match self {
            OpenFile::Mem { file, .. } => Some(*file),
            _ => None,
        }
    }

    /// The open file description this descriptor points at, for the same reason
    /// `mem_file` exists: every site that duplicates or drops a descriptor has
    /// to consult it. A clone that takes a reference on the file but not on the
    /// offset frees the offset slot under a live descriptor.
    pub fn mem_offset(&self) -> Option<usize> {
        match self {
            OpenFile::Mem { off, .. } => Some(*off),
            _ => None,
        }
    }

    pub fn pipe_ends(&self) -> Vec<(usize, bool)> {
        match self {
            OpenFile::Pipe { idx, write } => vec![(*idx, *write)],
            OpenFile::SocketPair { rx, tx } => vec![(*rx, false), (*tx, true)],
            _ => Vec::new(),
        }
    }
}

/// Number of signal slots (signums 1..=64; index 0 is unused).
pub const NSIG: usize = 65;
/// SIGCHLD (aarch64 Linux): sent to the parent when a child exits/stops.
pub const SIGILL: u32 = 4;
pub const SIGUSR1: u32 = 10;
pub const SIGCHLD: u32 = 17;
/// The two signals a task may neither catch nor block. aarch64 uses the
/// asm-generic numbering, which for 1..=31 is the same table as x86-64 --
/// checked against the `SIGCHLD = 17` above, which every postmaster path relies
/// on and which differs on mips/alpha/sparc.
pub const SIGKILL: u32 = 9;
pub const SIGSTOP: u32 = 19;

/// What Linux does with a signal whose disposition is still `SIG_DFL`.
///
/// The four kernel actions plus `Cont`, which the man page folds into "Term"'s
/// table as a fifth. Modelled as data rather than as a list of signal numbers
/// at each decision site, because the same table is read from three places
/// (delivery, the pre-block check, and the pre-run check) and a table that
/// disagreed with itself between them would kill a process in one path and not
/// in another.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum DefaultAction {
    /// Terminate the process.
    Term,
    /// Terminate and dump core. ecvisor produces no dump -- see
    /// `terminating_signal` for why that is not a shortcut.
    Core,
    /// Discard.
    Ign,
    /// Stop the process until SIGCONT. NOT modelled; see `terminating_signal`.
    Stop,
    /// Resume a stopped process. Nothing here is ever stopped, so it is `Ign`
    /// with a different name -- kept distinct so the table stays readable
    /// against `signal(7)` rather than encoding a conclusion.
    Cont,
}

/// The default disposition of `sig`, straight from `signal(7)` under the
/// asm-generic numbering aarch64 uses.
///
/// Signals 32..=64 are the real-time range. glibc reserves the first few
/// (SIGSETXID and friends) and installs handlers for them during thread
/// bring-up, so their default is reached only when that bring-up did not
/// happen -- but the kernel's answer for an RT signal with no handler is
/// still Term, and inventing an `Ign` here to be safe would be exactly the
/// "silently hides every real boundary" failure CLAUDE.md warns about.
pub fn default_action(sig: u32) -> DefaultAction {
    match sig {
        // Ign: nothing happens, and the process is not disturbed.
        SIGCHLD | 23 /* SIGURG */ | 28 /* SIGWINCH */ => DefaultAction::Ign,
        18 /* SIGCONT */ => DefaultAction::Cont,
        // Stop: job control. SIGSTOP cannot be caught or blocked; the other
        // three are what a terminal driver sends.
        SIGSTOP | 20 /* SIGTSTP */ | 21 /* SIGTTIN */ | 22 /* SIGTTOU */ => DefaultAction::Stop,
        // Core: terminate, and on a machine with a core pattern, dump.
        3 /* SIGQUIT */ | SIGILL | 5 /* SIGTRAP */ | 6 /* SIGABRT */ | 7 /* SIGBUS */
        | 8 /* SIGFPE */ | 11 /* SIGSEGV */ | 24 /* SIGXCPU */ | 25 /* SIGXFSZ */
        | 31 /* SIGSYS */ => DefaultAction::Core,
        // Term: everything else that exists, including the RT range.
        _ => DefaultAction::Term,
    }
}

/// The lowest-numbered pending signal that must TERMINATE this task now, or
/// `None`.
///
/// This is the whole ruling on default actions in one function, and the reason
/// it takes plain words rather than `&EcvContext` is that it is then reachable
/// from `cargo test` -- the delivery path around it is not (`sys` and `entry`
/// are `wasm32`-only, and running a handler calls into lifted code).
///
/// # What is modelled
///
/// **Term and Core both terminate.** There is no core dump: there is no core
/// pattern, no `RLIMIT_CORE`, and no debugger that could read one. That is not
/// a shortcut -- it is exactly what Linux does under the container default
/// `RLIMIT_CORE=0`, and the observable outcome (the process dies on that
/// signal) is identical. The `WCOREDUMP` bit is correspondingly NOT set, for
/// the same reason: no dump was written.
///
/// **SIGKILL is unconditional.** It ignores `blocked` and it ignores
/// `actions[9]`. `rt_sigaction` here records a handler for any signal 1..=64,
/// including SIGKILL and SIGSTOP, where Linux answers EINVAL -- so the
/// enforcement has to be here, at the one place the disposition is *consumed*,
/// or a guest could install a handler for SIGKILL and become unkillable. A
/// disposition that cannot be honoured is refused where it is read, not where
/// it is written.
///
/// # What is NOT modelled, and why faking it would be worse
///
/// **Stop is not implemented.** SIGSTOP, SIGTSTP, SIGTTIN and SIGTTOU leave the
/// bit pending and do nothing, and `None` here says so. Stopping a process is
/// only meaningful with a way back and a way to see it: SIGCONT delivered to a
/// stopped task, `wait4(WUNTRACED)`, `WIFSTOPPED`, a SIGCHLD raised on stop --
/// and, for the three terminal signals, a controlling terminal and process
/// groups, which this runtime does not have at all (`kill` with `pid <= 0` is
/// ESRCH by construction). It also needs a fifth process state that the idle
/// path must not count as a wake source and the deadlock detector must not
/// count as a hang, which is a change to the scheduler rather than to signals.
///
/// The tempting half-measure -- terminate on a Stop signal -- is strictly worse
/// than doing nothing. SIGTSTP is sent by things that expect the process back.
///
/// **Ign is left pending rather than consumed.** Clearing the bit would be the
/// literal reading, and it is the one thing here that would break a guest that
/// works today: a signalfd reports readiness from `signals.pending`
/// (`fd_ready`), and PostgreSQL's latch is a signalfd over a signal whose
/// disposition is SIG_DFL. Consuming it at a delivery boundary would swallow a
/// wake the fd owns. A bit left set is observationally identical to Ign for
/// everything except `sigpending(2)`, which no guest in this corpus calls.
pub fn terminating_signal(
    group_pending: u64,
    task_pending: u64,
    blocked: u64,
    actions: &[SigAction; NSIG],
) -> Option<u32> {
    let mut pending = group_pending | task_pending;
    while pending != 0 {
        let sig = pending.trailing_zeros() + 1;
        let bit = 1u64 << (sig - 1);
        pending &= !bit;
        if sig as usize >= NSIG {
            break;
        }
        // SIGKILL first, before both filters it is defined to ignore.
        if sig == SIGKILL {
            return Some(sig);
        }
        if blocked & bit != 0 {
            continue;
        }
        // A caught or explicitly ignored signal has no DEFAULT action left to
        // take. SIGSTOP is not exempted here the way SIGKILL is: its default is
        // Stop, so the arm below declines it either way.
        if actions[sig as usize].handler != 0 {
            continue;
        }
        match default_action(sig) {
            DefaultAction::Term | DefaultAction::Core => return Some(sig),
            DefaultAction::Ign | DefaultAction::Cont | DefaultAction::Stop => continue,
        }
    }
    None
}

/// Whether a signal posted right now would be handed to an installed guest
/// HANDLER, rather than left for a default action.
///
/// The mirror image of [`terminating_signal`], and it exists for one caller:
/// `__ecv_warning` has to know the answer BEFORE it posts SIGILL, because
/// posting is not free. `deliver_pending_signals` ends by arming SIGILL's
/// default action -- `Pending::Exit(128 + SIGILL)` plus `suspended` -- and it
/// does that before it returns, so by the time a caller learns "no handler ran"
/// the process is already condemned. That was harmless while `fatal!` was the
/// only thing that could follow; the undecoded-instruction census
/// (`diag::Undecoded::Census`) RETURNS instead, and inherited a run that died at
/// the guest's next syscall with exit 132 and no diagnostic -- roughly one
/// censused site per run, from an instrument whose entire purpose is to
/// enumerate them all in ONE run.
///
/// # ⚠️ It must agree with the delivery loop, not approximate it
///
/// These are exactly the two tests `deliver_pending_signals` applies before it
/// reaches its `handler => { consume; run_signal_handler(..) }` arm, in the same
/// order: the signal must survive `deliverable_set`'s mask subtraction (the
/// caller passes its blocked mask), and its disposition must be a real handler
/// address rather than SIG_DFL (0) or SIG_IGN (1). A predicate that said "handler"
/// where the loop says "default" would let a census skip the post and lose a
/// SIGILL the guest had arranged to catch -- PostgreSQL probes for ARMv8 CRC32C
/// by executing it under a handler that `siglongjmp`s, so losing it is not a
/// missing diagnostic, it is a wrong answer about the CPU.
///
/// SIGKILL and SIGSTOP are refused here for the same reason the loop refuses
/// them: `rt_sigaction` in this runtime records a disposition for any signal
/// 1..=64, so the refusal has to live where the disposition is CONSUMED.
/// Unreachable from today's only caller, which always asks about SIGILL, and
/// stated anyway so that the next caller does not have to know.
pub fn signal_reaches_handler(sig: u32, blocked: u64, actions: &[SigAction; NSIG]) -> bool {
    if sig == 0 || sig as usize >= NSIG {
        return false;
    }
    if sig == SIGKILL || sig == SIGSTOP {
        return false;
    }
    if blocked & (1u64 << (sig - 1)) != 0 {
        return false;
    }
    actions[sig as usize].handler > 1
}

/// `sa_flags` bits we honor during handler delivery (generic Linux ABI values,
/// shared by aarch64). SA_SIGINFO selects the 3-arg `void h(int, siginfo_t*,
/// void*)` handler shape; SA_NODEFER leaves the delivered signal unblocked while
/// its handler runs (default is to block it).
const SA_SIGINFO: u64 = 4;
const SA_NODEFER: u64 = 0x4000_0000;

/// A recorded signal disposition (rt_sigaction). `handler` is the guest
/// sa_handler VMA: 0 = SIG_DFL, 1 = SIG_IGN, else a handler function pointer.
/// ecvisor RECORDS dispositions so a real shell's startup sigaction calls
/// succeed and return the previous action; it does NOT deliver signals yet
/// (nothing raises a signal on the builtin-only script path). `flags` and
/// `mask` are the kernel sa_flags / sa_mask, stored verbatim so oldact
/// round-trips exactly. See .agents/docs/SHAREDMEM.md ("Signals").
#[derive(Clone, Copy)]
pub struct SigAction {
    pub handler: u64,
    pub flags: u64,
    pub mask: u64,
}

impl SigAction {
    pub const fn dfl() -> SigAction {
        SigAction {
            handler: 0,
            flags: 0,
            mask: 0,
        }
    }
}

/// `rt_sigprocmask` `how` (aarch64 / asm-generic).
///
/// Here rather than in `sys` so that [`sigprocmask_next`] -- and therefore the
/// whole `how` ruling -- is reachable from `cargo test`; `mod sys` is
/// `#[cfg(target_arch = "wasm32")]`, so a test written beside the syscall would
/// compile on no host and never run.
pub const SIG_BLOCK: u64 = 0;
pub const SIG_UNBLOCK: u64 = 1;
pub const SIG_SETMASK: u64 = 2;

/// Whether `rt_sigaction(signum, act, ...)` is admissible, or must be -EINVAL.
///
/// `installing` is `act != NULL`, and it is the entire subtlety. Linux checks
/// `!valid_signal(sig) || sig < 1 || (act && sig_kernel_only(sig))`
/// (`kernel/signal.c`, `do_sigaction`) -- so the SIGKILL/SIGSTOP refusal is
/// conditioned on `act`, and a PURE QUERY of either signal SUCCEEDS and reports
/// its disposition through `oldact`. That disposition is always SIG_DFL, since
/// the same check is what prevents it ever becoming anything else.
///
/// Refusing the query too would be the tidier-looking rule and it would be
/// wrong: `sigaction(sig, NULL, &old)` over the whole 1..=64 range is how a
/// program snapshots its inherited dispositions before changing them (and how
/// it discovers which signals arrived already ignored, e.g. from a shell's
/// `nohup`). An EINVAL at 9 and 19 turns a loop that Linux completes into an
/// error the guest never expected to handle.
///
/// # Why this is not redundant with the consumption-site refusal
///
/// [`terminating_signal`] and `deliver_pending_signals` already refuse to ACT
/// on a disposition recorded for SIGKILL or SIGSTOP, and that safety net stays:
/// it is what makes a task genuinely unkillable-proof rather than
/// unkillable-proof-by-agreement-with-this-function. What it cannot do is give
/// the guest the right ANSWER. Today `rt_sigaction(SIGKILL, {SIG_IGN}, NULL)`
/// returns 0, so a guest is told its handler took effect and can only find out
/// otherwise by dying; and the disposition it then reads back through `oldact`
/// is the SIG_IGN it wrote, not the SIG_DFL Linux would report. The net stops
/// the damage; this stops the lie.
///
/// `signum` is taken as the raw `u64` syscall argument rather than a `usize` so
/// the range check cannot be defeated by truncation: `usize` is 32-bit on
/// wasm32, and `sig as usize` would fold e.g. 0x1_0000_0009 onto SIGKILL.
pub fn sigaction_permitted(signum: u64, installing: bool) -> bool {
    // valid_signal() is 1..=_NSIG; NSIG here is that bound plus the unused
    // slot 0.
    if signum == 0 || signum >= NSIG as u64 {
        return false;
    }
    // sig_kernel_only(): the two the kernel reserves to itself.
    !(installing && (signum == SIGKILL as u64 || signum == SIGSTOP as u64))
}

/// The new blocked mask for `rt_sigprocmask(how, set, ...)`, or `None` for an
/// unrecognised `how` (-EINVAL).
///
/// # SIGKILL and SIGSTOP are dropped, not refused
///
/// This is deliberately NOT the `rt_sigaction` rule, and the asymmetry is the
/// kernel's, not a simplification. `rt_sigprocmask` does
/// `sigdelsetmask(&new_set, sigmask(SIGKILL)|sigmask(SIGSTOP))` and then
/// proceeds (`kernel/signal.c`): blocking either is silently ignored and the
/// call SUCCEEDS. Answering EINVAL instead would break ordinary guests, because
/// `sigfillset()` + `SIG_BLOCK` -- the standard "block everything across this
/// critical section" idiom, which dash, nginx and glibc's `pthread` bring-up
/// all use -- passes a set with both bits set on every single call.
///
/// The drop is applied to `newset`, before `how`, exactly where the kernel
/// applies it. For SIG_BLOCK and SIG_SETMASK that is indistinguishable from
/// masking the result. For SIG_UNBLOCK it is not: dropping the bits from
/// `newset` means an UNBLOCK request for SIGKILL is discarded rather than
/// honoured, so a mask that somehow already contained it keeps it. Modelling
/// the placement rather than the usual-case outcome is what keeps this true if
/// another path (`rt_sigsuspend` installs a mask verbatim today) ever puts one
/// of the two bits into `blocked`.
///
/// The mask is bookkeeping either way -- delivery ignores `blocked` for SIGKILL
/// at the point it is consumed -- so what this changes is the value the guest
/// reads back out of `oldset`, which before this was its own unsanitised bits.
pub fn sigprocmask_next(how: u64, prev: u64, newset: u64) -> Option<u64> {
    let newset = newset & !((1u64 << (SIGKILL - 1)) | (1u64 << (SIGSTOP - 1)));
    match how {
        SIG_BLOCK => Some(prev | newset),
        SIG_UNBLOCK => Some(prev & !newset),
        SIG_SETMASK => Some(newset),
        _ => None,
    }
}

/// The default-action ruling, tested as data.
///
/// These are the only signal tests in the crate that need nothing else: no
/// arena, no process table, no lifted code. The delivery path that consumes the
/// answer lives in `sched_tests` below.
#[cfg(test)]
mod default_action_tests {
    use super::*;

    const SIGINT: u32 = 2;
    const SIGABRT: u32 = 6;
    const SIGTERM: u32 = 15;
    const SIGTSTP: u32 = 20;

    fn dfl_table() -> [SigAction; NSIG] {
        [SigAction::dfl(); NSIG]
    }

    fn bit(sig: u32) -> u64 {
        1u64 << (sig - 1)
    }

    /// One process-directed signal, everything else at its default.
    fn posted(sig: u32) -> Option<u32> {
        terminating_signal(bit(sig), 0, 0, &dfl_table())
    }

    /// The whole point of the change: a terminating signal with no handler
    /// installed used to be recorded as pending and then consumed by nothing.
    #[test]
    fn an_uncaught_terminating_signal_is_a_termination() {
        assert_eq!(posted(SIGTERM), Some(SIGTERM));
        assert_eq!(posted(SIGINT), Some(SIGINT));
        assert_eq!(posted(1 /* SIGHUP */), Some(1));
        assert_eq!(posted(13 /* SIGPIPE */), Some(13));
    }

    /// Core is Term plus a dump this runtime does not produce, so it terminates.
    /// SIGABRT is the one that matters: `abort()` is `raise(SIGABRT)`, and every
    /// assertion failure in a guest goes through it.
    #[test]
    fn a_core_dumping_signal_terminates_too() {
        assert_eq!(posted(SIGABRT), Some(SIGABRT));
        assert_eq!(posted(11 /* SIGSEGV */), Some(11));
    }

    /// A caught signal has no default action left to take. If this were wrong
    /// every guest that installs a SIGTERM handler would die on its first one --
    /// which is nginx's master, PostgreSQL's postmaster, and dash.
    #[test]
    fn a_caught_signal_is_not_a_termination() {
        let mut acts = dfl_table();
        acts[SIGTERM as usize].handler = 0x4011_2233;
        assert_eq!(terminating_signal(bit(SIGTERM), 0, 0, &acts), None);
    }

    #[test]
    fn an_explicitly_ignored_signal_is_not_a_termination() {
        let mut acts = dfl_table();
        acts[SIGTERM as usize].handler = 1; // SIG_IGN
        assert_eq!(terminating_signal(bit(SIGTERM), 0, 0, &acts), None);
    }

    /// Blocked means PENDING, not delivered. A guest that blocks SIGTERM to read
    /// it from a signalfd must not be killed by the signal it is collecting.
    #[test]
    fn a_blocked_signal_stays_pending_instead_of_terminating() {
        assert_eq!(
            terminating_signal(bit(SIGTERM), 0, bit(SIGTERM), &dfl_table()),
            None
        );
    }

    /// ⚠️ The regression this whole change could have caused. SIGCHLD's default
    /// is Ign, and the runtime itself posts one to the parent of every process
    /// that exits -- so a rule of "SIG_DFL means die" would kill the parent of
    /// the first child to finish, in every multi-process guest there is.
    #[test]
    fn a_default_ignored_signal_is_not_a_termination() {
        assert_eq!(posted(SIGCHLD), None);
        assert_eq!(posted(23 /* SIGURG, the postgres latch */), None);
        assert_eq!(posted(28 /* SIGWINCH */), None);
        assert_eq!(posted(18 /* SIGCONT, with nothing stopped */), None);
    }

    /// SIGKILL ignores both filters, which is the only way to enforce
    /// "cannot be caught, cannot be blocked" in a runtime whose `rt_sigaction`
    /// accepts a disposition for it.
    #[test]
    fn sigkill_is_neither_catchable_nor_blockable() {
        let mut acts = dfl_table();
        acts[SIGKILL as usize].handler = 0x4011_2233;
        assert_eq!(
            terminating_signal(bit(SIGKILL), 0, u64::MAX, &acts),
            Some(SIGKILL),
            "a handler plus a full blocked mask must not make a task unkillable"
        );
        acts[SIGKILL as usize].handler = 1; // SIG_IGN
        assert_eq!(terminating_signal(bit(SIGKILL), 0, 0, &acts), Some(SIGKILL));
    }

    /// ⚠️ GUARDS A DELIBERATE NON-IMPLEMENTATION. Stop is not modelled, so a
    /// stop signal must do NOTHING -- and specifically must not be quietly
    /// rounded up to Term, which would turn a signal senders expect the process
    /// to come back from into a kill. See `terminating_signal` for the ruling.
    #[test]
    fn a_stop_signal_is_not_faked_as_a_termination() {
        for sig in [
            SIGSTOP, SIGTSTP, 21, /* SIGTTIN */
            22, /* SIGTTOU */
        ] {
            assert_eq!(posted(sig), None, "signal {sig} was rounded up to Term");
        }
        // ...and SIGSTOP is uncatchable in the other direction too: an installed
        // handler does not turn it into something deliverable.
        let mut acts = dfl_table();
        acts[SIGSTOP as usize].handler = 0x4011_2233;
        assert_eq!(terminating_signal(bit(SIGSTOP), 0, 0, &acts), None);
    }

    /// Lowest-numbered first, as Linux dequeues. Asserted with a terminating
    /// signal on BOTH sides so the answer cannot be right by the other one
    /// simply being declined.
    #[test]
    fn the_lowest_numbered_pending_signal_is_the_one_that_terminates() {
        assert_eq!(
            terminating_signal(bit(SIGINT) | bit(SIGTERM), 0, 0, &dfl_table()),
            Some(SIGINT)
        );
    }

    /// Both queues. A `tgkill` files into the task's own queue, and a check that
    /// read only the group's would let a `pthread_kill(SIGTERM)` sit forever --
    /// the exact defect `deliverable_set` exists to prevent.
    #[test]
    fn a_thread_directed_signal_terminates_as_well() {
        assert_eq!(
            terminating_signal(0, bit(SIGTERM), 0, &dfl_table()),
            Some(SIGTERM)
        );
    }

    /// The table itself, against `signal(7)` under the asm-generic numbering.
    /// Pinned because every other test here is only as good as this mapping,
    /// and one wrong row is a signal that silently changes category.
    #[test]
    fn the_default_action_table_matches_signal_7() {
        for (sig, want) in [
            (1, DefaultAction::Term),
            (2, DefaultAction::Term),
            (3, DefaultAction::Core),
            (4, DefaultAction::Core),
            (6, DefaultAction::Core),
            (9, DefaultAction::Term),
            (11, DefaultAction::Core),
            (13, DefaultAction::Term),
            (15, DefaultAction::Term),
            (17, DefaultAction::Ign),
            (18, DefaultAction::Cont),
            (19, DefaultAction::Stop),
            (20, DefaultAction::Stop),
            (23, DefaultAction::Ign),
            (28, DefaultAction::Ign),
            (31, DefaultAction::Core),
            (34, DefaultAction::Term), // the real-time range
        ] {
            assert_eq!(default_action(sig), want, "signal {sig}");
        }
    }
}

/// The admission rulings for `rt_sigaction` and `rt_sigprocmask`, tested as
/// data.
///
/// They live in `context` for exactly one reason: `lib.rs` gates `mod sys` to
/// `#[cfg(target_arch = "wasm32")]`, so a `#[test]` written beside either
/// syscall compiles for no target `cargo test` builds and would never run --
/// silently, in green. Same precedent as `arena::madvise_zeroes` and
/// `arena::mmap_round_len`.
#[cfg(test)]
mod sigmask_admission_tests {
    use super::*;

    const SIGHUP: u32 = 1;
    const SIGINT: u32 = 2;
    const SIGTERM: u32 = 15;

    fn bit(sig: u32) -> u64 {
        1u64 << (sig - 1)
    }

    /// The bits `rt_sigprocmask` is defined to discard.
    fn uncatchable() -> u64 {
        bit(SIGKILL) | bit(SIGSTOP)
    }

    // ---- rt_sigaction ----

    /// The TODO item's first half. Linux's `do_sigaction` refuses
    /// `sig_kernel_only()` signals, so `signal(SIGKILL, h)` fails with EINVAL
    /// rather than succeeding and quietly meaning nothing.
    #[test]
    fn installing_a_disposition_for_sigkill_or_sigstop_is_refused() {
        assert!(!sigaction_permitted(SIGKILL as u64, true));
        assert!(!sigaction_permitted(SIGSTOP as u64, true));
    }

    /// ⚠️ THE HALF THAT IS EASY TO GET WRONG. The kernel's condition is
    /// `act && sig_kernel_only(sig)` -- a pure QUERY of SIGKILL or SIGSTOP is
    /// legal and returns their (always SIG_DFL) disposition. A rule of "these
    /// two are always EINVAL" passes the test above and breaks the common
    /// `for (sig = 1; sig <= NSIG; sig++) sigaction(sig, NULL, &old)` sweep that
    /// programs use to snapshot inherited dispositions.
    #[test]
    fn querying_sigkill_or_sigstop_is_permitted() {
        assert!(
            sigaction_permitted(SIGKILL as u64, false),
            "sigaction(SIGKILL, NULL, &old) must succeed, as Linux does"
        );
        assert!(sigaction_permitted(SIGSTOP as u64, false));
    }

    /// The refusal is narrow: every other signal is admissible both ways,
    /// including the whole real-time range. Bounds the claim above so it cannot
    /// pass by refusing more than it should.
    #[test]
    fn every_other_signal_is_admissible_both_ways() {
        for sig in [SIGHUP, SIGINT, 8, SIGTERM, 10, SIGCHLD, 18, 20, 34, 64] {
            assert!(sigaction_permitted(sig as u64, true), "install {sig}");
            assert!(sigaction_permitted(sig as u64, false), "query {sig}");
        }
    }

    /// `valid_signal()`: 1..=64 and nothing else, whether installing or
    /// querying. Signal 0 is `kill`'s existence probe, never a disposition.
    #[test]
    fn out_of_range_signals_are_refused_either_way() {
        for sig in [0u64, NSIG as u64, 65, 100, u64::MAX] {
            assert!(!sigaction_permitted(sig, true), "install {sig}");
            assert!(!sigaction_permitted(sig, false), "query {sig}");
        }
    }

    /// The range check takes the raw 64-bit syscall argument, so it cannot be
    /// walked past by truncation. `usize` is 32 bits on wasm32, which is the
    /// only target `sys` compiles for: a check written as `sig as usize` would
    /// fold 0x1_0000_0009 onto SIGKILL and 0x1_0000_000f onto SIGTERM, indexing
    /// a valid slot for a signal number the kernel rejects outright. The host
    /// has a 64-bit `usize` and cannot reproduce that, which is exactly why the
    /// signature -- not the caller -- has to be what prevents it.
    #[test]
    fn a_signal_number_above_32_bits_is_not_truncated_into_range() {
        for sig in [(1u64 << 32) | 9, (1u64 << 32) | 15, 1u64 << 32] {
            assert!(!sigaction_permitted(sig, true), "install {sig:#x}");
            assert!(!sigaction_permitted(sig, false), "query {sig:#x}");
        }
    }

    // ---- rt_sigprocmask ----

    /// The TODO item's second half, and the asymmetry with `rt_sigaction`:
    /// blocking SIGKILL/SIGSTOP is not an error, it is a no-op. `Some` is half
    /// the assertion -- the call SUCCEEDS -- and the dropped bits are the other.
    #[test]
    fn blocking_sigkill_or_sigstop_succeeds_with_the_bits_dropped() {
        assert_eq!(
            sigprocmask_next(SIG_BLOCK, 0, uncatchable()),
            Some(0),
            "blocking only the two uncatchable signals must change nothing"
        );
        // sigfillset() + SIG_BLOCK: the idiom that made this matter. Everything
        // blocks except the two.
        assert_eq!(
            sigprocmask_next(SIG_BLOCK, 0, u64::MAX),
            Some(u64::MAX & !uncatchable())
        );
        assert_eq!(
            sigprocmask_next(SIG_SETMASK, 0, u64::MAX),
            Some(u64::MAX & !uncatchable())
        );
    }

    /// ⚠️ WHERE the drop is applied, not merely that it happens. Linux does
    /// `sigdelsetmask(&new_set, ...)` on the REQUEST and then applies `how`, so
    /// an UNBLOCK of SIGKILL is discarded rather than honoured. Masking the
    /// RESULT instead would clear the bit here and read identically on every
    /// BLOCK/SETMASK case above -- this is the only assertion that can tell the
    /// two implementations apart.
    #[test]
    fn the_drop_applies_to_the_request_not_the_result() {
        assert_eq!(
            sigprocmask_next(SIG_UNBLOCK, u64::MAX, uncatchable()),
            Some(u64::MAX),
            "an UNBLOCK request for the two must be discarded, not honoured"
        );
        // ...and an ordinary UNBLOCK in the same shape still works, so the
        // assertion above is not passing because UNBLOCK does nothing at all.
        assert_eq!(
            sigprocmask_next(SIG_UNBLOCK, u64::MAX, bit(SIGTERM)),
            Some(u64::MAX & !bit(SIGTERM))
        );
    }

    /// The three `how` values keep their ordinary meaning for every other
    /// signal; the sanitising must not have become a mask on the whole result.
    #[test]
    fn the_how_arms_are_otherwise_unchanged() {
        let prev = bit(SIGHUP) | bit(SIGTERM);
        assert_eq!(
            sigprocmask_next(SIG_BLOCK, prev, bit(SIGINT)),
            Some(prev | bit(SIGINT))
        );
        assert_eq!(
            sigprocmask_next(SIG_UNBLOCK, prev, bit(SIGHUP)),
            Some(bit(SIGTERM))
        );
        assert_eq!(
            sigprocmask_next(SIG_SETMASK, prev, bit(SIGINT)),
            Some(bit(SIGINT))
        );
    }

    /// An unrecognised `how` is EINVAL -- and the three that ARE recognised are
    /// asserted alongside, so `None` cannot come from a table that lost an arm.
    #[test]
    fn an_unrecognised_how_is_refused() {
        for how in [SIG_BLOCK, SIG_UNBLOCK, SIG_SETMASK] {
            assert!(sigprocmask_next(how, 0, 0).is_some(), "how={how}");
        }
        for how in [3u64, 4, u64::MAX] {
            assert_eq!(sigprocmask_next(how, 0, 0), None, "how={how}");
        }
    }
}

/// The signal state that belongs to the thread GROUP: the disposition table and
/// the PROCESS-directed pending queue. Inherited by fork like the fd table;
/// execve resets caught handlers to SIG_DFL (SIG_IGN survives).
///
/// What is NOT here, and used to be, is the blocked mask -- see `TaskSignals`.
#[derive(Clone)]
pub struct SignalState {
    pub actions: [SigAction; NSIG],
    /// Pending PROCESS-directed signals, bit `sig-1` -- the same bit numbering
    /// as sigset_t. Set by `kill` (which names a process) and by SIGCHLD; any
    /// member of the group that does not block the signal may take it. A
    /// thread-directed `tgkill` goes to `TaskSignals::pending` instead.
    ///
    /// Standard signals do not queue: re-raising an already-pending signal is a
    /// no-op, which is why the latch fixture handshakes between rounds.
    pub pending: u64,
}

impl Default for SignalState {
    fn default() -> SignalState {
        SignalState {
            actions: [SigAction::dfl(); NSIG],
            pending: 0,
        }
    }
}

/// The signal state that belongs to ONE TASK. POSIX is explicit that the
/// blocked mask is per thread, and treating it as per process is not a subtle
/// deviation: **glibc's own `pthread_create` blocks every signal but SIGSETXID
/// while the new thread starts**, so a shared mask leaves the WHOLE PROCESS
/// deaf after its first thread. Measured before this split, at a sleep boundary
/// with SIGUSR1 posted and a handler installed:
///
/// ```text
/// [sleep] pid=1 resumed: pending=0x200 blocked=0xfffffffeffffffff left=249 ms
/// ```
///
/// Every bit but 32 (SIGSETXID) set, so `pending & !blocked` was 0 and nothing
/// could ever be delivered.
///
/// `pending` here is the THREAD-directed queue (`tgkill`, which is what
/// `pthread_kill` issues). Linux keeps both queues and a thread may dequeue
/// from either; `deliver_pending_signals` unions them.
#[derive(Clone, Copy, Default)]
pub struct TaskSignals {
    pub blocked: u64,
    pub pending: u64,
    /// The `blocked` mask that `rt_sigsuspend` displaced, held while it waits so
    /// the resume path can put it back. `Some` only between entering sigsuspend
    /// and returning from it.
    ///
    /// Per TASK, and it always was per something: a suspended sigsuspend yields
    /// to the scheduler, so two tasks can be inside one at the same time. Held
    /// on the context instead, the second would skip saving (a slot was already
    /// occupied) and then restore the FIRST task's mask over its own.
    pub sigsuspend_saved: Option<u64>,
}

/// What the scheduler decided, after a process suspended or exited.
///
/// This used to be nothing: `schedule_after_suspend` looped internally until
/// something was runnable, blocking in the host to wait, and called
/// `std::process::exit` when it decided the run was over. That is correct for a
/// host that can block and fatal for one that cannot -- a browser event loop has
/// to be given control back, not slept on.
///
/// Returning the decision instead lets the SAME scheduler serve both: a blocking
/// backend never yields `Idle`, because its `wait` does not return until
/// something is ready, so the command profile behaves exactly as before.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum SchedOutcome {
    /// A process is loaded and ready to run.
    Ready,
    /// Nothing is runnable and the backend declined to block. `wake_at` is the
    /// soonest guest deadline in absolute nanos, and `io` says whether anything
    /// is parked on a socket -- between them, everything a host needs to decide
    /// when to call back.
    Idle { wake_at: Option<u128>, io: bool },
    /// Every remaining process is blocked with no wake source. NOT completion:
    /// exiting 0 here made every hang look like a clean run.
    Deadlock,
    /// Nothing left to run. Carries init's exit code.
    Exited(i32),
}

/// One file-backed `MAP_SHARED` region, keyed by the path that names it.
pub struct ShmFile {
    /// Absolute path of the backing file, as resolved at first map. This is the
    /// sharing key: a second `mmap` of the same path lands on `vma`.
    pub path: Vec<u8>,
    /// Start of the region in the shared window.
    pub vma: u64,
    /// Page-rounded length of the region.
    pub len: usize,
    /// Whether any mapping of this region has EVER been able to write to it.
    ///
    /// Accumulated, never cleared: it is a property of the REGION's history, not
    /// of the mapping that happens to be asking. `shm_file_reclaimable` is the
    /// one consumer, and the invariant it needs is "no byte can have been
    /// written through this region", which one read-only mapper cannot establish
    /// on its own if a writable mapper came before or after it.
    pub writable: bool,
}

/// Whether a file-backed shared region may be recycled once nothing maps it.
///
/// THE RULE. A region is reclaimable unless bytes may have been written through
/// it AND the name that would let somebody go looking for those bytes still
/// resolves. Both halves are needed, and each on its own is wrong:
///
///   - `path_exists` alone is what this used to be, and it is what pinned the
///     window. glibc `MAP_SHARED`s three files that are never unlinked --
///     `/usr/lib/locale/locale-archive`, `/etc/locale.alias`, and the gconv
///     modules cache -- so any guest that touches locales or iconv held 3.2 MiB
///     of the shared window forever. Because the window's floor is the top of
///     the PRIVATE mmap area, that is not a 3.2 MiB loss: it froze `shm_top`
///     at 0x10fd0000 and collapsed the private region from 96 MiB to 15.8 MiB,
///     which is where PostgreSQL's postmaster met `FATAL: could not map
///     anonymous shared memory ... (currently 78618624 bytes)`.
///   - `writable` alone would hold a region whose name is ALREADY gone, and
///     whose bytes are by that same fact unreachable -- a pure leak, and it is
///     the path PostgreSQL's `posix` DSM backend actually retires through
///     (`shm_unlink`, then the last unmap). Four such regions come and go before
///     the postmaster starts.
///
/// WHY WRITABILITY IS THE HONEST DISCRIMINATOR, not a proxy for one. The reason
/// a named region must be preserved is that a later `mmap` of the same name is
/// entitled to the bytes an earlier mapper wrote through it -- and this runtime
/// never writes a `MAP_SHARED` mapping back to the file, so those bytes exist
/// ONLY in the region. If no mapping of the region was ever writable, no such
/// byte can exist, and a later mapper rebuilding the region from the file gets
/// everything it would have got from the old one. That is the premise of the
/// rule being false, not a correlate of it.
///
/// This deliberately does NOT test the path prefix (`/dev/shm/...`) or the VFS
/// layer the file came from:
///
///   - a `/dev/shm` prefix would miss PostgreSQL's `mmap` DSM backend outright,
///     which maps `$PGDATA/pg_dynshmem/...` `MAP_SHARED` with exactly the
///     cross-process expectations the `posix` backend has;
///   - "the file lives in the read-only rfs lower layer, so it cannot change"
///     answers a question nobody asked. The file changing is not the hazard;
///     the region diverging from the file is, and it diverges the moment a
///     writable mapping stores into it -- image-backed or not.
///
/// ⚠️ Writability has to come from every route to it. `NR_MMAP` reads `prot`,
/// and `NR_MPROTECT` -- which is otherwise a no-op, since the flat arena has no
/// protection to enforce -- marks the region too, because "was never mapped
/// writable" is a claim about permission and `mprotect` is the other way to
/// acquire it.
pub fn shm_file_reclaimable(writable: bool, path_exists: bool) -> bool {
    !(writable && path_exists)
}

pub struct EcvContext {
    pub arena: Arena,
    pub vfs: Vfs,
    /// The network transport. A concrete type chosen at COMPILE time, not a
    /// trait object -- see `crate::net` for why a `dyn` here would put every
    /// backend's imports into the module and defeat the whole seam.
    pub net: crate::net::Net,
    /// The readiness generation this scheduler has already acted on.
    ///
    /// Only meaningful for a backend that cannot block; see the `WouldBlock`
    /// arm in `resume_scheduling`.
    last_ready_gen: u64,
    pub fds: Vec<Option<OpenFile>>,
    /// Close-on-exec flags for the CURRENT process, parallel to `fds`. `fd >=
    /// cloexec.len()` reads as not-cloexec. `execve` closes every fd whose flag is
    /// set; fork inherits it; `dup`/`dup2` clear it on the new fd; `F_SETFD`,
    /// `O_CLOEXEC`, `F_DUPFD_CLOEXEC`, `SOCK_CLOEXEC` set it.
    pub cloexec: Vec<bool>,
    /// O_NONBLOCK flags for the CURRENT process, parallel to `fds`. Set by
    /// `ioctl(FIONBIO)`, `fcntl(F_SETFL)` and the `O_NONBLOCK`/`SOCK_NONBLOCK`
    /// open flags; read by the pipe and socketpair paths, which are the ones that
    /// can park a process forever. Host sockets do not consult it: a would-block
    /// there suspends cooperatively and the scheduler polls the fd, so the guest
    /// resumes on its own. An internal socketpair has no such backstop -- nothing
    /// external will ever make it readable -- which is why nginx's workers hung in
    /// the channel `recvmsg` that should have returned EAGAIN.
    pub nonblock: Vec<bool>,
    pub cwd: Vec<u8>,
    pub uid: u32,
    pub gid: u32,
    /// Signal dispositions + blocked mask of the CURRENTLY-running process
    /// (snapshotted into `Process.signals` on a context switch, like `fds`).
    pub signals: SignalState,
    /// The RUNNING task's signal state. Travels with `state`/`call_history` (by
    /// task index) rather than with the group's tables.
    pub task_signals: TaskSignals,
    /// Set by a yielding syscall so the svc trampoline knows to capture the
    /// replay state and start the return rather than continue normally.
    /// Consumed (cleared) by the trampoline.
    pub suspended: bool,
    /// Set by `__remill_jump` when it recognises a NON-LOCAL jump (a `longjmp`)
    /// and records a replay for it, so the scheduler's leg loop re-enters THIS
    /// process immediately instead of treating the unwind as a yield.
    ///
    /// ❗ Deliberately not `suspended`. A `longjmp` is not a scheduling point on
    /// Linux, and routing it through `schedule_after_suspend` would let another
    /// process run in the middle of one -- harmless under a cooperative
    /// scheduler, but it would make a `longjmp` observably preempt, which is a
    /// behaviour nothing asked for and which `retire_after_suspend` reads as a
    /// process that yielded with nothing pending.
    pub longjmp_pending: bool,
    // `unwinding` USED TO BE A FIELD HERE. It is now a wasm global -- see
    // `unwinding()` / `set_unwinding()` below -- because the lifted code reads it
    // after every guest call and every syscall, and a global.get is one
    // instruction where a call was 33,540 of them per bash-sized module. Nothing
    // about its meaning or lifetime changed: there is exactly one EcvContext, so
    // a field on it and a module-wide global are the same object.
    /// Set by the scheduler right before it re-enters a process at a syscall-resume
    /// point (the innermost replay frame), so the resumed handler takes its resume
    /// path instead of blocking again. Consumed (cleared) by `svc` after dispatch.
    pub resuming: bool,
    /// Program registry + path resolver, for execve.
    pub programs: Programs,
    /// Registry indices of LIBRARY units (the shared-libc superset unit) — programs
    /// with no exec-map path, merged into whichever binary runs and re-merged after
    /// each execve. Derived from the exec map at construction; empty for the
    /// full-fuse execve model and single-program modules. See merge_libraries.
    pub library_units: Vec<usize>,
    // --- dlopen units ---
    /// Guest `.so` path -> unit index, from the sidecar. Empty when the image
    /// has no dlopen-able units, which is the common case today.
    pub dlmap: crate::dlmap::DlMap,
    /// The loader backend, chosen at compile time. See `crate::loader`.
    pub loader: crate::loader::Loader,
    /// Hashes of units asked for before they had a registry index, in ask order.
    ///
    /// A host-driven loader cannot be given a registry index for the unit it is
    /// about to place: the descriptor lives in the side module, so nothing is
    /// registered until the host has instantiated it. The guest still has to
    /// park on SOMETHING, and `ecv_side_loaded` still has to name the same
    /// something when it wakes it. This vec supplies that stable token --
    /// position `i` becomes `PENDING_UNIT_BASE + i` -- and the host merely
    /// echoes back whatever it was handed.
    ///
    /// Kept per-CONTEXT rather than per-process because the token identifies a
    /// unit, not a caller: two processes dlopening the same plugin must park on
    /// the same token so one host load wakes both.
    pub pending_units: Vec<Vec<u8>>,
    // --- process model (M4) ---
    /// Process table. procs[current] is the running process (its live data is
    /// in the fields above); the others hold snapshots.
    pub procs: Vec<Process>,
    pub current: usize,
    /// Which process's memory is physically in the live arena right now. Equal to
    /// `current` except between a `save_current` and the `load_current` that
    /// follows it, and it is what lets `load_current` skip the restore when the
    /// same process is re-loaded. Only `load_current` and `exec_into` change what
    /// the live arena holds; the idle paths in between (host poll, deadline
    /// sweep, signal posting) never touch guest memory.
    live_owner: usize,
    /// Which program's dispatch tables (`funcs`, `bb_maps`) are currently built,
    /// so a context switch between processes running the SAME program — the
    /// nginx master/worker case, and every fork that has not execve'd — reuses
    /// them instead of rebuilding. `None` until the first build.
    tables_prog_idx: Option<usize>,
    /// Dispatch tables for programs that are not the live one, indexed by
    /// program index. Together with `funcs`/`bb_maps` this holds AT MOST ONE
    /// copy per program: a switch parks the live tables in the slot for the
    /// program they belong to and takes the incoming program's out.
    ///
    /// # Why
    ///
    /// `tables_prog_idx` alone is a one-slot cache, so two programs ping-ponging
    /// miss on every switch -- measured 2026-08-17 at 602 rebuilds in 600
    /// cross-program switches against 1 in 600 same-program ones, and a rebuild
    /// is 13.9 ms against a 36 us switch. That is the postgres shape exactly:
    /// dash execs psql while the postmaster is still program 0.
    ///
    /// It is sound because `build_tables` is a pure function of `&EcvProgram`,
    /// which is `&'static` -- there is nothing for a cached entry to go stale
    /// against. The cost is measured, not estimated: `RAPTORMARK_ECV_TABLES`
    /// reports 314 KiB per program for a static-glibc guest, against 384 MiB for
    /// one arena.
    tables_cache: Vec<Option<DispatchTables>>,
    pub run_queue: VecDeque<usize>,
    /// Rotating offset for the two wake paths, so processes contending for the
    /// same readiness event take turns being enqueued first. Advanced only when a
    /// wake actually had more than one claimant. See `wake_pollers` and
    /// `poll_sockets_and_wake`.
    wake_cursor: usize,
    pub next_pid: u32,
    /// Shared pipe table (pipe buffers persist across fork; fds reference them).
    pub pipes: Vec<Pipe>,
    /// Which program's image is currently materialised in the live arena.
    ///
    /// The arena is a single shared buffer, so a switch between processes
    /// running DIFFERENT programs has to reload the image: the read-only text
    /// and rodata are identical only within one program. Measured: a
    /// cross-program switch differs by the whole 57 MiB image.
    ///
    /// This was dead whenever the full-buffer scheme was selected -- each
    /// process carried its own image -- and that scheme is gone, so it is now
    /// live on every switch. A `Full` snapshot (a multi-threaded group) still
    /// carries its own image and merely makes the reload redundant.
    materialized_prog: usize,
    /// Context-global table of in-memory regular files, indexed by
    /// `OpenFile::Mem::file`. Shared for the same reason `pipes` is: the object
    /// outlives any one descriptor and is visible to every process.
    pub open_files: Vec<MemFile>,
    /// Context-global table of file OFFSETS, indexed by `OpenFile::Mem::off`.
    ///
    /// This is the level Linux calls an open file description (`struct file`)
    /// and this runtime did not have. `open_files` above is the INODE -- one
    /// buffer per path, joined by `mem_file_for` -- and the descriptor held the
    /// offset directly, which put the two levels of the model into one. Linux
    /// has three: a descriptor points at a description, which points at an
    /// inode, and `dup`/`fork` copy the POINTER while a fresh `open` makes a new
    /// description. That is exactly the distinction the offset needs, and
    /// without it `dup2(fd, 5); read(fd); read(5)` re-read the same bytes.
    ///
    /// Only the offset lives here. `file` and `writable` stay in the descriptor
    /// although Linux keeps their equivalents in `struct file` too, and that is
    /// deliberate rather than an omission: both are fixed when the descriptor is
    /// created and copied verbatim by every clone, so no guest can observe the
    /// difference. Moving them would touch every match arm to no observable end.
    pub file_offsets: Vec<FileOffset>,
    /// Context-global table of AF_UNIX sockets bound to a filesystem path.
    /// Context-global rather than per-process for the same reason `pipes` is:
    /// the whole point of a named socket is that a DIFFERENT process finds it,
    /// and every guest process lives inside this one module. Entries are never
    /// removed, only marked `dead`, because `OpenFile::UnixSocket` holds an
    /// index into this vector.
    pub unix_listeners: Vec<UnixListener>,
    /// Context-global shared-memory segments (`mmap(MAP_SHARED|MAP_ANONYMOUS)`).
    /// These VMA ranges are exempt from the per-process arena restore, so the
    /// single physical copy persists across cooperative context switches and is
    /// visible to every process (including fork children — the registry is
    /// context-global and the VMA is reserved before the fork). See
    /// `.agents/docs/SHAREDMEM.md`.
    pub shared_segments: Vec<SharedSeg>,
    /// File-backed MAP_SHARED mappings, keyed by the file's absolute path.
    ///
    /// The identity is the point. POSIX shared memory works by two processes
    /// `shm_open`-ing the SAME name and mmap-ing it MAP_SHARED, so the second
    /// mapping has to land on the region the first one got -- that is what makes
    /// the bytes shared. PostgreSQL's `posix` dynamic-shared-memory backend is
    /// exactly this: the postmaster creates `/PostgreSQL.<n>` under /dev/shm and
    /// each backend maps the same name.
    pub shm_files: Vec<ShmFile>,
    /// Descending allocator for SHARED mappings, from the top of the mmap
    /// window down.
    ///
    /// Shared regions cannot come from `arena.mmap_cur`, which is per-process
    /// and travels with the arena on a context switch: every forked child
    /// restarts its bump at the same place, so two processes creating different
    /// POSIX shm segments both landed on the same VMA and, being exempt from the
    /// swap, aliased each other. Observed as two distinct
    /// `/dev/shm/PostgreSQL.<n>` names mapped to 0x14c01000 and then
    /// `munmap_chunk(): invalid pointer` from the corrupted heap.
    pub shm_window: ShmWindow,
    /// Nesting depth of guest signal handlers currently running.
    ///
    /// Only `sigaltstack` reads it, and only to answer SS_ONSTACK honestly:
    /// `run_signal_handler` builds the handler frame below the interrupted SP,
    /// which is not a stack the guest allocated, and glibc's `____longjmp_chk`
    /// asks exactly this question before allowing a longjmp out of a handler.
    /// Answering "no alternate stack" made it refuse with
    /// `*** longjmp causes uninitialized stack frame ***`.
    pub in_signal_handler: u32,
    /// System V shared-memory segments (context-global, like `pipes` and
    /// `shared_segments`; shared across cooperatively-scheduled processes).
    pub shm: Vec<ShmSeg>,
    pub next_shmid: i32,
    /// Raw pointer to the single live State (fixed address), set by entry.
    pub live_state: *mut State,
    /// What the current process suspended for.
    pub pending: Pending,
    /// The init process's exit code — the module's exit code once nothing runs.
    pub exit_code: i32,
    /// Live call-history stack of the CURRENTLY-running process (fork_emulation):
    /// {func_vma, return_pc} pushed at each lifted prologue, popped at its
    /// epilogue (intrinsics.rs). Snapshotted into `Process.call_history` on a
    /// context switch; a fork child's copy seeds the replay driver.
    pub call_history: Vec<(u64, u64)>,
    /// Whether the inlined call history is LIVE for this run. Read once at
    /// construction; see `inline_call_history_enabled`.
    pub ch_inline: bool,
    /// The buffer address most recently handed to the lifted code.
    ///
    /// The lifted fast path writes frames through the
    /// published base; if the vector reallocates and nothing republishes, those
    /// writes land in a freed allocation -- which shares one linear memory with
    /// the guest arena, so it corrupts GUEST memory and surfaces as the guest's
    /// own stack canary firing. This records what was published so a mismatch
    /// can be caught at the boundary instead of inferred from the wreckage.
    pub ch_published: *const (u64, u64),
    /// getrandom PRNG state (xorshift64*). NON-cryptographic: a deterministic
    /// stream is fine for the offline model, but it MUST vary between calls --
    /// OpenSSL 3.x's DRBG runs a repetition-count health test on its entropy
    /// source and rejects a constant fill, which failed PostgreSQL's cancel-key /
    /// auth `pg_strong_random` (RAND_bytes). Context-global (advances across all
    /// processes); never zero.
    pub rng_state: u64,
    /// Lifted function table, sorted by VMA (the active program).
    funcs: Vec<(u64, LiftedFunc)>,
    /// Per-function basic-block address maps for indirect branches:
    /// (fn_vma, sorted [(bb_vma, block_addr_ptr)]).
    bb_maps: Vec<(u64, Vec<(u64, *mut u64)>)>,
}

/// Sets the initial register state for a program: entry PC, stack pointer, and
/// the aarch64 system-register defaults upstream `Entry.cpp` uses.
pub fn setup_state(state: &mut State, prog: &EcvProgram, sp: u64) {
    state.gpr.sp.val = sp;
    state.gpr.pc.val = prog.entry_pc();
    state.sr.tpidr_el0 = crate::arena::THREAD_PTR;
    state.sr.midr_el1 = 0xf0510;
    state.sr.ctr_el0 = 0x8003_8003;
    state.sr.dczid_el0 = 0x4;
}

/// Builds the sorted dispatch tables from a program descriptor (mirroring
/// upstream `Entry.cpp` main()). Shared by initial entry and execve.
/// One program's dispatch tables: its functions by VMA, and its basic-block map
/// per function. What `build_tables` returns and what `tables_cache` holds.
pub type DispatchTables = (Vec<(u64, LiftedFunc)>, Vec<(u64, Vec<(u64, *mut u64)>)>);

pub fn build_tables(prog: &EcvProgram) -> DispatchTables {
    let mut funcs = Vec::new();
    unsafe {
        let mut i = 0;
        while *prog.fun_vmas.add(i) != 0 {
            match *prog.fun_ptrs.add(i) {
                Some(f) => funcs.push((*prog.fun_vmas.add(i), f)),
                None => break,
            }
            i += 1;
        }
    }
    funcs.sort_by_key(|&(vma, _)| vma);

    let mut bb_maps = Vec::new();
    unsafe {
        let n = prog.block_count() as usize;
        for i in 0..n {
            let bb_num = *prog.block_sizes.add(i) as usize;
            let vmas = *prog.block_vmas.add(i);
            let ptrs = *prog.block_ptrs.add(i);
            let mut m: Vec<(u64, *mut u64)> =
                (0..bb_num).map(|j| (*vmas.add(j), *ptrs.add(j))).collect();
            m.sort_by_key(|&(vma, _)| vma);
            m.dedup_by_key(|e| e.0);
            bb_maps.push((*prog.block_fn_vmas.add(i), m));
        }
    }
    bb_maps.sort_by_key(|&(vma, _)| vma);
    (funcs, bb_maps)
}

/// True only the FIRST time this `(fn, bb)` pair misses.
///
/// Capping by unique pair rather than by occurrence is the point. An occurrence
/// cap lets one hot pair crowd out every rare one -- and removing the cap made
/// the guest so slow it never reached the crash the dump was meant to explain,
/// after which "the suspect never appears in 39,893 lines" looked like evidence
/// and was not. (⚠️ This cited `.agents/docs/JOURNAL.md`; that account is gone
/// from JOURNAL.md and LTM/ alike as of 2026-08-25, so this comment is now the
/// record.) An instrument verbose enough to
/// change the outcome has stopped measuring the thing.
fn bbmiss_first_time(fn_vma: u64, bb_vma: u64) -> bool {
    // `Mutex::new` is const, so this needs no LazyLock -- which matters here:
    // a lazy initialiser on the fork path is what caused the post-fork
    // `panic_poisoned` infinite loop (see sys.rs).
    static SEEN: std::sync::Mutex<Vec<(u64, u64)>> = std::sync::Mutex::new(Vec::new());
    let Ok(mut seen) = SEEN.lock() else {
        return false;
    };
    if seen.len() >= 4096 || seen.contains(&(fn_vma, bb_vma)) {
        return false;
    }
    seen.push((fn_vma, bb_vma));
    true
}

/// The `[bbmiss]` line for one indirect-branch miss.
///
/// Split out of `block_address` for one reason: it is the only part of that
/// diagnostic a host test can reach. `block_address` needs a live `ContextInner`
/// -- a 384 MiB arena, real lifted function pointers, and a `fatal!` on the
/// no-catch-all path -- so the format string itself is otherwise only observable
/// from the env-gated E2E suite.
///
/// # ⚠️ Why there is no `insn=` field
///
/// There was one, and it never carried a real encoding. It read the guest word
/// with `self.arena.slice(bb_vma, 4)`, and **the arena does not contain guest
/// .text**: the arena is filled only by `Arena::load_data_sections`, from
/// `EcvProgram`'s `data_*` tables, and elfconv's `MainLifter::SetDataSections`
/// skips every `SEC_TYPE_CODE` section when it builds them. So the read yields
/// zeros. All 138 `bbmiss` lines from a ruby run printed `insn=0x00000000`,
/// including one whose address holds `1e688800 fnmul d0, d0, d8`.
///
/// A fabricated `0x00000000` is worse than no encoding at all: it is a valid
/// aarch64 word (`udf #0`), so it reads as a real answer and sends the next
/// reader looking for a UDF that is not there. The fused ELF is not available at
/// run time, so `bb=` plus a disassembler is the honest instruction. The same
/// removal, for the same reason, is recorded on `diag::undecoded_message`.
fn bbmiss_message(
    fn_vma: u64,
    bb_vma: u64,
    nblocks: usize,
    has_catchall: bool,
    near: &[u64],
) -> String {
    format!("fn=0x{fn_vma:x} bb=0x{bb_vma:x} nblocks={nblocks} has_catchall={has_catchall} near={near:x?}")
}

#[cfg(test)]
mod bbmiss_message_tests {
    use super::bbmiss_message;

    /// The fields that make the line actionable: which function, which block,
    /// how big the map is, whether the catch-all that is about to be taken even
    /// exists, and the neighbouring block starts.
    #[test]
    fn names_the_function_the_block_and_the_map() {
        let m = bbmiss_message(0x4e1000, 0x40053c, 12, true, &[0x400538, 0x400540]);
        assert!(m.contains("fn=0x4e1000"), "{m}");
        assert!(m.contains("bb=0x40053c"), "{m}");
        assert!(m.contains("nblocks=12"), "{m}");
        assert!(m.contains("has_catchall=true"), "{m}");
        assert!(m.contains("400538"), "the near list is the lead: {m}");
    }

    /// ⚠️ Guards a REMOVAL, and the reason it can never come back by reading the
    /// arena. `insn=` was fed by `arena.slice(bb_vma, 4)`, and the arena is
    /// loaded only from `EcvProgram`'s `data_*` tables, which elfconv builds with
    /// every `SEC_TYPE_CODE` section skipped -- so the read returned zeros for an
    /// address holding a real `fnmul`. `0x00000000` is a valid aarch64 `udf #0`,
    /// so the fabricated field read as an answer.
    ///
    /// Asserted on inputs whose own hex contains no run of eight zeros, so the
    /// only way this line can appear is a re-added encoding field.
    #[test]
    fn does_not_claim_an_encoding_it_cannot_read() {
        let m = bbmiss_message(0x4e1000, 0x40053c, 12, true, &[0x400538]);
        assert!(
            !m.contains("insn"),
            "the arena holds no guest .text, so any encoding here is invented: {m}"
        );
        assert!(
            !m.contains("0x00000000"),
            "`udf #0` is a real instruction and reads as an answer: {m}"
        );
    }
}

impl EcvContext {
    /// Wires the runtime state around the entry program's dispatch tables.
    pub fn new(
        arena: Arena,
        vfs: Vfs,
        cwd: Vec<u8>,
        uid: u32,
        gid: u32,
        programs: Programs,
        entry_idx: usize,
    ) -> EcvContext {
        let (funcs, bb_maps) = build_tables(programs.get(entry_idx));
        let n_progs = programs.len();
        // Read here rather than in entry.rs so every construction path gets it,
        // the way `library_units` is derived rather than passed.
        let dlmap = crate::dlmap::DlMap::load(
            programs.regs(),
            vfs.read(b"/", crate::dlmap::DL_PATH).as_deref(),
        );

        // ❗ A DLOPEN-ABLE UNIT MUST NOT BE A LIBRARY UNIT.
        //
        // `library_indices` classifies by ABSENCE from the exec map -- a program
        // with no entry path is taken to be the shared-libc superset unit and is
        // merged into whichever binary runs, at startup and after every execve.
        // A dlopen'd plugin has no exec-map path either, so it fell into exactly
        // that bucket and was merged EAGERLY.
        //
        // That silently defeated the whole deferral: every unit's data was
        // loaded into the arena before the guest ran, so "the merge is what was
        // deferred" was not true of any build. Nothing failed -- it was slower
        // and larger than claimed, which is the kind of wrong that does not
        // announce itself.
        //
        // Subtraction rather than a new classification, deliberately: the exec
        // map still decides what an ENTRY is, and this only removes the units
        // the dlopen map says are loaded on demand.
        let library_units = library_units_for(&programs, &dlmap);
        EcvContext {
            arena,
            vfs,
            net: crate::net::Net::default(),
            last_ready_gen: 0,
            fds: vec![
                Some(OpenFile::Stdio(0)),
                Some(OpenFile::Stdio(1)),
                Some(OpenFile::Stdio(2)),
            ],
            cloexec: vec![false; 3], // stdio is never close-on-exec
            nonblock: vec![false; 3],
            cwd,
            uid,
            gid,
            signals: SignalState::default(),
            task_signals: TaskSignals::default(),
            suspended: false,
            longjmp_pending: false,
            resuming: false,
            programs,
            library_units,
            dlmap,
            loader: crate::loader::Loader::default(),
            pending_units: Vec::new(),
            procs: vec![Process {
                units: Vec::new(),
                dlerror: None,
                pid: 1,
                ppid: 0,
                tgid: 1,
                status: ProcStatus::Runnable,
                started: false,
                prog_idx: entry_idx,
                blocked_on: BlockedOn::None,
                arena: None,
                state: None,
                fds: None,
                cloexec: None,
                nonblock: None,
                cwd: None,
                signals: None,
                task_signals: TaskSignals::default(),
                call_history: None,
                replay: None,
                deadline: None,
                timed_out: false,
                clear_child_tid: 0,
                // Linux's boot value for an ordinary (non-setuid) process:
                // `PR_GET_DUMPABLE` before any `PR_SET_DUMPABLE` reads 1.
                dumpable: SUID_DUMP_USER,
                thp_disable: THP_NOT_DISABLED,
            }],
            current: 0,
            live_owner: 0,
            tables_prog_idx: None,
            tables_cache: (0..n_progs).map(|_| None).collect(),
            run_queue: VecDeque::new(),
            wake_cursor: 0,
            next_pid: 2,
            pipes: Vec::new(),
            // entry.rs loads the entry program's image before building the
            // context, so the arena already holds it.
            open_files: Vec::new(),
            file_offsets: Vec::new(),
            materialized_prog: entry_idx,
            unix_listeners: Vec::new(),
            shared_segments: Vec::new(),
            shm_files: Vec::new(),
            shm_window: ShmWindow::new(),
            in_signal_handler: 0,
            shm: Vec::new(),
            next_shmid: 1,
            live_state: core::ptr::null_mut(),
            pending: Pending::None,
            exit_code: 0,
            // Preallocated to the recursion-alarm depth. `push` runs once per
            // guest BL -- 32,972 call sites in a linked bash module -- and a
            // growing Vec pays a capacity branch on every one of them plus a
            // realloc-and-copy at each doubling. 16,384 frames is 256 KiB, which
            // is nothing beside a 384 MiB arena, and it is the depth at which
            // `report_runaway_recursion` gives up anyway.
            call_history: Vec::with_capacity(crate::diag::DEFAULT_MAX_DEPTH),
            ch_inline: inline_call_history_enabled(),
            ch_published: core::ptr::null(),
            // Fixed non-zero seed: the stream only needs to VARY, not be
            // unpredictable, in the offline model. See `rng_state`.
            rng_state: 0x9e37_79b9_7f4a_7c15,
            funcs,
            bb_maps,
        }
    }

    /// Draws `count` pseudo-random bytes into the guest buffer at `buf` (the
    /// getrandom source), advancing the context RNG. xorshift64* -- fast, and its
    /// output varies enough to satisfy OpenSSL's DRBG entropy health checks. See
    /// `rng_state`.
    pub fn fill_random(&mut self, buf: u64, count: usize) {
        let bytes = self.random_bytes(count);
        self.arena.slice_mut(buf, count).copy_from_slice(&bytes);
    }

    /// Returns `n` pseudo-random bytes (xorshift64*, advancing the context RNG).
    /// Backs both getrandom and the synthetic /dev/urandom device. See `rng_state`.
    pub fn random_bytes(&mut self, n: usize) -> Vec<u8> {
        let mut out = Vec::with_capacity(n);
        let mut s = self.rng_state;
        while out.len() < n {
            // xorshift64*
            s ^= s >> 12;
            s ^= s << 25;
            s ^= s >> 27;
            let r = s.wrapping_mul(0x2545_F491_4F6C_DD1D);
            let take = (n - out.len()).min(8);
            out.extend_from_slice(&r.to_le_bytes()[..take]);
        }
        self.rng_state = s;
        out
    }

    /// SPIKE (shared-libc mechanism proof, .agents/docs/DYNLINK.md): merges the
    /// dispatch tables (function table + basic-block address maps) of EVERY other
    /// registered program into the current one, and loads their data sections into
    /// the single shared arena. After this, a `blr`/`br` from the entry unit's
    /// lifted code to a VMA that lives in a SEPARATELY-lifted unit resolves
    /// in-process through `func_at`/`func_containing`/`block_address` — the
    /// cross-unit call that shared-libc needs (one lifted libc called by many
    /// binaries, without execve program-switching).
    ///
    /// This is gated (entry.rs reads `RAPTORMARK_SHARED_UNITS`) and is NOT the
    /// default: the multi-binary execve path deliberately keeps per-program
    /// isolation (each unit is a full ET_EXEC that may reuse the same VMAs, and
    /// `execve` SWITCHES the active program's tables rather than merging them).
    /// Merging is only sound when the units occupy DISJOINT VMA ranges — exactly
    /// what a fixed-base shared library guarantees.
    ///
    /// Requires the merged VMA sets to be globally unique (the binary searches in
    /// `func_at`/`block_address` assume sorted, deduped keys); a fixed
    /// non-overlapping base per unit is what makes that hold.
    pub fn merge_shared_units(&mut self, entry_idx: usize) {
        let all: Vec<usize> = (0..self.programs.len())
            .filter(|&i| i != entry_idx)
            .collect();
        self.merge_units(&all, true);
    }

    /// Merges the LIBRARY units (self.library_units — the shared-libc superset unit)
    /// into the live dispatch tables AND loads their data into the arena, so a
    /// binary's `blr`/`br` into a libc VMA resolves in-process into the ONE lifted
    /// libc. Called at startup (entry.rs) and RE-CALLED by exec_into after every
    /// execve (the arena reset there wipes libc's data + the new binary's tables
    /// replace the func table, so libc must be re-merged — with PRISTINE data, which
    /// is correct execve semantics: the new image re-initializes libc). No-op when
    /// there are no library units (full-fuse execve model / single program).
    pub fn merge_libraries(&mut self) {
        if self.library_units.is_empty() {
            return;
        }
        let libs = self.library_units.clone();
        self.merge_units(&libs, true);
    }

    /// Like merge_libraries but re-merges only the library FUNCTION tables, NOT their
    /// data — for load_current, where the process's arena (including the shared
    /// libc's MUTATED data as of its suspend) is restored from the snapshot, so
    /// reloading pristine libc data would clobber it. build_tables reset the func
    /// table to the resumed program alone, so libc's functions must be re-merged.
    pub fn merge_library_funcs(&mut self) {
        if self.library_units.is_empty() {
            return;
        }
        let libs = self.library_units.clone();
        self.merge_units(&libs, false);
    }

    /// Extends the live dispatch tables with each listed program's functions + block
    /// maps (and, when with_data, loads its data sections), then re-sorts/dedups the
    /// tables (`func_at`/`block_address` binary-search sorted, deduped keys). `regs`
    /// are `&'static`, so holding `prog` across the `self.*` writes does not borrow
    /// `self`.
    fn merge_units(&mut self, indices: &[usize], with_data: bool) {
        for &i in indices {
            let prog = self.programs.get(i);
            let (funcs, bb_maps) = build_tables(prog);
            self.funcs.extend(funcs);
            self.bb_maps.extend(bb_maps);
            if with_data {
                self.arena.load_data_sections(prog);
                // Several programs' data now coexist; no single index describes
                // the arena, so force the next bounded restore to reload.
                self.materialized_prog = usize::MAX;
            }
        }
        self.funcs.sort_by_key(|&(vma, _)| vma);
        self.funcs.dedup_by_key(|e| e.0);
        self.bb_maps.sort_by_key(|&(vma, _)| vma);
        self.bb_maps.dedup_by_key(|e| e.0);
    }

    // --- process model / scheduler (M4) ---

    /// The current process's pid.
    pub fn current_pid(&self) -> u32 {
        self.procs[self.current].pid
    }

    fn alloc_pid(&mut self) -> u32 {
        let p = self.next_pid;
        self.next_pid += 1;
        p
    }

    fn find_pid(&self, pid: u32) -> Option<usize> {
        self.procs
            .iter()
            .position(|p| p.pid == pid && p.status != ProcStatus::Dead)
    }

    /// True if the current process has any not-yet-reaped child.
    ///
    /// A child is a thread-group LEADER (`pid == tgid`). A sibling thread of a
    /// child process carries the same `ppid` as its leader -- that is what makes
    /// `getppid` agree across a group -- so without the leader test a threaded
    /// child would be counted once per thread, and a `wait4` would keep finding
    /// "children" that can never be reaped.
    pub fn has_children(&self) -> bool {
        let me = self.current_tgid();
        self.procs
            .iter()
            .any(|p| p.ppid == me && p.pid == p.tgid && p.status != ProcStatus::Dead)
    }

    /// Reaps a zombie child of the current process matching `target` (0 or
    /// u32::MAX = any), returning (pid, how it ended).
    ///
    /// The reason, not a status word: encoding it is `sys_wait4`'s job (through
    /// [`ExitReason::wait_status`]), and this is also what the SIGCHLD path and
    /// any future `waitid` would need.
    pub fn reap_zombie(&mut self, target: u32) -> Option<(u32, ExitReason)> {
        let me = self.current_tgid();
        for p in &mut self.procs {
            // Leaders only; see `has_children`. A non-leader is never a Zombie
            // either, but stating it here keeps the two rules in one shape.
            if p.ppid == me && p.pid == p.tgid {
                if let ProcStatus::Zombie(reason) = p.status {
                    if target == 0 || target == u32::MAX || p.pid == target {
                        p.status = ProcStatus::Dead;
                        return Some((p.pid, reason));
                    }
                }
            }
        }
        None
    }

    /// Recomputes every `open_files` slot's reference count from the descriptors
    /// that actually name it, and returns the slots where the recorded count
    /// disagrees as `(idx, recorded, actual)`.
    ///
    /// The three things that hold a reference are the three that must be counted:
    /// this process's fd table, every OTHER process's saved fd table, and any
    /// SCM_RIGHTS batch still queued on a pipe direction. Miss one of those and
    /// the audit reports a phantom leak; that is why they are listed here rather
    /// than left to the reader.
    ///
    /// Too LOW is the dangerous direction and the one this exists for -- a slot
    /// whose count understates its users gets recycled under a live descriptor,
    /// which is silent and was worth a day. Too high is only a leak.
    #[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
    pub fn audit_mem_refs(&self) -> Vec<(usize, u32, u32)> {
        let recorded: Vec<u32> = self.open_files.iter().map(|f| f.refs).collect();
        mem_ref_mismatches(
            &recorded,
            &self.count_holders(self.open_files.len(), OpenFile::mem_file),
        )
    }

    /// The same audit for `file_offsets`. Separate entry point rather than a
    /// second return value because the two tables are recycled independently and
    /// a caller reporting a mismatch has to name which one.
    #[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
    pub fn audit_offset_refs(&self) -> Vec<(usize, u32, u32)> {
        let recorded: Vec<u32> = self.file_offsets.iter().map(|f| f.refs).collect();
        mem_ref_mismatches(
            &recorded,
            &self.count_holders(self.file_offsets.len(), OpenFile::mem_offset),
        )
    }

    /// Counts, for each slot of a context-global table, the descriptors that
    /// name it -- `which` says which index an entry contributes.
    ///
    /// The three holders it walks are the whole of the rule, and they are here
    /// once rather than per-table so that a fourth kind of holder cannot be
    /// added to one audit and forgotten by the other.
    fn count_holders(&self, len: usize, which: fn(&OpenFile) -> Option<usize>) -> Vec<u32> {
        let tables = std::iter::once(self.fds.as_slice()).chain(
            self.procs
                .iter()
                .enumerate()
                // Its live table is self.fds; the saved copy is stale.
                .filter(|(i, _)| *i != self.current)
                .filter_map(|(_, p)| p.fds.as_deref()),
        );
        let queued = self
            .pipes
            .iter()
            .flat_map(|p| p.scm.iter())
            .flat_map(|batch| batch.iter());
        count_named_slots(tables, queued, len, which)
    }

    /// Takes a reference on everything an fd entry names. Call this for EVERY
    /// clone of an entry, before the copy becomes reachable.
    ///
    /// It exists because the rule was written out by hand at each clone site and
    /// one of them got it wrong: `dup_entry` bumped a pipe end's count and not a
    /// mem file's, so `dup` left two descriptors and one reference on a shared
    /// `open_files` slot. Closing either freed the slot while the survivor still
    /// pointed at it, the next `open` recycled it, and from then on that
    /// descriptor read and wrote a different file with no error anywhere. The
    /// three clone sites -- `fork_current` below, `dup_entry`, and the SCM_RIGHTS
    /// send queue -- now share this one statement of the rule.
    ///
    /// Sockets are deliberately absent. They carry no counter; `close_fd_full`
    /// refcounts them by SCANNING the fd tables of every live process, so a
    /// clone that lands in an fd table is already counted and one that does not
    /// (a queued SCM right) is invisible to that scan either way. That asymmetry
    /// is pre-existing and is not made better or worse here.
    pub fn retain_entry(&mut self, entry: &OpenFile) {
        if let Some(idx) = entry.mem_file() {
            self.open_files[idx].refs += 1;
        }
        // The offset slot, which a clone shares rather than copies. Its count is
        // NOT the file's: two `open`s of one path join one `MemFile` and take two
        // offsets, so the two tables diverge as soon as a guest opens anything
        // twice. Bumped here, next to the file, because both are the same rule --
        // "every clone of an entry takes a reference on everything it names" --
        // and splitting them across two call sites is how the `dup` bug happened.
        if let Some(idx) = entry.mem_offset() {
            self.file_offsets[idx].refs += 1;
        }
        for (idx, write) in entry.pipe_ends() {
            if write {
                self.pipes[idx].writers += 1;
            } else {
                self.pipes[idx].readers += 1;
            }
        }
    }

    /// fork: snapshots the current process into a new child (child's fork return
    /// x0 = 0, parent's = child pid). The PARENT does NOT unwind — it returns from
    /// `clone` on its intact native stack. The child has NO native stack; it is
    /// enqueued as a replay-start process (its `Replay` seeds the driver in
    /// entry.rs) and its stack is rebuilt by re-entry when it is first scheduled,
    /// NOT by asyncify (which cannot rewind the lifted glibc fork frames — see
    /// .agents/docs/DYNLINK.md). This is why fork no longer sets `pending`/unwinds.
    pub fn fork_current(&mut self) -> u32 {
        let child_pid = self.alloc_pid();
        // The parent of a fork child is the forking task's thread GROUP, not the
        // thread: Linux reports the tgid as `getppid`, and `wait4` on the other
        // side matches against it.
        let ppid = self.current_tgid();

        // Innermost replay frame: the function executing the `clone` SVC, resumed
        // at its post-syscall pc with x0=0 (SyscallBrowser.cpp: t_func_addr =
        // fork_entry_fun_addr, t_next_pc = pc). The outer frames are the parent's
        // call history at this instant.
        let mut cstate = State::new_boxed();
        let innermost = unsafe {
            core::ptr::copy_nonoverlapping(self.live_state, &mut *cstate, 1);
            cstate.set_ret(0);
            (*self.live_state).set_ret(child_pid as u64);
            (
                (*self.live_state).fork_entry_fun_addr,
                (*self.live_state).pc(),
            )
        };

        // The child inherits copies of the parent's fds; each pipe end gains a
        // reference (the pipe stays open until every process closes it).
        // ...and it inherits the parent's shared mappings, which likewise stay
        // alive until the LAST of the two drops them. Without this the parent's
        // exit would reclaim a region its child is still reading -- and the
        // child holding one alone would never keep it, since the reclaim test
        // is "does any live process map this".
        self.shm_inherit(self.current_pid(), child_pid);
        let child_fds = self.fds.clone();
        for entry in child_fds.iter().flatten() {
            self.retain_entry(entry);
        }
        let child = Process {
            // fork COPIES the address space, so whatever the parent had loaded
            // is present in the child's arena from the first instruction. An
            // empty set here would make the child re-run a plugin's
            // constructors over data that is already live.
            units: self.procs[self.current].units.clone(),
            // NOT inherited. `dlerror` reports the last dl-call THIS task made,
            // and the child has made none -- a fork is not a failed dlopen.
            dlerror: None,
            pid: child_pid,
            ppid,
            // fork always starts a NEW thread group, even when the forking task
            // is itself a thread: the child is single-threaded by definition.
            tgid: child_pid,
            status: ProcStatus::Runnable,
            // Entered via the replay driver (re-execute the clone SVC with
            // resuming=true; sys_clone's resume branch returns with x0=0 preset).
            started: false,
            prog_idx: self.procs[self.current].prog_idx,
            blocked_on: BlockedOn::None,
            arena: Some(self.snapshot_for(self.current)),
            state: Some(cstate),
            fds: Some(child_fds),
            cloexec: Some(self.cloexec.clone()), // fork inherits close-on-exec flags
            nonblock: Some(self.nonblock.clone()), // ... and the fd status flags
            cwd: Some(self.cwd.clone()),
            // The child inherits the parent's signal dispositions, exactly like
            // the fd table (POSIX fork semantics)...
            signals: Some(self.signals.clone()),
            // ...and its MASK, but not its pending signals: Linux gives a fork
            // child an empty pending set, and inheriting one would deliver the
            // parent's signal twice.
            task_signals: TaskSignals {
                blocked: self.task_signals.blocked,
                pending: 0,
                sigsuspend_saved: None,
            },
            // Seed the child's live stack with the parent's; each reconstructed
            // frame's epilogue pops it in lockstep with `replay.remaining`.
            call_history: Some(self.call_history.clone()),
            replay: Some(Replay {
                cur: innermost,
                remaining: self.call_history.clone(),
                resuming: true, // re-execute the clone SVC on first entry
            }),
            // A fork child starts runnable; any timed wait the parent was in
            // belongs to the parent alone.
            deadline: None,
            timed_out: false,
            // Not inherited: `clone(CLONE_CHILD_CLEARTID)` sets it per task and
            // plain fork does not pass it.
            clear_child_tid: 0,
            // Inherited: `mmf_init_flags` carries MMF_DUMPABLE into the new mm.
            // Measured on Linux 6.17 -- a child of a process that set 0 reads 0.
            dumpable: self.procs[self.current].dumpable,
            // Inherited for the same reason: MMF_DISABLE_THP is in MMF_INIT_MASK
            // too. Measured -- a child of a process that set 1 reads 1, and the
            // child's own later SET does NOT reach the parent (a fork gets a new
            // mm, unlike a CLONE_THREAD sibling).
            thp_disable: self.procs[self.current].thp_disable,
        };
        if crate::diag::trace_log() {
            let h = &self.call_history;
            let tail: Vec<String> = h
                .iter()
                .rev()
                .take(8)
                .map(|(f, r)| format!("{{fn=0x{f:x} ret=0x{r:x}}}"))
                .collect();
            ecv_trace!(
                clone,
                "fork child_pid={} innermost=(fn=0x{:x} pc=0x{:x}) call_history depth={} top8(caller-first)=[{}]",
                child_pid,
                innermost.0,
                innermost.1,
                h.len(),
                tail.join(" ")
            );
        }
        ecv_debug!(
            mem,
            "fork -> {} procs, linear memory {} MiB",
            self.procs.len() + 1,
            crate::diag::linear_memory_mib()
        );
        let child_idx = self.procs.len();
        self.procs.push(child);
        // The parent keeps running (no unwind); enqueue the child so the scheduler
        // reaches it at the parent's next suspend. No `pending`/copy_buf: the child
        // reconstructs by replay.
        self.run_queue.push_back(child_idx);
        child_pid
    }

    /// `clone(CLONE_THREAD|CLONE_VM|...)`: a new task in the CURRENT thread
    /// group, sharing this one's arena, fd table, cwd and signal state.
    ///
    /// The cooperative scheduler is what makes this tractable: it switches only
    /// where a task blocks, yields or exits, so threads sharing one arena never
    /// interleave at an arbitrary instruction and the data races that make real
    /// threads hard cannot arise. Sharing is therefore literal — there is one
    /// arena and one fd table for the group, MOVED between members on a switch
    /// (`tables_holder` / `arena_holder`), not copied.
    ///
    /// It differs from `fork_current` in exactly three ways, and each is forced:
    ///
    /// - `remaining` is EMPTY. A thread does not return into its creator's
    ///   frames; it runs `__clone`'s post-SVC tail, calls the start routine, and
    ///   exits from there. Seeding it with the creator's call history would have
    ///   the thread unwind into frames that belong to another stack.
    /// - `sp` comes from the caller's `child_stack`, not the creator's stack.
    ///   musl's `__clone` has already pushed {func, arg} onto it before the SVC,
    ///   so the value passed in x1 is the post-push sp the child must resume on.
    /// - `tpidr_el0` comes from `CLONE_SETTLS`. The thread pointer lives in the
    ///   per-task `State`, so each thread reads its own `__thread` storage with
    ///   no further machinery.
    ///
    /// No arena snapshot is taken and no fd/cwd/signal state is cloned: the new
    /// task's entries stay None, and the group's single live copy reaches it
    /// through the holder lookups.
    pub fn clone_thread(&mut self, child_stack: u64, tls: u64, ctid: u64) -> u32 {
        let tid = self.alloc_pid();
        let cur = self.current;
        let tgid = self.procs[cur].tgid;
        // Shared regions are tracked per pid, and a thread shares the group's
        // address space by definition -- so it maps everything the creator maps.
        // Without this the creator's exit would reclaim a segment its own
        // threads are still reading.
        self.shm_inherit(self.procs[cur].pid, tid);
        // A thread's parent is the group's parent, not the creating thread:
        // only a group leader is ever waitable, and `getppid` must agree across
        // the group.
        let ppid = match self.find_pid(tgid) {
            Some(l) => self.procs[l].ppid,
            None => self.procs[cur].ppid,
        };
        let prog_idx = self.procs[cur].prog_idx;

        let mut cstate = State::new_boxed();
        let innermost = unsafe {
            core::ptr::copy_nonoverlapping(self.live_state, &mut *cstate, 1);
            cstate.set_ret(0); // the child sees clone() == 0
            cstate.gpr.sp.val = child_stack;
            cstate.sr.tpidr_el0 = tls;
            (*self.live_state).set_ret(tid as u64); // the creator sees the tid
            (
                (*self.live_state).fork_entry_fun_addr,
                (*self.live_state).pc(),
            )
        };

        let thread = Process {
            // A fresh thread has made no dl-call, so it has no last error --
            // and unlike `units`, this one really IS per-task: glibc's `dlerror`
            // is per-thread.
            dlerror: None,
            // Never read: a thread shares its group's address space, so
            // `units_owner` resolves every lookup to the group LEADER's entry.
            // Kept empty rather than cloned so there is only ever one copy to
            // be right.
            units: Vec::new(),
            pid: tid,
            ppid,
            tgid,
            status: ProcStatus::Runnable,
            started: false,
            prog_idx,
            blocked_on: BlockedOn::None,
            // Shared with the group, held by whichever member last ran.
            arena: None,
            state: Some(cstate),
            fds: None,
            cloexec: None,
            nonblock: None,
            cwd: None,
            signals: None,
            // A new thread INHERITS the creator's mask -- glibc depends on it:
            // `pthread_create` blocks everything, clones, and each side then
            // restores its own. Its pending queue starts empty, and being its
            // own queue is what keeps the creator's block-all from following it
            // back out into the group.
            task_signals: TaskSignals {
                blocked: self.task_signals.blocked,
                pending: 0,
                sigsuspend_saved: None,
            },
            // Its own stack starts empty; `remaining` is empty for the same
            // reason.
            call_history: Some(Vec::new()),
            replay: Some(Replay {
                cur: innermost,
                remaining: Vec::new(),
                resuming: true, // re-execute the clone SVC on first entry
            }),
            deadline: None,
            timed_out: false,
            clear_child_tid: ctid,
            // Shared with the group in Linux (it lives in the mm); this is the
            // creator's copy, and `set_group_dumpable` keeps every copy equal.
            dumpable: self.procs[cur].dumpable,
            // Shared with the group for the same reason (MMF_DISABLE_THP is an
            // mm flag); `set_group_thp_disable` keeps every copy equal.
            thp_disable: self.procs[cur].thp_disable,
        };
        ecv_trace!(
            clone,
            "THREAD tgid={} creator={} -> tid={} sp={:#x} tp={:#x} ctid={:#x} entry=(fn={:#x} pc={:#x})",
            tgid,
            self.procs[cur].pid,
            tid,
            child_stack,
            tls,
            ctid,
            innermost.0,
            innermost.1
        );
        let idx = self.procs.len();
        self.procs.push(thread);
        self.run_queue.push_back(idx);
        tid
    }

    /// The member of `idx`'s thread group whose entry currently holds the
    /// group's arena snapshot.
    ///
    /// `save_current` files the group's shared state under whichever member
    /// happened to be running, so a switch INTO a group has to find it by
    /// group. For a single-threaded process this is always `idx` itself and the
    /// scan never runs. Zombie and dead members are eligible holders: a thread
    /// that exits while its siblings live must not take the group's memory with
    /// it, and leaving the buffer filed where it lies is cheaper than moving it.
    fn arena_holder(&self, idx: usize) -> usize {
        group_member_where(&self.procs, idx, |p| p.arena.is_some())
    }

    /// The member of `idx`'s thread group holding the group's fd table, cwd and
    /// signal state. See `arena_holder`; `fds` is the marker because it is the
    /// one shared table that is never legitimately absent from a live group.
    fn tables_holder(&self, idx: usize) -> usize {
        group_member_where(&self.procs, idx, |p| p.fds.is_some())
    }

    /// True if no member of `tgid` is still alive (every one is a zombie or has
    /// been reaped). The group's arena may be released only then.
    fn group_is_dead(&self, tgid: u32) -> bool {
        group_is_dead(&self.procs, tgid)
    }

    /// The current task's thread-group id -- what `getpid` must report.
    pub fn current_tgid(&self) -> u32 {
        self.procs[self.current].tgid
    }

    /// `set_tid_address(ptr)`: the current task's clear-on-exit word.
    pub fn set_clear_child_tid(&mut self, addr: u64) {
        self.procs[self.current].clear_child_tid = addr;
    }

    /// `prctl(PR_SET_DUMPABLE, value)`: records `value` for the whole thread
    /// group. The caller has already checked it with `dumpable_arg_permitted`.
    pub fn set_dumpable(&mut self, value: u64) {
        set_group_dumpable(&mut self.procs, self.current, value);
    }

    /// `prctl(PR_GET_DUMPABLE)`: what the last accepted `PR_SET_DUMPABLE`
    /// stored, or `SUID_DUMP_USER` if there was none.
    pub fn dumpable(&self) -> u64 {
        self.procs[self.current].dumpable
    }

    /// `prctl(PR_SET_THP_DISABLE, value)`: records `value` for the whole thread
    /// group. The caller has already reduced it with [`thp_disable_value`].
    pub fn set_thp_disable(&mut self, value: u64) {
        set_group_thp_disable(&mut self.procs, self.current, value);
    }

    /// `prctl(PR_GET_THP_DISABLE)`: what the last accepted `PR_SET_THP_DISABLE`
    /// stored, or [`THP_NOT_DISABLED`] if there was none.
    pub fn thp_disable(&self) -> u64 {
        self.procs[self.current].thp_disable
    }

    /// True if the current task shares its thread group with another live task.
    pub fn current_has_siblings(&self) -> bool {
        self.is_multithreaded(self.procs[self.current].tgid)
    }

    /// True if `tgid` has more than one task that has not been reaped. False for
    /// every process that never called `clone(CLONE_THREAD)`, which is the whole
    /// existing corpus, so every path guarded by it keeps its old behavior.
    fn is_multithreaded(&self, tgid: u32) -> bool {
        group_is_multithreaded(&self.procs, tgid)
    }

    /// Records the replay state for the current process at a blocking syscall, so
    /// the scheduler can rebuild its stack when it is next run. Called by the svc
    /// trampoline right before it yields (EH-unwinds) to the scheduler. The
    /// innermost frame is the function executing the SVC (resumed at its post-SVC
    /// pc; the scheduler completes the syscall via a direct svc call before
    /// re-entering it, since re-entry does not re-execute the SVC). `remaining` is
    /// ALWAYS rebuilt from the current call_history — the hooks keep it exact even
    /// mid-reconstruction (a re-entered outer frame that blocks again has its frame
    /// back on top), so a stale copy would drop frames. Not called for execve
    /// (fresh re-entry) — distinguished by `started == false` in the trampoline.
    pub fn capture_suspend_replay(&mut self) {
        let cur = self.current;
        let innermost = unsafe {
            (
                (*self.live_state).fork_entry_fun_addr,
                (*self.live_state).pc(),
            )
        };
        let remaining = self.call_history.clone();
        self.procs[cur].replay = Some(Replay {
            cur: innermost,
            remaining,
            resuming: true,
        });
    }

    /// Called by the svc trampoline when a syscall suspended the current process,
    /// just before it EH-unwinds. Blocking syscalls and exit reconstruct on resume
    /// (started stays true); execve reset `started=false` and re-enters fresh, so
    /// it must NOT capture replay state.
    pub fn on_suspend(&mut self) {
        if self.procs[self.current].started {
            self.capture_suspend_replay();
        }
    }

    /// Snapshots the current process's live data into its table entry.
    fn save_current(&mut self) {
        let cur = self.current;
        // Bounded-snapshot sizing probe. Reported here rather than in
        // `load_current` because this is the one place the DEPARTING process's
        // stack pointer is still live -- after this its state has been boxed
        // away. See Arena::bounded_snapshot_bytes and the design note in
        // .agents/docs/TODO.md; this measurement is what decides whether that
        // work is worth doing.
        if crate::diag::snapstat() {
            let sp = unsafe { (*self.live_state).gpr.sp.val };
            let prog = self.programs.get(self.procs[cur].prog_idx);
            let loads = prog.writable_loads();
            let tls = prog.tls_extent_above_tp();
            let (tot, img, brk, mm, stk) = self.arena.bounded_snapshot_bytes(&loads, sp, tls);
            const MB: u64 = 1024 * 1024;
            ecv_probe!(
                snapstat,
                "bounded={}MiB of {}MiB (image_w={} brk={} mmap={} stack={} KiB)",
                tot / MB,
                crate::arena::MEMORY_ARENA_SIZE as u64 / MB,
                img / 1024,
                brk / 1024,
                mm / 1024,
                stk / 1024
            );
        }
        // No arena snapshot here. The live buffer IS this process's memory, and
        // `load_current` hands it over only when a DIFFERENT process is swapped
        // in. The invariant: the live owner's `arena` is None, and every other
        // process holds its own full-size buffer.
        let mut st = State::new_boxed();
        unsafe { core::ptr::copy_nonoverlapping(self.live_state, &mut *st, 1) };
        self.procs[cur].state = Some(st);
        self.procs[cur].fds = Some(core::mem::take(&mut self.fds));
        self.procs[cur].cloexec = Some(core::mem::take(&mut self.cloexec));
        self.procs[cur].nonblock = Some(core::mem::take(&mut self.nonblock));
        self.procs[cur].cwd = Some(core::mem::take(&mut self.cwd));
        self.procs[cur].signals = Some(core::mem::take(&mut self.signals));
        // By `cur`, deliberately: the mask and the thread-directed queue belong
        // to this task alone, so they must NOT travel with the group's tables.
        self.procs[cur].task_signals = self.task_signals;
        self.procs[cur].call_history = Some(core::mem::take(&mut self.call_history));
    }

    /// Materialises `prog_idx`'s image into the live arena AND records that it
    /// is there.
    ///
    /// Every image load must go through this. `materialized_prog` is a cache of
    /// "whose image is in the arena", and a cache updated at some of its write
    /// sites is worse than no cache: a stale entry makes a bounded restore SKIP
    /// the reload it needed. That is what shipped in the first version -- the
    /// tracker was set in `load_current` only, so an `execve` swapped the image
    /// underneath it and the next switch back to the old program ran with the
    /// new program's text.
    fn materialize_image(&mut self, prog_idx: usize) {
        let prog = self.programs.get(prog_idx);
        self.arena.load_data_sections(prog);
        self.materialized_prog = prog_idx;
    }

    /// Marks the arena's image contents as UNKNOWN, forcing the next bounded
    /// restore to reload. Used where several programs' data are merged into one
    /// arena and no single index describes it.
    fn invalidate_materialized_image(&mut self) {
        self.materialized_prog = usize::MAX;
    }

    /// A snapshot of the LIVE arena as process `idx` sees it: bounded, except
    /// for a multi-threaded group.
    ///
    /// The bounded ranges are computed from `idx`'s own saved state -- its stack
    /// pointer, its program's writable segments and TLS extent -- and from the
    /// live arena's `brk_cur`/`mmap_live`, which belong to `idx` because it is
    /// the process whose memory is currently live.
    fn snapshot_for(&self, idx: usize) -> ArenaSnapshot {
        let prog_idx = self.procs[idx].prog_idx;
        // THE ONE REASON a full snapshot is still taken: a bounded range set is
        // derived from ONE stack pointer, which is exactly right for a process
        // and wrong for a thread group, because every sibling's stack is live
        // memory that `idx`'s `sp` says nothing about. musl and glibc both mmap
        // thread stacks, so they would usually fall inside the mmap range and be
        // captured by luck -- and "usually" is not a basis for deciding which
        // bytes of a shared address space to keep.
        //
        // This is a property of the process, not a mode: there is no longer any
        // way to ask for a full snapshot of a single-threaded process, and the
        // `Full` variant exists for this branch alone.
        if self.is_multithreaded(self.procs[idx].tgid) {
            return self.arena.snapshot(prog_idx);
        }
        let prog = self.programs.get(prog_idx);
        // `state` is None for the process that is currently executing (its
        // registers are in `live_state`), and Some for one that has been saved.
        let sp = match self.procs[idx].state.as_ref() {
            Some(st) => st.gpr.sp.val,
            None => unsafe { (*self.live_state).gpr.sp.val },
        };
        let ranges = crate::arena::bounded_ranges(
            self.arena.brk_cur(),
            self.arena.mmap_live(),
            &prog.writable_loads(),
            sp,
            prog.tls_extent_above_tp(),
        );
        self.arena.snapshot_bounded(&ranges, prog_idx)
    }

    /// Loads a process's snapshot into the live context and makes it current,
    /// rebuilding the dispatch tables for its program (processes may run
    /// different programs after execve).
    fn load_current(&mut self, idx: usize) {
        self.current = idx;
        // Publish the pid diagnostics are attributed to. This is the ONLY
        // assignment to `self.current` in the crate, which is what makes one
        // call here sufficient; a second assignment site would need one too.
        crate::trace::set_current_pid(self.procs[idx].pid);
        // Threads of one group share ONE arena, so a switch between them must
        // not snapshot or restore anything: the live buffer already holds the
        // incoming thread's memory, because it is the same memory. Doing the
        // swap anyway would be worse than slow -- it would hand each thread a
        // private copy and silently un-share the address space.
        let same_group = self.procs[idx].tgid == self.procs[self.live_owner].tgid;
        // A switch trades buffers rather than contents. Skipped entirely when
        // `idx` is the process that last ran, which is the common case: a
        // process blocks, the scheduler finds nothing else runnable, sleeps in
        // the host poll, and re-loads the same process.
        if idx != self.live_owner && !same_group {
            let incoming = self.arena_holder(idx);
            let t0 = crate::diag::trace_log().then(mono_nanos);
            // `outgoing` comes back holding the DEPARTING process's memory,
            // which is then filed under `live_owner` -- the one process whose
            // `arena` was None.
            // Bounded-snapshot SAFETY probe, before the swap: the live arena
            // still holds the DEPARTING process's memory and `procs[idx].arena`
            // holds the incoming one's, so this is the only moment both exist
            // and can be compared. See Arena::bytes_differing_outside.
            let prev_idx = self.live_owner;
            // Refcount invariant, checked at the same moment as the snapshot one
            // and for the same reason: a switch is when every process's fd table
            // is materialised and comparable. O(fds), not O(arena).
            if crate::diag::fdcheck() {
                for (i, recorded, actual) in self.audit_mem_refs() {
                    ecv_probe!(
                        fdcheck,
                        "idx={i} refs={recorded} actual={actual} {} path={}",
                        if recorded < actual {
                            "TOO LOW -- slot can be recycled under a live fd"
                        } else {
                            "too high -- leak"
                        },
                        String::from_utf8_lossy(&self.open_files[i].path)
                    );
                }
            }
            if crate::diag::snapcheck() {
                let inc = self.procs[incoming].arena.as_ref().unwrap();
                let sp = self.procs[idx]
                    .state
                    .as_ref()
                    .map(|st| st.gpr.sp.val)
                    .unwrap_or(0);
                let prog = self.programs.get(self.procs[idx].prog_idx);
                let loads = prog.writable_loads();
                let tls = prog.tls_extent_above_tp();
                let ranges = inc.bounded_ranges(&loads, sp, tls);
                // ⚠️ `None` means the probe had NO ORACLE and compared nothing,
                // which is now the common case: the comparison needs the
                // incoming process's whole memory, and only a `Full` snapshot
                // carries it. Since bounded snapshots became unconditional the
                // only `Full` snapshots are multi-threaded groups, so every
                // ordinary switch lands here. Printing `miss=0` for it would be
                // a fabricated pass -- zero differences found against a buffer
                // that does not exist -- so it says so instead.
                match self.arena.bytes_differing_outside(inc, &ranges) {
                    None => ecv_probe!(
                        snapcheck,
                        "prog {}->{} tls={} NO-ORACLE nothing compared \
                         (incoming snapshot is bounded, so the full buffer this \
                         probe diffs against does not exist)",
                        self.procs[prev_idx].prog_idx,
                        self.procs[idx].prog_idx,
                        tls
                    ),
                    Some(d) => {
                        let [below, img, brk, mm, stk] = d.counts;
                        let at = |i: usize| {
                            if d.first[i] == u64::MAX {
                                String::from("-")
                            } else {
                                format!("{:#x}", d.first[i])
                            }
                        };
                        // The two program indices are the point: the read-only
                        // image is identical only between processes running the
                        // SAME program, and this module holds four. The
                        // ADDRESSES are the other point: a count says a region
                        // is wrong, an address says which structure. pid is the
                        // subscriber's -- `idx` is already `self.current`.
                        //
                        // `hypothetical` because the switch this line describes
                        // did NOT use the range set it was scored against: an
                        // oracle exists only for a multi-threaded group, and
                        // such a group is restored in full. The number answers
                        // "would a bounded snapshot have been safe here", which
                        // is worth knowing and is not the same claim.
                        ecv_probe!(snapcheck,
                            "prog {}->{} tls={} hypothetical miss={} (below_image={}@{} image={}@{} brk={}@{} mmap={}@{} stack={}@{})",
                            self.procs[prev_idx].prog_idx,
                            self.procs[idx].prog_idx,
                            tls,
                            d.total(),
                            below,
                            at(0),
                            img,
                            at(1),
                            brk,
                            at(2),
                            mm,
                            at(3),
                            stk,
                            at(4)
                        );
                    }
                }
            }
            let prev = self.live_owner;
            // The live buffer never moves. Save only what the departing process
            // can have written, then materialise the incoming one on top of it.
            //
            // There is no longer an alternative branch here. The full-buffer
            // scheme -- swap the buffers, then `adopt_shared_from` -- was
            // selectable by environment until 2026-08-22 and is not any more:
            // it dies with `memory allocation of 402653184 bytes failed` the
            // moment a fourth process exists, so keeping it reachable only kept
            // a path that cannot run the workloads this runtime is for.
            //
            // Order matters and is not interchangeable: the departing process's
            // bytes must be captured BEFORE anything overwrites them, and a
            // cross-program restore must reload the image BEFORE the incoming
            // process's own ranges go back, or the pristine image would clobber
            // the writes it just restored.
            //
            // `inc` is not necessarily BOUNDED: `snapshot_for` returns a full
            // snapshot for a multi-threaded group, because a range set derived
            // from one stack pointer is wrong for a group. `restore_in_place`
            // handles both -- and the `materialize_image` below is then merely
            // redundant, since a full restore carries its own image.
            let outgoing = {
                let saved = self.snapshot_for(prev);
                let inc = self.procs[incoming].arena.take().unwrap();
                if inc.prog_idx != self.materialized_prog {
                    self.materialize_image(inc.prog_idx);
                }
                self.arena.restore_in_place(&inc, &self.shared_segments);
                saved
            };
            // Do not hand a whole arena back to a corpse. A zombie exists only to
            // carry an exit status until its parent reaps it, and on Linux its
            // memory is gone the moment it exits -- but the swap files the
            // outgoing buffer under `live_owner`, which after an exit IS the
            // zombie, so every dead process kept a full arena forever.
            //
            // Measured on initdb, which forks repeatedly to probe
            // max_connections and shared_buffers: linear memory grew by exactly
            // 384 MiB per fork and never came back --
            //   fork -> 5 procs, 2257 MiB / exit pid=5 -> 2271 MiB
            //   fork -> 9 procs, 3793 MiB / ... allocation of 402653184 failed
            // Dropping it here returns the block to the allocator for the next
            // fork's snapshot to reuse.
            //
            // A THREAD group qualifies only once every member is gone: the
            // buffer is the whole group's address space, so releasing it while
            // a sibling still runs would delete memory that is still in use.
            if matches!(self.procs[prev].status, ProcStatus::Zombie(_))
                && self.group_is_dead(self.procs[prev].tgid)
            {
                drop(outgoing);
            } else {
                self.procs[prev].arena = Some(outgoing);
            }
            if let Some(t0) = t0 {
                ecv_trace!(
                    sched,
                    "switch from_pid={} {}us",
                    self.procs[prev].pid,
                    (mono_nanos() - t0) / 1000
                );
            }
        }
        self.live_owner = idx;
        let st = self.procs[idx].state.take().unwrap();
        unsafe { core::ptr::copy_nonoverlapping(&*st, self.live_state, 1) };
        // The fd table, cwd and signal dispositions belong to the thread GROUP,
        // so they are taken from whichever member last filed them -- which is
        // `idx` itself for every single-threaded process, and a sibling when a
        // group switches internally. Moving the one table rather than copying
        // it is what makes the sharing real: a descriptor one thread opens is
        // the same descriptor to the next thread scheduled.
        let tbl = self.tables_holder(idx);
        self.fds = self.procs[tbl].fds.take().unwrap();
        self.cloexec = self.procs[tbl].cloexec.take().unwrap_or_default();
        self.nonblock = self.procs[tbl].nonblock.take().unwrap_or_default();
        self.cwd = self.procs[tbl].cwd.take().unwrap();
        self.signals = self.procs[tbl].signals.take().unwrap();
        // ...but this one comes from `idx`, never from `tbl`.
        self.task_signals = self.procs[idx].task_signals;
        self.call_history = self.procs[idx].call_history.take().unwrap_or_default();
        // The dispatch tables are a pure function of the PROGRAM, and a process's
        // program changes only at execve — so rebuilding them per context switch
        // is pure waste. `build_tables` walks every lifted function and every
        // basic block of every function, allocating and sorting a Vec per
        // function: on nginx (24,708 functions) that is 164 ms, and it was the
        // real cost of a switch. The arena restore above, the obvious suspect at
        // 384 MiB, turned out to be ~15 ms of it — measured by skipping each in
        // turn, in that order, and being wrong the first time.
        let want = self.procs[idx].prog_idx;
        if self.tables_prog_idx != Some(want) {
            let t0 = crate::diag::tables().then(mono_nanos);
            let cached = self.swap_in_tables(want);
            if !cached {
                let (funcs, bb_maps) = build_tables(self.programs.get(want));
                self.funcs = funcs;
                self.bb_maps = bb_maps;
                // Re-merge the shared library FUNCTION tables (their data is
                // already in the restored arena snapshot); build_tables above
                // reset the table to this program alone. Not repeated on the
                // cached path: a parked table was merged before it was parked,
                // and `merge_units` sorts and dedups by VMA, so re-merging would
                // be work with no effect.
                self.merge_library_funcs();
            }
            self.tables_prog_idx = Some(want);
            if let Some(t0) = t0 {
                self.report_table_build(idx, t0, cached);
            }
        }
    }

    /// Parks the live dispatch tables under the program they belong to and
    /// installs `want`'s if they have been built before. Returns whether they
    /// had.
    ///
    /// Both halves are `mem::take`, so this moves two `Vec` headers and copies
    /// no table content -- which is the entire point, against a rebuild that
    /// walks every function and every basic block and sorts both.
    ///
    /// **The live tables are parked only when `tables_prog_idx` is `Some`.**
    /// `None` does not mean "unknown", it means they are a MERGE of several
    /// programs and belong to no single index: `EcvContext::new` leaves it unset
    /// while `entry.rs` merges the library units, and `merge_shared_units` folds
    /// every non-entry program in under `RAPTORMARK_SHARED_UNITS`. Filing that
    /// merge under one program would hand a later switch a table containing
    /// other programs' VMAs.
    fn swap_in_tables(&mut self, want: usize) -> bool {
        let mut live = (
            core::mem::take(&mut self.funcs),
            core::mem::take(&mut self.bb_maps),
        );
        let hit = swap_tables(
            &mut self.tables_cache,
            self.tables_prog_idx,
            want,
            &mut live,
        );
        self.funcs = live.0;
        self.bb_maps = live.1;
        hit
    }

    /// Reports what a dispatch-table rebuild cost, in time AND in bytes
    /// (RAPTORMARK_ECV_TABLES). Split out of `load_current` so the measurement
    /// costs that function nothing but a `then(mono_nanos)` when the gate is off.
    ///
    /// It reports SIZE because the obvious fix for the rebuild -- caching the
    /// tables per program instead of in the single `tables_prog_idx` slot --
    /// trades memory for latency, and this runtime ran out of memory at about
    /// ten 384 MiB arenas. The size of that trade was an arithmetic estimate off
    /// the type definitions until this printed it.
    ///
    /// `blocks` is the total number of basic-block entries across every
    /// function, not the number of functions that have a block map. That is the
    /// distinction the dose-response of 2026-08-17 could not settle: padding a
    /// guest with single-block functions moved the function count while holding
    /// blocks nearly constant, leaving ~11.2 ms of the 13.9 ms switch attributed
    /// to block work by inference alone.
    #[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
    fn report_table_build(&self, idx: usize, t0: u128, cached: bool) {
        let us = (mono_nanos() - t0) / 1000;
        let blocks: usize = self.bb_maps.iter().map(|(_, m)| m.len()).sum();
        let func_bytes = self.funcs.len() * core::mem::size_of::<(u64, LiftedFunc)>();
        let map_bytes = self.bb_maps.len() * core::mem::size_of::<(u64, Vec<(u64, *mut u64)>)>()
            + blocks * core::mem::size_of::<(u64, *mut u64)>();
        ecv_probe!(
            tables,
            "{} prog={} funcs={} fnmaps={} blocks={} bytes={} ({} KiB) {}us",
            // Distinct verbs so `grep -c rebuild` counts misses directly, which
            // is how the 602-against-600 measurement was taken.
            if cached { "cached " } else { "rebuild" },
            self.procs[idx].prog_idx,
            self.funcs.len(),
            self.bb_maps.len(),
            blocks,
            func_bytes + map_bytes,
            (func_bytes + map_bytes) / 1024,
            us
        );
    }

    /// Resets the live context to run a fresh program (execve): loads the
    /// program's data + a new guest stack into the (fixed) live arena, resets
    /// the live State, rebuilds the tables, and marks the current process for a
    /// fresh scheduler entry.
    pub fn exec_into(&mut self, prog_idx: usize, argv: &[Vec<u8>], envp: &[Vec<u8>]) {
        // POSIX execve: close every close-on-exec fd (the fd table otherwise
        // survives the execve). Non-cloexec fds (stdio, inherited redirections)
        // stay open for the new image.
        // execve from ANY thread terminates every other thread in the group, and
        // the caller takes over the group leader's identity. Without this the
        // siblings stay Runnable with replay frames naming VMAs in the image
        // that is about to be replaced -- the scheduler would re-enter them into
        // another program's address space.
        let tgid = self.procs[self.current].tgid;
        if self.is_multithreaded(tgid) {
            for i in 0..self.procs.len() {
                if i == self.current
                    || self.procs[i].tgid != tgid
                    || self.procs[i].status == ProcStatus::Dead
                {
                    continue;
                }
                self.clear_and_wake_child_tid(i);
                self.procs[i].status = ProcStatus::Dead;
                self.procs[i].blocked_on = BlockedOn::None;
                // Their saved registers and replay describe the OLD image.
                self.procs[i].state = None;
                self.procs[i].replay = None;
                self.procs[i].call_history = None;
            }
            // Linux hands the leader's pid to the execing thread, and the parent
            // is waiting on exactly that pid -- so it has to move, not be
            // recreated. Every other member is Dead by now, including the old
            // leader entry if it was not us.
            if self.procs[self.current].pid != tgid {
                self.procs[self.current].pid = tgid;
            }
        }
        self.close_cloexec_fds();
        // execve installs a NEW mm, and `begin_new_exec` sets its dumpable flag
        // to SUID_DUMP_USER -- a `PR_SET_DUMPABLE(0)` does not survive an exec.
        // Measured on Linux 6.17: a process that set 0 and then exec'd reads 1.
        // (The kernel's other case, a privilege-gaining exec forcing 0, cannot
        // arise here: no path changes credentials across exec_into.)
        self.procs[self.current].dumpable = SUID_DUMP_USER;
        // ⚠️ `thp_disable` is deliberately NOT reset beside it. The two flags
        // live in the same `mm_struct` and are the same shape, but MMF_DISABLE_THP
        // is in MMF_INIT_MASK and MMF_DUMPABLE is not, so exec CARRIES the first
        // and RESETS the second. Measured on Linux 6.17: a process that set
        // PR_SET_THP_DISABLE(1) and exec'd reads 1 in the new image, in the same
        // run where PR_GET_DUMPABLE reads back 1 after having been set to 0.
        // execve replaces the address space, so it drops every shared mapping
        // this process held -- the same teardown as exit, minus the process.
        // Doing it BEFORE the reset below also decides what the reset preserves.
        let execing = self.current_pid();
        self.shm_drop_process(execing);
        let prog = self.programs.get(prog_idx);
        // The live arena is about to hold the CURRENT process's fresh image, which
        // it already did; stated explicitly because this is the other place (with
        // `load_current`) that changes what the live arena holds.
        self.live_owner = self.current;
        // Whatever shared regions are left belong to OTHER processes and must
        // survive: there is one physical copy, so a wholesale wipe here would
        // zero a segment the postmaster is still using because one of its
        // children happened to exec.
        let surviving = core::mem::take(&mut self.shared_segments);
        self.arena.reset(&surviving);
        self.shared_segments = surviving;
        self.arena.load_data_sections(prog);
        // execve replaced the image in the arena. Record it, or a later switch
        // back to a process running the PREVIOUS program skips its reload and
        // runs against this one's text.
        self.materialized_prog = prog_idx;
        let sp = self.arena.build_stack(prog, argv, envp, self.uid, self.gid);
        unsafe {
            core::ptr::write_bytes(self.live_state, 0, 1);
            setup_state(&mut *self.live_state, prog, sp);
        }
        // Park the tables of the program being replaced -- another process may
        // still be running it -- and drop any parked copy of the one being
        // exec'd into, since the live tables become the only copy below.
        if let Some(prev) = self.tables_prog_idx {
            let parked = (
                core::mem::take(&mut self.funcs),
                core::mem::take(&mut self.bb_maps),
            );
            if let Some(slot) = self.tables_cache.get_mut(prev) {
                *slot = Some(parked);
            }
        }
        if let Some(slot) = self.tables_cache.get_mut(prog_idx) {
            *slot = None;
        }
        // Built rather than taken from the cache: `merge_libraries` below is the
        // `with_data` form and must run for its ARENA effect (execve re-initialises
        // libc's data), so the cheap path would save only the build and still pay
        // the merge.
        let (funcs, bb_maps) = build_tables(prog);
        self.funcs = funcs;
        self.bb_maps = bb_maps;
        // Re-merge the shared library units (the ONE lifted libc) into the new
        // image's tables + arena. The arena.reset above wiped libc's data and
        // build_tables reset the func table to the execve'd binary alone, so libc
        // must be re-merged with pristine data (correct execve semantics). No-op
        // unless this module uses a shared libc.
        self.merge_libraries();
        self.tables_prog_idx = Some(prog_idx);
        self.procs[self.current].prog_idx = prog_idx;
        // Fresh re-entry at the new image's entry (NOT a reconstruction): clear any
        // replay state and mark not-started so the scheduler runs entry_func, and
        // the trampoline (seeing started==false) skips the replay capture on the
        // execve yield. The old image's stack is abandoned in place.
        self.procs[self.current].started = false;
        self.procs[self.current].replay = None;
        // ...and the call history, which describes the image being REPLACED.
        //
        // Every frame in it names a VMA in the old program, so after execve it
        // is not merely stale but meaningless, and two consumers read it:
        //
        //   * the runaway-recursion guard counts its length, so inherited
        //     frames inflate the depth. One exec level stayed under the limit;
        //     `dash -> initdb -> postgres` accumulated two and aborted a
        //     healthy initdb with "runaway recursion at fn 0x781c60 (depth
        //     96)" in code that had just completed the same work as pid 1.
        //   * `fork_current` seeds the replay driver from it, so a process that
        //     execs and THEN forks would have its child reconstruct frames from
        //     a dead image -- the worse of the two, and silent.
        self.call_history.clear();
        self.pending = Pending::Yield;

        // execve replaces the image, so every dlopen'd unit goes with it -- as
        // it does natively. Leaving the set would be actively wrong rather than
        // merely stale: `arena.reset` below wipes the plugin's data, so a
        // `dlopen` in the NEW image would find `inited` true, return a handle,
        // and hand the guest code whose `.data` is zeroes.
        let cur = self.current;
        self.procs[cur].units.clear();

        // POSIX execve signal semantics: dispositions of CAUGHT signals reset to
        // SIG_DFL (the new image has no handler); SIG_IGN survives; the blocked
        // mask is preserved. Cheap to keep correct even though the builtin path
        // never installs a handler before execve.
        for a in self.signals.actions.iter_mut() {
            if a.handler > 1 {
                *a = SigAction::dfl();
            }
        }

        // Re-run the load-time runtime setup for the new image, exactly as the
        // initial entry (entry.rs) does: copy every PT_TLS module's `.tdata`
        // template to its TP block and resolve the load-time ifunc GOT slots. A
        // PRELINKED dynamic program (a fused-glibc ET_EXEC) REQUIRES both after an
        // execve — its dynamic `__libc_start_main` runs neither glibc's static
        // `__libc_setup_tls` nor the `__rela_iplt_*` apply loop, so without this a
        // `__thread` read sees an uninitialized block and an indirect call through
        // an unresolved ifunc GOT slot lands on the resolver instead of the
        // implementation (the multi-binary dynamic-PIE execve path). For a plain
        // static binary this is the same `setup_tls` the entry program already
        // gets at init and an `apply_ifuncs` no-op (no `.ecv.irela` section), so
        // the static execve/fork fixtures are unaffected. It runs synchronously
        // here (the resolvers are nested guest calls on a scratch stack): asyncify
        // is still in normal mode because `sys_execve` unwinds only AFTER
        // `exec_into` returns.
        let arena_ptr = self.arena.base_ptr();
        let base = self.live_state as *const State;
        unsafe {
            self.setup_tls(crate::arena::THREAD_PTR);
            self.apply_ifuncs(arena_ptr, base);
            // The loader (which runs early_init + init_array) is bypassed here too,
            // so the execve'd glibc program needs its main-thread ctype/locale
            // (__libc_early_init) and its DT_INIT/DT_INIT_ARRAY constructors run, or
            // e.g. a shell's `exec /bin/gosu` target sees a null ctype table and no
            // constructors. Static binaries have no .ecv.early/.ecv.init -> no-ops.
            self.apply_early_init(arena_ptr, base);
            // Initialize glibc's _rtld_global stack-list heads (ld.so's pthread
            // bring-up, which we skip); without it an execve'd dynamic program's
            // later fork() would wedge in __libc_fork child cleanup.
            self.apply_stacklists(arena_ptr, base);
            self.apply_musl_tp(arena_ptr, base);
            self.apply_init_array(arena_ptr, base);
        }
    }

    fn wake_waiter(&mut self, ppid: u32) {
        if let Some(i) = self.find_pid(ppid) {
            if self.procs[i].status == ProcStatus::Blocked
                && self.procs[i].blocked_on == BlockedOn::Wait
            {
                self.make_runnable(i);
            }
        }
    }

    /// Wakes every process blocked reading the given pipe (data arrived or the
    /// write end closed → EOF).
    pub fn wake_pipe_readers(&mut self, pipe_idx: usize) {
        for i in 0..self.procs.len() {
            if self.procs[i].status == ProcStatus::Blocked
                && self.procs[i].blocked_on == BlockedOn::PipeRead(pipe_idx)
            {
                self.make_runnable(i);
            }
        }
        // A pipe write/close also changes readiness for anyone watching that pipe
        // through an epoll set, so the pollers must be re-evaluated too.
        self.wake_pollers();
    }

    /// Wakes every process blocked in `accept` on the given named AF_UNIX
    /// socket, and re-evaluates the pollers (a queued connection makes a
    /// listening socket readable, which is how an event-driven server notices).
    pub fn wake_unix_acceptors(&mut self, listener: usize) {
        for i in 0..self.procs.len() {
            if self.procs[i].status == ProcStatus::Blocked
                && self.procs[i].blocked_on == (BlockedOn::UnixAccept { listener })
            {
                self.make_runnable(i);
            }
        }
        self.wake_pollers();
    }

    /// Wakes every process parked in `epoll_pwait` / a blocking signalfd read.
    /// Called after any event that can change fd readiness (a posted signal, a
    /// pipe write or close). Each woken process re-evaluates its interest list
    /// and re-blocks if nothing is actually ready, so over-waking is safe.
    ///
    /// The pollers are enqueued starting from a ROTATING index rather than always
    /// from 0, which is what makes several processes waiting on one socket share
    /// the work. They are all appended to a FIFO run queue, so the first enqueued
    /// is the first to run -- and with N nginx workers epolling one listening
    /// socket, the first to run accepts the connection and the rest find nothing.
    /// A fixed scan order therefore hands every request to the same worker: with
    /// four workers, 100 requests at 25-way concurrency all went to one of them,
    /// on both libcs, and `accept_mutex on` could not change it because it
    /// arbitrates who *tries* to accept, one stage after who *runs*.
    ///
    /// Linux has no equivalent problem -- it wakes every epoll waiter and they run
    /// concurrently on different CPUs -- so this is a cost of scheduling
    /// cooperatively, and the fairness has to be supplied here.
    ///
    /// Note this is NOT the path nginx's workers take. A worker whose epoll
    /// interest list contains a socket parks in `BlockedOn::Socket` and is woken
    /// by `poll_sockets_and_wake`, which carries the same rotation for the same
    /// reason. Rotating only here fixed nothing, and measured identically to no
    /// change at all -- if the balance does not move, check which `BlockedOn` the
    /// processes are actually in before touching the order again.
    pub fn wake_pollers(&mut self) {
        let n = self.procs.len();
        if n == 0 {
            return;
        }
        let start = self.wake_cursor % n;
        let mut woken = 0usize;
        for k in 0..n {
            let i = (start + k) % n;
            if self.procs[i].status == ProcStatus::Blocked
                && self.procs[i].blocked_on == BlockedOn::Poll
            {
                self.make_runnable(i);
                woken += 1;
            }
        }
        if woken > 1 {
            self.wake_cursor = self.wake_cursor.wrapping_add(1);
        }
    }
    /// True when a task parked in `on` can be released early by a signal. Poll
    /// waiters are excluded because `wake_pollers` handles them wholesale.
    pub(crate) fn signal_interruptible(on: BlockedOn) -> bool {
        matches!(
            on,
            BlockedOn::Socket { .. }
                | BlockedOn::Wait
                | BlockedOn::UnixAccept { .. }
                | BlockedOn::Sleep
                // A futex waiter too: Linux interrupts one for a deliverable
                // signal, and it is where every glibc timed wait parks. It
                // resumes, runs the handler at the boundary in `sys_futex`, and
                // reports an ordinary wake -- so the cost of a needless one is a
                // context switch, not a wrong answer. `wake_task_for_signal`
                // already refuses to wake a task that BLOCKS the signal, which
                // is what keeps that cost off the common path.
                | BlockedOn::Futex { .. }
        )
    }

    /// A task's blocked mask, live or saved. The running task's is on the
    /// context; everyone else's is in the table.
    fn task_blocked(&self, idx: usize) -> u64 {
        if idx == self.current {
            self.task_signals.blocked
        } else {
            self.procs[idx].task_signals.blocked
        }
    }

    /// Files `bit` in the GROUP's pending queue, reached through whichever member
    /// currently holds the group's table.
    ///
    /// Writing to `procs[idx]` directly would take the materialise-a-new-state
    /// branch below for a non-holder and silently split the group's table in two,
    /// so half its pending signals would be invisible.
    fn set_group_pending(&mut self, idx: usize, bit: u64) {
        let i = if self.procs[idx].tgid == self.procs[self.current].tgid {
            self.current
        } else {
            self.tables_holder(idx)
        };
        if i == self.current {
            self.signals.pending |= bit;
        } else if let Some(st) = self.procs[i].signals.as_mut() {
            st.pending |= bit;
        } else {
            // A process that has never been context-switched out has no saved
            // state yet; materialize one so the signal is not lost.
            let mut st = SignalState::default();
            st.pending |= bit;
            self.procs[i].signals = Some(st);
        }
    }

    /// Files `bit` in ONE TASK's own pending queue.
    fn set_task_pending(&mut self, idx: usize, bit: u64) {
        if idx == self.current {
            self.task_signals.pending |= bit;
        } else {
            self.procs[idx].task_signals.pending |= bit;
        }
    }

    /// Releases `idx` if it is parked in a wait a signal may interrupt. A task
    /// that BLOCKS the signal is left alone: waking it would cost a context
    /// switch to discover there is nothing deliverable and re-park.
    fn wake_task_for_signal(&mut self, idx: usize, bit: u64) {
        if idx == self.current || self.procs[idx].status != ProcStatus::Blocked {
            return;
        }
        // SIGKILL passes BOTH filters below. It cannot be blocked, and it is the
        // one signal whose effect must reach a task parked in a wait no ordinary
        // signal interrupts -- a pipe read with no writer, an AF_UNIX accept.
        // Leaving those out is what makes `kill -9` work on an idle process and
        // not on a stuck one, which is the opposite of what it is for.
        //
        // Waking such a task cuts its wait short, and that would be wrong for a
        // signal it might survive. It cannot survive this one: `pick_next`
        // retires it before it re-enters the syscall, so the resume path never
        // runs and there is no truncated read to report.
        if bit == 1u64 << (SIGKILL - 1) {
            self.make_runnable(idx);
            return;
        }
        if Self::signal_interruptible(self.procs[idx].blocked_on)
            && self.task_blocked(idx) & bit == 0
        {
            self.make_runnable(idx);
        }
    }

    /// Posts a PROCESS-directed signal: `kill(pid)`, and the SIGCHLD the
    /// scheduler raises when a child exits. It goes to the group's shared queue,
    /// where ANY member that does not block it may take it -- so the wake has to
    /// pick such a member rather than assuming the leader.
    ///
    /// Returns false only when no such pid exists (the caller reports ESRCH);
    /// `sig == 0` is the existence check and posts nothing.
    pub fn post_signal(&mut self, pid: u32, sig: u32) -> bool {
        let Some(idx) = self.procs.iter().position(|p| p.pid == pid) else {
            return false;
        };
        if sig == 0 {
            return true; // existence check only
        }
        if sig as usize >= NSIG {
            return false;
        }
        let bit = 1u64 << (sig - 1);
        self.set_group_pending(idx, bit);
        ecv_debug!(sched, "post sig={sig} -> pid={pid} (process-directed)");
        // Wake a member that can actually take it. Waking the addressed task
        // regardless is what made a group-directed signal look delivered while
        // the only woken thread was one that blocked it.
        let tgid = self.procs[idx].tgid;
        // SIGKILL takes the whole group and no member can decline it, so there is
        // no candidate to choose: every blocked member is released. Only one of
        // them has to reach `pick_next` for the group to be torn down, but the
        // others are parked on waits that are about to stop existing, and leaving
        // them there would strand a task on a pipe whose descriptors the teardown
        // has already closed.
        if sig == SIGKILL {
            for i in 0..self.procs.len() {
                if self.procs[i].tgid == tgid {
                    self.wake_task_for_signal(i, bit);
                }
            }
            self.wake_pollers();
            return true;
        }
        let (cur, live) = (self.current, self.task_signals.blocked);
        let cand = signal_wake_candidate(&self.procs, tgid, bit, |i| {
            if i == cur {
                live
            } else {
                self.procs[i].task_signals.blocked
            }
        });
        if let Some(i) = cand {
            self.wake_task_for_signal(i, bit);
        }
        self.wake_pollers();
        true
    }

    /// Posts a THREAD-directed signal: `tgkill`/`tkill`, which is what
    /// `pthread_kill` issues, and a synchronous fault such as SIGILL. It goes to
    /// that task's own queue and no other task may take it.
    ///
    /// A signal interrupts any interruptible wait of the TARGET task. Poll
    /// waiters are handled by `wake_pollers`; the target is also woken if it is
    /// parked on a socket (the postmaster's ServerLoop epoll blocks as a socket
    /// waiter), in wait4, or in a SLEEP -- the last being the case with the most
    /// visible symptom, since without it a 60 s sleep swallows a signal for 60 s
    /// and the guest reports it as a hang rather than a delay.
    pub fn post_signal_to_thread(&mut self, tid: u32, sig: u32) -> bool {
        let Some(idx) = self.procs.iter().position(|p| p.pid == tid) else {
            return false;
        };
        if sig == 0 {
            return true;
        }
        if sig as usize >= NSIG {
            return false;
        }
        let bit = 1u64 << (sig - 1);
        self.set_task_pending(idx, bit);
        ecv_debug!(sched, "post sig={sig} -> tid={tid} (thread-directed)");
        self.wake_task_for_signal(idx, bit);
        self.wake_pollers();
        true
    }

    /// Takes `bit` out of whichever queue holds it. A thread-directed signal and
    /// a process-directed one of the same number can both be pending at once;
    /// accepting the signal consumes both, because standard signals do not
    /// queue and the guest is told about it exactly once.
    pub fn consume_signal(&mut self, bit: u64) {
        self.task_signals.pending &= !bit;
        self.signals.pending &= !bit;
    }

    /// Delivers every pending, unblocked signal that has an installed SA_HANDLER
    /// to the CURRENT process by running its handler as a nested guest call. This
    /// is what turns an async signal into the guest's handler actually executing
    /// -- the missing half of the signal machinery `post_signal` sets up. Called
    /// at a signal-delivery boundary (`epoll_pwait`), which passes the effective
    /// `wait_mask` for the wait (a signal the process blocks in its main loop but
    /// unblocks for the wait is deliverable here); a signal in `wait_mask` stays
    /// pending (e.g. a signalfd-owned SIGURG is consumed by the fd, not a handler).
    ///
    /// Concretely this drives PostgreSQL's postmaster reaper: the startup process
    /// exits -> `post_signal` posts SIGCHLD + wakes the postmaster from its
    /// ServerLoop epoll -> on resume this runs `handle_pm_child_exit`, which sets
    /// the child-exit flag and `SetLatch()`; SetLatch does `kill(self, SIGURG)`,
    /// so the latch signalfd becomes readable and the SAME `epoll_pwait` returns
    /// the latch event -> the main loop reaps -> "ready to accept connections".
    ///
    /// Delivery model (cooperative, synchronous): unlike a real kernel we do NOT
    /// build an rt_sigframe to asynchronously interrupt arbitrary code. We invoke
    /// the handler as an ordinary `void h(int, ...)` call -- exactly as ctors run
    /// (`apply_init_array`) -- on the interrupted process's own stack, with a
    /// CLONED State so its live register file is untouched. The handler returns
    /// normally through its lifted epilogue, so there is no sa_restorer /
    /// rt_sigreturn round-trip to model. call_history stays balanced for the same
    /// reason a ctor's does: elflift emits the push/pop around each call site in
    /// the CALLER, so a function entered directly from Rust never pushes its own
    /// frame. Constraint: a handler must not itself block in a syscall
    /// (async-signal-safety already forbids that); if one did, its `ecv_yield`
    /// would unwind the enclosing scheduler leg -- accepted, as ctors are too.
    /// Returns how many handlers actually ran. `rt_sigsuspend` needs that: it
    /// must report EINTR once a handler has run and keep waiting otherwise, and
    /// "was anything delivered" is not recoverable from `pending` afterwards.
    pub unsafe fn deliver_pending_signals(&mut self, wait_mask: u64) -> usize {
        let arena_ptr = self.arena.base_ptr();
        let mut ran = 0usize;
        // Snapshot the deliverable set once: signals pending AND unblocked for this
        // wait, lowest-numbered first. Iterating a snapshot (not re-reading
        // `pending`) is deliberate: a handler may raise a FURTHER signal -- e.g.
        // SetLatch's SIGURG, which a signalfd owns and must stay PENDING for the fd
        // to read, not be handed to a handler -- and snapshotting keeps that signal
        // out of this pass. It is picked up (or consumed by its fd) on the next entry.
        //
        // BOTH queues: a thread may dequeue a signal directed at it
        // (`tgkill` -> `TaskSignals::pending`) or one directed at the process
        // (`kill` -> the group's `pending`), exactly as Linux does.
        let mut deliverable =
            deliverable_set(self.signals.pending, self.task_signals.pending, wait_mask);
        // ⚠️ SIGKILL AND SIGSTOP HAVE NO DISPOSITION TO RUN. `rt_sigaction` here
        // records one for any signal 1..=64, where Linux answers EINVAL, so the
        // refusal has to happen where the disposition is CONSUMED. Leaving them in
        // is not a cosmetic deviation: a guest that installs SIG_IGN for SIGKILL
        // would reach the SIG_IGN arm below, `consume_signal` would clear the
        // pending bit, and the `pending_termination` check after the loop -- which
        // reads that same bit -- would find nothing. The process would be
        // unkillable, by two lines of ordinary-looking guest setup.
        //
        // Their default actions are decided after the loop like every other one,
        // from the pending bits, which is also why SIGKILL needs no exemption from
        // `deliverable_set`'s mask subtraction: nothing in this loop would act on
        // it, and `terminating_signal` ignores `blocked` for SIGKILL itself.
        deliverable &= !((1u64 << (SIGKILL - 1)) | (1u64 << (SIGSTOP - 1)));
        while deliverable != 0 {
            let sig = deliverable.trailing_zeros() + 1;
            let bit = 1u64 << (sig - 1);
            deliverable &= !bit;
            let act = self.signals.actions[sig as usize];
            match act.handler {
                // SIG_DFL: the default action, which `terminating_signal` rules
                // on below for the whole task at once rather than per signal.
                // The bit is deliberately LEFT PENDING either way: a signalfd
                // reads readiness out of `pending`, and dropping the bit here
                // would swallow an fd-owned signal.
                0 => continue,
                // SIG_IGN: consume and discard. From whichever queue held it --
                // clearing only one leaves the signal to be "delivered" again on
                // the next boundary, forever.
                1 => self.consume_signal(bit),
                // Installed handler: accept (standard signals don't queue), run it.
                handler => {
                    self.consume_signal(bit);
                    self.run_signal_handler(arena_ptr, sig, &act, handler);
                    ran += 1;
                }
            }
        }
        // Default actions. Handlers run FIRST: a signal with a handler is not a
        // default action at all, and one of those handlers may be what installs
        // or clears a disposition this then reads.
        //
        // ⚠️ `ran` is NOT incremented for a termination, and that is deliberate
        // twice over. `rt_sigsuspend` reports EINTR on a non-zero count, which is
        // meaningless to a process that is about to be retired; and
        // `__ecv_warning` treats a zero count as "SIGILL reached no handler" and
        // raises the fatal that names the undecoded ENCODING. Counting a
        // default-Term SIGILL as delivered would swallow that diagnostic and turn
        // a lifter defect into a guest that quietly exits 132.
        if let Some(sig) = self.pending_termination() {
            self.arm_signal_exit(sig);
        }
        ran
    }

    /// The signal that must terminate the CURRENT task now, if any.
    ///
    /// Reads the live tables, so it is only ever asked about the running
    /// process. That is not a limitation: Linux performs a default action in the
    /// target's own context too, on its way back to userspace, and the three
    /// callers here are the cooperative equivalents of that moment -- a delivery
    /// boundary, the instant before a task parks, and the instant before a task
    /// is handed the CPU.
    pub fn pending_termination(&self) -> Option<u32> {
        terminating_signal(
            self.signals.pending,
            self.task_signals.pending,
            self.task_signals.blocked,
            &self.signals.actions,
        )
    }

    /// Whether posting `sig` to the CURRENT task right now would run a guest
    /// handler. See [`signal_reaches_handler`] for why a caller would want to
    /// know that BEFORE posting rather than after.
    #[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
    pub fn delivers_to_handler(&self, sig: u32) -> bool {
        signal_reaches_handler(sig, self.task_signals.blocked, &self.signals.actions)
    }

    /// Arms the exit a terminating default action performs, from inside a
    /// syscall.
    ///
    /// It does exactly what `sys_exit` does -- `Pending::Exit` plus the unwind
    /// flag -- so the whole teardown (descriptors, shared segments, the parent's
    /// SIGCHLD and its `wait4`) is the one that already exists, rather than a
    /// second one written for signals. The svc trampoline reads `suspended`,
    /// EH-unwinds the leg, and `retire_after_suspend` applies it.
    ///
    /// The pending bit is NOT consumed. A caller that goes on to block
    /// (`ppoll`, `epoll_pwait` and `rt_sigsuspend` all can) overwrites
    /// `self.pending` with `Pending::Block`, and `retire_after_suspend` re-asks
    /// `pending_termination` precisely to catch that; consuming the bit here
    /// would leave that check nothing to find and park a task that must die.
    ///
    /// The reason armed is [`ExitReason::Killed`] carrying the SIGNAL NUMBER,
    /// not `128 + signo`. The shell's rendering is recovered where it is
    /// actually wanted -- `ExitReason::status_code`, which is what the module
    /// exits with when init dies -- while the parent's `wait4` gets the kernel's
    /// `WIFSIGNALED`/`WTERMSIG` encoding. ⚠️ Arming `Exited(128 + signo)` here
    /// instead would compile, run, and silently restore the old defect: every
    /// signal death would read as a normal exit to the parent, which is what
    /// PostgreSQL's postmaster uses to decide how to log a crash.
    ///
    /// ⚠️ Six of the seven `deliver_pending_signals` call sites are syscall
    /// handlers, where the svc trampoline consumes `suspended` on the way out.
    /// The seventh is `__ecv_warning`, which is called from lifted code and
    /// checks nothing: it raises SIGILL for an undecoded instruction and
    /// `fatal!`s when no HANDLER ran, so an armed exit there is normally
    /// unreachable rubble behind a process that is already dying. Should a
    /// handler have run and a *different* terminating signal be due, the flag
    /// simply survives to the guest's next syscall and the leg unwinds there --
    /// late by one syscall, never wrong.
    ///
    /// ⚠️ **"Normally unreachable" held only while `fatal!` was the sole
    /// outcome.** The undecoded-instruction census (`diag::Undecoded::Census`)
    /// RETURNS from `__ecv_warning` instead of aborting, and it reached exactly
    /// this rubble: the exit armed here survived to the guest's next syscall,
    /// the syscall completed, the leg unwound, and the module exited 132 with no
    /// diagnostic. An instrument meant to enumerate every undecoded site a
    /// workload executes reported about ONE per run, and a truncated list that
    /// says nothing reads as a complete one.
    ///
    /// The fix is upstream of here and deliberately so: `__ecv_warning` now asks
    /// [`signal_reaches_handler`] BEFORE posting, and in census mode with no
    /// handler it never posts, so no default action is ever armed. Nothing
    /// CANCELS an armed exit -- cancelling one armed for another reason (a real
    /// `kill`, a SIGKILL) would be a far worse defect than the one being fixed,
    /// and this function's contract is unchanged: what it arms, stays armed.
    fn arm_signal_exit(&mut self, sig: u32) {
        ecv_debug!(
            sched,
            "sig={sig} default action TERM -> exit {}",
            128 + sig as i32
        );
        self.pending = Pending::Exit(ExitReason::Killed(sig));
        self.suspended = true;
    }

    /// Runs one guest signal handler synchronously (see `deliver_pending_signals`).
    unsafe fn run_signal_handler(
        &mut self,
        arena_ptr: *mut u8,
        sig: u32,
        act: &SigAction,
        handler: u64,
    ) {
        let Some(f) = self.func_at(handler) else {
            fatal!(
                "signal handler 0x{handler:x} for signal {sig} not in the lifted function table"
            );
        };
        ecv_debug!(sched, "deliver sig={} -> handler 0x{:x}", sig, handler);
        // Kernel semantics: block `sig` (+ sa_mask) while the handler runs, unless
        // SA_NODEFER. Restored when it returns.
        let saved_blocked = self.task_signals.blocked;
        self.task_signals.blocked |= act.mask;
        if act.flags & SA_NODEFER == 0 {
            self.task_signals.blocked |= 1u64 << (sig - 1);
        }

        // Build the handler frame on the interrupted process's OWN stack, just
        // below its live SP past the 128-byte red zone, 16-byte aligned. The real
        // stack (not a scratch page) stays valid for a running process whose mmap
        // arena is in use, and the CLONED State means the original SP is preserved.
        let base = self.live_state as *const State;
        let live_sp = (*self.live_state).gpr.sp.val;
        let mut sp = (live_sp - 128) & !15u64;

        let mut st = State::new_boxed();
        core::ptr::copy_nonoverlapping(base, &mut *st, 1);
        st.gpr.x[0].val = sig as u64;
        if act.flags & SA_SIGINFO != 0 {
            // Minimal siginfo_t (128 B) + zeroed ucontext placeholder; only
            // si_signo is populated. postgres's SIGNAL_ARGS handlers read neither,
            // but an SA_SIGINFO handler dereferences x1/x2, so both must be valid
            // guest pointers.
            sp = (sp - 128) & !15u64;
            let si = sp;
            {
                let b = self.arena.slice_mut(si, 128);
                b.fill(0);
                b[0..4].copy_from_slice(&sig.to_le_bytes());
            }
            sp = (sp - 512) & !15u64;
            let uc = sp;
            self.arena.slice_mut(uc, 512).fill(0);
            st.gpr.x[1].val = si;
            st.gpr.x[2].val = uc;
        }
        st.gpr.sp.val = sp;
        st.gpr.pc.val = handler;

        let ctx_ptr = self as *mut EcvContext;
        self.in_signal_handler += 1;
        f(arena_ptr, &mut *st, handler, ctx_ptr);
        self.in_signal_handler = self.in_signal_handler.saturating_sub(1);

        self.task_signals.blocked = saved_blocked;
    }

    // `wake_expired_deadlines` lived here. It both SLEPT and swept, which is why
    // it was a blocking site; `resume_scheduling` now hands the deadline to the
    // backend as its timeout and calls `sweep_expired_deadlines` afterwards, so
    // the sleep belongs to whichever backend is willing to perform it.

    /// The soonest deadline among blocked processes, if any. Used both to size
    /// the idle sleep and to bound the idle host socket poll.
    fn earliest_deadline(&self) -> Option<u128> {
        self.procs
            .iter()
            .filter(|p| p.status == ProcStatus::Blocked)
            .filter_map(|p| p.deadline)
            .min()
    }

    /// Releases every process whose deadline has already passed, without
    /// sleeping. Split out of `wake_expired_deadlines` so the idle path can wait
    /// inside the host socket poll (which watches the sockets at the same time)
    /// and then sweep, rather than sleeping blind first.
    fn sweep_expired_deadlines(&mut self) -> bool {
        // `Process::deadline` is monotonic, like every other deadline here.
        let now = mono_nanos();
        let mut woke = false;
        for i in 0..self.procs.len() {
            if self.procs[i].status != ProcStatus::Blocked {
                continue;
            }
            match self.procs[i].deadline {
                Some(d) if d <= now => {
                    self.make_runnable(i);
                    self.procs[i].timed_out = true;
                    woke = true;
                }
                _ => {}
            }
        }
        woke
    }

    /// Records a deadline for the process that is blocking now.
    pub fn set_current_deadline(&mut self, deadline: Option<u128>) {
        let cur = self.current;
        self.procs[cur].deadline = deadline;
    }

    /// The deadline already armed for the current process, if any. A syscall
    /// that is being RESUMED uses this to keep its original deadline instead of
    /// restarting it: the wake it just took may have been spurious (a socket
    /// went ready for somebody else), and re-arming a fresh timeout on every
    /// such wake makes a 5s guest timer expire in a multiple of 5s, once per
    /// spurious wake. Measured on nginx with four workers: an idle keepalive
    /// connection closed at either 4.59s or 19.78s, never in between.
    pub fn current_deadline(&self) -> Option<u128> {
        self.procs[self.current].deadline
    }

    /// Takes the timed-out flag for the process being resumed, so the syscall
    /// that blocked can tell a timeout from a wake-up.
    pub fn take_timed_out(&mut self) -> bool {
        let cur = self.current;
        std::mem::take(&mut self.procs[cur].timed_out)
    }

    pub fn wake_futex(&mut self, uaddr: u64, n: u32) -> u32 {
        let mut woken = 0u32;
        for i in 0..self.procs.len() {
            if woken >= n {
                break;
            }
            if self.procs[i].status == ProcStatus::Blocked
                && self.procs[i].blocked_on == (BlockedOn::Futex { uaddr })
            {
                self.make_runnable(i);
                woken += 1;
            }
        }
        woken
    }

    /// The `CLONE_CHILD_CLEARTID` half of retiring a task: zero the word it
    /// registered (via `clone`'s ctid argument or `set_tid_address`) and wake
    /// every futex waiter on it.
    ///
    /// This is what ends a `pthread_join`. The join blocks on that word being
    /// non-zero, and only the kernel -- here, this -- ever clears it, so a
    /// runtime that retires a thread and skips this leaves the joiner parked
    /// against a condition that can no longer change.
    ///
    /// Wakes ALL waiters rather than one: musl points every thread's ctid at the
    /// single `__thread_list_lock`, so the waiters on one address are not
    /// necessarily waiting for the same thread.
    fn clear_and_wake_child_tid(&mut self, idx: usize) {
        let addr = core::mem::take(&mut self.procs[idx].clear_child_tid);
        // A guest pointer that outlived its mapping is a real possibility here
        // (the task is dying); refuse rather than scribble.
        if addr == 0 || !self.arena.in_bounds(addr, 4) {
            return;
        }
        self.arena
            .slice_mut(addr, 4)
            .copy_from_slice(&0u32.to_le_bytes());
        self.wake_futex(addr, u32::MAX);
    }

    /// Makes a blocked process runnable and queues it.
    ///
    /// Deliberately does NOT clear `deadline`. A wake is not the end of a wait:
    /// `wake_socket_waiters_on` over-wakes on purpose, so a process woken that
    /// way usually re-blocks in the same syscall, and that syscall's timeout has
    /// to keep running. Clearing here made a 5s guest timer restart on every
    /// spurious wake and expire in a multiple of what was asked for -- measured
    /// as an idle keepalive connection closing at 19.50s instead of 4.50s. The
    /// stale-deadline problem this looked like it was solving is handled where it
    /// actually belongs, at the start of a FRESH syscall; see `svc_dispatch`.
    fn make_runnable(&mut self, i: usize) {
        self.procs[i].status = ProcStatus::Runnable;
        self.procs[i].blocked_on = BlockedOn::None;
        self.run_queue.push_back(i);
    }

    // ---- The shared window -------------------------------------------------
    //
    // One physical arena, so a shared region is a VMA range exempted from the
    // per-process swap (see `SharedSeg`). These five methods are the whole of
    // its allocation and lifetime; every caller that hands out or gives up a
    // shared VMA goes through them, because the two failure modes -- leaking
    // the window, and recycling memory somebody still reads -- are both silent.

    /// Reserves `len` bytes in the shared window; see `ShmWindow::reserve`.
    ///
    /// The floor is the current process's private mmap bump. It is not a sound
    /// GLOBAL floor -- `arena.mmap_cur` travels with the arena, so another
    /// process may already sit higher than the running one -- but raising it to
    /// a high-water mark across all processes shrinks the window for everybody
    /// and is a separate fix; see TODO.md.
    pub fn shm_reserve(&mut self, len: u64) -> Option<u64> {
        let at = self.shm_window.reserve(len, self.arena.mmap_cur);
        // The shared side of the same 96 MiB window, as bytes carved DOWN from
        // its top -- the mirror of the arena's private bump.
        crate::diag::note_address_use(
            0,
            0,
            crate::arena::MMAP_END_VMA.saturating_sub(self.shm_window.top),
        );
        at
    }

    /// Index of the shared region starting at `vma`.
    pub fn shm_seg_at(&self, vma: u64) -> Option<usize> {
        self.shared_segments.iter().position(|s| s.vma_start == vma)
    }

    /// Records `pid` as a mapper of the region at index `i`.
    pub fn shm_add_mapper(&mut self, i: usize, pid: u32) {
        let m = &mut self.shared_segments[i].mappers;
        if !m.contains(&pid) {
            m.push(pid);
        }
    }

    /// Drops `pid` from every shared region and reclaims those it was the last
    /// to map. Called on exit and on execve, both of which tear down a whole
    /// address space without matching `munmap`s.
    pub fn shm_drop_process(&mut self, pid: u32) {
        let mut i = 0;
        while i < self.shared_segments.len() {
            self.shared_segments[i].mappers.retain(|&p| p != pid);
            if self.shm_try_reclaim(i) {
                continue; // the vec shifted the next entry down onto `i`
            }
            i += 1;
        }
    }

    /// Adds `to` as a mapper of every region `from` maps. Fork: the child
    /// inherits its parent's shared mappings, and keeps them alive on its own.
    pub fn shm_inherit(&mut self, from: u32, to: u32) {
        for seg in &mut self.shared_segments {
            if seg.mappers.contains(&from) {
                seg.mappers.push(to);
            }
        }
    }

    /// Reclaims `shared_segments[i]` if nothing maps it and its kind allows.
    /// Returns true if the entry was removed.
    pub fn shm_try_reclaim(&mut self, i: usize) -> bool {
        let seg = &self.shared_segments[i];
        if !seg.mappers.is_empty() {
            return false;
        }
        let (start, len, kind) = (seg.vma_start, seg.len as u64, seg.kind);
        match kind {
            SharedKind::Anon => {}
            SharedKind::File => {
                let Some(fi) = self.shm_files.iter().position(|f| f.vma == start) else {
                    return false;
                };
                // The NAME outlives the mapping, so the last unmap is not
                // necessarily the end: while the backing path still resolves, a
                // later mmap of it has to land on this same region and find the
                // bytes written through the old one. PostgreSQL's `posix` DSM
                // backend is exactly that -- /dev/shm/PostgreSQL.<n> mapped by
                // more than one process, not always at the same time.
                //
                // But that argument needs bytes to have been WRITTEN, and a
                // read-only mapping cannot have written any; see
                // `shm_file_reclaimable` for why that is the premise failing
                // rather than a heuristic, and for the 80 MiB of PRIVATE mmap
                // area the old path-only test cost.
                let path = self.shm_files[fi].path.clone();
                let writable = self.shm_files[fi].writable;
                let path_exists = self.vfs.resolve(b"/", &path, true).is_some();
                if !shm_file_reclaimable(writable, path_exists) {
                    return false;
                }
                self.shm_files.remove(fi);
            }
            SharedKind::SysV => {
                // IPC lifetime, not mapping lifetime: destroyed only once
                // IPC_RMID has been issued AND the last attach is gone. Until
                // then the segment deliberately outlives every process that
                // touched it, which is exactly what PostgreSQL's postmaster
                // interlock relies on -- a second postmaster finds the segment
                // of a first one that is no longer running and reads nattch back
                // through IPC_STAT to decide whether it may start.
                let Some(si) = self.shm.iter().position(|s| s.vma == start) else {
                    return false;
                };
                if !self.shm[si].removed {
                    return false;
                }
                self.shm.remove(si);
            }
        }
        self.shared_segments.remove(i);
        self.shm_window.release(start, start + len);
        ecv_debug!(
            shm,
            "reclaimed {kind:?} {start:#x}..{:#x} ({len} bytes); shm_top {:#x}, {} hole(s)",
            start + len,
            self.shm_window.top,
            self.shm_window.free.len()
        );
        true
    }

    /// Closes every descriptor the current process holds, applying the same pipe
    /// and socket refcounting an explicit `close` would. Called when a process
    /// exits; see the comment at that call site for what breaks without it.
    fn close_all_fds(&mut self) {
        for fd in 0..self.fds.len() {
            if self.fds.get(fd).map_or(false, Option::is_some) {
                // `close_fd_full` lives in the wasm-only `sys` because closing a
                // socket goes through a WASI import; the host build exists only
                // for unit tests and never runs a guest.
                #[cfg(target_arch = "wasm32")]
                crate::sys::close_fd_full(self, fd);
                #[cfg(not(target_arch = "wasm32"))]
                {
                    self.fds[fd] = None;
                }
            }
        }
    }

    /// Marks the current process blocked (a syscall is about to unwind).
    pub fn block_current(&mut self, on: BlockedOn) {
        ecv_debug!(sched, "block on {:?}", on);
        self.procs[self.current].blocked_on = on;
        self.pending = Pending::Block;
    }

    /// After a process suspends (unwind), updates the table for the pending
    /// action, saves the current process, and loads the next runnable one.
    /// Never returns if nothing is runnable (the whole module exits).
    /// Applies the pending action from the suspension that just happened, and
    /// files the current task away. Runs EXACTLY ONCE per suspension -- which is
    /// why it is split from the selection below, which may run many times.
    fn retire_after_suspend(&mut self) {
        ecv_trace!(
            sched,
            "after_suspend cur_idx={} pending={:?} blocked_on={:?}",
            self.current,
            self.pending,
            self.procs[self.current].blocked_on
        );
        match core::mem::replace(&mut self.pending, Pending::None) {
            Pending::Yield => self.run_queue.push_back(self.current),
            Pending::Block => {
                // ⚠️ A TASK WITH A TERMINATING SIGNAL DUE MUST NOT PARK. This is
                // the kernel's `signal_pending()` test at the top of every
                // interruptible sleep, and here it is load-bearing in a way it is
                // not there: a parked task is only re-entered when something
                // wakes it, so a process that parks after taking a fatal signal
                // is not merely late, it never dies at all.
                //
                // It is also the recovery for a delivery boundary that armed the
                // exit and then blocked anyway. `ppoll`, `epoll_pwait` and
                // `rt_sigsuspend` all call `deliver_pending_signals` and go on to
                // `block_current`, which overwrites `Pending::Exit` with
                // `Pending::Block` -- so the arming is deliberately non-consuming
                // and re-derived here rather than defended at each call site in
                // `sys`, where the next boundary added would have to remember.
                match self.pending_termination() {
                    Some(sig) => {
                        ecv_debug!(sched, "sig={sig} default action TERM at a block");
                        self.exit_group_current(ExitReason::Killed(sig));
                    }
                    None => self.procs[self.current].status = ProcStatus::Blocked,
                }
            }
            Pending::ExitThread(code) => {
                // One thread of a live group. Nothing that belongs to the group
                // may be torn down here: no close_all_fds, no shm_drop_process,
                // no SIGCHLD -- a thread is not a child, and its siblings are
                // still using every one of those things.
                let (tid, tgid) = {
                    let p = &self.procs[self.current];
                    (p.pid, p.tgid)
                };
                // Dead rather than Zombie: nothing can `wait4` a thread, so a
                // zombie thread would never be reaped and would sit in the
                // table forever pretending to be a child of the group's parent.
                self.procs[self.current].status = ProcStatus::Dead;
                self.clear_and_wake_child_tid(self.current);
                ecv_debug!(sched, "tid={tid} (tgid={tgid}) THREAD EXIT code={code}");
                // The dying thread is holding the GROUP's live fd table, cwd and
                // signal state. `save_current` below runs only for a task left
                // Runnable or Blocked, so without this the next `load_current`
                // would overwrite them and the surviving threads would come back
                // with no descriptors at all. Filing them under a dead member is
                // fine: `tables_holder` searches the group, not the living.
                self.save_current();
            }
            Pending::Exit(reason) => self.exit_group_current(reason),
            Pending::None => self.run_queue.push_back(self.current),
        }

        if matches!(
            self.procs[self.current].status,
            ProcStatus::Runnable | ProcStatus::Blocked
        ) {
            self.save_current();
        }
    }

    /// Retires the CURRENT task's whole thread group with status `code`.
    ///
    /// The body of the old `Pending::Exit` arm, unchanged and moved out so that a
    /// terminating default action performs the same teardown an `exit_group`
    /// does rather than a second one written for signals -- the descriptor close,
    /// the shared-window release, the parent's `wait4` wake and its SIGCHLD are
    /// all things a signal death owes the rest of the system exactly as much as a
    /// voluntary exit does.
    ///
    /// ⚠️ Requires the group's tables to be LIVE, which is why every caller runs
    /// with `self.current` loaded: `close_all_fds` walks `self.fds`, not
    /// `procs[i].fds`, so calling this for a task that is merely in the table
    /// would close somebody else's descriptors.
    fn exit_group_current(&mut self, reason: ExitReason) {
        // `exit_group`, or `exit` from a task with no siblings: the whole
        // thread group goes. Every other member is retired first so that
        // none of them is scheduled again, and the LEADER carries the
        // exit status, because the leader is the only member the parent
        // can wait for.
        let tgid = self.procs[self.current].tgid;
        // A non-leader may call exit_group, so resolve the leader BEFORE
        // retiring anyone -- `find_pid` skips Dead entries, and the loop
        // below would otherwise hide the very task whose status the
        // parent is waiting to read.
        let leader = self.find_pid(tgid).unwrap_or(self.current);
        for i in 0..self.procs.len() {
            if self.procs[i].tgid == tgid && i != leader {
                self.procs[i].status = ProcStatus::Dead;
                self.procs[i].blocked_on = BlockedOn::None;
                self.clear_and_wake_child_tid(i);
            }
        }
        self.clear_and_wake_child_tid(leader);
        self.procs[leader].status = ProcStatus::Zombie(reason);
        self.procs[leader].blocked_on = BlockedOn::None;
        if tgid == 1 {
            // The MODULE's exit code, which is a shell-level `$?` and not a wait
            // status -- so a signal death of init still leaves `128 + signo`,
            // exactly as before. Only what the parent's `wait4` sees changes.
            self.exit_code = reason.status_code();
        }
        let ppid = self.procs[leader].ppid;
        ecv_debug!(sched, "EXIT {:?} -> wake+SIGCHLD ppid={}", reason, ppid);
        // Linux closes every descriptor at exit, and that is what gives
        // a pipe reader EOF once its last writer dies. Skipping it turns
        // an ordinary "child failed" into a hang: initdb's popen() child
        // exited 127, the parent stayed in `read` on the pipe forever,
        // and the module died with
        //   deadlock: every process is blocked ... [(1, PipeRead(0))]
        // with the writer already a zombie. Sockets are refcounted the
        // same way, so this also releases a listener the child inherited.
        self.close_all_fds();
        // ...and unmaps its whole address space, which for us means
        // giving up its share of the shared window. Nothing else
        // returns an anonymous MAP_SHARED region: there is no name to
        // reach it by, and a process that dies holding one (or simply
        // never bothers to munmap, which is normal at exit) leaks it
        // permanently. `initdb` runs `postgres --boot` several times in
        // sequence and each run maps its own shared_buffers, so the
        // third run met `FATAL: could not map anonymous shared memory:
        // Cannot allocate memory` with both of its predecessors long
        // dead.
        // Every member's mappings, not just the caller's: shared regions
        // are recorded per pid, and a thread that mapped one and is now
        // being retired alongside the group would otherwise pin the
        // window forever.
        let members: Vec<u32> = self
            .procs
            .iter()
            .filter(|p| p.tgid == tgid)
            .map(|p| p.pid)
            .collect();
        for dying in members {
            self.shm_drop_process(dying);
        }
        let (brk, mmap, shm) = crate::diag::peak_address_use();
        ecv_debug!(
            mem,
            "exit -> linear memory {} MiB, peak guest call depth {} (limit {})",
            crate::diag::linear_memory_mib(),
            crate::diag::peak_depth(),
            crate::diag::max_depth(),
        );
        // ❗ THE ADDRESS BUDGET, as bytes used of each fixed region. This exists
        // to settle the ⭐ decision in `.agents/docs/TODO.md`: the 96 MiB mmap
        // window is shared between private mmap and MAP_SHARED, and its
        // neighbour -- the 96 MiB brk heap -- may be largely dead. If it is,
        // lowering `MMAP_START_VMA` relieves the starvation as a CONSTANT
        // change, with none of the cost of growing the arena or making
        // reservations lazy.
        ecv_debug!(
            mem,
            "address budget -> brk {} KiB of {} MiB | private mmap {} KiB | shared {} KiB | window {} MiB total",
            brk / 1024,
            (crate::arena::BRK_END_VMA - crate::arena::BRK_START_VMA) / (1024 * 1024),
            mmap / 1024,
            shm / 1024,
            (crate::arena::MMAP_END_VMA - crate::arena::MMAP_START_VMA) / (1024 * 1024),
        );
        // Wake a parent parked in wait4 (BlockedOn::Wait)...
        self.wake_waiter(ppid);
        // ...and, as Linux does, post SIGCHLD to the parent. A parent that
        // instead waits in epoll_pwait over a signalfd (PostgreSQL's postmaster
        // watches SIGCHLD there, not wait4) only learns a child exited via this
        // signal: post_signal sets the pending bit -> the signalfd becomes
        // readable -> wake_pollers unblocks the epoll wait -> the postmaster
        // reaps and advances (e.g. startup process exits -> "database system is
        // ready to accept connections"). wake_waiter alone left it blocked.
        self.post_signal(ppid, SIGCHLD);
    }

    /// ONE selection pass. Never blocks, never exits.
    ///
    /// Everything that used to happen inside the old scheduler's `loop` that
    /// could WAIT now lives in the driver below, and everything that could
    /// TERMINATE now lives in `entry.rs`. What is left is a pure decision, which
    /// is what makes the idle path reachable from `cargo test`.
    fn pick_next(&mut self) -> SchedOutcome {
        while let Some(next) = self.run_queue.pop_front() {
            if self.procs[next].status == ProcStatus::Runnable {
                ecv_trace!(
                    sched,
                    "load next_idx={} next_pid={} (runq_remaining={})",
                    next,
                    self.procs[next].pid,
                    self.run_queue.len()
                );
                self.load_current(next);
                // ⚠️ THE KILL POINT. A task with a terminating default action due
                // must not execute one more guest instruction, and this is the
                // only place in the runtime that every about-to-run task passes
                // through -- `entry.rs` runs a leg for whoever is current, and
                // current is set here.
                //
                // It has to be AFTER `load_current`, not before: the group's
                // arena, fd table and disposition table are only live once the
                // switch has happened, and `exit_group_current` tears down the
                // LIVE ones. That is also what makes this reachable for a task
                // the signal woke out of an uninterruptible wait -- see
                // `wake_task_for_signal` -- whose syscall resume never runs.
                if let Some(sig) = self.pending_termination() {
                    ecv_debug!(
                        sched,
                        "pid={} sig={sig} default action TERM before its next leg",
                        self.procs[next].pid
                    );
                    self.exit_group_current(ExitReason::Killed(sig));
                    continue;
                }
                return SchedOutcome::Ready;
            }
        }
        if crate::diag::debug_log() {
            let snap: Vec<_> = self
                .procs
                .iter()
                .map(|p| (p.pid, p.status, p.blocked_on))
                .collect();
            ecv_debug!(sched, "IDLE (runq empty). procs: {:?}", snap);
        }
        // ⚠️ "Everything is blocked on a socket" is NOT terminal -- that was the
        // blocking-only bug's failure mode. A socket becomes ready from outside,
        // so the run can still progress and the caller must wait rather than
        // conclude anything.
        let io = self.has_socket_waiters();
        let wake_at = self.earliest_deadline();
        if io || wake_at.is_some() {
            return SchedOutcome::Idle { wake_at, io };
        }
        // ❗ A SIDE-LOAD WAITER IS NOT A DEADLOCK, and this is the branch that
        // actually decides it for a socket-free profile.
        //
        // The same guard exists further down, in the `WaitOutcome::TimedOut`
        // arm -- and that one is unreachable here. Getting there requires the
        // net backend's `wait` to be called, which requires a socket waiter,
        // which a guest parked on `dlopen` does not have. The `hosted` profile
        // is loopback-based and imports no socket function at all, so a parked
        // first dlopen lands HERE: nothing runnable, no io, no deadline, one
        // blocked process -- every condition of a deadlock, and not one.
        //
        // ⚠️ FOUND BY RUNNING IT, not by review. The guard below was written
        // with the park path and looked sufficient; the first real mid-run load
        // exited 111 with the host holding the very module the guest was
        // waiting for. `has_side_load_waiters` had a caller, a test, and a
        // correct implementation, and the one branch that needed it did not ask.
        //
        // Idling hands control back to the host, which is the only thing that
        // CAN resolve this: it calls `ecv_side_loaded` between slices. A host
        // that never delivers leaves the guest idle rather than killed, which is
        // the right failure -- the host can see it, and this cannot.
        if self.has_side_load_waiters() {
            return SchedOutcome::Idle { wake_at, io };
        }
        // Nothing runnable, nothing waiting on a host event, no deadline. If
        // processes still exist and are merely blocked, that is a DEADLOCK, not
        // completion -- exiting 0 here made every hang look like a clean run,
        // which is how a guest that stopped on its first `clock_nanosleep`
        // presented as "the program printed nothing".
        if self.procs.iter().any(|p| p.status == ProcStatus::Blocked) {
            return SchedOutcome::Deadlock;
        }
        SchedOutcome::Exited(self.exit_code)
    }

    /// Retires the suspended task, then selects the next one.
    pub fn schedule_after_suspend(&mut self) -> SchedOutcome {
        self.retire_after_suspend();
        self.resume_scheduling()
    }

    /// Selects, waiting in the BACKEND while it is willing to wait.
    ///
    /// ⚠️ ONE WAIT SERVES BOTH IDLE WAKE SOURCES. A socket becoming ready and a
    /// deadline expiring can each release the guest, and waiting on either alone
    /// starves the other: an unbounded socket poll sleeps through every guest
    /// timer, and sleeping for the earliest deadline first sleeps through an
    /// arriving connection for as long as the longest timer -- and nginx
    /// routinely asks for 60 s. So the deadline is passed to the backend AS the
    /// timeout, and a pure sleep is just that call with no waiters.
    ///
    /// That also collapses what used to be three separate blocking sites (a
    /// blocking `poll_oneoff`, a zero-timeout probe, and `std::thread::sleep`)
    /// into one place a backend can decline.
    pub fn resume_scheduling(&mut self) -> SchedOutcome {
        use crate::net::{NetBackend, WaitOutcome};
        loop {
            // ⚠️ SWEEP BEFORE DECIDING. A deadline that has already passed must
            // release its process no matter how control got here -- and after a
            // re-entrant host returns from an `Idle`, that is exactly the state:
            // it waited out the deadline itself and called back, so nothing else
            // is going to notice the timer. Sweeping only AFTER a wait works for
            // a blocking backend, which never leaves this function, and wedges a
            // re-entrant one into reporting `Idle` forever with a deadline in
            // the past.
            self.sweep_expired_deadlines();
            let outcome = self.pick_next();
            let SchedOutcome::Idle { wake_at, io } = outcome else {
                return outcome;
            };
            // Cap the wait so a guest asking for an absurd timeout cannot wedge
            // the module; it simply re-enters this path.
            let budget = wake_at.map(|d| d.saturating_sub(mono_nanos()).min(MAX_IDLE_SLEEP_NANOS));
            let mut waiters = self.socket_waiters();
            if !waiters.is_empty() {
                // ROTATE before waiting: N processes accepting on one listening
                // socket are N waiters on the SAME handle, the host reports them
                // all ready at once, and the first to run takes the connection.
                // Collected in plain index order that is the same worker every
                // time -- measured, 100 requests at 25-way concurrency all
                // served by one of four nginx workers, on both libcs.
                let rot = self.wake_cursor % waiters.len();
                waiters.rotate_left(rot);
            }
            let fds: Vec<(crate::net::NetHandle, bool)> =
                waiters.iter().map(|&(_, h, w)| (h, w)).collect();
            match self.net.wait(&fds, budget) {
                // The backend cannot block. Hand the decision to the host, which
                // is the whole point of the outcome enum.
                WaitOutcome::WouldBlock => {
                    // ⚠️ AN EPOLL SET IS WIDER THAN THE ONE HANDLE PARKED ON.
                    // `BlockedOn::Socket` carries a single handle, so a process
                    // epolling a listener AND its accepted connections is
                    // recorded against whichever came first in the set. A
                    // blocking backend does not care: it re-probes the host on
                    // every `wait`, so it sees readiness anywhere. A backend
                    // that cannot block answers from a cache, and readiness that
                    // lands on a handle nobody is parked on wakes nobody at all.
                    //
                    // That is a HANG, not a slowdown, and it is what nginx hit:
                    // it accepted a connection, added it to the epoll set, and
                    // parked -- recorded against the LISTENER. The request was
                    // sitting readable on the connection and the listener never
                    // became readable again, so nothing ever ran.
                    //
                    // Waking every socket waiter is safe by the argument used
                    // throughout this file: a woken process re-scans its own
                    // interest list and re-parks if it was wrong. The generation
                    // check is what makes it TERMINATE -- only a change in what
                    // the host has reported counts, so this fires once per
                    // notification instead of spinning on readiness that is
                    // simply still true.
                    let generation = self.net.ready_generation();
                    if generation != self.last_ready_gen {
                        self.last_ready_gen = generation;
                        // ⚠️ SET WAITERS ONLY. A process parked on one socket for
                        // one operation cannot survive a spurious wake -- see
                        // `BlockedOn::Socket`. A set waiter re-scans its whole
                        // interest list on resume, which is exactly what makes
                        // waking it on someone else's readiness correct.
                        let mut woke = false;
                        for &(proc_idx, _, _) in &waiters {
                            if matches!(
                                self.procs[proc_idx].blocked_on,
                                BlockedOn::Socket { poll: true, .. }
                            ) {
                                self.wake_socket(proc_idx);
                                woke = true;
                            }
                        }
                        if woke {
                            continue;
                        }
                    }
                    return SchedOutcome::Idle { wake_at, io };
                }
                WaitOutcome::Ready(ready) => {
                    if ready.len() > 1 {
                        self.wake_cursor = self.wake_cursor.wrapping_add(1);
                    }
                    let to_wake: Vec<usize> = ready
                        .iter()
                        .filter_map(|&i| waiters.get(i))
                        .map(|w| w.0)
                        .collect();
                    for proc_idx in to_wake {
                        self.wake_socket(proc_idx);
                    }
                }
                WaitOutcome::TimedOut => {
                    // ⚠️ A TIMEOUT WITH NO DEADLINE MEANS NOTHING CAN CHANGE.
                    //
                    // `wake_at` is None here, so the clock cannot release
                    // anyone and I/O is the only remaining wake source -- and
                    // the backend has just reported that it waited as long as it
                    // is able and nothing became ready. A blocking backend never
                    // reaches this (its `wait` does not return until something
                    // is ready), but an in-process one does, and looping would
                    // re-select the same state forever: a 100% CPU spin instead
                    // of a diagnosis. Report the deadlock the caller is for.
                    if wake_at.is_none() {
                        // ⚠️ UNLESS SOMEONE IS WAITING ON A HOST LOAD. A
                        // `SideLoad` waiter has no socket fd and no deadline, so
                        // every condition above reads exactly like a deadlock --
                        // and it is not one: the wake comes from the host
                        // calling `ecv_side_loaded` between slices, the same way
                        // `ecv_net_ready` delivers readiness. Idling hands
                        // control back so the host CAN do that; reporting
                        // deadlock would kill the module while the thing it
                        // waits for was on its way.
                        //
                        // Reachable only under a backend that returns `Pending`,
                        // which by construction means a host exists to be handed
                        // back to -- `preloaded` never does.
                        if self.has_side_load_waiters() {
                            return SchedOutcome::Idle { wake_at, io };
                        }
                        return SchedOutcome::Deadlock;
                    }
                }
            }
            self.sweep_expired_deadlines();
        }
    }

    /// Reports a deadlock: every process blocked with nothing that can wake
    /// them. Diagnostic only -- the caller decides what to do about it.
    ///
    /// Split out of the scheduler when the exit moved to `entry.rs`. It stays
    /// here because it reads the process table and the arena, and because the
    /// opt-in heap scan below is the tool for the question a deadlock actually
    /// raises: "was this structure ever initialised, or is the guest holding the
    /// wrong pointer?"
    pub fn report_deadlock(&self) {
        let stuck: Vec<_> = self
            .procs
            .iter()
            .filter(|p| p.status == ProcStatus::Blocked)
            .map(|p| (p.pid, p.blocked_on))
            .collect();
        ecv_warn!(
            ecvisor,
            "deadlock: every process is blocked and nothing can wake them: {stuck:?}"
        );
        // Opt-in heap scan. A deadlock on a data structure is usually a
        // question of "was this thing ever initialised, or is the guest
        // holding the wrong pointer", and that is answerable by looking
        // for a correctly-initialised instance elsewhere in the heap.
        // RAPTORMARK_ECV_SCAN=off:min:max[,off:min:max...] reports every
        // 8-byte-aligned address in the mmap arena satisfying ALL
        // clauses, each testing the u32 at `off` against [min,max].
        //
        // A conjunction is the point: one field matching a small integer
        // is what arbitrary heap data looks like, so a single clause
        // produces only noise. Identifying a struct needs several fields
        // agreeing at one base. Diagnostic only.
        if crate::diag::count_ret() != 0 {
            ecv_warn!(
                count,
                "call site ret={:#x} executed {} time(s)",
                crate::diag::count_ret(),
                crate::diag::ret_hits()
            );
        }
        //
        // The spec is parsed by `diag` at STARTUP, not read here: this
        // reporter runs only once every process is blocked, so at least
        // one fork has happened, and `std::env::var` on a post-fork path
        // is the `lazy_lock::panic_poisoned` hazard `diag` exists to
        // prevent. It would have bitten only a run with the scan armed --
        // that is, a run already chasing a hang.
        let clauses = crate::diag::scan_clauses();
        if !clauses.is_empty() {
            let span = clauses.iter().map(|c| c.0).max().unwrap_or(0) + 4;
            let mut hits = 0;
            let mut a = crate::arena::MMAP_START_VMA;
            while a + span < self.arena.mmap_cur {
                let at =
                    |off: u64| u32::from_le_bytes(self.arena.slice(a + off, 4).try_into().unwrap());
                if clauses.iter().all(|&(o, lo, hi)| {
                    let w = at(o);
                    w >= lo && w <= hi
                }) {
                    let fields: Vec<String> = clauses
                        .iter()
                        .map(|&(o, _, _)| format!("+{o}={}", at(o)))
                        .collect();
                    ecv_warn!(scan, "{a:#x}  {}", fields.join(" "));
                    hits += 1;
                    if hits >= 40 {
                        ecv_warn!(scan, "(truncated)");
                        break;
                    }
                }
                a += 8;
            }
            ecv_warn!(
                scan,
                "{hits} address(es) satisfying all {} clause(s)",
                clauses.len()
            );
        }
    }

    /// Collects (proc_idx, handle, want_write) for every process blocked on a
    /// socket. Pure table scan (host-safe).
    fn socket_waiters(&self) -> Vec<(usize, crate::net::NetHandle, bool)> {
        let mut v = Vec::new();
        for i in 0..self.procs.len() {
            if self.procs[i].status == ProcStatus::Blocked {
                if let BlockedOn::Socket { h, write, .. } = self.procs[i].blocked_on {
                    v.push((i, h, write));
                }
            }
        }
        v
    }

    /// True if any process is parked on a socket (its readiness comes from the
    /// host, so the scheduler's idle `poll_sockets_and_wake` can still make the
    /// module progress even when no guest process is Runnable). Used by
    /// `epoll_pwait` to decide whether a last-runnable timeout wait may safely
    /// block (and let the idle poll service the socket) rather than spin-returning.
    pub fn has_socket_waiters(&self) -> bool {
        self.procs.iter().any(|p| {
            p.status == ProcStatus::Blocked && matches!(p.blocked_on, BlockedOn::Socket { .. })
        })
    }

    /// True if any process is parked waiting for the host to load a unit.
    ///
    /// The counterpart of `has_socket_waiters`, and it exists for a sharper
    /// reason: a socket waiter at least has an fd the idle poll can watch, so
    /// the scheduler has something to do. A side-load waiter has NOTHING --
    /// no fd, no deadline -- so without this it is indistinguishable from a
    /// deadlocked process, and the module would be killed while waiting for a
    /// load the host was about to deliver.
    pub fn has_side_load_waiters(&self) -> bool {
        self.procs.iter().any(|p| {
            p.status == ProcStatus::Blocked && matches!(p.blocked_on, BlockedOn::SideLoad { .. })
        })
    }

    /// Wakes every OTHER process parked on `host_fd`, in rotated order.
    ///
    /// Called when a running process observes that host fd to be ready. On Linux
    /// the kernel wakes every waiter on a level-triggered descriptor; here,
    /// readiness is discovered privately by whoever happens to call `epoll_pwait`,
    /// and `poll_sockets_and_wake` only runs when NOTHING is runnable. A worker
    /// that always finds work therefore never lets the scheduler idle, and the
    /// other waiters are never reconsidered at all.
    ///
    /// That is what pinned nginx to a single worker, and it is not the race it
    /// looked like. Under load, with four workers and 20 requests, the traced
    /// `accept4` counts were 20 / 1 / 0 / 0 -- two workers never entered the race
    /// even once. Rotating the wake order (both here and in
    /// `poll_sockets_and_wake`) cannot fix that on its own: an order only matters
    /// among processes that are actually woken.
    ///
    /// Over-waking is safe by the same argument as `wake_pollers`: a woken process
    /// re-scans its interest list and re-blocks if nothing is ready. And this only
    /// fires when the fd IS ready, so at least one of them can make progress.
    pub fn wake_socket_waiters_on(&mut self, h: crate::net::NetHandle) {
        let n = self.procs.len();
        if n == 0 {
            return;
        }
        let start = self.wake_cursor % n;
        let mut woken = 0usize;
        for k in 0..n {
            let i = (start + k) % n;
            if i == self.current || self.procs[i].status != ProcStatus::Blocked {
                continue;
            }
            if matches!(self.procs[i].blocked_on, BlockedOn::Socket { h: x, .. } if x == h) {
                self.make_runnable(i);
                woken += 1;
            }
        }
        if woken > 0 {
            self.wake_cursor = self.wake_cursor.wrapping_add(1);
        }
    }

    /// Marks a socket-blocked process Runnable and queues it. Wasm-only caller.
    #[cfg_attr(not(target_arch = "wasm32"), allow(dead_code))]
    fn wake_socket(&mut self, proc_idx: usize) {
        if self.procs[proc_idx].status == ProcStatus::Blocked {
            self.make_runnable(proc_idx);
        }
    }

    // `poll_sockets_and_wake` lived here. Its body -- rotate the waiters, bound
    // the wait by the earliest deadline, wake by userdata index -- moved into
    // `resume_scheduling`, where it is one arm of a single wait that also covers
    // the clock. It used to need a wasm impl and a `false`-returning host stub;
    // going through the backend seam removed the need for both.

    /// Allocates the lowest free descriptor >= 3.
    pub fn alloc_fd(&mut self, f: OpenFile) -> i32 {
        let idx = self
            .fds
            .iter()
            .enumerate()
            .skip(3)
            .find(|(_, s)| s.is_none())
            .map(|(i, _)| i);
        let i = match idx {
            Some(i) => {
                self.fds[i] = Some(f);
                i
            }
            None => {
                self.fds.push(Some(f));
                self.fds.len() - 1
            }
        };
        self.set_cloexec(i, false); // a freshly allocated fd is not close-on-exec
        self.set_nonblock(i, false);
        i as i32
    }

    /// Sets fd `fd`'s close-on-exec flag, growing the parallel table as needed.
    pub fn set_cloexec(&mut self, fd: usize, on: bool) {
        if fd >= self.cloexec.len() {
            self.cloexec.resize(fd + 1, false);
        }
        self.cloexec[fd] = on;
    }

    /// Sets fd `fd`'s O_NONBLOCK flag, growing the parallel table as needed.
    pub fn set_nonblock(&mut self, fd: usize, on: bool) {
        if fd >= self.nonblock.len() {
            self.nonblock.resize(fd + 1, false);
        }
        self.nonblock[fd] = on;
    }

    /// Reports whether fd `fd` is marked non-blocking (absent => false).
    pub fn is_nonblock(&self, fd: usize) -> bool {
        self.nonblock.get(fd).copied().unwrap_or(false)
    }

    /// Reports whether fd `fd` is marked close-on-exec (absent => false).
    pub fn is_cloexec(&self, fd: usize) -> bool {
        self.cloexec.get(fd).copied().unwrap_or(false)
    }

    /// Closes every fd marked close-on-exec (POSIX execve semantics), applying pipe
    /// refcounting. Non-cloexec fds survive the execve.
    fn close_cloexec_fds(&mut self) {
        for fd in 0..self.fds.len() {
            if self.is_cloexec(fd) && self.fds.get(fd).map_or(false, Option::is_some) {
                // `close_fd_full` lives in `sys` because closing a SOCKET fd has
                // to go through the WASI import; nothing else about this loop is
                // target-specific. The host build exists only to make the crate's
                // unit tests reachable and never runs execve, so dropping the
                // table entry keeps the bookkeeping consistent there. Anything
                // that starts testing execve on the host must revisit this.
                #[cfg(target_arch = "wasm32")]
                crate::sys::close_fd_full(self, fd);
                #[cfg(not(target_arch = "wasm32"))]
                {
                    self.fds[fd] = None;
                }
                self.set_cloexec(fd, false);
            }
        }
    }

    /// Sets up the guest's static TLS at the thread pointer `tp`: for EVERY
    /// PT_TLS module in the fused image it copies that module's `.tdata` template
    /// to its TP-relative block and zeroes its `.tbss` tail, driven by the
    /// prelinker's per-module `.ecv.tls` descriptor table (one 32-byte entry per
    /// module: `{template_vaddr, filesz, memsz, tp_offset}`, all little-endian
    /// u64). This matters when more than one object carries `__thread` state —
    /// e.g. a program that itself defines a `__thread` variable AND links glibc,
    /// where the executable's block and libc's block are distinct modules; a
    /// single-module setup would leave the executable's initializer uncopied.
    ///
    /// Falls back to the single advertised PT_TLS (`init_static_tls`) if no
    /// `.ecv.tls` section is present. Runs once, before the guest's first
    /// instruction and after `load_data_sections` has staged every `.tdata`
    /// template into the arena.
    pub unsafe fn setup_tls(&mut self, tp: u64) {
        let prog = self.programs.get(self.procs[self.current].prog_idx);
        if let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.tls") {
            self.arena.tls_zero_tcb(tp);
            let count = size / 32;
            for i in 0..count {
                let e = bytes.add(i * 32);
                let template = core::ptr::read_unaligned(e as *const u64);
                let filesz = core::ptr::read_unaligned(e.add(8) as *const u64);
                let memsz = core::ptr::read_unaligned(e.add(16) as *const u64);
                let tp_offset = core::ptr::read_unaligned(e.add(24) as *const u64);
                self.arena
                    .init_tls_module(template, filesz, memsz, tp_offset, tp);
            }
            return;
        }
        // No per-module table: single advertised PT_TLS (the pre-multi-module path).
        if let Some((vaddr, filesz, memsz, align)) = prog.tls_phdr() {
            self.arena.init_static_tls(vaddr, filesz, memsz, align, tp);
        }
    }

    /// Copies every static TLS module template to a NEW thread's block at `tp`.
    ///
    /// The module half of `setup_tls`, and deliberately not the TCB half: see
    /// the call site in `sys_clone`. Both libcs need it for a different reason
    /// -- glibc because `_dl_allocate_tls_init` has no slotinfo list to walk in
    /// a fused image, musl because it is cheap insurance that the child's block
    /// holds the same bytes the initial thread's does. Re-copying an identical
    /// template is idempotent.
    ///
    /// No `.ecv.tls` (an image with no thread-locals) -> nothing to do.
    pub unsafe fn init_thread_tls_modules(&mut self, tp: u64) {
        let prog = self.programs.get(self.procs[self.current].prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.tls") else {
            return;
        };
        for i in 0..size / 32 {
            let e = bytes.add(i * 32);
            let template = core::ptr::read_unaligned(e as *const u64);
            let filesz = core::ptr::read_unaligned(e.add(8) as *const u64);
            let memsz = core::ptr::read_unaligned(e.add(16) as *const u64);
            let tp_offset = core::ptr::read_unaligned(e.add(24) as *const u64);
            // A thread pointer the guest computed from half-initialised state
            // can be anywhere; refuse rather than scribble.
            if !self.arena.in_bounds(tp + tp_offset, memsz as usize) {
                ecv_debug!(
                    ecvisor,
                    "thread TLS init SKIPPED: tp 0x{tp:x} + 0x{tp_offset:x} \
                     ({memsz} bytes) is outside the arena"
                );
                return;
            }
            self.arena
                .init_tls_module(template, filesz, memsz, tp_offset, tp);
        }
    }

    /// Applies the load-time ifunc GOT slots the prelinker recorded in the
    /// `.ecv.irela` data section (IRELATIVE relocs plus JUMP_SLOT/GLOB_DAT/ABS64
    /// bound to STT_GNU_IFUNC symbols, e.g. a program calling glibc's ifunc'd
    /// memset/memcpy/strlen). For each slot it runs the resolver as a guest
    /// function and writes the returned implementation VMA into the GOT slot, so
    /// the guest's later indirect dispatch through that slot lands on the real
    /// implementation. Runs once, before the guest's first instruction.
    ///
    /// `base` is the guest's initial State, cloned for each resolver call so the
    /// live register set is left untouched. Must run after the dispatch tables
    /// are built (the resolvers and their implementations are lifted functions).
    pub unsafe fn apply_ifuncs(&mut self, arena_ptr: *mut u8, base: *const State) {
        let cur = self.procs[self.current].prog_idx;
        self.apply_ifuncs_in(cur, arena_ptr, base);
    }

    /// The same, for a NAMED unit -- what `dlopen` runs for a freshly loaded
    /// plugin. A unit's `.ecv.irela` describes only its own GOT slots
    /// (`internal/fuse.ifuncsWithin`), so this fills exactly that unit's.
    ///
    /// # Safety
    ///
    /// Same as `apply_ifuncs`: the dispatch tables must already contain the
    /// unit's functions, because a resolver IS one of them.
    pub unsafe fn apply_ifuncs_in(
        &mut self,
        prog_idx: usize,
        arena_ptr: *mut u8,
        base: *const State,
    ) {
        if prog_idx >= self.programs.len() {
            return;
        }
        let prog = self.programs.get(prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.irela") else {
            return;
        };
        let count = size / 16;
        for i in 0..count {
            let e = bytes.add(i * 16);
            let got_slot = core::ptr::read_unaligned(e as *const u64);
            let resolver = core::ptr::read_unaligned(e.add(8) as *const u64);
            match self.run_ifunc_resolver(arena_ptr, base, resolver) {
                Some(impl_vma) => {
                    self.arena
                        .slice_mut(got_slot, 8)
                        .copy_from_slice(&impl_vma.to_le_bytes());
                    ecv_warn!(ecvisor, "ifunc: resolver 0x{resolver:x} -> impl 0x{impl_vma:x}; GOT slot 0x{got_slot:x} filled"
                    );
                }
                None => fatal!(
                    "ifunc resolver 0x{resolver:x} (GOT slot 0x{got_slot:x}) not in the lifted function table"
                ),
            }
        }
    }

    /// Runs glibc's `__libc_early_init` at bring-up, if the prelinker emitted its
    /// fused VMA to `.ecv.early`. For a dynamic binary the loader invokes this hook
    /// for libc EARLY (via `_dl_call_libc_early_init`), before init_arrays and main;
    /// it runs `__ctype_init`, which populates the main thread's ctype tsd so
    /// `isalpha`/`is_name` work. Our prelink+trampoline entry bypasses ld.so, so we
    /// call it here. No `.ecv.early` section (the closure has no glibc) -> no-op.
    ///
    /// Invoked with the aarch64 ABI x0 = initial(1) on the ifunc scratch stack, with
    /// a State cloned from `base` so `tpidr_el0` = THREAD_PTR and the ctype writes
    /// land in the live TLS block. It runs to completion (pure memory setup, no
    /// syscalls), exactly like an ifunc resolver, so no asyncify suspension arises.
    pub unsafe fn apply_early_init(&mut self, arena_ptr: *mut u8, base: *const State) {
        let prog = self.programs.get(self.procs[self.current].prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.early") else {
            return;
        };
        if size < 8 {
            return;
        }
        let target = core::ptr::read_unaligned(bytes as *const u64);
        if target == 0 {
            return;
        }
        let Some(f) = self.func_at(target) else {
            fatal!("__libc_early_init 0x{target:x} (.ecv.early) not in the lifted function table");
        };
        let mut st = State::new_boxed();
        core::ptr::copy_nonoverlapping(base, &mut *st, 1);
        st.gpr.x[0].val = 1; // __libc_early_init(_Bool initial = true)
        st.gpr.sp.val = crate::arena::IFUNC_STACK_TOP;
        st.gpr.pc.val = target;
        let ctx_ptr = self as *mut EcvContext;
        f(arena_ptr, &mut *st, target, ctx_ptr);
        ecv_warn!(
            ecvisor,
            "__libc_early_init 0x{target:x} ran (main-thread ctype/locale init)"
        );
    }

    /// Initializes glibc's `_rtld_global` thread-stack list heads at bring-up, if
    /// the prelinker emitted `.ecv.stacklists`. The dynamic loader's pthread
    /// bring-up (`__pthread_initialize_minimal` / `__tls_init_tp`) normally sets
    /// `_dl_stack_used`/`_dl_stack_cache` to self-referential-empty and links the
    /// main thread's `struct pthread` onto `_dl_stack_user`; our prelink+trampoline
    /// entry bypasses ld.so, and that init function is internal (not exported), so
    /// we cannot call it like `__libc_early_init`. Instead the prelinker derived the
    /// version-specific struct offsets from glibc's `_thread_db_*` metadata and we
    /// write the eight list-head pointers ourselves. Without this, glibc's
    /// `__libc_fork` child cleanup walks a null `_dl_stack_used` and loops forever.
    ///
    /// Pure memory init (no guest execution). No `.ecv.stacklists` section (the
    /// closure has no glibc) -> no-op. `_arena_ptr`/`_base` are unused (the writes
    /// go through `self.arena`) but kept to match the sibling bring-up hooks.
    pub unsafe fn apply_stacklists(&mut self, arena_ptr: *mut u8, base: *const State) {
        let prog = self.programs.get(self.procs[self.current].prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.stacklists") else {
            return;
        };
        if size < 48 {
            return;
        }
        let rd = |i: usize| core::ptr::read_unaligned(bytes.add(i * 8) as *const u64);
        let rtld = rd(0);
        if rtld == 0 {
            return;
        }
        let used_off = rd(1);
        let user_off = rd(2);
        let cache_off = rd(3);
        let sz_pthread = rd(4);
        let plist_off = rd(5);
        // The main thread's `struct pthread` sits just below the thread pointer
        // (aarch64: pthread_self() == tp - sizeof(pthread)); `list` is at plist_off
        // within it. This lands in unused, zeroed low arena, safe to write.
        let selflist = crate::arena::THREAD_PTR - sz_pthread + plist_off;
        let mut wr = |addr: u64, val: u64| {
            self.arena
                .slice_mut(addr, 8)
                .copy_from_slice(&val.to_le_bytes());
        };
        // _dl_stack_used / _dl_stack_cache: empty, self-referential (next=prev=&head).
        wr(rtld + used_off, rtld + used_off);
        wr(rtld + used_off + 8, rtld + used_off);
        wr(rtld + cache_off, rtld + cache_off);
        wr(rtld + cache_off + 8, rtld + cache_off);
        // _dl_stack_user: holds the main thread (head <-> self->list).
        wr(rtld + user_off, selflist);
        wr(rtld + user_off + 8, selflist);
        wr(selflist, rtld + user_off);
        wr(selflist + 8, rtld + user_off);
        ecv_warn!(ecvisor, "_dl_stack_* bring-up: rtld=0x{rtld:x} used=0x{:x} user=0x{:x} cache=0x{:x} self->list=0x{selflist:x}",
            rtld + used_off,
            rtld + user_off,
            rtld + cache_off,
        );
        // rtld recursive-lock fn ptrs (words 6,7): ld.so's bootstrap points the
        // lock/unlock pair at its no-op default lock; we bypass that bootstrap, so
        // set them here or dlopen dispatches through a NULL fn ptr. Present only when
        // the prelinker emitted the extended (64-byte) section.
        if size >= 64 {
            let lock_slot = rd(6);
            let noop_lock = rd(7);
            if lock_slot != 0 && noop_lock != 0 {
                wr(lock_slot, noop_lock);
                wr(lock_slot + 8, noop_lock);
                ecv_warn!(
                    ecvisor,
                    "rtld lock-fn-ptr bring-up: slot=0x{lock_slot:x} -> noop=0x{noop_lock:x}"
                );
            }
        }
        // Seed the main thread's `pthread->tid` (word 8 = offsetof(struct pthread,
        // tid), from thread_db). ld.so's pthread bring-up (__tls_init_tp ->
        // set_tid_address(&pd->tid)) that our trampoline entry bypasses normally
        // sets it; left 0, glibc's pthread_rwlock_rdlock sees __cur_writer(0)==tid(0)
        // and returns EDEADLK on every fresh read-lock, which breaks OpenSSL's
        // default OSSL_LIB_CTX property-string init (and thus all provider crypto).
        // tid == gettid() == the process pid in our single-thread-per-process model.
        if size >= 72 {
            let tid_off = rd(8);
            let tid = self.procs[self.current].pid;
            let addr = crate::arena::THREAD_PTR - sz_pthread + tid_off;
            self.arena
                .slice_mut(addr, 4)
                .copy_from_slice(&tid.to_le_bytes());
            ecv_warn!(ecvisor, "pthread->tid bring-up: 0x{addr:x} = {tid}");
        }
        // Word 9: `_rtld_global_ro._dl_minsigstacksize`. ld.so takes it from
        // AT_MINSIGSTKSZ and falls back to the architecture's MINSIGSTKSZ; a
        // fused image runs neither path, and `sysconf(_SC_MINSIGSTKSZ)` asserts
        // on zero and aborts. python:3-slim dies there, after every constructor
        // has already run.
        if size >= 80 {
            let addr = rd(9);
            match seed_dl_minsigstacksize(&mut self.arena, addr) {
                Some(v) => ecv_warn!(ecvisor, "_dl_minsigstacksize bring-up: 0x{addr:x} = {v}"),
                None if addr != 0 => {
                    ecv_debug!(ecvisor, "_dl_minsigstacksize: refused 0x{addr:x}")
                }
                None => {}
            }
        }
        // Words 10 and 11: `_rtld_global_ro._dl_tls_static_size` and
        // `._dl_tls_static_align`. `allocate_stack` computes
        // `size &= ~(GLRO(dl_tls_static_align) - 1)`, so an unset align makes
        // that mask ALL-ONES and every thread stack size becomes zero:
        //
        //   Fatal glibc error: allocatestack.c:335 (allocate_stack):
        //   assertion failed: size != 0
        //
        // A fused dynamic glibc image cannot create a thread without this. It
        // is the glibc counterpart of musl's `libc.tls_size`/`tls_align`, and
        // like those it is filled from the image's OWN TLS geometry rather than
        // from a constant, so a guest's threads get the layout its code was
        // compiled for.
        if size >= 96 {
            let (size_addr, align_addr) = (rd(10), rd(11));
            if size_addr != 0 && align_addr != 0 {
                let mods = self.musl_tls_modules();
                let (tls_size, tls_align) = glibc_tls_static_geometry(&mods);
                // Only zero fields are written: a non-zero one means something
                // already initialised this and must not be overruled.
                let live = |a: &Arena, at: u64| -> u64 {
                    u64::from_le_bytes(a.slice(at, 8).try_into().unwrap())
                };
                if self.arena.in_bounds(size_addr, 8)
                    && self.arena.in_bounds(align_addr, 8)
                    && live(&self.arena, size_addr) == 0
                    && live(&self.arena, align_addr) == 0
                {
                    self.arena
                        .slice_mut(size_addr, 8)
                        .copy_from_slice(&tls_size.to_le_bytes());
                    self.arena
                        .slice_mut(align_addr, 8)
                        .copy_from_slice(&tls_align.to_le_bytes());
                    ecv_warn!(
                        ecvisor,
                        "_dl_tls_static bring-up: size={tls_size} align={tls_align} \
                         (0x{size_addr:x}, 0x{align_addr:x})"
                    );
                }
            }
        }
        // Word 12: ld.so's hook installer, CALLED rather than read. It fills the
        // cluster of indirect-call pointers ld.so keeps in its own data --
        // `__rtld_malloc` and family -- which a fused image leaves NULL, so the
        // first caller dies with `vma 0 not in the lifted function table`.
        // `pthread_create` is that caller: `_dl_allocate_tls_storage` allocates
        // the new thread's TLS through them.
        //
        // Calling ld.so's own installer rather than writing the slots is the
        // whole point -- see `internal/fuse.rtldHookInitVMA`. The slots are
        // hidden and unnamed, so choosing which is malloc and which is free
        // would be guesswork with silent corruption as the failure mode.
        // Words 12..15, zero-padded: there is more than ONE installer -- the
        // malloc family and the lock pair have their own -- and a missing one
        // is not a degraded mode. Installing only the first is what left
        // `_dl_allocate_tls_init` calling a null lock pointer immediately after
        // the allocator hooks started working.
        for i in 12..16u64 {
            if size < ((i + 1) * 8) as usize {
                break;
            }
            let init = rd(i as usize);
            if init == 0 {
                continue;
            }
            if let Some(f) = self.func_at(init) {
                let mut st = State::new_boxed();
                core::ptr::copy_nonoverlapping(base, &mut *st, 1);
                st.gpr.sp.val = crate::arena::IFUNC_STACK_TOP;
                st.gpr.pc.val = init;
                let ctx_ptr = self as *mut EcvContext;
                f(arena_ptr, &mut *st, init, ctx_ptr);
                ecv_warn!(ecvisor, "ld.so hook installer ran: 0x{init:x}");
            } else {
                ecv_debug!(ecvisor, "ld.so hook installer 0x{init:x} is not lifted");
            }
        }
    }

    /// Seeds musl's `pthread->tid` at bring-up, if the prelinker emitted
    /// `.ecv.musltp`. The musl counterpart of `apply_stacklists`' word 8.
    ///
    /// For a dynamic musl program the loader runs `__init_tp`, which issues
    /// `set_tid_address` and stores the result in `pthread->tid`. Our
    /// prelink+trampoline entry starts at the executable's `_start` and never
    /// runs ld.so, so the tid stays zero -- and musl's `exit` reads it, sees
    /// zero, and branches to a deliberate crash path (`strb wzr,[x0=0]; brk`).
    /// nginx died there after printing its version.
    ///
    /// The offsets come from `.ecv.musltp`, which the prelinker derives from
    /// musl's own exported accessors (`gettid`, `pthread_self`) rather than
    /// from a hardcoded struct layout -- musl publishes no thread_db metadata.
    /// Both are TP-relative distances to subtract. Pure memory init, no guest
    /// execution. No section (a glibc closure, or a musl too old to export
    /// these) -> no-op.
    ///
    /// A third word, present only in sections the current prelinker emits, is
    /// the absolute address of `libc.can_do_threads` -- the other field
    /// `__init_tp` sets, and the one that gates `pthread_create` entirely.
    pub unsafe fn apply_musl_tp(&mut self, _arena_ptr: *mut u8, _base: *const State) {
        let prog = self.programs.get(self.procs[self.current].prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.musltp") else {
            return;
        };
        if size < 16 {
            return;
        }
        let base_off = core::ptr::read_unaligned(bytes as *const u64);
        let tid_off = core::ptr::read_unaligned(bytes.add(8) as *const u64);
        if tid_off == 0 || tid_off >= crate::arena::THREAD_PTR {
            return;
        }
        let tid = self.procs[self.current].pid;
        let addr = crate::arena::THREAD_PTR - tid_off;
        self.arena
            .slice_mut(addr, 4)
            .copy_from_slice(&tid.to_le_bytes());
        ecv_warn!(
            ecvisor,
            "musl pthread->tid bring-up: 0x{addr:x} = {tid} (struct base TP-0x{base_off:x})"
        );
        if base_off != 0 && base_off < crate::arena::THREAD_PTR {
            let base = crate::arena::THREAD_PTR - base_off;
            if seed_musl_thread_list(&mut self.arena, base) {
                ecv_warn!(
                    ecvisor,
                    "musl thread-list bring-up: self=prev=next=0x{base:x}"
                );
            }
        }
        // Third word, optional: `libc.can_do_threads`. `__init_tp` sets it, and
        // `__init_tp` runs inside ld.so -- so in a fused image it stays zero and
        // `pthread_create` returns ENOSYS before issuing any syscall at all.
        // That is the whole of redis:7-alpine's "Can't initialize Background
        // Jobs", and it presents as a thread bug in the runtime when nothing in
        // the runtime was ever reached.
        //
        // Only a zero byte is written to: a non-zero one means musl's own
        // bring-up did run, and this must not overrule it.
        if size >= 24 {
            let flag = core::ptr::read_unaligned(bytes.add(16) as *const u64);
            if flag != 0 && flag < crate::arena::MEMORY_ARENA_SIZE as u64 {
                let cur = self.arena.slice(flag, 1)[0];
                if cur == 0 {
                    self.arena.slice_mut(flag, 1)[0] = 1;
                    ecv_warn!(ecvisor, "musl can_do_threads bring-up: 0x{flag:x} = 1");
                }
                // `can_do_threads` is field 0 of `struct __libc`, so its address
                // IS `&__libc`. The fourth word is the offset of `tls_size`,
                // decoded from `pthread_create`'s own loads; without it the
                // layout is unconfirmed and nothing is seeded.
                let tls_size_off = if size >= 32 {
                    core::ptr::read_unaligned(bytes.add(24) as *const u64)
                } else {
                    0
                };
                self.seed_musl_tls(flag, base_off, tls_size_off);
            }
        }
    }

    /// Seeds musl's `libc.tls_head` / `tls_size` / `tls_align` / `tls_cnt`, the
    /// fields `__init_tls` would have written from inside ld.so.
    ///
    /// Without them `__copy_tls` sizes a new thread's TLS block relative to
    /// NULL and the first `pthread_create` never returns -- measured, before
    /// this existed, as `range end index 4294967140 out of range` (that is -156
    /// truncated to 32 bits) from a fused dynamic musl guest. It is the same
    /// class of gap as the tid seed and `can_do_threads` above: state ld.so
    /// owns, in an image that never runs ld.so.
    ///
    /// `libc_addr` is `&__libc`; `pthread_size` is the part of `struct pthread`
    /// below the thread pointer (`.ecv.musltp` word 1).
    ///
    /// THE FIELD OFFSETS are not a version table, and they are not confirmed
    /// HERE either -- there is nothing here to confirm them against. This runs
    /// before the guest's first instruction, when every field of
    /// `struct __libc` is still zero, because `__init_libc` runs inside the
    /// guest's own `_start`. A first version tested `libc.page_size` and always
    /// refused, correctly: `__libc+48 = 0x0 is not a page size`.
    ///
    /// The confirmation comes from the IMAGE'S CODE instead, decoded by the
    /// prelinker (`internal/fuse.muslLibcTLSOffset`) and handed over as
    /// `tls_size_off`: within `pthread_create` the 64-bit loads off `&__libc`
    /// must be exactly {24, 48} -- `tls_size` and `page_size` -- which pins
    /// musl's declared field order, so `tls_head` at -8, `tls_align` at +8 and
    /// `tls_cnt` at +16 from it follow from the same struct. Zero means the
    /// prelinker could not confirm the layout and nothing is written: a wrong
    /// guess corrupts arbitrary libc state.
    unsafe fn seed_musl_tls(&mut self, libc_addr: u64, pthread_size: u64, tls_size_off: u64) {
        if tls_size_off == 0 {
            return;
        }
        // Relative to the one offset the prelinker confirmed, in musl's
        // declared order: ... tls_head, tls_size, tls_align, tls_cnt ...
        let Some(off_head) = tls_size_off.checked_sub(8) else {
            return;
        };
        let (off_size, off_align, off_cnt) = (tls_size_off, tls_size_off + 8, tls_size_off + 16);
        if !self.arena.in_bounds(libc_addr, (off_cnt + 8) as usize) {
            return;
        }
        let cur_size = u64::from_le_bytes(
            self.arena
                .slice(libc_addr + off_size, 8)
                .try_into()
                .unwrap(),
        );
        if cur_size != 0 {
            // musl's own bring-up ran; it owns these fields.
            return;
        }

        let mods = self.musl_tls_modules();
        let bytes = (mods.len() as u64) * MUSL_TLS_MODULE_SIZE;
        if bytes > crate::arena::MUSL_TLS_MODULES_MAX {
            ecv_warn!(ecvisor, "musl TLS seed SKIPPED: {} modules need {bytes} bytes, more than the                  {} reserved below the thread pointer",
                mods.len(),
                crate::arena::MUSL_TLS_MODULES_MAX
            );
            return;
        }
        let (tls_size, tls_align, tls_cnt) = musl_tls_geometry(&mods, pthread_size);

        // The list itself, one 48-byte record per module, chained in order.
        let base = crate::arena::MUSL_TLS_MODULES_VMA;
        for (i, m) in mods.iter().enumerate() {
            let at = base + i as u64 * MUSL_TLS_MODULE_SIZE;
            let next = if i + 1 < mods.len() {
                at + MUSL_TLS_MODULE_SIZE
            } else {
                0
            };
            let rec = [next, m.image, m.len, m.size, m.align, m.offset];
            for (j, v) in rec.iter().enumerate() {
                self.arena
                    .slice_mut(at + j as u64 * 8, 8)
                    .copy_from_slice(&v.to_le_bytes());
            }
        }
        let head = if mods.is_empty() { 0 } else { base };
        for (off, v) in [
            (off_head, head),
            (off_size, tls_size),
            (off_align, tls_align),
            (off_cnt, tls_cnt),
        ] {
            self.arena
                .slice_mut(libc_addr + off, 8)
                .copy_from_slice(&v.to_le_bytes());
        }
        ecv_warn!(ecvisor, "musl TLS seed: {tls_cnt} module(s) at 0x{head:x}, tls_size={tls_size} tls_align={tls_align} (tls_size at __libc+{tls_size_off}, decoded from pthread_create)"
        );
    }

    /// The TLS modules of the running program, from `.ecv.tls` (geometry) and
    /// `.ecv.tlsalign` (per-module p_align), which the prelinker emits in the
    /// same order.
    ///
    /// A fused image built before `.ecv.tlsalign` existed has the geometry and
    /// no alignments; each module then reports 1, which is what an ELF with no
    /// declared p_align means. `musl_tls_geometry` floors the aggregate at the
    /// TCB size regardless, so an old image seeds a working -- if
    /// conservatively aligned -- list rather than nothing.
    fn musl_tls_modules(&self) -> Vec<MuslTlsModule> {
        let prog = self.programs.get(self.procs[self.current].prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.tls") else {
            return Vec::new();
        };
        let aligns = prog.find_data_section(b".ecv.tlsalign");
        let count = size / 32;
        let mut out = Vec::with_capacity(count);
        for i in 0..count {
            let e = unsafe { bytes.add(i * 32) };
            let align = match aligns {
                Some((_, asize, abytes)) if (i + 1) * 8 <= asize => unsafe {
                    core::ptr::read_unaligned(abytes.add(i * 8) as *const u64)
                },
                _ => 1,
            };
            out.push(MuslTlsModule {
                image: unsafe { core::ptr::read_unaligned(e as *const u64) },
                len: unsafe { core::ptr::read_unaligned(e.add(8) as *const u64) },
                size: unsafe { core::ptr::read_unaligned(e.add(16) as *const u64) },
                align: align.max(1),
                offset: unsafe { core::ptr::read_unaligned(e.add(24) as *const u64) },
            });
        }
        out
    }

    /// Gives glibc a default thread stack size, by calling glibc's OWN API.
    ///
    /// Without it a fused DYNAMIC glibc image cannot create a thread at all:
    ///
    /// ```text
    /// Fatal glibc error: allocatestack.c:335 (allocate_stack): assertion failed: size != 0
    /// ```
    ///
    /// `__default_pthread_attr.stacksize` is set by ld.so during
    /// `__pthread_initialize_minimal`, from RLIMIT_STACK. Our prelink+trampoline
    /// entry never runs ld.so, so it stays zero -- the same class of gap as the
    /// musl `libc.tls_*` seed and `can_do_threads`, on the other libc. It hid
    /// because every threading guard in the suite was static glibc (which does
    /// this in its own `__libc_start_main`) or fused musl.
    ///
    /// WHY THIS ONE POKES NOTHING. The two musl seeds write struct fields, and
    /// each needed its own evidence that the layout was what we thought.
    /// `struct pthread_attr` has no such anchor, but glibc EXPORTS the three
    /// functions that manipulate it -- so this builds an attr with
    /// `pthread_attr_init`, sets the size with `pthread_attr_setstacksize`, and
    /// installs it with `pthread_setattr_default_np`, exactly as a guest would.
    /// No offsets, no version table, and glibc validates its own input on the
    /// way through. A closure missing any of the three (musl, or no pthread) is
    /// a no-op.
    ///
    /// Runs at bring-up on the ifunc scratch stack, like `apply_early_init`:
    /// pure setup, no syscalls, no suspension.
    pub unsafe fn apply_pthread_attr_default(&mut self, arena_ptr: *mut u8, base: *const State) {
        let init = self.dlsym_lookup(b"pthread_attr_init");
        let setsize = self.dlsym_lookup(b"pthread_attr_setstacksize");
        let setdefault = self.dlsym_lookup(b"pthread_setattr_default_np");
        if init == 0 || setsize == 0 || setdefault == 0 {
            return;
        }
        let attr = crate::arena::PTHREAD_ATTR_VMA;
        self.arena
            .slice_mut(attr, crate::arena::PTHREAD_ATTR_SIZE as usize)
            .fill(0);

        let size = crate::diag::thread_stack_size();
        // Each call is (vma, x0, x1) -> x0. Bail on the first failure rather
        // than installing a half-built attr.
        for (vma, a0, a1, what) in [
            (init, attr, 0, "pthread_attr_init"),
            (setsize, attr, size, "pthread_attr_setstacksize"),
            (setdefault, attr, 0, "pthread_setattr_default_np"),
        ] {
            let Some(f) = self.func_at(vma) else {
                ecv_warn!(
                    ecvisor,
                    "pthread attr seed SKIPPED: {what} 0x{vma:x} is not lifted"
                );
                return;
            };
            let mut st = State::new_boxed();
            core::ptr::copy_nonoverlapping(base, &mut *st, 1);
            st.gpr.x[0].val = a0;
            st.gpr.x[1].val = a1;
            st.gpr.sp.val = crate::arena::IFUNC_STACK_TOP;
            st.gpr.pc.val = vma;
            let ctx_ptr = self as *mut EcvContext;
            f(arena_ptr, &mut *st, vma, ctx_ptr);
            let rc = st.gpr.x[0].val as i64;
            if rc != 0 {
                // glibc rejected it -- e.g. a size below PTHREAD_STACK_MIN.
                // Report the value it refused, because the knob is the only
                // thing that can produce this.
                ecv_warn!(
                    ecvisor,
                    "pthread attr seed FAILED at {what}: rc={rc} (stack size {size})"
                );
                return;
            }
        }
        // ASK GLIBC WHAT IT NOW BELIEVES, rather than trusting three zero
        // return codes. `pthread_setattr_default_np` returning 0 says the call
        // was accepted, not that `pthread_create` will read what we set -- and
        // the first version of this seed reported success while
        // `allocate_stack` still asserted `size != 0`.
        let getdefault = self.dlsym_lookup(b"pthread_getattr_default_np");
        let getsize = self.dlsym_lookup(b"pthread_attr_getstacksize");
        if getdefault != 0 && getsize != 0 {
            let attr2 = attr + crate::arena::PTHREAD_ATTR_SIZE;
            let out = attr2 + crate::arena::PTHREAD_ATTR_SIZE;
            self.arena
                .slice_mut(attr2, crate::arena::PTHREAD_ATTR_SIZE as usize + 8)
                .fill(0);
            let mut ok = true;
            for (vma, a0, a1) in [(getdefault, attr2, 0), (getsize, attr2, out)] {
                let Some(f) = self.func_at(vma) else {
                    ok = false;
                    break;
                };
                let mut st = State::new_boxed();
                core::ptr::copy_nonoverlapping(base, &mut *st, 1);
                st.gpr.x[0].val = a0;
                st.gpr.x[1].val = a1;
                st.gpr.sp.val = crate::arena::IFUNC_STACK_TOP;
                st.gpr.pc.val = vma;
                let ctx_ptr = self as *mut EcvContext;
                f(arena_ptr, &mut *st, vma, ctx_ptr);
                if st.gpr.x[0].val != 0 {
                    ok = false;
                    break;
                }
            }
            let got = if ok {
                u64::from_le_bytes(self.arena.slice(out, 8).try_into().unwrap())
            } else {
                u64::MAX
            };
            if got != size {
                ecv_warn!(
                    ecvisor,
                    "pthread default stack seed NOT OBSERVED: set {size}, glibc reports {got}"
                );
                return;
            }
        }
        ecv_warn!(
            ecvisor,
            "pthread default stack seed: {size} bytes (RAPTORMARK_ECV_THREAD_STACK)"
        );
    }

    /// Resolves a symbol name to its fused VMA via the prelinker's `.ecv.dlsyms`
    /// export table (see internal/prelink/glibc.go buildDlsymsData), for the
    /// intercepted dlsym. Returns 0 if absent or unresolved. Layout: [count u32]
    /// [pad u32], then count [vma u64][nameoff u32][pad u32] entries, then a blob
    /// of NUL-terminated names (nameoff is a byte offset from the section start).
    pub fn dlsym_lookup(&self, name: &[u8]) -> u64 {
        self.dlsym_in(self.procs[self.current].prog_idx, name)
    }

    /// The same lookup, against a NAMED unit's export table.
    ///
    /// This is what makes `dlsym(handle, name)` handle-AWARE. Before it, the
    /// intercepted `dlsym` ignored its handle and searched one flat closure-wide
    /// table, so postgres:17's 79 extensions -- which each define
    /// `Pg_magic_func`, `_PG_init` and `pg_finfo_*` -- collapsed to whichever
    /// definition the fuser saw first, and every extension after the first bound
    /// the wrong one silently. Since `internal/fuse.FuseWithUnits` each unit
    /// carries only its OWN exports, so scoping the search to the unit the
    /// handle names is the whole fix.
    ///
    /// `prog_idx` out of range yields 0 rather than panicking: a handle comes
    /// from the guest, and a guest may pass anything.
    pub fn dlsym_in(&self, prog_idx: usize, name: &[u8]) -> u64 {
        if prog_idx >= self.programs.len() {
            return 0;
        }
        let prog = self.programs.get(prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.dlsyms") else {
            return 0;
        };
        if size < 8 {
            return 0;
        }
        unsafe {
            let count = core::ptr::read_unaligned(bytes as *const u32) as usize;
            for i in 0..count {
                let e = 8 + i * 16;
                if e + 12 > size {
                    break;
                }
                let vma = core::ptr::read_unaligned(bytes.add(e) as *const u64);
                let noff = core::ptr::read_unaligned(bytes.add(e + 8) as *const u32) as usize;
                // Compare `name` against the NUL-terminated entry name at `noff`.
                let mut j = 0usize;
                loop {
                    if noff + j >= size {
                        break;
                    }
                    let c = *bytes.add(noff + j);
                    if c == 0 {
                        if j == name.len() {
                            return vma;
                        }
                        break;
                    }
                    if j >= name.len() || name[j] != c {
                        break;
                    }
                    j += 1;
                }
            }
        }
        0
    }

    /// Runs the program's constructors at bring-up (the `_dl_init` equivalent), if
    /// the prelinker emitted them to `.ecv.init`. For a dynamic binary ld.so runs
    /// each object's DT_INIT then DT_INIT_ARRAY (deps before dependents, the main
    /// object's DT_PREINIT_ARRAY first) between relocation and `main`; our
    /// prelink+trampoline entry bypasses ld.so, so the prelinker resolved the
    /// constructor VMAs (in that order, loader skipped) into `.ecv.init` and we call
    /// each here. Runs AFTER `apply_early_init` (`_dl_call_libc_early_init` precedes
    /// `_dl_init`). No section (no constructors / no glibc) -> no-op.
    ///
    /// Each ctor is called with the SysV init ABI `fn(argc, argv, envp)`: argc/argv/
    /// envp are read from the guest's initial stack (`base.sp` points at argc, argv
    /// follows, envp after the argv NULL terminator). Like the ifunc/early_init
    /// calls it runs on the ifunc scratch stack with a State cloned from `base`
    /// (arena writes persist), and completes without suspending (constructors do no
    /// blocking I/O).
    pub unsafe fn apply_init_array(&mut self, arena_ptr: *mut u8, base: *const State) {
        let cur = self.procs[self.current].prog_idx;
        self.apply_init_array_in(cur, arena_ptr, base);
    }

    /// The same, for a NAMED unit: a plugin's own constructors, which `dlopen`
    /// must run and startup must not. `internal/fuse.unitTables` puts only the
    /// unit's own `DT_INIT`/`DT_INIT_ARRAY` in its `.ecv.init`.
    ///
    /// # Safety
    ///
    /// Same as `apply_init_array`.
    pub unsafe fn apply_init_array_in(
        &mut self,
        prog_idx: usize,
        arena_ptr: *mut u8,
        base: *const State,
    ) {
        if prog_idx >= self.programs.len() {
            return;
        }
        let prog = self.programs.get(prog_idx);
        let Some((_vma, size, bytes)) = prog.find_data_section(b".ecv.init") else {
            return;
        };
        let count = size / 8;
        if count == 0 {
            return;
        }
        // Snapshot the VMA list before running any ctor (a ctor mutates the arena,
        // and `bytes` may point into it).
        let vmas: Vec<u64> = (0..count)
            .map(|i| core::ptr::read_unaligned(bytes.add(i * 8) as *const u64))
            .collect();
        // argc/argv/envp from the initial stack the entry program will consume.
        let sp = (*base).gpr.sp.val;
        let argc = u64::from_le_bytes(self.arena.slice(sp, 8).try_into().unwrap());
        let argv = sp + 8;
        let envp = sp + 8 + (argc + 1) * 8;
        for vma in vmas {
            if vma == 0 {
                continue;
            }
            let Some(f) = self.func_at(vma) else {
                fatal!("init_array ctor 0x{vma:x} (.ecv.init) not in the lifted function table");
            };
            let mut st = State::new_boxed();
            core::ptr::copy_nonoverlapping(base, &mut *st, 1);
            st.gpr.x[0].val = argc;
            st.gpr.x[1].val = argv;
            st.gpr.x[2].val = envp;
            st.gpr.sp.val = crate::arena::IFUNC_STACK_TOP;
            st.gpr.pc.val = vma;
            let ctx_ptr = self as *mut EcvContext;
            f(arena_ptr, &mut *st, vma, ctx_ptr);
        }
        ecv_warn!(ecvisor, "ran {count} _dl_init constructor(s)");
    }

    /// Runs one ifunc resolver as a guest function and returns its result (x0),
    /// or None if the resolver VMA is not a lifted function. The resolver is
    /// invoked with the aarch64 ifunc ABI — x0 = hwcap | _IFUNC_ARG_HWCAP,
    /// x1 = &__ifunc_arg_t — on a dedicated scratch stack, with a temporary State
    /// cloned from `base`. dl_hwcap is 0 in the frozen offline image (ld.so's
    /// runtime hwcap detection never ran), so resolvers select their generic
    /// implementation, which is always correct.
    unsafe fn run_ifunc_resolver(
        &mut self,
        arena_ptr: *mut u8,
        base: *const State,
        resolver_vma: u64,
    ) -> Option<u64> {
        let f = self.func_at(resolver_vma)?;

        // __ifunc_arg_t { _size, _hwcap, _hwcap2 } in the scratch slot.
        let arg_vma = crate::arena::IFUNC_ARG_VMA;
        {
            let a = self.arena.slice_mut(arg_vma, 24);
            a[0..8].copy_from_slice(&24u64.to_le_bytes()); // _size = sizeof(__ifunc_arg_t)
            a[8..16].copy_from_slice(&0u64.to_le_bytes()); // _hwcap
            a[16..24].copy_from_slice(&0u64.to_le_bytes()); // _hwcap2
        }

        const IFUNC_ARG_HWCAP: u64 = 0x8000_0000_0000_0000;
        let mut st = State::new_boxed();
        core::ptr::copy_nonoverlapping(base, &mut *st, 1);
        st.gpr.x[0].val = IFUNC_ARG_HWCAP; // dl_hwcap(0) | _IFUNC_ARG_HWCAP
        st.gpr.x[1].val = arg_vma;
        st.gpr.sp.val = crate::arena::IFUNC_STACK_TOP;
        st.gpr.pc.val = resolver_vma;

        let ctx_ptr = self as *mut EcvContext;
        f(arena_ptr, &mut *st, resolver_vma, ctx_ptr);
        Some(st.gpr.x[0].val)
    }

    /// Exact-match function lookup (BLR path, `__remill_function_call`).
    pub fn func_at(&self, vma: u64) -> Option<LiftedFunc> {
        self.funcs
            .binary_search_by_key(&vma, |&(v, _)| v)
            .ok()
            .map(|i| self.funcs[i].1)
    }

    /// BR-path lookup (`__remill_jump`): the target may be the entry of a
    /// function or a point inside the preceding function.
    pub fn func_containing(&self, vma: u64) -> Option<LiftedFunc> {
        match self.funcs.binary_search_by_key(&vma, |&(v, _)| v) {
            Ok(i) => Some(self.funcs[i].1),
            Err(0) => None,
            Err(i) => Some(self.funcs[i - 1].1),
        }
    }

    /// The canonical directory a dirfd is open on, or `None` if it is not an
    /// open directory.
    ///
    /// Borrowed, not cloned: this sits on the path of every relative `*at`
    /// call and callers only need a resolution base.
    ///
    /// ⚠️ A negative fd is handled by `Vec::get`, NOT by a range check: `-7 as
    /// usize` is an enormous index and `get` returns `None` for it. An earlier
    /// version carried an explicit `if dirfd < 0` guard and a comment claiming
    /// it was what made this safe. Neutralizing it changed no test and no
    /// behaviour, which is how the claim was found to be false; the guard is
    /// gone rather than left as defence nothing can observe.
    pub fn dir_fd_path(&self, dirfd: i64) -> Option<&[u8]> {
        match self.fds.get(dirfd as usize).and_then(|s| s.as_ref()) {
            Some(OpenFile::Dir { path, .. }) => Some(path),
            _ => None,
        }
    }

    /// The directory a syscall path argument is relative to.
    ///
    /// * an ABSOLUTE path ignores the dirfd (the base is unused, but the cwd is
    ///   returned so callers have one value to pass on);
    /// * `AT_FDCWD` means the cwd;
    /// * anything else means the directory that dirfd is open on.
    ///
    /// `None` only when a dirfd was given, the path is relative, and that fd is
    /// not an open directory -- so a caller can report its own errno for the
    /// case it could not serve.
    ///
    /// ❗ **`sys.rs` used to make this decision in FOUR places and refuse in all
    /// four** -- `resolve_arg`, `unlinkat` (ENOENT), `readlinkat` (EINVAL) and
    /// `mkdirat` (EINVAL), each with its own comment saying relative-to-dirfd
    /// was unsupported. Fixing one would have left three, and the next symptom
    /// would have looked like a different bug.
    ///
    /// ⚠️ It lives HERE, not in `sys.rs`, because `mod sys` is
    /// `#[cfg(target_arch = "wasm32")]`: nothing in that file is compiled on the
    /// host, so nothing in it can be reached by `cargo test`. A test module was
    /// written there first and silently compiled NOWHERE -- green, and covering
    /// nothing.
    pub fn resolve_base(&self, dirfd: i64, path: &[u8]) -> Option<&[u8]> {
        if path.first() == Some(&b'/') || dirfd == AT_FDCWD {
            return Some(&self.cwd);
        }
        self.dir_fd_path(dirfd)
    }

    /// The START VMA of the function containing `vma`, the address
    /// [`Self::func_containing`] resolves through. `call_history` is keyed by
    /// this, so recognising a non-local jump needs the number, not the pointer.
    pub fn func_start_containing(&self, vma: u64) -> Option<u64> {
        match self.funcs.binary_search_by_key(&vma, |&(v, _)| v) {
            Ok(i) => Some(self.funcs[i].0),
            Err(0) => None,
            Err(i) => Some(self.funcs[i - 1].0),
        }
    }

    /// The `call_history` depth of the frame a non-local jump to `t_vma` lands
    /// in, or `None` if this is an ordinary `br`.
    ///
    /// # What distinguishes the two
    ///
    /// A `br` is a TAIL CALL when its target is a function ENTRY: it abandons
    /// exactly one guest frame, and `__remill_jump`'s nested-call dispatch pops
    /// exactly one host frame for it (the lifted function's `ret void`, which
    /// `AddTerminatingTailCall` emits right after the call). Host frames popped
    /// equals guest frames abandoned, so it is correct and must not be touched.
    ///
    /// A `br` is a NON-LOCAL JUMP when its target is a point INSIDE a function
    /// that is already on the guest stack -- which is what `__longjmp`'s
    /// `br x30` is, every time. It abandons every frame between here and that
    /// one, and the nested call still pops one. See JOURNAL.md 2026-09-01 for
    /// the measurement; the visible symptom was `__libc_siglongjmp` resuming at
    /// the instruction after its `bl __longjmp` -- the mask-restore block, which
    /// branches straight back to the call.
    ///
    /// ⚠️ **Ambiguous under recursion, and it cannot be otherwise here.** The
    /// innermost matching frame is chosen. Disambiguating would need the guest
    /// SP recorded per frame, and `call_history` entries are written by the
    /// INLINED fast path (elfconv patch 0060) straight into a wasm global as
    /// (func, ret) pairs, so widening them is a lifter change. Choosing the
    /// innermost frame of a recursive function is a guess; today's behaviour is
    /// wrong for every longjmp including the non-recursive ones, so this is
    /// strictly better, but it is a guess and is logged as one.
    pub fn nonlocal_jump_depth(&self, t_vma: u64) -> Option<usize> {
        // A function ENTRY is a tail call, never a longjmp. Checked FIRST: a
        // self-recursive tail call has its own entry on the history and would
        // otherwise match below.
        if self.func_at(t_vma).is_some() {
            return None;
        }
        let f = self.func_start_containing(t_vma)?;
        self.call_history
            .iter()
            .rposition(|&(fn_vma, _)| fn_vma == f)
    }

    /// Records the replay for a non-local jump to `t_vma` landing at `depth`,
    /// and asks the leg loop to re-enter this process there.
    ///
    /// The guest state is ALREADY correct -- `__longjmp` restored the callee-
    /// saved registers, SP and FP before its `br` -- so nothing here touches it
    /// beyond the pc. What has to change is the HOST stack, and the only way to
    /// shorten it is to return through every frame, which is what
    /// `set_unwinding` makes the lifted code do.
    ///
    /// `call_history` is truncated in lockstep with `remaining` so the two stay
    /// the same length, the invariant [`Self::capture_suspend_replay`] holds and
    /// the leg loop's pop-in-lockstep depends on.
    ///
    /// ⚠️ **The ordering around the inlined call history is load-bearing** and
    /// is NOT exercised by the reproducers, because `inline_call_history_enabled`
    /// is an opt-in and they run with it off. It works out as:
    /// `adopt_call_history_depth` (caller, before the search) -> `truncate` here
    /// -> the unwind, which pushes nothing because it makes no calls and pops
    /// nothing because every frame returns BEFORE its `_ecv_func_epilogue`
    /// (elfconv patch 0026) -> the leg loop's `publish_call_history`, which
    /// writes the truncated length back to `__ecv_ch_len` before re-entry.
    /// Truncating cannot reallocate, so the published base stays valid and
    /// `adopt`'s moved-buffer check cannot fire on account of this.
    pub fn begin_nonlocal_jump(&mut self, t_vma: u64, depth: usize) {
        let f = self
            .func_start_containing(t_vma)
            .expect("nonlocal_jump_depth already resolved the containing function");
        let remaining = self.call_history[..depth].to_vec();
        self.call_history.truncate(depth);
        let cur = self.current;
        self.procs[cur].replay = Some(Replay {
            cur: (f, t_vma),
            remaining,
            // Not a syscall-resume: the frame resumes at a branch target, so the
            // scheduler must NOT drive a syscall handler before re-entering it.
            resuming: false,
        });
        self.longjmp_pending = true;
    }

    /// True if the 4-byte guest instruction at `vma` is a BTI or NOP -- a
    /// no-op hint a computed branch may land on. aarch64 is little-endian;
    /// NOP = 0xD503201F, BTI {c,j,jc} = 0xD503241F | (op2 << 5) (CRm = 0b0100),
    /// matched by masking out op2's high bits.
    fn is_skippable_landing_pad(&self, vma: u64) -> bool {
        // `vma` is a COMPUTED branch target, so it can be anything the guest
        // managed to put in a register. Slicing it unchecked turned a wild
        // branch into a Rust panic inside the arena -- observed as
        //   panicked at src/arena.rs: range end index 1600614256 out of range
        // where 0x5F676F6C is the ASCII "log_", i.e. a string being followed as
        // a code pointer. That reported the wrong layer and buried the guest
        // address. Out of range is simply "not a landing pad", which lets the
        // caller's existing miss path name the bad target instead.
        let off = vma.wrapping_sub(crate::arena::MEMORY_ARENA_VMA);
        if off.saturating_add(4) > crate::arena::MEMORY_ARENA_SIZE as u64 {
            return false;
        }
        let b = self.arena.slice(vma, 4);
        let insn = u32::from_le_bytes([b[0], b[1], b[2], b[3]]);
        insn == 0xD503_201F || (insn & 0xFFFF_FF1F) == 0xD503_241F
    }

    /// Skips forward over leading BTI/NOP landing pads from `bb_vma`, returning
    /// the first real block start found in `m`, or None. A jump-table switch can
    /// target a `bti j` case landing pad that the lifter never registered as a
    /// block start; because BTI/NOP are no-ops, entering at the instruction after
    /// them is semantically identical, and this avoids the catch-all re-dispatch
    /// loop (entering at the missing pad re-runs the same computed branch forever,
    /// each hop nesting a wasm shadow-stack frame until it overflows). See
    /// JOURNAL "Blocker 4" (create_scan_plan+0x698 / ValuesScan jump table).
    fn skip_landing_pads(&self, m: &[(u64, *mut u64)], bb_vma: u64) -> Option<*mut u64> {
        let mut probe = bb_vma;
        while probe < bb_vma + 16 && self.is_skippable_landing_pad(probe) {
            probe += 4;
            if let Ok(j) = m.binary_search_by_key(&probe, |&(v, _)| v) {
                ecv_debug!(
                    ecv,
                    "indirectbr: skipped BTI/NOP pad 0x{bb_vma:x} -> block 0x{probe:x}"
                );
                return Some(m[j].1);
            }
        }
        None
    }

    /// Basic-block address for an indirect branch inside fn_vma. A miss on
    /// bb_vma first skips BTI/NOP landing pads, then falls back to the function's
    /// `UINT64_MAX` catch-all entry, mirroring upstream.
    pub fn block_address(&self, fn_vma: u64, bb_vma: u64) -> *mut u64 {
        let Ok(i) = self.bb_maps.binary_search_by_key(&fn_vma, |&(v, _)| v) else {
            fatal!("0x{fn_vma:x} is not the entry address of any lifted function (indirectbr)");
        };
        let m = &self.bb_maps[i].1;
        if let Ok(j) = m.binary_search_by_key(&bb_vma, |&(v, _)| v) {
            return m[j].1;
        }
        if let Some(p) = self.skip_landing_pads(m, bb_vma) {
            return p;
        }
        // A miss here is the interesting event, not a curiosity: the branch is
        // about to fall back to the function's UINT64_MAX catch-all (L_far_jump)
        // and thence to __remill_jump, which RE-ENTERS the containing function.
        // When the target is inside the branching function -- a jump table -- that
        // turns a guest loop into one wasm frame per iteration. So report it under
        // plain DEBUG, not just the shadow-stack probe.
        //
        // (This used to also carry a dump hardcoded to 0x4e1000..0x4e2000, the
        // PostgreSQL `create_scan_plan` ValuesScan runaway. Same failure, chased
        // once before; generalised rather than re-pinned to a new address.)
        if crate::diag::legsp() || crate::diag::debug_log() {
            if bbmiss_first_time(fn_vma, bb_vma) {
                let near: Vec<u64> = m
                    .iter()
                    .map(|&(v, _)| v)
                    .filter(|&v| {
                        v != u64::MAX && v >= bb_vma.saturating_sub(0x40) && v <= bb_vma + 0x80
                    })
                    .collect();
                // No `insn=` field: the arena holds no guest .text, so the read
                // this line used to do yielded a fabricated `udf #0`. See
                // `bbmiss_message` before adding one back.
                ecv_log!(
                    crate::diag::legsp() || crate::diag::debug_log(),
                    bbmiss,
                    "{}",
                    bbmiss_message(
                        fn_vma,
                        bb_vma,
                        m.len(),
                        m.iter().any(|&(v, _)| v == u64::MAX),
                        &near,
                    )
                );
            }
        }
        match m.binary_search_by_key(&u64::MAX, |&(v, _)| v) {
            Ok(j) => m[j].1,
            Err(_) => fatal!(
                "bb 0x{bb_vma:x} not found in fn 0x{fn_vma:x} and no catch-all entry (indirectbr)"
            ),
        }
    }

    /// Strict basic-block lookup for the no-opt indirectbr path.
    pub fn block_address_strict(&self, fn_vma: u64, bb_vma: u64) -> *mut u64 {
        let Ok(i) = self.bb_maps.binary_search_by_key(&fn_vma, |&(v, _)| v) else {
            fatal!("fn 0x{fn_vma:x} not in bb map (_ecv_noopt_get_bb)");
        };
        let m = &self.bb_maps[i].1;
        if let Ok(j) = m.binary_search_by_key(&bb_vma, |&(v, _)| v) {
            return m[j].1;
        }
        if let Some(p) = self.skip_landing_pads(m, bb_vma) {
            return p;
        }
        fatal!("bb 0x{bb_vma:x} not in fn 0x{fn_vma:x} (_ecv_noopt_get_bb)");
    }
}

/// Closes musl's thread list on itself: `self->self = self->prev = self->next =
/// self`, which `__init_tp` would have done. Returns false if it was already
/// linked. See `apply_musl_tp`, which restores the `tid` half of the same
/// bring-up.
///
/// musl walks the list with a DO-WHILE that terminates on `td == self`:
///
/// ```text
/// do td->tsd[k] = 0; while ((td = td->next) != self);      // pthread_key_delete
/// ```
///
/// With `next` left at zero that loop reads `0 + tsd_off`, writes through it,
/// reads `0 + 16`, gets zero again and never leaves -- an infinite loop that
/// issues NO SYSCALLS, so a syscall trace just stops with no error at all.
/// `redis-server --version` printed its banner and then span there forever.
///
/// OFFSETS. `self` at 0 is ABI (musl's asm reads it). `prev` at 8 and `next` at
/// 16 are marked non-ABI in `pthread_impl.h`, so they were read off a real
/// image rather than trusted -- musl's own `pthread_exit` unlinks with
///
/// ```text
/// a62324: ldp x0, x1, [x19, #8]    // x0 = prev, x1 = next
/// a62328: str x0, [x1, #8]         // next->prev = prev
/// a6232c: ldr x1, [x19, #16]
/// a62330: str x1, [x0, #16]        // prev->next = next
/// a62334: stp x19, x19, [x19, #8]  // prev = next = self
/// ```
///
/// and the `tid_off` `apply_musl_tp` derives from musl's exported `gettid`
/// lands at base+32, which is only where `tid` sits if `self`, `prev`, `next`
/// and `sysinfo` occupy 0/8/16/24. Two independent readings of one layout.
///
/// Only NULL links are filled. If musl does run its own `__init_tp`, or a
/// forked child inherits an already-linked arena, the list is left exactly as
/// it is: this can seed the list, never relink it.
/// Writes `MINSIGSTKSZ` into `_rtld_global_ro._dl_minsigstacksize` at `addr`,
/// returning the value written, or None if anything about `addr` fails to check
/// out.
///
/// ld.so sets that field from `AT_MINSIGSTKSZ`, falling back to the
/// architecture's `MINSIGSTKSZ`. A fused image runs neither path, so it stays
/// zero and `sysconf(_SC_MINSIGSTKSZ)` -- which asserts on zero -- aborts the
/// guest. python:3-slim dies there, after every one of its constructors has run.
///
/// The address is DECODED FROM CODE by the prelinker (`minsigstacksizeVMA`), so
/// it is checked again here rather than trusted. Two things must hold, and both
/// are properties of the struct rather than of the decode:
///
/// - the preceding word is `_dl_pagesize`, which ld.so's static initialiser DOES
///   set, so it must already look like a page size. An offset that landed
///   elsewhere in `_rtld_global_ro` almost certainly fails this.
/// - the target is still zero, i.e. nobody has set it. If some path did run
///   ld.so's initialisation after all, its value wins.
fn seed_dl_minsigstacksize(arena: &mut Arena, addr: u64) -> Option<u64> {
    if addr < 8 || !arena.in_bounds(addr - 8, 16) {
        return None;
    }
    let at = |a: &Arena, x: u64| u64::from_le_bytes(a.slice(x, 8).try_into().unwrap());
    let pagesize = at(arena, addr - 8);
    if !pagesize.is_power_of_two() || !(4096..=65536).contains(&pagesize) {
        return None;
    }
    if at(arena, addr) != 0 {
        return None;
    }
    let v = crate::arena::MINSIGSTKSZ;
    arena.slice_mut(addr, 8).copy_from_slice(&v.to_le_bytes());
    Some(v)
}

fn seed_musl_thread_list(arena: &mut Arena, base: u64) -> bool {
    const PREV: u64 = 8;
    const NEXT: u64 = 16;
    let read = |a: &Arena, at: u64| u64::from_le_bytes(a.slice(at, 8).try_into().unwrap());
    if read(arena, base + PREV) != 0 || read(arena, base + NEXT) != 0 {
        return false;
    }
    for off in [0, PREV, NEXT] {
        arena
            .slice_mut(base + off, 8)
            .copy_from_slice(&base.to_le_bytes());
    }
    true
}

/// The member of `tgid` that should be woken to take a PROCESS-directed signal,
/// or None when no member can take it right now.
///
/// "Can take it" is the whole point: a process-directed signal may be handled by
/// any thread that does not block it, and picking one that DOES block it means
/// the signal is filed, a task is woken, and nothing is delivered -- which reads
/// from outside as "signals do not work" rather than as a selection bug.
/// Only tasks parked in an interruptible wait are candidates; a runnable one
/// will reach a delivery boundary on its own.
fn signal_wake_candidate(
    procs: &[Process],
    tgid: u32,
    bit: u64,
    blocked_of: impl Fn(usize) -> u64,
) -> Option<usize> {
    (0..procs.len()).find(|&i| {
        procs[i].tgid == tgid
            && procs[i].status == ProcStatus::Blocked
            && EcvContext::signal_interruptible(procs[i].blocked_on)
            && blocked_of(i) & bit == 0
    })
}

/// The signals a task may dequeue right now: either queue, minus the set the
/// caller wants left pending (its blocked mask, or the temporary mask of a
/// `ppoll`/`sigsuspend` wait).
///
/// The union is the fix for a single shared queue: a `tgkill` files into the
/// TASK's queue, and a delivery pass that read only the group's would leave a
/// `pthread_kill` pending forever.
pub fn deliverable_set(group_pending: u64, task_pending: u64, wait_mask: u64) -> u64 {
    (group_pending | task_pending) & !wait_mask
}

/// The member of `idx`'s thread group for which `holds` is true, or `idx` when
/// no member qualifies.
///
/// Threads share one arena and one fd table, and `save_current` files whatever
/// is live under whichever member happened to be running -- so a switch INTO a
/// group has to locate the shared state by GROUP, not by task. `idx` is checked
/// first so that every single-threaded process short-circuits and the scan never
/// runs on the existing corpus.
///
/// Zombie and dead members are eligible holders on purpose: a thread that exits
/// while its siblings run must not carry the group's memory off with it, and
/// leaving the buffer filed where it lies is cheaper than relocating it.
fn group_member_where(procs: &[Process], idx: usize, holds: fn(&Process) -> bool) -> usize {
    if holds(&procs[idx]) {
        return idx;
    }
    let tg = procs[idx].tgid;
    procs
        .iter()
        .position(|p| p.tgid == tg && holds(p))
        .unwrap_or(idx)
}

/// Parks `live` under `prev` and replaces it with `want`'s parked tables,
/// reporting whether `want` had any. On a miss `live` is left EMPTY, so the
/// caller builds into it.
///
/// A free function taking the cache and the live tables, for the same reason
/// `mem_ref_mismatches` is one: the DECISION is what can be tested on the host,
/// while an `EcvContext` to hang it off cannot be built there. The failure mode
/// is a process running another program's dispatch tables, which is silent --
/// the same class of bug the stale `materialized_prog` cache produced, and it
/// took a 40-minute postgres run to find that one.
pub fn swap_tables(
    cache: &mut [Option<DispatchTables>],
    prev: Option<usize>,
    want: usize,
    live: &mut DispatchTables,
) -> bool {
    // `None` means the live tables are a MERGE of several programs and belong to
    // no single index -- not "unknown". Parking them under one program would
    // hand a later switch a table carrying other programs' VMAs.
    if let Some(prev) = prev {
        if let Some(slot) = cache.get_mut(prev) {
            *slot = Some(core::mem::take(live));
        }
    }
    match cache.get_mut(want).and_then(Option::take) {
        Some(t) => {
            *live = t;
            true
        }
        None => {
            // Left empty on purpose: the caller is about to build into it, and a
            // stale table here would be the previous program's.
            *live = DispatchTables::default();
            false
        }
    }
}

/// Slots whose recorded reference count disagrees with the recomputed one, as
/// `(idx, recorded, actual)`.
///
/// A free function taking both vectors, for the same reason `bounded_ranges` is
/// one: the COMPARISON is the part that can be tested on the host, while the
/// collection it consumes needs a whole `EcvContext` and cannot be. Splitting
/// them keeps the rule "too low is dangerous, too high is a leak" checkable
/// without a 384 MiB arena and a program table.
///
/// What this does NOT cover, and the split is why it is worth saying: whether
/// `audit_mem_refs` counted all three kinds of holder. Miss one and every slot
/// it holds reports a phantom mismatch. That side is verified by the probe
/// staying SILENT on a real multi-process workload, not here.
/// Counts, per slot of a context-global table, the descriptors that name it.
///
/// Free function rather than a method so the "three holders" rule can be tested
/// without an `EcvContext`, which the host build cannot construct (it needs the
/// `&'static EcvProgram`s the linked registry supplies). That rule is the part
/// worth testing: it took a day to get right, and it is now consulted by two
/// audits -- the file table's and the offset table's -- which must not be able
/// to drift apart.
///
/// `tables` is every fd table alive anywhere; `queued` is every entry held by an
/// SCM_RIGHTS batch that has not landed in one yet. An index past `len` is
/// ignored rather than panicking: the audit is a diagnostic, and a table that
/// has since shrunk must not take the module down.
pub fn count_named_slots<'a>(
    tables: impl Iterator<Item = &'a [Option<OpenFile>]>,
    queued: impl Iterator<Item = &'a OpenFile>,
    len: usize,
    which: fn(&OpenFile) -> Option<usize>,
) -> Vec<u32> {
    let mut actual = vec![0u32; len];
    let mut bump = |idx: usize, actual: &mut Vec<u32>| {
        if let Some(slot) = actual.get_mut(idx) {
            *slot += 1;
        }
    };
    for fds in tables {
        for idx in fds.iter().flatten().filter_map(which) {
            bump(idx, &mut actual);
        }
    }
    for idx in queued.filter_map(which) {
        bump(idx, &mut actual);
    }
    actual
}

pub fn mem_ref_mismatches(recorded: &[u32], actual: &[u32]) -> Vec<(usize, u32, u32)> {
    recorded
        .iter()
        .zip(actual.iter())
        .enumerate()
        .filter(|(_, (r, a))| r != a)
        .map(|(i, (r, a))| (i, *r, *a))
        .collect()
}

/// Tasks in `tgid` that are still running (neither zombie nor reaped).
fn live_in_group(procs: &[Process], tgid: u32) -> usize {
    procs
        .iter()
        .filter(|p| p.tgid == tgid && !matches!(p.status, ProcStatus::Zombie(_) | ProcStatus::Dead))
        .count()
}

/// Tasks in `tgid` that still occupy a slot -- a zombie leader counts, a reaped
/// one does not. This, not `live_in_group`, decides whether `exit` means the
/// thread or the process: a leader that has already exited but is still awaiting
/// its parent's `wait4` is not something the last worker thread should ignore.
fn unreaped_in_group(procs: &[Process], tgid: u32) -> usize {
    procs
        .iter()
        .filter(|p| p.tgid == tgid && p.status != ProcStatus::Dead)
        .count()
}

/// True if no task of `tgid` is still running. Only then may the group's arena
/// be released -- it is the whole group's address space, and a sibling that is
/// merely a zombie has already stopped executing, while one that is Runnable or
/// Blocked has not.
fn group_is_dead(procs: &[Process], tgid: u32) -> bool {
    live_in_group(procs, tgid) == 0
}

/// True if `tgid` holds more than one unreaped task -- the test that decides
/// whether `exit` retires a thread or the whole process.
///
/// It counts UNREAPED, not live: a leader that has exited and is waiting to be
/// reaped still owns the process's identity, so the last worker's `exit` must
/// not be promoted into a second group teardown.
fn group_is_multithreaded(procs: &[Process], tgid: u32) -> bool {
    unreaped_in_group(procs, tgid) > 1
}

#[cfg(test)]
mod call_history_tests {
    use super::*;

    // WHAT IS AND IS NOT COVERED HERE. The call history is a plain
    // `Vec<(u64, u64)>`, so its push/pop semantics are the standard library's
    // and not worth restating. What IS this project's to get right is the
    // layout the lifted code writes through, and the default-path gate.
    //
    // The reconciliation itself (`publish_call_history` /
    // `adopt_call_history_depth`) is unreachable from a host test: it is a no-op
    // unless `ch_inline`, which is false off wasm32, and the code it cooperates
    // with lives in `intrinsics.rs`, which `lib.rs` gates to wasm32. Only the
    // env-gated E2E suite covers that. An earlier design put this logic in a
    // host-testable type and paid 6.5% on the default path for the privilege;
    // the coverage was not worth the tax.

    /// The lifted fast path computes `base + len*16` and stores the function VMA
    /// at +0 and the return address at +8. A tuple's layout is not guaranteed by
    /// the language, so this is checked rather than assumed -- at compile time by
    /// the `const` block above, and here so it is visible to a reader.
    #[test]
    fn frame_layout_is_the_one_the_lifter_writes() {
        use core::mem::{offset_of, size_of};
        assert_eq!(size_of::<(u64, u64)>(), 16);
        assert_eq!(offset_of!((u64, u64), 0), 0);
        assert_eq!(offset_of!((u64, u64), 1), 8);
    }

    /// The gate is off unless explicitly asked for. On the host it is
    /// unconditionally off, which is what makes `publish_call_history` and
    /// `adopt_call_history_depth` inert in every host test.
    #[test]
    fn inline_call_history_is_off_by_default() {
        assert!(!inline_call_history_enabled());
    }
}

#[cfg(test)]
mod table_cache_tests {
    use super::*;

    extern "C" fn dummy(_: *mut u8, _: *mut crate::abi::State, _: u64, _: *mut EcvContext) {}

    /// Tables that are distinguishable by program, so a test can tell WHICH
    /// program's tables came back rather than only that some did.
    fn tables_for(prog: u64) -> DispatchTables {
        (
            vec![(prog * 0x1000, dummy as LiftedFunc)],
            vec![(
                prog * 0x1000,
                vec![(prog * 0x1000 + 4, core::ptr::null_mut())],
            )],
        )
    }

    fn prog_of(t: &DispatchTables) -> u64 {
        t.0[0].0 / 0x1000
    }

    // A program never seen before is a miss, and the live tables are left EMPTY
    // rather than holding the previous program's -- the caller builds into them.
    #[test]
    fn a_program_with_no_parked_tables_is_a_miss() {
        let mut cache: Vec<Option<DispatchTables>> = vec![None, None];
        let mut live = tables_for(0);
        assert!(!swap_tables(&mut cache, None, 1, &mut live));
        assert!(live.0.is_empty() && live.1.is_empty());
    }

    // The whole point: park program 0, take it back, and get program 0's tables.
    #[test]
    fn parked_tables_come_back_for_the_program_that_parked_them() {
        let mut cache: Vec<Option<DispatchTables>> = vec![None, None];
        let mut live = tables_for(0);
        assert!(!swap_tables(&mut cache, Some(0), 1, &mut live));
        live = tables_for(1);
        assert!(swap_tables(&mut cache, Some(1), 0, &mut live));
        assert_eq!(prog_of(&live), 0, "took the wrong program's tables");
    }

    // The load-bearing one. `None` means the live tables are a MERGE of several
    // programs; parking them under an index would later hand some program a
    // table carrying another's VMAs.
    #[test]
    fn a_merge_is_never_parked_under_a_program() {
        let mut cache: Vec<Option<DispatchTables>> = vec![None, None];
        let mut merged = (
            vec![(0, dummy as LiftedFunc), (0x1000, dummy as LiftedFunc)],
            vec![],
        );
        assert!(!swap_tables(&mut cache, None, 0, &mut merged));
        assert!(
            cache.iter().all(Option::is_none),
            "a merged table was filed under a single program"
        );
    }

    // At most ONE copy per program exists at any moment, which is what bounds
    // the memory this cache costs to 314 KiB per program rather than per switch.
    #[test]
    fn taking_a_program_removes_it_from_the_cache() {
        let mut cache: Vec<Option<DispatchTables>> = vec![None, None];
        let mut live = tables_for(0);
        swap_tables(&mut cache, Some(0), 1, &mut live);
        assert!(cache[0].is_some());
        live = tables_for(1);
        swap_tables(&mut cache, Some(1), 0, &mut live);
        assert!(cache[0].is_none(), "program 0 is now live AND cached");
        assert!(cache[1].is_some());
    }

    // The ping-pong that motivated this: after each program has been built once,
    // every subsequent switch hits. Under the one-slot cache this sequence
    // missed every time -- measured at 602 rebuilds in 600 switches.
    #[test]
    fn a_ping_pong_misses_once_per_program_and_then_never() {
        let mut cache: Vec<Option<DispatchTables>> = vec![None, None];
        let mut live = DispatchTables::default();
        let mut prev = None;
        let mut misses = 0;
        for round in 0..8 {
            let want = round % 2;
            if !swap_tables(&mut cache, prev, want, &mut live) {
                misses += 1;
                live = tables_for(want as u64); // what build_tables would produce
            }
            assert_eq!(
                prog_of(&live),
                want as u64,
                "round {round} ran the wrong tables"
            );
            prev = Some(want);
        }
        assert_eq!(misses, 2, "expected one build per program, not per switch");
    }

    // A `prev` outside the cache cannot corrupt a slot or panic. It should not
    // happen -- but the alternative to checking is an index that silently files
    // one program's tables under another.
    #[test]
    fn an_out_of_range_prev_parks_nothing() {
        let mut cache: Vec<Option<DispatchTables>> = vec![None];
        let mut live = tables_for(0);
        assert!(!swap_tables(&mut cache, Some(9), 0, &mut live));
        assert!(cache[0].is_none());
    }
}

#[cfg(test)]
mod refcount_tests {
    use super::*;

    // The `dup` bug's signature: a slot whose recorded count is LOWER than the
    // number of descriptors that actually name it. That slot is eligible for
    // recycling while a live descriptor still points at it, which is silent.
    #[test]
    fn a_count_below_the_real_one_is_reported() {
        let got = mem_ref_mismatches(&[1, 2, 1], &[2, 2, 1]);
        assert_eq!(got, vec![(0, 1, 2)]);
    }

    // The opposite direction is a leak: the slot is pinned and never reused.
    // Wrong, but it wastes memory rather than corrupting a read, and the probe
    // must distinguish the two rather than lumping them together.
    #[test]
    fn a_count_above_the_real_one_is_reported_too() {
        let got = mem_ref_mismatches(&[3, 1], &[1, 1]);
        assert_eq!(got, vec![(0, 3, 1)]);
    }

    #[test]
    fn agreement_reports_nothing() {
        assert!(mem_ref_mismatches(&[0, 1, 7], &[0, 1, 7]).is_empty());
        assert!(mem_ref_mismatches(&[], &[]).is_empty());
    }

    // Every slot is checked, not just the first disagreement -- one clone site
    // forgetting to retain corrupts every slot it touches, and stopping at the
    // first would understate the blast radius.
    #[test]
    fn every_disagreeing_slot_is_reported() {
        let got = mem_ref_mismatches(&[0, 5, 2, 9], &[1, 5, 4, 0]);
        assert_eq!(got, vec![(0, 0, 1), (2, 2, 4), (3, 9, 0)]);
    }

    // --- who holds a reference -------------------------------------------

    fn mem(file: usize, off: usize) -> Option<OpenFile> {
        Some(OpenFile::Mem {
            file,
            off,
            writable: false,
        })
    }

    /// Lifetime-generic on purpose: `impl Iterator<Item = &'static OpenFile>`
    /// pins `count_named_slots`' one lifetime parameter to `'static`, which then
    /// outlives every local fd table the tests build.
    fn none<'a>() -> std::iter::Empty<&'a OpenFile> {
        std::iter::empty()
    }

    /// The property the whole change rests on, and the one a single shared
    /// counter could not express: two descriptors on ONE path are one file and
    /// TWO offsets. `mem_file_for` joins by path; `alloc_file_offset` never
    /// joins. Counting both with the same walker must therefore give different
    /// answers for the same fd table -- if it ever gives the same, the offset
    /// has been joined by path and `dup` is no longer distinguishable from a
    /// second `open`.
    #[test]
    fn two_opens_of_one_path_are_one_file_and_two_offsets() {
        // fd 3 and fd 4: `open(p)` twice. fd 5: `dup(3)`, so it shares BOTH.
        let fds = [mem(0, 0), mem(0, 1), mem(0, 0)];
        assert_eq!(
            count_named_slots(std::iter::once(&fds[..]), none(), 1, OpenFile::mem_file),
            vec![3],
            "all three descriptors name the one file"
        );
        assert_eq!(
            count_named_slots(std::iter::once(&fds[..]), none(), 2, OpenFile::mem_offset),
            vec![2, 1],
            "the dup shares the opener's offset; the second open has its own"
        );
    }

    /// A saved fd table belongs to another process and holds references just as
    /// the live one does. This is the holder that a `fork`-shaped bug hides
    /// behind: the child's table is not `self.fds`, so an audit that walked only
    /// the current process would call the child's reference a leak.
    #[test]
    fn every_process_table_counts_not_only_the_live_one() {
        let live = [mem(0, 0)];
        let saved = [mem(0, 0), None];
        let got = count_named_slots(
            [&live[..], &saved[..]].into_iter(),
            none(),
            1,
            OpenFile::mem_offset,
        );
        assert_eq!(got, vec![2], "parent and forked child share one offset");
    }

    /// An entry in flight over SCM_RIGHTS is in NO fd table and still holds its
    /// references -- the queue took them at `sendmsg` so that a receiver cannot
    /// be handed a descriptor whose slot was freed in between.
    #[test]
    fn a_queued_scm_right_counts_too() {
        let queued = [
            OpenFile::Mem {
                file: 0,
                off: 3,
                writable: true,
            },
            OpenFile::Null { zero: false },
        ];
        let got = count_named_slots(std::iter::empty(), queued.iter(), 4, OpenFile::mem_offset);
        assert_eq!(got, vec![0, 0, 0, 1]);
    }

    /// Out-of-range indices are ignored rather than panicking. The audit is a
    /// diagnostic and runs after syscalls that shrink tables; taking the module
    /// down to report a refcount would be worse than the refcount.
    #[test]
    fn an_index_past_the_table_is_ignored() {
        let fds = [mem(9, 9)];
        let got = count_named_slots(std::iter::once(&fds[..]), none(), 2, OpenFile::mem_offset);
        assert_eq!(got, vec![0, 0]);
    }

    /// Only `Mem` descriptors name an offset. A pipe or a socket in the table
    /// must not contribute, or every audit reports a phantom leak.
    #[test]
    fn only_mem_descriptors_name_an_offset() {
        let fds = [
            Some(OpenFile::Pipe {
                idx: 0,
                write: true,
            }),
            Some(OpenFile::Stdio(1)),
            Some(OpenFile::Null { zero: true }),
            mem(0, 0),
        ];
        let got = count_named_slots(std::iter::once(&fds[..]), none(), 1, OpenFile::mem_offset);
        assert_eq!(got, vec![1]);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tls_mod(size: u64, align: u64, offset: u64) -> MuslTlsModule {
        MuslTlsModule {
            image: 0x400000,
            len: size,
            size,
            align,
            offset,
        }
    }

    /// glibc uses `align - 1` as a MASK (`size &= ~(align-1)` in
    /// `allocate_stack`), so a zero align makes it all-ones and masks away
    /// whatever it is applied to. That is the defect this seed exists to
    /// prevent, and it is worth pinning that the floor cannot be zero for ANY
    /// input -- including an image with no thread-locals at all.
    #[test]
    fn the_glibc_static_align_is_never_zero() {
        let (_, align) = glibc_tls_static_geometry(&[]);
        assert!(align > 0, "a zero align becomes an all-ones mask");
        assert_eq!(align, crate::arena::TCB_SIZE);
        let (_, align) = glibc_tls_static_geometry(&[tls_mod(8, 0, 16)]);
        assert!(align > 0);
    }

    /// The static size must COVER the furthest static block, and be a multiple
    /// of the alignment -- glibc rounds the area it allocates by that mask, so
    /// a size that is not a multiple gets truncated below the block it was
    /// supposed to hold.
    #[test]
    fn the_glibc_static_size_covers_the_blocks_and_respects_the_mask() {
        let mods = [tls_mod(0x90, 16, 16), tls_mod(0x40, 64, 0xc0)];
        let (size, align) = glibc_tls_static_geometry(&mods);
        assert_eq!(align, 64, "the alignment is the maximum over the modules");
        assert!(
            size >= 0xc0 + 0x40,
            "static size {size} does not reach the last block at 0xc0+0x40"
        );
        assert_eq!(
            size % align,
            0,
            "size {size} is not a multiple of align {align}"
        );
        // ...and masking it the way allocate_stack does must not shrink it.
        assert_eq!(size & !(align - 1), size);
    }

    /// `tls_size` is the whole per-thread allocation, and it must COVER the
    /// furthest thing `__copy_tls` writes: the DTV, which it places at
    /// `mem + libc.tls_size`. Under-estimating writes past the end of the
    /// mapping `pthread_create` made, which is silent corruption rather than a
    /// crash -- so this asserts the inequality directly, not a formula.
    #[test]
    fn the_seeded_tls_size_covers_everything_a_thread_needs() {
        let pthread_size = 0xc8; // musl aarch64, from .ecv.musltp word 1
        let mods = [tls_mod(0x90, 16, 16), tls_mod(8, 8, 0xb0)];
        let (size, align, cnt) = musl_tls_geometry(&mods, pthread_size);

        let block_end = 0xb0 + 8; // furthest TP-relative byte of any module
        let dtv = (cnt + 1) * 8;
        assert!(
            size >= pthread_size + block_end + dtv,
            "tls_size {size} does not cover pthread({pthread_size}) + blocks({block_end}) + dtv({dtv})"
        );
        assert_eq!(cnt, 2);
        assert_eq!(size % 16, 0, "the allocation must stay 16-byte aligned");
    }

    /// `tls_align` is the maximum over the modules, floored at the TCB gap.
    /// Taking the FIRST module's alignment instead -- an easy mistake, and one
    /// that passes whenever the list happens to be sorted -- gives every thread
    /// a block that is misaligned for whatever the later module holds.
    #[test]
    fn the_seeded_alignment_is_the_maximum_not_the_first() {
        let (_, align, _) = musl_tls_geometry(&[tls_mod(8, 8, 16), tls_mod(0x40, 64, 64)], 0xc8);
        assert_eq!(align, 64);
        // Floor: an image whose modules declare nothing still gets the TCB gap.
        let (_, align, _) = musl_tls_geometry(&[tls_mod(8, 1, 16)], 0xc8);
        assert_eq!(align, crate::arena::TCB_SIZE);
    }

    /// An image with no thread-locals still needs room for the pthread struct
    /// and a one-entry DTV; seeding zero would have `pthread_create` mmap
    /// nothing at all.
    #[test]
    fn an_image_without_thread_locals_still_gets_a_size() {
        let (size, align, cnt) = musl_tls_geometry(&[], 0xc8);
        assert_eq!(cnt, 0);
        assert!(
            size >= 0xc8 + 8,
            "tls_size {size} leaves no room for the pthread struct"
        );
        assert_eq!(align, crate::arena::TCB_SIZE);
    }

    /// The list has to fit the sliver reserved for it below the thread pointer,
    /// and `seed_musl_tls` refuses rather than growing into the pthread struct
    /// and `_dl_stack_*` state that live there. This pins the arithmetic that
    /// decision uses.
    #[test]
    fn the_reserved_module_area_bounds_the_list() {
        let per = MUSL_TLS_MODULE_SIZE;
        let fits = crate::arena::MUSL_TLS_MODULES_MAX / per;
        assert!(
            fits >= 8,
            "only {fits} modules fit; a real closure has more"
        );
        let top = crate::arena::MUSL_TLS_MODULES_VMA + crate::arena::MUSL_TLS_MODULES_MAX;
        assert!(
            top <= crate::arena::THREAD_PTR - 0x800,
            "the module list at {top:#x} reaches the bring-up structures below the thread pointer"
        );
        assert!(
            crate::arena::MUSL_TLS_MODULES_VMA >= crate::arena::LOW_PERPROCESS_FLOOR,
            "the module list must sit inside the range a bounded snapshot carries"
        );
    }

    /// The defect this whole split exists for, in miniature. glibc's
    /// `pthread_create` blocks every signal but SIGSETXID in the STARTING
    /// thread; with one shared mask that left the whole process unable to
    /// receive anything. A process-directed signal must find the thread that
    /// can take it.
    #[test]
    fn a_process_directed_signal_skips_a_thread_that_blocks_it() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Blocked), // leader, parked in a sleep
            task(2, 1, ProcStatus::Blocked), // worker, mid pthread_create
        ];
        procs[0].blocked_on = BlockedOn::Sleep;
        procs[1].blocked_on = BlockedOn::Sleep;
        let sigusr1 = 1u64 << (SIGUSR1 - 1);
        // The worker carries glibc's block-all-but-SIGSETXID; the leader blocks
        // nothing.
        let blocked = [0u64, !(1u64 << 32)];
        assert_eq!(
            signal_wake_candidate(&procs, 1, sigusr1, |i| blocked[i]),
            Some(0),
            "the only thread that can take the signal is the one that does not block it"
        );
        // With the leader blocking it too, nobody may be woken: the signal stays
        // pending until some thread unblocks it. Waking anyway costs a switch
        // and delivers nothing.
        let all_blocked = [sigusr1, !(1u64 << 32)];
        assert_eq!(
            signal_wake_candidate(&procs, 1, sigusr1, |i| all_blocked[i]),
            None
        );
    }

    /// A candidate must be parked in a wait a signal can cut short. Two
    /// non-candidates, for different reasons: a RUNNABLE thread reaches a
    /// delivery boundary on its own, and a `Poll` waiter is woken wholesale by
    /// `wake_pollers` rather than selected here -- picking one here would leave
    /// the group's other pollers asleep on a signal that is theirs to take too.
    #[test]
    fn only_an_interruptible_waiter_is_woken() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Runnable),
            task(2, 1, ProcStatus::Blocked),
        ];
        procs[1].blocked_on = BlockedOn::Poll;
        assert_eq!(signal_wake_candidate(&procs, 1, 1, |_| 0), None);
        procs[1].blocked_on = BlockedOn::Sleep;
        assert_eq!(signal_wake_candidate(&procs, 1, 1, |_| 0), Some(1));
    }

    /// A FUTEX waiter IS a candidate, and this is the assertion that changed
    /// when the futex path got a delivery boundary. It is where every glibc
    /// timed wait parks -- `pthread_cond_timedwait`, `sem_timedwait`, the
    /// condvars -- so excluding it meant a thread waiting on a condvar never ran
    /// a handler at all.
    #[test]
    fn a_futex_waiter_can_take_a_signal() {
        let mut procs = vec![task(1, 1, ProcStatus::Blocked)];
        procs[0].blocked_on = BlockedOn::Futex { uaddr: 0x1000 };
        assert_eq!(signal_wake_candidate(&procs, 1, 1, |_| 0), Some(0));
        // ...but not one it blocks: waking it would cost a switch to learn
        // there is nothing deliverable and re-park.
        assert_eq!(signal_wake_candidate(&procs, 1, 1, |_| 1), None);
    }

    /// A thread-directed signal lands in the TASK's queue, and a delivery pass
    /// that read only the group's would leave every `pthread_kill` pending
    /// forever.
    #[test]
    fn either_queue_can_supply_a_deliverable_signal() {
        let sigusr1 = 1u64 << (SIGUSR1 - 1);
        assert_eq!(deliverable_set(0, sigusr1, 0), sigusr1, "thread-directed");
        assert_eq!(deliverable_set(sigusr1, 0, 0), sigusr1, "process-directed");
        // ...and the mask still wins over both.
        assert_eq!(deliverable_set(sigusr1, sigusr1, sigusr1), 0);
        // A mask that covers an unrelated signal does not hide this one.
        assert_eq!(deliverable_set(0, sigusr1, 1 << 3), sigusr1);
    }

    const SEC: u128 = 1_000_000_000;

    /// A relative sleep is measured from NOW. The failure this pins is the one
    /// that would look like a working sleep: treating a relative request as
    /// absolute gives a deadline a few nanoseconds past the epoch, which is
    /// long gone, so every sleep returns instantly and nothing ever parks.
    #[test]
    fn a_relative_request_is_an_interval_from_now() {
        let now = 1_000 * SEC;
        assert_eq!(
            sleep_deadline(now, false, 0, 100_000_000),
            Some(now + SEC / 10)
        );
        assert_eq!(
            sleep_deadline(now, false, 2, 500_000_000),
            Some(now + 2 * SEC + SEC / 2)
        );
    }

    /// An absolute request names an instant and must ignore `now` entirely --
    /// including one already in the past, which must NOT become "sleep that
    /// long again from here".
    #[test]
    fn an_absolute_request_names_an_instant() {
        let now = 1_000 * SEC;
        assert_eq!(sleep_deadline(now, true, 1_001, 0), Some(1_001 * SEC));
        assert_eq!(
            sleep_deadline(now, true, 10, 0),
            Some(10 * SEC),
            "a past absolute deadline must stay in the past, so the sleep returns at once"
        );
    }

    /// The monotonic clock must not BE the wall clock, and the cheapest way to
    /// see that is its magnitude: a wall clock reads ~1.8e18 ns (the epoch is
    /// over fifty-six years old), a monotonic one reads however long ecvisor has
    /// been up.
    ///
    /// ⚠️ THIS IS THE ONE ASSERTION THAT FIRES ON THE ORIGINAL DEFECT. Point
    /// `mono_nanos` back at `SystemTime::now` -- which is exactly what the whole
    /// runtime used to do -- and the reading is decades, not seconds. The bound
    /// is a year so that a machine with a long-lived process cannot flake it,
    /// and it is still eight orders of magnitude below the epoch.
    #[test]
    fn the_monotonic_clock_counts_from_boot_and_not_from_the_epoch() {
        let a = mono_nanos();
        assert!(
            a < 365 * 24 * 60 * 60 * SEC,
            "mono_nanos() read {a} ns, which is more than a year: it is reporting \
             wall-clock time, not time since this runtime started"
        );
        let b = mono_nanos();
        assert!(b >= a, "monotonic clock went backwards: {a} then {b}");
    }

    /// The clock ids `clock_gettime` used to ignore.
    ///
    /// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? This pins the
    /// TABLE, not the syscall: a `clock_gettime` that went back to ignoring its
    /// argument would still leave this green, because it would never consult the
    /// table at all. The guard that observes the syscall is the clock-STEP test
    /// in `e2e/`; this one exists to pin the two mappings that are judgement
    /// calls and would otherwise drift silently.
    #[test]
    fn clock_ids_map_to_the_timebase_they_name() {
        // The defect, stated as a test: MONOTONIC is not on the wall clock.
        assert_eq!(clock_base(1), Some(ClockBase::Mono));
        assert_eq!(clock_base(0), Some(ClockBase::Real));
        // CPU time is approximated by elapsed monotonic time. Served from the
        // wall clock -- what it used to be -- `clock()` reports the age of the
        // epoch as this process's CPU consumption.
        assert_eq!(clock_base(2), Some(ClockBase::Mono));
        assert_eq!(clock_base(3), Some(ClockBase::Mono));
        // BOOTTIME and the _RAW/_COARSE variants follow their family.
        assert_eq!(clock_base(4), Some(ClockBase::Mono));
        assert_eq!(clock_base(5), Some(ClockBase::Real));
        assert_eq!(clock_base(6), Some(ClockBase::Mono));
        assert_eq!(clock_base(7), Some(ClockBase::Mono));
        // Linux has no CLOCK_SGI_CYCLE on aarch64, and nothing above 11 exists.
        assert_eq!(clock_base(10), None);
        assert_eq!(clock_base(12), None);
        assert_eq!(clock_base(u64::MAX), None);
    }

    /// `to_mono` carries the INTERVAL across, not the instant.
    #[test]
    fn a_realtime_deadline_becomes_the_distance_to_it() {
        // 12:00 tomorrow, asked for at 11:00 today, on a runtime up for 5 s:
        // the guest waits an hour, not until nanosecond 1.8e18 of a counter that
        // started five seconds ago.
        let wall_now = 1_800_000_000 * SEC;
        let wall_deadline = wall_now + 3_600 * SEC;
        let mono_now = 5 * SEC;
        assert_eq!(
            to_mono(wall_deadline, wall_now, mono_now),
            mono_now + 3_600 * SEC
        );
        // Already past: fire at once rather than wrap into a wait of nearly the
        // age of the epoch, which is what an unchecked subtraction would give.
        assert_eq!(to_mono(wall_now - SEC, wall_now, mono_now), mono_now);
        // A relative sleep composes to `mono_now + interval` whichever clock it
        // named, which is why getting the clock wrong is invisible on one.
        let d = sleep_deadline(wall_now, false, 2, 0).unwrap();
        assert_eq!(to_mono(d, wall_now, mono_now), mono_now + 2 * SEC);
    }

    /// Linux validates the timespec before it does anything else, and a guest
    /// passing tv_nsec >= 1e9 expects EINVAL rather than a sleep of whatever
    /// that would carry into tv_sec.
    #[test]
    fn a_malformed_timespec_is_refused() {
        let now = 1_000 * SEC;
        assert_eq!(sleep_deadline(now, false, 0, 1_000_000_000), None);
        assert_eq!(sleep_deadline(now, false, 0, u64::MAX), None);
        // Negative tv_sec arrives as a huge u64; it is a rejected request, not
        // a 584-year sleep.
        assert_eq!(sleep_deadline(now, false, (-1i64) as u64, 0), None);
        // ...and the boundary just below it is legal.
        assert_eq!(
            sleep_deadline(now, false, 0, 999_999_999),
            Some(now + 999_999_999)
        );
    }

    /// glibc's "sleep forever" is a very large tv_sec, and it must land in the
    /// FUTURE. A deadline that wrapped would land in the past and turn an
    /// indefinite sleep into a busy loop -- the exact symptom that hid the
    /// missing syscall for as long as it hid.
    ///
    /// The u128 arithmetic has room for this by construction (i64::MAX seconds
    /// is ~9.2e27 ns against a u128 ceiling of ~3.4e38), so `saturating_add` is
    /// belt-and-braces rather than the thing under test. What IS under test is
    /// that the largest legal request is accepted and stays in the future.
    #[test]
    fn an_enormous_interval_stays_in_the_future() {
        let now = 1_000 * SEC;
        let d = sleep_deadline(now, false, i64::MAX as u64, 0).expect("a legal request");
        assert!(
            d > now,
            "a huge sleep must land in the FUTURE, got {d} against now {now}"
        );
        assert_eq!(d, now + i64::MAX as u128 * SEC);
    }

    /// What an interrupted sleep hands back. glibc's `sleep()` loops on this
    /// remainder, so an over-large value makes an interrupted sleep sleep
    /// LONGER in total than was asked for.
    #[test]
    fn the_remainder_is_the_time_actually_left() {
        let start = 1_000 * SEC;
        assert_eq!(
            remaining_timespec(start + 2 * SEC + 250_000_000, start),
            (2, 250_000_000)
        );
        // Nanoseconds must be a remainder, never a whole second's worth.
        let (_, ns) = remaining_timespec(start + 3 * SEC, start);
        assert!(
            ns < 1_000_000_000,
            "nsec field must be normalised, got {ns}"
        );
    }

    /// A deadline already passed leaves ZERO remaining. Saturating matters: an
    /// unsigned underflow here would hand the guest a ~584-year remainder, and
    /// glibc would loop on it.
    #[test]
    fn a_passed_deadline_leaves_no_remainder() {
        let now = 1_000 * SEC;
        assert_eq!(remaining_timespec(now - SEC, now), (0, 0));
        assert_eq!(remaining_timespec(now, now), (0, 0));
    }

    /// A process table entry with only the fields the thread-group predicates
    /// read; everything else is the None/zero a fresh task carries.
    fn task(pid: u32, tgid: u32, status: ProcStatus) -> Process {
        Process {
            units: Vec::new(),
            dlerror: None,
            pid,
            ppid: 0,
            tgid,
            status,
            started: false,
            prog_idx: 0,
            blocked_on: BlockedOn::None,
            arena: None,
            state: None,
            fds: None,
            cloexec: None,
            nonblock: None,
            cwd: None,
            signals: None,
            task_signals: TaskSignals::default(),
            call_history: None,
            replay: None,
            deadline: None,
            timed_out: false,
            clear_child_tid: 0,
            dumpable: SUID_DUMP_USER,
            thp_disable: THP_NOT_DISABLED,
        }
    }

    /// The single-threaded answer, which is every guest that existed before
    /// threads: the holder of a process's state is that process, and the group
    /// scan must not be able to reach past it into an unrelated pid.
    #[test]
    fn a_lone_process_holds_its_own_state() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Runnable),
            task(2, 2, ProcStatus::Runnable),
        ];
        procs[0].fds = Some(Vec::new());
        procs[1].fds = Some(Vec::new());
        assert_eq!(group_member_where(&procs, 1, |p| p.fds.is_some()), 1);
        // ...and with its own table absent, it must NOT borrow pid 1's.
        procs[1].fds = None;
        assert_eq!(
            group_member_where(&procs, 1, |p| p.fds.is_some()),
            1,
            "a process in its own thread group borrowed another process's fd table"
        );
    }

    /// The case the lookup exists for: the group's fd table was filed under
    /// whichever thread last ran, and a switch into a different thread has to
    /// find it there. Taking `idx`'s own (absent) entry is what would silently
    /// give a worker thread an empty descriptor table.
    #[test]
    fn a_thread_finds_the_table_its_sibling_filed() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Runnable), // leader
            task(7, 1, ProcStatus::Runnable), // worker
            task(9, 9, ProcStatus::Runnable), // an unrelated process, also holding one
        ];
        procs[0].fds = Some(Vec::new());
        procs[2].fds = Some(Vec::new());
        assert_eq!(group_member_where(&procs, 1, |p| p.fds.is_some()), 0);
        // And symmetrically once the worker is the one that filed it.
        procs[0].fds = None;
        procs[1].fds = Some(Vec::new());
        assert_eq!(group_member_where(&procs, 0, |p| p.fds.is_some()), 1);
    }

    /// A dead sibling is still a valid holder. It is where the buffer was filed
    /// when the thread exited, and refusing to look there would lose the whole
    /// group's memory the first time a worker finished.
    #[test]
    fn a_retired_thread_is_still_a_valid_holder() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Runnable),
            task(7, 1, ProcStatus::Dead),
        ];
        procs[1].fds = Some(Vec::new());
        assert_eq!(group_member_where(&procs, 0, |p| p.fds.is_some()), 1);
    }

    /// The arena may be released only when the whole group is gone -- dropping it
    /// on the first thread's exit would delete an address space its siblings are
    /// still executing in. A zombie does not keep a group alive; an unreaped
    /// zombie leader does keep it addressable for `exit`'s group/thread decision,
    /// which is why the two counts differ.
    #[test]
    fn a_group_dies_only_with_its_last_live_task() {
        // A zombie leader with a worker still running: the arena is in use.
        let procs = vec![
            task(1, 1, ProcStatus::Zombie(ExitReason::Exited(0))),
            task(7, 1, ProcStatus::Runnable),
            task(8, 1, ProcStatus::Dead),
        ];
        assert!(
            !group_is_dead(&procs, 1),
            "the group's address space was released while a thread was still \
             executing in it"
        );
        // ...and the same table says the group is multi-tasked, so this is the
        // configuration that separates the two counts. If `group_is_dead` were
        // written on the unreaped count it would agree here and disagree below.
        assert!(group_is_multithreaded(&procs, 1));

        // Every worker gone, leader a zombie awaiting wait4. Nothing is
        // executing, so the arena may go -- but the slot is still occupied, so
        // the group is not "multi-threaded" and an exit here is the process's.
        let procs = vec![
            task(1, 1, ProcStatus::Zombie(ExitReason::Exited(0))),
            task(7, 1, ProcStatus::Dead),
        ];
        assert!(
            group_is_dead(&procs, 1),
            "a zombie leader with no live thread still pinned the arena"
        );
        assert!(
            !group_is_multithreaded(&procs, 1),
            "a reaped worker still counted as a sibling"
        );
    }

    // ---- prctl(PR_SET_DUMPABLE / PR_GET_DUMPABLE) ----
    //
    // The whole ruling is "accept what Linux accepts, store it, and answer the
    // GET with it". Both halves are pinned against a MEASURED kernel (Linux
    // 6.17), not against a reading of the manual page.

    /// Measured: `prctl(PR_SET_DUMPABLE, v)` returns 0 for v in {0, 1} and
    /// EINVAL for 2 and 3. Two (`SUID_DUMP_ROOT`) is the one that looks
    /// plausible and is not -- the kernel sets it, userspace may not, so an
    /// implementation written as "0..=2" or as a truthiness test would accept a
    /// request Linux refuses and record a value `PR_GET_DUMPABLE` would then
    /// report back.
    #[test]
    fn dumpable_accepts_exactly_the_values_linux_accepts() {
        assert!(dumpable_arg_permitted(0), "SUID_DUMP_DISABLE was refused");
        assert!(dumpable_arg_permitted(1), "SUID_DUMP_USER was refused");
        assert!(
            !dumpable_arg_permitted(2),
            "SUID_DUMP_ROOT was accepted; Linux gives EINVAL (measured 6.17)"
        );
        assert!(!dumpable_arg_permitted(3));
        assert!(!dumpable_arg_permitted(u64::MAX));
    }

    /// ⚠️ THE HALF THAT IS EASY TO GET WRONG, and the reason the check takes a
    /// `u64`. `usize` is 32 bits on wasm32, so a rule written on `arg as usize`
    /// (or `as u32`) folds `0x1_0000_0001` onto 1 and accepts it. Measured on
    /// Linux 6.17: `prctl(PR_SET_DUMPABLE, 0x100000001)` is EINVAL, because the
    /// kernel compares the full `unsigned long`.
    #[test]
    fn a_dumpable_argument_wider_than_32_bits_is_refused() {
        assert!(
            !dumpable_arg_permitted(0x1_0000_0001),
            "a 64-bit argument was truncated onto SUID_DUMP_USER"
        );
        assert!(
            !dumpable_arg_permitted(0x1_0000_0000),
            "a 64-bit argument was truncated onto SUID_DUMP_DISABLE"
        );
    }

    /// `PR_GET_DUMPABLE` must report what `PR_SET_DUMPABLE` stored -- accepting
    /// the SET and then answering the GET with something else would trade one
    /// divergence for a worse one.
    #[test]
    fn a_dumpable_set_is_what_the_get_reads_back() {
        let mut procs = vec![task(1, 1, ProcStatus::Runnable)];
        assert_eq!(
            procs[0].dumpable, SUID_DUMP_USER,
            "a process that never called prctl did not read Linux's boot value"
        );
        set_group_dumpable(&mut procs, 0, SUID_DUMP_DISABLE);
        assert_eq!(procs[0].dumpable, SUID_DUMP_DISABLE);
        set_group_dumpable(&mut procs, 0, SUID_DUMP_USER);
        assert_eq!(procs[0].dumpable, SUID_DUMP_USER);
    }

    /// The flag is per-MM in Linux, and a thread group shares one MM: measured
    /// on 6.17, a `PR_SET_DUMPABLE(0)` issued by a worker thread is what the
    /// main thread's `PR_GET_DUMPABLE` reads. A per-task write would leave the
    /// sibling reporting the value nobody set.
    ///
    /// The unrelated process in the table is the other half: writing the whole
    /// table would pass the sibling assertion and silently change a process
    /// that never called prctl.
    #[test]
    fn a_dumpable_set_by_one_thread_is_read_by_its_siblings() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Runnable), // leader
            task(7, 1, ProcStatus::Runnable), // worker: the one that calls prctl
            task(9, 1, ProcStatus::Dead),     // a retired sibling
            task(4, 4, ProcStatus::Runnable), // an unrelated process
        ];
        set_group_dumpable(&mut procs, 1, SUID_DUMP_DISABLE);
        assert_eq!(
            procs[0].dumpable, SUID_DUMP_DISABLE,
            "a worker's PR_SET_DUMPABLE was invisible to its own thread group"
        );
        assert_eq!(procs[1].dumpable, SUID_DUMP_DISABLE);
        assert_eq!(procs[2].dumpable, SUID_DUMP_DISABLE);
        assert_eq!(
            procs[3].dumpable, SUID_DUMP_USER,
            "an unrelated process's dumpable flag was overwritten"
        );
    }

    /// ⚠️ `PR_SET_THP_DISABLE` DOES NOT HAVE PR_SET_DUMPABLE'S SHAPE, and
    /// copying that shape is the mistake this guards. The kernel is literally
    /// `if (arg2) set_bit(...) else clear_bit(...)` -- no value validation at
    /// all. Measured on Linux 6.17: `prctl(41, 2)`, `prctl(41, 3)` and
    /// `prctl(41, -1)` each return 0 and each make `PR_GET_THP_DISABLE` read 1,
    /// where `prctl(PR_SET_DUMPABLE, 2)` is EINVAL.
    ///
    /// An implementation that stored the raw argument instead of its truthiness
    /// would pass every "did the SET succeed" check and then answer the GET with
    /// 2, which is a value the bit cannot hold.
    #[test]
    fn thp_disable_stores_the_truthiness_of_any_argument() {
        assert_eq!(thp_disable_value(0), 0, "0 must clear the bit");
        assert_eq!(thp_disable_value(1), 1);
        assert_eq!(
            thp_disable_value(2),
            1,
            "2 was refused or stored raw; Linux stores 1 (measured 6.17)"
        );
        assert_eq!(thp_disable_value(3), 1);
        assert_eq!(thp_disable_value(u64::MAX), 1);
    }

    /// ⚠️ THE CASE WHERE `usize` CHANGES THE ANSWER RATHER THAN JUST WIDENING
    /// IT. `usize` is 32-bit on wasm32, so `arg2 as usize` folds `0x1_0000_0000`
    /// onto 0 -- and because this option is a truthiness test, that does not
    /// merely refuse a value, it CLEARS the flag on a call that sets it.
    /// Measured on Linux 6.17: `prctl(41, 0x100000000)` returns 0 and the GET
    /// then reads 1.
    #[test]
    fn a_thp_disable_argument_wider_than_32_bits_still_sets_the_bit() {
        assert_eq!(
            thp_disable_value(0x1_0000_0000),
            1,
            "a 64-bit argument truncated to zero and CLEARED the flag"
        );
        assert_eq!(thp_disable_value(0x1_0000_0001), 1);
    }

    /// The two reserved-argument rules, which are DIFFERENT for the setter and
    /// the getter -- `PR_SET_THP_DISABLE` reserves arg3..arg5 and
    /// `PR_GET_THP_DISABLE` reserves arg2..arg5, because the getter has no value
    /// to take. Measured on Linux 6.17: `prctl(41, 1, 1, 0, 0)` is EINVAL and so
    /// is `prctl(42, 1, 0, 0, 0)`, while `prctl(41, 1, 0, 0, 0)` is 0.
    ///
    /// Writing one rule for both is the plausible error: it would either refuse
    /// ruby's `prctl(41, 1, 0, 0, 0)` or accept a `PR_GET_THP_DISABLE(1)` Linux
    /// refuses.
    #[test]
    fn thp_disable_reserves_different_arguments_for_set_and_get() {
        assert!(
            thp_disable_set_permitted(0, 0, 0),
            "ruby's call was refused"
        );
        assert!(!thp_disable_set_permitted(1, 0, 0));
        assert!(!thp_disable_set_permitted(0, 1, 0));
        assert!(!thp_disable_set_permitted(0, 0, 1));
        assert!(!thp_disable_set_permitted(0, 0, 0x1_0000_0000));

        assert!(thp_disable_get_permitted(0, 0, 0, 0));
        assert!(
            !thp_disable_get_permitted(1, 0, 0, 0),
            "the getter accepted a non-zero arg2; Linux gives EINVAL (measured 6.17)"
        );
        assert!(!thp_disable_get_permitted(0, 1, 0, 0));
        assert!(!thp_disable_get_permitted(0, 0, 1, 0));
        assert!(!thp_disable_get_permitted(0, 0, 0, 1));
    }

    /// `MMF_DISABLE_THP` is an mm flag, so it behaves like `dumpable` across a
    /// thread group: measured on 6.17, a worker thread's
    /// `PR_SET_THP_DISABLE(1)` is what the main thread's GET reads.
    ///
    /// The unrelated process and the *dumpable* assertions are the other half:
    /// writing the whole table, or writing the wrong field, would each pass a
    /// check that only looked at the worker's own group and own flag.
    #[test]
    fn a_thp_disable_set_by_one_thread_is_read_by_its_siblings() {
        let mut procs = vec![
            task(1, 1, ProcStatus::Runnable), // leader
            task(7, 1, ProcStatus::Runnable), // worker: the one that calls prctl
            task(9, 1, ProcStatus::Dead),     // a retired sibling
            task(4, 4, ProcStatus::Runnable), // an unrelated process
        ];
        assert_eq!(
            procs[0].thp_disable, THP_NOT_DISABLED,
            "a process that never called prctl did not read Linux's boot value"
        );
        // ⚠️ dumpable is moved OFF the value this test writes first. It starts
        // at SUID_DUMP_USER, which is 1 -- the same 1 `set_group_thp_disable`
        // stores -- so the "did it write the wrong field" assertion below cannot
        // fire while the two agree. Neutralized: with `p.dumpable = value` added
        // to the group write, this test passed until the line below was added.
        set_group_dumpable(&mut procs, 1, SUID_DUMP_DISABLE);
        set_group_thp_disable(&mut procs, 1, 1);
        assert_eq!(
            procs[0].thp_disable, 1,
            "a worker's PR_SET_THP_DISABLE was invisible to its own thread group"
        );
        assert_eq!(procs[1].thp_disable, 1);
        assert_eq!(procs[2].thp_disable, 1);
        assert_eq!(
            procs[3].thp_disable, THP_NOT_DISABLED,
            "an unrelated process's THP flag was overwritten"
        );
        assert_eq!(
            procs[0].dumpable, SUID_DUMP_DISABLE,
            "set_group_thp_disable wrote the dumpable flag"
        );
        set_group_thp_disable(&mut procs, 0, THP_NOT_DISABLED);
        assert_eq!(
            procs[1].thp_disable, THP_NOT_DISABLED,
            "the bit never cleared"
        );
    }

    /// `exit` retires one thread and `exit_group` the whole process, and the
    /// runtime distinguishes them by whether the caller has a sibling. Getting
    /// this backwards for the LAST thread would leave a process that never
    /// closes its descriptors and never notifies its parent.
    #[test]
    fn the_last_task_standing_makes_exit_a_group_exit() {
        let solo = vec![task(1, 1, ProcStatus::Runnable)];
        assert!(!group_is_multithreaded(&solo, 1));

        let threaded = vec![
            task(1, 1, ProcStatus::Runnable),
            task(7, 1, ProcStatus::Runnable),
        ];
        assert!(
            group_is_multithreaded(&threaded, 1),
            "a worker's exit would have closed the whole process's descriptors"
        );

        // Workers finished; the leader's own exit is the process's exit.
        let drained = vec![
            task(1, 1, ProcStatus::Runnable),
            task(7, 1, ProcStatus::Dead),
        ];
        assert!(
            !group_is_multithreaded(&drained, 1),
            "the last task's exit would never have notified the parent"
        );
    }

    /// A layout that checks out gets the write. `_dl_pagesize` at 65536 is what
    /// aarch64 glibc's static initialiser leaves in a fused image (EXEC_PAGESIZE,
    /// never overwritten because ld.so never reads AT_PAGESZ), so this is the
    /// real configuration and not a contrived one.
    #[test]
    fn a_confirmed_layout_gets_the_minsigstacksize_seed() {
        let mut arena = Arena::new();
        let field = crate::arena::THREAD_PTR + 0x2000;
        arena
            .slice_mut(field - 8, 8)
            .copy_from_slice(&65536u64.to_le_bytes());
        assert_eq!(
            seed_dl_minsigstacksize(&mut arena, field),
            Some(crate::arena::MINSIGSTKSZ)
        );
        assert_eq!(
            u64::from_le_bytes(arena.slice(field, 8).try_into().unwrap()),
            crate::arena::MINSIGSTKSZ,
            "the field was reported written but holds something else"
        );
    }

    /// Every refusal. The address is decoded out of glibc's own code by the
    /// prelinker, and a wrong one writes 5120 into an unrelated member of
    /// `_rtld_global_ro` -- which is silent, and would surface as glibc
    /// misbehaving somewhere with no connection to this.
    #[test]
    fn an_unconfirmed_layout_is_left_alone() {
        let field = crate::arena::THREAD_PTR + 0x2000;
        let setup = |pagesize: u64, cur: u64| {
            let mut a = Arena::new();
            a.slice_mut(field - 8, 8)
                .copy_from_slice(&pagesize.to_le_bytes());
            a.slice_mut(field, 8).copy_from_slice(&cur.to_le_bytes());
            a
        };
        // The preceding word is not a page size, so the offset did not land
        // where the prelinker thinks it did.
        for bad in [0u64, 1, 1234, 100_000, 0x1_0000_0000] {
            let mut a = setup(bad, 0);
            assert!(
                seed_dl_minsigstacksize(&mut a, field).is_none(),
                "wrote next to a _dl_pagesize of {bad}"
            );
            assert_eq!(
                u64::from_le_bytes(a.slice(field, 8).try_into().unwrap()),
                0,
                "refused and wrote anyway"
            );
        }
        // Already set: whoever set it knows more than we do.
        let mut a = setup(65536, 8192);
        assert!(seed_dl_minsigstacksize(&mut a, field).is_none());
        assert_eq!(
            u64::from_le_bytes(a.slice(field, 8).try_into().unwrap()),
            8192,
            "an existing value was overwritten"
        );
        // No descriptor, and addresses that would index out of the arena.
        let mut a = Arena::new();
        for addr in [0u64, 4, u64::MAX] {
            assert!(
                seed_dl_minsigstacksize(&mut a, addr).is_none(),
                "accepted {addr:#x}"
            );
        }
        // The field ITSELF must be in bounds, not just the `_dl_pagesize` word
        // before it. This is the case a check on the preceding word alone lets
        // through: the page size reads fine, and the write is what runs off the
        // end -- a host panic rather than a refusal.
        let end = crate::arena::MEMORY_ARENA_VMA + crate::arena::MEMORY_ARENA_SIZE as u64;
        let mut a = Arena::new();
        a.slice_mut(end - 8, 8)
            .copy_from_slice(&65536u64.to_le_bytes());
        assert!(
            seed_dl_minsigstacksize(&mut a, end).is_none(),
            "accepted a field one word past the end of the arena"
        );
    }

    /// A zeroed thread struct must come back circular, because that is what
    /// terminates musl's `while ((td = td->next) != self)` walks.
    #[test]
    fn seeding_closes_the_musl_thread_list_on_itself() {
        let mut arena = Arena::new();
        let base = crate::arena::THREAD_PTR - 0xc8;
        assert!(seed_musl_thread_list(&mut arena, base));
        let read = |a: &Arena, at: u64| u64::from_le_bytes(a.slice(at, 8).try_into().unwrap());
        for (off, what) in [(0u64, "self"), (8, "prev"), (16, "next")] {
            assert_eq!(
                read(&arena, base + off),
                base,
                "{what} must point at the thread struct itself; a zero here is an \
                 infinite, syscall-free loop in any musl thread-list walk"
            );
        }
    }

    /// Seeding must never RELINK. A forked child inherits a linked arena, and a
    /// second pass over it would drop whatever the guest had built.
    #[test]
    fn seeding_leaves_an_already_linked_list_alone() {
        let mut arena = Arena::new();
        let base = crate::arena::THREAD_PTR - 0xc8;
        let other = base + 0x400;
        arena
            .slice_mut(base + 16, 8)
            .copy_from_slice(&other.to_le_bytes());
        assert!(
            !seed_musl_thread_list(&mut arena, base),
            "a list with a non-null next was re-seeded"
        );
        let read = |a: &Arena, at: u64| u64::from_le_bytes(a.slice(at, 8).try_into().unwrap());
        assert_eq!(read(&arena, base + 16), other, "next was overwritten");
    }
}

#[cfg(test)]
mod sched_tests {
    use super::*;
    use crate::net::{NetBackend, SockAddr};

    /// A context with one runnable process and nothing else.
    ///
    /// ⚠️ THIS IS NEW GROUND. Until the backend seam and the scheduler split,
    /// none of the code below was reachable from `cargo test` at all: the idle
    /// path called a wasm import through `sys` (gated to `wasm32`), and its
    /// terminal states called `std::process::exit`, which ends the test binary
    /// rather than returning a value anything can assert on.
    fn ctx() -> EcvContext {
        static NAME: &[u8] = b"t\0";
        let progs = Programs::load(
            vec![Box::leak(Box::new(EcvProgram::for_test_with_tables(NAME)))],
            None,
        );
        let mut c = EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            b"/".to_vec(),
            0,
            0,
            progs,
            0,
        );
        // `EcvContext::new` leaves `live_state` null; `entry.rs` points it at
        // the boot `State`. `load_current` copies into it, so a test that
        // selects a process needs a real one.
        c.live_state = Box::leak(State::new_boxed()) as *mut State;
        c
    }

    /// Files the current task away, as `retire_after_suspend` does before any
    /// selection. Without it the current process has no saved `state` and
    /// `load_current` unwraps a `None` -- which is not a bug in the scheduler
    /// but a test that skipped half the sequence.
    fn retire(c: &mut EcvContext) {
        c.save_current();
    }

    #[test]
    fn a_runnable_process_is_selected() {
        let mut c = ctx();
        retire(&mut c);
        c.run_queue.push_back(0);
        assert_eq!(c.pick_next(), SchedOutcome::Ready);
    }

    #[test]
    fn nothing_left_to_run_is_an_exit_carrying_inits_code() {
        let mut c = ctx();
        c.procs[0].status = ProcStatus::Zombie(ExitReason::Exited(7));
        c.exit_code = 7;
        assert_eq!(c.pick_next(), SchedOutcome::Exited(7));
    }

    /// The one the old design could not express.
    ///
    /// Every process blocked with no wake source is a DEADLOCK, not completion.
    /// Reporting it as `Exited(0)` made every hang look like a clean run -- a
    /// guest that stopped on its first `clock_nanosleep` presented as "the
    /// program printed nothing". Before this split the distinction existed only
    /// as two different `std::process::exit` calls, which no test could observe.
    #[test]
    fn everything_blocked_with_no_wake_source_is_a_deadlock_not_an_exit() {
        let mut c = ctx();
        c.procs[0].status = ProcStatus::Blocked;
        c.procs[0].blocked_on = BlockedOn::Wait;
        let got = c.pick_next();
        assert_eq!(got, SchedOutcome::Deadlock);
        assert_ne!(
            got,
            SchedOutcome::Exited(0),
            "a hang must not be reported as a clean run"
        );
    }

    #[test]
    fn a_pending_deadline_is_idle_with_a_wake_time_not_a_deadlock() {
        let mut c = ctx();
        c.procs[0].status = ProcStatus::Blocked;
        c.procs[0].blocked_on = BlockedOn::Sleep;
        let due = mono_nanos() + 60_000_000_000;
        c.procs[0].deadline = Some(due);
        match c.pick_next() {
            SchedOutcome::Idle { wake_at, io } => {
                assert_eq!(
                    wake_at,
                    Some(due),
                    "the host needs the deadline to time its callback"
                );
                assert!(!io, "nothing is parked on a socket");
            }
            other => panic!("expected Idle, got {other:?}"),
        }
    }

    /// ⚠️ "Everything is blocked on a socket" is NOT terminal. That was the
    /// blocking-only bug's failure mode: readiness arrives from outside, so the
    /// run can still progress and the scheduler must say Idle, not Deadlock.
    #[test]
    fn a_socket_waiter_is_idle_not_a_deadlock_even_with_no_deadline() {
        let mut c = ctx();
        let h = c.net.socket(false, false).expect("loopback socket");
        c.procs[0].status = ProcStatus::Blocked;
        c.procs[0].blocked_on = BlockedOn::Socket {
            h,
            write: false,
            poll: false,
        };
        match c.pick_next() {
            SchedOutcome::Idle { wake_at, io } => {
                assert!(io, "the host must be told to watch for readiness");
                assert_eq!(wake_at, None);
            }
            other => panic!("expected Idle, got {other:?}"),
        }
    }

    /// The spin guard.
    ///
    /// `resume_scheduling` drives `pick_next` in a loop, waiting in the backend
    /// between passes. With an in-process backend a socket that can never become
    /// ready would re-select the same Idle state forever -- 100% CPU with no
    /// diagnosis. A timeout with NO deadline means nothing can change, so it
    /// must terminate as a deadlock instead.
    ///
    /// Without this the failure is not a wrong answer but a hang, which is the
    /// hardest kind to attribute.
    #[test]
    fn an_unreachable_socket_wake_terminates_instead_of_spinning() {
        let mut c = ctx();
        let h = c.net.socket(false, false).expect("loopback socket");
        c.procs[0].status = ProcStatus::Blocked;
        c.procs[0].blocked_on = BlockedOn::Socket {
            h,
            write: false,
            poll: false,
        };
        assert_eq!(c.resume_scheduling(), SchedOutcome::Deadlock);
    }

    /// A ready socket must wake its waiter rather than report Idle.
    #[test]
    fn resume_scheduling_wakes_a_waiter_whose_socket_became_readable() {
        let mut c = ctx();
        // A connected loopback pair, with bytes waiting on the server end.
        let ln = c.net.socket(false, false).unwrap();
        c.net.bind(ln, &SockAddr::v4([127, 0, 0, 1], 80)).unwrap();
        c.net.listen(ln, 4).unwrap();
        let cl = c.net.socket(false, false).unwrap();
        c.net
            .connect(cl, &SockAddr::v4([127, 0, 0, 1], 80))
            .unwrap();
        let sv = c.net.accept(ln).unwrap();
        c.net.send(cl, b"hi").unwrap();

        c.procs[0].status = ProcStatus::Blocked;
        c.procs[0].blocked_on = BlockedOn::Socket {
            poll: false,
            h: sv,
            write: false,
        };
        retire(&mut c);
        assert_eq!(c.resume_scheduling(), SchedOutcome::Ready);
        assert_eq!(c.procs[0].status, ProcStatus::Runnable);
    }

    // ---- Default signal actions -------------------------------------------
    //
    // `terminating_signal` decides; these check that the three places which ASK
    // it act on the answer. Every one of them is a place a signal used to be
    // recorded as pending and then consumed by nothing.

    const SIGTERM: u32 = 15;
    /// What a shell reports -- and what the MODULE exits with when init takes an
    /// uncaught SIGTERM -- for a SIGTERM.
    ///
    /// ⚠️ NOT what `Pending::Exit` carries any more, and the split is the point.
    /// `Pending::Exit` carries `TERM_REASON`, from which this is derived by
    /// `status_code`; what `wait4` hands the parent is the unrelated
    /// `WIFSIGNALED` encoding. This constant used to be all three at once.
    const TERM_STATUS: i32 = 128 + SIGTERM as i32;
    /// What `Pending::Exit` and `ProcStatus::Zombie` actually carry for an
    /// uncaught SIGTERM: the signal number, so the parent's `wait4` can say
    /// "killed" rather than "exited 143".
    const TERM_REASON: ExitReason = ExitReason::Killed(SIGTERM);

    fn sigbit(sig: u32) -> u64 {
        1u64 << (sig - 1)
    }

    /// Adds a second, unrelated single-threaded process, blocked on `on`.
    ///
    /// Deliberately NOT via `fork_current`: a fork needs an arena snapshot and a
    /// replay point, and none of the wake logic reads either.
    fn add_blocked_proc(c: &mut EcvContext, pid: u32, on: BlockedOn) -> usize {
        let mut p = Process {
            dlerror: None,
            units: Vec::new(),
            pid,
            ppid: 1,
            tgid: pid,
            status: ProcStatus::Blocked,
            started: true,
            prog_idx: 0,
            blocked_on: on,
            arena: None,
            state: None,
            fds: None,
            cloexec: None,
            nonblock: None,
            cwd: None,
            signals: None,
            task_signals: TaskSignals::default(),
            call_history: None,
            replay: None,
            deadline: None,
            timed_out: false,
            clear_child_tid: 0,
            dumpable: SUID_DUMP_USER,
            thp_disable: THP_NOT_DISABLED,
        };
        p.signals = Some(SignalState::default());
        c.procs.push(p);
        c.procs.len() - 1
    }

    /// A delivery boundary is where Linux would perform the default action, so
    /// it is where this arms the exit -- with the SAME `Pending::Exit` an
    /// `exit_group` raises, so the teardown that follows is the existing one.
    #[test]
    fn an_uncaught_sigterm_at_a_delivery_boundary_arms_the_exit() {
        let mut c = ctx();
        c.signals.pending |= sigbit(SIGTERM);
        let ran = unsafe { c.deliver_pending_signals(c.task_signals.blocked) };
        assert_eq!(ran, 0, "no handler is installed, so none can have run");
        assert!(
            matches!(c.pending, Pending::Exit(TERM_REASON)),
            "expected Exit({TERM_REASON:?}), got {:?}",
            c.pending
        );
        assert!(
            c.suspended,
            "the leg must unwind for the exit to be applied"
        );
    }

    /// ⚠️ Guards the NON-CONSUMPTION the recovery below depends on. `ppoll`,
    /// `epoll_pwait` and `rt_sigsuspend` all call `deliver_pending_signals` and
    /// may then call `block_current`, which overwrites `Pending::Exit`. The
    /// pending bit is what `retire_after_suspend` re-derives the ruling from, so
    /// consuming it here would silently restore the old "parks forever" bug at
    /// exactly those three call sites.
    #[test]
    fn arming_the_exit_leaves_the_signal_pending_for_the_pre_block_check() {
        let mut c = ctx();
        c.signals.pending |= sigbit(SIGTERM);
        unsafe { c.deliver_pending_signals(c.task_signals.blocked) };
        assert_ne!(
            c.signals.pending & sigbit(SIGTERM),
            0,
            "the bit was consumed; a boundary that blocks afterwards would lose the ruling"
        );
    }

    /// The `signal_pending()` test at the top of every interruptible sleep. A
    /// parked task is only re-entered when something wakes it, so a process that
    /// parks after taking a fatal signal does not die late -- it never dies.
    ///
    /// `PipeRead` on purpose: nothing ever wakes it here, so if the process is
    /// left Blocked the run is over.
    #[test]
    fn a_terminating_signal_forbids_the_process_from_parking() {
        let mut c = ctx();
        c.signals.pending |= sigbit(SIGTERM);
        c.block_current(BlockedOn::PipeRead(0));
        c.retire_after_suspend();
        assert_eq!(
            c.procs[0].status,
            ProcStatus::Zombie(TERM_REASON),
            "a task with a terminating signal due parked instead of dying"
        );
    }

    /// ...and the same check must NOT fire for a signal that has no terminating
    /// default action, or every blocking syscall in a guest holding a pending
    /// SIGCHLD would turn into an exit.
    #[test]
    fn an_ignored_signal_does_not_forbid_parking() {
        let mut c = ctx();
        c.signals.pending |= sigbit(SIGCHLD);
        c.block_current(BlockedOn::PipeRead(0));
        c.retire_after_suspend();
        assert_eq!(c.procs[0].status, ProcStatus::Blocked);
    }

    /// The kill point: a task selected to run must not execute one more guest
    /// instruction. This is what covers a task the signal woke out of a wait its
    /// syscall-resume would otherwise re-enter.
    #[test]
    fn a_process_with_a_terminating_signal_is_retired_instead_of_run() {
        let mut c = ctx();
        retire(&mut c);
        // AFTER `retire`: `save_current` moved the live disposition table into
        // the process entry, and `load_current` will move it back -- so a bit set
        // on the live one here would be overwritten by the very switch under test.
        c.procs[0].signals.as_mut().unwrap().pending |= sigbit(SIGTERM);
        c.run_queue.push_back(0);
        let got = c.pick_next();
        assert_ne!(
            got,
            SchedOutcome::Ready,
            "the process was handed the CPU with a terminating signal due"
        );
        assert_eq!(c.procs[0].status, ProcStatus::Zombie(TERM_REASON));
    }

    /// And the module's own status follows, because a signal death is an exit:
    /// init killed by SIGTERM leaves 143, which is what a shell reports.
    #[test]
    fn init_killed_by_a_signal_exits_the_module_with_128_plus_the_signal() {
        let mut c = ctx();
        retire(&mut c);
        c.procs[0].signals.as_mut().unwrap().pending |= sigbit(SIGTERM);
        c.run_queue.push_back(0);
        assert_eq!(c.pick_next(), SchedOutcome::Exited(TERM_STATUS));
        assert_eq!(c.exit_code, TERM_STATUS);
    }

    // ---- The wait status word ---------------------------------------------
    //
    // `sys_wait4` writes `ExitReason::wait_status()` through the guest's
    // `wstatus` pointer and does nothing else with it, so this IS the encoding
    // a guest sees. `mod sys` is `#[cfg(target_arch = "wasm32")]` and its
    // `#[test]`s would never run, which is why the encoding is a function here.
    //
    // The four helpers below are TRANSCRIPTIONS of glibc's `<bits/waitstatus.h>`,
    // written out rather than reused so the test asserts against the macros the
    // guest actually expands, not against this runtime's idea of them. Getting
    // one wrong is the way this whole file could pass while a real guest reads
    // the opposite answer -- `__WIFSIGNALED` in particular is NOT the tautology
    // `(s & 0x7f) != 0`, and writing it that way would hide the 0x7f case.

    /// `__WIFEXITED(s)`: `__WTERMSIG(s) == 0`.
    fn wifexited(s: u32) -> bool {
        s & 0x7f == 0
    }
    /// `__WEXITSTATUS(s)`: `((s) & 0xff00) >> 8`.
    fn wexitstatus(s: u32) -> u32 {
        (s & 0xff00) >> 8
    }
    /// `__WTERMSIG(s)`: `(s) & 0x7f`.
    fn wtermsig(s: u32) -> u32 {
        s & 0x7f
    }
    /// `__WIFSIGNALED(s)`: `((signed char) (((s) & 0x7f) + 1) >> 1) > 0`.
    ///
    /// The cast is applied to the SUM and the shift is arithmetic, which is what
    /// makes `0x7f` -- `WIFSTOPPED`'s marker -- come out false: `0x7f + 1` is
    /// `0x80`, `-128` as a signed char, and `-128 >> 1` is `-64`.
    fn wifsignaled(s: u32) -> bool {
        let v = (((s & 0x7f) + 1) as u8) as i8;
        (v as i32) >> 1 > 0
    }
    /// `__WIFSTOPPED(s)`: `((s) & 0xff) == 0x7f`.
    fn wifstopped(s: u32) -> bool {
        s & 0xff == 0x7f
    }
    /// `__WCOREDUMP(s)`: `(s) & __WCOREFLAG`, and `__WCOREFLAG` is `0x80`.
    fn wcoredump(s: u32) -> bool {
        s & 0x80 != 0
    }

    /// The helpers above, checked against the three status words whose meaning
    /// is fixed by the kernel, so a mis-transcribed macro fails HERE rather than
    /// silently agreeing with a mis-encoded `wait_status`.
    ///
    /// ⚠️ Without this, `wifsignaled` written as `(s & 0x7f) != 0` would pass
    /// every other test in this section: no test below constructs `0x7f`,
    /// because `wait_status` never produces it. The three literals are the
    /// independent oracle.
    #[test]
    fn the_wait_macros_agree_with_the_kernels_own_encodings() {
        // A normal exit(0).
        assert!(wifexited(0x0000) && !wifsignaled(0x0000) && !wifstopped(0x0000));
        // exit(7): high byte carries the code, low 7 bits clear.
        assert!(wifexited(0x0700) && !wifsignaled(0x0700));
        assert_eq!(wexitstatus(0x0700), 7);
        // Killed by SIGTERM.
        assert!(wifsignaled(0x000f) && !wifexited(0x000f) && !wifstopped(0x000f));
        assert_eq!(wtermsig(0x000f), 15);
        // Stopped by SIGSTOP: 0x7f in the low byte. NEITHER exited nor
        // signalled -- this is the value that makes `WIFSIGNALED` non-trivial.
        assert!(wifstopped(0x137f));
        assert!(!wifsignaled(0x137f) && !wifexited(0x137f));
    }

    /// A normal exit round-trips: the parent reads `WIFEXITED` and gets its code
    /// back, and it is NOT mistakable for a signal death.
    #[test]
    fn a_normal_exit_round_trips_through_wifexited() {
        for code in [0, 1, 7, 42, 127, 255] {
            let s = ExitReason::Exited(code).wait_status();
            assert!(wifexited(s), "exit({code}) -> {s:#06x} is not WIFEXITED");
            assert!(
                !wifsignaled(s),
                "exit({code}) -> {s:#06x} reads as a signal death"
            );
            assert!(!wifstopped(s), "exit({code}) -> {s:#06x} reads as stopped");
            assert_eq!(wexitstatus(s), code as u32, "exit({code}) -> {s:#06x}");
            assert!(!wcoredump(s), "exit({code}) claimed a core dump");
        }
    }

    /// ...and a signal death round-trips through the OTHER pair. This is the
    /// whole of the change: before it, an uncaught SIGTERM produced
    /// `Exited(143)` and the assertions below were all false.
    #[test]
    fn a_signal_death_round_trips_through_wifsignaled() {
        for sig in [SIGILL, SIGKILL, SIGTERM, 1u32, 64u32] {
            let s = ExitReason::Killed(sig).wait_status();
            assert!(wifsignaled(s), "kill({sig}) -> {s:#06x} is not WIFSIGNALED");
            assert!(
                !wifexited(s),
                "kill({sig}) -> {s:#06x} reads as a normal exit -- this is the \
                 exact defect: a parent sees WIFEXITED and logs \"exited\" for a \
                 process that crashed"
            );
            assert!(!wifstopped(s), "kill({sig}) -> {s:#06x} reads as stopped");
            assert_eq!(wtermsig(s), sig, "kill({sig}) -> {s:#06x}");
            assert!(
                !wcoredump(s),
                "kill({sig}) -> {s:#06x} set WCOREDUMP; this runtime writes no \
                 core, and Linux does not set it under RLIMIT_CORE=0 either"
            );
        }
    }

    /// The two are never confusable, stated as the one pair that would be if the
    /// shell's rendering leaked back into the encoding.
    ///
    /// `Exited(143)` is a guest that really did call `exit(143)`; `Killed(15)`
    /// is a guest that took an uncaught SIGTERM. A shell prints `143` for both,
    /// and that collapse is what the old code shipped. The kernel does not, and
    /// neither may this.
    #[test]
    fn a_shell_status_of_143_and_a_sigterm_death_are_different_words() {
        let exited = ExitReason::Exited(TERM_STATUS).wait_status();
        let killed = ExitReason::Killed(SIGTERM).wait_status();
        assert_ne!(exited, killed);
        assert!(wifexited(exited) && wexitstatus(exited) == TERM_STATUS as u32);
        assert!(wifsignaled(killed) && wtermsig(killed) == SIGTERM);
        // ...and the shell rendering is still available where it is wanted, so
        // the module's own exit code for a killed init is unchanged.
        assert_eq!(ExitReason::Killed(SIGTERM).status_code(), TERM_STATUS);
        assert_eq!(ExitReason::Exited(7).status_code(), 7);
    }

    /// ⚠️ The two low-byte values that would encode a death `WIFSIGNALED`
    /// denies: `0` (a normal exit) and `0x7f` (`WIFSTOPPED`'s marker). Neither
    /// is reachable, because every signal number in this runtime comes from a
    /// bit index of a 64-bit pending mask -- but "not reachable" is a property
    /// of the RANGE, and this is what pins it.
    #[test]
    fn only_a_real_signal_number_survives_the_encoding() {
        for sig in 1..(NSIG as u32) {
            let low = ExitReason::Killed(sig).wait_status() & 0x7f;
            assert_ne!(low, 0x00, "sig={sig} encodes as a normal exit");
            assert_ne!(low, 0x7f, "sig={sig} encodes as a STOPPED process");
        }
        // The range itself: NSIG is 65, so the largest signal is 64 and the
        // mask cannot reach 0x7f. If NSIG ever grows past 128 this fails, which
        // is the point -- sig 127 would be silently unreportable.
        assert!(NSIG <= 0x7f, "NSIG={NSIG} admits a signal number of 0x7f");
    }

    /// Closes the chain the two ends above leave open: `reap_zombie` has to
    /// CARRY the reason out of the process table, or a correct encoder would be
    /// handed the wrong input and every test above would still pass.
    ///
    /// `sys_wait4` does exactly two things with the result -- `set_ret(pid)` and
    /// `wait_status()` through `wstatus` -- and `mod sys` is wasm-only, so this
    /// is as close to it as a host test reaches.
    #[test]
    fn reaping_carries_the_reason_out_of_the_process_table() {
        let mut c = ctx();
        // Two children of pid 1: one that exited 7, one killed by SIGTERM.
        let quit = add_blocked_proc(&mut c, 7, BlockedOn::Wait);
        let killed = add_blocked_proc(&mut c, 8, BlockedOn::Wait);
        c.procs[quit].status = ProcStatus::Zombie(ExitReason::Exited(7));
        c.procs[killed].status = ProcStatus::Zombie(TERM_REASON);

        let (pid, reason) = c.reap_zombie(7).expect("the exited child is reapable");
        assert_eq!(pid, 7);
        let s = reason.wait_status();
        assert!(
            wifexited(s) && wexitstatus(s) == 7,
            "exit(7) reaped as {s:#06x}"
        );

        let (pid, reason) = c.reap_zombie(8).expect("the killed child is reapable");
        assert_eq!(pid, 8);
        let s = reason.wait_status();
        assert!(
            wifsignaled(s) && wtermsig(s) == SIGTERM,
            "a SIGTERM death reaped as {s:#06x}, which is what the parent's \
             WIFSIGNALED reads"
        );

        // ...and both are gone, so a second wait4 finds nothing.
        assert!(c.reap_zombie(u32::MAX).is_none());
    }

    /// ⚠️ Guards the deliberate non-implementation, at the level that acts on it.
    /// A SIGSTOP must leave the process RUNNABLE and running: we cannot stop it,
    /// and killing it instead would be worse than the nothing we do now.
    #[test]
    fn a_stop_signal_neither_stops_nor_kills_the_process() {
        let mut c = ctx();
        retire(&mut c);
        c.procs[0].signals.as_mut().unwrap().pending |= sigbit(SIGSTOP);
        c.run_queue.push_back(0);
        assert_eq!(c.pick_next(), SchedOutcome::Ready);
        assert_eq!(c.procs[0].status, ProcStatus::Runnable);
    }

    /// SIGKILL has to reach a task parked in a wait no ordinary signal
    /// interrupts, or `kill -9` works on an idle process and not on a stuck one.
    /// `PipeRead` is outside `signal_interruptible` and has no other waker.
    #[test]
    fn sigkill_wakes_a_task_parked_in_a_wait_no_other_signal_interrupts() {
        let mut c = ctx();
        let idx = add_blocked_proc(&mut c, 7, BlockedOn::PipeRead(0));
        assert!(c.post_signal(7, SIGKILL));
        assert_eq!(
            c.procs[idx].status,
            ProcStatus::Runnable,
            "kill -9 left the target parked on a pipe read nothing will ever satisfy"
        );
    }

    /// ...and the counter-check that the wake above is SIGKILL's privilege and
    /// not "wake everything". An ordinary signal must leave an uninterruptible
    /// waiter alone: waking it would cut a wait short that the guest can survive
    /// and expects to resume.
    #[test]
    fn an_ordinary_signal_does_not_wake_an_uninterruptible_waiter() {
        let mut c = ctx();
        let idx = add_blocked_proc(&mut c, 7, BlockedOn::PipeRead(0));
        assert!(c.post_signal(7, SIGTERM));
        assert_eq!(c.procs[idx].status, ProcStatus::Blocked);
    }

    /// A blocked SIGKILL is still a SIGKILL, all the way through the delivery
    /// path. `deliverable_set` subtracts the blocked mask, so the loop sees
    /// nothing at all here -- the ruling has to come from the pending bits after
    /// it, and this is what says so.
    #[test]
    fn a_fully_blocked_task_is_still_killable() {
        let mut c = ctx();
        c.task_signals.blocked = u64::MAX;
        c.signals.pending |= sigbit(SIGKILL);
        unsafe { c.deliver_pending_signals(c.task_signals.blocked) };
        assert!(
            matches!(c.pending, Pending::Exit(ExitReason::Killed(SIGKILL))),
            "expected Exit(Killed(SIGKILL)), got {:?}",
            c.pending
        );
    }

    /// ⚠️ A guest can `rt_sigaction(SIGKILL, SIG_IGN)` here and be told it
    /// worked, because this runtime records any disposition for any signal. If
    /// the delivery loop honoured it, the SIG_IGN arm would CONSUME the pending
    /// bit -- and the termination check after the loop reads that same bit, so
    /// the process would become unkillable by two lines of ordinary setup.
    ///
    // ---- What `__ecv_warning` has to know BEFORE it posts SIGILL -----------
    //
    // `intrinsics` is `#[cfg(target_arch = "wasm32")]`, so `__ecv_warning`
    // itself is unreachable from `cargo test`. What IS reachable is the pair of
    // facts its ordering rests on, and they are the whole argument: posting an
    // unhandled SIGILL condemns the process before delivery returns, and
    // `delivers_to_handler` is the predicate that says whether that will happen
    // -- asked before the post, not inferred from the count afterwards.

    /// What a shell reports for a SIGILL, and what the module exits with when
    /// init takes one. See `TERM_STATUS` for why this is no longer the same
    /// thing as what `Pending::Exit` carries.
    const ILL_STATUS: i32 = 128 + SIGILL as i32;
    /// What `Pending::Exit` carries for a SIGILL.
    const ILL_REASON: ExitReason = ExitReason::Killed(SIGILL);

    /// ⚠️ THE FACT THE CENSUS FIX EXISTS FOR. `deliver_pending_signals` arms
    /// SIGILL's default action BEFORE it returns, so a caller that posts first
    /// and asks afterwards is already too late: the census arm of
    /// `__ecv_warning` returned into a process carrying `Pending::Exit(132)` and
    /// `suspended`, and the run ended at the guest's next syscall with no
    /// diagnostic -- about one censused site per run.
    ///
    /// If this test ever passes with `Pending::None`, the fix in
    /// `__ecv_warning` is solving a problem that no longer exists and the
    /// ordering there can be reconsidered. Until then it is load-bearing.
    #[test]
    fn posting_an_unhandled_sigill_condemns_the_process_before_delivery_returns() {
        let mut c = ctx();
        c.task_signals.pending |= sigbit(SIGILL);
        let ran = unsafe { c.deliver_pending_signals(c.task_signals.blocked) };
        assert_eq!(ran, 0, "SIG_DFL: no handler exists, so none can have run");
        assert!(
            matches!(c.pending, Pending::Exit(ILL_REASON)),
            "expected Exit({ILL_REASON:?}) armed by the delivery pass, got {:?}",
            c.pending
        );
        assert!(
            c.suspended,
            "the leg is marked to unwind, which is what ends the run at the \
             next syscall"
        );
    }

    /// The predicate, over the four dispositions a SIGILL can have here.
    ///
    /// A handler ADDRESS is the only one that means "the guest will receive
    /// this". Getting SIG_IGN wrong in the permissive direction would be the
    /// expensive mistake: the census would decline to post, which is right, but
    /// for a reason that would also decline to post for a real handler if the
    /// two were ever conflated -- and postgres decides whether its CPU has
    /// CRC32C by catching this exact signal.
    #[test]
    fn delivers_to_handler_is_true_only_for_a_real_unblocked_handler() {
        let mut c = ctx();
        assert!(
            !c.delivers_to_handler(SIGILL),
            "SIG_DFL (0) is a default action, not a handler"
        );
        c.signals.actions[SIGILL as usize].handler = 1; // SIG_IGN
        assert!(
            !c.delivers_to_handler(SIGILL),
            "SIG_IGN (1) discards the signal; no guest code runs"
        );
        c.signals.actions[SIGILL as usize].handler = 0x400abc;
        assert!(
            c.delivers_to_handler(SIGILL),
            "an installed handler address is exactly the case that must still \
             be posted and delivered"
        );
        c.task_signals.blocked |= sigbit(SIGILL);
        assert!(
            !c.delivers_to_handler(SIGILL),
            "a blocked signal is subtracted by `deliverable_set` and reaches \
             no handler, however it is installed"
        );
    }

    /// ⚠️ The predicate must agree with the DELIVERY LOOP, not with "was an
    /// exit armed" -- which is why `__ecv_warning` cannot simply read
    /// `deliver_pending_signals`'s return value and undo what it finds.
    ///
    /// All three handlerless shapes return `ran == 0`. Only ONE of them arms an
    /// exit. A census arm that tried to repair the damage after the fact would
    /// therefore be guessing about which case it was in, and guessing wrong
    /// means cancelling a termination that was already due.
    #[test]
    fn the_handlerless_shapes_run_nothing_but_do_not_all_arm_an_exit() {
        // SIG_IGN: consumed inside the loop. No handler, and no default action
        // left to take.
        let mut c = ctx();
        c.signals.actions[SIGILL as usize].handler = 1;
        c.task_signals.pending |= sigbit(SIGILL);
        assert!(!c.delivers_to_handler(SIGILL));
        assert_eq!(
            unsafe { c.deliver_pending_signals(c.task_signals.blocked) },
            0
        );
        assert!(
            matches!(c.pending, Pending::None),
            "SIG_IGN must not arm an exit, got {:?}",
            c.pending
        );

        // Blocked SIG_DFL: `deliverable_set` subtracts it and
        // `terminating_signal` skips a blocked signal, so nothing happens at
        // all -- same zero count, no exit.
        let mut c = ctx();
        c.task_signals.blocked |= sigbit(SIGILL);
        c.task_signals.pending |= sigbit(SIGILL);
        assert!(!c.delivers_to_handler(SIGILL));
        assert_eq!(
            unsafe { c.deliver_pending_signals(c.task_signals.blocked) },
            0
        );
        assert!(
            matches!(c.pending, Pending::None),
            "a blocked SIGILL must not arm an exit, got {:?}",
            c.pending
        );

        // Unblocked SIG_DFL: same zero count, and the process is dead.
        // Covered in full by the test above; asserted here so the three sit
        // side by side and the divergence is visible in one place.
        let mut c = ctx();
        c.task_signals.pending |= sigbit(SIGILL);
        assert!(!c.delivers_to_handler(SIGILL));
        assert_eq!(
            unsafe { c.deliver_pending_signals(c.task_signals.blocked) },
            0
        );
        assert!(
            matches!(c.pending, Pending::Exit(ILL_REASON)),
            "expected Exit({ILL_REASON:?}), got {:?}",
            c.pending
        );
    }

    /// The two signals whose disposition this runtime records but must never
    /// honour. `rt_sigaction` accepts a handler for any signal 1..=64, so the
    /// refusal lives wherever a disposition is READ -- and this predicate is a
    /// new reader of one.
    #[test]
    fn no_handler_can_be_reached_for_sigkill_or_sigstop() {
        let mut actions = [SigAction::dfl(); NSIG];
        actions[SIGKILL as usize].handler = 0x400abc;
        actions[SIGSTOP as usize].handler = 0x400abc;
        actions[SIGILL as usize].handler = 0x400abc;
        assert!(!signal_reaches_handler(SIGKILL, 0, &actions));
        assert!(!signal_reaches_handler(SIGSTOP, 0, &actions));
        // ...and the exclusion is theirs alone, or the predicate would be
        // vacuously false and the census would never post anything.
        assert!(signal_reaches_handler(SIGILL, 0, &actions));
    }

    /// Out-of-range signal numbers index nothing. `sig == 0` is `kill`'s
    /// existence probe and has no disposition slot at all; `>= NSIG` would be an
    /// out-of-bounds read of `actions`.
    #[test]
    fn an_out_of_range_signal_reaches_no_handler_instead_of_indexing() {
        let actions = [SigAction {
            handler: 0x400abc,
            flags: 0,
            mask: 0,
        }; NSIG];
        assert!(!signal_reaches_handler(0, 0, &actions));
        assert!(!signal_reaches_handler(NSIG as u32, 0, &actions));
        assert!(!signal_reaches_handler(u32::MAX, 0, &actions));
    }

    /// SIG_IGN rather than a handler address on purpose: it exercises the same
    /// exclusion without needing a real lifted function, so the failure when the
    /// exclusion is removed is an assertion and not a `fatal!`.
    #[test]
    fn a_sigkill_the_guest_tried_to_ignore_still_kills() {
        let mut c = ctx();
        c.signals.actions[SIGKILL as usize].handler = 1; // SIG_IGN
        c.signals.pending |= sigbit(SIGKILL);
        let ran = unsafe { c.deliver_pending_signals(c.task_signals.blocked) };
        assert_eq!(ran, 0);
        assert!(
            matches!(c.pending, Pending::Exit(ExitReason::Killed(SIGKILL))),
            "SIGKILL was ignorable: expected Exit(Killed(SIGKILL)), got {:?}",
            c.pending
        );
    }
}

/// The lifetime rule for file-backed `MAP_SHARED` regions.
///
/// The subject is `shm_try_reclaim`'s `SharedKind::File` arm and the predicate
/// it delegates to, `shm_file_reclaimable`. These live here rather than beside
/// `NR_MMAP` because `mod sys` is `#[cfg(target_arch = "wasm32")]` and a `#[test]`
/// there never runs; the same reason `dumpable_arg_permitted` sits in this file.
///
/// Every case below is built the way `NR_MMAP` builds one -- reserve from the
/// window, push a `SharedSeg` and a matching `ShmFile` -- so the fixture and the
/// production path share the shape of the thing under test.
#[cfg(test)]
mod shm_file_reclaim_tests {
    use super::*;
    use crate::arena::MMAP_END_VMA;

    const PAGE: u64 = 65536;

    fn ctx() -> EcvContext {
        static NAME: &[u8] = b"t\0";
        let progs = Programs::load(
            vec![Box::leak(Box::new(EcvProgram::for_test_with_tables(NAME)))],
            None,
        );
        EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            b"/".to_vec(),
            0,
            0,
            progs,
            0,
        )
    }

    /// Registers a file-backed shared region exactly as the `MAP_SHARED` arm of
    /// `NR_MMAP` does, and creates the backing file so the path RESOLVES.
    ///
    /// The file matters: with no file at the path, every case here would be
    /// reclaimed by the old path-only rule too, and each test would pass without
    /// observing the change at all.
    fn map_file(c: &mut EcvContext, path: &[u8], len: u64, writable: bool, pid: u32) -> u64 {
        c.vfs.upper_mut().write_file(path, vec![0xA5; 16]);
        assert!(
            c.vfs.resolve(b"/", path, true).is_some(),
            "fixture is inert: {} does not resolve, so the rule under test is never consulted",
            String::from_utf8_lossy(path)
        );
        let at = c.shm_reserve(len).expect("shared window exhausted");
        c.shared_segments.push(SharedSeg {
            vma_start: at,
            len: len as usize,
            kind: SharedKind::File,
            mappers: vec![pid],
        });
        c.shm_files.push(ShmFile {
            path: path.to_vec(),
            vma: at,
            len: len as usize,
            writable,
        });
        at
    }

    /// Drops `pid` and asks whether the region survived, by NAME rather than by
    /// index: the index shifts when an entry is removed.
    fn unmap_all(c: &mut EcvContext, pid: u32) {
        c.shm_drop_process(pid);
    }

    fn still_registered(c: &EcvContext, at: u64) -> bool {
        c.shared_segments.iter().any(|s| s.vma_start == at)
    }

    /// The defect. glibc `MAP_SHARED`s the locale archive PROT_READ and never
    /// unlinks it, so the old path-only test held its region -- and with it the
    /// window top, and with that 80 MiB of the PRIVATE mmap area -- for the whole
    /// life of the module.
    #[test]
    fn a_read_only_file_region_is_reclaimed_even_though_its_path_remains() {
        let mut c = ctx();
        let at = map_file(
            &mut c,
            b"/usr/lib/locale/locale-archive",
            3 * PAGE,
            false,
            5,
        );
        unmap_all(&mut c, 5);
        assert!(
            !still_registered(&c, at),
            "a read-only MAP_SHARED region was pinned by a path that still resolves; \
             nothing can have been written through it, so there is nothing to preserve"
        );
        assert!(
            c.shm_files.is_empty(),
            "the region went but its name entry stayed, so the next map of the same \
             path would be handed a recycled VMA"
        );
    }

    /// The purpose the rule exists for, kept. PostgreSQL's `posix` DSM backend
    /// maps /dev/shm/PostgreSQL.<n> PROT_READ|PROT_WRITE from more than one
    /// process, not always at the same time; recycling it between two of them
    /// corrupts shared state instead of failing loudly.
    #[test]
    fn a_writable_file_region_is_preserved_while_its_path_resolves() {
        let mut c = ctx();
        let at = map_file(&mut c, b"/dev/shm/PostgreSQL.1274626088", PAGE, true, 6);
        unmap_all(&mut c, 6);
        assert!(
            still_registered(&c, at),
            "a writable POSIX shm region was recycled while its name still resolved; \
             the next mmap of that name would land elsewhere and read stale bytes"
        );
    }

    /// The retirement path postgres actually takes: `shm_unlink`, then the last
    /// unmap. Writable regions must still be reclaimable, or the fix trades the
    /// locale leak for a DSM leak.
    #[test]
    fn a_writable_file_region_is_reclaimed_once_its_path_is_gone() {
        let mut c = ctx();
        let path: &[u8] = b"/dev/shm/PostgreSQL.1002523686";
        let at = map_file(&mut c, path, PAGE, true, 7);
        c.vfs.upper_mut().whiteout(path);
        assert!(c.vfs.resolve(b"/", path, true).is_none(), "unlink fixture");
        unmap_all(&mut c, 7);
        assert!(
            !still_registered(&c, at),
            "an unlinked POSIX shm region leaked: nothing maps it and no name reaches it"
        );
    }

    /// Reclaimability is about the region, not about the caller asking. A live
    /// mapper holds it regardless of how it was mapped.
    #[test]
    fn a_read_only_region_is_held_while_anything_still_maps_it() {
        let mut c = ctx();
        let at = map_file(&mut c, b"/etc/locale.alias", PAGE, false, 8);
        let i = c.shm_seg_at(at).unwrap();
        c.shm_add_mapper(i, 9);
        unmap_all(&mut c, 8);
        assert!(
            still_registered(&c, at),
            "a region with a live mapper was reclaimed; pid 9 still has it mapped"
        );
        unmap_all(&mut c, 9);
        assert!(
            !still_registered(&c, at),
            "reclaimed once the last mapper went"
        );
    }

    /// The consequence that made this a bug rather than an inefficiency.
    ///
    /// The private mmap area is `[MMAP_START_VMA, shm_window.top)`, so a pinned
    /// region low in the window is not a loss of its own size -- it is a loss of
    /// everything below it. Asserting on `top` rather than on the segment list is
    /// what connects the rule to `FATAL: could not map anonymous shared memory`.
    #[test]
    fn the_window_top_returns_to_its_ceiling_once_the_last_region_goes() {
        let mut c = ctx();
        // The three glibc maps, in the order the postgres run made them.
        map_file(
            &mut c,
            b"/usr/lib/locale/locale-archive",
            47 * PAGE,
            false,
            5,
        );
        map_file(&mut c, b"/etc/locale.alias", PAGE, false, 5);
        let last = map_file(
            &mut c,
            b"/usr/lib/aarch64-linux-gnu/gconv/gconv-modules.cache",
            PAGE,
            false,
            5,
        );
        assert!(
            c.shm_window.top < MMAP_END_VMA,
            "fixture did not move the window, so its recovery proves nothing"
        );
        assert_eq!(
            c.shm_window.top, last,
            "window grew down to the last region"
        );

        unmap_all(&mut c, 5);
        assert_eq!(
            c.shm_window.top, MMAP_END_VMA,
            "the window stayed at {:#x} instead of {MMAP_END_VMA:#x}: the private \
             mmap area is [MMAP_START_VMA, top), so this is where postgres loses \
             80 MiB it never gets back",
            c.shm_window.top
        );
        assert!(
            c.shm_window.free.is_empty(),
            "the window came back as holes rather than as the bump: {:?}",
            c.shm_window.free
        );
    }

    /// The predicate on its own, as a truth table.
    ///
    /// Both inputs have to matter in both directions; a predicate that ignored
    /// either one would still pass three of the four cases above.
    #[test]
    fn only_a_writable_region_with_a_live_name_is_held() {
        assert!(shm_file_reclaimable(false, true), "read-only, name present");
        assert!(shm_file_reclaimable(false, false), "read-only, name gone");
        assert!(shm_file_reclaimable(true, false), "writable, name gone");
        assert!(
            !shm_file_reclaimable(true, true),
            "writable with a live name is the ONE case the region must survive"
        );
    }
}

/// Whether a load finished or parked the caller.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum UnitLoadStep {
    /// The unit is loaded; the caller may proceed.
    Done,
    /// The caller has been parked awaiting the host. It must NOT proceed -- the
    /// guest re-enters the same syscall when resumed.
    Parked,
}

/// Per-unit `dlopen` bookkeeping.
#[derive(Default, Clone, Copy, Debug, PartialEq, Eq)]
pub struct UnitLoad {
    /// `dlopen`/`dlclose` refcount. Zero means the unit is not loaded.
    pub refs: u32,
    /// Set once the unit's data, ifuncs, TLS and constructors have been applied.
    ///
    /// ⚠️ LOAD-ONCE, and it is deliberately NOT cleared by `dlclose`. A real
    /// loader may unmap on the last close, but re-initialising a unit's `.data`
    /// underneath a guest that still holds pointers into it is silent
    /// corruption, and the guest has no way to know it happened. Leaking a
    /// loaded unit is the safe direction -- the same choice `munmap` of a
    /// partial shared region already makes in `arena.rs`.
    pub inited: bool,
}

// --- dlopen unit loading -------------------------------------------------

impl EcvContext {
    /// Makes unit `idx`'s code and data reachable, once.
    ///
    /// This is the backend-independent half of the loader seam. On the FLAT
    /// artifact the unit's code is already linked in and only the merge was
    /// deferred, so this is all there is; a split artifact adds instantiating
    /// the side module before it, which is what the `load-hosted` /
    /// `load-emscripten` / `load-wasix` backends do.
    ///
    /// ⚠️ Guarded by `inited`, not by `refs`. `dlclose` decrements the refcount
    /// but never tears a unit down -- see `UnitLoad::inited` for why -- so a
    /// close/reopen must not re-run constructors over data the guest is still
    /// using.
    pub fn ensure_unit_loaded(&mut self, idx: usize) -> Result<UnitLoadStep, &'static str> {
        if idx >= self.programs.len() {
            return Err("unit index out of range");
        }
        if self.unit(idx).inited {
            return Ok(UnitLoadStep::Done);
        }

        // ❗ A unit carrying its own PT_TLS needs a dynamic TLS block and a dtv
        // slot, which this does not provide: `setup_tls` lays out the STATIC
        // block from `.ecv.tls` at bring-up and nothing extends it afterwards.
        // Refused rather than half-loaded, because the alternative is a plugin
        // whose `__thread` reads land on another module's block -- silent, and
        // arbitrarily far from the dlopen that caused it.
        //
        // ⚠️ THIS REFUSAL IS DLOPEN'S, NOT EXECVE'S, and conflating them broke
        // every realistic execve. See `ensure_unit_code`.
        if self
            .programs
            .get(idx)
            .find_data_section(b".ecv.tls")
            .is_some()
        {
            return Err("the unit has its own TLS, which dynamic loading does not support yet");
        }

        if self.ensure_unit_code(idx)? == UnitLoadStep::Parked {
            return Ok(UnitLoadStep::Parked);
        }

        // Function tables, block maps and data sections, through the same path
        // the shared-libc units already use at startup.
        self.merge_units(&[idx], true);

        let arena_ptr = self.arena.base_ptr();
        let base = self.live_state as *const State;
        unsafe {
            // Ifuncs BEFORE constructors: a constructor may call an ifunc'd
            // libc function through a slot this fills, and an unfilled slot
            // holds the RESOLVER, which returns a pointer and does nothing.
            self.apply_ifuncs_in(idx, arena_ptr, base);
            self.apply_init_array_in(idx, arena_ptr, base);
        }
        self.unit_mut(idx).inited = true;
        Ok(UnitLoadStep::Done)
    }

    /// Makes unit `idx`'s CODE reachable, and nothing else.
    ///
    /// THE SEAM, on its own. On the flat artifact it is a no-op returning
    /// `Ready` -- the code is already linked in -- but every caller goes through
    /// it, so a backend that has to instantiate a side module needs no change at
    /// either call site.
    ///
    /// # ❗ Why `execve` uses THIS and not `ensure_unit_loaded`
    ///
    /// `ensure_unit_loaded` is `dlopen`'s operation: it also refuses a unit
    /// carrying its own `.ecv.tls`, merges the unit into the RUNNING image, and
    /// runs its ifuncs and constructors. Every one of those is wrong for
    /// `execve`, which REPLACES the image: `exec_into` rebuilds the dispatch
    /// tables, calls `merge_libraries`, and marks the process not-started so
    /// bring-up -- `setup_tls` included -- runs fresh for the new program.
    ///
    /// ⚠️ THE TLS REFUSAL IS WHY THIS EXISTS, and it was not a theoretical
    /// distinction. `sys_execve` called `ensure_unit_loaded`, so an `execve` of
    /// any program with static TLS returned `ENOEXEC` -- and a fused DYNAMIC
    /// program always has `.ecv.tls`, so that is every realistic glibc program.
    /// Measured on a two-program image: `FAIL execve: Exec format error`,
    /// immediately after a successful `dlopen` in the same guest.
    ///
    /// ⚠️ No test could see it: `e2e`'s `compileGuest` builds with
    /// `gcc -static`, and a static image carries no `.ecv.tls`, so every execve
    /// test in the suite exercised the one shape that happened to work.
    pub fn ensure_unit_code(&mut self, idx: usize) -> Result<UnitLoadStep, &'static str> {
        if idx >= self.programs.len() {
            return Err("unit index out of range");
        }
        let name = self.programs.get(idx).name_bytes().to_vec();
        let outcome = crate::loader::LoaderBackend::request(&mut self.loader, idx, &name);
        // A backend that needs the host parks the caller here; the guest
        // re-enters the same syscall on resume and asks again.
        self.apply_load_outcome(idx, outcome)
    }

    /// Asks the host for a unit that has NO registry index yet.
    ///
    /// # Why this exists at all
    ///
    /// `ensure_unit_loaded` takes a registry index, and under a host-driven
    /// loader the unit does not have one: its `EcvProgram` descriptor lives in
    /// the side module's data, so nothing can register it until the host has
    /// instantiated that module. The index only comes into being as a RESULT of
    /// the load. So the first `dlopen` of a lazily-placed plugin cannot go
    /// through `ensure_unit_loaded` -- there is no argument to give it.
    ///
    /// The guest still has to park on something, and `ecv_side_loaded` still
    /// has to name the same something. That token is
    /// `PENDING_UNIT_BASE + position in `pending_units``: stable across the
    /// park/wake, identical for two processes asking for the same unit, and far
    /// above any registry index so it can never be mistaken for one. The host
    /// treats it as opaque and echoes it back.
    ///
    /// # ⚠️ The token is NOT a unit index and must never reach `unit()`
    ///
    /// `unit`/`unit_mut` grow a per-process vec on demand, so passing a token
    /// there would silently allocate a billion-entry vec. Nothing here does:
    /// `apply_load_outcome` only blocks and sets flags, and the caller
    /// re-resolves through the dlmap after the wake to get the REAL index.
    ///
    /// Returns `Done` when the host says the unit is ready -- at which point the
    /// caller must resolve the path again, because the index now exists.
    pub fn ensure_unregistered_unit(&mut self, hash: &[u8]) -> Result<UnitLoadStep, &'static str> {
        let pos = match self.pending_units.iter().position(|h| h == hash) {
            Some(i) => i,
            None => {
                self.pending_units.push(hash.to_vec());
                self.pending_units.len() - 1
            }
        };
        let token = PENDING_UNIT_BASE + pos;
        let outcome = crate::loader::LoaderBackend::request(&mut self.loader, token, hash);
        self.apply_load_outcome(token, outcome)
    }

    /// The REGISTRY INDEX a pending token now names, if it has one.
    ///
    /// Split out of `note_side_loaded` so the mapping is testable. The
    /// `note_loaded` call it feeds is NOT: `Preloaded` -- the only backend
    /// `cargo test` can build -- records nothing and answers Ready regardless,
    /// so asserting on its answer would pass whether or not the call happened.
    /// The mapping is the part that can be wrong, so that is what is tested.
    ///
    /// Returns None for a real index (nothing to reconcile), for a token with no
    /// entry, and for a unit the host has not registered yet -- which is not an
    /// error here, only nothing to do.
    pub(crate) fn registry_index_for_token(&self, idx: usize) -> Option<usize> {
        if idx < PENDING_UNIT_BASE {
            return None;
        }
        let hash = self.pending_units.get(idx - PENDING_UNIT_BASE)?;
        self.programs.index_of_name(hash)
    }

    /// Decides what a backend's answer means for the calling process.
    ///
    /// Split out from `ensure_unit_loaded` so the PARK path is reachable from a
    /// test. With `preloaded` the only backend, `Pending` can never be produced
    /// in practice -- and code no test can reach is code that is wrong the day a
    /// backend produces it, which is exactly the shape of vacuous check this
    /// work keeps turning up.
    pub fn apply_load_outcome(
        &mut self,
        idx: usize,
        outcome: crate::loader::LoadOutcome,
    ) -> Result<UnitLoadStep, &'static str> {
        match outcome {
            crate::loader::LoadOutcome::Ready => Ok(UnitLoadStep::Done),
            crate::loader::LoadOutcome::Failed(why) => Err(why),
            crate::loader::LoadOutcome::Pending => {
                // Park exactly as a blocking syscall parks. The guest re-enters
                // the same `svc` on resume and asks the backend again, so a
                // spurious wake costs a context switch, not a wrong answer.
                self.block_current(BlockedOn::SideLoad { unit: idx });
                self.suspended = true;
                Ok(UnitLoadStep::Parked)
            }
        }
    }

    /// The host reports that unit `idx` is now loadable, or has failed.
    ///
    /// ⚠️ MUST NOT be called while a slice is on the stack: it mutates the
    /// process table, which a running slice holds `&mut`. The host queues these
    /// and flushes them between slices, exactly as it does for `ecv_net_ready`
    /// and `ecv_signal`.
    ///
    /// Returns whether anything was waiting, so a host can tell a delivered
    /// load from one nobody asked for.
    pub fn note_side_loaded(&mut self, idx: usize, ok: bool) -> bool {
        crate::loader::LoaderBackend::note_loaded(&mut self.loader, idx, ok);

        // ❗ A PENDING TOKEN AND THE REGISTRY INDEX ARE TWO NAMES FOR ONE UNIT,
        // and the backend must learn both or it places the module twice.
        //
        // The first dlopen of a lazily-placed unit parks on a TOKEN, because no
        // registry index exists yet. By the time the host calls this, it has
        // completed the whole sequence -- including `ecv_register_program` --
        // so the unit now HAS an index. The guest's retry resolves through the
        // dlmap, gets that index, and calls `ensure_unit_loaded(index)`, which
        // asks the backend about `index`. The backend has never heard of it:
        // its state is keyed by the token.
        //
        // ⚠️ MEASURED, not predicted. Without this the host was asked to place
        // the same side module a second time -- `token=1073741824` then
        // `token=1`, same name -- reserving a second arena region and a second
        // block of table slots for a unit already live, and the guest then
        // trapped. A "successful" double-load is worse than a failure: nothing
        // reports it, and the duplicate quietly consumes the resources.
        if let Some(real) = self.registry_index_for_token(idx) {
            crate::loader::LoaderBackend::note_loaded(&mut self.loader, real, ok);
        }

        let mut woken = false;
        for i in 0..self.procs.len() {
            if self.procs[i].status == ProcStatus::Blocked
                && self.procs[i].blocked_on == (BlockedOn::SideLoad { unit: idx })
            {
                // ❗ `make_runnable`, NOT the two field writes it looks like.
                //
                // This hand-rolled `status = Runnable; blocked_on = None` and
                // omitted the third thing a wake must do: `run_queue.push_back`.
                // `Pending::Block` does not leave the task on the queue, so a
                // process woken without being re-enqueued is Runnable and
                // UNREACHABLE -- and `resume_scheduling` then finds an empty
                // queue with nothing BLOCKED either, which is its definition of
                // "the run is over": it returns `Exited(exit_code)`, and
                // `exit_code` is still 0.
                //
                // ⚠️ So the symptom was a CLEAN EXIT 0 with the guest's work
                // undone. The first real mid-run load placed and registered the
                // unit, `ecv_side_loaded` reported `woke=1`, and the guest then
                // never ran another instruction or printed a line. Nothing
                // failed anywhere, and the host had every reason to believe it
                // had delivered.
                //
                // The host-side test missed it because it asserted the same two
                // fields this code set. A test written from a model of the fix
                // inherits that model's blind spot.
                self.make_runnable(i);
                woken = true;
            }
        }
        woken
    }

    /// Which process entry holds the loaded-unit set for the caller.
    ///
    /// The thread-group LEADER, not the calling task. Threads share ONE arena,
    /// so they share one loaded set: if each thread kept its own, a second
    /// thread's `dlopen` of a unit the first already loaded would re-run
    /// `load_data_sections` over data the first is using -- pristine bytes
    /// written across a live plugin's state.
    ///
    /// Falls back to the caller when the leader is gone (mid-execve, when the
    /// leader entry has been retired), which is correct: by then the caller IS
    /// the group.
    fn units_owner(&self) -> usize {
        let tgid = self.procs[self.current].tgid;
        self.find_pid(tgid).unwrap_or(self.current)
    }

    /// This group's record for unit `idx`, grown on demand.
    pub fn unit_mut(&mut self, idx: usize) -> &mut UnitLoad {
        let o = self.units_owner();
        if self.procs[o].units.len() <= idx {
            self.procs[o].units.resize(idx + 1, UnitLoad::default());
        }
        &mut self.procs[o].units[idx]
    }

    /// This group's record for unit `idx`, without growing.
    pub fn unit(&self, idx: usize) -> UnitLoad {
        self.procs[self.units_owner()]
            .units
            .get(idx)
            .copied()
            .unwrap_or_default()
    }

    /// Records the message this task's next `dlerror` returns.
    pub fn set_dlerror(&mut self, msg: Vec<u8>) {
        let cur = self.current;
        self.procs[cur].dlerror = Some(msg);
    }

    /// Clears it, as a successful dl-call must.
    pub fn clear_dlerror(&mut self) {
        let cur = self.current;
        self.procs[cur].dlerror = None;
    }

    /// Takes the pending `dlerror` message, writes it into the arena and returns
    /// its guest address, or 0 when there is none.
    ///
    /// Clearing on read is POSIX: `dlerror` reports the last error and then
    /// resets, so a second call returns NULL.
    pub fn take_dlerror(&mut self) -> u64 {
        let cur = self.current;
        let Some(msg) = self.procs[cur].dlerror.take() else {
            return 0;
        };
        let max = crate::arena::DLERROR_MAX as usize - 1;
        let n = msg.len().min(max);
        let dst = self.arena.slice_mut(crate::arena::DLERROR_VMA, n + 1);
        dst[..n].copy_from_slice(&msg[..n]);
        dst[n] = 0; // the guest reads this as a C string
        crate::arena::DLERROR_VMA
    }
}

#[cfg(test)]
mod dlopen_tests {
    use super::*;
    use crate::execmap::Programs;

    /// A context with N registered units and nothing else.
    fn ctx_with(n: usize) -> EcvContext {
        static NAMES: [&[u8]; 3] = [b"u0\0", b"u1\0", b"u2\0"];
        let regs: Vec<&'static EcvProgram> = (0..n)
            .map(|i| Box::leak(Box::new(EcvProgram::for_test_with_tables(NAMES[i]))) as &'static _)
            .collect();
        let mut c = EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            b"/".to_vec(),
            0,
            0,
            Programs::load(regs, None),
            0,
        );
        c.live_state = Box::leak(State::new_boxed()) as *mut State;
        c
    }

    /// `dlerror` is report-once: POSIX says it returns the last error and then
    /// clears, so a second call yields NULL. Before this the intercepted
    /// `dlerror` returned 0 unconditionally, which a guest cannot distinguish
    /// from "no error" -- and that is why an absent plugin looked like a version
    /// mismatch.
    #[test]
    fn dlerror_reports_once_and_then_clears() {
        let mut c = ctx_with(1);
        assert_eq!(c.take_dlerror(), 0, "a fresh context has no error");

        c.set_dlerror(b"boom: no such unit".to_vec());
        let p = c.take_dlerror();
        assert_ne!(p, 0, "a set error must return a non-null pointer");

        // The guest reads a C string, so the bytes must be there AND terminated.
        let got = c.arena.read_cstr(p);
        assert_eq!(got, b"boom: no such unit");

        assert_eq!(c.take_dlerror(), 0, "the second call must clear to NULL");
    }

    /// ❗ `dlerror` IS PER-TASK, as glibc's is per-thread.
    ///
    /// Held on the context it was global, so a process could read the message
    /// another process's failed `dlopen` left behind -- a wrong diagnosis rather
    /// than corruption, but wrong in the one place a guest looks when it is
    /// already confused.
    #[test]
    fn dlerror_does_not_leak_between_tasks() {
        let mut c = ctx_with(1);
        c.procs[0].pid = 1;
        c.procs[0].tgid = 1;
        c.set_dlerror(b"process 1 failed".to_vec());

        // A second task -- and deliberately one in the SAME thread group, which
        // is the harder case: `units` is shared across a group, `dlerror` is not.
        c.procs.push(task(2, 1));
        c.current = 1;
        assert_eq!(
            c.take_dlerror(),
            0,
            "a second task saw an error only the first had raised"
        );

        // And the first must not have lost it to the second's read.
        c.current = 0;
        let p = c.take_dlerror();
        assert_ne!(p, 0, "the first task's error was consumed by another task");
        assert_eq!(c.arena.read_cstr(p), b"process 1 failed");
    }

    /// A message longer than the buffer is truncated, not written past the end.
    /// The buffer sits between musl's TLS module list and glibc's `_dl_stack_*`
    /// bring-up state, so an overrun would corrupt libc's thread list.
    #[test]
    fn dlerror_truncates_rather_than_overrunning() {
        let mut c = ctx_with(1);
        let long = vec![b'x'; crate::arena::DLERROR_MAX as usize * 3];
        c.set_dlerror(long);
        let p = c.take_dlerror();
        let got = c.arena.read_cstr(p);
        assert_eq!(got.len(), crate::arena::DLERROR_MAX as usize - 1);

        // The byte just past the buffer must be untouched.
        let after = c
            .arena
            .slice(crate::arena::DLERROR_VMA + crate::arena::DLERROR_MAX, 1)[0];
        assert_eq!(after, 0, "the write ran past DLERROR_MAX");
    }

    /// One `.ecv.dlsyms` defining `sym` at 0x1234, in the layout
    /// `internal/fuse/tables.go` writes and `dlsym_in` parses:
    /// `[count u32][pad u32]`, then `[vma u64][nameoff u32][pad u32]`, then the
    /// NUL-terminated names, with `nameoff` measured from the section start.
    static DLSYMS: [u8; 28] = [
        1, 0, 0, 0, 0, 0, 0, 0, // count = 1, pad
        0x34, 0x12, 0, 0, 0, 0, 0, 0, // vma = 0x1234
        24, 0, 0, 0, 0, 0, 0, 0, // nameoff = 24, pad
        b's', b'y', b'm', 0,
    ];

    /// A handle comes from the guest, so it can be anything. Out of range must
    /// yield 0 -- not panic, and above all not answer from ANOTHER unit's table.
    ///
    /// ⚠️ Unit 0 carries a real export table on purpose. Without one every
    /// lookup returns 0 because the SECTION is absent, so "refused for being out
    /// of range" and "found nothing" are the same answer and the bound is not
    /// under test at all. Measured: with an empty fixture, replacing the bounds
    /// check with a clamp to the last unit still passed.
    #[test]
    fn dlsym_in_refuses_an_out_of_range_unit() {
        static NAME: &[u8] = b"u0\0";
        let regs: Vec<&'static EcvProgram> = vec![Box::leak(Box::new(
            EcvProgram::for_test_with_data(NAME, b".ecv.dlsyms\0", &DLSYMS),
        ))];
        let c = EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            b"/".to_vec(),
            0,
            0,
            Programs::load(regs, None),
            0,
        );

        // The control: unit 0 really does answer, so a 0 below means refusal.
        assert_eq!(c.dlsym_in(0, b"sym"), 0x1234);
        assert_eq!(c.dlsym_in(0, b"absent"), 0);

        // Out of range must NOT fall back to unit 0 and return 0x1234.
        assert_eq!(c.dlsym_in(1, b"sym"), 0);
        assert_eq!(c.dlsym_in(99, b"sym"), 0);
        assert_eq!(c.dlsym_in(usize::MAX, b"sym"), 0);
    }

    /// Load-once. A second `dlopen` of a unit must NOT re-run its constructors
    /// or re-initialise its data: the guest may hold pointers into it.
    #[test]
    fn ensure_unit_loaded_is_idempotent() {
        let mut c = ctx_with(2);
        assert!(!c.unit(1).inited);
        c.ensure_unit_loaded(1).expect("a fixture unit should load");
        assert!(c.unit(1).inited);

        // Prove the second call is a no-op by making a re-run observable: the
        // merge would re-load data sections and reset `materialized_prog`.
        c.materialized_prog = 7;
        c.ensure_unit_loaded(1).expect("second load");
        assert_eq!(
            c.materialized_prog, 7,
            "the second ensure_unit_loaded re-ran the merge"
        );
    }

    /// The PARK path, which no shipping backend can reach today.
    ///
    /// ⚠️ Driven through `apply_load_outcome` with a hand-made `Pending` rather
    /// than through a backend, because `Preloaded` never returns one -- and
    /// untested code is code that is wrong the day a backend does. This is the
    /// same reasoning that had the exclusion test deferred rather than written
    /// vacuously.
    #[test]
    fn a_pending_load_parks_the_caller_and_the_host_wakes_it() {
        let mut c = ctx_with(2);
        assert_eq!(c.procs[c.current].status, ProcStatus::Runnable);

        let step = c
            .apply_load_outcome(1, crate::loader::LoadOutcome::Pending)
            .expect("Pending is not an error");
        assert_eq!(step, UnitLoadStep::Parked);
        assert!(c.suspended, "the caller must unwind, not carry on");
        assert!(matches!(c.pending, Pending::Block));
        assert_eq!(
            c.procs[c.current].blocked_on,
            BlockedOn::SideLoad { unit: 1 }
        );

        // Without this the scheduler cannot tell a side-load waiter from a
        // deadlocked process: it has no fd and no deadline.
        c.procs[c.current].status = ProcStatus::Blocked;
        assert!(c.has_side_load_waiters());

        // A load for a DIFFERENT unit must not wake it.
        assert!(!c.note_side_loaded(0, true));
        assert_eq!(
            c.procs[c.current].blocked_on,
            BlockedOn::SideLoad { unit: 1 }
        );

        assert!(c.note_side_loaded(1, true), "the waiter was not woken");
        assert_eq!(c.procs[c.current].status, ProcStatus::Runnable);
        assert_eq!(c.procs[c.current].blocked_on, BlockedOn::None);
        assert!(!c.has_side_load_waiters());
        // ❗ AND IT MUST BE SCHEDULABLE. Runnable is a flag; the RUN QUEUE is
        // what the scheduler reads. A wake that sets the flag without enqueuing
        // leaves the process unreachable, and `resume_scheduling` then sees an
        // empty queue with nothing blocked -- which it reports as
        // `Exited(exit_code)`: a clean exit 0 with the guest's work undone.
        //
        // This assertion was absent, and that is why the bug shipped: the two
        // above check exactly the fields the buggy code wrote.
        assert!(
            c.run_queue.contains(&c.current),
            "the woken process is Runnable but not on the run queue, so nothing \
             will ever select it"
        );
    }

    /// `Ready` and `Failed` must NOT park anyone. A backend answering
    /// synchronously that also left the caller blocked would hang the module.
    #[test]
    fn a_settled_load_never_parks() {
        let mut c = ctx_with(1);
        assert_eq!(
            c.apply_load_outcome(0, crate::loader::LoadOutcome::Ready),
            Ok(UnitLoadStep::Done)
        );
        assert!(!c.suspended);
        assert_eq!(c.procs[c.current].blocked_on, BlockedOn::None);

        assert_eq!(
            c.apply_load_outcome(0, crate::loader::LoadOutcome::Failed("nope")),
            Err("nope")
        );
        assert!(!c.suspended);
        assert_eq!(c.procs[c.current].blocked_on, BlockedOn::None);
    }

    /// A bare process-table entry, mirroring `sched_tests::task`.
    fn task(pid: u32, tgid: u32) -> Process {
        Process {
            units: Vec::new(),
            dlerror: None,
            pid,
            ppid: 0,
            tgid,
            status: ProcStatus::Runnable,
            started: false,
            prog_idx: 0,
            blocked_on: BlockedOn::None,
            arena: None,
            state: None,
            fds: None,
            cloexec: None,
            nonblock: None,
            cwd: None,
            signals: None,
            task_signals: TaskSignals::default(),
            call_history: None,
            replay: None,
            deadline: None,
            timed_out: false,
            clear_child_tid: 0,
            dumpable: SUID_DUMP_USER,
            thp_disable: THP_NOT_DISABLED,
        }
    }

    /// ❗ THE LOADED SET IS PER-PROCESS, and holding it on the context was a
    /// silent bug.
    ///
    /// `ensure_unit_loaded` writes a unit's data into the LIVE arena, and every
    /// process has its own arena buffer restored on each switch. A context-level
    /// `inited` would therefore be true for a process whose arena never received
    /// the data: its `dlopen` short-circuits, returns a handle, and the plugin
    /// reads its own `.data` as zeroes -- silent, and arbitrarily far from the
    /// dlopen that caused it. It is also the native semantics.
    #[test]
    fn a_loaded_unit_is_not_visible_to_another_process() {
        let mut c = ctx_with(2);
        c.procs[0].pid = 1;
        c.procs[0].tgid = 1;
        c.ensure_unit_loaded(1).expect("load");
        assert!(c.unit(1).inited);

        // An unrelated process, its own thread group.
        c.procs.push(task(2, 2));
        c.current = 1;
        assert!(
            !c.unit(1).inited,
            "a second process saw a unit only the first had loaded; its arena \
             does not contain that unit's data"
        );

        // And back: the first process must not have lost it.
        c.current = 0;
        assert!(c.unit(1).inited);
    }

    /// THREADS share one arena, so they must share one loaded set -- otherwise a
    /// second thread's dlopen re-runs `load_data_sections` over data the first
    /// thread is using.
    #[test]
    fn threads_share_their_groups_loaded_set() {
        let mut c = ctx_with(2);
        c.procs[0].pid = 1;
        c.procs[0].tgid = 1;
        c.ensure_unit_loaded(1).expect("load");

        // A CLONE_THREAD sibling: own pid, the leader's tgid.
        c.procs.push(task(2, 1));
        c.current = 1;
        assert!(
            c.unit(1).inited,
            "a thread did not see its group's loaded unit, so its dlopen would \
             re-initialise data the leader is using"
        );
    }

    #[test]
    fn ensure_unit_loaded_refuses_an_unknown_unit() {
        let mut c = ctx_with(1);
        assert!(c.ensure_unit_loaded(5).is_err());
    }

    /// The pending TOKEN must be stable per unit and distinct across units.
    ///
    /// ⚠️ This is the whole correctness argument for the hosted first-dlopen
    /// path, and it is invisible in `sys.rs` -- that file is
    /// `#[cfg(target_arch = "wasm32")]`, so `cargo test` never compiles it.
    /// Tested here or nowhere.
    ///
    /// If the token were not stable, the guest would park on one number and
    /// `ecv_side_loaded` would wake a different one: a hang, with the host
    /// convinced it had delivered. If two units shared a token, one host load
    /// would wake a guest waiting for a plugin nobody had placed, and the retry
    /// would resolve to nothing.
    #[test]
    fn a_pending_token_is_stable_per_unit_and_never_a_registry_index() {
        let mut c = ctx_with(2);

        // `Preloaded` answers Ready, so these settle rather than park -- which is
        // fine: what is under test is the TOKEN, and it is allocated before the
        // backend is asked.
        let _ = c.ensure_unregistered_unit(b"aaaa_1111");
        let _ = c.ensure_unregistered_unit(b"bbbb_2222");
        let _ = c.ensure_unregistered_unit(b"aaaa_1111");

        assert_eq!(
            c.pending_units.len(),
            2,
            "asking twice for the same unit allocated a second token, so the wake \
             for the first ask can never match the second"
        );
        assert_eq!(c.pending_units[0], b"aaaa_1111");
        assert_eq!(c.pending_units[1], b"bbbb_2222");

        // And no token can be mistaken for a registry index.
        assert!(
            PENDING_UNIT_BASE > c.programs.len() * 1000,
            "PENDING_UNIT_BASE is not comfortably above the registry"
        );
    }

    /// A parked FIRST dlopen must block on the token, and the host's wake for
    /// that token must release it.
    ///
    /// ⚠️ Driven through `apply_load_outcome` with a hand-made `Pending`,
    /// because `Preloaded` -- the only backend `cargo test` can build -- never
    /// returns one. Same reasoning as `a_pending_load_parks_the_caller_...`.
    #[test]
    fn the_host_wake_releases_a_guest_parked_on_a_pending_token() {
        let mut c = ctx_with(1);
        c.pending_units.push(b"late_beef".to_vec());
        let token = PENDING_UNIT_BASE;

        let step = c
            .apply_load_outcome(token, crate::loader::LoadOutcome::Pending)
            .expect("Pending is not an error");
        assert_eq!(step, UnitLoadStep::Parked);
        assert_eq!(
            c.procs[c.current].blocked_on,
            BlockedOn::SideLoad { unit: token }
        );
        c.procs[c.current].status = ProcStatus::Blocked;
        assert!(
            c.has_side_load_waiters(),
            "a token waiter has no fd and no deadline, so without this the \
             scheduler reads it as a deadlock"
        );

        // A wake for the REGISTRY index 0 must not release a token waiter: the
        // two number spaces meet here, and this is where a collision would show.
        assert!(!c.note_side_loaded(0, true));
        assert_eq!(
            c.procs[c.current].blocked_on,
            BlockedOn::SideLoad { unit: token }
        );

        assert!(
            c.note_side_loaded(token, true),
            "the token waiter was not woken"
        );
        assert_eq!(c.procs[c.current].status, ProcStatus::Runnable);
        assert!(
            c.run_queue.contains(&c.current),
            "woken but not enqueued: the scheduler reads the run queue, not the flag"
        );
    }

    /// ❗ A GUEST PARKED ON A HOST LOAD MUST IDLE, NOT DEADLOCK -- on the
    /// socket-free path, which is the one that decides it.
    ///
    /// `resume_scheduling` has TWO places that can conclude deadlock. The one
    /// inside `WaitOutcome::TimedOut` had this guard from the start, and it is
    /// unreachable for a parked `dlopen`: getting there needs the net backend's
    /// `wait` to be called, which needs a socket waiter. The earlier branch --
    /// nothing runnable, no io, no deadline, something blocked -- is where a
    /// `SideLoad` waiter actually lands, and it did not ask.
    ///
    /// ⚠️ NOT HYPOTHETICAL. The first real mid-run load exited 111 while the
    /// host was holding the module the guest was waiting for. Every host-side
    /// gate was green: `has_side_load_waiters` had a caller, a passing test and
    /// a correct implementation.
    ///
    /// ⚠️ NEUTRALIZED: deleting the guard in the socket-free branch makes this
    /// return `Deadlock` and the test fails on the assertion below, not on a
    /// compile error.
    #[test]
    fn a_side_load_waiter_idles_rather_than_deadlocking() {
        let mut c = ctx_with(1);
        // Park the only process on a host load, exactly as a first dlopen does.
        c.apply_load_outcome(PENDING_UNIT_BASE, crate::loader::LoadOutcome::Pending)
            .expect("Pending is not an error");
        c.procs[c.current].status = ProcStatus::Blocked;

        // The conditions that make this branch fire: nothing runnable, no
        // socket waiter, no deadline. Asserted rather than assumed -- if any
        // were false the test would pass without reaching the code it is for.
        assert!(
            !c.has_socket_waiters(),
            "a socket waiter would take another path"
        );
        assert_eq!(
            c.earliest_deadline(),
            None,
            "a deadline would take another path"
        );
        assert!(c.has_side_load_waiters());

        assert_eq!(
            c.resume_scheduling(),
            SchedOutcome::Idle {
                wake_at: None,
                io: false
            },
            "a guest waiting for the host to place a side module was reported as \
             deadlocked. The host is the only thing that can resolve it, and \
             killing the module denies it the chance -- ecv_side_loaded arrives \
             between slices."
        );
    }

    /// ❗ THE WAKE MUST RECONCILE THE TOKEN WITH THE UNIT'S REGISTRY INDEX.
    ///
    /// A lazily-placed unit is asked for twice under two different names: a
    /// synthetic token on the first `dlopen`, when no index exists, and its real
    /// index on the retry after the wake. A backend that only learned the token
    /// reports the index as Cold and asks the host to place the SAME module
    /// again.
    ///
    /// ⚠️ Measured, not predicted: the host was asked for
    /// `unit_a_fused_d7c8ba95ccd2` first as token 1073741824 and then as index
    /// 1, placed it twice, and the guest trapped afterwards. A double placement
    /// SUCCEEDS at every step, which is what makes it dangerous rather than
    /// merely wasteful.
    ///
    /// ⚠️ This tests the MAPPING, not the `note_loaded` call it feeds.
    /// `Preloaded` records nothing and answers Ready regardless, so an assertion
    /// on the backend's answer would pass whether or not the call was made --
    /// the exact vacuity this work keeps finding. The mapping is the part that
    /// can be wrong.
    #[test]
    fn the_wake_maps_a_token_back_to_the_units_registry_index() {
        let mut c = ctx_with(2);
        let hash = c.programs.get(1).name_bytes().to_vec();
        assert_eq!(
            c.programs.index_of_name(&hash),
            Some(1),
            "the fixture's unit 1 must be resolvable by name, or this proves nothing"
        );

        c.pending_units.push(hash);
        assert_eq!(
            c.registry_index_for_token(PENDING_UNIT_BASE),
            Some(1),
            "the token does not map back to the unit's registry index, so the \
             guest's retry asks the host to place the same module a second time"
        );

        // The exclusions, without which the mapping would just answer anything.
        assert_eq!(
            c.registry_index_for_token(1),
            None,
            "a REAL index must not be treated as a token"
        );
        assert_eq!(
            c.registry_index_for_token(PENDING_UNIT_BASE + 7),
            None,
            "a token with no pending entry must not resolve"
        );

        // A pending hash the registry does not have stays unresolved: that is a
        // unit the host has not registered yet, which is the normal state before
        // a load completes.
        c.pending_units.push(b"never_registered".to_vec());
        assert_eq!(c.registry_index_for_token(PENDING_UNIT_BASE + 1), None);
    }

    /// ❗ A HOST THAT PLACES THE SAME UNIT TWICE MUST BE REFUSED, and the
    /// pointer check alone cannot do it.
    ///
    /// A unit's name is its content hash, so two descriptors carrying one name
    /// are one unit placed twice, at two addresses. Registering both leaves a
    /// registry entry that `index_of_name` can never return -- live, unreachable,
    /// holding an arena region and a block of table slots.
    ///
    /// ⚠️ This is the shape the token/index reconciliation bug produced: the
    /// host was asked for the same unit under two names and placed it both
    /// times. That cause is fixed; a careless host can still do it, and only
    /// this layer knows what is already registered.
    ///
    /// ⚠️ NEUTRALIZED: dropping the name check makes the second registration
    /// return ECV_REG_OK and `programs.len()` reach 2, and this test fails on
    /// the assertion rather than on a compile error.
    #[test]
    fn a_late_registration_of_an_already_known_unit_is_refused() {
        let mut c = ctx_with(1);
        let before = c.programs.len();

        // A DIFFERENT descriptor object carrying the SAME name -- which is what
        // a second placement of one unit looks like. Using the same pointer
        // would only exercise the check that already existed.
        let dup: &'static EcvProgram =
            Box::leak(Box::new(EcvProgram::for_test_with_tables(b"u0\0")));
        assert!(
            !core::ptr::eq(dup, c.programs.regs()[0]),
            "the fixture must use a distinct descriptor, or this tests the \
             pointer check instead of the name check"
        );

        let rc = unsafe { c.register_late_unit(dup) };
        assert_eq!(
            rc,
            crate::abi::ECV_REG_DUPLICATE,
            "a second placement of the same unit was accepted"
        );
        assert_eq!(
            c.programs.len(),
            before,
            "the duplicate was appended anyway: it is now a registry entry \
             index_of_name can never return"
        );

        // The CONTROL. A genuinely new unit must still register, or the check
        // above would pass on an implementation that refuses everything.
        let fresh: &'static EcvProgram =
            Box::leak(Box::new(EcvProgram::for_test_with_tables(b"brand_new\0")));
        assert_eq!(
            unsafe { c.register_late_unit(fresh) },
            crate::abi::ECV_REG_OK
        );
        assert_eq!(c.programs.len(), before + 1);
        // Without the NUL: the descriptor stores a NUL-terminated name and
        // `name_bytes()` returns it WITHOUT the terminator, so querying with one
        // matches nothing. Worth stating, because the miss looks like a failed
        // registration rather than a mis-spelled lookup.
        assert_eq!(c.programs.index_of_name(b"brand_new"), Some(before));
    }

    /// ❗ `execve` MUST ACCEPT A UNIT WITH STATIC TLS; `dlopen` must refuse it.
    ///
    /// The two triggers share the loader seam and nothing else. `dlopen` adds a
    /// unit to a RUNNING image whose static TLS block is already laid out, so a
    /// unit carrying its own `.ecv.tls` has to be refused until dynamic TLS
    /// exists. `execve` REPLACES the image -- `exec_into` rebuilds the tables
    /// and marks the process not-started, so `setup_tls` runs fresh.
    ///
    /// ⚠️ `sys_execve` called `ensure_unit_loaded`, inheriting the refusal, so
    /// an execve of any program with static TLS returned ENOEXEC. A fused
    /// DYNAMIC program always has `.ecv.tls`, so that was every realistic glibc
    /// program. Measured on a two-program image: `FAIL execve: Exec format
    /// error`, one line after a successful dlopen in the same guest.
    ///
    /// ⚠️ And no test could see it. `e2e`'s `compileGuest` builds with
    /// `gcc -static`; a static image carries no `.ecv.tls`, so every execve test
    /// in the suite exercised the one shape that happened to work. That is why
    /// this test constructs the TLS-carrying case explicitly.
    #[test]
    fn execve_accepts_a_unit_with_static_tls_and_dlopen_does_not() {
        let tls: &'static EcvProgram = Box::leak(Box::new(EcvProgram::for_test_with_data(
            b"withtls\0",
            b".ecv.tls\0",
            &[0u8; 16],
        )));
        let mut c = EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            b"/".to_vec(),
            0,
            0,
            Programs::load(vec![tls], None),
            0,
        );
        c.live_state = Box::leak(State::new_boxed()) as *mut State;

        // The fixture must actually carry the section, or both assertions below
        // are about a unit with no TLS and prove nothing.
        assert!(
            c.programs.get(0).find_data_section(b".ecv.tls").is_some(),
            "the fixture has no .ecv.tls, so this test cannot distinguish the paths"
        );

        // dlopen's operation: refused.
        assert!(
            c.ensure_unit_loaded(0).is_err(),
            "dlopen accepted a unit with its own static TLS; its __thread reads \
             would land on another module's block"
        );

        // execve's: accepted, because exec_into lays TLS out fresh.
        assert_eq!(
            c.ensure_unit_code(0),
            Ok(UnitLoadStep::Done),
            "execve was refused a program with static TLS -- which is every fused \
             DYNAMIC program, so this is ENOEXEC for every realistic glibc guest"
        );
    }

    /// `dlclose` decrements but never tears down, and it must not underflow --
    /// a wrapped refcount would read as billions of live references.
    #[test]
    fn unit_refcount_saturates_at_zero() {
        let mut c = ctx_with(1);
        c.unit_mut(0).refs = 1;
        for _ in 0..2 {
            let n = c.unit(0).refs.saturating_sub(1);
            c.unit_mut(0).refs = n;
        }
        assert_eq!(c.unit(0).refs, 0);
    }
}

impl EcvContext {
    /// Registers a unit the host placed AFTER `_start`, returning its index.
    ///
    /// The registry is otherwise fixed at construction; this is the only way it
    /// grows. `units` grows with it so a handle stays `index + 1` and the two
    /// cannot drift -- a `units` shorter than the registry would make the last
    /// unit's `dlopen` panic on an index the registry considers valid.
    ///
    /// # Safety
    ///
    /// `p` must point at a live `EcvProgram` that outlives the module, which a
    /// side module's exported descriptor does. Nothing here dereferences it
    /// beyond reading its name.
    pub unsafe fn register_late_unit(&mut self, p: *const EcvProgram) -> i32 {
        let r: &'static EcvProgram = &*p;
        if self.programs.regs().iter().any(|q| core::ptr::eq(*q, r)) {
            return crate::abi::ECV_REG_DUPLICATE;
        }
        // ❗ AND BY NAME, not only by pointer.
        //
        // A unit's name is its CONTENT HASH, so two descriptors carrying the
        // same name are the same unit -- placed twice, at two addresses. The
        // pointer check cannot see that: the addresses differ, so both would be
        // registered, `index_of_name` would return the first, and the second
        // would be a live registry entry nothing can ever reach, holding an
        // arena region and a block of table slots.
        //
        // ⚠️ Not hypothetical. A double placement is exactly what happened when
        // the pending token and the registry index were not reconciled: the host
        // was asked for `unit_a_fused_d7c8ba95ccd2` first as token 1073741824
        // and then as index 1, and placed it both times. That cause is fixed
        // (`registry_index_for_token`), but a buggy or careless host can still
        // do it, and this is the layer that can refuse -- the host cannot know
        // what is already registered.
        if self.programs.index_of_name(r.name_bytes()).is_some() {
            return crate::abi::ECV_REG_DUPLICATE;
        }
        let _ = self.programs.push_late(r);
        // No per-process fix-up: `unit_mut` grows each process's vec on demand,
        // which is exactly why it is lazy. The unit is registered but NOT
        // loaded -- its data, ifuncs and constructors are `ensure_unit_loaded`'s
        // job, reached when the guest resumes into the dlopen it parked in.
        crate::abi::ECV_REG_OK
    }
}

/// Where pending-load tokens start, above every possible registry index.
///
/// A token names a unit the host has been asked to place but that has no
/// registry index yet (see `EcvContext::ensure_unregistered_unit`). It shares a
/// number space with real indices only in that `ecv_side_loaded` and
/// `BlockedOn::SideLoad` accept either, so it must be impossible to confuse
/// with one. The largest closure this tree has built has 71 programs; a plugin
/// per host `.so` would be ~2,100. 2^30 leaves no doubt, and the token is never
/// used to index anything.
pub const PENDING_UNIT_BASE: usize = 1 << 30;

/// Which registry indices are LIBRARY units: merged into whichever binary runs,
/// at startup and after every execve.
///
/// ❗ A DLOPEN-ABLE UNIT IS NOT ONE, and getting this wrong is silent.
/// `Programs::library_indices` classifies by ABSENCE from the exec map -- a
/// program with no entry path is taken to be the shared-libc superset unit. A
/// dlopen'd plugin has no exec-map path either, so it fell into exactly that
/// bucket and was merged EAGERLY, which defeated the whole deferral: every
/// unit's data was loaded into the arena before the guest ran. Nothing failed.
/// It was slower and larger than claimed, which is the kind of wrong that does
/// not announce itself.
///
/// A named function rather than an expression inside `EcvContext::new` so the
/// rule can be TESTED. Inline, the only way to check it was to re-implement the
/// filter in the test, which would have tested the test.
pub(crate) fn library_units_for(
    progs: &crate::execmap::Programs,
    dlmap: &crate::dlmap::DlMap,
) -> Vec<usize> {
    let dl = dlmap.referenced_units(progs);
    progs
        .library_indices()
        .into_iter()
        .filter(|i| !dl.contains(i))
        .collect()
}

#[cfg(test)]
mod library_unit_tests {
    use super::*;
    use crate::dlmap::DlMap;
    use crate::execmap::Programs;

    fn program(name: &'static [u8]) -> &'static EcvProgram {
        Box::leak(Box::new(EcvProgram::for_test(name)))
    }

    fn encode(magic: &[u8], entries: &[(&[u8], &[u8])]) -> Vec<u8> {
        let mut b = magic.to_vec();
        b.extend_from_slice(&(entries.len() as u32).to_le_bytes());
        for (path, hash) in entries {
            b.extend_from_slice(&(path.len() as u32).to_le_bytes());
            b.extend_from_slice(path);
            b.extend_from_slice(&(hash.len() as u32).to_le_bytes());
            b.extend_from_slice(hash);
        }
        b
    }

    /// A closure with an entry program, a shared libc, and a dlopen-able plugin.
    /// Only the libc may be merged eagerly.
    #[test]
    fn a_dlopen_unit_is_not_a_library_unit() {
        let regs = vec![
            program(b"main_0000\0"),
            program(b"plugin_1111\0"),
            program(b"libc_2222\0"),
        ];
        let progs = Programs::load(
            regs,
            Some(&encode(b"RMEXEC01", &[(b"/bin/main", b"main_0000")])),
        );
        let dlmap = DlMap::load(
            progs.regs(),
            Some(&encode(b"RMDLOP01", &[(b"/lib/plug.so", b"plugin_1111")])),
        );

        // The CONTROL, and the whole point: without the filter the plugin IS
        // classified as a library, so this assertion is what makes the next one
        // mean something.
        assert!(
            progs.library_indices().contains(&1),
            "library_indices no longer sweeps in dlopen units; this test is now vacuous"
        );

        let libs = library_units_for(&progs, &dlmap);
        assert!(
            !libs.contains(&1),
            "the plugin is still an eagerly merged library unit: {libs:?}"
        );
        assert!(
            libs.contains(&2),
            "libc must STILL be merged eagerly; the filter took too much: {libs:?}"
        );
    }

    /// With no dlopen map nothing is filtered, so the shared-libc path is
    /// exactly as it was.
    #[test]
    fn no_dlopen_map_changes_nothing() {
        let regs = vec![program(b"main_0000\0"), program(b"libc_2222\0")];
        let progs = Programs::load(
            regs,
            Some(&encode(b"RMEXEC01", &[(b"/bin/main", b"main_0000")])),
        );
        let dlmap = DlMap::load(progs.regs(), None);
        assert_eq!(library_units_for(&progs, &dlmap), progs.library_indices());
    }
}

/// The `br` discrimination that makes `longjmp` work: which indirect branches
/// are tail calls (dispatch by nested call, as before) and which are non-local
/// jumps (unwind and replay).
///
/// ⚠️ These are HOST tests of the decision, not of the unwind. The unwind needs
/// the lifted code's suspend checks and so only runs under wasm; the
/// reproducers in `.agents-workspace/longjmp/` cover that end, and the fix was
/// neutralized against them (2026-09-01: disabling `nonlocal_jump_depth`'s
/// search restores `_ecv_unreached ... 0x101010101010101` on `nocall`, `mask0`
/// and `variants`). What is worth testing here is the classification, because
/// getting it wrong in the OTHER direction -- calling an ordinary tail call
/// non-local -- would unwind a frame the guest still needs, and no reproducer
/// in that directory would notice.
#[cfg(test)]
mod nonlocal_jump_tests {
    use super::*;

    /// Never called. `funcs` maps a VMA to a function pointer and the tests
    /// below only ever compare VMAs, so the body is unreachable by construction
    /// -- and says so rather than returning quietly, which would let a test that
    /// accidentally DISPATCHES pass.
    unsafe extern "C" fn never(_: *mut u8, _: *mut State, _: u64, _: *mut EcvContext) {
        unreachable!("nonlocal_jump_depth must not dispatch; it only classifies")
    }

    fn ctx() -> EcvContext {
        static NAME: &[u8] = b"t\0";
        let progs = Programs::load(
            vec![Box::leak(Box::new(EcvProgram::for_test_with_tables(NAME)))],
            None,
        );
        let mut c = EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            b"/".to_vec(),
            0,
            0,
            progs,
            0,
        );
        // Three lifted functions, sorted -- `func_at` / `func_containing`
        // binary-search this. `main` at 0x400500, `helper` at 0x400600,
        // `__longjmp` at 0x405540, spaced so a mid-function address is
        // unambiguous.
        c.funcs = vec![
            (0x400500, never as LiftedFunc),
            (0x400600, never as LiftedFunc),
            (0x405540, never as LiftedFunc),
        ];
        c
    }

    /// The guest stack of `nocall.c` at the moment of the `longjmp`:
    /// main -> helper -> __longjmp. Entry `i` is (function at depth i, where it
    /// resumes), which is the layout `_ecv_save_call_history` pushes and the
    /// scheduler's replay walks.
    fn stack(c: &mut EcvContext) {
        c.call_history = vec![
            (0x400500, 0x400538), // main, resuming after its call to helper
            (0x400600, 0x400610), // helper, resuming after its call to __longjmp
        ];
    }

    /// ⭐ THE CASE THE FIX EXISTS FOR. `__longjmp`'s `br x30` targets a return
    /// address inside `main`, which is on the stack at depth 0.
    #[test]
    fn a_branch_into_a_frame_on_the_stack_is_non_local() {
        let mut c = ctx();
        stack(&mut c);
        assert_eq!(c.nonlocal_jump_depth(0x400538), Some(0));
    }

    /// ❗ THE OTHER DIRECTION, and the one no reproducer covers. A `br` to a
    /// function ENTRY is a tail call: it abandons one guest frame and the
    /// nested-call dispatch pops one host frame, which is correct. Treating it
    /// as non-local would unwind a live frame.
    #[test]
    fn a_branch_to_a_function_entry_is_a_tail_call() {
        let mut c = ctx();
        stack(&mut c);
        // 0x400600 is `helper`'s ENTRY and `helper` is also on the stack, so
        // this is exactly the case an entry check has to win.
        assert_eq!(c.nonlocal_jump_depth(0x400600), None);
        assert_eq!(c.nonlocal_jump_depth(0x400500), None);
    }

    /// A `br` into a function that is NOT on the stack is an ordinary far jump
    /// -- a computed branch, a PLT thunk -- and must keep dispatching.
    #[test]
    fn a_branch_into_a_frame_not_on_the_stack_is_not_non_local() {
        let mut c = ctx();
        stack(&mut c);
        // Inside `__longjmp`, which is RUNNING but is not itself an entry on
        // the history (the history names its CALLER).
        assert_eq!(c.nonlocal_jump_depth(0x405560), None);
    }

    /// An address below every lifted function has no containing function, and
    /// must not be mistaken for one at depth 0.
    #[test]
    fn an_address_below_every_function_is_not_non_local() {
        let mut c = ctx();
        stack(&mut c);
        assert_eq!(c.nonlocal_jump_depth(0x100), None);
    }

    /// An empty stack can match nothing. Stated because the search is a
    /// `rposition` over `call_history`, and an empty history returning `Some`
    /// would mean the boot frame could be "returned to" before it exists.
    #[test]
    fn an_empty_stack_has_no_non_local_target() {
        let mut c = ctx();
        c.call_history.clear();
        assert_eq!(c.nonlocal_jump_depth(0x400538), None);
    }

    /// ⚠️ Documents the known limitation rather than hiding it: with `main`
    /// recursive, the INNERMOST matching frame is chosen, because
    /// `call_history` records no per-frame SP to disambiguate with. This test
    /// exists so that a later change which DOES disambiguate has to come here
    /// and say so.
    #[test]
    fn recursion_resolves_to_the_innermost_matching_frame() {
        let mut c = ctx();
        c.call_history = vec![
            (0x400500, 0x400520), // main, outer
            (0x400500, 0x400538), // main, inner
            (0x400600, 0x400610), // helper
        ];
        assert_eq!(c.nonlocal_jump_depth(0x400538), Some(1));
    }

    /// The replay the scheduler consumes: enter the target FRAME at the target
    /// ADDRESS, with the history truncated to the frames outside it.
    ///
    /// ❗ `remaining.len() == call_history.len()` is the invariant the leg loop
    /// depends on -- it pops one from each in lockstep as reconstruction walks
    /// outward. Asserted directly, because a mismatch does not fail here; it
    /// fails much later as a process resuming into the wrong frame.
    #[test]
    fn beginning_a_non_local_jump_truncates_both_stacks_in_lockstep() {
        let mut c = ctx();
        stack(&mut c);
        let d = c.nonlocal_jump_depth(0x400538).expect("non-local");
        c.begin_nonlocal_jump(0x400538, d);

        assert!(c.longjmp_pending, "the leg loop must be told to re-enter");
        let cur = c.current;
        let rp = c.procs[cur].replay.as_ref().expect("a replay was recorded");
        assert_eq!(
            rp.cur,
            (0x400500, 0x400538),
            "re-enter main at the branch target, not at its entry"
        );
        assert!(
            !rp.resuming,
            "a branch target is not a syscall-resume point; driving a syscall \
             handler before re-entry would complete a syscall that never blocked"
        );
        assert_eq!(rp.remaining.len(), c.call_history.len());
        assert_eq!(c.call_history.len(), d, "frames outside the target only");
        assert!(
            rp.remaining.is_empty(),
            "main was outermost here, so nothing remains to reconstruct"
        );
    }

    /// The same, one frame deeper, so the truncation is observed removing
    /// something -- the test above would pass with `truncate` doing nothing.
    #[test]
    fn truncation_actually_drops_the_abandoned_frames() {
        let mut c = ctx();
        c.call_history = vec![
            (0x400400, 0x400410), // an outer frame, kept
            (0x400500, 0x400538), // main, the target
            (0x400600, 0x400610), // helper, abandoned
        ];
        c.funcs.insert(0, (0x400400, never as LiftedFunc));
        let d = c.nonlocal_jump_depth(0x400538).expect("non-local");
        assert_eq!(d, 1);
        c.begin_nonlocal_jump(0x400538, d);
        assert_eq!(c.call_history, vec![(0x400400, 0x400410)]);
        let rp = c.procs[c.current].replay.as_ref().unwrap();
        assert_eq!(rp.remaining, vec![(0x400400, 0x400410)]);
    }
}

/// Directory descriptors as a resolution base: [`EcvContext::dir_fd_path`] and
/// [`EcvContext::resolve_base`].
///
/// ⚠️ These live here and not beside the syscalls that use them because
/// `mod sys` is `#[cfg(target_arch = "wasm32")]` -- nothing in `sys.rs` is
/// compiled on the host, so a test module written there compiles NOWHERE and
/// reports nothing while looking like coverage. That happened once on
/// 2026-09-01, on this very change, and is why the decision was moved out.
#[cfg(test)]
mod resolve_base_tests {
    use super::*;

    fn ctx_at(cwd: &[u8]) -> EcvContext {
        static NAME: &[u8] = b"t\0";
        let progs = Programs::load(
            vec![Box::leak(Box::new(EcvProgram::for_test_with_tables(NAME)))],
            None,
        );
        EcvContext::new(
            Arena::new(),
            crate::vfs::Vfs::new(None),
            cwd.to_vec(),
            0,
            0,
            progs,
            0,
        )
    }

    /// Installs a directory descriptor exactly as `sys_openat`'s `NodeKind::Dir`
    /// arm does.
    fn dirfd(c: &mut EcvContext, path: &[u8]) -> i64 {
        c.alloc_fd(OpenFile::Dir {
            entries: Vec::new(),
            pos: 0,
            path: path.to_vec(),
        }) as i64
    }

    /// POSITIVE CONTROL. Every assertion below distinguishes a base from the
    /// cwd, so the two must differ -- if `dirfd` stopped installing a `Dir`,
    /// `resolve_base` would return `None` and the interesting tests would fail
    /// rather than pass vacuously. Stated as its own test so the reason is
    /// visible.
    #[test]
    fn a_directory_descriptor_reports_its_path() {
        let mut c = ctx_at(b"/etc");
        let fd = dirfd(&mut c, b"/bin");
        assert_eq!(c.dir_fd_path(fd), Some(&b"/bin"[..]));
        assert_ne!(c.dir_fd_path(fd), Some(&c.cwd[..]));
    }

    /// ⭐ THE CASE `find` NEEDS. Relative + a real dirfd resolves against that
    /// directory, NOT the cwd. This returned `None` before 2026-09-01.
    #[test]
    fn a_relative_path_uses_the_dirfd_not_the_cwd() {
        let mut c = ctx_at(b"/etc");
        let fd = dirfd(&mut c, b"/bin");
        assert_eq!(c.resolve_base(fd, b"busybox"), Some(&b"/bin"[..]));
    }

    /// An absolute path ignores the dirfd, as on Linux.
    #[test]
    fn an_absolute_path_ignores_the_dirfd() {
        let mut c = ctx_at(b"/etc");
        let fd = dirfd(&mut c, b"/bin");
        assert_eq!(c.resolve_base(fd, b"/usr/lib/x"), Some(&b"/etc"[..]));
    }

    /// `AT_FDCWD` still means the cwd, for both spellings of a relative path.
    #[test]
    fn at_fdcwd_means_the_cwd() {
        let c = ctx_at(b"/etc");
        assert_eq!(c.resolve_base(AT_FDCWD, b"passwd"), Some(&b"/etc"[..]));
        assert_eq!(c.resolve_base(AT_FDCWD, b"./passwd"), Some(&b"/etc"[..]));
    }

    /// ❗ A dirfd that is not a directory yields `None` rather than falling back
    /// to the cwd. The fallback is the dangerous answer: it would resolve a
    /// relative path somewhere plausible and report success, so a caller passing
    /// a wrong fd would silently operate on the wrong file.
    #[test]
    fn a_non_directory_dirfd_has_no_base() {
        let mut c = ctx_at(b"/etc");
        let fd = c.alloc_fd(OpenFile::Null { zero: true }) as i64;
        assert_eq!(c.resolve_base(fd, b"passwd"), None);
    }

    /// An fd that was never opened, and a negative fd that is not `AT_FDCWD`.
    ///
    /// ⚠️ This asserts the CONTRACT ("no base"), not a mechanism. `Vec::get` is
    /// what makes the negative case safe -- `-7 as usize` is a huge index and
    /// `get` returns `None` -- so no guard in `dir_fd_path` can be neutralized
    /// to make this fail. Kept because callers depend on the contract; not
    /// cited as evidence that any guard works.
    #[test]
    fn an_unopened_or_negative_descriptor_has_no_base() {
        let c = ctx_at(b"/etc");
        assert_eq!(c.resolve_base(4242, b"passwd"), None);
        assert_eq!(c.resolve_base(-7, b"passwd"), None);
        assert_eq!(c.dir_fd_path(-1), None);
    }

    /// A negative non-`AT_FDCWD` fd with an ABSOLUTE path still resolves -- the
    /// path wins before the fd is ever looked at, which is what Linux does.
    #[test]
    fn an_absolute_path_survives_a_nonsense_descriptor() {
        let c = ctx_at(b"/etc");
        assert_eq!(c.resolve_base(-7, b"/usr/bin/env"), Some(&b"/etc"[..]));
    }
}
