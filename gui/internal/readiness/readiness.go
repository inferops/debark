// Package readiness answers one question about the machine the operator is
// sitting at: can this computer build a bundle, and when it cannot, what does
// the operator do about it?
//
// # Rows with actions, never a wall
//
// This package exists to make the application's first screen useful on a
// machine that is not perfectly set up, which is most machines. A first-run
// screen that reports "requirements not met" and stops is a product failure:
// the operator learns nothing they can act on, and the application they just
// installed does nothing. So every Result carries two sentences — Summary says
// what is true, Remedy says what to do next — and, wherever a command exists
// that would fix it, an Action carrying that exact command.
//
// # Nothing here gates the application
//
// Severity is three-valued on purpose, and it is not a pass/fail boolean on
// purpose. An operator with no signing key can still choose a target, browse a
// catalogue, pick packages and reach the build screen; the key is needed at the
// moment of signing, and debark writes an unsigned bundle when there is none
// (resolveSignOptions, in the core repository's cmd_build.go). An operator with
// no container runtime is perfectly fine if the host has apt. Gating the
// application on either would be gating work that does not depend on them.
//
// Report.CanBuild is therefore the only question this package answers with a
// boolean, and it is a question about the build button, not about the
// application. Exactly two results can make it false: a missing debark
// binary, which every other operation shells out to, and a machine with no
// usable build environment at all. Everything else degrades.
//
// # Nothing here runs anything privileged, and nothing here dials out
//
// Actions are described, never executed. A Result hands the application layer
// an argv to show and for the operator to consent to; this package never
// elevates, and never itself runs a package manager, "wsl --install" or
// "debark keygen". Showing the exact command is a project rule — the CLI
// stays the complete interface — so the command is a field of the type rather
// than prose the UI has to compose.
//
// The default check set makes no network connection whatsoever. See
// Options.ArchiveHost for the one check that can, why it is off by default, and
// why it takes its host from the operator's chosen target rather than from a
// constant in this file.
//
// # Concurrency and cost
//
// Every check runs in its own goroutine and every external command runs under a
// deadline, because this report is on the cold-start path and because the
// realistic failure mode of a broken container socket is a "docker info" that
// never returns. The whole report therefore costs at most the longest single
// check's deadline in wall time, whatever the machine is doing — which is
// Options.ContainerTimeout, since the container probe is the one check with a
// budget of its own.
package readiness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Check identifiers. Stable: the application layer keys its rows off them and
// the frontend keys its copy off them, so they outlive any rewording.
const (
	// CheckBinary is the debark command-line tool itself.
	CheckBinary = "debark-binary"
	// CheckAPT is a native apt-get and dpkg on the host. Linux only.
	CheckAPT = "apt"
	// CheckContainer is docker or podman, present and actually answering.
	CheckContainer = "container"
	// CheckWSL is the Windows Subsystem for Linux. Windows only.
	CheckWSL = "wsl"
	// CheckSelfBinary is the static linux/<arch> debark that the container
	// backend mounts into the container and re-execs. Non-Linux hosts only:
	// on Linux the running debark already is one. See checks_selfbinary.go
	// for why a working container runtime is not enough on its own.
	CheckSelfBinary = "self-binary"
	// CheckBuildEnvironment is derived, not probed: it is the answer to "is
	// there any way at all to run apt on this machine", read off the three
	// checks above. See DeriveBuildEnvironment.
	CheckBuildEnvironment = "build-environment"
	// CheckSigningKey is the operator's ed25519 signing key.
	CheckSigningKey = "signing-key"
	// CheckDiskSpace is free space where bundles and the store will land.
	CheckDiskSpace = "disk-space"
	// CheckArchiveNetwork is reachability of the target's own archive host.
	// Opt-in; absent from the default set. See Options.ArchiveHost.
	CheckArchiveNetwork = "archive-network"
)

// checkOrder is the order results are presented in, blockers first and
// nice-to-knows last, so the application layer never has to sort and the
// frontend never has to know about severity to lay the list out. A check that
// does not apply to a platform simply never appears.
var checkOrder = []string{
	CheckBinary,
	CheckAPT,
	CheckWSL,
	CheckContainer,
	CheckSelfBinary,
	CheckBuildEnvironment,
	CheckSigningKey,
	CheckDiskSpace,
	CheckArchiveNetwork,
}

// CheckIDs returns every check id this package can produce, in checkOrder, on
// any platform — including the ones that are never registered on the platform
// asking.
//
// It exists so the invariant that internal/app mirrors these ids one for one
// can be a test rather than a habit. It was a habit, and self-binary was added
// here without reaching internal/app/types.go or the id union in
// docs/dev/binding-surface.md, because nothing anywhere would have failed.
func CheckIDs() []string {
	return append([]string(nil), checkOrder...)
}

// Severity is what a result costs the operator. The three values are the whole
// point of the type: without them the only expressible outcome is "not ready",
// which is the wall this package exists to avoid.
type Severity string

const (
	// SeverityBlocking marks something that stops a build outright. Only two
	// results may carry it — see the package comment — and both are genuinely
	// fatal to producing a bundle here, not merely untidy.
	SeverityBlocking Severity = "blocking"
	// SeverityDegraded marks something that narrows what is possible without
	// stopping it: no container runtime on a host that has apt, a WSL 1 distro,
	// less disk than a large bundle wants. The operator keeps working and may
	// never reach the limit.
	SeverityDegraded Severity = "degraded"
	// SeverityInfo marks something worth knowing that costs nothing today. A
	// passing check always carries it, and so does a missing signing key: not
	// having one is a choice debark supports, not a fault.
	SeverityInfo Severity = "info"
)

// severityRank orders severities for AtLeast. Private on purpose: callers
// compare severities, they do not do arithmetic on them.
var severityRank = map[Severity]int{SeverityInfo: 0, SeverityDegraded: 1, SeverityBlocking: 2}

// AtLeast reports whether s is at least as severe as other, so a UI can ask for
// "everything degraded or worse" without hard-coding the ordering.
func (s Severity) AtLeast(other Severity) bool {
	return severityRank[s] >= severityRank[other]
}

// Status is what happened when the check ran, kept separate from Severity
// because they answer different questions. Status is "did this pass"; Severity
// is "how much does it matter that it did not". Collapsing the two would make a
// passing check that would have been blocking indistinguishable from a failing
// one, and the screen could no longer show a green row for the thing that
// matters most.
type Status string

const (
	// StatusOK means the check found what it was looking for.
	StatusOK Status = "ok"
	// StatusProblem means it did not, and Remedy says what to do.
	StatusProblem Status = "problem"
	// StatusSkipped means the check did not apply or could not run — an
	// unconfigured archive host, a filesystem whose free space this platform
	// cannot report. Never a failure, never blocking.
	StatusSkipped Status = "skipped"
)

// Action is a concrete remedy the application layer can offer as a button. It
// is data, never behaviour: nothing in this package runs one.
//
// Command is a field rather than prose because the project rule is that the CLI
// stays the complete interface and the GUI shows the command it is about to
// run. An Action with an empty Command is meaningless — where no honest
// one-line fix exists, a check sets Remedy and leaves Action nil rather than
// invent a command that will not work.
type Action struct {
	// Label is the button text: imperative and short, "Create a signing key".
	Label string `json:"label"`
	// Command is the exact argv, already split. It is not a shell string: no
	// pipes, no globs, no $VAR anyone is expected to expand. Render it with
	// Action.String.
	Command []string `json:"command"`
	// Elevated is true when Command needs root or Administrator. The operator
	// consents to elevation in the application layer; this package only reports
	// that it will be needed.
	Elevated bool `json:"elevated"`
	// Note is the sentence to show beside the command when running it is not
	// the whole story — "log out and back in before this takes effect".
	Note string `json:"note,omitempty"`
}

// String renders Command the way a person would type it, quoting only the
// arguments that need it. For display and for copy-to-clipboard; never parse it
// back.
func (a Action) String() string {
	parts := make([]string, 0, len(a.Command))
	for _, c := range a.Command {
		if c == "" || strings.ContainsAny(c, " \t\"'") {
			parts = append(parts, strconv.Quote(c))
			continue
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " ")
}

// Result is one row on the readiness screen.
//
// Summary and Remedy are two sentences on purpose. Summary states what is true
// ("Docker is installed but its daemon is not running"); Remedy states what the
// operator does about it ("Start Docker, then run this check again"). A row
// that merges them states neither clearly, and a row with a Summary and no
// Remedy is the wall.
type Result struct {
	// ID is one of the Check constants. Stamped by the runner, not by the
	// check, so a check cannot disagree with the list it was registered in.
	ID string `json:"id"`
	// Title is the human name of the check, for the row's label.
	Title string `json:"title"`
	// Status is whether the check passed.
	Status Status `json:"status"`
	// Severity is what this result costs. Always SeverityInfo when Status is
	// StatusOK or StatusSkipped.
	Severity Severity `json:"severity"`
	// Summary is one sentence saying what is true. Always set.
	Summary string `json:"summary"`
	// Remedy is one sentence saying what to do next. Always set when Status is
	// StatusProblem; that invariant is what makes the screen actionable, and
	// Report.Validate enforces it across every check at once.
	Remedy string `json:"remedy,omitempty"`
	// Action is the concrete command that would fix this, when one exists.
	Action *Action `json:"action,omitempty"`
	// Detail is raw evidence for a details drawer: a version string, the first
	// line of a command's stderr, the paths that were searched. Never required
	// reading — the row must make sense without it.
	Detail string `json:"detail,omitempty"`
	// Duration is how long this check took, for the cold-start budget.
	Duration time.Duration `json:"duration"`
}

// Report is the whole first-run answer.
type Report struct {
	// Results is one row per check, in checkOrder.
	Results []Result `json:"results"`
	// Platform is GOOS/GOARCH: half the remedies differ by it, and a bug report
	// needs it.
	Platform string `json:"platform"`
	// StartedAt is when Run began.
	StartedAt time.Time `json:"started_at"`
	// Duration is the wall time of the whole concurrent run, which is the
	// number the cold-start budget cares about.
	Duration time.Duration `json:"duration"`
}

// Get returns the result with the given ID.
func (r Report) Get(id string) (Result, bool) {
	for _, res := range r.Results {
		if res.ID == id {
			return res, true
		}
	}
	return Result{}, false
}

// Problems returns every result at or above the given severity that did not
// pass, in report order. Passing and skipped rows are never problems, whatever
// severity they would have carried had they failed.
func (r Report) Problems(atLeast Severity) []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status == StatusProblem && res.Severity.AtLeast(atLeast) {
			out = append(out, res)
		}
	}
	return out
}

// Blocking returns the results that stop a build outright.
func (r Report) Blocking() []Result { return r.Problems(SeverityBlocking) }

// CanBuild reports whether a build can be attempted on this machine. It is the
// only boolean this package offers and it governs the build button — not
// navigation, not the catalogue, not the picker. See the package comment.
func (r Report) CanBuild() bool { return len(r.Blocking()) == 0 }

// Validate reports rows that break this package's own contract: a problem with
// no remedy, an action with no command, a missing summary, a passing row
// carrying a severity.
//
// It exists so one test can assert the contract across every check at once
// rather than one wording at a time, and so that a check added later cannot
// quietly reintroduce the wall.
func (r Report) Validate() error {
	var bad []string
	for _, res := range r.Results {
		switch {
		case res.Summary == "":
			bad = append(bad, res.ID+": no summary")
		case res.Status == StatusProblem && res.Remedy == "":
			bad = append(bad, res.ID+": problem with no remedy")
		case res.Action != nil && len(res.Action.Command) == 0:
			bad = append(bad, res.ID+": action with no command")
		case res.Action != nil && res.Action.Label == "":
			bad = append(bad, res.ID+": action with no label")
		case res.Status != StatusProblem && res.Severity != SeverityInfo:
			bad = append(bad, res.ID+": non-problem carrying severity "+string(res.Severity))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("readiness: %s", strings.Join(bad, "; "))
	}
	return nil
}

// With returns a copy of the report with updated replacing the result of the
// same ID, re-deriving CheckBuildEnvironment from the new set.
//
// This is the "I started Docker, check again" path: the application layer calls
// Checker.RunOne for the one row the operator just fixed and folds the answer
// back in here, so the derived headline row follows without a second full run.
func (r Report) With(updated Result) Report {
	out := r
	out.Results = make([]Result, len(r.Results))
	copy(out.Results, r.Results)

	replaced := false
	for i := range out.Results {
		if out.Results[i].ID == updated.ID {
			out.Results[i] = updated
			replaced = true
			break
		}
	}
	if !replaced {
		out.Results = append(out.Results, updated)
	}

	derived := DeriveBuildEnvironment(out.Results)
	found := false
	for i := range out.Results {
		if out.Results[i].ID == CheckBuildEnvironment {
			out.Results[i] = derived
			found = true
			break
		}
	}
	if !found {
		out.Results = append(out.Results, derived)
	}
	sortResults(out.Results)
	return out
}

// CommandOutput is everything one external command produced. A non-zero exit is
// not an error here: for these probes it is usually the answer — a "docker
// info" that exits 1 is exactly how a stopped daemon reports itself — so it is
// a field, and only a command that never started sets StartErr.
type CommandOutput struct {
	// Stdout and Stderr are captured whole. Nothing probed here produces enough
	// output to be worth streaming.
	Stdout string
	Stderr string
	// ExitCode is the process exit status, or -1 if it never ran or was killed.
	ExitCode int
	// TimedOut is true when the context deadline expired before the process
	// finished. The process was killed and Stdout and Stderr hold a prefix.
	TimedOut bool
	// StartErr is set only when the command could not be started at all:
	// missing binary, or an executable this user may not run.
	StartErr error
}

// OK reports whether the command ran and exited zero.
func (o CommandOutput) OK() bool { return o.StartErr == nil && !o.TimedOut && o.ExitCode == 0 }

// Message is the most useful single line the command said, preferring stderr,
// for a Result's Detail.
func (o CommandOutput) Message() string {
	if s := firstLine(o.Stderr); s != "" {
		return s
	}
	return firstLine(o.Stdout)
}

// Runner runs one external command to completion.
//
// Every probe goes through a Runner for two reasons. It is the seam that lets
// the decision logic be tested against fabricated output on a machine with no
// docker and no WSL, which is where nearly all of this package's value lives.
// And it is the single place the deadline is enforced: implementations must
// honour ctx, because Options.Timeout is applied by giving ctx a deadline, and
// a Runner that ignores it hands the cold-start path a "docker info" on a
// wedged socket.
type Runner func(ctx context.Context, name string, args ...string) CommandOutput

// ExecRunner is the real Runner: exec.CommandContext with output captured and,
// on Windows, no console window flashed in the operator's face.
func ExecRunner(ctx context.Context, name string, args ...string) CommandOutput {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	hideConsole(cmd)

	err := cmd.Run()
	out := CommandOutput{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		out.ExitCode = 0
	case errors.As(err, &exitErr):
		out.ExitCode = exitErr.ExitCode()
	default:
		out.StartErr = err
	}
	// Report the deadline specifically. A cancelled context means the caller
	// abandoned the whole report and the distinction stops mattering; an
	// expired deadline is a finding the operator should see, because "docker
	// did not answer within five seconds" is itself a symptom of a wedged
	// socket and deserves a different sentence from "docker said no".
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.TimedOut = true
	}
	return out
}

// BinaryLocator finds the debark executable and reports where it is.
//
// This is the seam to internal/cliadapter. That package owns locating and
// invoking debark for real, and this package deliberately does not import it:
// a readiness check that cannot run until the adapter is built is a readiness
// check that cannot report on the adapter's own precondition, and coupling the
// first screen to the largest package in the tree is the wrong dependency
// direction anyway. lookPathLocator is a minimal self-contained stand-in until
// then. When the application layer wires this package up it sets
// Options.BinaryPath from the adapter's own Probe, or passes the adapter's
// locator here, and the two agree by construction rather than by coincidence.
type BinaryLocator func(ctx context.Context) (path string, err error)

// Options configures a Checker. The zero value is usable: every field falls
// back to a documented default in withDefaults.
type Options struct {
	// Runner runs external commands. Defaults to ExecRunner.
	Runner Runner
	// BinaryPath is the debark executable when the caller already located it
	// — the seam described on BinaryLocator. When set, no search happens.
	BinaryPath string
	// Locator finds the debark executable when BinaryPath is empty. Defaults
	// to lookPathLocator.
	Locator BinaryLocator
	// Timeout bounds each check, including every command it runs. Because
	// checks run concurrently this is also the ceiling on the whole report.
	//
	// The container check is the one exception; see ContainerTimeout.
	Timeout time.Duration
	// ContainerTimeout bounds the container check alone, because `docker info`
	// on a healthy but idle Docker Desktop is an order of magnitude slower
	// than every other probe here. Defaults to defaultContainerTimeout.
	ContainerTimeout time.Duration
	// SpacePaths are the directories whose free space matters. Defaults to the
	// debark store root. The application layer adds the operator's chosen
	// output directory once there is one.
	SpacePaths []string
	// LowDiskThreshold is the free-space figure below which the disk check
	// warns. Defaults to defaultLowDisk.
	LowDiskThreshold uint64
	// SigningKeyPath is where to look for the operator's ed25519 key. Defaults
	// to DefaultSigningKeyPath.
	SigningKeyPath string
	// ArchiveHost enables the reachability check, for one host and only that
	// host.
	//
	// It is empty by default, and the default check set therefore makes no
	// network connection at all. That is correctness rather than caution: at
	// first-run time this application does not yet know which target the
	// operator wants, so it does not know which archive is relevant, and any
	// host it dialled would be a host chosen by this source file rather than by
	// the operator's own sources. That is the shape of a phone-home whatever
	// the hostname happens to be, and this is an air-gap tool. The check
	// therefore cannot run until a target exists, and the caller —
	// internal/app, after target selection — supplies the host it read from
	// that target. See archiveCheck.
	ArchiveHost string
	// SelfBinaryPath is an explicit path to the static linux/<arch> debark
	// the container backend mounts, when the caller already knows one.
	//
	// It is the config half of the three routes containerSelfPath honours.
	// DEBARK_SELF_BINARY is read by the check itself; the self_binary
	// config key is not, for the same reason probeSigningKey does not read
	// the config file — reimplementing debark's flag/env/file/profile
	// precedence here would be a second answer that can disagree with the
	// binary doing the building. The application layer has the CLI adapter,
	// reads `config show --json`, and sets this.
	SelfBinaryPath string
	// SelfBinaryArch is the dpkg architecture the build container will run,
	// which is the architecture that mounted binary has to be built for.
	// Defaults to DefaultContainerArch, this machine's own. The application
	// layer refines it once a target is chosen, exactly as it does
	// ArchiveHost: a snapshot records its own architecture, and an arm64
	// target needs an arm64 binary however the builder is built.
	SelfBinaryArch string
	// Username is the account the container-permission remedy names. Defaults
	// to the current user from the environment; it is a field so that remedy is
	// testable on a machine that has no docker group at all.
	Username string
	// LookPath finds an executable on PATH. Defaults to exec.LookPath; a field
	// so a test can describe a machine with only podman, or with nothing.
	LookPath func(string) (string, error)
}

const (
	// defaultTimeout bounds one check. Five seconds is chosen for wsl.exe,
	// which on a cold boot starts the WSL service before answering anything and
	// routinely takes two to three seconds; every other probe here finishes in
	// milliseconds or is already broken. Because checks run concurrently this
	// is also the worst case for the whole report.
	defaultTimeout = 5 * time.Second

	// defaultContainerTimeout bounds the container check, and is four times
	// the general one because `docker info` is not like the other probes.
	//
	// Measured on a real Windows machine with Docker Desktop running and
	// healthy: the first `docker info` after the daemon had been idle took
	// 6431 ms, and the two immediately after it took 558 ms and 455 ms. At the
	// general five-second bound that first call times out, and a perfectly
	// working Docker Desktop reports "installed but did not answer within the
	// timeout" on every cold run.
	//
	// That was always wrong and is now expensive: with WSL 2 no longer counted
	// as a route, this row is the entire build verdict on Windows and macOS,
	// so a false negative here is a machine being told it cannot build when it
	// can. Twenty seconds is roughly three times the measured cold call.
	//
	// It does not slow down the common failure. A stopped daemon does not hang
	// — `docker info` fails immediately with "cannot connect to the Docker
	// daemon" — so this budget is only ever spent on a socket that is
	// genuinely wedged, and the other rows land while it is being spent
	// because Run is concurrent.
	defaultContainerTimeout = 20 * time.Second
	// defaultLowDisk is the free-space warning threshold. A bundle is a copy of
	// every .deb the target needs plus its indexes: a handful of packages is
	// hundreds of megabytes, a desktop base with a real selection runs to
	// several gigabytes. Five GiB is the point below which a realistic build is
	// likely to run out rather than certain to — which is exactly why this
	// check warns and never blocks.
	defaultLowDisk = 5 << 30
)

func (o Options) withDefaults() Options {
	if o.Runner == nil {
		o.Runner = ExecRunner
	}
	if o.Locator == nil {
		o.Locator = lookPathLocator
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.ContainerTimeout <= 0 {
		o.ContainerTimeout = defaultContainerTimeout
	}
	if len(o.SpacePaths) == 0 {
		o.SpacePaths = defaultSpacePaths()
	}
	if o.LowDiskThreshold == 0 {
		o.LowDiskThreshold = defaultLowDisk
	}
	if o.SigningKeyPath == "" {
		o.SigningKeyPath = DefaultSigningKeyPath()
	}
	if o.Username == "" {
		o.Username = currentUsername()
	}
	return o
}

// check is one runnable row: an identity, a label, and a probe-and-decide pair
// behind a single func.
type check struct {
	id    string
	title string
	run   func(context.Context, Options) Result
}

// Checker runs the readiness checks. Safe for concurrent use; it keeps no state
// between runs, which is what makes re-running one row both cheap and correct.
type Checker struct {
	opt Options
}

// New returns a Checker. The zero Options is fine.
func New(opt Options) *Checker { return &Checker{opt: opt.withDefaults()} }

// checks returns every check that applies on this machine, in no particular
// order — Run sorts the results, so a platform file appends without having to
// know anything about presentation.
func (c *Checker) checks() []check {
	all := []check{
		{CheckBinary, "debark command-line tool", binaryCheck},
		{CheckContainer, "Container runtime", containerCheck},
		{CheckSigningKey, "Operator signing key", signingKeyCheck},
		{CheckDiskSpace, "Disk space", diskCheck},
	}
	all = append(all, platformChecks()...)
	if c.opt.ArchiveHost != "" {
		all = append(all, check{CheckArchiveNetwork, "Archive reachability", archiveCheck})
	}
	return all
}

// Run runs every applicable check concurrently and returns the whole report.
//
// The concurrency is not an optimisation. Run serially, one unreachable
// container socket would add its entire timeout to the cold start on top of
// everything else; run concurrently, the report costs one deadline in the
// worst case however many probes are wedged. That matters more since the
// container check got a longer budget of its own: serially, its twenty seconds
// would be twenty seconds nothing else could use.
func (c *Checker) Run(ctx context.Context) Report {
	start := time.Now()
	checks := c.checks()

	// Each goroutine writes its own index and nothing reads the slice until
	// Wait returns, so no lock is needed and none is taken.
	results := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, ck := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.run(ctx, ck)
		}()
	}
	wg.Wait()

	results = append(results, DeriveBuildEnvironment(results))
	sortResults(results)

	return Report{
		Results:   results,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		StartedAt: start,
		Duration:  time.Since(start),
	}
}

// RunOne re-runs a single check, so "I started Docker, check again" costs one
// "docker info" rather than a whole report. Fold the answer back in with
// Report.With, which re-derives CheckBuildEnvironment from it.
//
// CheckBuildEnvironment itself has no probe and cannot be run this way: it is
// computed from the others, so asking for it is a caller bug rather than an
// environment problem and it returns an error saying what to do instead.
func (c *Checker) RunOne(ctx context.Context, id string) (Result, error) {
	for _, ck := range c.checks() {
		if ck.id == id {
			return c.run(ctx, ck), nil
		}
	}
	if id == CheckBuildEnvironment {
		return Result{}, fmt.Errorf("readiness: %s is derived from the other checks; re-run one of those and fold it in with Report.With", CheckBuildEnvironment)
	}
	return Result{}, fmt.Errorf("readiness: no check named %q on %s", id, runtime.GOOS)
}

// run applies the deadline, stamps the identity the check itself does not know,
// and times it.
func (c *Checker) run(ctx context.Context, ck check) Result {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, c.timeoutFor(ck.id))
	defer cancel()

	r := ck.run(cctx, c.opt)
	r.ID = ck.id
	r.Title = ck.title
	r.Duration = time.Since(start)
	return r
}

// DeriveBuildEnvironment answers "is there any way at all to run apt on this
// machine" by reading the environment checks rather than probing anything
// itself.
//
// It is separate, derived and pure because the question is genuinely a
// cross-check one that no single row can answer. Absent apt is not fatal on a
// machine with docker; absent docker is not fatal on Debian. Only the
// conjunction is fatal, so only the conjunction is SeverityBlocking and every
// individual row stays SeverityDegraded. Modelling it any other way produces
// three red blockers on a perfectly workable machine, which is the wall.
//
// Being pure, this is also the one place the "can this machine build" rule is
// written down, and it can be table-tested across every combination without a
// container or a Windows kernel anywhere in sight.
//
// # There are two routes, and WSL 2 is not one of them
//
// A build runs apt either natively or inside a container, and nothing else.
// debark's --backend takes local, container or auto and core/lock records
// only the first two (core/lock/types.go); no code in either repository ever
// invokes wsl.exe to do work, and internal/cliadapter has no way to express a
// WSL route even if it wanted one. So on Windows, where a native apt is not
// registered at all, the container backend is the only route — and since a
// non-Linux host mounts and re-execs a Linux build of debark, that route is
// gated by the self-binary row.
//
// This function used to count a green WSL 2 row as a way to build. The
// consequence was not a cosmetic one. Because the self-binary gate below only
// fires when NO other route is ok, a working WSL 2 installation silently
// defeated it:
//
//	wsl  container  self-binary   ->  verdict          can_build
//	ok   ok         problem           ok, "using WSL 2"    true
//	ok   problem    problem           ok, "using WSL 2"    true
//	problem ok      problem           blocking, gate fires false
//
// Row 1 is an ordinary Windows machine with Docker Desktop, and it was
// measured live: can_build true, a green "This machine can build bundles"
// banner, and every build then failing. Row 3 is the only combination where
// the gate worked, and it requires a Windows machine with no WSL 2 — but
// Docker Desktop's own default backend IS WSL 2, and docker-desktop registers
// itself as a WSL 2 distribution, so on the machines the gate was built for it
// could essentially never fire.
//
// The WSL row is still probed and still shown. It is worth knowing, because
// Docker Desktop usually runs on it and a WSL fault is very often the
// explanation for a container fault. It is not a route.
func DeriveBuildEnvironment(results []Result) Result {
	res := Result{
		ID:       CheckBuildEnvironment,
		Title:    "Build environment",
		Severity: SeverityInfo,
	}

	// A container runtime that answers is not the same thing as a container
	// backend debark can use. On a non-Linux host debark mounts and
	// re-execs the binary it was invoked as, so a green container row plus a
	// missing linux/<arch> debark is precisely the combination that
	// reported CanBuild true and then failed every build. When the
	// self-binary row is in the set and is a problem, the container runtime
	// is still counted as considered — it exists — but it is not counted as a
	// way to build, because it is not one.
	//
	// The row is only consulted when it is present. On Linux it is never
	// registered, and a caller that folded in a partial set must not have the
	// container row silently discounted by a check that did not run.
	containerBlocked := false
	for _, r := range results {
		if r.ID == CheckSelfBinary && r.Status == StatusProblem {
			containerBlocked = true
		}
	}

	var usable []string
	considered := 0
	containerOnlyBlocked := false
	for _, r := range results {
		var name string
		switch r.ID {
		case CheckAPT:
			name = "native apt"
		case CheckContainer:
			name = "a container runtime"
		default:
			// CheckWSL lands here deliberately. See the block comment above:
			// it is context, not a route, and counting it defeated the
			// self-binary gate on every machine that gate exists for.
			continue
		}
		considered++
		if r.Status != StatusOK {
			continue
		}
		if r.ID == CheckContainer && containerBlocked {
			containerOnlyBlocked = true
			continue
		}
		usable = append(usable, name)
	}

	if considered == 0 {
		res.Status = StatusSkipped
		res.Summary = "No build-environment checks ran, so there is nothing to conclude."
		return res
	}

	if len(usable) > 0 {
		res.Status = StatusOK
		res.Summary = "This machine can run a build using " + joinWords(usable) + "."
		return res
	}

	res.Status = StatusProblem
	res.Severity = SeverityBlocking
	if containerOnlyBlocked {
		// The one case where the generic remedy would be actively misleading:
		// the operator has already done the thing it asks for. Naming the
		// real obstacle here as well as on its own row is what stops the
		// screen sending them to install a container runtime they are
		// already running.
		res.Summary = "A container runtime is running, but debark has no Linux build of itself to run inside it, so it cannot build a bundle yet."
		res.Remedy = "Fix the \"Linux debark for containers\" row below — that is the only thing standing between this machine and a build."
		return res
	}
	res.Summary = "This machine has no way to run apt, so it cannot build a bundle yet."
	res.Remedy = noBuildEnvironmentRemedy()
	return res
}

// timeoutFor is how long one check may take. Every check gets Options.Timeout
// except the container one, which gets its own because a healthy Docker
// Desktop is measurably slower than the general bound. See
// defaultContainerTimeout.
//
// A function rather than a field on check, because the difference is a
// property of what the probe talks to rather than of how it is registered, and
// because RunOne and Run must not be able to disagree about it.
func (c *Checker) timeoutFor(id string) time.Duration {
	if id == CheckContainer {
		return c.opt.ContainerTimeout
	}
	return c.opt.Timeout
}

// sortResults puts results in checkOrder. Stable, so an unrecognised ID a
// caller folded in with Report.With lands at the end in the order it was added
// rather than moving between runs.
func sortResults(rs []Result) {
	index := func(id string) int {
		for i, want := range checkOrder {
			if want == id {
				return i
			}
		}
		return len(checkOrder)
	}
	sort.SliceStable(rs, func(i, j int) bool { return index(rs[i].ID) < index(rs[j].ID) })
}

// joinWords renders a list the way a sentence wants it.
func joinWords(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// firstLine returns the first non-empty line of s, trimmed, which is what a
// details drawer wants out of a command that failed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
