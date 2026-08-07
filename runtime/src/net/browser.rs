//! The browser backend: `raptormark_net_v1`.
//!
//! # Why a new import namespace
//!
//! WASI preview1's socket surface is accept-only -- `sock_accept`, `sock_recv`,
//! `sock_send`, `sock_shutdown`, for listeners the embedder pre-opens. There is
//! no `sock_open`, no `sock_bind`, no `sock_connect`, so **an outbound
//! connection cannot be expressed in standard preview1 at all**. WASI 0.2's
//! `wasi:sockets` exists but is a component-model world, and a core module
//! pinned to Wasm 2.0 cannot import it.
//!
//! Reusing WasmEdge's eleven names was considered and rejected. A JS host could
//! implement them, but that ABI is shaped for a host with real non-blocking
//! sockets underneath: a browser has promises, so it must fabricate `EAGAIN`
//! plus an out-of-band readiness signal anyway -- i.e. invent half of what is
//! below and then hide it behind an ill-fitting name. It would also inherit
//! WasmEdge's limitations, which are not the browser's: there is no TCP level in
//! its option set, so `TCP_NODELAY` is inexpressible there and expressible here.
//!
//! The name carries a VERSION so an ABI mismatch is an *instantiation* failure --
//! loud, immediate, and impossible to mistake for a runtime bug. That is the
//! opposite of what happens today when a host is missing a name it never knew
//! about.
//!
//! # Readiness is a local cache, not an import
//!
//! `NetBackend::ready` is called once per socket per `epoll_pwait`/`ppoll` scan.
//! Under WasmEdge each of those is a `poll_oneoff` round trip; here it is a
//! table lookup, because the host PUSHES readiness through the `ecv_net_ready`
//! export instead. So the browser profile is faster on this path than the server
//! one, and its idle path makes **zero** import calls.
//!
//! ⚠️ The edge/level reconciliation is the contract most likely to break.
//! `recv` returning `AGAIN` clears READ; `send` returning `AGAIN` clears WRITE;
//! `accept` returning `AGAIN` clears READ. The host must re-notify when the
//! condition becomes true again. Wrong in one direction gives a hang, wrong in
//! the other gives a spin. Over-notification is always safe: a woken process
//! re-checks and re-parks.

use super::{NetBackend, NetErr, NetHandle, Readiness, SockAddr, WaitOutcome};

/// Readiness bits, shared with the host through `ecv_net_ready`.
pub const READY_READ: u32 = 1;
pub const READY_WRITE: u32 = 2;

#[link(wasm_import_module = "raptormark_net_v1")]
extern "C" {
    /// Negotiates which transports the host offers. Returns a WASI errno.
    fn net_init(caps_out: *mut u32) -> u32;
    fn net_socket(v6: u32, dgram: u32, h_out: *mut u32) -> u32;
    fn net_bind(h: u32, addr: *const u8, addr_len: u32, port: u32) -> u32;
    fn net_listen(h: u32, backlog: u32) -> u32;
    /// 0, or `EINPROGRESS` -- never a completed connect on the first call.
    fn net_connect(h: u32, addr: *const u8, addr_len: u32, port: u32) -> u32;
    fn net_accept(h: u32, h_out: *mut u32) -> u32;
    fn net_recv(h: u32, buf: *mut u8, len: u32, n_out: *mut u32) -> u32;
    fn net_send(h: u32, buf: *const u8, len: u32, n_out: *mut u32) -> u32;
    fn net_recv_from(
        h: u32,
        buf: *mut u8,
        len: u32,
        n_out: *mut u32,
        addr_out: *mut u8,
        addr_len_out: *mut u32,
        port_out: *mut u32,
    ) -> u32;
    fn net_send_to(
        h: u32,
        buf: *const u8,
        len: u32,
        addr: *const u8,
        addr_len: u32,
        port: u32,
        n_out: *mut u32,
    ) -> u32;
    fn net_addr(
        h: u32,
        peer: u32,
        addr_out: *mut u8,
        addr_len_out: *mut u32,
        port_out: *mut u32,
    ) -> u32;
    /// One call for both directions: `set` selects, `len_io` is in/out. They are
    /// one host operation over one option table, so splitting them would double
    /// the surface for nothing.
    fn net_sockopt(h: u32, set: u32, level: i32, name: i32, buf: *mut u8, len_io: *mut u32) -> u32;
    fn net_shutdown(h: u32, rd: u32, wr: u32) -> u32;
    fn net_close(h: u32) -> u32;
    /// Resolves a name to an address the HOST chooses.
    ///
    /// Returns 0 with the address written, `EAGAIN` while the host is still
    /// working (the runtime retries when the socket is next readable), or an
    /// error, which becomes NXDOMAIN.
    ///
    /// ⚠️ The address is the host's to mint. A browser transport needs a NAME --
    /// `fetch` needs a URL, a relay's CONNECT needs a host, TLS needs SNI -- but
    /// `connect(2)` only carries an address. So the host allocates one per name
    /// and recognises it later; the runtime keeps no mapping and needs no
    /// connect-by-name call.
    fn net_resolve(
        name: *const u8,
        name_len: u32,
        v6: u32,
        addr_out: *mut u8,
        addr_len_out: *mut u32,
    ) -> u32;
}

fn err(e: u32) -> NetErr {
    NetErr(e as u16)
}

/// Host capabilities reported by `net_init`.
pub const CAP_RELAY: u32 = 1;
pub const CAP_FETCH_PROXY: u32 = 2;
pub const CAP_DATAGRAM: u32 = 4;

/// The port a guest's resolver talks to. Traffic to it is answered in-runtime
/// rather than sent anywhere, because a browser has no UDP to send it over.
const DNS_PORT: u16 = 53;

/// TTL on synthesized answers. Short on purpose: the host may mint a different
/// address for the same name later (a different transport, a restarted relay),
/// and a long TTL would pin a stale one inside the guest's own resolver cache
/// where nothing here can reach it.
const DNS_TTL: u32 = 30;

#[derive(Default)]
pub struct BrowserNet {
    /// Level-triggered readiness, indexed by handle. The host ORs bits in
    /// through `ecv_net_ready`; the operations below clear them on `AGAIN`.
    ready: Vec<u32>,
    /// Bumped whenever the host reports readiness. See `ready_generation`.
    ready_gen: u64,
    /// Datagram handles, so a `send_to` can be recognised as a DNS query.
    dgram: Vec<bool>,
    /// The peer a datagram socket was `connect`ed to, if any.
    ///
    /// ⚠️ A RESOLVER USUALLY CONNECTS ITS UDP SOCKET. glibc's `res_send` calls
    /// `connect(2)` on the datagram socket and then plain `send`/`recv` -- so an
    /// interception that only watched `sendto` sees nothing at all, and
    /// `getaddrinfo` fails with a name-resolution timeout that looks like a
    /// missing nameserver rather than a missing hook. Found exactly that way.
    peer: Vec<Option<SockAddr>>,
    /// Answers synthesized for a handle and not yet read by the guest, with the
    /// address they came from so `recvfrom` can report a source.
    pending: Vec<(NetHandle, Vec<u8>, SockAddr)>,
    caps: u32,
    inited: bool,
}

impl BrowserNet {
    pub fn new() -> BrowserNet {
        BrowserNet::default()
    }

    fn init(&mut self) {
        if self.inited {
            return;
        }
        self.inited = true;
        let mut caps: u32 = 0;
        // A host that cannot answer leaves caps at zero, which is honest: it has
        // no transport. Nothing here fails on it -- the failure surfaces at the
        // first connect, where it can name the destination.
        if unsafe { net_init(&mut caps) } == 0 {
            self.caps = caps;
        }
    }

    pub fn capabilities(&self) -> u32 {
        self.caps
    }

    fn slot(&mut self, h: NetHandle) -> &mut u32 {
        let i = h.0 as usize;
        if i >= self.ready.len() {
            self.ready.resize(i + 1, 0);
        }
        &mut self.ready[i]
    }

    /// Called from the `ecv_net_ready` export: the host reports that a handle
    /// made progress. ORs rather than assigns, so two notifications between
    /// slices cannot lose one.
    pub fn note_ready(&mut self, h: NetHandle, events: u32) {
        let slot = self.slot(h);
        let before = *slot;
        *slot |= events;
        // Only a CHANGE counts. Re-reporting readiness that is already recorded
        // must not make the scheduler think something happened, or a host that
        // notifies liberally -- which it is told to do, because under-notifying
        // hangs -- would keep waking every socket waiter forever and the module
        // would never go idle.
        if *slot != before {
            self.ready_gen = self.ready_gen.wrapping_add(1);
        }
    }

    fn clear(&mut self, h: NetHandle, bits: u32) {
        *self.slot(h) &= !bits;
    }

    /// A new handle starts with NOTHING ready.
    ///
    /// ⚠️ It used to start WRITE-ready, on the reasoning that "nothing has
    /// arrived and a send can be attempted". That is true of a CONNECTED socket
    /// and false of a fresh one, and the difference is not cosmetic: a blocking
    /// `connect` parks on writability, so a pre-set WRITE bit wakes the guest
    /// *before the connection has resolved*. The retry then sees "still in
    /// progress", and `sys_connect` treats a resumed in-progress connect as
    /// success -- so a connect to a dead port SUCCEEDED.
    ///
    /// Caught by `nonblockGuestSrc`'s last check, which exists for exactly this
    /// and is pinned against a real kernel. Readiness is the host's to report;
    /// inventing it here is guessing.
    fn fresh(&mut self, h: NetHandle) {
        *self.slot(h) = 0;
    }

    fn mark_dgram(&mut self, h: NetHandle, on: bool) {
        let i = h.0 as usize;
        if i >= self.dgram.len() {
            self.dgram.resize(i + 1, false);
        }
        self.dgram[i] = on;
    }

    fn is_dgram(&self, h: NetHandle) -> bool {
        self.dgram.get(h.0 as usize).copied().unwrap_or(false)
    }

    fn set_peer(&mut self, h: NetHandle, a: SockAddr) {
        let i = h.0 as usize;
        if i >= self.peer.len() {
            self.peer.resize(i + 1, None);
        }
        self.peer[i] = Some(a);
    }

    /// True when traffic on this handle is bound for a resolver -- either
    /// addressed explicitly (`sendto`) or implied by a previous `connect`.
    fn is_dns(&self, h: NetHandle, to: Option<&SockAddr>) -> Option<SockAddr> {
        if !self.is_dgram(h) {
            return None;
        }
        if let Some(a) = to {
            return (a.port == DNS_PORT).then(|| *a);
        }
        match self.peer.get(h.0 as usize).copied().flatten() {
            Some(a) if a.port == DNS_PORT => Some(a),
            _ => None,
        }
    }

    /// Answers a resolver query in-runtime.
    ///
    /// Returns the byte count "sent" on success, so the guest's `sendto` looks
    /// entirely ordinary; the answer is queued for its next `recvfrom`.
    fn dns_query(&mut self, h: NetHandle, msg: &[u8], server: &SockAddr) -> Result<usize, NetErr> {
        let Some(q) = super::dns::parse_query(msg) else {
            // Not a shape this tap answers. Dropping it silently would be a
            // resolver timeout with nothing to point at, so refuse loudly.
            return Err(NetErr::INVAL);
        };
        let want_v6 = q.qtype == super::dns::TYPE_AAAA;
        let name = q.name.as_bytes();
        let mut octets = [0u8; 16];
        let mut alen: u32 = 0;
        let r = unsafe {
            net_resolve(
                name.as_ptr(),
                name.len() as u32,
                u32::from(want_v6),
                octets.as_mut_ptr(),
                &mut alen,
            )
        };
        if r == NetErr::AGAIN.0 as u32 {
            // The host is still working. Report a would-block on the SEND so the
            // guest retries the whole query -- resolvers do exactly that, and it
            // needs no second state machine here.
            self.clear(h, READY_WRITE);
            return Err(NetErr::AGAIN);
        }
        let reply = if r == 0 && alen as usize == if want_v6 { 16 } else { 4 } {
            super::dns::answer(msg, &q, &octets[..alen as usize], DNS_TTL)
        } else {
            super::dns::nxdomain(msg, &q)
        };
        let Some(reply) = reply else {
            return Err(NetErr::INVAL);
        };
        self.pending.push((h, reply, *server));
        // The answer is available now, so the socket is readable now.
        *self.slot(h) |= READY_READ;
        Ok(msg.len())
    }

    fn take_pending(&mut self, h: NetHandle) -> Option<(Vec<u8>, SockAddr)> {
        let i = self.pending.iter().position(|(x, _, _)| *x == h)?;
        let (_, data, from) = self.pending.remove(i);
        Some((data, from))
    }
}

impl NetBackend for BrowserNet {
    fn socket(&mut self, v6: bool, dgram: bool) -> Result<NetHandle, NetErr> {
        self.init();
        let mut h: u32 = 0;
        let r = unsafe { net_socket(u32::from(v6), u32::from(dgram), &mut h) };
        if r != 0 {
            return Err(err(r));
        }
        let handle = NetHandle(h as i32);
        self.fresh(handle);
        self.mark_dgram(handle, dgram);
        Ok(handle)
    }

    fn bind(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        let o = a.bytes();
        match unsafe { net_bind(h.0 as u32, o.as_ptr(), o.len() as u32, a.port as u32) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn listen(&mut self, h: NetHandle, backlog: u32) -> Result<(), NetErr> {
        match unsafe { net_listen(h.0 as u32, backlog) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn connect(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        if self.is_dgram(h) {
            // A connected datagram socket only fixes its default peer; there is
            // no handshake to wait for, and a resolver expects it to succeed
            // immediately so it can `send`.
            self.set_peer(h, *a);
            *self.slot(h) |= READY_WRITE;
            return Ok(());
        }
        let o = a.bytes();
        match unsafe { net_connect(h.0 as u32, o.as_ptr(), o.len() as u32, a.port as u32) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn accept(&mut self, h: NetHandle) -> Result<NetHandle, NetErr> {
        let mut c: u32 = 0;
        let r = unsafe { net_accept(h.0 as u32, &mut c) };
        if r != 0 {
            // Nothing pending: the listener is no longer readable until the host
            // says otherwise. Without this clear the guest spins on a stale bit.
            if err(r) == NetErr::AGAIN {
                self.clear(h, READY_READ);
            }
            return Err(err(r));
        }
        let child = NetHandle(c as i32);
        self.fresh(child);
        Ok(child)
    }

    fn recv(&mut self, h: NetHandle, buf: &mut [u8]) -> Result<usize, NetErr> {
        // A synthesized answer is served here too: a resolver that connected its
        // socket reads with `recv`, not `recvfrom`.
        if let Some((data, _)) = self.take_pending(h) {
            let n = buf.len().min(data.len());
            buf[..n].copy_from_slice(&data[..n]);
            if !self.pending.iter().any(|(x, _, _)| *x == h) {
                self.clear(h, READY_READ);
            }
            return Ok(n);
        }
        let mut n: u32 = 0;
        let r = unsafe { net_recv(h.0 as u32, buf.as_mut_ptr(), buf.len() as u32, &mut n) };
        if r != 0 {
            if err(r) == NetErr::AGAIN {
                self.clear(h, READY_READ);
            }
            return Err(err(r));
        }
        Ok(n as usize)
    }

    fn send(&mut self, h: NetHandle, buf: &[u8]) -> Result<usize, NetErr> {
        if let Some(server) = self.is_dns(h, None) {
            return self.dns_query(h, buf, &server);
        }
        let mut n: u32 = 0;
        let r = unsafe { net_send(h.0 as u32, buf.as_ptr(), buf.len() as u32, &mut n) };
        if r != 0 {
            if err(r) == NetErr::AGAIN {
                self.clear(h, READY_WRITE);
            }
            return Err(err(r));
        }
        Ok(n as usize)
    }

    fn recv_from(
        &mut self,
        h: NetHandle,
        buf: &mut [u8],
    ) -> Result<(usize, Option<SockAddr>), NetErr> {
        // A synthesized answer is served before the transport is consulted, and
        // keeps its message boundary: one recvfrom yields one datagram.
        if let Some((data, from)) = self.take_pending(h) {
            let n = buf.len().min(data.len());
            buf[..n].copy_from_slice(&data[..n]);
            if !self.pending.iter().any(|(x, _, _)| *x == h) {
                self.clear(h, READY_READ);
            }
            return Ok((n, Some(from)));
        }
        let mut n: u32 = 0;
        let mut octets = [0u8; 16];
        let mut alen: u32 = 0;
        let mut port: u32 = 0;
        let r = unsafe {
            net_recv_from(
                h.0 as u32,
                buf.as_mut_ptr(),
                buf.len() as u32,
                &mut n,
                octets.as_mut_ptr(),
                &mut alen,
                &mut port,
            )
        };
        if r != 0 {
            if err(r) == NetErr::AGAIN {
                self.clear(h, READY_READ);
            }
            return Err(err(r));
        }
        let from = SockAddr {
            octets,
            v6: alen == 16,
            port: port as u16,
        };
        Ok((n as usize, Some(from)))
    }

    fn send_to(&mut self, h: NetHandle, buf: &[u8], a: &SockAddr) -> Result<usize, NetErr> {
        // ⚠️ The resolver's traffic never leaves. A browser has no UDP at all,
        // so this is not an optimisation -- it is the only way the guest's own
        // resolver can work. See `net::dns`.
        if let Some(server) = self.is_dns(h, Some(a)) {
            return self.dns_query(h, buf, &server);
        }
        let o = a.bytes();
        let mut n: u32 = 0;
        let r = unsafe {
            net_send_to(
                h.0 as u32,
                buf.as_ptr(),
                buf.len() as u32,
                o.as_ptr(),
                o.len() as u32,
                a.port as u32,
                &mut n,
            )
        };
        if r != 0 {
            if err(r) == NetErr::AGAIN {
                self.clear(h, READY_WRITE);
            }
            return Err(err(r));
        }
        Ok(n as usize)
    }

    fn addr(&mut self, h: NetHandle, peer: bool) -> Result<SockAddr, NetErr> {
        let mut octets = [0u8; 16];
        let mut alen: u32 = 0;
        let mut port: u32 = 0;
        let r = unsafe {
            net_addr(
                h.0 as u32,
                u32::from(peer),
                octets.as_mut_ptr(),
                &mut alen,
                &mut port,
            )
        };
        if r != 0 {
            return Err(err(r));
        }
        Ok(SockAddr {
            octets,
            v6: alen == 16,
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
        let mut buf = [0u8; 8];
        let n = val.len().min(8);
        buf[..n].copy_from_slice(&val[..n]);
        let mut len = n as u32;
        match unsafe { net_sockopt(h.0 as u32, 1, level, name, buf.as_mut_ptr(), &mut len) } {
            0 => Ok(()),
            // An option the host cannot express is dropped, not failed -- nginx
            // treats a failed setsockopt on a listener as fatal.
            _ => Ok(()),
        }
    }

    fn getsockopt(
        &mut self,
        h: NetHandle,
        level: i32,
        name: i32,
        out: &mut [u8],
    ) -> Result<usize, NetErr> {
        let mut len = out.len().min(8) as u32;
        let mut buf = [0u8; 8];
        let r = unsafe { net_sockopt(h.0 as u32, 0, level, name, buf.as_mut_ptr(), &mut len) };
        let n = (len as usize).min(out.len()).min(8);
        if r != 0 {
            // Zero rather than an error: libpq queries SO_ERROR on EVERY
            // connection and reads a failure as "could not get socket error
            // status", which fails a connection that already succeeded.
            out[..n].fill(0);
            return Ok(n);
        }
        out[..n].copy_from_slice(&buf[..n]);
        Ok(n)
    }

    fn shutdown(&mut self, h: NetHandle, read: bool, write: bool) -> Result<(), NetErr> {
        match unsafe { net_shutdown(h.0 as u32, u32::from(read), u32::from(write)) } {
            0 => Ok(()),
            e => Err(err(e)),
        }
    }

    fn close(&mut self, h: NetHandle) {
        unsafe { net_close(h.0 as u32) };
        *self.slot(h) = 0;
        self.pending.retain(|(x, _, _)| *x != h);
        self.mark_dgram(h, false);
        if let Some(p) = self.peer.get_mut(h.0 as usize) {
            *p = None;
        }
    }

    fn ready(&mut self, h: NetHandle, want_write: bool) -> Readiness {
        let bits = *self.slot(h);
        Readiness {
            read: !want_write && (bits & READY_READ) != 0,
            write: want_write && (bits & READY_WRITE) != 0,
        }
    }

    fn ready_generation(&self) -> u64 {
        self.ready_gen
    }

    fn wait(&mut self, waiters: &[(NetHandle, bool)], _timeout: Option<u128>) -> WaitOutcome {
        // Anything already ready is reported without involving the host.
        let hit: Vec<usize> = waiters
            .iter()
            .enumerate()
            .filter(|(_, (h, w))| {
                let bits = *self.slot(*h);
                if *w {
                    bits & READY_WRITE != 0
                } else {
                    bits & READY_READ != 0
                }
            })
            .map(|(i, _)| i)
            .collect();
        if !hit.is_empty() {
            return WaitOutcome::Ready(hit);
        }
        // ⚠️ NEVER BLOCKS, on purpose and permanently. `Atomics.wait` is illegal
        // on a browser main thread, and in a worker a blocking host stalls the
        // very event loop that would deliver the readiness being waited for. The
        // scheduler turns this into an `Idle` outcome and the host resumes us
        // when something happens.
        WaitOutcome::WouldBlock
    }
}
