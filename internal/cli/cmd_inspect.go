package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/internal/cli/render"
)

// inspectSchemaVersion is a CLI-local envelope, not a core/ schema: it
// exists only to give `inspect --json` one object to print, and every field
// inside it is itself a real, independently versioned core/ document
// (manifest.Manifest, lock.Lock) rather than CLI-invented shape.
const inspectSchemaVersion = "debark.inspect/v1"

type inspectJSON struct {
	SchemaVersion string             `json:"schema_version"`
	BundlePath    string             `json:"bundle_path"`
	Signed        bool               `json:"signed"`
	Manifest      *manifest.Manifest `json:"manifest"`
	Lock          *lock.Lock         `json:"lock"`
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect BUNDLE",
		Short: "Show a bundle's lock, manifest, warnings and sizes.",
		Long: "Open a bundle and print what it contains: the manifest, the lock plan\n" +
			"and its warnings, and file/package sizes. This does not verify the\n" +
			"bundle — it only parses it; run `debark verify` before trusting what\n" +
			"it says.",
		Args: cobra.ExactArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			b, err := bundle.Open(ctx.Context, args[0])
			if err != nil {
				return err
			}
			defer func() { _ = b.Close() }()

			signed := b.Signature != nil && len(b.Signature.Signatures) > 0

			if ctx.JSON() {
				out := inspectJSON{
					SchemaVersion: inspectSchemaVersion,
					BundlePath:    b.Dir,
					Signed:        signed,
					Manifest:      b.Manifest,
					Lock:          b.Lock,
				}
				enc, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(ctx.Stdout, string(enc))
				return nil
			}

			printInspectHuman(ctx, b, signed)
			return nil
		}),
	}
}

func printInspectHuman(ctx *Ctx, b *bundle.Bundle, signed bool) {
	fmt.Fprintf(ctx.Stdout, "%s\n", ctx.Style.Bold(b.Dir))
	if m := b.Manifest; m != nil {
		fmt.Fprintf(ctx.Stdout, "  bundle id: %s\n", m.BundleID)
		fmt.Fprintf(ctx.Stdout, "  created:   %s\n", m.CreatedAt)
		fmt.Fprintf(ctx.Stdout, "  tool:      %s %s (%s)\n", "debark", m.Tool.Version, m.Tool.Edition)
		fmt.Fprintf(ctx.Stdout, "  target:    %s %s (%s) %s\n", m.Target.DistroID, m.Target.VersionID, m.Target.Codename, m.Target.Arch)
		fmt.Fprintf(ctx.Stdout, "  packages:  %d (%s pool)\n", m.Repository.PackageCount, render.Bytes(m.Repository.PoolBytes))
		fmt.Fprintf(ctx.Stdout, "  files:     %d\n", len(m.Files))
	}
	signedText := ctx.Style.Warn("no")
	if signed {
		signedText = ctx.Style.OK("yes")
		for _, s := range b.Signature.Signatures {
			signedText += fmt.Sprintf(" (%s %s)", s.SignerKind, s.KeyID)
		}
	}
	fmt.Fprintf(ctx.Stdout, "  signed:    %s\n", signedText)

	if l := b.Lock; l != nil {
		fmt.Fprintf(ctx.Stdout, "  resolver:  %s backend, apt %s / dpkg %s\n", l.Resolver.Backend, l.Resolver.APTVersion, l.Resolver.DpkgVersion)
		fmt.Fprintf(ctx.Stdout, "  install:   %d packages\n", len(l.Install))
		fmt.Fprintf(ctx.Stdout, "  closed-world check: %s\n", l.ClosedWorld.Result)
		if len(l.Warnings) > 0 {
			fmt.Fprintf(ctx.Stdout, "  %s  %d\n", ctx.Style.Warn("warnings:"), len(l.Warnings))
			for _, w := range l.Warnings {
				fmt.Fprintf(ctx.Stdout, "    - [%s] %s\n", w.Code, w.Message)
			}
		}
		if len(l.Unresolved) > 0 {
			fmt.Fprintf(ctx.Stdout, "  %s %d\n", ctx.Style.Fail("unresolved:"), len(l.Unresolved))
		}
	}
}
