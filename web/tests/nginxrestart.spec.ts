import { expect, test } from '@playwright/test';

// ⚠️ SIGTERM, NOT SIGKILL, and this is a fact about ecvisor rather than a
// preference. `deliver_pending_signals` does NOT model default actions: a
// signal with no installed handler is SIG_DFL, and the runtime leaves the bit
// pending and does nothing. SIGKILL is uncatchable and therefore never has a
// handler, so killing a worker with it is a silent no-op -- measured, the
// worker kept serving every request afterwards.
//
// nginx's worker installs a SIGTERM handler, so 15 is delivered for real.
const SIGTERM = 15;

/**
 * A worker dies and nginx's master replaces it.
 *
 * ⚠️ THIS NEEDED A SENDER. A guest in a tab has no outside: nothing can `kill`
 * it, so a supervisor whose entire job is reacting to signals could only ever be
 * watched doing its startup work. `ecv_signal` is that missing sender, queued by
 * the host and delivered BETWEEN slices because a slice holds the process table
 * `&mut`.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? "Requests still work"
 * proves nothing -- the surviving worker would serve them all. So the test
 * requires a pid that did NOT exist before the kill, which only a re-fork can
 * produce, and requires the killed pid never to serve again.
 */
test('a killed worker is replaced by the master', async ({ page }) => {
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

  // Learn both workers before touching anything.
  const before = new Set<number>();
  for (let i = 0; i < 6; i++) before.add(await hit());
  expect(before.size, `expected two workers, saw ${[...before]}`).toBeGreaterThan(1);
  expect(before.has(1), 'the master served a request').toBe(false);

  const victim = [...before][0]!;
  await page.evaluate(([p, sig]) => (window as any).__signal(p, sig), [victim, SIGTERM]);

  // The master has to be scheduled to reap and re-fork, and the signal is
  // delivered between slices -- so this is a bounded poll, not an instant.
  let replacement = -1;
  const seen: number[] = [];
  for (let i = 0; i < 60 && replacement < 0; i++) {
    const p = await hit();
    seen.push(p);
    if (!before.has(p)) replacement = p;
    else await page.waitForTimeout(50);
  }

  const log = (await page.locator('#out').textContent()) ?? '';
  const ctx = `\nkilled=${victim} before=${[...before]} seen=${seen.join(',')}\n--- nginx log tail ---\n${log
    .split('\n')
    .slice(-20)
    .join('\n')}`;

  // ⚠️ THE LOAD-BEARING ASSERTION. A pid that did not exist before the kill can
  // only have come from the master forking a replacement.
  expect(replacement, `no replacement worker ever appeared${ctx}`).toBeGreaterThan(1);

  // ⚠️ A REQUEST IN FLIGHT ON THE DYING WORKER CAN BE LOST, and that is true of
  // any server -- the connection closes without a response and the host reports
  // a 502. `seen` therefore may contain a -1 around the kill, and pretending
  // otherwise would be asserting something untrue. What must hold is that the
  // STEADY STATE recovers completely.
  const settled: number[] = [];
  for (let i = 0; i < 8; i++) settled.push(await hit());
  for (const p of settled) {
    expect(p, `a request failed after recovery (${settled.join(',')})${ctx}`).toBeGreaterThan(1);
  }
  expect(settled.includes(victim), `the killed worker served again${ctx}`).toBe(false);
  // Both workers again: the replacement is carrying traffic, not just existing.
  expect(
    new Set(settled).size,
    `only one worker serving after recovery (${settled.join(',')})${ctx}`,
  ).toBeGreaterThan(1);

  // nginx's master says so itself, independently of which pid answered.
  expect(log, `master never logged the exit${ctx}`).toMatch(/worker process \d+ exited/);
});
