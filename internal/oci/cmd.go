package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"raptormark/internal/image"
	"raptormark/internal/rootfs"
)

// Pack is the `raptormark oci` subcommand: take a translated module and its
// sidecar and write an importable OCI image tar.
type Pack struct {
	Module  string `name:"module" required:"" type:"path" help:"The translated .wasm."`
	Sidecar string `name:"sidecar" type:"path" help:"The rfs image. Omit only for a module that needs no filesystem."`
	Ref     string `name:"ref" required:"" help:"Image reference, e.g. ghcr.io/me/app:latest."`
	Out     string `name:"out" short:"o" required:"" type:"path" help:"Tar file to write."`

	Format string `name:"format" default:"rootfs" enum:"rootfs,wasm-layers" help:"rootfs keeps the shim's image handling stock (only the engine's exception-handling proposal is needed); wasm-layers is the wasm-OCI shape and additionally needs a shim that materialises the sidecar layer."`
	OS     string `name:"os" default:"wasip1" enum:"wasip1,wasi,wasip2" help:"The image config's os. wasip1 matches runwasi's own builder; use wasi for Docker, whose Wasm workloads are documented as --platform=wasi/wasm. The shim itself only checks the architecture."`

	// The personality. --from reads it off the source image, which is the
	// normal path; the rest override individual fields.
	From    string   `name:"from" help:"Inherit Env/Cmd/WorkingDir from this image's config (needs docker)."`
	Env     []string `name:"env" help:"KEY=VALUE, repeatable. Appends to (and overrides) --from."`
	Cwd     string   `name:"cwd" help:"Guest working directory. Default / or --from's WorkingDir."`
	User    string   `name:"user" help:"uid[:gid] the guest believes it runs as. Default 0:0."`
	Argv    []string `name:"arg" help:"Guest argv, repeatable. Default --from's Entrypoint+Cmd."`
	Verbose bool     `name:"verbose" short:"v" help:"Report what went into the image."`
}

func (c *Pack) Run() error {
	boot, err := c.personality()
	if err != nil {
		return err
	}

	spec := Spec{
		Ref:         c.Ref,
		Format:      Format(c.Format),
		Module:      c.Module,
		Sidecar:     c.Sidecar,
		OS:          c.OS,
		Personality: boot,
	}

	// A wasm-layers image with a sidecar asks more of the shim than rootfs does:
	// the sidecar arrives as a layer, but ecvisor opens it as a file, and that
	// image has no filesystem. Say so at build time rather than let it surface at
	// `ctr run` as a guest with an empty rootfs and no boot record.
	if spec.SidecarNeedsShim() {
		fmt.Fprintln(os.Stderr, "oci: WARNING: --format wasm-layers with a sidecar needs more from the shim.")
		fmt.Fprintf(os.Stderr, "  The sidecar rides as layer %s,\n", MediaTypeRootfsLayer)
		fmt.Fprintln(os.Stderr, "  which the shim must accept and write to the guest filesystem before")
		fmt.Fprintln(os.Stderr, "  starting the module; ecvisor opens it with fs::read. --format rootfs")
		fmt.Fprintln(os.Stderr, "  keeps the shim's image handling stock. (Either way the shim must enable")
		fmt.Fprintln(os.Stderr, "  the engine's exception-handling proposal -- no released shim does.)")
	}

	if err := os.MkdirAll(filepath.Dir(c.Out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(c.Out)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := Build(f, filepath.Dir(c.Out), spec); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if c.Verbose {
		c.report(spec, boot)
	}
	fmt.Printf("oci: wrote %s (%s, %s, %s/wasm)\n", c.Out, c.Ref, spec.format(), spec.OS)
	return nil
}

// personality assembles the boot record the image config mirrors.
func (c *Pack) personality() (rootfs.Boot, error) {
	var boot rootfs.Boot
	boot.Cwd = "/"

	if c.From != "" {
		cfg, err := image.Inspect(context.Background(), c.From)
		if err != nil {
			return boot, err
		}
		boot.Env = cfg.Env
		boot.Argv = append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
		if cfg.WorkingDir != "" {
			boot.Cwd = cfg.WorkingDir
		}
	}

	boot.Env = append(boot.Env, c.Env...)
	if len(c.Argv) > 0 {
		boot.Argv = c.Argv
	}
	if c.Cwd != "" {
		boot.Cwd = c.Cwd
	}
	if c.User != "" {
		uid, gid, err := parseUser(c.User)
		if err != nil {
			return boot, err
		}
		boot.UID, boot.GID = uid, gid
	}
	return boot, nil
}

func (c *Pack) report(spec Spec, boot rootfs.Boot) {
	fmt.Printf("  module   %s\n", spec.Module)
	if spec.Sidecar != "" {
		fmt.Printf("  sidecar  %s -> %s\n", spec.Sidecar, SidecarPath)
	} else {
		fmt.Printf("  sidecar  (none)\n")
	}
	fmt.Printf("  argv     %q\n", boot.Argv)
	fmt.Printf("  cwd      %s\n", boot.Cwd)
	fmt.Printf("  user     %d:%d\n", boot.UID, boot.GID)
	fmt.Printf("  env      %d entries\n", len(boot.Env))
}

// parseUser accepts uid or uid:gid, numeric only — the guest's ids are numbers
// in the boot record and there is no passwd file here to resolve a name against.
func parseUser(s string) (uint32, uint32, error) {
	uidStr, gidStr, ok := strings.Cut(s, ":")
	uid, err := strconv.ParseUint(uidStr, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("oci: --user %q: uid must be numeric", s)
	}
	if !ok {
		return uint32(uid), uint32(uid), nil
	}
	gid, err := strconv.ParseUint(gidStr, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("oci: --user %q: gid must be numeric", s)
	}
	return uint32(uid), uint32(gid), nil
}
