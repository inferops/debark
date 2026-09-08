package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/version"
)

// debark gen-manpages — hidden. Debian packaging needs man pages;
// this is what a package build (not an end user) runs, once, to produce
// them: one man(1) page per command in the tree, walking the cobra tree
// cobra already built (Use/Short/Long/Example and every flag's usage text)
// rather than this file maintaining a second copy of that documentation.
//
// cobra/doc.GenManTree would normally do this rendering, but its import
// requires github.com/cpuguy83/go-md2man/v2 (it feeds each command's
// Markdown-formatted long description through md2man.Render to get troff),
// and that module is not among the dependencies restored into go.sum for
// this package (cobra, pflag, koanf and friends are; go-md2man is cobra/doc's
// own transitive dependency, one level further than what was verified).
// Rather than adding a dependency nothing
// in go.sum currently provides, genManPage below writes troff directly: a
// debark command's Long text is already plain sentences, never Markdown,
// so there is nothing for a Markdown-to-troff converter to do here beyond
// what escapeTroff already handles (backslashes and leading dots, the two
// characters troff itself treats specially).
func newGenManPagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "gen-manpages DIR",
		Hidden: true,
		Short:  "Generate man pages for the whole command tree.",
		Long:   "Hidden packaging command: writes one man(1) page per command under DIR. Run once at package-build time, not by end users.",
		Args:   cobra.ExactArgs(1),
		RunE: wrapRun(func(ctx *Ctx, args []string) error {
			dir := args[0]
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return dferr.Wrap(dferr.Environment, err, "gen-manpages: create %s", dir)
			}
			n, err := genManTree(ctx.cmd.Root(), dir)
			if err != nil {
				return dferr.Wrap(dferr.Environment, err, "gen-manpages: write %s", dir)
			}
			fmt.Fprintf(ctx.Stdout, "%s %d man page(s) under %s\n", ctx.Style.OK("wrote"), n, dir)
			return nil
		}),
	}
	return cmd
}

// genManTree writes one page per non-hidden command in cmd's tree (hidden
// commands are debugging/packaging internals, not part of the documented
// surface a man page describes) and returns how many it wrote.
func genManTree(cmd *cobra.Command, dir string) (int, error) {
	n := 0
	for _, c := range cmd.Commands() {
		if c.Hidden {
			continue
		}
		cn, err := genManTree(c, dir)
		if err != nil {
			return n, err
		}
		n += cn
	}
	if !cmd.Runnable() && !cmd.HasAvailableSubCommands() {
		return n, nil
	}
	path := filepath.Join(dir, manPageName(cmd))
	if err := os.WriteFile(path, []byte(genManPage(cmd)), 0o644); err != nil {
		return n, err
	}
	return n + 1, nil
}

func manPageName(cmd *cobra.Command) string {
	return strings.ReplaceAll(cmd.CommandPath(), " ", "-") + ".1"
}

func genManPage(cmd *cobra.Command) string {
	var b strings.Builder
	title := strings.ToUpper(strings.ReplaceAll(cmd.CommandPath(), " ", "-"))
	date := time.Now().UTC().Format("2006-01-02")
	source := version.Name + " " + version.Get().Version

	fmt.Fprintf(&b, ".TH %s 1 %q %q %q\n", title, date, source, "debark Manual")

	fmt.Fprintln(&b, ".SH NAME")
	name := strings.ReplaceAll(cmd.CommandPath(), " ", "-")
	short := cmd.Short
	if short == "" {
		short = cmd.Long
	}
	fmt.Fprintf(&b, "%s \\- %s\n", escapeTroff(name), escapeTroff(short))

	fmt.Fprintln(&b, ".SH SYNOPSIS")
	usage := cmd.UseLine()
	if usage == "" {
		usage = cmd.CommandPath()
	}
	fmt.Fprintf(&b, "\\fB%s\\fR\n", escapeTroff(usage))

	if cmd.Long != "" {
		fmt.Fprintln(&b, ".SH DESCRIPTION")
		for _, line := range strings.Split(strings.TrimRight(cmd.Long, "\n"), "\n") {
			fmt.Fprintln(&b, escapeTroff(line))
		}
	}

	if cmd.HasAvailableSubCommands() {
		fmt.Fprintln(&b, ".SH COMMANDS")
		var subs []*cobra.Command
		for _, c := range cmd.Commands() {
			if !c.Hidden {
				subs = append(subs, c)
			}
		}
		sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
		for _, c := range subs {
			fmt.Fprintf(&b, ".TP\n\\fB%s\\fR\n%s\n", escapeTroff(c.Name()), escapeTroff(c.Short))
		}
	}

	flagLines := manFlagLines(cmd)
	if len(flagLines) > 0 {
		fmt.Fprintln(&b, ".SH OPTIONS")
		for _, fl := range flagLines {
			fmt.Fprintf(&b, ".TP\n\\fB--%s\\fR%s\n%s\n", escapeTroff(fl.name), escapeTroff(fl.valueSuffix), escapeTroff(fl.usage))
		}
	}

	if cmd.Example != "" {
		fmt.Fprintln(&b, ".SH EXAMPLES")
		for _, line := range strings.Split(strings.TrimRight(cmd.Example, "\n"), "\n") {
			fmt.Fprintln(&b, escapeTroff(line))
		}
	}

	fmt.Fprintln(&b, ".SH SEE ALSO")
	fmt.Fprintf(&b, "\\fBdebark\\fR(1)\n")

	return b.String()
}

type manFlag struct {
	name        string
	valueSuffix string
	usage       string
}

// manFlagLines lists cmd's own flags plus every inherited (persistent)
// flag, sorted, deduplicated by name (a locally redeclared name — none
// exist in this tree — would otherwise shadow the inherited one twice).
func manFlagLines(cmd *cobra.Command) []manFlag {
	seen := map[string]bool{}
	var out []manFlag
	add := func(name, usage, varType string) {
		if seen[name] {
			return
		}
		seen[name] = true
		suffix := ""
		if varType != "bool" {
			suffix = " " + strings.ToUpper(varType)
		}
		out = append(out, manFlag{name: name, valueSuffix: suffix, usage: usage})
	}
	visit := func(f *pflag.Flag) { add(f.Name, f.Usage, f.Value.Type()) }
	cmd.LocalFlags().VisitAll(visit)
	cmd.InheritedFlags().VisitAll(visit)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// escapeTroff neutralises the two characters troff treats specially in
// plain text: a leading '.' or ”' would start a request, and a bare '\\'
// would start an escape sequence.
func escapeTroff(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "'") {
		s = `\&` + s
	}
	return s
}
