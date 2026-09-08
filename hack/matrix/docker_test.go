package main

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/test/e2e/harness"
)

// These two tests are the only ones in this package that start a container,
// and they run only when DEBARK_MATRIX_DOCKER=1 is set — the same shape of
// explicit opt-in as test/e2e's DEBARK_E2E guard, so `go test ./...` stays
// container-free. They are deliberately one container each and a few seconds
// long: the point is to prove the cleanup filter and the environment probe
// against a real daemon without running the matrix itself.
//
//	DEBARK_MATRIX_DOCKER=1 go test ./hack/matrix -run 'Docker' -v
func requireDockerOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv("DEBARK_MATRIX_DOCKER") != "1" {
		t.Skip("set DEBARK_MATRIX_DOCKER=1 to run the one-container checks")
	}
	if ok, reason := harness.DockerAvailable(context.Background()); !ok {
		t.Skipf("docker unavailable: %s", reason)
	}
}

func allContainerIDs(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := harness.Docker(ctx, "ps", "-aq")
	if err != nil {
		t.Fatalf("docker ps -aq: %v", err)
	}
	ids := strings.Fields(string(res.Stdout))
	sort.Strings(ids)
	return ids
}

func containerIDsForRun(t *testing.T, runID string) []string {
	t.Helper()
	filter, err := runFilter(runID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := harness.Docker(ctx, "ps", "-aq", "--filter", filter)
	if err != nil {
		t.Fatalf("docker ps -aq --filter %s: %v", filter, err)
	}
	return strings.Fields(string(res.Stdout))
}

// TestDockerSweepRemovesOnlyThisRunsContainers starts exactly one labelled
// container and proves the sweep removes it and nothing else. The
// "nothing else" half is the important half: this development host carries
// dozens of containers belonging to unrelated projects, and a broad sweep
// would destroy someone else's work.
func TestDockerSweepRemovesOnlyThisRunsContainers(t *testing.T) {
	requireDockerOptIn(t)

	runID := harness.NewRunID()
	before := allContainerIDs(t)

	args := []string{"run", "-d", "--name", "debark-e2e-sweeptest-" + runID}
	for _, l := range runLabels(runID) {
		args = append(args, "--label", l)
	}
	args = append(args, "debian:bookworm-slim", "sleep", "120")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	res, err := harness.Docker(ctx, args...)
	cancel()
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("could not start the test container: err=%v exit=%d %s", err, res.ExitCode, res.Combined())
	}
	// Whatever this test does next, the container must not survive it.
	t.Cleanup(func() { sweepRun(runID, os.Stderr) })

	if got := containerIDsForRun(t, runID); len(got) != 1 {
		t.Fatalf("filter found %d container(s), want exactly the one just started: %v", len(got), got)
	}

	var log strings.Builder
	sweepRun(runID, &log)
	if !strings.Contains(log.String(), "swept 1 container(s)") {
		t.Errorf("sweep log = %q, want it to report the one container", log.String())
	}
	if got := containerIDsForRun(t, runID); len(got) != 0 {
		t.Errorf("after the sweep the run still owns %v", got)
	}

	after := allContainerIDs(t)
	if len(after) != len(before) {
		t.Errorf("host container count changed from %d to %d: the sweep touched something it did not own",
			len(before), len(after))
	}
	beforeSet := map[string]bool{}
	for _, id := range before {
		beforeSet[id] = true
	}
	for _, id := range after {
		delete(beforeSet, id)
	}
	if len(beforeSet) != 0 {
		t.Errorf("the sweep removed containers it did not own: %v", beforeSet)
	}

	// Idempotent: a second sweep finds nothing and says nothing.
	var second strings.Builder
	sweepRun(runID, &second)
	if second.String() != "" {
		t.Errorf("second sweep printed %q, want silence", second.String())
	}
}

// TestDockerEnvPreflightOnThisHost runs the real one-container apt probe.
// On a healthy host it passes in a few seconds; on the host state that
// invalidated the 2026-09-03 run it fails inside its own budget instead of
// costing four hours.
func TestDockerEnvPreflightOnThisHost(t *testing.T) {
	requireDockerOptIn(t)

	rel, err := distro.Resolve("debian", "12", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := harness.NewRunID()
	t.Cleanup(func() { sweepRun(runID, os.Stderr) })

	start := time.Now()
	ok, detail := envPreflight(context.Background(), []Job{{Release: rel, Arch: "amd64"}}, runID, 60*time.Second, os.Stderr)
	t.Logf("env preflight: ok=%v detail=%q elapsed=%s", ok, detail, time.Since(start).Round(time.Millisecond))
	if !ok {
		t.Fatalf("environment preflight refused this host: %s\n%s", detail, envPreflightRefusal(detail, 1))
	}
	if time.Since(start) > 60*time.Second {
		t.Errorf("the probe outran its own budget (%s)", time.Since(start))
	}
	if got := containerIDsForRun(t, runID); len(got) != 0 {
		t.Errorf("the probe left containers behind: %v", got)
	}
}
