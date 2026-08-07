import { E } from './abi.ts';
import { Mem } from './mem.ts';
import type { SockAddr, SocketBackend } from './sockets.ts';

/**
 * `raptormark_net_v1` -- the import module the browser profile uses instead of
 * WasmEdge's socket extension.
 *
 * ⚠️ WHY NOT JUST IMPLEMENT `sock_*`? A JS host could, and this package already
 * does. But that ABI assumes real non-blocking sockets underneath: a browser has
 * promises, so it has to fabricate `EAGAIN` plus an out-of-band readiness signal
 * regardless. This module makes that explicit instead of hiding it, and it
 * carries a VERSION so an ABI mismatch is an instantiation failure rather than a
 * mystery at the first connect.
 *
 * It also drops WasmEdge's limitations, which are not a browser's -- there is no
 * TCP level in its option table, so `TCP_NODELAY` is inexpressible there.
 *
 * # Readiness is PUSHED
 *
 * The runtime keeps a level-triggered cache and answers `poll`/`epoll` scans
 * from it with no import call at all. The host's job is to call the module's
 * `ecv_net_ready(handle, events)` export whenever a handle makes progress.
 *
 * ⚠️ Over-notifying is safe; under-notifying hangs. A woken process re-checks
 * and re-parks, so a spurious notification costs one scheduler pass. A missing
 * one costs the run.
 *
 * ⚠️ `ecv_net_ready` must NOT be called from inside one of these imports:
 * `ecv_run_slice` is on the stack there. Queue it and notify between slices.
 */

/** Bits for `ecv_net_ready`. Mirrors `runtime/src/net/browser.rs`. */
export const READY = { READ: 1, WRITE: 2 } as const;

/** Capability bits reported by `net_init`. */
export const CAP = { RELAY: 1, FETCH_PROXY: 2, DATAGRAM: 4 } as const;

/**
 * Mints a synthetic address per name and remembers which is which.
 *
 * ⚠️ THIS IS THE PRECONDITION FOR EVERY BROWSER TRANSPORT, and it is easy to
 * miss until nothing works. `fetch` needs a URL, a relay's CONNECT needs a host,
 * TLS needs SNI -- but `connect(2)` carries only an address, the name having
 * been consumed by the guest's resolver and thrown away. So the resolver is
 * answered with an address from a reserved range, and the reverse lookup at
 * connect time is what turns it back into a name.
 *
 * This is what slirp and passt do, for the same reason.
 *
 * 240.0.0.0/4 is reserved and never routable, so a synthetic address cannot be
 * confused with a real destination the guest might legitimately reach.
 */
export class AddressPool {
  private readonly byName = new Map<string, string>();
  private readonly byAddr = new Map<string, string>();
  private next = 1;

  /** The address for `name`, minting one on first sight. */
  mint(name: string): string {
    const have = this.byName.get(name);
    if (have) return have;
    // 240.x.y.z, skipping .0 and .255 in the low octet for tidiness.
    const n = this.next++;
    const addr = `240.${(n >> 16) & 0xff}.${(n >> 8) & 0xff}.${n & 0xff || 1}`;
    this.byName.set(name, addr);
    this.byAddr.set(addr, name);
    return addr;
  }

  /** The name an address stands for, or undefined if it is not ours. */
  lookup(addr: string): string | undefined {
    return this.byAddr.get(addr);
  }

  static isSynthetic(addr: string): boolean {
    const first = Number(addr.split('.')[0]);
    return first >= 240 && first <= 255;
  }
}

export interface NetV1Options {
  memory: () => WebAssembly.Memory;
  backend: SocketBackend;
  /** Which transports this host offers, as a `CAP` bitmask. */
  capabilities: number;
  /** Records a handle whose readiness changed, for the driver to flush. */
  notify: (handle: number, events: number) => void;
  /** Names minted here; the transport reverses them at connect time. */
  pool?: AddressPool;
}

export function netV1(opts: NetV1Options): Record<string, Function> {
  const mem = () => new Mem(opts.memory());
  const b = opts.backend;

  const readAddr = (ptr: number, len: number, port: number): SockAddr => {
    const m = mem();
    const octets = m.read(ptr, len);
    const v6 = len === 16;
    return { ip: ipOf(octets), port, v6 };
  };

  const writeAddr = (a: SockAddr, addrOut: number, lenOut: number, portOut: number): void => {
    const m = mem();
    const octets = octetsOf(a);
    if (addrOut) m.write(addrOut, octets);
    if (lenOut) m.setU32(lenOut, octets.length);
    if (portOut) m.setU32(portOut, a.port);
  };

  /** A readiness hint after an operation, so the host does not have to guess. */
  const hint = (h: number, events: number) => opts.notify(h, events);

  return {
    net_init: (capsOut: number) => {
      if (capsOut) mem().setU32(capsOut, opts.capabilities);
      return E.SUCCESS;
    },

    net_socket: (v6: number, dgram: number, hOut: number) => {
      const r = b.open(v6 !== 0, dgram !== 0);
      if (r.errno !== E.SUCCESS) return r.errno;
      mem().setU32(hOut, r.handle);
      return E.SUCCESS;
    },

    net_bind: (h: number, addr: number, len: number, port: number) =>
      b.bind(h, readAddr(addr, len, port)),

    net_listen: (h: number, backlog: number) => b.listen(h, backlog),

    net_connect: (h: number, addr: number, len: number, port: number) => {
      const to = readAddr(addr, len, port);
      // ⚠️ THE REVERSE LOOKUP IS THE WHOLE POINT OF THE POOL. The guest is
      // dialling an address this host minted for a name, and every browser
      // transport needs the NAME back -- a URL for `fetch`, a host for a
      // relay's CONNECT, an SNI for TLS. Without this the address is a dead end
      // that routes nowhere, because 240/4 is reserved.
      //
      // The name is carried in the `ip` field because a `SockAddr` is what the
      // backend takes and node's `connect` accepts a hostname there
      // interchangeably. A transport that needs them distinguished should widen
      // the type rather than re-derive the name.
      const name = AddressPool.isSynthetic(to.ip) ? opts.pool?.lookup(to.ip) : undefined;
      const rc = b.connect(h, name ? { ...to, ip: name } : to);
      // A connect that is under way becomes writable when it completes, and the
      // runtime parks on exactly that. The transport reports it; this only
      // ensures a connect that completed synchronously is not missed.
      if (rc === E.SUCCESS) hint(h, READY.WRITE);
      return rc;
    },

    net_accept: (h: number, hOut: number) => {
      const r = b.accept(h);
      if (r.errno !== E.SUCCESS) return r.errno;
      mem().setU32(hOut, r.handle);
      // A freshly accepted connection may already hold bytes.
      hint(r.handle, READY.READ | READY.WRITE);
      return E.SUCCESS;
    },

    net_recv: (h: number, buf: number, len: number, nOut: number) => {
      const r = b.recv(h, len);
      if (r.errno !== E.SUCCESS) return r.errno;
      const m = mem();
      m.write(buf, r.data);
      m.setU32(nOut, r.data.length);
      return E.SUCCESS;
    },

    net_send: (h: number, buf: number, len: number, nOut: number) => {
      const m = mem();
      const r = b.send(h, m.read(buf, len));
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(nOut, r.sent);
      return E.SUCCESS;
    },

    net_recv_from: (
      h: number,
      buf: number,
      len: number,
      nOut: number,
      addrOut: number,
      lenOut: number,
      portOut: number,
    ) => {
      const r = b.recvFrom(h, len);
      if (r.errno !== E.SUCCESS) return r.errno;
      const m = mem();
      m.write(buf, r.data);
      m.setU32(nOut, r.data.length);
      if (r.from) writeAddr(r.from, addrOut, lenOut, portOut);
      return E.SUCCESS;
    },

    net_send_to: (
      h: number,
      buf: number,
      len: number,
      addr: number,
      addrLen: number,
      port: number,
      nOut: number,
    ) => {
      const m = mem();
      const r = b.send(h, m.read(buf, len), readAddr(addr, addrLen, port));
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(nOut, r.sent);
      return E.SUCCESS;
    },

    net_addr: (h: number, peer: number, addrOut: number, lenOut: number, portOut: number) => {
      const r = b.addr(h, peer !== 0);
      if (r.errno !== E.SUCCESS || !r.addr) return r.errno;
      writeAddr(r.addr, addrOut, lenOut, portOut);
      return E.SUCCESS;
    },

    // One call for both directions: `set` selects, `lenIo` is in/out.
    net_sockopt: (
      h: number,
      set: number,
      level: number,
      name: number,
      buf: number,
      lenIo: number,
    ) => {
      const m = mem();
      const cap = lenIo ? m.u32(lenIo) : 0;
      if (set !== 0) {
        return b.setsockopt(h, level, name, cap >= 4 ? m.u32(buf) : 0);
      }
      const r = b.getsockopt(h, level, name);
      const n = Math.min(cap, 4);
      if (buf && n > 0) m.setU32(buf, r.value);
      if (lenIo) m.setU32(lenIo, n);
      // ⚠️ SUCCESS even on a backend error, with a zeroed value. libpq queries
      // SO_ERROR on every connection and reads a failure as "could not get
      // socket error status", which fails a connection that already succeeded.
      return E.SUCCESS;
    },

    net_shutdown: (h: number, rd: number, wr: number) =>
      // The backend takes WasmEdge's bitflags (1 = RD, 2 = WR).
      b.shutdown(h, (rd !== 0 ? 1 : 0) | (wr !== 0 ? 2 : 0)),

    net_close: (h: number) => b.close(h),

    /**
     * Resolves a name to an address THIS HOST chooses.
     *
     * Synchronous and always successful here, because minting is a map
     * insertion: nothing is looked up, and the address means "whatever the
     * transport decides this name is" rather than a real endpoint. A host that
     * had to consult a real resolver would return `E.AGAIN` and notify when the
     * answer arrived -- the runtime retries the whole query, which is what
     * resolvers do anyway.
     */
    net_resolve: (
      namePtr: number,
      nameLen: number,
      v6: number,
      addrOut: number,
      lenOut: number,
    ) => {
      const m = mem();
      const name = m.str(namePtr, nameLen);
      if (v6 !== 0) {
        // No synthetic v6 range is minted: a guest that asks for AAAA gets
        // NXDOMAIN and falls back to A, which every resolver does. Answering
        // both would double the mapping for no gain.
        return E.NOTSUP;
      }
      const pool = opts.pool;
      if (!pool) return E.NOTSUP;
      const addr = pool.mint(name);
      const octets = new Uint8Array(addr.split('.').map((x) => Number(x) & 0xff));
      m.write(addrOut, octets);
      m.setU32(lenOut, octets.length);
      return E.SUCCESS;
    },
  };
}

function ipOf(o: Uint8Array): string {
  if (o.length === 4) return Array.from(o).join('.');
  const parts: string[] = [];
  for (let i = 0; i < 16; i += 2) parts.push((((o[i]! << 8) | o[i + 1]!) >>> 0).toString(16));
  return parts.join(':');
}

function octetsOf(a: SockAddr): Uint8Array {
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
