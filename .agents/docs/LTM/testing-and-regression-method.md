# Testing and Regression Method

## Summary

The test strategy combines fast host-side Go tests with env-gated real-image E2E coverage. Evidence is accepted only when the check could have failed for the claimed defect and has been neutralized against a deliberately broken behavior.

## Key Facts

- Default `go test ./...` needs neither Docker nor network.
- E2E tests are environment-gated rather than build-tagged, so they still compile and vet normally.
- `RAPTORMARK_E2E_SLOW=1` enables expensive fused-image lifts.
- Audit current code behavior before writing a regression test.
- Deliberately break the fix and confirm the test fails with the intended diagnostic; a compile failure is not neutralization.
- Real fixtures must include both glibc and musl where assumptions differ.

## Details

The E2E suite drives discovery, fusing, translation, registry generation, linking, and execution using a selected builder. Guest test binaries are compiled inside the builder rather than taken from irreplaceable historical images. Without the environment gate, E2E cases skip cleanly when Docker or the builder is absent.

The OpenSSL fixture was split into a fast discovery-and-fuse check and a slow full lift. This catches host-side dynamic-image regressions in seconds without imposing a half-hour default test. Nginx tests extend coverage from startup to real socket traffic and multiple workers.

The upstream OpenSSL slow arm is not a usable green gate: it compiles roughly
162 MB of bitcode in one serial `clang++ -O3` invocation and reaches its
45-minute deadline by construction. The ecvisor arm exercises the shipping split
path and passes. A timeout from the upstream arm must not be attributed to host
load or cited as evidence for a patch.

A regression guard is evidence only within what it observes. The no-proposal unit guard reads `wasmOptArgs`, so neutralizing it with an exception-handling flag proves it catches argument regressions, not proposals introduced elsewhere. The released-shim E2E assertion supplies the broader behavioral check.

Synthetic inputs must preserve the property under test. An arithmetic byte sequence once compressed heavily despite looking random; incompressibility tests now use a real seeded PRNG. Instruction decoding tests use actual encodings taken from `objdump` rather than hand-derived masks alone.

Fork-based tests must deliberately make process state differ. Otherwise a child
inherits the expected bytes and can pass even when no state transfer or sharing
works. The bounded-snapshot probe similarly compares each region and program
pair against the full-buffer scheme as an oracle; aggregating too early hides
which invariant failed. Compiler-folded constants are another false control:
guest instruction tests must force runtime loads so the lifted instruction is
actually executed.

Performance probes follow the same standard. The different-size closure fixture
was necessary to disprove stable partition reuse that appeared correct on an
equal-size pair. Served cache artifacts, rather than merely equal symbol names,
are the observable that proves reuse happened.

## Files

- `e2e/`: Env-gated end-to-end suite.
- `.agents/docs/QUALITY_GATE.md`: Canonical verification commands.
- `internal/*/*_test.go`: Host-side package coverage.
- `runtime/`: Rust unit tests and the `wasm32-wasip1` shipping-target check.

## Test Coverage

Run the Go gate described in `QUALITY_GATE.md`. Use `RAPTORMARK_E2E=1 go test ./e2e/ -v -timeout 60m`, adding `RAPTORMARK_E2E_SLOW=1` and a longer timeout for fused lifts.

## Consolidated Update: Guards that survive their own mutation (2026-08-18)

A guard whose assertion is written in terms of the very thing it checks passes
after that thing is removed, because it stops checking rather than fails. Three
instances so far, all found by neutralization and none by review:

- `TestMaxLinksMatchesTheRuntime`'s predecessor spelled the limit as `maxLinks`,
  so moving the constant moved the test with it. Fixed with literals plus a
  cross-language parse of the Rust constant.
- A cache-identity guard iterated over the runtime list of switches it was
  meant to police. Deleting an entry from that list broke nothing -- the loop
  simply had one fewer case. Fixed by naming the expected set in the test and
  comparing the two lists in BOTH directions, so an addition fails as loudly as
  a removal.
- A key-pinning test compared only the LENGTH of a hash against a fabricated
  constant. It read as "the key is pinned" and would have passed against any
  64-character hex string.

The check that finds all three: after a mutation SURVIVES, ask first whether the
test's expectation is derived from the code under test. If it is, the test has no
independent statement of the truth and cannot fail.

A second question belongs beside it, from the same session: **did the mutation
reach the code the test runs?** A surviving mutation is a question, not a verdict.
Removing a redundant validation -- one whose work another layer already did --
survives correctly, and the right response is deleting the redundancy, not
strengthening the test.

Some guards cannot be neutralized from the host at all: a `try_from` that
protects wasm32's 32-bit `usize` behaves identically under the host's 64-bit one.
Label those in the source as unneutralizable rather than counting them among the
tested rules.

## Pitfalls

- A green test can pass for the wrong reason; identify what a false-claim pass would look like.
- State exactly what a neutralized guard observes and what remains outside its scope.
- Distinguish the 47 host Rust unit tests from the `wasm32-wasip1` shipping-target check and behavioral E2E coverage.
- Validate existing known-good fixtures before treating a new image failure as a regression.
- A deadline failure from a structurally unpassable test is not a regression signal.

## Consolidated Update: Instruments and False Controls

The BTI guard first proves its fixture contains branch landing pads; byte identity on cheap non-branch-protected fixtures would otherwise be vacuous. Instruction guards compare native-oracle bit patterns and force lane, signedness, accumulator, and register-number distinctions. A TLS thread test required a barrier rather than `usleep`; without forced overlap it passed even when every thread shared one thread pointer.

Correctness checks cannot detect a cache reuse regression. Four behavioral and IR checks passed while linker-assigned local suffixes destroyed every shared partition. `e2e/sharednames_test.go` detected it by asserting on served artifacts. A green suite with an empty library-cache directory proves inertness, not operation.

Expensive gates answer yes or no, not which bytes differ. The per-library cache defect was localized by diffing keys and partitions: per-program section names broke keys, and accumulated `llvm.ident` broke bytes. Normalize metadata and attribute ordinals before textual IR comparison.

Stale drivers repeatedly invalidated otherwise sound probes. Prefer dependency assertions in the probe itself: verify the expected patch assertion in a build log, the expected source-sensitive identity change, non-empty generated objects, cache population, and reported plugin skips.

## Consolidated Update: Reachability, Counts, and Causality

Three guards passed without reaching their subject: host tests placed in a wasm32-only module, C fixtures that did not emit the required PAC entry, and an E2E corpus with zero PAC-signed entries. In each case an unchanged count exposed the gap. Record expected test names, totals, affected-function counts, or fixture encodings.

Controls make absence meaningful: a C loop validated the patch 0060 benchmark, pre-patch PostgreSQL reproduced the failure blamed on 0062, unchanged padding bounded patch 0063, and byte-identical nginx artifacts bounded a no-op claim. Diagnostics must distinguish missing instructions from missing blocks and be read after the guest's own error.

## Consolidated Update: Browser and Cross-Layer Evidence

Browser claims use independent witnesses: synthetic DNS addresses, RFC WebSocket vectors, nginx headers and pids, distinct concurrency markers, VFS hashes, sendfile traces, and reload file traces.

Neutralization must fire on the assertion carrying the claim. Generated artifacts need freshness guards; the browser bundle mtime check has caught repeated stale-host recurrences. Unit, type, lint, and browser checks remain complementary.
Vitest owns `src/**/*.test.ts` while Playwright owns `tests/*.spec.ts`; explicit include boundaries prevent the two runners from collecting each other. Service-worker unit tests use an injected fake scope, and a clock-shim unit test guards the perturbation instrument used by the wall-clock-step E2E.

Tooling changes can make previously inexpressible claims testable without any old test failing. Passing a service-worker scope as a parameter instead of reading a global enabled fast fake-scope tests; the typing and testability arguments are independent.
