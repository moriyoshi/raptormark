import assert from 'node:assert/strict';
import { test } from 'vitest';

import { ResponseFramer, decodeChunked } from './framing.ts';

const enc = (s: string) => new TextEncoder().encode(s);
const dec = (b: Uint8Array) => new TextDecoder().decode(b);

/** Feeds a response one byte at a time and reports where it declared itself done. */
function byteAtATime(method: string, wire: string): { at: number; framing: string } {
  const f = new ResponseFramer(method);
  const bytes = enc(wire);
  for (let i = 0; i < bytes.length; i++) {
    f.push(bytes.subarray(i, i + 1));
    if (f.complete) return { at: i + 1, framing: f.framing };
  }
  return { at: -1, framing: f.framing };
}

test('Content-Length ends the response without any close', () => {
  const wire = 'HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello';
  const r = byteAtATime('GET', wire);
  // ⚠️ THE WHOLE POINT. A keep-alive server writes exactly this and then waits
  // for the next request. If the host needed a close, it would wait too -- and
  // time out against a server that did nothing wrong.
  assert.equal(r.framing, 'length');
  assert.equal(r.at, wire.length, 'must complete on the last body byte');
});

test('a body shorter than Content-Length is not complete', () => {
  const f = new ResponseFramer('GET');
  f.push(enc('HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nshort'));
  assert.equal(f.complete, false);
  // A close is still a truthful end: truncated is all there will ever be.
  f.atClose();
  assert.equal(f.complete, true);
});

test('no Content-Length and no chunking falls back to close', () => {
  const f = new ResponseFramer('GET');
  f.push(enc('HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nbody'));
  assert.equal(f.framing, 'close');
  assert.equal(f.complete, false, 'HTTP/1.0 framing genuinely needs the close');
  f.atClose();
  assert.equal(f.complete, true);
});

test('HEAD has no body however large the Content-Length claims to be', () => {
  // ⚠️ A HEAD response advertises the length the GET would have had. Framing by
  // it waits forever for bytes the server was never going to send.
  const wire = 'HTTP/1.1 200 OK\r\nContent-Length: 4096\r\n\r\n';
  const r = byteAtATime('HEAD', wire);
  assert.equal(r.framing, 'none');
  assert.equal(r.at, wire.length);
});

test('204 and 304 have no body whatever their headers say', () => {
  for (const status of ['204 No Content', '304 Not Modified']) {
    const wire = `HTTP/1.1 ${status}\r\nContent-Length: 99\r\n\r\n`;
    const r = byteAtATime('GET', wire);
    assert.equal(r.framing, 'none', status);
    assert.equal(r.at, wire.length, status);
  }
});

test('a chunked response ends at the terminal chunk', () => {
  const wire =
    'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n' + '5\r\nhello\r\n' + '0\r\n\r\n';
  const r = byteAtATime('GET', wire);
  assert.equal(r.framing, 'chunked');
  assert.equal(r.at, wire.length);
});

test('chunk DATA containing the terminator is not mistaken for the end', () => {
  // ⚠️ THIS IS WHY THE CHUNKS ARE WALKED. Scanning for "0\r\n\r\n" is one line
  // and wrong: those bytes are ordinary data here, and a scan would cut the
  // body short at them while reporting success.
  const payload = '0\r\n\r\nXY';
  const wire =
    'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n' +
    `${payload.length.toString(16)}\r\n${payload}\r\n` +
    '0\r\n\r\n';
  const r = byteAtATime('GET', wire);
  assert.equal(r.at, wire.length, 'completed early, inside the chunk data');

  const f = new ResponseFramer('GET');
  f.push(enc(wire));
  const body = f.bytes().slice(dec(f.bytes()).indexOf('\r\n\r\n') + 4);
  assert.equal(dec(decodeChunked(body)), payload);
});

test('chunk extensions and trailers are tolerated', () => {
  const wire =
    'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n' +
    '5;name=value\r\nhello\r\n' +
    '0\r\nX-Trailer: 1\r\n\r\n';
  const r = byteAtATime('GET', wire);
  assert.equal(r.at, wire.length);
});

test('Transfer-Encoding wins over a conflicting Content-Length', () => {
  // ⚠️ RFC 9112 says chunked wins, and this combination is also the shape of a
  // request-smuggling attempt -- resolving it the other way is the vulnerable
  // reading, not merely a different one.
  const f = new ResponseFramer('GET');
  f.push(enc('HTTP/1.1 200 OK\r\nContent-Length: 2\r\nTransfer-Encoding: chunked\r\n\r\n'));
  assert.equal(f.framing, 'chunked');
});

test('a head split across writes is still parsed', () => {
  const f = new ResponseFramer('GET');
  f.push(enc('HTTP/1.1 200 OK\r\nContent-'));
  assert.equal(f.framing, 'head');
  f.push(enc('Length: 2\r\n\r\n'));
  assert.equal(f.framing, 'length');
  assert.equal(f.complete, false);
  f.push(enc('hi'));
  assert.equal(f.complete, true);
});

test('decodeChunked joins chunks and stops at the terminator', () => {
  assert.equal(dec(decodeChunked(enc('3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n'))), 'abcde');
  assert.equal(dec(decodeChunked(enc('0\r\n\r\n'))), '');
});
