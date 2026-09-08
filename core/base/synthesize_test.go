package base

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// Only the pure parts of synthesize.go are covered here. Everything from
// Synthesize down needs a real apt (a private root, a resolving archive) or a
// container runtime, and a fake standing in for either could not express the
// failures those paths have — a fake that cannot fail the way the real thing
// fails makes the test vacuous, which this project has now hit nine times.
// Those paths belong to hack/linux-test.sh and the matrix.

// minimalSynthesizedSnapshot is the smallest document snapshot.Validate
// accepts with a synthesized origin. It is deliberately built here rather than
// by calling into this package's own builders, so the checks below are made
// against core/snapshot's rules and not against core/base's idea of them.
func minimalSynthesizedSnapshot() *snapshot.Snapshot {
	return &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-09-06T00:00:00Z",
		Target: snapshot.Target{
			DistroID: "ubuntu", VersionID: "26.04", Codename: "resolute", Arch: "amd64",
		},
		Origin: snapshot.Origin{
			Kind:   snapshot.OriginSynthesized,
			BaseID: "ubuntu:26.04/minimal",
		},
		DpkgStatus: snapshot.File{
			Path:        dpkgStatusPath,
			ArchivePath: "var/lib/dpkg/status",
			SHA256:      digest.Bytes([]byte("x")),
		},
	}
}

// TestArchivePathIsWhatCoreSnapshotAccepts. archivePath re-implements
// core/snapshot's target-path-to-member-name rule, because this package builds
// File records without going through Capture. Two copies of a rule drift, so
// the test does not restate the rule: it feeds the result to
// snapshot.Validate, which enforces safeArchivePath from the other side.
//
// The negative control at the bottom is the part that stops this being
// vacuous. An oracle that accepts everything proves nothing, so the same
// document is also handed a deliberately unsafe member name and must be
// refused — otherwise a broken archivePath would sail through unnoticed.
func TestArchivePathIsWhatCoreSnapshotAccepts(t *testing.T) {
	targets := []string{
		sourcesDir + "/ubuntu.sources",
		"/usr/share/keyrings/ubuntu-archive-keyring.gpg",
		dpkgStatusPath,
	}
	for _, target := range targets {
		ap := archivePath(target)
		if ap == "" {
			t.Errorf("archivePath(%q) = %q", target, ap)
			continue
		}
		if strings.HasPrefix(ap, "/") || strings.HasPrefix(ap, `\`) {
			t.Errorf("archivePath(%q) = %q, which still has a leading separator", target, ap)
		}

		s := minimalSynthesizedSnapshot()
		s.DpkgStatus = snapshot.File{
			Path:        target,
			ArchivePath: ap,
			Size:        1,
			SHA256:      digest.Bytes([]byte("x")),
			Mode:        "0644",
		}
		if err := snapshot.Validate(s); err != nil {
			t.Errorf("archivePath(%q) = %q, which core/snapshot refuses: %v", target, ap, err)
		}

		// Negative control: the same document with the leading separator put
		// back must be refused, or the assertion above means nothing.
		bad := minimalSynthesizedSnapshot()
		bad.DpkgStatus = snapshot.File{
			Path:        target,
			ArchivePath: "/" + ap,
			Size:        1,
			SHA256:      digest.Bytes([]byte("x")),
			Mode:        "0644",
		}
		if err := snapshot.Validate(bad); err == nil {
			t.Fatalf("snapshot.Validate accepted the absolute member name %q; this test has no oracle", "/"+ap)
		}
	}
}

// TestArchivePathNormalises covers the shapes that are not the happy path: a
// Windows separator (this package runs on the host, which may be Windows), a
// dot-dot that must be cleaned rather than carried, and a relative input.
// Each result goes back through snapshot.Validate for the same reason as
// above.
func TestArchivePathNormalises(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/var/lib/dpkg/status", "var/lib/dpkg/status"},
		{"var/lib/dpkg/status", "var/lib/dpkg/status"},
		{"/etc/apt/./sources.list.d/../sources.list", "etc/apt/sources.list"},
		{`\usr\share\keyrings\k.gpg`, "usr/share/keyrings/k.gpg"},
		{"/usr/share/keyrings//k.gpg", "usr/share/keyrings/k.gpg"},
		// Cleaning is rooted at "/", so no number of dot-dots escapes.
		{"/../../etc/passwd", "etc/passwd"},
		{"../../etc/passwd", "etc/passwd"},
	}
	for _, c := range cases {
		got := archivePath(c.in)
		if got != c.want {
			t.Errorf("archivePath(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		s := minimalSynthesizedSnapshot()
		s.DpkgStatus = snapshot.File{
			Path: "/" + got, ArchivePath: got, SHA256: digest.Bytes([]byte("x")),
		}
		if err := snapshot.Validate(s); err != nil {
			t.Errorf("archivePath(%q) = %q, which core/snapshot refuses: %v", c.in, got, err)
		}
	}
}

// TestOriginWithClosureRecordsTheResolvedBaseAndASortedSet.
//
// origin.assumed_installed is the only copy of the base's claim that reaches
// the target: a bundle carries snapshot.json but not the snapshot's files, so
// on the one machine where the assumption can finally be checked against a
// real dpkg status, this list is all there is. It has to be sorted (a lock and
// a bundle are byte-compared across builds), name:arch (install does a set
// membership test, not a parse) and version-free (a target running a different
// version of an assumed package still HAS it, and reporting that as divergence
// would bury the direction that matters).
func TestOriginWithClosureRecordsTheResolvedBaseAndASortedSet(t *testing.T) {
	r := Resolved{
		Definition: sampleDefinition(),
		Source:     "our-soe.yaml",
		Digest:     strings.Repeat("ab", 32),
	}
	// Deliberately not in sorted order, and with the same name under two
	// architectures, which is exactly what a multiarch closure looks like.
	packages := []apt.ClosurePackage{
		{Name: "zlib1g", Arch: "amd64", Version: "1:1.3.dfsg-3.1"},
		{Name: "apt", Arch: "amd64", Version: "2.9.8"},
		{Name: "zlib1g", Arch: "i386", Version: "1:1.3.dfsg-3.1"},
		{Name: "libc6", Arch: "amd64", Version: "2.40-1"},
	}

	o := originWithClosure(r, packages)

	if o.Kind != snapshot.OriginSynthesized {
		t.Errorf("Kind = %q, want %q", o.Kind, snapshot.OriginSynthesized)
	}
	if o.BaseID != r.Definition.ID {
		t.Errorf("BaseID = %q, want %q", o.BaseID, r.Definition.ID)
	}
	if o.Source != r.Source {
		t.Errorf("Source = %q, want %q", o.Source, r.Source)
	}
	if o.SourceDigest != r.Digest {
		t.Errorf("SourceDigest = %q, want %q", o.SourceDigest, r.Digest)
	}

	want := []string{"apt:amd64", "libc6:amd64", "zlib1g:amd64", "zlib1g:i386"}
	assertStrings(t, "AssumedInstalled", o.AssumedInstalled, want)
	if !sort.StringsAreSorted(o.AssumedInstalled) {
		t.Errorf("AssumedInstalled is not sorted: %q", o.AssumedInstalled)
	}
	seen := map[string]bool{}
	for _, n := range o.AssumedInstalled {
		if seen[n] {
			t.Errorf("AssumedInstalled contains %q twice", n)
		}
		seen[n] = true
		if strings.Count(n, ":") != 1 {
			t.Errorf("AssumedInstalled entry %q is not name:arch", n)
		}
	}
	for _, p := range packages {
		if strings.Contains(strings.Join(o.AssumedInstalled, " "), p.Version) {
			t.Errorf("AssumedInstalled carries the version %q; versions are deliberately left out", p.Version)
		}
	}

	// The order of the input must not change the output, or two builds of the
	// same base would produce different documents.
	shuffled := []apt.ClosurePackage{packages[3], packages[0], packages[2], packages[1]}
	if !reflect.DeepEqual(originWithClosure(r, shuffled), o) {
		t.Error("originWithClosure depends on the order apt happened to list the packages in")
	}

	// An empty closure still produces the origin block, and a document built
	// from it still validates.
	empty := originWithClosure(r, nil)
	if len(empty.AssumedInstalled) != 0 {
		t.Errorf("an empty closure produced %q", empty.AssumedInstalled)
	}
	s := minimalSynthesizedSnapshot()
	s.Origin = o
	if err := snapshot.Validate(s); err != nil {
		t.Errorf("a document carrying this origin block does not validate: %v", err)
	}
}

// TestSynthesisWarningsLeadWithTheSynthesizedSentence.
//
// The sentence is carried in the document rather than only printed once at
// synthesis time, because the snapshot is the thing that gets committed to
// git, copied between machines and read months later by someone who did not
// run the command. It has to come FIRST: the resolver's own warnings can be
// numerous, and a reader who stops after the first line must still learn that
// what they are holding is an assumption rather than a measurement.
func TestSynthesisWarningsLeadWithTheSynthesizedSentence(t *testing.T) {
	const lead = "this snapshot was synthesized from a base definition, not captured from a real machine"

	for _, c := range []struct {
		name    string
		closure *apt.Closure
		want    []string
	}{
		{"no resolver warnings", &apt.Closure{}, nil},
		{
			"resolver warnings follow, in order",
			&apt.Closure{Warnings: []lock.Warning{
				{Code: "phased-updates.never-include", Message: "no machine id, so phased updates are excluded"},
				{Code: "redistribution.multiverse", Message: "a selection comes from multiverse"},
			}},
			[]string{
				"phased-updates.never-include: no machine id, so phased updates are excluded",
				"redistribution.multiverse: a selection comes from multiverse",
			},
		},
	} {
		got := synthesisWarnings(c.closure)
		if len(got) == 0 {
			t.Errorf("%s: synthesisWarnings returned nothing", c.name)
			continue
		}
		if !strings.HasPrefix(got[0], lead) {
			t.Errorf("%s: the first warning is %q, which does not lead with %q", c.name, got[0], lead)
		}
		if !strings.Contains(got[0], "complete only if the target really is a stock install") {
			t.Errorf("%s: the leading warning does not say what the assumption is: %q", c.name, got[0])
		}
		assertStrings(t, c.name+": the warnings after the first", got[1:], c.want)
	}

	// The document these go into is held to core/snapshot's display-string
	// rules, so a warning that is too long or carries a control character
	// would make an otherwise good synthesis produce an unopenable snapshot.
	s := minimalSynthesizedSnapshot()
	s.Warnings = synthesisWarnings(&apt.Closure{Warnings: []lock.Warning{
		{Code: "redistribution.non-free", Message: "a selection comes from non-free"},
	}})
	if err := snapshot.Validate(s); err != nil {
		t.Errorf("a document carrying these warnings does not validate: %v", err)
	}
}

// TestTargetForInventsNothing. APTVersion and DpkgVersion are absent on
// purpose: nobody has run apt at this point, and a guess at them would be a
// fact about a machine invented by this function — which a later build's
// solver-divergence check would then compare against.
func TestTargetForInventsNothing(t *testing.T) {
	d := sampleDefinition()
	got := targetFor(d)
	want := snapshot.Target{
		DistroID: d.DistroID, VersionID: d.VersionID, Codename: d.Codename, Arch: d.Arch,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targetFor filled in more than the base knows:\n got %+v\nwant %+v", got, want)
	}
	if got.APTVersion != "" || got.DpkgVersion != "" {
		t.Errorf("targetFor invented apt/dpkg versions: %q %q", got.APTVersion, got.DpkgVersion)
	}
	if got.MachineID != "" {
		// ADR-006: a machine id is the most machine-specific fact there is,
		// and inventing one would make every build from this base reproduce a
		// phasing decision belonging to no machine at all.
		t.Errorf("targetFor invented a machine id: %q", got.MachineID)
	}
	if got.OSRelease != nil {
		t.Error("targetFor invented an os-release file")
	}
}

// TestFileRecordDigestsWhatItRecords: the File entries this package builds are
// verified byte for byte by snapshot.Open before Synthesize returns, so a
// record whose size or digest does not match its bytes would fail late and
// opaquely rather than here.
func TestFileRecordDigestsWhatItRecords(t *testing.T) {
	data := []byte("Types: deb\nURIs: http://example/\n")
	f := fileRecord(sourcesDir+"/ubuntu.sources", data)
	if f.Path != sourcesDir+"/ubuntu.sources" {
		t.Errorf("Path = %q", f.Path)
	}
	if f.ArchivePath != archivePath(f.Path) {
		t.Errorf("ArchivePath = %q, want %q", f.ArchivePath, archivePath(f.Path))
	}
	if f.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", f.Size, len(data))
	}
	if f.SHA256 != digest.Bytes(data) {
		t.Errorf("SHA256 = %q, want %q", f.SHA256, digest.Bytes(data))
	}
	if f.Mode != "0644" {
		t.Errorf("Mode = %q", f.Mode)
	}

	files := &snapshot.FileSet{Bytes: map[string][]byte{}}
	rec := record(dpkgStatusPath, data, files)
	if got, ok := files.Bytes[rec.ArchivePath]; !ok {
		t.Errorf("record did not stage the bytes under %q", rec.ArchivePath)
	} else if string(got) != string(data) {
		t.Errorf("record staged %q", got)
	}
}

// TestWriteFileSetRefusesToEscapeItsDirectory. Every member name has already
// been through archivePath by the time writeFileSet sees it, so this check is
// redundant on the happy path — which is precisely why it has to be tested.
// It is the "every extractor re-checks rather than trusting that Validate ran"
// rule, and a redundant check nobody exercises is a check that quietly stops
// working.
func TestWriteFileSetRefusesToEscapeItsDirectory(t *testing.T) {
	root := t.TempDir()

	good := filepath.Join(root, "good")
	files := &snapshot.FileSet{Bytes: map[string][]byte{}}
	sources := record(sourcesDir+"/ubuntu.sources", []byte("Types: deb\n"), files)
	status := record(dpkgStatusPath, []byte("Package: apt\n"), files)
	if err := writeFileSet(good, files); err != nil {
		t.Fatalf("writeFileSet: %v", err)
	}
	for _, f := range []snapshot.File{sources, status} {
		got, err := os.ReadFile(filepath.Join(good, filepath.FromSlash(f.ArchivePath)))
		if err != nil {
			t.Errorf("staged file %s: %v", f.ArchivePath, err)
			continue
		}
		if string(got) != string(files.Bytes[f.ArchivePath]) {
			t.Errorf("staged file %s has the wrong bytes: %q", f.ArchivePath, got)
		}
	}

	for _, name := range []string{
		"../escaped",
		"a/../../escaped",
		"/etc/passwd",
	} {
		dir := filepath.Join(root, "refused")
		err := writeFileSet(dir, &snapshot.FileSet{Bytes: map[string][]byte{name: []byte("x")}})
		if err == nil {
			t.Errorf("writeFileSet staged %q without complaint", name)
			continue
		}
		if got := dferr.ClassOf(err); got != dferr.Verification {
			t.Errorf("writeFileSet(%q) error class = %v, want %v", name, got, dferr.Verification)
		}
		if _, serr := os.Stat(filepath.Join(root, "escaped")); serr == nil {
			t.Fatalf("writeFileSet(%q) wrote outside its directory", name)
		}
	}
}
