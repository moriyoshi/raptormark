import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';

const wantSHA = readFileSync(new URL('../public/data.bin.sha256', import.meta.url), 'utf8').trim();

/**
 * nginx serving real FILES out of the RAPTORFS sidecar.
 *
 * Every earlier nginx variant used `return 200`, which needs no document root —
 * so the sidecar was read exactly once, at startup, for nginx.conf. Here the VFS
 * is on the request path: nginx opens, stats and reads a file per request,
 * resolves an index, and picks a content type from mime.types.
 *
 * ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A body that merely
 * "looks right" is weak — a short read, an off-by-one, or a buffer reused
 * between requests all produce plausible text. So the binary file is checked by
 * SHA-256 against the bytes the Go side wrote, which is the only assertion here
 * that a partially-working read cannot satisfy.
 */
function suite(name: string, rootfs: string) {
  test(name, async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(String(e)));

    await page.goto(`/inbound.html?module=./public/nginx.wasm&rootfs=${rootfs}&port=8080`);
    await page
      .waitForFunction(() => (window as any).__ready === true || (window as any).__error, null, {
        timeout: 180_000,
      })
      .catch(async () => {
        const partial = (await page.locator('#out').textContent()) ?? '';
        throw new Error(`nginx never listened\nnginx log:\n${partial}`);
      });
    expect(await page.evaluate(() => (window as any).__error)).toBeFalsy();

    const got = await page.evaluate(async () => {
      const hex = (b: ArrayBuffer) =>
        [...new Uint8Array(b)].map((x) => x.toString(16).padStart(2, '0')).join('');
      const index = await fetch('/_guest/');
      const css = await fetch('/_guest/style.css');
      const bin = await fetch('/_guest/data.bin');
      const missing = await fetch('/_guest/nope.txt');
      const binBytes = await bin.arrayBuffer();
      return {
        index: {
          status: index.status,
          type: index.headers.get('content-type'),
          body: await index.text(),
        },
        css: { status: css.status, type: css.headers.get('content-type'), body: await css.text() },
        bin: {
          status: bin.status,
          type: bin.headers.get('content-type'),
          len: binBytes.byteLength,
          sha: hex(await crypto.subtle.digest('SHA-256', binBytes)),
        },
        missing: { status: missing.status, body: await missing.text() },
      };
    });

    const log = (await page.locator('#out').textContent()) ?? '';
    const ctx = `\n--- nginx log tail ---\n${log.split('\n').slice(-12).join('\n')}`;

    // Index resolution: the request names a DIRECTORY and nginx finds index.html.
    expect(got.index.status, `GET /_guest/ failed${ctx}`).toBe(200);
    expect(got.index.body).toContain('RAPTORMARK-VFS-INDEX');
    expect(got.index.type, `wrong type for the index${ctx}`).toContain('text/html');

    // mime.types was read from the sidecar too, and nginx applied it.
    expect(got.css.status).toBe(200);
    expect(got.css.body).toContain('RAPTORMARK-VFS-CSS');
    expect(got.css.type, `mime.types was not applied${ctx}`).toContain('text/css');

    // ⚠️ THE LOAD-BEARING ASSERTION. 64 KiB of PRNG bytes, byte for byte.
    expect(got.bin.status).toBe(200);
    expect(got.bin.len, `wrong length from the VFS${ctx}`).toBe(64 * 1024);
    expect(got.bin.sha, `data.bin came back CORRUPTED or short${ctx}`).toBe(wantSHA);
    expect(got.bin.type).toContain('application/octet-stream');

    // ⚠️ nginx FALLS BACK SILENTLY. If sendfile(2) fails it logs `sendfile()
    // failed` and serves the file by read/write instead, so every assertion
    // above would still pass while the mechanism under test never ran. Traced
    // directly once: `svc nr=71` from both workers with count 0x10000, the whole
    // 64 KiB in one call. This is the cheap standing check for the fallback.
    expect(log, `nginx fell back from sendfile${ctx}`).not.toContain('sendfile() failed');

    // A missing file is nginx's own 404, from a stat that failed in the VFS.
    expect(got.missing.status, `expected 404 for a missing file${ctx}`).toBe(404);
    expect(got.missing.body).toContain('404 Not Found');
  });
}

suite('nginx serves files from the VFS', './public/nginx-files.img');
suite('nginx serves files from the VFS with sendfile on', './public/nginx-sendfile.img');
