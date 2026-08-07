# Wasm Runtime and OCI Compatibility

## Summary

Raptormark modules target Wasm 2.0 and must run through released OCI/containerd Wasm shims without enabling newer proposals. The `raptormark oci` command packages translated output for that environment, while ecvisor's suspension model stays within the supported feature set.

## Key Facts

- The emitted module must declare no proposal beyond Wasm 2.0.
- Earlier exception-based suspension required unsupported exception handling.
- Suspension by ordinary early return removed that requirement.
- Released runwasi shims have their own process and environment model; direct WasmEdge success is not sufficient evidence.
- OCI artifacts and containerd E2E fixtures must be tested through the released shim path.

## Details

Inspection of runwasi established how the shim supplies argv, environment, preopens, and lifecycle state. `raptormark oci` was added to produce the expected artifact shape. Initial measurements showed released shims rejecting modules that requested exception handling even when direct runtimes could be configured to accept it.

Ecvisor was changed so a guest scheduling boundary unwinds by returning normally through lifted frames. Re-benchmarking showed the proposal-free model remains functional, and the resulting module ran through the stock released containerd/WasmEdge path.

The compatibility requirement has two guards with different scopes. `TestWasmOptEnablesNoProposal` observes the `wasmOptArgs` list and catches a reintroduced `--enable-*` or `--translate-to-exnref` flag; it was neutralized by adding `--enable-exception-handling` and observing the intended diagnostic. It cannot detect a proposal entering by another producer or toolchain route. The released containerd-shim E2E path covers that broader outcome and retains the old exception-handling module as historical neutralization evidence.

Wasmtime has a separate portability blocker: live code unconditionally imports WasmEdge-specific socket extensions from `wasi_snapshot_preview1`, so its shim loads the module but rejects the unknown `sock_open` import. That unresolved work is recorded in `TODO.md` and must not be confused with proposal support.

## Files

- `cmd/raptormark/`: OCI command implementation.
- `runtime/`: Ecvisor suspension and entry logic.
- `e2e/containerd_test.go`: Released-shim integration coverage.
- `internal/builder/linkall.go`: Final Wasm linking and optimization arguments.

## Test Coverage

`TestWasmOptEnablesNoProposal` guards the finalizer argument list and has been behaviorally neutralized. Env-gated containerd tests cover the emitted module through released shims and distinguish WasmEdge success from the known wasmtime socket-import failure.

## Pitfalls

- Do not infer shim compatibility from direct `wasmedge` execution.
- Do not solve compatibility by enabling exception handling shim-side.
- An argument-list guard cannot detect proposals introduced elsewhere.
- Do not confuse the wasmtime socket-import blocker with a Wasm feature rejection.

## Consolidated Update: Multi-Module Host Constraints

The split protocol works under a development Node embedder, but Node's WASI binding caches the linear-memory backing store and aborts after memory growth unless privately rebound; it also lacks WasmEdge socket imports. WasmEdge 0.17.1 cannot instantiate the module set, while V8 inlines cross-module calls and cannot price lost AOT inlining in advance. Supporting the design therefore means owning an embedder. LLVM 22's wider default feature set also needs a real-pipeline Wasm 2.0 check.

## Consolidated Update: Portable Profiles and Browser Modules

A loopback module runs on stock wasmtime; default and browser profiles retain distinct imports. Command modules expose both `_start` and re-entrant entry points.

A vDSO cannot transfer because dispatch tables contain only lift-time code and lifted `svc` already calls ecvisor without a privilege transition. Only Chromium has exercised the ES-module service worker.
