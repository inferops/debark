package apt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/snapshot"
)

// debian12Fixture returns a *snapshot.Snapshot plus its files directory,
// built from the real, checked-in testdata/real-targets/debian-12-state.tar.gz
// (captured from a debian:bookworm-slim container, apt 2.6.1, 2026-09-03 —
// see testdata/real-targets/README.md). It is real, messy input: a deb822
// .sources file with Signed-By, real trusted.gpg.d keyrings, a real 88-entry
// dpkg status.
func debian12Fixture(t *testing.T) (*snapshot.Snapshot, string) {
	t.Helper()
	tarPath := filepath.Join("..", "..", "testdata", "real-targets", "debian-12-state.tar.gz")
	if _, err := os.Stat(tarPath); err != nil {
		t.Skipf("fixture not found at %s: %v", tarPath, err)
	}
	extractDir := t.TempDir()
	stateDir := extractPrototypeState(t, tarPath, extractDir)
	filesDir := t.TempDir()
	snap := snapshotFromPrototypeState(t, stateDir, filesDir)
	return snap, filesDir
}

func buildDebian12Root(t *testing.T, rootDir string) (*PrivateRoot, *snapshot.Snapshot) {
	t.Helper()
	snap, filesDir := debian12Fixture(t)
	root, err := BuildPrivateRoot(RootSpec{
		Dir:                 rootDir,
		ArchivesDir:         filepath.Join(rootDir, "..", "archives"),
		Arch:                snap.Target.Arch,
		ForeignArchs:        snap.Target.ForeignArchs,
		Recommends:          true,
		PhasedPolicy:        snap.PhasedPolicyFor(),
		MachineID:           snap.Target.MachineID,
		Snapshot:            snap,
		SnapshotFilesDir:    filesDir,
		CopySnapshotSources: true,
	})
	if err != nil {
		t.Fatalf("BuildPrivateRoot: %v", err)
	}
	return root, snap
}

func TestBuildPrivateRoot_Debian12Fixture_FileTree(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "aptroot")
	root, _ := buildDebian12Root(t, rootDir)

	mustExist := []string{
		"etc/apt/sources.list",
		"etc/apt/sources.list.d/debian.sources",
		"etc/apt/preferences",
		"var/lib/dpkg/status",
		"var/lib/apt/lists",
		"var/cache/apt",
		"var/log/apt",
	}
	for _, rel := range mustExist {
		if _, err := os.Stat(filepath.Join(rootDir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected %s to exist: %v", rel, err)
		}
	}

	// The fixture's debian.sources declares Signed-By pointing at
	// /usr/share/keyrings/debian-archive-keyring.gpg, and the fixture also
	// captured that exact keyring under keyrings/. Both halves are present,
	// so this must be a rewrite, never the strip fallback.
	data, err := os.ReadFile(filepath.Join(rootDir, "etc", "apt", "sources.list.d", "debian.sources"))
	if err != nil {
		t.Fatalf("read rewritten debian.sources: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "/usr/share/keyrings/debian-archive-keyring.gpg") {
		t.Errorf("debian.sources still references the target's original keyring path:\n%s", content)
	}
	if !strings.Contains(content, "Signed-By: "+filepath.Join(rootDir, "etc", "apt", "trusted.gpg.d")) {
		t.Errorf("debian.sources was not rewritten to point inside trusted.gpg.d:\n%s", content)
	}

	// trusted.gpg.d must contain the rewritten-to keyring file, and it must
	// be non-empty (the real captured key material, not a stub).
	trustedEntries, err := os.ReadDir(filepath.Join(rootDir, "etc", "apt", "trusted.gpg.d"))
	if err != nil || len(trustedEntries) == 0 {
		t.Fatalf("trusted.gpg.d is empty or unreadable: %v", err)
	}
	foundKeyContent := false
	for _, e := range trustedEntries {
		fi, _ := e.Info()
		if fi != nil && fi.Size() > 0 {
			foundKeyContent = true
		}
	}
	if !foundKeyContent {
		t.Error("no non-empty keyring file found in trusted.gpg.d")
	}

	// dpkg status must be the real fixture's, not empty.
	statusData, err := os.ReadFile(filepath.Join(rootDir, "var", "lib", "dpkg", "status"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusData), "Package: adduser") {
		t.Error("dpkg status does not look like the real fixture (missing 'Package: adduser')")
	}

	if len(root.Warnings) > 0 {
		for _, w := range root.Warnings {
			if w.Code == "private-root.signed-by-stripped" {
				t.Errorf("unexpected strip-fallback warning for a keyring the fixture actually captured: %+v", w)
			}
		}
	}

	foundRewrite := false
	for _, rec := range root.SignedBy {
		if rec.RewrittenTo != "" {
			foundRewrite = true
			if len(rec.Fingerprints) == 0 {
				t.Log("note: rewritten Signed-By record carries no fingerprints (KeyringFingerprints join is best-effort and this fixture builds Snapshot.KeyringFingerprints as empty)")
			}
		}
	}
	if !foundRewrite {
		t.Errorf("expected at least one rewritten Signed-By record, got %+v", root.SignedBy)
	}
}

func TestBuildPrivateRoot_OptionList(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "aptroot")
	root, snap := buildDebian12Root(t, rootDir)

	for _, o := range root.Options {
		if strings.HasPrefix(o, "Dir::Etc::main=") || strings.HasPrefix(o, "Dir::Etc::parts=") {
			t.Errorf("regression: Dir::Etc::main/parts must never be passed as -o (E4: measured no-op), got %q", o)
		}
	}

	want := map[string]bool{
		"Dir::Etc::sourcelist=":                 false,
		"Dir::Etc::sourceparts=":                false,
		"Dir::Etc::preferences=":                false,
		"Dir::Etc::preferencesparts=":           false,
		"Dir::Etc::trustedparts=":               false,
		"Dir::State=":                           false,
		"Dir::State::status=":                   false,
		"Dir::Cache=":                           false,
		"Dir::Cache::archives=":                 false,
		"Dir::Log=":                             false,
		"APT::Architecture=" + snap.Target.Arch: false,
		"Acquire::GzipIndexes=false":            false,
		"APT::Sandbox::User=root":               false,
		"Debug::NoLocking=1":                    false,
		"APT::Install-Recommends=true":          false,
	}
	for _, o := range root.Options {
		for prefix := range want {
			if o == prefix || strings.HasPrefix(o, prefix) {
				want[prefix] = true
			}
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("expected an option starting with %q, options were: %v", k, root.Options)
		}
	}

	// No machine id in this fixture (snapshot-target.sh never captures one)
	// -> never-include, never APT::Machine-ID.
	for _, o := range root.Options {
		if strings.HasPrefix(o, "APT::Machine-ID=") {
			t.Errorf("expected no APT::Machine-ID (fixture has no machine id), got %q", o)
		}
	}
	found := false
	for _, o := range root.Options {
		if o == "APT::Get::Never-Include-Phased-Updates=true" {
			found = true
		}
	}
	if !found {
		t.Error("expected APT::Get::Never-Include-Phased-Updates=true")
	}

	// The APT_CONFIG loader is the actual mechanism (E4); it must exist and
	// carry both Dir::Etc keys the -o list deliberately omits.
	if root.AptConfigPath == "" {
		t.Fatal("AptConfigPath is empty")
	}
	loaderData, err := os.ReadFile(root.AptConfigPath)
	if err != nil {
		t.Fatalf("read AptConfigPath: %v", err)
	}
	// Parse with this package's own apt.conf tokenizer rather than a raw
	// substring match: the loader's values are quoted and backslash-escaped
	// (correctly — apt.conf uses C-style string quoting, and a Windows test
	// path itself contains backslashes), so comparing raw, unescaped values
	// is both more robust and platform-independent.
	toks := tokenizeAptConf(loaderData)
	pos := 0
	items := parseAptConfItems(toks, &pos)
	values := map[string]string{}
	for _, it := range items {
		values[it.Key] = it.Value
	}
	wantMain := filepath.Join(rootDir, "etc", "apt", "apt.conf")
	wantParts := filepath.Join(rootDir, "etc", "apt", "apt.conf.d")
	if values["Dir::Etc::main"] != wantMain {
		t.Errorf("Dir::Etc::main = %q, want %q (loader: %q)", values["Dir::Etc::main"], wantMain, string(loaderData))
	}
	if values["Dir::Etc::parts"] != wantParts {
		t.Errorf("Dir::Etc::parts = %q, want %q (loader: %q)", values["Dir::Etc::parts"], wantParts, string(loaderData))
	}
}

func TestBuildPrivateRoot_Deterministic(t *testing.T) {
	base := t.TempDir()
	root1, _ := buildDebian12Root(t, filepath.Join(base, "run1", "aptroot"))
	root2, _ := buildDebian12Root(t, filepath.Join(base, "run2", "aptroot"))

	norm := func(opts []string) []string {
		out := make([]string, len(opts))
		for i, o := range opts {
			o = strings.ReplaceAll(o, filepath.Join(base, "run1"), "<ROOT>")
			o = strings.ReplaceAll(o, filepath.Join(base, "run2"), "<ROOT>")
			out[i] = o
		}
		return out
	}
	o1, o2 := norm(root1.Options), norm(root2.Options)
	if len(o1) != len(o2) {
		t.Fatalf("option count differs: %d vs %d\n%v\n%v", len(o1), len(o2), o1, o2)
	}
	for i := range o1 {
		if o1[i] != o2[i] {
			t.Errorf("option %d differs: %q vs %q", i, o1[i], o2[i])
		}
	}
}

func TestBuildPrivateRoot_ApprovedKeysPolicy(t *testing.T) {
	snap, filesDir := debian12Fixture(t)

	// No approved keys configured at all: passes (policy is opt-in).
	rootDir := filepath.Join(t.TempDir(), "aptroot")
	_, err := BuildPrivateRoot(RootSpec{
		Dir: rootDir, ArchivesDir: filepath.Join(rootDir, "..", "archives"),
		Arch: snap.Target.Arch, PhasedPolicy: snapshot.PhasedNeverInclude,
		Snapshot: snap, SnapshotFilesDir: filesDir, CopySnapshotSources: true,
	})
	if err != nil {
		t.Fatalf("unexpected error with no approved-keys policy: %v", err)
	}

	// An approved-keys list that cannot possibly match anything in this
	// fixture must fail closed (exit-6 territory: dferr.Policy).
	rootDir2 := filepath.Join(t.TempDir(), "aptroot")
	_, err = BuildPrivateRoot(RootSpec{
		Dir: rootDir2, ArchivesDir: filepath.Join(rootDir2, "..", "archives"),
		Arch: snap.Target.Arch, PhasedPolicy: snapshot.PhasedNeverInclude,
		Snapshot: snap, SnapshotFilesDir: filesDir, CopySnapshotSources: true,
		ApprovedKeys: []string{"0000000000000000000000000000000000ABCD"},
	})
	if err == nil {
		t.Fatal("expected an error when no source can be traced to an approved key")
	}
}
