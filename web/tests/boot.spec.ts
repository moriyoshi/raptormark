import { expect, test } from '@playwright/test';

/**
 * The claim: a raptormark module -- an aarch64 Linux program translated ahead of
 * time -- boots and runs inside a browser tab.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A page that failed to
 * load the module would still render, and a test that only checked the page
 * loaded would pass. So the assertions are on the GUEST's own output and on the
 * exit code the runtime reported -- neither of which exists unless the module
 * booted, read its sidecar, ran lifted aarch64 code, and exited through
 * `proc_exit`.
 */

const banner = process.env.RAPTORMARK_BROWSER_BANNER ?? 'BROWSER-OK';

test('a translated guest boots and prints in a tab', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text());
  });

  const module = process.env.RAPTORMARK_BROWSER_MODULE_URL ?? './public/guest.wasm';
  const rootfs = process.env.RAPTORMARK_BROWSER_ROOTFS_URL ?? './public/rootfs.img';
  const q = new URLSearchParams({ module });
  if (rootfs) q.set('rootfs', rootfs);

  await page.goto(`/?${q}`);

  // `__result` is set by the page once the guest has exited, either way.
  await page.waitForFunction(() => (window as any).__result !== undefined, null, {
    timeout: 150_000,
  });
  const result = await page.evaluate(() => (window as any).__result);

  expect(
    result.error,
    `page reported: ${result.error}\nconsole: ${errors.join('\n')}`,
  ).toBeUndefined();
  expect(await page.locator('#out').textContent()).toContain(banner);
  expect(result.exitCode).toBe(0);

  // The re-entrant driver must actually have run slices. A module driven by
  // `_start` would produce the same output while blocking the tab, which is the
  // thing this whole profile exists to avoid.
  expect(result.slices, 'the driver ran no slices').toBeGreaterThan(0);
});
