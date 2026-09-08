package fetch

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// TestParseListFile_RealPrototypeFile parses the actual 55KB packages.txt
// shipped with the Bash prototype (copied verbatim into testdata/) and
// checks it against the exact counts and samples this pipeline produces:
//
//	tr -d '\r' < packages.txt | sed 's/#.*//' | awk 'NF{print $1}'
//
// which is the prototype's own list-reading pipeline (download-packages.sh).
func TestParseListFile_RealPrototypeFile(t *testing.T) {
	inputs, err := ParseListFile(filepath.Join("testdata", "packages.txt"))
	if err != nil {
		t.Fatalf("ParseListFile: %v", err)
	}

	if len(inputs.URLs) != 0 {
		t.Errorf("URLs = %d, want 0 (every URL example in this file is commented out)", len(inputs.URLs))
	}
	if len(inputs.Files) != 0 {
		t.Errorf("Files = %d, want 0 (every file example in this file is commented out)", len(inputs.Files))
	}
	const wantPackages = 1854 // verified against the prototype's own awk pipeline
	if len(inputs.Packages) != wantPackages {
		t.Errorf("Packages = %d, want %d", len(inputs.Packages), wantPackages)
	}

	wantFirst := []string{
		"coreutils", "util-linux", "bsdextrautils", "bsdmainutils", "debianutils",
		"findutils", "grep", "sed", "gawk", "diffutils",
	}
	if len(inputs.Packages) >= len(wantFirst) {
		for i, want := range wantFirst {
			if got := inputs.Packages[i]; got != want {
				t.Errorf("Packages[%d] = %q, want %q", i, got, want)
			}
		}
	}

	wantLast := []string{
		"apt-file", "apt-transport-https", "software-properties-gtk", "flatpak", "gnome-software-plugin-flatpak",
	}
	n := len(inputs.Packages)
	if n >= len(wantLast) {
		for i, want := range wantLast {
			got := inputs.Packages[n-len(wantLast)+i]
			if got != want {
				t.Errorf("Packages[%d] (near end) = %q, want %q", n-len(wantLast)+i, got, want)
			}
		}
	}

	if len(inputs.ListFiles) != 1 || inputs.ListFiles[0] != filepath.Join("testdata", "packages.txt") {
		t.Errorf("ListFiles = %v", inputs.ListFiles)
	}

	// Order is preserved, not sorted (buildjob.Inputs's doc: "the engine
	// sorts before hashing"). coreutils must come before util-linux, exactly
	// as it appears in the file, not alphabetically.
	if inputs.Packages[0] != "coreutils" || inputs.Packages[1] != "util-linux" {
		t.Errorf("first two packages out of order: %v", inputs.Packages[:2])
	}
}

func TestParseListFile_EdgeCases(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("comments and blanks", func(t *testing.T) {
		p := write("a.txt", "# a full-line comment\n\nvlc # inline trailing comment\n   \n# another\ntree\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		want := []string{"vlc", "tree"}
		if !equalStrings(in.Packages, want) {
			t.Errorf("Packages = %v, want %v", in.Packages, want)
		}
	})

	t.Run("pinned version", func(t *testing.T) {
		p := write("b.txt", "vlc=3.0.21-1build1\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if !equalStrings(in.Packages, []string{"vlc=3.0.21-1build1"}) {
			t.Errorf("Packages = %v", in.Packages)
		}
	})

	t.Run("explicit apt prefix", func(t *testing.T) {
		p := write("c.txt", "apt:vlc\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if !equalStrings(in.Packages, []string{"vlc"}) {
			t.Errorf("Packages = %v", in.Packages)
		}
	})

	t.Run("bare https URL", func(t *testing.T) {
		p := write("d.txt", "https://host.example/path/x.deb\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if len(in.URLs) != 1 || in.URLs[0].URL != "https://host.example/path/x.deb" || in.URLs[0].SHA256 != "" {
			t.Errorf("URLs = %+v", in.URLs)
		}
	})

	t.Run("url prefix for a URL not ending in .deb", func(t *testing.T) {
		p := write("e.txt", "url:https://host.example/get?fmt=deb\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if len(in.URLs) != 1 || in.URLs[0].URL != "https://host.example/get?fmt=deb" {
			t.Errorf("URLs = %+v", in.URLs)
		}
	})

	t.Run("URL with trailing sha256 digest", func(t *testing.T) {
		digest := strings.Repeat("ab", 32)
		p := write("f.txt", "https://host.example/x.deb sha256="+digest+"\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if len(in.URLs) != 1 || in.URLs[0].SHA256 != digest {
			t.Errorf("URLs = %+v", in.URLs)
		}
	})

	t.Run("URL with uppercase hex digest is lowercased", func(t *testing.T) {
		digest := strings.Repeat("AB", 32)
		p := write("f2.txt", "https://host.example/x.deb sha256="+digest+"\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if len(in.URLs) != 1 || in.URLs[0].SHA256 != strings.ToLower(digest) {
			t.Errorf("URLs = %+v", in.URLs)
		}
	})

	t.Run("relative local file resolves against list directory, not cwd", func(t *testing.T) {
		sub := filepath.Join(dir, "sublist")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(sub, "g.txt")
		if err := os.WriteFile(p, []byte("./local-debs/zoom.deb\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		want := filepath.Join(sub, "local-debs", "zoom.deb")
		if len(in.Files) != 1 || in.Files[0] != want {
			t.Errorf("Files = %v, want [%s]", in.Files, want)
		}
	})

	t.Run("file prefix explicit", func(t *testing.T) {
		p := write("h.txt", "file:some/path.deb\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		want := filepath.Join(dir, "some", "path.deb")
		if len(in.Files) != 1 || in.Files[0] != want {
			t.Errorf("Files = %v, want [%s]", in.Files, want)
		}
	})

	t.Run("bare .deb suffix with no prefix", func(t *testing.T) {
		p := write("i.txt", "zoom_amd64.deb\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		want := filepath.Join(dir, "zoom_amd64.deb")
		if len(in.Files) != 1 || in.Files[0] != want {
			t.Errorf("Files = %v, want [%s]", in.Files, want)
		}
	})

	t.Run("unix-absolute local path kept as-is", func(t *testing.T) {
		p := write("j.txt", "/abs/path/thing.deb\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if len(in.Files) != 1 || in.Files[0] != "/abs/path/thing.deb" {
			t.Errorf("Files = %v", in.Files)
		}
	})

	t.Run("windows-absolute local path kept as-is", func(t *testing.T) {
		p := write("k.txt", `C:\vendor\thing.deb`+"\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if len(in.Files) != 1 || in.Files[0] != `C:\vendor\thing.deb` {
			t.Errorf("Files = %v", in.Files)
		}
	})

	t.Run("windows CRLF line endings", func(t *testing.T) {
		p := write("l.txt", "vlc\r\ntree\r\n# comment\r\n\r\nhtop\r\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		want := []string{"vlc", "tree", "htop"}
		if !equalStrings(in.Packages, want) {
			t.Errorf("Packages = %v, want %v", in.Packages, want)
		}
	})

	t.Run("stray CR mid-file (old Mac line endings) collapses like the prototype's tr -d", func(t *testing.T) {
		// The prototype deletes every \r byte outright, not just ones before
		// \n. A lone \r (no following \n) therefore does not start a new
		// line: it just vanishes, gluing the surrounding text together.
		p := write("m.txt", "vlc\rtree\n")
		in, err := ParseListFile(p)
		if err != nil {
			t.Fatalf("ParseListFile: %v", err)
		}
		if !equalStrings(in.Packages, []string{"vlctree"}) {
			t.Errorf("Packages = %v, want [vlctree] (matches `tr -d %s`)", in.Packages, "'\\r'")
		}
	})

	// --- rejections: each of these must fail with dferr.Usage, naming the
	// file and line number, per the contract brief.

	t.Run("empty prefix payload is rejected", func(t *testing.T) {
		p := write("n.txt", "vlc\napt:\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 2)
	})

	t.Run("url prefix with nothing after it is rejected", func(t *testing.T) {
		p := write("o.txt", "url:\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("extra text after a package name is rejected", func(t *testing.T) {
		p := write("p.txt", "vlc extra-garbage\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("sha256 suffix on a non-URL line is rejected", func(t *testing.T) {
		digest := strings.Repeat("cd", 32)
		p := write("q.txt", "vlc sha256="+digest+"\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("malformed sha256 (wrong length) is rejected", func(t *testing.T) {
		p := write("r.txt", "https://host.example/x.deb sha256=deadbeef\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("malformed sha256 (non-hex) is rejected", func(t *testing.T) {
		bad := strings.Repeat("zz", 32)
		p := write("s.txt", "https://host.example/x.deb sha256="+bad+"\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("more than one extra field is rejected even on a URL line", func(t *testing.T) {
		digest := strings.Repeat("ab", 32)
		p := write("t.txt", "https://host.example/x.deb sha256="+digest+" extra\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("garbled URL is rejected", func(t *testing.T) {
		p := write("u.txt", "url:not-a-url\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("non-http(s) URL scheme is rejected", func(t *testing.T) {
		p := write("v.txt", "url:ftp://host.example/x.deb\n")
		_, err := ParseListFile(p)
		assertUsageErrorAtLine(t, err, p, 1)
	})

	t.Run("missing list file", func(t *testing.T) {
		_, err := ParseListFile(filepath.Join(dir, "does-not-exist.txt"))
		if dferr.ClassOf(err) != dferr.Usage {
			t.Fatalf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
		}
	})
}

func assertUsageErrorAtLine(t *testing.T, err error, path string, line int) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Fatalf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not name the file %q", msg, path)
	}
	lineStr := ":" + strconv.Itoa(line) + ":"
	if !strings.Contains(msg, lineStr) {
		t.Errorf("error %q does not name line %d (want substring %q)", msg, line, lineStr)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
