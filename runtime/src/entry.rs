//! Module entry point. wasi-libc's `crt1-command.c` `_start` resolves the C
//! `main` through `__main_argc_argv`, which we export here in place of
//! upstream `Entry.cpp`'s `main`.
//!
//! Startup: slurp the sidecar rfs via WASI, mount the default overlay, read the
//! boot record (personality) and the exec map (path -> program), select the
//! entry program the boot argv names, build its guest stack, and jump in.

use crate::abi::{self, State};
use crate::arena::Arena;
use crate::boot::{Boot, BOOT_PATH};
use crate::context::{setup_state, EcvContext, SchedOutcome, EXIT_DEADLOCK};
use crate::execmap::{Programs, EXEC_PATH};
use crate::trace::{ecv_debug, ecv_log, ecv_trace, ecv_warn};
use crate::vfs::Vfs;
use std::ffi::CStr;
use std::os::raw::{c_char, c_int};

/// Env var naming the sidecar path (set by `raptormark run`); falls back to
/// conventional names relative to a preopened dir for plain `wasmtime --dir`.
const ROOTFS_ENV: &str = "RAPTORMARK_ROOTFS";
const ROOTFS_DEFAULTS: &[&str] = &["out.rootfs.img", "rootfs.img", "/out.rootfs.img"];

#[no_mangle]
unsafe fn boot_world(argc: c_int, argv: *mut *mut c_char) -> i32 {
    // Read the diagnostic env vars ONCE here, before any guest code or fork runs, so
    // no syscall handler ever does an env read (std env's LazyLock, poisoned across
    // the fork's asyncify replay, would infinite-loop — see sys::init_diag_flags).
    crate::sys::init_diag_flags();
    // ⚠️ `ecv_boot` HAS NO crt1 TO HAND IT argv, so it passes (0, null) and the
    // arguments must be fetched here instead. Without this the re-entrant path
    // silently dropped every guest argument: the guest fell back to ecvisor's
    // "app" placeholder and read its own `argv[1]` as absent. It surfaced as a
    // guest connecting to a DEFAULT port while the host had passed a real one --
    // a wrong destination, not an error, which is the expensive kind.
    //
    // `std::env::args_os` is the same `args_get` wasi-libc's `_start` uses, so
    // the two entry points now see identical arguments.
    let mut host_args: Vec<Vec<u8>> = Vec::with_capacity(argc.max(0) as usize);
    if argc > 0 && !argv.is_null() {
        for i in 0..argc as usize {
            let p = *argv.add(i);
            if p.is_null() {
                break;
            }
            host_args.push(CStr::from_ptr(p).to_bytes().to_vec());
        }
    } else {
        for a in std::env::args_os() {
            host_args.push(a.into_encoded_bytes());
        }
    }

    let sidecar = load_sidecar();
    let vfs = Vfs::new(sidecar);
    let boot = vfs.read(b"/", BOOT_PATH).and_then(|b| Boot::parse(&b));

    let (argv_g, envp_g, cwd, uid, gid) = match boot {
        Some(b) => (b.argv, b.env, b.cwd, b.uid, b.gid),
        None => {
            let mut a = host_args.clone();
            if a.is_empty() {
                a.push(b"app".to_vec());
            }
            (a, Vec::new(), b"/".to_vec(), 0, 0)
        }
    };

    // The two `std::env::var` reads that used to gate this are gone: both flags are
    // already in `diag`'s atomics by now, and `init_diag_flags` exists precisely so
    // that no code after it touches the environment.
    //
    // The gate is the UNION of the two, which is what it always was. `show` is
    // defined unconditionally but only ever CALLED inside the macro's `if`, since
    // its result is an argument to `format_args!`.
    let verbose = crate::diag::debug_log() || crate::diag::trace_log();
    let show = |v: &[Vec<u8>]| {
        v.iter()
            .map(|e| String::from_utf8_lossy(e).into_owned())
            .collect::<Vec<_>>()
    };
    ecv_log!(verbose, ecvisor, "boot argv: {:?}", show(&argv_g));
    ecv_log!(verbose, ecvisor, "boot env: {:?}", show(&envp_g));
    ecv_log!(
        verbose,
        ecvisor,
        "boot cwd: {:?}",
        String::from_utf8_lossy(&cwd)
    );

    let programs = Programs::load(abi::registry(), vfs.read(b"/", EXEC_PATH).as_deref());
    // Select the entry program the boot argv names, following `#!` lines the way
    // `execve` does; single-program modules (empty exec map) fall back to
    // program 0.
    //
    // ❗ WITHOUT THE `#!` WALK THIS RAN THE WRONG PROGRAM, SILENTLY. A real
    // image's ENTRYPOINT is a script -- `raptormark build postgres:17` writes
    // `["docker-entrypoint.sh", "postgres"]` -- and a script is not a registry
    // program, so `resolve` returned None and `unwrap_or(0)` ran program 0.
    // Measured 2026-08-27: postgres:17 printed apt's `E: Invalid operation
    // postgres`, because apt is program 0. That is the fallback `execmap.rs`
    // records four incidents for, reached from the one place that cannot report
    // an errno instead.
    //
    // ⚠️ The fallback to 0 is KEPT, because a single-program module has an empty
    // exec map and must still boot. What changed is that a script no longer
    // reaches it.
    let mut argv_g = argv_g;
    let mut sb_depth = 0usize;
    let entry_idx = loop {
        let Some(a0) = argv_g.first().cloned() else {
            break 0;
        };
        if let Some(i) = programs.resolve(&vfs, &cwd, &a0) {
            break i;
        }
        let Some(sb) = vfs
            .read(&cwd, &a0)
            .as_deref()
            .and_then(crate::shebang::parse)
        else {
            break 0;
        };
        sb_depth += 1;
        if sb_depth > crate::shebang::MAX_DEPTH {
            crate::fatal!(
                "boot argv follows more than {} #! levels",
                crate::shebang::MAX_DEPTH
            );
        }
        argv_g = crate::shebang::rewrite_argv(&sb, &a0, &argv_g);
        ecv_log!(verbose, ecvisor, "boot argv after #!: {:?}", show(&argv_g));
    };
    if entry_idx >= programs.len() {
        crate::fatal!("no entry program (registry is empty)");
    }

    let prog = programs.get(entry_idx);
    let state = State::new_boxed();
    let mut arena = Arena::new();
    arena.load_data_sections(prog);
    let sp = arena.build_stack(prog, &argv_g, &envp_g, uid, gid);
    // ⚠️ LEAKED, NOT STACK-LOCAL, and that is the whole basis of re-entrancy.
    //
    // These two used to be `let mut state` / `let mut ctx` right here, with
    // their addresses handed to lifted code as raw pointers. That is sound only
    // as long as this function never returns -- and a host that has to be given
    // control back needs it to return and then be re-entered, at which point
    // every one of those pointers would dangle.
    //
    // Leaking is not a lifetime workaround: there is exactly one of each per
    // instance, they live until the module dies, and nothing ever drops them.
    // `Box::leak` states that, and it costs nothing at run time.
    let state: &'static mut State = Box::leak(state);
    setup_state(state, prog, sp);

    let ctx: &'static mut EcvContext = Box::leak(Box::new(EcvContext::new(
        arena, vfs, cwd, uid, gid, programs, entry_idx,
    )));
    // Shared-libc (.agents/docs/PERF.md Lever B): merge the LIBRARY units — the ONE
    // lifted libc superset — into the entry binary's dispatch tables + arena, so its
    // calls into libc resolve in-process. Driven by the exec map (a registry program
    // with no exec-map path is a library); re-merged after every execve
    // (context.rs exec_into). No-op for the full-fuse execve model and single-program
    // modules (no library units), so those paths are unaffected. Runs BEFORE the
    // bring-up below, which needs libc's __libc_early_init/ctors resolvable.
    // A registration arriving after this point cannot go into `DYNAMIC` -- the
    // list has already been copied into `ctx.programs` -- so route it into the
    // live context instead of refusing it. That refusal was correct when nothing
    // could be placed mid-run; a host-driven loader places units exactly then.
    unsafe {
        crate::abi::set_late_register_hook(|p, size| {
            if size as usize != core::mem::size_of::<crate::abi::EcvProgram>() {
                return crate::abi::ECV_REG_ABI;
            }
            match world() {
                Some(w) => (*w.ctx).register_late_unit(p),
                None => crate::abi::ECV_REG_FROZEN,
            }
        });
    }

    ctx.merge_libraries();
    // Legacy PoC: with NO exec map, RAPTORMARK_SHARED_UNITS merges everything-but-
    // entry (the single-binary shared-libc spike, tests/shared_libc_test.go).
    if ctx.library_units.is_empty() && std::env::var("RAPTORMARK_SHARED_UNITS").is_ok() {
        ctx.merge_shared_units(entry_idx);
    }
    // The arena's base pointer is NOT constant for the run: a context switch
    // swaps the live buffer with the incoming process's, so the address changes
    // whenever a different process is loaded. It is therefore re-read at the top
    // of every scheduler leg below; this one is for the load-time setup calls,
    // which all run before the first switch.
    let arena_ptr = ctx.arena.base_ptr();
    // `state` is the single live State (fixed address); the scheduler swaps its
    // contents per process. Guest register/stack state lives here + in the arena,
    // never in wasm locals, so a process's native frames are disposable: a blocking
    // syscall discards them (EH-unwind) and the scheduler rebuilds them by replay.
    ctx.live_state = state as *mut State;
    let state_ptr = state as *mut State;
    let ctx_ptr = ctx as *mut EcvContext;

    // Load-time runtime setup, before the guest's first instruction:
    //   1. static TLS — copy EVERY PT_TLS module's `.tdata` template to its
    //      TP-relative block (per the prelinker's `.ecv.tls` descriptor table)
    //      so `__thread` reads see their initial values;
    //   2. ifunc GOT slots — run each `.ecv.irela` resolver as a guest function
    //      and fill its GOT slot with the resolved implementation.
    // setup_state already put THREAD_PTR in tpidr_el0; the TLS blocks sit above.
    // Publish the call-history buffer BEFORE the first call into lifted code.
    //
    // Everything below this line runs guest functions -- ifunc resolvers,
    // __libc_early_init, the constructors -- and they run BEFORE the scheduler
    // loop, which is where the per-leg publish lives. A module built with the
    // inlined call history reaches its slow arm during all of that (capacity is
    // still zero, so `len >= cap` holds), and the slow arm ADOPTS: it set the
    // vector's length from a global nothing had published, truncating away the
    // frames bring-up had already pushed.
    //
    // Caught by the `ch_published` guard rather than by the wreckage:
    // `call history moved without republish: published 0x0, now 0x19aad5b0
    // (len 1 cap 16384)` in TestSharedNamesReuseAcrossAClosure. Without that
    // guard this was a silent truncation during startup.
    ctx.publish_call_history();
    ctx.setup_tls(crate::arena::THREAD_PTR);
    ctx.apply_ifuncs(arena_ptr, state_ptr);
    //   3. glibc bring-up — call __libc_early_init (the ld.so hook our trampoline
    //      entry skips) so the main thread's ctype/locale tsd is initialized before
    //      the guest runs (dash's is_name()/isalpha() need it). No-op when the
    //      closure has no glibc (.ecv.early absent).
    ctx.apply_early_init(arena_ptr, state_ptr);
    //   3b. glibc _rtld_global stack lists — the ld.so pthread bring-up we skip
    //      leaves _dl_stack_used/_user/_cache zeroed; write their heads (the
    //      prelinker derived the offsets from thread_db metadata) so __libc_fork's
    //      child cleanup doesn't walk a null list. No-op without .ecv.stacklists.
    ctx.apply_stacklists(arena_ptr, state_ptr);
    // musl's equivalent of the pthread->tid seed above (see apply_musl_tp).
    ctx.apply_musl_tp(arena_ptr, state_ptr);
    // glibc's counterpart: the default thread stack size ld.so would have taken
    // from RLIMIT_STACK. Must follow apply_early_init -- it calls into libc.
    ctx.apply_pthread_attr_default(arena_ptr, state_ptr);
    //   4. constructors — the _dl_init equivalent: run the program's DT_INIT /
    //      DT_INIT_ARRAY (and DT_PREINIT_ARRAY) in loader order (.ecv.init). Runs
    //      after early_init, before the entry. No-op when there are no constructors.
    ctx.apply_init_array(arena_ptr, state_ptr);

    ecv_debug!(
        mem,
        "boot complete -> linear memory {} MiB",
        crate::diag::linear_memory_mib()
    );
    // Cooperative scheduler (full-replay; no asyncify, no EH). Each iteration runs
    // ONE leg of the current process by calling straight into lifted code: a
    // fresh/execve'd process enters at its program entry; a suspended process (fork
    // child, or one blocked in a syscall) re-enters its current replay frame.
    //
    // A blocking syscall does not unwind. It raises `ctx.unwinding`, and the
    // fork_emulation codegen returns out of every frame between the syscall and
    // here (elfconv patch 0026); the trampoline has already recorded the replay
    // state, so schedule_after_suspend just picks the next runnable process. A leg
    // that returns with `unwinding` CLEAR ran to completion: during reconstruction
    // that retires the frame and advances to the next-outer one, rebuilding the
    // stack. The module exits when schedule_after_suspend finds nothing runnable.
    publish_world(state_ptr, ctx_ptr);
    0
}

/// The WASI command entry: boot, then run to completion.
///
/// ⚠️ This is still a plain `_start` command module and nothing about it
/// changed observably. It drives the SAME slices a re-entrant host would, with
/// no leg budget, and simply never sees `Idle` -- a blocking backend's `wait`
/// does not return until something is ready. One driver for both profiles is
/// what stops them drifting apart.
#[no_mangle]
pub unsafe extern "C" fn __main_argc_argv(argc: c_int, argv: *mut *mut c_char) -> c_int {
    if boot_world(argc, argv) != 0 {
        return 1;
    }
    loop {
        match run_slice(u32::MAX) {
            LegOutcome::Continue | LegOutcome::Preempted => continue,
            LegOutcome::Idle { .. } => {
                // Unreachable with a blocking backend. If a build ever reaches
                // it, the backend declined to wait while this profile has no
                // event loop to hand control to -- so say that, rather than
                // spinning or exiting 0 as if the guest had finished.
                crate::fatal!(
                    "scheduler went idle under a blocking backend: nothing can resume it"
                );
            }
            LegOutcome::Exit(code) => std::process::exit(code),
        }
    }
}

/// The booted instance. See `publish_world`.
struct World {
    state: *mut State,
    ctx: *mut EcvContext,
    /// Soonest guest deadline as of the last `Idle`, in absolute nanos.
    wake_at: Option<u128>,
    /// The previous slice returned `Idle`, so nothing is loaded and the next
    /// one must SELECT before it runs a leg.
    needs_select: bool,
    exit_code: i32,
    exited: bool,
}

/// The one booted instance, or null before `ecv_boot`.
///
/// ⚠️ `static mut` accessed only through `addr_of_mut!`, which is the idiom this
/// crate already uses for the lifted-code globals (`abi.rs`). The runtime is
/// single-threaded by construction -- a cooperative scheduler that only switches
/// at a block, a yield or an exit -- so there is no other accessor to race with.
static mut WORLD: *mut World = core::ptr::null_mut();

fn publish_world(state: *mut State, ctx: *mut EcvContext) {
    unsafe {
        *core::ptr::addr_of_mut!(WORLD) = Box::into_raw(Box::new(World {
            state,
            ctx,
            wake_at: None,
            needs_select: false,
            exit_code: 0,
            exited: false,
        }));
    }
}

fn world() -> Option<&'static mut World> {
    unsafe { core::ptr::addr_of_mut!(WORLD).read().as_mut() }
}

/// Runs scheduler legs until the guest cannot proceed without the host.
///
/// This is the loop that used to be inline in `__main_argc_argv` and never
/// returned. Nothing about the replay machinery changed: a leg is still entered
/// by calling straight into lifted code, a blocking syscall still returns out of
/// every frame with `__ecv_unwinding` raised, and the call-history walk is
/// untouched. What changed is only that reaching the end of the work is now a
/// RETURN rather than a `std::process::exit`.
fn run_slice(max_legs: u32) -> LegOutcome {
    let Some(w) = world() else {
        crate::fatal!("ecv_run_slice before ecv_boot");
    };
    let state_ptr = w.state;
    let ctx_ptr = w.ctx;
    let ctx: &mut EcvContext = unsafe { &mut *ctx_ptr };
    // ⚠️ RE-ENTRY AFTER `Idle` HAS NOTHING LOADED. The previous slice returned
    // from the scheduler without selecting a process, so `ctx.current` still
    // names the one that blocked. Running a leg for it would re-enter a process
    // that is still Blocked -- observed as a guest that printed everything
    // before its `nanosleep`, went idle once, and then "exited 0" without ever
    // reaching the line after the sleep.
    if w.needs_select {
        w.needs_select = false;
        let outcome = ctx.resume_scheduling();
        match act_on(ctx, outcome) {
            LegOutcome::Continue => {}
            other => return other,
        }
    }
    let mut legs: u32 = 0;
    loop {
        // ⚠️ BETWEEN legs, never inside one. A leg runs lifted code to a
        // suspension point, and a syscall boundary is the only place guest state
        // is wholly in `State` + the arena. There is no safe interrupt inside a
        // leg, so a guest that computes forever with no syscalls still holds the
        // thread -- that limit is real and unchanged.
        if legs >= max_legs {
            return LegOutcome::Preempted;
        }
        legs += 1;
        let cur = ctx.current;
        // Re-read after every switch: `load_current` swaps the live buffer, so a
        // pointer captured before it belongs to some other process's memory now.
        // Guest pointers are unaffected -- under the identity map a guest pointer
        // is a VMA, not a host address -- so this is the only holder that matters.
        let arena_ptr = ctx.arena.base_ptr();
        // Choose the call target and whether this is a syscall-resume. A process
        // mid-reconstruction re-enters `replay.cur` (a lifted function at a mid-body
        // pc via the block-address map); a fresh/execve'd process runs the program's
        // entry dispatcher. Both share the LiftedFunc signature.
        let (call_func, call_pc, replaying, resuming) = match ctx.procs[cur]
            .replay
            .as_ref()
            .map(|rp| (rp.cur, rp.resuming))
        {
            Some(((fvma, pc), res)) => {
                let f = ctx.func_containing(fvma).unwrap_or_else(|| {
                    crate::fatal!("replay: no lifted function contains 0x{fvma:x}")
                });
                (f, pc, true, res)
            }
            None => {
                let cur_prog = ctx.programs.get(ctx.procs[cur].prog_idx);
                (cur_prog.entry_func(), cur_prog.entry_pc(), false, false)
            }
        };
        ctx.procs[cur].started = true;
        // Resuming a blocked syscall: COMPLETE it here before re-entering the frame.
        // Re-entering the innermost frame at its post-SVC pc does NOT re-execute the
        // SVC, so the handler's resume path (reap / set return value / wake) must be
        // driven directly — mirroring the old asyncify rewind, which ran the handler
        // as part of the rewound SVC. It may re-block (Shape A: still no data), in
        // which case we re-capture the (unchanged) replay point and yield.
        if resuming {
            ctx.resuming = true;
            unsafe { crate::sys::svc(arena_ptr, &mut *state_ptr, &mut *ctx_ptr) };
            if ctx.suspended {
                ctx.suspended = false;
                ctx.on_suspend();
                // `pid=` is no longer formatted by hand: the subscriber appends
                // the pid to every DEBUG/TRACE line from `trace::CURRENT_PID`,
                // which `load_current` publishes.
                ecv_trace!(sched, "resume re-blocked cur_idx={}", cur);
                let outcome = ctx.schedule_after_suspend();
                match act_on(ctx, outcome) {
                    // `act_on` never yields Preempted -- only the budget above
                    // does -- so this is unreachable rather than merged.
                    LegOutcome::Preempted => unreachable!("act_on cannot preempt"),
                    LegOutcome::Continue => continue,
                    LegOutcome::Idle { .. } => break,
                    LegOutcome::Exit(code) => std::process::exit(code),
                }
            }
        }
        // The frame is entered at a post-SVC / post-call pc — not a syscall — so a
        // syscall it makes next is fresh.
        ctx.resuming = false;
        ecv_trace!(
            sched,
            "enter cur_idx={} {}{} pc={:#x}",
            cur,
            if replaying { "REPLAY" } else { "FRESH" },
            if resuming { "/resumed-svc" } else { "" },
            call_pc
        );
        // Hand the lifted code the call-history buffer before entering it. This
        // is the ONE site that covers every context switch: `load_current` puts
        // a different vector in place, so a base published before the switch
        // points at the wrong allocation. No-op unless the inline path is on.
        ctx.publish_call_history();
        unsafe { call_func(arena_ptr, state_ptr, call_pc, ctx_ptr) };
        // The leg is back, by ordinary return either way: it ran to completion, or
        // a syscall suspended it and every frame on the way out returned early off
        // `_ecv_suspended`. `unwinding` is what tells the two apart.
        let suspended = crate::context::unwinding();
        crate::context::set_unwinding(false);
        ecv_trace!(
            sched,
            "return cur_idx={} suspended={} replaying={}",
            cur,
            suspended,
            replaying
        );
        // A NON-LOCAL JUMP unwound back here. `__remill_jump` already recorded
        // the replay (the target frame, and the call history truncated to it),
        // so the only thing left is to re-enter this process there -- WITHOUT
        // going through the scheduler, because a `longjmp` is not a yield point.
        // Checked before the `suspended` arm: both ride the same `unwinding`
        // flag, and only this one has a replay that is already correct.
        if suspended && ctx.longjmp_pending {
            ctx.longjmp_pending = false;
            ecv_trace!(sched, "longjmp cur_idx={} re-entering", cur);
            continue;
        }
        if suspended {
            // A blocking syscall EH-unwound back here; the trampoline captured the
            // replay state (or, for execve, left replay=None + Pending::Yield).
            let outcome = ctx.schedule_after_suspend();
            match act_on(ctx, outcome) {
                LegOutcome::Preempted => unreachable!("act_on cannot preempt"),
                LegOutcome::Continue => continue,
                // Only a non-blocking backend reaches this. The command profile
                // never does, because its backend's `wait` does not return until
                // something is ready.
                LegOutcome::Idle { .. } => break,
                LegOutcome::Exit(code) => std::process::exit(code),
            }
        }
        // The leg RETURNED without yielding.
        if replaying {
            // A reconstructed frame ran to completion. elflift emits the call-history
            // pop (_ecv_func_epilogue) in the CALLER, right AFTER the call — but we
            // re-entered this frame at the return address, PAST that pop, so it was
            // skipped and call_history still holds the entry for the call we just
            // finished. Pop it in lockstep with `remaining` so call_history stays
            // exact for any nested fork/yield (otherwise it leaks one entry per
            // reconstructed call and a returning frame loops on the leaked entries —
            // e.g. glibc __libc_fork, or an unrolled loop of calls). See
            // TraceLifter.cpp GenForkNearJump / kCategoryDirectFunctionCall and
            // .agents/docs/DYNLINK.md.
            //
            // ADOPT FIRST. With the inlined call history (elfconv patch 0060)
            // the leg just run pushed frames straight into the global without
            // Rust seeing them, so the vector's own length is stale by exactly
            // those frames. Popping the stale vector discarded every one of
            // them, and the next `publish_call_history` then handed the guest a
            // truncated history -- so replay rebuilt the wrong stack and the
            // process resumed into the wrong frame. It surfaced as the GUEST's
            // stack canary firing (`*** stack smashing detected ***`) in
            // `TestNginxSyscallsUnderEcvisor` and `TestEpollTimeoutUnderEcvisor`,
            // nowhere near here. No-op when the fast path is off.
            ctx.adopt_call_history_depth();
            ctx.call_history.pop();
            // Advance to the next-outer frame (resuming at its post-call pc, not a
            // syscall), or finish reconstruction.
            let advanced = match ctx.procs[cur].replay.as_mut() {
                Some(rp) => match rp.remaining.pop() {
                    Some(f) => {
                        rp.cur = f;
                        rp.resuming = false;
                        true
                    }
                    None => false,
                },
                None => false,
            };
            if advanced {
                let pc = ctx.procs[cur].replay.as_ref().unwrap().cur.1;
                unsafe { (*state_ptr).gpr.pc.val = pc };
                continue; // re-enter the next-outer frame (same process)
            }
            // Below the outermost recorded frame — reconstruction complete (the real
            // _start exits via a syscall rather than returning here).
            ctx.procs[cur].replay = None;
            break;
        }
        break; // fresh program returned without exiting; treat as done
    }
    // The leg loop fell out: a fresh program returned without exiting, or
    // reconstruction finished. Neither is a syscall exit, so ask the scheduler
    // what is left rather than assuming the run is over.
    let outcome = ctx.resume_scheduling();
    act_on(ctx, outcome)
}

/// What one slice of the scheduler decided, in the form a HOST cares about.
///
/// The command profile collapses this back into a `proc_exit`; a re-entrant
/// host uses `Idle` to hand control to its event loop and come back later.
pub(crate) enum LegOutcome {
    /// Keep running legs.
    Continue,
    /// The leg budget ran out. The guest is fine and mid-flight; re-enter as
    /// soon as convenient.
    Preempted,
    /// Nothing runnable and the backend declined to block.
    Idle { wake_at: Option<u128> },
    /// The run is over, with this status.
    Exit(i32),
}

/// Turns a scheduler decision into a leg-loop action.
///
/// ⚠️ The EXIT lives here rather than in the scheduler, and that is the point of
/// the split. `context.rs` used to call `std::process::exit` from inside its
/// selection loop, which is correct for a command module and impossible for a
/// host that has to be handed control back. Now the scheduler reports and the
/// entry decides.
pub(crate) fn act_on(ctx: &mut EcvContext, outcome: SchedOutcome) -> LegOutcome {
    match outcome {
        SchedOutcome::Ready => LegOutcome::Continue,
        SchedOutcome::Idle { wake_at, .. } => LegOutcome::Idle { wake_at },
        SchedOutcome::Deadlock => {
            ctx.report_deadlock();
            LegOutcome::Exit(EXIT_DEADLOCK)
        }
        SchedOutcome::Exited(code) => LegOutcome::Exit(code),
    }
}

// # The re-entrant host interface
//
// A browser cannot let the guest block: `Atomics.wait` is illegal on the main
// thread, and even in a worker a blocking host stalls the event loop that
// delivers the very readiness the guest is waiting for. So instead of the host
// waiting for ecvisor, ecvisor returns to the host.
//
// That is possible here and nowhere obvious because of one property: **no
// native frames survive a suspension**. At the top of the leg loop the wasm
// shadow stack is fully unwound and all guest state lives in `State`, the arena
// and `EcvContext`. Returning to the host from there is indistinguishable, to
// the guest, from `continue`.
//
// ⚠️ Host imports must NEVER call back into these exports. `ecv_run_slice` is
// on the stack during every import call, so re-entering it is reentrancy into a
// `&mut`-borrowed context. It is safe by construction -- a JS callback cannot
// fire during a synchronous wasm call -- but that is the REASON, not an
// assumption to rely on silently.

/// `ecv_run_slice` status codes. Kept as plain integers because they cross a
/// C ABI to a host that is not Rust.
pub const ECV_IDLE: i32 = 0;
pub const ECV_PREEMPTED: i32 = 1;
pub const ECV_EXITED: i32 = 2;

/// Boots the instance. Returns 0 on success.
///
/// Runs exactly what the command path runs -- sidecar, VFS, boot record,
/// programs, arena and stack, TLS, ifuncs, early init, stacklists, the musl
/// thread pointer, the default pthread attr, and the constructors -- and then
/// stops, with the first guest instruction not yet executed.
///
/// Calling it twice is refused rather than tolerated: a second boot would leak
/// the first instance and leave lifted code holding pointers into it.
#[no_mangle]
pub unsafe extern "C" fn ecv_boot() -> i32 {
    if world().is_some() {
        crate::fatal!("ecv_boot called twice");
    }
    boot_world(0, core::ptr::null_mut())
}

/// Runs up to `max_legs` scheduler legs and reports why it stopped.
///
/// Returns `ECV_IDLE`, `ECV_PREEMPTED` or `ECV_EXITED`. On `ECV_IDLE` the host
/// should consult `ecv_next_deadline_in_ms`; on `ECV_EXITED`, `ecv_exit_code`.
/// A deadlock is reported as `ECV_EXITED` carrying 111, which is the same status
/// the command profile exits with -- one fewer code for a host to special-case,
/// and the diagnostic has already been printed.
#[no_mangle]
pub unsafe extern "C" fn ecv_run_slice(max_legs: u32) -> i32 {
    let budget = if max_legs == 0 { u32::MAX } else { max_legs };
    match run_slice(budget) {
        LegOutcome::Continue | LegOutcome::Preempted => ECV_PREEMPTED,
        LegOutcome::Idle { wake_at } => {
            if let Some(w) = world() {
                w.wake_at = wake_at;
                w.needs_select = true;
            }
            ECV_IDLE
        }
        LegOutcome::Exit(code) => {
            if let Some(w) = world() {
                w.exit_code = code;
                w.exited = true;
            }
            ECV_EXITED
        }
    }
}

/// How long until the soonest guest deadline, in milliseconds, or -1 for none.
///
/// `f64` because that is what `setTimeout` takes; the value is a DURATION, so it
/// can be passed to a timer without arithmetic.
///
/// ⚠️ RENAMED FROM `ecv_next_deadline_ms`, WHICH RETURNED AN ABSOLUTE UNIX
/// INSTANT. Deadlines are now monotonic, counted from ecvisor's boot, and that
/// number means nothing next to `Date.now()` -- a host that kept subtracting it
/// would compute a delay of roughly minus the age of the epoch, clamp to zero
/// and spin. Keeping the old name would have made that a silent behaviour
/// change; the rename makes a stale host fail at instantiation, naming the
/// missing export.
///
/// Measured at CALL time rather than reported from the slice, so any host-side
/// work between `ecv_run_slice` returning and this being asked comes out of the
/// delay instead of being added to it. Truncating division rounds the wait DOWN,
/// which is the safe direction: an early wake costs one scheduler pass that
/// re-parks, a late one overruns the guest's timer.
#[no_mangle]
pub unsafe extern "C" fn ecv_next_deadline_in_ms() -> f64 {
    match world().and_then(|w| w.wake_at) {
        Some(ns) => (ns.saturating_sub(crate::context::mono_nanos()) / 1_000_000) as f64,
        None => -1.0,
    }
}

/// The host reports that a socket handle made progress.
///
/// `events` is a bitmask: 1 = readable, 2 = writable. Bits are OR'd into a
/// runtime-side cache, so two notifications between slices cannot lose one, and
/// `NetBackend::ready` then answers from that cache with no import call at all.
///
/// ⚠️ OVER-NOTIFYING IS SAFE; under-notifying hangs. A woken process re-checks
/// its condition and re-parks, so a spurious call costs one scheduler pass. A
/// missing one costs the run: the guest waits for readiness that already
/// happened and nothing will mention again. When in doubt, notify.
///
/// ⚠️ MUST NOT be called from inside a host import. `ecv_run_slice` is on the
/// stack there, and re-entering the runtime is reentrancy into a `&mut`-borrowed
/// context. Queue the event and call this between slices.
#[cfg(all(target_arch = "wasm32", feature = "net-browser"))]
#[no_mangle]
pub unsafe extern "C" fn ecv_net_ready(handle: u32, events: u32) {
    if let Some(w) = world() {
        (*w.ctx)
            .net
            .note_ready(crate::net::NetHandle(handle as i32), events);
    }
}

/// The host reports that unit `idx` has been placed and registered (`ok != 0`),
/// or could not be (`ok == 0`).
///
/// The counterpart of `ecv_net_ready`, and it exists for the same reason: a
/// process parked on a host-driven event cannot be woken from inside the guest.
///
/// ⚠️ MUST NOT BE CALLED WHILE `ecv_run_slice` IS ON THE STACK, exactly as
/// `ecv_signal` must not: it mutates the process table, which a running slice
/// holds `&mut`. The host queues these and flushes them between slices.
///
/// ⚠️ The host must have completed the WHOLE §8 sequence before calling this --
/// reserve, place, relocate, run ctors, and `ecv_register_program` -- because
/// the woken guest resumes straight into code that assumes the unit is
/// registered. Calling it early is not a race that resolves itself; it is a
/// dispatch into a program the registry does not have.
///
/// Returns 1 if a process was actually waiting, so a host can tell a delivered
/// load from one nobody asked for.
#[no_mangle]
pub unsafe extern "C" fn ecv_side_loaded(idx: u32, ok: u32) -> i32 {
    match world() {
        Some(w) => i32::from((*w.ctx).note_side_loaded(idx as usize, ok != 0)),
        None => 0,
    }
}

/// Posts a signal to a guest process, as `kill(2)` would from outside.
///
/// ⚠️ A GUEST HAS NO OUTSIDE. Under containerd something can `kill` it; in a tab
/// nothing can, so a supervisor like nginx's master -- whose whole job is
/// reacting to signals -- could only ever be observed doing its startup work.
/// This is the missing sender.
///
/// ⚠️ MUST NOT BE CALLED WHILE `ecv_run_slice` IS ON THE STACK. It mutates the
/// process table, which a slice is holding `&mut`. The host queues these and
/// flushes them between slices, exactly as it does readiness.
///
/// Returns 1 if the signal was posted, 0 if no such process exists.
#[no_mangle]
pub unsafe extern "C" fn ecv_signal(pid: u32, sig: u32) -> i32 {
    match world() {
        Some(w) => i32::from((*w.ctx).post_signal(pid, sig)),
        None => 0,
    }
}

/// The guest's exit status. Meaningful only after `ECV_EXITED`.
#[no_mangle]
pub unsafe extern "C" fn ecv_exit_code() -> i32 {
    world().map(|w| w.exit_code).unwrap_or(0)
}

/// Reads the sidecar image bytes via WASI, or None if not found.
fn load_sidecar() -> Option<Vec<u8>> {
    if let Ok(path) = std::env::var(ROOTFS_ENV) {
        if let Ok(bytes) = std::fs::read(&path) {
            return Some(bytes);
        }
        ecv_warn!(ecvisor, "warning: {ROOTFS_ENV}={path} set but unreadable");
    }
    for cand in ROOTFS_DEFAULTS {
        if let Ok(bytes) = std::fs::read(cand) {
            return Some(bytes);
        }
    }
    None
}
