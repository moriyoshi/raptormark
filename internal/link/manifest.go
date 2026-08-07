package link

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ManifestName is the file a link writes beside its registry.c, recording the
// programs that link actually contained.
//
// # Why this exists
//
// The registry and the exec map are two descriptions of the same program list,
// and until now each was DERIVED INDEPENDENTLY: the linker driver computed
// module ids from the fused ELFs, and the sidecar builder computed them again.
// Two derivations of one fact drift, and when they do the runtime cannot match
// an exec-map hash to a program, falls back to program 0, and the guest runs
// the wrong program under the right argv.
//
// That has happened four times, for four different reasons -- a stale sidecar
// after a re-lift, a non-canonical guest path, a sidecar built with a different
// builder tag, and a change to how module ids are derived. The runtime now
// WARNS when it happens (see runtime/src/execmap.rs), but a warning reports the
// drift; the manifest removes the second derivation that causes it.
//
// So: the linker writes this, and whoever builds the sidecar READS it rather
// than recomputing. `ExecMap` already refuses an entry naming an unknown
// program -- that check only means something when the program list came from
// the link instead of from the same guesswork as the entries.
const ManifestName = "programs.json"

// Manifest is the on-disk form. A struct rather than a bare array so fields can
// be added without breaking readers.
type Manifest struct {
	Programs []Program `json:"programs"`
}

// WriteManifest records `progs` in dir/ManifestName. Call it from whatever
// invokes link-all, with exactly the programs passed to RegistryC.
func WriteManifest(dir string, progs []Program) error {
	if err := validatePrograms(progs); err != nil {
		return err
	}
	b, err := json.MarshalIndent(Manifest{Programs: progs}, "", "  ")
	if err != nil {
		return fmt.Errorf("link: encoding manifest: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, ManifestName)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("link: writing manifest: %w", err)
	}
	return nil
}

// WriteLinkInputs writes BOTH the registry translation unit and the manifest
// into dir, and returns the registry's path.
//
// One call, because the two must describe the same program list and a driver
// that can write one without the other will eventually do so -- which is the
// original defect wearing a different hat. I introduced the manifest to remove
// a second derivation of the program list and then wired it into one of the two
// drivers that link, leaving the other able to produce a registry with no
// manifest beside it. Prefer this over calling RegistryC and WriteManifest
// separately.
func WriteLinkInputs(dir, registryName string, progs []Program) (string, error) {
	progs = SortedByIndex(progs)
	registry, err := RegistryC(progs)
	if err != nil {
		return "", err
	}
	if err := WriteManifest(dir, progs); err != nil {
		return "", err
	}
	path := filepath.Join(dir, registryName)
	if err := os.WriteFile(path, []byte(registry), 0o644); err != nil {
		return "", fmt.Errorf("link: writing registry: %w", err)
	}
	return path, nil
}

// ReadManifest loads the programs a link recorded in dir. The validation is not
// ceremony: a manifest that disagrees with itself would reintroduce exactly the
// silent mismatch it exists to prevent, so a bad one is an error here rather
// than a wrong program at runtime.
func ReadManifest(dir string) ([]Program, error) {
	path := filepath.Join(dir, ManifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("link: reading manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("link: parsing %s: %w", path, err)
	}
	if err := validatePrograms(m.Programs); err != nil {
		return nil, fmt.Errorf("link: %s: %w", path, err)
	}
	return m.Programs, nil
}

// ProgramByIndex finds a program by its registry index. Callers name a program
// by INDEX rather than by hash so that no hash is ever typed by hand -- the
// identity comes from the manifest, which came from the link.
func ProgramByIndex(progs []Program, index int) (Program, error) {
	for _, p := range progs {
		if p.Index == index {
			return p, nil
		}
	}
	return Program{}, fmt.Errorf("link: no program with index %d (the module has %d)", index, len(progs))
}

// validatePrograms enforces what the registry itself requires: indices are
// exactly 0..n-1, and names are present and distinct. The registry is an ARRAY
// indexed by position, so a gap or a duplicate index is not a cosmetic problem
// -- it means some entry of that array describes the wrong program.
func validatePrograms(progs []Program) error {
	if len(progs) == 0 {
		return fmt.Errorf("link: manifest has no programs")
	}
	seenIdx := make(map[int]bool, len(progs))
	seenName := make(map[string]bool, len(progs))
	for _, p := range progs {
		if p.Name == "" {
			return fmt.Errorf("link: program at index %d has no name", p.Index)
		}
		if p.Index < 0 || p.Index >= len(progs) {
			return fmt.Errorf("link: program %q has index %d, outside 0..%d",
				p.Name, p.Index, len(progs)-1)
		}
		if seenIdx[p.Index] {
			return fmt.Errorf("link: two programs share index %d", p.Index)
		}
		if seenName[p.Name] {
			return fmt.Errorf("link: two programs share the name %q", p.Name)
		}
		seenIdx[p.Index] = true
		seenName[p.Name] = true
	}
	return nil
}

// SortedByIndex returns the programs in registry order, which is the order
// RegistryC emits them and the order link-all expects the objects in.
func SortedByIndex(progs []Program) []Program {
	out := append([]Program(nil), progs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}
