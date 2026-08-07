import assert from 'node:assert/strict';
import { test } from 'vitest';

import { outputStamper } from './stamp.ts';

/**
 * A stamper over a clock the test drives, so the assertions are about WHICH
 * instant a line was stamped at rather than about how fast the machine is.
 */
function harness(prefix = 'BENCH-MARK') {
  const out: string[] = [];
  let clock = 0;
  const stamper = outputStamper({
    prefix,
    nowUs: () => clock,
    emit: (line) => out.push(line),
  });
  return {
    out,
    at(us: number, fd: number, text: string) {
      clock = us;
      stamper(fd, text);
    },
  };
}

test('a marker line is stamped with the host clock at the moment it arrives', () => {
  const h = harness();
  h.at(1000, 1, 'BENCH-MARK CLOCK-BEGIN\n');
  h.at(21_000, 1, 'BENCH-MARK CLOCK-END\n');
  assert.deepEqual(h.out, ['HOST-STAMP-CLOCK-BEGIN-US: 1000', 'HOST-STAMP-CLOCK-END-US: 21000']);
});

test('ordinary guest output is not stamped', () => {
  const h = harness();
  h.at(5, 1, 'hello\nBENCH-CLOCK-NS 41\n');
  h.at(6, 2, 'some stderr chatter\n');
  assert.deepEqual(h.out, []);
});

test('the prefix must be a whole token', () => {
  const h = harness();
  // ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A plain
  // `startsWith` accepts both of these, and the second would be stamped under
  // the name `X` -- a stamp that exists, parses, and means nothing.
  h.at(1, 1, 'BENCH-MARKER X\n');
  h.at(2, 1, 'BENCH-MARKED X\n');
  assert.deepEqual(h.out, []);
});

test('a marker split across two writes is stamped by the write that finished it', () => {
  const h = harness();
  // ecvisor writes when it has bytes, not on line boundaries, so this is a
  // shape the real host sees. The stamp must be the SECOND time: that is when
  // the guest had finished the line.
  h.at(100, 1, 'BENCH-MARK GET');
  h.at(140, 1, 'PID-END\nnoise\n');
  assert.deepEqual(h.out, ['HOST-STAMP-GETPID-END-US: 140']);
});

test('stdout and stderr are tracked separately', () => {
  const h = harness();
  // An interleaved partial on the other fd must not corrupt this line.
  h.at(10, 1, 'BENCH-MARK A');
  h.at(11, 2, 'log: something happened\n');
  h.at(12, 1, '\n');
  assert.deepEqual(h.out, ['HOST-STAMP-A-US: 12']);
});

test('an unparseable marker name is reported, not dropped', () => {
  const h = harness();
  // `clockValue` only finds `[A-Z][A-Z0-9-]*`, so a lowercase name would be
  // stamped into a line no test can look up. Silence there is the exact failure
  // mode this mechanism exists to remove, so it has to say something.
  h.at(3, 1, 'BENCH-MARK phase1\n');
  assert.deepEqual(h.out, ['HOST-NOTE: marker name "phase1" is not stampable']);
});

test('a guest that never emits a newline does not grow the buffer without bound', () => {
  const h = harness();
  // Not a memory-accounting test: the buffer is only observable through
  // behaviour, and the behaviour that matters is that a marker AFTER the flood
  // is still found.
  for (let i = 0; i < 1000; i++) h.at(i, 1, 'x'.repeat(1024));
  h.at(2000, 1, '\nBENCH-MARK AFTER\n');
  assert.deepEqual(h.out, ['HOST-STAMP-AFTER-US: 2000']);
});
