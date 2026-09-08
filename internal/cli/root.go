// Package cli is the debark command-line shell: the command tree,
// built on spf13/cobra, wired to the frozen core/*
// interfaces, plus config loading (internal/cli/config, knadh/koanf),
// human/JSON rendering (internal/cli/render, charmbracelet/lipgloss),
// progress and the exit-code mapping.
//
// Nothing in core/ may import this package (contract-brief.md rule 7); the
// dependency runs one way, cli -> core.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/dferr"
)

// newRootCmd builds the full debark command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "debark",
		Short: "Download Debian and Ubuntu packages for offline installation.",
		Long: "Download packages and their dependencies on an online computer, then\n" +
			"copy the bundle to your offline Debian or Ubuntu machine and install it.\n" +
			"\n" +
			"Choose a baseline OS with build --base, or use a snapshot of the target\n" +
			"with build --snapshot. A baseline needs no captured snapshot; it assumes\n" +
			"a stock installed package set. Use a snapshot for the actual machine state.\n" +
			"\n" +
			"Run debark build --interactive for guided target and package choices.\n" +
			"Run debark snapshot list-bases to see the available baseline OS options.",
		SilenceErrors: true, // this package prints errors itself (dferr-classified, with hints)
		SilenceUsage:  true, // ...and prints usage only for usage-classed errors, see Execute
	}

	root.PersistentFlags().Bool("json", false, "print the documented JSON object for this command instead of human text")
	root.PersistentFlags().String("json-events", "", `stream NDJSON evidence events to PATH, or "-" for stdout`)
	root.PersistentFlags().Bool("no-color", false, "disable coloured output (also honours NO_COLOR and TERM=dumb)")
	root.PersistentFlags().String("config", "", "config file path (overrides XDG discovery)")
	root.PersistentFlags().String("profile", "", "named config profile to apply")

	root.AddCommand(
		newSnapshotCmd(),
		newBuildCmd(),
		newVerifyCmd(),
		newInstallCmd(),
		newInspectCmd(),
		newDoctorCmd(),
		newStoreCmd(),
		newConfigCmd(),
		newVersionCmd(),
		newKeygenCmd(),
		newResolveCmd(),
		newFetchCmd(),
		newAptRootCmd(),
		newGenManPagesCmd(),
	)
	return root
}

// Execute runs the real debark command tree end to end and returns the
// process exit code. It never calls os.Exit itself, so it is directly
// testable; cmd/debark/main.go's run() is a one-line wrapper around it.
//
// stdin is threaded through cobra's SetIn/InOrStdin, the same way stdout and
// stderr already were, so a test can drive the guided flows with a
// strings.Reader and — more importantly — can drive them with a PIPE, which
// is the case that actually breaks: a prompt must never fire when there is
// nobody to answer it.
//
// cobra's own error/usage printing is silenced (SilenceErrors/SilenceUsage
// on the root command) so this function is the single place an error
// becomes both process output and an exit code, matching rule 5
// (contract-brief.md: "errors carry a class... the CLI maps them").
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "debark: internal error: %v\n", r)
			code = int(dferr.Environment)
		}
	}()

	root := newRootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return int(dferr.Success)
	}

	// Every failure says why, on stderr, always — including the commands
	// that also print a full report of their own on stdout (build, verify,
	// install, fetch). Those used to return the error wrapped as "already
	// reported", which suppressed this line, and the result was a command
	// that exited 3 or 4 with a well-formed document on stdout and ZERO
	// BYTES on stderr. `debark build ... > result.json` then failed in
	// silence at a terminal, and any caller that reads stdout as data and
	// stderr as the diagnosis — a CI log, a shell pipeline, a GUI driving
	// the CLI — was left with an exit code and nothing else. The report on
	// stdout is the detail; this line is the diagnosis, and one is not a
	// substitute for the other.
	printErr(stderr, err)
	if dferr.ClassOf(err) == dferr.Usage && cmd != nil {
		fmt.Fprintln(stderr)
		fmt.Fprint(stderr, cmd.UsageString())
	}
	return dferr.ExitCode(err)
}

func printErr(w io.Writer, err error) {
	fmt.Fprintf(w, "debark: %s\n", err.Error())
	if hint := dferr.HintOf(err); hint != "" {
		fmt.Fprintln(w, hint)
	}
}
