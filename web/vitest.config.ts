import { defineConfig } from 'vitest/config';

/**
 * ⚠️ THE TWO SUITES MUST NOT OVERLAP, and by default they do. Vitest's default
 * include is `**\/*.{test,spec}.?(c|m)[jt]s?(x)`, which sweeps up `tests/*.spec.ts`
 * -- Playwright's directory. Those files `import { test } from '@playwright/test'`
 * and fail immediately under vitest, so the first run after adopting it reported
 * 14 failed files and 39 passing tests.
 *
 * The split is by LOCATION and it is deliberate:
 *
 *   * `src/**\/*.test.ts` -- vitest. Pure logic, fakes, and anything reachable
 *     without a browser. Milliseconds, runs on every change, no artifacts.
 *   * `tests/*.spec.ts` -- Playwright. Needs a real browser, a built bundle, a
 *     lifted guest and a server. Minutes, gated behind `RAPTORMARK_E2E_BROWSER`.
 *
 * The point of the split is that the FIRST list should keep growing at the
 * expense of the second: anything a fake can cover should not need Chromium.
 */
export default defineConfig({
  test: {
    include: ['src/**/*.test.ts'],
    // Belt and braces. `include` alone is enough today, but a future default
    // change or a stray `src/**/*.spec.ts` should not silently start running
    // browser specs in a Node process.
    exclude: ['tests/**', 'node_modules/**', 'dist/**', 'public/**'],
    // `Request`, `Response`, `MessageChannel` and `crypto` are all global in
    // Node 22+, so the service-worker and WASI shim code under test needs no DOM
    // emulation. Adding jsdom would slow the suite and change the very globals
    // the code is written against.
    environment: 'node',
  },
});
