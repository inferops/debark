package resolve

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
)

// SortSelections is what makes bundles byte-identical across runs: every
// derived artefact (the pool order, the lock, the Packages index, the
// manifest, the SBOM) is written from this order. Until now nothing tested
// it at all.

func names(sels []Selection) []string {
	out := make([]string, len(sels))
	for i, s := range sels {
		out[i] = s.Name + "|" + s.Arch + "|" + s.Version
	}
	return out
}

func TestSortSelections_OrdersByNameThenArchThenVersion(t *testing.T) {
	in := []Selection{
		{Name: "vlc", Arch: "amd64", Version: "3.0.21-1"},
		{Name: "jq", Arch: "i386", Version: "1.6-2.1"},
		{Name: "jq", Arch: "amd64", Version: "1.7-1"},
		{Name: "jq", Arch: "amd64", Version: "1.6-2.1"},
		{Name: "base-files", Arch: "all", Version: "12.4"},
	}
	SortSelections(in)
	want := []string{
		"base-files|all|12.4",
		"jq|amd64|1.6-2.1",
		"jq|amd64|1.7-1",
		"jq|i386|1.6-2.1",
		"vlc|amd64|3.0.21-1",
	}
	if got := names(in); !reflect.DeepEqual(got, want) {
		t.Errorf("order =\n %v\nwant\n %v", got, want)
	}
}

// The property that actually matters: whatever order the caller hands over,
// the same set comes back in the same order. Backends build their selection
// slice by iterating a map (core/apt/local.go's fetchAndBuildSelections does
// exactly that), so the input order genuinely varies run to run.
func TestSortSelections_IsIndependentOfInputOrder(t *testing.T) {
	base := []Selection{
		{Name: "jq", Arch: "amd64", Version: "1.6-2.1", SHA256: "a"},
		{Name: "jq", Arch: "amd64", Version: "1.7-1", SHA256: "b"},
		{Name: "jq", Arch: "i386", Version: "1.6-2.1", SHA256: "c"},
		{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1", SHA256: "d"},
		{Name: "libonig5", Arch: "amd64", Version: "6.9.8-1", SHA256: "e"},
		{Name: "base-files", Arch: "all", Version: "12.4", SHA256: "f"},
		{Name: "", Arch: "", Version: "", SHA256: "g"},
	}
	sorted := append([]Selection(nil), base...)
	SortSelections(sorted)

	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		shuffled := append([]Selection(nil), base...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		SortSelections(shuffled)
		if !reflect.DeepEqual(shuffled, sorted) {
			t.Fatalf("trial %d: permuting the input changed the sorted order:\n got %v\nwant %v",
				trial, names(shuffled), names(sorted))
		}
	}
}

// Sorting twice must be a no-op, and sorting an already-sorted slice must not
// disturb it.
func TestSortSelections_IsIdempotent(t *testing.T) {
	in := []Selection{
		{Name: "b", Arch: "amd64", Version: "2"},
		{Name: "a", Arch: "amd64", Version: "1"},
		{Name: "a", Arch: "all", Version: "1"},
	}
	SortSelections(in)
	once := append([]Selection(nil), in...)
	SortSelections(in)
	if !reflect.DeepEqual(in, once) {
		t.Errorf("second sort changed the order: %v -> %v", names(once), names(in))
	}
}

// Degenerate shapes must not panic. A nil slice, an empty slice and a
// one-element slice all reach SortSelections from a real build (a request
// entirely satisfied on the target produces no selections at all).
func TestSortSelections_DegenerateSlices(t *testing.T) {
	SortSelections(nil)
	SortSelections([]Selection{})
	one := []Selection{{Name: "only", Arch: "all", Version: "1"}}
	SortSelections(one)
	if len(one) != 1 || one[0].Name != "only" {
		t.Errorf("one-element slice was disturbed: %+v", one)
	}
}

// Empty and unusual fields sort without special-casing, and empties sort
// first — which is what puts a malformed selection at the front of every
// derived artefact rather than scattered through it.
func TestSortSelections_EmptyAndUnusualFields(t *testing.T) {
	in := []Selection{
		{Name: "z", Arch: "amd64", Version: "1"},
		{Name: "", Arch: "amd64", Version: "1"},
		{Name: "a", Arch: "", Version: "1"},
		{Name: "a", Arch: "amd64", Version: ""},
		{Name: "a", Arch: "amd64", Version: "1:2.0-1~bpo12+1"},
		{Name: "é́", Arch: "amd64", Version: "1"},   // non-ASCII name
		{Name: "a\nb", Arch: "amd64", Version: "1"}, // newline in a name
	}
	SortSelections(in)
	if in[0].Name != "" {
		t.Errorf("empty name did not sort first: %v", names(in))
	}
	// Byte order throughout: no locale, no Unicode collation, nothing that
	// could differ between a builder in one locale and a builder in another.
	for i := 1; i < len(in); i++ {
		prev, cur := in[i-1], in[i]
		if prev.Name > cur.Name {
			t.Fatalf("names out of byte order at %d: %q then %q", i, prev.Name, cur.Name)
		}
	}
}

// Version ordering is byte-lexicographic, NOT Debian version order: "1.10"
// sorts before "1.9". That is only ever a tie-break between two selections
// sharing a (name, arch), which a real solve does not produce, and byte order
// is what determinism needs. Pinned here so a future change to it is a
// deliberate one rather than an accident.
func TestSortSelections_VersionOrderIsLexicographicNotDebian(t *testing.T) {
	in := []Selection{
		{Name: "p", Arch: "amd64", Version: "1.9"},
		{Name: "p", Arch: "amd64", Version: "1.10"},
	}
	SortSelections(in)
	if in[0].Version != "1.10" {
		t.Errorf("version order = %v; SortSelections is documented as byte order, not dpkg --compare-versions order", names(in))
	}
}

// KNOWN GAP, pinned deliberately. SortSelections compares only (Name, Arch,
// Version), so two selections agreeing on all three but differing anywhere
// else — Filename, SHA256, URI, StagedPath — compare EQUAL. sort.SliceStable
// then leaves them in the order the caller supplied, which for a caller that
// builds its slice from a map is not a fixed order.
//
// Nothing in the tree produces such a pair today (core/apt keys its selection
// map on (name, arch), so version is a function of the key, and
// core/bundle's materialiseSelections refuses two selections claiming one
// pool path), and lock.Validate does not check for duplicates either. The
// order is therefore total in practice but partial by contract. Adding
// Filename and SHA256 as further tie-breaks would make it total outright;
// core/resolve/types.go is on the frozen-file list, so that is reported
// rather than done here.
func TestSortSelections_TiesOnNameArchVersionKeepCallerOrder(t *testing.T) {
	forward := []Selection{
		{Name: "p", Arch: "amd64", Version: "1", Filename: "b.deb", SHA256: "bb"},
		{Name: "p", Arch: "amd64", Version: "1", Filename: "a.deb", SHA256: "aa"},
	}
	reversed := []Selection{forward[1], forward[0]}
	SortSelections(forward)
	SortSelections(reversed)
	if forward[0].Filename == reversed[0].Filename {
		t.Fatalf("ties are broken by a field beyond (name, arch, version) — the documented gap is closed; update this test and the finding it records")
	}
}

func TestSelectionKeyAndNameVersion(t *testing.T) {
	s := Selection{Name: "jq", Arch: "amd64", Version: "1.6-2.1"}
	if got := s.Key(); got != "jq:amd64" {
		t.Errorf("Key() = %q, want %q", got, "jq:amd64")
	}
	if got := s.NameVersion(); got != "jq=1.6-2.1" {
		t.Errorf("NameVersion() = %q, want %q", got, "jq=1.6-2.1")
	}
	// Key is the dedup/prune identity core/apt keys its "already seen" maps
	// on, so it has to separate two architectures of one name.
	a := Selection{Name: "jq", Arch: "amd64"}
	b := Selection{Name: "jq", Arch: "i386"}
	if a.Key() == b.Key() {
		t.Errorf("two architectures share a Key: %q", a.Key())
	}
}

// Key concatenates around a ':' with no escaping, so a Name containing a ':'
// can collide with a different (name, arch) pair. Debian Policy §5.6.7
// forbids ':' in a package name and core/apt validates its apt operands, so
// nothing honest produces this; recorded because Selection itself is
// unvalidated and Key is what core/apt's container reconciliation dedups on.
func TestSelectionKeyIsNotInjective(t *testing.T) {
	a := Selection{Name: "jq:amd64"}
	b := Selection{Name: "jq", Arch: "amd64"}
	if a.Key() != b.Key() {
		t.Skip("Key() gained escaping or Selection gained validation; update the finding this records")
	}
}

func TestDependencyOfRoundTripsThroughIsDependency(t *testing.T) {
	cases := []string{"jq", "lib:weird", "a b", "é", "-"}
	for _, pkg := range cases {
		reason := DependencyOf(pkg)
		if !strings.HasPrefix(reason, lock.ReasonDependencyOfPrefix) {
			t.Errorf("DependencyOf(%q) = %q, missing the schema prefix", pkg, reason)
		}
		got, ok := IsDependency(reason)
		if !ok || got != pkg {
			t.Errorf("IsDependency(%q) = (%q, %v), want (%q, true)", reason, got, ok, pkg)
		}
	}
}

func TestIsDependencyRejectsTheOtherReasons(t *testing.T) {
	for _, reason := range []string{
		lock.ReasonRequested,
		lock.ReasonUpgrade,
		lock.ReasonExternal,
		"",
		"dependency-of", // the prefix without its separator
		"Dependency-Of:jq",
		"xdependency-of:jq",
	} {
		if pkg, ok := IsDependency(reason); ok {
			t.Errorf("IsDependency(%q) = (%q, true), want false", reason, pkg)
		}
	}
}

// A bare prefix with nothing after it is a reason lock.Validate rejects
// (validReason requires something past the prefix), so IsDependency reporting
// "yes, of nothing" is worth knowing about explicitly.
func TestIsDependencyOnABarefPrefix(t *testing.T) {
	pkg, ok := IsDependency(lock.ReasonDependencyOfPrefix)
	if !ok || pkg != "" {
		t.Errorf("IsDependency(bare prefix) = (%q, %v), want (\"\", true)", pkg, ok)
	}
}
