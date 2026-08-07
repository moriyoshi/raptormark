import { Worker } from 'node:worker_threads';
import { fileURLToPath } from 'node:url';
import {
  C,
  CTRL_INTS,
  E_AGAIN,
  E_SUCCESS,
  FAMILY,
  OP,
  PAYLOAD_BYTES,
  REQ_OFF,
  RES_OFF,
  SAB_BYTES,
  SOCKTYPE,
} from './protocol.ts';
import type { SockAddr, SocketBackend } from '../wasi/sockets.ts';

/**
 * A `SocketBackend` backed by real Node sockets running in a worker thread.
 *
 * Every method here is a synchronous round trip: write the request into the
 * SharedArrayBuffer, bump a sequence counter, then `Atomics.wait` for the
 * worker to bump its own. See protocol.ts for why the sockets cannot live on
 * this thread.
 */
export class NodeSockets implements SocketBackend {
  private readonly sab = new SharedArrayBuffer(SAB_BYTES);
  private readonly ctrl = new Int32Array(this.sab, 0, CTRL_INTS);
  private readonly req = new Uint8Array(this.sab, REQ_OFF, PAYLOAD_BYTES);
  private readonly res = new Uint8Array(this.sab, RES_OFF, PAYLOAD_BYTES);
  private readonly worker: Worker;
  private stopped = false;
  /** Handles the guest still holds, for the re-entrant driver to poll. */
  private readonly live = new Set<number>();

  constructor(handleBase: number) {
    this.worker = new Worker(fileURLToPath(new URL('./sockets-worker.ts', import.meta.url)), {
      workerData: { sab: this.sab, handleBase },
    });
    // Nothing here should keep the process alive on its own; the guest's exit
    // is what ends the run.
    this.worker.unref();
  }

  /** One synchronous request/response round trip. */
  private call(
    op: number,
    a0 = 0,
    a1 = 0,
    a2 = 0,
    a3 = 0,
    payload?: Uint8Array,
  ): { errno: number; v0: number; v1: number; len: number } {
    if (this.stopped) return { errno: E_AGAIN, v0: 0, v1: 0, len: 0 };
    Atomics.store(this.ctrl, C.OP, op);
    Atomics.store(this.ctrl, C.A0, a0);
    Atomics.store(this.ctrl, C.A1, a1);
    Atomics.store(this.ctrl, C.A2, a2);
    Atomics.store(this.ctrl, C.A3, a3);
    if (payload && payload.length > 0) {
      this.req.set(payload.subarray(0, PAYLOAD_BYTES), 0);
      Atomics.store(this.ctrl, C.REQ_LEN, Math.min(payload.length, PAYLOAD_BYTES));
    } else {
      Atomics.store(this.ctrl, C.REQ_LEN, 0);
    }

    const before = Atomics.load(this.ctrl, C.RES_SEQ);
    Atomics.add(this.ctrl, C.REQ_SEQ, 1);
    Atomics.notify(this.ctrl, C.REQ_SEQ);
    // Loop rather than a single wait: `Atomics.wait` can return `not-equal`
    // when the worker replied between the load and the wait, and a spurious
    // wake is permitted in general.
    while (Atomics.load(this.ctrl, C.RES_SEQ) === before) {
      Atomics.wait(this.ctrl, C.RES_SEQ, before);
    }
    return {
      errno: Atomics.load(this.ctrl, C.ERRNO),
      v0: Atomics.load(this.ctrl, C.V0),
      v1: Atomics.load(this.ctrl, C.V1),
      len: Atomics.load(this.ctrl, C.RES_LEN),
    };
  }

  private static octets(a: SockAddr): Uint8Array {
    if (!a.v6) {
      const out = new Uint8Array(4);
      a.ip.split('.').forEach((p, i) => (out[i] = Number(p) & 0xff));
      return out;
    }
    const out = new Uint8Array(16);
    const [head, tail] = a.ip.replace(/%.*$/, '').split('::');
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

  private static ip(o: Uint8Array): string {
    if (o.length === 4) return Array.from(o).join('.');
    const parts: string[] = [];
    for (let i = 0; i < 16; i += 2) parts.push((((o[i]! << 8) | o[i + 1]!) >>> 0).toString(16));
    return parts.join(':');
  }

  open(v6: boolean, dgram: boolean): { errno: number; handle: number } {
    const r = this.call(
      OP.OPEN,
      v6 ? FAMILY.INET6 : FAMILY.INET4,
      dgram ? SOCKTYPE.DGRAM : SOCKTYPE.STREAM,
    );
    if (r.errno === E_SUCCESS) this.live.add(r.v0);
    return { errno: r.errno, handle: r.v0 };
  }

  /**
   * The address crosses to the worker as a STRING, not as octets.
   *
   * ⚠️ It used to be octets, and that silently destroyed hostnames. The
   * synthetic-address pool reverses a minted address back to the NAME it stands
   * for and puts it in `ip` -- node's `connect` accepts a hostname there -- but
   * `octets()` parses dotted-quad, so `"localhost"` became `[0,0,0,0]` and the
   * guest connected to 0.0.0.0 and got ECONNREFUSED. A string carries both
   * forms losslessly and node resolves whichever it is given.
   */
  private static host(a: SockAddr): Uint8Array {
    return new TextEncoder().encode(a.ip);
  }

  bind(h: number, a: SockAddr): number {
    return this.call(OP.BIND, h, a.port, 0, 0, NodeSockets.host(a)).errno;
  }

  listen(h: number, backlog: number): number {
    return this.call(OP.LISTEN, h, backlog).errno;
  }

  connect(h: number, a: SockAddr): number {
    return this.call(OP.CONNECT, h, a.port, 0, 0, NodeSockets.host(a)).errno;
  }

  accept(h: number): { errno: number; handle: number } {
    const r = this.call(OP.ACCEPT, h);
    if (r.errno === E_SUCCESS) this.live.add(r.v0);
    return { errno: r.errno, handle: r.v0 };
  }

  recv(h: number, max: number): { errno: number; data: Uint8Array; from?: SockAddr } {
    const r = this.call(OP.RECV, h, max);
    if (r.errno !== E_SUCCESS) return { errno: r.errno, data: new Uint8Array(0) };
    return { errno: E_SUCCESS, data: this.res.slice(0, r.v0) };
  }

  recvFrom(h: number, max: number): { errno: number; data: Uint8Array; from?: SockAddr } {
    const r = this.call(OP.RECVFROM, h, max);
    if (r.errno !== E_SUCCESS) return { errno: r.errno, data: new Uint8Array(0) };
    const data = this.res.slice(0, r.v0);
    // Bit 16 of the port word carries the family; see the worker's opRecv.
    const v6 = (r.v1 & 0x10000) !== 0;
    const n = v6 ? 16 : 4;
    const from: SockAddr = {
      ip: NodeSockets.ip(this.res.slice(r.v0, r.v0 + n)),
      port: r.v1 & 0xffff,
      v6,
    };
    return { errno: E_SUCCESS, data, from };
  }

  send(h: number, data: Uint8Array, to?: SockAddr): { errno: number; sent: number } {
    if (to) {
      const addr = NodeSockets.host(to);
      const buf = new Uint8Array(data.length + addr.length);
      buf.set(data, 0);
      buf.set(addr, data.length);
      const r = this.call(OP.SENDTO, h, to.port, data.length, 0, buf);
      return { errno: r.errno, sent: r.v0 };
    }
    const r = this.call(OP.SEND, h, 0, 0, 0, data);
    return { errno: r.errno, sent: r.v0 };
  }

  addr(h: number, peer: boolean): { errno: number; addr?: SockAddr } {
    const r = this.call(OP.ADDR, h, peer ? 1 : 0);
    if (r.errno !== E_SUCCESS) return { errno: r.errno };
    const v6 = r.v1 !== 0;
    return {
      errno: E_SUCCESS,
      addr: { ip: NodeSockets.ip(this.res.slice(0, v6 ? 16 : 4)), port: r.v0, v6 },
    };
  }

  getsockopt(h: number, level: number, name: number): { errno: number; value: number } {
    const r = this.call(OP.GETSOCKOPT, h, level, name);
    return { errno: r.errno, value: r.v0 };
  }

  setsockopt(h: number, level: number, name: number, value: number): number {
    return this.call(OP.SETSOCKOPT, h, level, name, value).errno;
  }

  shutdown(h: number, how: number): number {
    return this.call(OP.SHUTDOWN, h, how).errno;
  }

  close(h: number): number {
    this.live.delete(h);
    return this.call(OP.CLOSE, h).errno;
  }

  liveHandles(): number[] {
    return [...this.live];
  }

  /**
   * Blocks until one of `subs` is ready or `timeoutMs` elapses. A zero timeout
   * is the probe form; a negative one waits indefinitely.
   */
  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>, timeoutMs: number): number[] {
    const buf = new Uint8Array(subs.length * 8);
    const view = new DataView(buf.buffer);
    subs.forEach((s, i) => {
      view.setUint32(i * 8, s.fd, true);
      view.setUint32(i * 8 + 4, s.write ? 1 : 0, true);
    });
    const r = this.call(OP.POLL, subs.length, timeoutMs | 0, 0, 0, buf);
    if (r.errno !== E_SUCCESS) return [];
    const out: number[] = [];
    const rv = new DataView(this.res.buffer, this.res.byteOffset, this.res.byteLength);
    for (let i = 0; i < r.v0; i++) out.push(rv.getUint32(i * 4, true));
    return out;
  }

  ready(subs: ReadonlyArray<{ fd: number; write: boolean }>): number[] {
    return this.wait(subs, 0);
  }

  /** Stops the worker so the process can exit. */
  async stop(): Promise<void> {
    if (this.stopped) return;
    this.call(OP.SHUTDOWN_WORKER);
    this.stopped = true;
    await this.worker.terminate();
  }
}
