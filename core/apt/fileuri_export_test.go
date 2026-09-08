package apt_test

// The one test in this package that is deliberately EXTERNAL (package
// apt_test, not package apt). Everything it asserts is already asserted by
// TestFileURI in hardening_test.go, from inside the package — except the one
// thing that is the whole point of this file: that FileURI is reachable by
// its exported name from another package at all.
//
// That is not a formality. FileURI was unexported, so the desktop GUI, which
// needs the identical escaping to hand an operator-chosen directory to the
// platform's file manager, landed a verbatim copy of the function instead of
// calling it. An in-package test cannot fail if a later change un-exports it,
// and the copy is exactly what re-appears when it does.

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/apt"
)

// TestFileURIIsCallableFromOutsideThePackage pins the exported name and the
// two properties that hold on every platform, over the inputs a file dialog
// actually produces. It asserts properties rather than literals for the
// reason bf51914 records: the literal answer is genuinely OS-dependent, and a
// literal assertion is how this function's own test came to fail on Linux
// while passing on Windows.
func TestFileURIIsCallableFromOutsideThePackage(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"a plain absolute path", "/tmp/plain"},
		{"a space, which would otherwise split a sources line into five fields", "/tmp/has space/repo"},
		{"a fragment marker, which would otherwise truncate the path at the #", "/tmp/release #2"},
		{"a query marker, which would otherwise truncate the path at the ?", "/tmp/what?now"},
		{"a percent, which would otherwise decode into a different directory", "/tmp/100%20done"},
		{"a tab, after which the result does not parse as a URL at all", "/tmp/a\tb"},
		{"a Windows drive-letter path, which must not put C: in the authority", `D:\projects\ext dir`},
		{"a UNC-shaped path", `\server\share\bundle`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apt.FileURI(tc.in)

			// Property 1: one whitespace-free field. This is what makes the
			// result usable as one field of a whitespace-separated apt
			// sources line, and what stops a file manager being handed two
			// arguments where one was meant.
			if strings.ContainsAny(got, " \t\n\r") {
				t.Errorf("FileURI(%q) = %q: contains whitespace", tc.in, got)
			}
			if fields := strings.Fields("deb [trusted=yes] " + got + " ./"); len(fields) != 4 {
				t.Errorf("FileURI(%q) = %q: a sources line built from it has %d fields, want 4", tc.in, got, len(fields))
			}

			// Property 2: it parses as a file: URL with an empty authority,
			// nothing has leaked into the query or fragment, and decoding
			// returns exactly the path that went in.
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("FileURI(%q) = %q: does not parse as a URL: %v", tc.in, got, err)
			}
			if u.Scheme != "file" {
				t.Errorf("FileURI(%q) = %q: scheme %q, want file", tc.in, got, u.Scheme)
			}
			if u.Host != "" {
				t.Errorf("FileURI(%q) = %q: authority %q — part of the path has leaked into the host", tc.in, got, u.Host)
			}
			if u.RawQuery != "" || u.Fragment != "" {
				t.Errorf("FileURI(%q) = %q: split into query %q and fragment %q — the path was truncated", tc.in, got, u.RawQuery, u.Fragment)
			}
			if want := "/" + strings.TrimPrefix(filepath.ToSlash(tc.in), "/"); u.Path != want {
				t.Errorf("FileURI(%q) = %q: decodes to %q, want %q", tc.in, got, u.Path, want)
			}
		})
	}
}

// TestFileURIBeatsTheConcatenationItReplaces drives the defect rather than
// describing it: for each input above, "file://" + filepath.ToSlash(abs) —
// the expression both this package and the GUI used before — is shown to
// produce a DIFFERENT and wrong answer. Without this, a future "simplify"
// that reverts to concatenation passes every other assertion in this file on
// the plain-path case and fails nothing.
func TestFileURIBeatsTheConcatenationItReplaces(t *testing.T) {
	naive := func(abs string) string { return "file://" + filepath.ToSlash(abs) }
	for _, in := range []string{
		"/tmp/has space/repo",
		"/tmp/release #2",
		"/tmp/what?now",
	} {
		if got, bad := apt.FileURI(in), naive(in); got == bad {
			t.Errorf("FileURI(%q) = %q, which is what plain concatenation produces; the escaping is gone", in, got)
		}
	}
	// The drive-letter half, which concatenation gets wrong in a different
	// way: two slashes put "D:" in the authority. Checked as a property so it
	// holds on both platforms — on Linux the backslashes are legal filename
	// characters and are percent-encoded, on Windows they become separators.
	drive := apt.FileURI(`D:\projects\ext dir`)
	if !strings.HasPrefix(drive, "file:///") {
		t.Errorf("FileURI(drive-letter path) = %q, want three slashes so the drive letter stays in the path (GOOS=%s)", drive, runtime.GOOS)
	}
}
