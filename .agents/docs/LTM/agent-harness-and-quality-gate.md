# Agent Harness and Quality Gate

## Summary

The repository uses `.agents/docs/` for chronological findings, durable memory, open work, and verification policy. `README.md` remains the single canonical architecture document; agent memory supplements rather than replaces it.

## Key Facts

- `JOURNAL.md` is append-only during ordinary work; reconciliation removes entries only after audited promotion to LTM.
- `LTM/` contains editable topic-organized synthesis.
- `TODO.md` contains open work extracted from evidence and must be re-verified against the tree.
- `QUALITY_GATE.md` defines the Go, Rust, and E2E gates.
- `README.md` is the canonical architecture and honest-status document.
- Shared working-tree changes may belong to another agent.

## Details

The harness was adapted from cornus after the recovery journal was moved from the repository root to `.agents/docs/JOURNAL.md`. Source references were updated while `_recovery/RECOVERY.md` remained a distinct evidence file. ⚠️ That file is GONE with the rest of `_recovery/` (2026-08-25); `.agents/docs/JOURNAL.md` is now the only recovery narrative, and it is a different document that never contained the same material.

The standard Go gate is formatting, build, vet, and tests. The earlier claim that
host Rust tests could not compile became stale: 47 tests now run clean, and the
installed `wasm32-wasip1` target type-checks the shipping configuration. Runtime
behavior still requires E2E coverage; host unit tests and cross-target checking
are complementary rather than substitutes.

This correction matters operationally. While the stale failure baseline was
trusted, runtime changes were not unit-tested at all. Quality-gate claims must be
rechecked against the current tree and toolchain, and documentation must be
updated when a previously unavailable gate becomes green.

Long-term-memory maintenance is staged: `good-sleep` extracts topic documents and TODOs, `deep-sleep` merges overlapping LTM documents, `distill-memories` promotes durable conclusions to canonical docs, and reconciliation audits coverage.

## Files

- `AGENTS.md`: Durable operating rules.
- `README.md`: Canonical architecture and project status.
- `.agents/docs/JOURNAL.md`: Append-only evidence.
- `.agents/docs/TODO.md`: Re-verifiable open work.
- `.agents/docs/QUALITY_GATE.md`: Verification contract.
- `.agents/docs/LTM/`: Topic-organized durable memory.

## Test Coverage

Documentation changes should be scanned for forbidden decomposed kana and full-width punctuation. Code changes follow the language-specific and E2E gates in `QUALITY_GATE.md`.

## Pitfalls

- Keep journal entries append-only during ordinary work; the explicitly invoked
  reconciliation workflow may remove entries after their durable knowledge is
  audited into LTM.
- Do not assume a TODO is still open without checking the current tree.
- Do not commit, restore, or clean shared changes unless explicitly authorized.

## Consolidated Update: Web Quality Gate and Tooling

The web gate runs TypeScript checking, `oxlint`, `oxfmt`, Node tests, and env-gated browser E2E. Go's cache does not observe TypeScript, so wrappers need `-count=1`. Rebuild the generated browser bundle before E2E and use explicit absolute working directories in long shell sessions.
Vitest replaced `node --test`; its explicit include covers `src/**/*.test.ts` and excludes Playwright specs. The web gate now includes 59 unit tests, including service-worker behavior that no longer requires a browser.

The TypeScript conversion, oxc migration, and Vitest migration each exposed an existing gap by making a new class of assertion expressible. Their value is not limited to running more checks.

## Consolidated Update: Environment and Process-Scope Checks

The working E2E environment is recorded only in `.agents/docs/QUALITY_GATE.md`. On this machine, Node is installed through mise but absent from the non-interactive `PATH`; `RAPTORMARK_NODE` must name the binary explicitly, while Playwright also needs that bin directory on `PATH` for `npx`. The object cache must be the existing `.agents-workspace/objcache`, not an absent path that the tool silently creates empty.

Read pass, fail, and skip counts together. A clean run moved from 99 pass / 29 skip to 121 pass / 7 skip chiefly by enabling already-written Node and Chromium coverage. Confirm the emitted test names when using `-run`; a green command that selected nothing is not evidence.

Docker operations require explicit ownership filters. Never pipe an unfiltered container list into `docker kill`. Also, `timeout docker run ...` kills the CLI client, not necessarily the container; identify and stop the container itself. These are user-impacting operational constraints, not mere test hygiene.

## Consolidated Update: Gate Documentation Drift

`QUALITY_GATE.md` is designated by `AGENTS.md` as the single authority for gate
commands and test counts, and both had drifted -- from the tree and from each
other.

❗ **`AGENTS.md` showed a Go gate that skips 36 tests.** `tools/decode-oracle` is
a separate module, and a workspace does NOT make `./...` recursive across
modules: a relative pattern resolves against the module you are standing in. So
both patterns must be named:

```
go test ./... raptormark/tools/decode-oracle/...
```

`AGENTS.md` is what an agent reads first, so the short form was taught and
followed for a whole session. The short form is green, and `go test` prints only
what it ran, so a skipped module does not announce itself.

**A rule that lives in two documents will drift, and the copy that drifts is the
one nobody re-measures.** The fix was to make the second copy point at the
first. That is why this file no longer pins its own test count: `AGENTS.md` once
carried "47 tests, verified 2026-08-13", `QUALITY_GATE` re-measured twice, and
the two disagreed. A count is only useful for spotting a large DROP, which it
cannot do from two files that disagree.

⚠️ **`grep -rc "#\[test\]" runtime/src` over-counts.** It returned 292 against
288 run, which looks precisely like four silently skipped tests. The extra four
are occurrences inside COMMENTS -- comments that exist to explain why `mod sys`
cannot host a test, since it is `#[cfg(target_arch = "wasm32")]`. Counting bare
attribute lines gives exactly 288. The refining command is in `QUALITY_GATE` §2.

⚠️ Readings are kept UNRECONCILED rather than folded into a total when sessions
between them are unaccounted for. That is the entire purpose of keeping them.
