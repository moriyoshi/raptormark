/**
 * The minimal service-worker global surface, hand-declared.
 *
 * ⚠️ WHY THESE ARE NOT IMPORTED FROM A LIB. `ServiceWorkerGlobalScope`,
 * `FetchEvent`, `ExtendableEvent` and `Clients` live in TypeScript's
 * `WebWorker` lib, and `WebWorker` cannot be in the same program as `DOM`: they
 * declare the same names -- `self`, `fetch`, `location`, `Event` and dozens more
 * -- with different types, so a program containing both is a wall of duplicate
 * identifiers.
 *
 * Since `raptormark.js` is ONE entry point that runs in both a window and a
 * service worker, it needs the DOM lib, and so the service-worker half has to
 * bring its own types. Declaring the handful actually used is the honest version
 * of that; casting the global to `any` is the alternative and it silently gives
 * up every check on the code that answers real requests.
 *
 * These describe what this codebase uses, not the full specification. Widen them
 * when something new is needed -- do not reach for `any`.
 */

/** `install` / `activate`: may hold the worker alive for a promise. */
export interface ExtendableEventLike {
  waitUntil(f: Promise<unknown>): void;
}

/** `fetch`: may take over the response. */
export interface FetchEventLike {
  readonly request: Request;
  respondWith(r: Response | Promise<Response>): void;
}

/** A window under this worker's origin. Only `postMessage` is used. */
export interface WindowClientLike {
  postMessage(message: unknown): void;
}

export interface ClientsLike {
  claim(): Promise<void>;
  matchAll(options?: {
    type?: 'window' | 'worker' | 'sharedworker' | 'all';
    includeUncontrolled?: boolean;
  }): Promise<WindowClientLike[]>;
}

export interface ServiceWorkerScope {
  readonly clients: ClientsLike;
  readonly location: { readonly origin: string };
  skipWaiting(): Promise<void>;
  addEventListener(
    type: 'install' | 'activate',
    listener: (event: ExtendableEventLike) => void,
  ): void;
  addEventListener(type: 'fetch', listener: (event: FetchEventLike) => void): void;
  addEventListener(type: 'message', listener: (event: MessageEvent) => void): void;
}

/**
 * The service-worker global, or `null` when this code is running in a window.
 *
 * ⚠️ `instanceof`, NOT DUCK TYPING. A window has `self`, `location` and
 * `addEventListener` too, and a page that happened to define `clients` would be
 * misidentified. The constructor exists only inside a service worker, so testing
 * against it is exact -- and the `typeof` guard is what keeps the reference from
 * throwing in a window, where the name is undefined rather than falsy.
 */
export function serviceWorkerScope(): ServiceWorkerScope | null {
  const ctor = (globalThis as Record<string, unknown>)['ServiceWorkerGlobalScope'];
  if (typeof ctor !== 'function') return null;
  if (!(globalThis instanceof (ctor as new () => object))) return null;
  return globalThis as unknown as ServiceWorkerScope;
}
