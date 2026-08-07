package builder

// Timing instrumentation for the translation pipeline.
//
// Translation is ~99% of build cost — 45m12s for a fused postgres — and until
// now it was one opaque number. run() records nothing, internal/translate keeps
// the container's stdout only on failure, and splitAndCompile deletes the split
// directory on success, so the per-partition evidence was destroyed too. Every
// cost figure in .agents/docs was obtained externally, by timestamping a wrapper
// or watching ps.
//
// This records the breakdown in-process and writes it beside the object, where
// the host can pick it up.
//
// ❗ NOTHING IN THIS FILE MAY INFLUENCE THE BYTES translate-one EMITS. That is
// why it is deliberately absent from translateSources (toolsid.go): a change
// here does not invalidate the object cache, which is only sound while the file
// stays pure measurement. If timing ever starts feeding codegen — an adaptive
// optimisation level, a scheduling change that reorders partition output — add
// this file to translateSources in the same commit.

import (
	"encoding/json"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
)

// phaseRecord is one sequential step of the pipeline. Sizes are recorded
// alongside the duration because the interesting question for the four
// whole-module steps is cost per byte of bitcode, not cost alone.
type phaseRecord struct {
	Name     string  `json:"name"`
	Seconds  float64 `json:"seconds"`
	InBytes  int64   `json:"in_bytes,omitempty"`
	OutBytes int64   `json:"out_bytes,omitempty"`
}

// partRecord is one bitcode partition's codegen. The tail is what matters: on
// postgres, one ~17 MiB part took 17 minutes on its own, which no amount of
// added parallelism can divide.
type partRecord struct {
	Name    string  `json:"name"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
}

type timingReport struct {
	ModuleID string        `json:"module_id"`
	ELF      string        `json:"elf"`
	Runtime  string        `json:"runtime"`
	NumCPU   int           `json:"num_cpu"`
	Total    float64       `json:"total_seconds"`
	Phases   []phaseRecord `json:"phases"`
	Parts    []partRecord  `json:"parts,omitempty"`
}

// timing accumulates the report. A nil *timing is valid and disables recording,
// so the pipeline functions need no branching of their own.
type timing struct {
	mu     sync.Mutex
	start  time.Time
	report timingReport
}

func newTiming(moduleID, elf, rt string) *timing {
	return &timing{
		start: time.Now(),
		report: timingReport{
			ModuleID: moduleID,
			ELF:      elf,
			Runtime:  rt,
			NumCPU:   runtime.NumCPU(),
		},
	}
}

// step runs fn, recording how long it took and the sizes of the named input and
// output files. Either path may be empty. The error is returned untouched — a
// failed step is still recorded, because a translation that dies after 40
// minutes is exactly the one whose breakdown is wanted.
func (t *timing) step(name, in, out string, fn func() error) error {
	if t == nil {
		return fn()
	}
	started := time.Now()
	err := fn()
	rec := phaseRecord{
		Name:     name,
		Seconds:  time.Since(started).Seconds(),
		InBytes:  fileSize(in),
		OutBytes: fileSize(out),
	}
	t.mu.Lock()
	t.report.Phases = append(t.report.Phases, rec)
	t.mu.Unlock()
	return err
}

// part records one partition's codegen. Called from the concurrent workers, so
// it takes the lock.
func (t *timing) part(name string, bytes int64, d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.report.Parts = append(t.report.Parts, partRecord{
		Name:    name,
		Bytes:   bytes,
		Seconds: d.Seconds(),
	})
	t.mu.Unlock()
}

// write emits the report as JSON. Partitions are sorted slowest-first: the tail
// is the finding, and reading it should not require sorting by hand.
//
// Failure to write is deliberately ignored by callers — losing the measurement
// must never fail a translation that otherwise succeeded.
func (t *timing) write(path string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.report.Total = time.Since(t.start).Seconds()
	sort.Slice(t.report.Parts, func(i, j int) bool {
		return t.report.Parts[i].Seconds > t.report.Parts[j].Seconds
	})
	b, err := json.MarshalIndent(t.report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// fileSize reports a file's size, or 0 when the path is empty or unreadable.
// A missing file is not an error here: several steps are recorded before their
// output exists, and one that failed may have produced nothing at all.
func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
