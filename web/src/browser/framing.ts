/**
 * Knowing when a response has ENDED.
 *
 * # ⚠️ WAITING FOR THE GUEST TO CLOSE IS NOT GOOD ENOUGH
 *
 * The inbound host first decided a response was finished when the guest shut
 * down its write side. That works for a server told `Connection: close`, and it
 * fails for every server that keeps the connection open -- which is the DEFAULT
 * for HTTP/1.1, nginx included. Such a server answers correctly and then waits
 * for the next request, so the host sits there until its request deadline and
 * reports a timeout against a server that did nothing wrong.
 *
 * So the response is framed the way HTTP says to frame it: `Content-Length`,
 * or `Transfer-Encoding: chunked`, or -- only when neither is present -- until
 * the connection closes. That last case is the HTTP/1.0 rule, and it is the
 * fallback rather than the mechanism.
 *
 * ⚠️ THE REQUEST METHOD AND THE STATUS CODE CAN BOTH ABOLISH THE BODY. A `HEAD`
 * response carries `Content-Length` describing a body that is not there, and
 * 204/304 have no body whatever their headers say. Reading a body in those
 * cases waits forever for bytes that were never going to arrive.
 */

/** A growing byte buffer. Doubling, so accumulating a large body stays linear. */
class Buf {
  private b = new Uint8Array(new ArrayBuffer(1024));
  private n = 0;

  push(chunk: Uint8Array): void {
    if (this.n + chunk.length > this.b.length) {
      let cap = this.b.length * 2;
      while (cap < this.n + chunk.length) cap *= 2;
      const grown = new Uint8Array(new ArrayBuffer(cap));
      grown.set(this.b.subarray(0, this.n));
      this.b = grown;
    }
    this.b.set(chunk, this.n);
    this.n += chunk.length;
  }

  get length(): number {
    return this.n;
  }

  view(): Uint8Array {
    return this.b.subarray(0, this.n);
  }

  copy(): Uint8Array<ArrayBuffer> {
    return this.b.slice(0, this.n);
  }
}

export type Framing = 'head' | 'none' | 'length' | 'chunked' | 'close';

/**
 * Accumulates a response and reports when it is complete.
 *
 * `complete` going true means the response can be delivered without waiting for
 * the guest to do anything else.
 */
export class ResponseFramer {
  private readonly buf = new Buf();
  private readonly method: string;
  private mode: Framing = 'head';
  private headEnd = -1;
  private need = 0;
  private done = false;

  constructor(requestMethod: string) {
    this.method = requestMethod.toUpperCase();
  }

  push(chunk: Uint8Array): void {
    if (chunk.length > 0) this.buf.push(chunk);
    this.evaluate();
  }

  /** True once the response is framed and whole. */
  get complete(): boolean {
    return this.done;
  }

  /** How the body length was determined, for diagnostics. */
  get framing(): Framing {
    return this.mode;
  }

  /** Everything received so far, head included. */
  bytes(): Uint8Array<ArrayBuffer> {
    return this.buf.copy();
  }

  /**
   * Declares the connection closed.
   *
   * The only thing that can end a `close`-framed response, and a truthful end
   * for the others too -- a truncated response is still all there will ever be.
   */
  atClose(): void {
    this.done = true;
  }

  private evaluate(): void {
    if (this.done) return;
    const b = this.buf.view();

    if (this.mode === 'head') {
      const end = indexOfCRLFCRLF(b);
      if (end < 0) return;
      this.headEnd = end + 4;
      this.decide(new TextDecoder().decode(b.subarray(0, end)));
    }

    switch (this.mode) {
      case 'none':
        this.done = true;
        return;
      case 'length':
        if (b.length - this.headEnd >= this.need) this.done = true;
        return;
      case 'chunked':
        this.done = chunkedComplete(b, this.headEnd);
        return;
      default:
        // 'close': only `atClose` ends it.
        return;
    }
  }

  private decide(head: string): void {
    const [statusLine, ...rest] = head.split('\r\n');
    const status = Number(/^HTTP\/1\.[01] (\d{3})/.exec(statusLine ?? '')?.[1] ?? 0);

    let length: number | undefined;
    let chunked = false;
    for (const line of rest) {
      const i = line.indexOf(':');
      if (i < 0) continue;
      const name = line.slice(0, i).trim().toLowerCase();
      const value = line.slice(i + 1).trim();
      if (name === 'content-length') {
        const n = Number(value);
        if (Number.isInteger(n) && n >= 0) length = n;
      } else if (name === 'transfer-encoding') {
        chunked = value.toLowerCase().includes('chunked');
      }
    }

    // ⚠️ ORDER MATTERS AND IS NOT ARBITRARY. A body-less response is body-less
    // no matter what its headers claim, and `Transfer-Encoding` beats
    // `Content-Length` when a server sends both (RFC 9112) -- which is also the
    // shape of a request-smuggling attempt, so it must not be resolved the
    // other way.
    if (
      this.method === 'HEAD' ||
      status === 204 ||
      status === 304 ||
      (status >= 100 && status < 200)
    ) {
      this.mode = 'none';
    } else if (chunked) {
      this.mode = 'chunked';
    } else if (length !== undefined) {
      this.mode = 'length';
      this.need = length;
    } else {
      this.mode = 'close';
    }
  }
}

/**
 * Walks the chunked body to find the terminal chunk.
 *
 * ⚠️ SCANNING FOR `0\r\n\r\n` IS WRONG, and tempting because it is one line.
 * Those bytes occur inside ordinary chunk DATA -- any body containing that
 * sequence would be cut short at it. The chunks have to be walked.
 */
function chunkedComplete(b: Uint8Array, from: number): boolean {
  let pos = from;
  for (;;) {
    const eol = indexOfCRLF(b, pos);
    if (eol < 0) return false;
    const line = new TextDecoder().decode(b.subarray(pos, eol));
    // A chunk extension (`;name=value`) may follow the size.
    const size = parseInt(line.split(';')[0]!.trim(), 16);
    if (!Number.isInteger(size) || size < 0) {
      // Malformed. Not complete: let the close path end it rather than
      // delivering a body this cannot account for.
      return false;
    }
    pos = eol + 2;
    if (size === 0) {
      // Trailers, then a blank line. With none, the blank line is immediate.
      for (;;) {
        const t = indexOfCRLF(b, pos);
        if (t < 0) return false;
        if (t === pos) return true;
        pos = t + 2;
      }
    }
    pos += size + 2;
    if (pos > b.length) return false;
  }
}

/** Decodes a chunked body into the bytes it represents. */
export function decodeChunked(b: Uint8Array): Uint8Array<ArrayBuffer> {
  const out: Uint8Array[] = [];
  let pos = 0;
  for (;;) {
    const eol = indexOfCRLF(b, pos);
    if (eol < 0) break;
    const size = parseInt(new TextDecoder().decode(b.subarray(pos, eol)).split(';')[0]!.trim(), 16);
    if (!Number.isInteger(size) || size <= 0) break;
    pos = eol + 2;
    out.push(b.slice(pos, Math.min(pos + size, b.length)));
    pos += size + 2;
  }
  let total = 0;
  for (const c of out) total += c.length;
  const joined = new Uint8Array(new ArrayBuffer(total));
  let at = 0;
  for (const c of out) {
    joined.set(c, at);
    at += c.length;
  }
  return joined;
}

export function indexOfCRLFCRLF(b: Uint8Array): number {
  for (let i = 0; i + 3 < b.length; i++) {
    if (b[i] === 13 && b[i + 1] === 10 && b[i + 2] === 13 && b[i + 3] === 10) return i;
  }
  return -1;
}

function indexOfCRLF(b: Uint8Array, from: number): number {
  for (let i = from; i + 1 < b.length; i++) {
    if (b[i] === 13 && b[i + 1] === 10) return i;
  }
  return -1;
}
