package builder

import (
	"slices"
	"testing"
)

// The expectations below are the command lines builder/translate-one.sh
// produced, transcribed from the script before it was ported. They exist to
// catch a flag lost or reworded in translation — the failure mode a port like
// this actually has, and one that would otherwise surface as a miscompiled
// guest hours into a build.

func TestLiftArgsMatchTheScript(t *testing.T) {
	c := &TranslateOne{ELF: "/in/prog"}
	want := []string{
		"--arch", "aarch64",
		"--bc_out", "/out/prog_ab12.bc",
		"--target_elf", "/in/prog",
		"--dbg_fun_vma", "0",
		"--bitcode_path", "",
		"--target_arch", "wasi32",
		"--float_exception", "0",
		"--norm_mode", "1",
		"--fork_emulation", "1",
	}
	if got := c.liftArgs("/out/prog_ab12.bc"); !slices.Equal(got, want) {
		t.Errorf("liftArgs:\n got %q\nwant %q", got, want)
	}
}

func TestWasiFlagsMatchTheScript(t *testing.T) {
	c := &TranslateOne{ELF: "/in/prog"}
	want := []string{
		"-O3", "-std=c++20",
		"--sysroot=/wasi/share/wasi-sysroot",
		"-D_WASI_EMULATED_SIGNAL", "-D_WASI_EMULATED_PROCESS_CLOCKS", "-D_WASI_EMULATED_MMAN",
		"-I/root/elfconv/backend/remill/include", "-I/root/elfconv",
		"-fno-exceptions",
		"-DTARGET_IS_WASI=1", "-DELF_IS_AARCH64",
		`-DELFNAME="prog"`,
	}
	if got := c.wasiFlags("/wasi"); !slices.Equal(got, want) {
		t.Errorf("wasiFlags:\n got %q\nwant %q", got, want)
	}
}

// ELFNAME carries literal quotes into the macro value. The script wrote
// -DELFNAME="\"${ELFNAME}\"", where the shell strips one layer and passes the
// inner quotes through; dropping them would define an undeclared identifier
// instead of a string.
func TestELFNameKeepsItsQuotes(t *testing.T) {
	c := &TranslateOne{ELF: "/in/some/dir/openssl"}
	flags := c.wasiFlags("/wasi")
	want := `-DELFNAME="openssl"`
	if !slices.Contains(flags, want) {
		t.Errorf("wasiFlags missing %q, got %q", want, flags)
	}
}

func TestUpstreamArgsMatchTheScript(t *testing.T) {
	c := &TranslateOne{ELF: "/in/prog"}
	base := c.wasiFlags("/wasi")

	wantObj := append(slices.Clone(base), "-c", "/out/m.bc", "-o", "/out/m.o")
	if got := c.upstreamObjArgs("/wasi", "/out/m.bc", "/out/m.o"); !slices.Equal(got, wantObj) {
		t.Errorf("upstreamObjArgs:\n got %q\nwant %q", got, wantObj)
	}

	wantWasm := append(slices.Clone(base),
		"-lwasi-emulated-process-clocks", "-lwasi-emulated-mman", "-lwasi-emulated-signal",
		"-o", "/out/m.wasm", "/out/m.o",
		"/root/elfconv/runtime/Entry.cpp",
		"/root/elfconv/runtime/Memory.cpp",
		"/root/elfconv/runtime/Runtime.cpp",
		"/root/elfconv/runtime/VmIntrinsics.cpp",
		"/root/elfconv/utils/Util.cpp",
		"/root/elfconv/utils/elfconv.cpp",
		"/root/elfconv/runtime/syscalls/SyscallWasi.cpp",
	)
	if got := c.upstreamWasmArgs("/wasi", "/out/m.o", "/out/m.wasm"); !slices.Equal(got, wantWasm) {
		t.Errorf("upstreamWasmArgs:\n got %q\nwant %q", got, wantWasm)
	}
}

// The LLVM-16 line uses the wasi-sdk clang with no --target; every other line
// uses the system clang-<ver> with one. Bitcode must be read by a toolchain at
// least as new as the one that wrote it, so a mismatch here is not a warning —
// llvm-link simply cannot parse the module.
func TestLLVMToolsSelection(t *testing.T) {
	t.Setenv("ECV_LLVM_VER", "16")
	got := newLLVMTools("/wasi")
	if got.cc != "/wasi/bin/clang" || len(got.target) != 0 || got.wasmLD != "/wasi/bin/wasm-ld" {
		t.Errorf("LLVM 16: cc=%q target=%q wasm-ld=%q", got.cc, got.target, got.wasmLD)
	}

	t.Setenv("ECV_LLVM_VER", "22")
	got = newLLVMTools("/wasi")
	if got.cc != "clang-22" || !slices.Equal(got.target, []string{"--target=wasm32-wasi"}) ||
		got.wasmLD != "/usr/lib/llvm-22/bin/wasm-ld" {
		t.Errorf("LLVM 22: cc=%q target=%q wasm-ld=%q", got.cc, got.target, got.wasmLD)
	}
}

func TestLLVMToolsDefaultsTo16(t *testing.T) {
	t.Setenv("ECV_LLVM_VER", "")
	if got := newLLVMTools("/wasi"); got.ver != "16" {
		t.Errorf("default ECV_LLVM_VER = %q, want 16", got.ver)
	}
}

func TestRejectsUnsupportedTarget(t *testing.T) {
	c := &TranslateOne{ELF: "/in/prog", Out: "/out", ModuleID: "m", Target: "x86_64-wasi32", Runtime: "upstream"}
	err := c.Run()
	if err == nil {
		t.Fatal("expected an error for a non-aarch64 target")
	}
	if _, ok := err.(*UsageError); !ok {
		t.Errorf("want UsageError (exit 2), got %T: %v", err, err)
	}
}

// --runtime ecvisor without --fragment/--keep must fail before doing any work:
// internalize would otherwise keep nothing and the object would link empty.
func TestEcvisorRequiresFragmentAndKeep(t *testing.T) {
	c := &TranslateOne{ELF: "/in/prog", Out: "/out", ModuleID: "m", Target: "aarch64-wasi32", Runtime: "ecvisor"}
	err := c.Run()
	if _, ok := err.(*UsageError); !ok {
		t.Errorf("want UsageError (exit 2), got %T: %v", err, err)
	}
}

// Every translated object is built PIC so that ONE object set serves both the
// stock-shim single-module link and the embedder's `-shared` side-module link.
// See compileIR's comment for the measurements that make it affordable.
//
// This asserts the flag reaches clang. That the resulting object still links
// FLAT with an unchanged import surface — the property the scheme actually rests
// on — is not something an argument list can show; e2e/imports_test.go holds it,
// by asserting a real module imports no `__memory_base`.
func TestCompileIREmitsPIC(t *testing.T) {
	tools := llvmTools{cc: "clang", target: []string{"--target=wasm32-wasi"}, sysroot: "--sysroot=/x"}
	for _, asIR := range []bool{true, false} {
		args := tools.compileIRArgs("-O1", "in.bc", "out.o", asIR)
		if !slices.Contains(args, "-fPIC") {
			t.Errorf("asIR=%v: -fPIC missing from %q", asIR, args)
		}
	}
}

// The partitioner default. ecv-split was promoted from ECV_STABLE_SPLIT to the
// default on 2026-08-23, which is a change of BYTES: llvm-split and ecv-split
// assign differently, so a silent revert here would emit different objects under
// a cache key that says otherwise.
//
// ⚠️ Read the assertions, not the switch name. The third case is the one that
// matters most and is the easiest to leave out: the OLD switch is gone, and a
// shell still exporting it must not select a partitioner at all -- if it were
// ever wired back up as a toggle, an environment carrying both would get
// whichever check ran first.
func TestStableSplitIsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"clean environment", nil, true},
		{"ECV_NO_STABLE_SPLIT turns it off", map[string]string{"ECV_NO_STABLE_SPLIT": "1"}, false},
		{"the retired switch is inert", map[string]string{"ECV_STABLE_SPLIT": "1"}, true},
		{"the retired switch cannot re-enable it", map[string]string{
			"ECV_STABLE_SPLIT": "1", "ECV_NO_STABLE_SPLIT": "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ECV_STABLE_SPLIT", "")
			t.Setenv("ECV_NO_STABLE_SPLIT", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := stableSplitEnabled(); got != tc.want {
				t.Errorf("stableSplitEnabled() = %v, want %v with env %v\n"+
					"The partitioner selects which bytes translate-one emits; "+
					"changing this default is a decision, not a refactor.", got, tc.want, tc.env)
			}
		})
	}
}
