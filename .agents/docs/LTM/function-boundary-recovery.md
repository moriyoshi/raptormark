# Function Boundary Recovery

## Summary

The lifter needs trustworthy function extents. Fused stripped libraries lose most local symbols, so raptormark combines symbol tables with `.eh_frame`, init/fini entries, PLT ranges, and computed code pointers while refusing to claim ranges that are not genuinely bounded.

## Key Facts

- Missing boundaries are loud; overly broad boundaries silently turn data into code and hide real entries.
- `.dynsym` covers only exports in stripped libraries and can leave most executable bytes undescribed.
- `.eh_frame` FDEs recover exact extents for most compiler-generated functions on glibc.
- musl may have almost no useful `.eh_frame`; DT_INIT/DT_FINI and computed pointers matter there.
- A recovered start already inside a sized symbol must not split that symbol.
- Hand-written assembly can have neither symbols nor CFI, so lifter hardening remains necessary.
- `.ecv.funcs` is currently an unused, incomplete inventory, not a safe restrictive keep-list.

## Details

The nginx investigation localized a bogus far branch to data decoded inside a synthetic function spanning an unsymbolized region of stripped libcrypto. `internal/fuse/ehframe.go` parses the CIE/FDE chain and emits local `FUNC` symbols for non-overlapping recovered ranges. It supports the DWARF pointer encodings observed in real toolchains and rejects relative bases it cannot compute safely.

This substantially raised executable-byte coverage, but it did not describe OpenSSL hand-written assembly. That distinction matters: boundary recovery reduces unknown regions but cannot invent facts missing from the binary. Patches that clamp synthetic functions or skip out-of-section targets are robustness measures, not proof that an arbitrary input is fully liftable.

Further sources close musl-specific gaps: DT_INIT and DT_FINI identify entrypoints even when sections are absent, while relocation targets and other computed code pointers reveal additional starts. `initStubs` once bounded an init entry by an entire `.text` section; that over-claim would hide every true function inside it and was corrected to use a real bound.

The fuser emits `.ecv.funcs`, but neither the lifter nor the patch series consumes
it. On the fused PostgreSQL image it is 942 KB of `SHF_ALLOC` payload and lists
6,151 functions where elflift discovers 6,570; the missing 419 include PLT stubs
and gap-filling rest functions. Using it restrictively would therefore send real
entries to `_ecv_unreached`.

Per-program reachability pruning also conflicts with library-scoped object reuse:
the reusable unit depends on one library producing the same lifted set in every
program. Pruning may still be valid inside the executable's own range, but a
library keep-list cannot depend on which executable happens to reference it.
Dropping or safely repurposing `.ecv.funcs` is tracked in `TODO.md`.

## Files

- `internal/fuse/ehframe.go`: FDE parser and boundary synthesis.
- `internal/fuse/`: Symbol merging and other boundary sources.
- `patches/`: Lifter decoder and boundary-hardening changes.

## Test Coverage

Parser tests use constructed `.eh_frame` data and optionally compare a real library against `readelf`. Regression coverage includes stripped OpenSSL, glibc, and musl inputs. Instruction masks are tested against real `objdump` encodings.

## Pitfalls

- Never infer that a larger claimed range is safer.
- Do not use nginx as the sole baseline; first compare surviving known-good fixtures.
- `readelf -SW` token positions shift with section-number width; locate columns relative to a known token.

## Consolidated Update: Musl Pointer-Derived Boundaries

Redis's stripped musl libc demonstrated two additional boundary sources. `storedAsPointer` recognizes when a materialized address is the value operand of an AArch64 store; its masks are pinned to assembler-produced encodings. `relocPointerFuncs` reads RELR and `R_AARCH64_RELATIVE` targets from original section contents before rebasing obscures them.

Every added start must shorten the synthetic function that previously enclosed it. These rules raised busybox's synthetic boundary count from 252 to 469 and Redis's from 7,781 to 7,830, while a glibc Python closure remained byte-identical.
