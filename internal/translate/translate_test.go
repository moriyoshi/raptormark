package translate

import (
	"os"
	"path/filepath"
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
	"RAPTORMARK_STABLE_SPLIT",
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
