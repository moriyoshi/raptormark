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

## Details

The recovery evidence lives under `_recovery/`. The patched elfconv tree was validated by applying the recovered patch series to the clean pin and comparing every resulting source file with the copy extracted from the builder image. A BuildKit cache hit on the reconstructed patch-application layers provided an additional byte-sensitive check of the recovered recipe.

The original `builder/build-image.sh`, `.dockerignore`, and `.gitmodules` were lost. Their replacements were inferred from Docker history, labels, Dockerfile comments, and upstream metadata. Later, the shell entrypoints were ported into `cmd/raptormark`; their CLI is maintained with `github.com/alecthomas/kong`.

Builder labels must describe the image actually used. `internal/translate.BuilderFromImage` reads them back rather than trusting caller-supplied identity. A lifter or translation-driver change must rotate the object key; a runtime-only change must not.

Historical image tags document successive experimental states, but `latest` does not mean newest. Pipeline and E2E commands should set `RAPTORMARK_BUILDER=raptormark-builder:<tag>` explicitly.

## Files

- `builder/Dockerfile`: Recovered builder recipe and image labels.
- `cmd/raptormark/`: Go implementations of builder commands.
- `patches/`: Ordered elfconv fork patches.
- `third_party/elfconv/`: Clean pinned submodule.
- `_recovery/`: Untracked recovery evidence; preserve it.

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
