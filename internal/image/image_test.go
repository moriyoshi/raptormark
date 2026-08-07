package image

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestShebang(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, content, interp, arg string
		ok                         bool
	}{
		{"plain", "#!/bin/sh\necho hi\n", "/bin/sh", "", true},
		// The idiom that hides the real interpreter behind env.
		{"env", "#!/usr/bin/env bash\necho hi\n", "/usr/bin/env", "bash", true},
		{"spaces", "#!  /bin/bash   -e\n", "/bin/bash", "-e", true},
		{"crlf", "#!/bin/sh\r\n", "/bin/sh", "", true},
		{"not a script", "echo hi\n", "", "", false},
		// A relative interpreter cannot be resolved to a guest path.
		{"relative interp", "#!bin/sh\n", "", "", false},
	}
	for _, c := range cases {
		p := writeFile(t, dir, c.name, c.content, 0o755)
		interp, arg, ok := shebang(p)
		if ok != c.ok || interp != c.interp || arg != c.arg {
			t.Errorf("%s: got (%q,%q,%v), want (%q,%q,%v)", c.name, interp, arg, ok, c.interp, c.arg, c.ok)
		}
	}
}

func TestAbsolutePathsNeedsNulTermination(t *testing.T) {
	// Only NUL-terminated runs count, which is how a real string table stores
	// paths; otherwise any slash in a data blob becomes a candidate.
	got := absolutePaths([]byte("/usr/bin/initdb\x00junk/not/terminated"))
	if !slices.Contains(got, "/usr/bin/initdb") {
		t.Errorf("missed NUL-terminated path: %v", got)
	}
	for _, g := range got {
		if g == "junk/not/terminated" {
			t.Errorf("accepted unterminated non-absolute run: %v", got)
		}
	}
}

func TestBareWords(t *testing.T) {
	got := bareWords([]byte("exec gosu postgres \"$@\"\npg_isready -U x\n"))
	for _, want := range []string{"exec", "gosu", "postgres", "pg_isready"} {
		if !slices.Contains(got, want) {
			t.Errorf("bareWords missed %q: %v", want, got)
		}
	}
}

// inventory builds a synthetic image: a script entrypoint using the env idiom,
// its interpreter, and programs named only by bare word.
func inventory(t *testing.T) (*Inventory, string) {
	t.Helper()
	dir := t.TempDir()
	entry := writeFile(t, dir, "entry.sh", "#!/usr/bin/env bash\nexec gosu postgres\n", 0o755)
	inv := &Inventory{
		Programs: map[string]Executable{
			"/usr/bin/env":    {GuestPath: "/usr/bin/env", HostPath: writeFile(t, dir, "env.bin", "", 0o755)},
			"/usr/bin/bash":   {GuestPath: "/usr/bin/bash", HostPath: writeFile(t, dir, "bash.bin", "", 0o755)},
			"/usr/bin/gosu":   {GuestPath: "/usr/bin/gosu", HostPath: writeFile(t, dir, "gosu.bin", "", 0o755)},
			"/usr/bin/unused": {GuestPath: "/usr/bin/unused", HostPath: writeFile(t, dir, "unused.bin", "", 0o755)},
		},
		Scripts: map[string]Script{
			"/entry.sh": {GuestPath: "/entry.sh", HostPath: entry, Interp: "/usr/bin/env", InterpArg: "bash"},
		},
	}
	return inv, dir
}

func TestClosureFollowsScriptsAndBareWords(t *testing.T) {
	inv, _ := inventory(t)
	got, _, err := Closure(inv, ClosureOptions{
		Seeds:    []string{"/entry.sh"},
		PathDirs: []string{"/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// env is the #! interpreter; bash is its argument resolved via PATH; gosu is
	// a bare command word in the script body.
	for _, want := range []string{"/usr/bin/env", "/usr/bin/bash", "/usr/bin/gosu"} {
		if !slices.Contains(got, want) {
			t.Errorf("closure missing %q: %v", want, got)
		}
	}
	// The script itself is not a registry program — the interpreter execs it.
	if slices.Contains(got, "/entry.sh") {
		t.Errorf("script listed as a program: %v", got)
	}
	// Nothing references it, so it must not be dragged in.
	if slices.Contains(got, "/usr/bin/unused") {
		t.Errorf("unreferenced program pulled into closure: %v", got)
	}
}

func TestClosureExcludeAndMax(t *testing.T) {
	inv, _ := inventory(t)
	got, _, err := Closure(inv, ClosureOptions{
		Seeds:    []string{"/entry.sh"},
		PathDirs: []string{"/usr/bin"},
		Exclude:  map[string]bool{"/usr/bin/gosu": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "/usr/bin/gosu") {
		t.Errorf("Exclude ignored: %v", got)
	}

	capped, _, err := Closure(inv, ClosureOptions{
		Seeds:    []string{"/entry.sh"},
		PathDirs: []string{"/usr/bin"},
		Max:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) > 1 {
		t.Errorf("Max ignored: %v", capped)
	}
}

func TestClosureRejectsUnresolvableSeeds(t *testing.T) {
	inv, _ := inventory(t)
	// Silently producing an empty module would be far worse than failing here.
	if _, _, err := Closure(inv, ClosureOptions{Seeds: []string{"/nope"}}); err == nil {
		t.Error("expected an error for a seed that names nothing")
	}
}

func TestEntrypointSeedsResolvesScriptsAndPath(t *testing.T) {
	inv, _ := inventory(t)
	cfg := Config{
		Entrypoint: []string{"/entry.sh"},
		// A bare CMD name must resolve against PATH...
		Cmd: []string{"gosu", "-u", "postgres"},
		Env: []string{"PATH=/usr/bin"},
	}
	got := EntrypointSeeds(cfg, inv)
	for _, want := range []string{"/entry.sh", "/usr/bin/gosu"} {
		if !slices.Contains(got, want) {
			t.Errorf("seeds missing %q: %v", want, got)
		}
	}
	// ...while flags are arguments, not programs.
	for _, unwanted := range []string{"-u", "postgres"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("seeds contain non-program %q: %v", unwanted, got)
		}
	}
}

// symlink creates dir/name -> target inside dir, returning nothing useful.
func symlink(t *testing.T, dir, name, target string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func TestScanRecordsExecutableSymlinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "usr/local/bin/python3.14", "", 0o755)
	writeFile(t, root, "bin/busybox", "", 0o755)
	writeFile(t, root, "etc/passwd", "root:x:0:0\n", 0o644)
	// The shape that breaks python:3-slim: a relative link to a versioned name.
	symlink(t, root, "usr/local/bin/python3", "python3.14")
	// A two-hop chain, and an ABSOLUTE target, which must stay inside the rootfs
	// rather than escaping to the host.
	symlink(t, root, "usr/local/bin/python", "/usr/local/bin/python3")
	symlink(t, root, "bin/sh", "/bin/busybox")
	// Links to non-executables and to nothing at all are not exec targets.
	symlink(t, root, "etc/pw", "/etc/passwd")
	symlink(t, root, "bin/gone", "/bin/missing")

	inv, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/usr/local/bin/python3": "/usr/local/bin/python3.14",
		"/usr/local/bin/python":  "/usr/local/bin/python3.14",
		"/bin/sh":                "/bin/busybox",
	}
	for src, dst := range want {
		if got := inv.Links[src]; got != dst {
			t.Errorf("Links[%q] = %q, want %q", src, got, dst)
		}
	}
	for _, src := range []string{"/etc/pw", "/bin/gone"} {
		if got, ok := inv.Links[src]; ok {
			t.Errorf("Links[%q] = %q, want absent", src, got)
		}
	}
}

func TestResolveGuestLinkStopsOnLoop(t *testing.T) {
	root := t.TempDir()
	symlink(t, root, "a", "b")
	symlink(t, root, "b", "a")
	if got, ok := resolveGuestLink(root, "/a"); ok {
		t.Errorf("resolveGuestLink followed a loop to %q", got)
	}
}

// A symlinked entrypoint must resolve to the program behind it — the failure
// observed on python:3-slim, where python3 -> python3.14 left seeds empty.
func TestSymlinkedEntrypointResolves(t *testing.T) {
	inv, dir := inventory(t)
	inv.Programs["/usr/local/bin/python3.14"] = Executable{
		GuestPath: "/usr/local/bin/python3.14",
		HostPath:  writeFile(t, dir, "python3.14.bin", "", 0o755),
	}
	inv.Links = map[string]string{
		"/usr/local/bin/python3": "/usr/local/bin/python3.14",
	}
	cfg := Config{
		Cmd: []string{"python3"},
		Env: []string{"PATH=/usr/local/bin:/usr/bin"},
	}
	got := EntrypointSeeds(cfg, inv)
	if !slices.Contains(got, "/usr/local/bin/python3.14") {
		t.Fatalf("seeds did not follow the symlink: %v", got)
	}

	// The closure must list the real program once, and never the symlink: the
	// runtime resolves the path through the VFS before the exec-map lookup, so a
	// second entry would translate the same binary twice.
	closure, _, err := Closure(inv, ClosureOptions{Seeds: got, PathDirs: []string{"/usr/local/bin", "/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(closure, "/usr/local/bin/python3") {
		t.Errorf("symlink admitted as a program: %v", closure)
	}
	n := 0
	for _, p := range closure {
		if p == "/usr/local/bin/python3.14" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("program listed %d times, want 1: %v", n, closure)
	}
}

// A wrapper symlink must not hide the binary it dispatches to. Debian's
// /usr/bin/psql -> pg_wrapper execs /usr/lib/postgresql/17/bin/psql from a path
// it builds at run time, which no static scan can see — so both have to be in
// the closure. Resolving only the first PATH hit silently dropped the real one.
func TestPathLookupDoesNotStopAtAWrapper(t *testing.T) {
	inv, dir := inventory(t)
	inv.Programs["/usr/lib/postgresql/17/bin/psql"] = Executable{
		GuestPath: "/usr/lib/postgresql/17/bin/psql",
		HostPath:  writeFile(t, dir, "psql.bin", "", 0o755),
	}
	inv.Scripts["/usr/share/postgresql-common/pg_wrapper"] = Script{
		GuestPath: "/usr/share/postgresql-common/pg_wrapper",
		HostPath:  writeFile(t, dir, "pg_wrapper", "#!/usr/bin/bash\nexec psql\n", 0o755),
		Interp:    "/usr/bin/bash",
	}
	inv.Links = map[string]string{
		"/usr/bin/psql": "/usr/share/postgresql-common/pg_wrapper",
	}
	dirs := []string{"/usr/bin", "/usr/lib/postgresql/17/bin"}

	got := resolveInPath("psql", dirs, inv)
	for _, want := range []string{
		"/usr/share/postgresql-common/pg_wrapper",
		"/usr/lib/postgresql/17/bin/psql",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("resolveInPath missing %q: %v", want, got)
		}
	}

	cfg := Config{Cmd: []string{"psql"}, Env: []string{"PATH=" + dirs[0] + ":" + dirs[1]}}
	closure, _, err := Closure(inv, ClosureOptions{Seeds: EntrypointSeeds(cfg, inv), PathDirs: dirs})
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper is a script, so only the real binary is a registry program.
	if !slices.Contains(closure, "/usr/lib/postgresql/17/bin/psql") {
		t.Errorf("wrapper hid the real binary: %v", closure)
	}
}

// Scan must record a symlink that names a DIRECTORY. `recordLink` deliberately
// ignores those -- it keeps only exec targets -- which is why `/bin` was absent
// from the inventory entirely before 2026-08-18.
func TestScanRecordsDirectorySymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Relative, exactly as Debian ships merged-usr.
	symlink(t, root, "bin", "usr/bin")
	// A dangling link and a link to a plain file must NOT become DirLinks.
	symlink(t, root, "nowhere", "usr/absent")
	writeFile(t, root, "usr/bin/data", "x", 0o644)
	symlink(t, root, "data-link", "usr/bin/data")

	inv, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := inv.DirLinks["/bin"]; got != "/usr/bin" {
		t.Errorf("DirLinks[/bin] = %q, want /usr/bin", got)
	}
	for _, unwanted := range []string{"/nowhere", "/data-link"} {
		if got, ok := inv.DirLinks[unwanted]; ok {
			t.Errorf("DirLinks[%q] = %q; only directory targets belong here", unwanted, got)
		}
	}
}

// The merged-usr layout every current Debian and Ubuntu uses: `/bin` is a
// symlink to `usr/bin`, so an image with `CMD ["/bin/foo"]` names a program the
// walk inventoried as `/usr/bin/foo`. Before 2026-08-18 that resolved to nothing
// and the closure came out empty -- `/bin/foo` is not itself a link, `/bin` is,
// and `filepath.WalkDir` does not descend through a directory symlink, so
// `/bin/foo` was never a key in Programs either.
func TestCanonResolvesThroughASymlinkedDirectory(t *testing.T) {
	inv := &Inventory{
		Programs: map[string]Executable{"/usr/bin/tdesc": {GuestPath: "/usr/bin/tdesc"}},
		Scripts:  map[string]Script{},
		Links:    map[string]string{},
		DirLinks: map[string]string{"/bin": "/usr/bin"},
	}
	if got := canon(inv, "/bin/tdesc"); got != "/usr/bin/tdesc" {
		t.Errorf("canon(/bin/tdesc) = %q, want /usr/bin/tdesc", got)
	}
	// The real path keeps working...
	if got := canon(inv, "/usr/bin/tdesc"); got != "/usr/bin/tdesc" {
		t.Errorf("canon(/usr/bin/tdesc) = %q", got)
	}
	// ...and a name under the link that does not exist is not invented.
	if got := canon(inv, "/bin/absent"); got != "/bin/absent" {
		t.Errorf("canon(/bin/absent) = %q, want it unchanged", got)
	}
}

// A prefix must end at a component boundary. `/binary` is not under `/bin`, and
// a plain string-prefix test would rewrite it to `/usr/binary`.
func TestADirectoryLinkOnlyMatchesWholeComponents(t *testing.T) {
	// ⚠️ TWO earlier versions of this test could not fail, for different reasons,
	// and both were found by neutralizing rather than by reading:
	//
	//  1. asserting on a path that resolved to itself -- the bad rewrite simply
	//     found nothing and canon fell through to the same answer. A rule that
	//     mis-fires is only observable where the mis-fire RESOLVES.
	//  2. asserting on a path that is ITSELF in Programs -- `canon` answers from
	//     the exact lookup and never reaches the prefix loop at all.
	//
	// So the subject must be a path that is NOT known directly, and whose
	// mistaken rewrite IS known.
	inv := &Inventory{
		Programs: map[string]Executable{
			"/usr/bin/ary/absent": {GuestPath: "/usr/bin/ary/absent"},
		},
		Scripts:  map[string]Script{},
		Links:    map[string]string{},
		DirLinks: map[string]string{"/bin": "/usr/bin"},
	}
	if got := canon(inv, "/binary/absent"); got != "/binary/absent" {
		t.Errorf("canon(/binary/absent) = %q; /bin must not match /binary, and "+
			"a prefix that ends mid-component rewrote it to a real program", got)
	}
}

// Longest prefix wins, or a nested link is shadowed by its parent and resolves
// to the wrong directory.
func TestTheLongestDirectoryLinkWins(t *testing.T) {
	// ⚠️ BOTH rewrites resolve, on purpose. With only the long one resolving,
	// the loop simply tried prefixes until something matched and the ORDER never
	// mattered -- the test passed with the sort reversed.
	inv := &Inventory{
		Programs: map[string]Executable{
			"/opt/real/tool":      {GuestPath: "/opt/real/tool"},
			"/mnt/usr/bin/x/tool": {GuestPath: "/mnt/usr/bin/x/tool"},
		},
		Scripts: map[string]Script{},
		Links:   map[string]string{},
		DirLinks: map[string]string{
			"/usr":       "/mnt/usr",
			"/usr/bin/x": "/opt/real",
		},
	}
	if got := canon(inv, "/usr/bin/x/tool"); got != "/opt/real/tool" {
		t.Errorf("canon(/usr/bin/x/tool) = %q, want /opt/real/tool", got)
	}
}

// TestResolveExecPathResolvesABareNameAgainstPATH.
//
// # The defect this guards
//
// `internal/pipeline` wrote the sidecar's boot argv straight from the image
// config. postgres:17's is `["docker-entrypoint.sh", "postgres"]` -- a BARE
// name. The runtime resolves argv[0] with `vfs.resolve(cwd, path)`, which is
// CWD-relative and does no PATH lookup, so with cwd "/" it looked for
// `/docker-entrypoint.sh`, found nothing, and fell back to program 0. Measured
// 2026-08-27: the guest printed apt's `E: Invalid operation postgres`.
//
// ⚠️ 5 of 7 surveyed images were affected. nginx:latest and nginx:alpine escaped
// only because their Dockerfiles write `/docker-entrypoint.sh` with a leading
// slash, which is why nothing had caught it.
func TestResolveExecPathResolvesABareNameAgainstPATH(t *testing.T) {
	dir := t.TempDir()
	inv := &Inventory{
		Programs: map[string]Executable{
			"/usr/local/bin/postgres": {GuestPath: "/usr/local/bin/postgres",
				HostPath: writeFile(t, dir, "pg", "", 0o755)},
		},
		Scripts: map[string]Script{
			// Where postgres:17 really keeps it -- NOT at "/".
			"/usr/local/bin/docker-entrypoint.sh": {GuestPath: "/usr/local/bin/docker-entrypoint.sh",
				HostPath: writeFile(t, dir, "ep.sh", "#!/bin/bash\n", 0o755), Interp: "/bin/bash"},
		},
		Links: map[string]string{}, DirLinks: map[string]string{},
	}
	cfg := Config{Env: []string{"PATH=/usr/local/bin:/usr/bin"}}

	if got := ResolveExecPath("docker-entrypoint.sh", cfg, inv); got != "/usr/local/bin/docker-entrypoint.sh" {
		t.Errorf("ResolveExecPath(bare script) = %q, want /usr/local/bin/docker-entrypoint.sh.\n"+
			"Unresolved, the runtime joins it to the cwd, gets /docker-entrypoint.sh, "+
			"finds nothing and boots program 0 -- the wrong program, silently.", got)
	}
	if got := ResolveExecPath("postgres", cfg, inv); got != "/usr/local/bin/postgres" {
		t.Errorf("ResolveExecPath(bare program) = %q, want /usr/local/bin/postgres", got)
	}
	// An absolute path is already resolved and must pass through unchanged --
	// this is the nginx shape, the one that always worked.
	if got := ResolveExecPath("/usr/local/bin/postgres", cfg, inv); got != "/usr/local/bin/postgres" {
		t.Errorf("ResolveExecPath(absolute) = %q, want it unchanged", got)
	}
	// ❗ A name nothing answers to returns "", so the caller can LEAVE ARGV
	// ALONE. Substituting some other program would be the defect this fixes,
	// pointed the other way.
	if got := ResolveExecPath("nonesuch", cfg, inv); got != "" {
		t.Errorf("ResolveExecPath(unknown) = %q, want \"\" so the caller does not substitute", got)
	}
}
