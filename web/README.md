# web/ — the host embedder

A host for raptormark modules: a hand-rolled WASI preview1 shim plus WasmEdge's
socket extension, written once and shared between the Node reference host and
(later) the browser.

## Why it exists

1. **It is the executable specification of the import ABI.** A linked module
   imports 28 functions, 11 of them WasmEdge's non-standard socket extension.
   `runtime/src/sys.rs:520` and `:568` both name
   `third_party/wazero/imports/wasi_snapshot_preview1/sock_wasmedge.go` as the
   reference host for that ABI — **and that path does not exist in this tree**,
   is not in `.gitmodules` (one entry, elfconv), and is in no commit. Until
   this package, the ABI was specified only by the Rust `extern` blocks that
   consume it, so no host could be checked against it.
2. **It runs a real module without Docker and without a browser**, which makes
   a `runtime/` change verifiable in seconds instead of a 20-minute E2E run.
3. **It is the core the browser embedder is built on.** Everything under
   `src/` is platform-agnostic; only the entry points differ.

## Running

```sh
node bin/run.ts --module <out.wasm> [--rootfs <rootfs.img>] [--env K=V]... [-- guest args]
```

Node >= 22.6 is required and there is **no build step**: node strips the types
and executes the `.ts` sources directly. (The builder image's emsdk node is
20.18 and cannot do this, which is why the E2E tests run this on the host — the
image is needed to *build* a module, not to run one.)

```sh
npm install          # typescript, esbuild, oxlint, oxfmt, playwright
npm run typecheck    # tsc --noEmit; the only thing that checks types
npm run lint         # oxlint, warnings denied
npm run format:check # oxfmt
npm test             # vitest, unit only
```

Unit tests run under **vitest**; the browser specs under Playwright. ⚠️ **The two
suites are split by LOCATION and must not overlap**: vitest owns
`src/**/*.test.ts`, Playwright owns `tests/*.spec.ts`. Vitest's default include
sweeps up `*.spec.ts` too, so the first run after adopting it reported 14 failed
files -- `vitest.config.ts` pins the boundary.

The point of the split is that the FIRST list should grow at the expense of the
second. The service worker was reachable only through Chromium until
`src/browser/sw.test.ts` existed; it now has ten tests running in ~120 ms against
a thirty-line fake scope, covering cases a browser run cannot easily reach at all
(a request the worker must DECLINE, a page that answers with an error, three
requests answered out of order).

The formatter and linter are **oxc** (`oxfmt`, `oxlint`), not prettier and not
eslint. `oxfmt` migrated from `.prettierrc` with `--migrate=prettier` and changed
exactly one line in the tree, so the switch cost nothing in diff.

⚠️ **`oxlint` is a real addition, not a rename.** Prettier was a FORMATTER; this
repo had no linter at all. On its first run it found dead code in two files, and
every rule in `.oxlintrc.json` that is turned off is turned off with a written
reason -- two of them describe fixes that would have introduced bugs. Read those
before re-enabling one.

## Layout

| Path | What |
| --- | --- |
| `src/wasi/abi.ts` | Constants and struct layouts. Specified by `runtime/src/sys.rs:484-576`. |
| `src/wasi/mem.ts` | Linear-memory accessors. See the invariant below. |
| `src/wasi/files.ts` | The one-file virtual preopen that serves the rootfs sidecar. |
| `src/wasi/preview1.ts` | The 17 standard preview1 imports. |
| `src/wasi/sockets.ts` | The 11 `sock_*` imports, over a pluggable backend. |
| `src/host.ts` | Assembles the import object and runs the module. |
| `src/node/protocol.ts` | The main-thread <-> socket-worker wire format. |
| `src/node/sockets-worker.ts` | Owns the real sockets; runs its own event loop. |
| `src/node/sockets.ts` | The synchronous `SocketBackend` the shim sees. |
| `bin/run.ts` | The Node CLI. |

## Why sockets need a second thread

The WasmEdge socket ABI is **synchronous**: `sock_connect` returns an errno,
`fd_read` returns bytes or `EAGAIN`, and `poll_oneoff` blocks until something is
ready. Node's `net` is the opposite — data arrives in `'data'` events.

Those events cannot fire while the guest is running. `_start()` is one
synchronous call that does not return until the guest exits, so the event loop
is blocked for the entire run. A single-threaded implementation deadlocks on the
first read: the socket has data, the event that would deliver it is queued behind
a wasm call that is waiting for that data, and neither moves.

So the sockets live in a worker with its own event loop, and the main thread
talks to it over a `SharedArrayBuffer` with `Atomics.wait`.

Two rules keep it working, both learned the hard way:

- **The worker waits with `Atomics.waitAsync`, never `Atomics.wait`.** A blocking
  wait there stalls the worker's own event loop and reintroduces the deadlock.
- **Something must hold the worker's event loop open.** A pending
  `Atomics.waitAsync` does not, and every socket is deliberately `unref`'d — so
  the worker exited moments after starting and the main thread blocked forever
  on a reply nobody was left to send. It is timing-dependent: a request issued
  immediately after construction is answered normally, and only a run with a gap
  before its first socket call hangs. An active `parentPort` listener fixes it.

None of this carries over to the browser, where `Atomics.wait` is illegal on the
main thread. The browser profile solves the same problem the other way, by making
the guest re-entrant so the host never blocks.

## The one invariant

**Never cache a typed-array view or an `ArrayBuffer` across a call.** Every
accessor in `src/wasi/mem.ts` re-reads `memory.buffer` on entry.

`memory.grow` *detaches* the previous `ArrayBuffer`, so a view built before the
growth silently becomes zero-length — reads return nothing, writes go nowhere.
This is why `node:wasi` is deliberately unused: it caches the backing store when
the memory is bound and never re-reads it, so the first WASI call after any
growth **aborts the process with SIGABRT, no message and no stack**.
`e2e/testdata/embedder.mjs:127-141` documents this from experience.

Every raptormark guest triggers it, because ecvisor allocates a 384 MiB arena
during startup. It is the normal path, not an edge case.

Verified by breaking it: caching the view at module scope made a real guest fail
with `TypeError: Cannot perform DataView.prototype.getUint32 on a detached or
out-of-bounds ArrayBuffer`, raised from `iovecTotal` under `std::fs::read` —
i.e. at `load_sidecar`, the very first host interaction. `src/wasi/mem.test.ts`
guards it by growing the memory between two calls.

## Status

Runs real artifacts end to end. Measured 2026-08-19:

- `hop.wasm` — 2000 rounds of execve, 4000 context switches, exit 0
- `python.wasm` — `ld.so` hook install, 12 `_dl_init` constructors, `STARTUP-OK`
- a freshly built banner guest — byte-identical output to wasmedge
  (`e2e/nodehost_test.go`)

Sockets work, over real `node:net` and `node:dgram`. `e2e/nodehost_test.go`
covers them by reusing `nonblockGuestSrc` — the guest whose expectations are
already pinned to a real Linux kernel by `TestNonblockingSocketNativeBaseline` —
so passing means faithful, not merely self-consistent. It exercises bind, listen,
getsockname, `accept` returning `EAGAIN` on an empty non-blocking listener,
connect completing through the backlog, `accept4` propagating `SOCK_NONBLOCK`,
`recv` returning `EAGAIN`, a real send/recv round trip, and a connect to a dead
port that must fail. A second guest covers `sendto`/`recvfrom`.

`--no-net` swaps in `NullSockets`, which reports `ENOTSUP` and records which
names a guest reached; `bin/run.ts` prints that as `HOST-NOTE:`.

### Known gaps

- **`sock_setsockopt` and `sock_shutdown` are accepted and dropped.** No guest in
  the tree exercises them, so they are unverified rather than known-good.
  `getsockopt(SO_ERROR)` *is* implemented, because libpq calls it on every
  connection and reads a non-zero value as failure.
- **`send` buffers without bound.** Node's `write` accepts everything and reports
  backpressure only through its return value, which is used to clear writability
  but not to refuse data.
- **No IPv6 guest has been run.** The v6 paths are written but untested.

## The re-entrant driver

`--reentrant` drives `ecv_boot` / `ecv_run_slice` instead of `_start`: the guest
runs until it cannot proceed, hands control back, and says when to come back.

```sh
node bin/run.ts --module out.wasm --reentrant --env RAPTORMARK_ECV_NONBLOCK=1
```

⚠️ **It needs both halves, and either alone silently does nothing useful.**

1. A module linked with the re-entrant surface — `link-all --profile loopback`.
   The shipping profile omits it, because `--export=` is also what *links* the
   symbols in and nothing drives them there.
2. A backend that **declines to wait**. With a blocking backend the scheduler
   simply sleeps inside a slice and `ECV_IDLE` never occurs: the run completes,
   looks identical to `_start`, and proves nothing. `RAPTORMARK_ECV_NONBLOCK=1`
   makes the in-process backend decline, which is the browser backend's
   permanent behaviour.

`HOST-SLICES: <n> idle=<k>` reports how many slices ran and how many times the
guest went idle. **`idle=0` means the host never got control back** — the thing
the whole design exists to provide.

### Why a browser needs this

`Atomics.wait` is illegal on a browser main thread, and even in a worker a
blocking host stalls the event loop that delivers the readiness the guest is
waiting for. So instead of the host waiting for ecvisor, ecvisor returns.

That is possible because **no native frames survive a suspension**: at the top of
the leg loop the wasm shadow stack is fully unwound and all guest state lives in
`State`, the arena and `EcvContext`. Returning to the host from there is
indistinguishable, to the guest, from `continue`.

### The deadline is a DELAY, on a clock the host cannot read

`ecv_next_deadline_in_ms()` returns milliseconds until the soonest guest
deadline, or -1 for none. Pass it to `setTimeout` unchanged: do not subtract
`Date.now()`, because guest deadlines are measured against ecvisor's own
monotonic clock, which counts from boot and has no relation to the epoch.

It used to be `ecv_next_deadline_ms()`, an absolute unix instant, back when
guest deadlines were wall-clock. A wall clock steps — NTP, a laptop suspend, a
backgrounded tab whose `Date.now()` jumps on resume — and a deadline measured
against one moves with the step, ending a sleep early or overrunning it by the
size of the jump. The export was renamed rather than redefined so a host still
holding the old contract fails at instantiation, naming the missing export,
instead of silently computing a negative delay and spinning.

### Timing a guest from the host: `--stamp`

A guest cannot time itself with the clock under test. `clock_gettime` around a
loop survives a clock running at the wrong *rate* — both ends scale — but not a
clock that advances per *read*: two reads bracketing 200 000 syscalls land in
the same tick and the loop measures as zero.

```sh
node bin/run.ts --module bench.wasm --stamp BENCH-MARK
```

The guest prints `BENCH-MARK <NAME>` on its own line and flushes. `fd_write` is
a synchronous import, so the host's `onOutput` runs at the instant the guest
reached that line — inside the same slice, before the guest gets control back —
and it reports

```
HOST-STAMP-<NAME>-US: <microseconds since process start>
```

on stderr. `performance.now()` is monotonic and is not the clock
`--clock-step-ms` moves, so it is the one reading in the run that neither the
guest nor a test instrument can touch. That is the same argument
`HOST-AFTER-STEP-MS` rests on; `--stamp` is its general form.

Microseconds, not milliseconds, because the consumer divides by an iteration
count: at millisecond resolution a 20 ms loop of 200 000 calls quantises to
5 ns/call, which is a visible fraction of the number being reported.

Names are restricted to `[A-Z][A-Z0-9-]*` — what `clockValue` in
`e2e/clock_test.go` parses. A name outside it is *reported* rather than dropped,
because a stamp no consumer can find is exactly the silence the mechanism exists
to remove. Consumer: `TestClockBenchIsolatesTheHostClockRead`.

### Host imports must never call back into the exports

`ecv_run_slice` is on the stack during every import call, so re-entering it is
reentrancy into a `&mut`-borrowed context. It is safe by construction — a JS
callback cannot fire during a synchronous wasm call — but that is the *reason*,
not an assumption to lean on silently.

## DNS, and why it comes first

⚠️ **There is no UDP in a browser.** Not "awkward" — there is no API, and
WebTransport and WebRTC are datagram channels to a peer you control, not to an
arbitrary host. A guest's resolver sends UDP to port 53, so DNS is the *first*
thing that breaks, before any HTTP request is attempted. It has to be solved
before any transport, which is the opposite of the intuitive order.

The runtime taps the DNS **wire** (`runtime/src/net/dns.rs`), so the guest's own
resolver runs unmodified — `nsswitch.conf`, musl's and glibc's very different
paths, search domains, the hosts file. Rewriting `getaddrinfo` instead would mean
reimplementing each libc's policy, per libc.

### The host mints the addresses

`fetch` needs a URL, a relay's `CONNECT` needs a host, TLS needs SNI — but
`connect(2)` carries only an **address**, the name having been consumed by the
resolver and thrown away. So `AddressPool` mints one address per name out of
`240.0.0.0/4` (reserved, never routable), the runtime encodes whatever it is
given into the answer verbatim, and `net_connect` reverses it back to the name.

The runtime therefore keeps **no** name mapping and needs no connect-by-name
call, and per-destination policy — fetch, relay, refuse — stays entirely
host-side. This is what slirp and passt do, for the same reason.

### Two traps

- **A resolver `connect()`s its UDP socket.** glibc's `res_send` calls
  `connect(2)` and then plain `send`/`recv`, so an interception watching only
  `sendto`/`recvfrom` sees nothing at all — and `getaddrinfo` fails with a
  name-resolution timeout that looks like a missing nameserver rather than a
  missing hook.
- **Do not serialise the address as octets.** The reversed name travels in
  `SockAddr.ip` because node's `connect` accepts a hostname there; a four-octet
  encoding in between turns `"localhost"` into `0.0.0.0`.

## The browser

```sh
npm install
npm run build                       # one artifact, two roles; see below
raptormark serve --root web         # Content-Type: application/wasm
# then open http://127.0.0.1:8787/?module=./public/guest.wasm&rootfs=./public/rootfs.img
```

Artifacts must be built with `link-all --profile browser`. Build the fixtures
with:

```sh
RAPTORMARK_E2E=1 RAPTORMARK_BUILD_BROWSER_FIXTURE=1 \
  RAPTORMARK_BUILDER=raptormark-builder:<tag> go test ./e2e/ -run TestBuildBrowserFixture
```

### There IS a build step here, unlike the Node path

Node strips types and runs `.ts` directly; a browser cannot, and `.ts` imports
carry extensions no browser resolves. So `npm run build` bundles with esbuild.

It produces **one** artifact: `src/browser/run.ts` bundles to
`web/dist/raptormark.js`, ESM, and that single file is both the module the pages
import **and** the script registered as the service worker. It forks on
`serviceWorkerScope()` at module top level -- in a window that returns null and
the file is just its exports.

⚠️ **A service worker's maximum scope is the directory its script was served
from.** The bundle lives under `dist/`, so by default its worker could only
control `/dist/*` and would never see the `/_guest/*` requests it exists to
intercept. It reaches the whole origin because `internal/serve` sends
`Service-Worker-Allowed: /` **with the script response** — the browser reads the
header there, not from the page, so any other server hosting this bundle must
send it too. Without it, registration is rejected with a message that names the
scope and not the header.

⚠️ **Registration must pass `{ type: 'module' }`.** The bundle is ESM and a
classic worker cannot execute `import`. That sets a browser floor -- Chromium 91,
Firefox 114, Safari 16.4 -- which a separate classic worker script would not
have. It is the price of one entry point, taken deliberately.

⚠️ **The worker therefore carries the whole page bundle**, ~66 KB rather than the
~3 KB the service-worker half needs, re-parsed every time the browser restarts a
terminated worker. Nothing in the graph runs at import time, so it is dead weight
rather than a hazard -- but it is on the request path.

⚠️ **And nothing in this module graph may use top-level `await`.** A service
worker only receives events whose listeners were added during initial
evaluation, so a graph that suspends before `installServiceWorker` runs yields a
worker that installs, activates, controls the page and then silently ignores
every request.

`src/browser/swscope.ts` hand-declares the handful of service-worker globals the
code uses. They cannot come from TypeScript's `WebWorker` lib, because that
cannot share a program with `DOM` -- the two declare the same names with
different types -- and one file running in both contexts needs `DOM`.

⚠️ **`web/dist/` is generated and gitignored.** `requireBrowserSuite` checks the
bundle against the newest `.ts` and refuses to run on a stale one: a stale bundle
otherwise fails as ten minutes of browser timeouts blamed on the guest.

### ⚠️ The compiled module cannot be cached

Storing a `WebAssembly.Module` in IndexedDB is widely advised and **Chromium
refuses it**:

```
DataCloneError: A WebAssembly.Module can not be serialized for storage.
```

That capability existed and was removed. The first version of this code did
exactly that, and it failed *silently* — the write rejected, every read missed,
and the page recompiled every load while appearing to have a cache. It was found
only because the test printed hit-or-miss instead of merely asserting the page
still worked.

What is cached is the **bytes**, through the Cache API, which removes the
download — the dominant cost for a 120 MB artifact. Compilation is left to the
engine's implicit code cache. The test now asserts a real hit.

### ⚠️ Cap the idle sleep only when there is a transport to poll

A deadline with no I/O has exactly one wake source, so the host should wait the
whole delay. Capping unconditionally turned a single 400 ms guest sleep into
**76 wakeups** instead of one — not a correctness bug, but pure churn, and in a
browser it is 76 timer callbacks.

### What is verified in a real browser

Chromium, via Playwright (`web/tests/`, driven from `e2e/browser_test.go`):

- a translated aarch64 guest boots and prints its own output in a tab, exit 0
- a guest that sleeps **yields the tab** — the page stays responsive mid-run,
  and the driver reports 1 idle wait rather than 0
- the bytes cache misses then hits

Firefox and WebKit are one flag away (`RAPTORMARK_BROWSERS=firefox,webkit`) but
need system libraries that only root can install (`sudo npx playwright
install-deps`), so they are not enabled by default. **Only Chromium has actually
been run**; the cross-engine differences that motivated a multi-engine harness —
storage behaviour above all — remain unmeasured.

### Egress

`RelaySockets` (`src/browser/relay.ts`) speaks the multiplexed WebSocket-to-TCP
subprotocol; `internal/relay` serves it, and `raptormark serve --relay
--relay-allow <host:port>` mounts it beside the page. A guest in a tab resolves
a name through the in-runtime DNS tap, connects to the synthetic address it gets
back, and the relay dials the real destination — verified end to end by
`tests/relay.spec.ts`, driven from `e2e/relay_test.go`.

⚠️ **The relay is off unless asked for, and an empty allowlist permits
nothing** — there is deliberately no allow-all value, and `Origin` is required.
A relay with no policy will dial anywhere on behalf of anyone who can reach it,
from inside whatever network it runs on, which is an SSRF pivot. Set
`Config.Log` when debugging: every refusal reaches the guest as a bare errno,
and "not permitted" and "the dial failed" are indistinguishable from the far
side by design.

### Datagrams

`open(v6, dgram)` mints a handle that carries nothing, and anything the DNS tap
does not answer is refused at first use.

⚠️ **Do not "simplify" this to `ENOTSUP`.** It was that once, and it broke
`getaddrinfo` from a layer away: `BrowserNet::socket` calls the host for a
resolver's datagram socket like any other, and only *afterwards* does the
in-runtime tap intercept the query. Refusing the socket meant the tap never saw
one, and the failure reads as "no nameserver configured".

### Inbound: a service worker serving the guest

`inbound.html` runs a guest that *listens*. The service-worker half of
`dist/raptormark.js` (`src/browser/sw.ts`) intercepts `/_guest/*`, hands the request
to the page, and `InboundSockets`
writes HTTP/1.1 request bytes into the guest's accepted socket and reads the
response back. Nothing touches the network — this path works offline.

⚠️ **The runtime and the ABI needed no changes for this.** `bind`/`listen`/
`accept` already cross the `raptormark_net_v1` seam; only `RelaySockets` refuses
them. Inbound is a second backend, not a new capability.

Three things that are easy to get wrong:

- ⚠️ **`Host` is synthesized, not copied.** It is a forbidden header, so `fetch`
  never exposes it on a `Request` — and every HTTP/1.1 server requires one.
  nginx's default vhost answers 400 without it.
- ⚠️ **The response is bounded by FRAMING, not by the guest hanging up.**
  `Content-Length`, or `Transfer-Encoding: chunked`, or — only when neither is
  present — at close. Waiting for a close times out against every server that
  keeps the connection open, which is the HTTP/1.1 default and includes nginx.
  See `framing.ts`; `HEAD`/204/304 have no body whatever their headers claim, so
  the request method has to reach the framer.
- ⚠️ **A server guest never exits**, so `run()` cannot be awaited.
  `serveGuestInBrowser` waits for the guest to reach `listen` instead, and
  exposes `exited` so an unexpected exit surfaces as itself rather than as
  requests that quietly start timing out.

The `notify()` in `InboundSockets.deliver` is **latency, not correctness**:
the driver's idle poll finds a queued connection on its own within one poll
interval. Removing the call changes nothing any test can observe — measured, not
assumed.

### Serving and dialling from one guest

`CompositeSockets` routes a guest's sockets between `InboundSockets` and
`RelaySockets`, so one process can accept a request and dial an upstream for it.
`serveGuestInBrowser` always uses it, with or without a relay.

⚠️ **The hard part is that `socket()` does not say which direction it is.**
`socket(AF_INET, SOCK_STREAM, 0)` is identical for a server and a client; only
`listen` or `connect` reveals it, and `bind` comes before either and belongs to
both. So the composite defers: `open` mints an *undecided* handle, `bind` and
`setsockopt` are recorded, and the first `listen`/`connect` materializes the
socket in the matching backend and replays them in order. That replay matters —
an ordinary server sets `SO_REUSEADDR` before it binds, which is before anything
knows what it is.

Handles the guest sees are **outer** handles minted by the composite, including
for accepted sockets, and there is **one inner→outer map per side**: the two
backends allocate independently and their numbers *will* collide.

⚠️ **`RelaySockets` handle bases are not free.** A relay handle *is* the stream
id on the wire and that id is `u16`, so a base of 2,000,000 silently became
33920 — the relay dialled successfully and answered about a stream this side had
never heard of, and the guest hung with a working connection in the log.
`open` now refuses anything above `0xFFFF`. Ids are never recycled, so that is
also a ceiling of 65535 outbound connections per relay socket.

A client's `bind` is recorded and then **dropped** if the socket turns out to
dial: over a relay the source address is the relay's, so a guest-chosen source
port cannot be honoured by anything, and failing instead would break every
client that harmlessly binds `0.0.0.0:0` first.

### ⚠️ A service worker forgets

A service worker is **not** a long-lived process: the browser terminates an idle
one and starts a fresh copy for the next event, and that copy has none of the
module state the old one had — including the `MessagePort` the page handed it.

Reading a missing port as "no page is open" produces a 503 blaming a page that is
open and running, **intermittently**, only under enough load for the worker to
look idle. The worker now asks its clients to re-announce and only falls back to
the 503 if none answers. `sw.ts` accepts a `raptormark-forget-host` message so
that path can be tested — a real restart happens on the browser's schedule and
cannot be provoked.

### nginx

`nginx/1.31.3`, translated from the Alpine aarch64 binary, serves this page's
`fetch` calls from inside the tab — `Server:` header, `$request_uri`/`$http_host`
echoed from the bytes it parsed, and its own 403 page for a denied path.

⚠️ **The locations in `nginx.conf` carry the `/_guest/` prefix.** The service
worker forwards the path VERBATIM and does not rewrite it, which is the honest
arrangement — nginx sees exactly what the browser sent. Matching on `/echo`
silently fell through to `location /`, because nginx takes the longest matching
prefix and `/_guest/echo` does not begin with `/echo`.

⚠️ `io_setup()` returns `ENOSYS` and nginx logs it at `[emerg]`. Not fatal —
nginx disables file AIO and carries on — but a config that actually uses `aio`
would behave differently.

**Worker processes work too.** `nginx-workers.img` runs nginx's normal shape --
a master that forks workers, reconstructed by ecvisor's full replay, through the
re-entrant scheduler. It needed no runtime change; the module is byte-identical
and only the sidecar differs.

⚠️ **`$pid` is what makes that checkable.** A correct body is not evidence a
worker ran, because the master would serve it too. ecvisor starts the master as
pid 1, so the test requires a reply from something else — and requires more than
one distinct pid, because nginx was once pinned to a single worker here (traced
`accept4` counts 20/1/0/0 across four workers).

`nginx.img` keeps the single-process (`master_process off`) variant as the
simpler regression guard.

**Under concurrency**, 25 `fetch` calls issued together all complete in ~21–23 ms
with both workers serving. ⚠️ The assertion that matters is not that they
succeed — a fully serialised host would satisfy that — but that each response
echoes **its own** marker. Crossed responses are the shape a concurrency bug
takes here: 25 sockets are open at once, each with its own framer and pending
promise, and putting a reply on the wrong one is silent.

⚠️ When timing this, `await` before stamping the clock. Written as
`{ ms: performance.now() - t0, results: await Promise.all(...) }` the properties
evaluate in order and `ms` measures how long it took to *issue* the fetches —
2 ms, a number that looks like a latency and is not one.

### Files from the VFS

`nginx-files.img` and `nginx-sendfile.img` give nginx a document root, so the
sidecar is on the request path rather than read once at startup: open, stat,
read, index resolution, and mime types. Both `sendfile off` and `sendfile on`
work — the latter is what the Alpine image ships.

⚠️ **The assertion is a SHA-256, not a body.** A short read or an off-by-one
still produces plausible text; 64 KiB of PRNG bytes compared byte for byte does
not. Dropping one byte from every response fails this test and *passes* every
other nginx test in the suite.

⚠️ **sendfile passing is not sendfile running.** nginx falls back silently — it
logs `sendfile() failed` and serves by read/write, and every assertion would
still hold. Traced once to confirm: `svc nr=71` from both workers with count
`0x10000`, the whole file in one call. The standing test checks the outcome plus
the absence of that log line, which is weaker than tracing every run.

### Signalling the guest

`ecv_signal(pid, sig)` lets the host post a signal, and `GuestServer.signal()`
exposes it (`window.__signal` on the demo page). ⚠️ **A guest in a tab otherwise
has no outside** — nothing can `kill` it — so a supervisor like nginx's master
could only ever be watched starting up. Killing a worker now makes the master
fork a replacement.

⚠️ Queued and delivered **between** slices, never during one: a slice holds the
process table `&mut`.

⚠️ **SIGKILL is a silent no-op.** ecvisor does not model default signal actions
(`deliver_pending_signals` leaves SIG_DFL pending), and SIGKILL is uncatchable so
it never has a handler. `post_signal` still returns true. Use a signal the guest
installs a handler for — SIGTERM for an nginx worker.

A request in flight on a dying worker **is lost**, as on any server: the
connection closes with no response and the host returns 502. `parseResponse`
names that case specifically rather than reporting a parse error.

**SIGHUP reloads** too: nginx re-reads its config from the VFS, starts a new
worker pool and retires the old one without dropping the listener. Confirmed by
`RAPTORMARK_ECV_FILETRACE=1` showing `/etc/nginx/nginx.conf` read a second time
on a fresh descriptor. ⚠️ The config's *content* cannot be changed mid-run — the
sidecar is parsed into guest memory at boot — so this shows the mechanism, not a
changed behaviour.

### Not yet built

Connection reuse (the response is framed correctly, but the host still opens one
connection per request), response streaming (the whole body is buffered), UDP
over the relay, inbound *over* the relay (`LISTEN`/`ACCEPT` opcodes and a public
port), Worker hosting, the fetch-backed HTTP proxy, and the `VirtNet` L2 stack.
