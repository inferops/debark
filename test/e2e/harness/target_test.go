package harness

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestApplyTargetStateWithSyntheticRepo is the other half of the sanity
// check syntheticdeb_test.go's TestSyntheticDebInstallsWithRealDpkg does for
// a single hand-built .deb: does a real apt, pointed at a synthetic
// repository served over HTTP from this host (httprepo.go), actually
// `apt-get update` and `apt-get install` it — including resolving a real
// archive package (jq) the synthetic package Depends on, proving both the
// Release file's own hash manifest (WriteSyntheticRepo) and the
// host.docker.internal reachability this harness's cross-container fixtures
// (stale-installed-version, third-party-repo, origin-release-pin,
// phased-updates) depend on. Guarded like every other container-touching
// test (docs/dev/contract-brief.md).
func TestApplyTargetStateWithSyntheticRepo(t *testing.T) {
	RequireE2E(t)
	ctx := context.Background()
	runID := NewRunID()
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// A discarded sweep error leaves containers behind on a shared
		// Docker host with nothing said about it — the one thing this
		// harness promises never to do.
		if _, err := Sweep(cctx, runID); err != nil {
			t.Logf("sweep run %s: %v", runID, err)
		}
	})

	ok, reason := DockerAvailable(ctx)
	if !ok {
		t.Skipf("docker unavailable: %s", reason)
	}

	work := t.TempDir()
	srv, err := StartRepoServer(work + "/repos")
	if err != nil {
		t.Fatalf("StartRepoServer: %v", err)
	}
	defer srv.Close()

	spec := TargetSpec{
		Distro: "debian", Version: "12", Arch: "amd64",
		Installed: InstalledSet{
			Repos: []SyntheticRepo{{
				Name: "acme-repo",
				Packages: []SyntheticRepoPackage{
					{Spec: SyntheticDebSpec{Name: "acme-target-demo", Version: "1.0", Depends: "jq", Bin: true}},
				},
				Release: &SyntheticRelease{Origin: "AcmeTest", Label: "Acme", Suite: "bundle", Codename: "bookworm"},
			}},
			FromRepos: []string{"acme-target-demo"},
		},
	}

	c, err := StartContainer(ctx, ContainerOpts{
		Image:  "docker.io/library/debian:bookworm-slim",
		Name:   "dfe2e-selftest-repo-" + runID,
		Labels: map[string]string{LabelMarker: "1", LabelRun: runID},
	})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	defer c.Remove(ctx)

	if err := ApplyTargetState(ctx, c, spec, work+"/repos", srv.BaseURL()); err != nil {
		t.Fatalf("ApplyTargetState: %v", err)
	}

	for _, pkg := range []string{"acme-target-demo", "jq"} {
		res, err := c.Exec(ctx, ExecOpts{}, "dpkg-query", "-W", "-f=${Status}", pkg)
		if err != nil || res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "install ok installed") {
			t.Errorf("%s not installed: err=%v res=%s", pkg, err, res)
		}
	}

	run, err := c.Exec(ctx, ExecOpts{}, "/usr/bin/acme-target-demo")
	if err != nil || run.ExitCode != 0 || !strings.Contains(string(run.Stdout), "acme-target-demo 1.0") {
		t.Errorf("installed binary did not run as expected: err=%v res=%s", err, run)
	}
}
