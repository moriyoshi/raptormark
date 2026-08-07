//! The backend that asks a HOST to place a side module.
//!
//! # Why this also serves an Emscripten-shaped host
//!
//! The plan kept `hosted` and `emscripten` as separate candidates and deferred
//! the choice to Phase 0b's evidence. That evidence collapses them. Measured on
//! real artifacts (`.agents-workspace/tmp/dynload/foreignhost.mjs`, JOURNAL
//! 2026-08-23):
//!
//! * a raptormark side module imports **exactly 20 names, all from `env`** --
//!   lifted code reaches guest memory by `arena_ptr + addr` arithmetic, so
//!   there is almost nothing for a GOT to hold;
//! * it instantiates, relocates and runs its constructors against a host that
//!   supplies its OWN memory and table and stubs every intrinsic -- no
//!   supervisor involved;
//! * bring-up reaches **no intrinsic at all**.
//!
//! ⚠️ **CORRECTION 2026-08-24: "no `GOT.mem` / `GOT.func`" was measured on a
//! MAIN PROGRAM's side module and is false for a LIBRARY unit.** A library has
//! no entry function, so elflift emits no `_ecv_entry_func` and the descriptor
//! fragment's `.entry_func = &_ecv_entry_func` leaves an undefined data symbol;
//! the flat link resolves it to 0 via `--allow-undefined`, a PIC side link turns
//! it into `GOT.mem._ecv_entry_func`. Confirmed with `llvm-nm`: `U
//! _ecv_entry_func` in a unit object, `D _ecvmain_..._entry_func` in a main.
//!
//! The conclusion below SURVIVES, and is if anything strengthened: a GOT import
//! is Emscripten's own mechanism, and `library_dylink.js` supplies GOT entries
//! natively. What it costs is that a host cannot ignore the namespace -- see
//! `e2e/testdata/hostedembedder.mjs`, which supplies 0 from a one-name
//! ALLOWLIST rather than blanket-zeroing, because zeroing an unrecognised GOT
//! import silences a genuinely missing symbol and turns it into a null
//! dereference far from here.
//!
//! So conformance to Emscripten's `library_dylink.js` is a matter of NAMING,
//! not ABI, and two backends would have been two copies of one body. One
//! backend, and the host decides which convention it speaks on its own side.
//!
//! # What the host has to do
//!
//! The nine-step sequence in `.agents/docs/MULTIMODULE.md` §8, which
//! `e2e/testdata/embedder.mjs` already executes: read `dylink.0` MEM_INFO,
//! `ecv_reserve_side`, grow the table, instantiate against the supervisor's
//! memory/table/stack pointer, apply relocs, run ctors, read the descriptor
//! global (an OFFSET, not an address), `ecv_register_program`.
//!
//! ⚠️ "The only new part is that it happens MID-RUN" was the original claim and
//! it understated the job. §10 of that document has what running it actually
//! required: the host must drive `ecv_boot`/`ecv_run_slice` rather than `_start`
//! (which returns only when the guest is finished, leaving no window to
//! instantiate); it must serve loads BETWEEN slices, never from inside this
//! import, because a slice is on the wasm stack holding `&mut` on the process
//! table that `ecv_side_loaded` mutates; and registration lands on `push_late`,
//! not the frozen registry.

use super::{LoadOutcome, LoaderBackend};
use std::collections::HashMap;

/// What we know about one unit.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
enum UnitState {
    /// Never asked for.
    #[default]
    Cold,
    /// The host has been asked and has not answered.
    Requested,
    /// The host reported it placed and registered.
    Ready,
    /// The host reported it could not.
    Failed,
}

/// Asks the host, then waits.
#[derive(Default)]
pub struct Hosted {
    /// ❗ A MAP, NOT A VEC INDEXED BY `idx`.
    ///
    /// This was `Vec<UnitState>` grown with `resize(idx + 1)`, which assumes
    /// `idx` is a small dense registry index. It is not: a unit with no registry
    /// index yet -- every unit on its FIRST dlopen, because its descriptor lives
    /// in the side module -- is identified by a synthetic token starting at
    /// `PENDING_UNIT_BASE` (2^30). `resize` then asks for a billion elements.
    ///
    /// ⚠️ The first such call did not fail. It allocated ~1 GiB and carried on;
    /// the SECOND dlopen was what panicked, with `capacity overflow` in
    /// `raw_vec` and a wasm `unreachable` trap. So the symptom appeared one
    /// whole successful load away from the cause, and the successful load is
    /// what made it look like the loader worked.
    ///
    /// A map has no opinion about density. Unit counts are small -- tens, not
    /// thousands -- so nothing is lost.
    state: HashMap<usize, UnitState>,
}

impl Hosted {
    fn slot(&mut self, idx: usize) -> &mut UnitState {
        self.state.entry(idx).or_insert(UnitState::Cold)
    }
}

impl LoaderBackend for Hosted {
    fn request(&mut self, idx: usize, name: &[u8]) -> LoadOutcome {
        match *self.slot(idx) {
            UnitState::Ready => return LoadOutcome::Ready,
            UnitState::Failed => {
                // Sticky. A host that could not place a unit will not place it
                // on the next dlopen either, and re-asking would turn one
                // failure into an unbounded retry loop -- the guest re-enters
                // this syscall on every wake.
                return LoadOutcome::Failed("the host could not load this unit");
            }
            // Already asked: park again rather than asking twice. The guest
            // re-enters this syscall after any wake, including a spurious one.
            UnitState::Requested => return LoadOutcome::Pending,
            UnitState::Cold => {}
        }

        let rc = unsafe { host_load_side(idx as u32, name.as_ptr(), name.len() as u32) };
        match rc {
            // Placed synchronously -- a host that can compile without yielding
            // (node, a native embedder) may answer immediately, and making it
            // round-trip through a park would be pure latency.
            1 => {
                *self.slot(idx) = UnitState::Ready;
                LoadOutcome::Ready
            }
            0 => {
                *self.slot(idx) = UnitState::Requested;
                LoadOutcome::Pending
            }
            _ => {
                *self.slot(idx) = UnitState::Failed;
                LoadOutcome::Failed("the host refused to load this unit")
            }
        }
    }

    fn note_loaded(&mut self, idx: usize, ok: bool) {
        *self.slot(idx) = if ok {
            UnitState::Ready
        } else {
            UnitState::Failed
        };
    }

    fn backend_name() -> &'static str {
        "hosted"
    }
}

/// The one import this backend adds.
///
/// ⚠️ It is why the backend must be selected at COMPILE time. `wasm-ld` emits an
/// import for every undefined symbol reachable from live code, so linking this
/// module into the default profile would put `env.ecv_host_load_side` into the
/// artifact a stock runwasi shim loads -- and nothing there supplies it, so the
/// module would fail to instantiate before running a single instruction. See
/// `crate::loader`'s module doc.
///
/// Returns 1 if the unit is ready now, 0 if the host has started an
/// asynchronous load and will call `ecv_side_loaded`, and anything else for a
/// refusal.
#[link(wasm_import_module = "env")]
extern "C" {
    #[link_name = "ecv_host_load_side"]
    fn host_load_side(idx: u32, name: *const u8, name_len: u32) -> i32;
}

#[cfg(test)]
mod tests {
    // ⚠️ NOT unit-tested here, and the reason is structural rather than a gap
    // being waved through: `request` calls a wasm import, so this module only
    // compiles under `--target wasm32-wasip1`, which `cargo test` does not
    // build (see `lib.rs`'s cfg gates on `sys`). The behaviour that CAN be
    // tested on the host -- what a `Pending` does to the caller, and that the
    // wake reaches the right waiter -- is tested against the SEAM instead, in
    // `context.rs`'s `dlopen_tests`, which is where it belongs anyway.
}
