package bundle

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
)

// readLines reads one of the last-run-*.txt reports back as a slice, so a
// test asserts on what an operator would actually read out of the bundle
// rather than on an in-memory Result field the file is only supposed to
// mirror.
func readLines(t *testing.T, dir, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func warningWithCode(l *lock.Lock, code string) *lock.Warning {
	for i := range l.Warnings {
		if l.Warnings[i].Code == code {
			return &l.Warnings[i]
		}
	}
	return nil
}

func manifestCovers(m *manifest.Manifest, rel string) bool {
	for _, f := range m.Files {
		if f.Path == rel {
			return true
		}
	}
	return false
}

// TestAssembleReportsThePinRejectedLeftover is the reporting counterpart to
// TestAssemblePruneKeepsTheVersionTheLockNames, which pins the behaviour:
// when a target pin makes this run's plan select a LOWER version than a
// leftover already in the pool, prune keeps both, deliberately
// (docs/experiments/E3-pin-fidelity.md, "Prune resolution"). That is settled
// and this test does not question it.
//
// What it pins is that the survivor is no longer invisible. Before this
// report existed, the pin-rejected .deb was on the media, inside the
// manifest and therefore inside the operator's signature, and named by
// nothing at all: not repo/Packages (written from this run's selections),
// not last-run-removed.txt (it was not removed), not last-run-added.txt (it
// was not added this run). "What crossed the gap" had two different answers
// and an auditor reading either index got the smaller one.
func TestAssembleReportsThePinRejectedLeftover(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()

	// An earlier, ordinary additive run leaves foo 2.0 in the pool.
	inHigh := baseAssembleInput(t, root, []resolve.Selection{
		selectionFor(t, staging, "foo", "2.0", "amd64", lock.ReasonRequested),
	})
	if _, err := Assemble(context.Background(), inHigh); err != nil {
		t.Fatalf("Assemble 2.0: %v", err)
	}

	// This run's plan selects the pinned lower version, with prune on.
	inPinned := baseAssembleInput(t, root, []resolve.Selection{
		selectionFor(t, staging, "foo", "1.0", "amd64", lock.ReasonRequested),
	})
	inPinned.Prune = true
	res, err := Assemble(context.Background(), inPinned)
	if err != nil {
		t.Fatalf("Assemble pinned 1.0 with prune: %v", err)
	}

	leftover := repository.PoolPath("foo", "foo_2.0_amd64.deb")
	pinned := repository.PoolPath("foo", "foo_1.0_amd64.deb")

	// The premise: the leftover really is still on disk and really is absent
	// from every existing signal. Without these the rest proves nothing.
	if _, err := os.Stat(filepath.Join(inPinned.Dir, "repo", leftover)); err != nil {
		t.Fatalf("premise broken: the higher leftover is not in the pool: %v", err)
	}
	pkgs, err := os.ReadFile(filepath.Join(inPinned.Dir, "repo", "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	if strings.Contains(string(pkgs), "Version: 2.0") {
		t.Fatalf("premise broken: Packages indexes the pin-rejected 2.0")
	}
	if got := readLines(t, inPinned.Dir, RemovedFile); len(got) != 0 {
		t.Fatalf("premise broken: last-run-removed.txt = %v, want empty", got)
	}
	if got := readLines(t, inPinned.Dir, AddedFile); len(got) != 1 || got[0] != pinned {
		t.Fatalf("premise broken: last-run-added.txt = %v, want [%s]", got, pinned)
	}

	// The report itself: exactly the leftover, and NOT the file Packages does
	// index. A report that simply listed the pool would name both, so this
	// second half is what stops the assertion being satisfiable by a fake.
	got := readLines(t, inPinned.Dir, UnreferencedFile)
	want := []string{leftover}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("%s = %v, want %v", UnreferencedFile, got, want)
	}
	for _, line := range got {
		if line == pinned {
			t.Errorf("%s names %s, which repo/Packages does index", UnreferencedFile, pinned)
		}
	}
	if len(res.Unreferenced) != 1 || res.Unreferenced[0] != leftover {
		t.Errorf("Result.Unreferenced = %v, want %v", res.Unreferenced, want)
	}

	// And the signal an operator sees without going looking: lock.json, which
	// is also what README.txt's Warnings section and `debark inspect` are
	// rendered from.
	l, err := lock.Load(inPinned.Dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	w := warningWithCode(l, WarnUnreferenced)
	if w == nil {
		t.Fatalf("lock.json carries no %s warning; warnings=%+v", WarnUnreferenced, l.Warnings)
	}
	// Both versions, named: the one on the media and the one apt will get.
	for _, frag := range []string{leftover, "foo 1.0"} {
		if !strings.Contains(w.Message, frag) {
			t.Errorf("warning message does not name %q: %s", frag, w.Message)
		}
	}
	if len(w.Packages) != 1 || w.Packages[0] != "foo" {
		t.Errorf("warning Packages = %v, want [foo]", w.Packages)
	}
	readme, err := os.ReadFile(filepath.Join(inPinned.Dir, ReadmeFile))
	if err != nil {
		t.Fatalf("read README.txt: %v", err)
	}
	if !strings.Contains(string(readme), WarnUnreferenced) {
		t.Errorf("README.txt's Warnings section does not carry %s", WarnUnreferenced)
	}
}

// TestAssembleReportsAStaleLeftoverFromAnyEarlierRun covers the wider class
// the pin case is only one instance of: any pool file this run's Packages
// does not index has the same shape, and the commonest source is not pinning
// at all. Here an earlier request named tree and this one does not. Nothing
// supersedes tree, so prune keeps it whatever the flags say - it is the only
// member of its (name, arch) group and therefore its own numeric best - and
// it lands in exactly the same blind spot: on the media, under the
// signature, in no index and in no run report.
func TestAssembleReportsAStaleLeftoverFromAnyEarlierRun(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()

	inBoth := baseAssembleInput(t, root, []resolve.Selection{
		selectionFor(t, staging, "jq", "1.6", "amd64", lock.ReasonRequested),
		selectionFor(t, staging, "tree", "2.1.0", "amd64", lock.ReasonRequested),
	})
	if _, err := Assemble(context.Background(), inBoth); err != nil {
		t.Fatalf("Assemble jq+tree: %v", err)
	}

	inJQOnly := baseAssembleInput(t, root, []resolve.Selection{
		selectionFor(t, staging, "jq", "1.6", "amd64", lock.ReasonRequested),
	})
	inJQOnly.Prune = true
	res, err := Assemble(context.Background(), inJQOnly)
	if err != nil {
		t.Fatalf("Assemble jq only with prune: %v", err)
	}

	stale := repository.PoolPath("tree", "tree_2.1.0_amd64.deb")
	if _, err := os.Stat(filepath.Join(inJQOnly.Dir, "repo", stale)); err != nil {
		t.Fatalf("premise broken: the dropped package is not in the pool: %v", err)
	}
	if got := readLines(t, inJQOnly.Dir, RemovedFile); len(got) != 0 {
		t.Fatalf("premise broken: last-run-removed.txt = %v, want empty", got)
	}
	pkgs, err := os.ReadFile(filepath.Join(inJQOnly.Dir, "repo", "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	if strings.Contains(string(pkgs), "Package: tree") {
		t.Fatalf("premise broken: Packages still indexes tree")
	}

	got := readLines(t, inJQOnly.Dir, UnreferencedFile)
	if len(got) != 1 || got[0] != stale {
		t.Fatalf("%s = %v, want [%s]", UnreferencedFile, got, stale)
	}
	if len(res.Unreferenced) != 1 || res.Unreferenced[0] != stale {
		t.Errorf("Result.Unreferenced = %v, want [%s]", res.Unreferenced, stale)
	}

	l, err := lock.Load(inJQOnly.Dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	w := warningWithCode(l, WarnUnreferenced)
	if w == nil {
		t.Fatalf("lock.json carries no %s warning; warnings=%+v", WarnUnreferenced, l.Warnings)
	}
	if !strings.Contains(w.Message, stale) {
		t.Errorf("warning message does not name %q: %s", stale, w.Message)
	}
	// No version of tree is indexed, so there is no authoritative name to
	// quote for the indexed side and the warning must not invent one.
	if !strings.Contains(w.Message, "no version of this package") {
		t.Errorf("warning does not distinguish the no-indexed-sibling case: %s", w.Message)
	}
	if len(w.Packages) != 0 {
		t.Errorf("warning Packages = %v, want empty: nothing here is indexed at another version", w.Packages)
	}
}

// TestAssembleReportsNothingUnreferencedOnACleanBuild is the other half of
// the teeth: the report must be silent when there is nothing to report, or
// the two tests above would pass against an implementation that always
// warns. It also pins that the file is written even when empty - its constant
// presence is what keeps the bundle's manifest file set stable across runs.
func TestAssembleReportsNothingUnreferencedOnACleanBuild(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	in := baseAssembleInput(t, root, []resolve.Selection{
		selectionFor(t, staging, "jq", "1.6", "amd64", lock.ReasonRequested),
		selectionFor(t, staging, "tree", "2.1.0", "amd64", lock.ReasonRequested),
	})
	res, err := Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(res.Unreferenced) != 0 {
		t.Errorf("Result.Unreferenced = %v on a clean build, want empty", res.Unreferenced)
	}
	b, err := os.ReadFile(filepath.Join(in.Dir, UnreferencedFile))
	if err != nil {
		t.Fatalf("read %s: %v", UnreferencedFile, err)
	}
	if len(b) != 0 {
		t.Errorf("%s = %q on a clean build, want empty", UnreferencedFile, b)
	}
	l, err := lock.Load(in.Dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if w := warningWithCode(l, WarnUnreferenced); w != nil {
		t.Errorf("clean build raised %s: %s", WarnUnreferenced, w.Message)
	}
}

// TestUnreferencedReportIsCoveredByTheManifest guards the ordering trap. A
// report about what the operator's signature covers is worthless if it is
// itself outside that signature - and worse than worthless, because
// verify.Verify reports a file the manifest does not list as
// file-unexpected. Assemble writes README.txt and the two older last-run
// reports AFTER its own manifest.Build; this one must not inherit that.
func TestUnreferencedReportIsCoveredByTheManifest(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	in := baseAssembleInput(t, root, []resolve.Selection{
		selectionFor(t, staging, "jq", "1.6", "amd64", lock.ReasonRequested),
	})
	res, err := Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !manifestCovers(res.Manifest, UnreferencedFile) {
		t.Errorf("%s is not in the manifest's file list; it must be written before manifest.Build", UnreferencedFile)
	}
	// The manifest must also agree with what is on disk, which is what
	// verify re-derives: writing the file before Build is only correct if
	// nothing rewrites it afterwards.
	m, _, err := manifest.Load(in.Dir)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	if !manifestCovers(m, UnreferencedFile) {
		t.Errorf("%s is missing from the saved manifest", UnreferencedFile)
	}
}

// snapshotTree reads every file under dir into a path-keyed map, so two
// bundles can be compared byte for byte rather than field by field.
func snapshotTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestUnreferencedReportIsByteReproducible measures the project's
// non-negotiable rather than assuming it: two independent runs of the same
// two-step scenario, in two separate roots with two separate stores, must
// produce byte-identical bundles - Release, Packages and Packages.gz
// included, and now last-run-unreferenced.txt and the lock warning that
// quotes it too. Comparing the whole tree rather than a named list means a
// future file that is not reproducible cannot slip past this.
func TestUnreferencedReportIsByteReproducible(t *testing.T) {
	staging := t.TempDir()

	buildOnce := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		inHigh := baseAssembleInput(t, root, []resolve.Selection{
			selectionFor(t, staging, "foo", "2.0", "amd64", lock.ReasonRequested),
		})
		if _, err := Assemble(context.Background(), inHigh); err != nil {
			t.Fatalf("Assemble 2.0: %v", err)
		}
		inPinned := baseAssembleInput(t, root, []resolve.Selection{
			selectionFor(t, staging, "foo", "1.0", "amd64", lock.ReasonRequested),
		})
		inPinned.Prune = true
		if _, err := Assemble(context.Background(), inPinned); err != nil {
			t.Fatalf("Assemble pinned 1.0: %v", err)
		}
		return inPinned.Dir
	}

	a := snapshotTree(t, buildOnce(t))
	b := snapshotTree(t, buildOnce(t))

	if len(a[UnreferencedFile]) == 0 {
		t.Fatalf("%s is empty; this test would compare two bundles that never exercised the report", UnreferencedFile)
	}

	var paths []string
	for p := range a {
		paths = append(paths, p)
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		av, aok := a[p]
		bv, bok := b[p]
		switch {
		case !aok:
			t.Errorf("%s exists in the second bundle only", p)
		case !bok:
			t.Errorf("%s exists in the first bundle only", p)
		case string(av) != string(bv):
			t.Errorf("%s differs between two identical runs (%d vs %d bytes)", p, len(av), len(bv))
		}
	}
}
