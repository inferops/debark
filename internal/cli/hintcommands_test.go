package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// backtickedCommand matches a `debark ...` command written between
// backticks, which is how this codebase spells one inside a hint, an error
// message or a doc comment.
var backtickedCommand = regexp.MustCompile("`debark ([^`\n]+)`")

// TestHintedCommandsExist fails when any Go file in this repository names a
// `debark` subcommand the binary does not have.
//
// Written after finding two:
//
//   - core/sign/ed25519.go told an operator whose private key could not be
//     read to generate one with "debark key generate". There is no "key"
//     command; it is `debark keygen --out PATH`. This is the worst place
//     for it — a hint exists to be typed, and typing that one printed
//     unknown command "key" for "debark".
//   - core/apt/closure.go described ClosureSchemaVersion as the document a
//     hidden "snapshot closure" subcommand printed, and called that the way
//     the container backend carried a Closure across the container boundary.
//     There is no such subcommand and there is no such crossing:
//     core/apt/basecontainer.go runs snapshot from-base end to end inside the
//     container and brings back one snapshot archive.
//
// Neither could be caught by anything else. A hint is a string; nothing
// compiles it, and no test ran it.
//
// Note that neither wrong command is written in backticks above, here or in
// the two files that carried them. That is the rule this test creates rather
// than an exception to it: backticks around `debark ...` mean "this is a
// command you can run", so a command that cannot be run must not be written
// that way even when the subject is that it does not exist. Quoting it as
// prose says the same thing and stays true.
//
// Scope is Go files only, deliberately. docs/experiments/ is a historical
// record of what was actually run and must never be "corrected", and the
// same argument applies to any prose recording a past state. Code is
// different: a hint in code is advice being given now.
func TestHintedCommandsExist(t *testing.T) {
	root := repoRoot(t)
	cmd := newRootCmd()

	var checked int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "tmp", "vendor", "node_modules", ".demo":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range backtickedCommand.FindAllSubmatch(b, -1) {
			checked++
			if bad := unknownSubcommand(cmd, string(m[1])); bad != "" {
				t.Errorf("%s: `debark %s` names %q, which is not a command.\n"+
					"A hint exists to be typed; typing this one prints an error.", rel, m[1], bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	// A regex that silently stops matching would turn this test into a
	// no-op that passes forever.
	if checked < 20 {
		t.Fatalf("only %d backticked commands found in the whole repository; the pattern has probably stopped matching", checked)
	}
	t.Logf("checked %d backticked `debark ...` commands", checked)
}

// unknownSubcommand walks the command tree along the leading words of line
// and returns the first word that should have been a subcommand and was not,
// or "" when the whole path resolves.
//
// It stops descending the moment the current command has no subcommands of
// its own: everything after that is arguments, so `debark verify
// some-folder` is fine and "debark key generate" is not. It also stops at
// the first word that cannot be a subcommand — one beginning with "-", one
// containing an upper-case letter (a placeholder such as BUNDLE or KEYFILE),
// or one carrying punctuation no command name uses.
//
// cobra's own Find is deliberately not used. legacyArgs only reports an
// unknown subcommand for the ROOT command, so Find accepts `snapshot
// closure` without complaint — which is the same behaviour C6 of the GUI's
// cli-surface.md records for a real invocation, and exactly the case this
// test has to catch.
func unknownSubcommand(cmd *cobra.Command, line string) string {
	for _, word := range strings.Fields(line) {
		if !cmd.HasSubCommands() {
			return "" // the rest are arguments
		}
		if !looksLikeCommandName(word) {
			return ""
		}
		child := findChild(cmd, word)
		if child == nil {
			return word
		}
		cmd = child
	}
	return ""
}

func looksLikeCommandName(word string) bool {
	if word == "" || strings.HasPrefix(word, "-") {
		return false
	}
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// findChild matches by name or alias, and finds hidden commands too: resolve,
// fetch and apt-root are hidden, and a hint that named one wrongly would be
// no less wrong for it.
func findChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

// repoRoot walks up from the test's working directory to the directory
// holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
