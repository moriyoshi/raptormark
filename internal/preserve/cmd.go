package preserve

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Preserve is the `raptormark preserve` command group.
type Preserve struct {
	Snapshot Snapshot `cmd:"" name:"snapshot" help:"Record what is present now, into .agents/preserve.json. Commit the result."`
	Check    Check_   `cmd:"" name:"check" help:"Report anything recorded that has since disappeared. Exits non-zero if something has."`
}

// Snapshot records the current state as the baseline.
//
// ⚠️ It records what is ACTUALLY THERE, and refuses to record a name that is
// already absent. A manifest that lists something missing at the moment it is
// written would fail `check` immediately and forever, which is how a check gets
// switched off.
type Snapshot struct {
	Root  string   `help:"Repository root." default:"."`
	Path  []string `help:"A file or directory to record. Repeatable." name:"path"`
	Image []string `help:"A local Docker image (repository:tag) to record. Repeatable." name:"image"`
	Note  string   `help:"Why these are irreplaceable. Stored with every entry recorded in this run."`
	Add   bool     `help:"Merge into the existing manifest instead of replacing it."`
}

func (c *Snapshot) Run() error {
	m := &Manifest{}
	if c.Add {
		if existing, ok, err := Load(c.Root); err != nil {
			return err
		} else if ok {
			m = existing
		}
	}
	// Keyed so --add re-recording a name UPDATES it rather than duplicating it.
	// A duplicated image entry with two different ids would report a spurious
	// "changed" against whichever copy lost.
	idx := map[string]int{}
	for i, e := range m.Entries {
		idx[string(e.Kind)+"\x00"+e.Name] = i
	}
	put := func(e Entry) {
		if i, ok := idx[string(e.Kind)+"\x00"+e.Name]; ok {
			m.Entries[i] = e
			return
		}
		idx[string(e.Kind)+"\x00"+e.Name] = len(m.Entries)
		m.Entries = append(m.Entries, e)
	}

	var refused []string
	for _, p := range c.Path {
		if _, err := os.Stat(p); err != nil {
			refused = append(refused, "path "+p)
			continue
		}
		put(Entry{Kind: KindPath, Name: p, Note: c.Note})
	}
	for _, img := range c.Image {
		id := DockerImageID(img)
		if id == "" {
			refused = append(refused, "image "+img)
			continue
		}
		put(Entry{Kind: KindImage, Name: img, Note: c.Note, ID: id})
	}
	if len(refused) > 0 {
		// ❗ FATAL, not a warning. Recording a name that is already gone is the
		// one way to produce a manifest that can never pass, and a check that
		// can never pass is a check somebody deletes.
		return fmt.Errorf("preserve snapshot: refusing to record %d thing(s) that are "+
			"not present:\n  %s\n\nA manifest is a record of what IS here. If one of "+
			"these is already lost, that is worth knowing now rather than being "+
			"baked into a check that fails forever",
			len(refused), strings.Join(refused, "\n  "))
	}
	if len(m.Entries) == 0 {
		return fmt.Errorf("preserve snapshot: nothing to record; pass --path or --image")
	}
	// ⚠️ The CLI reads the clock; the PACKAGE does not. `preserve.go` keeps
	// `Recorded` caller-supplied so every test is deterministic, and this is the
	// one caller that has a real answer. An earlier version wrote the literal
	// "unset" here, which is the "declared field nothing reads" shape this tree
	// keeps finding -- a field carrying a value that means nothing is worse than
	// no field, because it looks answered.
	m.Recorded = time.Now().UTC().Format(time.RFC3339)
	if err := Save(c.Root, m); err != nil {
		return err
	}
	fmt.Printf("recorded %d entries in %s\n", len(m.Entries), ManifestPath)
	fmt.Printf("⚠️  COMMIT IT. A manifest that is not committed shares the fate of what it guards.\n")
	return nil
}

// Check_ reports disappearances.
//
// Named with a trailing underscore because `Check` is the package's own
// function and kong needs the struct; renaming the function would cost the
// clearer name at every call site.
type Check_ struct {
	Root string `help:"Repository root." default:"."`
}

func (c *Check_) Run() error {
	m, ok, err := Load(c.Root)
	if err != nil {
		return err
	}
	if !ok {
		// ⚠️ NOT an error and NOT an all-clear. Nothing has been recorded, so
		// nothing is known -- and saying "ok" here would be the exact false
		// reassurance this package exists to remove.
		fmt.Printf("nothing recorded (%s does not exist), so nothing can be checked.\n"+
			"Record a baseline with:\n"+
			"  raptormark preserve snapshot --image <tag> --path <dir> --note '<why>'\n",
			ManifestPath)
		return nil
	}
	ss := Check(c.Root, m, DockerImageID)
	missing, changed := Missing(ss), Changed(ss)

	for _, s := range changed {
		fmt.Printf("CHANGED  %-6s %s\n  recorded %s\n  now      %s\n",
			s.Kind, s.Name, s.ID, s.Now)
	}
	if len(missing) == 0 {
		fmt.Printf("ok: all %d recorded entries are present", len(ss))
		if len(changed) > 0 {
			fmt.Printf(" (%d now refer to something else, above)", len(changed))
		}
		fmt.Println()
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d recorded thing(s) have DISAPPEARED:\n", len(missing))
	for _, s := range missing {
		fmt.Fprintf(&b, "\n  MISSING %-6s %s\n", s.Kind, s.Name)
		if s.Note != "" {
			fmt.Fprintf(&b, "          %s\n", s.Note)
		}
	}
	b.WriteString("\n❗ This is the event that went unreported when `_recovery/` and " +
		"raptormark-tmp-ossldgst:latest were lost. Check for a backup before doing " +
		"anything else; if the loss was deliberate, re-run `preserve snapshot` so the " +
		"record matches reality and the change is visible in the diff.")
	return fmt.Errorf("%s", b.String())
}
