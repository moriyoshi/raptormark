// Package translate drives translate-one inside the builder image and keys the
// resulting per-binary objects on a content-addressed identity.
//
// RECONSTRUCTED on 2026-08-01. Unlike internal/link — whose contract is fixed
// exactly by runtime/src/abi.rs — only this package's *inputs* survive. From
// builder/Dockerfile:
//
//	Cache identity for per-binary translated objects (internal/translate.TranslateID;
//	.agents/docs/PERF.md, Lever A): the base image digest (elflift + wasi-sdk) and the
//	translate-one.sh content hash, both computed and passed by `raptormark build-image`. These
//	are stable across ecvisor edits (which rebuild only the layers above, not the
//	base), so keying the .o cache on them lets an ecvisor-only change reuse every
//	translated object.
//
// So the inputs are known — base image .Id, translate-one.sh sha256, the ELF —
// but the original hash *construction* is not recorded anywhere that survived.
//
// (translate-one.sh has since become `raptormark translate-one`, and the second
// input a hash of that pipeline's Go sources — see internal/builder.TranslateSH.
// The label it lands in, raptormark.translate_sh, kept its name so images built
// before the port still describe themselves the same way.)
// The scheme below is therefore a design choice, not a recovery. That is safe
// because this is only a cache key: it must be stable and collision-resistant,
// not bit-identical to the original. It will not match any pre-wipe cache.
package translate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// idVersion is mixed into every TranslateID. Bump it whenever the meaning of
// the pipeline changes in a way the other inputs do not capture, to force a
// cache miss rather than silently serving a stale object.
const idVersion = "raptormark-translate-id-v1"

// objectKeyVersion is the same escape hatch for ObjectKey: bump it when the set
// of inputs an object depends on changes, so old entries miss rather than being
// served against a key that no longer means what it did.
const objectKeyVersion = "raptormark-object-key-v1"

// Options are the translate-one flags that change the produced object. Anything
// here must feed TranslateID, or a cached object could be served for a
// different translation.
type Options struct {
	// Runtime is "upstream" (elfconv's C++ runtime) or "ecvisor" (the Rust
	// supervisor). ecvisor additionally requires a fragment and keep symbol.
	Runtime string
	// Promote runs the ecv-promote register-promotion pass over the bitcode.
	Promote bool
	// Target is translate-one's --target. Only aarch64-wasi32 is supported.
	Target string
	// InlineCallHistory has elflift emit the guest call history inline rather
	// than calling `_ecv_save_call_history` / `_ecv_func_epilogue` at every
	// guest BL (elfconv patch 0060). Opt-in.
	//
	// Measured 2026-08-15: -18.7% on a call-heavy guest, -2.3% on a realistic
	// one, +10% module size. Worth it for interpreter-shaped guests, a poor
	// trade for server-shaped ones, which is why it is a per-translation option
	// rather than a default.
	//
	// Requires a runtime that publishes `__ecv_ch_base`/`_len`/`_cap` and
	// `__ecv_slow`. ecvisor's cshim does; elfconv's own runtime defines them
	// zero, and a zero capacity selects the call path, so `--runtime upstream`
	// keeps working either way.
	InlineCallHistory bool
}

func (o Options) withDefaults() Options {
	if o.Runtime == "" {
		o.Runtime = "upstream"
	}
	if o.Target == "" {
		o.Target = "aarch64-wasi32"
	}
	return o
}

func (o Options) validate() error {
	switch o.Runtime {
	case "upstream", "ecvisor":
	default:
		return fmt.Errorf("translate: unknown runtime %q", o.Runtime)
	}
	if o.Target != "aarch64-wasi32" {
		return fmt.Errorf("translate: unsupported target %q", o.Target)
	}
	return nil
}

// Builder is a raptormark-builder image and the toolchain identity it records.
//
// BaseID and TranslateSH are read back from the labels `raptormark build-image` sets, so
// the cache key is derived from the image actually being used rather than from
// anything the caller asserts.
type Builder struct {
	Image       string
	BaseID      string
	TranslateSH string
}

// BuilderFromImage reads the toolchain identity out of a builder image's
// labels. `raptormark build-image` writes both; an image missing them predates the
// labelling and cannot be cached against safely.
func BuilderFromImage(ctx context.Context, image string) (Builder, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect",
		"--format", "{{json .Config.Labels}}", image).Output()
	if err != nil {
		return Builder{}, fmt.Errorf("translate: inspect %s: %w", image, err)
	}
	var labels map[string]string
	if err := json.Unmarshal(out, &labels); err != nil {
		return Builder{}, fmt.Errorf("translate: decode labels for %s: %w", image, err)
	}
	b := Builder{
		Image:       image,
		BaseID:      labels["raptormark.base_id"],
		TranslateSH: labels["raptormark.translate_sh"],
	}
	if b.BaseID == "" || b.TranslateSH == "" {
		return Builder{}, fmt.Errorf(
			"translate: %s lacks raptormark.base_id/raptormark.translate_sh labels; rebuild with builder/`raptormark build-image`", image)
	}
	return b, nil
}

// TranslateID is the content-addressed identity of one translation: this ELF,
// through this toolchain, with these options. It deliberately excludes the
// registry index and program name — see ObjectKey.
//
// Inputs are length-prefixed and domain-separated so no two distinct input sets
// can produce the same byte stream (e.g. a base id ending in a hex digit cannot
// be confused with a longer id).
//
// `InlineCallHistory` is appended ONLY when set, so a default translation keys
// exactly as it did before the option existed. That is a claim about codegen,
// not a convenience, and it was PROVEN before being relied on: lifting the same
// fixture with elfconv patch 0060 absent, and with it applied but the flag off,
// produced bitcode with the identical SHA-256 (`f6243d9d…`), while the flag on
// produced different bytes. An unconditional component here would have
// invalidated every cached object for an option nobody asked for.
//
// Any FUTURE option must earn the same treatment the same way. Without that
// proof, add it unconditionally and accept the invalidation — serving a stale
// object is the failure this key exists to prevent.
func (b Builder) TranslateID(elfSHA256 string, opts Options) string {
	opts = opts.withDefaults()
	parts := []string{
		idVersion,
		b.BaseID,
		b.TranslateSH,
		opts.Runtime,
		opts.Target,
		fmt.Sprintf("promote=%t", opts.Promote),
		elfSHA256,
	}
	if opts.InlineCallHistory {
		parts = append(parts, "inline_call_history=true")
	}
	// The experimental switches that change the emitted bytes. Appended only
	// when set, exactly like the flag above, so a default translation keys as it
	// always has -- see experimental.go for which switches qualify and why this
	// reads the environment rather than taking a parameter.
	parts = append(parts, ExperimentalSettings()...)
	return hashParts(parts...)
}

// hashParts hashes length-prefixed fields, so no two distinct field sets can
// produce the same byte stream.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ObjectKey is the cache key for a built *object*, as opposed to a translation,
// and is what Cache stores under.
//
// TranslateID identifies the lift — this ELF, this toolchain, these options —
// and stops there. Three further inputs reach the object's bytes, so a key
// without them would serve a wrong object:
//
//   - ModuleID. namespace-object tags every local symbol in the module with it
//     (translate-one.sh passes it as the tag), so two translations differing
//     only in module id produce differently-named symbols. It carries the ELF's
//     basename, which means /bin/cat and /usr/bin/cat are NOT interchangeable
//     even when their contents are identical.
//   - Keep, the internalize public-api list, i.e. ecv_program_<index>. This is
//     where the index-dependence below enters.
//   - Fragment. It is compiled and llvm-link'd into the module, and it carries
//     the program's name as well as its index.
//
// Upstream-runtime output has neither Keep nor Fragment, so it is
// index-independent; only the module id distinguishes it.
//
// Naming the descriptor by content hash instead of index would make every
// object index-independent and improve the hit rate — adding one program to a
// 71-program closure currently misses on every object whose index shifts.
//
// ⚠️ CORRECTED 2026-08-21. This used to say the rename was blocked because
// "ecv_program_<i> is the recovered contract, see builder/translate-one.sh".
// Both halves are wrong. That script is not in the tree (translate-one is
// internal/builder/translateone.go), and the name is NOT a runtime contract:
// runtime/src/abi.rs reads only `ecv_programs`, `ecv_program_count` and
// `ecv_program_size`, never a per-program symbol name. `ecv_program_<i>` is a
// build-time linkage handle with four consumers, all inside this repo —
// link.RegistryC's extern, translate-one's --keep, link-all's side-module
// `--export` (internal/builder.sideLinkArgs, which derives it from the object's
// POSITION in --objs rather than from the manifest), and the development
// embedder e2e/testdata/embedder.mjs.
//
// The real blocker is this key. `internal/link` is deliberately absent from
// internal/builder.translateSources, so a rename does not move TranslateID — it
// moves `Keep` and the fragment text, which are direct inputs here and which
// genuinely reach the object's bytes. So every cached ecvisor object misses
// exactly once, correctly. That is a cost decision (hours of re-translation),
// not a refactor, which is why it is still not done. Most of the cost is
// already absorbed: with ECV_STABLE_SPLIT every partition but the one holding
// the fragment is byte-identical across an index shift and the partition cache
// serves it (internal/builder/partcache.go; builder/ecv-partition.h's
// dead-declaration sweep is what made that true). What is still paid per
// shifted program is the lift, ecv-prepare, the split and the final
// `wasm-ld -r`. See .agents/docs/TODO.md, "Decouple the registry index from the
// object".
func (b Builder) ObjectKey(r Request) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	elfSHA, err := FileSHA256(r.ELF)
	if err != nil {
		return "", err
	}
	opts := r.Options.withDefaults()
	parts := []string{
		objectKeyVersion,
		b.TranslateID(elfSHA, opts),
		r.ModuleID,
		r.Keep,
	}
	if opts.Runtime == "ecvisor" {
		frag, err := os.ReadFile(r.Fragment)
		if err != nil {
			return "", fmt.Errorf("translate: reading fragment for the object key: %w", err)
		}
		parts = append(parts, string(frag))
	}
	return hashParts(parts...), nil
}

// partCacheEnv names a host directory holding compiled bitcode partitions,
// mounted into translate-one. It is separate from RAPTORMARK_OBJECT_CACHE
// because it stores a different thing at a different granularity: that one keys
// a whole program's object on the ELF and the toolchain, this one keys a single
// partition on its bitcode.
//
// The case it serves is the one the object cache cannot: when a program's INDEX
// shifts because another program joined the closure, only `Keep` and the
// fragment change, so the object key misses while every partition but one is
// byte-identical.
const partCacheEnv = "RAPTORMARK_PART_CACHE"

// PartCacheDir returns the configured partition-cache directory, or "" when
// unset. A nil result disables the cache everywhere it is consulted.
func PartCacheDir() string { return os.Getenv(partCacheEnv) }

// libCacheHostDir is where lifted LIBRARY code is kept, or "" for off.
//
// DEFAULT ON since 2026-08-14, which for this cache means choosing a LOCATION
// rather than flipping a flag -- the directory is the switch. It lands beside
// whichever persistent cache root the run already has, because a library lift is
// worth keeping exactly as long as the artifacts built from it are:
//
//	RAPTORMARK_LIB_CACHE      explicit, wins outright
//	<RAPTORMARK_PART_CACHE>/lib-lifts
//	<RAPTORMARK_OBJECT_CACHE>/lib-lifts
//	otherwise off
//
// The partition cache comes first because it marks the regime this pays in: it
// only ever hits under the stable partitioner, i.e. the closure path, which is
// also the only path where two programs share libraries. With NEITHER root
// configured there is nowhere durable to write, and a cache that does not
// outlive one translation is only overhead.
//
// Inert without RAPTORMARK_LIB_RANGES, which only the closure path sets, so a
// single-program run is unaffected by this default.
//
// THE EVIDENCE, and it took two attempts to get right. A wall-clock A/B said the
// cache cost 13% (671 s against 592 s) and the default was held. That was noise:
// the same A/B's FIRST program does identical work in both arms and its codegen
// still differed by 60 s (395 vs 456). Measuring the phases the cache actually
// touches, on the second program of an echo+bash closure:
//
//	              cache off   cache on
//	lift            27.98 s     10.85 s   -17.13
//	prepare+split   16.29 s     17.32 s    +1.03
//
// So the merge cost of composing from N cached halves is ONE SECOND, not the
// dominant term the wall clock implied, and the lift saving is real and large.
//
// RAPTORMARK_NO_LIB_CACHE turns it off wherever it would otherwise apply, so a
// regression can be bisected without rebuilding an image.
func libCacheHostDir() string {
	if os.Getenv("RAPTORMARK_NO_LIB_CACHE") != "" {
		return ""
	}
	if v := os.Getenv("RAPTORMARK_LIB_CACHE"); v != "" {
		return v
	}
	for _, root := range []string{os.Getenv(partCacheEnv), os.Getenv(cacheEnv)} {
		if root != "" {
			return filepath.Join(root, "lib-lifts")
		}
	}
	return ""
}

// ModuleID is the --module-id translate-one writes its outputs under, and the
// program name the runtime reports. Shaped like the example in
// builder/translate-one.sh's usage block: prog_ab12cd34ef56.
//
// It is derived from the ELF's CONTENT HASH, not from TranslateID.
//
// namespace-object tags every local symbol with this id, so it reaches every
// symbol name in the emitted object. Folding TranslateID in would make those
// names depend on the patched-base image digest, i.e. on the LIFTER -- so a
// one-instruction lifter patch renamed all ~10,000 symbols and no compiled
// partition could be reused, which is most of what makes a patch cost a full
// re-translation.
//
// The elf hash satisfies what the id actually has to do: be unique per program
// within a link, since two programs have different contents. It deliberately
// does NOT distinguish two translations of the same ELF under different Options
// -- those are never linked together, and ObjectKey still folds in TranslateID,
// so the object cache keeps telling them apart.
func ModuleID(elfName, elfSHA256 string) string {
	base := filepath.Base(elfName)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		name = "p" + name
	}
	if len(elfSHA256) < 12 {
		return name + "_" + elfSHA256
	}
	return name + "_" + elfSHA256[:12]
}

// FileSHA256 hashes a file's contents — the ELF input to TranslateID.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Request is one binary to translate.
type Request struct {
	// ELF is the host path of the aarch64 binary.
	ELF string
	// OutDir is the host directory translate-one writes into.
	OutDir string
	// ModuleID names the outputs; use ModuleID().
	ModuleID string
	// Fragment is the host path of the per-program C fragment from
	// internal/link.FragmentC. Required for the ecvisor runtime.
	Fragment string
	// Keep is the symbol the internalize pass preserves, i.e.
	// link.Program.Symbol(). Required for the ecvisor runtime.
	Keep string
	// Jobs caps translate-one's concurrent codegen processes; 0 means one per
	// core. It lives on Request rather than Options ON PURPOSE: Options feeds
	// TranslateID and hence ObjectKey, and a scheduling knob that cannot change
	// the emitted object must not invalidate a cache that costs hours to refill.
	// TestObjectKeyIgnoresJobs holds this.
	Jobs int
	Options
}

// Args builds the translate-one argument list for a request, using the
// in-container mount points Run sets up.
func (r Request) args() []string {
	o := r.Options.withDefaults()
	args := []string{
		"--elf", "/in/" + filepath.Base(r.ELF),
		"--out", "/out",
		"--module-id", r.ModuleID,
		"--target", o.Target,
		"--runtime", o.Runtime,
	}
	if o.Promote {
		args = append(args, "--promote")
	}
	if o.InlineCallHistory {
		args = append(args, "--inline-call-history")
	}
	if r.Jobs > 0 {
		args = append(args, "--jobs", strconv.Itoa(r.Jobs))
	}
	if o.Runtime == "ecvisor" {
		args = append(args, "--fragment", "/work/"+filepath.Base(r.Fragment), "--keep", r.Keep)
	}
	return args
}

func (r Request) validate() error {
	// Defaults first: an unset Target/Runtime is normal and must not be rejected.
	if err := r.Options.withDefaults().validate(); err != nil {
		return err
	}
	if r.ELF == "" || r.OutDir == "" || r.ModuleID == "" {
		return fmt.Errorf("translate: ELF, OutDir and ModuleID are required")
	}
	if r.Options.withDefaults().Runtime == "ecvisor" && (r.Fragment == "" || r.Keep == "") {
		return fmt.Errorf("translate: ecvisor runtime requires Fragment and Keep")
	}
	return nil
}

// Run invokes translate-one in the builder image for one binary.
//
// The builder's ENTRYPOINT is translate-one itself, so the container takes the
// flags directly. Inputs are mounted read-only; only OutDir is writable.
func (b Builder) Run(ctx context.Context, r Request) error {
	if err := r.validate(); err != nil {
		return err
	}
	elfDir, err := filepath.Abs(filepath.Dir(r.ELF))
	if err != nil {
		return err
	}
	outDir, err := filepath.Abs(r.OutDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	docker := []string{
		"run", "--rm",
		"-v", elfDir + ":/in:ro",
		"-v", outDir + ":/out",
	}
	if r.Options.withDefaults().Runtime == "ecvisor" {
		fragDir, err := filepath.Abs(filepath.Dir(r.Fragment))
		if err != nil {
			return err
		}
		docker = append(docker, "-v", fragDir+":/work:ro")
	}
	// Partition cache, if the caller configured one. Unlike the object cache
	// this is consulted INSIDE the container, per bitcode partition, so it needs
	// a mount and an env var rather than a lookup out here. Opt-in for the same
	// reason the object cache is: serving compiled objects out of a directory is
	// behaviour a test should have to ask for.
	if dir := PartCacheDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		docker = append(docker, "-v", abs+":/partcache", "-e", "ECV_PART_CACHE=/partcache")
	}
	// ⚠️ THE RAPTORMARK_* PASSTHROUGHS BELOW: the byte-affecting ones ARE part of
	// the object identity as of 2026-08-18. `ExperimentalSettings` lists them and
	// `TranslateID` folds them in, so two translations of one ELF with different
	// settings no longer collide on one key -- which was the stated precondition
	// for promoting any of them to a shipping default. The paragraph below is the
	// history, kept because it explains why the switches are shaped this way.
	//
	// Still outside the identity, on purpose: KEEP_SPLIT, LIFT_JOBS,
	// TRANSLATE_VERBOSE and the library-cache switches, none of which can change
	// an emitted byte.
	//
	// HISTORICAL:
	//
	// Each is read from the HOST environment rather than from Request, so none of
	// them reaches ObjectKey, yet several change the emitted bytes -- shared
	// naming rewrites symbol names, and the library ranges change how the module
	// is partitioned. Two translations of one ELF with different settings
	// therefore collide on the same object-cache key.
	//
	// That is deliberate and it is why they are opt-in switches for experiments
	// rather than build options: a run that flips one is expected to use its own
	// cache directory. Anything here that becomes a shipping default has to move
	// onto Request and into ObjectKey first, or the cache will serve the wrong
	// object.
	//
	// Content-stable partitioning: without it no partition can be cached, since
	// llvm-split reshuffles every bucket on any change.
	if os.Getenv("RAPTORMARK_STABLE_SPLIT") != "" {
		docker = append(docker, "-e", "ECV_STABLE_SPLIT=1")
	}
	// Keep <module-id>.split.d, which sits under the bind-mounted out dir, so the
	// partitions survive on the host. Deleting them on success is what destroyed
	// the evidence for the 17-minute partition; diagnosing a slow partition needs
	// the bitcode, and re-running the whole pipeline to get it costs minutes.
	if os.Getenv("RAPTORMARK_KEEP_SPLIT") != "" {
		docker = append(docker, "-e", "ECV_KEEP_SPLIT=1")
	}
	// NOT ONE OF THE OPT-IN SWITCHES ABOVE. ecv-prepare -- one parse for
	// link + internalize/globaldce + namespacing -- is the DEFAULT since
	// 2026-08-13; this only turns it back off. It sits here rather than in the
	// block above because it is the inverse: absent, the pipeline takes the new
	// path, so the identity caveat that heads this block does not apply. What
	// makes that safe is that the switch lives in the translation sources
	// TranslateSH hashes (internal/builder/stablesplit.go), so flipping it moves
	// the object-cache key rather than silently reusing objects the old pipeline
	// produced.
	if os.Getenv("RAPTORMARK_NO_MERGED_PREPARE") != "" {
		docker = append(docker, "-e", "ECV_NO_MERGED_PREPARE=1")
	}
	// Where to keep library-half lifts, so a closure's libraries are lifted once
	// rather than once per program. Paired with RAPTORMARK_LIB_RANGES, which is
	// what tells translate-one which addresses are library; without both, the
	// lift is whole-image as before. The lifter's identity goes in as well,
	// because a lifter patch changes the emitted IR for unchanged input and a
	// cached half from the previous lifter would be silently wrong.
	if v := libCacheHostDir(); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
		docker = append(docker, "-v", abs+":/libcache", "-e", "ECV_LIB_CACHE=/libcache",
			"-e", "ECV_BASE_ID="+b.BaseID)
		// How many elflift processes run at once when the lift is split by range.
		// Only useful for measuring: the default already reaches the ceiling,
		// because one range (the executable) dominates the wall.
		if v := os.Getenv("RAPTORMARK_LIFT_JOBS"); v != "" {
			docker = append(docker, "-e", "ECV_LIFT_JOBS="+v)
		}
	}
	// Shared naming: keep address-derived lifted names untagged and linkonce_odr,
	// so two programs of one closure produce identical partitions. Requires the
	// closure-wide fuse layout to be in effect, or the addresses -- and therefore
	// the names -- differ anyway and this changes nothing but the linkage.
	if os.Getenv("RAPTORMARK_SHARED_NAMES") != "" {
		docker = append(docker, "-e", "ECV_SHARED_NAMES=1")
	}
	// The boundary between program-specific and shared code, from
	// fuse.Layout.SharedMin. Without it namespace-object shares nothing, since
	// two programs' executables occupy the same addresses with different bodies.
	if v := os.Getenv("RAPTORMARK_SHARED_MIN"); v != "" {
		docker = append(docker, "-e", "ECV_SHARED_MIN="+v)
	}
	// Where each library sits, from fuse.FormatLibRanges, so ecv-split can keep a
	// partition inside ONE library. Without it the partitioner falls back to
	// hashing names over all buckets, which spreads every library across every
	// bucket and makes a partition reusable only between programs of the same
	// size -- measured 0 of 80 partitions shared between /bin/echo and /bin/bash
	// even though 100% of echo's library symbols are in bash.
	if v := os.Getenv("RAPTORMARK_LIB_RANGES"); v != "" {
		docker = append(docker, "-e", "ECV_LIB_RANGES="+v)
	}
	// How many clusters share one library-scoped partition. Exposed because the
	// right value depends on the closure: it is a partition SIZE target, and the
	// same target gives a small program few fat partitions and a large one many
	// thin ones. Tuning it needs a wall-clock run, not a rebuild.
	if v := os.Getenv("RAPTORMARK_LIB_CHUNK"); v != "" {
		docker = append(docker, "-e", "ECV_LIB_CHUNK="+v)
	}
	docker = append(docker, b.Image)
	docker = append(docker, r.args()...)

	cmd := exec.CommandContext(ctx, "docker", docker...)
	var stderr strings.Builder
	cmd.Stderr = containerStderr(&stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("translate: %s: %w\n%s", r.ModuleID, err, tail(stderr.String(), 40))
	}
	return nil
}

// containerStderr returns where a container's stderr should go.
//
// It is captured into a buffer so a FAILURE can report the actual error rather
// than thousands of lines of compiler warnings (see `tail`). The cost of that
// is that a SUCCESSFUL run discards everything the container said -- which is
// how elflift's `[ecv-undecoded]` report, the whole point of patch 0057, came
// out empty on a real image while working perfectly on a test one.
//
// With RAPTORMARK_TRANSLATE_VERBOSE=1 the stream is also forwarded live. Live
// rather than dumped at the end because a translate takes half an hour and the
// diagnostic is useful before it finishes.
func containerStderr(buf io.Writer) io.Writer {
	if os.Getenv("RAPTORMARK_TRANSLATE_VERBOSE") == "" {
		return buf
	}
	return io.MultiWriter(buf, os.Stderr)
}

// tail returns the last n lines, so a failure reports the actual error rather
// than thousands of lines of compiler warnings.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// LinkRequest is the final ecvisor link: compile the generated registry.c and
// link it with the namespaced lifted objects and libecvisor.a into one module.
// translate-one stops at the per-program `.o`; this is the step that produces a
// runnable wasm, and it is separate because one link consumes N translations.
type LinkRequest struct {
	// Registry is the host path of the registry.c from internal/link.RegistryC.
	Registry string
	// Objects are host paths of the per-program objects translate-one produced.
	// They must live in the same directory.
	Objects []string
	// Out is the host path of the wasm module to write.
	Out string
	// InlineCallHistory records in the module that its objects were lifted with
	// `--inline-call-history`. It MUST match how they were translated: it is
	// what permits `RAPTORMARK_ECV_INLINE_CH=1` to take effect at run time, and
	// ecvisor refuses the gate without it.
	InlineCallHistory bool
	// WasmOptLevel overrides link-all's finalisation level (ECV_WASM_OPT_LEVEL).
	// Empty selects link-all's default of -O0; see builder/link-all.sh on why
	// anything higher does not scale to a lifted module.
	WasmOptLevel string
	// SideOut, when set, is a host directory that ALSO receives one PIC side
	// module per program -- the artifact an embedder instantiates separately.
	// The flat module is produced either way; this does not choose between the
	// two paths, it produces both from one set of objects. See
	// .agents/docs/MULTIMODULE.md §5b and internal/builder.LinkAll.sideLink.
	SideOut string
}

// toolFlags are the link-all flags that come from the request rather than from
// the mounts, split out so they can be asserted without Docker.
//
// ⚠️ `--inline-call-history` was MISSING here until 2026-08-18. The field was
// declared and documented as load-bearing -- it writes the `__ecv_ch_built = 1`
// marker that permits `RAPTORMARK_ECV_INLINE_CH=1` to take effect -- and nothing
// read it, so a module linked through this path from objects lifted WITH the
// inline call history never carried the marker, and the runtime gate refused
// silently. A struct field that is never read is invisible in review precisely
// because it looks like it is doing something.
func (r LinkRequest) toolFlags() []string {
	var f []string
	if r.InlineCallHistory {
		f = append(f, "--inline-call-history")
	}
	if r.SideOut != "" {
		f = append(f, "--side-out", "/side")
	}
	return f
}

func (r LinkRequest) validate() error {
	if r.Registry == "" || r.Out == "" || len(r.Objects) == 0 {
		return fmt.Errorf("translate: Registry, Out and at least one object are required")
	}
	dir := filepath.Dir(r.Objects[0])
	for _, o := range r.Objects[1:] {
		if filepath.Dir(o) != dir {
			return fmt.Errorf("translate: all objects must share a directory, got %s and %s", r.Objects[0], o)
		}
	}
	return nil
}

// Link runs link-all in the builder image. The image's ENTRYPOINT selects the
// translate-one subcommand, so this overrides it to reach link-all instead.
func (b Builder) Link(ctx context.Context, r LinkRequest) error {
	if err := r.validate(); err != nil {
		return err
	}
	regDir, err := filepath.Abs(filepath.Dir(r.Registry))
	if err != nil {
		return err
	}
	objDir, err := filepath.Abs(filepath.Dir(r.Objects[0]))
	if err != nil {
		return err
	}
	outDir, err := filepath.Abs(filepath.Dir(r.Out))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// link-all writes registry.o beside the registry, so that mount is writable.
	docker := []string{
		"run", "--rm",
		"-v", regDir + ":/reg",
		"-v", objDir + ":/objs:ro",
		"-v", outDir + ":/out",
	}
	if r.WasmOptLevel != "" {
		docker = append(docker, "-e", "ECV_WASM_OPT_LEVEL="+r.WasmOptLevel)
	}
	objs := make([]string, len(r.Objects))
	for i, o := range r.Objects {
		objs[i] = "/objs/" + filepath.Base(o)
	}
	if r.SideOut != "" {
		sideDir, err := filepath.Abs(r.SideOut)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(sideDir, 0o755); err != nil {
			return err
		}
		docker = append(docker, "-v", sideDir+":/side")
	}
	docker = append(docker,
		"--entrypoint", "/usr/local/bin/raptormark-tools", b.Image, "link-all",
		"--registry", "/reg/"+filepath.Base(r.Registry),
		"--out", "/out/"+filepath.Base(r.Out),
		"--objs", strings.Join(objs, " "),
	)
	docker = append(docker, r.toolFlags()...)
	cmd := exec.CommandContext(ctx, "docker", docker...)
	var stderr strings.Builder
	cmd.Stderr = containerStderr(&stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("translate: linking %s: %w\n%s", r.Out, err, tail(stderr.String(), 40))
	}
	return nil
}
