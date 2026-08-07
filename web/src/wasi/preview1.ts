import {
  CLOCK,
  E,
  EVENT,
  EVENTTYPE,
  FILESTAT,
  PREOPENTYPE_DIR,
  PRESTAT,
  SUB,
  SUBSCRIPTION_CLOCK_ABSTIME,
} from './abi.ts';
import type { Files } from './files.ts';
import { Mem, gather, iovecTotal, scatter } from './mem.ts';

/**
 * Thrown out of `proc_exit`. The module ALWAYS leaves this way -- ecvisor calls
 * `std::process::exit` for a guest exit, for a detected deadlock
 * (`context.rs:3314`, code 111) and for a fatal error, and wasi-libc's
 * `_start` calls `exit()` after `main` returns too. So a clean run and a crash
 * both arrive here, and the caller must catch it.
 *
 * A throw that did NOT come through `proc_exit` is a real fault and must not be
 * swallowed -- hence a nominal class rather than a magic value.
 */
export class ProcExit extends Error {
  readonly code: number;
  constructor(code: number) {
    super(`proc_exit(${code})`);
    this.name = 'ProcExit';
    this.code = code;
  }
}

/**
 * What `poll_oneoff` delegates fd subscriptions to. Kept as an interface so the
 * file layer and the socket layer stay independent: with no sockets wired up,
 * `ready` returns nothing and only clock subscriptions can fire, which is
 * exactly the milestone-1 configuration.
 */
export interface FdPoll {
  /**
   * Returns the indices of `subs` that are ready, waiting up to `timeoutMs`
   * for one to become so. `0` is a pure probe and `-1` waits indefinitely.
   *
   * The WAIT belongs to the backend, not to this file. Polling here on a timer
   * would work, but it would put a fixed latency floor under every socket
   * wake-up and burn a round trip per slice; a backend that owns the sockets
   * can block on the real thing and return the instant it is ready.
   */
  wait(subs: ReadonlyArray<{ fd: number; write: boolean }>, timeoutMs: number): number[];
}

const NO_FDS: FdPoll = { wait: () => [] };

/**
 * Blocks the calling thread for `ms`.
 *
 * `Atomics.wait` on a SharedArrayBuffer is the only way to sleep synchronously
 * without spinning. It is legal in Node on any thread, and legal in a browser
 * WORKER -- but NOT on a browser main thread, where it throws. That asymmetry
 * is fine and expected: the browser profile never reaches this path, because
 * re-entrancy makes the host return to the event loop instead of sleeping. If
 * this throws in a browser, the re-entrant driver is not wired up.
 */
function sleepSync(ms: number): void {
  if (ms <= 0) return;
  const sab = new Int32Array(new SharedArrayBuffer(4));
  Atomics.wait(sab, 0, 0, ms);
}

export interface Preview1Options {
  memory: () => WebAssembly.Memory;
  files: Files;
  /** Guest argv. Normally irrelevant: ecvisor prefers the sidecar boot record
   *  and only falls back to host argv when there is none (`entry.rs:45-51`). */
  args?: string[];
  /** Host environment. This is ecvisor's OWN configuration -- the ~26
   *  `RAPTORMARK_ECV_*` knobs read once in `sys::init_diag_flags`
   *  (`sys.rs:52-116`) plus `RAPTORMARK_ROOTFS` -- and NOT the guest's
   *  environment, which comes from the sidecar boot record. */
  env?: Record<string, string>;
  fdPoll?: FdPoll;
  /**
   * Milliseconds to add to the REALTIME clock, sampled on every read. The
   * MONOTONIC clock is deliberately NOT offset -- that asymmetry is the whole
   * point.
   *
   * ⚠️ A TEST INSTRUMENT. Guest deadlines must not move when the host's wall
   * clock steps, and there is no way to step a real one from a test, so the
   * shim provides a fake step. Unset, the clock is the host's, unmodified.
   */
  realtimeOffsetMs?: () => number;
}

export function preview1(opts: Preview1Options): Record<string, Function> {
  const files = opts.files;
  const fdPoll = opts.fdPoll ?? NO_FDS;
  const mem = () => new Mem(opts.memory());

  const encode = (pairs: string[]): Uint8Array[] =>
    pairs.map((s) => new TextEncoder().encode(s + '\0'));

  const argv = encode(opts.args ?? ['app']);
  const environ = encode(Object.entries(opts.env ?? {}).map(([k, v]) => `${k}=${v}`));

  /** `*_sizes_get`: count and total byte size of a NUL-terminated string vector. */
  const sizes = (vec: Uint8Array[], countPtr: number, sizePtr: number): number => {
    const m = mem();
    m.setU32(countPtr, vec.length);
    m.setU32(
      sizePtr,
      vec.reduce((n, b) => n + b.length, 0),
    );
    return E.SUCCESS;
  };

  /** `*_get`: a pointer array followed by the packed NUL-terminated strings. */
  const getVec = (vec: Uint8Array[], ptrsPtr: number, bufPtr: number): number => {
    const m = mem();
    let off = bufPtr;
    for (let i = 0; i < vec.length; i++) {
      m.setU32(ptrsPtr + i * 4, off);
      m.write(off, vec[i]!);
      off += vec[i]!.length;
    }
    return E.SUCCESS;
  };

  return {
    args_sizes_get: (countPtr: number, sizePtr: number) => sizes(argv, countPtr, sizePtr),
    args_get: (ptrsPtr: number, bufPtr: number) => getVec(argv, ptrsPtr, bufPtr),
    environ_sizes_get: (countPtr: number, sizePtr: number) => sizes(environ, countPtr, sizePtr),
    environ_get: (ptrsPtr: number, bufPtr: number) => getVec(environ, ptrsPtr, bufPtr),

    // `precision` arrives as a BigInt: it is an i64 in the wasm signature, and
    // JS surfaces every i64 parameter that way. Ignored -- we cannot honour a
    // precision request the host clock does not offer.
    clock_time_get: (id: number, _precision: bigint, outPtr: number) => {
      // ⚠️ THE TWO CLOCKS ARE NOT THE SAME CLOCK, and the runtime now depends on
      // that. MONOTONIC backs `context::mono_nanos`, which every guest deadline
      // is measured against; REALTIME backs `context::now_nanos`, which the
      // guest sees through `clock_gettime(CLOCK_REALTIME)` and `gettimeofday`.
      // The runtime used to serve both from REALTIME, so a wall-clock step moved
      // guest timers with it.
      //
      // `performance.now()` is monotonic in both Node and browsers and survives
      // a system clock change; it is coarsened against Spectre, which costs
      // resolution and not monotonicity.
      if (id === CLOCK.MONOTONIC) {
        mem().setU64(outPtr, BigInt(Math.round(performance.now() * 1e6)));
        return E.SUCCESS;
      }
      const stepMs = opts.realtimeOffsetMs?.() ?? 0;
      mem().setU64(outPtr, BigInt(Date.now() + Math.round(stepMs)) * 1_000_000n);
      return E.SUCCESS;
    },

    random_get: (buf: number, len: number) => {
      const m = mem();
      // ⚠️ `crypto.getRandomValues` THROWS above 65536 bytes. Chunking is not
      // optional; a single large draw is a hard failure, not a short read.
      const CHUNK = 65536;
      for (let off = 0; off < len; off += CHUNK) {
        const n = Math.min(CHUNK, len - off);
        const tmp = new Uint8Array(n);
        crypto.getRandomValues(tmp);
        m.write(buf + off, tmp);
      }
      return E.SUCCESS;
    },

    proc_exit: (code: number) => {
      throw new ProcExit(code);
    },

    fd_close: (fd: number) => files.close(fd),

    // ecvisor calls this on every socket it opens (`sys.rs:5419`) and every one
    // it accepts (`sys.rs:5763`), to force the host handle non-blocking. The
    // socket layer already opens handles non-blocking, so there is nothing to
    // do -- but it must SUCCEED, because ecvisor treats a failure as fatal.
    fd_fdstat_set_flags: (_fd: number, _flags: number) => E.SUCCESS,

    fd_filestat_get: (fd: number, buf: number) => {
      const st = files.stat(fd);
      if (st.errno !== E.SUCCESS) return st.errno;
      const m = mem();
      // Zero the whole struct first: `std::fs::read` sizes its buffer from
      // `size`, and a stale nlink or timestamp is harmless, but leaving the
      // padding undefined is the kind of thing that differs between hosts.
      m.write(buf, new Uint8Array(FILESTAT.SIZE));
      m.setU8(buf + FILESTAT.FILETYPE, st.filetype);
      m.setU64(buf + FILESTAT.NLINK, 1);
      m.setU64(buf + FILESTAT.FSIZE, st.size);
      return E.SUCCESS;
    },

    fd_prestat_get: (fd: number, buf: number) => {
      // The preopen list is enumerated by walking fds upward until one returns
      // BADF. Returning anything else for fd 4 makes the walk continue.
      if (!files.isPreopen(fd)) return E.BADF;
      const m = mem();
      m.setU8(buf + PRESTAT.TAG, PREOPENTYPE_DIR);
      m.setU32(buf + PRESTAT.NAME_LEN, new TextEncoder().encode(files.preopenName).length);
      return E.SUCCESS;
    },

    fd_prestat_dir_name: (fd: number, path: number, pathLen: number) => {
      if (!files.isPreopen(fd)) return E.BADF;
      const name = new TextEncoder().encode(files.preopenName);
      if (pathLen < name.length) return E.INVAL;
      mem().write(path, name);
      return E.SUCCESS;
    },

    fd_read: (fd: number, iovs: number, iovsLen: number, nreadPtr: number) => {
      const m = mem();
      const want = iovecTotal(m, iovs, iovsLen);
      const r = files.read(fd, want);
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(nreadPtr, scatter(m, iovs, iovsLen, r.data));
      return E.SUCCESS;
    },

    fd_write: (fd: number, iovs: number, iovsLen: number, nwrittenPtr: number) => {
      const m = mem();
      const w = files.write(fd, gather(m, iovs, iovsLen));
      if (w.errno !== E.SUCCESS) return w.errno;
      m.setU32(nwrittenPtr, w.written);
      return E.SUCCESS;
    },

    // Two BigInts in the middle: fs_rights_base and fs_rights_inheriting are
    // i64s. Getting the arity wrong here shifts `fdflags` and `fd_out`, and the
    // symptom is a write to a nonsense address rather than an error.
    path_open: (
      dirfd: number,
      _dirflags: number,
      path: number,
      pathLen: number,
      _oflags: number,
      _rightsBase: bigint,
      _rightsInheriting: bigint,
      _fdflags: number,
      fdOut: number,
    ) => {
      const m = mem();
      const r = files.pathOpen(dirfd, m.str(path, pathLen));
      if (r.errno !== E.SUCCESS) return r.errno;
      m.setU32(fdOut, r.fd);
      return E.SUCCESS;
    },

    /**
     * ecvisor drives this from exactly two places, and they want different
     * things: `host_poll_sockets` (`sys.rs:591`) BLOCKS until a socket is ready
     * or a deadline expires, and `socket_poll_ready` (`sys.rs:651`) passes a
     * zero relative timeout as a pure readiness probe.
     *
     * Both are served here by the same loop. The probe falls out naturally: a
     * zero timeout means the deadline has already passed, so it returns after
     * one non-blocking check.
     */
    poll_oneoff: (inPtr: number, outPtr: number, nsubs: number, neventsPtr: number) => {
      const m = mem();
      if (nsubs === 0) return E.INVAL;

      const fds: { fd: number; write: boolean; userdata: bigint }[] = [];
      // `deadlineMs` is meaningless without the clock it was measured against --
      // MONOTONIC is a small `performance.now()` value and REALTIME is a
      // ~1.7e12 unix millisecond -- so the id is carried with it. Inferring the
      // base from the magnitude instead works until it doesn't, and the failure
      // is a wait of several decades.
      let clock: { userdata: bigint; deadlineMs: number; id: number } | null = null;
      const nowOf = (id: number) => (id === CLOCK.MONOTONIC ? performance.now() : Date.now());

      for (let i = 0; i < nsubs; i++) {
        const s = inPtr + i * SUB.SIZE;
        const userdata = m.view().getBigUint64(s + SUB.USERDATA, true);
        const type = m.u8(s + SUB.EVENTTYPE);
        if (type === EVENTTYPE.CLOCK) {
          const id = m.u32(s + SUB.CLOCK_ID);
          const timeoutNs = m.u64(s + SUB.TIMEOUT);
          const abs = (m.u16(s + SUB.FLAGS) & SUBSCRIPTION_CLOCK_ABSTIME) !== 0;
          const deadlineMs = abs ? timeoutNs / 1e6 : nowOf(id) + timeoutNs / 1e6;
          // Keep only the soonest to fire. ecvisor never subscribes more than
          // one clock, so this compares remaining time rather than raw
          // deadlines -- two different clock bases are not comparable.
          const remaining = deadlineMs - nowOf(id);
          if (!clock || remaining < clock.deadlineMs - nowOf(clock.id)) {
            clock = { userdata, deadlineMs, id };
          }
        } else {
          fds.push({ fd: m.u32(s + SUB.FD), write: type === EVENTTYPE.FD_WRITE, userdata });
        }
      }

      const emit = (events: { userdata: bigint; type: number }[]): number => {
        const mm = mem();
        events.forEach((e, i) => {
          const o = outPtr + i * EVENT.SIZE;
          mm.write(o, new Uint8Array(EVENT.SIZE));
          mm.setU64(o + EVENT.USERDATA, e.userdata);
          mm.setU16(o + EVENT.ERRNO, E.SUCCESS);
          mm.setU8(o + EVENT.EVENTTYPE, e.type);
        });
        mm.setU32(neventsPtr, events.length);
        return E.SUCCESS;
      };

      // ⚠️ THE TWO WAKE SOURCES MUST BE SERVICED BY ONE WAIT. A socket becoming
      // ready and a deadline expiring can each release the guest, and waiting
      // on either alone starves the other: an unbounded socket wait sleeps
      // straight through every guest timer, and sleeping for the deadline first
      // sleeps through an arriving connection for as long as the longest timer
      // -- and nginx routinely asks for 60 s. The Rust side documents the same
      // interlock at `sys.rs:583-590`, which is where it was learned. So the
      // deadline is handed to the backend AS its timeout rather than raced
      // against it here.
      // ⚠️ THE FDS ARE CHECKED FIRST, EVEN WHEN THE DEADLINE HAS ALREADY PASSED.
      // `socket_poll_ready` (`sys.rs:651`) is a readiness PROBE, and it asks for
      // it as {the fd, a zero relative clock} -- so the deadline is expired on
      // arrival, every time. Reporting the expired clock first would answer
      // "not ready" for every socket that is in fact ready, which inverts the
      // probe and is precisely the PostgreSQL `ServerLoop` deadlock the Rust
      // side added that function to fix.
      for (;;) {
        const remain = clock ? clock.deadlineMs - nowOf(clock.id) : -1;
        if (fds.length > 0) {
          const hit = fdPoll.wait(fds, clock ? Math.max(0, Math.ceil(remain)) : -1);
          if (hit.length > 0) {
            return emit(
              hit.map((i) => ({
                userdata: fds[i]!.userdata,
                type: fds[i]!.write ? EVENTTYPE.FD_WRITE : EVENTTYPE.FD_READ,
              })),
            );
          }
          // The wait came back with nothing: the deadline is what expired.
          if (clock) return emit([{ userdata: clock.userdata, type: EVENTTYPE.CLOCK }]);
          continue; // infinite wait, woken with nothing ready
        }
        if (!clock) return E.INVAL;
        if (remain <= 0) return emit([{ userdata: clock.userdata, type: EVENTTYPE.CLOCK }]);
        sleepSync(remain);
      }
    },
  };
}
