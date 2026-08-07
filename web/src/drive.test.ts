import assert from 'node:assert/strict';
import { test } from 'vitest';

import { ECV, drive } from './host.ts';
import type { Reentrant } from './host.ts';
import type { SockAddr, SocketBackend } from './wasi/sockets.ts';

/**
 * A backend with one socket that is always writable.
 *
 * That is not a contrivance -- it is what every CONNECTED socket looks like.
 * `wait` reporting a hit on every pass is the normal steady state, so any
 * driver behaviour conditioned on "the poll found something" runs forever.
 */
function alwaysWritable(): SocketBackend {
  const stub = () => 0;
  return {
    open: () => ({ errno: 0, handle: 1 }),
    bind: stub,
    listen: stub,
    connect: stub,
    accept: () => ({ errno: 0, handle: 0 }),
    recv: () => ({ errno: 0, data: new Uint8Array(0) }),
    recvFrom: () => ({ errno: 0, data: new Uint8Array(0) }),
    send: () => ({ errno: 0, sent: 0 }),
    addr: (_h: number, _p: boolean) => ({
      errno: 0,
      addr: undefined as SockAddr | undefined,
    }),
    getsockopt: () => ({ errno: 0, value: 0 }),
    setsockopt: stub,
    shutdown: stub,
    close: stub,
    liveHandles: () => [1],
    // The write subscription for handle 1 -- index 1 in the [read, write] pair
    // `pollTransport` builds. Always ready, like any connected socket.
    wait: () => [1],
  };
}

/** A guest that goes idle with no deadline, then exits. */
function idleThenExit(n: number): Reentrant {
  let calls = 0;
  return {
    ecv_boot: () => 0,
    ecv_run_slice: () => (++calls >= n ? ECV.EXITED : ECV.IDLE),
    ecv_next_deadline_in_ms: () => -1,
    ecv_exit_code: () => 0,
    ecv_net_ready: () => {},
  };
}

test('the driver turns the event loop before re-entering the guest', async () => {
  // ⚠️ THIS IS THE WHOLE TEST. A macrotask scheduled before the driver starts
  // must have run by the time it finishes. It is not a style preference: the
  // event loop is what delivers WebSocket messages, so a driver that re-enters
  // the guest without yielding can never receive the readiness it is idle
  // waiting for. It DEADLOCKS, and only once a socket is connected -- which
  // reads as a transport bug rather than a driver bug.
  //
  // The bug this guards was exactly `if (polled) continue;` with no `await`.
  let turned = false;
  setTimeout(() => {
    turned = true;
  }, 0);

  const r = await drive(idleThenExit(4), alwaysWritable(), [], 0);

  assert.equal(r.exitCode, 0);
  assert.ok(r.idleWaits > 0, 'the guest must actually have gone idle');
  assert.ok(
    turned,
    'the driver ran every slice without yielding: a queued macrotask never ran, ' +
      'so no socket event could have been delivered',
  );
});
