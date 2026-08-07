---
name: distill-memories
description: Read `.agents/docs/JOURNAL.md` and `.agents/docs/LTM/` documents, find durable knowledge that belongs in the canonical docs, and update those documents with concise synthesized findings. Targets are the root `README.md` (canonical architecture and status), `AGENTS.md` (durable rules), and `.agents/docs/QUALITY_GATE.md`. Use when journal or LTM notes have accumulated implementation, system, or workflow knowledge that should be promoted into the project's canonical documentation.
user-invocable: true
allowed-tools: Bash, Read, Write, Edit, Grep, Glob
---

# Distill Memories

## Overview

Promote durable facts from `.agents/docs/JOURNAL.md` and `.agents/docs/LTM/` into the project's canonical documentation, keeping these up to date:

a. `README.md` (repository root) ... the canonical, human-reader-ready document: goal and scope, the four pipeline stages, the ecvisor runtime, repository layout, building and running, and the honest Status section (what works, what does not, what is next). raptormark has no separate `ARCHITECTURE.md` — `README.md` carries both the orientation and the architecture register.
b. `AGENTS.md` (repository root) ... durable **rules**: constraints a future agent must not violate, environment facts, and workflow protocol. A pitfall that generalizes into "never do X" or "always check Y" belongs here, not in prose.
c. `.agents/docs/QUALITY_GATE.md` ... the verification gate. Update when a change alters what must be run before declaring work complete, or adds a guard worth naming.

## Sources: JOURNAL and LTM

- `.agents/docs/JOURNAL.md` is the append-only log of findings, insights, and code-review history. It holds the **freshest** durable facts, including changes not yet consolidated into LTM. The `good-sleep` / `reconcile-journal-ltm` skills consolidate JOURNAL entries into `.agents/docs/LTM/`; distill runs against whatever is present.
- `.agents/docs/LTM/` holds the consolidated, topic-organized reference material (`INDEX.md` is its table of contents).

Read both. When JOURNAL and LTM overlap, treat LTM as the settled synthesis and JOURNAL as the source of anything newer than the last consolidation. If JOURNAL records a substantial code change that the target docs do not yet reflect, that is exactly the gap this skill closes.

## Read in This Order

1. Read `README.md` and `AGENTS.md` first.
2. Read `.agents/docs/JOURNAL.md` (at least the entries since the last consolidation record) and `.agents/docs/LTM/INDEX.md`.
3. Open only the LTM documents that look relevant to the gaps, stale sections, or missing detail you identified in the target docs.

Do not bulk-load every LTM file unless the set is still small enough that selective reading costs more than reading them all.

The source of truth for any concrete detail is always the code (`cmd/raptormark/main.go`, `internal/**`, `runtime/src/**`, `builder/*.sh`, `patches/`). Verify a fact against the tree before writing it into `README.md`; JOURNAL/LTM point you at what changed, not at the exact current flag, env var, section name, or measured number. This matters more here than in most projects: much of the tree was reconstructed, so a journal entry may describe an intent that the code never reached.

## Classify Findings Before Editing

For each candidate fact, decide whether it belongs in:

- `README.md`: pipeline structure and stage boundaries, the `internal/` <-> builder-image split, the ecvisor interception layering, the `EcvProgram`/`.ecv.*` contracts, the CLI surface, the object-cache identity, honest capability claims and their evidence, measured costs and size ratios, and the ordered "Next" list. Keep it human-reader-ready — narrative and durable rationale over fine-grained implementation trivia.
- `AGENTS.md`: rules and environment facts. Anything phrased as a prohibition, a required check, or a fact about the machine (toolchain paths, shell behavior, what must not be pruned).
- `.agents/docs/QUALITY_GATE.md`: what to run and what a pass looks like, including neutralization of new guards.
- More than one: a single change often lands in two places (e.g. a new emitted `.ecv.*` table belongs in `README.md`'s fusing section, and its producer/consumer invariant may belong in `AGENTS.md`).
- None of them: narrow bug history, one-off recovery archaeology, or details too fine-grained for canonical docs. Those stay in JOURNAL/LTM.

Prefer durable knowledge over incident history. Convert timelines into timeless guidance.

## Update Strategy

When updating the target docs:

- Synthesize; do not copy JOURNAL/LTM prose verbatim.
- Merge into existing sections when possible instead of appending random new sections.
- Add a new section only when the information represents a stable topic that the current document truly lacks.
- Keep summaries compact. Core docs should stay easier to scan than the underlying notes.
- Preserve exact file paths, symbol names, section names (`.ecv.init`, `.got.l0`), env vars (`RAPTORMARK_OBJECT_CACHE`), and flag names when they help precision.
- `README.md`'s Status section is a **claim about verified behavior**. Only move an item into "Working today" when the journal records an end-to-end run that demonstrates it, and keep the qualifier that names the evidence (which fixture, which runtime).
- Measured numbers (build times, expansion ratios, function counts) carry their measurement conditions or they are worthless. Keep the condition when you keep the number.
- If multiple sources disagree, or JOURNAL and the current code disagree, call out the ambiguity or stop and ask the user before cementing one interpretation. JOURNAL marks reversals with **CORRECTION** / **SUPERSEDED** — honor the later entry, and do not resurrect a claim that a later entry retracted.

## Editing Heuristics

- Favor architectural patterns over patch-level history.
- Favor current subsystem boundaries over implementation anecdotes.
- Omit test names unless they explain an architectural guarantee or an important invariant (`TestWasmOptEnablesNoProposal` earns its mention because it guards a shim-compatibility constraint).
- Omit details that are already covered well in the target document.
- Fix obvious typos or stale wording in the touched sections if doing so improves clarity.

## Validation

Before finishing:

1. Re-read the edited sections of every document you changed.
2. Check that each added fact is supported by a source note (JOURNAL/LTM) **and** verified against the tree.
3. Check that orientation-level material did not leak into deep implementation detail, that rules did not leak into `README.md` prose, and vice versa.
4. Keep the documentation style rules in `AGENTS.md`: half-width parentheses and half-width colons, no decomposed Japanese.
5. If you changed a mermaid diagram in `README.md`, check that it still parses — an unbalanced quote or a stray `<br/>` silently breaks the whole block on GitHub.
