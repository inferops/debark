package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/verify"
)

// This file is about the refusals: the paths where install must NOT proceed,
// and the paths where something went wrong and install must not call it a
// success. A green install of a bundle that is fine proves very little on
// its own; the exit code an operator gets when it is not fine is the whole
// product on the target side (ADR-012).

// --- verify-before-apt has no back door ------------------------------------

// TestExecute_RefusesSkipFileDigests: verify.Options.SkipFileDigests turns
// off content hashing for every pool .deb (core/verify's checkFiles compares
// only the size for those under that flag). Options.Verify comes from
// install's caller, so without this refusal a caller could ask install to
// unpack, as root, bytes nothing ever digested - a same-size substitution
// would pass. That is not a speed knob on the install path, it is a way
// around verify-before-apt.
func TestExecute_RefusesSkipFileDigests(t *testing.T) {
	for _, apply := range []bool{false, true} {
		name := "Plan"
		if apply {
			name = "Apply"
		}
		t.Run(name, func(t *testing.T) {
			lk := simpleLock()
			bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
			v := &fakeVerifier{report: okVerifyReport(lk.Target)}
			deps := baseDeps(t, v)
			deps.Root = newRootFixture(t, "debian", "12", "bookworm")
			t.Setenv("FAKE_ARCH", "amd64")
			t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
			log := newFakeLogFile(t)
			r := New(deps)

			opts := Options{Verify: verify.Options{SkipFileDigests: true}}
			var err error
			if apply {
				opts.Yes = true
				_, err = r.Apply(context.Background(), bundleDir, opts)
			} else {
				_, err = r.Plan(context.Background(), bundleDir, opts)
			}
			if err == nil {
				t.Fatalf("expected a refusal")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %v (exit %d), want Usage (exit 1): %v", got, dferr.ExitCode(err), err)
			}
			// It has to refuse BEFORE running the verifier, so no report
			// describing a reduced verification can ever be attached to an
			// install.
			if v.calls != 0 {
				t.Errorf("the verifier ran %d times; the refusal must come first", v.calls)
			}
			if entries := fakeArgvLog(t, log); len(entries) != 0 {
				t.Errorf("apt/dpkg ran %d times despite the refusal: %v", len(entries), entries)
			}
		})
	}
}

// --- the lock names a package the pool does not hold -----------------------

// TestExecute_LockNamesAPackageThePoolDoesNotHold covers both halves of the
// same defect. On the apt path it must be caught at Plan, before anything is
// unpacked. On the --dpkg path it is the more serious one: applyDpkg unpacks
// whatever .deb files the pool walk finds and reports ToInstall out of the
// LOCK, so a lock entry with no file behind it used to come back as an
// applied, OK install of a package that was never installed at all.
func TestExecute_LockNamesAPackageThePoolDoesNotHold(t *testing.T) {
	// The pool holds jq and libjq1, but not libonig5.
	setup := func(t *testing.T) (bundleDir string, deps Deps) {
		t.Helper()
		lk := simpleLock()
		bundleDir = newFixtureBundle(t, fixtureOptions{Lock: lk})
		gone := filepath.Join(bundleDir, repoDirName, filepath.FromSlash(lk.Packages[2].Filename))
		if _, err := os.Stat(gone); err != nil {
			t.Fatalf("fixture should have written %s: %v", gone, err)
		}
		if err := os.Remove(gone); err != nil {
			t.Fatalf("remove pool file: %v", err)
		}
		deps = baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
		deps.Root = newRootFixture(t, "debian", "12", "bookworm")
		t.Setenv("FAKE_ARCH", "amd64")
		t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
		return bundleDir, deps
	}

	hasProblem := func(t *testing.T, problems []string) {
		t.Helper()
		for _, p := range problems {
			if strings.Contains(p, "libonig5") && strings.Contains(p, "does not hold") {
				return
			}
		}
		t.Fatalf("Problems = %v, want one naming libonig5 as missing from the pool", problems)
	}

	t.Run("Plan reports it", func(t *testing.T) {
		bundleDir, deps := setup(t)
		report, err := New(deps).Plan(context.Background(), bundleDir, Options{})
		if err != nil {
			t.Fatalf("Plan itself must not hard-fail: %v", err)
		}
		if report.OK {
			t.Errorf("OK = true, want false")
		}
		hasProblem(t, report.Problems)
	})

	t.Run("Apply refuses with exit 4", func(t *testing.T) {
		bundleDir, deps := setup(t)
		log := newFakeLogFile(t)
		report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true})
		if err == nil {
			t.Fatalf("expected a refusal")
		}
		if got := dferr.ClassOf(err); got != dferr.Verification {
			t.Errorf("class = %v (exit %d), want Verification (exit 4): %v", got, dferr.ExitCode(err), err)
		}
		if report.Applied {
			t.Errorf("Applied = true, want false")
		}
		for _, argv := range fakeArgvLog(t, log) {
			if containsToken(argv, "install") && !containsToken(argv, "-s") {
				t.Errorf("a mutating apt-get install ran despite the refusal: %v", argv)
			}
		}
	})

	t.Run("--dpkg refuses instead of reporting a false success", func(t *testing.T) {
		bundleDir, deps := setup(t)
		log := newFakeLogFile(t)
		report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true})
		if err == nil {
			t.Fatalf("expected a refusal; --dpkg would otherwise unpack the two files it found and report OK")
		}
		if got := dferr.ClassOf(err); got != dferr.Verification {
			t.Errorf("class = %v (exit %d), want Verification (exit 4): %v", got, dferr.ExitCode(err), err)
		}
		if report.Applied || report.OK {
			t.Errorf("Applied/OK = %v/%v, want false/false", report.Applied, report.OK)
		}
		for _, argv := range fakeArgvLog(t, log) {
			if containsToken(argv, "--unpack") || containsToken(argv, "--configure") {
				t.Errorf("dpkg mutated the system despite the refusal: %v", argv)
			}
		}
	})
}

// TestMissingPoolFiles_RefusesEscapingFilename: the filename in a lock is a
// document field, and this is the one place install turns one into a
// filesystem path. It must never be followed out of the bundle, whatever
// core/lock did or did not reject on the way in.
func TestMissingPoolFiles_RefusesEscapingFilename(t *testing.T) {
	repo := t.TempDir()
	cases := []string{
		"../../../etc/shadow",
		"/etc/shadow",
		"",
		"pool/../../outside.deb",
	}
	for _, fn := range cases {
		t.Run(fn, func(t *testing.T) {
			got := missingPoolFiles(repo, []lock.Package{{Name: "evil", Arch: "amd64", Filename: fn}})
			if len(got) != 1 {
				t.Fatalf("missingPoolFiles = %v, want exactly one problem", got)
			}
			if !strings.Contains(got[0], "not a path inside the bundle") {
				t.Errorf("problem = %q, want it to say the filename is not inside the bundle", got[0])
			}
		})
	}
}

// --- preconditions ---------------------------------------------------------

func TestExecute_MissingDpkg_IsEnvironment(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.DpkgPath = filepath.Join(t.TempDir(), "no-such-dpkg")
	log := newFakeLogFile(t)

	report, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Errorf("class = %v (exit %d), want Environment (exit 2): %v", got, dferr.ExitCode(err), err)
	}
	if !strings.Contains(dferr.HintOf(err), "dpkg") {
		t.Errorf("hint = %q, want it to tell the operator to install dpkg", dferr.HintOf(err))
	}
	if report.Applied {
		t.Errorf("Applied = true, want false")
	}
	if entries := fakeArgvLog(t, log); len(entries) != 0 {
		t.Errorf("nothing may be executed once a required binary is missing: %v", entries)
	}
}

func TestExecute_MissingApt_IsEnvironment(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.AptPath = filepath.Join(t.TempDir(), "no-such-apt-get")
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")

	_, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Fatalf("class = %v (exit %d), want Environment (exit 2): %v", got, dferr.ExitCode(err), err)
	}
	if !strings.Contains(dferr.HintOf(err), "--dpkg") {
		t.Errorf("hint = %q, want it to mention the --dpkg way out", dferr.HintOf(err))
	}

	// ... and --dpkg, which never calls apt-get, must not require it.
	if _, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true}); err != nil {
		t.Errorf("--dpkg must not require apt-get: %v", err)
	}
}

// TestExecute_DpkgArchQueryFails_IsEnvironment: dpkg is on PATH but cannot
// answer. install must not fall through with an empty architecture, which
// would silently disable the exit-7 architecture check below it.
func TestExecute_DpkgArchQueryFails_IsEnvironment(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	t.Setenv("FAKE_DPKG_ARCH_EXIT", "1")

	_, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Errorf("class = %v (exit %d), want Environment (exit 2): %v", got, dferr.ExitCode(err), err)
	}
}

func TestExecute_AptUpdateFails_IsEnvironment(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_UPDATE_EXIT", "100")
	log := newFakeLogFile(t)

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Errorf("class = %v (exit %d), want Environment (exit 2): %v", got, dferr.ExitCode(err), err)
	}
	if report.Applied || report.OK {
		t.Errorf("Applied/OK = %v/%v, want false/false", report.Applied, report.OK)
	}
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "install") {
			t.Errorf("apt-get install ran after a failed update: %v", argv)
		}
	}
}

// --- a failure must never come back as a success ---------------------------

// TestApply_AptInstallFails_IsNotASuccess is the shape a shell pipe hides:
// the real install ran, apt exited non-zero, and the only thing that carries
// that fact is the exit code and OK. Both are asserted here, together with
// the durable evidence record, which must not say ok:true either.
func TestApply_AptInstallFails_IsNotASuccess(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	t.Setenv("FAKE_APT_INSTALL_EXIT", "100")
	evidencePath := filepath.Join(t.TempDir(), "evidence.ndjson")

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, EvidencePath: evidencePath})
	if err == nil {
		t.Fatalf("expected an error when apt-get install exits non-zero")
	}
	if got := dferr.ClassOf(err); got != dferr.Resolution {
		t.Errorf("class = %v (exit %d), want Resolution (exit 5): %v", got, dferr.ExitCode(err), err)
	}
	if !report.Applied {
		t.Errorf("Applied = false: the mutating call did run and the report must say so")
	}
	if report.OK {
		t.Errorf("OK = true after a failed apt-get install")
	}
	if len(report.Problems) == 0 {
		t.Errorf("expected the apt failure recorded in Problems")
	}

	data, rerr := os.ReadFile(evidencePath)
	if rerr != nil {
		t.Fatalf("read evidence log: %v", rerr)
	}
	var rec struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("evidence line is not valid JSON: %v", err)
	}
	if ok, _ := rec.Data["ok"].(bool); ok {
		t.Errorf("the durable evidence record says ok:true for a failed install: %s", data)
	}
}

func TestApply_DpkgFailures_AreNotSuccesses(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string // substring of the recorded Problem
	}{
		{"unpack fails", "FAKE_DPKG_UNPACK_EXIT", "dpkg --unpack"},
		{"configure fails", "FAKE_DPKG_CONFIGURE_EXIT", "dpkg --configure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lk := simpleLock()
			bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
			deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
			deps.Root = newRootFixture(t, "debian", "12", "bookworm")
			t.Setenv("FAKE_ARCH", "amd64")
			t.Setenv(tc.env, "1")

			report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true})
			if err == nil {
				t.Fatalf("expected an error")
			}
			if got := dferr.ClassOf(err); got != dferr.Resolution {
				t.Errorf("class = %v (exit %d), want Resolution (exit 5): %v", got, dferr.ExitCode(err), err)
			}
			if report.OK {
				t.Errorf("OK = true after %s", tc.name)
			}
			found := false
			for _, p := range report.Problems {
				if strings.Contains(p, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("Problems = %v, want one naming %q", report.Problems, tc.want)
			}
		})
	}
}

// TestApply_EvidenceLogFailure_IsAWarningNotASuccessFlip: the evidence
// record is written after the packages are in. Failing to write it must be
// reported, and must not retroactively turn a completed install into a
// failure - nor be swallowed.
func TestApply_EvidenceLogFailure_IsAWarningNotASuccessFlip(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))

	// A path whose parent is a regular file: MkdirAll fails on any platform.
	blocked := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{
		Yes:          true,
		EvidencePath: filepath.Join(blocked, "install-log.ndjson"),
	})
	if err != nil {
		t.Fatalf("a failed evidence write must not fail the install: %v", err)
	}
	if !report.OK || !report.Applied {
		t.Fatalf("report = %+v", report)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "could not write local evidence log") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want one about the evidence log", report.Warnings)
	}
}

// --- bad input -------------------------------------------------------------

func TestExecute_BadBundlePath(t *testing.T) {
	deps := baseDeps(t, &fakeVerifier{})
	cases := []struct {
		name string
		path func(t *testing.T) string
		want string
	}{
		{
			// apt file: URIs break on a path containing a space, so this is
			// refused up front with a sentence about the real reason rather
			// than left to surface as an apt parse error later.
			name: "whitespace in the path",
			path: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "bundle with space")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want: "whitespace",
		},
		{
			name: "not a directory",
			path: func(t *testing.T) string {
				f := filepath.Join(t.TempDir(), "bundle.debark.tar.zst")
				if err := os.WriteFile(f, []byte("archive"), 0o644); err != nil {
					t.Fatal(err)
				}
				return f
			},
			want: "not a directory",
		},
		{
			name: "does not exist",
			path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			want: "bundle path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &fakeVerifier{}
			d := deps
			d.Verifier = v
			report, err := New(d).Plan(context.Background(), tc.path(t), Options{})
			if err == nil {
				t.Fatalf("expected an error")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %v (exit %d), want Usage (exit 1): %v", got, dferr.ExitCode(err), err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if v.calls != 0 {
				t.Errorf("the verifier ran on an unusable bundle path")
			}
			if report == nil || report.FinishedAt == "" {
				t.Errorf("even a refusal must come back as a finished report: %+v", report)
			}
		})
	}
}

// TestExecute_UnusableLock_IsUsage: verify proves the bytes were not
// tampered with; lock.Validate is the separate check that the bytes that did
// survive are self-consistent. A lock that fails it must stop the run before
// an apt-get argv is built out of it.
func TestExecute_UnusableLock_IsUsage(t *testing.T) {
	cases := []struct {
		name  string
		mutle func(lk *lock.Lock)
	}{
		{
			// An install entry naming a version no package entry carries.
			name: "install entry does not match any package",
			mutle: func(lk *lock.Lock) {
				lk.Install = []string{"jq:amd64=9.9.9-1"}
			},
		},
		{
			// closed_world_check.result is required and has three legal
			// values; the zero value is not one of them.
			name:  "closed-world result missing",
			mutle: func(lk *lock.Lock) { lk.ClosedWorld = lock.ClosedWorld{} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lk := simpleLock()
			tc.mutle(&lk)
			bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
			deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
			log := newFakeLogFile(t)

			_, err := New(deps).Plan(context.Background(), bundleDir, Options{})
			if err == nil {
				t.Fatalf("expected an error")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %v (exit %d), want Usage (exit 1): %v", got, dferr.ExitCode(err), err)
			}
			if entries := fakeArgvLog(t, log); len(entries) != 0 {
				t.Errorf("apt/dpkg ran against a lock that does not load: %v", entries)
			}
		})
	}
}

func TestExecute_OnlyNamesSomethingNotInTheLock_IsUsage(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	log := newFakeLogFile(t)

	_, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, Only: []string{"jq", "not-in-the-bundle"}})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("class = %v (exit %d), want Usage (exit 1): %v", got, dferr.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "not-in-the-bundle") {
		t.Errorf("err = %v, want it to name the package that is not there", err)
	}
	for _, argv := range fakeArgvLog(t, log) {
		if containsToken(argv, "install") {
			t.Errorf("apt-get install ran despite an unusable --only: %v", argv)
		}
	}
}

// TestApply_DpkgFallback_EmptyPool: --dpkg with nothing to unpack is a
// failure, not a no-op success. (With every lock entry's file missing the
// pool-presence check fires first, so this is the empty-lock case: nothing
// selected, nothing in the pool.)
func TestApply_DpkgFallback_EmptyPool(t *testing.T) {
	lk := lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Target:        basicLockTarget(),
		ClosedWorld:   lock.ClosedWorld{Result: lock.ClosedWorldSkipped},
	}
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true})
	if err == nil {
		t.Fatalf("expected an error: there is nothing to unpack")
	}
	if report.OK {
		t.Errorf("OK = true with an empty pool")
	}
}
