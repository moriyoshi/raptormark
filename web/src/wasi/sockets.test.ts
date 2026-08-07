import assert from 'node:assert/strict';
import { test } from 'vitest';

import { E } from './abi.ts';
import { Mem } from './mem.ts';
import { NullSockets, socketAwareRw } from './sockets.ts';
import type { SockAddr, SocketBackend } from './sockets.ts';

const enc = (s: string) => new TextEncoder().encode(s);
const dec = (b: Uint8Array) => new TextDecoder().decode(b);

/** A backend that hands back queued bytes and records what it was sent. */
function ioBackend(): SocketBackend & { queued: Uint8Array; sent: Uint8Array[]; errno: number } {
  const b = {
    queued: new Uint8Array(0),
    sent: [] as Uint8Array[],
    errno: E.SUCCESS,
    open: () => ({ errno: E.SUCCESS, handle: 0 }),
    bind: () => E.SUCCESS,
    listen: () => E.SUCCESS,
    connect: () => E.SUCCESS,
    accept: () => ({ errno: E.SUCCESS, handle: 0 }),
    recv: (_h: number, max: number) => {
      if (b.errno !== E.SUCCESS) return { errno: b.errno, data: new Uint8Array(0) };
      const data = b.queued.subarray(0, max);
      b.queued = b.queued.subarray(data.length);
      return { errno: E.SUCCESS, data };
    },
    recvFrom: () => ({ errno: E.NOTSUP, data: new Uint8Array(0) }),
    send: (_h: number, data: Uint8Array, _to?: SockAddr) => {
      if (b.errno !== E.SUCCESS) return { errno: b.errno, sent: 0 };
      b.sent.push(data.slice());
      return { errno: E.SUCCESS, sent: data.length };
    },
    addr: () => ({ errno: E.NOTSUP }),
    getsockopt: () => ({ errno: E.NOTSUP, value: 0 }),
    setsockopt: () => E.NOTSUP,
    shutdown: () => E.SUCCESS,
    close: () => E.SUCCESS,
    wait: () => [],
  };
  return b;
}

/** Lays out `iovec`s at `at`, with their buffers packed after them. */
function iovecs(m: Mem, at: number, sizes: number[], bufBase: number): number {
  let buf = bufBase;
  sizes.forEach((n, i) => {
    m.setU32(at + i * 8, buf);
    m.setU32(at + i * 8 + 4, n);
    buf += n;
  });
  return sizes.length;
}

function rw(backend: SocketBackend, socketBase = 4096) {
  const memory = new WebAssembly.Memory({ initial: 2 });
  const fileCalls: string[] = [];
  const imports = socketAwareRw(
    () => memory,
    backend,
    (fd) => fd >= socketBase,
    (fd) => {
      fileCalls.push(`read:${fd}`);
      return E.SUCCESS;
    },
    (fd) => {
      fileCalls.push(`write:${fd}`);
      return E.SUCCESS;
    },
  );
  return {
    read: imports['fd_read'] as (fd: number, iovs: number, n: number, out: number) => number,
    write: imports['fd_write'] as (fd: number, iovs: number, n: number, out: number) => number,
    fileCalls,
    mem: () => new Mem(memory),
  };
}

/**
 * ⚠️ ONE `fd_read` SERVES TWO WORLDS. A guest reads its sidecar and its sockets
 * through the same import, and the only thing separating them is the handle
 * range. Routing a socket to the file layer returns EBADF for a live connection;
 * routing a file to the backend hands the sidecar read to a socket that does not
 * exist. Both look like the other subsystem failing.
 */
test('fd_read and fd_write route by handle range, not by guesswork', () => {
  const b = ioBackend();
  const { read, write, fileCalls, mem } = rw(b);
  const m = mem();
  iovecs(m, 0, [8], 512);

  assert.equal(read(3, 0, 1, 256), E.SUCCESS);
  assert.equal(write(1, 0, 1, 256), E.SUCCESS);
  assert.deepEqual(fileCalls, ['read:3', 'write:1'], 'below the base is the file layer');

  b.queued = enc('sock');
  assert.equal(read(4096, 0, 1, 256), E.SUCCESS);
  assert.equal(write(4096, 0, 1, 256), E.SUCCESS);
  assert.deepEqual(fileCalls, ['read:3', 'write:1'], 'at or above the base must not reach files');
  assert.equal(b.sent.length, 1, 'the socket write reached the backend');
});

/**
 * ⚠️ A SHORT READ MUST BE REPORTED AS SHORT. `recv` returning fewer bytes than
 * the iovecs can hold is the normal case on a stream socket, and a `nread` of
 * the REQUESTED length would have the guest read uninitialised memory as data --
 * silently, and differently on every run.
 */
test('a scattered read fills the iovecs in order and reports what arrived', () => {
  const b = ioBackend();
  b.queued = enc('abcdefg');
  const { read, mem } = rw(b);
  const m = mem();
  iovecs(m, 0, [3, 3, 3], 512);

  assert.equal(read(4096, 0, 3, 256), E.SUCCESS);
  const m2 = mem();
  assert.equal(m2.u32(256), 7, 'seven bytes arrived, not the nine the buffers could hold');
  assert.equal(dec(m2.read(512, 3)), 'abc');
  assert.equal(dec(m2.read(515, 3)), 'def');
  assert.equal(dec(m2.read(518, 1)), 'g');
});

test('a gathered write concatenates the iovecs into one send', () => {
  const b = ioBackend();
  const { write, mem } = rw(b);
  const m = mem();
  iovecs(m, 0, [5, 6], 512);
  m.write(512, enc('HTTP/'));
  m.write(517, enc('1.1 OK'));

  assert.equal(write(4096, 0, 2, 256), E.SUCCESS);
  assert.equal(b.sent.length, 1, 'the pieces must reach the transport as ONE write');
  assert.equal(dec(b.sent[0]!), 'HTTP/1.1 OK');
  assert.equal(mem().u32(256), 11);
});

test('an empty read is success with zero, which is how a guest sees EOF', () => {
  const b = ioBackend();
  const { read, mem } = rw(b);
  iovecs(mem(), 0, [8], 512);
  assert.equal(read(4096, 0, 1, 256), E.SUCCESS);
  assert.equal(mem().u32(256), 0);
});

test('a backend error is returned and no count is written', () => {
  const b = ioBackend();
  b.errno = E.AGAIN;
  const { read, write, mem } = rw(b);
  const m = mem();
  iovecs(m, 0, [8], 512);
  m.setU32(256, 0xdeadbeef);

  assert.equal(read(4096, 0, 1, 256), E.AGAIN);
  assert.equal(mem().u32(256), 0xdeadbeef, 'a failed read must not claim a byte count');
  assert.equal(write(4096, 0, 1, 256), E.AGAIN);
  assert.equal(mem().u32(256), 0xdeadbeef);
});

/**
 * ⚠️ `NullSockets` EXISTS TO NAME WHAT A GUEST WANTED. Returning ENOTSUP alone
 * would be a guest that fails for no stated reason; the recorded names are what
 * `bin/run.ts` prints as `HOST-NOTE`, and they are how an unimplemented call is
 * discovered at all.
 */
test('the null backend records every name a guest reached', () => {
  const n = new NullSockets();
  assert.equal(n.open().errno, E.NOTSUP);
  assert.equal(n.connect(), E.NOTSUP);
  assert.equal(n.recv().errno, E.NOTSUP);
  assert.equal(n.send().errno, E.NOTSUP);

  assert.deepEqual(
    [...n.reached].sort(),
    ['sock_connect', 'sock_open', 'sock_recv', 'sock_send'],
    'the report must name the calls that were made and no others',
  );
  // A call never made must not appear: the point is telling a guest that needs
  // `sock_bind` from one that does not.
  assert.ok(!n.reached.has('sock_bind'));
});

test('the null backend is never ready, so a driver cannot spin on it', () => {
  // `wait` returning indices would have the driver re-enter a guest whose every
  // socket call fails, forever.
  assert.deepEqual(new NullSockets().wait(), []);
});
