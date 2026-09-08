package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/internal/cli/render"
)

// debark fetch — hidden debugging command. Downloads one or more vendor
// URLs straight into the content-addressed store via core/fetch.Fetcher,
// without resolving, indexing or bundling anything. It exists to debug a
// vendor URL in isolation (redirects, TLS, a wrong --digest) without paying
// for a full build.
func newFetchCmd() *cobra.Command {
	var digests map[string]string

	cmd := &cobra.Command{
		Use:    "fetch URL...",
		Hidden: true,
		Short:  "Download vendor URLs into the store.",
		Long:   "Hidden debugging command: download one or more vendor .deb URLs into the content-addressed store directly, without resolving or bundling anything.",
		Args:   cobra.MinimumNArgs(1),
	}
	dir := storeDirFlag(cmd)
	registerDigestFlag(cmd.Flags(), &digests)
	cmd.RunE = wrapRun(func(ctx *Ctx, args []string) (err error) {
		st, err := store.Open(resolveStoreDir(ctx, *dir))
		if err != nil {
			return err
		}
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

		f := fetch.New(fetch.Options{Store: st, Events: events})
		if f == nil {
			return dferr.New(dferr.Environment, "fetch: fetcher unavailable")
		}

		inputs := make([]buildjob.URLInput, len(args))
		for i, u := range args {
			inputs[i] = buildjob.URLInput{URL: u, SHA256: digests[u]}
		}
		results := f.FetchAll(ctx.Context, inputs)

		return renderFetchResults(ctx, results)
	})
	return cmd
}

func renderFetchResults(ctx *Ctx, results []fetch.Result) error {
	if ctx.JSON() {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
	} else {
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(ctx.Stdout, "%s %s: %v\n", ctx.Style.Fail("failed "), r.Input.URL, r.Err)
				continue
			}
			fmt.Fprintf(ctx.Stdout, "%s %s (%s, %s)\n", ctx.Style.OK("fetched"), r.Fetched.Filename, render.Bytes(r.Fetched.Size), r.Fetched.Verification)
		}
	}
	var failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.Input.URL)
		}
	}
	if len(failed) > 0 {
		return dferr.New(dferr.Incomplete, "fetch: %d of %d URL(s) failed: %s",
			len(failed), len(results), nameSome(failed))
	}
	return nil
}
