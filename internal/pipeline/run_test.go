package pipeline

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRuntimeArgsSpellDirTheRightWayRound is the guard for a mistake that does
// not fail loudly.
//
// The two runtimes take `--dir` in OPPOSITE orders, verified against wasmedge
// 0.17.1 and wasmtime 46.0.1:
//
//	wasmedge --dir GUEST:HOST
//	wasmtime --dir HOST::GUEST
//
// Swap either and the runtime still starts: it opens *something*, the guest then
// cannot read its sidecar, ecvisor reports the rootfs "set but unreadable", and
// every dlopen fails with "cannot open shared object file" -- which reads as a
// broken guest rather than a broken command line.
func TestRuntimeArgsSpellDirTheRightWayRound(t *testing.T) {
	const dir = "/work/out"
	const mod = "/work/out/app.wasm"
	env := []string{"RAPTORMARK_ROOTFS=/rootfs.img"}

	edge := runtimeArgs("wasmedge", dir, mod, env, nil)
	if i := slices.Index(edge, "--dir"); i < 0 || edge[i+1] != "/:"+dir {
		t.Errorf("wasmedge --dir is %q, want %q (GUEST:HOST)", edge, "/:"+dir)
	}
	time := runtimeArgs("wasmtime", dir, mod, env, nil)
	if i := slices.Index(time, "--dir"); i < 0 || time[i+1] != dir+"::/" {
		t.Errorf("wasmtime --dir is %q, want %q (HOST::GUEST)", time, dir+"::/")
	}
	// And they must NOT be the same string, which is the whole hazard: a single
	// shared spelling would be wrong for one of them and this test would be the
	// only thing that noticed.
	ei, ti := slices.Index(edge, "--dir"), slices.Index(time, "--dir")
	if edge[ei+1] == time[ti+1] {
		t.Error("both runtimes were given the same --dir spelling; they are reversed")
	}

	// ⚠️ wasmedge accepts --env only BEFORE the module path. Anything after it
	// is handed to the GUEST instead, silently -- so the variable is simply
	// absent and the sidecar is never read.
	mi := slices.Index(edge, mod)
	if mi < 0 {
		t.Fatalf("the module is not in the wasmedge argv: %q", edge)
	}
	for i, a := range edge {
		if a == "--env" && i > mi {
			t.Errorf("wasmedge got --env AFTER the module path (%q); it would be "+
				"passed to the guest and the sidecar would never be read", edge)
		}
	}
}

// TestWasmerArgsUseVolumeHostFirstAndEnableTheNetwork covers the third runtime,
// which manages to differ from both of the others in two ways at once.
//
// ⚠️ THE FLAG. wasmer 7.3.0 spells it `--volume HOST:GUEST` -- the wasmtime
// ORDER with the wasmedge SEPARATOR. Its `--mapdir GUEST:HOST` is the wasmedge
// spelling exactly, and is deprecated with a warning that it goes in the next
// major, so a copy of either neighbour's line is wrong and one of them is
// wrong silently.
//
// ❗ AND `--net`. Without it a WASIX guest's `sock_open` SUCCEEDS and its
// `sock_bind` returns errno 58, so the run reports nothing until the first
// bind. That is the whole reason the wasix profile exists, so a wasmer
// invocation without it is never what anyone meant.
//
// Both measured against wasmer 7.3.0 with a `path_open` probe rather than read
// off `--help`: `fd_prestat_dir_name` answers "/" whichever order it is given,
// so it cannot tell the two apart. See `.agents/docs/WASIX_ABI.md`.
func TestWasmerArgsUseVolumeHostFirstAndEnableTheNetwork(t *testing.T) {
	const dir = "/work/out"
	const mod = "/work/out/app.wasm"
	args := runtimeArgs("wasmer", dir, mod, []string{"RAPTORMARK_ROOTFS=/rootfs.img"}, nil)

	i := slices.Index(args, "--volume")
	if i < 0 || args[i+1] != dir+":/" {
		t.Errorf("wasmer volume is %q, want %q (HOST:GUEST, one colon)", args, dir+":/")
	}
	if slices.Contains(args, "--mapdir") {
		t.Error("wasmer got --mapdir, which is deprecated in 7.3.0 and warns on every run")
	}
	if slices.Contains(args, "--dir") {
		t.Errorf("wasmer got --dir, which is another runtime's flag: %q", args)
	}
	if !slices.Contains(args, "--net") {
		t.Errorf("wasmer was not given --net (%q). Without it sock_open succeeds "+
			"and sock_bind returns errno 58, so the guest fails to bind an "+
			"address that is perfectly fine and nothing points at the flag", args)
	}

	// ⚠️ THE EXCLUSION HALF. The other two runtimes must be untouched by any of
	// this -- neither gains --net, and neither starts spelling its directory
	// wasmer's way.
	for _, rt := range []string{"wasmedge", "wasmtime"} {
		other := runtimeArgs(rt, dir, mod, nil, nil)
		if slices.Contains(other, "--net") {
			t.Errorf("%s was given --net, which is a wasmer flag: %q", rt, other)
		}
		if slices.Contains(other, "--volume") {
			t.Errorf("%s was given --volume, which is a wasmer flag: %q", rt, other)
		}
	}
}

// TestRuntimeArgsPassGuestArgsLast checks the guest's own argv survives, and
// after the module rather than before it -- where it would be consumed as
// runtime flags.
func TestRuntimeArgsPassGuestArgsLast(t *testing.T) {
	for _, rt := range []string{"wasmedge", "wasmtime", "wasmer"} {
		args := runtimeArgs(rt, "/d", "/d/app.wasm", nil, []string{"--serve", "9000"})
		mi := slices.Index(args, "/d/app.wasm")
		gi := slices.Index(args, "--serve")
		if mi < 0 || gi < 0 {
			t.Fatalf("%s: module or guest arg missing from %q", rt, args)
		}
		if gi < mi {
			t.Errorf("%s: guest args come BEFORE the module (%q); the runtime would "+
				"try to interpret them as its own flags", rt, args)
		}
	}
}

// TestRunRefusesASidecarOutsideThePreopen is the guard for the trap this command
// exists to remove.
//
// Only the module's directory is preopened, so a sidecar elsewhere is
// unreachable however RAPTORMARK_ROOTFS is spelled. Accepting it would produce a
// run that starts, silently has no rootfs, and fails later somewhere unrelated.
func TestRunRefusesASidecarOutsideThePreopen(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	mod := filepath.Join(dir, "app.wasm")
	if err := os.WriteFile(mod, []byte("\x00asm"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(other, "rootfs.img")
	if err := os.WriteFile(sidecar, []byte("rfs"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Run{Module: mod, Sidecar: sidecar, Runtime: "wasmedge"}
	err := c.Run()
	if err == nil {
		t.Fatal("a sidecar outside the preopened directory was accepted; the guest " +
			"could not have read it, and the run would fail later as if the guest " +
			"were at fault")
	}
	// The message must say WHY, not just "invalid": the reader's next move is
	// otherwise to try a different RAPTORMARK_ROOTFS spelling, which cannot work.
	for _, want := range []string{"preopened", "set but unreadable", "beside the module"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// THE CONTROL. A sidecar in the right place must NOT be refused for this
	// reason -- otherwise the check above would pass on an implementation that
	// rejects everything. It will still fail (no runtime binary in the sandbox,
	// or a bogus module), so assert on the message rather than on success.
	good := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(good, []byte("rfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	c2 := &Run{Module: mod, Sidecar: good, Runtime: "wasmedge", Bin: "/nonexistent/wasmedge"}
	if err := c2.Run(); err != nil && strings.Contains(err.Error(), "preopened") {
		t.Errorf("a sidecar BESIDE the module was refused as outside the preopen: %v", err)
	}
}

// TestRunDefaultsTheSidecarBesideTheModule pins the ergonomics that make the
// command worth having: `raptormark run app.wasm` with no flags must find the
// rootfs.img that `raptormark build` wrote next to it.
func TestRunDefaultsTheSidecarBesideTheModule(t *testing.T) {
	dir := t.TempDir()
	mod := filepath.Join(dir, "app.wasm")
	if err := os.WriteFile(mod, []byte("\x00asm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), []byte("rfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Run{Module: mod, Runtime: "wasmedge", Bin: "/nonexistent/wasmedge"}
	err := c.Run()
	if err == nil {
		t.Fatal("expected the missing runtime binary to fail the run")
	}
	// It got as far as exec, which means the sidecar was found and accepted:
	// a missing default would have printed the no-sidecar warning and still
	// reached exec, so this alone is not enough -- hence the refusal test above
	// pairs with it.
	if strings.Contains(err.Error(), "preopened") {
		t.Errorf("the defaulted sidecar was rejected as outside the preopen: %v", err)
	}
}
