package e2e

import (
	"strings"
	"testing"
)

// The ecvisor half of `TestNetGuestsNativeContract`, which had no ecvisor half
// for as long as anyone had looked.
//
// # ❗ Why this did not exist, which is the interesting part
//
// `net_test.go` said the lifted side "needs syscalls the runtime does not have
// yet (socketpair/sendmsg/recvmsg are absent) ... so the ecvisor side is added
// when they land". They landed. Verified 2026-08-25: all three are dispatched
// (`runtime/src/sys.rs:697-699`) and implemented, not stubbed, and
// `e2e/uds_test.go` exercises guest AF_UNIX end to end.
//
// So the pair had a trusted native baseline and nothing checking that ecvisor
// reproduced it, guarded by a comment asserting a blocker that no longer
// existed. ⚠️ A stale "we cannot yet" is more expensive than a stale pointer: a
// dangling pointer wastes a search, a false blocker closes off work.
//
// # Why only the forkserver guest
//
// `net_test.go` has three, and the other two are not liftable as they stand:
// `netServerSrc` binds a FIXED 47826 and `netClientSrc` dials a FIXED 47825,
// each expecting an in-process Go peer on the host.
//
// ⚠️ Those ports are DELIBERATE, not careless -- `net_test.go:39-42` says they
// are the originals' and are fixed "because the guest's whole job is to be
// found at a known address by a peer it does not coordinate with". The obstacle
// is that a LIFTED guest runs inside a container, so its loopback is not the
// host's unless the container gets `--network host` (as `wasixnet_test.go`
// needs) -- and that shares the host port space, which is exactly when a fixed
// port bites: `AGENTS.md` records two overlapping `go test ./e2e/` runs
// producing EADDRINUSE and a wall of unrelated-looking errnos.
// So an ecvisor half for those two is a decision about the pair, not an
// addition to it. See `.agents/docs/TODO.md`.
//
// `netForkServerSrc` has neither problem: it binds port 0, learns the port with
// `getsockname`, and is its own peer across a `fork`. Nothing outside the guest
// can make it flaky, which is exactly what makes it the one to lift first.
//
// # What it covers that no other e2e guest does
//
// Sockets AND fork in one process, with the two ends of a connection on either
// side of the fork. `uds_test.go` is AF_UNIX, the socket tests do not fork, and
// the fork tests do not open sockets. The parent's listening descriptor has to
// survive into the child's world well enough for the child to CONNECT to it and
// the parent to ACCEPT -- which is a claim about the interaction of two
// subsystems, and interactions are what a suite of single-subsystem tests
// cannot reach.

// assertForkServerOK judges a run against the NATIVE contract, in the native
// test's own terms: exit 0 and exactly "ok\n".
//
// ⚠️ The banner is checked with Contains rather than equality, because a wasm
// runtime writes its own diagnostics to the same stream. That is a real
// weakening, so the FAIL arm below compensates: the guest returns non-zero for
// every failure mode it has, and a non-zero exit fails the run before this is
// reached.
//
// ❗ It matches "ok\n", not "ok". Bare "ok" is a substring of far too much --
// any runtime banner containing the word would satisfy it, and this assertion
// would then be green for a guest that never ran. Measured 2026-08-25 that
// neither wasmedge nor wasmtime emits it here, but that is a property of two
// current releases, not a guarantee, and the newline is free.
func assertForkServerOK(t *testing.T, profile, out string) {
	t.Helper()
	if !strings.Contains(out, "ok\n") {
		t.Errorf("[%s] the fork/socket guest did not print its success banner.\n"+
			"It returns non-zero for every failure it can detect, so reaching here "+
			"with no banner means it died rather than failed a check.\nfull output:\n%s",
			profile, out)
	}
}

// TestNetForkServerUnderEcvisor is the shipping profile's half of the pair.
//
// ❗ The native baseline is `TestNetGuestsNativeContract/forkserver`, which runs
// the SAME source on this host. That is what makes this a differential rather
// than an assertion: if the two disagree, one of them is wrong about Linux, and
// the native one is the half that can be trusted.
func TestNetForkServerUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "netforkecv", netForkServerSrc)
	// No --profile: the default is the shipping one, and naming it would let a
	// change to the default pass this under the old backend.
	wasm := liftOne(t, ctx, img, dir, elf, "netforkecv")

	out, err := runEpollGuest(ctx, t, wasm, "wasmedge --enable-all", nil)
	if err != nil {
		t.Fatalf("the fork/socket guest failed under wasmedge: %v\n%s", err, out)
	}
	assertForkServerOK(t, "wasmedge", out)
}

// TestNetForkServerUnderLoopback runs the same guest on the in-process network.
//
// ❗ THIS IS THE ONE THAT COULD LEGITIMATELY FAIL, and it is worth running for
// that reason. `net::loopback` keeps its sockets in a plain `Vec` inside the
// runtime, so "the child connects to the port the parent bound" is a question
// about how that table interacts with ecvisor's fork -- not something the
// wasmedge run can answer, because there the host kernel owns the sockets and
// fork is not involved in reaching them.
//
// ⚠️ It also exercises the 2026-08-25 ephemeral-port fix through a fork. The
// guest binds port 0 and hands `getsockname`'s answer to the child; before that
// fix `bind` stored port 0 verbatim, so the child would have dialled port 0 --
// which `find_listener` matched, meaning the guest PASSED while every real
// server advertised a port of 0.
func TestNetForkServerUnderLoopback(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "netforklb", netForkServerSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "netforklb", "--profile", "loopback")

	out, err := runEpollGuest(ctx, t, wasm, "wasmtime run", nil)
	if err != nil {
		t.Fatalf("the fork/socket guest failed under the loopback profile: %v\n%s\n"+
			"⚠️ Unlike the wasmedge run, the socket table here lives INSIDE the "+
			"runtime, so a failure implicates the interaction of net::loopback with "+
			"ecvisor's fork rather than the socket layer alone.", err, out)
	}
	assertForkServerOK(t, "loopback", out)
}
