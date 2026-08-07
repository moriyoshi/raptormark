import { expect, test } from '@playwright/test';

const N = 25;

/**
 * 25 requests in flight at once, through one single-threaded guest.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? "All 25 returned 200"
 * is nearly worthless: a host that serialised them completely would satisfy it,
 * and so would one that answered every socket with the same buffer. So each
 * request carries a DISTINCT marker in its path and each response must echo ITS
 * OWN marker back.
 *
 * That is the assertion that matters, because CROSSED RESPONSES are the shape a
 * concurrency bug takes here. Twenty-five connections are open simultaneously,
 * each with its own accepted socket, its own response framer and its own pending
 * promise; getting one reply onto the wrong one is silent, plausible, and
 * invisible to any check that only counts successes.
 *
 * The concurrency is real on the HOST side -- 25 sockets genuinely open at once
 * -- while the guest services them one leg at a time, which is what a
 * single-threaded re-entrant scheduler means. This does not claim parallelism.
 */
test('nginx answers 25 concurrent requests without crossing them', async ({ page }) => {
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
      throw new Error(`nginx never listened\nnginx log:\n${partial}`);
    });
  expect(await page.evaluate(() => (window as any).__error)).toBeFalsy();

  const out = await page.evaluate(async (n) => {
    const started = performance.now();
    // ⚠️ ALL ISSUED BEFORE ANY IS AWAITED. Mapping then awaiting the array is
    // what puts them in flight together; a `for await` loop would be 25
    // sequential requests wearing a concurrency test's name.
    const inFlight = Array.from({ length: n }, (_, i) =>
      fetch(`/_guest/echo?i=${i}`).then(async (r) => ({
        i,
        status: r.status,
        body: await r.text(),
      })),
    );
    // ⚠️ AWAIT FIRST, THEN STAMP. Written as
    //     { ms: performance.now() - started, results: await Promise.all(...) }
    // the object's properties evaluate in order, so `ms` is taken BEFORE the
    // await and measures how long it took to ISSUE 25 fetches -- 2 ms, a number
    // that looks like a latency and is not one.
    const results = await Promise.all(inFlight);
    return { ms: performance.now() - started, results };
  }, N);

  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\n--- nginx log tail ---\n${log.split('\n').slice(-15).join('\n')}`;

  expect(out.results.length).toBe(N);
  for (const r of out.results) {
    expect(r.status, `request ${r.i} failed${ctx}`).toBe(200);
    // ⚠️ ITS OWN marker. This is the crossed-response check.
    expect(r.body, `request ${r.i} got another request's reply: ${r.body}${ctx}`).toContain(
      `i=${r.i}`,
    );
  }

  // Both workers took part, as in the sequential case. Under load this is the
  // regime where nginx was once pinned to one worker (accept4 20/1/0/0).
  const pids = out.results.map((r) => Number(/pid=(\d+)/.exec(r.body)?.[1] ?? -1));
  for (const p of pids) expect(p, `no pid, or served by the master${ctx}`).toBeGreaterThan(1);
  const spread = new Set(pids);
  expect(
    spread.size,
    `all ${N} served by one worker (pids ${[...spread].join(',')})${ctx}`,
  ).toBeGreaterThan(1);

  console.log(
    `CONC ${N} requests in ${out.ms.toFixed(0)} ms, worker pids ${[...spread].join(',')}`,
  );
});
