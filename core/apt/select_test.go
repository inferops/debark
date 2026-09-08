package apt

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// fakeVersionRunner answers only "-v" (what Probe uses); everything else is
// unused by these tests and returns a zero Output.
type fakeVersionRunner struct {
	out Output
	err error
}

func (f fakeVersionRunner) Run(ctx context.Context, opts []string, args ...string) (Output, error) {
	if len(args) == 1 && args[0] == "-v" {
		return f.out, f.err
	}
	return Output{}, nil
}

func writeOSRelease(t *testing.T, id, versionID string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "os-release")
	content := "ID=" + id + "\nVERSION_ID=\"" + versionID + "\"\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// withFakeHostSolver installs a deterministic hostSolverProbeFunc for the
// duration of one test, restoring the real one (probeHostSolver) on
// cleanup. Every test in this file that reaches localMismatchReason must set
// this: selectBackendWithRunner calls it unconditionally, for every
// requested backend, before the switch on what was actually asked for, so a
// test that forgets this would otherwise depend on whatever `apt-config` (if
// any) happens to be on the machine running the test -- exactly the
// real-apt dependency the contract brief's Windows-safe rule exists to rule
// out.
//
// configured is not a detail: it is the difference between "apt reported an
// APT::Solver key" and "apt reported none, so this is its own compiled-in
// default". The gate treats those two observations differently on purpose
// (solverProbe, probeMeasuresTargetDefault), so a case that sets the wrong
// one is testing a different scenario than its name claims.
func withFakeHostSolver(t *testing.T, solver string, configured, ok bool) {
	t.Helper()
	orig := hostSolverProbeFunc
	hostSolverProbeFunc = func(ctx context.Context) solverProbe {
		return solverProbe{Solver: solver, Configured: configured, OK: ok}
	}
	t.Cleanup(func() { hostSolverProbeFunc = orig })
}

func TestSelectBackend(t *testing.T) {
	origOSRelease := localOSReleasePath
	t.Cleanup(func() { localOSReleasePath = origOSRelease })

	// Selection logic must not depend on whether a real docker/podman is
	// actually installed on the machine running these tests: substitute a
	// deterministic "unavailable" container probe (the own container
	// backend tests cover its real availability logic).
	origProbeContainer := probeContainerFunc
	probeContainerFunc = func(ctx context.Context, sel Selection) (Backend, Capabilities) {
		return nil, Capabilities{
			Available: false,
			Reason:    "container backend unavailable in this build",
			Hint:      "install docker or podman",
		}
	}
	t.Cleanup(func() { probeContainerFunc = origProbeContainer })

	envUnavailable := dferr.New(dferr.Environment, "apt-get not found")

	cases := []struct {
		name          string
		hostID        string
		hostVersionID string
		runner        Runner
		// hostSolver/hostSolverConfigured/hostSolverOK stand in for a live
		// `apt-config dump` probe (see withFakeHostSolver): every case must
		// set them, even ones not focused on solver logic, since
		// localMismatchReason calls hostSolverProbeFunc unconditionally.
		// hostSolverConfigured false means "apt reported no APT::Solver key
		// at all", the stock shape on every release that does not ship one.
		hostSolver           string
		hostSolverConfigured bool
		hostSolverOK         bool
		requested            lock.Backend
		target               snapshot.Target
		wantErr              bool
		wantErrClass         dferr.Class
		wantKind             lock.Backend
	}{
		{
			name:   "auto selects local when host matches target (same version, same solver)",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested: "",
			target:    snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantKind:  lock.BackendLocal,
		},
		{
			// REGRESSION, measured 2026-09-05: the whole
			// basic-install-matrix Debian 13 row (amd64 and arm64) failed
			// here with exit 2, on a builder that WAS the target image.
			// debian:trixie-slim runs apt 3.0.3 and its `apt-config dump`
			// contains no APT::Solver key at all, so the live probe says
			// "internal" while defaultSolverForAPTVersion's "major >= 3"
			// rule guesses "3.0". The gate compared the guess against the
			// measurement and refused the local backend for a Debian 13
			// target on a Debian 13 host.
			name:   "auto selects local for a Debian 13 target on a Debian 13 host, though the apt-version table guesses solver3 for apt 3.0.3",
			hostID: "debian", hostVersionID: "13",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 3.0.3 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverConfigured: false, hostSolverOK: true,
			requested: "",
			target:    snapshot.Target{DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			wantKind:  lock.BackendLocal,
		},
		{
			// Same measurement, reached through --backend=local: the matrix
			// row's exit-2 message was "apt: local backend unavailable",
			// which is this path.
			name:   "explicit local requested for a Debian 13 target on a Debian 13 host is not refused",
			hostID: "debian", hostVersionID: "13",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 3.0.3 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverConfigured: false, hostSolverOK: true,
			requested: lock.BackendLocal,
			target:    snapshot.Target{DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			wantKind:  lock.BackendLocal,
		},
		{
			name:   "auto falls to container (unavailable) when apt is missing on the host",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{err: envUnavailable},
			hostSolver: solverInternal, hostSolverOK: true,
			requested:    "",
			target:       snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "auto falls to container (unavailable) on distro id mismatch",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested:    "",
			target:       snapshot.Target{DistroID: "ubuntu", VersionID: "24.04", APTVersion: "2.8.3"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			// The fix this file exists to prove: E2 measured zero genuine
			// selection divergence when only apt major.minor differs and
			// the solver is held fixed (25.10-default vs 26.04-default).
			// Before this fix, this exact case (apt major.minor mismatch,
			// same underlying solver) was refused; it must now succeed.
			name:   "auto selects local when apt major.minor differs but the effective solver matches (E2 version-only comparison)",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.9.5 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested: "",
			target:    snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantKind:  lock.BackendLocal,
		},
		{
			// The new primary gate: same apt major.minor is not enough by
			// itself once the probed solver actually differs -- exactly
			// what -o APT::Solver=internal demonstrated in E2 (same apt
			// binary, forced onto a different solver).
			name:   "auto falls to container (unavailable) when the effective solver differs even though apt major.minor matches",
			hostID: "ubuntu", hostVersionID: "26.04",
			runner: fakeVersionRunner{out: Output{Stdout: []byte("apt 3.2.0 (amd64)\n")}},
			// Configured: the host's apt.conf.d really does force
			// APT::Solver=internal, so the dump carries the key. That is a
			// fact about this machine only, and 26.04's own default remains
			// solver3 (E2, measured) -- a genuine divergence the gate must
			// still catch.
			hostSolver: solverInternal, hostSolverConfigured: true, hostSolverOK: true,
			requested:    "",
			target:       snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"}, // expects solver3 by default
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "auto falls to container (unavailable) when both apt version and solver differ (E2 version+solver-together comparison)",
			hostID: "ubuntu", hostVersionID: "24.04",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.8.3 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested:    "",
			target:       snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "auto falls to container (unavailable) when the host solver cannot be confirmed",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: "", hostSolverOK: false,
			requested:    "",
			target:       snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "explicit local requested and available",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested: lock.BackendLocal,
			target:    snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantKind:  lock.BackendLocal,
		},
		{
			name:   "explicit local requested, apt major.minor differs but solver matches -- allowed",
			hostID: "ubuntu", hostVersionID: "25.10",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 3.1.6ubuntu2 (amd64)\n")}},
			hostSolver: solver3, hostSolverOK: true,
			requested: lock.BackendLocal,
			target:    snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			wantKind:  lock.BackendLocal,
		},
		{
			name:   "explicit local requested but distro mismatched is refused, not silently swapped",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested:    lock.BackendLocal,
			target:       snapshot.Target{DistroID: "ubuntu", VersionID: "24.04", APTVersion: "2.8.3"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "explicit local requested but solver mismatched is refused, not silently swapped",
			hostID: "ubuntu", hostVersionID: "26.04",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 3.2.0 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverConfigured: true, hostSolverOK: true,
			requested:    lock.BackendLocal,
			target:       snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "explicit container requested is unavailable in this build",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested:    lock.BackendContainer,
			target:       snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantErr:      true,
			wantErrClass: dferr.Environment,
		},
		{
			name:   "unknown backend name is a usage error",
			hostID: "debian", hostVersionID: "12",
			runner:     fakeVersionRunner{out: Output{Stdout: []byte("apt 2.6.1 (amd64)\n")}},
			hostSolver: solverInternal, hostSolverOK: true,
			requested:    lock.Backend("bogus"),
			target:       snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			wantErr:      true,
			wantErrClass: dferr.Usage,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			localOSReleasePath = writeOSRelease(t, c.hostID, c.hostVersionID)
			withFakeHostSolver(t, c.hostSolver, c.hostSolverConfigured, c.hostSolverOK)

			// selectBackend always constructs its own local backend
			// internally; selectBackendWithRunner is the same logic with
			// that backend's Runner overridable, so every outcome here is
			// exercised without a real apt-get on the test machine.
			backend, gotCap, err := selectBackendWithRunner(context.Background(), Selection{
				Target:    c.target,
				Requested: c.requested,
			}, c.runner)

			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got backend=%v gotCap=%+v", backend, gotCap)
				}
				if got := dferr.ClassOf(err); got != c.wantErrClass {
					t.Errorf("error class = %v, want %v (err: %v)", got, c.wantErrClass, err)
				}
				// An unavailable backend must carry a concrete-fix hint
				// (task requirement); a plain usage error like an unknown
				// backend name already states its fix inline in the message.
				if c.wantErrClass == dferr.Environment && dferr.HintOf(err) == "" {
					t.Errorf("expected a hint on the environment error, got none: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if backend == nil {
				t.Fatal("expected a non-nil backend")
			}
			if backend.Kind() != c.wantKind {
				t.Errorf("backend.Kind() = %v, want %v", backend.Kind(), c.wantKind)
			}
		})
	}
}

// TestLocalMismatchReason exercises the gate function directly, over the
// full matrix the task calls for: same/different distro, same/different apt
// major.minor, same/different effective solver. This is deliberately more
// exhaustive than TestSelectBackend above, which proves the same logic wired
// into the full selection flow (error classes, hints) for a smaller,
// representative set of cases.
func TestLocalMismatchReason(t *testing.T) {
	origHostSolver := hostSolverProbeFunc
	t.Cleanup(func() { hostSolverProbeFunc = origHostSolver })

	cases := []struct {
		name                 string
		cap                  Capabilities
		target               snapshot.Target
		hostSolver           string
		hostSolverConfigured bool
		hostSolverOK         bool
		wantBlocked          bool
	}{
		{
			name:       "same distro, same version, same solver -> local",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			hostSolver: solverInternal, hostSolverOK: true,
			wantBlocked: false,
		},
		{
			name:       "different distro -> blocked regardless of solver match",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			target:     snapshot.Target{DistroID: "ubuntu", VersionID: "24.04", APTVersion: "2.8.3"},
			hostSolver: solverInternal, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			name:       "different major.minor, same solver (E2 version-only comparison) -> local, not blocked",
			cap:        Capabilities{Available: true, DistroID: "ubuntu", VersionID: "25.10", APTVersion: "3.1.6ubuntu2"},
			target:     snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			hostSolver: solver3, hostSolverOK: true,
			wantBlocked: false,
		},
		{
			// The host's dump carries an explicit APT::Solver key
			// (hostSolverConfigured), which is what E2's own lever produces
			// and what an apt.conf.d edit produces. A configured value is a
			// fact about this host and nothing else, so it stays comparable
			// against the target's expected default -- this case must keep
			// blocking even though the two apt versions are identical.
			name:       "same major.minor, different solver (E2's -o APT::Solver=internal lever) -> blocked",
			cap:        Capabilities{Available: true, DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			target:     snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			hostSolver: solverInternal, hostSolverConfigured: true, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			// REGRESSION (measured 2026-09-05, debian:trixie-slim): apt
			// 3.0.3, `apt-config dump` has no APT::Solver key at all, so the
			// probe reports internal with Configured=false while
			// defaultSolverForAPTVersion guesses "3.0" from major >= 3.
			// Both sides are the same apt, so there is nothing here to
			// diverge -- only a wrong table entry that used to be treated as
			// the second opinion in a disagreement.
			name:       "Debian 13 host, Debian 13 target: apt 3.0.3 reports no APT::Solver, and the measurement beats the version table -> local",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			hostSolver: solverInternal, hostSolverConfigured: false, hostSolverOK: true,
			wantBlocked: false,
		},
		{
			// The same apt version pair, but now the host really did
			// configure the solver it reports. Pinning this separately is
			// what stops the fix above from degenerating into "same apt
			// version => always allow local", which would silently undo the
			// gate E2 asked for.
			name:       "same apt 3.0.3 on both sides, but the host has APT::Solver explicitly configured -> still compared, still blocked",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			hostSolver: solverInternal, hostSolverConfigured: true, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			// Point-release drift between the snapshot's recorded apt and
			// the builder's: same major.minor, so a compiled-in default
			// measured on one still describes the other (the assumption
			// E2's Limitations leave open, and a far narrower one than the
			// major-number-only table this replaces).
			name:       "Debian 13 host apt 3.0.3 against a target snapshot recorded with apt 3.0.2 -> local",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "13", APTVersion: "3.0.2"},
			hostSolver: solverInternal, hostSolverConfigured: false, hostSolverOK: true,
			wantBlocked: false,
		},
		{
			// The measurement speaks only for its own apt version. A
			// different major.minor falls back to the table, and the table
			// says solver3 for 3.2.0, so this stays blocked -- conservative
			// (the container is the safe fallback) rather
			// than assuming trixie's default generalises to every apt 3.x.
			name:       "Debian 13 host apt 3.0.3 against a different-major.minor apt 3.2.0 target -> blocked, the measurement does not generalise",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "14", APTVersion: "3.2.0"},
			hostSolver: solverInternal, hostSolverConfigured: false, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			name:       "different major.minor and different solver (E2 version+solver-together comparison) -> blocked",
			cap:        Capabilities{Available: true, DistroID: "ubuntu", VersionID: "24.04", APTVersion: "2.8.3"},
			target:     snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", APTVersion: "3.2.0"},
			hostSolver: solverInternal, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			name:       "host solver probe fails -> blocked (uncertain, prefer the container)",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			hostSolver: "", hostSolverOK: false,
			wantBlocked: true,
		},
		{
			name:       "host has no apt at all -> blocked",
			cap:        Capabilities{Available: false, Reason: "apt-get not found"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			hostSolver: solverInternal, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			name:       "target apt version unrecognised and no override -> blocked (target solver unknown)",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			target:     snapshot.Target{DistroID: "debian", VersionID: "12", APTVersion: "not-a-version"},
			hostSolver: solverInternal, hostSolverOK: true,
			wantBlocked: true,
		},
		{
			name:       "empty target distro id never mismatches on distro (only compared when both sides are set)",
			cap:        Capabilities{Available: true, DistroID: "debian", VersionID: "12", APTVersion: "2.6.1"},
			target:     snapshot.Target{DistroID: "", VersionID: "", APTVersion: "2.6.1"},
			hostSolver: solverInternal, hostSolverOK: true,
			wantBlocked: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hostSolverProbeFunc = func(ctx context.Context) solverProbe {
				return solverProbe{Solver: c.hostSolver, Configured: c.hostSolverConfigured, OK: c.hostSolverOK}
			}
			reason, blocked := localMismatchReason(context.Background(), c.cap, c.target)
			if blocked != c.wantBlocked {
				t.Errorf("blocked = %v (reason %q), want %v", blocked, reason, c.wantBlocked)
			}
			if blocked && reason == "" {
				t.Error("blocked but reason is empty")
			}
			if !blocked && reason != "" {
				t.Errorf("not blocked but reason = %q, want empty", reason)
			}
		})
	}
}

// TestLocalSelectedReason checks the observability nicety separately from
// the gate itself: the secondary apt-version note is attached when versions
// differ and omitted when they match, regardless of the gate's outcome.
func TestLocalSelectedReason(t *testing.T) {
	cap := Capabilities{APTVersion: "2.6.1"}
	target := snapshot.Target{APTVersion: "2.6.1"}
	if got := localSelectedReason("base", cap, target); got != "base" {
		t.Errorf("same version: got %q, want \"base\" (no note appended)", got)
	}

	target.APTVersion = "3.2.0"
	got := localSelectedReason("base", cap, target)
	if got == "base" {
		t.Error("different major.minor should append a note")
	}
	if len(got) <= len("base") || got[:len("base")] != "base" {
		t.Errorf("expected the base reason preserved as a prefix, got %q", got)
	}
}
