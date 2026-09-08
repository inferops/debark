package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/sign"
)

// publicKeyPathFor mirrors sign.GenerateKey's own rule ("privPath with the
// .key suffix replaced by .pub") so the CLI reports the exact path
// GenerateKey wrote to, without importing anything unexported from sign.
func publicKeyPathFor(privPath string) string {
	if strings.HasSuffix(privPath, sign.PrivateKeyFileSuffix) {
		return strings.TrimSuffix(privPath, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix
	}
	return privPath + sign.PublicKeyFileSuffix
}

func newKeygenCmd() *cobra.Command {
	var out, comment string

	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate an ed25519 operator signing key.",
		Long: "Generate a new ed25519 operator key pair for signing bundles: an\n" +
			"unencrypted private key (0600) at --out, and its public key alongside\n" +
			"it with a .pub suffix. There is no passphrase; an operator who wants\n" +
			"one uses --sign gpg:<keyid> with gpg instead.",
		Args: cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			keyID, err := sign.GenerateKey(out, comment)
			if err != nil {
				return err
			}
			if ctx.JSON() {
				b, mErr := json.MarshalIndent(map[string]string{
					"key_id":      keyID,
					"private_key": out,
					"public_key":  publicKeyPathFor(out),
				}, "", "  ")
				if mErr != nil {
					return mErr
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}
			fmt.Fprintf(ctx.Stdout, "%s ed25519 key %s\n", ctx.Style.OK("generated"), keyID)
			fmt.Fprintf(ctx.Stdout, "  private: %s\n", out)
			fmt.Fprintf(ctx.Stdout, "  public:  %s\n", publicKeyPathFor(out))
			fmt.Fprintln(ctx.Stdout, "Keep the private key off the media you sign with it; the target must")
			fmt.Fprintln(ctx.Stdout, "receive the public key independently (config, pre-provisioning, or a")
			fmt.Fprintln(ctx.Stdout, "fingerprint compared by hand) — never from the same media as the bundle.")
			return nil
		}),
	}

	cmd.Flags().StringVar(&out, "out", "", "path to write the private key to (required); the public key is written alongside it")
	cmd.Flags().StringVar(&comment, "comment", "", `operator comment recorded in the key, e.g. "release engineering key, 2026"`)
	_ = cmd.MarkFlagRequired("out")
	return cmd
}
