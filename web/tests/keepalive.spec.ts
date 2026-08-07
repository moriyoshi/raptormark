import { expect, test } from '@playwright/test';

/**
 * A response ended by FRAMING, not by the guest hanging up.
 *
 * The guest answers with a `Content-Length` and then holds the connection open
 * forever, the way an HTTP/1.1 server waiting for the next request does. A host
 * that decided a response was over when the socket closed would wait for a close
 * that never comes and report a timeout against a server that answered correctly.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? The requests time out.
 * That bluntness is deliberate: against a guest that DOES close, a framing host
 * and a wait-for-close host produce identical bytes at nearly the same instant,
 * so no assertion on the response could tell them apart.
 */
test('a server that never closes is still answered promptly', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  await page.goto('/inbound.html?module=./public/ka.wasm');

  await page
    .waitForFunction(() => (window as any).__ready === true || (window as any).__error, null, {
      timeout: 120_000,
    })
    .catch(async () => {
      const partial = (await page.locator('#out').textContent()) ?? '';
      throw new Error(`never became ready\npartial output:\n${partial}`);
    });

  const failed = await page.evaluate(() => (window as any).__error);
  expect(failed, `page reported: ${failed}\n${errors.join('\n')}`).toBeFalsy();

  // Timed on the page. The host's request deadline is 15 s, so a wait-for-close
  // host cannot come in under it -- there is no close to wait for.
  const runs = await page.evaluate(async () => {
    const out: Array<{ ms: number; body: string; status: number }> = [];
    for (const p of ['/_guest/alpha', '/_guest/beta']) {
      const t0 = performance.now();
      const r = await fetch(p);
      const body = await r.text();
      out.push({ ms: performance.now() - t0, body, status: r.status });
    }
    return out;
  });

  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\n--- guest log ---\n${log}`;

  expect(runs[0]!.status, `first request failed${ctx}`).toBe(200);
  expect(runs[0]!.body + ctx).toContain('path=/_guest/alpha');
  expect(runs[1]!.body + ctx).toContain('path=/_guest/beta');

  // Per-connection state still advances, so each really was a fresh connection
  // into the guest rather than one reply reused.
  const n = (s: string) => Number(/req=(\d+)/.exec(s)?.[1] ?? -1);
  expect(n(runs[1]!.body), `counter did not advance${ctx}`).toBeGreaterThan(n(runs[0]!.body));

  // ⚠️ The load-bearing assertion. 15 s is the host's request deadline; a host
  // waiting for a close would spend all of it and then fail, so anything near it
  // means the framing did not do the work even if the bytes look right.
  for (const r of runs) {
    expect(r.ms, `took ${r.ms} ms, which is deadline territory${ctx}`).toBeLessThan(5_000);
  }

  expect(log).toContain('KA-READY');
  expect(log).toContain('KA-SERVED');
});
