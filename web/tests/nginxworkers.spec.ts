import { expect, test } from '@playwright/test';

/**
 * nginx's real process model: a master that forks workers.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A response is not
 * evidence of a worker -- the master could have served it. So the assertion is
 * on nginx's own `$pid` in the body, which must not be the master's. ecvisor
 * starts the master as pid 1, so a reply from pid 1 means the fork never
 * produced a process that ran.
 *
 * This is the path that had never been driven re-entrantly: ecvisor
 * reconstructs a fork by FULL REPLAY, and a browser host returns to its event
 * loop part-way through, which a blocking host never does.
 */
test('nginx forks workers and they serve the requests', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  await page.goto(
    '/inbound.html?module=./public/nginx.wasm&rootfs=./public/nginx-workers.img&port=8080',
  );

  await page
    .waitForFunction(() => (window as any).__ready === true || (window as any).__error, null, {
      timeout: 180_000,
    })
    .catch(async () => {
      const partial = (await page.locator('#out').textContent()) ?? '';
      const status = (await page.locator('#status').textContent()) ?? '';
      throw new Error(`nginx never listened; status=${status}\nnginx log:\n${partial}`);
    });

  expect(await page.evaluate(() => (window as any).__error)).toBeFalsy();

  const bodies: string[] = [];
  for (const p of ['/_guest/one', '/_guest/two', '/_guest/three', '/_guest/four']) {
    bodies.push(await page.evaluate(async (u) => (await fetch(u)).text(), p));
  }

  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\n--- nginx log ---\n${log}\n--- bodies ---\n${bodies.join('')}`;

  for (const b of bodies) expect(b + ctx).toContain('RAPTORMARK-NGINX-OK');

  const pids = bodies.map((b) => Number(/pid=(\d+)/.exec(b)?.[1] ?? -1));
  for (const p of pids) expect(p, `no pid in a response${ctx}`).toBeGreaterThan(0);

  // ⚠️ THE LOAD-BEARING ASSERTION. ecvisor's master is pid 1. A reply from pid 1
  // means no forked child ever ran, however correct the body looks.
  for (const p of pids) {
    expect(p, `served by the MASTER, so no worker ran${ctx}`).not.toBe(1);
  }

  // ⚠️ BOTH workers must have served. This guards a bug with history: nginx was
  // once pinned to a single worker here, and the traced `accept4` counts across
  // four workers were 20 / 1 / 0 / 0 -- two of them never entered the race even
  // once. Readiness is discovered privately by whoever calls `epoll_pwait`, so
  // without the scheduler rotating its wake order the same process wins every
  // time. Four requests across two workers came back 3/2/3/2, exact alternation.
  expect(
    new Set(pids).size,
    `every request was served by the same worker (pids ${pids.join(',')})${ctx}`,
  ).toBeGreaterThan(1);

  // nginx logs the pid of each worker it starts. Independent of the responses.
  expect(log, `no worker start in the log${ctx}`).toMatch(/start worker process \d+/);
});
