// Command raptormark is the project's harness: the build-image driver that runs
// on the host, and the translate-one / link-all steps that run inside the
// builder image (where this same binary is installed as the entrypoint).
//
// These were three bash scripts under builder/. The CLI surface is unchanged, so
// internal/translate drives them over `docker run` exactly as before.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"raptormark/internal/builder"
	"raptormark/internal/oci"
	"raptormark/internal/serve"
)

type CLI struct {
	BuildImage   builder.BuildImage   `cmd:"" name:"build-image" help:"Build the raptormark builder image and the two base images below it. Runs on the host."`
	BuildTools   builder.BuildTools   `cmd:"" name:"build-tools" help:"Rebuild only builder/_tools/raptormark-builder-tools, the prebuilt pipeline binary the image COPYs in. Needed before a raw docker build; see the side-build recipe in CLAUDE.md."`
	TranslateOne builder.TranslateOne `cmd:"" name:"translate-one" help:"Translate one aarch64 ELF to wasm. Runs inside the builder image."`
	LinkAll      builder.LinkAll      `cmd:"" name:"link-all" help:"Link the lifted objects and libecvisor.a into one WASI module. Runs inside the builder image."`
	OCI          oci.Pack             `cmd:"" name:"oci" help:"Pack a module and its sidecar into an importable OCI image tar."`
	Serve        serve.Serve          `cmd:"" name:"serve" help:"Host the browser embedder and its artifacts, with the Content-Type compileStreaming requires. Runs on the host."`
}

// The decode oracle is deliberately NOT a subcommand here.
//
// `tools/decode-oracle` embeds QEMU's decodetree tables (LGPL-2.1-or-later) and
// is licensed the same way; it is its own Go module with its own binary, so that
// obligation never reaches this one. raptormark's lifter lineage
// (third_party/elfconv, remill, LLVM) is Apache-2.0, which conflicts with GPLv2
// and LGPLv2.1 specifically -- not with the GPL family at large. It is a
// developer-only analysis tool that reads a log and prints a report; nothing it
// touches reaches a translated object or module.wasm.
//
//	cd tools/decode-oracle && go run ./cmd/decode-report -log <translate log>
//
// ❌ Do not re-add it here for convenience. See tools/decode-oracle/README.md.

func main() {
	var cli CLI
	parser := kong.Must(&cli,
		kong.Name("raptormark"),
		kong.Description("Ahead-of-time translation of aarch64 Linux container images into WebAssembly."),
		kong.UsageOnError(),
	)

	// Parse and run by hand rather than via kong.Parse so the scripts' exit
	// codes survive: 2 for a bad invocation, 1 for a step that failed.
	ctx, err := parser.Parse(os.Args[1:])
	if err != nil {
		parser.Errorf("%s", err)
		os.Exit(2)
	}
	if err := ctx.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", ctx.Command(), err)
		var usage *builder.UsageError
		if errors.As(err, &usage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
