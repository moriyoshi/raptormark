import assert from 'node:assert/strict';
import { test } from 'vitest';

import { E } from '../wasi/abi.ts';
import { RelaySockets } from './relay.ts';

const make = (handleBase: number) =>
  new RelaySockets({ url: 'ws://127.0.0.1:1/relay', handleBase, notify: () => {} });

test('a relay handle beyond the u16 stream id is refused, not truncated', () => {
  // ⚠️ THIS IS NOT A CAPACITY LIMIT, IT IS A CORRECTNESS ONE. The stream id is
  // u16 on the wire, so handle 2_000_000 does not overflow into a big number --
  // it becomes id 33920, a DIFFERENT stream. That really happened: the relay
  // connected to the upstream and answered about 33920 while this side had
  // filed the stream under 2_000_000, so every reply was dropped and the guest
  // hung with a successful connection sitting in the relay's log.
  const over = make(2_000_000);
  assert.equal(over.open(false, false).errno, E.MFILE);

  const ok = make(1);
  assert.equal(ok.open(false, false).errno, E.SUCCESS);
});

test('a datagram handle is minted rather than refused', () => {
  // See the DNS trap: the tap lives in the runtime and needs the socket to
  // exist before it can intercept, so ENOTSUP here breaks `getaddrinfo`.
  const r = make(1);
  assert.equal(r.open(false, true).errno, E.SUCCESS);
});
