import { E } from '../wasi/abi.ts';
import type { SockAddr, SocketBackend } from '../wasi/sockets.ts';

/**
 * A `SocketBackend` that carries TCP over one multiplexed WebSocket.
 *
 * # Why a relay and not `fetch`
 *
 * ⚠️ `fetch` cannot carry a guest's connection, and the reason is CORS rather
 * than TLS. A cross-origin request to a server that has not opted in rejects
 * with an opaque `TypeError` -- deliberately indistinguishable from DNS failure
 * or connection refused, so it cannot even be reported accurately. `no-cors`
 * resolves with `status === 0`, no headers and no body. `Set-Cookie` is never
 * exposed to JS at any setting, so a guest with its own cookie jar cannot work.
 *
 * Terminating TLS inside the runtime does not help: it converts "TLS is opaque"
 * into "CORS blocks it", at the cost of a TLS stack in wasm and a CA in the
 * guest's trust store. A relay keeps TLS genuinely end to end -- cert pinning,
 * client certs and ALPN all survive -- and CORS never applies to a WebSocket.
 *
 * # One socket, many streams
 *
 * A WebSocket per connection would be simpler and wrong: browsers cap
 * concurrent sockets, each costs a handshake, and inbound reachability and UDP
 * would each need their own scheme later. One multiplexed subprotocol carries
 * all of it and rides WebTransport unchanged when that is wanted.
 *
 * # The synchronous illusion
 *
 * The ABI is synchronous -- `recv` returns bytes or EAGAIN, now -- while a
 * WebSocket is not. So every method here answers from a QUEUE that the socket's
 * own event handlers fill, and readiness is pushed to the runtime separately.
 * Nothing in this file ever waits.
 */

/** Wire opcodes. A byte, then a u16 stream id, then opcode-specific bytes. */
const OP = {
  OPEN: 1,
  OPEN_OK: 2,
  OPEN_ERR: 3,
  DATA: 4,
  SHUTDOWN: 5,
  CLOSE: 6,
} as const;

const HDR = 3;

type Stream = {
  id: number;
  rx: Uint8Array[];
  rxLen: number;
  opening: boolean;
  open: boolean;
  eof: boolean;
  err?: number;
  /** Bytes handed to the socket but not yet flushed, for backpressure. */
  writable: boolean;
};

export interface RelayOptions {
  url: string;
  /** Called when a handle's readiness may have changed. */
  notify: (handle: number, events: number) => void;
  /** Handles are allocated from here up, above the file-descriptor range. */
  handleBase: number;
  /**
   * Bytes buffered in the WebSocket before `send` reports EAGAIN.
   *
   * ⚠️ `bufferedAmount` is the ONLY backpressure signal a WebSocket offers. A
   * relay that ignores it buffers without bound and turns a slow link into
   * memory growth; the ABI already expresses the alternative natively, so this
   * costs nothing but has to be remembered.
   */
  watermark?: number;
}

const READY_READ = 1;
const READY_WRITE = 2;

export class RelaySockets implements SocketBackend {
  private ws?: WebSocket;
  private readonly streams = new Map<number, Stream>();
  /**
   * Datagram handles, which exist and carry nothing.
   *
   * ⚠️ REFUSING `SOCK_DGRAM` OUTRIGHT BREAKS DNS, which is not obvious because
   * the DNS tap lives in the RUNTIME, not here. `BrowserNet::socket` calls
   * `net_socket` for a datagram socket like any other, and only afterwards does
   * the tap intercept the query -- `connect`, `send` and `recv` for a resolver
   * never reach this class at all. So the host's sole job is to hand back a
   * handle. Returning ENOTSUP instead made `getaddrinfo` fail before the tap
   * could ever see a query, which reads as "no nameserver" rather than as a
   * refusal here.
   */
  private readonly dgrams = new Set<number>();
  private nextHandle: number;
  private readonly opts: RelayOptions;
  private readonly watermark: number;
  private connected = false;

  constructor(opts: RelayOptions) {
    this.opts = opts;
    this.nextHandle = opts.handleBase;
    this.watermark = opts.watermark ?? 1 << 20;
  }

  /** Opens the relay socket. Resolves once it is usable. */
  start(): Promise<void> {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(this.opts.url);
      ws.binaryType = 'arraybuffer';
      this.ws = ws;
      ws.onopen = () => {
        this.connected = true;
        resolve();
      };
      ws.onerror = () => reject(new Error(`relay ${this.opts.url} failed to open`));
      ws.onclose = () => {
        this.connected = false;
        // Every stream is now dead. Reporting EOF rather than silence is what
        // lets a guest notice; a stalled read would just hang.
        for (const s of this.streams.values()) {
          s.eof = true;
          s.open = false;
          this.opts.notify(s.id, READY_READ | READY_WRITE);
        }
      };
      ws.onmessage = (ev) => this.onFrame(new Uint8Array(ev.data as ArrayBuffer));
    });
  }

  stop(): void {
    this.ws?.close();
  }

  private onFrame(f: Uint8Array): void {
    if (f.length < HDR) return;
    const op = f[0]!;
    const id = (f[1]! << 8) | f[2]!;
    const s = this.streams.get(id);
    if (!s) return;
    switch (op) {
      case OP.OPEN_OK:
        s.opening = false;
        s.open = true;
        s.writable = true;
        this.opts.notify(id, READY_WRITE);
        break;
      case OP.OPEN_ERR: {
        // ⚠️ The connect error must arrive IN BAND. The WebSocket opens fine
        // even when the far-side TCP connect is refused, so there is no
        // transport-level signal to read it from -- and collapsing it would
        // destroy the ECONNREFUSED-versus-timeout distinction the guest acts on.
        s.opening = false;
        s.err = f.length > HDR ? f[HDR]! : E.CONNREFUSED;
        this.opts.notify(id, READY_READ | READY_WRITE);
        break;
      }
      case OP.DATA:
        // ⚠️ Message boundaries are NOT record boundaries. WebSocket is
        // message-framed and TCP is a byte stream, so a frame is a fragment and
        // nothing may attribute meaning to where it ends.
        s.rx.push(f.subarray(HDR));
        s.rxLen += f.length - HDR;
        this.opts.notify(id, READY_READ);
        break;
      case OP.CLOSE:
      case OP.SHUTDOWN:
        s.eof = true;
        this.opts.notify(id, READY_READ);
        break;
    }
  }

  private frame(op: number, id: number, body?: Uint8Array): void {
    const out = new Uint8Array(HDR + (body?.length ?? 0));
    out[0] = op;
    out[1] = (id >> 8) & 0xff;
    out[2] = id & 0xff;
    if (body) out.set(body, HDR);
    this.ws?.send(out);
  }

  private get(h: number): Stream | undefined {
    return this.streams.get(h);
  }

  open(_v6: boolean, dgram: boolean): { errno: number; handle: number } {
    // ⚠️ THE STREAM ID IS u16 ON THE WIRE, so a handle beyond 0xFFFF is not a
    // large number -- it is a DIFFERENT stream. A base of 2_000_000 once made
    // every OPEN arrive as id 33920 (2_000_000 & 0xFFFF): the relay connected
    // and answered about 33920 while this side had filed the stream under
    // 2_000_000, so every reply was dropped and the guest hung with a
    // successful connection sitting in the log.
    //
    // Refused rather than truncated. Ids are also never recycled, so this is
    // additionally the ceiling on connections for one relay socket.
    if (this.nextHandle > 0xffff) return { errno: E.MFILE, handle: 0 };

    if (dgram) {
      // A handle and nothing else. See `dgrams` above: the runtime answers
      // resolver traffic itself, and anything else is refused at first use
      // rather than silently dropped -- a dropped datagram is a hang.
      const d = this.nextHandle++;
      this.dgrams.add(d);
      return { errno: E.SUCCESS, handle: d };
    }
    const id = this.nextHandle++;
    this.streams.set(id, {
      id,
      rx: [],
      rxLen: 0,
      opening: false,
      open: false,
      eof: false,
      writable: false,
    });
    return { errno: E.SUCCESS, handle: id };
  }

  bind(): number {
    // Inbound needs the relay to own a public port and multiplex it back down
    // the tunnel -- LISTEN/ACCEPT opcodes, not yet defined.
    return E.NOTSUP;
  }

  listen(): number {
    return E.NOTSUP;
  }

  connect(h: number, a: SockAddr): number {
    // The runtime short-circuits `connect` on a datagram handle and never calls
    // through, so this arm is for completeness rather than for the DNS path.
    if (this.dgrams.has(h)) return E.SUCCESS;
    const s = this.get(h);
    if (!s) return E.BADF;
    if (s.err !== undefined) {
      const e = s.err;
      s.err = undefined;
      return e;
    }
    if (s.open) return E.SUCCESS;
    if (s.opening) return E.INPROGRESS;
    if (!this.connected) return E.NOTCONN;
    // `a.ip` is a HOSTNAME when the address pool reversed a synthetic address,
    // which is the normal case; the relay resolves it on the far side.
    const body = new TextEncoder().encode(`${a.ip}:${a.port}`);
    s.opening = true;
    this.frame(OP.OPEN, h, body);
    // ⚠️ Never SUCCESS on the first call. `sys_connect` treats a resumed
    // in-progress connect as complete, so reporting success here would make a
    // refused connection look established.
    return E.INPROGRESS;
  }

  accept(): { errno: number; handle: number } {
    return { errno: E.NOTSUP, handle: 0 };
  }

  recv(h: number, max: number): { errno: number; data: Uint8Array } {
    if (this.dgrams.has(h)) return { errno: E.NOTSUP, data: new Uint8Array(0) };
    const s = this.get(h);
    if (!s) return { errno: E.BADF, data: new Uint8Array(0) };
    if (s.rxLen === 0) {
      if (s.err !== undefined) {
        const e = s.err;
        s.err = undefined;
        return { errno: e, data: new Uint8Array(0) };
      }
      // EOF is zero bytes with no error; that is how a guest learns the peer
      // closed, and it is a different answer from "nothing yet".
      return s.eof
        ? { errno: E.SUCCESS, data: new Uint8Array(0) }
        : { errno: E.AGAIN, data: new Uint8Array(0) };
    }
    const out = new Uint8Array(Math.min(max, s.rxLen));
    let off = 0;
    while (off < out.length && s.rx.length > 0) {
      const head = s.rx[0]!;
      const take = Math.min(out.length - off, head.length);
      out.set(head.subarray(0, take), off);
      off += take;
      if (take === head.length) s.rx.shift();
      else s.rx[0] = head.subarray(take);
      s.rxLen -= take;
    }
    return { errno: E.SUCCESS, data: out };
  }

  recvFrom(h: number, max: number): { errno: number; data: Uint8Array; from?: SockAddr } {
    return this.recv(h, max);
  }

  send(h: number, data: Uint8Array): { errno: number; sent: number } {
    // Only reached for a datagram the tap did NOT answer, i.e. not DNS. The
    // relay speaks TCP; carrying this would need UDP opcodes.
    if (this.dgrams.has(h)) return { errno: E.NOTSUP, sent: 0 };
    const s = this.get(h);
    if (!s) return { errno: E.BADF, sent: 0 };
    if (s.err !== undefined) {
      const e = s.err;
      s.err = undefined;
      return { errno: e, sent: 0 };
    }
    if (!s.open) return { errno: s.opening ? E.AGAIN : E.NOTCONN, sent: 0 };
    if ((this.ws?.bufferedAmount ?? 0) > this.watermark) {
      s.writable = false;
      return { errno: E.AGAIN, sent: 0 };
    }
    this.frame(OP.DATA, h, data);
    return { errno: E.SUCCESS, sent: data.length };
  }

  addr(h: number): { errno: number; addr?: SockAddr } {
    return this.get(h)
      ? { errno: E.SUCCESS, addr: { ip: '0.0.0.0', port: 0, v6: false } }
      : { errno: E.BADF };
  }

  getsockopt(): { errno: number; value: number } {
    // Zero, never an error: libpq queries SO_ERROR on every connection and
    // reads a failure as "could not get socket error status".
    return { errno: E.SUCCESS, value: 0 };
  }

  setsockopt(): number {
    return E.SUCCESS;
  }

  shutdown(h: number, how: number): number {
    const s = this.get(h);
    if (!s) return E.BADF;
    // Half-close has no WebSocket equivalent, so it needs its own opcode.
    this.frame(OP.SHUTDOWN, h, new Uint8Array([how & 0xff]));
    return E.SUCCESS;
  }

  close(h: number): number {
    if (this.dgrams.delete(h)) return E.SUCCESS;
    if (!this.streams.delete(h)) return E.BADF;
    this.frame(OP.CLOSE, h);
    return E.SUCCESS;
  }

  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>): number[] {
    // NEVER waits: readiness arrives through the socket's own handlers, and the
    // guest is resumed by the driver rather than by a return from here.
    const hit: number[] = [];
    subs.forEach((sub, i) => {
      const s = this.get(sub.fd);
      if (!s) return;
      const ready = sub.write
        ? s.err !== undefined || (s.open && s.writable)
        : s.rxLen > 0 || s.eof || s.err !== undefined;
      if (ready) hit.push(i);
    });
    return hit;
  }

  liveHandles(): number[] {
    return [...this.streams.keys()];
  }
}
