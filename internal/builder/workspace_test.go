package builder

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkspaceCoversEveryModule guards the repo's build configuration.
//
// tools/decode-oracle is a separate Go module for licensing reasons, and a
// nested go.mod is invisible to the parent module's `./...`. That is the whole
// mechanism keeping an LGPL obligation out of an Apache-2.0 pipeline, and it is
// also a way to lose a test suite silently: before go.work existed, 36 tests
// were not run by the repo-root gate and nothing said so.
//
// So: every module in the tree must be listed in go.work. A module that is not
// is one nobody's gate reaches.
//
// This lives in internal/builder because that package already owns how this
// repo builds -- TranslateSH hashes repo files from here for the same reason.
// It reads the filesystem only; no toolchain, no network, no Docker.
//
// ⚠️ It does NOT assert that anyone ran the right command. `./...` is not
// recursive across modules even with a workspace, so the gate has to name both
// patterns explicitly; see go.work and .agents/docs/QUALITY_GATE.md.
func TestWorkspaceCoversEveryModule(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("no go.work at the repo root (%s). It is what lets one gate command cover "+
			"every module; see the comment in that file", root)
	}

	found := modulesInTree(t, root)
	if len(found) < 2 {
		t.Fatalf("found %d modules under %s; expected at least the root and tools/decode-oracle. "+
			"If a module was removed, update this test with it", len(found), root)
	}

	listed := workspaceUses(t, filepath.Join(root, "go.work"))

	for _, dir := range found {
		if !listed[dir] {
			t.Errorf("module %q has a go.mod but is not `use`d in go.work, so no repo-root gate "+
				"command reaches its packages. Add it.", dir)
		}
	}
	// The other direction -- a `use` entry with no go.mod -- is deliberately NOT
	// asserted here. The toolchain refuses to load the workspace at all in that
	// case ("cannot load module ... listed in go.work file"), so `go test`
	// never reaches this function. An assertion that cannot fire carries no
	// information; verified by adding a bogus entry and watching the toolchain,
	// not the test, produce the error.
}

// modulesInTree returns every directory containing a go.mod, repo-relative and
// slash-separated, with the root as ".".
//
// third_party/elfconv is skipped: it is a pinned submodule with its own build
// system, not a Go module this repo gates. Everything else is in scope, which
// is what makes a NEW module fail this test by default rather than by being
// remembered.
func modulesInTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "_recovery", ".agents-workspace":
				return filepath.SkipDir
			}
			if path == filepath.Join(root, "third_party", "elfconv") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// workspaceUseLine matches a `use` entry, in either the block or single-line form.
var workspaceUseLine = regexp.MustCompile(`^\s*(?:use\s+)?(\.[^\s()]*|\.)\s*$`)

// workspaceUses parses the `use` directives out of go.work.
//
// Hand-parsed rather than shelled out to `go work edit -json`: this test must
// run without invoking the toolchain, and the directive is two shapes.
func workspaceUses(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	inBlock := false
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if i := strings.Index(s, "//"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		switch {
		case s == "use (":
			inBlock = true
			continue
		case inBlock && s == ")":
			inBlock = false
			continue
		}
		if !inBlock && !strings.HasPrefix(s, "use ") {
			continue
		}
		m := workspaceUseLine.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		out[normalizeUse(m[1])] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no `use` directives", path)
	}
	return out
}

// normalizeUse maps a go.work path onto the form modulesInTree produces: "."
// for the root, and a slash-separated relative path otherwise.
func normalizeUse(p string) string {
	p = strings.TrimSuffix(p, "/")
	if p == "." || p == "" {
		return "."
	}
	return strings.TrimPrefix(p, "./")
}
