// Command debark is the CLI entry point. It builds the root command, runs
// it and exits with the code core/dferr documents — nothing else. The
// command tree, output rendering, config and progress all live in
// internal/cli; core/dferr.ClassOf(err) is the single place an error becomes
// an exit status, and internal/cli.Execute is the only caller of it.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/inferops/debark/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is Execute under a name main_test.go can call directly.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return cli.Execute(ctx, args, stdin, stdout, stderr)
}
