package fuse

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// IFUNC resolution.
//
// glibc reaches memchr, strlen and friends through R_AARCH64_IRELATIVE: the slot
// holds a *resolver*, which the dynamic loader calls at startup to pick an
// implementation for the running CPU. Eager static binding has no loader and so
// no one to call it.
//
// The resolvers are tiny — 40 to 56 bytes — and do exactly one thing: read the
// glibc cpu_features struct and select between a few candidate implementations.
// So rather than execute guest code at build time, this interprets them over the
// fused image with a small aarch64 evaluator.
//
// That yields the right answer for free. cpu_features is populated by the loader
// at *runtime*; in the image it is all zeroes, so evaluating against the image
// selects the baseline implementation on every resolver. Baseline is exactly
// what a WASM target wants — the variants exist to exploit SVE/MTE/etc., none of
// which survive lifting.
//
// The evaluator is deliberately narrow: it implements only the instructions
// these resolvers use and fails loudly on anything else, rather than guessing.

// maxIfuncSteps bounds interpretation. A resolver is a dozen instructions; this
// is a runaway guard, not a real limit.
const maxIfuncSteps = 256

// ifuncState is the register/flag file for one resolver evaluation.
type ifuncState struct {
	x          [32]uint64 // x[31] reads as zero (xzr)
	n, z, c, v bool
}

func (s *ifuncState) get(r uint32) uint64 {
	if r == 31 {
		return 0 // xzr
	}
	return s.x[r]
}

func (s *ifuncState) set(r uint32, v uint64) {
	if r == 31 {
		return // writes to xzr are discarded
	}
	s.x[r] = v
}

// readMem reads 8 bytes from the fused address space. Addresses inside an
// object's span but past its file contents read as zero, which is what makes
// cpu_features evaluate to "no features".
func readMem(objs []*Object, addr uint64) (uint64, error) {
	for _, o := range objs {
		lo, hi := o.addr(o.lo), o.addr(o.hi)
		if addr < lo || addr+8 > hi {
			continue
		}
		return binary.LittleEndian.Uint64(o.image[addr-lo:]), nil
	}
	return 0, fmt.Errorf("address %#x is not mapped in the fused image", addr)
}

// resolveIfunc interprets the resolver at resolverPC and returns the address of
// the implementation it selects.
func resolveIfunc(objs []*Object, resolverPC uint64) (uint64, error) {
	var st ifuncState
	pc := resolverPC
	for step := 0; step < maxIfuncSteps; step++ {
		word, err := readInsn(objs, pc)
		if err != nil {
			return 0, err
		}
		next := pc + 4
		switch {
		// The whole HINT space is a no-op for our purposes: NOP itself, the BTI
		// landing pads that guard every indirect-branch target on a
		// BTI-enabled build, and the pointer-auth prologue hints (PACIASP /
		// AUTIASP). Matching only bare NOP made a resolver that begins with
		// `bti c` — which is every resolver in a Debian libc — unfuseable.
		// Mask keeps bits 31:12 and 4:0, ignoring CRm and op2.
		case word&0xfffff01f == 0xd503201f: // HINT: nop, bti, paciasp, ...

		case word == 0xd65f03c0: // ret
			return st.get(0), nil

		case word&0x9f000000 == 0x90000000: // adrp Xd, imm
			immlo := (word >> 29) & 0x3
			immhi := (word >> 5) & 0x7ffff
			imm := int64(signExtend((immhi<<2)|immlo, 21)) << 12
			st.set(word&0x1f, uint64(int64(pc&^0xfff)+imm))

		case word&0xff800000 == 0x91000000: // add Xd, Xn, #imm
			imm := (word >> 10) & 0xfff
			if (word>>22)&1 == 1 {
				imm <<= 12
			}
			st.set(word&0x1f, st.get((word>>5)&0x1f)+uint64(imm))

		case word&0xffc00000 == 0xf9400000: // ldr Xt, [Xn, #imm]
			off := uint64((word>>10)&0xfff) * 8
			v, err := readMem(objs, st.get((word>>5)&0x1f)+off)
			if err != nil {
				return 0, fmt.Errorf("ldr at %#x: %w", pc, err)
			}
			st.set(word&0x1f, v)

		case word&0xffc00000 == 0xd3400000: // UBFM (lsr Xd, Xn, #sh)
			immr := (word >> 16) & 0x3f
			imms := (word >> 10) & 0x3f
			if imms != 63 {
				return 0, fmt.Errorf("unsupported UBFM %#08x at %#x (only the LSR alias is handled)", word, pc)
			}
			st.set(word&0x1f, st.get((word>>5)&0x1f)>>immr)

		case word&0xff800000 == 0xf1000000: // subs Xd, Xn, #imm (cmp when Xd==xzr)
			imm := uint64((word >> 10) & 0xfff)
			if (word>>22)&1 == 1 {
				imm <<= 12
			}
			a := st.get((word >> 5) & 0x1f)
			res := a - imm
			st.setFlagsSub(a, imm, res)
			st.set(word&0x1f, res)

		case word&0xff800000 == 0xf2000000: // ands Xd, Xn, #bimm (tst when Xd==xzr)
			mask, ok := decodeBitMasks((word>>22)&1, (word>>10)&0x3f, (word>>16)&0x3f)
			if !ok {
				return 0, fmt.Errorf("undecodable logical immediate in %#08x at %#x", word, pc)
			}
			res := st.get((word>>5)&0x1f) & mask
			st.n, st.z, st.c, st.v = int64(res) < 0, res == 0, false, false
			st.set(word&0x1f, res)

		case word&0xffe00c00 == 0x9a800000: // csel Xd, Xn, Xm, cond
			cond := (word >> 12) & 0xf
			if st.cond(cond) {
				st.set(word&0x1f, st.get((word>>5)&0x1f))
			} else {
				st.set(word&0x1f, st.get((word>>16)&0x1f))
			}

		case word&0xff000010 == 0x54000000: // b.cond
			if st.cond(word & 0xf) {
				next = uint64(int64(pc) + int64(signExtend((word>>5)&0x7ffff, 19))*4)
			}

		default:
			return 0, fmt.Errorf("unsupported instruction %#08x at %#x", word, pc)
		}
		pc = next
	}
	return 0, fmt.Errorf("resolver at %#x did not return within %d instructions", resolverPC, maxIfuncSteps)
}

func readInsn(objs []*Object, addr uint64) (uint32, error) {
	for _, o := range objs {
		lo, hi := o.addr(o.lo), o.addr(o.hi)
		if addr < lo || addr+4 > hi {
			continue
		}
		return binary.LittleEndian.Uint32(o.image[addr-lo:]), nil
	}
	return 0, fmt.Errorf("instruction address %#x is not mapped in the fused image", addr)
}

func (s *ifuncState) setFlagsSub(a, b, res uint64) {
	s.n = int64(res) < 0
	s.z = res == 0
	s.c = a >= b // unsigned: no borrow
	s.v = ((a^b)&(a^res))>>63 != 0
}

// cond evaluates a standard aarch64 condition code against the flags.
func (s *ifuncState) cond(c uint32) bool {
	var r bool
	switch c >> 1 {
	case 0:
		r = s.z
	case 1:
		r = s.c
	case 2:
		r = s.n
	case 3:
		r = s.v
	case 4:
		r = s.c && !s.z
	case 5:
		r = s.n == s.v
	case 6:
		r = s.n == s.v && !s.z
	case 7:
		r = true
	}
	if c&1 == 1 && c != 0xf {
		r = !r
	}
	return r
}

func signExtend(v uint32, bitsN uint) int64 {
	shift := 64 - bitsN
	return int64(uint64(v)<<shift) >> shift
}

// decodeBitMasks implements the ARM pseudocode of the same name for 64-bit
// logical immediates, which is how `tst Xn, #imm` encodes its operand.
func decodeBitMasks(n, imms, immr uint32) (uint64, bool) {
	x := (n << 6) | (^imms & 0x3f)
	if x == 0 {
		return 0, false
	}
	length := uint32(31 - bits.LeadingZeros32(x))
	if length < 1 || length > 6 {
		return 0, false
	}
	esize := uint32(1) << length
	levels := esize - 1
	s := imms & levels
	r := immr & levels
	if s == levels {
		return 0, false // reserved encoding
	}

	welem := ^uint64(0) >> (64 - (s + 1)) // Ones(S+1)
	if esize < 64 {
		emask := (uint64(1) << esize) - 1
		welem &= emask
		if r != 0 {
			welem = ((welem >> r) | (welem << (esize - r))) & emask
		}
		out := uint64(0)
		for i := uint32(0); i < 64; i += esize {
			out |= welem << i
		}
		return out, true
	}
	if r != 0 {
		welem = bits.RotateLeft64(welem, -int(r))
	}
	return welem, true
}
