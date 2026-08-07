// Package link generates the C glue that binds N independently translated
// programs into one ecvisor WASI module.
//
// RECONSTRUCTED on 2026-08-01. The original package was lost with the working
// tree and never entered a Docker layer. It is rebuilt here against three
// surviving sources that together pin the contract down exactly:
//
//   - runtime/src/abi.rs — declares `EcvProgram` with the note that field order
//     and types "must match the C struct emitted by internal/link.RegistryC
//     exactly", and declares the three registry symbols `ecv_programs`,
//     `ecv_program_count`, `ecv_program_size`. That file names this package and
//     this function, so the names below are recovered, not invented.
//   - third_party/elfconv runtime/Memory.h — the `extern "C"` declarations of
//     every `_ecv_*` symbol elflift emits, giving each one's exact C type and
//     whether it is a scalar or an array.
//   - the translate-one and link-all steps (internal/builder, once bash scripts
//     under builder/) — how the generated C is consumed (see "Pipeline" below).
//
// The 18 EcvProgram fields map 1:1 onto elflift's symbols; the mapping was
// confirmed against `llvm-nm` output from a real lifted module.
//
// # Pipeline
//
// For each program i, translate-one is invoked as:
//
//	translate-one --runtime ecvisor --fragment <frag.c> --keep ecv_program_<i> ...
//
// It compiles the fragment to bitcode, llvm-links it with the lifted bitcode,
// then runs `internalize,globaldce` with `--internalize-public-api-list=
// ecv_program_<i>`. That makes every `_ecv_*` singleton file-local, so N objects
// link into one module without colliding, leaving exactly one exported symbol
// per program: its EcvProgram descriptor.
//
// link-all then compiles the registry and links it with the N objects and
// libecvisor.a:
//
//	link-all --registry registry.c --objs "p0.o p1.o ..." --out out.wasm
//
// # ABI
//
// Every EcvProgram field is a pointer — 4 bytes on wasm32 — so the struct has
// no interleaved padding and sizeof is 18*4 = 72. Scalar singletons (the entry
// func and pc, the counts, the phdr sizes) are stored as *addresses*, because a
// C static initializer can take a const scalar's address but not its runtime
// value; the Rust accessors dereference them. `ecv_program_size` carries
// sizeof(EcvProgram) so the runtime can trip on ABI drift at startup.
package link

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// Program is one translated program to be linked into the module.
type Program struct {
	// Name is the program name the runtime reports, a content hash. It is
	// emitted as a NUL-terminated string and reached via EcvProgram.name.
	Name string
	// Index is the program's position in the registry. It determines the
	// exported symbol name, ecv_program_<Index>, which must match the --keep
	// argument passed to translate-one for this program.
	Index int
}

// Symbol is the exported descriptor symbol for this program — the one symbol
// translate-one's internalize pass preserves.
func (p Program) Symbol() string { return fmt.Sprintf("ecv_program_%d", p.Index) }

// ecvProgramC is the C definition of the descriptor. It is emitted into both
// the fragments and the registry: the registry needs the complete type to take
// sizeof, and each fragment needs it to define its instance.
//
// Field order and types mirror runtime/src/abi.rs exactly. Do not reorder.
const ecvProgramC = `#include <stdint.h>

/* Lifted function: (memory arena bytes, CPU state, pc, runtime manager). */
typedef void (*LiftedFunc)(uint8_t *, void *, uint64_t, void *);

/* Must match EcvProgram in runtime/src/abi.rs. Every field is a pointer. */
typedef struct {
  const uint8_t *name;
  const LiftedFunc *entry_func;
  const uint64_t *fun_vmas;
  const LiftedFunc *fun_ptrs;
  uint64_t ***block_ptrs;
  const uint64_t **block_vmas;
  const uint64_t *block_sizes;
  const uint64_t *block_fn_vmas;
  const uint64_t *block_count;
  const uint8_t **data_names;
  const uint64_t *data_vmas;
  const uint64_t *data_sizes;
  const uint8_t **data_bytes;
  const uint64_t *data_num;
  const uint8_t *e_ph;
  const uint32_t *e_phent;
  const uint32_t *e_phnum;
  const uint64_t *entry_pc;
} EcvProgram;
`

// externsC declares the lifted singletons the fragment points at. Types are
// taken verbatim from third_party/elfconv runtime/Memory.h.
const externsC = `
/* Emitted by elflift into this program's lifted bitcode; llvm-linked with this
   fragment, then internalized. Types from elfconv runtime/Memory.h. */
extern const LiftedFunc _ecv_entry_func;
extern const uint64_t _ecv_entry_pc;
extern uint64_t _ecv_fun_vmas[];
extern LiftedFunc _ecv_fun_ptrs[];
extern uint64_t **_ecv_block_address_ptrs_array[];
extern const uint64_t *_ecv_block_address_vmas_array[];
extern const uint64_t _ecv_block_address_size_array[];
extern const uint64_t _ecv_block_address_fn_vma_array[];
extern const uint64_t _ecv_block_address_array_size;
extern const uint8_t *_ecv_data_sec_name_ptr_array[];
extern const uint64_t _ecv_data_sec_vma_array[];
extern const uint64_t _ecv_data_sec_size_array[];
extern const uint8_t *_ecv_data_sec_bytes_ptr_array[];
extern const uint64_t _ecv_data_sec_num;
extern uint8_t _ecv_e_ph[];
extern uint32_t _ecv_e_phent;
extern uint32_t _ecv_e_phnum;
`

// FragmentC returns the per-program C fragment passed to
// `translate-one --fragment`. It defines the single symbol p.Symbol(), binding
// this program's lifted `_ecv_*` singletons into one descriptor.
//
// The fragment is compiled and llvm-linked against the lifted bitcode, so the
// externs above resolve within that module — never across programs.
func FragmentC(p Program) string {
	var b strings.Builder
	fmt.Fprintf(&b, "/* Generated by internal/link for program %d (%s). Do not edit. */\n",
		p.Index, p.Name)
	b.WriteString(ecvProgramC)
	b.WriteString(externsC)
	fmt.Fprintf(&b, "\nstatic const uint8_t ecv_program_name_%d[] = %s;\n", p.Index, cString(p.Name))
	fmt.Fprintf(&b, `
EcvProgram %s = {
    .name = ecv_program_name_%d,
    .entry_func = &_ecv_entry_func,
    .fun_vmas = _ecv_fun_vmas,
    .fun_ptrs = _ecv_fun_ptrs,
    .block_ptrs = _ecv_block_address_ptrs_array,
    .block_vmas = _ecv_block_address_vmas_array,
    .block_sizes = _ecv_block_address_size_array,
    .block_fn_vmas = _ecv_block_address_fn_vma_array,
    .block_count = &_ecv_block_address_array_size,
    .data_names = _ecv_data_sec_name_ptr_array,
    .data_vmas = _ecv_data_sec_vma_array,
    .data_sizes = _ecv_data_sec_size_array,
    .data_bytes = _ecv_data_sec_bytes_ptr_array,
    .data_num = &_ecv_data_sec_num,
    .e_ph = _ecv_e_ph,
    .e_phent = &_ecv_e_phent,
    .e_phnum = &_ecv_e_phnum,
    .entry_pc = &_ecv_entry_pc,
};
`, p.Symbol(), p.Index)
	return b.String()
}

// RegistryC returns the registry translation unit passed to
// `link-all --registry`. It defines the three symbols runtime/src/abi.rs
// declares: ecv_programs, ecv_program_count and ecv_program_size.
//
// Programs are emitted in ascending Index order; the runtime indexes this table
// directly, so the order is the program numbering and must agree with the
// --keep symbol each object was internalized to.
func RegistryC(progs []Program) (string, error) {
	if len(progs) == 0 {
		return "", fmt.Errorf("link: registry needs at least one program")
	}
	seen := make(map[int]string, len(progs))
	for _, p := range progs {
		if p.Index < 0 {
			return "", fmt.Errorf("link: program %q has negative index %d", p.Name, p.Index)
		}
		if prev, dup := seen[p.Index]; dup {
			return "", fmt.Errorf("link: duplicate program index %d (%q and %q)", p.Index, prev, p.Name)
		}
		seen[p.Index] = p.Name
	}
	// The runtime walks ecv_programs[0..count), so the indices must be exactly
	// 0..n-1 with no holes — a gap would leave a null entry and fault on deref.
	for i := range progs {
		if _, ok := seen[i]; !ok {
			return "", fmt.Errorf("link: program indices must be contiguous from 0; missing %d", i)
		}
	}

	ordered := make([]Program, len(progs))
	for _, p := range progs {
		ordered[p.Index] = p
	}

	var b strings.Builder
	fmt.Fprintf(&b, "/* Generated by internal/link for %d program(s). Do not edit. */\n", len(ordered))
	b.WriteString(ecvProgramC)
	b.WriteString("\n")
	for _, p := range ordered {
		fmt.Fprintf(&b, "extern EcvProgram %s; /* %s */\n", p.Symbol(), p.Name)
	}
	b.WriteString("\nEcvProgram *ecv_programs[] = {\n")
	for _, p := range ordered {
		fmt.Fprintf(&b, "    &%s,\n", p.Symbol())
	}
	b.WriteString("};\n")
	fmt.Fprintf(&b, "const uint64_t ecv_program_count = %d;\n", len(ordered))
	b.WriteString("const uint64_t ecv_program_size = sizeof(EcvProgram);\n")
	return b.String(), nil
}

// ExecPath is where the exec map lives inside the rfs sidecar. Mirrors
// runtime/src/execmap.rs EXEC_PATH; the runtime reads it from exactly here.
const ExecPath = "/.raptormark/exec"

// execMagic prefixes the exec map. runtime/src/execmap.rs returns an empty map
// — silently, and every exec then falls back to program 0 — if this does not
// match, so it is a hard format version, not a courtesy.
const execMagic = "RMEXEC01"

// ExecEntry maps one guest path to the content hash of the program that serves
// it. Paths an entrypoint or execve can name must appear here; programs with no
// entry are library units, merged into whichever program runs (the shared-libc
// superset unit).
type ExecEntry struct {
	// Path MUST be the CANONICAL path in the image being built -- fully
	// symlink-resolved, the form `Vfs::resolve` produces.
	//
	// The runtime tries an exact match first and then retries with the path
	// resolved through the VFS (`Programs::resolve` in runtime/src/execmap.rs),
	// so a canonical entry serves every spelling of it. A NON-canonical one
	// serves only a literal execve of that exact string, which nothing does:
	// libc spawns through /bin/sh, and on a usr-merged Debian image (postgres:17
	// among them) `/bin` is a symlink to `usr/bin`, so /bin/sh resolves to
	// /usr/bin/dash and never to /bin/dash. Registering /bin/dash there produced
	// a map that looked complete, matched nothing, and fell back to program 0 --
	// which is the same silent failure the hash check below exists to prevent,
	// arriving by a different route.
	Path string
	Hash string
}

// ExecMap encodes the exec map the runtime parses to resolve a guest path to a
// program index. Layout, from runtime/src/execmap.rs:
//
//	magic "RMEXEC01", u32 count, count × (u32 pathlen, path, u32 hashlen, hash)
//
// All integers little-endian.
//
// progs is required so every entry's hash can be checked against the registry.
// The runtime drops entries whose hash names no program *silently*, which would
// surface much later as an exec falling back to program 0 — so that is rejected
// here instead. Programs with no entry are fine: those are the library units.
func ExecMap(progs []Program, entries []ExecEntry) ([]byte, error) {
	known := make(map[string]bool, len(progs))
	for _, p := range progs {
		known[p.Name] = true
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Path == "" {
			return nil, fmt.Errorf("link: exec map entry has empty path")
		}
		if !known[e.Hash] {
			return nil, fmt.Errorf("link: exec map path %q names unknown program %q", e.Path, e.Hash)
		}
		if seen[e.Path] {
			return nil, fmt.Errorf("link: duplicate exec map path %q", e.Path)
		}
		seen[e.Path] = true
	}
	if int64(len(entries)) > math.MaxUint32 {
		return nil, fmt.Errorf("link: exec map has too many entries (%d)", len(entries))
	}

	var b []byte
	b = append(b, execMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		var err error
		if b, err = appendLenPrefixed(b, e.Path); err != nil {
			return nil, err
		}
		if b, err = appendLenPrefixed(b, e.Hash); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// ParseExecMap decodes what ExecMap encodes.
//
// It exists so the encoded map can be CHECKED after the fact -- by
// rootfs.Build, which has the tree the paths must be canonical against, and
// which receives the map as opaque bytes. Without a decoder the only enforcement
// point is the caller that happens to build the entries, and that is precisely
// the enforcement the exec map has never had.
//
// Stricter than the runtime's `parse` on purpose. The runtime returns an empty
// map on a bad magic and drops a truncated tail, because at run time there is
// nothing better to do than carry on -- but the result is every exec silently
// falling back to program 0. Here both are errors: build time is where a
// malformed map should stop.
func ParseExecMap(b []byte) ([]ExecEntry, error) {
	if len(b) < len(execMagic)+4 || string(b[:len(execMagic)]) != execMagic {
		return nil, fmt.Errorf("link: exec map does not start with %q; the runtime would read it as empty and run program 0 for every exec", execMagic)
	}
	pos := len(execMagic)
	count := binary.LittleEndian.Uint32(b[pos:])
	pos += 4
	entries := make([]ExecEntry, 0, count)
	readField := func() (string, error) {
		if pos+4 > len(b) {
			return "", fmt.Errorf("link: exec map is truncated")
		}
		n := int(binary.LittleEndian.Uint32(b[pos:]))
		pos += 4
		if n < 0 || pos+n > len(b) {
			return "", fmt.Errorf("link: exec map is truncated")
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
		entries = append(entries, ExecEntry{Path: path, Hash: hash})
	}
	if pos != len(b) {
		return nil, fmt.Errorf("link: exec map has %d trailing bytes after %d entries", len(b)-pos, count)
	}
	return entries, nil
}

func appendLenPrefixed(b []byte, s string) ([]byte, error) {
	if int64(len(s)) > math.MaxUint32 {
		return nil, fmt.Errorf("link: exec map field too long (%d bytes)", len(s))
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...), nil
}

// cString renders s as a C string literal. Program names are content hashes in
// practice, but escaping keeps a hostile or unusual name from breaking the
// generated TU.
func cString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\%03o`, c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
