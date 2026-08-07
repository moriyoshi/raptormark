package builder

import (
	"fmt"
	"os"
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
//
// ─────────────────────────────────────────────────────────────────────────────
// ✅ OBSOLETE SINCE 2026-08-23, and it REFUSES TO RUN rather than no-op.
//
// The hazard above is gone, structurally rather than by discipline.
// `builder/Dockerfile` no longer COPYs a prebuilt binary -- it copies
// `//builder:stage`, which DEPENDS on
// `//cmd/raptormark:builder_tools_linux_arm64`. Bazel cannot assemble the image
// contents without building the binary, so "the image shipped yesterday's
// pipeline" is no longer a state the build can reach.
//
// ❌ It would be worse than useless to leave working. It writes
// `builder/_tools/raptormark-builder-tools`, and NOTHING READS THAT PATH ANY
// MORE. A command that rebuilds a file the image ignores, and prints a
// reassuring "was:/now:" diff while doing it, is the same failure as the one it
// was written to prevent -- a signal that says the change landed when it did
// not. So it fails, and says what to run instead.
type BuildTools struct {
	Base string `name:"base" help:"Obsolete. The image contents are built by Bazel; see the error text."`
	Out  string `name:"out" type:"path" help:"Obsolete."`
}

func (c *BuildTools) Run() error {
	return fmt.Errorf(`build-tools is obsolete and does nothing useful.

builder/Dockerfile no longer COPYs builder/_tools/raptormark-builder-tools.
The pipeline binary reaches the image through //builder:stage, which depends on
//cmd/raptormark:builder_tools_linux_arm64, so it cannot be stale.

  What you probably want:
    raptormark build-image --tag <tag>          # the whole thing
    raptormark bazel --image <patched-base> build //builder:stage

  The side-build recipe in CLAUDE.md that called this command should now stage
  with Bazel and docker build against builder/_stage. build-image does both.`)
}

func describeFile(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%d bytes, %s", st.Size(), st.ModTime().Format("2006-01-02 15:04:05"))
}
