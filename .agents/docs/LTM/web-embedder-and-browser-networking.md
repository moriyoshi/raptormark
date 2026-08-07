# Web Embedder and Browser Networking

## Summary

The `web/` host runs the flat shipping module under Node and Chromium. Ecvisor's compile-time network profiles keep incompatible imports separate, while the browser path combines a re-entrant scheduler, versioned socket ABI, DNS interception, a WebSocket relay, and service-worker inbound sockets.

## Key Facts

- Node must re-read `memory.buffer` after every possible `memory.grow`; cached views become detached.
- Network backend selection is compile-time. Runtime polymorphism would keep every backend's imports live.
- The browser driver uses `ecv_boot`, `ecv_run_slice`, `ecv_next_deadline_in_ms`, `ecv_exit_code`, `ecv_net_ready`, and `ecv_signal`.
- Browser DNS is intercepted at the wire and mapped into `240.0.0.0/4`; the host owns the reverse name mapping.
- Egress is a policy-gated multiplexed WebSocket-to-TCP relay. Inbound HTTP uses in-tab sockets reached through a service worker.
- The service-worker bundle is `web/dist/raptormark.js`; its response needs `Service-Worker-Allowed: /`.
- Browser tests have run only under Chromium.

## Details

### Node and backend profiles

Node's built-in WASI caches the old linear-memory backing store and aborts after guest memory growth, so the shim creates views from the current buffer on each access. Node also cannot deliver asynchronous socket events while the main thread is blocked inside synchronous Wasm. Socket ownership therefore lives in a worker reached through `SharedArrayBuffer` and atomics. The worker uses `Atomics.waitAsync`, and an active `parentPort` listener keeps it alive.

`NetBackend` is a conformance interface, not runtime polymorphism. A cfg-controlled type alias selects one static implementation:

| Profile | Network | Intended host |
|---|---|---|
| default | WasmEdge socket imports | WasmEdge and released shim path |
| loopback | Pure in-process sockets | Stock wasmtime and local guest networking |
| browser | `raptormark_net_v1` | Node re-entrant host and browsers |

The loopback profile reduced a probe from 28 imports to 15 and ran the kernel-pinned non-blocking socket fixture under stock wasmtime. Import count alone is not behavioral evidence.

### Re-entry and readiness

The scheduler separates suspension retirement from process selection. A host waits outside Wasm and re-enters through `ecv_run_slice`; expired deadlines are swept on entry because the host already performed the wait.

The browser driver yields a macrotask before re-entry. A connected socket remains writable, so immediate re-entry on unchanged readiness spins and prevents WebSocket messages from arriving. Readiness is pushed into a runtime cache. A readiness generation wakes poll-set waiters once per actual change, but single-socket `connect`, `recv`, `send`, and `accept` waits wake only for their own socket. Those operations do not re-scan a set, and a spurious wake can leak `EAGAIN`.

### DNS, relay, and inbound HTTP

Browsers have no arbitrary UDP API, so the runtime intercepts both addressed DNS operations and glibc's connected-UDP form. The guest resolver retains libc policy. The host mints a reserved address for each name and reverses it on connect; the runtime keeps no name map.

The relay is off unless requested, requires an allowed origin, and permits nothing under an empty allowlist. Matching is against the requested name before resolution. Stream ids are `u16`; handles above `0xffff` fail rather than truncate. Datagram handles exist for intercepted DNS, while other UDP is refused.

`InboundSockets` serializes a service-worker request as HTTP/1.1 bytes for an ordinary guest listener. `Host` is synthesized because Fetch does not expose it. A restarted service worker loses its saved `MessagePort`, so it asks window clients, including uncontrolled ones, to re-announce.

Responses finish by HTTP framing: `Content-Length`, chunked coding, bodyless `HEAD`/1xx/204/304 rules, or close as fallback. `Transfer-Encoding` takes precedence over `Content-Length`, and chunked bodies are decoded before constructing the browser response.

`CompositeSockets` defers backend selection because an opened TCP socket does not reveal whether it will listen or connect. It records `bind` and socket options, materializes on the first directional operation, and replays them. Outer handles and separate per-backend maps prevent collisions.

### nginx coverage

nginx has served a Chromium tab through the service worker and real TCP under Node. Coverage uses nginx-generated headers and variables, worker pids, distinct concurrent request markers, fixed-seed PRNG file hashes, and direct `sendfile` traces. The master has forked serving workers, replaced a terminated worker, and re-read its VFS configuration on SIGHUP.

Repeated `listen(2)` is supported because nginx uses a second call to update backlog. The Node backend must not create a second server, overwrite the working one, or collapse `ADDRINUSE` into `EIO`.

`ecv_signal` queues host-requested signals between slices. Default signal actions remain unmodelled: SIGKILL and other unhandled defaults may do nothing while posting reports success. nginx tests use signals for which nginx installs handlers.

### Bundle constraints

One ES module bundle serves page and service-worker roles. Role detection uses `globalThis instanceof ServiceWorkerGlobalScope`, and worker listeners install synchronously during module evaluation. A worker script normally controls only its directory, so `internal/serve` sends `Service-Worker-Allowed: /` on JavaScript responses from `dist/`.

Chromium does not serialize `WebAssembly.Module` into IndexedDB. The host caches bytes through the Cache API and relies on the engine's implicit code cache. E2E setup compares the generated bundle's mtime with TypeScript sources so stale output fails immediately.

## Files

- `web/src/`: Shared WASI, driver, Node, and browser backends.
- `web/src/browser/`: Relay, inbound, composite, framing, and service-worker code.
- `runtime/src/net/`: Compile-time network backends.
- `runtime/src/context.rs`, `runtime/src/entry.rs`: Re-entry, readiness, and exports.
- `internal/relay/`, `internal/serve/`: Relay policy and browser artifact serving.
- `e2e/nodehost_test.go`, `e2e/browser_test.go`: Integration drivers.

## Test Coverage

The web gate runs type checking, `oxlint` with warnings denied, `oxfmt` checking, and Node tests. Browser specs run through the env-gated Go E2E suite. Neutralizations cover detached memory views, synthetic DNS, driver yielding, protocol vectors, cross-backend routing, service-worker restart, HTTP framing, response correlation, readiness, signals, and repeated listen behavior.

## Pitfalls

- Use `-count=1` for Go wrappers after changing `web/`; Go's cache does not observe TypeScript inputs.
- Rebuild `web/dist/raptormark.js` after TypeScript changes.
- Rebuild every linked fixture after changing `runtime/`.
- A successful response does not prove DNS interception, `sendfile`, worker forks, concurrency, or reload; each needs its own witness.
- Only Chromium is measured.
- There is no relay UDP or inbound, no response streaming or connection reuse, and no demonstrated guest TLS path.
