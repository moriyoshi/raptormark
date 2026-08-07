package link

import (
	"encoding/binary"
	"fmt"
	"math"
)

// The DLOPEN MAP: guest `.so` path -> the content hash of the unit that serves
// it.
//
// It is to `dlopen` what the exec map is to `execve`. The runtime resolves an
// execve path to a program index through the exec map; it resolves a dlopen
// path to a UNIT index through this one, and then asks the loader to make that
// unit's code reachable.
//
// WHY A SECOND MAP RATHER THAN MORE ENTRIES IN THE FIRST. They answer different
// questions and a merged table could not tell them apart: an exec-map hit means
// "replace this process's image", a dlopen-map hit means "add this to the
// current one". Worse, the failure modes are opposite -- an unknown execve path
// currently falls back to program 0 (which has caused four separate incidents),
// while an unknown dlopen path must simply fail the dlopen, which is what a real
// loader does when a plugin is absent.
//
// The encoding is deliberately IDENTICAL to the exec map's apart from the magic,
// so one reader in the runtime serves both. `TestDlMapEncodesLikeTheExecMap`
// pins that, because two formats that drift apart silently would each need their
// own parser and the second one would be the one nobody tested.

// DlPath is where the dlopen map lives inside the rfs sidecar, beside the exec
// map at ExecPath.
const DlPath = "/.raptormark/dlopen"

// dlMagic prefixes the dlopen map. As with execMagic this is a hard format
// version: a reader that does not match it must treat the map as absent, and an
// absent dlopen map means every dlopen of a unit fails.
const dlMagic = "RMDLOP01"

// DlEntry maps one guest `.so` path to the content hash of the unit serving it.
type DlEntry struct {
	// Path MUST be the CANONICAL guest path, symlink-resolved, exactly as
	// ExecEntry.Path must be and for the same reason: the runtime tries an exact
	// match first and then retries through the VFS, so a canonical entry serves
	// every spelling while a non-canonical one serves only a literal dlopen of
	// that exact string.
	//
	// This matters MORE here than for execve. A guest names a plugin through
	// whatever path its own configuration holds -- postgres builds one from
	// `$libdir`, python from `sys.path` -- so the spellings are more varied than
	// the handful of paths libc spawns through.
	Path string
	// Hash is the unit's content hash, i.e. its translate.ModuleID, the same
	// identity the registry knows it by.
	Hash string
}

// DlMap encodes the dlopen map.
//
//	magic "RMDLOP01", u32 count, count * (u32 pathlen, path, u32 hashlen, hash)
//
// All integers little-endian.
//
// `progs` is required so every entry's hash can be checked against the registry,
// for the same reason ExecMap requires it: an entry naming a unit the module
// does not contain would otherwise be discovered at run time, as a dlopen that
// resolves to nothing.
func DlMap(progs []Program, entries []DlEntry) ([]byte, error) {
	known := make(map[string]bool, len(progs))
	for _, p := range progs {
		known[p.Name] = true
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Path == "" {
			return nil, fmt.Errorf("link: dlopen map entry has empty path")
		}
		if !known[e.Hash] {
			return nil, fmt.Errorf("link: dlopen map path %q names unknown unit %q", e.Path, e.Hash)
		}
		if seen[e.Path] {
			// Two units claiming one path is precisely the collision this whole
			// mechanism exists to remove; accepting it would reintroduce
			// first-wins by the back door.
			return nil, fmt.Errorf("link: duplicate dlopen map path %q", e.Path)
		}
		seen[e.Path] = true
	}
	if int64(len(entries)) > math.MaxUint32 {
		return nil, fmt.Errorf("link: dlopen map has too many entries (%d)", len(entries))
	}

	var b []byte
	b = append(b, dlMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		var err error
		if b, err = appendDlField(b, e.Path); err != nil {
			return nil, err
		}
		if b, err = appendDlField(b, e.Hash); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// ParseDlMap decodes what DlMap encodes.
//
// Stricter than a runtime reader would be, on the same reasoning as
// ParseExecMap: at run time there is nothing better to do with a malformed map
// than carry on, but build time is where it should stop.
func ParseDlMap(b []byte) ([]DlEntry, error) {
	if len(b) < len(dlMagic)+4 || string(b[:len(dlMagic)]) != dlMagic {
		return nil, fmt.Errorf("link: dlopen map does not start with %q; a reader would treat it as absent and every dlopen of a unit would fail", dlMagic)
	}
	pos := len(dlMagic)
	count := binary.LittleEndian.Uint32(b[pos:])
	pos += 4
	entries := make([]DlEntry, 0, count)
	readField := func() (string, error) {
		if pos+4 > len(b) {
			return "", fmt.Errorf("link: dlopen map is truncated")
		}
		n := int(binary.LittleEndian.Uint32(b[pos:]))
		pos += 4
		if n < 0 || pos+n > len(b) {
			return "", fmt.Errorf("link: dlopen map is truncated")
		}
		s := string(b[pos : pos+n])
		pos += n
		return s, nil
	}
	for i := uint32(0); i < count; i++ {
		path, err := readField()
		if err != nil {
			return nil, err
		}
		hash, err := readField()
		if err != nil {
			return nil, err
		}
		entries = append(entries, DlEntry{Path: path, Hash: hash})
	}
	if pos != len(b) {
		return nil, fmt.Errorf("link: dlopen map has %d trailing bytes after %d entries", len(b)-pos, count)
	}
	return entries, nil
}

func appendDlField(b []byte, s string) ([]byte, error) {
	if int64(len(s)) > math.MaxUint32 {
		return nil, fmt.Errorf("link: dlopen map field too long (%d bytes)", len(s))
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...), nil
}
