# Ruby, JIT Scope, and Jump-Table Bring-Up

## Summary

`ruby:3-slim` failed because a PAC-signed function's compact jump table was not recovered, not because of TLS or YJIT. Patch 0062 fixes the gap. **Adoption is DONE, verified 2026-08-22 on the image**: `raptormark-elfconv-base-patched:sisd0065`, the base every builder in use layers onto, carries 0062 through 0065. Only the project's JIT scope remains a policy decision. Check the CONDITION rather than the identifier when confirming this -- `entry_is_landing_pad` PREDATES 0062, which merely removes it from one gate, so grepping the name reports "adopted" on an unpatched base; the post-patch form reads `if (br_bb && seeded == 0 && entry_read) {`.

## Key Facts

- Ruby runs with native-oracle checksums after patch 0062.
- Mixed branch protection makes a binary-wide PAC/BTI classification invalid per function.
- YJIT is compiled in but disabled by default, so argv can move one image across the AOT boundary.
- Ruby handles the refused 384 MiB mapping as `ENOMEM`; it was not the crash.

## Details

The missed landing pad in `rb_method_definition_set` fell through the catch-all and re-entered through `__remill_jump`. Re-entry with `sp == 0` made the prologue store at `0xffffffc0`, exactly matching the trap. Patch 0025 already had the symbol-table sweep, but `entry_is_landing_pad` rejected a function beginning with `paciasp` even though `seeded == 0` showed that BTI discovery found nothing. Patch 0062 drops that proxy.

Validation used assembly because four C fixtures failed to place `paciasp` at the literal entry. Ruby passes; Python retains checksums with a 20-byte module delta; nginx is byte-identical; PostgreSQL has zero affected functions. The attempted `--yjit` probe died before the jump-table fix and establishes nothing about JIT execution.

## Files

- `patches/0062-jump-table-sweep-not-gated-on-pac.patch`: Lifter fix.
- `e2e/pacjumptable_test.go`: Assembly regression guard.
- `.agents-workspace/fixtures/rbbench/`: Preserved investigation artifacts.
- `.agents-workspace/drivers/wasmnames.py`: Trap-index resolver.

## Test Coverage

The PAC jump-table guard fails on the pre-patch builder. Cross-guest validation bounds the blast radius, and nginx byte identity is the strongest no-op control.

## Pitfalls

- PAC at entry does not prove BTI discovery covered the function.
- An E2E corpus with zero PAC-signed entries cannot validate patch 0062.
- Do not revive the refuted TLS hypothesis.
- Distinguish the two measured pre-codegen YJIT walls from execution of generated code.
- Adopting a lifter patch changes `BaseID` and invalidates translated objects.

## Consolidated Update: Ruby Startup and the YJIT Boundary

Ruby reaches `PR_SET_THP_DISABLE` on every startup and `PR_SET_VMA_ANON_NAME` for GC heap pages. Ecvisor now accepts the supported forms, validates guest ranges, and stores names for overlapping mappings. Ruby's 384 MiB startup reservation comes from `Init_default_shapes` sizing its red-black shape cache; the equality with `MEMORY_ARENA_SIZE` is arithmetic, and the normal interpreter handles the resulting `ENOMEM`.

YJIT has now been measured, but it dies before emitting code in two independent ways. Any `--yjit*` argv spelling reaches an undecoded ASIMD immediate `orr` in `proc_options` and exits via SIGILL. Enabling YJIT without that argv path reaches a 128 MiB `PROT_NONE` reservation, which cannot fit in ecvisor's 96 MiB private mmap window. Smaller execution-memory options still traverse the undecoded argv path. These results do not test W^X or execution of generated instructions; they bound the work required to reach those questions.
