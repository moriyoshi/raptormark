// Package preserve turns "do not delete this" from a rule into a check.
//
// # Why it exists
//
// Two irreplaceable things were lost in one week and NOTHING REPORTED EITHER.
//
//   - `_recovery/`, the untracked evidence from the 2026-08-01 rebuild, which
//     `AGENTS.md` protected with "kept on disk deliberately. Read it; never
//     clean it up."
//   - `raptormark-tmp-ossldgst:latest`, a pre-wipe fixture three e2e tests
//     depended on.
//
// The instructive part is not that they went. It is that every reference to
// `_recovery/` in code and build config -- `.gitignore`, `.bazelignore`,
// `BUILD.bazel`'s `gazelle:exclude`, a `filepath.SkipDir` arm in
// `internal/builder/workspace_test.go` -- was an EXCLUSION. Each told a tool to
// ignore it; not one required it to exist. Every gate stayed green throughout.
// The fixture was marginally better off: it announced itself as a SKIP, which is
// `0 fail` with coverage silently gone, and nobody reads a skip.
//
// ❗ **A rule in a document is not a guard.** This package is the guard.
//
// # The design problem, and why "check it exists" is the WRONG check
//
// The obvious implementation -- a list of things that must be present, failing
// when one is not -- is wrong, and wrong in a way that would get it deleted
// within a week. On a fresh clone or a new machine NOTHING is present, so it
// would fail for everyone who had lost nothing at all. A check that cries wolf
// is removed, and then there is no check.
//
// What was actually missing is not detection of ABSENCE. It is detection of
// DISAPPEARANCE, and those differ by a baseline:
//
//	recorded and present    -> fine
//	recorded and MISSING    -> ❗ this is the event nothing reported
//	not recorded            -> nothing is known; say so, do not guess
//
// So a manifest is written when things are known good (`snapshot`), and `check`
// compares against it. Never recorded means never claimed, and the check says
// "nothing recorded" rather than inventing an alarm or a false all-clear.
//
// # ⚠️ The manifest must outlive what it guards
//
// It is written to `.agents/preserve.json`, inside the repository and intended
// to be COMMITTED. A manifest kept beside the things it protects would vanish
// with them, which is how the `_recovery/` references failed -- they were all in
// files that would have been perfectly happy in its absence.
//
// # What this cannot do
//
// ❗ **It cannot detect its own loss, and must not pretend to.** Recording
// `.agents/preserve.json` as an entry looks appealing and is VACUOUS: if the
// manifest is deleted, `Load` reports not-recorded and `check` prints "nothing
// recorded" -- the entry that would have complained went with the file. That
// self-entry was added on 2026-08-25 and removed the same hour, once the
// question "what would this actually fire on?" was asked of it.
// The manifest's own protection is that it is COMMITTED: `git status` shows its
// deletion, which is a guard living outside the thing it guards. That is the
// same property the manifest gives everything else, applied one level up.
//
// It reports disappearance; it cannot undo one. It also cannot tell a
// deliberate deletion from an accident, and deliberately does not try: the
// answer to "I removed that on purpose" is to re-run `snapshot`, which is a
// visible edit to a committed file that a reviewer can see.
package preserve

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestPath is where the record lives, relative to the repository root.
//
// ⚠️ Under `.agents/` rather than `.agents-workspace/` ON PURPOSE.
// `.agents-workspace/` is documented as DISPOSABLE and gets wiped without
// warning -- it is one of the things this package guards. A manifest kept there
// would be destroyed by exactly the event it exists to detect.
const ManifestPath = ".agents/preserve.json"

// Kind distinguishes what a record names, because the two are checked by
// completely different means and only one of them is on the filesystem.
type Kind string

const (
	// KindPath is a file or directory in the working tree.
	KindPath Kind = "path"
	// KindImage is a local Docker image, by `repository:tag`.
	KindImage Kind = "image"
)

// Entry is one thing worth noticing the loss of.
type Entry struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	// Note says why this is irreplaceable, in the recorder's own words. It is
	// carried so a `check` failure can explain the stakes at the moment someone
	// is looking at it -- "raptormark-elfconv-base-patched:wasix is missing" is
	// a fact, and "...and it cannot be rebuilt from this tree" is the reason to
	// stop what you are doing.
	Note string `json:"note,omitempty"`
	// ID is the Docker image id at snapshot time, for KindImage. A tag that
	// still exists but now points at a DIFFERENT image is not a loss the way a
	// missing tag is, but it is worth reporting: it means the name no longer
	// refers to what was recorded.
	ID string `json:"id,omitempty"`
}

// Manifest is the recorded baseline.
type Manifest struct {
	// Recorded is set by the caller rather than read from the clock, because
	// this package has to stay testable and a timestamp is not what any
	// decision here turns on.
	Recorded string  `json:"recorded"`
	Entries  []Entry `json:"entries"`
}

// Status is what `check` found for one entry.
type Status struct {
	Entry
	// Present is whether the thing is there at all. This is the field that
	// matters; everything else is detail.
	Present bool
	// Changed is set when a KindImage tag resolves to a different id than was
	// recorded. Present stays true: the name exists, it just no longer names
	// what it did.
	Changed bool
	// Now is the current id for a KindImage, when it differs.
	Now string
}

// Load reads the manifest. A missing file is NOT an error: it means nothing has
// been recorded, which is a legitimate state with a correct answer ("nothing is
// known"), and conflating it with a broken manifest would make the first run on
// any machine look like a failure.
func Load(root string) (*Manifest, bool, error) {
	b, err := os.ReadFile(filepath.Join(root, ManifestPath))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		// A manifest that does not parse IS an error, and loudly: it is the one
		// state where something was recorded and cannot be read, so answering
		// "nothing is known" would silently discard a real baseline.
		return nil, false, fmt.Errorf("preserve: %s is unreadable: %w", ManifestPath, err)
	}
	return &m, true, nil
}

// Save writes the manifest, sorted so a re-snapshot produces a reviewable diff
// rather than a reordering.
func Save(root string, m *Manifest) error {
	sort.Slice(m.Entries, func(i, j int) bool {
		if m.Entries[i].Kind != m.Entries[j].Kind {
			return m.Entries[i].Kind < m.Entries[j].Kind
		}
		return m.Entries[i].Name < m.Entries[j].Name
	})
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p := filepath.Join(root, ManifestPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// Check resolves every entry against the current machine.
//
// `imageID` resolves a Docker image name to its id, returning "" when the image
// is absent. It is a parameter so the whole of this package is testable without
// Docker -- `internal/preserve` must stay on the default `go test ./...` path,
// which `AGENTS.md` requires to run without Docker, root or network.
func Check(root string, m *Manifest, imageID func(string) string) []Status {
	out := make([]Status, 0, len(m.Entries))
	for _, e := range m.Entries {
		s := Status{Entry: e}
		switch e.Kind {
		case KindPath:
			_, err := os.Stat(filepath.Join(root, e.Name))
			s.Present = err == nil
		case KindImage:
			id := imageID(e.Name)
			s.Present = id != ""
			if s.Present && e.ID != "" && id != e.ID {
				s.Changed = true
				s.Now = id
			}
		default:
			// An unknown kind is reported as MISSING rather than skipped. A
			// manifest written by a newer version naming a kind this binary
			// cannot check must not read as an all-clear.
			s.Present = false
		}
		out = append(out, s)
	}
	return out
}

// Missing returns the statuses that represent a loss.
func Missing(ss []Status) []Status {
	var out []Status
	for _, s := range ss {
		if !s.Present {
			out = append(out, s)
		}
	}
	return out
}

// Changed returns entries whose name now refers to something else.
func Changed(ss []Status) []Status {
	var out []Status
	for _, s := range ss {
		if s.Present && s.Changed {
			out = append(out, s)
		}
	}
	return out
}

// DockerImageID is the production `imageID` for [Check].
func DockerImageID(name string) string {
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
