package builder

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuildTools rebuilds only `builder/_tools/raptormark-builder-tools`, the
// prebuilt binary the image COPYs in as its pipeline.
//
// # Why this exists as its own command
//
// `raptormark build-image` already rebuilds it, and that is the normal path. But
// the SIDE-BUILD recipe deliberately avoids `build-image` -- it derives the
// patched base from `--tag`, and re-applying the patch series can yield a
// different image id, which invalidates every cached object (see CLAUDE.md). The
// documented workaround is a raw `docker build` against an existing patched
// base, and that workaround silently skips the tools build.
//
// ⚠️ It cost a void gate on 2026-08-18. A `-fPIC` change to `translateone.go`
// never reached the image, and every signal a careful check would look at said it
// had: `raptormark.translate_sh` moved (it hashes the pipeline's SOURCE, which
// really had changed), the object cache went cold and re-translated for 20
// minutes, and `libecvisor.a` differed. All three are downstream of "the source
// changed"; none is downstream of "the binary changed". The emitted object was
// the only witness, and nothing looked at it. It also poisoned 45 cache entries,
// which had to be purged by hand.
//
// The two alternatives were considered and not taken:
//
//   - fold the tools binary's hash into `TranslateSH`. `toolsid.go` rejects
//     hashing binaries for a stated and still-correct reason: it would discard a
//     cache worth hours on every unrelated toolchain bump.
//   - build the tools in a Docker stage. `builder/Dockerfile` keeps the Go
//     toolchain out of the image on purpose.
//
// So the fix is additive: a command the side-build recipe can call, changing
// neither the cache identity design nor the image's contents.
type BuildTools struct {
	Base string `name:"base" required:"" help:"The image the tools must run inside — its OS/arch selects GOOS/GOARCH. Use the patched base or the builder you are layering onto."`
	Out  string `name:"out" type:"path" help:"Where to write the binary. Defaults to builder/_tools/raptormark-builder-tools, which is what the Dockerfile COPYs."`
}

func (c *BuildTools) Run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	out := c.Out
	if out == "" {
		out = filepath.Join(root, "builder", "_tools", "raptormark-builder-tools")
	}
	// Reported because the whole point is to make the rebuild visible: the
	// failure this command exists for is a build that quietly used yesterday's
	// binary, and a command that says nothing would leave the same gap.
	before := describeFile(out)
	if err := buildTools(root, c.Base, out); err != nil {
		return err
	}
	fmt.Printf("build-tools: %s\n  was: %s\n  now: %s\n", out, before, describeFile(out))
	return nil
}

func describeFile(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%d bytes, %s", st.Size(), st.ModTime().Format("2006-01-02 15:04:05"))
}
