package render

import (
	"os"
	"strings"
	"testing"
)

func TestStyleDisabledEmitsNoANSI(t *testing.T) {
	s := Style{Enabled: false}
	for _, got := range []string{
		s.Bold("x"), s.Faint("x"), s.Red("x"), s.Green("x"), s.Yellow("x"), s.Cyan("x"),
		s.OK("ok"), s.Fail("fail"), s.Warn("warn"),
	} {
		if strings.ContainsRune(got, '\x1b') {
			t.Errorf("disabled style emitted an ANSI escape: %q", got)
		}
	}
}

func TestStyleEnabledWrapsInANSI(t *testing.T) {
	s := Style{Enabled: true}
	got := s.Bold("x")
	if !strings.Contains(got, "\x1b[1m") || !strings.Contains(got, "\x1b[0m") {
		t.Errorf("Bold(%q) = %q, want ANSI bold wrapping", "x", got)
	}
	// Empty text stays empty even when enabled: no dangling reset code.
	if got := s.Bold(""); got != "" {
		t.Errorf("Bold(\"\") = %q, want \"\"", got)
	}
}

func TestNewStyleHonoursNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	// Even a TTY-classified file must come back disabled.
	s := NewStyle(os.Stdout, false)
	if s.Enabled {
		t.Error("NewStyle: NO_COLOR set but colour is enabled")
	}
}

func TestNewStyleHonoursDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	s := NewStyle(os.Stdout, false)
	if s.Enabled {
		t.Error("NewStyle: TERM=dumb but colour is enabled")
	}
}

func TestNewStyleForceOff(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	s := NewStyle(os.Stdout, true)
	if s.Enabled {
		t.Error("NewStyle: forceOff=true but colour is enabled")
	}
}

func TestNewStyleNonTTYDisablesColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := NewStyle(f, false)
	if s.Enabled {
		t.Error("NewStyle: plain file is not a TTY but colour is enabled")
	}
}

func TestIsTerminalFalseForRegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("IsTerminal(regular file) = true, want false")
	}
	if IsTerminal(nil) {
		t.Error("IsTerminal(nil) = true, want false")
	}
}

func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0:             "0 B",
		999:           "999 B",
		1000:          "1.0 kB",
		1500:          "1.5 kB",
		1_000_000:     "1.0 MB",
		38_200_000:    "38.2 MB",
		2_000_000_000: "2.0 GB",
	}
	for n, want := range cases {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := Plural(1, "package"); got != "1 package" {
		t.Errorf("Plural(1, package) = %q", got)
	}
	if got := Plural(0, "package"); got != "0 packages" {
		t.Errorf("Plural(0, package) = %q", got)
	}
	if got := Plural(2, "package"); got != "2 packages" {
		t.Errorf("Plural(2, package) = %q", got)
	}
}

func TestTableRender(t *testing.T) {
	tb := NewTable("NAME", "VERSION")
	tb.AddRow("vlc", "3.0.21")
	tb.AddRow("libfoo", "1.2.3-1")
	got := tb.String()
	for _, want := range []string{"NAME", "VERSION", "vlc", "3.0.21", "libfoo"} {
		if !strings.Contains(got, want) {
			t.Errorf("table output missing %q:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("table output should never contain ANSI escapes itself: %q", got)
	}
}

func TestTableAddRowPadsShortRow(t *testing.T) {
	tb := NewTable("A", "B", "C")
	tb.AddRow("x")
	if len(tb.Rows[0]) != 3 {
		t.Fatalf("row length = %d, want 3", len(tb.Rows[0]))
	}
	if tb.Rows[0][0] != "x" || tb.Rows[0][1] != "" || tb.Rows[0][2] != "" {
		t.Errorf("row = %#v", tb.Rows[0])
	}
}

// TestTableColumnsAreAlwaysSeparated pins the fix for a defect that shipped
// unnoticed: with every lipgloss border disabled, the column separator went
// with them, so the widest cell in a column -- and only that one, since every
// shorter cell was padded out to match it -- ran into the next column.
// `snapshot list-bases` was the first table to carry a cell long enough to
// set its own column's width, and produced "task-gnome-desktopDebian 12".
//
// The rows below are shaped to catch exactly that: the assertion fails if the
// gap ever comes from padding the other rows rather than from the cell style,
// because the offending cell is the longest one in its column.
func TestTableColumnsAreAlwaysSeparated(t *testing.T) {
	tb := NewTable("A", "B")
	tb.AddRow("short", "x")
	tb.AddRow("the-longest-cell-in-this-column", "y")
	got := tb.String()
	for _, joined := range []string{"the-longest-cell-in-this-columny", "shortx"} {
		if strings.Contains(got, joined) {
			t.Errorf("columns are not separated (%q appears):\n%s", joined, got)
		}
	}
	// The header row is padded by the same rule and must be separated too.
	if strings.Contains(got, "AB") {
		t.Errorf("header columns are not separated:\n%s", got)
	}
}
