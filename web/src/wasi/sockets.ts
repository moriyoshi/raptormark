import { E, WASI_ADDRESS, WE_FAMILY, WE_SOCKTYPE } from './abi.ts';
import { Mem, gather, iovecTotal, scatter } from './mem.ts';
import type { FdPoll } from './preview1.ts';

/**
 * WasmEdge's socket extension to preview1: the 11 non-standard names that make
 * a raptormark module WasmEdge-bound, plus `sock_accept`, which ecvisor takes
 * in its STANDARDIZED 3-argument preview1 form and which therefore is not part
 * of the extension.
 *
 * ⚠️ The specification is `runtime/src/sys.rs:517-563`, not the `third_party/
 * wazero` path those comments name -- that tree does not exist here. This file
 * is the first executable statement of that ABI anywhere in the repo.
 *
 * Ports cross this boundary in HOST byte order as a plain u32 argument, while
 * addresses cross as network-order octets behind a `WasiAddress`. Mixing those
 * two up produces a connection to a plausible-looking wrong host, so the
 * conversion lives in `readAddr`/`writeAddr` and nowhere else.
 */

export type SockAddr = { ip: string; port: number; v6: boolean };

/**
 * What a transport must provide. Every method is NON-BLOCKING and returns a
 * WASI errno; `E.AGAIN` and `E.INPROGRESS` are resumable states that ecvisor
 * relies on, and collapsing them into a generic failure turns an ordinary
 * asynchronous transition into a false permanent error.
 */
export interface SocketBackend {
  open(v6: boolean, dgram: boolean): { errno: number; handle: number };
  bind(h: number, a: SockAddr): number;
  listen(h: number, backlog: number): number;
  connect(h: number, a: SockAddr): number;
  accept(h: number): { errno: number; handle: number };
  recv(h: number, max: number): { errno: number; data: Uint8Array };
  /** As `recv`, but also reports the source -- the `recvfrom` path. */
  recvFrom(h: number, max: number): { errno: number; data: Uint8Array; from?: SockAddr };
  send(h: number, data: Uint8Array, to?: SockAddr): { errno: number; sent: number };
  addr(h: number, peer: boolean): { errno: number; addr?: SockAddr };
  getsockopt(h: number, level: number, name: number): { errno: number; value: number };
  setsockopt(h: number, level: number, name: number, value: number): number;
  shutdown(h: number, how: number): number;
  close(h: number): number;
  /**
   * Readiness for `poll_oneoff`, waiting up to `timeoutMs` (0 probes, -1 waits
   * indefinitely). Returns indices into `subs`.
   */
  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>, timeoutMs: number): number[];
  /**
   * Handles this backend currently owns, if it can say.
   *
   * Used by the re-entrant driver: when the guest goes idle waiting on I/O, the
   * host does not know WHICH handle it is parked on -- that lives in the
   * runtime's process table. Polling everything open and notifying liberally is
   * correct because over-notification is safe: a woken process re-checks its
   * condition and re-parks.
   */
  liveHandles?(): number[];
}

/**
 * The backend used before any transport is wired up. Every call reports
 * ENOTSUP and records the name, so a guest that unexpectedly reaches the
 * network fails with an errno it can handle AND names itself in the summary,
 * rather than dying somewhere inside the harness.
 */
export class NullSockets implements SocketBackend {
  readonly reached = new Set<string>();
  private no(name: string): number {
    this.reached.add(name);
    return E.NOTSUP;
  }
  open(): { errno: number; handle: number } {
    return { errno: this.no('sock_open'), handle: 0 };
  }
  bind(): number {
    return this.no('sock_bind');
  }
  listen(): number {
    return this.no('sock_listen');
  }
  connect(): number {
    return this.no('sock_connect');
  }
  accept(): { errno: number; handle: number } {
    return { errno: this.no('sock_accept'), handle: 0 };
  }
  recv(): { errno: number; data: Uint8Array } {
    return { errno: this.no('sock_recv'), data: new Uint8Array(0) };
  }
  recvFrom(): { errno: number; data: Uint8Array } {
    return { errno: this.no('sock_recv_from'), data: new Uint8Array(0) };
  }
  send(): { errno: number; sent: number } {
    return { errno: this.no('sock_send'), sent: 0 };
  }
  addr(): { errno: number } {
    return { errno: this.no('sock_addr') };
  }
  getsockopt(): { errno: number; value: number } {
    return { errno: this.no('sock_getsockopt'), value: 0 };
  }
  setsockopt(): number {
    return this.no('sock_setsockopt');
  }
  shutdown(): number {
    return this.no('sock_shutdown');
  }
  close(): number {
    return this.no('sock_close');
  }
  wait(): number[] {
    return [];
  }
}

/** Network-order octets -> a printable address. */
function octetsToIp(o: Uint8Array): string {
  if (o.length === 4) return Array.from(o).join('.');
  const parts: string[] = [];
  for (let i = 0; i < 16; i += 2) parts.push(((o[i]! << 8) | o[i + 1]!).toString(16));
  return parts.join(':');
}

/** A printable address -> network-order octets. */
function ipToOctets(ip: string, v6: boolean): Uint8Array {
  if (!v6) {
    const out = new Uint8Array(4);
    ip.split('.').forEach((p, i) => (out[i] = Number(p) & 0xff));
    return out;
  }
  const out = new Uint8Array(16);
  // Enough for the forms a resolver hands back; `::` expansion included.
  const [head, tail] = ip.split('::');
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

export function sockets(
  memory: () => WebAssembly.Memory,
  backend: SocketBackend,
): { imports: Record<string, Function>; fdPoll: FdPoll } {
  const mem = () => new Mem(memory());

  /** Reads a `WasiAddress` (pointer + octet count) into a SockAddr. */
  const readAddr = (addrPtr: number, port: number): SockAddr => {
    const m = mem();
    const buf = m.u32(addrPtr + WASI_ADDRESS.BUF);
    const size = m.u32(addrPtr + WASI_ADDRESS.LEN);
    const octets = m.read(buf, size);
    return { ip: octetsToIp(octets), port, v6: size === 16 };
  };

  /** Writes octets back through a `WasiAddress`, plus the type and port outs. */
  const writeAddr = (addrPtr: number, typePtr: number, portPtr: number, a: SockAddr): void => {
    const m = mem();
    const buf = m.u32(addrPtr + WASI_ADDRESS.BUF);
    const cap = m.u32(addrPtr + WASI_ADDRESS.LEN);
    const octets = ipToOctets(a.ip, a.v6);
    m.write(buf, octets.subarray(0, Math.min(cap, octets.length)));
    if (typePtr) m.setU32(typePtr, a.v6 ? WE_FAMILY.INET6 : WE_FAMILY.INET4);
    if (portPtr) m.setU32(portPtr, a.port);
  };

  const imports: Record<string, Function> = {
    sock_open: (family: number, sockType: number, fdOut: number) => {
      const r = backend.open(family === WE_FAMILY.INET6, sockType === WE_SOCKTYPE.DGRAM);
      if (r.errno !== E.SUCCESS) return r.errno;
      mem().setU32(fdOut, r.handle);
      return E.SUCCESS;
    },

    sock_bind: (h: number, addrPtr: number, port: number) =>
      backend.bind(h, readAddr(addrPtr, port)),

    sock_connect: (h: number, addrPtr: number, port: number) =>
      backend.connect(h, readAddr(addrPtr, port)),

    sock_listen: (h: number, backlog: number) => backend.listen(h, backlog),

    // The standardized preview1 form: (fd, fdflags, fd_out). NOT part of the
    // WasmEdge extension, which is why a stock preview1 host provides it.
    sock_accept: (h: number, _flags: number, fdOut: number) => {
      const r = backend.accept(h);
      if (r.errno !== E.SUCCESS) return r.errno;
      mem().setU32(fdOut, r.handle);
      return E.SUCCESS;
    },

    sock_getlocaladdr: (h: number, addrPtr: number, typePtr: number, portPtr: number) => {
      const r = backend.addr(h, false);
      if (r.errno !== E.SUCCESS || !r.addr) return r.errno;
      writeAddr(addrPtr, typePtr, portPtr, r.addr);
      return E.SUCCESS;
    },

    sock_getpeeraddr: (h: number, addrPtr: number, typePtr: number, portPtr: number) => {
      const r = backend.addr(h, true);
      if (r.errno !== E.SUCCESS || !r.addr) return r.errno;
      writeAddr(addrPtr, typePtr, portPtr, r.addr);
      return E.SUCCESS;
    },

    sock_getsockopt: (
      h: number,
      level: number,
      name: number,
      flagPtr: number,
      flagSizePtr: number,
    ) => {
      const r = backend.getsockopt(h, level, name);
      if (r.errno !== E.SUCCESS) return r.errno;
      const m = mem();
      m.setU32(flagPtr, r.value);
      m.setU32(flagSizePtr, 4);
      return E.SUCCESS;
    },

    sock_setsockopt: (h: number, level: number, name: number, flagPtr: number, size: number) =>
      backend.setsockopt(h, level, name, size >= 4 ? mem().u32(flagPtr) : 0),

    sock_shutdown: (h: number, how: number) => backend.shutdown(h, how),

    // ⚠️ sendto/recvfrom do NOT use the `WasiAddress` form the other calls use.
    // They pass a bare 128-byte buffer whose first two bytes are the address
    // FAMILY as a little-endian u16, with the octets from offset 2
    // (`sys.rs:5978-5981` builds it, `sys.rs:6056-6061` reads it back). Treating
    // it as raw octets -- which an earlier draft of this file did -- silently
    // shifts every address by two bytes and dials a plausible wrong host.
    //
    // The iovecs are `IoVec { buf, len }` with both fields usize rather than the
    // ciovec fd_read/fd_write take; identical layout on wasm32.
    sock_send_to: (
      h: number,
      iov: number,
      iovLen: number,
      addrPtr: number,
      port: number,
      _flags: number,
      sentOut: number,
    ) => {
      const m = mem();
      const data = gather(m, iov, iovLen);
      let to: SockAddr | undefined;
      if (addrPtr) {
        const fam = m.u16(addrPtr);
        const v6 = fam === WE_FAMILY.INET6;
        to = { ip: octetsToIp(m.read(addrPtr + 2, v6 ? 16 : 4)), port, v6 };
      }
      const r = backend.send(h, data, to);
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(sentOut, r.sent);
      return E.SUCCESS;
    },

    sock_recv_from: (
      h: number,
      iov: number,
      iovLen: number,
      addrPtr: number,
      _flags: number,
      portOut: number,
      recvOut: number,
      oflagsOut: number,
    ) => {
      const m = mem();
      const r = backend.recvFrom(h, iovecTotal(m, iov, iovLen));
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(recvOut, scatter(m, iov, iovLen, r.data));
      if (r.from && addrPtr) {
        m.setU16(addrPtr, r.from.v6 ? WE_FAMILY.INET6 : WE_FAMILY.INET4);
        m.write(addrPtr + 2, ipToOctets(r.from.ip, r.from.v6));
      }
      if (r.from && portOut) m.setU32(portOut, r.from.port);
      if (oflagsOut) m.setU32(oflagsOut, 0);
      return E.SUCCESS;
    },
  };

  return { imports, fdPoll: { wait: (subs, timeoutMs) => backend.wait(subs, timeoutMs) } };
}

/**
 * The connected-socket data plane does NOT go through `sock_send`/`sock_recv`:
 * ecvisor calls `fd_read`/`fd_write` on the socket handle directly, so it can
 * observe a raw EAGAIN (`sys.rs:5084`, `sys.rs:5113`). A host therefore has to
 * route those two by handle, which is why they are built here rather than in
 * the file layer, and why the file layer's `read`/`write` are consulted only
 * when the fd is not a socket.
 */
export function socketAwareRw(
  memory: () => WebAssembly.Memory,
  backend: SocketBackend,
  isSocket: (fd: number) => boolean,
  fileRead: (fd: number, iovs: number, iovsLen: number, nread: number) => number,
  fileWrite: (fd: number, iovs: number, iovsLen: number, nwritten: number) => number,
): Record<string, Function> {
  const mem = () => new Mem(memory());
  return {
    fd_read: (fd: number, iovs: number, iovsLen: number, nreadPtr: number) => {
      if (!isSocket(fd)) return fileRead(fd, iovs, iovsLen, nreadPtr);
      const m = mem();
      const r = backend.recv(fd, iovecTotal(m, iovs, iovsLen));
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(nreadPtr, scatter(m, iovs, iovsLen, r.data));
      return E.SUCCESS;
    },
    fd_write: (fd: number, iovs: number, iovsLen: number, nwrittenPtr: number) => {
      if (!isSocket(fd)) return fileWrite(fd, iovs, iovsLen, nwrittenPtr);
      const m = mem();
      const r = backend.send(fd, gather(m, iovs, iovsLen));
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(nwrittenPtr, r.sent);
      return E.SUCCESS;
    },
  };
}
