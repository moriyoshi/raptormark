/**
 * The main-thread <-> socket-worker protocol.
 *
 * ⚠️ WHY THERE IS A WORKER AT ALL. The WasmEdge socket ABI is synchronous:
 * `sock_connect` returns an errno, `fd_read` returns bytes or EAGAIN, and
 * `poll_oneoff` blocks until something is ready. Node's `net` is the opposite --
 * data arrives in `'data'` events, which only fire when the event loop runs.
 *
 * But the event loop CANNOT run while the guest is executing: `_start()` is one
 * synchronous call that does not return until the guest exits. So a single-
 * threaded implementation deadlocks on the first read -- the socket has data,
 * the event that would deliver it is queued behind a wasm call that is waiting
 * for that data, and neither ever moves.
 *
 * So the real sockets live in a worker with its own event loop, and the main
 * thread talks to it over a SharedArrayBuffer with `Atomics.wait`. That is what
 * turns an asynchronous API back into the synchronous one the ABI needs.
 *
 * The worker waits with `Atomics.waitAsync`, NOT `Atomics.wait`: a blocking
 * wait there would stall the worker's own event loop and reintroduce exactly
 * the deadlock this design exists to avoid. It is the single most important
 * line in the worker.
 *
 * This is also why the design does not carry over to a browser main thread,
 * where `Atomics.wait` is illegal. The browser profile solves the same problem
 * the other way, by making the guest re-entrant so the host never blocks.
 */

export const OP = {
  OPEN: 1,
  BIND: 2,
  LISTEN: 3,
  CONNECT: 4,
  ACCEPT: 5,
  RECV: 6,
  SEND: 7,
  ADDR: 8,
  GETSOCKOPT: 9,
  SETSOCKOPT: 10,
  SHUTDOWN: 11,
  CLOSE: 12,
  POLL: 13,
  SENDTO: 14,
  RECVFROM: 15,
  SHUTDOWN_WORKER: 16,
} as const;

/** Control-word indices into the Int32Array header. */
export const C = {
  REQ_SEQ: 0,
  RES_SEQ: 1,
  OP: 2,
  A0: 3,
  A1: 4,
  A2: 5,
  A3: 6,
  A4: 7,
  ERRNO: 8,
  V0: 9,
  V1: 10,
  REQ_LEN: 11,
  RES_LEN: 12,
} as const;

export const CTRL_INTS = 16;
export const CTRL_BYTES = CTRL_INTS * 4;

/**
 * Payload capacity in each direction.
 *
 * 1 MiB bounds a single `recv`/`send`; ecvisor asks for whatever the guest
 * asked for, and a larger request is served short, which is a legal read. It
 * also bounds a `POLL` waiter list at 131072 entries, far past anything real.
 */
export const PAYLOAD_BYTES = 1 << 20;

export const REQ_OFF = CTRL_BYTES;
export const RES_OFF = CTRL_BYTES + PAYLOAD_BYTES;
export const SAB_BYTES = CTRL_BYTES + PAYLOAD_BYTES * 2;

/** Readiness bits used by the POLL op's reply. */
export const READY = { READ: 1, WRITE: 2 } as const;

/**
 * WasmEdge address-family tags, as they cross `sock_open` and the 128-byte
 * `sendto`/`recvfrom` address form (`runtime/src/sys.rs:421-424`, `:5978-5981`).
 */
export const FAMILY = { INET4: 1, INET6: 2 } as const;
export const SOCKTYPE = { DGRAM: 1, STREAM: 2 } as const;

/**
 * Node error codes -> WASI errno.
 *
 * ⚠️ Do NOT collapse these into one value. `sys_connect`
 * (`runtime/src/sys.rs:5574-5599`) treats EAGAIN/INPROGRESS/ALREADY as the
 * resumable "still connecting" family and everything else as a hard
 * ECONNREFUSED, and `wasi_send_errno` (`:5282`) maps PIPE to EPIPE precisely
 * because reporting EIO for an ordinary client disconnect made nginx log
 * `sendfile() failed (5: I/O error)` for a normal event.
 */
export const NODE_ERRNO: Record<string, number> = {
  EACCES: 2,
  EADDRINUSE: 3,
  EADDRNOTAVAIL: 4,
  EAGAIN: 6,
  EALREADY: 7,
  EBADF: 8,
  ECONNABORTED: 13,
  ECONNREFUSED: 14,
  ECONNRESET: 15,
  EHOSTUNREACH: 23,
  EINPROGRESS: 26,
  EINVAL: 28,
  EIO: 29,
  EISCONN: 30,
  ENETUNREACH: 40,
  ENOTCONN: 53,
  ENOTSUP: 58,
  EPIPE: 64,
  ETIMEDOUT: 73,
};

export const E_SUCCESS = 0;
export const E_AGAIN = 6;
export const E_BADF = 8;
export const E_INPROGRESS = 26;
export const E_INVAL = 28;
export const E_IO = 29;
export const E_NOTSUP = 58;

export function errnoOf(err: unknown): number {
  const code = (err as { code?: string } | undefined)?.code;
  return (code && NODE_ERRNO[code]) || E_IO;
}
