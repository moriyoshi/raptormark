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
- Do not cite the failed `--yjit` run as JIT evidence.
- Adopting a lifter patch changes `BaseID` and invalidates translated objects.
