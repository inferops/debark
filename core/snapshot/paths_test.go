package snapshot

import (
	"path/filepath"
	"testing"
)

func TestToArchivePath(t *testing.T) {
	cases := map[string]string{
		"/etc/apt/sources.list": "etc/apt/sources.list",
		"/etc/os-release":       "etc/os-release",
		"etc/os-release":        "etc/os-release",
		"/":                     "",
	}
	for in, want := range cases {
		if got := toArchivePath(in); got != want {
			t.Errorf("toArchivePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeArchivePath(t *testing.T) {
	valid := []string{
		"etc/apt/sources.list",
		"usr/share/keyrings/debian-archive-keyring.gpg",
		"a",
		"a/b/c",
	}
	for _, p := range valid {
		if !safeArchivePath(p) {
			t.Errorf("safeArchivePath(%q) = false, want true", p)
		}
	}

	invalid := []string{
		"",
		".",
		"/etc/passwd",
		"../../../etc/passwd",
		"etc/../../../passwd",
		"etc/..",
		"..",
		`etc\apt\sources.list`,
		`..\..\evil`,
		"C:/Windows/System32",
		"C:evil",
		"etc//apt",
		"etc/./apt",
		"etc/",
		// Control characters. This value is both joined onto a real directory
		// and printed back to an operator, so a NUL truncates the name for
		// anything downstream that hands it to a C API, and ESC, CR/LF or a
		// C1 byte turn a "refusing %q" diagnostic into terminal commands --
		// which is the whole point of the doc comment calling this THE
		// security invariant.
		"etc/apt/sources.list\x00",
		"etc/apt\x00/sources.list",
		"etc/apt/\x1b[2Ksources.list",
		"etc/apt/sources\n.list",
		"etc/apt/sources\r.list",
		"etc/apt/\u009b2Ksources.list",
		"etc/apt/sources\x7f.list",
		"etc/apt/sources\a.list",
	}
	for _, p := range invalid {
		if safeArchivePath(p) {
			t.Errorf("safeArchivePath(%q) = true, want false", p)
		}
	}
}

func TestHostPath(t *testing.T) {
	got := hostPath("/some/root", "/etc/apt/sources.list")
	want := filepath.Join("/some/root", "etc", "apt", "sources.list")
	if got != want {
		t.Errorf("hostPath = %q, want %q", got, want)
	}
}
