package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/internal/cli/render"
)

// The assumption caveat, in one sentence, printed wherever a synthesized
// snapshot is produced or listed. One string, so the docs, the human output
// and the snapshot's own recorded warning cannot drift into three slightly
// different promises.
const baseAssumptionCaveat = "a baseline OS is an assumption about the installed package set; " +
	"use a captured snapshot for the actual machine's packages and settings"

func newSnapshotFromBaseCmd() *cobra.Command {
	var out, arch, backend, image, selfBinary string
	var interactive bool

	cmd := &cobra.Command{
		Use:   "from-base [BASE]",
		Short: "Save a baseline OS as a reusable snapshot file.",
		Long: "Choose a baseline OS and save its assumed package state for later builds.\n" +
			"No access to the offline machine or captured snapshot is required.\n" +
			"BASE is an id such as ubuntu:24.04/minimal, or a base definition file.\n" +
			"Use debark snapshot list-bases to see the built-in choices.\n" +
			"\n" +
			"Pass --interactive to choose the release, variant, architecture and output\n" +
			"through prompts. This requires a terminal and cannot be combined with\n" +
			"--json or --json-events. Otherwise, supply BASE on the command line.\n" +
			"\n" +
			"This command needs internet access to resolve the baseline's packages.\n" +
			"To build a bundle directly without saving a baseline file first, use\n" +
			"debark build --base. A baseline assumes a stock installation; use a\n" +
			"captured snapshot when you need the actual machine's packages and settings.",
		Example: "  debark snapshot from-base --interactive\n" +
			"  debark snapshot from-base ubuntu:24.04/minimal --arch amd64 --out base.tar.zst",
		Args: cobra.MaximumNArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) (err error) {
			effectiveBackend, err := checkBackendFlag("snapshot from-base", backend, ctx.Cfg.Backend)
			if err != nil {
				return err
			}

			baseRef := ""
			if len(args) == 1 {
				baseRef = args[0]
			}
			if baseRef == "" && !interactive {
				return missingBaseError(ctx)
			}
			if interactive {
				answers, ierr := runFromBaseInteractive(ctx, fromBaseAnswers{
					BaseRef: baseRef,
					Arch:    arch,
					Out:     out,
				})
				if ierr != nil {
					return ierr
				}
				baseRef, arch, out = answers.BaseRef, answers.Arch, answers.Out
			}

			if arch == "" {
				arch = base.HostArch()
			}
			resolved, err := base.Resolve(baseRef, arch)
			if err != nil {
				return err
			}
			if out == "" {
				out = defaultBaseSnapshotName(resolved.Definition)
			}

			events, closeEvents, err := ctx.NewEvidenceSink()
			if err != nil {
				return err
			}
			defer func() {
				if cerr := closeEvents(); cerr != nil && err == nil {
					err = dferr.Wrap(dferr.Environment, cerr, "close event stream")
				}
			}()

			result, err := synthesizeBase(ctx, resolved, baseRef, out, effectiveBackend, image, ctx.selfBinary(selfBinary), events)
			if err != nil {
				return err
			}

			if interactive {
				printEquivalentCommand(ctx, fromBaseCommandLine(baseRef, arch, out, backend, image))
			}
			return renderFromBaseResult(ctx, resolved, result)
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&out, "out", "", "output path for the snapshot archive (default: derived from the base id and architecture)")
	flags.StringVar(&arch, "arch", "", "dpkg architecture to describe (default: this machine's)")
	flags.StringVar(&backend, "backend", "", "auto, local or container (default: config backend, else auto)")
	flags.StringVar(&image, "image", "", "override the container image for the container backend")
	flags.StringVar(&selfBinary, "self-binary", "", selfBinaryFlagUsage)
	flags.BoolVar(&interactive, "interactive", false, "ask for anything not given on the command line (a terminal is required)")
	return cmd
}

// missingBaseError is what a bare `snapshot from-base` gets.
//
// On a terminal it mentions the guided flow, because a bare invocation there
// is a person who does not yet know the flags. It never *starts* the guided
// flow: a command that silently begins reading stdin when an argument is
// missing is how a tool hangs in CI when a variable expands empty, and this
// binary must never do that.
func missingBaseError(ctx *Ctx) error {
	err := dferr.New(dferr.Usage, "snapshot from-base: a base is required")
	if ctx.Interactive() {
		return err.WithHint("run `debark snapshot list-bases` to see them, or `debark snapshot from-base --interactive` to be asked")
	}
	return err.WithHint("run `debark snapshot list-bases` to see the builtin bases, or pass a path to a base definition file")
}

// defaultBaseSnapshotName derives an output name that says what the file is,
// so a directory holding several of them is readable without opening any.
func defaultBaseSnapshotName(d base.Definition) string {
	parts := []string{d.DistroID, d.VersionID}
	if d.Variant != "" {
		parts = append(parts, d.Variant)
	}
	parts = append(parts, d.Arch)
	return sanitiseFileName(strings.Join(parts, "-")) + ".snapshot.tar.zst"
}

// sanitiseFileName keeps a derived name to characters every filesystem this
// runs on accepts. A base id may legitimately contain ':' and '/' — both of
// which are a path separator or an alternate-data-stream marker somewhere —
// and an operator-supplied definition may use anything at all.
func sanitiseFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func newSnapshotListBasesCmd() *cobra.Command {
	var arch string

	cmd := &cobra.Command{
		Use:   "list-bases",
		Short: "List the base definitions compiled into this binary.",
		Long: "List the stock releases `snapshot from-base` and `build --base` know about\n" +
			"without being given a definition file. Any base not listed here can be\n" +
			"described in a file and passed by path.",
		Args: cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			if arch == "" {
				arch = base.HostArch()
			}
			defs, err := base.Builtin(arch)
			if err != nil {
				return err
			}

			if ctx.JSON() {
				listing, lerr := base.NewList(arch, defs)
				if lerr != nil {
					return lerr
				}
				b, merr := json.MarshalIndent(listing, "", "  ")
				if merr != nil {
					return merr
				}
				fmt.Fprintln(ctx.Stdout, string(b))
				return nil
			}

			t := render.NewTable("BASE", "SEEDS", "DESCRIPTION")
			for _, d := range defs {
				t.AddRow(d.ID, strings.Join(d.Seeds, " "), d.Description)
			}
			t.Render(ctx.Stdout)
			fmt.Fprintf(ctx.Stdout, "\narchitecture: %s (use --arch to see another)\n", arch)
			fmt.Fprintf(ctx.Stdout, "%s %s\n", ctx.Style.Warn("note:"), baseAssumptionCaveat)
			return nil
		}),
	}
	cmd.Flags().StringVar(&arch, "arch", "", "dpkg architecture to render the bases for (default: this machine's)")
	return cmd
}

// The debark.baselist/v1 shape this command emits lives in core/base
// (List, ListEntry, ListSchemaVersion, NewList), next to the Definition every
// field of it is a fact about, and is held to
// api/schema/baselist.v1.schema.json by api/schema/schema_test.go.

// synthesizeBase is the single call both entry points make, which is what
// makes `build --base X` and `snapshot from-base X` + `build --snapshot`
// produce the same bundle: there is one synthesizer and one artefact, and
// build does not know or care which of the two produced the snapshot it is
// handed.
func synthesizeBase(ctx *Ctx, resolved base.Resolved, baseRef, out, backend, image, selfBinary string, events evidence.Sink) (*base.Result, error) {
	workDir, err := os.MkdirTemp("", "debark-from-base-")
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "snapshot from-base: create work directory")
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	return base.Synthesize(ctx.Context, base.Options{
		Base:     resolved,
		BaseRef:  baseRef,
		Out:      out,
		WorkDir:  workDir,
		Backend:  backend,
		Image:    image,
		SelfPath: selfBinary,
		Events:   events,
	})
}

func renderFromBaseResult(ctx *Ctx, resolved base.Resolved, r *base.Result) error {
	if ctx.JSON() {
		b, err := json.MarshalIndent(map[string]any{
			"path":    r.Path,
			"digest":  r.Digest,
			"backend": string(r.Backend),
			"base":    resolved.Definition,
			"origin":  r.Snapshot.Origin,
			"target":  r.Snapshot.Target,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
		return nil
	}

	snap := r.Snapshot
	fmt.Fprintf(ctx.Stdout, "%s %s\n", ctx.Style.OK("snapshot synthesized:"), r.Path)
	fmt.Fprintf(ctx.Stdout, "  base:      %s (%s)\n", snap.Origin.BaseID, resolved.Source)
	fmt.Fprintf(ctx.Stdout, "  target:    %s %s (%s) %s\n",
		snap.Target.DistroID, snap.Target.VersionID, snap.Target.Codename, snap.Target.Arch)
	fmt.Fprintf(ctx.Stdout, "  assumed:   %s installed on the target\n",
		render.Plural(snap.InstalledCount, "package"))
	fmt.Fprintf(ctx.Stdout, "  resolver:  %s\n", r.Backend)
	fmt.Fprintf(ctx.Stdout, "  digest:    sha256:%s\n", r.Digest)
	fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Warn("assumption:"), baseAssumptionCaveat)
	for _, w := range r.Warnings {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Warn("warning:"), w.Message)
	}
	return nil
}

// fromBaseCommandLine renders the non-interactive equivalent of a guided run.
func fromBaseCommandLine(baseRef, arch, out, backend, image string) []string {
	argv := []string{"debark", "snapshot", "from-base", baseRef}
	if arch != "" {
		argv = append(argv, "--arch", arch)
	}
	if out != "" {
		argv = append(argv, "--out", out)
	}
	if backend != "" {
		argv = append(argv, "--backend", backend)
	}
	if image != "" {
		argv = append(argv, "--image", image)
	}
	return argv
}

// checkBackendFlag resolves and validates a --backend value the same way
// `build` does, so the three commands that take the flag reject the same
// values with the same message.
func checkBackendFlag(cmdName, flagValue, configValue string) (string, error) {
	effective := firstNonEmpty(flagValue, configValue, "auto")
	if !isOneOf(effective, "auto", "local", "container") {
		return "", dferr.Usagef("%s: --backend must be auto, local or container, got %q", cmdName, effective)
	}
	return effective, nil
}

// originLine is the one-line description of a snapshot's provenance, used by
// `snapshot inspect` so a reader is told which of the two very different
// documents they are holding before they read anything else.
//
// The synthesized case is coloured as a warning, not as neutral information.
// A captured snapshot proves a bundle is complete for one machine; a
// synthesized one only assumes it, and the whole reason origin is a required
// field is that the weaker claim must never be the quiet one.
func originLine(ctx *Ctx, snap *snapshot.Snapshot) string {
	if !snap.Synthesized() {
		return "captured from a real machine"
	}
	src := snap.Origin.Source
	if src == "" {
		src = "unrecorded source"
	}
	return fmt.Sprintf("%s base %s (%s), assuming %s installed",
		ctx.Style.Warn("synthesized"), snap.Origin.BaseID, src,
		render.Plural(len(snap.Origin.AssumedInstalled), "package"))
}
