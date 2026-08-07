/**
 * Getting a raptormark module into a browser.
 *
 * The artifacts are large -- lifting expands roughly `wasm ~= 6.89 x .text +
 * 2.37 MB`, so 17 MB is a small one and 120 MB is ordinary -- which makes how
 * they are fetched and compiled the difference between a usable page and an
 * unusable one.
 *
 * Nothing here is raptormark-specific cleverness; it is the standard set of
 * browser footguns, each of which costs an hour to rediscover.
 */

/**
 * Compiles a module, reusing previously fetched BYTES when it can.
 *
 * ⚠️ `compileStreaming` THROWS unless the response's `Content-Type` is exactly
 * `application/wasm`. A static server that serves `.wasm` as
 * `application/octet-stream` produces an "Incorrect response MIME type" error
 * that says nothing about the server. `internal/serve` sets it; any other host
 * must too.
 *
 * # ⚠️ THE COMPILED MODULE CANNOT BE CACHED. Measured, not assumed.
 *
 * An earlier version of this file stored the `WebAssembly.Module` itself in
 * IndexedDB, on the widely-repeated claim that a Module is structured-cloneable
 * and storable -- and that a warm load therefore skips both the download AND the
 * compile. **Chromium refuses it:**
 *
 *     DataCloneError: Failed to execute 'put' on 'IDBObjectStore':
 *     A WebAssembly.Module can not be serialized for storage.
 *
 * That capability existed and was removed. The claim survives in a great deal of
 * advice, including this file's own first draft, and it fails SILENTLY -- the
 * write rejects, the read misses, and the page simply recompiles every time
 * while appearing to have a cache. It was caught only because a test printed
 * hit-or-miss rather than asserting the page still worked.
 *
 * So this caches the BYTES, through the Cache API, which does work and removes
 * the download -- the dominant cost for a 120 MB artifact on any real link.
 * Compilation is left to the engine's own implicit code cache, which is keyed on
 * the response and is the only mechanism that can skip it.
 */
export async function loadModule(
  url: string,
  opts: { cacheKey?: string; onProgress?: (received: number, total: number) => void } = {},
): Promise<{ module: WebAssembly.Module; cached: boolean }> {
  const res = await fetchMaybeCached(url, opts.cacheKey);
  if (!res.response.ok) throw new Error(`fetching ${url}: HTTP ${res.response.status}`);

  let module: WebAssembly.Module;
  if (opts.onProgress && res.response.body) {
    // ⚠️ `tee()` so progress does NOT cost streaming. Buffering the whole body
    // to count bytes would defeat `compileStreaming`, which is the point of
    // using it on a module this size: compilation overlaps the download.
    const [a, b] = res.response.body.tee();
    const total = Number(res.response.headers.get('content-length') ?? 0);
    void count(b, total, opts.onProgress);
    module = await WebAssembly.compileStreaming(
      new Response(a, { headers: { 'content-type': 'application/wasm' } }),
    );
  } else {
    module = await WebAssembly.compileStreaming(res.response);
  }
  return { module, cached: res.cached };
}

const CACHE = 'raptormark-modules';

/**
 * Serves the module bytes from the Cache API when they are there, and stores
 * them when they are not.
 *
 * Keyed on a caller-supplied version, not the URL alone: a stale artifact is the
 * worst failure available here, because it is silent and looks exactly like a
 * rebuild that did not take.
 *
 * Every step is best-effort. The Cache API is unavailable on insecure origins
 * other than localhost, and storage can be evicted under pressure at any time,
 * so a miss must always fall back to the network rather than fail.
 */
async function fetchMaybeCached(
  url: string,
  key?: string,
): Promise<{ response: Response; cached: boolean }> {
  if (!key || typeof caches === 'undefined') {
    return { response: await fetch(url), cached: false };
  }
  const req = new Request(`${url}?__v=${encodeURIComponent(key)}`);
  try {
    const cache = await caches.open(CACHE);
    const hit = await cache.match(req);
    if (hit) return { response: hit, cached: true };
    const fresh = await fetch(url);
    if (fresh.ok) {
      // `put` consumes the body, so the copy is stored and the original is
      // returned -- the other way round re-downloads on every load.
      await cache.put(req, fresh.clone()).catch(() => {});
    }
    return { response: fresh, cached: false };
  } catch {
    return { response: await fetch(url), cached: false };
  }
}

async function count(
  stream: ReadableStream<Uint8Array>,
  total: number,
  onProgress: (received: number, total: number) => void,
): Promise<void> {
  const reader = stream.getReader();
  let received = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    received += value?.length ?? 0;
    onProgress(received, total);
  }
}

/**
 * Fetches the RAPTORFS sidecar.
 *
 * ⚠️ The bytes exist twice, transiently: once here and once inside linear
 * memory after `Rfs::parse` takes ownership. For a 100 MB Debian rootfs that is
 * ~200 MB peak, which matters on mobile. The caller should drop its reference as
 * soon as the guest has read it.
 *
 * There is no streaming alternative today: ecvisor reads the sidecar with a
 * single `std::fs::read` through a preopen, so the host has to be able to answer
 * the whole thing.
 */
export async function loadRootfs(url: string): Promise<Uint8Array> {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`fetching ${url}: HTTP ${res.status}`);
  return new Uint8Array(await res.arrayBuffer());
}
