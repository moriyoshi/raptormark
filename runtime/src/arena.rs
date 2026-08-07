//! Guest memory arena: one identity-mapped 384 MiB region, plus the initial
//! guest stack build (argv/envp/auxv). Ports `runtime/Memory.h` constants and
//! `Memory.cpp`'s `MemoryArenaInit` from the pinned submodule.
//!
//! Layout (VMA == byte offset, identity map): fused-object LOW REGION
//! [pieExeBase 0x100000, BRK_START 0x0A00_0000) = 159 MiB (must hold the whole
//! prelinked closure — large glibc apps like postgres pull ~150 MiB of objects),
//! then brk [160 MiB, 256 MiB), mmap [256 MiB, 352 MiB), guest stack top at
//! 384 MiB. Grown from 256 MiB (low region was only 63 MiB) to fit big closures.
//! The prelinker's `arenaBrkStart` (internal/prelink/prelink.go) MUST match
//! BRK_START_VMA.
//!
//! WHY NOT 512 MiB. It WAS 512 for a day, on this measurement: initdb's
//! post-bootstrap backend walked malloc's doubling ladder -- 12, 24, 48, 96 MiB
//! -- and then asked `brk` for 96 MiB in one increment with the break already
//! at `0xf7d0000`. That ladder was real but it was a SYMPTOM, not a workload:
//! the by-element `M` bit was decoded wrong (patches/0045), ICU's hash table
//! read a zero high-water mark, and every insert looked like an overflow, so
//! the table rehashed all the way up its PRIMES list. With the lifter fixed the
//! ladder does not happen, and the 128 MiB it bought is dead weight -- paid by
//! EVERY process, on a budget that has no room for it.
//!
//! CEILING: ecvisor is wasm32 (4 GiB linear memory) and each SUSPENDED process
//! owns a full-size buffer (`swap_with` trades buffers rather than copying
//! contents), so arena_size × (live + suspended) < 4 GiB. This is not slack:
//! a measured postgres run reached 4010 MiB and then failed the NEXT 512 MiB
//! request, which was the backend for its first client. 384 MiB allows 10
//! buffers where 512 allowed 7, and postgres needs 7 concurrently to serve one
//! guest-side psql (dash, postmaster, checkpointer, bgwriter, walwriter,
//! backend, psql) before any second connection exists. Scaling to the
//! many-backend model needs BOUNDED snapshots (copy only live ranges), which
//! removes the tradeoff entirely and is tracked in .agents/docs/TODO.md.

use crate::abi::EcvProgram;

pub const MEMORY_ARENA_VMA: u64 = 0;
pub const MEMORY_ARENA_SIZE: usize = 384 * 1024 * 1024;
pub const BRK_START_VMA: u64 = 0x0A00_0000;
pub const BRK_END_VMA: u64 = 0x1000_0000;
pub const MMAP_START_VMA: u64 = 0x1000_0000;
pub const MMAP_END_VMA: u64 = 0x1600_0000;
pub const STACK_TOP_VMA: u64 = 0x1800_0000;
/// Thread pointer / TLS base (inside the low region). The 16-byte TCB sits at
/// [THREAD_PTR, THREAD_PTR+TCB_SIZE); static TLS blocks follow at positive
/// offsets (aarch64 variant-I layout — see `init_static_tls`).
/// Where the fused image starts. Also THREAD_PTR: the guest's TLS block begins
/// here, and the runtime writes pthread/dl bring-up structures just BELOW it --
/// which is why that sliver is its own region in the snapshot check.
pub const IMAGE_BASE_VMA: u64 = 0x0010_0000;
pub const THREAD_PTR: u64 = 0x0010_0000;

/// aarch64's `MINSIGSTKSZ`: the smallest stack a signal handler may run on, and
/// the floor glibc clamps `sysconf(_SC_MINSIGSTKSZ)` to when the kernel supplies
/// nothing. It is an architecture constant, not a libc-version one.
///
/// Used twice, and they must not drift: as `AT_MINSIGSTKSZ` in the initial stack
/// (emulation only -- a fused glibc never reads the auxv) and as the value
/// `apply_stacklists` writes into `_rtld_global_ro._dl_minsigstacksize`, which is
/// what actually satisfies glibc.
pub const MINSIGSTKSZ: u64 = 5120;
/// aarch64 TCB reservation between the thread pointer and the first TLS block.
/// The prelinker (`internal/prelink/tls.go`) bakes TPREL offsets assuming this
/// same 16-byte gap (`tcbSize`); the two MUST agree.
pub const TCB_SIZE: u64 = 16;
/// Floor of the low PER-PROCESS sliver that a bounded snapshot must carry.
///
/// The guest's thread-local state does not begin at `THREAD_PTR`: the runtime
/// writes pthread and `_dl_stack_*` bring-up structures just BELOW it, and
/// nothing in the ELF describes them -- they are in no PT_LOAD, so a range set
/// derived from program headers alone misses them entirely.
///
/// One page below the thread pointer, chosen EMPIRICALLY:
/// `RAPTORMARK_ECV_SNAPCHECK` put every observed difference at 0xff9a0..0xff9e0,
/// comfortably inside it. That is evidence, not a proof, which is exactly why
/// the check exists -- rerun it after touching the bring-up code, and anything
/// that slipped below this floor shows up as a `below_image` miss.
pub const LOW_PERPROCESS_FLOOR: u64 = THREAD_PTR - 0x1000;
/// Where the runtime builds musl's `struct tls_module` list (see
/// `EcvContext::seed_musl_tls`). It lives in the low per-process sliver rather
/// than in the mmap region for two reasons: it is per-process bring-up state
/// exactly like the pthread struct beside it, so a bounded snapshot already
/// carries this range; and taking it from `mmap_reserve` would make a startup
/// allocation shift every subsequent guest mmap address.
///
/// Placed WELL BELOW the structures already there -- the pthread struct sits at
/// [TP-0xc8, TP) and glibc's `_dl_stack_*` bring-up was measured at
/// 0xff9a0..0xff9e0 -- with `MUSL_TLS_MODULES_MAX` bounding the gap between
/// them. A seed that would not fit is refused rather than allowed to grow into
/// the neighbours.
pub const MUSL_TLS_MODULES_VMA: u64 = THREAD_PTR - 0xf00;
pub const MUSL_TLS_MODULES_MAX: u64 = 0x600;
/// Startup ifunc-resolver scratch, both in the free mmap region: a stack top
/// the resolver grows DOWN from (far from the guest stack and the guest data),
/// and a slot for the `__ifunc_arg_t` argument placed ABOVE the stack top so the
/// downward-growing frame never clobbers it. Used only transiently while ecvisor
/// runs ifunc resolvers before the guest's first instruction.
pub const IFUNC_STACK_TOP: u64 = MMAP_END_VMA - 0x200;
pub const IFUNC_ARG_VMA: u64 = MMAP_END_VMA - 0x40;
/// Scratch for the `pthread_attr_t` the runtime builds at bring-up (see
/// `EcvContext::apply_pthread_attr_default`). ABOVE `IFUNC_STACK_TOP`, like
/// `IFUNC_ARG_VMA` and for the same reason: that stack grows DOWN, and the
/// guest calls this buffer is passed to run on it, so anything below the top
/// would be inside their frames.
pub const PTHREAD_ATTR_VMA: u64 = IFUNC_STACK_TOP + 0x40;
/// glibc's `__SIZEOF_PTHREAD_ATTR_T` on aarch64.
pub const PTHREAD_ATTR_SIZE: u64 = 64;

/// A shared-memory VMA range that is EXEMPT from the per-process arena
/// save/restore. Because ecvisor has ONE physical arena and processes run one
/// at a time (cooperative), a range that `restore()` never overwrites persists
/// as a single physical copy every process sees at the same VMA — that is the
/// whole shared-memory mechanism. Registered by `mmap(MAP_SHARED|MAP_ANONYMOUS)`
/// and kept context-global on `EcvContext` (so fork inherits it automatically).
/// See `.agents/docs/SHAREDMEM.md`.
#[derive(Clone)]
pub struct SharedSeg {
    pub vma_start: u64,
    pub len: usize,
    pub kind: SharedKind,
    /// PIDs that currently map this region.
    ///
    /// A set of pids rather than a reference COUNT, because the events that end
    /// a mapping are not all symmetric with the one that made it: `exit` and
    /// `execve` tear down every mapping a process holds without a matching
    /// `munmap`, and a guest is free to `munmap` a range twice. Removing a pid
    /// is idempotent under both; decrementing a counter is not, and an
    /// off-by-one here either leaks the region forever or -- far worse --
    /// recycles memory another process is still reading.
    pub mappers: Vec<u32>,
}

impl SharedSeg {
    /// Whether `munmap(addr, len)` gives up the WHOLE of this region, and may
    /// therefore drop the caller's claim on it.
    ///
    /// `len` must ALREADY be page-rounded the way a registration is rounded
    /// (`mmap_round_len`); this deliberately does no rounding of its own, so the
    /// one rule lives at the one call site that also has the raw syscall
    /// argument. A 1000-byte `mmap(MAP_SHARED)` registers a 64 KiB region, and
    /// the guest's matching `munmap(p, 1000)` has to mean all of it.
    ///
    /// WHY THE LENGTH IS CHECKED AT ALL. `shm_seg_at` matches on the region's
    /// START, and `NR_MUNMAP` used to act on that alone. The start match does
    /// ignore a TAIL unmap -- it names no region's start -- but it does nothing
    /// about a HEAD one: `munmap(region, 4096)` on a 16 MiB region dropped the
    /// caller's claim and, if it was the last mapper, handed all 16 MiB back to
    /// the window while the caller still had the rest of it mapped. That is the
    /// recycling-memory-somebody-still-reads failure the start-only match was
    /// believed to avoid, and it is the direction that corrupts silently rather
    /// than leaking.
    ///
    /// A partial unmap is still IGNORED rather than honoured, which is what the
    /// entry in TODO.md tracks. Honouring one means SPLITTING the region, and
    /// three separate things here cannot express a split: `mappers` is a set of
    /// pids with no extents, so a split cannot say which process still holds
    /// which half; `shm_files` keys a POSIX region by its start VMA, so a split
    /// loses the name that other processes map it by; and `ShmSeg.vma` keys a
    /// SysV segment the same way, so it loses the shmid that `shmdt` and
    /// `shmctl` find it with. Leaking the window is the safe direction.
    ///
    /// An unmap that starts BELOW this region and covers it is likewise not
    /// recognised (`shm_seg_at` never finds it) and also leaks. Deliberate:
    /// recognising it means scanning every segment for overlap, which would let
    /// one wide `munmap` reclaim regions the guest never named.
    pub fn unmap_is_whole(&self, addr: u64, len: u64) -> bool {
        addr == self.vma_start && len >= self.len as u64
    }
}

/// What decides when a shared region may be reclaimed once nothing maps it.
///
/// The three shared-memory mechanisms all reduce to one arena region here, but
/// they do NOT share a lifetime rule, and treating them alike either leaks the
/// window or destroys a segment a guest still expects to find.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum SharedKind {
    /// `mmap(MAP_SHARED|MAP_ANONYMOUS)`. The region *is* the mapping: nothing
    /// names it, so once no process maps it nothing can ever reach it again and
    /// it is safe to reclaim. PostgreSQL's `shared_buffers` is this.
    Anon,
    /// `mmap(MAP_SHARED)` of a file (POSIX shm). The NAME outlives the mapping,
    /// so the last unmap is not the end: another process may `shm_open` the same
    /// path and expect the bytes that were written through the old mapping.
    /// Held while the backing path still resolves AND some mapping of it was
    /// ever writable -- glibc `MAP_SHARED`s permanent read-only rootfs files
    /// (the locale archive, the gconv cache) that no guest ever unlinks, and
    /// holding those pinned the window for the life of the module. See
    /// `context::shm_file_reclaimable`.
    File,
    /// SysV `shmget`. Deliberately outlives its mappers -- an IPC segment is
    /// destroyed by `IPC_RMID` after the last detach, and PostgreSQL's
    /// postmaster interlock depends on exactly that: the segment survives the
    /// process that created it so a second postmaster can find it and refuse to
    /// start.
    SysV,
}

/// Inserts `[start, end)` into a sorted, coalesced free list.
///
/// Shared by both window allocators. Keeping "no two entries touch or overlap"
/// as an invariant is what lets ranges freed in any order find each other, and
/// getting it subtly wrong in two places independently is the obvious way to
/// end up with a list that never merges and a window that fragments away.
fn insert_coalesced(free: &mut Vec<(u64, u64)>, start: u64, end: u64) {
    let i = free.partition_point(|&(s, _)| s < start);
    free.insert(i, (start, end));
    // Upper neighbour first, so the lower merge sees the already-widened entry.
    if i + 1 < free.len() && free[i].1 >= free[i + 1].0 {
        let e = free.remove(i + 1).1;
        free[i].1 = free[i].1.max(e);
    }
    if i > 0 && free[i - 1].1 >= free[i].0 {
        let e = free.remove(i).1;
        free[i - 1].1 = free[i - 1].1.max(e);
    }
}

/// Best fit over a free list: the smallest hole that still fits.
///
/// Not first fit. The holes are few and very unequal here -- malloc's arena
/// ladder leaves 0.75, 1.5, 3, 6, 12, 24 MiB corpses side by side -- so first
/// fit spends a large hole on a small request and then has to grow the window
/// for the large request it could have served.
fn best_fit(free: &[(u64, u64)], len: u64) -> Option<usize> {
    free.iter()
        .enumerate()
        .filter(|(_, &(s, e))| e - s >= len)
        .min_by_key(|(_, &(s, e))| e - s)
        .map(|(i, _)| i)
}

/// The allocator for the shared window: a descending bump from the top of the
/// mmap region, plus the holes reclaimed inside it.
///
/// Split out of `EcvContext` so the arithmetic is reachable from a host unit
/// test. The interesting cases are orderings -- free the lower of two regions
/// first, free them out of order, ask for a size only the larger hole fits --
/// and none of those can be provoked from an end-to-end run on demand.
///
/// A bump alone could not support the workload that motivated the window.
/// `initdb` runs `postgres --boot` several times in sequence and each run maps
/// its own `shared_buffers`; with nothing ever given back, the third run met
/// `FATAL: could not map anonymous shared memory: Cannot allocate memory` with
/// both of its predecessors long dead.
pub struct ShmWindow {
    /// Bottom of the allocated window: everything in `[top, MMAP_END_VMA)` is
    /// either live or in `free`.
    pub top: u64,
    /// Reclaimed holes as half-open `(start, end)` pairs, sorted by start.
    ///
    /// INVARIANT: no two entries touch or overlap. `release` restores it on
    /// every insert, and it is what lets two regions freed in the wrong order
    /// still find their way back to the bump.
    pub free: Vec<(u64, u64)>,
}

impl ShmWindow {
    pub fn new() -> ShmWindow {
        ShmWindow {
            top: MMAP_END_VMA,
            free: Vec::new(),
        }
    }

    /// Reserves `len` bytes, reusing a reclaimed hole before growing the window
    /// downward past `floor`. Returns the VMA, or None if neither can serve it.
    ///
    /// Deliberately does NOT initialise the bytes or register anything: the
    /// three callers disagree on both (a file mapping copies the file in over
    /// part of the region, SysV registers a `ShmSeg` alongside), so this is the
    /// allocator by itself.
    pub fn reserve(&mut self, len: u64, floor: u64) -> Option<u64> {
        // Best fit, not first fit. The holes here are few and very unequal --
        // one dead `postgres --boot`'s 16 MiB beside another's 39 MiB -- so
        // first fit would spend the large hole on a small request and then have
        // to grow the window for the large request it could have served.
        if let Some(i) = best_fit(&self.free, len) {
            let (s, e) = self.free[i];
            // Carve from the top, keeping the window's descending grain.
            let at = e - len;
            if at == s {
                self.free.remove(i);
            } else {
                self.free[i].1 = at;
            }
            return Some(at);
        }
        let at = self.top.checked_sub(len)?;
        if at < floor {
            return None;
        }
        self.top = at;
        Some(at)
    }

    /// Returns `[start, end)` to the window.
    pub fn release(&mut self, start: u64, end: u64) {
        if end <= start {
            return;
        }
        insert_coalesced(&mut self.free, start, end);
        // Anything now sitting on the bump goes back to it, so a run of
        // sequential processes leaves the window exactly as it found it rather
        // than as a growing list of holes. The invariant above means at most one
        // entry can qualify, but looping costs nothing and cannot spin: every
        // entry is non-empty, so `top` strictly increases.
        while let Some(i) = self.free.iter().position(|&(s, _)| s == self.top) {
            self.top = self.free.remove(i).1;
        }
    }
}

impl Default for ShmWindow {
    fn default() -> ShmWindow {
        ShmWindow::new()
    }
}

pub struct Arena {
    bytes: Vec<u8>,
    pub brk_cur: u64,
    /// Bump for PRIVATE mmap, ascending from MMAP_START_VMA. Everything below
    /// it is either live or in `mmap_free`.
    pub mmap_cur: u64,
    /// Reclaimed holes below `mmap_cur`, sorted and coalesced.
    ///
    /// Private mappings used to be a pure bump with `munmap` a no-op, on the
    /// reasoning that a bump cannot reclaim. That is true of a bump and false
    /// of the workload: glibc's malloc grows its arena by DOUBLING -- mmap a
    /// bigger one, munmap the old -- so reaching a 96 MiB arena first burned
    /// 0.75+1.5+3+6+12+24+48 MiB of superseded ones. A guest needed roughly
    /// twice its peak, and postgres's collation import died two locales in
    /// however much arena it was given. Measured on initdb; see the bump
    /// progression in .agents/docs/JOURNAL.md.
    pub mmap_free: Vec<(u64, u64)>,
    /// Extents handed out, as `(start, len)`.
    ///
    /// `munmap` reclaims only on an EXACT match against this list. A guest may
    /// unmap a sub-range (malloc trims arena tails), and releasing the whole
    /// region for a partial request would hand back memory still in use --
    /// silent corruption, where ignoring it merely leaks. Leaking is the safe
    /// direction, and the doubling case this exists for always unmaps whole.
    pub mmap_live: Vec<(u64, u64)>,
}

/// The ranges a bounded snapshot would copy, as `(vma, len)`.
///
/// A free function taking the four inputs explicitly, because it must produce
/// the SAME set for a live `Arena` and for a saved `ArenaSnapshot`. Two copies
/// of this rule, one per type, is how a save and a restore come to disagree
/// about which bytes matter -- and that disagreement is silent.
pub fn bounded_ranges(
    brk_cur: u64,
    mmap_live: &[(u64, u64)],
    writable_loads: &[(u64, u64)],
    sp: u64,
    tls_memsz: u64,
) -> Vec<(u64, u64)> {
    let mut out: Vec<(u64, u64)> = writable_loads
        .iter()
        .filter(|(vaddr, _)| *vaddr < BRK_START_VMA)
        .copied()
        .collect();
    // The TLS area, which straddles the image base and is invisible to the
    // program headers. `THREAD_PTR` IS the image base, so the static TLS block
    // sits at the very bottom of what looks like image -- measured, a byte of it
    // differed at THREAD_PTR+0x40 and read as an "image" difference. Below the
    // pointer is the pthread/dl scratch. One range covers both.
    let tls_end = THREAD_PTR + TCB_SIZE + tls_memsz;
    out.push((LOW_PERPROCESS_FLOOR, tls_end - LOW_PERPROCESS_FLOOR));
    out.push((BRK_START_VMA, brk_cur.saturating_sub(BRK_START_VMA)));
    out.extend(mmap_live.iter().copied());
    if sp >= MMAP_END_VMA && sp <= STACK_TOP_VMA {
        out.push((sp, STACK_TOP_VMA - sp));
    }
    out
}

/// The shared segments as arena OFFSETS, clamped to `n` and sorted ascending.
///
/// Callers walk this to rewrite the COMPLEMENT of the shared window -- `reset`
/// zeroes it, `restore_in_place`'s `Full` arm copies into it -- so both must
/// agree on what "shared" covers, which is why there is one function.
///
/// Ranges may overlap or nest (nothing forbids two registrations covering the
/// same bytes), so a caller must carry a high-water mark rather than assume they
/// are disjoint; sorting is what makes that mark valid.
fn shared_offsets(shared: &[SharedSeg], n: usize) -> Vec<(usize, usize)> {
    let mut ranges: Vec<(usize, usize)> = shared
        .iter()
        .map(|s| {
            let start = ((s.vma_start - MEMORY_ARENA_VMA) as usize).min(n);
            (start, (start + s.len).min(n))
        })
        .filter(|(s, e)| s < e)
        .collect();
    ranges.sort_unstable();
    ranges
}

/// Per-region result of `bytes_differing_outside`: how many bytes a bounded
/// snapshot would fail to restore, and where the first of them is.
pub struct SnapDiff {
    /// below_image, image, brk, mmap, stack.
    pub counts: [u64; 5],
    /// First differing address per region, `u64::MAX` when the region is clean.
    pub first: [u64; 5],
}

impl SnapDiff {
    pub fn total(&self) -> u64 {
        self.counts.iter().sum()
    }
}

/// What a saved process's memory is stored as.
///
/// ⚠️ This is NOT a mode any more. Until 2026-08-22 the two variants were two
/// schemes chosen by an environment variable; that variable was removed, and the
/// variant a process gets is now decided by the process -- see
/// `EcvContext::snapshot_for`, whose one remaining test is `is_multithreaded`.
/// `diag::tests::the_removed_snapshot_gate_stays_removed` is what fails if the
/// gate comes back.
pub enum SnapshotData {
    /// The whole arena, for a MULTI-THREADED group and nothing else.
    ///
    /// A bounded range set is derived from one stack pointer, and a group's
    /// siblings each have a live stack that pointer says nothing about, so a
    /// group has to carry everything. This is the only producer of this variant
    /// left, and it is not optional: deleting it breaks threads.
    ///
    /// It costs 384 MiB per suspended group, which is what capped concurrency at
    /// one guest-side client back when EVERY process took this path.
    Full(Vec<u8>),
    /// Only the ranges this process can have written, as `(vma, bytes)`.
    /// Measured at ~6 MiB against 384 (1.6%). A switch COPIES, but only these
    /// ranges, and the live arena buffer never moves. Every single-threaded
    /// process, unconditionally, since 2026-08-22.
    Bounded(Vec<(u64, Vec<u8>)>),
}

/// A saved arena for a non-running process: its memory plus the per-process
/// allocator bookkeeping that travels with it.
pub struct ArenaSnapshot {
    pub data: SnapshotData,
    /// Which program's image the saved ranges assume. A bounded restore must
    /// re-materialise the image when this differs from what is in the arena --
    /// measured, a cross-program switch differs by the whole 57 MiB image.
    pub prog_idx: usize,
    brk_cur: u64,
    mmap_cur: u64,
    mmap_free: Vec<(u64, u64)>,
    mmap_live: Vec<(u64, u64)>,
}

impl ArenaSnapshot {
    /// This snapshot's bounded ranges. Delegates to the shared rule.
    pub fn bounded_ranges(
        &self,
        writable_loads: &[(u64, u64)],
        sp: u64,
        tls_memsz: u64,
    ) -> Vec<(u64, u64)> {
        bounded_ranges(self.brk_cur, &self.mmap_live, writable_loads, sp, tls_memsz)
    }
}

/// Whether a `madvise` advice obliges the kernel to make the range read as
/// ZEROES afterwards.
///
/// # Why madvise cannot simply return 0
///
/// Most advice is exactly that -- a hint a kernel may ignore -- so success with
/// no action is the honest answer for a runtime with no paging. `MADV_DONTNEED`
/// is not advice. On an anonymous private mapping Linux guarantees the range
/// reads as zeroes afterwards, and allocators USE that: it is how a large free
/// returns memory without unmapping it. Answering 0 without zeroing hands the
/// guest its own stale bytes where it is entitled to zeroes, which is silent
/// corruption rather than a missing feature.
///
/// `MADV_FREE` (8) is deliberately NOT here. Linux may return the old contents
/// until the pages are reused, so zeroing is permitted but not required, and a
/// guest cannot depend on either -- so the no-op is honest and cheaper.
///
/// aarch64 `asm-generic/mman-common.h`: MADV_DONTNEED = 4, MADV_REMOVE = 9,
/// MADV_FREE = 8.
pub fn madvise_zeroes(advice: u64) -> bool {
    const MADV_DONTNEED: u64 = 4;
    // MADV_REMOVE punches a hole in a shared mapping: subsequent reads see
    // zeroes, by the same argument.
    const MADV_REMOVE: u64 = 9;
    matches!(advice, MADV_DONTNEED | MADV_REMOVE)
}

/// Alignment and granularity for guest mappings: 64 KiB, not 4 KiB.
///
/// The arena is flat, so any alignment "works" until a guest checks. glibc does:
/// `munmap_chunk` validates that an mmap-backed chunk's base and length are
/// page multiples, against `GLRO(dl_pagesize)` -- and in a frozen image ld.so
/// never ran, so that variable still holds aarch64 glibc's STATIC default,
/// `EXEC_PAGESIZE` = 65536. It never sees our AT_PAGESZ of 4096.
///
/// A 4 KiB-aligned mapping therefore looks malformed to it: 0x14c08000 & 0xffff
/// is 0x8000, and PostgreSQL died with `munmap_chunk(): invalid pointer` on the
/// first large allocation it freed. 64 KiB is the largest page any aarch64 guest
/// can believe in, and satisfies a 4 KiB believer too, so aligning to it is
/// correct for both rather than a guess about who is asking.
///
/// Lives here rather than in `sys` so that [`mmap_round_len`] -- and therefore
/// the length rules `NR_MMAP` enforces -- is reachable from a host unit test;
/// `sys` is gated to wasm32.
pub const GUEST_PAGE_MASK: u64 = 0xffff;

/// The page size we ADVERTISE (`AT_PAGESZ` = 4096), as a mask.
///
/// Distinct from [`GUEST_PAGE_MASK`] on purpose, and the distinction is the one
/// `NR_MMAP` already makes for its offset check: we ALIGN generously, to the
/// 64 KiB a frozen aarch64 glibc believes in, and we VALIDATE leniently, against
/// the 4096 we put in the auxv -- because a guest that page-aligned an argument
/// from `sysconf(_SC_PAGESIZE)` is entitled to have it accepted. Rejecting a
/// 4 KiB-aligned address as "not page-aligned" would be a rule we never told the
/// guest about.
pub const AUXV_PAGE_MASK: u64 = 0xfff;

/// Why a guest `mmap` length is unusable, distinguished by the errno Linux
/// answers with. Kept as a variant rather than a raw errno so the errno numbers
/// stay in `sys`, where every other one lives.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MmapLenError {
    /// `do_mmap`'s very first statement: `if (!len) return -EINVAL`. Checked
    /// before anything else, MAP_FIXED included, because Linux checks it before
    /// anything else -- a zero-length mapping is refused whether or not the
    /// caller named an address.
    Zero,
    /// Page-aligning the length wrapped. Linux writes this as
    /// `len = PAGE_ALIGN(len); if (!len) return -ENOMEM;` with the comment
    /// "Careful about overflows..", and ENOMEM is the honest answer: the
    /// mapping cannot be placed, rather than the arguments being malformed.
    /// EINVAL would be the wrong one -- a guest that probes downward, as
    /// PostgreSQL's initdb does for `shared_buffers`, reads ENOMEM as "ask for
    /// less" and EINVAL as "stop asking".
    Overflow,
}

/// Page-rounds a guest `mmap` length the way `do_mmap` does, or reports the
/// reason Linux would refuse it.
///
/// # Why this exists
///
/// `NR_MMAP` used to compute `(len + GUEST_PAGE_MASK) & !GUEST_PAGE_MASK`
/// inline and unchecked, which diverged from Linux in two ways:
///
/// * `mmap(NULL, 0, ...)` SUCCEEDED. The rounding turned 0 into 0, the bump
///   allocator reserved a zero-byte slot, and the guest got back an address for
///   a mapping of nothing -- where Linux returns EINVAL. Silent, not loud: a
///   guest that then wrote through that address would scribble on whatever the
///   next reservation handed out.
/// * A `len` above `u64::MAX - GUEST_PAGE_MASK` WRAPPED. Measured, not
///   assumed: the wrapped sum is always below `GUEST_PAGE_MASK`, so masking it
///   yields exactly ZERO for every such length -- a colossal request became the
///   same zero-byte mapping as case one, by a different route. That is why the
///   two guards are independent and both needed: a `len == 0` test alone does
///   not catch this, because the zero appears after the rounding.
///
/// This is the same wrap the `checked_add` bounds test on the MAP_FIXED path
/// was added for, after a wild `addr + len` passed a bounds check with a small
/// sum and panicked the arena. Here the wrap does not panic, which is worse:
/// the guest gets an address back.
pub fn mmap_round_len(len: u64) -> Result<u64, MmapLenError> {
    if len == 0 {
        return Err(MmapLenError::Zero);
    }
    // `checked_add` and not `wrapping_add`+zero-test: the two agree for a 64 KiB
    // mask (only a wrap can produce 0 from a non-zero len), but the intent is
    // "did this overflow", and stating it that way survives someone changing the
    // mask.
    match len.checked_add(GUEST_PAGE_MASK) {
        Some(sum) => Ok(sum & !GUEST_PAGE_MASK),
        None => Err(MmapLenError::Overflow),
    }
}

/// The longest anonymous-VMA name Linux accepts, NUL EXCLUDED.
///
/// The kernel's own constant is `ANON_VMA_NAME_MAX_LEN = 80` and it is the size
/// of the buffer INCLUDING the terminator: `strndup_user(uname, 80)` refuses a
/// string whose NUL does not land inside 80 bytes. Spelled here as the usable
/// length instead, because that is the number the check is written against.
/// Measured on Linux 6.17/aarch64: a 79-character name returns 0 and an
/// 80-character name is EINVAL.
pub const ANON_NAME_MAX_LEN: usize = 79;

/// True for a byte Linux allows in an anonymous-VMA name.
///
/// The kernel's rule (`mm/madvise.c`) is `ch > 0x1f && ch < 0x7f` minus the five
/// characters `[ ] \ ` $`, and the reason is that the name is printed into
/// `/proc/<pid>/maps` as `[anon:NAME]` -- the brackets would forge a field and
/// the other three are shell metacharacters in a file everything greps.
///
/// ⚠️ SPACE (0x20) IS ALLOWED and DEL (0x7f) is not, which is the opposite of
/// the natural guess for both. Measured: `"a b"` returns 0, `"a\tb"`,
/// `"a\x7fb"`, `"a\x1fb"` and `"a\x80b"` are each EINVAL.
pub fn anon_name_char_permitted(ch: u8) -> bool {
    ch > 0x1f && ch < 0x7f && !matches!(ch, b'[' | b']' | b'\\' | b'`' | b'$')
}

/// True for an anonymous-VMA name Linux would accept (NUL excluded).
///
/// The empty name is accepted -- measured, it shows up as a literal `[anon:]` --
/// so this is deliberately not an `is_empty` refusal.
pub fn anon_name_permitted(name: &[u8]) -> bool {
    name.len() <= ANON_NAME_MAX_LEN && name.iter().all(|&ch| anon_name_char_permitted(ch))
}

/// What `prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, addr, len, name)` should do
/// with its address range, before anything looks at what is mapped there.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AnonNameRange {
    /// `len` was zero, so Linux returns 0 having consulted no mapping at all.
    /// ⚠️ Including for an address that is not mapped: measured, a zero length
    /// at an unmapped address returns 0 where a page-sized one returns ENOMEM.
    Empty,
    /// The page-rounded half-open range `[start, end)` the name applies to.
    Range { start: u64, end: u64 },
    /// EINVAL: `addr` is not page-aligned, or `addr + PAGE_ALIGN(len)` wrapped.
    Invalid,
}

/// Decides the range exactly as `madvise_set_anon_name` does, in its order.
///
/// The order is load-bearing and was measured, not read off: alignment is
/// checked before the length, the length is page-ROUNDED UP (a `len` of 1 names
/// a whole page -- measured: `len` 1 and `len` 4097 both succeed and name 1 and
/// 2 pages), and a zero length short-circuits to success before any mapping is
/// consulted.
///
/// ⚠️ Rounding is against [`AUXV_PAGE_MASK`], not [`GUEST_PAGE_MASK`], for the
/// reason spelled out on `AUXV_PAGE_MASK`: the guest computed these arguments
/// against the 4096 we advertised.
pub fn anon_name_range(addr: u64, len: u64) -> AnonNameRange {
    if addr & AUXV_PAGE_MASK != 0 {
        return AnonNameRange::Invalid;
    }
    // `checked_add` rather than Linux's `if (len_in && !len)` post-test: for a
    // 4 KiB mask the two agree (only a wrap can round a non-zero length to
    // zero), and the intent is "did this overflow". Same argument as
    // `mmap_round_len`'s, which is why they are written the same way.
    let Some(rounded) = len.checked_add(AUXV_PAGE_MASK) else {
        return AnonNameRange::Invalid;
    };
    let rounded = rounded & !AUXV_PAGE_MASK;
    let Some(end) = addr.checked_add(rounded) else {
        return AnonNameRange::Invalid;
    };
    if end == addr {
        return AnonNameRange::Empty;
    }
    AnonNameRange::Range { start: addr, end }
}

/// True if the half-open guest range `[start, end)` lies inside the arena.
///
/// ⚠️ Exists instead of `Arena::in_bounds` for one reason: `in_bounds` takes its
/// length as a `usize`, which is 32-BIT on wasm32, so a range of exactly 4 GiB
/// truncates to a length of zero and passes. That is the same truncation
/// `dumpable_arg_permitted` is written in `u64` to avoid, and a range is the
/// place it is easiest to reach -- `PR_SET_VMA_ANON_NAME` takes both ends from
/// guest registers.
///
/// A free function so a host test can call it without allocating the 384 MiB an
/// `Arena` carries.
pub fn range_in_arena(start: u64, end: u64) -> bool {
    start >= MEMORY_ARENA_VMA && end >= start && end <= MEMORY_ARENA_VMA + MEMORY_ARENA_SIZE as u64
}

impl Arena {
    pub fn new() -> Arena {
        Arena {
            bytes: vec![0u8; MEMORY_ARENA_SIZE],
            brk_cur: BRK_START_VMA,
            mmap_cur: MMAP_START_VMA,
            mmap_free: Vec::new(),
            mmap_live: Vec::new(),
        }
    }

    pub fn base_ptr(&mut self) -> *mut u8 {
        self.bytes.as_mut_ptr()
    }

    /// Snapshots the arena contents + break pointers for fork/context-switch.
    /// The live arena buffer address is fixed, so a process's saved bytes can
    /// be reloaded into the same live arena and any asyncify-saved stack (which
    /// baked in that fixed base) stays valid.
    pub fn snapshot(&self, prog_idx: usize) -> ArenaSnapshot {
        ArenaSnapshot {
            data: SnapshotData::Full(self.bytes.clone()),
            prog_idx,
            brk_cur: self.brk_cur,
            mmap_cur: self.mmap_cur,
            mmap_free: self.mmap_free.clone(),
            mmap_live: self.mmap_live.clone(),
        }
    }

    /// Saves only `ranges`, which must be what `bounded_ranges` produced for
    /// this process. Everything outside them is either identical in every
    /// process (the read-only image) or unreachable until the process allocates
    /// it -- and allocation zero-fills, so the previous occupant's bytes can
    /// never be observed. See the design note in .agents/docs/TODO.md; every
    /// clause of that sentence was measured before this was written.
    pub fn snapshot_bounded(&self, ranges: &[(u64, u64)], prog_idx: usize) -> ArenaSnapshot {
        let n = self.bytes.len();
        let mut saved = Vec::with_capacity(ranges.len());
        for &(vma, len) in ranges {
            if len == 0 {
                continue;
            }
            let start = (vma as usize).min(n);
            let end = (start + len as usize).min(n);
            if start < end {
                saved.push((vma, self.bytes[start..end].to_vec()));
            }
        }
        ArenaSnapshot {
            data: SnapshotData::Bounded(saved),
            prog_idx,
            brk_cur: self.brk_cur,
            mmap_cur: self.mmap_cur,
            mmap_free: self.mmap_free.clone(),
            mmap_live: self.mmap_live.clone(),
        }
    }

    /// Materialises `snap` INTO the live arena -- whichever variant it is -- and
    /// adopts its bookkeeping. The live buffer's address never changes, which is
    /// the whole point: this is the restore half of the bounded scheme, where
    /// there is ONE live arena and a suspended process holds only bytes.
    ///
    /// It is deliberately TOTAL over `SnapshotData`, and the `match` is
    /// exhaustive so that it stays total. It was not, and that was a live bug:
    /// `snapshot_for` legitimately returns a `Full` snapshot even under bounded
    /// snapshots (a multi-threaded group's ranges cannot be derived from one
    /// stack pointer), and an `if let SnapshotData::Bounded(..)` here restored
    /// NO MEMORY for such a process while still adopting its brk/mmap
    /// bookkeeping. A process forked from a thread then ran on whatever the
    /// previously-scheduled process had left in the live arena and died calling
    /// through a null pointer. Compare `swap_with`, whose opposite mismatch is a
    /// `fatal!`: that half was loud, this half was silent, and only the silent
    /// half shipped a corruption.
    ///
    /// `shared` is the live shared segments, which the `Full` arm must SKIP.
    /// Shared memory has one physical copy and it belongs to whoever is running,
    /// so the incoming process's saved buffer holds a stale view of it -- the
    /// same hazard `adopt_shared_from` exists to repair on the swapping path,
    /// except that here the correct bytes are already in the live arena and the
    /// fix is to not overwrite them. The `Bounded` arm needs no such care:
    /// `bounded_ranges` never covers the shared window.
    ///
    /// The caller MUST have re-materialised the program image first when
    /// `snap.prog_idx` differs from what the arena holds and the snapshot is
    /// `Bounded`; that variant only restores what the process itself wrote.
    pub fn restore_in_place(&mut self, snap: &ArenaSnapshot, shared: &[SharedSeg]) {
        let n = self.bytes.len();
        match &snap.data {
            SnapshotData::Bounded(ranges) => {
                for (vma, bytes) in ranges {
                    let start = (*vma as usize).min(n);
                    let end = (start + bytes.len()).min(n);
                    if start < end {
                        self.bytes[start..end].copy_from_slice(&bytes[..end - start]);
                    }
                }
            }
            // COPY, never swap. Trading buffers here would move the live arena's
            // address -- which the bounded scheme's other processes, holding
            // ranges rather than buffers, have no way to hear about -- and would
            // leave the outgoing bounded snapshot describing a buffer that is no
            // longer live. A whole-arena copy is what a multi-threaded group
            // already pays for its snapshot; it pays the same on the way back.
            SnapshotData::Full(buf) => {
                let end = buf.len().min(n);
                let mut at = 0usize;
                for (s, e) in shared_offsets(shared, end) {
                    if s > at {
                        self.bytes[at..s].copy_from_slice(&buf[at..s]);
                    }
                    at = at.max(e);
                }
                if at < end {
                    self.bytes[at..end].copy_from_slice(&buf[at..end]);
                }
            }
        }
        self.brk_cur = snap.brk_cur;
        self.mmap_cur = snap.mmap_cur;
        self.mmap_free = snap.mmap_free.clone();
        self.mmap_live = snap.mmap_live.clone();
    }

    /// Bytes a bounded snapshot occupies -- what the scheme actually costs.
    pub fn snapshot_len(snap: &ArenaSnapshot) -> usize {
        match &snap.data {
            SnapshotData::Full(b) => b.len(),
            SnapshotData::Bounded(r) => r.iter().map(|(_, b)| b.len()).sum(),
        }
    }

    /// Exchanges the live buffer with a saved one.
    ///
    /// ⚠️ **Nothing on the switch path calls this since 2026-08-22.** It was the
    /// restore half of the full-buffer scheme, and `load_current` now always goes
    /// through `restore_in_place`, whose `Full` arm copies instead. It is kept
    /// deliberately, not by oversight: the buffer trade is the only O(1) restore
    /// this arena has, it is the technique a re-introduced whole-arena path would
    /// want, and its `Bounded` arm is a documented `fatal!` rather than a
    /// silent wrong answer. Its tests are its only callers -- so treat it as
    /// unexercised by any workload, and re-validate before wiring it back in.
    ///
    /// Every non-running process already owned a full-size buffer under that
    /// scheme, so a switch never needed to move 384 MiB in each direction -- it
    /// only had to trade which buffer is live. Measured before it existed:
    /// `snapshot` 19.98 ms mean and `restore` 17.05 ms mean, ~37 ms per switch,
    /// 95% of nginx's request wall clock.
    ///
    /// ⚠️ The live arena's ADDRESS changes here, and that is precisely what the
    /// old copy-based scheme existed to prevent. Every holder of `base_ptr`
    /// must re-read it afterwards; `entry.rs` does so once per scheduler leg.
    /// Guest-side pointers are unaffected -- under the identity map a guest
    /// pointer is a VMA, not a host address.
    pub fn swap_with(&mut self, s: &mut ArenaSnapshot) {
        match &mut s.data {
            SnapshotData::Full(b) => core::mem::swap(&mut self.bytes, b),
            // Bounded snapshots have no buffer to trade; the live arena stays
            // put and `restore_in_place` copies into it instead.
            //
            // This `fatal!` STAYS. It is the loud half of the pair whose silent
            // half (`restore_in_place` ignoring a `Full` snapshot) shipped a
            // memory corruption: a bounded snapshot genuinely has no buffer to
            // trade, so there is no correct thing to do here, and the only
            // choice is between saying so and swapping garbage. Note the
            // asymmetry is real rather than an oversight to be tidied away --
            // `restore_in_place` can be made total because both variants have a
            // meaning under an in-place restore; `swap_with` cannot.
            SnapshotData::Bounded(_) => {
                crate::fatal!("swap_with called on a bounded snapshot")
            }
        }
        core::mem::swap(&mut self.brk_cur, &mut s.brk_cur);
        core::mem::swap(&mut self.mmap_cur, &mut s.mmap_cur);
        // The private allocator's bookkeeping is PER PROCESS, exactly like the
        // bump it accompanies, so it travels with the arena. (The shared
        // window's does not -- that one is context-global by definition.)
        core::mem::swap(&mut self.mmap_free, &mut s.mmap_free);
        core::mem::swap(&mut self.mmap_live, &mut s.mmap_live);
    }

    pub fn brk_cur(&self) -> u64 {
        self.brk_cur
    }
    pub fn mmap_live(&self) -> &[(u64, u64)] {
        &self.mmap_live
    }

    /// Upper bound on what a BOUNDED snapshot of this process would have to
    /// copy, as `(total, image_writable, brk, mmap, stack)` in bytes.
    ///
    /// Purely diagnostic, and deliberately an upper bound rather than a true
    /// dirty set: it sums the ranges a process CAN have written, not the pages
    /// it did. That is the honest number for the decision it informs, because a
    /// bounded snapshot without page-level dirty tracking -- which wasm gives no
    /// way to do, since detecting writes would mean instrumenting every store in
    /// lifted code -- has to copy exactly these ranges.
    ///
    /// If this is not much smaller than `MEMORY_ARENA_SIZE`, bounded snapshots
    /// are not worth building, and that is the point of measuring it first.
    /// See the design note in .agents/docs/TODO.md.
    pub fn bounded_snapshot_bytes(
        &self,
        writable_loads: &[(u64, u64)],
        sp: u64,
        tls_memsz: u64,
    ) -> (u64, u64, u64, u64, u64) {
        // Terms are reported separately, but each one must match what
        // `bounded_ranges` would emit -- the sum and the ranges are two views of
        // one rule and a divergence would make the measurement describe
        // something the implementation would not copy.
        let image: u64 = writable_loads
            .iter()
            .filter(|(vaddr, _)| *vaddr < BRK_START_VMA)
            .map(|(_, memsz)| *memsz)
            .sum::<u64>()
            + (THREAD_PTR + TCB_SIZE + tls_memsz - LOW_PERPROCESS_FLOOR);
        let brk = self.brk_cur.saturating_sub(BRK_START_VMA);
        // `mmap_live` holds (start, LENGTH), not (start, end) -- see
        // `mmap_reserve`, which pushes `(at, len)`. Reading it as an end
        // underflowed by roughly the window base and reported snapshots of
        // 17592186044163 MiB, which is 2^64 in disguise.
        let mmap: u64 = self.mmap_live.iter().map(|(_, len)| *len).sum();
        // The stack grows DOWN from the top, so what is live is everything above
        // the current pointer. A wild sp (a process that has not started, or one
        // whose state was not saved) would make this meaningless, so it is
        // clamped rather than allowed to report a nonsense total.
        let stack = if sp >= MMAP_END_VMA && sp <= STACK_TOP_VMA {
            STACK_TOP_VMA - sp
        } else {
            0
        };
        (image + brk + mmap + stack, image, brk, mmap, stack)
    }

    /// Counts bytes that differ between the live arena and `other`, OUTSIDE the
    /// ranges a bounded snapshot of the incoming process would restore, broken
    /// down by REGION.
    ///
    /// This tests the single assumption the whole bounded-snapshot design rests
    /// on: that everything a snapshot does NOT copy is already identical in
    /// every process, so leaving the departing process's bytes there is
    /// harmless. If that is false, a bounded snapshot is not slower or
    /// wasteful, it is SILENT CORRUPTION.
    ///
    /// The breakdown is by region rather than a single count because the first
    /// version reported only a total and a first address, and that could not
    /// distinguish "a few bytes of thread scratch" from "the entire image of a
    /// different program" -- which turned out to be both happening at once.
    ///
    /// Returns per-region counts AND the first differing address in each. The
    /// count alone says a region is wrong; the ADDRESS says which structure --
    /// and the difference between "1 byte in the image" and "1 byte at
    /// 0x100000, the TLS control block" is the difference between an unexplained
    /// anomaly and a named gap in the range set.
    ///
    /// ⚠️ **Returns `None` when there is NO ORACLE**, which since 2026-08-22 is
    /// the ordinary case. The comparison needs the incoming process's whole
    /// memory to compare the live arena against, and only `SnapshotData::Full`
    /// carries it. That variant used to be every suspended process; with bounded
    /// snapshots unconditional it is only a multi-threaded group, so a
    /// single-threaded switch has nothing to check against.
    ///
    /// The `Option` is the whole point of the signature. This used to return a
    /// zeroed `SnapDiff` in that case, and a caller printing it reported
    /// `miss=0` -- a clean bill of health obtained by comparing against nothing.
    /// Two earlier probes in this tree failed in exactly that shape
    /// (`bbmiss insn=`, `undecoded_message`), and both times the wrong answer
    /// was the reassuring one. `None` cannot be printed as a zero by accident.
    ///
    /// ⚠️ Even a `Some` is now HYPOTHETICAL: an oracle exists only for a
    /// multi-threaded group, and such a group is snapshotted and restored in
    /// FULL, so the range set scored here is not the one the switch used. It
    /// answers "would a bounded snapshot have been safe here", which is worth
    /// knowing and is a weaker claim than what this probe once made. The
    /// differential that validated the scheme -- run the same workload both ways
    /// and diff -- no longer exists at all, because there is no other way to
    /// run. See the stack-below-`sp` caveat in `.agents/docs/TODO.md`, which was
    /// ACCEPTED rather than closed and is now unsettleable by this route.
    ///
    /// O(arena) per call. Diagnostic only, behind RAPTORMARK_ECV_SNAPCHECK.
    pub fn bytes_differing_outside(
        &self,
        other: &ArenaSnapshot,
        ranges: &[(u64, u64)],
    ) -> Option<SnapDiff> {
        let SnapshotData::Full(other_bytes) = &other.data else {
            // Nothing to compare against: the oracle IS the full buffer.
            return None;
        };
        let mut covered = vec![false; MEMORY_ARENA_SIZE];
        for &(start, len) in ranges {
            let s = (start as usize).min(MEMORY_ARENA_SIZE);
            let e = ((start + len) as usize).min(MEMORY_ARENA_SIZE);
            covered[s..e].fill(true);
        }
        let mut out = [0u64; 5];
        let mut first = [u64::MAX; 5];
        for i in 0..MEMORY_ARENA_SIZE {
            if covered[i] || self.bytes[i] == other_bytes[i] {
                continue;
            }
            let a = i as u64;
            let bucket = if a < IMAGE_BASE_VMA {
                0
            } else if a < BRK_START_VMA {
                1
            } else if a < MMAP_START_VMA {
                2
            } else if a < MMAP_END_VMA {
                3
            } else {
                4
            };
            out[bucket] += 1;
            if first[bucket] == u64::MAX {
                first[bucket] = a;
            }
        }
        Some(SnapDiff { counts: out, first })
    }

    /// Reserves `len` bytes of PRIVATE mmap, reusing a reclaimed hole before
    /// growing the bump up to (but not into) `ceiling` -- the shared window's
    /// floor. Returns the VMA, or None if neither can serve it.
    pub fn mmap_reserve(&mut self, len: u64, ceiling: u64) -> Option<u64> {
        let at = if let Some(i) = best_fit(&self.mmap_free, len) {
            let (s, e) = self.mmap_free[i];
            // Carve from the BOTTOM, keeping this window's ascending grain.
            if s + len == e {
                self.mmap_free.remove(i);
            } else {
                self.mmap_free[i].0 = s + len;
            }
            s
        } else {
            let at = self.mmap_cur;
            if at.checked_add(len)? > ceiling {
                return None;
            }
            self.mmap_cur = at + len;
            at
        };
        // ZERO the extent before handing it out. Linux guarantees a fresh
        // MAP_ANONYMOUS reads as zeroes, and under bounded snapshots this is
        // load-bearing rather than cosmetic: the live arena is shared, so
        // whatever a PREVIOUS process left at these addresses would otherwise
        // be visible to this one. Measured before it was written -- mmap
        // regions outside a process's live extents differed from the incoming
        // process's own memory in 39 of 59 switches, by up to 257 KB.
        //
        // Correct under the full-buffer scheme too, where it stops a process
        // seeing its own freed data, which Linux would not show it either.
        let n = self.bytes.len();
        let start = (at as usize).min(n);
        let end = (start + len as usize).min(n);
        self.bytes[start..end].fill(0);
        self.mmap_live.push((at, len));
        Some(at)
    }

    /// Moves the program break to `addr`, zeroing memory the break newly
    /// exposes. Linux hands out zeroed pages on a brk growth, and the reasoning
    /// is the same as `mmap_reserve`'s: under bounded snapshots the bytes above
    /// a process's break belong to whoever ran last (measured: differing in 9 of
    /// 59 switches, by up to 33 KB).
    pub fn set_brk(&mut self, addr: u64) {
        if addr > self.brk_cur {
            let n = self.bytes.len();
            let start = (self.brk_cur as usize).min(n);
            let end = (addr as usize).min(n);
            if start < end {
                self.bytes[start..end].fill(0);
            }
        }
        self.brk_cur = addr;
    }

    /// Returns a private mapping, identified by an EXACT `(start, len)` match
    /// against what was handed out. Returns whether anything was reclaimed.
    pub fn mmap_release(&mut self, start: u64, len: u64) -> bool {
        let Some(i) = self
            .mmap_live
            .iter()
            .position(|&(s, l)| s == start && l == len)
        else {
            return false;
        };
        self.mmap_live.remove(i);
        insert_coalesced(&mut self.mmap_free, start, start + len);
        // A hole that reaches the bump goes back to it, so a run of
        // grow-then-free-the-old leaves the window as it found it rather than
        // as a growing list of corpses.
        while let Some(i) = self.mmap_free.iter().position(|&(_, e)| e == self.mmap_cur) {
            self.mmap_cur = self.mmap_free.remove(i).0;
        }
        true
    }

    /// Copies the shared VMA ranges out of `prev` into the live arena.
    ///
    /// ⚠️ The companion to [`Arena::swap_with`], and like it, **nothing on the
    /// switch path calls this since 2026-08-22**: it exists to repair the shared
    /// window after a buffer TRADE, and there are no trades left.
    /// `restore_in_place`'s `Full` arm solves the same hazard the other way, by
    /// not overwriting the shared window in the first place. Kept for the same
    /// reason `swap_with` is, and with the same caveat -- its tests are its only
    /// callers.
    ///
    /// Shared memory has ONE physical copy, and it belongs to whoever is
    /// running rather than to a process: after a swap the live buffer holds the
    /// incoming process's own stale view of those addresses. Under the previous
    /// copy-based restore this fell out for free -- the shared ranges were
    /// exactly the bytes NOT copied back -- so with a swap it has to be done
    /// explicitly, but only over the shared ranges instead of everything around
    /// them. Overlapping ranges are harmless: copying a byte twice is
    /// idempotent, which is why this needs no sorting.
    /// See `.agents/docs/SHAREDMEM.md`.
    pub fn adopt_shared_from(&mut self, prev: &ArenaSnapshot, shared: &[SharedSeg]) {
        // Only meaningful when buffers are traded: the shared region's single
        // physical copy lives in whichever buffer just went out, so it has to be
        // carried into the incoming one. Under bounded snapshots the live buffer
        // never moves, so the shared bytes are already where they belong.
        let SnapshotData::Full(prev_bytes) = &prev.data else {
            return;
        };
        let n = self.bytes.len();
        for seg in shared {
            let start = ((seg.vma_start - MEMORY_ARENA_VMA) as usize).min(n);
            let end = (start + seg.len).min(n);
            if start < end {
                self.bytes[start..end].copy_from_slice(&prev_bytes[start..end]);
            }
        }
    }

    /// Clears the arena for a fresh program image (execve), keeping the same
    /// (fixed-address) buffer and every still-live shared region.
    ///
    /// `shared` must be the regions that survive this execve -- i.e. the caller
    /// has already dropped the exec'ing process's own mappings. They are skipped
    /// rather than zeroed because they belong to OTHER processes: there is one
    /// physical copy of a shared region, so wiping the arena wholesale here
    /// would silently zero a segment the postmaster is still using while one of
    /// its children execs. `restore_in_place`/`adopt_shared_from` already exempt
    /// these ranges; execve is the third place that rewrites the live arena and
    /// must agree.
    pub fn reset(&mut self, shared: &[SharedSeg]) {
        // Zero the COMPLEMENT of the shared ranges, walking them in address
        // order.
        let n = self.bytes.len();
        let mut at = 0usize;
        for (s, e) in shared_offsets(shared, n) {
            if s > at {
                self.bytes[at..s].fill(0);
            }
            at = at.max(e);
        }
        self.bytes[at..].fill(0);
        self.brk_cur = BRK_START_VMA;
        self.mmap_cur = MMAP_START_VMA;
        // execve replaces the address space: every private mapping is gone.
        self.mmap_free.clear();
        self.mmap_live.clear();
    }

    /// TranslateVMA: identity map, `arena + (vma - MEMORY_ARENA_VMA)`.
    #[inline]
    pub fn translate(&mut self, vma: u64) -> *mut u8 {
        debug_assert!(((vma - MEMORY_ARENA_VMA) as usize) < MEMORY_ARENA_SIZE);
        unsafe {
            self.bytes
                .as_mut_ptr()
                .add((vma - MEMORY_ARENA_VMA) as usize)
        }
    }

    /// True if `[vma, vma+len)` lies inside the arena.
    ///
    /// `slice`/`slice_mut` index a Vec and PANIC otherwise, and on wasm32 a
    /// 64-bit guest pointer truncates to 32 bits on the way in -- so a
    /// nonsensical guest address does not merely read the wrong bytes, it aborts
    /// the module with a message that names the runtime. Any path that takes a
    /// pointer from an unvalidated guest register should check here first.
    pub fn in_bounds(&self, vma: u64, len: usize) -> bool {
        let Some(off) = vma.checked_sub(MEMORY_ARENA_VMA) else {
            return false;
        };
        off.checked_add(len as u64)
            .is_some_and(|end| end <= MEMORY_ARENA_SIZE as u64)
    }

    pub fn slice_mut(&mut self, vma: u64, len: usize) -> &mut [u8] {
        let off = (vma - MEMORY_ARENA_VMA) as usize;
        &mut self.bytes[off..off + len]
    }

    pub fn slice(&self, vma: u64, len: usize) -> &[u8] {
        let off = (vma - MEMORY_ARENA_VMA) as usize;
        &self.bytes[off..off + len]
    }

    /// Reads a NUL-terminated guest string starting at vma (NUL excluded).
    pub fn read_cstr(&self, vma: u64) -> Vec<u8> {
        let start = (vma - MEMORY_ARENA_VMA) as usize;
        let mut end = start;
        while end < self.bytes.len() && self.bytes[end] != 0 {
            end += 1;
        }
        self.bytes[start..end].to_vec()
    }

    /// Reads at most `max` bytes of a NUL-terminated guest string, returning the
    /// bytes before the NUL and whether a NUL was actually found.
    ///
    /// [`read_cstr`](Self::read_cstr) scans to the END OF THE ARENA when there is
    /// no NUL, which is fine on a path that already knows it has a string. This
    /// one exists for `PR_SET_VMA_ANON_NAME`, where the pointer is an
    /// unvalidated guest register and the kernel itself reads at most 80 bytes:
    /// scanning 384 MiB for a name that cannot legally exceed 79 characters is
    /// work a guest should not be able to ask for by passing a bad pointer.
    ///
    /// The caller must have established that `vma` is in the arena; the scan
    /// additionally stops at the arena end, and `false` then means the string
    /// ran off the end rather than being too long.
    pub fn read_cstr_capped(&self, vma: u64, max: usize) -> (Vec<u8>, bool) {
        let start = (vma - MEMORY_ARENA_VMA) as usize;
        let stop = start.saturating_add(max).min(self.bytes.len());
        let mut end = start;
        while end < stop && self.bytes[end] != 0 {
            end += 1;
        }
        (self.bytes[start..end].to_vec(), end < stop)
    }

    fn write_u64(&mut self, vma: u64, v: u64) {
        self.slice_mut(vma, 8).copy_from_slice(&v.to_le_bytes());
    }

    /// Builds the initial guest stack (port of `MemoryArenaInit`): AT_RANDOM
    /// bytes, program headers, end marker, env/argv strings, auxv, envp/argv
    /// pointer arrays, argc. Returns the initial stack pointer. `uid`/`gid`
    /// fill the identity auxv entries.
    pub fn build_stack(
        &mut self,
        prog: &EcvProgram,
        argv: &[Vec<u8>],
        envp: &[Vec<u8>],
        uid: u32,
        gid: u32,
    ) -> u64 {
        let mut sp = STACK_TOP_VMA;

        // AT_RANDOM: upstream WASI fills with 1s.
        sp -= 16;
        self.slice_mut(sp, 16).fill(1);
        let randomp = sp;

        // Program headers for AT_PHDR.
        let (phent, phnum, e_ph) = (prog.e_phent() as u64, prog.e_phnum() as u64, prog.e_ph);
        let e_ph_size = (phent * phnum) as usize;
        sp -= e_ph_size as u64;
        let dst = self.slice_mut(sp, e_ph_size);
        unsafe { core::ptr::copy_nonoverlapping(e_ph, dst.as_mut_ptr(), e_ph_size) };
        let phdr = sp;

        sp -= sp & 0xf;

        // End marker.
        sp -= 8;
        self.write_u64(sp, 0);

        // String contents: env block then argv block, matching upstream order.
        let envp_size: u64 = envp.iter().map(|e| e.len() as u64 + 1).sum();
        let argv_size: u64 = argv.iter().map(|a| a.len() as u64 + 1).sum();
        let env_0_sp = sp - envp_size;
        let argv_0_sp = env_0_sp - argv_size;
        sp -= envp_size + argv_size;

        let mut at = env_0_sp;
        for e in envp {
            self.slice_mut(at, e.len()).copy_from_slice(e);
            self.write_u8(at + e.len() as u64, 0);
            at += e.len() as u64 + 1;
        }
        let mut at = argv_0_sp;
        for a in argv {
            self.slice_mut(at, a.len()).copy_from_slice(a);
            self.write_u8(at + a.len() as u64, 0);
            at += a.len() as u64 + 1;
        }

        sp -= sp & 0xf;

        // auxv.
        //
        // AT_MINSIGSTKSZ (51) is supplied because Linux on aarch64 has supplied
        // it since 5.14 and this table is meant to be what the kernel hands a
        // process.
        //
        // ❌ It does NOT fix python:3-slim, which is what it was added for, and
        // it is kept only as faithful emulation. python died in
        // `sysconf(_SC_MINSIGSTKSZ)` with
        //
        //   Fatal glibc error: sysconf-sigstksz.h:25 (sysconf_sigstksz):
        //   assertion failed: minsigstacksize != 0
        //
        // and adding the entry changed nothing, because a FUSED glibc never
        // consumes this table: `_dl_aux_init` runs only in glibc's static path,
        // and the shared path reads the auxv in `dl_main`, which is ld.so, which
        // eager binding means we never enter. So `_rtld_global_ro` keeps its
        // static initializer, and every GLRO field that exists only to hold an
        // auxv value stays zero. `_dl_pagesize` does have an initializer, which
        // is why sys.rs:349 records the guest seeing EXEC_PAGESIZE=65536 and
        // never our AT_PAGESZ of 4096 -- the same fact from the other side, and
        // the reason nothing had noticed the auxv was inert.
        //
        // What DOES fix python is writing the field directly, the way
        // `.ecv.stacklists` writes the `_dl_stack_*` heads: see
        // `apply_stacklists`' word 9 in context.rs. The value written there is
        // `MINSIGSTKSZ` below, so the two agree by construction.
        //
        // musl does not rescue it either: `__init_libc` drops any tag >= AUX_CNT
        // (38), so it discards 51 as well.
        let auxv: [(u64, u64); 13] = [
            (3, phdr),            // AT_PHDR
            (4, phent),           // AT_PHENT
            (5, phnum),           // AT_PHNUM
            (6, 4096),            // AT_PAGESZ
            (9, prog.entry_pc()), // AT_ENTRY
            (11, uid as u64),     // AT_UID
            (12, uid as u64),     // AT_EUID
            (13, gid as u64),     // AT_GID
            (14, gid as u64),     // AT_EGID
            (23, 0),              // AT_SECURE
            (25, randomp),        // AT_RANDOM
            (51, MINSIGSTKSZ),    // AT_MINSIGSTKSZ
            (0, 0),               // AT_NULL
        ];
        sp -= (auxv.len() * 16) as u64;
        let mut at = sp;
        for (tag, val) in auxv {
            self.write_u64(at, tag);
            self.write_u64(at + 8, val);
            at += 16;
        }

        // envp pointer array (NULL-terminated).
        sp -= 8 * (envp.len() as u64 + 1);
        let mut at = sp;
        let mut s = env_0_sp;
        for e in envp {
            self.write_u64(at, s);
            s += e.len() as u64 + 1;
            at += 8;
        }
        self.write_u64(at, 0);

        // argv pointer array (NULL-terminated).
        sp -= 8 * (argv.len() as u64 + 1);
        let mut at = sp;
        let mut s = argv_0_sp;
        for a in argv {
            self.write_u64(at, s);
            s += a.len() as u64 + 1;
            at += 8;
        }
        self.write_u64(at, 0);

        // argc.
        sp -= 8;
        self.write_u64(sp, argv.len() as u64);

        sp
    }

    fn write_u8(&mut self, vma: u64, v: u8) {
        self.slice_mut(vma, 1)[0] = v;
    }

    /// Zeroes the TCB `[tp, tp+TCB_SIZE)` (aarch64 variant-I: the thread pointer
    /// points at the TCB; its dtv / private words are unused by local-/initial-
    /// exec `__thread` accesses). Call once before laying out the TLS modules.
    pub fn tls_zero_tcb(&mut self, tp: u64) {
        let tcb = (tp - MEMORY_ARENA_VMA) as usize;
        self.bytes[tcb..tcb + TCB_SIZE as usize].fill(0);
    }

    /// Lays out ONE static-TLS module at `tp + tp_offset`: copies its `.tdata`
    /// template (`filesz` bytes from `template_vaddr`) and zeroes its `.tbss`
    /// tail (`memsz - filesz` bytes). `tp_offset` is the full TP-relative base
    /// offset the prelinker assigned to this module (it already includes the TCB
    /// gap), so a `__thread` access at `TP + tprel` — whether baked into the exe
    /// (local-exec) or resolved via a GOT TPREL (initial-exec) — lands inside the
    /// initialized block. See `.agents/docs/DYNLINK.md` for the layout convention
    /// (`internal/prelink/tls.go` assigns the offsets and emits the per-module
    /// `.ecv.tls` descriptor table `setup_tls` drives this from).
    pub fn init_tls_module(
        &mut self,
        template_vaddr: u64,
        filesz: u64,
        memsz: u64,
        tp_offset: u64,
        tp: u64,
    ) {
        let block = tp + tp_offset;
        if filesz > 0 {
            let src = (template_vaddr - MEMORY_ARENA_VMA) as usize;
            let dst = (block - MEMORY_ARENA_VMA) as usize;
            let n = filesz as usize;
            self.bytes.copy_within(src..src + n, dst);
        }
        if memsz > filesz {
            let z = (block + filesz - MEMORY_ARENA_VMA) as usize;
            let zn = (memsz - filesz) as usize;
            self.bytes[z..z + zn].fill(0);
        }
    }

    /// Single-module static-TLS setup: the fallback path when no per-module
    /// `.ecv.tls` descriptor table is present (only one advertised PT_TLS). The
    /// module's offset is `align_up(TCB_SIZE, align)` — the same value the
    /// prelinker assigns the first module. `setup_tls` (context.rs) is the
    /// preferred entry point; it lays out EVERY module.
    pub fn init_static_tls(
        &mut self,
        template_vaddr: u64,
        filesz: u64,
        memsz: u64,
        align: u64,
        tp: u64,
    ) {
        let a = if align < 1 { 1 } else { align };
        let tp_offset = (TCB_SIZE + a - 1) & !(a - 1); // align_up(TCB_SIZE, align)
        self.tls_zero_tcb(tp);
        self.init_tls_module(template_vaddr, filesz, memsz, tp_offset, tp);
    }

    /// Copies the program's ELF data sections into the arena, skipping `.tbss`
    /// like upstream.
    pub fn load_data_sections(&mut self, prog: &EcvProgram) {
        unsafe {
            let n = prog.data_num() as usize;
            for i in 0..n {
                let name = *prog.data_names.add(i);
                if !name.is_null() && core::slice::from_raw_parts(name, 5) == b".tbss" {
                    continue;
                }
                let vma = *prog.data_vmas.add(i);
                let size = *prog.data_sizes.add(i) as usize;
                let src = *prog.data_bytes.add(i);
                let dst = self.slice_mut(vma, size);
                core::ptr::copy_nonoverlapping(src, dst.as_mut_ptr(), size);
            }
        }
    }
}

#[cfg(test)]
mod tests {

    /// ⚠️ MADV_DONTNEED is the one advice that is not advice: Linux guarantees
    /// the range reads as zeroes afterwards, and allocators use it as "free this
    /// without unmapping". Returning success without zeroing hands the guest its
    /// own stale bytes where it is entitled to zeroes.
    #[test]
    fn madvise_dontneed_and_remove_oblige_zeroing() {
        assert!(madvise_zeroes(4), "MADV_DONTNEED");
        assert!(madvise_zeroes(9), "MADV_REMOVE");
    }

    /// Everything else is a hint a kernel may ignore, so a no-op is honest --
    /// including MADV_FREE, where Linux may return the OLD contents until the
    /// pages are reused, so a guest cannot depend on zeroes either way.
    #[test]
    fn ordinary_advice_is_a_no_op() {
        for advice in [0u64, 1, 2, 3, 8, 10, 12, 15] {
            assert!(!madvise_zeroes(advice), "advice {advice} must not zero");
        }
    }

    /// `do_mmap` opens with `if (!len) return -EINVAL`. `NR_MMAP` used to round
    /// 0 up to 0, reserve a zero-byte slot and hand back an ADDRESS -- a
    /// successful mapping of nothing, where Linux refuses.
    ///
    /// If the guard were absent this would read `Ok(0)`, so the assertion is on
    /// the variant and not merely on "it is an Err": an `Ok(0)` is precisely the
    /// old behaviour, and a test that only checked `is_err()` would still pass
    /// against an `Overflow` misclassification.
    #[test]
    fn a_zero_length_mmap_is_einval_not_a_zero_byte_mapping() {
        assert_eq!(super::mmap_round_len(0), Err(super::MmapLenError::Zero));
    }

    /// Linux: `len = PAGE_ALIGN(len); if (!len) return -ENOMEM;` -- "Careful
    /// about overflows..". Unchecked, `len + GUEST_PAGE_MASK` wraps to a value
    /// below the mask, which the mask then clears: every such length rounds to
    /// `Ok(0)`, and the neutralization run printed exactly that. So this is not
    /// a duplicate of the zero-length test -- the zero appears AFTER the
    /// rounding, where a `len == 0` guard has already run.
    ///
    /// Every `len` in the top page wraps; the boundary is the point, so the
    /// first length that wraps and the largest one that does not are both here.
    #[test]
    fn a_length_whose_page_alignment_overflows_is_enomem() {
        // Largest representable length: wraps.
        assert_eq!(
            super::mmap_round_len(u64::MAX),
            Err(super::MmapLenError::Overflow)
        );
        // The exact boundary: one byte past the last alignable length.
        let last_alignable = u64::MAX & !GUEST_PAGE_MASK; // 0xffff_ffff_ffff_0000
        assert_eq!(
            super::mmap_round_len(last_alignable + 1),
            Err(super::MmapLenError::Overflow),
            "the first length that cannot be page-aligned must be ENOMEM"
        );
        // ...and the last one that CAN is still served, so the guard is not
        // simply rejecting everything large.
        assert_eq!(super::mmap_round_len(last_alignable), Ok(last_alignable));
    }

    /// The rounding itself, unchanged by the guards: an ordinary length rounds
    /// UP to the 64 KiB granule and an already-aligned one is left alone. This
    /// is what would catch a "fix" that returned an error for everything.
    #[test]
    fn an_ordinary_length_still_rounds_up_to_the_guest_page() {
        assert_eq!(super::mmap_round_len(1), Ok(0x10000));
        assert_eq!(super::mmap_round_len(4096), Ok(0x10000));
        assert_eq!(super::mmap_round_len(0x10000), Ok(0x10000));
        assert_eq!(super::mmap_round_len(0x10001), Ok(0x20000));
    }

    /// ⚠️ THE CHARACTER RULE IS NOT THE OBVIOUS ONE, and both surprises are
    /// here. Measured on Linux 6.17/aarch64 against
    /// `prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, ...)`: SPACE is ACCEPTED (the
    /// kernel's bound is `ch > 0x1f`, not "no whitespace") and DEL is REFUSED
    /// (`ch < 0x7f`, so the printable range excludes it). A rule written as
    /// `is_ascii_graphic()` gets both backwards.
    ///
    /// The five excluded punctuation characters are excluded because the name is
    /// printed as `[anon:NAME]` into `/proc/<pid>/maps`, so `[` and `]` could
    /// forge a field and `\`, backtick and `$` are shell metacharacters. Each
    /// was measured EINVAL individually; `:` and the rest of ASCII punctuation
    /// were measured accepted, which is what stops this from being a rule that
    /// merely refuses a lot.
    #[test]
    fn an_anon_vma_name_takes_exactly_the_characters_linux_takes() {
        for ch in b'!'..=b'~' {
            let expected = !matches!(ch, b'[' | b']' | b'\\' | b'`' | b'$');
            assert_eq!(
                super::anon_name_char_permitted(ch),
                expected,
                "byte {ch:#04x} ({:?}) disagreed with the kernel's rule",
                ch as char
            );
        }
        assert!(
            super::anon_name_char_permitted(b' '),
            "SPACE was refused; Linux accepts it (measured: \"a b\" returns 0)"
        );
        assert!(
            !super::anon_name_char_permitted(0x7f),
            "DEL was accepted; Linux gives EINVAL (measured)"
        );
        assert!(!super::anon_name_char_permitted(b'\t'));
        assert!(!super::anon_name_char_permitted(0x1f));
        assert!(!super::anon_name_char_permitted(0));
        // ⚠️ `char` is unsigned on aarch64, so the kernel's `ch < 0x7f` rejects
        // the whole high half rather than accepting it as a negative.
        assert!(!super::anon_name_char_permitted(0x80));
        assert!(!super::anon_name_char_permitted(0xff));
    }

    /// The length boundary, measured rather than derived: the kernel's constant
    /// is 80 and it counts the NUL, so 79 characters is the longest name that
    /// works. Both sides of the boundary are here because an off-by-one is the
    /// whole risk -- `ANON_NAME_MAX_LEN` spelled as 80 would accept a name Linux
    /// refuses with EINVAL.
    ///
    /// The empty name is accepted, which is also measured (`[anon:]` appears
    /// verbatim in maps) and is the other plausible wrong guess.
    #[test]
    fn an_anon_vma_name_is_at_most_79_characters() {
        let a = |n: usize| vec![b'a'; n];
        assert!(super::anon_name_permitted(&a(79)), "79 chars was refused");
        assert!(
            !super::anon_name_permitted(&a(80)),
            "80 chars was accepted; Linux gives EINVAL (measured 6.17)"
        );
        assert!(!super::anon_name_permitted(&a(100)));
        assert!(
            super::anon_name_permitted(b""),
            "the empty name was refused"
        );
        // Ruby's actual name, from `heap_page_allocate_and_initialize` in
        // ruby-glibc.fused -- the guest this ruling exists for.
        assert!(super::anon_name_permitted(
            b"Ruby:GC:default:heap_page_body_allocate"
        ));
        // Length alone must not rescue a bad character, nor the reverse.
        assert!(!super::anon_name_permitted(b"a`b"));
    }

    /// The range rules, in Linux's order and with its rounding. Each line was
    /// measured against `madvise_set_anon_name` on 6.17.
    ///
    /// ⚠️ The zero-length case is the one that is easy to get wrong in the
    /// direction of an ERROR: Linux returns 0 for it, and does so BEFORE it
    /// consults any mapping -- a zero length at a wildly unmapped address is
    /// still 0, where the same address with a page-sized length is ENOMEM. An
    /// implementation that checked the address first would answer ENOMEM.
    #[test]
    fn an_anon_vma_name_range_rounds_and_refuses_the_way_linux_does() {
        use super::AnonNameRange::*;
        // A page-aligned page: exactly itself.
        assert_eq!(
            super::anon_name_range(0x1000, 0x1000),
            Range {
                start: 0x1000,
                end: 0x2000
            }
        );
        // ⚠️ The length is rounded UP, not refused: measured, `len` 1 names one
        // whole page and `len` 4097 names two.
        assert_eq!(
            super::anon_name_range(0x1000, 1),
            Range {
                start: 0x1000,
                end: 0x2000
            },
            "a sub-page length was refused or taken literally"
        );
        assert_eq!(
            super::anon_name_range(0x1000, 0x1001),
            Range {
                start: 0x1000,
                end: 0x3000
            }
        );
        // Zero length: success, and no mapping consulted.
        assert_eq!(super::anon_name_range(0x1000, 0), Empty);
        assert_eq!(
            super::anon_name_range(0x100_0000_0000, 0),
            Empty,
            "a zero length at an unmapped address must still be success"
        );
        // Unaligned start, checked before the length.
        assert_eq!(super::anon_name_range(0x1001, 0x1000), Invalid);
        assert_eq!(
            super::anon_name_range(0x1001, 0),
            Invalid,
            "alignment must be checked before the zero-length short-circuit"
        );
        // ⚠️ Aligned against the 4096 we ADVERTISE, not the 64 KiB we allocate
        // in. A guest that used `sysconf(_SC_PAGESIZE)` is entitled to this.
        assert_eq!(
            super::anon_name_range(0x1000, 0x1000),
            Range {
                start: 0x1000,
                end: 0x2000
            },
            "a 4 KiB-aligned address was refused against GUEST_PAGE_MASK"
        );
        // Overflow, both routes: the rounding wraps, and start+len wraps.
        assert_eq!(super::anon_name_range(0x1000, u64::MAX), Invalid);
        let last_alignable = u64::MAX & !super::AUXV_PAGE_MASK;
        assert_eq!(super::anon_name_range(0x1000, last_alignable + 1), Invalid);
        assert_eq!(super::anon_name_range(last_alignable, 0x2000), Invalid);
        // ...and the largest range that does NOT wrap is still served, so the
        // guard is not simply refusing everything large.
        assert_eq!(
            super::anon_name_range(0, last_alignable),
            Range {
                start: 0,
                end: last_alignable
            }
        );
    }

    /// A range of exactly 4 GiB is outside a 384 MiB arena, and so is one byte
    /// past the end. Why 4 GiB specifically: `Arena::in_bounds` takes its length
    /// as a `usize`, which is 32 bits on wasm32, so a length that is a multiple
    /// of 4 GiB narrows to zero there and passes a bounds check it should fail.
    /// `range_in_arena` takes both ends as `u64` so nothing can narrow.
    ///
    /// ⚠️ WHAT THIS TEST CANNOT SEE, stated because the neutralization run said
    /// so rather than because it was predicted: `usize` is 64 BITS ON THE HOST,
    /// so re-writing `range_in_arena` in terms of `usize` does NOT make this test
    /// fail -- it was tried, and it passed. What the test does observe is that
    /// the WIDTH of the length is honoured: neutralized with `as u32 as u64` on
    /// the length it fails with the 4 GiB assertion below. The wasm32-specific
    /// half is carried by the signature and by this comment, exactly as it is for
    /// `context::dumpable_arg_permitted`, whose 64-bit test has the same limit.
    #[test]
    fn a_range_four_gib_long_is_not_inside_a_384_mib_arena() {
        let arena_end = MEMORY_ARENA_VMA + MEMORY_ARENA_SIZE as u64;
        assert!(super::range_in_arena(MEMORY_ARENA_VMA, arena_end));
        assert!(super::range_in_arena(0x1000, 0x2000));
        assert!(
            !super::range_in_arena(MEMORY_ARENA_VMA, MEMORY_ARENA_VMA + 0x1_0000_0000),
            "a 4 GiB range narrowed to zero length and passed"
        );
        assert!(
            !super::range_in_arena(MEMORY_ARENA_VMA, arena_end + 1),
            "one byte past the arena was accepted"
        );
        assert!(!super::range_in_arena(0x100_0000_0000, 0x100_0000_1000));
    }

    /// `read_cstr_capped` is what keeps an unvalidated guest pointer from
    /// costing a 384 MiB scan, and its `terminated` flag is what lets the
    /// caller tell Linux's EINVAL (too long) from its EFAULT (ran off the end).
    ///
    /// ⚠️ The boundary case is a name of exactly `max - 1` bytes: its NUL is the
    /// last byte read, so it must count as TERMINATED. An off-by-one here turns
    /// every 79-character name -- the longest legal one -- into EINVAL.
    #[test]
    fn a_capped_cstr_read_stops_at_the_cap_and_says_so() {
        let mut a = Arena::new();
        let at = 0x1000;
        a.slice_mut(at, 200).fill(b'a');
        a.slice_mut(at + 79, 1)[0] = 0;
        let (s, term) = a.read_cstr_capped(at, 80);
        assert!(
            term,
            "a 79-byte name whose NUL is the last byte read was not terminated"
        );
        assert_eq!(s.len(), 79);

        // 80 bytes with no NUL inside the window: not terminated, which is what
        // the caller turns into EINVAL.
        a.slice_mut(at + 79, 1)[0] = b'a';
        a.slice_mut(at + 120, 1)[0] = 0;
        let (s, term) = a.read_cstr_capped(at, 80);
        assert!(
            !term,
            "a name longer than the cap reported itself terminated"
        );
        assert_eq!(s.len(), 80, "the read did not stop at the cap");

        // The scan also stops at the arena end rather than indexing past it.
        let near_end = MEMORY_ARENA_VMA + MEMORY_ARENA_SIZE as u64 - 4;
        a.slice_mut(near_end, 4).fill(b'z');
        let (s, term) = a.read_cstr_capped(near_end, 80);
        assert!(!term);
        assert_eq!(s, b"zzzz", "the read ran past the end of the arena");
    }

    use super::*;

    const MB: u64 = 1024 * 1024;

    /// Walks the initial stack the way a libc's startup does -- argc, the argv
    /// array to its NULL, the envp array to its NULL, then the auxv -- and
    /// returns the auxv as tag/value pairs.
    ///
    /// It parses rather than searches deliberately. Scanning the stack for the
    /// tag would find a match in AT_RANDOM's bytes or in an environment string
    /// and pass while the auxv itself was malformed, which is the failure this
    /// is supposed to catch.
    fn auxv_of(arena: &Arena, sp: u64) -> Vec<(u64, u64)> {
        let word = |a: u64| u64::from_le_bytes(arena.slice(a, 8).try_into().unwrap());
        let argc = word(sp);
        let mut at = sp + 8 + argc * 8;
        assert_eq!(word(at), 0, "argv array must be NULL-terminated");
        at += 8;
        while word(at) != 0 {
            at += 8; // envp
        }
        at += 8;
        let mut out = Vec::new();
        loop {
            let (tag, val) = (word(at), word(at + 8));
            at += 16;
            if tag == 0 {
                return out;
            }
            out.push((tag, val));
            assert!(out.len() < 64, "auxv is not NULL-terminated");
        }
    }

    /// A program descriptor with just enough of a phdr table for `build_stack`,
    /// which dereferences `e_phent_p`/`e_phnum_p`/`entry_pc_p` unconditionally.
    fn program_with_phdrs() -> EcvProgram {
        EcvProgram::for_test_with_phdrs(
            b"stack_test\0",
            56,
            1,
            0x400000,
            Box::leak(Box::new([0u8; 56])),
        )
    }

    /// AT_MINSIGSTKSZ must be present and non-zero.
    ///
    /// This asserts the AUXV CONTRACT, not a guest outcome: Linux on aarch64
    /// supplies tag 51, so this table does too. Do not read it as a guard for
    /// python:3-slim's `sysconf(_SC_MINSIGSTKSZ)` abort -- a fused glibc never
    /// reads this table at all, and supplying the entry did not move that
    /// failure by an instruction. See the note beside the array in
    /// `build_stack`.
    #[test]
    fn the_initial_stack_carries_at_minsigstksz() {
        const AT_MINSIGSTKSZ: u64 = 51;
        let prog = program_with_phdrs();
        let mut arena = Arena::new();
        let sp = arena.build_stack(
            &prog,
            &[b"/usr/bin/prog".to_vec()],
            &[b"PATH=/bin".to_vec()],
            0,
            0,
        );
        let auxv = auxv_of(&arena, sp);
        let got = auxv.iter().find(|(t, _)| *t == AT_MINSIGSTKSZ);
        match got {
            None => panic!(
                "no AT_MINSIGSTKSZ in the auxv; glibc's sysconf(_SC_MINSIGSTKSZ) \
                 asserts minsigstacksize != 0 and aborts. auxv tags present: {:?}",
                auxv.iter().map(|(t, _)| *t).collect::<Vec<_>>()
            ),
            Some(&(_, v)) => assert!(v != 0, "AT_MINSIGSTKSZ must not be zero"),
        }
        // The entries that were already load-bearing, so a future edit to the
        // array cannot drop one while adding another.
        for tag in [3u64, 4, 5, 6, 9, 25] {
            assert!(
                auxv.iter().any(|(t, _)| *t == tag),
                "auxv lost tag {tag}; present: {:?}",
                auxv.iter().map(|(t, _)| *t).collect::<Vec<_>>()
            );
        }
    }

    /// The layout constants must stay mutually consistent, and an arithmetic
    /// slip in one of them is silent: a stack top past the end of the buffer,
    /// or an mmap window overlapping brk, shows up as corruption in an
    /// unrelated guest rather than as a failure here.
    #[test]
    fn the_arena_layout_is_consistent() {
        assert!(THREAD_PTR < BRK_START_VMA, "TLS must sit in the low region");
        assert!(BRK_START_VMA < BRK_END_VMA, "brk region must be non-empty");
        assert_eq!(
            BRK_END_VMA, MMAP_START_VMA,
            "the mmap window must start where brk ends, with no gap to lose"
        );
        assert!(
            MMAP_START_VMA < MMAP_END_VMA,
            "mmap window must be non-empty"
        );
        assert!(
            MMAP_END_VMA < STACK_TOP_VMA,
            "the guest stack grows DOWN from STACK_TOP_VMA and must not start \
             inside the mmap window"
        );
        assert_eq!(
            STACK_TOP_VMA, MEMORY_ARENA_SIZE as u64,
            "the stack top is the end of the buffer; a stack top beyond it \
             writes outside the arena on the very first push"
        );
        // The ifunc scratch is carved from the top of the mmap window and used
        // before the guest runs, so it must be inside it -- these are derived,
        // but a change to MMAP_END_VMA is exactly when a derivation breaks.
        assert!(MMAP_START_VMA < IFUNC_STACK_TOP && IFUNC_STACK_TOP < MMAP_END_VMA);
        assert!(IFUNC_STACK_TOP < IFUNC_ARG_VMA && IFUNC_ARG_VMA < MMAP_END_VMA);
    }

    /// The window has to fit postgres's one-shot allocation BESIDE its shared
    /// memory -- and the second of those is not a constant, which is the whole
    /// trap here.
    ///
    /// A first version of this test hardcoded `SHARED_BUFFERS = 77 MiB`, the
    /// figure measured on the 384 MiB arena. Growing the window to 224 MiB
    /// immediately falsified it: initdb's `test_config_settings` probes
    /// shared_buffers DOWNWARD from a large start and keeps the first size the
    /// arena grants, so it rose to 144 MiB and swallowed 68 of the 128 MiB
    /// added. The guest sizes itself to the arena; the window alone therefore
    /// proves nothing, and the invariant has to be stated against a CAP the
    /// fixture imposes (`initdb -c shared_buffers=...`).
    #[test]
    fn the_mmap_window_fits_the_postgres_peak_under_a_shared_buffers_cap() {
        // CORRECTION (2026-08-12). This test used to require room for a 96 MiB
        // ONE_SHOT_REQUEST, which is why the arena was 512 MiB. That request was
        // not a workload: the by-element `M` bit was decoded wrong, ICU's hash
        // table saw a zero high-water mark, and it rehashed up its PRIMES list
        // (patches/0045). With the lifter fixed the ladder does not occur, and
        // requiring room for it cost 128 MiB in EVERY process on a budget that
        // could not afford it -- see the ceiling assertion below, which is the
        // constraint that actually failed in practice.
        const SHARED_BUFFERS_CAP: u64 = 24 * MB;
        // Measured: private mappings held by a backend at its peak.
        const PRIVATE_AT_PEAK: u64 = 49 * MB;
        let window = MMAP_END_VMA - MMAP_START_VMA;
        assert!(
            window >= SHARED_BUFFERS_CAP + PRIVATE_AT_PEAK,
            "mmap window is {} MiB; postgres needs {} MiB with shared_buffers capped",
            window / MB,
            (SHARED_BUFFERS_CAP + PRIVATE_AT_PEAK) / MB
        );
    }

    #[test]
    fn bounded_snapshot_bytes_sums_the_ranges_it_claims_to() {
        // Written after the probe reported snapshots of 17592186044163 MiB on a
        // real run. `mmap_live` holds (start, LENGTH) and the sum read it as
        // (start, end), so every live mapping subtracted the window base: a u64
        // underflow, printed as a plausible-looking huge number rather than a
        // crash. A test with KNOWN inputs would have caught it before the run.
        let mut a = Arena::new();
        // Two private mappings, 1 MiB and 3 MiB.
        let m1 = a.mmap_reserve(1 << 20, MMAP_END_VMA).unwrap();
        let m2 = a.mmap_reserve(3 << 20, MMAP_END_VMA).unwrap();
        assert!(m1 >= MMAP_START_VMA && m2 > m1);
        a.brk_cur = BRK_START_VMA + (5 << 20);
        let writable = [(0x0020_0000u64, 2u64 << 20)];
        let sp = STACK_TOP_VMA - (4 << 20);

        // 4 KiB of scratch below the thread pointer + the 16-byte TCB + 1 KiB
        // of static TLS, all of which a snapshot must carry.
        let tls_memsz = 1024u64;
        let tls_span = 0x1000 + TCB_SIZE + tls_memsz;
        let (tot, img, brk, mm, stk) = a.bounded_snapshot_bytes(&writable, sp, tls_memsz);
        assert_eq!(
            img,
            (2 << 20) + tls_span,
            "one 2 MiB writable segment plus the TLS area, which no PT_LOAD covers"
        );
        assert_eq!(brk, 5 << 20, "brk grew 5 MiB above its base");
        assert_eq!(mm, 4 << 20, "1 MiB + 3 MiB of live mappings");
        assert_eq!(stk, 4 << 20, "the stack is 4 MiB below the top");
        assert_eq!(tot, (15 << 20) + tls_span);
        assert!(
            tot < MEMORY_ARENA_SIZE as u64,
            "a bounded snapshot that exceeds the arena is arithmetic gone wrong, \
             which is exactly how the underflow presented"
        );
    }

    #[test]
    fn a_bounded_snapshot_round_trips_the_bytes_it_covers() {
        let mut a = Arena::new();
        let m = a.mmap_reserve(4096, MMAP_END_VMA).unwrap();
        a.set_brk(BRK_START_VMA + 4096);
        a.slice_mut(m, 4).copy_from_slice(b"MMAP");
        a.slice_mut(BRK_START_VMA, 4).copy_from_slice(b"BRKX");
        a.slice_mut(THREAD_PTR + 8, 4).copy_from_slice(b"TLSX");

        let ranges = bounded_ranges(a.brk_cur, &a.mmap_live.clone(), &[], 0, 64);
        let snap = a.snapshot_bounded(&ranges, 0);

        // Another process runs and scribbles over all of it.
        a.slice_mut(m, 4).copy_from_slice(b"junk");
        a.slice_mut(BRK_START_VMA, 4).copy_from_slice(b"junk");
        a.slice_mut(THREAD_PTR + 8, 4).copy_from_slice(b"junk");

        a.restore_in_place(&snap, &[]);
        assert_eq!(&a.slice(m, 4), b"MMAP", "a live mapping must come back");
        assert_eq!(
            &a.slice(BRK_START_VMA, 4),
            b"BRKX",
            "the heap must come back"
        );
        assert_eq!(&a.slice(THREAD_PTR + 8, 4), b"TLSX", "TLS must come back");
        assert_eq!(
            a.brk_cur,
            BRK_START_VMA + 4096,
            "and the bookkeeping with it"
        );
    }

    /// THE REGRESSION. `restore_in_place` is the restore half of the bounded
    /// scheme, but the snapshot it is handed is not always `Bounded`:
    /// `snapshot_for` returns a `Full` one for a multi-threaded group, because a
    /// range set derived from ONE stack pointer says nothing about a sibling's
    /// stack. While this function was an `if let SnapshotData::Bounded(..)` that
    /// case restored NO BYTES AT ALL and adopted the snapshot's brk/mmap
    /// bookkeeping anyway, so a process forked from a thread resumed on the
    /// previously-scheduled process's memory. It died in the guest with
    /// `vma 0x0 not in the lifted function table (__remill_function_call)` --
    /// a call through a function pointer that belonged to nobody.
    ///
    /// Every address checked here is deliberately scribbled with DIFFERENT bytes
    /// after the snapshot, so no assertion can pass on what the live arena
    /// happened to hold already. `far` is the one that separates "restored the
    /// whole buffer" from "restored some ranges": it is in the mmap window with
    /// no live mapping over it, so `bounded_ranges` would not cover it and a
    /// bounded snapshot could not bring it back.
    #[test]
    fn a_full_snapshot_restored_in_place_reproduces_its_bytes() {
        let mut a = Arena::new();
        let m = a.mmap_reserve(4096, MMAP_END_VMA).unwrap();
        a.set_brk(BRK_START_VMA + 4096);
        let far = MMAP_START_VMA + (32 << 20);
        let stack = STACK_TOP_VMA - 4096;
        a.slice_mut(m, 4).copy_from_slice(b"MMAP");
        a.slice_mut(BRK_START_VMA, 4).copy_from_slice(b"BRKX");
        a.slice_mut(THREAD_PTR + 8, 4).copy_from_slice(b"TLSX");
        a.slice_mut(stack, 4).copy_from_slice(b"STAK");
        a.slice_mut(far, 4).copy_from_slice(b"FARX");

        // What `snapshot_for` produces for a multi-threaded group, in the mode
        // where the restore path is `restore_in_place`.
        let snap = a.snapshot(0);

        // Another process runs: it scribbles over every one of those, and moves
        // the allocator bookkeeping with it.
        a.slice_mut(m, 4).copy_from_slice(b"junk");
        a.slice_mut(BRK_START_VMA, 4).copy_from_slice(b"junk");
        a.slice_mut(THREAD_PTR + 8, 4).copy_from_slice(b"junk");
        a.slice_mut(stack, 4).copy_from_slice(b"junk");
        a.slice_mut(far, 4).copy_from_slice(b"junk");
        a.set_brk(BRK_START_VMA + (8 << 20));
        let other = a.mmap_reserve(1 << 20, MMAP_END_VMA).unwrap();
        assert_ne!(other, m, "the interloper must own a mapping of its own");

        a.restore_in_place(&snap, &[]);

        assert_eq!(&a.slice(m, 4), b"MMAP", "a live mapping must come back");
        assert_eq!(
            &a.slice(BRK_START_VMA, 4),
            b"BRKX",
            "the heap must come back"
        );
        assert_eq!(&a.slice(THREAD_PTR + 8, 4), b"TLSX", "TLS must come back");
        assert_eq!(&a.slice(stack, 4), b"STAK", "the stack must come back");
        assert_eq!(
            &a.slice(far, 4),
            b"FARX",
            "a full snapshot restores the WHOLE buffer -- this address is in no \
             bounded range, so only a full copy can reproduce it"
        );
        assert_eq!(
            a.brk_cur,
            BRK_START_VMA + 4096,
            "the bookkeeping must come back too -- adopting it WITHOUT the bytes \
             is exactly the bug"
        );
        assert_eq!(a.mmap_live.len(), 1, "the interloper's mapping is not ours");
        assert_eq!(a.mmap_live[0].0, m);
    }

    /// The hazard a whole-buffer copy introduces, and the reason the `Full` arm
    /// walks the complement of the shared window. Shared memory has ONE physical
    /// copy and it belongs to whoever is running, so the incoming process's
    /// saved buffer holds a STALE view of it -- the same thing
    /// `adopt_shared_from` repairs on the swapping path, except that here the
    /// correct bytes are already live and the fix is to not overwrite them.
    #[test]
    fn a_full_restore_in_place_leaves_the_shared_window_alone() {
        let mut a = Arena::new();
        let shared_at = MMAP_END_VMA - (16 << 20);
        let private_at = shared_at - 4096;

        // The snapshot was taken when this process last saw the segment.
        a.slice_mut(shared_at, 4096).fill(0x11);
        a.slice_mut(private_at, 4).copy_from_slice(b"MINE");
        let snap = a.snapshot(0);

        // Meanwhile another process wrote through the shared mapping, and
        // scribbled on the private byte too.
        a.slice_mut(shared_at, 4096).fill(0xAB);
        a.slice_mut(private_at, 4).copy_from_slice(b"junk");

        a.restore_in_place(
            &snap,
            &[SharedSeg {
                vma_start: shared_at,
                len: 4096,
                kind: SharedKind::Anon,
                mappers: vec![7],
            }],
        );

        assert!(
            a.slice(shared_at, 4096).iter().all(|&b| b == 0xAB),
            "the live shared window is the one physical copy; a full restore \
             must not revert it to this process's stale view"
        );
        assert_eq!(
            &a.slice(private_at, 4),
            b"MINE",
            "and everything around it must still be restored, or the exemption \
             has swallowed the copy"
        );
    }

    /// A shared segment abutting the end of the arena must not make the `Full`
    /// arm skip the tail copy or run off the end -- the high-water mark reaches
    /// `end` and the trailing copy has to notice.
    #[test]
    fn a_full_restore_in_place_handles_shared_at_the_arena_edge() {
        let mut a = Arena::new();
        let at = MEMORY_ARENA_SIZE as u64 - 4096;
        a.slice_mut(at, 4096).fill(0x22);
        a.slice_mut(0, 4).copy_from_slice(b"LOWX");
        let snap = a.snapshot(0);
        a.slice_mut(at, 4096).fill(0xCD);
        a.slice_mut(0, 4).copy_from_slice(b"junk");

        a.restore_in_place(
            &snap,
            &[SharedSeg {
                vma_start: at,
                len: 4096,
                kind: SharedKind::Anon,
                mappers: vec![3],
            }],
        );

        assert!(a.slice(at, 4096).iter().all(|&b| b == 0xCD));
        assert_eq!(&a.slice(0, 4), b"LOWX");
    }

    #[test]
    /// The guard that turns a nonsense guest pointer into a refusal instead of
    /// a host panic. The truncating case is the one that matters: on wasm32 a
    /// pointer's low 32 bits are the offset, so a 64-bit -0xa8 becomes an
    /// in-range-looking 0xffffff58 the moment it is cast -- the check has to
    /// happen in 64-bit arithmetic, before any cast.
    #[test]
    fn in_bounds_refuses_what_would_panic_a_slice() {
        let a = Arena::new();
        assert!(a.in_bounds(MEMORY_ARENA_VMA, 4));
        assert!(a.in_bounds(MEMORY_ARENA_VMA + MEMORY_ARENA_SIZE as u64 - 4, 4));
        assert!(
            !a.in_bounds(MEMORY_ARENA_VMA + MEMORY_ARENA_SIZE as u64 - 3, 4),
            "a slice running off the end must be refused"
        );
        // musl's uninitialised __copy_tls produced exactly this: &new->tid with
        // new == -sizeof(struct pthread).
        assert!(
            !a.in_bounds(0xffff_ffff_ffff_ff58, 4),
            "a negative guest pointer was accepted; as usize it truncates to \
             0xffffff58 and the slice index panics the module"
        );
        assert!(
            !a.in_bounds(u64::MAX, 4),
            "the length addition must not wrap"
        );
    }

    fn a_bounded_snapshot_is_a_fraction_of_the_arena() {
        // The entire point. If this ever approaches MEMORY_ARENA_SIZE the
        // scheme has stopped paying for itself and the range set has grown
        // something it should not have.
        let mut a = Arena::new();
        a.set_brk(BRK_START_VMA + (4 << 20));
        a.mmap_reserve(1 << 20, MMAP_END_VMA).unwrap();
        let ranges = bounded_ranges(
            a.brk_cur,
            &a.mmap_live.clone(),
            &[(0x0040_0000, 2 << 20)],
            STACK_TOP_VMA - 4096,
            256,
        );
        let snap = a.snapshot_bounded(&ranges, 0);
        let len = Arena::snapshot_len(&snap);
        assert!(
            len < MEMORY_ARENA_SIZE / 8,
            "bounded snapshot is {} MiB of a {} MiB arena",
            len / (1024 * 1024),
            MEMORY_ARENA_SIZE / (1024 * 1024)
        );
    }

    #[test]
    fn mmap_hands_out_zeroed_memory_even_after_reuse() {
        // Under bounded snapshots the arena is SHARED, so a fresh mapping that
        // returned the previous occupant's bytes would leak data between guest
        // processes -- and Linux guarantees zeroes here regardless.
        let mut a = Arena::new();
        let first = a.mmap_reserve(8192, MMAP_END_VMA).unwrap();
        a.slice_mut(first, 8).copy_from_slice(b"SECRETS!");
        a.mmap_release(first, 8192);
        let again = a.mmap_reserve(8192, MMAP_END_VMA).unwrap();
        assert_eq!(
            again, first,
            "the hole should be reused, or this proves nothing"
        );
        assert_eq!(
            &a.slice(again, 8),
            &[0u8; 8],
            "reused memory must read as zeroes"
        );
    }

    #[test]
    fn growing_the_break_exposes_zeroes() {
        let mut a = Arena::new();
        a.set_brk(BRK_START_VMA + 4096);
        a.slice_mut(BRK_START_VMA + 100, 4).copy_from_slice(b"old!");
        // Shrink and grow again: the bytes above the old break must be zero.
        a.set_brk(BRK_START_VMA);
        a.set_brk(BRK_START_VMA + 4096);
        assert_eq!(&a.slice(BRK_START_VMA + 100, 4), &[0u8; 4]);
    }

    #[test]
    fn the_range_set_covers_the_tls_area_on_both_sides_of_the_image_base() {
        // Measured with RAPTORMARK_ECV_SNAPCHECK: per-process state lives at
        // 0xff9d8 (pthread/dl bring-up, BELOW the image base) and at
        // THREAD_PTR+0x40 (the static TLS block, which starts AT the image
        // base because THREAD_PTR == the image base). Neither is in a PT_LOAD,
        // so a range set derived from program headers alone misses both and a
        // bounded snapshot silently corrupts thread-local state.
        let a = Arena::new();
        let ranges = bounded_ranges(BRK_START_VMA, &[], &[], 0, 4096);
        let covers = |addr: u64| ranges.iter().any(|&(s, l)| addr >= s && addr < s + l);
        assert!(covers(0xff9d8), "the pthread/dl scratch below THREAD_PTR");
        assert!(covers(THREAD_PTR), "the TCB");
        assert!(
            covers(THREAD_PTR + 0x40),
            "the byte the check actually caught"
        );
        assert!(
            covers(THREAD_PTR + TCB_SIZE + 4095),
            "the last byte of the static TLS block"
        );
        assert!(
            !covers(THREAD_PTR + TCB_SIZE + 4096 + 64),
            "and not the image data beyond it, which is shared and must not be copied"
        );
        let _ = a;
    }

    #[test]
    fn the_ranges_and_the_sum_describe_the_same_bytes() {
        // The probe reports a SUM and an implementation would copy RANGES. If
        // those ever disagree the measurement stops describing the thing being
        // decided on, and nothing else would notice.
        let mut a = Arena::new();
        a.mmap_reserve(1 << 20, MMAP_END_VMA).unwrap();
        a.mmap_reserve(3 << 20, MMAP_END_VMA).unwrap();
        a.brk_cur = BRK_START_VMA + (5 << 20);
        let writable = [(0x0020_0000u64, 2u64 << 20)];
        let sp = STACK_TOP_VMA - (4 << 20);

        let (tot, ..) = a.bounded_snapshot_bytes(&writable, sp, 512);
        let ranges = bounded_ranges(a.brk_cur, &a.mmap_live, &writable, sp, 512);
        let from_ranges: u64 = ranges.iter().map(|(_, len)| *len).sum();
        assert_eq!(tot, from_ranges, "the sum and the ranges must agree");
    }

    #[test]
    fn bounded_snapshot_ignores_a_stack_pointer_outside_the_stack() {
        // A process that has not started yet has sp == 0. Reporting
        // STACK_TOP - 0 would claim the whole arena is live and make the
        // measurement useless in precisely the case it is read most.
        let a = Arena::new();
        let (_, _, _, _, stk) = a.bounded_snapshot_bytes(&[], 0, 0);
        assert_eq!(stk, 0, "an unstarted process contributes no stack");
        let (_, _, _, _, stk2) = a.bounded_snapshot_bytes(&[], STACK_TOP_VMA + 4096, 0);
        assert_eq!(
            stk2, 0,
            "a stack pointer above the top is not negative space"
        );
    }

    #[test]
    fn seven_concurrent_arenas_fit_under_the_wasm32_ceiling() {
        // This is the constraint that actually broke, and the number is measured,
        // not chosen: a postgres run reached `linear memory 4010 MiB` and then
        // failed the NEXT 512 MiB request -- the arena for the backend serving
        // its first client.
        //
        // Seven is what one guest-side client costs concurrently: dash,
        // postmaster, checkpointer, background writer, walwriter, the backend,
        // and psql. Every one of them holds a full-size buffer, because
        // `swap_with` trades buffers rather than copying contents.
        const CONCURRENT: usize = 7;
        const WASM32_LINEAR_MEMORY_MAX: usize = 4 * 1024 * MB as usize;
        // Not the whole budget: the module's own data, the guest images and the
        // allocator's fragmentation share it. The measured run had 4010 MiB in
        // use at a point where far fewer than seven buffers were live, so leave
        // real room rather than asserting the arithmetic bound.
        const HEADROOM: usize = 1024 * MB as usize;
        assert!(
            MEMORY_ARENA_SIZE * CONCURRENT + HEADROOM <= WASM32_LINEAR_MEMORY_MAX,
            "{} processes x {} MiB leaves no room under wasm32's 4 GiB",
            CONCURRENT,
            MEMORY_ARENA_SIZE / MB as usize
        );
    }

    /// wasm32 has 4 GiB of linear memory and every SUSPENDED process owns a
    /// full-size buffer, so the arena size sets a hard process ceiling. This
    /// states it out loud: growing the arena spends fork headroom, and the
    /// number below is what an fork-heavy guest has to live within.
    #[test]
    fn the_arena_leaves_room_for_the_processes_we_need() {
        const WASM32_LINEAR_MEMORY: u64 = 4 * 1024 * MB;
        // nginx runs one master plus four workers; initdb peaked at four live
        // buffers. Six is the smallest number that covers both with a margin.
        const BUFFERS_NEEDED: u64 = 6;
        let per_buffer = MEMORY_ARENA_SIZE as u64;
        assert!(
            per_buffer * BUFFERS_NEEDED < WASM32_LINEAR_MEMORY,
            "{} MiB x {} buffers exceeds wasm32's 4 GiB",
            per_buffer / MB,
            BUFFERS_NEEDED
        );
    }
    /// A floor low enough that the window itself is the only limit, except in
    /// the tests that deliberately test the floor.
    const FLOOR: u64 = MMAP_START_VMA;

    fn win() -> ShmWindow {
        ShmWindow::new()
    }

    /// Every entry is non-empty and no two touch or overlap. Checked after each
    /// mutation in the tests below, because a violation is not otherwise
    /// visible: the allocator keeps working, it just stops coalescing, and the
    /// window fragments away over a run too long to reproduce on demand.
    fn assert_invariant(w: &ShmWindow) {
        for (i, &(s, e)) in w.free.iter().enumerate() {
            assert!(s < e, "empty/inverted hole {s:#x}..{e:#x}");
            assert!(s >= w.top, "hole {s:#x} below the bump {:#x}", w.top);
            if i + 1 < w.free.len() {
                assert!(
                    e < w.free[i + 1].0,
                    "holes {:#x}..{:#x} and {:#x}..{:#x} touch",
                    s,
                    e,
                    w.free[i + 1].0,
                    w.free[i + 1].1
                );
            }
        }
    }

    /// THE regression. `initdb` runs `postgres --boot` several times in
    /// sequence, each mapping its own shared_buffers; before reclamation the
    /// third run got ENOMEM with the first two long dead.
    ///
    /// The assertion that matters is the last one: exhaustion has to be
    /// REACHABLE, or "all three succeeded" would prove nothing about
    /// reclamation -- a window big enough for all three would pass identically
    /// with `release` deleted. So the same three requests are replayed without
    /// releasing and must fail.
    #[test]
    fn sequential_processes_reuse_the_window() {
        // The sizes are the ones the failing initdb actually asked for. The
        // floor stands in for a guest that has already taken 32 MiB privately,
        // which is what made the real window 64 MiB rather than the nominal 96.
        let sizes = [16 * MB, 39 * MB, 32 * MB];
        let floor = MMAP_END_VMA - 64 * MB;

        let mut w = win();
        for &n in &sizes {
            let at = w.reserve(n, floor).expect("reserve after the previous run");
            w.release(at, at + n);
            assert_invariant(&w);
            assert_eq!(w.top, MMAP_END_VMA, "window not fully returned");
            assert!(w.free.is_empty(), "leftover holes: {:?}", w.free);
        }

        // Neutralization, in the test itself: the same three requests without a
        // release must FAIL. Without this, "all three succeeded" above would
        // pass identically with `release` deleted -- it would only be measuring
        // that the window is big enough.
        let mut leaky = win();
        let outcomes: Vec<_> = sizes.iter().map(|&n| leaky.reserve(n, floor)).collect();
        assert!(outcomes[0].is_some() && outcomes[1].is_some());
        assert!(
            outcomes[2].is_none(),
            "three un-reclaimed runs fit in the window, so the loop above proves \
             nothing about reclamation"
        );
    }

    /// A live region keeps its address across another region's whole lifetime,
    /// and freeing the lower one first still ends with the window intact.
    #[test]
    fn out_of_order_release_still_reaches_the_bump() {
        let mut w = win();
        let a = w.reserve(8 * MB, FLOOR).unwrap(); // upper
        let b = w.reserve(4 * MB, FLOOR).unwrap(); // below a
        assert!(b < a, "the window must grow downward");

        // Free the UPPER one first: it is not adjacent to the bump, so it can
        // only be recovered once `b` merges with it.
        w.release(a, a + 8 * MB);
        assert_invariant(&w);
        assert_eq!(
            w.top, b,
            "freeing a non-adjacent region must not move the bump"
        );
        assert_eq!(w.free.len(), 1);

        w.release(b, b + 4 * MB);
        assert_invariant(&w);
        assert_eq!(
            w.top, MMAP_END_VMA,
            "the coalesced pair did not return to the bump"
        );
        assert!(w.free.is_empty());
    }

    /// Best fit: a small request must not consume the one hole a large request
    /// needs.
    ///
    /// The address ORDER is the point here and is easy to get wrong. `free` is
    /// sorted ascending, so for first fit and best fit to disagree the LARGER
    /// hole has to be the lower-addressed one -- which is why the small region
    /// is reserved first, the window descending. With the holes the other way
    /// round both policies pick the same entry and the test measures nothing;
    /// written that way first, it passed with best fit replaced by `.next()`.
    #[test]
    fn reserve_picks_the_smallest_sufficient_hole() {
        let mut w = win();
        let small = w.reserve(4 * MB, FLOOR).unwrap(); // highest
        let keep = w.reserve(1 * MB, FLOOR).unwrap(); // stays live, keeps the holes apart
        let big = w.reserve(16 * MB, FLOOR).unwrap(); // below `small`
        let pin = w.reserve(1 * MB, FLOOR).unwrap(); // keeps `big` off the bump
        assert!(big < small);
        w.release(small, small + 4 * MB);
        w.release(big, big + 16 * MB);
        assert_invariant(&w);
        assert_eq!(
            w.free.len(),
            2,
            "the two holes must stay distinct: {:?}",
            w.free
        );
        let top_before = w.top;

        let a = w.reserve(3 * MB, FLOOR).unwrap();
        assert!(
            a >= small && a < small + 4 * MB,
            "3 MiB should come from the 4 MiB hole, not the 16 MiB one; got {a:#x}"
        );
        let b = w.reserve(14 * MB, FLOOR).unwrap();
        assert!(
            b >= big && b < big + 16 * MB,
            "14 MiB should come from the 16 MiB hole, got {b:#x}"
        );
        assert_eq!(
            w.top, top_before,
            "neither request should have grown the window"
        );
        let _ = (keep, pin);
    }

    /// A hole larger than the request is carved from its top and the remainder
    /// stays available.
    #[test]
    fn a_partial_hole_keeps_its_remainder() {
        let mut w = win();
        let a = w.reserve(8 * MB, FLOOR).unwrap();
        let pin = w.reserve(1 * MB, FLOOR).unwrap();
        w.release(a, a + 8 * MB);

        let got = w.reserve(3 * MB, FLOOR).unwrap();
        assert_eq!(got, a + 5 * MB, "should carve from the hole's top");
        assert_eq!(w.free, vec![(a, a + 5 * MB)]);
        assert_invariant(&w);
        let _ = pin;
    }

    /// The floor stops the bump, and a refused request must leave the window
    /// untouched -- a `top` moved by a failed reserve would hand the next
    /// caller an address inside the private mmap arena.
    #[test]
    fn the_floor_bounds_the_bump_and_a_refusal_changes_nothing() {
        let mut w = win();
        let floor = MMAP_END_VMA - 10 * MB;
        assert!(w.reserve(11 * MB, floor).is_none());
        assert_eq!(w.top, MMAP_END_VMA);
        let at = w.reserve(10 * MB, floor).unwrap();
        assert_eq!(at, floor);
        assert!(w.reserve(1, floor).is_none());
        assert_eq!(w.top, floor);
        assert_invariant(&w);
    }

    /// Releasing an empty range must not insert a zero-length hole: the
    /// bump-absorption loop keys on `start == top`, and a zero-length entry
    /// sitting there would spin forever.
    #[test]
    fn an_empty_release_is_a_no_op() {
        let mut w = win();
        w.release(MMAP_END_VMA, MMAP_END_VMA);
        w.release(MMAP_END_VMA, MMAP_END_VMA - 1);
        assert!(w.free.is_empty());
        assert_eq!(w.top, MMAP_END_VMA);
    }

    /// Adjacent holes merge whichever order they arrive in.
    #[test]
    fn adjacent_holes_merge_from_either_side() {
        let base = MMAP_START_VMA + 32 * MB;
        for reverse in [false, true] {
            let mut w = win();
            w.top = base; // pretend the bump has already been pushed down
            let mut parts = vec![(base + MB, base + 2 * MB), (base + 2 * MB, base + 3 * MB)];
            if reverse {
                parts.reverse();
            }
            for (s, e) in parts {
                w.release(s, e);
            }
            assert_invariant(&w);
            assert_eq!(
                w.free,
                vec![(base + MB, base + 3 * MB)],
                "reverse={reverse}: adjacent holes did not merge"
            );
        }
    }

    /// Same-size churn -- mmap N bytes, free it, repeat -- must cost the window
    /// nothing. This is what a long-running guest does, and with `munmap` a
    /// no-op it consumed the window at N per cycle until the guest died.
    ///
    /// The neutralization is inside the test: the same loop without releasing
    /// must run out, or "the loop completed" would only be recording that the
    /// window is large.
    #[test]
    fn same_size_churn_costs_the_window_nothing() {
        let ceiling = MMAP_START_VMA + 64 * MB;
        let mut a = Arena::new();
        for _ in 0..50 {
            let at = a
                .mmap_reserve(16 * MB, ceiling)
                .expect("reserve in a churn loop");
            assert!(a.mmap_release(at, 16 * MB));
            assert_eq!(a.mmap_cur, MMAP_START_VMA, "window not returned");
            assert!(a.mmap_free.is_empty());
        }

        let mut leaky = Arena::new();
        let mut cycles = 0;
        while leaky.mmap_reserve(16 * MB, ceiling).is_some() {
            cycles += 1;
            assert!(
                cycles < 50,
                "the ceiling is too high for this to mean anything"
            );
        }
        assert_eq!(cycles, 4, "without releasing, a 64 MiB window holds four");
    }

    /// A strictly DOUBLING ladder is the case reclamation does NOT rescue, and
    /// this pins that so nobody re-derives it the expensive way.
    ///
    /// glibc's malloc grows its arena by doubling and holds both the old and
    /// the new while it moves over. Every rung is therefore larger than every
    /// rung before it COMBINED, so the coalesced hole left below is always
    /// about half of what the next rung needs, and the live previous arena pins
    /// the bump high. Reclaiming turns a requirement of "the sum of all rungs"
    /// into a requirement of... the sum of all rungs. The window simply has to
    /// be big enough; that is why the arena grew to 512 MiB and why capping
    /// `shared_buffers` matters, not this allocator.
    #[test]
    fn a_doubling_ladder_is_not_rescued_by_reclaiming() {
        let ladder = [3 * MB, 6 * MB, 12 * MB, 24 * MB, 48 * MB, 96 * MB];
        let sum: u64 = ladder.iter().sum();
        // 1.5x the peak. A doubling series sums to very nearly TWICE its last
        // term, so "room for the peak, and half again" is not enough -- which
        // is the number worth knowing, and which I got wrong twice by
        // reasoning instead of running it.
        let ceiling = MMAP_START_VMA + 144 * MB;

        let mut a = Arena::new();
        let mut prev: Option<(u64, u64)> = None;
        let mut refused = None;
        for &n in &ladder {
            match a.mmap_reserve(n, ceiling) {
                Some(at) => {
                    if let Some((ps, pl)) = prev {
                        assert!(a.mmap_release(ps, pl));
                    }
                    prev = Some((at, n));
                }
                None => {
                    refused = Some(n);
                    break;
                }
            }
        }
        assert_eq!(
            refused,
            Some(96 * MB),
            "a doubling ladder needs its SUM ({} MiB, ~2x the peak), not the \
             peak; if this now passes the allocator has changed and the sizing \
             argument in the module header needs re-deriving",
            sum / MB
        );

        // And with the window sized for the sum, it runs -- reclaiming or not.
        let mut big = Arena::new();
        let roomy = MMAP_START_VMA + sum + 16 * MB;
        for &n in &ladder {
            assert!(big.mmap_reserve(n, roomy).is_some(), "{} MiB rung", n / MB);
        }
    }

    /// A partial unmap must be IGNORED, not honoured. Releasing a whole region
    /// for a sub-range request would hand back memory the guest still has
    /// mapped, which is silent corruption; leaking is the safe direction.
    #[test]
    fn a_partial_unmap_is_not_reclaimed() {
        let ceiling = MMAP_START_VMA + 64 * MB;
        let mut a = Arena::new();
        let at = a.mmap_reserve(8 * MB, ceiling).unwrap();
        assert!(
            !a.mmap_release(at, 4 * MB),
            "a shorter extent must not match"
        );
        assert!(
            !a.mmap_release(at + MB, 8 * MB),
            "a shifted extent must not match"
        );
        assert!(a.mmap_free.is_empty());
        assert_eq!(a.mmap_cur, at + 8 * MB, "the bump must not have moved");
        assert!(a.mmap_release(at, 8 * MB), "the exact extent must match");
        assert_eq!(a.mmap_cur, MMAP_START_VMA);
    }

    /// The same rule for a SHARED region, where the failure direction is the
    /// opposite one and worse.
    ///
    /// `shm_seg_at` matches a shared region by its START and `NR_MUNMAP` used to
    /// act on that alone, so `munmap(region, 4 MiB)` on a 16 MiB region counted
    /// as a full detach. A TAIL unmap was ignored (it names no region's start),
    /// which is the leak TODO.md tracks; a HEAD unmap of any length was
    /// HONOURED, and as the last mapper that hands the whole region back to the
    /// window while the caller still has the rest of it mapped.
    #[test]
    fn only_a_whole_unmap_gives_up_a_shared_region() {
        let seg = SharedSeg {
            vma_start: MMAP_END_VMA - 16 * MB,
            len: 16 * MB as usize,
            kind: SharedKind::Anon,
            mappers: vec![9],
        };
        let at = seg.vma_start;
        assert!(
            seg.unmap_is_whole(at, 16 * MB),
            "the region's own length must be a detach, or nothing is ever reclaimed"
        );
        assert!(
            seg.unmap_is_whole(at, 32 * MB),
            "a request covering MORE than the region is still a detach"
        );
        assert!(
            !seg.unmap_is_whole(at, 4 * MB),
            "a HEAD unmap must not give up the 12 MiB the caller still maps"
        );
        assert!(
            !seg.unmap_is_whole(at, 0),
            "a zero length -- which is how a refused rounding arrives -- gives up nothing"
        );
        assert!(
            !seg.unmap_is_whole(at + 4 * MB, 12 * MB),
            "a TAIL unmap does not name this region's start"
        );
        // The rounding lives at the call site, and the two must agree or every
        // ordinary unmap would look partial: a 1000-byte MAP_SHARED registers a
        // 64 KiB region and the guest unmaps the 1000 it asked for.
        let small = SharedSeg {
            len: mmap_round_len(1000).unwrap() as usize,
            ..seg
        };
        assert_eq!(small.len as u64, GUEST_PAGE_MASK + 1);
        assert!(small.unmap_is_whole(at, mmap_round_len(1000).unwrap()));
    }

    /// The consequence, at the level the syscall arm acts on it: a partial unmap
    /// must leave both the claim and the window exactly as they were, and a
    /// later reservation must not be able to land inside the live region.
    ///
    /// The body mirrors `NR_MUNMAP`, which lives in the wasm-only `sys` and
    /// cannot be called from here -- match the region by start, and only if the
    /// request covers all of it drop the mapper and hand the range back. What is
    /// under test is the predicate that arm branches on; the two lines around it
    /// are here so the assertion can be about a recycled ADDRESS rather than
    /// about a boolean.
    #[test]
    fn a_partial_unmap_does_not_recycle_the_shared_window() {
        let unmap = |seg: &mut SharedSeg, w: &mut ShmWindow, pid: u32, addr, len| {
            if seg.unmap_is_whole(addr, len) {
                seg.mappers.retain(|&p| p != pid);
                if seg.mappers.is_empty() {
                    w.release(seg.vma_start, seg.vma_start + seg.len as u64);
                }
            }
        };

        let mut w = win();
        let at = w.reserve(16 * MB, FLOOR).unwrap();
        let mut seg = SharedSeg {
            vma_start: at,
            len: 16 * MB as usize,
            kind: SharedKind::Anon,
            mappers: vec![11],
        };

        // The guest gives up the first 4 MiB and goes on reading the other 12.
        unmap(&mut seg, &mut w, 11, at, 4 * MB);
        assert_eq!(
            seg.mappers,
            vec![11],
            "a partial unmap dropped the caller's claim on the whole region"
        );
        assert!(
            w.free.is_empty(),
            "a partial unmap returned a range that is still mapped: {:?}",
            w.free
        );
        assert_eq!(w.top, at, "a partial unmap moved the window's bump");
        // The address is the point: a recycled region is handed to somebody else
        // at the same VMA, which is the corruption a leak avoids.
        let other = w.reserve(4 * MB, FLOOR).unwrap();
        assert!(
            other + 4 * MB <= at,
            "the next reservation landed at {other:#x}, inside the live region \
             {at:#x}..{:#x}",
            at + 16 * MB
        );
        assert_invariant(&w);

        // ...and a whole unmap still gives the region back, or the check above
        // would be satisfied by never reclaiming anything.
        unmap(&mut seg, &mut w, 11, at, 16 * MB);
        assert!(seg.mappers.is_empty(), "a whole unmap kept the claim");
        assert_eq!(
            w.free,
            vec![(at, at + 16 * MB)],
            "a whole unmap did not return the region to the window"
        );
        assert_invariant(&w);
    }

    /// A hole is reused before the bump grows, and carved from the BOTTOM so
    /// the window keeps its ascending grain.
    #[test]
    fn a_private_hole_is_reused_before_the_bump_grows() {
        let ceiling = MMAP_START_VMA + 64 * MB;
        let mut a = Arena::new();
        let lo = a.mmap_reserve(8 * MB, ceiling).unwrap();
        let pin = a.mmap_reserve(MB, ceiling).unwrap(); // keeps `lo` off the bump
        a.mmap_release(lo, 8 * MB);
        let top_before = a.mmap_cur;

        let got = a.mmap_reserve(3 * MB, ceiling).unwrap();
        assert_eq!(got, lo, "should carve from the hole's bottom");
        assert_eq!(a.mmap_free, vec![(lo + 3 * MB, lo + 8 * MB)]);
        assert_eq!(a.mmap_cur, top_before, "the bump must not have grown");
        let _ = pin;
    }

    /// The private bump must never climb into the shared window, whose floor is
    /// passed in as the ceiling. A refused reservation must also leave the bump
    /// where it was.
    #[test]
    fn the_private_bump_stops_at_the_shared_floor() {
        let ceiling = MMAP_START_VMA + 10 * MB;
        let mut a = Arena::new();
        assert!(a.mmap_reserve(11 * MB, ceiling).is_none());
        assert_eq!(a.mmap_cur, MMAP_START_VMA);
        assert!(a.mmap_reserve(10 * MB, ceiling).is_some());
        assert!(a.mmap_reserve(1, ceiling).is_none());
        assert_eq!(a.mmap_cur, ceiling);
    }

    /// execve wipes the arena for the new image, and must leave live shared
    /// regions -- which belong to OTHER processes -- alone.
    #[test]
    fn reset_preserves_shared_regions() {
        let mut a = Arena::new();
        let shared_at = MMAP_END_VMA - 2 * MB;
        let private_at = MMAP_START_VMA;
        a.slice_mut(shared_at, 4096).fill(0xAB);
        a.slice_mut(private_at, 4096).fill(0xCD);

        a.reset(&[SharedSeg {
            vma_start: shared_at,
            len: 2 * MB as usize,
            kind: SharedKind::Anon,
            mappers: vec![7],
        }]);

        assert!(
            a.slice(shared_at, 4096).iter().all(|&b| b == 0xAB),
            "execve zeroed a shared region another process still maps"
        );
        assert!(
            a.slice(private_at, 4096).iter().all(|&b| b == 0),
            "execve left the private image behind"
        );
        // The byte just past the region is private and must be cleared, which is
        // what catches an off-by-one that preserves too much.
        assert_eq!(a.slice(shared_at + 2 * MB, 1)[0], 0);
        assert_eq!(a.brk_cur, BRK_START_VMA);
        assert_eq!(a.mmap_cur, MMAP_START_VMA);
    }

    /// Overlapping and nested registrations are legal (two mappings may cover
    /// the same bytes), and must not make `reset` skip the gap between them.
    #[test]
    fn reset_handles_overlapping_shared_regions() {
        let mut a = Arena::new();
        let lo = MMAP_START_VMA + 8 * MB;
        a.slice_mut(lo, 3 * MB as usize).fill(0xEE);
        let segs = vec![
            SharedSeg {
                vma_start: lo,
                len: 2 * MB as usize,
                kind: SharedKind::Anon,
                mappers: vec![1],
            },
            // Nested inside the first.
            SharedSeg {
                vma_start: lo + MB / 2,
                len: MB as usize / 2,
                kind: SharedKind::Anon,
                mappers: vec![1],
            },
            // Overlapping its tail.
            SharedSeg {
                vma_start: lo + MB,
                len: 2 * MB as usize,
                kind: SharedKind::File,
                mappers: vec![2],
            },
        ];
        a.reset(&segs);
        assert!(a.slice(lo, 3 * MB as usize).iter().all(|&b| b == 0xEE));
        assert_eq!(a.slice(lo + 3 * MB, 1)[0], 0);
        assert_eq!(a.slice(lo - 1, 1)[0], 0);
    }

    // --- The snapcheck oracle -------------------------------------------------

    /// ⚠️ THE oracle test. `bytes_differing_outside` compares the live arena
    /// against the incoming process's saved memory, and only `SnapshotData::Full`
    /// carries that memory. Since bounded snapshots became the only scheme
    /// (2026-08-22) a single-threaded process saves ranges, so the probe has
    /// NOTHING to compare against on an ordinary switch.
    ///
    /// This used to return a zeroed `SnapDiff`, and the caller printed it as
    /// `miss=0` -- a clean bill of health computed from no data. The claim being
    /// pinned here is that "no oracle" is now DISTINGUISHABLE from "checked, no
    /// differences", and the arena below is deliberately made to differ wildly
    /// from the snapshot so that a `Some(0)` would be a lie about a real
    /// difference rather than an accident of an empty arena.
    #[test]
    fn a_bounded_snapshot_is_no_oracle_and_says_so_rather_than_reporting_zero() {
        let mut a = Arena::new();
        a.set_brk(BRK_START_VMA + 4096);
        let ranges = bounded_ranges(a.brk_cur, &a.mmap_live.clone(), &[], 0, 64);
        let snap = a.snapshot_bounded(&ranges, 0);

        // Scribble OUTSIDE every saved range, so a comparison that really
        // happened would have to report a non-zero count.
        let far = MMAP_START_VMA + (32 << 20);
        a.slice_mut(far, 4096).fill(0xAB);

        assert!(
            a.bytes_differing_outside(&snap, &ranges).is_none(),
            "a bounded snapshot carries no full buffer, so there is nothing to \
             compare against and the probe must say so. Returning a zeroed \
             SnapDiff here is what made it print `miss=0` for a check it never \
             performed -- and this arena differs from the snapshot by 4096 bytes."
        );
    }

    /// The other half, so the test above cannot pass by the function having
    /// become a constant `None`. A `Full` snapshot IS an oracle, and the
    /// comparison must both happen and find what is really there.
    ///
    /// Both directions are asserted from one snapshot: a byte inside the range
    /// set is exempt (it would be restored), and a byte outside it is counted
    /// and located.
    #[test]
    fn a_full_snapshot_is_an_oracle_and_the_comparison_actually_runs() {
        let mut a = Arena::new();
        a.set_brk(BRK_START_VMA + 4096);
        let ranges = bounded_ranges(a.brk_cur, &a.mmap_live.clone(), &[], 0, 64);
        let snap = a.snapshot(0);

        // One byte INSIDE the range set (the brk region) and one OUTSIDE it (the
        // mmap window, with no live mapping over it).
        let far = MMAP_START_VMA + (32 << 20);
        a.slice_mut(BRK_START_VMA, 1)[0] = 0x5A;
        a.slice_mut(far, 3).fill(0xAB);

        let d = a
            .bytes_differing_outside(&snap, &ranges)
            .expect("a Full snapshot is exactly the oracle this probe needs");
        assert_eq!(
            d.total(),
            3,
            "the three bytes outside the range set must be counted, and the one \
             inside it must not -- a bounded snapshot would restore that one"
        );
        assert_eq!(d.counts[3], 3, "and they belong to the mmap region");
        assert_eq!(
            d.first[3], far,
            "the address is the point: a count says a region is wrong, an \
             address says which structure"
        );
        assert_eq!(
            d.first[2],
            u64::MAX,
            "the brk region is covered, so it must report clean"
        );
    }
}
