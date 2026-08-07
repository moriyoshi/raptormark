package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// TranslateOne is the non-interactive single-binary translation that runs
// inside the builder image, and the image's entrypoint.
//
// It mirrors the *-wasi32 branch of the pinned submodule's scripts/elfconv.sh
// (third_party/elfconv @ the commit this image was built from), with an
// explicit CLI instead of the env-var UI, deterministic output paths, and no
// trailing `wasmedge compile` step (AOT precompilation is a host-side concern).
//
// Outputs, in --out:
//
//	<module-id>.wasm   final single-binary WASI module (upstream runtime only)
//	<module-id>.bc     lifted LLVM bitcode
//	<module-id>.o      compiled lifted object, input to the ecvisor link
type TranslateOne struct {
	ELF      string `name:"elf" required:"" type:"path" help:"aarch64 ELF to translate."`
	Out      string `name:"out" required:"" type:"path" help:"Output directory."`
	ModuleID string `name:"module-id" required:"" help:"Names the outputs and tags every local symbol, e.g. prog_ab12cd34ef56."`
	Target   string `name:"target" default:"aarch64-wasi32" help:"Only aarch64-wasi32 is supported."`
	Runtime  string `name:"runtime" default:"upstream" enum:"upstream,ecvisor" help:"upstream links elfconv's C++ runtime; ecvisor emits an object for the Rust supervisor link."`
	Fragment string `name:"fragment" type:"path" help:"Per-program registry fragment from internal/link. Required for --runtime ecvisor."`
	Keep     string `name:"keep" help:"Symbol the internalize pass preserves, i.e. ecv_program_<i>. Required for --runtime ecvisor."`
	Promote  bool   `name:"promote" help:"Run the ecv-promote register-promotion pass over the lifted bitcode."`
	// Opt-in, and off is byte-identical to the option not existing — proven by
	// SHA-256 on the lifted bitcode, which is what licenses internal/translate
	// to leave the cache key unchanged for a default build. See
	// translate.Options.InlineCallHistory.
	SuspendViaCall bool `name:"suspend-via-call" help:"Have elflift emit the suspend check as a call to _ecv_suspended rather than a read of the __ecv_unwinding wasm global (elfconv patch 0067). Required by the wasix profile, whose loader refuses a module importing a GLOBAL from env; slower everywhere else."`

	InlineCallHistory bool `name:"inline-call-history" help:"Have elflift emit the guest call history inline instead of calling _ecv_save_call_history / _ecv_func_epilogue at every guest BL. Faster on call-heavy guests, larger module. Needs a runtime that publishes the __ecv_ch_* globals."`
	Jobs              int  `name:"jobs" help:"Concurrent codegen processes. 0 means one per core. Scheduling only: the partition count is fixed, so this cannot change what is emitted."`

	// tm records the per-phase and per-partition breakdown. Unexported, so kong
	// does not see it as a flag. Nil is valid and disables recording; see
	// timing.go, which must never influence what this pipeline emits.
	tm *timing
}

const elfconvRoot = "/root/elfconv"

func (c *TranslateOne) Run() error {
	if c.Target != "aarch64-wasi32" {
		return usageErrorf("unsupported target: %s (only aarch64-wasi32)", c.Target)
	}
	if c.Runtime == "ecvisor" && (c.Fragment == "" || c.Keep == "") {
		return usageErrorf("--runtime ecvisor requires --fragment and --keep")
	}

	// WASI_SDK_PATH comes from the base image's /root/.bash_profile, which a
	// plain (non-login) invocation never sees.
	sourceLoginProfile()
	sdk, err := wasiSDK()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return err
	}
	bc := filepath.Join(c.Out, c.ModuleID+".bc")
	obj := filepath.Join(c.Out, c.ModuleID+".o")
	wasm := filepath.Join(c.Out, c.ModuleID+".wasm")

	// The report is written even when a step fails: a translation that dies 40
	// minutes in is precisely the one whose breakdown is wanted. It lands beside
	// the object rather than on stdout, which internal/translate discards.
	c.tm = newTiming(c.ModuleID, c.ELF, c.Runtime)
	defer c.tm.write(filepath.Join(c.Out, c.ModuleID+".timing.json"))

	// THE LIFT, whole or split. Split when a library cache is configured: the
	// exe half is lifted per program and the library half comes from cache when
	// another program of the closure already lifted it. See libcache.go for why
	// splitting costs nothing even on a miss.
	//
	// libBC is "" unless a library half is in play; codegenEcvisor passes it to
	// ecv-prepare as --merge.
	var libBCs []string
	if libCacheActive() {
		var err error
		if libBCs, err = c.liftSplit(bc); err != nil {
			return err
		}
	} else if err := c.tm.step("elflift", c.ELF, bc, func() error { return c.lift(bc) }); err != nil {
		return err
	}
	if c.Promote {
		if err := c.tm.step("ecv-promote", bc, bc, func() error { return c.promote(bc) }); err != nil {
			return err
		}
	}

	switch c.Runtime {
	case "upstream":
		return c.codegenUpstream(sdk, bc, obj, wasm)
	case "ecvisor":
		return c.codegenEcvisor(sdk, bc, obj, libBCs)
	}
	return usageErrorf("unknown --runtime: %s", c.Runtime)
}

// lift is the ELF -> LLVM bitcode step: elflift, an upstream black box.
func (c *TranslateOne) lift(bc string) error {
	// ⚠️ An ENV VAR, not a flag: patch 0067 gates on `ECV_SUSPEND_VIA_CALL`
	// inside the lifter rather than adding an elflift command-line option, so
	// the flag-off command line stays byte-identical to what it was before the
	// option existed -- the same property `--inline_call_history` preserves by
	// being omitted.
	var extra []string
	if c.SuspendViaCall {
		extra = append(extra, "ECV_SUSPEND_VIA_CALL=1")
	}
	return runWithEnv(elfconvRoot+"/build/lifter/elflift", extra, c.liftArgs(bc)...)
}

func (c *TranslateOne) liftArgs(bc string) []string {
	args := []string{
		"--arch", "aarch64",
		"--bc_out", bc,
		"--target_elf", c.ELF,
		"--dbg_fun_vma", "0",
		"--bitcode_path", "",
		"--target_arch", "wasi32",
		"--float_exception", "0",
		"--norm_mode", "1",
		"--fork_emulation", "1",
	}
	// Passed only when asked for. elflift defaults it to "0" and the flag-off
	// codegen is byte-identical to the flag not existing, so omitting it and
	// passing 0 are the same lift -- but omitting keeps the command line, and
	// therefore any log or reproduction of it, identical to before the option
	// existed.
	if c.InlineCallHistory {
		args = append(args, "--inline_call_history", "1")
	}
	return args
}

// promote runs the companion register-promotion pass in place on the bitcode.
//
// elflift --norm_mode emits remill-standard load/store IR (register access via
// State), with Vro disabled (norm_mode forces it off). ecv-promote is the
// post-lift replacement: a sound, barrier-aware register-promotion pass. Without
// --promote the pipeline is unchanged. See VRO_REWRITE_PLAN.md.
func (c *TranslateOne) promote(bc string) error {
	if err := run("ecv-promote", bc, bc+".promoted"); err != nil {
		return fmt.Errorf("ecv-promote failed on %s: %w", bc, err)
	}
	return os.Rename(bc+".promoted", bc)
}

// wasiFlags are the compile flags shared by the upstream-runtime steps.
func (c *TranslateOne) wasiFlags(sdk string) []string {
	return []string{
		"-O3", "-std=c++20",
		"--sysroot=" + sdk + "/share/wasi-sysroot",
		"-D_WASI_EMULATED_SIGNAL", "-D_WASI_EMULATED_PROCESS_CLOCKS", "-D_WASI_EMULATED_MMAN",
		"-I" + elfconvRoot + "/backend/remill/include", "-I" + elfconvRoot,
		"-fno-exceptions",
		"-DTARGET_IS_WASI=1", "-DELF_IS_AARCH64",
		// The quotes are part of the macro value, as in the script's
		// -DELFNAME="\"${ELFNAME}\"".
		fmt.Sprintf("-DELFNAME=%q", filepath.Base(c.ELF)),
	}
}

// codegenUpstream produces elfconv's own single-binary output, mirroring
// scripts/elfconv.sh's *-wasi32 branch.
func (c *TranslateOne) codegenUpstream(sdk, bc, obj, wasm string) error {
	cxx := sdk + "/bin/clang++"
	if err := run(cxx, c.upstreamObjArgs(sdk, bc, obj)...); err != nil {
		return err
	}
	if err := run(cxx, c.upstreamWasmArgs(sdk, obj, wasm)...); err != nil {
		return err
	}
	fmt.Printf("translate-one: built %s (runtime: upstream)\n", wasm)
	return nil
}

func (c *TranslateOne) upstreamObjArgs(sdk, bc, obj string) []string {
	return append(c.wasiFlags(sdk), "-c", bc, "-o", obj)
}

func (c *TranslateOne) upstreamWasmArgs(sdk, obj, wasm string) []string {
	runtimeDir := elfconvRoot + "/runtime"
	utilsDir := elfconvRoot + "/utils"
	return append(c.wasiFlags(sdk),
		"-lwasi-emulated-process-clocks", "-lwasi-emulated-mman", "-lwasi-emulated-signal",
		"-o", wasm, obj,
		runtimeDir+"/Entry.cpp",
		runtimeDir+"/Memory.cpp",
		runtimeDir+"/Runtime.cpp",
		runtimeDir+"/VmIntrinsics.cpp",
		utilsDir+"/Util.cpp",
		utilsDir+"/elfconv.cpp",
		runtimeDir+"/syscalls/SyscallWasi.cpp",
	)
}

// llvmTools names the version-matched LLVM toolchain for the bitcode pipeline.
//
// Default 16 (the pinned elfconv line). ECV_LLVM_VER=22 selects the LLVM-22
// toolchain: elflift-22 emits LLVM-22 bitcode, so llvm-link/opt/split/clang must
// ALL match — older tools cannot read newer bitcode. For 16 the wasi-sdk clang
// (LLVM 18) reads LLVM-16 bitcode; for others use the system clang-<ver> plus
// the wasi-sysroot (validated JOURNAL 5c4/5c5).
type llvmTools struct {
	ver     string
	cc      string
	target  []string
	wasmLD  string
	sysroot string
}

func newLLVMTools(sdk string) llvmTools {
	t := llvmTools{ver: envOr("ECV_LLVM_VER", "16"), sysroot: "--sysroot=" + sdk + "/share/wasi-sysroot"}
	if t.ver == "16" {
		t.cc = sdk + "/bin/clang"
		t.wasmLD = sdk + "/bin/wasm-ld"
	} else {
		t.cc = "clang-" + t.ver
		t.target = []string{"--target=wasm32-wasi"}
		t.wasmLD = "/usr/lib/llvm-" + t.ver + "/bin/wasm-ld"
	}
	return t
}

// compileIR compiles one bitcode part to an object at the given optimisation
// level.
//
// # Why -fPIC is unconditional
//
// So that ONE set of translated objects serves both link paths: the ordinary
// single-module link a stock runwasi shim runs, and the
// `--experimental-pic -shared` side-module link an owned embedder would
// (.agents/docs/MULTIMODULE.md §5). Translation is the expensive half of this
// pipeline — minutes to hours per program — so a per-path codegen flag would
// mean translating everything twice. A per-path LINK is about a second.
//
// It is affordable because PIC costs nothing on lifted code, measured on four
// partitions of a real bash lift (MULTIMODULE.md §6, 2026-08-18): defined
// function counts unchanged, and the only partition with memory relocations came
// out 34 bytes SMALLER — its 16 `R_WASM_MEMORY_ADDR_SLEB` collapse to one
// `MEMORY_ADDR_REL_SLEB` plus an undefined `__memory_base`. Lifted code reaches
// guest memory through `arena_ptr`, so there is almost nothing for PIC to
// relocate.
//
// And it does not change the stock-shim artifact: linked the ordinary way, the
// PIC object produces a module with the SAME 319 imports as the non-PIC one and
// no `__memory_base` among them — wasm-ld resolves it internally in a static
// link. That is the property the whole scheme rests on, so
// TestCompileIREmitsPIC guards it, by asserting a real module imports no
// `__memory_base`.
//
// ⚠️ This named a `…PICObjectsStillLinkFlat` test until 2026-08-27, and no such
// test has ever existed. The guard is real under the name above.
//
// ⚠️ This flag invalidates every cached object, because translateone.go is in
// `translateSources` (toolsid.go) and so feeds `raptormark.translate_sh`. That
// is by design and it is the conservative side to err on, but it means adopting
// this costs a full re-translation of every closure. Batch it with other
// pipeline changes.
func (t llvmTools) compileIR(level, in, out string, asIR bool) error {
	return run(t.cc, t.compileIRArgs(level, in, out, asIR)...)
}

// compileIRArgs is split out so the flags can be asserted without a toolchain,
// the way LinkAll.wasmOptArgs is. TestCompileIREmitsPIC holds the -fPIC above.
func (t llvmTools) compileIRArgs(level, in, out string, asIR bool) []string {
	args := append([]string{}, t.target...)
	args = append(args, level, t.sysroot, "-fPIC")
	if asIR {
		args = append(args, "-x", "ir")
	}
	return append(args, "-c", in, "-o", out)
}

// codegenEcvisor produces the self-contained object for the ecvisor
// multi-binary link.
//
// The per-program registry fragment (generated by internal/link, referencing
// this object's standard `_ecv_*` symbols) is llvm-link'd in, then internalize
// makes the `_ecv_*` + lifted funcs file-local so N objects link without
// collision. This stays entirely in bitcode — no llvm-dis/as text roundtrip —
// which is what lets the wasm backend keep -O2 on the lifted indirectbr tables.
func (c *TranslateOne) codegenEcvisor(sdk, bc, obj string, libBCs []string) error {
	t := newLLVMTools(sdk)
	p := func(ext string) string { return filepath.Join(c.Out, c.ModuleID+ext) }

	if err := c.tm.step("clang-fragment", c.Fragment, p(".frag.bc"), func() error {
		return run(t.cc, append(append([]string{}, t.target...),
			"-O2", t.sysroot, "-emit-llvm", "-c", c.Fragment, "-o", p(".frag.bc"))...)
	}); err != nil {
		return err
	}

	nsbc := p(".ns.bc")

	// ONE PARSE INSTEAD OF THREE. The default; ECV_NO_MERGED_PREPARE restores
	// the three-tool chain below.
	//
	// llvm-link, opt -passes=internalize,globaldce and namespace-object each
	// parsed and re-serialized the whole module. Measured on bash-glibc,
	// 2026-08-13: a bare `opt -passes=` round trip of the merged bitcode is
	// 4.999 s and the real internalize+globaldce is 5.057 s, so that pass is
	// 0.06 s of work behind a 4.7 s parse -- and the parse was paid three times
	// over. ecv-prepare does all three with the module in memory once: 3.25 s
	// against 13.30 s, and a partition-cache-warm translation 22.85 s against
	// 32.51 s.
	//
	// See stablesplit.go for what the flip did and did not keep byte-identical
	// (ecv-split: identical; llvm-split: not, and why that is not a regression
	// in kind), and builder/ecv-prepare.cpp for the tool.
	if mergedPrepareEnabled() {
		// AND THE SPLIT, when the partitioner is ours. ecv-prepare holds the
		// module already, so partitioning in the same process drops the last
		// round trip: the ~28 MB .ns.bc write plus the next process's parse of
		// it, measured at 4.324 s on bash-glibc against 5.6 s for the whole
		// split. Unavailable under ECV_NO_STABLE_SPLIT, because llvm-split is an
		// external binary that cannot be called on an in-memory module — which
		// is the same reason ecv-split exists at all. That is not a gap in
		// practice: the partition cache only ever hits under the stable
		// partitioner, so the warm path this saves on is exactly the one that
		// has it — and since 2026-08-23 that is the default, so this branch and
		// not the one below is the ordinary path.
		if stableSplitEnabled() {
			if err := c.prepareAndSplitAndCompile(t, bc, p(".frag.bc"), nsbc, obj, libBCs); err != nil {
				return err
			}
			fmt.Printf("translate-one: built %s (runtime: ecvisor, %s)\n", obj, c.Keep)
			return nil
		}
		if err := c.tm.step("ecv-prepare", bc, nsbc, func() error {
			args := []string{bc, p(".frag.bc"), nsbc, c.ModuleID, c.Keep}
			for _, lib := range libBCs {
				args = append(args, "--merge", lib)
			}
			return run("ecv-prepare", args...)
		}); err != nil {
			return err
		}
		if err := c.splitAndCompile(t, nsbc, obj); err != nil {
			return err
		}
		fmt.Printf("translate-one: built %s (runtime: ecvisor, %s)\n", obj, c.Keep)
		return nil
	}

	if err := c.tm.step("llvm-link", bc, p(".merged.bc"), func() error {
		return run("llvm-link-"+t.ver, bc, p(".frag.bc"), "-o", p(".merged.bc"))
	}); err != nil {
		return err
	}
	if err := c.tm.step("opt-internalize-globaldce", p(".merged.bc"), p(".mi.bc"), func() error {
		return run("opt-"+t.ver, "-passes=internalize,globaldce",
			"--internalize-public-api-list="+c.Keep, p(".merged.bc"), "-o", p(".mi.bc"))
	}); err != nil {
		return err
	}

	// NAMESPACING, then split. Both steps are required, and the order matters.
	//
	// A plain split externalizes every cross-part local to hidden-external and
	// `wasm-ld -r` keeps them hidden-but-GLOBAL, leaving thousands of external
	// symbols in the object instead of 1, so two programs collide at the ecvisor
	// link on the shared remill helpers (__remill_sync_hyper_call, the (anonymous
	// namespace):: semantics templates).
	//
	// llvm-split --preserve-locals suppresses the promotion, but at real scale it
	// defeats splitting outright: after internalize almost everything is local, so
	// anything mutually reachable lands in ONE partition. On a fused glibc binary
	// it emitted a 35.7 MB part plus 80 small ones — serial codegen, which is
	// what splitting exists to avoid.
	//
	// namespace-object resolves the tension: tag every local with the module id
	// BEFORE splitting, so llvm-split may promote freely and the promoted names
	// stay unique per program. It cannot be done after codegen: llvm-objcopy on
	// wasm supports "only flags for section dumping, removal, and addition" (no
	// --redefine-syms; --localize-hidden no-ops).
	if err := c.tm.step("namespace-object", p(".mi.bc"), nsbc, func() error {
		return run("namespace-object", p(".mi.bc"), nsbc, c.ModuleID, c.Keep)
	}); err != nil {
		return err
	}

	if err := c.splitAndCompile(t, nsbc, obj); err != nil {
		return err
	}
	fmt.Printf("translate-one: built %s (runtime: ecvisor, %s)\n", obj, c.Keep)
	return nil
}

// splitAndCompile is the split-and-parallel codegen.
//
// A big fused guest (postgres: ~59k functions) makes a single serial clang
// codegen the dominant build cost (>2h). Split the INTERNALIZED module into many
// parts and compile them concurrently across all cores, then relocatably link
// into the one object the ecvisor link expects.
//
// CORRECTED 2026-08-11. This comment used to claim "no single function dominates
// (measured: even base_yyparse compiles in <1s) — the cost is the sheer function
// count — so it parallelizes near-linearly". Measured per-partition on
// aptget-glibc, that is wrong in both halves. Codegen time is set by the LARGEST
// SINGLE FUNCTION in a partition, superlinearly, and barely tracks total mass:
//
//	part  seconds  largest fn  total insts
//	p29      93.8      51,198      165,090
//	p35      20.1      19,792      186,904   <- most mass, 4.7x faster
//	p3        2.8      11,315      114,453
//
// So it does NOT parallelize near-linearly: the wall is one single-threaded
// clang on the biggest function (here a real 35,180-byte glibc function, from a
// genuine .eh_frame FDE), and the other 79 partitions together need only ~6
// cores to finish inside its shadow. Raising the part count cannot help, and
// neither can balancing by size. See JOURNAL.md 2026-08-11.
//
// More parts than cores (4x) gives the scheduler slack. Do NOT raise it further
// to chase the tail: measured on a 34.9 MB module, going from -j 80 to -j 320
// left the largest partition at ~8 MB (llvm-split keeps strongly connected
// components together, so it is indivisible) while total bytes nearly tripled,
// since every extra part carries its own declarations.
// The partition COUNT and the codegen CONCURRENCY are deliberately separate.
//
// Both used to derive from runtime.NumCPU(), which tied what the pipeline emits
// to the machine it ran on: the same ELF partitioned into 80 parts on a 20-core
// box and 128 on a 32-core one, so no partition was byte-identical between them
// and the content-addressed partition cache could never hit across machines. It
// also means concurrency cannot be capped -- which cross-program overlap
// requires -- without changing the emitted split.
//
// nparts is therefore fixed. jobs is the only knob a caller may turn, and it
// changes scheduling alone, never output.
const nparts = 80

func (c *TranslateOne) splitAndCompile(t llvmTools, nsbc, obj string) error {
	njobs := c.Jobs
	if njobs <= 0 {
		njobs = runtime.NumCPU()
	}
	splitDir := filepath.Join(c.Out, c.ModuleID+".split.d")
	if err := os.RemoveAll(splitDir); err != nil {
		return err
	}
	if err := os.MkdirAll(splitDir, 0o755); err != nil {
		return err
	}
	// Kept out of the p* namespace so it is not mistaken for a partition.
	splitLog := filepath.Join(splitDir, "llvm-split.stderr")

	var parts []part
	var splitErr error
	splitName := "llvm-split"
	if stableSplitEnabled() {
		splitName = "ecv-split"
	}
	_ = c.tm.step(splitName, nsbc, "", func() error {
		if stableSplitEnabled() {
			parts, splitErr = c.stableSplit(nparts, nsbc, splitDir)
		} else {
			parts, splitErr = c.split(t, nparts, nsbc, splitDir, splitLog)
		}
		return nil
	})
	if splitErr != nil {
		return c.splitFailed(t, splitDir, splitLog, splitErr, nsbc, obj)
	}
	return c.compileAndLink(t, parts, splitDir, nsbc, obj, njobs)
}

// prepareAndSplitAndCompile is splitAndCompile for the path where ecv-prepare
// also does the partitioning, so there is no separate split step to time or to
// recover from. It shares compileAndLink with the split-as-its-own-process path,
// which is what keeps the scheduling, the link order and the cleanup identical
// between them.
func (c *TranslateOne) prepareAndSplitAndCompile(t llvmTools, bc, fragbc, nsbc, obj string, libBCs []string) error {
	njobs := c.Jobs
	if njobs <= 0 {
		njobs = runtime.NumCPU()
	}
	splitDir := filepath.Join(c.Out, c.ModuleID+".split.d")
	if err := os.RemoveAll(splitDir); err != nil {
		return err
	}
	if err := os.MkdirAll(splitDir, 0o755); err != nil {
		return err
	}

	var parts []part
	var err error
	// One phase, named for what it is: the three passes AND the split, against a
	// single parse. It replaces the "ecv-prepare" and "ecv-split" rows, so a
	// timing.json from this path has one fewer phase than one from the other --
	// worth knowing before comparing two runs by phase name.
	_ = c.tm.step("ecv-prepare-split", bc, "", func() error {
		parts, err = c.prepareAndSplit(nparts, bc, fragbc, nsbc, splitDir, libBCs)
		return nil
	})
	if err != nil {
		return err
	}
	return c.compileAndLink(t, parts, splitDir, bc, obj, njobs)
}

// compileAndLink is the second half of every split path: schedule the partitions,
// compile them concurrently, and relocatably link the results into the one object
// the ecvisor link expects.
func (c *TranslateOne) compileAndLink(t llvmTools, parts []part, splitDir, in, obj string, njobs int) error {
	// LARGEST FIRST. The partition sizes are wildly uneven — on the fused OpenSSL
	// closure the median part is under 800 KB and two are ~7.5 MB — and the
	// biggest one alone can take longer than every other part put together. In
	// lexical order that part may not start until the last scheduling wave, which
	// adds the whole preceding queue to the wall clock; started first, it runs
	// alongside everything else. This is plain longest-processing-time scheduling
	// and it cannot be worse than any other static order.
	sort.Slice(parts, func(i, j int) bool { return parts[i].size > parts[j].size })

	var objs []string
	var compileErr error
	// One aggregate phase for the whole parallel section; the per-partition
	// records under "parts" are what expose the tail inside it.
	_ = c.tm.step("codegen-parts", in, "", func() error {
		objs, compileErr = c.compileParts(t, parts, njobs)
		return nil
	})
	if compileErr != nil {
		return compileErr
	}
	if err := c.tm.step("wasm-ld-r", "", obj, func() error {
		return run(t.wasmLD, append([]string{"-r"}, append(objs, "-o", obj)...)...)
	}); err != nil {
		return err
	}

	// ECV_KEEP_SPLIT preserves the partitions for diagnosis. Removing them on
	// success is what destroyed the evidence for the 17-minute partition.
	if os.Getenv("ECV_KEEP_SPLIT") != "" {
		return nil
	}
	return os.RemoveAll(splitDir)
}

type part struct {
	path string
	size int64
}

// split runs llvm-split and returns the partitions it produced. Exit 0 with no
// parts is the worse failure: nothing to report, but the module silently
// unsplit.
func (c *TranslateOne) split(t llvmTools, n int, nsbc, splitDir, splitLog string) ([]part, error) {
	err := runCapture(splitLog, "llvm-split-"+t.ver,
		"-j", fmt.Sprint(n), "-o", filepath.Join(splitDir, "p"), nsbc)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(splitDir, "p*"))
	if err != nil {
		return nil, err
	}
	var parts []part
	for _, m := range matches {
		if m == splitLog {
			continue
		}
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue
		}
		parts = append(parts, part{path: m, size: info.Size()})
	}
	if len(parts) == 0 {
		msg := fmt.Sprintf("llvm-split-%s exited 0 but produced no partitions\n", t.ver)
		appendFile(splitLog, msg)
		return nil, fmt.Errorf("%s", strings.TrimSpace(msg))
	}
	return parts, nil
}

// compileParts compiles the partitions concurrently, njobs at a time.
//
// -O1 (not -O2) for the final lifted-object codegen: -O2's SSA/GVN passes are
// superlinear and gave no measurable runtime benefit here.
func (c *TranslateOne) compileParts(t llvmTools, parts []part, njobs int) ([]string, error) {
	// Content-addressed reuse, when the host mounted a store. See partcache.go
	// for exactly which builds can share a partition today.
	cache := newPartCache(t, "-O1")
	defer func() {
		if s := cache.summary(); s != "" {
			fmt.Fprintf(os.Stderr, "translate-one: %s\n", s)
		}
	}()

	objs := make([]string, len(parts))
	sem := make(chan struct{}, njobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, pt := range parts {
		objs[i] = pt.path + ".o"
		wg.Add(1)
		go func(in, out string, size int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			stop := firstErr != nil
			mu.Unlock()
			if stop {
				return
			}
			// Timed from acquiring a slot, not from being queued, so the record
			// is the partition's own compile cost rather than its wait.
			started := time.Now()

			// A cache hit still gets a timing record, so the report shows which
			// partitions were served rather than compiled.
			key, keyErr := "", error(nil)
			if cache != nil {
				key, keyErr = cache.key(in)
				if keyErr == nil && cache.get(key, out) {
					c.tm.part(filepath.Base(in)+" (cached)", size, time.Since(started))
					return
				}
			}

			err := t.compileIR("-O1", in, out, true)
			c.tm.part(filepath.Base(in), size, time.Since(started))
			if err == nil && cache != nil && keyErr == nil {
				cache.put(key, out)
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(pt.path, objs[i], pt.size)
	}
	wg.Wait()
	return objs, firstErr
}

// splitFailed reports a broken split and, by default, refuses to continue.
//
// A FAILED SPLIT IS AN ERROR, NOT A FALLBACK. This used to discard llvm-split's
// stderr and drop through to single-shot codegen, which on a fused guest does
// not finish at all — measured on the OpenSSL closure, killed at 45 minutes with
// the object still 0 bytes. So a split that broke for any reason presented as
// "the build is mysteriously taking hours", with nothing on stderr saying why.
// Printing a warning is not enough either: internal/translate.Run keeps this
// program's stderr only when the run FAILS, so a warning on the success path is
// never seen.
//
// Set ECV_ALLOW_SERIAL_CODEGEN=1 to take the serial path deliberately. It is
// viable only for small modules.
func (c *TranslateOne) splitFailed(t llvmTools, splitDir, splitLog string, cause error, nsbc, obj string) error {
	fmt.Fprintf(os.Stderr, "translate-one: llvm-split-%s failed on %s:\n", t.ver, nsbc)
	if log, err := os.ReadFile(splitLog); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(log), "\n"), "\n") {
			fmt.Fprintf(os.Stderr, "    %s\n", line)
		}
	}
	os.RemoveAll(splitDir)

	if os.Getenv("ECV_ALLOW_SERIAL_CODEGEN") == "" {
		fmt.Fprintln(os.Stderr, "translate-one: refusing to fall back to single-shot codegen -- it does not")
		fmt.Fprintln(os.Stderr, "  complete on a fused guest (see the comment above). Fix the split, or set")
		fmt.Fprintln(os.Stderr, "  ECV_ALLOW_SERIAL_CODEGEN=1 to compile serially anyway.")
		return fmt.Errorf("llvm-split-%s failed: %w", t.ver, cause)
	}
	fmt.Fprintf(os.Stderr, "translate-one: ECV_ALLOW_SERIAL_CODEGEN=1 -- compiling %s in one shot\n", nsbc)
	return t.compileIR("-O1", nsbc, obj, false)
}

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(s)
}
