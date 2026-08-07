import assert from 'node:assert/strict';
import { afterEach, test, vi } from 'vitest';

import { loadModule, loadRootfs } from './load.ts';

/**
 * The module cache, whose failure mode is documented as SILENT.
 *
 * ⚠️ THE ORIGINAL BUG WAS INVISIBLE FROM EVERY OBSERVABLE OUTCOME. Storing a
 * `WebAssembly.Module` in IndexedDB is widely advised and Chromium refuses it:
 * the write rejects, every read misses, and the page recompiles on every load
 * while APPEARING to have a cache. It was caught only because a test printed
 * hit-or-miss instead of asserting the page still worked -- and "the page still
 * works" is exactly what a broken cache looks like.
 *
 * So every test here asserts on the CACHE TRAFFIC, not on the module coming
 * back. A load that ignored the cache entirely would pass any test that only
 * checked the result.
 */

/**
 * ⚠️ ABSOLUTE URLS, because `new Request('/m.wasm')` throws outside a browser:
 * there is no document base to resolve against. `load.ts` is not wrong to pass
 * a relative URL -- the page hands it `./public/nginx.wasm` and the browser
 * resolves it against `location` -- so this is a property of the test
 * environment, not something to "fix" in the module under test.
 */
const BASE = 'https://host.test';

/** The four bytes of a valid empty wasm module, plus its version. */
const WASM = new Uint8Array([0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00]);

// ⚠️ `Uint8Array<ArrayBuffer>`, not a bare `Uint8Array`. Since TS 5.7 the type
// is generic over its backing buffer, and `BodyInit` wants the ArrayBuffer-backed
// form -- naming it here is what avoids a cast, because these really are
// ArrayBuffer-backed.
const wasmResponse = (body: Uint8Array<ArrayBuffer> = WASM) =>
  new Response(body, { headers: { 'content-type': 'application/wasm' } });

/** A Cache API good enough to be wrong in the ways that matter. */
function fakeCaches(opts: { openThrows?: boolean; putRejects?: boolean } = {}) {
  const store = new Map<string, Response>();
  const calls = { open: 0, match: 0, put: 0 };
  return {
    store,
    calls,
    api: {
      open: async () => {
        calls.open++;
        if (opts.openThrows) throw new Error('no storage on this origin');
        return {
          match: async (req: Request) => {
            calls.match++;
            const hit = store.get(req.url);
            return hit ? hit.clone() : undefined;
          },
          put: async (req: Request, res: Response) => {
            calls.put++;
            if (opts.putRejects) throw new Error('quota exceeded');
            store.set(req.url, res);
          },
        };
      },
    },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

test('loadRootfs returns the bytes, and names the URL and status when it cannot', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(new Uint8Array([1, 2, 3]))),
  );
  assert.deepEqual([...(await loadRootfs(BASE + '/rootfs.img'))], [1, 2, 3]);

  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response('nope', { status: 404 })),
  );
  // ⚠️ A missing sidecar is the most common misconfiguration here, and the guest
  // reports it as "the config file does not exist" from three layers down.
  await assert.rejects(loadRootfs(BASE + '/missing.img'), /missing\.img.*404/);
});

test('without a cache key the module is fetched and reported as uncached', async () => {
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  const cache = fakeCaches();
  vi.stubGlobal('caches', cache.api);

  const r = await loadModule(BASE + '/m.wasm');
  assert.equal(r.cached, false);
  assert.equal(fetchMock.mock.calls.length, 1);
  // ⚠️ No key means no cache ENTRY, not a cache keyed on the URL alone. A stale
  // artifact is the worst failure available here: silent, and indistinguishable
  // from a rebuild that did not take.
  assert.equal(cache.calls.open, 0, 'the cache must not be consulted without a version');
});

test('a first load misses and stores; a second serves from the cache without fetching', async () => {
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  const cache = fakeCaches();
  vi.stubGlobal('caches', cache.api);

  const first = await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  assert.equal(first.cached, false);
  assert.equal(cache.calls.put, 1, 'a miss must populate the cache');
  assert.equal(cache.store.size, 1);

  const second = await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  assert.equal(second.cached, true);
  // ⚠️ THIS IS THE ASSERTION THE WHOLE FILE EXISTS FOR. `cached: true` is a
  // claim about network traffic, and the original bug reported exactly that
  // while re-downloading every time.
  assert.equal(fetchMock.mock.calls.length, 1, 'a hit must not go to the network');
});

test('the stored copy is a clone, so the returned body is still readable', async () => {
  // ⚠️ `put` CONSUMES the body. Storing the original and returning it gives a
  // response whose stream is already locked -- `compileStreaming` then fails on
  // the FIRST load and succeeds on every one after it, which reads as a flaky
  // network rather than a bug here.
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  const cache = fakeCaches();
  vi.stubGlobal('caches', cache.api);

  const r = await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  assert.ok(r.module instanceof WebAssembly.Module, 'the first load must still compile');
});

test('a different version key is a different entry', async () => {
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  const cache = fakeCaches();
  vi.stubGlobal('caches', cache.api);

  await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  const bumped = await loadModule(BASE + '/m.wasm', { cacheKey: 'v2' });
  assert.equal(bumped.cached, false, 'a new version must not serve the old bytes');
  assert.equal(fetchMock.mock.calls.length, 2);
  assert.equal(cache.store.size, 2);
});

/**
 * ⚠️ EVERY CACHE STEP IS BEST-EFFORT, AND THAT IS LOAD-BEARING. The Cache API is
 * unavailable on insecure origins other than localhost, and storage is evicted
 * under pressure at any time. A cache failure that propagated would take the
 * page down for a reason that has nothing to do with the guest.
 */
test('the page still loads when there is no Cache API at all', async () => {
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('caches', undefined);

  const r = await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  assert.equal(r.cached, false);
  assert.ok(r.module instanceof WebAssembly.Module);
});

test('the page still loads when opening the cache throws', async () => {
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('caches', fakeCaches({ openThrows: true }).api);

  const r = await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  assert.equal(r.cached, false);
  assert.ok(r.module instanceof WebAssembly.Module);
});

test('the page still loads when the cache refuses to store', async () => {
  // Quota exceeded is the ordinary case for a 120 MB artifact on a phone.
  const fetchMock = vi.fn(async () => wasmResponse());
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('caches', fakeCaches({ putRejects: true }).api);

  const r = await loadModule(BASE + '/m.wasm', { cacheKey: 'v1' });
  assert.ok(r.module instanceof WebAssembly.Module, 'a rejected put must not fail the load');
  assert.equal(r.cached, false);
});

test('an HTTP error is refused rather than compiled', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response('not found', { status: 404 })),
  );
  vi.stubGlobal('caches', undefined);
  // Without this, `compileStreaming` receives an HTML error page and reports a
  // magic-number failure, which reads as a corrupt artifact.
  await assert.rejects(loadModule(BASE + '/m.wasm'), /m\.wasm.*404/);
});

test('progress is reported without consuming the body being compiled', async () => {
  // ⚠️ THE BODY IS TEE'd for exactly this reason: buffering it to count bytes
  // would defeat `compileStreaming`, whose point on a 40 MB module is that
  // compilation overlaps the download.
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(WASM.subarray(0, 4));
      c.enqueue(WASM.subarray(4));
      c.close();
    },
  });
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(body, {
          headers: { 'content-type': 'application/wasm', 'content-length': String(WASM.length) },
        }),
    ),
  );
  vi.stubGlobal('caches', undefined);

  const seen: [number, number][] = [];
  const r = await loadModule(BASE + '/m.wasm', {
    onProgress: (got, total) => seen.push([got, total]),
  });

  assert.ok(r.module instanceof WebAssembly.Module, 'compilation must still succeed');
  await vi.waitFor(() => assert.ok(seen.length > 0, 'no progress was reported at all'));
  assert.equal(seen.at(-1)![0], WASM.length, 'the final count must be the whole body');
  assert.equal(seen.at(-1)![1], WASM.length, 'the total comes from content-length');
});
