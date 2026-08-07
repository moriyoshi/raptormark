# QEMU decodetree tables — provenance

Verbatim, unmodified upstream files. This directory follows the same rule as
`third_party/elfconv`: **pinned upstream material, never edited in place.** If a
newer table is wanted, re-vendor at a new pin and update this file. Do not patch
these files; a local fix here would be invisible to every later re-vendor.

Unlike `third_party/elfconv` this is **not a git submodule** — it is a handful
of files copied at a pin, so that `go build` works offline and `//go:embed` can
read them.
Nothing here affects the elfconv submodule.

## Pin

| | |
|---|---|
| Upstream | https://gitlab.com/qemu-project/qemu (GitHub mirror: qemu/qemu) |
| Tag | `v11.1.0` |
| Commit | `84f07211cc5b4fc6a371559bf8a5de4fb068e648` |
| Vendored | 2026-08-14 |

| file | upstream path | sha256 |
|---|---|---|
| `a64.decode` | `target/arm/tcg/a64.decode` | `752c95108544a4a4582efa318b1aae0418ad16aeb2f27cb9877c4849501013fc` |
| `sve.decode` | `target/arm/tcg/sve.decode` | `873f15eb3124a4d77e059fd43a9afadfc6c3d8a0951a7e210b8324291063653b` |
| `LICENSE.LGPL-2.1` | `COPYING.LIB` | `31c90ce76b6f5aab90a205851e71d5c27e31c0aa3d7017a4383b98a6fe3f1faa` |

Verify with:

```sh
sha256sum third_party/qemu-decode/*.decode
```

## License

`a64.decode` (© 2023 Linaro, Ltd) and `sve.decode` (© 2017 Linaro, Ltd) are both
**LGPL-2.1-or-later**, per their own file headers. `LICENSE.LGPL-2.1` is QEMU's
own `COPYING.LIB`, carried alongside them.

Note this is LGPL, *not* QEMU's overall GPL-2.0. Only these decode tables are
vendored; no QEMU code, no TCG, no softfloat.

The enclosing module `tools/decode-oracle` is licensed **LGPL-2.1-or-later** to
match, so no cross-licence election is needed for the `//go:embed` that links
these tables into the tool's binary. `qemudecode.go` in this directory is
raptormark-authored (the embed shim) and carries that licence; the `.decode`
files and `LICENSE.LGPL-2.1` are upstream and keep their own headers. See
`../../README.md` for the reasoning and for why the obligation stops at this
module.

## Two tables, and why they must stay separate

AArch64 is not one decoder. QEMU dispatches, at `translate-a64.c:11200`:

```c
if (!disas_a64(s, insn) &&
    !disas_sme(s, insn) &&
    !disas_sve(s, insn)) {
        unallocated_encoding(s);
}
```

Patterns are required to be mutually exclusive only **within** one table.
a64.decode and sve.decode may overlap **each other**, and the earlier call wins.
`internal/decode.Decoder` preserves that order and validates each table
separately; merging them into one list, or cross-validating them, would be wrong
in the worst way — a confident answer from the wrong decoder.

## Why this pin and not the host's QEMU

The host has QEMU 8.2 installed. Its `a64.decode` is **not** a substitute: the
A64 Advanced SIMD decodetree conversion landed progressively across the 9.x/10.x
series, and in 8.2 much of it was still a hand-written switch in
`translate-a64.c`. The families raptormark actually owes coverage on are exactly
the converted ones, so an older pin silently reports `NoMatch` for the entries
that matter.

Verified present at this pin (`internal/decode` has a test asserting each, so a
re-vendor that loses one fails the gate rather than degrading quietly):

From `a64.decode`:

- `ST_mult` / `LD_mult` — Advanced SIMD load/store multiple structures
- `ST_single` / `LD_single` / `LD_single_repl` — single-structure forms
- `TBL_TBX` — table lookup, with `len` (register-group count) as a field
- `SLI_v` / `SRI_v` / `SLI_s` / `SRI_s` — shift-and-insert

From `sve.decode`, which are exactly the families the corpus differential
measured as missing while only a64 was vendored:

- `LD_zprr` / `ST_zprr` — `ld1b`/`ld1w`/`st1b`/`st1w` with a register offset
- `WHILE_lt`, `PTRUE`, `CNT_r` — loop and predicate setup
- `XAR`, `ZIP1_z` / `ZIP2_z`, `ADD_zzz`, `EOR_zzz`, `LSL_zpzi` / `LSR_zpzi`
- `SEL_zpzz` — what `mov zN, pM/m, zK` actually decodes to

## Grammar surface actually used by these files

Measured at this pin, so the parser in `internal/decode` has a bounded target:

| construct | `a64.decode` | `sve.decode` |
|---|---|---|
| patterns | 1,161 | 929 |
| distinct pattern names | 732 | 807 |
| `%field` definitions | 33 | 35 |
| `&argset` definitions | 52 | 35 |
| `@format` definitions | 116 | 79 |
| `!function=` references | 22 | 18 |
| `!extern` | 0 | 0 |
| group delimiter lines `{ } [ ]` | 4 | 26 |
| line continuations (`\`) | 33 | 156 |

`sve.decode` uses groups an order of magnitude more heavily, which is why it is
worth validating separately rather than treating as more of the same: a parser
bug that `a64.decode` happens not to exercise gets a second chance to surface.

## The `!function=` helpers

decodetree defers post-processing of a field to a C function. These are the
twenty-one the two files reference, with the upstream definitions transcribed
from `target/arm/tcg/translate.h`, `target/arm/tcg/translate-a64.c` and
`target/arm/tcg/translate-sve.c` at the same pin. `internal/decode`
reimplements them; a mismatch here is a silently wrong field value, so they are
transcribed rather than inferred.

| function | definition | source |
|---|---|---|
| `plus_1` | `x + 1` | `translate.h:231` |
| `plus_2` | `x + 2` | `translate.h:236` |
| `times_4` | `x * 4` | `translate.h:256` |
| `times_8` | `x * 8` | `translate.h:261` |
| `rsub_64` | `64 - x` | `translate.h:276` |
| `rsub_32` | `32 - x` | `translate.h:281` |
| `rsub_16` | `16 - x` | `translate.h:286` |
| `rsub_8` | `8 - x` | `translate.h:291` |
| `shl_12` | `x << 12` | `translate.h:296` |
| `xor_2` | `x ^ 2` | `translate.h:301` |
| `uimm_scaled` | `(x >> 3) << (x & 7)` | `translate-a64.c:63` |
| `scale_by_log2_tag_granule` | `x << 4` (`LOG2_TAG_GRANULE` = 4, `cpu.h:2716`) | `translate-a64.c:71` |
| `plus_8` | `x + 8` | `translate.h:241` |
| `plus_12` | `x + 12` | `translate.h:246` |
| `times_2` | `x * 2` | `translate.h:251` |
| `tszimm_esz` | `x >>= 3; 31 - clz32(x)` | `translate-sve.c:50` |
| `tszimm_shr` | `esz < 0 ? esz : (16 << esz) - x` | `translate-sve.c:56` |
| `tszimm_shl` | `esz < 0 ? esz : x - (8 << esz)` | `translate-sve.c:71` |
| `expand_imm_sh8s` | `(int8_t)x << (x & 0x100 ? 8 : 0)` | `translate-sve.c:82` |
| `expand_imm_sh8u` | `(uint8_t)x << (x & 0x100 ? 8 : 0)` | `translate-sve.c:87` |
| `msz_dtype` | `(uint8_t[5]){0,5,10,15,18}[msz]` | `translate-sve.c:95` |

Three of these have a trap in them worth naming:

- `clz32(0)` is **32** (`include/qemu/host-utils.h:165`), so `tszimm_esz` of a
  zero `tsz` is **-1**. That negative esz is meaningful — QEMU's trans functions
  test for it — and `tszimm_shr`/`tszimm_shl` propagate it unchanged. Clamping
  it to 0 would invent a lane width for an encoding that has none.
- `expand_imm_sh8s`/`u` cast to **eight** bits but take the shift decision from
  **bit 8**, which is outside them. An implementation that truncates before
  testing the flag loses the shift.
- `msz_dtype` is a lookup table, not arithmetic. Identity would give 2 where the
  answer is 10.

## What is deliberately not vendored

`sme.decode`, `sme-fa64.decode` and the A32/T32 tables. A32 is not a target.

SME is a measured, bounded gap rather than an assumption. With a64 and sve
vendored, the corpus differential over the fused fixtures reads 100.0000%
(busybox-musl, bash-glibc), 99.9985% (aptget-glibc) and 99.9990%
(postgres-glibc); the residue is SME (`fmopa`, `bfmopa`, ZA-array `ldr`/`str`,
`st1q`) at 24 and 50 sites respectively. Several of the residual examples are
repeated-byte words — `0x80808000`, `0xe0e000e0`, `0xc000c0c0` — which is the
signature of data lifted as code rather than of real SME.

Adding it is mechanical: copy the file here, extend the tables above, add it to
the embed shim, and insert it **between** a64 and sve in
`internal/decode.AArch64`, because that is QEMU's dispatch order.
