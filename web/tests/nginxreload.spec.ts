import { expect, test } from '@playwright/test';

// SIGHUP. nginx's master installs a handler for it, which is what makes it
// deliverable at all -- see `nginxrestart.spec.ts` on unmodelled default
// actions.
const SIGHUP = 1;

/**
 * A graceful reload: nginx re-reads its config and cycles its workers without
 * dropping the listener.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? "Requests still work"
 * is satisfied by a signal that did nothing at all -- the original workers would
 * carry on serving, which is precisely the healthy state. So the test requires
 * that EVERY worker serving afterwards is one that did not exist before: a
 * reload replaces its workers, and nothing else here produces new pids.
 *
 * The log lines are nginx's own account of the same events, and are independent
 * of which pid happened to answer.
 */
test('SIGHUP cycles the workers and keeps serving', async ({ page }) => {
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

  const pidOf = (b: string) => Number(/pid=(\d+)/.exec(b)?.[1] ?? -1);
  const hit = async () => pidOf(await page.evaluate(async () => (await fetch('/_guest/')).text()));

  const before = new Set<number>();
  for (let i = 0; i < 6; i++) before.add(await hit());
  expect(before.size, `expected two workers, saw ${[...before]}`).toBeGreaterThan(1);

  await page.evaluate(([sig]) => (window as any).__signal(1, sig), [SIGHUP]);

  // The master must be scheduled to reconfigure and fork; old workers finish
  // what they are holding first. Poll until nothing old is answering.
  // ⚠️ THE POLL ONLY WAITS; IT DOES NOT DECIDE. An earlier version asserted on
  // whether the loop reached a settled state, so neutralizing the signal tripped
  // that instead of the assertion below -- leaving the one that carries the
  // claim unexercised. The verdict is taken from an UNCONDITIONAL final sample.
  // Wait for the old workers to drain. ⚠️ A FIXED SLEEP IS NOT ENOUGH -- 150 ms
  // caught worker 2 mid-retirement and failed a reload that had worked
  // perfectly (`settled=2,5,4,5,4,5,4,5`, one stale sample and seven new ones).
  // Graceful shutdown finishes when it finishes.
  const seen: number[] = [];
  for (let i = 0; i < 100; i++) {
    const probe: number[] = [];
    for (let k = 0; k < 4; k++) probe.push(await hit());
    seen.push(...probe);
    if (probe.every((p) => p > 1 && !before.has(p))) break;
    await page.waitForTimeout(25);
  }

  const settled: number[] = [];
  for (let k = 0; k < 8; k++) settled.push(await hit());

  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\nbefore=${[...before]} seen=${seen.join(',')} settled=${settled.join(',')}\n--- nginx log tail ---\n${log
    .split('\n')
    .slice(-24)
    .join('\n')}`;

  // ⚠️ THE LOAD-BEARING ASSERTION. Every worker serving after the reload is one
  // that did not exist before it. A no-op signal leaves the originals serving,
  // and this is what says so.
  for (const p of settled) {
    expect(p, `a request failed after the reload${ctx}`).toBeGreaterThan(1);
    expect(before.has(p), `an OLD worker is still serving after the reload${ctx}`).toBe(false);
  }
  // More than one new worker, so the reload rebuilt the pool rather than
  // limping along on a single process.
  expect(new Set(settled).size, `only one worker after the reload${ctx}`).toBeGreaterThan(1);

  // nginx's own account: it re-read the config, and the old workers retired
  // rather than being killed.
  expect(log, `nginx never reported reconfiguring${ctx}`).toContain('reconfiguring');
  expect(log, `old workers were not shut down gracefully${ctx}`).toContain(
    'gracefully shutting down',
  );
});
