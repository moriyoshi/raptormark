# Recovery and Builder Provenance

## Summary

The repository was reconstructed after the 2026-08-01 working-tree loss from local Docker images, BuildKit snapshots, and surviving source artifacts. This document records which components are recovered evidence, which are reconstructed designs, and which builder identities remain load-bearing.

## Key Facts

- `runtime/`, `patches/`, `builder/Dockerfile`, the original builder helper scripts, and `builder/ecv-promote.cpp` were recovered from images or BuildKit snapshots.
- `internal/` and the original image-build driver were reconstructed. Their behavior must not be treated as recovered unless independently pinned by a runtime consumer or artifact.
- `third_party/elfconv` stays clean at commit `8bfe808`; the fork is the ordered series under `patches/`.
- `raptormark.base_id` is the base image ID and `raptormark.translate_sh` is the SHA-256 of the translation driver. Both participate in translation cache identity.
- `raptormark-builder:latest` is stale by design. Side builds use explicit tags and do not update `latest`.
- Builder images and BuildKit cache include artifacts that cannot be reproduced from the current tree. Never prune Docker globally.
- ⚠️ **SUPERSEDED BY EVENT, 2026-08-23: the images described here are GONE.** On the operator's explicit instruction, after being shown this rule and its consequence, **307 `raptormark-builder` tags and 233 `raptormark-elfconv-base*` images were removed**, including `latest`, the pinned line, and the whole `llvm22` line. Two survived only because running containers referenced them: `raptormark-builder:fsync` and `raptormark-elfconv-base:8bfe80860118`. Read everything below as the provenance HISTORY it is, not as a description of this machine. Rebuilding needs `raptormark build-image`, which re-applies the `patches/` series to the clean pin; the resulting base id will very likely differ, so `.agents-workspace/objcache` (6.2 GB, keyed on `BASE_ID`) misses on every entry and every closure re-translates cold. The BuildKit cache is the remaining source of byte-sensitive recovery evidence and is now the ONLY thing standing between this tree and a from-scratch base. See `JOURNAL.md`, "ALL raptormark Docker images REMOVED".

## Details

❗ **The recovery evidence under `_recovery/` is GONE** -- confirmed absent and unrecoverable by the user on 2026-08-25. It held `_recovery/RECOVERY.md` and `_recovery/reference/`. It was gitignored and never tracked, so the loss cannot be dated or explained, and a reference to it in any older document is not evidence that it survived. What follows describes what that evidence ESTABLISHED, which stands; the evidence itself can no longer be re-read. The patched elfconv tree was validated by applying the recovered patch series to the clean pin and comparing every resulting source file with the copy extracted from the builder image. A BuildKit cache hit on the reconstructed patch-application layers provided an additional byte-sensitive check of the recovered recipe.

The original `builder/build-image.sh`, `.dockerignore`, and `.gitmodules` were lost. Their replacements were inferred from Docker history, labels, Dockerfile comments, and upstream metadata. Later, the shell entrypoints were ported into `cmd/raptormark`; their CLI is maintained with `github.com/alecthomas/kong`.

Builder labels must describe the image actually used. `internal/translate.BuilderFromImage` reads them back rather than trusting caller-supplied identity. A lifter or translation-driver change must rotate the object key; a runtime-only change must not.

Historical image tags document successive experimental states, but `latest` does not mean newest. Pipeline and E2E commands should set `RAPTORMARK_BUILDER=raptormark-builder:<tag>` explicitly.

## Files

- `builder/Dockerfile`: Recovered builder recipe and image labels.
- `cmd/raptormark/`: Go implementations of builder commands.
- `patches/`: Ordered elfconv fork patches.
- `third_party/elfconv/`: Clean pinned submodule.
- `_recovery/`: ❗ GONE as of 2026-08-25 (see Details). Held untracked recovery evidence. Its exclusions in `.gitignore`, `.bazelignore` and `BUILD.bazel` are left in place deliberately.

## Test Coverage

The reconstructed toolchain was exercised from a real aarch64 ELF through lifting and Wasm execution. The normal Go gate covers the Go command ports; env-gated E2E tests exercise explicitly selected builder images.

## Pitfalls

- Do not edit `third_party/elfconv`; add or update a patch.
- Do not use `raptormark-builder:latest` for current behavior.
- Do not run `docker system prune` or `docker buildx prune`.
- A builder image captures the shared working tree at build time, including unrelated uncommitted edits.

## Consolidated Update: Builder and Patch State

The target-enablement close-out records 58 ordered patches applying to the clean submodule pin. Patch 0050 indexes BTI discovery; patches 0051-0058 add range lifting, instruction coverage, and undecoded-instruction reporting. All were built as explicit side tags, with `raptormark-builder:latest` left at its 2026-07-29 state.

Neutralization tags `nA`, `nB`, and `nC` contain deliberately broken runtimes for thread TLS, fd-table sharing, and arena sharing. They are evidence artifacts, not builders for ordinary runs. Builder labels and cache identities must be computed by a freshly rebuilt host CLI; a stale binary once labeled a current image from a nine-hour-old source list.

## Consolidated Update: Current Docker State (2026-08-23)

On the operator's explicit instruction, 307 `raptormark-builder:*`, 233 elfconv-base, and 50 temporary E2E images were removed. One builder and one patched base survived only because running user containers referenced them; they must not be assumed suitable for development. Non-raptormark images and small guest fixtures were left alone.

The repository source remains. ⚠️ The `_recovery/` evidence this sentence called "preserved" does NOT -- see Details; only the source survives. A new `build-image` can recreate a toolchain, not restore the old image IDs. If the patched base ID changes, the 6.2 GB object cache misses and closures must translate cold; the LLVM 22 experimental artifacts are also gone. Treat historical tag lists as provenance, not current inventory, and verify the actual Docker state before planning a lift or link.
