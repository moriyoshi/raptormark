package image

import (
	"bytes"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Finding what a shell script pulls in, by PARSING it.
//
// # Why this exists
//
// `bareWords` is the only mechanism a script had, and it is a tokenizer over the
// whole file: every `[A-Za-z0-9_.-]{2,63}` run, resolved against PATH. The other
// extractor, `absolutePaths`, requires a NUL terminator (`b[j] == 0`) because it
// was written for a binary's string table -- and a shell script is text, so
// **no literal absolute path in a script was ever followed.**
//
// ❗ THE MECHANISM FAILED IN BOTH DIRECTIONS AT ONCE, measured 2026-08-26:
//
//   - `nginx:alpine` fans out over a directory --
//     `find "/docker-entrypoint.d/" … | while read -r f; do … . "$f" … done`.
//     All four files there are `#!/bin/sh` scripts and one execs `envsubst`.
//     `/usr/bin/envsubst` is a real 67,408-byte ELF, and the closure was three
//     programs: `/bin/busybox`, `/usr/sbin/nginx`, `/usr/sbin/nginx-debug`.
//     At run time that is `execve` -> not in the exec map -> the silent fallback
//     to program 0 (`runtime/src/execmap.rs`).
//   - `/usr/bin/script` WAS in the postgres:17 closure. Nothing runs it: a
//     script mentions some `…/script.sh` path, `bareWords` splits it into
//     components, and `script` resolves against PATH. The literal path is missed
//     while an unrelated binary named after one of its components is admitted.
//
// # What the parser buys over a regex, which is the reason for the dependency
//
// Not "paths that look like paths" -- WORD BOUNDARIES. Four cases a regex gets
// wrong and `mvdan.cc/sh/v3/syntax` gets right:
//
//	PATH=/usr/local/bin:/usr/bin   ONE word, not two paths. Critical: treating
//	                               /usr/bin as a path here would fan the whole
//	                               image into the closure.
//	# see /usr/bin/foo             a comment, not a reference
//	case "$f" in *.sh) … ;;        a pattern, not a path
//	"/some path/x"                 one word despite the space
//
// # ❗ THIS IS ADDITIVE ONLY, DELIBERATELY
//
// The caller unions these results with the UNCHANGED `bareWords` loop. Nothing
// here can remove a program from a closure. That is a scope decision, not an
// oversight: the same measurement shows heavy over-approximation (postgres:17
// admits `apt`, `free`, `login`, `passwd`, `who`, `tabs`, `more`, `mount`,
// `locale`, `perl` -- English words that happen to be Debian binaries), and
// cutting those is a separate change, because a closure that DROPS a program the
// guest execs produces exactly the silent program-0 fallback above.

// shellRefs is what a shell script names, as guest paths.
//
// ⚠️ NO `Words` FIELD, and its absence is deliberate. The plan for this change
// listed one -- command-position bare words, for PATH resolution -- and it would
// have been a field that provably changes nothing: `bareWords` still runs
// unchanged beside this, and its charset (`[A-Za-z0-9_.-]`, length 2..63) is a
// superset of any command word this could report. A declared field whose
// contents cannot alter the result is the shape this tree keeps finding
// (`pipeline.Result.Skipped`, `translate.LinkRequest.toolFlags`). If the
// narrowing half is ever built, command position is where it starts -- and then
// the field earns itself, because `bareWords` will be gone.
type shellRefs struct {
	// Files are literal guest paths the script names: a sourced file, an exec
	// target, or a command invoked by absolute path. The caller pushes them
	// through the same admission rule as everything else, so a path that is not
	// a program or script costs nothing.
	Files []string
	// Dirs are literal guest directories the script names, for the
	// `for f in DIR/*; do . "$f"; done` idiom that `/docker-entrypoint.d`,
	// `/docker-entrypoint-initdb.d` and `/etc/cont-init.d` all use. The caller
	// enumerates each one's direct children.
	Dirs []string
	// Unresolved is what the scanner saw and could NOT turn into a path. See
	// Unresolved.
	Unresolved []Unresolved
}

// Unresolved is a reference a script makes that could not be resolved to a path.
//
// # Why report instead of guessing
//
// The obvious next step after literal paths is to resolve computed ones, and the
// obvious way is to emulate the tools that compute them. Measured 2026-08-26
// across six images (nginx x2, redis, postgres, php, node -- every entrypoint and
// `docker-entrypoint.d` script, 1,223 lines): **37 command substitutions, none of
// which produces a path to an executable.** They produce log prefixes
// (`ME=$(basename "$0")`), a resolver list from `/etc/resolv.conf`, an envsubst
// filter built from `ENVIRON`, and `mkdir -p` subdirectories.
//
// ❗ And two of those inputs -- `/etc/resolv.conf` and the environment -- DO NOT
// EXIST at build time. raptormark is AOT, so a perfect `sed` would still not
// resolve a path that depends on a file the operator mounts at `docker run`.
// The ceiling is not effort.
//
// So rather than pick a tool to emulate from a sample of six, this records what
// was not resolved. The next image reports which tool actually matters, instead
// of somebody guessing.
//
// ⚠️ It is a DIAGNOSTIC, not a defect list. Most entries are fine: a computed
// path may name a config file, or may already be covered by the directory
// fan-out (`"$f"` in nginx's loop is both unresolved HERE and resolved by Dirs --
// this scanner has no way to know that).
type Unresolved struct {
	// Script is the guest path of the script the reference appears in. Set by
	// the caller, which knows it; scanShell only knows the name it was given.
	Script string
	// Word is the reference as written, e.g. `"$f"` or `"$template_dir/$name"`.
	Word string
	// Kind says why it is interesting.
	Kind UnresolvedKind
	// Via are the commands named inside `$(…)` substitutions in the word, in
	// source order. THIS IS THE FIELD THE QUESTION TURNS ON: it is the measured
	// answer to "which tool would we have to emulate", and it is empty for the
	// far more common case of a bare variable expansion.
	Via []string
}

// UnresolvedKind distinguishes the two references worth knowing about.
type UnresolvedKind string

const (
	// UnresolvedCommand is a word in COMMAND POSITION that is not a literal:
	// the script execs something it computed. `"$f"` in nginx's
	// `/docker-entrypoint.d` loop is the archetype.
	UnresolvedCommand UnresolvedKind = "command"
	// UnresolvedPath is an argument that contains a `/` and an expansion, so it
	// is path-shaped and might have named a file worth following.
	UnresolvedPath UnresolvedKind = "path"
)

// positionalOnly reports whether a word is nothing but positional parameters --
// `"$@"`, `"$1"`, `"$0"`.
//
// ❗ These are EXCLUDED from the report, deliberately. `exec "$@"` ends almost
// every container entrypoint, and its target is the image's CMD, which
// `pipeline.EntryFromSeeds` already resolves. Reporting it on every image would
// put a known, handled case at the top of every run, and a diagnostic whose
// first line is always noise is one people stop reading.
func positionalOnly(w *syntax.Word) bool {
	if len(w.Parts) == 0 {
		return false
	}
	isPositional := func(p syntax.WordPart) bool {
		pe, ok := p.(*syntax.ParamExp)
		if !ok || pe.Param == nil || pe.Excl || pe.Length || pe.Width {
			return false
		}
		if pe.Index != nil || pe.Slice != nil || pe.Repl != nil || pe.Exp != nil {
			return false
		}
		name := pe.Param.Value
		if name == "@" || name == "*" {
			return true
		}
		return len(name) == 1 && name[0] >= '0' && name[0] <= '9'
	}
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.ParamExp:
			if !isPositional(p) {
				return false
			}
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if !isPositional(inner) {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// shellInterpreters are the #! interpreters whose scripts this can parse.
//
// ⚠️ A `#!/usr/bin/python3` script must NOT be handed to a shell parser: it
// would fail, and a failure here is silent by design (the caller falls back to
// `bareWords`). Failing silently on a file that was never shell is fine; doing
// it because nobody checked the shebang is how a whole class goes unnoticed.
// `zsh` is absent because the parser does not implement it.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ash": true, "busybox": true,
	"ksh": true, "mksh": true,
}

// shellLangFor picks a parser variant from a script's #! line, and reports
// whether the script is shell at all.
//
// `#!/usr/bin/env bash` puts the real interpreter in InterpArg, which is the
// same reason `Closure` already resolves that argument against PATH.
func shellLangFor(s Script) (syntax.LangVariant, bool) {
	name := path.Base(s.Interp)
	if name == "env" && s.InterpArg != "" {
		name = path.Base(s.InterpArg)
	}
	if !shellInterpreters[name] {
		return 0, false
	}
	switch name {
	case "bash":
		return syntax.LangBash, true
	case "mksh", "ksh":
		return syntax.LangMirBSDKorn, true
	default:
		// POSIX for sh/dash/ash/busybox. ⚠️ NOT LangBash as a catch-all: parsing
		// a dash script as bash would ACCEPT bashisms that the real interpreter
		// would reject, which is over-permissive in the one direction that
		// matters -- it would report references from a branch that can never run.
		return syntax.LangPOSIX, true
	}
}

// scanShell parses a shell script and returns the paths and directories it
// names. `name` is the script's own guest path; relative references are resolved
// against its directory.
//
// An unparseable script is an error, and the caller's correct response is to
// fall back to `bareWords` alone -- which is what every script got before this
// existed.
func scanShell(src []byte, name string, lang syntax.LangVariant) (shellRefs, error) {
	f, err := syntax.NewParser(syntax.Variant(lang)).Parse(bytes.NewReader(src), name)
	if err != nil {
		return shellRefs{}, err
	}
	dir := path.Dir(name)

	var refs shellRefs
	seen := map[string]bool{}
	addFile := func(p string) {
		if p == "" || seen["f:"+p] {
			return
		}
		seen["f:"+p] = true
		refs.Files = append(refs.Files, p)
	}
	addDir := func(p string) {
		if p == "" || seen["d:"+p] {
			return
		}
		seen["d:"+p] = true
		refs.Dirs = append(refs.Dirs, p)
	}

	addUnresolved := func(w *syntax.Word, kind UnresolvedKind) {
		if positionalOnly(w) {
			return
		}
		text := printWord(w)
		if text == "" || seen["u:"+string(kind)+text] {
			return
		}
		seen["u:"+string(kind)+text] = true
		refs.Unresolved = append(refs.Unresolved,
			Unresolved{Word: text, Kind: kind, Via: substitutedCommands(w)})
	}

	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		// What could NOT be resolved, before the cases that can be. Two shapes
		// only -- reporting every non-literal word would bury the two that matter
		// under `"$1"`, `"${VAR:-}"` and every other ordinary expansion.
		if len(call.Args) > 0 {
			if cmd := call.Args[0]; wordLiteral(cmd) == "" {
				addUnresolved(cmd, UnresolvedCommand)
			}
		}
		for _, w := range call.Args[min(1, len(call.Args)):] {
			if wordLiteral(w) != "" {
				continue
			}
			// Path-SHAPED: a separator somewhere in its literal parts, and no
			// whitespace. A bare `"$x"` is not worth reporting; `"$dir/conf"` is.
			//
			// ❗ THE WHITESPACE RULE IS WHAT MAKES THE REPORT READABLE, and it was
			// added after running it. nginx:latest produced 25 entries of which
			// ONE was a computed exec target; the rest were `entrypoint_log`
			// arguments -- `"$ME: info: /$DEFAULT_CONF_FILE is not a file or does
			// not exist"` -- human-readable messages that happen to contain a
			// slash. A diagnostic that buries its one real finding under two
			// dozen log strings is one nobody reads twice.
			//
			// ⚠️ UNLESS it contains a `$(…)`. Those are reported whatever they
			// look like, because the commands inside are the entire measurement
			// this reporting exists to collect, and the awk case that answers it
			// happens to be full of spaces.
			if !hasLiteralSlash(w) {
				continue
			}
			if hasLiteralSpace(w) && len(substitutedCommands(w)) == 0 {
				continue
			}
			addUnresolved(w, UnresolvedPath)
		}
		// Every word of a simple command, INCLUDING the arguments. The directory
		// this exists for is an argument, not a command:
		//   find "/docker-entrypoint.d/" -follow -type f -print
		// Restricting to Args[0] would miss the case that motivated the change.
		//
		// ⚠️ `Assigns` is deliberately NOT walked. `PATH=/usr/local/bin:/usr/bin`
		// parses as an assignment whose value is one word; reading paths out of
		// it would offer /usr/bin for directory fan-out and drag the entire image
		// into the closure.
		for _, w := range call.Args {
			lit := wordLiteral(w)
			if lit == "" || !path.IsAbs(lit) {
				continue
			}
			clean := path.Clean(lit)
			addFile(clean)
			addDir(clean)
		}
		// A RELATIVE source or exec target, resolved against the script's own
		// directory. `. /abs/lib.sh` is already covered above; `. ./lib.sh` and
		// `. lib.sh` are not, and they are the spellings a script uses for a
		// helper that ships beside it.
		//
		// ❗ Only for these three builtins. A relative word in any other command
		// is far more likely to be a data file than a program, and admitting
		// every one of them would re-create `bareWords`' problem with a longer
		// program.
		if len(call.Args) >= 2 {
			switch call.Args[0].Lit() {
			case ".", "source", "exec":
				if arg := wordLiteral(call.Args[1]); arg != "" && !path.IsAbs(arg) {
					addFile(path.Join(dir, arg))
				} else if arg == "" {
					// ❗ `exec "$CMD"` / `. "$f"`: command position is the literal
					// builtin, so the target sits in ARGUMENT position and the
					// path-shaped rule above misses it -- `"$CMD"` has no literal
					// separator. This is the single most interesting shape there
					// is, so it is reported as a command rather than a path.
					//
					// ⚠️ Found by a positive control, not by reading. The first
					// version of this reporter returned nothing for `exec "$CMD"`
					// while correctly staying silent on `exec "$@"`, which made
					// the positional-parameter exclusion indistinguishable from a
					// reporter that never fires.
					addUnresolved(call.Args[1], UnresolvedCommand)
				}
			}
		}
		return true
	})
	return refs, nil
}

// printWord renders a word back to source, so a report can quote the reference
// as the script actually spells it rather than describing it.
func printWord(w *syntax.Word) string {
	var b strings.Builder
	if err := syntax.NewPrinter().Print(&b, w); err != nil {
		return ""
	}
	return b.String()
}

// hasLiteralSlash reports whether any LITERAL part of a word contains a path
// separator. `"$dir/conf"` does; `"$path"` does not, even though its value might.
func hasLiteralSlash(w *syntax.Word) bool {
	found := false
	syntax.Walk(w, func(n syntax.Node) bool {
		if lit, ok := n.(*syntax.Lit); ok && strings.Contains(lit.Value, "/") {
			found = true
		}
		return !found
	})
	return found
}

// hasLiteralSpace reports whether any LITERAL part of a word contains
// whitespace. A log message does; a path almost never does.
func hasLiteralSpace(w *syntax.Word) bool {
	found := false
	syntax.Walk(w, func(n syntax.Node) bool {
		if lit, ok := n.(*syntax.Lit); ok && strings.ContainsAny(lit.Value, " \t") {
			found = true
		}
		return !found
	})
	return found
}

// substitutedCommands returns the command names run inside `$(…)` within a word.
//
// ❗ This is the measurement the reporting exists for. "Should we emulate sed and
// awk?" is answerable only from real images, and this is the field that answers
// it: if a future image's unresolved EXEC targets come from `$(sed …)`, that is
// evidence. Six images produced no such case -- their substitutions run
// `basename`, `id`, `dirname`, `date`, `awk`, `printf`, and none of the results
// is a program path.
func substitutedCommands(w *syntax.Word) []string {
	var out []string
	seen := map[string]bool{}
	syntax.Walk(w, func(n syntax.Node) bool {
		cs, ok := n.(*syntax.CmdSubst)
		if !ok {
			return true
		}
		for _, st := range cs.Stmts {
			call, ok := st.Cmd.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				continue
			}
			name := call.Args[0].Lit()
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
		return true
	})
	return out
}

// wordLiteral returns a word's value when the whole word is literal text,
// including single- and double-quoted runs, and "" otherwise.
//
// ⚠️ `syntax.Word.Lit` is NOT sufficient: it returns "" for any word that is not
// made of bare `*Lit` parts, so it gives "" for `"/docker-entrypoint.d/"` -- the
// exact word this change exists to read. Quoting a path is the normal way to
// write one.
//
// ❗ Anything with an expansion in it returns "": `"$dir/x"`, `${FOO}`,
// `$(cmd)`. Those are what the directory fan-out is for. Guessing at a partial
// expansion would produce paths that do not exist and, worse, occasionally ones
// that do.
func wordLiteral(w *syntax.Word) string {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return ""
				}
				b.WriteString(lit.Value)
			}
		default:
			return ""
		}
	}
	return b.String()
}
