package link

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := []Program{
		{Name: "dash_fused_e23036abafac", Index: 0},
		{Name: "initdb_fused_4ce3bcd0524a", Index: 1},
		{Name: "psql_fused_2a30ec46cff1", Index: 2},
	}
	if err := WriteManifest(dir, want); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d programs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("program %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestManifestFeedsExecMap is the whole point of the manifest: the program list
// that validates the exec map comes from the LINK, not from a second derivation
// that can disagree with it. With the manifest in hand, a hash typed or
// computed wrongly is caught at build time instead of becoming a program-0
// fallback at run time.
func TestManifestFeedsExecMap(t *testing.T) {
	dir := t.TempDir()
	progs := []Program{
		{Name: "dash_fused_aaaaaaaaaaaa", Index: 0},
		{Name: "psql_fused_bbbbbbbbbbbb", Index: 1},
	}
	if err := WriteManifest(dir, progs); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	linked, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}

	psql, err := ProgramByIndex(linked, 1)
	if err != nil {
		t.Fatalf("ProgramByIndex: %v", err)
	}
	if _, err := ExecMap(linked, []ExecEntry{{Path: "/usr/bin/psql", Hash: psql.Name}}); err != nil {
		t.Errorf("a manifest-derived hash must be accepted: %v", err)
	}

	// The failure this whole mechanism exists to prevent. A stale hash -- the
	// shape of every one of the four incidents -- must not produce a map.
	_, err = ExecMap(linked, []ExecEntry{{Path: "/usr/bin/psql", Hash: "psql_fused_STALEHASH00"}})
	if err == nil {
		t.Fatal("a stale hash produced an exec map; it must be rejected at build time")
	}
	if !strings.Contains(err.Error(), "unknown program") {
		t.Errorf("error should name the unknown program, got: %v", err)
	}
}

func TestProgramByIndexRejectsAnAbsentIndex(t *testing.T) {
	progs := []Program{{Name: "a", Index: 0}}
	if _, err := ProgramByIndex(progs, 3); err == nil {
		t.Fatal("index 3 of a one-program module must be an error")
	}
}

// TestReadManifestRejectsInconsistentPrograms covers the states that would
// silently mis-describe the registry. The registry is an ARRAY indexed by
// position, so a gap or a duplicate index means some element of it describes
// the wrong program -- exactly the failure mode the manifest exists to remove,
// so it must not be able to arrive through the manifest itself.
func TestReadManifestRejectsInconsistentPrograms(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "a gap in the indices",
			json: `{"programs":[{"Name":"a","Index":0},{"Name":"b","Index":2}]}`,
			want: "outside",
		},
		{
			name: "two programs at one index",
			json: `{"programs":[{"Name":"a","Index":0},{"Name":"b","Index":0}]}`,
			want: "share index",
		},
		{
			name: "two programs with one name",
			json: `{"programs":[{"Name":"a","Index":0},{"Name":"a","Index":1}]}`,
			want: "share the name",
		},
		{
			name: "an unnamed program",
			json: `{"programs":[{"Name":"","Index":0}]}`,
			want: "no name",
		},
		{
			name: "no programs at all",
			json: `{"programs":[]}`,
			want: "no programs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(tc.json), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ReadManifest(dir)
			if err == nil {
				t.Fatalf("%s was accepted; it must be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestReadManifestReportsAMissingFile(t *testing.T) {
	if _, err := ReadManifest(t.TempDir()); err == nil {
		t.Fatal("a missing manifest must be an error, not an empty program list")
	}
}

func TestSortedByIndexPutsProgramsInRegistryOrder(t *testing.T) {
	got := SortedByIndex([]Program{{Name: "c", Index: 2}, {Name: "a", Index: 0}, {Name: "b", Index: 1}})
	for i, p := range got {
		if p.Index != i {
			t.Fatalf("position %d holds index %d; link-all takes objects in registry order", i, p.Index)
		}
	}
}

// TestWriteLinkInputsProducesBoth is the guard for the reason that function
// exists: a registry without a manifest beside it is the state the manifest was
// introduced to eliminate, and a driver that can produce it eventually will.
func TestWriteLinkInputsProducesBoth(t *testing.T) {
	dir := t.TempDir()
	progs := []Program{
		{Name: "b_fused_bbbbbbbbbbbb", Index: 1},
		{Name: "a_fused_aaaaaaaaaaaa", Index: 0},
	}
	path, err := WriteLinkInputs(dir, "registry.c", progs)
	if err != nil {
		t.Fatalf("WriteLinkInputs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("registry not written: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("manifest not written beside it: %v", err)
	}
	// Written in registry order regardless of the caller's ordering, because
	// link-all takes objects by index and a manifest that disagreed with the
	// registry it shipped with would be worse than none.
	for i, p := range got {
		if p.Index != i {
			t.Errorf("manifest entry %d has index %d; must be registry order", i, p.Index)
		}
	}
	reg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range progs {
		if !strings.Contains(string(reg), p.Name) {
			t.Errorf("registry does not mention %q", p.Name)
		}
	}
}

func TestWriteLinkInputsRejectsAnInconsistentList(t *testing.T) {
	// Neither file should appear if the list is bad: a half-written pair is the
	// mismatch this mechanism exists to prevent.
	dir := t.TempDir()
	_, err := WriteLinkInputs(dir, "registry.c", []Program{
		{Name: "a", Index: 0}, {Name: "b", Index: 0},
	})
	if err == nil {
		t.Fatal("two programs at index 0 must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "registry.c")); err == nil {
		t.Error("a registry was written for a program list that was rejected")
	}
}
