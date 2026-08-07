import assert from 'node:assert/strict';
import net from 'node:net';
import { test } from 'vitest';
import { NodeSockets } from './sockets.ts';
import { E_SUCCESS } from './protocol.ts';

const BASE = 4096;

/**
 * `listen(2)` on an already-listening socket SUCCEEDS on Linux -- it only
 * adjusts the backlog -- and nginx does exactly this on every startup, because
 * `ngx_configure_listening_sockets` re-listens to apply the configured backlog
 * after the socket is already bound and listening.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? The second call
 * returning an errno is only half the failure and the cheaper half. The backend
 * used to create a SECOND `net.createServer()`, which could not bind (the first
 * holds the port), overwrote `s.server` with the failed one and set a STICKY
 * socket error -- and a socket carrying an error reports as permanently
 * readable. So the guest is woken forever by an accept that always fails.
 *
 * Asserting only on the errno would therefore pass against a fix that returned
 * 0 while still poisoning the socket. This asserts the errno AND that the
 * listener still works afterwards, by connecting to it for real.
 */
test('a second listen on the same socket succeeds and keeps the listener alive', async () => {
  const b = new NodeSockets(BASE);
  try {
    const { errno: openErrno, handle } = b.open(false, false);
    assert.equal(openErrno, E_SUCCESS, 'open');
    assert.equal(b.bind(handle, { ip: '127.0.0.1', port: 0, v6: false }), E_SUCCESS, 'bind');
    assert.equal(b.listen(handle, 511), E_SUCCESS, 'first listen');

    // The port is only knowable after listen: bind asked for 0.
    const bound = b.addr(handle, false);
    assert.equal(bound.errno, E_SUCCESS, 'getsockname');
    const port = bound.addr!.port;
    assert.ok(port > 0, 'bound to a real port');

    assert.equal(
      b.listen(handle, 511),
      E_SUCCESS,
      'a second listen must succeed; Linux only adjusts the backlog',
    );

    // The listener still accepts. Without this the test passes against a fix
    // that merely returns 0 early while the socket is already poisoned.
    await new Promise<void>((resolve, reject) => {
      const c = net.connect({ host: '127.0.0.1', port }, () => {
        c.end();
        resolve();
      });
      c.on('error', reject);
      setTimeout(
        () => reject(new Error(`nothing accepted on ${port} after a second listen`)),
        5000,
      );
    });

    // And the connection reached the guest-visible backlog, not just the OS.
    const deadline = Date.now() + 5000;
    let accepted = -1;
    while (Date.now() < deadline) {
      const a = b.accept(handle);
      if (a.errno === E_SUCCESS) {
        accepted = a.handle;
        break;
      }
      await new Promise((r) => setTimeout(r, 20));
    }
    assert.ok(accepted >= 0, 'accept never produced the connection that was made');
    b.close(accepted);
    b.close(handle);
  } finally {
    await b.stop();
  }
});

/**
 * A listen that genuinely FAILS must leave the socket retryable. nginx retries
 * EADDRINUSE five times, 500 ms apart, and a backend that remembered the failed
 * server would take the already-listening path and report a success that never
 * happened -- handing the guest a listener that does not exist.
 */
test('a failed listen can be retried', async () => {
  // Hold a port with an ordinary node server so the guest's bind collides.
  const blocker = net.createServer();
  await new Promise<void>((r) => blocker.listen({ host: '127.0.0.1', port: 0 }, () => r()));
  const port = (blocker.address() as net.AddressInfo).port;

  const b = new NodeSockets(BASE);
  try {
    const { handle } = b.open(false, false);
    assert.equal(b.bind(handle, { ip: '127.0.0.1', port, v6: false }), E_SUCCESS);
    const first = b.listen(handle, 511);
    assert.notEqual(first, E_SUCCESS, 'listening on a taken port must fail');

    // Free it, then retry on the SAME socket.
    await new Promise<void>((r) => blocker.close(() => r()));
    assert.equal(b.listen(handle, 511), E_SUCCESS, 'the retry must be accepted');

    // ⚠️ THE ERRNO ABOVE IS NOT THE TEST, and asserting only on it certifies the
    // bug. The failure mode here is precisely "reports success without
    // listening": a backend that remembered the FAILED server takes the
    // already-listening path and returns 0. Both the fix and the defect return
    // 0, so the only assertion that separates them is whether anything is
    // actually on the port. Caught by neutralizing -- the first version of this
    // test passed against the break.
    await new Promise<void>((resolve, reject) => {
      const c = net.connect({ host: '127.0.0.1', port }, () => {
        c.end();
        resolve();
      });
      c.on('error', (e) =>
        reject(
          new Error(
            `the retried listen reported success but nothing is on ${port}: ${e}. ` +
              'The backend kept the failed attempt and short-circuited.',
          ),
        ),
      );
      setTimeout(() => reject(new Error('connect to the retried listener timed out')), 5000);
    });
    b.close(handle);
  } finally {
    await b.stop();
    if (blocker.listening) await new Promise<void>((r) => blocker.close(() => r()));
  }
});
