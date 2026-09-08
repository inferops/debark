package apt

import (
	"context"
	"testing"
	"time"
)

// TestE2E_HostSolverProbe_RealAptConfig proves the mechanism experiment E2
// itself used (docs/experiments/E2-solver-divergence.md): a real `apt-config
// dump` on the actual apt installed in this test environment, and the
// -o APT::Solver=internal lever E2 confirmed changes the result identically
// across every apt release it tested. This is the honest way to prove the
// probe works -- a Windows-safe unit test can only ever prove the *parser*
// against captured text (solver_test.go), never that the real `apt-config
// dump` invocation and environment plumbing (APT_CONFIG, cleanEnv) actually
// work against a real apt.
func TestE2E_HostSolverProbe_RealAptConfig(t *testing.T) {
	requireLinuxAPT(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. The bare, un-overridden probe must succeed and return one of the
	// two solver values apt actually understands (never garbage, never
	// empty -- effectiveSolverFromDump always resolves to one of the two,
	// see its doc on why "found neither key" is itself a confident
	// "internal", not an unknown).
	solver, ok := probeHostSolverUncached(ctx)
	if !ok {
		t.Fatal("probeHostSolverUncached failed against the real local apt-config; is apt-config on PATH in this test image?")
	}
	if solver != solverInternal && solver != solver3 {
		t.Fatalf("probeHostSolverUncached returned %q, want %q or %q", solver, solverInternal, solver3)
	}
	t.Logf("this test image's real, un-overridden effective solver: %q", solver)

	// Cross-check: this environment's own apt version's release default
	// (defaultSolverForAPTVersion, from the same table select.go's gate
	// falls back to) should agree with the live probe on a stock,
	// unmodified test image -- exactly the property E2 measured holds for
	// every stock release it tried. This is not a tautology: the probe
	// reads apt-config dump, the default table is a hand-maintained
	// constant derived from E2's measurements, and this assertion is what
	// would catch the two drifting apart.
	aptVersionOut, err := newRunner("apt-get").Run(ctx, nil, "-v")
	if err != nil {
		t.Fatalf("apt-get -v: %v", err)
	}
	aptVersion, ok := ParseAptGetVersion(aptVersionOut.Stdout)
	if !ok {
		t.Fatalf("could not parse apt-get -v output: %s", aptVersionOut.Stdout)
	}
	wantDefault, ok := defaultSolverForAPTVersion(aptVersion)
	if !ok {
		t.Fatalf("defaultSolverForAPTVersion could not parse apt version %q", aptVersion)
	}
	if solver != wantDefault {
		t.Errorf("live probe (%q) disagrees with the release-default table for apt %s (%q) on this stock image -- either the table is out of date or this image is not stock",
			solver, aptVersion, wantDefault)
	}

	// 2. The exact lever E2 used: -o APT::Solver=internal must change (or
	// confirm, if the stock default already is internal) the result to
	// EXACTLY "internal", deterministically, regardless of what this
	// specific test image's default happens to be. This is the "same apt
	// binary diverges from itself" mechanism the whole fix exists to gate
	// on, proven against a real apt-config, not a recorded fixture.
	dump, err := runAptConfigDump(ctx, "", "APT::Solver=internal")
	if err != nil {
		t.Fatalf("apt-config dump -o APT::Solver=internal: %v", err)
	}
	overridden := effectiveSolverFromDump(dump)
	if overridden != solverInternal {
		t.Fatalf("apt-config dump -o APT::Solver=internal -> effective solver %q, want %q (E2's own confirmed, portable sentinel)", overridden, solverInternal)
	}

	// 3. The cached production entry point (hostSolverProbeFunc's default)
	// agrees with the uncached bare probe from step 1, proving the caching
	// wrapper does not change the answer.
	cachedSolver, cachedOK := probeHostSolverCached(ctx)
	if !cachedOK || cachedSolver != solver {
		t.Errorf("probeHostSolverCached = (%q, %v), want (%q, true) to match the uncached probe", cachedSolver, cachedOK, solver)
	}
}
