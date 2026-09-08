package fetch

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseListFile fuzzes the packages.txt-style list reader with the real
// fixture already in the tree (testdata/packages.txt) as a seed, plus a
// handful of hand-picked edge cases for its documented syntax. The
// invariant is the function's own documented grammar: every returned URL
// input must be a parseable http(s) URL (ParseListFile's own doc: "a URL
// that does not parse as http(s)... is rejected"), and every digest it
// attaches must be well-formed 64-character lowercase hex — a parser that
// let a malformed digest or a non-http(s) "URL" through would silently
// widen what a caller downstream trusts as validated input.
func FuzzParseListFile(f *testing.F) {
	if seed, err := os.ReadFile(filepath.Join("testdata", "packages.txt")); err == nil {
		f.Add(seed)
	}
	f.Add([]byte("vlc\n"))
	f.Add([]byte("vlc=3.0.21-1build1\n"))
	f.Add([]byte("apt:vlc\n"))
	f.Add([]byte("https://host/path/x.deb\n"))
	f.Add([]byte("https://host/path/x.deb sha256=" + strings.Repeat("a", 64) + "\n"))
	f.Add([]byte("url:https://host/get?fmt=deb\n"))
	f.Add([]byte("./local-debs/zoom.deb\n"))
	f.Add([]byte("/abs/path/x.deb\n"))
	f.Add([]byte(`C:\Users\x\pkg.deb` + "\n"))
	f.Add([]byte("file:some/path.deb\n"))
	f.Add([]byte("# just a comment\n\n\nvlc # inline comment\n"))
	f.Add([]byte("https://host/x.deb sha256=not-hex\n"))
	f.Add([]byte("https://host/x.deb sha256=" + strings.Repeat("a", 64) + " extra-field\n"))
	f.Add([]byte("url:\n"))
	f.Add([]byte("ftp://host/x.deb\n"))
	f.Add([]byte("\x00\x01\xff not utf8 \xfe\n"))
	f.Add([]byte("vlc\r\nvlc2\r\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "packages.txt")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("writing fuzz input to a temp file: %v", err)
		}

		inputs, err := ParseListFile(path)
		if err != nil {
			return
		}
		for _, u := range inputs.URLs {
			parsed, perr := url.Parse(u.URL)
			if perr != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
				t.Fatalf("ParseListFile accepted a non-http(s) URL: %q", u.URL)
			}
			if u.SHA256 != "" {
				if len(u.SHA256) != 64 || strings.ToLower(u.SHA256) != u.SHA256 || strings.TrimLeft(u.SHA256, "0123456789abcdef") != "" {
					t.Fatalf("ParseListFile attached a malformed digest %q for URL %q", u.SHA256, u.URL)
				}
			}
		}
		for _, p := range inputs.Packages {
			if p == "" {
				t.Fatalf("ParseListFile returned an empty package name")
			}
		}
		for _, fl := range inputs.Files {
			if fl == "" {
				t.Fatalf("ParseListFile returned an empty file path")
			}
		}
	})
}
