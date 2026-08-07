package fuse

import (
	"strconv"
	"strings"
	"testing"
)

// A closure that fits must actually get the shared layout, and the report must
// say so. The report is how a build distinguishes "sharing applied" from
// "sharing silently skipped" -- without it a lost optimization looks exactly
// like a slow machine.
func TestClosureThatFitsGetsTheSharedLayout(t *testing.T) {
	a := Program{Objs: []*Object{exe("/bin/a", 0x20000), lib("/lib/libc.so.6", 0, 2<<20)}, ExeIsPIE: true}
	b := Program{Objs: []*Object{exe("/bin/b", 0x20000), lib("/lib/libc.so.6", 0, 2<<20), lib("/lib/libz.so.1", 0, 1<<20)}, ExeIsPIE: true}

	opts, rep := planOrFallback([]Program{a, b}, Options{})
	if !rep.Shared {
		t.Fatalf("closure did not share: %s", rep.Reason)
	}
	if opts.Layout == nil {
		t.Fatal("Shared reported true but no Layout was set -- the images would be packed per-image anyway")
	}
	if rep.Libraries != 2 {
		t.Errorf("Libraries = %d, want 2 (libc and libz)", rep.Libraries)
	}
	if rep.Top == 0 || rep.Top > brkStartVMA {
		t.Errorf("Top = %#x, want a nonzero address below %#x", rep.Top, uint64(brkStartVMA))
	}
}

// Overflow must DEGRADE, not fail. Making a large closure unbuildable in
// exchange for an optimization is the wrong trade.
func TestOverflowFallsBackInsteadOfFailing(t *testing.T) {
	objs := []*Object{exe("/bin/big", 0x10000)}
	for i := 0; i < 40; i++ {
		objs = append(objs, lib("/lib/lib"+strconv.Itoa(i)+".so", 0, 8<<20))
	}
	opts, rep := planOrFallback([]Program{{Objs: objs, ExeIsPIE: true}}, Options{})

	if rep.Shared {
		t.Error("a 320 MB closure reported as shared")
	}
	if opts.Layout != nil {
		t.Error("fallback left a Layout set; assignBases would then reject objects it cannot place")
	}
	if rep.Reason == "" {
		t.Error("fallback gave no reason, so a build could not log why sharing was skipped")
	}
	if !strings.Contains(rep.Reason, "fall back") {
		t.Errorf("reason should name the remedy, got %q", rep.Reason)
	}
}

// The fallback Options must be usable: assignBases with a nil Layout is the
// existing dense path, and it must still place every library.
func TestFallbackOptionsStillFuse(t *testing.T) {
	objs := []*Object{exe("/bin/big", 0x10000)}
	for i := 0; i < 40; i++ {
		objs = append(objs, lib("/lib/lib"+strconv.Itoa(i)+".so", 0, 8<<20))
	}
	p := Program{Objs: objs, ExeIsPIE: true}
	opts, _ := planOrFallback([]Program{p}, Options{})
	if err := assignBases(p.Objs, p.ExeIsPIE, opts); err != nil {
		t.Fatalf("fallback options did not fuse: %v", err)
	}
	for _, o := range p.Objs[1:] {
		if o.Base == 0 {
			t.Fatalf("%s got no base under the fallback", o.Path)
		}
	}
}
