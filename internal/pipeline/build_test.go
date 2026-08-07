package pipeline

import (
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/image"
)

// ⚠️ These tests deliberately cover only the parts that need NEITHER Docker NOR
// wasm, per AGENTS.md: `go test ./...` must not require either. The pipeline
// itself is exercised end to end by `e2e/`, which is env-gated.
//
// What is here is the logic that is easy to get wrong and invisible when it is:
// the refusal to default the builder image, and the filename derivation that
// two guest paths must not collide in.

// TestBuildRefusesWithoutABuilder pins the one thing this command must not be
// helpful about.
//
// AGENTS.md: `raptormark-builder:latest` is not necessarily the newest builder,
// and a stale one "fails deep inside elflift with an error that reads like a
// defect in the input". A default would make that the common case rather than
// the rare one, and the person hitting it would have no reason to suspect the
// builder tag.
func TestBuildRefusesWithoutABuilder(t *testing.T) {
	c := &Build{Image: "postgres:17", Out: t.TempDir()}
	err := c.Run()
	if err == nil {
		t.Fatal("a build with no --builder succeeded; it must refuse rather than " +
			"guess a tag, because a stale builder fails deep inside elflift with an " +
			"error that looks like a defect in the input")
	}
	// The message has to name the flag AND the reason. An error saying only
	// "builder required" sends the reader to pick any tag, which is the failure.
	for _, want := range []string{"--builder", "RAPTORMARK_BUILDER", "not necessarily the newest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// It must refuse BEFORE doing any work: `Run` is reached with a real image
	// reference here, and inspecting or exporting it would be minutes of I/O
	// before the same refusal.
	if entries, _ := filepath.Glob(filepath.Join(c.Out, "*")); len(entries) != 0 {
		t.Errorf("the refusal happened after creating %v; it must come first", entries)
	}
}

// TestSanitiseCannotCollide is the guard for a silent, expensive corruption.
//
// Every fused image is written to <work>/<sanitise(guestPath)>.fused. If two
// distinct guest paths produced one stem, the second write would overwrite the
// first, the second translation would run on the wrong bytes, and the module
// would contain one program twice under two names -- with the exec map pointing
// both guest paths at it. Nothing would fail; the wrong program would run.
func TestSanitiseCannotCollide(t *testing.T) {
	// Paths chosen to attack the transformation itself: '/' and '.' both map to
	// '_', so any pair that differs ONLY in which separator it used is the
	// dangerous case.
	paths := []string{
		"/usr/bin/psql",
		"/usr/bin/pg_dump",
		"/usr/lib/postgresql/17/lib/ext_a.so",
		"/usr/lib/postgresql/17/lib/ext_b.so",
		"/bin/sh",
		"/usr/bin/sh",
		"/a/b.c",
		"/a.b/c", // vs /a/b.c -- differs only in separator kind
		"/a/b/c", // vs both of the above
		"/a.b.c", // and again
		"/x_y/z", // an underscore already present
		"/x/y_z", // vs the line above
	}
	seen := map[string]string{}
	for _, p := range paths {
		s := sanitise(p)
		if prev, ok := seen[s]; ok {
			t.Errorf("sanitise(%q) and sanitise(%q) both give %q.\n"+
				"Two fused images would be written to one file: the second overwrites "+
				"the first, the second translation runs on the wrong bytes, and the "+
				"module ends up containing one program twice under two names. Nothing "+
				"fails -- the wrong program runs.", prev, p, s)
		}
		seen[s] = p
		if strings.ContainsAny(s, "/") {
			t.Errorf("sanitise(%q) = %q still contains a path separator", p, s)
		}
		if s == "" {
			t.Errorf("sanitise(%q) is empty", p)
		}
	}
}

// TestGuestPathOfRoundTrips checks the host->guest mapping the dlopen map is
// built from. A wrong guest path is not a build failure: the map is written, the
// module links, and the guest's dlopen of the real path finds no entry -- which
// presents as "this build does not contain that plugin".
func TestGuestPathOfRoundTrips(t *testing.T) {
	root := "/tmp/export"
	cases := map[string]string{
		"/tmp/export/usr/lib/postgresql/17/lib/ext_a.so": "/usr/lib/postgresql/17/lib/ext_a.so",
		"/tmp/export/bin/sh":                             "/bin/sh",
		"/tmp/export/x":                                  "/x",
	}
	for host, want := range cases {
		got, err := guestPathOf(root, host)
		if err != nil {
			t.Fatalf("guestPathOf(%q): %v", host, err)
		}
		if got != want {
			t.Errorf("guestPathOf(%q) = %q, want %q", host, got, want)
		}
	}
	// A path outside the root must be an ERROR, not a silently mangled result:
	// `filepath.Rel` happily returns "../.." forms, and "/../../etc/passwd" as a
	// guest path would be written into the map as if it were real.
	if got, err := guestPathOf(root, "/elsewhere/lib.so"); err == nil {
		t.Errorf("guestPathOf accepted a path outside the root, giving %q", got)
	}
}

// TestLibraryPathsAreRootRelative catches the mistake of handing fuse the host's
// own /lib. The search list must be inside the exported rootfs; a list that
// escaped it would resolve DT_NEEDED against the BUILD MACHINE's libraries and
// fuse an x86-64 or wrong-glibc object into an aarch64 image.
func TestLibraryPathsAreRootRelative(t *testing.T) {
	root := t.TempDir()
	paths := libraryPaths(root)
	if len(paths) == 0 {
		t.Fatal("no library paths")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Errorf("library path %q is not under the exported rootfs %q: DT_NEEDED "+
				"would resolve against the build machine's own libraries", p, root)
		}
	}
	// Both usr-merged spellings, because a Debian rootfs reaches one directory
	// two ways and an image may name either.
	var haveLib, haveUsrLib bool
	for _, p := range paths {
		haveLib = haveLib || strings.HasSuffix(p, "/lib/aarch64-linux-gnu")
		haveUsrLib = haveUsrLib || strings.HasSuffix(p, "/usr/lib/aarch64-linux-gnu")
	}
	if !haveLib || !haveUsrLib {
		t.Errorf("the multiarch directories are missing from %v; that is where a "+
			"Debian rootfs actually keeps its libraries", paths)
	}
}

// TestLibraryPathsCoverWhatTheImageItselfDeclares.
//
// # The defect this guards
//
// The search list was a SUBSET of what the images declare, and the gap was not
// exotic: `python:3-slim` keeps `libpython3.14.so.1.0` in `/usr/local/lib` and
// `ruby:3-slim` keeps `libruby.so.3.4` there, so neither image's PRINCIPAL
// library resolved and neither could be built. Both are in README's image
// survey. Found 2026-08-26 by planning a layout for each
// (`.agents-workspace/drivers/headroom`), which reported
// `fuse: cannot find libpython3.14.so.1.0 in [...]`.
//
// ⚠️ The wanted set below is TRANSCRIBED from the rootfs, not from the code it
// checks -- `cat /etc/ld.so.conf.d/*.conf` inside `python:3-slim`:
//
//	libc.conf               /usr/local/lib
//	aarch64-linux-gnu.conf  /usr/local/lib/aarch64-linux-gnu
//	                        /lib/aarch64-linux-gnu
//	                        /usr/lib/aarch64-linux-gnu
//
// A test that read the same list the function returns would ratify whatever the
// function currently does, which is exactly how the two missing entries survived.
//
// ❗ This does NOT claim the resolver is correct. `libraryPaths` is a hardcoded
// list; it does not read the rootfs's own `/etc/ld.so.conf`, and the fuser
// honours neither `DT_RUNPATH` nor `DT_RPATH`. An image with a custom conf still
// fails. What is guarded is only that the stock Debian set is covered.
func TestLibraryPathsCoverWhatTheImageItselfDeclares(t *testing.T) {
	root := t.TempDir()
	paths := libraryPaths(root)

	index := func(suffix string) int {
		for i, p := range paths {
			if strings.HasSuffix(p, suffix) {
				return i
			}
		}
		return -1
	}

	for _, want := range []string{
		"/usr/local/lib",
		"/usr/local/lib/aarch64-linux-gnu",
		"/lib/aarch64-linux-gnu",
		"/usr/lib/aarch64-linux-gnu",
	} {
		if index(want) < 0 {
			t.Errorf("%s is missing from %v.\n"+
				"Debian's own /etc/ld.so.conf.d names it, and an image whose "+
				"principal library lives there cannot be fused at all -- "+
				"python:3-slim and ruby:3-slim both keep theirs in /usr/local/lib.",
				want, paths)
		}
	}

	// ❗ THE ORDERING HALF, and it is the reason this change was safe to make
	// without re-fusing every image. `fuse.findLib` takes the FIRST match over
	// this list, so an entry appended at the end cannot move a name that already
	// resolved -- only a name that resolved nowhere. Putting /usr/local first
	// would match Debian's ld.so.conf order and would silently re-point every
	// name present in both places, invalidating cached objects for images that
	// were building fine.
	local, multi := index("/usr/local/lib"), index("/usr/lib/aarch64-linux-gnu")
	if local >= 0 && multi >= 0 && local < multi {
		t.Errorf("/usr/local/lib is at index %d, BEFORE the multiarch directory at "+
			"%d. findLib takes the first match, so this re-points any name present "+
			"in both -- a silent change to what gets fused, and a cold object cache "+
			"for images that were already building.", local, multi)
	}
}

// TestOnlyTheEntryCarriesPlugins is the guard for a defect that a one-program
// fixture cannot see, and that is how it shipped.
//
// `opts` carries every discovered plugin. If the non-entry programs were fused
// with it too, then on `postgres:17` all 79 plugins would go into each of 71
// programs -- defeating the per-unit design and multiplying the image. Worse,
// `fuse.Fuse` ERRORS on a plugin it cannot satisfy rather than skipping it, so
// one program that cannot resolve one plugin fails the WHOLE build, with a
// message about a plugin it had no business loading.
//
// ⚠️ This asserts the RULE, not the fuse call: reaching a real fuse needs an
// aarch64 rootfs, which `go test ./...` must not require. The rule is "the
// options handed to a non-entry program carry no Extra", and it is expressed
// here the same way the code expresses it, which is the most this can honestly
// check without Docker. `e2e/pipeline_test.go` covers the real path.
func TestOnlyTheEntryCarriesPlugins(t *testing.T) {
	base := fuseOptionsFor([]string{"/host/a.so", "/host/b.so"})
	if len(base.Extra) != 2 {
		t.Fatalf("the entry's options lost their plugins: %+v", base.Extra)
	}

	prog := optionsForNonEntryProgram(base)
	if len(prog.Extra) != 0 {
		t.Errorf("a non-entry program was given %d plugin(s) to fuse in: %v.\n"+
			"Every program in the closure would carry every plugin, and any program "+
			"that cannot satisfy one fails the entire build.", len(prog.Extra), prog.Extra)
	}
	// Everything ELSE must survive, or this "fix" would silently drop the shared
	// layout and the library search path -- which would not fail, just produce
	// unshared images that resolve nothing.
	if prog.Layout != base.Layout {
		t.Error("the non-entry options lost the shared layout")
	}
	if len(prog.LibraryPaths) != len(base.LibraryPaths) {
		t.Errorf("the non-entry options lost the library search path: %v", prog.LibraryPaths)
	}
	// ⚠️ THIS ONE CANNOT FAIL TODAY, and saying so is the point.
	//
	// `optionsForNonEntryProgram` takes `fuse.Options` BY VALUE, so the slice
	// header is copied and nothing it does to `opts.Extra` can change the
	// caller's length. Neutralization confirmed it: rewriting the body as
	// `opts.Extra = opts.Extra[:0]` -- a genuine in-place-looking truncation --
	// leaves this assertion green.
	//
	// It is kept as a guard against a future signature change to `*fuse.Options`,
	// where clearing in place WOULD strip the entry's plugins whenever a
	// non-entry program came first in the closure. It must not be cited as
	// evidence that the current code is safe; the value receiver is what makes
	// it safe, and that is what a reviewer should check.
	if len(base.Extra) != 2 {
		t.Error("deriving the non-entry options mutated the caller's copy; the " +
			"entry would then get no plugins at all, depending on closure order")
	}
}

// TestSuspendViaCallOnlyWhenSideModulesAreEmitted pins a rule whose two halves
// fail in opposite directions, neither of them loudly.
//
// Get it WRONG BY OMISSION -- no call form where side modules are emitted --
// and WASIX refuses the side module at load time with
// `Expected import to be a function: 'env'.__ecv_unwinding`, hours after the
// lift that decided it.
//
// Get it WRONG BY EXCESS -- the call form on a flat wasix build -- and nothing
// fails at all. `SuspendViaCall` is part of `TranslateID`, so the whole object
// cache misses and the build silently re-translates for hours to remove an
// import a flat module never had: `ecv_globals.o` DEFINES `__ecv_unwinding`
// (`llvm-nm` says `D`), and a defined symbol cannot become an import.
func TestSuspendViaCallOnlyWhenSideModulesAreEmitted(t *testing.T) {
	cases := []struct {
		name    string
		flag    bool
		profile string
		sideOut string
		want    bool
	}{
		{"the flag alone still works", true, "wasmedge", "", true},
		{"and is not undone by a profile", true, "loopback", "", true},
		{"wasix WITH side modules implies it", false, "wasix", "/tmp/side", true},
		{"wasix WITHOUT side modules does NOT", false, "wasix", "", false},
		{"side modules alone do not imply it", false, "hosted", "/tmp/side", false},
		{"and the default is untouched", false, "wasmedge", "", false},
	}
	for _, tc := range cases {
		got := suspendViaCallFor(tc.flag, tc.profile, tc.sideOut)
		if got != tc.want {
			t.Errorf("%s: suspendViaCallFor(%v, %q, %q) = %v, want %v",
				tc.name, tc.flag, tc.profile, tc.sideOut, got, tc.want)
		}
	}

	// ⚠️ THE ONE THAT COSTS REAL TIME, restated on its own so a failure names
	// the consequence rather than a boolean. Every other row above is cheap to
	// be wrong about; this one is measured in hours of re-translation.
	if suspendViaCallFor(false, "wasix", "") {
		t.Error("a flat --profile wasix build was given --suspend-via-call. " +
			"That changes TranslateID, so every cached object misses and the " +
			"build re-translates from scratch -- to drop an env.__ecv_unwinding " +
			"import that only a PIC side-module link ever has")
	}
}

// TestEntryFromSeedsTakesTheFirstSeedThatIsAProgram.
//
// # The defect this guards
//
// The caller used `seeds[0]`, and `raptormark build <image>` failed at DISCOVERY
// for almost every real image. Verified against the real command 2026-08-26:
//
//	$ raptormark build nginx:alpine --out … --builder …
//	build <image>: the entry /docker-entrypoint.sh is not in the closure
//	(3 programs). …
//
// A real image's ENTRYPOINT is nearly always a shell script, and a script is
// never in the closure -- `image.Closure` seeds FROM it. So `seeds[0]` named a
// path that by construction could not pass the containment check.
//
// ⚠️ The table is TRANSCRIBED from the seven images measured, printed by
// `.agents-workspace/drivers/headroom` with its seed dump. A test that built its
// own seed lists would encode my model of Docker entrypoints rather than what
// these images actually declare.
func TestEntryFromSeedsTakesTheFirstSeedThatIsAProgram(t *testing.T) {
	cases := []struct {
		image   string
		seeds   []string
		closure []string
		want    string
	}{
		// ENTRYPOINT is a script; CMD's program is seeds[1] and IS in the closure.
		{"nginx:alpine",
			[]string{"/docker-entrypoint.sh", "/usr/sbin/nginx"},
			[]string{"/bin/busybox", "/usr/sbin/nginx", "/usr/sbin/nginx-debug"},
			"/usr/sbin/nginx"},
		{"postgres:17",
			[]string{"/usr/local/bin/docker-entrypoint.sh", "/usr/lib/postgresql/17/bin/postgres"},
			[]string{"/usr/bin/apt", "/usr/lib/postgresql/17/bin/postgres", "/usr/local/bin/gosu"},
			"/usr/lib/postgresql/17/bin/postgres"},
		{"redis:7-alpine",
			[]string{"/usr/local/bin/docker-entrypoint.sh", "/usr/local/bin/redis-server"},
			[]string{"/bin/busybox", "/usr/local/bin/redis-server"},
			"/usr/local/bin/redis-server"},
		{"php:8.3-cli",
			[]string{"/usr/local/bin/docker-php-entrypoint", "/usr/local/bin/php"},
			[]string{"/usr/bin/bash", "/usr/local/bin/php"},
			"/usr/local/bin/php"},

		// ❗ CONTROL. A bare program CMD must still choose seeds[0]. This is the
		// half that makes the change strictly widening: every image that built
		// before must pick exactly what it picked before.
		{"debian:trixie-slim",
			[]string{"/usr/bin/bash"},
			[]string{"/usr/bin/bash"},
			"/usr/bin/bash"},
		{"python:3-slim",
			[]string{"/usr/local/bin/python3.14"},
			[]string{"/usr/local/bin/python3.14"},
			"/usr/local/bin/python3.14"},

		// ❗ CONTROL. seeds[0] in the closure WINS even when a later seed is too,
		// so this is "first match", never "last" or "the longest path".
		{"synthetic: two program seeds",
			[]string{"/usr/bin/first", "/usr/bin/second"},
			[]string{"/usr/bin/first", "/usr/bin/second"},
			"/usr/bin/first"},
	}
	for _, c := range cases {
		t.Run(c.image, func(t *testing.T) {
			got, err := EntryFromSeeds(c.seeds, c.closure)
			if err != nil {
				t.Fatalf("EntryFromSeeds(%v, %v): %v", c.seeds, c.closure, err)
			}
			if got != c.want {
				t.Errorf("entry = %q, want %q (seeds %v)", got, c.want, c.seeds)
			}
		})
	}
}

// ❗ AND IT MUST STILL REFUSE. `ruby:3-slim` has ONE seed -- CMD `irb`, a Ruby
// script, and no ENTRYPOINT -- so there is no program seed at all.
//
// ⚠️ This is the half that keeps the fix honest. Falling through to `closure[0]`
// would make every image "work", and `build.go`'s own comment rejects that: a
// guess that is right most of the time is the worst kind. An image that cannot
// name its entry must say so and point at --entry.
func TestEntryFromSeedsRefusesWhenNoSeedIsAProgram(t *testing.T) {
	_, err := EntryFromSeeds(
		[]string{"/usr/local/bin/irb"},
		[]string{"/usr/bin/true", "/usr/local/bin/ruby"},
	)
	if err == nil {
		t.Fatal("entryFromSeeds accepted a seed set with no program in the closure; " +
			"ruby:3-slim's only seed is the irb SCRIPT, and picking closure[0] " +
			"instead would silently build a different program than the image runs")
	}
	// The message has to name --entry, because that is the only way out and the
	// user has no way to derive it from the failure otherwise.
	if !strings.Contains(err.Error(), "--entry") {
		t.Errorf("the refusal does not mention --entry, so it says what went wrong "+
			"and not what to do: %v", err)
	}
}

// TestReportUnresolvedSaysNothingWhenThereIsNothing, and says the count when
// there is.
//
// ⚠️ The silent case is the one worth pinning. `raptormark build` prints a
// handful of lines before a translation that runs for hours; a diagnostic that
// emits a header even when it has nothing to report is one people learn to skip,
// and then it is not a diagnostic.
func TestReportUnresolvedSaysNothingWhenThereIsNothing(t *testing.T) {
	var buf strings.Builder
	reportUnresolved(&buf, nil)
	if buf.String() != "" {
		t.Errorf("reportUnresolved printed %q for an empty list; it must be silent", buf.String())
	}
}

// The report must lead with the COMMAND-kind entries. Those are the ones that
// name a program, and they are what decides whether a closure is missing
// something; a path-shaped argument usually names a config file.
func TestReportUnresolvedLeadsWithComputedExecTargets(t *testing.T) {
	var buf strings.Builder
	reportUnresolved(&buf, []image.Unresolved{
		{Script: "/a.sh", Word: `"$conf_dir/nginx.conf"`, Kind: image.UnresolvedPath},
		{Script: "/b.sh", Word: `"$f"`, Kind: image.UnresolvedCommand},
		{Script: "/c.sh", Word: `$(sed -n 1p /etc/x)`, Kind: image.UnresolvedCommand, Via: []string{"sed"}},
	})
	out := buf.String()
	if !strings.Contains(out, "3 unresolved shell reference(s) in 3 script(s), 2 of them a computed EXEC target") {
		t.Errorf("the summary line does not carry the counts a reader decides on:\n%s", out)
	}
	// ❗ `Via` must reach the output. It is the only part that could ever justify
	// writing a tool emulator, and a report that says "a path was computed" but
	// not BY WHAT cannot answer the question it exists for.
	if !strings.Contains(out, "via sed") {
		t.Errorf("the commands inside $(...) are missing from the output:\n%s", out)
	}
	iCmd := strings.Index(out, `"$f"`)
	iPath := strings.Index(out, `"$conf_dir/nginx.conf"`)
	if iCmd < 0 || iPath < 0 || iCmd > iPath {
		t.Errorf("a path-shaped argument is listed before a computed exec target:\n%s", out)
	}
}
