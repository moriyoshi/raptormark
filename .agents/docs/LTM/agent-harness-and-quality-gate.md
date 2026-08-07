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

The harness was adapted from cornus after the recovery journal was moved from the repository root to `.agents/docs/JOURNAL.md`. Source references were updated while `_recovery/RECOVERY.md` remained a distinct evidence file.

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
