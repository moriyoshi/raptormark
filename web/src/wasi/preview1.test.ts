import assert from 'node:assert/strict';
import { test } from 'vitest';

import { CLOCK, E } from './abi.ts';
import { Files } from './files.ts';
import { Mem } from './mem.ts';
import { preview1 } from './preview1.ts';

/**
 * The preview1 shim's clocks, argv/environ and `random_get`.
 *
 * ⚠️ THE CLOCK HALF EXISTS BECAUSE OF WHAT IT GUARDS. `realtimeOffsetMs` is the
 * instrument `TestGuestTimersSurviveAWallClockStep` uses to prove the runtime's
 * monotonic clock does not follow a wall-clock step. If the shim ever offset
 * BOTH clocks, that E2E would still pass -- both readings would move together,
 * the deltas would still look right, and the test would certify nothing while
 * staying green. Nothing else in the tree checks the instrument itself.
 */

function shim(opts: Partial<Parameters<typeof preview1>[0]> = {}) {
  const memory = new WebAssembly.Memory({ initial: 2 });
  const files = new Files('/');
  const imports = preview1({ memory: () => memory, files, ...opts });
  return { imports, mem: () => new Mem(memory) };
}

const read64 = (m: Mem, at: number) => m.view().getBigUint64(at, true);

test('the two clocks are different clocks', () => {
  const { imports, mem } = shim();
  const clock = imports['clock_time_get'] as (id: number, p: bigint, out: number) => number;

  assert.equal(clock(CLOCK.REALTIME, 0n, 0), E.SUCCESS);
  const real = read64(mem(), 0);
  assert.equal(clock(CLOCK.MONOTONIC, 0n, 8), E.SUCCESS);
  const mono = read64(mem(), 8);

  // REALTIME is unix time: seconds since the epoch, so ~1.7e18 ns and rising.
  assert.ok(real > 1_700_000_000_000_000_000n, `REALTIME reads ${real}, not a unix time`);
  // ⚠️ MONOTONIC IS NOT. It comes from `performance.now()`, which counts from
  // process start -- so it is small, and it is the magnitude that separates the
  // two. A shim that answered both from `Date.now()` would pass every
  // "time advances" check ever written.
  assert.ok(
    mono < 365n * 24n * 60n * 60n * 1_000_000_000n,
    `MONOTONIC reads ${mono}, which is more than a year: it is wall-clock time`,
  );
});

test('both clocks advance', async () => {
  const { imports, mem } = shim();
  const clock = imports['clock_time_get'] as (id: number, p: bigint, out: number) => number;
  clock(CLOCK.MONOTONIC, 0n, 0);
  const before = read64(mem(), 0);
  await new Promise((r) => setTimeout(r, 5));
  clock(CLOCK.MONOTONIC, 0n, 8);
  assert.ok(read64(mem(), 8) > before, 'a monotonic clock that never moves wedges every sleep');
});

/**
 * ⚠️ THIS PINS THE TEST INSTRUMENT, NOT THE PRODUCT. The asymmetry IS the
 * instrument: step REALTIME, leave MONOTONIC alone, and see whether a guest's
 * timers move. Both halves are asserted, because offsetting neither and
 * offsetting both are equally useless and equally invisible from the E2E.
 */
test('realtimeOffsetMs moves REALTIME and only REALTIME', () => {
  let offset = 0;
  const { imports, mem } = shim({ realtimeOffsetMs: () => offset });
  const clock = imports['clock_time_get'] as (id: number, p: bigint, out: number) => number;

  clock(CLOCK.REALTIME, 0n, 0);
  clock(CLOCK.MONOTONIC, 0n, 8);
  const real0 = read64(mem(), 0);
  const mono0 = read64(mem(), 8);

  offset = 3_600_000; // one hour, the step the E2E uses
  clock(CLOCK.REALTIME, 0n, 16);
  clock(CLOCK.MONOTONIC, 0n, 24);
  const real1 = read64(mem(), 16);
  const mono1 = read64(mem(), 24);

  const jumpedMs = Number((real1 - real0) / 1_000_000n);
  assert.ok(
    jumpedMs > 3_599_000 && jumpedMs < 3_610_000,
    `REALTIME moved ${jumpedMs} ms, not the ~3600000 the offset asked for`,
  );
  const monoMovedMs = Number((mono1 - mono0) / 1_000_000n);
  assert.ok(
    monoMovedMs < 1_000,
    `MONOTONIC moved ${monoMovedMs} ms. It must be untouched by the offset, or ` +
      'the clock-step E2E is stepping both clocks and proving nothing.',
  );
});

test('an unset offset leaves the clock alone', () => {
  const { imports, mem } = shim();
  const clock = imports['clock_time_get'] as (id: number, p: bigint, out: number) => number;
  clock(CLOCK.REALTIME, 0n, 0);
  const drift = Math.abs(Number(read64(mem(), 0) / 1_000_000n) - Date.now());
  assert.ok(drift < 1_000, `REALTIME is ${drift} ms from Date.now() with no offset configured`);
});

test('argv and environ round-trip through the two-call protocol', () => {
  const { imports, mem } = shim({ args: ['guest', '8080'], env: { A: '1', BB: 'two' } });
  const sizes = imports['args_sizes_get'] as (c: number, s: number) => number;
  const get = imports['args_get'] as (p: number, b: number) => number;

  assert.equal(sizes(0, 4), E.SUCCESS);
  const m = mem();
  assert.equal(m.u32(0), 2, 'argc');
  // Each string is NUL-terminated, so the buffer is the sum of the lengths + 1.
  assert.equal(m.u32(4), 'guest\0'.length + '8080\0'.length);

  assert.equal(get(64, 128), E.SUCCESS);
  const m2 = mem();
  const at = (i: number) => {
    const p = m2.u32(64 + i * 4);
    const bytes = m2.read(p, 16);
    const end = bytes.indexOf(0);
    return new TextDecoder().decode(bytes.subarray(0, end));
  };
  assert.equal(at(0), 'guest');
  assert.equal(at(1), '8080');

  const esizes = imports['environ_sizes_get'] as (c: number, s: number) => number;
  assert.equal(esizes(0, 4), E.SUCCESS);
  assert.equal(mem().u32(0), 2, 'two environment entries');
});

/**
 * ⚠️ `crypto.getRandomValues` THROWS above 65536 bytes. Chunking is not a
 * refinement: a single large draw is a hard failure, and ecvisor's own PRNG
 * seeding is exactly the kind of caller that asks for a big buffer once.
 */
test('random_get chunks past the 65536-byte limit', () => {
  const { imports, mem } = shim();
  const random = imports['random_get'] as (buf: number, len: number) => number;
  const len = 70_000;

  assert.equal(random(0, len), E.SUCCESS);
  const bytes = mem().read(0, len);
  assert.equal(bytes.length, len);

  // The tail is past the chunk boundary; an unchunked implementation would have
  // thrown, and one that filled only the first chunk would leave this zeroed.
  const tail = bytes.subarray(65_536);
  assert.ok(
    tail.some((b) => b !== 0),
    'everything past the first 65536-byte chunk is zero, so it was never filled',
  );
});
