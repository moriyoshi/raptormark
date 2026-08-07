// namespace-object — per-program symbol namespacing for the ecvisor link.
//
// RECONSTRUCTED on 2026-08-05. builder/namespace-object.sh was lost with the
// working tree; this is a rewrite of the step it performed, as an LLVM tool
// alongside ecv-promote (the existing precedent for a companion pass).
//
// THE TRANSFORMATION MOVED OUT on 2026-08-13, unchanged, into
// builder/ecv-namespace.h -- read that file for what is renamed, what is shared,
// and why each of those decisions is a correctness boundary rather than a
// tuning knob. This file is now only the standalone entry point: parse, apply,
// verify, write.
//
// It remains a separate binary because it is the step of the DEFAULT pipeline
// (the one that ends in llvm-split). builder/ecv-prepare.cpp performs the same
// transformation without a second parse of the module, and translate-one uses
// that instead when ECV_MERGED_PREPARE is set. Both must produce the same bytes;
// sharing the header is what makes that true by construction rather than by
// review.
//
// Usage:
//   namespace-object <in.bc> <out.bc> <tag> [keep-symbol]

#include "ecv-namespace.h"

#include <llvm/Bitcode/BitcodeWriter.h>
#include <llvm/IR/LLVMContext.h>
#include <llvm/IR/Module.h>
#include <llvm/IR/Verifier.h>
#include <llvm/IRReader/IRReader.h>
#include <llvm/Support/FileSystem.h>
#include <llvm/Support/SourceMgr.h>
#include <llvm/Support/raw_ostream.h>

using namespace llvm;

int main(int argc, char **argv) {
  if (argc != 4 && argc != 5) {
    errs() << "usage: namespace-object <in.bc> <out.bc> <tag> [keep-symbol]\n";
    return 2;
  }
  StringRef in = argv[1], out = argv[2], tag = argv[3];
  StringRef keep = argc == 5 ? argv[4] : "";

  LLVMContext ctx;
  SMDiagnostic err;
  auto mod = parseIRFile(in, err, ctx);
  if (!mod) {
    err.print("namespace-object", errs());
    return 1;
  }

  auto [n, s] = ecvns::applyNamespacing(*mod, tag, keep);

  if (verifyModule(*mod, &errs())) {
    errs() << "namespace-object: module failed verification after renaming\n";
    return 1;
  }

  std::error_code ec;
  raw_fd_ostream os(out, ec, sys::fs::OF_None);
  if (ec) {
    errs() << "namespace-object: cannot write " << out << ": " << ec.message() << "\n";
    return 1;
  }
  WriteBitcodeToFile(*mod, os);
  errs() << "namespace-object: renamed " << n << " local symbol(s) with tag '" << tag << "'";
  if (ecvns::sharedNamesEnabled()) {
    errs() << ", shared " << s << " symbol(s) as weak_odr";
  }
  errs() << "\n";
  return 0;
}
