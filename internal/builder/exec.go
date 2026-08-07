// Package builder implements the three programs that used to be bash scripts
// under builder/: translate-one and link-all (which run *inside* the builder
// image) and build-image (which runs on the host and builds that image).
//
// The CLI surface of all three is preserved exactly as the scripts had it, so
// internal/translate — which drives translate-one and link-all over `docker
// run` — needs no knowledge of the port beyond the entrypoint path.
package builder

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// run executes a command with stdout/stderr wired straight through. Inside the
// builder container both streams are captured by internal/translate, which
// keeps stderr only when the run fails — so anything printed on the success
// path is for a human watching a direct `docker run`, not for the driver.
//
// Every call is checked: the scripts ran under `set -e`, where any non-zero
// status aborted the pipeline, and that behaviour is load-bearing (see the
// llvm-split commentary in TranslateOne.codegenEcvisor).
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// runCapture is run with stderr diverted to a file, for the one case that needs
// the output on the failure path only (llvm-split).
func runCapture(stderrPath string, name string, args ...string) error {
	f, err := os.Create(stderrPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = f
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// output runs a command and returns its trimmed stdout.
func output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// loginEnv reproduces the scripts' `source /root/.bash_profile`. The base image
// exports WASI_SDK_PATH (and PATH additions) from the login profile rather than
// from Docker ENV, so a program started without a login shell sees neither.
//
// The scripts handled this by sourcing the profile; a Go binary cannot, so it
// asks a login shell for its environment instead. Variables already set in the
// current process win — an explicit `docker run -e` must still override.
var loginEnv = sync.OnceValue(func() map[string]string {
	out, err := exec.Command("/bin/bash", "--login", "-c", "env -0").Output()
	if err != nil {
		return nil
	}
	env := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	sc.Split(splitNUL)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			env[k] = v
		}
	}
	return env
})

func splitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// sourceLoginProfile merges the login shell's environment into this process for
// any variable not already set, and adopts its PATH outright.
//
// PATH is taken wholesale because the profile builds on the inherited value
// (`export PATH=$PATH:...`), so the login shell's PATH is a superset of ours —
// and it is what puts the LLVM and emsdk tools in reach. Everything else is
// filled in only where missing, so `docker run -e ECV_LLVM_VER=22` still wins.
func sourceLoginProfile() {
	env := loginEnv()
	if env == nil {
		return
	}
	for k, v := range env {
		if k == "PATH" {
			continue
		}
		if _, ok := os.LookupEnv(k); !ok {
			os.Setenv(k, v)
		}
	}
	if p := env["PATH"]; p != "" {
		os.Setenv("PATH", p)
	}
}

// envOr returns the environment's value for key, or def when unset or empty.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// wasiSDK resolves WASI_SDK_PATH and verifies it actually carries the toolchain,
// mirroring the scripts' check on ${WASI_SDK_PATH}/bin/clang++.
func wasiSDK() (string, error) {
	path := envOr("WASI_SDK_PATH", "")
	if path == "" {
		return "", fmt.Errorf("WASI_SDK_PATH not usable (unset)")
	}
	if err := executable(path + "/bin/clang++"); err != nil {
		return "", fmt.Errorf("WASI_SDK_PATH not usable (%s): %w", path, err)
	}
	return path, nil
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}
