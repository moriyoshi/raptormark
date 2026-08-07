package builder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Running Bazel inside the elfconv base image.
//
// # Why in the image
//
// `//builder`'s LLVM tools link against the LLVM that built `elflift`, and
// `//runtime`'s C shims compile with the wasi-sdk that image carries. Building
// them anywhere else means building them against a DIFFERENT toolchain of the
// same version, which is very probably equivalent and is not proven. See
// //bazel:sdk.bzl -- `RAPTORMARK_BAZEL_SDK=hermetic` is the other mode, and it
// exists precisely so that question can be asked separately from this one.
//
// # Three things this has to get right, all found by getting them wrong
//
//  1. THE ENTRYPOINT TAKES ONE STRING. The base image's entrypoint is
//     `["/bin/bash", "--login", "-c"]`, so a normal argv is silently truncated
//     to its first word -- `bazel` alone, which prints usage and exits 0. The
//     command is joined into a single argument here for that reason.
//
//  2. IT MUST RUN AS ROOT. `$WASI_SDK_PATH` is under /root, mode 700, so a
//     non-root uid cannot even stat the compiler and the SDK repo rule reports
//     it as absent.
//
//  3. WHICH MEANS IT WRITES ROOT-OWNED FILES INTO THE REPO. gazelle writes
//     BUILD.bazel files and bzlmod writes MODULE.bazel.lock, and as root those
//     land in the user's tree owned by root. `restoreOwnership` puts them back.
//     ❌ Do not skip this: CLAUDE.md's e2e code already carries a `removeAsRoot`
//     helper because this exact thing happened before.
//
// Bazel's own output base is a named Docker VOLUME rather than a bind mount.
// Bazel refuses an output base it does not own, and a bind mount would put
// root-owned build outputs in the source tree; a volume avoids both and
// survives between runs, which matters because the container is ephemeral and
// re-fetching rules_rust and the Go SDK on every build would be absurd.
const bazelCacheVolume = "raptormark-bazel-cache"

// Bazel runs `bazel <args>` inside a builder-capable image.
//
// The escape hatch as much as the entry point: when something in the Bazel
// build misbehaves, `raptormark bazel build //builder:stage --verbose_failures`
// is how to look at it without reconstructing the docker invocation by hand.
type Bazel struct {
	Image string   `name:"image" help:"Image to run Bazel inside. Defaults to the elfconv base, which is where LLVM and the wasi-sdk are."`
	Args  []string `arg:"" optional:"" passthrough:"" help:"Arguments to bazel, e.g. build //builder:stage"`
}

func (c *Bazel) Run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	image := c.Image
	if image == "" {
		return fmt.Errorf("raptormark bazel: --image is required; there is no safe default " +
			"(raptormark-builder:latest is not the newest builder, see CLAUDE.md)")
	}
	args := c.Args
	if len(args) == 0 {
		args = []string{"info"}
	}
	return runBazel(root, image, args...)
}

// bazelBinary locates a host bazel to mount into the container. The image has
// none, and putting one there would mean a `RUN` line in a base image this
// project does not own.
func bazelBinary() (string, error) {
	if p := os.Getenv("RAPTORMARK_BAZEL"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("RAPTORMARK_BAZEL=%s: %w", p, err)
		}
		return p, nil
	}
	for _, name := range []string{"bazelisk", "bazel"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no bazel found: put bazelisk or bazel on PATH, or set RAPTORMARK_BAZEL " +
		"to one. It is mounted into the image, which has no bazel of its own")
}

func runBazel(root, image string, args ...string) error {
	bin, err := bazelBinary()
	if err != nil {
		return err
	}
	binDir, binName := filepath.Split(bin)

	// --symlink_prefix=/ suppresses the bazel-out convenience symlinks, which
	// would otherwise be written into the source tree pointing at container
	// paths that do not exist on the host.
	//
	// ⚠️ MODULE.bazel.lock IS LEFT ENABLED, deliberately. An earlier version
	// passed --lockfile_mode=off to stop bzlmod writing it into the
	// bind-mounted tree as root -- but `restoreOwnership` below already names
	// that exact file, so the ownership problem is solved without giving up the
	// lock. Disabling it would have traded a reproducibility guarantee for a
	// workaround to a problem that was already handled.
	inner := fmt.Sprintf("/opt/bazelbin/%s --output_user_root=/bzlcache/root %s --symlink_prefix=/",
		binName, strings.Join(args, " "))

	docker := []string{
		"run", "--rm",
		"-v", root + ":/src",
		"-v", binDir + ":/opt/bazelbin",
		"-v", bazelCacheVolume + ":/bzlcache",
		"-w", "/src",
		image,
		inner,
	}
	cmd := exec.Command("docker", docker...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	// Ownership is restored even when the build failed: a failed gazelle run
	// still leaves files behind, and leaving them root-owned is what makes the
	// NEXT run fail for an unrelated reason.
	if err := restoreOwnership(root, image); err != nil {
		if runErr == nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "raptormark bazel: also failed to restore ownership: %v\n", err)
	}
	if runErr != nil {
		return fmt.Errorf("raptormark bazel: %w", runErr)
	}
	return nil
}

// restoreOwnership chowns back the only files Bazel writes into the source
// tree. It names them rather than sweeping, so a pre-existing root-owned file
// that has nothing to do with Bazel is left alone -- there is one in this tree
// already (`runtime/runtime`), and taking it over would be a silent change to
// something the user owns.
func restoreOwnership(root, image string) error {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return nil
	}
	script := fmt.Sprintf(
		`find /src \( -name BUILD.bazel -o -name MODULE.bazel.lock \) -user 0 `+
			`-not -path '/src/.agents-workspace/*' -print0 | xargs -0 -r chown %d:%d`, uid, gid)
	cmd := exec.Command("docker", "run", "--rm", "-v", root+":/src", "-w", "/src", image, script)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restoring ownership of Bazel-written files: %w", err)
	}
	return nil
}

// stageImageFiles builds //builder:stage and copies the result to
// builder/_stage, which is what `docker build` then uses as its CONTEXT.
//
// The copy exists because Bazel's outputs live in a Docker volume the host
// cannot see. Everything else about this is deliberately dumb: the tree is ~20
// MB, it is rewritten from scratch each time, and it holds nothing that is not
// a Bazel output.
func stageImageFiles(root, image string) (string, error) {
	stage := filepath.Join(root, "builder", "_stage")
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", err
	}
	if err := runBazel(root, image, "build", "//builder:stage"); err != nil {
		return "", err
	}
	// `cp -a` out of the volume and into the bind-mounted tree, then hand it
	// back to the invoking user -- same reason as restoreOwnership.
	script := fmt.Sprintf(
		`set -e; S=$(find /bzlcache -type d -path '*/bin/builder/stage' | head -1); `+
			`test -n "$S" || { echo "no //builder:stage output found in the bazel cache" >&2; exit 1; }; `+
			`cp -a "$S"/. /src/builder/_stage/; chown -R %d:%d /src/builder/_stage`,
		os.Getuid(), os.Getgid())
	cmd := exec.Command("docker", "run", "--rm",
		"-v", root+":/src", "-v", bazelCacheVolume+":/bzlcache", "-w", "/src", image, script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("staging the image contents: %w", err)
	}
	return stage, nil
}
