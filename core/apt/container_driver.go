package apt

// This file holds the container backend's low-level, dependency-free
// mechanics: container-runtime detection, host-path-to-mount-source
// conversion, dpkg-arch-to-container-platform lookup, ELF validation of the
// binary that gets mounted in, docker/podman argv construction, the exec
// wrapper that runs a container and captures its output, classification of
// the ways that can fail (missing runtime, missing qemu binfmt handler,
// unreachable daemon), the debark.resolveplan/v1 envelope's parser and its
// plan-vs-files cross-check, apt/dpkg image-version discovery, and the
// named-volume-to-host copy step experiment E5 needs.
//
// Every exported-within-package name here is prefixed "container" (or starts
// with a sufficiently distinctive word) on purpose: other files in this
// same package are edited concurrently, and an unprefixed helper
// like "mount" or "lastLines" is exactly the kind of name that collides.
//
// Nothing in this file calls into any non-container*.go file in this
// package: it depends only on the frozen declarations in iface.go/api.go and
// on other core/ packages, never on the rest of core/apt's in-progress
// internals, so it keeps compiling regardless of what state their files are
// in.
import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
)

// --- runtime detection -------------------------------------------------

// containerLookPath is exec.LookPath, indirected so a test can simulate a
// machine with no runtime, or only podman, without touching the real PATH.
var containerLookPath = exec.LookPath

// containerRuntimeCandidates is the autodetection search order: docker
// first, then podman.
var containerRuntimeCandidates = []string{"docker", "podman"}

// containerDetectRuntime finds the container runtime to use: opts.Runtime
// when set, otherwise the first of containerRuntimeCandidates found on PATH.
// Neither present is always a *dferr.Error of class Environment whose Hint
// names the exact install command for runtime.GOOS -- never a silent
// downgrade to a second-class resolver (the contract brief, non-negotiable).
func containerDetectRuntime(opts ContainerOptions) (name, execPath string, err error) {
	if opts.Runtime != "" {
		p, lerr := containerLookPath(opts.Runtime)
		if lerr != nil {
			return "", "", dferr.New(dferr.Environment,
				"container: requested runtime %q not found on PATH", opts.Runtime).
				WithHint("%s", containerRuntimeInstallHint())
		}
		return opts.Runtime, p, nil
	}
	tried := make([]string, 0, len(containerRuntimeCandidates))
	for _, cand := range containerRuntimeCandidates {
		if p, lerr := containerLookPath(cand); lerr == nil {
			return cand, p, nil
		}
		tried = append(tried, cand)
	}
	return "", "", dferr.New(dferr.Environment,
		"container: no container runtime found (tried %s)", strings.Join(tried, ", ")).
		WithHint("%s", containerRuntimeInstallHint())
}

// containerRuntimeInstallHint names the concrete install command for this
// operator's platform.
func containerRuntimeInstallHint() string { return containerRuntimeInstallHintFor(runtime.GOOS) }

// containerRuntimeInstallHintFor is containerRuntimeInstallHint with GOOS as
// an explicit parameter, so a test can exercise every branch deterministically
// regardless of which platform `go test` itself runs on.
func containerRuntimeInstallHintFor(goos string) string {
	switch goos {
	case "windows":
		return "install Docker Desktop (https://docs.docker.com/desktop/install/windows-install/) or Podman Desktop (https://podman-desktop.io/), then make sure `docker` or `podman` is on PATH"
	case "darwin":
		return "install Docker Desktop (https://docs.docker.com/desktop/install/mac-install/, or `brew install --cask docker`) or Podman (`brew install podman`), then make sure `docker` or `podman` is on PATH"
	case "linux":
		return "install Docker (https://docs.docker.com/engine/install/, e.g. `sudo apt-get install docker.io`) or Podman (e.g. `sudo apt-get install podman`), then make sure `docker` or `podman` is on PATH"
	default:
		return "install Docker or Podman and make sure `docker` or `podman` is on PATH"
	}
}

// --- host path -> mount source ------------------------------------------

var containerWinDriveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// containerMountSource converts an absolute, OS-native path into the form
// accepted as a docker/podman "-v" bind-mount source: forward slashes
// throughout, and a Windows drive-letter path lowercased to the form docker
// itself expects ("d:/projects/debark", not "D:\projects\debark" and not a
// Git-Bash-mangled "/d/projects/...").
//
// This is a pure function of the path string's own shape, not of
// runtime.GOOS -- it recognises a Windows drive-letter path by pattern, so
// it produces the same answer under `go test` on any platform, which is
// exactly what makes it unit-testable without a Windows machine. The
// argv this package builds is always executed directly via os/exec with an
// explicit argv slice, never through a shell, so this conversion is the
// only path-mangling risk in the whole pipeline: no MSYS/Git-Bash argv
// rewriting ever touches these strings, because no shell is ever invoked
// to run docker/podman.
func containerMountSource(p string) (string, error) {
	if p == "" {
		return "", dferr.Usagef("container: empty mount path")
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "", dferr.Usagef(
			"container: %q is a UNC path, which cannot be used as a container mount source; copy it to a local drive path first", p)
	}
	if containerWinDriveRe.MatchString(p) {
		drive := p[0]
		if drive >= 'A' && drive <= 'Z' {
			drive += 'a' - 'A'
		}
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		rest = path.Clean("/" + rest)
		return string(drive) + ":" + rest, nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", dferr.Usagef("container: %q is not an absolute path", p)
	}
	return path.Clean(p), nil
}

// containerCheckAbs rejects a directory that cannot become a container mount
// source, before anything has been created inside it.
//
// It validates by calling containerMountSource itself rather than by
// re-deciding what "absolute" means: this package accepts two shapes (a POSIX
// "/x" path and a Windows "D:\x" drive path) independently of which OS the
// process is running on, precisely so the same conversion is testable
// everywhere, and filepath.IsAbs answers only for the host's own OS -- it
// calls "/work" relative on Windows and "d:/work" relative on Linux, either of
// which would be a wrong rejection here. One definition, used twice.
func containerCheckAbs(p, what string) error {
	if _, err := containerMountSource(p); err != nil {
		return dferr.Wrap(dferr.Usage, err, "container: %s", what)
	}
	return nil
}

// containerCheckMountSourceDir stats path and returns a clear error if it is
// missing or is not a directory.
//
// This check exists because docker/podman do NOT error on a missing
// bind-mount source: verified empirically against Docker Desktop 29.6.2 on
// Windows, "-v <nonexistent host path>:/mnt" silently creates an empty
// directory at /mnt inside the container instead of failing. Without this
// check, a simple path bug on the host turns into a baffling "file not
// found" report from *inside* the container instead of a clear message
// naming the actual missing host path.
func containerCheckMountSourceDir(path, what string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "container: %s", what)
	}
	if !fi.IsDir() {
		return dferr.Usagef("container: %s (%s) is not a directory", what, path)
	}
	return nil
}

// containerCheckMountSourceFile is containerCheckMountSourceDir for a file
// mount.
func containerCheckMountSourceFile(path, what string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "container: %s", what)
	}
	if fi.IsDir() {
		return dferr.Usagef("container: %s (%s) is a directory, want a file", what, path)
	}
	return nil
}

// --- platform mapping ----------------------------------------------------

// containerPlatformFor resolves the docker/podman --platform value for a
// dpkg architecture, honouring ContainerOptions.Platform as an override.
func containerPlatformFor(opts ContainerOptions, dpkgArch string) (string, error) {
	if opts.Platform != "" {
		return opts.Platform, nil
	}
	p, ok := distro.Platform(dpkgArch)
	if !ok {
		return "", dferr.Usagef(
			"container: no container --platform mapping for dpkg architecture %q (known: %s)",
			dpkgArch, strings.Join(distro.Architectures(), ", "))
	}
	return p, nil
}

// --- ELF validation --------------------------------------------------------

type containerELFArchSpec struct {
	Machine elf.Machine
	Class   elf.Class
	Data    elf.Data
}

// containerArchELF maps a dpkg architecture name to the ELF header fields a
// static Linux binary for that architecture must carry. Keys match
// core/distro's architecture set exactly.
var containerArchELF = map[string]containerELFArchSpec{
	"amd64":   {elf.EM_X86_64, elf.ELFCLASS64, elf.ELFDATA2LSB},
	"arm64":   {elf.EM_AARCH64, elf.ELFCLASS64, elf.ELFDATA2LSB},
	"armhf":   {elf.EM_ARM, elf.ELFCLASS32, elf.ELFDATA2LSB},
	"armel":   {elf.EM_ARM, elf.ELFCLASS32, elf.ELFDATA2LSB},
	"i386":    {elf.EM_386, elf.ELFCLASS32, elf.ELFDATA2LSB},
	"ppc64el": {elf.EM_PPC64, elf.ELFCLASS64, elf.ELFDATA2LSB},
	"s390x":   {elf.EM_S390, elf.ELFCLASS64, elf.ELFDATA2MSB},
	"riscv64": {elf.EM_RISCV, elf.ELFCLASS64, elf.ELFDATA2LSB},
}

// containerValidateBinary checks that path is a static Linux ELF binary for
// wantArch before it is ever mounted into a container: a Windows PE binary,
// a macOS Mach-O binary, or a Linux binary for the wrong architecture
// produces nothing but a baffling "exec format error" from *inside* the
// container if this is not caught first (docs/dev/resolve-contract.md: "The
// driver checks this before running and fails with a clear environment
// error rather than producing an exec-format error from inside the
// container").
//
// Reading the ELF header (magic, class, machine) is a handful of bytes with
// no third-party dependency: this uses only the standard library's
// debug/elf. The PT_INTERP program-header check additionally distinguishes
// a static binary (CGO_ENABLED=0, no interpreter requested) from a
// dynamically linked one, which is the other half of "static" that a bare
// magic/class/machine check would miss.
func containerValidateBinary(path, wantArch string) error {
	spec, ok := containerArchELF[wantArch]
	if !ok {
		return dferr.Usagef("container: %q is not an architecture debark can validate a binary for", wantArch)
	}
	f, err := elf.Open(path)
	if err != nil {
		return dferr.New(dferr.Environment,
			"container: %s does not look like a Linux ELF binary (%v)", path, err).
			WithHint("%s", containerSelfPathHint(wantArch))
	}
	defer func() { _ = f.Close() }()

	if f.Class != spec.Class || f.Data != spec.Data || f.Machine != spec.Machine {
		return dferr.New(dferr.Environment,
			"container: %s is a %s %s %s binary, not a static linux/%s binary", path, f.Class, f.Data, f.Machine, wantArch).
			WithHint("%s", containerSelfPathHint(wantArch))
	}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return dferr.New(dferr.Environment,
				"container: %s is dynamically linked (it names an ELF interpreter); the mounted binary must be static", path).
				WithHint("build with CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -o debark-linux-%s ./cmd/debark", wantArch, wantArch)
		}
	}
	return nil
}

// containerSelfPathHint is the operator-facing instruction for how to supply
// the Linux debark binary the container backend mounts and re-enters: see
// containerSelfPath in container.go for the three places it is looked for
// and why there usually is no working default off Linux.
//
// It names things an operator can actually type. It used to say "set
// ContainerOptions.SelfPath", which is a Go struct field on an unexported
// code path -- correct for the one caller that is a Go program, and useless
// to the operator reading it, who has no way to set a struct field from a
// command line. That was the whole of the guidance a Windows or macOS user
// got when `debark build --backend container` refused with "debark.exe
// does not look like a Linux ELF binary".
func containerSelfPathHint(wantArch string) string {
	return fmt.Sprintf(
		"build a static linux/%s debark and point --self-binary at it: CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -o bin/debark-linux-%s ./cmd/debark "+
			"(bin/debark-linux-%s beside the debark you are running is found without any flag; DEBARK_SELF_BINARY and the self_binary config key also work)",
		wantArch, wantArch, wantArch, wantArch)
}

// --- argv construction -----------------------------------------------------

// containerResolveArgs is everything needed to build the argv of the
// `debark resolve` invocation that runs *inside* the container.
type containerResolveArgs struct {
	Packages      []string
	ExternalRepo  string // container path, e.g. "/external"; empty when unused
	ExternalNames []string
	Recommends    bool
	// RecommendsSet says the caller actually decided a Recommends value, so
	// --recommends is passed. Without it the flag is omitted entirely and the
	// inner process falls back to its own documented default ("from the
	// snapshot").
	//
	// This exists because "false" and "no opinion" were the same value.
	// containerBuildInnerArgv appended --recommends=%t unconditionally, so a
	// caller that left Recommends alone -- which is exactly what
	// containerBackend.ClosedWorld did, its own comment claiming the flag was
	// "left unset" -- shipped "--recommends=false" and narrowed the inner
	// resolve's dependency closure without anyone choosing that. A bool that
	// cannot express "unset" must be paired with one that can, or the zero
	// value silently becomes policy.
	RecommendsSet bool
	Upgrades      bool
	Arch          string
	ApprovedKeys  []string
	// JSONEvents adds --json-events - so the inner process's evidence
	// stream comes back to the host over its own stderr, instead of being
	// discarded inside the container. Set only when there is a sink to
	// forward it to, and deliberately NOT set by ClosedWorld: see
	// containerForwardEventTo for why that caller's captured output must
	// not change.
	JSONEvents bool
}

// containerBuildInnerArgv builds the argv of the debark binary run *inside*
// the container: /debark resolve --backend local ... The flag order
// matches docs/dev/resolve-contract.md's command-line grammar field for
// field, so this function doubles as a conformance check against that
// frozen contract -- change the order here only if the contract changes.
//
// --backend local is always passed explicitly (never left to the inner
// process's own "auto" default): "Inside a container this is always local"
// per the contract, and the driver, not the inner process, is what must make
// that true.
func containerBuildInnerArgv(a containerResolveArgs) []string {
	argv := []string{
		"/debark", "resolve",
		"--snapshot", "/snapshot",
		"--archives", "/archives",
		"--work", "/work",
		"--plan-out", "/work/plan.json",
		"--backend", "local",
	}
	for _, p := range a.Packages {
		argv = append(argv, "--package", p)
	}
	if a.ExternalRepo != "" {
		argv = append(argv, "--external-repo", a.ExternalRepo)
		for _, n := range a.ExternalNames {
			argv = append(argv, "--external-name", n)
		}
	}
	if a.RecommendsSet {
		argv = append(argv, fmt.Sprintf("--recommends=%t", a.Recommends))
	}
	if a.Upgrades {
		argv = append(argv, "--upgrades")
	}
	if a.Arch != "" {
		argv = append(argv, "--arch", a.Arch)
	}
	if len(a.ApprovedKeys) > 0 {
		keys := append([]string(nil), a.ApprovedKeys...)
		sort.Strings(keys)
		for _, k := range keys {
			argv = append(argv, "--approved-key", k)
		}
	}
	if a.JSONEvents {
		// Appended immediately before --json, so the two inner invocations
		// keep the shape docs/dev/resolve-contract.md fixes: operand,
		// flags in a fixed order, then the output flags last.
		//
		// "-" is stderr for this command, not stdout: resolveEvidenceSink
		// (internal/cli/cmd_resolve.go) exists precisely to make that true,
		// because stdout is reserved for the plan envelope. The host reads
		// both streams anyway (execContainer), so this is documented rather
		// than depended on.
		argv = append(argv, "--json-events", "-")
	}
	argv = append(argv, "--json")
	return argv
}

// containerFromBaseArgs is everything needed to build the argv of the
// `debark snapshot from-base` invocation that runs *inside* the container
// (basecontainer.go).
//
// A separate type from containerResolveArgs rather than more fields on it:
// the two inner commands share only --arch and --backend, and a single
// struct whose fields apply to one of two commands is how a field ends up
// silently ignored on the path it does not belong to.
type containerFromBaseArgs struct {
	// BaseRef is the base as the INNER binary must see it: a builtin id
	// ("ubuntu:26.04/desktop"), which the mounted binary already has
	// compiled in, or the fixed container path a host definition file was
	// mounted at. Never a host path -- basecontainer.go substitutes the
	// mount target before calling this, because a host path means nothing
	// inside the container and core/base.Resolve would report it as
	// "neither a builtin base id nor a file that exists", naming a path the
	// operator can see on their own disk. That would be a confusing way to
	// say "the mount is missing".
	BaseRef string
	// Arch is the dpkg architecture the base is materialised for.
	Arch string
	// OutName is the archive's base name; the inner command writes it into
	// the shared /work mount, which is how the finished snapshot crosses
	// back to the host. Already validated as a plain base name by
	// containerCheckOutName before this is reached.
	OutName string
	// JSONEvents adds --json-events - so the inner process's evidence
	// stream comes back to the host. Set only when there is a sink to
	// forward it to; see containerForwardEventTo.
	JSONEvents bool
}

// containerBuildFromBaseArgv builds the argv of the debark binary run
// *inside* the container to synthesize a base snapshot end to end.
//
// Mirrors containerBuildInnerArgv's shape -- the operand first, then the
// flags in a fixed order, then --json -- so the two inner invocations read
// the same way and a reader who has understood one has understood the
// other.
//
// --backend local is always passed explicitly, for exactly the reason
// containerBuildInnerArgv gives: inside a container it is always local, and
// the driver, not the inner process, is what must make that true. Left to
// "auto", an inner process that decided it could reach a container runtime
// of its own would try to nest one.
//
// Note what is NOT here, because it is the interesting half. There is no
// --snapshot: this command has no input snapshot, it produces one. There is
// no --archives: a base closure resolves a package list and downloads
// nothing (core/apt/closure.go's package comment -- a desktop seed closure
// is gigabytes of .deb files that would be fetched, hashed and thrown away),
// so there is no Dir::Cache::archives for the inner process to fill and no
// mount for it to fill it into. The whole reason this operation crosses the
// boundary as a finished snapshot archive rather than as a package list is
// that a Windows or macOS host has no Debian/Ubuntu archive keyring to build
// a bootstrap root with, and so cannot construct the input a package list
// would have to be resolved against or the keyrings the finished snapshot
// must carry (see iface.go's note where Backend deliberately does not gain a
// third method).
func containerBuildFromBaseArgv(a containerFromBaseArgs) []string {
	argv := []string{"/debark", "snapshot", "from-base", a.BaseRef}
	if a.Arch != "" {
		argv = append(argv, "--arch", a.Arch)
	}
	argv = append(argv, "--backend", "local")
	argv = append(argv, "--out", path.Join("/work", a.OutName))
	if a.JSONEvents {
		// Unlike `resolve`, this command has no envelope on stdout to
		// protect, so its "-" is the ordinary sink's stdout. execContainer
		// splits both streams for exactly this reason -- so neither inner
		// command's choice has to be encoded here.
		argv = append(argv, "--json-events", "-")
	}
	argv = append(argv, "--json")
	return argv
}

// containerMount is one explicit bind mount or named volume, rendered in
// docker/podman "-v" syntax.
type containerMount struct {
	// Source is a host path already converted by containerMountSource, or a
	// named volume name.
	Source string
	Target string
	RO     bool
}

func (m containerMount) spec() string {
	if m.RO {
		return m.Source + ":" + m.Target + ":ro"
	}
	return m.Source + ":" + m.Target
}

// containerBuildRunArgv builds the full docker/podman argv: run --rm
// [--platform ...] [extraFlags...] -v ... -v ... <image> <inner argv...>.
// Mount order is the order docs/dev/resolve-contract.md's mount table lists
// them in: binary, snapshot, work, archives, external (when present).
func containerBuildRunArgv(platform string, mounts []containerMount, extraFlags []string, image string, inner []string) []string {
	argv := []string{"run", "--rm"}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	argv = append(argv, extraFlags...)
	for _, m := range mounts {
		argv = append(argv, "-v", m.spec())
	}
	argv = append(argv, image)
	argv = append(argv, inner...)
	return argv
}

// --- exec wrapper ------------------------------------------------------

// containerRunResult is what one docker/podman invocation produced.
type containerRunResult struct {
	Argv     []string
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Events counts what the inner process's forwarded event stream did,
	// including what was refused. Zero when nothing was forwarded, which is
	// every run made with a nil onEvent. See containerEmitForwardSummary.
	Events containerEventCounts
}

func (r containerRunResult) combined() []byte {
	return append(append([]byte{}, r.Stdout...), r.Stderr...)
}

// execContainer runs the container runtime directly via os/exec with an
// explicit argv slice -- never through a shell -- and captures stdout and
// stderr separately (stdout carries only the plan envelope when --plan-out
// is not used; this driver always uses --plan-out and reads the file, so
// stdout here is diagnostic only). A non-zero exit is not itself a Go error:
// it is returned as part of the result so the caller can classify it against
// the resolve contract's exit-code table; only a failure to even start the
// runtime process is returned as an error here.
//
// onEvent, when non-nil, turns both captured streams into line splitters: a
// line that is a valid debark.events/v1 record from the process inside the
// container is handed to onEvent AS IT ARRIVES and kept out of the captured
// output, and everything else is captured exactly as before. That is the
// whole of the container's event forwarding (containerevents.go), and its
// liveness is the point -- a 61 s build that reports four events at the end
// is indistinguishable from one that reports nothing.
//
// A nil onEvent is byte-for-byte the old behaviour: plain buffers, no
// splitting, nothing removed from Stdout or Stderr. Callers whose captured
// output is hashed into an artefact pass nil deliberately; see
// containerForwardEventTo.
func execContainer(ctx context.Context, runtimePath string, argv []string, onEvent func(evidence.Event)) (containerRunResult, error) {
	cmd := exec.CommandContext(ctx, runtimePath, argv...)
	var stdout, stderr bytes.Buffer
	var outW, errW *containerEventStream
	if onEvent == nil {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	} else {
		// One sink behind two splitters: os/exec copies each stream on its
		// own goroutine, and the two inner commands disagree about which
		// stream carries the events. See containerevents.go.
		sink := &containerEventSink{emit: onEvent}
		outW = &containerEventStream{diag: &stdout, sink: sink}
		errW = &containerEventStream{diag: &stderr, sink: sink}
		cmd.Stdout, cmd.Stderr = outW, errW
	}
	runErr := cmd.Run()
	if outW != nil {
		// Flushed here, before the buffers are read: Run returns only after
		// Wait has closed the pipes and joined the goroutines copying them,
		// so there is no writer left to race with -- but a process killed
		// mid-write leaves a trailing partial line, and that line is exactly
		// the last thing the container managed to say.
		outW.Close()
		errW.Close()
	}

	res := containerRunResult{
		Argv:   append([]string{runtimePath}, argv...),
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}
	if errW != nil {
		res.Events = errW.sink.counts()
	}
	if runErr == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, dferr.Wrap(dferr.Environment, runErr, "container: failed to run %s", runtimePath)
}

// execContainerFn indirects execContainer, mirroring containerLookPath's
// existing indirection for runtime detection above: production code
// (container.go's Resolve and ClosedWorld) always calls through this var, so
// a test can substitute a fake container run -- no docker/podman binary, no
// network, no filesystem beyond t.TempDir() -- and prove what container.go
// does with a given (fake) inner resolve result, the same way fakeCWRunner
// (closedworld_test.go) lets the local backend's tests substitute apt-get
// itself.
var execContainerFn = execContainer

// --- error classification -------------------------------------------------

var (
	containerExecFormatErrorRe   = regexp.MustCompile(`(?i)exec format error`)
	containerDaemonUnreachableRe = regexp.MustCompile(`(?i)cannot connect to the docker daemon|is the docker daemon running|permission denied while trying to connect|error during connect`)
	containerNoManifestRe        = regexp.MustCompile(`(?i)no matching manifest for|no such manifest|not found: manifest unknown`)
)

// containerBinfmtHint is the fix for the qemu/binfmt failure mode.
func containerBinfmtHint() string {
	return "register a QEMU binfmt handler for the target architecture: `docker run --privileged --rm tonistiigi/binfmt --install all` (Linux hosts), or install the `qemu-user-static`/`binfmt-support` packages; Docker Desktop normally registers these automatically, so also check Settings > General for a disabled virtualization/emulation option"
}

// containerExitClassFor maps an inner debark process's own exit code onto
// the standard dferr class table (core/dferr), which is also the process's
// own convention (docs/dev/contract-brief.md: "Exit codes 0-7 are frozen").
func containerExitClassFor(code int) dferr.Class {
	switch code {
	case 0:
		return dferr.Success
	case 1:
		return dferr.Usage
	case 2:
		return dferr.Environment
	case 3:
		return dferr.Incomplete
	case 4:
		return dferr.Verification
	case 5:
		return dferr.Resolution
	case 6:
		return dferr.Policy
	case 7:
		return dferr.TargetMismatch
	default:
		// An unrecognised code (a shell's 127, a signal-related 128+n, ...)
		// almost always means something about the environment went wrong
		// rather than the resolution itself, so Environment is the safer
		// default classification.
		return dferr.Environment
	}
}

// containerClassifyRunResult turns a finished (non-zero, and not the
// documented "3 = incomplete but a plan was still written" case) container
// invocation into a classified error. Docker/podman-level failures (missing
// qemu binfmt handler, unreachable daemon, no image for --platform) are
// detected from the combined output text first, since those never reach the
// inner debark command at all; anything else falls back to the inner
// command's own exit code.
func containerClassifyRunResult(res containerRunResult, platform string) error {
	out := res.combined()
	switch {
	case containerExecFormatErrorRe.Match(out):
		return dferr.New(dferr.Environment,
			"container: could not execute the mounted binary under --platform %s (exec format error)", platform).
			WithHint("%s", containerBinfmtHint())
	case containerDaemonUnreachableRe.Match(out):
		return dferr.New(dferr.Environment, "container: could not reach the container runtime's daemon/socket").
			WithHint("start Docker Desktop (or `podman machine start`), then retry")
	case containerNoManifestRe.Match(out):
		return dferr.New(dferr.Environment,
			"container: no image found for --platform %s", platform).
			WithHint("%s", containerBinfmtHint())
	default:
		class := containerExitClassFor(res.ExitCode)
		return dferr.New(class, "container: %s exited %d: %s", res.Argv[0], res.ExitCode, containerLastLines(out, 6))
	}
}

// containerLastLines returns the last n non-empty lines of b, joined with
// " | ", for a compact one-line error message. (A private copy: this file
// intentionally depends on nothing outside container*.go -- see the package
// doc at the top of this file.)
func containerLastLines(b []byte, n int) string {
	trimmed := strings.TrimRight(string(b), "\r\n")
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// --- resolve envelope -------------------------------------------------

// containerEnvelopeSchema is the only debark resolve --plan-out schema
// this driver understands (docs/dev/resolve-contract.md).
const containerEnvelopeSchema = "debark.resolveplan/v1"

// containerEnvelope is debark.resolveplan/v1. Its Plan field is typed as
// resolve.Plan directly (not a hand-shadowed struct): resolve.Plan carries
// no json tags of its own, so whatever wrote the envelope must have
// marshalled a real resolve.Plan value with encoding/json's default field
// naming, and unmarshalling into the identical Go type is what guarantees
// this side reads it back exactly, rather than trusting a hand-written
// mirror of the field names to stay in sync.
//
// Backend and Files, by contrast, are small structs private to this
// envelope: the contract's example shows them with their own lowercase
// field names distinct from any frozen struct in core/lock or core/resolve
// (e.g. "backend.kind", not lock.Resolver's "backend"/"image" shape), so
// they are modelled here to match the documented example exactly.
type containerEnvelope struct {
	SchemaVersion string                   `json:"schema_version"`
	Plan          resolve.Plan             `json:"plan"`
	Backend       containerEnvelopeBackend `json:"backend"`
	ArchivesDir   string                   `json:"archives_dir"`
	Files         []containerEnvelopeFile  `json:"files"`
}

type containerEnvelopeBackend struct {
	Kind        string `json:"kind"`
	APTVersion  string `json:"apt_version"`
	DpkgVersion string `json:"dpkg_version"`
	DistroID    string `json:"distro_id"`
	VersionID   string `json:"version_id"`
}

type containerEnvelopeFile struct {
	Name     string `json:"name"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

// containerParseEnvelope decodes and validates the schema_version of one
// debark resolve --plan-out envelope.
func containerParseEnvelope(data []byte) (containerEnvelope, error) {
	var env containerEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return containerEnvelope{}, dferr.Wrap(dferr.Verification, err, "container: malformed resolve plan envelope")
	}
	if env.SchemaVersion != containerEnvelopeSchema {
		return containerEnvelope{}, dferr.New(dferr.Verification,
			"container: resolve plan envelope schema_version %q, want %q", env.SchemaVersion, containerEnvelopeSchema)
	}
	return env, nil
}

// containerCheckEnvelopeFilename is the guard on the one envelope field
// this driver turns into a host path.
//
// The envelope is untrusted by this file's own design -- containerCrossCheck-
// Envelope and containerReverifyFiles exist precisely because "the caller
// checks the two agree, which catches a truncated mount or a partial download
// without trusting either side alone". Filename escaped that treatment
// completely: containerRewriteStagedPaths joined it onto ArchivesDir or
// ExternalRepoDir with nothing in between, and core/engine's ingestPlan then
// calls store.PutFile(StagedPath, moveOK=true) -- an os.Rename of that path
// into the content store, with a copy-then-os.Remove fallback. A Filename of
// "../../.ssh/id_ed25519" therefore did not read a file outside the mount, it
// MOVED it off the builder. That was demonstrated end to end against the real
// core/store before this guard was written, not reasoned about.
//
// repository.ValidFilename is the project's existing answer to exactly this
// question -- "is this string safe to use as the base name of a file" -- and
// it is already what core/repository, core/bundle and the lock validator hold
// a .deb filename to, so using it here keeps one definition rather than
// inventing a second that could drift: a single path element, no separator,
// no parent reference, no leading dot, no control characters, ending .deb.
//
// An empty Filename is NOT rejected here: a selection may legitimately carry
// none (it is then skipped by containerRewriteStagedPaths and never becomes a
// path at all), and files[] entries are checked with the same helper only
// where they are non-empty for the same reason.
func containerCheckEnvelopeFilename(what, filename string) error {
	if repository.ValidFilename(filename) {
		return nil
	}
	return dferr.New(dferr.Verification,
		"container: envelope %s has filename %q, which is not a plain .deb file name; "+
			"this driver joins it onto a host directory and core/engine then MOVES that path into the store, "+
			"so a name carrying a separator or a parent reference is refused rather than resolved",
		what, filename)
}

// containerCrossCheckEnvelope compares the envelope's plan against its files
// list. The contract states the two are deliberately redundant ("the caller
// checks the two agree, which catches a truncated mount or a partial
// download without trusting either side alone"); this is that check. It
// looks only at data already in memory -- containerReverifyFiles is the
// half that touches the filesystem.
//
// requestedExternal is ResolveInput.ExternalNames: the package names the HOST
// staged vendor .deb files for, and therefore the only names entitled to come
// back labelled reason:external.
//
// Why that argument exists. "reason: external" is not a description in this
// envelope, it is a routing decision: every check in this function
// short-circuits on it (external .debs legitimately never appear in files[],
// because files[] is scoped to what is in --archives), containerReverifyFiles
// iterates files[] and so cannot see one either, and
// containerRewriteStagedPaths sends it to ExternalRepoDir instead of
// ArchivesDir. The envelope is produced by a process the host cannot vouch
// for -- that is the premise this whole file is written on -- so a label that
// steers a selection past every byte check must be checked against something
// the host knows independently, and in.ExternalNames is that thing.
//
// Measured, before this argument existed: an envelope claiming the package
// `openssl` was reason:external with filename "vendorpkg_1.0_amd64.deb" was
// accepted, and vendor bytes entered the bundle under the identity `openssl`
// with its provenance decided entirely by the envelope. localBackend has no
// such hole: it sets Reason = ReasonExternal only for names in
// in.ExternalNames (local.go's externalConfirmed), so the label there is
// always a host-side conclusion. This makes the container backend enforce the
// same rule on the way back in, on the same bare-Name key local uses.
// archivesVolume is the shared named volume backing /archives for this run,
// or "" when /archives was a bind mount of a directory this run created and
// owns (storeVolumeChoice makes that choice; Linux binds, Windows and macOS
// use a volume). It changes no check. It decides the CLASS of exactly one of
// them -- see the files[]-lists-something-unselected refusal at the bottom of
// this function -- and supplies the hint that says what to do about it.
func containerCrossCheckEnvelope(env containerEnvelope, requestedExternal []string, archivesVolume string) error {
	allowedExternal := make(map[string]bool, len(requestedExternal))
	for _, n := range requestedExternal {
		allowedExternal[n] = true
	}
	byKey := make(map[string]containerEnvelopeFile, len(env.Files))
	for _, f := range env.Files {
		if f.Filename != "" {
			if err := containerCheckEnvelopeFilename("files[] entry for "+f.Name, f.Filename); err != nil {
				return err
			}
		}
		byKey[f.Name+":"+f.Arch] = f
	}
	seen := make(map[string]bool, len(env.Files))
	for _, sel := range env.Plan.Selections {
		// Checked for EVERY selection, external included, and before the
		// external short-circuit below: this is the earliest point at which
		// the field that becomes a host path is in memory, and an external
		// selection is exactly the one that used to reach
		// containerRewriteStagedPaths with nothing having looked at it.
		if sel.Filename != "" {
			if err := containerCheckEnvelopeFilename("plan selection "+sel.Name, sel.Filename); err != nil {
				return err
			}
		}
		if sel.Reason == lock.ReasonExternal {
			// The label is only allowed to do that skipping if the host
			// asked for this name as an external in the first place. An
			// envelope that invents the label for anything else is claiming
			// operator-supplied provenance for bytes the operator never
			// supplied, and using the claim to route itself past the
			// plan/files agreement check below and past
			// containerReverifyFiles' re-hash. Refused here, at the earliest
			// point the whole envelope is in memory.
			if !allowedExternal[sel.Name] {
				return dferr.New(dferr.Verification,
					"container: plan selects %s (%s) with reason %q, but %q was not among the external package names this build staged; "+
						"that label routes a selection past the plan/files cross-check and the host-side re-hash, so it is not the envelope's to assign",
					sel.Name, sel.Arch, sel.Reason, sel.Name)
			}
			// External .debs are staged from --external-repo (a separate ro
			// mount), not fetched into --archives, so they are not expected
			// to appear in files[], which is scoped to "what is actually on
			// disk in --archives" per the contract. Their bytes are checked
			// instead by containerReverifyExternalFiles, host-side, against
			// the external staging directory the host itself populated.
			continue
		}
		f, ok := byKey[sel.Key()]
		if !ok {
			return dferr.New(dferr.Verification,
				"container: plan selected %s=%s (%s) but it is not in the envelope's files[] (archives mount may be truncated)",
				sel.Name, sel.Version, sel.Arch)
		}
		seen[sel.Key()] = true
		if f.Version != sel.Version || f.Filename != sel.Filename {
			// The class stays Verification: the file that will actually
			// enter the bundle is the one this branch says the envelope
			// contradicts itself about, and that is what exit 4 is for.
			//
			// The hint does not stay silent, though, because a stale shared
			// volume reaches this branch too and is by far the likelier
			// cause on a Windows or macOS builder. files[] is keyed here by
			// name:arch, so when the volume holds two versions of one
			// package -- jq 1.6 from a Debian 12 build beside jq 1.7.1 from
			// an Ubuntu 24.04 one, both measured in one volume -- the map
			// keeps whichever came last and reports it as a disagreement
			// with the plan, even though the file the plan selected is
			// sitting right there. That keying is a defect in its own right
			// and is deliberately not changed here; naming the volume costs
			// nothing and is the difference between "your bundle was
			// tampered with" and "your cache is stale".
			err := dferr.New(dferr.Verification,
				"container: envelope plan/files disagree for %s: plan says %s (%s), files says %s (%s)",
				sel.Name, sel.Version, sel.Filename, f.Version, f.Filename)
			if archivesVolume != "" {
				return err.WithHint("%s", containerStaleArchivesVolumeHint(archivesVolume))
			}
			return err
		}
		if sel.SHA256 != "" && f.SHA256 != "" && !digest.Equal(sel.SHA256, f.SHA256) {
			return dferr.New(dferr.Verification,
				"container: envelope plan/files digest mismatch for %s: plan says %s, files says %s",
				sel.Name, sel.SHA256, f.SHA256)
		}
		if sel.Size != 0 && f.Size != 0 && sel.Size != f.Size {
			return dferr.New(dferr.Verification,
				"container: envelope plan/files size mismatch for %s: plan says %d, files says %d",
				sel.Name, sel.Size, f.Size)
		}
	}
	// env.Files, not byKey: ranging over the map made the refusal name
	// whichever unselected file Go's randomised map iteration reached first,
	// so two runs of one failing build blamed different packages. Caught by
	// this function's own test failing intermittently on Linux with two
	// leftovers present -- which is the same defect, one level down, as the
	// name:arch keying noted at the plan/files disagreement above. An error
	// message an operator is meant to act on has to say the same thing twice.
	//
	// Reporting the FIRST unselected entry in the envelope's own order also
	// means it names the file the inner process listed first, which is the
	// order scanArchivesDir read the directory in.
	for _, f := range env.Files {
		key := f.Name + ":" + f.Arch
		if !seen[key] {
			return containerUnselectedFileError(f, key, archivesVolume)
		}
	}
	return nil
}

// containerUnselectedFileError refuses an envelope whose files[] lists
// something the plan does not select, and decides which KIND of failure that
// is from how /archives was backed.
//
// Why the class is not simply Verification any more. The two directions of
// this cross-check are not the same kind of statement:
//
//   - A selection with no files[] entry means a file the bundle NEEDS is
//     absent. That is a truncated mount or a partial download, it is exactly
//     what exit 4 exists to say, and it is unchanged above.
//   - A files[] entry with no selection means there is a file present that
//     nothing asked for. It cannot enter the bundle: core/engine builds the
//     repository from plan.Selections, containerRewriteStagedPaths walks the
//     same list, and nothing anywhere reads an unselected files[] row except
//     containerReverifyFiles, which only re-hashes it. Truncation means files
//     MISSING; this is the opposite.
//
// And on Windows and macOS the overwhelmingly likely cause is not an anomaly
// at all. storeVolumeChoice defaults /archives to the shared, persistent,
// never-cleaned volume named by containerDefaultStoreVolume, and
// scanArchivesDir (internal/cli/cmd_resolve.go) puts EVERY .deb in that
// directory into files[]. So one .deb left behind by an earlier build of a
// different target fails every later build on that machine -- any target, any
// architecture, any package list -- before apt's work is ever used. Measured
// on a Windows builder whose debark-archives-cache held seven .deb files
// from two distributions side by side: a fresh `build --backend container`
// for ubuntu:24.04/minimal asking only for jq exited 4 on hello_2.10-3, left
// there by something else entirely, with the CLI's verification-class hint
// telling the operator not to install a bundle that failed verification and
// to obtain a fresh copy from the machine that built it. No bundle was ever
// produced. Nothing had been verified.
//
// dferr.Environment (exit 2) is the honest class for that: "the machine
// cannot do the job". It is machine state the operator did not choose, cannot
// see, and -- until ContainerOptions.StoreVolume gets a flag -- cannot point
// elsewhere. The only remedy is a machine-level command, which is what the
// hint now says.
//
// The boundary is structural, in the shape 2d6d5c7 and 5837d82 drew for
// `snapshot inspect` and `verify`, and not a general softening: it moves only
// when /archives is a SHARED volume that outlives this build. With a bind
// mount -- every Linux build, and any run given an explicit directory -- the
// host created that directory for this run and this run alone, so a file in
// it that the plan does not select really is an anomaly, and it stays
// Verification. Both halves are pinned by tests in one file, for the reason
// 5837d82 gives: they are one decision, and splitting them is how a later
// change softens one without noticing it has broken the other.
func containerUnselectedFileError(f containerEnvelopeFile, key, archivesVolume string) error {
	if archivesVolume == "" {
		return dferr.New(dferr.Verification,
			"container: envelope files[] lists %s (%s) but the plan does not select it", f.Name, key)
	}
	return dferr.New(dferr.Environment,
		"container: the shared archives cache volume %q holds %s (%s), which this build's plan does not select, "+
			"so the envelope's files[] and its plan cannot agree; nothing unselected can enter a bundle, "+
			"so this is a stale cache on this machine rather than a failed check on anything this build produced",
		archivesVolume, f.Name, key).
		WithHint("%s", containerStaleArchivesVolumeHint(archivesVolume))
}

// containerStaleArchivesVolumeHint is the one wording of the stale-volume
// remedy, shared by the two refusals a populated shared volume can reach, so
// they cannot drift apart.
//
// It names the volume and the command, because neither appears anywhere else
// an operator will look: there is no flag, no config key and no DEBARK_*
// variable for the volume, `debark store gc` operates on the
// content-addressed object store and not on this, and the volume's name is a
// constant in this package.
//
// It deliberately does not promise the build will then do what it was asked.
// Emptying the volume removes the stale files and the next build repopulates
// it; that is the whole remedy available today, and saying so is worth more
// than a hint that implies a flag exists.
func containerStaleArchivesVolumeHint(volume string) string {
	return "This is the shared .deb cache every container build on this machine writes into, and nothing ever " +
		"empties it, so one file left by an earlier build of a different target fails every later build here, " +
		"whatever target, architecture or packages it asks for. Empty it and build again: " +
		"`docker volume rm " + volume + "` (or `podman volume rm " + volume + "`). " +
		"Nothing was tampered with and no bundle was produced. There is no flag, config key or environment " +
		"variable that points this build at a different volume yet."
}

// containerReverifyFiles independently recomputes each envelope file's
// digest from the bytes actually sitting in hostArchivesDir, rather than
// trusting the envelope's self-reported sha256: the envelope was produced by
// the very process whose mount we are trying to catch a truncation in, so
// only re-hashing on the host, after the container has exited, closes the
// loop (the contract brief: "re-verify every staged file's digest on the host").
func containerReverifyFiles(env containerEnvelope, hostArchivesDir string) error {
	for _, f := range env.Files {
		p := filepath.Join(hostArchivesDir, f.Filename)
		sum, size, err := digest.SHA256File(p)
		if err != nil {
			return dferr.Wrap(dferr.Verification, err,
				"container: staged file for %s missing or unreadable on the host", f.Name)
		}
		if !digest.Equal(sum, f.SHA256) {
			return dferr.New(dferr.Verification,
				"container: %s digest mismatch: envelope says %s, host file re-hashes to %s", f.Filename, f.SHA256, sum)
		}
		if size != f.Size {
			return dferr.New(dferr.Verification,
				"container: %s size mismatch: envelope says %d, host file is %d bytes", f.Filename, f.Size, size)
		}
	}
	return nil
}

// containerReverifyExternalFiles is containerReverifyFiles for the half of
// the plan that function structurally cannot see.
//
// containerReverifyFiles iterates env.Files, and the envelope's files[] is
// scoped to "what is actually on disk in --archives"; an external/vendor .deb
// is used in place from the read-only --external-repo mount and never lands
// there, so containerCrossCheckEnvelope skips it and files[] never lists it.
// The result was that an external selection was the one kind of selection no
// host-side check ever touched: not the plan/files digest cross-check, not
// the post-exit re-hash. This closes that, using the only host-side truth
// available for it -- the external staging directory core/engine itself
// populated before the container ever ran.
//
// The digest comparison is conditional because Selection.SHA256 is not
// guaranteed non-empty by any contract this backend can enforce; the
// existence and readability check is not, because a selection naming a file
// that is not in the staging directory is either a truncated mount or an
// envelope describing something the host never staged, and core/engine would
// otherwise discover it only as a bare "no such file" while moving files
// around.
//
// dir == "" is an ERROR when the plan carries an external selection, not the
// pass it used to be. This function's own doc comment used to say that with
// no ExternalRepoDir "containerRewriteStagedPaths leaves external selections
// pointing into --archives and containerReverifyFiles already covers them",
// and that claim was simply false: containerReverifyFiles iterates
// env.Files, and files[] never lists an external selection -- that is the
// exact reason this function exists. So with ExternalRepoDir empty, a
// reason:external selection was checked by nothing at all: skipped by
// containerCrossCheckEnvelope, returned early on here, invisible to
// containerReverifyFiles. Measured: an envelope with a reason:external
// selection naming a file that does not exist, sha256 all zeros, was
// accepted and returned to the engine with a StagedPath. The host had staged
// no external repository, so there is no directory in which such a selection
// could be true; saying so is the only honest answer.
func containerReverifyExternalFiles(plan *resolve.Plan, dir string) error {
	if dir == "" {
		for i := range plan.Selections {
			if sel := plan.Selections[i]; sel.Reason == lock.ReasonExternal {
				return dferr.New(dferr.Verification,
					"container: plan selects %s (%s) with reason %q, but this build staged no external repository directory, "+
						"so there is nothing host-side that could hold its bytes and nothing that could verify them",
					sel.Name, sel.Arch, sel.Reason)
			}
		}
		return nil
	}
	for i := range plan.Selections {
		sel := &plan.Selections[i]
		if sel.Reason != lock.ReasonExternal || sel.Filename == "" {
			continue
		}
		if err := containerCheckEnvelopeFilename("external plan selection "+sel.Name, sel.Filename); err != nil {
			return err
		}
		sum, size, err := digest.SHA256File(filepath.Join(dir, sel.Filename))
		if err != nil {
			return dferr.Wrap(dferr.Verification, err,
				"container: external staged file for %s missing or unreadable on the host", sel.Name)
		}
		if sel.SHA256 != "" && !digest.Equal(sum, sel.SHA256) {
			return dferr.New(dferr.Verification,
				"container: %s digest mismatch: envelope says %s, host file re-hashes to %s", sel.Filename, sel.SHA256, sum)
		}
		if sel.Size != 0 && size != sel.Size {
			return dferr.New(dferr.Verification,
				"container: %s size mismatch: envelope says %d, host file is %d bytes", sel.Filename, sel.Size, size)
		}
	}
	return nil
}

// containerRewriteStagedPaths rewrites every selection's StagedPath from
// whatever the container-internal `debark resolve` process reported --
// necessarily a container-side path such as "/archives/foo.deb", meaningless
// once the container has exited -- to the corresponding host-side path.
//
// ADR-013 states this plainly: "The container's mounted workspace becomes a
// small but real inter-process contract -- staged .deb files at agreed
// paths, matching resolve.Plan.Selections[].StagedPath -- that both the
// container-internal resolve invocation and the host-side ingestion step
// must agree on." The host-side ingestion step (core/engine, downstream of
// this backend) opens StagedPath directly to read bytes into the store; a
// container path left in there would make every file silently unreadable on
// the host. External selections (Reason == lock.ReasonExternal) are staged
// from ExternalRepoDir -- already a host path the caller supplied -- rather
// than from ArchivesDir.
func containerRewriteStagedPaths(plan *resolve.Plan, archivesDir, externalRepoDir string) error {
	for i := range plan.Selections {
		sel := &plan.Selections[i]
		if sel.Filename == "" {
			continue
		}
		// Re-checked here even though containerCrossCheckEnvelope already
		// rejected a bad name: this is the function that actually builds the
		// path, it is exported-within-package and called from more than one
		// place over time, and the cost of being wrong is a file MOVED off
		// the builder rather than a bad error message. The guard belongs
		// next to the join.
		if err := containerCheckEnvelopeFilename("plan selection "+sel.Name, sel.Filename); err != nil {
			return err
		}
		if sel.Reason == lock.ReasonExternal && externalRepoDir != "" {
			sel.StagedPath = filepath.Join(externalRepoDir, sel.Filename)
			continue
		}
		sel.StagedPath = filepath.Join(archivesDir, sel.Filename)
	}
	return nil
}

// --- image apt/dpkg version discovery --------------------------------

// containerImageProbeKey caches by every input that could change the answer.
type containerImageProbeKey struct {
	Runtime  string
	Image    string
	Platform string
}

type containerImageProbeResult struct {
	APTVersion  string
	DpkgVersion string
	DistroID    string
	VersionID   string
}

// containerImageProbeCache caches containerProbeImageVersions results for
// the life of this process, keyed by (runtime, image, platform).
//
// Probe must stay cheap -- it runs during backend auto-selection -- but the
// only way to learn an image's apt/dpkg versions is to actually run it once
// (Capabilities doc: "discovered on first use and cached"). This cache means
// that cost is paid at most once per distinct (runtime, image, platform)
// triple per process, no matter how many containerBackend values
// NewContainerBackend hands out or how many times Probe is called on them.
// Only successful probes are cached: a transient failure (daemon not
// started yet, a slow pull) should be retried on the next call, not
// remembered as permanent for the rest of the process.
var containerImageProbeCache sync.Map // containerImageProbeKey -> containerImageProbeResult

func containerCachedProbeImageVersions(ctx context.Context, runtimePath, runtimeName, image, platform string) (containerImageProbeResult, error) {
	key := containerImageProbeKey{Runtime: runtimeName, Image: image, Platform: platform}
	if v, ok := containerImageProbeCache.Load(key); ok {
		return v.(containerImageProbeResult), nil
	}
	apt, dpkg, distroID, versionID, err := containerProbeImageVersions(ctx, runtimePath, image, platform)
	if err != nil {
		return containerImageProbeResult{}, err
	}
	res := containerImageProbeResult{APTVersion: apt, DpkgVersion: dpkg, DistroID: distroID, VersionID: versionID}
	containerImageProbeCache.Store(key, res)
	return res, nil
}

// containerProbeImageVersions runs the image once, printing three
// unambiguously delimited pieces of output, to discover the apt/dpkg
// versions and distro identity it carries.
func containerProbeImageVersions(ctx context.Context, runtimePath, image, platform string) (aptVersion, dpkgVersion, distroID, versionID string, err error) {
	const script = `echo "DEBARK_APT:$(apt-get -v 2>/dev/null | head -n1)"; echo "DEBARK_DPKG:$(dpkg --version 2>/dev/null | head -n1)"; echo DEBARK_OSRELEASE_BEGIN; cat /etc/os-release 2>/dev/null; echo DEBARK_OSRELEASE_END`
	argv := containerBuildRunArgv(platform, nil, nil, image, []string{"sh", "-c", script})
	res, rerr := execContainer(ctx, runtimePath, argv, nil)
	if rerr != nil {
		return "", "", "", "", rerr
	}
	if res.ExitCode != 0 {
		return "", "", "", "", containerClassifyRunResult(res, platform)
	}

	var osReleaseLines []string
	inOSRelease := false
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "DEBARK_APT:"):
			aptVersion = containerParseAptGetVersion(strings.TrimPrefix(line, "DEBARK_APT:"))
		case strings.HasPrefix(line, "DEBARK_DPKG:"):
			dpkgVersion = containerParseDpkgVersion(strings.TrimPrefix(line, "DEBARK_DPKG:"))
		case line == "DEBARK_OSRELEASE_BEGIN":
			inOSRelease = true
		case line == "DEBARK_OSRELEASE_END":
			inOSRelease = false
		case inOSRelease:
			osReleaseLines = append(osReleaseLines, line)
		}
	}
	distroID, versionID = containerParseOSRelease([]byte(strings.Join(osReleaseLines, "\n")))
	if aptVersion == "" || dpkgVersion == "" {
		return aptVersion, dpkgVersion, distroID, versionID, dferr.New(dferr.Environment,
			"container: could not determine apt/dpkg versions from image %s (apt=%q dpkg=%q)", image, aptVersion, dpkgVersion)
	}
	return aptVersion, dpkgVersion, distroID, versionID, nil
}

var (
	containerAptVersionRe  = regexp.MustCompile(`^apt\s+(\S+)`)
	containerDpkgVersionRe = regexp.MustCompile(`version\s+(\S+)`)
)

// containerParseAptGetVersion extracts "2.6.1" from apt-get -v's first line,
// e.g. "apt 2.6.1 (amd64)".
func containerParseAptGetVersion(line string) string {
	line = strings.TrimSpace(line)
	if m := containerAptVersionRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// containerParseDpkgVersion extracts "1.21.22" from dpkg --version's first
// line, e.g. "Debian 'dpkg' package management program version 1.21.22
// (amd64)."
func containerParseDpkgVersion(line string) string {
	line = strings.TrimSpace(line)
	m := containerDpkgVersionRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return strings.TrimRight(m[1], ".")
}

// containerParseOSRelease extracts ID and VERSION_ID from an /etc/os-release
// file's bytes.
func containerParseOSRelease(data []byte) (id, versionID string) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			versionID = v
		}
	}
	return id, versionID
}

// --- named volume copy-out (experiment E5) ---------------------------

// containerCopyVolumeToHost copies everything from a named volume into a
// host directory by running one throwaway container that mounts the volume
// read-only and the host directory read-write and does a plain recursive
// copy.
//
// This is the extra step a named volume costs relative to a bind mount: the
// volume's bytes live inside the container runtime's own storage (invisible
// as a host path), so the caller -- which must hand back a plain host
// directory per the Backend.Resolve contract, since every downstream stage
// after resolution runs on the host -- needs them copied out once the
// in-container work is done.
func containerCopyVolumeToHost(ctx context.Context, runtimePath, image, volume, hostDir string) error {
	src, err := containerMountSource(hostDir)
	if err != nil {
		return err
	}
	argv := []string{"run", "--rm",
		"-v", volume + ":/from:ro",
		"-v", src + ":/to",
		image,
		"sh", "-c", "cp -a /from/. /to/ 2>/dev/null || cp -r /from/. /to/",
	}
	// Through execContainerFn, not execContainer directly: this is the only
	// container invocation on the Resolve path, and it runs on exactly the
	// platforms this backend exists for (Windows and macOS default to a named
	// volume -- see storeVolumeChoice). Calling the unindirected execContainer
	// here made every step of Resolve after the envelope untestable on those
	// hosts, because a test with no docker on PATH died here rather than at
	// the thing it was exercising.
	res, err := execContainerFn(ctx, runtimePath, argv, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return dferr.New(dferr.Environment, "container: copying named volume %s to host failed: %s",
			volume, containerLastLines(res.combined(), 4))
	}
	return nil
}
