// ecv-namespace.h — per-program symbol namespacing for the ecvisor link.
//
// EXTRACTED 2026-08-13 from builder/namespace-object.cpp, unchanged, so that
// builder/ecv-prepare.cpp can apply the same transformation without a second
// parse of the module. namespace-object.cpp is now a main() over this header and
// must keep emitting the same bytes; ecv-prepare does the same work inside the
// pass that already has the module in memory.
//
// The one rule for this file: it holds the DECISION about which symbols may
// carry one name across programs. That decision is a correctness boundary --
// sharing a name whose bodies differ hands the linker two definitions and lets
// it keep either -- so it must exist once. The reason for a header rather than
// two copies is the drift warning already in the tree: ecv-split.cpp's addressOf
// is a deliberate duplicate of this one and is annotated as such.
//
// WHY THIS EXISTS
//
// translate-one internalizes each program's module down to one exported symbol,
// its ecv_program_<i> descriptor. That alone is not enough, because codegen is
// SPLIT across many parts to run in parallel: the splitter externalizes any local
// that is referenced across a partition boundary, promoting it to
// hidden-but-GLOBAL, and `wasm-ld -r` keeps it that way. Two programs then
// collide at the ecvisor link on every shared remill helper
// (__remill_sync_hyper_call, the (anonymous namespace):: semantics templates),
// because both objects define the same names.
//
// Passing llvm-split --preserve-locals avoids the promotion, but at real scale
// it defeats the split entirely: after internalize almost everything is local,
// so anything mutually reachable must stay in one partition. Measured on a fused
// glibc binary it emitted one 35.7 MB part plus 80 small ones -- i.e. serial
// codegen, which is exactly what splitting exists to avoid.
//
// Renaming resolves the tension. If every local carries a per-program tag before
// the split, the splitter may promote freely and the promoted names are still
// unique per program.
//
// WHAT IT RENAMES
//
// Only symbols with LOCAL linkage that have a definition -- precisely the set
// the splitter can promote. Declarations keep their names so they still resolve
// against ecvisor and wasi-libc, and any remaining external definition (the
// ecv_program_<i> descriptor, which the registry references by name) is left
// alone.

#pragma once

#include <llvm/IR/Constants.h>
#include <llvm/IR/GlobalValue.h>
#include <llvm/IR/GlobalVariable.h>
#include <llvm/IR/Instructions.h>
#include <llvm/IR/Module.h>

#include <cctype>
#include <cstdint>
#include <cstdlib>
#include <string>
#include <vector>

namespace ecvns {

using namespace llvm;

// newName applies the tag. `_ecv_`-prefixed singletons take the documented
// `_ecv<TAG>_` form; everything else gets a tag prefix, which cannot collide
// with a mangled C++ name.
inline std::string newName(StringRef old, StringRef tag) {
  if (old.starts_with("_ecv_")) {
    return ("_ecv" + tag + "_" + old.drop_front(5)).str();
  }
  return (tag + "." + old).str();
}

// SHARED NAMES (ECV_SHARED_NAMES=1) -- Phase 1b of the cross-program reuse work.
//
// The per-program tag exists so that two programs' promoted locals do not
// collide at the ecvisor link. It also guarantees they never MATCH, which is
// what stops the partition cache from ever serving one program's work to
// another, and stops the linker from collapsing code both programs contain.
//
// With a closure-wide address layout (internal/fuse) and address-derived lifted
// names (patches/0046), the same library function is now byte-identical and
// identically named in every program of a closure -- measured 1527 of 1527 on two
// Debian programs. Those symbols can therefore be shared rather than tagged.
//
// Sharing a name is only safe if the definitions are identical, and only linkable
// if the linker is told to expect duplicates: a linker rejects two STRONG
// definitions of one name however identical their bodies (verified: `wasm-ld`
// gives `duplicate symbol` for strong, and deduplicates to a single `W` for an
// ODR linkage, through both `-r` and the final link). So these become
// weak_odr + hidden here, before the split, which also makes the splitter's
// externalize() skip them -- it only promotes hasLocalLinkage().
//
// WEAK_ODR, NOT LINKONCE_ODR, and the difference is not cosmetic. linkonce_odr
// is discardable-if-unused, and a shared partition holds the library BODIES
// while every caller sits in some other partition -- so `clang -O1` found each
// definition unreferenced within its translation unit and dropped it. Measured:
// all 70 shared partitions compiled to a 314-byte empty object, every one of
// them, while the 10 program partitions carried the whole 1.79 MB. The partition
// cache then "reused" 69 empty objects across two programs, which looks exactly
// like success in the key count and is worth nothing. weak_odr permits the same
// duplicate definitions but must be emitted.
//
// addressOf recognises what the lifter names from a guest address: everything it
// emits ends `_____<hex-vma>`, optionally with a `.suffix` for the companion
// tables (`.bb_addrs`). Anything else -- the remill helpers, the
// anonymous-namespace semantics templates -- keeps the per-program tag for now.
//
// ECV_SHARED_MIN is the boundary. Being address-derived is NOT sufficient: each
// program's own executable is lifted from addresses too, and every executable in
// a closure starts at the same ExeBase, so exe functions across two programs
// collide on the SAME name while having DIFFERENT bodies. Sharing those would
// hand the linker two different definitions under one name and let it keep
// either -- silent wrong code, the worst available failure. Only addresses at or
// above the closure's first library base are shared; internal/fuse.Layout.SharedMin
// reports it.
//
// With no ECV_SHARED_MIN set, nothing is shared, because there is no way to tell
// the two populations apart and guessing is not acceptable here.
inline bool addressOf(StringRef name, uint64_t &out) {
  size_t sep = name.rfind("_____");
  if (sep == StringRef::npos) {
    return false;
  }
  StringRef tail = name.drop_front(sep + 5);
  tail = tail.take_until([](char c) { return c == '.'; });
  if (tail.empty()) {
    return false;
  }
  for (char c : tail) {
    if (!isxdigit(static_cast<unsigned char>(c))) {
      return false;
    }
  }
  return !tail.getAsInteger(16, out);
}

inline uint64_t sharedMin() {
  const char *v = getenv("ECV_SHARED_MIN");
  if (v == nullptr || *v == '\0') {
    return 0;
  }
  return strtoull(v, nullptr, 0);
}

// isShared decides which definitions may carry one name across programs. It is
// the name-based half of the decision; isSharedTable below adds the one class
// that no name rule can recognise.
//
// THREE classes, and collapsing any two of them is a correctness bug:
//
//  1. address-derived at/above minAddr -- library code at a closure-fixed base.
//     SHARE: identical address implies identical body (patches/0046).
//  2. address-derived below minAddr -- each program's own executable. TAG: every
//     exe starts at the same ExeBase, so these names collide across programs
//     while the bodies differ.
//  3. not address-derived -- split again by prefix:
//     - `_Z...` is Itanium C++ mangling. In a lifted module those come only from
//       remill's own C++ (the (anonymous namespace) semantics helpers such as
//       _ZN12_GLOBAL__N_15UMULHEmm), which is compiled identically into every
//       program. SHARE. These are the ORIGINAL reason this tool exists, and
//       measured, they are what kept every shared partition apart: diffing one
//       shared partition between two programs gave 54,554 differing lines, all
//       of them tagged declarations of these helpers.
//     - everything else TAGS. This is deliberately conservative rather than
//       "share whatever is left". The per-program metadata (`_ecv_*` block
//       address arrays, the program descriptor) is program-specific, and so are
//       the fragment's private constants -- an unnamed `.str` holding the
//       PROGRAM NAME is local, non-address-derived, and differs per program.
//       Sharing it would put two different bodies under one name and let the
//       linker keep either.
inline bool isShared(StringRef name, uint64_t minAddr) {
  if (minAddr == 0) {
    return false;
  }
  uint64_t addr = 0;
  if (addressOf(name, addr)) {
    return addr >= minAddr;
  }
  return name.starts_with("_Z");
}

// isSharedGlue covers remill's `extern "C"` runtime glue, which no name rule
// above reaches: it is neither lifted from a guest address nor Itanium-mangled.
//
// There are only three of them in a lifted module -- `emulate_system_call`,
// `__remill_sync_hyper_call`, `__remill_intrinsics` -- and enumerating the
// non-address-derived, non-`_Z` DEFINITIONS of two programs shows nothing else
// in the class that is a function; everything else there is data (the guest's
// section blobs, the block-address tables, the program descriptor, and
// `__remill_state`). They are compiled from remill's own sources into every
// program and were verified identical: the bodies of `emulate_system_call`
// differ between /bin/echo and /bin/cat in one character, the ordinal of an
// attribute group, which is per-module numbering and not content.
//
// `emulate_system_call` alone reached 71 of 80 partitions, because every guest
// syscall site references it.
//
// A NAME LIST, deliberately, where isSharedTable takes a structural test. After
// patches/0049 every function lifted from guest code carries its address, so
// "function without an address" does characterise remill's glue today -- but
// nothing in the lifter FORCES that to stay true, and the two errors are not
// symmetric. A glue function this list misses stays tagged and costs one
// partition's reuse. A guest function it wrongly admitted would put two
// different bodies under one name and let the linker keep either. Requiring a
// Function is a second guard on the same hazard: `__remill_state` is mutable
// per-guest state, and sharing it would give two processes one CPU.
//
// THE NAME IS NOT ENOUGH, and one partition proved it. With the list alone, 69
// of 80 partitions matched across two programs; the odd one out, p41, differed
// by a single symbol -- an undefined reference to the per-program
// `__remill_state`. It came from `__remill_intrinsics`, which the list admits by
// prefix and whose body is
//
//     tail call void @__remill_mark_as_used(ptr @<TAG>.__remill_state)
//
// So `__remill_intrinsics` is NOT program-independent: its body embeds a pointer
// to state that is correctly kept per program. Sharing it puts two different
// bodies under one name, which is precisely the hazard the list was written to
// avoid, and it would leave the losing program's `__remill_state` without the
// marker that keeps it alive.
//
// referencesOnlySharedDefs closes that. A glue function qualifies only if every
// definition it names is itself shared. Declarations are fine -- they are never
// renamed and resolve against ecvisor. The walk is deliberately ONE level deep
// and asks only the name-based questions of what it finds, because the point is
// to reject a program-specific reference, not to compute a transitive closure;
// deepening it would need cycle handling for no benefit yet demonstrated.
inline bool isSharedGlueName(StringRef name) {
  return name.starts_with("__remill_") || name == "emulate_system_call";
}

inline bool referencesOnlySharedDefs(const Function &F, uint64_t minAddr) {
  for (const BasicBlock &bb : F) {
    for (const Instruction &in : bb) {
      for (const Value *op : in.operand_values()) {
        const auto *gv = dyn_cast<GlobalValue>(op->stripPointerCasts());
        if (gv == nullptr || gv->isDeclaration() || !gv->hasLocalLinkage()) {
          continue; // nothing that would be tagged
        }
        // isSharedGlueName is a prefix test, so it must be paired with the
        // Function check here exactly as isSharedGlue pairs it. `__remill_state`
        // starts with `__remill_` and is the one thing this walk exists to
        // reject; asking the name alone accepted it and made the whole test a
        // no-op.
        if (isShared(gv->getName(), minAddr) ||
            (isa<Function>(gv) && isSharedGlueName(gv->getName()))) {
          continue;
        }
        return false;
      }
    }
  }
  return true;
}

inline bool isSharedGlue(const GlobalValue &G, uint64_t minAddr) {
  if (minAddr == 0) {
    return false;
  }
  const auto *fn = dyn_cast<Function>(&G);
  if (fn == nullptr || !isSharedGlueName(fn->getName())) {
    return false;
  }
  return referencesOnlySharedDefs(*fn, minAddr);
}

// isSharedTable covers remill's instruction-selection tables, which the name
// rules above cannot reach and which alone kept EVERY partition apart.
//
// DEF_ISEL emits one global per semantics variant -- `ISEL_ORR_32_LOG_SHIFT`,
// `COND_AL` -- holding a pointer to the semantics function that implements it.
// The names are plain identifiers, so they are neither address-derived nor
// `_Z`-mangled and were tagged per program; the functions they point at are
// `_Z`-mangled and were already shared. Measured on two Debian programs, that
// left 1,541 `ISEL_*` and 15 `COND_*` names divergent, and because a lifted
// function reaches its semantics through these tables they appeared in 80 of 80
// partitions. Nothing could match across programs while they were tagged, no
// matter how much of the rest agreed.
//
// The test is STRUCTURAL rather than a list of name prefixes: a constant whose
// entire value is a pointer to a definition that is itself shared. Sharing is
// then justified by the same argument that already covers the pointee -- if the
// target is identical in every program, a constant pointer to it is too.
//
// Checked against the emitted IR rather than assumed: of every internal global
// in one program's partitions, 491 have this exact form and all 491 are
// `ISEL_*`/`COND_*`. The populations it must NOT sweep in fail it for a reason,
// not by luck -- the guest's own section blobs (`__private_.data_bytes`) and the
// fragment's private strings are byte arrays, not pointers, and `_ecv_*` block
// address arrays are arrays of pointers whose targets are program-specific.
inline bool isSharedTable(const GlobalValue &G, uint64_t minAddr) {
  const auto *var = dyn_cast<GlobalVariable>(&G);
  if (var == nullptr || !var->isConstant() || !var->hasInitializer()) {
    return false;
  }
  const auto *target = dyn_cast<GlobalValue>(var->getInitializer()->stripPointerCasts());
  if (target == nullptr) {
    return false;
  }
  // Either kind of shared pointee will do. DEF_ISEL(SVC_EX_EXCEPTION) points at
  // `emulate_system_call` rather than at a mangled semantics function, so this
  // table shares only once the glue it names does.
  return isShared(target->getName(), minAddr) || isSharedGlue(*target, minAddr);
}

inline bool sharedNamesEnabled() {
  const char *v = getenv("ECV_SHARED_NAMES");
  return v != nullptr && *v != '\0';
}

// makeShared marks a definition as one the linker may see more than once, and
// which the compiler must emit even where nothing local uses it. See the
// weak_odr note above: linkonce_odr silently deleted every shared body.
inline void makeShared(GlobalValue &G) {
  G.setLinkage(GlobalValue::WeakODRLinkage);
  G.setVisibility(GlobalValue::HiddenVisibility);
}

// shouldRename picks out exactly the symbols the splitter may promote to
// hidden-external: locally-linked definitions.
inline bool shouldRename(const GlobalValue &G, StringRef keep) {
  if (G.isDeclaration()) {
    return false; // resolved by ecvisor / wasi-libc; must keep its name
  }
  if (!G.getName().empty() && G.getName() == keep) {
    return false; // the descriptor the registry references by name
  }
  if (G.getName().empty()) {
    return false; // unnamed values are already collision-free
  }
  return G.hasLocalLinkage();
}

// applyNamespacing tags every promotable local, or marks it shared. Returns the
// counts (renamed, shared) so a caller can report them exactly as
// namespace-object always has.
//
// The two-phase structure is load-bearing and is not a style choice. Decide
// everything before renaming anything: isSharedTable asks whether the definition
// an initializer points at is shared, and that question is asked of a NAME -- so
// a target renamed earlier in the same walk would answer it differently.
// Deciding first makes the outcome independent of the order targets were
// collected in.
inline std::pair<size_t, size_t> applyNamespacing(Module &mod, StringRef tag, StringRef keep) {
  // Collect first, rename after: setName can re-hash the value symbol table, so
  // mutating while iterating is not safe.
  std::vector<GlobalValue *> targets;
  for (Function &F : mod.functions()) {
    if (shouldRename(F, keep)) {
      targets.push_back(&F);
    }
  }
  for (GlobalVariable &G : mod.globals()) {
    if (shouldRename(G, keep)) {
      targets.push_back(&G);
    }
  }
  for (GlobalAlias &A : mod.aliases()) {
    if (shouldRename(A, keep)) {
      targets.push_back(&A);
    }
  }
  for (GlobalIFunc &I : mod.ifuncs()) {
    if (shouldRename(I, keep)) {
      targets.push_back(&I);
    }
  }

  const bool shared = sharedNamesEnabled();
  const uint64_t minAddr = sharedMin();

  std::vector<bool> share(targets.size(), false);
  if (shared) {
    for (size_t i = 0; i < targets.size(); ++i) {
      share[i] = isShared(targets[i]->getName(), minAddr) || isSharedGlue(*targets[i], minAddr) ||
                 isSharedTable(*targets[i], minAddr);
    }
  }

  size_t n = 0, s = 0;
  for (size_t i = 0; i < targets.size(); ++i) {
    if (share[i]) {
      // Keep the name; make it a definition the linker may see twice.
      makeShared(*targets[i]);
      ++s;
      continue;
    }
    targets[i]->setName(newName(targets[i]->getName(), tag));
    ++n;
  }
  return {n, s};
}

} // namespace ecvns
