package apt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

// requireLinuxAPT is the contract brief's standard guard: apt-backed tests
// skip cleanly everywhere except a Linux run with DEBARK_E2E=1 set (see
// hack/linux-test.sh).
func requireLinuxAPT(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("DEBARK_E2E") == "" {
		t.Skip("needs Linux with apt; set DEBARK_E2E=1 (see hack/linux-test.sh)")
	}
}

// TestE2E_LocalBackend_ResolveJqTree is the definition-of-done scenario from the contract brief: a private root built from the captured Debian 12 snapshot resolves
// jq (plus tree, matching the recorded prototype baseline in
// docs/dev/prototype-baseline.md) and produces a Plan whose Install set apt
// itself accepts in a closed-world check. It runs real apt-get against the
// real Debian archive (network required) and the real fixture in
// testdata/real-targets/debian-12-state.tar.gz.
func TestE2E_LocalBackend_ResolveJqTree(t *testing.T) {
	requireLinuxAPT(t)

	snap, filesDir := debian12Fixture(t)
	work := t.TempDir()
	backend := newLocalBackend(LocalOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	plan, err := backend.Resolve(ctx, ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"jq", "tree"},
		ArchivesDir:      filepath.Join(work, "archives"),
		WorkDir:          work,
		Recommends:       true,
		PhasedPolicy:     snap.PhasedPolicyFor(),
		DownloadOnly:     true,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var names []string
	byName := map[string]resolve.Selection{}
	for _, s := range plan.Selections {
		names = append(names, s.Name)
		byName[s.Name] = s
	}
	sort.Strings(names)
	want := []string{"jq", "libjq1", "libonig5", "tree"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("Selections = %v, want exactly %v (recorded prototype baseline: docs/dev/prototype-baseline.md)", names, want)
	}

	for _, s := range plan.Selections {
		if len(s.SHA256) != 64 {
			t.Errorf("%s: SHA256 = %q, want 64 lowercase hex chars", s.Name, s.SHA256)
		}
		if s.StagedPath == "" {
			t.Errorf("%s: no StagedPath", s.Name)
		} else if fi, err := os.Stat(s.StagedPath); err != nil || fi.Size() != s.Size {
			t.Errorf("%s: StagedPath %s: stat err=%v, want size %d", s.Name, s.StagedPath, err, s.Size)
		}
		if s.PublisherVerification != lock.VerifiedAPTSigned {
			t.Errorf("%s: PublisherVerification = %q, want apt-signed", s.Name, s.PublisherVerification)
		}
		if s.URI == "" {
			t.Errorf("%s: URI is empty", s.Name)
		}
		if s.Arch != "amd64" {
			t.Errorf("%s: Arch = %q, want amd64 (fixture's target arch)", s.Name, s.Arch)
		}
	}

	if byName["jq"].Reason != lock.ReasonRequested {
		t.Errorf("jq.Reason = %q, want %q", byName["jq"].Reason, lock.ReasonRequested)
	}
	if byName["tree"].Reason != lock.ReasonRequested {
		t.Errorf("tree.Reason = %q, want %q", byName["tree"].Reason, lock.ReasonRequested)
	}
	if got, want := byName["libjq1"].Reason, resolve.DependencyOf("jq"); got != want {
		t.Errorf("libjq1.Reason = %q, want %q (jq -> libjq1)", got, want)
	}
	if got, want := byName["libonig5"].Reason, resolve.DependencyOf("jq"); got != want {
		t.Errorf("libonig5.Reason = %q, want %q (jq -> libjq1 -> libonig5)", got, want)
	}

	if len(plan.Install) != 4 {
		t.Fatalf("Install = %v, want 4 entries", plan.Install)
	}
	for _, entry := range plan.Install {
		if !strings.Contains(entry, ":") || !strings.Contains(entry, "=") {
			t.Errorf("Install entry %q is not in the required name:arch=version form", entry)
		}
	}
	if plan.Resolver.Backend != lock.BackendLocal {
		t.Errorf("Resolver.Backend = %q", plan.Resolver.Backend)
	}
	if plan.Resolver.APTVersion == "" {
		t.Error("Resolver.APTVersion is empty")
	}
	for _, o := range plan.Resolver.APTOptions {
		if strings.HasPrefix(o, "Dir::Etc::main=") || strings.HasPrefix(o, "Dir::Etc::parts=") {
			t.Errorf("Resolver.APTOptions should never carry the no-op -o form: %q", o)
		}
	}

	// --- Closed-world check against a minimal, self-built repo. ---
	// The real Packages/Release writer is the (core/repository); this
	// builds just enough of a flat repo — a Packages file with the fields
	// this test already knows from Plan.Selections, and the real fetched
	// .deb bytes — to prove this package's own ClosedWorld mechanism (fresh
	// private root, file:// source, real apt-get -s install, digesting).
	// Depends fields are deliberately omitted: every package in the closure
	// is itself a direct install argument, so apt needs no dependency data
	// to accept the request; that is a property of this test's flat set, not
	// a hole in the closed-world mechanism (which asks apt, not this repo,
	// to catch a broken closure).
	repoDir := filepath.Join(work, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var packagesText strings.Builder
	for _, s := range plan.Selections {
		data, err := os.ReadFile(s.StagedPath)
		if err != nil {
			t.Fatalf("read staged %s: %v", s.StagedPath, err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, s.Filename), data, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&packagesText, "Package: %s\nVersion: %s\nArchitecture: %s\nFilename: %s\nSize: %d\nSHA256: %s\n\n",
			s.Name, s.Version, s.Arch, s.Filename, s.Size, s.SHA256)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "Packages"), []byte(packagesText.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	cw, err := backend.ClosedWorld(ctx, ClosedWorldInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		BundleRepoDir:    repoDir,
		Install:          plan.Install,
		WorkDir:          filepath.Join(work, "closedworld"),
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("ClosedWorld.Result = %q, want ok; detail: %s", cw.Result, cw.Detail)
	}
	if cw.CommandDigest == "" || cw.OutputDigest == "" {
		t.Errorf("ClosedWorld missing digests: %+v", cw)
	}
}
