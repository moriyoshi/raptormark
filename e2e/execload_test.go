package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/link"
	"raptormark/internal/rootfs"
	"raptormark/internal/translate"
)

// The first program: it does nothing but execve the second. Deliberately
// minimal, because everything interesting happens inside that call.
const execLoadAGuestSrc = `#include <stdio.h>
#include <unistd.h>
int main(void) {
    printf("EXECLOAD-A running, about to execve /bin/xb\n");
    fflush(stdout);
    char *argv[] = {"/bin/xb", "arg1", 0};
    char *envp[] = {0};
    execve("/bin/xb", argv, envp);
    // Only reached if execve failed. perror goes to stderr, which the harness
    // also captures, and the message names the errno -- ENOEXEC is what a
    // deferred program used to give.
    perror("EXECLOAD-FAIL execve");
    return 1;
}
`

// The second program. It checks its own argv, because the failure this test
// guards against is not "nothing ran" but "the WRONG program ran under the
// right argv" -- the exec-map fallback that `execmap.rs` records four incidents
// for. A guest that only printed a marker could not tell the two apart.
const execLoadBGuestSrc = `#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
    if (argc != 2 || strcmp(argv[1], "arg1") != 0) {
        printf("EXECLOAD-FAIL wrong argv: argc=%d argv[1]=%s\n",
               argc, argc > 1 ? argv[1] : "(none)");
        return 1;
    }
    printf("EXECLOAD-B ran with argv[1]=%s\n", argv[1]);
    printf("EXECLOAD-OK\n");
    return 0;
}
`

// TestHostedLoaderServesAnExecveMidRun is the SECOND trigger of the loader seam.
//
// `dlopen` adds a unit to a running image; `execve` replaces the image with one.
// The plan's whole argument for a single seam is that they differ only in what
// happens afterwards -- so a mid-run load has to work for both, and until
// 2026-08-24 it worked for neither and then for only one.
//
// # What was actually broken
//
// `Programs::load` resolved every exec-map path to a registry INDEX at
// construction and dropped entries whose hash was not registered. Under a
// host-driven loader that is every program the host has not placed yet, so:
//
//   - `execve` to one returned ENOEXEC, and
//   - startup warned that those paths "fall back to program 0, so the guest runs
//     the WRONG PROGRAM", about what is under `hosted` the normal case.
//
// `crate::dlmap` was written hash-keyed from the start and its doc comment gives
// the reason. The exec map kept the old shape because nothing had exercised
// execve on a split artifact.
//
// # What a PASS would look like if the claim were false
//
//   - If program 1 were placed before boot, execve would resolve with no load.
//     Guarded: the harness places only `--main`, and the test asserts
//     `loads served: 1`.
//   - If execve silently fell back to program 0, the guest would re-run program
//     A -- which would print EXECLOAD-A twice and never reach EXECLOAD-OK.
//     Guarded by counting the A marker.
//   - If the right program ran with the wrong arguments, the marker would still
//     appear. Guarded inside the guest: program B checks its own argv.
func TestHostedLoaderServesAnExecveMidRun(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elfA := compileGuest(t, ctx, dir, "xa", execLoadAGuestSrc)
	elfB := compileGuest(t, ctx, dir, "xb", execLoadBGuestSrc)

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	topts := translate.Options{Runtime: "ecvisor"}
	outDir := filepath.Join(dir, "out")

	var progs []link.Program
	var objs []string
	for i, elf := range []string{elfA, elfB} {
		sha, err := translate.FileSHA256(elf)
		if err != nil {
			t.Fatal(err)
		}
		id := translate.ModuleID(elf, sha)
		prog := link.Program{Name: id, Index: i}
		progs = append(progs, prog)
		objs = append(objs, filepath.Join(outDir, id+".o"))
		frag := filepath.Join(dir, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			t.Fatal(err)
		}
		translateOne(t, ctx, b, translate.Request{
			ELF: elf, OutDir: outDir, ModuleID: id,
			Fragment: frag, Keep: prog.Symbol(), Options: topts,
		})
	}

	if _, err := link.WriteLinkInputs(dir, "registry.c", progs); err != nil {
		t.Fatal(err)
	}
	linked, err := link.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	execMap, err := link.ExecMap(linked, []link.ExecEntry{
		{Path: "/bin/xa", Hash: linked[0].Name},
		{Path: "/bin/xb", Hash: linked[1].Name},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A minimal rootfs. The files exist because execve resolves the path through
	// the VFS before consulting the exec map; their contents are never executed.
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"xa", "xb"} {
		if err := os.WriteFile(filepath.Join(root, "bin", n), []byte{0x7f, 'E', 'L', 'F'}, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecarImg, _, err := rootfs.Build(root, rootfs.Options{
		ExecMap: execMap,
		Boot:    &rootfs.Boot{Argv: []string{"/bin/xa"}, Cwd: "/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "rootfs.img"), sidecarImg, 0o644); err != nil {
		t.Fatal(err)
	}

	sideDir := filepath.Join(dir, "side")
	if err := b.Link(ctx, translate.LinkRequest{
		Registry: filepath.Join(dir, "registry.c"),
		Objects:  objs,
		Out:      filepath.Join(outDir, "execload.wasm"),
		SideOut:  sideDir,
		Profile:  "hosted",
	}); err != nil {
		t.Fatalf("link with --side-out --profile hosted: %v", err)
	}

	args := []string{
		"node", "/w/hostedembedder.mjs",
		"--supervisor", "/w/side/supervisor.wasm",
		"--program-size", fmt.Sprint(ecvProgramSize(t, link.FragmentC(progs[0]))),
		"--dir", "/w/out",
		"--env", "RAPTORMARK_ROOTFS=/rootfs.img",
		"--main", "/w/side/" + linked[0].Name + ".side.wasm",
		// Program 1 is OFFERED, not placed. Only the guest's execve can cause it
		// to exist -- which is the whole point.
		"--unit", fmt.Sprintf("%s:1:%s", linked[1].Name, "/w/side/"+linked[1].Name+".side.wasm"),
	}
	harness, err := os.ReadFile("testdata/hostedembedder.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostedembedder.mjs"), harness, 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, strings.Join(args, " "))
	t.Logf("harness output:\n%s", out)
	if runErr != nil {
		t.Fatalf("the mid-run embedder failed: %v", runErr)
	}

	if !strings.Contains(out, "EXECLOAD-OK") {
		t.Errorf("the exec'd program did not run to completion. Before 2026-08-24 " +
			"execve to a program the host had not placed returned ENOEXEC, because " +
			"the exec map resolved paths to registry indices at construction and " +
			"dropped the ones not registered yet.")
	}
	// The exec-map fallback: if execve silently selected program 0, the guest
	// would re-run program A and print its marker twice.
	if n := strings.Count(out, "EXECLOAD-A running"); n != 1 {
		t.Errorf("program A's marker appears %d times, want 1. More than one means "+
			"execve fell back to program 0 and re-ran the caller.", n)
	}
	// A load was really served, and for the exec target specifically.
	if !strings.Contains(out, "loads served: 1") {
		t.Errorf("the host served no side-module load, so execve resolved without " +
			"one -- program 1 was already reachable and this proved nothing about " +
			"the execve trigger.")
	}
	if !strings.Contains(out, "woke=1") {
		t.Errorf("no parked process was woken; the guest did not actually wait for " +
			"the load")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "EXECLOAD-FAIL") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "HOSTEDEMBEDDER-COMPLETE exit=0") {
		t.Errorf("the run did not complete cleanly")
	}
}
