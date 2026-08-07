# Image Discovery and Rootfs

## Summary

`internal/image` implements host-side container discovery, while `internal/rootfs` produces the compressed sidecar consumed by ecvisor's read-only filesystem. Both packages were reconstructed from downstream contracts and verified against real glibc and musl images.

## Key Facts

- Discovery and fusing run on the host; translation and linking run in the builder image.
- The selected policy is an entrypoint-reachable executable closure, not every executable in the image.
- Scripts are data, not registry programs. Their interpreters and invoked commands expand the closure.
- PIE executables and shared objects are distinguished using ELF type plus `PT_INTERP`.
- `docker export` provides a flattened rootfs with layer whiteouts already resolved.
- The RFS sidecar is necessary for all guest file access and must contain generated `/.raptormark/*` metadata too.

## Details

`Inspect` reads image configuration, `ExportRootfs` flattens the image, `Scan` inventories aarch64 ELF files and shebang scripts, and `Closure` expands from the configured entrypoint and command. Static exec targets cannot be known perfectly, so closure discovery combines embedded NUL-terminated absolute paths with script command-word resolution against `PATH`. `Extra` and `Exclude` correct the resulting over-approximation.

**Updated 2026-08-27: there is a THIRD mechanism, and the two above were not enough.** Shell scripts are now PARSED (`internal/image/shellscan.go`, `mvdan.cc/sh/v3/syntax`), because the NUL-termination requirement means `absolutePaths` never fires on a script -- a script is text -- so **no literal absolute path in a script was ever followed**. `nginx:alpine` reaches its four `/docker-entrypoint.d/` scripts only through `$f`, and its closure was missing `/usr/bin/envsubst`, a real 67 KB ELF the entrypoint execs. The parse contributes literal paths, and a directory fan-out for the `for f in DIR/*; do . "$f"; done` idiom. It is unioned with the unchanged bare-word pass, so it can only ADD. `Closure` also returns `[]Unresolved`: the references it could not resolve, with the commands inside any `$(…)`, so "which tool would we have to emulate" is measured rather than guessed. See TODO.md and JOURNAL.md, 2026-08-26/27.

**Also updated 2026-08-27: the ENTRY was `seeds[0]`, and a real image's ENTRYPOINT is a shell script, which is never a closure member.** `raptormark build <image>` therefore failed at discovery for almost every real image. `pipeline.EntryFromSeeds` now takes the first seed that is a program in the closure.

Symlink resolution must preserve the container namespace. Both a symlinked entrypoint and absolute symlink targets can otherwise escape or resolve relative to the host extraction directory. The OpenSSL fixture provides a cheap regression case for this path.

`internal/rootfs` writes the format parsed by `runtime/src/vfs/rfs.rs`. The sidecar is not optional: without it, ecvisor starts but no file provider exists. Large regular files may be compressed; tests must use a real PRNG when they need incompressible data.

## Files

- `internal/image/`: Image inspection, export, scan, and closure selection.
- `internal/rootfs/`: RFS sidecar producer.
- `runtime/src/vfs/rfs.rs`: Consumer and format authority.
- `e2e/`: Real-image discovery and end-to-end coverage.

## Test Coverage

Host tests cover shebangs, `/usr/bin/env`, closure limits, shared-object exclusion, symlinks, and rootfs encoding. `TestOpenSSLFixtureDiscoverAndFuse` exercises the real OpenSSL image quickly; slow E2E coverage takes it through Wasm execution.

## Pitfalls

- A spare closure member costs build time; a missing member becomes a late `execve` failure.
- Closure admission limits must apply to interpreters and script-discovered commands, not only the initial scan.
- glibc and musl images have materially different layouts and require separate fixtures.

## Consolidated Update: Runtime-Loaded Plugins

`fuse.Options.Extra` is now wired from `image.PluginDirs`. Mandatory dependencies are resolved first; each optional plugin is then walked independently with rollback of objects and dedup maps on failure. `Report.SkippedExtras` makes omissions visible. Partial admission is unsafe because runtime `dlopen` returns a sentinel even when later symbol lookup will fail.

❌ **CORRECTION 2026-08-27: "`Report.SkippedExtras` makes omissions visible" is TRUE OF `FuseClosure` AND DOES NOT PROTECT A BUILD.** `internal/pipeline.build` never calls `FuseClosure`; it calls `FuseWithUnits` for the entry and `Fuse` for every other program, and both turn `load`'s `SkippedExtra` into a FATAL error, deliberately. So an unsatisfiable plugin fails the whole build rather than being reported: `raptormark build python:3-slim` dies on `_tkinter`, which DT_NEEDEDs `libtk8.6.so` that the image does not ship (verified -- `import tkinter` fails in the stock container too). The same false reassurance was in `internal/image/plugins.go`'s own doc comment and has been corrected there. Open in TODO.md, where the fix is a placement decision rather than a mechanism.

Plugin discovery uses a named list for Python extension directories and OpenSSL module/engine directories. It deliberately does not scan every unreferenced shared object, which would collect roughly 250 gconv and NSS objects. A plugin-heavy closure can exceed the closure-wide shared-layout window and fall back to per-image packing, losing cross-program library sharing.

## Consolidated Update: Directory Symlinks

Merged-usr images need directory symlinks represented separately from executable symlinks. `Inventory.DirLinks` records links such as `/bin -> usr/bin`; `canon` rewrites the longest matching prefix at a component boundary before retrying lookup. Tests must use an unknown subject whose incorrect rewrite resolves, because an exact program hit or another missing path hides the defect.
