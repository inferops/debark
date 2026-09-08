package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/internal/cli/render"
)

func newStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Manage the content-addressed object store.",
		Long: "The store at ~/.local/share/debark/store (XDG; %LOCALAPPDATA% on\n" +
			"Windows) holds every fetched .deb by sha256, which is what makes\n" +
			"repeat builds incremental: nothing already held is fetched again.",
	}
	cmd.AddCommand(newStoreGCCmd(), newStoreLsCmd())
	return cmd
}

func storeDirFlag(cmd *cobra.Command) *string {
	var dir string
	cmd.Flags().StringVar(&dir, "store", "", "store location (default: config store_dir, else the XDG-correct default)")
	return &dir
}

func resolveStoreDir(ctx *Ctx, flag string) string {
	if flag != "" {
		return flag
	}
	if ctx.Cfg.StoreDir != "" {
		return ctx.Cfg.StoreDir
	}
	return store.DefaultRoot()
}

func newStoreGCCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "gc [BUNDLE...]",
		Short: "Remove objects unreferenced by any given bundle.",
		Long: "Remove store objects not referenced by any bundle manifest named on\n" +
			"the command line and not marked user-supplied. debark does not keep\n" +
			"a registry of every bundle ever built (rung 2 is a paid-edition\n" +
			"concern), so with no BUNDLE arguments this only protects\n" +
			"user-supplied objects — pass every bundle you still care about.",
		Args: cobra.ArbitraryArgs,
	}
	dir := storeDirFlag(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed without removing anything")
	cmd.RunE = wrapRun(func(ctx *Ctx, args []string) error {
		st, err := store.Open(resolveStoreDir(ctx, *dir))
		if err != nil {
			return err
		}
		idx, err := st.Index()
		if err != nil {
			return err
		}

		protected := map[string]bool{}
		for _, e := range idx.Entries {
			if e.UserSupplied {
				protected[e.Digest] = true
			}
		}
		for _, bp := range args {
			b, err := bundle.Open(ctx.Context, bp)
			if err != nil {
				return err
			}
			if b.Manifest != nil {
				for _, f := range b.Manifest.Files {
					protected[f.SHA256] = true
				}
			}
			_ = b.Close()
		}

		if dryRun {
			var removed int
			var bytes int64
			for _, e := range idx.Entries {
				if !protected[e.Digest] {
					removed++
					bytes += e.Size
				}
			}
			return printGCStats(ctx, store.GCStats{Removed: removed, Kept: len(idx.Entries) - removed, BytesFreed: bytes}, true)
		}

		stats, err := st.GC(ctx.Context, func(digest string) bool { return protected[digest] })
		if err != nil {
			return err
		}
		return printGCStats(ctx, stats, false)
	})
	return cmd
}

func printGCStats(ctx *Ctx, stats store.GCStats, dryRun bool) error {
	if ctx.JSON() {
		b, err := json.MarshalIndent(stats, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
		return nil
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	fmt.Fprintf(ctx.Stdout, "%s %s, freeing %s (kept %s)\n",
		verb, render.Plural(stats.Removed, "object"), render.Bytes(stats.BytesFreed), render.Plural(stats.Kept, "object"))
	return nil
}

func newStoreLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List store contents.",
		Args:  cobra.NoArgs,
	}
	dir := storeDirFlag(cmd)
	cmd.RunE = wrapRun(func(ctx *Ctx, args []string) error {
		st, err := store.Open(resolveStoreDir(ctx, *dir))
		if err != nil {
			return err
		}
		idx, err := st.Index()
		if err != nil {
			return err
		}
		if ctx.JSON() {
			b, err := json.MarshalIndent(idx, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(ctx.Stdout, string(b))
			return nil
		}
		t := render.NewTable("NAME", "VERSION", "ARCH", "SIZE", "USER", "DIGEST")
		var total int64
		for _, e := range idx.Entries {
			user := ""
			if e.UserSupplied {
				user = "yes"
			}
			digest := e.Digest
			if len(digest) > 12 {
				digest = digest[:12]
			}
			t.AddRow(e.Name, e.Version, e.Arch, render.Bytes(e.Size), user, digest)
			total += e.Size
		}
		t.Render(ctx.Stdout)
		fmt.Fprintf(ctx.Stdout, "%s, %s\n", render.Plural(len(idx.Entries), "object"), render.Bytes(total))
		return nil
	})
	return cmd
}
