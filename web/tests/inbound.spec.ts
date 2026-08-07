import { expect, test } from '@playwright/test';

/**
 * A page rendered by an aarch64 server running in the same tab.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A service worker that
 * synthesized its own response would satisfy "the iframe rendered something",
 * and so would a cached response. So this fetches TWO distinct paths and
 * requires each to echo ITS OWN path and to carry a DIFFERENT, INCREASING
 * connection counter. Only something holding per-connection state behind a
 * socket can produce that, and the guest is the only such thing here.
 */
test('a service worker serves a guest listening on a socket in the tab', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  await page.goto('/inbound.html');

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

  // The iframe the page pointed at /_guest/index.html.
  const frame = page.frameLocator('#frame');
  await expect(frame.locator('body')).toContainText('RAPTORMARK-GUEST', { timeout: 30_000 });
  const first = (await frame.locator('body').textContent()) ?? '';
  expect(first, 'the guest must echo the path it was asked for').toContain(
    'path=/_guest/index.html',
  );

  // A SECOND request, to a DIFFERENT path. Fetched rather than framed so the
  // body is compared exactly.
  const second = await page.evaluate(async () => {
    const r = await fetch('/_guest/second/page?q=1');
    return { status: r.status, type: r.headers.get('content-type'), body: await r.text() };
  });

  expect(second.status).toBe(200);
  expect(
    second.type,
    'the guest set this header, so it proves headers survived the round trip',
  ).toContain('text/plain');
  expect(second.body).toContain('path=/_guest/second/page?q=1');

  // ⚠️ THE COUNTER IS THE LOAD-BEARING ASSERTION. Both bodies could echo their
  // own path from a worker that parsed the URL itself; only per-connection state
  // inside the guest makes the second one HIGHER than the first.
  const n = (s: string) => Number(/req=(\d+)/.exec(s)?.[1] ?? -1);
  expect(n(first), `no counter in first response: ${first}`).toBeGreaterThan(0);
  expect(
    n(second.body),
    `counter did not advance: first=${JSON.stringify(first)} second=${JSON.stringify(second.body)}`,
  ).toBeGreaterThan(n(first));

  // The guest logged each connection it served, which is independent evidence
  // that the requests reached it rather than being answered short of it.
  const out = (await page.locator('#out').textContent()) ?? '';
  expect(out).toContain('HTTPD-READY');
  expect(out).toContain('HTTPD-SERVED');
});
