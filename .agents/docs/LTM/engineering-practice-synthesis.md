# Engineering Practice Synthesis

## Summary

Raptormark work is shaped by expensive irregular builds, a shared reconstructed
tree, and checks that can pass without observing the intended behavior. Durable
progress depends on precise evidence, cheap feedback loops, current quality
gates, and disciplined memory maintenance.

## Included Documents

| Document | Focus |
|----------|-------|
| [performance-investigation.md](./performance-investigation.md) | Current critical paths and measurement method |
| [testing-and-regression-method.md](./testing-and-regression-method.md) | Host/E2E gates, false controls, and neutralization |
| [agent-harness-and-quality-gate.md](./agent-harness-and-quality-gate.md) | Repository rules, canonical documents, and memory workflow |
| [hot-path-cost-and-opt-in-design.md](./hot-path-cost-and-opt-in-design.md) | Interpreted hot-path costs, genuinely opt-in design, and real-workload controls |

## Stable Knowledge

- Build cost follows the largest function or partition, not total bytes. Partition bytes predict compile cost poorly.
- Deterministic lifting, library-scoped partitions, caching, and relink-without-relift shorten the critical path.
- `patches/0038` removed per-byte executable-code hashing for a 2.8-3.0x lift improvement; `patches/0040` added word reads and block membership for another 1.9x.
- Once library codegen is cached, the measured ordering is elflift 32%, `ecv-split` 24%, internalize 15%, `llvm-link` 13%, namespace-object 9%, and codegen 6%. Reprice work after every bottleneck moves.
- Runtime variance is evidence. Timestamping and removal localized table rebuilding and arena copies; buffer swapping removed the measured 37 ms switch cost.
- When the guest clock is under test, `web/bin/run.ts --stamp` timestamps guest markers from the host. A frozen-clock probe reporting 0 ns/call inside and 134 ns/call outside proves the two clocks are independent.
- A regression guard counts only after deliberate behavioral neutralization. Compilation failure is not neutralization.
- Fork tests must create divergent state, compiler-folded constants do not prove a guest instruction ran, and equal-size cache fixtures can hide unstable membership.
- The upstream OpenSSL slow arm cannot be green within its deadline because it performs one unsplit serial compile. The ecvisor arm is the shipping-path gate.
- `README.md` is canonical architecture; `AGENTS.md`, `QUALITY_GATE.md`, `TODO.md`, JOURNAL, and LTM have distinct durable roles.
- A green E2E result is incomplete without test names and skip counts. Missing `RAPTORMARK_NODE` once hid ten tests, and a `go test -run` pattern that matches nothing still returns green.
- For runtime-only work, compare builders with identical fused ELF, lifted object, `BaseID`, and `TranslateSH`, changing only `libecvisor.a`. This turns a one-variable claim into an artifact property and reduces the cycle to relinking.

## Operational Guidance

Re-verify TODOs against the tree. Choose the cheapest check that observes the
boundary, ask how it could pass if the claim were false, and neutralize guards
before citing them.

For performance work, measure inputs and live processes, keep every round,
localize to a statement, and refute by removal. A count passed into trace lifting
is not a count of emitted functions. Prefer relinking when runtime-only changes
leave lifted objects unchanged.

Use the exact E2E environment in `.agents/docs/QUALITY_GATE.md`. On this host,
Node is installed but absent from the non-interactive `PATH`; browser work also
needs its bin directory on `PATH` for `npx`. Rebuild
`web/dist/raptormark.js` before browser E2E and verify the bundle freshness
guard.

Scope every Docker stop or removal to an explicit id, name, or ancestor. An
unfiltered kill stopped the user's k3d and application stacks. Also,
`timeout docker run` can kill only the client while the daemon-owned container
continues; name the container and stop it explicitly after the completion marker.

## Files

- `README.md`, `AGENTS.md`: Architecture and mandatory rules.
- `.agents/docs/QUALITY_GATE.md`: Verification contract.
- `.agents/docs/TODO.md`: Re-verifiable open work.
- `.agents/docs/JOURNAL.md`, `.agents/docs/LTM/`: Episodic and durable memory.
- `.agents-workspace/tmp/`: Required temporary-artifact location.

## Tests

```sh
gofmt -l <changed-go-files>
go build ./...
go vet ./...
go test ./...
cargo fmt --manifest-path runtime/Cargo.toml --check
cargo test --manifest-path runtime/Cargo.toml
cargo check --manifest-path runtime/Cargo.toml --target wasm32-wasip1
rg -n --pcre2 '[\x{3099}\x{309A}]' --glob '*.md' .
```

E2E tests remain environment-gated. Host Rust tests, the shipping-target check,
and runtime E2E behavior are complementary evidence.

For E2E commands, always set an explicit builder and persistent object cache,
and compare the pass/fail/skip tuple with `QUALITY_GATE.md`. Do not copy the
current count into another document.

## Consolidated Update: Stale claims and how they are actually found (2026-08-18)

One session hit five instances of a claim that described something already
changed. They share a cause and a cure, and neither is "read more carefully".

**The tree moves faster than the documents describing it.** Expected in a project
reconstructed from a Docker layer and rebuilt by successive agents. In one
session: a prebuilt tools binary predating its source; a timing corpus holding
two pipelines; two README roadmap items describing work that already existed
behind opt-in switches; and a 149-line scheduler whose header assumed a cost
profile that later work had replaced.

**Every one was found by trying to ACT on the claim and touching the code it
named** -- typically a grep taking under two minutes for the mechanism the item
said was missing. None would have been found by re-reading the claim, because the
claims were not wrong about what should happen; they were stale about what
already had. Before pricing or planning against a documented number, go read the
mechanism it names.

**A stale artifact can produce a "the fix works" conclusion.** The existing rule
("rebuild tools after library changes") is written for the failure that looks
like a broken fix. The mirror is worse and nobody re-checks it: `builder/`'s
pipeline is a PREBUILT binary that `raptormark build-image` rebuilds and a raw
`docker build` does not, so a codegen change never shipped while a full cold
re-translation appeared to prove it had. Verify a pipeline change on the emitted
ARTIFACT (`llvm-objdump -r`, `llvm-nm`), never on labels or cache behaviour.

**Corroborating signals are not corroboration when they share a cause.** Three
independent-looking checks agreed the change was in -- a moved cache-identity
label, a genuinely cold 20-minute re-translation, a differing archive -- and all
three were downstream of "the source changed", none of "the binary changed".
Before treating agreement as evidence, ask what each signal is actually
downstream of.

**An aggregate over a mixed corpus measures neither regime.** 124 `timing.json`
files span two pipelines, 66 files to 44, with nothing in a row announcing which;
the discriminator sits in a field the aggregation read and discarded. Averaging
them produced a confident, actionable, wrong number for a path nobody runs.
Before quoting an aggregate, state what must be true of every row for it to mean
anything, then check that.

**A cache identity is only as good as the guarantee that the artifact was built
from what it hashes.** Hashing sources rather than binaries is correct and
deliberate -- it avoids discarding hours of cache on an unrelated toolchain bump --
but it assumes the build path always rebuilds from those sources. Any build path
that does not must be treated as outside the identity's guarantee.

## Pitfalls

- Do not average away variance or rerun until a favorable number appears.
- Completed outputs hide the slow partitions that have not finished.
- Inherited identical state can make a broken sharing test pass.
- Do not blame the upstream OpenSSL deadline on transient host load.
- Use a real PRNG for synthetic incompressibility and real encodings for masks.
- Rebuild tools after library changes -- in BOTH directions: a stale binary can make a fix look broken, and can equally make an absent fix look shipped. Never overwrite or clean shared state without authorization.
## Refresh: Hot Paths, Reachability, and Controls

A feature is genuinely opt-in only when the default artifact retains the original hot path; predictable runtime gates still cost on an interpreted module. Patch 0060's earlier microbenchmark benefit did not transfer to Python, while a C-loop control on the same machine still resolved the expected effect.

An unchanged count is often the only sign that a green check never reached its subject. Pin expected test names, fixture encodings, affected-function counts, or generated artifacts. Controls make negative results meaningful: reproduce a failure before a patch, keep unrelated inventory counts fixed, and use byte-identical unaffected outputs to bound no-op claims.

Mechanism witnesses are required for silent failures. Cache tests count network
fetches, descriptor tests prove independent positions, reverse-DNS tests assert
the recovered hostname, and short-read tests assert bytes actually received.
Checking only the final value would let each original defect pass.
