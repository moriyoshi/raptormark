//! The network backend seam.
//!
//! Every socket syscall used to call a host import directly and unsafely, at
//! the syscall site: `sock_open` inside `sys_socket`, `fd_read` inside
//! `socket_recv`, and so on across about twenty places in `sys.rs`. That made
//! the module WasmEdge-bound whatever the guest did -- even
//! `int main(void){return 0;}` linked all eleven of WasmEdge's non-standard
//! socket imports, because they come from ecvisor being linked in whole rather
//! than from what the guest reaches.
//!
//! This module is the seam those calls now go through.
//!
//! # Why a cfg-selected type alias, and not `dyn`
//!
//! ⚠️ **`dyn NetBackend` would defeat the entire point.** wasm-ld emits an
//! import for every undefined function symbol reachable from live code. A trait
//! object's vtable takes the address of every method of every impl, so both
//! backends stay live, so BOTH import sets end up in the module -- and a
//! browser host would have to stub eleven WasmEdge functions while a WasmEdge
//! host stubbed the browser's. `--gc-sections` cannot help: it cannot prove a
//! vtable slot is never called.
//!
//! An `enum Net { Host(..), Browser(..) }` with a `match` in each method has
//! exactly the same problem, for the same reason: both arms are reachable code.
//! **Any runtime selection defeats the goal.** Only compile-time exclusion
//! works, which is what `type Net` below does.
//!
//! So the trait is a CONFORMANCE CONTRACT -- three implementations checked
//! against one set of signatures, and the vehicle for backend-generic tests --
//! rather than a polymorphism mechanism. That distinction is the whole design.

pub mod addr;
pub mod dns;
// The WASIX address codec. Uncfg'd, like `addr`, so `cargo test` reaches it
// without wasm -- and it needs that more than `addr` does, because the encoding
// is asymmetric and a wrong codec binds somewhere plausible rather than failing.
pub mod wasix_addr;
// The `poll_oneoff` buffer layout, shared by the two backends that build one by
// hand. Uncfg'd for the same reason: both of them are wasm32-only, so until
// this module the offsets had no host test at all.
pub mod poll1;

// Exactly one backend module is COMPILED, not merely selected. An unused
// backend costs no imports -- `LoopbackNet` calls nothing -- but it is still
// dead code in the shipping artifact, and leaving it in blurs the property this
// module exists to guarantee. Measured before gating: the wasmedge staticlib
// carried 27 `net::loopback` symbols it could never reach.
//
// ❗ `net-loopback` IS A LABEL, NOT A `cfg`. Loopback is selected by the
// ABSENCE of every other backend, so the two `any(...)` lists below are what
// exclude it -- and a new backend that is added to one of them and not the
// other compiles loopback in ALONGSIDE itself. `profile_exclusion_test` is what
// notices, which is the good case; the bad case is noticing at deploy time.
#[cfg(all(target_arch = "wasm32", feature = "net-browser"))]
pub mod browser;
#[cfg(not(all(
    target_arch = "wasm32",
    any(
        feature = "net-wasmedge",
        feature = "net-browser",
        feature = "net-wasix"
    )
)))]
pub mod loopback;
#[cfg(all(target_arch = "wasm32", feature = "net-wasix"))]
pub mod wasix;
#[cfg(all(target_arch = "wasm32", feature = "net-wasmedge"))]
pub mod wasmedge;

/// The backend this build talks to. Exactly one, chosen at compile time.
#[cfg(all(target_arch = "wasm32", feature = "net-wasmedge"))]
pub type Net = wasmedge::HostWasiNet;
#[cfg(all(target_arch = "wasm32", feature = "net-browser"))]
pub type Net = browser::BrowserNet;
#[cfg(all(target_arch = "wasm32", feature = "net-wasix"))]
pub type Net = wasix::WasixNet;
#[cfg(all(
    target_arch = "wasm32",
    not(any(
        feature = "net-wasmedge",
        feature = "net-browser",
        feature = "net-wasix"
    ))
))]
pub type Net = loopback::LoopbackNet;
/// The host (`cargo test`) build has no imports to call, so it always gets the
/// pure-Rust backend. This is what makes the socket layer testable at all.
#[cfg(not(target_arch = "wasm32"))]
pub type Net = loopback::LoopbackNet;

/// A backend-owned socket handle.
///
/// ⚠️ It must stay a plain `Copy` scalar, and the runtime must never allocate
/// or free one -- it only counts references. Three existing behaviours force
/// this, and a handle carrying per-socket state would break all three:
///
/// * `close_fd_full` refcounts a socket by SCANNING every process's fd table
///   for an equal handle, so handles have to compare by value.
/// * `fork` copies the fd table by value, so two processes legitimately hold
///   the same handle.
/// * `BlockedOn::Socket` is `Copy` and survives `save_current`/`load_current`.
///
/// The newtype earns its keep immediately: it makes the libc `close(2)` that
/// `close_fd_full` used to perform on a socket a type error. That only ever
/// worked because a WasmEdge socket handle happens to BE a WASI fd, which is
/// not true of any other backend.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct NetHandle(pub i32);

/// A WASI preview1 errno, which is the numbering every backend speaks.
///
/// Not a third invention: `HostWasiNet` already receives these, and a browser
/// host gets a published table. The Linux translation then lives in exactly one
/// place (`errno_of`), which is what stops the mapping drifting per call site.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct NetErr(pub u16);

impl NetErr {
    pub const ACCES: NetErr = NetErr(2);
    pub const ADDRINUSE: NetErr = NetErr(3);
    pub const ADDRNOTAVAIL: NetErr = NetErr(4);
    pub const AGAIN: NetErr = NetErr(6);
    pub const ALREADY: NetErr = NetErr(7);
    pub const BADF: NetErr = NetErr(8);
    pub const CONNABORTED: NetErr = NetErr(13);
    pub const CONNREFUSED: NetErr = NetErr(14);
    pub const CONNRESET: NetErr = NetErr(15);
    pub const INPROGRESS: NetErr = NetErr(26);
    pub const INVAL: NetErr = NetErr(28);
    pub const IO: NetErr = NetErr(29);
    pub const INTR: NetErr = NetErr(27);
    pub const NOTCONN: NetErr = NetErr(53);
    pub const NOTSUP: NetErr = NetErr(58);
    pub const PIPE: NetErr = NetErr(64);

    /// True for the states a connect may legitimately be left in, and which the
    /// caller must resume from rather than fail. Collapsing these into a
    /// generic error is what once made every outbound dial impossible.
    pub fn is_in_progress(self) -> bool {
        self == NetErr::AGAIN || self == NetErr::INPROGRESS || self == NetErr::ALREADY
    }
}

/// An IPv4/IPv6 endpoint in the form the syscall layer works in: network-order
/// octets plus a host-order port.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct SockAddr {
    pub octets: [u8; 16],
    pub v6: bool,
    pub port: u16,
}

impl SockAddr {
    pub fn v4(octets: [u8; 4], port: u16) -> SockAddr {
        let mut o = [0u8; 16];
        o[..4].copy_from_slice(&octets);
        SockAddr {
            octets: o,
            v6: false,
            port,
        }
    }

    /// The meaningful prefix of `octets`: 4 for IPv4, 16 for IPv6.
    pub fn bytes(&self) -> &[u8] {
        &self.octets[..if self.v6 { 16 } else { 4 }]
    }
}

/// Non-blocking readiness for one handle.
#[derive(Clone, Copy, Default, PartialEq, Eq, Debug)]
pub struct Readiness {
    pub read: bool,
    pub write: bool,
}

/// The result of the scheduler's idle wait.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum WaitOutcome {
    /// Indices into the waiter list that are now ready.
    Ready(Vec<usize>),
    /// The deadline expired with nothing ready.
    TimedOut,
    /// The backend cannot block. Only a non-blocking host returns this, and it
    /// is what a re-entrant driver needs in order to hand control back to its
    /// event loop instead of sleeping.
    WouldBlock,
}

/// Which operation an errno came from. The Linux number for one WASI errno is
/// not the same in every context, so the translation is operation-sensitive.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Op {
    Connect,
    Send,
    Recv,
    Accept,
    Other,
}

/// Translates a backend errno into the Linux errno a guest expects.
///
/// ⚠️ Do not collapse this into one value. Two failures are on record from
/// having done so:
///
/// * `connect` reporting ECONNREFUSED for the in-progress family meant no guest
///   could ever dial out, because every host socket is kept non-blocking and a
///   connect essentially never completes inside the call.
/// * `send` reporting EIO for `__WASI_ERRNO_PIPE` made nginx log
///   `sendfile() failed (5: I/O error)` for an ordinary client disconnect,
///   which it would otherwise have closed quietly.
pub fn errno_of(e: NetErr, op: Op) -> u64 {
    // Linux aarch64 errnos, duplicated here rather than imported from `sys`
    // because this module is not gated to wasm32 and `sys` is.
    const EACCES: u64 = 13;
    const EADDRINUSE: u64 = 98;
    const EADDRNOTAVAIL: u64 = 99;
    const EAGAIN: u64 = 11;
    const ECONNABORTED: u64 = 103;
    const ECONNREFUSED: u64 = 111;
    const ECONNRESET: u64 = 104;
    const EINPROGRESS: u64 = 115;
    const EINTR: u64 = 4;
    const EINVAL: u64 = 22;
    const EIO: u64 = 5;
    const ENOTCONN: u64 = 107;
    const EPIPE: u64 = 32;

    match (op, e) {
        (Op::Connect, x) if x.is_in_progress() => EINPROGRESS,
        (Op::Connect, _) => ECONNREFUSED,
        (_, NetErr::AGAIN) => EAGAIN,
        (Op::Send, NetErr::PIPE) => EPIPE,
        (Op::Send, NetErr::CONNRESET) => ECONNRESET,
        (Op::Send, NetErr::CONNABORTED) => ECONNABORTED,
        (Op::Send, NetErr::NOTCONN) => ENOTCONN,
        (Op::Send, NetErr::INTR) => EINTR,
        (_, NetErr::INVAL) => EINVAL,
        // ⚠️ THE BIND/LISTEN FAMILY MUST NOT COLLAPSE INTO EIO. These say
        // something the guest and its operator can act on -- the port is taken,
        // the address is not ours, we may not have it -- and `EIO` says only
        // that the kernel is unwell.
        //
        // This is not hypothetical. A double `listen` in the Node backend
        // returned ADDRINUSE, arrived at nginx as `(5: I/O error)`, and the
        // resulting investigation filed a WRONG root cause before instrumenting
        // the ABI. The rule in CLAUDE.md about not collapsing host socket errors
        // was written for resumable states; it applies just as much to the ones
        // that name a cause.
        (_, NetErr::ADDRINUSE) => EADDRINUSE,
        (_, NetErr::ADDRNOTAVAIL) => EADDRNOTAVAIL,
        (_, NetErr::ACCES) => EACCES,
        _ => EIO,
    }
}

/// What the syscall layer needs from a network transport.
///
/// Two invariants every implementation must uphold:
///
/// * **Handles are always non-blocking.** A would-block is reported as
///   `NetErr::AGAIN`; whether the GUEST sees `EAGAIN` or is suspended is
///   decided by the guest's own descriptor flag, above this layer.
/// * **`wait` is the only method permitted to block.** Everything else must
///   return promptly, because `ready` is called once per socket per epoll scan.
pub trait NetBackend {
    /// A counter bumped whenever the host reports readiness out of band.
    ///
    /// ⚠️ ONLY A RE-ENTRANT HOST NEEDS THIS, and the default of 0 is what keeps
    /// every blocking backend behaving exactly as before. A backend that BLOCKS
    /// in `wait` re-probes the host on every call, so it always sees current
    /// readiness. One that cannot block answers from a cache the host refreshes
    /// between slices, and the scheduler has no other way to tell "nothing has
    /// happened" from "something happened to a handle nobody is waiting on".
    fn ready_generation(&self) -> u64 {
        0
    }

    fn socket(&mut self, v6: bool, dgram: bool) -> Result<NetHandle, NetErr>;
    fn bind(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr>;
    fn listen(&mut self, h: NetHandle, backlog: u32) -> Result<(), NetErr>;
    fn connect(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr>;
    fn accept(&mut self, h: NetHandle) -> Result<NetHandle, NetErr>;
    fn recv(&mut self, h: NetHandle, buf: &mut [u8]) -> Result<usize, NetErr>;
    fn send(&mut self, h: NetHandle, buf: &[u8]) -> Result<usize, NetErr>;
    fn recv_from(
        &mut self,
        h: NetHandle,
        buf: &mut [u8],
    ) -> Result<(usize, Option<SockAddr>), NetErr>;
    fn send_to(&mut self, h: NetHandle, buf: &[u8], a: &SockAddr) -> Result<usize, NetErr>;
    fn addr(&mut self, h: NetHandle, peer: bool) -> Result<SockAddr, NetErr>;
    /// `level`/`name` are LINUX numbers. Translating to a backend's own
    /// numbering is the backend's job -- WasmEdge needs it, and a backend that
    /// does not should not inherit the limitation. (WasmEdge has no TCP level
    /// at all, so `TCP_NODELAY` is inexpressible there and nowhere else.)
    fn setsockopt(&mut self, h: NetHandle, level: i32, name: i32, val: &[u8])
        -> Result<(), NetErr>;
    fn getsockopt(
        &mut self,
        h: NetHandle,
        level: i32,
        name: i32,
        out: &mut [u8],
    ) -> Result<usize, NetErr>;
    fn shutdown(&mut self, h: NetHandle, read: bool, write: bool) -> Result<(), NetErr>;
    fn close(&mut self, h: NetHandle);
    /// Non-blocking readiness. MUST be cheap: `fd_ready` calls it once per
    /// socket per `epoll_pwait`/`ppoll` scan.
    fn ready(&mut self, h: NetHandle, want_write: bool) -> Readiness;
    /// The scheduler's idle wait, bounded by `deadline_nanos` when one is
    /// pending. The ONLY method allowed to block.
    fn wait(&mut self, waiters: &[(NetHandle, bool)], timeout_nanos: Option<u128>) -> WaitOutcome;
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The bind/listen family must reach the guest as itself.
    ///
    /// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? The catch-all
    /// arm is `EIO`, so a mapping that silently lost these would answer 5 for
    /// all three -- which is what it did. It cost a real investigation: a double
    /// `listen` in the Node backend returned ADDRINUSE, nginx printed
    /// `(5: I/O error)`, and the first root cause filed against it was wrong.
    #[test]
    fn the_bind_family_does_not_collapse_into_eio() {
        const EIO: u64 = 5;
        // `bind`/`listen` carry `Op::Other`; there is no variant per syscall.
        for op in [Op::Other, Op::Accept, Op::Recv] {
            assert_eq!(errno_of(NetErr::ADDRINUSE, op), 98, "{op:?} ADDRINUSE");
            assert_eq!(
                errno_of(NetErr::ADDRNOTAVAIL, op),
                99,
                "{op:?} ADDRNOTAVAIL"
            );
            assert_eq!(errno_of(NetErr::ACCES, op), 13, "{op:?} ACCES");
            assert_ne!(errno_of(NetErr::ADDRINUSE, op), EIO, "{op:?}");
        }
        // The catch-all still exists for genuinely unclassified errors, and the
        // resumable states are untouched -- both are what this must not break.
        assert_eq!(errno_of(NetErr::IO, Op::Recv), EIO);
        assert_eq!(errno_of(NetErr::AGAIN, Op::Recv), 11);
        assert_eq!(errno_of(NetErr::INPROGRESS, Op::Connect), 115);
    }
}
