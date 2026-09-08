package apt

import (
	"strings"
	"testing"

	"github.com/inferops/debark/core/snapshot"
)

func TestDefaultSolverForAPTVersion(t *testing.T) {
	cases := []struct {
		version string
		want    string
		wantOK  bool
	}{
		// MEASURED (hack/experiments/out/e2/e2-run.log, 2026-09-03 run --
		// see solver.go's doc comment for the full citation):
		{"2.8.3", solverInternal, true},
		{"3.1.6ubuntu2", solver3, true},
		{"3.2.0", solver3, true},
		// INFERRED generalisation (major < 3 -> internal, major >= 3 -> 3.0):
		{"2.6.1", solverInternal, true},
		{"1.6~exp1+deb12u1", solverInternal, true},
		{"4.0.0", solver3, true},
		// unparseable:
		{"", "", false},
		{"not-a-version", "", false},
	}
	for _, c := range cases {
		got, ok := defaultSolverForAPTVersion(c.version)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("defaultSolverForAPTVersion(%q) = (%q, %v), want (%q, %v)", c.version, got, ok, c.want, c.wantOK)
		}
	}
}

func TestTargetGateSolver(t *testing.T) {
	solver, known := targetGateSolver(snapshot.Target{APTVersion: "2.8.3"})
	if !known || solver != solverInternal {
		t.Errorf("targetGateSolver(2.8.3) = (%q, %v), want (%q, true)", solver, known, solverInternal)
	}
	solver, known = targetGateSolver(snapshot.Target{APTVersion: "3.2.0"})
	if !known || solver != solver3 {
		t.Errorf("targetGateSolver(3.2.0) = (%q, %v), want (%q, true)", solver, known, solver3)
	}
	if _, known := targetGateSolver(snapshot.Target{APTVersion: ""}); known {
		t.Error("targetGateSolver(\"\") should report known=false")
	}
}

// --- apt-config dump parsing, against REAL captured evidence -------------

// The three blocks below are copied verbatim (only re-indented) from
// hack/experiments/out/e2/e2-run.log, the actual `apt-config dump | grep -i
// solv` output hack/experiments/e2-solver-divergence.sh captured from the
// three real, stock Ubuntu images on 2026-09-03 -- not synthesised. See
// docs/experiments/E2-solver-divergence.md's "Raw evidence" for the
// narrative and solver.go's doc comment for why both scoped and bare keys
// must be checked (25.10 carries the value only in the scoped key; 26.04
// carries it only in the bare one).
const (
	e2Dump2404 = `Dir::Bin::solvers "";
Dir::Bin::solvers:: "/usr/lib/apt/solvers";
`
	e2Dump2510 = `APT::Solver "";
APT::Solver::RemoveManual "true";
Dir::Bin::solvers "";
Dir::Bin::solvers:: "/usr/lib/apt/solvers";
binary::apt::APT::Solver "3.0";
binary::apt-get::APT::Solver "3.0";
`
	e2Dump2604 = `APT::Solver "3.0";
Dir::Bin::solvers "";
Dir::Bin::solvers:: "/usr/lib/apt/solvers";
Version::1.21::APT::Solver "3.0";
Version::2.11::APT::Solver "3.0";
Version::3.1::APT::Solver "3.0";
`
)

// debian13DumpNoSolverKey is the shape measured on 2026-09-05 against a
// stock debian:trixie-slim container (apt 3.0.3, amd64):
//
//	$ apt-config dump | grep -i '^APT::Solver'   -> no output
//
// and corroborated by the matrix failure itself, which is the stronger
// evidence of the two: the live probe (which checks the apt-get-scoped key
// FIRST, then the bare one) reported "internal" on that host
// (hack/matrix/results/run-2026-09-05.json), so neither key carried a value.
// The non-solver lines are not a verbatim capture -- only the grep was
// recorded -- so they are the same solver-free shape E2 captured on 24.04,
// which is all this fixture is asserting about.
const debian13DumpNoSolverKey = `Dir::Bin::solvers "";
Dir::Bin::solvers:: "/usr/lib/apt/solvers";
APT::Architecture "amd64";
`

// blankBareSolverKey is 25.10's bare-key shape with its scoped key removed:
// apt printed the key, but empty. apt treats an empty APT::Solver as unset
// (E2 recorded questing carrying `APT::Solver ""` while the real value lived
// in the scoped key), so this must read as internal AND as unconfigured --
// an empty assignment selects no solver, so it cannot be evidence that this
// machine chose one.
const blankBareSolverKey = `APT::Solver "";
Dir::Bin::solvers "";
`

func TestEffectiveSolverFromDump_RealCapturedOutput(t *testing.T) {
	cases := []struct {
		name string
		dump string
		want string
		// wantConfigured: did apt report a non-empty APT::Solver value?
		// This is what tells "the machine selected a solver" apart from
		// "apt fell back to its own compiled-in default", the distinction
		// the whole gate now turns on (solverProbe).
		wantConfigured bool
	}{
		{"ubuntu 24.04 / apt 2.8.3: no solver key at all -> internal", e2Dump2404, solverInternal, false},
		{"ubuntu 25.10 / apt 3.1.6ubuntu2: scoped key carries 3.0, bare key is blank", e2Dump2510, solver3, true},
		{"ubuntu 26.04 / apt 3.2.0: only the bare key carries 3.0, no scoped key at all", e2Dump2604, solver3, true},
		{"debian 13 / apt 3.0.3: no solver key at all -> internal, and NOT configured", debian13DumpNoSolverKey, solverInternal, false},
		{"blank bare key selects nothing -> internal, and NOT configured", blankBareSolverKey, solverInternal, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, configured := solverFromDump([]byte(c.dump))
			if got != c.want || configured != c.wantConfigured {
				t.Errorf("solverFromDump(%s) = (%q, configured=%v), want (%q, configured=%v)", c.name, got, configured, c.want, c.wantConfigured)
			}
			if v := effectiveSolverFromDump([]byte(c.dump)); v != c.want {
				t.Errorf("effectiveSolverFromDump(%s) = %q, want %q (must agree with solverFromDump)", c.name, v, c.want)
			}
		})
	}
}

func TestEffectiveSolverFromDump_OverrideBeatsDefault(t *testing.T) {
	// -o APT::Solver=internal on a solver3-default release: the exact lever
	// E2 used. apt-config dump reflects it as a plain top-level assignment.
	dump := `APT::Solver "internal";
Dir::Bin::solvers "";
`
	if got := effectiveSolverFromDump([]byte(dump)); got != solverInternal {
		t.Errorf("effectiveSolverFromDump(override) = %q, want %q", got, solverInternal)
	}
}

func TestParseAptConfigDumpMap(t *testing.T) {
	dump := `APT::Architecture "amd64";
APT::Install-Recommends "true";
Acquire::http::Proxy "http://user:pass@proxy.example.com:3128/";
Binary::apt-get::APT::Solver "3.0";
this is not a valid dump line
`
	kv := parseAptConfigDumpMap([]byte(dump))
	want := map[string]string{
		"apt::architecture":            "amd64",
		"apt::install-recommends":      "true",
		"acquire::http::proxy":         "http://user:pass@proxy.example.com:3128/",
		"binary::apt-get::apt::solver": "3.0",
	}
	for k, v := range want {
		if kv[k] != v {
			t.Errorf("kv[%q] = %q, want %q (full map: %+v)", k, kv[k], v, kv)
		}
	}
	if len(kv) != len(want) {
		t.Errorf("parsed %d keys, want %d (map: %+v)", len(kv), len(want), kv)
	}
}

func TestUnescapeAptConfigValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `plain`},
		{`with \"quote\" inside`, `with "quote" inside`},
		{`trailing backslash\`, `trailing backslash\`}, // dangling backslash: copied verbatim, never panics
	}
	for _, c := range cases {
		if got := unescapeAptConfigValue(c.in); got != c.want {
			t.Errorf("unescapeAptConfigValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- buildSolverDivergenceMessage: pure, no I/O ---------------------------

func TestBuildSolverDivergenceMessage(t *testing.T) {
	t.Run("both known and equal is not a divergence", func(t *testing.T) {
		if got := buildSolverDivergenceMessage(solverInternal, false, "2.8.3", solverInternal, true, "2.8.3"); got != "" {
			t.Errorf("got %q, want \"\"", got)
		}
	})

	t.Run("both known and equal even when target was explicit", func(t *testing.T) {
		if got := buildSolverDivergenceMessage(solver3, true, "3.2.0", solver3, true, "3.2.0"); got != "" {
			t.Errorf("got %q, want \"\"", got)
		}
	})

	t.Run("both known and different names both solvers and both methods", func(t *testing.T) {
		got := buildSolverDivergenceMessage(solverInternal, false, "2.8.3", solver3, true, "3.2.0")
		for _, want := range []string{`"internal"`, `"3.0"`, "2.8.3", "3.2.0", "apt-config dump"} {
			if !strings.Contains(got, want) {
				t.Errorf("message %q missing %q", got, want)
			}
		}
	})

	t.Run("explicit target override is named as such", func(t *testing.T) {
		got := buildSolverDivergenceMessage(solverInternal, true, "3.2.0", solver3, true, "3.2.0")
		if !strings.Contains(got, "explicit APT::Solver") {
			t.Errorf("message %q should credit the explicit override", got)
		}
	})

	t.Run("resolver unknown is reported, not silently treated as a match", func(t *testing.T) {
		got := buildSolverDivergenceMessage(solverInternal, false, "2.8.3", "", false, "2.8.3")
		if got == "" {
			t.Fatal("an unverified match must not collapse to no divergence")
		}
		if !strings.Contains(got, "could not be confirmed") {
			t.Errorf("message %q should say the resolver solver could not be confirmed", got)
		}
	})

	t.Run("target unknown is reported, not silently treated as a match", func(t *testing.T) {
		got := buildSolverDivergenceMessage("", false, "weird-version", solver3, true, "3.2.0")
		if got == "" {
			t.Fatal("an unverified target must not collapse to no divergence")
		}
		if !strings.Contains(got, "could not be determined") {
			t.Errorf("message %q should say the target solver could not be determined", got)
		}
	})
}

func TestAptVersionDivergenceNote(t *testing.T) {
	if got := aptVersionDivergenceNote("2.8.3", "2.8.3"); got != "" {
		t.Errorf("same version: got %q, want \"\"", got)
	}
	if got := aptVersionDivergenceNote("3.1.6ubuntu2", "3.2.0"); got == "" {
		t.Error("different major.minor should still produce a note (it just no longer blocks selection)")
	}
	if got := aptVersionDivergenceNote("", "3.2.0"); got != "" {
		t.Errorf("unparseable resolver version: got %q, want \"\" (nothing to compare)", got)
	}
}

// --- the inference/measurement rule (the Debian 13 regression) ------------

func TestSameAPTMajorMinor(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"3.0.3", "3.0.3", true},
		{"3.0.3", "3.0.2", true},         // point-release drift between snapshot and builder
		{"3.0.3", "3.1.6ubuntu2", false}, // trixie's measured default must not speak for questing
		{"3.0.3", "2.6.1", false},
		{"3.0.3", "not-a-version", false}, // unparseable is never "the same apt"
		{"", "", false},
	}
	for _, c := range cases {
		if got := sameAPTMajorMinor(c.a, c.b); got != c.want {
			t.Errorf("sameAPTMajorMinor(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestProbeMeasuresTargetDefault pins the predicate that decides whether a
// probe and the version table are two opinions or one measurement. Each
// negative case is a way the fix could have been over-applied into "same apt
// version => always trust the host", which would quietly retire the gate E2
// asked for.
func TestProbeMeasuresTargetDefault(t *testing.T) {
	trixie := solverProbe{Solver: solverInternal, Configured: false, OK: true}

	cases := []struct {
		name           string
		probe          solverProbe
		probeVersion   string
		targetVersion  string
		targetExplicit bool
		want           bool
	}{
		{
			// The measured Debian 13 case: apt 3.0.3 both sides, apt itself
			// reports no APT::Solver, target captured no override. The
			// probe IS the target's release default.
			name:  "apt 3.0.3 both sides, nothing configured, target not explicit",
			probe: trixie, probeVersion: "3.0.3", targetVersion: "3.0.3",
			want: true,
		},
		{
			name:  "same major.minor, different point release",
			probe: trixie, probeVersion: "3.0.3", targetVersion: "3.0.2",
			want: true,
		},
		{
			// E2's own lever. A configured value describes this machine, so
			// it can never stand in for the target's default.
			name:         "host has APT::Solver configured",
			probe:        solverProbe{Solver: solverInternal, Configured: true, OK: true},
			probeVersion: "3.0.3", targetVersion: "3.0.3",
			want: false,
		},
		{
			// The target captured a real APT::Solver: a recorded fact about
			// the target that no measurement taken elsewhere replaces.
			name:  "target set an explicit APT::Solver",
			probe: trixie, probeVersion: "3.0.3", targetVersion: "3.0.3", targetExplicit: true,
			want: false,
		},
		{
			// A compiled-in default is a property of the binary, so it says
			// nothing about a different apt.
			name:  "different major.minor",
			probe: trixie, probeVersion: "3.0.3", targetVersion: "3.2.0",
			want: false,
		},
		{
			name:         "probe did not run at all",
			probe:        solverProbe{OK: false},
			probeVersion: "3.0.3", targetVersion: "3.0.3",
			want: false,
		},
		{
			name:  "target apt version unparseable",
			probe: trixie, probeVersion: "3.0.3", targetVersion: "not-a-version",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := probeMeasuresTargetDefault(c.probe, c.probeVersion, c.targetVersion, c.targetExplicit)
			if got != c.want {
				t.Errorf("probeMeasuresTargetDefault(%+v, %q, %q, explicit=%v) = %v, want %v",
					c.probe, c.probeVersion, c.targetVersion, c.targetExplicit, got, c.want)
			}
		})
	}
}

// TestSolverGateReason_Debian13 is the regression this fix exists for, at the
// level of the decision function itself: measured 2026-09-05, a Debian 13
// builder resolving for a Debian 13 target was refused the local backend
// (exit 2, environment) because defaultSolverForAPTVersion guesses "3.0" for
// apt 3.0.3 while trixie's own apt-config dump has no APT::Solver key at all
// and so resolves with "internal". Both sides are the same apt; there is
// nothing here that could diverge.
func TestSolverGateReason_Debian13(t *testing.T) {
	host := solverProbe{Solver: solverInternal, Configured: false, OK: true}
	target := snapshot.Target{DistroID: "debian", VersionID: "13", APTVersion: "3.0.3"}

	// The guess the gate used to compare against, spelled out here so this
	// test still means something if the table is ever corrected: the point
	// is that the gate must not block EVEN WHILE the table disagrees.
	if guess, ok := targetGateSolver(target); !ok || guess != solver3 {
		t.Fatalf("precondition: defaultSolverForAPTVersion(3.0.3) = (%q, %v), want (%q, true) -- this test is meaningless unless the table still disagrees with the measurement", guess, ok, solver3)
	}

	reason, mismatched := solverGateReason(host, "3.0.3", target)
	if mismatched {
		t.Errorf("Debian 13 host refused a Debian 13 target: %s", reason)
	}
	if reason != "" {
		t.Errorf("not mismatched but reason = %q, want empty", reason)
	}
}

func TestSolverGateReason(t *testing.T) {
	t.Run("configured host override on the same apt still blocks", func(t *testing.T) {
		// E2's `-o APT::Solver=internal` lever, on a release whose default
		// really is solver3 (26.04, measured). The gate must keep catching
		// this: it is the case E2 built it for.
		host := solverProbe{Solver: solverInternal, Configured: true, OK: true}
		reason, mismatched := solverGateReason(host, "3.2.0", snapshot.Target{APTVersion: "3.2.0"})
		if !mismatched {
			t.Fatal("a host that explicitly configures a different solver must not be allowed to resolve locally")
		}
		for _, want := range []string{`"internal"`, `"3.0"`, "explicitly configured", "E2"} {
			if !strings.Contains(reason, want) {
				t.Errorf("reason %q missing %q", reason, want)
			}
		}
	})

	t.Run("unconfigured host solver that disagrees on a different apt still blocks", func(t *testing.T) {
		// The measurement speaks only for its own apt version, so a target
		// on a different major.minor falls back to the table and the
		// container remains the safe fallback.
		host := solverProbe{Solver: solverInternal, Configured: false, OK: true}
		reason, mismatched := solverGateReason(host, "3.0.3", snapshot.Target{APTVersion: "3.2.0"})
		if !mismatched {
			t.Fatal("trixie's measured default must not be generalised to another apt version")
		}
		if !strings.Contains(reason, "no APT::Solver configured") {
			t.Errorf("reason %q should say the host value is apt's own default, not a configured one", reason)
		}
	})

	t.Run("matching solvers across different apt versions still allowed (E2 version-only comparison)", func(t *testing.T) {
		host := solverProbe{Solver: solver3, Configured: true, OK: true}
		if reason, mismatched := solverGateReason(host, "3.1.6ubuntu2", snapshot.Target{APTVersion: "3.2.0"}); mismatched {
			t.Errorf("E2 found zero selection divergence when only the apt version differs: %s", reason)
		}
	})

	t.Run("probe failure is never an assumed match", func(t *testing.T) {
		reason, mismatched := solverGateReason(solverProbe{OK: false}, "3.0.3", snapshot.Target{APTVersion: "3.0.3"})
		if !mismatched {
			t.Fatal("an unprobed host must fall back to the container, not be assumed to match")
		}
		if !strings.Contains(reason, "could not confirm") {
			t.Errorf("reason %q should say the match could not be confirmed", reason)
		}
	})

	t.Run("unrecognised target apt version blocks", func(t *testing.T) {
		host := solverProbe{Solver: solverInternal, Configured: false, OK: true}
		if _, mismatched := solverGateReason(host, "3.0.3", snapshot.Target{APTVersion: "not-a-version"}); !mismatched {
			t.Error("a target whose apt version cannot be parsed has no expected solver to match")
		}
	})
}
