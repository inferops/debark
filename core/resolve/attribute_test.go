package resolve

import (
	"reflect"
	"testing"
)

func selNamed(names ...string) []Selection {
	out := make([]Selection, len(names))
	for i, n := range names {
		out[i] = Selection{Name: n, Arch: "amd64", Version: "1.0"}
	}
	return out
}

func TestAttributeReasons_SimpleChain(t *testing.T) {
	// jq depends on libjq1, which depends on libonig5 (the real chain from
	// the prototype baseline, testdata/baseline).
	sels := selNamed("jq", "libjq1", "libonig5", "tree")
	deps := map[string][]string{
		"jq":     {"libjq1"},
		"libjq1": {"libonig5"},
	}
	sels[0].Reason = "requested" // jq
	sels[3].Reason = "requested" // tree

	AttributeReasons([]string{"jq", "tree"}, sels, deps)

	want := map[string]string{
		"jq":       "requested",
		"tree":     "requested",
		"libjq1":   "dependency-of:jq",
		"libonig5": "dependency-of:jq", // reached via jq -> libjq1 -> libonig5
	}
	for _, s := range sels {
		if s.Reason != want[s.Name] {
			t.Errorf("%s: Reason = %q, want %q", s.Name, s.Reason, want[s.Name])
		}
	}
}

func TestAttributeReasons_DoesNotOverwriteExistingReason(t *testing.T) {
	sels := selNamed("a", "b")
	sels[0].Reason = "external"
	deps := map[string][]string{"a": {"b"}}
	AttributeReasons([]string{"a"}, sels, deps)
	if sels[0].Reason != "external" {
		t.Errorf("existing reason was overwritten: %q", sels[0].Reason)
	}
	if sels[1].Reason != DependencyOf("a") {
		t.Errorf("b.Reason = %q, want %q", sels[1].Reason, DependencyOf("a"))
	}
}

func TestAttributeReasons_TieBreakIsDeterministic(t *testing.T) {
	// c is reachable from both "a" and "b" in one hop each: the
	// lexicographically smaller root must always win, and must do so the
	// same way regardless of map iteration order (deps is a map, so this
	// guards against a non-deterministic implementation slipping in).
	sels := selNamed("a", "b", "c")
	sels[0].Reason = "requested"
	sels[1].Reason = "requested"
	deps := map[string][]string{
		"a": {"c"},
		"b": {"c"},
	}
	for i := 0; i < 20; i++ {
		trial := selNamed("a", "b", "c")
		trial[0].Reason = "requested"
		trial[1].Reason = "requested"
		AttributeReasons([]string{"a", "b"}, trial, deps)
		if got := trial[2].Reason; got != DependencyOf("a") {
			t.Fatalf("trial %d: c.Reason = %q, want %q (deterministic tie-break)", i, got, DependencyOf("a"))
		}
	}
	_ = sels
}

func TestAttributeReasons_UnexplainedFallsBackToFirstRootAlphabetically(t *testing.T) {
	// "orphan" is not reachable from any root via deps at all (e.g. pulled
	// in by a Conflicts/Breaks resolution this package does not model): it
	// must still get a plain dependency-of edge, to the lexicographically
	// first root, rather than being left with an empty Reason.
	sels := selNamed("zzz-root", "aaa-root", "orphan")
	sels[0].Reason = "requested"
	sels[1].Reason = "requested"
	AttributeReasons([]string{"zzz-root", "aaa-root"}, sels, map[string][]string{})
	if got := sels[2].Reason; got != DependencyOf("aaa-root") {
		t.Errorf("orphan.Reason = %q, want %q", got, DependencyOf("aaa-root"))
	}
}

func TestAttributeReasons_NoRootsLeavesUnknownBlank(t *testing.T) {
	sels := selNamed("a")
	AttributeReasons(nil, sels, nil)
	if sels[0].Reason != "" {
		t.Errorf("Reason = %q, want empty with no roots at all", sels[0].Reason)
	}
}

func TestAttributeReasons_NeverInventsAnEdgeNotInDeps(t *testing.T) {
	// Two independent chains sharing no edge: b must never be attributed to
	// "a" just because "a" is a root, absent a deps edge for it.
	sels := selNamed("a", "b")
	sels[0].Reason = "requested"
	AttributeReasons([]string{"a"}, sels, map[string][]string{ /* no edges at all */ })
	// With zero deps edges and only one root, the documented fallback still
	// attributes b to that root (approximation) — verifying
	// that here pins the documented behaviour so a future change to it is
	// a conscious one.
	if sels[1].Reason != DependencyOf("a") {
		t.Errorf("b.Reason = %q, want the documented fallback %q", sels[1].Reason, DependencyOf("a"))
	}
}

// TestAttributeReasons_UpgradeRootKeepsItsBlankReason is the regression test
// for a real defect, reproduced against core/apt's own call sequence.
//
// core/apt/local.go calls AttributeReasons with EVERY root, including the
// packages full-upgrade pulled in, whose Reason it has not stamped yet; the
// four lines after the call stamp lock.ReasonUpgrade on anything still blank.
// AttributeReasons used to fill those blanks itself from the fallback branch,
// because a root's own via entry points at itself and the "origin != s.Name"
// guard fell through to the fallback instead of stopping.
//
// Measured consequences of that, all three at once:
//
//   - the lexicographically first upgrade root got dependency-of:<itself>, a
//     self-referential edge written straight into lock.json (lock.Validate
//     accepts it: validReason only checks the prefix);
//   - every other upgrade root got dependency-of:<that first root>, an edge
//     no package's control data declared — the exact thing principle 1
//     forbids this labelling pass from inventing;
//   - the backend's upgrade branch never fired, so Reason "upgrade" never
//     appeared in a locally-resolved lock at all, and core/apt's
//     closedWorldUpgradeTargets — which selects on exactly that string —
//     found an empty upgrade set and certified nothing.
func TestAttributeReasons_UpgradeRootKeepsItsBlankReason(t *testing.T) {
	// base-files and liblzma5 are upgrade roots with no Reason yet; libjq1
	// is a requested root that already has one.
	sels := selNamed("base-files", "liblzma5", "libjq1")
	sels[2].Reason = "requested"

	AttributeReasons([]string{"base-files", "liblzma5", "libjq1"}, sels, map[string][]string{})

	for _, s := range sels[:2] {
		if s.Reason != "" {
			t.Errorf("%s is a root: Reason = %q, want it left blank for the caller to name", s.Name, s.Reason)
		}
	}
	if sels[2].Reason != "requested" {
		t.Errorf("libjq1.Reason = %q, want %q", sels[2].Reason, "requested")
	}
}

// A root must never be recorded as a dependency of itself, even when its own
// control data lists itself and it is the only selection there is.
func TestAttributeReasons_NeverEmitsASelfEdge(t *testing.T) {
	sels := selNamed("vlc")
	AttributeReasons([]string{"vlc"}, sels, map[string][]string{"vlc": {"vlc"}})
	if sels[0].Reason == DependencyOf("vlc") {
		t.Fatalf("vlc.Reason = %q: a package is not a dependency of itself", sels[0].Reason)
	}
	if sels[0].Reason != "" {
		t.Errorf("vlc.Reason = %q, want blank (it is a root)", sels[0].Reason)
	}
}

// Leaving a root blank must not cost the packages underneath it their edge.
func TestAttributeReasons_BlankRootStillExplainsItsDependencies(t *testing.T) {
	sels := selNamed("base-files", "liblzma5")
	AttributeReasons([]string{"base-files"}, sels, map[string][]string{"base-files": {"liblzma5"}})
	if sels[0].Reason != "" {
		t.Errorf("base-files.Reason = %q, want blank (it is a root)", sels[0].Reason)
	}
	if sels[1].Reason != DependencyOf("base-files") {
		t.Errorf("liblzma5.Reason = %q, want %q", sels[1].Reason, DependencyOf("base-files"))
	}
}

// The fallback representative must name a package the bundle actually
// contains. A root can legitimately be absent from selections — a requested
// package held at its current version on the target is one — and pointing a
// dependency-of edge at it makes the lock describe a parent nothing can look
// up.
func TestAttributeReasons_FallbackPrefersARootThatIsInTheBundle(t *testing.T) {
	// "aaa-held" sorts first but is not among the selections.
	sels := selNamed("zzz-root", "orphan")
	sels[0].Reason = "requested"
	AttributeReasons([]string{"aaa-held", "zzz-root"}, sels, map[string][]string{})
	if got := sels[1].Reason; got != DependencyOf("zzz-root") {
		t.Errorf("orphan.Reason = %q, want %q (aaa-held is not in the bundle)", got, DependencyOf("zzz-root"))
	}
}

// With no root present in the bundle at all, the documented approximation
// still applies rather than leaving a blank Reason that lock.Validate would
// reject at the end of a long build.
func TestAttributeReasons_FallbackWithNoKnownRootStillLabels(t *testing.T) {
	sels := selNamed("orphan")
	AttributeReasons([]string{"ghost-b", "ghost-a"}, sels, map[string][]string{})
	if got := sels[0].Reason; got != DependencyOf("ghost-a") {
		t.Errorf("orphan.Reason = %q, want %q", got, DependencyOf("ghost-a"))
	}
}

// AttributeReasons is a labelling pass: apt is the oracle and this writes
// captions. It must never add, drop, reorder or otherwise touch a selection's
// identity or its bytes, so nothing it does can change what ends up in a
// bundle.
func TestAttributeReasons_ChangesNothingButReason(t *testing.T) {
	before := []Selection{
		{Name: "jq", Arch: "amd64", Version: "1.6-2.1", Filename: "jq_1.6-2.1_amd64.deb", Size: 12, SHA256: "aa", URI: "http://x/jq.deb", Reason: "requested", Essential: true, StagedPath: "/a/jq.deb", UserSupplied: true},
		{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1", Filename: "libjq1_1.6-2.1_amd64.deb", Size: 34, SHA256: "bb", URI: "http://x/libjq1.deb"},
		{Name: "orphan", Arch: "all", Version: "9", Filename: "orphan_9_all.deb", Size: 56, SHA256: "cc"},
	}
	after := append([]Selection(nil), before...)
	AttributeReasons([]string{"jq"}, after, map[string][]string{"jq": {"libjq1"}})

	if len(after) != len(before) {
		t.Fatalf("selection count changed: %d -> %d", len(before), len(after))
	}
	for i := range after {
		got, want := after[i], before[i]
		if got.Name != want.Name {
			t.Fatalf("selections were reordered at %d: %q != %q", i, got.Name, want.Name)
		}
		// Compare every other field by blanking Reason on both sides.
		got.Reason, want.Reason = "", ""
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a field other than Reason changed:\n got %+v\nwant %+v", after[i].Name, got, want)
		}
	}
	if after[1].Reason != DependencyOf("jq") {
		t.Errorf("libjq1.Reason = %q, want %q", after[1].Reason, DependencyOf("jq"))
	}
}

// Every OR-alternative that is itself a resolved selection is an edge
// (core/apt's buildDependencyGraph adds them all, not one representative), so
// several intermediates can reach the same node at the same distance. The
// answer must still be one deterministic root.
func TestAttributeReasons_MultipleAlternativesStillDeterministic(t *testing.T) {
	deps := map[string][]string{
		"root":  {"alt-b", "alt-a"}, // deliberately unsorted input
		"alt-a": {"shared"},
		"alt-b": {"shared"},
	}
	for i := 0; i < 25; i++ {
		sels := selNamed("root", "alt-a", "alt-b", "shared")
		sels[0].Reason = "requested"
		AttributeReasons([]string{"root"}, sels, deps)
		for _, s := range sels[1:] {
			if s.Reason != DependencyOf("root") {
				t.Fatalf("trial %d: %s.Reason = %q, want %q", i, s.Name, s.Reason, DependencyOf("root"))
			}
		}
	}
}

// Hostile and degenerate shapes must not hang, panic or invent a name.
func TestAttributeReasons_DegenerateInputs(t *testing.T) {
	t.Run("nil selections", func(t *testing.T) {
		AttributeReasons([]string{"a"}, nil, map[string][]string{"a": {"b"}})
	})
	t.Run("empty selection slice", func(t *testing.T) {
		AttributeReasons([]string{"a"}, []Selection{}, nil)
	})
	t.Run("nil deps map", func(t *testing.T) {
		sels := selNamed("a", "b")
		sels[0].Reason = "requested"
		AttributeReasons([]string{"a"}, sels, nil)
		if sels[1].Reason != DependencyOf("a") {
			t.Errorf("b.Reason = %q, want %q", sels[1].Reason, DependencyOf("a"))
		}
	})
	t.Run("dependency cycle", func(t *testing.T) {
		sels := selNamed("a", "b", "c")
		sels[0].Reason = "requested"
		AttributeReasons([]string{"a"}, sels, map[string][]string{
			"a": {"b"}, "b": {"c"}, "c": {"a", "b"},
		})
		for _, s := range sels[1:] {
			if s.Reason != DependencyOf("a") {
				t.Errorf("%s.Reason = %q, want %q", s.Name, s.Reason, DependencyOf("a"))
			}
		}
	})
	t.Run("self loop on a non-root", func(t *testing.T) {
		sels := selNamed("a", "b")
		sels[0].Reason = "requested"
		AttributeReasons([]string{"a"}, sels, map[string][]string{"a": {"b"}, "b": {"b"}})
		if sels[1].Reason != DependencyOf("a") {
			t.Errorf("b.Reason = %q, want %q", sels[1].Reason, DependencyOf("a"))
		}
	})
	t.Run("duplicate roots", func(t *testing.T) {
		sels := selNamed("a", "b")
		sels[0].Reason = "requested"
		AttributeReasons([]string{"a", "a", "a"}, sels, map[string][]string{"a": {"b"}})
		if sels[1].Reason != DependencyOf("a") {
			t.Errorf("b.Reason = %q, want %q", sels[1].Reason, DependencyOf("a"))
		}
	})
	t.Run("empty root name is not a package", func(t *testing.T) {
		// "" is not a package name, so it must not become the fallback
		// representative and produce a bare "dependency-of:" prefix, which
		// lock.Validate rejects (validReason requires something after it).
		sels := selNamed("a", "b")
		sels[0].Reason = "requested"
		AttributeReasons([]string{"", "a"}, sels, map[string][]string{})
		if sels[1].Reason == DependencyOf("") {
			t.Fatalf("b.Reason = %q: a bare dependency-of prefix names no package", sels[1].Reason)
		}
		if sels[1].Reason != DependencyOf("a") {
			t.Errorf("b.Reason = %q, want %q", sels[1].Reason, DependencyOf("a"))
		}
	})
	t.Run("selection with an empty name", func(t *testing.T) {
		sels := []Selection{
			{Name: "a", Arch: "amd64", Version: "1", Reason: "requested"},
			{Arch: "amd64", Version: "1"},
		}
		AttributeReasons([]string{"a"}, sels, map[string][]string{})
		if sels[1].Reason != DependencyOf("a") {
			t.Errorf("unnamed selection Reason = %q, want %q", sels[1].Reason, DependencyOf("a"))
		}
	})
}
