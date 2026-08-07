package translate

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var b = Builder{
	Image:       "raptormark-builder:test",
	BaseID:      "sha256:ea734017fd3660a02aec8d985e0892d00ffddd741f5c279b6f388a578e59c85b",
	TranslateSH: "08ce848126a2e3bd8c62cef84949316e9cc7f239d8df3efa99dccd8d2e88d3c0",
}

const elfA = "aaaa1111"
const elfB = "bbbb2222"

func TestTranslateIDIsStable(t *testing.T) {
	if b.TranslateID(elfA, Options{}) != b.TranslateID(elfA, Options{}) {
		t.Error("TranslateID is not deterministic")
	}
	// Defaults must be applied before hashing, or an explicit "upstream" and an
	// empty Runtime would key differently for the same translation.
	if b.TranslateID(elfA, Options{}) != b.TranslateID(elfA, Options{Runtime: "upstream", Target: "aarch64-wasi32"}) {
		t.Error("defaults not normalised before hashing")
	}
}

// An opt-in option that is OFF must not move the key, or adding it would
// invalidate every cached object in the store for a feature nobody enabled.
//
// This is only sound because the flag-off codegen was proven byte-identical:
// lifting the same fixture without elfconv patch 0060, and with it applied but
// the flag off, gave bitcode with the same SHA-256. Without that proof the
// option would have to be hashed unconditionally. The literal below is what
// pins the guarantee — if a future edit starts folding the option in
// unconditionally, this fails rather than silently costing hours of re-lifting.
func TestTranslateIDIsUnchangedWhenInlineCallHistoryIsOff(t *testing.T) {
	off := b.TranslateID(elfA, Options{})
	explicit := b.TranslateID(elfA, Options{InlineCallHistory: false})
	if off != explicit {
		t.Error("an explicitly-false option must hash as absent")
	}
	// The ORIGINAL formula, spelled out independently rather than pinned as a
	// literal. A hard-coded digest would be circular -- it would record whatever
	// the current code emits -- and would also have to be re-derived whenever the
	// fixture Builder changes. This is the exact `hashParts` call TranslateID
	// made before `InlineCallHistory` existed.
	beforeTheOptionExisted := hashParts(
		idVersion, b.BaseID, b.TranslateSH, "upstream", "aarch64-wasi32", "promote=false", elfA,
	)
	if off != beforeTheOptionExisted {
		t.Errorf("adding InlineCallHistory moved the default key:\n got %s\nwant %s\n"+
			"Every cached object for the default path just became a miss.", off, beforeTheOptionExisted)
	}
}

func TestTranslateIDSeparatesEveryInput(t *testing.T) {
	base := b.TranslateID(elfA, Options{})
	cases := map[string]string{
		"different elf":       b.TranslateID(elfB, Options{}),
		"different runtime":   b.TranslateID(elfA, Options{Runtime: "ecvisor"}),
		"promote":             b.TranslateID(elfA, Options{Promote: true}),
		"inline call history": b.TranslateID(elfA, Options{InlineCallHistory: true}),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s: TranslateID collided with base", name)
		}
	}
	// The whole point of Lever A: a new base image or an edited translation
	// pipeline must invalidate every object.
	other := b
	other.BaseID = "sha256:deadbeef"
	if other.TranslateID(elfA, Options{}) == base {
		t.Error("base image change did not invalidate the id")
	}
	other = b
	other.TranslateSH = "72795e05327c32188c2fb4751b134a48b0cf3385357b741f94918cdb7b61af23"
	if other.TranslateID(elfA, Options{}) == base {
		t.Error("translation-pipeline change did not invalidate the id")
	}
}

func TestTranslateIDLengthPrefixingPreventsSmearing(t *testing.T) {
	// Without length prefixes, moving a character across a field boundary would
	// hash identically. These two builders differ only in where the split falls.
	x := Builder{BaseID: "ab", TranslateSH: "cd"}
	y := Builder{BaseID: "a", TranslateSH: "bcd"}
	if x.TranslateID(elfA, Options{}) == y.TranslateID(elfA, Options{}) {
		t.Error("adjacent fields smear into each other; length prefixing is broken")
	}
}

// ecvisorReq builds a translation whose ELF and fragment really exist, since
// ObjectKey hashes both files' contents.
func ecvisorReq(t *testing.T, elf, frag string) Request {
	t.Helper()
	dir := t.TempDir()
	elfPath := filepath.Join(dir, "prog")
	if err := os.WriteFile(elfPath, []byte(elf), 0o755); err != nil {
		t.Fatal(err)
	}
	fragPath := filepath.Join(dir, "frag.c")
	if err := os.WriteFile(fragPath, []byte(frag), 0o644); err != nil {
		t.Fatal(err)
	}
	return Request{
		ELF: elfPath, OutDir: filepath.Join(dir, "out"),
		ModuleID: "prog_ab12cd34ef56", Fragment: fragPath, Keep: "ecv_program_0",
		Options: Options{Runtime: "ecvisor"},
	}
}

func mustKey(t *testing.T, r Request) string {
	t.Helper()
	k, err := b.ObjectKey(r)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestObjectKeyCoversEverythingBakedIntoTheObject(t *testing.T) {
	base := ecvisorReq(t, "\x7fELFaaaa", "ecv_program_0")
	key := mustKey(t, base)
	if key != mustKey(t, ecvisorReq(t, "\x7fELFaaaa", "ecv_program_0")) {
		t.Error("ObjectKey is not deterministic across equal requests")
	}

	// Each of these changes the object's bytes, so each must miss.
	keepSym := base
	keepSym.Keep = "ecv_program_1"
	// namespace-object tags every local with the module id.
	modID := base
	modID.ModuleID = "prog_ff99ee88dd77"

	for name, r := range map[string]Request{
		"different elf":       ecvisorReq(t, "\x7fELFbbbb", "ecv_program_0"),
		"different fragment":  ecvisorReq(t, "\x7fELFaaaa", "ecv_program_1"),
		"different keep":      keepSym,
		"different module id": modID,
	} {
		if mustKey(t, r) == key {
			t.Errorf("%s: ObjectKey collided with the base request", name)
		}
	}
}

func TestObjectKeyIgnoresTheFragmentForUpstream(t *testing.T) {
	// Upstream output has no fragment and no keep symbol, so it is
	// index-independent: only the elf, the toolchain and the module id matter.
	a := ecvisorReq(t, "\x7fELFaaaa", "ecv_program_0")
	a.Options = Options{Runtime: "upstream"}
	a.Fragment, a.Keep = "", ""
	bq := ecvisorReq(t, "\x7fELFaaaa", "ecv_program_7")
	bq.Options = Options{Runtime: "upstream"}
	bq.Fragment, bq.Keep = "", ""
	if mustKey(t, a) != mustKey(t, bq) {
		t.Error("upstream object key should not depend on the fragment")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	r := ecvisorReq(t, "\x7fELFaaaa", "ecv_program_0")
	key := mustKey(t, r)

	if hit, err := c.Get(key, r); err != nil || hit {
		t.Fatalf("empty cache reported hit=%v err=%v", hit, err)
	}

	// Stand in for a completed translate-one run.
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(r.OutDir, r.ModuleID+".o")
	if err := os.WriteFile(obj, []byte("object bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key, r); err != nil {
		t.Fatal(err)
	}
	// Putting the same key twice must not fail: the key is content-addressed,
	// so a concurrent writer's entry is equally valid.
	if err := c.Put(key, r); err != nil {
		t.Fatalf("second Put failed: %v", err)
	}

	// A fresh output directory, as a later run would have.
	r2 := r
	r2.OutDir = filepath.Join(t.TempDir(), "out")
	hit, err := c.Get(key, r2)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("populated cache reported a miss")
	}
	got, err := os.ReadFile(filepath.Join(r2.OutDir, r.ModuleID+".o"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "object bytes" {
		t.Errorf("served object = %q, want %q", got, "object bytes")
	}
}

func TestCacheTreatsAPartialEntryAsAMiss(t *testing.T) {
	// An upstream translation contributes both a .o and a .wasm. An entry
	// holding only one of them must not be served, or the link would consume a
	// module that was never written.
	c := Cache{Dir: t.TempDir()}
	r := ecvisorReq(t, "\x7fELFaaaa", "")
	r.Options = Options{Runtime: "upstream"}
	r.Fragment, r.Keep = "", ""
	key := mustKey(t, r)

	dir := filepath.Join(c.Dir, key[:2], key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, r.ModuleID+".o"), []byte("obj"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hit, err := c.Get(key, r); err != nil || hit {
		t.Errorf("partial entry served: hit=%v err=%v", hit, err)
	}
}

func TestCachedArtifactsAreReadOnly(t *testing.T) {
	// Get hardlinks, so the served file shares an inode with the store. Writing
	// through it would corrupt every future hit; read-only mode makes that fail.
	c := Cache{Dir: t.TempDir()}
	r := ecvisorReq(t, "\x7fELFaaaa", "ecv_program_0")
	key := mustKey(t, r)
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.OutDir, r.ModuleID+".o"), []byte("obj"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key, r); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(c.Dir, key[:2], key, r.ModuleID+".o")
	info, err := os.Stat(cached)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("cached artifact mode = %v, want no write bits", info.Mode().Perm())
	}
}

func TestCacheFromEnv(t *testing.T) {
	t.Setenv(cacheEnv, "")
	if c := CacheFromEnv(); c != nil {
		t.Errorf("unset %s should disable caching, got %+v", cacheEnv, c)
	}
	t.Setenv(cacheEnv, "/var/tmp/objs")
	if c := CacheFromEnv(); c == nil || c.Dir != "/var/tmp/objs" {
		t.Errorf("CacheFromEnv() = %+v, want Dir=/var/tmp/objs", c)
	}
}

// The id must not depend on the toolchain. namespace-object stamps it onto every
// local symbol, so if it moved with the lifter, a one-instruction patch would
// rename every symbol in the object and no compiled partition could be reused.
func TestModuleIDFollowsTheELFNotTheToolchain(t *testing.T) {
	const elfSHA = "ab12cd34ef567890"
	a := Builder{Image: "img-a", BaseID: "sha256:aaa", TranslateSH: "111"}
	b := Builder{Image: "img-b", BaseID: "sha256:bbb", TranslateSH: "222"}
	opts := Options{Runtime: "ecvisor"}

	if a.TranslateID(elfSHA, opts) == b.TranslateID(elfSHA, opts) {
		t.Fatal("two different toolchains produced the same TranslateID; test is vacuous")
	}
	if got, want := ModuleID("/usr/bin/prog", elfSHA), ModuleID("/usr/bin/prog", elfSHA); got != want {
		t.Fatalf("not deterministic: %q vs %q", got, want)
	}
	// Different ELF content must still give a different id, or two programs
	// would collide on every namespaced symbol at the ecvisor link.
	if ModuleID("/usr/bin/prog", elfSHA) == ModuleID("/usr/bin/prog", "ffffffffffffffff") {
		t.Error("different ELF contents shared a module id")
	}
}

func TestModuleID(t *testing.T) {
	id := "ab12cd34ef567890"
	for in, want := range map[string]string{
		"/usr/bin/prog":                       "prog_ab12cd34ef56",
		"prog":                                "prog_ab12cd34ef56",
		"/usr/lib/postgresql/18/bin/postgres": "postgres_ab12cd34ef56",
		// Non-identifier characters would break the generated C symbol names.
		"lib-foo.so.6": "lib_foo_so_6_ab12cd34ef56",
		"9lives":       "p9lives_ab12cd34ef56",
	} {
		if got := ModuleID(in, id); got != want {
			t.Errorf("ModuleID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestArgs(t *testing.T) {
	r := Request{
		ELF: "/host/path/postgres", OutDir: "/out", ModuleID: "postgres_ab12",
		Fragment: "/host/work/frag_3.c", Keep: "ecv_program_3",
		Options: Options{Runtime: "ecvisor", Promote: true},
	}
	got := strings.Join(r.args(), " ")
	// Paths must be the in-container mount points, not host paths.
	for _, want := range []string{
		"--elf /in/postgres", "--out /out", "--module-id postgres_ab12",
		"--runtime ecvisor", "--promote",
		"--fragment /work/frag_3.c", "--keep ecv_program_3",
		"--target aarch64-wasi32",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q\ngot: %s", want, got)
		}
	}
	if strings.Contains(got, "/host/") {
		t.Errorf("host paths leaked into container args: %s", got)
	}
}

func TestRequestArgsUpstreamOmitsFragment(t *testing.T) {
	r := Request{ELF: "/x/hello", OutDir: "/out", ModuleID: "hello_ab12"}
	got := strings.Join(r.args(), " ")
	for _, unwanted := range []string{"--fragment", "--keep", "--promote"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("upstream args should not contain %q: %s", unwanted, got)
		}
	}
}

func TestRequestValidate(t *testing.T) {
	cases := map[string]Request{
		"missing elf":       {OutDir: "/o", ModuleID: "m"},
		"missing out":       {ELF: "/e", ModuleID: "m"},
		"missing module id": {ELF: "/e", OutDir: "/o"},
		"bad runtime":       {ELF: "/e", OutDir: "/o", ModuleID: "m", Options: Options{Runtime: "nope"}},
		"bad target":        {ELF: "/e", OutDir: "/o", ModuleID: "m", Options: Options{Target: "x86"}},
		// ecvisor without a fragment fails deep inside translate-one; catch it here.
		"ecvisor no fragment": {ELF: "/e", OutDir: "/o", ModuleID: "m", Options: Options{Runtime: "ecvisor"}},
	}
	for name, r := range cases {
		if err := r.validate(); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestRequestValidateAcceptsDefaults(t *testing.T) {
	// Regression: validate() checked Options before applying defaults, so an
	// unset Target ("") was rejected even though withDefaults fills it in. Every
	// realistic caller leaves Target unset.
	cases := map[string]Request{
		"upstream defaults": {ELF: "/e", OutDir: "/o", ModuleID: "m"},
		"ecvisor defaults": {ELF: "/e", OutDir: "/o", ModuleID: "m",
			Fragment: "/w/f.c", Keep: "ecv_program_0", Options: Options{Runtime: "ecvisor"}},
	}
	for name, r := range cases {
		if err := r.validate(); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

func TestLinkRequestValidate(t *testing.T) {
	ok := LinkRequest{Registry: "/r/registry.c", Objects: []string{"/o/a.o", "/o/b.o"}, Out: "/w/out.wasm"}
	if err := ok.validate(); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	for name, r := range map[string]LinkRequest{
		"no registry": {Objects: []string{"/o/a.o"}, Out: "/w/o.wasm"},
		"no objects":  {Registry: "/r/registry.c", Out: "/w/o.wasm"},
		"no out":      {Registry: "/r/registry.c", Objects: []string{"/o/a.o"}},
		// link-all takes one --objs list against a single mount, so objects
		// scattered across directories would silently not be found.
		"split dirs": {Registry: "/r/registry.c", Out: "/w/o.wasm",
			Objects: []string{"/o/a.o", "/other/b.o"}},
	} {
		if err := r.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// ⚠️ This field was declared, documented as load-bearing, and never read. A
// module linked from objects lifted WITH the inline call history then carried no
// `__ecv_ch_built` marker, so `RAPTORMARK_ECV_INLINE_CH=1` was refused at run
// time with nothing to point at. Found 2026-08-18 while plumbing --side-out
// through the same function.
func TestLinkForwardsInlineCallHistory(t *testing.T) {
	on := LinkRequest{InlineCallHistory: true}.toolFlags()
	if !slices.Contains(on, "--inline-call-history") {
		t.Errorf("the flag must reach link-all, got %q", on)
	}
	off := LinkRequest{}.toolFlags()
	if slices.Contains(off, "--inline-call-history") {
		t.Errorf("a default link must not claim the marker, got %q", off)
	}
}

// --side-out is what makes both artifacts come out of one link. The container
// path is fixed because the host directory is bind-mounted at /side; passing the
// HOST path would name a directory the container cannot see.
func TestLinkForwardsSideOutAsTheContainerPath(t *testing.T) {
	f := LinkRequest{SideOut: "/home/me/build/side"}.toolFlags()
	i := slices.Index(f, "--side-out")
	if i < 0 || i+1 >= len(f) {
		t.Fatalf("--side-out missing from %q", f)
	}
	if f[i+1] != "/side" {
		t.Errorf("--side-out must name the MOUNT point, got %q", f[i+1])
	}
	if slices.Contains(LinkRequest{}.toolFlags(), "--side-out") {
		t.Error("a request that did not ask for side modules must not emit the flag")
	}
}

// A DEFAULT build must key exactly as it did before the experimental switches
// joined the identity. If this fails, every cached object in the project has
// been invalidated by a change that cannot alter one of them.
func TestDefaultBuildKeysUnchangedByExperimentalSettings(t *testing.T) {
	for _, name := range experimentalVars {
		t.Setenv(name, "")
	}
	if got := ExperimentalSettings(); got != nil {
		t.Fatalf("a default environment must contribute nothing, got %q", got)
	}
	// ⚠️ The first version of this test compared only the LENGTH of the key
	// against a fabricated constant. It passed, it read as "the key is pinned",
	// and it would have passed against any 64-character hex string -- an
	// assertion written in terms of the very thing it claimed to check.
	//
	// The literal below is the real key, recorded 2026-08-18. It cannot prove
	// what the key was BEFORE experimental.go (that is an argument, made in
	// `ExperimentalSettings`: a clean environment returns nil, and appending nil
	// to a slice changes nothing). What it does is make any FUTURE change to the
	// default identity fail here, loudly, with the reason attached -- because a
	// change to this value means every cached object in the project just became
	// unreachable.
	const want = "8290151d4bee8ef60935808fb4a6b0cf1710a2fbae799c7b6bd8adea8f17ce96"
	if got := b.TranslateID("elfsha", Options{Runtime: "ecvisor"}); got != want {
		t.Errorf("the DEFAULT object identity moved:\n got %s\nwant %s\n"+
			"Every cached object keys differently now. If that is intended, batch it "+
			"with other pipeline changes and update this literal; if not, something "+
			"reached TranslateID that should not have.", got, want)
	}
}

// ⚠️ NAMED HERE, not read from `experimentalVars`. The first version iterated
// over the list under test, so DELETING a switch from that list broke nothing --
// the test simply stopped checking it, and a byte-affecting switch would have
// silently left the object identity. That is the same shape as the `maxLinks`
// guard this project has been caught by before: an assertion written in terms of
// the very thing it claims to check.
//
// Adding a switch to the runtime list is caught by
// TestTheIdentityListIsExactlyThese, below.
var byteAffecting = []string{
	"RAPTORMARK_NO_STABLE_SPLIT",
	"RAPTORMARK_NO_MERGED_PREPARE",
	"RAPTORMARK_SHARED_NAMES",
	"RAPTORMARK_SHARED_MIN",
	"RAPTORMARK_LIB_RANGES",
	"RAPTORMARK_LIB_CHUNK",
}

// The list and this test must agree in BOTH directions: the loop below proves
// each named switch reaches the key, and this proves the runtime list holds
// exactly those and nothing else.
func TestTheIdentityListIsExactlyThese(t *testing.T) {
	got := slices.Clone(experimentalVars)
	want := slices.Clone(byteAffecting)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the object-identity switch list changed:\n got %q\nwant %q\n"+
			"Adding one invalidates every cached object; removing one lets two "+
			"different builds collide on a single key. Either is a decision.", got, want)
	}
}

// Each byte-affecting switch must move the key, or a cache entry can be served
// for a build it does not describe.
func TestEachByteAffectingSwitchMovesTheKey(t *testing.T) {
	opts := Options{Runtime: "ecvisor"}
	for _, name := range experimentalVars {
		t.Setenv(name, "")
	}
	base := b.TranslateID("elfsha", opts)
	for _, name := range byteAffecting {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "1")
			if got := b.TranslateID("elfsha", opts); got == base {
				t.Errorf("%s does not reach the object key; two builds with "+
					"different settings would collide on it", name)
			}
		})
	}
}

// ...and the ones that cannot change a byte must NOT move it, or every unrelated
// debugging run invalidates the cache.
func TestSchedulingAndDebugSwitchesDoNotMoveTheKey(t *testing.T) {
	opts := Options{Runtime: "ecvisor"}
	for _, name := range experimentalVars {
		t.Setenv(name, "")
	}
	base := b.TranslateID("elfsha", opts)
	for _, name := range []string{
		"RAPTORMARK_KEEP_SPLIT", "RAPTORMARK_LIFT_JOBS",
		"RAPTORMARK_TRANSLATE_VERBOSE", "RAPTORMARK_LIB_CACHE",
		"RAPTORMARK_NO_LIB_CACHE",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "1")
			if got := b.TranslateID("elfsha", opts); got != base {
				t.Errorf("%s reaches the object key; it cannot change an emitted "+
					"byte, so it would invalidate every cached object for nothing", name)
			}
		})
	}
}

// Two DIFFERENT values of one switch must key differently: RAPTORMARK_LIB_CHUNK
// is a size target, so "4" and "8" are different partitionings, not the same
// "enabled" state.
func TestASwitchesVALUEReachesTheKey(t *testing.T) {
	opts := Options{Runtime: "ecvisor"}
	t.Setenv("RAPTORMARK_LIB_CHUNK", "4")
	four := b.TranslateID("elfsha", opts)
	t.Setenv("RAPTORMARK_LIB_CHUNK", "8")
	if eight := b.TranslateID("elfsha", opts); eight == four {
		t.Error("only the switch's presence reaches the key, not its value")
	}
}

// TestLinkRequestProfileReachesTheTool guards the same class of defect the
// InlineCallHistory comment above records: a declared, documented field that
// nothing reads.
//
// It matters more here than for most flags. The profile selects which ecvisor
// ARCHIVE is linked, and therefore whether the module has a loader backend at
// all. A Profile that never reached link-all would produce a module that links,
// runs, and cannot ask a host for anything -- with the caller believing it had
// asked for exactly that.
func TestLinkRequestProfileReachesTheTool(t *testing.T) {
	f := LinkRequest{Profile: "hosted"}.toolFlags()
	var seen bool
	for i, a := range f {
		if a == "--profile" {
			if i+1 >= len(f) || f[i+1] != "hosted" {
				t.Fatalf("--profile is not followed by its value: %q", f)
			}
			seen = true
		}
	}
	if !seen {
		t.Errorf("Profile never reaches link-all: %q.\nThe archive selects the "+
			"loader backend, so a module built this way could not ask a host to "+
			"load anything, and nothing about the failure would point here.", f)
	}

	// The DEFAULT must stay bare: every shipping artifact was linked without
	// --profile, and TestLinkArgsMatchTheScript asserts that argv verbatim.
	for _, a := range (LinkRequest{}).toolFlags() {
		if a == "--profile" {
			t.Error("an unset Profile still passes --profile; the default link argv " +
				"is asserted verbatim elsewhere and every existing artifact used it")
		}
	}
	// And it composes with --side-out rather than replacing it.
	both := LinkRequest{Profile: "hosted", SideOut: "/x/side"}.toolFlags()
	var hasSide, hasProfile bool
	for _, a := range both {
		hasSide = hasSide || a == "--side-out"
		hasProfile = hasProfile || a == "--profile"
	}
	if !hasSide || !hasProfile {
		t.Errorf("--side-out and --profile do not compose: %q", both)
	}
}

// TestObjectKeyIgnoresJobs is the guard `translate.go`'s `Jobs` field says
// exists.
//
// ❗ IT DID NOT. Found 2026-08-27 by auditing every `Test*` name referenced in a
// comment against the tests actually defined: `Request.Jobs` carries
// "TestObjectKeyIgnoresJobs holds this", and nothing in this package so much as
// mentioned `Jobs`. The invariant held by construction -- `ObjectKey` reads
// `TranslateID`, `ModuleID`, `Keep` and the fragment, never `Jobs` -- so nothing
// was broken. What was missing is the thing that would NOTICE if it broke.
//
// # What it costs if it breaks
//
// `Jobs` caps translate-one's concurrent codegen processes. It cannot change a
// single byte of the emitted object. If it reached the key, every build that
// passed a different `--jobs` would miss a cache that costs HOURS to refill, and
// the symptom is "the object cache stopped working" -- a slow build, not a
// failure, which is the kind of regression that survives for months.
//
// ⚠️ The field's placement on `Request` rather than `Options` is what enforces
// this: `Options` feeds `TranslateID`. The comment there explains the reasoning;
// this makes the reasoning checkable.
func TestObjectKeyIgnoresJobs(t *testing.T) {
	base := ecvisorReq(t, "\x7fELFaaaa", "ecv_program_0")
	base.Jobs = 0
	key := mustKey(t, base)

	for _, jobs := range []int{1, 8, 32} {
		r := base
		r.Jobs = jobs
		if got := mustKey(t, r); got != key {
			t.Errorf("ObjectKey changed when Jobs went 0 -> %d (%s -> %s).\n"+
				"Jobs caps concurrent codegen and cannot change the emitted object, so "+
				"putting it in the key makes every build with a different --jobs miss a "+
				"cache that costs hours to refill. The symptom is a slow build rather "+
				"than a failure, which is how it would survive unnoticed.", jobs, key, got)
		}
	}

	// ❗ POSITIVE CONTROL. Every assertion above is an EQUALITY, and equality is
	// also what a broken `mustKey` returning a constant would give. Something
	// that genuinely belongs in the key must still move it.
	other := base
	other.Keep = "ecv_program_1"
	if mustKey(t, other) == key {
		t.Fatal("changing Keep did not change the ObjectKey, so the equality checks " +
			"above would pass against a key that ignores everything")
	}
}

// TestEveryEnvSwitchIsClassified closes the one gap the two list tests above
// leave: a switch that is READ but appears in neither list.
//
// `TestTheIdentityListIsExactlyThese` compares two hand-maintained lists, so it
// fires when someone edits `experimentalVars`. It cannot fire when someone adds
// `os.Getenv("RAPTORMARK_SOMETHING")` to this package and classifies it nowhere
// -- and that is the direction that costs, because the default for an
// unclassified switch is "not in the object identity", i.e. two builds that
// differ collide on one cache key and the wrong object is served.
//
// ⚠️ `nonByteAffecting` is TRANSCRIBED from experimental.go's "What is NOT here,
// and why" comment, not read from it. That comment is prose and parsing it would
// make this test agree with itself; transcription means the comment is checked
// too. Verified 2026-08-27: it names exactly these five, and this package reads
// exactly eleven RAPTORMARK_* switches.
var nonByteAffecting = []string{
	"RAPTORMARK_KEEP_SPLIT",        // keeps intermediates on disk
	"RAPTORMARK_LIB_CACHE",         // where cached library halves live
	"RAPTORMARK_LIFT_JOBS",         // concurrency, like Request.Jobs
	"RAPTORMARK_NO_LIB_CACHE",      // the same switch, negated
	"RAPTORMARK_TRANSLATE_VERBOSE", // logging
}

// translateSrc locates this package's own non-test sources, under `go test` and
// under Bazel. Modelled on `e2e/abiagree_test.go`'s `runtimeSrc`.
//
// ❌ Both are TRIED and the failure is FATAL. A guard that skips when it cannot
// find its input goes quiet in exactly the environment nobody watches.
func translateSrc(t *testing.T, name string) string {
	t.Helper()
	for _, p := range []string{
		name, // go test, from internal/translate/
		filepath.Join("internal", "translate", name), // bazel runfiles root
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	wd, _ := os.Getwd()
	t.Fatalf("cannot find internal/translate/%s from %s.\n"+
		"Under `bazel test` this needs `:gosrcs` in the test target's `data`.", name, wd)
	return ""
}

func TestEveryEnvSwitchIsClassified(t *testing.T) {
	// The package's own sources. Test files are excluded on purpose: a switch a
	// TEST reads is a test gate, not a translation knob.
	sources := []string{"cache.go", "experimental.go", "translate.go"}

	read := map[string]bool{}
	envRead := regexp.MustCompile(`os\.Getenv\("(RAPTORMARK_[A-Z0-9_]+)"\)`)
	for _, f := range sources {
		b, err := os.ReadFile(translateSrc(t, f))
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		for _, m := range envRead.FindAllSubmatch(b, -1) {
			read[string(m[1])] = true
		}
	}

	// ❗ FATAL ON AN EMPTY SCAN. Every assertion below is "each found name is in
	// a list", which is vacuously true of nothing. If `os.Getenv` is ever
	// wrapped in a helper, this regex stops matching and the test passes forever
	// while checking nothing -- the failure mode AGENTS.md flags for every
	// scan-based guard in this tree.
	if len(read) == 0 {
		t.Fatal("the scan found NO os.Getenv(\"RAPTORMARK_*\") calls in " +
			"cache.go/experimental.go/translate.go. The pattern has stopped matching " +
			"-- most likely the reads moved behind a helper -- so this test would " +
			"pass while checking nothing.")
	}

	classified := map[string]string{}
	for _, n := range experimentalVars {
		classified[n] = "experimentalVars (in the object identity)"
	}
	for _, n := range nonByteAffecting {
		classified[n] = "nonByteAffecting (deliberately out of it)"
	}

	for name := range read {
		if _, ok := classified[name]; !ok {
			t.Errorf("%s is read by internal/translate and classified NOWHERE.\n"+
				"Every switch this package reads must be either in `experimentalVars`, "+
				"because it changes the emitted object, or in `nonByteAffecting`, "+
				"because it cannot.\n"+
				"❗ Doing nothing is not neutral: an unclassified switch is silently "+
				"OUT of the object identity, so two builds that differ collide on one "+
				"cache key and the wrong object is served -- which is the failure "+
				"experimental.go exists to prevent.", name)
		}
	}

	// The other direction: a list naming a switch nothing reads is dead weight
	// and, worse, suggests coverage that is not there.
	for name := range classified {
		if !read[name] {
			t.Errorf("%s is classified as %q but this package never reads it. Either "+
				"the read was removed and the list was not, or the name is misspelled -- "+
				"and a misspelled entry in experimentalVars contributes nothing to the "+
				"key while looking like it does.", name, classified[name])
		}
	}
}
