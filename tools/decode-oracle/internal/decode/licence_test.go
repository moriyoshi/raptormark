// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This module is LGPL-2.1-or-later while the root module is Apache-2.0, and the
// separation is the only reason the root module gets to be Apache-2.0 at all.
// The per-file header is what carries that fact to anyone reading one file in
// isolation -- a grep, a code-search hit, a vendored copy.
//
// It is also a claim that decays: it was true of all 12 .go files when written,
// and was false within the same session, because an MCP server arrived with
// five more. Documents cannot hold this invariant; only a test can.
//
// ⚠️ The walk starts at the MODULE ROOT, not this package. A test scoped to
// internal/decode/ would have passed throughout the window in which
// cmd/decode-mcp/ and internal/mcp/ were unlicensed, which is exactly the
// failure it exists to catch.
const (
	wantCopyright = "// Copyright 2026 The raptormark Authors"
	wantSPDX      = "// SPDX-License-Identifier: LGPL-2.1-or-later"
)

func TestEveryGoFileCarriesTheLicenceHeader(t *testing.T) {
	// This file lives at <module>/internal/decode/, so the module root is two up.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	// Prove we resolved what we think we did before trusting the walk: a wrong
	// root would walk an empty tree and report success.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod, so the walk below proves nothing: %v", root, err)
	}

	var scanned int
	var bad []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		lines := strings.SplitN(string(b), "\n", 3)
		if len(lines) < 2 || lines[0] != wantCopyright || lines[1] != wantSPDX {
			bad = append(bad, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// A walk that found nothing is a broken test, not a clean module.
	if scanned == 0 {
		t.Fatalf("scanned no .go files under %s", root)
	}
	if len(bad) != 0 {
		t.Errorf("%d of %d .go files lack the two-line licence header:\n\t%s\n\n"+
			"This module embeds LGPL-2.1-or-later material and is licensed to match.\n"+
			"Prepend to each file, before the package doc comment:\n\n\t%s\n\t%s\n",
			len(bad), scanned, strings.Join(bad, "\n\t"), wantCopyright, wantSPDX)
	}
}

// The upstream QEMU tables are pinned and must never be edited in place, so the
// header rule above deliberately does not reach them. This records that the
// exemption is intentional and bounded: it covers data files only, and
// qemudecode.go -- raptormark's embed shim, which merely lives beside them --
// is covered by the rule like any other .go file.
func TestUpstreamDataFilesAreNotExpectedToCarryOurHeader(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	vendored := filepath.Join(root, "third_party", "qemu-decode")
	for _, name := range []string{"a64.decode", "sve.decode", "LICENSE.LGPL-2.1"} {
		b, err := os.ReadFile(filepath.Join(vendored, name))
		if err != nil {
			t.Fatalf("reading vendored %s: %v", name, err)
		}
		if strings.Contains(string(b), wantCopyright) {
			t.Errorf("%s contains raptormark's copyright line; upstream files are "+
				"verbatim and must not be edited in place (see PROVENANCE.md)", name)
		}
	}
}
