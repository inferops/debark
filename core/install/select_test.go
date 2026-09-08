package install

import (
	"reflect"
	"sort"
	"testing"

	"github.com/inferops/debark/core/lock"
)

// testLock's Install entries are architecture-qualified (name:arch=version,
// dpkg's own syntax): that is what lock.Validate now requires, and what
// selectInstallSet resolves via lk.Find(name, arch).
func testLock() *lock.Lock {
	return &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Install:       []string{"jq:amd64=1.6-2.1", "libjq1:amd64=1.6-2.1", "libonig5:amd64=6.9.8-1"},
		Packages: []lock.Package{
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1", Size: 100},
			{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1", Size: 50},
			{Name: "libonig5", Arch: "amd64", Version: "6.9.8-1", Size: 200},
			{Name: "tree", Arch: "amd64", Version: "2.1.0", Size: 60}, // in the bundle but not in Install
		},
	}
}

func TestSelectInstallSet_Default(t *testing.T) {
	names, pkgs, err := selectInstallSet(testLock(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"jq:amd64=1.6-2.1", "libjq1:amd64=1.6-2.1", "libonig5:amd64=6.9.8-1"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if len(pkgs) != 3 {
		t.Errorf("len(pkgs) = %d, want 3", len(pkgs))
	}
	// tree must never appear: the plan comes from the lock's Install set, not
	// from "everything in the bundle" (ADR-007), unless --all is given.
	for _, nv := range names {
		if nv == "tree:amd64=2.1.0" {
			t.Errorf("default selection must not include unrequested bundle contents")
		}
	}
}

func TestSelectInstallSet_All(t *testing.T) {
	names, pkgs, err := selectInstallSet(testLock(), Options{All: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"jq:amd64=1.6-2.1", "libjq1:amd64=1.6-2.1", "libonig5:amd64=6.9.8-1", "tree:amd64=2.1.0"}
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if len(pkgs) != 4 {
		t.Errorf("len(pkgs) = %d, want 4", len(pkgs))
	}
}

func TestSelectInstallSet_Only(t *testing.T) {
	names, pkgs, err := selectInstallSet(testLock(), Options{Only: []string{"jq"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"jq:amd64=1.6-2.1"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "jq" {
		t.Errorf("pkgs = %v", pkgs)
	}
}

func TestSelectInstallSet_OnlyAndAll(t *testing.T) {
	names, _, err := selectInstallSet(testLock(), Options{All: true, Only: []string{"tree"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"tree:amd64=2.1.0"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v (--only must narrow --all too)", names, want)
	}
}

func TestSelectInstallSet_OnlyUnknownName(t *testing.T) {
	_, _, err := selectInstallSet(testLock(), Options{Only: []string{"does-not-exist"}})
	if err == nil {
		t.Fatalf("expected an error for an --only name absent from the lock")
	}
}

func TestSelectInstallSet_Empty(t *testing.T) {
	lk := testLock()
	lk.Install = nil
	names, pkgs, err := selectInstallSet(lk, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 0 || len(pkgs) != 0 {
		t.Errorf("expected empty selection, got names=%v pkgs=%v", names, pkgs)
	}
}

func TestSelectInstallSet_MalformedInstallEntry(t *testing.T) {
	lk := testLock()
	lk.Install = []string{"jq=1.6-2.1"} // unqualified: must be rejected
	if _, _, err := selectInstallSet(lk, Options{}); err == nil {
		t.Fatalf("expected an error for an unqualified (non name:arch=version) install entry")
	}
}

func TestArchQualifiedEntry(t *testing.T) {
	got := archQualifiedEntry(lock.Package{Name: "jq", Arch: "amd64", Version: "1.6-2.1"})
	if got != "jq:amd64=1.6-2.1" {
		t.Errorf("archQualifiedEntry = %q, want jq:amd64=1.6-2.1", got)
	}
}

func TestSelectUpgradeSet_Basic(t *testing.T) {
	lk := &lock.Lock{
		Packages: []lock.Package{
			{Name: "base-files", Arch: "amd64", Version: "12.4+deb12u15", Reason: lock.ReasonUpgrade},
			{Name: "liblzma5", Arch: "amd64", Version: "5.4.1-1+deb12u1", Reason: lock.ReasonUpgrade},
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1", Reason: lock.ReasonRequested},
		},
	}
	names, pkgs, total := selectUpgradeSet(lk, nil)
	want := []string{"base-files:amd64=12.4+deb12u15", "liblzma5:amd64=5.4.1-1+deb12u1"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if len(pkgs) != 2 {
		t.Errorf("len(pkgs) = %d, want 2", len(pkgs))
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
}

func TestSelectUpgradeSet_ExcludesNonUpgradeReasons(t *testing.T) {
	lk := &lock.Lock{
		Packages: []lock.Package{
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1", Reason: lock.ReasonRequested},
			{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1", Reason: lock.ReasonDependencyOfPrefix + "jq"},
			{Name: "vendor-pkg", Arch: "amd64", Version: "1.0", Reason: lock.ReasonExternal},
		},
	}
	names, pkgs, total := selectUpgradeSet(lk, nil)
	if len(names) != 0 || len(pkgs) != 0 || total != 0 {
		t.Errorf("names=%v pkgs=%v total=%d, want all empty: none of these reasons is \"upgrade\"", names, pkgs, total)
	}
}

func TestSelectUpgradeSet_Empty(t *testing.T) {
	lk := &lock.Lock{}
	names, pkgs, total := selectUpgradeSet(lk, nil)
	if len(names) != 0 || len(pkgs) != 0 || total != 0 {
		t.Errorf("expected a fully empty result for an empty lock, got names=%v pkgs=%v total=%d", names, pkgs, total)
	}
}

// TestSelectUpgradeSet_DependencyTakesPrecedence is the case called out
// explicitly in this fix's test brief: the lock records the same package
// both ways — once inside the request/dependency closure at one version,
// and separately, as its own pool entry, with Reason "upgrade" at a
// different version. That is a normal outcome of the additive bundles
// (the same package can legitimately sit in the pool at two versions — the
// exact shape E3's own bundle-accumulated fixture used, docs/experiments/
// E3-pin-fidelity.md), not a contrived case.
//
// The dependency/request version must win: selectUpgradeSet must not also
// add a second, conflicting version request for the same name:arch, since
// apt-get install cannot sensibly be asked to install two different
// versions of one package in a single invocation, and the dependency
// closure's version is the harder, more specific constraint (something
// requested actually needs exactly that version) versus "also happened to
// be upgradeable".
func TestSelectUpgradeSet_DependencyTakesPrecedence(t *testing.T) {
	baseSelection := []lock.Package{
		{Name: "libfoo", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonDependencyOfPrefix + "bar"},
	}
	lk := &lock.Lock{
		Packages: []lock.Package{
			baseSelection[0],
			// A second, separate pool entry for the SAME name:arch at a
			// DIFFERENT version, tagged upgrade: e.g. left over from a prior
			// incremental build, or what a bare full-upgrade would have
			// chosen online before the dependency closure pinned libfoo to
			// 1.0-1 for this run.
			{Name: "libfoo", Arch: "amd64", Version: "2.0-1", Reason: lock.ReasonUpgrade},
			{Name: "quux", Arch: "amd64", Version: "3.0-1", Reason: lock.ReasonUpgrade},
		},
	}

	names, pkgs, total := selectUpgradeSet(lk, baseSelection)

	if total != 2 {
		t.Errorf("total = %d, want 2 (both upgrade-reason entries counted, before the base-selection dedup)", total)
	}
	want := []string{"quux:amd64=3.0-1"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v (libfoo:amd64 must be excluded: the dependency closure already claims it, at a different version)", names, want)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "quux" {
		t.Errorf("pkgs = %v, want just quux", pkgs)
	}
}

func TestSplitInstallEntry(t *testing.T) {
	name, arch, version, ok := splitInstallEntry("jq:amd64=1.6-2.1")
	if !ok || name != "jq" || arch != "amd64" || version != "1.6-2.1" {
		t.Fatalf("got (%q, %q, %q, %v)", name, arch, version, ok)
	}
	for _, bad := range []string{"jq=1.6-2.1", "jq:amd64", "", "=1.6", "jq:=1.6"} {
		if _, _, _, ok := splitInstallEntry(bad); ok {
			t.Errorf("splitInstallEntry(%q) = ok, want rejected", bad)
		}
	}
}
