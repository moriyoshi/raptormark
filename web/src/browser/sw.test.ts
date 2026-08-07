import assert from 'node:assert/strict';
import { beforeEach, test, vi } from 'vitest';

import type { ServiceWorkerScope, WindowClientLike } from './swscope.ts';
import type { WireRequest, WireResponse } from './inbound.ts';

/**
 * The service worker, without a browser.
 *
 * ⚠️ THIS LOGIC WAS REACHABLE ONLY THROUGH CHROMIUM until now: eight Playwright
 * specs exercised it end to end, at minutes each, needing a built bundle, a
 * lifted guest and a server. None of them could state what the worker does with
 * a request it should ignore, or with a page that answers with an error, because
 * getting the guest into those states is harder than the behaviour being tested.
 *
 * `installServiceWorker(sw)` takes its global as a PARAMETER rather than reading
 * `self`, which is what makes this possible at all -- the fake below is thirty
 * lines and needs no DOM.
 */

type Listeners = Map<string, (event: unknown) => void>;

/** A `ServiceWorkerScope` that records what the worker did to it. */
function fakeScope(clients: WindowClientLike[] = []) {
  const listeners: Listeners = new Map();
  const calls = { skipWaiting: 0, claim: 0, waitUntil: 0 };
  const scope: ServiceWorkerScope = {
    clients: {
      claim: async () => {
        calls.claim++;
      },
      matchAll: async () => clients,
    },
    location: { origin: 'https://host.test' },
    skipWaiting: async () => {
      calls.skipWaiting++;
    },
    // One listener per type is all this worker registers.
    addEventListener: ((type: string, listener: (event: unknown) => void) => {
      listeners.set(type, listener);
    }) as ServiceWorkerScope['addEventListener'],
  };
  return { scope, listeners, calls };
}

/** A window client that records the messages the worker sends it. */
function fakeClient(): WindowClientLike & { messages: unknown[] } {
  const messages: unknown[] = [];
  return { messages, postMessage: (m: unknown) => messages.push(m) };
}

/** A fetch event whose `respondWith` is captured rather than dispatched. */
function fetchEvent(url: string, init?: RequestInit) {
  let answered: Promise<Response> | undefined;
  return {
    event: {
      request: new Request(url, init),
      respondWith(r: Response | Promise<Response>) {
        answered = Promise.resolve(r);
      },
    },
    // undefined means the worker declined the request and let it hit the
    // network, which is a different outcome from answering with an error.
    get answered() {
      return answered;
    },
  };
}

/**
 * ⚠️ A FRESH MODULE PER TEST. `sw.ts` keeps `host`, `pending`, `nextId` and
 * `hostWaiters` at module scope -- deliberately, because a service worker has
 * one global per evaluation -- so without resetting, a test that announces a
 * host leaks it into every test after it, and the 503 case would pass or fail
 * depending on file order.
 */
async function freshWorker() {
  vi.resetModules();
  return (await import('./sw.ts')).installServiceWorker;
}

beforeEach(() => {
  vi.resetModules();
});

test('install skips waiting and activate claims the clients', async () => {
  const install = await freshWorker();
  const { scope, listeners, calls } = fakeScope();
  install(scope);

  listeners.get('install')!({});
  assert.equal(calls.skipWaiting, 1, 'a new worker must not sit in `waiting`');

  const waited: Promise<unknown>[] = [];
  listeners.get('activate')!({ waitUntil: (p: Promise<unknown>) => waited.push(p) });
  await Promise.all(waited);
  assert.equal(calls.claim, 1, 'a fresh worker controls nothing until it claims');
});

/**
 * ⚠️ WHAT THE WORKER DECLINES IS AS IMPORTANT AS WHAT IT ANSWERS. A worker that
 * called `respondWith` for everything would route the page's own HTML, its
 * bundle and its wasm through a guest that serves none of them.
 */
test('requests outside the prefix or the origin are left to the network', async () => {
  const install = await freshWorker();
  const { scope, listeners } = fakeScope();
  install(scope);
  const onFetch = listeners.get('fetch')!;

  const notPrefixed = fetchEvent('https://host.test/index.html');
  onFetch(notPrefixed.event);
  assert.equal(notPrefixed.answered, undefined, 'only /_guest/ belongs to the guest');

  const crossOrigin = fetchEvent('https://elsewhere.test/_guest/x');
  onFetch(crossOrigin.event);
  assert.equal(crossOrigin.answered, undefined, 'another origin is not ours to answer');

  const ours = fetchEvent('https://host.test/_guest/x');
  onFetch(ours.event);
  assert.notEqual(ours.answered, undefined, 'a /_guest/ request must be intercepted');
});

test('with no page open at all, the answer is a 503 that says so', async () => {
  const install = await freshWorker();
  // No clients: the page really is gone, as opposed to merely not announced.
  const { scope, listeners } = fakeScope([]);
  install(scope);

  const f = fetchEvent('https://host.test/_guest/x');
  listeners.get('fetch')!(f.event);
  const res = await f.answered!;
  assert.equal(res.status, 503);
  assert.match(await res.text(), /No raptormark host is connected/);
});

/** Announces a host port and returns the page's end of the channel. */
function announce(listeners: Listeners) {
  const channel = new MessageChannel();
  listeners.get('message')!({
    data: { type: 'raptormark-host' },
    ports: [channel.port2],
  });
  channel.port1.start();
  return channel.port1;
}

test('a request is serialized to the page and its answer becomes the Response', async () => {
  const install = await freshWorker();
  const { scope, listeners } = fakeScope();
  install(scope);
  const page = announce(listeners);

  const seen: WireRequest[] = [];
  page.onmessage = (ev: MessageEvent) => {
    const { id, req } = ev.data as { id: number; req: WireRequest };
    seen.push(req);
    const res: WireResponse = {
      status: 201,
      statusText: 'Created',
      headers: [['x-guest', 'yes']],
      body: new TextEncoder().encode('hi'),
    };
    page.postMessage({ id, res });
  };

  const f = fetchEvent('https://host.test/_guest/a?q=1', {
    method: 'POST',
    body: 'payload',
    headers: { 'x-req': '1' },
  });
  listeners.get('fetch')!(f.event);
  const res = await f.answered!;

  assert.equal(res.status, 201);
  assert.equal(res.statusText, 'Created');
  assert.equal(res.headers.get('x-guest'), 'yes');
  assert.equal(await res.text(), 'hi');

  assert.equal(seen.length, 1);
  assert.equal(seen[0]!.method, 'POST');
  assert.equal(seen[0]!.url, 'https://host.test/_guest/a?q=1');
  // ⚠️ The BODY has to cross too. A worker that forwarded only method and URL
  // would serve every GET correctly and silently drop every POST.
  assert.equal(new TextDecoder().decode(seen[0]!.body), 'payload');
  assert.ok(
    seen[0]!.headers.some(([k, v]) => k.toLowerCase() === 'x-req' && v === '1'),
    'request headers must reach the guest',
  );
});

test('a GET carries no body rather than an empty one', async () => {
  const install = await freshWorker();
  const { scope, listeners } = fakeScope();
  install(scope);
  const page = announce(listeners);

  const seen: WireRequest[] = [];
  page.onmessage = (ev: MessageEvent) => {
    const { id, req } = ev.data as { id: number; req: WireRequest };
    seen.push(req);
    page.postMessage({
      id,
      res: { status: 200, statusText: 'OK', headers: [], body: new Uint8Array(0) },
    });
  };

  const f = fetchEvent('https://host.test/_guest/g');
  listeners.get('fetch')!(f.event);
  await f.answered!;
  assert.equal(seen[0]!.body, undefined);
});

test('an error from the page becomes a 502 naming it, not a dead connection', async () => {
  const install = await freshWorker();
  const { scope, listeners } = fakeScope();
  install(scope);
  const page = announce(listeners);

  page.onmessage = (ev: MessageEvent) => {
    const { id } = ev.data as { id: number };
    page.postMessage({ id, error: 'the guest did not answer in time' });
  };

  const f = fetchEvent('https://host.test/_guest/x');
  listeners.get('fetch')!(f.event);
  const res = await f.answered!;
  assert.equal(res.status, 502);
  assert.match(await res.text(), /did not answer in time/);
});

test('concurrent requests are correlated by id, not by arrival order', async () => {
  const install = await freshWorker();
  const { scope, listeners } = fakeScope();
  install(scope);
  const page = announce(listeners);

  // ⚠️ ANSWERED IN REVERSE. This is the assertion that matters: a worker that
  // resolved the oldest pending request would pass every sequential test and
  // cross the replies here. It is the unit-level twin of `nginxconc.spec.ts`,
  // which needs a browser and 25 real sockets to state the same property.
  const queue: { id: number; path: string }[] = [];
  page.onmessage = (ev: MessageEvent) => {
    const { id, req } = ev.data as { id: number; req: WireRequest };
    queue.push({ id, path: new URL(req.url).pathname });
    if (queue.length === 3) {
      for (const q of [...queue].reverse()) {
        page.postMessage({
          id: q.id,
          res: {
            status: 200,
            statusText: 'OK',
            headers: [],
            body: new TextEncoder().encode(q.path),
          },
        });
      }
    }
  };

  const onFetch = listeners.get('fetch')!;
  const events = ['/_guest/one', '/_guest/two', '/_guest/three'].map((p) => {
    const f = fetchEvent('https://host.test' + p);
    onFetch(f.event);
    return { path: p, f };
  });

  for (const { path, f } of events) {
    assert.equal(await (await f.answered!).text(), path, `${path} got another request's answer`);
  }
});

/**
 * ⚠️ THE RECOVERY PATH THAT WENT MISSING ONCE. A service worker's globals do not
 * survive termination, so a restarted worker has lost the port the page gave it.
 * Treating that as "no page is open" produced an intermittent 503 from a page
 * that was open the whole time.
 */
test('a worker that lost its port asks the open pages to re-announce', async () => {
  const install = await freshWorker();
  const client = fakeClient();
  const { scope, listeners } = fakeScope([client]);
  install(scope);

  const f = fetchEvent('https://host.test/_guest/x');
  listeners.get('fetch')!(f.event);

  // The worker has no host, but a window client exists, so it must ask rather
  // than give up.
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(client.messages, [{ type: 'raptormark-need-host' }]);

  // The page answers by handing over a fresh port; the request that was already
  // in flight must then complete.
  const page = announce(listeners);
  page.onmessage = (ev: MessageEvent) => {
    const { id } = ev.data as { id: number };
    page.postMessage({
      id,
      res: {
        status: 200,
        statusText: 'OK',
        headers: [],
        body: new TextEncoder().encode('recovered'),
      },
    });
  };
  assert.equal(await (await f.answered!).text(), 'recovered');
});

test('the forget-host hook drops the port, so the recovery path can be tested', async () => {
  const install = await freshWorker();
  const client = fakeClient();
  const { scope, listeners } = fakeScope([client]);
  install(scope);

  const page = announce(listeners);
  page.onmessage = (ev: MessageEvent) => {
    const { id } = ev.data as { id: number };
    page.postMessage({
      id,
      res: { status: 200, statusText: 'OK', headers: [], body: new Uint8Array(0) },
    });
  };

  const first = fetchEvent('https://host.test/_guest/a');
  listeners.get('fetch')!(first.event);
  assert.equal((await first.answered!).status, 200);
  assert.deepEqual(client.messages, [], 'a worker holding a port must not ask for another');

  listeners.get('message')!({ data: { type: 'raptormark-forget-host' }, ports: [] });

  const second = fetchEvent('https://host.test/_guest/b');
  listeners.get('fetch')!(second.event);
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(
    client.messages,
    [{ type: 'raptormark-need-host' }],
    'after forgetting, the next request must re-announce',
  );
});

test('an unrelated message is ignored rather than mistaken for an announcement', async () => {
  const install = await freshWorker();
  const { scope, listeners } = fakeScope([]);
  install(scope);

  const channel = new MessageChannel();
  listeners.get('message')!({ data: { type: 'something-else' }, ports: [channel.port2] });
  listeners.get('message')!({ data: null, ports: [] });

  // Still hostless: a 503 rather than a request posted into a port the worker
  // was never given.
  const f = fetchEvent('https://host.test/_guest/x');
  listeners.get('fetch')!(f.event);
  assert.equal((await f.answered!).status, 503);
});
