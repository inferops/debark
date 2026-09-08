package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// aptRootInspectSchemaVersion is a CLI-local diagnostic object, not a core/
// schema. apt-root inspect never crosses the gap and is never hashed or
// signed.
const aptRootInspectSchemaVersion = "debark.aptroot-inspect/v1"

func newAptRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "apt-root",
		Hidden: true,
		Short:  "Inspect the private apt root a snapshot would produce.",
	}
	cmd.AddCommand(newAptRootInspectCmd())
	return cmd
}

func newAptRootInspectCmd() *cobra.Command {
	var backend, image, selfBinary string

	cmd := &cobra.Command{
		Use:    "inspect SNAPSHOT",
		Hidden: true,
		Short:  "Show what backend and apt/dpkg would resolve a snapshot.",
		Long: "Hidden debugging command. Opens a snapshot, runs backend selection\n" +
			"(apt.SelectBackend) and reports what it found — the backend that\n" +
			"would run, its apt/dpkg versions, and a summary of the apt policy\n" +
			"captured in the snapshot (sources, preferences, conf, keyrings). It\n" +
			"does not itself materialise a private apt root on disk: that\n" +
			"construction is internal to Backend.Resolve; this\n" +
			"command only reports what Resolve would have to work with.",
		Args: cobra.ExactArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) (err error) {
			archive, err := snapshot.Open(ctx.Context, args[0])
			if err != nil {
				return err
			}
			defer func() { _ = archive.Close() }()

			events, closeEvents, err := ctx.NewEvidenceSink()
			if err != nil {
				return err
			}
			defer func() {
				// The event stream is often the only durable record of the
				// run (core/evidence: "this is the one sink whose Close error
				// the Sink contract says must not be swallowed"), so a
				// truncated one is a real failure, not a detail — but never a
				// failure that gets to stand in for the one the command was
				// already reporting.
				if cerr := closeEvents(); cerr != nil && err == nil {
					err = dferr.Wrap(dferr.Environment, cerr, "close event stream")
				}
			}()

			be, caps, err := apt.SelectBackend(ctx.Context, apt.Selection{
				Target:    archive.Snapshot.Target,
				Requested: lock.Backend(backend),
				Image:     image,
				Events:    events,
				SelfPath:  ctx.selfBinary(selfBinary),
			})
			if err != nil {
				return err
			}

			result := aptRootInspectResult{
				SchemaVersion: aptRootInspectSchemaVersion,
				Snapshot:      args[0],
				Target:        archive.Snapshot.Target,
				Backend:       string(be.Kind()),
				Capabilities:  caps,
				Policy: aptRootPolicySummary{
					Sources:            len(archive.Snapshot.APT.Sources),
					Preferences:        len(archive.Snapshot.APT.Preferences),
					Conf:               len(archive.Snapshot.APT.Conf),
					TrustedKeyrings:    len(archive.Snapshot.APT.Trusted),
					ReferencedKeyrings: len(archive.Snapshot.APT.Keyrings),
					KeyFingerprints:    len(archive.Snapshot.KeyringFingerprints),
					PhasedUpdatePolicy: string(archive.Snapshot.PhasedPolicyFor()),
				},
			}

			if ctx.JSON() {
				b, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}
			printAptRootHuman(ctx, result)
			return nil
		}),
	}

	cmd.Flags().StringVar(&backend, "backend", "", "auto, local or container (empty means auto)")
	cmd.Flags().StringVar(&image, "image", "", "container image override")
	cmd.Flags().StringVar(&selfBinary, "self-binary", "", selfBinaryFlagUsage)
	return cmd
}

type aptRootInspectResult struct {
	SchemaVersion string               `json:"schema_version"`
	Snapshot      string               `json:"snapshot"`
	Target        snapshot.Target      `json:"target"`
	Backend       string               `json:"backend"`
	Capabilities  apt.Capabilities     `json:"capabilities"`
	Policy        aptRootPolicySummary `json:"policy"`
}

type aptRootPolicySummary struct {
	Sources            int    `json:"sources"`
	Preferences        int    `json:"preferences"`
	Conf               int    `json:"conf"`
	TrustedKeyrings    int    `json:"trusted_keyrings"`
	ReferencedKeyrings int    `json:"referenced_keyrings"`
	KeyFingerprints    int    `json:"key_fingerprints"`
	PhasedUpdatePolicy string `json:"phased_update_policy"`
}

func printAptRootHuman(ctx *Ctx, r aptRootInspectResult) {
	fmt.Fprintf(ctx.Stdout, "target:   %s %s (%s) %s\n", r.Target.DistroID, r.Target.VersionID, r.Target.Codename, r.Target.Arch)
	fmt.Fprintf(ctx.Stdout, "backend:  %s\n", r.Backend)
	if !r.Capabilities.Available {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Fail("unavailable:"), r.Capabilities.Reason)
		if r.Capabilities.Hint != "" {
			fmt.Fprintf(ctx.Stdout, "  hint: %s\n", r.Capabilities.Hint)
		}
		return
	}
	fmt.Fprintf(ctx.Stdout, "  apt %s / dpkg %s\n", r.Capabilities.APTVersion, r.Capabilities.DpkgVersion)
	if r.Capabilities.Image != "" {
		fmt.Fprintf(ctx.Stdout, "  image: %s (%s)\n", r.Capabilities.Image, r.Capabilities.ImageDigest)
	}
	fmt.Fprintf(ctx.Stdout, "policy:   %d source file(s), %d preference file(s), %d conf file(s)\n", r.Policy.Sources, r.Policy.Preferences, r.Policy.Conf)
	fmt.Fprintf(ctx.Stdout, "          %d trusted keyring(s), %d referenced keyring(s), %d fingerprint(s)\n", r.Policy.TrustedKeyrings, r.Policy.ReferencedKeyrings, r.Policy.KeyFingerprints)
	fmt.Fprintf(ctx.Stdout, "          phased updates: %s\n", r.Policy.PhasedUpdatePolicy)
}
