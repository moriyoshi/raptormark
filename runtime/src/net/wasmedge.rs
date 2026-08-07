//! The WasmEdge socket extension, and everything that is specific to it.
//!
//! This is the backend the shipping server profile uses. Every WasmEdge fact
//! now lives here rather than at a syscall site: the `extern` declarations, the
//! `WasiAddress` form, the address-family and socket-type enums, and the
//! `SocketOptName` translation. `sys.rs` no longer knows the host it is talking
//! to, which is the point of the seam.
//!
//! ⚠️ The specification for this ABI is the `extern` block below plus WasmEdge
//! itself. `sys.rs` used to cite
//! `third_party/wazero/imports/wasi_snapshot_preview1/sock_wasmedge.go` as the
//! reference host; that path is not in this tree and never has been. The
//! executable statement of it is `web/` (the Node host), which is checked
//! against a real module by `e2e/nodehost_test.go`.

use super::{NetBackend, NetErr, NetHandle, Readiness, SockAddr, WaitOutcome};

/// The WasmEdge `WasiAddress` (raw-address V1 form): `buf` points at the
/// network-order address octets, `size` is their length (4 or 16).
///
/// On wasm32 a Rust pointer's integer value IS its linear-memory offset, which
/// is exactly what the host reads -- so pointers into ecvisor's own stack cross
/// the boundary correctly with no translation.
#[repr(C)]
struct WasiAddress {
    buf: *const u8,
    size: usize,
}

/// A single iovec for `sock_send_to` / `sock_recv_from`.
#[repr(C)]
struct IoVec {
    buf: *const u8,
    len: usize,
}

/// A WASI `ciovec`/`iovec` (`{ buf: u32, len: u32 }` on wasm32) for
/// `fd_read`/`fd_write`.
#[repr(C)]
struct Ciovec {
    buf: *const u8,
    len: u32,
}

const WE_FAMILY_INET4: u32 = 1;
const WE_FAMILY_INET6: u32 = 2;
const WE_SOCK_DGRAM: u32 = 1;
const WE_SOCK_STREAM: u32 = 2;
const WE_SOL_SOCKET: i32 = 0;
const FD_NONBLOCK: u32 = 4;

/// Linux `SOL_SOCKET` and the option names that have a WasmEdge equivalent.
const SOL_SOCKET: i32 = 1;
const SO_REUSEADDR: i32 = 2;
const SO_DONTROUTE: i32 = 5;
const SO_BROADCAST: i32 = 6;
const SO_SNDBUF: i32 = 7;
const SO_RCVBUF: i32 = 8;
const SO_KEEPALIVE: i32 = 9;
const SO_OOBINLINE: i32 = 10;
const SO_LINGER: i32 = 13;
const SO_RCVLOWAT: i32 = 18;
const SO_RCVTIMEO: i32 = 20;
const SO_SNDTIMEO: i32 = 21;

const EVENT_CLOCK: u8 = 0;
const EVENT_FD_READ: u8 = 1;
const EVENT_FD_WRITE: u8 = 2;

#[link(wasm_import_module = "wasi_snapshot_preview1")]
extern "C" {
    fn sock_open(family: u32, sock_type: u32, fd_out: *mut u32) -> u32;
    fn sock_bind(fd: u32, addr: *const WasiAddress, port: u32) -> u32;
    fn sock_connect(fd: u32, addr: *const WasiAddress, port: u32) -> u32;
    fn sock_listen(fd: u32, backlog: u32) -> u32;
    fn sock_accept(fd: u32, flags: u32, fd_out: *mut u32) -> u32;
    fn sock_getlocaladdr(
        fd: u32,
        addr: *const WasiAddress,
        addr_type: *mut u32,
        port: *mut u32,
    ) -> u32;
    fn sock_getpeeraddr(
        fd: u32,
        addr: *const WasiAddress,
        addr_type: *mut u32,
        port: *mut u32,
    ) -> u32;
    fn sock_getsockopt(fd: u32, level: i32, name: i32, flag: *mut i32, flag_size: *mut u32) -> u32;
    fn sock_setsockopt(fd: u32, level: i32, name: i32, flag: *const i32, flag_size: u32) -> u32;
    fn sock_shutdown(fd: u32, how: u32) -> u32;
    fn sock_send_to(
        fd: u32,
        iov: *const IoVec,
        iov_len: u32,
        addr: *const u8,
        port: u32,
        flags: u32,
        sent_out: *mut u32,
    ) -> u32;
    fn sock_recv_from(
        fd: u32,
        iov: *const IoVec,
        iov_len: u32,
        addr: *mut u8,
        flags: u32,
        port_out: *mut u32,
        recv_out: *mut u32,
        oflags_out: *mut u32,
    ) -> u32;
}

// Standard preview1, called directly on socket handles so the raw WASI errno
// (specifically EAGAIN) is observable rather than being flattened by libc.
#[link(wasm_import_module = "wasi_snapshot_preview1")]
extern "C" {
    fn fd_read(fd: u32, iovs: *const Ciovec, iovs_len: u32, nread: *mut u32) -> u32;
    fn fd_write(fd: u32, iovs: *const Ciovec, iovs_len: u32, nwritten: *mut u32) -> u32;
    fn fd_fdstat_set_flags(fd: u32, flags: u32) -> u32;
    fn fd_close(fd: u32) -> u32;
    fn poll_oneoff(in_subs: *const u8, out_events: *mut u8, nsubs: u32, nevents: *mut u32) -> u32;
}

fn err(e: u32) -> NetErr {
    NetErr(e as u16)
}

/// Translates a Linux `(level, optname)` into WasmEdge's own `SocketOptName`,
/// or `None` when there is no equivalent.
///
/// ⚠️ The numbering is NOT shared, which is easy to miss because the call
/// type-checks either way and the result used to be discarded: nginx's
/// `SOL_SOCKET`/`SO_REUSEADDR` (1, 2) and `IPPROTO_TCP`/`TCP_NODELAY` (6, 1)
/// both came back as WASI errno 28 and were reported to the guest as success.
/// So `SO_REUSEADDR` never took, and an nginx restart inside the port's
/// TIME_WAIT window failed to bind.
///
/// The mapping was established by sweeping what the host accepts rather than by
/// reading a spec: level 0 is the only level accepted at all, and it takes
/// names [0, 3, 4, 5, 6, 7, 8, 10] -- exactly WasmEdge's `SocketOptName` minus
/// the read-only entries and the ones needing a payload other than a 4-byte
/// int. Two independent facts agreeing is the evidence; neither alone would be.
///
/// **There is no TCP level, so `TCP_NODELAY` cannot be expressed at all.** That
/// is a WasmEdge limitation and it stops here: a backend that can express it
/// should not inherit this.
fn we_sockopt(level: i32, name: i32) -> Option<(i32, i32)> {
    if level != SOL_SOCKET {
        return None;
    }
    let nm = match name {
        SO_REUSEADDR => 0,
        SO_DONTROUTE => 3,
        SO_BROADCAST => 4,
        SO_SNDBUF => 5,
        SO_RCVBUF => 6,
        SO_KEEPALIVE => 7,
        SO_OOBINLINE => 8,
        SO_LINGER => 9,
        SO_RCVLOWAT => 10,
        SO_RCVTIMEO => 11,
        SO_SNDTIMEO => 12,
        _ => return None,
    };
    Some((WE_SOL_SOCKET, nm))
}

/// The WasmEdge-backed network. Stateless: every handle is a host descriptor.
#[derive(Default)]
pub struct HostWasiNet;

impl HostWasiNet {
    pub fn new() -> HostWasiNet {
        HostWasiNet
    }
}

impl NetBackend for HostWasiNet {
    fn socket(&mut self, v6: bool, dgram: bool) -> Result<NetHandle, NetErr> {
        let family = if v6 { WE_FAMILY_INET6 } else { WE_FAMILY_INET4 };
        let ty = if dgram { WE_SOCK_DGRAM } else { WE_SOCK_STREAM };
        let mut fd: u32 = 0;
        let r = unsafe { sock_open(family, ty, &mut fd) };
        if r != 0 {
            return Err(err(r));
        }
        // Every host socket is kept non-blocking so a would-block suspends the
        // guest process cooperatively instead of parking the whole instance.
        // This belongs to the backend, not to the syscall layer: the guest's
        // own O_NONBLOCK is a separate, higher-level concept.
        unsafe { fd_fdstat_set_flags(fd, FD_NONBLOCK) };
        Ok(NetHandle(fd as i32))
    }

    fn bind(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        let octets = a.bytes();
        let waddr = WasiAddress {
            buf: octets.as_ptr(),
            size: octets.len(),
        };
        match unsafe { sock_bind(h.0 as u32, &waddr, a.port as u32) } {
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
        let octets = a.bytes();
        let waddr = WasiAddress {
            buf: octets.as_ptr(),
            size: octets.len(),
        };
        match unsafe { sock_connect(h.0 as u32, &waddr, a.port as u32) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn accept(&mut self, h: NetHandle) -> Result<NetHandle, NetErr> {
        let mut fd: u32 = 0;
        // The standardized 3-argument preview1 form, not a WasmEdge extension.
        let r = unsafe { sock_accept(h.0 as u32, 0, &mut fd) };
        if r != 0 {
            return Err(err(r));
        }
        unsafe { fd_fdstat_set_flags(fd, FD_NONBLOCK) };
        Ok(NetHandle(fd as i32))
    }

    fn recv(&mut self, h: NetHandle, buf: &mut [u8]) -> Result<usize, NetErr> {
        let iov = Ciovec {
            buf: buf.as_ptr(),
            len: buf.len() as u32,
        };
        let mut n: u32 = 0;
        match unsafe { fd_read(h.0 as u32, &iov, 1, &mut n) } {
            0 => Ok(n as usize),
            e => Err(err(e)),
        }
    }

    fn send(&mut self, h: NetHandle, buf: &[u8]) -> Result<usize, NetErr> {
        let iov = Ciovec {
            buf: buf.as_ptr(),
            len: buf.len() as u32,
        };
        let mut n: u32 = 0;
        match unsafe { fd_write(h.0 as u32, &iov, 1, &mut n) } {
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
            len: buf.len(),
        };
        // The address form here is NOT WasiAddress: it is a flat buffer whose
        // first two bytes are the family as a little-endian u16, with the
        // octets from offset 2.
        let mut addr128 = [0u8; 128];
        let mut port: u32 = 0;
        let mut recvd: u32 = 0;
        let mut oflags: u32 = 0;
        let r = unsafe {
            sock_recv_from(
                h.0 as u32,
                &iov,
                1,
                addr128.as_mut_ptr(),
                0,
                &mut port,
                &mut recvd,
                &mut oflags,
            )
        };
        if r != 0 {
            return Err(err(r));
        }
        let fam = u16::from_le_bytes([addr128[0], addr128[1]]) as u32;
        let v6 = fam == WE_FAMILY_INET6;
        let mut octets = [0u8; 16];
        let n = if v6 { 16 } else { 4 };
        octets[..n].copy_from_slice(&addr128[2..2 + n]);
        Ok((
            recvd as usize,
            Some(SockAddr {
                octets,
                v6,
                port: port as u16,
            }),
        ))
    }

    fn send_to(&mut self, h: NetHandle, buf: &[u8], a: &SockAddr) -> Result<usize, NetErr> {
        let iov = IoVec {
            buf: buf.as_ptr(),
            len: buf.len(),
        };
        let mut addr128 = [0u8; 128];
        let fam: u32 = if a.v6 {
            WE_FAMILY_INET6
        } else {
            WE_FAMILY_INET4
        };
        addr128[0..2].copy_from_slice(&(fam as u16).to_le_bytes());
        let octets = a.bytes();
        addr128[2..2 + octets.len()].copy_from_slice(octets);
        let mut sent: u32 = 0;
        let r = unsafe {
            sock_send_to(
                h.0 as u32,
                &iov,
                1,
                addr128.as_ptr(),
                a.port as u32,
                0,
                &mut sent,
            )
        };
        if r != 0 {
            return Err(err(r));
        }
        Ok(sent as usize)
    }

    fn addr(&mut self, h: NetHandle, peer: bool) -> Result<SockAddr, NetErr> {
        let mut octets = [0u8; 16];
        let waddr = WasiAddress {
            buf: octets.as_mut_ptr() as *const u8,
            size: 16,
        };
        let mut addr_type: u32 = 0;
        let mut port: u32 = 0;
        let r = unsafe {
            if peer {
                sock_getpeeraddr(h.0 as u32, &waddr, &mut addr_type, &mut port)
            } else {
                sock_getlocaladdr(h.0 as u32, &waddr, &mut addr_type, &mut port)
            }
        };
        if r != 0 {
            return Err(err(r));
        }
        Ok(SockAddr {
            octets,
            v6: addr_type == WE_FAMILY_INET6,
            port: port as u16,
        })
    }

    fn setsockopt(
        &mut self,
        h: NetHandle,
        level: i32,
        name: i32,
        val: &[u8],
    ) -> Result<(), NetErr> {
        // ⚠️ NOT expressible through WasmEdge's option set -> NOTSUP, which the
        // caller turns into success for the guest while LOGGING it as dropped.
        // Reporting plain `Ok` here would make the diagnostic agree with the
        // bug it exists to catch: an option that was never sent would look
        // applied, which is precisely how SO_REUSEADDR went missing for so long.
        let Some((lv, nm)) = we_sockopt(level, name) else {
            return Err(NetErr::NOTSUP);
        };
        let p = val.as_ptr() as *const i32;
        match unsafe { sock_setsockopt(h.0 as u32, lv, nm, p, val.len() as u32) } {
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
        // ⚠️ Linux numbers go through VERBATIM here, unlike setsockopt above.
        // That is what the syscall layer has always done, and it is deliberate:
        // the read-only options a guest actually asks for (SO_ERROR above all,
        // which libpq queries on every connection) have no WasmEdge equivalent
        // to translate to, and answering zero is the useful reply.
        let mut flag: i32 = 0;
        let mut size: u32 = out.len().min(4) as u32;
        let r = unsafe { sock_getsockopt(h.0 as u32, level, name, &mut flag, &mut size) };
        let n = (size as usize).min(out.len()).min(4);
        if r != 0 {
            out[..n].fill(0);
            return Ok(n);
        }
        out[..n].copy_from_slice(&flag.to_le_bytes()[..n]);
        Ok(n)
    }

    fn shutdown(&mut self, h: NetHandle, read: bool, write: bool) -> Result<(), NetErr> {
        // WasmEdge takes SD_RD/SD_WR as bitflags (1/2), not Linux's 0/1/2.
        let sd = u32::from(read) | (u32::from(write) << 1);
        match unsafe { sock_shutdown(h.0 as u32, sd) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn close(&mut self, h: NetHandle) {
        // A WasmEdge socket handle IS a WASI fd, so the standard close applies.
        // This is the one place that fact is allowed to be used.
        unsafe { fd_close(h.0 as u32) };
    }

    fn ready(&mut self, h: NetHandle, want_write: bool) -> Readiness {
        // A zero-timeout poll over {the fd, a 0ns clock} returns at once, and
        // the fd's userdata appears iff it is ready now.
        //
        // ⚠️ Without this a socket looks perpetually readable AND writable, so
        // a guest polling a listen socket always sees "readable", calls
        // accept() with nothing pending, and blocks forever -- exactly the
        // PostgreSQL postmaster ServerLoop deadlock.
        let mut subs = [0u8; 96];
        subs[8] = if want_write {
            EVENT_FD_WRITE
        } else {
            EVENT_FD_READ
        };
        subs[16..20].copy_from_slice(&(h.0 as u32).to_le_bytes());
        subs[48..56].copy_from_slice(&1u64.to_le_bytes()); // userdata = 1 (clock)
        subs[56] = EVENT_CLOCK;
        subs[64..68].copy_from_slice(&1u32.to_le_bytes()); // CLOCK_MONOTONIC
        let mut events = [0u8; 64];
        let mut nevents: u32 = 0;
        let r = unsafe { poll_oneoff(subs.as_ptr(), events.as_mut_ptr(), 2, &mut nevents) };
        let hit = if r != 0 {
            true // fail open: never wedge the guest on a probe error
        } else {
            (0..nevents as usize)
                .any(|e| u64::from_le_bytes(events[e * 32..e * 32 + 8].try_into().unwrap()) == 0)
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
        let mut subs = vec![0u8; 48 * nsubs];
        for (k, &(h, is_write)) in waiters.iter().enumerate() {
            let off = 48 * k;
            subs[off..off + 8].copy_from_slice(&(k as u64).to_le_bytes());
            subs[off + 8] = if is_write {
                EVENT_FD_WRITE
            } else {
                EVENT_FD_READ
            };
            subs[off + 16..off + 20].copy_from_slice(&(h.0 as u32).to_le_bytes());
        }
        if let Some(t) = timeout_nanos {
            let off = 48 * n;
            subs[off..off + 8].copy_from_slice(&(n as u64).to_le_bytes());
            subs[off + 8] = EVENT_CLOCK;
            subs[off + 16..off + 20].copy_from_slice(&1u32.to_le_bytes());
            subs[off + 24..off + 32]
                .copy_from_slice(&(t.min(u64::MAX as u128) as u64).to_le_bytes());
        }
        let mut events = vec![0u8; 32 * nsubs];
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
            .map(|e| u64::from_le_bytes(events[e * 32..e * 32 + 8].try_into().unwrap()) as usize)
            .filter(|&i| i < n)
            .collect();
        if ready.is_empty() {
            WaitOutcome::TimedOut
        } else {
            WaitOutcome::Ready(ready)
        }
    }
}
