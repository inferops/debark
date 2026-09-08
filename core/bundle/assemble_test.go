package bundle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
)

func selectionFor(t *testing.T, stagingDir, pkg, version, arch, reason string) resolve.Selection {
	t.Helper()
	filename := pkg + "_" + version + "_" + arch + ".deb"
	staged := writeStagedDeb(t, stagingDir, filename, pkg, version, arch)
	sum, size, err := digest.SHA256File(staged)
	if err != nil {
		t.Fatalf("hash staged deb: %v", err)
	}
	return resolve.Selection{
		Name: pkg, Arch: arch, Version: version,
		Filename: filename, Size: size, SHA256: sum,
		Reason:                reason,
		PublisherVerification: lock.VerifiedAPTSigned,
		StagedPath:            staged,
	}
}

func lockTemplate() *lock.Lock {
	return &lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      "2026-09-03T00:00:00Z",
		SnapshotDigest: "",
		RequestDigest:  "req-digest",
		Target:         lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Resolver: lock.Resolver{
			Backend: lock.BackendContainer, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates: "never-include", InstallRecommends: true,
		},
		ClosedWorld: lock.ClosedWorld{Result: lock.ClosedWorldSkipped},
	}
}

func baseAssembleInput(t *testing.T, dir string, selections []resolve.Selection) Input {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, ".store"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	var install []string
	for _, s := range selections {
		install = append(install, s.NameVersion())
	}
	return Input{
		Dir:   filepath.Join(dir, "bundle"),
		Plan:  &resolve.Plan{Selections: selections, Install: install},
		Store: st,
		Lock:  lockTemplate(),
		Release: repository.ReleaseFields{
			Origin: repository.DefaultOrigin, Label: repository.DefaultLabel, Suite: repository.DefaultSuite,
			Codename: "bookworm", Architectures: []string{"amd64"}, Components: []string{repository.DefaultComponent},
			Description: repository.DefaultDescription,
		},
		CreatedAt: "2026-09-03T00:00:00Z",
	}
}

func TestAssembleEndToEndNoSnapshot(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	selections := []resolve.Selection{
		selectionFor(t, staging, "jq", "1.6-2.1", "amd64", lock.ReasonRequested),
		selectionFor(t, staging, "tree", "2.1.0-1", "amd64", lock.ReasonRequested),
	}
	in := baseAssembleInput(t, root, selections)

	res, err := Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if res.Repo.PackageCount != 2 {
		t.Errorf("PackageCount = %d, want 2", res.Repo.PackageCount)
	}
	if res.Stats.Added != 2 || res.Stats.Unchanged != 0 {
		t.Errorf("Stats = %+v, want Added=2 Unchanged=0", res.Stats)
	}
	if len(res.Added) != 2 {
		t.Errorf("Added = %v, want 2 entries", res.Added)
	}

	for _, want := range []string{
		filepath.Join(in.Dir, "repo", "Packages"),
		filepath.Join(in.Dir, "repo", "Packages.gz"),
		filepath.Join(in.Dir, "repo", "Release"),
		filepath.Join(in.Dir, "lock.json"),
		filepath.Join(in.Dir, "debark.manifest.json"),
		filepath.Join(in.Dir, "README.txt"),
		filepath.Join(in.Dir, "last-run-added.txt"),
		filepath.Join(in.Dir, "last-run-removed.txt"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected file missing: %s (%v)", want, err)
		}
	}

	// snapshot.json is only written when a Snapshot was supplied.
	if _, err := os.Stat(filepath.Join(in.Dir, "snapshot.json")); !os.IsNotExist(err) {
		t.Errorf("snapshot.json written despite Input.Snapshot being nil (err=%v)", err)
	}

	l, err := lock.Load(in.Dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if len(l.Packages) != 2 {
		t.Fatalf("lock.Packages has %d entries, want 2", len(l.Packages))
	}
	if l.Packages[0].Name != "jq" || l.Packages[1].Name != "tree" {
		t.Errorf("lock.Packages not sorted by name: %v", []string{l.Packages[0].Name, l.Packages[1].Name})
	}
	if len(l.Install) != 2 {
		t.Errorf("lock.Install = %v, want 2 entries", l.Install)
	}

	m, _, err := manifest.Load(in.Dir)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	if m.Repository.PackageCount != 2 {
		t.Errorf("manifest Repository.PackageCount = %d, want 2", m.Repository.PackageCount)
	}
	if m.LockDigest == "" {
		t.Errorf("manifest LockDigest is empty")
	}
	foundReadme := false
	for _, f := range m.Files {
		if f.Path == "README.txt" {
			foundReadme = true
		}
	}
	if foundReadme {
		t.Errorf("README.txt is in the manifest's hashed file list; it is written after Build so it must not be")
	}

	readme, err := os.ReadFile(filepath.Join(in.Dir, "README.txt"))
	if err != nil {
		t.Fatalf("read README.txt: %v", err)
	}
	if !strings.Contains(string(readme), "NOT SIGNED") {
		t.Errorf("README.txt does not carry the unsigned warning (Assemble never signs)")
	}
}

func TestAssembleWithSnapshot(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	selections := []resolve.Selection{selectionFor(t, staging, "jq", "1.6-2.1", "amd64", lock.ReasonRequested)}
	in := baseAssembleInput(t, root, selections)
	in.Snapshot = &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-09-03T00:00:00Z",
		Tool:          snapshot.Tool{Name: "debark", Version: "test"},
		Target: snapshot.Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
			APTVersion: "2.6.1", DpkgVersion: "1.21.22",
		},
		DpkgStatus: snapshot.File{
			Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status",
			SHA256: digest.Bytes(nil),
		},
	}

	res, err := Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("Assemble with a snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "snapshot.json")); err != nil {
		t.Errorf("snapshot.json missing: %v", err)
	}
	if res.Manifest.SnapshotDigest == "" {
		t.Errorf("manifest SnapshotDigest is empty despite Input.Snapshot being set")
	}
}

func TestAssembleIncrementalRunLeavesUnchangedFileAlone(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	selections := []resolve.Selection{selectionFor(t, staging, "jq", "1.6-2.1", "amd64", lock.ReasonRequested)}
	in := baseAssembleInput(t, root, selections)

	res1, err := Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("first Assemble: %v", err)
	}
	if res1.Stats.Added != 1 {
		t.Fatalf("first run Added = %d, want 1", res1.Stats.Added)
	}

	res2, err := Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("second Assemble: %v", err)
	}
	if res2.Stats.Added != 0 || res2.Stats.Unchanged != 1 {
		t.Errorf("second run Stats = %+v, want Added=0 Unchanged=1", res2.Stats)
	}
	if len(res2.Added) != 0 {
		t.Errorf("second run Added = %v, want empty", res2.Added)
	}
	added, err := os.ReadFile(filepath.Join(in.Dir, "last-run-added.txt"))
	if err != nil {
		t.Fatalf("read last-run-added.txt: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("last-run-added.txt = %q on an unchanged run, want empty", added)
	}
}

func TestAssembleWithPruneRemovesSupersededVersion(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()

	inV1 := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "foo", "1.0", "amd64", lock.ReasonRequested)})
	if _, err := Assemble(context.Background(), inV1); err != nil {
		t.Fatalf("Assemble v1: %v", err)
	}
	oldPath := filepath.Join(inV1.Dir, "repo", repository.PoolPath("foo", "foo_1.0_amd64.deb"))
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("v1 pool file missing after first Assemble: %v", err)
	}

	inV2 := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "foo", "2.0", "amd64", lock.ReasonUpgrade)})
	inV2.Prune = true
	res2, err := Assemble(context.Background(), inV2)
	if err != nil {
		t.Fatalf("Assemble v2 with prune: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("v1 pool file still present after pruning: err=%v", err)
	}
	newPath := filepath.Join(inV2.Dir, "repo", repository.PoolPath("foo", "foo_2.0_amd64.deb"))
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("v2 pool file missing: %v", err)
	}
	if len(res2.Removed) != 1 {
		t.Fatalf("Removed = %v, want exactly one entry", res2.Removed)
	}
	removedTxt, err := os.ReadFile(filepath.Join(inV2.Dir, "last-run-removed.txt"))
	if err != nil {
		t.Fatalf("read last-run-removed.txt: %v", err)
	}
	if len(removedTxt) == 0 {
		t.Errorf("last-run-removed.txt is empty despite a prune having happened")
	}
}

func TestAssembleWithoutPruneKeepsSupersededVersion(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()

	inV1 := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "foo", "1.0", "amd64", lock.ReasonRequested)})
	if _, err := Assemble(context.Background(), inV1); err != nil {
		t.Fatalf("Assemble v1: %v", err)
	}
	inV2 := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "foo", "2.0", "amd64", lock.ReasonUpgrade)})
	// Prune left false (default, additive run).
	if _, err := Assemble(context.Background(), inV2); err != nil {
		t.Fatalf("Assemble v2 without prune: %v", err)
	}

	oldPath := filepath.Join(inV2.Dir, "repo", repository.PoolPath("foo", "foo_1.0_amd64.deb"))
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("v1 pool file removed despite Prune being false: %v", err)
	}
}

func TestAssembleValidatesRequiredFields(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	valid := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "jq", "1.0", "amd64", lock.ReasonRequested)})

	cases := []struct {
		name   string
		mutate func(Input) Input
	}{
		{"missing Dir", func(in Input) Input { in.Dir = ""; return in }},
		{"missing Plan", func(in Input) Input { in.Plan = nil; return in }},
		{"missing Store", func(in Input) Input { in.Store = nil; return in }},
		{"missing Lock", func(in Input) Input { in.Lock = nil; return in }},
		{"missing CreatedAt", func(in Input) Input { in.CreatedAt = ""; return in }},
		{"malformed CreatedAt", func(in Input) Input { in.CreatedAt = "not a timestamp"; return in }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.mutate(valid)
			in.Dir = filepath.Join(root, "case-"+tc.name) // keep each case's bundle dir distinct
			if tc.name == "missing Dir" {
				in.Dir = ""
			}
			if _, err := Assemble(context.Background(), in); err == nil {
				t.Errorf("Assemble did not reject %s", tc.name)
			}
		})
	}
}

func TestAssembleRejectsSelectionWithNeitherDigestNorStagedPath(t *testing.T) {
	root := t.TempDir()
	in := baseAssembleInput(t, root, []resolve.Selection{
		{Name: "ghost", Arch: "amd64", Version: "1.0", Filename: "ghost_1.0_amd64.deb"},
	})
	if _, err := Assemble(context.Background(), in); err == nil {
		t.Fatalf("Assemble accepted a selection with no digest and no staged path")
	}
}

// TestAssemblePruneKeepsTheVersionTheLockNames is the product-level guarantee
// behind PoolEntry.LockReferenced: whatever pruning decides, every file
// lock.json names must still be in the repo when Assemble returns.
//
// The scenario is a downgrade, which a target pin can legitimately force
// (docs/experiments/E3-pin-fidelity.md): an earlier additive run left foo 2.0
// in the pool, and this run's plan selects foo 1.0. Because prune runs after
// materialisation, 1.0 is on disk and visible to the pure decision, where a
// version-only rule would rank it below the 2.0 leftover and delete it - the
// one file the bundle exists to ship.
//
// The assertion is driven off the lock Assemble actually wrote rather than a
// hardcoded path, so it cannot pass vacuously if the plan or the pool layout
// changes.
func TestAssemblePruneKeepsTheVersionTheLockNames(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()

	inHigh := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "foo", "2.0", "amd64", lock.ReasonRequested)})
	if _, err := Assemble(context.Background(), inHigh); err != nil {
		t.Fatalf("Assemble 2.0: %v", err)
	}

	inPinned := baseAssembleInput(t, root, []resolve.Selection{selectionFor(t, staging, "foo", "1.0", "amd64", lock.ReasonRequested)})
	inPinned.Prune = true
	if _, err := Assemble(context.Background(), inPinned); err != nil {
		t.Fatalf("Assemble pinned 1.0 with prune: %v", err)
	}

	l, err := lock.Load(inPinned.Dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if len(l.Packages) == 0 {
		t.Fatal("lock names no packages; the test would assert nothing")
	}
	for _, p := range l.Packages {
		if _, err := os.Stat(filepath.Join(inPinned.Dir, "repo", p.Filename)); err != nil {
			t.Errorf("lock names %s %s at %s but the file is not in the repo: %v",
				p.Name, p.Version, p.Filename, err)
		}
	}
	// And the lock really is the pinned lower version, not a re-resolution.
	if got := l.Packages[0].Version; got != "1.0" {
		t.Errorf("lock records foo %s, want the plan's pinned 1.0", got)
	}

	// Documented consequence of protecting the lock-referenced entry: the
	// higher leftover is the group's numeric best, so it is not removed
	// either and stays in the pool as dead weight. It is covered by the
	// manifest (Build walks the whole bundle dir) but deliberately absent
	// from Packages, which indexes only this run's selections - so the
	// target's apt cannot see, let alone select, the version the pin
	// rejected. That is the property that actually matters.
	if _, err := os.Stat(filepath.Join(inPinned.Dir, "repo", repository.PoolPath("foo", "foo_2.0_amd64.deb"))); err != nil {
		t.Errorf("higher leftover unexpectedly removed: %v", err)
	}
	pkgs, err := os.ReadFile(filepath.Join(inPinned.Dir, "repo", "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	if !strings.Contains(string(pkgs), "Version: 1.0") {
		t.Error("Packages does not index the locked version 1.0")
	}
	if strings.Contains(string(pkgs), "Version: 2.0") {
		t.Error("Packages indexes the pin-rejected version 2.0; apt could select it")
	}
}

// ---------------------------------------------------------------------------
// A bundle may only ship bytes that match the digest apt vouched for.
//
// The store is content-addressed by file name: <root>/sha256/<hh>/<digest> is
// an ordinary file, and anything able to write one can park a .deb of its own
// behind a digest the plan already trusts. Nothing downstream would notice on
// its own, because the pool file, the Packages index and the manifest the
// operator signs are all re-derived from whatever was materialised - so the
// bundle comes out internally consistent about the attacker's package and
// verifies clean on the target, which then runs its maintainer scripts as
// root. These tests pin the two places that has to be caught.
// ---------------------------------------------------------------------------

// poisonStoreObject overwrites the object filed under dg with a different but
// still structurally valid .deb, and returns those bytes. Structurally valid
// matters: a corrupt archive would be caught by the repository writer parsing
// control data, which is not the property under test.
func poisonStoreObject(t *testing.T, st store.Store, dg string) []byte {
	t.Helper()
	bad := buildDebBytes(t, simpleControl("jq", "1.6-2.1", "amd64")+"Pwned: yes\n")
	if digest.Equal(digest.Bytes(bad), dg) {
		t.Fatalf("the planted .deb happens to hash to %s; this test would prove nothing", dg)
	}
	if err := os.WriteFile(st.Path(dg), bad, 0o644); err != nil {
		t.Fatalf("plant object: %v", err)
	}
	return bad
}

func TestAssembleRefusesAStoreObjectThatDoesNotMatchItsAddress(t *testing.T) {
	// With no staged copy to fall back on, the store object is all Assemble
	// has, and it must not be believed on the strength of being present.
	setUp := func(t *testing.T, root string) (Input, resolve.Selection) {
		t.Helper()
		staging := t.TempDir()
		sel := selectionFor(t, staging, "jq", "1.6-2.1", "amd64", lock.ReasonRequested)
		in := baseAssembleInput(t, root, []resolve.Selection{sel})
		if _, _, err := in.Store.PutFile(context.Background(), sel.StagedPath, false); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		return in, sel
	}

	t.Run("planted object", func(t *testing.T) {
		root := t.TempDir()
		in, sel := setUp(t, root)
		bad := poisonStoreObject(t, in.Store, sel.SHA256)
		sel.StagedPath = ""
		in.Plan = &resolve.Plan{Selections: []resolve.Selection{sel}, Install: in.Plan.Install}

		_, err := Assemble(context.Background(), in)
		if err == nil {
			t.Fatalf("Assemble built a bundle from an object whose content does not hash to its address")
		}
		if got := dferr.ClassOf(err); got != dferr.Verification {
			t.Errorf("error class = %v, want Verification: %v", got, err)
		}
		poolPath := filepath.Join(in.Dir, "repo", repository.PoolPath(sel.Name, sel.Filename))
		if b, rerr := os.ReadFile(poolPath); rerr == nil && bytes.Equal(b, bad) {
			t.Errorf("the planted .deb was written into the pool at %s", poolPath)
		}
	})

	t.Run("intact object", func(t *testing.T) {
		// Guards the case above against passing because Assemble refuses every
		// store-only selection: the identical build with the object left alone
		// must still succeed.
		root := t.TempDir()
		in, sel := setUp(t, root)
		sel.StagedPath = ""
		in.Plan = &resolve.Plan{Selections: []resolve.Selection{sel}, Install: in.Plan.Install}
		if _, err := Assemble(context.Background(), in); err != nil {
			t.Fatalf("Assemble from an intact store object: %v", err)
		}
	})
}

func TestAssembleShipsOnlyPoolBytesTheLockNames(t *testing.T) {
	// The same planting, but with the staged download still available: the
	// build is expected to succeed, and the point is what it ships. Every
	// digest in lock.json is re-checked against the file actually sitting at
	// that pool path, which is the guarantee the target ultimately relies on.
	root := t.TempDir()
	staging := t.TempDir()
	sel := selectionFor(t, staging, "jq", "1.6-2.1", "amd64", lock.ReasonRequested)
	genuine, err := os.ReadFile(sel.StagedPath)
	if err != nil {
		t.Fatalf("read staged deb: %v", err)
	}

	in := baseAssembleInput(t, root, []resolve.Selection{sel})
	if _, _, err := in.Store.PutFile(context.Background(), sel.StagedPath, false); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	bad := poisonStoreObject(t, in.Store, sel.SHA256)

	if _, err := Assemble(context.Background(), in); err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	l, err := lock.Load(in.Dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if len(l.Packages) == 0 {
		t.Fatal("lock names no packages; the test would assert nothing")
	}
	for _, p := range l.Packages {
		onDisk, _, herr := digest.SHA256File(filepath.Join(in.Dir, "repo", p.Filename))
		if herr != nil {
			t.Fatalf("hash pool file for %s: %v", p.Name, herr)
		}
		if !digest.Equal(onDisk, p.SHA256) {
			t.Errorf("lock names %s at %s with sha256 %s, but that file hashes to %s",
				p.Name, p.Filename, p.SHA256, onDisk)
		}
	}

	poolPath := filepath.Join(in.Dir, "repo", repository.PoolPath(sel.Name, sel.Filename))
	got, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	if bytes.Equal(got, bad) {
		t.Fatalf("the planted .deb reached the pool")
	}
	if !bytes.Equal(got, genuine) {
		t.Errorf("pool file is neither the planted nor the genuine .deb")
	}
	// And the store no longer holds the planted bytes for the next build to
	// pick up.
	if held, err := os.ReadFile(in.Store.Path(sel.SHA256)); err != nil {
		t.Errorf("read store object: %v", err)
	} else if !bytes.Equal(held, genuine) {
		t.Errorf("store still holds content that is not the genuine .deb at %s", sel.SHA256)
	}
}

func TestAssembleRefusesToEmbedTheBinaryUnderATraversingArch(t *testing.T) {
	// Target.Arch becomes part of a file name. It reaches Assemble from the
	// snapshot or from --arch (Options.ArchOverride), neither of which is
	// checked upstream, so anything that is not an architecture tuple is a
	// path fragment - and this write is mode 0755.
	arena := t.TempDir()
	staging := t.TempDir()
	binSrc := filepath.Join(arena, "debark")
	if err := os.WriteFile(binSrc, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	assembleWithArch := func(t *testing.T, dir, arch string) error {
		t.Helper()
		in := baseAssembleInput(t, dir, []resolve.Selection{
			selectionFor(t, staging, "jq", "1.6-2.1", "amd64", lock.ReasonRequested),
		})
		in.Lock.Target.Arch = arch
		in.EmbedBinary = binSrc
		_, err := Assemble(context.Background(), in)
		return err
	}

	// Nested deep enough that the traversal below lands inside this test's own
	// temp tree, where an escape is contained and still observable.
	nest := filepath.Join(arena, "deep", "a", "b", "c")
	const evil = "../../../../PWNED-ARCH"
	err := assembleWithArch(t, nest, evil)
	if err == nil {
		t.Fatalf("Assemble accepted %q as an architecture", evil)
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("error class = %v, want Usage: %v", got, err)
	}
	escaped := filepath.Join(nest, "bundle", BinDir, "debark-linux-"+evil)
	if _, err := os.Stat(escaped); err == nil {
		t.Errorf("the embedded binary was written to %s, outside the bundle", escaped)
	}

	// Guards against passing because Assemble refuses to embed anything.
	good := filepath.Join(arena, "ok")
	if err := assembleWithArch(t, good, "amd64"); err != nil {
		t.Fatalf("Assemble with a real architecture: %v", err)
	}
	if _, err := os.Stat(filepath.Join(good, "bundle", BinDir, "debark-linux-amd64")); err != nil {
		t.Errorf("binary not embedded for a real architecture: %v", err)
	}
}

func TestAssembleRejectsTwoSelectionsClaimingOnePoolPath(t *testing.T) {
	// The later selection would overwrite the earlier one, leaving the
	// earlier lock entry naming a digest that is on disk nowhere. lock.Validate
	// cannot see it - its uniqueness key is name/arch/version, which differ -
	// and neither can verify, because the manifest is built afterwards.
	root := t.TempDir()
	staging := t.TempDir()
	first := selectionFor(t, staging, "collide", "1.0", "amd64", lock.ReasonRequested)
	second := selectionFor(t, staging, "collide", "2.0", "amd64", lock.ReasonUpgrade)
	if digest.Equal(first.SHA256, second.SHA256) {
		t.Fatal("the two selections have identical content; the test would prove nothing")
	}
	second.Filename = first.Filename // both now resolve to one pool path

	in := baseAssembleInput(t, root, []resolve.Selection{first, second})
	if _, err := Assemble(context.Background(), in); err == nil {
		t.Fatalf("Assemble accepted two selections mapping to one pool path")
	} else if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("error class = %v, want Usage: %v", got, err)
	}

	// Guards against passing because Assemble refuses two selections of the
	// same package: under their own filenames both must build and both must be
	// in the pool.
	okIn := baseAssembleInput(t, filepath.Join(root, "ok"), []resolve.Selection{
		selectionFor(t, staging, "collide", "1.0", "amd64", lock.ReasonRequested),
		selectionFor(t, staging, "collide", "2.0", "amd64", lock.ReasonUpgrade),
	})
	res, err := Assemble(context.Background(), okIn)
	if err != nil {
		t.Fatalf("Assemble with distinct filenames: %v", err)
	}
	if len(res.Lock.Packages) != 2 {
		t.Fatalf("lock names %d packages, want 2", len(res.Lock.Packages))
	}
	for _, p := range res.Lock.Packages {
		if _, err := os.Stat(filepath.Join(okIn.Dir, "repo", p.Filename)); err != nil {
			t.Errorf("%s %s is named by the lock but missing from the pool: %v", p.Name, p.Version, err)
		}
	}
}
