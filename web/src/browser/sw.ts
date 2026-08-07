/**
 * The service-worker half of the guest host: turning a guest socket into an
 * origin.
 *
 * Requests under `PREFIX` are handed to the page hosting the guest, which writes
 * them into the guest's listening socket and reads the response back. Everything
 * else goes to the network untouched.
 *
 * ⚠️ THIS IS NOT A SEPARATE SCRIPT. It is part of `dist/raptormark.js`, the
 * single entry point that a window imports and a service worker registers, and
 * `installServiceWorker` runs only in the latter. Four things follow:
 *
 *   * **The bundle must be a MODULE service worker.** `register()` passes
 *     `{ type: 'module' }`; a classic worker cannot execute the ESM the page
 *     side needs. That sets a browser floor -- Chromium 91, Firefox 114,
 *     Safari 16.4 -- which a classic, separate worker would not have.
 *   * **The worker carries the whole page bundle**, ~65 KB rather than the ~3 KB
 *     this file needs, re-parsed every time the browser restarts a terminated
 *     worker. Nothing in the page graph runs at import time, so it is dead
 *     weight rather than a hazard, but it is on the request path.
 *   * **Listeners must be registered during INITIAL EVALUATION**, which is why
 *     `installServiceWorker` is called at the top level of the entry module and
 *     why nothing in the browser graph may use top-level `await`. A worker that
 *     adds its `fetch` listener asynchronously never receives one.
 *   * **The bundle lives under `dist/`, so its scope has to be widened.** A
 *     worker's maximum scope defaults to the directory it was served from;
 *     `internal/serve` sends `Service-Worker-Allowed: /` so this one can control
 *     the origin. Without that header it registers, activates, and intercepts
 *     nothing.
 *
 * The wire types are the payoff of sharing a program with the page side:
 * `serve.ts` already speaks `WireRequest`/`WireResponse`, and this file used to
 * restate that contract in comments with nothing checking that the two halves
 * agreed.
 */

import type { WireRequest, WireResponse } from './inbound.ts';
import type { ServiceWorkerScope } from './swscope.ts';

const PREFIX = '/_guest/';

/** What the page sends back for a request, or the error it failed with. */
type Answer = { id: number; res?: WireResponse; error?: string };

/**
 * The port to the page hosting the guest, if one has announced itself.
 *
 * ⚠️ THIS DOES NOT SURVIVE. A service worker is not a long-lived process: the
 * browser terminates an idle one and starts a fresh copy for the next event, and
 * the fresh copy has none of this state. The page announced its port to a worker
 * that no longer exists.
 *
 * That is why a missing host asks the clients to announce again rather than
 * failing. Treating `host === null` as "no page is open" is the assumption that
 * breaks -- intermittently, under load, and looking exactly like the guest
 * having crashed.
 *
 * Module scope rather than a closure: there is at most one service-worker global
 * per script evaluation, and hanging it off the module makes the lifetime claim
 * above the obvious reading.
 */
let host: MessagePort | null = null;
let nextId = 1;
const pending = new Map<
  number,
  { resolve: (r: WireResponse) => void; reject: (e: Error) => void }
>();
/** Resolvers waiting for a re-announced host. */
let hostWaiters: ((p: MessagePort | null) => void)[] = [];

/**
 * Registers this worker's event listeners. Call once, synchronously, from the
 * top level of the entry module, and only when `serviceWorkerScope()` returned a
 * scope.
 */
export function installServiceWorker(sw: ServiceWorkerScope): void {
  sw.addEventListener('install', () => {
    // ⚠️ WITHOUT `skipWaiting` A NEW WORKER SITS IN `waiting` until every client
    // using the old one goes away, so a reload during development silently keeps
    // serving the previous version.
    void sw.skipWaiting();
  });

  sw.addEventListener('activate', (event) => {
    // ⚠️ A FRESH WORKER CONTROLS NOTHING. Clients loaded before it activated
    // stay uncontrolled unless it claims them, so the page that just registered
    // it would find its own iframe request went straight to the network.
    event.waitUntil(sw.clients.claim());
  });

  sw.addEventListener('message', (event: MessageEvent) => {
    const data = event.data as { type?: string } | null;
    if (data && data.type === 'raptormark-forget-host') {
      // ⚠️ A TEST HOOK, and it earns its place. The condition it creates -- this
      // worker restarted and lost the port a page handed it -- happens on its
      // own schedule, decided by the browser, and cannot be provoked from a
      // test. The recovery path would otherwise be shipped unexercised, which is
      // exactly how it came to be missing in the first place.
      host = null;
      return;
    }
    if (!data || data.type !== 'raptormark-host') return;
    const port = event.ports[0];
    if (!port) return;
    host = port;
    for (const resolve of hostWaiters) resolve(port);
    hostWaiters = [];
    port.onmessage = (ev: MessageEvent) => {
      const { id, res, error } = ev.data as Answer;
      const waiter = pending.get(id);
      if (!waiter) return;
      pending.delete(id);
      if (error !== undefined) waiter.reject(new Error(error));
      else if (res !== undefined) waiter.resolve(res);
      // Neither field set is a protocol violation by the page, not something to
      // paper over: leaving the waiter deleted and unsettled lets the request
      // hit the caller's own timeout, which is the honest outcome.
    };
  });

  sw.addEventListener('fetch', (event) => {
    const url = new URL(event.request.url);
    if (url.origin !== sw.location.origin || !url.pathname.startsWith(PREFIX)) return;
    event.respondWith(fromGuest(sw, event.request));
  });
}

async function fromGuest(sw: ServiceWorkerScope, request: Request): Promise<Response> {
  const target = await ensureHost(sw);
  if (!target) {
    // ⚠️ SAY WHAT HAPPENED. Reached only when no window client answered, which
    // means the page really is gone. A bare network error would look like the
    // guest crashed.
    return new Response(
      'No raptormark host is connected to this service worker.\n' +
        'The page that runs the guest must be open and must have announced itself.\n',
      { status: 503, headers: { 'content-type': 'text/plain; charset=utf-8' } },
    );
  }

  let body: Uint8Array | undefined;
  if (request.method !== 'GET' && request.method !== 'HEAD') {
    body = new Uint8Array(await request.arrayBuffer());
  }
  const req: WireRequest = {
    method: request.method,
    url: request.url,
    headers: [...request.headers],
    body,
  };

  const id = nextId++;
  const answer = new Promise<WireResponse>((resolve, reject) =>
    pending.set(id, { resolve, reject }),
  );
  target.postMessage({ id, req });

  try {
    const res = await answer;
    // ⚠️ THE CAST IS A VARIANCE ARTEFACT, NOT A LIE ABOUT THE VALUE. Since TS
    // 5.7 `Uint8Array` is generic over its backing buffer, and `BodyInit` wants
    // `ArrayBufferView<ArrayBuffer>` while a value that crossed `postMessage` is
    // the wider `Uint8Array<ArrayBufferLike>`. Every such array here is
    // ArrayBuffer-backed -- a SharedArrayBuffer cannot be structured-cloned into
    // a service worker -- so the runtime value is exactly what `Response` wants.
    return new Response(res.body as BodyInit, {
      status: res.status,
      statusText: res.statusText,
      headers: res.headers,
    });
  } catch (err) {
    pending.delete(id);
    return new Response(`raptormark guest failed to answer: ${err}\n`, {
      status: 502,
      headers: { 'content-type': 'text/plain; charset=utf-8' },
    });
  }
}

/**
 * Returns the host port, asking the open pages to re-announce if this worker has
 * been restarted and lost it.
 */
async function ensureHost(sw: ServiceWorkerScope, timeoutMs = 5000): Promise<MessagePort | null> {
  if (host) return host;

  // `includeUncontrolled` because a client that loaded before this worker took
  // control still hosts the guest and can still answer.
  const clients = await sw.clients.matchAll({ type: 'window', includeUncontrolled: true });
  if (clients.length === 0) return null;

  const waited = new Promise<MessagePort | null>((resolve) => {
    hostWaiters.push(resolve);
    setTimeout(() => resolve(null), timeoutMs);
  });
  for (const c of clients) c.postMessage({ type: 'raptormark-need-host' });
  return waited;
}
