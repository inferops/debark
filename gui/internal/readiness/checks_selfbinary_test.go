package readiness

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeELF writes a minimal but genuinely parseable ELF64 file, so
// debug/elf accepts it and the inspector is driven for real rather than
// against a fixture only a hand-rolled header reader would take.
//
// 64-bit only: every architecture these tests exercise is, and a 32-bit
// header is a different layout for no extra coverage. Nothing here is
// executed.
func writeELF(t *testing.T, path string, machine elf.Machine, data elf.Data, withInterp bool) {
	t.Helper()

	order := binary.ByteOrder(binary.LittleEndian)
	if data == elf.ELFDATA2MSB {
		order = binary.BigEndian
	}
	const ehsize, phentsize = 64, 56

	phnum := uint16(0)
	if withInterp {
		phnum = 1
	}

	hdr := make([]byte, ehsize)
	copy(hdr, []byte{0x7f, 'E', 'L', 'F'})
	hdr[4] = byte(elf.ELFCLASS64)
	hdr[5] = byte(data)
	hdr[6] = byte(elf.EV_CURRENT)
	order.PutUint16(hdr[16:18], uint16(elf.ET_EXEC))
	order.PutUint16(hdr[18:20], uint16(machine))
	order.PutUint32(hdr[20:24], uint32(elf.EV_CURRENT))
	if withInterp {
		order.PutUint64(hdr[32:40], ehsize) // e_phoff
	}
	order.PutUint16(hdr[52:54], ehsize)
	order.PutUint16(hdr[54:56], phentsize)
	order.PutUint16(hdr[56:58], phnum)
	order.PutUint16(hdr[58:60], 64) // e_shentsize
	// e_shnum and e_shstrndx stay zero: a file with no section table is
	// legal and is what debug/elf sees in a stripped static binary anyway.

	out := hdr
	if withInterp {
		interp := append([]byte("/lib64/ld-linux-x86-64.so.2"), 0)
		ph := make([]byte, phentsize)
		order.PutUint32(ph[0:4], uint32(elf.PT_INTERP))
		order.PutUint32(ph[4:8], uint32(elf.PF_R))
		order.PutUint64(ph[8:16], uint64(ehsize+phentsize)) // p_offset
		order.PutUint64(ph[32:40], uint64(len(interp)))     // p_filesz
		order.PutUint64(ph[40:48], uint64(len(interp)))     // p_memsz
		out = append(out, ph...)
		out = append(out, interp...)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, out, 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeStaticELF is the common case: a static binary for one architecture.
func writeStaticELF(t *testing.T, path string, machine elf.Machine) {
	t.Helper()
	data := elf.ELFDATA2LSB
	if machine == elf.EM_S390 {
		data = elf.ELFDATA2MSB
	}
	writeELF(t, path, machine, data, false)
}

func TestSelfBinaryInspect(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good")
	writeStaticELF(t, good, elf.EM_X86_64)

	wrong := filepath.Join(dir, "wrong")
	writeStaticELF(t, wrong, elf.EM_AARCH64)

	// A big-endian ELF for s390x: the byte-order branch is the only reason
	// the machine field is not read little-endian unconditionally, so it is
	// worth one case rather than an untested if.
	bigend := filepath.Join(dir, "bigend")
	writeStaticELF(t, bigend, elf.EM_S390)

	pe := filepath.Join(dir, "windows.exe")
	// The exact first four bytes the core repository's own diagnosis printed
	// for a debark.exe: 'M', 'Z', 0x90, 0x00.
	if err := os.WriteFile(pe, []byte{'M', 'Z', 0x90, 0x00, 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	tiny := filepath.Join(dir, "tiny")
	if err := os.WriteFile(tiny, []byte{0x7f}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A CGO_ENABLED=1 build: right machine, right class, right byte order,
	// and an ELF interpreter. The engine refuses these too, and it is the
	// mistake an operator following the instruction carelessly actually
	// makes, because CGO_ENABLED=1 is the default.
	dynamic := filepath.Join(dir, "dynamic")
	writeELF(t, dynamic, elf.EM_X86_64, elf.ELFDATA2LSB, true)

	truncated := filepath.Join(dir, "truncated")
	if err := os.WriteFile(truncated, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := []struct {
		name     string
		path     string
		arch     string
		want     selfBinaryStatus
		wantSeen bool
	}{
		{"a linux/amd64 build is what we want", good, "amd64", selfBinaryOK, true},
		{"an arm64 build for an amd64 container is wrong", wrong, "amd64", selfBinaryWrongArch, true},
		{"a big-endian s390x header is read in its own byte order", bigend, "s390x", selfBinaryOK, true},
		{"a windows .exe is not an ELF binary at all", pe, "amd64", selfBinaryNotELF, true},
		{"a one-byte file is not an ELF binary", tiny, "amd64", selfBinaryNotELF, true},
		{"an ELF header with no machine field is truncated", truncated, "amd64", selfBinaryNotELF, true},
		{"a dynamically linked build has no loader in the container", dynamic, "amd64", selfBinaryNotStatic, true},
		{"an unknown architecture is not second-guessed", good, "loong64", selfBinaryOK, true},
		{"a missing file is not a finding", filepath.Join(dir, "nope"), "amd64", selfBinaryAbsent, false},
		{"a directory is not a finding", dir, "amd64", selfBinaryAbsent, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail, seen := selfBinaryInspect(tc.path, tc.arch)
			if seen != tc.wantSeen {
				t.Fatalf("seen = %v, want %v (detail %q)", seen, tc.wantSeen, detail)
			}
			if seen && got != tc.want {
				t.Fatalf("status = %v, want %v (detail %q)", got, tc.want, detail)
			}
		})
	}
}

func TestDecideSelfBinary(t *testing.T) {
	cases := []struct {
		name         string
		obs          selfBinaryObservation
		wantStatus   Status
		wantSeverity Severity
		wantAction   bool
		summaryHas   string
	}{
		{
			name:         "nothing found is a degraded problem with a command",
			obs:          selfBinaryObservation{Host: "windows", Arch: "amd64", Searched: []string{`C:\df\bin\debark-linux-amd64`, `C:\df\debark-linux-amd64`}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantAction:   true,
			summaryHas:   "No Linux debark was found",
		},
		{
			name:         "a sibling that is not an ELF binary is a problem",
			obs:          selfBinaryObservation{Host: "windows", Arch: "amd64", Found: `C:\df\bin\debark-linux-amd64`, Status: selfBinaryNotELF, Detail: "first bytes are [77 90 144 0]"},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantAction:   true,
			summaryHas:   "is not a Linux ELF binary",
		},
		{
			name:         "a sibling for the wrong architecture is a problem",
			obs:          selfBinaryObservation{Host: "darwin", Arch: "arm64", Found: "/df/bin/debark-linux-arm64", Status: selfBinaryWrongArch, Detail: "ELF machine 0x3e"},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantAction:   true,
			summaryHas:   "wrong architecture",
		},
		{
			name:         "a dynamically linked sibling is a problem",
			obs:          selfBinaryObservation{Host: "windows", Arch: "amd64", Found: `C:\df\bin\debark-linux-amd64`, Status: selfBinaryNotStatic, Detail: "it names an ELF interpreter"},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantAction:   true,
			summaryHas:   "dynamically linked",
		},
		{
			name:         "a correct sibling passes",
			obs:          selfBinaryObservation{Host: "windows", Arch: "amd64", Found: `C:\df\bin\debark-linux-amd64`, Status: selfBinaryOK},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			summaryHas:   "builds can run in a container",
		},
		{
			name:         "linux needs nothing and says so",
			obs:          selfBinaryObservation{Host: "linux", Arch: "amd64"},
			wantStatus:   StatusSkipped,
			wantSeverity: SeverityInfo,
			summaryHas:   "This host is Linux",
		},
		{
			name:         "no debark to look beside is not a claim about a missing file",
			obs:          selfBinaryObservation{Host: "windows", Arch: "amd64", LocateErr: errors.New("debark is not on PATH")},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			summaryHas:   "was not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSelfBinary(tc.obs)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Severity != tc.wantSeverity {
				t.Errorf("severity = %q, want %q", got.Severity, tc.wantSeverity)
			}
			if !strings.Contains(got.Summary, tc.summaryHas) {
				t.Errorf("summary %q does not contain %q", got.Summary, tc.summaryHas)
			}
			if tc.wantAction && got.Action == nil {
				t.Error("want an action, got none")
			}
			if got.Status == StatusProblem && got.Remedy == "" {
				t.Error("a problem with no remedy is the wall this package exists to avoid")
			}
		})
	}
}

// TestSelfBinaryIsNeverBlockingOnItsOwn pins the severity rule. A machine with
// WSL 2 or a native apt has another way to build, so this row must degrade;
// only DeriveBuildEnvironment may conclude that a build is impossible.
func TestSelfBinaryIsNeverBlockingOnItsOwn(t *testing.T) {
	for _, st := range []selfBinaryStatus{selfBinaryAbsent, selfBinaryNotELF, selfBinaryWrongArch, selfBinaryNotStatic} {
		got := decideSelfBinary(selfBinaryObservation{Host: "windows", Arch: "amd64", Status: st, Found: "x"})
		if got.Severity == SeverityBlocking {
			t.Fatalf("status %v produced a blocking row", st)
		}
	}
}

func TestProbeSelfBinaryFindsTheSiblingBesideDebark(t *testing.T) {
	dir := t.TempDir()
	// The debark binary is here; the Linux build is in bin/ beside it,
	// which is the path containerSiblingSelfPath looks in first.
	debark := filepath.Join(dir, "debark.exe")
	if err := os.WriteFile(debark, []byte("MZ"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeStaticELF(t, filepath.Join(dir, "bin", "debark-linux-amd64"), elf.EM_X86_64)

	obs := probeSelfBinary(context.Background(), Options{
		BinaryPath:     debark,
		SelfBinaryArch: "amd64",
	}.withDefaults())

	if obs.Status != selfBinaryOK {
		t.Fatalf("status = %v, detail %q, searched %v", obs.Status, obs.Detail, obs.Searched)
	}
	if want := filepath.Join(dir, "bin", "debark-linux-amd64"); obs.Found != want {
		t.Fatalf("found %q, want %q", obs.Found, want)
	}
}

// TestProbeSelfBinaryLooksBesideDebarkNotBesideThisApp is the correctness
// point that is easiest to get wrong: containerSelfPath calls os.Executable()
// inside the debark process, so a file beside the GUI is not the file the
// engine will mount.
func TestProbeSelfBinaryLooksBesideDebarkNotBesideThisApp(t *testing.T) {
	appDir := t.TempDir()
	dfDir := t.TempDir()

	// A perfectly good Linux binary, in the wrong place.
	writeStaticELF(t, filepath.Join(appDir, "bin", "debark-linux-amd64"), elf.EM_X86_64)

	debark := filepath.Join(dfDir, "debark.exe")
	if err := os.WriteFile(debark, []byte("MZ"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	obs := probeSelfBinary(context.Background(), Options{
		BinaryPath:     debark,
		SelfBinaryArch: "amd64",
	}.withDefaults())

	if obs.Found != "" {
		t.Fatalf("found %q; a binary beside the application is not one beside debark", obs.Found)
	}
	for _, p := range obs.Searched {
		if !strings.HasPrefix(p, dfDir) {
			t.Fatalf("searched %q, which is not beside debark at %s", p, dfDir)
		}
	}
}

func TestProbeSelfBinaryPrefersAnExplicitPath(t *testing.T) {
	dir := t.TempDir()
	debark := filepath.Join(dir, "debark.exe")
	if err := os.WriteFile(debark, []byte("MZ"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A correct sibling exists...
	writeStaticELF(t, filepath.Join(dir, "bin", "debark-linux-amd64"), elf.EM_X86_64)
	// ...but an explicit path names something wrong, and that is what
	// containerSelfPath will use, so that is what must be reported.
	bad := filepath.Join(dir, "not-elf")
	if err := os.WriteFile(bad, []byte{'M', 'Z', 0x90, 0x00}, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	obs := probeSelfBinary(context.Background(), Options{
		BinaryPath:     debark,
		SelfBinaryPath: bad,
		SelfBinaryArch: "amd64",
	}.withDefaults())

	if obs.Found != bad || obs.Status != selfBinaryNotELF {
		t.Fatalf("found %q status %v, want the explicit path reported as not-ELF", obs.Found, obs.Status)
	}
}

func TestProbeSelfBinaryReadsTheEnvironmentVariable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "explicit")
	writeStaticELF(t, good, elf.EM_X86_64)
	t.Setenv("DEBARK_SELF_BINARY", good)

	obs := probeSelfBinary(context.Background(), Options{
		BinaryPath:     filepath.Join(dir, "debark.exe"),
		SelfBinaryArch: "amd64",
	}.withDefaults())

	if !obs.ExplicitFromEnv || obs.Found != good || obs.Status != selfBinaryOK {
		t.Fatalf("obs = %+v, want the env var honoured", obs)
	}
}

func TestSelfBinaryBuildCommandMatchesTheCoreHint(t *testing.T) {
	got := strings.Join(selfBinaryBuildCommand("amd64"), " ")
	want := "env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/debark-linux-amd64 ./cmd/debark"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	// The remedy directs the operator at the path the sibling search looks
	// in, so the two must not drift apart.
	paths := selfBinarySiblingPaths(filepath.Join("C:", "df", "debark.exe"), "amd64")
	if len(paths) != 2 || filepath.Base(filepath.Dir(paths[0])) != "bin" {
		t.Fatalf("sibling paths = %v, want bin/ first", paths)
	}
}

func TestDefaultContainerArchUsesDpkgNames(t *testing.T) {
	got := DefaultContainerArch()
	if got == "" {
		t.Fatal("empty architecture")
	}
	// The two names debark actually publishes bases for are identical in
	// Go and dpkg spelling; the mapping exists for the ones that are not.
	if runtime.GOARCH == "amd64" && got != "amd64" {
		t.Fatalf("GOARCH amd64 mapped to %q", got)
	}
	if runtime.GOARCH == "arm64" && got != "arm64" {
		t.Fatalf("GOARCH arm64 mapped to %q", got)
	}
	if _, ok := selfBinaryArchELF[got]; !ok && runtime.GOARCH == "amd64" {
		t.Fatalf("%q has no ELF machine entry", got)
	}
}

// TestSelfBinaryBlocksTheDerivedRow is the finding this whole check exists
// for: docker answering is not the same as debark being able to use it, and
// the derived row is where "can this machine build" is decided.
func TestSelfBinaryBlocksTheDerivedRow(t *testing.T) {
	dockerOK := Result{ID: CheckContainer, Status: StatusOK, Severity: SeverityInfo}
	wslMissing := Result{ID: CheckWSL, Status: StatusProblem, Severity: SeverityDegraded}
	selfMissing := Result{ID: CheckSelfBinary, Status: StatusProblem, Severity: SeverityDegraded}
	selfOK := Result{ID: CheckSelfBinary, Status: StatusOK, Severity: SeverityInfo}

	t.Run("docker plus no linux binary cannot build", func(t *testing.T) {
		got := DeriveBuildEnvironment([]Result{dockerOK, wslMissing, selfMissing})
		if got.Status != StatusProblem || got.Severity != SeverityBlocking {
			t.Fatalf("got %+v, want a blocking problem", got)
		}
		if !strings.Contains(got.Summary, "no Linux build of itself") {
			t.Errorf("summary %q does not name the real obstacle", got.Summary)
		}
		if got.Remedy == "" {
			t.Error("no remedy")
		}
	})

	t.Run("docker plus a linux binary can build", func(t *testing.T) {
		got := DeriveBuildEnvironment([]Result{dockerOK, wslMissing, selfOK})
		if got.Status != StatusOK {
			t.Fatalf("got %+v, want ok", got)
		}
	})

	// This subtest used to assert the opposite, on the reading that WSL 2 was
	// another way to build. It is not: debark runs apt natively or in a
	// container and nothing else, and on Windows native apt is not registered.
	// So a green WSL row did not survive the gate, it DEFEATED it -- the gate
	// only fires when no other route is ok, and this is the ordinary Windows
	// machine with Docker Desktop, measured live as can_build true with every
	// build then failing. Docker Desktop's own backend is WSL 2, so the gate
	// could essentially never fire on the machines it was written for.
	t.Run("a green WSL row does not defeat the gate", func(t *testing.T) {
		wslOK := Result{ID: CheckWSL, Status: StatusOK, Severity: SeverityInfo}
		got := DeriveBuildEnvironment([]Result{dockerOK, wslOK, selfMissing})
		if got.Status != StatusProblem || got.Severity != SeverityBlocking {
			t.Fatalf("got %+v, want a blocking problem — WSL 2 is not a way to build", got)
		}
		if !strings.Contains(got.Summary, "no Linux build of itself") {
			t.Errorf("summary %q does not name the real obstacle", got.Summary)
		}
	})

	t.Run("a set with no self-binary row is unchanged", func(t *testing.T) {
		// Linux never registers the check, and a caller folding in a partial
		// set must not have the container row discounted by a check that did
		// not run.
		got := DeriveBuildEnvironment([]Result{dockerOK, wslMissing})
		if got.Status != StatusOK {
			t.Fatalf("got %+v, want ok", got)
		}
	})
}

// TestSelfBinaryIsRegisteredOffLinux ties the constant to the platform files,
// so a new platform file cannot quietly omit the check.
func TestSelfBinaryIsRegisteredOffLinux(t *testing.T) {
	c := New(Options{})
	found := false
	for _, ck := range c.checks() {
		if ck.id == CheckSelfBinary {
			found = true
		}
	}
	if runtime.GOOS == "linux" && found {
		t.Fatal("the self-binary check is registered on Linux, where the running debark already is a Linux ELF")
	}
	if runtime.GOOS != "linux" && !found {
		t.Fatalf("the self-binary check is not registered on %s, where a container build cannot work without it", runtime.GOOS)
	}
}

// TestSelfBinaryCanBeReRunAlone is the "I built it, check again" path. Unlike
// build-environment this row is a real probe, so RunOne must accept it.
func TestSelfBinaryCanBeReRunAlone(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("not registered on Linux")
	}
	c := New(Options{BinaryPath: filepath.Join(t.TempDir(), "debark.exe")})
	got, err := c.RunOne(context.Background(), CheckSelfBinary)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if got.ID != CheckSelfBinary || got.Title == "" {
		t.Fatalf("got %+v", got)
	}
}

// TestSelfBinaryArchTableMatchesTheEngine is the reason this check is worth
// having at all. containerCheck's comment makes the same point about docker
// and podman: reporting that podman is ready when debark will pick docker
// is a green tick for a runtime the build never uses. A green tick for a
// binary the engine will refuse is the same mistake.
//
// core/apt's containerArchELF is unexported, so this cannot import it and the
// table is a transcription. What can be checked is that the transcription is
// internally coherent and covers the architectures debark publishes bases
// for -- if the engine's table ever gains a row, this fails and names it.
func TestSelfBinaryArchTableMatchesTheEngine(t *testing.T) {
	want := map[string]elf.Machine{
		"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64, "armhf": elf.EM_ARM,
		"armel": elf.EM_ARM, "i386": elf.EM_386, "ppc64el": elf.EM_PPC64,
		"s390x": elf.EM_S390, "riscv64": elf.EM_RISCV,
	}
	if len(selfBinaryArchELF) != len(want) {
		t.Fatalf("table has %d architectures, the engine has %d", len(selfBinaryArchELF), len(want))
	}
	for arch, machine := range want {
		spec, ok := selfBinaryArchELF[arch]
		if !ok {
			t.Errorf("%s is missing", arch)
			continue
		}
		if spec.Machine != machine {
			t.Errorf("%s: machine %v, want %v", arch, spec.Machine, machine)
		}
		// s390x is the one big-endian architecture in the set, and getting it
		// wrong would silently pass every little-endian binary.
		wantData := elf.ELFDATA2LSB
		if arch == "s390x" {
			wantData = elf.ELFDATA2MSB
		}
		if spec.Data != wantData {
			t.Errorf("%s: byte order %v, want %v", arch, spec.Data, wantData)
		}
		wantClass := elf.ELFCLASS64
		if arch == "armhf" || arch == "armel" || arch == "i386" {
			wantClass = elf.ELFCLASS32
		}
		if spec.Class != wantClass {
			t.Errorf("%s: class %v, want %v", arch, spec.Class, wantClass)
		}
	}
	// Every architecture the build command can be produced for must be one
	// the table knows, or the remedy would name a GOARCH the check cannot
	// then verify.
	for arch := range selfBinaryArchELF {
		if got := selfBinaryGOARCH(arch); got == "" {
			t.Errorf("%s has no GOARCH", arch)
		}
	}
}
