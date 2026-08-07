// ecv-prepare — link, internalize, dead-strip and namespace in ONE parse.
//
// WHY THIS EXISTS
//
// The ecvisor codegen path ran four whole-module passes back to back, each of
// which parsed a 500-600 MB bitcode module and wrote it out again:
//
//     llvm-link  ->  opt -passes=internalize,globaldce  ->  namespace-object  ->  split
//
// Measured on bash-glibc (31.6 MB merged bitcode, LLVM 16, 2026-08-13):
//
//     opt -passes=                     (parse + write, NO passes)   4.999 s
//     opt -passes=internalize,globaldce (the real pass)             5.057 s
//     opt -passes= -o /dev/null        (parse only)                 4.710 s
//
// So internalize plus globaldce is **0.06 s of work behind a 4.7 s parse**, and
// the same parse was being paid by llvm-link and namespace-object either side of
// it. The four passes together were 19.3 s of a 99.3 s cold translation; the
// share is far larger once the partition cache serves the codegen, which is the
// regime the library-scoped reuse work created.
//
// This tool does the first three of the four in one parse. The split stays a
// separate binary because the DEFAULT pipeline uses llvm-split, which is an
// external program that cannot be taken apart -- see builder/ecv-split.cpp for
// that argument in full.
//
// WHAT IT MUST NOT CHANGE
//
// The output has to be byte-identical to the three-step chain it replaces.
// Partition objects are content-addressed (internal/builder/partcache.go), so a
// difference of one byte here invalidates every cached partition of every
// program -- not a correctness failure but a silent, expensive one. The
// namespacing therefore comes from builder/ecv-namespace.h, shared verbatim with
// namespace-object.cpp, and the internalize/globaldce pair runs through the same
// pass manager `opt` would build rather than through hand-written equivalents.
//
// AND THE SPLIT, optionally. Passing --split leaves the fourth pass out too:
// the module is already in memory, so partitioning it here saves writing ~28 MB
// of bitcode for the next process to parse straight back in. Measured on
// bash-glibc, a bare `opt -passes=` round trip of the .ns.bc is 4.324 s against
// 5.6 s for the whole split, so that round trip is most of what the split cost.
//
// It is opt-in per invocation rather than automatic because only the
// content-stable partitioner can be called this way. The default pipeline uses
// `llvm-split`, an external binary that cannot be taken apart -- the argument in
// builder/ecv-partition.h -- so that path still needs the .ns.bc on disk.
//
// AND THE CACHED LIBRARY HALF, optionally. `--merge <lib.bc>` folds in a module
// lifted separately with elflift's `--lift_range` (patches/0052) -- the step
// TODO #34 exists for, so a closure's libraries are lifted once rather than once
// per program. See the table notes below for what merging actually involves;
// the short version is that the runtime already sorts and dedupes these tables,
// so it is a concatenation with two terminator details to get right.
//
// Usage:
//   ecv-prepare <in.bc> <fragment.bc> <out.bc> <tag> <keep-symbol>
//               [--merge <lib.bc>]... [--split <prefix> <n>]
//
// With --split, <out.bc> is still written when ECV_KEEP_SPLIT is set, because
// that is the switch that already means "leave the intermediates for diagnosis";
// otherwise it is skipped, which is the point.

#include "ecv-namespace.h"
#include "ecv-partition.h"

#include <llvm/Bitcode/BitcodeWriter.h>
#include <llvm/IR/LLVMContext.h>
#include <llvm/IR/Module.h>
#include <llvm/IR/PassManager.h>
#include <llvm/IR/Verifier.h>
#include <llvm/IRReader/IRReader.h>
#include <llvm/Linker/Linker.h>
#include <llvm/Passes/PassBuilder.h>
#include <llvm/Support/FileSystem.h>
#include <llvm/Support/SourceMgr.h>
#include <llvm/ADT/StringSet.h>
#include <llvm/Support/raw_ostream.h>
#include <llvm/Transforms/IPO/GlobalDCE.h>
#include <llvm/Transforms/IPO/Internalize.h>

using namespace llvm;

// THE DISPATCH TABLES, and why merging them is a concatenation.
//
// A module lifted with elflift's --lift_range (patches/0052) covers only part of
// the guest, so its seven descriptor globals describe only that part. Composing
// a cached library lift with a per-program exe lift means merging them.
//
// internal/link.FragmentC binds these seven names into the ecv_program_<i>
// descriptor, so the merged module must define exactly one of each:
//
//   _ecv_fun_vmas[]                     guest vma per lifted function, 0-terminated
//   _ecv_fun_ptrs[]                     parallel function pointers, NO terminator
//   _ecv_block_address_ptrs_array[]     per function, the block label pointers
//   _ecv_block_address_vmas_array[]     per function, the block vmas
//   _ecv_block_address_size_array[]     per function, how many blocks
//   _ecv_block_address_fn_vma_array[]   per function, its own vma
//   _ecv_block_address_array_size       how many functions (a SCALAR, not an array)
//
// No re-sorting is needed here, and that is not an accident of this design --
// `build_tables` in runtime/src/context.rs:640 already sorts `funcs` by vma,
// sorts and DEDUPES each inner block map, and sorts the outer map by function
// vma. The runtime was written to accept these tables in any order, so a merge
// only has to produce the right SET.
//
// Two details are load-bearing:
//   - `_ecv_fun_vmas` ends in a 0 guard and the runtime loops `while vma != 0`.
//     Concatenating naively would leave a guard in the middle and silently
//     truncate the table to the first half.
//   - `_ecv_fun_ptrs` has NO guard, so it is one element shorter than
//     `_ecv_fun_vmas`. Dropping "the last element" from both would drop a real
//     function pointer.
static const char *const kFunVmas = "_ecv_fun_vmas";
static const char *const kFunPtrs = "_ecv_fun_ptrs";
static const char *const kBlockPtrs = "_ecv_block_address_ptrs_array";
static const char *const kBlockVmas = "_ecv_block_address_vmas_array";
static const char *const kBlockSizes = "_ecv_block_address_size_array";
static const char *const kBlockFnVmas = "_ecv_block_address_fn_vma_array";
static const char *const kBlockCount = "_ecv_block_address_array_size";

static const char *const kMergeTables[] = {kFunVmas,     kFunPtrs,     kBlockPtrs, kBlockVmas,
                                           kBlockSizes, kBlockFnVmas, kBlockCount};

// elementsOf reads a constant array global's elements.
//
// THREE representations, and missing one reads as an empty table rather than as
// an error. LLVM canonicalises an array of plain integers to ConstantDataArray,
// which is NOT a ConstantArray and does not answer getOperand -- so handling only
// ConstantArray made every i64 table here (`_ecv_fun_vmas`,
// `_ecv_block_address_size_array`, `_ecv_block_address_fn_vma_array`) come back
// empty while the pointer tables read fine. The failure surfaced as
// "_ecv_fun_vmas missing from one of the modules" on a module that plainly
// defines it. A zeroinitializer is expanded for the same reason.
static std::vector<Constant *> elementsOf(GlobalVariable *g) {
  std::vector<Constant *> out;
  if (g == nullptr || !g->hasInitializer()) {
    return out;
  }
  Constant *init = g->getInitializer();
  if (auto *cda = dyn_cast<ConstantDataSequential>(init)) {
    for (unsigned i = 0, e = cda->getNumElements(); i < e; ++i) {
      out.push_back(cda->getElementAsConstant(i));
    }
  } else if (auto *ca = dyn_cast<ConstantArray>(init)) {
    for (unsigned i = 0, e = ca->getNumOperands(); i < e; ++i) {
      out.push_back(ca->getOperand(i));
    }
  } else if (isa<ConstantAggregateZero>(init)) {
    auto *at = cast<ArrayType>(init->getType());
    for (uint64_t i = 0, e = at->getNumElements(); i < e; ++i) {
      out.push_back(Constant::getNullValue(at->getElementType()));
    }
  }
  return out;
}

// replaceArray swaps a global for one holding `elems`, keeping the name so the
// registry fragment still resolves. Uses are `ptr` under opaque pointers, so the
// change of array length needs no cast.
static void replaceArray(Module &m, StringRef name, ArrayRef<Constant *> elems, Type *elemTy) {
  auto *at = ArrayType::get(elemTy, elems.size());
  auto *ng = new GlobalVariable(m, at, true, GlobalValue::ExternalLinkage,
                                ConstantArray::get(at, elems), name + ".merged");
  if (auto *old = m.getNamedGlobal(name)) {
    old->replaceAllUsesWith(ng);
    old->eraseFromParent();
  }
  ng->setName(name);
}

// dropDuplicateDefs strips from the incoming library module every EXTERNAL
// definition the program half already provides, leaving a declaration that
// resolves at link time.
//
// Both halves carry far more than their own lifted functions. `SetCommonMetaData`
// emits the guest's data sections whatever the lift range, and remill's glue --
// the ISEL/COND dispatch tables, `__remill_*`, `emulate_system_call`,
// `__remill_state` -- is compiled into every module. Measured on bash-glibc, the
// two halves share 3,096 address-less symbols, and the first merge attempt died
// on the first of them: `Linking globals named 'ISEL_UNSUPPORTED_INSTRUCTION':
// symbol multiply defined!`
//
// The program half is the one to keep, for a reason beyond arbitrariness:
// `__remill_state` is per-guest MUTABLE state, and two copies would give one
// process two CPUs. Everything else in the class is compiled from the same
// sources into both and was verified identical when the shared-name path was
// built (bodies differing only in per-module attribute-group numbering).
//
// EXTERNAL duplicates are handled by dropping the library's body. INTERNAL ones
// need the opposite treatment -- see mergeInternalDupes below, and do not
// "simplify" this by dropping internal bodies too: nothing would resolve the
// library's own references to the program's copy, because an internal symbol is
// not visible across the link.
static size_t dropDuplicateDefs(Module &prog, Module &lib) {
  StringSet<> progDefs;
  for (const GlobalValue &g : prog.global_values()) {
    if (!g.isDeclaration() && g.hasName() && !g.hasLocalLinkage()) {
      progDefs.insert(g.getName());
    }
  }
  size_t dropped = 0;
  for (Function &f : lib.functions()) {
    if (!f.isDeclaration() && !f.hasLocalLinkage() && progDefs.contains(f.getName())) {
      f.deleteBody();
      f.setLinkage(GlobalValue::ExternalLinkage);
      ++dropped;
    }
  }
  for (GlobalVariable &g : lib.globals()) {
    if (g.hasInitializer() && !g.hasLocalLinkage() && progDefs.contains(g.getName())) {
      g.setInitializer(nullptr);
      g.setLinkage(GlobalValue::ExternalLinkage);
      ++dropped;
    }
  }
  return dropped;
}

// mergeInternalDupes stops the linker from RENAMING the internal definitions
// both halves carry, by making them ODR for the duration of the link.
//
// THE BUG THIS FIXES, which the E2E suite caught and no earlier check did.
// Both halves contain remill's anonymous-namespace semantics helpers
// (`_ZN12_GLOBAL__N_1...`), compiled identically into every module and INTERNAL
// by C++ semantics. Two internal definitions of one name do not collide -- the
// linker quietly renames one, appending `.103`, `.238` and so on. Measured on
// bin_echo: 399 such renamed symbols in a merged module against 7 in a
// whole-program one.
//
// Those suffixes are assigned in link order, so they depend on the PROGRAM half.
// ecv-split buckets by symbol name, so a renamed helper lands in a different
// partition per program -- and cross-program partition reuse collapsed:
// `TestSharedNamesReuseAcrossAClosure` measured **-4 of 121** partitions served,
// program 2 adding 125 new ones where it should have reused nearly all.
//
// The merged module was CORRECT throughout -- it ran, and its tables matched --
// which is exactly why nothing before the suite noticed. Renaming a private
// helper changes no behaviour; it only changes which bucket it lands in.
//
// WEAK_ODR ONLY FOR THE LINK, then internal again. Making them ODR lets the
// linker merge the two copies instead of renaming either, which is sound because
// the bodies are identical -- the same argument builder/ecv-namespace.h already
// makes for sharing `_Z` names across programs. Restoring INTERNAL afterwards
// matters just as much: `shouldRename` only tags locals, so leaving them weak_odr
// would silently opt them out of the per-program namespacing that the
// whole-program path applies, and change the emitted names a second time.
static size_t mergeInternalDupes(Module &prog, Module &lib,
                                 std::vector<std::string> &restore) {
  StringSet<> progInternal;
  for (const GlobalValue &g : prog.global_values()) {
    if (!g.isDeclaration() && g.hasName() && g.hasLocalLinkage()) {
      progInternal.insert(g.getName());
    }
  }
  // TWO SHAPES, and the second only appears once library halves are stripped.
  //
  //  1. The library half DEFINES the same internal symbol -- the duplicate case
  //     above, where the linker would rename one copy.
  //  2. The library half only DECLARES it, because stripToRange removed the body
  //     before caching. An external declaration cannot bind to an INTERNAL
  //     definition, so without promoting the program's copy the reference stays
  //     unresolved -- and `shouldRename` then tags the internal definition while
  //     leaving the declaration untagged, so they diverge by name as well.
  //     Measured on bash before this case was handled: 393 newly undefined
  //     symbols, every one an anonymous-namespace semantics helper.
  //
  // Both are fixed the same way: make the program's copy ODR for the duration of
  // the link so the reference binds, then restore internal afterwards.
  size_t merged = 0;
  for (GlobalValue &g : lib.global_values()) {
    if (!g.hasName()) {
      continue;
    }
    const bool localDef = !g.isDeclaration() && g.hasLocalLinkage();
    const bool bareDecl = g.isDeclaration();
    if (!localDef && !bareDecl) {
      continue;
    }
    if (!progInternal.contains(g.getName())) {
      continue;
    }
    std::string name = g.getName().str();
    if (GlobalValue *p = prog.getNamedValue(name)) {
      p->setLinkage(GlobalValue::WeakODRLinkage);
      p->setVisibility(GlobalValue::HiddenVisibility);
    }
    if (localDef) {
      g.setLinkage(GlobalValue::WeakODRLinkage);
      g.setVisibility(GlobalValue::HiddenVisibility);
    }
    restore.push_back(name);
    ++merged;
  }
  return merged;
}

// mergeTables folds the library half's tables (renamed to <name>.lib before
// linking, so the link itself did not collide) into the program's.
static bool mergeTables(Module &m, LLVMContext &ctx) {
  Type *i64 = Type::getInt64Ty(ctx);
  Type *ptr = PointerType::getUnqual(ctx);

  auto libOf = [&](const char *name) { return m.getNamedGlobal(std::string(name) + ".lib"); };

  // Functions: drop the exe half's 0 guard, append the library half's entries,
  // re-add exactly one guard at the end.
  std::vector<Constant *> vmas = elementsOf(m.getNamedGlobal(kFunVmas));
  std::vector<Constant *> libVmas = elementsOf(libOf(kFunVmas));
  if (vmas.empty() || libVmas.empty()) {
    errs() << "ecv-prepare: --merge: " << kFunVmas << " missing from one of the modules\n";
    return false;
  }
  vmas.pop_back();     // the exe half's guard
  libVmas.pop_back();  // the library half's guard
  vmas.insert(vmas.end(), libVmas.begin(), libVmas.end());
  vmas.push_back(ConstantInt::get(i64, 0));

  // Pointers have no guard, so both halves are taken whole.
  std::vector<Constant *> ptrs = elementsOf(m.getNamedGlobal(kFunPtrs));
  std::vector<Constant *> libPtrs = elementsOf(libOf(kFunPtrs));
  ptrs.insert(ptrs.end(), libPtrs.begin(), libPtrs.end());
  if (ptrs.size() + 1 != vmas.size()) {
    errs() << "ecv-prepare: --merge: " << ptrs.size() << " function pointers against "
           << vmas.size() << " vmas (expected one more vma, for the guard)\n";
    return false;
  }

  replaceArray(m, kFunVmas, vmas, i64);
  replaceArray(m, kFunPtrs, ptrs, ptr);

  // The four per-function block arrays concatenate with no terminator games.
  size_t nfun = 0;
  for (const auto &[name, elemTy] : std::initializer_list<std::pair<const char *, Type *>>{
           {kBlockPtrs, ptr}, {kBlockVmas, ptr}, {kBlockSizes, i64}, {kBlockFnVmas, i64}}) {
    std::vector<Constant *> all = elementsOf(m.getNamedGlobal(name));
    std::vector<Constant *> lib = elementsOf(libOf(name));
    all.insert(all.end(), lib.begin(), lib.end());
    if (nfun == 0) {
      nfun = all.size();
    } else if (all.size() != nfun) {
      errs() << "ecv-prepare: --merge: " << name << " has " << all.size() << " entries, expected "
             << nfun << "; the four block arrays must stay parallel\n";
      return false;
    }
    replaceArray(m, name, all, elemTy);
  }

  // The count is a scalar, and it must equal the merged array length or the
  // runtime reads past the end of every table at once.
  if (auto *old = m.getNamedGlobal(kBlockCount)) {
    auto *ng = new GlobalVariable(m, i64, true, GlobalValue::ExternalLinkage,
                                  ConstantInt::get(i64, nfun), std::string(kBlockCount) + ".merged");
    old->replaceAllUsesWith(ng);
    old->eraseFromParent();
    ng->setName(kBlockCount);
  }

  // The renamed library copies have served their purpose.
  for (const char *name : kMergeTables) {
    if (auto *g = libOf(name)) {
      g->eraseFromParent();
    }
  }
  errs() << "ecv-prepare: merged tables: " << (vmas.size() - 1) << " functions, " << nfun
         << " block maps\n";
  return true;
}

// stripToRange turns a lifted library half into just its own contribution.
//
// WHY. Every module elflift emits carries the same fixed payload whatever range
// it lifted -- remill's semantics, the ISEL/COND dispatch tables, the guest's
// data sections -- because `SetCommonMetaData` emits them unconditionally.
// Measured on bash-glibc's three library halves: **3,268 non-address-derived
// definitions in every one**, against 751, 3,599 and 341 lifted functions
// respectively. So a half for a 60 KB library is 81% payload and one for a
// 115 KB library is 91%.
//
// The merge already throws that copy away -- dropDuplicateDefs deletes the
// external duplicates, mergeInternalDupes collapses the internal ones -- so
// today it is parsed and then discarded, once per cached half. Composing from N
// halves parses N copies of it, and parsing is where ecv-prepare-split's time
// goes (a bare round trip of the .ns.bc is 4.565 s of a 7.318 s phase).
//
// WHAT SURVIVES: definitions the lifter named from an address inside this
// library's range, plus the seven dispatch tables describing them. Everything
// else becomes an external declaration, resolved at merge time against the
// program half -- which always defines it, being a full lift of the exe range.
//
// Internal definitions become EXTERNAL declarations, the only shape a
// declaration can take. mergeInternalDupes promotes the program half's internal
// copy to weak_odr for the link and restores it after, so those declarations
// bind; that pairing already existed for the duplicate case and now serves this
// one too.
static size_t stripToRange(Module &m, uint64_t lo, uint64_t hi) {
  StringSet<> keepTables;
  for (const char *n : kMergeTables) {
    keepTables.insert(n);
  }
  auto keep = [&](StringRef name) {
    if (keepTables.contains(name)) {
      return true;
    }
    uint64_t addr = 0;
    return ecvns::addressOf(name, addr) && addr >= lo && addr < hi;
  };

  size_t stripped = 0;
  for (Function &f : m.functions()) {
    if (!f.isDeclaration() && !f.getName().empty() && !keep(f.getName())) {
      f.deleteBody();
      f.setLinkage(GlobalValue::ExternalLinkage);
      f.setVisibility(GlobalValue::DefaultVisibility);
      f.setComdat(nullptr);
      ++stripped;
    }
  }
  for (GlobalVariable &g : m.globals()) {
    if (g.hasInitializer() && !g.getName().empty() && !keep(g.getName())) {
      g.setInitializer(nullptr);
      g.setLinkage(GlobalValue::ExternalLinkage);
      g.setVisibility(GlobalValue::DefaultVisibility);
      g.setComdat(nullptr);
      ++stripped;
    }
  }
  // llvm.used pins things that no longer have bodies here; the program half
  // keeps its own copy of that list, so dropping ours costs nothing and lets
  // the dead declarations go.
  for (const char *n : {"llvm.used", "llvm.compiler.used"}) {
    if (GlobalVariable *g = m.getGlobalVariable(n)) {
      g->eraseFromParent();
    }
  }
  // Sweep declarations nothing references, which is most of what was just
  // stripped -- the point is to make the module SMALLER to parse.
  bool changed = true;
  while (changed) {
    changed = false;
    for (Function &f : make_early_inc_range(m.functions())) {
      if (f.isDeclaration() && f.use_empty()) {
        f.eraseFromParent();
        changed = true;
      }
    }
    for (GlobalVariable &g : make_early_inc_range(m.globals())) {
      if (g.isDeclaration() && g.use_empty()) {
        g.eraseFromParent();
        changed = true;
      }
    }
  }
  return stripped;
}

int main(int argc, char **argv) {
  // `--strip <lo:hi> <in.bc> <out.bc>` is a separate mode: reduce a lifted
  // library half to its own contribution before it is cached. See stripToRange.
  if (argc == 5 && StringRef(argv[1]) == "--strip") {
    StringRef spec = argv[2], sin = argv[3], sout = argv[4];
    auto colon = spec.find(':');
    if (colon == StringRef::npos) {
      errs() << "ecv-prepare: --strip wants lo:hi, got " << spec << "\n";
      return 2;
    }
    uint64_t lo = 0, hi = 0;
    if (spec.substr(0, colon).getAsInteger(0, lo) || spec.substr(colon + 1).getAsInteger(0, hi) ||
        hi <= lo) {
      errs() << "ecv-prepare: --strip range " << spec << " is unparsable or empty\n";
      return 2;
    }
    LLVMContext sctx;
    SMDiagnostic serr;
    auto smod = parseIRFile(sin, serr, sctx);
    if (!smod) {
      serr.print("ecv-prepare", errs());
      return 1;
    }
    const size_t n = stripToRange(*smod, lo, hi);
    if (verifyModule(*smod, &errs())) {
      errs() << "ecv-prepare: --strip produced an invalid module\n";
      return 1;
    }
    std::error_code sec;
    raw_fd_ostream sos(sout, sec, sys::fs::OF_None);
    if (sec) {
      errs() << "ecv-prepare: cannot write " << sout << ": " << sec.message() << "\n";
      return 1;
    }
    WriteBitcodeToFile(*smod, sos);
    errs() << "ecv-prepare: --strip removed " << n << " definition(s) outside " << spec << "\n";
    return 0;
  }

  if (argc < 6) {
    errs() << "usage: ecv-prepare <in.bc> <fragment.bc> <out.bc> <tag> <keep-symbol>"
              " [--merge <lib.bc>]... [--split <prefix> <n>]\n"
              "       ecv-prepare --strip <lo:hi> <in.bc> <out.bc>\n";
    return 2;
  }
  StringRef in = argv[1], frag = argv[2], out = argv[3], tag = argv[4], keep = argv[5];

  StringRef splitPrefix;
  std::vector<StringRef> mergePaths;
  unsigned nparts = 0;
  for (int i = 6; i < argc;) {
    StringRef opt = argv[i];
    if (opt == "--merge" && i + 1 < argc) {
      // Repeatable: one per cached library half. Caching per LIBRARY rather than
      // per library set is what lets two programs with overlapping-but-unequal
      // library sets share the libraries they do have in common -- /bin/echo and
      // /bin/bash share libc and differ elsewhere, and with a set-granular cache
      // neither could serve the other.
      mergePaths.push_back(argv[i + 1]);
      i += 2;
    } else if (opt == "--split" && i + 2 < argc) {
      splitPrefix = argv[i + 1];
      const int want = std::atoi(argv[i + 2]);
      if (want <= 0) {
        errs() << "ecv-prepare: --split partition count must be positive\n";
        return 2;
      }
      nparts = static_cast<unsigned>(want);
      i += 3;
    } else {
      errs() << "ecv-prepare: unexpected argument " << opt << "\n";
      return 2;
    }
  }

  LLVMContext ctx;
  SMDiagnostic err;

  auto mod = parseIRFile(in, err, ctx);
  if (!mod) {
    err.print("ecv-prepare", errs());
    return 1;
  }
  auto fragMod = parseIRFile(frag, err, ctx);
  if (!fragMod) {
    err.print("ecv-prepare", errs());
    return 1;
  }

  // 1. LINK. The per-program registry fragment references this module's standard
  // `_ecv_*` symbols by name, so it has to be present before internalize decides
  // what is reachable -- link first, exactly as the llvm-link step did.
  if (Linker::linkModules(*mod, std::move(fragMod))) {
    errs() << "ecv-prepare: linking " << frag << " into " << in << " failed\n";
    return 1;
  }

  // 1b. MERGE the cached library half, if one was given.
  //
  // Before internalize, deliberately: the library definitions have to be present
  // when reachability is computed, or globaldce would delete the half of the
  // program that the exe reaches only through the runtime's VMA dispatch.
  //
  // The seven descriptor globals are renamed in the INCOMING module first, so
  // the link itself cannot collide on them; mergeTables then folds them in and
  // erases the renamed copies.
  // One library half at a time. Each iteration leaves `mod` with a single
  // consolidated set of the seven tables, so the next merge sees exactly the
  // shape the first one did and no special case is needed for N > 1.
  for (StringRef mergePath : mergePaths) {
    auto libMod = parseIRFile(mergePath, err, ctx);
    if (!libMod) {
      err.print("ecv-prepare", errs());
      return 1;
    }
    for (const char *name : kMergeTables) {
      if (auto *g = libMod->getNamedGlobal(name)) {
        g->setName(std::string(name) + ".lib");
      }
    }
    const size_t dropped = dropDuplicateDefs(*mod, *libMod);
    std::vector<std::string> restoreInternal;
    const size_t odr = mergeInternalDupes(*mod, *libMod, restoreInternal);
    errs() << "ecv-prepare: --merge " << mergePath << ": dropped " << dropped
           << " duplicate definition(s), merged " << odr << " internal duplicate(s)\n";
    if (Linker::linkModules(*mod, std::move(libMod))) {
      errs() << "ecv-prepare: linking " << mergePath << " into " << in << " failed\n";
      return 1;
    }
    // Back to internal, before namespacing sees them. See mergeInternalDupes.
    for (const std::string &name : restoreInternal) {
      if (GlobalValue *g = mod->getNamedValue(name)) {
        if (!g->isDeclaration()) {
          g->setLinkage(GlobalValue::InternalLinkage);
          g->setVisibility(GlobalValue::DefaultVisibility);
        }
      }
    }
    if (!mergeTables(*mod, ctx)) {
      return 1;
    }
  }

  // 2. INTERNALIZE + GLOBALDCE, through a real pass manager.
  //
  // `InternalizePass` takes the must-preserve predicate that `opt`'s
  // `--internalize-public-api-list` builds: membership of the name list, with
  // every other rule (llvm.used, appending linkage, declarations) staying inside
  // the pass. Writing those rules out by hand here is exactly the kind of
  // near-copy that drifts, and the whole point of this tool is that its output
  // must not differ from the chain it replaces.
  auto mustPreserve = [keep](const GlobalValue &GV) { return GV.getName() == keep; };

  PassBuilder pb;
  LoopAnalysisManager lam;
  FunctionAnalysisManager fam;
  CGSCCAnalysisManager cgam;
  ModuleAnalysisManager mam;
  pb.registerModuleAnalyses(mam);
  pb.registerCGSCCAnalyses(cgam);
  pb.registerFunctionAnalyses(fam);
  pb.registerLoopAnalyses(lam);
  pb.crossRegisterProxies(lam, fam, cgam, mam);

  ModulePassManager mpm;
  mpm.addPass(InternalizePass(mustPreserve));
  mpm.addPass(GlobalDCEPass());
  mpm.run(*mod, mam);

  // 3. NAMESPACE. Shared verbatim with namespace-object.cpp.
  auto [n, s] = ecvns::applyNamespacing(*mod, tag, keep);

  if (verifyModule(*mod, &errs())) {
    errs() << "ecv-prepare: module failed verification\n";
    return 1;
  }

  // The .ns.bc is skipped when we are about to partition in place, unless
  // ECV_KEEP_SPLIT asks for the intermediates. Skipping it is the saving.
  const bool keepIntermediate = getenv("ECV_KEEP_SPLIT") != nullptr;
  if (splitPrefix.empty() || keepIntermediate) {
    std::error_code ec;
    raw_fd_ostream os(out, ec, sys::fs::OF_None);
    if (ec) {
      errs() << "ecv-prepare: cannot write " << out << ": " << ec.message() << "\n";
      return 1;
    }
    // Written WITHOUT use-list order, which is namespace-object's default and
    // therefore what every cached partition was produced from. `opt` and
    // `llvm-link` default the other way (`-preserve-bc-uselistorder` is
    // init(true) there), but that only ever affected the intermediates this tool
    // no longer writes.
    WriteBitcodeToFile(*mod, os);
  }

  // 4. SPLIT, in place. partitionModule MUTATES the module -- it externalizes
  // every local definition before cloning -- so it has to come after the write
  // above, or the .ns.bc kept for diagnosis would not be the module the
  // partitions were made from.
  if (!splitPrefix.empty()) {
    if (int rc = partitionModule(*mod, splitPrefix, nparts, "ecv-prepare")) {
      return rc;
    }
  }

  errs() << "ecv-prepare: renamed " << n << " local symbol(s) with tag '" << tag << "'";
  if (ecvns::sharedNamesEnabled()) {
    errs() << ", shared " << s << " symbol(s) as weak_odr";
  }
  errs() << "\n";
  return 0;
}
