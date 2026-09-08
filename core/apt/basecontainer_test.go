package apt

// Pure unit tests for BaseSnapshotInContainer and its argv/name helpers: no
// docker/podman binary, no network, no filesystem beyond t.TempDir(), so
// these pass on Windows, macOS and Linux alike, exactly as
// container_test.go's do and for the same reason
// (docs/dev/contract-brief.md's testing rule). Nothing here runs a
// container; execContainerFn is substituted, the same seam
// container_test.go's closed-world tests use.
//
// Run just these with:
//
//	go test ./core/apt/... -run BaseSnapshot

import (
	"context"
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/snapshot"
)

// --- inner argv -----------------------------------------------------------

func TestContainerBuildFromBaseArgvBuiltin(t *testing.T) {
	got := containerBuildFromBaseArgv(containerFromBaseArgs{
		BaseRef: "ubuntu:26.04/desktop",
		Arch:    "arm64",
		OutName: "base-snapshot.tar.zst",
	})
	want := []string{
		"/debark", "snapshot", "from-base", "ubuntu:26.04/desktop",
		"--arch", "arm64",
		"--backend", "local",
		"--out", "/work/base-snapshot.tar.zst",
		"--json",
	}
	assertArgvEqual(t, got, want)
}

// TestContainerBuildFromBaseArgvMountedFile pins the half that would be a
// silent failure rather than a loud one: for an operator-supplied definition
// file the inner argv must name the MOUNT TARGET, never the host path. A host
// path reaches core/base.Resolve inside the container, which stats it, does
// not find it, and reports "neither a builtin base id nor a file that exists"
// -- naming a path the operator can see on their own disk, which is about the
// most misleading way available to say "the bind mount is missing".
func TestContainerBuildFromBaseArgvMountedFile(t *testing.T) {
	got := containerBuildFromBaseArgv(containerFromBaseArgs{
		BaseRef: containerBaseDefinitionTarget,
		Arch:    "amd64",
		OutName: "bootstrap.tar.zst",
	})
	want := []string{
		"/debark", "snapshot", "from-base", "/base/definition.yaml",
		"--arch", "amd64",
		"--backend", "local",
		"--out", "/work/bootstrap.tar.zst",
		"--json",
	}
	assertArgvEqual(t, got, want)
}

// TestContainerBuildFromBaseArgvAlwaysPassesBackendLocal states the rule
// separately from the golden argv above, because it is the one flag whose
// absence would still produce a working-looking command line. Left to
// "auto", an inner process that decided it could reach a container runtime
// of its own would try to nest one.
func TestContainerBuildFromBaseArgvAlwaysPassesBackendLocal(t *testing.T) {
	for _, arch := range []string{"", "amd64"} {
		got := containerBuildFromBaseArgv(containerFromBaseArgs{
			BaseRef: "debian:12/minimal", Arch: arch, OutName: "s.tar.zst",
		})
		if !containsArgPair(got, "--backend", "local") {
			t.Errorf("arch=%q: argv = %q, want it to carry --backend local", arch, got)
		}
		if arch == "" && containsArg(got, "--arch") {
			t.Errorf("argv = %q, want no --arch when none was given", got)
		}
	}
}

// --- OutName validation ---------------------------------------------------

// TestContainerCheckOutName is the guard on the one caller-supplied string
// that becomes a path on BOTH sides of the container boundary. Every reject
// case here is a path that escapes the directory meant to contain it, and
// the two-separator coverage is the point: filepath.Base answers for the
// host's separator only, so `a\b` is a single legal element on Linux and two
// on Windows, while the same string is joined onto /work inside a Linux
// container either way.
func TestContainerCheckOutName(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		wantErr bool
	}{
		{"plain", "base-snapshot.tar.zst", false},
		{"plain with dots", "ubuntu-26.04-desktop.tar.zst", false},
		{"double dot inside a name is not a parent reference", "a..b.tar.zst", false},
		{"empty", "", true},
		{"parent escape", "../escape", true},
		{"forward slash", "a/b", true},
		{"backslash", `a\b`, true},
		{"absolute", "/etc/passwd", true},
		{"current dir", ".", true},
		{"parent dir", "..", true},
		{"leading dash", "-out.tar.zst", true},
		{"NUL byte", "ok.tar.zst\x00.png", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := containerCheckOutName(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("containerCheckOutName(%q) = nil, want a usage error", tc.out)
				}
				if got := dferr.ClassOf(err); got != dferr.Usage {
					t.Errorf("class = %v, want %v (%v)", got, dferr.Usage, err)
				}
				return
			}
			if err != nil {
				t.Errorf("containerCheckOutName(%q) = %v, want nil", tc.out, err)
			}
		})
	}
}

func TestBaseSnapshotInContainerRejectsBadOutName(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	for _, bad := range []string{"", "../escape", "a/b", `a\b`} {
		in := fx.input()
		in.OutName = bad
		_, err := BaseSnapshotInContainer(context.Background(), in)
		if err == nil {
			t.Fatalf("OutName %q: err = nil, want a usage error", bad)
		}
		if got := dferr.ClassOf(err); got != dferr.Usage {
			t.Errorf("OutName %q: class = %v, want %v (%v)", bad, got, dferr.Usage, err)
		}
	}
}

func TestBaseSnapshotInContainerRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name  string
		mutbr func(*BaseContainerInput)
	}{
		{"no base ref", func(in *BaseContainerInput) { in.BaseRef = "" }},
		{"no architecture", func(in *BaseContainerInput) { in.Target.Arch = "" }},
		{"no work dir", func(in *BaseContainerInput) { in.WorkDir = "" }},
		{"relative work dir", func(in *BaseContainerInput) { in.WorkDir = "work" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := setupBaseContainerFixture(t)
			in := fx.input()
			tc.mutbr(&in)
			_, err := BaseSnapshotInContainer(context.Background(), in)
			if err == nil {
				t.Fatalf("err = nil, want a usage error")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %v, want %v (%v)", got, dferr.Usage, err)
			}
		})
	}
}

// --- the mount set --------------------------------------------------------

// TestBaseSnapshotInContainerMountsBuiltin is the test the contract brief asked
// for by its most interesting property: what is NOT mounted.
//
// It asserts the complete -v set, not just the presence of the two mounts
// that should be there, so it fails if anyone later copy-pastes an
// /archives bind mount, a named store volume or a /snapshot pair in from
// Resolve. Each of those would be wrong for a different reason -- nothing is
// downloaded, so /archives and the volume have nothing to carry; and there
// is no input snapshot at all, because producing one is the whole job -- and
// each would look entirely plausible in a diff.
func TestBaseSnapshotInContainerMountsBuiltin(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	var argv []string
	execContainerFn = fakeFromBaseRun(t, filepath.Join(fx.workDir, fx.outName), 0, "", &argv)

	out, err := BaseSnapshotInContainer(context.Background(), fx.input())
	if err != nil {
		t.Fatalf("BaseSnapshotInContainer: %v", err)
	}
	if want := filepath.Join(fx.workDir, fx.outName); out != want {
		t.Errorf("returned path = %q, want %q", out, want)
	}

	binSrc := mustMountSource(t, fx.selfPath)
	workSrc := mustMountSource(t, fx.workDir)
	assertMountSpecs(t, argv, []string{
		binSrc + ":/debark:ro",
		workSrc + ":/work",
	})
	assertInnerArgv(t, argv, []string{
		"/debark", "snapshot", "from-base", "ubuntu:26.04/desktop",
		"--arch", "amd64",
		"--backend", "local",
		"--out", "/work/" + fx.outName,
		"--json",
	})
}

// TestBaseSnapshotInContainerMountsDefinitionFile is the same assertion for
// an operator-supplied definition file: exactly three mounts, the extra one
// read-only at the fixed target, and the inner argv naming that target
// rather than the host path.
func TestBaseSnapshotInContainerMountsDefinitionFile(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	defPath := writeTempFile(t, "mybase.yaml", []byte("schema_version: debark.base/v1\n"))

	var argv []string
	execContainerFn = fakeFromBaseRun(t, filepath.Join(fx.workDir, fx.outName), 0, "", &argv)

	in := fx.input()
	in.BaseRef = defPath
	in.BaseIsFile = true
	if _, err := BaseSnapshotInContainer(context.Background(), in); err != nil {
		t.Fatalf("BaseSnapshotInContainer: %v", err)
	}

	assertMountSpecs(t, argv, []string{
		mustMountSource(t, fx.selfPath) + ":/debark:ro",
		mustMountSource(t, fx.workDir) + ":/work",
		mustMountSource(t, defPath) + ":" + containerBaseDefinitionTarget + ":ro",
	})
	assertInnerArgv(t, argv, []string{
		"/debark", "snapshot", "from-base", containerBaseDefinitionTarget,
		"--arch", "amd64",
		"--backend", "local",
		"--out", "/work/" + fx.outName,
		"--json",
	})
	if containsArg(argv, defPath) {
		t.Errorf("argv carries the HOST definition path %q; the inner binary must only ever see the mount target", defPath)
	}
}

// TestBaseSnapshotInContainerMissingDefinitionFileIsRefusedHostSide keeps the
// containerCheckMountSourceDir lesson honest on this path too: docker does
// NOT error on a missing bind-mount source, it silently creates an empty
// directory at the target (measured against Docker Desktop 29.6.2 on
// Windows, see container_driver.go), so without a host-side stat the
// operator's typo would come back as core/base failing to parse a directory
// from inside the container.
func TestBaseSnapshotInContainerMissingDefinitionFileIsRefusedHostSide(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	execContainerFn = func(context.Context, string, []string, func(evidence.Event)) (containerRunResult, error) {
		t.Fatal("a missing base definition file must be refused before any container is started")
		return containerRunResult{}, nil
	}
	in := fx.input()
	in.BaseRef = filepath.Join(t.TempDir(), "does-not-exist.yaml")
	in.BaseIsFile = true
	_, err := BaseSnapshotInContainer(context.Background(), in)
	if err == nil {
		t.Fatal("err = nil, want a usage error naming the missing host path")
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("class = %v, want %v (%v)", got, dferr.Usage, err)
	}
}

// --- what comes back ------------------------------------------------------

// TestBaseSnapshotInContainerNonZeroExitIsClassified proves the exit code the
// inner process chose survives as a dferr class rather than being flattened
// -- including that exit 3 is NOT treated the way Resolve treats it (a
// partial result worth reading back). A snapshot is all-or-nothing.
func TestBaseSnapshotInContainerNonZeroExitIsClassified(t *testing.T) {
	cases := []struct {
		exit int
		want dferr.Class
	}{
		{1, dferr.Usage},
		{3, dferr.Incomplete},
		{5, dferr.Resolution},
		{127, dferr.Environment},
	}
	for _, tc := range cases {
		fx := setupBaseContainerFixture(t)
		var argv []string
		// Writes nothing, exactly as a failed inner run would not.
		execContainerFn = fakeFromBaseRun(t, "", tc.exit, "base seeds do not resolve", &argv)
		_, err := BaseSnapshotInContainer(context.Background(), fx.input())
		if err == nil {
			t.Fatalf("exit %d: err = nil, want a classified failure", tc.exit)
		}
		if got := dferr.ClassOf(err); got != tc.want {
			t.Errorf("exit %d: class = %v, want %v (%v)", tc.exit, got, tc.want, err)
		}
	}
}

// TestBaseSnapshotInContainerExitZeroWithNoArchive covers the case the
// weak-on-purpose acceptance check exists for: the container reported
// success and produced nothing. Verification, not Environment: the run
// completed, its claim about itself is what failed.
func TestBaseSnapshotInContainerExitZeroWithNoArchive(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	var argv []string
	execContainerFn = fakeFromBaseRun(t, "", 0, "all done", &argv)
	_, err := BaseSnapshotInContainer(context.Background(), fx.input())
	if err == nil {
		t.Fatal("err = nil, want a verification failure")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("class = %v, want %v (%v)", got, dferr.Verification, err)
	}
}

// TestBaseSnapshotInContainerExitZeroWithEmptyArchive is the twin. An empty
// file is what a truncated write or a container killed mid-flush leaves, and
// it must not be handed to the caller as a snapshot to open.
func TestBaseSnapshotInContainerExitZeroWithEmptyArchive(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	outPath := filepath.Join(fx.workDir, fx.outName)
	execContainerFn = func(_ context.Context, runtimePath string, a []string, _ func(evidence.Event)) (containerRunResult, error) {
		if err := os.WriteFile(outPath, nil, 0o644); err != nil {
			t.Fatalf("write empty archive: %v", err)
		}
		return containerRunResult{Argv: append([]string{runtimePath}, a...)}, nil
	}
	_, err := BaseSnapshotInContainer(context.Background(), fx.input())
	if err == nil {
		t.Fatal("err = nil, want a verification failure for a zero-byte archive")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("class = %v, want %v (%v)", got, dferr.Verification, err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to say the archive is empty rather than that it is missing", err)
	}
}

// TestBaseSnapshotInContainerStaleArchiveIsNotReturned is the twin of
// container_test.go's TestContainerClosedWorld_StalePlanEnvelopeIsNotReadBackAsThisRun,
// and it is the reason the acceptance check above is allowed to be as weak
// as "exists and is not empty". A previous run's archive sitting at the same
// path is not merely plausible-looking, it is a genuinely valid snapshot --
// snapshot.Open would validate it perfectly. Only "this run wrote it" can
// distinguish the two, and only the pre-run removal makes that knowable.
func TestBaseSnapshotInContainerStaleArchiveIsNotReturned(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	outPath := filepath.Join(fx.workDir, fx.outName)
	if err := os.WriteFile(outPath, []byte("a previous run's perfectly valid snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exits 0 and writes nothing, which is exactly the shape the Resolve
	// path measured.
	execContainerFn = func(_ context.Context, runtimePath string, a []string, _ func(evidence.Event)) (containerRunResult, error) {
		return containerRunResult{Argv: append([]string{runtimePath}, a...)}, nil
	}
	_, err := BaseSnapshotInContainer(context.Background(), fx.input())
	if err == nil {
		t.Fatal("err = nil: a leftover archive from a previous run was returned as this run's result")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("class = %v, want %v (%v)", got, dferr.Verification, err)
	}
}

// --- evidence -------------------------------------------------------------

// TestBaseSnapshotInContainerEmitsBackendSelected checks the one event this
// function is contracted to emit, and that the image it names is the one the
// host resolved.
func TestBaseSnapshotInContainerEmitsBackendSelected(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	var argv []string
	execContainerFn = fakeFromBaseRun(t, filepath.Join(fx.workDir, fx.outName), 0, "", &argv)

	events := &evidence.Collector{}
	in := fx.input()
	in.Events = events
	if _, err := BaseSnapshotInContainer(context.Background(), in); err != nil {
		t.Fatalf("BaseSnapshotInContainer: %v", err)
	}

	// One call: Collector.Events returns a fresh copy each time, so indexing
	// into a second call would be reading a different slice than the one
	// just ranged over -- harmless here, and exactly the sort of thing that
	// stops being harmless later.
	emitted := events.Events()
	var selected *evidence.Event
	for i := range emitted {
		if emitted[i].Type == evidence.TypeBackendSelected {
			selected = &emitted[i]
		}
	}
	if selected == nil {
		t.Fatalf("no %s event emitted; events = %+v", evidence.TypeBackendSelected, emitted)
	}
	if got := selected.Attrs["backend"]; got != "container" {
		t.Errorf("backend attr = %v, want container", got)
	}
	if got := selected.Attrs["runtime"]; got != "docker" {
		t.Errorf("runtime attr = %v, want docker", got)
	}
	if got, _ := selected.Attrs["image"].(string); got == "" || !strings.Contains(got, "ubuntu") {
		t.Errorf("image attr = %v, want the ubuntu image the host resolved", selected.Attrs["image"])
	}
}

// TestBaseSnapshotInContainerUnpinnedImageWarns covers the one host-side
// finding that has nowhere structured to go on this path: Resolve attaches
// the unpinned-image warning to the plan it returns, and this function
// returns a bare path, so the evidence stream is the only record. Losing it
// silently would mean a base snapshot built from a re-taggable image with
// nothing anywhere saying so.
func TestBaseSnapshotInContainerUnpinnedImageWarns(t *testing.T) {
	fx := setupBaseContainerFixture(t)
	var argv []string
	execContainerFn = fakeFromBaseRun(t, filepath.Join(fx.workDir, fx.outName), 0, "", &argv)

	events := &evidence.Collector{}
	in := fx.input()
	in.Events = events
	in.Image = "myregistry/ubuntu:custom" // an override with no digest
	if _, err := BaseSnapshotInContainer(context.Background(), in); err != nil {
		t.Fatalf("BaseSnapshotInContainer: %v", err)
	}

	var found bool
	for _, e := range events.Events() {
		if e.Type == evidence.TypeWarning && e.Attrs["code"] == containerWarnUnpinnedImage {
			found = true
			if e.Level != evidence.LevelWarn {
				t.Errorf("level = %q, want %q", e.Level, evidence.LevelWarn)
			}
			if !strings.Contains(e.Msg, "myregistry/ubuntu:custom") {
				t.Errorf("msg = %q, want it to name the unpinned image", e.Msg)
			}
		}
	}
	if !found {
		t.Errorf("no %s event with code %q; events = %+v", evidence.TypeWarning, containerWarnUnpinnedImage, events.Events())
	}
}

// --- fixture --------------------------------------------------------------

type baseContainerFixture struct {
	workDir, selfPath, outName string
}

// setupBaseContainerFixture fakes a machine with docker on PATH and a valid
// linux/amd64 debark binary to mount, and restores both seams afterwards.
// Mirrors setupContainerClosedWorldFixtureFor (container_test.go); kept
// separate because this operation has no snapshot to stage and no bundle
// repo, so sharing that fixture would mean carrying two fields that mean
// nothing here.
func setupBaseContainerFixture(t *testing.T) baseContainerFixture {
	t.Helper()
	origLook := containerLookPath
	t.Cleanup(func() { containerLookPath = origLook })
	containerLookPath = func(name string) (string, error) {
		if name == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	origExec := execContainerFn
	t.Cleanup(func() { execContainerFn = origExec })

	return baseContainerFixture{
		workDir:  t.TempDir(),
		selfPath: writeTempFile(t, "debark-linux-amd64", buildELF64(t, elf.EM_X86_64, elf.ELFDATA2LSB, false)),
		outName:  "base-snapshot.tar.zst",
	}
}

// input is the fixture's default BaseContainerInput: a builtin Ubuntu base,
// which core/distro must be able to resolve an image for.
func (fx baseContainerFixture) input() BaseContainerInput {
	return BaseContainerInput{
		BaseRef:  "ubuntu:26.04/desktop",
		Target:   snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", Codename: "resolute", Arch: "amd64"},
		WorkDir:  fx.workDir,
		OutName:  fx.outName,
		SelfPath: fx.selfPath,
	}
}

// fakeFromBaseRun builds an execContainerFn replacement that captures the
// argv, writes a non-empty archive at outPath when outPath is non-empty
// (what a real inner `snapshot from-base --out /work/...` leaves behind in
// the shared mount), and reports exitCode.
func fakeFromBaseRun(t *testing.T, outPath string, exitCode int, output string, argvOut *[]string) func(context.Context, string, []string, func(evidence.Event)) (containerRunResult, error) {
	t.Helper()
	return func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		*argvOut = argv
		if outPath != "" {
			// Bytes, not an empty file: the "exited 0 but wrote nothing
			// usable" cases have their own tests and must not be reachable
			// from the happy path by accident.
			if err := os.WriteFile(outPath, []byte("\x28\xb5\x2f\xfd fake zstd archive"), 0o644); err != nil {
				t.Fatalf("write fake archive: %v", err)
			}
		}
		return containerRunResult{
			Argv:     append([]string{runtimePath}, argv...),
			Stdout:   []byte(output),
			ExitCode: exitCode,
		}, nil
	}
}

func mustMountSource(t *testing.T, p string) string {
	t.Helper()
	src, err := containerMountSource(p)
	if err != nil {
		t.Fatalf("containerMountSource(%q): %v", p, err)
	}
	return src
}

// assertMountSpecs compares the COMPLETE set of -v specs in a docker argv
// against want, in order. Complete rather than "contains", deliberately: the
// property worth defending on this path is which mounts are absent.
func assertMountSpecs(t *testing.T, argv []string, want []string) {
	t.Helper()
	var got []string
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "-v" {
			got = append(got, argv[i+1])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("mount count = %d, want %d\ngot:  %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("mount[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Stated separately from the exact comparison above so a failure says
	// WHY it matters, not just that a slice differs.
	for _, spec := range got {
		for _, forbidden := range []string{":/archives", ":/snapshot"} {
			if strings.Contains(spec, forbidden) {
				t.Errorf("mount %q carries %s: a base snapshot downloads nothing and has no input snapshot, "+
					"so neither mount has anything to carry", spec, forbidden)
			}
		}
	}
}

// assertInnerArgv compares everything after the image name -- i.e. the
// command the mounted binary is asked to run -- against want.
func assertInnerArgv(t *testing.T, runArgv []string, want []string) {
	t.Helper()
	idx := -1
	for i, a := range runArgv {
		if a == "/debark" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("no /debark in run argv %q", runArgv)
	}
	assertArgvEqual(t, runArgv[idx:], want)
}

func containsArgPair(argv []string, flag, value string) bool {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
