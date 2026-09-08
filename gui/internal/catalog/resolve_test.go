package catalog

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/snapshot"
)

// TestResolveEveryBuiltinBase is the test this package exists to make pass.
//
// Before it, catalog.Target carried Sources that nothing produced, so
// Target.Validate failed on every real target and the whole catalogue path was
// unreachable. Every stock base debark ships must resolve to a Target that
// validates, carries real sources, and expands into index references the
// fetcher can actually use.
func TestResolveEveryBuiltinBase(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		defs, err := base.Builtin(arch)
		if err != nil {
			t.Fatalf("base.Builtin(%q): %v", arch, err)
		}
		if len(defs) == 0 {
			t.Fatalf("base.Builtin(%q) returned nothing", arch)
		}
		for _, def := range defs {
			t.Run(def.ID+"/"+arch, func(t *testing.T) {
				target, problems, err := ResolveBase(def)
				if err != nil {
					t.Fatalf("ResolveBase: %v (problems: %s)", err, srcTestDump(problems))
				}
				for _, p := range problems {
					if !p.Deliberate() {
						t.Errorf("a stock base produced a non-deliberate problem: %s", p)
					}
				}

				if err := target.Validate(); err != nil {
					t.Fatalf("the resolved target does not validate: %v", err)
				}
				if target.Kind != TargetBase {
					t.Errorf("kind = %q, want %q", target.Kind, TargetBase)
				}
				if target.BaseID != def.ID {
					t.Errorf("base id = %q, want %q", target.BaseID, def.ID)
				}
				if target.DistroID != def.DistroID || target.VersionID != def.VersionID ||
					target.Codename != def.Codename || target.Arch != arch {
					t.Errorf("identity mismatch: %+v", target)
				}
				if target.PrettyName == "" {
					t.Error("no pretty name for the title bar")
				}
				if len(target.Sources) == 0 {
					t.Fatal("resolved to zero apt sources, which is the blocker this package exists to remove")
				}

				refs := target.IndexRefs()
				if len(refs) == 0 {
					t.Fatal("IndexRefs is empty, so there is nothing for the catalogue to be built from")
				}
				sawPackages, sawDEP11 := false, false
				for _, r := range refs {
					if r.Arch != arch {
						t.Errorf("ref %s is for arch %q, want %q", r.ID(), r.Arch, arch)
					}
					if r.Suite == "" || r.Component == "" {
						t.Errorf("ref %s has no suite or component", r.ID())
					}
					u, err := url.Parse(r.URL())
					if err != nil || u.Scheme == "" || u.Host == "" {
						t.Errorf("ref %s does not produce a fetchable URL: %q", r.ID(), r.URL())
					}
					if !strings.HasPrefix(r.Path(), "dists/"+r.Suite+"/") {
						t.Errorf("ref %s has an unexpected path %q", r.ID(), r.Path())
					}
					switch r.Kind {
					case IndexPackages:
						sawPackages = true
					case IndexDEP11:
						sawDEP11 = true
					}
				}
				if !sawPackages || !sawDEP11 {
					t.Errorf("refs cover packages=%v dep11=%v; both families are expected", sawPackages, sawDEP11)
				}

				if len(target.Suites()) == 0 || len(target.Components()) == 0 {
					t.Errorf("suites=%v components=%v", target.Suites(), target.Components())
				}
				if key := target.CacheKey(); key == "" || strings.ContainsAny(key, `/\:`) {
					t.Errorf("cache key %q is not a safe path segment", key)
				}
			})
		}
	}
}

// TestResolveBuiltinBasesDifferByArch: a base's sources differ per
// architecture (Ubuntu serves arm64 from the ports archive), so the two must
// not collapse into one cache.
func TestResolveBuiltinBasesDifferByArch(t *testing.T) {
	amd64, _, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetBase, BaseID: "ubuntu:26.04/desktop", Arch: "amd64",
	})
	if err != nil {
		t.Fatalf("amd64: %v", err)
	}
	arm64, _, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetBase, BaseID: "ubuntu:26.04/desktop", Arch: "arm64",
	})
	if err != nil {
		t.Fatalf("arm64: %v", err)
	}
	if amd64.CacheKey() == arm64.CacheKey() {
		t.Errorf("both architectures share cache key %q", amd64.CacheKey())
	}
	if amd64.Sources[0].URIs[0] == arm64.Sources[0].URIs[0] {
		t.Errorf("both architectures point at %q; arm64 should use the ports archive", amd64.Sources[0].URIs[0])
	}
}

func TestResolveTargetBaseByID(t *testing.T) {
	target, problems, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64",
	})
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("unexpected problems:\n%s", srcTestDump(problems))
	}
	if target.Codename != "noble" {
		t.Errorf("codename = %q, want noble", target.Codename)
	}
	if got, want := target.Display(), "Ubuntu 24.04 desktop (amd64)"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}
	if len(target.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(target.Sources))
	}
	if len(target.IndexRefs()) == 0 {
		t.Fatal("no index refs")
	}
}

// TestResolveTargetBaseDefaultsArch: an operator who did not say which
// architecture means the one they are sitting at, which is what the CLI's
// --arch defaults to as well.
func TestResolveTargetBaseDefaultsArch(t *testing.T) {
	if _, ok := map[string]bool{"amd64": true, "arm64": true, "386": true, "arm": true}[runtime.GOARCH]; !ok {
		t.Skipf("no dpkg architecture is a sensible default on %s", runtime.GOARCH)
	}
	target, _, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetBase, BaseID: "debian:13/minimal",
	})
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if target.Arch != base.HostArch() {
		t.Errorf("arch = %q, want the host's %q", target.Arch, base.HostArch())
	}
}

func TestResolveTargetRejectsBadSelections(t *testing.T) {
	cases := []struct {
		name string
		sel  TargetSelection
		want string
	}{
		{"no kind", TargetSelection{}, "unknown target kind"},
		{"unknown kind", TargetSelection{Kind: TargetKind("machine")}, "unknown target kind"},
		{"base with no id", TargetSelection{Kind: TargetBase}, "no base was named"},
		{"snapshot with no path", TargetSelection{Kind: TargetSnapshot}, "no snapshot file was named"},
		{"base that does not exist", TargetSelection{Kind: TargetBase, BaseID: "ubuntu:99.04/desktop", Arch: "amd64"}, "resolving base"},
		{"snapshot that does not exist", TargetSelection{Kind: TargetSnapshot, SnapshotPath: filepath.Join(t.TempDir(), "absent.tar.zst")}, "opening snapshot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ResolveTarget(context.Background(), tc.sel)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestResolveTargetHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := ResolveTarget(ctx, TargetSelection{Kind: TargetBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); !errors.Is(err, context.Canceled) {
		t.Errorf("base: got %v, want context.Canceled", err)
	}

	path := resolveTestSnapshot(t, "amd64", map[string]string{
		"/etc/apt/sources.list.d/ubuntu.sources": string(srcTestRead(t, "ubuntu-noble.sources")),
	})
	if _, _, err := ResolveTarget(ctx, TargetSelection{Kind: TargetSnapshot, SnapshotPath: path}); !errors.Is(err, context.Canceled) {
		t.Errorf("snapshot: got %v, want context.Canceled", err)
	}
}

// TestResolveBaseRefusesASurvivingPlaceholder: base.Validate refuses a
// definition whose sources still carry "${", so a survivor means an upstream
// guarantee did not hold. Fetching from an archive named
// "http://archive/${codename}" would be a confusing failure much later.
func TestResolveBaseRefusesASurvivingPlaceholder(t *testing.T) {
	def := base.Definition{
		SchemaVersion: base.SchemaVersion,
		ID:            "example:1/minimal",
		DistroID:      "example",
		VersionID:     "1",
		Codename:      "one",
		Arch:          "amd64",
		Seeds:         []string{"example-minimal"},
		Sources: "Types: deb\nURIs: http://archive.example/${codename}\n" +
			"Suites: stable\nComponents: main\n",
	}
	_, problems, err := ResolveBase(def)
	if err == nil {
		t.Fatal("an unexpanded placeholder was accepted")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error %q does not say what went wrong", err)
	}
	found := false
	for _, p := range problems {
		if p.Kind == SourceProblemPlaceholder {
			found = true
		}
	}
	if !found {
		t.Errorf("the problems do not carry the placeholder: %s", srcTestDump(problems))
	}
}

// TestResolveBaseWithNoUsableSources: a base whose only source is a flat
// repository has nothing to browse, and the error says so rather than leaving
// the operator with Validate's bare complaint.
func TestResolveBaseWithNoUsableSources(t *testing.T) {
	def := base.Definition{
		SchemaVersion: base.SchemaVersion,
		ID:            "example:1/minimal",
		DistroID:      "example",
		VersionID:     "1",
		Codename:      "one",
		Arch:          "amd64",
		Seeds:         []string{"example-minimal"},
		Sources:       "Types: deb\nURIs: https://vendor.example/flat\nSuites: ./\n",
	}
	target, problems, err := ResolveBase(def)
	if err == nil {
		t.Fatal("a target with no usable sources was accepted")
	}
	if !strings.Contains(err.Error(), "flat") {
		t.Errorf("the error does not explain itself: %v", err)
	}
	if len(problems) == 0 {
		t.Error("no problems returned alongside the error")
	}
	// The identity fields are still returned so a screen can say which target
	// it failed on.
	if target.DistroID != "example" {
		t.Errorf("the partial target lost its identity: %+v", target)
	}
}

// ---------------------------------------------------------------------------
// snapshots
// ---------------------------------------------------------------------------

// resolveTestSnapshot writes a real debark.snapshot/v1 archive to a temp
// directory and returns its path.
//
// It is a real archive rather than a hand-laid directory: snapshot.Open
// validates the document, extracts the tar and digest-verifies every captured
// file, and testing the resolver against anything less would be testing a
// shape this application never actually receives.
func resolveTestSnapshot(t *testing.T, arch string, sources map[string]string) string {
	t.Helper()
	return resolveTestSnapshotOrigin(t, arch, sources, snapshot.Origin{Kind: snapshot.OriginCaptured})
}

func resolveTestSnapshotOrigin(t *testing.T, arch string, sources map[string]string, origin snapshot.Origin) string {
	t.Helper()

	files := &snapshot.FileSet{Bytes: map[string][]byte{}}
	record := func(targetPath string, data []byte) snapshot.File {
		archivePath := strings.TrimPrefix(filepath.ToSlash(targetPath), "/")
		files.Bytes[archivePath] = data
		return snapshot.File{
			Path:        targetPath,
			ArchivePath: archivePath,
			Size:        int64(len(data)),
			SHA256:      digest.Bytes(data),
			Mode:        "0644",
		}
	}

	doc := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-09-03T00:00:00Z",
		Tool:          snapshot.Tool{Name: "debark", Version: "0.0.0-test"},
		Target: snapshot.Target{
			DistroID:    "ubuntu",
			VersionID:   "24.04",
			Codename:    "noble",
			PrettyName:  "Ubuntu 24.04.3 LTS",
			Arch:        arch,
			APTVersion:  "2.8.3",
			DpkgVersion: "1.22.6ubuntu6.1",
		},
		DpkgStatus: record("/var/lib/dpkg/status", []byte("Package: hello\nStatus: install ok installed\n\n")),
		Origin:     origin,
	}

	// Sorted so the snapshot's source order is the fixture's, not the map's:
	// Target.Sources is documented as "in the order the target lists them".
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sortStringsForTest(names)
	for _, name := range names {
		doc.APT.Sources = append(doc.APT.Sources, record(name, []byte(sources[name])))
	}

	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := snapshot.WriteArchive(path, doc, files); err != nil {
		t.Fatalf("writing the fixture snapshot: %v", err)
	}
	return path
}

func sortStringsForTest(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestResolveSnapshot walks the whole snapshot path against a real archive
// carrying the mixture a real machine has: a deb822 file, a third-party
// one-line file, and a flat vendor repo that contributes nothing.
func TestResolveSnapshot(t *testing.T) {
	path := resolveTestSnapshot(t, "amd64", map[string]string{
		"/etc/apt/sources.list":                    string(srcTestRead(t, "ubuntu-noble-sources.list")),
		"/etc/apt/sources.list.d/ubuntu.sources":   string(srcTestRead(t, "ubuntu-noble.sources")),
		"/etc/apt/sources.list.d/tailscale.list":   string(srcTestRead(t, "tailscale.list")),
		"/etc/apt/sources.list.d/vendor-flat.list": "deb [trusted=yes] https://vendor.example/flat ./\n",
	})

	target, problems, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetSnapshot, SnapshotPath: path,
	})
	if err != nil {
		t.Fatalf("ResolveTarget: %v (problems: %s)", err, srcTestDump(problems))
	}

	if target.Kind != TargetSnapshot {
		t.Errorf("kind = %q", target.Kind)
	}
	if !filepath.IsAbs(target.SnapshotPath) {
		t.Errorf("snapshot path %q is not absolute", target.SnapshotPath)
	}
	if target.BaseID != "" {
		t.Errorf("a captured snapshot was measured, not assumed, so it carries no base id; got %q", target.BaseID)
	}
	if target.DistroID != "ubuntu" || target.VersionID != "24.04" || target.Codename != "noble" || target.Arch != "amd64" {
		t.Errorf("identity mismatch: %+v", target)
	}
	if got, want := target.Display(), "Ubuntu 24.04.3 LTS (amd64)"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}

	// Two stanzas from ubuntu.sources plus the tailscale line. The flat vendor
	// repo and the comment-only sources.list contribute nothing.
	if len(target.Sources) != 3 {
		t.Fatalf("got %d sources, want 3:\n%#v", len(target.Sources), target.Sources)
	}
	if len(problems) != 1 || problems[0].Kind != SourceProblemFlatRepo {
		t.Fatalf("got %v, want exactly one flat-repository problem:\n%s", srcTestKinds(problems), srcTestDump(problems))
	}
	if problems[0].File != "/etc/apt/sources.list.d/vendor-flat.list" {
		t.Errorf("the problem does not name the file it came from: %q", problems[0].File)
	}

	if err := target.Validate(); err != nil {
		t.Fatalf("the resolved target does not validate: %v", err)
	}
	refs := target.IndexRefs()
	if len(refs) == 0 {
		t.Fatal("no index refs")
	}
	wantRef := "http://archive.ubuntu.com/ubuntu dists/noble/main/binary-amd64/Packages.gz"
	found := false
	for _, r := range refs {
		if r.ID() == wantRef {
			found = true
		}
	}
	if !found {
		t.Errorf("refs do not include %q; got %d refs starting with %v", wantRef, len(refs), refs[0].ID())
	}
	if !containsSuiteForTest(target.Suites(), "noble-security") {
		t.Errorf("suites %v omit noble-security", target.Suites())
	}
}

func containsSuiteForTest(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestResolveSnapshotCarriesTheOriginBaseID.
//
// A snapshot is not automatically a measurement. `debark snapshot from-base`
// writes a *synthesized* one — the same document shape, built from a stock
// base definition, describing a machine nobody has looked at. If that reaches
// the operator labelled the same way a captured snapshot is, the caveat that
// exists to say "this may be short" never appears, and a short bundle is
// discovered on the offline side where it cannot be fixed.
//
// The frozen Target has no origin-kind field, so BaseID carries it: set for a
// synthesized snapshot, empty for a captured one. Both directions are asserted
// because either one silently flipping is the bug.
func TestResolveSnapshotCarriesTheOriginBaseID(t *testing.T) {
	sources := map[string]string{
		"/etc/apt/sources.list.d/ubuntu.sources": string(srcTestRead(t, "ubuntu-noble.sources")),
	}

	captured := resolveTestSnapshotOrigin(t, "amd64", sources, snapshot.Origin{Kind: snapshot.OriginCaptured})
	target, _, err := ResolveSnapshot(context.Background(), captured)
	if err != nil {
		t.Fatalf("captured: %v", err)
	}
	if target.BaseID != "" {
		t.Errorf("a captured snapshot claimed base id %q; emptiness is what the view layer reads as \"measured\"", target.BaseID)
	}

	synthesized := resolveTestSnapshotOrigin(t, "amd64", sources, snapshot.Origin{
		Kind:         snapshot.OriginSynthesized,
		BaseID:       "ubuntu:26.04/desktop",
		Source:       snapshot.OriginSourceBuiltin,
		SourceDigest: strings.Repeat("ab", 32),
	})
	target, _, err = ResolveSnapshot(context.Background(), synthesized)
	if err != nil {
		t.Fatalf("synthesized: %v", err)
	}
	if target.BaseID != "ubuntu:26.04/desktop" {
		t.Errorf("base id = %q, want the origin's %q: without it a synthesized snapshot is presented as a measurement",
			target.BaseID, "ubuntu:26.04/desktop")
	}
	if target.Kind != TargetSnapshot {
		t.Errorf("kind = %q, want %q: the base id does not change what this is", target.Kind, TargetSnapshot)
	}

	// The cache key must not move: Target.Identity excludes BaseID precisely
	// so that a base and a snapshot describing the same release with the same
	// sources share one catalogue on disk.
	plain, _, err := ResolveSnapshot(context.Background(), captured)
	if err != nil {
		t.Fatal(err)
	}
	if plain.CacheKey() != target.CacheKey() {
		t.Errorf("the origin base id moved the cache key: %q vs %q", plain.CacheKey(), target.CacheKey())
	}
}

// TestResolveSnapshotWithNoUsableSources: a snapshot whose apt sources are all
// deb-src fails, and the failure carries the explanation.
func TestResolveSnapshotWithNoUsableSources(t *testing.T) {
	path := resolveTestSnapshot(t, "amd64", map[string]string{
		"/etc/apt/sources.list": "deb-src http://deb.debian.org/debian trixie main\n",
	})
	_, problems, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetSnapshot, SnapshotPath: path,
	})
	if err == nil {
		t.Fatal("a snapshot with no usable sources was accepted")
	}
	if !strings.Contains(err.Error(), "deb-src") {
		t.Errorf("the error does not explain itself: %v", err)
	}
	if len(problems) != 1 || problems[0].Kind != SourceProblemSourceOnly {
		t.Errorf("got %v, want one source-only problem", srcTestKinds(problems))
	}
}

// TestResolveSnapshotHonoursForeignArchRestriction: an [arch=...] entry that
// does not name the snapshot's own architecture contributes nothing, and says
// so.
func TestResolveSnapshotHonoursArchRestriction(t *testing.T) {
	path := resolveTestSnapshot(t, "amd64", map[string]string{
		"/etc/apt/sources.list": "deb http://archive.ubuntu.com/ubuntu noble main\n" +
			"deb [arch=arm64] https://vendor.example/ports noble main\n",
	})
	target, problems, err := ResolveTarget(context.Background(), TargetSelection{
		Kind: TargetSnapshot, SnapshotPath: path,
	})
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if len(target.Sources) != 1 {
		t.Errorf("got %d sources, want 1", len(target.Sources))
	}
	if len(problems) != 1 || problems[0].Kind != SourceProblemOtherArch {
		t.Fatalf("got %v, want one other-architecture problem", srcTestKinds(problems))
	}
}

// TestResolveSnapshotClosesItsArchive. snapshot.Open extracts into a temporary
// directory; leaving one behind on every target the operator inspects is a
// slow leak nobody notices until a disk fills.
func TestResolveSnapshotClosesItsArchive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)

	path := resolveTestSnapshot(t, "amd64", map[string]string{
		"/etc/apt/sources.list.d/ubuntu.sources": string(srcTestRead(t, "ubuntu-noble.sources")),
	})
	// The archive itself lives under its own t.TempDir, not this one.
	if _, _, err := ResolveTarget(context.Background(), TargetSelection{Kind: TargetSnapshot, SnapshotPath: path}); err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("reading %s: %v", tmp, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "debark-snapshot-") {
			t.Errorf("the extracted archive %s was left behind", e.Name())
		}
	}

	// The same, on the failure path: a snapshot with no usable sources still
	// has to clean up after itself.
	bad := resolveTestSnapshot(t, "amd64", map[string]string{
		"/etc/apt/sources.list": "deb-src http://deb.debian.org/debian trixie main\n",
	})
	if _, _, err := ResolveTarget(context.Background(), TargetSelection{Kind: TargetSnapshot, SnapshotPath: bad}); err == nil {
		t.Fatal("expected an error")
	}
	entries, err = os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("reading %s: %v", tmp, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "debark-snapshot-") {
			t.Errorf("the extracted archive %s was left behind after a failure", e.Name())
		}
	}
}

// TestResolveReadCapturedRefusesEscapes. snapshot.Open validates archive paths
// already; this function joins one onto a directory and then opens the result,
// so it re-checks rather than inheriting the assumption.
func TestResolveReadCapturedRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "../../etc/passwd", "/etc/passwd"} {
		if _, err := resolveReadCaptured(dir, snapshot.File{ArchivePath: bad}); err == nil {
			t.Errorf("archive path %q was accepted", bad)
		}
	}
}

func TestResolveReadCapturedRefusesOversize(t *testing.T) {
	dir := t.TempDir()
	name := "etc/apt/sources.list"
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, SourceMaxDocumentBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveReadCaptured(dir, snapshot.File{ArchivePath: name})
	if err == nil {
		t.Fatal("an oversize sources file was read")
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// TestResolveBasePrettyName covers the label a base has no field for. It is
// display only: Target.Identity excludes PrettyName, so nothing here can move
// a cache key.
func TestResolveBasePrettyName(t *testing.T) {
	cases := []struct {
		def  base.Definition
		want string
	}{
		{base.Definition{DistroID: "ubuntu", VersionID: "26.04", Variant: "desktop"}, "Ubuntu 26.04 desktop"},
		{base.Definition{DistroID: "debian", VersionID: "13", Variant: "server"}, "Debian 13 server"},
		{base.Definition{DistroID: "linuxmint", VersionID: "22"}, "Linuxmint 22"},
		{base.Definition{VersionID: "1"}, "1"},
	}
	for _, tc := range cases {
		if got := resolveBasePrettyName(tc.def); got != tc.want {
			t.Errorf("resolveBasePrettyName(%+v) = %q, want %q", tc.def, got, tc.want)
		}
	}

	a, _, err := ResolveTarget(context.Background(), TargetSelection{Kind: TargetBase, BaseID: "ubuntu:26.04/desktop", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.PrettyName = "something else entirely"
	if a.CacheKey() != b.CacheKey() {
		t.Error("the pretty name changed the cache key; it is excluded from Identity for a reason")
	}
}

// TestResolveBaseSourcesPathMatchesSynthesize keeps a problem reported against
// a base naming the same file an operator would find if they synthesized the
// snapshot and looked inside it.
func TestResolveBaseSourcesPathMatchesSynthesize(t *testing.T) {
	if got, want := resolveBaseSourcesPath("ubuntu"), "/etc/apt/sources.list.d/ubuntu.sources"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.HasSuffix(resolveBaseSourcesPath(""), ".sources") {
		t.Error("the fallback name must still select the deb822 grammar")
	}

	def := base.Definition{
		SchemaVersion: base.SchemaVersion,
		ID:            "example:1/minimal",
		DistroID:      "example",
		VersionID:     "1",
		Codename:      "one",
		Arch:          "amd64",
		Sources:       "Types: deb-src\nURIs: https://example.invalid/x\nSuites: stable\nComponents: main\n",
	}
	_, problems, _ := ResolveBase(def)
	if len(problems) != 1 || problems[0].File != "/etc/apt/sources.list.d/example.sources" {
		t.Errorf("problems do not name the synthesized path: %s", srcTestDump(problems))
	}
}
