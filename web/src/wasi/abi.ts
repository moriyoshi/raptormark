// WASI preview1 constants and struct layouts, plus the WasmEdge socket
// extension's own types.
//
// ⚠️ THE SPECIFICATION FOR EVERYTHING IN THIS FILE IS `runtime/src/sys.rs:484-576`.
// Not the WASI repo, and NOT `third_party/wazero/imports/wasi_snapshot_preview1/
// sock_wasmedge.go`, which `sys.rs:520` and `sys.rs:568` both cite as the
// reference host: that path does not exist in this tree and never has
// (`.gitmodules` has one entry, elfconv). Anyone sent to read the vendored
// wazero will find nothing, so read the Rust externs instead -- they are what
// the module actually calls.

/** WASI `errno`. Only the values ecvisor distinguishes are named. */
export const E = {
  SUCCESS: 0,
  ACCES: 2,
  AGAIN: 6,
  ALREADY: 7,
  BADF: 8,
  CONNREFUSED: 14,
  EXIST: 20,
  FAULT: 21,
  INPROGRESS: 26,
  INVAL: 28,
  IO: 29,
  ISDIR: 31,
  MFILE: 33,
  NOENT: 44,
  NOSYS: 52,
  NOTCONN: 53,
  NOTDIR: 54,
  NOTSUP: 58,
  PERM: 63,
  PIPE: 64,
} as const;

/** WASI `filetype`. */
export const FILETYPE = {
  UNKNOWN: 0,
  CHARACTER_DEVICE: 2,
  DIRECTORY: 3,
  REGULAR_FILE: 4,
  SOCKET_STREAM: 6,
} as const;

/** `preopentype`. Only `dir` exists. */
export const PREOPENTYPE_DIR = 0;

/** `clockid`. */
export const CLOCK = { REALTIME: 0, MONOTONIC: 1 } as const;

/** `eventtype` for a `poll_oneoff` subscription. */
export const EVENTTYPE = { CLOCK: 0, FD_READ: 1, FD_WRITE: 2 } as const;

/** `subclockflags`: set means `timeout` is an absolute time. */
export const SUBSCRIPTION_CLOCK_ABSTIME = 1;

/** `fdflags`. ecvisor sets NONBLOCK on every socket it opens. */
export const FDFLAGS_NONBLOCK = 4;

// --- Struct sizes and field offsets -----------------------------------------
//
// Named rather than inlined because a wrong offset here reads as a corrupt guest
// rather than as a shim bug, which is an expensive way to find a typo.

/** `filestat` is 64 bytes. */
export const FILESTAT = {
  SIZE: 64,
  DEV: 0,
  INO: 8,
  FILETYPE: 16,
  NLINK: 24,
  FSIZE: 32,
  ATIM: 40,
  MTIM: 48,
  CTIM: 56,
} as const;

/** `prestat` is 8 bytes: a u8 tag, padding, then a u32 name length. */
export const PRESTAT = { SIZE: 8, TAG: 0, NAME_LEN: 4 } as const;

/**
 * `subscription` is 48 bytes: userdata u64 @0, eventtype u8 @8, then a union at
 * @16 -- `fd` u32 @16 for a read/write, or clock_id u32 @16, timeout u64 @24,
 * precision u64 @32, flags u16 @40.
 *
 * This layout is mirrored on the Rust side in `host_poll_sockets`
 * (`sys.rs:591`) and `socket_poll_ready` (`sys.rs:651`), which build the bytes
 * by hand. If one moves, both move.
 */
export const SUB = {
  SIZE: 48,
  USERDATA: 0,
  EVENTTYPE: 8,
  FD: 16,
  CLOCK_ID: 16,
  TIMEOUT: 24,
  PRECISION: 32,
  FLAGS: 40,
} as const;

/** `event` is 32 bytes: userdata u64 @0, errno u16 @8, eventtype u8 @10. */
export const EVENT = { SIZE: 32, USERDATA: 0, ERRNO: 8, EVENTTYPE: 10 } as const;

/**
 * A `ciovec`/`iovec` on wasm32: `{ buf: u32, len: u32 }`.
 *
 * ecvisor passes pointers into its own Rust stack and heap across this
 * boundary, relying on the fact that on wasm32 a pointer's integer value IS its
 * linear-memory offset (`sys.rs:508-515`). So there is no translation to do
 * here -- the u32 is already the offset to read or write.
 */
export const IOVEC = { SIZE: 8, BUF: 0, LEN: 4 } as const;

/**
 * WasmEdge's `WasiAddress` (raw-address V1 form): `{ buf: u32, size: u32 }` on
 * wasm32, where `buf` points at network-order address octets and `size` is 4
 * for IPv4 or 16 for IPv6 (`sys.rs:490-499`).
 */
export const WASI_ADDRESS = { SIZE: 8, BUF: 0, LEN: 4 } as const;

/** WasmEdge address families and socket types (`sys.rs:421-424`). */
export const WE_FAMILY = { INET4: 1, INET6: 2 } as const;
export const WE_SOCKTYPE = { DGRAM: 1, STREAM: 2 } as const;
