//! The WASIX socket ABI, and everything that is specific to wasmer.
//!
//! The `--profile wasix` backend. It is the first profile other than the
//! default with real external egress: `loopback` is in-process only and
//! `browser` needs a relay server that does not exist, so a guest that has to
//! reach the network has had exactly one host until now.
//!
//! Its shape follows `net::wasmedge` deliberately -- the same seam, the same
//! two-block import layout, the same "keep every handle non-blocking" rule --
//! because the differences that matter are then the ones you can see.
//!
//! ⚠️ **THE SPECIFICATION IS `.agents/docs/WASIX_ABI.md`, WHICH WAS MEASURED.**
//! Four things in this file are not what reading wasmer's syscall sources would
//! suggest, and every one of them fails silently rather than loudly:
//!
//! 1. **`wasix_32v1.sock_accept` IS `sock_accept_v2`.** wasmer binds the name to
//!    a different function in each namespace, so it takes FOUR parameters here
//!    and returns the peer address.
//! 2. **A `poll_oneoff` clock timeout of 0 means WAIT FOREVER**, not "return
//!    now". `net::wasmedge::ready` relies on the preview1 meaning; copying it
//!    hangs the guest on its first `epoll_pwait`. `PROBE_NANOS` below is 1.
//! 3. **The address port is little-endian going out and big-endian coming
//!    back** -- see `net::wasix_addr`, which is where that lives so it can be
//!    tested on the host.
//! 4. **`sock_get_opt_size(LastError)` answers in WASI numbering**, so
//!    `SO_ERROR` has to be translated before a guest sees it.
//!
//! # There is no DNS interception here, and that is not an omission
//!
//! `net::dns` exists because a browser has no UDP at all, so a guest's resolver
//! is the first thing that breaks there. WASIX has real datagram sockets: the
//! guest's own resolver sends to port 53 and gets an answer, with
//! `/etc/nsswitch.conf`, the `search` domains and both libcs' very different
//! paths all working unmodified. WASIX also offers a `resolve` call, which this
//! backend deliberately does not import -- nothing needs it, and an unused
//! import is a demand on the host for no gain.
//!
//! ⚠️ It does need `wasmer run --net`. Without it `sock_open` SUCCEEDS and
//! `sock_bind` returns errno 58; a run reports nothing wrong until the first
//! bind. `internal/pipeline.runtimeArgs` passes it.

use super::{poll1, wasix_addr, NetBackend, NetErr, NetHandle, Readiness, SockAddr, WaitOutcome};

/// A WASI `iovec`/`ciovec` (`{ buf: u32, len: u32 }` on wasm32).
///
/// On wasm32 a Rust pointer's integer value IS its linear-memory offset, which
/// is what the host reads, so pointers into ecvisor's own stack cross the
/// boundary with no translation.
#[repr(C)]
struct IoVec {
    buf: *const u8,
    len: u32,
}

/// `Addressfamily`. The families live in `wasix_addr` with the codec; these are
/// the two `sock_open` needs.
const WX_AF_INET4: u32 = 1;
const WX_AF_INET6: u32 = 2;

/// `Socktype`. ⚠️ NOT WasmEdge's numbering, which has `Dgram=1 Stream=2`. The
/// two are swapped, so a value copied across silently opens the wrong kind of
/// socket -- and a stream socket that behaves like a datagram one fails much
/// later than it was created.
const WX_SOCK_STREAM: u32 = 1;
const WX_SOCK_DGRAM: u32 = 2;

/// `SockProto::Ip`. `sock_open` rejects `Tcp` with a non-stream type and `Udp`
/// with a non-datagram one, so 0 is the argument that is always right.
const WX_PROTO_DEFAULT: u32 = 0;

/// `Fdflags::NONBLOCK`, the preview1 value.
const FD_NONBLOCK: u32 = 4;

/// The `poll_oneoff` timeout that means "answer now".
///
/// ❗ ONE NANOSECOND, NOT ZERO. WASIX reads a zero timeout as `Duration::MAX`
/// and does not even record the subscription, so a zero-timeout probe never
/// returns. Measured by having to kill it; `.agents/docs/WASIX_ABI.md` has the
/// run. This is the single most dangerous difference from `net::wasmedge`,
/// because the failure is a hang with no error, no bad import and nothing to
/// grep for.
const PROBE_NANOS: u64 = 1;

/// `Sockoption`, `#[repr(u8)]`, in declaration order.
const OPT_REUSE_PORT: u32 = 1;
const OPT_REUSE_ADDR: u32 = 2;
const OPT_NO_DELAY: u32 = 3;
const OPT_DONT_ROUTE: u32 = 4;
const OPT_ONLY_V6: u32 = 5;
const OPT_BROADCAST: u32 = 6;
const OPT_LAST_ERROR: u32 = 11;
const OPT_KEEP_ALIVE: u32 = 12;
const OPT_RECV_BUF_SIZE: u32 = 15;
const OPT_SEND_BUF_SIZE: u32 = 16;
const OPT_TTL: u32 = 23;

/// Linux `(level, optname)` pairs, aarch64. Same list as `net::wasmedge`'s plus
/// the ones WasmEdge cannot express.
const SOL_SOCKET: i32 = 1;
const SO_REUSEADDR: i32 = 2;
const SO_ERROR: i32 = 4;
const SO_DONTROUTE: i32 = 5;
const SO_BROADCAST: i32 = 6;
const SO_SNDBUF: i32 = 7;
const SO_RCVBUF: i32 = 8;
const SO_KEEPALIVE: i32 = 9;
const SO_REUSEPORT: i32 = 15;
const IPPROTO_IP: i32 = 0;
const IP_TTL: i32 = 2;
const IPPROTO_TCP: i32 = 6;
const TCP_NODELAY: i32 = 1;
const IPPROTO_IPV6: i32 = 41;
const IPV6_V6ONLY: i32 = 26;

#[link(wasm_import_module = "wasix_32v1")]
extern "C" {
    fn sock_open(af: u32, ty: u32, proto: u32, ro_sock: *mut u32) -> u32;
    fn sock_bind(sock: u32, addr: *const u8) -> u32;
    fn sock_listen(sock: u32, backlog: u32) -> u32;
    fn sock_connect(sock: u32, addr: *const u8) -> u32;
    /// ❗ FOUR parameters: in `wasix_32v1` this name is bound to
    /// `sock_accept_v2`, not to the three-parameter preview1 `sock_accept`.
    fn sock_accept(sock: u32, fd_flags: u32, ro_fd: *mut u32, ro_addr: *mut u8) -> u32;
    fn sock_recv(
        sock: u32,
        ri_data: *const IoVec,
        ri_data_len: u32,
        ri_flags: u16,
        ro_data_len: *mut u32,
        ro_flags: *mut u16,
    ) -> u32;
    fn sock_send(
        sock: u32,
        si_data: *const IoVec,
        si_data_len: u32,
        si_flags: u16,
        ret_data_len: *mut u32,
    ) -> u32;
    fn sock_recv_from(
        sock: u32,
        ri_data: *const IoVec,
        ri_data_len: u32,
        ri_flags: u16,
        ro_data_len: *mut u32,
        ro_flags: *mut u16,
        ro_addr: *mut u8,
    ) -> u32;
    fn sock_send_to(
        sock: u32,
        si_data: *const IoVec,
        si_data_len: u32,
        si_flags: u16,
        addr: *const u8,
        ret_data_len: *mut u32,
    ) -> u32;
    fn sock_addr_local(sock: u32, ret_addr: *mut u8) -> u32;
    fn sock_addr_peer(sock: u32, ro_addr: *mut u8) -> u32;
    fn sock_shutdown(sock: u32, how: u32) -> u32;
    fn sock_set_opt_flag(sock: u32, opt: u32, flag: u32) -> u32;
    fn sock_get_opt_flag(sock: u32, opt: u32, ret_flag: *mut u8) -> u32;
    /// ⚠️ The ONLY one of these taking an i64. `Filesize` is a u64, so the size
    /// is a value rather than a pointer and the wasm type is `(i32, i32, i64)`.
    fn sock_set_opt_size(sock: u32, opt: u32, size: u64) -> u32;
    fn sock_get_opt_size(sock: u32, opt: u32, ret_size: *mut u64) -> u32;
}

// Standard preview1. ⚠️ Taken from `wasi_snapshot_preview1` on purpose, not
// from `wasix_32v1` which also exports all three: it is where the rest of
// ecvisor already gets them, so this adds no import, and a WASIX socket fd is
// an ordinary WASI fd -- measured, `poll_oneoff` imported from preview1 does
// observe a `wasix_32v1` socket.
#[link(wasm_import_module = "wasi_snapshot_preview1")]
extern "C" {
    fn fd_fdstat_set_flags(fd: u32, flags: u32) -> u32;
    fn fd_close(fd: u32) -> u32;
    fn poll_oneoff(in_subs: *const u8, out_events: *mut u8, nsubs: u32, nevents: *mut u32) -> u32;
}

fn err(e: u32) -> NetErr {
    NetErr(e as u16)
}

/// Where a Linux socket option has to go, if anywhere.
///
/// ⚠️ WASIX splits `setsockopt` across three calls and the split is FIXED per
/// option, not a matter of payload width: `sock_set_opt_size` rejects anything
/// outside its own four names with `EINVAL` before it looks at the value. One
/// table cannot express that, which is why this is an enum rather than a
/// `(level, name)` pair like `net::wasmedge::we_sockopt`.
enum Opt {
    /// A boolean, through `sock_{set,get}_opt_flag`.
    Flag(u32),
    /// An integer, through `sock_{set,get}_opt_size`.
    Size(u32),
}

/// Translates a Linux `(level, optname)` into the WASIX option and the call
/// that carries it, or `None` when there is no equivalent.
///
/// ✅ **`TCP_NODELAY` IS EXPRESSIBLE HERE.** WasmEdge has no TCP level at all,
/// so `net/wasmedge.rs` cannot send it and nginx's `tcp_nodelay on` is inert
/// under the shipping profile. That is a WasmEdge limitation and it stops
/// there; a backend that can express an option must not inherit another's
/// inability to. Same for `SO_REUSEPORT` and `IPV6_V6ONLY`.
fn wx_sockopt(level: i32, name: i32) -> Option<Opt> {
    match (level, name) {
        (SOL_SOCKET, SO_REUSEADDR) => Some(Opt::Flag(OPT_REUSE_ADDR)),
        (SOL_SOCKET, SO_REUSEPORT) => Some(Opt::Flag(OPT_REUSE_PORT)),
        (SOL_SOCKET, SO_DONTROUTE) => Some(Opt::Flag(OPT_DONT_ROUTE)),
        (SOL_SOCKET, SO_BROADCAST) => Some(Opt::Flag(OPT_BROADCAST)),
        (SOL_SOCKET, SO_KEEPALIVE) => Some(Opt::Flag(OPT_KEEP_ALIVE)),
        (IPPROTO_TCP, TCP_NODELAY) => Some(Opt::Flag(OPT_NO_DELAY)),
        (IPPROTO_IPV6, IPV6_V6ONLY) => Some(Opt::Flag(OPT_ONLY_V6)),
        (SOL_SOCKET, SO_SNDBUF) => Some(Opt::Size(OPT_SEND_BUF_SIZE)),
        (SOL_SOCKET, SO_RCVBUF) => Some(Opt::Size(OPT_RECV_BUF_SIZE)),
        (IPPROTO_IP, IP_TTL) => Some(Opt::Size(OPT_TTL)),
        // SO_ERROR is READ-ONLY and lives on `LastError`, so it is not here:
        // `getsockopt` handles it directly, because its value needs translating
        // out of WASI numbering and no setter should ever reach it.
        _ => None,
    }
}

/// The first 4 bytes of a guest's option payload, as the `int` it almost always
/// is. A short buffer reads as zero rather than reading past what the guest
/// supplied.
fn opt_int(val: &[u8]) -> i32 {
    let mut b = [0u8; 4];
    let n = val.len().min(4);
    b[..n].copy_from_slice(&val[..n]);
    i32::from_le_bytes(b)
}

/// The WASIX-backed network. Stateless: every handle is a host descriptor, and
/// the trait's invariant that a handle is a plain `Copy` scalar is upheld for
/// free.
#[derive(Default)]
pub struct WasixNet;

impl WasixNet {
    pub fn new() -> WasixNet {
        WasixNet
    }

    /// Every host socket is kept non-blocking, so a would-block suspends the
    /// guest process cooperatively instead of parking the whole instance. The
    /// guest's own `O_NONBLOCK` is a separate, higher-level concept decided
    /// above this layer.
    ///
    /// Measured that this is the mechanism rather than assumed: with the flag,
    /// `sock_accept` on an empty listener answers errno 6; without it, the
    /// same probe has to be killed. The socket paths read the fd entry's flags
    /// (`sock_accept.rs:129`, `sock_send.rs:102`, `sock_connect.rs:72`).
    fn set_nonblocking(fd: u32) {
        unsafe { fd_fdstat_set_flags(fd, FD_NONBLOCK) };
    }
}

impl NetBackend for WasixNet {
    fn socket(&mut self, v6: bool, dgram: bool) -> Result<NetHandle, NetErr> {
        let family = if v6 { WX_AF_INET6 } else { WX_AF_INET4 };
        let ty = if dgram { WX_SOCK_DGRAM } else { WX_SOCK_STREAM };
        let mut fd: u32 = 0;
        let r = unsafe { sock_open(family, ty, WX_PROTO_DEFAULT, &mut fd) };
        if r != 0 {
            return Err(err(r));
        }
        WasixNet::set_nonblocking(fd);
        Ok(NetHandle(fd as i32))
    }

    fn bind(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        let buf = wasix_addr::encode(a);
        match unsafe { sock_bind(h.0 as u32, buf.as_ptr()) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn listen(&mut self, h: NetHandle, backlog: u32) -> Result<(), NetErr> {
        match unsafe { sock_listen(h.0 as u32, backlog) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn connect(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        let buf = wasix_addr::encode(a);
        match unsafe { sock_connect(h.0 as u32, buf.as_ptr()) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn accept(&mut self, h: NetHandle) -> Result<NetHandle, NetErr> {
        let mut fd: u32 = 0;
        // ❗ A REAL BUFFER, NOT A NULL POINTER. This call writes 20 bytes of
        // peer address whether or not the caller wants them, and on wasm32 a
        // null pointer is linear-memory offset 0 -- which is memory the guest
        // owns. The trait's `accept` returns only a handle, so the address is
        // dropped here; `sys_accept` asks for it separately through `addr`.
        let mut peer = [0u8; wasix_addr::ADDR_PORT_LEN];
        // ⚠️ `fd_flags` is 0 rather than NONBLOCK, and the accepted socket is
        // still non-blocking: wasmer copies the LISTENER's flag onto the new fd
        // entry itself (`sock_accept.rs:129` and `:157`), and `socket` above
        // sets it on every listener this backend creates.
        //
        // `set_nonblocking` below is therefore BELT AND BRACES rather than the
        // mechanism, and it is kept for the case the propagation does not
        // cover: a handle that reached us some other way. The invariant this
        // backend owes `NetBackend` -- every handle is non-blocking -- should
        // not rest on another runtime's copying behaviour.
        let r = unsafe { sock_accept(h.0 as u32, 0, &mut fd, peer.as_mut_ptr()) };
        if r != 0 {
            return Err(err(r));
        }
        WasixNet::set_nonblocking(fd);
        Ok(NetHandle(fd as i32))
    }

    fn recv(&mut self, h: NetHandle, buf: &mut [u8]) -> Result<usize, NetErr> {
        let iov = IoVec {
            buf: buf.as_ptr(),
            len: buf.len() as u32,
        };
        let mut n: u32 = 0;
        let mut oflags: u16 = 0;
        match unsafe { sock_recv(h.0 as u32, &iov, 1, 0, &mut n, &mut oflags) } {
            0 => Ok(n as usize),
            e => Err(err(e)),
        }
    }

    fn send(&mut self, h: NetHandle, buf: &[u8]) -> Result<usize, NetErr> {
        let iov = IoVec {
            buf: buf.as_ptr(),
            len: buf.len() as u32,
        };
        let mut n: u32 = 0;
        match unsafe { sock_send(h.0 as u32, &iov, 1, 0, &mut n) } {
            0 => Ok(n as usize),
            e => Err(err(e)),
        }
    }

    fn recv_from(
        &mut self,
        h: NetHandle,
        buf: &mut [u8],
    ) -> Result<(usize, Option<SockAddr>), NetErr> {
        let iov = IoVec {
            buf: buf.as_ptr(),
            len: buf.len() as u32,
        };
        let mut n: u32 = 0;
        let mut oflags: u16 = 0;
        let mut from = [0u8; wasix_addr::ADDR_PORT_LEN];
        let r = unsafe {
            sock_recv_from(
                h.0 as u32,
                &iov,
                1,
                0,
                &mut n,
                &mut oflags,
                from.as_mut_ptr(),
            )
        };
        if r != 0 {
            return Err(err(r));
        }
        // `decode` answers `None` for `Addressfamily::Unspec`, which is what an
        // untouched buffer is. Reporting that as `0.0.0.0:0` would give the
        // guest a source address that never existed.
        Ok((n as usize, wasix_addr::decode(&from)))
    }

    fn send_to(&mut self, h: NetHandle, buf: &[u8], a: &SockAddr) -> Result<usize, NetErr> {
        let iov = IoVec {
            buf: buf.as_ptr(),
            len: buf.len() as u32,
        };
        let dest = wasix_addr::encode(a);
        let mut n: u32 = 0;
        let r = unsafe { sock_send_to(h.0 as u32, &iov, 1, 0, dest.as_ptr(), &mut n) };
        if r != 0 {
            return Err(err(r));
        }
        Ok(n as usize)
    }

    fn addr(&mut self, h: NetHandle, peer: bool) -> Result<SockAddr, NetErr> {
        let mut buf = [0u8; wasix_addr::ADDR_PORT_LEN];
        let r = unsafe {
            if peer {
                sock_addr_peer(h.0 as u32, buf.as_mut_ptr())
            } else {
                sock_addr_local(h.0 as u32, buf.as_mut_ptr())
            }
        };
        if r != 0 {
            return Err(err(r));
        }
        // A success that decodes to nothing means the host wrote a family this
        // runtime does not speak -- `Unix`, or nothing at all. `EINVAL` says
        // that; inventing an all-zero address would not.
        wasix_addr::decode(&buf).ok_or(NetErr::INVAL)
    }

    fn setsockopt(
        &mut self,
        h: NetHandle,
        level: i32,
        name: i32,
        val: &[u8],
    ) -> Result<(), NetErr> {
        // ⚠️ NOTSUP rather than Ok for anything inexpressible. `sys_setsockopt`
        // turns that into success for the guest while LOGGING it as DROPPED,
        // and the logging is the point: an option that was never sent must not
        // look applied. That is exactly how `SO_REUSEADDR` went missing under
        // WasmEdge for long enough to break an nginx restart inside TIME_WAIT.
        let Some(opt) = wx_sockopt(level, name) else {
            return Err(NetErr::NOTSUP);
        };
        let v = opt_int(val);
        let r = match opt {
            Opt::Flag(o) => unsafe { sock_set_opt_flag(h.0 as u32, o, u32::from(v != 0)) },
            Opt::Size(o) => unsafe { sock_set_opt_size(h.0 as u32, o, v.max(0) as u64) },
        };
        match r {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn getsockopt(
        &mut self,
        h: NetHandle,
        level: i32,
        name: i32,
        out: &mut [u8],
    ) -> Result<usize, NetErr> {
        let n = out.len().min(4);
        // ⚠️ SO_ERROR IS NOT A PASS-THROUGH. `sock_get_opt_size(LastError)`
        // answers with a WASI errno, and handing that to a guest reports one
        // numbering as another -- WASI 14 is CONNREFUSED, Linux 14 is EFAULT.
        // libpq queries SO_ERROR on every connection and believes the answer.
        //
        // The in-progress family becomes 0, not EINPROGRESS: SO_ERROR means
        // "how did the connect END", and a connect still running has not.
        //
        // ⚠️ `Op::Connect` DOES collapse every other failure into
        // ECONNREFUSED -- that is `errno_of`'s catch-all for this op, and it is
        // a real loss: a timeout and an unreachable host both arrive as
        // "refused". It is still the better of the two available answers.
        // `Op::Other` would send them to EIO instead, and libpq reports what it
        // is given: "Connection refused" is wrong-but-actionable where
        // "Input/output error" is neither. Narrowing this means giving
        // `errno_of` the errnos it currently has no constants for, not
        // reinterpreting it here.
        let v: i32 = if level == SOL_SOCKET && name == SO_ERROR {
            let mut raw: u64 = 0;
            if unsafe { sock_get_opt_size(h.0 as u32, OPT_LAST_ERROR, &mut raw) } != 0 {
                0
            } else {
                let e = NetErr(raw as u16);
                if raw == 0 || e.is_in_progress() {
                    0
                } else {
                    super::errno_of(e, super::Op::Connect) as i32
                }
            }
        } else {
            match wx_sockopt(level, name) {
                Some(Opt::Flag(o)) => {
                    let mut flag: u8 = 0;
                    if unsafe { sock_get_opt_flag(h.0 as u32, o, &mut flag) } != 0 {
                        0
                    } else {
                        i32::from(flag != 0)
                    }
                }
                Some(Opt::Size(o)) => {
                    let mut raw: u64 = 0;
                    if unsafe { sock_get_opt_size(h.0 as u32, o, &mut raw) } != 0 {
                        0
                    } else {
                        raw as i32
                    }
                }
                // Answering zero for an option we cannot read is what
                // `net::wasmedge` does and for the same reason: libpq treats a
                // FAILED getsockopt as "could not get socket error status" and
                // kills a connection that had already succeeded.
                None => 0,
            }
        };
        out[..n].copy_from_slice(&v.to_le_bytes()[..n]);
        Ok(n)
    }

    fn shutdown(&mut self, h: NetHandle, read: bool, write: bool) -> Result<(), NetErr> {
        // `SdFlags` is a u8 bitmask, as under WasmEdge -- not Linux's 0/1/2.
        let sd = u32::from(read) | (u32::from(write) << 1);
        match unsafe { sock_shutdown(h.0 as u32, sd) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn close(&mut self, h: NetHandle) {
        // A WASIX socket handle IS a WASI fd, so the standard close applies.
        // `net::wasmedge::close` is the only other place that fact is used.
        unsafe { fd_close(h.0 as u32) };
    }

    fn ready(&mut self, h: NetHandle, want_write: bool) -> Readiness {
        // An immediate poll over {the fd, a 1ns clock}: the fd's userdata
        // appears iff it is ready now.
        //
        // ⚠️ Without this a socket looks perpetually readable AND writable, so
        // a guest polling a listen socket always sees "readable", calls
        // accept() with nothing pending, and blocks forever -- the PostgreSQL
        // postmaster ServerLoop deadlock.
        //
        // ❗ And the 1 is `PROBE_NANOS`, not the 0 `net::wasmedge` uses. See its
        // definition: a zero here does not probe, it waits forever.
        let mut subs = [0u8; poll1::SUB_LEN * 2];
        poll1::fd_sub(&mut subs, 0, 0, h.0 as u32, want_write);
        poll1::clock_sub(&mut subs, 1, 1, PROBE_NANOS);
        let mut events = [0u8; poll1::EVENT_LEN * 2];
        let mut nevents: u32 = 0;
        let r = unsafe { poll_oneoff(subs.as_ptr(), events.as_mut_ptr(), 2, &mut nevents) };
        let hit = if r != 0 {
            true // fail open: never wedge the guest on a probe error
        } else {
            (0..nevents as usize).any(|e| poll1::event_userdata(&events, e) == 0)
        };
        Readiness {
            read: hit && !want_write,
            write: hit && want_write,
        }
    }

    fn wait(&mut self, waiters: &[(NetHandle, bool)], timeout_nanos: Option<u128>) -> WaitOutcome {
        let n = waiters.len();
        // One subscription per waiter, plus a clock when a deadline is pending.
        // The clock's userdata is `n` -- deliberately outside the waiter index
        // range, so the caller's lookup drops it with no special case.
        let nsubs = n + usize::from(timeout_nanos.is_some());
        let mut subs = vec![0u8; poll1::SUB_LEN * nsubs];
        for (k, &(h, is_write)) in waiters.iter().enumerate() {
            poll1::fd_sub(&mut subs, k, k as u64, h.0 as u32, is_write);
        }
        if let Some(t) = timeout_nanos {
            // ❗ `.max(PROBE_NANOS)` is load-bearing. The scheduler passes the
            // time remaining until the earliest deadline, and an already-passed
            // deadline gives 0 -- which WASIX reads as "wait forever". The
            // guest would then sleep through the timer it was waiting on, and
            // it would happen only under load, only sometimes.
            poll1::clock_sub(
                &mut subs,
                n,
                n as u64,
                (t.min(u64::MAX as u128) as u64).max(PROBE_NANOS),
            );
        }
        let mut events = vec![0u8; poll1::EVENT_LEN * nsubs];
        let mut nevents: u32 = 0;
        let r = unsafe {
            poll_oneoff(
                subs.as_ptr(),
                events.as_mut_ptr(),
                nsubs as u32,
                &mut nevents,
            )
        };
        if r != 0 {
            crate::fatal!("poll_oneoff failed (errno {r})");
        }
        let ready: Vec<usize> = (0..nevents as usize)
            .map(|e| poll1::event_userdata(&events, e) as usize)
            .filter(|&i| i < n)
            .collect();
        if ready.is_empty() {
            WaitOutcome::TimedOut
        } else {
            WaitOutcome::Ready(ready)
        }
    }
}
