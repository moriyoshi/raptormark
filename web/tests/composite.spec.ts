import { expect, test } from '@playwright/test';

/**
 * One guest, both directions, two different transports.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A composite that sent
 * every socket to the inbound backend would still serve this page; one that sent
 * every socket to the relay would still reach the upstream. Neither could produce
 * a response carrying BOTH the guest's own per-connection counter AND the
 * upstream's reply, because that requires the two sockets to have been routed to
 * different backends inside a single process.
 *
 * The upstream is on loopback, so the relay is the only thing that can reach it,
 * and the guest resolves it by NAME, so the DNS tap and address pool are on the
 * path as well.
 */
test('one guest serves the page and dials an upstream', async ({ page }) => {
  const relay = process.env.RAPTORMARK_RELAY_URL;
  const upstream = process.env.RAPTORMARK_UPSTREAM_PORT;
  test.skip(!relay || !upstream, 'needs RAPTORMARK_RELAY_URL and RAPTORMARK_UPSTREAM_PORT');

  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  const q = new URLSearchParams({
    module: './public/both.wasm',
    relay: relay!,
    port: '8080',
  });
  // argv: [0] name, [1] listen port, [2] upstream host, [3] upstream port.
  q.append('arg', 'guest');
  q.append('arg', '8080');
  q.append('arg', 'localhost');
  q.append('arg', upstream!);

  await page.goto(`/inbound.html?${q}`);

  await page
    .waitForFunction(() => (window as any).__ready === true || (window as any).__error, null, {
      timeout: 120_000,
    })
    .catch(async () => {
      const partial = (await page.locator('#out').textContent()) ?? '';
      const status = (await page.locator('#status').textContent()) ?? '';
      throw new Error(`never became ready; status=${status}\npartial output:\n${partial}`);
    });

  const failed = await page.evaluate(() => (window as any).__error);
  expect(failed, `page reported: ${failed}\n${errors.join('\n')}`).toBeFalsy();

  const first = await page.evaluate(async () => (await fetch('/_guest/one')).text());
  const second = await page.evaluate(async () => (await fetch('/_guest/two')).text());

  // ⚠️ The guest's own log goes into every failure message. A request that times
  // out says only "no answer", which is the same message whether it hung at
  // accept, at the upstream resolve, or at the upstream connect -- and those are
  // three different bugs.
  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\n--- guest log ---\n${log}`;

  // The INBOUND half: the guest's own per-connection state and the path it read
  // off the socket the service worker minted.
  expect(first + ctx).toContain('path=/_guest/one');
  expect(second + ctx).toContain('path=/_guest/two');
  const n = (s: string) => Number(/req=(\d+)/.exec(s)?.[1] ?? -1);
  expect(n(second), `counter did not advance: ${first} / ${second}`).toBeGreaterThan(n(first));

  // The OUTBOUND half, in the same response: the upstream answered through the
  // relay. `upstream=PONG` can only come from a socket that left the tab.
  expect(first, `upstream did not answer: ${first}${ctx}`).toContain('upstream=PONG');
  expect(second, `upstream did not answer: ${second}${ctx}`).toContain('upstream=PONG');

  expect(log).toContain('BOTH-READY');
  expect(log).toContain('BOTH-SERVED');
});
