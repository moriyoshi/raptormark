import assert from 'node:assert/strict';
import { afterEach, test } from 'vitest';

import { serviceWorkerScope } from './swscope.ts';

/**
 * The fork that makes one bundle serve two roles.
 *
 * ⚠️ GETTING THIS WRONG IS SILENT IN BOTH DIRECTIONS. A false negative gives a
 * service worker that registers, activates, controls the page and then ignores
 * every request -- which is what a Playwright run reports as "the guest never
 * listened". A false positive would have a WINDOW install fetch listeners it can
 * never receive. Neither raises anything.
 */

const KEY = 'ServiceWorkerGlobalScope';
const g = globalThis as Record<string, unknown>;

afterEach(() => {
  delete g[KEY];
});

test('a plain Node or window global is not a service worker', () => {
  assert.equal(g[KEY], undefined, 'the fixture assumes the name is absent here');
  assert.equal(
    serviceWorkerScope(),
    null,
    'without the constructor there is no service worker, and referencing the ' +
      'name must not throw either',
  );
});

/**
 * ⚠️ THE DUCK-TYPING TRAP. A window has `self`, `location`, `addEventListener`
 * and, in a page that defines one, `clients`. Only an `instanceof` against the
 * constructor separates them -- and merely finding the NAME defined is not
 * enough, because a page can define anything.
 */
test('the constructor existing is not enough; the global must be an instance', () => {
  // A plain constructor `globalThis` is not built from.
  function NotOurScope() {}
  g[KEY] = NotOurScope;
  assert.equal(
    serviceWorkerScope(),
    null,
    'the name is defined but `globalThis` is not an instance of it, so this is ' +
      'a page that happens to have the symbol, not a service worker',
  );
});

test('a global that IS an instance is returned', () => {
  // `Symbol.hasInstance` is how a test can make `globalThis instanceof X` true
  // without running inside a real service worker. It has to hang off a FUNCTION
  // rather than an object literal: `serviceWorkerScope` checks `typeof ctor ===
  // 'function'` before it ever reaches `instanceof`, which is the next test.
  function OurScope() {}
  Object.defineProperty(OurScope, Symbol.hasInstance, {
    value: (x: unknown) => x === globalThis,
  });
  g[KEY] = OurScope;
  assert.equal(serviceWorkerScope(), globalThis as unknown);
});

test('a non-callable value under the name is refused rather than thrown on', () => {
  // ⚠️ `instanceof` against a non-constructor is a TypeError. Reaching it would
  // turn a page with an unrelated global into a crash at module evaluation --
  // i.e. the whole bundle failing to load, for every visitor.
  g[KEY] = 42;
  assert.equal(serviceWorkerScope(), null);
  g[KEY] = { not: 'callable' };
  assert.equal(serviceWorkerScope(), null);
});
