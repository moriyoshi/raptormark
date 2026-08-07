import assert from 'node:assert/strict';
import { test } from 'vitest';

import { InboundSockets, parseResponse, serializeRequest } from './inbound.ts';

const dec = (b: Uint8Array) => new TextDecoder().decode(b);
const enc = (s: string) => new TextEncoder().encode(s);

test('a request carries a synthesized Host and Connection: close', () => {
  const wire = dec(
    serializeRequest(
      {
        method: 'GET',
        url: 'http://example.test/_guest/a/b?q=1',
        headers: [['Accept', '*/*']],
      },
      '127.0.0.1:8080',
    ),
  );
  const lines = wire.split('\r\n');

  assert.equal(lines[0], 'GET /_guest/a/b?q=1 HTTP/1.1');
  // ⚠️ `Host` is a forbidden header, so `fetch` never exposes it on a Request.
  // Every HTTP/1.1 server requires one -- nginx answers 400 without it.
  assert.equal(lines[1], 'Host: 127.0.0.1:8080');
  assert.ok(lines.includes('Accept: */*'));
  // ⚠️ This is what BOUNDS the response: the host reads until the guest closes
  // its write side, so a keep-alive server would never appear to finish.
  assert.ok(lines.includes('Connection: close'));
  assert.ok(wire.endsWith('\r\n\r\n'), 'headers must be terminated');
});

test('a client Connection header is not forwarded', () => {
  const wire = dec(
    serializeRequest(
      {
        method: 'GET',
        url: 'http://x/_guest/',
        headers: [
          ['Connection', 'keep-alive'],
          ['Host', 'evil.test'],
        ],
      },
      '127.0.0.1:80',
    ),
  );
  assert.ok(!wire.includes('keep-alive'), 'the caller must not override framing');
  assert.ok(!wire.includes('evil.test'), 'the caller must not override the authority');
  assert.equal(wire.match(/Host:/g)?.length, 1);
});

test('a body sets Content-Length and follows the headers', () => {
  const wire = dec(
    serializeRequest(
      {
        method: 'POST',
        url: 'http://x/_guest/p',
        headers: [],
        body: enc('hello'),
      },
      'h:1',
    ),
  );
  assert.ok(wire.includes('Content-Length: 5'));
  assert.ok(wire.endsWith('\r\n\r\nhello'));
});

test('a response parses and drops hop-by-hop headers', () => {
  const r = parseResponse(
    enc(
      'HTTP/1.1 201 Created\r\n' +
        'Content-Type: text/plain\r\n' +
        'Content-Length: 2\r\n' +
        'Connection: close\r\n' +
        'X-Guest: yes\r\n' +
        '\r\n' +
        'hi',
    ),
  );
  assert.equal(r.status, 201);
  assert.equal(r.statusText, 'Created');
  assert.equal(dec(r.body), 'hi');

  const names = r.headers.map(([k]) => k.toLowerCase());
  assert.ok(names.includes('content-type'));
  assert.ok(names.includes('x-guest'));
  // ⚠️ Forwarding `Content-Length` is the dangerous one: the browser would
  // believe it over the actual body length. The others describe the GUEST'S
  // socket, not this response.
  assert.ok(!names.includes('content-length'));
  assert.ok(!names.includes('connection'));
});

test('a chunked body is DECODED, not just stripped of its header', () => {
  // ⚠️ THIS TEST USED TO DECLARE `chunked` AND SEND A RAW BODY, and passed --
  // because nothing acted on the header it declared. It was checking that the
  // header is dropped, which is only half the job: forwarding a chunk-encoded
  // body with the header removed hands the browser data interleaved with hex
  // length lines, and that RENDERS as garbage rather than failing.
  const r = parseResponse(
    enc(
      'HTTP/1.1 200 OK\r\n' +
        'Transfer-Encoding: chunked\r\n' +
        '\r\n' +
        '5\r\nhello\r\n' +
        '6\r\n world\r\n' +
        '0\r\n\r\n',
    ),
  );
  assert.equal(dec(r.body), 'hello world');
  const names = r.headers.map(([k]) => k.toLowerCase());
  assert.ok(!names.includes('transfer-encoding'));
});

test('a body containing CRLFCRLF is not split twice', () => {
  const r = parseResponse(enc('HTTP/1.1 200 OK\r\nX: 1\r\n\r\nbefore\r\n\r\nafter'));
  assert.equal(dec(r.body), 'before\r\n\r\nafter');
});

test('a malformed response is refused rather than guessed at', () => {
  assert.throws(() => parseResponse(enc('not http at all')), /header terminator/);
  assert.throws(() => parseResponse(enc('GARBAGE\r\n\r\n')), /status line/);
});

/**
 * ⚠️ A REPEAT `listen` MUST NOT DISCARD THE BACKLOG. Linux's `listen(2)` on an
 * already-listening socket adjusts the backlog and nothing else, and nginx
 * re-listens on every startup (`ngx_configure_listening_sockets`), so anything
 * queued in between would vanish.
 *
 * WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A test that only checked
 * the second `listen` returned SUCCESS would pass against the version that reset
 * the queue, because resetting it also returns SUCCESS. So the assertion is that
 * a connection delivered BEFORE the second listen is still acceptable AFTER it.
 *
 * This has never bitten in a browser only because the second call lands during
 * boot, before a request can exist. In the Node backend the same double-listen
 * was catastrophic, which is what prompted looking here.
 */
test('a second listen keeps queued connections', () => {
  const b = new InboundSockets({ handleBase: 1, notify: () => {} });
  const { errno, handle } = b.open(false, false);
  assert.equal(errno, 0);
  assert.equal(b.bind(handle, { ip: '0.0.0.0', port: 8080, v6: false }), 0);
  assert.equal(b.listen(handle, 16), 0, 'first listen');

  // A request arrives and queues on the listener.
  void b.deliver(8080, new TextEncoder().encode('GET /queued HTTP/1.1\r\n\r\n'));

  assert.equal(b.listen(handle, 511), 0, 'second listen must succeed');

  const a = b.accept(handle);
  assert.equal(
    a.errno,
    0,
    'the connection queued before the second listen was discarded by it; ' +
      'listen(2) adjusts the backlog, it does not empty it',
  );
});
