package cliadapter

// invoke.go is the implementing package: locating the debark binary, running the
// short-lived commands, and decoding their --json documents into debark's
// own published types.
//
// Three files make up the real Adapter, and they share the struct declared
// here:
//
//	invoke.go  this file — process plumbing, discovery, --json decoding
//	events.go  Build, the NDJSON tail and delivery to an EventSink
//	errors.go  classify(): exit code + stderr → a *Error a person can act on
//
// # The seam events.go and errors.go build on
//
// adapter, procRunner and (*adapter).binary are the contract between the three
// files. Two rules matter:
//
//   - Every short-lived command goes through procRunner. Build does not: a
//     streaming build needs its own process handling, and events.go owns it.
//   - Nothing reads adapter.bin directly. It is empty until the binary has been
//     located, which happens lazily on first use; (*adapter).binary is the
//     accessor and returns an actionable *Error when there is nothing to run.
//
// # Why every invocation carries --json and --no-color, and no stdin
//
// --json is not a preference. It suppresses colour, progress rendering and —
// the reason it is mandatory — every interactive prompt (docs/dev/cli-surface.md
// §1). A GUI that let debark ask a question on a terminal that is not there
// would hang forever with nothing on screen. --no-color costs nothing and
// covers the case where a future command renders something outside the JSON
// document. And the child's stdin is the null device: if a prompt ever did slip
// through, it reads EOF and fails, rather than blocking the app.
//
// # Why capabilities are probed rather than assumed
//
// A debark that predates `snapshot list-bases` does not fail loudly when
// asked for it. cobra matches "list-bases" against `snapshot`'s subcommands,
// finds nothing, runs the parent — which has no RunE — and so PRINTS THE GROUP
// HELP TO STDOUT AND EXITS 0. Measured against the real binary with
// "debark snapshot bogus"; the capture is testdata/help/snapshot-group.txt.
// So "the command succeeded" is not evidence the command exists, and the only
// honest test is whether a document of the expected schema came back.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/verify"
	"github.com/inferops/debark/core/version"
)

// ---------------------------------------------------------------------------
// The process seam
// ---------------------------------------------------------------------------

// procRunner is the single exec seam for everything short-lived: it runs one
// command to completion and hands back what it left behind.
//
// The contract, which every implementation and every caller depends on:
//
//   - err is nil whenever the process actually STARTED AND EXITED, however it
//     exited. exitCode then carries its status, and 0 means success.
//   - err is non-nil only when there was no process to get a status from — the
//     binary could not be found or could not be started, or the context ended
//     first. exitCode is then procExitNotStarted, or procExitNotFound when the
//     cause was specifically a missing executable.
//   - stdout and stderr are returned in both cases, because a debark that
//     failed usually printed the interesting part before it did. `verify` and
//     `build` in particular print their whole --json document and THEN exit
//     non-zero (docs/dev/cli-surface.md C1, and measured again here for
//     verify — see Verify).
//
// events.go's Build deliberately does NOT use this: a build streams events
// while it runs, and collecting output into two byte slices at exit is the
// wrong shape for it.
type procRunner interface {
	Run(ctx context.Context, bin string, args []string) (stdout, stderr []byte, exitCode int, err error)
}

const (
	// procExitNotStarted is the exit code reported when no process ran, or
	// when one was killed by a signal. NewError maps anything outside the
	// frozen 0–7 table to dferr.Environment, which is what a process that died
	// in a way debark did not choose actually is.
	procExitNotStarted = -1

	// procExitNotFound is the conventional "command not found" status. It is
	// reported for a missing or unstartable binary so that a shell's 127 and
	// this application's own discovery failure reach classify() as the same
	// fact.
	procExitNotFound = 127

	// procWaitDelay bounds how long Wait will hang on the output pipes after
	// the process itself has gone. Without it a grandchild that inherited
	// stdout can keep Wait — and therefore a cancelled command — alive
	// indefinitely.
	procWaitDelay = 2 * time.Second
)

// procExec is the real procRunner: os/exec, with the child's stdin pointed at
// the null device and its stderr capped.
type procExec struct{}

// Run implements procRunner.
//
// exec.CommandContext kills the child when ctx ends — that is its default
// Cancel — so cancellation does not leak a process. WaitDelay covers the
// second half of the problem: a child that has died but whose descendants
// still hold the output pipe.
func (procExec) Run(ctx context.Context, bin string, args []string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = procWaitDelay

	var out bytes.Buffer
	// One byte over the cap, so newError sees a length greater than
	// MaxStderrBytes and marks the truncation instead of silently trimming to
	// exactly the limit.
	errOut := &procCapWriter{limit: MaxStderrBytes + 1}
	cmd.Stdout = &out
	cmd.Stderr = errOut
	// Nil Stdin means the null device. debark is already told not to prompt
	// by --json; this is the belt to that pair of braces, and it is what turns
	// a hypothetical prompt from a hang into an error.
	cmd.Stdin = nil

	err := cmd.Run()
	switch err {
	case nil:
		return out.Bytes(), errOut.Bytes(), int(dferr.Success), nil
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The process ran and exited: that is a status, not an error.
			return out.Bytes(), errOut.Bytes(), exitErr.ExitCode(), nil
		}
		code := procExitNotStarted
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			code = procExitNotFound
		}
		return out.Bytes(), errOut.Bytes(), code, err
	}
}

// procCapWriter keeps the first limit bytes written to it and discards the
// rest, without ever reporting a short write. A usage-class failure includes
// cobra's entire help text and a build that fails late can print far more;
// this is what stops a runaway child filling memory through the error path.
type procCapWriter struct {
	limit int
	buf   bytes.Buffer
}

func (w *procCapWriter) Write(p []byte) (int, error) {
	total := len(p)
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.buf.Write(p)
	}
	return total, nil
}

func (w *procCapWriter) Bytes() []byte { return w.buf.Bytes() }

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// Locate finds the debark executable: PATH first, then the directory this
// application was started from.
//
// That order is deliberate and matches internal/readiness's own stand-in
// locator, so the readiness screen and every later invocation agree about which
// debark is being talked about. PATH first because an operator who installed
// the debark package expects the packaged one; beside the executable second
// because that is where a .deb shipping both the GUI and the CLI puts them, and
// where a development tree keeps them.
//
// The error is a *Error carrying a summary that names both places searched. It
// is the first thing an operator hits on a fresh machine, so it says what to do
// rather than that something was not found.
func Locate() (string, error) {
	p, err := invokeLocate()
	if err != nil {
		return "", err
	}
	return p, nil
}

// invokeLocate is Locate with the concrete error type, so callers inside this
// package do not have to re-extract it and cannot trip over a typed nil.
func invokeLocate() (string, *Error) {
	if p, err := exec.LookPath(ProgramName); err == nil {
		if abs, absErr := filepath.Abs(p); absErr == nil {
			return abs, nil
		}
		return p, nil
	}

	beside := ""
	if self, err := os.Executable(); err == nil {
		beside = filepath.Dir(self)
		candidate := filepath.Join(beside, procExecutableName())
		if procIsExecutableFile(candidate) {
			return candidate, nil
		}
	}

	where := "PATH"
	if beside != "" {
		where = fmt.Sprintf("PATH and %s", beside)
	}
	e := classify([]string{ProgramName}, procExitNotFound, "").
		WithSummary("the %s command-line tool was not found (searched %s)", ProgramName, where)
	return "", invokeDefaultHint(e, fmt.Sprintf(
		"install the %s package, or put the %s binary on PATH — every catalogue, build and export in this application runs it",
		ProgramName, ProgramName))
}

// procExecutableName is the file name to look for beside this application.
// exec.LookPath already applies PATHEXT when it searches PATH; a direct
// filesystem probe does not, so the suffix is spelled out here.
func procExecutableName() string {
	if runtime.GOOS == "windows" {
		return ProgramName + ".exe"
	}
	return ProgramName
}

// procIsExecutableFile reports whether path is a regular file this process
// could plausibly exec. On Windows the mode bits say nothing about
// executability, so existence is the whole test there.
func procIsExecutableFile(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// ---------------------------------------------------------------------------
// The adapter
// ---------------------------------------------------------------------------

// Options configures New.
type Options struct {
	// BinaryPath is an explicit path to the debark binary, from an operator
	// setting or from a readiness check that already located one. Empty means
	// discover it with Locate on first use.
	BinaryPath string

	// runner replaces the process seam. Tests set it; there is deliberately no
	// exported way to do so, because code outside this package substitutes the
	// whole Adapter with internal/cliadapter/fake instead of reaching into
	// this one.
	runner procRunner
}

// adapter is the real Adapter. events.go adds Build to it.
type adapter struct {
	// mu guards bin, which is filled in on first use.
	mu sync.Mutex
	// bin is the absolute path to the debark binary. It is empty until the
	// binary has been located. Read it through (*adapter).binary, never
	// directly — that is what makes lazy discovery work and what turns a
	// missing binary into a sentence instead of an exec error.
	bin string

	// runner is the exec seam. Never nil after New.
	runner procRunner

	// capMu guards jsonEvents. It is deliberately NOT mu: filling the memo
	// runs `--help`, and that goes through binary(), which takes mu.
	capMu sync.Mutex
	// jsonEvents memoises the one capability the build path branches on. See
	// invokeJSONEventsSupported.
	jsonEvents uint8
}

// The three states of the --json-events memo. A tri-state rather than a bool
// plus a flag because "we have not asked" and "we asked and it said no" lead to
// opposite decisions, and collapsing them is exactly the bug this memo exists
// to avoid.
const (
	invokeEventsUnasked uint8 = iota
	invokeEventsYes
	invokeEventsNo
)

var _ Adapter = (*adapter)(nil)

// New returns the real Adapter.
//
// # Discovery is LAZY
//
// New touches no filesystem and starts no process; it cannot fail for anything
// but a malformed BinaryPath. The binary is located on first use and the
// successful result is cached, so an operator who installs debark while the
// app is open can press Retry rather than restart it — a failed discovery is
// never remembered.
//
// The alternative, locating eagerly here, was rejected: it makes New fail on
// exactly the machine where the app most needs to start, namely one where
// debark is not installed yet, and it would leave internal/app holding a nil
// Adapter with no way to explain itself. The readiness screen calls Locate
// directly for the up-front check instead.
func New(opts Options) (Adapter, error) {
	a := &adapter{runner: opts.runner}
	if a.runner == nil {
		a.runner = procExec{}
	}
	if p := strings.TrimSpace(opts.BinaryPath); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			e := classify([]string{ProgramName}, procExitNotFound, "").
				WithSummary("the configured %s path %q could not be resolved", ProgramName, p).
				WithCause(err)
			return nil, invokeDefaultHint(e, "clear the configured path to search PATH instead, or set it to an absolute path")
		}
		a.bin = abs
	}
	return a, nil
}

// binary returns the debark binary this adapter execs, locating it on first
// use. Every method — this file's and events.go's — must go through it.
func (a *adapter) binary() (string, *Error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bin != "" {
		return a.bin, nil
	}
	p, err := invokeLocate()
	if err != nil {
		// Deliberately not cached: discovery is retried on the next call, so
		// installing debark and pressing Retry works without a restart.
		return "", err
	}
	a.bin = p
	return p, nil
}

// ---------------------------------------------------------------------------
// Invocation
// ---------------------------------------------------------------------------

// invokeArgs builds the argv for one short-lived --json command.
//
// argv[0] is ProgramName rather than the located path: it is what a person
// sees in the details drawer and what they could paste into a shell. The real
// path is substituted only at the moment of exec.
func invokeArgs(parts ...string) []string {
	argv := make([]string, 0, len(parts)+3)
	argv = append(argv, ProgramName)
	argv = append(argv, parts...)
	return append(argv, "--json", "--no-color")
}

// invokeRun locates the binary and runs one command, returning everything the
// process left behind. It is the raw seam; invoke is this plus classification.
func (a *adapter) invokeRun(ctx context.Context, argv []string) (stdout, stderr []byte, exitCode int, err *Error) {
	bin, locErr := a.binary()
	if locErr != nil {
		return nil, nil, procExitNotFound, locErr
	}
	if ctx.Err() != nil {
		return nil, nil, procExitNotStarted, invokeCtxError(ctx, argv)
	}
	out, errOut, code, runErr := a.runner.Run(ctx, bin, argv[1:])
	// A cancelled context is checked before the exit status is believed: a
	// process this adapter killed reports a status that says nothing about the
	// work, and the operator pressing Stop is not a failure.
	if ctx.Err() != nil {
		return out, errOut, code, invokeCtxError(ctx, argv)
	}
	if runErr != nil {
		e := classify(argv, code, string(errOut)).WithCause(runErr)
		if code == procExitNotFound {
			e = e.WithSummary("the %s binary at %s could not be run", ProgramName, bin)
			e = invokeDefaultHint(e, fmt.Sprintf(
				"check that %s is installed and on PATH; this application found nothing it could execute there", ProgramName))
		}
		return out, errOut, code, e
	}
	return out, errOut, code, nil
}

// invoke runs one command and returns its stdout.
//
// stdout is returned even on failure, because the commands that matter most
// print their whole --json document before exiting non-zero. Callers that can
// use it — Verify, and events.go's Build — decode it regardless of the error.
func (a *adapter) invoke(ctx context.Context, argv []string) ([]byte, *Error) {
	out, errOut, code, err := a.invokeRun(ctx, argv)
	if err != nil {
		return out, err
	}
	if code != int(dferr.Success) {
		return out, classify(argv, code, string(errOut))
	}
	return out, nil
}

// invokeHelp runs a --help command and returns whatever text it produced,
// from either stream. A failure is reported as empty text rather than as an
// error: help is only ever read to decide a capability, and "it would not tell
// us" and "it does not have it" lead to the same degraded UI.
func (a *adapter) invokeHelp(ctx context.Context, argv []string) string {
	text, _ := a.invokeHelpOK(ctx, argv)
	return text
}

// invokeHelpOK is invokeHelp plus the one thing a caller that MEMOISES the
// answer needs: whether the command actually succeeded.
//
// cobra prints `--help` and exits 0. Anything else — a binary too old to
// understand the flag, a stand-in that is not cobra at all — writes an error
// message and exits non-zero, and that message is not help text: scanning it
// for a "Flags:" block finds none and so reports every capability absent. That
// is harmless for a probe that runs again next time and wrong for a decision
// remembered for the life of the adapter, which is why the exit status is
// returned rather than discarded here.
func (a *adapter) invokeHelpOK(ctx context.Context, argv []string) (string, bool) {
	out, errOut, code, err := a.invokeRun(ctx, argv)
	if err != nil {
		return "", false
	}
	text := string(out)
	if len(errOut) > 0 {
		text = string(out) + "\n" + string(errOut)
	}
	return text, code == int(dferr.Success)
}

// invokeCtxError turns an ended context into the right kind of *Error.
//
// Cancellation and a deadline are different events and must not read alike:
// the operator pressing Stop is not a failure of the work (Canceled() reports
// it, and the UI must not show it as an error), while a debark that never
// answered is a fact about the machine.
func invokeCtxError(ctx context.Context, argv []string) *Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		e := classify(argv, procExitNotStarted, "").
			WithSummary("%s did not answer in the time allowed", ProgramName).
			WithCause(ctx.Err())
		return invokeDefaultHint(e, fmt.Sprintf(
			"run the command below in a terminal; if it hangs there too, the %s binary is not usable on this machine", ProgramName))
	}
	e := NewCanceledError(argv).
		WithSummary("the command was cancelled before it finished")
	return e.WithHint("nothing was changed; run it again when you are ready")
}

// ---------------------------------------------------------------------------
// Decoding
// ---------------------------------------------------------------------------

// invokeDecode decodes one --json document into v.
//
// Empty output and unparseable output are told apart, because they mean
// different things: a debark that printed nothing at all was probably asked
// for something it does not have, and a debark that printed prose was asked
// for a subcommand it does not know (see the package comment on cobra's
// exit-0 group help).
func invokeDecode(argv []string, stdout []byte, v any) *Error {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return invokeOutputError(argv, nil,
			fmt.Sprintf("%s printed nothing where a JSON document was expected", ProgramName))
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return invokeOutputError(argv, err, fmt.Sprintf(
			"%s printed something this application could not read as JSON (it began %q)",
			ProgramName, invokeExcerpt(trimmed, 90)))
	}
	return nil
}

// invokeSchemaCheck rejects a document whose schema_version is not the one
// this application knows how to read, rather than letting a partially-decoded
// struct through.
//
// Not every command carries one. version, keygen, store gc, snapshot create
// and snapshot from-base emit documents with no schema_version at all
// (docs/dev/cli-surface.md §2 and §4), so their decoders do not call this.
// want is always an imported constant — base.ListSchemaVersion,
// snapshot.SchemaVersion, verify.SchemaVersion — never a literal, so a version
// bump in the core repository is a compile-time fact here.
func invokeSchemaCheck(argv []string, what, got, want string) *Error {
	if got == want {
		return nil
	}
	var summary string
	if got == "" {
		summary = fmt.Sprintf("%s printed a document with no schema_version where %s expected %s",
			ProgramName, what, want)
	} else {
		summary = fmt.Sprintf("%s printed a %s document where %s expected %s",
			ProgramName, got, what, want)
	}
	e := classify(argv, int(dferr.Success), "").WithSummary("%s", summary)
	return invokeDefaultHint(e, fmt.Sprintf(
		"this %s and this application disagree about the shape of `%s`; install a matching pair — the About panel shows which version of each is running",
		ProgramName, what))
}

// invokeOutputError is the "debark succeeded but the adapter could not use
// its output" case.
//
// Exit code 0 is deliberate and documented: NewError's contract says calling
// it with 0 is legitimate and means exactly this. The summary always says what
// was wrong with the output, so nothing here can surface a bare status.
func invokeOutputError(argv []string, cause error, summary string) *Error {
	e := classify(argv, int(dferr.Success), "").WithSummary("%s", summary)
	if cause != nil {
		e = e.WithCause(cause)
	}
	return invokeDefaultHint(e, fmt.Sprintf(
		"the command below ran but printed something unexpected — run it in a terminal to see what; a %s older or newer than this application is the usual cause",
		ProgramName))
}

// invokeDefaultHint fills in a hint only when nothing better is already there.
// The hint catalogue is errors.go's; this is the floor beneath it, for the
// failures errors.go cannot know anything about — a binary that is not
// installed, a document that would not decode.
func invokeDefaultHint(e *Error, hint string) *Error {
	if e.Hint() != "" {
		return e
	}
	return e.WithHint("%s", hint)
}

// invokeExcerpt renders the start of some output as one short quoted line, for
// a summary. Runs of whitespace collapse so a help screen does not arrive as
// forty lines in an error message.
func invokeExcerpt(b []byte, limit int) string {
	var sb strings.Builder
	space := false
	for _, r := range string(b) {
		if unicode.IsSpace(r) {
			space = sb.Len() > 0
			continue
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		if sb.Len() >= limit {
			return sb.String() + "…"
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

// Probe locates the binary and reports its version and capabilities.
//
// It runs `version --json` first — if that does not work there is nothing to
// report and no point probing further — and then three independent commands
// concurrently, because they are read-only, take about 60 ms each on a warm
// cache, and Probe sits on the cold-start path.
func (a *adapter) Probe(ctx context.Context) (Probe, error) {
	bin, locErr := a.binary()
	if locErr != nil {
		return Probe{}, locErr
	}

	argv := invokeArgs("version")
	out, err := a.invoke(ctx, argv)
	if err != nil {
		return Probe{}, err
	}
	// version --json carries no schema_version (docs/dev/cli-surface.md §2),
	// so the document's own name field is the only sanity check available.
	var info version.Info
	if e := invokeDecode(argv, out, &info); e != nil {
		return Probe{}, e
	}
	if info.Name != version.Name {
		e := classify(argv, int(dferr.Success), "").WithSummary(
			"the program at %s answered `version --json` but does not identify itself as %s", bin, ProgramName)
		return Probe{}, invokeDefaultHint(e,
			fmt.Sprintf("point this application at a real %s binary, or clear the configured path to search PATH", ProgramName))
	}

	caps, hostArch := a.invokeCapabilities(ctx)
	if ctx.Err() != nil {
		return Probe{}, invokeCtxError(ctx, argv)
	}
	return Probe{
		Path:         bin,
		Info:         info,
		HostArch:     hostArch,
		Capabilities: caps,
	}, nil
}

// invokeCapabilities asks the located binary what it can do, and returns the
// dpkg architecture it would default to.
//
// Each capability is decided by the strongest cheap evidence available:
//
//   - Bases is true only when `snapshot list-bases --json` actually returned a
//     debark.baselist/v1 document. Parsing `snapshot --help` was rejected:
//     an old debark answers `snapshot list-bases` with the group's help text
//     and exit 0, so success is no evidence at all, and a document of the right
//     schema is the only thing that is. The same call yields HostArch, so this
//     costs nothing extra.
//   - JSONEvents and Keygen come from the root --help: one is a global flag,
//     the other a top-level command, and both are in the same 1.6 KB of text.
//   - SBOM comes from `build --help`, the only place --sbom appears.
//
// HostArch is empty when Bases is false. It comes from List.Arch — what
// debark says it would render for — and there is deliberately no fallback to
// this application's own runtime.GOARCH, which is a fact about the GUI and not
// about what debark would do. Nothing reads it when Bases is false anyway:
// the base picker is not offered at all.
//
// A failed capability probe reports the capability as absent. That is the
// point: an older debark must degrade with an explanation, not crash.
func (a *adapter) invokeCapabilities(ctx context.Context) (Capabilities, string) {
	var (
		wg        sync.WaitGroup
		rootHelp  string
		rootOK    bool
		buildHelp string
		list      *base.List
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		rootHelp, rootOK = a.invokeHelpOK(ctx, []string{ProgramName, "--help"})
	}()
	go func() {
		defer wg.Done()
		buildHelp = a.invokeHelp(ctx, []string{ProgramName, "build", "--help"})
	}()
	go func() {
		defer wg.Done()
		list, _ = a.invokeListBases(ctx, "")
	}()
	wg.Wait()

	caps := Capabilities{
		JSONEvents: invokeHasFlag(rootHelp, "--json-events"),
		Keygen:     invokeHasSubcommand(rootHelp, "keygen"),
		SBOM:       invokeHasFlag(buildHelp, "--sbom"),
	}
	arch := ""
	if list != nil {
		caps.Bases = true
		arch = list.Arch
	}
	// Remember what the help text said, so a build started after a probe pays
	// nothing to ask the same question again — but only when `--help` actually
	// succeeded, because an error message is not a flag listing.
	if rootOK {
		a.invokeRememberJSONEvents(rootHelp, caps.JSONEvents)
	}
	return caps, arch
}

// invokeRememberJSONEvents records what the root help text proved about
// --json-events, and records nothing when it proved nothing.
//
// Empty help is not evidence of absence: invokeHelp reports a command that
// would not run as empty text, and caching "no" from that would mean a
// transient failure silently cost every later build its whole progress stream.
func (a *adapter) invokeRememberJSONEvents(rootHelp string, present bool) {
	if strings.TrimSpace(rootHelp) == "" {
		return
	}
	state := invokeEventsNo
	if present {
		state = invokeEventsYes
	}
	a.capMu.Lock()
	a.jsonEvents = state
	a.capMu.Unlock()
}

// invokeJSONEventsSupported reports whether this binary understands
// --json-events, so Build can leave the flag off one that does not.
//
// It matters because cobra does not shrug at an unknown flag: a debark that
// predates --json-events answers it with an error and cobra's usage block, exit
// 1. Passing the flag unconditionally therefore turned every build against such
// a binary into a usage failure — instead of the build running with no progress
// stream, which is the degraded mode Capabilities.JSONEvents was defined for.
//
// The answer is memoised for the life of the adapter, because it is a fact
// about one binary at one path and that path is fixed once discovered. It is
// filled for free by Probe, which the application calls on the cold-start path,
// so a build normally pays nothing here; a build that got here first pays one
// `--help`, about 60 ms against a job measured in minutes.
//
// On no evidence at all — a binary that would not answer `--help` — it reports
// true. That is the pre-existing behaviour, and it is the right way to be
// wrong: a binary this adapter cannot even ask is one the build is about to
// fail on anyway, whereas guessing "absent" would cost a modern debark its
// entire event stream every time the probe hiccuped.
func (a *adapter) invokeJSONEventsSupported(ctx context.Context) bool {
	a.capMu.Lock()
	state := a.jsonEvents
	a.capMu.Unlock()
	switch state {
	case invokeEventsYes:
		return true
	case invokeEventsNo:
		return false
	}

	rootHelp, ok := a.invokeHelpOK(ctx, []string{ProgramName, "--help"})
	if !ok || strings.TrimSpace(rootHelp) == "" {
		return true
	}
	present := invokeHasFlag(rootHelp, "--json-events")
	a.invokeRememberJSONEvents(rootHelp, present)
	return present
}

// invokeHasSubcommand reports whether cobra's help text lists name under
// "Available Commands:".
//
// Scoped to that block on purpose. A command's long description is prose and
// mentions other commands by name; matching anywhere in the text would report
// capabilities a binary does not have.
func invokeHasSubcommand(help, name string) bool {
	inBlock := false
	for _, line := range invokeLines(help) {
		if !invokeIsIndented(line) {
			inBlock = strings.EqualFold(strings.TrimSpace(line), "Available Commands:")
			continue
		}
		if !inBlock {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

// invokeHasFlag reports whether cobra's help text declares flag in one of its
// "Flags:" or "Global Flags:" blocks.
//
// Scoped to those blocks for the same reason as invokeHasSubcommand, and here
// it is not hypothetical: `build --help`'s own description names --snapshot,
// --base, --list and --local-dir in a sentence about how to use them.
func invokeHasFlag(help, flag string) bool {
	inBlock := false
	for _, line := range invokeLines(help) {
		if !invokeIsIndented(line) {
			t := strings.TrimSpace(line)
			inBlock = strings.EqualFold(t, "Flags:") || strings.EqualFold(t, "Global Flags:")
			continue
		}
		if !inBlock {
			continue
		}
		t := strings.TrimSpace(line)
		// A flag line is either "--long ..." or "-s, --long ...".
		if i := strings.Index(t, "--"); i >= 0 {
			t = t[i:]
		}
		if !strings.HasPrefix(t, flag) {
			continue
		}
		rest := t[len(flag):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '=' || rest[0] == ',' {
			return true
		}
	}
	return false
}

func invokeLines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// invokeIsIndented reports whether a help line belongs to the block above it
// rather than starting a new one. An empty line belongs to whatever block is
// open, because cobra separates entries within a block with blank lines but
// always starts a new block with an unindented heading.
func invokeIsIndented(line string) bool {
	if strings.TrimSpace(line) == "" {
		return true
	}
	return line[0] == ' ' || line[0] == '\t'
}

// ---------------------------------------------------------------------------
// ListBases
// ---------------------------------------------------------------------------

// ListBases wraps `snapshot list-bases --json`.
func (a *adapter) ListBases(ctx context.Context, arch string) (*base.List, error) {
	list, err := a.invokeListBases(ctx, arch)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// invokeListBases is ListBases with the concrete error type, so Probe can use
// it without re-extracting one.
//
// An empty arch means "whatever debark would default to", and --arch is then
// NOT passed. Supplying this application's own base.HostArch() instead would
// substitute a fact about the GUI for a fact about debark, which is exactly
// what Probe.HostArch's documentation forbids; the returned List.Arch is the
// answer.
func (a *adapter) invokeListBases(ctx context.Context, arch string) (*base.List, *Error) {
	parts := []string{"snapshot", "list-bases"}
	if arch != "" {
		parts = append(parts, "--arch", arch)
	}
	argv := invokeArgs(parts...)

	out, err := a.invoke(ctx, argv)
	if err != nil {
		return nil, err
	}
	list := new(base.List)
	if e := invokeDecode(argv, out, list); e != nil {
		return nil, e
	}
	if e := invokeSchemaCheck(argv, "snapshot list-bases --json", list.SchemaVersion, base.ListSchemaVersion); e != nil {
		return nil, e
	}
	return list, nil
}

// ---------------------------------------------------------------------------
// InspectSnapshot
// ---------------------------------------------------------------------------

// InspectSnapshot wraps `snapshot inspect --json`. Nothing is uploaded
// anywhere.
//
// The path is carried back in SnapshotInfo because the document does not
// contain it: `snapshot inspect --json` prints the bare debark.snapshot/v1
// object, unlike `inspect --json`, whose envelope carries bundle_path. It is
// the path as this process was given it, from the argv that ran, so what the UI
// shows next to the snapshot's contents is the file the operator picked.
func (a *adapter) InspectSnapshot(ctx context.Context, path string) (SnapshotInfo, error) {
	argv := invokeArgs("snapshot", "inspect", path)

	out, err := a.invoke(ctx, argv)
	if err != nil {
		return SnapshotInfo{}, err
	}
	var snap snapshot.Snapshot
	if e := invokeDecode(argv, out, &snap); e != nil {
		return SnapshotInfo{}, e
	}
	if e := invokeSchemaCheck(argv, "snapshot inspect --json", snap.SchemaVersion, snapshot.SchemaVersion); e != nil {
		return SnapshotInfo{}, e
	}
	return SnapshotInfo{Path: path, Snapshot: snap}, nil
}

// ---------------------------------------------------------------------------
// Keygen
// ---------------------------------------------------------------------------

// Keygen wraps `keygen --json`, which emits an ad-hoc map[string]string with
// keys key_id, private_key and public_key, and no schema_version. KeyInfo is
// that map given a name and matching tags, so the decode is one step.
//
// --out is marked required and cobra refuses the command without it. That is
// checked here rather than shelled out, so an empty path produces a sentence
// instead of two kilobytes of help text.
func (a *adapter) Keygen(ctx context.Context, outPath string) (KeyInfo, error) {
	argv := invokeArgs("keygen", "--out", outPath)
	if strings.TrimSpace(outPath) == "" {
		return KeyInfo{}, newError(argv, int(dferr.Usage), "",
			"no path was given for the new signing key",
			"choose where to write the private key; the public key is written alongside it, and only the public key ever goes to the target")
	}

	out, err := a.invoke(ctx, argv)
	if err != nil {
		return KeyInfo{}, err
	}
	var key KeyInfo
	if e := invokeDecode(argv, out, &key); e != nil {
		return KeyInfo{}, e
	}
	if key.KeyID == "" || key.PrivateKeyPath == "" {
		return KeyInfo{}, invokeOutputError(argv, nil, fmt.Sprintf(
			"%s reported a new key without saying what it is", ProgramName))
	}
	if key.PublicKeyPath == "" {
		// Belt and braces for a debark that stopped echoing the public path.
		// The rule is core/sign's own, through its exported suffixes, rather
		// than a hard-coded ".pub".
		key.PublicKeyPath = invokePublicKeyPath(key.PrivateKeyPath)
	}
	return key, nil
}

// invokePublicKeyPath derives the public key path keygen writes alongside a
// private key, using core/sign's suffixes rather than a literal.
func invokePublicKeyPath(privPath string) string {
	if strings.HasSuffix(privPath, sign.PrivateKeyFileSuffix) {
		return strings.TrimSuffix(privPath, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix
	}
	return privPath + sign.PublicKeyFileSuffix
}

// PublicKeyPath is invokePublicKeyPath, exported.
//
// It is `keygen`'s own rule — the private path with core/sign's .key suffix
// replaced by .pub, or .pub appended when there was no .key suffix — and it is
// exported so that internal/app can name the public key beside the operator's
// signing key without writing the rule a third time. It was already written
// twice: here, and in internal/readiness, which cannot import this package.
// Two copies of a rule this small is tolerable; three is how they drift.
func PublicKeyPath(privateKeyPath string) string { return invokePublicKeyPath(privateKeyPath) }

// ---------------------------------------------------------------------------
// Verify
// ---------------------------------------------------------------------------

// Verify wraps `verify --json`.
//
// # verify is a second silent-error command, and cli-surface.md does not say so
//
// docs/dev/cli-surface.md C1 documents `build` as "the exception": it prints
// its --json document to stdout and then returns a silent error, leaving stderr
// empty. Measured here against the real binary, `verify` behaves exactly the
// same way — a bundle whose signature is untrusted prints a complete
// debark.verifyreport/v1 document to stdout, exits 4, and writes NOTHING to
// stderr. The capture is testdata/json/verify-untrusted.json.
//
// So the report is decoded whether or not the process succeeded, and the
// error's generic class description is replaced with the first problem the
// report names. Both are returned, and callers must read both — which is what
// the Adapter interface already says.
func (a *adapter) Verify(ctx context.Context, bundlePath string) (VerifyReport, error) {
	return a.VerifyWithKeys(ctx, bundlePath, nil)
}

// VerifyWithKeys is Verify, told which public keys to trust. It satisfies
// KeyedVerifier; see that type for why the extension exists rather than a
// change to Adapter.Verify.
//
// Each key becomes one --key FILE. An empty list passes none, which leaves
// debark's own configured verify_keys/verify_keyring_dirs in charge — and is
// exactly what Verify does, so nothing about the frozen method changes.
func (a *adapter) VerifyWithKeys(ctx context.Context, bundlePath string, keys []string) (VerifyReport, error) {
	parts := []string{"verify", bundlePath}
	for _, k := range keys {
		if k != "" {
			parts = append(parts, "--key", k)
		}
	}
	argv := invokeArgs(parts...)

	out, runErr := a.invoke(ctx, argv)

	var rep VerifyReport
	decodeErr := invokeDecode(argv, out, &rep)
	if decodeErr == nil {
		if e := invokeSchemaCheck(argv, "verify --json", rep.SchemaVersion, verify.SchemaVersion); e != nil {
			decodeErr = e
			rep = VerifyReport{}
		}
	}

	if runErr != nil {
		if decodeErr != nil {
			// Nothing usable came back; the exit code and stderr are all there
			// is, which is the case NewError was shaped for.
			return VerifyReport{}, runErr
		}
		if s := invokeVerifySummary(rep); s != "" {
			runErr = runErr.WithSummary("%s", s)
		}
		return rep, runErr
	}
	if decodeErr != nil {
		return VerifyReport{}, decodeErr
	}
	if !rep.OK {
		// Belt and braces: a report that says the bundle did not pass must
		// never be returned as a success, whatever the process claimed.
		e := classify(argv, int(dferr.Verification), "")
		if s := invokeVerifySummary(rep); s != "" {
			e = e.WithSummary("%s", s)
		}
		return rep, e
	}
	return rep, nil
}

// invokeVerifySummary turns a failing report into the sentence an operator
// reads, so a verification failure does not fall back to dferr's generic
// "verification failed: signature, digest or repository metadata mismatch".
func invokeVerifySummary(rep VerifyReport) string {
	if rep.OK || len(rep.Problems) == 0 {
		return ""
	}
	first := ""
	for _, p := range rep.Problems {
		if p.Message != "" {
			first = p.Message
			break
		}
		if p.Kind != "" {
			first = p.Kind
			break
		}
	}
	if first == "" {
		return ""
	}
	if n := len(rep.Problems) - 1; n > 0 {
		return fmt.Sprintf("the bundle did not verify: %s (and %d more problem%s)", first, n, invokePlural(n))
	}
	return "the bundle did not verify: " + first
}

func invokePlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------------------
// Command
// ---------------------------------------------------------------------------

// Command returns the exact argv a spec would run, without running it.
//
// It is BuildArgv and nothing else, so this and internal/cliadapter/fake cannot
// disagree about what the UI shows against what actually runs. Rule 8 of the
// contract brief depends on that.
func (a *adapter) Command(spec BuildSpec) []string { return BuildArgv(spec) }
