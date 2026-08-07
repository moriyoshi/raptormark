/**
 * One guest that both SERVES and DIALS.
 *
 * Inbound and egress are different transports in a browser -- a service worker
 * minting connections on one side, a WebSocket relay carrying them on the other
 * -- and a guest doing both needs its sockets routed to whichever one applies.
 *
 * # ⚠️ THE HARD PART IS THAT `socket()` DOES NOT SAY WHICH DIRECTION IT IS
 *
 * `socket(AF_INET, SOCK_STREAM, 0)` is identical for a server and a client. The
 * direction is only revealed later, by `listen` or by `connect` -- and `bind`
 * comes BEFORE either and belongs to both (`bind` then `listen` is a server,
 * `bind` then `connect` is source-address selection). So a socket cannot be
 * assigned to a backend when it is created.
 *
 * This defers instead: `open` mints an UNDECIDED handle, `bind` and `setsockopt`
 * are recorded, and the first `listen` or `connect` materializes the socket in
 * the matching backend and replays what was recorded. Guessing at `open` time
 * -- by socket type, by a flag, by which backend exists -- cannot be made
 * correct, because the information genuinely is not there yet.
 *
 * ⚠️ Handles the guest sees are OUTER handles, minted here. They are not the
 * sub-backend's handles, and the two must never be confused: a notification
 * arriving from a sub-backend carries an INNER handle and has to be translated
 * before it reaches the runtime, or the guest is told that some unrelated
 * socket became ready.
 */

import { E } from '../wasi/abi.ts';
import type { SockAddr, SocketBackend } from '../wasi/sockets.ts';

type Slot = {
  v6: boolean;
  dgram: boolean;
  /** Which backend owns this socket, once that is known. */
  side?: 'listen' | 'dial';
  inner?: number;
  /** Recorded before the direction is known, replayed on materialization. */
  bind?: SockAddr;
  opts: Array<[number, number, number]>;
};

export interface CompositeOptions<L extends SocketBackend, D extends SocketBackend> {
  /** Outer handles are allocated from here up, above the file-descriptor range. */
  handleBase: number;
  /** Records an OUTER handle whose readiness may have changed. */
  notify: (handle: number, events: number) => void;
  /**
   * Builds the backend that owns listening sockets.
   *
   * ⚠️ A FACTORY, not an instance, so the sub-backend's `notify` is wired to the
   * translating one by construction. Handing in a pre-built backend would let a
   * caller wire it to the runtime directly, and inner handles would reach the
   * guest as if they were its own -- silently, and only for sockets that ever
   * become ready.
   */
  listen: (notify: (handle: number, events: number) => void) => L;
  /** Builds the backend that owns dialled sockets. Omit for inbound only. */
  dial?: (notify: (handle: number, events: number) => void) => D;
}

export class CompositeSockets<
  L extends SocketBackend = SocketBackend,
  D extends SocketBackend = SocketBackend,
> implements SocketBackend {
  /** The backend owning listening sockets. */
  readonly listener: L;
  /** The backend owning dialled sockets, if one was configured. */
  readonly dialer?: D;

  private readonly slots = new Map<number, Slot>();
  // ⚠️ ONE MAP PER SIDE. The two backends allocate independently and their
  // handles WILL collide numerically; a single inner->outer map would route a
  // relay stream to an inbound connection that happens to share its number.
  private readonly fromListen = new Map<number, number>();
  private readonly fromDial = new Map<number, number>();
  private next: number;
  private readonly notify: (handle: number, events: number) => void;

  constructor(opts: CompositeOptions<L, D>) {
    this.next = opts.handleBase;
    this.notify = opts.notify;
    this.listener = opts.listen((h, ev) => this.report(this.fromListen, h, ev));
    this.dialer = opts.dial?.((h, ev) => this.report(this.fromDial, h, ev));
  }

  private report(map: Map<number, number>, inner: number, events: number): void {
    const outer = map.get(inner);
    // A notification for a handle that has been closed is dropped rather than
    // forwarded under its raw inner number, which would name a live socket.
    if (outer !== undefined) this.notify(outer, events);
  }

  private slot(h: number): Slot | undefined {
    return this.slots.get(h);
  }

  private backendOf(s: Slot): SocketBackend | undefined {
    if (s.side === 'listen') return this.listener;
    if (s.side === 'dial') return this.dialer;
    return undefined;
  }

  /** Resolves an outer handle to the backend and inner handle that own it. */
  private route(h: number): { b: SocketBackend; inner: number } | undefined {
    const s = this.slot(h);
    if (!s || s.inner === undefined) return undefined;
    const b = this.backendOf(s);
    if (!b) return undefined;
    return { b, inner: s.inner };
  }

  open(v6: boolean, dgram: boolean): { errno: number; handle: number } {
    const h = this.next++;
    // ⚠️ NEITHER BACKEND IS TOUCHED YET. A datagram socket is never delegated at
    // all: the DNS tap answers it inside the runtime, and the host's only job is
    // that a handle exists. See `relay.ts` for what refusing it costs.
    this.slots.set(h, { v6, dgram, opts: [] });
    return { errno: E.SUCCESS, handle: h };
  }

  bind(h: number, a: SockAddr): number {
    const s = this.slot(h);
    if (!s) return E.BADF;
    if (s.inner !== undefined) return this.backendOf(s)!.bind(s.inner, a);
    // ⚠️ RECORDED, NOT PERFORMED, and reported as success. The direction is
    // still unknown, so there is no backend to bind on. A server's bind is
    // replayed by `listen` below.
    //
    // A client's is DROPPED, which is a real limitation stated plainly: over a
    // relay the source address is the relay's, so a guest-chosen source port
    // cannot be honoured by anything. Failing instead would break every client
    // that harmlessly binds `0.0.0.0:0` first, which is common.
    s.bind = a;
    return E.SUCCESS;
  }

  listen(h: number, backlog: number): number {
    const s = this.slot(h);
    if (!s) return E.BADF;
    if (s.side === 'dial') return E.INVAL;
    if (s.inner === undefined) {
      const e = this.materialize(s, h, 'listen');
      if (e !== E.SUCCESS) return e;
    }
    return this.listener.listen(s.inner!, backlog);
  }

  connect(h: number, a: SockAddr): number {
    const s = this.slot(h);
    if (!s) return E.BADF;
    if (s.side === 'listen') return E.INVAL;
    if (!this.dialer) {
      // No egress transport was configured. Refused loudly: a `connect` that
      // went nowhere would look like an unreachable destination.
      return E.NOTSUP;
    }
    if (s.inner === undefined) {
      const e = this.materialize(s, h, 'dial');
      if (e !== E.SUCCESS) return e;
    }
    return this.dialer.connect(s.inner!, a);
  }

  /** Creates the socket in the backend the direction has now revealed. */
  private materialize(s: Slot, outer: number, side: 'listen' | 'dial'): number {
    const b = side === 'listen' ? this.listener : this.dialer;
    if (!b) return E.NOTSUP;
    const r = b.open(s.v6, s.dgram);
    if (r.errno !== E.SUCCESS) return r.errno;
    s.side = side;
    s.inner = r.handle;
    (side === 'listen' ? this.fromListen : this.fromDial).set(r.handle, outer);

    // Replay what was recorded while the direction was unknown, in the order the
    // guest issued it: options first, then the bind they were meant to affect.
    for (const [level, name, value] of s.opts) b.setsockopt(r.handle, level, name, value);
    if (s.bind && side === 'listen') {
      const e = b.bind(r.handle, s.bind);
      if (e !== E.SUCCESS) return e;
    }
    return E.SUCCESS;
  }

  accept(h: number): { errno: number; handle: number } {
    const r = this.route(h);
    if (!r) return { errno: E.BADF, handle: 0 };
    const a = r.b.accept(r.inner);
    if (a.errno !== E.SUCCESS) return a;
    // ⚠️ AN ACCEPTED SOCKET NEEDS AN OUTER HANDLE TOO. Returning the inner one
    // would hand the guest a number from the sub-backend's space -- which may
    // collide with a live outer handle, and which every later call would then
    // route wrongly.
    const outer = this.next++;
    const side = this.slot(h)!.side!;
    this.slots.set(outer, { v6: false, dgram: false, side, inner: a.handle, opts: [] });
    (side === 'listen' ? this.fromListen : this.fromDial).set(a.handle, outer);
    return { errno: E.SUCCESS, handle: outer };
  }

  recv(h: number, max: number): { errno: number; data: Uint8Array } {
    const r = this.route(h);
    if (!r) return { errno: this.undecided(h), data: new Uint8Array(0) };
    return r.b.recv(r.inner, max);
  }

  recvFrom(h: number, max: number): { errno: number; data: Uint8Array; from?: SockAddr } {
    const r = this.route(h);
    if (!r) return { errno: this.undecided(h), data: new Uint8Array(0) };
    return r.b.recvFrom(r.inner, max);
  }

  send(h: number, data: Uint8Array, to?: SockAddr): { errno: number; sent: number } {
    const r = this.route(h);
    if (!r) return { errno: this.undecided(h), sent: 0 };
    return r.b.send(r.inner, data, to);
  }

  addr(h: number, peer: boolean): { errno: number; addr?: SockAddr } {
    const r = this.route(h);
    if (!r) {
      const s = this.slot(h);
      if (!s) return { errno: E.BADF };
      // Undecided: the only address it has is the one it asked to bind.
      return { errno: E.SUCCESS, addr: s.bind };
    }
    return r.b.addr(r.inner, peer);
  }

  getsockopt(h: number, level: number, name: number): { errno: number; value: number } {
    const r = this.route(h);
    if (!r) return { errno: this.undecided(h), value: 0 };
    return r.b.getsockopt(r.inner, level, name);
  }

  setsockopt(h: number, level: number, name: number, value: number): number {
    const s = this.slot(h);
    if (!s) return E.BADF;
    if (s.inner !== undefined) return this.backendOf(s)!.setsockopt(s.inner, level, name, value);
    // Recorded so `SO_REUSEADDR`, which a server sets BEFORE it binds and
    // therefore before the direction is known, still reaches the backend.
    s.opts.push([level, name, value]);
    return E.SUCCESS;
  }

  shutdown(h: number, how: number): number {
    const r = this.route(h);
    if (!r) return this.undecided(h);
    return r.b.shutdown(r.inner, how);
  }

  close(h: number): number {
    const s = this.slot(h);
    if (!s) return E.BADF;
    this.slots.delete(h);
    if (s.inner === undefined) return E.SUCCESS;
    (s.side === 'listen' ? this.fromListen : this.fromDial).delete(s.inner);
    return this.backendOf(s)!.close(s.inner);
  }

  /**
   * Readiness across both backends.
   *
   * ⚠️ THE TIMEOUT IS IGNORED, as it is in both sub-backends: no browser
   * transport may block, and waiting on one backend would starve the other
   * regardless. The driver resumes the guest; this only reports.
   */
  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>, _timeoutMs: number): number[] {
    const parts = new Map<
      SocketBackend,
      { subs: Array<{ fd: number; write: boolean }>; origin: number[] }
    >();
    subs.forEach((sub, i) => {
      const r = this.route(sub.fd);
      if (!r) return;
      let p = parts.get(r.b);
      if (!p) {
        p = { subs: [], origin: [] };
        parts.set(r.b, p);
      }
      p.subs.push({ fd: r.inner, write: sub.write });
      p.origin.push(i);
    });

    const hits: number[] = [];
    for (const [b, p] of parts) {
      // ⚠️ INDICES COME BACK RELATIVE TO THE SLICE THAT WAS SENT, so they have
      // to be mapped through `origin`. Returning them directly reports readiness
      // for whichever subscription happens to sit at that index in the caller's
      // array -- wrong, and only when both backends have live sockets.
      for (const i of b.wait(p.subs, 0)) {
        const o = p.origin[i];
        if (o !== undefined) hits.push(o);
      }
    }
    return hits.sort((a, b) => a - b);
  }

  liveHandles(): number[] {
    const out: number[] = [];
    for (const [outer, s] of this.slots) {
      if (s.inner !== undefined) out.push(outer);
    }
    return out;
  }

  /** `EBADF` for an unknown handle, `ENOTCONN` for one with no direction yet. */
  private undecided(h: number): number {
    return this.slots.has(h) ? E.NOTCONN : E.BADF;
  }
}
