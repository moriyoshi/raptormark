package builder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildImage builds the raptormark builder image and the two base images below
// it. It runs on the host, not in a container.
//
// Three images, each layered on the last:
//
//	raptormark-elfconv-base:<tag>          the pinned submodule's own Dockerfile,
//	                                       ECV_AARCH64=1 -> toolchain + elflift
//	raptormark-elfconv-base-patched:<tag>  + patches/*.patch, elflift rebuilt,
//	                                       + decoder assertions
//	raptormark-builder:<tag>               + Rust/ecvisor, ecv-promote,
//	                                       the Go builder tools
type BuildImage struct {
	LLVM    string `name:"llvm" default:"16" enum:"16,22" help:"LLVM toolchain line."`
	Tag     string `name:"tag" help:"Image tag. Defaults to the submodule pin (LLVM 16) or llvm22."`
	BaseTag string `name:"base-tag" help:"Layer onto a base image tagged differently from --tag, without retagging it."`

	SkipBase bool `name:"skip-base" help:"Reuse the existing base image."`
	NoCache  bool `name:"no-cache" help:"Pass --no-cache to every docker build."`

	PrintDockerfile bool `name:"print-dockerfile" help:"Print the generated patched-base Dockerfile and exit. That stage has no standing Dockerfile, so this is how you inspect or lint it."`

	// TranslateSH pins the cache-identity label instead of deriving it from the
	// translation sources. The only reason to use it is to keep an existing
	// object cache valid across a change you know cannot alter the objects —
	// and getting that judgement wrong serves stale objects for hours of work.
	TranslateSH string `name:"translate-sh" help:"Override the raptormark.translate_sh cache-identity label. Unsafe unless you are certain the objects are unchanged."`
}

func (c *BuildImage) Run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	submodule := filepath.Join(root, "third_party", "elfconv")
	if _, err := os.Stat(filepath.Join(submodule, "Dockerfile")); err != nil {
		return fmt.Errorf("%s is not populated (git submodule update --init)", submodule)
	}

	// The submodule pin threads through every tag, so an image always names the
	// elfconv commit it was lifted from. 12 hex chars matches the recorded tags
	// (e.g. raptormark-elfconv-base:8bfe80860118).
	pin, err := output("git", "-C", submodule, "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return err
	}

	// `latest` follows the pinned LLVM-16 line only. An explicit --tag (or the
	// llvm22 line) is a side build and must not move it — matching the surviving
	// images, where raptormark-builder:latest and :8bfe80860118 are the same
	// image while :llvm22-v2 and :dbg are distinct.
	tag, tagIsPin := c.Tag, false
	if tag == "" {
		if c.LLVM == "22" {
			tag = "llvm22"
		} else {
			tag, tagIsPin = pin, true
		}
	}

	baseTag := c.BaseTag
	if baseTag == "" {
		baseTag = tag
	}
	base := "raptormark-elfconv-base:" + baseTag
	patched := "raptormark-elfconv-base-patched:" + tag
	builder := "raptormark-builder:" + tag

	if c.PrintDockerfile {
		fmt.Print(c.patchedDockerfile(base))
		return nil
	}

	fmt.Printf("build-image: elfconv pin %s, LLVM %s, tag %s\n", pin, c.LLVM, tag)

	// --- 1. base: the pinned submodule's own Dockerfile ---------------------
	// Context is the submodule itself (its Dockerfile does `COPY ./ ./` then
	// ./scripts/build.sh). ECV_AARCH64=1 / ECV_X86 unset — the Dockerfile
	// asserts exactly one of the two is set.
	if c.SkipBase {
		fmt.Printf("build-image: --skip-base, reusing %s\n", base)
	} else {
		fmt.Printf("build-image: building %s\n", base)
		if err := c.dockerBuild(nil, "-t", base, "--build-arg", "ECV_AARCH64=1", submodule); err != nil {
			return err
		}
	}

	// --- 2. patched base: the fork, applied on top of the pin ---------------
	// The patch series is applied inside the image rather than committed into
	// the submodule, so the submodule stays clean at the pin. Context is
	// patches/ (the `COPY *.patch`), and the Dockerfile is generated here — the
	// patched base has no standing Dockerfile of its own.
	fmt.Printf("build-image: building %s\n", patched)
	if err := c.dockerBuild([]byte(c.patchedDockerfile(base)),
		"-t", patched, "-f", "-", filepath.Join(root, "patches")); err != nil {
		return err
	}

	// --- 3. the builder tools ------------------------------------------------
	// translate-one and link-all are this same Go binary, cross-built for the
	// image's platform and copied in. Building on the host rather than in a
	// Docker stage keeps the image free of a Go toolchain and the build free of
	// a module download.
	toolsPath := filepath.Join(root, "builder", "_tools", "raptormark-builder-tools")
	if err := buildTools(root, patched, toolsPath); err != nil {
		return err
	}

	// --- 4. cache identity ---------------------------------------------------
	// Both values are baked into the builder image as labels and read back by
	// internal/translate.TranslateID to key the per-binary translated-object
	// cache. BASE_ID is the patched base image's .Id; TRANSLATE_SH hashes the
	// sources of the translation pipeline (see toolsid.go).
	baseID, err := output("docker", "image", "inspect", "--format", "{{.Id}}", patched)
	if err != nil {
		return err
	}
	translateSH := c.TranslateSH
	if translateSH == "" {
		if translateSH, err = TranslateSH(root); err != nil {
			return err
		}
	} else {
		fmt.Println("build-image: WARNING: --translate-sh pins the object-cache identity;")
		fmt.Println("  cached objects will be reused even if the pipeline changed.")
	}
	fmt.Printf("build-image: BASE_ID=%s\n", baseID)
	fmt.Printf("build-image: TRANSLATE_SH=%s\n", translateSH)

	// --- 5. builder ----------------------------------------------------------
	// Context is the repo root so builder/ and runtime/ are both reachable;
	// .dockerignore trims it to just those two (third_party/elfconv is ~500 MB
	// and reaches the image via the base, not this context).
	fmt.Printf("build-image: building %s\n", builder)
	args := []string{"-t", builder}
	if tagIsPin {
		args = append(args, "-t", "raptormark-builder:latest")
	}
	args = append(args,
		"-f", filepath.Join(root, "builder", "Dockerfile"),
		"--build-arg", "ELFCONV_BASE="+patched,
		"--build-arg", "ECV_LLVM_VER="+c.LLVM,
		"--build-arg", "BASE_ID="+baseID,
		"--build-arg", "TRANSLATE_SH="+translateSH,
		root)
	if err := c.dockerBuild(nil, args...); err != nil {
		return err
	}

	if tagIsPin {
		fmt.Printf("build-image: done — %s (also tagged raptormark-builder:latest)\n", builder)
	} else {
		fmt.Printf("build-image: done — %s (side build; raptormark-builder:latest unchanged)\n", builder)
	}
	return nil
}

// dockerBuild runs `docker build`, optionally feeding a generated Dockerfile on
// stdin (paired with `-f -`).
func (c *BuildImage) dockerBuild(stdin []byte, args ...string) error {
	full := []string{"build"}
	if c.NoCache {
		full = append(full, "--no-cache")
	}
	full = append(full, args...)
	cmd := exec.Command("docker", full...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

// buildTools cross-builds this binary for the builder image's platform.
//
// The platform is read off the patched base rather than assumed, so an arm64
// host building an amd64 image (or the reverse) still produces a binary the
// image can execute. CGO is off so it needs no loader inside the image.
func buildTools(root, image, out string) error {
	platform, err := output("docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image)
	if err != nil {
		return err
	}
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok {
		return fmt.Errorf("build-image: cannot parse image platform %q", platform)
	}
	fmt.Printf("build-image: building builder tools for %s/%s\n", goos, goarch)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/raptormark")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build-image: building builder tools: %w", err)
	}
	return nil
}

// patchedDockerfile generates the patched-base stage.
func (c *BuildImage) patchedDockerfile(base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", base)
	b.WriteString(`COPY *.patch /patches/
RUN cd /root/elfconv && for p in /patches/*.patch; do echo "applying $p"; git apply "$p" || exit 1; done
`)

	// Assert the decoders the fork adds are actually present, so a silently
	// mis-applied patch fails the build instead of surfacing as a lifting bug.
	b.WriteString(`RUN cd /root/elfconv && \
    grep -qE 'bool TryDecodeSTLRB_SL32_LDSTEXCL\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    grep -qE 'bool TryDecodeMUL_ASIMDSAME_ONLY\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    grep -qE 'bool TryDecodeSTLLRB_SL32_LDSTEXCL\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    grep -qE 'bool TryDecodeUMULL_ASIMDDIFF_L\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    grep -qE 'bool TryDecodeFNEG_ASIMDMISC_R\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    grep -qE 'bool TryDecodeUADDLP_ASIMDMISC_P\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    grep -qE 'bool TryDecodeNEG_ASIMDMISC_R\(const InstData &data' backend/remill/lib/Arch/AArch64/Arch.cpp && \
    echo "ASSERT OK: STLRB + MUL + LOR + UMULL + FNEG + UADDLP + NEG present"
`)

	if c.LLVM == "22" {
		// The toolchain swap happens *after* the patches land, so the rebuild
		// compiles the patched sources with clang-22 in one pass.
		b.WriteString(`RUN echo "deb [signed-by=/usr/share/keyrings/llvm-snapshot.gpg] http://apt.llvm.org/jammy/ llvm-toolchain-jammy-22 main" >> /etc/apt/sources.list && \
    apt-get update -qq && apt-get install -y -qq llvm-22 llvm-22-dev clang-22 lld-22 libclang-22-dev >/dev/null
RUN cd /root/elfconv && rm -rf build build22 && \
    cmake -B build -DCMAKE_PREFIX_PATH="/root/elfconv/dependencies/install;/usr/lib/llvm-22" \
      -DCMAKE_C_COMPILER=clang-22 -DCMAKE_CXX_COMPILER=clang++-22 -DREMILL_BUILD_SPARC32_RUNTIME=OFF \
      -DCMAKE_ELFCONV_AARCH64_BUILD=1 -DCMAKE_ELFCONV_X86_BUILD=0 -GNinja . && \
    cmake --build build --target elflift aarch64 -j"$(nproc)"
`)
	} else {
		// LLVM 16: the base's build tree is already configured, so just rebuild
		// the targets the patches touch.
		b.WriteString("RUN cd /root/elfconv && cmake --build build --target elflift aarch64 -j\"$(nproc)\"\n")
	}
	return b.String()
}

// repoRoot locates the module root, which is where the Dockerfiles, patch
// series and submodule are anchored.
func repoRoot() (string, error) {
	gomod, err := output("go", "env", "GOMOD")
	if err != nil {
		return "", fmt.Errorf("build-image: locating the repo root: %w", err)
	}
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("build-image: not inside the raptormark module")
	}
	return filepath.Dir(gomod), nil
}
