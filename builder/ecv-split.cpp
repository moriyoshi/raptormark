// ecv-split — content-stable module partitioning for the split codegen.
//
// WHY THIS EXISTS
//
// `llvm-split` does two jobs in one binary and we only want one of them.
//
//   1. ASSIGNMENT. It balances partitions by size across the whole module, so
//      any change reshuffles every bucket. Translating one program at registry
//      index 0 and then index 1 -- where only the fragment and the internalize
//      keep-list differ -- produced 80 partitions with ZERO byte-identical to
//      the first run, so the partition cache could not hit once.
//   2. PROMOTION. It externalizes a local that a sibling partition references,
//      so the body survives codegen and resolves at link. This is exactly why
//      builder/namespace-object.cpp exists: promoted names must stay unique per
//      program, which the per-program tag guarantees.
//
// We need (2) and must replace (1), and an external binary cannot be taken
// apart. Substituting `llvm-extract` was tried twice and fails: it is not
// SplitModule with a different policy but a different operation -- extract plus
// its own cleanup -- and on a lifted function whose control flow hangs off an
// `indirectbr` through an external helper it does not preserve the code. A
// 60,375-line function came out as a 9-instruction stub while its symbol still
// appeared defined, both with and without promotion. The cloning is the part
// that matters.
//
// WHAT IT DOES
//
// Mirrors llvm::SplitModule's mechanics with our assignment:
//
//   - cluster every defined GlobalValue by a stable hash of its NAME. Within one
//     program the lifter names a function from its symbol and address, so names
//     are identical across an index shift and the buckets are too; a changed
//     body changes only the bytes of the one partition holding it.
//   - externalize local definitions, as SplitModule does, so a body referenced
//     from a sibling partition survives.
//   - emit each partition with CloneModule under a predicate: members keep their
//     definitions, everything else is cloned as a declaration.
//
// Usage:
//   ecv-split <in.bc> <out-prefix> <num-partitions>
//
// Writes <out-prefix>0 .. <out-prefix>N-1, skipping empty buckets.

//
// THE PARTITIONER ITSELF MOVED to builder/ecv-partition.h on 2026-08-13, so that
// builder/ecv-prepare.cpp can call it on a module it already has in memory
// rather than through a ~28 MB bitcode round trip. This file is now the
// standalone entry point, which the pipeline still uses whenever ecv-prepare is
// turned off (ECV_NO_MERGED_PREPARE) and which stays the way to run a split by
// hand on a saved .ns.bc.

#include "ecv-partition.h"

#include <llvm/IR/LLVMContext.h>
#include <llvm/IRReader/IRReader.h>
#include <llvm/Support/SourceMgr.h>
#include <llvm/Support/raw_ostream.h>

using namespace llvm;

int main(int argc, char **argv) {
  if (argc != 4) {
    errs() << "usage: ecv-split <in.bc> <out-prefix> <num-partitions>\n";
    return 2;
  }
  StringRef in = argv[1], prefix = argv[2];
  const int want = std::atoi(argv[3]);
  if (want <= 0) {
    errs() << "ecv-split: num-partitions must be positive\n";
    return 2;
  }

  LLVMContext ctx;
  SMDiagnostic err;
  auto mod = parseIRFile(in, err, ctx);
  if (!mod) {
    err.print("ecv-split", errs());
    return 1;
  }
  return partitionModule(*mod, prefix, static_cast<unsigned>(want), "ecv-split");
}
