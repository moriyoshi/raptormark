//! Lifted-code ABI: the aarch64 `State` layout and the `_ecv_*` symbols the
//! lifter injects into every lifted object.
//!
//! `State` mirrors `remill/Arch/AArch64/Runtime/State.h` at the pinned
//! submodule commit. The header pins its own layout with `static_assert`s
//! (GPR = 528, SR = 104, State = 1280); the `const` assertions below pin ours
//! to the same numbers. Regenerate/re-verify on every submodule pin bump.

use crate::context::EcvContext;

/// Signature of every lifted function and of `_ecv_entry_func`
/// (`LiftedFunc` in remill's `Types.h`). The last parameter was
/// `RuntimeManager*` upstream and is opaque to lifted code.
pub type LiftedFunc = unsafe extern "C" fn(*mut u8, *mut State, u64, *mut EcvContext);

/// One GPR slot: remill lays out each register behind an 8-byte shadow slot.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct PaddedReg {
    pub _shadow: u64,
    pub val: u64,
}

/// `struct GPR`: x0..x30, sp, pc — 33 padded slots, 528 bytes.
#[repr(C)]
pub struct Gpr {
    pub x: [PaddedReg; 31],
    pub sp: PaddedReg,
    pub pc: PaddedReg,
}

/// `struct SR`: system registers, 104 bytes.
#[repr(C)]
pub struct Sr {
    pub _0: u64,
    pub tpidr_el0: u64,
    pub _1: u64,
    pub tpidrro_el0: u64,
    pub _2: u64,
    pub ctr_el0: u64,
    pub _3: u64,
    pub dczid_el0: u64,
    pub _4: u64,
    pub midr_el1: u64,
    pub flags: [u8; 18],
    pub _padding: [u8; 6],
}

/// `struct State` (`AArch64State`), 1280 bytes, align 16.
#[repr(C, align(16))]
pub struct State {
    pub arch_state: [u8; 16], // remill ArchState base (hyper-call bookkeeping)
    pub simd: [u128; 32],
    pub _0: u64,
    pub gpr: Gpr,
    pub _1: u64,
    pub nzcv: u64,
    pub fpcr: u64,
    pub fpsr: u64,
    pub _2: u64,
    pub sr: Sr,
    pub _3: u64,
    pub _reserved_sleigh: [u8; 24],
    pub ecv_nzcv: u64,
    pub ecv_fpsr: u64,
    pub fork_entry_fun_addr: u64,
    pub inst_count: u64,
    pub func_depth: u64,
}

/// Byte offset of `State.gpr.x[30].val`, the guest link register.
///
/// elfconv patch 0060 BAKES this number into the inlined call-history fast path,
/// which writes x30 without going through `_ecv_save_call_history`. Nothing on
/// the C++ side can derive it, so the assertion below is the only thing standing
/// between a `State` layout change and a fast path that silently stores the
/// return address into the wrong register.
///
/// If that assertion fires, fix `kStateX30ValOffset` in
/// `backend/remill/lib/BC/ForkEmulation.cpp` (patch 0060) and rebuild the
/// builder image -- a stale value cannot be caught any later than here.
pub const STATE_X30_VAL_OFFSET: usize = 1024;

const _: () = {
    use core::mem::{offset_of, size_of};
    assert!(offset_of!(State, gpr) + 30 * 16 + 8 == STATE_X30_VAL_OFFSET);
    assert!(size_of::<Gpr>() == 528);
    assert!(size_of::<Sr>() == 104);
    assert!(size_of::<State>() == 1280);
    assert!(offset_of!(State, simd) == 16);
    assert!(offset_of!(State, gpr) == 536);
    assert!(offset_of!(State, sr) == 1104);
    assert!(offset_of!(State, func_depth) == 1272);
};

impl State {
    /// A zeroed State on the heap (all-zero is the valid initial register set).
    pub fn new_boxed() -> Box<State> {
        let mut b = Box::<State>::new_uninit();
        unsafe {
            core::ptr::write_bytes(b.as_mut_ptr(), 0, 1);
            b.assume_init()
        }
    }

    /// aarch64 Linux syscall convention: NR in x8, args in x0..x5, result in x0.
    pub fn syscall_nr(&self) -> u64 {
        self.gpr.x[8].val
    }
    pub fn arg(&self, n: usize) -> u64 {
        self.gpr.x[n].val
    }
    pub fn set_ret(&mut self, v: u64) {
        self.gpr.x[0].val = v;
    }
    pub fn set_ret_err(&mut self, linux_errno: u64) {
        self.gpr.x[0].val = (-(linux_errno as i64)) as u64;
    }
    pub fn pc(&self) -> u64 {
        self.gpr.pc.val
    }
}

/// One translated program's descriptor. Each lifted object's `_ecv_*`
/// singletons are namespaced (builder/namespace-object.sh) so N objects link
/// into one module; the generated `registry.c` gathers each program's renamed
/// symbols into one of these. Field order and types must match the C struct
/// emitted by `internal/link.RegistryC` exactly — the `ecv_program_size` guard
/// (checked in [`registry`]) catches any drift.
///
/// Every field is a pointer (all 4 bytes on wasm32, so the struct has no
/// interleaved padding). Array singletons point at their first element
/// directly; scalar singletons (entry_func, entry_pc, the counts, the phdr
/// sizes) are stored as pointers because C static initializers can take a
/// `const` scalar's *address* but not its runtime value. Accessors below deref
/// the scalars.
#[repr(C)]
pub struct EcvProgram {
    pub name: *const u8, // NUL-terminated program name (content hash)
    entry_func_p: *const LiftedFunc,
    pub fun_vmas: *const u64, // null-terminated
    pub fun_ptrs: *const Option<LiftedFunc>,
    pub block_ptrs: *const *const *mut u64,
    pub block_vmas: *const *const u64,
    pub block_sizes: *const u64,
    pub block_fn_vmas: *const u64,
    block_count_p: *const u64,
    pub data_names: *const *const u8,
    pub data_vmas: *const u64,
    pub data_sizes: *const u64,
    pub data_bytes: *const *const u8,
    data_num_p: *const u64,
    pub e_ph: *const u8,
    e_phent_p: *const u32,
    e_phnum_p: *const u32,
    entry_pc_p: *const u64,
}

extern "C" {
    // Defined by the generated registry.c: `EcvProgram *ecv_programs[]` (an
    // array of pointers to per-program descriptors, each defined in that
    // program's fragment).
    static ecv_programs: *const EcvProgram;
    static ecv_program_count: u64;
    static ecv_program_size: u64; // sizeof(EcvProgram) in C, for the ABI guard
}

/// The program registry: dereferences each pointer in `ecv_programs[]`. Panics
/// if the C struct size disagrees with the Rust one (an ABI-drift tripwire).
///
/// # An ABSENT registry is not a mismatched one
///
/// A supervisor linked WITHOUT any `registry.c` -- which is what the multi-module
/// build is (`.agents/docs/MULTIMODULE.md`; the programs are separate modules, so
/// there is no static array of them) -- resolves all three symbols to zero under
/// `--allow-undefined`. The size guard then read `C 0 vs Rust 72` and fatal'd
/// with an ABI-drift message about a struct nobody had declared.
///
/// The guard compares two spellings of one struct, so it only means anything
/// when there IS a C side. With no programs there is nothing to disagree about,
/// and the honest answer is an empty registry -- whose caller
/// (`entry.rs`) already reports `no entry program (registry is empty)`, which
/// names the real condition. Found by linking the supervisor standalone
/// 2026-08-18; the tripwire fired on a failure mode that did not exist when it
/// was written, and its message pointed away from the cause.
pub fn registry() -> Vec<&'static EcvProgram> {
    unsafe {
        if ecv_program_count == 0 {
            // No static array: this is either the multi-module build, where the
            // embedder has registered program modules, or a supervisor with no
            // programs at all. Freezing here is what makes a LATE registration
            // an error rather than a silent no-op -- from this point the caller
            // holds the list and nothing added later would be seen.
            *core::ptr::addr_of_mut!(FROZEN) = true;
            return (*core::ptr::addr_of!(DYNAMIC))
                .iter()
                .map(|p| &**p)
                .collect();
        }
        if ecv_program_size as usize != core::mem::size_of::<EcvProgram>() {
            crate::fatal!(
                "EcvProgram ABI mismatch: C {} vs Rust {} bytes",
                ecv_program_size,
                core::mem::size_of::<EcvProgram>()
            );
        }
        let arr = &ecv_programs as *const *const EcvProgram;
        (0..ecv_program_count as usize)
            .map(|i| &**arr.add(i))
            .collect()
    }
}

// ---------------------------------------------------------------------------
// The DYNAMIC registry, for the multi-module build
// ---------------------------------------------------------------------------
//
// A program is a separate wasm module there, so there is no static
// `ecv_programs[]` to read -- each program module exports its own
// `ecv_program_<i>` descriptor and something outside has to hand them over. That
// something is the embedder, before it calls `_start`.
//
// ADDITIVE. The flat build never touches any of this: its static array is
// non-empty, `registry()` returns it, and `ecv_register_program` is an export
// nobody calls. See .agents/docs/MULTIMODULE.md §5b.

/// Descriptors handed over by the embedder. `static mut` behind `addr_of`
/// accessors, as `intrinsics::WATCH_PREV` and `diag::SCAN_CLAUSES` are: the
/// runtime is a single-threaded cooperative scheduler and this is written only
/// during bring-up, before any guest code runs.
static mut DYNAMIC: Vec<*const EcvProgram> = Vec::new();

/// Set the first time `registry()` is consumed. After that a registration is a
/// silent no-op, so it is refused instead -- `EcvContext::new` has already read
/// the list, and an embedder that registers late would otherwise see success and
/// a module that never runs.
static mut FROZEN: bool = false;

/// Registration outcomes. Negative, and distinct, because the embedder is
/// outside this codebase and "it returned nonzero" is not a diagnosis.
pub const ECV_REG_OK: i32 = 0;
pub const ECV_REG_NULL: i32 = -1;
pub const ECV_REG_ABI: i32 = -2;
pub const ECV_REG_FROZEN: i32 = -3;
pub const ECV_REG_DUPLICATE: i32 = -4;
pub const ECV_REG_STATIC_PRESENT: i32 = -5;

/// Registers one program module's descriptor. Exported for the embedder.
///
/// `size` is the embedder's `sizeof(EcvProgram)`, and it is required rather than
/// inferred: the static path has `ecv_program_size` as its ABI-drift tripwire,
/// and dropping that guard on the new path would be a regression precisely where
/// drift is most likely -- a descriptor laid out by a DIFFERENT module's
/// generated fragment.
///
/// # Safety
///
/// `p` must point to a live `EcvProgram` that outlives the module, which for a
/// side module's exported descriptor it does. Nothing here dereferences it;
/// validation is by size and identity only, because there is nothing else a
/// pointer from outside can be checked against.
/// Where a LATE registration goes.
///
/// Set by `entry.rs` once the context exists. Before `_start` a registration
/// lands in `DYNAMIC` and is picked up when `registry()` is consumed; after it,
/// the list has already been copied into the live `Programs` and appending to
/// `DYNAMIC` would be the silent no-op `FROZEN` exists to prevent. So a frozen
/// registration is DELEGATED rather than refused.
///
/// A function pointer rather than a direct call because `abi.rs` compiles on the
/// host, where there is no `world()` -- and because the rules in `dyn_register`
/// are worth keeping pure and testable.
static mut LATE_HOOK: Option<fn(*const EcvProgram, u64) -> i32> = None;

/// Installs the late-registration hook. Called once, at bring-up.
///
/// # Safety
///
/// Single-threaded cooperative runtime, written once before any guest runs --
/// the same conditions `DYNAMIC` and `FROZEN` rely on.
pub unsafe fn set_late_register_hook(f: fn(*const EcvProgram, u64) -> i32) {
    *core::ptr::addr_of_mut!(LATE_HOOK) = Some(f);
}

#[no_mangle]
pub unsafe extern "C" fn ecv_register_program(p: *const EcvProgram, size: u64) -> i32 {
    let code = dyn_register(
        &mut *core::ptr::addr_of_mut!(DYNAMIC),
        *core::ptr::addr_of!(FROZEN),
        ecv_program_count,
        p,
        size,
    );
    // A frozen registry is no longer a refusal: it means the embedder is placing
    // a side module MID-RUN, which is the whole point of dynamic side-module
    // loading. It stays an error only when nothing is there to take it.
    if code == ECV_REG_FROZEN {
        if let Some(hook) = *core::ptr::addr_of!(LATE_HOOK) {
            return hook(p, size);
        }
        return ECV_REG_FROZEN;
    }
    if code == ECV_REG_OK {
        crate::trace::ecv_warn!(
            ecvisor,
            "registered program {} ({:?})",
            (*core::ptr::addr_of!(DYNAMIC)).len() - 1,
            p
        );
    }
    code
}

/// The decision half of `ecv_register_program`, taking its state explicitly so
/// it can be tested without a linked registry -- the host build cannot supply
/// `ecv_program_count`, and the rules here are the part worth testing.
///
/// It never dereferences `p`.
pub(crate) fn dyn_register(
    list: &mut Vec<*const EcvProgram>,
    frozen: bool,
    static_count: u64,
    p: *const EcvProgram,
    size: u64,
) -> i32 {
    if p.is_null() {
        return ECV_REG_NULL;
    }
    if size as usize != core::mem::size_of::<EcvProgram>() {
        return ECV_REG_ABI;
    }
    // A flat build already has its programs. Accepting here would build a second
    // list that `registry()` ignores, and report success for it.
    if static_count != 0 {
        return ECV_REG_STATIC_PRESENT;
    }
    if frozen {
        return ECV_REG_FROZEN;
    }
    if list.contains(&p) {
        return ECV_REG_DUPLICATE;
    }
    list.push(p);
    ECV_REG_OK
}

impl EcvProgram {
    pub fn entry_func(&self) -> LiftedFunc {
        unsafe { *self.entry_func_p }
    }
    pub fn entry_pc(&self) -> u64 {
        unsafe { *self.entry_pc_p }
    }
    pub fn e_phent(&self) -> u32 {
        unsafe { *self.e_phent_p }
    }
    pub fn e_phnum(&self) -> u32 {
        unsafe { *self.e_phnum_p }
    }
    pub fn block_count(&self) -> u64 {
        unsafe { *self.block_count_p }
    }
    pub fn data_num(&self) -> u64 {
        unsafe { *self.data_num_p }
    }

    /// The program's name (content hash) as bytes.
    pub fn name_bytes(&self) -> &[u8] {
        unsafe {
            let mut n = 0;
            while *self.name.add(n) != 0 {
                n += 1;
            }
            core::slice::from_raw_parts(self.name, n)
        }
    }

    /// Whether this descriptor carries a usable program header table.
    ///
    /// All THREE pointers must be checked, not just `e_ph`: `e_phent()` and
    /// `e_phnum()` dereference pointers of their own, so a descriptor with a
    /// header table but null counts (or the reverse) would fault while
    /// evaluating the guard that was supposed to prevent it.
    fn has_phdrs(&self) -> bool {
        !self.e_ph.is_null() && !self.e_phent_p.is_null() && !self.e_phnum_p.is_null()
    }

    /// The program's PT_TLS program header, if any, as
    /// `(template_vaddr, filesz, memsz, align)` — the static TLS template's VMA,
    /// its initialized (`.tdata`) size, its total (`.tdata` + `.tbss`) size, and
    /// its alignment. elflift copies the input ELF's program headers verbatim
    /// into `_ecv_e_ph`, so the prelinker's PT_TLS (offline TLS layout) survives
    /// here for ecvisor to set up the guest's static TLS block. Fields are read
    /// from the Elf64_Phdr at the fixed offsets p_vaddr(16)/filesz(32)/memsz(40)/
    /// align(48).
    pub fn tls_phdr(&self) -> Option<(u64, u64, u64, u64)> {
        const PT_TLS: u32 = 7;
        // Null-check BEFORE reading the scalars: `e_phent()` and `e_phnum()`
        // dereference their own pointers, so testing `e_ph` afterwards was too
        // late for any descriptor that does not have all three.
        if !self.has_phdrs() {
            return None;
        }
        let phent = self.e_phent() as usize;
        let phnum = self.e_phnum() as usize;
        if phent < 56 {
            return None;
        }
        unsafe {
            for i in 0..phnum {
                let p = self.e_ph.add(i * phent);
                if core::ptr::read_unaligned(p as *const u32) == PT_TLS {
                    return Some((
                        core::ptr::read_unaligned(p.add(16) as *const u64),
                        core::ptr::read_unaligned(p.add(32) as *const u64),
                        core::ptr::read_unaligned(p.add(40) as *const u64),
                        core::ptr::read_unaligned(p.add(48) as *const u64),
                    ));
                }
            }
        }
        None
    }

    /// A descriptor carrying nothing but a name, for host tests.
    ///
    /// `Programs` is reached only through this struct, so without a constructor
    /// its silent-drop path -- an exec-map entry naming a hash the registry does
    /// not contain -- cannot be unit-tested at all. That path has caused four
    /// separate incidents, which is a poor reason to leave it untestable.
    ///
    /// Every pointer but `name` is null, so ONLY `name_bytes`, `writable_loads`
    /// and `tls_phdr` are safe to call on one -- the latter two because they now
    /// guard on `has_phdrs` before touching anything. The rest
    /// (`entry_func`, `entry_pc`, `block_count`, `data_num`,
    /// `find_data_section`) dereference unconditionally and would fault.
    ///
    /// An earlier version of this comment claimed the phdr accessors "check for
    /// null first". They did not -- they read `e_phent()` and `e_phnum()`, each
    /// dereferencing its own pointer, and only then tested `e_ph`. Harmless in
    /// production, where nothing is null, and a fault the first time a test
    /// called them.
    #[cfg(test)]
    pub fn for_test(name: &'static [u8]) -> EcvProgram {
        assert_eq!(
            name.last(),
            Some(&0),
            "name must be NUL-terminated; name_bytes scans for the terminator"
        );
        EcvProgram {
            name: name.as_ptr(),
            entry_func_p: core::ptr::null(),
            fun_vmas: core::ptr::null(),
            fun_ptrs: core::ptr::null(),
            block_ptrs: core::ptr::null(),
            block_vmas: core::ptr::null(),
            block_sizes: core::ptr::null(),
            block_fn_vmas: core::ptr::null(),
            block_count_p: core::ptr::null(),
            data_names: core::ptr::null(),
            data_vmas: core::ptr::null(),
            data_sizes: core::ptr::null(),
            data_bytes: core::ptr::null(),
            data_num_p: core::ptr::null(),
            e_ph: core::ptr::null(),
            e_phent_p: core::ptr::null(),
            e_phnum_p: core::ptr::null(),
            entry_pc_p: core::ptr::null(),
        }
    }

    /// `for_test` plus a phdr table, for the paths that dereference
    /// `e_phent_p`/`e_phnum_p`/`entry_pc_p` unconditionally -- `build_stack`
    /// does, because a real descriptor always has them.
    #[cfg(test)]
    /// As `for_test`, but with EMPTY-but-VALID dispatch tables.
    ///
    /// `for_test` leaves `fun_vmas` and the block tables null, which is fine for
    /// anything that only reads the name -- and fatal for `build_tables`, which
    /// walks `fun_vmas` looking for a zero terminator and would dereference
    /// null. A static `[0]` IS that terminator, so the walk stops immediately
    /// and yields an empty table.
    ///
    /// Null-guarding `build_tables` instead was considered and rejected: it puts
    /// a branch in production code to serve a test, and the guard would then be
    /// the thing under test rather than the walk.
    #[cfg(test)]
    pub fn for_test_with_tables(name: &'static [u8]) -> EcvProgram {
        static TERM: [u64; 1] = [0];
        static NO_PTRS: [Option<LiftedFunc>; 1] = [None];
        static ZERO: u64 = 0;
        let mut p = EcvProgram::for_test(name);
        p.fun_vmas = TERM.as_ptr();
        p.fun_ptrs = NO_PTRS.as_ptr();
        p.block_count_p = &ZERO as *const u64;
        // A COUNT of zero, for the same reason the terminator above is a real
        // `[0]`: `Arena::load_data_sections` reads `data_num()` unconditionally,
        // so a null here is a null dereference the moment a test merges this
        // program with `with_data`. Zero says "no data sections", which is true
        // of a fixture and lets the loop exit on its own rather than being
        // guarded around.
        p.data_num_p = &ZERO as *const u64;
        p
    }

    /// A fixture carrying ONE data section, so tests that exercise
    /// `find_data_section` consumers (`.ecv.dlsyms`, `.ecv.irela`, ...) can see
    /// a non-empty table.
    ///
    /// Without it a lookup returns 0 because the SECTION is absent, which is
    /// indistinguishable from returning 0 because the symbol is absent -- and a
    /// test cannot tell a bounds check from an empty table. That
    /// indistinguishability let a bounds-check neutralization pass.
    #[cfg(test)]
    pub fn for_test_with_data(
        name: &'static [u8],
        sec: &'static [u8],
        bytes: &'static [u8],
    ) -> EcvProgram {
        let mut p = EcvProgram::for_test_with_tables(name);
        let names: &'static [*const u8] = Box::leak(Box::new([sec.as_ptr()]));
        let vmas: &'static [u64] = Box::leak(Box::new([0u64]));
        let sizes: &'static [u64] = Box::leak(Box::new([bytes.len() as u64]));
        let datas: &'static [*const u8] = Box::leak(Box::new([bytes.as_ptr()]));
        p.data_names = names.as_ptr();
        p.data_vmas = vmas.as_ptr();
        p.data_sizes = sizes.as_ptr();
        p.data_bytes = datas.as_ptr();
        p.data_num_p = Box::leak(Box::new(1u64));
        p
    }

    #[cfg(test)]
    pub fn for_test_with_phdrs(
        name: &'static [u8],
        phent: u32,
        phnum: u32,
        entry: u64,
        ph: &'static [u8],
    ) -> EcvProgram {
        let mut p = EcvProgram::for_test(name);
        p.e_phent_p = Box::leak(Box::new(phent));
        p.e_phnum_p = Box::leak(Box::new(phnum));
        p.entry_pc_p = Box::leak(Box::new(entry));
        p.e_ph = ph.as_ptr();
        p
    }

    /// How far above the thread pointer this program's static TLS reaches, in
    /// bytes -- the largest `tp_offset + memsz` over every TLS module.
    ///
    /// The source is `.ecv.tls`, NOT `tls_phdr()`. A fused multi-module image
    /// advertises no PT_TLS at all (measured: 0 TLS program headers in
    /// dash.fused and postgres-ext.fused); the prelinker instead emits a table
    /// of 32-byte entries `(template, filesz, memsz, tp_offset)` that
    /// `setup_tls` walks. Sizing the TLS area from `tls_phdr` therefore returns
    /// 0 for exactly the images that matter, which left a byte at
    /// `THREAD_PTR+0x40` outside the bounded-snapshot range set and still
    /// differing after the "fix".
    ///
    /// Falls back to the PT_TLS `memsz` for a single-module image, which is the
    /// pre-multi-module path `setup_tls` still honours.
    pub fn tls_extent_above_tp(&self) -> u64 {
        if let Some((_vma, size, bytes)) = self.find_data_section(b".ecv.tls") {
            let mut end = 0u64;
            let count = size / 32;
            unsafe {
                for i in 0..count {
                    let e = bytes.add(i * 32);
                    let memsz = core::ptr::read_unaligned(e.add(16) as *const u64);
                    let tp_offset = core::ptr::read_unaligned(e.add(24) as *const u64);
                    end = end.max(tp_offset.saturating_add(memsz));
                }
            }
            return end;
        }
        self.tls_phdr().map(|(_, _, memsz, _)| memsz).unwrap_or(0)
    }

    /// The WRITABLE PT_LOAD segments, as `(vaddr, memsz)`.
    ///
    /// This is the part of a process's image that a bounded arena snapshot would
    /// have to save. `.text` and `.rodata` are identical in every process
    /// running this program and never change, so only the writable segments
    /// (`.data`, `.got`, `.bss`) can differ between two processes -- which is
    /// also where the ifunc GOT fills and the startup relocations land.
    ///
    /// Read from the same `_ecv_e_ph` table `tls_phdr` walks, at the fixed
    /// Elf64_Phdr offsets p_type(0), p_flags(4), p_vaddr(16), p_memsz(40).
    pub fn writable_loads(&self) -> Vec<(u64, u64)> {
        const PT_LOAD: u32 = 1;
        const PF_W: u32 = 2;
        let mut out = Vec::new();
        if !self.has_phdrs() {
            return out;
        }
        let phent = self.e_phent() as usize;
        let phnum = self.e_phnum() as usize;
        if phent < 56 {
            return out;
        }
        unsafe {
            for i in 0..phnum {
                let p = self.e_ph.add(i * phent);
                if core::ptr::read_unaligned(p as *const u32) != PT_LOAD {
                    continue;
                }
                if core::ptr::read_unaligned(p.add(4) as *const u32) & PF_W == 0 {
                    continue;
                }
                out.push((
                    core::ptr::read_unaligned(p.add(16) as *const u64),
                    core::ptr::read_unaligned(p.add(40) as *const u64),
                ));
            }
        }
        out
    }

    /// Finds a data section by name, returning `(vma, size, bytes)` — the
    /// section's fused VMA, its byte length, and a pointer to its content. Used
    /// to locate ecvisor-private tables the prelinker synthesizes as allocatable
    /// data sections (e.g. `.ecv.irela`, the load-time ifunc GOT-slot table).
    pub fn find_data_section(&self, name: &[u8]) -> Option<(u64, usize, *const u8)> {
        // Guard before the deref, not after: `data_num()` dereferences its own
        // pointer, so a descriptor without the table faulted inside the very
        // call meant to look the table up. Same shape as the phdr accessors.
        if self.data_num_p.is_null() || self.data_names.is_null() {
            return None;
        }
        unsafe {
            let n = self.data_num() as usize;
            for i in 0..n {
                let nm = *self.data_names.add(i);
                if nm.is_null() {
                    continue;
                }
                let mut k = 0;
                loop {
                    let c = *nm.add(k);
                    if k == name.len() {
                        if c == 0 {
                            return Some((
                                *self.data_vmas.add(i),
                                *self.data_sizes.add(i) as usize,
                                *self.data_bytes.add(i),
                            ));
                        }
                        break;
                    }
                    if c != name[k] {
                        break;
                    }
                    k += 1;
                }
            }
        }
        None
    }
}

#[cfg(test)]
mod registry_tests {
    use super::*;

    // `dyn_register` never dereferences, so a fabricated non-null address is a
    // valid input and lets the RULES be tested without a linked registry --
    // which the host build cannot supply.
    fn prog(n: usize) -> *const EcvProgram {
        n as *const EcvProgram
    }
    const SZ: u64 = core::mem::size_of::<EcvProgram>() as u64;

    #[test]
    fn a_program_registers_once() {
        let mut l = Vec::new();
        assert_eq!(dyn_register(&mut l, false, 0, prog(1), SZ), ECV_REG_OK);
        assert_eq!(dyn_register(&mut l, false, 0, prog(2), SZ), ECV_REG_OK);
        assert_eq!(l, vec![prog(1), prog(2)]);
    }

    /// Registration order is not meaningful -- `Programs::load` keys `by_hash`
    /// on each program's name -- but a DUPLICATE is: it would put one program
    /// at two indices, and an exec-map lookup resolves to an index.
    #[test]
    fn the_same_descriptor_twice_is_refused() {
        let mut l = Vec::new();
        assert_eq!(dyn_register(&mut l, false, 0, prog(1), SZ), ECV_REG_OK);
        assert_eq!(
            dyn_register(&mut l, false, 0, prog(1), SZ),
            ECV_REG_DUPLICATE
        );
        assert_eq!(l.len(), 1, "a refused registration must not append");
    }

    /// The ABI tripwire the static path gets from `ecv_program_size`. Dropping
    /// it here would remove the guard exactly where drift is most likely: a
    /// descriptor laid out by a different module's generated fragment.
    #[test]
    fn a_descriptor_of_the_wrong_size_is_refused() {
        let mut l = Vec::new();
        assert_eq!(dyn_register(&mut l, false, 0, prog(1), SZ - 8), ECV_REG_ABI);
        assert_eq!(dyn_register(&mut l, false, 0, prog(1), 0), ECV_REG_ABI);
        assert!(l.is_empty());
    }

    #[test]
    fn a_null_descriptor_is_refused() {
        let mut l = Vec::new();
        assert_eq!(
            dyn_register(&mut l, false, 0, core::ptr::null(), SZ),
            ECV_REG_NULL
        );
    }

    /// After `registry()` has been read the list is already in the caller's
    /// hands, so a later registration would succeed and do nothing. Refusing is
    /// the difference between an embedder learning it called too late and an
    /// embedder shipping a module that never runs.
    #[test]
    fn registering_after_the_registry_was_read_is_refused() {
        let mut l = Vec::new();
        assert_eq!(dyn_register(&mut l, true, 0, prog(1), SZ), ECV_REG_FROZEN);
        assert!(l.is_empty());
    }

    /// A flat build already has its programs. Accepting here would build a
    /// second list that `registry()` ignores -- and report success for it.
    #[test]
    fn registering_into_a_flat_build_is_refused() {
        let mut l = Vec::new();
        assert_eq!(
            dyn_register(&mut l, false, 3, prog(1), SZ),
            ECV_REG_STATIC_PRESENT
        );
        assert!(l.is_empty());
    }

    /// Every refusal is distinguishable. The embedder is outside this codebase,
    /// so "nonzero" is not a diagnosis and two rules sharing a code would make
    /// one of them unreportable.
    /// ⚠️ This test is weaker than it reads, and both weaknesses were found by
    /// neutralizing rather than by inspection:
    ///
    /// * an explicit `is_power_of_two` check SURVIVED removal, because
    ///   `Layout::from_size_align` already enforces it. The check was redundant
    ///   and is gone; only the size-zero rule is ours.
    /// * the `try_from` truncation guard survives removal too, and always will
    ///   here: it protects wasm32's 32-bit `usize`, and the host's is 64-bit.
    ///   See `side_layout`.
    ///
    /// And `dylink.0` carries LOG2 of the alignment, so a caller that forgets to
    /// undo that passes 4 instead of 16 -- a power of two, accepted, and
    /// under-aligned. Nothing at this boundary can catch it.
    #[test]
    fn a_side_reservation_validates_its_request() {
        assert!(side_layout(443939, 16).is_some(), "a real dylink MEM_INFO");
        assert!(
            side_layout(0, 16).is_none(),
            "zero size has no address to give"
        );
        assert!(side_layout(64, 0).is_none(), "zero alignment");
        assert!(
            side_layout(64, 24).is_none(),
            "alignment must be a power of two"
        );
        assert!(
            side_layout(u64::MAX, 16).is_none(),
            "unrepresentable on wasm32"
        );
    }

    #[test]
    fn the_refusal_codes_are_distinct() {
        let all = [
            ECV_REG_OK,
            ECV_REG_NULL,
            ECV_REG_ABI,
            ECV_REG_FROZEN,
            ECV_REG_DUPLICATE,
            ECV_REG_STATIC_PRESENT,
        ];
        for (i, a) in all.iter().enumerate() {
            for b in &all[i + 1..] {
                assert_ne!(a, b, "two outcomes share a code");
            }
        }
    }
}

/// Reserves memory for a side module's data segment, and returns its address.
///
/// # Why the SUPERVISOR allocates, rather than the embedder
///
/// A side module declares its needs in its `dylink.0` MEM_INFO subsection --
/// measured on a real one: 443,939 bytes at 16-byte alignment, 901 table
/// entries -- and the embedder must place that somewhere before instantiating
/// it, passing the address as `__memory_base`.
///
/// The obvious placement is "past `__heap_base`" or "grow the memory and use the
/// old size". Both are hazards. This module's allocator (wasi-libc's dlmalloc,
/// under Rust's `alloc`) grows the SAME linear memory and tracks its own
/// high-water mark; a region the embedder took by calling `memory.grow` behind
/// its back is a region dlmalloc never saw, and a later allocation can be handed
/// out overlapping it. The failure would be a side module's data quietly
/// overwritten by the supervisor's heap -- silent, and long after placement.
///
/// Allocating HERE removes the hazard by construction: dlmalloc owns the region,
/// so it cannot hand it out again. The allocation is deliberately leaked, since
/// a side module's data lives as long as the module does, which is the process.
///
/// Returns 0 if the request is unserviceable, which the embedder must treat as
/// fatal -- there is nowhere else for that module's data to go.
///
/// # Safety
///
/// Nothing is dereferenced. `align` must be a power of two, as `dylink.0`
/// states it (it carries log2), or the request is refused.
#[no_mangle]
pub unsafe extern "C" fn ecv_reserve_side(size: u64, align: u64) -> u32 {
    let Some(layout) = side_layout(size, align) else {
        return 0;
    };
    let p = std::alloc::alloc_zeroed(layout);
    // Zeroed because a data segment is copied over only the parts the module
    // declares; the rest is .bss and must read as zero.
    crate::trace::ecv_warn!(
        ecvisor,
        "reserved {size} bytes (align {align}) for a side module at {:?}",
        p
    );
    p as u32
}

/// The validation half, split out so it can be tested: the host build has an
/// allocator but a 400 KB leak per test case is not what a unit test should do.
pub(crate) fn side_layout(size: u64, align: u64) -> Option<std::alloc::Layout> {
    // Only the size-zero rule is ours. `Layout::from_size_align` already rejects
    // a zero or non-power-of-two alignment and an oversized request, and an
    // explicit power-of-two check here was REDUNDANT -- proved by removing it and
    // watching the test still pass, which is the same evidence that would have
    // been read as "the guard is weak" if the code had been trusted instead.
    if size == 0 {
        return None;
    }
    // ⚠️ `try_from`, not `as`, and the host CANNOT test the difference: on
    // wasm32 `usize` is 32 bits, so `size as usize` TRUNCATES a large request
    // into a small successful allocation, and the side module's data then
    // overruns it. On the host `usize` is 64 bits and both spellings behave
    // identically, so `a_side_reservation_validates_its_request` passes either
    // way. This is an unneutralizable guard, recorded as such rather than
    // counted among the tested ones.
    let (s, a) = (usize::try_from(size).ok()?, usize::try_from(align).ok()?);
    std::alloc::Layout::from_size_align(s, a).ok()
}

#[cfg(test)]
mod late_register_tests {
    use super::*;

    /// The un-freeze, stated as a rule rather than as behaviour of the hook.
    ///
    /// `dyn_register` still REFUSES a frozen registration -- its job is the pure
    /// decision, and "the registry has been consumed" is true either way. What
    /// changed is what `ecv_register_program` does with that answer: it
    /// delegates to the live context instead of returning it. Keeping the two
    /// apart is what lets the rules stay testable without a linked registry.
    #[test]
    fn dyn_register_still_reports_frozen() {
        let mut list: Vec<*const EcvProgram> = Vec::new();
        let p = Box::leak(Box::new(EcvProgram::for_test(b"u\0"))) as *const EcvProgram;
        let sz = core::mem::size_of::<EcvProgram>() as u64;
        assert_eq!(dyn_register(&mut list, true, 0, p, sz), ECV_REG_FROZEN);
        assert!(
            list.is_empty(),
            "a frozen registration must not join the list"
        );
    }

    /// The rules that must keep refusing even under the un-freeze, because each
    /// describes a registration that could never work:
    ///   * a null descriptor has nothing to read;
    ///   * a size mismatch is ABI drift between the module and this runtime;
    ///   * a static registry means `registry()` will ignore the dynamic list.
    #[test]
    fn the_other_refusals_are_unaffected() {
        let mut list: Vec<*const EcvProgram> = Vec::new();
        let p = Box::leak(Box::new(EcvProgram::for_test(b"u\0"))) as *const EcvProgram;
        let sz = core::mem::size_of::<EcvProgram>() as u64;
        assert_eq!(
            dyn_register(&mut list, false, 0, core::ptr::null(), sz),
            ECV_REG_NULL
        );
        assert_eq!(dyn_register(&mut list, false, 0, p, sz + 8), ECV_REG_ABI);
        assert_eq!(
            dyn_register(&mut list, false, 3, p, sz),
            ECV_REG_STATIC_PRESENT
        );
        // And the ordinary case still works, or the three above prove nothing.
        assert_eq!(dyn_register(&mut list, false, 0, p, sz), ECV_REG_OK);
        assert_eq!(dyn_register(&mut list, false, 0, p, sz), ECV_REG_DUPLICATE);
    }
}
