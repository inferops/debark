package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewPrivateRoot_OptionSet_MatchesPrototype pins the exact -o option set
// against install-offline.sh (lines ~147-159), which the brief calls
// "proven and measured; port it faithfully". Any change to this set should
// be a deliberate, reviewed decision, not a drive-by edit — that is the
// point of asserting the exact list rather than just "contains Dir::State".
func TestNewPrivateRoot_OptionSet_MatchesPrototype(t *testing.T) {
	repoDir := t.TempDir()
	pr, err := newPrivateRoot(repoDir, false)
	if err != nil {
		t.Fatalf("newPrivateRoot: %v", err)
	}
	defer pr.Close()

	args := pr.AptArgs(false)
	got := optionValues(t, args)

	want := map[string]string{
		"Dir::Etc::sourcelist":       "/dev/null",
		"Dir::Etc::sourceparts":      toSlash(filepath.Join(pr.dir, "sources.list.d")),
		"Dir::Etc::preferences":      "/dev/null",
		"Dir::Etc::preferencesparts": toSlash(filepath.Join(pr.dir, "preferences.d")),
		"Dir::State::lists":          toSlash(filepath.Join(pr.dir, "lists")),
		"Dir::Cache::pkgcache":       "",
		"Dir::Cache::srcpkgcache":    "",
		"Acquire::Languages":         "none",
		"APT::Sandbox::User":         "root",
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing -o %s (got options: %v)", k, got)
			continue
		}
		if gv != v {
			t.Errorf("-o %s = %q, want %q", k, gv, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d options, want exactly %d: %v", len(got), len(want), got)
	}
	if containsToken(args, "-y") {
		t.Errorf("AptArgs(false) must not include -y")
	}
}

func TestNewPrivateRoot_Yes(t *testing.T) {
	pr, err := newPrivateRoot(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if !containsToken(pr.AptArgs(true), "-y") {
		t.Errorf("AptArgs(true) must include -y")
	}
}

func TestNewPrivateRoot_Fast_AddsDpkgSpeedOptions(t *testing.T) {
	pr, err := newPrivateRoot(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	got := optionValues(t, pr.AptArgs(false))
	if got["Dpkg::Options::"] != "--force-unsafe-io" {
		t.Errorf("Dpkg::Options:: = %q, want --force-unsafe-io", got["Dpkg::Options::"])
	}
	if got["Dpkg::Use-Pty"] != "false" {
		t.Errorf("Dpkg::Use-Pty = %q, want false", got["Dpkg::Use-Pty"])
	}
}

func TestNewPrivateRoot_SourcesStanza(t *testing.T) {
	repoDir := t.TempDir()
	pr, err := newPrivateRoot(repoDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	data, err := os.ReadFile(pr.sourcesFile)
	if err != nil {
		t.Fatalf("read .sources: %v", err)
	}
	content := string(data)
	absRepo, _ := filepath.Abs(repoDir)
	wantURI := "URIs: file://" + toSlash(absRepo)

	for _, want := range []string{"Types: deb", wantURI, "Suites: ./", "Trusted: yes"} {
		if !strings.Contains(content, want) {
			t.Errorf(".sources = %q, missing %q", content, want)
		}
	}
}

func TestNewPrivateRoot_Close_RemovesTempDir(t *testing.T) {
	pr, err := newPrivateRoot(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	dir := pr.dir
	if err := pr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("private root %s still exists after Close", dir)
	}
}

func TestNewPrivateRoot_MissingRepoDir(t *testing.T) {
	_, err := newPrivateRoot(filepath.Join(t.TempDir(), "does-not-exist"), false)
	if err == nil {
		t.Fatalf("expected an error for a missing repository directory")
	}
}

func TestNewPrivateRoot_PersistSource(t *testing.T) {
	pr, err := newPrivateRoot(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	root := t.TempDir()
	dest, err := pr.PersistSource(root)
	if err != nil {
		t.Fatalf("PersistSource: %v", err)
	}
	want := filepath.Join(root, "etc", "apt", "sources.list.d", sourcesFileName)
	if dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("persisted file missing: %v", err)
	}
}

// optionValues parses a "-o Key=Value" argv slice into a map, asserting the
// -o/value pairing itself is well-formed (this is also, incidentally, a
// check that no option got mis-paired while building the slice).
func optionValues(t *testing.T, args []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] != "-o" {
			continue
		}
		if i+1 >= len(args) {
			t.Fatalf("-o with no following value in %v", args)
		}
		kv := args[i+1]
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("-o value %q is not KEY=VALUE", kv)
		}
		out[key] = val
		i++
	}
	return out
}
