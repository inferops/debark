package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/verify"
	"github.com/inferops/debark/internal/cli/render"
)

func newVerifyCmd() *cobra.Command {
	var keys, keyrings []string
	var gpgKeyring string
	var allowUnsigned bool

	cmd := &cobra.Command{
		Use:   "verify BUNDLE",
		Short: "Check a bundle before apt ever sees it.",
		Long: "Verify checks, in order and stopping at the first hard failure: the\n" +
			"manifest parses; the signature file matches the manifest; at least one\n" +
			"signature verifies against a trusted key (unless --allow-unsigned);\n" +
			"every file in the manifest exists with the right size and digest; no\n" +
			"unexpected file is present; the repository metadata digests match; the\n" +
			"lock and snapshot digests match. It never executes apt and never\n" +
			"trusts a key found inside the bundle it is checking.",
		Args: cobra.ExactArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			ks := sign.KeySource{
				Files:      firstNonEmptySlice(keys, ctx.Cfg.VerifyKeys),
				Dirs:       firstNonEmptySlice(keyrings, ctx.Cfg.VerifyKeyringDirs),
				GPGKeyring: gpgKeyring,
			}
			report, err := verify.New().Verify(ctx.Context, args[0], verify.Options{
				Keys:          ks,
				AllowUnsigned: allowUnsigned,
			})
			if err != nil {
				return err
			}
			return renderVerifyReport(ctx, report)
		}),
	}

	cmd.Flags().StringArrayVar(&keys, "key", nil, "operator public key file; repeatable")
	cmd.Flags().StringArrayVar(&keyrings, "keyring", nil, "directory of trusted public keys; repeatable")
	// --gpg-keyring is the only way to narrow the trust set for a gpg-signed
	// bundle. Without it gpg falls back to whatever this machine's default
	// keyring happens to hold, which is the ambient trust ADR-008 layer 2
	// exists to replace; --keyring above is a different thing entirely (a
	// directory of debark .pub files), so the two names are kept distinct
	// rather than overloaded.
	cmd.Flags().StringVar(&gpgKeyring, "gpg-keyring", "", "gpg keyring FILE holding the expected release key(s), for gpg-signed bundles; without it gpg uses this machine's default keyring")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "accept a bundle with no valid signature (must be an explicit choice)")
	return cmd
}

func renderVerifyReport(ctx *Ctx, report *verify.Report) error {
	if ctx.JSON() {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
	} else {
		printVerifyHuman(ctx, report)
	}
	if !report.OK {
		return verifyFailureError(report)
	}
	return nil
}

// verifyFailureError is the one line a failed verification writes to stderr.
//
// It names the first problem, because "failed verification" is the one
// sentence every failure shares and so tells the reader nothing: a missing
// signature, a digest that does not match and an unexpected file in the
// bundle are three entirely different situations, and only the first
// Problem's own text separates them. The full list is on stdout, in the
// human report and in the --json document alike.
func verifyFailureError(r *verify.Report) error {
	msg := fmt.Sprintf("verify: %s failed verification", r.BundlePath)
	if n := len(r.Problems); n > 0 {
		first := r.Problems[0].Message
		if p := r.Problems[0].Path; p != "" {
			first = p + ": " + first
		}
		msg += fmt.Sprintf(": %s: %s", render.Plural(n, "problem"), first)
		if n > 1 {
			msg += fmt.Sprintf(" (and %d more)", n-1)
		}
	}
	err := dferr.New(dferr.Verification, "%s", msg)
	// The trust problems are the ones with an operator action attached, and
	// they are also the ones the sentence above reads worst for: "no
	// signature verifies against a trusted key" is indistinguishable, to
	// someone who has just signed the bundle themselves, from "this bundle
	// was tampered with". It usually means the public key is not on this
	// machine (ADR-008: it must arrive independently of the media).
	for _, p := range r.Problems {
		switch p.Kind {
		case verify.ProblemSignatureMissing, verify.ProblemSignatureUntrusted, verify.ProblemSameMediaKey:
			return err.WithHint("the operator public key must reach this machine independently of the bundle's media: pass it with --key, or list it under verify_keys in the config file")
		}
	}
	return err
}

// printVerifyHuman answers the fourth of the design's four questions ("can
// this artifact be trusted") directly: status first, then exactly what was
// and was not established.
func printVerifyHuman(ctx *Ctx, r *verify.Report) {
	status := ctx.Style.OK("OK")
	if !r.OK {
		status = ctx.Style.Fail("FAILED")
	}
	fmt.Fprintf(ctx.Stdout, "%s  %s\n", status, r.BundlePath)

	if r.Target.DistroID != "" {
		fmt.Fprintf(ctx.Stdout, "  target:  %s %s (%s) %s\n", r.Target.DistroID, r.Target.VersionID, r.Target.Codename, r.Target.Arch)
	}
	switch {
	case r.Signed:
		for _, s := range r.Signatures {
			mark := ctx.Style.OK("valid")
			if !s.Valid {
				mark = ctx.Style.Fail("invalid")
			} else if !s.Trusted {
				mark = ctx.Style.Warn("untrusted")
			}
			fmt.Fprintf(ctx.Stdout, "  signed:  %s by %s (%s) [%s]\n", mark, s.KeyID, s.SignerKind, s.Algorithm)
		}
	case r.OK:
		fmt.Fprintf(ctx.Stdout, "  signed:  %s (--allow-unsigned)\n", ctx.Style.Warn("no"))
	default:
		fmt.Fprintln(ctx.Stdout, "  signed:  no")
	}
	fmt.Fprintf(ctx.Stdout, "  checked: %s (%s)\n", render.Plural(r.FilesChecked, "file"), render.Bytes(r.BytesChecked))

	for _, p := range r.Problems {
		line := p.Message
		if p.Path != "" {
			line = p.Path + ": " + line
		}
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Fail("problem:"), line)
		if p.Expected != "" || p.Got != "" {
			fmt.Fprintf(ctx.Stdout, "      expected %s, got %s\n", p.Expected, p.Got)
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Warn("warning:"), w)
	}
}

func firstNonEmptySlice(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}
