// Package image discovers what to translate inside a container image: the
// entrypoint, the aarch64 executables present, and the closure of binaries the
// entrypoint can reach by exec.
//
// RECONSTRUCTED on 2026-08-01. Nothing about this layer survived the wipe — the
// packaging policy it implements was chosen deliberately (see
// .agents/docs/LTM/image-discovery-and-rootfs.md), not recovered:
//
//   - Full-fuse execve model: every unit is a self-contained program with its own
//     exec-map path, so runtime/src/execmap.rs reports no library units.
//   - Entrypoint closure: only binaries reachable from the image entrypoint
//     become programs, not every ELF in the filesystem.
package image

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Config is the part of an image's configuration that determines what runs.
type Config struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
}

// Inspect reads an image's configuration.
func Inspect(ctx context.Context, image string) (Config, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect",
		"--format", "{{json .Config}}", image).Output()
	if err != nil {
		return Config{}, fmt.Errorf("image: inspect %s: %w", image, err)
	}
	var raw struct {
		Entrypoint []string
		Cmd        []string
		Env        []string
		WorkingDir string
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Config{}, fmt.Errorf("image: decode config for %s: %w", image, err)
	}
	return Config(raw), nil
}

// ExportRootfs materialises an image's filesystem under dest.
//
// `docker export` flattens a container rather than replaying layers, which is
// what we want: whiteouts are already resolved, so what lands on disk is exactly
// what the guest would see.
func ExportRootfs(ctx context.Context, image, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	create := exec.CommandContext(ctx, "docker", "create", image)
	idOut, err := create.Output()
	if err != nil {
		return fmt.Errorf("image: create container from %s: %w", image, err)
	}
	id := strings.TrimSpace(string(idOut))
	defer exec.Command("docker", "rm", "-f", id).Run()

	export := exec.CommandContext(ctx, "docker", "export", id)
	pipe, err := export.StdoutPipe()
	if err != nil {
		return err
	}
	// Symlinks and hardlinks matter here: guest paths routinely reach binaries
	// through them, and Programs.resolve() consults the VFS for exactly that.
	untar := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", dest)
	untar.Stdin = pipe
	var stderr bytes.Buffer
	untar.Stderr = &stderr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := export.Run(); err != nil {
		return fmt.Errorf("image: export %s: %w", image, err)
	}
	if err := untar.Wait(); err != nil {
		// tar warns about unpackable entries (device nodes, xattrs) as an
		// unprivileged user; those are irrelevant to finding ELFs.
		if !strings.Contains(stderr.String(), "Cannot") && !strings.Contains(stderr.String(), "Exiting with failure") {
			return fmt.Errorf("image: untar %s: %w\n%s", image, err, stderr.String())
		}
	}
	return nil
}

// Executable is an aarch64 program found in the image.
type Executable struct {
	// GuestPath is the absolute path as the guest sees it.
	GuestPath string
	// HostPath is where it lives under the exported rootfs.
	HostPath string
	// Interp is the ELF interpreter, empty for static binaries.
	Interp string
}

// Static reports whether the binary has no ELF interpreter.
func (e Executable) Static() bool { return e.Interp == "" }

// Script is an executable #! script. Scripts are NOT registry programs — the
// runtime's shebang handling execs the *interpreter* and feeds it the script
// file, which comes from the rfs sidecar as data. But a script is reachable by
// execve, so it pulls its interpreter and anything it invokes into the closure.
//
// ❗ THAT SENTENCE WAS FALSE UNTIL 2026-08-28. `runtime/src/sys.rs` returned
// ENOEXEC for a script with the comment "shebang support is a later addition",
// and the boot path fell back to program 0 — so the design this type implements
// rested on a capability the runtime did not have. It was found by building
// postgres:17 end to end and watching the guest print apt's `E: Invalid
// operation postgres`. `runtime/src/shebang.rs` implements it now, on both the
// execve and boot paths, and the sentence above is true as written.
type Script struct {
	GuestPath string
	HostPath  string
	// Interp is the program named on the #! line.
	Interp string
	// InterpArg is the interpreter's first argument, if any. It matters for the
	// `#!/usr/bin/env bash` idiom, where Interp is env and the *real*
	// interpreter is this argument, resolved against PATH.
	InterpArg string
}

// Inventory is everything in an image that can be the target of an exec.
type Inventory struct {
	// Programs are aarch64 ELF executables, keyed by guest path. These become
	// registry programs.
	Programs map[string]Executable
	// Scripts are #! scripts, keyed by guest path.
	Scripts map[string]Script
	// Links maps a symlink's guest path to the guest path of the executable it
	// finally names (/usr/local/bin/python3 -> /usr/local/bin/python3.14).
	//
	// A symlink is NOT itself a registry program: the runtime resolves the path
	// through the VFS before consulting the exec map (runtime/src/execmap.rs,
	// Programs::resolve), so one program serves every name that reaches it.
	// This map exists so host-side discovery can follow the same paths — without
	// it, an image whose entrypoint is a symlink yields no seeds at all.
	Links map[string]string
	// DirLinks maps a symlink's guest path to the guest DIRECTORY it names, for
	// the merged-usr layout every current Debian and Ubuntu uses: `/bin` ->
	// `usr/bin`, `/lib` -> `usr/lib`, and so on.
	//
	// Separate from Links because Links deliberately holds only exec targets,
	// and because these are consumed differently: a directory link is not a
	// path to resolve but a path PREFIX to rewrite. Without it an image whose
	// entrypoint is `/bin/foo` yields no seeds at all -- `/bin/foo` is not
	// itself a link, `/bin` is, and `filepath.WalkDir` does not descend through
	// a directory symlink, so `/bin/foo` is never a key in Programs either.
	DirLinks map[string]string
}

// Scan walks the exported rootfs and inventories exec targets.
//
// Shared libraries are excluded from Programs: in the full-fuse model a program
// is something you can exec, and libraries reach the module fused into their
// user. A .so is ET_DYN with no PT_INTERP, which is how it is told apart from a
// PIE executable (ET_DYN *with* a PT_INTERP).
func Scan(root string) (*Inventory, error) {
	inv := &Inventory{
		Programs: make(map[string]Executable),
		Scripts:  make(map[string]Script),
		Links:    make(map[string]string),
		DirLinks: make(map[string]string),
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are not interesting
		}
		if d.Type()&fs.ModeSymlink != 0 {
			recordLink(inv, root, p)
			recordDirLink(inv, root, p)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		guest := "/" + filepath.ToSlash(rel)
		if interp, ok := aarch64Executable(p); ok {
			inv.Programs[guest] = Executable{GuestPath: guest, HostPath: p, Interp: interp}
			return nil
		}
		if interp, arg, ok := shebang(p); ok {
			inv.Scripts[guest] = Script{GuestPath: guest, HostPath: p, Interp: interp, InterpArg: arg}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// recordLink notes a symlink that reaches an executable file. Links to
// anything else (directories, data files, dangling targets) are ignored — only
// exec targets matter here.
func recordLink(inv *Inventory, root, p string) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return
	}
	src := "/" + filepath.ToSlash(rel)
	dst, ok := resolveGuestLink(root, src)
	if !ok || dst == src {
		return
	}
	fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(dst)))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return
	}
	inv.Links[src] = dst
}

// recordDirLink notes a symlink that names a DIRECTORY, which `recordLink`
// deliberately ignores. These are prefixes to rewrite, not paths to resolve.
func recordDirLink(inv *Inventory, root, p string) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return
	}
	src := "/" + filepath.ToSlash(rel)
	dst, ok := resolveGuestLink(root, src)
	if !ok || dst == src {
		return
	}
	fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(dst)))
	if err != nil || !fi.IsDir() {
		return
	}
	inv.DirLinks[src] = dst
}

// resolveGuestLink follows a symlink chain the way the guest will: an absolute
// target is rootfs-relative, not host-absolute, so /bin/sh -> /bin/busybox
// stays inside the image instead of escaping to the host. The 40-link cap
// matches the runtime VFS (runtime/src/vfs/mod.rs, MAX_LINKS).
//
// Only the final component is followed. An *absolute* symlink standing in for a
// directory mid-path is not resolved; relative ones (debian's /lib -> usr/lib)
// work anyway, because the host resolves them identically.
func resolveGuestLink(root, guest string) (string, bool) {
	cur := guest
	for range 40 {
		host := filepath.Join(root, filepath.FromSlash(cur))
		fi, err := os.Lstat(host)
		if err != nil {
			return "", false
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return cur, true
		}
		tgt, err := os.Readlink(host)
		if err != nil {
			return "", false
		}
		if path.IsAbs(tgt) {
			cur = path.Clean(tgt)
		} else {
			cur = path.Join(path.Dir(cur), tgt)
		}
	}
	return "", false // link loop, or deeper than the guest would follow
}

// canon maps a guest path onto the exec target it names, following one symlink
// hop when the path is not itself a program or script. Inventory.Links already
// holds fully-resolved targets, so a single hop is enough.
func canon(inv *Inventory, p string) string {
	if t, ok := canonExact(inv, p); ok {
		return t
	}
	// Nothing named `p` directly. Try rewriting it through a directory symlink,
	// LONGEST PREFIX FIRST so `/usr/local/bin` wins over `/usr` when both are
	// links. This is the merged-usr case: `/bin/foo` names a program the walk
	// inventoried as `/usr/bin/foo`, because `/bin` is a link to `usr/bin` and
	// `filepath.WalkDir` does not descend through it.
	for _, src := range longestFirst(inv.DirLinks, p) {
		rewritten := path.Join(inv.DirLinks[src], strings.TrimPrefix(p, src))
		if t, ok := canonExact(inv, rewritten); ok {
			return t
		}
	}
	return p
}

// canonExact is canon without the directory-link rewrite: the three exact
// lookups, reporting whether any matched.
func canonExact(inv *Inventory, p string) (string, bool) {
	if _, ok := inv.Programs[p]; ok {
		return p, true
	}
	if _, ok := inv.Scripts[p]; ok {
		return p, true
	}
	if t, ok := inv.Links[p]; ok {
		return t, true
	}
	return p, false
}

// longestFirst returns the directory-link sources that are path prefixes of p,
// longest first.
//
// A prefix must end at a component boundary: `/binary/x` is not under `/bin`,
// and a plain string prefix test would say it is.
func longestFirst(dirLinks map[string]string, p string) []string {
	var out []string
	for src := range dirLinks {
		if p == src || strings.HasPrefix(p, src+"/") {
			out = append(out, src)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// shebang reads a script's interpreter and its first argument from the #! line.
func shebang(p string) (string, string, bool) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	var buf [256]byte
	n, _ := io.ReadFull(f, buf[:])
	if n < 3 || buf[0] != '#' || buf[1] != '!' {
		return "", "", false
	}
	line := buf[2:n]
	if i := bytes.IndexAny(line, "\n\r"); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(string(line))
	if len(fields) == 0 || !path.IsAbs(fields[0]) {
		return "", "", false
	}
	var arg string
	if len(fields) > 1 {
		arg = fields[1]
	}
	return fields[0], arg, true
}

// aarch64Executable reports whether p is an aarch64 ELF executable, and its
// interpreter if any.
func aarch64Executable(p string) (string, bool) {
	f, err := elf.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 || f.Class != elf.ELFCLASS64 {
		return "", false
	}
	switch f.Type {
	case elf.ET_EXEC:
		// Classic non-PIE executable, static or dynamic.
	case elf.ET_DYN:
		// Either a PIE executable or a shared library. Only the former has a
		// PT_INTERP.
	default:
		return "", false
	}
	var interp string
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(prog.Open())
		if err != nil {
			return "", false
		}
		interp = string(bytes.TrimRight(b, "\x00"))
	}
	if f.Type == elf.ET_DYN && interp == "" {
		return "", false // shared library, not a program
	}
	return interp, true
}

// Closure returns the guest paths reachable from seeds as a sorted list, and
// the references it could NOT resolve.
//
// Exec targets cannot be determined statically in general, so this expands three
// ways, all of them unioned:
//
//	every file      embedded NUL-terminated absolute paths (a string table)
//	shell scripts   a PARSE: literal paths, and a directory fan-out for the
//	                `for f in DIR/*; do . "$f"; done` idiom (shellscan.go)
//	shell scripts   every bare word, resolved against PATH (bareWords)
//
// It is a heuristic: it over-approximates when a path string is merely
// mentioned, and misses targets assembled at run time. Extra and Exclude exist
// to correct both.
//
// ❗ THE SECOND RETURN IS A DIAGNOSTIC, NOT AN ERROR LIST. It is what the shell
// parse saw and could not turn into a path -- a computed exec target, or a
// path-shaped argument -- with the commands inside any `$(…)`. Most entries are
// benign, and some are already covered by the fan-out. It exists so the question
// "which tool would we have to emulate to resolve these" is answered by the next
// real image rather than extrapolated from a sample. See Unresolved.
//
// Callers that do not report may discard it; `internal/pipeline` prints a capped
// summary and carries the full list on its Result.
func Closure(inv *Inventory, opts ClosureOptions) ([]string, []Unresolved, error) {
	programs := make(map[string]bool) // registry programs
	visited := make(map[string]bool)  // programs and scripts already scanned
	var queue []string
	var unresolved []Unresolved

	max := opts.Max
	if max <= 0 {
		max = 256
	}

	// The cap is enforced here rather than at the scan loop so that *every* way
	// a program can be admitted respects it — including a script's interpreter
	// and its `env` argument, which are pushed outside the candidate loop.
	push := func(p string) {
		if p == "" || opts.Exclude[p] {
			return
		}
		// A binary naming /usr/bin/python3 means the program that path reaches;
		// admit that program, not the symlink, so it is not translated twice.
		p = canon(inv, p)
		if visited[p] || opts.Exclude[p] {
			return
		}
		_, isProg := inv.Programs[p]
		_, isScript := inv.Scripts[p]
		if !isProg && !isScript {
			return
		}
		if isProg && len(programs) >= max {
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
	for _, s := range opts.Extra {
		push(s)
	}
	if len(queue) == 0 {
		return nil, nil, fmt.Errorf("image: no seed resolved to an exec target (seeds: %v)", opts.Seeds)
	}

	for i := 0; i < len(queue); i++ {
		if len(programs) >= max {
			break
		}
		p := queue[i]
		e, isProgram := inv.Programs[p]
		hostPath := e.HostPath
		isScript := false
		if !isProgram {
			s := inv.Scripts[p]
			hostPath = s.HostPath
			isScript = true
			// A script is not itself a program, but its interpreter is, and that
			// is what actually gets executed.
			push(s.Interp)
			// `#!/usr/bin/env bash`: the real interpreter is the argument, named
			// bare and resolved against PATH.
			if s.InterpArg != "" && !strings.Contains(s.InterpArg, "/") {
				for _, p := range resolveInPath(s.InterpArg, opts.PathDirs, inv) {
					push(p)
				}
			}
		}
		b, err := os.ReadFile(hostPath)
		if err != nil {
			continue
		}
		for _, cand := range absolutePaths(b) {
			if len(programs) >= max {
				break
			}
			push(cand)
		}
		// A SHELL script additionally gets PARSED, and what the parse names is
		// pushed IN ADDITION to the bare-word pass below — never instead of it.
		//
		// ❗ The union is what makes this strictly widening, and that is a
		// property a test checks (`TestShellScanOnlyEverWidensTheClosure`) rather
		// than one this comment asserts. It also means a parse failure costs
		// nothing: an unparseable or non-shell script falls through to exactly
		// the behaviour it had before.
		//
		// See shellscan.go for what the parse finds and why a regex would not do.
		if isScript {
			if lang, isShell := shellLangFor(inv.Scripts[p]); isShell {
				if refs, err := scanShell(b, p, lang); err == nil {
					for _, f := range refs.Files {
						push(f)
					}
					for _, d := range refs.Dirs {
						for _, e := range entriesUnder(inv, d, opts.PathDirs) {
							push(e)
						}
					}
					// ⚠️ Collected only for scripts actually REACHED. Scanning
					// every script in the image would report references from
					// files nothing runs, and a diagnostic that describes
					// unreachable code is one nobody can act on.
					for _, u := range refs.Unresolved {
						u.Script = p
						unresolved = append(unresolved, u)
					}
				}
			}
		}
		// Shell scripts name what they run by bare command word far more often
		// than by absolute path (initdb, gosu, pg_isready). Resolving every
		// word against PATH over-approximates, but a missing program is an exec
		// failure at run time whereas a spare one only costs build time. Only
		// done for scripts — doing it for a binary's string table would drag in
		// most of the image.
		if isScript {
			for _, word := range bareWords(b) {
				if len(programs) >= max {
					break
				}
				for _, p := range resolveInPath(word, opts.PathDirs, inv) {
					push(p)
				}
			}
		}
	}

	out := make([]string, 0, len(programs))
	for p := range programs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, unresolved, nil
}

// fanOutLimit caps how many entries one directory may contribute.
//
// ⚠️ A BACKSTOP, not a tuning knob. The directories this exists for hold a
// handful of scripts -- `/docker-entrypoint.d` has four. A literal path that
// happens to name a large directory (`cd /usr/bin`) would otherwise admit every
// binary under it, and the closure `Max` cap would then be spent on whatever
// `filepath.WalkDir` happened to order first rather than on anything reachable.
const fanOutLimit = 64

// entriesUnder returns the programs and scripts that are DIRECT children of a
// guest directory, for the `for f in DIR/*; do . "$f"; done` idiom.
//
// Direct children only: recursing would turn a mention of `/usr` into the whole
// image. It reads `inv`, never the filesystem — Programs and Scripts are already
// keyed by guest path, so this is a prefix match over maps in memory.
//
// ❗ PATH DIRECTORIES ARE REFUSED, and that is the load-bearing guard. Scripts
// name them constantly (`cd /usr/bin`, `ls /usr/local/bin`), and fanning one out
// would admit every executable in the image — the closure would stop meaning
// anything while still looking like it worked.
func entriesUnder(inv *Inventory, dir string, pathDirs []string) []string {
	dir = path.Clean(dir)
	if dir == "/" || dir == "." {
		return nil
	}
	for _, pd := range pathDirs {
		if path.Clean(pd) == dir {
			return nil
		}
	}
	prefix := dir + "/"
	var out []string
	collect := func(guest string) {
		if !strings.HasPrefix(guest, prefix) {
			return
		}
		if strings.Contains(guest[len(prefix):], "/") {
			return // not a direct child
		}
		out = append(out, guest)
	}
	for guest := range inv.Programs {
		collect(guest)
	}
	for guest := range inv.Scripts {
		collect(guest)
	}
	if len(out) > fanOutLimit {
		return nil
	}
	// Sorted so a build is reproducible: map iteration order would otherwise
	// decide which entries survive the closure's Max cap.
	sort.Strings(out)
	return out
}

// ClosureOptions controls closure expansion.
type ClosureOptions struct {
	// Seeds are the starting guest paths, normally from the entrypoint.
	Seeds []string
	// Extra forces additional programs in (targets built at runtime).
	Extra []string
	// Exclude keeps programs out (paths merely mentioned in a string table).
	Exclude map[string]bool
	// Max caps the number of programs; 0 means 256.
	Max int
	// PathDirs resolves bare command names found in scripts. Use PathDirs(cfg.Env).
	PathDirs []string
}

// resolveInPath resolves a bare command name to the guest paths it can name,
// returning nil if it names nothing in the image.
//
// Every PATH entry is searched, not just up to the first hit, because a wrapper
// legitimately shadows the binary it dispatches to: debian's /usr/bin/psql is a
// symlink to pg_wrapper, a script that execs /usr/lib/postgresql/17/bin/psql
// from a path it assembles at run time. Stopping at the wrapper drops the real
// binary from the closure and turns `psql` into an exec failure, so both are
// admitted — the same over-approximation Closure already prefers for bare words.
func resolveInPath(name string, dirs []string, inv *Inventory) []string {
	if name == "" || strings.Contains(name, "/") {
		return nil
	}
	if len(dirs) == 0 {
		dirs = pathDirs(nil)
	}
	var out []string
	seen := make(map[string]bool)
	for _, dir := range dirs {
		p := canon(inv, path.Join(dir, name))
		if seen[p] {
			continue
		}
		_, isProg := inv.Programs[p]
		_, isScript := inv.Scripts[p]
		if !isProg && !isScript {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// bareWords extracts candidate command words from script text: tokens that
// could name an executable.
func bareWords(b []byte) []string {
	var out []string
	start := -1
	isWord := func(c byte) bool {
		return c == '_' || c == '-' || c == '.' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	}
	for i := 0; i <= len(b); i++ {
		if i < len(b) && isWord(b[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if i-start > 1 && i-start < 64 {
				out = append(out, string(b[start:i]))
			}
			start = -1
		}
	}
	return out
}

// absolutePaths extracts plausible absolute path strings from a binary. Only
// NUL- or bound-terminated printable runs are considered, which is how paths
// appear in a string table.
func absolutePaths(b []byte) []string {
	var out []string
	for i := 0; i < len(b); {
		if b[i] != '/' {
			i++
			continue
		}
		j := i
		for j < len(b) && isPathByte(b[j]) {
			j++
		}
		// A real string-table entry ends at a NUL, not mid-buffer.
		if j < len(b) && b[j] == 0 && j-i > 1 {
			s := string(b[i:j])
			if path.IsAbs(s) && !strings.HasSuffix(s, "/") {
				out = append(out, path.Clean(s))
			}
		}
		i = j + 1
	}
	return out
}

func isPathByte(c byte) bool {
	return c > 0x20 && c < 0x7f && c != '"' && c != '\'' && c != '*' && c != '?' && c != ';' && c != ':'
}

// EntrypointSeeds returns the guest paths an image's entrypoint and command
// name, resolved against PATH when they are bare names.
func EntrypointSeeds(cfg Config, inv *Inventory) []string {
	argv := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	var seeds []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			continue // a flag, not a program
		}
		seeds = append(seeds, resolveProgram(a, cfg, inv)...)
	}
	return seeds
}

// resolveProgram resolves an argv entry to the guest paths it can name, each
// either an ELF program or a script. A bare name searches every PATH entry —
// see resolveInPath for why the first hit is not enough.
func resolveProgram(name string, cfg Config, inv *Inventory) []string {
	if strings.Contains(name, "/") {
		p := canon(inv, path.Clean(name))
		if _, ok := inv.Programs[p]; ok {
			return []string{p}
		}
		if _, ok := inv.Scripts[p]; ok {
			return []string{p}
		}
		return nil
	}
	return resolveInPath(name, pathDirs(cfg.Env), inv)
}

// ResolveExecPath turns one argv word into the guest path it names, or "" when
// nothing in the image answers to it.
//
// ❗ EXPORTED FOR THE BOOT RECORD, and the reason is a measured defect.
// `internal/pipeline` wrote the sidecar's boot argv straight from the image
// config -- `["docker-entrypoint.sh", "postgres"]` for postgres:17. The runtime
// resolves argv[0] with `vfs.resolve(cwd, path)`, which is CWD-relative and does
// no PATH lookup, so with cwd "/" it looked for `/docker-entrypoint.sh`, found
// nothing, and fell back to program 0. The guest ran apt.
//
// Docker resolves a bare exec-form entrypoint with `execvp`, i.e. against PATH,
// and that resolution belongs HERE: discovery already does it for the closure
// (`EntrypointSeeds` -> `resolveProgram`), it needs the inventory and the image
// environment, and the runtime has neither.
//
// ⚠️ Returns the FIRST resolution. `resolveInPath` deliberately returns every
// PATH hit -- a closure wants them all, because any of them might be exec'd --
// but argv[0] names exactly one thing, and PATH order is what picks it.
func ResolveExecPath(name string, cfg Config, inv *Inventory) string {
	if got := resolveProgram(name, cfg, inv); len(got) > 0 {
		return got[0]
	}
	return ""
}

// PathDirs returns the PATH entries from an image's environment.
func PathDirs(env []string) []string { return pathDirs(env) }

func pathDirs(env []string) []string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			return strings.Split(v, ":")
		}
	}
	return []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
}

// PluginDirs returns the directories in `root` that hold objects a program
// loads with `dlopen` rather than through DT_NEEDED, in the order they should be
// offered to `fuse.Options.Extra`.
//
// WHY THIS IS A CONVENTION LIST AND NOT A SCAN. The tempting general rule is
// "every shared object nothing DT_NEEDEDs", and it over-collects catastrophically:
// on python:3-slim that is ~250 gconv converters and the whole NSS set on top of
// the 77 extension modules actually wanted, and each one costs image size and
// translate time. The same shape of mistake as scanning a binary for stubbed
// MNEMONICS -- a rule that is technically sound and practically useless because
// the population it selects is dominated by things nobody calls.
//
// So the directories are named. The list is short because the mechanism is rare:
// a program that dlopens by path either ships its plugins in a private directory
// or asks a libc facility that has one.
//
// ⚠️ Being absent from this list is invisible at build time and shows up as a
// dlopen returning a handle whose every symbol resolves to NULL -- see
// fuse.Options.Extra. Add to it deliberately, and prefer a directory that a
// project OWNS over a glob that happens to match.
func PluginDirs(root string) []string {
	// Relative to the rootfs. Globs are expanded; a pattern that matches
	// nothing is simply skipped, so one list serves every image.
	patterns := []string{
		// CPython C extension modules, and anything pip installed.
		"usr/local/lib/python*/lib-dynload",
		"usr/local/lib/python*/site-packages",
		"usr/lib/python3/dist-packages",
		// OpenSSL providers and engines. Absent from a static OpenSSL, which is
		// why `cryptography` needed none of this.
		"usr/lib/*/ossl-modules",
		"usr/lib/*/engines-3",
		"usr/lib/ossl-modules",
		"usr/lib/engines-3",
		// PostgreSQL extensions ($libdir), the case Options.Extra was written for.
		"usr/lib/postgresql/*/lib",
	}
	var out []string
	seen := map[string]bool{}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, pat))
		if err != nil {
			continue // a malformed pattern is a bug in the list, not in the image
		}
		sort.Strings(matches)
		for _, m := range matches {
			if seen[m] {
				continue
			}
			if st, err := os.Stat(m); err != nil || !st.IsDir() {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
