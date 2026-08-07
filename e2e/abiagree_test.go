package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// The re-entrant run states cross THREE languages and were tied in none of them.
//
// `runtime/src/entry.rs` defines `ECV_IDLE`/`ECV_PREEMPTED`/`ECV_EXITED` as the
// return codes of `ecv_run_slice`, and `e2e/testdata/hostedembedder.mjs`
// hard-codes the same three numbers to interpret them. Nothing compared the two.
//
// # ⚠️ NOT GATED ON RAPTORMARK_E2E, deliberately
//
// Every other test in this package calls `requireE2E` and skips without Docker.
// This one reads two source files and needs nothing, so gating it would make the
// guard absent exactly when someone runs the cheap gate -- and this tree has
// twice this week paid for a check that announced its own absence as a skip.
// `AGENTS.md` requires `go test ./...` to need no Docker, root or network; this
// honours that by needing none of them, not by skipping.
//
// # What drift would look like
//
// Nothing fails to build and nothing errors. Renumber the states in Rust and the
// embedder reads IDLE as PREEMPTED and spins, or reads PREEMPTED as EXITED and
// stops with the guest's work half done -- which is a CLEAN EXIT 0. That exact
// failure has already happened here once from a different cause: a wake that set
// `Runnable` without enqueueing gave "a clean exit 0 with the guest's work
// undone", and it took a run to find because nothing reports it.
//
// # Why the numbers are not obviously right
//
// `hostedembedder.mjs` itself warns that "ECV_IDLE IS 0 AND ECV_PREEMPTED IS 1,
// which reads backwards if you expect the busy state to be the falsy one". A
// pairing that reads backwards is one somebody eventually "corrects".

var (
	rustI32Const = regexp.MustCompile(`pub const ([A-Z_]+): i32 = (-?\d+);`)
	// Matches the embedder's single-line form:
	//   const ECV_IDLE = 0, ECV_PREEMPTED = 1, ECV_EXITED = 2;
	jsConst = regexp.MustCompile(`\b(ECV_[A-Z_]+)\s*=\s*(-?\d+)`)
)

// scanConsts pulls every NAME -> value pair a pattern finds in a file.
//
// ❌ It FATALS when the file yields nothing. A source-scanning guard whose
// pattern stops matching is green forever, which is the failure mode that makes
// this whole class of test worth distrusting -- so "found nothing" is treated as
// breakage, never as agreement.
func scanConsts(t *testing.T, path string, re *regexp.Regexp) map[string]int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		out[m[1]] = n
	}
	if len(out) == 0 {
		t.Fatalf("%s: the pattern %s matched nothing.\n"+
			"❌ Do not relax it until you know why: a pattern that matches nothing "+
			"passes vacuously, and this guard exists precisely because these "+
			"constants are otherwise unchecked.", path, re)
	}
	return out
}

// runtimeSrc locates a runtime Rust source, under `go test` and under Bazel.
//
// ⚠️ Bazel runs tests in a SANDBOX containing only declared inputs, so the plain
// relative path resolves under `go test` and not under `bazel test`. The e2e
// target declares `//runtime:srcs` as data for exactly this, which puts the tree
// at a runfiles-relative path instead.
//
// ❌ Both are TRIED and the failure is FATAL. Falling back to a skip when
// neither resolves is the shape this guard exists to avoid -- it would go quiet
// in precisely the environment where nobody is watching it.
func runtimeSrc(t *testing.T, name string) string {
	t.Helper()
	for _, p := range []string{
		filepath.Join("..", "runtime", "src", name), // go test, from e2e/
		filepath.Join("runtime", "src", name),       // bazel runfiles root
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	wd, _ := os.Getwd()
	t.Fatalf("cannot find runtime/src/%s from %s.\n"+
		"Under `bazel test` this needs `//runtime:srcs` in the e2e target's `data`; "+
		"under `go test` it is a plain relative path. ❌ Do not convert this to a "+
		"skip -- a guard that goes quiet when it cannot find its input is worse "+
		"than no guard, because it reports success.", name, wd)
	return ""
}

// TestRunStatesAgreeBetweenRuntimeAndEmbedder ties the two copies together.
func TestRunStatesAgreeBetweenRuntimeAndEmbedder(t *testing.T) {
	rust := scanConsts(t, runtimeSrc(t, "entry.rs"), rustI32Const)
	js := scanConsts(t, "testdata/hostedembedder.mjs", jsConst)

	for _, name := range []string{"ECV_IDLE", "ECV_PREEMPTED", "ECV_EXITED"} {
		r, okR := rust[name]
		j, okJ := js[name]
		if !okR {
			t.Errorf("runtime/src/entry.rs no longer defines %s as a `pub const ... : i32`. "+
				"Either it moved or its form changed; until that is resolved this guard "+
				"is not comparing it.", name)
			continue
		}
		if !okJ {
			t.Errorf("testdata/hostedembedder.mjs no longer defines %s. The embedder "+
				"interprets ecv_run_slice's return by these names, so an absent one "+
				"means it is interpreting something else.", name)
			continue
		}
		if r != j {
			t.Errorf("%s is %d in runtime/src/entry.rs and %d in the embedder.\n"+
				"Nothing would error: the embedder would misread the guest's run state "+
				"and either spin or stop early with a CLEAN EXIT 0 and the guest's work "+
				"undone.", name, r, j)
		}
	}

	// ⚠️ The three must also be DISTINCT. Equal values pass every comparison
	// above while collapsing two states into one, and the resulting behaviour --
	// a guest that exits when it meant to yield -- looks like a scheduler bug
	// rather than a constant.
	seen := map[int]string{}
	for _, name := range []string{"ECV_IDLE", "ECV_PREEMPTED", "ECV_EXITED"} {
		v, ok := rust[name]
		if !ok {
			continue
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("%s and %s are both %d, so two run states are indistinguishable",
				prev, name, v)
		}
		seen[v] = name
	}
}
