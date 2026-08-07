package e2e

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/pipeline"
)

// Program A: dlopens the plugin, checks it, then execve's program B. Both
// triggers of the loader seam in one guest, driven by the real CLI path.
const twoProgAGuestSrc = `#include <stdio.h>
#include <dlfcn.h>
#include <unistd.h>
int main(void) {
    void *h = dlopen("/usr/lib/postgresql/17/lib/ext_a.so", RTLD_NOW);
    if (!h) { printf("FAIL dlopen: %s\n", dlerror()); return 1; }
    int (*magic)(void) = (int (*)(void))dlsym(h, "Pg_magic_func");
    if (!magic) { printf("FAIL dlsym: %s\n", dlerror()); return 1; }
    int got = magic();
    if (got != 0xA1) { printf("FAIL magic=0x%X want=0xA1\n", got); return 1; }
    printf("TWOPROG-A dlopen ok, magic=0x%X\n", got);
    fflush(stdout);
    char *argv[] = {"/usr/bin/twob", "second", 0};
    char *envp[] = {0};
    execve("/usr/bin/twob", argv, envp);
    perror("FAIL execve");
    return 1;
}
`

// Program B. Checks its argv, so "the wrong program ran under the right argv"
// -- the exec-map fallback `execmap.rs` records four incidents for -- fails here
// rather than passing as a marker.
const twoProgBGuestSrc = `#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
    if (argc != 2 || strcmp(argv[1], "second") != 0) {
        printf("FAIL wrong argv: argc=%d\n", argc);
        return 1;
    }
    printf("TWOPROG-OK\n");
    return 0;
}
`

const twoProgFixture = "raptormark-tmp-twoprog:latest"

func buildTwoProgFixture(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("twoa.c", twoProgAGuestSrc)
	write("twob.c", twoProgBGuestSrc)
	write("ext_a.c", fmt.Sprintf(pgExtSrcFmt, 0xA1, 0xA1))

	// The plugin goes in postgres's real $libdir so `image.PluginDirs`
	// recognises it; nothing DT_NEEDEDs it, so only dlopen can reach it.
	df := "FROM " + builderImage() + ` AS build
COPY twoa.c twob.c ext_a.c /tmp/
RUN gcc -O2 -o /tmp/twoa /tmp/twoa.c -ldl \
 && gcc -O2 -o /tmp/twob /tmp/twob.c \
 && gcc -shared -fPIC -O2 -o /tmp/ext_a.so /tmp/ext_a.c
FROM debian:trixie-slim
COPY --from=build /tmp/twoa /usr/bin/twoa
COPY --from=build /tmp/twob /usr/bin/twob
COPY --from=build /tmp/ext_a.so /usr/lib/postgresql/17/lib/ext_a.so
CMD ["/usr/bin/twoa"]
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	abs := mustAbs(t, dir)
	if out, err := dockerBuild(ctx, abs, twoProgFixture); err != nil {
		t.Skipf("cannot build the twoprog fixture: %v\n%s", err, out)
	}
}

// TestBuildHandlesAMultiProgramClosure exercises the path that
// TestBuildCommandDrivesTheWholePipeline cannot: a closure with MORE THAN ONE
// program.
//
// # Why a second pipeline test
//
// The driver was written and proven against a one-program fixture, and two
// defects hid there. The worse one: every discovered plugin was being fused into
// every NON-ENTRY program. On `postgres:17` that is 79 plugins into each of 71
// programs -- and since `fuse.Fuse` ERRORS on a plugin it cannot satisfy rather
// than skipping it, one unresolvable plugin would fail the whole build naming a
// program that never wanted it. A one-program closure has no non-entry program,
// so nothing could see it.
//
// # What a PASS would look like if the claim were false
//
// The decisive check is not that the guest runs -- it would run either way here,
// since this fixture's single plugin IS satisfiable from both programs. It is
// that program B's fused image does not DEFINE the plugin's symbol. That is
// observable directly: the driver leaves each fused ELF under <out>/work, and
// `Pg_magic_func` is defined only by the plugin.
func TestBuildHandlesAMultiProgramClosure(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	buildTwoProgFixture(t, ctx, dir)

	out := filepath.Join(dir, "build")
	c := &pipeline.Build{
		Image:       twoProgFixture,
		Out:         out,
		Builder:     img,
		ObjectCache: os.Getenv("RAPTORMARK_OBJECT_CACHE"),
		Plugins:     "auto",
		Profile:     "wasmedge",
		MaxClosure:  10000,
		KeepRootfs:  false,
	}
	res, err := c.BuildForTest(ctx)
	if err != nil {
		t.Fatalf("raptormark build: %v", err)
	}
	t.Logf("built: %d program(s), %d unit(s), shared layout=%v",
		len(res.Programs), len(res.Units), res.SharedLayout)

	// The closure really has two programs, or the rest of this test is a
	// one-program test wearing a different name.
	if len(res.Programs) < 2 {
		t.Fatalf("the closure has %d program(s), want at least 2. Discovery did not "+
			"follow /usr/bin/twoa's execve of /usr/bin/twob, so the multi-program "+
			"path is not being exercised at all.", len(res.Programs))
	}
	if len(res.Units) < 1 {
		t.Fatalf("no units: discovery did not find the plugin in postgres's $libdir")
	}

	// ❗ THE ASSERTION THIS TEST EXISTS FOR.
	//
	// Program B is not the entry, so its fused image must not contain the
	// plugin. `Pg_magic_func` is defined only by the plugin, so its presence in
	// B's symbol table means every plugin was fused into every program.
	workDir := filepath.Join(out, "work")
	bFused := filepath.Join(workDir, "usr_bin_twob.fused")
	if _, err := os.Stat(bFused); err != nil {
		names, _ := filepath.Glob(filepath.Join(workDir, "*.fused"))
		t.Fatalf("cannot find program B's fused image at %s (have: %v): %v",
			bFused, names, err)
	}
	if defines(t, bFused, "Pg_magic_func") {
		t.Errorf("the NON-ENTRY program's image defines Pg_magic_func, so every " +
			"discovered plugin was fused into every program.\n" +
			"On postgres:17 that is 79 plugins into each of 71 programs, and one " +
			"plugin that some program cannot satisfy fails the entire build.\n" +
			"See optionsForNonEntryProgram in internal/pipeline.")
	}

	// THE CONTROL, without which the assertion above passes for any image at all
	// -- a misspelled symbol, a stripped table, the wrong file. The plugin's own
	// unit MUST define it.
	units, _ := filepath.Glob(filepath.Join(workDir, "*ext__a*.fused"))
	if len(units) == 0 {
		t.Fatalf("no unit image for ext_a.so under %s; the check above proves nothing",
			workDir)
	}
	if !defines(t, units[0], "Pg_magic_func") {
		t.Fatalf("the plugin's own unit (%s) does not define Pg_magic_func, so the "+
			"absence check above would pass against anything", units[0])
	}

	// And the artifact runs: dlopen in program A, execve into program B.
	got := runWasmIn(t, ctx, res.Module, nil,
		[]string{"RAPTORMARK_ROOTFS=/rootfs.img"}, "/:/out")
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(got, "TWOPROG-A dlopen ok") {
		t.Errorf("program A's dlopen did not succeed:\n%s", got)
	}
	if !strings.Contains(got, "TWOPROG-OK") {
		t.Errorf("program B did not run, so execve did not reach it:\n%s", got)
	}
	if n := strings.Count(got, "TWOPROG-A dlopen ok"); n != 1 {
		t.Errorf("program A's marker appears %d times; more than one means execve "+
			"fell back to program 0 and re-ran the caller", n)
	}
}

// defines reports whether a fused ELF defines this symbol.
func defines(t *testing.T, path, sym string) bool {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("reading symbols of %s: %v", path, err)
	}
	for _, s := range syms {
		// SHN_UNDEF means "referenced, not defined" -- which a program that
		// merely mentions the name would have, and is not what is being asked.
		if s.Name == sym && s.Section != elf.SHN_UNDEF {
			return true
		}
	}
	return false
}
