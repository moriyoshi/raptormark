//! A pure-Rust, in-process network. No host imports at all.
//!
//! It serves two purposes, and the second is the load-bearing one:
//!
//! 1. It is the backend `cargo test` gets, which is what makes the socket
//!    syscall layer testable on the host for the first time. `sys.rs` is gated
//!    to `wasm32` because of its `extern` blocks, so until the seam existed
//!    none of this logic could be executed off-target.
//! 2. Built for wasm, it produces a module with **zero** `sock_*` imports. That
//!    is the empirical check that the compile-time seam actually removes them,
//!    which no amount of reasoning about `dyn` can substitute for.
//!
//! What it models is deliberately small: AF_INET/AF_INET6 loopback between
//! sockets in the same process. There is no routing, no external world, and a
//! `connect` to an address nobody has bound is refused rather than pending.

use super::{NetBackend, NetErr, NetHandle, Readiness, SockAddr, WaitOutcome};
use std::collections::VecDeque;

#[derive(Default)]
struct Sock {
    /// Set once `bind` names an endpoint.
    bound: Option<SockAddr>,
    listening: bool,
    /// Connections accepted by the transport, awaiting the guest's `accept`.
    backlog: VecDeque<NetHandle>,
    /// The other end of an established stream, if any.
    peer: Option<NetHandle>,
    peer_addr: Option<SockAddr>,
    /// Bytes waiting to be read by THIS socket.
    rx: VecDeque<u8>,
    /// Datagram queue: (payload, source).
    dgrams: VecDeque<(Vec<u8>, SockAddr)>,
    dgram: bool,
    /// The peer hung up; a read past the queued bytes is EOF, not a would-block.
    eof: bool,
    closed: bool,
}

#[derive(Default)]
pub struct LoopbackNet {
    socks: Vec<Sock>,
}

impl LoopbackNet {
    pub fn new() -> LoopbackNet {
        LoopbackNet::default()
    }

    fn get(&mut self, h: NetHandle) -> Result<&mut Sock, NetErr> {
        self.socks
            .get_mut(h.0 as usize)
            .filter(|s| !s.closed)
            .ok_or(NetErr::BADF)
    }

    fn find_listener(&self, a: &SockAddr) -> Option<usize> {
        self.socks.iter().position(|s| {
            !s.closed
                && s.listening
                && s.bound
                    .map(|b| {
                        b.port == a.port
                            && (b.bytes().iter().all(|&x| x == 0) || b.octets == a.octets)
                    })
                    .unwrap_or(false)
        })
    }

    fn find_bound_dgram(&self, a: &SockAddr) -> Option<usize> {
        self.socks.iter().position(|s| {
            !s.closed && s.dgram && s.bound.map(|b| b.port == a.port).unwrap_or(false)
        })
    }

    /// Whether any live socket already holds this port.
    ///
    /// ⚠️ Deliberately PORT-ONLY, ignoring the address. It has to agree with
    /// whatever `find_listener` and `find_bound_dgram` would match, and both of
    /// those key on the port -- `find_listener` treats an all-zero bind address
    /// as a wildcard, and `find_bound_dgram` does not look at the address at
    /// all. A stricter test here would hand out a port those two then consider
    /// taken, which is the same silent cross-connect this fix exists to remove.
    fn port_taken(&self, port: u16) -> bool {
        self.socks
            .iter()
            .any(|s| !s.closed && s.bound.map(|b| b.port == port).unwrap_or(false))
    }

    /// An unused port from Linux's default ephemeral range.
    ///
    /// The range is `net.ipv4.ip_local_port_range`'s default, 32768..=60999, so
    /// a guest that has an opinion about what an ephemeral port looks like is
    /// not surprised. `None` when every port in the range is taken -- 28,232
    /// live bound sockets, which this backend will not reach, but returning an
    /// `Option` means the caller answers EADDRINUSE rather than looping or
    /// handing out a duplicate.
    ///
    /// ⚠️ Scans from the low end rather than tracking a rotating cursor. That
    /// makes the assignment DETERMINISTIC, which is what a test can assert on;
    /// this backend has no randomness available to it (`Math.random`-style
    /// entropy is not part of the wasip1 surface it is built for) and a
    /// pseudo-random cursor would only imitate one property of a kernel while
    /// costing the property that makes it testable.
    fn ephemeral_port(&self) -> Option<u16> {
        (32768u16..=60999).find(|&p| !self.port_taken(p))
    }

    fn alloc(&mut self, dgram: bool) -> NetHandle {
        self.socks.push(Sock {
            dgram,
            ..Sock::default()
        });
        NetHandle(self.socks.len() as i32 - 1)
    }
}

impl NetBackend for LoopbackNet {
    fn socket(&mut self, _v6: bool, dgram: bool) -> Result<NetHandle, NetErr> {
        Ok(self.alloc(dgram))
    }

    fn bind(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        let mut addr = *a;
        // ❗ PORT 0 MEANS "CHOOSE ONE", and it must be chosen HERE.
        //
        // This backend used to store the address verbatim, so a `bind` to port 0
        // stayed bound to port 0 and `getsockname` reported 0. That is not a
        // cosmetic difference from a kernel: it breaks the ordinary ephemeral
        // server shape -- bind 0, ask what you got, advertise it -- which is how
        // most guests that do not have a fixed port in mind bind. And it fails
        // QUIETLY, because `find_listener` matches port 0 against port 0 just as
        // happily as any other number, so the connection still works and only
        // the advertised port is wrong.
        //
        // Worse, two sockets binding port 0 both became "bound to 0" and
        // `find_listener` returned the FIRST for both, silently cross-connecting
        // two unrelated servers.
        //
        // Found 2026-08-25 by `TestLoopbackProfileEpollsASocketWithoutHanging`,
        // whose guest binds port 0 so two profiles' runs cannot collide on a
        // fixed one. Every earlier loopback test named a port, which is why
        // nothing had noticed.
        if addr.port == 0 {
            addr.port = self.ephemeral_port().ok_or(NetErr::ADDRINUSE)?;
        }
        self.get(h)?.bound = Some(addr);
        Ok(())
    }

    fn listen(&mut self, h: NetHandle, _backlog: u32) -> Result<(), NetErr> {
        let s = self.get(h)?;
        if s.bound.is_none() {
            return Err(NetErr::INVAL);
        }
        s.listening = true;
        Ok(())
    }

    fn connect(&mut self, h: NetHandle, a: &SockAddr) -> Result<(), NetErr> {
        if self.get(h)?.dgram {
            // A connected datagram socket only records its default peer.
            self.get(h)?.peer_addr = Some(*a);
            return Ok(());
        }
        // Already established -- this is the resumed call, and it succeeds.
        if self.get(h)?.peer.is_some() {
            return Ok(());
        }
        let li = self.find_listener(a).ok_or(NetErr::CONNREFUSED)?;
        // The listener gets a fresh handle for its end of the pair.
        let server_end = self.alloc(false);
        let local = self.socks[h.0 as usize]
            .bound
            .unwrap_or(SockAddr::v4([127, 0, 0, 1], 0));

        self.socks[server_end.0 as usize].peer = Some(h);
        self.socks[server_end.0 as usize].peer_addr = Some(local);
        self.socks[server_end.0 as usize].bound = Some(*a);
        self.socks[h.0 as usize].peer = Some(server_end);
        self.socks[h.0 as usize].peer_addr = Some(*a);
        self.socks[li].backlog.push_back(server_end);
        Ok(())
    }

    fn accept(&mut self, h: NetHandle) -> Result<NetHandle, NetErr> {
        let s = self.get(h)?;
        if !s.listening {
            return Err(NetErr::INVAL);
        }
        s.backlog.pop_front().ok_or(NetErr::AGAIN)
    }

    fn recv(&mut self, h: NetHandle, buf: &mut [u8]) -> Result<usize, NetErr> {
        let s = self.get(h)?;
        if s.rx.is_empty() {
            // EOF is a zero-length read, not an error: that is how a guest
            // learns the peer closed.
            return if s.eof { Ok(0) } else { Err(NetErr::AGAIN) };
        }
        let n = buf.len().min(s.rx.len());
        for slot in buf.iter_mut().take(n) {
            *slot = s.rx.pop_front().expect("checked non-empty");
        }
        Ok(n)
    }

    fn send(&mut self, h: NetHandle, buf: &[u8]) -> Result<usize, NetErr> {
        let peer = self.get(h)?.peer.ok_or(NetErr::NOTCONN)?;
        if self.socks.get(peer.0 as usize).map(|p| p.closed) != Some(false) {
            return Err(NetErr::PIPE);
        }
        self.socks[peer.0 as usize].rx.extend(buf.iter().copied());
        Ok(buf.len())
    }

    fn recv_from(
        &mut self,
        h: NetHandle,
        buf: &mut [u8],
    ) -> Result<(usize, Option<SockAddr>), NetErr> {
        let s = self.get(h)?;
        if !s.dgram {
            return self.recv(h, buf).map(|n| (n, None));
        }
        let (data, from) = s.dgrams.pop_front().ok_or(NetErr::AGAIN)?;
        // Datagrams keep their boundaries: one recv yields one message,
        // truncated to the caller's capacity.
        let n = buf.len().min(data.len());
        buf[..n].copy_from_slice(&data[..n]);
        Ok((n, Some(from)))
    }

    fn send_to(&mut self, h: NetHandle, buf: &[u8], a: &SockAddr) -> Result<usize, NetErr> {
        if !self.get(h)?.dgram {
            return self.send(h, buf);
        }
        let from = self.socks[h.0 as usize]
            .bound
            .unwrap_or(SockAddr::v4([127, 0, 0, 1], 0));
        let dst = self.find_bound_dgram(a).ok_or(NetErr::CONNREFUSED)?;
        self.socks[dst].dgrams.push_back((buf.to_vec(), from));
        Ok(buf.len())
    }

    fn addr(&mut self, h: NetHandle, peer: bool) -> Result<SockAddr, NetErr> {
        let s = self.get(h)?;
        if peer {
            s.peer_addr.ok_or(NetErr::NOTCONN)
        } else {
            s.bound.ok_or(NetErr::INVAL)
        }
    }

    fn setsockopt(
        &mut self,
        h: NetHandle,
        _level: i32,
        _name: i32,
        _val: &[u8],
    ) -> Result<(), NetErr> {
        self.get(h)?;
        Ok(())
    }

    fn getsockopt(
        &mut self,
        h: NetHandle,
        _level: i32,
        _name: i32,
        out: &mut [u8],
    ) -> Result<usize, NetErr> {
        self.get(h)?;
        let n = out.len().min(4);
        out[..n].fill(0);
        Ok(n)
    }

    fn shutdown(&mut self, h: NetHandle, _read: bool, write: bool) -> Result<(), NetErr> {
        let peer = self.get(h)?.peer;
        if write {
            if let Some(p) = peer {
                if let Some(ps) = self.socks.get_mut(p.0 as usize) {
                    ps.eof = true;
                }
            }
        }
        Ok(())
    }

    fn close(&mut self, h: NetHandle) {
        let peer = self.socks.get(h.0 as usize).and_then(|s| s.peer);
        if let Some(s) = self.socks.get_mut(h.0 as usize) {
            s.closed = true;
            s.rx.clear();
        }
        // The peer sees EOF, which is what makes a closed connection readable
        // rather than silently stalled.
        if let Some(p) = peer {
            if let Some(ps) = self.socks.get_mut(p.0 as usize) {
                ps.eof = true;
                ps.peer = None;
            }
        }
    }

    fn ready(&mut self, h: NetHandle, want_write: bool) -> Readiness {
        let Some(s) = self.socks.get(h.0 as usize) else {
            return Readiness::default();
        };
        if want_write {
            return Readiness {
                read: false,
                write: !s.closed,
            };
        }
        Readiness {
            // ⚠️ A listener is readable only when a connection is PENDING.
            // Reporting it perpetually readable is what made PostgreSQL's
            // postmaster call accept() on an empty backlog and then block
            // forever -- the ServerLoop deadlock.
            read: if s.listening {
                !s.backlog.is_empty()
            } else {
                !s.rx.is_empty() || !s.dgrams.is_empty() || s.eof
            },
            write: false,
        }
    }

    fn wait(&mut self, waiters: &[(NetHandle, bool)], timeout: Option<u128>) -> WaitOutcome {
        // ⚠️ THIS MUST ACTUALLY SLEEP when given a deadline. Socket readiness
        // here is in-process and cannot change while this runs, so it is
        // tempting to return immediately -- but the scheduler's idle path hands
        // this call the earliest GUEST TIMER as its timeout, and returning at
        // once turns every `nanosleep` into a spin at 100% CPU. The old
        // `wake_expired_deadlines` did this sleep; the seam moved the
        // responsibility here, where a non-blocking backend can decline it
        // instead.
        let ready: Vec<usize> = waiters
            .iter()
            .enumerate()
            .filter(|(_, (h, w))| {
                let r = self.ready(*h, *w);
                if *w {
                    r.write
                } else {
                    r.read
                }
            })
            .map(|(i, _)| i)
            .collect();
        if !ready.is_empty() {
            return WaitOutcome::Ready(ready);
        }
        // A re-entrant host cannot let this block at all -- see
        // `diag::nonblocking`. Declining is the browser backend's permanent
        // behaviour; here it is opt-in so the same code path can be exercised.
        if crate::diag::nonblocking() {
            return WaitOutcome::WouldBlock;
        }
        match timeout {
            // Nothing ready and a deadline pending: wait it out, then report a
            // timeout so the caller sweeps its deadlines and re-selects.
            Some(ns) => {
                std::thread::sleep(std::time::Duration::from_nanos(
                    ns.min(u64::MAX as u128) as u64
                ));
                WaitOutcome::TimedOut
            }
            // Nothing ready and no deadline. Sleeping would hang forever with no
            // diagnostic; returning lets the caller's deadlock detector fire
            // with one.
            None => WaitOutcome::TimedOut,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const LOCAL: [u8; 4] = [127, 0, 0, 1];

    fn listener(n: &mut LoopbackNet, port: u16) -> NetHandle {
        let h = n.socket(false, false).unwrap();
        n.bind(h, &SockAddr::v4(LOCAL, port)).unwrap();
        n.listen(h, 16).unwrap();
        h
    }

    #[test]
    fn accept_reports_would_block_on_an_empty_backlog() {
        let mut n = LoopbackNet::new();
        let ln = listener(&mut n, 80);
        assert_eq!(n.accept(ln), Err(NetErr::AGAIN));
    }

    #[test]
    fn a_listener_is_readable_only_with_a_pending_connection() {
        // The PostgreSQL ServerLoop deadlock: a listener reported perpetually
        // readable makes the guest accept() on an empty backlog and block.
        let mut n = LoopbackNet::new();
        let ln = listener(&mut n, 80);
        assert!(
            !n.ready(ln, false).read,
            "empty backlog must not be readable"
        );

        let cl = n.socket(false, false).unwrap();
        n.connect(cl, &SockAddr::v4(LOCAL, 80)).unwrap();
        assert!(
            n.ready(ln, false).read,
            "a pending connection must be readable"
        );

        n.accept(ln).unwrap();
        assert!(!n.ready(ln, false).read, "the backlog is drained again");
    }

    #[test]
    fn a_stream_round_trips_both_ways() {
        let mut n = LoopbackNet::new();
        let ln = listener(&mut n, 80);
        let cl = n.socket(false, false).unwrap();
        n.connect(cl, &SockAddr::v4(LOCAL, 80)).unwrap();
        let sv = n.accept(ln).unwrap();

        assert_eq!(n.send(cl, b"ping").unwrap(), 4);
        let mut buf = [0u8; 8];
        assert_eq!(n.recv(sv, &mut buf).unwrap(), 4);
        assert_eq!(&buf[..4], b"ping");

        assert_eq!(n.send(sv, b"pong").unwrap(), 4);
        assert_eq!(n.recv(cl, &mut buf).unwrap(), 4);
        assert_eq!(&buf[..4], b"pong");
    }

    #[test]
    fn an_empty_connection_would_block_but_a_closed_one_is_eof() {
        let mut n = LoopbackNet::new();
        let ln = listener(&mut n, 80);
        let cl = n.socket(false, false).unwrap();
        n.connect(cl, &SockAddr::v4(LOCAL, 80)).unwrap();
        let sv = n.accept(ln).unwrap();

        let mut buf = [0u8; 8];
        assert_eq!(n.recv(sv, &mut buf), Err(NetErr::AGAIN));
        n.close(cl);
        // EOF is Ok(0), NOT an error: the two are different answers and a guest
        // acts on them differently.
        assert_eq!(n.recv(sv, &mut buf), Ok(0));
    }

    #[test]
    fn connect_to_an_unbound_port_is_refused_not_pending() {
        let mut n = LoopbackNet::new();
        let cl = n.socket(false, false).unwrap();
        let e = n.connect(cl, &SockAddr::v4(LOCAL, 9)).unwrap_err();
        assert_eq!(e, NetErr::CONNREFUSED);
        assert!(
            !e.is_in_progress(),
            "a refusal must not look resumable, or the caller retries forever"
        );
    }

    #[test]
    fn a_short_recv_buffer_takes_a_prefix_and_leaves_the_rest() {
        let mut n = LoopbackNet::new();
        let ln = listener(&mut n, 80);
        let cl = n.socket(false, false).unwrap();
        n.connect(cl, &SockAddr::v4(LOCAL, 80)).unwrap();
        let sv = n.accept(ln).unwrap();
        n.send(cl, b"abcdef").unwrap();

        let mut small = [0u8; 2];
        assert_eq!(n.recv(sv, &mut small).unwrap(), 2);
        assert_eq!(&small, b"ab");
        let mut rest = [0u8; 8];
        assert_eq!(n.recv(sv, &mut rest).unwrap(), 4);
        assert_eq!(&rest[..4], b"cdef");
    }

    #[test]
    fn datagrams_keep_their_boundaries_and_report_a_source() {
        let mut n = LoopbackNet::new();
        let a = n.socket(false, true).unwrap();
        n.bind(a, &SockAddr::v4(LOCAL, 53)).unwrap();
        let b = n.socket(false, true).unwrap();
        n.bind(b, &SockAddr::v4(LOCAL, 5300)).unwrap();

        n.send_to(b, b"one", &SockAddr::v4(LOCAL, 53)).unwrap();
        n.send_to(b, b"two", &SockAddr::v4(LOCAL, 53)).unwrap();

        let mut buf = [0u8; 16];
        let (n1, from) = n.recv_from(a, &mut buf).unwrap();
        // Two datagrams must not coalesce into one read, the way stream bytes do.
        assert_eq!(&buf[..n1], b"one");
        assert_eq!(from.unwrap().port, 5300);
        let (n2, _) = n.recv_from(a, &mut buf).unwrap();
        assert_eq!(&buf[..n2], b"two");
    }

    // `bind` to port 0 means "choose one for me".
    //
    // ⚠️ Every other test in this file names a port, which is precisely why
    // this went unnoticed until an E2E guest bound 0 to avoid colliding with
    // another profile's run.
    #[test]
    fn binding_port_zero_assigns_a_real_one() {
        let mut n = LoopbackNet::new();
        let h = n.socket(false, false).unwrap();
        n.bind(h, &SockAddr::v4(LOCAL, 0)).unwrap();

        let got = n.addr(h, false).unwrap();
        assert_ne!(
            got.port, 0,
            "getsockname after bind(0) must report the port that was chosen, \
             not the 0 that asked for one"
        );
        assert!(
            (32768..=60999).contains(&got.port),
            "the chosen port must look ephemeral to a guest, got {}",
            got.port
        );
    }

    // The half that a "return any non-zero number" fix would fail.
    //
    // Two sockets both binding 0 must get DIFFERENT ports. Before this,
    // both were bound to 0 and `find_listener` matched the first for both --
    // so a connect intended for the second server reached the first, with no
    // error anywhere.
    #[test]
    fn two_ephemeral_binds_do_not_collide() {
        let mut n = LoopbackNet::new();
        let a = n.socket(false, false).unwrap();
        n.bind(a, &SockAddr::v4(LOCAL, 0)).unwrap();
        n.listen(a, 4).unwrap();
        let b = n.socket(false, false).unwrap();
        n.bind(b, &SockAddr::v4(LOCAL, 0)).unwrap();
        n.listen(b, 4).unwrap();

        let pa = n.addr(a, false).unwrap().port;
        let pb = n.addr(b, false).unwrap().port;
        assert_ne!(pa, pb, "two ephemeral binds must not share a port");

        // And the ports must actually ROUTE, which is the property the shared
        // port silently destroyed: a connect to B must land on B's backlog and
        // leave A's empty.
        let cl = n.socket(false, false).unwrap();
        n.connect(cl, &SockAddr::v4(LOCAL, pb)).unwrap();
        assert!(
            n.ready(b, false).read,
            "the connection must reach the server that owns the port"
        );
        assert!(
            !n.ready(a, false).read,
            "and must NOT reach the other one -- this is the cross-connect that \
             two sockets bound to port 0 used to produce"
        );
    }

    // An explicit port must still be honoured exactly. A fix that assigned an
    // ephemeral port unconditionally would pass both tests above and break
    // every server that has a port in mind, which is most of them.
    #[test]
    fn an_explicit_port_is_left_alone() {
        let mut n = LoopbackNet::new();
        let h = n.socket(false, false).unwrap();
        n.bind(h, &SockAddr::v4(LOCAL, 8080)).unwrap();
        assert_eq!(n.addr(h, false).unwrap().port, 8080);
    }
}
