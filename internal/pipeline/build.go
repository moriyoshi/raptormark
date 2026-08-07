// Package pipeline is the host-side end-to-end driver: container image in, one
// wasm module and its rfs sidecar out.
//
// # Why this exists and why it did not until 2026-08-24
//
// `cmd/raptormark` has always covered the individual build STEPS --
// `build-image`, `translate-one`, `link-all` -- and README recorded, correctly,
// that "the pipeline that strings them together, discovery, fuse, translate,
// link, still runs only from the `e2e/` suite. There is no `raptormark build`."
//
// That had a cost beyond convenience. `image.Plugins` sat with no production
// caller for three sessions, carried in TODO.md as if it were a loose wire,
// when the truth was that stages 1 and 2 had no CLI driver at all and there was
// nowhere for a caller to live. The pipeline being test-only also meant every
// change to it was validated only by tests that each rebuild their own variant
// of it.
//
// # What this is NOT
//
// It is not a new pipeline. Every step here is the same library call the `e2e/`
// suite already makes, in the same order, and deliberately so: a driver that
// reimplemented any of it would be a second thing to keep in step with the one
// that is actually exercised. What is new is that the steps have one caller and
// one place to report from.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"raptormark/internal/fuse"
	"raptormark/internal/image"
	"raptormark/internal/link"
	"raptormark/internal/rootfs"
	"raptormark/internal/translate"
)

// Build is `raptormark build <image>`.
type Build struct {
	Image string `arg:"" help:"Container image reference, e.g. postgres:17. Must be present locally or pullable."`

	Out         string `short:"o" type:"path" required:"" help:"Directory to write the module, the sidecar and the intermediate artifacts into."`
	Builder     string `env:"RAPTORMARK_BUILDER" help:"Builder image that carries elflift and the wasi-sdk. REQUIRED: raptormark-builder:latest is not necessarily the newest, so there is deliberately no default."`
	ObjectCache string `env:"RAPTORMARK_OBJECT_CACHE" type:"path" help:"Content-addressed object cache. Strongly recommended: without it every run re-translates from scratch, which is hours on a real closure."`

	Entry   string `help:"Guest path of the entry program. Defaults to the image's own entrypoint."`
	Profile string `default:"wasmedge" enum:"wasmedge,loopback,browser,hosted,wasix" help:"Runtime profile, passed to link-all. 'wasix' targets wasmer and is the only profile besides the default with external egress; run it with 'raptormark run --runtime wasmer'. See internal/builder.ecvisorArchive."`
	SideOut string `type:"path" help:"Also emit one PIC side module per program here, for an embedder that instantiates modules separately."`

	Plugins string `default:"auto" enum:"auto,none" help:"'auto' discovers dlopen-able plugins with image.Plugins and fuses each as its own unit; 'none' fuses only the closure's programs."`

	MaxClosure        int  `default:"10000" help:"Refuse a closure larger than this. A runaway seed set is otherwise found only after hours of translation."`
	InlineCallHistory bool `help:"Lift with the inline guest call history (elfconv patch 0060). Worth it for interpreter-shaped guests, a poor trade for server-shaped ones."`
	SuspendViaCall    bool `help:"Lift the suspend check as a call to _ecv_suspended rather than a read of the __ecv_unwinding wasm global (elfconv patch 0067). Implied by '--profile wasix --side-out', and REQUIRED there: WASIX's loader refuses a side module that imports a GLOBAL from env. A flat link defines that global rather than importing it, so a socket-only wasix build does not need this - and does not want it, because it is part of the object-cache key. Costs a call at every suspend-check site everywhere else."`

	KeepRootfs bool `help:"Keep the exported rootfs under <out>/rootfs. It is large; by default it is left in place only if the build fails."`
}

// Artifact identifies one translated thing in the built module.
//
// `Index` is the registry index, which is also which `ecv_program_<i>` global
// the corresponding side module exports -- the two are the same number by
// construction (`link.Program.Index`), and an embedder needs it to read the
// descriptor. `Side` is empty unless the build was given --side-out.
type Artifact struct {
	// Guest is the path the guest names: an entry path for a program, a `.so`
	// path for a unit.
	Guest string
	// Hash is the unit's content name -- what `ecv_host_load_side` is handed.
	Hash  string
	Index int
	Side  string
}

// Result is what a build produced, so a caller can report rather than re-derive.
type Result struct {
	Module  string
	Sidecar string
	// Programs are the closure's entry points; Units are the dlopen-able
	// plugins. Slices rather than counts because a host-driven loader needs the
	// identity of each one -- `ecv_host_load_side` names a unit by its content
	// HASH, and the embedder must know which side module that is and which
	// `ecv_program_<i>` global it exports. A count cannot answer any of that,
	// and reconstructing it from outside means re-deriving the driver's own
	// ordering, which is exactly the second copy this package exists to avoid.
	Programs []Artifact
	Units    []Artifact
	// SharedLayout is false when the closure did not fit and each program was
	// packed independently. Not an error -- see fuse.FuseClosure -- but the
	// difference between "sharing applied" and "sharing silently skipped" is
	// otherwise indistinguishable from a slow machine.
	SharedLayout bool
	// ⚠️ NO `Skipped` FIELD, deliberately, and its absence is the accurate
	// report. `fuse.Fuse` and `fuse.FuseWithUnits` ERROR on a plugin they cannot
	// satisfy -- unlike `fuse.FuseClosure`, which degrades and reports through a
	// Report -- so a skipped extra cannot reach here: the build has already
	// failed. A `Skipped []fuse.SkippedExtra` field existed briefly and was
	// always nil, which is exactly the "declared field nothing populates" defect
	// `translate.LinkRequest.toolFlags` carries a comment about.
	Excluded []image.ExcludedPlugin
	// Unresolved is every shell reference discovery could not turn into a path.
	// A DIAGNOSTIC, not a defect list -- see reportUnresolved. Carried on the
	// Result so a tool can enumerate them without re-running discovery, and so a
	// test can assert on them rather than scraping stderr.
	Unresolved []image.Unresolved
}

func (c *Build) Run() error {
	// ❗ REQUIRED, not defaulted, and the help text says why. AGENTS.md: a stale
	// `raptormark-builder:latest` fails deep inside elflift with an error that
	// reads like a defect in the input. Defaulting would make that the common
	// case.
	if c.Builder == "" {
		return fmt.Errorf(
			"--builder (or RAPTORMARK_BUILDER) is required: raptormark-builder:latest is " +
				"not necessarily the newest builder, and a stale one fails deep inside " +
				"elflift with an error that looks like a defect in the input")
	}
	if c.ObjectCache == "" {
		// A warning rather than an error: a first run on a small image is a
		// legitimate thing to do. On a real closure it is hours.
		fmt.Fprintln(os.Stderr,
			"raptormark build: no --object-cache; every program will be translated from "+
				"scratch. On a real closure that is hours, and the cache is keyed so a "+
				"change confined to runtime/ reuses every object.")
	}
	ctx := context.Background()
	res, err := c.build(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("raptormark build: %s\n", res.Module)
	fmt.Printf("  sidecar:  %s\n", res.Sidecar)
	fmt.Printf("  programs: %d, units: %d\n", len(res.Programs), len(res.Units))
	if !res.SharedLayout {
		fmt.Println("  ⚠️  the closure did not fit one shared layout; each program was packed " +
			"independently and cross-program library sharing was lost")
	}
	for _, e := range res.Excluded {
		fmt.Printf("  note: discovery excluded %s: %s\n", e.Guest, e.Reason)
	}
	return nil
}

// BuildForTest runs the build and returns what it produced, without the
// human-facing printing `Run` does.
//
// Exported so `e2e/pipeline_test.go` can assert on the RESULT -- unit and
// program counts, whether the layout was shared -- rather than scraping stdout.
// A test that parsed the printed summary would be testing the formatting, and
// would keep passing if the numbers themselves went wrong.
func (c *Build) BuildForTest(ctx context.Context) (*Result, error) { return c.build(ctx) }

// suspendViaCallFor decides whether to lift the suspend check as a call
// (elfconv patch 0067) rather than as a read of the `__ecv_unwinding` wasm
// global.
//
// ⚠️ `wasix` WITH `--side-out` implies it, and silently doing so is better than
// letting the build succeed into an artifact WASIX cannot load. The lift is the
// only place it can be decided, and by the time the loader refuses, the
// translation is hours behind you.
//
// ❗ AND ONLY WITH `--side-out`. What 0067 removes is the `env.__ecv_unwinding`
// GLOBAL import, which WASIX's linker refuses because it requires every `env`
// import to be a function -- and only a PIC SIDE-module link has one. A flat
// link pulls in `/opt/ecvisor/ecv_globals.o`, which DEFINES that symbol
// (`llvm-nm` reports `D __ecv_unwinding`; measured on the object in the image,
// not argued from the source), and a defined symbol cannot become an import.
//
// The extra condition earns itself because the implication is expensive:
// `SuspendViaCall` is part of `TranslateID`, so implying it unconditionally
// gives every socket-only wasmer build a cold object cache and hours of
// re-translation -- to remove an import that build never had.
//
// Extracted from `build` so a test can assert the rule without a builder image,
// the same way `runtimeArgs` is.
func suspendViaCallFor(flag bool, profile, sideOut string) bool {
	return flag || (profile == "wasix" && sideOut != "")
}

// unresolvedShown caps how many references the build log names individually.
//
// ⚠️ A build log is read while waiting for a build. The COUNT is the part that
// says whether to look; the examples are there so a common case does not need a
// second command to see. The full list is on `Result.Unresolved`.
const unresolvedShown = 5

// reportUnresolved prints what shell-script scanning could not turn into a path.
//
// ❗ NOT A DEFECT LIST, and the wording has to say so. Most entries are benign: a
// computed path often names a config file, and `"$f"` from a
// `/docker-entrypoint.d` loop is BOTH reported here and already resolved by the
// directory fan-out -- the scanner has no way to know the second thing.
//
// What it is for: "should we emulate sed/awk to resolve computed paths?" was
// asked on 2026-08-26 and answered by measuring six images -- 37 command
// substitutions, none producing a path to an executable. Six images is a sample,
// not a census. This makes the next image report the answer instead of somebody
// extrapolating from that sample, which is why `Via` -- the commands inside
// `$(…)` -- is printed at all.
func reportUnresolved(w io.Writer, us []image.Unresolved) {
	if len(us) == 0 {
		return
	}
	scripts := map[string]bool{}
	var commands int
	for _, u := range us {
		scripts[u.Script] = true
		if u.Kind == image.UnresolvedCommand {
			commands++
		}
	}
	fmt.Fprintf(w, "discovery: %d unresolved shell reference(s) in %d script(s), "+
		"%d of them a computed EXEC target (not necessarily a problem)\n",
		len(us), len(scripts), commands)

	// Command-kind first: those are the ones that name a program.
	ordered := append([]image.Unresolved(nil), us...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Kind == image.UnresolvedCommand && ordered[j].Kind != image.UnresolvedCommand
	})
	for i, u := range ordered {
		if i == unresolvedShown {
			fmt.Fprintf(w, "  ... and %d more (full list in Result.Unresolved)\n",
				len(ordered)-unresolvedShown)
			break
		}
		via := ""
		if len(u.Via) > 0 {
			via = "  via " + strings.Join(u.Via, ",")
		}
		fmt.Fprintf(w, "  %-7s %-34s %s%s\n", u.Kind, u.Script, u.Word, via)
	}
}

// EntryFromSeeds picks the entry program: THE FIRST SEED THAT IS A PROGRAM IN
// THE CLOSURE, not `seeds[0]`.
//
// Exported for the same reason `BuildForTest` is: `.agents-workspace/drivers/
// headroom` mirrors this driver's stages 1-2, and a driver that re-implements
// the rule reports on a pipeline that does not exist. Every second copy in this
// area has already been wrong once -- see the `/usr/local/lib` entry in TODO.md.
//
// ❗ Until 2026-08-26 the caller used `seeds[0]`, and `raptormark build <image>`
// therefore FAILED AT DISCOVERY for almost every real image -- verified against
// the real command:
//
//	build <image>: the entry /docker-entrypoint.sh is not in the closure
//	(3 programs). …
//
// `image.EntrypointSeeds` deliberately resolves SCRIPTS as well as programs
// (`resolveProgram` returns `inv.Scripts[p]`), and a real image's ENTRYPOINT is
// nearly always a script -- postgres, nginx, redis and node all ship
// `docker-entrypoint.sh`, php ships `docker-php-entrypoint`. A script is never
// IN the closure: `image.Closure` seeds FROM it, scanning it for the programs it
// names. So `seeds[0]` was a path that by construction could not satisfy the
// caller's containment check.
//
// Measured over seven images: in six, `seeds[1]` is the intended program and is
// already in the closure -- `/usr/sbin/nginx`, `/usr/local/bin/redis-server`,
// `/usr/lib/postgresql/17/bin/postgres`, `/usr/local/bin/node`,
// `/usr/local/bin/php`. Seeds are ENTRYPOINT then CMD, so this rule is just "the
// program the entrypoint script runs".
//
// ⚠️ STRICTLY WIDENING. When `seeds[0]` IS in the closure -- a bare program CMD,
// e.g. `debian:trixie-slim`'s `bash` or `python:3-slim`'s `python3` -- it is
// still chosen, because it is the first match. Nothing that built before builds
// differently; only inputs that hard-failed can move.
//
// ⚠️ It does NOT rescue every image, deliberately. `ruby:3-slim` has ONE seed
// (CMD `irb`, a script, no ENTRYPOINT), so there is no program seed at all and
// it still needs `--entry /usr/local/bin/ruby`. Falling through to `closure[0]`
// would be the guess the caller's comment rejects: right most of the time, which
// is the worst kind.
//
// Extracted from `build` so a test can assert the rule without an image or a
// builder, the same way `suspendViaCallFor` and `runtimeArgs` are.
func EntryFromSeeds(seeds, closure []string) (string, error) {
	for _, s := range seeds {
		if slices.Contains(closure, s) {
			return s, nil
		}
	}
	return "", fmt.Errorf("none of the %d entrypoint seed(s) is a program in the "+
		"closure of %d: %v.\nThe seeds are the image's ENTRYPOINT and CMD resolved "+
		"against the rootfs, and a shell script is never in the closure -- the "+
		"closure is seeded FROM it. Pass --entry with the program the script "+
		"ultimately runs", len(seeds), len(closure), seeds)
}

func (c *Build) build(ctx context.Context) (*Result, error) {
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return nil, err
	}
	work := filepath.Join(c.Out, "work")
	root := filepath.Join(c.Out, "rootfs")
	outDir := filepath.Join(c.Out, "obj")
	for _, d := range []string{work, root, outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	// --- stage 1: discovery -------------------------------------------------
	cfg, err := image.Inspect(ctx, c.Image)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", c.Image, err)
	}
	fmt.Fprintf(os.Stderr, "discovery: exporting %s ...\n", c.Image)
	if err := image.ExportRootfs(ctx, c.Image, root); err != nil {
		return nil, fmt.Errorf("exporting %s: %w", c.Image, err)
	}
	inv, err := image.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("scanning the rootfs: %w", err)
	}
	closure, unresolved, err := image.Closure(inv, image.ClosureOptions{
		Seeds:    image.EntrypointSeeds(cfg, inv),
		PathDirs: image.PathDirs(cfg.Env),
		Max:      c.MaxClosure,
	})
	if err != nil {
		return nil, fmt.Errorf("computing the closure: %w", err)
	}
	if len(closure) == 0 {
		return nil, fmt.Errorf("the closure is empty: %s has no entrypoint this can reach", c.Image)
	}

	entry := c.Entry
	if entry == "" {
		// The image's own entrypoint, which is what `EntrypointSeeds` seeded
		// from. Taking closure[0] instead would be a guess that happens to be
		// right most of the time, which is the worst kind.
		seeds := image.EntrypointSeeds(cfg, inv)
		if len(seeds) == 0 {
			return nil, fmt.Errorf("%s declares no entrypoint; pass --entry", c.Image)
		}
		var err error
		if entry, err = EntryFromSeeds(seeds, closure); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Image, err)
		}
	}
	if !slices.Contains(closure, entry) {
		return nil, fmt.Errorf("the entry %s is not in the closure (%d programs). "+
			"Either it is not an executable this can see, or --entry names a path the "+
			"image resolves differently", entry, len(closure))
	}
	fmt.Fprintf(os.Stderr, "discovery: %d program(s), entry %s\n", len(closure), entry)
	reportUnresolved(os.Stderr, unresolved)

	// --- plugin discovery ---------------------------------------------------
	var extras []string
	var excluded []image.ExcludedPlugin
	if c.Plugins == "auto" {
		found, ex, err := image.Plugins(root)
		if err != nil {
			return nil, fmt.Errorf("plugin discovery: %w", err)
		}
		excluded = ex
		for _, p := range found {
			extras = append(extras, p.Host)
		}
		fmt.Fprintf(os.Stderr, "discovery: %d plugin(s), %d excluded\n", len(found), len(ex))
	}

	// --- stage 2: fuse ------------------------------------------------------
	opts := fuseOptionsFor(extras)
	opts.LibraryPaths = libraryPaths(root)

	// ❗ HOST paths. `PlanLayoutFor` OPENS what it is given, so guest paths make
	// it fail with "open /usr/bin/pgdlhost: no such file or directory".
	//
	// ⚠️ That failure is why the pre-check below exists. The fallback is written
	// to degrade rather than fail -- correct for an overflow -- and it swallowed
	// this as one, reporting "the closure did not fit one shared layout" for what
	// was a wrong argument. Every image lost layout sharing and the message
	// named the wrong cause. Two very different problems must not render alike.
	hostPaths := make([]string, 0, len(closure))
	for _, g := range closure {
		prog, ok := inv.Programs[g]
		if !ok {
			return nil, fmt.Errorf("%s is in the closure but not inventoried", g)
		}
		if _, err := os.Stat(prog.HostPath); err != nil {
			return nil, fmt.Errorf("the closure names %s at %s, which cannot be read: %w",
				g, prog.HostPath, err)
		}
		hostPaths = append(hostPaths, prog.HostPath)
	}

	shared := true
	layout, err := fuse.PlanLayoutFor(hostPaths, opts)
	if err != nil {
		// ⚠️ FALLBACK IS NOT AN ERROR, and it must be LOUD. fuse.FuseClosure
		// documents the same policy: a closure that does not fit one layout is
		// still buildable, it just loses cross-program library sharing.
		// Reporting overflow as a failure would make a large image unbuildable
		// in exchange for an optimization. Reporting it as nothing at all is
		// how a missing speedup becomes indistinguishable from a slow machine.
		fmt.Fprintf(os.Stderr,
			"fuse: no shared layout (%v); packing each program independently\n", err)
		shared = false
	} else {
		opts.Layout = layout
	}

	type fused struct {
		file  string
		bytes []byte
		guest string // the guest .so path, for a unit; "" for a program
		exec  string // the guest program path, for a program; "" for a unit
	}
	var images []fused

	for _, guestPath := range closure {
		prog, ok := inv.Programs[guestPath]
		if !ok {
			return nil, fmt.Errorf("%s is in the closure but not inventoried", guestPath)
		}
		base := sanitise(guestPath)
		if guestPath == entry && len(extras) > 0 {
			// Only the ENTRY carries the plugin units. A plugin is dlopen'd by
			// whatever runs it, and fusing the same unit under every program in
			// the closure would translate it once per program for one copy of
			// the code that is ever reachable.
			img, units, err := fuse.FuseWithUnits(prog.HostPath, opts)
			if err != nil {
				return nil, fmt.Errorf("fusing %s with units: %w", guestPath, err)
			}
			images = append(images, fused{base + ".fused", img, "", guestPath})
			for _, u := range units {
				g, err := guestPathOf(root, u.Path)
				if err != nil {
					return nil, err
				}
				images = append(images, fused{sanitise(g) + ".fused", u.Image, g, ""})
			}
			continue
		}
		// ❗ WITHOUT `Extra`, and this is not a micro-optimisation.
		//
		// `opts` carries every discovered plugin. Passing it here would fuse ALL
		// of them into EVERY non-entry program: on `postgres:17` that is 79
		// plugins into each of 71 programs, which defeats the entire per-unit
		// design and multiplies the image.
		//
		// And it would not merely bloat. `fuse.Fuse` ERRORS on a plugin it
		// cannot satisfy rather than skipping it, so one program in the closure
		// that cannot resolve one plugin fails the whole build -- with a message
		// about a plugin that program never had any business loading.
		//
		// ⚠️ Invisible on a one-program fixture, which is what this driver was
		// first exercised on. Only the entry carries units; see the branch above.
		img, err := fuse.Fuse(prog.HostPath, optionsForNonEntryProgram(opts))
		if err != nil {
			return nil, fmt.Errorf("fusing %s: %w", guestPath, err)
		}
		images = append(images, fused{base + ".fused", img, "", guestPath})
	}
	fmt.Fprintf(os.Stderr, "fuse: %d image(s)\n", len(images))

	// --- stage 3: translate -------------------------------------------------
	b, err := translate.BuilderFromImage(ctx, c.Builder)
	if err != nil {
		return nil, fmt.Errorf("reading the builder image %s: %w", c.Builder, err)
	}
	suspendViaCall := suspendViaCallFor(c.SuspendViaCall, c.Profile, c.SideOut)
	if suspendViaCall && !c.SuspendViaCall {
		fmt.Fprintln(os.Stderr,
			"raptormark build: --profile wasix --side-out implies --suspend-via-call; lifting the "+
				"suspend check as a call so the side modules import no GLOBAL from env")
	}
	topts := translate.Options{
		Runtime:           "ecvisor",
		InlineCallHistory: c.InlineCallHistory,
		SuspendViaCall:    suspendViaCall,
	}
	// The flag wins over the environment, and both are honoured: kong already
	// fills ObjectCache from RAPTORMARK_OBJECT_CACHE, so this is nil only when
	// neither was given -- which RunCached treats as "no cache".
	var cache *translate.Cache
	if c.ObjectCache != "" {
		cache = &translate.Cache{Dir: c.ObjectCache}
	}

	var progs []link.Program
	var objs []string
	execOf := map[int]string{}
	dlOf := map[int]string{}
	for i, im := range images {
		p := filepath.Join(work, im.file)
		if err := os.WriteFile(p, im.bytes, 0o755); err != nil {
			return nil, err
		}
		sha, err := translate.FileSHA256(p)
		if err != nil {
			return nil, err
		}
		id := translate.ModuleID(p, sha)
		prog := link.Program{Name: id, Index: i}
		progs = append(progs, prog)
		objs = append(objs, filepath.Join(outDir, id+".o"))
		execOf[i], dlOf[i] = im.exec, im.guest

		frag := filepath.Join(work, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "translate: [%d/%d] %s\n", i+1, len(images), im.file)
		if _, err := b.RunCached(ctx, cache, translate.Request{
			ELF: p, OutDir: outDir, ModuleID: id,
			Fragment: frag, Keep: prog.Symbol(), Options: topts,
		}); err != nil {
			return nil, fmt.Errorf("translating %s: %w", im.file, err)
		}
	}

	// --- stage 4: link ------------------------------------------------------
	if _, err := link.WriteLinkInputs(work, "registry.c", progs); err != nil {
		return nil, err
	}
	linked, err := link.ReadManifest(work)
	if err != nil {
		return nil, err
	}
	var execEntries []link.ExecEntry
	var dlEntries []link.DlEntry
	var progArts, unitArts []Artifact
	for i := range images {
		// The side module's path, when one was emitted. link-all names it after
		// the object, which is named after the module id -- so this is derived
		// the same way `sideLink` derives it rather than guessed.
		side := ""
		if c.SideOut != "" {
			side = filepath.Join(c.SideOut, linked[i].Name+".side.wasm")
		}
		art := Artifact{Hash: linked[i].Name, Index: i, Side: side}
		if execOf[i] != "" {
			execEntries = append(execEntries, link.ExecEntry{Path: execOf[i], Hash: linked[i].Name})
			art.Guest = execOf[i]
			progArts = append(progArts, art)
		}
		if dlOf[i] != "" {
			dlEntries = append(dlEntries, link.DlEntry{Path: dlOf[i], Hash: linked[i].Name})
			art.Guest = dlOf[i]
			unitArts = append(unitArts, art)
		}
	}
	execMap, err := link.ExecMap(linked, execEntries)
	if err != nil {
		return nil, fmt.Errorf("encoding the exec map: %w", err)
	}
	var dlMap []byte
	if len(dlEntries) > 0 {
		if dlMap, err = link.DlMap(linked, dlEntries); err != nil {
			return nil, fmt.Errorf("encoding the dlopen map: %w", err)
		}
	}

	argv := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	if len(argv) == 0 {
		argv = []string{entry}
	}
	// ❗ RESOLVE argv[0], or the guest runs the wrong program. The runtime
	// resolves it with `vfs.resolve(cwd, path)` -- CWD-relative, no PATH lookup
	// -- so a bare `docker-entrypoint.sh` with cwd "/" becomes
	// `/docker-entrypoint.sh`, which postgres:17 does not have (its script is in
	// /usr/local/bin). `resolve` then returned None and the boot path fell back
	// to program 0: measured 2026-08-27, the guest printed apt's
	// `E: Invalid operation postgres`.
	//
	// Docker uses execvp for a bare exec-form entrypoint, i.e. PATH. That
	// resolution is done HERE because discovery already does it and has what it
	// needs -- the inventory and the image environment -- which the runtime does
	// not.
	//
	// ⚠️ 5 of 7 surveyed images were affected. The two that worked
	// (nginx:latest, nginx:alpine) write `/docker-entrypoint.sh` with a leading
	// slash, which is why nothing caught this.
	if len(argv) > 0 {
		if r := image.ResolveExecPath(argv[0], cfg, inv); r != "" {
			argv[0] = r
		}
		// A word nothing answers to is LEFT AS IT WAS rather than replaced with
		// `entry`: the runtime reports it, and silently substituting a different
		// program is the failure this is fixing.
	}
	cwd := cfg.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	sidecarImg, stats, err := rootfs.Build(root, rootfs.Options{
		ExecMap: execMap,
		DlMap:   dlMap,
		Boot:    &rootfs.Boot{Argv: argv, Env: cfg.Env, Cwd: cwd},
	})
	if err != nil {
		return nil, fmt.Errorf("building the rfs sidecar: %w", err)
	}
	_ = stats
	sidecar := filepath.Join(c.Out, "rootfs.img")
	if err := os.WriteFile(sidecar, sidecarImg, 0o644); err != nil {
		return nil, err
	}

	module := filepath.Join(c.Out, "app.wasm")
	fmt.Fprintf(os.Stderr, "link: %d object(s) -> %s\n", len(objs), module)
	if err := b.Link(ctx, translate.LinkRequest{
		Registry:          filepath.Join(work, "registry.c"),
		Objects:           objs,
		Out:               module,
		SideOut:           c.SideOut,
		Profile:           c.Profile,
		InlineCallHistory: c.InlineCallHistory,
	}); err != nil {
		return nil, fmt.Errorf("linking: %w", err)
	}

	if !c.KeepRootfs {
		// The export is large (hundreds of MB for a real image) and everything
		// downstream of it is already on disk. Removed only on SUCCESS: on a
		// failure it is the thing you need to look at.
		if err := os.RemoveAll(root); err != nil {
			fmt.Fprintf(os.Stderr, "note: could not remove %s: %v\n", root, err)
		}
	}

	return &Result{
		Module:       module,
		Sidecar:      sidecar,
		Programs:     progArts,
		Units:        unitArts,
		SharedLayout: shared,
		Unresolved:   unresolved,
		Excluded:     excluded,
	}, nil
}

// fuseOptionsFor builds the fuse options for a closure whose plugins are these.
//
// A named constructor so the test can build the same thing the driver does,
// rather than a second literal that could drift from it.
func fuseOptionsFor(extras []string) fuse.Options {
	return fuse.Options{Extra: extras}
}

// optionsForNonEntryProgram strips the plugins, and ONLY the plugins.
//
// ❗ See the call site: `opts` carries every discovered plugin, and only the
// ENTRY fuses them (as separate units). Handing them to the other programs would
// fuse all of them into each -- 79 plugins into each of 71 programs on
// `postgres:17` -- and, because `fuse.Fuse` ERRORS on a plugin it cannot satisfy
// rather than skipping it, one unresolvable plugin would fail the whole build
// with a message about a program that never wanted it.
//
// ⚠️ Returns a COPY. `opts` is reused for every iteration, so clearing Extra in
// place would strip the entry's plugins too whenever a non-entry program came
// first in the closure -- an order-dependent bug that a one-program fixture and
// a lucky ordering both hide.
func optionsForNonEntryProgram(opts fuse.Options) fuse.Options {
	opts.Extra = nil
	return opts
}

// libraryPaths is the search list a fused image resolves DT_NEEDED against.
//
// The first four are the ones the e2e suite uses, and they are not a guess: a
// Debian rootfs is usr-merged, so /lib and /usr/lib are the same directory
// reached two ways, and the multiarch subdirectory is where everything actually
// lives.
//
// ❗ THE `/usr/local` PAIR IS NOT OPTIONAL, and this list was a subset of what
// the images themselves declare until 2026-08-26. Debian's own
// `/etc/ld.so.conf.d/` names four directories, and two of them were missing
// here:
//
//	libc.conf              /usr/local/lib
//	aarch64-linux-gnu.conf /usr/local/lib/aarch64-linux-gnu, /lib/…, /usr/lib/…
//
// `python:3-slim` keeps `libpython3.14.so.1.0` in `/usr/local/lib` and
// `ruby:3-slim` keeps `libruby.so.3.4` there, so neither image's PRINCIPAL
// library could be resolved: `fuse: cannot find libpython3.14.so.1.0 in [...]`.
// Both are in README's image survey. Measured with
// `.agents-workspace/drivers/headroom`, which plans a layout without
// translating.
//
// ⚠️ **APPENDED, never inserted.** `findLib` is first-match-over-an-ordered-list,
// so adding to the END cannot change where any name that already resolved
// resolves. That is what makes this safe to ship without re-fusing every image:
// only a previously UNRESOLVABLE name can move, and it moves from "build fails"
// to "found". Putting them first would match Debian's ld.so.conf ORDER and would
// silently re-point any name present in both places.
//
// ⚠️ This is still a hardcoded list, not a reading of the rootfs's own
// `/etc/ld.so.conf`, and it does not honour `DT_RUNPATH`/`DT_RPATH` -- the fuser
// reads only `DT_NEEDED` (`fuse.go:272`, `:290`). An image with a custom conf or
// a RUNPATH-relative layout still fails. See `.agents/docs/TODO.md`.
func libraryPaths(root string) []string {
	return []string{
		filepath.Join(root, "lib"),
		filepath.Join(root, "usr/lib"),
		filepath.Join(root, "lib/aarch64-linux-gnu"),
		filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
		filepath.Join(root, "usr/local/lib"),
		filepath.Join(root, "usr/local/lib/aarch64-linux-gnu"),
	}
}

// guestPathOf maps a host path under root back to the path the guest sees.
func guestPathOf(root, hostPath string) (string, error) {
	rel, err := filepath.Rel(root, hostPath)
	if err != nil {
		return "", fmt.Errorf("%s is not under %s: %w", hostPath, root, err)
	}
	// ❗ `filepath.Rel` SUCCEEDS for a path outside the root, returning a
	// `../..` form -- so the error above is not the check it looks like.
	// Without this, a host path from elsewhere became the guest path
	// "/../../elsewhere/lib.so" and was written into the dlopen map as if it
	// were real. Caught by TestGuestPathOfRoundTrips before this ran once.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the exported rootfs %s (relative path %q)",
			hostPath, root, rel)
	}
	return "/" + filepath.ToSlash(rel), nil
}

// sanitise turns a guest path into a filename stem, INJECTIVELY.
//
// ⚠️ Collision here is silent and expensive: two images written to one file
// means the second overwrites the first, the second translation runs on the
// wrong bytes, and the module ends up containing one program twice under two
// names -- with the exec map pointing both guest paths at it. Nothing fails; the
// wrong program runs.
//
// ⚠️ THE FIRST VERSION COLLIDED, and `TestSanitiseCannotCollide` caught it
// before the function was ever called: it mapped BOTH '/' and '.' to '_', so
// `/a/b.c`, `/a.b/c`, `/a/b/c` and `/a.b.c` all became `a_b_c`. Mapping two
// distinct characters onto one is exactly how injectivity is lost, and '.' never
// needed mapping -- it is legal in a filename everywhere this runs.
//
// So: escape the escape character first, then map the separator. `_` -> `__`
// and `/` -> `_` cannot collide, because after the first pass every `_` in the
// output came from a real `_` in pairs, and any lone `_` is a separator.
func sanitise(guestPath string) string {
	s := strings.TrimPrefix(guestPath, "/")
	s = strings.ReplaceAll(s, "_", "__")
	return strings.ReplaceAll(s, "/", "_")
}
