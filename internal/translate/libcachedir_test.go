package translate

import (
	"path/filepath"
	"testing"
)

// The library cache defaults to a location beside an existing persistent cache
// root. There is no separate enable flag — the directory IS the switch — so
// these cases are the whole of the default's behaviour.
func TestLibCacheHostDir(t *testing.T) {
	cases := []struct {
		name           string
		explicit, part string
		object, off    string
		want           string
	}{
		{name: "off with no roots at all"},
		{name: "explicit wins", explicit: "/x", part: "/p", object: "/o", want: "/x"},
		{name: "partition root preferred", part: "/p", object: "/o", want: filepath.Join("/p", "lib-lifts")},
		{name: "object root as fallback", object: "/o", want: filepath.Join("/o", "lib-lifts")},
		{name: "opt-out beats everything", explicit: "/x", part: "/p", object: "/o", off: "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RAPTORMARK_LIB_CACHE", c.explicit)
			t.Setenv(partCacheEnv, c.part)
			t.Setenv(cacheEnv, c.object)
			t.Setenv("RAPTORMARK_NO_LIB_CACHE", c.off)
			if got := libCacheHostDir(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
