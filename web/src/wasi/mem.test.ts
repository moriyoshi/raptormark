import assert from 'node:assert/strict';
import { test } from 'vitest';
import { Mem, gather, iovecTotal, scatter } from './mem.ts';

/**
 * The guard for the one invariant that actually breaks hosts: an accessor must
 * never hold a view across a `memory.grow`.
 *
 * ⚠️ WHAT A FALSE PASS WOULD LOOK LIKE. A test that only reads and writes
 * without ever growing passes against a fully cached implementation, because
 * nothing detaches. So growing between two calls is not incidental setup -- it
 * IS the test, and a version of this file without the `grow()` proves nothing.
 *
 * Neutralized 2026-08-19 by caching the view and buffer at module scope, the
 * way node:wasi caches its backing store. Result: `TypeError: Cannot perform
 * DataView.prototype.getUint32 on a detached or out-of-bounds ArrayBuffer`,
 * raised from `iovecTotal` under `std::fs::read` -- a behavioural failure, not
 * a compile error. Reverted immediately afterwards.
 */

function fresh(pages = 1): Mem {
  return new Mem(new WebAssembly.Memory({ initial: pages }));
}

test('reads and writes survive a memory.grow between calls', () => {
  const mem = fresh();
  mem.setU32(16, 0xdeadbeef);
  assert.equal(mem.u32(16), 0xdeadbeef);

  // Growing detaches the previous ArrayBuffer. A cached view dies here.
  mem.memory.grow(1);

  assert.equal(mem.u32(16), 0xdeadbeef, 'data written before the grow must still read back');
  mem.setU32(70000, 0x12345678);
  assert.equal(mem.u32(70000), 0x12345678, 'the newly mapped page must be writable');
});

test('iovec helpers survive a grow', () => {
  const mem = fresh();
  // Two iovecs at 0: {buf: 256, len: 3}, {buf: 512, len: 2}.
  mem.setU32(0, 256);
  mem.setU32(4, 3);
  mem.setU32(8, 512);
  mem.setU32(12, 2);
  mem.write(256, new Uint8Array([1, 2, 3]));
  mem.write(512, new Uint8Array([4, 5]));

  mem.memory.grow(1);

  assert.equal(iovecTotal(mem, 0, 2), 5);
  assert.deepEqual([...gather(mem, 0, 2)], [1, 2, 3, 4, 5]);
});

test('scatter reports a short read rather than overrunning', () => {
  const mem = fresh();
  mem.setU32(0, 256);
  mem.setU32(4, 4);
  mem.setU32(8, 512);
  mem.setU32(12, 4);

  // Only 6 bytes for 8 bytes of capacity: the second iovec is partly filled.
  const n = scatter(mem, 0, 2, new Uint8Array([1, 2, 3, 4, 5, 6]));
  assert.equal(n, 6, 'must report what it actually placed, not the capacity');
  assert.deepEqual([...mem.read(256, 4)], [1, 2, 3, 4]);
  assert.deepEqual([...mem.read(512, 2)], [5, 6]);
});

test('read returns a copy, not a live view', () => {
  const mem = fresh();
  mem.write(64, new Uint8Array([9, 9]));
  const got = mem.read(64, 2);
  mem.write(64, new Uint8Array([1, 1]));
  assert.deepEqual([...got], [9, 9], 'a returned buffer must not alias linear memory');
});
