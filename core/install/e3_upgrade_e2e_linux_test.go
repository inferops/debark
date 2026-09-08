//go:build linux

// This file reproduces, end to end against a real apt-get/dpkg, the exact
// divergence experiment E3 measured (docs/experiments/E3-pin-fidelity.md)
// and proves this fix closes it. It is guarded off by default (see
// requireLinuxAPT, the contract brief's standard guard) and does not run on
// the Windows/macOS/CI-default unit test run.
//
// It lives in package install (not install_test): the one other thing this
// needs, besides a real apt-get/dpkg, is a real (non-faked) execCommandContext
// — this package's own TestMain (exechelper_test.go) points that package
// variable at a fake for the whole test binary so every other test here runs
// with no real apt-get/dpkg anywhere, and staying in-package lets this test
// simply restore the real implementation for its own duration (t.Cleanup
// restores the fake afterwards) — the exact pattern runner_test.go already
// uses for stdinIsTerminal, rather than the separate-driver-binary+docker
// indirection e2e_linux_test.go needs for its own, different goal of proving
// "--network none". This test does not need network isolation: it needs a
// real apt-get to disagree with a locked version, which requires nothing
// more than running where a real apt-get lives (hack/linux-test.sh's own
// container already provides that, disposably, via `docker run --rm`).
//
// Run it with:
//
//	DEBARK_E2E=1 bash hack/linux-test.sh ./core/install/...
package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/verify"
)

// requireLinuxAPT is the contract brief's standard guard
// (docs/dev/contract-brief.md).
func requireLinuxAPT(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("DEBARK_E2E") == "" {
		t.Skip("needs Linux with apt; set DEBARK_E2E=1")
	}
}

// TestE2E_UpgradeObeysLock_E3Scenario is E3's own scenario
// (hack/experiments/e3-pin-fidelity.sh's technique, reused rather than
// reinvented — see each step below for exactly which part it mirrors),
// built directly as a Go fixture instead of shelling out to the bash script,
// so it runs as part of this package's own Go test suite and asserts
// against install.Runner's real behaviour rather than parsing apt's raw
// text output by hand.
//
//  1. Two real .deb files, same package name, different versions — E3 step 1
//     (a hand-written DEBIAN/control + dpkg-deb --build placeholder, not a
//     real network archive: "Real network PPAs would make this experiment
//     non-reproducible").
//  2. The LOWER version installed for real via dpkg -i into this container's
//     own, real, disposable dpkg database — E3 step 3's "really
//     (non-simulated) install that same package, so a second, equally real
//     dpkg status exists with the package already present" (E1's pitfall
//     note: a hand-rolled single-stanza status misbehaves under
//     upgrade-style resolution; using the container's own real database,
//     exactly as E1 and E3 both do, sidesteps that categorically).
//  3. A flat bundle repo containing BOTH versions (Origin: debark,
//     Suite: bundle) — E3's "bundle-accumulated" fixture: not a contrived
//     case, the normal outcome of the default additive mode.
//  4. A setup sanity check reproducing E3's own finding directly: a bare
//     "apt-get -s full-upgrade" against this exact bundle, with the lower
//     version already installed, must select the HIGHER version — E3's own
//     log-07c-bundleB-fullupgrade.txt ("Inst … [1.0-1] (2.0-1 …)"). This is
//     not the fix under test; it is proof the fixture actually reproduces
//     the divergence this fix exists to close, the same way E3 itself
//     confirmed the online choice before checking the offline one.
//  5. The actual proof: install.Runner, given a lock whose Reason: "upgrade"
//     entry names the LOWER (pin-preferred, online-chosen) version, installs
//     --upgrade and ends up with exactly that version — never the higher one
//     a free full-upgrade would have picked, and without install ever
//     calling apt-get full-upgrade at all (grep the recorded argv for it).
func TestE2E_UpgradeObeysLock_E3Scenario(t *testing.T) {
	requireLinuxAPT(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not on PATH")
	}

	// This is the one test in this package that needs a real apt-get/dpkg;
	// see the file doc comment for why restoring execCommandContext here
	// (rather than a separate driver binary) is enough.
	oldExec := execCommandContext
	execCommandContext = exec.CommandContext
	t.Cleanup(func() { execCommandContext = oldExec })

	ctx := context.Background()
	work := t.TempDir()

	arch := realDpkgPrintArchitecture(t)
	codename := realVersionCodename()

	const pkg = "debark-e3-demo"
	const lowerVersion = "1.0-1"  // the online, pin-preferred choice (E3's vendor origin, priority 900)
	const higherVersion = "2.0-1" // what a free full-upgrade prefers (E3's backports-like origin, priority 100 - but the higher version number)

	// --- 1. Two real .deb files. ---
	lowerDeb := buildPlaceholderDeb(t, work, pkg, lowerVersion, arch)
	higherDeb := buildPlaceholderDeb(t, work, pkg, higherVersion, arch)

	// --- 2. Real dpkg status: the lower version genuinely installed. ---
	runReal(t, "dpkg", "-i", lowerDeb)
	t.Cleanup(func() { _ = exec.Command("dpkg", "--purge", pkg).Run() })
	if got := realInstalledVersion(t, pkg); got != lowerVersion {
		t.Fatalf("setup: dpkg -i did not leave %s installed at %s (got %q)", pkg, lowerVersion, got)
	}

	// --- 3. Flat bundle repo with BOTH versions in the pool. ---
	bundleDir := filepath.Join(work, "bundle")
	repoDir := filepath.Join(bundleDir, "repo")
	prefix := pkg[:1]
	poolDir := filepath.Join(repoDir, "pool", prefix, pkg)
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lowerRel := fmt.Sprintf("pool/%s/%s/%s_%s_%s.deb", prefix, pkg, pkg, lowerVersion, arch)
	higherRel := fmt.Sprintf("pool/%s/%s/%s_%s_%s.deb", prefix, pkg, pkg, higherVersion, arch)
	copyFileReal(t, lowerDeb, filepath.Join(repoDir, filepath.FromSlash(lowerRel)))
	copyFileReal(t, higherDeb, filepath.Join(repoDir, filepath.FromSlash(higherRel)))

	lowerHash, err := digest.AllFile(filepath.Join(repoDir, filepath.FromSlash(lowerRel)))
	if err != nil {
		t.Fatal(err)
	}
	higherHash, err := digest.AllFile(filepath.Join(repoDir, filepath.FromSlash(higherRel)))
	if err != nil {
		t.Fatal(err)
	}

	packagesText := aptPackagesStanza(pkg, lowerVersion, arch, lowerRel, lowerHash) + "\n" +
		aptPackagesStanza(pkg, higherVersion, arch, higherRel, higherHash)
	packagesPath := filepath.Join(repoDir, "Packages")
	if err := os.WriteFile(packagesPath, []byte(packagesText), 0o644); err != nil {
		t.Fatal(err)
	}
	packagesHash, err := digest.AllFile(packagesPath)
	if err != nil {
		t.Fatal(err)
	}

	releaseText := "Origin: debark\n" +
		"Label: debark\n" +
		"Suite: bundle\n" +
		"Codename: " + codename + "\n" +
		"Architectures: " + arch + "\n" +
		"Components: main\n" +
		"Date: " + time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC") + "\n" +
		"MD5Sum:\n" +
		fmt.Sprintf(" %s %16d Packages\n", packagesHash.MD5, packagesHash.Size) +
		"SHA256:\n" +
		fmt.Sprintf(" %s %16d Packages\n", packagesHash.SHA256, packagesHash.Size)
	releasePath := filepath.Join(repoDir, "Release")
	if err := os.WriteFile(releasePath, []byte(releaseText), 0o644); err != nil {
		t.Fatal(err)
	}
	releaseHash, err := digest.AllFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}

	// --- 4. Setup sanity check: reproduce E3's own finding directly. ---
	if got := realSimulatedFullUpgradeVersion(t, ctx, repoDir, pkg); got != higherVersion {
		t.Fatalf("setup check failed: a bare 'apt-get -s full-upgrade' against this bundle selected %q for %s, want %q "+
			"(E3's own divergence must reproduce first, or this test proves nothing)", got, pkg, higherVersion)
	}
	t.Logf("setup confirmed E3's divergence reproduces: a bare full-upgrade against this bundle would pick %s over the locked %s", higherVersion, lowerVersion)

	// --- 5. The fix: install.Runner --upgrade must install the LOCK's
	// upgrade target (lowerVersion), never the free-upgrade pick. ---
	lk := lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		CreatedAt:     canonical.Time(time.Now()),
		Target:        lock.Target{DistroID: "debian", VersionID: "12", Codename: codename, Arch: arch},
		Resolver:      lock.Resolver{Backend: lock.BackendLocal, APTVersion: "e2e-fixture", DpkgVersion: "e2e-fixture", PhasedUpdates: "never-include"},
		Packages: []lock.Package{{
			Name: pkg, Arch: arch, Version: lowerVersion,
			Filename: lowerRel, Size: lowerHash.Size, SHA256: lowerHash.SHA256,
			// The online solve's own full-upgrade pass locked this at
			// lowerVersion (what a target pin, in play online where Release
			// still carried real origins, would have chosen) - this is the
			// one field that carries that decision to the target.
			Reason:                lock.ReasonUpgrade,
			PublisherVerification: lock.VerifiedAPTSigned,
		}},
		ClosedWorld: lock.ClosedWorld{Result: lock.ClosedWorldSkipped, Detail: "E2E fixture: hand-assembled, not resolved by apt"},
	}
	if _, err := lock.Save(bundleDir, &lk); err != nil {
		t.Fatalf("lock.Save: %v", err)
	}
	lockDigest, err := lock.Digest(&lk)
	if err != nil {
		t.Fatal(err)
	}
	lockHash, err := digest.AllFile(filepath.Join(bundleDir, lock.FileName))
	if err != nil {
		t.Fatal(err)
	}

	man := manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion, BundleID: "e3-e2e-test-bundle", CreatedAt: canonical.Time(time.Now()),
		FormatVersion: manifest.CurrentFormatVersion,
		Tool:          manifest.Tool{Name: "debark-e3-e2e-test", Version: "0", Edition: manifest.EditionCommunity},
		LockDigest:    lockDigest,
		Repository:    manifest.Repository{PackagesSHA256: packagesHash.SHA256, ReleaseSHA256: releaseHash.SHA256},
		Files: []manifest.File{
			{Path: lock.FileName, Size: lockHash.Size, SHA256: lockHash.SHA256},
			{Path: "repo/Packages", Size: packagesHash.Size, SHA256: packagesHash.SHA256},
			{Path: "repo/Release", Size: releaseHash.Size, SHA256: releaseHash.SHA256},
			{Path: "repo/" + lowerRel, Size: lowerHash.Size, SHA256: lowerHash.SHA256},
			{Path: "repo/" + higherRel, Size: higherHash.Size, SHA256: higherHash.SHA256},
		},
		Target: manifest.Target{DistroID: lk.Target.DistroID, VersionID: lk.Target.VersionID, Codename: lk.Target.Codename, Arch: lk.Target.Arch},
	}
	manBytes, err := canonical.MarshalIndent(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, manifest.FileName), manBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately unsigned, exactly like e2e_linux_test.go's own fixture:
	// Options.Verify.AllowUnsigned makes this legitimate for testing
	// install's own machinery without needing core/sign's signing infrastructure.

	// OnAptOutput records every apt-get/dpkg argv this run actually makes
	// (iface.go's own seam for exactly this): the direct, textual proof that
	// install --upgrade never once asks apt-get for a free full-upgrade,
	// not just an inference from the outcome below.
	var recordedArgv [][]string
	r := New(Deps{
		Verifier:    verify.New(),
		OnAptOutput: func(argv []string, _ []byte) { recordedArgv = append(recordedArgv, argv) },
	})
	report, err := r.Apply(ctx, bundleDir, Options{
		Upgrade: true, Yes: true,
		Verify: verify.Options{AllowUnsigned: true},
	})
	if err != nil {
		t.Fatalf("Apply: %v (problems=%v)", err, report.Problems)
	}
	if !report.Applied || !report.OK {
		t.Fatalf("report = %+v", report)
	}

	// The whole point: install --upgrade must never have asked apt for a
	// free full-upgrade (E3/ADR-007) - confirm it from the recorded argv,
	// not just from the outcome.
	for _, argv := range recordedArgv {
		// containsToken (runner_test.go, this same package) is the exact
		// helper the rest of this package's tests already use for this.
		if containsToken(argv, "full-upgrade") {
			t.Errorf("install --upgrade invoked apt-get full-upgrade (argv=%v); it must not (E3/ADR-007)", argv)
		}
	}

	installedVersion := realInstalledVersion(t, pkg)
	if installedVersion != lowerVersion {
		t.Fatalf("FIX FAILED: after 'install --upgrade', dpkg reports %s installed at %s; want %s (the lock's upgrade target). "+
			"A free full-upgrade against this same bundle picks %s instead - exactly E3's divergence, un-fixed.",
			pkg, installedVersion, lowerVersion, higherVersion)
	}
	t.Logf("install --upgrade correctly kept %s at the lock's exact upgrade target %s "+
		"(a bare full-upgrade against this same bundle would have picked %s - E3's divergence, closed)",
		pkg, lowerVersion, higherVersion)
}

// realDpkgPrintArchitecture runs the real dpkg --print-architecture (not
// through the fake exec machinery: this file already restored the real
// execCommandContext before calling this).
func realDpkgPrintArchitecture(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("dpkg", "--print-architecture").CombinedOutput()
	if err != nil {
		t.Fatalf("dpkg --print-architecture: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// realVersionCodename reads the real /etc/os-release's VERSION_CODENAME,
// best-effort (empty if unreadable - a mismatch there is only a warning,
// never a hard failure, per install's own precondition handling).
func realVersionCodename() string {
	rel := readOSRelease("/")
	return rel.VersionCodename
}

// buildPlaceholderDeb writes a minimal DEBIAN/control and builds a real .deb
// with dpkg-deb, exactly E3's own fixture technique
// (hack/experiments/e3-pin-fidelity.sh's build_placeholder_deb): a
// placeholder package built only to test version selection, never a real
// network archive.
func buildPlaceholderDeb(t *testing.T, workDir, pkg, version, arch string) string {
	t.Helper()
	srcDir, err := os.MkdirTemp(workDir, "deb-src-*")
	if err != nil {
		t.Fatal(err)
	}
	debianDir := filepath.Join(srcDir, "DEBIAN")
	if err := os.MkdirAll(debianDir, 0o755); err != nil {
		t.Fatal(err)
	}
	control := "Package: " + pkg + "\n" +
		"Version: " + version + "\n" +
		"Section: misc\n" +
		"Priority: optional\n" +
		"Architecture: " + arch + "\n" +
		"Maintainer: debark E2E tests <noreply@example.invalid>\n" +
		"Description: E3 scenario placeholder package (" + version + ")\n" +
		" Built only to test install --upgrade's pin-fidelity fix; carries no files.\n"
	if err := os.WriteFile(filepath.Join(debianDir, "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	debPath := filepath.Join(workDir, pkg+"_"+version+"_"+arch+".deb")
	runReal(t, "dpkg-deb", "--build", "-Zgzip", srcDir, debPath)
	return debPath
}

// runReal execs name with args for real (never through execCommandContext /
// runProcess), failing the test with the combined output on error - used for
// one-off fixture setup (dpkg-deb, dpkg -i) where there is nothing to parse.
func runReal(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// realInstalledVersion asks the real, live dpkg database what version of pkg
// is installed, "" if none (or not fully configured).
func realInstalledVersion(t *testing.T, pkg string) string {
	t.Helper()
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status} ${Version}", pkg).CombinedOutput()
	if err != nil {
		// dpkg-query exits non-zero for an unknown package; that just means
		// "not installed" for this helper's purposes.
		return ""
	}
	fields := strings.Fields(string(out))
	// Status is three words ("install ok installed" when present); Version
	// is whatever follows. A purged/never-installed package still has *some*
	// entry once dpkg -i touched it once in this test, so check the status
	// value explicitly rather than just trusting a non-empty version field.
	if len(fields) < 4 || fields[2] != "installed" {
		return ""
	}
	return fields[3]
}

// copyFileReal copies src to dst, creating dst's parent directory.
func copyFileReal(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// aptPackagesStanza renders one real apt Packages control stanza - the
// fields apt-get actually consults to locate and verify a file: URI
// candidate (Package, Version, Architecture, Filename, Size, and a real
// hash it checks the fetched/local bytes against).
func aptPackagesStanza(pkg, version, arch, relPath string, h digest.FileHashes) string {
	return fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: %s\nFilename: %s\nSize: %d\nMD5sum: %s\nSHA256: %s\n",
		pkg, version, arch, relPath, h.Size, h.MD5, h.SHA256)
}

// realSimulatedFullUpgradeVersion builds the same kind of private apt view
// install.Runner itself builds (newPrivateRoot) over repoDir, points it at a
// real apt-get -s full-upgrade, and returns the version apt selected for pkg
// ("" if apt left it untouched). This is the setup-validation step (E3 step
// 5c reproduced directly): it proves the fixture bundle really does make a
// bare, unpinned full-upgrade diverge from the locked version, using this
// package's own real (not faked, in this one test) apt-get plumbing.
func realSimulatedFullUpgradeVersion(t *testing.T, ctx context.Context, repoDir, pkg string) string {
	t.Helper()
	pr, err := newPrivateRoot(repoDir, false)
	if err != nil {
		t.Fatalf("newPrivateRoot: %v", err)
	}
	defer pr.Close()

	updOut, err := runProcess(ctx, "apt-get", append(pr.AptArgs(false), "update"), buildEnv(false))
	if err != nil {
		t.Fatalf("apt-get update: %v\n%s", err, updOut.Combined)
	}
	upOut, upErr := runProcess(ctx, "apt-get", append(pr.AptArgs(false), "-s", "full-upgrade"), buildEnv(false))
	if upErr != nil {
		t.Logf("apt-get -s full-upgrade (setup check): %v\n%s", upErr, upOut.Combined)
	}
	_, toUpgrade, _, _ := parseAptSim(upOut.Combined)
	for _, nv := range toUpgrade {
		name, version, ok := strings.Cut(nv, "=")
		if ok && name == pkg {
			return version
		}
	}
	return ""
}
