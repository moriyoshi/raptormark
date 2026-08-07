package e2e

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The guard for `RAPTORMARK_ECV_UNDEC_CENSUS`, the undecoded-instruction census
// in `runtime/src/diag.rs` and the `__ecv_warning` branch in
// `runtime/src/intrinsics.rs`.
//
// # What the runtime actually does, read off the code
//
// `__ecv_warning` decides its disposition BEFORE it posts anything:
//
//   - `diag::undecoded_disposition()` is `Undecoded::Census` AND
//     `EcvContext::delivers_to_handler(SIGILL)` is false: note the address in a
//     process-wide `CensusTable` deduped by unique ADDRESS and capped at
//     `UNDEC_CENSUS_CAP`, log `[undec_census] addr=0x... SKIPPED-UNSOUND` on the
//     FIRST occurrence only, and RETURN -- which resumes at the next
//     instruction, because `TraceLifter`'s `kCategoryInvalid` arm emits the
//     `__ecv_warning` call followed by a plain branch to the next block. No
//     signal is posted at all.
//   - otherwise: post a thread-directed SIGILL and call
//     `deliver_pending_signals`. When a handler took it, nothing else happens --
//     that is the postgres CRC32C probe path, and the `handlerDefault` /
//     `handlerCensus` subtests below are its only e2e coverage (`crc32_test.go`
//     reads as if it covered it, but `patches/0027` taught the lifter `crc32c*`,
//     so that guest has not reached `__ecv_warning` since). When the count comes
//     back 0 it is `fatal!` with `diag::undecoded_message(addr)`, i.e.
//     `[ecvisor] fatal: undecoded instruction at 0x...`, then
//     `std::process::exit(1)`.
//
// The banner is printed by `diag::set_undec_census(true)`, from
// `sys::init_diag_flags`, at startup -- not from the first skipped site. So an
// armed run that skips nothing still says it is unsound.
//
// # ⚠️ WHY THE ORDER OF THOSE TWO TESTS IS THE POINT
//
// This test previously asserted the OPPOSITE of what it asserts now, because
// the census used to be decided after the post and an armed run died at the
// guest's next syscall. `deliver_pending_signals` ends with
//
//	if let Some(sig) = self.pending_termination() { self.arm_signal_exit(sig); }
//
// so before it ever returned 0, SIGILL's default action had already set
// `Pending::Exit(128 + SIGILL)` and `suspended = true`. The census arm consumed
// the pending signal BIT and returned, and nothing cleared the armed exit; the
// svc trampoline picked `suspended` up at the next syscall, that syscall
// completed, the leg unwound, and the module exited 132 with no diagnostic.
// `arm_signal_exit`'s own doc called that "normally unreachable rubble behind a
// process that is already dying", which was true while `fatal!` was the only
// outcome -- the census arm is what reached it.
//
// The consequence was that ONE armed run enumerated the sites executed between
// the first skip and the next syscall. On a real workload, which syscalls
// constantly, that is very close to one site per run -- and a list of one that
// says nothing about being truncated reads as a COMPLETE list, which is the
// exact false completeness this instrument exists to prevent.
//
// # Why this test exists
//
// The mode is deliberately UNSOUND: a skipped instruction never applies its
// effect, so everything downstream of it is garbage. Four properties have to be
// nailed down, and they are in increasing order of what they cost to get wrong:
//
//  1. It FIRES: the site is logged, once, and the guest steps over it.
//  2. It SPANS A SYSCALL: two DIFFERENT undecoded sites, separated by a `write`,
//     are both reported by ONE run, which is the property the armed-exit defect
//     silently removed and which a single-site test cannot see at all.
//  3. It STAYS OFF: with the variable unset, an undecoded instruction is still
//     the loud `fatal!` it has always been. A census that defaulted on would
//     silently convert this runtime's one honest failure into wrong answers, on
//     every workload, with nobody having asked for it.
//  4. It KEEPS ITS HANDS OFF A GUEST THAT CATCHES SIGILL. A guest that installs
//     a handler receives the signal identically with the census on and off, and
//     no census line is written for a site it handled. PostgreSQL decides
//     whether its CPU has ARMv8 CRC32C by catching exactly this signal, so
//     swallowing it is not a lost diagnostic -- it is a wrong answer about the
//     hardware, on census runs only.
//
// # Why these two instructions
//
// `0xd5033fdf` (`isb sy`) and `0x6e3b2fff` (`uqsub v31.16b, v31.16b, v27.16b`).
// Both are listed as undecoded in `.agents/docs/TODO.md` under
// `## Instruction coverage after patches 0063/0064` -- `isb` inside postgres's
// `perform_spin_delay`, `uqsub` inside `json_lex` -- and `patches/0065` adds
// only scalar-SISD add/sub, so neither is decoded on
// `raptormark-elfconv-base-patched:sisd0065`.
//
// ⚠️ That provenance is NOT the evidence. Each is proved undecoded by a subtest
// that runs the module with the census UNSET and requires it to die naming that
// address, so a decoder that learns either one fails this test rather than
// quietly draining it of meaning. `defaultSecond` exists for no other reason:
// with the census off the guest dies at the FIRST site, so the second site
// needs its own run that never reaches the first.
//
// Both are harmless to skip, which is what lets the census arm assert a CLEAN
// exit all the way to the last marker rather than merely "it got further".
// `isb` has no destination at all. `uqsub` writes `v31` and reads `v27`, both
// named in the asm block's clobber list, so the compiler holds no live value in
// either and nothing downstream reads what the skipped instruction failed to
// write. An instruction whose destination the guest went on to USE would leave
// it computing on stale values and the markers after it would prove nothing.
const (
	isbEncoding   = 0xd5033fdf
	uqsubEncoding = 0x6e3b2fff
)

// undecCensusIters is how many times each static site executes. The dedupe
// claim is "reported once per unique ADDRESS, however often it runs", and a loop
// is the only thing that can distinguish that from "reported once because it ran
// once".
const undecCensusIters = 64

// Both loops are written in ASM, not in C, and that is load-bearing twice over.
//
// A C `for` around an `asm volatile` can be unrolled, which would put the
// `.inst` at several distinct addresses -- and several addresses is exactly what
// the census is entitled to report several times, so an unrolled loop would turn
// the dedupe assertion green by removing the thing it tests. One `.inst` inside
// one asm block is one static site by construction, and
// `soleEncodingAddrInSymbol` re-checks that against the built ELF rather than
// trusting the reasoning.
//
// The `add` on `done` sits immediately AFTER each `.inst`, inside the same
// block, so `iters` is not a count of loop entries -- it counts the times
// control arrived at the instruction after the skipped one. That is the
// independent evidence that the site executed `undecCensusIters` times; without
// it, "one census line" is equally consistent with a loop that ran once.
//
// ⚠️ THE `printf` BETWEEN THE TWO LOOPS IS A TEST FIXTURE, not decoration. It is
// the syscall that the armed-exit defect used to end the run at: the first
// site's skip armed `Pending::Exit(132)`, this `write` completed, and the leg
// unwound on its way out -- so `UNDEC-AFTER` printed and nothing after it ever
// did. The second site is on the far side of it, which is what makes "one run
// enumerates every site the workload executes" a claim this test can check.
//
// `argc` selects which sites run, because ecvisor hands a module's wasmedge
// arguments to the guest as argv when there is no boot record (`entry.rs`) and
// passes NO environment through to it. ONE extra argument means "skip the isb",
// which is how `defaultSecond` reaches the uqsub with the census off; TWO means
// "install a SIGILL handler and take one site under it", which is the case the
// census must leave completely alone.
//
// ⚠️ The handler mode does nothing on real hardware -- `isb` is a perfectly
// legal instruction there, so a native run of this guest reports `caught=0`.
// It is only under a lifter that lacks the decoder that it becomes SIGILL, which
// is exactly the situation PostgreSQL's CRC32C probe is written for.
const undecCensusGuestSrc = `#include <signal.h>
#include <stdio.h>

static volatile sig_atomic_t caught = 0;
static void on_sigill(int s) { (void)s; caught++; }

/* Executes the isb at ONE address n times, returning how many times control
   reached the instruction after it. noinline so the symbol survives -O2 and the
   test can bound its search for the encoding. */
__attribute__((noinline)) unsigned long isb_loop(unsigned long n) {
	unsigned long done = 0;
	__asm__ volatile(
		"1:\n\t"
		".inst 0xd5033fdf\n\t"   /* isb sy -- undecoded through patches/0065 */
		"add %[done], %[done], #1\n\t"
		"subs %[n], %[n], #1\n\t"
		"b.ne 1b\n\t"
		: [done] "+r"(done), [n] "+r"(n)
		:
		: "cc", "memory");
	return done;
}

/* The same, for a DIFFERENT undecoded instruction. v27/v31 are clobbered, so no
   live value depends on the write this instruction does not perform. */
__attribute__((noinline)) unsigned long uqsub_loop(unsigned long n) {
	unsigned long done = 0;
	__asm__ volatile(
		"1:\n\t"
		".inst 0x6e3b2fff\n\t"   /* uqsub v31.16b, v31.16b, v27.16b */
		"add %[done], %[done], #1\n\t"
		"subs %[n], %[n], #1\n\t"
		"b.ne 1b\n\t"
		: [done] "+r"(done), [n] "+r"(n)
		:
		: "v27", "v31", "cc", "memory");
	return done;
}

int main(int argc, char **argv) {
	(void)argv;
	printf("UNDEC-BEFORE\n");
	fflush(stdout);
	if (argc > 2) {
		/* TWO extra arguments: install a SIGILL handler and take exactly one
		   undecoded site under it. This is the path the census must NOT take. */
		signal(SIGILL, on_sigill);
		unsigned long h = isb_loop(1);
		printf("UNDEC-HANDLER caught=%d stepped=%lu\n", (int)caught, h);
		fflush(stdout);
		return 0;
	}
	if (argc < 2) {
		unsigned long i1 = isb_loop(ITERS);
		/* THE SYSCALL BETWEEN THE TWO SITES. */
		printf("UNDEC-AFTER iters=%lu\n", i1);
		fflush(stdout);
	}
	unsigned long i2 = uqsub_loop(ITERS);
	printf("UNDEC-AFTER2 iters2=%lu\n", i2);
	fflush(stdout);
	printf("UNDEC-AFTER3\n");
	fflush(stdout);
	return 0;
}
`

// TestUndecodedCensusFiresAndStaysOff runs ONE module three times, changing only
// the environment and the argument list. Every arm shares the lift, so nothing
// but those can account for a difference between them.
//
// Gated on `RAPTORMARK_E2E` alone, with no second opt-in. It compiles a
// ~700 KB static guest, lifts it once (seconds, and cached), and runs it three
// times; the slow arms in this suite are the ~30-minute fused lifts and this is
// nowhere near them. Gating it further would keep the default suite green while
// the property it guards -- "the census is OFF unless asked" -- is the one this
// runtime cannot afford to get wrong quietly.
//
// Neutralized against `raptormark-builder:census`, the same pipeline with the
// pre-fix `libecvisor.a` (identical `raptormark.base_id` and
// `raptormark.translate_sh`, so the object cache serves every arm the same
// translated object; only `libecvisor.a` differs). See the comment on the
// census subtest.
func TestUndecodedCensusFiresAndStaysOff(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	// Substitution, not fmt.Sprintf: the guest's inline asm is full of `%[done]`
	// and `%[n]` operand names, which Sprintf reads as explicit argument
	// indices and mangles.
	src := strings.ReplaceAll(undecCensusGuestSrc, "ITERS", strconv.Itoa(undecCensusIters))
	elfPath := compileGuest(t, ctx, dir, "undeccensus", src)

	// The addresses the runtime must name. Taken from the ELF, so every arm is
	// asserted against values no run produced -- a test that scraped an address
	// out of the module's own output would agree with any address at all,
	// including a wrong one.
	addr1 := soleEncodingAddrInSymbol(t, elfPath, "isb_loop", isbEncoding)
	addr2 := soleEncodingAddrInSymbol(t, elfPath, "uqsub_loop", uqsubEncoding)
	t.Logf("the guest's only isb (%#08x) is at %#x; its only uqsub (%#08x) is at %#x",
		isbEncoding, addr1, uqsubEncoding, addr2)
	if addr1 == addr2 {
		t.Fatalf("both sites resolved to %#x, so the two-site claim below is one "+
			"site asserted twice", addr1)
	}

	wasm := liftOne(t, ctx, img, dir, elfPath, "undeccensus")

	censusLine := func(a uint64) string { return fmt.Sprintf("addr=%#x SKIPPED-UNSOUND", a) }
	fatalLine := func(a uint64) string { return fmt.Sprintf("undecoded instruction at %#x", a) }

	// --- The default. Nothing set; the FIRST instruction is fatal. -----------
	//
	// This arm is also the PROOF that `isb` is genuinely undecoded on this
	// builder. If the decoder ever learns it, the guest runs past it and this
	// arm fails -- which is the correct outcome, because the census arm below
	// would then be measuring one site fewer and must not stay green.
	t.Run("default", func(t *testing.T) {
		out, err := runWasmAllowingFailure(t, ctx, wasm, nil, nil)
		if err == nil {
			t.Fatalf("the module exited 0 with the census UNSET. Either the "+
				"census defaulted ON -- the one failure this whole feature must "+
				"not have -- or %#08x is now DECODED on this builder and this "+
				"guest no longer reaches an undecoded site. Check which before "+
				"changing anything:\n%s", isbEncoding, out)
		}
		if !strings.Contains(out, fatalLine(addr1)) {
			t.Errorf("the fatal message does not name %#x.\nwant substring: %s\ngot:\n%s",
				addr1, fatalLine(addr1), out)
		}
		// The wording distinguishes a missing INSTRUCTION from a missing basic
		// block, which is the distinction that cost an hour on 2026-08-19. A
		// module that died for any other reason would fail the check above but
		// could still be mistaken for this one.
		if !strings.Contains(out, "not a missing basic block") {
			t.Errorf("the module died, but not with diag::undecoded_message:\n%s", out)
		}
		// It must die AT the instruction, not after it.
		if !strings.Contains(out, "UNDEC-BEFORE") {
			t.Errorf("the guest never reached the marker before the isb, so it "+
				"died somewhere else entirely:\n%s", out)
		}
		if strings.Contains(out, "UNDEC-AFTER") {
			t.Errorf("the guest ran PAST the undecoded instruction with the "+
				"census unset:\n%s", out)
		}
		// Not one word of the census may appear on a run that did not ask for
		// it -- neither the banner nor a site line.
		if strings.Contains(out, "[undec_census]") {
			t.Errorf("a run with the census unset emitted census output:\n%s", out)
		}
	})

	// --- The same proof for the SECOND instruction. -------------------------
	//
	// One extra argument makes the guest skip `isb_loop` entirely, so this run
	// reaches the uqsub with the census still unset and must die naming it. It
	// exists because the arm above cannot say anything about the second site:
	// with the census off the guest never gets past the first.
	//
	// Without this, "the census reported two addresses" would be satisfied just
	// as well by a decoder that had learned `uqsub` -- there would simply be one
	// line instead of two, and the failure would read as a census bug.
	t.Run("defaultSecond", func(t *testing.T) {
		out, err := runWasmAllowingFailure(t, ctx, wasm, nil, []string{"skip-isb"})
		if err == nil {
			t.Fatalf("the module exited 0 with the census UNSET and the isb "+
				"skipped, so %#08x is DECODED on this builder and the two-site "+
				"assertion in the census arm is measuring one site:\n%s",
				uqsubEncoding, out)
		}
		if !strings.Contains(out, fatalLine(addr2)) {
			t.Errorf("the fatal message does not name %#x.\nwant substring: %s\ngot:\n%s",
				addr2, fatalLine(addr2), out)
		}
		// The argument really did steer the guest past the first site: the
		// syscall marker between the two loops must be absent, or this run died
		// at the isb after all and says nothing about the uqsub.
		if strings.Contains(out, "UNDEC-AFTER iters=") {
			t.Errorf("the guest ran isb_loop despite the extra argument, so this "+
				"arm did not reach %#x at all:\n%s", addr2, out)
		}
		if strings.Contains(out, fatalLine(addr1)) {
			t.Errorf("the guest died at the isb, not at the uqsub:\n%s", out)
		}
		if strings.Contains(out, "UNDEC-AFTER2") {
			t.Errorf("the guest ran PAST the uqsub with the census unset:\n%s", out)
		}
	})

	// --- A guest that CATCHES SIGILL, with and without the census. -----------
	//
	// ⚠️ THE BEHAVIOUR THE FIX MUST NOT CHANGE, and the only e2e coverage of it.
	// `e2e/crc32_test.go` reads as if it covered this and no longer does:
	// `patches/0027` taught the lifter `crc32c*`, so that guest has executed a
	// DECODED instruction ever since and never reaches `__ecv_warning` at all.
	//
	// PostgreSQL decides whether its CPU has ARMv8 CRC32C by executing the
	// instruction under a SIGILL handler that `siglongjmp`s out. If the census
	// swallowed that signal, postgres would not get a diagnostic it could live
	// without -- it would get a WRONG ANSWER about its own hardware, silently,
	// and only on census runs. `__ecv_warning` asks `delivers_to_handler` before
	// it decides anything, precisely so this path is identical either way.
	//
	// The handler here RETURNS rather than longjmping, which is the weaker of the
	// two shapes: it proves the signal was delivered and the guest's own code ran
	// on it. The longjmp variant additionally exercises `run_signal_handler`'s
	// nested-call model against a `sigsetjmp` frame, and is not covered here.
	//
	// `caught=1` is the whole assertion. `stepped=1` alongside it says the guest
	// also carried on afterwards, so a handler that ran and then wedged the
	// process would not read as a pass.
	for _, arm := range []struct {
		name string
		env  []string
	}{
		{"handlerDefault", nil},
		{"handlerCensus", []string{"RAPTORMARK_ECV_UNDEC_CENSUS=1"}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			out, err := runWasmAllowingFailure(t, ctx, wasm, arm.env,
				[]string{"skip-isb", "handler"})
			if err != nil {
				t.Fatalf("a guest that installs a SIGILL handler must survive its "+
					"own undecoded instruction, got %v:\n%s", err, out)
			}
			if !strings.Contains(out, "UNDEC-HANDLER caught=1 stepped=1") {
				t.Errorf("the guest's SIGILL handler did not run exactly once for "+
					"its one undecoded site.\nwant substring: UNDEC-HANDLER "+
					"caught=1 stepped=1\ngot:\n%s", out)
			}
			// Neither arm may take the fatal path: something handled it.
			if strings.Contains(out, "undecoded instruction at") {
				t.Errorf("the fatal path ran for a site the guest caught:\n%s", out)
			}
			// ⚠️ And the census must not have EATEN it. A census line for the
			// isb address here would mean `__ecv_warning` took the record-and-skip arm
			// for a site that has a handler -- which is the failure that would
			// hand postgres a wrong answer about its own CPU.
			if strings.Contains(out, censusLine(addr1)) {
				t.Errorf("the census recorded %#x although the guest has a SIGILL "+
					"handler for it. The census arm must be reachable ONLY when "+
					"`delivers_to_handler` is false:\n%s", addr1, out)
			}
		})
	}

	// --- Armed. Both sites are recorded once and the guest runs to the end. --
	//
	// ⚠️ THE NEUTRALIZATION. On `raptormark-builder:census` -- the same pipeline
	// with the pre-fix `libecvisor.a`, identical `raptormark.base_id` and
	// `raptormark.translate_sh` -- this subtest fails and the two `default` arms
	// still pass. There the armed run prints the banner, one census line for the
	// isb address, `UNDEC-AFTER iters=64`, and then exits 132 without ever reaching
	// `UNDEC-AFTER2`: `deliver_pending_signals` armed SIGILL's default action
	// before the census branch ran, and the leg unwound at the first syscall
	// past the skip. Steps 1, 2, 7 and 8 below all fail there. It is not a
	// compile error and it is not a skip; the assertions observe a behaviour
	// that is simply absent.
	t.Run("census", func(t *testing.T) {
		out, err := runWasmAllowingFailure(t, ctx, wasm,
			[]string{"RAPTORMARK_ECV_UNDEC_CENSUS=1"}, nil)
		// Logged whole, not only on failure: this IS the instrument's product,
		// and a passing run under `-v` is where a reader sees what a census
		// actually looks like before pointing it at postgres.
		t.Logf("the armed run:\n%s", out)

		// 1. It ran PAST both instructions, all the way to the last marker --
		//    observed, not inferred. `UNDEC-AFTER2` is the one that used to be
		//    missing: it sits on the far side of the `write` that the armed
		//    SIGILL exit ended the run at.
		wantIters := fmt.Sprintf("UNDEC-AFTER iters=%d", undecCensusIters)
		wantIters2 := fmt.Sprintf("UNDEC-AFTER2 iters2=%d", undecCensusIters)
		for _, want := range []string{"UNDEC-BEFORE", wantIters, wantIters2, "UNDEC-AFTER3"} {
			if !strings.Contains(out, want) {
				t.Errorf("the armed run did not reach %q. An armed run must survive "+
					"every skip and every syscall between them; ending early is the "+
					"defect that made this census report one site per run.\ngot:\n%s",
					want, out)
			}
		}

		// 2. BOTH addresses are reported, and they are the ones in the ELF.
		//    This is the two-site claim: they are different instructions in
		//    different functions with a `write` syscall between them.
		for _, a := range []uint64{addr1, addr2} {
			if !strings.Contains(out, censusLine(a)) {
				t.Errorf("no census line for %#x.\nwant substring: %s\ngot:\n%s",
					a, censusLine(a), out)
			}
		}

		// 3. Each ONCE, though each executed undecCensusIters times. `wantIters`
		//    above is what makes this a dedupe assertion rather than an
		//    arithmetic identity: without it, one line is equally consistent
		//    with one execution.
		for _, a := range []uint64{addr1, addr2} {
			if n := strings.Count(out, fmt.Sprintf("addr=%#x", a)); n != 1 {
				t.Errorf("%#x reported %d times after %d executions, want exactly 1 "+
					"-- CensusTable::note is not deduping by address:\n%s",
					a, n, undecCensusIters, out)
			}
		}

		// 4. ...and nothing else was reported. The guest executes exactly two
		//    undecoded sites, so a third line means either glibc reached one on
		//    a path this test does not model -- in which case the count below is
		//    the only warning -- or the dedupe is leaking.
		if n := strings.Count(out, "SKIPPED-UNSOUND"); n != 2 {
			t.Errorf("%d census lines, want exactly 2 (%#x and %#x):\n%s",
				n, addr1, addr2, out)
		}

		// 5. The run says it is unsound, loudly and at STARTUP -- from
		//    `set_undec_census`, not from the first skipped site. That is what
		//    reaches the reader of an armed run that skipped nothing at all and
		//    came out looking clean, who is the reader most likely to believe
		//    its results.
		for _, want := range []string{
			"UNDECODED-INSTRUCTION CENSUS IS ON (RAPTORMARK_ECV_UNDEC_CENSUS)",
			"THIS RUN IS UNSOUND",
			"Trust the addr= lines. Nothing else.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the unsoundness banner is missing %q:\n%s", want, out)
			}
		}
		// Every banner line must carry the prefix, or a `grep '\[undec_census\]'`
		// that collects the site list drops half the warning that came with it.
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "UNSOUND") && !strings.HasPrefix(line, "[undec_census]") {
				t.Errorf("a census line is not prefixed and would be lost to a "+
					"prefix grep: %q", line)
			}
		}

		// 6. The list is complete: nothing was clipped by UNDEC_CENSUS_CAP.
		//    A truncated census that the reader takes for a complete one is the
		//    exact failure this instrument exists to avoid.
		if strings.Contains(out, "census TRUNCATED") {
			t.Errorf("the cap clipped a 2-site run, which should be impossible:\n%s", out)
		}

		// 7. It did NOT take the fatal path...
		if strings.Contains(out, "undecoded instruction at") {
			t.Errorf("the fatal path ran with the census armed:\n%s", out)
		}

		// 8. ...and the run ended CLEANLY. ⚠️ This is the assertion that was
		//    inverted: it used to require `exit status 132`, because
		//    `deliver_pending_signals` armed SIGILL's default action before the
		//    census branch could return and nothing cleared it. `__ecv_warning`
		//    now decides its disposition BEFORE posting, so in census mode with
		//    no SIGILL handler no signal is posted and no default action is ever
		//    armed.
		//
		//    Both halves are needed and neither alone would do. A clean exit
		//    with `UNDEC-AFTER3` missing would be some other early death that
		//    happened to return 0; `UNDEC-AFTER3` with a non-zero status would
		//    be a guest that printed everything and died on the way out.
		if err != nil {
			t.Errorf("the armed run exited with %v, want a clean exit.\n"+
				"exit 132 (= 128 + SIGILL): the armed SIGILL default action is back "+
				"-- something posts SIGILL before deciding the disposition, or the "+
				"census arm sits below `deliver_pending_signals` again.\n"+
				"exit 1 with the fatal above: this runtime has no census at all.\n"+
				"got:\n%s", err, out)
		}
	})
}

// runWasmAllowingFailure is `runWasm` plus the process error and a guest
// argument list, for the two things `runWasm` cannot express: a run that is
// REQUIRED to die, and a run that has to steer the guest down a different path.
//
// It keeps `runWasm`'s `--env` discipline, because wasmedge does not inherit the
// host environment and a variable that is not passed here simply does not reach
// the guest -- which, for this test, would look exactly like the census failing
// to fire. `args` land AFTER the module path, which is where wasmedge stops
// reading its own flags and starts building the module's argv; ecvisor hands
// that argv to the guest when there is no boot record (`entry.rs`).
func runWasmAllowingFailure(t *testing.T, ctx context.Context, wasmPath string, env, args []string) (string, error) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(wasmPath))
	if err != nil {
		t.Fatal(err)
	}
	cmd := "wasmedge --enable-all"
	// Before the module path: wasmedge stops reading its own flags there.
	for _, e := range append(wasmEdgeEnv(), env...) {
		cmd += " --env " + e
	}
	cmd += " /out/" + filepath.Base(wasmPath)
	for _, a := range args {
		cmd += " " + a
	}
	return dockerRun(ctx, []string{"-v", dir + ":/out"}, cmd)
}

// soleEncodingAddrInSymbol returns the virtual address of the ONE occurrence of
// instruction word `want` inside `sym`, and fails if there is any other number
// of them.
//
// Bounded by the symbol rather than by the whole file on purpose: a static glibc
// contains its own barriers, so a file-wide search would find some other
// program's `isb` and the test would then assert against an address the guest
// never executes. And "exactly one" is not fussiness -- it is what bounds the
// dedupe claim. Several static sites are several addresses, which the census is
// entitled to report several times, so a duplicated site would make the
// once-only assertion pass for the wrong reason.
func soleEncodingAddrInSymbol(t *testing.T, path, sym string, want uint32) uint64 {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("reading symbols from %s: %v", path, err)
	}
	var target *elf.Symbol
	for i := range syms {
		if syms[i].Name == sym && syms[i].Size > 0 {
			target = &syms[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("no sized symbol %q in %s -- the guest was compiled differently "+
			"than this test assumes", sym, path)
	}
	var body []byte
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_EXECINSTR == 0 || s.Type == elf.SHT_NOBITS || s.Addr == 0 {
			continue
		}
		if target.Value < s.Addr || target.Value+target.Size > s.Addr+s.Size {
			continue
		}
		data, err := s.Data()
		if err != nil {
			t.Fatalf("reading %s: %v", s.Name, err)
		}
		off := target.Value - s.Addr
		body = data[off : off+target.Size]
		break
	}
	if body == nil {
		t.Fatalf("no executable section covers %s (%#x..%#x)",
			sym, target.Value, target.Value+target.Size)
	}
	var at []uint64
	for i := 0; i+4 <= len(body); i += 4 {
		if binary.LittleEndian.Uint32(body[i:]) == want {
			at = append(at, target.Value+uint64(i))
		}
	}
	if len(at) != 1 {
		t.Fatalf("found %d occurrences of %#08x in %s (%#x), want exactly 1 -- "+
			"the once-per-address assertion needs ONE static site", len(at), want, sym, at)
	}
	return at[0]
}
