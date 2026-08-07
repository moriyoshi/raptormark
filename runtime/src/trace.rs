//! ecvisor's diagnostic logging: one line per event, `[category]`-prefixed,
//! attributed to a pid, filtered by the [`crate::diag`] flags.
//!
//! WHY NOT THE `tracing` CRATE. This module was built on the `tracing` facade
//! first, and the accounting afterwards did not support keeping it. What `tracing`
//! contributed to the working system was a `max_level_hint` fast path (one relaxed
//! atomic load, which is three lines here) and the `Visit` field abstraction. What
//! it charged for that was four crates and ~390 KB of unlinked archive, ~228 KB of
//! it `tracing-core` -- a callsite registry, a dynamic `Dispatch` indirection, and
//! span machinery, none of which a runtime with exactly ONE compile-time subscriber
//! can use.
//!
//! The features that justify the dependency went unused. Across the ~90 converted
//! call sites, not one uses structured `k = v` fields; they are all format strings.
//! Not one creates a span -- the pid a span would have carried is the atomic below,
//! because ecvisor's processes are cooperative contexts switched at arbitrary
//! syscall boundaries and there is no lexical scope for an RAII guard to live in.
//! Span support existed only so that `info_span!` would not be a silent no-op.
//!
//! And it cost correctness twice. A `tracing` callsite carries exactly ONE level,
//! so the two sites gated on a UNION of flags -- `sys.rs`'s `fd_write` errno report
//! (`debug_log() || trace_log()`) and `context.rs`'s `[bbmiss]` report
//! (`legsp() || debug_log()`) -- could not be expressed. Fitting them required
//! making TRACE subsume DEBUG and inventing a named gate for one call site: two
//! semantic changes to serve the logging library rather than the runtime. Here a
//! gate is an ordinary `bool` expression, so both sites read exactly as they always
//! did and neither compromise exists.
//!
//! Also gone with it: a global dispatcher behind `once_cell` and a
//! `thread_local!`/`RefCell`, in a runtime whose `diag.rs` exists specifically
//! because lazy statics deadlock across this fork emulation. That risk was argued
//! safe but never verified, and it was being taken on for machinery nothing used.
//!
//! WHAT IT REPLACES. Diagnostics here were 21 hand-written `eprintln!` prefixes
//! (`[ecv]`, `[sched]`, `[ftrace]`, `[futex]`, ...) each wrapped in an
//! `if crate::diag::<gate>()`, with the pid formatted by hand at every site that
//! wanted one. The prefix was already a category and the `if` was already a filter;
//! they simply were not stated once. Now the macro names the gate, the category
//! renders as the prefix, and the pid comes from [`CURRENT_PID`].
//!
//! [`crate::diag`] IS THE SOURCE OF TRUTH, unchanged. Flags are read from the
//! environment exactly once, into atomics, before any guest code or fork runs --
//! see the fork-safety note atop `diag.rs`. Nothing here reads the environment, and
//! several gates (`snapcheck`, `fdcheck`) guard O(arena) computation rather than a
//! print, so their boolean form has to keep existing regardless.
//!
//! THE HOT PATH IS UNCHANGED, AND `hot_slow()` MUST STAY THE OUTER GATE.
//! `_ecv_save_call_history` and `_ecv_func_epilogue` are called from 32,972 and
//! 32,115 sites in a linked bash module, and `diag::hot_slow()` is what lets each
//! entry test ONE atomic and jump over all five per-call diagnostics. A per-gate
//! check ahead of that guard would run on every guest BL, so it must not go there.
//! Inside the guard it is free -- those bodies only reach a print on a rare edge --
//! so the prints inside the hooks are converted and the guard around them is not.

use core::fmt;
use core::sync::atomic::{AtomicU32, Ordering};

// ---------------------------------------------------------------------------
// Current pid, published rather than passed
// ---------------------------------------------------------------------------

/// The pid a diagnostic line is attributed to.
///
/// Every syscall handler threads a `&mut EcvContext` around, but the log macros
/// expand at sites that do not all have one in scope, and threading it purely to
/// print a pid is what produced the hand-formatted `pid={}` at 15 separate sites.
/// Publishing to an atomic at the one place the current process changes is what the
/// rest of this crate already does with diagnostic state.
///
/// Seeded to 1 because that is not a guess: `EcvContext::new` builds `procs` with a
/// literal `pid: 1` for the initial process, so 1 is correct for every line emitted
/// before the first switch.
static CURRENT_PID: AtomicU32 = AtomicU32::new(1);

/// Publishes the pid subsequent diagnostics are attributed to. Called from
/// `EcvContext::load_current`, the single place `self.current` is assigned -- which
/// is what makes one call site sufficient, and what makes this value provably equal
/// to `EcvContext::current_pid()` at every point either can be read.
#[inline]
pub(crate) fn set_current_pid(pid: u32) {
    CURRENT_PID.store(pid, Ordering::Relaxed);
}

/// The pid diagnostics are currently attributed to.
#[inline]
pub(crate) fn current_pid() -> u32 {
    CURRENT_PID.load(Ordering::Relaxed)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

/// Renders one diagnostic line.
///
/// Pure, and the pid is a parameter rather than a read of [`CURRENT_PID`], so every
/// formatting rule is testable without touching process-wide state. `emit_gated`
/// and `emit_plain` are the only things that supply it.
pub(crate) fn line(cat: &str, pid: Option<u32>, args: fmt::Arguments<'_>) -> String {
    use fmt::Write as _;
    let mut s = String::with_capacity(96);
    let _ = write!(s, "[{cat}]");
    if let Some(p) = pid {
        let _ = write!(s, " pid={p}");
    }
    let _ = write!(s, " {args}");
    s
}

/// A gated diagnostic: carries the pid.
///
/// Gated lines are the firehose, where "which process did this" is the question.
#[inline]
pub(crate) fn emit_gated(cat: &str, args: fmt::Arguments<'_>) {
    write_line(&line(cat, Some(current_pid()), args));
}

/// An ungated notice: no pid.
///
/// These are the lines a normal run shows. Appending `pid=` to them would change
/// text an operator reads for no diagnostic gain, so the rule is the gate axis:
/// gated lines are attributed, unconditional ones are verbatim.
///
/// ⚠️ Nothing ASSERTS on that text. This doc used to say "two e2e tests match one
/// literally (`e2e/muslthread_test.go` on `"[ecvisor] musl"`)", which reads as a
/// guard and is not one: that test `t.Log`s the matching lines and asserts
/// elsewhere, and `e2e/exitfds_test.go` quotes the deadlock line in a COMMENT
/// while asserting on exit code 111. So a prefix change here is caught by review
/// and by reading a run's stderr, not by a test.
///
/// Reached on every target. It used to need `allow(dead_code)` off-wasm, when
/// every `ecv_warn!` site was in `entry` or `intrinsics`; `context` and
/// `execmap` are host-compiled and now call it too.
#[inline]
pub(crate) fn emit_plain(cat: &str, args: fmt::Arguments<'_>) {
    write_line(&line(cat, None, args));
}

/// The one place a diagnostic reaches the outside world.
fn write_line(s: &str) {
    #[cfg(test)]
    if tests::capture_line(s) {
        return;
    }
    eprintln!("{s}");
}

// ---------------------------------------------------------------------------
// Callsite macros
// ---------------------------------------------------------------------------

// Every macro takes the category as an identifier OR a string literal. The literal
// form is not sugar: `[ecv-emulate]` and `[ecv-wildstore]` are not valid Rust
// identifiers, and without it a conversion would have to rename them. Renaming a
// prefix is a visible change to stderr, so it should be a decision rather than a
// consequence of macro syntax.
//
// The category IS the prefix, so a converted site drops the brackets and keeps the
// rest:
//
//     eprintln!("[sched] pid={pid} blocked on {b:?}");   // before
//     ecv_trace!(sched, "blocked on {b:?}");             // after

/// A diagnostic behind an arbitrary gate expression.
///
/// This is the general form, and it is why the `tracing` version needed two
/// semantic compromises that this one does not: a gate here is any `bool`, so the
/// two sites gated on a union of flags say so directly.
macro_rules! ecv_log {
    ($gate:expr, $cat:literal, $($arg:tt)+) => {
        if $gate {
            $crate::trace::emit_gated($cat, ::core::format_args!($($arg)+))
        }
    };
    ($gate:expr, $cat:ident, $($arg:tt)+) => {
        if $gate {
            $crate::trace::emit_gated(::core::stringify!($cat), ::core::format_args!($($arg)+))
        }
    };
}

/// `RAPTORMARK_ECV_DEBUG` line.
macro_rules! ecv_debug {
    ($cat:literal, $($arg:tt)+) => {
        if $crate::diag::debug_log() {
            $crate::trace::emit_gated($cat, ::core::format_args!($($arg)+))
        }
    };
    ($cat:ident, $($arg:tt)+) => {
        if $crate::diag::debug_log() {
            $crate::trace::emit_gated(::core::stringify!($cat), ::core::format_args!($($arg)+))
        }
    };
}

/// `RAPTORMARK_ECV_TRACE` line.
macro_rules! ecv_trace {
    ($cat:literal, $($arg:tt)+) => {
        if $crate::diag::trace_log() {
            $crate::trace::emit_gated($cat, ::core::format_args!($($arg)+))
        }
    };
    ($cat:ident, $($arg:tt)+) => {
        if $crate::diag::trace_log() {
            $crate::trace::emit_gated(::core::stringify!($cat), ::core::format_args!($($arg)+))
        }
    };
}

/// A named probe: `RAPTORMARK_ECV_<CAT>`, where the category is also the name of
/// its gate function in [`crate::diag`].
///
/// `ecv_probe!(filetrace, ...)` expands to `if crate::diag::filetrace()` and prints
/// `[filetrace]`. The coincidence of the three names is the point -- an operator can
/// read a prefix off stderr and know which environment variable produced it, which
/// was not true of the `[ftrace]`/`RAPTORMARK_ECV_FILETRACE` pair it replaced. Valid
/// for `legsp`, `snapstat`, `snapcheck`, `filetrace` and `fdcheck`; anything else is
/// a compile error naming the missing `diag` function.
macro_rules! ecv_probe {
    ($cat:ident, $($arg:tt)+) => {
        if $crate::diag::$cat() {
            $crate::trace::emit_gated(::core::stringify!($cat), ::core::format_args!($($arg)+))
        }
    };
}

/// A line every run shows: an anomaly that is not fatal. Ungated, and no pid.
///
/// Note what this is NOT for. The fatal path -- `crate::fatal!`, `runtime_error`,
/// the panic hook, and the `eprintln!`s that dump guest state immediately before a
/// `fatal!` -- deliberately stays on raw `eprintln!`. Those are the last words
/// before an abort, and there is no reason for the final message to travel through
/// one more layer than it has to.
///
/// ⚠️ `context` and `execmap` used to keep `eprintln!` for their ungated notices,
/// on the reading that this macro would make them conditional. It does not -- see
/// the correction at the top of `context.rs`. With those 27 converted, the ONLY
/// diagnostics that bypass `write_line` are the 8 on the fatal path above.
macro_rules! ecv_warn {
    ($cat:literal, $($arg:tt)+) => {
        $crate::trace::emit_plain($cat, ::core::format_args!($($arg)+))
    };
    ($cat:ident, $($arg:tt)+) => {
        $crate::trace::emit_plain(::core::stringify!($cat), ::core::format_args!($($arg)+))
    };
}

pub(crate) use ecv_warn;
pub(crate) use {ecv_debug, ecv_log, ecv_probe, ecv_trace};

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::sync::Mutex;

    // Capture is THREAD-LOCAL, so a test that inspects rendered output cannot see
    // or be seen by another test running concurrently. `write_line` consults it
    // only under `cfg(test)`.
    thread_local! {
        static CAPTURE: RefCell<Option<String>> = const { RefCell::new(None) };
    }

    /// Returns true when the line was captured instead of written to stderr.
    pub(crate) fn capture_line(s: &str) -> bool {
        CAPTURE.with(|c| match &mut *c.borrow_mut() {
            Some(buf) => {
                buf.push_str(s);
                buf.push('\n');
                true
            }
            None => false,
        })
    }

    /// `pub(crate)` for `diag::undec_census_tests`, which has to observe that
    /// the census banner reaches the sink when the mode is armed and that
    /// nothing reaches it when the mode is off.
    pub(crate) fn capture(f: impl FnOnce()) -> String {
        CAPTURE.with(|c| *c.borrow_mut() = Some(String::new()));
        f();
        CAPTURE.with(|c| c.borrow_mut().take().unwrap())
    }

    // `diag`'s flags AND the pid atomic are process-wide, so every test that
    // touches either goes through `with_diag`, which serializes on this lock.
    //
    // The lock covers the assertions, not just the mutation, and it covers the pid
    // as well as the gates. Both matter: two of these tests assert on what is
    // emitted while a flag is OFF, which means nothing if another test can flip it
    // meanwhile -- and an earlier version set the pid outside the lock, so a test
    // expecting `pid=1` could observe the 4242 another test had installed.
    static DIAG: Mutex<()> = Mutex::new(());

    /// `[debug, trace, legsp, snapstat, snapcheck, filetrace, fdcheck]`.
    type Gates = [bool; 7];
    const ALL_ON: Gates = [true; 7];
    const ALL_OFF: Gates = [false; 7];

    /// Runs `f` with the process-wide diagnostic state as asked, returns what it
    /// emitted, and puts the state back.
    fn with_diag(g: Gates, pid: u32, f: impl FnOnce()) -> String {
        // `unwrap_or_else(into_inner)` so one panicking test does not cascade into
        // spurious failures in every other test that touches diagnostic state.
        let _lock = DIAG.lock().unwrap_or_else(|e| e.into_inner());
        let [debug, trace, legsp, snapstat, snapcheck, filetrace, fdcheck] = g;
        // `bounded` is deliberately NOT touched here. It changes BEHAVIOUR rather
        // than output, so flipping it either way under a concurrently running
        // context test is not a risk worth taking for a logging assertion -- and
        // since it left `set_gates`, this helper can no longer do so by accident.
        crate::diag::set_gates(debug, trace, legsp, snapstat, snapcheck);
        crate::diag::set_filetrace(filetrace);
        crate::diag::set_fdcheck(fdcheck);
        let prev = current_pid();
        set_current_pid(pid);
        let out = capture(f);
        set_current_pid(prev);
        crate::diag::set_gates(false, false, false, false, false);
        crate::diag::set_filetrace(false);
        crate::diag::set_fdcheck(false);
        out
    }

    #[test]
    fn a_category_renders_as_the_bracketed_prefix() {
        assert_eq!(line("sched", None, format_args!("x")), "[sched] x");
        assert_eq!(line("ecvisor", None, format_args!("x")), "[ecvisor] x");
        // Not an identifier, and kept rather than renamed. See the macro comment.
        assert_eq!(
            line("ecv-emulate", None, format_args!("x")),
            "[ecv-emulate] x"
        );
    }

    #[test]
    fn a_gated_line_carries_a_pid_and_an_ungated_one_does_not() {
        assert_eq!(
            line("sched", Some(42), format_args!("blocked")),
            "[sched] pid=42 blocked"
        );
        // The rule that keeps `[ecvisor] musl ...` matchable byte for byte by
        // e2e/muslthread_test.go.
        assert_eq!(
            line("ecvisor", None, format_args!("musl thread {} exited", 3)),
            "[ecvisor] musl thread 3 exited"
        );
    }

    #[test]
    fn the_pid_atomic_is_what_a_gated_line_renders() {
        // Not a test of `set_current_pid` alone: it checks that the value
        // `load_current` publishes is the value a line actually carries.
        let out = with_diag(ALL_ON, 4242, || emit_gated("sched", format_args!("hello")));
        assert_eq!(out, "[sched] pid=4242 hello\n");
    }

    #[test]
    fn a_converted_line_renders_exactly_as_the_eprintln_did() {
        // `eprintln!("[sched] pid=7 blocked on PipeRead(0)")`.
        let out = with_diag(ALL_ON, 7, || {
            ecv_trace!(sched, "blocked on {:?}", "PipeRead(0)");
        });
        assert_eq!(out, "[sched] pid=7 blocked on \"PipeRead(0)\"\n");
    }

    #[test]
    fn each_macro_answers_to_its_own_gate() {
        // With every flag on, all five macros emit, each under its own prefix.
        let out = with_diag(ALL_ON, 3, || {
            ecv_debug!(ecv, "d");
            ecv_trace!(ecv, "t");
            ecv_warn!(ecvisor, "w");
            ecv_probe!(filetrace, "f");
            ecv_probe!(fdcheck, "c");
            ecv_probe!(legsp, "l");
            ecv_probe!(snapstat, "s");
            ecv_probe!(snapcheck, "k");
            ecv_log!(true, bbmiss, "b");
        });
        // Gated lines carry the pid; the ungated `ecv_warn!` does not.
        assert!(out.contains("[ecv] pid=3 d\n"), "{out}");
        assert!(out.contains("[ecv] pid=3 t\n"), "{out}");
        assert!(out.contains("[ecvisor] w\n"), "{out}");
        assert!(out.contains("[filetrace] pid=3 f\n"), "{out}");
        assert!(out.contains("[fdcheck] pid=3 c\n"), "{out}");
        assert!(out.contains("[legsp] pid=3 l\n"), "{out}");
        assert!(out.contains("[snapstat] pid=3 s\n"), "{out}");
        assert!(out.contains("[snapcheck] pid=3 k\n"), "{out}");
        assert!(out.contains("[bbmiss] pid=3 b\n"), "{out}");
    }

    #[test]
    fn gated_macros_are_silent_with_every_flag_off() {
        // The property that makes the gates worth having, and the one that a
        // "does it print" test cannot show on its own.
        let out = with_diag(ALL_OFF, 1, || {
            ecv_debug!(ecv, "d");
            ecv_trace!(ecv, "t");
            ecv_probe!(filetrace, "f");
            ecv_probe!(fdcheck, "c");
            ecv_probe!(legsp, "l");
            ecv_probe!(snapstat, "s");
            ecv_probe!(snapcheck, "k");
            ecv_log!(false, bbmiss, "b");
        });
        assert_eq!(out, "");
    }

    #[test]
    fn an_ungated_warning_survives_every_flag_being_off() {
        // The other half: `ecv_warn!` must NOT be silenced by the gates, or the
        // deadlock and bring-up notices would vanish on a normal run.
        let out = with_diag(ALL_OFF, 1, || ecv_warn!(ecvisor, "musl thing"));
        assert_eq!(out, "[ecvisor] musl thing\n");
    }

    #[test]
    fn a_probe_is_not_switched_on_by_the_general_flags() {
        // What `RAPTORMARK_ECV_FDCHECK` being independent means: DEBUG and TRACE
        // together must not produce fdcheck output. Under the `tracing` version
        // this was a property of `gate_for`'s target match; here it is a property
        // of which `diag` function the macro names, so it is still worth pinning.
        let out = with_diag([true, true, false, false, false, false, false], 1, || {
            ecv_probe!(fdcheck, "should not appear");
            ecv_probe!(filetrace, "should not appear");
            ecv_debug!(ecv, "should appear");
        });
        assert_eq!(out, "[ecv] pid=1 should appear\n");
    }

    #[test]
    fn debug_and_trace_stay_independent() {
        // Restored by dropping `tracing`: with only TRACE set, a DEBUG line stays
        // quiet, exactly as the hand-rolled `if`s behaved. The `tracing` version
        // could not express this -- it had to make TRACE subsume DEBUG, because a
        // callsite there carries one level and one site wanted the union.
        let out = with_diag([false, true, false, false, false, false, false], 1, || {
            ecv_debug!(ecv, "debug only");
            ecv_trace!(ecv, "trace only");
        });
        assert_eq!(out, "[ecv] pid=1 trace only\n");
    }

    #[test]
    fn a_union_gate_fires_on_either_flag() {
        // The two sites that forced a semantic change under `tracing`:
        // `sys.rs`'s fd_write errno report and `context.rs`'s [bbmiss] report.
        // Here the union is just an expression, so it is exact.
        let union = |g: Gates| {
            with_diag(g, 1, || {
                ecv_log!(
                    crate::diag::debug_log() || crate::diag::trace_log(),
                    ecvisor,
                    "union"
                )
            })
        };
        let a = union([false, true, false, false, false, false, false]);
        let b = union([true, false, false, false, false, false, false]);
        let c = union(ALL_OFF);
        assert_eq!(a, "[ecvisor] pid=1 union\n", "TRACE alone must fire it");
        assert_eq!(b, "[ecvisor] pid=1 union\n", "DEBUG alone must fire it");
        assert_eq!(c, "", "neither flag must leave it quiet");
    }

    #[test]
    fn arguments_are_not_evaluated_when_the_gate_is_shut() {
        // Not a micro-optimisation: several converted sites pass expressions that
        // read guest memory or walk the call history, and the `if` has to sit
        // OUTSIDE them or a disabled diagnostic would still do that work.
        let mut ran = false;
        {
            let mut side_effect = || {
                ran = true;
                1u32
            };
            with_diag(ALL_OFF, 1, || {
                ecv_debug!(ecv, "{}", side_effect());
            });
        }
        assert!(!ran, "a disabled callsite evaluated its arguments");
    }
}
