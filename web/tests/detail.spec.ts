import { expect, test } from '@playwright/test';

/**
 * The re-entrancy claim, in a real browser: a guest that sleeps must hand
 * control back to the page rather than block it.
 *
 * ⚠️ The banner guest cannot show this. It never blocks, so it completes in one
 * slice with zero idle waits -- proving the module boots and nothing about
 * whether the tab stayed alive. Only a guest that BLOCKS distinguishes the two,
 * which is why there is a second fixture.
 */
test('a sleeping guest yields the tab instead of blocking it', async ({ page }) => {
  await page.goto('/?module=./public/sleep.wasm&rootfs=./public/rootfs.img');

  // The page must remain responsive WHILE the guest is mid-sleep. Evaluating
  // anything at all requires the event loop to be free; a blocking host would
  // stall this until the guest finished.
  await page.waitForFunction(
    () => document.getElementById('out')?.textContent?.includes('BEFORE'),
    null,
    { timeout: 60_000 },
  );
  const alive = await page.evaluate(() => {
    // Runs on the main thread mid-run. If the guest were blocking it, this
    // would not be scheduled until the guest exited.
    return typeof performance.now() === 'number';
  });
  expect(alive, 'the page did not respond while the guest was sleeping').toBe(true);

  await page.waitForFunction(() => (window as any).__result !== undefined, null, {
    timeout: 120_000,
  });
  const r = await page.evaluate(() => (window as any).__result);
  console.log('RESULT', JSON.stringify(r));

  expect(r.error).toBeUndefined();
  expect(r.exitCode).toBe(0);
  expect(await page.locator('#out').textContent()).toContain('AFTER');
  // The guest must actually have gone idle: that IS the re-entrancy. Zero means
  // the scheduler blocked inside a slice and the tab was frozen throughout.
  expect(r.idleWaits, 'the guest never yielded, so the tab was blocked').toBeGreaterThan(0);
});
