package engine

import (
	"strings"
	"testing"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/lock"
)

// lockWithHostPathsEverywhere builds a lock in which every string field a
// collaborator can populate carries the build's temporary work root or its
// output directory — the two paths the engine itself creates and therefore
// the two it is responsible for keeping out of the artefact.
func lockWithHostPathsEverywhere(workRoot, bundleDir string) *lock.Lock {
	return &lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      "2026-01-01T00:00:00Z",
		SnapshotDigest: "aa",
		RequestDigest:  "bb",
		Target:         lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal,
			// What an unmasking backend hands back: the real apt argv.
			APTOptions: []string{
				"Dir::Etc::sourcelist=" + workRoot + "/aptroot/etc/apt/sources.list",
				"Dir::Cache::archives=" + workRoot + "/archives",
				"APT::Architecture=amd64",
			},
		},
		Packages: []lock.Package{{
			Name: "vlc", Arch: "amd64", Version: "1.0", Filename: "pool/v/vlc/vlc_1.0_amd64.deb",
			SHA256: strings.Repeat("a", 64),
			Origin: lock.Origin{URI: "file://" + bundleDir + "/repo"},
			Reason: lock.ReasonRequested,
		}},
		Install: []string{"vlc:amd64=1.0"},
		ClosedWorld: lock.ClosedWorld{
			Result: lock.ClosedWorldFailed,
			// What runClosedWorld folds in when the backend call fails
			// outright: err.Error(), which names the private root.
			Detail: "apt: private root: create " + workRoot + "/closed-world: permission denied",
		},
		Warnings: []lock.Warning{{
			Code:    "private-root.dropped-conf",
			Message: "dropped " + workRoot + "/aptroot/etc/apt/apt.conf.d/99proxy",
		}},
		Unresolved: []lock.Unresolved{{
			Input:  "acme-agent",
			Kind:   "package",
			Detail: "E: Unable to locate package in " + bundleDir + "/repo",
		}},
		Stats: lock.Stats{Bytes: 100, DownloadedBytes: 100, Added: 1, Unchanged: 2},
	}
}

// TestMakeLockReproducible_ScrubsEveryHostPathInTheLock proves the engine's
// last-line-of-defence scrub reaches every string in the document, not just
// the two or three fields anyone remembered to list. The strings here are the
// real shapes: an unmasked apt argv from a backend, an err.Error() folded in
// as ClosedWorld.Detail, a private-root warning and apt's own Unresolved
// text.
func TestMakeLockReproducible_ScrubsEveryHostPathInTheLock(t *testing.T) {
	const workRoot = "/tmp/debark-build-3141592653"
	const bundleDir = "/home/alice/out/bundle"

	b := &build{
		workRoot:  workRoot,
		bundleDir: bundleDir,
		lockDoc:   lockWithHostPathsEverywhere(workRoot, bundleDir),
	}
	b.makeLockReproducible()

	// Nothing anywhere in the document may still name either path. Rendering
	// the whole lock and searching it is deliberate: a field-by-field
	// assertion would have exactly the blind spot this scrub exists to close.
	rendered := renderLockForTest(t, b.lockDoc)
	for _, host := range []string{workRoot, bundleDir} {
		if strings.Contains(rendered, host) {
			t.Errorf("%q still appears in the scrubbed lock:\n%s", host, rendered)
		}
	}
	for _, want := range []string{workRootPlaceholder, bundleDirPlaceholder} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected %q to appear in the scrubbed lock (the placeholder must replace the path, not delete the value):\n%s", want, rendered)
		}
	}

	// The shape around the masked prefix must survive: an auditor still sees
	// which apt options were set and what the failure was.
	if got := b.lockDoc.Resolver.APTOptions[0]; got != "Dir::Etc::sourcelist="+workRootPlaceholder+"/aptroot/etc/apt/sources.list" {
		t.Errorf("APTOptions[0] = %q; only the varying prefix may be replaced", got)
	}
	if got := b.lockDoc.Resolver.APTOptions[2]; got != "APT::Architecture=amd64" {
		t.Errorf("APTOptions[2] = %q; an option with no path in it must be untouched", got)
	}
	if !strings.Contains(b.lockDoc.ClosedWorld.Detail, "permission denied") {
		t.Errorf("ClosedWorld.Detail lost its reason: %q", b.lockDoc.ClosedWorld.Detail)
	}
}

// TestMakeLockReproducible_DropsCacheDependentStats: DownloadedBytes counts
// what the machine's persistent content store did not already hold, so the
// same build run twice on one machine recorded N and then 0. It is reported
// in BuildResult instead; the lock records only what is a function of the
// request. Bytes, Added, Removed and Unchanged are NOT touched — those are
// computed against the output directory's own previous contents, which an
// auditor rebuilding from scratch has empty.
func TestMakeLockReproducible_DropsCacheDependentStats(t *testing.T) {
	b := &build{
		workRoot:  "/tmp/debark-build-1",
		bundleDir: "/out",
		lockDoc:   lockWithHostPathsEverywhere("/tmp/debark-build-1", "/out"),
	}
	b.makeLockReproducible()

	if got := b.lockDoc.Stats.DownloadedBytes; got != 0 {
		t.Errorf("Stats.DownloadedBytes = %d, want 0: it is a property of the machine's store, not the request", got)
	}
	if got := b.lockDoc.Stats.Bytes; got != 100 {
		t.Errorf("Stats.Bytes = %d, want 100: the bundle's own size is a function of the request and must survive", got)
	}
	if b.lockDoc.Stats.Added != 1 || b.lockDoc.Stats.Unchanged != 2 {
		t.Errorf("Stats added/unchanged were cleared (%+v); only DownloadedBytes is cache-dependent", b.lockDoc.Stats)
	}
}

// TestScrubHostPaths_EmptyPathIsIgnored: an empty replacement key would match
// at every position and destroy the document. b.workRoot and b.bundleDir are
// always set by the time finalizeBundle runs, but a future caller's mistake
// must not corrupt a lock.
func TestScrubHostPaths_EmptyPathIsIgnored(t *testing.T) {
	l := lockWithHostPathsEverywhere("/tmp/wr", "/out")
	before := renderLockForTest(t, l)
	scrubHostPaths(l, map[string]string{"": "<NOTHING>"})
	if got := renderLockForTest(t, l); got != before {
		t.Errorf("an empty path key changed the lock:\n before=%s\n after =%s", before, got)
	}
}

func renderLockForTest(t *testing.T, l *lock.Lock) string {
	t.Helper()
	b, err := canonical.Marshal(l)
	if err != nil {
		t.Fatalf("canonical.Marshal: %v", err)
	}
	return string(b)
}
