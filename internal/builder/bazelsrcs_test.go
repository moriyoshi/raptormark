package builder

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A `.go` file missing from its package's Bazel `srcs` is INVISIBLE to Go
// tooling, and `AGENTS.md` records what that cost.
//
// # ❗ Why this needs a test rather than care
//
// The rule is stated plainly in `AGENTS.md`: "Adding a Go package means adding a
// BUILD.bazel AND a deps entry, and a new FILE in an existing package means
// adding it to that package's srcs -- they are listed explicitly." Then the
// consequence: "`gofmt`/`go build`/`go vet`/`go test` all stay green when you
// forget; `bazel test //...` reports 'N tests pass and M were SKIPPED', which
// reads like caching and is not."
//
// It has been paid for once. Measured 2026-08-24: the omission broke
// `//cmd/raptormark:builder_tools_linux_arm64`, which `//builder:stage` depends
// on, so **the builder image could not have been built** -- and every Go gate was
// green throughout.
//
// ⚠️ A rule in a document is not a guard. That sentence has been the theme of an
// entire session's findings; this is the same lesson applied to the one hazard
// `AGENTS.md` calls out in bold and nothing checked.
//
// # What it does and does not compare
//
// It compares each package's `.go` files on disk against the bare filenames its
// `BUILD.bazel` lists ANYWHERE -- across `go_library` and `go_test` together,
// without trying to match a file to the right rule. That is deliberate: which
// rule a file belongs to is rules_go's business and it will say so loudly, while
// a file listed in NO rule is the silent case. Checking the weaker property is
// what keeps this from being a second, worse copy of the build graph.
//
// ❌ Generated files and `//go:build ignore` scripts would be false positives if
// this tree had any; it does not, and adding an exclusion list before one exists
// would be a hole waiting for a file to fall through.

var goFileInBuild = regexp.MustCompile(`"([A-Za-z0-9_.-]+\.go)"`)

// TestEveryGoFileIsInItsBazelSrcs.
func TestEveryGoFileIsInItsBazelSrcs(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

	var checked int
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// `_recovery` is gone (2026-08-25) but stays in this list for the
			// same reason its other exclusions do: it costs nothing and is what a
			// restored copy would need.
			case ".git", "node_modules", "_recovery", ".agents-workspace", "third_party":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "BUILD.bazel" {
			return nil
		}
		dir := filepath.Dir(path)
		onDisk, derr := goFilesIn(dir)
		if derr != nil {
			return derr
		}
		if len(onDisk) == 0 {
			return nil
		}
		checked++

		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		listed := map[string]bool{}
		for _, m := range goFileInBuild.FindAllStringSubmatch(string(b), -1) {
			listed[m[1]] = true
		}
		rel, _ := filepath.Rel(root, dir)
		for _, f := range onDisk {
			if !listed[f] {
				t.Errorf("%s/%s is not listed in %s/BUILD.bazel.\n"+
					"Every Go gate stays GREEN when this is forgotten; `bazel test //...` "+
					"reports it as a SKIP, which reads like caching. On 2026-08-24 the same "+
					"omission meant the builder image could not be built.", rel, f, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// ❗ POSITIVE CONTROL. Without it this passes the day the walk stops finding
	// BUILD.bazel files -- and finding nothing is the only way this test can be
	// wrong while looking right.
	if checked < 5 {
		t.Fatalf("only %d packages with Go files and a BUILD.bazel were examined; "+
			"this tree has many more, so the walk is looking at the wrong place "+
			"and the comparison is vacuous", checked)
	}
	t.Logf("checked %d packages", checked)
}

// goFilesIn lists the .go files directly in dir, sorted.
func goFilesIn(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
