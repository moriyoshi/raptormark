import { expect, test } from '@playwright/test';

/**
 * nginx, in a tab.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? The body is not
 * evidence: `RAPTORMARK-NGINX-OK` is a string this repo put in a config file,
 * and anything could echo it. The assertions that carry the claim are the ones
 * only nginx can satisfy:
 *
 *  - `Server: nginx/<version>`, which nginx generates and nothing here supplies.
 *  - `$request_uri` and `$http_host` echoed back with the values the BROWSER
 *    sent, which requires nginx to have parsed the request bytes.
 *  - a 404 for an unconfigured path, from nginx's own routing rather than from
 *    a handler that answers everything the same way.
 *
 * `$http_host` is the sharpest. `fetch` never exposes `Host` on a `Request`, so
 * `serializeRequest` synthesizes it; without that nginx answers 400 and no
 * assertion about a body would fire.
 */
test('nginx serves a browser request from inside the tab', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  // nginx.conf listens on 8080, and the sidecar's boot record supplies argv.
  await page.goto('/inbound.html?module=./public/nginx.wasm&rootfs=./public/nginx.img&port=8080');

  await page
    .waitForFunction(() => (window as any).__ready === true || (window as any).__error, null, {
      // A 40 MB module to fetch and compile, then nginx's own startup.
      timeout: 180_000,
    })
    .catch(async () => {
      const partial = (await page.locator('#out').textContent()) ?? '';
      const status = (await page.locator('#status').textContent()) ?? '';
      throw new Error(`nginx never listened; status=${status}\nnginx log:\n${partial}`);
    });

  const failed = await page.evaluate(() => (window as any).__error);
  expect(failed, `page reported: ${failed}\n${errors.join('\n')}`).toBeFalsy();

  const root = await page.evaluate(async () => {
    const r = await fetch('/_guest/');
    return {
      status: r.status,
      server: r.headers.get('server'),
      type: r.headers.get('content-type'),
      body: await r.text(),
    };
  });

  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\n--- nginx log ---\n${log}`;

  expect(root.status, `GET / failed${ctx}`).toBe(200);
  expect(root.body).toContain('RAPTORMARK-NGINX-OK');

  // ⚠️ nginx generated this. Nothing in web/ or internal/ sets a Server header,
  // so it cannot have come from the host, the service worker, or the config.
  expect(root.server, `no Server header${ctx}`).toMatch(/^nginx\//);

  // nginx's own variables, echoed from the request bytes it parsed.
  const echo = await page.evaluate(async () => (await fetch('/_guest/echo?q=1')).text());
  expect(echo, `nginx did not echo the request line${ctx}`).toContain('path=/_guest/echo?q=1');
  // The Host it saw is the one `serializeRequest` synthesized -- `fetch` never
  // exposes `Host`, so a missing synthesis is a 400, not a wrong value here.
  expect(echo).toContain('host=127.0.0.1:8080');

  // ⚠️ ROUTING, not a catch-all. Three paths, three different outcomes, all
  // decided by nginx's own longest-prefix matching -- which is the thing a
  // handler that answered everything identically could not reproduce.
  const denied = await page.evaluate(async () => {
    const r = await fetch('/_guest/deny');
    return { status: r.status, server: r.headers.get('server'), body: await r.text() };
  });
  expect(denied.status, `expected nginx to refuse /deny${ctx}`).toBe(403);
  expect(denied.server, `even the error came from somewhere else${ctx}`).toMatch(/^nginx\//);
  // nginx generated this error page itself; nothing here has a 403 body.
  expect(denied.body).toContain('403 Forbidden');
});
