package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/internal/cli/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage the debark config file.",
		Long: "Manage the debark config file: precedence is flag > env (DEBARK_*)\n" +
			"> config file > defaults. The file lives at an XDG-correct path\n" +
			"(\"debark config path\" prints it) and may define named profiles for\n" +
			"repeated targets, selected with --profile or DEBARK_PROFILE.",
	}
	cmd.AddCommand(newConfigInitCmd(), newConfigShowCmd(), newConfigPathCmd())
	return cmd
}

func newConfigInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a default config file.",
		Args:  cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			path, err := config.Init(ctx.Cfg.Path, force)
			if err != nil {
				return err
			}
			fmt.Fprintf(ctx.Stdout, "%s %s\n", ctx.Style.OK("wrote"), path)
			return nil
		}),
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective config.",
		Args:  cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			if ctx.JSON() {
				b, err := ctx.Cfg.JSON()
				if err != nil {
					return err
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}
			c := ctx.Cfg
			fmt.Fprintf(ctx.Stdout, "path:      %s", c.Path)
			if !c.Loaded {
				fmt.Fprint(ctx.Stdout, " (not found; showing defaults)")
			}
			fmt.Fprintln(ctx.Stdout)
			if c.Profile != "" {
				fmt.Fprintf(ctx.Stdout, "profile:   %s\n", c.Profile)
			}
			fmt.Fprintf(ctx.Stdout, "backend:   %s\n", nonEmpty(c.Backend, "(unset)"))
			fmt.Fprintf(ctx.Stdout, "image:     %s\n", nonEmpty(c.Image, "(unset)"))
			fmt.Fprintf(ctx.Stdout, "store_dir: %s\n", nonEmpty(c.StoreDir, "(default location)"))
			fmt.Fprintf(ctx.Stdout, "sign_key:  %s\n", nonEmpty(c.SignKey, "(unset)"))
			// Printed only when set. Unlike the four above it there is no
			// useful "(unset)" to show: the default is a per-architecture
			// search core/apt performs at the moment it needs a binary, and
			// this command does not know which target is coming.
			if c.SelfBinary != "" {
				fmt.Fprintf(ctx.Stdout, "self_binary: %s\n", c.SelfBinary)
			}
			if len(c.ProfileNames) > 0 {
				fmt.Fprintf(ctx.Stdout, "profiles:  %v\n", c.ProfileNames)
			}
			return nil
		}),
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path.",
		Args:  cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			fmt.Fprintln(ctx.Stdout, ctx.Cfg.Path)
			return nil
		}),
	}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
