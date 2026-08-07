package link

import (
	"bytes"
	"strings"
	"testing"
)

func progsFor(names ...string) []Program {
	var out []Program
	for i, n := range names {
		out = append(out, Program{Name: n, Index: i})
	}
	return out
}

func TestDlMapRoundTrips(t *testing.T) {
	want := []DlEntry{
		{Path: "/usr/lib/postgresql/17/lib/pgcrypto.so", Hash: "pgcrypto_a1b2"},
		{Path: "/usr/lib/postgresql/17/lib/amcheck.so", Hash: "amcheck_c3d4"},
	}
	b, err := DlMap(progsFor("pgcrypto_a1b2", "amcheck_c3d4"), want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseDlMap(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("round-tripped %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d round-tripped as %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The two maps must stay one format, or the runtime needs two parsers and the
// second is the one nobody tested. Asserted by encoding the SAME pairs both ways
// and comparing everything after the magic.
func TestDlMapEncodesLikeTheExecMap(t *testing.T) {
	progs := progsFor("h1", "h2")
	pairs := [][2]string{{"/a/x.so", "h1"}, {"/b/y.so", "h2"}}

	var de []DlEntry
	var ee []ExecEntry
	for _, p := range pairs {
		de = append(de, DlEntry{Path: p[0], Hash: p[1]})
		ee = append(ee, ExecEntry{Path: p[0], Hash: p[1]})
	}
	d, err := DlMap(progs, de)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ExecMap(progs, ee)
	if err != nil {
		t.Fatal(err)
	}
	if len(dlMagic) != len(execMagic) {
		t.Fatalf("magics differ in LENGTH (%d vs %d); every offset after them shifts",
			len(dlMagic), len(execMagic))
	}
	if dlMagic == execMagic {
		t.Fatal("the two magics are identical, so a reader cannot tell the maps apart")
	}
	if !bytes.Equal(d[len(dlMagic):], e[len(execMagic):]) {
		t.Errorf("the two encodings diverge after the magic:\n dlopen %x\n exec   %x",
			d[len(dlMagic):], e[len(execMagic):])
	}
}

func TestDlMapRefusesWhatWouldFailAtRunTime(t *testing.T) {
	progs := progsFor("known")
	for _, c := range []struct {
		name    string
		entries []DlEntry
		want    string
	}{
		{"unknown unit", []DlEntry{{Path: "/a.so", Hash: "absent"}}, "unknown unit"},
		{"empty path", []DlEntry{{Path: "", Hash: "known"}}, "empty path"},
		{"duplicate path", []DlEntry{
			{Path: "/a.so", Hash: "known"}, {Path: "/a.so", Hash: "known"},
		}, "duplicate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := DlMap(progs, c.entries)
			if err == nil {
				t.Fatalf("DlMap accepted %s; the failure would surface at run time as a dlopen that resolves to nothing", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the problem (%q)", err, c.want)
			}
		})
	}
}

// A reader that does not recognise the magic treats the map as ABSENT, and an
// absent dlopen map means every dlopen of a unit fails. Build time must refuse
// rather than emit something that reads as empty.
func TestParseDlMapRefusesAForeignMagic(t *testing.T) {
	e, err := ExecMap(progsFor("h1"), []ExecEntry{{Path: "/bin/x", Hash: "h1"}})
	if err != nil {
		t.Fatal(err)
	}
	// An exec map is byte-compatible except for its magic, which is exactly the
	// mix-up most likely to happen and least likely to be noticed.
	if _, err := ParseDlMap(e); err == nil {
		t.Fatal("ParseDlMap accepted an EXEC map; the two would be interchangeable")
	}
	if _, err := ParseDlMap(nil); err == nil {
		t.Fatal("ParseDlMap accepted empty bytes")
	}
	d, err := DlMap(progsFor("h1"), []DlEntry{{Path: "/a.so", Hash: "h1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDlMap(d[:len(d)-1]); err == nil {
		t.Fatal("ParseDlMap accepted a truncated map")
	}
}
