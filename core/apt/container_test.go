package apt

// Pure unit tests for the container backend: no docker/podman binary, no
// network, no filesystem beyond t.TempDir(), so these pass on Windows,
// macOS and Linux alike (docs/dev/contract-brief.md's testing rule). The
// Docker-backed tests that actually run a container live in
// container_e2e_test.go, gated separately.
//
// Run just this package's container tests with:
//
//	go test ./core/apt/... -run Container
//
// Regenerate the golden argv fixture (after a deliberate change to argv
// construction) with:
//
//	go test ./core/apt/... -run RunArgvGolden -update

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

var updateGolden = flag.Bool("update", false, "update golden files")

// --- containerMountSource ------------------------------------------------

func TestContainerMountSource(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"windows drive backslash", `D:\projects\debark`, "d:/projects/debark", false},
		{"windows drive already forward", `d:/already/forward`, "d:/already/forward", false},
		{"windows drive mixed slashes", `C:\Users\operator\AppData\Local\Temp\work`, "c:/Users/operator/AppData/Local/Temp/work", false},
		{"windows drive trailing backslash", `E:\data\`, "e:/data", false},
		{"windows drive root only", `F:\`, "f:/", false},
		{"already lowercase", `d:\projects`, "d:/projects", false},
		{"unc path rejected", `\\server\share\x`, "", true},
		{"unc path forward form rejected", `//server/share/x`, "", true},
		{"relative path rejected", `projects\debark`, "", true},
		{"bare drive rejected", `D:`, "", true},
		{"empty rejected", ``, "", true},
		{"unix absolute passthrough", `/home/user/project`, "/home/user/project", false},
		{"unix with dot segments cleaned", `/home/user/../user/project`, "/home/user/project", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := containerMountSource(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("containerMountSource(%q) = %q, want error", c.in, got)
				}
				if dferr.ClassOf(err) != dferr.Usage {
					t.Errorf("containerMountSource(%q) error class = %v, want Usage", c.in, dferr.ClassOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("containerMountSource(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("containerMountSource(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- containerPlatformFor -------------------------------------------------

func TestContainerPlatformFor(t *testing.T) {
	got, err := containerPlatformFor(ContainerOptions{}, "arm64")
	if err != nil || got != "linux/arm64" {
		t.Fatalf("containerPlatformFor(arm64) = %q, %v, want linux/arm64, nil", got, err)
	}

	got, err = containerPlatformFor(ContainerOptions{Platform: "linux/riscv64"}, "amd64")
	if err != nil || got != "linux/riscv64" {
		t.Fatalf("override: containerPlatformFor = %q, %v, want linux/riscv64, nil", got, err)
	}

	_, err = containerPlatformFor(ContainerOptions{}, "sparc64")
	if err == nil {
		t.Fatal("containerPlatformFor(sparc64) = nil error, want an error for an unknown architecture")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("unknown arch error class = %v, want Usage", dferr.ClassOf(err))
	}
}

// --- ELF validation --------------------------------------------------

// buildELF64 constructs a minimal, syntactically valid ELF64 binary with the
// given machine/data encoding, for testing containerValidateBinary without a
// real compiled binary. When withInterp is true it adds one PT_INTERP
// program header, simulating a dynamically linked binary.
func buildELF64(t *testing.T, machine elf.Machine, data elf.Data, withInterp bool) []byte {
	t.Helper()
	order := binary.ByteOrder(binary.LittleEndian)
	if data == elf.ELFDATA2MSB {
		order = binary.BigEndian
	}
	const ehsize = 64
	const phentsize = 56
	var phoff uint64
	var phnum uint16
	var phdr []byte
	if withInterp {
		phoff = ehsize
		phnum = 1
		phdr = make([]byte, phentsize)
		order.PutUint32(phdr[0:4], uint32(elf.PT_INTERP)) // p_type
	}

	buf := make([]byte, ehsize+len(phdr))
	copy(buf[0:4], []byte{0x7f, 'E', 'L', 'F'})
	buf[4] = byte(elf.ELFCLASS64)
	buf[5] = byte(data)
	buf[6] = 1 // EI_VERSION
	order.PutUint16(buf[16:18], uint16(elf.ET_EXEC))
	order.PutUint16(buf[18:20], uint16(machine))
	order.PutUint32(buf[20:24], 1) // e_version
	order.PutUint64(buf[32:40], phoff)
	order.PutUint16(buf[52:54], ehsize)
	order.PutUint16(buf[54:56], phentsize)
	order.PutUint16(buf[56:58], phnum)
	order.PutUint16(buf[58:60], 64) // e_shentsize, unused (e_shnum=0)
	if len(phdr) > 0 {
		copy(buf[ehsize:], phdr)
	}
	return buf
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestContainerValidateBinary(t *testing.T) {
	t.Run("valid static amd64", func(t *testing.T) {
		p := writeTempFile(t, "debark-amd64", buildELF64(t, elf.EM_X86_64, elf.ELFDATA2LSB, false))
		if err := containerValidateBinary(p, "amd64"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid static arm64", func(t *testing.T) {
		p := writeTempFile(t, "debark-arm64", buildELF64(t, elf.EM_AARCH64, elf.ELFDATA2LSB, false))
		if err := containerValidateBinary(p, "arm64"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid static s390x (big endian)", func(t *testing.T) {
		p := writeTempFile(t, "debark-s390x", buildELF64(t, elf.EM_S390, elf.ELFDATA2MSB, false))
		if err := containerValidateBinary(p, "s390x"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("wrong architecture", func(t *testing.T) {
		p := writeTempFile(t, "debark-amd64", buildELF64(t, elf.EM_X86_64, elf.ELFDATA2LSB, false))
		err := containerValidateBinary(p, "arm64")
		if err == nil {
			t.Fatal("expected an error validating an amd64 binary as arm64")
		}
		if dferr.ClassOf(err) != dferr.Environment {
			t.Errorf("error class = %v, want Environment", dferr.ClassOf(err))
		}
		if dferr.HintOf(err) == "" {
			t.Error("expected a Hint telling the operator how to build the right binary")
		}
	})

	t.Run("dynamically linked rejected", func(t *testing.T) {
		p := writeTempFile(t, "debark-dynamic", buildELF64(t, elf.EM_X86_64, elf.ELFDATA2LSB, true))
		err := containerValidateBinary(p, "amd64")
		if err == nil {
			t.Fatal("expected an error validating a dynamically linked binary")
		}
		if !strings.Contains(err.Error(), "dynamically linked") {
			t.Errorf("error = %v, want it to mention 'dynamically linked'", err)
		}
	})

	t.Run("not an ELF file (Windows PE header)", func(t *testing.T) {
		pe := []byte{'M', 'Z', 0x90, 0x00, 0x03, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00}
		p := writeTempFile(t, "debark.exe", pe)
		err := containerValidateBinary(p, "amd64")
		if err == nil {
			t.Fatal("expected an error validating a PE binary")
		}
		if dferr.ClassOf(err) != dferr.Environment {
			t.Errorf("error class = %v, want Environment", dferr.ClassOf(err))
		}
	})

	t.Run("missing file", func(t *testing.T) {
		err := containerValidateBinary(filepath.Join(t.TempDir(), "does-not-exist"), "amd64")
		if err == nil {
			t.Fatal("expected an error for a missing file")
		}
	})

	t.Run("unknown target architecture", func(t *testing.T) {
		p := writeTempFile(t, "debark", buildELF64(t, elf.EM_X86_64, elf.ELFDATA2LSB, false))
		err := containerValidateBinary(p, "sparc64")
		if err == nil {
			t.Fatal("expected an error for an architecture debark does not know")
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("error class = %v, want Usage", dferr.ClassOf(err))
		}
	})
}

// --- runtime detection -------------------------------------------------

func TestContainerDetectRuntime(t *testing.T) {
	orig := containerLookPath
	t.Cleanup(func() { containerLookPath = orig })

	t.Run("docker preferred over podman", func(t *testing.T) {
		containerLookPath = func(name string) (string, error) {
			switch name {
			case "docker":
				return "/usr/bin/docker", nil
			case "podman":
				return "/usr/bin/podman", nil
			}
			return "", errors.New("not found")
		}
		name, path, err := containerDetectRuntime(ContainerOptions{})
		if err != nil || name != "docker" || path != "/usr/bin/docker" {
			t.Fatalf("got %q %q %v, want docker /usr/bin/docker nil", name, path, err)
		}
	})

	t.Run("falls back to podman", func(t *testing.T) {
		containerLookPath = func(name string) (string, error) {
			if name == "podman" {
				return "/usr/bin/podman", nil
			}
			return "", errors.New("not found")
		}
		name, path, err := containerDetectRuntime(ContainerOptions{})
		if err != nil || name != "podman" || path != "/usr/bin/podman" {
			t.Fatalf("got %q %q %v, want podman /usr/bin/podman nil", name, path, err)
		}
	})

	t.Run("neither present is a classified environment error with a hint", func(t *testing.T) {
		containerLookPath = func(name string) (string, error) { return "", errors.New("not found") }
		_, _, err := containerDetectRuntime(ContainerOptions{})
		if err == nil {
			t.Fatal("expected an error when no runtime is on PATH")
		}
		if dferr.ClassOf(err) != dferr.Environment {
			t.Errorf("error class = %v, want Environment", dferr.ClassOf(err))
		}
		if dferr.HintOf(err) == "" {
			t.Error("expected a Hint naming the install command")
		}
	})

	t.Run("explicit runtime request not found", func(t *testing.T) {
		containerLookPath = func(name string) (string, error) { return "", errors.New("not found") }
		_, _, err := containerDetectRuntime(ContainerOptions{Runtime: "nerdctl"})
		if err == nil || dferr.ClassOf(err) != dferr.Environment {
			t.Fatalf("got %v, want a classified Environment error", err)
		}
	})
}

func TestContainerRuntimeInstallHintPerPlatform(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux", "plan9"} {
		hint := containerRuntimeInstallHintFor(goos)
		if hint == "" {
			t.Errorf("containerRuntimeInstallHintFor(%q) is empty", goos)
		}
		if !strings.Contains(strings.ToLower(hint), "docker") {
			t.Errorf("containerRuntimeInstallHintFor(%q) = %q, want it to mention docker", goos, hint)
		}
	}
}

// --- argv construction (golden) ----------------------------------------

func TestContainerBuildInnerArgv(t *testing.T) {
	got := containerBuildInnerArgv(containerResolveArgs{
		Packages:      []string{"jq", "tree=2.1.0-1"},
		ExternalRepo:  "/external",
		ExternalNames: []string{"mytool"},
		Recommends:    true,
		RecommendsSet: true,
		Upgrades:      true,
		Arch:          "arm64",
		ApprovedKeys:  []string{"BBBB000000000000000000000000000000BBBB", "AAAA000000000000000000000000000000AAAA"},
	})
	want := []string{
		"/debark", "resolve",
		"--snapshot", "/snapshot",
		"--archives", "/archives",
		"--work", "/work",
		"--plan-out", "/work/plan.json",
		"--backend", "local",
		"--package", "jq",
		"--package", "tree=2.1.0-1",
		"--external-repo", "/external",
		"--external-name", "mytool",
		"--recommends=true",
		"--upgrades",
		"--arch", "arm64",
		"--approved-key", "AAAA000000000000000000000000000000AAAA",
		"--approved-key", "BBBB000000000000000000000000000000BBBB",
		"--json",
	}
	assertArgvEqual(t, got, want)
}

// TestContainerBuildInnerArgvMinimal also pins the opt-in half of the
// --recommends contract: with RecommendsSet false the flag must not appear at
// all, so the inner process reaches its own documented default ("from the
// snapshot"). Before RecommendsSet existed, containerBuildInnerArgv appended
// --recommends=%t unconditionally and this same zero-valued call shipped
// "--recommends=false" -- a policy nobody chose, and the exact reason
// container.go's ClosedWorld comment ("--recommends is left unset") described
// behaviour the code did not have.
func TestContainerBuildInnerArgvMinimal(t *testing.T) {
	got := containerBuildInnerArgv(containerResolveArgs{Recommends: false})
	want := []string{
		"/debark", "resolve",
		"--snapshot", "/snapshot",
		"--archives", "/archives",
		"--work", "/work",
		"--plan-out", "/work/plan.json",
		"--backend", "local",
		"--json",
	}
	assertArgvEqual(t, got, want)
	for _, a := range got {
		if strings.HasPrefix(a, "--recommends") {
			t.Errorf("argv carries %q for a caller that expressed no Recommends opinion: a zero value is not a decision", a)
		}
	}
}

// TestContainerBuildInnerArgvRecommendsExplicitFalse is the companion: a
// caller that DID decide false still gets the flag, so RecommendsSet only
// distinguishes "unset" from "false" and never suppresses a real choice.
func TestContainerBuildInnerArgvRecommendsExplicitFalse(t *testing.T) {
	got := containerBuildInnerArgv(containerResolveArgs{Recommends: false, RecommendsSet: true})
	if !containsArg(got, "--recommends=false") {
		t.Errorf("argv = %q, want it to carry --recommends=false for an explicit choice", got)
	}
}

func assertArgvEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length = %d, want %d\ngot:  %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\ngot:  %q\nwant: %q", i, got[i], want[i], got, want)
		}
	}
}

// TestContainerRunArgvGolden renders the exact docker command line the
// driver would run for a representative Resolve() call and compares it
// against a checked-in golden file -- the highest-value test in this
// package, per the contract brief. Regenerate with `-update` after a
// deliberate change to argv construction.
func TestContainerRunArgvGolden(t *testing.T) {
	mounts := []containerMount{
		{Source: "d:/bin/debark-linux-arm64", Target: "/debark", RO: true},
		{Source: "d:/work/container-snapshot/snapshot.json", Target: "/snapshot/snapshot.json", RO: true},
		{Source: "d:/snap/files", Target: "/snapshot/files", RO: true},
		{Source: "d:/work", Target: "/work", RO: false},
		{Source: "d:/archives", Target: "/archives", RO: false},
		{Source: "d:/external", Target: "/external", RO: true},
	}
	inner := containerBuildInnerArgv(containerResolveArgs{
		Packages:      []string{"jq", "tree"},
		ExternalRepo:  "/external",
		ExternalNames: []string{"mytool"},
		Recommends:    true,
		RecommendsSet: true,
		Upgrades:      false,
		Arch:          "arm64",
		ApprovedKeys:  []string{"AAAA000000000000000000000000000000AAAA"},
	})
	argv := containerBuildRunArgv("linux/arm64", mounts, nil,
		"docker.io/library/debian:bookworm-slim@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		inner)

	got := "docker\n" + strings.Join(argv, "\n") + "\n"
	goldenPath := filepath.Join("testdata", "container", "run-argv.golden")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test ./core/apt/... -run RunArgvGolden -update` to create it)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("docker argv mismatch (run with -update to see/regenerate)\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestContainerRunArgvWithEventsGolden is the SAME command line as
// TestContainerRunArgvGolden with the one difference production actually has:
// the driver forwards the inner process's event stream whenever it has a sink
// to forward it to, and the engine always gives it one.
//
// A second golden rather than an edit to the first, because both shapes are
// real and the frozen contract now has two of them. The first is what a
// caller with no event sink runs, and every "nothing else changed" claim in
// containerevents.go rests on that argv still being byte-for-byte what it
// was; this one is what an operator's build runs. Pinning only one would leave
// the shape that ships unpinned, which is the wrong one to leave unpinned.
func TestContainerRunArgvWithEventsGolden(t *testing.T) {
	mounts := []containerMount{
		{Source: "d:/bin/debark-linux-arm64", Target: "/debark", RO: true},
		{Source: "d:/work/container-snapshot/snapshot.json", Target: "/snapshot/snapshot.json", RO: true},
		{Source: "d:/snap/files", Target: "/snapshot/files", RO: true},
		{Source: "d:/work", Target: "/work", RO: false},
		{Source: "d:/archives", Target: "/archives", RO: false},
		{Source: "d:/external", Target: "/external", RO: true},
	}
	inner := containerBuildInnerArgv(containerResolveArgs{
		Packages:      []string{"jq", "tree"},
		ExternalRepo:  "/external",
		ExternalNames: []string{"mytool"},
		Recommends:    true,
		RecommendsSet: true,
		Upgrades:      false,
		Arch:          "arm64",
		ApprovedKeys:  []string{"AAAA000000000000000000000000000000AAAA"},
		JSONEvents:    true,
	})
	argv := containerBuildRunArgv("linux/arm64", mounts, nil,
		"docker.io/library/debian:bookworm-slim@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		inner)

	got := "docker\n" + strings.Join(argv, "\n") + "\n"
	goldenPath := filepath.Join("testdata", "container", "run-argv-events.golden")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test ./core/apt/... -run RunArgvWithEventsGolden -update` to create it)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("docker argv mismatch (run with -update to see/regenerate)\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestContainerRunArgvClosedWorldGolden covers the --network none path used
// only by ClosedWorld.
func TestContainerRunArgvClosedWorldGolden(t *testing.T) {
	mounts := []containerMount{
		{Source: "d:/bin/debark-linux-amd64", Target: "/debark", RO: true},
		{Source: "d:/work/snapshot.json", Target: "/snapshot/snapshot.json", RO: true},
		{Source: "d:/snap/files", Target: "/snapshot/files", RO: true},
		{Source: "d:/work", Target: "/work", RO: false},
		{Source: "d:/work/closedworld-archives", Target: "/archives", RO: false},
		{Source: "d:/bundle/repo", Target: "/bundlerepo", RO: true},
	}
	// Mirrors containerBackend.ClosedWorld's own call field for field,
	// including Recommends: true -- the value localBackend.ClosedWorld's
	// RootSpec uses (closedworld.go), which this backend now matches instead
	// of shipping a zero-valued "--recommends=false" it never chose.
	inner := containerBuildInnerArgv(containerResolveArgs{
		ExternalRepo:  "/bundlerepo",
		ExternalNames: []string{"jq", "tree"},
		Recommends:    true,
		RecommendsSet: true,
		Arch:          "amd64",
	})
	argv := containerBuildRunArgv("linux/amd64", mounts, []string{"--network", "none"},
		"docker.io/library/debian:bookworm-slim@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		inner)

	got := "docker\n" + strings.Join(argv, "\n") + "\n"
	goldenPath := filepath.Join("testdata", "container", "run-argv-closedworld.golden")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test ./core/apt/... -run RunArgvClosedWorldGolden -update` to create it)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("docker argv mismatch (run with -update to see/regenerate)\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// --- envelope parsing ----------------------------------------------------

func TestContainerParseEnvelopeRoundTrip(t *testing.T) {
	env := containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Target:   lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
			Resolver: lock.Resolver{Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22"},
			Selections: []resolve.Selection{
				{Name: "jq", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "jq_1.6-2.1+deb12u2_amd64.deb", Size: 63984, SHA256: strings.Repeat("a", 64), Reason: lock.ReasonRequested},
			},
			Install: []string{"jq=1.6-2.1+deb12u2"},
		},
		Backend:     containerEnvelopeBackend{Kind: "local", APTVersion: "2.6.1", DpkgVersion: "1.21.22", DistroID: "debian", VersionID: "12"},
		ArchivesDir: "/archives",
		Files: []containerEnvelopeFile{
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "jq_1.6-2.1+deb12u2_amd64.deb", SHA256: strings.Repeat("a", 64), Size: 63984},
		},
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := containerParseEnvelope(data)
	if err != nil {
		t.Fatalf("containerParseEnvelope: %v", err)
	}
	if got.Plan.Target.DistroID != "debian" || got.Plan.Resolver.APTVersion != "2.6.1" {
		t.Errorf("round-tripped plan lost fields: %+v", got.Plan)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "jq" {
		t.Errorf("round-tripped files = %+v", got.Files)
	}
	if err := containerCrossCheckEnvelope(got, nil, ""); err != nil {
		t.Errorf("cross-check on a self-consistent envelope: %v", err)
	}
}

func TestContainerParseEnvelopeLiteralExample(t *testing.T) {
	// A hand-written JSON literal shaped like docs/dev/resolve-contract.md's
	// example envelope (its own "plan" field is a placeholder in the doc, so
	// a minimal-but-valid resolve.Plan stands in for it here).
	const raw = `{
		"schema_version": "debark.resolveplan/v1",
		"plan": {
			"Target": {"distro_id": "debian", "version_id": "12", "codename": "bookworm", "arch": "amd64"},
			"Resolver": {"backend": "local", "apt_version": "2.6.1", "dpkg_version": "1.21.22", "phased_updates": "never-include", "install_recommends": true},
			"Selections": [],
			"Install": []
		},
		"backend": {"kind": "local", "apt_version": "2.6.1", "dpkg_version": "1.21.22", "distro_id": "debian", "version_id": "12"},
		"archives_dir": "/archives",
		"files": [
			{"name": "jq", "arch": "amd64", "version": "1.6-2.1+deb12u2", "filename": "jq_1.6-2.1+deb12u2_amd64.deb", "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "size": 63984}
		]
	}`
	env, err := containerParseEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("containerParseEnvelope: %v", err)
	}
	if env.Backend.Kind != "local" || env.Backend.APTVersion != "2.6.1" {
		t.Errorf("backend = %+v", env.Backend)
	}
	if env.Plan.Target.DistroID != "debian" || env.Plan.Resolver.APTVersion != "2.6.1" {
		t.Errorf("plan = %+v", env.Plan)
	}
	if len(env.Files) != 1 || env.Files[0].Size != 63984 {
		t.Errorf("files = %+v", env.Files)
	}
}

func TestContainerParseEnvelopeErrors(t *testing.T) {
	t.Run("malformed json", func(t *testing.T) {
		_, err := containerParseEnvelope([]byte(`{not json`))
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
	t.Run("wrong schema version", func(t *testing.T) {
		_, err := containerParseEnvelope([]byte(`{"schema_version":"debark.resolveplan/v2"}`))
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
}

// --- plan/files cross-check ------------------------------------------

func containerBaseEnvelope() containerEnvelope {
	return containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Selections: []resolve.Selection{
				{Name: "jq", Arch: "amd64", Version: "1.6-2.1", Filename: "jq_1.6-2.1_amd64.deb", Size: 100, SHA256: strings.Repeat("a", 64), Reason: lock.ReasonRequested},
			},
		},
		Files: []containerEnvelopeFile{
			{Name: "jq", Arch: "amd64", Version: "1.6-2.1", Filename: "jq_1.6-2.1_amd64.deb", Size: 100, SHA256: strings.Repeat("a", 64)},
		},
	}
}

func TestContainerCrossCheckEnvelope(t *testing.T) {
	t.Run("consistent", func(t *testing.T) {
		if err := containerCrossCheckEnvelope(containerBaseEnvelope(), nil, ""); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("external selection not required in files", func(t *testing.T) {
		env := containerBaseEnvelope()
		env.Plan.Selections = append(env.Plan.Selections, resolve.Selection{
			Name: "vendor-tool", Arch: "amd64", Version: "1.0", Filename: "vendor-tool_1.0_amd64.deb", Reason: lock.ReasonExternal,
		})
		// "vendor-tool" is what this build asked for as an external, so the
		// label is the host's own conclusion and the files[] exemption
		// applies; see TestContainerCrossCheckEnvelope_ExternalLabelMustMatch-
		// WhatTheHostStaged for the case where it is not.
		if err := containerCrossCheckEnvelope(env, []string{"vendor-tool"}, ""); err != nil {
			t.Errorf("external selection should not require a files[] entry: %v", err)
		}
	})

	t.Run("plan selects something files does not have", func(t *testing.T) {
		env := containerBaseEnvelope()
		env.Plan.Selections = append(env.Plan.Selections, resolve.Selection{
			Name: "missing", Arch: "amd64", Version: "1.0", Filename: "missing_1.0_amd64.deb", Reason: lock.ReasonRequested,
		})
		err := containerCrossCheckEnvelope(env, nil, "")
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})

	t.Run("files has something plan does not select", func(t *testing.T) {
		env := containerBaseEnvelope()
		env.Files = append(env.Files, containerEnvelopeFile{Name: "extra", Arch: "amd64", Version: "1.0", Filename: "extra_1.0_amd64.deb"})
		err := containerCrossCheckEnvelope(env, nil, "")
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})

	t.Run("version disagreement", func(t *testing.T) {
		env := containerBaseEnvelope()
		env.Files[0].Version = "9.9.9"
		env.Files[0].Filename = "jq_9.9.9_amd64.deb"
		err := containerCrossCheckEnvelope(env, nil, "")
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})

	t.Run("digest disagreement", func(t *testing.T) {
		env := containerBaseEnvelope()
		env.Files[0].SHA256 = strings.Repeat("b", 64)
		err := containerCrossCheckEnvelope(env, nil, "")
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})

	t.Run("size disagreement", func(t *testing.T) {
		env := containerBaseEnvelope()
		env.Files[0].Size = 999
		err := containerCrossCheckEnvelope(env, nil, "")
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
}

// --- host-side re-verification -----------------------------------------

func TestContainerReverifyFiles(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake .deb contents for testing\n")
	if err := os.WriteFile(filepath.Join(dir, "jq_1.6-2.1_amd64.deb"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := digest.SHA256File(filepath.Join(dir, "jq_1.6-2.1_amd64.deb"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("matches", func(t *testing.T) {
		env := containerEnvelope{Files: []containerEnvelopeFile{
			{Name: "jq", Filename: "jq_1.6-2.1_amd64.deb", SHA256: sum, Size: size},
		}}
		if err := containerReverifyFiles(env, dir); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		env := containerEnvelope{Files: []containerEnvelopeFile{
			{Name: "jq", Filename: "jq_1.6-2.1_amd64.deb", SHA256: strings.Repeat("f", 64), Size: size},
		}}
		err := containerReverifyFiles(env, dir)
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})

	t.Run("size mismatch", func(t *testing.T) {
		env := containerEnvelope{Files: []containerEnvelopeFile{
			{Name: "jq", Filename: "jq_1.6-2.1_amd64.deb", SHA256: sum, Size: size + 1},
		}}
		err := containerReverifyFiles(env, dir)
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})

	t.Run("missing file (truncated mount)", func(t *testing.T) {
		env := containerEnvelope{Files: []containerEnvelopeFile{
			{Name: "gone", Filename: "gone_1.0_amd64.deb", SHA256: sum, Size: size},
		}}
		err := containerReverifyFiles(env, dir)
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
}

// --- error classification -----------------------------------------------

func TestContainerExitClassFor(t *testing.T) {
	cases := []struct {
		code int
		want dferr.Class
	}{
		{0, dferr.Success}, {1, dferr.Usage}, {2, dferr.Environment}, {3, dferr.Incomplete},
		{4, dferr.Verification}, {5, dferr.Resolution}, {6, dferr.Policy}, {7, dferr.TargetMismatch},
		{127, dferr.Environment}, {137, dferr.Environment}, {-1, dferr.Environment},
	}
	for _, c := range cases {
		if got := containerExitClassFor(c.code); got != c.want {
			t.Errorf("containerExitClassFor(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestContainerClassifyRunResult(t *testing.T) {
	t.Run("exec format error -> environment with binfmt hint", func(t *testing.T) {
		res := containerRunResult{
			Argv: []string{"docker", "run"}, ExitCode: 1,
			Stderr: []byte("standard_init_linux.go:228: exec user process caused: exec format error\n"),
		}
		err := containerClassifyRunResult(res, "linux/arm64")
		if dferr.ClassOf(err) != dferr.Environment {
			t.Fatalf("class = %v, want Environment", dferr.ClassOf(err))
		}
		if !strings.Contains(strings.ToLower(dferr.HintOf(err)), "binfmt") && !strings.Contains(strings.ToLower(dferr.HintOf(err)), "qemu") {
			t.Errorf("hint = %q, want it to mention binfmt/qemu", dferr.HintOf(err))
		}
	})

	t.Run("daemon unreachable -> environment", func(t *testing.T) {
		res := containerRunResult{
			Argv: []string{"docker", "run"}, ExitCode: 1,
			Stderr: []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"),
		}
		err := containerClassifyRunResult(res, "linux/amd64")
		if dferr.ClassOf(err) != dferr.Environment {
			t.Fatalf("class = %v, want Environment", dferr.ClassOf(err))
		}
	})

	t.Run("no matching manifest -> environment", func(t *testing.T) {
		res := containerRunResult{
			Argv: []string{"docker", "run"}, ExitCode: 1,
			Stderr: []byte("no matching manifest for linux/riscv64 in the manifest list entries\n"),
		}
		err := containerClassifyRunResult(res, "linux/riscv64")
		if dferr.ClassOf(err) != dferr.Environment {
			t.Fatalf("class = %v, want Environment", dferr.ClassOf(err))
		}
	})

	t.Run("falls back to inner exit code", func(t *testing.T) {
		res := containerRunResult{Argv: []string{"docker", "run"}, ExitCode: 5, Stderr: []byte("E: Unable to locate package bogus\n")}
		err := containerClassifyRunResult(res, "linux/amd64")
		if dferr.ClassOf(err) != dferr.Resolution {
			t.Fatalf("class = %v, want Resolution", dferr.ClassOf(err))
		}
	})
}

// --- image version output parsing --------------------------------------

func TestContainerParseAptGetVersion(t *testing.T) {
	cases := map[string]string{
		"apt 2.6.1 (amd64)":              "2.6.1",
		"  apt 2.8.3 (arm64)  ":          "2.8.3",
		"apt 3.0.2 (amd64) with solver3": "3.0.2",
		"":                               "",
		"not apt output at all":          "",
	}
	for in, want := range cases {
		if got := containerParseAptGetVersion(in); got != want {
			t.Errorf("containerParseAptGetVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerParseDpkgVersion(t *testing.T) {
	cases := map[string]string{
		"Debian 'dpkg' package management program version 1.21.22 (amd64).": "1.21.22",
		"Debian 'dpkg' package management program version 1.22.6 (arm64).":  "1.22.6",
		"":                          "",
		"totally unrelated garbage": "",
	}
	for in, want := range cases {
		if got := containerParseDpkgVersion(in); got != want {
			t.Errorf("containerParseDpkgVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerParseOSRelease(t *testing.T) {
	const debian12 = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION="12 (bookworm)"
ID=debian
`
	id, ver := containerParseOSRelease([]byte(debian12))
	if id != "debian" || ver != "12" {
		t.Errorf("got id=%q ver=%q, want debian 12", id, ver)
	}

	const ubuntu2404 = `NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
`
	id, ver = containerParseOSRelease([]byte(ubuntu2404))
	if id != "ubuntu" || ver != "24.04" {
		t.Errorf("got id=%q ver=%q, want ubuntu 24.04", id, ver)
	}

	id, ver = containerParseOSRelease([]byte(""))
	if id != "" || ver != "" {
		t.Errorf("empty input: got id=%q ver=%q, want empty", id, ver)
	}
}

// --- image resolution / digest pinning ----------------------------------

func TestContainerResolveImage(t *testing.T) {
	t.Run("table release with no pinned digest warns", func(t *testing.T) {
		release, image, imageDigest, warn, err := containerResolveImage(ContainerOptions{}, "debian", "12", "bookworm")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if image == "" || !strings.Contains(image, "debian") {
			t.Errorf("image = %q, want it to name debian", image)
		}
		if release.ImageDigest == "" && warn == "" {
			t.Error("an unpinned table release should produce a warning")
		}
		if imageDigest != release.ImageDigest {
			t.Errorf("imageDigest = %q, want the table row's %q when no override is in play", imageDigest, release.ImageDigest)
		}
	})

	t.Run("operator override already pinned: no warning", func(t *testing.T) {
		_, image, imageDigest, warn, err := containerResolveImage(
			ContainerOptions{Image: "example.com/debian@sha256:" + strings.Repeat("a", 64)},
			"debian", "12", "bookworm")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if warn != "" {
			t.Errorf("warn = %q, want empty for an already-pinned override", warn)
		}
		if !strings.HasPrefix(image, "example.com/debian@sha256:") {
			t.Errorf("image = %q", image)
		}
		if want := "sha256:" + strings.Repeat("a", 64); imageDigest != want {
			t.Errorf("imageDigest = %q, want %q parsed out of the override itself", imageDigest, want)
		}
	})

	t.Run("operator override without digest warns", func(t *testing.T) {
		_, _, imageDigest, warn, err := containerResolveImage(ContainerOptions{Image: "example.com/debian:bookworm"}, "debian", "12", "bookworm")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if warn == "" {
			t.Error("expected a warning for a tag-only override image")
		}
		if imageDigest != "" {
			t.Errorf("imageDigest = %q, want empty: a tag-only override ran an image whose digest this build does not know", imageDigest)
		}
	})

	t.Run("unknown release is a usage error", func(t *testing.T) {
		_, _, _, _, err := containerResolveImage(ContainerOptions{}, "debian", "999", "nonexistent")
		if err == nil || dferr.ClassOf(err) != dferr.Usage {
			t.Fatalf("got %v, want a Usage error", err)
		}
	})
}

// --- mount source existence checks --------------------------------------

func TestContainerCheckMountSourceDir(t *testing.T) {
	dir := t.TempDir()
	if err := containerCheckMountSourceDir(dir, "test"); err != nil {
		t.Errorf("unexpected error for a real directory: %v", err)
	}
	if err := containerCheckMountSourceDir(filepath.Join(dir, "nope"), "test"); err == nil {
		t.Error("expected an error for a missing directory")
	}
	f := writeTempFile(t, "afile", []byte("x"))
	if err := containerCheckMountSourceDir(f, "test"); err == nil {
		t.Error("expected an error mounting a file as a directory")
	}
}

func TestContainerCheckMountSourceFile(t *testing.T) {
	f := writeTempFile(t, "afile", []byte("x"))
	if err := containerCheckMountSourceFile(f, "test"); err != nil {
		t.Errorf("unexpected error for a real file: %v", err)
	}
	dir := t.TempDir()
	if err := containerCheckMountSourceFile(dir, "test"); err == nil {
		t.Error("expected an error mounting a directory as a file")
	}
}

// --- snapshot dir staging -------------------------------------------

func TestContainerStageSnapshotDir(t *testing.T) {
	work := t.TempDir()
	filesDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(filesDir, "etc-apt-sources.list"), []byte("deb http://example/ bookworm main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Target:        snapshot.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
	}
	jsonPath, filesPath, err := containerStageSnapshotDir(work, snap, filesDir)
	if err != nil {
		t.Fatalf("containerStageSnapshotDir: %v", err)
	}
	if filesPath != filesDir {
		t.Errorf("filesPath = %q, want it to reuse SnapshotFilesDir unchanged (%q)", filesPath, filesDir)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read staged snapshot.json: %v", err)
	}
	var got snapshot.Snapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("staged snapshot.json does not parse: %v", err)
	}
	if got.Target.DistroID != "debian" || got.Target.Codename != "bookworm" {
		t.Errorf("staged snapshot = %+v", got.Target)
	}

	t.Run("missing files dir is reported clearly", func(t *testing.T) {
		_, _, err := containerStageSnapshotDir(work, snap, filepath.Join(work, "does-not-exist"))
		if err == nil || dferr.ClassOf(err) != dferr.Usage {
			t.Fatalf("got %v, want a Usage error", err)
		}
	})
}

// --- staged path rewriting (ADR-013) -------------------------------------

// TestContainerRewriteStagedPaths covers the ADR-013 inter-process contract:
// StagedPath must end up host-readable, never a leftover container path.
func TestContainerRewriteStagedPaths(t *testing.T) {
	plan := &resolve.Plan{
		Selections: []resolve.Selection{
			{Name: "jq", Filename: "jq_1.6-2.1_amd64.deb", Reason: lock.ReasonRequested, StagedPath: "/archives/jq_1.6-2.1_amd64.deb"},
			{Name: "tree", Filename: "tree_2.1.0_amd64.deb", Reason: resolve.DependencyOf("jq"), StagedPath: "/archives/tree_2.1.0_amd64.deb"},
			{Name: "vendor-tool", Filename: "vendor-tool_1.0_amd64.deb", Reason: lock.ReasonExternal, StagedPath: "/external/vendor-tool_1.0_amd64.deb"},
			{Name: "no-filename", Filename: "", StagedPath: "/archives/should-not-be-touched"},
		},
	}
	if err := containerRewriteStagedPaths(plan, `d:\work\archives`, `d:\vendor\staging`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		filepath.Join(`d:\work\archives`, "jq_1.6-2.1_amd64.deb"),
		filepath.Join(`d:\work\archives`, "tree_2.1.0_amd64.deb"),
		filepath.Join(`d:\vendor\staging`, "vendor-tool_1.0_amd64.deb"),
		"/archives/should-not-be-touched", // no Filename: left alone
	}
	for i, w := range want {
		if got := plan.Selections[i].StagedPath; got != w {
			t.Errorf("Selections[%d] (%s) StagedPath = %q, want %q", i, plan.Selections[i].Name, got, w)
		}
	}
}

// --- store placement (E5) ------------------------------------------------

// TestContainerStoreVolumeChoice covers the E5 flip point: Windows and macOS
// default to the named volume docs/experiments/E5-container-io.md
// recommends (measured on Windows: ~61x faster write, ~8x faster read than a
// bind mount to a native Windows path; macOS inferred same direction but not
// measured there); native Linux keeps the bind-mount default since it has no
// host<->VM boundary to pay for. An explicit ContainerOptions.StoreVolume
// always wins regardless of platform.
func TestContainerStoreVolumeChoice(t *testing.T) {
	t.Run("explicit override always wins", func(t *testing.T) {
		b := &containerBackend{opts: ContainerOptions{StoreVolume: "my-operator-volume"}}
		vol, use := b.storeVolumeChoice()
		if !use || vol != "my-operator-volume" {
			t.Fatalf("got (%q, %v), want (\"my-operator-volume\", true)", vol, use)
		}
	})

	t.Run("default follows current platform per E5", func(t *testing.T) {
		b := &containerBackend{}
		vol, use := b.storeVolumeChoice()
		switch runtime.GOOS {
		case "windows", "darwin":
			if !use || vol != containerDefaultStoreVolume {
				t.Errorf("on %s: got (%q, %v), want (%q, true) -- see docs/experiments/E5-container-io.md",
					runtime.GOOS, vol, use, containerDefaultStoreVolume)
			}
		default:
			if use {
				t.Errorf("on %s: got useVolume=true, want the bind-mount default (no host<->VM boundary to pay for)", runtime.GOOS)
			}
		}
	})
}

// --- runContainer / execContainer ---------------------------------------

func TestExecContainerMissingBinary(t *testing.T) {
	_, err := execContainer(context.Background(), filepath.Join(t.TempDir(), "no-such-runtime-binary"), []string{"version"}, nil)
	if err == nil {
		t.Fatal("expected an error running a nonexistent binary")
	}
	if dferr.ClassOf(err) != dferr.Environment {
		t.Errorf("class = %v, want Environment", dferr.ClassOf(err))
	}
}

// --- ClosedWorld (E3 upgrade-set fix) -------------------------------------
//
// These exercise containerBackend.ClosedWorld end to end with no docker or
// podman binary anywhere on the machine: containerLookPath (runtime
// detection) and execContainerFn (the one point that would otherwise shell
// out to docker/podman) are both faked, exactly as select_test.go's
// fakeVersionRunner and this file's own containerLookPath fakes already do
// for the rest of this package's container tests, and as closedworld_test.go's
// fakeCWRunner does for the local backend's identical E3 scenario. The lock
// fixtures, package names and versions below are deliberately the same ones
// TestClosedWorld_UpgradeDivergence_Fails (closedworld_test.go) uses, so both
// backends are proven against the same evidence
// (docs/experiments/E3-pin-fidelity.md).

// containerClosedWorldFixture is what every test below needs before it ever
// reaches execContainerFn: a fake runtime on PATH, a valid static ELF
// SelfPath binary, and a snapshot naming a release core/distro resolves
// (debian/12/bookworm, matching TestContainerResolveImage) without any
// network access.
type containerClosedWorldFixture struct {
	workDir, filesDir, selfPath string
	snap                        *snapshot.Snapshot
}

func setupContainerClosedWorldFixture(t *testing.T) containerClosedWorldFixture {
	t.Helper()
	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Target:        snapshot.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
	}
	return setupContainerClosedWorldFixtureFor(t, t.TempDir(), snap)
}

// setupContainerClosedWorldFixtureWithDpkgStatus is setupContainerClosedWorldFixture
// with a real captured dpkg status under the returned filesDir, for tests of
// containerVersionMismatches' second verification layer
// (containerInstalledVersions' host-side cross-check). status is raw deb822
// content; see containerSnapshotWithDpkgStatus for the on-disk layout, which
// matches local_test.go's minimalSnapshotWithHolds convention exactly so
// this reads the same shape BuildPrivateRoot itself would.
func setupContainerClosedWorldFixtureWithDpkgStatus(t *testing.T, status string) containerClosedWorldFixture {
	t.Helper()
	snap, filesDir := containerSnapshotWithDpkgStatus(t, status)
	return setupContainerClosedWorldFixtureFor(t, filesDir, snap)
}

func setupContainerClosedWorldFixtureFor(t *testing.T, filesDir string, snap *snapshot.Snapshot) containerClosedWorldFixture {
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

	return containerClosedWorldFixture{
		workDir:  t.TempDir(),
		filesDir: filesDir,
		selfPath: writeTempFile(t, "debark-linux-amd64", buildELF64(t, elf.EM_X86_64, elf.ELFDATA2LSB, false)),
		snap:     snap,
	}
}

// containerSnapshotWithDpkgStatus builds a snapshot.Snapshot naming a real
// dpkg status file under a fresh directory, mirroring local_test.go's
// minimalSnapshotWithHolds fixture layout (same ArchivePath shape, same
// on-disk path) so containerInstalledVersions reads it exactly the way
// BuildPrivateRoot would for a real private root. status == "" means "no
// captured dpkg status at all" (DpkgStatus left zero-valued).
func containerSnapshotWithDpkgStatus(t *testing.T, status string) (*snapshot.Snapshot, string) {
	t.Helper()
	filesDir := t.TempDir()
	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Target:        snapshot.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
	}
	if status == "" {
		return snap, filesDir
	}
	const archivePath = "var/lib/dpkg/status"
	dst := filepath.Join(filesDir, filepath.FromSlash(archivePath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(status)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	snap.DpkgStatus = snapshot.File{Path: "/var/lib/dpkg/status", ArchivePath: archivePath, Size: int64(len(data))}
	return snap, filesDir
}

// fakeContainerPlanRun builds an execContainerFn replacement that captures
// the argv it is called with into *argvOut, writes env to workDir/plan.json
// -- what a real `debark resolve --plan-out /work/plan.json` running
// inside the container would leave for the host to read back -- and reports
// exit 0.
func fakeContainerPlanRun(t *testing.T, workDir string, env containerEnvelope, argvOut *[]string) func(context.Context, string, []string, func(evidence.Event)) (containerRunResult, error) {
	t.Helper()
	return func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		*argvOut = argv
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal fake envelope: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "plan.json"), data, 0o644); err != nil {
			t.Fatalf("write fake plan.json: %v", err)
		}
		return containerRunResult{Argv: append([]string{runtimePath}, argv...), ExitCode: 0}, nil
	}
}

// TestContainerClosedWorld_UpgradeDivergence_Fails is the container
// backend's twin of TestClosedWorld_UpgradeDivergence_Fails
// (closedworld_test.go): before this fix, ClosedWorld passed Upgrades
// straight through to the inner `debark resolve --upgrades` (a bare,
// unpinned full-upgrade) and only checked Unresolved and the exit code, so
// this exact divergence -- reproduced here by faking the inner resolve's
// result, in the shape E3's own bare-name-against-an-accumulated-bundle
// resolution actually produced -- reported ClosedWorldOK. This test proves
// it now reports ClosedWorldFailed with a Detail naming the package and both
// versions, and that the fix never passes --upgrades to get there.
func TestContainerClosedWorld_UpgradeDivergence_Fails(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			// The online solve's own full-upgrade pass locked this package's
			// upgrade target at 1.0-1 (E3's own scenario: a target pin
			// favouring the lower-numbered version).
			{Name: "debark-e3-demo", Arch: "all", Version: "1.0-1", Reason: lock.ReasonUpgrade},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Selections: []resolve.Selection{
				{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
				// The (faked) inner resolve's bare-name resolution against
				// the bundle picks 2.0-1 -- E3's own recorded divergence
				// (hack/experiments/out/e3/log-07c-bundleB-fullupgrade.txt),
				// reproduced through the container path.
				{Name: "debark-e3-demo", Arch: "all", Version: "2.0-1", Reason: lock.ReasonExternal},
			},
		},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		Upgrades:         true,
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"debark-e3-demo:all", "1.0-1", "2.0-1"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q (say what diverged)", cw.Detail, want)
		}
	}
	if cw.CommandDigest == "" || cw.OutputDigest == "" {
		t.Errorf("ClosedWorld missing digests even on failure: %+v", cw)
	}
	if containsArg(argv, "--upgrades") {
		t.Errorf("container ClosedWorld must never pass --upgrades to the inner resolve (E3/ADR-007): argv=%v", argv)
	}
	if !containsArg(argv, "debark-e3-demo:all") {
		t.Errorf("expected --external-name debark-e3-demo:all requesting the locked upgrade target from the bundle; argv=%v", argv)
	}
}

// TestContainerClosedWorld_UpgradeMatchesLock_OK is the companion
// true-negative: when the (faked) inner resolve's result matches exactly
// what the lock recorded, the check still reports ClosedWorldOK -- this fix
// must not turn every --upgrade container build into a false failure, and it
// must never fall back to a bare --upgrades call anywhere in the process.
func TestContainerClosedWorld_UpgradeMatchesLock_OK(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			{Name: "debark-e3-demo", Arch: "all", Version: "2.0-1", Reason: lock.ReasonUpgrade},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Selections: []resolve.Selection{
				{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
				{Name: "debark-e3-demo", Arch: "all", Version: "2.0-1", Reason: lock.ReasonExternal},
			},
		},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		Upgrades:         true,
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldOK, cw.Detail)
	}
	if containsArg(argv, "--upgrades") {
		t.Errorf("container ClosedWorld must never pass --upgrades to the inner resolve (E3/ADR-007): argv=%v", argv)
	}
	if !containsArg(argv, "debark-e3-demo:all") {
		t.Errorf("expected --external-name debark-e3-demo:all requesting the locked upgrade target from the bundle; argv=%v", argv)
	}
}

// TestContainerClosedWorld_NoUpgradeReasonPackages_OK: Upgrades is requested
// but the lock recorded nothing with Reason "upgrade" -- ClosedWorld must
// not invent an --external-name for one, and must not fail the build over
// it either.
func TestContainerClosedWorld_NoUpgradeReasonPackages_OK(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Selections: []resolve.Selection{
				{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			},
		},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		Upgrades:         true,
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldOK, cw.Detail)
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "debark-e3-demo") {
			t.Errorf("nothing in the lock has Reason upgrade: must not invent an --external-name entry; argv=%v", argv)
		}
	}
}

// TestContainerClosedWorld_MissingLock_Errors: Upgrades is requested but the
// bundle has no lock.json next to its repo/ directory -- a hard error, not a
// silent "nothing to upgrade": the check cannot prove what it could not
// read, and it must fail before ever starting a container.
func TestContainerClosedWorld_MissingLock_Errors(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ranContainer := false
	execContainerFn = func(context.Context, string, []string, func(evidence.Event)) (containerRunResult, error) {
		ranContainer = true
		return containerRunResult{}, nil
	}

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	_, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		// Install must be non-empty to reach the lock read at all: an empty
		// install set is now ClosedWorldSkipped before any of this, matching
		// localBackend.ClosedWorld (closedworld.go). See
		// TestContainerClosedWorld_EmptyInstallSetIsSkippedNotOK.
		Install:  []string{"demo-app:amd64=1.0-1"},
		Upgrades: true,
		WorkDir:  fx.workDir,
	})
	if err == nil {
		t.Fatal("expected an error when the bundle has no lock.json to read the upgrade set from")
	}
	if ranContainer {
		t.Error("must fail before ever running a container when the upgrade set cannot be determined")
	}
}

// TestContainerUpgradeMismatches exercises the adapter that lets
// ClosedWorld's version comparison share closedworld.go's own
// upgradeMismatches rather than re-implementing it (the contract brief: "one
// implementation of the comparison").
func TestContainerUpgradeMismatches(t *testing.T) {
	want := map[string]string{
		"demo-app:amd64":     "1.0-1",
		"debark-e3-demo:all": "1.0-1",
	}
	sels := []resolve.Selection{
		{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		{Name: "debark-e3-demo", Arch: "all", Version: "2.0-1", Reason: lock.ReasonExternal},
		// Not in want at all: containerUpgradeMismatches, like
		// upgradeMismatches itself, only judges the packages it was asked
		// about.
		{Name: "untracked-pkg", Arch: "amd64", Version: "5.0", Reason: lock.ReasonRequested},
	}
	got := containerUpgradeMismatches(want, sels)
	if len(got) != 1 {
		t.Fatalf("mismatches = %v, want exactly 1", got)
	}
	for _, want := range []string{"debark-e3-demo:all", "1.0-1", "2.0-1"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("mismatch message %q, want it to mention %q", got[0], want)
		}
	}
}

func TestContainerUpgradeMismatches_MatchingVersionIsNotAMismatch(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "2.0-1"}
	sels := []resolve.Selection{{Name: "demo-app", Arch: "amd64", Version: "2.0-1"}}
	if got := containerUpgradeMismatches(want, sels); len(got) != 0 {
		t.Errorf("mismatches = %v, want none: the resolved version matches the lock exactly", got)
	}
}

func TestContainerUpgradeMismatches_AbsentSelectionIsNotAMismatch(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	if got := containerUpgradeMismatches(want, nil); len(got) != 0 {
		t.Errorf("mismatches = %v, want none: no Selection at all is not a divergence here", got)
	}
}

// --- ClosedWorld's install-set extension --------------------------------
//
// The coordinator flagged that the E3 fix above only reached in.Upgrades'
// Reason: "upgrade" entries -- the ordinary install set (the bulk of a
// typical bundle) still only proved bare-name resolvability, not exact
// version. These extend the same fix to in.Install, and specifically cover
// the case containerUpgradeMismatches alone cannot see at all: a want key
// the bare-name resolve touched not at all, which containerVersionMismatches
// now cross-checks against the target's own captured dpkg status
// (containerInstalledVersions) rather than silently trusting it.

// TestContainerClosedWorld_InstallSetDivergence_Fails is
// TestContainerClosedWorld_UpgradeDivergence_Fails' twin for the ordinary
// install set: before this fix's extension, a wrong version resolved for an
// in.Install entry (not an upgrade at all) was invisible to
// containerUpgradeMismatches because it was only ever compared against the
// upgrade-set want map.
func TestContainerClosedWorld_InstallSetDivergence_Fails(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Selections: []resolve.Selection{
				// The bundle-only, bare-name resolve picks 2.0-1 for an
				// ordinary requested package -- the same class of divergence
				// as E3's, just landing on the install set instead of the
				// upgrade set.
				{Name: "demo-app", Arch: "amd64", Version: "2.0-1", Reason: lock.ReasonRequested},
			},
		},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"demo-app:amd64", "1.0-1", "2.0-1"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", cw.Detail, want)
		}
	}
	if !containsArg(argv, "demo-app:amd64") {
		t.Errorf("expected --external-name demo-app:amd64 requesting the install-set package from the bundle; argv=%v", argv)
	}
}

// TestContainerClosedWorld_InstallSetMatchesLock_OK is the companion
// true-negative.
func TestContainerClosedWorld_InstallSetMatchesLock_OK(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{
			Selections: []resolve.Selection{
				{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			},
		},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldOK, cw.Detail)
	}
}

// TestContainerClosedWorld_SilentDivergence_DpkgStatusDiffers_Fails is the
// case containerUpgradeMismatches structurally cannot see on its own: the
// bare-name resolve proposes NO action at all for demo-app (no Selection),
// which would previously have passed silently. The target's own captured
// dpkg status (injected here, read host-side by containerInstalledVersions)
// shows a DIFFERENT version installed than the lock recorded, proving the
// resolve's silence reflected "already has *something*", not "already has
// what the lock wants" -- exactly the coordinator's "no candidate at all"
// case, distinguished by wording from a present-but-wrong-version mismatch.
func TestContainerClosedWorld_SilentDivergence_DpkgStatusDiffers_Fails(t *testing.T) {
	fx := setupContainerClosedWorldFixtureWithDpkgStatus(t,
		"Package: demo-app\nStatus: install ok installed\nVersion: 0.9-1\nArchitecture: amd64\n\n")
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan:          resolve.Plan{}, // no Selection for demo-app at all
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"demo-app:amd64", "1.0-1", "0.9-1", "proposed no change", "captured dpkg status"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q (an operator needs to know this was silent, not a wrong-version proposal)", cw.Detail, want)
		}
	}
}

// TestContainerClosedWorld_SilentButCorrect_DpkgStatusMatches_OK is the
// companion true-negative: the bare-name resolve again proposes nothing,
// but this time because the target's own captured dpkg status genuinely
// already has the exact locked version -- correct silence, not a divergence.
func TestContainerClosedWorld_SilentButCorrect_DpkgStatusMatches_OK(t *testing.T) {
	fx := setupContainerClosedWorldFixtureWithDpkgStatus(t,
		"Package: demo-app\nStatus: install ok installed\nVersion: 1.0-1\nArchitecture: amd64\n\n")
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan:          resolve.Plan{},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldOK {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldOK, cw.Detail)
	}
}

// TestContainerClosedWorld_SilentDivergence_NotInstalledAtAll_Fails: same
// silent-resolve shape, but the target has no captured dpkg status at all --
// the "no candidate" wording (distinct from "proposed no change ... shows
// %s installed instead" above) fires here.
func TestContainerClosedWorld_SilentDivergence_NotInstalledAtAll_Fails(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t) // no DpkgStatus captured
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan:          resolve.Plan{},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q; detail: %s", cw.Result, lock.ClosedWorldFailed, cw.Detail)
	}
	for _, want := range []string{"demo-app:amd64", "1.0-1", "found no candidate", "does not show it installed"} {
		if !strings.Contains(cw.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", cw.Detail, want)
		}
	}
}

// --- containerInstalledVersions (host-side dpkg status reader) ----------

func TestContainerInstalledVersions(t *testing.T) {
	filesDir := t.TempDir()
	data := []byte(
		"Package: foo\nStatus: install ok installed\nVersion: 1.0\nArchitecture: amd64\n\n" +
			"Package: bar\nStatus: hold ok installed\nVersion: 2.0\nArchitecture: amd64\n\n" +
			"Package: baz\nStatus: deinstall ok config-files\nVersion: 3.0\nArchitecture: amd64\n\n" +
			"Package: multi\nStatus: install ok installed\nVersion: 4.0\nArchitecture: i386\n\n",
	)
	dst := filepath.Join(filesDir, "var", "lib", "dpkg", "status")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{DpkgStatus: snapshot.File{ArchivePath: "var/lib/dpkg/status"}}

	got, err := containerInstalledVersions(snap, filesDir)
	if err != nil {
		t.Fatalf("containerInstalledVersions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(got), got)
	}
	for key, wantVer := range map[string]string{"foo:amd64": "1.0", "bar:amd64": "2.0", "multi:i386": "4.0"} {
		if got[key] != wantVer {
			t.Errorf("got[%q] = %q, want %q (full map: %v)", key, got[key], wantVer, got)
		}
	}
	if _, ok := got["baz:amd64"]; ok {
		t.Errorf("a deinstalled package must not be reported as installed: %v", got)
	}
}

func TestContainerInstalledVersions_NoDpkgStatus(t *testing.T) {
	got, err := containerInstalledVersions(&snapshot.Snapshot{}, t.TempDir())
	if err != nil || len(got) != 0 {
		t.Errorf("empty snapshot: got=%v err=%v, want empty map and no error", got, err)
	}
	got, err = containerInstalledVersions(nil, t.TempDir())
	if err != nil || len(got) != 0 {
		t.Errorf("nil snapshot: got=%v err=%v, want empty map and no error", got, err)
	}
}

func TestContainerInstalledVersions_UnreadableStatus_Errors(t *testing.T) {
	snap := &snapshot.Snapshot{DpkgStatus: snapshot.File{ArchivePath: "var/lib/dpkg/status"}}
	if _, err := containerInstalledVersions(snap, t.TempDir()); err == nil {
		t.Fatal("expected an error when a captured dpkg status is named but cannot be read")
	}
}

// --- containerVersionMismatches (the combined comparison) ----------------

func TestContainerVersionMismatches_SelectionPresentWrongVersion(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	sels := []resolve.Selection{{Name: "demo-app", Arch: "amd64", Version: "2.0-1"}}
	got := containerVersionMismatches(want, sels, nil)
	if len(got) != 1 || !strings.Contains(got[0], "1.0-1") || !strings.Contains(got[0], "2.0-1") {
		t.Fatalf("got %v, want exactly one mismatch naming both versions", got)
	}
}

func TestContainerVersionMismatches_NoSelection_InstalledDiffers(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	installed := map[string]string{"demo-app:amd64": "0.9-1"}
	got := containerVersionMismatches(want, nil, installed)
	if len(got) != 1 {
		t.Fatalf("got %v, want exactly 1", got)
	}
	for _, w := range []string{"demo-app:amd64", "1.0-1", "0.9-1", "proposed no change"} {
		if !strings.Contains(got[0], w) {
			t.Errorf("mismatch %q, want it to mention %q", got[0], w)
		}
	}
}

func TestContainerVersionMismatches_NoSelection_InstalledMatches_NotAMismatch(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	installed := map[string]string{"demo-app:amd64": "1.0-1"}
	if got := containerVersionMismatches(want, nil, installed); len(got) != 0 {
		t.Errorf("got %v, want none: the target's own recorded state already matches", got)
	}
}

func TestContainerVersionMismatches_NoSelection_NotInstalledAtAll(t *testing.T) {
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	got := containerVersionMismatches(want, nil, nil)
	if len(got) != 1 {
		t.Fatalf("got %v, want exactly 1", got)
	}
	for _, w := range []string{"demo-app:amd64", "1.0-1", "found no candidate", "does not show it installed"} {
		if !strings.Contains(got[0], w) {
			t.Errorf("mismatch %q, want it to mention %q", got[0], w)
		}
	}
}

func TestContainerVersionMismatches_TouchedTakesPriorityOverInstalled(t *testing.T) {
	// A key present in Selections is judged by containerUpgradeMismatches
	// alone, even if installed also has an (irrelevant, stale) entry for it.
	want := map[string]string{"demo-app:amd64": "1.0-1"}
	sels := []resolve.Selection{{Name: "demo-app", Arch: "amd64", Version: "1.0-1"}}
	installed := map[string]string{"demo-app:amd64": "0.5-1"} // stale/irrelevant
	if got := containerVersionMismatches(want, sels, installed); len(got) != 0 {
		t.Errorf("got %v, want none: the Selection's version matches, installed must not be consulted", got)
	}
}

// --- ClosedWorld's recorded digests must not carry host paths --------------

// closedWorldFakeRun is an execContainerFn replacement for the digest tests
// below. Unlike fakeContainerPlanRun it deliberately emits OUTPUT containing
// the very host paths this run was given, in two of the spellings
// maskClosedWorldPaths handles: the native path, and the name apt gives that
// path's cached index under Dir::State::lists.
//
// That second one is not hypothetical decoration. closedworld.go found it on
// the ordinary success path of a stock Debian 12 builder, in a "Download is
// performed unsandboxed" warning naming
// lists/partial/_work_bundle_repo_._InRelease, and it is precisely the
// spelling a hand-rolled masking pass in container.go would have missed --
// the more so since apt's encoding is not "replace / with _" but
// libapt-pkg's URItoFileName, which percent-encodes '_' itself. It is built
// here with the package's own aptURItoFileName(FileURI(...)) for the same
// reason container.go reuses the masking rather than reimplementing it:
// closedworld_test.go's own fixture does exactly this, and a test that
// generates its fixture with its own copy of the rule is its own oracle --
// which is how the first version of that encoding shipped wrong.
func closedWorldFakeListFileName(repoDir string) string {
	return aptURItoFileName(FileURI(repoDir))
}

func closedWorldFakeRun(t *testing.T, fx containerClosedWorldFixture, repoDir string, env containerEnvelope, exitCode int, extraOutput string) func(context.Context, string, []string, func(evidence.Event)) (containerRunResult, error) {
	t.Helper()
	return func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal fake envelope: %v", err)
		}
		if err := os.WriteFile(filepath.Join(fx.workDir, "plan.json"), data, 0o644); err != nil {
			t.Fatalf("write fake plan.json: %v", err)
		}
		out := "mounting " + fx.selfPath + " at /debark\n" +
			"W: Download is performed unsandboxed as root as file '/var/lib/apt/lists/partial/" +
			closedWorldFakeListFileName(repoDir) + "_._InRelease' couldn't be accessed by user '_apt'.\n" +
			"work dir " + fx.workDir + "\n" + extraOutput
		return containerRunResult{
			Argv:     append([]string{runtimePath}, argv...),
			Stdout:   []byte(out),
			ExitCode: exitCode,
		}, nil
	}
}

// runContainerClosedWorldInFreshDirs runs one complete ClosedWorld against a
// wholly fresh set of host paths -- a new WorkDir, a new bundle directory, a
// new SelfPath binary and a different container-runtime exec path -- with
// everything about the REQUEST held identical. Two calls differ in nothing an
// operator asked for.
func runContainerClosedWorldInFreshDirs(t *testing.T, dockerPath string, exitCode int, extraOutput string) lock.ClosedWorld {
	t.Helper()
	fx := setupContainerClosedWorldFixture(t)
	containerLookPath = func(name string) (string, error) {
		if name == "docker" {
			return dockerPath, nil
		}
		return "", errors.New("not found")
	}
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages: []lock.Package{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		},
	}
	repoDir := writeClosedWorldBundle(t, lk)
	execContainerFn = closedWorldFakeRun(t, fx, repoDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		}},
	}, exitCode, extraOutput)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	return cw
}

// TestContainerClosedWorld_DigestsDoNotDependOnHostPaths is the container
// backend's twin of the determinism finding closedworld.go already documents
// for the local backend, and it is the one that matters most in practice:
// the container backend serves every macOS build, every Windows build and
// every cross-release build.
//
// Before the fix, ClosedWorld recorded
//
//	cmdDigest := digest.Bytes([]byte(strings.Join(res.Argv, "\x00")))
//	outDigest := digest.Bytes(res.combined())
//
// over the raw `docker run` argv -- the absolute path to this machine's
// docker/podman binary, plus -v mount specs naming WorkDir (a fresh
// os.MkdirTemp "debark-build-*" every run), BundleRepoDir, SnapshotFilesDir
// and SelfPath -- and over apt's unmasked output. Measured: two runs with
// identical inputs in two temp directories gave CommandDigest abf7d38d... vs
// 8debae9a.... CommandDigest reaches lock.ClosedWorld -> lock.json ->
// LockDigest -> manifest.NewBundleID -> the signature, so design 3.13's
// byte-identical-rebuild claim was false for every container build. The
// engine's scrubHostPaths cannot rescue it: it rewrites strings in the lock,
// and by then these are already SHA-256s.
func TestContainerClosedWorld_DigestsDoNotDependOnHostPaths(t *testing.T) {
	first := runContainerClosedWorldInFreshDirs(t, "/usr/bin/docker", 0, "")
	second := runContainerClosedWorldInFreshDirs(t, `C:\Program Files\Docker\resources\bin\docker.exe`, 0, "")

	if first.Result != lock.ClosedWorldOK || second.Result != lock.ClosedWorldOK {
		t.Fatalf("both runs should be ok: %q / %q (details %q / %q)", first.Result, second.Result, first.Detail, second.Detail)
	}
	if first.CommandDigest == "" || first.OutputDigest == "" {
		t.Fatalf("missing digests: %+v", first)
	}
	if first.CommandDigest != second.CommandDigest {
		t.Errorf("CommandDigest = %s then %s for two runs of the identical request in different directories: "+
			"the recorded argv still carries host paths, so every container build gets a different bundle id",
			first.CommandDigest, second.CommandDigest)
	}
	if first.OutputDigest != second.OutputDigest {
		t.Errorf("OutputDigest = %s then %s for two runs of the identical request in different directories: "+
			"the recorded output still carries host paths (including apt's list-file spelling of them)",
			first.OutputDigest, second.OutputDigest)
	}
}

// TestContainerClosedWorld_FailureDetailIsMasked covers the same fix on the
// failure branches, which is where it is easiest to lose: docker's own mount
// errors quote host paths straight back at you, and SelfPath is not one of
// the paths the engine's scrubHostPaths rewrites on the way out. Detail is a
// lock.json field like any other.
func TestContainerClosedWorld_FailureDetailIsMasked(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages:      []lock.Package{{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested}},
	}
	repoDir := writeClosedWorldBundle(t, lk)
	// The shape docker actually fails in: the host path, quoted back.
	execContainerFn = closedWorldFakeRun(t, fx, repoDir, containerEnvelope{SchemaVersion: containerEnvelopeSchema}, 125,
		"docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: "+fx.selfPath+"\n")

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q", cw.Result, lock.ClosedWorldFailed)
	}
	for _, host := range []string{fx.selfPath, fx.workDir, repoDir} {
		if strings.Contains(cw.Detail, host) || strings.Contains(cw.Detail, filepath.ToSlash(host)) {
			t.Errorf("Detail = %q still contains the host path %q; it is a lock.json field", cw.Detail, host)
		}
	}
	// Masked, not emptied: an auditor must still be able to read what failed.
	if !strings.Contains(cw.Detail, "bind source path does not exist") {
		t.Errorf("Detail = %q lost the diagnostic itself", cw.Detail)
	}
	if !strings.Contains(cw.Detail, "<SELF-BINARY>") {
		t.Errorf("Detail = %q, want the SelfPath placeholder in place of the host path", cw.Detail)
	}
}

// --- ClosedWorld must not report ok for what it never checked --------------

// TestContainerClosedWorld_EmptyInstallSetIsSkippedNotOK mirrors
// localBackend.ClosedWorld's own guard (closedworld.go: "install set is
// empty" -> ClosedWorldSkipped). The container backend returned
// ClosedWorldOK, and the engine reads the two completely differently:
// core/engine/finalize.go warns on skipped and records a
// closed-world.skipped warning precisely so a build cannot claim a check
// passed that never ran. A container build was the one path that could put
// "closed-world: ok" into a signed lock for an install set nothing was ever
// asked about.
func TestContainerClosedWorld_EmptyInstallSetIsSkippedNotOK(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	repoDir := writeClosedWorldBundle(t, &lock.Lock{SchemaVersion: lock.SchemaVersion})
	// A container run that succeeds and writes a perfectly valid, empty
	// envelope -- so that without the skip guard this method takes its
	// success path all the way to ClosedWorldOK. Nothing here must run, but
	// it has to be the shape that WOULD have produced the wrong answer, or
	// the test would pass for the wrong reason.
	ran := false
	execContainerFn = func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		ran = true
		data, merr := json.Marshal(containerEnvelope{SchemaVersion: containerEnvelopeSchema, Plan: resolve.Plan{}})
		if merr != nil {
			t.Fatal(merr)
		}
		if werr := os.WriteFile(filepath.Join(fx.workDir, "plan.json"), data, 0o644); werr != nil {
			t.Fatal(werr)
		}
		return containerRunResult{Argv: append([]string{runtimePath}, argv...), ExitCode: 0}, nil
	}

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          nil,
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldSkipped {
		t.Fatalf("Result = %q, want %q: with nothing to install there is nothing this check can prove, and "+
			"ok is a claim about a check that ran", cw.Result, lock.ClosedWorldSkipped)
	}
	if !strings.Contains(cw.Detail, "install set is empty") {
		t.Errorf("Detail = %q, want localBackend.ClosedWorld's own wording", cw.Detail)
	}
	if ran {
		t.Error("a skipped check must not start a container")
	}
}

// TestContainerClosedWorld_InstallEntryWithoutAVersionIsRefused: the parse of
// in.Install used to be `key, version, ok := strings.Cut(nv, "="); if ok &&
// key != ""`, which SILENTLY dropped any entry carrying no "=". A dropped
// entry never reached externalNames, so the inner resolve was never asked for
// it, so nothing ever checked it -- and ClosedWorld still returned
// ClosedWorldOK, certifying a bundle for an install entry it had not looked
// at. localBackend cannot have this defect: it passes in.Install verbatim to
// `apt-get -s install`, where a malformed entry is apt's error, loudly.
func TestContainerClosedWorld_InstallEntryWithoutAVersionIsRefused(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	repoDir := writeClosedWorldBundle(t, &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages:      []lock.Package{{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested}},
	})
	// A run that resolves the ONE well-formed entry perfectly. Without the
	// refusal, openssl:amd64 is dropped before it ever reaches externalNames,
	// so the inner resolve is never asked for it, nothing checks it, and this
	// method returns ClosedWorldOK -- certifying a bundle for an install
	// entry it never looked at. The fake has to be able to produce that "ok"
	// or the test would pass for the wrong reason.
	ran := false
	execContainerFn = func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		ran = true
		data, merr := json.Marshal(containerEnvelope{
			SchemaVersion: containerEnvelopeSchema,
			Plan: resolve.Plan{Selections: []resolve.Selection{
				{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
			}},
		})
		if merr != nil {
			t.Fatal(merr)
		}
		if werr := os.WriteFile(filepath.Join(fx.workDir, "plan.json"), data, 0o644); werr != nil {
			t.Fatal(werr)
		}
		return containerRunResult{Argv: append([]string{runtimePath}, argv...), ExitCode: 0}, nil
	}

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		// One well-formed entry and one with no version at all.
		Install: []string{"demo-app:amd64=1.0-1", "openssl:amd64"},
		WorkDir: fx.workDir,
	})
	if err == nil {
		t.Fatalf("ClosedWorld returned %q for an install entry it silently dropped and never checked", cw.Result)
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("dferr.ClassOf(err) = %v, want dferr.Usage: %v", got, err)
	}
	if !strings.Contains(err.Error(), "openssl:amd64") {
		t.Errorf("error %q does not name the entry it refused", err)
	}
	if ran {
		t.Error("must refuse before ever starting a container")
	}
}

// --- the network-URI scan, where isolation is only --network none ----------

// TestContainerClosedWorld_NetworkURIInOutputFails ports the scan
// localBackend.ClosedWorld has always had (closedworld.go: "a defence against
// a future change accidentally widening the sources"). It belongs here more
// than it does there, not less: the local check is closed BY CONSTRUCTION --
// its private root's only configured source is a file: URI -- whereas the
// inner `debark resolve --external-repo /bundlerepo` ADDS the bundle to the
// target's normal, still-online sources rather than replacing them, so the
// single thing between this check and the internet is the outer `docker run
// --network none`. Drop that flag and the check would keep passing while
// resolving against the network, and would certify the bundle self-contained
// on the strength of it.
func TestContainerClosedWorld_NetworkURIInOutputFails(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	repoDir := writeClosedWorldBundle(t, &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages:      []lock.Package{{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested}},
	})
	execContainerFn = closedWorldFakeRun(t, fx, repoDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		}},
	}, 0, "Get:1 https://deb.debian.org/debian bookworm/main amd64 demo-app amd64 1.0-1 [4242 B]\n")

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if cw.Result != lock.ClosedWorldFailed {
		t.Fatalf("Result = %q, want %q: the captured output names an https:// source, so the world was not closed",
			cw.Result, lock.ClosedWorldFailed)
	}
	if !strings.Contains(cw.Detail, "network URI") {
		t.Errorf("Detail = %q, want it to say a network URI was referenced", cw.Detail)
	}
}

// --- ClosedWorld's own --recommends, and its own stale envelope ------------

// TestContainerClosedWorld_PassesRecommendsTrueLikeLocal: ClosedWorldInput
// carries no Recommends field, so this method must choose, and it chooses
// what localBackend.ClosedWorld's RootSpec chooses (Recommends: true,
// closedworld.go) -- the two backends run the same check against the same
// bundle and must not resolve different dependency closures within it.
// container.go's comment claimed --recommends was "left unset", but
// containerBuildInnerArgv appended it unconditionally, so the zero value
// shipped "--recommends=false" on every container closed-world run: a
// narrower closure than local's, silently, while the comment said otherwise.
func TestContainerClosedWorld_PassesRecommendsTrueLikeLocal(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	repoDir := writeClosedWorldBundle(t, &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages:      []lock.Package{{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested}},
	})
	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		}},
	}, &argv)

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	if _, err := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	}); err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}
	if containsArg(argv, "--recommends=false") {
		t.Errorf("inner argv ships --recommends=false, which nobody chose and which localBackend.ClosedWorld does "+
			"not use (closedworld.go uses Recommends: true): argv=%v", argv)
	}
	if !containsArg(argv, "--recommends=true") {
		t.Errorf("inner argv = %v, want --recommends=true to match localBackend.ClosedWorld's RootSpec", argv)
	}
}

// TestContainerClosedWorld_StalePlanEnvelopeIsNotReadBackAsThisRun is the
// Resolve-side stale-envelope defect on the check that certifies the bundle:
// ClosedWorld reads <WorkDir>/plan.json too, and removed no pre-existing one,
// so a leftover file plus a container that exits 0 writing nothing certified
// this bundle against a previous run's plan.
func TestContainerClosedWorld_StalePlanEnvelopeIsNotReadBackAsThisRun(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	repoDir := writeClosedWorldBundle(t, &lock.Lock{
		SchemaVersion: lock.SchemaVersion,
		Packages:      []lock.Package{{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested}},
	})
	stale, err := json.Marshal(containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.workDir, "plan.json"), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	// Exits 0, writes nothing: everything this check knows would have to come
	// from the file already sitting there.
	execContainerFn = func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		return containerRunResult{Argv: append([]string{runtimePath}, argv...), ExitCode: 0}, nil
	}

	backend := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	cw, cerr := backend.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		BundleRepoDir:    repoDir,
		Install:          []string{"demo-app:amd64=1.0-1"},
		WorkDir:          fx.workDir,
	})
	if cerr != nil {
		t.Fatalf("ClosedWorld: %v", cerr)
	}
	if cw.Result == lock.ClosedWorldOK {
		t.Fatalf("Result = ok for a run that wrote no envelope: the check passed on a previous run's plan.json")
	}
	if !strings.Contains(cw.Detail, "no plan envelope written") {
		t.Errorf("Detail = %q, want it to say this run wrote no envelope", cw.Detail)
	}
}

// --- naming the Linux binary the container mounts --------------------------

func TestContainerSelfPathPrefersExplicitOption(t *testing.T) {
	got, err := containerSelfPath(ContainerOptions{SelfPath: "/explicit/debark"}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/debark" {
		t.Errorf("containerSelfPath = %q, want the explicit SelfPath", got)
	}
}

// With no explicit answer, a linux/<arch> build sitting beside the running
// executable is used. This is what makes `build --backend container` work on
// Windows and macOS without a flag on every command line, once the operator
// has built the binary the error message told them to build.
func TestContainerSiblingSelfPathFindsBothSpellings(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("bin", "debark-linux-arm64"),
		"debark-linux-arm64",
	} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "debark.exe")
			cand := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(cand), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cand, []byte("\x7fELF"), 0o755); err != nil {
				t.Fatal(err)
			}
			got, ok := containerSiblingSelfPath(exe, "arm64")
			if !ok {
				t.Fatalf("no sibling found for %s", rel)
			}
			if got != cand {
				t.Errorf("containerSiblingSelfPath = %q, want %q", got, cand)
			}
		})
	}
}

// bin/ wins, because that is the spelling the error message, the
// --embed-binary bundle layout and the documentation all use.
func TestContainerSiblingSelfPathPrefersBinSubdirectory(t *testing.T) {
	dir := t.TempDir()
	inBin := filepath.Join(dir, "bin", "debark-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(inBin), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{inBin, filepath.Join(dir, "debark-linux-amd64")} {
		if err := os.WriteFile(p, []byte("\x7fELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := containerSiblingSelfPath(filepath.Join(dir, "debark"), "amd64")
	if !ok || got != inBin {
		t.Errorf("containerSiblingSelfPath = %q (%v), want %q", got, ok, inBin)
	}
}

func TestContainerSiblingSelfPathIsArchitectureSpecific(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "debark-linux-amd64"), []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := containerSiblingSelfPath(filepath.Join(dir, "debark"), "arm64"); ok {
		t.Errorf("an amd64 sibling must not answer an arm64 request; got %q", got)
	}
	// A directory of the right name is not a binary.
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, "debark-linux-amd64"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := containerSiblingSelfPath(filepath.Join(dir2, "debark"), "amd64"); ok {
		t.Errorf("a directory must not be taken for a binary; got %q", got)
	}
	if _, ok := containerSiblingSelfPath("", "amd64"); ok {
		t.Error("an empty executable path must find nothing")
	}
	if _, ok := containerSiblingSelfPath(filepath.Join(dir, "debark"), ""); ok {
		t.Error("an empty architecture must find nothing")
	}
}

// The hint an operator actually reads when the mounted binary is refused
// must name things typeable on a command line. It used to name a Go struct
// field, ContainerOptions.SelfPath, which no CLI user can set.
func TestContainerSelfPathHintNamesOperatorControls(t *testing.T) {
	hint := containerSelfPathHint("amd64")
	for _, want := range []string{
		"--self-binary",
		"DEBARK_SELF_BINARY",
		"self_binary",
		"bin/debark-linux-amd64",
		"GOOS=linux GOARCH=amd64",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint does not mention %q:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "ContainerOptions") {
		t.Errorf("hint still names a Go struct field:\n%s", hint)
	}
}

func TestContainerValidateBinaryHintReachesTheOperator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debark.exe")
	if err := os.WriteFile(path, []byte("MZ not an ELF binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := containerValidateBinary(path, "amd64")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if h := dferr.HintOf(err); !strings.Contains(h, "--self-binary") {
		t.Errorf("the refusal's hint must say how to fix it; got %q", h)
	}
}
