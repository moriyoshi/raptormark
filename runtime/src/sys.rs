//! Linux aarch64 syscall emulation (the ecvisor replacement for upstream
//! `SyscallWasi.cpp`). Filesystem calls route through the pluggable VFS and a
//! per-process fd table; stdio passes through to WASI. Unimplemented calls log
//! and return -ENOSYS (upstream returned a bare -1).

use crate::abi::State;
// `GUEST_PAGE_MASK` and `mmap_round_len` live in `arena` rather than here for
// one reason: `sys` is `#[cfg(target_arch = "wasm32")]` (see lib.rs), so nothing
// defined in this file is reachable from `cargo test`. `arena` is host-compiled,
// which is what lets the length rules below be asserted by a unit test instead
// of only by the env-gated E2E suite -- the same seam `madvise_zeroes` uses.
use crate::arena::{
    mmap_round_len, Arena, MmapLenError, SharedKind, SharedSeg, BRK_END_VMA, BRK_START_VMA,
    GUEST_PAGE_MASK,
};
use crate::context::{
    sigaction_permitted, sigprocmask_next, BlockedOn, EcvContext, EpollItem, ExitReason, OpenFile,
    Pending, Pipe, ShmFile, ShmSeg, SigAction, UnitLoadStep, UnixListener,
};
use crate::net::{errno_of, NetBackend, NetErr, NetHandle, Op, SockAddr};
// No `ecv_warn` here: every diagnostic in this file is gated, and the one
// remaining raw `eprintln!` is the panic hook, which stays raw on purpose.
use crate::trace::{ecv_debug, ecv_probe, ecv_trace};
use crate::vfs::{NodeKind, Resolved};
use std::collections::VecDeque;
use std::time::{SystemTime, UNIX_EPOCH};

// The debug/trace flags are read ONCE at startup (init_diag_flags, called from
// entry.rs before the scheduler runs any guest) into plain atomics, NOT lazily on
// each syscall. Lazy env reads were fatal on the fork path: std's env accessor
// takes a LazyLock, and the fork's asyncify unwind/rewind leaves ecvisor's static
// state such that a post-fork `std::env::var` hits `lazy_lock::panic_poisoned` and
// the panic handler re-reads env -> an infinite loop (the dynamic-PIE fork "hang";
// pinned with the wazero fntrace harness, see .agents/docs/DYNLINK.md). Reading a
// pre-initialized atomic touches no env and cannot poison. 0 = uninit, 1 = off,
// 2 = on.

// Wasm C shadow-stack low-water probe (RAPTORMARK_ECV_LEGSP): when set, every
// guest call samples `__stack_pointer` and logs a line whenever it reaches a new
// low, with the guest call depth (call_history len). Diagnostic; off by default.

// Call-site execution counter (RAPTORMARK_ECV_COUNTRET=<hex return address>).
// Counting one call site distinguishes "ran once, state corrupt" from "really
// ran N times", which end-state dumps cannot. 0 = off.
// The diagnostic gates live in `crate::diag` so `context` (which is not
// target-gated) can read them; re-exported here because every existing caller
// spells them `sys::`.
//
// `debug_log`/`trace_log` are still needed directly at the four sites that keep an
// explicit guard -- the ones where the guard protects WORK (an arena read, a Vec)
// rather than a print -- and at the one site whose gate is the UNION of the two.
// Everywhere else `ecv_debug!`/`ecv_trace!` own the test.
pub(crate) use crate::diag::{
    bump_ret_count, count_ret, dtrace_dump, dtrace_dumpx0, dtrace_minpid, dtrace_range,
    dtrace_regs, legsp, trace_log,
};

/// Reads the diagnostic env vars ONCE, at startup, before any guest runs or forks.
/// Idempotent. Call from entry.rs.
pub fn init_diag_flags() {
    let on = |name: &str| std::env::var(name).is_ok();
    crate::diag::set_gates(
        on("RAPTORMARK_ECV_DEBUG"),
        on("RAPTORMARK_ECV_TRACE"),
        on("RAPTORMARK_ECV_LEGSP"),
        on("RAPTORMARK_ECV_SNAPSTAT"),
        on("RAPTORMARK_ECV_SNAPCHECK"),
    );
    // ⚠️ Nothing here selects a snapshot scheme. Bounded arena snapshots were an
    // opt-in gate, then the default, and since 2026-08-22 they are the only
    // scheme -- the variable is gone rather than pinned on, so no script can put
    // this runtime back on the full-buffer path. A full snapshot is still taken
    // for a multi-threaded group, decided by `is_multithreaded` and not by any
    // environment. `diag::tests::the_removed_snapshot_gate_stays_removed` is the
    // tripwire for a reintroduction.
    crate::diag::set_filetrace(on("RAPTORMARK_ECV_FILETRACE"));
    crate::diag::set_fdcheck(on("RAPTORMARK_ECV_FDCHECK"));
    crate::diag::set_tables(on("RAPTORMARK_ECV_TABLES"));
    crate::diag::set_nonblocking(on("RAPTORMARK_ECV_NONBLOCK"));
    // Not a diagnostic gate, but it must be read HERE for the same reason they
    // are: `loader::wasix` needs it from inside a `dlopen` syscall, which is a
    // post-fork path, and an env read there can loop forever.
    crate::diag::set_side_dir(std::env::var("RAPTORMARK_SIDE_DIR").unwrap_or_default());
    // ⚠️ UNSOUND WHEN ON. Makes `__ecv_warning` record an undecoded site and
    // RETURN instead of aborting, so the guest runs on with the instruction's
    // effect never applied and everything downstream of it is garbage. It is a
    // census instrument -- "which undecoded sites does this workload actually
    // execute" in one run -- and never a way to get further. Its own line and
    // not part of `set_gates` because it changes BEHAVIOUR, not output; it
    // prints its own banner when armed. See `diag::set_undec_census`.
    crate::diag::set_undec_census(on("RAPTORMARK_ECV_UNDEC_CENSUS"));

    let hexenv = |name: &str| -> u64 {
        std::env::var(name)
            .ok()
            .and_then(|s| u64::from_str_radix(s.trim().trim_start_matches("0x"), 16).ok())
            .unwrap_or(0)
    };
    crate::diag::set_count_ret(hexenv("RAPTORMARK_ECV_COUNTRET"));
    // Decimal: it is a frame count, not a VMA.
    crate::diag::set_max_depth(
        std::env::var("RAPTORMARK_ECV_MAXDEPTH")
            .ok()
            .and_then(|s| s.trim().parse::<u64>().ok())
            .unwrap_or(0),
    );
    // Decimal bytes: a size, not a VMA.
    crate::diag::set_thread_stack(
        std::env::var("RAPTORMARK_ECV_THREAD_STACK")
            .ok()
            .and_then(|s| s.trim().parse::<u64>().ok())
            .unwrap_or(0),
    );
    let decenv = |name: &str| -> u64 {
        std::env::var(name)
            .ok()
            .and_then(|s| s.trim().parse::<u64>().ok())
            .unwrap_or(0)
    };
    crate::diag::set_watch(
        hexenv("RAPTORMARK_ECV_WATCH"),
        hexenv("RAPTORMARK_ECV_WATCHLEN"),
    );
    crate::diag::set_dtrace(
        hexenv("RAPTORMARK_ECV_DTRACE_LO"),
        hexenv("RAPTORMARK_ECV_DTRACE_HI"),
        // MINPID is decimal (a pid), not hex like the VMA bounds; so are the
        // two dump byte counts.
        decenv("RAPTORMARK_ECV_DTRACE_MINPID"),
        decenv("RAPTORMARK_ECV_DTRACE_DUMP"),
        decenv("RAPTORMARK_ECV_DTRACE_DUMPX0"),
        // Read ONCE here, not per traced call. See diag::dtrace_regs.
        on("RAPTORMARK_ECV_DTRACE_REGS"),
    );
    // Neither of these prints anything, and that is exactly why they were the
    // last two env reads left on post-fork paths -- a sweep that looks for
    // logging gates does not find a bisect switch or a heap-scan spec. The
    // contract is about the environment, not about logging.
    crate::diag::set_no_file_shm(on("RAPTORMARK_ECV_NO_FILE_SHM"));
    crate::diag::set_scan(&std::env::var("RAPTORMARK_ECV_SCAN").unwrap_or_default());
    // `=1`, not merely set: this one selects between two codegen paths, so an
    // operator who writes `=0` to turn it off must get it turned off.
    crate::diag::set_inline_ch(std::env::var("RAPTORMARK_ECV_INLINE_CH").is_ok_and(|v| v == "1"));

    // LAST, and it must stay last: it summarises every gate set above into the
    // one flag the per-call hooks test. Anything added here that
    // `_ecv_save_call_history` or `_ecv_func_epilogue` consults belongs in this
    // disjunction too -- omitting it costs that diagnostic, never correctness,
    // because the cold path re-reads the real gate.
    let slow = crate::diag::debug_log()
        || crate::diag::legsp()
        || crate::diag::count_ret() != 0
        || crate::diag::watch()
        || crate::diag::dtrace();
    crate::diag::set_hot_slow(slow);
    // NOT published to a global any more. The lifted code used to read
    // `__ecv_slow` at every call site to decide whether to take the inline
    // path; it now reads only the capacity, and `EcvContext::new` folds this
    // answer into that (a diagnostics-armed run publishes capacity zero). One
    // fewer global read and one fewer branch per guest BL.

    // Custom panic hook that prints the location + message WITHOUT reading the
    // environment (the default hook reads RUST_BACKTRACE via std env's LazyLock,
    // which — poisoned across the fork's asyncify replay — infinite-loops). Makes
    // any post-fork panic a clean, visible abort instead of a hang. See DYNLINK.md.
    //
    // Deliberately a raw `eprintln!` rather than an `ecv_warn!`, and so is
    // `runtime_error` in lib.rs. Both run on the way out, and there is no reason for
    // the last message before an abort to travel through one more layer than it has
    // to.
    std::panic::set_hook(Box::new(|info| {
        eprintln!("[ecvisor] PANIC: {info}");
    }));
}

// aarch64 Linux syscall numbers (SysTable.h / unistd.h).
const NR_GETCWD: u64 = 17;
const NR_DUP: u64 = 23;
const NR_DUP3: u64 = 24;
const NR_FCNTL: u64 = 25;
const NR_IOCTL: u64 = 29;
const NR_PIPE2: u64 = 59;
const NR_FACCESSAT: u64 = 48;
const NR_CHDIR: u64 = 49;
const NR_FCHDIR: u64 = 50;
const NR_RENAMEAT: u64 = 38;
const NR_TRUNCATE: u64 = 45;
const NR_UNLINKAT: u64 = 35;
const NR_FTRUNCATE: u64 = 46;
const NR_FALLOCATE: u64 = 47;
const NR_SET_ROBUST_LIST: u64 = 99;
const NR_OPENAT: u64 = 56;
const NR_CLOSE: u64 = 57;
const NR_GETDENTS64: u64 = 61;
const NR_LSEEK: u64 = 62;
const NR_READ: u64 = 63;
const NR_WRITE: u64 = 64;
const NR_READV: u64 = 65;
const NR_WRITEV: u64 = 66;
const NR_PREAD64: u64 = 67;
const NR_PWRITE64: u64 = 68;
const NR_READLINKAT: u64 = 78;
const NR_NEWFSTATAT: u64 = 79;
const NR_FSTAT: u64 = 80;
const NR_EXIT: u64 = 93;
const NR_EXIT_GROUP: u64 = 94;
const NR_SET_TID_ADDRESS: u64 = 96;
const NR_FUTEX: u64 = 98;
// epoll + signalfd + signal delivery: PostgreSQL's latch / WaitEventSet layer
// (SIGURG -> signalfd -> epoll, SetLatch = kill) is built entirely on these.
// aarch64 has no epoll_wait/epoll_create; glibc routes them to epoll_pwait(22)
// and epoll_create1(20).
const NR_EPOLL_CREATE1: u64 = 20;
const NR_EPOLL_CTL: u64 = 21;
const NR_EPOLL_PWAIT: u64 = 22;
/// ppoll. aarch64 has NO plain `poll` syscall, so glibc's `poll()` -- and
/// therefore libpq's `pqSocketCheck`, which is how psql waits for the server on
/// EVERY connection -- lands here. Leaving it ENOSYS made a guest-side psql
/// unable to talk to a postmaster that was listening perfectly well.
const NR_PPOLL: u64 = 73;
const NR_SIGNALFD4: u64 = 74;
const NR_EVENTFD2: u64 = 19;

// The nginx surface. Every number here was checked against
// <asm-generic/unistd.h> on an aarch64 host rather than recalled: aarch64 has no
// legacy numbering to fall back on, and a wrong constant is a syscall that
// silently does something else.
const NR_MKDIRAT: u64 = 34;
const NR_FCHMOD: u64 = 52;
const NR_FCHMODAT: u64 = 53;
const NR_FCHOWNAT: u64 = 54;
const NR_FCHOWN: u64 = 55;
const NR_SENDFILE: u64 = 71;
const NR_RT_SIGSUSPEND: u64 = 133;
const NR_SETGID: u64 = 144;
const NR_SETUID: u64 = 146;
const NR_SETRESUID: u64 = 147;
const NR_SETRESGID: u64 = 149;
const NR_GETGROUPS: u64 = 158;
const NR_SETGROUPS: u64 = 159;
const NR_SOCKETPAIR: u64 = 199;
const NR_SENDMSG: u64 = 211;
const NR_RECVMSG: u64 = 212;
const NR_PREADV: u64 = 69;
const NR_PWRITEV: u64 = 70;
const NR_PREADV2: u64 = 286;
const NR_PWRITEV2: u64 = 287;
// Deliberately unimplemented, listed so they read as decisions rather than gaps:
// io_setup is Linux AIO (nginx logs a notice and falls back to blocking I/O) and
// rseq is glibc's restartable-sequences registration (glibc falls back when it
// fails). Both must fail, and ENOSYS is how they are told so.
const NR_IO_SETUP: u64 = 0;
const NR_RSEQ: u64 = 293;
// System V shared memory. PostgreSQL's PGSharedMemoryCreate makes a small SysV
// segment as its postmaster interlock even when the main shmem is anon-mmap.
const NR_SHMGET: u64 = 194;
const NR_SHMCTL: u64 = 195;
const NR_SHMAT: u64 = 196;
const NR_SHMDT: u64 = 197;
const IPC_CREAT: u64 = 0o1000;
const IPC_EXCL: u64 = 0o2000;
const IPC_PRIVATE: i32 = 0;
const IPC_RMID: u64 = 0;
const IPC_STAT: u64 = 2;
const NR_KILL: u64 = 129;
const NR_TKILL: u64 = 130;
const NR_TGKILL: u64 = 131;
const NR_UMASK: u64 = 166;
const NR_GET_MEMPOLICY: u64 = 236;

// epoll_ctl ops and the event bits we model.
const EPOLL_CTL_ADD: u64 = 1;
const EPOLL_CTL_DEL: u64 = 2;
const EPOLL_CTL_MOD: u64 = 3;
const EPOLLIN: u32 = 0x1;
const EPOLLOUT: u32 = 0x4;
const EPOLLHUP: u32 = 0x10;
// poll(2) bits. POLLIN/POLLOUT/POLLHUP deliberately share EPOLLIN/EPOLLOUT/
// EPOLLHUP's values -- that is true on Linux and is what lets `fd_ready` serve
// both interfaces from one readiness oracle rather than two that can disagree.
const POLLERR: u32 = 0x8;
const POLLNVAL: u32 = 0x20;
/// sizeof(struct pollfd): int fd; short events; short revents.
const POLLFD_SIZE: u64 = 8;
/// `struct epoll_event` is NOT __EPOLL_PACKED on aarch64 (only on x86_64), so it
/// is 16 bytes: u32 events at 0, 4 bytes padding, u64 data at 8.
const EPOLL_EVENT_SIZE: u64 = 16;
/// `struct signalfd_siginfo` is a fixed 128 bytes; ssi_signo is the leading u32.
const SIGNALFD_SIGINFO_SIZE: usize = 128;

const NR_NANOSLEEP: u64 = 101;
const NR_GETITIMER: u64 = 102;
const NR_SETITIMER: u64 = 103;
const NR_CLOCK_GETTIME: u64 = 113;
const NR_CLOCK_GETRES: u64 = 114;
const NR_CLOCK_NANOSLEEP: u64 = 115;
const NR_SCHED_GETAFFINITY: u64 = 123;
const NR_SCHED_YIELD: u64 = 124;
const NR_RT_SIGTIMEDWAIT: u64 = 137;
const NR_RT_SIGACTION: u64 = 134;
const NR_RT_SIGPROCMASK: u64 = 135;
const NR_RT_SIGRETURN: u64 = 139;
const NR_UNAME: u64 = 160;
const NR_GETRUSAGE: u64 = 165;
const NR_PRCTL: u64 = 167;
const NR_GETTIMEOFDAY: u64 = 169;
const NR_SETPGID: u64 = 154;
const NR_GETPGID: u64 = 155;
const NR_GETSID: u64 = 156;
const NR_SETSID: u64 = 157;
const NR_GETPID: u64 = 172;
const NR_GETPPID: u64 = 173;
const NR_GETUID: u64 = 174;
const NR_GETEUID: u64 = 175;
const NR_GETGID: u64 = 176;
const NR_GETEGID: u64 = 177;
const NR_GETTID: u64 = 178;
const NR_CLONE: u64 = 220;
const NR_EXECVE: u64 = 221;
const NR_WAIT4: u64 = 260;
const NR_PRLIMIT64: u64 = 261;
const NR_BRK: u64 = 214;
const NR_MUNMAP: u64 = 215;
const NR_MMAP: u64 = 222;

const NR_SIGALTSTACK: u64 = 132;
const NR_MPROTECT: u64 = 226;
const NR_MREMAP: u64 = 216;
const NR_MADVISE: u64 = 233;
const NR_MEMBARRIER: u64 = 283;
const NR_SYSINFO: u64 = 179;
const NR_GETRANDOM: u64 = 278;
// Pseudo-syscalls: the prelinker overwrites dlopen/dlsym/dlclose/dlerror with a
// `movz x8,#nr; svc #0; ret` stub, so these trap here instead of running glibc's
// loader-dependent dlopen. See internal/prelink/glibc.go (dl* interception).
/// `RTLD_NOLOAD` from glibc's `<dlfcn.h>`: return a handle only if the object is
/// already loaded, and never load it.
const RTLD_NOLOAD: u64 = 0x00004;
const NR_ECV_DLOPEN: u64 = 0xF00;
const NR_ECV_DLSYM: u64 = 0xF01;
const NR_ECV_DLCLOSE: u64 = 0xF02;
const NR_ECV_DLERROR: u64 = 0xF03;
const NR_FACCESSAT2: u64 = 439;
// Durability syscalls. The guest filesystem is the in-memory rfs plus a tmpfs
// overlay, so there is no storage below to flush to.
const NR_SYNC: u64 = 81;
const NR_FSYNC: u64 = 82;
const NR_FDATASYNC: u64 = 83;
const NR_SYNC_FILE_RANGE: u64 = 84;
const NR_SYNCFS: u64 = 267;

// Socket syscalls (aarch64 Linux).
const NR_SOCKET: u64 = 198;
const NR_BIND: u64 = 200;
const NR_LISTEN: u64 = 201;
const NR_ACCEPT: u64 = 202;
const NR_CONNECT: u64 = 203;
const NR_GETSOCKNAME: u64 = 204;
const NR_GETPEERNAME: u64 = 205;
const NR_SENDTO: u64 = 206;
const NR_RECVFROM: u64 = 207;
const NR_SETSOCKOPT: u64 = 208;
const NR_GETSOCKOPT: u64 = 209;
const NR_SHUTDOWN: u64 = 210;
const NR_ACCEPT4: u64 = 242;

// Linux errnos (positive; returned negated).
const EBADF: u64 = 9;
const ECHILD: u64 = 10;
const EPIPE: u64 = 32;
// Socket-teardown errnos (aarch64 Linux), for translating host write failures.
const ENOTCONN: u64 = 107;
const ENOENT: u64 = 2;
const ENOEXEC: u64 = 8;
const ELOOP: u64 = 40; // too many `#!` levels, as Linux reports it
const ENOTDIR: u64 = 20;
const EISDIR: u64 = 21;
const EINVAL: u64 = 22;
const EAGAIN: u64 = 11; // FUTEX_WAIT race fast-path (value mismatch)
const ETIMEDOUT: u64 = 110; // a timed FUTEX_WAIT whose deadline passed
const ESRCH: u64 = 3; // kill: no such process
const EEXIST: u64 = 17; // epoll_ctl ADD on an already-registered fd
const ENOTTY: u64 = 25;
const EINTR: u64 = 4; // rt_sigsuspend always returns this once a handler ran
const ESPIPE: u64 = 29;
const ENOSYS: u64 = 38;
const ERANGE: u64 = 34;
const EIO: u64 = 5;
const EAFNOSUPPORT: u64 = 97;
const ENOTSOCK: u64 = 88;
const EACCES: u64 = 13;
const EADDRINUSE: u64 = 98;
const EADDRNOTAVAIL: u64 = 99;
const ENOMEM: u64 = 12;
const ENODEV: u64 = 19;
const ECONNREFUSED: u64 = 111;
const EINPROGRESS: u64 = 115; // a non-blocking connect that has not finished yet
const EISCONN: u64 = 106; // connect on a socket that already has a peer
const ENXIO: u64 = 6; // open() of a bound AF_UNIX socket path
const EFAULT: u64 = 14; // a guest pointer outside the arena

// Linux address families / socket types.
const AF_INET: u64 = 2;
const AF_INET6: u64 = 10;
const SOCK_STREAM: u64 = 1;
const SOCK_DGRAM: u64 = 2;
const SOCK_TYPE_MASK: u64 = 0xff; // low bits; SOCK_NONBLOCK/CLOEXEC are higher
const AF_UNIX: u32 = 1; // entirely in-runtime; there is no host object for these
const EFD_SEMAPHORE: u32 = 1;
const EFD_CLOEXEC: u32 = 0o2000000;

// struct msghdr / struct cmsghdr, aarch64 LP64. Offsets are spelled out because
// sendmsg/recvmsg parse guest memory by hand and a wrong one is silent.
const MSGHDR_SIZE: usize = 56;
const MSGHDR_IOV: usize = 16; // struct iovec *msg_iov
const MSGHDR_IOVLEN: usize = 24; // size_t msg_iovlen
const MSGHDR_CONTROL: usize = 32; // void *msg_control
const MSGHDR_CONTROLLEN: usize = 40; // size_t msg_controllen
const MSGHDR_FLAGS: usize = 48; // int msg_flags
const CMSGHDR_SIZE: usize = 16; // size_t cmsg_len; int cmsg_level; int cmsg_type
const SOL_SOCKET: u32 = 1;
const SCM_RIGHTS: u32 = 1;
const MSG_CTRUNC: u32 = 8;
const FIONBIO: u64 = 0x5421; // asm-generic/ioctls.h
const FIOASYNC: u64 = 0x5452; // asm-generic/ioctls.h -- arm SIGIO on the fd

// WasmEdge socket-extension enum values (see NETWORKING.md).
// WasmEdge's socket-option enum; see `we_sockopt` for how these were established.
// Linux asm-generic/socket.h, which aarch64 uses. SOL_SOCKET is already
// defined above for the SCM_RIGHTS cmsg path.

// WASI preview1 wire constants used for cooperative socket scheduling.
const WASI_ACCES: u32 = 2; // ErrnoAcces
const WASI_ADDRINUSE: u32 = 3; // ErrnoAddrinuse
const WASI_ADDRNOTAVAIL: u32 = 4; // ErrnoAddrnotavail

use crate::context::AT_FDCWD;
const AT_SYMLINK_NOFOLLOW: u64 = 0x100;
const AT_EMPTY_PATH: u64 = 0x1000;

// Linux open flags.
const O_WRONLY: u64 = 1;
const O_RDWR: u64 = 2;
const O_CREAT: u64 = 0o100;
const O_TRUNC: u64 = 0o1000;
const O_NOFOLLOW: u64 = 0o400000;

// File type bits.
const S_IFDIR: u32 = 0o040000;
const S_IFREG: u32 = 0o100000;
const S_IFLNK: u32 = 0o120000;
const S_IFCHR: u32 = 0o020000;
const S_IFIFO: u32 = 0o010000;
const S_IFSOCK: u32 = 0o140000;

const PR_SET_PDEATHSIG: u64 = 1;
const PR_GET_DUMPABLE: u64 = 3;
const PR_SET_DUMPABLE: u64 = 4;
const PR_SET_NAME: u64 = 15;
const PR_GET_NAME: u64 = 16;
const PR_SET_THP_DISABLE: u64 = 41;
const PR_GET_THP_DISABLE: u64 = 42;
// "SVMA" in ASCII, and not a small ordinal like every other option, because it
// was added as an out-of-band namespace with its own sub-option in arg2.
const PR_SET_VMA: u64 = 0x53564d41;
const PR_SET_VMA_ANON_NAME: u64 = 0;

// rt_sigprocmask `how` lives in `context` (SIG_BLOCK/SIG_UNBLOCK/SIG_SETMASK),
// next to `sigprocmask_next`, so the ruling it selects is host-testable.

// mmap flags (aarch64/asm-generic). MAP_SHARED marks the segment exempt from
// the per-process arena restore (inter-process shared memory); MAP_ANONYMOUS +
// fd == -1 is the anonymous-shared form postgres/shm uses.
const MAP_SHARED: u64 = 0x01;
const MAP_ANONYMOUS: u64 = 0x20;
const MAP_FIXED: u64 = 0x10;

// mmap/mprotect protection bits. The flat arena enforces NONE of them -- there
// is no page table below to hang a permission on -- so PROT_WRITE is read for
// exactly one purpose: it is the only evidence that a byte MIGHT have been
// written through a file-backed MAP_SHARED region, which is what decides whether
// that region may be recycled. See `context::shm_file_reclaimable`.
const PROT_WRITE: u64 = 0x2;

// futex ops (aarch64/asm-generic). The command is `op & FUTEX_CMD_MASK`, which
// strips FUTEX_PRIVATE_FLAG (128) and FUTEX_CLOCK_REALTIME (256); ecvisor treats
// private and shared identically (a futex is matched by its guest uaddr / VMA,
// and a word in a MAP_SHARED segment is the same physical bytes for every
// process). See sys_futex and .agents/docs/SHAREDMEM.md.
const FUTEX_CMD_MASK: u64 = 0x7f;
/// Selects CLOCK_REALTIME for an ABSOLUTE `FUTEX_WAIT_BITSET` timeout; without
/// it the deadline is on CLOCK_MONOTONIC. Masked out of `cmd`, so `sys_futex`
/// reads it from the raw op word.
const FUTEX_CLOCK_REALTIME: u64 = 256;
const FUTEX_WAIT: u64 = 0;
const FUTEX_WAKE: u64 = 1;
const FUTEX_WAIT_BITSET: u64 = 9;
const FUTEX_WAKE_BITSET: u64 = 10;

extern "C" {
    fn write(fd: i32, buf: *const u8, count: usize) -> isize;
    fn read(fd: i32, buf: *mut u8, count: usize) -> isize;
}

/// The WasmEdge socket extension, the `WasiAddress` form, the raw `fd_read`/
/// `fd_write`/`poll_oneoff` declarations and the two host-poll helpers all
/// moved to `crate::net::wasmedge` when the backend seam landed. This module
/// no longer knows which host it is talking to, which is the point: the same
/// syscall code now serves the WasmEdge host, an in-process loopback network
/// and (next) a browser.
///
/// The libc `write`/`read`/`close` above stay, because they are for STDIO and
/// have nothing to do with sockets.

/// Entry from `__remill_syscall_tranpoline_call`.
///
/// Runs the syscall dispatch and consumes `ctx.resuming` (a resumed handler reads
/// it to finish rather than re-block, then any further syscall in the same frame is
/// fresh). It no longer touches the stack: if a handler set `ctx.suspended`, the
/// trampoline (our caller) captures the replay state and EH-unwinds via `ecv_yield`
/// back to the scheduler — see intrinsics.rs. This replaces the old asyncify
/// unwind/rewind, which is mutually exclusive with --fork_emulation codegen
/// (.agents/docs/DYNLINK.md).
pub fn svc(_arena_ptr: *mut u8, state: &mut State, ctx: &mut EcvContext) {
    svc_dispatch(state, ctx);
    ctx.resuming = false;
}

fn svc_dispatch(state: &mut State, ctx: &mut EcvContext) {
    let nr = state.syscall_nr();
    // A deadline belongs to ONE wait. A FRESH syscall therefore starts with none,
    // and whichever handler wants a timeout arms it below; a RESUMED one keeps
    // what it armed, because the wake that brought it back may have been spurious
    // and its timeout is still running.
    //
    // Without the clear, a process woken normally from a timed `futex` carried
    // that deadline into its next block, and if that block armed none of its own
    // (`BlockedOn::Wait`, `PipeRead`) the idle sweep released it immediately with
    // `timed_out` set -- a spurious wake plus a flag the next `sys_futex` resume
    // reads as an `ETIMEDOUT` that never happened.
    if !ctx.resuming {
        ctx.set_current_deadline(None);
    }
    // The pid is not decoration. With more than one process alive the trace is
    // otherwise an interleaving of indistinguishable streams, and the question it
    // gets used for -- "did THIS process reach its event loop" -- cannot be
    // answered at all. nginx master/worker is the first guest where that bites.
    // It is no longer formatted here: the subscriber appends it to every
    // DEBUG/TRACE line from `trace::CURRENT_PID`.
    ecv_trace!(
        ecv,
        "svc nr={} args=({:#x},{:#x},{:#x},{:#x}) pc={:#x}",
        nr,
        state.arg(0),
        state.arg(1),
        state.arg(2),
        state.arg(3),
        state.pc()
    );
    match nr {
        // ioctl is ENOTTY by default -- there are no terminals and no drivers --
        // with two exceptions, both of which nginx treats as fatal on failure.
        //
        // FIONBIO is how nginx puts a socket into non-blocking mode
        // (`ngx_nonblocking` is `ioctl(s, FIONBIO, &nb)` on Linux), and it checks
        // the result: a failure is logged and the connection or the whole
        // listening socket is dropped.
        //
        // This used to accept the call and record NOTHING, reasoning that a
        // would-block on a host socket suspends cooperatively and the scheduler
        // polls the fd, so there was no blocking mode to switch off. True of host
        // sockets; false of an internal socketpair, where nothing external will
        // ever make the fd readable. nginx's workers hung exactly there -- each
        // drains its channel, issues one more `recvmsg` expecting EAGAIN, and
        // parked forever without reaching `epoll_pwait`. The flag is recorded now
        // and honoured by the pipe/socketpair paths.
        //
        // FIOASYNC arms SIGIO on a descriptor. `ngx_spawn_process` sets it on the
        // master's end of each worker channel and returns NGX_INVALID_PID if it
        // fails -- BEFORE the fork -- so ENOTTY here does not degrade master mode,
        // it prevents any worker from being created at all:
        //
        //     [alert] 1#1: ioctl(FIOASYNC) failed while spawning "worker process"
        //
        // repeated once per configured worker, then a master listening alone and
        // answering nothing. Accepting it is honest as far as it goes: we do not
        // deliver SIGIO, but nginx does not depend on it. The channel is also
        // registered with the worker's epoll (`ngx_add_channel_event`), which is
        // the path that actually carries commands; SIGIO is a legacy wakeup.
        NR_IOCTL => match state.arg(1) {
            FIONBIO => {
                // The third argument is a pointer to an int: non-zero enables.
                let on = u32::from_le_bytes(ctx.arena.slice(state.arg(2), 4).try_into().unwrap());
                ctx.set_nonblock(state.arg(0) as usize, on != 0);
                state.set_ret(0);
            }
            FIOASYNC => state.set_ret(0),
            _ => state.set_ret_err(ENOTTY),
        },
        NR_OPENAT => {
            sys_openat(state, ctx);
            fdcheck_after(ctx, "openat");
        }
        NR_CLOSE => {
            sys_close(state, ctx);
            fdcheck_after(ctx, "close");
        }
        NR_UNLINKAT => sys_unlinkat(state, ctx),
        NR_RENAMEAT => sys_renameat(state, ctx),
        NR_TRUNCATE => sys_truncate(state, ctx),
        NR_FTRUNCATE => sys_ftruncate(state, ctx),
        NR_FALLOCATE => sys_fallocate(state, ctx),
        // set_robust_list(head, len): records the thread's robust-futex list head
        // for kernel cleanup on exit. We model one thread per process and never do
        // that cleanup, so accepting it (return 0) is correct; glibc calls it during
        // thread bring-up and treats ENOSYS as fatal on some paths.
        NR_SET_ROBUST_LIST => state.set_ret(0),
        NR_READ => sys_read(state, ctx),
        NR_WRITE => sys_write(state, ctx),
        NR_READV => iovec_loop(state, ctx, false),
        NR_WRITEV => iovec_loop(state, ctx, true),
        NR_PREAD64 => sys_pread64(state, ctx),
        NR_PWRITE64 => sys_pwrite64(state, ctx),
        NR_LSEEK => sys_lseek(state, ctx),
        NR_FSTAT => sys_fstat(state, ctx),
        NR_NEWFSTATAT => sys_newfstatat(state, ctx),
        NR_GETDENTS64 => sys_getdents64(state, ctx),
        NR_GETCWD => sys_getcwd(state, ctx),
        NR_READLINKAT => sys_readlinkat(state, ctx),
        NR_FACCESSAT | NR_FACCESSAT2 => sys_faccessat(state, ctx),
        NR_CHDIR => sys_chdir(state, ctx),
        NR_FCHDIR => sys_fchdir(state, ctx),
        NR_SCHED_GETAFFINITY => {
            // sched_getaffinity(pid, cpusetsize, mask): report a single online CPU
            // (0). Without this, libnuma's max-cpu probe gets ENOSYS, "guesses", and
            // dereferences a wild pointer (postgres OOB, JOURNAL 3q). The raw syscall
            // writes the kernel cpumask (bit 0 set) into `mask` and returns the number
            // of bytes written; glibc's wrapper zeroes the remainder up to cpusetsize.
            let cpusetsize = state.arg(1) as usize;
            let mask_ptr = state.arg(2);
            if cpusetsize < 8 || mask_ptr == 0 {
                state.set_ret_err(EINVAL);
            } else {
                let n = cpusetsize.min(1024);
                let buf = ctx.arena.slice_mut(mask_ptr, n);
                buf.fill(0);
                buf[0] = 0x01; // CPU 0 online
                state.set_ret(8); // bytes of the kernel cpumask written
            }
        }
        NR_SCHED_YIELD => sys_sched_yield(state, ctx),
        NR_CLONE => sys_clone(state, ctx),
        NR_WAIT4 => sys_wait4(state, ctx),
        NR_PIPE2 => sys_pipe2(state, ctx),
        NR_DUP => {
            sys_dup(state, ctx);
            fdcheck_after(ctx, "dup");
        }
        NR_DUP3 => {
            sys_dup3(state, ctx);
            fdcheck_after(ctx, "dup3");
        }
        NR_FCNTL => sys_fcntl(state, ctx),
        NR_EXECVE => sys_execve(state, ctx),
        NR_SOCKET => sys_socket(state, ctx),
        NR_BIND => sys_bind(state, ctx),
        NR_LISTEN => sys_listen(state, ctx),
        NR_ACCEPT | NR_ACCEPT4 => sys_accept(state, ctx),
        NR_CONNECT => sys_connect(state, ctx),
        NR_GETSOCKNAME => sys_getsockname(state, ctx, false),
        NR_GETPEERNAME => sys_getsockname(state, ctx, true),
        NR_SENDTO => sys_sendto(state, ctx),
        NR_RECVFROM => sys_recvfrom(state, ctx),
        NR_SETSOCKOPT => sys_setsockopt(state, ctx),
        NR_GETSOCKOPT => sys_getsockopt(state, ctx),
        NR_SHUTDOWN => sys_shutdown(state, ctx),
        NR_EXIT => sys_exit(state, ctx, false),
        NR_EXIT_GROUP => sys_exit(state, ctx, true),
        // set_tid_address(ptr): record the address the kernel must zero and
        // futex-wake when this task dies, and return the caller's tid. The
        // kernel does NOT write through the pointer here -- both libcs take the
        // tid from the return value -- but we keep the write because glibc's
        // static startup path reads `pd->tid` back without using the result.
        NR_SET_TID_ADDRESS => {
            let tid = ctx.current_pid();
            ctx.set_clear_child_tid(state.arg(0));
            let p = ctx.arena.translate(state.arg(0)) as *mut u32;
            unsafe { p.write_unaligned(tid) };
            state.set_ret(tid as u64);
        }
        NR_FUTEX => sys_futex(state, ctx),
        NR_RT_SIGACTION => sys_rt_sigaction(state, ctx),
        NR_RT_SIGPROCMASK => sys_rt_sigprocmask(state, ctx),
        NR_SHMGET => sys_shmget(state, ctx),
        NR_SHMAT => sys_shmat(state, ctx),
        NR_SHMDT => sys_shmdt(state, ctx),
        NR_SHMCTL => sys_shmctl(state, ctx),
        NR_SIGNALFD4 => sys_signalfd4(state, ctx),
        NR_EVENTFD2 => sys_eventfd2(state, ctx),
        NR_SOCKETPAIR => sys_socketpair(state, ctx),
        NR_SENDMSG => sys_sendmsg(state, ctx),
        NR_RECVMSG => sys_recvmsg(state, ctx),
        NR_SENDFILE => sys_sendfile(state, ctx),
        NR_MKDIRAT => sys_mkdirat(state, ctx),
        NR_RT_SIGSUSPEND => sys_rt_sigsuspend(state, ctx),
        NR_RT_SIGTIMEDWAIT => sys_rt_sigtimedwait(state, ctx),
        // preadv/pwritev (69/70) and preadv2/pwritev2 (286/287) share the first
        // four arguments; the v2 forms only add a `flags` word this handler does
        // not consult. Only the v2 numbers were wired up, so PostgreSQL's WAL
        // pre-allocation -- `pg_pwrite_zeros`, which uses plain pwritev -- got
        // ENOSYS and initdb died with
        //   FATAL: could not write to file "pg_wal/xlogtemp.11": Function not implemented
        NR_PREADV | NR_PREADV2 => sys_preadv2(state, ctx, false),
        NR_PWRITEV | NR_PWRITEV2 => sys_preadv2(state, ctx, true),
        // Credentials. There is no permission model -- one arena, one VFS with no
        // ownership -- so these only have to be believed, not enforced. Recording
        // the new ids (rather than returning 0 and staying root) keeps getuid
        // honest afterwards, which is the difference between a worker that has
        // dropped privileges and one that merely thinks it has.
        //
        // nginx is the caller that matters: its master runs as root, and each
        // worker does setgid/setgroups/setuid before serving. An ENOSYS here is
        // fatal to the worker, and the resulting "setuid(101) failed (38:
        // Function not implemented)" is what a missing syscall looks like from
        // the guest side. (⚠️ This cited `.agents/docs/JOURNAL.md`; that account
        // is gone from JOURNAL.md and LTM/ alike as of 2026-08-25, so the
        // symptom quoted above is now the record.)
        NR_SETUID => {
            ctx.uid = state.arg(0) as u32;
            state.set_ret(0);
        }
        NR_SETGID => {
            ctx.gid = state.arg(0) as u32;
            state.set_ret(0);
        }
        // setresuid/setresgid(ruid, euid, suid): -1 means "leave unchanged", and
        // the effective id is the one anything else observes.
        NR_SETRESUID => {
            if state.arg(1) as i32 != -1 {
                ctx.uid = state.arg(1) as u32;
            }
            state.set_ret(0);
        }
        NR_SETRESGID => {
            if state.arg(1) as i32 != -1 {
                ctx.gid = state.arg(1) as u32;
            }
            state.set_ret(0);
        }
        // Supplementary groups are not modeled at all. setgroups accepts and
        // discards; getgroups reports an empty set, which is a truthful answer to
        // "which supplementary groups am I in" given we never joined any.
        NR_SETGROUPS => state.set_ret(0),
        NR_GETGROUPS => state.set_ret(0),
        // Ownership and mode. The VFS carries neither, so these succeed without
        // recording anything. That is a deliberate lie with a bounded blast
        // radius: nothing in the runtime ever consults an owner or a mode, so
        // there is no later reader to mislead. Returning EPERM instead would kill
        // nginx, which chowns its temp directories to the worker user at startup.
        // Ownership is not modelled (the VFS reports the running uid/gid), so
        // chown succeeds as a no-op. chmod is NOT a no-op: a silent success
        // there made initdb's `chmod(PGDATA, 0700)` change nothing, and the
        // postmaster then died with `data directory ... has invalid
        // permissions` reading back the image's 0775.
        NR_FCHOWN | NR_FCHOWNAT => state.set_ret(0),
        NR_FCHMOD | NR_FCHMODAT => sys_fchmodat(state, ctx),
        // See the constants: these must fail, and their callers handle it.
        NR_IO_SETUP => state.set_ret_err(ENOSYS),
        // rseq(rseq_ptr, rseq_len, flags, sig): restartable sequences. ACCEPTED,
        // and the acceptance is honest rather than a stub -- this runtime has
        // exactly one CPU and never preempts a guest between two instructions,
        // so every rseq critical section runs to completion and the current CPU
        // is always 0. Registration is the whole of what the kernel does that
        // the guest can observe here.
        //
        // ENOSYS was not survivable. glibc has a new thread INHERIT the parent's
        // registration state and then abort if its own registration fails:
        //
        //   Fatal glibc error: rseq registration failed
        //
        // which killed the first pthread_create in a fused dynamic glibc image.
        // A fused image never runs ld.so, so the main thread's rseq area was
        // never initialised to "unregistered" either -- accepting the call is
        // both simpler and truer than trying to make the guest believe rseq is
        // unavailable.
        NR_RSEQ => {
            let (ptr, len, flags) = (state.arg(0), state.arg(1) as usize, state.arg(2));
            const RSEQ_FLAG_UNREGISTER: u64 = 1;
            // struct rseq is 32 bytes: cpu_id_start, cpu_id, rseq_cs, flags.
            if ptr == 0 || len < 32 || !ctx.arena.in_bounds(ptr, 32) {
                state.set_ret_err(EINVAL);
            } else if flags & RSEQ_FLAG_UNREGISTER != 0 {
                // Unregistering leaves the area alone but must report the CPU as
                // no longer valid, the way the kernel does.
                let b = ctx.arena.slice_mut(ptr, 8);
                b[0..4].copy_from_slice(&(-1i32).to_le_bytes());
                b[4..8].copy_from_slice(&(-1i32).to_le_bytes());
                state.set_ret(0);
            } else {
                // cpu_id_start and cpu_id both 0: one CPU, and it is CPU 0.
                let b = ctx.arena.slice_mut(ptr, 8);
                b.fill(0);
                state.set_ret(0);
            }
        }
        NR_EPOLL_CREATE1 => sys_epoll_create1(state, ctx),
        NR_EPOLL_CTL => sys_epoll_ctl(state, ctx),
        NR_EPOLL_PWAIT => sys_epoll_pwait(state, ctx),
        NR_PPOLL => sys_ppoll(state, ctx),
        // kill(pid, sig) / tgkill(tgid, tid, sig) / tkill(tid, sig). Threads are
        // not modeled, so a tid IS a pid and tgkill's tid argument is the target.
        NR_KILL => sys_kill(state, ctx, state.arg(0) as i64, state.arg(1) as u32, false),
        // tkill/tgkill name a TASK, and that distinction is the whole of
        // pthread_kill: the signal must land on that thread's queue and on no
        // other thread's. tgkill's tid is arg 1; its arg 0 is the tgid.
        NR_TKILL => sys_kill(state, ctx, state.arg(0) as i64, state.arg(1) as u32, true),
        NR_TGKILL => sys_kill(state, ctx, state.arg(1) as i64, state.arg(2) as u32, true),
        // umask: no VFS permission model, so report a conventional prior mask
        // (022) and ignore the new one. postgres calls this during startup.
        NR_UMASK => state.set_ret(0o022),
        // get_mempolicy: libnuma probe. Report "default policy" success rather
        // than ENOSYS so libnuma stops guessing (cf. sched_getaffinity).
        NR_GET_MEMPOLICY => {
            let mode = state.arg(0);
            if mode != 0 {
                ctx.arena.slice_mut(mode, 4).fill(0); // MPOL_DEFAULT
            }
            state.set_ret(0);
        }
        // No signal is delivered on the builtin-shell path, so sigreturn is
        // never actually reached; a success stub keeps a defensive caller from
        // seeing ENOSYS. (A real delivery machine would restore the saved
        // register frame here.)
        NR_RT_SIGRETURN => state.set_ret(0),
        NR_NANOSLEEP => sys_nanosleep(state, ctx),
        NR_CLOCK_NANOSLEEP => sys_clock_nanosleep(state, ctx),
        // clock_gettime(clockid, tp).
        //
        // ⚠️ `clockid` USED TO BE IGNORED. Every clock was answered from the wall
        // clock, which made `CLOCK_MONOTONIC` step whenever the host's clock did
        // -- an NTP correction, a laptop suspend, a backgrounded browser tab --
        // and made `CLOCK_PROCESS_CPUTIME_ID` report the epoch as CPU time.
        //
        // Measured on a SERVING nginx: six of the 27.6 syscalls a request costs,
        // as three adjacent pairs (`ngx_time_update`, once per epoll wake). So
        // the mapping is a table lookup and the read is one branch -- but do not
        // read that as "this is hot". At 208 ns a call it is ~0.13% of a request,
        // and a cache was built, measured and REJECTED on 2026-08-21. See the
        // journal before proposing one.
        NR_CLOCK_GETTIME => match crate::context::clock_read(state.arg(0)) {
            Some(ns) => {
                let tp = ctx.arena.slice_mut(state.arg(1), 16);
                tp[..8].copy_from_slice(&((ns / 1_000_000_000) as u64).to_le_bytes());
                tp[8..].copy_from_slice(&((ns % 1_000_000_000) as u64).to_le_bytes());
                state.set_ret(0);
            }
            None => state.set_ret_err(EINVAL),
        },
        // clock_getres(clockid, res). Previously unimplemented, so it reached the
        // ENOSYS advisory -- and a guest reads the resolution to decide how to
        // round its own timers, which makes a missing answer worse than a coarse
        // one. Both timebases are nanosecond `u128` counters all the way to the
        // host call, so 1 ns is the truthful answer for every clock that exists;
        // what the HOST hands back is coarser (a browser deliberately coarsens
        // `performance.now()` against Spectre) but that is a property of the
        // deployment, not of the clock the guest is asking about.
        //
        // `res` may be NULL: Linux uses that as a pure existence check for the
        // clock id, and glibc's `clock_getcpuclockid` relies on it.
        NR_CLOCK_GETRES => match crate::context::clock_base(state.arg(0)) {
            Some(_) => {
                let res = state.arg(1);
                if res != 0 {
                    let out = ctx.arena.slice_mut(res, 16);
                    out[..8].copy_from_slice(&0u64.to_le_bytes());
                    out[8..].copy_from_slice(&1u64.to_le_bytes());
                }
                state.set_ret(0);
            }
            None => state.set_ret_err(EINVAL),
        },
        NR_GETTIMEOFDAY => {
            let now = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default();
            let tv = ctx.arena.slice_mut(state.arg(0), 16);
            tv[..8].copy_from_slice(&now.as_secs().to_le_bytes());
            tv[8..].copy_from_slice(&(now.subsec_micros() as u64).to_le_bytes());
            state.set_ret(0);
        }
        NR_UNAME => {
            let buf = ctx.arena.slice_mut(state.arg(0), 65 * 5);
            buf.fill(0);
            for (i, s) in [
                &b"Linux"[..],
                b"raptormark",
                b"6.0.0-00-generic",
                b"#0~raptormark",
                b"aarch64",
            ]
            .iter()
            .enumerate()
            {
                buf[i * 65..i * 65 + s.len()].copy_from_slice(s);
            }
            state.set_ret(0);
        }
        NR_GETRUSAGE => {
            ctx.arena.slice_mut(state.arg(1), 144).fill(0);
            state.set_ret(0);
        }
        NR_PRCTL => {
            match state.arg(0) {
                PR_GET_NAME => {
                    let buf = ctx.arena.slice_mut(state.arg(1), 16);
                    buf.fill(0);
                    buf[..3].copy_from_slice(b"app");
                    state.set_ret(0);
                }
                // PR_SET_PDEATHSIG(sig): request a signal when the parent dies. In the
                // cooperative single-address-space model the whole process tree lives or
                // dies together, so parent-death delivery is moot -- but postgres's child
                // startup treats EINVAL here as fatal ("could not request parent death
                // signal"). Accept it as a no-op so workers get past prctl. PR_SET_NAME
                // (per-thread comm name) is likewise accepted; we do not track it.
                PR_SET_PDEATHSIG | PR_SET_NAME => state.set_ret(0),
                // PR_SET_DUMPABLE(v) / PR_GET_DUMPABLE: whether a core dump is
                // written for this process, and whether another process of the
                // same uid may ptrace it.
                //
                // Accepting it is the TRUTHFUL answer, not a convenience.
                // ecvisor writes no core dumps and offers no ptrace, so the
                // flag has no observable effect either way -- there is nothing
                // for it to enable and therefore nothing to lie about. EINVAL
                // would be the lie: it says "not a valid request", and on Linux
                // it is an ordinary one that returns 0, which is why each nginx
                // worker logged `prctl(PR_SET_DUMPABLE) failed`.
                //
                // ⚠️ This does NOT claim a core-dump facility. Nothing here
                // produces a dump, and `WCOREDUMP` is correspondingly not
                // claimed in any wait status: setting the flag and refusing to
                // invent the dump it governs is the pair that stays honest.
                //
                // The value is recorded, not discarded, because `PR_GET_DUMPABLE`
                // must answer with what was stored -- a SET that "succeeds" and
                // a GET that reports something else is a worse divergence than
                // the EINVAL this replaces. Validation is Linux's: exactly 0 or
                // 1 (see `context::dumpable_arg_permitted`), so an out-of-range
                // request still gets the EINVAL Linux gives it.
                PR_SET_DUMPABLE => {
                    let v = state.arg(1);
                    if crate::context::dumpable_arg_permitted(v) {
                        ctx.set_dumpable(v);
                        state.set_ret(0);
                    } else {
                        state.set_ret_err(EINVAL);
                    }
                }
                // Returned as the syscall's VALUE, not through a pointer.
                PR_GET_DUMPABLE => state.set_ret(ctx.dumpable()),
                // PR_SET_THP_DISABLE(v) / PR_GET_THP_DISABLE: whether this mm's
                // anonymous VMAs may be backed by transparent huge pages.
                //
                // Accepting it is the TRUTHFUL answer. The flag withholds a
                // kernel OPTIMISATION, and ecvisor has no page tables, no fault
                // handler and no huge pages -- guest memory is one flat wasm
                // linear-memory arena, so "do not give me huge pages" is a
                // request that is already satisfied and can never stop being.
                // EINVAL says "not a valid request", and on Linux it is an
                // ordinary one that returns 0; ruby's `ruby_setup` calls it on
                // EVERY startup, so every ruby run took the divergence.
                //
                // ⚠️ This claims NO huge-page facility in either direction.
                // Nothing here maps a huge page, and `PR_GET_THP_DISABLE`
                // reading 0 means "nobody asked to disable it", not "THP is
                // available" -- see `context::THP_NOT_DISABLED`.
                //
                // Validation is Linux's, and it is NOT the shape PR_SET_DUMPABLE
                // has: arg2 is unvalidated and stored as a TRUTHINESS
                // (`thp_disable_value`), while arg3..arg5 are reserved and must
                // be zero (`thp_disable_set_permitted`) -- the getter reserves
                // arg2 as well. Each measured on Linux 6.17.
                PR_SET_THP_DISABLE => {
                    if crate::context::thp_disable_set_permitted(
                        state.arg(2),
                        state.arg(3),
                        state.arg(4),
                    ) {
                        ctx.set_thp_disable(crate::context::thp_disable_value(state.arg(1)));
                        state.set_ret(0);
                    } else {
                        state.set_ret_err(EINVAL);
                    }
                }
                // Stored rather than discarded for the same reason as
                // PR_SET_DUMPABLE: a SET that succeeds and a GET that reports
                // something else is a worse divergence than the EINVAL both
                // replace. Returned as the syscall's VALUE.
                PR_GET_THP_DISABLE => {
                    if crate::context::thp_disable_get_permitted(
                        state.arg(1),
                        state.arg(2),
                        state.arg(3),
                        state.arg(4),
                    ) {
                        state.set_ret(ctx.thp_disable());
                    } else {
                        state.set_ret_err(EINVAL);
                    }
                }
                // prctl(PR_SET_VMA, PR_SET_VMA_ANON_NAME, addr, len, name):
                // label an anonymous mapping. ruby names every GC heap page
                // (`heap_page_allocate_and_initialize` passes
                // "Ruby:GC:default:heap_page_body_allocate"), so this recurs for
                // the life of a ruby process.
                //
                // Accepting it is the TRUTHFUL answer, and here the reason is
                // sharper than for the two flags above: the name has exactly ONE
                // observable effect on Linux -- it is printed as `[anon:NAME]`
                // in `/proc/<pid>/maps` and `smaps` -- and ecvisor HAS NO
                // `/proc` AT ALL (`vfs/` is rfs + tmpfs; nothing synthesises a
                // procfs). So the name is not merely unused, it is unreadable,
                // and the guest has no way to ask a question this answer would
                // be wrong to.
                //
                // ⚠️ THE NAME IS DELIBERATELY NOT STORED. That is the opposite
                // of the PR_SET_DUMPABLE decision, and the reason is the
                // opposite too: `PR_GET_DUMPABLE` exists, so a stored value had
                // a reader, whereas there is no PR_GET_VMA_ANON_NAME (measured:
                // prctl(0x53564d42) is EINVAL) and no /proc for it to surface
                // in. Keeping a per-range name table nothing can ever read back
                // would be state pretending to be a facility -- and the moment
                // a /proc/<pid>/maps exists here, that table has to be built
                // from the mapping list anyway, not from this.
                //
                // What IS enforced is every rule the guest can still observe
                // through the return value, in Linux's order (measured, not read
                // off): sub-option, then the name, then the range.
                PR_SET_VMA => {
                    let (opt, addr, len, name_ptr) =
                        (state.arg(1), state.arg(2), state.arg(3), state.arg(4));
                    // Compared as the full u64: `usize` is 32-bit on wasm32, so
                    // `opt as usize` would fold 0x1_0000_0000 onto the valid 0.
                    // Measured: prctl(PR_SET_VMA, 0x100000000, ...) is EINVAL.
                    if opt != PR_SET_VMA_ANON_NAME {
                        state.set_ret_err(EINVAL);
                        return;
                    }
                    // A NULL name CLEARS the label and is never dereferenced;
                    // everything else is read and validated BEFORE the range, so
                    // a bad name at an unmapped address is EINVAL/EFAULT rather
                    // than the ENOMEM the range alone would give.
                    if name_ptr != 0 {
                        if !ctx.arena.in_bounds(name_ptr, 1) {
                            state.set_ret_err(EFAULT);
                            return;
                        }
                        let (name, terminated) = ctx
                            .arena
                            .read_cstr_capped(name_ptr, crate::arena::ANON_NAME_MAX_LEN + 1);
                        if !terminated {
                            // No NUL in the 80 bytes the kernel reads. If all 80
                            // were readable that is `strndup_user`'s length
                            // refusal (EINVAL); if the arena ended first, the
                            // kernel would have faulted instead.
                            let readable = ctx
                                .arena
                                .in_bounds(name_ptr, crate::arena::ANON_NAME_MAX_LEN + 1);
                            state.set_ret_err(if readable { EINVAL } else { EFAULT });
                            return;
                        }
                        if !crate::arena::anon_name_permitted(&name) {
                            state.set_ret_err(EINVAL);
                            return;
                        }
                    }
                    match crate::arena::anon_name_range(addr, len) {
                        crate::arena::AnonNameRange::Invalid => state.set_ret_err(EINVAL),
                        crate::arena::AnonNameRange::Empty => state.set_ret(0),
                        // ⚠️ ENOMEM only for a range OUTSIDE the arena, which is
                        // the only "not mapped" this runtime can honestly
                        // assert: everything the guest can address is the arena,
                        // but WITHIN it the bump allocator cannot tell a live
                        // mapping from a hole -- the same limit NR_MREMAP states
                        // for growing in place. Naming an in-arena hole
                        // therefore succeeds where Linux gives ENOMEM, which is
                        // the one divergence left here and is stated rather than
                        // guessed at. EBADF (Linux's answer for a file-backed
                        // VMA) is likewise unreachable: the arena has no
                        // file-backed mappings to distinguish.
                        crate::arena::AnonNameRange::Range { start, end } => {
                            if crate::arena::range_in_arena(start, end) {
                                state.set_ret(0);
                            } else {
                                state.set_ret_err(ENOMEM);
                            }
                        }
                    }
                }
                _ => state.set_ret_err(EINVAL),
            }
        }
        // getpid is the thread GROUP id and gettid the task id. They coincide
        // for every single-threaded guest, which is why one arm served both
        // until threads existed -- and they must not now, because a library
        // that compares `gettid() == getpid()` to ask "am I the main thread"
        // would get yes from every worker.
        NR_GETPID => state.set_ret(ctx.current_tgid() as u64),
        NR_GETTID => state.set_ret(ctx.current_pid() as u64),
        NR_GETPPID => state.set_ret(ctx.procs[ctx.current].ppid as u64),
        NR_GETUID | NR_GETEUID => state.set_ret(ctx.uid as u64),
        NR_GETGID | NR_GETEGID => state.set_ret(ctx.gid as u64),
        // Process groups / sessions. The cooperative model has one process tree and
        // does not track separate groups/sessions, so report each process as its own
        // group and session leader (pgid/sid == pid) and accept the setters. This is
        // enough for a shell's job-control init (bash's getpgrp()==getpgid(0)) and
        // for a program calling setsid()/setpgid() defensively; ioctl(TIOC[GS]PGRP)
        // stays ENOTTY (no controlling terminal), so interactive job control is off.
        NR_GETPGID | NR_GETSID => {
            let pid = state.arg(0) as u32;
            let p = if pid == 0 { ctx.current_pid() } else { pid };
            state.set_ret(p as u64);
        }
        NR_SETPGID => state.set_ret(0),
        NR_SETSID => state.set_ret(ctx.current_pid() as u64),
        // prlimit64(pid, resource, new, old): we do not enforce resource limits, but
        // a shell reads them at startup (glibc getrlimit lowers to this). Report a
        // bounded RLIMIT_NOFILE (7) so a program sizing fd_sets stays sane, and
        // "unlimited" elsewhere; ignore any new limit.
        NR_PRLIMIT64 => {
            let (resource, old) = (state.arg(1), state.arg(3));
            if old != 0 {
                const RLIM_INFINITY: u64 = u64::MAX;
                const RLIMIT_NOFILE: u64 = 7;
                let (cur, max) = if resource == RLIMIT_NOFILE {
                    (1024u64, 4096u64)
                } else {
                    (RLIM_INFINITY, RLIM_INFINITY)
                };
                let b = ctx.arena.slice_mut(old, 16);
                b[0..8].copy_from_slice(&cur.to_le_bytes());
                b[8..16].copy_from_slice(&max.to_le_bytes());
            }
            state.set_ret(0);
        }
        NR_BRK => {
            let addr = state.arg(0);
            if addr == 0 {
                state.set_ret(ctx.arena.brk_cur);
            } else if (BRK_START_VMA..BRK_END_VMA).contains(&addr) {
                // Through `set_brk`, which zeroes what a growth exposes.
                ctx.arena.set_brk(addr);
                // Linux returns the NEW break on success. This was returning the
                // right value only by accident -- x0 still held the requested
                // address because nothing had written it. Say it on purpose.
                state.set_ret(addr);
            } else {
                // Out of range: return the CURRENT break unchanged, which is how
                // Linux reports failure -- brk() answers with the break it ended
                // up at, and glibc's wrapper turns "not what I asked for" into
                // ENOMEM. malloc's response to that is to switch to mmap, so the
                // guest keeps running on a heap it allocates a different way.
                //
                // This used to be `fatal!`, the fifth instance of aborting where
                // the kernel returns an errno. It killed initdb's post-bootstrap
                // backend at brk(0x157d0000): the brk region is 96 MiB
                // ([0x0A000000, 0x10000000)) and the backend wanted ~190 MiB,
                // which is an ordinary condition with an ordinary fallback.
                ecv_debug!(
                    ecv,
                    "brk({addr:#x}) outside [{BRK_START_VMA:#x}, {BRK_END_VMA:#x}) -> refused, break stays {:#x}",
                    ctx.arena.brk_cur
                );
                state.set_ret(ctx.arena.brk_cur);
            }
        }
        // munmap(addr, len): a PRIVATE region cannot be reclaimed -- the per-process
        // mmap arena is a bump allocator -- so for those this stays a best-effort
        // no-op. The pages stay resident, which is invisible to the guest. postgres
        // unmaps DSM/shared segments during resize and shutdown and treats ENOSYS as
        // an error, so a plain 0 return keeps it happy.
        //
        // A SHARED region is different, and is the one case worth handling: it comes
        // from the global window, which is small (it starts where the private arena
        // ends) and is the resource that actually runs out. Drop this process's claim
        // and let the region go if it was the last.
        NR_MUNMAP => {
            let addr = state.arg(0);
            // Rounded exactly as `NR_MMAP` rounds a registration, so the two are
            // comparable: a guest that unmaps the length it asked for must match
            // the region that length created. A length Linux itself refuses
            // (zero, or one that overflows the rounding) becomes 0 here, which
            // matches no shared region and no private extent -- the same
            // nothing-happens the wrapping arithmetic gave it before, without
            // the wrap.
            let len = mmap_round_len(state.arg(1)).unwrap_or(0);
            // Match on the region's start, AND require the request to cover the
            // whole of it. A partial unmap would have to split the region, which
            // neither the allocator nor `adopt_shared_from` can express -- and
            // answering a partial request by freeing the WHOLE region would hand
            // back memory the guest still has mapped. Ignoring it leaks, which is
            // the safe direction.
            //
            // The start match alone is not that ignore: it drops a TAIL unmap,
            // which names no region's start, and used to accept a HEAD unmap of
            // any length as a full detach. See `SharedSeg::unmap_is_whole`.
            match ctx.shm_seg_at(addr) {
                Some(i) if ctx.shared_segments[i].unmap_is_whole(addr, len) => {
                    let pid = ctx.current_pid();
                    ctx.shared_segments[i].mappers.retain(|&p| p != pid);
                    ctx.shm_try_reclaim(i);
                }
                Some(i) => {
                    ecv_debug!(
                        ecv,
                        "munmap {addr:#x} len={len} covers part of the {}-byte shared \
                         region there -> ignored (leaked), the region stays mapped",
                        ctx.shared_segments[i].len
                    );
                }
                None => {
                    // PRIVATE. Reclaimed on an exact extent match; see
                    // `Arena::mmap_live`. This is the case malloc exercises
                    // constantly -- grow the arena, unmap the old one -- and
                    // ignoring it made a guest need twice its peak.
                    let freed = ctx.arena.mmap_release(addr, len);
                    ecv_debug!(
                        ecv,
                        "munmap {addr:#x} len={len} -> {}; bump {:#x}, {} hole(s)",
                        if freed {
                            "reclaimed"
                        } else {
                            "not an exact extent, leaked"
                        },
                        ctx.arena.mmap_cur,
                        ctx.arena.mmap_free.len()
                    );
                }
            }
            state.set_ret(0)
        }
        // setitimer/getitimer: accept and succeed as a no-op. PostgreSQL arms an
        // ITIMER_REAL/SIGALRM timer for its timeout infrastructure (statement_timeout,
        // lock_timeout, deadlock detection) very early in EVERY backend
        // (InitPostgres -> schedule_alarm), and treats a failure as FATAL ("could not
        // enable SIGALRM timer") -- an ENOSYS here killed the backend the instant a
        // client connected. The cooperative model has no interval-timer delivery, so
        // the alarm never actually fires; that only means those timeouts don't trip,
        // which is correct for a normally-completing query (default statement_timeout
        // is 0/disabled, and a single query hits no lock/deadlock wait). getitimer
        // reports a disarmed timer (all zero) so a later readback stays consistent.
        NR_SETITIMER => state.set_ret(0),
        NR_GETITIMER => {
            let curr = state.arg(2);
            if curr != 0 {
                ctx.arena.slice_mut(curr, 16).fill(0);
            }
            state.set_ret(0)
        }
        NR_MMAP => {
            let (addr, len, prot, flags, fd, off) = (
                state.arg(0),
                state.arg(1),
                state.arg(2),
                state.arg(3),
                state.arg(4) as i32,
                state.arg(5),
            );
            // LENGTH FIRST, before the MAP_FIXED branch, because that is where
            // Linux checks it: `do_mmap` opens with `if (!len) return -EINVAL`
            // and page-aligns immediately after, both ahead of anything that
            // looks at `addr` or the flags. So a zero length is refused for a
            // fixed mapping too, and putting the test after the branch would
            // reproduce the divergence for exactly the callers that named an
            // address.
            //
            // Ordering it here also subsumes the MAP_FIXED path's own
            // `(len + 0xfff) & !0xfff`: if `len + 0xffff` does not overflow then
            // `len + 0xfff` cannot either, so that rounding is safe once this
            // has run.
            let map_len = match mmap_round_len(len) {
                Ok(n) => n,
                Err(e) => {
                    // EINVAL for zero, ENOMEM for the overflow, matching
                    // `do_mmap`. Neither used to fail at all: BOTH used to
                    // succeed as a zero-byte mapping -- the zero-length one
                    // directly, the colossal one because the wrapped sum is
                    // always below the mask, which then clears it. See
                    // `mmap_round_len`.
                    let (errno, why) = match e {
                        MmapLenError::Zero => (EINVAL, "zero length"),
                        MmapLenError::Overflow => (ENOMEM, "length overflows page alignment"),
                    };
                    ecv_debug!(ecv, "mmap(len={len:#x}): {why} -> errno {errno}");
                    state.set_ret_err(errno);
                    return;
                }
            };
            // A non-zero addr WITHOUT MAP_FIXED is only a hint; Linux is free to
            // place the mapping anywhere, so ignoring it is correct and the bump
            // arena handles it like any other request.
            //
            // WITH MAP_FIXED the guest is entitled to that exact address. musl's
            // allocator does this at startup -- mmap(brk_base, 4096, PROT_NONE,
            // MAP_PRIVATE|MAP_FIXED|MAP_ANONYMOUS) -- to place a guard page at the
            // bottom of its heap, and nginx died here before reaching main. The
            // arena is flat and unprotected, so PROT_NONE cannot be enforced; what
            // matters is that the address stays valid and the contents are the
            // zeroes an anonymous mapping promises. A guard page is never read, so
            // the unenforceable protection is not observable.
            if addr != 0 && flags & MAP_FIXED != 0 {
                // Deliberately NOT `map_len`, and deliberately a different name:
                // a fixed mapping zeroes exactly the extent it is given, so it
                // rounds to 4 KiB rather than to `GUEST_PAGE_MASK`'s 64 KiB --
                // rounding up here would clobber up to 60 KiB of whatever the
                // guest had beside its guard page. The addition is safe because
                // `mmap_round_len` above already refused every `len` for which
                // `len + 0xffff` overflows.
                let fixed_len = (len + 0xfff) & !0xfff;
                if fd != -1 {
                    // ENODEV, not `fatal!`. Linux serves this; we cannot, because
                    // there is no demand paging to make a fixed file mapping mean
                    // anything. But an abort takes down EVERY process in the
                    // module, and mmap failing is a condition every caller of it
                    // already has to handle -- so the guest gets the same answer
                    // it would get for a descriptor the kernel refuses to map,
                    // which is the answer the MAP_SHARED file path below gives.
                    ecv_debug!(
                        ecv,
                        "file-backed MAP_FIXED mmap (addr={addr:#x}, fd={fd}) is unsupported -> ENODEV"
                    );
                    state.set_ret_err(ENODEV);
                    return;
                }
                // ⚠️ CHECKED, and the check is the point rather than the style:
                // `addr + fixed_len` WRAPS for an addr near u64::MAX, so a wild
                // MAP_FIXED address passed this bounds test with a small sum and
                // went on to `slice_mut`, which panics the arena -- an abort with
                // a message about the arena for what is really a bad argument.
                // Linux answers a fixed mapping it cannot place with ENOMEM.
                match addr.checked_add(fixed_len) {
                    Some(end) if end <= crate::arena::MEMORY_ARENA_SIZE as u64 => {}
                    _ => {
                        ecv_debug!(
                            ecv,
                            "MAP_FIXED mmap outside the arena (addr={addr:#x}, len={len}) -> ENOMEM"
                        );
                        state.set_ret_err(ENOMEM);
                        return;
                    }
                }
                // MAP_FIXED replaces whatever was there, and MAP_ANONYMOUS pages
                // read as zero -- so discarding the old contents is faithful, not
                // a shortcut.
                ctx.arena.slice_mut(addr, fixed_len as usize).fill(0);
                ecv_debug!(
                    ecv,
                    "mmap MAP_FIXED at 0x{addr:x} len={fixed_len} (protection not enforced)"
                );
                state.set_ret(addr);
                return;
            }
            // `map_len` -- the page-rounded length from `mmap_round_len` above --
            // is what reserves the slot. Rounding keeps every returned base
            // page-aligned even when the guest asks for an unaligned length (a
            // file mapping is sized from the file, not a page multiple).
            // Bounded by `shm_top`, NOT by MMAP_END_VMA. Shared regions are
            // carved downward from the top of the same window, so stopping at
            // the window end lets a per-process mmap climb into one -- and a
            // malloc'd mmap chunk landing inside a shared segment has its header
            // overwritten by `adopt_shared_from` on the next context switch.
            // That is heap corruption with no bad pointer anywhere in sight;
            // it presented as `munmap_chunk(): invalid pointer` deep in initdb.
            //
            // `mmap_reserve` reuses a reclaimed hole before growing the bump;
            // see `Arena::mmap_free` for why a pure bump could not carry
            // malloc's doubling ladder.
            let Some(at) = ctx.arena.mmap_reserve(map_len, ctx.shm_window.top) else {
                // ENOMEM, not a fatal. Running out of address space is an
                // ordinary condition a guest is expected to handle, and killing
                // the module takes away its ability to: PostgreSQL's initdb
                // probes shared_buffers DOWNWARD, halving until a size fits, so
                // failing the first attempt is how it discovers what it can have.
                // This is the third place ecvisor turned a recoverable limit into
                // an abort; see the file-backed MAP_SHARED arm just below.
                ecv_debug!(
                    ecv,
                    "mmap region exhausted (want {len} bytes, bump {:#x}, {} hole(s), shm_top {:#x}) -> ENOMEM",
                    ctx.arena.mmap_cur,
                    ctx.arena.mmap_free.len(),
                    ctx.shm_window.top
                );
                state.set_ret_err(ENOMEM);
                return;
            };
            // Address AND extent, so a later shared segment landing inside a
            // range some process already took privately is visible by
            // inspection rather than by theory. pid comes from the subscriber.
            ecv_debug!(
                mmap,
                "{:#x}..{:#x} len={} fd={} flags={:#x}",
                at,
                at + map_len,
                len,
                fd,
                flags
            );
            if fd != -1 {
                // File-backed mapping. glibc maps read-only files this way — most
                // notably the locale archive (_nl_load_locale_from_archive) and
                // shared objects' segments — as MAP_PRIVATE. We have no demand
                // paging, so materialize the mapping eagerly: copy the file bytes
                // [off, off+len) into the reserved region and zero-fill past EOF
                // (kernel mmap zeroes the tail of the final page too). MAP_PRIVATE
                // means writes never reach the file, so a private in-arena copy is
                // exactly correct; the copy is part of the arena and thus cloned
                // per-process on fork. Write-back (MAP_SHARED file) is unsupported.
                if flags & MAP_SHARED != 0 {
                    // POSIX shared memory. Backed by an arena region registered
                    // in `shared_segments`, exactly like SysV shmget and
                    // MAP_SHARED|MAP_ANONYMOUS below, so the range is exempt from
                    // the per-process arena swap and every process sees one
                    // physical copy.
                    //
                    // What is special here is the IDENTITY: sharing only happens
                    // if the second mapper lands on the first mapper's region, so
                    // mappings are keyed by the file's path. PostgreSQL's `posix`
                    // DSM backend depends on precisely that -- the postmaster
                    // creates /dev/shm/PostgreSQL.<n> and each backend maps the
                    // same name expecting the same bytes.
                    //
                    // The file's own contents are copied in once, at first map,
                    // and writes thereafter go to the arena and NOT back to the
                    // file. That is right for shm (nobody reads these through
                    // read(2)) and wrong for a general MAP_SHARED write-back
                    // mapping, which remains unimplemented.
                    // Hand back the private slot reserved above. Every path out
                    // of this arm either reuses an existing shared region or
                    // allocates from the shared window, so the reservation was
                    // pure waste -- and it is not idle waste: the shared
                    // window's floor IS `arena.mmap_cur`, so leaving it
                    // advanced shrinks the space the allocation below is about
                    // to ask for.
                    ctx.arena.mmap_release(at, map_len);
                    // Bisect switch (RAPTORMARK_ECV_NO_FILE_SHM, read at startup
                    // into `diag` -- this arm is post-fork, and `std::env::var`
                    // here is the poisoning hazard `diag` exists to prevent).
                    // File-backed MAP_SHARED is the newest thing
                    // in this path, so being able to take it out WITHOUT another
                    // build is what makes "is my shm code responsible" a
                    // measurement instead of an argument. With it off, a guest
                    // that has a fallback takes it: PostgreSQL drops from posix
                    // dynamic shared memory to sysv.
                    if crate::diag::no_file_shm() {
                        state.set_ret_err(ENODEV);
                        return;
                    }
                    let path = match ctx.fds.get(fd as usize).and_then(|s| s.as_ref()) {
                        Some(OpenFile::Mem { file, .. }) => ctx.open_files[*file].path.clone(),
                        _ => {
                            ecv_debug!(
                                ecv,
                                "MAP_SHARED mmap of fd={fd}, not a regular file -> ENODEV"
                            );
                            state.set_ret_err(ENODEV);
                            return;
                        }
                    };
                    if let Some(f) = ctx.shm_files.iter_mut().find(|f| f.path == path) {
                        let (vma, have) = (f.vma, f.len);
                        // Accumulate, never overwrite. `writable` is a fact about
                        // the REGION's whole history, and it is what
                        // `shm_file_reclaimable` reads to decide the region can
                        // hold no byte the file does not. A later read-only
                        // mapper clearing it would recycle a region an earlier
                        // writable mapper had already stored into.
                        f.writable |= prot & PROT_WRITE != 0;
                        if (len as usize) <= have {
                            // Give back the region the first mapper reserved; the
                            // arena bump above is simply unused.
                            if let Some(i) = ctx.shm_seg_at(vma) {
                                let pid = ctx.current_pid();
                                ctx.shm_add_mapper(i, pid);
                            }
                            ecv_debug!(
                                ecv,
                                "shm map {:?} -> existing {vma:#x} ({have} bytes)",
                                String::from_utf8_lossy(&path)
                            );
                            state.set_ret(vma);
                            return;
                        }
                        // A larger mapping of the same name would need the region
                        // to grow in place, which the bump allocator cannot do.
                        state.set_ret_err(ENOMEM);
                        return;
                    }
                    let file_bytes: Vec<u8> =
                        match ctx.fds.get(fd as usize).and_then(|s| s.as_ref()) {
                            Some(OpenFile::Mem { file, .. }) => {
                                let data = &ctx.open_files[*file].data;
                                let start = (off as usize).min(data.len());
                                let end = (start + len as usize).min(data.len());
                                data[start..end].to_vec()
                            }
                            _ => Vec::new(),
                        };
                    // Allocate from the GLOBAL shared window, never from the
                    // per-process bump `at` above: `arena.mmap_cur` travels with
                    // the arena, so every forked child restarts it at the same
                    // place and two processes creating different shm segments
                    // both landed on one VMA. See `EcvContext::shm_window`.
                    let Some(at) = ctx.shm_reserve(map_len) else {
                        state.set_ret_err(ENOMEM);
                        return;
                    };
                    let region = ctx.arena.slice_mut(at, map_len as usize);
                    region[..file_bytes.len()].copy_from_slice(&file_bytes);
                    region[file_bytes.len()..].fill(0);
                    let pid = ctx.current_pid();
                    ctx.shared_segments.push(SharedSeg {
                        vma_start: at,
                        len: map_len as usize,
                        kind: SharedKind::File,
                        mappers: vec![pid],
                    });
                    ctx.shm_files.push(ShmFile {
                        path: path.clone(),
                        vma: at,
                        len: map_len as usize,
                        writable: prot & PROT_WRITE != 0,
                    });
                    ecv_debug!(
                        ecv,
                        "shm map {:?} -> new {at:#x} ({map_len} bytes)",
                        String::from_utf8_lossy(&path)
                    );
                    state.set_ret(at);
                    return;
                }
                let file_bytes: Vec<u8> = match ctx.fds.get(fd as usize).and_then(|s| s.as_ref()) {
                    Some(OpenFile::Mem { file, .. }) => {
                        let data = &ctx.open_files[*file].data;
                        let start = (off as usize).min(data.len());
                        let end = (start + len as usize).min(data.len());
                        data[start..end].to_vec()
                    }
                    // ENODEV, not `fatal!`: this is exactly what Linux answers
                    // for a descriptor whose file operations have no `mmap` --
                    // a pipe, a socket, an eventfd. The sibling MAP_SHARED arm
                    // above already returns it for the same condition, so this
                    // was the one place where mapping a pipe killed the module
                    // instead of the caller.
                    _ => {
                        ecv_debug!(ecv, "mmap of fd={fd}, not a regular file -> ENODEV");
                        state.set_ret_err(ENODEV);
                        return;
                    }
                };
                let region = ctx.arena.slice_mut(at, map_len as usize);
                region[..file_bytes.len()].copy_from_slice(&file_bytes);
                region[file_bytes.len()..].fill(0);
                state.set_ret(at);
                return;
            }
            // An ANONYMOUS mapping with a non-zero offset. This used to be
            // `fatal!`, and the kernel does not fail it at all: `do_mmap` records
            // `vm_pgoff` and never reads it back for an anonymous VMA, so the
            // offset is simply meaningless here. Aborting was the one answer
            // Linux definitely does not give.
            //
            // The exception is a MISALIGNED offset, which arm64's `sys_mmap`
            // rejects before it looks at anything else
            // (`if (offset_in_page(off)) return -EINVAL`). Checked against 4 KiB
            // and not `GUEST_PAGE_MASK`, which is 64 KiB: the two are different
            // questions. We ALIGN generously, to the largest page any aarch64
            // guest can believe in, because a frozen glibc believes 65536; we
            // VALIDATE leniently, against the AT_PAGESZ of 4096 we actually
            // advertise, because a guest that computed this offset from the auxv
            // page size is entitled to have it accepted.
            if off & 0xfff != 0 {
                ecv_debug!(ecv, "mmap offset {off:#x} is not page-aligned -> EINVAL");
                state.set_ret_err(EINVAL);
                return;
            }
            if off != 0 {
                ecv_debug!(ecv, "anonymous mmap offset {off:#x} ignored, as Linux does");
            }
            // MAP_SHARED|MAP_ANONYMOUS (fd == -1): register the VMA as a shared
            // segment so it is exempt from the per-process arena restore — one
            // physical copy every cooperatively-scheduled process sees at the
            // same VMA. MAP_PRIVATE keeps today's copy-on-fork (bump-only)
            // behavior. Zero the shared bytes so the initial contents are
            // defined regardless of prior use of the region.
            if flags & MAP_SHARED != 0 && flags & MAP_ANONYMOUS != 0 {
                // From the GLOBAL shared window, never the per-process bump.
                //
                // A shared segment is exempt from the arena swap by definition,
                // so its VMA range means the same memory in EVERY process. Taking
                // it from `arena.mmap_cur` -- which travels with the arena, so a
                // forked child restarts it low -- therefore aliases whatever the
                // PARENT already had privately at those addresses.
                //
                // PostgreSQL's shared_buffers is exactly this call
                // (MAP_SHARED|MAP_ANONYMOUS|MAP_HUGETLB), and a child asking for
                // 76 MiB at 0x10000000 published a window straight over initdb's
                // private heap: malloc's 192 KiB chunk at 0x10040000 had its
                // header rewritten under it and initdb died with
                // `double free or corruption (out)`.
                // Hand back the private slot reserved above; see the file-backed
                // arm for why it is not merely wasted.
                ctx.arena.mmap_release(at, map_len);
                let seg = (len + GUEST_PAGE_MASK) & !GUEST_PAGE_MASK;
                let Some(at) = ctx.shm_reserve(seg) else {
                    state.set_ret_err(ENOMEM);
                    return;
                };
                ctx.arena.slice_mut(at, seg as usize).fill(0);
                let pid = ctx.current_pid();
                ctx.shared_segments.push(SharedSeg {
                    vma_start: at,
                    len: seg as usize,
                    kind: SharedKind::Anon,
                    mappers: vec![pid],
                });
                ecv_debug!(
                    ecv,
                    "anon-shared mmap {len} bytes -> {at:#x} (shared window)"
                );
                state.set_ret(at);
                return;
            }
            state.set_ret(at);
        }
        // fsync/fdatasync/sync/syncfs/sync_file_range: succeed as a no-op.
        //
        // ENOSYS here is not a harmless gap. `initdb` finishes with
        //   syncing data to disk ... initdb: error: could not fsync file
        //   ".../PG_VERSION": Function not implemented
        // and then DELETES the cluster it just built -- the whole run thrown
        // away on the last step, because a database that cannot promise
        // durability is right to refuse.
        //
        // A no-op is honest rather than a shortcut: the guest filesystem is the
        // in-memory rfs plus its tmpfs overlay, so there is no storage beneath
        // it and nothing a flush could make more durable. What the guest is
        // promised -- that its writes are visible to every later read -- is
        // already unconditionally true. Nothing here is lost that fsync could
        // have saved; the whole filesystem is lost when the module exits, which
        // is a property of the model and not of this syscall.
        //
        // The fd is not validated: sync() takes none, and returning EBADF for a
        // closed one would only turn a no-op into a failure the guest must
        // handle.
        NR_FSYNC | NR_FDATASYNC | NR_SYNC | NR_SYNCFS | NR_SYNC_FILE_RANGE => state.set_ret(0),
        // mprotect(addr, len, prot): 0, and one side effect.
        //
        // The arena is one flat wasm linear-memory buffer with no page table
        // under it, so no protection here is ENFORCEABLE and success-with-no-
        // action is the only answer that does not break guests which merely
        // tighten permissions they never violate.
        //
        // The side effect is not enforcement, it is bookkeeping. `mprotect` is
        // the second route to write permission on a file-backed MAP_SHARED
        // region, and `context::shm_file_reclaimable` recycles such a region on
        // the claim that nothing was ever ABLE to write to it. Ignoring
        // `mprotect` would leave that claim false for a guest that maps
        // PROT_READ and upgrades afterwards -- rare, but silently wrong in the
        // direction that recycles memory somebody's bytes are in. Overlap, not
        // containment: a partial mprotect grants write to part of the region,
        // which is enough to invalidate the claim about all of it.
        NR_MPROTECT => {
            let (addr, len, prot) = (state.arg(0), state.arg(1), state.arg(2));
            if prot & PROT_WRITE != 0 {
                let end = addr.saturating_add(len);
                for f in ctx.shm_files.iter_mut() {
                    if addr < f.vma + f.len as u64 && f.vma < end {
                        f.writable = true;
                    }
                }
            }
            state.set_ret(0)
        }
        // madvise(addr, len, advice): a hint, EXCEPT where it is not.
        //
        // Most advice a kernel may ignore, so success with no action is honest
        // for a runtime with no paging. `MADV_DONTNEED` is different: on an
        // anonymous private mapping Linux guarantees the range reads as ZEROES
        // afterwards, and allocators use exactly that to release a large block
        // without unmapping it. Answering 0 without zeroing hands the guest its
        // own stale bytes where it is entitled to zeroes -- silent corruption,
        // not a missing feature. See `arena::madvise_zeroes`.
        NR_MADVISE => {
            let (addr, len, advice) = (state.arg(0), state.arg(1), state.arg(2));
            if crate::arena::madvise_zeroes(advice) && ctx.arena.in_bounds(addr, len as usize) {
                ctx.arena.slice_mut(addr, len as usize).fill(0);
                ecv_debug!(ecv, "madvise({addr:#x}, {len}, {advice}) -> zeroed");
            }
            state.set_ret(0)
        }
        // mremap(old, old_len, new_len, flags, new_addr): the two cases that are
        // safe here, and ENOMEM for the rest -- which is what Linux answers when
        // it cannot extend a mapping in place.
        //
        // ENOSYS was survivable: both libcs fall back to malloc+memcpy+free for a
        // large realloc. But it is a gap rather than an honest answer, and the
        // fallback is the expensive path on exactly the allocations big enough to
        // reach here.
        //
        // ⚠️ GROWING IN PLACE IS NOT ONE OF THE CASES. The bump arena has no way
        // to know whether the bytes after `old` are free -- `mmap_free` holds
        // reclaimed holes, not a map of what is live -- so extending would risk
        // handing the guest memory another mapping already owns. Moving is always
        // correct and costs a copy; guessing is silently wrong.
        NR_MREMAP => {
            let (old, old_len, new_len, flags) =
                (state.arg(0), state.arg(1), state.arg(2), state.arg(3));
            const MREMAP_MAYMOVE: u64 = 1;
            let round = |n: u64| (n + GUEST_PAGE_MASK) & !GUEST_PAGE_MASK;
            let (old_len, new_len) = (round(old_len), round(new_len));
            if new_len == 0 || !ctx.arena.in_bounds(old, old_len as usize) {
                state.set_ret_err(EINVAL);
                return;
            }
            if new_len <= old_len {
                // Shrink in place: hand the tail back and keep the address, as
                // Linux does. Zeroed on release rather than on the next reserve
                // only because `mmap_reserve` already zeroes what it hands out.
                if new_len < old_len {
                    ctx.arena.mmap_release(old + new_len, old_len - new_len);
                }
                ecv_debug!(
                    ecv,
                    "mremap {old:#x} {old_len} -> {new_len} (shrink in place)"
                );
                state.set_ret(old);
                return;
            }
            if flags & MREMAP_MAYMOVE == 0 {
                ecv_debug!(ecv, "mremap {old:#x} grow without MAYMOVE -> ENOMEM");
                state.set_ret_err(ENOMEM);
                return;
            }
            let Some(at) = ctx.arena.mmap_reserve(new_len, ctx.shm_window.top) else {
                state.set_ret_err(ENOMEM);
                return;
            };
            let src = ctx.arena.slice(old, old_len as usize).to_vec();
            ctx.arena.slice_mut(at, src.len()).copy_from_slice(&src);
            ctx.arena.mmap_release(old, old_len);
            ecv_debug!(
                ecv,
                "mremap {old:#x} {old_len} -> {at:#x} {new_len} (moved)"
            );
            state.set_ret(at)
        }
        // membarrier(cmd, flags, cpu_id): 0.
        //
        // Honest rather than a shortcut. ecvisor's processes are cooperative
        // contexts on ONE wasm thread and switch only at syscall boundaries, so
        // every other context is already at a barrier by construction. There is
        // no reordering for this to prevent.
        NR_MEMBARRIER => state.set_ret(0),
        // sysinfo(info): the fields a guest actually reads.
        //
        // ENOSYS here is survivable but wasteful: a guest sizing a cache from
        // `totalram` falls back to a conservative default, and the number we can
        // give is exact -- `memory.size` is what the host has actually granted.
        // The rest is reported as zero, which is what a fresh boot looks like and
        // is what `uptime`/`loads` would be if we tracked them.
        NR_SYSINFO => {
            let out = state.arg(0);
            // struct sysinfo on LP64: uptime, loads[3], totalram, freeram,
            // sharedram, bufferram, totalswap, freeswap (i64/u64), then procs
            // (u16), pad, totalhigh, freehigh, mem_unit (u32). 112 bytes with
            // the trailing pad; zeroing the whole thing and filling two fields
            // is what keeps this independent of the tail's exact layout.
            const SYSINFO_LEN: usize = 112;
            if !ctx.arena.in_bounds(out, SYSINFO_LEN) {
                state.set_ret_err(EFAULT);
                return;
            }
            let total = (crate::diag::linear_memory_mib() as u64) << 20;
            let buf = ctx.arena.slice_mut(out, SYSINFO_LEN);
            buf.fill(0);
            buf[32..40].copy_from_slice(&total.to_le_bytes()); // totalram
            buf[40..48].copy_from_slice(&total.to_le_bytes()); // freeram
            buf[104..108].copy_from_slice(&1u32.to_le_bytes()); // mem_unit = 1
            state.set_ret(0)
        }
        // sigaltstack(new, old): no alternate signal stack is modelled --
        // `run_signal_handler` builds the handler frame below the interrupted
        // SP. Report "none installed" rather than ENOSYS: glibc's siglongjmp
        // queries it while restoring, and an error there left it computing a
        // branch target of 0 immediately after correctly recovering the real
        // one. ss_flags = SS_DISABLE (2).
        NR_SIGALTSTACK => {
            let old = state.arg(1);
            if old != 0 {
                // SS_ONSTACK (1) while a handler is running, SS_DISABLE (2)
                // otherwise. `run_signal_handler` builds the handler frame below
                // the interrupted SP rather than on a stack the guest allocated,
                // so "on a signal stack" is the truthful answer while one runs.
                //
                // Note this does NOT satisfy glibc's `____longjmp_chk`, which
                // also range-checks the target SP against ss_sp/ss_size. Probed
                // by reporting the whole arena as the stack, which would admit
                // any in-arena target: it still refused, so the SP glibc recovers
                // from the jmp_buf is outside the arena altogether. See the
                // journal entry for that day.
                let flags: u32 = if ctx.in_signal_handler > 0 { 1 } else { 2 };
                let sp = state.gpr.sp.val;
                let b = ctx.arena.slice_mut(old, 24);
                b.fill(0);
                b[0..8].copy_from_slice(&sp.to_le_bytes()); // ss_sp
                b[8..16].copy_from_slice(&(64u64 * 1024).to_le_bytes()); // ss_size
                b[16..20].copy_from_slice(&flags.to_le_bytes()); // ss_flags
            }
            state.set_ret(0)
        }
        NR_GETRANDOM => {
            let (buf, count) = (state.arg(0), state.arg(1) as usize);
            ecv_debug!(ecv, "getrandom(count={count})");
            // Varying (not constant) bytes: OpenSSL 3.x's DRBG health-tests its
            // entropy source and rejects a constant fill, which failed PostgreSQL's
            // pg_strong_random (cancel key / auth). Non-cryptographic by design.
            ctx.fill_random(buf, count);
            state.set_ret(count as u64);
        }
        // Intercepted dl* API (pseudo-syscalls; the fuser stubbed the real
        // dlopen/dlsym/dlclose/dlerror -- internal/fuse/tables.go patchDLStubs).
        //
        // ⚠️ THESE USED TO LIE, and the lie was the point of the rewrite. dlopen
        // returned a non-null SENTINEL for every path, whether or not the module
        // existed; dlsym ignored the handle and searched one flat closure-wide
        // table; dlerror always returned NULL. So an absent plugin "loaded"
        // successfully, every symbol then resolved to NULL, and the guest could
        // not find out why -- postgres reports that as
        // `incompatible library "...": missing magic block`, a version mismatch
        // by appearance and an absent object in fact.
        NR_ECV_DLOPEN => {
            let p = state.arg(0);
            // dlopen(NULL) means "a handle to the calling program itself", and
            // that one really is always satisfiable.
            if p == 0 {
                let cur = ctx.procs[ctx.current].prog_idx;
                ctx.clear_dlerror();
                state.set_ret(cur as u64 + 1);
                return;
            }
            let path = ctx.arena.read_cstr(p);
            let shown = String::from_utf8_lossy(&path).into_owned();
            let mode = state.arg(1);
            match ctx.dlmap.resolve(&ctx.vfs, &ctx.cwd, &path, &ctx.programs) {
                // RTLD_NOLOAD asks "is this ALREADY loaded?" and must never
                // cause a load. Ignoring it inverted the caller's request: code
                // that probes for an optional plugin without pulling it in would
                // have pulled it in, run its constructors, and been told it was
                // there all along.
                Some(idx) if mode & RTLD_NOLOAD != 0 => {
                    ctx.clear_dlerror();
                    if ctx.unit(idx).inited {
                        ctx.unit_mut(idx).refs += 1;
                        state.set_ret(idx as u64 + 1);
                    } else {
                        // NULL with no error, as glibc does: not-loaded is the
                        // answer to the question, not a failure.
                        state.set_ret(0);
                    }
                }
                Some(idx) => match ctx.ensure_unit_loaded(idx) {
                    Ok(UnitLoadStep::Parked) => {
                        // The host is loading it. The process is parked and will
                        // re-enter THIS syscall when woken, so deliberately set
                        // no return value: whatever we wrote would be the answer
                        // to a dlopen that has not happened.
                    }
                    Ok(UnitLoadStep::Done) => {
                        ctx.unit_mut(idx).refs += 1;
                        ctx.clear_dlerror();
                        ecv_trace!(ecvisor, "dlopen({}) -> handle={}", shown, idx + 1);
                        // Handle = index + 1, so 0 is never valid and a caller
                        // testing `if (!h)` behaves as it does natively.
                        state.set_ret(idx as u64 + 1);
                    }
                    Err(why) => {
                        ctx.set_dlerror(format!("{shown}: {why}").into_bytes());
                        // Gated, per this file's convention (see the note beside
                        // the `trace` import). An ungated warning would be
                        // redundant now: the refusal reaches the guest through
                        // `dlerror`, which is the whole point of the rewrite --
                        // ecvisor no longer has to shout on the guest's behalf.
                        ecv_trace!(ecvisor, "dlopen({}) refused: {}", shown, why);
                        state.set_ret(0);
                    }
                },
                // ❗ NOT-RESOLVED IS TWO DIFFERENT SITUATIONS, and conflating
                // them is what made a host-driven loader impossible.
                //
                // `resolve` answers "which registry index serves this path". A
                // lazily-placed unit has none until the host instantiates its
                // side module, so under `hosted` the FIRST dlopen of a plugin
                // the build definitely shipped lands here -- and reporting "no
                // such file" would be a flat lie that no dlerror could correct.
                //
                // `hash_for` separates them: a hash means the build shipped this
                // plugin and the host has simply not placed it yet.
                None => match ctx.dlmap.hash_for(&ctx.vfs, &ctx.cwd, &path) {
                    Some(hash) => match ctx.ensure_unregistered_unit(&hash) {
                        Ok(UnitLoadStep::Parked) => {
                            // Parked on a pending token; the guest re-enters this
                            // syscall when the host calls `ecv_side_loaded`, and
                            // `resolve` succeeds on that pass. No return value,
                            // for the same reason as the registered park above.
                        }
                        Ok(UnitLoadStep::Done) => {
                            // The host placed it synchronously. It registered the
                            // descriptor, so the SAME path now resolves -- and if
                            // it does not, the host claimed a load it did not
                            // finish, which is a host bug and must be said rather
                            // than papered over with a plausible handle.
                            match ctx.dlmap.resolve(&ctx.vfs, &ctx.cwd, &path, &ctx.programs) {
                                Some(idx) => match ctx.ensure_unit_loaded(idx) {
                                    Ok(UnitLoadStep::Parked) => {}
                                    Ok(UnitLoadStep::Done) => {
                                        ctx.unit_mut(idx).refs += 1;
                                        ctx.clear_dlerror();
                                        ecv_trace!(
                                            ecvisor,
                                            "dlopen({}) -> handle={} (host-placed)",
                                            shown,
                                            idx + 1
                                        );
                                        state.set_ret(idx as u64 + 1);
                                    }
                                    Err(why) => {
                                        ctx.set_dlerror(format!("{shown}: {why}").into_bytes());
                                        state.set_ret(0);
                                    }
                                },
                                None => {
                                    ctx.set_dlerror(
                                        format!(
                                            "{shown}: the host reported this unit loaded but did \
                                             not register its descriptor"
                                        )
                                        .into_bytes(),
                                    );
                                    state.set_ret(0);
                                }
                            }
                        }
                        Err(why) => {
                            ctx.set_dlerror(format!("{shown}: {why}").into_bytes());
                            ecv_trace!(ecvisor, "dlopen({}) refused by the host: {}", shown, why);
                            state.set_ret(0);
                        }
                    },
                    None => {
                        // The message a real loader gives, because a guest may
                        // match on it -- and because "no such file" is the truth:
                        // this module was built without that unit.
                        ctx.set_dlerror(
                            format!(
                                "{shown}: cannot open shared object file: No such file or directory"
                            )
                            .into_bytes(),
                        );
                        ecv_trace!(ecvisor, "dlopen({}) -> NULL (no such unit)", shown);
                        state.set_ret(0);
                    }
                },
            }
        }
        NR_ECV_DLSYM => {
            // dlsym(handle=x0, name=x1), and the handle is now HONOURED.
            let h = state.arg(0);
            let name = ctx.arena.read_cstr(state.arg(1));
            let shown = String::from_utf8_lossy(&name).into_owned();
            // RTLD_DEFAULT is NULL: search global scope, which is the old
            // behaviour and remains right for it.
            let vma = if h == 0 {
                ctx.dlsym_lookup(&name)
            } else if h as usize <= ctx.programs.len() && h != u64::MAX {
                ctx.dlsym_in(h as usize - 1, &name)
            } else {
                // RTLD_NEXT (-1) and any other bogus handle. RTLD_NEXT needs a
                // caller-relative link order this has no notion of, so it is
                // refused rather than silently answered from global scope --
                // which would return the SAME definition the caller already has
                // and is the classic way an interposer recurses forever.
                ctx.set_dlerror(
                    format!("dlsym({shown}): invalid or unsupported handle {h:#x}").into_bytes(),
                );
                state.set_ret(0);
                return;
            };
            if vma == 0 {
                ctx.set_dlerror(format!("undefined symbol: {shown}").into_bytes());
            } else {
                ctx.clear_dlerror();
            }
            ecv_trace!(ecvisor, "dlsym(h={}, {}) -> 0x{:x}", h, shown, vma);
            state.set_ret(vma);
        }
        NR_ECV_DLCLOSE => {
            // Refcount only; a unit is never torn down. See UnitLoad::inited.
            let h = state.arg(0);
            if h == 0 || h as usize > ctx.programs.len() {
                ctx.set_dlerror(format!("dlclose: invalid handle {h:#x}").into_bytes());
                state.set_ret(u64::MAX); // non-zero: dlclose reports failure that way
                return;
            }
            let n = ctx.unit(h as usize - 1).refs.saturating_sub(1);
            ctx.unit_mut(h as usize - 1).refs = n;
            ctx.clear_dlerror();
            state.set_ret(0);
        }
        NR_ECV_DLERROR => {
            let p = ctx.take_dlerror();
            state.set_ret(p);
        }
        _ => {
            ecv_debug!(ecvisor, "ENOSYS syscall {} (pc=0x{:x})", nr, state.pc());
            state.set_ret_err(ENOSYS);
        }
    }
}

// --- filesystem syscalls -------------------------------------------------

/// The contents of a synthetic `/proc/self/maps`.
///
/// Standard Linux format -- `start-end perms offset dev inode pathname`, with
/// addresses as lowercase hex WITHOUT a `0x` prefix, which is what glibc's
/// `%SCNxPTR` parse expects.
///
/// The regions mirror `arena.rs`: the image, the brk heap, the mmap window and
/// the stack. ❗ The `[stack]` line must CONTAIN `__libc_stack_end`, or
/// `pthread_getattr_np` still fails -- glibc looks for the line spanning that
/// address, not for the label.
fn proc_self_maps() -> Vec<u8> {
    use crate::arena::{
        BRK_END_VMA, BRK_START_VMA, IMAGE_BASE_VMA, MMAP_END_VMA, MMAP_START_VMA, STACK_TOP_VMA,
    };
    let mut s = String::new();
    let mut line = |lo: u64, hi: u64, perms: &str, label: &str| {
        s.push_str(&format!(
            "{lo:08x}-{hi:08x} {perms} 00000000 00:00 0 {label}\n"
        ));
    };
    line(IMAGE_BASE_VMA, BRK_START_VMA, "r-xp", "");
    line(BRK_START_VMA, BRK_END_VMA, "rw-p", "[heap]");
    line(MMAP_START_VMA, MMAP_END_VMA, "rw-p", "");
    line(MMAP_END_VMA, STACK_TOP_VMA, "rw-p", "[stack]");
    s.into_bytes()
}

/// Resolves a syscall path argument against a dirfd + the cwd.
///
/// The BASE is chosen by [`EcvContext::resolve_base`], which lives in
/// `context.rs` rather than here for one reason: `mod sys` is
/// `#[cfg(target_arch = "wasm32")]`, so nothing in this file is compiled on the
/// host and nothing in it can have a `cargo test`. The decision is the part
/// worth testing, so it is kept where a test can reach it.
fn resolve_arg(ctx: &EcvContext, dirfd: i64, path: &[u8], follow: bool) -> Option<Resolved> {
    let base = ctx.resolve_base(dirfd, path)?;
    // Borrow ends before `resolve` needs `&ctx.vfs`; cloning here would be on
    // the path of every `*at` call.
    let base = base.to_vec();
    ctx.vfs.resolve(&base, path, follow)
}

/// Applies O_CLOEXEC to a freshly opened fd and returns it to the guest.
fn finish_open(state: &mut State, ctx: &mut EcvContext, fd: i32, flags: u64) {
    if flags & O_CLOEXEC != 0 {
        ctx.set_cloexec(fd as usize, true);
    }
    state.set_ret(fd as u64);
}

#[inline(never)]
/// Maps `/dev/stdin`, `/dev/stdout` and `/dev/stderr` -- directly, or through one
/// level of symlink -- to the host stdio descriptor they name. Returns None for
/// anything else, so an image that really does ship a file at one of those paths
/// still resolves normally.
fn synthetic_stdio(ctx: &EcvContext, path: &[u8]) -> Option<i32> {
    fn to_fd(p: &[u8]) -> Option<i32> {
        match p {
            b"/dev/stdin" => Some(0),
            b"/dev/stdout" => Some(1),
            b"/dev/stderr" => Some(2),
            _ => None,
        }
    }
    if let Some(fd) = to_fd(path) {
        return Some(fd);
    }
    to_fd(&ctx.vfs.readlink(&ctx.cwd, path)?)
}

fn sys_openat(state: &mut State, ctx: &mut EcvContext) {
    let dirfd = state.arg(0) as i64;
    let path = ctx.arena.read_cstr(state.arg(1));
    let flags = state.arg(2);
    let follow = flags & O_NOFOLLOW == 0;

    // Synthetic /dev/urandom + /dev/random: a container image's /dev is empty
    // (devices are bind-mounted at runtime), so these do not exist in the flattened
    // rootfs -- and PostgreSQL's pg_strong_random reads /dev/urandom for its backend
    // cancel key, FATAL'ing ("could not generate random cancel key") on ENOENT.
    // Back them with an in-memory buffer of pseudo-random bytes (1 MiB, far more than
    // any seed/key draw needs from a single fd). Non-cryptographic, like getrandom.
    // Synthetic /proc/self/maps. ecvisor synthesises no /proc at all, and glibc
    // implements `pthread_getattr_np` for the MAIN thread by reading this file:
    // it scans for the line whose range contains `__libc_stack_end` and takes
    // the stack extent from it. With no /proc the call returns ENOENT, so a
    // guest cannot discover where its own stack is.
    //
    // ❗ That is not just an API gap. A CONSERVATIVE garbage collector uses
    // exactly this to choose the range it marks -- Ruby's
    // `rb_gc_mark_machine_context` marks over `[stack_start, stack_end)`. A
    // wrong or guessed range means live references on the real stack are not
    // seen and their objects are freed while still in use. Measured 2026-09-01:
    // ruby corrupts a VALUE while loading RubyGems, and `GC.disable` avoids it.
    //
    // ⚠️ The STACK line is the load-bearing one; the others are emitted because
    // a plausible map is more useful than a single line, not because anything
    // is known to read them.
    if path == b"/proc/self/maps" || path == b"/proc/curproc/map" {
        ecv_debug!(ecv, "synthetic /proc/self/maps open");
        let data = proc_self_maps();
        let file = mem_file_for(ctx, &path, data);
        let off = alloc_file_offset(ctx);
        let fd = ctx.alloc_fd(OpenFile::Mem {
            file,
            off,
            writable: false,
        });
        finish_open(state, ctx, fd, flags);
        return;
    }

    if path == b"/dev/urandom" || path == b"/dev/random" {
        ecv_debug!(ecv, "synthetic /dev/urandom open");
        let data = ctx.random_bytes(1 << 20);
        let file = mem_file_for(ctx, &path, data);
        let off = alloc_file_offset(ctx);
        let fd = ctx.alloc_fd(OpenFile::Mem {
            file,
            off,
            writable: false,
        });
        finish_open(state, ctx, fd, flags);
        return;
    }

    // Synthetic /dev/null and /dev/zero, the same missing-/dev problem. dash
    // opens /dev/null for every redirection it cannot satisfy otherwise, and
    // reported `sh: 1: cannot open /dev/null: No such file` under initdb.
    if path == b"/dev/null" || path == b"/dev/zero" {
        ecv_debug!(ecv, "synthetic {}", String::from_utf8_lossy(&path));
        let fd = ctx.alloc_fd(OpenFile::Null {
            zero: path == b"/dev/zero",
        });
        finish_open(state, ctx, fd, flags);
        return;
    }

    // Synthetic /dev/stdin, /dev/stdout, /dev/stderr -- the same missing-/dev
    // problem as /dev/urandom above, and the one every container image hits:
    // nginx's image ships /var/log/nginx/error.log as a SYMLINK to /dev/stderr,
    // and nginx opens its default error log before it has even parsed the config
    // that would redirect it. With the target missing that open ENOENTs and nginx
    // alerts and dies, no matter what the config says.
    //
    // Checked through one level of symlink for exactly that reason: matching only
    // the literal path would catch a direct open of /dev/stderr and miss every
    // image that points its logs at one.
    if let Some(hostfd) = synthetic_stdio(ctx, &path) {
        ecv_debug!(
            ecv,
            "synthetic {} -> host fd {hostfd}",
            String::from_utf8_lossy(&path)
        );
        let fd = ctx.alloc_fd(OpenFile::Stdio(hostfd));
        finish_open(state, ctx, fd, flags);
        return;
    }

    let resolved = resolve_arg(ctx, dirfd, &path, follow);
    ecv_trace!(
        ecv,
        "openat {:?} flags=0x{flags:x} -> {}",
        String::from_utf8_lossy(&path),
        if resolved.is_some() { "OK" } else { "ENOENT" }
    );
    match resolved {
        Some(r) => match r.meta.kind {
            NodeKind::Dir => {
                let entries = ctx.vfs.readdir(&ctx.cwd, &r.path).unwrap_or_default();
                let fd = ctx.alloc_fd(OpenFile::Dir {
                    entries,
                    pos: 0,
                    path: r.path.clone(),
                });
                finish_open(state, ctx, fd, flags);
            }
            NodeKind::File => {
                let data = ctx.vfs.read(&ctx.cwd, &r.path).unwrap_or_default();
                let writable = flags & (O_WRONLY | O_RDWR) != 0;
                // O_TRUNC must reach the SHARED buffer, not just this
                // descriptor's view: another process may already hold the file
                // open, and truncating a private copy would leave it with the
                // old contents and resurrect them on its close.
                let truncate = flags & O_TRUNC != 0;
                let file = mem_file_for(ctx, &r.path, data);
                if truncate {
                    ctx.open_files[file].data.clear();
                    ctx.open_files[file].dirty = true;
                }
                let off = alloc_file_offset(ctx);
                let fd = ctx.alloc_fd(OpenFile::Mem {
                    file,
                    off,
                    writable,
                });
                finish_open(state, ctx, fd, flags);
            }
            NodeKind::Symlink => state.set_ret_err(ENOENT),
            // Opening a bound AF_UNIX socket by path is ENXIO on Linux -- the
            // name is a rendezvous point, not a file. Returning a readable
            // empty file instead would hand a guest an fd that silently reports
            // EOF forever.
            NodeKind::Socket => state.set_ret_err(ENXIO),
        },
        None => {
            if flags & O_CREAT != 0 {
                // Create in the tmpfs upper layer.
                let abs = absolutize(&ctx.cwd, &path);
                ctx.vfs.upper_mut().write_file(&abs, Vec::new());
                let file = mem_file_for(ctx, &abs, Vec::new());
                let off = alloc_file_offset(ctx);
                let fd = ctx.alloc_fd(OpenFile::Mem {
                    file,
                    off,
                    writable: true,
                });
                finish_open(state, ctx, fd, flags);
            } else {
                state.set_ret_err(ENOENT);
            }
        }
    }
}

#[inline(never)]
fn sys_close(state: &mut State, ctx: &mut EcvContext) {
    let fd = state.arg(0) as usize;
    if close_fd_full(ctx, fd) {
        state.set_ret(0);
    } else {
        state.set_ret_err(EBADF);
    }
}

/// unlinkat(dirfd, path, flags): remove a file (or, with AT_REMOVEDIR, a dir) by
/// placing an overlay whiteout so the name disappears from both layers.
#[inline(never)]
fn sys_unlinkat(state: &mut State, ctx: &mut EcvContext) {
    let dirfd = state.arg(0) as i64;
    let path = ctx.arena.read_cstr(state.arg(1));
    // Relative paths resolve against the dirfd's directory; the errno is
    // unchanged from when this refused every such call, so only an
    // UNUSABLE dirfd reaches it now.
    let Some(base) = ctx.resolve_base(dirfd, &path).map(|b| b.to_vec()) else {
        state.set_ret_err(ENOENT);
        return;
    };
    // Must exist to unlink.
    if ctx.vfs.resolve(&base, &path, false).is_none() {
        state.set_ret_err(ENOENT);
        return;
    }
    let abs = absolutize(&base, &path);
    // Unlinking a bound AF_UNIX socket removes the NAME, not the endpoint: the
    // listener keeps serving what it has already queued and accepted, and only
    // new lookups fail. PostgreSQL depends on both halves -- it unlinks a stale
    // socket before binding, and unlinks its live one during shutdown while
    // backends are still talking through it.
    if let Some(l) = find_unix_listener(ctx, &abs) {
        ctx.unix_listeners[l].path.clear();
    }
    ctx.vfs.upper_mut().whiteout(&abs);
    state.set_ret(0);
}

/// ftruncate(fd, len): resize an in-memory file to `len` (zero-extending). Used
/// by postgres for DSM/mmap segment sizing and WAL preallocation.
#[inline(never)]
fn sys_ftruncate(state: &mut State, ctx: &mut EcvContext) {
    let (fd, len) = (state.arg(0) as usize, state.arg(1) as usize);
    match ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        Some(OpenFile::Mem { file, writable, .. }) => {
            if !*writable {
                state.set_ret_err(EBADF);
                return;
            }
            let file = *file;
            ctx.open_files[file].data.resize(len, 0);
            ctx.open_files[file].dirty = true;
            state.set_ret(0);
        }
        None => state.set_ret_err(EBADF),
        _ => state.set_ret_err(EINVAL),
    }
}

/// truncate(path, len): resize the file at `path` to `len` (zero-extending),
/// writing the result to the tmpfs upper. postgres truncates relation files by
/// path during CREATE/relation storage (the fd-based ftruncate handles the mmap/WAL
/// cases). Any open in-memory fd for the same path is resized too, so its
/// flush-on-close does not resurrect the old size.
#[inline(never)]
fn sys_truncate(state: &mut State, ctx: &mut EcvContext) {
    let path = ctx.arena.read_cstr(state.arg(0));
    let len = state.arg(1) as usize;
    let Some(mut content) = ctx.vfs.read(&ctx.cwd, &path) else {
        state.set_ret_err(ENOENT);
        return;
    };
    content.resize(len, 0);
    let cwd = ctx.cwd.clone();
    let abs = absolutize(&cwd, &path);
    ctx.vfs.upper_mut().write_file(&abs, content);
    // Resize the SHARED buffer too, or an open descriptor's flush-on-close
    // resurrects the old size. One buffer per path means one resize reaches
    // every process that has it open.
    let mut resized = None;
    for (i, f) in ctx.open_files.iter_mut().enumerate() {
        if f.refs > 0 && absolutize(&cwd, &f.path) == abs {
            f.data.resize(len, 0);
            resized = Some(i);
        }
    }
    if let Some(i) = resized {
        // Collected first: the offsets live in their own table now, so walking
        // `fds` and clamping cannot hold two borrows of `ctx` at once. Clamping
        // a SHARED description also clamps it for whoever dup'd or forked it,
        // which is what sharing one means.
        let offs: Vec<usize> = ctx
            .fds
            .iter()
            .flatten()
            .filter_map(|e| match e {
                OpenFile::Mem { file, off, .. } if *file == i => Some(*off),
                _ => None,
            })
            .collect();
        for off in offs {
            if ctx.file_offsets[off].pos > len {
                ctx.file_offsets[off].pos = len;
            }
        }
    }
    state.set_ret(0);
}

/// renameat(olddirfd, oldpath, newdirfd, newpath): move a file within the overlay.
/// The content is copied to `newpath` in the tmpfs upper and `oldpath` is whited
/// out (dirfds are AT_FDCWD in practice: postgres renames temp files into place --
/// pg_control, relation/init forks). An open fd on the old path is repointed so its
/// flush-on-close lands at the new path. Directory rename is not modeled.
#[inline(never)]
fn sys_renameat(state: &mut State, ctx: &mut EcvContext) {
    let old = ctx.arena.read_cstr(state.arg(1));
    let new = ctx.arena.read_cstr(state.arg(3));
    let Some(content) = ctx.vfs.read(&ctx.cwd, &old) else {
        state.set_ret_err(ENOENT);
        return;
    };
    let cwd = ctx.cwd.clone();
    let old_abs = absolutize(&cwd, &old);
    let new_abs = absolutize(&cwd, &new);
    ctx.vfs.upper_mut().write_file(&new_abs, content);
    ctx.vfs.upper_mut().whiteout(&old_abs);
    // Repoint the shared entry, so an open descriptor's flush-on-close lands at
    // the NEW path. One entry per path means this is a single update rather
    // than a sweep over every process's descriptors -- which the previous
    // per-fd copy could only ever do for the CURRENT process anyway.
    for f in ctx.open_files.iter_mut() {
        if f.refs > 0 && absolutize(&cwd, &f.path) == old_abs {
            f.path = new_abs.clone();
        }
    }
    state.set_ret(0);
}

/// fallocate(fd, mode, offset, len): ensure the file spans at least offset+len
/// bytes (zero-filled). We treat every mode as plain allocation (the file is a
/// dense in-memory buffer, so punch-hole/collapse modes are not distinguishable
/// and preallocation is a no-op beyond the size guarantee).
#[inline(never)]
fn sys_fallocate(state: &mut State, ctx: &mut EcvContext) {
    let (fd, offset, len) = (
        state.arg(0) as usize,
        state.arg(2) as usize,
        state.arg(3) as usize,
    );
    match ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        Some(OpenFile::Mem { file, writable, .. }) => {
            if !*writable {
                state.set_ret_err(EBADF);
                return;
            }
            let file = *file;
            let f = &mut ctx.open_files[file];
            f.dirty = true;
            if offset + len > f.data.len() {
                f.data.resize(offset + len, 0);
            }
            state.set_ret(0);
        }
        None => state.set_ret_err(EBADF),
        _ => state.set_ret_err(EINVAL),
    }
}

/// Opens `path` as a shared in-memory file and returns its `open_files` index,
/// joining an entry that is already open on the same path.
///
/// Joining is the whole point: two descriptors on one path must see ONE buffer.
/// A fresh copy per descriptor is what let a PostgreSQL backend extend a
/// relation while another held a shorter copy of it.
/// Allocates a fresh open file description offset, at 0, with one reference.
///
/// The mirror image of `mem_file_for` and the contrast is the point: that one
/// JOINS an existing slot when the path matches, because two descriptors on one
/// path must see one buffer. This one never joins, because two `open`s of one
/// path must see two positions. Sharing happens only through `retain_entry`,
/// i.e. only where Linux shares a `struct file` -- `dup`, `fork`, SCM_RIGHTS.
///
/// Slots are recycled once nothing references them, exactly as `mem_file_for`
/// recycles file slots, and for the same reason: a live descriptor holds the
/// index, so reuse is safe only at zero.
fn alloc_file_offset(ctx: &mut EcvContext) -> usize {
    if let Some(i) = ctx.file_offsets.iter().position(|o| o.refs == 0) {
        ctx.file_offsets[i] = crate::context::FileOffset { pos: 0, refs: 1 };
        return i;
    }
    ctx.file_offsets
        .push(crate::context::FileOffset { pos: 0, refs: 1 });
    ctx.file_offsets.len() - 1
}

/// Drops a reference on an open file description. Frees the slot at zero; the
/// offset is not written back anywhere, so there is nothing to flush.
fn release_file_offset(ctx: &mut EcvContext, idx: usize) {
    let o = &mut ctx.file_offsets[idx];
    o.refs = o.refs.saturating_sub(1);
}

/// Drops everything a `Mem` descriptor holds: the file's reference and the open
/// file description's.
///
/// ONE function for the same reason `retain_entry` is one function. The rule --
/// a descriptor going away drops a reference on everything it names -- has two
/// call sites (`close_fd_full` and `release_entry`), and the last time this rule
/// was written out separately at each site, one of them dropped a pipe end and
/// not a mem file. That bug cost a day and presented as a descriptor silently
/// reading a different file.
fn release_mem_entry(ctx: &mut EcvContext, file: usize, off: usize) {
    release_mem_file(ctx, file);
    release_file_offset(ctx, off);
}

fn mem_file_for(ctx: &mut EcvContext, path: &[u8], data: Vec<u8>) -> usize {
    let trace = crate::diag::filetrace();
    if let Some(i) = ctx
        .open_files
        .iter()
        .position(|f| f.refs > 0 && f.path == path)
    {
        ctx.open_files[i].refs += 1;
        if trace {
            let f = &ctx.open_files[i];
            ecv_probe!(
                filetrace,
                "join idx={i} refs={} len={} path={}",
                f.refs,
                f.data.len(),
                String::from_utf8_lossy(&f.path)
            );
        }
        return i;
    }
    // Reuse a slot whose last descriptor closed; indices are held by live fds,
    // so a slot may only be recycled once nothing references it.
    if let Some(i) = ctx.open_files.iter().position(|f| f.refs == 0) {
        if trace {
            // The recycle is logged with BOTH paths. An index is only meaningful
            // together with the moment it is read at, and this line is the only
            // place its meaning changes.
            ecv_probe!(
                filetrace,
                "recycle idx={i} was={} now={} len={}",
                String::from_utf8_lossy(&ctx.open_files[i].path),
                String::from_utf8_lossy(path),
                data.len()
            );
        }
        ctx.open_files[i] = crate::context::MemFile {
            path: path.to_vec(),
            data,
            refs: 1,
            dirty: false,
        };
        return i;
    }
    ecv_probe!(
        filetrace,
        "new idx={} len={} path={}",
        ctx.open_files.len(),
        data.len(),
        String::from_utf8_lossy(path)
    );
    ctx.open_files.push(crate::context::MemFile {
        path: path.to_vec(),
        data,
        refs: 1,
        dirty: false,
    });
    ctx.open_files.len() - 1
}

/// Drops one reference to a shared in-memory file, flushing it to the tmpfs
/// upper layer when the LAST descriptor goes.
///
/// Flushing per descriptor instead is precisely the bug this replaced: a
/// process closing a stale copy would overwrite a newer one.
fn release_mem_file(ctx: &mut EcvContext, idx: usize) {
    if crate::diag::filetrace() {
        let f = &ctx.open_files[idx];
        ecv_probe!(
            filetrace,
            "release idx={idx} refs={}->{} len={} path={}",
            f.refs,
            f.refs.saturating_sub(1),
            f.data.len(),
            String::from_utf8_lossy(&f.path)
        );
    }
    let f = &mut ctx.open_files[idx];
    f.refs = f.refs.saturating_sub(1);
    if f.refs == 0 && f.dirty {
        let (path, data) = (f.path.clone(), f.data.clone());
        f.dirty = false;
        ctx.vfs.upper_mut().write_file(&path, data);
    }
}

/// Drops a reference taken by [`EcvContext::retain_entry`] on an entry that
/// never reached an fd table -- currently only a queued SCM_RIGHTS batch that a
/// plain `read` steps over.
///
/// The inverse of `retain_entry` and deliberately as narrow: mem file and pipe
/// ends, the two kinds that carry a counter. A socket right dropped this way
/// leaks its host fd, which is pre-existing -- `close_fd_full` refcounts sockets
/// by scanning fd tables, and a queued entry is in none of them, so the scan
/// could not have seen it before this either.
fn release_entry(ctx: &mut EcvContext, entry: OpenFile) {
    match entry {
        OpenFile::Mem { file, off, .. } => release_mem_entry(ctx, file, off),
        entry @ (OpenFile::Pipe { .. } | OpenFile::SocketPair { .. }) => {
            release_pipe_ends(ctx, &entry)
        }
        _ => {}
    }
}

/// Runs the `open_files` AND `file_offsets` refcount audits and reports any slot
/// that disagrees.
///
/// Called after the syscalls that CREATE or DESTROY a reference, not only at
/// context switch. Switch-time alone was measured to be useless for this: a
/// guest that dups, closes and churns without yielding violates and restores the
/// invariant entirely between two switches, so the probe stayed silent on a
/// deliberately re-injected `dup` bug. The check is O(open fds); these syscalls
/// are not hot, and it is behind a gate regardless.
#[inline]
fn fdcheck_after(ctx: &EcvContext, what: &str) {
    if !crate::diag::fdcheck() {
        return;
    }
    for (i, recorded, actual) in ctx.audit_mem_refs() {
        ecv_probe!(
            fdcheck,
            "after {what} idx={i} refs={recorded} actual={actual} {} path={}",
            if recorded < actual {
                "TOO LOW -- slot can be recycled under a live fd"
            } else {
                "too high -- leak"
            },
            String::from_utf8_lossy(&ctx.open_files[i].path)
        );
    }
    // The offset table is audited by the same rule and reported separately,
    // because the two counts are legitimately DIFFERENT -- two opens of one path
    // are one file and two offsets -- so a reader shown a single number could not
    // tell a real mismatch from that. `[fdcheck] offset` is the prefix; the
    // failure mode it exists for is a clone site that takes a reference on the
    // file and forgets the offset, which frees an offset slot under a live
    // descriptor and silently gives two unrelated files one position.
    for (i, recorded, actual) in ctx.audit_offset_refs() {
        ecv_probe!(
            fdcheck,
            "after {what} offset idx={i} refs={recorded} actual={actual} {} pos={}",
            if recorded < actual {
                "TOO LOW -- slot can be recycled under a live fd"
            } else {
                "too high -- leak"
            },
            ctx.file_offsets[i].pos
        );
    }
}

/// Fully closes fd `fd`: flushes a dirty in-memory file back to the tmpfs upper,
/// refcounts a pipe end (waking readers on the last writer), and closes a socket's
/// host fd. Returns false if the fd was not open. Shared by `sys_close` and the
/// execve close-on-exec sweep (`EcvContext::close_cloexec_fds`).
pub(crate) fn close_fd_full(ctx: &mut EcvContext, fd: usize) -> bool {
    let entry = ctx.fds.get_mut(fd).and_then(Option::take);
    if entry.is_some() {
        purge_epoll_interest(ctx, fd);
    }
    match entry {
        Some(OpenFile::Mem { file, off, .. }) => {
            release_mem_entry(ctx, file, off);
            true
        }
        Some(entry @ (OpenFile::Pipe { .. } | OpenFile::SocketPair { .. })) => {
            release_pipe_ends(ctx, &entry);
            true
        }
        Some(OpenFile::Socket { h }) => {
            // fork clones the fd table by VALUE, so a child and its parent share the
            // SAME backend handle. Closing it unconditionally means a child's
            // ClosePostmasterPorts() yanks the postmaster's listen socket out from
            // under it (handle -> BADF -> the ServerLoop accept() storm that stalls
            // "ready to accept connections"). Refcount by scan, like pipe ends: only
            // release the handle when no other fd of this process and no other
            // process still references it.
            let refd = |fds: &[Option<OpenFile>]| {
                fds.iter()
                    .flatten()
                    .any(|f| matches!(f, OpenFile::Socket { h: x } if *x == h))
            };
            let still_referenced = refd(&ctx.fds)
                || ctx
                    .procs
                    .iter()
                    .enumerate()
                    .any(|(i, p)| i != ctx.current && p.fds.as_deref().is_some_and(refd));
            if !still_referenced {
                // ⚠️ This used to be a libc `close(2)` on the raw descriptor,
                // which only worked because a WasmEdge socket handle happens to
                // BE a WASI fd. The newtype made that a type error, which is
                // most of why it exists.
                crate::net::NetBackend::close(&mut ctx.net, h);
            }
            true
        }
        Some(OpenFile::UnixSocket { listener: Some(l) }) => {
            // Same refcount hazard as the host socket above, and the same guest
            // does it: the postmaster binds the UDS listener and forks, and each
            // backend's ClosePostmasterPorts() closes its inherited copy. Marking
            // the listener dead on the first of those would unbind the socket
            // the postmaster is still accepting on.
            let refd = |fds: &[Option<OpenFile>]| {
                fds.iter()
                    .flatten()
                    .any(|f| matches!(f, OpenFile::UnixSocket { listener: Some(h) } if *h == l))
            };
            let still_referenced = refd(&ctx.fds)
                || ctx
                    .procs
                    .iter()
                    .enumerate()
                    .any(|(i, p)| i != ctx.current && p.fds.as_deref().is_some_and(refd));
            if !still_referenced {
                ctx.unix_listeners[l].dead = true;
                // Connections queued but never accepted die with the listener.
                // Releasing both ends is what makes the peer's read return EOF
                // instead of hanging: a client that connected to a server which
                // exited before accepting must see ECONNRESET-ish EOF, not wait
                // forever for a process that no longer exists.
                let pending: Vec<(usize, usize)> =
                    ctx.unix_listeners[l].pending.drain(..).collect();
                for (rx, tx) in pending {
                    release_pipe_ends(ctx, &OpenFile::SocketPair { rx, tx });
                }
            }
            true
        }
        Some(_) => true,
        None => false,
    }
}

#[inline(never)]
fn sys_read(state: &mut State, ctx: &mut EcvContext) {
    // On resume svc has already stop_rewind'd; a resumed read just re-attempts,
    // which is identical to a fresh read (blocks again on a spurious wakeup).
    let (fd, buf, count) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    // Pipe read ends and sockets are handled separately (they can block).
    match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Pipe { idx, write: false }) => {
            let idx = *idx;
            return read_pipe(state, ctx, fd, idx, buf, count);
        }
        Some(OpenFile::Socket { h }) => {
            let h = *h;
            return socket_recv(state, ctx, fd, h, buf, count);
        }
        Some(OpenFile::SignalFd { mask }) => {
            let mask = *mask;
            return read_signalfd(state, ctx, mask, buf, count);
        }
        // A socketpair read is a pipe read on its rx direction. Any SCM_RIGHTS
        // batch queued on that direction is dropped, exactly as the kernel drops
        // rights a plain read(2) steps over -- only recvmsg can claim them.
        Some(OpenFile::SocketPair { rx, .. }) => {
            let rx = *rx;
            // Dropping the batch has to drop its references too, now that the
            // queue holds them: a discarded right that kept its reference would
            // pin an `open_files` slot forever. Leaking is the safe direction --
            // a pinned slot is only wasted memory, where a missing reference
            // retargets a live descriptor -- but it is still wrong.
            if let Some(rights) = ctx.pipes[rx].scm.pop_front() {
                for entry in rights {
                    release_entry(ctx, entry);
                }
            }
            return read_pipe(state, ctx, fd, rx, buf, count);
        }
        // eventfd read: 8 bytes, draining the counter (or taking 1 under
        // EFD_SEMAPHORE). A zero counter must NOT return 0 -- that reads as EOF --
        // so it is EAGAIN and the caller polls. Every eventfd user reaches it
        // through epoll, so blocking here would only add a path nothing takes.
        Some(OpenFile::EventFd {
            count: c,
            semaphore,
        }) => {
            let (c, semaphore) = (*c, *semaphore);
            if count < 8 {
                state.set_ret_err(EINVAL);
            } else if c == 0 {
                state.set_ret_err(EAGAIN);
            } else {
                let taken = if semaphore { 1 } else { c };
                if let Some(OpenFile::EventFd { count: c, .. }) =
                    ctx.fds.get_mut(fd).and_then(|s| s.as_mut())
                {
                    *c -= taken;
                }
                ctx.arena
                    .slice_mut(buf, 8)
                    .copy_from_slice(&taken.to_le_bytes());
                state.set_ret(8);
            }
            return;
        }
        _ => {}
    }
    match ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        Some(OpenFile::Null { zero }) => {
            // /dev/zero yields as many zero bytes as asked for; /dev/null is
            // always at end of file.
            let n = if *zero { count } else { 0 };
            if n > 0 {
                ctx.arena.slice_mut(buf, n).fill(0);
            }
            state.set_ret(n as u64);
        }
        Some(OpenFile::Stdio(hostfd)) => {
            let hostfd = *hostfd;
            let p = ctx.arena.translate(buf);
            let n = unsafe { read(hostfd, p, count) };
            set_io_result(state, n);
        }
        Some(OpenFile::Mem { file, off, .. }) => {
            let (file, off) = (*file, *off);
            let at = ctx.file_offsets[off].pos;
            let data = &ctx.open_files[file].data;
            let n = data.len().saturating_sub(at).min(count);
            let chunk = data[at..at + n].to_vec();
            if crate::diag::filetrace() {
                let f = &ctx.open_files[file];
                ecv_probe!(
                    filetrace,
                    "read fd={fd} idx={file} off={at} count={count} n={n} len={} path={}",
                    f.data.len(),
                    String::from_utf8_lossy(&f.path)
                );
            }
            ctx.file_offsets[off].pos = at + n;
            ctx.arena.slice_mut(buf, n).copy_from_slice(&chunk);
            state.set_ret(n as u64);
        }
        Some(OpenFile::Pipe { .. }) => state.set_ret_err(EBADF), // write end
        Some(OpenFile::Socket { .. }) => unreachable!("socket read handled above"),
        Some(OpenFile::SignalFd { .. }) => unreachable!("signalfd read handled above"),
        Some(OpenFile::SocketPair { .. }) => unreachable!("socketpair read handled above"),
        // An AF_UNIX socket that has not been connected. Linux answers
        // ENOTCONN, and it matters that this is not EBADF: a guest that dials
        // and then reads without checking connect's result must see the reason.
        Some(OpenFile::UnixSocket { .. }) => state.set_ret_err(ENOTCONN),
        Some(OpenFile::EventFd { .. }) => unreachable!("eventfd read handled above"),
        Some(OpenFile::Epoll { .. }) => state.set_ret_err(EINVAL), // not readable
        Some(OpenFile::Dir { .. }) => state.set_ret_err(EISDIR),
        None => state.set_ret_err(EBADF),
    }
}

/// Reads a signalfd: consume ONE pending signal selected by `mask` and hand back
/// a `struct signalfd_siginfo` (128 bytes; only ssi_signo is meaningful here).
/// Blocks when nothing is pending -- woken by `kill` via `wake_pollers`.
fn read_signalfd(state: &mut State, ctx: &mut EcvContext, mask: u64, buf: u64, count: usize) {
    if count < SIGNALFD_SIGINFO_SIZE {
        state.set_ret_err(EINVAL);
        return;
    }
    // Both queues: a signalfd is a descriptor of the process, and the signal it
    // is waiting for may have been directed at the process (`kill`) or at this
    // thread (`tgkill`).
    let avail = (ctx.signals.pending | ctx.task_signals.pending) & mask;
    if avail == 0 {
        // Same last-runnable-process guard as epoll_pwait: never park the only
        // runnable process, or the module deadlocks.
        let others_runnable = ctx
            .procs
            .iter()
            .enumerate()
            .any(|(i, p)| i != ctx.current && p.status == crate::context::ProcStatus::Runnable);
        if others_runnable {
            ctx.block_current(BlockedOn::Poll);
            ctx.suspended = true;
        } else {
            state.set_ret_err(EAGAIN);
        }
        return;
    }
    let sig = avail.trailing_zeros() + 1; // lowest-numbered pending signal
    ctx.consume_signal(1u64 << (sig - 1));
    let b = ctx.arena.slice_mut(buf, SIGNALFD_SIGINFO_SIZE);
    b.fill(0);
    b[0..4].copy_from_slice(&sig.to_le_bytes()); // ssi_signo
    state.set_ret(SIGNALFD_SIGINFO_SIZE as u64);
}

/// Reads from a pipe: return buffered data; EOF (0) if the write end is closed;
/// EAGAIN if `fd` is non-blocking; otherwise block until a write or close wakes
/// us. `fd` is carried purely for that flag -- nothing external will ever make an
/// internal pipe readable, so a non-blocking reader that blocks here never wakes.
fn read_pipe(
    state: &mut State,
    ctx: &mut EcvContext,
    fd: usize,
    idx: usize,
    buf: u64,
    count: usize,
) {
    if !ctx.pipes[idx].buf.is_empty() {
        let n = count.min(ctx.pipes[idx].buf.len());
        let data: Vec<u8> = ctx.pipes[idx].buf.drain(..n).collect();
        ctx.arena.slice_mut(buf, n).copy_from_slice(&data);
        state.set_ret(n as u64);
    } else if ctx.pipes[idx].writers == 0 {
        state.set_ret(0); // EOF
    } else if ctx.is_nonblock(fd) {
        state.set_ret_err(EAGAIN);
    } else {
        ctx.block_current(BlockedOn::PipeRead(idx));
        ctx.suspended = true; // svc unwinds
    }
}

/// Writes to a pipe: EPIPE once the last reader is gone, otherwise append and
/// wake anyone parked on it. The buffer is unbounded, so a write never blocks --
/// which is why the socketpair channel cannot deadlock a cooperative scheduler
/// that has no way to preempt a writer stuck on a full buffer.
fn write_pipe(state: &mut State, ctx: &mut EcvContext, idx: usize, buf: u64, count: usize) {
    if ctx.pipes[idx].readers == 0 {
        state.set_ret_err(EPIPE);
        return;
    }
    let src = ctx.arena.slice(buf, count).to_vec();
    ctx.pipes[idx].buf.extend(src);
    ctx.wake_pipe_readers(idx);
    state.set_ret(count as u64);
}

#[inline(never)]
fn sys_write(state: &mut State, ctx: &mut EcvContext) {
    // On resume svc has already stop_rewind'd; a resumed write just re-attempts.
    let (fd, buf, count) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Pipe { idx, write: true }) => {
            let idx = *idx;
            return write_pipe(state, ctx, idx, buf, count);
        }
        Some(OpenFile::Socket { h }) => {
            let h = *h;
            return socket_send(state, ctx, fd, h, buf, count);
        }
        Some(OpenFile::SocketPair { tx, .. }) => {
            let tx = *tx;
            return write_pipe(state, ctx, tx, buf, count);
        }
        // eventfd write: add an 8-byte value to the counter, which makes it
        // readable and so wakes anything polling it.
        Some(OpenFile::EventFd { .. }) => {
            if count < 8 {
                state.set_ret_err(EINVAL);
                return;
            }
            let add = u64::from_le_bytes(ctx.arena.slice(buf, 8).try_into().unwrap());
            if let Some(OpenFile::EventFd { count: c, .. }) =
                ctx.fds.get_mut(fd).and_then(|s| s.as_mut())
            {
                *c = c.saturating_add(add);
            }
            ctx.wake_pollers();
            state.set_ret(8);
            return;
        }
        _ => {}
    }
    match ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        // Both devices swallow writes and report the full count.
        Some(OpenFile::Null { .. }) => state.set_ret(count as u64),
        Some(OpenFile::Stdio(hostfd)) => {
            let hostfd = *hostfd;
            let p = ctx.arena.translate(buf);
            let n = unsafe { write(hostfd, p, count) };
            set_io_result(state, n);
        }
        Some(OpenFile::Mem {
            file,
            off,
            writable,
            ..
        }) => {
            if !*writable {
                state.set_ret_err(EBADF);
                return;
            }
            let (file, off) = (*file, *off);
            let at = ctx.file_offsets[off].pos;
            let src = ctx.arena.slice(buf, count).to_vec();
            let f = &mut ctx.open_files[file];
            if at + count > f.data.len() {
                f.data.resize(at + count, 0);
            }
            f.data[at..at + count].copy_from_slice(&src);
            f.dirty = true;
            ctx.file_offsets[off].pos = at + count;
            state.set_ret(count as u64);
        }
        Some(OpenFile::Pipe { .. }) => state.set_ret_err(EBADF), // read end
        Some(OpenFile::Socket { .. }) => unreachable!("socket write handled above"),
        Some(OpenFile::SocketPair { .. }) => unreachable!("socketpair write handled above"),
        // An AF_UNIX socket that has not been connected. Linux answers
        // ENOTCONN, and it matters that this is not EBADF: a guest that dials
        // and then reads without checking connect's result must see the reason.
        Some(OpenFile::UnixSocket { .. }) => state.set_ret_err(ENOTCONN),
        Some(OpenFile::EventFd { .. }) => unreachable!("eventfd write handled above"),
        // signalfd/epoll fds are not writable.
        Some(OpenFile::SignalFd { .. }) | Some(OpenFile::Epoll { .. }) => state.set_ret_err(EINVAL),
        Some(OpenFile::Dir { .. }) => state.set_ret_err(EBADF),
        None => state.set_ret_err(EBADF),
    }
}

#[inline(never)]
fn sys_lseek(state: &mut State, ctx: &mut EcvContext) {
    let (fd, off, whence) = (state.arg(0) as usize, state.arg(1) as i64, state.arg(2));
    match ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        Some(OpenFile::Mem {
            file, off: desc, ..
        }) => {
            let (file, desc) = (*file, *desc);
            let len = ctx.open_files[file].data.len();
            let base = match whence {
                0 => 0i64,                              // SEEK_SET
                1 => ctx.file_offsets[desc].pos as i64, // SEEK_CUR
                2 => len as i64,                        // SEEK_END
                _ => {
                    state.set_ret_err(EINVAL);
                    return;
                }
            };
            let np = base + off;
            if np < 0 {
                state.set_ret_err(EINVAL);
            } else {
                ctx.file_offsets[desc].pos = np as usize;
                state.set_ret(np as u64);
            }
        }
        Some(_) => state.set_ret_err(ESPIPE),
        None => state.set_ret_err(EBADF),
    }
}

#[inline(never)]
fn sys_fstat(state: &mut State, ctx: &mut EcvContext) {
    let fd = state.arg(0) as usize;
    let (mode, size) = match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Stdio(_)) => (S_IFCHR | 0o620, 0u64),
        // Real /dev/null and /dev/zero are character devices, 0666.
        Some(OpenFile::Null { .. }) => (S_IFCHR | 0o666, 0),
        Some(OpenFile::Socket { .. }) => (S_IFSOCK | 0o777, 0),
        Some(OpenFile::Mem { file, .. }) => {
            (S_IFREG | 0o644, ctx.open_files[*file].data.len() as u64)
        }
        Some(OpenFile::Pipe { .. }) => (S_IFIFO | 0o600, 0),
        // A socketpair end is a socket, and nginx's channel code stats it.
        Some(OpenFile::SocketPair { .. }) | Some(OpenFile::UnixSocket { .. }) => {
            (S_IFSOCK | 0o777, 0)
        }
        // signalfd/epoll/eventfd are anon-inode fds on Linux; report a plain
        // character device so a caller that stats them sees something coherent.
        Some(OpenFile::SignalFd { .. })
        | Some(OpenFile::Epoll { .. })
        | Some(OpenFile::EventFd { .. }) => (S_IFCHR | 0o600, 0),
        Some(OpenFile::Dir { .. }) => (S_IFDIR | 0o755, 0),
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };
    let mut st = [0u8; 128];
    fill_stat(&mut st, mode, size, 0, ctx.uid, ctx.gid);
    ctx.arena.slice_mut(state.arg(1), 128).copy_from_slice(&st);
    state.set_ret(0);
}

#[inline(never)]
fn sys_newfstatat(state: &mut State, ctx: &mut EcvContext) {
    let dirfd = state.arg(0) as i64;
    let path = ctx.arena.read_cstr(state.arg(1));
    let flags = state.arg(3);
    if flags & AT_EMPTY_PATH != 0 && path.is_empty() {
        // fstat of the dirfd.
        state.gpr.x[0].val = dirfd as u64;
        state.gpr.x[1].val = state.arg(2);
        sys_fstat(state, ctx);
        return;
    }
    let follow = flags & AT_SYMLINK_NOFOLLOW == 0;
    match resolve_arg(ctx, dirfd, &path, follow) {
        Some(r) => {
            let mut st = [0u8; 128];
            // Report the CURRENT process's uid/gid as the owner, matching fd-based
            // fstat (which uses ctx.uid/ctx.gid). The rootfs does not preserve host
            // ownership (every inode is uid/gid 0), so using meta.uid here would make
            // a program that checks `st_uid == geteuid()` -- e.g. PostgreSQL's
            // checkDataDir on PGDATA -- fail whenever the process runs as non-root.
            fill_stat(
                &mut st,
                r.meta.mode,
                r.meta.size,
                r.meta.mtime,
                ctx.uid,
                ctx.gid,
            );
            ctx.arena.slice_mut(state.arg(2), 128).copy_from_slice(&st);
            ecv_trace!(
                ecv,
                "newfstatat {:?} -> mode=0o{:o} uid={} gid={} size={}",
                String::from_utf8_lossy(&path),
                r.meta.mode,
                ctx.uid,
                ctx.gid,
                r.meta.size
            );
            state.set_ret(0);
        }
        None => state.set_ret_err(ENOENT),
    }
}

/// pread64(fd, buf, count, offset): read at an explicit offset without moving the
/// fd position. Only in-memory files are supported (postgres pread/pwrite the
/// postmaster.pid lock file); pipes/sockets are unseekable (ESPIPE).
#[inline(never)]
fn sys_pread64(state: &mut State, ctx: &mut EcvContext) {
    let (fd, buf, count, off) = (
        state.arg(0) as usize,
        state.arg(1),
        state.arg(2) as usize,
        state.arg(3) as usize,
    );
    match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Mem { file, .. }) => {
            let file = *file;
            let data = &ctx.open_files[file].data;
            let n = data.len().saturating_sub(off).min(count);
            let chunk = data[off..off + n].to_vec();
            // The decisive line for a short read that upper layers do not expect:
            // PostgreSQL reading block N of an N-block relation lands here with
            // n=0, and reading a full block from the WRONG file lands here with
            // n=count and a path that is not the one it asked for.
            if crate::diag::filetrace() {
                let f = &ctx.open_files[file];
                ecv_probe!(
                    filetrace,
                    "pread fd={fd} idx={file} off={off} count={count} n={n} len={} path={}",
                    f.data.len(),
                    String::from_utf8_lossy(&f.path)
                );
            }
            ctx.arena.slice_mut(buf, n).copy_from_slice(&chunk);
            state.set_ret(n as u64);
        }
        Some(OpenFile::Pipe { .. }) | Some(OpenFile::Socket { .. }) => state.set_ret_err(ESPIPE),
        None => state.set_ret_err(EBADF),
        _ => state.set_ret_err(EINVAL),
    }
}

/// pwrite64(fd, buf, count, offset): write at an explicit offset without moving
/// the fd position, extending the file if needed. postgres uses this to update
/// individual lines of the data-directory lock file (AddToDataDirLockFile).
#[inline(never)]
fn sys_pwrite64(state: &mut State, ctx: &mut EcvContext) {
    let (fd, buf, count, off) = (
        state.arg(0) as usize,
        state.arg(1),
        state.arg(2) as usize,
        state.arg(3) as usize,
    );
    let src = ctx.arena.slice(buf, count).to_vec();
    match ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        Some(OpenFile::Mem { file, writable, .. }) => {
            if !*writable {
                state.set_ret_err(EBADF);
                return;
            }
            let file = *file;
            let f = &mut ctx.open_files[file];
            f.dirty = true;
            if off + count > f.data.len() {
                f.data.resize(off + count, 0);
            }
            f.data[off..off + count].copy_from_slice(&src);
            state.set_ret(count as u64);
        }
        Some(OpenFile::Pipe { .. }) | Some(OpenFile::Socket { .. }) => state.set_ret_err(ESPIPE),
        None => state.set_ret_err(EBADF),
        _ => state.set_ret_err(EINVAL),
    }
}

#[inline(never)]
fn sys_getdents64(state: &mut State, ctx: &mut EcvContext) {
    let (fd, dirp, count) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    let Some(OpenFile::Dir { entries, pos, .. }) = ctx.fds.get_mut(fd).and_then(|s| s.as_mut())
    else {
        state.set_ret_err(if ctx.fds.get(fd).map_or(true, |s| s.is_none()) {
            EBADF
        } else {
            ENOTDIR
        });
        return;
    };
    // Serialize linux_dirent64 records into a scratch buffer first.
    let mut out: Vec<u8> = Vec::new();
    let mut consumed = 0usize;
    // "." and ".." first (only at pos 0/1).
    let synth: [(&[u8], NodeKind); 2] = [(b".", NodeKind::Dir), (b"..", NodeKind::Dir)];
    let total = entries.len() + 2;
    let mut i = *pos;
    while i < total {
        let (name, kind) = if i < 2 {
            synth[i]
        } else {
            let (ref n, k) = entries[i - 2];
            (n.as_slice(), k)
        };
        let reclen = dirent_reclen(name.len());
        if consumed + reclen > count {
            break;
        }
        encode_dirent64(&mut out, (i as u64) + 1, kind, name);
        consumed += reclen;
        i += 1;
    }
    *pos = i;
    if !out.is_empty() {
        ctx.arena.slice_mut(dirp, out.len()).copy_from_slice(&out);
    }
    state.set_ret(out.len() as u64);
}

#[inline(never)]
fn sys_getcwd(state: &mut State, ctx: &mut EcvContext) {
    let (buf, size) = (state.arg(0), state.arg(1) as usize);
    let need = ctx.cwd.len() + 1;
    if need > size {
        state.set_ret_err(ERANGE);
        return;
    }
    let dst = ctx.arena.slice_mut(buf, need);
    dst[..ctx.cwd.len()].copy_from_slice(&ctx.cwd);
    dst[ctx.cwd.len()] = 0;
    state.set_ret(need as u64);
}

#[inline(never)]
fn sys_readlinkat(state: &mut State, ctx: &mut EcvContext) {
    let dirfd = state.arg(0) as i64;
    let path = ctx.arena.read_cstr(state.arg(1));
    let (buf, bufsiz) = (state.arg(2), state.arg(3) as usize);
    // Relative paths resolve against the dirfd's directory; the errno is
    // unchanged from when this refused every such call, so only an
    // UNUSABLE dirfd reaches it now.
    let Some(base) = ctx.resolve_base(dirfd, &path).map(|b| b.to_vec()) else {
        state.set_ret_err(EINVAL);
        return;
    };
    match ctx.vfs.readlink(&base, &path) {
        Some(target) => {
            let n = target.len().min(bufsiz);
            ctx.arena.slice_mut(buf, n).copy_from_slice(&target[..n]);
            state.set_ret(n as u64);
        }
        None => state.set_ret_err(EINVAL),
    }
}

/// fchmod(fd, mode) and fchmodat(dirfd, path, mode, flags). The new permissions
/// are recorded in the overlay's upper layer, so a path that came from the
/// read-only image can still be chmod-ed -- which is what initdb does to PGDATA.
#[inline(never)]
fn sys_fchmodat(state: &mut State, ctx: &mut EcvContext) {
    let nr = state.syscall_nr();
    let (target, mode) = if nr == NR_FCHMOD {
        // fchmod(fd, mode): resolve the fd back to the path it was opened from.
        let fd = state.arg(0) as usize;
        let path = match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
            Some(OpenFile::Mem { file, .. }) => ctx.open_files[*file].path.clone(),
            // Only `Mem` records the path it was opened from, so fchmod on any
            // other descriptor cannot be turned into an overlay override. Report
            // success, as this whole family did before, rather than invent an
            // error a guest has no reason to expect -- but say so, so the gap is
            // discoverable instead of silent. No guest has needed it yet:
            // initdb chmods PGDATA by PATH.
            _ => {
                ecv_debug!(ecv, "fchmod on fd {fd} ignored (no path recorded for it)");
                state.set_ret(0);
                return;
            }
        };
        (path, state.arg(1) as u32)
    } else {
        let dirfd = state.arg(0) as i64;
        let path = ctx.arena.read_cstr(state.arg(1));
        match resolve_arg(ctx, dirfd, &path, true) {
            Some(r) => (r.path, state.arg(2) as u32),
            None => {
                state.set_ret_err(ENOENT);
                return;
            }
        }
    };
    ecv_debug!(
        ecv,
        "chmod {:?} -> {:#o}",
        String::from_utf8_lossy(&target),
        mode & 0o7777
    );
    ctx.vfs.upper_mut().set_mode(&target, mode);
    state.set_ret(0);
}

#[inline(never)]
fn sys_faccessat(state: &mut State, ctx: &mut EcvContext) {
    let dirfd = state.arg(0) as i64;
    let path = ctx.arena.read_cstr(state.arg(1));
    match resolve_arg(ctx, dirfd, &path, true) {
        Some(_) => state.set_ret(0),
        None => state.set_ret_err(ENOENT),
    }
}

/// sched_yield: the asyncify suspend/resume proof. On the normal call it
/// unwinds the wasm stack into the process's async buffer (returning up through
/// the lifted + Rust frames to the scheduler); on resume the scheduler rewinds
/// back here and it finishes. This is the mechanism fork/pipes/wait4 build on.
#[inline(never)]
fn sys_sched_yield(state: &mut State, ctx: &mut EcvContext) {
    if ctx.resuming {
        // The scheduler rewound us back to this point (svc did stop_rewind).
        state.set_ret(0);
        return;
    }
    // Suspend: flag it for the scheduler; svc unwinds. Returning from here (and
    // every frame above) propagates the unwind up to the scheduler.
    ctx.pending = Pending::Yield;
    ctx.suspended = true;
}

/// exit / exit_group: route through the scheduler (marks the process a zombie,
/// wakes a waiting parent) rather than killing the whole module. The module
/// exits only when nothing is left to run.
///
/// The two are NOT the same call once threads exist. `exit_group` retires the
/// whole thread group; `exit` retires one task, and only takes the group with it
/// when it is the last member. Routing a worker thread's `exit` through the
/// group path closes the process's descriptors and posts SIGCHLD to its
/// parent -- from the parent's point of view the server has died.
#[inline(never)]
fn sys_exit(state: &mut State, ctx: &mut EcvContext, group: bool) {
    let code = state.arg(0) as i32;
    // `exit` from the last remaining task IS a group exit: someone has to close
    // the descriptors and tell the parent, and there is no one left to do it.
    let whole_group = group || !ctx.current_has_siblings();
    ecv_trace!(
        exit,
        "tgid={} code={} {}",
        ctx.current_tgid(),
        code,
        if whole_group { "group" } else { "thread" }
    );
    ctx.pending = if whole_group {
        Pending::Exit(ExitReason::Exited(code))
    } else {
        Pending::ExitThread(code)
    };
    ctx.suspended = true; // svc unwinds
}

/// clone: fork and vfork (real shared-VM threads unsupported). Snapshots the
/// caller into a child (child returns 0, parent gets the child pid) and suspends so
/// the scheduler can copy the parent's stack into the child and run both.
///
/// vfork (`CLONE_VM | CLONE_VFORK`, which dash and other shells use to spawn
/// commands) is handled as an ordinary fork: its contract only guarantees that the
/// child touches nothing but does an immediate execve/_exit, and execve resets the
/// child's arena+state anyway, so a copy-on-fork child is observably identical for
/// that pattern. We do NOT try to honor vfork's parent-suspend-until-exec (the
/// shell wait4()s the child right after, which blocks equivalently).
///
/// `CLONE_THREAD` is a real shared-VM thread (`EcvContext::clone_thread`): a new
/// task in the caller's thread group, running on the caller's arena and fd table
/// rather than a snapshot of them. The cooperative scheduler is what permits it —
/// it switches only at a block, a yield or an exit, so two threads of one group
/// never interleave mid-instruction.
#[inline(never)]
fn sys_clone(state: &mut State, ctx: &mut EcvContext) {
    if ctx.resuming {
        // svc already stop_rewind'd; x0 (parent: child pid, child: 0) is already in
        // the loaded State, so a resumed clone has nothing left to do.
        ecv_trace!(clone, "RESUME x0={:#x}", state.arg(0));
        return;
    }
    // aarch64 selects CONFIG_CLONE_BACKWARDS, so the argument order is
    //   clone(flags, child_stack, parent_tidptr, tls, child_tidptr)
    // -- tls at x3 and ctid at x4, NOT the x86-64 order. Reading them the other
    // way round installs a tid pointer as the thread pointer, which is not a
    // crash: it is a thread whose TLS silently aliases somebody's stack.
    const CLONE_THREAD: u64 = 0x0001_0000;
    const CLONE_SETTLS: u64 = 0x0008_0000;
    const CLONE_PARENT_SETTID: u64 = 0x0010_0000;
    const CLONE_CHILD_CLEARTID: u64 = 0x0020_0000;
    const CLONE_CHILD_SETTID: u64 = 0x0100_0000;
    let (flags, child_stack, ptid, tls, ctid) = (
        state.arg(0),
        state.arg(1),
        state.arg(2),
        state.arg(3),
        state.arg(4),
    );
    if flags & CLONE_THREAD != 0 {
        // A thread with no stack of its own would run on its creator's, which is
        // the one thing the caller cannot have meant. Every libc passes one.
        if child_stack == 0 {
            state.set_ret_err(EINVAL);
            return;
        }
        let tp = if flags & CLONE_SETTLS != 0 {
            tls
        } else {
            state.sr.tpidr_el0
        };
        let tid = ctx.clone_thread(
            child_stack,
            tp,
            if flags & CLONE_CHILD_CLEARTID != 0 {
                ctid
            } else {
                0
            },
        );
        // The new thread's static TLS blocks, copied from the image's own
        // templates. glibc allocates the block and then fills it in
        // `_dl_allocate_tls_init`, which walks `GL(dl_tls_dtv_slotinfo_list)`
        // -- a structure ld.so builds and a fused image never has, so glibc
        // copies NOTHING and every `__thread` variable with a non-zero
        // initialiser reads zero in the new thread. Silent wrong data, not a
        // crash; `e2e/rtldhooks_test.go` is what caught it, at round 0.
        //
        // Doing it here is the same work `setup_tls` does for the initial
        // thread, at the child's thread pointer instead. The ordering is what
        // makes it safe: the libc has finished with the block before it issues
        // the clone, and the child has not run yet.
        //
        // ⚠️ Module blocks ONLY -- no `tls_zero_tcb`. On aarch64 the TCB sits AT
        // the thread pointer and holds the DTV pointer the libc just installed;
        // zeroing it the way the initial-thread path does would break
        // `__tls_get_addr` for every dynamic access.
        if flags & CLONE_SETTLS != 0 {
            unsafe { ctx.init_thread_tls_modules(tp) };
        }
        // Both SETTID flags write the SAME value into the SAME shared address
        // space, so there is no ordering hazard between them here. musl's
        // pthread_create uses the parent form (`&new->tid`) and reads it as soon
        // as clone returns; glibc uses it too. Skipping it leaves every thread
        // believing its tid is 0.
        //
        // Both are guest pointers and neither may be trusted: a libc whose
        // thread bring-up did not complete computes them from zeroed state and
        // passes something like 0xffffffffffffff58. Writing through that is a
        // host-side PANIC, which reads as a defect in the runtime rather than in
        // the guest's own state -- and it is how this was actually found.
        for (flag, addr) in [(CLONE_PARENT_SETTID, ptid), (CLONE_CHILD_SETTID, ctid)] {
            if flags & flag == 0 || addr == 0 {
                continue;
            }
            if !ctx.arena.in_bounds(addr, 4) {
                ecv_debug!(
                    clone,
                    "tid pointer {addr:#x} is outside the arena; not written"
                );
                continue;
            }
            ctx.arena
                .slice_mut(addr, 4)
                .copy_from_slice(&tid.to_le_bytes());
        }
        // The creator does NOT suspend, exactly as in the fork case below: x0 is
        // already the tid and the new task is queued for the next switch.
        return;
    }
    let child = ctx.fork_current();
    ecv_trace!(
        clone,
        "FORK parent={} -> child={}",
        ctx.current_pid(),
        child
    );
    // The parent does NOT suspend: it returns from `clone` (x0=child pid) on its
    // intact native stack. `fork_current` enqueued the child as a replay-start
    // process; the scheduler reconstructs its stack by re-entry (entry.rs), not by
    // asyncify-rewinding the lifted glibc fork frames (which cannot be rewound —
    // the whole reason for --fork_emulation + call-history replay). See DYNLINK.md.
}

/// wait4: reap a zombie child, or block until one exits.
#[inline(never)]
fn sys_wait4(state: &mut State, ctx: &mut EcvContext) {
    let reap = |state: &mut State, ctx: &mut EcvContext| -> bool {
        let target = state.arg(0) as u32;
        let wstatus = state.arg(1);
        if let Some((pid, reason)) = ctx.reap_zombie(target) {
            if wstatus != 0 {
                // ⚠️ The ENCODING lives in `ExitReason::wait_status`, not here.
                // `mod sys` is `#[cfg(target_arch = "wasm32")]`, so a `#[test]`
                // written beside this line would never run under `cargo test`;
                // the pure function is in `context.rs` precisely so the
                // exited/killed round trip is a host test.
                let ws = reason.wait_status();
                ctx.arena
                    .slice_mut(wstatus, 4)
                    .copy_from_slice(&ws.to_le_bytes());
            }
            state.set_ret(pid as u64);
            true
        } else {
            false
        }
    };

    if ctx.resuming {
        // A child exited and the scheduler rewound us (svc did stop_rewind); reap it.
        if !reap(state, ctx) {
            state.set_ret_err(ECHILD);
        }
        return;
    }
    if reap(state, ctx) {
        return;
    }
    if !ctx.has_children() {
        state.set_ret_err(ECHILD);
        return;
    }
    // WNOHANG: a non-blocking poll -- no reapable child right now, so return 0
    // (NOT an error, NOT a block). PostgreSQL's postmaster reaper calls
    // waitpid(-1, &status, WNOHANG) in a loop and relies on the 0 return to STOP
    // reaping and go back to its ServerLoop epoll (where it accepts connections);
    // blocking here wedged it out of the accept loop after it reaped the startup
    // process, so a client connect just timed out. wait4 options is arg 2.
    const WNOHANG: u64 = 1;
    if state.arg(2) & WNOHANG != 0 {
        state.set_ret(0);
        return;
    }
    // Block until a child exits (the scheduler wakes us); svc unwinds.
    ctx.block_current(BlockedOn::Wait);
    ctx.suspended = true;
}

/// futex: FUTEX_WAIT + FUTEX_WAKE on a word inside a shared segment, the
/// inter-process lock/condvar primitive. Mirrors the wait4/pipe-read blocking
/// pattern under the cooperative scheduler (self-contained: `resuming`-check +
/// stop_rewind at the top, block + start_unwind on the WAIT path).
///
/// FUTEX_WAIT (and WAIT_BITSET): read the 32-bit word at guest `uaddr` from the
/// LIVE arena; if `*uaddr != val` return -EAGAIN immediately (the standard
/// race-avoidance fast path — this alone satisfies uncontended single-threaded
/// glibc, which never actually sleeps). If `*uaddr == val`, block on
/// BlockedOn::Futex and unwind, exactly like read_pipe; on resume (a FUTEX_WAKE
/// reached us) the handler re-runs and returns 0, and the guest re-checks its
/// own condition. FUTEX_WAKE (and WAKE_BITSET): wake up to `val` waiters on
/// `uaddr` and return the count. Other ops return -ENOSYS.
///
/// The word lives in a MAP_SHARED segment, so it is the same physical bytes for
/// every cooperatively-scheduled process: process B's write + FUTEX_WAKE reach
/// process A that blocked on the same VMA. FUTEX_WAIT MUST truly suspend — with
/// no preemption, a busy-return would monopolize the CPU and the waker would
/// never run. Gaps (prototype): the timeout argument is IGNORED (WAIT blocks
/// indefinitely); FUTEX_REQUEUE/CMP_REQUEUE and the bitset masks are not
/// honored. See .agents/docs/SHAREDMEM.md.
/// rt_sigaction(signum, act, oldact, sigsetsize): records the per-process
/// disposition for `signum` and returns the previous one in `oldact`. SIG_DFL(0)
/// / SIG_IGN(1) are stored verbatim like any other handler VMA; delivery reads
/// the table back in `EcvContext::deliver_pending_signals`. aarch64 kernel
/// `struct sigaction` layout: sa_handler @0, sa_flags @8, sa_mask @16 (the
/// 8-byte sigset for _NSIG=64; aarch64 has no sa_restorer). Returning the
/// previous action first is what a real shell's startup relies on (dash saves
/// and later restores dispositions). See .agents/docs/SHAREDMEM.md ("Signals").
///
/// SIGKILL and SIGSTOP are -EINVAL to INSTALL and legal to QUERY, per
/// [`sigaction_permitted`], which carries the ruling and the reason the two
/// cases differ. Two orderings here are load-bearing and neither is cosmetic:
///
/// * The refusal precedes the `oldact` write. Linux copies `act` in first, but
///   `do_sigaction` returns -EINVAL before `*oact = *k` and the syscall wrapper
///   only copies out on success -- so a refused call leaves the caller's
///   `oldact` buffer UNTOUCHED. Writing it anyway would be a store into guest
///   memory on an error return, which is precisely what a guest passing a
///   stack temporary it never initialises after a failure does not expect.
/// * The refusal is decided from `act` before anything is read from it, so the
///   permitted-query case still runs the `oldact` path below unchanged.
#[inline(never)]
fn sys_rt_sigaction(state: &mut State, ctx: &mut EcvContext) {
    let signum_raw = state.arg(0);
    let act = state.arg(1);
    let oldact = state.arg(2);
    if !sigaction_permitted(signum_raw, act != 0) {
        state.set_ret_err(EINVAL);
        return;
    }
    let signum = signum_raw as usize;
    // Snapshot the previous disposition first (act may alias oldact).
    let prev = ctx.signals.actions[signum];
    if oldact != 0 {
        let buf = ctx.arena.slice_mut(oldact, 24);
        buf[0..8].copy_from_slice(&prev.handler.to_le_bytes());
        buf[8..16].copy_from_slice(&prev.flags.to_le_bytes());
        buf[16..24].copy_from_slice(&prev.mask.to_le_bytes());
    }
    if act != 0 {
        let src = ctx.arena.slice(act, 24);
        let handler = u64::from_le_bytes(src[0..8].try_into().unwrap());
        let flags = u64::from_le_bytes(src[8..16].try_into().unwrap());
        let mask = u64::from_le_bytes(src[16..24].try_into().unwrap());
        ctx.signals.actions[signum] = SigAction {
            handler,
            flags,
            mask,
        };
    }
    state.set_ret(0);
}

/// rt_sigprocmask(how, set, oldset, sigsetsize): records/returns the
/// per-task blocked-signal mask. SIG_BLOCK/UNBLOCK/SETMASK update the mask,
/// `oldset` receives the prior mask. aarch64 sigset is one 8-byte word.
///
/// SIGKILL and SIGSTOP are SILENTLY DROPPED from `set` and the call succeeds --
/// the opposite of `rt_sigaction`'s answer for the same two signals, and the
/// kernel's own asymmetry rather than a shortcut. [`sigprocmask_next`] carries
/// the ruling, and is where the `how` arm and the drop are tested; it is in
/// `context` because `mod sys` is wasm32-only and a test here would never run.
///
/// `prev` is snapshotted from the context, not from guest memory, so `set` and
/// `oldset` may alias: the value reported is the mask as it stood on entry,
/// whatever `set` is then read to be.
///
/// The `oldset` write follows the `how` check, because Linux's wrapper copies
/// out only after `sigprocmask()` returns 0. A bad `how` is -EINVAL with the
/// caller's buffer untouched, not -EINVAL with a mask already stored into it.
#[inline(never)]
fn sys_rt_sigprocmask(state: &mut State, ctx: &mut EcvContext) {
    let how = state.arg(0);
    let set = state.arg(1);
    let oldset = state.arg(2);
    let prev = ctx.task_signals.blocked;
    if set != 0 {
        let newset = u64::from_le_bytes(ctx.arena.slice(set, 8).try_into().unwrap());
        match sigprocmask_next(how, prev, newset) {
            Some(next) => ctx.task_signals.blocked = next,
            None => {
                state.set_ret_err(EINVAL);
                return;
            }
        }
    }
    if oldset != 0 {
        ctx.arena
            .slice_mut(oldset, 8)
            .copy_from_slice(&prev.to_le_bytes());
    }
    state.set_ret(0);
}

/// kill(pid, sig) / tkill / tgkill. Posts a pending signal to the target, which
/// a handler or a signalfd read then consumes. pid <= 0 (process groups,
/// "everyone") is not modeled: we only ever see self-signals and
/// postmaster<->child signaling.
///
/// `to_thread` selects the queue. A process-directed signal may be taken by any
/// member of the group that does not block it; a thread-directed one may only be
/// taken by the named task.
#[inline(never)]
fn sys_kill(state: &mut State, ctx: &mut EcvContext, pid: i64, sig: u32, to_thread: bool) {
    if pid <= 0 {
        state.set_ret_err(ESRCH);
        return;
    }
    let ok = if to_thread {
        ctx.post_signal_to_thread(pid as u32, sig)
    } else {
        ctx.post_signal(pid as u32, sig)
    };
    if ok {
        state.set_ret(0);
    } else {
        state.set_ret_err(ESRCH);
    }
}

/// shmget(key, size, shmflg): create or look up a SysV shared-memory segment.
/// The segment is backed by a shared arena region (registered in
/// `shared_segments`), so every cooperatively-scheduled process and every fork
/// child sees the same physical bytes at the same VMA.
#[inline(never)]
fn sys_shmget(state: &mut State, ctx: &mut EcvContext) {
    let (key, size, shmflg) = (state.arg(0) as i32, state.arg(1) as usize, state.arg(2));
    if key != IPC_PRIVATE {
        if let Some(seg) = ctx.shm.iter().find(|s| s.key == key && !s.removed) {
            if shmflg & IPC_CREAT != 0 && shmflg & IPC_EXCL != 0 {
                state.set_ret_err(EEXIST);
            } else {
                state.set_ret(seg.shmid as u64);
            }
            return;
        }
    }
    if key != IPC_PRIVATE && shmflg & IPC_CREAT == 0 {
        state.set_ret_err(ENOENT);
        return;
    }
    // Reserve a page-rounded region in the SHARED window, not from
    // `arena.mmap_cur`. It used to come from the per-process bump, which is the
    // same defect the mmap paths above were moved off: the bump travels with the
    // arena, so a segment created by one process occupies an address another
    // process is free to hand out privately, and being exempt from the swap the
    // two then alias.
    let map_len = ((size as u64) + GUEST_PAGE_MASK) & !GUEST_PAGE_MASK;
    let Some(at) = ctx.shm_reserve(map_len) else {
        // ENOMEM, not a fatal. shmget failing is an ordinary condition -- it is
        // how a guest discovers the limit -- and PostgreSQL specifically probes
        // downward here, halving its request until one fits.
        state.set_ret_err(ENOMEM);
        return;
    };
    ctx.arena.slice_mut(at, map_len as usize).fill(0);
    ctx.shared_segments.push(SharedSeg {
        vma_start: at,
        len: map_len as usize,
        kind: SharedKind::SysV,
        // No mappers yet: shmget CREATES a segment, it does not attach it. The
        // empty set is not a reclaim trigger for SysV -- see `shm_try_reclaim`,
        // which additionally requires IPC_RMID -- so a segment survives between
        // its creation and the shmat that follows.
        mappers: Vec::new(),
    });
    let shmid = ctx.next_shmid;
    ctx.next_shmid += 1;
    let cpid = ctx.procs[ctx.current].pid;
    ctx.shm.push(ShmSeg {
        key,
        shmid,
        vma: at,
        size,
        cpid,
        removed: false,
    });
    state.set_ret(shmid as u64);
}

/// shmat(shmid, shmaddr, shmflg): attach a segment, returning its VMA. We ignore
/// a requested shmaddr (always attach at the segment's own arena VMA).
#[inline(never)]
fn sys_shmat(state: &mut State, ctx: &mut EcvContext) {
    let shmid = state.arg(0) as i32;
    let Some(vma) = ctx
        .shm
        .iter()
        .find(|s| s.shmid == shmid && !s.removed)
        .map(|s| s.vma)
    else {
        state.set_ret_err(EINVAL);
        return;
    };
    // The attach set is the segment's `mappers`, so that exit and execve
    // (which drop every mapping without a shmdt) and fork (which inherits one)
    // are all accounted for by the same mechanism as the mmap-backed regions.
    if let Some(i) = ctx.shm_seg_at(vma) {
        let pid = ctx.current_pid();
        ctx.shm_add_mapper(i, pid);
    }
    state.set_ret(vma);
}

/// shmdt(shmaddr): detach the segment mapped at shmaddr. A segment already
/// marked IPC_RMID is destroyed here if this was its last attach.
#[inline(never)]
fn sys_shmdt(state: &mut State, ctx: &mut EcvContext) {
    let addr = state.arg(0);
    if !ctx.shm.iter().any(|s| s.vma == addr) {
        state.set_ret_err(EINVAL);
        return;
    }
    if let Some(i) = ctx.shm_seg_at(addr) {
        let pid = ctx.current_pid();
        ctx.shared_segments[i].mappers.retain(|&p| p != pid);
        ctx.shm_try_reclaim(i);
    }
    state.set_ret(0);
}

/// shmctl(shmid, cmd, buf): IPC_STAT fills a `struct shmid_ds` (aarch64 layout,
/// 112 bytes); IPC_RMID marks the segment for destruction (it stays until the
/// last detach, which postgres relies on for its interlock). IPC_SET is a no-op.
#[inline(never)]
fn sys_shmctl(state: &mut State, ctx: &mut EcvContext) {
    let (shmid, cmd, buf) = (state.arg(0) as i32, state.arg(1), state.arg(2));
    let uid = ctx.uid;
    let gid = ctx.gid;
    let Some(si) = ctx.shm.iter().position(|s| s.shmid == shmid && !s.removed) else {
        state.set_ret_err(EINVAL);
        return;
    };
    match cmd {
        IPC_RMID => {
            let vma = ctx.shm[si].vma;
            ctx.shm[si].removed = true;
            // IPC_RMID does not destroy a segment that is still attached; it
            // marks it, and the last detach finishes the job. If nothing is
            // attached right now, this IS the last detach.
            if let Some(i) = ctx.shm_seg_at(vma) {
                ctx.shm_try_reclaim(i);
            }
            state.set_ret(0);
        }
        IPC_STAT => {
            let seg = &ctx.shm[si];
            let (key, size, vma, cpid) = (seg.key, seg.size, seg.vma, seg.cpid);
            // nattch is derived from the attach set, never counted separately.
            // PostgreSQL's postmaster interlock reads exactly this field back to
            // decide whether the process that owns the data directory is still
            // alive, so a count that only ever grew would make every restart
            // report "lock file already exists".
            let nattch = ctx
                .shm_seg_at(vma)
                .map_or(0, |i| ctx.shared_segments[i].mappers.len() as u64);
            let b = ctx.arena.slice_mut(buf, 112);
            b.fill(0);
            // struct ipc_perm (offset 0): __key@0, uid@4, gid@8, cuid@12, cgid@16, mode@20.
            b[0..4].copy_from_slice(&key.to_le_bytes());
            b[4..8].copy_from_slice(&uid.to_le_bytes());
            b[8..12].copy_from_slice(&gid.to_le_bytes());
            b[12..16].copy_from_slice(&uid.to_le_bytes());
            b[16..20].copy_from_slice(&gid.to_le_bytes());
            b[20..24].copy_from_slice(&0o600u32.to_le_bytes());
            // shm_segsz@48 (u64), shm_cpid@80 (i32), shm_lpid@84 (i32), shm_nattch@88 (u64).
            b[48..56].copy_from_slice(&(size as u64).to_le_bytes());
            b[80..84].copy_from_slice(&cpid.to_le_bytes());
            b[84..88].copy_from_slice(&cpid.to_le_bytes());
            b[88..96].copy_from_slice(&nattch.to_le_bytes());
            state.set_ret(0);
        }
        _ => state.set_ret(0), // IPC_SET and others: accept and ignore
    }
}

/// signalfd4(fd, mask, sizemask, flags). fd == -1 creates a new signalfd;
/// otherwise it retargets an existing one's mask. The fd is readable while the
/// process has a pending signal selected by `mask`.
#[inline(never)]
fn sys_signalfd4(state: &mut State, ctx: &mut EcvContext) {
    let (fd, maskp, sizemask, flags) = (
        state.arg(0) as i64,
        state.arg(1),
        state.arg(2) as usize,
        state.arg(3),
    );
    if sizemask < 8 {
        state.set_ret_err(EINVAL);
        return;
    }
    let mask = u64::from_le_bytes(ctx.arena.slice(maskp, 8).try_into().unwrap());
    if fd >= 0 {
        match ctx.fds.get_mut(fd as usize).and_then(|s| s.as_mut()) {
            Some(OpenFile::SignalFd { mask: m }) => {
                *m = mask;
                state.set_ret(fd as u64);
            }
            _ => state.set_ret_err(EINVAL),
        }
        return;
    }
    let new_fd = ctx.alloc_fd(OpenFile::SignalFd { mask });
    // SFD_CLOEXEC shares O_CLOEXEC's bit (0x80000).
    if flags & O_CLOEXEC != 0 {
        ctx.set_cloexec(new_fd as usize, true);
    }
    state.set_ret(new_fd as u64);
}

#[inline(never)]
fn sys_epoll_create1(state: &mut State, ctx: &mut EcvContext) {
    let flags = state.arg(0);
    let fd = ctx.alloc_fd(OpenFile::Epoll {
        interest: Vec::new(),
    });
    if flags & O_CLOEXEC != 0 {
        ctx.set_cloexec(fd as usize, true);
    }
    state.set_ret(fd as u64);
}

/// epoll_ctl(epfd, op, fd, event): maintain the interest list.
#[inline(never)]
fn sys_epoll_ctl(state: &mut State, ctx: &mut EcvContext) {
    let (epfd, op, fd, evp) = (
        state.arg(0) as usize,
        state.arg(1),
        state.arg(2) as i32,
        state.arg(3),
    );
    // Read the guest event struct before borrowing the fd table mutably.
    let (events, data) = if op == EPOLL_CTL_ADD || op == EPOLL_CTL_MOD {
        let b = ctx.arena.slice(evp, EPOLL_EVENT_SIZE as usize);
        (
            u32::from_le_bytes(b[0..4].try_into().unwrap()),
            u64::from_le_bytes(b[8..16].try_into().unwrap()),
        )
    } else {
        (0, 0)
    };
    if fd < 0 || ctx.fds.get(fd as usize).and_then(|s| s.as_ref()).is_none() {
        state.set_ret_err(EBADF);
        return;
    }
    let Some(OpenFile::Epoll { interest }) = ctx.fds.get_mut(epfd).and_then(|s| s.as_mut()) else {
        state.set_ret_err(EINVAL);
        return;
    };
    let pos = interest.iter().position(|i| i.fd == fd);
    match op {
        EPOLL_CTL_ADD => {
            if pos.is_some() {
                state.set_ret_err(EEXIST);
                return;
            }
            interest.push(EpollItem { fd, events, data });
        }
        EPOLL_CTL_MOD => match pos {
            Some(p) => {
                interest[p].events = events;
                interest[p].data = data;
            }
            None => {
                state.set_ret_err(ENOENT);
                return;
            }
        },
        EPOLL_CTL_DEL => match pos {
            Some(p) => {
                interest.remove(p);
            }
            None => {
                state.set_ret_err(ENOENT);
                return;
            }
        },
        _ => {
            state.set_ret_err(EINVAL);
            return;
        }
    }
    state.set_ret(0);
}

/// Readiness of one fd, as an epoll event mask. Regular files and stdio are
/// always ready (POSIX); a signalfd is readable while a selected signal is
/// pending; pipe ends follow their buffer/peer state. Sockets are host-driven
/// and reported ready so the guest's own read/write does the blocking.
fn fd_ready(ctx: &mut EcvContext, fd: i32) -> u32 {
    match ctx.fds.get(fd as usize).and_then(|s| s.as_ref()) {
        // Neither device ever blocks in either direction.
        Some(OpenFile::Null { .. }) => EPOLLIN | EPOLLOUT,
        Some(OpenFile::SignalFd { mask }) => {
            if (ctx.signals.pending | ctx.task_signals.pending) & *mask != 0 {
                EPOLLIN
            } else {
                0
            }
        }
        Some(OpenFile::Pipe { idx, write }) => {
            let p = &ctx.pipes[*idx];
            if *write {
                if p.readers > 0 {
                    EPOLLOUT
                } else {
                    EPOLLHUP
                }
            } else if !p.buf.is_empty() {
                EPOLLIN
            } else if p.writers == 0 {
                EPOLLIN | EPOLLHUP // EOF is readable
            } else {
                0
            }
        }
        Some(OpenFile::Mem { .. }) | Some(OpenFile::Dir { .. }) | Some(OpenFile::Stdio(_)) => {
            EPOLLIN | EPOLLOUT
        }
        Some(OpenFile::Socket { h }) => {
            // Probe TRUE readability (a listen socket is not perpetually readable;
            // reporting it so makes the guest accept() into an EAGAIN block). Keep
            // writability optimistic -- connected sockets are almost always writable
            // and the epoll path rarely waits on EPOLLOUT.
            let h = *h;
            let mut r = EPOLLOUT;
            if ctx.net.ready(h, false).read {
                r |= EPOLLIN;
            }
            r
        }
        // A socketpair end polls exactly like the pipe it reads from, plus
        // writability whenever the peer still has its end open.
        Some(OpenFile::SocketPair { rx, tx }) => {
            let (r, w) = (&ctx.pipes[*rx], &ctx.pipes[*tx]);
            let mut ev = 0;
            if !r.buf.is_empty() {
                ev |= EPOLLIN;
            } else if r.writers == 0 {
                ev |= EPOLLIN | EPOLLHUP; // peer closed: EOF is readable
            }
            if w.readers > 0 {
                ev |= EPOLLOUT;
            } else {
                ev |= EPOLLHUP;
            }
            ev
        }
        Some(OpenFile::EventFd { count, .. }) => {
            // Always writable: the counter is u64 and saturates rather than
            // blocking at the kernel's 2^64-2 ceiling.
            if *count > 0 {
                EPOLLIN | EPOLLOUT
            } else {
                EPOLLOUT
            }
        }
        // A named AF_UNIX listener is readable exactly when a connection is
        // waiting -- the same rule as a host listen socket, and the reason
        // `fd_ready` probes rather than reporting a listener perpetually ready.
        // An unbound one is neither readable nor writable: it is not connected.
        Some(OpenFile::UnixSocket { listener }) => match listener {
            Some(l) if !ctx.unix_listeners[*l].pending.is_empty() => EPOLLIN,
            _ => 0,
        },
        Some(OpenFile::Epoll { .. }) => 0,
        None => 0,
    }
}

/// ppoll(fds, nfds, timeout_ts, sigmask, sigsetsize).
///
/// The clocks a sleep may name. REALTIME reads the wall clock and the other two
/// read the monotonic one, so the guest CAN now observe a difference between
/// them: a wall-clock step moves an absolute REALTIME instant and leaves a
/// MONOTONIC one alone. Whichever it names, the deadline is converted to the
/// scheduler's monotonic timebase by `to_mono` before it is armed -- see there
/// for what that conversion deliberately does not track.
///
/// CLOCK_THREAD_CPUTIME_ID (3) is deliberately NOT here. Linux rejects it in
/// `clock_nanosleep` too, and a CPU-time sleep served from the wall clock would
/// be a wrong answer rather than a missing one -- the one outcome worse than
/// EINVAL.
const CLOCK_REALTIME: u64 = 0;
const CLOCK_MONOTONIC: u64 = 1;
const CLOCK_BOOTTIME: u64 = 7;
/// `clock_nanosleep` flag: `rqtp` is an ABSOLUTE time on `clockid`, not an
/// interval. The distinction is not cosmetic -- an absolute request re-read on
/// resume is still the same instant, while a relative one re-read on resume
/// starts the whole interval again.
const TIMER_ABSTIME: u64 = 1;

/// nanosleep(rqtp, rmtp) -- defined by Linux as clock_nanosleep on
/// CLOCK_MONOTONIC with a relative interval.
#[inline(never)]
fn sys_nanosleep(state: &mut State, ctx: &mut EcvContext) {
    let (rqtp, rmtp) = (state.arg(0), state.arg(1));
    do_sleep(state, ctx, CLOCK_MONOTONIC, 0, rqtp, rmtp);
}

/// clock_nanosleep(clockid, flags, rqtp, rmtp).
#[inline(never)]
fn sys_clock_nanosleep(state: &mut State, ctx: &mut EcvContext) {
    let (clockid, flags, rqtp, rmtp) = (state.arg(0), state.arg(1), state.arg(2), state.arg(3));
    do_sleep(state, ctx, clockid, flags, rqtp, rmtp);
}

/// Both sleeps. Parks on `BlockedOn::Sleep` with an absolute deadline and lets
/// the scheduler's deadline sweep be the wake source.
///
/// WHY THIS EXISTS AT ALL, since it looks like something that must have worked:
/// neither syscall was in `match nr`, so both fell to the ENOSYS default and a
/// guest that slept did not sleep -- it spun. Measured 2026-08-14 by
/// `e2e/threadgaps_test.go`: a 100 ms `nanosleep` returned -1/ENOSYS after 0 ms.
/// The condvar and semaphore paths were unaffected and always worked, because
/// glibc routes THOSE through FUTEX_WAIT_BITSET (see `sys_futex`), which is
/// exactly why this gap survived: every timed wait a server actually performs
/// went through the futex path, and only a plain sleep went through this one.
///
/// Three things the shape below is load-bearing for:
///
///   - the deadline is armed ONCE, on first entry, and a resume takes it from
///     `current_deadline` rather than re-reading `rqtp`. Re-reading would
///     restart a RELATIVE sleep on every wake, so a signal with no handler
///     could extend a sleep indefinitely.
///   - a pending handler is delivered BEFORE parking, and again on every wake.
///     That is what makes an interrupted sleep return EINTR with the remaining
///     time instead of sleeping through the handler.
///   - an already-expired deadline returns 0 without parking. `nanosleep(0)` is
///     a yield, not a block, and parking it would need something to wake it.
fn do_sleep(
    state: &mut State,
    ctx: &mut EcvContext,
    clockid: u64,
    flags: u64,
    rqtp: u64,
    rmtp: u64,
) {
    if flags & !TIMER_ABSTIME != 0 {
        state.set_ret_err(EINVAL);
        return;
    }
    if !matches!(clockid, CLOCK_REALTIME | CLOCK_MONOTONIC | CLOCK_BOOTTIME) {
        ecv_debug!(ecvisor, "clock_nanosleep: unsupported clockid {clockid}");
        state.set_ret_err(EINVAL);
        return;
    }
    let abstime = flags & TIMER_ABSTIME != 0;
    // `now` is the SCHEDULER's timebase -- what the armed deadline is compared
    // against, and what everything below reports remaining time in. The guest's
    // own clock is read separately and only to interpret its request.
    let now = crate::context::mono_nanos();

    // A resumed call keeps the deadline it armed on first entry; only a fresh
    // one reads the guest's request. `current_deadline` is None on a fresh call
    // because `svc_dispatch` clears it, so this cannot pick up a stale one from
    // the previous syscall.
    let deadline = match (ctx.resuming, ctx.current_deadline()) {
        (true, Some(d)) => d,
        _ => {
            if rqtp == 0 || !ctx.arena.in_bounds(rqtp, 16) {
                state.set_ret_err(EINVAL);
                return;
            }
            let b = ctx.arena.slice(rqtp, 16);
            let secs = u64::from_le_bytes(b[0..8].try_into().unwrap());
            let nsecs = u64::from_le_bytes(b[8..16].try_into().unwrap());
            // The allowlist above is exactly the ids `clock_base` maps, so the
            // fallback is unreachable; it is there so a widened allowlist cannot
            // silently start measuring a new clock against the wrong timebase.
            let clock_now = crate::context::clock_read(clockid).unwrap_or(now);
            match crate::context::sleep_deadline(clock_now, abstime, secs, nsecs)
                .map(|d| crate::context::to_mono(d, clock_now, now))
            {
                Some(d) => d,
                // The only way a request is refused: a malformed timespec.
                None => {
                    state.set_ret_err(EINVAL);
                    return;
                }
            }
        }
    };

    // Consume the flag whether or not it is used, or a later timed wait in this
    // process inherits a timeout that already fired.
    let timed_out = ctx.resuming && ctx.take_timed_out();
    // `deliver_pending_signals` takes the set to LEAVE pending, which is the
    // process's blocked mask.
    let mask = ctx.task_signals.blocked;
    if timed_out || now >= deadline {
        // The full interval elapsed, so the sleep SUCCEEDED -- but a signal that
        // arrived while it ran still has to reach its handler. Linux runs it on
        // the way back to userspace; here the syscall boundary is the only such
        // place, and returning 0 without this left a handler unrun for a signal
        // the guest had already been told nothing about. Measured: the guest saw
        // its own `nanosleep` complete and `usr1.count` still 0.
        //
        // The return value stays 0: the wait was not cut short, and a guest that
        // takes EINTR here would re-sleep an interval it has already served.
        // Linux writes nothing to `rmtp` on success either.
        unsafe { ctx.deliver_pending_signals(mask) };
        state.set_ret(0);
        return;
    }

    if ctx.resuming {
        ecv_debug!(
            sleep,
            "resumed: pending={:#x}/{:#x} (task/proc) blocked={:#x} left={} ms",
            ctx.task_signals.pending,
            ctx.signals.pending,
            mask,
            (deadline.saturating_sub(now)) / 1_000_000
        );
    }
    // Anything deliverable now ends the sleep early.
    if unsafe { ctx.deliver_pending_signals(mask) } > 0 {
        // The remaining time is only meaningful for a relative request; for
        // TIMER_ABSTIME the guest already holds the absolute deadline and Linux
        // leaves `rmtp` alone.
        if rmtp != 0 && !abstime && ctx.arena.in_bounds(rmtp, 16) {
            let (secs, nsecs) =
                crate::context::remaining_timespec(deadline, crate::context::mono_nanos());
            let out = ctx.arena.slice_mut(rmtp, 16);
            out[0..8].copy_from_slice(&secs.to_le_bytes());
            out[8..16].copy_from_slice(&nsecs.to_le_bytes());
        }
        state.set_ret_err(EINTR);
        return;
    }

    ecv_trace!(
        ecv,
        "sleep until {} (in {} ms)",
        deadline,
        (deadline - now) / 1_000_000
    );
    ctx.set_current_deadline(Some(deadline));
    ctx.block_current(BlockedOn::Sleep);
    ctx.suspended = true; // svc unwinds; re-entered on wake or at the deadline
}

/// aarch64 has no plain `poll` syscall, so every `poll()` in the guest arrives
/// here -- including libpq's, which is not optional: `pqSocketCheck` polls the
/// connection on every connect and every query, so psql could not exchange a
/// single byte with a postmaster it had successfully reached.
///
/// Readiness comes from `fd_ready`, the same oracle `epoll_pwait` uses, and the
/// blocking shape is deliberately the same as that function's: park as a SOCKET
/// waiter when a host socket is in the set (only the scheduler's idle host-poll
/// can make one ready), otherwise on `Poll`, with one ABSOLUTE deadline that
/// survives spurious wakes. Reimplementing either half here would give two
/// answers to "is this fd ready" that could drift apart.
#[inline(never)]
fn sys_ppoll(state: &mut State, ctx: &mut EcvContext) {
    let (fds_ptr, nfds, tmo_ptr, sigmask_ptr) = (
        state.arg(0),
        state.arg(1) as usize,
        state.arg(2),
        state.arg(3),
    );
    // A resumed call may have been released by its own deadline. Consume the
    // flag before re-scanning: if nothing is ready now, the guest's timeout
    // expired and it must get 0, not another park.
    let expired = ctx.resuming && ctx.take_timed_out();
    let wait_mask = if sigmask_ptr != 0 {
        u64::from_le_bytes(ctx.arena.slice(sigmask_ptr, 8).try_into().unwrap())
    } else {
        ctx.task_signals.blocked
    };
    unsafe { ctx.deliver_pending_signals(wait_mask) };

    // NULL timeout means wait forever; a zero timespec means poll and return.
    let timeout_ns: Option<u128> = if tmo_ptr == 0 {
        None
    } else {
        let b = ctx.arena.slice(tmo_ptr, 16);
        let sec = u64::from_le_bytes(b[0..8].try_into().unwrap()) as u128;
        let nsec = u64::from_le_bytes(b[8..16].try_into().unwrap()) as u128;
        Some(sec * 1_000_000_000 + nsec)
    };

    let mut nready = 0usize;
    let mut ready_socks: Vec<NetHandle> = Vec::new();
    for i in 0..nfds {
        let base = fds_ptr + i as u64 * POLLFD_SIZE;
        let entry = ctx.arena.slice(base, POLLFD_SIZE as usize);
        let fd = i32::from_le_bytes(entry[0..4].try_into().unwrap());
        let events = u16::from_le_bytes(entry[4..6].try_into().unwrap()) as u32;
        // A negative fd is ignored and reports nothing -- that is how callers
        // disable a slot without shrinking the array.
        let revents = if fd < 0 {
            0
        } else if ctx.fds.get(fd as usize).and_then(|s| s.as_ref()).is_none() {
            POLLNVAL
        } else {
            // HUP and ERR are reported whether or not they were requested.
            fd_ready(ctx, fd) & (events | EPOLLHUP | POLLERR)
        };
        if revents != 0 {
            nready += 1;
            if let Some(OpenFile::Socket { h }) = ctx.fds.get(fd as usize).and_then(|s| s.as_ref())
            {
                ready_socks.push(*h);
            }
        }
        let out = ctx.arena.slice_mut(base + 6, 2);
        out.copy_from_slice(&(revents as u16).to_le_bytes());
    }
    // Readiness one process discovers is readiness for every process parked on
    // the same host descriptor -- see the same call in `sys_epoll_pwait`.
    for h in ready_socks {
        ctx.wake_socket_waiters_on(h);
    }

    if nready > 0 || timeout_ns == Some(0) || expired {
        state.set_ret(nready as u64);
        return;
    }

    // Arm the deadline once per WAIT, not once per park: a resumed call keeps
    // the deadline it already has, or the guest's timeout expires in a multiple
    // of what it asked for, one multiple per spurious wake.
    if !ctx.resuming || ctx.current_deadline().is_none() {
        ctx.set_current_deadline(timeout_ns.map(|ns| crate::context::mono_nanos() + ns));
    }

    let sock_fd = (0..nfds).find_map(|i| {
        let base = fds_ptr + i as u64 * POLLFD_SIZE;
        let fd = i32::from_le_bytes(ctx.arena.slice(base, 4).try_into().unwrap());
        if fd < 0 {
            return None;
        }
        match ctx.fds.get(fd as usize).and_then(|s| s.as_ref()) {
            Some(OpenFile::Socket { h }) => Some(*h),
            _ => None,
        }
    });
    if let Some(h) = sock_fd {
        // A SET wait: the process cares about every socket in the poll list,
        // but only one of them is recorded here. See `BlockedOn::Socket`.
        ctx.block_current(BlockedOn::Socket {
            h,
            write: false,
            poll: true,
        });
        ctx.suspended = true;
        return;
    }
    let others_runnable = ctx
        .procs
        .iter()
        .enumerate()
        .any(|(i, p)| i != ctx.current && p.status == crate::context::ProcStatus::Runnable);
    // Same terminal case as epoll_pwait: park whenever something could still
    // release us -- another runnable process, a socket the idle poll services,
    // or our own finite deadline. Only an infinite wait with no possible waker
    // returns 0, which is a lie told to avoid wedging the module.
    if others_runnable || ctx.has_socket_waiters() || timeout_ns.is_some() {
        ctx.block_current(BlockedOn::Poll);
        ctx.suspended = true;
        return;
    }
    state.set_ret(0);
}

/// epoll_pwait(epfd, events, maxevents, timeout, sigmask). Blocks via
/// `BlockedOn::Poll` when nothing is ready; woken by `wake_pollers` (a posted
/// signal or a pipe write/close) and re-evaluated on resume.
#[inline(never)]
fn sys_epoll_pwait(state: &mut State, ctx: &mut EcvContext) {
    let (epfd, evp, maxevents, timeout) = (
        state.arg(0) as usize,
        state.arg(1),
        state.arg(2) as i64,
        state.arg(3) as i64,
    );
    if maxevents <= 0 {
        state.set_ret_err(EINVAL);
        return;
    }
    // Voluntary preemption point. Nothing else in this scheduler preempts: a woken
    // process is only marked Runnable and queued, and the running process keeps
    // going until it blocks. An nginx worker with a non-empty accept backlog never
    // blocks, so the workers it just woke sit in the run queue unserved -- which is
    // why publishing readiness (wake_socket_waiters_on) let a SECOND worker in but
    // never a third or fourth.
    //
    // `epoll_pwait` is the natural place to yield: it is the top of an event loop,
    // so re-executing it is free of side effects, and every server that matters
    // passes through it constantly.
    //
    // `ctx.resuming` is the anti-livelock guard, and it needs no new state: it is
    // true exactly when the scheduler has rewound us back into this syscall. So a
    // FRESH call yields once and a RESUMED one proceeds. Two workers therefore
    // alternate (A yields to B, B yields to A, A is now resuming and serves)
    // instead of deferring to each other forever, which is what an unguarded yield
    // would do.
    if !ctx.resuming && !ctx.run_queue.is_empty() {
        ctx.pending = Pending::Yield;
        ctx.suspended = true;
        return;
    }
    // A resumed call may have been released by its own deadline rather than by
    // readiness. Consume the flag here: if the re-scan below finds nothing, the
    // guest's timeout expired and it must get 0 events -- not another park.
    // Nothing else on this path consumed it before, so an expiry recorded
    // against a process sitting in epoll leaked into its next `sys_futex`
    // resume as a bogus ETIMEDOUT.
    let expired = ctx.resuming && ctx.take_timed_out();
    // epoll_pwait is a signal-delivery point. Its 5th arg is the sigmask to apply
    // for the wait (NULL = leave the process mask unchanged); run any pending,
    // unblocked handler NOW, before scanning readiness. This is how the
    // postmaster's SIGCHLD handler (blocked in its main loop, unblocked via this
    // arg for the wait) runs on resume -> SetLatch posts SIGURG -> the latch
    // signalfd below is then readable and this very call returns the latch event.
    // A handler can raise a signal or (in principle) close an fd, so do this
    // before snapshotting the interest list.
    let sigmask_ptr = state.arg(4);
    let wait_mask = if sigmask_ptr != 0 {
        u64::from_le_bytes(ctx.arena.slice(sigmask_ptr, 8).try_into().unwrap())
    } else {
        ctx.task_signals.blocked
    };
    unsafe { ctx.deliver_pending_signals(wait_mask) };
    let Some(OpenFile::Epoll { interest }) = ctx.fds.get(epfd).and_then(|s| s.as_ref()) else {
        state.set_ret_err(EINVAL);
        return;
    };
    // Snapshot the interest list so the readiness scan can borrow ctx immutably.
    let items: Vec<EpollItem> = interest.clone();

    let mut ready: Vec<(u32, u64)> = Vec::new();
    let mut ready_socks: Vec<NetHandle> = Vec::new();
    for it in &items {
        // EPOLLHUP/EPOLLERR are reported regardless of the requested mask.
        let got = fd_ready(ctx, it.fd) & (it.events | EPOLLHUP);
        if got != 0 {
            if let Some(OpenFile::Socket { h }) =
                ctx.fds.get(it.fd as usize).and_then(|s| s.as_ref())
            {
                ready_socks.push(*h);
            }
            ready.push((got, it.data));
            if ready.len() as i64 >= maxevents {
                break;
            }
        }
    }
    // Readiness this process just discovered is readiness for every process parked
    // on the same host descriptor. Nothing else will tell them: the scheduler's
    // `poll_sockets_and_wake` runs only when nothing is runnable, so a worker that
    // always finds work keeps the others asleep indefinitely. That is what pinned
    // nginx's accept loop to one worker of four. See wake_socket_waiters_on.
    for h in ready_socks {
        ctx.wake_socket_waiters_on(h);
    }

    if ready.is_empty() && timeout != 0 {
        // The deadline we were parked on has passed and nothing came ready: that
        // is exactly what `epoll_wait` returning 0 means.
        if expired {
            state.set_ret(0);
            return;
        }
        // Honour the timeout. It is a count of MILLISECONDS, negative meaning
        // "wait forever"; before this it was read only as a boolean and then
        // dropped, so every finite wait became an infinite one and a guest timer
        // could not fire at all. nginx drives its entire timer wheel off this
        // argument -- keepalive expiry, client_header_timeout, send_timeout, the
        // resolver -- and a traced run shows it asking for 5000 ms
        // (`keepalive_timeout 5`) and 60000 ms on both libcs, against 26 infinite
        // waits. A worker parked here could therefore only be released by an
        // unrelated event, which is the shape of the ~7s multi-worker stall.
        //
        // The deadline is absolute and belongs to the whole wait, not to one
        // park. A RESUMED call keeps the deadline it already has: the wake it
        // just took is often spurious (a socket went ready for another process,
        // and `wake_socket_waiters_on` deliberately over-wakes), and re-arming
        // the full timeout on each of those makes the guest's timer expire in a
        // MULTIPLE of what it asked for -- one per spurious wake. Measured on
        // nginx with four workers before this guard: an idle keepalive
        // connection closed at either 4.59s or 19.78s and nothing between, i.e.
        // one re-arm or four of them.
        //
        // A fresh call always (re)arms, so a stale deadline left by an earlier
        // blocking episode cannot leak in, and an infinite wait clears it.
        if !ctx.resuming || ctx.current_deadline().is_none() {
            ctx.set_current_deadline(if timeout > 0 {
                Some(crate::context::mono_nanos() + timeout as u128 * 1_000_000)
            } else {
                None
            });
        }
        // Nothing ready. If the interest set contains a socket, block as a SOCKET
        // waiter on it: the scheduler's idle path host-polls the fd (a socket only
        // becomes ready from the host, so a plain Poll block would either busy-loop
        // or, as the last runnable process, wrongly exit the module), and a posted
        // signal still wakes us via post_signal (so SIGCHLD from a reaped child
        // reaches the postmaster parked in ServerLoop's epoll). On wake we re-scan:
        // a ready signalfd/pipe/socket is then returned. This is what lets the
        // postmaster advance past recovery to "ready to accept connections".
        let sock_fd = items.iter().find_map(|it| {
            match ctx.fds.get(it.fd as usize).and_then(|s| s.as_ref()) {
                Some(OpenFile::Socket { h }) => Some(*h),
                _ => None,
            }
        });
        if let Some(h) = sock_fd {
            if trace_log() {
                let nsock = items
                    .iter()
                    .filter(|it| {
                        matches!(
                            ctx.fds.get(it.fd as usize).and_then(|s| s.as_ref()),
                            Some(OpenFile::Socket { .. })
                        )
                    })
                    .count();
                ecv_trace!(
                    sched,
                    "epoll block on handle={:?} of {} socket(s) in the set",
                    h,
                    nsock
                );
            }
            // A SET wait, as above: the epoll interest list is wider than `h`.
            ctx.block_current(BlockedOn::Socket {
                h,
                write: false,
                poll: true,
            });
            ctx.suspended = true;
            return;
        }
        // No socket in the set: block on Poll if anything else can still make
        // progress -- either some OTHER guest process is Runnable, OR a process is
        // parked on a socket (whose readiness the scheduler's idle host-poll
        // services). Blocking (not spin-returning) is essential in steady state:
        // PostgreSQL's checkpointer/bgwriter/walwriter wait on their latch with a
        // TIMEOUT, and as the LAST runnable process a spin-return of 0 turned their
        // WaitLatch loop into a 100%-CPU busy loop that starved the postmaster's
        // idle listen-socket poll -- so a client connect never got accepted. They
        // park on Poll instead (woken by any latch signal via wake_pollers); the
        // idle loop then reaches poll_sockets_and_wake and accepts the connection.
        // Only when NOTHING else can progress (no runnable, no socket waiter) do we
        // report an immediate timeout expiry, which avoids deadlocking the module.
        let others_runnable = ctx
            .procs
            .iter()
            .enumerate()
            .any(|(i, p)| i != ctx.current && p.status == crate::context::ProcStatus::Runnable);
        // A finite timeout is itself a wake source now that the deadline is
        // armed: the idle sweep releases us even with nothing else alive, so
        // parking is safe and is what the guest asked for. Only an INFINITE wait
        // with nothing that could ever wake it still spin-returns 0, which is a
        // lie but keeps the module from wedging.
        if others_runnable || ctx.has_socket_waiters() || timeout > 0 {
            ctx.block_current(BlockedOn::Poll);
            ctx.suspended = true; // svc unwinds; we are re-entered on wake
            return;
        }
        state.set_ret(0);
        return;
    }
    if ready.is_empty() {
        state.set_ret(0);
        return;
    }

    for (i, (events, data)) in ready.iter().enumerate() {
        let base = evp + i as u64 * EPOLL_EVENT_SIZE;
        let b = ctx.arena.slice_mut(base, EPOLL_EVENT_SIZE as usize);
        b[0..4].copy_from_slice(&events.to_le_bytes());
        b[4..8].fill(0); // explicit padding
        b[8..16].copy_from_slice(&data.to_le_bytes());
    }
    state.set_ret(ready.len() as u64);
}

#[inline(never)]
fn sys_futex(state: &mut State, ctx: &mut EcvContext) {
    if ctx.resuming {
        // A signal that arrived while we were parked runs its handler HERE,
        // before the guest gets its answer. This is the delivery boundary for
        // every glibc timed wait -- `pthread_cond_timedwait`, `sem_timedwait`
        // and the condvars all park in FUTEX_WAIT_BITSET -- and without it a
        // thread waiting on a condvar never runs a handler at all.
        //
        // The return value is deliberately NOT EINTR: POSIX says a condvar wait
        // does not report interruption, and a runtime that ended the wait early
        // here would break every caller that treats a return as "the predicate
        // may have changed". The guest sees an ordinary wake, re-checks its
        // predicate, and re-waits against the same absolute deadline.
        let mask = ctx.task_signals.blocked;
        unsafe { ctx.deliver_pending_signals(mask) };
        // Either a FUTEX_WAKE woke us or the deadline passed (svc did
        // stop_rewind). The guest re-checks its own condition word and may
        // FUTEX_WAIT again; it needs to know which happened, because a timed
        // wait that reports success looks to glibc like a spurious wake and it
        // will simply wait again -- forever.
        if ctx.take_timed_out() {
            state.set_ret_err(ETIMEDOUT);
        } else {
            state.set_ret(0);
        }
        return;
    }
    let cmd = state.arg(1) & FUTEX_CMD_MASK;
    match cmd {
        FUTEX_WAIT | FUTEX_WAIT_BITSET => {
            let uaddr = state.arg(0);
            let val = state.arg(2) as u32;
            let cur = u32::from_le_bytes(ctx.arena.slice(uaddr, 4).try_into().unwrap());
            if cur != val {
                // Race-avoidance fast path: the word already moved; don't sleep.
                state.set_ret_err(EAGAIN);
                return;
            }
            // Timeout. FUTEX_WAIT takes a RELATIVE timespec; FUTEX_WAIT_BITSET
            // takes an ABSOLUTE one. A null pointer means wait forever. glibc
            // routes clock_nanosleep, sem_timedwait and pthread_cond_timedwait
            // through the BITSET form, so ignoring this argument hangs the guest
            // on its first sleep.
            //
            // ⚠️ WHICH CLOCK THE ABSOLUTE FORM NAMES IS IN THE OP WORD, not in
            // the command: FUTEX_CLOCK_REALTIME (256) selects CLOCK_REALTIME and
            // its absence selects CLOCK_MONOTONIC. `FUTEX_CMD_MASK` strips that
            // bit to get `cmd`, so it has to be read from the raw argument --
            // and it used to be read from nowhere at all, which was harmless
            // only for as long as both clocks were the same clock.
            let deadline = match state.arg(3) {
                0 => None,
                tsp => {
                    let ts = ctx.arena.slice(tsp, 16);
                    let secs = u64::from_le_bytes(ts[..8].try_into().unwrap()) as u128;
                    let nsecs = u64::from_le_bytes(ts[8..].try_into().unwrap()) as u128;
                    let t = secs * 1_000_000_000 + nsecs;
                    let mono = crate::context::mono_nanos();
                    Some(if cmd == FUTEX_WAIT_BITSET {
                        // Absolute, on whichever clock the op selected.
                        let clock_now = if state.arg(1) & FUTEX_CLOCK_REALTIME != 0 {
                            crate::context::now_nanos()
                        } else {
                            mono
                        };
                        crate::context::to_mono(t, clock_now, mono)
                    } else {
                        mono + t // relative: the clock bit does not apply
                    })
                }
            };
            ctx.set_current_deadline(deadline);
            // *uaddr == val: park until a FUTEX_WAKE on this address wakes us.
            if trace_log() {
                // The guest call stack, innermost first. Without it a park is
                // just an address, and identifying the waiter means guessing
                // from a call graph -- which mis-attributes, because glibc's
                // low-level wait helpers are stripped locals shared by
                // clock_nanosleep, sem_wait and the condvars alike.
                let frames: Vec<String> = ctx
                    .call_history
                    .iter()
                    .rev()
                    .take(16)
                    .map(|(f, r)| format!("fn=0x{f:x}@ret=0x{r:x}"))
                    .collect();
                // One event per line. These were single `eprintln!`s carrying an
                // embedded "\n[futex]   ", i.e. a hand-written prefix on the
                // continuation line that no filter could see and that carried no
                // pid. Split, every line is filterable and attributed.
                ecv_trace!(
                    futex,
                    "WAIT uaddr={:#x} val={:#x} timeout={:#x} (parking)",
                    uaddr,
                    val,
                    state.arg(3)
                );
                ecv_trace!(futex, "  stack: {}", frames.join(" "));
                // The bytes around the futex word. A futex is almost always a
                // field inside a larger object -- a condvar inside a lock -- and
                // the interesting question when a wait never ends is what the
                // guest believes about the REST of that object. Printing the
                // surrounding window costs nothing and turns "waiting on
                // 0x10009120" into the actual counters the guest is polling.
                // Registers and the top of the guest stack. The blocked
                // function's own arguments are in the GPRs, and its caller's
                // callee-saved registers are in the frame at SP -- which is how
                // you get from "waiting on an address" to "which object was
                // passed in, and by whom".
                let gprs: Vec<String> = (0..31)
                    .map(|i| format!("x{i}={:#x}", state.gpr.x[i].val))
                    .collect();
                ecv_trace!(
                    futex,
                    "  sp={:#x} lr={:#x}",
                    state.gpr.sp.val,
                    state.gpr.x[30].val
                );
                ecv_trace!(futex, "  {}", gprs.join(" "));
                let sp = state.gpr.sp.val;
                if sp >= crate::arena::MEMORY_ARENA_VMA
                    && sp + 128 < crate::arena::MEMORY_ARENA_SIZE as u64
                {
                    ecv_trace!(futex, "  stack at sp:");
                    for r in 0..8u64 {
                        let a = sp + r * 16;
                        let w = ctx.arena.slice(a, 16);
                        let a0 = u64::from_le_bytes(w[..8].try_into().unwrap());
                        let a1 = u64::from_le_bytes(w[8..].try_into().unwrap());
                        ecv_trace!(futex, "    {a:#010x}  {a0:#018x} {a1:#018x}");
                    }
                }
                let lo = uaddr.saturating_sub(0x100) & !0xf;
                let hi = (uaddr + 0x40).min(crate::arena::MEMORY_ARENA_SIZE as u64);
                ecv_trace!(futex, "  memory around uaddr [{lo:#x},{hi:#x}):");
                let mut a = lo;
                while a < hi {
                    let row = ctx.arena.slice(a, 16.min((hi - a) as usize));
                    let hex: Vec<String> = row.iter().map(|b| format!("{b:02x}")).collect();
                    let words: Vec<String> = row
                        .chunks(4)
                        .map(|c| {
                            let mut w = [0u8; 4];
                            w[..c.len()].copy_from_slice(c);
                            format!("{:>10}", u32::from_le_bytes(w))
                        })
                        .collect();
                    ecv_trace!(
                        futex,
                        "    {a:#010x}  {}  u32:{}{}",
                        hex.join(" "),
                        words.join(" "),
                        if a <= uaddr && uaddr < a + 16 {
                            "   <== uaddr"
                        } else {
                            ""
                        }
                    );
                    a += 16;
                }
            }
            ctx.block_current(BlockedOn::Futex { uaddr });
            ctx.suspended = true; // svc unwinds
        }
        FUTEX_WAKE | FUTEX_WAKE_BITSET => {
            let uaddr = state.arg(0);
            let n = state.arg(2) as u32;
            let woken = ctx.wake_futex(uaddr, n);
            state.set_ret(woken as u64);
        }
        _ => {
            ecv_debug!(
                ecvisor,
                "ENOSYS futex cmd 0x{cmd:x} (op 0x{:x})",
                state.arg(1)
            );
            state.set_ret_err(ENOSYS);
        }
    }
}

/// Appends a pipe to the context-global table and returns its index.
fn new_pipe(ctx: &mut EcvContext, readers: u32, writers: u32) -> usize {
    let idx = ctx.pipes.len();
    ctx.pipes.push(Pipe {
        buf: std::collections::VecDeque::new(),
        readers,
        writers,
        scm: std::collections::VecDeque::new(),
    });
    idx
}

/// socketpair(domain, type, protocol, sv): a connected pair of AF_UNIX stream
/// endpoints, built from two pipes so each direction has its own buffer.
///
/// Only AF_UNIX/AF_LOCAL is accepted. AF_INET socketpairs do not exist on Linux
/// either, so refusing anything else is fidelity, not a limitation -- and it
/// keeps this away from the host-socket path, which these fds must never touch:
/// a socketpair is entirely internal to the guest and the host has no matching
/// object.
#[inline(never)]
fn sys_socketpair(state: &mut State, ctx: &mut EcvContext) {
    let (domain, ty, sv) = (state.arg(0) as u32, state.arg(1) as u32, state.arg(3));
    if domain != AF_UNIX {
        state.set_ret_err(EAFNOSUPPORT);
        return;
    }
    if sv == 0 {
        state.set_ret_err(EINVAL);
        return;
    }
    // a: fd0 -> fd1. b: fd1 -> fd0. Each end holds one reader and one writer, so
    // closing one end drops both counts on the direction the peer reads and the
    // peer sees EOF -- the same close semantics a real socketpair has.
    let a = new_pipe(ctx, 1, 1);
    let b = new_pipe(ctx, 1, 1);
    let fd0 = ctx.alloc_fd(OpenFile::SocketPair { rx: b, tx: a });
    let fd1 = ctx.alloc_fd(OpenFile::SocketPair { rx: a, tx: b });
    let cloexec = ty as u64 & SOCK_CLOEXEC != 0;
    ctx.set_cloexec(fd0 as usize, cloexec);
    ctx.set_cloexec(fd1 as usize, cloexec);
    let dst = ctx.arena.slice_mut(sv, 8);
    dst[..4].copy_from_slice(&(fd0 as u32).to_le_bytes());
    dst[4..].copy_from_slice(&(fd1 as u32).to_le_bytes());
    state.set_ret(0);
}

/// eventfd2(initval, flags): a counting notification fd. See `OpenFile::EventFd`
/// for why the counter is per-fd rather than shared.
#[inline(never)]
fn sys_eventfd2(state: &mut State, ctx: &mut EcvContext) {
    let (initval, flags) = (state.arg(0), state.arg(1) as u32);
    let fd = ctx.alloc_fd(OpenFile::EventFd {
        count: initval,
        semaphore: flags & EFD_SEMAPHORE != 0,
    });
    ctx.set_cloexec(fd as usize, flags & EFD_CLOEXEC != 0);
    state.set_ret(fd as u64);
}

/// Reads a guest `struct iovec[]` into (base, len) pairs.
fn read_iovecs(ctx: &EcvContext, vma: u64, count: usize) -> Vec<(u64, usize)> {
    (0..count)
        .map(|i| {
            let iov = ctx.arena.slice(vma + (i * 16) as u64, 16);
            (
                u64::from_le_bytes(iov[..8].try_into().unwrap()),
                u64::from_le_bytes(iov[8..].try_into().unwrap()) as usize,
            )
        })
        .collect()
}

/// sendmsg(fd, msghdr, flags). Only the socketpair path is implemented, because
/// that is the only one anything reaches: a real socket's sends come through
/// write/sendto, which the host socket extension already serves.
///
/// The part that matters is `SCM_RIGHTS`. nginx's master passes a *listening
/// descriptor* to each worker over the channel; without fd passing the worker
/// has a command and no socket to serve, which fails much later and looks
/// nothing like its cause.
#[inline(never)]
fn sys_sendmsg(state: &mut State, ctx: &mut EcvContext) {
    let (fd, hdr) = (state.arg(0) as usize, state.arg(1));
    let tx = match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::SocketPair { tx, .. }) => *tx,
        // A host socket has no msghdr path here; a scalar send is equivalent for
        // every caller we have, and pretending otherwise would be worse.
        Some(OpenFile::Socket { .. }) => {
            state.set_ret_err(ENOSYS);
            return;
        }
        Some(_) => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };
    if ctx.pipes[tx].readers == 0 {
        state.set_ret_err(EPIPE);
        return;
    }
    let h = ctx.arena.slice(hdr, MSGHDR_SIZE).to_vec();
    let iov = u64::from_le_bytes(h[MSGHDR_IOV..MSGHDR_IOV + 8].try_into().unwrap());
    let iovlen =
        u64::from_le_bytes(h[MSGHDR_IOVLEN..MSGHDR_IOVLEN + 8].try_into().unwrap()) as usize;
    let control = u64::from_le_bytes(h[MSGHDR_CONTROL..MSGHDR_CONTROL + 8].try_into().unwrap());
    let controllen = u64::from_le_bytes(
        h[MSGHDR_CONTROLLEN..MSGHDR_CONTROLLEN + 8]
            .try_into()
            .unwrap(),
    ) as usize;

    // Claim the rights BEFORE any payload byte lands, so a reader that wakes on
    // the data always finds the fds already queued ahead of it.
    let rights = parse_scm_rights(ctx, control, controllen);
    if !rights.is_empty() {
        // The QUEUE holds the reference, from here until recvmsg moves the entry
        // into the receiver's fd table or a plain read drops the batch. Without
        // this the receiver got a descriptor with no reference behind it -- the
        // same defect `dup_entry` had, and `parse_scm_rights` cannot take it
        // itself because it borrows the context immutably.
        for entry in &rights {
            ctx.retain_entry(entry);
        }
        ctx.pipes[tx].scm.push_back(rights);
    }

    let mut total = 0usize;
    for (base, len) in read_iovecs(ctx, iov, iovlen) {
        let src = ctx.arena.slice(base, len).to_vec();
        ctx.pipes[tx].buf.extend(src);
        total += len;
    }
    ctx.wake_pipe_readers(tx);
    state.set_ret(total as u64);
}

/// Collects the descriptors named by every SCM_RIGHTS control message, cloning
/// the sender's fd entries. Cloning (not moving) is correct: `sendmsg` leaves the
/// sender's descriptor open, and the receiver gets an independent reference.
fn parse_scm_rights(ctx: &EcvContext, control: u64, controllen: usize) -> Vec<OpenFile> {
    let mut out = Vec::new();
    if control == 0 || controllen < CMSGHDR_SIZE {
        return out;
    }
    let buf = ctx.arena.slice(control, controllen).to_vec();
    let mut off = 0usize;
    while off + CMSGHDR_SIZE <= buf.len() {
        let len = u64::from_le_bytes(buf[off..off + 8].try_into().unwrap()) as usize;
        let level = u32::from_le_bytes(buf[off + 8..off + 12].try_into().unwrap());
        let ty = u32::from_le_bytes(buf[off + 12..off + 16].try_into().unwrap());
        if len < CMSGHDR_SIZE || off + len > buf.len() {
            break; // malformed: stop rather than read past the caller's buffer
        }
        if level == SOL_SOCKET && ty == SCM_RIGHTS {
            for chunk in buf[off + CMSGHDR_SIZE..off + len].chunks_exact(4) {
                let fd = i32::from_le_bytes(chunk.try_into().unwrap());
                if let Some(Some(entry)) = ctx.fds.get(fd as usize) {
                    out.push(entry.clone());
                }
            }
        }
        // CMSG_ALIGN to the next header.
        let step = (len + 7) & !7;
        if step == 0 {
            break;
        }
        off += step;
    }
    out
}

/// recvmsg(fd, msghdr, flags): the receiving half of the channel. Scatters into
/// the iovecs and, if the caller supplied control space, installs any descriptors
/// the matching `sendmsg` attached.
#[inline(never)]
fn sys_recvmsg(state: &mut State, ctx: &mut EcvContext) {
    let (fd, hdr) = (state.arg(0) as usize, state.arg(1));
    let rx = match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::SocketPair { rx, .. }) => *rx,
        Some(OpenFile::Socket { .. }) => {
            state.set_ret_err(ENOSYS);
            return;
        }
        Some(_) => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };
    let h = ctx.arena.slice(hdr, MSGHDR_SIZE).to_vec();
    let iov = u64::from_le_bytes(h[MSGHDR_IOV..MSGHDR_IOV + 8].try_into().unwrap());
    let iovlen =
        u64::from_le_bytes(h[MSGHDR_IOVLEN..MSGHDR_IOVLEN + 8].try_into().unwrap()) as usize;
    let control = u64::from_le_bytes(h[MSGHDR_CONTROL..MSGHDR_CONTROL + 8].try_into().unwrap());
    let controllen = u64::from_le_bytes(
        h[MSGHDR_CONTROLLEN..MSGHDR_CONTROLLEN + 8]
            .try_into()
            .unwrap(),
    ) as usize;

    if ctx.pipes[rx].buf.is_empty() {
        if ctx.pipes[rx].writers == 0 {
            state.set_ret(0); // peer closed: orderly EOF
            return;
        }
        if ctx.is_nonblock(fd) {
            // nginx's `ngx_channel_handler` reads until this EAGAIN and only then
            // returns to the event loop. Blocking here parks the worker forever.
            state.set_ret_err(EAGAIN);
            return;
        }
        ctx.block_current(BlockedOn::PipeRead(rx));
        ctx.suspended = true;
        return;
    }

    let mut total = 0usize;
    for (base, len) in read_iovecs(ctx, iov, iovlen) {
        if ctx.pipes[rx].buf.is_empty() {
            break;
        }
        let n = len.min(ctx.pipes[rx].buf.len());
        let data: Vec<u8> = ctx.pipes[rx].buf.drain(..n).collect();
        ctx.arena.slice_mut(base, n).copy_from_slice(&data);
        total += n;
    }

    // Rights, if the caller left room for them. With no control buffer the fds
    // are dropped and MSG_CTRUNC is reported -- which is what the kernel does,
    // and is far better than silently leaking them into the receiver.
    let mut flags = 0u32;
    if let Some(rights) = ctx.pipes[rx].scm.pop_front() {
        let need = CMSGHDR_SIZE + rights.len() * 4;
        if control != 0 && controllen >= need {
            let mut fds = Vec::with_capacity(rights.len());
            for entry in rights {
                fds.push(ctx.alloc_fd(entry));
            }
            let mut cmsg = Vec::with_capacity(need);
            cmsg.extend_from_slice(&(need as u64).to_le_bytes());
            cmsg.extend_from_slice(&SOL_SOCKET.to_le_bytes());
            cmsg.extend_from_slice(&SCM_RIGHTS.to_le_bytes());
            for fd in &fds {
                cmsg.extend_from_slice(&fd.to_le_bytes());
            }
            ctx.arena.slice_mut(control, need).copy_from_slice(&cmsg);
            let clen = (need as u64).to_le_bytes();
            ctx.arena
                .slice_mut(hdr + MSGHDR_CONTROLLEN as u64, 8)
                .copy_from_slice(&clen);
        } else {
            flags |= MSG_CTRUNC;
            ctx.arena
                .slice_mut(hdr + MSGHDR_CONTROLLEN as u64, 8)
                .copy_from_slice(&0u64.to_le_bytes());
        }
    } else if control != 0 {
        ctx.arena
            .slice_mut(hdr + MSGHDR_CONTROLLEN as u64, 8)
            .copy_from_slice(&0u64.to_le_bytes());
    }
    ctx.arena
        .slice_mut(hdr + MSGHDR_FLAGS as u64, 4)
        .copy_from_slice(&flags.to_le_bytes());
    state.set_ret(total as u64);
}

/// sendfile(out_fd, in_fd, offset, count): copy within the guest. `in_fd` must be
/// a regular (Mem) file -- that is all Linux allows as the source anyway -- and
/// `out_fd` takes the bytes through the ordinary write path, so a socket, a pipe
/// and a socketpair all work without special-casing.
///
/// nginx serves every static response this way (`sendfile on;` is in the stock
/// config), so an ENOSYS here means no file is ever served.
#[inline(never)]
fn sys_sendfile(state: &mut State, ctx: &mut EcvContext) {
    let (out_fd, in_fd, off_ptr, count) = (
        state.arg(0) as usize,
        state.arg(1) as usize,
        state.arg(2),
        state.arg(3) as usize,
    );
    // Where to read from: *offset if given (and NOT advancing the file position),
    // otherwise the file's own position (which does advance).
    let explicit = if off_ptr != 0 {
        Some(u64::from_le_bytes(ctx.arena.slice(off_ptr, 8).try_into().unwrap()) as usize)
    } else {
        None
    };
    let (data, pos, in_off) = match ctx.fds.get(in_fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Mem { file, off, .. }) => (
            ctx.open_files[*file].data.clone(),
            ctx.file_offsets[*off].pos,
            *off,
        ),
        Some(_) => {
            state.set_ret_err(EINVAL); // Linux: in_fd must support mmap-like reads
            return;
        }
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };
    let start = explicit.unwrap_or(pos);
    if start >= data.len() {
        if off_ptr != 0 {
            ctx.arena
                .slice_mut(off_ptr, 8)
                .copy_from_slice(&(start as u64).to_le_bytes());
        }
        state.set_ret(0);
        return;
    }
    let n = count.min(data.len() - start);

    // Stage through a scratch VMA-free copy: write out via the normal path by
    // pointing it at a temporary arena buffer would need an allocation the arena
    // does not offer, so push the bytes directly per destination kind instead.
    let written = match ctx.fds.get(out_fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::SocketPair { tx, .. }) => {
            let tx = *tx;
            if ctx.pipes[tx].readers == 0 {
                state.set_ret_err(EPIPE);
                return;
            }
            ctx.pipes[tx].buf.extend(&data[start..start + n]);
            ctx.wake_pipe_readers(tx);
            n
        }
        Some(OpenFile::Pipe { idx, write: true }) => {
            let idx = *idx;
            if ctx.pipes[idx].readers == 0 {
                state.set_ret_err(EPIPE);
                return;
            }
            ctx.pipes[idx].buf.extend(&data[start..start + n]);
            ctx.wake_pipe_readers(idx);
            n
        }
        Some(OpenFile::Socket { h }) => {
            let h = *h;
            match socket_send_bytes(ctx, h, &data[start..start + n]) {
                Ok(w) => w,
                Err(e) => {
                    state.set_ret_err(e);
                    return;
                }
            }
        }
        Some(OpenFile::Stdio(hostfd)) => {
            let hostfd = *hostfd;
            let mut chunk = data[start..start + n].to_vec();
            let w = unsafe { write(hostfd, chunk.as_mut_ptr(), n) };
            if w < 0 {
                state.set_ret_err(EIO);
                return;
            }
            w as usize
        }
        Some(_) => {
            state.set_ret_err(EINVAL);
            return;
        }
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };

    if off_ptr != 0 {
        ctx.arena
            .slice_mut(off_ptr, 8)
            .copy_from_slice(&((start + written) as u64).to_le_bytes());
    } else {
        // No explicit offset: sendfile advances the DESCRIPTION's position, so a
        // descriptor sharing it sees the advance -- which is the point of the
        // `else` branch existing at all.
        ctx.file_offsets[in_off].pos = start + written;
    }
    state.set_ret(written as u64);
}

/// mkdirat(dirfd, path, mode): create a directory in the tmpfs upper layer.
/// nginx creates its temp directories (`client_body_temp`, `proxy_temp`, ...) at
/// startup and refuses to run if it cannot.
#[inline(never)]
fn sys_mkdirat(state: &mut State, ctx: &mut EcvContext) {
    let dirfd = state.arg(0) as i64;
    let path = ctx.arena.read_cstr(state.arg(1));
    // Relative paths resolve against the dirfd's directory; the errno is
    // unchanged from when this refused every such call, so only an
    // UNUSABLE dirfd reaches it now.
    let Some(base) = ctx.resolve_base(dirfd, &path).map(|b| b.to_vec()) else {
        state.set_ret_err(EINVAL);
        return;
    };
    if ctx.vfs.resolve(&base, &path, false).is_some() {
        state.set_ret_err(EEXIST);
        return;
    }
    let abs = absolutize(&base, &path);
    // The mode is kept, not discarded: initdb creates PGDATA with 0700 and the
    // postmaster then rejects anything that does not read back as 0700 or 0750.
    ctx.vfs
        .upper_mut()
        .mkdir_with_mode(&abs, state.arg(2) as u32);
    state.set_ret(0);
}

/// preadv2/pwritev2(fd, iov, iovcnt, pos_lo, pos_hi, flags). A negative offset
/// means "use the file position", i.e. plain readv/writev -- which is how musl
/// reaches this for an ordinary `pwrite` on some builds.
#[inline(never)]
fn sys_preadv2(state: &mut State, ctx: &mut EcvContext, is_write: bool) {
    let offset = state.arg(3) as i64;
    if offset < 0 {
        iovec_loop(state, ctx, is_write);
        return;
    }
    let (fd, vec_vma, vlen) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    let (saved, desc) = match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Mem { off, .. }) => (ctx.file_offsets[*off].pos, *off),
        // Only a seekable file has a meaningful offset; anything else is ESPIPE,
        // exactly as the kernel says.
        Some(_) => {
            state.set_ret_err(ESPIPE);
            return;
        }
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };
    ctx.file_offsets[desc].pos = offset as usize;
    state.gpr.x[0].val = fd as u64;
    state.gpr.x[1].val = vec_vma;
    state.gpr.x[2].val = vlen as u64;
    iovec_loop(state, ctx, is_write);
    // A positional read/write must not disturb the file position -- and now that
    // the position is shared, not disturbing it matters for the other holders of
    // the description too, not just for this descriptor.
    ctx.file_offsets[desc].pos = saved;
}

/// rt_sigsuspend(mask, sigsetsize): install `mask` as the blocked set, wait until
/// a handler runs, then restore and report EINTR. It never returns successfully --
/// EINTR is the only way out, by definition.
///
/// This is the second signal-delivery boundary, and the reason nginx can run at
/// all: `epoll_pwait` was the only one, because it was built for PostgreSQL's
/// postmaster (see `deliver_pending_signals`). nginx's master does nothing else --
/// `ngx_master_process_cycle` is suspend, wake on a signal, reap or reload, repeat
/// -- so without delivery here the master takes SIGCHLD and SIGTERM nowhere and
/// hangs forever with its workers running.
#[inline(never)]
fn sys_rt_sigsuspend(state: &mut State, ctx: &mut EcvContext) {
    let mask = if state.arg(0) != 0 {
        u64::from_le_bytes(ctx.arena.slice(state.arg(0), 8).try_into().unwrap())
    } else {
        0
    };
    // First entry installs the temporary mask; a resume finds it already there.
    if ctx.task_signals.sigsuspend_saved.is_none() {
        ctx.task_signals.sigsuspend_saved = Some(ctx.task_signals.blocked);
        ctx.task_signals.blocked = mask;
    }
    let finish = |ctx: &mut EcvContext, state: &mut State| {
        if let Some(saved) = ctx.task_signals.sigsuspend_saved.take() {
            ctx.task_signals.blocked = saved;
        }
        state.set_ret_err(EINTR);
    };

    // Anything deliverable under `mask` runs now. `deliver_pending_signals` takes
    // the signals to LEAVE pending, which is exactly the blocked set.
    if unsafe { ctx.deliver_pending_signals(mask) } > 0 {
        finish(ctx, state);
        return;
    }
    // Nothing ran. Park, as long as something else can still make progress and so
    // eventually post a signal -- another runnable process, or one parked on a
    // socket whose readiness the scheduler's idle host-poll services.
    let others_runnable = ctx
        .procs
        .iter()
        .enumerate()
        .any(|(i, p)| i != ctx.current && p.status == crate::context::ProcStatus::Runnable);
    if others_runnable || ctx.has_socket_waiters() {
        ctx.block_current(BlockedOn::Poll);
        ctx.suspended = true; // svc unwinds; re-entered on wake, mask still installed
        return;
    }
    // Nothing can ever post a signal: returning EINTR is a lie the guest handles
    // (it re-checks its flags and suspends again), whereas blocking here would
    // deadlock the module with no way out.
    finish(ctx, state);
}

/// rt_sigtimedwait(set, info, timeout, sigsetsize): wait for one of `set` to
/// become pending and ACCEPT it -- return its number, do not run its handler.
///
/// That distinction is the whole syscall. It is how a signal-driven program
/// avoids handlers altogether, and a runtime that implements it by delivering
/// would run guest code at a point the guest believes is a plain syscall return.
/// The caller is expected to have BLOCKED the set first (POSIX says the result
/// is undefined otherwise), which is also what keeps another delivery boundary
/// from stealing the signal first.
///
/// Both queues are searched: the signal may have been directed at the process
/// (`kill`) or at this thread (`tgkill`).
#[inline(never)]
fn sys_rt_sigtimedwait(state: &mut State, ctx: &mut EcvContext) {
    let (set_ptr, info_ptr, tmo_ptr) = (state.arg(0), state.arg(1), state.arg(2));
    if set_ptr == 0 || !ctx.arena.in_bounds(set_ptr, 8) {
        state.set_ret_err(EINVAL);
        return;
    }
    let set = u64::from_le_bytes(ctx.arena.slice(set_ptr, 8).try_into().unwrap());

    // A resumed call may have been released by its own deadline; consume the
    // flag before re-scanning, or a later timed wait inherits a fired timeout.
    let expired = ctx.resuming && ctx.take_timed_out();

    let avail = (ctx.signals.pending | ctx.task_signals.pending) & set;
    if avail != 0 {
        let sig = avail.trailing_zeros() + 1;
        ctx.consume_signal(1u64 << (sig - 1));
        if info_ptr != 0 && ctx.arena.in_bounds(info_ptr, SIGNALFD_SIGINFO_SIZE) {
            // Minimal siginfo_t: only si_signo is meaningful here, as in the
            // signalfd path. A caller that reads si_code gets 0, which is
            // SI_USER -- true of every signal this runtime can post.
            let b = ctx.arena.slice_mut(info_ptr, SIGNALFD_SIGINFO_SIZE);
            b.fill(0);
            b[0..4].copy_from_slice(&sig.to_le_bytes());
        }
        state.set_ret(sig as u64);
        return;
    }

    // NULL timeout waits forever; a zero timespec polls.
    let timeout_ns: Option<u128> = if tmo_ptr == 0 {
        None
    } else {
        let b = ctx.arena.slice(tmo_ptr, 16);
        let sec = u64::from_le_bytes(b[0..8].try_into().unwrap()) as u128;
        let nsec = u64::from_le_bytes(b[8..16].try_into().unwrap()) as u128;
        Some(sec * 1_000_000_000 + nsec)
    };
    if expired || timeout_ns == Some(0) {
        state.set_ret_err(EAGAIN);
        return;
    }

    // Arm the deadline once per WAIT, not once per park -- see sys_ppoll.
    if !ctx.resuming || ctx.current_deadline().is_none() {
        ctx.set_current_deadline(timeout_ns.map(|ns| crate::context::mono_nanos() + ns));
    }

    // Park as a poller: `post_signal` wakes every poll waiter, which is exactly
    // the event this call is waiting for. Never park the last runnable process
    // with no deadline -- nothing could post the signal, and the module would
    // deadlock instead of returning.
    let others_runnable = ctx
        .procs
        .iter()
        .enumerate()
        .any(|(i, p)| i != ctx.current && p.status == crate::context::ProcStatus::Runnable);
    if others_runnable || ctx.has_socket_waiters() || timeout_ns.is_some() {
        ctx.block_current(BlockedOn::Poll);
        ctx.suspended = true;
        return;
    }
    state.set_ret_err(EAGAIN);
}

/// pipe2: create a pipe and write its [read_fd, write_fd] into the guest array.
/// O_CLOEXEC in flags (arg 1) marks both ends close-on-exec.
#[inline(never)]
fn sys_pipe2(state: &mut State, ctx: &mut EcvContext) {
    let arr = state.arg(0);
    let cloexec = state.arg(1) & O_CLOEXEC != 0;
    let idx = new_pipe(ctx, 1, 1);
    let rfd = ctx.alloc_fd(OpenFile::Pipe { idx, write: false });
    let wfd = ctx.alloc_fd(OpenFile::Pipe { idx, write: true });
    ctx.set_cloexec(rfd as usize, cloexec);
    ctx.set_cloexec(wfd as usize, cloexec);
    let dst = ctx.arena.slice_mut(arr, 8);
    dst[..4].copy_from_slice(&(rfd as u32).to_le_bytes());
    dst[4..].copy_from_slice(&(wfd as u32).to_le_bytes());
    state.set_ret(0);
}

#[inline(never)]
fn sys_dup(state: &mut State, ctx: &mut EcvContext) {
    let old = state.arg(0) as usize;
    match dup_entry(ctx, old) {
        Some(entry) => {
            let fd = ctx.alloc_fd(entry);
            state.set_ret(fd as u64);
        }
        None => state.set_ret_err(EBADF),
    }
}

/// dup3(oldfd, newfd, flags): duplicate oldfd onto the specific newfd. glibc's
/// dup2 lowers to dup3(old, new, 0), so flags=0 leaves the new fd inheritable;
/// O_CLOEXEC in flags marks it close-on-exec.
#[inline(never)]
fn sys_dup3(state: &mut State, ctx: &mut EcvContext) {
    let (old, new) = (state.arg(0) as usize, state.arg(1) as usize);
    let flags = state.arg(2);
    let entry = match dup_entry(ctx, old) {
        Some(e) => e,
        None => {
            state.set_ret_err(EBADF);
            return;
        }
    };
    // dup3 silently closes an already-open newfd, and that IS a close: it has to
    // flush a dirty mem file, drop its reference, and refcount a socket handle.
    // A second, weaker close existed here and released only pipe ends, which is
    // how it came to leak a mem-file reference every time a guest dup2'd onto an
    // open descriptor.
    close_fd_full(ctx, new);
    if new >= ctx.fds.len() {
        ctx.fds.resize_with(new + 1, || None);
    }
    ctx.fds[new] = Some(entry);
    ctx.set_cloexec(new, flags & O_CLOEXEC != 0);
    state.set_ret(new as u64);
}

/// Clones an fd's entry for dup, taking a reference on whatever the entry names
/// via the shared [`EcvContext::retain_entry`].
///
/// The mem file was the kind that was missing. A `Mem` entry carries an INDEX
/// into the shared `open_files` table, and that table recycles
/// a slot as soon as its refs reach zero. A dup that copied the index without
/// taking a reference left two descriptors and one reference: closing either
/// dropped the count to zero, the buffer was flushed and the slot marked free
/// while the survivor still pointed at it, and the next unrelated `open` took
/// the slot over. From then on the surviving descriptor read and wrote a
/// different file, with no error at any point.
///
/// Measured on one PostgreSQL boot: 5,265 slot recycles, and the guest symptom
/// was `invalid page in block 1 of relation global/1262` from a relation exactly
/// one block long -- a full page read out of whatever file had taken the slot.
/// Guarded by `e2e/dupfd_test.go`.
fn dup_entry(ctx: &mut EcvContext, fd: usize) -> Option<OpenFile> {
    let entry = ctx.fds.get(fd).and_then(|s| s.clone())?;
    ctx.retain_entry(&entry);
    Some(entry)
}

/// Drops this process's references to an fd's pipe ends, waking the readers of
/// any direction whose last writer just went away so they see EOF.
fn release_pipe_ends(ctx: &mut EcvContext, entry: &OpenFile) {
    for (idx, write) in entry.pipe_ends() {
        if write {
            ctx.pipes[idx].writers -= 1;
            if ctx.pipes[idx].writers == 0 {
                ctx.wake_pipe_readers(idx); // readers now see EOF
            }
        } else {
            ctx.pipes[idx].readers -= 1;
        }
        // A direction with no reader AND no writer can never deliver what is
        // queued on it, so any SCM_RIGHTS batch still sitting there is
        // unreachable and its references have to go back. The queue took them at
        // `sendmsg` (see EcvContext::retain_entry); without this a socketpair
        // that dies with rights in flight -- a sender that exits before its peer
        // calls recvmsg -- pins an `open_files` slot for the life of the module.
        if ctx.pipes[idx].readers == 0 && ctx.pipes[idx].writers == 0 {
            let orphaned: Vec<Vec<OpenFile>> = ctx.pipes[idx].scm.drain(..).collect();
            for batch in orphaned {
                for e in batch {
                    release_entry(ctx, e);
                }
            }
        }
    }
}

/// Drops `fd` from every epoll interest list this process holds.
///
/// Linux does this implicitly: a descriptor leaves the interest set when it is
/// closed, so a server that accepts, registers, serves and closes in a loop may
/// re-register the same fd NUMBER on the next connection. Without this, the
/// second registration hits our own `EEXIST` check and nginx logs
///
///     [alert] epoll_ctl(1, 7) failed (17: File exists) while waiting for request
///
/// on every connection after the first -- it accepts, cannot arm the event, and
/// never reads the request. Found the moment `patches/0025` let nginx get as far
/// as accepting at all.
///
/// This keys on the descriptor NUMBER, where Linux keys on the open file
/// description: a dup'd fd keeps the registration alive there and loses it here.
/// Modelling that needs odf identity the fd table does not carry, and no guest
/// has needed it; the divergence is deliberate and narrow.
fn purge_epoll_interest(ctx: &mut EcvContext, fd: usize) {
    let fd = fd as i32;
    for slot in ctx.fds.iter_mut().flatten() {
        if let OpenFile::Epoll { interest } = slot {
            interest.retain(|i| i.fd != fd);
        }
    }
}

// fcntl commands (generic Linux asm-generic/fcntl.h).
const F_DUPFD: u64 = 0;
const F_GETFD: u64 = 1;
const F_SETFD: u64 = 2;
const F_GETFL: u64 = 3;
const F_SETFL: u64 = 4;
const F_SETOWN: u64 = 8;
const F_GETOWN: u64 = 9;
const F_DUPFD_CLOEXEC: u64 = 1030;

// The close-on-exec flag bit: FD_CLOEXEC in F_GET/SETFD (value 1), and O_CLOEXEC /
// SOCK_CLOEXEC in open/openat/dup3/pipe2/socket/accept4 (value 0o2000000 on Linux;
// SOCK_CLOEXEC shares O_CLOEXEC's value).
const FD_CLOEXEC: u64 = 1;
const O_CLOEXEC: u64 = 0o2000000;
const SOCK_CLOEXEC: u64 = O_CLOEXEC;
// O_NONBLOCK in open/openat flags and F_SETFL; SOCK_NONBLOCK shares its value.
const O_NONBLOCK: u64 = 0o4000;

/// fcntl(fd, cmd, arg): enough of the fd-management commands for a shell to run.
/// A shell moves its script and redirection fds out of the way with F_DUPFD /
/// F_DUPFD_CLOEXEC (dash dups the script fd to >= 10) and sets FD_CLOEXEC on fds
/// that must not leak into an execve'd child (the script fd, redirections). We now
/// track FD_CLOEXEC: F_SETFD/F_GETFD read/write the flag, F_DUPFD_CLOEXEC sets it
/// on the new fd (plain F_DUPFD clears it), and execve closes every cloexec fd. The
/// file status flags (F_GET/SETFL) are still best-effort GET=0/SET=accept. An
/// unknown command returns EINVAL, not ENOSYS, so it reads as an unsupported op
/// rather than a missing syscall.
#[inline(never)]
fn sys_fcntl(state: &mut State, ctx: &mut EcvContext) {
    let fd = state.arg(0) as usize;
    let cmd = state.arg(1);
    let arg = state.arg(2);
    if ctx.fds.get(fd).and_then(|s| s.as_ref()).is_none() {
        state.set_ret_err(EBADF);
        return;
    }
    match cmd {
        F_DUPFD | F_DUPFD_CLOEXEC => match dup_entry(ctx, fd) {
            Some(entry) => {
                let newfd = alloc_fd_min(ctx, arg as usize, entry);
                // alloc_fd_min cleared the new fd's flag; F_DUPFD_CLOEXEC re-sets it.
                if cmd == F_DUPFD_CLOEXEC {
                    ctx.set_cloexec(newfd as usize, true);
                }
                state.set_ret(newfd as u64);
            }
            None => state.set_ret_err(EBADF),
        },
        F_GETFD => state.set_ret(if ctx.is_cloexec(fd) { FD_CLOEXEC } else { 0 }),
        F_SETFD => {
            ctx.set_cloexec(fd, arg & FD_CLOEXEC != 0);
            state.set_ret(0);
        }
        // Only O_NONBLOCK is modelled; the rest of the status flags read back as
        // clear. musl's `fcntl(F_SETFL)` is the other route to non-blocking mode
        // (nginx uses `ioctl(FIONBIO)`, but the two must agree).
        F_GETFL => state.set_ret(if ctx.is_nonblock(fd) { O_NONBLOCK } else { 0 }),
        F_SETFL => {
            ctx.set_nonblock(fd, arg & O_NONBLOCK != 0);
            state.set_ret(0);
        }
        // F_SETOWN names the process that receives SIGIO/SIGURG on this fd.
        // `ngx_spawn_process` calls it on the master's end of each worker channel
        // right after `ioctl(FIOASYNC)`, and like that ioctl it returns
        // NGX_INVALID_PID on failure -- before the fork -- so EINVAL here would
        // kill worker creation outright.
        //
        // Not recorded. We never deliver SIGIO (see the FIOASYNC note in the
        // dispatch table), and storing an owner would need a per-fd vector
        // threaded through fork and execve exactly like `cloexec`, to feed a
        // signal that no code path raises. F_GETOWN reporting 0 -- "no owner" --
        // is the truthful answer for a descriptor whose SIGIO is not armed.
        F_SETOWN | F_GETOWN => state.set_ret(0),
        _ => state.set_ret_err(EINVAL),
    }
}

/// Allocates the lowest free fd >= `min`, placing `f` there (F_DUPFD semantics).
/// The new fd is not close-on-exec (dup clears the flag; F_DUPFD_CLOEXEC re-sets it).
fn alloc_fd_min(ctx: &mut EcvContext, min: usize, f: OpenFile) -> i32 {
    let fd = if min >= ctx.fds.len() {
        ctx.fds.resize_with(min + 1, || None);
        ctx.fds[min] = Some(f);
        min
    } else if let Some(i) = (min..ctx.fds.len()).find(|&i| ctx.fds[i].is_none()) {
        ctx.fds[i] = Some(f);
        i
    } else {
        ctx.fds.push(Some(f));
        ctx.fds.len() - 1
    };
    ctx.set_cloexec(fd, false);
    fd as i32
}

/// execve: replace the current process's image with a translated program, then
/// suspend so the scheduler fresh-starts it. `exec_into` resets the live arena
/// + State to the new program; the old wasm stack is discarded by the unwind.
#[inline(never)]
fn sys_execve(state: &mut State, ctx: &mut EcvContext) {
    let mut path = ctx.arena.read_cstr(state.arg(0));
    let mut argv = read_ptr_array(&ctx.arena, state.arg(1));
    let envp = read_ptr_array(&ctx.arena, state.arg(2));

    // `#!` levels followed so far. Bounded like Linux's BINPRM_MAX_RECURSION;
    // a cycle (`a` -> `b` -> `a`) terminates here rather than looping.
    let mut sb_depth = 0usize;

    let idx = 'resolve: loop {
        match ctx.programs.resolve(&ctx.vfs, &ctx.cwd, &path) {
            Some(i) => break 'resolve i,
            None => {
                // ❗ A SCRIPT, BEFORE the host-loader path below. A `#!` file is
                // never a unit an embedder can place, so asking the host about it
                // would park the guest waiting for something that cannot arrive.
                //
                // ⚠️ This reads the whole file to look at its first two bytes,
                // because `Vfs::read` is all-or-nothing. It sits on a path that has
                // ALREADY failed to resolve, so the common cases are cheap: a
                // missing path returns None without reading, and a real program
                // resolved above. What it does cost is a large non-program file --
                // rare, and not worth a second VFS entry point until something
                // measures it.
                if let Some(sb) = ctx
                    .vfs
                    .read(&ctx.cwd, &path)
                    .as_deref()
                    .and_then(crate::shebang::parse)
                {
                    sb_depth += 1;
                    if sb_depth > crate::shebang::MAX_DEPTH {
                        ecv_trace!(
                            ecvisor,
                            "execve({}): more than {} #! levels",
                            String::from_utf8_lossy(&path),
                            crate::shebang::MAX_DEPTH
                        );
                        state.set_ret_err(ELOOP);
                        return;
                    }
                    argv = crate::shebang::rewrite_argv(&sb, &path, &argv);
                    path = sb.interp;
                    continue 'resolve;
                }
                // ❗ NOT-RESOLVED IS TWO SITUATIONS HERE TOO, exactly as it is for
                // `dlopen`. Under a host-driven loader a program's descriptor lives
                // inside its side module, so a target the host has not placed yet
                // has no registry index -- and reporting ENOEXEC for it would deny
                // the one thing that could fix it.
                //
                // `hash_for` separates them: a hash means this module was BUILT with
                // that program and the host simply has not placed it.
                match ctx.programs.hash_for(&ctx.vfs, &ctx.cwd, &path) {
                    Some(hash) => match ctx.ensure_unregistered_unit(&hash) {
                        Ok(UnitLoadStep::Parked) => {
                            // Parked; the guest re-enters this execve when woken and
                            // resolves for real. Touching neither `state` nor the
                            // arena is what makes the retry see its original argv.
                            return;
                        }
                        Ok(UnitLoadStep::Done) => {
                            match ctx.programs.resolve(&ctx.vfs, &ctx.cwd, &path) {
                                // `break 'resolve`, not a tail value: the
                                // resolution now sits inside the `#!` loop.
                                Some(i) => break 'resolve i,
                                None => {
                                    // The host said it loaded the program and did not
                                    // register it. ENOEXEC rather than a fallback:
                                    // `execmap.rs` records four incidents caused by
                                    // running SOME program under the right argv.
                                    ecv_trace!(
                                        ecvisor,
                                        "execve({}): the host reported the unit loaded \
                                     but did not register its descriptor",
                                        String::from_utf8_lossy(&path)
                                    );
                                    state.set_ret_err(ENOEXEC);
                                    return;
                                }
                            }
                        }
                        Err(why) => {
                            ecv_trace!(
                                ecvisor,
                                "execve({}) refused by the host: {}",
                                String::from_utf8_lossy(&path),
                                why
                            );
                            state.set_ret_err(ENOEXEC);
                            return;
                        }
                    },
                    None => {
                        // A path that exists but has no translated program is ENOEXEC
                        // (e.g. a data file, a dynamic ELF, or a script — shebang
                        // support is a later addition); a missing path is ENOENT.
                        let err = if ctx.vfs.resolve(&ctx.cwd, &path, true).is_some() {
                            ENOEXEC
                        } else {
                            ENOENT
                        };
                        state.set_ret_err(err);
                        return;
                    }
                }
            }
        }
    };

    // The SECOND trigger of the loader seam. `dlopen` adds a unit to the running
    // image; `execve` replaces the image with one -- but both first have to make
    // the unit's code reachable, and on a split artifact that means instantiating
    // a side module the embedder has not placed yet.
    //
    // On the flat artifact every program is already linked in and this is a
    // no-op, which is why it can sit on the execve path unconditionally.
    //
    // ⚠️ Reported as ENOEXEC rather than swallowed. The alternative -- carry on
    // and let `exec_into` run a program whose tables are absent -- is the same
    // class of failure as the exec-map fallback to program 0 that `execmap.rs`
    // records four incidents for: the guest runs, and is wrong.
    // ❗ `ensure_unit_code`, NOT `ensure_unit_loaded`. This called the latter,
    // which also REFUSES a unit carrying its own `.ecv.tls` -- a rule that
    // belongs to `dlopen`, where a unit joins a running image whose static TLS
    // block is already laid out. An `execve` replaces the image, so `exec_into`
    // lays TLS out fresh.
    //
    // ⚠️ The conflation made `execve` return ENOEXEC for every program with
    // static TLS, which is every fused DYNAMIC program. It survived because
    // `e2e`'s `compileGuest` builds guests with `gcc -static`.
    match ctx.ensure_unit_code(idx) {
        Ok(UnitLoadStep::Done) => {}
        Ok(UnitLoadStep::Parked) => {
            // Parked awaiting the host; the guest re-enters this execve when
            // woken. Returning here without touching `state` is what makes the
            // retry see its original arguments.
            return;
        }
        Err(why) => {
            ecv_trace!(
                ecvisor,
                "execve({}) refused: {}",
                String::from_utf8_lossy(&path),
                why
            );
            state.set_ret_err(ENOEXEC);
            return;
        }
    }

    ctx.exec_into(idx, &argv, &envp); // resets live arena/State, sets Pending::Yield
    ctx.suspended = true; // svc unwinds; the process re-enters fresh (started=false)
}

/// Reads a NULL-terminated array of guest (64-bit) pointers, each to a C
/// string, into owned byte vectors.
fn read_ptr_array(arena: &Arena, mut vma: u64) -> Vec<Vec<u8>> {
    let mut out = Vec::new();
    if vma == 0 {
        return out;
    }
    loop {
        let p = u64::from_le_bytes(arena.slice(vma, 8).try_into().unwrap());
        if p == 0 {
            break;
        }
        out.push(arena.read_cstr(p));
        vma += 8;
    }
    out
}

#[inline(never)]
/// `fchdir(fd)`: make the directory `fd` is open on the cwd.
///
/// Added 2026-09-01. Before this, syscall 50 fell through to the ENOSYS
/// catch-all, and `find` -- which `fchdir`s to descend and again to restore
/// where it started -- reported "Failed to change directory: Function not
/// implemented" while walking `postgres:17`'s entrypoint.
///
/// The path is taken from the descriptor, so it is the directory the fd was
/// opened on even if that directory has since been renamed -- which is the
/// behaviour `find` depends on and the reason it uses `fchdir` rather than
/// re-`chdir`ing by name.
fn sys_fchdir(state: &mut State, ctx: &mut EcvContext) {
    let fd = state.arg(0) as i64;
    // EBADF and ENOTDIR are distinguished because a caller that gets ENOTDIR
    // for a closed fd cannot tell a bug in itself from a bug here.
    if fd < 0 || ctx.fds.get(fd as usize).and_then(|s| s.as_ref()).is_none() {
        state.set_ret_err(EBADF);
        return;
    }
    match ctx.dir_fd_path(fd) {
        Some(p) => {
            let p = p.to_vec();
            ctx.cwd = p;
            state.set_ret(0);
        }
        None => state.set_ret_err(ENOTDIR),
    }
}

fn sys_chdir(state: &mut State, ctx: &mut EcvContext) {
    let path = ctx.arena.read_cstr(state.arg(0));
    match ctx.vfs.resolve(&ctx.cwd, &path, true) {
        Some(r) if r.meta.kind == NodeKind::Dir => {
            ctx.cwd = r.path;
            state.set_ret(0);
        }
        Some(_) => state.set_ret_err(ENOTDIR),
        None => state.set_ret_err(ENOENT),
    }
}

// --- socket syscalls -----------------------------------------------------
//
// The guest makes raw aarch64 Linux socket syscalls; these translate Linux
// sockaddr_in/sockaddr_in6 <-> the WasmEdge socket-extension address form and
// bridge to the host sock_* imports. Data flows over the socket fd via the
// existing read/write (fd_read/fd_write) path (OpenFile::Socket arms above).
//
// Cooperative (non-blocking) sockets: ecvisor flips every host socket
// non-blocking (fd_fdstat_set_flags) right after it is created, so a would-block
// recv/accept/send returns EAGAIN instead of parking the single host goroutine.
// On EAGAIN the guest process SUSPENDS (asyncify unwind, exactly like a blocked
// pipe read) via BlockedOn::Socket, and the scheduler's idle path host-polls the
// pending socket fds when nothing else is runnable. connect stays synchronous
// (net.Dial completes immediately for loopback); a suspend on a slow/remote
// connect is a follow-up. See context.rs and .agents/docs/NETWORKING.md.

/// Resolves an fd to its backend socket handle, or None if not a socket.
fn socket_hostfd(ctx: &EcvContext, fd: usize) -> Option<NetHandle> {
    match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Socket { h }) => Some(*h),
        _ => None,
    }
}

/// Marks the current process blocked on socket readiness and unwinds. Mirrors
/// read_pipe's block: the CALLING syscall handler (which re-runs from its top on
/// resume, clearing `resuming`/stop_rewind) re-attempts the op.
fn socket_block(ctx: &mut EcvContext, h: NetHandle, write: bool) {
    // ⚠️ NOT a set wait: this process is waiting on THIS socket for THIS
    // operation, and a spurious wake would make it re-attempt something that is
    // not ready. See `BlockedOn::Socket`.
    ctx.block_current(BlockedOn::Socket {
        h,
        write,
        poll: false,
    });
    ctx.suspended = true; // svc unwinds
}

/// recv on a socket fd via the host fd_read. Returns the data, EOF (0), or (on
/// would-block) suspends the process. Used by sys_read's Socket arm and
/// sys_recvfrom's NULL-address (recv) path; both clear `resuming` at their top.
/// recv on a socket fd via the host fd_read.
///
/// A would-block suspends the process ONLY if the guest left the descriptor
/// blocking. Honouring the guest's own flag is not a refinement, it is the
/// difference between a working event loop and a wedged one: an nginx worker
/// that has finished a response calls `recvfrom` on the keepalive connection
/// expecting EAGAIN so it can return to `epoll_wait`. Suspending there parks the
/// whole process inside the read -- out of its event loop, unable to accept, to
/// run a timer, or to serve any other connection -- until that ONE client sends
/// bytes or hangs up. The pipe path (`read_pipe`) has always got this right;
/// sockets were the outlier.
fn socket_recv(
    state: &mut State,
    ctx: &mut EcvContext,
    fd: usize,
    h: NetHandle,
    buf: u64,
    count: usize,
) {
    // The arena and the backend are disjoint fields, so destructuring splits
    // the borrow without `unsafe`. Scoped so `ctx` is whole again below, where
    // the EAGAIN arms need it.
    let r = {
        let EcvContext { net, arena, .. } = &mut *ctx;
        net.recv(h, arena.slice_mut(buf, count))
    };
    match r {
        Ok(n) => state.set_ret(n as u64),
        Err(NetErr::AGAIN) if ctx.is_nonblock(fd) => state.set_ret_err(EAGAIN),
        Err(NetErr::AGAIN) => socket_block(ctx, h, false),
        Err(e) => state.set_ret_err(errno_of(e, Op::Recv)),
    }
}

/// send on a socket fd via the host fd_write. Returns the count sent; a
/// would-block yields EAGAIN on a non-blocking descriptor and otherwise
/// suspends, for the reasons in `socket_recv`. Used by sys_write's Socket arm
/// and sys_sendto's NULL-destination (send) path; both clear `resuming` at their
/// top. `socket_send_bytes`, the sendfile source path, already returned EAGAIN
/// unconditionally -- its comment about nginx's output chain being written to
/// resume on EAGAIN applies just as much here.
fn socket_send(
    state: &mut State,
    ctx: &mut EcvContext,
    fd: usize,
    h: NetHandle,
    buf: u64,
    count: usize,
) {
    let r = {
        let EcvContext { net, arena, .. } = &mut *ctx;
        net.send(h, arena.slice(buf, count))
    };
    match r {
        Ok(n) => state.set_ret(n as u64),
        Err(NetErr::AGAIN) if ctx.is_nonblock(fd) => state.set_ret_err(EAGAIN),
        Err(NetErr::AGAIN) => socket_block(ctx, h, true),
        Err(e) => state.set_ret_err(errno_of(e, Op::Send)),
    }
}

/// Sends bytes that are NOT in the guest's address space -- `sendfile`'s source
/// is an in-memory file, not a VMA. A Rust slice already points into the wasm
/// linear memory the host import reads from, so it needs no arena translation;
/// that is the only thing `ctx.arena.translate` was doing for `socket_send`.
///
/// EAGAIN is returned to the guest rather than blocking. That is the honest
/// answer for a partial copy, and it is nginx's normal path anyway: its sockets
/// are non-blocking and its output chain is written to resume on EAGAIN.
fn socket_send_bytes(ctx: &mut EcvContext, h: NetHandle, bytes: &[u8]) -> Result<usize, u64> {
    ctx.net.send(h, bytes).map_err(|e| errno_of(e, Op::Send))
}

/// Reads a guest sockaddr_in/sockaddr_in6 out of the arena.
///
/// The parsing itself lives in , which is pure and host
/// tested; this is only the arena access.  is the caller-supplied
/// length (a value, as in connect/bind/sendto) and is load-bearing: an
/// AF_INET6 address is only accepted when the caller claims 28 bytes.
fn parse_sockaddr(ctx: &EcvContext, addr: u64, addrlen: u64) -> Option<SockAddr> {
    let want = (addrlen as usize).min(28);
    if want < 8 {
        return None;
    }
    crate::net::addr::parse(ctx.arena.slice(addr, want), addrlen as usize)
}

/// Writes a Linux sockaddr into the guest, honouring the in/out
/// pointer (accept / getsockname / recvfrom).
fn write_sockaddr(ctx: &mut EcvContext, addr_ptr: u64, addrlen_ptr: u64, a: &SockAddr) {
    if addr_ptr == 0 {
        return;
    }
    let (buf, size) = crate::net::addr::encode(a);
    let avail = if addrlen_ptr != 0 {
        u32::from_le_bytes(ctx.arena.slice(addrlen_ptr, 4).try_into().unwrap()) as usize
    } else {
        size
    };
    let (n, true_size) = crate::net::addr::fit(size, avail);
    ctx.arena.slice_mut(addr_ptr, n).copy_from_slice(&buf[..n]);
    if addrlen_ptr != 0 {
        // The TRUE size, not the copied size: that is how a caller learns its
        // buffer was too small. See .
        ctx.arena
            .slice_mut(addrlen_ptr, 4)
            .copy_from_slice(&(true_size as u32).to_le_bytes());
    }
}

/// Asks the backend for a socket's local/peer address and stores it as a Linux
/// sockaddr in the guest (accept peer, getsockname, getpeername).
fn store_addr(ctx: &mut EcvContext, h: NetHandle, addr_ptr: u64, addrlen_ptr: u64, peer: bool) {
    if addr_ptr == 0 {
        return;
    }
    let Ok(a) = ctx.net.addr(h, peer) else {
        return;
    };
    write_sockaddr(ctx, addr_ptr, addrlen_ptr, &a);
}

/// Parses a Linux `sockaddr_un` into the rendezvous NAME used as the key in
/// `EcvContext::unix_listeners`. Returns None if this is not an AF_UNIX address.
///
/// `struct sockaddr_un { sa_family_t sun_family; char sun_path[108]; }`, and the
/// first byte of `sun_path` selects between two namespaces that must never
/// collide:
///
///   - **pathname**: a NUL-terminated filesystem path. The key is that path
///     absolutized against the cwd, so two processes with different cwds that
///     name the same socket find each other.
///   - **abstract** (Linux-only): a leading NUL followed by `addrlen - 3` bytes
///     that are NOT a path, may contain NULs, and have no filesystem node. The
///     key keeps its leading NUL, which is what makes it unable to collide with
///     any pathname key -- an absolutized path always starts with `/`.
///
/// The trailing length matters for abstract names and only for them: the name is
/// delimited by `addrlen`, not by a NUL. Getting that wrong would silently merge
/// distinct abstract sockets whose names share a prefix.
fn parse_sockaddr_un(ctx: &EcvContext, addr: u64, addrlen: u64) -> Option<Vec<u8>> {
    if addr == 0 || addrlen < 2 {
        return None;
    }
    let hdr = ctx.arena.slice(addr, 2);
    if u16::from_le_bytes([hdr[0], hdr[1]]) as u32 != AF_UNIX {
        return None;
    }
    // An addrlen of exactly 2 is the "unnamed" address; nothing can bind or
    // connect to it.
    let path_len = (addrlen as usize - 2).min(108);
    if path_len == 0 {
        return None;
    }
    let raw = ctx.arena.slice(addr + 2, path_len).to_vec();
    if raw[0] == 0 {
        // Abstract: keep the leading NUL, drop nothing else. glibc passes the
        // exact length; a caller that padded with zeroes gets those as part of
        // the name, which is also what the kernel does.
        return Some(raw);
    }
    let path: Vec<u8> = raw.into_iter().take_while(|&c| c != 0).collect();
    if path.is_empty() {
        return None;
    }
    Some(absolutize(&ctx.cwd, &path))
}

/// Writes a `sockaddr_un` for `name` (a key from `parse_sockaddr_un`) into the
/// guest, truncating to the caller's buffer but reporting the untruncated
/// length, as the kernel does. An empty `name` yields the 2-byte unnamed
/// address, which is what `accept` reports for a peer that never bound.
fn write_sockaddr_un(ctx: &mut EcvContext, addr_ptr: u64, addrlen_ptr: u64, name: &[u8]) {
    if addr_ptr == 0 {
        return;
    }
    let mut buf = vec![0u8; 2 + name.len() + 1];
    buf[0..2].copy_from_slice(&(AF_UNIX as u16).to_le_bytes());
    buf[2..2 + name.len()].copy_from_slice(name);
    // The reported length includes the terminating NUL for a pathname and does
    // not for an abstract name -- and is a bare 2 for the unnamed address.
    let size = if name.is_empty() {
        2
    } else if name[0] == 0 {
        2 + name.len()
    } else {
        2 + name.len() + 1
    };
    let avail = if addrlen_ptr != 0 {
        u32::from_le_bytes(ctx.arena.slice(addrlen_ptr, 4).try_into().unwrap()) as usize
    } else {
        size
    };
    let n = size.min(avail).min(buf.len());
    ctx.arena.slice_mut(addr_ptr, n).copy_from_slice(&buf[..n]);
    if addrlen_ptr != 0 {
        ctx.arena
            .slice_mut(addrlen_ptr, 4)
            .copy_from_slice(&(size as u32).to_le_bytes());
    }
}

/// The `unix_listeners` index of a LIVE listener currently reachable under
/// `name`. A dead entry (its bound fd closed) and an unlinked one (its path
/// cleared) are both unreachable, which is the whole reason those two states
/// exist separately from removal.
fn find_unix_listener(ctx: &EcvContext, name: &[u8]) -> Option<usize> {
    ctx.unix_listeners
        .iter()
        .position(|l| !l.dead && !l.path.is_empty() && l.path == name)
}

#[inline(never)]
fn sys_socket(state: &mut State, ctx: &mut EcvContext) {
    let domain = state.arg(0);
    let stype = state.arg(1);
    if domain == AF_UNIX as u64 {
        // A named UDS is implemented entirely in-runtime -- see `UnixListener`.
        // SOCK_DGRAM is refused rather than faked: a datagram UDS has different
        // message-boundary and connectionless semantics, and the pipe-backed
        // data plane here provides neither. Loud beats plausible.
        if stype & SOCK_TYPE_MASK != SOCK_STREAM {
            state.set_ret_err(EINVAL);
            return;
        }
        let fd = ctx.alloc_fd(OpenFile::UnixSocket { listener: None });
        ctx.set_cloexec(fd as usize, stype & SOCK_CLOEXEC != 0);
        // SOCK_NONBLOCK shares O_NONBLOCK's value (see sys_accept).
        ctx.set_nonblock(fd as usize, stype & O_NONBLOCK != 0);
        state.set_ret(fd as u64);
        return;
    }
    let v6 = match domain {
        AF_INET => false,
        AF_INET6 => true,
        _ => {
            state.set_ret_err(EAFNOSUPPORT);
            return;
        }
    };
    let dgram = match stype & SOCK_TYPE_MASK {
        SOCK_STREAM => false,
        SOCK_DGRAM => true,
        _ => {
            state.set_ret_err(EINVAL);
            return;
        }
    };
    // The backend keeps its handles non-blocking; that is its concern, not
    // this layer's. The guest's own O_NONBLOCK lives in `ctx.nonblock`.
    let h = match ctx.net.socket(v6, dgram) {
        Ok(h) => h,
        Err(e) => {
            state.set_ret_err(errno_of(e, Op::Other));
            return;
        }
    };
    let fd = ctx.alloc_fd(OpenFile::Socket { h });
    ctx.set_cloexec(fd as usize, stype & SOCK_CLOEXEC != 0);
    state.set_ret(fd as u64);
}

#[inline(never)]
fn sys_connect(state: &mut State, ctx: &mut EcvContext) {
    let (fd, addr, addrlen) = (state.arg(0) as usize, state.arg(1), state.arg(2));
    if matches!(
        ctx.fds.get(fd).and_then(|s| s.as_ref()),
        Some(OpenFile::UnixSocket { .. })
    ) {
        return unix_connect(state, ctx, fd, addr, addrlen);
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    let a = match parse_sockaddr(ctx, addr, addrlen) {
        Some(x) => x,
        None => {
            state.set_ret_err(EAFNOSUPPORT);
            return;
        }
    };
    let r = ctx.net.connect(h, &a);
    ecv_debug!(
        ecv,
        "connect fd={} resuming={} -> {:?}",
        fd,
        ctx.resuming,
        r
    );
    match r {
        Ok(()) => state.set_ret(0),
        // The in-progress family. Every backend handle is non-blocking, so a
        // connect essentially never completes inside the call; reporting
        // ECONNREFUSED for this is why no guest could ever dial out. Measured
        // on loopback under WasmEdge: the first call returns INPROGRESS and the
        // retry after the socket becomes writable returns 0. All three errnos
        // are accepted because which one a host picks is not consistent.
        Err(e) if e.is_in_progress() => {
            if ctx.resuming {
                // Woken because the socket became writable, which is how a
                // connect completes; "still in progress" now means finished.
                state.set_ret(0)
            } else if ctx.is_nonblock(fd) {
                state.set_ret_err(EINPROGRESS)
            } else {
                socket_block(ctx, h, true)
            }
        }
        // A real error, including one that only surfaces on the retry after the
        // socket became writable -- which is exactly how a refused asynchronous
        // connect reports itself. Answering "success" to that would be worse
        // than the bug this function started with.
        Err(e) => state.set_ret_err(errno_of(e, Op::Connect)),
    }
}

#[inline(never)]
fn sys_bind(state: &mut State, ctx: &mut EcvContext) {
    let (fd, addr, addrlen) = (state.arg(0) as usize, state.arg(1), state.arg(2));
    if let Some(OpenFile::UnixSocket { listener }) = ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        let bound = listener.is_some();
        return unix_bind(state, ctx, fd, bound, addr, addrlen);
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    let a = match parse_sockaddr(ctx, addr, addrlen) {
        Some(x) => x,
        None => {
            state.set_ret_err(EAFNOSUPPORT);
            return;
        }
    };
    match ctx.net.bind(h, &a) {
        Ok(()) => state.set_ret(0),
        // Not EIO for everything. "Address already in use" is the one a server
        // operator actually meets -- a restart inside the previous socket's
        // TIME_WAIT window -- and nginx prints the errno it is given, so
        // collapsing it made that read as `bind() ... failed (5: I/O error)`.
        Err(e) => state.set_ret_err(match e.0 as u32 {
            WASI_ADDRINUSE => EADDRINUSE,
            WASI_ADDRNOTAVAIL => EADDRNOTAVAIL,
            WASI_ACCES => EACCES,
            _ => EIO,
        }),
    }
}

#[inline(never)]
fn sys_listen(state: &mut State, ctx: &mut EcvContext) {
    let (fd, backlog) = (state.arg(0) as usize, state.arg(1));
    if let Some(OpenFile::UnixSocket { listener }) = ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        // Linux: listen on an AF_UNIX socket that was never bound is EINVAL, not
        // an implicit autobind. Nothing here can invent a name it would agree on
        // with a future connect, so refusing is also the only honest answer.
        match *listener {
            Some(l) => {
                ctx.unix_listeners[l].listening = true;
                state.set_ret(0);
            }
            None => state.set_ret_err(EINVAL),
        }
        return;
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    match ctx.net.listen(h, backlog as u32) {
        Ok(()) => state.set_ret(0),
        Err(e) => state.set_ret_err(errno_of(e, Op::Other)),
    }
}

/// `bind` on an AF_UNIX socket: publishes the rendezvous name.
///
/// Two things are created, and both matter. The `UnixListener` entry is what a
/// `connect` searches; the VFS node is what everything ELSE in the guest sees --
/// `stat` (S_ISSOCK), `unlink` of a stale socket, and a `readdir` of the socket
/// directory. PostgreSQL exercises all three: it refuses to start if the path
/// exists and is not a socket, and unlinks its own on shutdown.
fn unix_bind(
    state: &mut State,
    ctx: &mut EcvContext,
    fd: usize,
    already_bound: bool,
    addr: u64,
    addrlen: u64,
) {
    if already_bound {
        state.set_ret_err(EINVAL);
        return;
    }
    let Some(name) = parse_sockaddr_un(ctx, addr, addrlen) else {
        state.set_ret_err(EINVAL);
        return;
    };
    let pathname = name[0] != 0;
    if pathname {
        // The kernel's check is "does anything exist at this path", not "is a
        // socket bound here": a plain file left by a previous run also makes
        // bind fail with EADDRINUSE, which is exactly the stale-socket case
        // postgres reports and tells the operator to remove.
        if ctx.vfs.resolve(&ctx.cwd, &name, false).is_some() {
            state.set_ret_err(EADDRINUSE);
            return;
        }
    } else if find_unix_listener(ctx, &name).is_some() {
        state.set_ret_err(EADDRINUSE);
        return;
    }
    let idx = ctx.unix_listeners.len();
    ctx.unix_listeners.push(UnixListener {
        path: name.clone(),
        listening: false,
        pending: VecDeque::new(),
        dead: false,
    });
    if pathname {
        ctx.vfs.upper_mut().mksock(&name);
    }
    if let Some(OpenFile::UnixSocket { listener }) = ctx.fds.get_mut(fd).and_then(|s| s.as_mut()) {
        *listener = Some(idx);
    }
    state.set_ret(0);
}

/// `connect` on an AF_UNIX socket.
///
/// This never blocks and never returns EINPROGRESS. The connection is completed
/// synchronously here -- both pipe pairs are created, the client keeps its end
/// and the server's is queued -- because there is no host object to wait on and
/// no third party to involve: the peer is another guest process in this same
/// module. A connect that "completes later" would need a wakeup that nothing
/// could deliver.
fn unix_connect(state: &mut State, ctx: &mut EcvContext, fd: usize, addr: u64, addrlen: u64) {
    let Some(name) = parse_sockaddr_un(ctx, addr, addrlen) else {
        state.set_ret_err(EINVAL);
        return;
    };
    // An already-CONNECTED fd cannot reach here -- connecting turns it into a
    // SocketPair, and that arm never dispatches to this function. What can reach
    // here is a fd that was bound: dialling from a bound socket is legal (the
    // bound name would be the peer's view of us, which nothing here reports, so
    // the binding is simply left alone), but dialling from a LISTENING one is
    // EISCONN.
    if let Some(OpenFile::UnixSocket { listener: Some(l) }) =
        ctx.fds.get(fd).and_then(|s| s.as_ref())
    {
        if ctx.unix_listeners[*l].listening {
            state.set_ret_err(EISCONN);
            return;
        }
    }
    let target = match find_unix_listener(ctx, &name) {
        Some(l) if ctx.unix_listeners[l].listening => l,
        // Distinguishing these two is not pedantry: a client's retry loop keys
        // on it. ENOENT means "the server has not started yet, wait and retry";
        // ECONNREFUSED means "the path is there but nobody is serving it", which
        // is what psql prints as `No such file or directory` vs `Connection
        // refused` -- the two most common postgres startup diagnostics.
        _ if name[0] != 0 && ctx.vfs.resolve(&ctx.cwd, &name, false).is_none() => {
            state.set_ret_err(ENOENT);
            return;
        }
        _ => {
            state.set_ret_err(ECONNREFUSED);
            return;
        }
    };
    // Exactly `socketpair`'s construction: a is client->server, b is
    // server->client, and each end holds one reader and one writer so the last
    // close of either end gives the peer EOF.
    let a = new_pipe(ctx, 1, 1);
    let b = new_pipe(ctx, 1, 1);
    ctx.unix_listeners[target].pending.push_back((a, b)); // server: rx=a, tx=b
    if let Some(slot) = ctx.fds.get_mut(fd) {
        *slot = Some(OpenFile::SocketPair { rx: b, tx: a });
    }
    ctx.wake_unix_acceptors(target);
    state.set_ret(0);
}

/// `accept` on a named AF_UNIX socket: claims a queued connection.
fn unix_accept(
    state: &mut State,
    ctx: &mut EcvContext,
    fd: usize,
    listener: Option<usize>,
    addr_ptr: u64,
    addrlen_ptr: u64,
    flags: u64,
) {
    let Some(l) = listener else {
        state.set_ret_err(EINVAL);
        return;
    };
    if !ctx.unix_listeners[l].listening {
        state.set_ret_err(EINVAL);
        return;
    }
    let Some((rx, tx)) = ctx.unix_listeners[l].pending.pop_front() else {
        if ctx.is_nonblock(fd) {
            state.set_ret_err(EAGAIN);
        } else {
            // Woken by the connecting process, not by the host -- see
            // BlockedOn::UnixAccept. The handler re-runs from its top on resume.
            ctx.block_current(BlockedOn::UnixAccept { listener: l });
            ctx.suspended = true; // svc unwinds
        }
        return;
    };
    // The peer is an unbound client, so its address is the 2-byte unnamed one --
    // the same thing Linux reports, and the reason a UDS server cannot identify
    // its client by address.
    write_sockaddr_un(ctx, addr_ptr, addrlen_ptr, b"");
    let newfd = ctx.alloc_fd(OpenFile::SocketPair { rx, tx });
    if flags & O_NONBLOCK != 0 {
        ctx.set_nonblock(newfd as usize, true);
    }
    ctx.set_cloexec(newfd as usize, flags & SOCK_CLOEXEC != 0);
    state.set_ret(newfd as u64);
}

/// accept / accept4.
///
/// The host socket is kept non-blocking internally whatever the guest asked
/// for, because that is the only way a would-block can be turned into a
/// suspension. What the guest asked for still decides what it SEES: a blocking
/// listener suspends until a connection arrives (like wait4: resuming-check +
/// retry in one function), a non-blocking one gets EAGAIN and is expected to go
/// back to its poll. Ignoring the distinction parks an event-driven server
/// inside `accept`, where it can neither run a timer nor service any other
/// descriptor -- see `socket_recv`.
///
/// `accept4`'s flags are its 4th argument; plain `accept` has none, so the
/// syscall number has to be consulted rather than the register.
#[inline(never)]
fn sys_accept(state: &mut State, ctx: &mut EcvContext) {
    // On resume svc has already stop_rewind'd; a resumed accept just re-attempts.
    let (fd, addr_ptr, addrlen_ptr) = (state.arg(0) as usize, state.arg(1), state.arg(2));
    let flags = if state.syscall_nr() == NR_ACCEPT4 {
        state.arg(3)
    } else {
        0
    };
    if let Some(OpenFile::UnixSocket { listener }) = ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        let listener = *listener;
        return unix_accept(state, ctx, fd, listener, addr_ptr, addrlen_ptr, flags);
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    let conn = match ctx.net.accept(h) {
        Ok(c) => c,
        Err(NetErr::AGAIN) => {
            if ctx.is_nonblock(fd) {
                state.set_ret_err(EAGAIN);
            } else {
                // No pending connection: suspend until the listener is readable.
                socket_block(ctx, h, false);
            }
            return;
        }
        Err(e) => {
            state.set_ret_err(errno_of(e, Op::Accept));
            return;
        }
    };
    if addr_ptr != 0 {
        store_addr(ctx, conn, addr_ptr, addrlen_ptr, true);
    }
    let newfd = ctx.alloc_fd(OpenFile::Socket { h: conn });
    // SOCK_NONBLOCK shares O_NONBLOCK's value. nginx accepts with it set
    // (traced: accept4(..., 0x800)) precisely so the following recv yields
    // EAGAIN instead of stalling its event loop.
    if flags & O_NONBLOCK != 0 {
        ctx.set_nonblock(newfd as usize, true);
    }
    if flags & SOCK_CLOEXEC != 0 {
        ctx.set_cloexec(newfd as usize, true);
    }
    state.set_ret(newfd as u64);
}

#[inline(never)]
fn sys_getsockname(state: &mut State, ctx: &mut EcvContext, peer: bool) {
    let (fd, addr_ptr, addrlen_ptr) = (state.arg(0) as usize, state.arg(1), state.arg(2));
    // AF_UNIX endpoints have no host object to interrogate. A named socket knows
    // its own name; every other in-guest endpoint (a connected UDS, a
    // socketpair) is unnamed on Linux too, and reports the 2-byte address.
    match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::UnixSocket { listener }) => {
            if peer {
                // This variant is never a connected endpoint -- connect turns
                // the fd into a SocketPair -- so getpeername on it is ENOTCONN,
                // which is what Linux answers and what a caller probing for a
                // peer identity needs to see.
                state.set_ret_err(ENOTCONN);
                return;
            }
            let name = listener.map_or_else(Vec::new, |l| ctx.unix_listeners[l].path.clone());
            write_sockaddr_un(ctx, addr_ptr, addrlen_ptr, &name);
            state.set_ret(0);
            return;
        }
        Some(OpenFile::SocketPair { .. }) => {
            write_sockaddr_un(ctx, addr_ptr, addrlen_ptr, b"");
            state.set_ret(0);
            return;
        }
        _ => {}
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    store_addr(ctx, h, addr_ptr, addrlen_ptr, peer);
    state.set_ret(0);
}

/// sendto: a NULL destination (i.e. send()) routes to the fd_write data path;
/// otherwise it bridges to sock_send_to with a single iovec.
#[inline(never)]
fn sys_sendto(state: &mut State, ctx: &mut EcvContext) {
    // On resume svc has already stop_rewind'd; a resumed send just re-attempts.
    let (fd, buf, len) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    let (dest, addrlen) = (state.arg(4), state.arg(5));
    // An in-guest endpoint has no host socket, so it cannot go through
    // sock_send_to -- but `send()` on one is an ordinary write to its tx pipe,
    // which is exactly what sys_write already does. A UDS is connected, so a
    // destination address is meaningless on it and is ignored, as the kernel
    // does for a connected socket.
    //
    // Missing this made a PostgreSQL backend fail its first `recv` on an
    // accepted connection with `could not receive data from client: Socket
    // operation on non-socket`. nginx never found it because the only thing it
    // does with a socketpair is sendmsg/recvmsg, which were handled.
    if matches!(
        ctx.fds.get(fd).and_then(|s| s.as_ref()),
        Some(OpenFile::SocketPair { .. })
    ) {
        state.gpr.x[0].val = fd as u64;
        state.gpr.x[1].val = buf;
        state.gpr.x[2].val = len as u64;
        return sys_write(state, ctx);
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    if dest == 0 {
        return socket_send(state, ctx, fd, h, buf, len);
    }
    let to = match parse_sockaddr(ctx, dest, addrlen) {
        Some(x) => x,
        None => {
            state.set_ret_err(EAFNOSUPPORT);
            return;
        }
    };
    let r = {
        let EcvContext { net, arena, .. } = &mut *ctx;
        net.send_to(h, arena.slice(buf, len), &to)
    };
    match r {
        Ok(n) => state.set_ret(n as u64),
        Err(e) => state.set_ret_err(errno_of(e, Op::Send)),
    }
}

/// recvfrom: a NULL source address (i.e. recv()) routes to the fd_read data
/// path; otherwise it bridges to sock_recv_from with a single iovec.
#[inline(never)]
fn sys_recvfrom(state: &mut State, ctx: &mut EcvContext) {
    // On resume svc has already stop_rewind'd; a resumed recv just re-attempts.
    let (fd, buf, len) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    let (src, addrlen_ptr) = (state.arg(4), state.arg(5));
    // See sys_sendto: an in-guest endpoint reads from its rx pipe, which is
    // what sys_read does -- including the blocking, the EAGAIN on a
    // non-blocking descriptor, and the EOF when the peer closes. A UDS peer is
    // unnamed, so a requested source address is the 2-byte unnamed one.
    if matches!(
        ctx.fds.get(fd).and_then(|s| s.as_ref()),
        Some(OpenFile::SocketPair { .. })
    ) {
        if src != 0 {
            write_sockaddr_un(ctx, src, addrlen_ptr, b"");
        }
        state.gpr.x[0].val = fd as u64;
        state.gpr.x[1].val = buf;
        state.gpr.x[2].val = len as u64;
        return sys_read(state, ctx);
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    if src == 0 {
        return socket_recv(state, ctx, fd, h, buf, len);
    }
    let r = {
        let EcvContext { net, arena, .. } = &mut *ctx;
        net.recv_from(h, arena.slice_mut(buf, len))
    };
    match r {
        Ok((n, from)) => {
            if let Some(a) = from {
                write_sockaddr(ctx, src, addrlen_ptr, &a);
            }
            state.set_ret(n as u64)
        }
        Err(e) => state.set_ret_err(errno_of(e, Op::Recv)),
    }
}

#[inline(never)]
fn sys_setsockopt(state: &mut State, ctx: &mut EcvContext) {
    let (fd, level, name) = (
        state.arg(0) as usize,
        state.arg(1) as i32,
        state.arg(2) as i32,
    );
    let (val, len) = (state.arg(3), state.arg(4) as u32);
    // No backend socket behind an in-guest endpoint. Accept and drop: a guest
    // that treats a failed setsockopt on its own listener as fatal must not be
    // stopped by an option that has nowhere to go.
    if matches!(
        ctx.fds.get(fd).and_then(|s| s.as_ref()),
        Some(OpenFile::UnixSocket { .. }) | Some(OpenFile::SocketPair { .. })
    ) {
        state.set_ret(0);
        return;
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    // Linux numbers cross the seam; translating to a backend's own numbering is
    // the backend's job. A backend that CAN express an option should not
    // inherit another's inability to (WasmEdge has no TCP level at all, so
    // TCP_NODELAY is inexpressible there and nowhere else).
    let r = {
        let EcvContext { net, arena, .. } = &mut *ctx;
        net.setsockopt(h, level, name, arena.slice(val, len as usize))
    };
    // Distinguish APPLIED from DROPPED in the log. Reporting success for an
    // option the backend cannot express would make this line agree with the bug
    // it exists to catch -- nginx's SO_REUSEADDR was silently inert for exactly
    // that reason, and an nginx restart inside TIME_WAIT then failed to bind.
    match r {
        Err(NetErr::NOTSUP) => ecv_debug!(
            ecv,
            "setsockopt fd={fd} level={level} name={name} len={len} -> DROPPED (backend has no equivalent)"
        ),
        _ => ecv_debug!(
            ecv,
            "setsockopt fd={fd} level={level} name={name} len={len} -> {r:?}"
        ),
    }
    // Always success to the guest, as this has always done: nginx treats a
    // failed setsockopt on its listeners as fatal, so turning "cannot express"
    // into an error would stop the guest booting rather than lose a hint.
    state.set_ret(0);
}

#[inline(never)]
fn sys_getsockopt(state: &mut State, ctx: &mut EcvContext) {
    let (fd, level, name) = (
        state.arg(0) as usize,
        state.arg(1) as i32,
        state.arg(2) as i32,
    );
    let (val, len_ptr) = (state.arg(3), state.arg(4));
    // An in-guest endpoint has no backend socket to query. Answering zero is
    // not a convenience: libpq calls `getsockopt(SO_ERROR)` on EVERY
    // connection, AF_UNIX included, and treats a failure as "could not get
    // socket error status" -- so ENOTSOCK here makes every UDS connection fail
    // after it has already succeeded.
    if matches!(
        ctx.fds.get(fd).and_then(|s| s.as_ref()),
        Some(OpenFile::UnixSocket { .. }) | Some(OpenFile::SocketPair { .. })
    ) {
        if val != 0 && len_ptr != 0 {
            let len = u32::from_le_bytes(ctx.arena.slice(len_ptr, 4).try_into().unwrap()) as usize;
            let n = len.min(4);
            ctx.arena.slice_mut(val, n).fill(0);
            ctx.arena
                .slice_mut(len_ptr, 4)
                .copy_from_slice(&(n as u32).to_le_bytes());
        }
        state.set_ret(0);
        return;
    }
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    if val != 0 && len_ptr != 0 {
        let cap = u32::from_le_bytes(ctx.arena.slice(len_ptr, 4).try_into().unwrap()) as usize;
        let n = {
            let EcvContext { net, arena, .. } = &mut *ctx;
            net.getsockopt(h, level, name, arena.slice_mut(val, cap))
                .unwrap_or(0)
        };
        ctx.arena
            .slice_mut(len_ptr, 4)
            .copy_from_slice(&(n as u32).to_le_bytes());
    }
    state.set_ret(0);
}

#[inline(never)]
fn sys_shutdown(state: &mut State, ctx: &mut EcvContext) {
    let (fd, how) = (state.arg(0) as usize, state.arg(1));
    let h = match socket_hostfd(ctx, fd) {
        Some(h) => h,
        None => {
            state.set_ret_err(ENOTSOCK);
            return;
        }
    };
    // Linux SHUT_RD / SHUT_WR / SHUT_RDWR = 0 / 1 / 2. Any backend-specific
    // encoding (WasmEdge wants bitflags) is the backend's business.
    let (read, write) = match how {
        0 => (true, false),
        1 => (false, true),
        _ => (true, true),
    };
    let _ = ctx.net.shutdown(h, read, write);
    state.set_ret(0);
}

// --- helpers -------------------------------------------------------------

fn set_io_result(state: &mut State, n: isize) {
    if n < 0 {
        state.set_ret_err(EBADF);
    } else {
        state.set_ret(n as u64);
    }
}

/// Makes a guest path absolute against cwd (no symlink resolution).
fn absolutize(cwd: &[u8], path: &[u8]) -> Vec<u8> {
    if path.first() == Some(&b'/') {
        return path.to_vec();
    }
    let mut out = cwd.to_vec();
    if out.last() != Some(&b'/') {
        out.push(b'/');
    }
    out.extend_from_slice(path);
    out
}

fn put_u32(b: &mut [u8], off: usize, v: u32) {
    b[off..off + 4].copy_from_slice(&v.to_le_bytes());
}
fn put_u64(b: &mut [u8], off: usize, v: u64) {
    b[off..off + 8].copy_from_slice(&v.to_le_bytes());
}

/// Fills a 128-byte aarch64 `struct stat`.
fn fill_stat(b: &mut [u8; 128], mode: u32, size: u64, mtime: u64, uid: u32, gid: u32) {
    put_u64(b, 0, 1); // st_dev
    put_u64(b, 8, 1); // st_ino
    put_u32(b, 16, mode);
    put_u32(b, 20, 1); // st_nlink
    put_u32(b, 24, uid);
    put_u32(b, 28, gid);
    put_u64(b, 48, size);
    put_u32(b, 56, 4096); // st_blksize
    put_u64(b, 64, (size + 511) / 512); // st_blocks
    put_u64(b, 88, mtime); // st_mtime
    put_u64(b, 104, mtime); // st_ctime
}

fn dirent_reclen(name_len: usize) -> usize {
    // linux_dirent64: u64 ino, i64 off, u16 reclen, u8 type, name + NUL,
    // padded to 8.
    let base = 8 + 8 + 2 + 1 + name_len + 1;
    (base + 7) & !7
}

fn encode_dirent64(out: &mut Vec<u8>, ino: u64, kind: NodeKind, name: &[u8]) {
    let reclen = dirent_reclen(name.len());
    let d_type: u8 = match kind {
        NodeKind::Dir => 4,      // DT_DIR
        NodeKind::Symlink => 10, // DT_LNK
        NodeKind::File => 8,     // DT_REG
        NodeKind::Socket => 12,  // DT_SOCK
    };
    let start = out.len();
    out.resize(start + reclen, 0);
    let rec = &mut out[start..start + reclen];
    put_u64(rec, 0, ino);
    put_u64(rec, 8, (start as u64) + reclen as u64); // d_off (next)
    rec[16..18].copy_from_slice(&(reclen as u16).to_le_bytes());
    rec[18] = d_type;
    rec[19..19 + name.len()].copy_from_slice(name);
}

/// readv/writev over guest iovecs ({u64 base, u64 len}); stdio-focused.
fn iovec_loop(state: &mut State, ctx: &mut EcvContext, is_write: bool) {
    let (fd, vec_vma, vlen) = (state.arg(0) as usize, state.arg(1), state.arg(2) as usize);
    let is_stdio = matches!(
        ctx.fds.get(fd).and_then(|s| s.as_ref()),
        Some(OpenFile::Stdio(_))
    );
    let hostfd = match ctx.fds.get(fd).and_then(|s| s.as_ref()) {
        Some(OpenFile::Stdio(h)) => *h,
        _ => -1,
    };
    if !is_stdio {
        // Route non-stdio vectored I/O through the scalar path per-iov.
        let mut total = 0u64;
        for i in 0..vlen {
            let iov = ctx.arena.slice(vec_vma + (i * 16) as u64, 16);
            let base = u64::from_le_bytes(iov[..8].try_into().unwrap());
            let len = u64::from_le_bytes(iov[8..].try_into().unwrap());
            state.gpr.x[0].val = fd as u64;
            state.gpr.x[1].val = base;
            state.gpr.x[2].val = len;
            if is_write {
                sys_write(state, ctx);
            } else {
                sys_read(state, ctx);
            }
            let n = state.gpr.x[0].val as i64;
            if n < 0 {
                if total == 0 {
                    return;
                }
                break;
            }
            total += n as u64;
            if (n as u64) < len {
                break;
            }
        }
        state.set_ret(total);
        return;
    }
    let mut total: u64 = 0;
    for i in 0..vlen {
        let iov = ctx.arena.slice(vec_vma + (i * 16) as u64, 16);
        let base = u64::from_le_bytes(iov[..8].try_into().unwrap());
        let len = u64::from_le_bytes(iov[8..].try_into().unwrap()) as usize;
        if len == 0 {
            continue;
        }
        let p = ctx.arena.translate(base);
        let n = unsafe {
            if is_write {
                write(hostfd, p, len)
            } else {
                read(hostfd, p, len)
            }
        };
        if n < 0 {
            if total == 0 {
                set_io_result(state, n);
                return;
            }
            break;
        }
        total += n as u64;
        if (n as usize) < len {
            break;
        }
    }
    state.set_ret(total);
}
