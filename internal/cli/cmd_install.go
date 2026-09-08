package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/install"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/verify"
)

func newInstallCmd() *cobra.Command {
	var status, dryRun, upgrade, all, keepSource, fast, dpkg, yes, allowUnsigned bool
	var keys, keyrings []string
	var gpgKeyring string

	cmd := &cobra.Command{
		Use:   "install BUNDLE",
		Short: "Verify and install a bundle.",
		Long: "Verify the bundle (refusing on failure, exit 4), check preconditions\n" +
			"(architecture must match; release mismatch warns), then install exact\n" +
			"versions from the lock through a private, temporary apt view. The\n" +
			"system's own apt configuration is never touched.",
		Args: cobra.ExactArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) (err error) {
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

			runner := install.New(install.Deps{
				Verifier: verify.New(),
				Events:   events,
			})
			opts := install.Options{
				Verify: verify.Options{
					Keys: sign.KeySource{
						Files:      firstNonEmptySlice(keys, ctx.Cfg.VerifyKeys),
						Dirs:       firstNonEmptySlice(keyrings, ctx.Cfg.VerifyKeyringDirs),
						GPGKeyring: gpgKeyring,
					},
					AllowUnsigned: allowUnsigned,
				},
				Upgrade:    upgrade,
				All:        all,
				KeepSource: keepSource,
				Fast:       fast,
				Dpkg:       dpkg,
				Yes:        yes,
			}

			var report *install.Report
			if status || dryRun {
				report, err = runner.Plan(ctx.Context, args[0], opts)
			} else {
				report, err = runner.Apply(ctx.Context, args[0], opts)
			}
			if err != nil {
				return err
			}
			return renderInstallReport(ctx, report)
		}),
	}

	cmd.Flags().BoolVar(&status, "status", false, "report to-install/to-upgrade against this machine and exit; changes nothing")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "simulate the install and exit; changes nothing")
	cmd.Flags().BoolVar(&upgrade, "upgrade", false, "also install the lock's upgrade set, at the exact versions it recorded")
	cmd.Flags().BoolVar(&all, "all", false, "install every package in the bundle, not just the lock's install set")
	cmd.Flags().BoolVar(&keepSource, "keep-source", false, "leave a permanent Trusted: yes apt source for the bundle's repo/: every later apt install/upgrade installs from it as root, unverified")
	cmd.Flags().BoolVar(&fast, "fast", false, "add --force-unsafe-io and noninteractive debconf")
	cmd.Flags().BoolVar(&dpkg, "dpkg", false, "bypass apt: dpkg --unpack everything, then dpkg --configure -a")
	cmd.Flags().BoolVar(&yes, "yes", false, "assume yes to apt's prompts (required for a non-interactive install)")
	cmd.Flags().StringArrayVar(&keys, "key", nil, "operator public key file for the mandatory pre-install verify; repeatable")
	cmd.Flags().StringArrayVar(&keyrings, "keyring", nil, "directory of trusted public keys for the mandatory pre-install verify; repeatable")
	// See the note on the same flag in cmd_verify.go: this is the only way to
	// narrow the trust set for a gpg-signed bundle, and it is a keyring FILE,
	// not the directory --keyring takes.
	cmd.Flags().StringVar(&gpgKeyring, "gpg-keyring", "", "gpg keyring FILE holding the expected release key(s), for gpg-signed bundles; without it gpg uses this machine's default keyring")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "accept a bundle with no valid signature (must be an explicit choice)")
	cmd.MarkFlagsMutuallyExclusive("status", "dry-run")
	return cmd
}

func renderInstallReport(ctx *Ctx, r *install.Report) error {
	if ctx.JSON() {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
	} else {
		printInstallHuman(ctx, r)
	}
	if !r.OK {
		return installFailureError(r)
	}
	return nil
}

// installFailureError is the one line a failed install writes to stderr. It
// names the first problem the report recorded, for the same reason
// verifyFailureError does: "did not reach the requested state" is true of
// every failure and therefore describes none of them.
func installFailureError(r *install.Report) error {
	msg := fmt.Sprintf("install: %s did not reach the requested state", r.BundlePath)
	switch {
	case len(r.Problems) > 0:
		msg += ": " + nameSome(r.Problems)
	case r.Verify != nil && !r.Verify.OK:
		msg += ": the bundle did not verify"
	case r.TargetExpected.Arch != "" && r.TargetActual.Arch != "" && r.TargetExpected.Arch != r.TargetActual.Arch:
		msg += fmt.Sprintf(": the bundle is for %s, this machine is %s", r.TargetExpected.Arch, r.TargetActual.Arch)
	}
	return dferr.New(installExitClass(r), "%s", msg)
}

// installExitClass best-effort classifies a non-OK install.Report that came
// back without a Go error. In practice install.Runner is expected to return
// a *dferr.Error directly whenever something is actually wrong (rule 5:
// "errors carry a class"), so this is the fallback path, not the common one.
func installExitClass(r *install.Report) dferr.Class {
	switch {
	case r.Verify != nil && !r.Verify.OK:
		return dferr.Verification
	case r.TargetExpected.Arch != "" && r.TargetActual.Arch != "" && r.TargetExpected.Arch != r.TargetActual.Arch:
		return dferr.TargetMismatch
	default:
		return dferr.Resolution
	}
}

func printInstallHuman(ctx *Ctx, r *install.Report) {
	verb := "install plan for"
	if r.Applied {
		verb = "installed"
	}
	status := ctx.Style.OK("OK")
	if !r.OK {
		status = ctx.Style.Fail("FAILED")
	}
	fmt.Fprintf(ctx.Stdout, "%s  %s %s\n", status, verb, r.BundlePath)

	if r.Verify != nil {
		vstatus := ctx.Style.OK("OK")
		if !r.Verify.OK {
			vstatus = ctx.Style.Fail("FAILED")
		}
		fmt.Fprintf(ctx.Stdout, "  verify:    %s\n", vstatus)
	}
	fmt.Fprintf(ctx.Stdout, "  target:    %s %s (%s) %s\n",
		r.TargetActual.DistroID, r.TargetActual.VersionID, r.TargetActual.Codename, r.TargetActual.Arch)
	fmt.Fprintf(ctx.Stdout, "  to install: %d\n", len(r.ToInstall))
	fmt.Fprintf(ctx.Stdout, "  to upgrade: %d\n", len(r.ToUpgrade))
	if len(r.ToRemove) > 0 {
		fmt.Fprintf(ctx.Stdout, "  to remove:  %d\n", len(r.ToRemove))
	}
	fmt.Fprintf(ctx.Stdout, "  unchanged:  %d\n", r.AlreadyCurrent)

	// Above the warnings, not among them. The warning sentences say what was
	// found; this line says what the bundle was built against at all, which
	// is the fact that changes how everything else on the screen should be
	// read (ADR-014).
	if d := r.BaseDivergence; d != nil {
		fmt.Fprintf(ctx.Stdout, "  base:       %s (assumed %d installed, %d not present here)\n",
			d.BaseID, d.Assumed, len(d.Missing))
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Warn("warning:"), w)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Fail("problem:"), p)
	}
}
