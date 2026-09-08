package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/engine"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/internal/cli/render"
)

func newBuildCmd() *cobra.Command {
	var (
		snapshotFlag, baseFlag, archFlag                                  string
		lists, localDirs                                                  []string
		out, tar, backend, image, sign, approvedKeys, policy, embedBinary string
		selfBinary                                                        string
		update, noPrune, upgrades, noRecommends, recommendsOn, noSign     bool
		ackRedist, sbom                                                   bool
		interactive                                                       bool
		digests                                                           map[string]string
	)

	cmd := &cobra.Command{
		Use:   "build [pkg...]",
		Short: "Download packages and their dependencies into an offline bundle.",
		Long: "Build a package bundle on the online computer for a Debian or Ubuntu target.\n" +
			"Use --snapshot FILE for the target's actual package state, or --base ID\n" +
			"for a baseline OS such as ubuntu:24.04/minimal. --base needs no captured\n" +
			"snapshot; choose its architecture with --arch and check that its assumed\n" +
			"installed packages match the target. Do not combine --snapshot and --base.\n" +
			"\n" +
			"Use --interactive to choose the target, packages and output through\n" +
			"questions. It saves entered packages to a list and prints a reusable\n" +
			"command. A terminal is required; --json and --json-events cannot be used.\n" +
			"\n" +
			"Inputs can be package names, name=version requests, https:// .deb URLs,\n" +
			"or local .deb paths. Use --list for a saved list or --local-dir for a\n" +
			"directory of .deb files. Add --sign KEY to require a signed bundle.",
		Example: "  debark build --interactive\n" +
			"  debark build --base ubuntu:24.04/minimal --arch amd64 --out ./bundle --sign operator.key jq\n" +
			"  debark build --snapshot target.tar.zst --out ./bundle --sign operator.key jq",
		Args: cobra.ArbitraryArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) (err error) {
			effectiveBackend, err := checkBackendFlag("build", backend, ctx.Cfg.Backend)
			if err != nil {
				return err
			}
			if archFlag != "" && baseFlag == "" && !interactive {
				// --arch describes a base. A snapshot already carries the
				// target's architecture, measured on the machine itself, and
				// letting a flag contradict it would build for a machine that
				// does not exist. (Resolving for a different architecture than
				// the snapshot records is a separate thing the engine already
				// has, Options.ArchOverride, and is deliberately not wired to
				// this flag.)
				return dferr.Usagef("build: --arch describes a --base; it has no meaning with --snapshot, which records the target's own architecture")
			}

			if interactive {
				answers, ierr := runBuildInteractive(ctx, buildAnswers{
					SnapshotRef: snapshotFlag,
					BaseRef:     baseFlag,
					Arch:        archFlag,
					Packages:    args,
					Out:         out,
					Sign:        sign,
					Upgrades:    upgrades,
					SBOM:        sbom,
				})
				if ierr != nil {
					return ierr
				}
				snapshotFlag, baseFlag, archFlag = answers.SnapshotRef, answers.BaseRef, answers.Arch
				args, out, sign = answers.Packages, answers.Out, answers.Sign
				upgrades, sbom = answers.Upgrades, answers.SBOM
				defer func() { printEquivalentCommand(ctx, buildCommandLine(answers)) }()
			}

			if snapshotFlag == "" && baseFlag == "" {
				return missingBuildTargetError(ctx)
			}

			// One sink for the whole command, opened here rather than just
			// before the engine, because with --base the run starts earlier
			// than the engine does. Two sinks would put two progress
			// renderers on stderr and split one run's evidence into two
			// streams, of which --json-events would carry only the second.
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

			// --base synthesizes a snapshot and then enters the pipeline
			// unchanged. That is the architectural rule of this feature, and
			// it is narrower than "add a subcommand": below this point there
			// is exactly one resolution path and exactly one input artefact,
			// and nothing downstream branches on how the snapshot came to
			// exist. `build --base X` and `snapshot from-base X` followed by
			// `build --snapshot` therefore cannot diverge -- they call the
			// same synthesizer and then the same engine.
			//
			// A snapshot exists in every build whether or not the operator
			// ever sees one, because the format requires it: snapshot_digest
			// is required in both the lock and the manifest, and snapshot.json
			// is a mandatory bundle member (docs/formats.md section 2). The
			// artefact is not optional; only the command to produce it is.
			if baseFlag != "" {
				synthDir, derr := os.MkdirTemp("", "debark-base-")
				if derr != nil {
					return dferr.Wrap(dferr.Environment, derr, "build: create work directory")
				}
				defer func() { _ = os.RemoveAll(synthDir) }()

				if archFlag == "" {
					archFlag = base.HostArch()
				}
				resolved, rerr := base.Resolve(baseFlag, archFlag)
				if rerr != nil {
					return rerr
				}
				result, berr := synthesizeBase(ctx, resolved, baseFlag,
					filepath.Join(synthDir, "base.snapshot.tar.zst"), effectiveBackend, image,
					ctx.selfBinary(selfBinary), events)
				if berr != nil {
					return berr
				}
				snapshotFlag = result.Path
				if !ctx.JSON() {
					fmt.Fprintf(ctx.Stdout, "%s %s: %s assumed installed on the target\n",
						ctx.Style.Warn("assumed base"), resolved.Definition.ID,
						render.Plural(result.Snapshot.InstalledCount, "package"))
				}
			}

			archive, err := snapshot.Open(ctx.Context, snapshotFlag)
			if err != nil {
				return err
			}
			defer func() { _ = archive.Close() }()

			inputs, err := buildInputs(args, lists, localDirs, digests)
			if err != nil {
				return err
			}

			outputPath, outputFormat := out, buildjob.FormatDir
			if tar != "" {
				outputPath, outputFormat = tar, buildjob.FormatTar
			} else if outputPath == "" {
				outputPath = "bundle"
			}

			// Three states, because Options.Recommends is a *bool and the
			// engine already means all three by it: nil follows the target's
			// own Install-Recommends, false and true override it. Only two
			// of the three were expressible from the command line -- there
			// was --no-recommends and nothing that turned it back on -- so
			// an operator whose target has Install-Recommends off could not
			// ask for a bundle that includes recommends, and a caller
			// driving the CLI could not offer the control at all.
			//
			// cobra refuses the two together (MarkFlagsMutuallyExclusive
			// below), so this can read them in either order.
			recommends := recommendsOverride(recommendsOn, noRecommends)
			updateMode := buildjob.UpdateAdditive
			if update {
				updateMode = buildjob.UpdateRefresh
			}

			signerRef, required := resolveSignOptions(sign, noSign, ctx.Cfg.SignKey)

			req := buildjob.BuildRequest{
				SchemaVersion: buildjob.SchemaVersion,
				SnapshotRef:   snapshotFlag,
				Inputs:        inputs,
				Options: buildjob.Options{
					Recommends:                recommends,
					Upgrades:                  upgrades || update,
					UpdateMode:                updateMode,
					Prune:                     !noPrune,
					Backend:                   effectiveBackend,
					Image:                     firstNonEmpty(image, ctx.Cfg.Image),
					PolicyRef:                 firstNonEmpty(policy, ctx.Cfg.PolicyFile),
					ApprovedKeysRef:           firstNonEmpty(approvedKeys, ctx.Cfg.ApprovedKeysFile),
					AcknowledgeRedistribution: ackRedist,
					EmbedBinary:               embedBinary,
					SBOM:                      sbom,
					StoreDir:                  ctx.Cfg.StoreDir,
				},
				Output: buildjob.Output{
					Path:   outputPath,
					Format: outputFormat,
					Sign: buildjob.SignOptions{
						SignerRef: signerRef,
						Required:  required,
					},
				},
			}

			eng, err := engine.New(engine.Deps{Events: events, SelfPath: ctx.selfBinary(selfBinary)})
			if err != nil {
				return err
			}
			result, err := eng.Build(ctx.Context, req)
			if err != nil {
				return err
			}
			return renderBuildResult(ctx, archive.Snapshot, result)
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&snapshotFlag, "snapshot", "", "snapshot of the real target to resolve against (exactly one of --snapshot or --base)")
	flags.StringVar(&baseFlag, "base", "", "stock release to assume instead of a snapshot: a builtin id such as ubuntu:26.04/desktop, or a path to a base definition file")
	flags.StringVar(&archFlag, "arch", "", "dpkg architecture the --base describes (default: this machine's)")
	flags.StringArrayVar(&lists, "list", nil, "packages.txt-style input list; repeatable")
	flags.StringArrayVar(&localDirs, "local-dir", nil, "directory of vendor .deb files, scanned (not recursively); repeatable")
	flags.StringVar(&out, "out", "", "write the bundle as a directory (default when neither --out nor --tar is given: ./bundle)")
	flags.StringVar(&tar, "tar", "", "write the bundle as a <name>.debark.tar.zst archive")
	flags.BoolVar(&update, "update", false, "refresh indexes, re-resolve, fetch newer versions, then prune superseded files")
	flags.BoolVar(&noPrune, "no-prune", false, "with --update, keep superseded files instead of pruning them")
	flags.BoolVar(&upgrades, "upgrades", false, "add a full-upgrade pass for packages already installed on the target")
	flags.BoolVar(&noRecommends, "no-recommends", false, "override the target's Install-Recommends and exclude recommended packages")
	flags.BoolVar(&recommendsOn, "recommends", false, "override the target's Install-Recommends and include recommended packages (default: follow the target)")
	flags.StringVar(&backend, "backend", "", "auto, local or container (default: config backend, else auto)")
	flags.StringVar(&image, "image", "", "override the container image for the container backend")
	flags.StringVar(&selfBinary, "self-binary", "", selfBinaryFlagUsage)
	flags.StringVar(&sign, "sign", "", "signing key: a private key file, gpg:<keyid>, or plugin:<name>")
	flags.BoolVar(&noSign, "no-sign", false, "write an unsigned bundle explicitly")
	flags.StringVar(&approvedKeys, "approved-keys", "", "archive key fingerprints resolution must satisfy")
	flags.StringVar(&policy, "policy", "", "local policy file evaluated against the plan")
	flags.BoolVar(&ackRedist, "acknowledge-redistribution", false, "suppress the interactive redistribution prompt (warnings are still recorded)")
	flags.StringVar(&embedBinary, "embed-binary", "", "copy a debark binary into the bundle at bin/debark-linux-<arch>")
	flags.BoolVar(&sbom, "sbom", false, "write sbom.cdx.json (CycloneDX)")
	flags.StringToStringVar(&digests, "digest", nil, "expected digest for a URL input, upgrading its provenance to user-digest (URL=SHA256); repeatable")

	flags.BoolVar(&interactive, "interactive", false, "ask for anything not given on the command line, then print the equivalent command (a terminal is required)")

	// Exactly one of --snapshot and --base, rather than --snapshot required:
	// the two name the same thing -- which machine is this bundle for -- in two
	// different ways, and a build given both would have to pick one and
	// silently ignore the other. --interactive is the one way to give neither,
	// because it goes on to ask.
	cmd.MarkFlagsMutuallyExclusive("snapshot", "base")
	cmd.MarkFlagsOneRequired("snapshot", "base", "interactive")
	cmd.MarkFlagsMutuallyExclusive("out", "tar")
	cmd.MarkFlagsMutuallyExclusive("sign", "no-sign")
	// --recommends and --no-recommends name the same override in opposite
	// directions; a build given both would have to pick one and silently
	// ignore the other, exactly as with --snapshot/--base above.
	cmd.MarkFlagsMutuallyExclusive("recommends", "no-recommends")
	// There is deliberately no MarkFlagsMutuallyExclusive("interactive",
	// "json"): --json is inherited from the root's persistent set, and
	// cobra's flag-group check does not see it there, so the marking would
	// advertise a check it never performs. requireInteractive refuses the
	// combination at run time and explains why, which is the better message
	// in any case.
	return cmd
}

// missingBuildTargetError is what a guided run that answered nothing gets.
// The non-interactive case never reaches here: cobra's MarkFlagsOneRequired
// refuses it before RunE runs.
func missingBuildTargetError(ctx *Ctx) error {
	err := dferr.New(dferr.Usage, "build: give either --snapshot or --base")
	if ctx.Interactive() {
		return err.WithHint("`debark snapshot create` on the target machine gives you a snapshot; `debark snapshot list-bases` shows the releases --base accepts; `debark build --interactive` walks you through it")
	}
	return err.WithHint("`debark snapshot create` on the target machine gives you a snapshot; `debark snapshot list-bases` shows the releases --base accepts")
}

// buildInputs classifies positional CLI arguments per the grammar
// (apt:/url:/file: prefixes optional; https:// is a URL; a path ending in
// .deb is a file; anything else is an apt package name, optionally
// name=version) and merges in every --list file via fetch.ParseListFile,
// which is the single authoritative implementation of that same grammar for
// list files. digests applies --digest URL=SHA256
// overrides, upgrading a URL input's provenance to user-digest.
func buildInputs(positional, lists, localDirs []string, digests map[string]string) (buildjob.Inputs, error) {
	var in buildjob.Inputs
	for _, a := range positional {
		kind, val := classifyBuildArg(a)
		switch kind {
		case "url":
			in.URLs = append(in.URLs, buildjob.URLInput{URL: val, SHA256: digests[val]})
		case "file":
			in.Files = append(in.Files, val)
		default:
			in.Packages = append(in.Packages, val)
		}
	}
	for _, lf := range lists {
		expanded, err := fetch.ParseListFile(lf)
		if err != nil {
			return in, err
		}
		in.Packages = append(in.Packages, expanded.Packages...)
		for _, u := range expanded.URLs {
			if u.SHA256 == "" {
				u.SHA256 = digests[u.URL]
			}
			in.URLs = append(in.URLs, u)
		}
		in.Files = append(in.Files, expanded.Files...)
		in.ListFiles = append(in.ListFiles, lf)
	}
	in.LocalDirs = append(in.LocalDirs, localDirs...)
	return in, nil
}

func classifyBuildArg(s string) (kind, value string) {
	switch {
	case strings.HasPrefix(s, "apt:"):
		return "package", strings.TrimPrefix(s, "apt:")
	case strings.HasPrefix(s, "url:"):
		return "url", strings.TrimPrefix(s, "url:")
	case strings.HasPrefix(s, "file:"):
		return "file", strings.TrimPrefix(s, "file:")
	case strings.HasPrefix(s, "https://"), strings.HasPrefix(s, "http://"):
		return "url", s
	case strings.HasSuffix(s, ".deb"):
		return "file", s
	default:
		return "package", s
	}
}

// resolveSignOptions applies build's --sign/--no-sign policy: an explicit
// key is required to work; an explicit --no-sign is always honoured; with
// neither given, a configured default key is used but is not required to
// succeed, so a build never hard-fails only because no key is configured —
// it writes an unsigned bundle and says so (BuildResult.Signed=false).
func resolveSignOptions(sign string, noSign bool, configDefault string) (ref string, required bool) {
	if noSign {
		return "", false
	}
	if sign != "" {
		return sign, true
	}
	return configDefault, false
}

// recommendsOverride turns build's two boolean flags into Options.Recommends'
// three states. Neither flag given is nil: follow the target's own apt.conf,
// which is the right default because the target is the machine the bundle has
// to install on.
func recommendsOverride(on, off bool) *bool {
	switch {
	case on:
		v := true
		return &v
	case off:
		v := false
		return &v
	default:
		return nil
	}
}

func isOneOf(s string, allowed ...string) bool {
	for _, a := range allowed {
		if s == a {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func renderBuildResult(ctx *Ctx, snap *snapshot.Snapshot, result *buildjob.BuildResult) error {
	if ctx.JSON() {
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, string(b))
	} else {
		printBuildHuman(ctx, snap, result)
	}
	if result.ExitClass != buildjob.ExitSuccess {
		return buildFailureError(result)
	}
	return nil
}

// buildFailureError is the one line a failing build writes to stderr.
//
// A BuildResult with a non-success class always means a bundle EXISTS and
// something about it is short of what was asked for (core/engine's
// buildResult: every earlier failure returns a real error instead), so the
// line has to name the two things a caller can act on — where the bundle is,
// and what is missing from it. The class alone ("incomplete") says only
// which of two kinds of shortfall it was; it never named the package or the
// URL, and stdout was the only place that did.
func buildFailureError(r *buildjob.BuildResult) error {
	class := exitClassFromString(string(r.ExitClass))

	var parts []string
	if n := len(r.Unresolved); n > 0 {
		parts = append(parts, fmt.Sprintf("%s apt could not satisfy: %s",
			render.Plural(n, "input"), nameSome(r.Unresolved)))
	}
	if n := len(r.FetchFailed); n > 0 {
		parts = append(parts, fmt.Sprintf("%s that did not download: %s",
			render.Plural(n, "URL"), nameSome(r.FetchFailed)))
	}
	if len(parts) == 0 {
		// The remaining way to reach a non-success class is a failed
		// closed-world check, which records its explanation as a warning
		// (core/engine/finalize.go). BuildResult carries the warning
		// sentences but not their codes, so the sentence is matched on:
		// picking the wrong one costs a less specific stderr line and
		// nothing else, and picking none at all is what this replaces.
		if w := firstWarningAbout(r.Warnings, "closed-world"); w != "" {
			parts = append(parts, w)
		}
	}

	msg := fmt.Sprintf("build: %s: %s", r.BundlePath, r.ExitClass)
	if len(parts) > 0 {
		msg += ": " + strings.Join(parts, "; ")
	}
	err := dferr.New(class, "%s", msg)
	if r.ExitClass == buildjob.ExitIncomplete {
		return err.WithHint("the bundle holds everything that did resolve; supply the missing input as a local .deb (--local-dir) or drop it from the request, then re-run")
	}
	return err
}

// firstWarningAbout returns the first warning sentence containing substr, or
// "" when none does.
func firstWarningAbout(warnings []string, substr string) string {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return w
		}
	}
	return ""
}

// printBuildHuman answers the design's four questions in order: what
// target, what will be installed, how much must be downloaded, can this
// artifact be trusted.
func printBuildHuman(ctx *Ctx, snap *snapshot.Snapshot, r *buildjob.BuildResult) {
	status := ctx.Style.OK("OK")
	switch r.ExitClass {
	case buildjob.ExitSuccess:
	case buildjob.ExitIncomplete:
		status = ctx.Style.Warn("INCOMPLETE")
	default:
		status = ctx.Style.Fail(string(r.ExitClass))
	}
	fmt.Fprintf(ctx.Stdout, "%s  %s\n", status, r.BundlePath)

	// What target.
	fmt.Fprintf(ctx.Stdout, "  target:     %s %s (%s) %s\n",
		snap.Target.DistroID, snap.Target.VersionID, snap.Target.Codename, snap.Target.Arch)
	// What will be installed.
	fmt.Fprintf(ctx.Stdout, "  packages:   %s in the bundle (%d added, %d unchanged, %d removed this run)\n",
		render.Plural(r.Stats.PackageCount, "package"), r.Stats.Added, r.Stats.Unchanged, r.Stats.Removed)
	// How much must be downloaded.
	fmt.Fprintf(ctx.Stdout, "  size:       %s total, %s fetched this run\n",
		render.Bytes(r.Stats.Bytes), render.Bytes(r.Stats.DownloadedBytes))
	// Can this artifact be trusted.
	trust := ctx.Style.Warn("unsigned")
	if r.Signed {
		trust = ctx.Style.OK("signed")
	}
	fmt.Fprintf(ctx.Stdout, "  trust:      %s\n", trust)

	for _, w := range r.Warnings {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Warn("warning:"), w)
	}
	for _, u := range r.Unresolved {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Fail("unresolved:"), u)
	}
	for _, f := range r.FetchFailed {
		fmt.Fprintf(ctx.Stdout, "  %s %s\n", ctx.Style.Fail("fetch failed:"), f)
	}
}
