import { SOCKET_HANDLE_BASE, run } from '../host.ts';
import type { RunResult } from '../host.ts';
import { loadModule, loadRootfs } from './load.ts';
import { RelaySockets } from './relay.ts';
import { installServiceWorker } from './sw.ts';
import { serviceWorkerScope } from './swscope.ts';

// Re-exported because `run.ts` is the bundle's single entry point: esbuild emits
// only what is reachable from here, and `inbound.html` needs these.
export { installGuestServiceWorker, serveGuestInBrowser } from './serve.ts';
export type { GuestServer, ServeOptions } from './serve.ts';
export { InboundSockets, parseResponse, serializeRequest } from './inbound.ts';
export type { WireRequest, WireResponse } from './inbound.ts';

/**
 * ⚠️ ONE ARTIFACT, TWO ROLES. `raptormark.js` is both the module a page imports
 * and the script a service worker registers, and this is the fork between them:
 * in a window `serviceWorkerScope()` is null and nothing below runs, so the
 * exports above are all the file does.
 *
 * ⚠️ IT MUST RUN AT MODULE TOP LEVEL, SYNCHRONOUSLY. A service worker only
 * receives events whose listeners were added during initial evaluation, so
 * deferring this -- to a promise, a callback, or an exported `init()` the page
 * calls -- produces a worker that installs, activates, controls the page and
 * then silently ignores every request. Nothing in this module graph may use
 * top-level `await` for the same reason.
 *
 * ⚠️ AND THE BUNDLE'S LOCATION IS PART OF THE CONTRACT. A service worker's
 * maximum scope defaults to the directory its script was served from, so this
 * one -- `dist/raptormark.js` -- would be confined to `/dist/*` and would never
 * see `/_guest/*`. It reaches the whole origin only because `internal/serve`
 * sends `Service-Worker-Allowed: /` with the script. Serving this bundle from
 * anything that does not send that header gives a worker that registers,
 * activates, and intercepts nothing.
 */
const swScope = serviceWorkerScope();
if (swScope) installServiceWorker(swScope);

/**
 * Running a raptormark guest in a browser.
 *
 * Almost everything here is shared with the Node host: the WASI shim, the
 * socket ABI, the DNS tap's host half, the re-entrant driver. What differs is
 * only where the bytes come from, where output goes, and which transport is in
 * play -- which is what the shared core was for.
 *
 * ⚠️ THE GUEST MUST NEVER BLOCK THE HOST. `Atomics.wait` is illegal on a main
 * thread, and even in a worker a blocking host stalls the event loop that
 * delivers the readiness the guest is waiting for. So the module has to be one
 * built with `link-all --profile browser`, whose backend declines to wait and
 * whose scheduler hands control back instead. Handing this a default-profile
 * module produces a page that freezes on the first socket wait.
 */

export interface BrowserRunOptions {
  /** URL of a `--profile browser` module. */
  moduleUrl: string;
  /** URL of the RAPTORFS sidecar, if the guest needs a filesystem. */
  rootfsUrl?: string;
  /** Cache key for the compiled module. Change it when the artifact changes. */
  version?: string;
  /** `wss://` relay for outbound TCP. Without one the guest has no network. */
  relayUrl?: string;
  args?: string[];
  env?: Record<string, string>;
  onOutput?: (fd: number, text: string) => void;
  onProgress?: (received: number, total: number) => void;
  /**
   * Legs per slice.
   *
   * ⚠️ A LEG IS UNINTERRUPTIBLE. It runs lifted code to the next suspension
   * point, and a syscall boundary is the only place guest state is wholly in
   * `State` and the arena -- so this bounds how often the page can breathe, not
   * how long a single leg may take. A guest that computes forever with no
   * syscalls still holds the thread; that limit is real and unchanged.
   */
  legsPerSlice?: number;
}

export async function runInBrowser(opts: BrowserRunOptions): Promise<RunResult> {
  const { module, cached } = await loadModule(opts.moduleUrl, {
    cacheKey: opts.version ? `${opts.moduleUrl}#${opts.version}` : undefined,
    onProgress: opts.onProgress,
  });
  opts.onOutput?.(2, `[host] module ${cached ? 'from cache' : 'compiled'}\n`);

  let rootfs: Uint8Array | undefined;
  if (opts.rootfsUrl) rootfs = await loadRootfs(opts.rootfsUrl);

  // Readiness is queued here and flushed by the driver between slices --
  // never from inside an import, where `ecv_run_slice` is on the stack.
  const queued: Array<[number, number]> = [];
  let relay: RelaySockets | undefined;
  if (opts.relayUrl) {
    relay = new RelaySockets({
      url: opts.relayUrl,
      handleBase: SOCKET_HANDLE_BASE,
      notify: (h, ev) => queued.push([h, ev]),
    });
    await relay.start();
  }

  const env = { ...opts.env };
  // The browser backend never blocks; this only matters if a loopback-profile
  // module is used instead, where it selects the same shape.
  env['RAPTORMARK_ECV_NONBLOCK'] = '1';
  if (opts.rootfsUrl) env['RAPTORMARK_ROOTFS'] = '/rootfs.img';

  try {
    return await run({
      module,
      rootfs,
      rootfsName: 'rootfs.img',
      args: opts.args,
      env,
      socketBackend: relay,
      readyQueue: queued,
      netV1: true,
      reentrant: true,
      legsPerSlice: opts.legsPerSlice ?? 20_000,
      onOutput: opts.onOutput,
    });
  } finally {
    relay?.stop();
  }
}
