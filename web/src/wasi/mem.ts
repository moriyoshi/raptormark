/**
 * Accessors over the instance's linear memory.
 *
 * ⚠️ THE ONE INVARIANT: never cache a typed-array view or an ArrayBuffer across
 * a call. Every accessor here re-reads `this.memory.buffer` on entry, and that
 * is not defensive style -- it is the whole reason this shim exists instead of
 * `node:wasi`.
 *
 * `memory.grow` DETACHES the old ArrayBuffer. Any view built before the growth
 * silently becomes zero-length, so a cached view does not throw where you can
 * see it: reads return nothing and writes go nowhere. node's own WASI binding
 * caches the backing store when the memory is bound and never re-reads it, and
 * the result is that the first WASI call after any growth ABORTS THE PROCESS --
 * SIGABRT, no message, no stack. `e2e/testdata/embedder.mjs:127-141` documents
 * this from experience and works around it by fishing a private `kSetMemory`
 * symbol out of the instance.
 *
 * Every raptormark guest triggers it, because ecvisor allocates a 384 MiB arena
 * during startup (`arena.rs`, `MEMORY_ARENA_SIZE`). So this is not an edge case
 * to guard against; it is the normal path, on the first interesting call.
 *
 * The same hazard exists in the browser, where it is quieter still -- no abort,
 * just a detached buffer and wrong answers. `mem.test.ts` grows the memory
 * between two calls and asserts the second one still reads correctly.
 */
export class Mem {
  readonly memory: WebAssembly.Memory;

  constructor(memory: WebAssembly.Memory) {
    this.memory = memory;
  }

  /** A fresh DataView over the current buffer. Never stored. */
  view(): DataView {
    return new DataView(this.memory.buffer);
  }

  /** A fresh Uint8Array over the current buffer. Never stored. */
  bytes(): Uint8Array {
    return new Uint8Array(this.memory.buffer);
  }

  u8(ptr: number): number {
    return this.view().getUint8(ptr);
  }

  u16(ptr: number): number {
    return this.view().getUint16(ptr, true);
  }

  u32(ptr: number): number {
    return this.view().getUint32(ptr, true);
  }

  /** Reads a u64 as a JS number. Safe here: every u64 ecvisor passes is a size,
   *  an offset or a nanosecond count, all far below 2^53. */
  u64(ptr: number): number {
    return Number(this.view().getBigUint64(ptr, true));
  }

  setU8(ptr: number, v: number): void {
    this.view().setUint8(ptr, v);
  }

  setU16(ptr: number, v: number): void {
    this.view().setUint16(ptr, v, true);
  }

  setU32(ptr: number, v: number): void {
    this.view().setUint32(ptr, v, true);
  }

  setU64(ptr: number, v: number | bigint): void {
    this.view().setBigUint64(ptr, BigInt(v), true);
  }

  /** A COPY of `[ptr, ptr+len)`. Copies rather than subarrays on purpose: a
   *  subarray is a view, and handing one to a caller that outlives the call
   *  re-introduces exactly the detachment bug this class exists to prevent. */
  read(ptr: number, len: number): Uint8Array {
    return this.bytes().slice(ptr, ptr + len);
  }

  write(ptr: number, src: Uint8Array): void {
    this.bytes().set(src, ptr);
  }

  /** Reads a length-delimited (not NUL-terminated) UTF-8 string. WASI paths
   *  carry an explicit length and are not NUL-terminated. */
  str(ptr: number, len: number): string {
    return new TextDecoder().decode(this.read(ptr, len));
  }
}

/**
 * Total byte length of an iovec/ciovec array, without copying any of it.
 * Used to size a read before deciding how much a backend can supply.
 */
export function iovecTotal(mem: Mem, iovs: number, iovsLen: number): number {
  const v = mem.view();
  let total = 0;
  for (let i = 0; i < iovsLen; i++) {
    total += v.getUint32(iovs + i * 8 + 4, true);
  }
  return total;
}

/** Gathers an iovec array into one contiguous buffer (the write direction). */
export function gather(mem: Mem, iovs: number, iovsLen: number): Uint8Array {
  const v = mem.view();
  const parts: Uint8Array[] = [];
  let total = 0;
  for (let i = 0; i < iovsLen; i++) {
    const buf = v.getUint32(iovs + i * 8, true);
    const len = v.getUint32(iovs + i * 8 + 4, true);
    parts.push(mem.read(buf, len));
    total += len;
  }
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/**
 * Scatters `src` across an iovec array (the read direction), returning how many
 * bytes were placed. Stops when `src` runs out, which is the short-read case
 * every caller has to handle.
 */
export function scatter(mem: Mem, iovs: number, iovsLen: number, src: Uint8Array): number {
  const v = mem.view();
  let off = 0;
  for (let i = 0; i < iovsLen && off < src.length; i++) {
    const buf = v.getUint32(iovs + i * 8, true);
    const len = v.getUint32(iovs + i * 8 + 4, true);
    const n = Math.min(len, src.length - off);
    mem.write(buf, src.subarray(off, off + n));
    off += n;
  }
  return off;
}
