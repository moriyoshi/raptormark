import { expect, test } from '@playwright/test';

/**
 * Surviving a service worker restart.
 *
 * ⚠️ A SERVICE WORKER IS NOT A LONG-LIVED PROCESS. The browser terminates an
 * idle one and starts a fresh copy for the next event, and that copy has never
 * heard of the page that announced its port. Whatever the worker held in module
 * scope is simply gone.
 *
 * The first version treated a missing port as "no page is open" and answered
 * 503. That failed INTERMITTENTLY -- only under enough load for the worker to
 * look idle -- with a message saying no host was connected, sent from a page
 * that was open and running the whole time.
 *
 * The restart cannot be provoked from here, so the worker accepts a message that
 * makes it forget, producing exactly the state a restart leaves behind.
 */
test('a worker that has lost its host port recovers instead of failing', async ({ page }) => {
  await page.goto('/inbound.html?module=./public/httpd.wasm');
  await page.waitForFunction(
    () => (window as any).__ready === true || (window as any).__error,
    null,
    {
      timeout: 120_000,
    },
  );
  expect(await page.evaluate(() => (window as any).__error)).toBeFalsy();

  const before = await page.evaluate(async () => (await fetch('/_guest/before')).text());
  expect(before).toContain('path=/_guest/before');

  // Exactly the state a restarted worker is in: alive, controlling, no port.
  await page.evaluate(async () => {
    navigator.serviceWorker.controller!.postMessage({ type: 'raptormark-forget-host' });
    // Let the worker process it before the next request goes out.
    await new Promise((r) => setTimeout(r, 50));
  });

  const after = await page.evaluate(async () => {
    const r = await fetch('/_guest/after');
    return { status: r.status, body: await r.text() };
  });

  const log = (await page.locator('#out').textContent()) ?? '';
  expect(after.status, `after forgetting the port: ${after.body}\n${log}`).toBe(200);
  expect(after.body).toContain('path=/_guest/after');

  // Still the same guest, with its per-connection counter still advancing --
  // recovery re-attached to the running guest rather than starting anything.
  const n = (s: string) => Number(/req=(\d+)/.exec(s)?.[1] ?? -1);
  expect(n(after.body)).toBeGreaterThan(n(before));
});
