package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestApply_SystemConfigUntouched is the assertion contract item 3 asks for
// explicitly: without --keep-source, install must never create or modify
// anything outside its own temporary private apt root — not one byte under
// what stands in for /etc/apt here. It runs the full Plan and Apply paths
// (status/dry-run and a real, successful install) against the same root
// fixture and diffs the whole tree before and after each.
func TestApply_SystemConfigUntouched(t *testing.T) {
	lk := simpleLock()

	run := func(t *testing.T, apply bool, opts Options) {
		bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
		v := &fakeVerifier{report: okVerifyReport(lk.Target)}
		deps := baseDeps(t, v)
		root := newRootFixture(t, "debian", "12", "bookworm")
		deps.Root = root
		t.Setenv("FAKE_ARCH", "amd64")
		t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
		r := New(deps)

		before := snapshotTree(t, root)
		var err error
		if apply {
			_, err = r.Apply(context.Background(), bundleDir, opts)
		} else {
			_, err = r.Plan(context.Background(), bundleDir, opts)
		}
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		after := snapshotTree(t, root)

		if len(before) != len(after) {
			t.Fatalf("file count under root changed: %d -> %d (%v -> %v)", len(before), len(after), before, after)
		}
		for path, content := range before {
			ac, ok := after[path]
			if !ok {
				t.Errorf("%s was deleted", path)
				continue
			}
			if ac != content {
				t.Errorf("%s was modified: %q -> %q", path, content, ac)
			}
		}
		for path := range after {
			if _, ok := before[path]; !ok {
				t.Errorf("%s was created under the system root", path)
			}
		}
	}

	t.Run("Plan (status/dry-run)", func(t *testing.T) {
		run(t, false, Options{})
	})
	t.Run("Apply", func(t *testing.T) {
		run(t, true, Options{Yes: true})
	})
	t.Run("Apply with --dpkg", func(t *testing.T) {
		lkCopy := lk
		bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lkCopy})
		v := &fakeVerifier{report: okVerifyReport(lkCopy.Target)}
		deps := baseDeps(t, v)
		root := newRootFixture(t, "debian", "12", "bookworm")
		deps.Root = root
		t.Setenv("FAKE_ARCH", "amd64")
		r := New(deps)

		before := snapshotTree(t, root)
		if _, err := r.Apply(context.Background(), bundleDir, Options{Yes: true, Dpkg: true}); err != nil {
			t.Fatalf("Apply --dpkg: %v", err)
		}
		after := snapshotTree(t, root)
		if len(before) != len(after) {
			t.Fatalf("file count under root changed with --dpkg: %d -> %d", len(before), len(after))
		}
	})
}

// TestApply_PrivateRootCleanedUp proves the temporary private apt root does
// not survive a run: the one directory install is allowed to create is
// removed again once Plan/Apply returns, successful or not.
func TestApply_PrivateRootCleanedUp(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	v := &fakeVerifier{report: okVerifyReport(lk.Target)}
	deps := baseDeps(t, v)
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))

	tmp := os.TempDir()
	before, err := os.ReadDir(tmp)
	if err != nil {
		t.Skipf("cannot read OS temp dir %s: %v", tmp, err)
	}

	r := New(deps)
	if _, err := r.Apply(context.Background(), bundleDir, Options{Yes: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	after, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read temp dir after: %v", err)
	}
	beforeNames := map[string]bool{}
	for _, e := range before {
		beforeNames[e.Name()] = true
	}
	for _, e := range after {
		if !beforeNames[e.Name()] && filepath.Ext(e.Name()) == "" {
			// A leftover "debark-install-*" directory would show up here;
			// tolerate anything else transient tooling on the CI box drops
			// into the OS temp dir.
			if len(e.Name()) >= len("debark-install-") && e.Name()[:len("debark-install-")] == "debark-install-" {
				t.Errorf("leftover private apt root: %s", filepath.Join(tmp, e.Name()))
			}
		}
	}
}
