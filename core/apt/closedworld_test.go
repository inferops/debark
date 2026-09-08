package apt

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/snapshot"
)

// fakeCWRunner is the fake apt.Runner these ClosedWorld unit tests inject via
// LocalOptions.Runner — the same mechanism select_test.go's fakeVersionRunner
// already uses to exercise this package's logic without a real apt-get
// anywhere on the machine, including on Windows: BuildPrivateRoot's own work
// (writing the private root's files) runs for real, but nothing here ever
// execs a binary. It routes purely on which token each call's args carry, so
// one fake can tell the closed-world's install-set check apart from its
// upgrade-set check even though both are plain "-s install <entries>" calls.
type fakeCWRunner struct {
	calls [][]string

	// updateOut is returned for "update".
	updateOut Output
	// responses is consulted, in order, for anything else; the first entry
	// whose wantToken appears in that call's args wins.
	responses []cwResponse
}

type cwResponse struct {
	wantToken string
	out       Output
}

func (f *fakeCWRunner) Run(_ context.Context, _ []string, args ...string) (Output, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "update" {
		return f.updateOut, nil
	}
	for _, r := range f.responses {
		if containsArg(args, r.wantToken) {
			return r.out, nil
		}
	}
	return Output{}, nil
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// writeClosedWorldBundle creates <dir>/repo (empty — the fake Runner never
// actually reads it) and <dir>/lock.json (real, via lock.Save), and returns
// the repo directory's absolute path: the value ClosedWorldInput.BundleRepoDir
// carries in production. This mirrors the real bundle layout ClosedWorld
// relies on: bundle.Assemble writes lock.json next to repo/ before
// engine.finalizeBundle ever calls ClosedWorld (core/engine/finalize.go).
// fillLockRequirements supplies the fields lock.Validate requires but that
// these closed-world fixtures do not care about. The fixtures exist to
// exercise upgrade divergence, so they name only the identity fields that
// matter to that check; lock.Save validates the whole document, and a lock
// missing a filename or a provenance claim is one debark never writes.
// Filling them here keeps every test's own body about the thing it tests.
func fillLockRequirements(lk *lock.Lock) *lock.Lock {
	if lk.Target.Arch == "" {
		lk.Target.Arch = "amd64"
	}
	if lk.ClosedWorld.Result == "" {
		lk.ClosedWorld.Result = lock.ClosedWorldSkipped
	}
	if lk.Resolver.Backend == "" {
		lk.Resolver.Backend = lock.BackendLocal
	}
	for i := range lk.Packages {
		p := &lk.Packages[i]
		if p.Filename == "" {
			p.Filename = repository.PoolPath(p.Name, p.Name+"_"+p.Version+"_"+p.Arch+".deb")
		}
		if p.SHA256 == "" {
			p.SHA256 = strings.Repeat("a", 64)
		}
		if p.PublisherVerification == "" {
			p.PublisherVerification = lock.VerifiedAPTSigned
		}
		if p.Reason == "" {
			p.Reason = lock.ReasonRequested
		}
	}
	return lk
}

func writeClosedWorldBundle(t *testing.T, lk *lock.Lock) string {
	t.Helper()
	lk = fillLockRequirements(lk)
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Save(dir, lk); err != nil {
		t.Fatalf("lock.Save: %v", err)
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func baseSnapshot() *snapshot.Snapshot {
	return &snapshot.Snapshot{Target: snapshot.Target{Arch: "amd64"}}
}

// TestClosedWorld_UpgradeDivergence_Fails is the test this fix exists to make
// pass: constructs, deliberately, the exact divergence E3 recorded
// (docs/experiments/E3-pin-fidelity.md) — an online solve that locked a
// package's upgrade target at one version (as a target pin would force),
// against a simulated closed-world upgrade check that (faked, but in the
// real shape apt printed in E3's own raw evidence) would actually select a
// different, higher version instead. Before this fix, the closed-world check
// ran a bare "-s full-upgrade" and never compared its result to anything, so
// this exact situation reported ClosedWorldOK; this test proves it now
// reports ClosedWorldFailed with a Detail naming the package and both
// versions.
func TestClosedWorld_UpgradeDivergence_Fails(t *testing.T) {
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			// The online solve's own full-upgrade pass locked this package's
			// upgrade target at 1.0-1 (e.g. because of a target pin — E3's
			// own scenario, two suites publishing different versions of the
			// same package).
			{Name: "debark-e3-demo", Arch: "all", Version: "1.0-1", Reason: lock.ReasonUpgrade},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	runner := &fakeCWRunner{
		updateOut: Output{Stdout: []byte("Reading package lists...\n")},
		responses: []cwResponse{
			{wantToken: "demo-app:amd64=1.0-1", out: Output{Stdout: []byte(
				"Reading package lists...\nBuilding dependency tree...\n" +
					"The following NEW packages will be installed:\n  demo-app\n" +
					"0 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.\n" +
					"Inst demo-app (1.0-1 debark:bundle [amd64])\n" +
					"Conf demo-app (1.0-1 debark:bundle [amd64])\n")}},
			{wantToken: "debark-e3-demo:all=1.0-1", out: Output{Stdout: []byte(
				"Reading package lists...\nBuilding dependency tree...\nCalculating upgrade...\n" +
					"The following packages will be upgraded:\n  debark-e3-demo\n" +
					"1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n" +
					// The exact shape of E3's own recorded divergence
					// (hack/experiments/out/e3/log-07c-bundleB-fullupgrade.txt):
					// the lock's upgrade target was 1.0-1, this (faked)
					// simulation selects 2.0-1 instead, and reports it as
					// ordinary, non-erroring success.
					"Inst debark-e3-demo [1.0-1] (2.0-1 debark:bundle [all])\n" +
					"Conf debark-e3-demo (2.0-1 debark:bundle [all])\n")}},
		},
	}
	backend := newLocalBackend(LocalOptions{Runner: runner})

	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:      baseSnapshot(),
		BundleRepoDir: repoDir,
		Install:       []string{"demo-app:amd64=1.0-1"},
		Upgrades:      true,
		WorkDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"debark-e3-demo:all", "1.0-1", "2.0-1"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q (say what diverged)", cw.Detail, want)
		}
	}
	if cw.CommandDigest == "" || cw.OutputDigest == "" {
		t.Errorf("ClosedWorld missing digests even on failure: %+v", cw)
	}
}

// TestClosedWorld_UpgradeMatchesLock_OK is the companion positive case: when
// the simulated upgrade selects exactly what the lock recorded, the check
// still reports ClosedWorldOK — this fix must not turn every --upgrade build
// into a false failure, and it must never fall back to a bare full-upgrade
// call anywhere in the process.
func TestClosedWorld_UpgradeMatchesLock_OK(t *testing.T) {
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			{Name: "debark-e3-demo", Arch: "all", Version: "2.0-1", Reason: lock.ReasonUpgrade},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	runner := &fakeCWRunner{
		updateOut: Output{Stdout: []byte("Reading package lists...\n")},
		responses: []cwResponse{
			{wantToken: "demo-app:amd64=1.0-1", out: Output{Stdout: []byte(
				"The following NEW packages will be installed:\n  demo-app\n" +
					"0 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.\n" +
					"Inst demo-app (1.0-1 debark:bundle [amd64])\n")}},
			{wantToken: "debark-e3-demo:all=2.0-1", out: Output{Stdout: []byte(
				"The following packages will be upgraded:\n  debark-e3-demo\n" +
					"1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n" +
					"Inst debark-e3-demo [1.0-1] (2.0-1 debark:bundle [all])\n")}},
		},
	}
	backend := newLocalBackend(LocalOptions{Runner: runner})

	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:      baseSnapshot(),
		BundleRepoDir: repoDir,
		Install:       []string{"demo-app:amd64=1.0-1"},
		Upgrades:      true,
		WorkDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldOK, cw.Detail)
	}
	for _, argv := range runner.calls {
		if containsArg(argv, "full-upgrade") {
			t.Errorf("ClosedWorld must never invoke a bare full-upgrade (E3/ADR-007): calls=%v", runner.calls)
		}
	}
}

// TestClosedWorld_NoUpgradeReasonPackages_OK: Upgrades is requested but the
// lock recorded nothing with Reason "upgrade" — ClosedWorld must not invent
// an upgrade-set call (there is nothing to simulate) and must not fail the
// build over it either.
func TestClosedWorld_NoUpgradeReasonPackages_OK(t *testing.T) {
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	runner := &fakeCWRunner{
		updateOut: Output{Stdout: []byte("Reading package lists...\n")},
		responses: []cwResponse{
			{wantToken: "demo-app:amd64=1.0-1", out: Output{Stdout: []byte(
				"0 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.\n" +
					"Inst demo-app (1.0-1 debark:bundle [amd64])\n")}},
		},
	}
	backend := newLocalBackend(LocalOptions{Runner: runner})

	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:      baseSnapshot(),
		BundleRepoDir: repoDir,
		Install:       []string{"demo-app:amd64=1.0-1"},
		Upgrades:      true,
		WorkDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldOK, cw.Detail)
	}
	if len(runner.calls) != 2 {
		t.Errorf("calls = %v, want exactly 2 (update, base install set) - no upgrade-set call when the lock has nothing to upgrade", runner.calls)
	}
}

// TestUpgradeMismatches exercises the comparison function directly against
// real ParseSimulate output text, independent of ClosedWorld's plumbing.
func TestUpgradeMismatches(t *testing.T) {
	want := map[string]string{
		"demo-app:amd64":     "1.0-1",
		"debark-e3-demo:all": "1.0-1",
	}
	sim := ParseSimulate(Output{Stdout: []byte(
		"Inst demo-app (1.0-1 debark:bundle [amd64])\n" +
			"Inst debark-e3-demo [1.0-1] (2.0-1 debark:bundle [all])\n" +
			// Not in want at all: some other package apt happens to touch.
			// upgradeMismatches only judges the packages it was asked about.
			"Inst untracked-pkg (5.0 debark:bundle [amd64])\n",
	)})

	got := upgradeMismatches(want, sim)
	if len(got) != 1 {
		t.Fatalf("mismatches = %v, want exactly 1", got)
	}
	for _, want := range []string{"debark-e3-demo:all", "1.0-1", "2.0-1"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("mismatch message %q, want it to mention %q", got[0], want)
		}
	}
}

// TestUpgradeMismatches_NoActionIsNotAMismatch: a package apt left entirely
// untouched (no Inst/Conf line at all) is not a divergence - it means the
// target's dpkg status already carries the locked version.
func TestUpgradeMismatches_NoActionIsNotAMismatch(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	sim := ParseSimulate(Output{Stdout: []byte("0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n")})
	if got := upgradeMismatches(want, sim); len(got) != 0 {
		t.Errorf("mismatches = %v, want none", got)
	}
}

// TestUpgradeMismatches_MatchingVersionIsNotAMismatch: an Inst/Conf line at
// exactly the locked version is success, not a divergence.
func TestUpgradeMismatches_MatchingVersionIsNotAMismatch(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "2.0-1"}
	sim := ParseSimulate(Output{Stdout: []byte("Inst demo-app [1.0-1] (2.0-1 debark:bundle [amd64])\n")})
	if got := upgradeMismatches(want, sim); len(got) != 0 {
		t.Errorf("mismatches = %v, want none: the simulated version matches the lock exactly", got)
	}
}

// TestClosedWorldUpgradeTargets reads a real, written lock.json back and
// checks the extraction: only Reason: upgrade entries, architecture-
// qualified, sorted.
func TestClosedWorldUpgradeTargets(t *testing.T) {
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			{Name: "liblzma5", Arch: "amd64", Version: "5.4.1-1+deb12u1", Reason: lock.ReasonUpgrade},
			{Name: "base-files", Arch: "amd64", Version: "12.4+deb12u15", Reason: lock.ReasonUpgrade},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	entries, want, err := closedWorldUpgradeTargets(repoDir)
	if err != nil {
		t.Fatalf("closedWorldUpgradeTargets: %v", err)
	}
	wantEntries := []string{"base-files:amd64=12.4+deb12u15", "liblzma5:amd64=5.4.1-1+deb12u1"}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Errorf("entries = %v, want %v (sorted)", entries, wantEntries)
	}
	if want["base-files:amd64"] != "12.4+deb12u15" || want["liblzma5:amd64"] != "5.4.1-1+deb12u1" {
		t.Errorf("want map = %v", want)
	}
	if _, ok := want["demo-app:amd64"]; ok {
		t.Errorf("want map must not include a Reason: requested entry")
	}
}

// TestClosedWorldUpgradeTargets_MissingLock: no lock.json next to the repo
// directory is a hard error, not a silent "nothing to upgrade" - the check
// cannot prove what it was not able to read.
func TestClosedWorldUpgradeTargets_MissingLock(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := closedWorldUpgradeTargets(repoDir); err == nil {
		t.Fatalf("expected an error when lock.json does not exist next to the repo directory")
	}
}

// TestClosedWorld_UnreadableInstLine_Fails pins that an action line this
// package cannot parse makes the closed-world check FAIL rather than pass.
//
// The danger is specific and quiet: upgradeMismatches iterates sim.Actions
// only, so a line instConfRE drops is a package the check never compares
// against the lock at all. Before this, such a run reported ClosedWorldOK —
// certifying a closed world for a package it had not actually checked, which
// is worse than reporting nothing. apt is the oracle, so output whose meaning
// this package does not know cannot certify anything.
func TestClosedWorld_UnreadableInstLine_Fails(t *testing.T) {
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	runner := &fakeCWRunner{
		updateOut: Output{Stdout: []byte("Reading package lists...\n")},
		responses: []cwResponse{
			{wantToken: "demo-app:amd64=1.0-1", out: Output{Stdout: []byte(
				"Reading package lists...\nBuilding dependency tree...\n" +
					"0 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.\n" +
					// A package name containing a space: apt would never emit
					// this, but a hostile index can make it do so, and the
					// regex cannot read it.
					"Inst demo app (1.0-1 debark:bundle [amd64])\n")}},
		},
	}
	backend := newLocalBackend(LocalOptions{Runner: runner})

	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:      baseSnapshot(),
		BundleRepoDir: repoDir,
		Install:       []string{"demo-app:amd64=1.0-1"},
		WorkDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ClosedWorld returned a hard error: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q; an unreadable action line must not certify a closed world",
			cw.Result, lock.ClosedWorldFailed)
	}
	if !strings.Contains(cw.Detail, "could not read") {
		t.Errorf("Detail does not say the line was unreadable: %q", cw.Detail)
	}
}

// TestMaskClosedWorldPathsSurvivesAnyOutputDirectory pins the determinism
// defect the 2026-09-05 integration matrix surfaced, and the bug in the FIRST
// attempt at fixing it.
//
// The property is the one that matters and is not a spelling of the fix: mask
// the same closed-world output produced at two different bundle locations and
// the results must be identical, because OutputDigest is a SHA-256 of exactly
// that text and runs on to LockDigest -> manifest.BundleID -> the signature.
// Where a bundle happens to be written is not part of what was requested —
// requestDigestInput already refuses Output.Path by construction.
//
// The output fixture is built with aptURItoFileName, the package's existing
// transcription of apt's own URItoFileName (asserted against real captured
// list-file names in hardening_test.go), NOT with the rule under test. The
// first version of this test generated its fixture with the same "replace /
// with _" rule the code used, so the implementation was its own oracle and
// the test could not observe that apt percent-encodes '_' itself. It passed
// against a fix that still leaked any --out path containing an underscore.
//
// The directory pairs below therefore include the characters that exposed
// that: an underscore, a space and a '~'. "/work/bundle" vs
// "/work/bundle-determinism-2" — what checkDeterminism actually uses — is
// kept as the first case precisely because it is the one that passed anyway;
// had the harness used the natural "/work/bundle_2", the matrix would have
// stayed red.
func TestMaskClosedWorldPathsSurvivesAnyOutputDirectory(t *testing.T) {
	// One line of each shape apt really emits: the URI it echoes on Get:, and
	// two success-path warnings naming the cached index by apt's own name.
	output := func(repo string) string {
		list := aptURItoFileName(FileURI(repo))
		return "Get:1 " + FileURI(repo) + " ./ InRelease\n" +
			"W: Download is performed unsandboxed as root as file " +
			"'/tmp/cw/closedworld-aptroot/var/lib/apt/lists/partial/" + list +
			"_._InRelease' couldn't be accessed by user '_apt'.\n" +
			"W: Invalid 'Date' entry in Release file " +
			"/tmp/cw/closedworld-aptroot/var/lib/apt/lists/partial/" + list + "_._Release\n"
	}
	maskedFor := func(repo string) string {
		in := ClosedWorldInput{WorkDir: "/tmp/cw", BundleRepoDir: repo}
		return maskClosedWorldPaths(output(repo), closedWorldMasks(in, repo))
	}

	pairs := []struct{ name, a, b string }{
		{"plain, as checkDeterminism spells it", "/work/bundle/repo", "/work/bundle-determinism-2/repo"},
		{"underscore: apt encodes it as %5f", "/work/my_bundle/repo", "/work/my_bundle_2/repo"},
		{"space: percent-encoded twice over", "/work/has space/repo", "/work/has space 2/repo"},
		{"tilde and equals", "/home/user~1/out/repo", "/home/user~1/out=2/repo"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			a, b := maskedFor(p.a), maskedFor(p.b)
			if a != b {
				t.Errorf("masked closed-world output depends on the bundle's output directory,\n"+
					"so OutputDigest -> LockDigest -> BundleID does too:\n  %q\n  %q", a, b)
			}
			// Guard against passing for the wrong reason: if masking ever
			// collapsed the text away the comparison would be trivially
			// satisfied while proving nothing. Require that apt's sentence
			// survived and that no directory-specific fragment did.
			if !strings.Contains(a, "<BUNDLE-REPO>_._Release") {
				t.Errorf("the apt list file name was not masked to the placeholder while keeping its shape: %q", a)
			}
			for _, leak := range []string{"bundle-determinism", "my_bundle", "my%5fbundle", "has space", "has%20space", "user~1", "user%7e1"} {
				if strings.Contains(a, leak) || strings.Contains(b, leak) {
					t.Errorf("a directory-specific fragment %q survived masking:\n  %q\n  %q", leak, a, b)
				}
			}
		})
	}
}
