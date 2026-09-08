package render

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Style wraps text in ANSI codes when enabled, and returns it unchanged
// otherwise, so callers never need to branch on colour support themselves.
//
// It is a thin, fixed-palette shim over lipgloss (bold/faint plus the four
// status colours debark actually uses) rather than lipgloss.Style
// directly, for one reason: colour-or-not here is decided once, up front,
// by NewStyle's own NO_COLOR/TERM=dumb/TTY logic — never by
// lipgloss's own environment auto-detection, which would otherwise run
// again, independently, every time text is rendered and could disagree
// with the decision this package already made and tested. Enabled is
// always authoritative; renderer only supplies lipgloss's ANSI encoding
// once that decision is made.
type Style struct {
	Enabled bool
	// renderer is nil for a Style built as a struct literal (as the golden
	// tests do, deliberately, to drive both branches without touching a
	// real terminal); style() below builds one on demand in that case.
	renderer *lipgloss.Renderer
}

// NewStyle decides whether colour is enabled for the pair of streams a
// command writes to, following the policy: a TTY,
// not NO_COLOR, not TERM=dumb, and not forced off (--no-color or an implied
// --json/--json-events run, which the caller folds into forceOff).
//
// The colour profile is fixed at ANSI (basic 16-colour) rather than
// lipgloss's default truecolor/256-colour auto-detection: debark's output
// has to stay legible over a serial console and in a pasted transcript, and
// four status colours never needed more than that.
func NewStyle(out *os.File, forceOff bool) Style {
	enabled := !forceOff && !NoColorEnv() && !DumbTerminal() && IsTerminal(out)
	// Test the concrete pointer, not the interface. fileOf returns a typed
	// nil *os.File whenever the command's output is not an *os.File - which
	// is every golden test, since cobra's SetOut takes a *bytes.Buffer - and
	// assigning that to an io.Writer yields a NON-nil interface holding a
	// nil pointer. Checking the interface for nil therefore never fired and
	// the io.Discard fallback below was unreachable, leaving lipgloss with a
	// nil *os.File to write through.
	w := io.Writer(io.Discard)
	if out != nil {
		w = out
	}
	return Style{Enabled: enabled, renderer: rendererFor(w, enabled)}
}

func rendererFor(w io.Writer, enabled bool) *lipgloss.Renderer {
	r := lipgloss.NewRenderer(w)
	if enabled {
		r.SetColorProfile(termenv.ANSI)
	} else {
		r.SetColorProfile(termenv.Ascii)
	}
	return r
}

func (s Style) style() lipgloss.Style {
	r := s.renderer
	if r == nil {
		r = rendererFor(io.Discard, s.Enabled)
	}
	return r.NewStyle()
}

func (s Style) wrap(build func(lipgloss.Style) lipgloss.Style, text string) string {
	if !s.Enabled || text == "" {
		return text
	}
	return build(s.style()).Render(text)
}

func (s Style) Bold(text string) string {
	return s.wrap(func(st lipgloss.Style) lipgloss.Style { return st.Bold(true) }, text)
}

func (s Style) Faint(text string) string {
	return s.wrap(func(st lipgloss.Style) lipgloss.Style { return st.Faint(true) }, text)
}

// Red renders text in ANSI colour 1. Red, Green, Yellow and Cyan below all
// use ANSI colour numbers (not hex/truecolor), matching the fixed ANSI
// profile NewStyle sets: 1=red, 2=green, 3=yellow, 6=cyan.
func (s Style) Red(text string) string {
	return s.wrap(func(st lipgloss.Style) lipgloss.Style { return st.Foreground(lipgloss.Color("1")) }, text)
}

func (s Style) Green(text string) string {
	return s.wrap(func(st lipgloss.Style) lipgloss.Style { return st.Foreground(lipgloss.Color("2")) }, text)
}

func (s Style) Yellow(text string) string {
	return s.wrap(func(st lipgloss.Style) lipgloss.Style { return st.Foreground(lipgloss.Color("3")) }, text)
}

func (s Style) Cyan(text string) string {
	return s.wrap(func(st lipgloss.Style) lipgloss.Style { return st.Foreground(lipgloss.Color("6")) }, text)
}

// OK and Fail render a short coloured status word, used at the start of a
// verify/install/doctor summary line.
func (s Style) OK(text string) string   { return s.Green(text) }
func (s Style) Fail(text string) string { return s.Red(text) }
func (s Style) Warn(text string) string { return s.Yellow(text) }
