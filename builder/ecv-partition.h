// ecv-partition.h — the content-stable partitioner, as a function over a Module.
//
// EXTRACTED 2026-08-13 from builder/ecv-split.cpp, unchanged. The standalone
// tool still exists and still runs the DEFAULT pipeline's split; this header
// exists so builder/ecv-prepare.cpp can partition a module it already holds in
// memory, instead of writing ~28 MB of bitcode for the next process to parse
// straight back in. That round trip was the last one left after ecv-prepare
// merged the three passes before it: measured on bash-glibc, a bare
// `opt -passes=` round trip of the .ns.bc is 4.324 s, against 5.6 s for the
// whole split.
//
// Everything below the include block is the original file verbatim, with two
// mechanical changes: the partitioning half of `main` became `partitionModule`,
// taking `Module &` instead of parsing a path, and the hard-coded "ecv-split:"
// diagnostic prefix became the `tool` argument so ecv-prepare's errors name
// ecv-prepare.
//
// The original header comment follows, since it is the argument for the design.

#pragma once

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

#include <llvm/Bitcode/BitcodeWriter.h>
#include <llvm/IR/GlobalValue.h>
#include <llvm/IR/LLVMContext.h>
#include <llvm/IR/Module.h>
#include <llvm/IR/Metadata.h>
#include <llvm/IR/Verifier.h>
#include <llvm/IRReader/IRReader.h>
#include <llvm/Support/FileSystem.h>
#include <llvm/Support/SourceMgr.h>
#include <llvm/Support/raw_ostream.h>
#include <llvm/ADT/EquivalenceClasses.h>
#include <llvm/ADT/STLExtras.h>
#include <llvm/ADT/SmallPtrSet.h>
#include <llvm/ADT/SmallVector.h>
#include <llvm/IR/Constants.h>
#include <llvm/IR/Instructions.h>
#include <llvm/Transforms/Utils/Cloning.h>

#include <algorithm>
#include <cctype>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <map>
#include <string>
#include <vector>

using namespace llvm;

// bucketOf is FNV-1a over the name. Any stable hash does; what matters is that
// it depends on nothing but the name. A MIXING hash, not the low bits of
// anything: an earlier attempt keyed on address bits and put 96% of a module
// into one bucket, because function entries are heavily aligned.
static unsigned bucketOf(StringRef name, unsigned n) {
  uint64_t h = 14695981039346656037ULL;
  for (char c : name) {
    h ^= static_cast<unsigned char>(c);
    h *= 1099511628211ULL;
  }
  // FINALIZE. FNV-1a's low bits barely avalanche -- the last byte propagates
  // almost directly into them -- and every lifted name ends `_____<hex>`, so the
  // trailing bytes come from a 16-symbol alphabet. With an even modulus the
  // result inherits that skew: measured over postgres's 79,235 library names into
  // 70 buckets, even-indexed buckets averaged 1,370 against odd 894, a 1.53x
  // imbalance with a range of 836..1,463 where the mean is 1,131.
  //
  // An fmix64-style xor-shift-multiply before the modulo removes it (measured
  // 1,055..1,196, even/odd ratio 0.99). It matters for the codegen tail, which is
  // set by the largest partition, and it is why this cannot land separately from
  // the library scoping: it reassigns every bucket, so it invalidates the whole
  // partition cache and must be paid for once.
  h ^= h >> 33;
  h *= 0xff51afd7ed558ccdULL;
  h ^= h >> 33;
  return static_cast<unsigned>(h % n);
}

// SEGREGATED BUCKETS.
//
// Identical NAMES are not enough for a partition to be reusable across programs:
// a partition is identical only if its whole bucket is. Measured on two programs
// of one closure, 94% of symbols were name-identical after patches/0046 and yet
// 0 of 80 partitions matched, because the ~1,046 program-unique symbols scatter
// uniformly over the buckets. The arithmetic predicts exactly that:
// 80 * (1 - 1/80)^1046 = 0.00 buckets free of them.
//
// So the two populations get disjoint bucket ranges. Shared definitions -- the
// ones namespace-object marked with an ODR linkage, i.e. library code at a
// closure-fixed address -- land in [0, kSharedBuckets); everything program-
// specific lands above. A shared bucket then depends only on shared symbols, so
// it is byte-identical between programs that contain the same library code.
//
// kSharedBuckets is a FIXED fraction, deliberately. Sizing it from the observed
// ratio of shared to program-specific definitions would make it vary per program
// -- a different bucket count reshuffles every assignment and destroys the exact
// property this exists for. It must be a constant of the tool, not of the input.
static unsigned sharedBucketCount(unsigned n) {
  if (n < 8) {
    return n > 1 ? n - 1 : 1;
  }
  return n - n / 8; // 7/8 shared: library code dominates a fused guest
}

// unionBlockAddressUsers co-locates a function with whatever names one of its
// basic blocks.
//
// ONLY a blockaddress forces co-location. A global whose initializer merely
// takes a function's ADDRESS is satisfied by a declaration and the linker
// resolves it; a `blockaddress(@f, %bb)` names a block inside @f and cannot be
// resolved at link time, so it must travel with @f's definition. Splitting them
// yields "Never resolved function from blockaddress" -- malformed bitcode.
//
// Getting this scope wrong is what makes partitioning collapse. Unioning through
// call instructions closes over the whole call graph: 10,454 definitions in ONE
// cluster, i.e. serial codegen. Unioning through every GlobalValue reference
// still gave 5 clusters with 7,825 in the largest, because the lifted module has
// globals holding function pointers for the whole program. The real constraint
// is sparse: a fused bash has 570 blockaddress uses in total, and an llvm-split
// partition typically carries 7.
static void unionBlockAddressUsers(EquivalenceClasses<const GlobalValue *> &clusters,
                                   Function *F) {
  for (const User *U : F->users()) {
    if (!isa<BlockAddress>(U)) {
      continue;
    }
    SmallVector<const User *, 8> work(U->user_begin(), U->user_end());
    while (!work.empty()) {
      const User *UU = work.pop_back_val();
      if (const auto *GVU = dyn_cast<GlobalValue>(UU)) {
        clusters.unionSets(F, GVU);
      } else if (const auto *I = dyn_cast<Instruction>(UU)) {
        clusters.unionSets(F, I->getParent()->getParent());
      } else {
        work.append(UU->user_begin(), UU->user_end());
      }
    }
  }
}

// externalize matches SplitModule's rule: a local that another partition may
// reference has to survive as a linkable symbol. Safe here only because
// namespace-object has already tagged every local with the module id, so the
// promoted names cannot collide between two programs at the ecvisor link.
static void externalize(GlobalValue *GV) {
  if (GV->hasLocalLinkage()) {
    GV->setLinkage(GlobalValue::ExternalLinkage);
    GV->setVisibility(GlobalValue::HiddenVisibility);
  }
}

// LIBRARY-SCOPED PARTITIONS (ECV_LIB_RANGES) -- what makes a partition reusable
// between programs that are not the same size.
//
// Hashing a name gives a stable name-to-INDEX map. It does not give a stable
// index-to-SET map, and a partition's cache key is over its whole membership.
// Measured on the postgres closure: every shared bucket drew from a median of 26
// different libraries and every library was smeared over 63 of the 70 buckets, so
// one library present in program A and absent in B changed EVERY bucket at once.
// The arithmetic in the segregation note above predicts it -- it is the same
// formula, applied to a few hundred library symbols instead of program-unique
// ones. /bin/echo and /bin/bash share 100% of echo's library symbols and matched
// 0 of 80 partitions.
//
// Scoping a partition to ONE library removes the coupling: its membership is that
// library's own symbol list, which is a property of the library rather than of the
// program around it. That is measured, not assumed -- of the 21 libraries postgres
// and initdb both link, all 21 lift IDENTICAL symbol sets, and the entire
// 12,574-symbol gap between them is libraries initdb does not link at all.
//
// The ranges come from internal/fuse.Layout.Ranges via ECV_LIB_RANGES, because a
// lifted name carries its guest address but nothing in the module says where one
// library ends. Unset -- any build that is not a closure -- keeps the old
// assignment exactly, including its sparse partition numbering.
struct LibRange {
  uint64_t start, end;
};

// parseLibRanges reads `start:end,...` in hex, as fuse.FormatLibRanges writes it.
// A malformed entry is dropped rather than fatal: the worst case is that its
// library's functions fall back to the common range, which costs reuse and not
// correctness.
static std::vector<LibRange> parseLibRanges() {
  std::vector<LibRange> out;
  const char *v = getenv("ECV_LIB_RANGES");
  if (v == nullptr || *v == '\0') {
    return out;
  }
  StringRef s(v);
  while (!s.empty()) {
    StringRef item;
    std::tie(item, s) = s.split(',');
    StringRef lo, hi;
    std::tie(lo, hi) = item.split(':');
    uint64_t a = 0, b = 0;
    if (lo.getAsInteger(0, a) || hi.getAsInteger(0, b) || b <= a) {
      continue;
    }
    out.push_back({a, b});
  }
  std::sort(out.begin(), out.end(),
            [](const LibRange &x, const LibRange &y) { return x.start < y.start; });
  return out;
}

// chunkSize is how many clusters share one library-scoped partition. A library is
// split into consecutive chunks of this many, by address order.
//
// It trades the two costs the splitter exists to balance: too large and one
// partition dominates the wall, since codegen is tail-bound on the largest
// single partition; too small and every partition pays a clang process plus the
// declarations it must carry for what it references.
//
// SWEPT on the real postgres module (102,410 definitions), which found the second
// cost to be nearly absent -- the duplicated declarations barely register:
//
//   K      partitions   total bitcode   median partition
//   250       329          473 MiB          897 KB
//   500       218          474 MiB         1.58 MB
//   1000      166          473 MiB         2.37 MB
//   2000      141          474 MiB         2.70 MB
//
// So K is a scheduling choice, not a volume one, and it has to be settled on the
// clock rather than on bytes. Cold translation of /bin/echo, which is where a
// too-coarse K hurts because its libc has only ~4,200 clusters to spread:
//
//   K      wall     partitions
//   500    10m18s      99        <- 8 fat libc chunks, worse than no scoping
//   125     8m05s     122
//   60      8m21s     154
//   (the name-hashed baseline was 8m39s)
//
// Flat below 125 -- the slowest partition plateaus near 400 s either way, which
// is the indivisible-function floor -- so 125 is the knee: it recovers the whole
// cold regression while keeping the partition count and process overhead down.
// Above it, a small closure gets too few, too fat library partitions and
// parallelism suffers.
//
// It does NOT move the postgres wall: the largest partition there is 35.8 MB at
// every K because it is in the PROGRAM range and holds one oversized lifted
// function, which is a different problem (see the tail note in JOURNAL).
static size_t chunkSize() {
  const char *v = getenv("ECV_LIB_CHUNK");
  if (v != nullptr && *v != '\0') {
    if (size_t k = strtoull(v, nullptr, 0)) {
      return k;
    }
  }
  return 125;
}

// addressOf recovers the guest address patches/0046 puts in every lifted name:
// `<sym>_____<hex>`, optionally with a `.suffix` for a companion table. Kept in
// step with the identical helper in builder/namespace-object.cpp -- they read the
// same names for the same reason and must not drift.
static bool addressOf(StringRef name, uint64_t &out) {
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

// libOf finds the library containing an address, or -1.
static int libOf(const std::vector<LibRange> &libs, uint64_t addr) {
  size_t lo = 0, hi = libs.size();
  while (lo < hi) {
    size_t mid = lo + (hi - lo) / 2;
    if (addr < libs[mid].start) {
      hi = mid;
    } else if (addr >= libs[mid].end) {
      lo = mid + 1;
    } else {
      return static_cast<int>(mid);
    }
  }
  return -1;
}

// partitionModule assigns, clones and writes the partitions, returning 0 on
// success. `tool` names the caller in diagnostics; `prefix` is the output path
// stem, so partitions land at <prefix>0 .. <prefix>N-1 with empty buckets
// skipped, exactly as the standalone tool has always written them.
inline int partitionModule(Module &mod, StringRef prefix, unsigned n, StringRef tool) {
  // Cluster by name, and promote in the same pass. Declarations are left alone:
  // they resolve against ecvisor and wasi-libc and must keep their names.
  EquivalenceClasses<const GlobalValue *> clusters;
  size_t defs = 0;
  auto record = [&](GlobalValue &GV) {
    if (GV.isDeclaration() || GV.getName().empty()) {
      return;
    }
    clusters.insert(&GV);
    if (auto *F = dyn_cast<Function>(&GV)) {
      unionBlockAddressUsers(clusters, F);
    }
    externalize(&GV);
    ++defs;
  };
  for (Function &F : mod.functions()) {
    record(F);
  }
  for (GlobalVariable &G : mod.globals()) {
    record(G);
  }
  for (GlobalAlias &A : mod.aliases()) {
    record(A);
  }
  for (GlobalIFunc &I : mod.ifuncs()) {
    record(I);
  }
  if (defs == 0) {
    errs() << tool << ": module defines nothing to partition\n";
    return 1;
  }

  // Bucket whole CLUSTERS, keyed on the lexicographically smallest member name.
  // The leader that EquivalenceClasses happens to pick depends on union order,
  // so hashing it would not be stable; the minimum name is a property of the
  // cluster's contents alone.
  DenseMap<const GlobalValue *, unsigned> cluster;
  size_t nclusters = 0, biggest = 0, sharedClusters = 0, progClusters = 0;
  const unsigned nShared = sharedBucketCount(n);
  const std::vector<LibRange> libs = parseLibRanges();
  const size_t chunk = chunkSize();
  // Without library ranges the shared range stays one hash range, exactly as
  // before. With them it is replaced by per-library chunks, which frees the whole
  // bucket budget for what still needs hashing.
  //
  // THE PROGRAM RANGE GETS ALL OF IT, and the measurement says that is where the
  // wall now is. n/8 buckets was sized when the shared range needed the other
  // 7/8; on postgres that left 18,391 program clusters in 10 partitions, and the
  // eight largest partitions in the whole module were those -- 9 to 61 MB, against
  // a 1.5 MB median for the library-scoped ones. That tail is not new (the same
  // sizes come out of the old assignment, byte for byte) but nothing was competing
  // for the buckets any more, so it is free to fix.
  //
  // The common range stays narrow: 2,858 address-less clusters came to ~55 KB per
  // partition over 10, so subdividing them further would buy nothing and cost a
  // clang process each.
  const unsigned nProg = libs.empty() ? n - nShared : n;
  const unsigned nCommon = n - nShared;

  struct Rec {
    SmallVector<const GlobalValue *, 2> members;
    std::string label; // library-scoped path only
    unsigned bucket = 0; // legacy path only
    StringRef canon;
    bool allShared = false;
    int lib = -1;
    uint64_t addr = 0;
  };
  std::vector<Rec> recs;
  std::map<int, std::vector<uint64_t>> perLib;
  for (auto it = clusters.begin(), e = clusters.end(); it != e; ++it) {
    if (!it->isLeader()) {
      continue;
    }
    ++nclusters;
    StringRef canon;
    size_t members = 0;
    for (auto mi = clusters.member_begin(it); mi != clusters.member_end(); ++mi) {
      StringRef nm = (*mi)->getName();
      if (canon.empty() || nm < canon) {
        canon = nm;
      }
      ++members;
    }
    biggest = std::max(biggest, members);
    // A cluster is shared only if EVERY member is. A blockaddress cluster that
    // mixes shared and program-specific functions cannot be reused, and putting
    // it in a shared bucket would poison that bucket for every program.
    bool allShared = true;
    for (auto mi = clusters.member_begin(it); mi != clusters.member_end(); ++mi) {
      // Either ODR linkage counts. namespace-object emits weak_odr so that a
      // shared body survives codegen in a partition that does not call it;
      // matching only linkonce_odr here would classify every shared cluster as
      // program-specific and quietly undo the segregation.
      if (!(*mi)->isDeclaration() && !(*mi)->hasWeakODRLinkage() &&
          !(*mi)->hasLinkOnceODRLinkage()) {
        allShared = false;
        break;
      }
    }
    // Record the cluster; the label needs per-library ordinals that are only
    // known once every cluster has been seen.
    Rec r;
    r.canon = canon;
    r.allShared = allShared;
    for (auto mi = clusters.member_begin(it); mi != clusters.member_end(); ++mi) {
      if (!(*mi)->isDeclaration()) {
        r.members.push_back(*mi);
      }
    }
    if (allShared) {
      ++sharedClusters;
      if (!libs.empty() && addressOf(canon, r.addr)) {
        r.lib = libOf(libs, r.addr);
        // ASSERT, DO NOT ASSUME, that a cluster sits in one library. Only a
        // blockaddress co-locates, and measured on postgres all 27,708 companion
        // tables share their base definition's address, so a pair carries one
        // library. If that ever stops holding, the cluster must not be split --
        // send it to the common range and say so, rather than emitting bitcode
        // whose blockaddress cannot resolve.
        for (const GlobalValue *m : r.members) {
          uint64_t a = 0;
          if (addressOf(m->getName(), a) && libOf(libs, a) != r.lib) {
            errs() << tool << ": cluster " << canon << " spans two libraries; "
                   << "keeping it whole in the common range\n";
            r.lib = -1;
            break;
          }
        }
      }
    } else {
      ++progClusters;
    }
    if (r.lib >= 0) {
      perLib[r.lib].push_back(r.addr);
    }
    recs.push_back(std::move(r));
  }

  // ORDINAL, not address. Chunking on the address itself would make a partition's
  // membership depend on how densely that stretch of the library happens to be
  // packed; the rank among the library's own cluster addresses gives every
  // partition the same number of clusters, and is just as program-independent
  // because the library's address set is.
  for (auto &kv : perLib) {
    std::sort(kv.second.begin(), kv.second.end());
    kv.second.erase(std::unique(kv.second.begin(), kv.second.end()), kv.second.end());
  }

  const bool scoped = !libs.empty();
  for (Rec &r : recs) {
    char buf[32];
    if (!scoped) {
      // LEGACY PATH, byte-for-byte. No ranges means this is not a closure build,
      // and nothing here may perturb it -- same two hash ranges, same sparse
      // numbering, so an existing object cache stays valid.
      r.bucket = r.allShared ? bucketOf(r.canon, nShared)
                             : nShared + bucketOf(r.canon, nProg);
      continue;
    }
    if (r.lib >= 0) {
      const std::vector<uint64_t> &addrs = perLib[r.lib];
      const size_t ord =
          std::lower_bound(addrs.begin(), addrs.end(), r.addr) - addrs.begin();
      // ROUND-ROBIN, not consecutive blocks. Both depend only on the library's
      // own cluster list, so both are program-independent, which is the property
      // that matters -- but `ord / chunk` puts a contiguous NEIGHBOURHOOD of the
      // library in one partition, and that is measurably poison for codegen.
      // Measured on /bin/echo: one 6.1 MB block-chunked partition took 1,278 s
      // while a 9.6 MB one took 369 s, and the program's total went from 8m39s to
      // 22m15s. Size was not the variable; adjacency was. Interleaving restores
      // the scatter the name hash used to provide.
      const size_t nchunks = (addrs.size() + chunk - 1) / chunk;
      snprintf(buf, sizeof buf, "L%03d_%05zu", r.lib, nchunks ? ord % nchunks : 0);
    } else if (r.allShared) {
      // Address-less shared code: remill's semantics helpers and its DEF_ISEL
      // tables. The lifter emits the whole semantics library rather than only
      // what a program uses, so this set is program-independent -- measured
      // identical at 2,858 names across postgres, initdb and dash -- and hashing
      // it reuses cleanly.
      snprintf(buf, sizeof buf, "C%03u", bucketOf(r.canon, nCommon));
    } else {
      snprintf(buf, sizeof buf, "P%03u", bucketOf(r.canon, nProg));
    }
    r.label = buf;
  }

  // DEGENERATE CASE. If the module has no shared definitions at all -- shared
  // naming disabled, or a guest with no closure-fixed libraries -- the reserved
  // shared range would sit empty and every cluster would crowd into the
  // remaining n/8 buckets. Measured when this happened by accident: 80 requested
  // partitions became 10, the largest grew from 1.3 MB to 6.1 MB, and codegen
  // went from under a minute to more than ten.
  //
  // Re-bucket over the whole range instead. Special-casing EXACTLY zero is safe
  // for reuse: a module with no shared clusters has nothing to reuse, so its
  // assignment cannot differ from another program's in a way that matters. Sizing
  // the ranges from the observed RATIO would not be safe, and is not done.
  if (sharedClusters == 0 && progClusters > 0) {
    for (Rec &r : recs) {
      r.bucket = bucketOf(r.canon, n);
      if (scoped) {
        char buf[32];
        snprintf(buf, sizeof buf, "P%03u", r.bucket);
        r.label = buf;
      }
    }
  }

  unsigned nbuckets = n;
  if (scoped) {
    // Number the labels. Sorted, so the assignment is a function of the label set
    // alone and two runs over one module agree -- wasm-ld -r takes the objects in
    // this order and the final object would otherwise vary for no reason.
    std::vector<std::string> labels;
    labels.reserve(recs.size());
    for (const Rec &r : recs) {
      labels.push_back(r.label);
    }
    std::sort(labels.begin(), labels.end());
    labels.erase(std::unique(labels.begin(), labels.end()), labels.end());
    std::map<std::string, unsigned> labelIndex;
    for (size_t i = 0; i < labels.size(); ++i) {
      labelIndex[labels[i]] = static_cast<unsigned>(i);
    }
    for (Rec &r : recs) {
      r.bucket = labelIndex[r.label];
    }
    nbuckets = static_cast<unsigned>(labels.size());
  }
  for (const Rec &r : recs) {
    for (const GlobalValue *m : r.members) {
      cluster[m] = r.bucket;
    }
  }
  std::vector<size_t> counts(nbuckets, 0);
  for (auto &kv : cluster) {
    ++counts[kv.second];
  }

  size_t written = 0;
  for (unsigned i = 0; i < nbuckets; ++i) {
    if (counts[i] == 0) {
      continue; // an empty bucket is not an error, just an unused slot
    }
    ValueToValueMapTy vmap;
    // The predicate is what makes this a SPLIT rather than an extraction:
    // non-members are cloned as declarations, so every reference still resolves
    // and no body is discarded on the way.
    auto part = CloneModule(mod, vmap, [&](const GlobalValue *GV) {
      auto it = cluster.find(GV);
      return it != cluster.end() && it->second == i;
    });
    // Normalise the module identity. CloneModule copies the source module's
    // identifier and source filename into every partition, and those are the
    // INPUT PATH -- so two translations of the same program into different output
    // directories produced 80 partitions that differed only by a path string, and
    // the content-addressed cache missed on all of them. The partition's identity
    // must depend on its contents alone.
    part->setModuleIdentifier("ecv-split-partition");
    part->setSourceFileName("ecv-split-partition");

    // Deduplicate `llvm.ident`, which ACCUMULATES across links and otherwise
    // makes a partition's bytes depend on how many modules were linked to build
    // the program -- nothing to do with the partition's contents.
    //
    // This is not hypothetical tidying. Composing a program from three cached
    // library halves against one combined half produced partitions with
    // IDENTICAL membership -- same 12,023 definitions, same 11,076 clusters,
    // same 126 partitions, same 3,921 renamed symbols -- and 0 of 126 matching
    // byte for byte. The whole difference was `!llvm.ident` carrying 17 repeated
    // entries instead of 9. Cross-program partition reuse went to -5 of 121 on
    // that alone, because the two programs of a closure link different numbers
    // of libraries.
    //
    // Deduplicating rather than erasing keeps the provenance the field exists
    // for, and is enough: the entries are repeats of the same producer string,
    // so both shapes collapse to the same short list.
    if (NamedMDNode *ident = part->getNamedMetadata("llvm.ident")) {
      SmallVector<MDNode *, 4> uniq;
      SmallPtrSet<MDNode *, 4> seen;
      for (MDNode *op : ident->operands()) {
        if (seen.insert(op).second) {
          uniq.push_back(op);
        }
      }
      if (uniq.size() != ident->getNumOperands()) {
        ident->clearOperands();
        for (MDNode *op : uniq) {
          ident->addOperand(op);
        }
      }
    }

    // Drop declarations nothing in this partition uses.
    //
    // CloneModule reproduces the WHOLE symbol table, so every partition carries a
    // declaration for every global in the module. That makes a partition depend
    // on the rest of the module after all: changing one program's registry index
    // renames `ecv_program_name_<i>`, and all 80 partitions differed by that one
    // unused declaration while being otherwise byte-identical -- 55,657 identical
    // IR lines, 6 differing. Removing them restores the property this tool exists
    // for, and shrinks each partition as well.
    bool changed = true;
    while (changed) {
      changed = false;
      for (auto &F : make_early_inc_range(part->functions())) {
        if (F.isDeclaration() && F.use_empty()) {
          F.eraseFromParent();
          changed = true;
        }
      }
      for (auto &G : make_early_inc_range(part->globals())) {
        if (G.isDeclaration() && G.use_empty()) {
          G.eraseFromParent();
          changed = true;
        }
      }
      for (auto &A : make_early_inc_range(part->aliases())) {
        if (A.isDeclaration() && A.use_empty()) {
          A.eraseFromParent();
          changed = true;
        }
      }
    }

    // Canonicalise the ORDER, which is the last thing that made two partitions
    // of identical content different bytes.
    //
    // CloneModule walks the source module and appends in the order it finds
    // things, so a partition's layout is inherited from the whole module -- and
    // the whole module's layout follows the order the lifter created functions
    // in, which two programs of one closure do not share. Measured on Debian
    // /bin/echo and /bin/cat after every naming fix: partition p0 held the SAME
    // 495 entries in both programs and was the same 1,160,496 bytes, yet the
    // files differed, because from index 447 on the declarations appeared in a
    // different sequence. 69 of 80 partitions were in that state -- identical
    // content, unequal bytes, and therefore 160 cache keys for 2 programs.
    //
    // Module-level order carries no meaning, so sorting by name is free. It has
    // to happen after the dead-declaration sweep above, or erasing an entry
    // would undo it.
    auto byName = [](const auto &a, const auto &b) { return a.getName() < b.getName(); };
    part->getFunctionList().sort(byName);
    part->getGlobalList().sort(byName);
    part->getAliasList().sort(byName);
    part->getIFuncList().sort(byName);

    if (verifyModule(*part, &errs())) {
      errs() << tool << ": partition " << i << " failed verification\n";
      return 1;
    }

    std::string out = (prefix + Twine(i)).str();
    std::error_code ec;
    raw_fd_ostream os(out, ec, sys::fs::OF_None);
    if (ec) {
      errs() << tool << ": cannot write " << out << ": " << ec.message() << "\n";
      return 1;
    }
    WriteBitcodeToFile(*part, os);
    ++written;
  }

  errs() << tool << ": " << defs << " definitions in " << nclusters << " clusters (largest "
         << biggest << ", " << sharedClusters << " shared) into " << written << " partitions; ";
  if (scoped) {
    size_t scopedClusters = 0;
    for (const Rec &r : recs) {
      if (r.lib >= 0) {
        ++scopedClusters;
      }
    }
    errs() << libs.size() << " libraries, chunk " << chunk << ", " << scopedClusters
           << " clusters library-scoped, " << nCommon << " common and " << nProg
           << " program buckets\n";
  } else {
    errs() << "shared buckets [0," << nShared << ")\n";
  }
  return 0;
}

