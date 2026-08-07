/**
 * Inbound: serving a browser request from a socket inside the guest.
 *
 * The guest is an ordinary TCP server. It binds, listens and accepts, and it
 * has no idea that the "connections" it accepts were minted by a service worker
 * intercepting `fetch` rather than arriving from a network. That is the whole
 * design: a real server keeps working unmodified, which is what makes "nginx in
 * a tab" a transport problem rather than a porting problem.
 *
 * ⚠️ THE HOST SPEAKS HTTP/1.1 ON THE WIRE, not a structured RPC. A `Request` is
 * serialized to request bytes and the guest's response bytes are parsed back
 * into a `Response`. Handing the guest a pre-parsed request would make this
 * demo work and every real server fail, because a real server does its own
 * parsing and its behaviour depends on the exact bytes.
 *
 * Nothing here touches the network. The request never leaves the browser
 * process, so this path works fully offline and needs no relay.
 */

import { E } from '../wasi/abi.ts';
import type { SockAddr, SocketBackend } from '../wasi/sockets.ts';
import { ResponseFramer, decodeChunked, indexOfCRLFCRLF } from './framing.ts';

// Only READ is ever notified from here. Writability is answered by `wait`,
// which reports a host-side buffer as always writable, so there is no event to
// push -- see the note on `notify` in `deliver`.
const READY_READ = 1;

type Conn = {
  /** Request bytes still to be handed to the guest. */
  rx: Uint8Array;
  rxAt: number;
  /**
   * Accumulates the response and decides when it has ENDED.
   *
   * ⚠️ Not a byte list waiting for a close. See `framing.ts`: a server that
   * keeps the connection open -- the HTTP/1.1 default, nginx included -- answers
   * correctly and then waits, so closing is not the signal.
   */
  framer: ResponseFramer;
  /** The response has been handed back; nothing more is expected. */
  done: boolean;
  /** Resolves once the response is complete. */
  finish: (bytes: Uint8Array) => void;
};

type Listener = {
  port: number;
  /** Connections accepted by the host but not yet by the guest. */
  backlog: Conn[];
};

export interface InboundOptions {
  /** Handles are allocated from here up, above the file-descriptor range. */
  handleBase: number;
  /** Records a handle whose readiness may have changed. */
  notify: (handle: number, events: number) => void;
}

/**
 * A `SocketBackend` whose connections come from the host rather than a network.
 *
 * ⚠️ INBOUND ONLY. `connect` is refused: a guest that both serves and dials
 * needs a composite backend routing by handle, which is easy and is not this.
 * Refusing loudly is better than a `connect` that silently goes nowhere.
 */
export class InboundSockets implements SocketBackend {
  private readonly listeners = new Map<number, Listener>();
  private readonly conns = new Map<number, Conn>();
  private readonly dgrams = new Set<number>();
  private readonly ports = new Map<number, number>();
  private next: number;
  private readonly opts: InboundOptions;

  constructor(opts: InboundOptions) {
    this.opts = opts;
    this.next = opts.handleBase;
  }

  /** True once the guest is listening on `port`, so requests can be delivered. */
  listeningOn(port: number): boolean {
    for (const l of this.listeners.values()) if (l.port === port) return true;
    return false;
  }

  /**
   * Hands `request` to the guest and resolves with its raw response bytes.
   *
   * Settles as soon as the response is FRAMED -- `Content-Length` satisfied, the
   * terminal chunk seen, or the guest closing when neither applies. A caller
   * must still apply its own timeout: a guest that never answers is a guest bug,
   * and hanging forever here would present it as a hung page.
   */
  deliver(port: number, request: Uint8Array, method = 'GET'): Promise<Uint8Array> {
    const l = [...this.listeners.entries()].find(([, v]) => v.port === port);
    if (!l) return Promise.reject(new Error(`nothing is listening on port ${port}`));
    return new Promise((resolve) => {
      const conn: Conn = {
        rx: request,
        rxAt: 0,
        // ⚠️ The METHOD is needed here, not just the bytes: a `HEAD` response
        // carries a `Content-Length` describing a body that does not exist, so
        // framing by that length waits forever.
        framer: new ResponseFramer(method),
        done: false,
        finish: resolve,
      };
      l[1].backlog.push(conn);
      // The listener is now readable.
      //
      // ⚠️ THIS IS LATENCY, NOT CORRECTNESS -- measured, and it contradicts what
      // this comment first claimed. `accept` returning EAGAIN does clear the
      // read bit in the runtime, so the reasoning "without this the guest parks
      // forever" looks sound. It is wrong: the driver's idle path calls
      // `wait()` on every `liveHandles()` entry each pass, so it discovers the
      // backlog by itself within one poll interval. DELETING THIS LINE CHANGES
      // NOTHING THAT ANY TEST CAN SEE.
      //
      // It is kept because it removes up to one poll interval from every
      // request, and because a backend without `liveHandles` would genuinely
      // need it. But it must not be cited as the mechanism that makes inbound
      // work -- the poll is.
      this.opts.notify(l[0], READY_READ);
    });
  }

  open(_v6: boolean, dgram: boolean): { errno: number; handle: number } {
    const h = this.next++;
    // A datagram handle that carries nothing. See `relay.ts`: the DNS tap lives
    // in the runtime and needs the socket to EXIST before it can intercept, so
    // refusing here breaks `getaddrinfo` from a layer away.
    if (dgram) this.dgrams.add(h);
    return { errno: E.SUCCESS, handle: h };
  }

  bind(h: number, a: SockAddr): number {
    this.ports.set(h, a.port);
    return E.SUCCESS;
  }

  listen(h: number, _backlog: number): number {
    const port = this.ports.get(h);
    if (port === undefined) {
      // Listening on an unbound socket would mean picking a port the host
      // invented, and the caller could never learn which one.
      return E.INVAL;
    }
    // ⚠️ A REPEAT `listen` MUST KEEP THE BACKLOG. Linux's `listen(2)` on an
    // already-listening socket adjusts the backlog and nothing else; it does not
    // discard queued connections. `set(h, { backlog: [] })` unconditionally
    // would, and nginx re-listens on every startup
    // (`ngx_configure_listening_sockets`), so anything that arrived in between
    // would vanish. It has not bitten here only because the second call lands
    // during boot, before a request can exist -- see the Node backend, where the
    // same double-listen was catastrophic.
    if (!this.listeners.has(h)) this.listeners.set(h, { port, backlog: [] });
    return E.SUCCESS;
  }

  connect(): number {
    return E.NOTSUP;
  }

  accept(h: number): { errno: number; handle: number } {
    const l = this.listeners.get(h);
    if (!l) return { errno: E.BADF, handle: 0 };
    const conn = l.backlog.shift();
    if (!conn) return { errno: E.AGAIN, handle: 0 };
    const c = this.next++;
    this.conns.set(c, conn);
    return { errno: E.SUCCESS, handle: c };
  }

  recv(h: number, max: number): { errno: number; data: Uint8Array } {
    if (this.dgrams.has(h)) return { errno: E.NOTSUP, data: new Uint8Array(0) };
    const c = this.conns.get(h);
    if (!c) return { errno: E.BADF, data: new Uint8Array(0) };
    if (c.rxAt >= c.rx.length) {
      // ⚠️ EOF, NOT `EAGAIN`. The whole request was delivered at once, so there
      // is nothing more coming -- and a server that reads until EOF would park
      // forever on an `EAGAIN` that no notification could ever clear.
      return { errno: E.SUCCESS, data: new Uint8Array(0) };
    }
    const n = Math.min(max, c.rx.length - c.rxAt);
    const out = c.rx.slice(c.rxAt, c.rxAt + n);
    c.rxAt += n;
    return { errno: E.SUCCESS, data: out };
  }

  recvFrom(h: number, max: number): { errno: number; data: Uint8Array; from?: SockAddr } {
    return this.recv(h, max);
  }

  send(h: number, data: Uint8Array): { errno: number; sent: number } {
    if (this.dgrams.has(h)) return { errno: E.NOTSUP, sent: 0 };
    const c = this.conns.get(h);
    if (!c) return { errno: E.BADF, sent: 0 };
    if (c.done) return { errno: E.PIPE, sent: 0 };
    c.framer.push(data);
    // The response may be whole the moment its last byte lands; the guest is
    // under no obligation to do anything further, and usually will not.
    if (c.framer.complete) this.complete(h);
    return { errno: E.SUCCESS, sent: data.length };
  }

  addr(_h: number, _peer: boolean): { errno: number; addr?: SockAddr } {
    // A synthetic peer. The guest may log it; nothing routes by it.
    return { errno: E.SUCCESS, addr: { ip: '127.0.0.1', port: 0, v6: false } };
  }

  getsockopt(): { errno: number; value: number } {
    return { errno: E.SUCCESS, value: 0 };
  }

  setsockopt(): number {
    return E.SUCCESS;
  }

  shutdown(h: number, how: number): number {
    // 1 = SHUT_WR, 2 = SHUT_RDWR. Either completes the response: the guest has
    // said it will write no more, which is exactly the signal a reader needs.
    if (how === 1 || how === 2) this.complete(h, true);
    return E.SUCCESS;
  }

  close(h: number): number {
    if (this.dgrams.delete(h)) return E.SUCCESS;
    if (this.listeners.delete(h)) return E.SUCCESS;
    this.complete(h, true);
    this.conns.delete(h);
    this.ports.delete(h);
    return E.SUCCESS;
  }

  /**
   * Hands back whatever the guest has written.
   *
   * `viaClose` marks the connection ending, which is the ONLY thing that can
   * finish a response framed neither by length nor by chunks.
   */
  private complete(h: number, viaClose = false): void {
    const c = this.conns.get(h);
    if (!c || c.done) return;
    if (viaClose) c.framer.atClose();
    if (!c.framer.complete) return;
    c.done = true;
    c.finish(c.framer.bytes());
  }

  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>): number[] {
    const hits: number[] = [];
    subs.forEach((s, i) => {
      if (s.write) {
        // A host-side buffer is always writable; there is nothing to fill up.
        if (this.conns.has(s.fd)) hits.push(i);
        return;
      }
      const l = this.listeners.get(s.fd);
      if (l) {
        if (l.backlog.length > 0) hits.push(i);
        return;
      }
      // A connection is readable while request bytes remain AND at EOF, since
      // a read at EOF returns immediately rather than blocking.
      if (this.conns.has(s.fd)) hits.push(i);
    });
    return hits;
  }

  liveHandles(): number[] {
    return [...this.listeners.keys(), ...this.conns.keys()];
  }
}

/**
 * A request as it crosses `postMessage`.
 *
 * ⚠️ PLAIN, STRUCTURED-CLONEABLE VALUES, not `Request`/`Response`. The service
 * worker is a separate script that cannot import this bundle, and a `Request`
 * cannot be posted to it anyway. Keeping the codec on plain data also makes it
 * testable without a DOM.
 */
export type WireRequest = {
  method: string;
  url: string;
  headers: [string, string][];
  body?: Uint8Array;
};

export type WireResponse = {
  status: number;
  statusText: string;
  headers: [string, string][];
  body: Uint8Array;
};

/** Serializes a request to HTTP/1.1 request bytes. */
export function serializeRequest(req: WireRequest, authority: string): Uint8Array {
  const url = new URL(req.url);
  const lines = [`${req.method} ${url.pathname}${url.search} HTTP/1.1`];

  // ⚠️ `Host` IS SYNTHESIZED, not copied. It is a forbidden header, so `fetch`
  // never exposes it on a `Request` -- and every HTTP/1.1 server requires one.
  // nginx's default vhost answers 400 without it, and the missing header would
  // be invisible from here.
  lines.push(`Host: ${authority}`);

  for (const [k, v] of req.headers) {
    const lower = k.toLowerCase();
    if (lower === 'host' || lower === 'connection' || lower === 'content-length') continue;
    lines.push(`${k}: ${v}`);
  }
  if (req.body && req.body.length > 0) lines.push(`Content-Length: ${req.body.length}`);
  // ⚠️ `Connection: close` is what BOUNDS THE RESPONSE. The host reads until the
  // guest closes its write side, so a keep-alive server would hold the socket
  // open and the host would wait forever for a response it already has in full.
  lines.push('Connection: close');

  const head = new TextEncoder().encode(lines.join('\r\n') + '\r\n\r\n');
  if (!req.body || req.body.length === 0) return head;
  const out = new Uint8Array(head.length + req.body.length);
  out.set(head, 0);
  out.set(req.body, head.length);
  return out;
}

/** Parses HTTP/1.1 response bytes. */
export function parseResponse(bytes: Uint8Array): WireResponse {
  // ⚠️ AN EMPTY RESPONSE IS A CLOSED CONNECTION, not a malformed one, and
  // saying so matters. It is what a request in flight sees when the worker
  // handling it dies -- observed while killing an nginx worker -- and reporting
  // it as a parse failure points the reader at the framing code instead of at
  // the process that went away.
  if (bytes.length === 0) {
    throw new Error('the guest closed the connection without sending a response');
  }
  const sep = indexOfCRLFCRLF(bytes);
  if (sep < 0) {
    throw new Error(`guest response has no header terminator (${bytes.length} bytes)`);
  }
  const head = new TextDecoder().decode(bytes.subarray(0, sep));
  let body = bytes.slice(sep + 4);

  const [statusLine, ...rest] = head.split('\r\n');
  const m = /^HTTP\/1\.[01] (\d{3})(?: (.*))?$/.exec(statusLine ?? '');
  if (!m) throw new Error(`guest response has no status line: ${JSON.stringify(statusLine)}`);

  const headers: [string, string][] = [];
  let transferEncoding = '';
  for (const line of rest) {
    const i = line.indexOf(':');
    if (i < 0) continue;
    const name = line.slice(0, i).trim();
    const lower = name.toLowerCase();
    // ⚠️ HOP-BY-HOP headers describe the GUEST'S socket, not this response.
    // `Content-Length` is the dangerous one to forward: the browser would
    // believe it over the actual body length.
    if (lower === 'transfer-encoding') transferEncoding = line.slice(i + 1).trim();
    if (lower === 'connection' || lower === 'transfer-encoding' || lower === 'content-length') {
      continue;
    }
    headers.push([name, line.slice(i + 1).trim()]);
  }

  // ⚠️ DROPPING `Transfer-Encoding` IS NOT ENOUGH -- the BODY is still chunk
  // encoded. Forwarding it with the header removed hands the browser a body
  // interleaved with hex length lines, which renders as garbage rather than
  // failing.
  if (/(^|,)\s*chunked\s*$/i.test(transferEncoding)) body = decodeChunked(body);

  return { status: Number(m[1]), statusText: m[2] ?? '', headers, body };
}
