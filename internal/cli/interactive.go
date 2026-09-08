package cli

// The guided flows for `build` and `snapshot from-base`.
//
// Six rules govern everything in this file. They are what keep a guided flow
// a way *into* the CLI rather than a second product, and every one of them is
// load-bearing:
//
//  1. Line-oriented, never a full-screen UI. The permanent
//     do-not-build list forbids "a GUI, or a full-screen TUI as the primary
//     interface", because debark must work over SSH, serial consoles, CI
//     logs and script(1) transcripts. internal/cli/prompt enforces the
//     mechanics; this file must not reach around it.
//  2. A terminal is required. Ctx.Interactive is the only gate, and
//     requireInteractive below is the only place it is consulted.
//  3. --json and --json-events never prompt. Ctx.Interactive already folds
//     that in (makes both imply non-interactive), so it falls out of
//     rule 2 rather than being a second check that could drift.
//  4. Flags always win. Every step is skipped when the corresponding flag was
//     given. Nothing here re-asks or overrides an explicit flag; the "given"
//     struct each flow receives is exactly what the command line supplied.
//  5. Never auto-enter on a missing argument. A bare `build` with no packages
//     must not start reading stdin — that is how tools hang in CI when a
//     variable expands empty. --interactive is always explicit; see
//     missingBaseError and missingBuildInputError for what a bare invocation
//     gets instead.
//  6. It ends by printing the equivalent non-interactive command, and writes
//     out what it collected as a real packages.txt. The operator can paste
//     the command into a runbook, diff the list, and learn the flags by
//     using them — which is what keeps the promise honest that the CLI is
//     the complete interface.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/internal/cli/prompt"
)

// defaultCollectedList is where a guided run writes what it collected.
const defaultCollectedList = "packages.txt"

// requireInteractive is rule 2 and rule 3, in one place.
//
// The refusal is deliberately specific about which of the two conditions
// failed, because they call for different fixes: a redirected stdin means
// "pass the flags instead", and --json means "you already asked for the
// automation contract".
func requireInteractive(ctx *Ctx, cmdName string) (*prompt.Prompter, error) {
	if !ctx.Interactive() {
		if ctx.JSON() || ctx.jsonEvents != "" {
			return nil, dferr.Usagef(
				"%s: --interactive cannot be combined with --json or --json-events; those are the automation contract and never prompt", cmdName)
		}
		return nil, dferr.New(dferr.Usage,
			"%s: --interactive needs a terminal, and stdin is not one", cmdName).
			WithHint("pass the values as flags instead — the guided flow prints the equivalent command when it finishes, so run it once on a terminal to see what to write")
	}
	return prompt.New(ctx.Stdin, ctx.Stdout, ctx.Style), nil
}

// printEquivalentCommand is rule 6's first half.
func printEquivalentCommand(ctx *Ctx, argv []string) {
	fmt.Fprintf(ctx.Stdout, "\n%s\n  %s\n\n",
		ctx.Style.Bold("The same run, without the questions:"), shellJoin(argv))
}

// shellJoin renders an argv as a line an operator can paste. Quoting is
// POSIX-shell single-quote style, which is also what a Windows operator
// pasting into Git Bash or WSL gets — and those are the shells that actually
// run debark, since the target side is always Linux.
//
// It quotes conservatively: anything outside a small safe set is quoted
// whole. A URL with a query string is the case that matters — an unquoted `&`
// would background the command and silently run something else.
func shellJoin(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, shellQuote(a))
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '/', r == ':', r == '=', r == '+', r == ',', r == '@':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- the base picker, shared by both flows ---------------------------------

// pickBase asks distro, then release, then variant — three short lists, each
// defaulting to the sensible answer. Architecture is asked separately by the
// caller, since `build` may already know it from a snapshot.
//
// The caveat is shown before the first question, not after the last, because
// it changes whether the operator should be here at all.
func pickBase(p *prompt.Prompter, ctx *Ctx) (string, error) {
	p.Section("Which release is the target?")
	p.Say("%s", ctx.Style.Warn("Note: ")+baseAssumptionCaveat+".")

	// Every list below comes from core/base's TABLE order, never from
	// BuiltinIDs, which is sorted. That distinction is load-bearing rather
	// than tidy, and it was got wrong first time round: deriving the variant
	// list from the sorted ids produced "desktop, minimal, server", so the
	// default became desktop — the LARGEST claim of the three, and the exact
	// inversion of the err-toward-not-installed rule this whole feature is
	// built on. Found by running the flow through a pty; no unit test would
	// have noticed, because every option was present and only their order
	// was wrong.
	chosenDistro, err := p.Choose("Distribution", optionsFrom(base.DistroIDs()), 0)
	if err != nil {
		return "", err
	}

	releases := base.ReleaseIDs(chosenDistro.Value)
	// Table order is oldest first, so the last entry is the newest release —
	// the sensible default for a machine being provisioned now.
	chosenRelease, err := p.Choose("Release", optionsFrom(releases), len(releases)-1)
	if err != nil {
		return "", err
	}

	// Index 0 is the least inclusive profile. The smallest claim is the safe
	// default: it produces a bundle larger than it needed to be rather than
	// one that is short.
	chosenVariant, err := p.Choose("Install profile", variantOptions(base.VariantNames()), 0)
	if err != nil {
		return "", err
	}
	return chosenRelease.Value + "/" + chosenVariant.Value, nil
}

func pickArch(p *prompt.Prompter, def string) (string, error) {
	if def == "" {
		def = base.HostArch()
	}
	primary := distro.PrimaryArchitectures()
	opts := optionsFrom(primary)
	defIndex := 0
	for i, a := range primary {
		if a == def {
			defIndex = i
		}
	}
	// The CI-tested architectures are offered as a list; anything else is
	// still reachable, by typing it, because the product supports more than
	// it tests and a picker that hid them would be narrower than the product.
	opts = append(opts, prompt.Option{Value: "other", Label: "other", Note: "type a dpkg architecture name"})
	chosen, err := p.Choose("Architecture", opts, defIndex)
	if err != nil {
		return "", err
	}
	if chosen.Value != "other" {
		return chosen.Value, nil
	}
	return p.Line("dpkg architecture", "")
}

func optionsFrom(values []string) []prompt.Option {
	out := make([]prompt.Option, 0, len(values))
	for _, v := range values {
		out = append(out, prompt.Option{Value: v})
	}
	return out
}

// variantOptions annotates each profile with the description the builtin
// table already carries, so the picker and `list-bases` say the same thing
// about the same profile rather than two paraphrases of it.
func variantOptions(names []string) []prompt.Option {
	out := make([]prompt.Option, 0, len(names))
	for _, n := range names {
		out = append(out, prompt.Option{Value: n, Note: base.VariantDescription(n)})
	}
	return out
}

// --- snapshot from-base ----------------------------------------------------

type fromBaseAnswers struct {
	BaseRef string
	Arch    string
	Out     string
}

func runFromBaseInteractive(ctx *Ctx, given fromBaseAnswers) (fromBaseAnswers, error) {
	p, err := requireInteractive(ctx, "snapshot from-base")
	if err != nil {
		return given, err
	}
	out := given

	if out.BaseRef == "" {
		ref, err := pickBase(p, ctx)
		if err != nil {
			return given, err
		}
		out.BaseRef = ref
	}
	if out.Arch == "" {
		a, err := pickArch(p, "")
		if err != nil {
			return given, err
		}
		out.Arch = a
	}
	if out.Out == "" {
		resolved, rerr := base.Resolve(out.BaseRef, out.Arch)
		suggested := "base.snapshot.tar.zst"
		if rerr == nil {
			suggested = defaultBaseSnapshotName(resolved.Definition)
		}
		p.Section("Output")
		got, err := p.Line("Write the snapshot to", suggested)
		if err != nil {
			return given, err
		}
		out.Out = got
	}

	p.Section("Ready")
	p.Say("base:   %s", out.BaseRef)
	p.Say("arch:   %s", out.Arch)
	p.Say("out:    %s", out.Out)
	p.Say("Nothing has touched the network yet.")
	ok, err := p.Confirm("Resolve the base's seed packages now", true)
	if err != nil {
		return given, err
	}
	if !ok {
		return given, dferr.New(dferr.Usage, "snapshot from-base: cancelled")
	}
	return out, nil
}

// --- build -----------------------------------------------------------------

type buildAnswers struct {
	SnapshotRef string
	BaseRef     string
	Arch        string
	Packages    []string
	ListPath    string
	Out         string
	Sign        string
	Upgrades    bool
	SBOM        bool
}

func runBuildInteractive(ctx *Ctx, given buildAnswers) (buildAnswers, error) {
	p, err := requireInteractive(ctx, "build")
	if err != nil {
		return given, err
	}
	out := given

	// 1. Snapshot first. It is the accurate answer, and a base is the
	//    fallback for when no target exists yet — leading with the base here
	//    would make the weaker option the documented default, which is
	//    exactly what this feature must not become.
	if out.SnapshotRef == "" && out.BaseRef == "" {
		p.Section("What is the target?")
		haveSnapshot, err := p.Confirm("Do you have a snapshot of the target machine", false)
		if err != nil {
			return given, err
		}
		if haveSnapshot {
			path, err := p.Line("Path to the snapshot", "target.snapshot.tar.zst")
			if err != nil {
				return given, err
			}
			out.SnapshotRef = path
		} else {
			p.Say("No snapshot, then — describing the target by its release instead.")
			ref, err := pickBase(p, ctx)
			if err != nil {
				return given, err
			}
			out.BaseRef = ref
			a, err := pickArch(p, out.Arch)
			if err != nil {
				return given, err
			}
			out.Arch = a
		}
	}

	// 2. Packages.
	if len(out.Packages) == 0 {
		p.Section("What should the bundle contain?")
		p.Say("One entry per line, blank line to finish. Package names, name=version,")
		p.Say("https:// URLs to .deb files and local .deb paths all work — the same")
		p.Say("grammar as a packages.txt, so you can paste a list straight in.")
		entries, err := p.Collect("package, URL or .deb path", classifyForPrompt)
		if err != nil {
			return given, err
		}
		if len(entries) == 0 {
			return given, dferr.New(dferr.Usage, "build: nothing to build; no packages were given")
		}
		out.Packages = entries
	}

	// 3. Options.
	p.Section("Options")
	if out.Out == "" {
		got, err := p.Line("Write the bundle to", "bundle")
		if err != nil {
			return given, err
		}
		out.Out = got
	}
	if out.Sign == "" {
		out.Sign, err = askSigningKey(p, ctx)
		if err != nil {
			return given, err
		}
	}
	if !out.Upgrades {
		out.Upgrades, err = p.Confirm("Also include upgrades for packages the target already has", false)
		if err != nil {
			return given, err
		}
	}
	if !out.SBOM {
		out.SBOM, err = p.Confirm("Write a CycloneDX SBOM alongside the bundle", false)
		if err != nil {
			return given, err
		}
	}

	// 4. Write out what was collected (rule 6), before the review, so the
	//    review can name the file and the operator can still say no.
	if len(given.Packages) == 0 && out.ListPath == "" {
		path, err := writeCollectedList(out.Packages)
		if err != nil {
			return given, err
		}
		out.ListPath = path
		p.Say("Saved what you entered to %s — edit it and re-run with --list %s to repeat this.", path, path)
	}

	// 5. Review.
	p.Section("Ready")
	if out.SnapshotRef != "" {
		p.Say("target:   snapshot %s", out.SnapshotRef)
	} else {
		p.Say("target:   base %s (%s) — assumed, not measured", out.BaseRef, out.Arch)
	}
	p.Say("packages: %d requested", len(out.Packages))
	p.Say("bundle:   %s", out.Out)
	if out.Sign == "" {
		p.Say("signing:  %s", ctx.Style.Warn("unsigned"))
	} else {
		p.Say("signing:  %s", out.Sign)
	}
	p.Say("Nothing has touched the network yet.")
	ok, err := p.Confirm("Resolve and build now", true)
	if err != nil {
		return given, err
	}
	if !ok {
		return given, dferr.New(dferr.Usage, "build: cancelled")
	}
	return out, nil
}

// askSigningKey offers to make a key when none is configured, because "no
// signing key" is the single most common reason a first bundle goes out
// unsigned, and an unsigned bundle is the one that cannot be trusted after it
// crosses the gap.
func askSigningKey(p *prompt.Prompter, ctx *Ctx) (string, error) {
	if ctx.Cfg.SignKey != "" {
		p.Say("signing key: %s (from your config)", ctx.Cfg.SignKey)
		return ctx.Cfg.SignKey, nil
	}
	sign, err := p.OptionalLine("Signing key (a key file, gpg:<keyid> or plugin:<name>; blank to leave the bundle unsigned)", "")
	if err != nil {
		return "", err
	}
	if sign == "" {
		p.Say("%s an unsigned bundle proves nothing about who built it. Make one with `debark keygen --out operator.key`.",
			ctx.Style.Warn("warning:"))
	}
	return sign, nil
}

// classifyForPrompt is the echo rule/2 of the packages step: it names what
// each entry actually is, at the moment the operator types it.
//
// This is where the guided flow earns its place. debark's real
// differentiator — that a vendor .deb's dependencies get resolved against the
// archive, not just downloaded — is invisible in a flag, and normally shows
// up only once a bundle has already crossed an air gap. Saying it here makes
// it visible while the operator is still deciding.
//
// It classifies with classifyBuildArg, the same function the non-interactive
// path uses on a positional argument, so the guided flow can never disagree
// with the command line about what an entry means.
func classifyForPrompt(entry string) (string, error) {
	kind, value := classifyBuildArg(entry)
	switch kind {
	case "url":
		return value + " — vendor .deb, dependencies will be resolved", nil
	case "file":
		if _, err := os.Stat(value); err != nil {
			return "", dferr.New(dferr.Usage, "no such file: %s", value)
		}
		return value + " — local .deb, dependencies will be resolved", nil
	default:
		if name, version, ok := strings.Cut(value, "="); ok {
			return name + " — apt package, pinned to " + version, nil
		}
		return value + " — apt package", nil
	}
}

// writeCollectedList is rule 6's second half: what the operator typed becomes
// a real file, so the run is reproducible, diffable and re-editable.
//
// It refuses to overwrite. A packages.txt already in the working directory is
// somebody's input, and silently replacing it with whatever was just typed
// into a prompt would be the worst kind of data loss — quiet, and to the file
// the operator would reach for to recover.
func writeCollectedList(entries []string) (string, error) {
	path := defaultCollectedList
	for i := 2; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		if i > 100 {
			return "", dferr.New(dferr.Environment,
				"build: could not find an unused name for the collected package list next to %s", defaultCollectedList)
		}
		ext := filepath.Ext(defaultCollectedList)
		path = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(defaultCollectedList, ext), i, ext)
	}

	var b strings.Builder
	b.WriteString("# Written by `debark build --interactive`.\n")
	b.WriteString("# One entry per line: a package name, name=version, an https:// URL to a\n")
	b.WriteString("# .deb, or a path to a local .deb. Re-run with --list " + path + ".\n")
	for _, e := range entries {
		b.WriteString(e)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "build: writing %s", path)
	}
	return path, nil
}

// buildCommandLine renders the non-interactive equivalent of a guided build.
func buildCommandLine(a buildAnswers) []string {
	argv := []string{"debark", "build"}
	switch {
	case a.SnapshotRef != "":
		argv = append(argv, "--snapshot", a.SnapshotRef)
	case a.BaseRef != "":
		argv = append(argv, "--base", a.BaseRef)
		if a.Arch != "" {
			argv = append(argv, "--arch", a.Arch)
		}
	}
	if a.Out != "" {
		argv = append(argv, "--out", a.Out)
	}
	if a.Sign != "" {
		argv = append(argv, "--sign", a.Sign)
	}
	if a.Upgrades {
		argv = append(argv, "--upgrades")
	}
	if a.SBOM {
		argv = append(argv, "--sbom")
	}
	// The saved list, not the fifty entries expanded back onto the command
	// line: --list is what the operator should actually keep, and it is what
	// makes the printed command match the file sitting next to it.
	if a.ListPath != "" {
		argv = append(argv, "--list", a.ListPath)
		return argv
	}
	return append(argv, a.Packages...)
}
