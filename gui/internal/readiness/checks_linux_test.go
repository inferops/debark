//go:build linux

package readiness

import (
	"context"
	"strings"
	"testing"
)

// TestDecideAPT covers the check the shipping story rests on. A Debian or
// Ubuntu host builds with no prerequisites at all, so a green row here is the
// zero-setup path — and its absence still must not block, because a container
// runs the target's own apt perfectly well.
func TestDecideAPT(t *testing.T) {
	cases := []struct {
		name         string
		obs          aptObservation
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
	}{
		{
			name: "a Debian or Ubuntu host",
			obs: aptObservation{
				AptGetPath: "/usr/bin/apt-get", DpkgPath: "/usr/bin/dpkg", Version: "2.7.14",
				VersionOut: CommandOutput{Stdout: "apt 2.7.14 (amd64)\n"},
			},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "apt 2.7.14 is available natively",
		},
		{
			name:         "a Fedora or Arch host",
			obs:          aptObservation{},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "no apt of its own",
		},
		{
			name:         "dpkg missing",
			obs:          aptObservation{AptGetPath: "/usr/bin/apt-get", Version: "2.7.14"},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "dpkg is missing",
		},
		{
			name:         "apt-get missing",
			obs:          aptObservation{DpkgPath: "/usr/bin/dpkg"},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "apt-get is missing",
		},
		{
			name: "apt-get held by the dpkg lock",
			obs: aptObservation{
				AptGetPath: "/usr/bin/apt-get", DpkgPath: "/usr/bin/dpkg",
				VersionOut: CommandOutput{TimedOut: true, ExitCode: -1},
			},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "did not answer within the timeout",
		},
		{
			name: "apt-get says something unexpected",
			obs: aptObservation{
				AptGetPath: "/usr/bin/apt-get", DpkgPath: "/usr/bin/dpkg",
				VersionOut: CommandOutput{ExitCode: 1, Stderr: "segfault"},
			},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "did not report a version",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideAPT(c.obs)
			assertRow(t, got, c.wantStatus, c.wantSeverity, c.wantMentions, false)
			if got.Severity == SeverityBlocking {
				t.Error("a missing apt must never block on its own: a container runs the target's apt instead")
			}
		})
	}
}

func TestProbeAPTReadsTheVersion(t *testing.T) {
	fr := newFakeRunner(map[string]CommandOutput{
		"apt-get": {Stdout: "apt 2.7.14 (amd64)\nSupported modules:\n*Ver: Standard .deb\n"},
	})
	o := Options{Runner: fr.run, LookPath: lookPathIn("apt-get", "dpkg")}.withDefaults()

	obs := probeAPT(context.Background(), o)
	if obs.Version != "2.7.14" {
		t.Errorf("Version = %q, want 2.7.14", obs.Version)
	}
	if obs.AptGetPath == "" || obs.DpkgPath == "" {
		t.Errorf("obs = %+v, want both paths", obs)
	}
}

func TestProbeAPTSkipsTheVersionWithoutAptGet(t *testing.T) {
	fr := newFakeRunner(nil)
	o := Options{Runner: fr.run, LookPath: lookPathIn("dpkg")}.withDefaults()

	obs := probeAPT(context.Background(), o)
	if len(fr.seen()) != 0 {
		t.Errorf("ran %v with no apt-get to run", fr.seen())
	}
	if obs.AptGetPath != "" {
		t.Errorf("AptGetPath = %q, want empty", obs.AptGetPath)
	}
}

func TestNoBuildEnvironmentRemedyIsActionable(t *testing.T) {
	r := noBuildEnvironmentRemedy()
	if !strings.Contains(r, "Docker") || !strings.Contains(r, "Debian") {
		t.Errorf("remedy %q must name both routes out", r)
	}
}

func TestFreeSpaceOnThisFilesystem(t *testing.T) {
	free, total, err := freeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("freeSpace: %v", err)
	}
	if total == 0 || free > total {
		t.Errorf("free = %d, total = %d", free, total)
	}
}

func TestFreeSpaceRejectsAMissingPath(t *testing.T) {
	if _, _, err := freeSpace("/definitely/not/a/path/9f2b"); err == nil {
		t.Error("freeSpace must report an error rather than a made-up number")
	}
}
