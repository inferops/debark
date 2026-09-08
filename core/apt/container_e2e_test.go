package apt

// Docker/Podman-backed tests: these actually run a container. They prove the
// low-level driver mechanics (mount conversion, argv construction, exec,
// envelope parsing, cross-check, host re-verification) against a REAL
// container runtime, without depending on the `debark resolve`
// command being finished -- the container mounts a small fake "debark"
// shell script that emits a canned envelope instead of the real binary.
//
// Guard: requireContainerE2E skips unless DEBARK_E2E is set AND a runtime
// is actually reachable. This deliberately does NOT also require
// runtime.GOOS=="linux" the way docs/dev/contract-brief.md's generic
// requireLinuxAPT helper does -- see requireContainerE2E's own doc comment
// below for why: gating this backend's own tests to Linux-only would make it
// impossible to ever prove the one thing this package exists to prove, on the
// one machine actually able to prove it.
//
// Run: DEBARK_E2E=1 go test ./core/apt/... -run Container -v
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// requireContainerE2E skips the test unless DEBARK_E2E is set and a
// container runtime is actually reachable, and returns the runtime's name
// and path.
//
// Unlike docs/dev/contract-brief.md's requireLinuxAPT, this does not gate on
// runtime.GOOS=="linux": the container backend's entire purpose is running
// on hosts with no native apt at all (macOS, Windows), so a Linux-only guard
// would make it impossible to exercise the thing this package exists to prove
// works, on the one machine actually able to prove it (this one, a Windows
// box with Docker Desktop). Gating on "a runtime is actually reachable"
// instead means this still skips cleanly inside
// `hack/linux-test.sh` (its bare golang:1.26-bookworm image has no docker
// CLI and no socket forwarded) and inside any other DEBARK_E2E=1
// environment that happens not to have a runtime, while running for real
// wherever one is available -- which is what the project's actual testing
// intent (skip cleanly where the dependency is missing, run for real where
// it is not) asks for.
func requireContainerE2E(t *testing.T) (runtimeName, runtimePath string) {
	t.Helper()
	if os.Getenv("DEBARK_E2E") == "" {
		t.Skip("set DEBARK_E2E=1 to run container-backed tests")
	}
	name, path, err := containerDetectRuntime(ContainerOptions{})
	if err != nil {
		t.Skipf("no container runtime reachable: %v", err)
	}
	return name, path
}

// TestContainerE2ERuntimeReachable is a cheap smoke test: the runtime this
// suite found actually runs something.
func TestContainerE2ERuntimeReachable(t *testing.T) {
	_, runtimePath := requireContainerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := execContainer(ctx, runtimePath, []string{"version", "--format", "{{.Server.Os}}/{{.Server.Arch}}"}, nil)
	if err != nil {
		t.Fatalf("execContainer(version): %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("%s version exited %d: %s", runtimePath, res.ExitCode, res.combined())
	}
	t.Logf("container runtime reachable: %s", string(res.Stdout))
}

// TestContainerE2EResolveFakeScript mounts a fake "debark" shell script
// (not a real ELF binary, so this drives the mount/exec/parse pipeline
// directly rather than through the full containerBackend.Resolve(), whose
// containerValidateBinary gate specifically requires a real static Linux
// ELF -- that gate is already covered thoroughly, and much faster, by
// TestContainerValidateBinary's hand-built ELF headers) that emits a canned
// debark.resolveplan/v1 envelope and a matching fake .deb file, and proves
// this package's real driver code -- containerBuildRunArgv, execContainer,
// containerParseEnvelope, containerCrossCheckEnvelope,
// containerReverifyFiles -- invokes it correctly and parses the result, end
// to end, against a real container.
func TestContainerE2EResolveFakeScript(t *testing.T) {
	_, runtimePath := requireContainerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	work := t.TempDir()
	archives := t.TempDir()
	snapFiles := t.TempDir()

	const debContent = "fake-deb-bytes-for-e2e-test\n"
	sum := sha256.Sum256([]byte(debContent))
	debSHA := hex.EncodeToString(sum[:])
	debSize := len(debContent)

	envelope := fmt.Sprintf(`{
  "schema_version": %q,
  "plan": {
    "Target": {"distro_id":"debian","version_id":"12","codename":"bookworm","arch":"amd64"},
    "Resolver": {"backend":"local","apt_version":"2.6.1","dpkg_version":"1.21.22","phased_updates":"never-include","install_recommends":true},
    "Selections": [
      {"Name":"testpkg","Arch":"amd64","Version":"1.0-1","Filename":"testpkg_1.0-1_amd64.deb","Size":%d,"SHA256":%q,"Reason":"requested"}
    ],
    "Install": ["testpkg=1.0-1"]
  },
  "backend": {"kind":"local","apt_version":"2.6.1","dpkg_version":"1.21.22","distro_id":"debian","version_id":"12"},
  "archives_dir": "/archives",
  "files": [
    {"name":"testpkg","arch":"amd64","version":"1.0-1","filename":"testpkg_1.0-1_amd64.deb","sha256":%q,"size":%d}
  ]
}
`, containerEnvelopeSchema, debSize, debSHA, debSHA, debSize)

	script := "#!/bin/sh\n" +
		"set -e\n" +
		"cat > /archives/testpkg_1.0-1_amd64.deb <<'DEBARK_DEB_EOF'\n" +
		debContent +
		"DEBARK_DEB_EOF\n" +
		"cat > /work/plan.json <<'DEBARK_PLAN_EOF'\n" +
		envelope +
		"DEBARK_PLAN_EOF\n" +
		"exit 0\n"

	scriptPath := filepath.Join(t.TempDir(), "fake-debark")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake debark script: %v", err)
	}

	binSrc, err := containerMountSource(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	workSrc, err := containerMountSource(work)
	if err != nil {
		t.Fatal(err)
	}
	archSrc, err := containerMountSource(archives)
	if err != nil {
		t.Fatal(err)
	}
	filesSrc, err := containerMountSource(snapFiles)
	if err != nil {
		t.Fatal(err)
	}

	mounts := []containerMount{
		{Source: binSrc, Target: "/debark", RO: true},
		{Source: filesSrc, Target: "/snapshot/files", RO: true},
		{Source: workSrc, Target: "/work", RO: false},
		{Source: archSrc, Target: "/archives", RO: false},
	}
	inner := containerBuildInnerArgv(containerResolveArgs{Packages: []string{"testpkg"}, Recommends: true, Arch: "amd64"})
	// platform "" (no --platform): this test exercises the mount/exec/parse
	// pipeline, not cross-arch emulation -- see
	// TestContainerE2ECrossArchEmulation for that.
	argv := containerBuildRunArgv("", mounts, nil, "busybox:latest", inner)

	res, err := execContainer(ctx, runtimePath, argv, nil)
	if err != nil {
		t.Fatalf("execContainer: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("fake debark script exited %d: %s", res.ExitCode, res.combined())
	}

	envBytes, err := os.ReadFile(filepath.Join(work, "plan.json"))
	if err != nil {
		t.Fatalf("read plan.json written by the fake script through the /work mount: %v", err)
	}
	env, err := containerParseEnvelope(envBytes)
	if err != nil {
		t.Fatalf("containerParseEnvelope: %v", err)
	}
	// nil: this round trip stages no external repository, so no selection
	// is entitled to reason:external (see containerCrossCheckEnvelope).
	if err := containerCrossCheckEnvelope(env, nil, ""); err != nil {
		t.Fatalf("containerCrossCheckEnvelope: %v", err)
	}
	if err := containerReverifyFiles(env, archives); err != nil {
		t.Fatalf("containerReverifyFiles against the host-visible archives dir: %v", err)
	}
	if len(env.Plan.Selections) != 1 || env.Plan.Selections[0].Name != "testpkg" || env.Plan.Selections[0].Version != "1.0-1" {
		t.Fatalf("unexpected plan after a real container round-trip: %+v", env.Plan)
	}
	t.Logf("real container round-trip OK: %d file(s) staged in %s, %d plan selection(s), digest re-verified on host",
		len(env.Files), archives, len(env.Plan.Selections))
}

// TestContainerE2ECrossArchEmulation proves --platform cross-arch actually
// works on this machine by running a real container under
// linux/arm64 and linux/arm/v7 (both foreign to this amd64 host) and
// checking `uname -m` reports the emulated architecture. See this package's
// report for what this machine needed before this passed: Docker Desktop
// 29.6.2 here did NOT have a working arm64 binfmt handler out of the box --
// `docker run --rm --platform linux/arm64 busybox uname -m` failed with the
// literal string "exec format error" (exactly what
// containerExecFormatErrorRe/containerClassifyRunResult in
// container_driver.go is built to catch and turn into a clear environment
// error) until `docker run --privileged --rm tonistiigi/binfmt --install
// all` registered the QEMU handlers -- the exact fix
// containerBinfmtHint() names.
func TestContainerE2ECrossArchEmulation(t *testing.T) {
	_, runtimePath := requireContainerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cases := []struct {
		platform string
		wantArch string
	}{
		{"linux/arm64", "aarch64"},
		{"linux/arm/v7", "armv7l"},
	}
	for _, c := range cases {
		t.Run(c.platform, func(t *testing.T) {
			argv := containerBuildRunArgv(c.platform, nil, nil, "busybox:latest", []string{"uname", "-m"})
			res, err := execContainer(ctx, runtimePath, argv, nil)
			if err != nil {
				t.Fatalf("execContainer: %v", err)
			}
			if res.ExitCode != 0 {
				classified := containerClassifyRunResult(res, c.platform)
				t.Skipf("emulation for %s is not available on this machine (%v); install qemu-user-static / binfmt handlers, see containerBinfmtHint(); raw output: %s",
					c.platform, classified, res.combined())
			}
			got := trimNewline(string(res.Stdout))
			if got != c.wantArch {
				t.Errorf("uname -m under --platform %s = %q, want %q", c.platform, got, c.wantArch)
			} else {
				t.Logf("cross-arch emulation confirmed: --platform %s runs as %s", c.platform, got)
			}
		})
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// --- ClosedWorld against a real container (E3 upgrade-set fix) -----------

// containerE2EDpkgArch maps this host's GOARCH to the matching dpkg
// architecture name, so TestContainerE2EClosedWorldUpgradeDivergence runs
// its container natively (no --platform emulation dependency, which
// TestContainerE2ECrossArchEmulation above already covers and skips cleanly
// on its own when unavailable -- this test has nothing to do with proving
// emulation works, so it deliberately does not entangle itself with it).
func containerE2EDpkgArch(t *testing.T) string {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	case "386":
		return "i386"
	default:
		t.Skipf("no dpkg architecture mapping for GOARCH=%s", runtime.GOARCH)
		return ""
	}
}

// containerE2EBuildFakeDebark cross-compiles a tiny, real static Linux ELF
// binary (CGO_ENABLED=0) that -- run with any arguments -- unconditionally
// writes one fixed debark.resolveplan/v1 envelope to /work/plan.json and
// exits 0, standing in for the inner `debark resolve --backend local`
// exactly as TestContainerE2EResolveFakeScript's shell script does above,
// but compiled rather than scripted: ClosedWorld (unlike the raw driver
// mechanics that test exercises) runs containerValidateBinary before ever
// starting a container, and a shell script cannot pass that gate. Its
// hardcoded response reports debark-e3-demo at 2.0-1 -- E3's own recorded
// divergence (docs/experiments/E3-pin-fidelity.md) -- deliberately different
// from the 1.0-1 the calling test's own lock.json records as the locked
// upgrade target, so a real container round trip through this package's
// real, unmodified ClosedWorld code is what has to notice the mismatch.
func containerE2EBuildFakeDebark(t *testing.T, dpkgArch string) string {
	t.Helper()
	srcDir := t.TempDir()

	envelope := strings.ReplaceAll(`{
  "schema_version": "debark.resolveplan/v1",
  "plan": {
    "Target": {"distro_id":"debian","version_id":"12","codename":"bookworm","arch":"ARCH"},
    "Resolver": {"backend":"local","apt_version":"2.6.1","dpkg_version":"1.21.22","phased_updates":"never-include","install_recommends":true},
    "Selections": [
      {"Name":"demo-app","Arch":"ARCH","Version":"1.0-1","Filename":"demo-app_1.0-1_ARCH.deb","Reason":"requested"},
      {"Name":"debark-e3-demo","Arch":"all","Version":"2.0-1","Filename":"debark-e3-demo_2.0-1_all.deb","Reason":"external"}
    ],
    "Install": ["demo-app:ARCH=1.0-1"]
  },
  "backend": {"kind":"local","apt_version":"2.6.1","dpkg_version":"1.21.22","distro_id":"debian","version_id":"12"},
  "archives_dir": "/archives",
  "files": []
}
`, "ARCH", dpkgArch)

	return containerE2ECompileFakeDebark(t, srcDir, dpkgArch, envelope)
}

// containerE2EBuildFakeDebarkEmpty is containerE2EBuildFakeDebark's
// sibling for TestContainerE2ESilentDivergence below: it writes an envelope
// with NO Selections and NO Unresolved at all -- what a real inner resolve
// would produce if its bare-name candidate for a package already matched
// whatever dpkg reported installed, proposing no action either way. Whether
// that silence is correct depends entirely on what the target's OWN
// captured dpkg status says, which containerVersionMismatches now checks
// host-side rather than trusting the silence -- this fake binary's job is
// only to produce that silence for real, against a real container; the
// calling test supplies the dpkg status fixture that makes the silence
// either correct or a divergence.
func containerE2EBuildFakeDebarkEmpty(t *testing.T, dpkgArch string) string {
	t.Helper()
	srcDir := t.TempDir()
	envelope := strings.ReplaceAll(`{
  "schema_version": "debark.resolveplan/v1",
  "plan": {
    "Target": {"distro_id":"debian","version_id":"12","codename":"bookworm","arch":"ARCH"},
    "Resolver": {"backend":"local","apt_version":"2.6.1","dpkg_version":"1.21.22","phased_updates":"never-include","install_recommends":true},
    "Selections": [],
    "Install": []
  },
  "backend": {"kind":"local","apt_version":"2.6.1","dpkg_version":"1.21.22","distro_id":"debian","version_id":"12"},
  "archives_dir": "/archives",
  "files": []
}
`, "ARCH", dpkgArch)
	return containerE2ECompileFakeDebark(t, srcDir, dpkgArch, envelope)
}

// containerE2ECompileFakeDebark cross-compiles a tiny, real static Linux
// ELF binary (CGO_ENABLED=0) that -- run with any arguments -- unconditionally
// writes envelope to /work/plan.json and exits 0. Shared by
// containerE2EBuildFakeDebark and containerE2EBuildFakeDebarkEmpty so
// there is one cross-compile implementation, not two that could drift on
// build flags or output naming.
func containerE2ECompileFakeDebark(t *testing.T, srcDir, dpkgArch, envelope string) string {
	t.Helper()
	const mainTemplate = `package main

import "os"

const envelope = %s

func main() {
	if err := os.WriteFile("/work/plan.json", []byte(envelope), 0o644); err != nil {
		os.Exit(1)
	}
}
`
	src := fmt.Sprintf(mainTemplate, "`"+envelope+"`")
	mainPath := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write fake debark source: %v", err)
	}

	outPath := filepath.Join(srcDir, "fake-debark-linux-"+dpkgArch)
	cmd := exec.Command("go", "build", "-o", outPath, mainPath)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+dpkgArch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile fake debark (GOOS=linux GOARCH=%s): %v\n%s", dpkgArch, err, out)
	}
	return outPath
}

// TestContainerE2EClosedWorldUpgradeDivergence proves the E3 fix
// (containerBackend.ClosedWorld's upgrade-set version comparison, added to
// container.go) against a REAL container runtime, not just the faked
// execContainerFn unit tests in container_test.go.
//
// It stops short of using the real `debark` binary and a real
// apt-capable image: that needs the whole resolve/engine/install pipeline
// (other packages' code, some of it still in progress per
// docs/dev/contract-brief.md) plus a hand-built flat apt repository --
// essentially reproducing hack/experiments/e3-pin-fidelity.sh's own setup --
// which is not economical here and would make this test's result depend on
// work this package does not own. Said plainly, as the contract brief asks: a
// true full-pipeline E2E (real debark binary, real target-release image,
// real accumulated bundle) is out of reach for this package alone. What this
// test does instead -- substituting a tiny compiled stand-in for the inner
// resolve, exactly as TestContainerE2EResolveFakeScript already does for the
// driver mechanics -- still exercises everything this fix actually changed
// for real: the merged --external-name argv (install set + upgrade set),
// never passing --upgrades, a real docker/podman run with --network none,
// reading /work/plan.json back through the real bind mount, and
// containerUpgradeMismatches catching the divergence.
func TestContainerE2EClosedWorldUpgradeDivergence(t *testing.T) {
	requireContainerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	arch := containerE2EDpkgArch(t)
	fakeBin := containerE2EBuildFakeDebark(t, arch)

	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: arch, Version: "1.0-1", Reason: lock.ReasonRequested},
			// The online solve's own full-upgrade pass locked this package's
			// upgrade target at 1.0-1; the fake binary's hardcoded response
			// above reports 2.0-1 instead.
			{Name: "debark-e3-demo", Arch: "all", Version: "1.0-1", Reason: lock.ReasonUpgrade},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	backend := newContainerBackend(ContainerOptions{
		SelfPath: fakeBin,
		Image:    "busybox:latest", // no apt needed: the fake binary never shells out to it
	})
	cw, err := backend.ClosedWorld(ctx, ClosedWorldInput{
		Snapshot: &snapshot.Snapshot{
			SchemaVersion: snapshot.SchemaVersion,
			Target:        snapshot.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: arch},
		},
		SnapshotFilesDir: t.TempDir(),
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:" + arch + "=1.0-1"},
		Upgrades:         true,
		WorkDir:          t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q against a real container; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"debark-e3-demo:all", "1.0-1", "2.0-1"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", cw.Detail, want)
		}
	}
	t.Logf("real container closed-world check correctly failed: %s", cw.Detail)
}

// TestContainerE2ESilentDivergence proves containerVersionMismatches'
// second verification layer (the coordinator-requested extension: closing
// the gap where a bare-name resolve proposes no action at all) against a
// REAL container runtime and a REAL, on-disk captured dpkg status -- not
// just the faked-execContainerFn unit tests in container_test.go. The fake
// inner resolve (containerE2EBuildFakeDebarkEmpty) reports NO Selections
// at all, exactly as a real bare-name resolve would when its candidate
// already matches whatever is installed; containerInstalledVersions then has
// to read the real dpkg status fixture below, host-side, and notice it does
// NOT match the lock's recorded version, all through this package's real,
// unmodified ClosedWorld code running against a real container.
func TestContainerE2ESilentDivergence(t *testing.T) {
	requireContainerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	arch := containerE2EDpkgArch(t)
	fakeBin := containerE2EBuildFakeDebarkEmpty(t, arch)

	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: arch, Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	// A real, on-disk snapshot dpkg status naming demo-app installed at
	// 0.9-1 -- NOT the 1.0-1 the lock above recorded -- read host-side by
	// containerInstalledVersions, entirely independent of the container run.
	filesDir := t.TempDir()
	const archivePath = "var/lib/dpkg/status"
	statusDst := filepath.Join(filesDir, filepath.FromSlash(archivePath))
	if err := os.MkdirAll(filepath.Dir(statusDst), 0o755); err != nil {
		t.Fatal(err)
	}
	statusData := []byte("Package: demo-app\nStatus: install ok installed\nVersion: 0.9-1\nArchitecture: " + arch + "\n\n")
	if err := os.WriteFile(statusDst, statusData, 0o644); err != nil {
		t.Fatal(err)
	}

	backend := newContainerBackend(ContainerOptions{
		SelfPath: fakeBin,
		Image:    "busybox:latest",
	})
	cw, err := backend.ClosedWorld(ctx, ClosedWorldInput{
		Snapshot: &snapshot.Snapshot{
			SchemaVersion: snapshot.SchemaVersion,
			Target:        snapshot.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: arch},
			DpkgStatus:    snapshot.File{Path: "/var/lib/dpkg/status", ArchivePath: archivePath, Size: int64(len(statusData))},
		},
		SnapshotFilesDir: filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:" + arch + "=1.0-1"},
		WorkDir:          t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q against a real container; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"demo-app:" + arch, "1.0-1", "0.9-1", "proposed no change"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", cw.Detail, want)
		}
	}
	t.Logf("real container closed-world check correctly caught the silent divergence: %s", cw.Detail)
}
