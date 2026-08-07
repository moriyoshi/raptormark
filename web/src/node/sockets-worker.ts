import dgram from 'node:dgram';
import net from 'node:net';
import { parentPort, workerData } from 'node:worker_threads';
import {
  C,
  CTRL_INTS,
  E_AGAIN,
  E_BADF,
  E_INPROGRESS,
  E_INVAL,
  E_NOTSUP,
  E_SUCCESS,
  FAMILY,
  OP,
  PAYLOAD_BYTES,
  READY,
  REQ_OFF,
  RES_OFF,
  SOCKTYPE,
  errnoOf,
} from './protocol.ts';

/**
 * The socket worker: it owns every real socket and its own event loop, and
 * answers the main thread's synchronous requests.
 *
 * See protocol.ts for why this thread exists. The rule that keeps it working is
 * that it must NEVER block: it waits for requests with `Atomics.waitAsync`, so
 * `'data'`, `'connect'` and `'drain'` events keep being delivered while the
 * main thread is parked inside `Atomics.wait`.
 */

const sab: SharedArrayBuffer = workerData.sab;
const ctrl = new Int32Array(sab, 0, CTRL_INTS);
const reqBuf = new Uint8Array(sab, REQ_OFF, PAYLOAD_BYTES);
const resBuf = new Uint8Array(sab, RES_OFF, PAYLOAD_BYTES);

/** Handles are allocated from the base the host reserves for sockets. */
const HANDLE_BASE: number = workerData.handleBase;

type Sock = {
  h: number;
  kind: 'stream' | 'dgram';
  v6: boolean;
  socket?: net.Socket;
  server?: net.Server;
  udp?: dgram.Socket;
  /** Queued inbound data. For dgram, one entry per datagram. */
  rx: { data: Buffer; from?: { ip: string; port: number } }[];
  rxLen: number;
  /** Connections accepted by a listener but not yet handed to `sock_accept`. */
  backlog: net.Socket[];
  eof: boolean;
  connecting: boolean;
  connected: boolean;
  writable: boolean;
  closed: boolean;
  /** Sticky error, reported to the next operation that can carry one. */
  err?: number;
  bound?: { ip: string; port: number };
};

const socks = new Map<number, Sock>();
let nextHandle = HANDLE_BASE;

/** Anything that might have changed readiness resolves a parked POLL. */
let pollWaiter: (() => void) | null = null;
function wake(): void {
  const w = pollWaiter;
  if (w) {
    pollWaiter = null;
    w();
  }
}

function alloc(kind: 'stream' | 'dgram', v6: boolean): Sock {
  const h = nextHandle++;
  const s: Sock = {
    h,
    kind,
    v6,
    rx: [],
    rxLen: 0,
    backlog: [],
    eof: false,
    connecting: false,
    connected: false,
    writable: false,
    closed: false,
  };
  socks.set(h, s);
  return s;
}

/** Wires a connected stream socket's events into a Sock. */
function attach(s: Sock, socket: net.Socket): void {
  s.socket = socket;
  socket.on('data', (chunk: Buffer) => {
    s.rx.push({ data: chunk });
    s.rxLen += chunk.length;
    // Pause once a reader is far behind, so a fast peer cannot grow the queue
    // without bound while the guest is busy. Resumed in `recv`.
    if (s.rxLen > PAYLOAD_BYTES * 4) socket.pause();
    wake();
  });
  socket.on('end', () => {
    s.eof = true;
    wake();
  });
  socket.on('close', () => {
    s.eof = true;
    s.writable = false;
    wake();
  });
  socket.on('error', (err) => {
    s.err = errnoOf(err);
    s.eof = true;
    s.writable = false;
    wake();
  });
  socket.on('drain', () => {
    s.writable = true;
    wake();
  });
}

function readiness(s: Sock): number {
  let r = 0;
  // An error or EOF makes a socket readable: that is how the guest discovers
  // either one, by reading and getting 0 or an errno.
  if (s.rxLen > 0 || s.eof || s.err !== undefined || s.backlog.length > 0) r |= READY.READ;
  if (s.err !== undefined || (s.connected && s.writable)) r |= READY.WRITE;
  return r;
}

// --- operations -------------------------------------------------------------

type Reply = { errno: number; v0?: number; v1?: number; payload?: Uint8Array };

function opOpen(family: number, type: number): Reply {
  const v6 = family === FAMILY.INET6;
  if (type === SOCKTYPE.DGRAM) {
    const s = alloc('dgram', v6);
    const udp = dgram.createSocket(v6 ? 'udp6' : 'udp4');
    s.udp = udp;
    s.connected = true;
    s.writable = true;
    udp.on('message', (data, rinfo) => {
      s.rx.push({ data, from: { ip: rinfo.address, port: rinfo.port } });
      s.rxLen += data.length;
      wake();
    });
    udp.on('error', (err) => {
      s.err = errnoOf(err);
      wake();
    });
    // Unref so an idle UDP socket cannot keep the worker alive on its own.
    udp.unref();
    return { errno: E_SUCCESS, v0: s.h };
  }
  const s = alloc('stream', v6);
  return { errno: E_SUCCESS, v0: s.h };
}

/**
 * ⚠️ BIND AND LISTEN MUST NOT REPLY UNTIL THE SOCKET IS ACTUALLY BOUND.
 *
 * On Linux `bind(2)` and `listen(2)` are synchronous: when they return, the
 * address is taken and a peer can connect. Node's are not -- `dgram.bind()` and
 * `server.listen()` complete on a later `'listening'` event. Replying as soon
 * as they are CALLED hands the guest a lie, and the guest immediately acts on
 * it: it sends to a port nothing is listening on yet (a silently dropped
 * datagram, i.e. a hang) or connects and gets ECONNREFUSED.
 *
 * It is a race, so it does not fail every time -- which is exactly why it is
 * worth closing here rather than after chasing a flaky test.
 */
function opBind(s: Sock, ip: string, port: number, reply: (r: Reply) => void): void {
  s.bound = { ip, port };
  if (s.kind !== 'dgram' || !s.udp) {
    // A stream socket's bind is applied by `listen`: node has no
    // bind-then-listen split, so the address is remembered and used there.
    return reply({ errno: E_SUCCESS });
  }
  const udp = s.udp;
  const onErr = (err: Error): void => {
    udp.off('listening', onOk);
    reply({ errno: errnoOf(err) });
  };
  const onOk = (): void => {
    udp.off('error', onErr);
    const a = udp.address();
    s.bound = { ip: a.address, port: a.port };
    reply({ errno: E_SUCCESS });
  };
  udp.once('listening', onOk);
  udp.once('error', onErr);
  try {
    udp.bind(port, ip);
  } catch (err) {
    udp.off('listening', onOk);
    udp.off('error', onErr);
    reply({ errno: errnoOf(err) });
  }
}

/**
 * ⚠️ `listen(2)` ON AN ALREADY-LISTENING SOCKET SUCCEEDS. Linux only adjusts the
 * backlog; POSIX says the same. This is not a corner case -- nginx does it on
 * every startup, because `ngx_configure_listening_sockets` re-listens to apply
 * the configured backlog after `ngx_open_listening_sockets` has already bound.
 *
 * Node has no bind-then-listen split and no way to change a backlog afterwards,
 * so the second call is a no-op that must still report success. Creating a
 * second `net.createServer()` is catastrophic rather than merely wasteful:
 *
 *   1. it cannot bind, because the FIRST server holds the port -- EADDRINUSE;
 *   2. `s.server` has already been overwritten with the failed one;
 *   3. the failure set a STICKY `s.err`, and `readiness()` reports a socket with
 *      an error as both readable and writable, forever;
 *   4. so epoll wakes the guest, `accept` returns the sticky error, the guest
 *      logs it and epolls again.
 *
 * Measured before this fix: 572 318 epoll/accept4/log iterations in 90 s, and
 * nginx never served a byte. The visible symptom was the spin, three layers
 * from the cause.
 */
function opListen(s: Sock, backlog: number, reply: (r: Reply) => void): void {
  if (s.kind !== 'stream') return reply({ errno: E_INVAL });
  if (s.server) return reply({ errno: E_SUCCESS });
  const server = net.createServer();
  s.server = server;
  server.on('connection', (socket) => {
    s.backlog.push(socket);
    wake();
  });
  const onErr = (err: Error): void => {
    server.off('listening', onOk);
    // ⚠️ DROP THE FAILED SERVER, and do NOT poison the socket with `s.err`.
    //
    // Dropping it because a listen that failed did not take the port, and the
    // guest is entitled to retry -- nginx retries EADDRINUSE five times, 500 ms
    // apart. Leaving `s.server` set would make the retry take the
    // already-listening path above and report a success that never happened.
    //
    // Not setting `s.err` because this failure is being RETURNED; a sticky
    // socket error is for something that goes wrong asynchronously after the
    // socket is up, and setting it here makes the socket permanently ready.
    s.server = undefined;
    reply({ errno: errnoOf(err) });
  };
  const onOk = (): void => {
    server.off('error', onErr);
    // Report ongoing errors from here on, rather than as a reply.
    server.on('error', (err) => {
      s.err = errnoOf(err);
      wake();
    });
    const a = server.address();
    if (a && typeof a === 'object') s.bound = { ip: a.address, port: a.port };
    reply({ errno: E_SUCCESS });
  };
  server.once('listening', onOk);
  server.once('error', onErr);
  const b = s.bound ?? { ip: s.v6 ? '::' : '0.0.0.0', port: 0 };
  server.listen({ host: b.ip, port: b.port, backlog: backlog || 511 });
  server.unref();
}

/**
 * connect, in the shape `sys_connect` (`runtime/src/sys.rs:5574-5599`) expects:
 * the FIRST call starts the attempt and reports INPROGRESS, and ecvisor retries
 * once the socket is writable -- at which point this must report 0, or the real
 * error if the attempt failed. Reporting success on the first call would be
 * wrong even when it eventually connects, because the guest has not yet been
 * told to wait.
 */
function opConnect(s: Sock, ip: string, port: number): Reply {
  if (s.err !== undefined) {
    const e = s.err;
    s.err = undefined;
    return { errno: e };
  }
  if (s.connected) return { errno: E_SUCCESS };
  if (s.connecting) return { errno: E_INPROGRESS };

  if (s.kind === 'dgram' && s.udp) {
    s.bound = { ip, port };
    return { errno: E_SUCCESS };
  }
  s.connecting = true;
  const socket = new net.Socket();
  attach(s, socket);
  socket.on('connect', () => {
    s.connecting = false;
    s.connected = true;
    s.writable = true;
    wake();
  });
  try {
    socket.connect({ host: ip, port });
  } catch (err) {
    s.connecting = false;
    return { errno: errnoOf(err) };
  }
  socket.unref();
  return { errno: E_INPROGRESS };
}

function opAccept(s: Sock): Reply {
  if (s.backlog.length === 0) {
    return { errno: s.err !== undefined ? s.err : E_AGAIN };
  }
  const socket = s.backlog.shift()!;
  const child = alloc('stream', s.v6);
  child.connected = true;
  child.writable = true;
  attach(child, socket);
  socket.unref();
  return { errno: E_SUCCESS, v0: child.h };
}

function opRecv(s: Sock, max: number, wantFrom: boolean): Reply {
  if (s.rxLen === 0) {
    if (s.err !== undefined) {
      const e = s.err;
      s.err = undefined;
      return { errno: e };
    }
    // EOF is errno 0 with zero bytes -- `socket_recv` (`sys.rs:5210`) maps
    // errno 0 straight to the byte count, so 0 bytes IS the guest's EOF.
    if (s.eof) return { errno: E_SUCCESS, v0: 0, payload: new Uint8Array(0) };
    return { errno: E_AGAIN };
  }
  const cap = Math.min(max, PAYLOAD_BYTES);
  let out: Buffer;
  let from: { ip: string; port: number } | undefined;
  if (s.kind === 'dgram') {
    // Datagrams keep their boundaries: one recv yields exactly one message,
    // truncated to the caller's capacity.
    const first = s.rx.shift()!;
    s.rxLen -= first.data.length;
    out = first.data.subarray(0, cap);
    from = first.from;
  } else {
    const parts: Buffer[] = [];
    let got = 0;
    while (got < cap && s.rx.length > 0) {
      const head = s.rx[0]!;
      const take = Math.min(cap - got, head.data.length);
      parts.push(head.data.subarray(0, take));
      got += take;
      if (take === head.data.length) s.rx.shift();
      else s.rx[0] = { data: head.data.subarray(take) };
      s.rxLen -= take;
    }
    out = Buffer.concat(parts);
    if (s.rxLen <= PAYLOAD_BYTES && s.socket?.isPaused()) s.socket.resume();
  }
  resBuf.set(out, 0);
  const reply: Reply = { errno: E_SUCCESS, v0: out.length, payload: out };
  if (wantFrom && from) {
    // The source address rides in the reply's tail, after the data. Its LENGTH
    // has to be recoverable, so the family travels in bit 16 of the port word --
    // a port is 16 bits, so the space is free, and the alternative (assuming the
    // socket's own family) is wrong for a v4-mapped peer on a v6 socket.
    const v6 = from.ip.includes(':');
    resBuf.set(ipToOctets(from.ip, v6), out.length);
    reply.v1 = (from.port & 0xffff) | (v6 ? 0x10000 : 0);
  }
  return reply;
}

function opSend(s: Sock, data: Uint8Array, to?: { ip: string; port: number }): Reply {
  if (s.err !== undefined) {
    const e = s.err;
    s.err = undefined;
    return { errno: e };
  }
  if (s.kind === 'dgram' && s.udp) {
    const dst = to ?? s.bound;
    if (!dst) return { errno: E_INVAL };
    s.udp.send(Buffer.from(data), dst.port, dst.ip);
    return { errno: E_SUCCESS, v0: data.length };
  }
  if (!s.socket || !s.connected) return { errno: s.connecting ? E_AGAIN : E_BADF };
  // `write` returning false means the kernel buffer is full; the bytes are
  // still accepted, so reporting a full write is correct. What it does change
  // is writability, which is what the guest polls on.
  const ok = s.socket.write(Buffer.from(data));
  if (!ok) s.writable = false;
  return { errno: E_SUCCESS, v0: data.length };
}

function opAddr(s: Sock, peer: boolean): Reply {
  const a = peer ? s.socket?.remoteAddress : (s.server?.address() ?? s.socket?.localAddress);
  let ip: string | undefined;
  let port = 0;
  if (typeof a === 'string') {
    ip = a;
    port = peer ? (s.socket?.remotePort ?? 0) : (s.socket?.localPort ?? 0);
  } else if (a && typeof a === 'object') {
    ip = a.address;
    port = a.port;
  }
  if (!ip) {
    if (s.bound) {
      ip = s.bound.ip;
      port = s.bound.port;
    } else return { errno: E_BADF };
  }
  const v6 = ip.includes(':');
  resBuf.set(ipToOctets(ip, v6), 0);
  return { errno: E_SUCCESS, v0: port, v1: v6 ? 1 : 0 };
}

/**
 * getsockopt receives LINUX numbers, not WasmEdge ones: `sys_getsockopt`
 * (`runtime/src/sys.rs:6206`) passes the guest's level and name straight
 * through, unlike `sys_setsockopt`, which translates via `we_sockopt`.
 *
 * It also discards our errno and reports success to the guest regardless
 * (`:6207`), so the VALUE we write is the entire answer. `SO_ERROR` is the one
 * that matters: libpq calls it on every connection and reads a non-zero value
 * as a failure, so a socket with no pending error must read back 0.
 */
const LINUX_SOL_SOCKET = 1;
const LINUX_SO_ERROR = 4;

function opGetsockopt(s: Sock, level: number, name: number): Reply {
  if (level === LINUX_SOL_SOCKET && name === LINUX_SO_ERROR) {
    const e = s.err ?? 0;
    s.err = undefined;
    return { errno: E_SUCCESS, v0: e };
  }
  return { errno: E_SUCCESS, v0: 0 };
}

function opShutdown(s: Sock, how: number): Reply {
  // WasmEdge bitflags: 1 = SD_RD, 2 = SD_WR (`sys_shutdown`, `sys.rs:6222`).
  if (how & 2) s.socket?.end();
  if (how & 1) s.socket?.pause();
  return { errno: E_SUCCESS };
}

function opClose(s: Sock): Reply {
  s.closed = true;
  s.socket?.destroy();
  s.server?.close();
  s.udp?.close();
  socks.delete(s.h);
  return { errno: E_SUCCESS };
}

/**
 * POLL: report which of the requested (handle, write) pairs are ready, waiting
 * up to `timeoutMs` for one to become so. A zero timeout is the probe form
 * `socket_poll_ready` (`sys.rs:651`) uses; a negative one waits indefinitely.
 */
function opPoll(
  pairs: { h: number; write: boolean }[],
  timeoutMs: number,
  reply: (r: Reply) => void,
): void {
  const check = (): number[] => {
    const hit: number[] = [];
    pairs.forEach((p, i) => {
      const s = socks.get(p.h);
      if (!s) {
        // A closed handle is "ready" so the guest reads it and learns.
        hit.push(i);
        return;
      }
      const r = readiness(s);
      if (p.write ? r & READY.WRITE : r & READY.READ) hit.push(i);
    });
    return hit;
  };

  const emit = (hit: number[]): void => {
    const out = new Uint32Array(hit.length);
    hit.forEach((v, i) => (out[i] = v));
    resBuf.set(new Uint8Array(out.buffer, 0, hit.length * 4), 0);
    reply({ errno: E_SUCCESS, v0: hit.length, payload: new Uint8Array(0) });
  };

  const first = check();
  if (first.length > 0 || timeoutMs === 0) return emit(first);

  let timer: NodeJS.Timeout | undefined;
  const finish = (): void => {
    if (timer) clearTimeout(timer);
    pollWaiter = null;
    emit(check());
  };
  pollWaiter = finish;
  if (timeoutMs > 0) timer = setTimeout(finish, timeoutMs);
}

// --- address helpers --------------------------------------------------------

function ipToOctets(ip: string, v6: boolean): Uint8Array {
  if (!v6) {
    const out = new Uint8Array(4);
    ip.split('.').forEach((p, i) => (out[i] = Number(p) & 0xff));
    return out;
  }
  const out = new Uint8Array(16);
  const clean = ip.replace(/%.*$/, '');
  const [head, tail] = clean.split('::');
  const h = head ? head.split(':').filter(Boolean) : [];
  const t = tail ? tail.split(':').filter(Boolean) : [];
  h.forEach((g, i) => {
    const v = parseInt(g, 16);
    out[i * 2] = v >> 8;
    out[i * 2 + 1] = v & 0xff;
  });
  t.forEach((g, i) => {
    const v = parseInt(g, 16);
    const base = 16 - t.length * 2 + i * 2;
    out[base] = v >> 8;
    out[base + 1] = v & 0xff;
  });
  return out;
}

// --- the request loop -------------------------------------------------------

function handle(reply: (r: Reply) => void): void {
  const op = Atomics.load(ctrl, C.OP);
  const a0 = Atomics.load(ctrl, C.A0);
  const a1 = Atomics.load(ctrl, C.A1);
  const a2 = Atomics.load(ctrl, C.A2);
  const a3 = Atomics.load(ctrl, C.A3);
  const reqLen = Atomics.load(ctrl, C.REQ_LEN);
  const payload = reqBuf.subarray(0, reqLen);

  if (op === OP.OPEN) return reply(opOpen(a0, a1));
  if (op === OP.POLL) {
    const n = a0;
    const pairs: { h: number; write: boolean }[] = [];
    const view = new DataView(reqBuf.buffer, reqBuf.byteOffset, reqLen);
    for (let i = 0; i < n; i++) {
      pairs.push({ h: view.getUint32(i * 8, true), write: view.getUint32(i * 8 + 4, true) !== 0 });
    }
    return opPoll(pairs, a1 | 0, reply);
  }

  const s = socks.get(a0);
  if (!s) return reply({ errno: E_BADF });

  switch (op) {
    // The address arrives as text: a dotted quad, or a HOSTNAME when the
    // address pool reversed a synthetic one. node accepts either.
    case OP.BIND:
      return opBind(s, Buffer.from(payload).toString(), a1, reply);
    case OP.LISTEN:
      return opListen(s, a1, reply);
    case OP.CONNECT:
      return reply(opConnect(s, Buffer.from(payload).toString(), a1));
    case OP.ACCEPT:
      return reply(opAccept(s));
    case OP.RECV:
      return reply(opRecv(s, a1, false));
    case OP.RECVFROM:
      return reply(opRecv(s, a1, true));
    case OP.SEND:
      return reply(opSend(s, payload));
    case OP.SENDTO:
      return reply(
        opSend(s, payload.subarray(0, a2), {
          ip: Buffer.from(payload.subarray(a2)).toString(),
          port: a1,
        }),
      );
    case OP.ADDR:
      return reply(opAddr(s, a1 !== 0));
    case OP.GETSOCKOPT:
      return reply(opGetsockopt(s, a1, a2));
    case OP.SETSOCKOPT:
      // Accepted and dropped. ecvisor already reports success to the guest for
      // options it cannot express (`sys_setsockopt`, `sys.rs:6136-6141`),
      // because nginx treats a failed setsockopt on a listener as fatal.
      void a3;
      return reply({ errno: E_SUCCESS });
    case OP.SHUTDOWN:
      return reply(opShutdown(s, a1));
    case OP.CLOSE:
      return reply(opClose(s));
    default:
      return reply({ errno: E_NOTSUP });
  }
}

let seen = 0;

function publish(r: Reply): void {
  Atomics.store(ctrl, C.ERRNO, r.errno);
  Atomics.store(ctrl, C.V0, r.v0 ?? 0);
  Atomics.store(ctrl, C.V1, r.v1 ?? 0);
  Atomics.store(ctrl, C.RES_LEN, r.payload ? r.payload.length : 0);
  Atomics.add(ctrl, C.RES_SEQ, 1);
  Atomics.notify(ctrl, C.RES_SEQ);
}

function loop(): void {
  const step = (): void => {
    seen = Atomics.load(ctrl, C.REQ_SEQ);
    if (Atomics.load(ctrl, C.OP) === OP.SHUTDOWN_WORKER) {
      // ⚠️ THE SPREAD IS LOAD-BEARING, whatever `no-useless-spread` says.
      // `opClose` does `socks.delete(s.h)`, so iterating the live map would
      // mutate it mid-iteration. Taking a snapshot first is the fix, not noise.
      // oxlint-disable-next-line unicorn/no-useless-spread
      for (const s of [...socks.values()]) opClose(s);
      publish({ errno: E_SUCCESS });
      return;
    }
    // ⚠️ EVERY PATH OUT OF `handle` MUST PUBLISH EXACTLY ONE REPLY. The main
    // thread is parked in `Atomics.wait` on the response counter, so a handler
    // that throws does not surface as an error -- it deadlocks the guest
    // forever, with no output and no diagnostic. `node:net` and `node:dgram`
    // both throw synchronously on a malformed address, which a guest can
    // produce, so this is reachable rather than theoretical.
    //
    // Defensive, and it has NOT been observed to fire: the ten-minute hang that
    // prompted it turned out to be the guest legitimately blocking in
    // `recvfrom` for a datagram that a corrupted address had sent elsewhere.
    // Recorded that way on purpose -- claiming this fixed that hang would be
    // wrong, and the bounded run in `e2e/nodehost_test.go` is what actually
    // makes such a wedge visible.
    let replied = false;
    const once = (r: Reply): void => {
      if (replied) return;
      replied = true;
      publish(r);
      loop();
    };
    try {
      handle(once);
    } catch (err) {
      once({ errno: errnoOf(err) });
    }
  };
  // ⚠️ waitAsync, never wait: a blocking wait here stalls this worker's own
  // event loop, so no 'data' or 'connect' event could ever fire and every
  // socket operation would deadlock -- the exact failure this thread exists to
  // prevent.
  const r = Atomics.waitAsync(ctrl, C.REQ_SEQ, seen);
  if (r.async) r.value.then(step);
  else step();
}

// ⚠️ KEEP THIS THREAD ALIVE. A pending `Atomics.waitAsync` does NOT hold a
// worker's event loop open, and every socket here is deliberately `unref`'d --
// so with nothing else registered the worker EXITS moments after starting, and
// the main thread then blocks forever in `Atomics.wait` on a reply nobody is
// left to send.
//
// The failure is nastier than it sounds because it is timing-dependent: a
// request issued immediately after construction is answered normally, and only
// a run with a gap before its first socket call hangs. It first appeared as a
// guest that makes NO socket calls at all hanging at shutdown, with its stdout
// missing entirely -- node buffers piped output, and the process was killed
// before the flush.
//
// An active listener refs `parentPort`, which holds the loop open.
parentPort?.on('message', () => {});

loop();
