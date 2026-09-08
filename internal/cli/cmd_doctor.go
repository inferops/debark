package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/doctor"
	"github.com/inferops/debark/core/snapshot"
)

func newDoctorCmd() *cobra.Command {
	var snapshotFile string
	var noScanScripts bool

	cmd := &cobra.Command{
		Use:   "doctor [BUNDLE]",
		Short: "Heuristics: network postinst, snap shims, DKMS, redistribution.",
		Long: "Report what is likely to go wrong on the far side of the gap:\n" +
			"maintainer scripts that look like they want the network, snap-shim\n" +
			"packages, DKMS packages without matching headers, and redistribution\n" +
			"terms (multiverse/restricted/non-free). Every finding is a heuristic\n" +
			"and says so — doctor reports \"no obvious network action found\", never\n" +
			"\"proven safe\" — and it never fails a build by itself; policy does.",
		Args: cobra.MaximumNArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			haveBundle := len(args) == 1
			haveSnapshot := snapshotFile != ""
			if haveBundle == haveSnapshot {
				return dferr.Usagef("doctor: give exactly one of BUNDLE or --snapshot FILE")
			}

			in := doctor.Input{ScanScripts: !noScanScripts}
			if haveBundle {
				b, err := bundle.Open(ctx.Context, args[0])
				if err != nil {
					return err
				}
				defer func() { _ = b.Close() }()
				in.BundleDir = b.Dir
				in.Lock = b.Lock
			} else {
				archive, err := snapshot.Open(ctx.Context, snapshotFile)
				if err != nil {
					return err
				}
				defer func() { _ = archive.Close() }()
				in.Snapshot = archive.Snapshot
			}

			report, err := doctor.Run(ctx.Context, in)
			if err != nil {
				return err
			}

			if ctx.JSON() {
				b, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}
			printDoctorHuman(ctx, report)
			return nil
		}),
	}

	cmd.Flags().StringVar(&snapshotFile, "snapshot", "", "examine a snapshot instead of a bundle")
	cmd.Flags().BoolVar(&noScanScripts, "no-scan-scripts", false, "skip extracting and scanning maintainer scripts (the slow part)")
	return cmd
}

func printDoctorHuman(ctx *Ctx, r *doctor.Report) {
	fmt.Fprintf(ctx.Stdout, "scanned %d package(s), %d finding(s)\n", r.Scanned, len(r.Findings))
	for _, f := range r.Findings {
		sev := ctx.Style.Faint(string(f.Severity))
		if f.Severity == doctor.SeverityWarn {
			sev = ctx.Style.Warn(string(f.Severity))
		}
		pkg := f.Package
		if pkg != "" && f.Version != "" {
			pkg = pkg + " " + f.Version
		}
		fmt.Fprintf(ctx.Stdout, "  [%s] %-24s %s\n", sev, f.Check, f.Message)
		if pkg != "" {
			fmt.Fprintf(ctx.Stdout, "        package: %s\n", pkg)
		}
		if f.Evidence != "" {
			fmt.Fprintf(ctx.Stdout, "        evidence: %s\n", f.Evidence)
		}
	}
	if len(r.Findings) == 0 {
		fmt.Fprintln(ctx.Stdout, "  no obvious issues found (heuristic only — not a guarantee)")
	}
}
