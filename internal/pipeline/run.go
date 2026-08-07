package pipeline

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Run is `raptormark run <module>`: execute a built module with its sidecar.
//
// # Why this is a command and not a line in a README
//
// The invocation has one combination that must be right and fails in a way that
// points somewhere else when it is not: the guest reaches its rfs sidecar
// through `RAPTORMARK_ROOTFS`, which names a path in the GUEST's namespace, and
// that namespace only exists if the sidecar's directory was preopened. Get it
// wrong and ecvisor reports the rootfs "set but unreadable", runs with no exec
// map and no dlopen map, and then every `execve` falls back to program 0 and
// every `dlopen` fails with "cannot open shared object file".
//
// ⚠️ Both of those look exactly like a defect in the feature under test. It cost
// this repository a debugging session at least twice -- once with a comment two
// lines above the mistake warning about the same class of error. Encoding it in
// a command is cheaper than remembering it.
type Run struct {
	Module string `arg:"" type:"existingfile" help:"The .wasm module to run, as built by 'raptormark build'."`

	Sidecar string   `type:"path" help:"The rfs sidecar. Defaults to rootfs.img beside the module, if present."`
	Runtime string   `default:"wasmedge" enum:"wasmedge,wasmtime,wasmer" help:"Which host runtime to exec. The DEFAULT profile imports WasmEdge's socket extensions, so wasmtime can only run a --profile loopback or browser module. wasmer is for --profile wasix, whose sockets come from the wasix_32v1 namespace no other engine has."`
	Bin     string   `help:"Path to the runtime binary. Default: look it up on PATH, then ~/.<runtime>/bin (which is where all three installers put it)."`
	Env     []string `short:"e" help:"Extra KEY=VALUE for the guest, repeatable. RAPTORMARK_ROOTFS is set automatically."`

	Args []string `arg:"" optional:"" passthrough:"" help:"Arguments passed to the guest."`
}

func (c *Run) Run() error {
	mod, err := filepath.Abs(c.Module)
	if err != nil {
		return err
	}
	dir := filepath.Dir(mod)

	// ❗ THE SIDECAR MUST LIVE INSIDE THE PREOPENED DIRECTORY.
	//
	// Only the module's own directory is granted, so a sidecar somewhere else is
	// unreachable no matter how RAPTORMARK_ROOTFS is spelled. Refusing here is
	// the whole point: the alternative is a run that starts, ignores its
	// filesystem, and fails later as if the guest were at fault.
	sidecar := c.Sidecar
	if sidecar == "" {
		if def := filepath.Join(dir, "rootfs.img"); fileExists(def) {
			sidecar = def
		}
	}
	var env []string
	if sidecar != "" {
		abs, err := filepath.Abs(sidecar)
		if err != nil {
			return err
		}
		if filepath.Dir(abs) != dir {
			return fmt.Errorf(
				"the sidecar %s is not in the module's directory (%s).\n"+
					"Only that directory is preopened, so the guest could not read it: "+
					"ecvisor would report the rootfs \"set but unreadable\", run with no "+
					"exec map and no dlopen map, and then every execve falls back to "+
					"program 0 and every dlopen fails with \"cannot open shared object "+
					"file\" -- which looks like a defect in the guest, not in this "+
					"invocation.\nMove it beside the module, or pass a module in the same "+
					"directory", abs, dir)
		}
		// The GUEST path, which is what the runtime variable means. The host
		// path would be a directory the guest cannot see.
		env = append(env, "RAPTORMARK_ROOTFS=/"+filepath.Base(abs))
	} else {
		fmt.Fprintln(os.Stderr,
			"raptormark run: no sidecar beside the module. The guest gets no rootfs, "+
				"no exec map and no dlopen map -- fine for a single-program module with "+
				"no filesystem, wrong for anything built by `raptormark build`.")
	}
	env = append(env, c.Env...)

	bin, err := c.resolveBin()
	if err != nil {
		return err
	}

	args := runtimeArgs(c.Runtime, dir, mod, env, c.Args)
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		// ❗ RETURNED, NOT `os.Exit`ed, and the difference is not cosmetic.
		//
		// This called `os.Exit(ee.ExitCode())` here. That makes the function
		// unusable from anything but `main`: it kills the process mid-call, so a
		// test cannot assert on what happened, `t.TempDir` cleanup never runs,
		// and any caller that wanted to do something afterwards silently does
		// not. It was found when a NEUTRALIZATION of a different guard let the
		// run reach exec -- the whole test binary exited and the assertion that
		// should have fired never printed.
		//
		// The exit code still survives: `main` unwraps this and exits with it.
		if ee, ok := err.(*exec.ExitError); ok {
			return &GuestExitError{Code: ee.ExitCode()}
		}
		return fmt.Errorf("running %s: %w", bin, err)
	}
	return nil
}

// GuestExitError carries a guest's non-zero exit status out to `main`.
//
// A guest exiting non-zero is its ANSWER, not this command failing, so the
// status has to reach the shell unchanged -- a build script testing `$?` cares
// about the difference between the guest returning 3 and raptormark failing.
type GuestExitError struct{ Code int }

func (e *GuestExitError) Error() string {
	return fmt.Sprintf("the guest exited %d", e.Code)
}

// ExitCode makes the status reachable without a type assertion at the call site.
func (e *GuestExitError) ExitCode() int { return e.Code }

// runtimeArgs builds the argv for one runtime.
//
// ⚠️ THE THREE RUNTIMES SPELL THE DIRECTORY FLAG THREE DIFFERENT WAYS, verified
// against wasmedge 0.17.1, wasmtime 46.0.1 and wasmer 7.3.0 rather than assumed:
//
//	wasmedge --dir    GUEST:HOST      (e.g. --dir    /:/out)
//	wasmtime --dir    HOST::GUEST     (e.g. --dir    /out::/)
//	wasmer   --volume HOST:GUEST      (e.g. --volume /out:/)
//
// ⚠️ wasmer's is the wasmtime ORDER with the wasmedge SEPARATOR, which is the
// worst of both: one colon, host first. It also has a `--mapdir GUEST:HOST`
// that is the wasmedge spelling exactly -- and is DEPRECATED in 7.3.0, warning
// that it goes away in the next major. `.agents/docs/LTM/wasix-and-wasmer.md` records
// `--mapdir` from the Phase 5 work, which still runs but is not what to write
// now. Measured with a `path_open` probe rather than read off `--help`:
// `fd_prestat_dir_name` answers "/" for the first preopen whatever it was
// given, so it cannot tell the two orders apart.
//
// Getting it backwards does not fail loudly. The runtime opens *something* --
// a host directory named "/" for wasmtime, or a guest mount of the wrong path
// for wasmedge -- and the guest then cannot find its sidecar, which presents as
// the "set but unreadable" path above.
//
// ❗ AND WASMER NEEDS `--net`, WHICH IS NOT A DIAGNOSTIC CONVENIENCE. Without
// it a WASIX guest's `sock_open` SUCCEEDS and its `sock_bind` returns errno 58
// (NOTSUP), so nothing anywhere reports a problem until the first bind, and
// what the guest then logs is a bind failure on an address that is perfectly
// fine. Measured; `.agents/docs/WASIX_ABI.md` has the two runs side by side.
// It is passed unconditionally because `raptormark run` only ever runs
// raptormark modules, and the profile that needs wasmer is the one with
// sockets.
//
// Split out as a pure function so a test can assert all three spellings without
// any of the runtimes installed.
func runtimeArgs(runtime, dir, module string, env, guestArgs []string) []string {
	return runtimeArgsImpl(runtime, dir, module, env, guestArgs)
}

// RuntimeArgsForTest is `runtimeArgs` for callers outside this package, and it
// exists for exactly one of them: `e2e/wasixnet_test.go` drives wasmer through
// a container and would otherwise SPELL THE FLAGS ITSELF.
//
// ❗ THAT DUPLICATION IS THE FAILURE THIS PREVENTS, and it is a quiet one. The
// unit test below pins what `runtimeArgs` produces; the E2E proves a real wasmer
// accepts what the E2E passes. Two different strings, each proven against a
// different thing, and nothing comparing them -- so `--volume` could regress to
// the deprecated `--mapdir`, or lose `--net`, and both tests would still pass
// while `raptormark run --runtime wasmer` was broken.
//
// Exported for tests only, following `BuildForTest` in build.go. The container
// paths ARE the arguments: the E2E mounts the module directory at /out, so it
// calls this with dir="/out".
func RuntimeArgsForTest(runtime, dir, module string, env, guestArgs []string) []string {
	return runtimeArgsImpl(runtime, dir, module, env, guestArgs)
}

func runtimeArgsImpl(runtime, dir, module string, env, guestArgs []string) []string {
	var args []string
	switch runtime {
	case "wasmer":
		args = append(args, "run", "--net")
		for _, e := range env {
			args = append(args, "--env", e)
		}
		args = append(args, "--volume", dir+":/")
		args = append(args, module)
	case "wasmtime":
		args = append(args, "run")
		for _, e := range env {
			args = append(args, "--env", e)
		}
		args = append(args, "--dir", dir+"::/")
		args = append(args, module)
	default: // wasmedge
		// `--enable-all` matches how every test in this tree runs a module.
		args = append(args, "--enable-all", "--dir", "/:"+dir)
		// ⚠️ wasmedge accepts --env only BEFORE the module path; anything after
		// it is passed to the guest instead, silently.
		for _, e := range env {
			args = append(args, "--env", e)
		}
		args = append(args, module)
	}
	return append(args, guestArgs...)
}

// resolveBin finds the runtime binary.
//
// PATH first, then the installer's own directory: wasmedge's install script puts
// it in ~/.wasmedge/bin and does NOT put that on PATH for non-interactive
// shells, so "command not found" is the common case on a machine that has it.
func (c *Run) resolveBin() (string, error) {
	if c.Bin != "" {
		return c.Bin, nil
	}
	if p, err := exec.LookPath(c.Runtime); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		cand := filepath.Join(home, "."+c.Runtime, "bin", c.Runtime)
		if fileExists(cand) {
			return cand, nil
		}
	}
	// ⚠️ The builder image has wasmedge and wasmtime but NOT wasmer -- wasmer is
	// deliberately not a build dependency of this tree and is on neither the
	// host nor the image. `.agents-workspace/wasmer/Dockerfile` builds a
	// container for it, which is what `e2e/wasixnet_test.go` drives.
	hint := fmt.Sprintf("the builder image has it, so `docker run --rm -v <dir>:/out ... %s ...` is the other way", c.Runtime)
	if c.Runtime == "wasmer" {
		hint = "the builder image does NOT have wasmer -- it is not a build dependency. " +
			"`.agents-workspace/wasmer/Dockerfile` builds a container with it"
	}
	return "", fmt.Errorf(
		"%s is not on PATH and not at ~/.%s/bin/%s. Install it, or pass --bin. (%s.)",
		c.Runtime, c.Runtime, c.Runtime, hint)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
