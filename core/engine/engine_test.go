package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/policy"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/store"
)

// TestBuild_HappyPath drives the whole pipeline against fakes and checks both
// the shape of the result and the order collaborators were called in.
func TestBuild_HappyPath(t *testing.T) {
	h := newHarness(t)

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res == nil {
		t.Fatal("Build returned a nil result")
	}

	if res.ExitClass != buildjob.ExitSuccess {
		t.Errorf("ExitClass = %q, want %q", res.ExitClass, buildjob.ExitSuccess)
	}
	if !res.Signed {
		t.Error("Signed = false, want true (a signer was configured)")
	}
	if res.Stats.PackageCount != 1 {
		t.Errorf("Stats.PackageCount = %d, want 1", res.Stats.PackageCount)
	}
	if res.BundlePath != h.outDir {
		t.Errorf("BundlePath = %q, want %q", res.BundlePath, h.outDir)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", res.Warnings)
	}
	if len(res.Unresolved) != 0 || len(res.FetchFailed) != 0 {
		t.Errorf("Unresolved/FetchFailed should be empty, got %v / %v", res.Unresolved, res.FetchFailed)
	}

	for _, name := range []string{"debark.manifest.json", "debark.manifest.sig", "evidence.json", "README.txt"} {
		if _, err := os.Stat(filepath.Join(h.outDir, name)); err != nil {
			t.Errorf("expected %s to exist in the bundle: %v", name, err)
		}
	}

	got := h.calls.list()
	want := []string{
		"snapshot.Open",
		"apt.Resolve",
		"policy.Evaluate",
		"bundle.Assemble",
		"doctor.Run",
		"apt.ClosedWorld",
		"lock.Save",
		"bundle.ReadmeText",
		"manifest.Build",
		"manifest.Save",
		"manifest.Canonical",
		"sign.Sign",
		"manifest.SaveSignature",
	}
	if len(got) != len(want) {
		t.Fatalf("call sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q (full sequence: %v)", i, got[i], want[i], got)
			break
		}
	}

	if h.signer.closed {
		t.Error("Deps.Signer was Close()d by the engine; only an engine-owned signer (built from SignerRef) should be")
	}
}

// TestBuild_PolicyDeny checks that a SeverityDeny finding aborts the build
// with dferr.Policy and that no bundle is written.
func TestBuild_PolicyDeny(t *testing.T) {
	h := newHarness(t)
	h.policyEval.findings = []policy.Finding{
		{Rule: "deny-test", Severity: policy.SeverityDeny, Message: "not allowed"},
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err == nil {
		t.Fatal("expected an error for a policy deny")
	}
	if res != nil {
		t.Fatalf("expected a nil result on policy deny, got %+v", res)
	}
	if got := dferr.ClassOf(err); got != dferr.Policy {
		t.Errorf("dferr.ClassOf(err) = %v, want %v", got, dferr.Policy)
	}
	if _, statErr := os.Stat(h.outDir); !os.IsNotExist(statErr) {
		t.Errorf("expected no bundle directory to exist, stat returned err=%v", statErr)
	}
}

// TestBuild_FetchFailureIsIncomplete checks that a failed URL download is
// reported as exit class incomplete, with the URL named in FetchFailed, and
// that the bundle is still produced.
func TestBuild_FetchFailureIsIncomplete(t *testing.T) {
	h := newHarness(t)
	const badURL = "https://example.invalid/pkg.deb"
	h.req.Inputs.URLs = []buildjob.URLInput{{URL: badURL}}
	newFetcher = func(opts fetch.Options) fetch.Fetcher {
		return &fakeFetcher{results: []fetch.Result{
			{Input: buildjob.URLInput{URL: badURL}, Err: errors.New("connection refused")},
		}}
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v (a fetch failure should still produce a BuildResult, not an error)", err)
	}
	if res.ExitClass != buildjob.ExitIncomplete {
		t.Errorf("ExitClass = %q, want %q", res.ExitClass, buildjob.ExitIncomplete)
	}
	if !containsString(res.FetchFailed, badURL) {
		t.Errorf("FetchFailed = %v, want it to contain %q", res.FetchFailed, badURL)
	}
	if _, statErr := os.Stat(filepath.Join(h.outDir, "debark.manifest.json")); statErr != nil {
		t.Errorf("bundle should still exist despite the fetch failure: %v", statErr)
	}
}

// TestBuild_ClosedWorldFailureIsReported checks that a failed closed-world
// check is never silently swallowed: it must surface as a non-success exit
// class and as a warning, on a bundle that still exists.
func TestBuild_ClosedWorldFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.backend.closedWorldFn = func(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error) {
		return lock.ClosedWorld{Result: lock.ClosedWorldFailed, Detail: "unmet dependencies offline"}, nil
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ExitClass != buildjob.ExitResolution {
		t.Errorf("ExitClass = %q, want %q (see the comment on runClosedWorld for why Resolution, not Incomplete)",
			res.ExitClass, buildjob.ExitResolution)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "closed-world") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want one mentioning the closed-world failure", res.Warnings)
	}
	if _, statErr := os.Stat(filepath.Join(h.outDir, "debark.manifest.json")); statErr != nil {
		t.Errorf("bundle should still exist: %v", statErr)
	}
}

// TestBuild_RequiredSigningNoSigner checks that Required:true with no signer
// configured anywhere is a usage error and that no bundle is produced.
func TestBuild_RequiredSigningNoSigner(t *testing.T) {
	h := newHarness(t)
	h.deps.Signer = nil
	h.req.Output.Sign = buildjob.SignOptions{Required: true}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err == nil {
		t.Fatal("expected a usage error")
	}
	if res != nil {
		t.Fatalf("expected a nil result, got %+v", res)
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("dferr.ClassOf(err) = %v, want %v", got, dferr.Usage)
	}
	if _, statErr := os.Stat(h.outDir); !os.IsNotExist(statErr) {
		t.Errorf("expected no bundle directory, stat returned err=%v", statErr)
	}
}

// TestBuild_UnsignedCarriesWarning checks that a build with no signer at all
// (and Required left false) still succeeds, but is marked unsigned and
// carries a warning saying so.
func TestBuild_UnsignedCarriesWarning(t *testing.T) {
	h := newHarness(t)
	h.deps.Signer = nil

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Signed {
		t.Error("Signed = true, want false")
	}
	if res.ExitClass != buildjob.ExitSuccess {
		t.Errorf("ExitClass = %q, want %q (unsigned is a warning, not a failure)", res.ExitClass, buildjob.ExitSuccess)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "unsigned") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want one mentioning the bundle is unsigned", res.Warnings)
	}
	if _, statErr := os.Stat(filepath.Join(h.outDir, "debark.manifest.sig")); !os.IsNotExist(statErr) {
		t.Errorf("no signature file should have been written, stat returned err=%v", statErr)
	}
}

// TestBuild_ExternalFileStaging exercises the external-input path end to
// end: a local .deb input must be fetched into the store, materialised into
// a flat staging directory, indexed there via repository.Writer, and its
// Package: name (via debPackageName) must reach apt.ResolveInput as
// ExternalNames alongside the staging directory as ExternalRepoDir.
func TestBuild_ExternalFileStaging(t *testing.T) {
	h := newHarness(t)
	h.req.Inputs.Packages = nil // exercise the external-only path in isolation

	localDeb := filepath.Join(t.TempDir(), "mytool_1.0_amd64.deb")
	if err := os.WriteFile(localDeb, []byte("fake mytool .deb bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.req.Inputs.Files = []string{localDeb}

	fromLocalFile = func(ctx context.Context, st store.Store, path string) (*fetch.Fetched, error) {
		sum, size, err := st.PutFile(ctx, path, false)
		if err != nil {
			return nil, err
		}
		return &fetch.Fetched{
			Filename: filepath.Base(path), Digest: sum, Size: size,
			Verification: lock.VerifiedURLUnverified,
		}, nil
	}
	debPackageName = func(ctx context.Context, path string) (string, error) {
		return "mytool", nil
	}

	var gotResolveInput apt.ResolveInput
	h.backend.resolveFn = func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
		gotResolveInput = in
		return fixturePlan(t), nil
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(h.repoWriter.calls) != 1 {
		t.Fatalf("expected exactly one repository.Write call for the staging repo, got %d", len(h.repoWriter.calls))
	}
	staged := h.repoWriter.calls[0]
	if len(staged.Files) != 1 || staged.Files[0].Path != "mytool_1.0_amd64.deb" {
		t.Errorf("staged repository.Input.Files = %+v, want one entry for mytool_1.0_amd64.deb", staged.Files)
	}

	if len(gotResolveInput.ExternalNames) != 1 || gotResolveInput.ExternalNames[0] != "mytool" {
		t.Errorf("ResolveInput.ExternalNames = %v, want [mytool]", gotResolveInput.ExternalNames)
	}
	if gotResolveInput.ExternalRepoDir == "" {
		t.Error("ResolveInput.ExternalRepoDir is empty, want the staging directory")
	}
	if gotResolveInput.ExternalRepoDir != staged.Dir {
		t.Errorf("ResolveInput.ExternalRepoDir = %q, want %q (the same dir repository.Writer indexed)",
			gotResolveInput.ExternalRepoDir, staged.Dir)
	}

	// The staging directory lives under the per-build temp root (workRoot),
	// which run() removes via defer once Build returns — by design, nothing
	// under it is meant to outlive the call. fakeRepoWriter.Write already
	// confirmed the staged file existed on disk at the moment it indexed it
	// (see its os.Stat check in fakes_test.go), which is the point in the
	// pipeline where that actually matters.
}

// TestBuild_PruneOnlyInUpdateMode checks the update-mode semantics end to
// end: additive runs never ask bundle.Assemble to prune, and --update
// (UpdateRefresh) with Prune:true does.
func TestBuild_PruneOnlyInUpdateMode(t *testing.T) {
	t.Run("additive does not prune", func(t *testing.T) {
		h := newHarness(t)
		h.req.Options.UpdateMode = buildjob.UpdateAdditive
		h.req.Options.Prune = true // even if set, additive mode must ignore it

		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := eng.Build(context.Background(), h.req); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(h.assembleInputs) != 1 {
			t.Fatalf("expected exactly one bundle.Assemble call, got %d", len(h.assembleInputs))
		}
		if h.assembleInputs[0].Prune {
			t.Error("additive mode: Input.Prune = true, want false")
		}
	})

	t.Run("update with prune requested does prune", func(t *testing.T) {
		h := newHarness(t)
		h.req.Options.UpdateMode = buildjob.UpdateRefresh
		h.req.Options.Prune = true

		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := eng.Build(context.Background(), h.req); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(h.assembleInputs) != 1 {
			t.Fatalf("expected exactly one bundle.Assemble call, got %d", len(h.assembleInputs))
		}
		if !h.assembleInputs[0].Prune {
			t.Error("refresh mode with Prune:true: Input.Prune = false, want true")
		}
	})

	t.Run("update without prune requested does not prune", func(t *testing.T) {
		h := newHarness(t)
		h.req.Options.UpdateMode = buildjob.UpdateRefresh
		h.req.Options.Prune = false // --update --no-prune

		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := eng.Build(context.Background(), h.req); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if h.assembleInputs[0].Prune {
			t.Error("refresh mode with Prune:false: Input.Prune = true, want false")
		}
	})
}

// The closed-world check's only path input has to be absolute: the container
// backend bind-mounts it and refuses a relative path outright, while the
// local backend absolutises it itself, so a relative one made the same build
// pass on one backend and fail on the other. --out's default is the relative
// "bundle", so this was the ordinary case, not an edge one.
func TestBundleRepoPathIsAlwaysAbsolute(t *testing.T) {
	for _, dir := range []string{
		"bundle",
		filepath.Join(".", "out", "site-42"),
		t.TempDir(),
	} {
		b := &build{bundleDir: dir}
		got := b.bundleRepoPath()
		if !filepath.IsAbs(got) {
			t.Errorf("bundleRepoPath() = %q for bundleDir %q, want an absolute path", got, dir)
		}
		if !strings.HasSuffix(got, filepath.FromSlash("/repo")) {
			t.Errorf("bundleRepoPath() = %q, want it to end at the bundle's repo/ directory", got)
		}
	}
}

// ...and it must not change what the build reports it wrote, which is the
// operator's own spelling of --out.
func TestBuildResultKeepsTheOperatorsOwnBundlePath(t *testing.T) {
	h := newHarness(t)
	var seen string
	h.backend.closedWorldFn = func(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error) {
		seen = in.BundleRepoDir
		return lock.ClosedWorld{Result: lock.ClosedWorldOK}, nil
	}
	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !filepath.IsAbs(seen) {
		t.Errorf("ClosedWorldInput.BundleRepoDir = %q, want absolute", seen)
	}
	if res.BundlePath != h.req.Output.Path {
		t.Errorf("BundlePath = %q, want the requested %q unchanged", res.BundlePath, h.req.Output.Path)
	}
}
