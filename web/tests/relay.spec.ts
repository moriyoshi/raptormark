import { expect, test } from '@playwright/test';

/**
 * The relay: a guest in a tab reaching a TCP server.
 *
 * ⚠️ EVERY LAYER HAS TO WORK FOR THIS TO PASS, which is why the assertion is on
 * the exchanged bytes rather than on any one of them. The guest resolves a NAME
 * (only the in-runtime DNS tap can answer it), connects to the synthetic address
 * it gets back (only the address pool can reverse it), and the relay dials the
 * real destination (only it can -- a browser cannot open a TCP socket, and
 * `fetch` cannot carry a stream).
 *
 * The destination is on loopback, which the page could not reach on the guest's
 * behalf by any other route.
 */
test('a guest in a tab reaches a TCP server through the relay', async ({ page }) => {
  const relay = process.env.RAPTORMARK_RELAY_URL;
  const port = process.env.RAPTORMARK_TARGET_PORT;
  test.skip(!relay || !port, 'needs RAPTORMARK_RELAY_URL and RAPTORMARK_TARGET_PORT');

  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text());
  });

  // ⚠️ NO `rootfs`, deliberately. The sidecar carries a BOOT RECORD, and when
  // one is present its argv WINS over the argv the host passed -- so adding a
  // sidecar here silently replaced the destination port with the guest's own
  // default, and the relay refused `localhost:80` against an allowlist naming
  // the real port. This guest needs no filesystem; the DNS tap answers at the
  // wire, so it does not even need `/etc/resolv.conf`.
  const q = new URLSearchParams({
    module: './public/net.wasm',
    relay: relay!,
  });
  // The guest reads its destination port from argv.
  q.append('arg', 'guest');
  q.append('arg', port!);

  await page.goto(`/?${q}`);
  try {
    await page.waitForFunction(() => (window as any).__result !== undefined, null, {
      timeout: 60_000,
    });
  } catch (e) {
    // ⚠️ Report what the guest DID manage before hanging. A bare timeout says
    // only "no result", which is the same message for a failure at DNS, at
    // connect, and at the first read -- and those need different fixes.
    const partial = (await page.locator('#out').textContent()) ?? '';
    const status = (await page.locator('#status').textContent()) ?? '';
    // `cause` keeps the original timeout: this message replaces it, and the
    // replacement is more useful but strictly less specific about WHERE it gave
    // up.
    throw new Error(
      `timed out; status=${status}\npartial output:\n${partial}\n${errors.join('\n')}`,
      { cause: e },
    );
  }
  const result = await page.evaluate(() => (window as any).__result);
  const out = (await page.locator('#out').textContent()) ?? '';
  expect(result.error, `page reported: ${result.error}\n${errors.join('\n')}`).toBeUndefined();
  for (const line of out.split('\n')) {
    expect(line.startsWith('FAIL '), `guest check failed: ${line}`).toBe(false);
  }
  // DNS-OK means resolve, connect, send AND receive all completed.
  expect(out).toContain('DNS-OK');
  expect(result.exitCode).toBe(0);

  // The address must have come from the synthetic pool. Without this the test
  // would pass on a host that never intercepted the resolver -- the guest would
  // have found `localhost` in its own hosts file and the relay would have dialled
  // it anyway.
  const m = /resolved=([0-9.]+)/.exec(out);
  expect(m, 'the guest never reported a resolved address').not.toBeNull();
  expect(
    Number(m![1].split('.')[0]),
    `resolved ${m![1]}, not from the synthetic range`,
  ).toBeGreaterThanOrEqual(240);
});
