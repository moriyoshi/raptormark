/**
 * Hosting a guest that SERVES rather than one that finishes.
 *
 * Every other browser fixture runs to completion, so the page can `await` the
 * result. A server does not: it parks in `accept` forever, and the host has to
 * keep driving it while simultaneously answering requests against it. That one
 * difference is what this file exists for.
 */

import { SOCKET_HANDLE_BASE, run } from '../host.ts';
import { CompositeSockets } from './composite.ts';
import { InboundSockets, parseResponse, serializeRequest } from './inbound.ts';
import type { WireRequest, WireResponse } from './inbound.ts';
import { loadModule, loadRootfs } from './load.ts';
import { RelaySockets } from './relay.ts';

export interface ServeOptions {
  /** URL of a `--profile browser` module whose guest is a server. */
  moduleUrl: string;
  /**
   * URL of the RAPTORFS sidecar.
   *
   * ⚠️ A REAL SERVER NEEDS ONE. nginx reads its config off the filesystem, and
   * without a sidecar there is no filesystem at all -- it exits before it
   * listens, which surfaces here as "the guest never listened".
   *
   * ⚠️ The sidecar's BOOT RECORD supplies argv and OVERRIDES `args` below. For
   * a real image that is what you want (the config path travels with the
   * sidecar); it has silently replaced a host-passed port before now.
   */
  rootfsUrl?: string;
  version?: string;
  /**
   * The port the guest listens on, passed to it as `argv[1]`.
   *
   * It is not a real port -- nothing binds on the host -- but the guest binds
   * it and the host has to name the same number to reach the listener.
   */
  port?: number;
  /**
   * `wss://` relay for OUTBOUND connections the guest makes.
   *
   * With one, the guest can both serve and dial -- a proxy, or a server that
   * calls an upstream. Without one, `connect` is refused, which is honest: a
   * browser has no other way out.
   */
  relayUrl?: string;
  args?: string[];
  env?: Record<string, string>;
  onOutput?: (fd: number, text: string) => void;
  onProgress?: (received: number, total: number) => void;
  legsPerSlice?: number;
  /** How long to wait for the guest to reach `listen`. */
  bootTimeoutMs?: number;
  /** How long one request may take before it is failed. */
  requestTimeoutMs?: number;
}

export interface GuestServer {
  /** Answers one request from the guest. */
  handle(req: WireRequest): Promise<WireResponse>;
  /**
   * Posts a signal to a guest process, as `kill(2)` would from outside.
   *
   * ⚠️ Queued and delivered BETWEEN slices, never during one -- a slice holds
   * the process table `&mut`. It therefore takes effect on the guest's next
   * scheduling pass rather than immediately.
   */
  signal(pid: number, sig: number): void;
  /** The port the guest is listening on. */
  readonly port: number;
  /**
   * Settles only if the guest EXITS, which a server should not do.
   *
   * Exposed so a caller can surface that as a failure rather than as requests
   * that quietly begin timing out.
   */
  readonly exited: Promise<number>;
}

export async function serveGuestInBrowser(opts: ServeOptions): Promise<GuestServer> {
  const port = opts.port ?? 8080;
  const { module, cached } = await loadModule(opts.moduleUrl, {
    cacheKey: opts.version ? `${opts.moduleUrl}#${opts.version}` : undefined,
    onProgress: opts.onProgress,
  });
  opts.onOutput?.(2, `[host] module ${cached ? 'from cache' : 'compiled'}\n`);

  let rootfs: Uint8Array | undefined;
  if (opts.rootfsUrl) rootfs = await loadRootfs(opts.rootfsUrl);

  const queued: Array<[number, number]> = [];
  const signals: Array<[number, number]> = [];
  // ⚠️ ALWAYS COMPOSITE, even with no relay. The socket a guest creates does not
  // say whether it will listen or dial, so the routing has to exist before the
  // question can be answered -- and with no relay the composite refuses
  // `connect` explicitly, which is a better answer than a dial into a backend
  // that was only ever built to accept.
  //
  // ⚠️ THE BASES ARE NOT ARBITRARY, which this comment used to claim. A relay
  // handle IS the stream id on the wire and that id is u16, so a base of
  // 2_000_000 silently became 33920 and every reply was dropped. The composite
  // does map per side, so collisions between the two are handled
  // (`composite.test.ts` asserts that) -- but each backend still has its own
  // constraints, and the relay's is a hard ceiling.
  const sockets = new CompositeSockets<InboundSockets, RelaySockets>({
    handleBase: SOCKET_HANDLE_BASE,
    notify: (h, ev) => queued.push([h, ev]),
    listen: (notify) => new InboundSockets({ handleBase: 1_000_000, notify }),
    dial: opts.relayUrl
      ? (notify) => new RelaySockets({ url: opts.relayUrl!, handleBase: 1, notify })
      : undefined,
  });
  const inbound = sockets.listener;
  if (sockets.dialer) await sockets.dialer.start();

  const env = { ...opts.env };
  env['RAPTORMARK_ECV_NONBLOCK'] = '1';
  if (opts.rootfsUrl) env['RAPTORMARK_ROOTFS'] = '/rootfs.img';

  // ⚠️ NOT AWAITED, AND THAT IS THE POINT. `run` drives the guest until it
  // exits, and this guest never does -- so awaiting here would hang before a
  // single request could be served. The promise is kept only to notice an
  // unexpected exit.
  const exited = run({
    module,
    rootfs,
    rootfsName: 'rootfs.img',
    args: opts.args ?? ['guest', String(port)],
    env,
    socketBackend: sockets,
    readyQueue: queued,
    signalQueue: signals,
    netV1: true,
    reentrant: true,
    legsPerSlice: opts.legsPerSlice ?? 20_000,
    onOutput: opts.onOutput,
  }).then((r) => r.exitCode);
  // An unobserved rejection here would surface as an unhandled promise error
  // with no connection to the request that failed because of it.
  exited.catch(() => {});

  // ⚠️ WAIT FOR `listen`, NOT FOR THE MODULE TO INSTANTIATE. Delivering a
  // request before the guest has a listener rejects with "nothing is listening",
  // which reads as a routing bug rather than as a race with boot.
  await until(
    () => inbound.listeningOn(port),
    opts.bootTimeoutMs ?? 30_000,
    `the guest never listened on port ${port}`,
  );

  const authority = `127.0.0.1:${port}`;
  const timeout = opts.requestTimeoutMs ?? 15_000;

  return {
    port,
    exited,
    signal(pid: number, sig: number): void {
      signals.push([pid, sig]);
    },
    async handle(req: WireRequest): Promise<WireResponse> {
      const wire = serializeRequest(req, authority);
      const bytes = await deadline(
        inbound.deliver(port, wire, req.method),
        timeout,
        `the guest did not answer ${req.method} ${req.url} within ${timeout} ms`,
      );
      return parseResponse(bytes);
    },
  };
}

/**
 * Registers the service worker and answers its requests from `server`.
 *
 * ⚠️ REGISTERING IS NOT ENOUGH -- the worker must CONTROL this page, and on a
 * first load it does not. A newly activated worker takes no existing client
 * unless it calls `clients.claim()`, so a page that registers and immediately
 * navigates an iframe finds its request went straight to the network. This
 * waits for `controller` to exist.
 *
 * ⚠️ A SERVICE WORKER NEEDS A SECURE CONTEXT. `localhost` and `127.0.0.1`
 * qualify without TLS; anything else needs https, and registration fails with a
 * message that names neither the origin nor the reason.
 */
export async function installGuestServiceWorker(
  server: GuestServer,
  opts: { swUrl?: string; scope?: string; onError?: (e: unknown) => void } = {},
): Promise<ServiceWorker> {
  if (!('serviceWorker' in navigator)) {
    throw new Error('no service worker support; this needs a secure context (localhost or https)');
  }
  // ⚠️ THE WORKER SCRIPT IS THIS SAME BUNDLE. `dist/raptormark.js` forks on
  // `serviceWorkerScope()` at module top level, so registering it here is what
  // makes the file's service-worker half run at all.
  //
  // ⚠️ `type: 'module'` IS NOT OPTIONAL. The bundle is ESM, and a CLASSIC worker
  // cannot execute `import` -- registration fails outright. The cost is a
  // browser floor: module service workers need Chromium 91, Firefox 114 or
  // Safari 16.4. A separate classic worker script would not have that floor, and
  // that is the trade made when the two entry points were merged.
  //
  // ⚠️ THE SCOPE IS WIDER THAN THE SCRIPT'S DIRECTORY, and that only works
  // because `internal/serve` sends `Service-Worker-Allowed: /` with the script.
  // A worker's maximum scope otherwise defaults to where it was served from, so
  // this one would be confined to `/dist/*` and would never see `/_guest/*`.
  // The header travels on the SCRIPT response, so a different server hosting
  // this bundle must send it too -- registration otherwise fails with a message
  // that names the scope and not the missing header.
  await navigator.serviceWorker.register(opts.swUrl ?? './dist/raptormark.js', {
    type: 'module',
    scope: opts.scope ?? './',
  });
  await navigator.serviceWorker.ready;

  const controller =
    navigator.serviceWorker.controller ??
    (await new Promise<ServiceWorker>((resolve, reject) => {
      const t = setTimeout(
        () => reject(new Error('the service worker never took control')),
        10_000,
      );
      navigator.serviceWorker.addEventListener('controllerchange', () => {
        const c = navigator.serviceWorker.controller;
        if (c) {
          clearTimeout(t);
          resolve(c);
        }
      });
    }));

  // ⚠️ A DEDICATED PORT, not `navigator.serviceWorker.onmessage`. The worker has
  // to reach THIS page specifically -- the one hosting the guest -- and
  // `clientId` on a navigation request refers to a client that does not exist
  // yet. Handing it a port sidesteps client resolution entirely.
  const announce = (): void => {
    const channel = new MessageChannel();
    channel.port1.onmessage = async (ev: MessageEvent) => {
      const { id, req } = ev.data as { id: number; req: WireRequest };
      try {
        const res = await server.handle(req);
        channel.port1.postMessage({ id, res });
      } catch (err) {
        opts.onError?.(err);
        channel.port1.postMessage({ id, error: String(err) });
      }
    };
    (navigator.serviceWorker.controller ?? controller).postMessage({ type: 'raptormark-host' }, [
      channel.port2,
    ]);
  };

  // ⚠️ ANNOUNCING ONCE IS NOT ENOUGH. A service worker is not a long-lived
  // process: the browser terminates an idle one and starts a fresh copy for the
  // next event, and that copy has never heard of this page. The port is gone
  // with the worker that held it, so it has to be handed over again on demand.
  //
  // The symptom without this is intermittent and thoroughly misleading -- a 503
  // saying no host is connected, arriving from a page that is right there and
  // running, and only under enough load to make the worker look idle.
  navigator.serviceWorker.addEventListener('message', (ev: MessageEvent) => {
    if ((ev.data as { type?: string } | null)?.type === 'raptormark-need-host') announce();
  });
  announce();
  return controller;
}

async function until(cond: () => boolean, ms: number, what: string): Promise<void> {
  const end = Date.now() + ms;
  for (;;) {
    if (cond()) return;
    if (Date.now() > end) throw new Error(`${what} (waited ${ms} ms)`);
    await new Promise((r) => setTimeout(r, 5));
  }
}

function deadline<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(what)), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}
