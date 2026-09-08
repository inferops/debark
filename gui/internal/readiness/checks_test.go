package readiness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeRunner answers from a table keyed on the executable's base name, so a
// test can describe a machine — "docker is here and its daemon is down" —
// without one being installed.
type fakeRunner struct {
	mu    sync.Mutex
	out   map[string]CommandOutput
	calls []string
}

func newFakeRunner(out map[string]CommandOutput) *fakeRunner {
	return &fakeRunner{out: out}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) CommandOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(append([]string{filepath.Base(name)}, args...), " "))
	if o, ok := f.out[strings.TrimSuffix(filepath.Base(name), ".exe")]; ok {
		return o
	}
	return CommandOutput{ExitCode: -1, StartErr: errors.New("no such command")}
}

func (f *fakeRunner) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func slowRunner(ctx context.Context, _ string, _ ...string) CommandOutput {
	<-ctx.Done()
	return CommandOutput{ExitCode: -1, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
}

// lookPathIn describes a PATH containing exactly the named executables.
func lookPathIn(names ...string) func(string) (string, error) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return filepath.Join("/usr/bin", name), nil
		}
		return "", exec.ErrNotFound
	}
}

// ---------------------------------------------------------------------------
// debark binary
// ---------------------------------------------------------------------------

func TestDecideBinary(t *testing.T) {
	cases := []struct {
		name         string
		obs          binaryObservation
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
		wantAction   bool
	}{
		{
			name:         "not installed",
			obs:          binaryObservation{LocateErr: errors.New("debark is not on PATH")},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityBlocking,
			wantMentions: "was not found",
		},
		{
			name:         "found and answering",
			obs:          binaryObservation{Path: "/usr/bin/debark", Located: "on PATH", Version: "1.2.0", VersionOut: CommandOutput{Stdout: `{"version":"1.2.0"}`}},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "debark 1.2.0",
		},
		{
			name:         "found but hangs",
			obs:          binaryObservation{Path: "/usr/bin/debark", VersionOut: CommandOutput{TimedOut: true, ExitCode: -1}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "did not answer",
			wantAction:   true,
		},
		{
			name:         "something else called debark",
			obs:          binaryObservation{Path: "/usr/bin/debark", VersionOut: CommandOutput{ExitCode: 2, Stderr: "unknown command"}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "did not report a debark version",
			wantAction:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideBinary(c.obs)
			assertRow(t, got, c.wantStatus, c.wantSeverity, c.wantMentions, c.wantAction)
		})
	}
}

// TestBinaryCheckUsesSuppliedPath pins the seam to internal/cliadapter: when
// the application has already located the binary, this package must not go
// looking for a second answer.
func TestBinaryCheckUsesSuppliedPath(t *testing.T) {
	fr := newFakeRunner(map[string]CommandOutput{"debark": {Stdout: `{"name":"debark","version":"9.9.9"}`}})
	o := Options{
		BinaryPath: filepath.Join("somewhere", "else", "debark"),
		Runner:     fr.run,
		Locator: func(context.Context) (string, error) {
			t.Error("the locator must not run when BinaryPath is supplied")
			return "", errors.New("unreachable")
		},
	}.withDefaults()

	got := binaryCheck(context.Background(), o)
	if got.Status != StatusOK || !strings.Contains(got.Summary, "9.9.9") {
		t.Fatalf("got %+v", got)
	}
	if calls := fr.seen(); len(calls) != 1 || !strings.HasPrefix(calls[0], "debark version --json") {
		t.Errorf("probed with %v, want a single `debark version --json`", calls)
	}
}

// ---------------------------------------------------------------------------
// container runtime
// ---------------------------------------------------------------------------

func TestDecideContainer(t *testing.T) {
	found := func(name string, out CommandOutput) runtimeProbe {
		return runtimeProbe{Name: name, Path: "/usr/bin/" + name, Probed: true, Out: out}
	}
	absent := func(name string) runtimeProbe { return runtimeProbe{Name: name} }

	cases := []struct {
		name         string
		obs          containerObservation
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
	}{
		{
			name:         "nothing installed",
			obs:          containerObservation{Runtimes: []runtimeProbe{absent("docker"), absent("podman")}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "No container runtime is installed",
		},
		{
			name:         "docker running",
			obs:          containerObservation{Runtimes: []runtimeProbe{found("docker", CommandOutput{Stdout: "Server Version: 24.0.7\n"})}},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "Docker is installed and its daemon is answering",
		},
		{
			name: "docker down, podman up",
			obs: containerObservation{Runtimes: []runtimeProbe{
				found("docker", CommandOutput{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock."}),
				found("podman", CommandOutput{Stdout: "version:\n  Version: 4.9.3\n"}),
			}},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "Podman is installed and its daemon is answering",
		},
		{
			name: "installed but not running",
			obs: containerObservation{Runtimes: []runtimeProbe{
				found("docker", CommandOutput{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"}),
				absent("podman"),
			}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "its daemon is not running",
		},
		{
			name: "the docker.sock group problem",
			obs: containerObservation{
				Username: "alice",
				Runtimes: []runtimeProbe{found("docker", CommandOutput{ExitCode: 1, Stderr: "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"})},
			},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "not permitted to talk to it",
		},
		{
			name:         "wedged socket",
			obs:          containerObservation{Runtimes: []runtimeProbe{found("docker", CommandOutput{TimedOut: true, ExitCode: -1})}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "did not answer within the timeout",
		},
		{
			name:         "something else went wrong",
			obs:          containerObservation{Runtimes: []runtimeProbe{found("docker", CommandOutput{ExitCode: 1, Stderr: "context deadline exceeded from the api"})}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "did not answer successfully",
		},
		{
			name:         "found but never probed is not a pass",
			obs:          containerObservation{Runtimes: []runtimeProbe{{Name: "docker", Path: "/usr/bin/docker"}}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "Docker",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideContainer(c.obs)
			assertRow(t, got, c.wantStatus, c.wantSeverity, c.wantMentions, false)
		})
	}
}

// TestContainerPermissionActionNamesTheUser checks the one remedy that has to
// be specific to be any use: a generic "add yourself to the docker group" is
// advice, an exact usermod line is a fix.
func TestContainerPermissionActionNamesTheUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the docker group problem is a unix socket problem")
	}
	got := decideContainer(containerObservation{
		Username: "alice",
		Runtimes: []runtimeProbe{{Name: "docker", Path: "/usr/bin/docker", Probed: true,
			Out: CommandOutput{ExitCode: 1, Stderr: "permission denied while trying to connect"}}},
	})
	if got.Action == nil {
		t.Fatal("no action offered for the docker group problem")
	}
	if want := "sudo usermod -aG docker alice"; got.Action.String() != want {
		t.Errorf("action = %q, want %q", got.Action.String(), want)
	}
	if !got.Action.Elevated {
		t.Error("usermod needs elevation and the action must say so")
	}
	if got.Action.Note == "" {
		t.Error("group membership only applies to a new session; the action must say so")
	}
}

func TestContainerPermissionOmitsActionWithoutAUsername(t *testing.T) {
	got := decideContainer(containerObservation{
		Runtimes: []runtimeProbe{{Name: "docker", Path: "/usr/bin/docker", Probed: true,
			Out: CommandOutput{ExitCode: 1, Stderr: "permission denied while trying to connect"}}},
	})
	if got.Action != nil {
		t.Errorf("offered %q with no user to name", got.Action.String())
	}
	if got.Remedy == "" {
		t.Error("dropping the action must not drop the sentence")
	}
}

// TestContainerInstallActionNeedsAnInstaller: a button that runs apt-get on a
// machine with no apt-get is worse than no button.
func TestContainerInstallActionNeedsAnInstaller(t *testing.T) {
	none := containerObservation{Runtimes: []runtimeProbe{{Name: "docker"}, {Name: "podman"}}}
	if got := decideContainer(none); got.Action != nil {
		t.Errorf("offered %q with no package manager present", got.Action.String())
	}

	withInstaller := none
	withInstaller.HasAPTGet = true
	withInstaller.HasWinget = true
	got := decideContainer(withInstaller)
	switch runtime.GOOS {
	case "linux", "windows":
		if got.Action == nil {
			t.Fatalf("no install action offered on %s with an installer available", runtime.GOOS)
		}
		if len(got.Action.Command) == 0 {
			t.Error("an action must carry the exact command")
		}
	default:
		if got.Action != nil {
			t.Errorf("offered %q on an unsupported platform", got.Action.String())
		}
	}
}

// TestProbeContainerStopsAtTheFirstWorkingRuntime keeps the cold-start cost
// down: probing podman after docker answered would add a whole timeout to
// learn nothing.
func TestProbeContainerStopsAtTheFirstWorkingRuntime(t *testing.T) {
	fr := newFakeRunner(map[string]CommandOutput{
		"docker": {Stdout: "Server Version: 24.0.7"},
		"podman": {Stdout: "Version: 4.9.3"},
	})
	o := Options{Runner: fr.run, LookPath: lookPathIn("docker", "podman")}.withDefaults()

	obs := probeContainer(context.Background(), o)
	if len(obs.Runtimes) != 1 || obs.Runtimes[0].Name != "docker" {
		t.Fatalf("probed %+v, want docker only", obs.Runtimes)
	}
	for _, c := range fr.seen() {
		if strings.HasPrefix(c, "podman") {
			t.Errorf("probed podman after docker answered: %v", fr.seen())
		}
	}
}

// ---------------------------------------------------------------------------
// signing key
// ---------------------------------------------------------------------------

func TestDecideSigningKey(t *testing.T) {
	cases := []struct {
		name         string
		obs          signingKeyObservation
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
		wantAction   bool
	}{
		{
			name:         "no key yet",
			obs:          signingKeyObservation{Path: "/home/a/.config/debark/operator.key"},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityInfo,
			wantMentions: "built unsigned",
			wantAction:   true,
		},
		{
			name:         "key in place",
			obs:          signingKeyObservation{Path: "/k/operator.key", Exists: true, PublicPath: "/k/operator.pub", PublicExists: true},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "signing key is in place",
		},
		{
			name:         "private key with no public key beside it",
			obs:          signingKeyObservation{Path: "/k/operator.key", Exists: true, PublicPath: "/k/operator.pub"},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "signing key is in place",
		},
		{
			name:         "delegated to gpg",
			obs:          signingKeyObservation{Ref: "gpg:DEADBEEF"},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "delegated to gpg",
		},
		{
			name:         "delegated to a plugin",
			obs:          signingKeyObservation{Ref: "plugin:yubikey"},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "signer plugin",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideSigningKey(c.obs)
			assertRow(t, got, c.wantStatus, c.wantSeverity, c.wantMentions, c.wantAction)
		})
	}
}

// TestMissingKeyIsNeverABlocker states the product rule in a test: an operator
// with no key can do everything except sign, so this row must never gate.
func TestMissingKeyIsNeverABlocker(t *testing.T) {
	got := decideSigningKey(signingKeyObservation{Path: "/k/operator.key"})
	if got.Severity == SeverityBlocking {
		t.Fatal("a missing signing key must not block a build")
	}
	if got.Action == nil {
		t.Fatal("a missing key is an offer to run keygen, so it must carry the command")
	}
	if want := "debark keygen --out /k/operator.key"; got.Action.String() != want {
		t.Errorf("action = %q, want %q", got.Action.String(), want)
	}
	if got.Action.Elevated {
		t.Error("keygen writes a file in the operator's own config directory; it must not claim elevation")
	}
}

func TestProbeSigningKeyReadsDebarkSign(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "custom.key")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom.pub"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DEBARK_SIGN", key)
	obs := probeSigningKey(Options{SigningKeyPath: filepath.Join(dir, "ignored.key")}.withDefaults())
	if obs.Path != key || !obs.Exists || !obs.PublicExists {
		t.Fatalf("obs = %+v, want the DEBARK_SIGN key with its .pub", obs)
	}

	t.Setenv("DEBARK_SIGN", "gpg:ABCD")
	obs = probeSigningKey(Options{SigningKeyPath: key}.withDefaults())
	if obs.Path != key {
		t.Errorf("a gpg ref must not be treated as a path: %+v", obs)
	}
	if got := decideSigningKey(obs); !strings.Contains(got.Summary, "gpg") {
		t.Errorf("summary = %q, want it to report the gpg delegation", got.Summary)
	}
}

func TestIsFileRef(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"gpg:DEADBEEF", false},
		{"plugin:yubikey", false},
		{"/home/a/op.key", true},
		{`C:\keys\op.key`, true},
	}
	for _, c := range cases {
		if got := isFileRef(c.in); got != c.want {
			t.Errorf("isFileRef(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPublicKeyPathFor(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/k/operator.key", "/k/operator.pub"},
		{"/k/operator", "/k/operator.pub"},
		{"/k/my.key.key", "/k/my.key.pub"},
	}
	for _, c := range cases {
		if got := publicKeyPathFor(c.in); got != c.want {
			t.Errorf("publicKeyPathFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultSigningKeyPathFollowsConfigDiscovery(t *testing.T) {
	t.Setenv("DEBARK_CONFIG", filepath.Join("etc", "df", "config.yaml"))
	if got, want := DefaultSigningKeyPath(), filepath.Join("etc", "df", "operator.key"); got != want {
		t.Errorf("with DEBARK_CONFIG: %q, want %q", got, want)
	}

	t.Setenv("DEBARK_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join("home", "a", ".config"))
	if got, want := DefaultSigningKeyPath(), filepath.Join("home", "a", ".config", "debark", "operator.key"); got != want {
		t.Errorf("with XDG_CONFIG_HOME: %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// disk space
// ---------------------------------------------------------------------------

func TestDecideDisk(t *testing.T) {
	const gib = uint64(1) << 30

	cases := []struct {
		name         string
		obs          spaceObservation
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
	}{
		{
			name:         "plenty",
			obs:          spaceObservation{Threshold: 5 * gib, Paths: []pathSpace{{Path: "/store", Free: 120 * gib, Total: 500 * gib}}},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "120.0 GiB free",
		},
		{
			name:         "tight",
			obs:          spaceObservation{Threshold: 5 * gib, Paths: []pathSpace{{Path: "/store", Free: gib + gib/2, Total: 500 * gib}}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "1.5 GiB is free",
		},
		{
			name: "reports the tightest of several",
			obs: spaceObservation{Threshold: 5 * gib, Paths: []pathSpace{
				{Path: "/store", Free: 400 * gib, Total: 500 * gib},
				{Path: "/media/usb", Free: 2 * gib, Total: 8 * gib},
			}},
			wantStatus:   StatusProblem,
			wantSeverity: SeverityDegraded,
			wantMentions: "/media/usb",
		},
		{
			name:         "unmeasurable",
			obs:          spaceObservation{Threshold: 5 * gib, Paths: []pathSpace{{Path: "/store", Err: errors.New("not supported")}}},
			wantStatus:   StatusSkipped,
			wantSeverity: SeverityInfo,
			wantMentions: "could not be measured",
		},
		{
			name: "one measurable path is enough",
			obs: spaceObservation{Threshold: 5 * gib, Paths: []pathSpace{
				{Path: "/store", Err: errors.New("gone")},
				{Path: "/out", Free: 50 * gib, Total: 100 * gib},
			}},
			wantStatus:   StatusOK,
			wantSeverity: SeverityInfo,
			wantMentions: "50.0 GiB free",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideDisk(c.obs)
			assertRow(t, got, c.wantStatus, c.wantSeverity, c.wantMentions, false)
		})
	}
}

// TestLowDiskNeverBlocks: how much space a bundle needs depends entirely on
// what the operator picks, so a guess must never refuse the build.
func TestLowDiskNeverBlocks(t *testing.T) {
	got := decideDisk(spaceObservation{Threshold: 5 << 30, Paths: []pathSpace{{Path: "/store", Free: 1024}}})
	if got.Severity == SeverityBlocking {
		t.Error("low disk must warn, never block")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1536, "1.5 KiB"},
		{5 << 30, "5.0 GiB"},
		{3 << 40, "3.0 TiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNearestExistingDir(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "does", "not", "exist", "yet")
	if got := nearestExistingDir(deep); got != dir {
		t.Errorf("nearestExistingDir(%q) = %q, want %q", deep, got, dir)
	}
	if got := nearestExistingDir(dir); got != dir {
		t.Errorf("nearestExistingDir on an existing dir = %q, want %q", got, dir)
	}
}

// TestDiskCheckSurvivesAFirstRun: on a first run the store directory does not
// exist yet, which is exactly when this check runs.
func TestDiskCheckSurvivesAFirstRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "store", "not", "created")
	got := diskCheck(context.Background(), Options{SpacePaths: []string{missing}}.withDefaults())
	if got.Status == StatusProblem && got.Severity == SeverityBlocking {
		t.Fatal("an uncreated store directory must not block")
	}
	if got.Summary == "" {
		t.Error("no summary")
	}
}

// ---------------------------------------------------------------------------
// archive reachability
// ---------------------------------------------------------------------------

func TestDecideArchive(t *testing.T) {
	cases := []struct {
		name         string
		obs          archiveObservation
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
	}{
		{"no host chosen", archiveObservation{}, StatusSkipped, SeverityInfo, "nothing was contacted"},
		{"reachable", archiveObservation{Host: "archive.ubuntu.com", Addr: "archive.ubuntu.com:443"}, StatusOK, SeverityInfo, "is reachable"},
		{"unreachable", archiveObservation{Host: "archive.ubuntu.com", Addr: "archive.ubuntu.com:443", Err: errors.New("i/o timeout")}, StatusProblem, SeverityDegraded, "did not accept a connection"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideArchive(c.obs)
			assertRow(t, got, c.wantStatus, c.wantSeverity, c.wantMentions, false)
		})
	}
}

// TestNoNetworkCheckByDefault is the rule-3 guard, and it is the reason the
// archive check takes its host from the caller: with no host configured the
// check is not registered at all, so the default report cannot open a socket
// however it is run.
func TestNoNetworkCheckByDefault(t *testing.T) {
	c := New(Options{})
	for _, ck := range c.checks() {
		if ck.id == CheckArchiveNetwork {
			t.Fatal("the archive check must not be in the default set")
		}
	}

	dialed := make(chan string, 1)
	restore := archiveDialer
	archiveDialer = func(context.Context, string) (net.Conn, error) {
		dialed <- "someone dialled"
		return nil, errors.New("refused")
	}
	t.Cleanup(func() { archiveDialer = restore })

	New(Options{Runner: func(context.Context, string, ...string) CommandOutput {
		return CommandOutput{ExitCode: 1}
	}, LookPath: lookPathIn()}).Run(context.Background())

	select {
	case msg := <-dialed:
		t.Fatalf("the default report opened a network connection: %s", msg)
	default:
	}

	withHost := New(Options{ArchiveHost: "archive.ubuntu.com"})
	found := false
	for _, ck := range withHost.checks() {
		if ck.id == CheckArchiveNetwork {
			found = true
		}
	}
	if !found {
		t.Error("the archive check must appear once the caller supplies a host")
	}
}

func TestArchiveAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"archive.ubuntu.com", "archive.ubuntu.com:443"},
		{"archive.example.com:80", "archive.example.com:80"},
		{"http://archive.example.com/", "archive.example.com:443"},
		{"https://archive.example.com", "archive.example.com:443"},
	}
	for _, c := range cases {
		if got := archiveAddr(c.in); got != c.want {
			t.Errorf("archiveAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// the runner: timeouts and concurrency
// ---------------------------------------------------------------------------

// TestChecksTimeOutRatherThanHang is the wedged-socket case. The fake command
// never returns on its own; only the deadline ends it, and the row that comes
// back has to say so and still carry a remedy.
func TestChecksTimeOutRatherThanHang(t *testing.T) {
	o := Options{
		Runner:     slowRunner,
		BinaryPath: filepath.Join("bin", "debark"),
		LookPath:   lookPathIn("docker", "wsl", "apt-get", "dpkg"),
		Timeout:    150 * time.Millisecond,
		// Set explicitly: the container check has a twenty-second budget of
		// its own in production, and this test asserts what a timed-out
		// container row says, not how long the real one waits.
		ContainerTimeout: 150 * time.Millisecond,
	}
	rep := New(o).Run(context.Background())

	if err := rep.Validate(); err != nil {
		t.Fatalf("a timed-out report broke the row contract: %v", err)
	}
	bin, _ := rep.Get(CheckBinary)
	if bin.Status != StatusProblem || !strings.Contains(bin.Summary, "did not answer") {
		t.Errorf("binary row = %+v, want a timeout row", bin)
	}
	cont, _ := rep.Get(CheckContainer)
	if !strings.Contains(cont.Summary, "did not answer within the timeout") {
		t.Errorf("container row = %+v, want a timeout row", cont)
	}
}

// TestReportIsBoundedByOneTimeout is the cold-start budget expressed as a test.
// Every probe hangs; run serially the report would cost one timeout per check,
// so finishing in appreciably less than that is the proof the checks really do
// run concurrently.
//
// ContainerTimeout is set explicitly and to the same value. It has its own
// budget in production — twenty seconds, for a measured 6431 ms `docker info`
// on a healthy but idle Docker Desktop — and leaving it at that default here
// would make this test wait it out rather than the 300 ms it is describing.
// Both are set so the bound under test is unambiguous.
func TestReportIsBoundedByOneTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	o := Options{
		Runner:           slowRunner,
		BinaryPath:       filepath.Join("bin", "debark"),
		LookPath:         lookPathIn("docker", "podman", "wsl", "apt-get", "dpkg"),
		Timeout:          timeout,
		ContainerTimeout: timeout,
	}

	slowChecks := 0
	for _, ck := range New(o).checks() {
		switch ck.id {
		case CheckBinary, CheckContainer, CheckAPT, CheckWSL:
			slowChecks++
		}
	}
	if slowChecks < 2 {
		t.Fatalf("only %d checks run commands on %s; this test proves nothing", slowChecks, runtime.GOOS)
	}

	start := time.Now()
	rep := New(o).Run(context.Background())
	elapsed := time.Since(start)

	serial := time.Duration(slowChecks) * timeout
	if elapsed >= serial {
		t.Errorf("report took %s; %d hung checks run serially would take %s, so they are not concurrent", elapsed, slowChecks, serial)
	}
	if elapsed < timeout {
		t.Errorf("report took %s, less than the %s timeout it should have waited out", elapsed, timeout)
	}
	if rep.Duration <= 0 {
		t.Error("the report must record its own duration for the cold-start budget")
	}
}

// TestCancellingTheContextEndsTheReport: the application closing the readiness
// screen must not leave probes running.
func TestCancellingTheContextEndsTheReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	o := Options{
		Runner:     slowRunner,
		BinaryPath: filepath.Join("bin", "debark"),
		LookPath:   lookPathIn("docker", "wsl", "apt-get", "dpkg"),
		Timeout:    10 * time.Second,
	}

	done := make(chan Report, 1)
	go func() { done <- New(o).Run(ctx) }()
	cancel()

	select {
	case rep := <-done:
		if len(rep.Results) == 0 {
			t.Error("a cancelled report must still describe its rows")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the context did not end the report")
	}
}

func TestRunOne(t *testing.T) {
	fr := newFakeRunner(map[string]CommandOutput{"docker": {Stdout: "Server Version: 25.0.0"}})
	c := New(Options{Runner: fr.run, LookPath: lookPathIn("docker")})

	got, err := c.RunOne(context.Background(), CheckContainer)
	if err != nil {
		t.Fatalf("RunOne(container): %v", err)
	}
	if got.ID != CheckContainer || got.Status != StatusOK {
		t.Errorf("got %+v, want a passing container row", got)
	}

	if _, err := c.RunOne(context.Background(), CheckBuildEnvironment); err == nil {
		t.Error("RunOne must refuse the derived row")
	} else if !strings.Contains(err.Error(), "Report.With") {
		t.Errorf("the refusal must say what to do instead: %v", err)
	}

	if _, err := c.RunOne(context.Background(), "not-a-check"); err == nil {
		t.Error("RunOne must refuse an unknown id")
	}
}

// TestExecRunnerHonoursTheDeadline exercises the real runner rather than a
// fake, because Options.Timeout is worthless if exec.CommandContext is not
// wired to it. Guarded: it needs a sleep-like command, and skips cleanly.
func TestExecRunnerHonoursTheDeadline(t *testing.T) {
	name, args := "sleep", []string{"5"}
	if runtime.GOOS == "windows" {
		name, args = "ping", []string{"-n", "6", "127.0.0.1"}
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("no %s on this machine", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	out := ExecRunner(ctx, name, args...)
	elapsed := time.Since(start)

	if !out.TimedOut {
		t.Errorf("out = %+v, want TimedOut", out)
	}
	if out.OK() {
		t.Error("a killed command must not report OK")
	}
	if elapsed > 3*time.Second {
		t.Errorf("the deadline took %s to bite", elapsed)
	}
}

func TestExecRunnerReportsAMissingCommand(t *testing.T) {
	out := ExecRunner(context.Background(), "definitely-not-a-real-command-9f2b")
	if out.StartErr == nil {
		t.Errorf("out = %+v, want StartErr set", out)
	}
	if out.OK() {
		t.Error("a command that never started must not report OK")
	}
}

func TestCommandOutputMessagePrefersStderr(t *testing.T) {
	out := CommandOutput{Stdout: "on stdout", Stderr: "\n  the real problem\nmore"}
	if got := out.Message(); got != "the real problem" {
		t.Errorf("Message() = %q", got)
	}
	if got := (CommandOutput{Stdout: "only stdout"}).Message(); got != "only stdout" {
		t.Errorf("Message() = %q", got)
	}
}

// ---------------------------------------------------------------------------
// this machine
// ---------------------------------------------------------------------------

// TestRealMachineReportIsActionable runs the checks against whatever this
// machine actually is. It asserts no particular environment — the point is that
// on any machine, set up or not, every row comes back with a sentence and every
// problem comes back with something to do about it.
func TestRealMachineReportIsActionable(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real docker, wsl and apt probes")
	}

	rep := New(Options{Timeout: 5 * time.Second}).Run(context.Background())
	if err := rep.Validate(); err != nil {
		t.Fatalf("this machine produced a row that breaks the contract: %v", err)
	}
	if len(rep.Results) < 4 {
		t.Errorf("only %d rows on %s: %v", len(rep.Results), runtime.GOOS, ids(rep.Results))
	}
	if _, ok := rep.Get(CheckBuildEnvironment); !ok {
		t.Error("the derived build-environment row is missing")
	}
	for _, r := range rep.Results {
		if r.Action != nil && r.Status != StatusProblem {
			t.Errorf("%s: a passing row offered an action", r.ID)
		}
	}
	t.Logf("%s, %s, can build: %v", rep.Platform, rep.Duration, rep.CanBuild())
	for _, r := range rep.Results {
		line := fmt.Sprintf("  %-20s %-8s %-9s %s", r.ID, r.Status, r.Severity, r.Summary)
		if r.Action != nil {
			line += " -> " + r.Action.String()
		}
		t.Log(line)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertRow(t *testing.T, got Result, status Status, severity Severity, mentions string, wantAction bool) {
	t.Helper()
	if got.Status != status {
		t.Errorf("Status = %q, want %q", got.Status, status)
	}
	if got.Severity != severity {
		t.Errorf("Severity = %q, want %q", got.Severity, severity)
	}
	if !strings.Contains(got.Summary, mentions) {
		t.Errorf("Summary = %q, want it to mention %q", got.Summary, mentions)
	}
	if wantAction && got.Action == nil {
		t.Error("no action offered")
	}
	if err := (Report{Results: []Result{got}}).Validate(); err != nil {
		t.Errorf("row breaks the contract: %v", err)
	}
}
