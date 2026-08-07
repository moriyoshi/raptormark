package oci

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// Import is one `(module, field)` pair a wasm module requires its host to
// supply.
//
// This is the SECOND axis of "will a runtime accept this module", and the one
// the package doc above used to leave out. No-wasm-proposals says the engine can
// decode the module; the import list says the embedder can satisfy it. A stock
// released runwasi shim needs both, and raptormark modules fail the second on
// wasmtime: they import WasmEdge's socket extension to `wasi_snapshot_preview1`,
// which standard preview1 does not define.
type Import struct {
	Module string
	Field  string
}

func (i Import) String() string { return i.Module + "." + i.Field }

// ModuleImports lists everything a wasm module imports, in binary order.
//
// It parses only far enough to walk the import section, but it does that
// STRICTLY: any descriptor shape it does not recognise is an error rather than a
// stopping point. That asymmetry is deliberate. A parser that gave up quietly
// would report a PREFIX of the imports, and every caller here is asking "is this
// set still what we expect" -- a short list is the answer that passes, so a
// silent truncation would read as a portability improvement.
func ModuleImports(b []byte) ([]Import, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], []byte("\x00asm")) {
		return nil, fmt.Errorf("oci: not a wasm module")
	}
	if v := binary.LittleEndian.Uint32(b[4:8]); v != 1 {
		return nil, fmt.Errorf("oci: unsupported wasm version %d", v)
	}
	p := &parser{b: b, i: 8}
	for p.i < len(p.b) {
		id, err := p.byte()
		if err != nil {
			return nil, err
		}
		size, err := p.uleb()
		if err != nil {
			return nil, err
		}
		end := p.i + int(size)
		if end > len(p.b) || end < p.i {
			return nil, fmt.Errorf("oci: section %d runs past the module", id)
		}
		if id != 2 {
			p.i = end
			continue
		}
		return p.imports(end)
	}
	// No import section is legal: a module that needs nothing from its host.
	return nil, nil
}

// ImportSet reports the imports as sorted "module.field" strings, which is the
// form a test compares. Sorted rather than binary order because the ORDER is a
// linker detail and comparing it would make this guard fire on a relink that
// changed nothing about what the module needs.
func ImportSet(b []byte) ([]string, error) {
	imps, err := ModuleImports(b)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(imps))
	for _, im := range imps {
		out = append(out, im.String())
	}
	sort.Strings(out)
	return out, nil
}

type parser struct {
	b []byte
	i int
}

func (p *parser) byte() (byte, error) {
	if p.i >= len(p.b) {
		return 0, fmt.Errorf("oci: truncated module")
	}
	v := p.b[p.i]
	p.i++
	return v, nil
}

func (p *parser) uleb() (uint64, error) {
	var v uint64
	var shift uint
	for {
		c, err := p.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			return 0, fmt.Errorf("oci: LEB128 value too long")
		}
	}
}

func (p *parser) name() (string, error) {
	n, err := p.uleb()
	if err != nil {
		return "", err
	}
	end := p.i + int(n)
	if end > len(p.b) || end < p.i {
		return "", fmt.Errorf("oci: name runs past the module")
	}
	s := string(p.b[p.i:end])
	p.i = end
	return s, nil
}

// limits, shared by table and memory descriptors. The flag byte carries more
// than a max-present bit in later proposals (0x02/0x03 are memory64, 0x04/0x05
// shared), and those are rejected rather than guessed at: a raptormark module is
// not supposed to contain them, so meeting one is a finding.
func (p *parser) limits() error {
	f, err := p.byte()
	if err != nil {
		return err
	}
	if f != 0x00 && f != 0x01 {
		return fmt.Errorf("oci: limits flag %#x needs a wasm proposal beyond 2.0", f)
	}
	if _, err := p.uleb(); err != nil {
		return err
	}
	if f == 0x01 {
		if _, err := p.uleb(); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) imports(end int) ([]Import, error) {
	n, err := p.uleb()
	if err != nil {
		return nil, err
	}
	out := make([]Import, 0, n)
	for k := uint64(0); k < n; k++ {
		mod, err := p.name()
		if err != nil {
			return nil, err
		}
		fld, err := p.name()
		if err != nil {
			return nil, err
		}
		kind, err := p.byte()
		if err != nil {
			return nil, err
		}
		switch kind {
		case 0x00: // func: typeidx
			_, err = p.uleb()
		case 0x01: // table: reftype limits
			if _, err = p.byte(); err == nil {
				err = p.limits()
			}
		case 0x02: // memory: limits
			err = p.limits()
		case 0x03: // global: valtype mut
			if _, err = p.byte(); err == nil {
				_, err = p.byte()
			}
		default:
			err = fmt.Errorf("oci: import %q has unknown kind %#x", mod+"."+fld, kind)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Import{Module: mod, Field: fld})
	}
	if p.i != end {
		return nil, fmt.Errorf("oci: import section has %d trailing bytes", end-p.i)
	}
	return out, nil
}

// Export is one name a module offers its host or its peers.
type Export struct {
	Name string
	Kind string // "func", "table", "memory", "global"
}

var exportKinds = [...]string{"func", "table", "memory", "global"}

// ModuleExports lists a module's exports, sorted by name.
//
// The companion to ModuleImports, and needed for the same reason: a side
// module's usable surface is `ecv_program_<i>` plus the two linker-generated
// initialisers, and "did the link actually export them" is not answerable from
// the link command alone (.agents/docs/MULTIMODULE.md §8).
func ModuleExports(b []byte) ([]Export, error) {
	p, end, err := findSection(b, 7)
	if err != nil || p == nil {
		return nil, err
	}
	n, err := p.uleb()
	if err != nil {
		return nil, err
	}
	out := make([]Export, 0, n)
	for k := uint64(0); k < n; k++ {
		name, err := p.name()
		if err != nil {
			return nil, err
		}
		kind, err := p.byte()
		if err != nil {
			return nil, err
		}
		if int(kind) >= len(exportKinds) {
			return nil, fmt.Errorf("oci: export %q has unknown kind %#x", name, kind)
		}
		if _, err := p.uleb(); err != nil {
			return nil, err
		}
		out = append(out, Export{Name: name, Kind: exportKinds[kind]})
	}
	if p.i != end {
		return nil, fmt.Errorf("oci: export section has %d trailing bytes", end-p.i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SideNeeds is what a PIC side module declares it must be given before it can be
// instantiated: `dylink.0`'s MEM_INFO subsection.
//
// ⚠️ The alignments are LOG2, as the section stores them. A caller that forgets
// to undo that passes 4 where 16 was meant -- still a power of two, still
// accepted by an allocator, and under-aligned. The field names say `Log2` for
// that reason.
type SideNeeds struct {
	MemSize        uint64
	MemAlignLog2   uint64
	TableSize      uint64
	TableAlignLog2 uint64
}

// SideModuleNeeds reads `dylink.0` MEM_INFO. It reports false for a module with
// no such section -- which is what a FLAT module is, so this doubles as the test
// for "was this linked as a side module".
func SideModuleNeeds(b []byte) (SideNeeds, bool, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], []byte("\x00asm")) {
		return SideNeeds{}, false, fmt.Errorf("oci: not a wasm module")
	}
	p := &parser{b: b, i: 8}
	for p.i < len(p.b) {
		id, err := p.byte()
		if err != nil {
			return SideNeeds{}, false, err
		}
		size, err := p.uleb()
		if err != nil {
			return SideNeeds{}, false, err
		}
		end := p.i + int(size)
		if end > len(p.b) || end < p.i {
			return SideNeeds{}, false, fmt.Errorf("oci: section %d runs past the module", id)
		}
		if id != 0 {
			p.i = end
			continue
		}
		name, err := p.name()
		if err != nil {
			return SideNeeds{}, false, err
		}
		if name != "dylink.0" {
			p.i = end
			continue
		}
		for p.i < end {
			sub, err := p.byte()
			if err != nil {
				return SideNeeds{}, false, err
			}
			ssz, err := p.uleb()
			if err != nil {
				return SideNeeds{}, false, err
			}
			next := p.i + int(ssz)
			if sub == 1 { // WASM_DYLINK_MEM_INFO
				var n SideNeeds
				for _, f := range []*uint64{&n.MemSize, &n.MemAlignLog2, &n.TableSize, &n.TableAlignLog2} {
					if *f, err = p.uleb(); err != nil {
						return SideNeeds{}, false, err
					}
				}
				return n, true, nil
			}
			p.i = next
		}
		return SideNeeds{}, false, fmt.Errorf("oci: dylink.0 has no MEM_INFO subsection")
	}
	return SideNeeds{}, false, nil
}

// findSection positions a parser at the payload of the first section with `id`,
// returning its end offset. A nil parser means the module has no such section,
// which is legal for every section this package reads.
func findSection(b []byte, want byte) (*parser, int, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], []byte("\x00asm")) {
		return nil, 0, fmt.Errorf("oci: not a wasm module")
	}
	p := &parser{b: b, i: 8}
	for p.i < len(p.b) {
		id, err := p.byte()
		if err != nil {
			return nil, 0, err
		}
		size, err := p.uleb()
		if err != nil {
			return nil, 0, err
		}
		end := p.i + int(size)
		if end > len(p.b) || end < p.i {
			return nil, 0, fmt.Errorf("oci: section %d runs past the module", id)
		}
		if id == want {
			return p, end, nil
		}
		p.i = end
	}
	return nil, 0, nil
}
