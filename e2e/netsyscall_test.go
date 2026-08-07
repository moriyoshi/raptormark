package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The one piece of reconstruction evidence still unspent, turned into a guard.
//
// # ❗ Why this exists
//
// All three guests in `net_test.go` were reconstructed by disassembling the lost
// `raptormark-test-net*` fixture images, and equivalence was established against
// those binaries on stdout, exit status AND syscall sequence. On 2026-08-25 two
// of them were deliberately changed to coordinate ports, which SPENT that
// evidence: their sequences now differ from the originals' by construction, and
// the originals cannot be re-compared because the images are unreproducible
// artifacts of the lost tree.
//
// `netforkserver` was not touched. Its equivalence still stands, and the
// sequence it was verified against is written down in `net_test.go`'s package
// doc -- **as prose, three screens above the code**. That is exactly the shape
// this tree has been paying for all week: a property protected by a paragraph.
// Nothing in the build, the tests or a review notices when an edit destroys it;
// the guest still compiles, still passes, still does what its test says.
//
// So the paragraph is now executable. If someone edits `netForkServerSrc`, this
// fails and names what is being spent.
//
// # What it does NOT claim
//
// ⚠️ It compares against the sequence RECORDED in the package doc, not against
// the original binary -- that binary is gone. This proves the guest still does
// what the reconstruction was verified to do; it cannot re-verify the
// reconstruction itself. That distinction is the whole reason the evidence is
// worth preserving rather than regenerating.

// forkServerParentSeq and forkServerChildSeq are transcribed from
// `net_test.go`'s package doc, which records what the ORIGINAL binaries did.
//
// ❗ Transcribed deliberately rather than derived. Deriving the expectation from
// a run would make this a change-detector that ratifies whatever the guest
// currently does -- the opposite of evidence.
var (
	forkServerParentSeq = []string{
		"socket", "bind", "listen", "getsockname", "clone", "accept",
		"read", "write", "wait4", "close", "close", "write",
	}
	forkServerChildSeq = []string{"socket", "connect", "write", "read", "close"}
)

// traced matches strace's `<pid> <syscall>(` prefix under `-f`.
var traced = regexp.MustCompile(`^(\d+) +([a-z_0-9]+)\(`)

// normalizeSyscall folds the variants a libc may pick between.
//
// ⚠️ This is a real weakening and is bounded on purpose: only where the kernel
// offers two spellings of ONE operation. `accept4` is `accept` with flags;
// `clone3` is `clone` with a struct. Folding anything else would let a genuine
// behavioural change hide behind a rename.
func normalizeSyscall(s string) string {
	switch s {
	case "accept4":
		return "accept"
	case "clone3":
		return "clone"
	default:
		return s
	}
}

// TestNetForkServerMatchesItsRecordedSyscallSequence.
func TestNetForkServerMatchesItsRecordedSyscallSequence(t *testing.T) {
	ctx := ctxFor(t)
	requireE2E(t) // the guest is compiled in the builder image
	strace, err := exec.LookPath("strace")
	if err != nil {
		// ⚠️ A SKIP, and the message says what is lost rather than just why.
		// strace absence is an environment fact, not a defect -- but this guard
		// going quiet is precisely how the properties it protects get spent, so
		// the skip has to be legible to whoever reads the run.
		t.Skip("strace is not on PATH, so the recorded syscall sequence of " +
			"netForkServerSrc is NOT being checked. It is the last unspent " +
			"reconstruction evidence from the lost raptormark-test-net* images; " +
			"an edit to that guest will go unnoticed on this machine.")
	}

	dir := t.TempDir()
	bin := compileGuest(t, ctx, dir, "netforkstrace", netForkServerSrc)
	out := filepath.Join(dir, "trace.txt")

	cmd := exec.CommandContext(ctx, strace, "-f", "-o", out, bin)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("strace could not trace the guest (%v): %s\n"+
			"This is usually kernel.yama.ptrace_scope. The sequence is NOT being "+
			"checked on this machine.", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	// Group by pid, keeping only the syscalls the record names. The record is a
	// filtered view: a real trace also has mmap, brk, exit_group and more, and
	// listing those would pin libc's startup rather than the guest's behaviour.
	want := map[string]bool{}
	for _, s := range append(append([]string{}, forkServerParentSeq...), forkServerChildSeq...) {
		want[s] = true
	}
	order := []string{}
	byPid := map[string][]string{}
	for _, line := range strings.Split(string(b), "\n") {
		m := traced.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		call := normalizeSyscall(m[2])
		if !want[call] {
			continue
		}
		if _, seen := byPid[m[1]]; !seen {
			order = append(order, m[1])
		}
		byPid[m[1]] = append(byPid[m[1]], call)
	}

	// ❌ Exactly two pids. Fewer means the fork did not happen or strace did not
	// follow it, and either would leave a "passing" comparison against one
	// sequence while the other went unchecked.
	if len(order) != 2 {
		t.Fatalf("expected exactly 2 traced processes (the fork), got %d: %v\n"+
			"trace head:\n%s", len(order), order, firstLines(string(b), 15))
	}
	parent, child := byPid[order[0]], byPid[order[1]]

	if got := strings.Join(parent, " "); got != strings.Join(forkServerParentSeq, " ") {
		t.Errorf("the PARENT's syscall sequence changed.\n got: %s\nwant: %s\n\n"+
			"❗ netforkserver is the last of the three guests whose reconstruction "+
			"equivalence is unspent -- it was verified against a binary from the "+
			"lost raptormark-test-net* images, which cannot be re-compared. If this "+
			"change is intended, you are spending that evidence: say so where the "+
			"package doc records it, as was done for netserver and netclient.",
			got, strings.Join(forkServerParentSeq, " "))
	}
	if got := strings.Join(child, " "); got != strings.Join(forkServerChildSeq, " ") {
		t.Errorf("the CHILD's syscall sequence changed.\n got: %s\nwant: %s",
			got, strings.Join(forkServerChildSeq, " "))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
