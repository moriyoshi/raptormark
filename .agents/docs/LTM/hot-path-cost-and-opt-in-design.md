# Hot-Path Cost and Opt-In Design

## Summary

What a wasm-interpreted hot path actually costs, and what it takes to add a
feature to one without taxing everybody who does not use it. Drawn from the
inlined-call-history work (elfconv patch 0060) and the phase-0 scheduler
measurements, but the findings are about the shape of the problem rather than
that feature.

## Key Facts

- On wasmedge's interpreter, runtime cost tracks EMITTED INSTRUCTION COUNT
  roughly 1:1. A 20% larger module measured 18% slower.
- There is no such thing as a cheap check on a path that runs millions of times.
  Four predictable branches per guest BL measured **~10%**; a single `if flag`
  guard on the same path measured 6.5%.
- A feature is only opt-in if the default path is the code it always was. Not
  "equivalent code" — the same code.
- Build cost follows the largest single function, and that rule applies BETWEEN
  closures, not only between toy guests and closures. Two closures in one run
  measured +39% and +111% for the same change.

## Details

### Measuring a hot path

The scheduler's per-call bookkeeping was three calls into the supervisor per
guest BL: push the call history, pop it at the epilogue, test a suspend flag.
Counted statically on a linked bash module: 32,972 / 32,115 / 33,540 sites in
7,567 lifted functions, about 13 per function.

Removing them is not uniformly valuable. The suspend check — a function whose
whole body was one flag read — was worth **1.8%** on a call-heavy guest and was
NOT RESOLVABLE above run-to-run variation on a realistic one. The two
call-history hooks, whose bodies did real work, were worth ~30% together on the
same call-heavy guest and 4.7% on the realistic one. Crossings are not
interchangeable; the body matters more than the call.

The single most valuable change in that work was not the clever one. Splitting
five diagnostic gates out of the two hooks into a cold path — ~40 lines of Rust,
no lifter change, every cached object reused — bought **19.6%** call-heavy and
3.2% realistic. The lifter change that replaced a call with a wasm global bought
1.8% and 0.26%, cost a full re-translation, and needed a `BaseID` bump.

### Bracketing a benchmark

A call-heavy microbenchmark and a realistic workload bracket the answer; neither
alone is one. The same change measured -19.6% and -3.2% on the two. Reporting
either number by itself would have been wrong in a different direction.

Subtract fixed startup before quoting a percentage. A guest that takes an
argument to skip its own workload gives the floor from the same module, which is
better than estimating it.

### Making a feature genuinely opt-in

Three designs were measured before one was free:

| design | default-path cost |
|---|---|
| new data structure behind accessors | +2.9% |
| new data structure behind an `if enabled` guard | +6.5% |
| `Vec` restored, `if !enabled { return }` guards at the touch points | +7 to +9% |
| **two symbols, chosen at LIFT time** | **0%** |

The working design emits a DIFFERENT SYMBOL when the feature is built in. The
default module calls the original function, byte-identical to before the feature
existed, and carries no evidence the feature is there — so there is nothing to
branch on. Anything that leaves a runtime test on the hot path is not opt-in, it
is a discount.

Proving inertness is a byte comparison, not an argument. Lifting the same fixture
with the patch absent, and with it applied but the flag off, produced bitcode
with the identical SHA-256. That proof is what licensed leaving the object-cache
key unchanged for default builds; without it, a new cache-key input invalidates
every cached object.

### Two gates, and why the build one is not enough

A runtime kill switch is worth having: a module carrying a suspected miscompile
is then one environment variable away from being ruled out rather than a rebuild
away. But a runtime gate ALONE is unsafe, because it can be set against a module
that was never built for it. That combination corrupted state silently.

The fix is a build marker with exactly one definition per module: a WEAK zero in
a C shim, overridden by a STRONG one linked in only when the feature was built.
A strong definition wins; an absent strong definition is not an undefined symbol,
so the default link is unchanged and cannot fail at instantiation. When the
marker is missing, SAY SO — an operator who asked for the gate deserves to know
why it did nothing.

### Invariants held by convention will fail

The feature's correctness required every access to a shared structure to be
bracketed by two calls. Nothing enforced it. Three holes were found in three
different places, and the design was declared complete after the first:

1. a scheduler path that mutated the structure outside the brackets;
2. the runtime gate accepting a module not built for it;
3. bring-up calling into guest code before the first publish.

Each was individually easy to miss. THREE FOUND IS NOT EVIDENCE THERE IS NO
FOURTH. If a design needs this shape, make it structural — a guard type that
acquires on construction and releases on drop, so the unbracketed access does not
compile.

## Pitfalls

- **Diagnostics that disable the thing you are debugging.** Folding "any
  diagnostic armed" into the feature's own enable condition meant arming any
  diagnostic turned the fast path off — so the bug vanished exactly when you
  looked at it. Anticipate this when a correctness condition and a debug gate
  share a flag.
- **A passing suite can prove nothing.** When a feature is an optimisation whose
  slow path is the same behaviour, a run where the gate silently refused passes
  identically to one where it worked. Assert the plumbing directly, and check for
  the "ignored" diagnostic's ABSENCE rather than trusting the pass tally.
- **`wasmedge` does not inherit the host environment.** A gate-on measurement
  showed no speedup at all because the variable was never passed with `--env`.
- **Do not extrapolate build cost between closures.** +39% on one predicted +111%
  on another badly.
- **A budget timeout is not a measurement.** A test killed at its ceiling gives a
  FLOOR. Raise the ceiling before quoting a number; `RAPTORMARK_E2E_BUDGET`
  exists for that, and `go test -timeout` must be raised alongside it.
- **`pkill -f <pattern>` can match its own shell.** A pattern containing the
  command being run killed the tool shell issuing it, reported as exit 144.
- **Piping a long run through `tail` discards the failure and the exit code.**
  The end of a Go test run is the summary, not the diagnosis, and a pipeline's
  status is its last stage.

## Files

- `runtime/src/intrinsics.rs`: the per-call hooks and their cold paths.
- `runtime/src/context.rs`: the shared structure, its gate, and the
  publish/adopt bracket.
- `runtime/cshim/ecv_globals.c`: wasm globals and the weak build marker; Rust
  cannot express either.
- `internal/builder/linkall.go`: the strong marker TU, linked only on opt-in.
- `.agents/docs/MULTIMODULE.md`: where the per-crossing numbers feed the
  module-split analysis.

## Consolidated Update: Patch 0060 on a Real Interpreter

The earlier 23% call-history result did not transfer to two Python workloads: +0.5% and -0.6%, with overlapping bands. A C-loop control on the same machine and hour still resolved -21.4%, proving the harness could observe the effect. Patch 0060 is therefore a keep/drop decision whose proposed interpreter beneficiary did not benefit.
