# Deployment Targets and Profiles Synthesis

## Summary

Raptormark emits one artifact shape per TARGET, selected by `--profile`. A
profile is a pair of compile-time backend choices -- one `net-*` and one
`load-*` -- and the whole point of the seam is that a backend which is not
selected leaves no import in the module. Each target forecloses something
different, and the constraints that bind the shipping artifact do not
automatically bind the others.

## Included Documents

| Document | Focus |
|----------|-------|
| [wasm-runtime-and-oci-compatibility.md](./wasm-runtime-and-oci-compatibility.md) | Wasm 2.0, proposal-free suspension, OCI/runwasi packaging |
| [wasix-and-wasmer.md](./wasix-and-wasmer.md) | The measured WASIX ABI, `--profile wasix`, the deferred loader half |
| [web-embedder-and-browser-networking.md](./web-embedder-and-browser-networking.md) | Node and browser hosts, the re-entrant surface, DNS, relay, inbound |

⚠️ `wasm-runtime-and-oci-compatibility.md` also feeds
[ecvisor-runtime-synthesis.md](./ecvisor-runtime-synthesis.md), which reads it
for the SUSPENSION model. This document reads it for the TARGET constraint. The
dual membership is deliberate; neither synthesis owns it.

Closely related and deliberately standalone:
[dynamic-side-module-loading.md](./dynamic-side-module-loading.md) supplies the
`load-*` half of every profile below.

## Stable Knowledge

### The profile matrix

| profile | net backend | loader | intended host | egress |
|---|---|---|---|---|
| **default** | `net-wasmedge` | `preloaded` | WasmEdge, released runwasi shim | real |
| `loopback` | `net-loopback` | `preloaded` | stock wasmtime | in-process only |
| `browser` | `net-browser` | `preloaded` | Node re-entrant host, browsers | via WebSocket relay |
| `hosted` | `net-loopback` | `hosted` | an embedder that serves loads | in-process only |
| `wasix` | `net-wasix` | `wasix` (deferred) | wasmer | real |

⚠️ The three-row table in `web-embedder-and-browser-networking.md` predates
`hosted` and `wasix`. This is the current set.

Measured import counts on a trivial guest: default **28** (11 of them WasmEdge
socket extensions), loopback **15**, hosted **16** -- the extra one being exactly
`env.ecv_host_load_side`. Import count is orientation, never behavioural
evidence.

### Why the seam is compile-time

`wasm-ld` emits an import for every undefined symbol reachable from live code,
so a `dyn` trait object or an `enum` keeps EVERY backend live and puts every
backend's imports in the module. `--gc-sections` cannot help: it cannot prove a
vtable slot is never called. Therefore `NetBackend` and `LoaderBackend` are
CONFORMANCE CONTRACTS and `type Net` / `type Loader` are cfg-selected aliases.

Consequence: a change to one backend cannot reach another profile's artifact,
and the hashes say so without anyone reasoning about it. Across five builder
rebuilds while `net/wasix.rs` was being neutralized, `/opt/ecvisor/libecvisor.a`
and `/opt/ecvisor/loopback/libecvisor.a` stayed byte-identical because that file
is `cfg`'d out of both; only the wasix archive moved.

### What each target forecloses

- **Released runwasi shims** reject any proposal beyond Wasm 2.0, and enabling
  one shim-side is not on the table. They also have their own process and
  environment model, so direct `wasmedge` success is not evidence for them.
- **Stock wasmtime** loads the module but rejects the WasmEdge socket
  extensions, so it needs `loopback` -- which has no external egress at all.
- **Browsers** have no arbitrary UDP API (hence DNS interception at the wire and
  synthetic `240.0.0.0/4` addresses), will not synchronously compile a module
  over **8 MB**, and freeze the page thread on a compute-bound guest.
- **Node** caches the linear-memory backing store and aborts after
  `memory.grow` unless views are re-created per access; it supplies none of
  WasmEdge's socket imports; and it cannot deliver async socket events while the
  main thread is inside synchronous Wasm, so socket ownership lives in a worker
  behind `SharedArrayBuffer` and atomics.
- **wasmer** rejects every wasmedge-profile module on `sock_open` -- an ABI
  mismatch, not a missing flag -- and its dynamic linking requires the main
  module to import `__stack_pointer`, which only a PIC link does.

### The Wasm 2.0 rule is SCOPED

The no-proposal-beyond-2.0 rule binds the SHIPPING artifact. Its rationale names
released `containerd-shim-wasmedge` and `wasmtime`; it does not bind a profile
targeting something else. An opt-in `--profile <engine>` artifact for an
advanced runtime may use proposals beyond 2.0, because nothing loads it through
a stock runwasi shim. It must never be the default and never
`TestWasmOptEnablesNoProposal`'s business.

❗ Misread once, at real cost: the rule stated absolutely was applied
absolutely, and `load-wasix` -- which needs a shared memory, i.e. the threads
proposal -- was written off as blocked. It was not. **If a constraint's
justification names a target, check whether your artifact has that target before
treating it as a wall.**

### The re-entrant surface pairs with the loader

A mid-run load REQUIRES the guest to yield so the host can instantiate
asynchronously and then call `ecv_side_loaded`; `_start` returns only when the
guest is finished. `linkall.go` puts the re-entrant surface on non-default
profiles only, so `hosted` could never have been wasmedge-based -- the pairing is
forced, not chosen. The driver exports are `ecv_boot`, `ecv_run_slice`,
`ecv_next_deadline_in_ms`, `ecv_exit_code`, `ecv_net_ready`, `ecv_signal`, and
on `hosted` also `ecv_side_loaded`.

`ecv_next_deadline_in_ms` returns a DELAY, not an instant, because a monotonic
instant means nothing next to the host's `Date.now()`.

## Operational Guidance

Pick the profile from the HOST, then check what that host forecloses before
diagnosing a guest. Three flags spell the same thing three ways and none fails
loudly when swapped:

```
wasmedge --dir    GUEST:HOST
wasmtime --dir    HOST::GUEST
wasmer   --volume HOST:GUEST     (--mapdir is deprecated in 7.3.0)
```

`--runtime wasmer` must always pass `--net`: without it `sock_open` SUCCEEDS and
`sock_bind` returns errno 58, so nothing reports a problem until the first bind.

⚠️ **Adding a `net-*` backend means editing FOUR places and the compiler catches
none of them.** `net-loopback` is a LABEL, not a `cfg`: `runtime/src/net/mod.rs`
selects loopback by the ABSENCE of every other backend, from **two separate
`any(...)` lists**. The others are the `PROFILES` list in
`profile_exclusion_test.sh` and a `cargo check --features net-<name>` line in
`QUALITY_GATE.md` §2.

## Files

- `runtime/src/net/`: `mod.rs` (the two `any(...)` lists), `wasmedge.rs`, `browser.rs`, `wasix.rs`, `wasix_addr.rs`, `poll1.rs`.
- `runtime/src/loader/`: `mod.rs`, `preloaded.rs`, `hosted.rs`, `wasix.rs`.
- `runtime/{loopback,browser,hosted,wasix}/BUILD.bazel`: one archive each.
- `internal/builder/linkall.go`: profile selection, the re-entrant surface, `wasmOptArgs`.
- `internal/pipeline/run.go`: `runtimeArgs`, the one place that knows the three flag spellings.
- `web/src/`: the Node and browser hosts.
- `.agents/docs/WASIX_ABI.md`: the measured wasmer ABI record.

## Tests

- `//runtime:profile_exclusion_test` -- each profile carries exactly one net backend AND carries its own.
- `//runtime:loader_exclusion_test` -- the shipping archive carries no host-aided loader; `hosted` and `wasix` carry only their own.
- `TestWasmOptEnablesNoProposal` -- guards the finalizer argument list for the DEFAULT only.
- `e2e/loopback_test.go` (stock wasmtime), `e2e/wasixnet_test.go` (stock `wasmer run --net`, gated by `RAPTORMARK_E2E_WASMER=1`), `e2e/containerd_test.go` (released shim), `e2e/nodehost_test.go` and `e2e/browser_test.go`.

## Pitfalls

- **Both exclusion tests once passed vacuously, and only a neutralization
  showed it.** They are ABSENCE assertions, and a backend that leaves no symbols
  satisfies them trivially: `ecvisor::net::loopback` is pure Rust and inlines
  away at `-Clto=fat`, so "loopback is absent from this archive" was true of an
  archive that had compiled it in. Proven by pointing `//runtime/wasix` at
  `net-loopback` and watching all four `foreign-backends=0` lines still print
  `ok`. Positive controls -- an export floor, a "pattern is live" check, and each
  importing backend found in its OWN archive -- are what make them mean
  something. The scripts say not to delete them.
- **Match the MANGLED module path, not the import name.** Grepping
  `ecv_host_load_side` finds 0 in both archives; the Rust symbol is
  `ecvisor::loader::hosted::host_load_side` and `#[link_name]` renames it only at
  link time. The same trap applies to `sock_*`, whose 17 occurrences are
  ecvisor's own functions, not host imports.
- **A host ABI is not portable between hosts just because the names match.**
  Every WASIX trap produced a module whose import section was perfect and whose
  behaviour was wrong: a `poll_oneoff` timeout of 0 means "return now" to
  preview1 and "wait forever" to WASIX; the address port is little-endian
  written and big-endian read back; `sock_accept` binds to a 4-parameter
  `sock_accept_v2`. Only the arity mistake fails loudly.
- `--profile hosted` imports `env.ecv_host_load_side`, which no stock runwasi
  shim supplies, so such a module fails to INSTANTIATE before running an
  instruction. It is never the default, and the exclusion test is what keeps it
  that way.
- **`NetBackend::ready` is exercised end-to-end on ONE profile.** Nothing in
  `e2e/` epolled a socket until `epollSocketGuestSrc`; `timers_test.go` epolls an
  eventfd, and the socket tests block in `accept`/`connect`, which the scheduler
  serves through `wait`. The SHIPPING profile's `ready` is still uncovered, and
  it is the one guarding the PostgreSQL ServerLoop deadlock.
- Do not infer shim compatibility from direct `wasmedge` execution, and do not
  confuse the wasmtime socket-import blocker with a Wasm feature rejection.
- Only Chromium has been measured in the browser matrix.
