package link

import (
	"bytes"
	"strings"
	"testing"
)

func TestRegistryCEmitsDeclaredSymbols(t *testing.T) {
	// runtime/src/abi.rs declares exactly these three symbols as extern.
	got, err := RegistryC([]Program{{Name: "a1b2", Index: 0}, {Name: "c3d4", Index: 1}})
	if err != nil {
		t.Fatalf("RegistryC: %v", err)
	}
	for _, want := range []string{
		"EcvProgram *ecv_programs[] = {",
		"const uint64_t ecv_program_count = 2;",
		"const uint64_t ecv_program_size = sizeof(EcvProgram);",
		"&ecv_program_0,",
		"&ecv_program_1,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("registry missing %q\n%s", want, got)
		}
	}
}

func TestRegistryCOrdersByIndex(t *testing.T) {
	// The runtime indexes ecv_programs[] directly, so emission order must be
	// index order regardless of the order programs are handed to us.
	got, err := RegistryC([]Program{{Name: "second", Index: 1}, {Name: "first", Index: 0}})
	if err != nil {
		t.Fatalf("RegistryC: %v", err)
	}
	first := strings.Index(got, "&ecv_program_0,")
	second := strings.Index(got, "&ecv_program_1,")
	if first < 0 || second < 0 || first > second {
		t.Errorf("ecv_programs[] not in index order:\n%s", got)
	}
}

func TestRegistryCRejectsBadIndices(t *testing.T) {
	// A hole would leave a null entry that the runtime dereferences.
	cases := map[string][]Program{
		"empty":      {},
		"gap":        {{Name: "a", Index: 0}, {Name: "b", Index: 2}},
		"duplicate":  {{Name: "a", Index: 0}, {Name: "b", Index: 0}},
		"negative":   {{Name: "a", Index: -1}},
		"not-from-0": {{Name: "a", Index: 1}},
	}
	for name, progs := range cases {
		if _, err := RegistryC(progs); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestFragmentCBindsEveryField(t *testing.T) {
	got := FragmentC(Program{Name: "pg_ab12", Index: 3})
	if !strings.Contains(got, "EcvProgram ecv_program_3 = {") {
		t.Errorf("fragment does not define ecv_program_3:\n%s", got)
	}
	// All 18 fields must be initialised — a missing one silently zeroes a
	// pointer the runtime dereferences.
	for _, field := range []string{
		".name =", ".entry_func =", ".fun_vmas =", ".fun_ptrs =",
		".block_ptrs =", ".block_vmas =", ".block_sizes =", ".block_fn_vmas =",
		".block_count =", ".data_names =", ".data_vmas =", ".data_sizes =",
		".data_bytes =", ".data_num =", ".e_ph =", ".e_phent =",
		".e_phnum =", ".entry_pc =",
	} {
		if !strings.Contains(got, field) {
			t.Errorf("fragment missing field %s", field)
		}
	}
	// Scalars are bound by address, arrays by name. Getting this backwards
	// compiles but hands the runtime the wrong pointer.
	for _, want := range []string{
		".entry_func = &_ecv_entry_func,",
		".entry_pc = &_ecv_entry_pc,",
		".block_count = &_ecv_block_address_array_size,",
		".data_num = &_ecv_data_sec_num,",
		".e_phent = &_ecv_e_phent,",
		".e_phnum = &_ecv_e_phnum,",
		".fun_vmas = _ecv_fun_vmas,",
		".e_ph = _ecv_e_ph,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fragment missing binding %q", want)
		}
	}
}

func TestFragmentCFieldOrderMatchesStruct(t *testing.T) {
	// The struct definition is shared with the registry, which takes sizeof;
	// field order is the ABI contract with runtime/src/abi.rs.
	want := []string{
		"const uint8_t *name;",
		"const LiftedFunc *entry_func;",
		"const uint64_t *fun_vmas;",
		"const LiftedFunc *fun_ptrs;",
		"uint64_t ***block_ptrs;",
		"const uint64_t **block_vmas;",
		"const uint64_t *block_sizes;",
		"const uint64_t *block_fn_vmas;",
		"const uint64_t *block_count;",
		"const uint8_t **data_names;",
		"const uint64_t *data_vmas;",
		"const uint64_t *data_sizes;",
		"const uint8_t **data_bytes;",
		"const uint64_t *data_num;",
		"const uint8_t *e_ph;",
		"const uint32_t *e_phent;",
		"const uint32_t *e_phnum;",
		"const uint64_t *entry_pc;",
	}
	got := FragmentC(Program{Name: "x", Index: 0})
	pos := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Fatalf("struct missing field %q", w)
		}
		if i < pos {
			t.Errorf("field %q out of order", w)
		}
		pos = i
	}
}

func TestSymbolMatchesKeepArgument(t *testing.T) {
	// translate-one is invoked with --keep <Symbol()>; a mismatch internalizes
	// the descriptor away and the link fails with an undefined reference.
	if got := (Program{Index: 7}).Symbol(); got != "ecv_program_7" {
		t.Errorf("Symbol() = %q, want ecv_program_7", got)
	}
}

func TestCStringEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`ab`:      `"ab"`,
		`a"b`:     `"a\"b"`,
		`a\b`:     `"a\\b"`,
		"a\nb":    `"a\012b"`,
		"caf\xc3": `"caf\303"`,
	} {
		if got := cString(in); got != want {
			t.Errorf("cString(%q) = %s, want %s", in, got, want)
		}
	}
}

// The Rust side's own test module (runtime/src/execmap.rs) encodes
// [("/bin/ls","hashA"), ("/bin/sh","hashB")] with its `encode` helper and
// asserts parse() round-trips it. This is that exact vector, byte for byte.
func TestExecMapMatchesRustVector(t *testing.T) {
	progs := []Program{{Name: "hashA", Index: 0}, {Name: "hashB", Index: 1}}
	got, err := ExecMap(progs, []ExecEntry{
		{Path: "/bin/ls", Hash: "hashA"},
		{Path: "/bin/sh", Hash: "hashB"},
	})
	if err != nil {
		t.Fatalf("ExecMap: %v", err)
	}
	want := []byte("RMEXEC01")
	want = append(want, 2, 0, 0, 0) // count
	want = append(want, 7, 0, 0, 0) // len("/bin/ls")
	want = append(want, "/bin/ls"...)
	want = append(want, 5, 0, 0, 0) // len("hashA")
	want = append(want, "hashA"...)
	want = append(want, 7, 0, 0, 0)
	want = append(want, "/bin/sh"...)
	want = append(want, 5, 0, 0, 0)
	want = append(want, "hashB"...)
	if !bytes.Equal(got, want) {
		t.Errorf("exec map mismatch\ngot  %v\nwant %v", got, want)
	}
}

func TestExecMapEmptyIsWellFormed(t *testing.T) {
	// A single-program module has no exec map entries; the runtime must still
	// see valid magic and a zero count rather than a truncated buffer.
	got, err := ExecMap([]Program{{Name: "h", Index: 0}}, nil)
	if err != nil {
		t.Fatalf("ExecMap: %v", err)
	}
	if want := append([]byte("RMEXEC01"), 0, 0, 0, 0); !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExecMapRejectsSilentDrops(t *testing.T) {
	progs := []Program{{Name: "hashA", Index: 0}}
	cases := map[string][]ExecEntry{
		// The runtime ignores an unknown hash silently; that would surface much
		// later as an exec falling back to program 0.
		"unknown hash":   {{Path: "/bin/ls", Hash: "nope"}},
		"duplicate path": {{Path: "/bin/ls", Hash: "hashA"}, {Path: "/bin/ls", Hash: "hashA"}},
		"empty path":     {{Path: "", Hash: "hashA"}},
	}
	for name, entries := range cases {
		if _, err := ExecMap(progs, entries); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestExecMapAllowsLibraryUnits(t *testing.T) {
	// A program with no exec-map path is a library unit (the shared-libc
	// superset unit), which is legitimate and must not be rejected.
	progs := []Program{{Name: "app", Index: 0}, {Name: "libc", Index: 1}}
	if _, err := ExecMap(progs, []ExecEntry{{Path: "/bin/app", Hash: "app"}}); err != nil {
		t.Errorf("library unit rejected: %v", err)
	}
}

// ParseExecMap is only worth having if it reads back exactly what ExecMap
// wrote; it exists so rootfs.Build can check an encoded map it did not build.
func TestExecMapRoundTrips(t *testing.T) {
	progs := []Program{{Name: "alpha", Index: 0}, {Name: "beta", Index: 1}}
	want := []ExecEntry{
		{Path: "/usr/bin/dash", Hash: "alpha"},
		{Path: "/usr/lib/postgresql/17/bin/postgres", Hash: "beta"},
	}
	b, err := ExecMap(progs, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseExecMap(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseExecMapRoundTripsAnEmptyMap(t *testing.T) {
	b, err := ExecMap(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseExecMap(b)
	if err != nil {
		t.Fatalf("an empty map is well-formed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want none", len(got))
	}
}

// The runtime treats all three of these as "no exec map" and runs program 0 for
// every exec. Build time is where they should stop instead.
func TestParseExecMapRejectsMalformedInput(t *testing.T) {
	good, err := ExecMap([]Program{{Name: "alpha", Index: 0}}, []ExecEntry{{Path: "/usr/bin/dash", Hash: "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":         nil,
		"bad magic":     append([]byte("RMEXEC99"), good[len(execMagic):]...),
		"truncated":     good[:len(good)-3],
		"trailing junk": append(append([]byte{}, good...), 0, 0),
	}
	for name, b := range cases {
		if got, err := ParseExecMap(b); err == nil {
			t.Errorf("%s: parsed as %+v, want an error", name, got)
		}
	}
}
