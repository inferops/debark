package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build identity.",
		Long:  "Print debark's version, commit, build digest, edition and Go version.",
		Args:  cobra.NoArgs,
		RunE:  wrapRun(runVersion),
	}
}

func runVersion(ctx *Ctx, args []string) error {
	info := version.Get()
	if ctx.JSON() {
		b, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
		return nil
	}

	fmt.Fprintln(ctx.Stdout, ctx.Style.Bold(version.String()))
	if info.Commit != "" {
		fmt.Fprintf(ctx.Stdout, "commit:       %s\n", info.Commit)
	}
	if info.Date != "" {
		fmt.Fprintf(ctx.Stdout, "built:        %s\n", info.Date)
	}
	fmt.Fprintf(ctx.Stdout, "go:           %s\n", info.GoVersion)
	fmt.Fprintf(ctx.Stdout, "platform:     %s\n", info.Platform)
	if info.BuildDigest != "" {
		fmt.Fprintf(ctx.Stdout, "build digest: sha256:%s\n", info.BuildDigest)
	}
	return nil
}
