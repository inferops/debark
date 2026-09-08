package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/snapshot"
)

func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Capture, synthesize and inspect target snapshots.",
		Long: "Capture the state of an offline target (everything its own apt would\n" +
			"need to resolve a request as the target would, and nothing more by\n" +
			"default), and inspect a snapshot of either kind.\n" +
			"\n" +
			"When there is no target to capture yet -- hardware not installed, or no\n" +
			"access to the air-gapped side -- `from-base` synthesizes a snapshot from a\n" +
			"stock release instead. That is an assumption about the machine rather than\n" +
			"a measurement of it, and every snapshot records which of the two it is.",
	}
	cmd.AddCommand(
		newSnapshotCreateCmd(),
		newSnapshotInspectCmd(),
		newSnapshotFromBaseCmd(),
		newSnapshotListBasesCmd(),
	)
	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	var out string
	var redact, noKeyrings bool
	var labels map[string]string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Capture this machine's package state.",
		Long: "Capture the target's apt/dpkg state into a portable snapshot archive.\n" +
			"Requires no root privileges and writes nothing outside --out.",
		Args: cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			opts := snapshot.CaptureOptions{
				Redact:          redact,
				Labels:          labels,
				IncludeKeyrings: !noKeyrings,
			}
			snap, fs, err := snapshot.Capture(ctx.Context, opts)
			if err != nil {
				return err
			}
			if err := snapshot.WriteArchive(out, snap, fs); err != nil {
				return err
			}
			digest, _ := snapshot.Digest(snap)

			if ctx.JSON() {
				b, err := json.MarshalIndent(map[string]any{
					"path":   out,
					"digest": digest,
					"target": snap.Target,
				}, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}

			fmt.Fprintf(ctx.Stdout, "%s %s\n", ctx.Style.OK("snapshot created:"), out)
			fmt.Fprintf(ctx.Stdout, "  target:    %s %s (%s) %s\n",
				snap.Target.DistroID, snap.Target.VersionID, snap.Target.Codename, snap.Target.Arch)
			fmt.Fprintf(ctx.Stdout, "  installed: %d packages\n", snap.InstalledCount)
			fmt.Fprintf(ctx.Stdout, "  digest:    sha256:%s\n", digest)
			if len(snap.Warnings) > 0 {
				fmt.Fprintf(ctx.Stdout, "  %s   %d (see --json for detail)\n", ctx.Style.Warn("warnings:"), len(snap.Warnings))
			}
			return nil
		}),
	}

	cmd.Flags().StringVar(&out, "out", "snapshot.tar.zst", "output path for the snapshot archive")
	cmd.Flags().BoolVar(&redact, "redact", false, "strip machine id, proxy settings and labels")
	cmd.Flags().BoolVar(&noKeyrings, "no-keyrings", false, "do not capture keyrings referenced by Signed-By (build then has no key material: Signed-By pins are stripped and --approved-keys cannot pass)")
	cmd.Flags().StringToStringVar(&labels, "label", nil, "attach operator-supplied metadata (hostname, site, ticket) as KEY=VAL; repeatable")
	return cmd
}

func newSnapshotInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect FILE",
		Short: "Show a snapshot, and say whether it was measured or assumed.",
		Args:  cobra.ExactArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			archive, err := snapshot.Open(ctx.Context, args[0])
			if err != nil {
				return err
			}
			defer func() { _ = archive.Close() }()
			snap := archive.Snapshot

			if ctx.JSON() {
				b, err := json.MarshalIndent(snap, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}

			fmt.Fprintf(ctx.Stdout, "%s\n", ctx.Style.Bold(archive.Path))
			// Provenance first, above the target, because it changes how
			// everything below it should be read: the same target line means
			// "this is what the machine is" for a captured snapshot and "this
			// is what the machine is assumed to be" for a synthesized one.
			fmt.Fprintf(ctx.Stdout, "  origin:      %s\n", originLine(ctx, snap))
			fmt.Fprintf(ctx.Stdout, "  target:      %s %s (%s) %s\n",
				snap.Target.DistroID, snap.Target.VersionID, snap.Target.Codename, snap.Target.Arch)
			if len(snap.Target.ForeignArchs) > 0 {
				fmt.Fprintf(ctx.Stdout, "  foreign:     %v\n", snap.Target.ForeignArchs)
			}
			fmt.Fprintf(ctx.Stdout, "  apt/dpkg:    %s / %s\n", snap.Target.APTVersion, snap.Target.DpkgVersion)
			fmt.Fprintf(ctx.Stdout, "  created:     %s\n", snap.CreatedAt)
			fmt.Fprintf(ctx.Stdout, "  digest:      sha256:%s\n", archive.Digest)
			fmt.Fprintf(ctx.Stdout, "  installed:   %d packages\n", snap.InstalledCount)
			fmt.Fprintf(ctx.Stdout, "  sources:     %d\n", len(snap.APT.Sources))
			fmt.Fprintf(ctx.Stdout, "  preferences: %d\n", len(snap.APT.Preferences))
			fmt.Fprintf(ctx.Stdout, "  keyrings:    %d (%d fingerprints)\n", len(snap.APT.Keyrings)+len(snap.APT.Trusted), len(snap.KeyringFingerprints))
			if len(snap.Redactions) > 0 {
				fmt.Fprintf(ctx.Stdout, "  redacted:    %v\n", snap.Redactions)
			}
			if len(snap.Labels) > 0 {
				fmt.Fprintf(ctx.Stdout, "  labels:      %v\n", snap.Labels)
			}
			if len(snap.Warnings) > 0 {
				fmt.Fprintf(ctx.Stdout, "  %s    %d\n", ctx.Style.Warn("warnings:"), len(snap.Warnings))
				for _, w := range snap.Warnings {
					fmt.Fprintf(ctx.Stdout, "    - %s\n", w)
				}
			}
			return nil
		}),
	}
}
