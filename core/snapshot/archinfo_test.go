package snapshot

import (
	"reflect"
	"testing"
)

func TestParseArchLines(t *testing.T) {
	native, foreign := parseArchLines([]byte("amd64\n"))
	if native != "amd64" || len(foreign) != 0 {
		t.Errorf("got native=%q foreign=%v, want amd64/[]", native, foreign)
	}

	native, foreign = parseArchLines([]byte("amd64\ni386\narmhf\n"))
	if native != "amd64" || !reflect.DeepEqual(foreign, []string{"armhf", "i386"}) {
		t.Errorf("got native=%q foreign=%v, want amd64/[armhf i386] (sorted)", native, foreign)
	}

	native, foreign = parseArchLines([]byte("\n\namd64\n\n\n"))
	if native != "amd64" || len(foreign) != 0 {
		t.Errorf("blank lines not skipped: native=%q foreign=%v", native, foreign)
	}
}

func TestParseAPTVersion(t *testing.T) {
	cases := map[string]string{
		"apt 2.8.3 (amd64)": "2.8.3",
		"apt 2.6.1 (amd64)": "2.6.1",
		"apt 1.8.2.3":       "1.8.2.3",
		"":                  "",
		"not apt output":    "",
	}
	for in, want := range cases {
		if got := parseAPTVersion(in); got != want {
			t.Errorf("parseAPTVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDpkgVersion(t *testing.T) {
	cases := map[string]string{
		"Debian 'dpkg' package management program version 1.22.6 (amd64).": "1.22.6",
		"Debian 'dpkg' package management program version 1.21.22.":        "1.21.22",
		"":                         "",
		"garbage, no version here": "",
	}
	for in, want := range cases {
		if got := parseDpkgVersion(in); got != want {
			t.Errorf("parseDpkgVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
