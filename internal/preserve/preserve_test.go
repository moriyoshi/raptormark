package preserve

import (
	"os"
	"path/filepath"
	"testing"
)

// The three states this package exists to distinguish, and the reason it exists
// at all is that two of them were previously indistinguishable from "fine".
//
// ⚠️ Every test here runs without Docker: `Check` takes its image resolver as a
// parameter precisely so this package stays on the default `go test ./...` path,
// which `AGENTS.md` requires to need neither Docker, root, nor network.

func writeManifest(t *testing.T, root string, m *Manifest) {
	t.Helper()
	if err := Save(root, m); err != nil {
		t.Fatal(err)
	}
}

// noImages is the resolver for a machine with no Docker at all. Named rather
// than inlined because "every image is absent" is a meaningful condition and a
// bare `func(string) string { return "" }` at four call sites reads like noise.
func noImages(string) string { return "" }

// ❗ THE CENTRAL CASE. A recorded thing that is gone must be reported, and this
// is the exact event that went unreported when `_recovery/` and
// `raptormark-tmp-ossldgst:latest` were lost.
func TestARecordedThingThatDisappearedIsReported(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Entries: []Entry{
		{Kind: KindPath, Name: "kept"},
		{Kind: KindPath, Name: "gone", Note: "irreplaceable"},
	}}
	miss := Missing(Check(root, m, noImages))
	if len(miss) != 1 {
		t.Fatalf("want exactly the one missing entry, got %d: %+v", len(miss), miss)
	}
	if miss[0].Name != "gone" {
		t.Errorf("reported the wrong entry: %q", miss[0].Name)
	}
	// The note has to survive into the report. A bare "gone is missing" does not
	// tell the reader whether to stop what they are doing.
	if miss[0].Note != "irreplaceable" {
		t.Errorf("the note did not reach the status, so a failure cannot explain the stakes")
	}
}

// ❗ THE CASE THAT WOULD GET THIS DELETED. On a fresh machine nothing is
// recorded, and the answer must be "nothing is known" -- NOT an alarm and NOT an
// all-clear. A check that cries wolf on a clean clone gets switched off, and
// then there is no check.
func TestNothingRecordedIsNeitherAnAlarmNorAnAllClear(t *testing.T) {
	root := t.TempDir()
	m, ok, err := Load(root)
	if err != nil {
		t.Fatalf("a missing manifest must not be an error: %v", err)
	}
	if ok || m != nil {
		t.Fatalf("a missing manifest must report not-recorded, got ok=%v m=%+v", ok, m)
	}
}

// A manifest that exists but cannot be parsed IS an error -- the one state where
// something was recorded and cannot be read. Answering "nothing is known" would
// silently discard a real baseline.
func TestAnUnreadableManifestIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestPath), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(root); err == nil {
		t.Error("a corrupt manifest must be an error, not an empty baseline: a real " +
			"record would be discarded silently")
	}
}

// An image tag that still exists but now names a DIFFERENT image is not a loss
// the way a missing tag is, and must not be reported as one -- but it is worth
// reporting, because the name no longer refers to what was recorded.
func TestARetaggedImageIsChangedNotMissing(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Entries: []Entry{
		{Kind: KindImage, Name: "builder:x", ID: "sha256:old"},
	}}
	ss := Check(root, m, func(string) string { return "sha256:new" })
	if len(Missing(ss)) != 0 {
		t.Error("a present tag must not be reported as missing")
	}
	ch := Changed(ss)
	if len(ch) != 1 || ch[0].Now != "sha256:new" {
		t.Fatalf("want one changed entry carrying the new id, got %+v", ch)
	}
}

// An image recorded WITHOUT an id (older manifest, or one written by hand) must
// check on presence alone and never report "changed" -- otherwise every such
// entry fires on the first run.
func TestAnImageWithNoRecordedIDChecksPresenceOnly(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Entries: []Entry{{Kind: KindImage, Name: "builder:x"}}}
	ss := Check(root, m, func(string) string { return "sha256:whatever" })
	if len(Missing(ss)) != 0 || len(Changed(ss)) != 0 {
		t.Errorf("an id-less image entry must be a plain presence check, got %+v", ss)
	}
}

// ⚠️ An unknown kind must read as MISSING, not as fine. A manifest written by a
// newer version naming a kind this binary cannot check must never produce an
// all-clear -- that is the "0 fail with coverage gone" shape.
func TestAnUnknownKindIsNotAnAllClear(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Entries: []Entry{{Kind: Kind("bucket"), Name: "s3://x"}}}
	if len(Missing(Check(root, m, noImages))) != 1 {
		t.Error("an unrecognised kind must report as missing; treating it as present " +
			"turns an unreadable record into a green result")
	}
}

// Save/Load must round-trip, and Save must sort so that re-snapshotting produces
// a reviewable diff rather than a reordering.
func TestSaveSortsAndRoundTrips(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, &Manifest{Entries: []Entry{
		{Kind: KindPath, Name: "z"},
		{Kind: KindImage, Name: "b:1"},
		{Kind: KindPath, Name: "a"},
		{Kind: KindImage, Name: "a:1"},
	}})
	m, ok, err := Load(root)
	if err != nil || !ok {
		t.Fatalf("round-trip failed: ok=%v err=%v", ok, err)
	}
	var got []string
	for _, e := range m.Entries {
		got = append(got, string(e.Kind)+"/"+e.Name)
	}
	want := []string{"image/a:1", "image/b:1", "path/a", "path/z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("not sorted for a reviewable diff:\n got %v\nwant %v", got, want)
		}
	}
}

// ❗ Snapshot must REFUSE to record something already absent. A manifest listing
// a missing thing fails `check` immediately and forever, and a check that can
// never pass is a check somebody deletes -- which would leave exactly the
// situation this package was written to end.
func TestSnapshotRefusesToRecordSomethingAlreadyGone(t *testing.T) {
	root := t.TempDir()
	c := &Snapshot{Root: root, Path: []string{filepath.Join(root, "nope")}}
	err := c.Run()
	if err == nil {
		t.Fatal("recording an absent path must fail; otherwise the manifest bakes in " +
			"a permanent failure")
	}
	if _, ok, _ := Load(root); ok {
		t.Error("a refused snapshot must not leave a manifest behind")
	}
}

// --add must UPDATE an existing entry rather than duplicating it: two entries
// for one image with different ids would report a spurious "changed" against
// whichever copy lost.
func TestAddUpdatesRatherThanDuplicating(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "thing")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{"first", "second"} {
		c := &Snapshot{Root: root, Path: []string{p}, Note: note, Add: true}
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("want one entry after re-recording the same path, got %d", len(m.Entries))
	}
	if m.Entries[0].Note != "second" {
		t.Errorf("re-recording must update the entry, got note %q", m.Entries[0].Note)
	}
}
