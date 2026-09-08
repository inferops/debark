package cli

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/internal/cli/config"
	"github.com/inferops/debark/internal/cli/progress"
	"github.com/inferops/debark/internal/cli/render"
)

// Ctx is what every command's RunE builds first (via newCtx(cmd)): the
// resolved global flags, the loaded config and the colour/interactivity
// policy, so business logic never re-derives it.
type Ctx struct {
	context.Context

	// Stdin is where a guided flow reads answers from. It is only ever read
	// by a command the operator explicitly asked to be interactive; nothing
	// else in the tree touches it.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Cfg            *config.Config
	// cmd is the cobra.Command actually invoked, for the rare command (only
	// gen-manpages today) that needs the whole tree via cmd.Root() rather
	// than just its own flags and streams.
	cmd *cobra.Command

	// Style renders ANSI colour for text written to Stdout.
	Style render.Style
	// Progress is true when a live-updating progress line should be drawn on
	// Stderr: Stderr is a real terminal, and neither --json nor --json-events
	// was given ("Both imply non-interactive, no colour, no progress
	// bars").
	Progress bool

	jsonMode    bool
	jsonEvents  string
	interactive bool
}

// JSON reports whether --json was given.
func (c *Ctx) JSON() bool { return c.jsonMode }

// Interactive reports whether this run may ask the operator a question:
// stdin is a real terminal, and neither --json nor --json-events was given.
//
// It is the ONLY gate a prompt may consult, and every prompt must consult it.
// A command still needs an explicit --interactive as well — see
// missingBaseError for why a missing argument must never start reading stdin
// on its own — so this answers "may I ask?", never "should I?".
func (c *Ctx) Interactive() bool { return c.interactive }

// selfBinaryFlagUsage is the one wording of --self-binary, shared by every
// command that can reach the container backend so the four cannot drift.
const selfBinaryFlagUsage = "static linux/<arch> debark for the container backend to mount " +
	"(default: bin/debark-linux-<arch> beside this binary, else this binary; " +
	"also DEBARK_SELF_BINARY or the self_binary config key)"

// selfBinary resolves the debark binary the container backend should mount
// and re-enter, from --self-binary (flag), then DEBARK_SELF_BINARY and the
// self_binary config key (both already merged into Cfg by config.Load, in
// that precedence).
//
// Empty is a real answer, not a failure: core/apt's containerSelfPath then
// looks for bin/debark-linux-<arch> beside the running executable and
// finally falls back to the running executable itself. Deciding it there
// rather than here is what lets the search be architecture-aware, since only
// the backend knows which architecture the container will run.
//
// This replaced an unconditional os.Executable(), which was correct on Linux
// and unusable everywhere else: a debark.exe or a Mach-O binary is not a
// Linux ELF binary, the container backend refused it, and there was no flag,
// environment variable or config key with which to name a different file --
// the error's own advice was to "set ContainerOptions.SelfPath", a Go struct
// field. `build --backend container` was therefore impossible from Windows
// and macOS through the CLI at all.
func (c *Ctx) selfBinary(flag string) string {
	if flag != "" {
		return flag
	}
	if c.Cfg != nil {
		return c.Cfg.SelfBinary
	}
	return ""
}

func fileOf(w io.Writer) *os.File {
	if f, ok := w.(*os.File); ok {
		return f
	}
	return nil
}

// fileOfReader is fileOf for the input side. Same type assertion, same nil
// for a buffer, so render.IsTerminal answers false for a test's strings.Reader
// exactly as it does for a pipe.
func fileOfReader(r io.Reader) *os.File {
	if f, ok := r.(*os.File); ok {
		return f
	}
	return nil
}

// newCtx builds the per-run Ctx from cmd's own resolved flags (which, via
// cobra's persistent-flag inheritance, include --json/--config/etc. no
// matter how deep cmd is in the tree) and streams. Config is loaded through
// cmd.Flags() itself, so the flag > env > file > defaults precedence
// (internal/cli/config's package doc) covers the command's own flags too,
// not just the two config-selection ones.
//
// A bad config file is a usage error, matching every other malformed-input
// case; newCtx returning one is why every RunE below starts with
// `ctx, err := newCtx(cmd); if err != nil { return err }`.
func newCtx(cmd *cobra.Command) (*Ctx, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	jsonEvents, _ := cmd.Flags().GetString("json-events")
	noColor, _ := cmd.Flags().GetBool("no-color")

	cfg, err := config.Load(cmd.Flags())
	if err != nil {
		return nil, err
	}

	stdin := cmd.InOrStdin()
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	// Two related but distinct questions, deliberately not conflated.
	//
	// automation is the contract: --json and --json-events both mean a
	// program is reading this, so no colour and no progress bars.
	//
	// interactive additionally requires a human at the other end of STDIN,
	// which is what a prompt actually needs. The stdin check is kept out of
	// automation on purpose: `debark build ... < packages.txt` on a real
	// terminal is an ordinary thing to type, and folding stdin into the
	// colour and progress decision would silently strip both from it. What a
	// redirected stdin means is "there is nobody here to answer a question",
	// and that is all it is allowed to mean.
	//
	// render.IsTerminal, not a stat: it already handles the Windows/MSYS pty
	// case a stat-based check gets wrong, and one detector is the point.
	automation := jsonMode || jsonEvents != ""
	interactive := !automation && render.IsTerminal(fileOfReader(stdin))

	style := render.NewStyle(fileOf(stdout), automation || noColor)
	prog := !automation && render.IsTerminal(fileOf(stderr)) && !render.DumbTerminal()

	return &Ctx{
		Context:     cmd.Context(),
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		Cfg:         cfg,
		cmd:         cmd,
		Style:       style,
		Progress:    prog,
		jsonMode:    jsonMode,
		jsonEvents:  jsonEvents,
		interactive: interactive,
	}, nil
}

// wrapRun adapts a (ctx *Ctx, args []string) error function — every
// command's actual business logic, unchanged by which flag/command
// framework calls it — to cobra's RunE signature, building Ctx first. Every
// newXCmd below uses this so RunE is one line and the logic underneath it
// reads exactly as it did before the cobra migration.
func wrapRun(fn func(ctx *Ctx, args []string) error) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, err := newCtx(cmd)
		if err != nil {
			return err
		}
		return fn(ctx, args)
	}
}

// openEventsFile opens the --json-events target for writing. It is a package
// variable purely so a test can substitute a stream whose Close fails: the
// failure the callers' deferred close handling exists for — a full disk, or
// removable media pulled out mid-run — surfaces at Close, and there is no way
// to provoke a real (*os.File).Close failure portably from a test on both
// Linux and Windows.
var openEventsFile = func(path string) (io.WriteCloser, error) { return os.Create(path) }

// NewEvidenceSink builds the evidence sink a command should pass to the
// engine/install/etc.: an NDJSON stream when --json-events was given, a
// live progress line on Stderr when interactive, or evidence.Discard{} when
// neither applies. The returned close function must run after the command's
// work is done and before the process exits, so a file sink's contents are
// flushed and a progress line is finished with a newline.
func (c *Ctx) NewEvidenceSink() (evidence.Sink, func() error, error) {
	var sinks evidence.MultiSink
	var fileToClose io.Closer

	if target := c.jsonEvents; target != "" {
		if target == "-" {
			sinks = append(sinks, evidence.NewNDJSONSink(c.Stdout))
		} else {
			f, err := openEventsFile(target)
			if err != nil {
				return nil, nil, dferr.Wrap(dferr.Environment, err, "create %s", target)
			}
			fileToClose = f
			sinks = append(sinks, evidence.NewNDJSONSink(f))
		}
	}
	if c.Progress {
		sinks = append(sinks, progress.New(c.Stderr, true, c.Style))
	}

	if len(sinks) == 0 {
		return evidence.Discard{}, func() error { return nil }, nil
	}
	closeFn := func() error {
		err := sinks.Close()
		if fileToClose != nil {
			if cerr := fileToClose.Close(); err == nil {
				err = cerr
			}
		}
		return err
	}
	return sinks, closeFn, nil
}
