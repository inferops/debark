package install

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// pinPermissions replaces the pathPermissions seam with a table keyed by
// path, so the permission logic is exercised identically on every platform
// this suite runs on - including Windows, whose os.Stat has no group or
// other bits to report at all. Any path not in the table is 0755 and
// readable, i.e. "nothing wrong with it".
func pinPermissions(t *testing.T, modes map[string]fs.FileMode, errs map[string]error) {
	t.Helper()
	old := pathPermissions
	t.Cleanup(func() { pathPermissions = old })
	pathPermissions = func(path string) (fs.FileMode, bool, error) {
		clean := filepath.Clean(path)
		if err, ok := errs[clean]; ok {
			return 0, false, err
		}
		if m, ok := modes[clean]; ok {
			return m | fs.ModeDir, true, nil
		}
		return 0o755 | fs.ModeDir, true, nil
	}
}

func TestKeepSourceTrustProblem(t *testing.T) {
	repo := filepath.Clean("/srv/bundle/repo")
	parent := filepath.Clean("/srv/bundle")
	grand := filepath.Clean("/srv")

	cases := []struct {
		name  string
		modes map[string]fs.FileMode
		errs  map[string]error
		want  string // substring; "" means "no problem"
	}{
		{name: "root-owned 0755 chain", want: ""},
		{
			name:  "repo world-writable",
			modes: map[string]fs.FileMode{repo: 0o777},
			want:  "group- and world-writable",
		},
		{
			name:  "repo group-writable only",
			modes: map[string]fs.FileMode{repo: 0o775},
			want:  "group-writable",
		},
		{
			name:  "repo other-writable only",
			modes: map[string]fs.FileMode{repo: 0o757},
			want:  "world-writable",
		},
		{
			name:  "ancestor world-writable, not sticky",
			modes: map[string]fs.FileMode{parent: 0o777},
			want:  "is world-writable and not sticky",
		},
		{
			// /tmp and /var/tmp are 1777 by design and are a legitimate
			// place to extract a bundle: sticky is exactly the bit that
			// stops a non-owner swapping the directory out.
			name:  "ancestor world-writable but sticky",
			modes: map[string]fs.FileMode{parent: 0o777 | fs.ModeSticky},
			want:  "",
		},
		{
			// A shared, group-owned staging directory is a normal way to
			// stage a bundle; the group already had to be trusted to put it
			// there.
			name:  "ancestor group-writable",
			modes: map[string]fs.FileMode{grand: 0o775 | fs.ModeSetgid},
			want:  "",
		},
		{
			name: "repo permissions unreadable",
			errs: map[string]error{repo: errors.New("permission denied")},
			want: "cannot check the permissions of",
		},
		{
			name: "ancestor permissions unreadable",
			errs: map[string]error{parent: errors.New("permission denied")},
			want: "an ancestor of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinPermissions(t, tc.modes, tc.errs)
			got := keepSourceTrustProblem(repo)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("keepSourceTrustProblem = %q, want no problem", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("keepSourceTrustProblem = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestKeepSourceTrustProblem_UnknownPermissions covers the platform where
// os.Stat's mode says nothing: refusing every bundle there would be a
// refusal for a reason that does not exist.
func TestKeepSourceTrustProblem_UnknownPermissions(t *testing.T) {
	old := pathPermissions
	t.Cleanup(func() { pathPermissions = old })
	pathPermissions = func(string) (fs.FileMode, bool, error) { return 0o777 | fs.ModeDir, false, nil }
	if got := keepSourceTrustProblem(filepath.Clean("/srv/bundle/repo")); got != "" {
		t.Fatalf("keepSourceTrustProblem = %q, want no problem when the platform reports no usable bits", got)
	}
}

// TestExecute_KeepSource_RefusesWritableRepo is the teeth of item 1's
// refusal: --keep-source over a directory other users can write is a
// standing root-level install channel, and install must decline it BEFORE
// installing anything - on Plan as well as Apply, so --dry-run answers the
// question honestly.
func TestExecute_KeepSource_RefusesWritableRepo(t *testing.T) {
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
			root := newRootFixture(t, "debian", "12", "bookworm")
			deps.Root = root
			t.Setenv("FAKE_ARCH", "amd64")
			t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
			log := newFakeLogFile(t)
			pinPermissions(t, map[string]fs.FileMode{
				filepath.Clean(filepath.Join(bundleDir, repoDirName)): 0o777,
			}, nil)
			r := New(deps)

			var err error
			var report *Report
			if apply {
				report, err = r.Apply(context.Background(), bundleDir, Options{Yes: true, KeepSource: true})
			} else {
				report, err = r.Plan(context.Background(), bundleDir, Options{KeepSource: true})
			}
			if err == nil {
				t.Fatalf("expected a refusal")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %v (exit %d), want Usage (exit 1): %v", got, dferr.ExitCode(err), err)
			}
			if !strings.Contains(err.Error(), "keep-source") {
				t.Errorf("error should name the flag it refused: %v", err)
			}
			if dferr.HintOf(err) == "" {
				t.Errorf("expected a hint telling the operator the way out")
			}
			if report == nil || report.Applied {
				t.Errorf("Applied = true, want false: nothing may have been installed")
			}
			// The refusal happens before the private apt root is built, so
			// apt-get was never invoked at all - not even `update`.
			for _, argv := range fakeArgvLog(t, log) {
				if containsToken(argv, "update") || containsToken(argv, "install") {
					t.Errorf("apt-get ran despite the refusal: %v", argv)
				}
			}
			// And nothing was written under the system root.
			dest := filepath.Join(root, "etc", "apt", "sources.list.d", sourcesFileName)
			if _, statErr := os.Stat(dest); statErr == nil {
				t.Errorf("%s was written despite the refusal", dest)
			}
		})
	}
}

// TestExecute_KeepSource_AllowedOnASafeDirectory is the other half: the
// refusal must not fire on an ordinary root-owned bundle directory, or the
// flag would simply be unusable.
func TestExecute_KeepSource_AllowedOnASafeDirectory(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	pinPermissions(t, map[string]fs.FileMode{
		filepath.Clean(filepath.Join(bundleDir, repoDirName)): 0o755,
	}, nil)

	if _, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, KeepSource: true}); err != nil {
		t.Fatalf("--keep-source over a 0755 directory must be allowed: %v", err)
	}
}

// TestApply_KeepSource_WarnsAboutStandingTrust is item 1's headline: the
// warning has to appear on SUCCESS. Before this, the only way --keep-source
// produced a Warning was by failing.
func TestApply_KeepSource_WarnsAboutStandingTrust(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	root := newRootFixture(t, "debian", "12", "bookworm")
	deps.Root = root
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, KeepSource: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !report.OK || !report.Applied {
		t.Fatalf("report = %+v", report)
	}

	dest := filepath.Join(root, "etc", "apt", "sources.list.d", sourcesFileName)
	repoDir, absErr := filepath.Abs(filepath.Join(bundleDir, repoDirName))
	if absErr != nil {
		t.Fatal(absErr)
	}
	var found string
	for _, w := range report.Warnings {
		if strings.Contains(w, "keep-source") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("a successful --keep-source install produced no standing-trust warning; warnings = %v", report.Warnings)
	}
	// The warning is only useful if it names the two things the operator has
	// to act on: the file to delete, and the directory that is now trusted.
	if !strings.Contains(found, dest) {
		t.Errorf("warning does not name the persisted file %s: %q", dest, found)
	}
	if !strings.Contains(found, repoDir) {
		t.Errorf("warning does not name the trusted directory %s: %q", repoDir, found)
	}
	for _, want := range []string{"root", "future"} {
		if !strings.Contains(found, want) {
			t.Errorf("warning should say what the standing trust means (%q missing): %q", want, found)
		}
	}

	// A plain install must NOT carry it - a warning that appears every time
	// is not a warning.
	plain := newFixtureBundle(t, fixtureOptions{Lock: lk})
	plainReport, err := New(deps).Apply(context.Background(), plain, Options{Yes: true})
	if err != nil {
		t.Fatalf("plain Apply: %v", err)
	}
	for _, w := range plainReport.Warnings {
		if strings.Contains(w, "keep-source") {
			t.Errorf("an install without --keep-source carried the standing-trust warning: %q", w)
		}
	}
}

// TestApply_KeepSource_WithDpkg_SaysItDidNothing: --dpkg never builds an apt
// source, so PersistSource is never reached and the flag is a no-op. Saying
// so beats letting a flag the operator typed vanish.
func TestApply_KeepSource_WithDpkg_SaysItDidNothing(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	root := newRootFixture(t, "debian", "12", "bookworm")
	deps.Root = root
	t.Setenv("FAKE_ARCH", "amd64")
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true, KeepSource: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "--keep-source does nothing with --dpkg") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning that --keep-source did nothing; warnings = %v", report.Warnings)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc", "apt", "sources.list.d", sourcesFileName)); statErr == nil {
		t.Errorf("--dpkg must not write an apt source")
	}
}

// TestApply_KeepSource_PersistFailure_StaysAWarning pins the existing
// contract for the other direction: by the time PersistSource runs the
// packages are already installed, so a failure to write the .sources file
// is reported, not turned into a failed install.
func TestApply_KeepSource_PersistFailure_StaysAWarning(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)

	// A root whose "etc" is a regular file: MkdirAll(root/etc/apt/...) then
	// fails on every platform, with no need for a permission trick.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "etc"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Root = root
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	r := New(deps)

	report, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, KeepSource: true})
	if err != nil {
		t.Fatalf("a failed PersistSource must not fail the install: %v", err)
	}
	if !report.OK || !report.Applied {
		t.Fatalf("report = %+v", report)
	}
	var warned, standing bool
	for _, w := range report.Warnings {
		if strings.Contains(w, "could not persist source") {
			warned = true
		}
		if strings.Contains(w, "will now install anything found in") {
			standing = true
		}
	}
	if !warned {
		t.Errorf("expected a 'could not persist source' warning; warnings = %v", report.Warnings)
	}
	if standing {
		t.Errorf("the standing-trust warning must not be emitted when nothing was persisted: %v", report.Warnings)
	}
}
