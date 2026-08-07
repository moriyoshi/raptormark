package builder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A nil *timing is the documented "recording disabled" state, and the pipeline
// relies on it: TranslateOne.tm is nil until Run sets it, and every helper
// (translateone_test.go's argument tests among them) calls through without one.
// step must still run the work and hand back its error untouched.
func TestNilTimingRunsWorkAndPropagatesError(t *testing.T) {
	var tm *timing
	want := errors.New("boom")
	ran := false

	got := tm.step("phase", "", "", func() error {
		ran = true
		return want
	})

	if !ran {
		t.Fatal("step on a nil *timing did not run the work")
	}
	if !errors.Is(got, want) {
		t.Fatalf("step returned %v, want %v", got, want)
	}
	tm.part("p0", 1, time.Second) // must not panic
	if err := tm.write(filepath.Join(t.TempDir(), "x.json")); err != nil {
		t.Fatalf("write on a nil *timing: %v", err)
	}
}

// A translation that dies 40 minutes in is exactly the one whose breakdown is
// wanted, so a failing step must still be recorded — and its error must reach
// the caller unchanged.
func TestFailedStepIsStillRecorded(t *testing.T) {
	tm := newTiming("mod", "/in/elf", "ecvisor")
	want := errors.New("llvm-split exploded")

	if got := tm.step("llvm-split", "", "", func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("step returned %v, want %v", got, want)
	}

	if n := len(tm.report.Phases); n != 1 {
		t.Fatalf("recorded %d phases, want 1", n)
	}
	if tm.report.Phases[0].Name != "llvm-split" {
		t.Fatalf("recorded phase %q, want llvm-split", tm.report.Phases[0].Name)
	}
}

// step records the sizes of the files it is told about, which is what makes the
// four whole-module passes comparable (cost per byte of bitcode, not cost
// alone). A path that does not exist reports 0 rather than failing: several
// steps are recorded before their output exists.
func TestStepRecordsFileSizes(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.bc")
	out := filepath.Join(dir, "out.bc")
	if err := os.WriteFile(in, make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}

	tm := newTiming("mod", "/in/elf", "ecvisor")
	err := tm.step("opt", in, out, func() error {
		return os.WriteFile(out, make([]byte, 99), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := tm.report.Phases[0]
	if rec.InBytes != 1234 {
		t.Errorf("InBytes = %d, want 1234", rec.InBytes)
	}
	if rec.OutBytes != 99 {
		t.Errorf("OutBytes = %d, want 99", rec.OutBytes)
	}

	tm2 := newTiming("mod", "/in/elf", "ecvisor")
	_ = tm2.step("missing", filepath.Join(dir, "nope"), "", func() error { return nil })
	if got := tm2.report.Phases[0].InBytes; got != 0 {
		t.Errorf("absent file reported %d bytes, want 0", got)
	}
}

// The tail is the finding — on postgres one ~17 MiB partition took 17 minutes on
// its own — so the report must arrive sorted slowest-first rather than in the
// order the concurrent workers happened to finish.
func TestWriteSortsPartitionsSlowestFirst(t *testing.T) {
	tm := newTiming("mod", "/in/elf", "ecvisor")
	tm.part("p03", 10, 2*time.Second)
	tm.part("p17", 20, 17*time.Second)
	tm.part("p01", 30, 5*time.Second)

	path := filepath.Join(t.TempDir(), "t.json")
	if err := tm.write(path); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got timingReport
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	want := []string{"p17", "p01", "p03"}
	if len(got.Parts) != len(want) {
		t.Fatalf("got %d parts, want %d", len(got.Parts), len(want))
	}
	for i, name := range want {
		if got.Parts[i].Name != name {
			t.Errorf("parts[%d] = %q, want %q (order: %v)", i, got.Parts[i].Name, name, got.Parts)
		}
	}
	if got.Parts[0].Seconds != 17 {
		t.Errorf("slowest part recorded %v s, want 17", got.Parts[0].Seconds)
	}
	if got.ModuleID != "mod" || got.Runtime != "ecvisor" {
		t.Errorf("report identity = %q/%q, want mod/ecvisor", got.ModuleID, got.Runtime)
	}
	if got.NumCPU < 1 {
		t.Errorf("NumCPU = %d, want >= 1", got.NumCPU)
	}
}
