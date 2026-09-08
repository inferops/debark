//go:build linux

// This file is the headline end-to-end test: it proves, in a container with
// no network at all, that a real bundle built from real .deb files verifies
// and installs through install.Runner's public API, and that the installed
// binaries actually run. It is guarded off by default (see requireE2E) and
// is not exercised by the Windows/macOS/CI-default unit test run.
//
// It lives in package install_test (a black-box, external test) rather than
// package install: the whole point is to exercise exactly the API a real
// caller (the CLI, or any other Go program) would use — install.New,
// install.Deps, install.Options — nothing unexported. It therefore also does
// not share TestMain with the rest of this package's tests: it never uses
// the fake exec (execCommandContext), because the entire point here is a
// real apt-get and a real dpkg, run with no network.
//
// Every container interaction here goes through `docker create` /
// `docker cp` / `docker start -a` rather than bind mounts (see the docker*
// helpers below). That is deliberate, not decorative: a bind mount's source
// path is resolved by the daemon, so it breaks the moment this test itself
// runs inside a container talking to the host's daemon over a mounted
// socket (this exact test was developed and verified that way — see the
// install end-to-end final report). docker cp streams bytes over the client
// connection instead and works identically whether the test runs natively on
// a Linux host with Docker or nested one level deep, which is one less way
// for this test to be flaky depending on who runs it and how.
//
// Run it with:
//
//	DEBARK_E2E=1 bash hack/linux-test.sh ./core/install/...
//
// hack/linux-test.sh's own container does not currently mount a Docker
// socket or ship a docker CLI, so requireE2E below will skip with a clear
// reason there until that is added; it runs end-to-end on any Linux host,
// nested or not, that has a working `docker`.
package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
)

// requireE2E is install's version of the contract-brief's requireLinuxAPT
// guard, extended with a docker-availability check so the test degrades to
// a clean skip (never a failure) wherever docker cannot be reached — the
// exact situation inside hack/linux-test.sh's own container today.
func requireE2E(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("DEBARK_E2E") == "" {
		t.Skip("needs Linux with docker; set DEBARK_E2E=1")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH in this environment (this test invokes `docker` itself, per the install end-to-end brief); " +
			"run it on a Linux host with a working docker, e.g. from inside WSL rather than via hack/linux-test.sh's own container, " +
			"until that container mounts a docker socket")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker is on PATH but not usable (%v): %s", err, out)
	}
}

// TestE2E_NetworkNoneInstall is the whole story in one test:
//
//  1. (networked) download real .deb files for jq + its two dependencies
//     with apt-get inside a plain, networked container, and capture each
//     one's real control stanza with dpkg-deb -f.
//  2. (local, pure Go) assemble a minimal but complete bundle from them: a
//     flat repo/ (Packages + Release, no apt-utils dependency — the exact
//     gap docs/dev/prototype-baseline.md records against the prototype),
//     lock.json, and a hand-built, unsigned debark.manifest.json whose
//     digests are computed with the same core/canonical and core/digest
//     packages every other packages uses.
//  3. build a tiny driver binary that does nothing but call
//     install.New(...).Apply via the public API, statically, for linux.
//  4. run that driver inside a fresh `docker create --network none`
//     container against the bundle, then, in a second --network none
//     container, prove jq actually runs: `jq --version` and a real filter
//     over stdin.
//
// What this proves, concretely: a bundle assembled entirely offline-style
// (no apt-utils, no signing infrastructure) verifies (AllowUnsigned — no
// operator key is part of this test) and installs through install.Runner
// with the target machine unable to reach the network at any point after
// the bundle exists, and the installed binary is not a stub: it runs and
// produces correct output.
func TestE2E_NetworkNoneInstall(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()
	work := t.TempDir()

	// --- 1. Networked step: real .deb files + their real control data. ---
	t.Log("downloading jq, libjq1, libonig5 and their control data (networked container)")
	dlDir := filepath.Join(work, "downloaded")
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dlCID := dockerCreate(ctx, t, "debian:bookworm-slim", "bash", "-c",
		`set -e; apt-get update -qq >/dev/null; mkdir -p /out; cd /out; `+
			`apt-get download jq libjq1 libonig5 >/dev/null; `+
			`for f in *.deb; do dpkg-deb -f "$f" > "$f.control"; done`,
	)
	out, code := dockerStartAttached(ctx, t, dlCID)
	if code != 0 {
		dockerRemove(ctx, t, dlCID)
		t.Fatalf("download container exited %d:\n%s", code, out)
	}
	dockerCopyOut(ctx, t, dlCID, "/out/.", dlDir)
	dockerRemove(ctx, t, dlCID)

	debs, err := filepath.Glob(filepath.Join(dlDir, "*.deb"))
	if err != nil || len(debs) != 3 {
		t.Fatalf("expected 3 downloaded .deb files, got %v (err=%v)", debs, err)
	}
	sort.Strings(debs)

	// --- 2. Assemble a minimal, complete bundle. ---
	bundleDir := filepath.Join(work, "bundle")
	repoDir := filepath.Join(bundleDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var pkgs []lock.Package
	var poolFiles []manifest.File
	var packagesStanzas []string
	var poolBytes int64

	for _, debPath := range debs {
		name, version, arch := parseDebFilename(t, debPath)
		control, err := os.ReadFile(debPath + ".control")
		if err != nil {
			t.Fatalf("read control for %s: %v", debPath, err)
		}
		hashes, err := digest.AllFile(debPath)
		if err != nil {
			t.Fatalf("hash %s: %v", debPath, err)
		}

		relPool := poolPath(name, filepath.Base(debPath))
		destPath := filepath.Join(repoDir, filepath.FromSlash(relPool))
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := copyFile(debPath, destPath); err != nil {
			t.Fatalf("copy %s into pool: %v", debPath, err)
		}

		stanza := strings.TrimRight(string(control), "\n") + "\n" +
			"Filename: " + relPool + "\n" +
			fmt.Sprintf("Size: %d\n", hashes.Size) +
			"MD5sum: " + hashes.MD5 + "\n" +
			"SHA1: " + hashes.SHA1 + "\n" +
			"SHA256: " + hashes.SHA256 + "\n"
		packagesStanzas = append(packagesStanzas, stanza)
		poolBytes += hashes.Size

		pkgs = append(pkgs, lock.Package{
			Name: name, Arch: arch, Version: version,
			Filename: "repo/" + relPool, Size: hashes.Size, SHA256: hashes.SHA256,
			Reason:                pickReason(name),
			PublisherVerification: lock.VerifiedAPTSigned,
		})
		poolFiles = append(poolFiles, manifest.File{Path: "repo/" + relPool, Size: hashes.Size, SHA256: hashes.SHA256})
	}

	packagesText := strings.Join(packagesStanzas, "\n")
	packagesPath := filepath.Join(repoDir, "Packages")
	if err := os.WriteFile(packagesPath, []byte(packagesText), 0o644); err != nil {
		t.Fatal(err)
	}
	packagesHashes, err := digest.AllFile(packagesPath)
	if err != nil {
		t.Fatal(err)
	}

	releaseText := "Origin: debark\n" +
		"Label: debark bundle\n" +
		"Suite: bundle\n" +
		"Codename: bookworm\n" +
		"Architectures: amd64\n" +
		"Components: main\n" +
		"Description: debark offline bundle (E2E test fixture)\n" +
		"Date: " + time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC") + "\n" +
		"MD5Sum:\n" +
		fmt.Sprintf(" %s %16d Packages\n", packagesHashes.MD5, packagesHashes.Size) +
		"SHA256:\n" +
		fmt.Sprintf(" %s %16d Packages\n", packagesHashes.SHA256, packagesHashes.Size)
	releasePath := filepath.Join(repoDir, "Release")
	if err := os.WriteFile(releasePath, []byte(releaseText), 0o644); err != nil {
		t.Fatal(err)
	}
	releaseHashes, err := digest.AllFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	poolFiles = append(poolFiles,
		manifest.File{Path: "repo/Packages", Size: packagesHashes.Size, SHA256: packagesHashes.SHA256},
		manifest.File{Path: "repo/Release", Size: releaseHashes.Size, SHA256: releaseHashes.SHA256},
	)

	target := lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	// jq's whole dependency closure (jq, libjq1, libonig5) is the exact set
	// the lock tells install to request — never "whatever is newest in the
	// bundle" (ADR-007). Architecture-qualified (name:arch=version): what
	// lock.Validate requires (a Multi-Arch: same package can legitimately
	// share a name and version across architectures).
	install := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		install = append(install, p.Name+":"+p.Arch+"="+p.Version)
	}
	sort.Strings(install)

	lk := lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		CreatedAt:     canonical.Time(time.Now()),
		Target:        target,
		Resolver:      lock.Resolver{Backend: lock.BackendContainer, APTVersion: "e2e-fixture", DpkgVersion: "e2e-fixture", PhasedUpdates: "never-include"},
		Packages:      pkgs,
		Install:       install,
		ClosedWorld:   lock.ClosedWorld{Result: lock.ClosedWorldSkipped, Detail: "E2E fixture: hand-assembled, not resolved by apt"},
	}
	lockBytes, err := canonical.MarshalIndent(lk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "lock.json"), lockBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	lockDigest, err := canonical.Digest(lk)
	if err != nil {
		t.Fatal(err)
	}
	lockFileHashes, err := digest.AllFile(filepath.Join(bundleDir, "lock.json"))
	if err != nil {
		t.Fatal(err)
	}

	files := append([]manifest.File{{Path: "lock.json", Size: lockFileHashes.Size, SHA256: lockFileHashes.SHA256}}, poolFiles...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	man := manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		BundleID:      "e2e-test-bundle",
		CreatedAt:     canonical.Time(time.Now()),
		FormatVersion: manifest.CurrentFormatVersion,
		Tool:          manifest.Tool{Name: "debark-e2e-test", Version: "0", Edition: manifest.EditionCommunity},
		LockDigest:    lockDigest,
		Repository: manifest.Repository{
			PackagesSHA256: packagesHashes.SHA256,
			ReleaseSHA256:  releaseHashes.SHA256,
			PackageCount:   len(pkgs),
			PoolBytes:      poolBytes,
		},
		Files:  files,
		Target: manifest.Target{DistroID: target.DistroID, VersionID: target.VersionID, Codename: target.Codename, Arch: target.Arch},
	}
	manBytes, err := canonical.MarshalIndent(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "debark.manifest.json"), manBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately unsigned: this test drives install.Options.Verify with
	// AllowUnsigned:true. Operator-key provisioning is core/verify's concern,
	// out of scope for proving install's own machinery.

	// --- 3. Build the driver: install.New(...).Apply(...), nothing else. ---
	driverSrc := filepath.Join(work, "e2edriver.go")
	if err := os.WriteFile(driverSrc, []byte(driverSource), 0o644); err != nil {
		t.Fatal(err)
	}
	driverBin := filepath.Join(work, "e2edriver")
	buildDriver(t, driverSrc, driverBin)

	// --- 4. The point of the test: install with NO network at all. ---
	t.Log("installing with --network none")
	report := runInstallContainer(ctx, t, driverBin, bundleDir)
	if !report.Applied || !report.OK {
		t.Fatalf("install did not succeed with no network: applied=%v ok=%v problems=%v",
			report.Applied, report.OK, report.Problems)
	}
	t.Logf("installed with no network: %v", report.ToInstall)

	// --- Prove the binaries run: a *second*, fresh --network none
	// container, using the installed jq for real. ---
	t.Log("proving the installed binary runs, in a second --network none container")
	verifyOut := runVerifyContainer(ctx, t, driverBin, bundleDir)
	if !strings.Contains(verifyOut, "jq-1.6") {
		t.Errorf("jq --version output unexpected: %s", verifyOut)
	}
	if !strings.Contains(strings.TrimSpace(lastNonEmptyLine(verifyOut)), "1") {
		t.Errorf("jq -r .a did not produce the filtered value 1: %s", verifyOut)
	}
	t.Logf("jq ran for real, with no network reachable at any point:\n%s", verifyOut)
}

// --- docker orchestration -------------------------------------------------
//
// Every helper below goes through `docker create` / `docker cp` /
// `docker start -a` / `docker rm`, never a bind mount: docker cp streams
// bytes over the client<->daemon API connection, so it needs no filesystem
// path the daemon can resolve, unlike a bind mount's source. That is what
// makes these helpers work identically whether this test runs natively on a
// Linux host with Docker, or one level of sibling-container nesting deep
// talking to a mounted socket (both were exercised for the install
// end-to-end final report).

func dockerCreate(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"create"}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(full, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func dockerCopyIn(ctx context.Context, t *testing.T, cid, localSrc, containerDest string) {
	t.Helper()
	runDockerQuiet(ctx, t, "cp", localSrc, cid+":"+containerDest)
}

func dockerCopyOut(ctx context.Context, t *testing.T, cid, containerSrc, localDest string) {
	t.Helper()
	runDockerQuiet(ctx, t, "cp", cid+":"+containerSrc, localDest)
}

// dockerStartAttached starts an already-created container attached, so its
// combined output can be captured, and returns that output plus its exit
// code (via `docker wait`, which is reliable even for a container whose
// entrypoint traps signals oddly — `start -a`'s own exit status is not
// always trustworthy across docker versions).
func dockerStartAttached(ctx context.Context, t *testing.T, cid string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "docker", "start", "-a", cid)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run() // exit status inspected via `docker wait` below, not here

	waitCmd := exec.CommandContext(ctx, "docker", "wait", cid)
	var waitOut bytes.Buffer
	waitCmd.Stdout = &waitOut
	code := -1
	if err := waitCmd.Run(); err == nil {
		fmt.Sscanf(strings.TrimSpace(waitOut.String()), "%d", &code)
	}
	return out.String(), code
}

func dockerRemove(ctx context.Context, t *testing.T, cid string) {
	t.Helper()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", cid).Run()
}

func runDockerQuiet(ctx context.Context, t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
}

type driverReport struct {
	Applied   bool     `json:"applied"`
	OK        bool     `json:"ok"`
	ToInstall []string `json:"to_install"`
	Problems  []string `json:"problems"`
}

// runInstallContainer creates a --network none container, copies the driver
// and the bundle into it (never a bind mount — see above), runs the driver,
// and parses its JSON report from stdout.
func runInstallContainer(ctx context.Context, t *testing.T, driverBin, bundleDir string) driverReport {
	t.Helper()
	cid := dockerCreate(ctx, t, "--network", "none", "debian:bookworm-slim",
		"/e2edriver", "-bundle=/bundle", "-yes", "-allow-unsigned")
	defer dockerRemove(ctx, t, cid)

	dockerCopyIn(ctx, t, cid, driverBin, "/e2edriver")
	dockerCopyIn(ctx, t, cid, bundleDir, "/bundle")

	out, code := dockerStartAttached(ctx, t, cid)
	t.Logf("driver output (exit %d):\n%s", code, out)

	line := lastJSONLine(out)
	if line == "" {
		t.Fatalf("no JSON report line found in driver output (exit %d):\n%s", code, out)
	}
	var report driverReport
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("parse driver report %q: %v", line, err)
	}
	return report
}

// runVerifyContainer is the same install, in a fresh --network none
// container, followed by actually running jq — the "prove the binaries
// run" half of the test.
func runVerifyContainer(ctx context.Context, t *testing.T, driverBin, bundleDir string) string {
	t.Helper()
	script := "/e2edriver -bundle=/bundle -yes -allow-unsigned >/dev/null && " +
		"jq --version && echo '{\"a\":1}' | jq -r .a"
	cid := dockerCreate(ctx, t, "--network", "none", "debian:bookworm-slim", "bash", "-c", script)
	defer dockerRemove(ctx, t, cid)

	dockerCopyIn(ctx, t, cid, driverBin, "/e2edriver")
	dockerCopyIn(ctx, t, cid, bundleDir, "/bundle")

	out, code := dockerStartAttached(ctx, t, cid)
	if code != 0 {
		t.Fatalf("verify container exited %d:\n%s", code, out)
	}
	return out
}

// --- fixture-building helpers ---------------------------------------------

// parseDebFilename extracts name_version_arch from apt-get download's
// standard <name>_<version>_<arch>.deb naming.
func parseDebFilename(t *testing.T, path string) (name, version, arch string) {
	t.Helper()
	base := strings.TrimSuffix(filepath.Base(path), ".deb")
	parts := strings.SplitN(base, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected .deb filename shape: %s", base)
	}
	// apt-get download percent-encodes ':' in the version as %3a.
	version = strings.ReplaceAll(parts[1], "%3a", ":")
	return parts[0], version, parts[2]
}

// poolPath mirrors core/repository.PoolPath's documented algorithm
// (pool/<prefix>/<pkg>/<file>, "lib" packages get a 4-char prefix) — a
// second, test-scoped implementation rather than an import, so this file's
// compilation never depends on core/repository.
func poolPath(pkg, filename string) string {
	prefix := pkg[:1]
	if len(pkg) >= 4 && pkg[:3] == "lib" {
		prefix = pkg[:4]
	}
	return "pool/" + prefix + "/" + pkg + "/" + filename
}

func pickReason(name string) string {
	if name == "jq" {
		return lock.ReasonRequested
	}
	return lock.ReasonDependencyOfPrefix + "jq"
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func lastJSONLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			return l
		}
	}
	return ""
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

func buildDriver(t *testing.T, srcFile, outBin string) {
	t.Helper()
	// The driver imports github.com/inferops/debark/core/install and
	// core/verify, so it must be built from inside this module. It is
	// generated into, and built from, a throwaway subdirectory of this
	// package, removed at the end of the test — this file itself lives
	// under a directory that is a real child of the module root, so `go
	// build` resolves the module import correctly.
	moduleDriverDir := filepath.Join(".", ".e2edriver-tmp")
	if err := os.MkdirAll(moduleDriverDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", moduleDriverDir, err)
	}
	t.Cleanup(func() { os.RemoveAll(moduleDriverDir) })

	genSrc := filepath.Join(moduleDriverDir, "main.go")
	data, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(genSrc, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "build", "-o", outBin, genSrc)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build driver: %v\n%s", err, out.String())
	}
	if err := os.Chmod(outBin, 0o755); err != nil {
		t.Fatal(err)
	}
}

// driverSource is a tiny program whose only job is to call install.Runner's
// public API exactly as any other caller (the CLI included) would, so the
// --network none proof exercises install.New/Apply for real rather than
// re-implementing its logic inline in the test.
const driverSource = `package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/inferops/debark/core/install"
	"github.com/inferops/debark/core/verify"
)

func main() {
	bundle := flag.String("bundle", "", "bundle directory")
	yes := flag.Bool("yes", false, "assume yes")
	allowUnsigned := flag.Bool("allow-unsigned", false, "allow an unsigned bundle")
	flag.Parse()

	r := install.New(install.Deps{Verifier: verify.New()})
	report, err := r.Apply(context.Background(), *bundle, install.Options{
		Yes:    *yes,
		Verify: verify.Options{AllowUnsigned: *allowUnsigned},
	})
	enc := json.NewEncoder(os.Stdout)
	if encErr := enc.Encode(report); encErr != nil {
		fmt.Fprintln(os.Stderr, "encode report:", encErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		os.Exit(1)
	}
}
`
