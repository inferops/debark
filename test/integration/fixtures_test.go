// Package integration is the cross-package integration smoke test for
// debark: it drives real bytes through the whole build -> verify ->
// export/import -> install.Plan chain in one process, against the real
// implementations of core/snapshot, core/store, core/repository,
// core/bundle, core/lock, core/manifest, core/sign, core/verify,
// core/install, core/fetch, core/engine, core/doctor and core/policy.
//
// Two packages are still in flight and this package deliberately does not
// depend on either: core/apt (the resolver) and internal/cli. In their
// place, fakeAptBackend in this file plays apt.Backend, exactly the way
// core/engine's own fakes_test.go plays it for that package's unit tests
// (same shape: a canned *resolve.Plan built over real .deb files), and
// core/install's Runner.Plan is driven against a fake apt-get/dpkg process
// rather than a fake Go interface, because install.Deps has no interface
// seam for that — see main_test.go's doc comment for why and how.
//
// This file holds the fixtures every test in the package shares: the
// snapshot fixture tree, the fake apt.Backend, and the synthetic .deb
// package specs resolved through it.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

func boolPtr(b bool) *bool { return &b }

// ---- snapshot fixture ------------------------------------------------------

// dpkgStatusFixture is a small, realistic /var/lib/dpkg/status: enough
// stanzas for InstalledCount to be meaningful, including one
// "deinstall ok config-files" entry that must NOT be counted, mirroring
// core/snapshot's own testdata/debian12-list fixture.
const dpkgStatusFixture = `Package: base-files
Status: install ok installed
Priority: required
Section: admin
Installed-Size: 400
Maintainer: Santiago Vila <sanvila@debian.org>
Architecture: amd64
Multi-Arch: foreign
Version: 12.4
Description: Debian base system miscellaneous files
 This package contains the basic filesystem hierarchy of a Debian system.

Package: libc6
Status: install ok installed
Priority: required
Section: libs
Installed-Size: 12000
Maintainer: GNU Libc Maintainers <debian-glibc@lists.debian.org>
Architecture: amd64
Multi-Arch: same
Source: glibc
Version: 2.36-9+deb12u4
Description: GNU C Library: Shared libraries
 Contains the standard libraries used by nearly all programs.

Package: old-removed-pkg
Status: deinstall ok config-files
Priority: optional
Section: oldlibs
Installed-Size: 10
Maintainer: Nobody <nobody@debian.org>
Architecture: amd64
Version: 1.0-1
Description: a package removed but with configuration files remaining
 Must not be counted by InstalledCount: it is not "install ok installed".
`

const fixtureOSRelease = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION="12 (bookworm)"
VERSION_CODENAME=bookworm
ID=debian
`

// writeSnapshotFixtureRoot builds a minimal filesystem-root fixture that
// snapshot.Capture reads without shelling out to a real dpkg/apt-get (real
// captures require Linux; this suite must also pass on Windows and macOS,
// per the contract brief's testing rules) — modelled directly on
// core/snapshot's own testdata/debian12-list fixture and the fixture-file
// convention documented in core/snapshot/archinfo.go.
func writeSnapshotFixtureRoot(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"etc/os-release":       fixtureOSRelease,
		"etc/apt/apt-version":  "apt 2.6.1 (amd64)\n",
		"etc/dpkg/version":     "Debian 'dpkg' package management program version 1.21.22 (amd64).\n",
		"etc/machine-id":       "0123456789abcdef0123456789abcdef\n",
		"var/lib/dpkg/arch":    "amd64\n",
		"var/lib/dpkg/status":  dpkgStatusFixture,
		"etc/apt/sources.list": "deb http://deb.debian.org/debian bookworm main\ndeb http://deb.debian.org/debian bookworm-updates main\ndeb http://deb.debian.org/debian-security bookworm-security main\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// buildFixtureSnapshotArchive builds the fixture root, captures it with the
// real snapshot.Capture, writes it with the real snapshot.WriteArchive, and
// reads it back with the real snapshot.Open — asserting that the digest is
// stable across that round trip (task requirement 1) before handing back
// the archive path for engine.Build's SnapshotRef.
func buildFixtureSnapshotArchive(t *testing.T, dir string) (archivePath, digest string) {
	t.Helper()
	root := filepath.Join(dir, "target-root")
	writeSnapshotFixtureRoot(t, root)

	s, fs, err := snapshot.Capture(context.Background(), snapshot.CaptureOptions{
		Root:            root,
		IncludeKeyrings: false, // no trusted.gpg.d in this fixture; keyrings are orthogonal to the pipeline this test exercises
	})
	if err != nil {
		t.Fatalf("snapshot.Capture: %v", err)
	}
	if len(s.Warnings) != 0 {
		t.Fatalf("snapshot.Capture produced unexpected warnings against a complete fixture: %v", s.Warnings)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatalf("snapshot.Validate: %v", err)
	}

	directDigest, err := snapshot.Digest(s)
	if err != nil {
		t.Fatalf("snapshot.Digest: %v", err)
	}

	archivePath = filepath.Join(dir, "snapshot.tar.zst")
	if err := snapshot.WriteArchive(archivePath, s, fs); err != nil {
		t.Fatalf("snapshot.WriteArchive: %v", err)
	}

	reopened, err := snapshot.Open(context.Background(), archivePath)
	if err != nil {
		t.Fatalf("snapshot.Open (round trip): %v", err)
	}
	defer reopened.Close()

	if reopened.Digest != directDigest {
		t.Fatalf("snapshot digest is not stable across the WriteArchive/Open round trip: captured=%s reopened=%s",
			directDigest, reopened.Digest)
	}
	if reopened.Snapshot.Target.DistroID != s.Target.DistroID || reopened.Snapshot.Target.Arch != s.Target.Arch {
		t.Fatalf("reopened snapshot target identity does not match the captured one: got %+v, want %+v",
			reopened.Snapshot.Target, s.Target)
	}

	return archivePath, reopened.Digest
}

// ---- fixture packages -------------------------------------------------------

// fixturePackageSpec is one synthetic package resolved through
// fakeAptBackend.
type fixturePackageSpec struct {
	Name, Version, Arch string
	Depends             string
	// Requested is true for the package the operator asked for by name
	// (goes into lock.Install); false for one pulled in only as a
	// dependency (stays in lock.Packages, never in lock.Install).
	Requested bool
	DependsOn string // set when !Requested: the Name of the requesting package
}

func (s fixturePackageSpec) filename() string {
	return fmt.Sprintf("%s_%s_%s.deb", s.Name, s.Version, s.Arch)
}

// fixturePlanSpecs is the fixed pair of packages every test in this package
// resolves. Deliberately one plain name and one "lib"-prefixed name, so
// repository.PoolPath's two prefix rules (first letter; special 4-letter
// "libX" form Debian uses for lib* packages) are both exercised by the real
// pipeline, not just repository's own unit tests.
func fixturePlanSpecs() []fixturePackageSpec {
	return []fixturePackageSpec{
		{Name: "aaa-base", Version: "1.0-1", Arch: "amd64", Depends: "libfoo1 (>= 2.3)", Requested: true},
		{Name: "libfoo1", Version: "2.3-4", Arch: "amd64", Requested: false, DependsOn: "aaa-base"},
	}
}

// installSimOutput renders the "apt-get -s install" simulation text a real
// apt would print for exactly the Requested packages in specs — the same
// set that becomes lock.Install — so the fake apt-get used to drive
// install.Plan (see main_test.go) echoes back a self-consistent simulation
// derived from the same fixture data, not an independently invented one.
func installSimOutput(specs []fixturePackageSpec) string {
	var b strings.Builder
	for _, spec := range specs {
		if !spec.Requested {
			continue
		}
		fmt.Fprintf(&b, "Inst %s (%s debark:bundle [%s])\n", spec.Name, spec.Version, spec.Arch)
		fmt.Fprintf(&b, "Conf %s (%s debark:bundle [%s])\n", spec.Name, spec.Version, spec.Arch)
	}
	return b.String()
}

// ---- fake apt.Backend --------------------------------------------------------

// fakeAptBackend plays core/apt.Backend (still in flight — see
// the package doc comment). Resolve builds a FRESH *resolve.Plan, over
// FRESH staged .deb files, on every call: core/engine's own ingestPlan
// moves a selection's StagedPath into the content store
// (store.PutFile(..., moveOK=true)), so a Backend whose Resolve runs more
// than once in this process (TestDeterminism calls it twice) must hand back
// new files each time, exactly as a real backend staging into a fresh
// ArchivesDir on every run would. Returning one fixed *resolve.Plan value
// from every call — which is what would happen if this simply mirrored
// core/engine/fakes_test.go's single-plan fixturePlan() helper verbatim —
// would make the second of two Resolve calls in the same process fail: its
// StagedPath would already have been consumed (moved into the store, then
// removed) by the first.
type fakeAptBackend struct {
	specs []fixturePackageSpec
}

func newFakeAptBackend() *fakeAptBackend {
	return &fakeAptBackend{specs: fixturePlanSpecs()}
}

func (f *fakeAptBackend) Kind() lock.Backend { return lock.BackendLocal }

func (f *fakeAptBackend) Probe(ctx context.Context, target snapshot.Target) (apt.Capabilities, error) {
	return apt.Capabilities{
		Available: true, DistroID: target.DistroID, VersionID: target.VersionID,
		APTVersion: "2.6.1", DpkgVersion: "1.21.22",
	}, nil
}

func (f *fakeAptBackend) Resolve(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
	if in.ArchivesDir == "" {
		return nil, dferr.New(dferr.Usage, "fakeAptBackend: ResolveInput.ArchivesDir is empty")
	}
	if err := os.MkdirAll(in.ArchivesDir, 0o755); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "fakeAptBackend: create archives dir")
	}

	var target lock.Target
	if in.Snapshot != nil {
		target = lock.Target{
			DistroID: in.Snapshot.Target.DistroID, VersionID: in.Snapshot.Target.VersionID,
			Codename: in.Snapshot.Target.Codename, Arch: in.Snapshot.Target.Arch,
		}
	}

	selections := make([]resolve.Selection, 0, len(f.specs))
	var install []string
	for _, spec := range f.specs {
		staged := filepath.Join(in.ArchivesDir, spec.filename())
		if err := fetch.WriteFixtureDeb(staged, fetch.FixtureDeb{
			Package: spec.Name, Version: spec.Version, Architecture: spec.Arch,
			Depends:     spec.Depends,
			Description: "debark integration test fixture package " + spec.Name,
		}); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "fakeAptBackend: build fixture .deb for %s", spec.Name)
		}
		// Selection.SHA256 is filled in, exactly as the real local backend
		// fills it from the Packages entry apt verified the download against
		// (core/apt/local.go's selectionsFor). It is not decoration: it is
		// the ONLY thing that makes "did the store already hold this file?"
		// answerable, in core/engine's ingestPlan (alreadyHeld) and in
		// core/bundle's materialiseSelections (alreadyInStore) alike — both
		// guard on sel.SHA256 != "".
		//
		// A fake that left it empty made every build, on the warmest store in
		// the world, report "not already held" — so a determinism test that
		// shares one store between two builds proved nothing at all about
		// cache-state dependence, which is precisely the defect that
		// dependence was supposed to expose. See TestDeterminism.
		sum, _, err := digest.SHA256File(staged)
		if err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "fakeAptBackend: hash fixture .deb for %s", spec.Name)
		}

		reason := lock.ReasonRequested
		if !spec.Requested {
			reason = resolve.DependencyOf(spec.DependsOn)
		}
		uri := fmt.Sprintf("http://deb.debian.org/debian/pool/main/%s/%s", spec.Name, spec.filename())

		selections = append(selections, resolve.Selection{
			Name: spec.Name, Arch: spec.Arch, Version: spec.Version, SourcePackage: spec.Name,
			Filename:              spec.filename(),
			SHA256:                sum,
			URI:                   uri,
			Origin:                lock.Origin{URI: uri, Suite: "bookworm", Component: "main"},
			Reason:                reason,
			PublisherVerification: lock.VerifiedAPTSigned,
			StagedPath:            staged,
		})

		if spec.Requested {
			install = append(install, fmt.Sprintf("%s:%s=%s", spec.Name, spec.Arch, spec.Version))
		}
	}
	sort.Strings(install)

	return &resolve.Plan{
		Target: target,
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates:     string(in.PhasedPolicy),
			InstallRecommends: in.Recommends,
		},
		Selections: selections,
		Install:    install,
	}, nil
}

// ClosedWorld should never be called by any test in this package: every
// build here explicitly disables the closed-world check
// (Options.ClosedWorldCheck = boolPtr(false)) because there is no real apt
// in this process to honestly answer "does this bundle install with the
// network gone" — see the "closed-world absence" assertion in
// pipeline_test.go and the corresponding finding in the implementation notes. If
// this method fires, a test forgot to disable the check and is about to let
// the engine fold a canned, dishonest answer into lock.json instead of
// recording that the check never really ran.
func (f *fakeAptBackend) ClosedWorld(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error) {
	return lock.ClosedWorld{
		Result: lock.ClosedWorldFailed,
		Detail: "fakeAptBackend.ClosedWorld: no real apt in this process; the test should have set Options.ClosedWorldCheck=false instead of relying on this canned answer",
	}, nil
}
