package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

// The vendor URL and the local .deb used by both tests below.
const (
	vendorURL     = "https://vendor.example/downloads/acme-agent_1.0_amd64.deb"
	urlDebName    = "acme-agent"
	localDebName  = "localtool"
	externalArch  = "amd64"
	externalVer   = "1.0"
	externalDebFn = "_1.0_amd64.deb"
)

// stageExternalInputs wires the harness up with one URL input and one local
// file input, both real .deb files, and returns nothing the test then has to
// remember: everything a test needs to assert on comes back out of the
// pipeline.
//
// The URL is the only faked boundary — a real download would need a network,
// which no test in this project has. Everything downstream of the fetch is
// the production path: fetch.FromLocalFile for the local input (the harness
// does not override fromLocalFile), the real dpkgDebPackageName reading each
// staged .deb's own control field, and the real staging/indexing in
// stageExternals.
func stageExternalInputs(t *testing.T, h *harness) {
	t.Helper()
	h.req.Inputs.Packages = nil // externals only, so the plan is unambiguous

	dir := t.TempDir()

	localPath := filepath.Join(dir, localDebName+externalDebFn)
	if err := os.WriteFile(localPath, realDebBytes(t, localDebName, externalVer, externalArch), 0o644); err != nil {
		t.Fatal(err)
	}
	h.req.Inputs.Files = []string{localPath}

	// The URL's bytes have to be in the store before stageExternals
	// materialises them, exactly as a real fetch would have put them there.
	urlPath := filepath.Join(dir, urlDebName+externalDebFn)
	if err := os.WriteFile(urlPath, realDebBytes(t, urlDebName, externalVer, externalArch), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := h.store.PutFile(context.Background(), urlPath, false)
	if err != nil {
		t.Fatalf("seed the store with the fetched bytes: %v", err)
	}
	h.req.Inputs.URLs = []buildjob.URLInput{{URL: vendorURL}}
	newFetcher = func(opts fetch.Options) fetch.Fetcher {
		return &fakeFetcher{results: []fetch.Result{{
			Input: buildjob.URLInput{URL: vendorURL},
			Fetched: &fetch.Fetched{
				// Redacted, like the real fetcher's own Fetched.URL. The
				// engine must NOT be relying on this field: it records the
				// operator's literal input instead (stageExternals).
				URL: fetch.RedactURL(vendorURL), Filename: urlDebName + externalDebFn,
				Digest: sum, Size: size, Verification: lock.VerifiedURLUnverified,
			},
		}}}
	}

	// The backend stands in for core/apt, and it is written to report
	// EXACTLY what the real local backend reports for an external: a file:
	// URI under this build's temporary work root, from apt's own
	// --print-uris over the "deb [trusted=yes] file://... ./" staging
	// source (core/apt/local.go). It never sees, and so never returns, the
	// operator's vendor URL — which is the entire defect. A fake that set
	// URI to the vendor URL here would be the same fake that let this ship.
	h.backend.resolveFn = func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
		if in.ExternalRepoDir == "" {
			t.Error("ResolveInput.ExternalRepoDir is empty: nothing was staged")
		}
		var sels []resolve.Selection
		var install []string
		for _, name := range in.ExternalNames {
			filename := name + externalDebFn
			staged := filepath.Join(in.ExternalRepoDir, filename)
			sha, sz, derr := digest.SHA256File(staged)
			if derr != nil {
				return nil, derr
			}
			sels = append(sels, resolve.Selection{
				Name: name, Arch: externalArch, Version: externalVer, SourcePackage: name,
				Filename: filename, Size: sz, SHA256: sha,
				URI:                   "file://" + filepath.ToSlash(staged),
				Reason:                lock.ReasonExternal,
				UserSupplied:          true,
				Flags:                 []string{lock.FlagUserSupplied},
				PublisherVerification: lock.VerifiedURLUnverified,
				StagedPath:            staged,
			})
			install = append(install, name+":"+externalArch+"="+externalVer)
		}
		return &resolve.Plan{
			Target: lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: externalArch},
			Resolver: lock.Resolver{
				Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
				PhasedUpdates: "never-include", InstallRecommends: true,
			},
			Selections: sels, Install: install,
		}, nil
	}
}

func selectionNamed(t *testing.T, plan *resolve.Plan, name string) resolve.Selection {
	t.Helper()
	if plan == nil {
		t.Fatal("no plan reached the policy evaluator")
	}
	for _, s := range plan.Selections {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no selection named %q in the plan (%d selections)", name, len(plan.Selections))
	return resolve.Selection{}
}

// TestBuild_URLInputProvenanceSurvivesResolution pins the fact that makes a
// documented security control work at all: after resolution, a package that
// came from a vendor URL must still be distinguishable from one that came off
// the operator's disk.
//
// It cannot be, on the backend's evidence alone. Both arrive at apt as files
// in the same file:// staging repository, so both come back carrying a file:
// URI under this build's work root. The operator's URL is known only to the
// engine, only before the fetch — and was dropped there. See
// attributeExternalOrigins (inputs.go).
//
// Nothing in this test hands the engine the answer: the URL is supplied where
// an operator supplies it (BuildRequest.Inputs.URLs), the fetch is the only
// faked step, and the fake backend deliberately reports the work-root file:
// URI production reports. The assertions on the file: URI being GONE are what
// stop this passing on a plan that was never touched.
func TestBuild_URLInputProvenanceSurvivesResolution(t *testing.T) {
	h := newHarness(t)
	stageExternalInputs(t, h)

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	plan := h.policyEval.gotInput.Plan
	if plan == nil || len(plan.Selections) != 2 {
		t.Fatalf("expected both externals to reach policy, got %+v", plan)
	}

	fromURL := selectionNamed(t, plan, urlDebName)
	if fromURL.Reason != lock.ReasonExternal {
		t.Fatalf("premise broken: %s Reason = %q, want %q", urlDebName, fromURL.Reason, lock.ReasonExternal)
	}
	if fromURL.URI != vendorURL {
		t.Errorf("%s Selection.URI = %q, want the operator's URL %q", urlDebName, fromURL.URI, vendorURL)
	}
	if fromURL.Origin.URI != vendorURL {
		t.Errorf("%s Origin.URI = %q, want %q (this is the field that reaches lock.json)",
			urlDebName, fromURL.Origin.URI, vendorURL)
	}
	if strings.HasPrefix(fromURL.URI, "file:") {
		t.Errorf("%s still carries the staging file: URI: %q", urlDebName, fromURL.URI)
	}

	fromFile := selectionNamed(t, plan, localDebName)
	if fromFile.Reason != lock.ReasonExternal {
		t.Fatalf("premise broken: %s Reason = %q, want %q", localDebName, fromFile.Reason, lock.ReasonExternal)
	}
	if fromFile.URI != "" {
		t.Errorf("%s Selection.URI = %q, want empty (Selection.URI: \"Empty for a local file input\")",
			localDebName, fromFile.URI)
	}
	if fromFile.Origin.URI != "" {
		t.Errorf("%s Origin.URI = %q, want empty", localDebName, fromFile.Origin.URI)
	}
	// Not populated on purpose: it is the operator's host path, and lock.json
	// is an artefact whose digest becomes the bundle id. See
	// attributeExternalOrigins.
	if fromFile.Origin.LocalPath != "" {
		t.Errorf("%s Origin.LocalPath = %q, want empty (a host path must not reach lock.json)",
			localDebName, fromFile.Origin.LocalPath)
	}
}

// TestBuild_AllowURLInputsFalseDeniesOnlyTheURLInput drives the same fix
// through the control it exists for, with the REAL policy evaluator loaded
// from a real policy file.
//
// allow_url_inputs: false is an operator's switch for "this bundle may not
// contain anything pulled off the network". With the vendor URL dropped
// during staging it matched nothing and the build passed clean, which is the
// worst possible failure for a guardrail: silent, and in the permissive
// direction.
//
// The second half is as important as the first. The rule must NOT fire on the
// local --file input, which examples/policy.yaml expects to keep working;
// denying every external would trade a silent allow for spurious exit-6
// denials on a perfectly ordinary vendor .deb handed over on a USB stick.
func TestBuild_AllowURLInputsFalseDeniesOnlyTheURLInput(t *testing.T) {
	h := newHarness(t)
	stageExternalInputs(t, h)

	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, []byte(
		`{"schema_version":"debark.policy/v1","allow_url_inputs":false,"default_severity":"deny"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The real evaluator, not the harness fake: this test is about whether
	// the rule can see what it needs to see.
	h.deps.Policy = nil
	h.req.Options.PolicyRef = policyPath

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err == nil {
		t.Fatal("allow_url_inputs: false did not deny a build containing a URL input")
	}
	if res != nil {
		t.Fatalf("expected a nil result on a policy deny, got %+v", res)
	}
	if got := dferr.ClassOf(err); got != dferr.Policy {
		t.Errorf("dferr.ClassOf(err) = %v, want %v (err: %v)", got, dferr.Policy, err)
	}
	if !strings.Contains(err.Error(), urlDebName) {
		t.Errorf("the denial does not name the URL-sourced package %q: %v", urlDebName, err)
	}
	if strings.Contains(err.Error(), localDebName) {
		t.Errorf("the denial also names the LOCAL file input %q; allow_url_inputs must not deny --file inputs: %v",
			localDebName, err)
	}
	if _, statErr := os.Stat(h.outDir); !os.IsNotExist(statErr) {
		t.Errorf("a policy deny must write no bundle, stat returned err=%v", statErr)
	}
}
