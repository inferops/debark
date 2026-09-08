package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/test/e2e/harness"
)

// This is the `go test` entry point into the integration matrix
// (the end-to-end scenario, run per fixture file under
// fixtures/). It is guarded behind DEBARK_E2E=1 exactly like every other
// container-touching test in this tree (docs/dev/contract-brief.md), so
// `go test ./...` from the repo root never starts a container.
//
// For the full matrix (every fixture across every supported release/arch,
// bounded parallelism, a machine-readable result file), use
// `go run ./hack/matrix` instead — this file exists for local development:
// `go test ./test/e2e/ -run TestFixtures/basic-install-matrix -v` runs and
// prints one row without touching the JSON result format at all. See
// README.md for both workflows.

var runID string

// TestMain generates one run id for the whole `go test` process and sweeps
// every container that id touched when the process exits, however it exits
// — the backstop the task asks for ("leave no containers or volumes behind,
// even on failure") for the case a single test's own cleanup did not run
// (a killed process, `go test -timeout` firing mid-row).
func TestMain(m *testing.M) {
	runID = harness.NewRunID()
	code := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if n, err := harness.Sweep(ctx, runID); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: sweep after run: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "e2e: swept %d container(s) a test's own cleanup did not remove\n", n)
	}
	os.Exit(code)
}

// TestFixtures walks every fixture under fixtures/ and runs each against its
// own default target release (harness.TargetSpec.Distro/Version/Arch) as a
// `go test` subtest — the place to debug one fixture in isolation:
//
//	DEBARK_E2E=1 go test ./test/e2e/ -run 'TestFixtures/alt-deps' -v
//
// A fixture whose target.matrix is true still only runs once here, against
// its own stated default; the full release x arch expansion is
// hack/matrix's job (task: "support --fixture NAME and --release NAME for
// running one row while debugging" is satisfied at both layers — this one
// and the standalone runner's flags).
func TestFixtures(t *testing.T) {
	harness.RequireE2E(t)

	if ok, reason := harness.DockerAvailable(context.Background()); !ok {
		t.Skipf("docker unavailable: %s", reason)
	}

	dir, err := FixturesDir()
	if err != nil {
		t.Fatalf("FixturesDir: %v", err)
	}
	fixtures, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll(%s): %v", dir, err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("no fixtures found in %s", dir)
	}

	workRoot := harness.DefaultWorkRoot()
	t.Logf("scratch directory for this run: %s", workRoot)

	for _, f := range fixtures {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			release, err := distro.Resolve(f.Target.Distro, f.Target.Version, "")
			if err != nil {
				t.Fatalf("resolve release: %v", err)
			}

			ok, reason := harness.EnsureImage(context.Background(), release.Ref(), 5*time.Minute)
			if !ok {
				t.Skipf("release image unavailable: %s", reason)
			}
			if platform, hasPlatform := distro.Platform(f.Target.Arch); hasPlatform && platform != nativePlatform() {
				if ok, reason := harness.PlatformSupported(context.Background(), platform, release.Ref(), 60*time.Second); !ok {
					t.Skipf("%s", reason)
				}
			}

			row := harness.RunFixture(context.Background(), f.Scenario, harness.RunOpts{
				RunID:                runID,
				WorkDir:              workRoot,
				Release:              release,
				Timeout:              rowTimeout,
				KeepWorkDirOnFailure: true,
			})
			reportRow(t, row)
		})
	}
}

var rowTimeout = 10 * time.Minute

func init() {
	flag.DurationVar(&rowTimeout, "e2e.row-timeout", rowTimeout, "per-fixture timeout for TestFixtures")
}

// nativePlatform names this host's own container platform, so TestFixtures
// only bothers probing emulation for an architecture that actually needs it.
// runtime.GOARCH is the harness process's own architecture (amd64 or arm64
// in every environment this runs in), which is also the Docker daemon's
// native platform on every supported development/CI host.
func nativePlatform() string {
	p, _ := distro.Platform(runtime.GOARCH)
	return p
}

func reportRow(t *testing.T, row harness.RowResult) {
	t.Helper()
	t.Logf("status=%s stage=%s total=%dms", row.Status, row.Stage, row.TotalMS)
	if row.LogPath != "" {
		t.Logf("transcript: %s", row.LogPath)
	}
	switch row.Status {
	case harness.StatusPass:
		// nothing more to say
	case harness.StatusBlocked:
		// A blocked row means a product capability this fixture needs does
		// not exist yet — expected for most of this tree today, and not
		// itself a test failure, so the CI signal
		// stays meaningful once implementations land: t.Skip, not t.Fail.
		t.Skipf("blocked at %s: %s", row.Stage, row.Blocker)
	case harness.StatusFail:
		t.Errorf("failed at %s: %s", row.Stage, row.Blocker)
	case harness.StatusSkipped:
		t.Skipf("skipped: %s", row.Blocker)
	}
}
