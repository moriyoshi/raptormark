---
name: tackle-todos
description: "Read TODO.md and scan source code for TODO/FIXME comments, build a consolidated list, then dispatch parallel agents to address as many items as possible."
user-invocable: true
allowed-tools: Bash, Read, Write, Edit, Grep, Glob, Agent
---

# Tackle TODOs: Consolidate and Resolve Open Items

This skill scans the project for all outstanding work items — both from `.agents/docs/TODO.md` and from `// TODO` / `// FIXME` comments in source code — builds a consolidated, deduplicated, prioritized list, and then dispatches parallel agents to resolve as many items as possible.

**Use this skill when:** you want to make a focused sweep of outstanding TODOs and fix them in bulk.

## Arguments

- `[filter]` (optional): A package name or keyword to restrict which TODOs to tackle (e.g., `registry`, `builder`, `deploy`). If omitted, all TODOs are considered.

## Step 0: Collect TODOs from TODO.md

Read `.agents/docs/TODO.md` and extract every unchecked `- [ ]` item. Record its area, description, and source.

## Step 1: Scan Source Code for TODO/FIXME Comments

Use Grep to search the repo for `// TODO` and `// FIXME` patterns in `*.go` and `*.rs` files, and for `# TODO` / `# FIXME` in `builder/*.sh` and `patches/*.patch`. Exclude `third_party/` — that is an upstream submodule pinned clean, and its TODOs are not ours.

Group and deduplicate the results:
- Identical or near-identical comments repeated across many files should be collapsed into a single work item with a note about which files/packages are affected.
- Comments that are informational-only (e.g., noting a known limitation that cannot be fixed without large design changes) should be flagged but deprioritized.

## Step 2: Build a Consolidated TODO List

Merge the two sources into a single list. Deduplicate items that appear in both TODO.md and as code comments.

For each item, assign a category:

| Category | Description | Priority |
|----------|-------------|----------|
| **systematic** | Same pattern repeated across many files | High — one fix propagates widely |
| **behavioural** | Missing or incorrect behaviour affecting correctness | High |
| **validation** | Missing input validation or error responses | Medium |
| **serialization** | Wire format, encoding, or protocol bugs | Medium |
| **test-only** | TODO in test code noting a test gap, not a code defect | Low |
| **design** | Requires significant design work or new abstractions | Deferred — flag for user |

Write the consolidated list to `.agents-workspace/tmp/consolidated-todos.md` for reference.

## Step 2b: Verify stale items before dispatch

Before dispatching agents on any TODO entry that is more than 24 hours old, verify the entry is still applicable: grep the relevant code for the symptom (the call site, the missing handler, etc.). Skip or close stale entries instead of dispatching.

## Step 3: Filter (if argument provided)

If the user passed a `[filter]` argument, restrict the work list to items matching that filter (package name or keyword).

## Step 4: Plan Parallel Work Items

Group the consolidated TODOs into independent, parallelizable work units. Each work unit should:
- Be self-contained (touching one package or one cross-cutting concern)
- Not conflict with other parallel work units (no two agents editing the same file)

Present the plan to the user and get confirmation before dispatching.

## Step 5: Dispatch Parallel Agents

For each approved work unit, launch an Agent (subagent_type: general-purpose) with a clear prompt that includes:

1. The specific TODO(s) to address
2. The files to modify
3. The expected behaviour
4. Instructions to run the local gate after making changes: `gofmt -w` on touched files, then `go build ./...`, `go vet ./...`, and the relevant focused tests (`go test ./internal/<pkg>/`).

Launch as many agents in parallel as there are independent work units, in a single batch.

❌ Never use `isolation: worktree` for parallel agents — it has repeatedly caused trouble. Instead, launch tasks that are independent of one another in a batch.

## Step 6: Collect Results and Update TODO.md

After all agents complete:

1. Review each agent's results — check if the change built, vetted, and tests passed.
2. For successfully resolved items, mark them as `- [x]` in `.agents/docs/TODO.md` and remove the corresponding `// TODO` comments from source code.
3. For items that could not be resolved, add notes about why and what was attempted.
4. Append a summary to `.agents/docs/JOURNAL.md` documenting what was tackled and the outcomes.

## Notes

- **Do not attempt design-category items** without user approval. These require architectural decisions.
- Respect `AGENTS.md` rules: no `git checkout`, no `git restore`, no discretionary commits. Agents should only edit files, not commit.
- When running tests, always use focused package targeting (`go test ./internal/<pkg>/`) rather than the whole suite when iterating. The build-engine integration test requires root and will skip otherwise.
