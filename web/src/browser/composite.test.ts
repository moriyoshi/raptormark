import assert from 'node:assert/strict';
import { test } from 'vitest';

import { E } from '../wasi/abi.ts';
import type { SockAddr, SocketBackend } from '../wasi/sockets.ts';
import { CompositeSockets } from './composite.ts';

/**
 * A backend that records what it was asked and hands out handles from `base`.
 *
 * ⚠️ THE TWO FAKES DELIBERATELY USE THE SAME `base` in most tests, so their
 * handle numbers COLLIDE. That is the condition a single inner->outer map gets
 * wrong, and it is the normal case in production: the two real backends
 * allocate independently and neither knows the other exists.
 */
class Fake implements SocketBackend {
  readonly log: string[] = [];
  readonly opts: Array<[number, number, number, number]> = [];
  readonly binds: Array<[number, SockAddr]> = [];
  notify!: (h: number, ev: number) => void;
  ready = new Set<string>();
  private next: number;

  readonly name: string;

  constructor(name: string, base = 1000) {
    // ⚠️ Not a parameter property: `erasableSyntaxOnly` is on, because node
    // strips types rather than compiling them and cannot synthesize the field.
    this.name = name;
    this.next = base;
  }

  open(): { errno: number; handle: number } {
    const h = this.next++;
    this.log.push(`open->${h}`);
    return { errno: E.SUCCESS, handle: h };
  }
  bind(h: number, a: SockAddr): number {
    this.binds.push([h, a]);
    this.log.push(`bind(${h},${a.ip}:${a.port})`);
    return E.SUCCESS;
  }
  listen(h: number): number {
    this.log.push(`listen(${h})`);
    return E.SUCCESS;
  }
  connect(h: number, a: SockAddr): number {
    this.log.push(`connect(${h},${a.ip}:${a.port})`);
    return E.SUCCESS;
  }
  accept(h: number): { errno: number; handle: number } {
    const c = this.next++;
    this.log.push(`accept(${h})->${c}`);
    return { errno: E.SUCCESS, handle: c };
  }
  recv(h: number): { errno: number; data: Uint8Array } {
    this.log.push(`recv(${h})`);
    return { errno: E.SUCCESS, data: new TextEncoder().encode(`${this.name}:${h}`) };
  }
  recvFrom(h: number): { errno: number; data: Uint8Array } {
    return this.recv(h);
  }
  send(h: number, d: Uint8Array): { errno: number; sent: number } {
    this.log.push(`send(${h},${new TextDecoder().decode(d)})`);
    return { errno: E.SUCCESS, sent: d.length };
  }
  addr(): { errno: number; addr?: SockAddr } {
    return { errno: E.SUCCESS, addr: { ip: '1.2.3.4', port: 9, v6: false } };
  }
  getsockopt(): { errno: number; value: number } {
    return { errno: E.SUCCESS, value: 0 };
  }
  setsockopt(h: number, level: number, name: number, value: number): number {
    this.opts.push([h, level, name, value]);
    return E.SUCCESS;
  }
  shutdown(h: number): number {
    this.log.push(`shutdown(${h})`);
    return E.SUCCESS;
  }
  close(h: number): number {
    this.log.push(`close(${h})`);
    return E.SUCCESS;
  }
  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>): number[] {
    const hits: number[] = [];
    subs.forEach((s, i) => {
      if (this.ready.has(`${s.fd}:${s.write ? 'w' : 'r'}`)) hits.push(i);
    });
    return hits;
  }
  liveHandles(): number[] {
    return [];
  }
}

function make(base = 1000) {
  const notified: Array<[number, number]> = [];
  let li!: Fake;
  let di!: Fake;
  const c = new CompositeSockets<Fake, Fake>({
    handleBase: 64,
    notify: (h, ev) => notified.push([h, ev]),
    listen: (n) => {
      li = new Fake('L', base);
      li.notify = n;
      return li;
    },
    dial: (n) => {
      di = new Fake('D', base);
      di.notify = n;
      return di;
    },
  });
  return { c, li, di, notified };
}

const addr = (ip: string, port: number): SockAddr => ({ ip, port, v6: false });

test('a socket touches neither backend until its direction is known', () => {
  const { c, li, di } = make();
  const s = c.open(false, false);
  assert.equal(s.errno, E.SUCCESS);
  // ⚠️ The whole point: `socket()` is identical for a server and a client.
  assert.deepEqual(li.log, []);
  assert.deepEqual(di.log, []);
});

test('listen routes to the listen backend and replays bind and sockopts', () => {
  const { c, li, di } = make();
  const { handle } = c.open(false, false);

  assert.equal(c.setsockopt(handle, 1, 2, 1), E.SUCCESS);
  assert.equal(c.bind(handle, addr('0.0.0.0', 8080)), E.SUCCESS);
  assert.equal(c.listen(handle, 16), E.SUCCESS);

  assert.deepEqual(di.log, [], 'the dial backend must not have been touched');
  // SO_REUSEADDR is set BEFORE bind, so it must be replayed before it too.
  assert.deepEqual(li.opts, [[1000, 1, 2, 1]]);
  assert.deepEqual(li.log, ['open->1000', 'bind(1000,0.0.0.0:8080)', 'listen(1000)']);
});

test('connect routes to the dial backend', () => {
  const { c, li, di } = make();
  const { handle } = c.open(false, false);
  assert.equal(c.connect(handle, addr('example.test', 443)), E.SUCCESS);
  assert.deepEqual(li.log, [], 'the listen backend must not have been touched');
  assert.deepEqual(di.log, ['open->1000', 'connect(1000,example.test:443)']);
});

test('a server and a client coexist even when the backends hand out the same numbers', () => {
  const { c } = make(1000);
  const srv = c.open(false, false).handle;
  c.bind(srv, addr('0.0.0.0', 80));
  c.listen(srv, 8);
  const cli = c.open(false, false).handle;
  c.connect(cli, addr('up.test', 9));

  // Both inner handles are 1000. The outer handles must still be distinct and
  // must route to different backends.
  assert.notEqual(srv, cli);
  assert.equal(new TextDecoder().decode(c.recv(srv, 99).data), 'L:1000');
  assert.equal(new TextDecoder().decode(c.recv(cli, 99).data), 'D:1000');
});

test('readiness is translated from inner handles to outer ones', () => {
  const { c, li, di, notified } = make(1000);
  const srv = c.open(false, false).handle;
  c.bind(srv, addr('0.0.0.0', 80));
  c.listen(srv, 8);
  const cli = c.open(false, false).handle;
  c.connect(cli, addr('up.test', 9));

  // ⚠️ BOTH BACKENDS NOTIFY HANDLE 1000. Untranslated, the guest would be told
  // that socket 1000 -- which is not even in its handle space -- became ready,
  // and one of the two would be attributed to the wrong socket.
  li.notify(1000, 1);
  di.notify(1000, 2);
  assert.deepEqual(notified, [
    [srv, 1],
    [cli, 2],
  ]);
});

test('a notification for a closed handle is dropped, not misrouted', () => {
  const { c, li, notified } = make();
  const srv = c.open(false, false).handle;
  c.bind(srv, addr('0.0.0.0', 80));
  c.listen(srv, 8);
  c.close(srv);
  li.notify(1000, 1);
  assert.deepEqual(notified, []);
});

test('an accepted socket gets its own outer handle and routes back correctly', () => {
  const { c, li } = make();
  const srv = c.open(false, false).handle;
  c.bind(srv, addr('0.0.0.0', 80));
  c.listen(srv, 8);

  const a = c.accept(srv);
  assert.equal(a.errno, E.SUCCESS);
  // ⚠️ Not the inner handle: that number belongs to the sub-backend's space and
  // can collide with a live outer handle.
  assert.notEqual(a.handle, 1001);
  assert.equal(new TextDecoder().decode(c.recv(a.handle, 9).data), 'L:1001');

  c.send(a.handle, new TextEncoder().encode('hi'));
  assert.ok(li.log.includes('send(1001,hi)'));
});

test('wait partitions across backends and maps indices back', () => {
  const { c, li, di } = make(1000);
  const srv = c.open(false, false).handle;
  c.bind(srv, addr('0.0.0.0', 80));
  c.listen(srv, 8);
  const cli = c.open(false, false).handle;
  c.connect(cli, addr('up.test', 9));

  const subs = [
    { fd: srv, write: false }, // index 0 -> listen backend
    { fd: cli, write: false }, // index 1 -> dial backend
    { fd: cli, write: true }, // index 2 -> dial backend
  ];

  // Only the dial backend's WRITE subscription is ready. It sits at index 1 of
  // the slice sent to that backend, so an untranslated result would report
  // index 1 -- the caller's `cli` READ subscription.
  di.ready.add('1000:w');
  assert.deepEqual(c.wait(subs, 0), [2]);

  li.ready.add('1000:r');
  assert.deepEqual(c.wait(subs, 0), [0, 2]);
});

test('a direction, once taken, cannot be reversed', () => {
  const { c } = make();
  const srv = c.open(false, false).handle;
  c.listen(srv, 8);
  assert.equal(c.connect(srv, addr('x', 1)), E.INVAL);

  const cli = c.open(false, false).handle;
  c.connect(cli, addr('x', 1));
  assert.equal(c.listen(cli, 8), E.INVAL);
});

test('connect is refused when no egress backend was configured', () => {
  let li!: Fake;
  const c = new CompositeSockets<Fake, Fake>({
    handleBase: 64,
    notify: () => {},
    listen: (n) => {
      li = new Fake('L');
      li.notify = n;
      return li;
    },
  });
  const h = c.open(false, false).handle;
  // ⚠️ Refused loudly. A `connect` that silently went nowhere would look like an
  // unreachable destination rather than a host with no egress transport.
  assert.equal(c.connect(h, addr('x', 1)), E.NOTSUP);
});

test('only materialized sockets are reported live', () => {
  const { c } = make();
  const undecided = c.open(false, false).handle;
  assert.deepEqual(c.liveHandles(), []);
  c.listen(undecided, 8);
  assert.deepEqual(c.liveHandles(), [undecided]);
});
