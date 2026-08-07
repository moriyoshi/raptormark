package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/link"
	"raptormark/internal/rootfs"
	"raptormark/internal/translate"
)

// Two programs in ONE module, which is the shape the whole project is built
// around and which nothing in this suite could previously test: `liftOne`
// produces single-program modules, so an `execve` in an e2e guest resolves back
// to itself and never crosses a program boundary.
//
// That blind spot has a cost on record. Bounded arena snapshots shipped with a
// stale `materialized_prog` cache -- the arena's "whose image is loaded" tracker
// was updated in `load_current` but not in `exec_into` -- so after a guest
// exec'd, a switch back to a process still running the FIRST program skipped the
// image reload and ran it against the second program's text. Every test in the
// suite passed. It took a 40-minute postgres run to find, because dash execing
// psql and then being scheduled again is the smallest real case.
//
// hopA forks; the child execs hopB; then the two ping-pong over pipes. The
// parent is still running program 0 while the child runs program 1, so every
// round trip forces the scheduler to swap images in both directions -- and the
// parent checks, after each one, that its OWN text and data still work.
const hopAGuestSrc = `#define _GNU_SOURCE
#include <stdio.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define ROUNDS 5
static char marker[16] = "PROGRAM-A-DATA";

/* READ-ONLY data, which is the only thing a stale image tracker can corrupt.
   Everything writable is in the snapshot's range set and comes back regardless,
   and which CODE runs is decided by the per-program dispatch tables rather than
   by the bytes in the arena -- so a check on a function's return value or on a
   writable global cannot see this bug at all. An earlier version of this guest
   checked exactly those two things and passed with the bug reintroduced.
   The const array is read through a VOLATILE pointer because the compiler will
   otherwise fold strcmp(x, "literal") into immediate comparisons and never
   touch .rodata. */
static const char rodata_tag[24] = "PROGRAM-A-RODATA-xxxxxx";

static int rodata_intact(void) {
	const char *volatile p = rodata_tag;
	return p[0] == 'P' && p[8] == 'A' && p[10] == 'R';
}

int main(void) {
	int up[2], down[2];
	if (pipe(up) != 0 || pipe(down) != 0) { printf("FAIL pipe\n"); return 1; }

	pid_t kid = fork();
	if (kid < 0) { printf("FAIL fork\n"); return 1; }
	if (kid == 0) {
		char rfd[16], wfd[16];
		snprintf(rfd, sizeof rfd, "%d", up[0]);
		snprintf(wfd, sizeof wfd, "%d", down[1]);
		char *const argv[] = { "/bin/hopb", rfd, wfd, 0 };
		execv("/bin/hopb", argv);
		_exit(90);           /* exec failed: the exec map did not resolve hopb */
	}

	int failures = 0;
	for (int r = 0; r < ROUNDS; r++) {
		char tok = 'a';
		if (write(up[1], &tok, 1) != 1) { printf("FAIL parent write\n"); failures++; break; }
		if (read(down[0], &tok, 1) != 1) { printf("FAIL parent read\n"); failures++; break; }
		if (tok != 'b') { printf("FAIL wrong token from the other program\n"); failures++; }
		/* Program 0's own read-only image, after a round trip through program 1. */
		if (!rodata_intact()) { printf("FAIL program A rodata after a cross-program switch\n"); failures++; }
		if (strcmp(marker, "PROGRAM-A-DATA") != 0) { printf("FAIL program A data after a cross-program switch\n"); failures++; }
	}
	close(up[1]);
	int st = 0;
	if (waitpid(kid, &st, 0) != kid) { printf("FAIL waitpid\n"); failures++; }
	if (!WIFEXITED(st) || WEXITSTATUS(st) != 0) {
		printf("FAIL child exit status %d\n", WIFEXITED(st) ? WEXITSTATUS(st) : -1);
		failures++;
	}
	if (failures == 0) { printf("CROSSPROG-OK\n"); }
	return failures == 0 ? 0 : 1;
}
`

const hopBGuestSrc = `#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define ROUNDS 5
static char marker[16] = "PROGRAM-B-DATA";
static const char rodata_tag[24] = "PROGRAM-B-RODATA-yyyyyy";

static int rodata_intact(void) {
	const char *volatile p = rodata_tag;
	return p[0] == 'P' && p[8] == 'B' && p[10] == 'R';
}

int main(int argc, char **argv) {
	if (argc < 3) { _exit(91); }
	int rfd = atoi(argv[1]), wfd = atoi(argv[2]);
	for (int r = 0; r < ROUNDS; r++) {
		char tok = 0;
		if (read(rfd, &tok, 1) != 1) { _exit(92); }
		if (tok != 'a') { _exit(93); }
		if (!rodata_intact()) { _exit(94); }
		if (strcmp(marker, "PROGRAM-B-DATA") != 0) { _exit(95); }
		tok = 'b';
		if (write(wfd, &tok, 1) != 1) { _exit(96); }
	}
	_exit(0);
}
`

// liftTwo translates two ELFs into ONE module and returns (wasmPath, sidecarPath).
// The sidecar carries the exec map, which is what lets `execv("/bin/hopb")`
// select program 1 -- without it the runtime falls back to program 0 and the
// guest silently re-runs itself.
func liftTwo(t *testing.T, ctx context.Context, img, dir, elfA, elfB string) (string, string) {
	t.Helper()
	return liftTwoNamed(t, ctx, img, dir, [2]string{"hopa", "hopb"}, [2]string{elfA, elfB})
}

// liftTwoNamed is liftTwo with the guest names as a parameter, so a second
// two-program test does not have to duplicate the registry/manifest/exec-map
// ceremony. Program 0 is the boot program; each program is reachable at
// /bin/<name>.
func liftTwoNamed(
	t *testing.T,
	ctx context.Context,
	img, dir string,
	names [2]string,
	elfs [2]string,
) (string, string) {
	t.Helper()
	elfA, elfB := elfs[0], elfs[1]
	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	opts := translate.Options{Runtime: "ecvisor"}
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
		objs = append(objs, "/out/"+id+".o")
		frag := filepath.Join(dir, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			t.Fatal(err)
		}
		translateOne(t, ctx, b, translate.Request{
			ELF: elf, OutDir: outDir, ModuleID: id,
			Fragment: frag, Keep: prog.Symbol(), Options: opts,
		})
	}

	// Registry and manifest together, so the exec map below is validated
	// against the program list this link actually contained.
	if _, err := link.WriteLinkInputs(dir, "registry.c", progs); err != nil {
		t.Fatal(err)
	}
	linked, err := link.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	execMap, err := link.ExecMap(linked, []link.ExecEntry{
		{Path: "/bin/" + names[0], Hash: linked[0].Name},
		{Path: "/bin/" + names[1], Hash: linked[1].Name},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A minimal rootfs. The files exist because execve resolves the path
	// through the VFS before consulting the exec map; their contents are never
	// executed -- the lifted programs are in the module.
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, "bin", n), []byte("placeholder\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	image, _, err := rootfs.Build(root, rootfs.Options{ExecMap: execMap, Boot: &rootfs.Boot{
		Argv: []string{"/bin/" + names[0]},
		Cwd:  "/",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Into outDir, not dir: `linkAll` returns the module inside outDir, and
	// `runWasmIn` mounts the module's OWN directory as /out. A sidecar written
	// beside the registry instead lands outside the mount and the runtime
	// reports it "set but unreadable" -- then runs with no exec map at all, so
	// execv falls back to program 0 and the guest re-runs itself.
	sidecar := filepath.Join(outDir, "rootfs.img")
	if err := os.WriteFile(sidecar, image, 0o644); err != nil {
		t.Fatal(err)
	}
	wasm := linkAll(t, ctx, dir, outDir, names[0]+".wasm", objs)
	return wasm, sidecar
}

// A switch between processes running DIFFERENT programs. Under bounded
// snapshots -- the only scheme since 2026-08-22 -- such a switch must
// re-materialise the image, and the tracker that decides whether to do so is a
// cache; the failure mode is running one program's text with another's.
//
// It absorbed TestCrossProgramSwitchesUnderBoundedSnapshots. The two existed as
// a pair while an environment variable chose the scheme, with this half pinned
// to the full-buffer path where a cross-program switch trades whole arenas and
// the image travels with them. That variable was removed and so was that path,
// so the twin would have run this exact test a second time under a name
// claiming it covered something else -- which is worse than not having it, and
// is the same silent duplication the twin was originally added to fix.
func TestCrossProgramSwitchesUnderEcvisor(t *testing.T) {
	runCrossProg(t, nil)
}

func runCrossProg(t *testing.T, env []string) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elfA := compileGuest(t, ctx, dir, "hopa", hopAGuestSrc)
	elfB := compileGuest(t, ctx, dir, "hopb", hopBGuestSrc)
	wasm, sidecar := liftTwo(t, ctx, img, dir, elfA, elfB)

	// `--dir /:/out` and a GUEST-side path. Without a preopen the runtime cannot
	// open the sidecar at all and reports it "set but unreadable"; and the path
	// is what the guest sees through the preopen, so /out/rootfs.img on the host
	// is /rootfs.img here.
	full := append([]string{"RAPTORMARK_ROOTFS=/" + filepath.Base(sidecar)}, env...)
	out := runWasmIn(t, ctx, wasm, nil, full, "/:/out")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "CROSSPROG-OK") {
		t.Errorf("guest did not reach CROSSPROG-OK; full output:\n%s", out)
	}
}
