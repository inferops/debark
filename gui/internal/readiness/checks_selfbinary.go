package readiness

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ---------------------------------------------------------------------------
// the Linux debark the container backend mounts
// ---------------------------------------------------------------------------
//
// This check exists because of a measured end-to-end failure, and it is worth
// stating the failure before the code: on Windows with Docker Desktop running,
// this package reported CanBuild true with a green container row, and then
// every single build failed.
//
// The reason is architectural rather than a bad probe. debark's container
// backend does not run the container image's own debark — it mounts the
// debark binary the operator invoked into the container and re-execs it
// (core/apt/container.go, containerSelfPath). A Windows debark.exe is a PE
// binary, so the re-exec fails with "does not look like a Linux ELF binary"
// and exit 2 (dferr.Environment). Nothing about docker is wrong, which is
// exactly why "docker info" answering says nothing about it: the question is
// not "does docker work" but "can debark use docker from here".
//
// At the time that was found there was no answer an operator could act on —
// the fix named a Go struct field. The core repository has since added three
// routes to one, and this check follows the same order containerSelfPath does:
//
//  1. an explicit path: --self-binary, DEBARK_SELF_BINARY, or the
//     self_binary config key;
//  2. bin/debark-linux-<arch> or debark-linux-<arch> beside the debark
//     binary itself;
//  3. otherwise the running debark, which on a non-Linux host is never a
//     Linux ELF.
//
// So the check is a file test plus containerValidateBinary's own four checks,
// run through debug/elf: the ELF magic, then class, byte order and machine,
// then a program-header walk for a PT_INTERP that would mean the binary is
// dynamically linked. Its remedy is the exact cross-compile command that
// produces the file at the path step 2 already looks in — after which no flag
// is needed on any future build.
//
// Verified against binaries built from core 6549f6f on this Windows host:
// with a real CGO_ENABLED=0 linux/amd64 build at bin/debark-linux-amd64
// beside debark.exe the row is green and a container build reached apt and
// resolved a Debian 12 request; with a real linux/arm64 build under the same
// name the row names the machine mismatch; with --self-binary pointed back at
// the .exe, debark itself fails with the ELF diagnosis and exit 2.

// selfBinaryArchELF is the ELF header a static Linux debark for one dpkg
// architecture must carry.
//
// It is a deliberate transcription of core/apt/container_driver.go's
// containerArchELF, and the reason it is transcribed rather than imported is
// that the original is unexported. Agreeing with the engine is the whole
// value of this row: containerCheck's own comment makes the same point about
// docker and podman — "reporting that podman is ready when debark will pick
// docker would be a green tick for a runtime the build never uses" — and a
// green tick for a binary the engine will refuse is the same mistake.
//
// If this table and the engine's ever disagree, the engine is right and this
// file is the one to change.
var selfBinaryArchELF = map[string]struct {
	Machine elf.Machine
	Class   elf.Class
	Data    elf.Data
}{
	"amd64":   {elf.EM_X86_64, elf.ELFCLASS64, elf.ELFDATA2LSB},
	"arm64":   {elf.EM_AARCH64, elf.ELFCLASS64, elf.ELFDATA2LSB},
	"armhf":   {elf.EM_ARM, elf.ELFCLASS32, elf.ELFDATA2LSB},
	"armel":   {elf.EM_ARM, elf.ELFCLASS32, elf.ELFDATA2LSB},
	"i386":    {elf.EM_386, elf.ELFCLASS32, elf.ELFDATA2LSB},
	"ppc64el": {elf.EM_PPC64, elf.ELFCLASS64, elf.ELFDATA2LSB},
	"s390x":   {elf.EM_S390, elf.ELFCLASS64, elf.ELFDATA2MSB},
	"riscv64": {elf.EM_RISCV, elf.ELFCLASS64, elf.ELFDATA2LSB},
}

// selfBinaryStatus is what was found at one candidate path.
type selfBinaryStatus int

const (
	// selfBinaryAbsent means nothing is at the path.
	selfBinaryAbsent selfBinaryStatus = iota
	// selfBinaryNotELF means a file is there and it is not an ELF binary at
	// all — the Windows .exe case, and the case an operator hits by copying
	// the wrong file.
	selfBinaryNotELF
	// selfBinaryWrongArch means an ELF binary built for another machine,
	// word size or byte order.
	selfBinaryWrongArch
	// selfBinaryNotStatic means a Linux ELF binary for the right machine that
	// names an ELF interpreter, so it is dynamically linked. The engine
	// refuses these too: the container image is not guaranteed to hold the
	// loader and libc the binary would ask for. It is the failure a CGO_ENABLED=1
	// build produces, which is the default, so it is the mistake an operator
	// following the instruction carelessly actually makes.
	selfBinaryNotStatic
	// selfBinaryOK means a static ELF binary for the wanted architecture.
	selfBinaryOK
)

// selfBinaryObservation is what could be learned about the Linux debark the
// container backend would mount.
type selfBinaryObservation struct {
	// Host is GOOS. The whole question only arises off Linux, and the
	// observation carries it so the decision does not read runtime state.
	Host string
	// Arch is the dpkg architecture the container will run, which is the
	// architecture the mounted binary has to be built for.
	Arch string
	// Explicit is a path the operator named through Options.SelfBinaryPath or
	// DEBARK_SELF_BINARY. Empty when neither is set.
	Explicit string
	// ExplicitFromEnv records that Explicit came from the environment rather
	// than from the caller, which changes the sentence: an operator who set
	// DEBARK_SELF_BINARY to something wrong needs to be told which one of
	// the two things they configured is at fault.
	ExplicitFromEnv bool
	// Searched are the sibling paths that were tried, in order, for the
	// details drawer. Always populated, even when Explicit won, because "here
	// is where it would have been looked for" is what makes the remedy make
	// sense.
	Searched []string
	// Found is the path that answered, empty when none did.
	Found string
	// Status is what Found turned out to be.
	Status selfBinaryStatus
	// Detail says what was wrong with Found, in the engine's own vocabulary:
	// elf.Open's message when it is not an ELF file at all, the class, byte
	// order and machine when it is one for the wrong target, or the
	// dynamic-linking sentence.
	Detail string
	// BinaryPath is the debark executable the siblings were looked for
	// beside, empty when it could not be located.
	BinaryPath string
	// LocateErr is why the debark binary could not be located, when it
	// could not. The row degrades to "cannot tell yet" rather than claiming a
	// file is missing from a directory it never found.
	LocateErr error
}

func probeSelfBinary(ctx context.Context, o Options) selfBinaryObservation {
	obs := selfBinaryObservation{
		Host: runtime.GOOS,
		Arch: o.SelfBinaryArch,
	}
	if obs.Arch == "" {
		obs.Arch = DefaultContainerArch()
	}

	obs.Explicit = strings.TrimSpace(o.SelfBinaryPath)
	if obs.Explicit == "" {
		// DEBARK_SELF_BINARY is read; the self_binary config key is not,
		// for the reason probeSigningKey gives about DEBARK_SIGN: parsing
		// debark's YAML config here would be a second implementation of its
		// flag/env/file/profile precedence, and a second implementation is a
		// second answer. The application layer has the CLI adapter and sets
		// Options.SelfBinaryPath from `config show --json` instead.
		if v := strings.TrimSpace(os.Getenv("DEBARK_SELF_BINARY")); v != "" {
			obs.Explicit = v
			obs.ExplicitFromEnv = true
		}
	}

	// The siblings are looked for beside the *debark* binary, not beside
	// this application's own executable. containerSelfPath calls
	// os.Executable() in the debark process, so the directory that matters
	// is debark's. Getting this wrong would produce a green row for a file
	// the engine will never look at.
	if o.BinaryPath != "" {
		obs.BinaryPath = o.BinaryPath
	} else if o.Locator != nil {
		p, err := o.Locator(ctx)
		if err != nil {
			obs.LocateErr = err
		} else {
			obs.BinaryPath = p
		}
	}
	obs.Searched = selfBinarySiblingPaths(obs.BinaryPath, obs.Arch)

	candidates := obs.Searched
	if obs.Explicit != "" {
		// An explicit answer always wins, and it is the only one examined:
		// containerSelfPath returns it without looking at the siblings, so a
		// sibling that happens to be correct would not save a build whose
		// --self-binary is wrong.
		candidates = []string{obs.Explicit}
	}

	for _, p := range candidates {
		st, detail, ok := selfBinaryInspect(p, obs.Arch)
		if !ok {
			continue
		}
		obs.Found, obs.Status, obs.Detail = p, st, detail
		if st == selfBinaryOK {
			break
		}
		// A present-but-wrong file stops the search for the same reason
		// containerSiblingSelfPath does: the engine will pick it and fail
		// with a specific diagnosis, and reporting the next candidate instead
		// would describe a build that is not going to happen.
		break
	}
	return obs
}

// selfBinarySiblingPaths is the two spellings containerSiblingSelfPath looks
// for, in its order, beside exePath. Empty when there is no exePath to look
// beside.
func selfBinarySiblingPaths(exePath, arch string) []string {
	if exePath == "" || arch == "" {
		return nil
	}
	dir := filepath.Dir(exePath)
	name := "debark-linux-" + arch
	return []string{filepath.Join(dir, "bin", name), filepath.Join(dir, name)}
}

// selfBinaryInspect reports what is at path. The third return is whether
// anything is there at all: a missing candidate is not a finding, it is the
// next candidate's turn.
//
// The four checks are exactly containerValidateBinary's, in its order — ELF
// magic, then class/data/machine, then PT_INTERP — because a row that
// disagreed with the engine would be worse than no row. debug/elf is the
// standard library and is what the engine uses; hand-parsing the header here
// would be a second implementation of the one thing this check exists to
// agree about.
func selfBinaryInspect(path, arch string) (selfBinaryStatus, string, bool) {
	st, err := os.Stat(path) //nolint:gosec // G703: the path is the operator's own answer to "where is the Linux debark" -- a --self-binary value, DEBARK_SELF_BINARY, or a name derived from the located debark binary. Statting a path the operator named is the check.
	if err != nil || st.IsDir() {
		return selfBinaryAbsent, "", false
	}

	f, err := elf.Open(path) //nolint:gosec // G703: same path, same reason. Only ELF headers are read; nothing is written, executed or copied, and the alternative to opening the file the engine will mount is not checking it.
	if err != nil {
		// Present and not an ELF file. This is the Windows .exe case and the
		// "copied the wrong file" case, and elf.Open's own message names
		// which, so it is passed through rather than paraphrased.
		return selfBinaryNotELF, err.Error(), true
	}
	defer func() { _ = f.Close() }()

	spec, known := selfBinaryArchELF[arch]
	if !known {
		// An architecture the engine cannot validate either
		// (containerValidateBinary refuses it outright). Reporting the file
		// as good would be a guess, but so would reporting it as bad, and
		// this row is not the place to fail a build over an architecture
		// nobody supports: the engine will say so, precisely, at build time.
		return selfBinaryOK, "unrecognised architecture " + arch + "; the file is a Linux ELF binary but was not checked further", true
	}

	if f.Class != spec.Class || f.Data != spec.Data || f.Machine != spec.Machine {
		return selfBinaryWrongArch,
			fmt.Sprintf("it is a %s %s %s binary, not a static linux/%s one", f.Class, f.Data, f.Machine, arch), true
	}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return selfBinaryNotStatic,
				"it names an ELF interpreter, so it is dynamically linked; build with CGO_ENABLED=0", true
		}
	}
	return selfBinaryOK, "", true
}

func decideSelfBinary(obs selfBinaryObservation) Result {
	// On Linux the running debark already is a Linux ELF for the host
	// architecture, so containerSelfPath's third step is a correct answer and
	// there is nothing to report. This check is not registered there; the
	// branch exists so a caller that folds a result in by hand cannot produce
	// a scary row on a machine where nothing is wrong.
	if obs.Host == "linux" {
		return Result{
			Status:   StatusSkipped,
			Severity: SeverityInfo,
			Summary:  "This host is Linux, so debark can mount itself into a build container.",
		}
	}

	buildCmd := selfBinaryBuildCommand(obs.Arch)
	wanted := "bin/debark-linux-" + obs.Arch

	if obs.LocateErr != nil && obs.Explicit == "" {
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  "The debark command-line tool was not found, so the Linux binary its container backend needs cannot be checked for.",
			Remedy:   "Install debark first — this row will then say whether a container build can run on this " + selfBinaryHostWord(obs.Host) + " host.",
			Detail:   "locating debark: " + obs.LocateErr.Error(),
		}
	}

	switch obs.Status {
	case selfBinaryOK:
		summary := "A Linux debark is in place, so builds can run in a container on this " + selfBinaryHostWord(obs.Host) + " host."
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  summary,
			Detail:   selfBinarySourceDetail(obs) + obs.Found,
		}

	case selfBinaryWrongArch, selfBinaryNotELF, selfBinaryNotStatic:
		what := "is not a Linux ELF binary"
		switch obs.Status {
		case selfBinaryWrongArch:
			what = "is built for the wrong architecture"
		case selfBinaryNotStatic:
			what = "is dynamically linked, and the build container has no loader for it"
		}
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary: fmt.Sprintf("The file debark would mount into the build container %s, so every container build will fail.",
				what),
			Remedy: "Replace it with a static linux/" + obs.Arch + " build of debark — the command below writes one where debark already looks.",
			Action: &Action{
				Label:   "Show how to build the Linux debark",
				Command: buildCmd,
				Note:    "Run this in a checkout of the debark source, then copy the result to " + wanted + " beside the debark you are running.",
			},
			Detail: selfBinarySourceDetail(obs) + obs.Found + " (" + obs.Detail + ")",
		}

	default:
		return Result{
			Status: StatusProblem,
			// Degraded, never blocking on its own. A machine with WSL 2 or
			// with a native apt has another way to build, and only the
			// conjunction is fatal — which DeriveBuildEnvironment decides,
			// the same way it does for the container row itself.
			Severity: SeverityDegraded,
			Summary: "No Linux debark was found for the container backend to mount, so container builds will fail on this " +
				selfBinaryHostWord(obs.Host) + " host even though a container runtime is present.",
			Remedy: "Build a static linux/" + obs.Arch + " debark and put it at " + wanted + " beside the debark binary; no flag is then needed on any build.",
			Action: &Action{
				Label:   "Show how to build the Linux debark",
				Command: buildCmd,
				Note:    "Run this in a checkout of the debark source, then copy the result to " + wanted + " beside the debark you are running. --self-binary, DEBARK_SELF_BINARY and the self_binary config key name it explicitly instead.",
			},
			Detail: selfBinarySearchedDetail(obs),
		}
	}
}

// selfBinaryBuildCommand is the cross-compile the core repository's own hint
// names, as an argv rather than a shell line.
//
// It is deliberately the same command core/apt/container.go's
// containerSelfPathHint prints, down to the output path, so an operator who
// reads either one gets the same instruction. CGO_ENABLED and the two GO*
// variables are environment, not arguments, so they lead the Command as an
// "env" invocation — Action.Command is an argv and must never become a shell
// string, and "env" is the portable way to say this without one.
func selfBinaryBuildCommand(arch string) []string {
	return []string{
		"env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + selfBinaryGOARCH(arch),
		"go", "build", "-o", "bin/debark-linux-" + arch, "./cmd/debark",
	}
}

// selfBinaryGOARCH translates a dpkg architecture back to the GOARCH that
// builds it. The two names agree for amd64 and arm64, which is the whole
// supported set today; the others are here so the command is right rather
// than plausible if the set ever widens.
func selfBinaryGOARCH(arch string) string {
	switch arch {
	case "i386":
		return "386"
	case "armhf":
		return "arm"
	case "ppc64el":
		return "ppc64le"
	default:
		return arch
	}
}

// DefaultContainerArch is the dpkg architecture a build on this machine will
// ask a container for when the operator has not chosen a target yet.
//
// It is exported because the application layer needs the same answer to
// pre-fill Options.SelfBinaryArch, and because a target chosen later can
// change it: a snapshot records its own architecture, and an arm64 target
// built from an amd64 host needs an arm64 mounted binary. Options.SelfBinaryArch
// is how that refinement arrives, exactly as Options.ArchiveHost is how the
// archive host does.
func DefaultContainerArch() string {
	switch runtime.GOARCH {
	case "386":
		return "i386"
	case "arm":
		return "armhf"
	case "ppc64le":
		return "ppc64el"
	default:
		return runtime.GOARCH
	}
}

// selfBinarySourceDetail says which of the three routes produced Found, so a
// details drawer can distinguish "you set this" from "this was found for you".
func selfBinarySourceDetail(obs selfBinaryObservation) string {
	switch {
	case obs.Explicit != "" && obs.ExplicitFromEnv:
		return "DEBARK_SELF_BINARY=" + obs.Explicit + "; "
	case obs.Explicit != "":
		return "configured self_binary; "
	default:
		return "found beside debark: "
	}
}

// selfBinarySearchedDetail lists where the search looked, which is the only
// useful detail when it found nothing.
func selfBinarySearchedDetail(obs selfBinaryObservation) string {
	if obs.Explicit != "" {
		return "looked at " + obs.Explicit + ", which is not there"
	}
	if len(obs.Searched) == 0 {
		return "no debark binary to look beside"
	}
	return "looked for " + strings.Join(obs.Searched, " and ")
}

// selfBinaryHostWord names the platform in a sentence.
func selfBinaryHostWord(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	default:
		return goos
	}
}
