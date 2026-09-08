// Package harness is the container orchestration layer for the
// integration matrix. It owns everything that talks
// to Docker: starting target/builder containers, executing commands inside
// them, copying files in and out, and tearing every container down again —
// even on failure, even on a panic, even under Ctrl-C.
//
// This package never imports core/ or internal/cli: it drives the product
// exactly as an operator would, over process boundaries, by invoking a real
// `debark` binary inside real containers. That is deliberate — the whole
// point of the matrix is to prove the product from the outside, the way an air-gapped
// operator experiences it, not to call Go functions across a package that
// fourteen other packages are actively rewriting underneath us.
//
// Every container this package creates is named "dfe2e-<run>-<slug>-<role>"
// and labeled "debark.e2e.run=<run>" so cleanup can be scoped precisely on
// a shared Docker host — this harness must never touch a container it did not
// create. The same naming and labelling covers the one kind of IMAGE it
// creates (the per-row commit of the snapshotted target machine; see
// scenario.go's commitTargetImage), because an image left behind is a leak
// too, and a quieter one: nothing about it stops or expires. Sweep() is the
// backstop for both, for anything a crashed process left behind.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LabelRun is the Docker label key that scopes every container/volume this
// harness creates to one run, so Sweep can clean up precisely.
const LabelRun = "debark.e2e.run"

// LabelMarker is set to "1" on everything this harness creates, independent
// of run id, so a human can find "everything debark's E2E harness ever
// made" with a single docker filter if a run id is lost.
const LabelMarker = "debark.e2e"

// NewRunID returns a short, filesystem- and Docker-name-safe identifier for
// one harness invocation (one `go test` process or one hack/matrix run).
func NewRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "r" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "r" + hex.EncodeToString(b[:])
}

// CmdResult is the outcome of one process execution, whether that process ran
// on the host (docker itself) or inside a container (docker exec). ExitCode
// is data, not an error: a fixture that expects `debark verify` to fail is
// asserting on ExitCode, and Exec/Run only return a non-nil error when the
// harness itself could not learn the outcome (docker missing, context
// deadline, container gone).
type CmdResult struct {
	Argv     []string
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// Combined returns stdout followed by stderr, which is what most substring
// assertions want.
func (r CmdResult) Combined() string { return string(r.Stdout) + string(r.Stderr) }

func (r CmdResult) String() string {
	return fmt.Sprintf("$ %s\n[exit %d in %s]\nstdout:\n%s\nstderr:\n%s",
		strings.Join(r.Argv, " "), r.ExitCode, r.Duration, string(r.Stdout), string(r.Stderr))
}

// runHost executes a command on the host (the Windows/Linux/macOS machine
// running the harness) — used only for the `docker` CLI itself, never for
// anything product-related. A non-nil error means docker could not be
// invoked at all (not found, context cancelled); a non-zero exit from docker
// is reported via CmdResult.ExitCode with a nil error, mirroring os/exec's
// own contract but making the common case (skip vs. genuine environment
// failure) easy to branch on without a type assertion.
func runHost(ctx context.Context, stdin []byte, name string, args ...string) (CmdResult, error) {
	start := time.Now()
	// Argument vector, no shell: `name` is always the literal "docker" (this
	// helper's doc comment above is the contract -- it exists only to invoke
	// the docker CLI) and args is a []string, one element per argv slot. The
	// values come from the harness's own scenario definitions, not from
	// anything the product under test produces. Same structural argument as
	// core/sign/gpg.go's runGPG and .golangci.yml's G204 note.
	// #nosec G702 -- argv vector, harness-controlled args, no shell
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	res := CmdResult{
		Argv:     append([]string{name}, args...),
		Stdout:   outBuf.Bytes(),
		Stderr:   errBuf.Bytes(),
		Duration: time.Since(start),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr) && ctx.Err() != nil:
		// The process did not choose this exit status: exec.CommandContext
		// killed it because the row's deadline expired or the run was
		// cancelled, and on Windows a killed process reports exit 1 — which
		// is dferr's "usage" class. Reporting that as the product's own
		// answer is exactly how hack/matrix/results/README.md's invalid run
		// came to claim nine rows had failed with `exit 1 (usage), want 0`
		// when nothing had returned a usage error at all: the host's apt was
		// hanging and every row was killed at its timeout. A killed process
		// yields no verdict, so this is a harness error, which every call
		// site turns into a blocked row rather than a product failure.
		return res, fmt.Errorf("exec %s: killed after %s before it could report an outcome: %w",
			name, res.Duration.Round(time.Millisecond), ctx.Err())
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		// docker itself could not be started, or the context was cancelled:
		// the harness has no outcome to report, so this is a real error.
		return res, fmt.Errorf("exec %s: %w", name, err)
	}
	return res, nil
}

// Docker runs one `docker` invocation on the host and returns its result.
func Docker(ctx context.Context, args ...string) (CmdResult, error) {
	return runHost(ctx, nil, "docker", args...)
}

// DockerAvailable reports whether the docker CLI can reach a daemon at all,
// with a human-readable reason when it cannot — the first line of "skip
// cleanly and say why" (task point 4).
func DockerAvailable(ctx context.Context) (bool, string) {
	res, err := Docker(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return false, fmt.Sprintf("docker CLI not runnable: %v", err)
	}
	if res.ExitCode != 0 {
		return false, fmt.Sprintf("docker daemon unreachable: %s", strings.TrimSpace(res.Combined()))
	}
	return true, ""
}

// hostMountPath turns an absolute host path into the form Docker Desktop's
// CLI accepts for -v on every platform this harness runs on: forward
// slashes, drive letter kept as-is on Windows (D:/foo/bar). Called directly
// from Go via os/exec, never through a shell, so MSYS path mangling (the
// Git-Bash-specific problem the task's own constraints note refers to) never
// enters into it — that note applies to shell scripts, not to this package.
func hostMountPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("abs path %s: %w", p, err)
	}
	return filepath.ToSlash(abs), nil
}

// ---------------------------------------------------------------------------
// Images and platforms
// ---------------------------------------------------------------------------

// ImageExistsLocally reports whether ref is already present in the local
// image cache, without attempting a pull. Checked first so a nightly run on
// a well-warmed host never pays for a network round trip it does not need.
func ImageExistsLocally(ctx context.Context, ref string) bool {
	res, err := Docker(ctx, "image", "inspect", ref, "--format", "{{.Id}}")
	return err == nil && res.ExitCode == 0
}

// EnsureImage makes ref available locally, pulling it if necessary within
// timeout. It returns a reason (empty on success) suitable for a skip: a
// pull failure here is exactly "a release image ... unavailable" (task point
// 4), not a fixture failure.
func EnsureImage(ctx context.Context, ref string, timeout time.Duration) (ok bool, reason string) {
	if ImageExistsLocally(ctx, ref) {
		return true, ""
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := Docker(pctx, "pull", ref)
	if err != nil {
		if errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return false, fmt.Sprintf("image %s: pull timed out after %s (no registry access?)", ref, timeout)
		}
		return false, fmt.Sprintf("image %s: pull failed to run: %v", ref, err)
	}
	if res.ExitCode != 0 {
		return false, fmt.Sprintf("image %s: pull failed: %s", ref, lastNonEmptyLine(res.Combined()))
	}
	return true, ""
}

var execFormatErrRe = regexp.MustCompile(`(?i)exec format error|no matching manifest|not supported for platform|cannot execute binary`)

// PlatformSupported probes whether containers can actually run for platform
// (e.g. "linux/arm64") on this host: image presence is necessary but not
// sufficient, since QEMU user-mode emulation (binfmt_misc) can be entirely
// unregistered even though the image itself pulls fine — precisely the "arm64
// via emulation where available" case the design calls out, and precisely
// what running one on this development host actually showed. tinyImage
// should be something already small and likely
// cached (e.g. the release image itself, or "busybox").
func PlatformSupported(ctx context.Context, platform, tinyImage string, timeout time.Duration) (ok bool, reason string) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := Docker(pctx, "run", "--rm", "--platform", platform, tinyImage, "true")
	if err != nil {
		if errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return false, fmt.Sprintf("platform %s: probe timed out after %s", platform, timeout)
		}
		return false, fmt.Sprintf("platform %s: probe failed to run: %v", platform, err)
	}
	if res.ExitCode == 0 {
		return true, ""
	}
	combined := res.Combined()
	if execFormatErrRe.MatchString(combined) {
		return false, fmt.Sprintf("platform %s: emulation unavailable (%s) — register QEMU with "+
			"`docker run --privileged --rm tonistiigi/binfmt --install all` on a host you control", platform, lastNonEmptyLine(combined))
	}
	return false, fmt.Sprintf("platform %s: probe failed: %s", platform, lastNonEmptyLine(combined))
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// Containers
// ---------------------------------------------------------------------------

// Mount is one bind mount into a container.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// ContainerOpts configures a new long-lived container (kept alive with `sleep
// infinity` so the scenario can exec into it repeatedly, testcontainers-
// style).
type ContainerOpts struct {
	Image    string
	Platform string // "" = host default
	// Network "none" is the mandatory hardening for every fresh target
	// container ("start a --network none target container").
	// "" leaves Docker's default bridge network (needed for target/builder
	// containers that must reach the real archive or a synthetic repo).
	Network string
	Name    string
	Labels  map[string]string
	Mounts  []Mount
}

// Container is one running, named, labeled container this harness owns.
type Container struct {
	ID   string
	Name string
}

// StartContainer runs a new detached container that stays alive until
// Remove is called.
func StartContainer(ctx context.Context, opts ContainerOpts) (*Container, error) {
	args := []string{"run", "-d", "--name", opts.Name}
	if opts.Platform != "" {
		args = append(args, "--platform", opts.Platform)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	for k, v := range opts.Labels {
		args = append(args, "--label", k+"="+v)
	}
	// Every container gets host.docker.internal, unconditionally: it is how
	// this harness's synthetic third-party repositories (httprepo.go) are
	// reachable from more than one container. Docker Desktop for Windows and
	// macOS wire this up automatically; plain Linux Docker (a nightly CI
	// runner) needs the explicit host-gateway mapping.
	args = append(args, "--add-host=host.docker.internal:host-gateway")
	for _, m := range opts.Mounts {
		hp, err := hostMountPath(m.HostPath)
		if err != nil {
			return nil, err
		}
		spec := hp + ":" + m.ContainerPath
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	// sleep infinity keeps the container alive across many `docker exec`
	// calls without needing the target's own init system or a real service.
	args = append(args, opts.Image, "sleep", "infinity")

	// A handful of concurrent `docker run` calls (a matrix row starts three
	// containers, and several rows run at once) on a Docker host already
	// carrying other, unrelated work can hit a transient daemon error with
	// no useful detail beyond "exit 125: Run 'docker run --help' for more
	// information" — reproduced directly during development under load,
	// absent when the identical command ran alone. A short bounded retry
	// tells that apart from a real, deterministic failure (a bad image
	// reference, an invalid flag) without masking one: those fail identically
	// on every attempt and this still returns their exact error, just after
	// paying for retries that could not have helped.
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err := Docker(ctx, args...)
		switch {
		case err != nil:
			return nil, fmt.Errorf("docker run %s: %w", opts.Name, err)
		case res.ExitCode == 0:
			id := strings.TrimSpace(string(res.Stdout))
			return &Container{ID: id, Name: opts.Name}, nil
		default:
			// The full combined output, not just its last line: docker's own
			// generic "Run 'docker run --help'" trailer is frequently the
			// last line, with the actually useful message just above it.
			lastErr = fmt.Errorf("docker run %s: exit %d: %s", opts.Name, res.ExitCode, strings.TrimSpace(res.Combined()))
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("%w (after %d attempts)", lastErr, maxAttempts)
}

// ExecOpts controls one docker exec.
type ExecOpts struct {
	WorkDir string
	Env     map[string]string
	User    string
	// Stdin, when non-nil, is piped to the command (used by WriteFile).
	Stdin []byte
}

// Exec runs argv[0] with argv[1:] inside the container and returns its
// result. A nil error with a non-zero ExitCode is the normal, expected shape
// for a command a fixture deliberately expects to fail (e.g. `debark
// verify` on a tampered bundle).
func (c *Container) Exec(ctx context.Context, opts ExecOpts, argv ...string) (CmdResult, error) {
	if len(argv) == 0 {
		return CmdResult{}, errors.New("exec: empty argv")
	}
	args := []string{"exec"}
	if opts.Stdin != nil {
		args = append(args, "-i")
	}
	if opts.WorkDir != "" {
		args = append(args, "-w", opts.WorkDir)
	}
	if opts.User != "" {
		args = append(args, "-u", opts.User)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, c.ID)
	args = append(args, argv...)
	res, err := runHost(ctx, opts.Stdin, "docker", args...)
	if err != nil {
		return res, fmt.Errorf("docker exec %s %s: %w", c.Name, argv[0], err)
	}
	res.Argv = argv // report the in-container argv, not the docker wrapper
	return res, nil
}

// MustSucceed is a convenience for setup steps (constructing installed
// state, writing sources) where a non-zero exit is always a harness/
// environment problem, never an expected fixture outcome.
func (c *Container) MustSucceed(ctx context.Context, opts ExecOpts, argv ...string) (CmdResult, error) {
	res, err := c.Exec(ctx, opts, argv...)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s: exit %d: %s", strings.Join(argv, " "), res.ExitCode, lastNonEmptyLine(res.Combined()))
	}
	return res, nil
}

// Shell runs script under `sh -c` inside the container — the escape hatch
// for the multi-step setup shell one-liners target-state construction needs
// (apt-get update && apt-get install -y ..., writing files with heredocs).
func (c *Container) Shell(ctx context.Context, opts ExecOpts, script string) (CmdResult, error) {
	return c.Exec(ctx, opts, "sh", "-c", script)
}

// ShellMust is Shell + MustSucceed's non-zero-is-an-error contract.
func (c *Container) ShellMust(ctx context.Context, opts ExecOpts, script string) (CmdResult, error) {
	res, err := c.Shell(ctx, opts, script)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("sh -c %q: exit %d: %s", script, res.ExitCode, lastNonEmptyLine(res.Combined()))
	}
	return res, nil
}

// CopyIn copies a host file or directory into the container.
func (c *Container) CopyIn(ctx context.Context, hostPath, containerPath string) error {
	res, err := Docker(ctx, "cp", hostPath, c.ID+":"+containerPath)
	if err != nil {
		return fmt.Errorf("docker cp into %s: %w", c.Name, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker cp %s into %s: %s", hostPath, c.Name, lastNonEmptyLine(res.Combined()))
	}
	return nil
}

// CopyOut copies a file or directory out of the container to the host.
func (c *Container) CopyOut(ctx context.Context, containerPath, hostPath string) error {
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	res, err := Docker(ctx, "cp", c.ID+":"+containerPath, hostPath)
	if err != nil {
		return fmt.Errorf("docker cp out of %s: %w", c.Name, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker cp %s:%s out: %s", c.Name, containerPath, lastNonEmptyLine(res.Combined()))
	}
	return nil
}

// WriteFile writes content to containerPath inside the container by staging
// it through a host temp file and `docker cp` — simpler and more reliable
// than piping through exec's stdin for content that may contain arbitrary
// bytes (a tampered .deb, a control file with odd characters).
func (c *Container) WriteFile(ctx context.Context, containerPath string, content []byte) error {
	tmp, err := os.CreateTemp("", "dfe2e-write-*")
	if err != nil {
		return err
	}
	// Best-effort cleanup of the host-side staging file. It is a temp file we
	// created and only `docker cp` reads; failing to unlink it leaves litter in
	// os.TempDir, which is not worth failing a test over or masking the real
	// error with.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Ensure the destination directory exists before the copy.
	if dir := pathDir(containerPath); dir != "" && dir != "." {
		if _, err := c.MustSucceed(ctx, ExecOpts{}, "mkdir", "-p", dir); err != nil {
			return err
		}
	}
	return c.CopyIn(ctx, tmp.Name(), containerPath)
}

// pathDir is a tiny POSIX dirname (containerPath is always a Linux path
// inside the container, so path/filepath's Windows-flavoured Dir on this
// host would give the wrong answer).
func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// Remove force-removes the container. Safe to call more than once and safe
// to call on a container that never fully started; it never returns an
// error for "already gone", only for a docker CLI that could not run at all,
// so callers can always `defer container.Remove(ctx)` unconditionally.
func (c *Container) Remove(ctx context.Context) error {
	if c == nil || c.ID == "" {
		return nil
	}
	res, err := Docker(ctx, "rm", "-f", "-v", c.ID)
	if err != nil {
		return fmt.Errorf("docker rm %s: %w", c.Name, err)
	}
	if res.ExitCode != 0 && !strings.Contains(strings.ToLower(res.Combined()), "no such container") {
		return fmt.Errorf("docker rm %s: %s", c.Name, lastNonEmptyLine(res.Combined()))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Images this harness creates
// ---------------------------------------------------------------------------

// CommitContainer freezes c's current filesystem into a new local image
// named ref, stamped with labels so Sweep can find it again if this process
// dies before its owner's cleanup runs.
//
// It has exactly one caller — scenario.go's commitTargetImage, which turns
// the snapshotted state container into the machine the bundle is then
// installed on. Read that function before touching either of them: an image
// is the only way to hand a second, brand-new, --network none container the
// *same* dpkg state, foreign architectures, holds and apt configuration the
// snapshot captured, and installing a closed-world bundle on any other
// machine measures nothing the fixture claims.
//
// Labels go through `--change LABEL`, the only form `docker commit` accepts,
// and each value is Go-quoted so a value containing a space could never
// split into two labels (values are sanitised today; the quoting is what
// keeps that from mattering). They also become the DEFAULT labels of every
// container later started from this image — harmless, because they carry
// this same run id, and because StartContainer stamps its own role label on
// top, so a fresh target still reports role=freshtarget rather than
// inheriting the image's.
//
// The commit is not given an explicit --pause flag: docker pauses the
// container by default for the duration, which is exactly what is wanted
// (nothing must be able to write to the filesystem being captured), and
// spelling out the default would only invite someone to flip it.
func CommitContainer(ctx context.Context, c *Container, ref string, labels map[string]string) error {
	if c == nil || c.ID == "" {
		return errors.New("docker commit: no container to commit")
	}
	if ref == "" {
		return errors.New("docker commit: empty image reference")
	}
	res, err := Docker(ctx, commitArgs(c.ID, ref, labels)...)
	if err != nil {
		return fmt.Errorf("docker commit %s as %s: %w", c.Name, ref, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker commit %s as %s: exit %d: %s", c.Name, ref, res.ExitCode, lastNonEmptyLine(res.Combined()))
	}
	return nil
}

// commitArgs builds the `docker commit` argv. Split out from CommitContainer
// so the one thing about that call which is easy to get silently wrong — the
// labels, which are what Sweep finds a leaked image by — is testable without
// a Docker daemon.
func commitArgs(containerID, ref string, labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	// Sorted so the argv recorded in a row's transcript is stable between
	// runs; Go map order would otherwise make two identical commits look
	// different to anyone diffing two transcripts.
	sort.Strings(keys)

	args := make([]string, 0, 3+2*len(keys))
	args = append(args, "commit")
	for _, k := range keys {
		// strconv.Quote, not bare concatenation: `--change` is parsed as a
		// Dockerfile instruction, so an unquoted value containing a space
		// would silently become two labels, one of them named after a
		// fragment of the other's value. Every value this harness passes is
		// sanitised today; the quoting is what stops that from being load
		// bearing.
		args = append(args, "--change", "LABEL "+k+"="+strconv.Quote(labels[k]))
	}
	return append(args, containerID, ref)
}

// imageGoneRe matches the daemon's several spellings of "that image is not
// here", so RemoveImage can treat them as success. Both forms are reachable
// from one call: `docker image rm` answers an unknown *tag* with "No such
// image" and an unknown *reference* with "reference does not exist", and a
// concurrent sweep can turn either one into the answer for a reference that
// existed a moment ago.
var imageGoneRe = regexp.MustCompile(`(?i)no such image|reference does not exist`)

// RemoveImage removes a local image by reference. Like Container.Remove it
// treats "already gone" as success, so a cleanup path can call it
// unconditionally on a reference whose commit may never have completed —
// which is precisely how scenario.go uses it (it registers the reference
// before committing, so a commit that fails halfway still gets swept).
func RemoveImage(ctx context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	res, err := Docker(ctx, "image", "rm", "-f", ref)
	if err != nil {
		return fmt.Errorf("docker image rm %s: %w", ref, err)
	}
	if res.ExitCode != 0 && !imageGoneRe.MatchString(res.Combined()) {
		return fmt.Errorf("docker image rm %s: %s", ref, lastNonEmptyLine(res.Combined()))
	}
	return nil
}

// Sweep force-removes every container labeled with this run id, and then
// every image this run committed (scenario.go commits one per row: the
// snapshotted target machine the bundle is installed on). It is the backstop
// called from TestMain / hack/matrix's top-level defer so a killed process or
// an unhandled panic never leaves anything behind on a Docker host that
// fifteen other things are actively using (contract-brief.md: this tree has
// many concurrent changes; the Docker daemon on this development host is
// shared with unrelated long-running projects, observed directly during
// development). It only ever touches objects
// carrying LabelRun=runID; it never runs a bare prune.
//
// Containers are swept before images, and that order is required rather than
// tidy: `docker image rm -f` on an image some container still references only
// untags it, leaving the layers behind as a dangling <none> image — a leak
// wearing a different name, and one no label filter can find afterwards.
//
// removed counts CONTAINERS only, deliberately, even though images are swept
// too. hack/matrix renders this number as "swept %d container(s) ... that a
// row's own cleanup did not remove", and this package must not quietly make
// another file's output say something untrue. An image that could not be
// removed is not lost either way: it is reported through err, which
// hack/matrix prints along with the exact filter to inspect by hand.
func Sweep(ctx context.Context, runID string) (removed int, err error) {
	filter := LabelRun + "=" + runID
	res, lerr := Docker(ctx, "ps", "-aq", "--filter", "label="+filter)
	if lerr != nil {
		return 0, lerr
	}
	if res.ExitCode != 0 {
		// Without this, a failed listing yields an empty id set and Sweep
		// reports "0 removed, no error" — indistinguishable from "there was
		// nothing to clean up", while containers stay behind on a shared
		// Docker host.
		return 0, fmt.Errorf("sweep: docker ps: exit %d: %s", res.ExitCode, lastNonEmptyLine(res.Combined()))
	}
	ids := strings.Fields(string(res.Stdout))
	if len(ids) > 0 {
		rmArgs := append([]string{"rm", "-f", "-v"}, ids...)
		rres, rerr := Docker(ctx, rmArgs...)
		if rerr != nil {
			return 0, rerr
		}
		if rres.ExitCode != 0 {
			return 0, fmt.Errorf("sweep: docker rm: %s", lastNonEmptyLine(rres.Combined()))
		}
	}
	// Reached even when no container matched: a row that crashed between
	// `docker commit` and its own cleanup can leave an image with no
	// container of its own still around, and an early return here would step
	// straight over it.
	if imgErr := sweepImages(ctx, runID); imgErr != nil {
		return len(ids), imgErr
	}
	return len(ids), nil
}

// sweepImages removes every image labeled with this run id. Separated from
// Sweep only so the container path above stays readable; it is not exported
// because there is no situation in which images should be swept while the
// containers holding them are left running.
func sweepImages(ctx context.Context, runID string) error {
	filter := LabelRun + "=" + runID
	res, err := Docker(ctx, "image", "ls", "-q", "--no-trunc", "--filter", "label="+filter)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// Same reasoning as the container listing above: a listing that
		// failed must not read as "there were no images".
		return fmt.Errorf("sweep: docker image ls: exit %d: %s", res.ExitCode, lastNonEmptyLine(res.Combined()))
	}
	// `docker image ls -q` prints one line per TAG, so an image carrying two
	// tags appears twice; passing the same id to `docker image rm` twice
	// makes the second removal fail with "No such image" and turns a clean
	// sweep into a reported error.
	seen := map[string]bool{}
	var ids []string
	for _, id := range strings.Fields(string(res.Stdout)) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rmArgs := append([]string{"image", "rm", "-f"}, ids...)
	rres, rerr := Docker(ctx, rmArgs...)
	if rerr != nil {
		return rerr
	}
	if rres.ExitCode != 0 && !imageGoneRe.MatchString(rres.Combined()) {
		return fmt.Errorf("sweep: docker image rm: %s", lastNonEmptyLine(rres.Combined()))
	}
	return nil
}
