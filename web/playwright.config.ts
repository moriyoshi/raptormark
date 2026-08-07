import { defineConfig, devices } from '@playwright/test';

/**
 * Browser tests for the raptormark embedder.
 *
 * ⚠️ Env-gated and never part of a default gate. `npx playwright install`
 * downloads ~500 MB of browser builds and needs network, which is the same
 * reason the Docker E2E suite is opt-in. The Go side skips unless
 * `RAPTORMARK_E2E_BROWSER=1`.
 *
 * `RAPTORMARK_BROWSER_MODULE` points at a PRE-BUILT `--profile browser` module.
 * Without it these tests would inherit the whole translation pipeline and would
 * therefore never be run.
 *
 * More than one engine on purpose. They differ on exactly what this design
 * leans on -- structured-clone of `WebAssembly.Module` into IndexedDB (the
 * compiled-module cache), streaming request bodies, WebTransport availability --
 * so a single-engine harness would certify a cache that silently never hits
 * elsewhere.
 */
export default defineConfig({
  testDir: './tests',
  // A cold compile of a large module is not fast, and the guest then runs.
  timeout: 180_000,
  expect: { timeout: 30_000 },
  reporter: [['list']],
  use: { baseURL: process.env.RAPTORMARK_BASE_URL ?? 'http://127.0.0.1:8787' },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    // Firefox and WebKit are enabled by installing them; the config lists them
    // so adding an engine is one `playwright install` rather than an edit.
    ...(process.env.RAPTORMARK_BROWSERS?.split(',').includes('firefox')
      ? [{ name: 'firefox', use: { ...devices['Desktop Firefox'] } }]
      : []),
    ...(process.env.RAPTORMARK_BROWSERS?.split(',').includes('webkit')
      ? [{ name: 'webkit', use: { ...devices['Desktop Safari'] } }]
      : []),
  ],
});
