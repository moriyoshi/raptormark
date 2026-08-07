package rootfs

import "testing"

// st_mode must answer the question a C consumer actually asks. The predicates
// below are S_ISREG/S_ISDIR/S_ISLNK written out, because the failure this
// guards against is not "the number is wrong" but "the number is a plausible
// permission mask that makes every type test false".
//
// nginx survived the bug for months by asking only S_ISDIR, which was correctly
// false for a file whether or not the type bits were present. PostgreSQL's
// validate_exec needs S_ISREG to be TRUE, and reported
// `invalid binary "/usr/lib/postgresql/17/bin/postgres"` instead of starting.
func TestStatModeCarriesTheFileType(t *testing.T) {
	const ifmt = 0o170000
	for _, tc := range []struct {
		name    string
		kind    uint8
		mode    uint32
		wantTyp uint32
	}{
		{"regular file", kindFile, 0o755, sIFREG},
		{"directory", kindDir, 0o750, sIFDIR},
		{"symlink", kindSymlink, 0o777, sIFLNK},
	} {
		got := statMode(tc.kind, tc.mode)
		if got&ifmt != tc.wantTyp {
			t.Errorf("%s: type bits %#o, want %#o (S_IS* would be false)",
				tc.name, got&ifmt, tc.wantTyp)
		}
		if got&permBits != tc.mode {
			t.Errorf("%s: permissions %#o, want %#o", tc.name, got&permBits, tc.mode)
		}
	}
}

// A mode that somehow already carries type bits must not end up with two of
// them ORed together, which would make every S_IS* test false again.
func TestStatModeIgnoresTypeBitsInTheInput(t *testing.T) {
	const ifmt = 0o170000
	got := statMode(kindFile, sIFDIR|0o644)
	if got&ifmt != sIFREG {
		t.Errorf("type bits %#o, want %#o — the node's kind must win", got&ifmt, sIFREG)
	}
	if got&permBits != 0o644 {
		t.Errorf("permissions %#o, want %#o", got&permBits, 0o644)
	}
}
