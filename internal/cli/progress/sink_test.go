package progress

import (
	"strings"
	"testing"

	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/internal/cli/render"
)

func TestNonTTYPrintsOneLinePerCompletedUnit(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, false, render.Style{Enabled: false})

	evidence.Emit(s, evidence.TypeFetchFile, "fetched libfoo", map[string]any{
		"filename": "libfoo_1.0_amd64.deb",
		"bytes":    int64(2048),
	})
	evidence.Emit(s, evidence.TypeFetchFile, "fetched libbar", map[string]any{
		"filename": "libbar_2.0_amd64.deb",
		"bytes":    int64(4096),
	})
	s.Close()

	out := buf.String()
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("non-TTY output must never contain ANSI escapes: %q", out)
	}
	if strings.Contains(out, "\r") {
		t.Errorf("non-TTY output must never use carriage returns: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (one per completed unit):\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "libfoo_1.0_amd64.deb") || !strings.Contains(lines[0], "2.0 kB") {
		t.Errorf("line 1 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "libbar_2.0_amd64.deb") {
		t.Errorf("line 2 = %q", lines[1])
	}
}

func TestNonTTYIgnoresIrrelevantEventTypes(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, false, render.Style{Enabled: false})
	evidence.Emit(s, evidence.TypeAPTUpdate, "apt update", nil)
	evidence.Emit(s, evidence.TypeManifestSigned, "signed", nil)
	s.Close()
	if buf.Len() != 0 {
		t.Errorf("expected no output for ignored event types, got %q", buf.String())
	}
}

func TestTTYModeRedrawsSingleLineAndEndsWithNewline(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, true, render.Style{Enabled: false})

	evidence.Emit(s, evidence.TypeFetchFile, "", map[string]any{
		"filename": "a.deb", "bytes": int64(1000),
	})
	evidence.Emit(s, evidence.TypeFetchFile, "", map[string]any{
		"filename": "b.deb", "bytes": int64(1000),
	})
	s.Close()

	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Errorf("TTY mode should redraw with carriage returns: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Close should leave the cursor on a fresh line: %q", out)
	}
	// Only one trailing newline: the live line was redrawn in place, not
	// printed twice.
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one newline (from Close), got %d in %q", strings.Count(out, "\n"), out)
	}
}

func TestTTYModeNoANSIWhenStyleDisabled(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, true, render.Style{Enabled: false})
	evidence.Emit(s, evidence.TypeFetchFile, "", map[string]any{
		"filename": "a.deb", "error": "connection reset",
	})
	s.Close()
	if strings.ContainsRune(buf.String(), '\x1b') {
		t.Errorf("style disabled but output contains ANSI: %q", buf.String())
	}
}

func TestTTYModeUsesANSIWhenStyleEnabled(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, true, render.Style{Enabled: true})
	evidence.Emit(s, evidence.TypeFetchFile, "", map[string]any{
		"filename": "a.deb", "error": "connection reset",
	})
	s.Close()
	if !strings.ContainsRune(buf.String(), '\x1b') {
		t.Errorf("style enabled but output has no ANSI: %q", buf.String())
	}
}

func TestFetchFileCachedAndFailedLinesInNonTTY(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, false, render.Style{Enabled: false})
	evidence.Emit(s, evidence.TypeFetchFile, "", map[string]any{"filename": "cached.deb", "cached": true})
	evidence.Emit(s, evidence.TypeFetchFile, "", map[string]any{"filename": "bad.deb", "error": "404"})
	s.Close()
	out := buf.String()
	if !strings.Contains(out, "cached  cached.deb") {
		t.Errorf("missing cached line: %q", out)
	}
	if !strings.Contains(out, "failed  bad.deb: 404") {
		t.Errorf("missing failed line: %q", out)
	}
}

func TestProgressEventNonTTY(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, false, render.Style{Enabled: false})
	evidence.Emit(s, evidence.TypeProgress, "resolving dependencies", map[string]any{"label": "resolving"})
	s.Close()
	if !strings.Contains(buf.String(), "resolving\n") {
		t.Errorf("got %q", buf.String())
	}
}

func TestCloseIsIdempotentAndSafeWithoutOutput(t *testing.T) {
	var buf strings.Builder
	s := New(&buf, true, render.Style{Enabled: false})
	if err := s.Close(); err != nil {
		t.Fatalf("Close on an empty sink: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Close on a sink with no events should print nothing, got %q", buf.String())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
