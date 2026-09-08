package install

import (
	"context"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
)

// foreignArchLock is simpleLock() plus one i386 package and the
// foreign_archs: ["i386"] the online side records for such a bundle - the
// shape of the real bundle used in the 2026-09-06 container measurement that
// produced these tests.
func foreignArchLock() lock.Lock {
	lk := simpleLock()
	lk.Target.ForeignArchs = []string{"i386"}
	// Sorted, like every other entry list in the lock: "z" after "l".
	lk.Install = append(lk.Install, "zlib1g:i386=1.2.13.dfsg-1")
	lk.Packages = append(lk.Packages, lock.Package{
		Name:                  "zlib1g",
		Arch:                  "i386",
		Version:               "1.2.13.dfsg-1",
		Filename:              "pool/z/zlib1g/zlib1g_1.2.13.dfsg-1_i386.deb",
		Size:                  100,
		SHA256:                fakeSHA256("zlib1g:i386"),
		Reason:                lock.ReasonRequested,
		PublisherVerification: lock.VerifiedAPTSigned,
	})
	return lk
}

// TestExecute_ForeignArchNotEnabled_IsTargetMismatch is defect 1, measured
// 2026-09-06: a bundle recording foreign_archs ["i386"], installed in
// debian:12-slim on a target that had never run `dpkg --add-architecture
// i386`, exited 100 with dpkg refusing every archive ("package architecture
// (i386) does not match system (amd64)"); the same bundle after that one
// command exited 0.
//
// The simulate file is wired up deliberately: `apt-get -s install` SUCCEEDS
// in both the working and the broken case, because apt's simulation never
// consults dpkg's foreign-architecture list. That is why this test would
// have passed - a clean plan, no error - before the precondition existed,
// and it is the whole reason the precondition cannot be folded into the
// existing simulate-based problem check.
func TestExecute_ForeignArchNotEnabled_IsTargetMismatch(t *testing.T) {
	lk := foreignArchLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	// FAKE_FOREIGN_ARCHS unset: dpkg reports no foreign architectures, i.e.
	// the machine on which the measured run failed.
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	log := newFakeLogFile(t)

	report, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if err == nil {
		t.Fatalf("Plan succeeded on a machine that cannot unpack this bundle; report = %+v", report)
	}
	if got := dferr.ClassOf(err); got != dferr.TargetMismatch {
		t.Errorf("class = %v (exit %d), want TargetMismatch (exit 7): %v", got, dferr.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "i386") {
		t.Errorf("error = %q, want it to name the missing architecture", err.Error())
	}
	if hint := dferr.HintOf(err); !strings.Contains(hint, "dpkg --add-architecture i386") {
		t.Errorf("hint = %q, want the exact command that fixes it", hint)
	}
	found := false
	for _, p := range report.Problems {
		if strings.Contains(p, "i386") {
			found = true
		}
	}
	if !found {
		t.Errorf("Problems = %v, want one naming i386, like the native-arch mismatch does", report.Problems)
	}

	// Refused as a precondition: nothing apt-side may have run.
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "install") || containsToken(argv, "update") {
			t.Errorf("apt ran despite the refusal: %v", argv)
		}
	}
	t.Logf("operator sees: debark: %s\n%s", err.Error(), dferr.HintOf(err))
}

// TestExecute_ForeignArchEnabled_Proceeds is the other half of the same
// measurement: the SAME bundle on a machine where `dpkg
// --add-architecture i386` has been run installs cleanly. The check must
// refuse the first machine and only the first machine.
func TestExecute_ForeignArchEnabled_Proceeds(t *testing.T) {
	lk := foreignArchLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_FOREIGN_ARCHS", "i386")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))

	report, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Plan refused a machine that has i386 enabled: %v", err)
	}
	if !report.OK {
		t.Errorf("OK = false, Problems = %v", report.Problems)
	}
	if got := report.TargetActual.ForeignArchs; len(got) != 1 || got[0] != "i386" {
		t.Errorf("TargetActual.ForeignArchs = %v, want [i386] - the report must describe the machine that was checked", got)
	}
	if got := report.TargetExpected.ForeignArchs; len(got) != 1 || got[0] != "i386" {
		t.Errorf("TargetExpected.ForeignArchs = %v, want [i386]", got)
	}
}

// TestExecute_ForeignArchExtraOnMachine_IsNotAMismatch: the machine may have
// architectures the bundle never asked for. That is a superset, not a
// mismatch, and refusing it would break every target that happens to have
// i386 enabled for unrelated reasons.
func TestExecute_ForeignArchExtraOnMachine_IsNotAMismatch(t *testing.T) {
	lk := simpleLock() // no foreign_archs at all
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_FOREIGN_ARCHS", "i386 armhf")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))

	report, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !report.OK {
		t.Errorf("OK = false, Problems = %v", report.Problems)
	}
}

// TestExecute_ForeignArchFromSignedManifest_IsChecked closes the union half
// of the precondition. core/manifest and core/verify both document
// Target.ForeignArchs as existing so that "install's architecture
// precondition check" has a signed source for it (manifest/types.go,
// verify/iface.go); until this check landed, nothing read it from either
// place. A lock that omits the field must not switch the check off when the
// verified manifest names the architecture.
func TestExecute_ForeignArchFromSignedManifest_IsChecked(t *testing.T) {
	lk := simpleLock() // lock says nothing about foreign architectures
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	vrep := okVerifyReport(lk.Target)
	vrep.Target.ForeignArchs = []string{"i386"} // ... but the signed manifest does
	deps := baseDeps(t, &fakeVerifier{report: vrep})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))

	_, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if got := dferr.ClassOf(err); got != dferr.TargetMismatch {
		t.Fatalf("class = %v, want TargetMismatch (exit 7); err = %v", got, err)
	}
}

// TestExecute_ForeignArchQueryFails_SeverityDependsOnTheBundle: a dpkg that
// cannot answer --print-foreign-architectures stops an install that needs a
// foreign architecture (falling through with an empty list would silently
// disable the check, exactly as a failed --print-architecture would) and must
// NOT stop an ordinary single-architecture install, which never needed the
// answer.
func TestExecute_ForeignArchQueryFails_SeverityDependsOnTheBundle(t *testing.T) {
	cases := []struct {
		name      string
		lk        lock.Lock
		wantClass dferr.Class
		wantErr   bool
	}{
		{"bundle needs a foreign arch", foreignArchLock(), dferr.Environment, true},
		{"ordinary single-arch bundle", simpleLock(), dferr.Success, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundleDir := newFixtureBundle(t, fixtureOptions{Lock: tc.lk})
			deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(tc.lk.Target)})
			deps.Root = newRootFixture(t, "debian", "12", "bookworm")
			t.Setenv("FAKE_ARCH", "amd64")
			t.Setenv("FAKE_DPKG_FOREIGN_ARCH_EXIT", "1")
			t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))

			_, err := New(deps).Plan(context.Background(), bundleDir, Options{})
			if tc.wantErr {
				if got := dferr.ClassOf(err); got != tc.wantClass {
					t.Fatalf("class = %v, want %v; err = %v", got, tc.wantClass, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("an unanswerable foreign-arch query must not fail a bundle that needs no foreign architecture: %v", err)
			}
		})
	}
}

func TestMissingForeignArchs(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		enabled  []string
		native   string
		want     []string
	}{
		{"nothing required", nil, nil, "amd64", nil},
		{"required and missing", []string{"i386"}, nil, "amd64", []string{"i386"}},
		{"required and present", []string{"i386"}, []string{"i386"}, "amd64", nil},
		{"machine has more", []string{"i386"}, []string{"armhf", "i386"}, "amd64", nil},
		{"several missing, sorted", []string{"i386", "armhf"}, nil, "amd64", []string{"armhf", "i386"}},
		{"partially missing", []string{"i386", "armhf"}, []string{"i386"}, "amd64", []string{"armhf"}},
		// dpkg refuses `--add-architecture amd64` on an amd64 machine ("it is
		// the native architecture"), so a lock naming the target's own
		// architecture among its foreign ones must not produce a refusal
		// whose only remedy is a command that cannot succeed.
		{"native arch is never missing", []string{"amd64"}, nil, "amd64", nil},
		{"blank and duplicate entries", []string{"i386", "", " i386 "}, nil, "amd64", []string{"i386"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingForeignArchs(tc.required, tc.enabled, tc.native)
			if !equalStrings(got, tc.want) {
				t.Errorf("missingForeignArchs(%v, %v, %q) = %v, want %v", tc.required, tc.enabled, tc.native, got, tc.want)
			}
		})
	}
}

func TestParseForeignArchs(t *testing.T) {
	// Real dpkg prints one per line; the space-separated form is accepted for
	// the same reason core/snapshot's capture accepts it (strings.Fields).
	if got := parseForeignArchs([]byte("i386\narmhf\n")); !equalStrings(got, []string{"armhf", "i386"}) {
		t.Errorf("line-separated: got %v", got)
	}
	if got := parseForeignArchs([]byte("  ")); len(got) != 0 {
		t.Errorf("a machine with no foreign architectures must parse as none, got %v", got)
	}
}

func TestAddArchitectureCommand(t *testing.T) {
	if got := addArchitectureCommand([]string{"i386"}); got != "dpkg --add-architecture i386" {
		t.Errorf("got %q", got)
	}
	if got := addArchitectureCommand([]string{"armhf", "i386"}); got != "dpkg --add-architecture armhf && dpkg --add-architecture i386" {
		t.Errorf("got %q", got)
	}
}
