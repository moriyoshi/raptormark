// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package qemudecode carries QEMU's vendored decodetree tables as embedded
// data. See PROVENANCE.md for the pin, the licence and the vendoring rules.
//
// This package exists only because `//go:embed` paths may not contain "..", so
// the embed directive has to live in the same directory as the data. Keeping
// the data under third_party/ (rather than copying it into internal/decode) is
// what marks it as pinned upstream material that is never edited in place.
//
// Do not add parsing here. This package is data and nothing else; the
// decodetree parser lives in internal/decode.
package qemudecode

import _ "embed"

// A64 is target/arm/tcg/a64.decode: the declarative AArch64 A64 encoding space.
//
// LGPL-2.1-or-later, (c) 2023 Linaro, Ltd. Verbatim; see PROVENANCE.md.
//
//go:embed a64.decode
var A64 string

// SVE is target/arm/tcg/sve.decode: the SVE and SVE2 encoding space.
//
// A SEPARATE decoder, not an extension of A64. QEMU tries them in order --
// `disas_a64`, then `disas_sme`, then `disas_sve` (translate-a64.c:11200) --
// so the two tables may legally overlap each other and must never be merged
// or cross-validated. internal/decode.AArch64 preserves that order.
//
// LGPL-2.1-or-later, (c) 2017 Linaro, Ltd. Verbatim; see PROVENANCE.md.
//
//go:embed sve.decode
var SVE string
