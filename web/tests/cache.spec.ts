import { expect, test } from '@playwright/test';

/**
 * The module-bytes cache.
 *
 * ⚠️ THE COMPILED MODULE CANNOT BE CACHED, and believing otherwise is the trap
 * this test exists to catch. Storing a `WebAssembly.Module` in IndexedDB is
 * widely advised and Chromium refuses it outright -- "A WebAssembly.Module can
 * not be serialized for storage" -- so the first version of this code cached
 * nothing while appearing to. It failed SILENTLY: the write rejected, every read
 * missed, and the page recompiled every time.
 *
 * It was caught only because this test PRINTS hit-or-miss instead of merely
 * asserting the page still works. A test that checked "the second load also
 * succeeds" would have passed against a cache that did nothing at all -- which
 * is precisely what was there.
 *
 * What is cached now is the BYTES, through the Cache API, which removes the
 * download. Compilation is left to the engine's own implicit code cache.
 */
test('a second load reuses the fetched bytes, or falls back cleanly', async ({ page }) => {
  const url = '/?module=./public/guest.wasm&rootfs=./public/rootfs.img&v=t1';

  await page.goto(url);
  await page.waitForFunction(() => (window as any).__result !== undefined, null, {
    timeout: 120_000,
  });
  const first = await page.evaluate(() => (window as any).__result);
  expect(first.exitCode).toBe(0);
  const firstOut = await page.locator('#out').textContent();

  await page.reload();
  await page.waitForFunction(() => (window as any).__result !== undefined, null, {
    timeout: 120_000,
  });
  const second = await page.evaluate(() => (window as any).__result);
  const secondOut = await page.locator('#out').textContent();

  // Whether it hit or missed, the guest must behave identically. That is the
  // property worth guarding: a cache is an optimisation, and one that changes
  // behaviour is a bug however fast it is.
  expect(second.exitCode).toBe(0);
  expect(secondOut).toContain('BROWSER-OK');

  const firstHit = firstOut?.includes('from cache') ?? false;
  const secondHit = secondOut?.includes('from cache') ?? false;
  console.log('CACHE first=%s second=%s', firstHit ? 'hit' : 'miss', secondHit ? 'hit' : 'miss');

  // The FIRST load populates and must therefore miss; a hit there means a
  // previous run leaked state into this one.
  expect(firstHit, 'the first load reported a hit, so the cache was pre-populated').toBe(false);
  // The second must hit. Tolerating a miss here is what let a cache that stored
  // nothing look healthy for as long as it did.
  expect(secondHit, 'the second load did not reuse the cached bytes').toBe(true);
});
