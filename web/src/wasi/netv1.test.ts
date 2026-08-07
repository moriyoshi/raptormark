import assert from 'node:assert/strict';
import { test } from 'vitest';

import { E } from './abi.ts';
import { Mem } from './mem.ts';
import { AddressPool, CAP, READY, netV1 } from './netv1.ts';
import type { SockAddr, SocketBackend } from './sockets.ts';

/** Records every call, and answers with whatever the test queued. */
function recordingBackend() {
  const calls: { op: string; args: unknown[] }[] = [];
  const log = (op: string, ...args: unknown[]) => calls.push({ op, args });
  const b: SocketBackend & { nextRecv?: Uint8Array; errno: number } = {
    errno: E.SUCCESS,
    open(v6: boolean, dgram: boolean) {
      log('open', v6, dgram);
      return { errno: b.errno, handle: 77 };
    },
    bind(h: number, a: SockAddr) {
      log('bind', h, a);
      return b.errno;
    },
    listen(h: number, backlog: number) {
      log('listen', h, backlog);
      return b.errno;
    },
    connect(h: number, a: SockAddr) {
      log('connect', h, a);
      return b.errno;
    },
    accept(h: number) {
      log('accept', h);
      return { errno: b.errno, handle: 88 };
    },
    recv(h: number, max: number) {
      log('recv', h, max);
      return { errno: b.errno, data: b.nextRecv ?? new Uint8Array(0) };
    },
    recvFrom(h: number, max: number) {
      log('recvFrom', h, max);
      return {
        errno: b.errno,
        data: b.nextRecv ?? new Uint8Array(0),
        from: { ip: '9.8.7.6', port: 53, v6: false },
      };
    },
    send(h: number, data: Uint8Array, to?: SockAddr) {
      log('send', h, [...data], to);
      return { errno: b.errno, sent: data.length };
    },
    addr(h: number, peer: boolean) {
      log('addr', h, peer);
      return { errno: b.errno, addr: { ip: '1.2.3.4', port: 4242, v6: false } };
    },
    getsockopt(h: number, level: number, name: number) {
      log('getsockopt', h, level, name);
      return { errno: b.errno, value: 1 };
    },
    setsockopt(h: number, level: number, name: number, value: number) {
      log('setsockopt', h, level, name, value);
      return b.errno;
    },
    shutdown(h: number, how: number) {
      log('shutdown', h, how);
      return b.errno;
    },
    close(h: number) {
      log('close', h);
      return b.errno;
    },
    wait() {
      return [];
    },
  };
  return { b, calls };
}

function abi(pool?: AddressPool) {
  const memory = new WebAssembly.Memory({ initial: 2 });
  const { b, calls } = recordingBackend();
  const notified: [number, number][] = [];
  const imports = netV1({
    memory: () => memory,
    backend: b,
    capabilities: CAP.RELAY | CAP.DATAGRAM,
    notify: (h, ev) => notified.push([h, ev]),
    pool,
  });
  const fn = (name: string) => imports[name] as (...a: number[]) => number;
  return { fn, b, calls, notified, mem: () => new Mem(memory) };
}

// --- the address pool -------------------------------------------------------

test('a name always mints the same address, and distinct names differ', () => {
  const p = new AddressPool();
  const a = p.mint('example.test');
  assert.equal(p.mint('example.test'), a, 'a second lookup must not mint a new address');
  assert.notEqual(p.mint('other.test'), a);
  assert.ok(AddressPool.isSynthetic(a), `${a} must be inside the reserved range`);
});

test('an address reverses to its name, and only its own', () => {
  const p = new AddressPool();
  const a = p.mint('example.test');
  assert.equal(p.lookup(a), 'example.test');
  assert.equal(p.lookup('240.9.9.9'), undefined, 'an address this pool never minted is not ours');
  assert.equal(p.lookup('1.2.3.4'), undefined);
});

test('isSynthetic covers the reserved range and nothing outside it', () => {
  // ⚠️ 240.0.0.0/4 is reserved and never routable, which is the whole reason it
  // is safe to mint from: a synthetic address cannot collide with a real
  // destination the guest might legitimately reach.
  assert.ok(AddressPool.isSynthetic('240.0.0.1'));
  assert.ok(AddressPool.isSynthetic('255.1.2.3'));
  assert.ok(!AddressPool.isSynthetic('239.255.255.255'));
  assert.ok(!AddressPool.isSynthetic('127.0.0.1'));
  assert.ok(!AddressPool.isSynthetic('8.8.8.8'));
});

// --- the ABI ----------------------------------------------------------------

test('net_init reports the capabilities this host offers', () => {
  const { fn, mem } = abi();
  assert.equal(fn('net_init')(0), E.SUCCESS);
  assert.equal(fn('net_init')(16), E.SUCCESS);
  assert.equal(mem().u32(16), CAP.RELAY | CAP.DATAGRAM);
  assert.equal(mem().u32(16) & CAP.FETCH_PROXY, 0, 'a capability not offered must not be claimed');
});

test('net_resolve mints an address and writes it as four octets', () => {
  const pool = new AddressPool();
  const { fn, mem } = abi(pool);
  const name = 'example.test';
  const m0 = mem();
  m0.write(0, new TextEncoder().encode(name));

  assert.equal(fn('net_resolve')(0, name.length, 0, 64, 80), E.SUCCESS);
  const m = mem();
  assert.equal(m.u32(80), 4, 'an A record is four octets');
  const octets = [...m.read(64, 4)];
  assert.equal(octets.join('.'), pool.mint(name), 'the wire answer must be the minted address');
});

test('net_resolve refuses AAAA and refuses to invent an address without a pool', () => {
  const { fn, mem } = abi(new AddressPool());
  mem().write(0, new TextEncoder().encode('x.test'));
  // ⚠️ NXDOMAIN for AAAA is deliberate: every resolver falls back to A, and
  // minting a v6 range too would double the mapping for nothing.
  assert.equal(fn('net_resolve')(0, 6, 1, 64, 80), E.NOTSUP);

  const noPool = abi();
  noPool.mem().write(0, new TextEncoder().encode('x.test'));
  assert.equal(noPool.fn('net_resolve')(0, 6, 0, 64, 80), E.NOTSUP);
});

/**
 * ⚠️ THE REVERSE LOOKUP IS THE WHOLE POINT OF THE POOL. `connect(2)` carries an
 * address and nothing else -- the name was consumed by the guest's resolver and
 * thrown away -- but every browser transport needs the NAME: a URL for `fetch`,
 * a host for a relay's CONNECT, an SNI for TLS. Without this the address routes
 * nowhere, because 240/4 is reserved and unroutable by design.
 */
test('connecting to a minted address hands the backend the name', () => {
  const pool = new AddressPool();
  const addr = pool.mint('upstream.test');
  const { fn, calls, mem } = abi(pool);

  mem().write(0, new Uint8Array(addr.split('.').map(Number)));
  assert.equal(fn('net_connect')(5, 0, 4, 443), E.SUCCESS);

  const c = calls.find((x) => x.op === 'connect')!;
  assert.equal((c.args[1] as SockAddr).ip, 'upstream.test');
  assert.equal((c.args[1] as SockAddr).port, 443, 'the port survives the substitution');
});

test('connecting to an ordinary address is passed through untouched', () => {
  const pool = new AddressPool();
  pool.mint('somewhere.test'); // so the pool is non-empty but irrelevant
  const { fn, calls, mem } = abi(pool);

  mem().write(0, new Uint8Array([127, 0, 0, 1]));
  assert.equal(fn('net_connect')(5, 0, 4, 8080), E.SUCCESS);
  assert.equal((calls.find((x) => x.op === 'connect')!.args[1] as SockAddr).ip, '127.0.0.1');
});

test('a synthetic address the pool never minted is not rewritten', () => {
  // Reversing it would be inventing a name; passing it through lets the
  // transport refuse it, which is the honest outcome.
  const { fn, calls, mem } = abi(new AddressPool());
  mem().write(0, new Uint8Array([240, 5, 5, 5]));
  fn('net_connect')(5, 0, 4, 80);
  assert.equal((calls.find((x) => x.op === 'connect')!.args[1] as SockAddr).ip, '240.5.5.5');
});

test('a successful connect and a fresh accept report readiness', () => {
  const { fn, notified, mem } = abi();
  mem().write(0, new Uint8Array([127, 0, 0, 1]));
  fn('net_connect')(5, 0, 4, 80);
  assert.deepEqual(notified, [[5, READY.WRITE]], 'a completed connect is writable');

  notified.length = 0;
  fn('net_accept')(5, 32);
  // ⚠️ A freshly accepted connection may ALREADY hold bytes -- the request that
  // caused the accept. Reporting only WRITE would park the guest on a read that
  // nothing will ever announce.
  assert.deepEqual(notified, [[88, READY.READ | READY.WRITE]]);
  assert.equal(mem().u32(32), 88, 'the new handle is written out');
});

test('recv and send move the bytes and the counts', () => {
  const { fn, b, calls, mem } = abi();
  b.nextRecv = new TextEncoder().encode('hello');
  assert.equal(fn('net_recv')(5, 128, 64, 160), E.SUCCESS);
  const m = mem();
  assert.equal(m.u32(160), 5, 'the byte count must be reported, not just the data');
  assert.equal(new TextDecoder().decode(m.read(128, 5)), 'hello');

  m.write(200, new TextEncoder().encode('bye'));
  assert.equal(fn('net_send')(5, 200, 3, 160), E.SUCCESS);
  assert.deepEqual(
    calls.find((x) => x.op === 'send')!.args[1],
    [...'bye'].map((c) => c.charCodeAt(0)),
  );
  assert.equal(mem().u32(160), 3);
});

/**
 * ⚠️ AN ERRNO MUST NOT COME WITH OUT-PARAMETERS. A guest that receives an error
 * does not read them, so writing anyway is invisible -- until a handle slot
 * holds a stale value from a failed call and the next success is attributed to
 * the wrong socket.
 */
test('a backend error is returned verbatim and writes nothing', () => {
  const { fn, b, mem } = abi();
  mem().setU32(32, 0xdeadbeef);
  b.errno = E.AGAIN;

  assert.equal(fn('net_socket')(0, 0, 32), E.AGAIN);
  assert.equal(mem().u32(32), 0xdeadbeef, 'the handle slot must be untouched on failure');
  assert.equal(fn('net_accept')(5, 32), E.AGAIN);
  assert.equal(mem().u32(32), 0xdeadbeef);
  assert.equal(fn('net_recv')(5, 128, 64, 160), E.AGAIN);
  assert.equal(fn('net_listen')(5, 16), E.AGAIN);
  assert.equal(fn('net_close')(5), E.AGAIN);
});

test('net_addr writes the address, its length and its port', () => {
  const { fn, mem } = abi();
  assert.equal(fn('net_addr')(5, 0, 64, 80, 96), E.SUCCESS);
  const m = mem();
  assert.equal([...m.read(64, 4)].join('.'), '1.2.3.4');
  assert.equal(m.u32(80), 4);
  assert.equal(m.u32(96), 4242);
});
