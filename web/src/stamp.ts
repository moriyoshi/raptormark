/**
 * Host-side timestamps for markers a guest prints.
 *
 * ⚠️ THIS EXISTS SO A MEASUREMENT DOES NOT DEPEND ON THE CLOCK IT MEASURES.
 * `HOST-AFTER-STEP-MS` (bin/run.ts) is the same idea for a different question:
 * the only clock in a run that neither the guest nor a test instrument can move
 * is the host's. A guest that times itself with `clock_gettime` is fine against
 * a clock that runs at the wrong RATE -- both ends of the bracket scale -- and
 * useless against a clock that advances per READ, where two reads bracketing
 * 200000 syscalls land in the same tick and report an elapsed time of zero.
 *
 * The mechanism: the guest prints `<prefix> <NAME>` on its own line and flushes.
 * `fd_write` is a synchronous import, so the host's `onOutput` callback runs at
 * the instant the guest reached that line -- inside the same slice, before the
 * guest gets control back. Sampling `performance.now()` there is a host-side
 * timestamp of a guest-side event, accurate to the cost of one `fd_write`.
 *
 * Stamps are emitted as `HOST-STAMP-<NAME>-US: <microseconds>`, which is the
 * shape `clockValue` in `e2e/clock_test.go` parses. Microseconds rather than
 * milliseconds because the consumer divides by an iteration count: at
 * millisecond resolution a 20 ms loop of 200000 calls quantises to 5 ns/call,
 * which is a visible fraction of the number being reported.
 */

export interface StamperOptions {
  /** Line prefix that introduces a marker, e.g. `BENCH-MARK`. */
  prefix: string;
  /** A MONOTONIC host clock, in microseconds. `performance.now() * 1000`. */
  nowUs: () => number;
  /** Receives each finished line to report, without a trailing newline. */
  emit: (line: string) => void;
}

/**
 * Marker names are restricted to what the Go side can parse back out
 * (`reClockValue` matches `^[A-Z][A-Z0-9-]*:? +-?\d+`). A name outside it would
 * be stamped into a line no consumer can find, so it is reported instead of
 * being dropped: a silently missing stamp is exactly the failure this whole
 * mechanism exists to make impossible.
 */
const NAME_OK = /^[A-Z][A-Z0-9-]*$/;

/**
 * A line longer than this cannot be a marker worth waiting for. The cap bounds
 * the memory a guest that never prints a newline can cost us; markers are short
 * by construction.
 */
const MAX_LINE = 512;

/**
 * Builds an `onOutput` observer that stamps marker lines with the host clock.
 *
 * Returns a function with the `RunOptions.onOutput` signature, so it composes
 * with whatever else the host does with the output. Call it FIRST: the stamp
 * should be the first thing that happens after the bytes arrive, not after they
 * have been written to a pipe.
 */
export function outputStamper(opts: StamperOptions): (fd: number, text: string) => void {
  // Per-fd, because stdout and stderr are separate streams and a partial line
  // on one says nothing about the other. `live` marks a line still capable of
  // being a marker; once it cannot be, the rest of it is discarded unbuffered.
  const state = new Map<number, { buf: string; live: boolean }>();

  const couldMatch = (b: string): boolean =>
    b.length > MAX_LINE
      ? false
      : b.length < opts.prefix.length
        ? opts.prefix.startsWith(b)
        : b.startsWith(opts.prefix);

  const stamp = (line: string, us: number): void => {
    if (!line.startsWith(opts.prefix)) return;
    const rest = line.slice(opts.prefix.length);
    // The prefix must be a whole token: `BENCH-MARKER x` is not `BENCH-MARK x`.
    if (rest.length > 0 && rest[0] !== ' ' && rest[0] !== '\t') return;
    const name = rest.trim().split(/\s+/)[0] ?? '';
    if (!NAME_OK.test(name)) {
      opts.emit(`HOST-NOTE: marker name ${JSON.stringify(name)} is not stampable`);
      return;
    }
    opts.emit(`HOST-STAMP-${name}-US: ${us}`);
  };

  return (fd: number, text: string): void => {
    // ⚠️ SAMPLE THE CLOCK BEFORE ANY WORK. Everything below -- splitting,
    // matching, emitting -- happens after the guest reached the marker, so it
    // belongs to neither interval.
    const us = Math.round(opts.nowUs());

    let st = state.get(fd) ?? { buf: '', live: true };
    let s = text;
    for (;;) {
      const nl = s.indexOf('\n');
      const chunk = nl < 0 ? s : s.slice(0, nl);
      if (st.live) st.buf += chunk;
      if (nl < 0) {
        // The line is still open. A partial that can no longer grow into the
        // prefix will never match, so stop accumulating it.
        if (st.live && !couldMatch(st.buf)) st = { buf: '', live: false };
        break;
      }
      // A line completed IN THIS CHUNK, so `us` is when it arrived. A marker
      // split across two writes is stamped by the write that finished it.
      if (st.live) stamp(st.buf.replace(/\r$/, ''), us);
      st = { buf: '', live: true };
      s = s.slice(nl + 1);
    }
    state.set(fd, st);
  };
}
