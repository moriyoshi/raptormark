// The decode oracle is a SEPARATE Go module on purpose.
//
// It embeds QEMU's decodetree tables, which are LGPL-2.1-or-later, and is
// itself licensed LGPL-2.1-or-later to match (see LICENSE and README.md).
//
// The raptormark pipeline has no declared license yet, and its lifter lineage
// (third_party/elfconv, remill, LLVM) is Apache-2.0 -- incompatible with GPLv2
// and LGPLv2.1 specifically, NOT with the GPL family at large; LGPLv3 is
// Apache-compatible and the tables' "or later" grant reaches it. Keeping this
// in its own module means the LGPL obligation attaches only to a developer-only
// analysis tool that is never built into, linked with, or shipped alongside the
// pipeline, and leaves the root module's licence an open decision.
//
// It is also deliberately DEPENDENCY-FREE -- stdlib only, no kong -- so the
// smallest possible surface carries that obligation, and so it builds offline
// forever.
module raptormark/tools/decode-oracle

go 1.26.0
