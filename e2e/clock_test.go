package e2e

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// clockGuestSrc reads both clocks around a sleep and reports the deltas AND the
// absolute readings.
//
// ⚠️ THE ABSOLUTE READINGS ARE NOT DECORATION. The delta assertions can only see
// a clock that MOVED wrongly; the absolute ones see a clock that IS the wrong
// clock. `CLOCK_MONOTONIC` served from the wall clock reads about fifty-six
// years, and a runtime that ignores `clockid` entirely -- which is what this one
// used to do -- produces exactly that with perfectly plausible deltas.
//
// The sleep is a plain relative `nanosleep`, i.e. `clock_nanosleep` on
// CLOCK_MONOTONIC, because that is what every guest in this tree actually does.
const clockGuestSrc = `#include <stdio.h>
#include <time.h>

static long long msec(struct timespec a, struct timespec b) {
	long long da = (long long)a.tv_sec * 1000 + a.tv_nsec / 1000000;
	long long db = (long long)b.tv_sec * 1000 + b.tv_nsec / 1000000;
	return db - da;
}

int main(void) {
	struct timespec m0, r0, m1, r1;
	clock_gettime(CLOCK_MONOTONIC, &m0);
	clock_gettime(CLOCK_REALTIME, &r0);
	printf("CLOCK-START\n");
	fflush(stdout);

	struct timespec req = { 0, 600 * 1000 * 1000 }; /* 600ms */
	nanosleep(&req, NULL);

	clock_gettime(CLOCK_MONOTONIC, &m1);
	clock_gettime(CLOCK_REALTIME, &r1);
	printf("CLOCK-MONO-MS %lld\n", msec(m0, m1));
	printf("CLOCK-REAL-MS %lld\n", msec(r0, r1));
	printf("CLOCK-MONO-ABS-S %lld\n", (long long)m1.tv_sec);
	printf("CLOCK-REAL-ABS-S %lld\n", (long long)r1.tv_sec);
	return 0;
}
`

// TestGuestTimersSurviveAWallClockStep is the guard the clock split exists for.
//
// ⚠️ EVERY OTHER TIMER TEST IN THIS TREE RUNS ON A CLOCK THAT NEVER JUMPS, and
// would pass identically before and after the fix. That is why this one needed
// a new instrument rather than a new assertion: `--clock-step-ms` makes the host
// shim add an hour to the REALTIME clock -- and only to REALTIME -- at the first
// moment the guest is IDLE, which is the one instant it has an armed deadline
// and nothing else to do.
//
// The step has to land AFTER the deadline is armed. Step earlier and a
// wall-clock deadline is simply computed from the already-stepped clock and the
// sleep behaves correctly for the wrong reason, so the test would certify the
// bug. That is why the trigger is the first idle and not a timer: how long boot
// takes depends on the module, and no fixed delay can hit the window.
//
// WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? Two of the five checks
// below exist only to answer that, and both are about the HARNESS rather than
// the runtime: if the step never landed, `CLOCK-REAL-MS` stays near 600 and the
// monotonic assertion passes having tested nothing. If both clocks were broken
// the same way, `CLOCK-REAL-ABS-S` catches it.
func TestGuestTimersSurviveAWallClockStep(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "clockstep", clockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "clockstep", "--profile", "loopback")

	// One hour. Not a plausible NTP correction -- deliberately far outside every
	// tolerance here, so a failure reports a number nobody has to interpret.
	const stepMs = 3_600_000
	out := runNodeHostArgs(t, ctx, node, wasm, "",
		"--reentrant", "--env", "RAPTORMARK_ECV_NONBLOCK=1",
		"--clock-step-ms", strconv.Itoa(stepMs))

	for _, want := range []string{"CLOCK-START", "HOST-CLOCK-STEP", "HOST-EXIT: 0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q; the run did not get far enough to test anything. "+
				"Full output:\n%s", want, out)
		}
	}

	monoMs := clockValue(t, out, "CLOCK-MONO-MS")
	realMs := clockValue(t, out, "CLOCK-REAL-MS")
	monoAbsS := clockValue(t, out, "CLOCK-MONO-ABS-S")
	realAbsS := clockValue(t, out, "CLOCK-REAL-ABS-S")
	afterStepMs := clockValue(t, out, "HOST-AFTER-STEP-MS")
	t.Logf("mono delta %d ms, real delta %d ms, mono abs %d s, real abs %d s, "+
		"host elapsed after the step %d ms", monoMs, realMs, monoAbsS, realAbsS, afterStepMs)

	// (1) HARNESS CHECK: the step actually landed. Without this the monotonic
	// assertion below is satisfied by a run in which nothing was perturbed.
	if realMs < stepMs/2 {
		t.Fatalf("CLOCK_REALTIME advanced only %d ms across a %d ms step, so the "+
			"clock never moved and nothing below was tested", realMs, stepMs)
	}

	// (2) The claim: a wall-clock step does not move the monotonic clock.
	if monoMs < 400 || monoMs > 5_000 {
		t.Errorf("CLOCK_MONOTONIC advanced %d ms across a 600 ms sleep. It is "+
			"following the wall clock, which stepped by %d ms during that sleep; "+
			"a monotonic clock must not.", monoMs, stepMs)
	}

	// (3) The claim, from the scheduler's side: an armed deadline is not moved
	// by the step either. Measured by the HOST, which is the only clock in this
	// run that neither the guest nor the step can touch. A wall-clock deadline
	// plus a forward step is already expired, so the guest wakes at once.
	if afterStepMs < 400 {
		t.Errorf("the guest finished %d ms after the clock stepped forward, but it "+
			"still had ~600 ms of sleep left. Its deadline moved with the wall "+
			"clock instead of being measured against the monotonic one.", afterStepMs)
	}

	// (4) The original defect stated directly: `clock_gettime` ignored its
	// `clockid` and answered every clock from the wall clock.
	const oneYearS = 365 * 24 * 60 * 60
	if monoAbsS > oneYearS {
		t.Errorf("CLOCK_MONOTONIC reads %d s. Linux counts it from BOOT; this is "+
			"time since the epoch, so the clock id is being ignored.", monoAbsS)
	}

	// (5) HARNESS CHECK: realtime is still the wall clock, so (4) is not passing
	// because both clocks were broken in the same direction.
	if realAbsS < 1_700_000_000 {
		t.Errorf("CLOCK_REALTIME reads %d s, which is before 2023. It is no longer "+
			"the wall clock, so the monotonic assertion above proves nothing.",
			realAbsS)
	}
}

// clockBenchIters is the loop count, and it is substituted into the guest
// rather than written twice: the Go side divides by it to get a per-call cost,
// so a drift between the two would scale every number silently.
const clockBenchIters = 200000

// clockBenchMarker introduces the lines `bin/run.ts --stamp` timestamps.
const clockBenchMarker = "BENCH-MARK"

// clockBenchGuestSrc times `clock_gettime` against a syscall that does almost
// nothing, so the two numbers separate the cost of REACHING ecvisor from the
// cost of the clock read itself.
//
// `getpid` is the control: same `svc` entry, same dispatch table, and a handler
// that returns a field. Whatever `clock_gettime` costs ABOVE it is the host
// clock read -- which is the only part a vDSO would be trying to remove, and
// the part it cannot remove here.
//
// ⚠️ TIMED TWICE, ONCE FROM EACH SIDE, and the second one is why this is
// trustworthy at all. The in-guest bracket (`BENCH-*-NS`) is only safe against
// SOME kinds of wrongness: a clock running at the wrong RATE scales both loops
// equally and the ratio survives, but a clock that advances per READ does not.
// Against the cached-clock experiment of 2026-08-21 the getpid loop -- two
// clock reads bracketing 200000 syscalls -- reports 0 ns/call, because only two
// reads happened.
//
// So each loop is also bracketed by a MARKER line that the host timestamps on
// its own clock (`HOST-STAMP-<NAME>-US`, see `web/src/stamp.ts`), which is the
// same trick `HOST-AFTER-STEP-MS` uses in the test above and for the same
// reason: the host's clock is the only one in the run that the thing under
// measurement cannot reach. The in-guest numbers are KEPT rather than replaced,
// because the disagreement between the two is itself the diagnosis -- a control
// loop that reads 0 ns/call from inside and 40 ns/call from outside names the
// defect exactly.
//
// The marker costs one `fd_write` at each end, so a host-timed interval carries
// a few microseconds of overhead that the guest-timed one does not. Spread over
// 200000 iterations that is well under a nanosecond per call, i.e. below the
// resolution of anything this is used to decide.
var clockBenchGuestSrc = fmt.Sprintf(`#define _GNU_SOURCE
#include <stdio.h>
#include <time.h>
#include <unistd.h>
#include <sys/syscall.h>

#define N %d

static long long ns(struct timespec a, struct timespec b) {
	return ((long long)b.tv_sec - a.tv_sec) * 1000000000LL + (b.tv_nsec - a.tv_nsec);
}

/* One line, flushed. fd_write is a synchronous host import, so the host
   observes this at the instant the guest reaches it. That is the mechanism. */
static void mark(const char *name) {
	printf("%s %%s\n", name);
	fflush(stdout);
}

int main(void) {
	struct timespec t0, t1, x;
	volatile long sink = 0;

	mark("CLOCK-BEGIN");
	clock_gettime(CLOCK_MONOTONIC, &t0);
	for (int i = 0; i < N; i++) {
		clock_gettime(CLOCK_MONOTONIC, &x);
		sink += x.tv_nsec;
	}
	clock_gettime(CLOCK_MONOTONIC, &t1);
	mark("CLOCK-END");
	printf("BENCH-CLOCK-NS %%lld\n", ns(t0, t1) / N);

	mark("GETPID-BEGIN");
	clock_gettime(CLOCK_MONOTONIC, &t0);
	for (int i = 0; i < N; i++) {
		sink += syscall(SYS_getpid);
	}
	clock_gettime(CLOCK_MONOTONIC, &t1);
	mark("GETPID-END");
	printf("BENCH-GETPID-NS %%lld\n", ns(t0, t1) / N);

	printf("BENCH-SINK %%ld\n", (long)(sink != 0));
	return 0;
}
`, clockBenchIters, clockBenchMarker)

// TestClockBenchIsolatesTheHostClockRead measures rather than asserts, and is
// kept because the vDSO question gets re-asked.
//
// ⚠️ IT IS AN INSTRUMENT, NOT A GUARD. It has no pass condition beyond the run
// completing, so it must never be in the default path -- and a number from it is
// a measurement of THIS machine on THIS day, not a property of the tree.
//
// What it exists to settle: a vDSO does not transfer to raptormark, because
// there is no privilege transition to avoid -- `svc` is already a plain call
// from lifted code into `sys::svc`. This splits the per-call cost to show it:
// whatever `clock_gettime` costs ABOVE `getpid` is the host clock read, and a
// vDSO here would still have to make it.
//
// ⚠️ It does NOT establish that clock reads matter. A serving nginx spends
// ~0.13% of a request on them, and a cached clock was built and refuted by
// removal on 2026-08-21.
//
// ⚠️ THE REPORTED NUMBERS ARE THE HOST-TIMED ONES. The guest-timed pair is
// printed beside them as a cross-check, not as the answer: it is measured with
// the clock under test and collapses to 0 ns/call against any clock that
// advances per read. When the two disagree, believe the host.
func TestClockBenchIsolatesTheHostClockRead(t *testing.T) {
	if os.Getenv("RAPTORMARK_CLOCK_BENCH") != "1" {
		t.Skip("set RAPTORMARK_CLOCK_BENCH=1; this measures, it does not assert")
	}
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "clockbench", clockBenchGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "clockbench", "--profile", "loopback")

	out := runNodeHostArgs(t, ctx, node, wasm, "",
		"--reentrant", "--env", "RAPTORMARK_ECV_NONBLOCK=1",
		"--stamp", clockBenchMarker)
	if !strings.Contains(out, "HOST-EXIT: 0") {
		t.Fatalf("the benchmark guest did not finish:\n%s", out)
	}

	hostClockNs := hostNsPerCall(t, out, "CLOCK")
	hostGetpidNs := hostNsPerCall(t, out, "GETPID")
	guestClockNs := clockValue(t, out, "BENCH-CLOCK-NS")
	guestGetpidNs := clockValue(t, out, "BENCH-GETPID-NS")

	t.Logf("HOST-TIMED: clock_gettime %d ns/call, getpid %d ns/call, "+
		"host clock read ~%d ns (%.1fx)", hostClockNs, hostGetpidNs,
		hostClockNs-hostGetpidNs, float64(hostClockNs)/float64(max(hostGetpidNs, 1)))
	t.Logf("GUEST-TIMED (cross-check only, uses the clock under test): "+
		"clock_gettime %d ns/call, getpid %d ns/call", guestClockNs, guestGetpidNs)
	if guestGetpidNs == 0 || guestClockNs == 0 {
		t.Logf("⚠️ a guest-timed loop reads 0 ns/call: the guest clock does not " +
			"advance between two reads that bracket the loop. That is precisely " +
			"the case the host timing exists for -- the host numbers above stand.")
	}
}

// hostNsPerCall converts a pair of host-side marker stamps into a per-iteration
// cost in nanoseconds.
//
// ⚠️ THE HARNESS CHECK IS THE POINT OF THE ZERO TEST BELOW, and it is not a
// performance threshold -- this file must not grow one. 200000 guest syscalls
// cannot take under a microsecond of real time on any machine, so a zero span
// means the stamps did not measure what they claim, which is the exact defect
// the in-guest timing had. Reporting 0 ns/call quietly is how that defect
// survived the first time.
func hostNsPerCall(t *testing.T, out, phase string) int64 {
	t.Helper()
	begin := clockValue(t, out, "HOST-STAMP-"+phase+"-BEGIN-US")
	end := clockValue(t, out, "HOST-STAMP-"+phase+"-END-US")
	if end <= begin {
		t.Fatalf("host stamps for %s span %d us (begin %d, end %d). The host clock "+
			"did not advance across %d guest calls, so the stamping is broken, not "+
			"the guest.", phase, end-begin, begin, end, clockBenchIters)
	}
	return (end - begin) * 1000 / clockBenchIters
}

// TestHostStampLinesParseAsClockValues runs `hostNsPerCall` over output
// `bin/run.ts --stamp` really produced. It needs neither Docker nor node.
//
// ⚠️ WHAT IT COVERS AND WHAT IT DOES NOT. It covers this side: the marker names
// `hostNsPerCall` builds, the regex that finds them, and the microsecond ->
// ns/call arithmetic. It does NOT re-run run.ts, so a future change to the
// EMITTER still passes here and fails in the expensive test. What keeps that
// honest is that the input is a verbatim capture rather than a format written
// from memory -- from
//
//	node bin/run.ts --module <probe>.wasm --stamp BENCH-MARK
//
// where the probe (a hand-written WASI module, not a lifted artifact) times a
// loop of 200000 host round trips with a CACHED clock, one that never advances.
// That capture is the defect stated in full: `BENCH-GETPID-NS 0` next to host
// stamps 26936 us apart. The guest measured 0 ns/call; the host measured 134.
func TestHostStampLinesParseAsClockValues(t *testing.T) {
	const captured = "HOST-STAMP-GETPID-BEGIN-US: 68567\n" +
		"BENCH-MARK GETPID-BEGIN\n" +
		"HOST-STAMP-GETPID-END-US: 95503\n" +
		"BENCH-MARK GETPID-END\n" +
		"BENCH-GETPID-NS 0\n" +
		"HOST-EXIT: 0\n"

	// The capture is of a run with the same iteration count this file uses; if
	// that ever stops being true the number below is not comparable.
	if clockBenchIters != 200000 {
		t.Fatalf("the capture was taken at 200000 iterations, not %d", clockBenchIters)
	}
	if got := hostNsPerCall(t, captured, "GETPID"); got != 134 {
		t.Errorf("host-timed = %d ns/call, want 134 (26936 us over 200000 calls)", got)
	}
	// The guest-timed line is what the host timing exists to replace: it is
	// present, it parses, and it says zero.
	if got := clockValue(t, captured, "BENCH-GETPID-NS"); got != 0 {
		t.Errorf("guest-timed = %d, want 0 -- the capture is of a stopped clock", got)
	}
	// And a marker line must NOT be mistaken for a value line: it carries a
	// name, not a number. A parser that matched it would hand back whatever
	// integer happened to follow.
	if reClockValue.MatchString("BENCH-MARK GETPID-BEGIN") {
		t.Error("a marker line parses as a value line; the two are not distinguishable")
	}
}

// The marker class deliberately excludes `:`, so a greedy match cannot swallow
// the separator and hand back a name with a colon stuck to it -- which is what
// `(\S+)[: ]` did, and it failed by reporting the line as ABSENT while printing
// it in the diagnostic.
var reClockValue = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9-]*):? +(-?\d+)`)

// clockValue pulls the integer that follows a marker, from either the guest's
// `MARKER n` lines or the host's `MARKER: n` ones.
func clockValue(t *testing.T, out, marker string) int64 {
	t.Helper()
	for _, m := range reClockValue.FindAllStringSubmatch(out, -1) {
		if m[1] != marker {
			continue
		}
		v, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatalf("%s: unparseable value %q: %v", marker, m[2], err)
		}
		return v
	}
	t.Fatalf("no %s line in the output:\n%s", marker, out)
	return 0
}
