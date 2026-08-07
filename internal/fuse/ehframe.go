package fuse

// Function boundaries recovered from `.eh_frame`.
//
// elflift derives every function boundary from the symbol table
// (lifter/TraceManager.cpp, SetELFData). A distro shared library is stripped, so
// only its *exported* symbols survive in `.dynsym` and most of its code arrives
// with no boundary at all — measured on a fused nginx, only 34% of executable
// bytes were covered, against 99.9% for a static unstripped binary. Everything
// uncovered ends up inside one enormous `_ecv_rest_fun`, and linear
// disassembly of that span eventually reads a literal pool or padding as an
// instruction. That is what produced branch targets like 0xfffffffffcde89ec.
//
// `.eh_frame` survives stripping, and each FDE records exactly the start and
// length of the function it unwinds — 10,523 of them in a stripped
// libcrypto.so.3, covering 85% of its executable bytes, against 5,503 dynamic
// symbols. So the boundaries elflift needs are already in the input; they just
// have to be read out and handed over as symbols.
//
// Only the header of each entry is parsed. The CFI program that follows says
// how to restore registers and is irrelevant here.

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// DWARF exception-header pointer encodings (DW_EH_PE_*). The low nibble selects
// the value format, the high nibble how it is applied.
const (
	pePtr     = 0x00 // absolute, target pointer size
	peULEB128 = 0x01
	peUData2  = 0x02
	peUData4  = 0x03
	peUData8  = 0x04
	peSigned  = 0x08 // OR'd into the format for the signed variants
	peSLEB128 = 0x09
	peSData2  = 0x0a
	peSData4  = 0x0b
	peSData8  = 0x0c

	peAbs     = 0x00
	pePCRel   = 0x10
	peTextRel = 0x20
	peDataRel = 0x30
	peFuncRel = 0x40
	peAligned = 0x50

	peOmit = 0xff
)

// funcRange is one function's extent in an object's own (link-time) addresses.
type funcRange struct {
	addr uint64
	size uint64
}

// cursor reads little-endian DWARF primitives, tracking how far it has come so
// pc-relative encodings can be resolved against the section's virtual address.
type cursor struct {
	b   []byte
	pos int
	err error
}

func (c *cursor) fail(format string, args ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
}

func (c *cursor) need(n int) bool {
	if c.err != nil {
		return false
	}
	if c.pos+n > len(c.b) {
		c.fail("eh_frame: truncated at %d (want %d bytes)", c.pos, n)
		return false
	}
	return true
}

func (c *cursor) u8() uint8 {
	if !c.need(1) {
		return 0
	}
	v := c.b[c.pos]
	c.pos++
	return v
}

func (c *cursor) u16() uint16 {
	if !c.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(c.b[c.pos:])
	c.pos += 2
	return v
}

func (c *cursor) u32() uint32 {
	if !c.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(c.b[c.pos:])
	c.pos += 4
	return v
}

func (c *cursor) u64() uint64 {
	if !c.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(c.b[c.pos:])
	c.pos += 8
	return v
}

func (c *cursor) uleb() uint64 {
	var v uint64
	var shift uint
	for {
		if !c.need(1) {
			return 0
		}
		b := c.b[c.pos]
		c.pos++
		if shift < 64 {
			v |= uint64(b&0x7f) << shift
		}
		if b&0x80 == 0 {
			return v
		}
		shift += 7
		if shift > 70 {
			c.fail("eh_frame: ULEB128 too long")
			return 0
		}
	}
}

func (c *cursor) sleb() int64 {
	var v int64
	var shift uint
	for {
		if !c.need(1) {
			return 0
		}
		b := c.b[c.pos]
		c.pos++
		if shift < 64 {
			v |= int64(b&0x7f) << shift
		}
		shift += 7
		if b&0x80 == 0 {
			if shift < 64 && b&0x40 != 0 {
				v -= 1 << shift
			}
			return v
		}
		if shift > 70 {
			c.fail("eh_frame: SLEB128 too long")
			return 0
		}
	}
}

// encoded reads one pointer in DW_EH_PE encoding `enc`. `secAddr` is the
// section's virtual address, used to resolve pc-relative values.
func (c *cursor) encoded(enc uint8, secAddr uint64) uint64 {
	if enc == peOmit {
		return 0
	}
	at := secAddr + uint64(c.pos) // address of the field itself, for pcrel

	var raw uint64
	switch enc & 0x0f {
	case pePtr:
		raw = c.u64() // aarch64 is LP64; the fuser is aarch64-only
	case peULEB128:
		raw = c.uleb()
	case peUData2:
		raw = uint64(c.u16())
	case peUData4:
		raw = uint64(c.u32())
	case peUData8:
		raw = c.u64()
	case peSLEB128:
		raw = uint64(c.sleb())
	case peSData2:
		raw = uint64(int64(int16(c.u16())))
	case peSData4:
		raw = uint64(int64(int32(c.u32())))
	case peSData8:
		raw = c.u64()
	default:
		c.fail("eh_frame: unsupported pointer format %#x", enc&0x0f)
		return 0
	}

	switch enc & 0x70 {
	case peAbs:
		return raw
	case pePCRel:
		return at + raw
	case peTextRel, peDataRel, peFuncRel, peAligned:
		// Not emitted by any aarch64 toolchain we consume. Refusing beats
		// inventing a base and producing plausible-looking wrong addresses.
		c.fail("eh_frame: unsupported pointer application %#x", enc&0x70)
		return 0
	default:
		c.fail("eh_frame: unknown pointer encoding %#x", enc)
		return 0
	}
}

// ehFrameFuncs returns the function extents recorded in f's `.eh_frame`, in the
// object's own link-time addresses. A missing or unparsable section yields no
// ranges rather than an error: the symbol table remains the primary source and
// callers treat this purely as an additional supply of boundaries.
func ehFrameFuncs(f *elf.File) []funcRange {
	sec := f.Section(".eh_frame")
	if sec == nil || sec.Type == elf.SHT_NOBITS || sec.Size == 0 {
		return nil
	}
	data, err := sec.Data()
	if err != nil {
		return nil
	}
	return parseEhFrame(data, sec.Addr)
}

// parseEhFrame walks the CIE/FDE chain in `data`, which is mapped at `secAddr`.
// Split from ehFrameFuncs so it can be tested without building an ELF.
func parseEhFrame(data []byte, secAddr uint64) []funcRange {
	// FDE pointer encoding, per CIE, keyed by the CIE's offset in the section.
	cieEnc := map[uint64]uint8{}
	var out []funcRange

	c := &cursor{b: data}
	for c.err == nil && c.pos < len(data) {
		start := c.pos
		length := uint64(c.u32())
		if c.err != nil {
			break
		}
		if length == 0 {
			break // terminator
		}
		if length == 0xffffffff {
			length = c.u64()
			if c.err != nil {
				break
			}
		}
		afterLen := c.pos
		end := afterLen + int(length)
		if end > len(data) || end < afterLen {
			break // truncated section: keep whatever parsed cleanly
		}

		id := c.u32()
		if id == 0 {
			// CIE: read only far enough to learn the FDE pointer encoding.
			enc := uint8(pePtr)
			version := c.u8()
			var aug []byte
			for {
				ch := c.u8()
				if c.err != nil || ch == 0 {
					break
				}
				aug = append(aug, ch)
			}
			c.uleb() // code alignment factor
			c.sleb() // data alignment factor
			if version == 1 {
				c.u8() // return address register
			} else {
				c.uleb()
			}
			if len(aug) > 0 && aug[0] == 'z' {
				augLen := c.uleb()
				augEnd := c.pos + int(augLen)
				for _, ch := range aug[1:] {
					switch ch {
					case 'R':
						enc = c.u8()
					case 'L':
						c.u8() // LSDA encoding
					case 'P':
						pe := c.u8()
						c.encoded(pe, secAddr) // personality routine
					case 'S', 'B', 'G':
						// signal frame / pointer auth / no-op flags: no data
					default:
						// Unknown augmentation: the remaining data is not
						// self-describing, so stop trusting this CIE.
						c.fail("eh_frame: unknown augmentation %q", string(ch))
					}
				}
				if c.err == nil && augEnd >= 0 && augEnd <= len(data) {
					c.pos = augEnd // skip anything we chose not to interpret
				}
			}
			if c.err != nil {
				// A CIE we cannot read makes its FDEs unreadable too. Drop what
				// we have rather than emit guesses.
				return out
			}
			cieEnc[uint64(start)] = enc
		} else {
			// FDE: `id` is the distance back from here to its CIE.
			ciePos := uint64(afterLen) - uint64(id)
			enc, ok := cieEnc[ciePos]
			if !ok {
				c.pos = end
				continue // FDE before its CIE, or a CIE we skipped
			}
			pc := c.encoded(enc, secAddr)
			// The range is a length, so only the value format applies.
			size := c.encoded(enc&0x0f, secAddr)
			if c.err != nil {
				return out
			}
			if pc != 0 && size != 0 {
				out = append(out, funcRange{addr: pc, size: size})
			}
		}
		c.pos = end
	}
	return out
}

// covered reports whether addr falls inside one of the sorted ranges. Used to
// keep an FDE from introducing a second boundary for a function the symbol
// table already describes — elflift keys functions by address, and a spurious
// entry part-way through a real function would split it.
func covered(sorted []funcRange, addr uint64) bool {
	lo, hi := 0, len(sorted)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if sorted[mid].addr <= addr {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return false
	}
	r := sorted[lo-1]
	return addr < r.addr+r.size
}
