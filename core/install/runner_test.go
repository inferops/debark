package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/verify"
)

// testAptPath / testDpkgPath point Deps.AptPath/DpkgPath at the compiled
// test binary itself, so lookExecutable's os.Stat branch succeeds without
// needing a real apt-get/dpkg anywhere on the machine (see
// exechelper_test.go for the fake exec mechanism that then intercepts every
// actual invocation).
func testBinaryPath(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return p
}

func baseDeps(t *testing.T, v *fakeVerifier) Deps {
	self := testBinaryPath(t)
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	return Deps{
		Verifier: v,
		AptPath:  self,
		DpkgPath: self,
		// Pointed at the same re-exec'able fake as the other two. It is
		// reached only by a bundle whose snapshot.json says the snapshot was
		// synthesized, so every test that predates basedivergence.go behaves
		// exactly as it did; setting it here rather than in those tests keeps
		// the default "a real dpkg-query is not on this machine" from turning
		// into a spurious warning the day one of them grows a snapshot.json.
		DpkgQueryPath: self,
	}
}

func simpleLock() lock.Lock {
	target := basicLockTarget()
	return lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Target:        target,
		// Every entry is architecture-qualified (name:arch=version, dpkg's
		// own syntax): lock.Validate requires it, since an unqualified
		// name=version is ambiguous for a Multi-Arch: same package.
		Install: []string{"jq:amd64=1.6-2.1+deb12u2", "libjq1:amd64=1.6-2.1+deb12u2", "libonig5:amd64=6.9.8-1"},
		Packages: []lock.Package{
			// Reasons mirror a real closure - jq is what was asked for, the
			// two libraries are why apt pulled it in - because lock.Validate
			// now requires the field and a blanket "requested" would describe
			// a lock the resolver never writes.
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb", Size: 100, SHA256: fakeSHA256("jq"), Reason: lock.ReasonRequested, PublisherVerification: lock.VerifiedAPTSigned},
			{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "pool/libj/libjq1/libjq1_1.6-2.1+deb12u2_amd64.deb", Size: 50, SHA256: fakeSHA256("libjq1"), Reason: lock.ReasonDependencyOfPrefix + "jq", PublisherVerification: lock.VerifiedAPTSigned},
			{Name: "libonig5", Arch: "amd64", Version: "6.9.8-1", Filename: "pool/libo/libonig5/libonig5_6.9.8-1_amd64.deb", Size: 200, SHA256: fakeSHA256("libonig5"), Reason: lock.ReasonDependencyOfPrefix + "jq", PublisherVerification: lock.VerifiedAPTSigned},
		},
		// lock.Validate requires closed_world_check.result to name one of the
		// three states; the zero value is not one of them. Every lock the
		// engine writes sets it, so a fixture that leaves it empty is
		// describing a document debark never produces.
		ClosedWorld: lock.ClosedWorld{Result: lock.ClosedWorldSkipped},
	}
}

func goldenPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// --- Verify gates everything --------------------------------------------

func TestExecute_VerifyError_Refuses(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{err: dferr.New(dferr.Verification, "bad signature")}
	deps := baseDeps(t, v)
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if dferr.ClassOf(err) != dferr.Verification {
		t.Errorf("class = %v, want Verification", dferr.ClassOf(err))
	}
	if report == nil {
		t.Fatalf("expected a non-nil report even on failure")
	}
	if v.calls != 1 {
		t.Errorf("Verify called %d times, want 1", v.calls)
	}
	if v.lastBundlePath != bundleDir {
		t.Errorf("Verify called with %q, want %q", v.lastBundlePath, bundleDir)
	}
}

// TestExecute_VerifierErrorClass_IsNotOverridden pins the exit code install
// reports for each kind of verifier failure. The verifier already classifies
// what it can tell apart, and install used to wrap every one of its errors in
// Verification — so a machine with no gnupg exited 4, which ADR-012 gives the
// operator as "the media is compromised, escalate", while `debark verify`
// on the same bundle and the same machine exited 2 and said "install gnupg".
// Both directions are asserted here on purpose: a genuine tamper must still
// be 4, or the fix for the false alarm would have disarmed the real one.
func TestExecute_VerifierErrorClass_IsNotOverridden(t *testing.T) {
	// The Environment case uses the error core/sign itself produces with gpg
	// off PATH, not a hand-written stand-in: the defect was precisely that
	// install disagreed with the class core/verify passes through verbatim
	// ("that is an environment problem, not evidence about the bundle"), so
	// a fabricated cause could agree with the fix and still miss a change in
	// what the verifier really returns.
	t.Setenv("PATH", t.TempDir())
	sv, err := sign.VerifierFor(context.Background(), sign.KeySource{}, t.TempDir())
	if err != nil {
		t.Fatalf("sign.VerifierFor: %v", err)
	}
	gpgMissing := sv.Verify(context.Background(), manifest.SignPurpose, []byte("{}"), manifest.Signature{
		SignerKind: manifest.SignerGPG,
		KeyID:      "DEADBEEF",
		Algorithm:  "gpg",
		Signature:  "AAAA",
	})
	if gpgMissing == nil {
		t.Fatal("expected an error verifying a gpg signature with gpg off PATH")
	}
	if got := dferr.ClassOf(gpgMissing); got != dferr.Environment {
		t.Fatalf("core/sign no longer classifies missing gpg as Environment (got %v); this test's premise is gone", got)
	}

	cases := []struct {
		name string
		v    *fakeVerifier
		want dferr.Class
	}{
		{"gpg not installed", &fakeVerifier{err: gpgMissing}, dferr.Environment},
		{"bad signature", &fakeVerifier{err: dferr.New(dferr.Verification, "sign: signature does not match the manifest")}, dferr.Verification},
		{"tampered file", &fakeVerifier{report: &verify.Report{OK: false, Problems: []verify.Problem{{Kind: verify.ProblemFileDigest, Message: "tampered"}}}}, dferr.Verification},
		// No class at all: install is still the one that decides such a
		// failure is evidence about the bundle.
		{"unclassified", &fakeVerifier{err: errors.New("verifier blew up")}, dferr.Verification},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lk := simpleLock()
			bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
			r := New(baseDeps(t, tc.v))

			_, err := r.Plan(context.Background(), bundleDir, Options{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := dferr.ClassOf(err); got != tc.want {
				t.Errorf("class = %v (exit %d), want %v (exit %d): %v", got, dferr.ExitCode(err), tc.want, int(tc.want), err)
			}
		})
	}

	// The cause's hint is the operator's way out of the Environment case; it
	// was collateral damage of the unconditional wrap, since HintOf reads the
	// outermost *Error and the wrapper carried none.
	if _, err := New(baseDeps(t, &fakeVerifier{err: gpgMissing})).
		Plan(context.Background(), newFixtureBundle(t, fixtureOptions{Lock: simpleLock()}), Options{}); !strings.Contains(dferr.HintOf(err), "gnupg") {
		t.Errorf("hint = %q, want the verifier's own \"install gnupg\" hint to survive", dferr.HintOf(err))
	}
}

func TestExecute_VerifyNotOK_Refuses(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: &verify.Report{OK: false, Problems: []verify.Problem{{Kind: verify.ProblemFileDigest, Message: "tampered"}}}}
	deps := baseDeps(t, v)
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if dferr.ClassOf(err) != dferr.Verification {
		t.Errorf("class = %v, want Verification", dferr.ClassOf(err))
	}
	if report.Verify == nil || report.Verify.OK {
		t.Errorf("report.Verify = %+v, want the failing report attached", report.Verify)
	}
}

func TestExecute_NoAptCalls_WhenVerifyFails(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{err: dferr.New(dferr.Verification, "bad")}
	deps := baseDeps(t, v)
	log := newFakeLogFile(t)
	r := New(deps)

	if _, err := r.Apply(context.Background(), bundleDir, Options{Yes: true}); err == nil {
		t.Fatalf("expected an error")
	}
	if entries := fakeArgvLog(t, log); len(entries) != 0 {
		t.Errorf("apt/dpkg was invoked %d times after a failed verify; want 0: %v", len(entries), entries)
	}
}

// --- Preconditions --------------------------------------------------------

func TestExecute_ArchMismatch_Hard(t *testing.T) {
	lk := simpleLock() // target arch amd64
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	t.Setenv("FAKE_ARCH", "arm64") // the "machine" is arm64, the bundle is amd64
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if dferr.ClassOf(err) != dferr.TargetMismatch {
		t.Fatalf("class = %v, want TargetMismatch (exit 7); err = %v", dferr.ClassOf(err), err)
	}
	if len(report.Problems) == 0 {
		t.Errorf("expected a Problem describing the mismatch")
	}
}

func TestExecute_CodenameMismatch_WarnsOnly(t *testing.T) {
	lk := simpleLock() // target codename bookworm
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "13", "trixie") // machine is trixie
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("codename mismatch must not be a hard failure: %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Errorf("expected a codename-mismatch warning")
	}
	for _, w := range report.Warnings {
		t.Logf("warning: %s", w)
	}
}

// --- Plan (status / dry-run) ----------------------------------------------

func TestPlan_FreshInstall_Golden(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if report.Applied {
		t.Errorf("Plan must never set Applied")
	}
	want := []string{"jq=1.6-2.1+deb12u2", "libjq1=1.6-2.1+deb12u2", "libonig5=6.9.8-1"}
	if !equalStrings(report.ToInstall, want) {
		t.Errorf("ToInstall = %v, want %v", report.ToInstall, want)
	}
	if !report.OK {
		t.Errorf("OK = false, Problems = %v", report.Problems)
	}
	if report.AptOutputDigest == "" {
		t.Errorf("expected AptOutputDigest to be set")
	}

	// Plan must never mutate: only "update" and "-s install" may have run.
	for _, argv := range fakeArgvLog(t, log) {
		joined := strings.Join(argv, " ")
		if containsToken(argv, "-y") {
			t.Errorf("Plan invoked apt-get with -y: %v", argv)
		}
		if strings.Contains(joined, "--configure") || strings.Contains(joined, "--unpack") {
			t.Errorf("Plan invoked a mutating dpkg call: %v", argv)
		}
	}
}

func TestPlan_AlreadyCurrent_Golden(t *testing.T) {
	lk := lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Target:        basicLockTarget(),
		Install:       []string{"jq:amd64=1.6-2.1+deb12u2"},
		Packages: []lock.Package{
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb", Size: 10, SHA256: fakeSHA256("jq"), Reason: lock.ReasonRequested, PublisherVerification: lock.VerifiedAPTSigned},
		},
		ClosedWorld: lock.ClosedWorld{Result: lock.ClosedWorldSkipped},
	}
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-already-current.txt"))
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(report.ToInstall) != 0 || len(report.ToUpgrade) != 0 {
		t.Errorf("expected nothing to install/upgrade, got %v / %v", report.ToInstall, report.ToUpgrade)
	}
	if report.AlreadyCurrent != 1 {
		t.Errorf("AlreadyCurrent = %d, want 1", report.AlreadyCurrent)
	}
}

// upgradePackages are two lock.Package entries at the "new" versions
// testdata/apt-sim-full-upgrade.txt records, tagged Reason: upgrade — the
// lock-recorded stand-in for what an online full-upgrade --download-only
// pass chose at build time (step 4's build side).
func upgradePackages() []lock.Package {
	return []lock.Package{
		{Name: "base-files", Arch: "amd64", Version: "12.4+deb12u15", Filename: "pool/b/base-files/base-files_12.4+deb12u15_amd64.deb", Size: 10, SHA256: fakeSHA256("base-files"), Reason: lock.ReasonUpgrade, PublisherVerification: lock.VerifiedAPTSigned},
		{Name: "liblzma5", Arch: "amd64", Version: "5.4.1-1+deb12u1", Filename: "pool/libl/liblzma5/liblzma5_5.4.1-1+deb12u1_amd64.deb", Size: 20, SHA256: fakeSHA256("liblzma5"), Reason: lock.ReasonUpgrade, PublisherVerification: lock.VerifiedAPTSigned},
	}
}

// combinedGolden concatenates two testdata files' real, recorded apt output
// into one temp file and returns its path — used where a test needs one
// simulated apt-get response covering both a fresh-install shape and an
// upgrade shape at once (this fix folds install's --upgrade into the same
// single "-s install" call the plain install path already makes, so real
// combined output for such a call is not itself checked in as its own
// golden — see the two files this concatenates for their own real,
// recorded provenance).
func combinedGolden(t *testing.T, names ...string) string {
	t.Helper()
	var buf []byte
	for _, name := range names {
		data, err := os.ReadFile(goldenPath(t, name))
		if err != nil {
			t.Fatalf("read golden %s: %v", name, err)
		}
		buf = append(buf, data...)
	}
	path := filepath.Join(t.TempDir(), "combined.txt")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlan_UpgradeOption_InstallsLockedUpgradeSet is this fix's headline
// case (E3, docs/experiments/E3-pin-fidelity.md; ADR-007): --upgrade must
// install the lock's own upgrade set at its exact recorded versions, never
// hand the target's apt a free "full-upgrade" against the flat bundle. Here
// nothing is requested at all (lk.Install is empty) — --upgrade alone must
// still be enough to pull in the lock's upgrade set.
func TestPlan_UpgradeOption_InstallsLockedUpgradeSet(t *testing.T) {
	lk := lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Target:        basicLockTarget(),
		Packages:      upgradePackages(),
		ClosedWorld:   lock.ClosedWorld{Result: lock.ClosedWorldSkipped},
	}
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-full-upgrade.txt"))
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{Upgrade: true})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []string{"base-files=12.4+deb12u15", "liblzma5=5.4.1-1+deb12u1"}
	if !equalStrings(report.ToUpgrade, want) {
		t.Errorf("ToUpgrade = %v, want %v", report.ToUpgrade, want)
	}
	if len(report.ToInstall) != 0 {
		t.Errorf("ToInstall = %v, want none: both entries are upgrades of an already-installed version", report.ToInstall)
	}

	var simArgv []string
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "full-upgrade") {
			t.Fatalf("install --upgrade must never invoke apt-get full-upgrade (E3/ADR-007): %v", argv)
		}
		if containsToken(argv, "-s") && containsToken(argv, "install") {
			simArgv = argv
		}
	}
	if simArgv == nil {
		t.Fatalf("no -s install invocation logged")
	}
	if !containsToken(simArgv, "base-files:amd64=12.4+deb12u15") || !containsToken(simArgv, "liblzma5:amd64=5.4.1-1+deb12u1") {
		t.Errorf("simulate argv = %v, want the lock's exact upgrade-set entries", simArgv)
	}
}

// TestPlan_UpgradeOption_CombinesRequestedAndUpgradeSet proves the merge:
// a requested/dependency set (from simpleLock's Install) and a separate
// lock-recorded upgrade set land in the same single "-s install" call, and
// each is bucketed into the right Report field.
func TestPlan_UpgradeOption_CombinesRequestedAndUpgradeSet(t *testing.T) {
	lk := simpleLock()
	lk.Packages = append(lk.Packages, upgradePackages()...)
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", combinedGolden(t, "apt-sim-install-new.txt", "apt-sim-full-upgrade.txt"))
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{Upgrade: true})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantInstall := []string{"jq=1.6-2.1+deb12u2", "libjq1=1.6-2.1+deb12u2", "libonig5=6.9.8-1"}
	wantUpgrade := []string{"base-files=12.4+deb12u15", "liblzma5=5.4.1-1+deb12u1"}
	if !equalStrings(report.ToInstall, wantInstall) {
		t.Errorf("ToInstall = %v, want %v", report.ToInstall, wantInstall)
	}
	if !equalStrings(report.ToUpgrade, wantUpgrade) {
		t.Errorf("ToUpgrade = %v, want %v", report.ToUpgrade, wantUpgrade)
	}

	var simCalls int
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "full-upgrade") {
			t.Fatalf("install --upgrade must never invoke apt-get full-upgrade (E3/ADR-007): %v", argv)
		}
		if containsToken(argv, "-s") && containsToken(argv, "install") {
			simCalls++
			if !containsToken(argv, "jq:amd64=1.6-2.1+deb12u2") || !containsToken(argv, "base-files:amd64=12.4+deb12u15") {
				t.Errorf("simulate argv = %v, want both the requested set and the upgrade set present in one call", argv)
			}
		}
	}
	if simCalls != 1 {
		t.Errorf("expected exactly one -s install simulate call (requested + upgrade merged), got %d", simCalls)
	}
}

// TestPlan_UpgradeOption_NoLockedUpgrades_Warns: --upgrade against a lock
// that recorded nothing with Reason "upgrade" must not silently do nothing
// unexplained — a warning says so, distinct from a hard failure, since the
// requested set (if any) still installs normally.
func TestPlan_UpgradeOption_NoLockedUpgrades_Warns(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{Upgrade: true})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, `reason "upgrade"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning that the lock recorded nothing to upgrade, got %v", report.Warnings)
	}
}

// TestApply_UpgradeOption_NeverCallsFullUpgrade is the mutating-path
// counterpart of TestPlan_UpgradeOption_CombinesRequestedAndUpgradeSet:
// proves the real (non-simulated) "apt-get full-upgrade" call this fix
// removes is genuinely gone from the apply path too, not just from Plan's
// simulation, and that the real "apt-get install" call carries the lock's
// exact upgrade-set entry alongside the requested one.
func TestApply_UpgradeOption_NeverCallsFullUpgrade(t *testing.T) {
	lk := simpleLock()
	lk.Packages = append(lk.Packages, upgradePackages()...)
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", combinedGolden(t, "apt-sim-install-new.txt", "apt-sim-full-upgrade.txt"))
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, Upgrade: true})
	if err != nil {
		t.Fatalf("Apply: %v (problems=%v)", err, report.Problems)
	}
	if !report.Applied || !report.OK {
		t.Fatalf("report = %+v", report)
	}

	var sawRealInstall bool
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "full-upgrade") {
			t.Fatalf("install --upgrade must never invoke apt-get full-upgrade, simulated or real (E3/ADR-007): %v", argv)
		}
		if containsToken(argv, "install") && !containsToken(argv, "-s") {
			sawRealInstall = true
			if !containsToken(argv, "base-files:amd64=12.4+deb12u15") {
				t.Errorf("real install argv = %v, want the lock's exact upgrade-set entry present", argv)
			}
			if !containsToken(argv, "jq:amd64=1.6-2.1+deb12u2") {
				t.Errorf("real install argv = %v, want the lock's exact requested entry present too", argv)
			}
		}
	}
	if !sawRealInstall {
		t.Errorf("expected a real (mutating) apt-get install call")
	}
}

func TestPlan_DiskSpace_Insufficient_IsAProblem(t *testing.T) {
	lk := simpleLock()
	// 1 PiB: larger than any machine's free space, but still inside the
	// +/-(2^53-1) range RFC 8785 number formatting preserves exactly. 1<<62
	// used to be the value here and now fails canonicalisation outright,
	// which is correct - a .deb size that JCS cannot round-trip must never
	// reach a signed document - but it made this test fail for a reason
	// unrelated to what it protects.
	lk.Packages[0].Size = 1 << 50
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	report, err := r.Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Plan itself must not hard-fail on a disk problem: %v", err)
	}
	if report.OK {
		t.Errorf("OK = true, expected a disk-space Problem to make it false")
	}
	found := false
	for _, p := range report.Problems {
		if strings.Contains(p, "disk space") {
			found = true
		}
	}
	if !found {
		t.Errorf("Problems = %v, want a disk-space entry", report.Problems)
	}
}

// TestApply_DiskSpace_Insufficient_IsEnvironmentClass pins the exit-code
// classification: the exit-code table buckets disk space under Environment
// ("no apt, no container runtime, no disk, no permission", exit 2), not
// Resolution (exit 5, "apt could not satisfy the request") — even though
// both surface through the same "the plan has problems" refusal path.
func TestApply_DiskSpace_Insufficient_IsEnvironmentClass(t *testing.T) {
	lk := simpleLock()
	lk.Packages[0].Size = 1 << 50 // see the note in the Plan test above
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if dferr.ClassOf(err) != dferr.Environment {
		t.Errorf("class = %v, want Environment (exit 2)", dferr.ClassOf(err))
	}
	if report.Applied {
		t.Errorf("Applied = true, want false")
	}
}

func TestPlan_OnlyOption_NarrowsSimulateArgv(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	log := newFakeLogFile(t)
	r := New(deps)

	if _, err := r.Plan(context.Background(), bundleDir, Options{Only: []string{"jq"}}); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var simArgv []string
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "-s") && containsToken(argv, "install") {
			simArgv = argv
		}
	}
	if simArgv == nil {
		t.Fatalf("no -s install invocation logged")
	}
	if !containsToken(simArgv, "jq:amd64=1.6-2.1+deb12u2") {
		t.Errorf("simulate argv = %v, want jq:amd64=1.6-2.1+deb12u2 present", simArgv)
	}
	if containsToken(simArgv, "libjq1:amd64=1.6-2.1+deb12u2") {
		t.Errorf("simulate argv = %v, --only=jq must narrow out libjq1", simArgv)
	}
}

// --- Apply -----------------------------------------------------------------

func TestApply_Success_WithYes(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")

	var captured [][]byte
	var capturedArgv [][]string
	deps.OnAptOutput = func(argv []string, out []byte) {
		capturedArgv = append(capturedArgv, argv)
		captured = append(captured, out)
	}

	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	log := newFakeLogFile(t)

	evidencePath := filepath.Join(t.TempDir(), "evidence.ndjson")
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, EvidencePath: evidencePath})
	if err != nil {
		t.Fatalf("Apply: %v (problems=%v)", err, report.Problems)
	}
	if !report.Applied || !report.OK {
		t.Fatalf("report = %+v", report)
	}
	if len(captured) == 0 {
		t.Errorf("Deps.OnAptOutput was never called")
	}

	entries := fakeArgvLog(t, log)
	var sawUpdate, sawSim, sawRealInstall bool
	for _, argv := range entries {
		switch {
		case containsToken(argv, "update"):
			sawUpdate = true
		case containsToken(argv, "-s") && containsToken(argv, "install"):
			sawSim = true
		case containsToken(argv, "install") && !containsToken(argv, "-s"):
			sawRealInstall = true
			if !containsToken(argv, "-y") {
				t.Errorf("real install must pass -y when Options.Yes is set: %v", argv)
			}
			if !containsToken(argv, "jq:amd64=1.6-2.1+deb12u2") {
				t.Errorf("real install argv = %v, want the exact architecture-qualified entry from the lock", argv)
			}
		}
	}
	if !sawUpdate || !sawSim || !sawRealInstall {
		t.Errorf("expected update, simulate and real install calls; got %v", entries)
	}

	// Evidence: durable local log got exactly one install.result line.
	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("read evidence log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("evidence log has %d lines, want 1: %s", len(lines), data)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("evidence line is not valid JSON: %v", err)
	}
	if rec["type"] != "install.result" {
		t.Errorf("evidence type = %v, want install.result", rec["type"])
	}
}

func TestApply_RefusesWithoutYes_NoTTY(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	log := newFakeLogFile(t)
	r := New(deps) // stdinIsTerminal is pinned false by TestMain

	report, err := r.Apply(context.Background(), bundleDir, Options{}) // no Yes
	if err == nil {
		t.Fatalf("expected a refusal")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
	}
	if report.Applied {
		t.Errorf("Applied = true, want false: nothing must have been mutated")
	}
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "-y") || (containsToken(argv, "install") && !containsToken(argv, "-s")) {
			t.Errorf("a mutating call happened despite the refusal: %v", argv)
		}
	}
}

func TestApply_RefusesWithoutYes_ButAllowsWithTerminal(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })

	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Apply should proceed (without -y) when a terminal is attached: %v", err)
	}
	if !report.Applied {
		t.Fatalf("expected Applied = true")
	}
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "install") && !containsToken(argv, "-s") && containsToken(argv, "-y") {
			t.Errorf("must not pass -y without Options.Yes: %v", argv)
		}
	}
}

func TestApply_PlanProblems_RefusesBeforeMutating(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-unknown-package.txt"))
	t.Setenv("FAKE_APT_SIM_EXIT", "100")
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if dferr.ClassOf(err) != dferr.Resolution {
		t.Errorf("class = %v, want Resolution", dferr.ClassOf(err))
	}
	if report.Applied {
		t.Errorf("Applied = true, want false")
	}
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "install") && !containsToken(argv, "-s") {
			t.Errorf("real install must not run when the plan already has problems: %v", argv)
		}
	}
}

func TestApply_DpkgFallback(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	log := newFakeLogFile(t)
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true})
	if err != nil {
		t.Fatalf("Apply --dpkg: %v (problems=%v)", err, report.Problems)
	}
	if !report.Applied || !report.OK {
		t.Fatalf("report = %+v", report)
	}
	foundWarning := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "ordering and conflict checks are skipped") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected the loud dpkg-fallback warning, got %v", report.Warnings)
	}

	var sawUnpack, sawConfigure, sawAptCall bool
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "--unpack") {
			sawUnpack = true
		}
		if containsToken(argv, "--configure") {
			sawConfigure = true
		}
		if containsToken(argv, "update") || containsToken(argv, "install") {
			sawAptCall = true
		}
	}
	if !sawUnpack || !sawConfigure {
		t.Errorf("expected dpkg --unpack and --configure -a calls")
	}
	if sawAptCall {
		t.Errorf("--dpkg must never invoke apt-get")
	}
}

func TestApply_KeepSource_PersistsExactlyOneFile(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	root := newRootFixture(t, "debian", "12", "bookworm")
	deps.Root = root
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	before := snapshotTree(t, root)
	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, KeepSource: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !report.OK {
		t.Fatalf("report = %+v", report)
	}
	after := snapshotTree(t, root)

	dest := filepath.Join(root, "etc", "apt", "sources.list.d", sourcesFileName)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected %s to exist: %v", dest, err)
	}

	var added []string
	for path := range after {
		if _, ok := before[path]; !ok {
			added = append(added, path)
		}
	}
	if len(added) != 1 {
		t.Fatalf("--keep-source added %d files under root, want exactly 1: %v", len(added), added)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsToken(argv []string, tok string) bool {
	for _, a := range argv {
		if a == tok {
			return true
		}
	}
	return false
}
