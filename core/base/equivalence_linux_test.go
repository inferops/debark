//go:build linux

package base_test

// The one property this whole feature turns on, checked against a real apt.
//
// `build --base X` must produce the same bundle as `snapshot from-base X`
// followed by `build --snapshot`. If those two can diverge then there are two
// resolution paths through the engine and the architectural rule of ADR-014
// has been broken — which would matter, because the second path would be the
// one nothing else in the suite exercises.
//
// This is an end-to-end test with a real apt and a real archive, so it is
// linux-only and gated on DEBARK_E2E, exactly like core/apt's own e2e
// tests. Run it with:
//
//	DEBARK_E2E=1 go test ./core/base/ -run TestBuildFromBaseEqualsFromBaseThenBuild -v
//
// or through hack/linux-test.sh, which supplies the container.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// equivalenceBase is deliberately the smallest builtin base. The property
// under test is about which code path ran, not about how big a closure is,
// and minimal resolves in seconds where desktop pulls eight hundred packages.
const equivalenceBase = "ubuntu:24.04/minimal"

// equivalencePackage is what both bundles are asked for. jq is the project's
// standing example: it needs two libraries the base does not have, so the
// bundle is genuinely resolved rather than trivially empty.
const equivalencePackage = "jq"

func TestBuildFromBaseEqualsFromBaseThenBuild(t *testing.T) {
	if os.Getenv("DEBARK_E2E") == "" {
		t.Skip("set DEBARK_E2E=1 to run: this needs a real apt and network access to the distro archive")
	}
	requireHostDistroOf(t, equivalenceBase)
	bin := buildDebark(t)
	work := t.TempDir()

	// A fixed clock, or the two runs stamp different times into the lock and
	// the manifest and the comparison below is meaningless. This is the same
	// mechanism core/engine and core/base both honour through
	// canonical.EffectiveTime.
	//
	// A SEPARATE STORE for each path is just as load-bearing, and less
	// obvious. With one shared store the second build finds every .deb
	// already ingested and records DownloadedBytes = 0 against the first
	// build's real figure — a difference in what the machine had cached
	// rather than in what the two paths decided. Measured: with a shared
	// store this test failed on lock.json and manifest alone, with an
	// identical pool and an identical snapshot.
	env := append(os.Environ(), "SOURCE_DATE_EPOCH=1757000000")

	bundleA := filepath.Join(work, "bundle-a")
	runDebark(t, bin, append(env, "DEBARK_STORE_DIR="+filepath.Join(work, "store-a")),
		"build", "--base", equivalenceBase, "--backend", "local",
		"--out", bundleA, "--no-sign", equivalencePackage)

	snap := filepath.Join(work, "eq.snapshot.tar.zst")
	storeB := "DEBARK_STORE_DIR=" + filepath.Join(work, "store-b")
	runDebark(t, bin, append(env, storeB),
		"snapshot", "from-base", equivalenceBase, "--backend", "local", "--out", snap)
	bundleB := filepath.Join(work, "bundle-b")
	runDebark(t, bin, append(env, storeB),
		"build", "--snapshot", snap, "--backend", "local",
		"--out", bundleB, "--no-sign", equivalencePackage)

	for _, name := range []string{"snapshot.json", "lock.json", "debark.manifest.json"} {
		a := mustDigestFile(t, filepath.Join(bundleA, name))
		b := mustDigestFile(t, filepath.Join(bundleB, name))
		if a != b {
			t.Errorf("%s differs between the two paths:\n  build --base       %s\n  from-base + build  %s\n"+
				"the two must be one code path; a difference here means build branches on how the snapshot was made", name, a, b)
		}
	}

	poolA := mustPoolListing(t, bundleA)
	poolB := mustPoolListing(t, bundleB)
	if len(poolA) == 0 {
		// Without this the test passes when both pools are empty, which is
		// the vacuous-pass shape this project has paid for repeatedly — and
		// which the shell version of this experiment actually hit.
		t.Fatal("bundle-a has an empty pool, so nothing was compared")
	}
	if strings.Join(poolA, "\n") != strings.Join(poolB, "\n") {
		t.Errorf("the two pools differ:\n  A: %v\n  B: %v", poolA, poolB)
	}
}

// requireHostDistroOf skips unless the host runs the same distro as base.
//
// Both halves of this test resolve with `--backend local`, and apt cannot
// resolve one distro's archive from another distro's host: core/apt's
// localMismatchReason refuses that outright ("host distro %q does not match
// target distro %q"), by design and correctly. On a host that is not the
// base's distro there is therefore nothing here to measure, and skipping
// says so — where failing would report a deliberate, documented refusal as
// if the equivalence property had broken.
//
// This is load-bearing in CI: the e2e-apt job runs this suite in both a
// Debian 12 and an Ubuntu 24.04 container, and equivalenceBase is an Ubuntu
// base. The Ubuntu leg is where this test does its work; the Debian leg
// skips. Point equivalenceBase at a Debian base and the two swap over with
// no change here.
func requireHostDistroOf(t *testing.T, base string) {
	t.Helper()
	want, _, ok := strings.Cut(base, ":")
	if !ok || want == "" {
		t.Fatalf("equivalenceBase %q is not in \"distro:version/variant\" form", base)
	}
	const osRelease = "/etc/os-release"
	data, err := os.ReadFile(osRelease)
	if err != nil {
		t.Skipf("cannot read %s to check the host distro against %s: %v", osRelease, base, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), "ID=")
		if !found {
			continue
		}
		got := strings.Trim(strings.TrimSpace(rest), "\"'")
		if !strings.EqualFold(got, want) {
			t.Skipf("host distro is %q but %s needs a %q host: `--backend local` refuses a cross-distro resolve", got, base, want)
		}
		return
	}
	t.Skipf("%s names no ID=, so the host distro cannot be checked against %s", osRelease, base)
}

// buildDebark compiles the CLI once for this test. Going through the real
// binary rather than calling cli.Execute in-process is deliberate: `build
// --base` re-execs itself for the container backend and reads its own
// os.Executable, and a test that never produced a binary could not catch a
// break in that.
func buildDebark(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "debark")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/inferops/debark/cmd/debark")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runDebark(t *testing.T, bin string, env []string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("debark %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	t.Logf("debark %s\n%s", strings.Join(args, " "), out)
}

func mustDigestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty", path)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mustPoolListing returns "<repo-relative path> <sha256>" for every file in
// the bundle's pool, sorted — the content of the pool, not just its shape, so
// two bundles holding differently-built .deb files with the same names still
// come out different.
func mustPoolListing(t *testing.T, bundleDir string) []string {
	t.Helper()
	root := filepath.Join(bundleDir, "repo", "pool")
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel)+" "+mustDigestFile(t, p))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}
