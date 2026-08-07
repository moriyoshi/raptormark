//! remill/elfconv intrinsics referenced by lifted objects. Ports
//! `runtime/VmIntrinsics.cpp` + `Runtime.cpp` from the pinned submodule.
//!
//! Signatures mirror the upstream C++ at the wasm ABI level (wasm-ld
//! validates them against the lifted object's declarations): C++ references
//! become raw pointers, `uint128_t`/`float128_t` become `u128` (identical
//! LLVM lowering on wasm32), and the variadic flag-computation helpers take
//! the single va-buffer pointer clang lowers `...` to.

use crate::abi::{LiftedFunc, State};
use crate::context::EcvContext;
use crate::fatal;
use crate::sys;
use crate::trace::{ecv_debug, ecv_probe, ecv_trace, ecv_warn};
use core::ffi::c_void;

// Getting a blocked process back to the scheduler is done by plain RETURN, not
// by unwinding. `__remill_syscall_tranpoline_call` sets `ctx.unwinding`; the
// fork_emulation codegen tests it via `_ecv_suspended` after every syscall and
// every lifted call and returns immediately when set (elfconv patch 0026,
// TraceLifter::Impl::AddSuspendCheck). The scheduler in entry.rs reads and
// clears the flag once the leg is back.
//
// This replaced the EH shim (runtime/cshim/ecv_sjlj.c: setjmp/longjmp lowered to
// wasm EH, then `wasm-opt --translate-to-exnref`), which in turn replaced
// asyncify. Two reasons: an exnref module is rejected by every released runwasi
// shim -- measured, both engines -- and an EH unwind returns each function
// BEFORE its epilogue, so the `__stack_pointer` global was never popped and
// leaked one frame per yield until it wrapped. A real `ret` runs the real
// epilogue and needs no proposal.
extern "C" {
    /// Current wasm `__stack_pointer` value (shadow-stack low-water probe).
    pub fn ecv_cur_sp() -> usize;
}

/// Non-zero once a syscall has suspended this process, until the scheduler picks
/// the leg back up. The lifted code calls this after every syscall and every
/// call to another lifted function; a non-zero answer means "return now".
#[no_mangle]
pub unsafe extern "C" fn _ecv_suspended(_ctx: *mut EcvContext) -> u8 {
    crate::context::unwinding() as u8
}

// Shadow-stack low-water tracking for the RAPTORMARK_ECV_LEGSP probe. Records the
// lowest `__stack_pointer` seen at any guest call so we can see whether the wasm
// C stack descends in step with guest call depth or leaks decoupled from it.
static LEGSP_MIN: core::sync::atomic::AtomicUsize =
    core::sync::atomic::AtomicUsize::new(usize::MAX);

#[inline]
unsafe fn mem(ctx: *mut EcvContext, addr: u64) -> *mut u8 {
    (*ctx).arena.translate(addr)
}

// --- Guest memory access -------------------------------------------------

macro_rules! rw_memory {
    ($read:ident, $write:ident, $t:ty) => {
        #[no_mangle]
        pub unsafe extern "C" fn $read(ctx: *mut EcvContext, addr: u64) -> $t {
            (mem(ctx, addr) as *const $t).read_unaligned()
        }
        #[no_mangle]
        pub unsafe extern "C" fn $write(ctx: *mut EcvContext, addr: u64, v: $t) {
            (mem(ctx, addr) as *mut $t).write_unaligned(v)
        }
    };
}

rw_memory!(__remill_read_memory_8, __remill_write_memory_8, u8);
rw_memory!(__remill_read_memory_16, __remill_write_memory_16, u16);
rw_memory!(__remill_read_memory_32, __remill_write_memory_32, u32);
rw_memory!(__remill_read_memory_64, __remill_write_memory_64, u64);
rw_memory!(__remill_read_memory_128, __remill_write_memory_128, u128);
rw_memory!(__remill_read_memory_f32, __remill_write_memory_f32, f32);
rw_memory!(__remill_read_memory_f64, __remill_write_memory_f64, f64);

// float128_t at the ABI level; upstream's write is a no-op and its read
// aborts, but plain 16-byte moves are strictly better.
#[no_mangle]
pub unsafe extern "C" fn __remill_read_memory_f128(ctx: *mut EcvContext, addr: u64) -> u128 {
    (mem(ctx, addr) as *const u128).read_unaligned()
}
#[no_mangle]
pub unsafe extern "C" fn __remill_write_memory_f128(ctx: *mut EcvContext, addr: u64, v: u128) {
    (mem(ctx, addr) as *mut u128).write_unaligned(v)
}

// --- Control flow --------------------------------------------------------

/// How many times one VMA may appear on the guest stack before it is named.
///
/// Paired with `diag::max_depth()`, not used alone: an interpreter re-enters its
/// eval loop once per interpreted frame, so a high repeat count is normal there
/// and only becomes evidence alongside an absurd depth.
const RECURSION_REPEAT_ALARM: usize = 16;

/// Names a lifted function that is re-entering itself without bound, before the
/// guest stack pointer runs off the arena.
///
/// Without this, unbounded recursion surfaces as a raw wasm trap -- "out of
/// bounds memory access ... offset 0xffffffd8" plus a list of *wasm function
/// indices*, which cannot be mapped back to guest addresses because the module
/// carries no name section. That is a dead end: the one thing you need to know
/// (which guest function) is the one thing the trap does not tell you.
///
/// The scan only runs once the stack is already absurd, so the cost on a healthy
/// run is a single length comparison per call.
unsafe fn report_runaway_recursion(ctx: *mut EcvContext, t_fun_vma: u64) {
    let depth = (*ctx).call_history.len();
    if depth < crate::diag::max_depth() {
        // `note_depth` moved to the cold path. It is gated on `debug_log`, which
        // `hot_slow` already covers, so leaving its gate here cost a second
        // atomic load on every guest BL to answer a question `hot_slow` had
        // already answered.
        return;
    }
    let repeats = (*ctx)
        .call_history
        .iter()
        .filter(|(f, _)| *f == t_fun_vma)
        .count();
    if repeats < RECURSION_REPEAT_ALARM {
        return;
    }
    // The innermost frames name the cycle: which functions call each other, and
    // from which call sites. Distinct VMAs matter more than raw depth -- a
    // self-call and a two-function mutual recursion need different fixes.
    let mut cycle: Vec<String> = Vec::new();
    let mut seen: Vec<u64> = Vec::new();
    for (f, r) in (*ctx).call_history.iter().rev().take(64) {
        if !seen.contains(f) {
            seen.push(*f);
            cycle.push(format!("fn=0x{f:x}@ret=0x{r:x}"));
        }
    }
    eprintln!(
        "[ecvisor] runaway recursion: fn 0x{t_fun_vma:x} is on the guest stack {repeats} times \
         at depth {}",
        (*ctx).call_history.len()
    );
    eprintln!(
        "[ecvisor]   distinct frames in the innermost 64 ({}): {}",
        cycle.len(),
        cycle.join(" ")
    );
    fatal!(
        "runaway recursion at fn 0x{t_fun_vma:x} (depth {})",
        (*ctx).call_history.len()
    )
}

/// BLR: exact-entry dispatch through the lifted function table.
#[no_mangle]
pub unsafe extern "C" fn __remill_function_call(
    arena_ptr: *mut u8,
    state: *mut State,
    t_fun_vma: u64,
    ctx: *mut EcvContext,
) {
    match (*ctx).func_at(t_fun_vma) {
        Some(f) => {
            report_runaway_recursion(ctx, t_fun_vma);
            (*state).gpr.pc.val = t_fun_vma;
            if sys::legsp() {
                use core::sync::atomic::Ordering;
                let sp = ecv_cur_sp();
                if sp < LEGSP_MIN.load(Ordering::Relaxed) {
                    LEGSP_MIN.store(sp, Ordering::Relaxed);
                    ecv_probe!(legsp, "FCALL new-min sp=0x{sp:x} t_vma=0x{t_fun_vma:x}");
                }
            }
            f(arena_ptr, state, t_fun_vma, ctx);
        }
        None => {
            // This arm ends in `fatal!`, so adopting here costs the hot path
            // NOTHING -- unlike the `Some` arm, where a guard would sit on every
            // indirect call. Without it the stack dumped below is short by every
            // frame the inlined fast path pushed, which is precisely the
            // information this diagnostic exists to provide.
            (*ctx).adopt_call_history_depth();
            let lr = (*state).gpr.x[30].val;
            // An indirect call to an address with no lifted function is almost
            // always a null or stale function POINTER, not a lifting gap, so
            // the useful question is who loaded it. `lr` alone cannot answer
            // that -- it is whatever the last call site set, which for a
            // pointer read out of a table is unrelated. Dump the guest call
            // stack (innermost first) and the argument registers, which between
            // them name the caller and usually the table.
            let frames: Vec<String> = (*ctx)
                .call_history
                .iter()
                .rev()
                .take(16)
                .map(|(f, r)| format!("fn=0x{f:x}@ret=0x{r:x}"))
                .collect();
            eprintln!("[ecvisor]   guest stack: {}", frames.join(" "));
            let x = &(*state).gpr.x;
            eprintln!(
                "[ecvisor]   x0={:#x} x1={:#x} x2={:#x} x3={:#x} x19={:#x} x20={:#x} x21={:#x} x22={:#x} sp={:#x}",
                x[0].val, x[1].val, x[2].val, x[3].val,
                x[19].val, x[20].val, x[21].val, x[22].val,
                (*state).gpr.sp.val
            );
            fatal!("vma 0x{t_fun_vma:x} not in the lifted function table (__remill_function_call) lr=0x{lr:x}")
        }
    }
}

/// Indirect branch: resolve the function CONTAINING the target and enter it at
/// that VMA. Reached only when the target was not among the blocks elflift
/// discovered for the branching function, i.e. via `L_far_jump`.
#[no_mangle]
pub unsafe extern "C" fn __remill_jump(
    arena_ptr: *mut u8,
    state: *mut State,
    t_vma: u64,
    ctx: *mut EcvContext,
) {
    match (*ctx).func_containing(t_vma) {
        Some(f) => {
            (*state).gpr.pc.val = t_vma;
            if sys::legsp() {
                use core::sync::atomic::Ordering;
                let sp = ecv_cur_sp();
                if sp < LEGSP_MIN.load(Ordering::Relaxed) {
                    LEGSP_MIN.store(sp, Ordering::Relaxed);
                    ecv_probe!(legsp, "JMP new-min sp=0x{sp:x} t_vma=0x{t_vma:x}");
                }
            }
            f(arena_ptr, state, t_vma, ctx);
        }
        None => {
            let g = &(*state).gpr;
            eprintln!(
                "[ecv] __remill_jump MISS t_vma=0x{t_vma:x} pc=0x{:x} lr=0x{:x} fp=0x{:x} x13=0x{:x} x0=0x{:x} x1=0x{:x}",
                g.pc.val, g.x[30].val, g.x[29].val, g.x[13].val, g.x[0].val, g.x[1].val
            );
            fatal!("vma 0x{t_vma:x} not in the lifted function table (__remill_jump)")
        }
    }
}

#[no_mangle]
pub unsafe extern "C" fn __remill_function_return(
    _arena_ptr: *mut u8,
    _state: *mut State,
    _fn_ret_vma: u64,
    _ctx: *mut EcvContext,
) {
}

#[no_mangle]
pub unsafe extern "C" fn __remill_missing_block(
    _arena_ptr: *mut u8,
    state: *mut State,
    _vma: u64,
    _ctx: *mut EcvContext,
) {
    ecv_warn!(
        ecvisor,
        "warning: reached __remill_missing_block, PC: 0x{:016x}",
        (*state).pc()
    );
}

#[no_mangle]
pub unsafe extern "C" fn __remill_async_hyper_call(
    _arena_ptr: *mut u8,
    _state: *mut State,
    _ret_addr: u64,
    _ctx: *mut EcvContext,
) {
}

#[no_mangle]
pub unsafe extern "C" fn __remill_error(
    _arena_ptr: *mut u8,
    _state: *mut State,
    addr: u64,
    _ctx: *mut EcvContext,
) {
    // Reached by the `kCategoryError` instructions: an undecodable opcode, or a
    // trap the guest executed deliberately (aarch64 `brk`, which on Linux raises
    // SIGTRAP and terminates). Either way the guest cannot continue, and saying
    // so loudly matters -- when `brk` was lifted as a no-op it fell through and
    // musl's `exit` returned, which surfaced hundreds of instructions later as
    // an unrelated null call.
    let lr = (*_state).gpr.x[30].val;
    fatal!("guest trap or undecodable instruction at PC 0x{addr:x} (__remill_error) lr=0x{lr:x}");
}

#[no_mangle]
pub unsafe extern "C" fn __ecv_warning(
    _arena_ptr: *mut u8,
    _state: *mut State,
    addr: u64,
    ctx: *mut EcvContext,
) {
    // An instruction the lifter could not translate IS an illegal instruction,
    // and SIGILL is how a machine reports one. Aborting instead does not merely
    // lose information -- it removes a recovery the guest has already arranged.
    //
    // PostgreSQL is the case in point. It decides whether to use the ARMv8
    // CRC32C path by EXECUTING it under a SIGILL handler:
    //
    //   pqsignal(SIGILL, h); if (sigsetjmp(env, 1) == 0) { pg_comp_crc32c_armv8(...); }
    //
    // On a CPU without CRC32 the handler siglongjmps and it uses the software
    // implementation. Killing the module denied it that answer, on an
    // instruction it was only ever probing for.
    //
    // With no handler installed (or with SIGILL blocked) there is nothing to
    // deliver to and the old fatal is still the honest outcome.
    //
    // ⚠️ THE DISPOSITION IS DECIDED BEFORE THE SIGNAL IS POSTED, and the order
    // is the whole of a fix, not a tidy-up. `deliver_pending_signals` ends with
    //
    //     if let Some(sig) = self.pending_termination() { self.arm_signal_exit(sig); }
    //
    // so BEFORE it returns 0 -- before this function can even ask what to do
    // about "nothing handled it" -- SIGILL's default action has already set
    // `Pending::Exit(128 + SIGILL)` and `suspended = true`. Under `fatal!` that
    // never mattered: the process was about to abort anyway, which is exactly
    // what `arm_signal_exit`'s "normally unreachable rubble" note describes.
    //
    // The census arm RETURNS, and it inherited the rubble. It consumed the
    // pending signal BIT and nothing cleared the armed exit, so the svc
    // trampoline picked `suspended` up at the guest's NEXT SYSCALL: that
    // syscall completed, the leg unwound, and the module exited 132 with no
    // diagnostic. One census line per run, from an instrument that exists to
    // enumerate every undecoded site a workload executes -- and a list of one
    // that says nothing about being truncated reads as a complete list, which is
    // the precise false completeness this mode was built to prevent.
    //
    // Asking first is what makes that unreachable rather than undone. The
    // alternative -- post as before, then clear `pending`/`suspended` in the
    // census arm -- has to CANCEL a termination, and it cannot tell the one it
    // armed from one that was already due (a real `kill`, a SIGKILL, a
    // `pending_termination` that names some other signal entirely). Cancelling a
    // real termination is a far worse defect than the one being fixed, so
    // nothing here cancels anything: with no handler and the census armed this
    // never posts at all, and `arm_signal_exit` keeps its contract that what it
    // arms stays armed.
    //
    // The handler test is `delivers_to_handler`, which applies exactly the two
    // checks the delivery loop applies (`deliverable_set`'s blocked-mask
    // subtraction, then a disposition that is a real address rather than SIG_DFL
    // or SIG_IGN). A guest WITH a SIGILL handler therefore takes the identical
    // path it always did, censused or not -- postgres's CRC32C probe is not a
    // diagnostic it can lose, it is how it decides what its CPU can do.
    if matches!(
        crate::diag::undecoded_disposition(),
        crate::diag::Undecoded::Census
    ) && !(*ctx).delivers_to_handler(crate::context::SIGILL)
    {
        // ⚠️ UNSOUND, and not a "get further" switch. The instruction's effect
        // is NEVER APPLIED: the register or memory location it should have
        // written keeps its old value, and every result computed from it
        // afterwards is wrong. The guest can print nonsense, spin, or die far
        // away from here for reasons that have nothing to do with this site. It
        // exists to answer "which undecoded sites does a real workload EXECUTE"
        // in ONE run instead of one run per site, because the static inventory
        // ranks by sites PRESENT and that ranking has already been wrong once
        // (patch 0063 cleared 706 `tbl` sites and moved nothing; 0064 cleared 11
        // and unblocked the planner). See `diag::set_undec_census`.
        //
        // Deduped by unique ADDRESS and capped, following
        // `context::bbmiss_first_time`: an occurrence cap lets one hot site
        // crowd out every rare one, and an instrument verbose enough to change
        // the outcome has stopped measuring the thing. The first occurrence is
        // LOGGED, so the site list falls out of the log with no dump step --
        // `grep '\[undec_census\] '`.
        //
        // `ecv_probe!` re-tests the gate that is already true here. It is kept
        // for the naming rule it enforces: the category, the `diag` accessor and
        // the environment variable are one name, so an operator can read
        // `[undec_census]` off stderr and know which variable produced it.
        match crate::diag::undec_census_note(addr) {
            crate::diag::Census::New => {
                ecv_probe!(undec_census, "{}", crate::diag::undec_census_message(addr))
            }
            crate::diag::Census::Truncated => ecv_probe!(
                undec_census,
                "{}",
                crate::diag::undec_census_truncated_message()
            ),
            crate::diag::Census::Seen => {}
        }
        return;
    }
    let pid = (*ctx).current_pid();
    // Thread-directed: a fault belongs to the task that took it, and a sibling
    // must not be able to dequeue this thread's SIGILL and "handle" a trap it
    // never hit.
    (*ctx).post_signal_to_thread(pid, crate::context::SIGILL);
    let blocked = (*ctx).task_signals.blocked;
    if (*ctx).deliver_pending_signals(blocked) == 0 {
        // ⚠️ The ENCODING, not just the address. This message used to read "no
        // lifted instruction at 0x...", which names a missing BASIC BLOCK -- the
        // signature of a block-discovery regression -- when the cause is a
        // missing INSTRUCTION in the decoder. On 2026-08-19 that wording cost
        // about an hour: postgres stopped at 0x9bd3f4 while a lifter patch that
        // changes block discovery was under test, and everything pointed at the
        // patch until the address was disassembled by hand and turned out to be
        // `fnmul`, which no `patches/` entry decodes.
        //
        // ⚠️ CORRECTED 2026-08-21. This used to claim "the runtime can read the
        // guest word at `addr` -- `block_address` in context.rs already does --
        // so it says which instruction". BOTH halves are false, and the claim
        // outlived the code it described.
        //
        // The runtime CANNOT read guest text. The arena is filled only by
        // `Arena::load_data_sections` out of `EcvProgram`'s `data_*` tables, and
        // those are built by elfconv's `SetDataSections`
        // (`lifter/MainLifter.cpp`), whose loop opens with `continue` on
        // `SEC_TYPE_CODE` -- which `Binary/Loader.cpp` assigns to any section
        // carrying bfd's `SEC_CODE`. No patch in `patches/` touches either site.
        // So `.text` is excluded by construction, and the read that was here
        // printed `0x00000000` for an address holding `1e688800 fnmul` -- a
        // valid aarch64 word (`udf #0`) that reads as an answer. It was removed;
        // `diag::undecoded_message` documents the removal and deliberately
        // prints NO encoding, only the address and how to disassemble it.
        //
        // The paragraph above is still exactly right about WHY the wording
        // matters, which is why it is kept.
        //
        // ⚠️ UNCONDITIONAL, and the census arm above is why it can be. Reaching
        // here means either the disposition is `Fatal`, or `delivers_to_handler`
        // said a handler would run -- and a handler that would run increments
        // `ran`, because the only path to its arm consumes the bit and calls
        // `run_signal_handler`. So a zero count here is "no handler, and the
        // operator did not ask for a census", which has exactly one honest
        // outcome. Re-testing the disposition at this point would put the census
        // back BEHIND `deliver_pending_signals`, i.e. behind the armed exit this
        // function's header is about.
        fatal!("{}", crate::diag::undecoded_message(addr));
    }
    ecv_debug!(
        ecv,
        "undecoded instruction at {addr:#x} -> SIGILL delivered"
    );
}

/// Diagnostic guard for wild (out-of-arena) stores: the store macros call this
/// only when the target address is >= the arena size, so a lifted miscompile that
/// computes a near-null / wild pointer is caught WITH its guest PC instead of an
/// opaque wasm OOB trap. Aborts after logging.
#[no_mangle]
pub unsafe extern "C" fn __ecv_wild_store(ctx: *mut EcvContext, addr: u64, size: u64) {
    let pc = {
        let st = (*ctx).live_state;
        if st.is_null() {
            0
        } else {
            (*st).pc()
        }
    };
    fatal!(
        "[ecv-wildstore] {size}-byte store to out-of-arena addr 0x{addr:x} at guest pc=0x{pc:x}"
    );
}

#[no_mangle]
pub unsafe extern "C" fn __remill_syscall_tranpoline_call(
    arena_ptr: *mut u8,
    state: *mut State,
    ctx: *mut EcvContext,
) {
    // RAPTORMARK_ECV_TRACE: print the syscall nr before dispatching to sys::svc.
    // `pid=` is gone from the format string -- the subscriber appends it to every
    // TRACE line, which is also why `(*ctx).current_pid()` is no longer read here.
    {
        let s = &*state;
        ecv_trace!(svctramp, "ENTER nr={} pc={:#x}", s.syscall_nr(), s.pc());
    }
    // The inline fast path bumps `__ecv_ch_len` without Rust seeing it, and a
    // syscall is where Rust reads the history for real: `fork` snapshots it to
    // build the child's replay. Adopting only in the `_ic` call-history
    // variants left that snapshot short by every inline-pushed frame, and the
    // child resumed into the wrong caller -- a forking guest produced NO output
    // and exited 0.
    //
    // Guarded, and a syscall is rare next to a guest BL, so unlike the per-call
    // guards this costs the default path nothing measurable.
    (*ctx).adopt_call_history_depth();
    sys::svc(arena_ptr, &mut *state, &mut *ctx);
    (*ctx).publish_call_history();
    ecv_trace!(svctramp, "DONE  nr={}", (*state).syscall_nr());
    // A blocking syscall (or exit/execve) set ctx.suspended. Capture the replay
    // state (unless this is an execve's fresh-reentry yield), then raise
    // `unwinding` and RETURN. The lifted SVC site tests it right here via
    // `_ecv_suspended` and returns; so does every caller up the chain, each
    // running its own epilogue and skipping its `_ecv_func_epilogue` -- which is
    // what keeps the call history intact for replay. Doing the capture HERE (not
    // deep in svc) keeps the return path short and free of Drop state.
    if (*ctx).suspended {
        (*ctx).suspended = false;
        (*ctx).on_suspend();
        ecv_trace!(svctramp, "YIELD nr={}", (*state).syscall_nr());
        crate::context::set_unwinding(true);
    }
}

#[no_mangle]
pub unsafe extern "C" fn __remill_mark_as_used(_mem: *mut c_void) {}

#[no_mangle]
pub unsafe extern "C" fn _ecv_get_indirectbr_block_address(
    ctx: *mut EcvContext,
    fun_vma: u64,
    bb_vma: u64,
) -> *mut u64 {
    (*ctx).block_address(fun_vma, bb_vma)
}

#[no_mangle]
pub unsafe extern "C" fn _ecv_noopt_get_bb(
    ctx: *mut EcvContext,
    cur_fun_vma: u64,
    t_vma: u64,
) -> *mut u64 {
    (*ctx).block_address_strict(cur_fun_vma, t_vma)
}

#[no_mangle]
pub unsafe extern "C" fn _ecv_unreached(value: u64) {
    fatal!("_ecv_unreached hit. value: 0x{value:x}");
}

// Call-history hooks (fork_emulation=1). Maintain a per-process stack of
// {func_vma, return_pc} frames: pushed at each lifted function's prologue,
// popped at its normal epilogue. This is the raw material for fork-by-replay
// (context.rs replays the child's stack from the parent's saved history instead
// of asyncify-rewinding the lifted glibc fork frames, which cannot be rewound).
//
// The history stays consistent across a suspend/resume because BOTH directions
// skip their half: a suspending frame returns from `AddSuspendCheck`, which sits
// before the caller's `_ecv_func_epilogue`, so nothing is popped on the way out;
// and the scheduler re-enters a replayed frame at its post-call pc, which is
// past that same epilogue, so nothing is re-pushed. entry.rs does the one pop
// that re-entry skipped, in lockstep with `remaining`.
#[no_mangle]
pub unsafe extern "C" fn _ecv_save_call_history(
    state: *mut State,
    ctx: *mut EcvContext,
    func_addr: u64,
    ret_addr: u64,
) {
    // The inline fast path may have pushed frames Rust has not seen; adopt them
    // before adding ours, and republish afterwards in case the push moved the
    // buffer. Both are no-ops unless the inline path is on.
    (*ctx).call_history.push((func_addr, ret_addr));
    // Every guest BL passes through here, which is why the recursion check
    // belongs here and not only on the indirect path: a DIRECT recursive call
    // never reaches `__remill_function_call`, so an alarm placed only there
    // stays silent through exactly the runaway it was written to catch.
    report_runaway_recursion(ctx, func_addr);
    // ONE test for the five diagnostics below, all of which are off in every
    // normal run. They are not free to skip individually: each costs a relaxed
    // atomic load and a branch, on a function that runs once per guest BL. The
    // cold path re-reads each real gate, so this summary can only cost a
    // diagnostic, never change behaviour. See diag::HOT_SLOW.
    if crate::diag::hot_slow() {
        // Peak-depth tracking lives here now, not in `report_runaway_recursion`:
        // it is a diagnostic, gated on `debug_log`, which `hot_slow` covers.
        crate::diag::note_depth((*ctx).call_history.len());
        save_call_history_diagnostics(state, ctx, func_addr, ret_addr);
    }
    // Maintain the link register. In fork-emulation the lifted bl/blr lowering
    // (TraceLifter.cpp) routes returns through `call_history` and SKIPS the CALL
    // semantic that writes x30, so x30 goes stale. Normal returns don't care, but
    // code that reads the link register directly -- glibc's setjmp saving the
    // return address, __builtin_return_address, PAC LR signing -- then gets
    // garbage. That is exactly what broke PostgreSQL's PG_TRY/PG_CATCH
    // (sigsetjmp saved a bogus LR -> siglongjmp jumped to it -> __remill_jump
    // miss). Writing x30 = ret_addr here restores the ABI-correct link register
    // on every call, matching what the CALL semantic does in non-fork lifting.
    //
    // SEMANTIC, not diagnostic: it stays on the fast path.
    (*state).gpr.x[30].val = ret_addr;
}

/// The diagnostics `_ecv_save_call_history` runs only when one of them is armed.
/// Split out and marked cold so the hot path is a push, a depth check, one flag
/// test and the x30 store.
#[cold]
#[inline(never)]
unsafe fn save_call_history_diagnostics(
    state: *mut State,
    ctx: *mut EcvContext,
    func_addr: u64,
    ret_addr: u64,
) {
    // Call-site counter. Every guest BL passes through here, so counting one
    // return address counts executions of one call site exactly -- which is how
    // you tell "this ran once and its counters are corrupt" from "this really
    // did run a million times". Off unless RAPTORMARK_ECV_COUNTRET names a
    // return address in hex; reported by the deadlock path.
    if sys::count_ret() == ret_addr {
        sys::bump_ret_count();
    }
    // Watchpoint. The lifted module has its own inlined LoadMem/StoreMem, so a
    // runtime hook cannot see individual guest stores -- but every guest BL
    // passes through here, so sampling one word at each call narrows a stray
    // write to the function that made it. RAPTORMARK_ECV_WATCH=<hex vma>.
    if crate::diag::watch() {
        let w = crate::diag::watch_addr();
        let n = crate::diag::watch_len();
        let cur = (*ctx).arena.slice(w, n).to_vec();
        let pid = crate::trace::current_pid();
        let prev = watch_prev();
        if crate::diag::watch_verdict(prev.as_ref().map(|(p, b)| (*p, b.as_slice())), pid, &cur)
            == crate::diag::WatchVerdict::Changed
        {
            let was = &prev.as_ref().expect("Changed implies a previous sample").1;
            for (i, (a, b)) in was.chunks(8).zip(cur.chunks(8)).enumerate() {
                if a != b {
                    let (av, bv) = (
                        u64::from_le_bytes(a.try_into().unwrap_or([0; 8])),
                        u64::from_le_bytes(b.try_into().unwrap_or([0; 8])),
                    );
                    ecv_probe!(
                        watch,
                        "{:#x} (+{}): {av:#x} -> {bv:#x}  (fn={func_addr:#x} ret={ret_addr:#x})",
                        w + (i * 8) as u64,
                        i * 8
                    );
                }
            }
        }
        set_watch_prev(pid, cur);
    }
    // Shadow-stack low-water probe: sample __stack_pointer at each guest call and
    // log new lows with the guest call depth. If SP descends while depth stays
    // small, the wasm C stack is leaking frames decoupled from guest recursion.
    if sys::legsp() {
        use core::sync::atomic::Ordering;
        let sp = ecv_cur_sp();
        // Only the descending edge matters; ignore the common no-new-low case cheaply.
        if sp < LEGSP_MIN.load(Ordering::Relaxed) {
            LEGSP_MIN.store(sp, Ordering::Relaxed);
            // The pid this line used to read out of `procs` by hand now comes
            // from the subscriber.
            ecv_probe!(
                legsp,
                "new-min sp=0x{:x} depth={} func=0x{:x} ret=0x{:x}",
                sp,
                (*ctx).call_history.len(),
                func_addr,
                ret_addr
            );
        }
    }
    // Differential value tracer (args): at the call prologue, State.x0..x2 hold the
    // callee's first three arguments. When RAPTORMARK_ECV_DTRACE_LO/_HI bound this
    // call site's return address, log them so a pointer argument (e.g. an OSSL_LIB_CTX)
    // can be tracked as it flows down a call chain and pinned at the exact site where
    // it turns NULL. Diagnostic; off by default.
    if let Some((lo, hi)) = sys::dtrace_range() {
        let minpid = sys::dtrace_minpid();
        let pid = (&(*ctx).procs)[(*ctx).current].pid as u64;
        if ret_addr >= lo && ret_addr < hi && pid >= minpid {
            let x1 = (*state).gpr.x[1].val;
            // No `pid=` in the format string: it names the CURRENT process, and
            // `ecv_probe!` already supplies that from `trace::CURRENT_PID`. Two
            // `pid=` tokens on one line is the failure the pid rule exists for.
            ecv_probe!(
                dtrace,
                "CALL  ret=0x{:x} (from fn 0x{:x}) x0=0x{:x} x1=0x{:x} x2=0x{:x} sp=0x{:x}",
                ret_addr,
                func_addr,
                (*state).gpr.x[0].val,
                x1,
                (*state).gpr.x[2].val,
                (*state).gpr.sp.val
            );
            if sys::dtrace_regs() {
                let r: Vec<String> = (19..=28)
                    .map(|i| format!("x{i}={:#x}", (*state).gpr.x[i].val))
                    .collect();
                ecv_probe!(dtrace, "REGS  {}", r.join(" "));
            }
            let dump = sys::dtrace_dump();
            if dump > 0 && x1 != 0 {
                let n = dump as usize;
                let p = mem(ctx, x1);
                let bytes = core::slice::from_raw_parts(p, n);
                let hex: String = bytes.iter().map(|b| format!("{:02x}", b)).collect();
                ecv_probe!(dtrace, "DUMP  @0x{:x} [{}]: {}", x1, n, hex);
            }
            let dumpx0 = sys::dtrace_dumpx0();
            let x0 = (*state).gpr.x[0].val;
            if dumpx0 > 0 && x0 != 0 {
                let n = dumpx0 as usize;
                let p = mem(ctx, x0);
                let bytes = core::slice::from_raw_parts(p, n);
                let hex: String = bytes.iter().map(|b| format!("{:02x}", b)).collect();
                ecv_probe!(dtrace, "DUMPX0 @0x{:x} [{}]: {}", x0, n, hex);
            }
        }
    }
}

/// Previous contents of the watched window.
///
/// Here rather than in `diag` with the watchpoint's ADDRESS and LENGTH, and the
/// split is the point: `diag` holds configuration that is read once at startup
/// and never changes, which is what makes it safe across this runtime's fork
/// emulation. This is per-call mutable state, and its only readers are the two
/// functions below.
///
/// A plain static is adequate because the guest is single-threaded from the
/// host's point of view -- ecvisor's processes are cooperative contexts on one
/// wasm thread -- so this avoids a lock on a per-call path.
///
/// Carries the OWNING PID with the bytes. The probe reads the live arena, which
/// a context switch swaps wholesale, so a sample taken after one is not
/// comparable to the sample before it -- see `diag::watch_verdict`.
static mut WATCH_PREV: Option<(u32, Vec<u8>)> = None;

fn watch_prev() -> Option<(u32, Vec<u8>)> {
    unsafe { (*core::ptr::addr_of!(WATCH_PREV)).clone() }
}

fn set_watch_prev(pid: u32, v: Vec<u8>) {
    unsafe { *core::ptr::addr_of_mut!(WATCH_PREV) = Some((pid, v)) }
}

// --- Inline-capable variants (elfconv patch 0060) -------------------------
//
// The lifted code calls THESE, and only these, when built with
// `--inline-call-history`. The two functions above stay byte-identical to what
// they were before that patch, which is the entire point.
//
// WHY A SECOND SYMBOL RATHER THAN A FLAG. The obvious design was one function
// with `if !ctx.ch_inline { return; }` guards around the reconciliation. It was
// measured, twice, and it cost the DEFAULT path 6.5-10%: four predictable
// branches per guest BL, two million of them, on an interpreter where every
// instruction is a dispatch. Removing the guards restored parity exactly
// (3.49 s -> 3.18 s against 3.24 s for the pre-patch build), which is what
// proved the branches themselves were the cost rather than anything they
// guarded.
//
// So the choice is made at LIFT time, by which symbol is emitted, and the
// default path carries no evidence that this feature exists.

/// `_ecv_save_call_history` for a module built with the inline fast path.
///
/// Reached only on the slow arm -- diagnostics armed, or the buffer full -- so
/// its own cost is irrelevant. It adopts whatever depth the guest pushed
/// inline, does the push (which may reallocate), and republishes the buffer.
#[no_mangle]
pub unsafe extern "C" fn _ecv_save_call_history_ic(
    state: *mut State,
    ctx: *mut EcvContext,
    func_addr: u64,
    ret_addr: u64,
) {
    (*ctx).adopt_call_history_depth();
    _ecv_save_call_history(state, ctx, func_addr, ret_addr);
    (*ctx).publish_call_history();
}

/// `_ecv_func_epilogue` for a module built with the inline fast path.
#[no_mangle]
pub unsafe extern "C" fn _ecv_func_epilogue_ic(state: *mut State, ctx: *mut EcvContext) {
    (*ctx).adopt_call_history_depth();
    _ecv_func_epilogue(state, ctx);
    (*ctx).publish_call_history();
}

#[no_mangle]
pub unsafe extern "C" fn _ecv_func_epilogue(state: *mut State, ctx: *mut EcvContext) {
    // Same split as `_ecv_save_call_history`: one flag test instead of the
    // dtrace range check, on a function that runs once per guest return.
    if crate::diag::hot_slow() {
        func_epilogue_diagnostics(state, ctx);
    }
    (*ctx).call_history.pop();
}

/// The dtrace hook `_ecv_func_epilogue` runs only when it is armed.
#[cold]
#[inline(never)]
unsafe fn func_epilogue_diagnostics(state: *mut State, ctx: *mut EcvContext) {
    // Differential value tracer: at each call site's epilogue, State.x0 holds the
    // callee's return value. When RAPTORMARK_ECV_DTRACE_LO/_HI bound the call
    // site's return address, log it so the lifted return value can be diffed
    // against the native gdb trace (nat_trace.py). Diagnostic; off by default.
    if let Some((lo, hi)) = sys::dtrace_range() {
        let minpid = sys::dtrace_minpid();
        let pid = (&(*ctx).procs)[(*ctx).current].pid as u64;
        if let Some(&(func_addr, ret_addr)) = (*ctx).call_history.last() {
            if ret_addr >= lo && ret_addr < hi && pid >= minpid {
                ecv_probe!(
                    dtrace,
                    "callsite ret=0x{:x} (in fn 0x{:x}) x0=0x{:x}",
                    ret_addr,
                    func_addr,
                    (*state).gpr.x[0].val
                );
            }
        }
    }
    // NO pop here. The caller does it, on both paths -- this function only
    // reports. Leaving the pop behind when this was split out would have popped
    // twice with dtrace armed, which is the kind of thing that surfaces as a
    // corrupted fork replay days later.
}

// --- Flag computations / comparisons ------------------------------------
// Variadic in C (`bool f(bool, ...)`); clang lowers the `...` to one
// va-buffer pointer on wasm32, so a two-argument definition matches.

macro_rules! flag_computation {
    ($($name:ident),*) => {$(
        #[no_mangle]
        pub unsafe extern "C" fn $name(result: bool, _va: *mut c_void) -> bool { result }
    )*};
}
flag_computation!(
    __remill_flag_computation_sign,
    __remill_flag_computation_zero,
    __remill_flag_computation_overflow,
    __remill_flag_computation_carry
);

macro_rules! compare {
    ($($name:ident),*) => {$(
        #[no_mangle]
        pub unsafe extern "C" fn $name(result: bool) -> bool { result }
    )*};
}
compare!(
    __remill_compare_sle,
    __remill_compare_slt,
    __remill_compare_sge,
    __remill_compare_sgt,
    __remill_compare_ule,
    __remill_compare_ult,
    __remill_compare_ugt,
    __remill_compare_uge,
    __remill_compare_eq,
    __remill_compare_neq
);

// --- Barriers / atomics (single-threaded supervisor) ---------------------

macro_rules! ctx_noop {
    ($($name:ident),*) => {$(
        #[no_mangle]
        pub unsafe extern "C" fn $name(_ctx: *mut EcvContext) {}
    )*};
}
ctx_noop!(
    __remill_barrier_load_load,
    __remill_barrier_load_store,
    __remill_barrier_store_load,
    __remill_barrier_store_store,
    __remill_atomic_begin,
    __remill_atomic_end
);

// elfconv routes a DECODED-BUT-UNIMPLEMENTED instruction (one remill can decode
// but has no DEF_ISEL semantic for) here via HandleUnsupported. The default is a
// silent no-op: the instruction's effect is simply dropped, which silently
// CORRUPTS data (e.g. an unlifted LD1/LD2 leaves the destination vector reg stale)
// with no fault -- the root cause of the pg_attribute catalog corruption. When
// RAPTORMARK_ECV_DEBUG is set, log the guest PC so the skipped instruction (and
// the remill semantics gap it needs) can be pinpointed.
#[no_mangle]
pub unsafe extern "C" fn __remill_aarch64_emulate_instruction(ctx: *mut EcvContext) {
    let st = (*ctx).live_state;
    if !st.is_null() {
        // Literal category: `[ecv-emulate]` is not a valid Rust identifier, and
        // the prefix is kept rather than renamed.
        ecv_debug!(
            "ecv-emulate",
            "SKIPPED unsupported instruction at pc=0x{:x}",
            (*st).pc()
        );
    }
}

// Single-threaded exclusive-monitor semantics: implemented for real (upstream
// aborts on these).
macro_rules! compare_exchange {
    ($name:ident, $t:ty) => {
        #[no_mangle]
        pub unsafe extern "C" fn $name(
            ctx: *mut EcvContext,
            addr: u64,
            expected: *mut $t,
            desired: $t,
        ) {
            let p = mem(ctx, addr) as *mut $t;
            let cur = p.read_unaligned();
            if cur == expected.read_unaligned() {
                p.write_unaligned(desired);
            } else {
                expected.write_unaligned(cur);
            }
        }
    };
}
compare_exchange!(__remill_compare_exchange_memory_8, u8);
compare_exchange!(__remill_compare_exchange_memory_16, u16);
compare_exchange!(__remill_compare_exchange_memory_32, u32);
compare_exchange!(__remill_compare_exchange_memory_64, u64);

#[no_mangle]
pub unsafe extern "C" fn __remill_compare_exchange_memory_128(
    ctx: *mut EcvContext,
    addr: u64,
    expected: *mut u128,
    desired: *mut u128,
) {
    let p = mem(ctx, addr) as *mut u128;
    let cur = p.read_unaligned();
    if cur == expected.read_unaligned() {
        p.write_unaligned(desired.read_unaligned());
    } else {
        expected.write_unaligned(cur);
    }
}

macro_rules! fetch_and {
    ($name:ident, $t:ty, $op:expr) => {
        #[no_mangle]
        pub unsafe extern "C" fn $name(ctx: *mut EcvContext, addr: u64, value: *mut $t) {
            let p = mem(ctx, addr) as *mut $t;
            let old = p.read_unaligned();
            #[allow(clippy::redundant_closure_call)]
            p.write_unaligned($op(old, value.read_unaligned()));
            value.write_unaligned(old);
        }
    };
}
macro_rules! fetch_and_family {
    ($add:ident, $sub:ident, $and:ident, $or:ident, $xor:ident, $nand:ident, $t:ty) => {
        fetch_and!($add, $t, |a: $t, b: $t| a.wrapping_add(b));
        fetch_and!($sub, $t, |a: $t, b: $t| a.wrapping_sub(b));
        fetch_and!($and, $t, |a: $t, b: $t| a & b);
        fetch_and!($or, $t, |a: $t, b: $t| a | b);
        fetch_and!($xor, $t, |a: $t, b: $t| a ^ b);
        fetch_and!($nand, $t, |a: $t, b: $t| !(a & b));
    };
}
fetch_and_family!(
    __remill_fetch_and_add_8,
    __remill_fetch_and_sub_8,
    __remill_fetch_and_and_8,
    __remill_fetch_and_or_8,
    __remill_fetch_and_xor_8,
    __remill_fetch_and_nand_8,
    u8
);
fetch_and_family!(
    __remill_fetch_and_add_16,
    __remill_fetch_and_sub_16,
    __remill_fetch_and_and_16,
    __remill_fetch_and_or_16,
    __remill_fetch_and_xor_16,
    __remill_fetch_and_nand_16,
    u16
);
fetch_and_family!(
    __remill_fetch_and_add_32,
    __remill_fetch_and_sub_32,
    __remill_fetch_and_and_32,
    __remill_fetch_and_or_32,
    __remill_fetch_and_xor_32,
    __remill_fetch_and_nand_32,
    u32
);
fetch_and_family!(
    __remill_fetch_and_add_64,
    __remill_fetch_and_sub_64,
    __remill_fetch_and_and_64,
    __remill_fetch_and_or_64,
    __remill_fetch_and_xor_64,
    __remill_fetch_and_nand_64,
    u64
);

// --- Miscellaneous remill intrinsics ------------------------------------

#[no_mangle]
pub unsafe extern "C" fn __remill_fpu_exception_test_and_clear(
    _read_mask: i32,
    clear_mask: i32,
) -> i32 {
    clear_mask
}

// Undefined values: any value satisfies the contract; return 0 quietly
// (upstream aborts on the non-8-bit variants, which is strictly worse).
#[no_mangle]
pub unsafe extern "C" fn __remill_undefined_8() -> u8 {
    0
}
#[no_mangle]
pub unsafe extern "C" fn __remill_undefined_16() -> u16 {
    0
}
#[no_mangle]
pub unsafe extern "C" fn __remill_undefined_32() -> u32 {
    0
}
#[no_mangle]
pub unsafe extern "C" fn __remill_undefined_64() -> u64 {
    0
}
#[no_mangle]
pub unsafe extern "C" fn __remill_undefined_f32() -> f32 {
    0.0
}
#[no_mangle]
pub unsafe extern "C" fn __remill_undefined_f64() -> f64 {
    0.0
}

// I/O ports (x86-only concept; aborting stubs like upstream).
#[no_mangle]
pub unsafe extern "C" fn __remill_read_io_port_8(_ctx: *mut EcvContext, _a: u64) -> u8 {
    fatal!("__remill_read_io_port_8");
}
#[no_mangle]
pub unsafe extern "C" fn __remill_read_io_port_16(_ctx: *mut EcvContext, _a: u64) -> u16 {
    fatal!("__remill_read_io_port_16");
}
#[no_mangle]
pub unsafe extern "C" fn __remill_read_io_port_32(_ctx: *mut EcvContext, _a: u64) -> u32 {
    fatal!("__remill_read_io_port_32");
}
#[no_mangle]
pub unsafe extern "C" fn __remill_write_io_port_8(_ctx: *mut EcvContext, _a: u64, _v: u8) {
    fatal!("__remill_write_io_port_8");
}
#[no_mangle]
pub unsafe extern "C" fn __remill_write_io_port_16(_ctx: *mut EcvContext, _a: u64, _v: u16) {
    fatal!("__remill_write_io_port_16");
}
#[no_mangle]
pub unsafe extern "C" fn __remill_write_io_port_32(_ctx: *mut EcvContext, _a: u64, _v: u32) {
    fatal!("__remill_write_io_port_32");
}

// Foreign-architecture stubs (`Memory * f(Memory *)` shape upstream): never
// legitimately reached for aarch64 guests.
macro_rules! foreign_stub {
    ($($name:ident),*) => {$(
        #[no_mangle]
        pub unsafe extern "C" fn $name(_m: *mut c_void) -> *mut c_void {
            fatal!(concat!(stringify!($name), " (foreign-arch intrinsic)"));
        }
    )*};
}
foreign_stub!(
    __remill_delay_slot_begin,
    __remill_delay_slot_end,
    __remill_x86_set_segment_es,
    __remill_x86_set_segment_ss,
    __remill_x86_set_segment_ds,
    __remill_x86_set_segment_fs,
    __remill_x86_set_segment_gs,
    __remill_x86_set_debug_reg,
    __remill_x86_set_control_reg_0,
    __remill_x86_set_control_reg_1,
    __remill_x86_set_control_reg_2,
    __remill_x86_set_control_reg_3,
    __remill_x86_set_control_reg_4,
    __remill_amd64_set_debug_reg,
    __remill_amd64_set_control_reg_0,
    __remill_amd64_set_control_reg_1,
    __remill_amd64_set_control_reg_2,
    __remill_amd64_set_control_reg_3,
    __remill_amd64_set_control_reg_4,
    __remill_amd64_set_control_reg_8,
    __remill_aarch32_emulate_instruction,
    __remill_aarch32_check_not_el2,
    __remill_sparc_set_asi_register,
    __remill_sparc_unimplemented_instruction,
    __remill_sparc_unhandled_dcti,
    __remill_sparc_window_underflow,
    __remill_sparc_trap_cond_a,
    __remill_sparc_trap_cond_n,
    __remill_sparc_trap_cond_ne,
    __remill_sparc_trap_cond_e,
    __remill_sparc_trap_cond_g,
    __remill_sparc_trap_cond_le,
    __remill_sparc_trap_cond_ge,
    __remill_sparc_trap_cond_l,
    __remill_sparc_trap_cond_gu,
    __remill_sparc_trap_cond_leu,
    __remill_sparc_trap_cond_cc,
    __remill_sparc_trap_cond_cs,
    __remill_sparc_trap_cond_pos,
    __remill_sparc_trap_cond_neg,
    __remill_sparc_trap_cond_vc,
    __remill_sparc_trap_cond_vs,
    __remill_sparc32_emulate_instruction,
    __remill_sparc64_emulate_instruction,
    __remill_ppc_emulate_instruction,
    __remill_ppc_syscall
);
