package image

import (
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// nginxEntrypoint is TRANSCRIBED from `nginx:alpine`'s /docker-entrypoint.sh,
// not written to suit the scanner.
//
// ⚠️ That distinction is the whole value of this fixture. A script invented to
// exercise the implementation would encode my model of what entrypoints look
// like; this is what the image actually ships, and the construct that matters --
// a directory named as an ARGUMENT to `find`, with the scripts themselves
// reached only through `$f` -- is one I would not have thought to write.
const nginxEntrypoint = `#!/bin/sh
set -e

entrypoint_log() {
    if [ -z "${NGINX_ENTRYPOINT_QUIET_LOGS:-}" ]; then
        echo "$@"
    fi
}

if [ "$1" = "nginx" ] || [ "$1" = "nginx-debug" ]; then
    if /usr/bin/find "/docker-entrypoint.d/" -mindepth 1 -maxdepth 1 -type f -print -quit 2>/dev/null | read v; then
        entrypoint_log "$0: Looking for shell scripts in /docker-entrypoint.d/"
        find "/docker-entrypoint.d/" -follow -type f -print | sort -V | while read -r f; do
            case "$f" in
                *.envsh)
                    if [ -x "$f" ]; then
                        . "$f"
                    fi
                    ;;
                *.sh)
                    if [ -x "$f" ]; then
                        "$f"
                    fi
                    ;;
            esac
        done
    fi
fi

exec "$@"
`

// envsubstScript stands for /docker-entrypoint.d/20-envsubst-on-templates.sh.
// The real one is 3,030 bytes; what matters is that it invokes envsubst, which
// on nginx:alpine is a real 67,408-byte ELF and not a busybox applet.
const envsubstScript = `#!/bin/sh
set -e
auto_envsubst() {
  local template_dir="${NGINX_ENVSUBST_TEMPLATE_DIR:-/etc/nginx/templates}"
  envsubst "$defined_envs" < "$template" > "$output_path"
}
auto_envsubst
`

// nginxInventory builds the nginx:alpine shape: an entrypoint that fans out over
// /docker-entrypoint.d, four scripts in it, and the programs they reach.
func nginxInventory(t *testing.T) *Inventory {
	t.Helper()
	dir := t.TempDir()
	prog := func(guest, stem string) (string, Executable) {
		return guest, Executable{GuestPath: guest, HostPath: writeFile(t, dir, stem, "", 0o755)}
	}
	script := func(guest, stem, body string) (string, Script) {
		return guest, Script{
			GuestPath: guest, HostPath: writeFile(t, dir, stem, body, 0o755), Interp: "/bin/sh",
		}
	}
	programs := map[string]Executable{}
	for _, p := range [][2]string{
		{"/bin/sh", "sh.bin"},
		{"/bin/busybox", "busybox.bin"},
		{"/usr/sbin/nginx", "nginx.bin"},
		{"/usr/bin/envsubst", "envsubst.bin"},
		// Present in the image and referenced by nothing. If it turns up in the
		// closure, the directory fan-out is not bounded to direct children of the
		// directory that was actually named.
		{"/usr/bin/unreferenced", "unref.bin"},
	} {
		g, e := prog(p[0], p[1])
		programs[g] = e
	}
	scripts := map[string]Script{}
	for _, s := range [][3]string{
		{"/docker-entrypoint.sh", "entry.sh", nginxEntrypoint},
		{"/docker-entrypoint.d/10-listen-on-ipv6-by-default.sh", "d10.sh", "#!/bin/sh\nexit 0\n"},
		{"/docker-entrypoint.d/15-local-resolvers.envsh", "d15.envsh", "#!/bin/sh\nexit 0\n"},
		{"/docker-entrypoint.d/20-envsubst-on-templates.sh", "d20.sh", envsubstScript},
		{"/docker-entrypoint.d/30-tune-worker-processes.sh", "d30.sh", "#!/bin/sh\nexit 0\n"},
	} {
		g, sc := script(s[0], s[1], s[2])
		scripts[g] = sc
	}
	return &Inventory{Programs: programs, Scripts: scripts, Links: map[string]string{}, DirLinks: map[string]string{}}
}

// TestClosureFollowsADirectoryFanOut is the case this change exists for.
//
// # What was measured
//
// On the real `nginx:alpine`, `/usr/bin/envsubst` is a 67,408-byte ELF that
// `/docker-entrypoint.d/20-envsubst-on-templates.sh` execs, and the closure was
// THREE programs -- `/bin/busybox`, `/usr/sbin/nginx`, `/usr/sbin/nginx-debug`.
// envsubst was absent, so at run time the guest's `execve` finds no exec-map
// entry and falls back to program 0, silently.
//
// # Why it was missed
//
// The four scripts are reached only through `$f`, whose value comes from `find`.
// Nothing names them literally. `bareWords` cannot help: it splits
// `/docker-entrypoint.d/` into `docker-entrypoint`, `d` and resolves those
// against PATH, which finds nothing. The literal that IS present is the
// DIRECTORY, and it appears as an argument to `find`, not in command position.
func TestClosureFollowsADirectoryFanOut(t *testing.T) {
	inv := nginxInventory(t)
	got, _, err := Closure(inv, ClosureOptions{
		Seeds:    []string{"/docker-entrypoint.sh"},
		PathDirs: []string{"/usr/bin", "/bin", "/usr/sbin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "/usr/bin/envsubst") {
		t.Errorf("closure is missing /usr/bin/envsubst: %v\n\n"+
			"It is reached only through /docker-entrypoint.d/20-envsubst-on-templates.sh, "+
			"which the entrypoint runs as \"$f\" after finding it with `find`. Nothing "+
			"names that script literally, so the only literal to work from is the "+
			"DIRECTORY -- and it appears as an ARGUMENT to find, not in command "+
			"position. A closure without envsubst gives the guest an execve that "+
			"falls back to program 0 with no diagnostic.", got)
	}
	// The fan-out must be bounded to the directory that was named.
	if slices.Contains(got, "/usr/bin/unreferenced") {
		t.Errorf("closure pulled in /usr/bin/unreferenced: %v\n"+
			"Nothing names it. If the fan-out reached it, it is not bounded to the "+
			"direct children of the directory the script actually named.", got)
	}
}

// ❗ THE PROPERTY THE WHOLE SCOPE DECISION RESTS ON: this change may only ADD.
//
// The shell scan is unioned with the UNCHANGED `bareWords` pass, so no closure
// can lose a member. That is easy to assert and worth checking instead, because
// the failure it guards is silent: a closure that DROPS a program the guest
// execs produces the same program-0 fallback as the bug above, in the opposite
// direction and with no error anywhere.
//
// ⚠️ The expectation comes from `bareWordClosure` below -- an independent
// reimplementation of the algorithm as it stood BEFORE this change -- not from a
// disable switch in `Closure`. A test-only flag in production code would be a
// second path nothing else exercises; a reference implementation in the test is
// the thing being compared against, and it doubles as a record of what changed.
//
// ❗ AN EARLIER VERSION OF THIS TEST WAS WRONG, and neutralization is what caught
// it. It computed the expectation by running `bareWords` over EVERY script in the
// inventory, including the four that only the directory fan-out can reach. So it
// was re-asserting reachability -- duplicating the test above -- rather than the
// widening property, and it FAILED when `scanShell` was stubbed to error, which
// is precisely the case where the bare-word fallback is supposed to carry the
// closure unharmed. The prediction "this one still passes" is what exposed it.
func TestShellScanOnlyEverWidensTheClosure(t *testing.T) {
	pathDirs := []string{"/usr/bin", "/bin", "/usr/sbin"}
	for _, tc := range []struct {
		name string
		inv  *Inventory
		seed string
	}{
		{"nginx fan-out", nginxInventory(t), "/docker-entrypoint.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := ClosureOptions{Seeds: []string{tc.seed}, PathDirs: pathDirs}
			got, _, err := Closure(tc.inv, opts)
			if err != nil {
				t.Fatal(err)
			}
			before := bareWordClosure(t, tc.inv, opts)
			if len(before) == 0 {
				t.Fatal("the reference implementation admitted NOTHING, so this superset " +
					"check would pass over an empty set and prove nothing")
			}
			for _, want := range before {
				if !slices.Contains(got, want) {
					t.Errorf("the closure LOST %q, which the pre-change algorithm finds.\n"+
						"  now:    %v\n  before: %v\n"+
						"This change is additive by construction -- the shell scan is unioned "+
						"with an unchanged bareWords loop -- so a missing member means that "+
						"union was broken.", want, got, before)
				}
			}
		})
	}
}

// bareWordClosure is `Closure` AS IT STOOD before the shell scan: seeds, the #!
// interpreter and its `env` argument, NUL-terminated absolute paths, and every
// bare word resolved against PATH. Nothing else.
//
// ⚠️ A deliberate second copy, and the only one in this tree that should exist.
// It is not a helper the production code could share: its whole purpose is to
// keep behaving the way production no longer does, so that "we only added" is
// something a test computes rather than something a comment claims.
func bareWordClosure(t *testing.T, inv *Inventory, opts ClosureOptions) []string {
	t.Helper()
	programs := map[string]bool{}
	visited := map[string]bool{}
	var queue []string
	push := func(p string) {
		if p == "" || opts.Exclude[p] {
			return
		}
		p = canon(inv, p)
		if visited[p] {
			return
		}
		_, isProg := inv.Programs[p]
		_, isScript := inv.Scripts[p]
		if !isProg && !isScript {
			return
		}
		visited[p] = true
		if isProg {
			programs[p] = true
		}
		queue = append(queue, p)
	}
	for _, s := range opts.Seeds {
		push(s)
	}
	for i := 0; i < len(queue); i++ {
		p := queue[i]
		e, isProgram := inv.Programs[p]
		hostPath := e.HostPath
		isScript := false
		if !isProgram {
			s := inv.Scripts[p]
			hostPath, isScript = s.HostPath, true
			push(s.Interp)
			if s.InterpArg != "" && !strings.Contains(s.InterpArg, "/") {
				for _, q := range resolveInPath(s.InterpArg, opts.PathDirs, inv) {
					push(q)
				}
			}
		}
		b, err := os.ReadFile(hostPath)
		if err != nil {
			continue
		}
		for _, cand := range absolutePaths(b) {
			push(cand)
		}
		if isScript {
			for _, word := range bareWords(b) {
				for _, q := range resolveInPath(word, opts.PathDirs, inv) {
					push(q)
				}
			}
		}
	}
	out := make([]string, 0, len(programs))
	for p := range programs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ❗ A NON-SHELL SCRIPT MUST BE UNAFFECTED. `scanShell` is only correct for
// shell; handing it a python script would fail to parse, and a parse failure is
// silent by design. Silence is right for a file that was never shell -- but only
// if the shebang is checked, rather than every script being thrown at the parser
// and most of them failing unnoticed.
func TestClosureLeavesNonShellScriptsToTheBareWordPass(t *testing.T) {
	dir := t.TempDir()
	inv := &Inventory{
		Programs: map[string]Executable{
			"/usr/bin/python3": {GuestPath: "/usr/bin/python3", HostPath: writeFile(t, dir, "py.bin", "", 0o755)},
			"/usr/bin/gosu":    {GuestPath: "/usr/bin/gosu", HostPath: writeFile(t, dir, "gosu.bin", "", 0o755)},
		},
		Scripts: map[string]Script{
			// Valid python, and NOT valid shell: `f"…"` and the `:=` walrus would
			// both be parse errors, which is the point.
			"/app.py": {GuestPath: "/app.py", Interp: "/usr/bin/python3", HostPath: writeFile(t, dir,
				"app.py", "#!/usr/bin/python3\nimport os\nif (n := 1): os.system(f\"gosu {n}\")\n", 0o755)},
		},
		Links: map[string]string{}, DirLinks: map[string]string{},
	}
	got, _, err := Closure(inv, ClosureOptions{Seeds: []string{"/app.py"}, PathDirs: []string{"/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/usr/bin/python3", "/usr/bin/gosu"} {
		if !slices.Contains(got, want) {
			t.Errorf("closure missing %q for a python script: %v\n"+
				"The interpreter and the bare-word pass must both still work; only the "+
				"shell parse is skipped.", want, got)
		}
	}
}

// ❗ POSITIVE CONTROL for the scanner itself.
//
// Every assertion above is about a closure, which has other ways to be populated.
// This one pins that `scanShell` returns something at all: a scanner that
// silently returned an empty result would leave every test above passing on the
// `bareWords` fallback alone, and the feature would be dead while green. That is
// the failure mode AGENTS.md flags for every scan-based guard in this tree.
func TestScanShellFindsSourcedAndExecutedPaths(t *testing.T) {
	src := []byte(`#!/bin/sh
. /usr/local/lib/common.sh
source "/etc/defaults.sh"
exec /usr/bin/setpriv --reuid redis "$0" "$@"
/opt/tool/run --flag
. ./sibling.sh
PATH=/usr/local/bin:/usr/bin
cd /var/lib/postgresql
`)
	refs, err := scanShell(src, "/docker-entrypoint.sh", syntax.LangPOSIX)
	if err != nil {
		t.Fatalf("scanShell: %v", err)
	}
	for _, want := range []string{
		"/usr/local/lib/common.sh", // . with a literal path
		"/etc/defaults.sh",         // source, DOUBLE QUOTED -- Word.Lit returns "" here
		"/usr/bin/setpriv",         // exec target
		"/opt/tool/run",            // invoked by absolute path
		"/sibling.sh",              // relative source, against the script's own dir
	} {
		if !slices.Contains(refs.Files, want) {
			t.Errorf("scanShell missed %q: %v", want, refs.Files)
		}
	}
	// ❗ THE ASSIGNMENT MUST NOT YIELD A PATH. `PATH=/usr/local/bin:/usr/bin`
	// parses as ONE word that does not start with '/', which is exactly why the
	// parser is here rather than a regex: offering /usr/bin for directory fan-out
	// would drag every executable in the image into the closure.
	for _, forbidden := range []string{"/usr/local/bin", "/usr/bin"} {
		if slices.Contains(refs.Dirs, forbidden) {
			t.Errorf("scanShell offered %q for fan-out from a PATH= assignment: %v\n"+
				"A regex over the same text would find it. This is the case the parser "+
				"exists for.", forbidden, refs.Dirs)
		}
	}
}

// A comment naming a path must not become a reference. The second reason the
// parser is here: `bareWords` reads comments, and the postgres closure contains
// `/usr/bin/script` because something mentions a `…/script.sh` path in passing.
func TestScanShellIgnoresComments(t *testing.T) {
	refs, err := scanShell([]byte("#!/bin/sh\n# see /usr/bin/apt for details\ntrue\n"),
		"/x.sh", syntax.LangPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(refs.Files, "/usr/bin/apt") {
		t.Errorf("a path in a COMMENT became a reference: %v", refs.Files)
	}
}

// shellLangFor decides whether a script is parsed at all, so a wrong answer
// either wastes a parse or silently skips a real shell script.
func TestShellLangForReadsTheShebang(t *testing.T) {
	cases := []struct {
		interp, arg string
		want        syntax.LangVariant
		isShell     bool
	}{
		{"/bin/sh", "", syntax.LangPOSIX, true},
		{"/bin/dash", "", syntax.LangPOSIX, true},
		{"/bin/ash", "", syntax.LangPOSIX, true},
		{"/bin/bash", "", syntax.LangBash, true},
		{"/usr/bin/env", "bash", syntax.LangBash, true}, // the env idiom
		{"/usr/bin/env", "sh", syntax.LangPOSIX, true},  // and its POSIX form
		{"/usr/bin/python3", "", 0, false},              // not shell
		{"/usr/bin/env", "python3", 0, false},           // not shell, via env
		{"/usr/bin/perl", "", 0, false},                 // not shell
	}
	for _, c := range cases {
		got, ok := shellLangFor(Script{Interp: c.interp, InterpArg: c.arg})
		if ok != c.isShell {
			t.Errorf("shellLangFor(%q, %q) isShell = %v, want %v", c.interp, c.arg, ok, c.isShell)
			continue
		}
		if ok && got != c.want {
			t.Errorf("shellLangFor(%q, %q) = %v, want %v", c.interp, c.arg, got, c.want)
		}
	}
}

// entriesUnder is the fan-out, and its refusals are the load-bearing part.
func TestEntriesUnderRefusesPathDirectoriesAndRecursion(t *testing.T) {
	inv := &Inventory{
		Programs: map[string]Executable{
			"/usr/bin/ls":                   {},
			"/usr/bin/cat":                  {},
			"/docker-entrypoint.d/a":        {},
			"/docker-entrypoint.d/nested/b": {},
		},
		Scripts: map[string]Script{"/docker-entrypoint.d/c.sh": {}},
	}
	pathDirs := []string{"/usr/bin", "/bin"}

	got := entriesUnder(inv, "/docker-entrypoint.d", pathDirs)
	want := []string{"/docker-entrypoint.d/a", "/docker-entrypoint.d/c.sh"}
	if !slices.Equal(got, want) {
		t.Errorf("entriesUnder = %v, want %v (direct children only, sorted)", got, want)
	}
	// ❗ A PATH DIRECTORY MUST YIELD NOTHING. Scripts say `cd /usr/bin` and
	// `ls /usr/bin` constantly; fanning one out admits every executable in the
	// image, and the closure keeps looking like it worked.
	if got := entriesUnder(inv, "/usr/bin", pathDirs); got != nil {
		t.Errorf("entriesUnder fanned out a PATH directory: %v", got)
	}
	if got := entriesUnder(inv, "/", pathDirs); got != nil {
		t.Errorf("entriesUnder fanned out the root: %v", got)
	}
}

// TestScanShellReportsWhatItCouldNotResolve.
//
// # Why this exists
//
// Asked 2026-08-26: should the scanner emulate sed/awk/printf so it can resolve
// computed paths? Measured across six images -- every entrypoint and
// docker-entrypoint.d script, 1,223 lines -- **37 command substitutions, none of
// which produces a path to an executable**. They produce log prefixes
// (`ME=$(basename "$0")`), a resolver list read from /etc/resolv.conf, an
// envsubst filter built from ENVIRON, and `mkdir -p` subdirectories.
//
// Six images is a sample, not a census. So instead of picking a tool to emulate
// from that sample, the scanner REPORTS what it could not resolve, and `Via`
// carries the commands inside `$(…)`. The next image answers the question with
// evidence.
func TestScanShellReportsWhatItCouldNotResolve(t *testing.T) {
	src := []byte(`#!/bin/sh
"$f"
$(command -v runner)
tool="$(sed -n 1p /etc/conf)"
cp "$template_dir/nginx.conf" /etc/nginx/nginx.conf
echo "$plain"
`)
	refs, err := scanShell(src, "/entry.sh", syntax.LangPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	byWord := map[string]Unresolved{}
	for _, u := range refs.Unresolved {
		byWord[u.Word] = u
	}

	// A computed EXEC target -- the shape that would justify emulating anything.
	if got, ok := byWord[`"$f"`]; !ok || got.Kind != UnresolvedCommand {
		t.Errorf(`"$f" not reported as a computed exec target: %+v`, refs.Unresolved)
	}
	// ❗ THE FIELD THE WHOLE QUESTION TURNS ON. If a future image's unresolved
	// exec target reports `via sed`, that is the evidence for emulating sed --
	// and until one does, there is none.
	if got, ok := byWord[`$(command -v runner)`]; !ok {
		t.Errorf(`$(command -v runner) not reported: %+v`, refs.Unresolved)
	} else if !slices.Contains(got.Via, "command") {
		t.Errorf(`Via for $(command -v runner) = %v, want it to name "command". `+
			`Without Via the report says a path was computed but not by WHAT, `+
			`which is the only part that could justify writing an emulator.`, got.Via)
	}
	// Path-SHAPED argument: it has a literal separator, so it might have named a
	// file worth following.
	if got, ok := byWord[`"$template_dir/nginx.conf"`]; !ok || got.Kind != UnresolvedPath {
		t.Errorf("a path-shaped argument was not reported as a path: %+v", refs.Unresolved)
	}
	// ❗ AND A BARE EXPANSION IN ARGUMENT POSITION MUST NOT BE. `echo "$plain"` is
	// not a path by any reading, and reporting every expansion would bury the two
	// shapes that matter under every ordinary variable in the script.
	if _, ok := byWord[`"$plain"`]; ok {
		t.Errorf(`"$plain" was reported; a bare expansion in argument position is `+
			`not path-shaped and reporting it makes the diagnostic unreadable: %+v`,
			refs.Unresolved)
	}
}

// ❗ `exec "$@"` MUST NOT BE REPORTED, and this is a judgment call worth pinning.
//
// It ends almost every container entrypoint, and its target is the image's CMD,
// which `pipeline.EntryFromSeeds` already resolves. Reporting it would put a
// known, handled case at the top of every single run -- and a diagnostic whose
// first line is always noise is one people stop reading, which costs more than
// the entry is worth.
func TestScanShellDoesNotReportPositionalParameters(t *testing.T) {
	refs, err := scanShell([]byte("#!/bin/sh\nexec \"$@\"\n\"$1\" --flag\nexec \"$0\"\n"),
		"/entry.sh", syntax.LangPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range refs.Unresolved {
		t.Errorf("positional parameter reported as unresolved: %+v\n"+
			"`exec \"$@\"` ends nearly every entrypoint and is already resolved by "+
			"pipeline.EntryFromSeeds from the image CMD.", u)
	}

	// ❗ POSITIVE CONTROL, in the same test. An empty result is what a broken
	// reporter also returns, so the exclusion above proves nothing on its own:
	// the SAME scanner must report a non-positional expansion in the same
	// position.
	refs, err = scanShell([]byte("#!/bin/sh\nexec \"$CMD\"\n"), "/entry.sh", syntax.LangPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Unresolved) == 0 {
		t.Error(`exec "$CMD" reported nothing. The positional exclusion is then ` +
			`indistinguishable from a reporter that never fires, and the test above ` +
			`passes for the wrong reason.`)
	}
}

// The report must cover only scripts the closure actually REACHED. A diagnostic
// listing references from files nothing runs is one nobody can act on.
func TestClosureReportsUnresolvedOnlyForReachedScripts(t *testing.T) {
	dir := t.TempDir()
	inv := &Inventory{
		Programs: map[string]Executable{
			"/bin/sh": {GuestPath: "/bin/sh", HostPath: writeFile(t, dir, "sh.bin", "", 0o755)},
		},
		Scripts: map[string]Script{
			"/entry.sh": {GuestPath: "/entry.sh", Interp: "/bin/sh",
				HostPath: writeFile(t, dir, "entry.sh", "#!/bin/sh\n\"$reached\"\n", 0o755)},
			// Never referenced by anything.
			"/orphan.sh": {GuestPath: "/orphan.sh", Interp: "/bin/sh",
				HostPath: writeFile(t, dir, "orphan.sh", "#!/bin/sh\n\"$orphaned\"\n", 0o755)},
		},
		Links: map[string]string{}, DirLinks: map[string]string{},
	}
	_, unresolved, err := Closure(inv, ClosureOptions{Seeds: []string{"/entry.sh"}, PathDirs: []string{"/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	var sawReached, sawOrphan bool
	for _, u := range unresolved {
		if u.Script != "/entry.sh" && u.Script != "/orphan.sh" {
			t.Errorf("unresolved entry has no script attributed: %+v", u)
		}
		sawReached = sawReached || u.Script == "/entry.sh"
		sawOrphan = sawOrphan || u.Script == "/orphan.sh"
	}
	if !sawReached {
		t.Errorf(`nothing reported for /entry.sh, which execs "$reached": %+v`, unresolved)
	}
	if sawOrphan {
		t.Errorf("reported a reference from /orphan.sh, which nothing runs: %+v", unresolved)
	}
}

// ❗ A LOG MESSAGE IS NOT A PATH, and the report is unreadable without this.
//
// Measured by running the reporter before this filter existed: nginx:latest
// produced 25 unresolved references of which exactly ONE was a computed exec
// target. The other 24 were `entrypoint_log` arguments -- messages that contain
// a slash because they quote a filename. A diagnostic that buries its one real
// finding under two dozen log strings gets read once.
func TestScanShellDoesNotReportLogMessagesAsPaths(t *testing.T) {
	// Transcribed from nginx:latest's 10-listen-on-ipv6-by-default.sh.
	src := []byte(`#!/bin/sh
entrypoint_log "$ME: info: /$DEFAULT_CONF_FILE is not a file or does not exist"
mkdir -p "$output_dir/$subdir"
defined_envs=$(printf '${%s} ' $(awk -v filter="$filter" 'END { print name }' </dev/null))
`)
	refs, err := scanShell(src, "/x.sh", syntax.LangPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range refs.Unresolved {
		if strings.Contains(u.Word, "is not a file or does not exist") {
			t.Errorf("a log MESSAGE was reported as a path: %q", u.Word)
		}
	}
	// ❗ POSITIVE CONTROL. The filter must not have silenced the reporter: a real
	// path-shaped argument in the same script still has to appear.
	var sawPath, sawVia bool
	for _, u := range refs.Unresolved {
		sawPath = sawPath || strings.Contains(u.Word, `"$output_dir/$subdir"`)
		sawVia = sawVia || slices.Contains(u.Via, "awk")
	}
	if !sawPath {
		t.Errorf(`"$output_dir/$subdir" was filtered out too; the whitespace rule is `+
			`dropping real path-shaped arguments: %+v`, refs.Unresolved)
	}
	// ⚠️ And a `$(…)` must survive the filter EVEN THOUGH it is full of spaces.
	// That awk substitution is the one case that answers "which tool would an
	// emulator have to be", so silencing it would remove the measurement while
	// leaving the reporting.
	if !sawVia {
		t.Errorf("the awk substitution was filtered out by the whitespace rule. It "+
			"is the measurement this reporting exists for: %+v", refs.Unresolved)
	}
}
