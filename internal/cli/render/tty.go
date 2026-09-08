// Package render is debark's terminal-output layer: TTY detection, the
// NO_COLOR/TERM=dumb colour policy, a small lipgloss-backed colour helper
// and a lipgloss/table-backed table — the narrow slice of what
// charmbracelet/lipgloss provides that debark actually uses (colour and
// tables only, never a full-screen UI).
//
// Nothing in core/ may import this package; it exists so progress and human
// summaries can live in the CLI while core/ stays terminal-free
// (contract-brief.md rule 7.1 principle 7).
package render

import (
	"os"

	"github.com/mattn/go-isatty"
)

// IsTerminal reports whether f is connected to an interactive terminal.
//
// mattn/go-isatty checks the real OS mechanism (an ioctl on Unix,
// GetConsoleMode on Windows) rather than a stat-based heuristic, and — the
// case a stat-based check gets wrong most often on Windows — also
// recognises an MSYS/Cygwin/Git-Bash pty, which GetConsoleMode alone
// reports as not a console at all even though it is an interactive
// terminal.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// DumbTerminal reports whether TERM=dumb, which conventionally means "no
// cursor control, no colour" even when the stream is otherwise a TTY (e.g. an
// Emacs subprocess, some serial consoles).
func DumbTerminal() bool { return os.Getenv("TERM") == "dumb" }

// NoColorEnv reports whether the NO_COLOR convention (https://no-color.org)
// is set: any non-empty value disables colour, unconditionally.
func NoColorEnv() bool { return os.Getenv("NO_COLOR") != "" }
