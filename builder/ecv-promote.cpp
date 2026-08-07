// ecv-promote — the companion register-promotion pass for raptormark.
// Cross-block, can-block-aware, SSA-clean. See VRO_REWRITE_PLAN.md, JOURNAL 5be-5bm.
//
// Eliminates redundant register LOADS across blocks. Correctness rests on:
//  1. KEEP all stores -> State always coherent -> replay-resume (reconstruct frames from State)
//     is always correct. We never insert or move stores; we only delete redundant loads.
//  2. RE-ENTRY GATE: a block in any @<fn>.bb_addrs array (hasAddressTaken()==true) can be
//     entered via block-address dispatch / fork-replay BYPASSING its CFG predecessor, so its
//     entry register state is unknown -> it does NOT inherit (starts empty, reloads).
//  3. Cross-block value flow ONLY along single-predecessor edges (P is the sole pred -> P
//     dominates B -> P's values dominate B's uses: valid SSA, no PHIs, no dominance breakage).
//  4. Barriers: a call that can SUSPEND (can-block/indirect/__remill_function_call) or clobber
//     an unknown State slot -> invalidate ALL tracked regs; a cant-block call -> invalidate only
//     the callee's transitive tagged write-set. So no register is held in SSA across a suspend.
//  5. SSA-CLEAN erasure: when a load is redundant it is RAUW'd IMMEDIATELY to the canonical
//     root value (never to another to-be-erased load), recorded in `canon`, and erased at the
//     end. The value map only ever holds live root values -> no chained/dangling RAUW (the
//     bug that crashed/hung the previous prototype).
//
// Sound only under cooperative (non-preemptive) scheduling (JOURNAL 5bf).
// Usage: ecv-promote <in.bc> <out.bc>

#include <llvm/ADT/PostOrderIterator.h>
#include <llvm/Bitcode/BitcodeWriter.h>
#include <llvm/IR/Constants.h>
#include <llvm/IR/Function.h>
#include <llvm/IR/InstIterator.h>
#include <llvm/IR/Instructions.h>
#include <llvm/IR/IntrinsicInst.h>

#include <algorithm>
#include <llvm/IR/LLVMContext.h>
#include <llvm/IR/Metadata.h>
#include <llvm/IR/Module.h>
#include <llvm/IR/Verifier.h>
#include <llvm/IRReader/IRReader.h>
#include <llvm/Support/FileSystem.h>
#include <llvm/Support/SourceMgr.h>
#include <llvm/Support/raw_ostream.h>

#include <map>
#include <set>
#include <string>
#include <vector>

using namespace llvm;

static bool regNameOf(const Value *v, unsigned kind, std::string &out) {
  const auto *inst = dyn_cast<Instruction>(v);
  if (!inst) return false;
  MDNode *md = inst->getMetadata(kind);
  if (!md || md->getNumOperands() == 0) return false;
  auto *vam = dyn_cast<ValueAsMetadata>(md->getOperand(0));
  if (!vam) return false;
  auto *cda = dyn_cast<ConstantDataArray>(vam->getValue());
  if (!cda || !cda->isCString()) return false;
  out = cda->getAsCString().str();
  return true;
}
static bool resolveRegPtr(const Value *ptr, unsigned kind, std::string &out) {
  const Value *cur = ptr;
  for (int g = 0; g < 16 && cur; ++g) {
    if (regNameOf(cur, kind, out)) return true;
    if (const auto *bc = dyn_cast<BitCastInst>(cur)) { cur = bc->getOperand(0); continue; }
    if (const auto *gep = dyn_cast<GetElementPtrInst>(cur)) { cur = gep->getPointerOperand(); continue; }
    break;
  }
  return false;
}
static bool peelToStateGep(const Value *p) {
  const Value *cur = p;
  for (int g = 0; g < 16 && cur; ++g) {
    if (auto *gep = dyn_cast<GetElementPtrInst>(cur)) {
      if (gep->getSourceElementType()->isStructTy() &&
          gep->getSourceElementType()->getStructName().contains("State"))
        return true;
      cur = gep->getPointerOperand(); continue;
    }
    if (auto *bc = dyn_cast<BitCastInst>(cur)) { cur = bc->getOperand(0); continue; }
    break;
  }
  return false;
}
static bool isSuspendCall(StringRef n) {
  return n.starts_with("__remill_syscall_tranpoline_call") ||
         n.starts_with("__remill_sync_hyper_call") || n.starts_with("__remill_async_hyper_call");
}

// A call that transfers control into ANOTHER lifted function -- so it may clobber any
// register and may suspend (the callee can unwind). `__remill_function_call` is the
// obvious one; `__remill_jump` is the subtle one: the ecvisor runtime implements it as
// `state.pc = t_vma; func_containing(t_vma)(state, ...)` -- an indirect-jump / computed-goto
// dispatch that CALLS the target lifted function and returns. Treated as a plain declaration
// it looks side-effect-free, so a cant-block callee containing one gets an incomplete
// write-set and a caller holds stale promoted registers across it (postgres miscompile,
// JOURNAL 5bv/5c*). Both must be a full barrier AND a can-block seed.
static bool isDispatchCall(StringRef n) {
  return n.starts_with("__remill_function_call") || n.starts_with("__remill_jump");
}

// The base register a name aliases: X<n>/W<n> share GPR<n>; Q/V/D/S/H/B<n> share the SIMD<n>
// slot; SP/WSP share SP. Writing one view changes the others -> they must be co-invalidated.
static std::string baseKey(const std::string &n) {
  if (n.size() >= 2 && (n[0] == 'X' || n[0] == 'W') && isdigit((unsigned char)n[1]))
    return "G" + n.substr(1);
  if (n.size() >= 2 && (n[0] == 'Q' || n[0] == 'V' || n[0] == 'D' || n[0] == 'S' ||
                        n[0] == 'H' || n[0] == 'B') && isdigit((unsigned char)n[1]))
    return "V" + n.substr(1);
  if (n == "SP" || n == "WSP") return "SP";
  // NEON ARRANGEMENT views also alias the SIMD slot but are DIGIT-prefixed, so the letter
  // checks above miss them: `<count><B|H|S|D>[F]<regnum>` -- 16B0/8B0/8H0/4H0/4S0/2S0/2D0/1D0
  // and the float forms 2SF0/4SF0/2DF0 -- are all views of V<regnum> (e.g. `mul v0.4s` writes
  // "4S0"). Missing this let a callee's "4S0" write NOT co-invalidate a caller's cached "Q0"/
  // "D0" (baseKey V0) -> stale SIMD register -> postgres miscompile (JOURNAL 5c*). Map to V<n>.
  if (!n.empty() && isdigit((unsigned char)n[0])) {
    size_t i = 0;
    while (i < n.size() && isdigit((unsigned char)n[i])) ++i;  // arrangement count
    if (i < n.size() && (n[i] == 'B' || n[i] == 'H' || n[i] == 'S' || n[i] == 'D')) {
      ++i;                                                     // element type
      if (i < n.size() && n[i] == 'F') ++i;                    // optional float marker
      if (i < n.size() && isdigit((unsigned char)n[i]) && n.find_first_not_of("0123456789", i) == std::string::npos)
        return "V" + n.substr(i);                              // trailing digits = reg number
    }
  }
  return n;  // specials (PC, ECV_NZCV, FPSR, ...): own base
}

// A store is "provably local" (cannot alias State) when its pointer traces back to
// an alloca (a stack slot the lifter spills into). Such stores never write a guest
// register, so ignoring them is sound. Any OTHER unresolved store pointer might
// alias State (e.g. an inttoptr of the State base + offset that peelToStateGep can't
// follow), so under the `safestore` debug mode we conservatively invalidate on it.
static bool isProvablyLocal(const Value *p) {
  const Value *cur = p;
  for (int g = 0; g < 16 && cur; ++g) {
    if (isa<AllocaInst>(cur)) return true;
    if (const auto *bc = dyn_cast<BitCastInst>(cur)) { cur = bc->getOperand(0); continue; }
    if (const auto *gep = dyn_cast<GetElementPtrInst>(cur)) { cur = gep->getPointerOperand(); continue; }
    break;
  }
  return false;
}

int main(int argc, char **argv) {
  if (argc != 3) { errs() << "usage: ecv-promote <in.bc> <out.bc>\n"; return 2; }
  LLVMContext ctx;
  SMDiagnostic err;
  auto mod = parseIRFile(argv[1], err, ctx);
  if (!mod) { err.print("ecv-promote", errs()); return 1; }
  unsigned kind = ctx.getMDKindID("remill_register");

  // ---- debug modes (ECV_PROMOTE_MODE) to bisect a suspected miscompile ----
  // "full" (default): all optimizations. "off": no-op (writes input unchanged).
  // "perblock": disable cross-block inheritance (pillar 3). "fullbarrier": every
  // non-suspend call is a FULL barrier -- disables the precise cant-block write-set
  // (pillar 4). "safestore": also invalidate on any non-local unresolved store
  // (tests a missed State-store via an untraced pointer). Modes compose left-to-right
  // via '+', e.g. "perblock+fullbarrier". Emitted to stderr for the build record.
  std::string mode = getenv("ECV_PROMOTE_MODE") ? getenv("ECV_PROMOTE_MODE") : "full";
  auto has = [&](const char *m) { return mode.find(m) != std::string::npos; };
  const bool m_off = has("off");
  const bool m_perblock = has("perblock");
  const bool m_fullbarrier = has("fullbarrier");
  const bool m_safestore = has("safestore");
  errs() << "ecv-promote: mode=" << mode << "\n";

  // ---- module analysis: can-block set + per-fn transitive (writeSet, unknownWrite) ----
  std::map<Function *, std::set<Function *>> callers;
  std::map<Function *, std::vector<Function *>> callees;
  std::map<Function *, std::set<std::string>> wset;
  std::map<Function *, bool> unknownW;
  std::set<Function *> canBlock;
  std::vector<Function *> defs;
  for (Function &F : *mod) {
    if (F.isDeclaration()) continue;
    defs.push_back(&F);
    bool seed = false;
    for (Instruction &I : instructions(F)) {
      if (auto *st = dyn_cast<StoreInst>(&I)) {
        std::string nm;
        if (resolveRegPtr(st->getPointerOperand(), kind, nm)) wset[&F].insert(nm);
        else if (peelToStateGep(st->getPointerOperand())) unknownW[&F] = true;
        continue;
      }
      auto *call = dyn_cast<CallBase>(&I);
      if (!call) continue;
      Function *cal = call->getCalledFunction();
      if (!cal) { seed = true; continue; }
      StringRef n = cal->getName();
      if (isSuspendCall(n) || isDispatchCall(n)) { seed = true; continue; }
      if (!cal->isDeclaration()) { callees[&F].push_back(cal); callers[cal].insert(&F); }
    }
    if (seed) canBlock.insert(&F);
  }
  { std::vector<Function *> w(canBlock.begin(), canBlock.end());
    while (!w.empty()) { Function *f = w.back(); w.pop_back();
      for (Function *c : callers[f]) if (canBlock.insert(c).second) w.push_back(c); } }
  bool ch = true;
  while (ch) { ch = false;
    for (Function *f : defs) { size_t b = wset[f].size(); bool u = unknownW[f];
      for (Function *c : callees[f]) { wset[f].insert(wset[c].begin(), wset[c].end()); u = u || unknownW[c]; }
      if (wset[f].size() != b || u != unknownW[f]) { unknownW[f] = u; ch = true; } } }

  // ---- diagnostic (ECV_PROMOTE_MODE=diag): what writes State outside tagged stores? ----
  // fullbarrier fixing the miscompile means a cant-block callee clobbers a register NOT in its
  // wset. safestore was a no-op (no untraced stores), so the missed write is a NON-store: a
  // memory intrinsic into State, an atomic on State, or a declaration call that writes State.
  // Report their prevalence to pinpoint the mechanism before fixing the analysis.
  if (has("diag")) {
    unsigned memTotal = 0, memToState = 0, atomicToState = 0, memInCantBlockFns = 0;
    std::map<std::string, int> declHist;
    std::set<Function *> cbWithStateMem;
    std::map<std::string, std::string> regBase;  // distinct reg name -> its baseKey
    for (Function &F : *mod) {
      if (F.isDeclaration()) continue;
      const bool cantblock = canBlock.count(&F) == 0;
      for (Instruction &I : instructions(F)) {
        std::string nm;
        if (auto *ld = dyn_cast<LoadInst>(&I)) {
          if (resolveRegPtr(ld->getPointerOperand(), kind, nm)) regBase[nm] = baseKey(nm);
        } else if (auto *stg = dyn_cast<StoreInst>(&I)) {
          if (resolveRegPtr(stg->getPointerOperand(), kind, nm)) regBase[nm] = baseKey(nm);
        }
        if (auto *mi = dyn_cast<MemIntrinsic>(&I)) {
          ++memTotal;
          Value *d = mi->getRawDest();
          if (peelToStateGep(d) || resolveRegPtr(d, kind, nm)) {
            ++memToState;
            if (cantblock) cbWithStateMem.insert(&F);
          }
        } else if (auto *rmw = dyn_cast<AtomicRMWInst>(&I)) {
          if (peelToStateGep(rmw->getPointerOperand()) || resolveRegPtr(rmw->getPointerOperand(), kind, nm))
            ++atomicToState;
        } else if (auto *cx = dyn_cast<AtomicCmpXchgInst>(&I)) {
          if (peelToStateGep(cx->getPointerOperand()) || resolveRegPtr(cx->getPointerOperand(), kind, nm))
            ++atomicToState;
        } else if (auto *call = dyn_cast<CallBase>(&I)) {
          Function *cal = call->getCalledFunction();
          if (cal && cal->isDeclaration()) ++declHist[cal->getName().str()];
        }
      }
    }
    memInCantBlockFns = cbWithStateMem.size();
    errs() << "== ecv-promote diag ==\n"
           << "mem-intrinsics total=" << memTotal << " to-State=" << memToState
           << "  cant-block fns with a State mem-intrinsic=" << memInCantBlockFns << "\n"
           << "atomic (rmw/cmpxchg) to-State=" << atomicToState << "\n"
           << "declaration calls (top 40 by count):\n";
    std::vector<std::pair<std::string, int>> dv(declHist.begin(), declHist.end());
    std::sort(dv.begin(), dv.end(), [](auto &a, auto &b) { return a.second > b.second; });
    for (size_t i = 0; i < dv.size() && i < 40; ++i)
      errs() << "  " << dv[i].second << "  " << dv[i].first << "\n";
    errs() << "distinct register names (" << regBase.size() << "): name=baseKey\n  ";
    for (auto &kv : regBase) errs() << kv.first << "=" << kv.second << " ";
    errs() << "\n";
    return 0;
  }

  // ---- transform ----
  using VMap = std::map<std::pair<std::string, Type *>, Value *>;
  uint64_t elim = 0, fns = 0;
  for (Function *F : defs) {
    if (m_off) break;                        // no-op mode: leave the module unchanged
    std::map<BasicBlock *, VMap> exitMap;
    std::map<Value *, Value *> canon;        // redundant load -> canonical live root
    std::vector<LoadInst *> dead;
    auto resolve = [&](Value *v) { while (canon.count(v)) v = canon[v]; return v; };

    ReversePostOrderTraversal<Function *> rpot(F);
    for (BasicBlock *BBp : rpot) {
      BasicBlock &BB = *BBp;
      VMap cur;
      if (!m_perblock)                                         // pillar 3: cross-block flow
        if (!BB.hasAddressTaken())                             // re-entry gate
          if (BasicBlock *P = BB.getSinglePredecessor())       // dominance-guaranteed edge
            if (auto it = exitMap.find(P); it != exitMap.end()) cur = it->second;

      for (Instruction &I : BB) {
        if (auto *call = dyn_cast<CallBase>(&I)) {
          Function *cal = call->getCalledFunction();
          if (!cal) { cur.clear(); continue; }
          StringRef n = cal->getName();
          if (isSuspendCall(n) || isDispatchCall(n)) { cur.clear(); continue; }
          if (cal->isDeclaration()) continue;                 // mem/flag helper: no reg clobber
          if (canBlock.count(cal) || unknownW[cal]) { cur.clear(); continue; }
          if (m_fullbarrier) { cur.clear(); continue; }       // pillar 4: precise write-set OFF
          std::set<std::string> invBases;                     // cant-block: clobber write-set
          for (const auto &w : wset[cal]) invBases.insert(baseKey(w));
          for (auto it = cur.begin(); it != cur.end();)        // co-invalidate aliasing views
            if (invBases.count(baseKey(it->first.first))) it = cur.erase(it); else ++it;
          continue;
        }
        std::string nm;
        if (auto *st = dyn_cast<StoreInst>(&I)) {
          if (resolveRegPtr(st->getPointerOperand(), kind, nm)) {
            std::string b = baseKey(nm);                       // invalidate all aliasing views
            for (auto it = cur.begin(); it != cur.end();)
              if (baseKey(it->first.first) == b) it = cur.erase(it); else ++it;
            cur[{nm, st->getValueOperand()->getType()}] = resolve(st->getValueOperand());
          } else if (peelToStateGep(st->getPointerOperand())) {
            cur.clear();                                       // untagged State store: unknown reg
          } else if (m_safestore && !isProvablyLocal(st->getPointerOperand())) {
            cur.clear();                                       // maybe-State store via untraced ptr
          }
        } else if (auto *ld = dyn_cast<LoadInst>(&I)) {
          if (!resolveRegPtr(ld->getPointerOperand(), kind, nm)) continue;
          auto key = std::make_pair(nm, ld->getType());
          auto it = cur.find(key);
          if (it != cur.end() && it->second != ld) {
            Value *root = resolve(it->second);
            if (root == ld || root->getType() != ld->getType()) { cur[key] = ld; continue; }
            ld->replaceAllUsesWith(root);                     // immediate RAUW to a LIVE root
            canon[ld] = root;
            dead.push_back(ld);
            ++elim;
          } else {
            cur[key] = ld;                                    // establish: this load is the root
          }
        }
      }
      exitMap[&BB] = std::move(cur);
    }
    for (LoadInst *ld : dead) ld->eraseFromParent();          // all RAUW'd -> dead -> safe
    if (!dead.empty()) ++fns;
  }

  if (verifyModule(*mod, &errs())) {
    errs() << "ecv-promote: promoted module failed verification; NOT writing output\n";
    return 1;
  }
  std::error_code ec;
  raw_fd_ostream os(argv[2], ec, sys::fs::OF_None);
  if (ec) { errs() << "ecv-promote: cannot write " << argv[2] << ": " << ec.message() << "\n"; return 1; }
  WriteBitcodeToFile(*mod, os);
  os.flush();
  errs() << "ecv-promote: " << elim << " redundant register loads eliminated across " << fns
         << " functions (cross-block, can-block-aware, stores kept)\n";
  return 0;
}
