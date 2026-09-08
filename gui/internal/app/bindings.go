package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/verify"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/cliadapter"
	"github.com/inferops/debark/gui/internal/export"
	"github.com/inferops/debark/gui/internal/readiness"
)

// This file is frozen contract 3: the exact set of Go methods the frontend may
// call. Five frontend packages and five backend packages build against it at the
// same time, so it is written down once, completely, before either side
// starts, and it does not change.
//
// # The rules, and why each one exists
//
// **One file holds the surface.** Every exported method on App is here. The
// packages that own the subsystems behind it (internal/catalog,
// internal/cliadapter, internal/export, internal/readiness) must NOT add
// exported methods to App from their own files: two packages adding the same method name is a compile
// error, and a surface spread across five files cannot be frozen at all. Where
// a subsystem is not finished, the method here is a stub marked `TODO` and
// the *signature* is already right. That is what the other packages build on.
//
// **A bound method returns a view type, never a backend type.** Nothing from
// `core/`, `internal/catalog`, `internal/cliadapter`, `internal/export` or
// `internal/readiness` crosses the bridge. types.go holds every type that
// does. The bridge is a serialisation boundary, so that a backend refactor is
// not a frontend change and a schema bump in the core repository does not
// silently rename a field the JavaScript reads.
//
// The one case worth arguing is `buildjob.BuildResult`: it is a versioned wire
// type that exists purely to be serialised, and passing it straight through
// would have been defensible. It is projected into BuildSummary anyway,
// because the GUI adds fields the core type has no room for (the argv that
// ran, whether the operator cancelled, wall time) and because pinning the
// frontend to `debark.buildjob/v1` field names would make a core schema bump
// a frontend change — exactly what this boundary exists to prevent. The same
// argument applies to `evidence.Event`, and there the projection additionally
// carries something the decoded struct cannot: the raw line, verbatim, for the
// details drawer to show and copy.
//
// **No bound method returns Go `error`.** Wails maps a non-nil error to a
// promise rejection carrying only `err.Error()` — a string. That would throw
// away the hint, the argv, the captured stderr and the exit class, which is
// precisely the information that turns an exit code into an actionable
// sentence. So every fallible method returns a struct with an `error` field
// holding a *UIError, and its promise resolves either way. The frontend
// pattern is uniform:
//
//	const r = await App.SearchPackages(q);
//	if (r.error) { showError(r.error); return; }
//
// **No bound method blocks for longer than it must.** Three tiers, and the
// documentation names which one every method is in:
//
//   - fast — pure memory, microseconds. Status reads, the selection tray.
//   - quick — bounded work with a deadline: one subprocess, one page of an
//     in-memory index, one directory walk. Target: under 500 ms, hard timeout
//     applied here.
//   - fire-and-forget — starts work, returns immediately, reports through
//     events. Catalogue build, bundle build, drive copy, verification,
//     readiness.
//
// The brief says "never blocks longer than a frame". Taken literally that
// forbids `ListBases`, which is one process spawn, and would push a 60 ms call
// into a start/status/event triple that no screen benefits from. The honest
// rule is the three tiers above: nothing *unbounded* ever blocks a call, and
// every bounded call carries a deadline. Native file dialogs are the deliberate
// exception — they block until the operator dismisses them, which is what a
// modal is.
//
// **Every long-running operation is a start/cancel/status triple.** Status
// exists because events are not a source of truth: a screen that mounts late, a
// drawer that reopens, or a reloaded window has missed everything emitted so
// far. Cancel is always safe to call when nothing is running.
//
// **No unbounded slice of catalogue rows, in either direction.** Every list
// method is paged or explicitly clamped; the limits are constants in types.go.

// ---------------------------------------------------------------------------
// Dependencies.
// ---------------------------------------------------------------------------

// ReadinessChecker is the readiness subsystem as this package uses it.
// *readiness.Checker satisfies it; a test supplies a stub.
type ReadinessChecker interface {
	Run(ctx context.Context) readiness.Report
	RunOne(ctx context.Context, id string) (readiness.Result, error)
}

// CommandRunner runs one external command for a readiness remedy. It is
// readiness.ExecRunner in production and a recording stub in tests.
type CommandRunner func(ctx context.Context, name string, args ...string) readiness.CommandOutput

// TargetResolver turns the operator's choice into the catalogue's Target.
//
// It is a seam rather than a function here because of a real gap in the
// contracts, and the gap is worth stating plainly. catalog.Target requires
// Sources — parsed deb822 apt sources — and docs/dev/catalogue-sourcing.md
// settles that the catalogue fetches indexes directly and so genuinely needs
// them. Nothing hands them over:
//
//   - base.ListEntry deliberately omits Sources, so `snapshot list-bases
//     --json` does not carry a stock base's archive URIs, suites or components
//     at all. There is nothing for a parser to parse.
//   - snapshot.APT.Sources carries the target's sources.list FILES by path
//     inside the snapshot archive, not parsed stanzas.
//
// So two things are missing: a deb822 parser, which internal/catalog owns per
// catalogue-sourcing.md, and this join, which nobody in the ownership table
// owns. The bound surface here (internal/app) is the natural home. Underneath
// both sits a core-repo gap that the definition of done explicitly allows
// for — the CLI lacking a --json field the GUI needs.
//
// Until it is set, the built-in resolver fills every identity field from the
// CLI and leaves Sources empty. That is enough to drive every screen against
// the fakes; a real catalogue build will refuse it with a message that says so.
type TargetResolver interface {
	// Resolve returns the target, plus every apt source that could not be
	// used.
	//
	// The problems are part of the answer, not a diagnostic aside. A source
	// the parser could not read confidently is a whole archive missing from
	// the catalogue, and docs/dev/catalogue-sourcing.md is explicit that this
	// is the one failure here an operator cannot see for themselves: the
	// picker simply has fewer rows, with nothing on screen to say why. An
	// earlier version of this interface returned only (Target, error), which
	// silently discarded exactly what the sources parser exists to surface.
	//
	// Some problems are deliberate scope limits rather than defects —
	// source-only stanzas, flat repositories, sources for another
	// architecture, and stanzas the operator disabled. catalog.SourceProblem
	// distinguishes them with Deliberate(); the UI should show the two
	// differently rather than alarming someone about a `deb-src` line.
	//
	// Problems come back alongside a non-nil error too: when resolution fails
	// because nothing usable was left, they are the explanation.
	Resolve(ctx context.Context, sel TargetSelection) (catalog.Target, []catalog.SourceProblem, error)
}

// CatalogInsight is the optional, additive half of frozen contract 2: facts a
// catalogue can state about itself that the frozen Catalog interface has no
// method for.
//
// A type assertion rather than a wider Catalog, deliberately, and the reasoning
// is worth stating because the contract brief calls a case like this "a missing
// contract" and says to report the decision rather than coordinate around it.
//
// Contract 2 is frozen and answers one question — "what can I install on this
// target?" — with five methods that every implementation, including the fake
// five frontend packages were built against, must supply. None of the three
// facts below is needed to answer that question, and none is knowable to an
// implementation that holds no cache: a catalogue that never wrote one has no
// build time, and only a cache-loaded catalogue can be stale at all, which is
// precisely why Stale was written as a method on the unexported implementation
// in the first place. Widening the frozen interface would make three optional
// facts compulsory for every implementation forever, to reach one banner.
// Probing for them costs one type assertion and leaves a Catalog that does not
// implement it working exactly as before, with the fields at their documented
// "not ready" values.
//
// All three or none: an implementation that can answer one of these can answer
// the rest, and splitting them into an interface each would let a partial
// implementation report a package count beside a build time it silently
// dropped.
//
// internal/catalog's own implementation and internal/catalog/fake both satisfy
// it. See docs/dev/binding-surface.md, "CatalogStatus".
type CatalogInsight interface {
	// PackageCount is how many rows the catalogue holds, 0 when not ready.
	PackageCount() int
	// Stale is true when a usable cache exists but the archive has published
	// new indexes since it was written.
	Stale() bool
	// BuiltAt is when the catalogue was written, zero when there is none.
	BuiltAt() time.Time
}

// catalogFacts reads the optional half of contract 2 off a Catalog, or returns
// the documented not-ready values for one that does not implement it.
//
// Every assignment to App.catalogStatus that can leave Ready true goes through
// this. There are two of them — the warm path in SelectTarget and the build
// path in StartCatalogBuild — and the reason this is a function rather than
// three lines at each is that the defect it fixes was exactly one of those two
// sites having been written and the other forgotten: PackageCount was assigned
// at neither, BuiltAt and Stale only at the build path, where Stale can only
// ever be false because the catalogue was just rebuilt.
func catalogFacts(cat catalog.Catalog) (count int, stale bool, builtAt string) {
	ins, ok := cat.(CatalogInsight)
	if !ok {
		return 0, false, ""
	}
	if t := ins.BuiltAt(); !t.IsZero() {
		builtAt = t.UTC().Format(time.RFC3339)
	}
	return ins.PackageCount(), ins.Stale(), builtAt
}

// SelectionPolicy decides what is worth warning the operator about at the
// moment they add something, rather than at the end of a build.
//
// The snap-transitional case is why it exists: on Ubuntu desktop, `firefox` in
// the archive is a stub that pulls in a snap, so a bundle containing it does
// not contain a browser. Nothing in the catalogue's data says so — it is
// product knowledge, and it belongs to the code that owns the tray.
//
// Implementations must be fast and must not do I/O: this runs on the add path.
// A nil policy means no warnings, which is a working default and not an error.
type SelectionPolicy interface {
	// WarnFor returns the warnings that apply to one entry as it is added.
	WarnFor(target TargetView, entry SelectionEntry) []SelectionWarning
	// WarnForSelection returns the warnings that apply to the tray as a whole
	// — total size, mixed architectures, and so on.
	WarnForSelection(target TargetView, summary SelectionSummary) []SelectionWarning
}

// Deps is everything App needs from the outside. Every field is an interface
// or a function, so the whole application runs against fakes: that is what lets
// ten packages develop in parallel and what makes the screens testable without a
// desktop, a network or a debark binary.
type Deps struct {
	// CLI is the single seam through which every debark invocation passes.
	// Required. internal/cliadapter/fake satisfies it.
	CLI cliadapter.Adapter
	// Catalog is the browsing index. Required.
	// internal/catalog/fake satisfies it.
	Catalog catalog.Catalog
	// Readiness runs the first-run environment checks. Optional: when nil, a
	// default *readiness.Checker is constructed.
	Readiness ReadinessChecker
	// Runner runs a readiness remedy command. Optional: defaults to
	// readiness.ExecRunner.
	Runner CommandRunner
	// Resolver turns a TargetSelection into a catalog.Target. Optional; see
	// TargetResolver for what the default cannot do.
	Resolver TargetResolver
	// Policy supplies selection-time warnings. Optional; nil means none.
	Policy SelectionPolicy
	// Emit delivers events. Optional: Startup installs the Wails emitter. A
	// headless harness sets it to collect events instead.
	Emit Emitter
	// Version is this GUI's version string, for AppInfo. Optional.
	Version string
	// SigningKeyPath is where the readiness check looks for the operator's
	// key, and the default path the keygen remedy writes to. Optional.
	SigningKeyPath string
}

// ---------------------------------------------------------------------------
// App.
// ---------------------------------------------------------------------------

// job is the state common to every long-running operation: is it running, how
// do we stop it, and which run is this. gen exists so that a goroutine from a
// cancelled run cannot write its result over a newer run's state — the classic
// way a cancelled operation appears to succeed thirty seconds later.
type job struct {
	running bool
	cancel  context.CancelFunc
	gen     uint64
	done    chan struct{}
}

// start marks a new run and returns its generation and a cancellable context.
func (j *job) start(parent context.Context) (context.Context, uint64) {
	ctx, cancel := context.WithCancel(parent)
	j.running = true
	j.cancel = cancel
	j.gen++
	j.done = make(chan struct{})
	return ctx, j.gen
}

// stop cancels the current run if there is one. Safe when idle.
func (j *job) stop() bool {
	if !j.running || j.cancel == nil {
		return false
	}
	j.cancel()
	return true
}

// finish clears the run if gen is still current.
func (j *job) finish(gen uint64) bool {
	if j.gen != gen || !j.running {
		return false
	}
	j.running = false
	j.cancel = nil
	if j.done != nil {
		close(j.done)
		j.done = nil
	}
	return true
}

// wailsContext wraps the lifecycle context so the emitter cannot be built from
// an arbitrary one. runtime.EventsEmit calls log.Fatalf — it kills the process,
// it does not return an error — when handed a context that did not come from a
// Wails lifecycle hook, so "is this the real one" is a fact worth making
// unforgeable rather than a nil check repeated in twenty places.
type wailsContext struct{ ctx context.Context }

// App is the bound struct. Everything the frontend can do, it does through a
// method on this type.
type App struct {
	mu sync.RWMutex

	// Set by Startup.
	ctx          context.Context
	runtimeReady bool
	emitter      Emitter

	droppedEvents atomic.Int64

	// Dependencies, all interface-typed.
	cli      cliadapter.Adapter
	cat      catalog.Catalog
	checker  ReadinessChecker
	runner   CommandRunner
	resolver TargetResolver
	policy   SelectionPolicy

	version    string
	keyPath    string
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// Cached CLI probe.
	probe     cliadapter.Probe
	probed    bool
	probeErr  *UIError
	probeOnce sync.Mutex

	// Screen state. Guarded by mu unless noted.
	target    TargetView
	catTarget catalog.Target

	readinessJob    job
	readinessReport ReadinessReport
	// readinessRaw is the last report the checker produced, kept beside its
	// projection so a single re-check can be folded in with
	// readiness.Report.With — which re-derives the build-environment row.
	// Only that package can compute it, and the view type has thrown away
	// what it needs by the time it reaches the frontend.
	readinessRaw readiness.Report

	catalogJob    job
	catalogStatus CatalogStatus

	buildJob    job
	buildState  BuildStatus
	buildLog    eventRing
	buildBundle string

	exportJob    job
	exportStatus ExportStatus

	verifyJob    job
	verifyStatus VerifyStatus

	sel               selection
	targetGeneration  uint64
	targetFingerprint string
	selectionRevision uint64
	completeTarget    uint64
	completeSelection uint64
	undo              selectionUndo
	targetPending     bool
	targetCancel      context.CancelFunc
	stopping          bool
	closePending      bool
	closeAllowed      bool
	closeError        *UIError
	closeTimeout      time.Duration
	confirmClose      func(context.Context, bool) (bool, error)
	quit              func(context.Context)
	shutdownOnce      sync.Once
}

// New builds the application. It performs no I/O and starts no goroutines, so
// that cold start stays inside its 1.5 s budget: everything expensive is a
// method the frontend calls when the screen that needs it appears.
func New(deps Deps) *App {
	// The two page-size ceilings must agree; a frontend that trusts this
	// package's constant and a catalogue that clamps to its own would page
	// past the end of a result set forever.
	if SearchPageMax != catalog.MaxPageSize || SearchPageDefault != catalog.DefaultPageSize {
		panic("app: paging constants have drifted from internal/catalog")
	}

	a := &App{
		cli:      deps.CLI,
		cat:      deps.Catalog,
		checker:  deps.Readiness,
		runner:   deps.Runner,
		resolver: deps.Resolver,
		policy:   deps.Policy,
		version:  deps.Version,
		keyPath:  deps.SigningKeyPath,
		emitter:  deps.Emit,
	}
	if a.version == "" {
		a.version = "dev"
	}
	if a.keyPath == "" {
		// The readiness checker defaults this internally, so leaving it empty
		// meant the screen looked at one path and the rest of the application
		// knew of none — the verification screen in particular could not name
		// the public key the bundle had just been signed with. Reading the
		// default here makes both halves talk about the same file.
		a.keyPath = readiness.DefaultSigningKeyPath()
	}
	if a.runner == nil {
		a.runner = readiness.ExecRunner
	}
	if a.checker == nil {
		// Locator closes the seam internal/readiness documents on
		// BinaryLocator: that package deliberately does not import
		// internal/cliadapter, and says the application layer — the one place
		// that holds both — must make the two agree. See readinessLocate.
		// It is a function value, so New still touches nothing.
		a.checker = readiness.New(readiness.Options{
			SigningKeyPath: a.keyPath,
			Locator:        a.readinessLocate,
		})
	}
	if a.resolver == nil {
		a.resolver = defaultResolver{cli: deps.CLI}
	}
	a.baseCtx, a.baseCancel = context.WithCancel(context.Background())
	a.buildLog.limit = BuildLogRing
	a.sel.init()
	a.selectionRevision = 1
	a.closeTimeout = 10 * time.Second
	a.confirmClose = nativeConfirmClose
	a.quit = wruntime.Quit
	a.catalogStatus = CatalogStatus{State: CatalogStateNone, Progress: CatalogProgress{Fraction: -1, OverallFraction: -1}}
	a.readinessReport = ReadinessReport{Platform: runtime.GOOS + "/" + runtime.GOARCH}
	return a
}

// Bind returns what main.go passes to options.App.Bind.
//
// It is a method on App because main.go is written against exactly New and
// Bind. Wails will therefore generate a JavaScript stub for it as well; that
// stub is not part of this contract and the frontend must never call it.
func (a *App) Bind() []any { return []any{a} }

// Startup is the OnStartup hook. main.go passes it as options.App.OnStartup,
// which is also what keeps Wails from binding it: the runtime exempts the
// lifecycle hooks from binding generation.
func (a *App) Startup(ctx context.Context) {
	ready := ctx != nil && ctx.Value("events") != nil && ctx.Value("frontend") != nil

	a.mu.Lock()
	a.ctx = ctx
	a.runtimeReady = ready
	if a.emitter == nil && ready {
		a.emitter = wailsEmitter(wailsContext{ctx: ctx})
	}
	a.mu.Unlock()
}

// Shutdown is the OnShutdown hook. It cancels every job in flight and closes
// the catalogue, so a window closed mid-build does not leave a subprocess and a
// mapped cache file behind.
func (a *App) Shutdown(_ context.Context) {
	a.shutdownOnce.Do(func() {
		a.mu.Lock()
		a.stopping = true
		a.stopJobsLocked()
		cancel, cat := a.baseCancel, a.cat
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		// Native close already waited. During an external shutdown, keep
		// cleanup off the UI thread and never unmap a catalogue still in use.
		cleanup := func() {
			for !a.jobsStopped() {
				time.Sleep(20 * time.Millisecond)
			}
			if cat != nil {
				_ = cat.Close()
			}
		}
		if a.jobsStopped() {
			cleanup()
		} else {
			go cleanup()
		}
	})
}

// BeforeClose is Wails' native close hook. It returns immediately and vetoes
// closure while the asynchronous confirmation/cancellation policy runs.
func (a *App) BeforeClose(ctx context.Context) bool {
	a.mu.Lock()
	if a.closeAllowed {
		a.mu.Unlock()
		return false
	}
	if a.closePending {
		a.mu.Unlock()
		return true
	}
	a.closePending = true
	a.closeError = nil
	view := a.lifecycleLocked()
	a.mu.Unlock()
	go a.runClose(ctx, view)
	return true
}

// RequestClose follows the same policy as the native window close button.
func (a *App) RequestClose() Result {
	a.mu.RLock()
	ctx, ready := a.ctx, a.runtimeReady
	a.mu.RUnlock()
	if !ready {
		return failed(errNoRuntime("close the window"))
	}
	a.BeforeClose(ctx)
	return ok()
}

func (a *App) LifecycleStatus() LifecycleView {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lifecycleLocked()
}

func (a *App) lifecycleLocked() LifecycleView {
	return LifecycleView{
		BuildRunning: a.buildJob.running, ExportRunning: a.exportJob.running,
		Stopping: a.stopping, TargetGeneration: a.targetGeneration,
		SelectionRevision: a.selectionRevision, Error: a.closeError,
		UnsavedSelection: a.sel.len() > 0 && (a.completeTarget != a.targetGeneration || a.completeSelection != a.selectionRevision),
	}
}

func (a *App) runClose(ctx context.Context, view LifecycleView) {
	if view.BuildRunning || view.ExportRunning || view.UnsavedSelection {
		accepted, err := a.confirmClose(ctx, view.BuildRunning || view.ExportRunning)
		if err != nil {
			a.closeFailed(dialogError(err))
			return
		}
		if !accepted {
			a.mu.Lock()
			a.closePending = false
			a.mu.Unlock()
			a.emitLifecycle()
			return
		}
	}
	a.mu.Lock()
	// Nothing can start or mutate while confirmation is visible. This also
	// closes the completion/confirmation race without a second unsaved prompt.
	a.stopping = true
	a.stopJobsLocked()
	timeout := a.closeTimeout
	a.mu.Unlock()
	a.emitLifecycle()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !a.jobsStopped() {
		select {
		case <-timer.C:
			a.closeFailed(&UIError{Code: ErrCodeBusy, Message: "Work has not stopped yet. Debark is still open.", Hint: "Wait for the running operation to finish, then try Quit again.", Retryable: true})
			return
		case <-tick.C:
		}
	}
	a.mu.Lock()
	a.closeAllowed = true
	a.mu.Unlock()
	a.quit(ctx)
}

func (a *App) closeFailed(err *UIError) {
	a.mu.Lock()
	a.stopping, a.closePending, a.closeAllowed = false, false, false
	a.closeError = err
	a.mu.Unlock()
	a.emitLifecycle()
}

func (a *App) stopJobsLocked() {
	if a.targetCancel != nil {
		a.targetCancel()
	}
	a.readinessJob.stop()
	a.catalogJob.stop()
	a.buildJob.stop()
	a.exportJob.stop()
	a.verifyJob.stop()
}

func (a *App) jobsStopped() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.readinessJob.running && !a.catalogJob.running && !a.buildJob.running && !a.exportJob.running && !a.verifyJob.running && !a.targetPending
}

func (a *App) mutationErrorLocked() *UIError {
	if a.stopping || a.closePending {
		return errBusy("Finish closing Debark or choose Keep working before editing.")
	}
	if a.buildJob.running || a.exportJob.running {
		return errBusy("Wait for the build or copy to finish before editing.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Meta.
// ---------------------------------------------------------------------------

// AppInfo returns what the About panel and the footer show. Quick: it probes
// the debark binary once and caches the answer.
func (a *App) AppInfo() AppInfo {
	a.mu.RLock()
	info := AppInfo{
		// The product name, as a person reads it. Deliberately not the module
		// or binary name: the on-disk identifiers (the cache directory, the
		// export tool field) stay "debark-gui" because renaming those would
		// orphan every existing cache and change a written marker.
		Name:     "Debark",
		Version:  a.version,
		Platform: runtime.GOOS,
		Arch:     runtime.GOARCH,
		Headless: !a.runtimeReady,
	}
	a.mu.RUnlock()

	p, uerr := a.cliProbe()
	if uerr != nil {
		info.Error = uerr
		return info
	}
	info.DebarkPath = p.Path
	info.DebarkVersion = p.Version()
	info.ProgressEvents = p.Capabilities.JSONEvents
	return info
}

// cliProbe probes the binary once and caches the result. Quick, with a
// deadline: a probe that hangs must not wedge the About panel.
func (a *App) cliProbe() (cliadapter.Probe, *UIError) {
	a.probeOnce.Lock()
	defer a.probeOnce.Unlock()

	a.mu.RLock()
	done, p, perr, cli := a.probed, a.probe, a.probeErr, a.cli
	a.mu.RUnlock()
	if done {
		return p, perr
	}
	if cli == nil {
		return cliadapter.Probe{}, errNoDep("the debark command-line tool")
	}

	ctx, cancel := context.WithTimeout(a.base(), 10*time.Second)
	defer cancel()
	got, err := cli.Probe(ctx)

	var uerr *UIError
	if err != nil {
		uerr = uiErrorFrom(err)
		uerr.Code = ErrCodeCLIMissing
	}
	a.mu.Lock()
	a.probe, a.probeErr, a.probed = got, uerr, true
	a.mu.Unlock()
	return got, uerr
}

// CopyToClipboard puts text on the system clipboard. It exists so the UI can
// offer "copy this command" beside every command it shows — rule 8, the CLI
// stays the complete interface. Fast.
func (a *App) CopyToClipboard(text string) Result {
	a.mu.RLock()
	ctx, ready := a.ctx, a.runtimeReady
	a.mu.RUnlock()
	if !ready {
		return failed(errNoRuntime("copy to the clipboard"))
	}
	if err := wruntime.ClipboardSetText(ctx, text); err != nil {
		return failed(&UIError{
			Code:    ErrCodeInternal,
			Message: "The clipboard could not be written.",
			Hint:    "Select the text and copy it by hand.",
			Details: err.Error(),
		})
	}
	return ok()
}

// RevealPath opens a file or folder in the operator's file manager. Fast; the
// file manager takes over from there.
func (a *App) RevealPath(path string) Result {
	a.mu.RLock()
	ctx, ready := a.ctx, a.runtimeReady
	a.mu.RUnlock()
	if !ready {
		return failed(errNoRuntime("open a file manager"))
	}
	if path == "" {
		return failed(errInvalid("No path was given to open.", "Build or export something first."))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if _, err := os.Stat(abs); err != nil {
		return failed(&UIError{
			Code:    ErrCodeInvalidInput,
			Message: fmt.Sprintf("There is nothing at %s to open.", abs),
			Hint:    "It may have been moved or deleted since it was made.",
			Details: err.Error(),
		})
	}
	// apt.FileURI, not a local copy and not concatenation. This file carried a
	// verbatim copy of core/apt's escaping rule for one wave, because the
	// function was unexported there; core 61b3e7b exported it precisely so
	// this copy could go, and two copies of one escaping rule drift invisibly
	// until a path with an awkward character reaches one of them.
	//
	// What it is protecting against, measured rather than imagined, for a path
	// straight out of a file dialog — the answer "file://" + ToSlash(abs)
	// gives:
	//
	//	"…/release #2"   -> path "…/release ", fragment "2"
	//	"…/what?now"     -> path "…/what",     query "now"
	//	"…/100%25 done"  -> path "…/100% done" — a different directory
	//	"…/a\tb"         -> does not parse as a URL at all
	//
	// Every one of those was handed to the OS URL handler as the folder the
	// operator asked to open. The GUI's own coverage of the two properties it
	// depends on stays in surface_test.go: the implementation is shared now,
	// the requirement is still this package's.
	wruntime.BrowserOpenURL(ctx, apt.FileURI(abs))
	return ok()
}

// ---------------------------------------------------------------------------
// Screen 1 — readiness and first run.
// ---------------------------------------------------------------------------

// Readiness returns the last readiness report. Fast: it never probes. Before
// the first StartReadinessCheck it returns an empty report with Checking false,
// which is the shell's cue to start one.
func (a *App) Readiness() ReadinessReport {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.readinessReport.clone()
}

// StartReadinessCheck runs every applicable check. Fire-and-forget.
//
// Events: readiness:started, then readiness:finished. The checks run
// concurrently inside the readiness package and finish together, so there is no
// meaningful per-check progress for a full pass; RecheckReadiness emits
// readiness:progress for the single-row case.
func (a *App) StartReadinessCheck() Result {
	a.mu.Lock()
	if a.stopping || a.closePending {
		a.mu.Unlock()
		return failed(errBusy("Debark is closing."))
	}
	if a.readinessJob.running {
		a.mu.Unlock()
		return failed(errBusy("A readiness check is already running."))
	}
	ctx, gen := a.readinessJob.start(a.baseCtx)
	a.readinessReport.Checking = true
	a.readinessReport.RunningCheck = ""
	report := a.readinessReport.clone()
	checker := a.checker
	a.mu.Unlock()

	a.emitReadinessStarted(report)

	go func() {
		rep := checker.Run(ctx)
		view := readinessView(rep, a.debarkPath())

		a.mu.Lock()
		current := a.readinessJob.finish(gen)
		if current {
			view.Checking = false
			a.readinessReport = view
			a.readinessRaw = rep
			view = view.clone()
		}
		a.mu.Unlock()
		if !current {
			return
		}
		a.emitReadinessFinished(ReadinessFinished{
			OK:        ctx.Err() == nil,
			Cancelled: ctx.Err() != nil,
			Report:    view,
		})
	}()
	return ok()
}

// RecheckReadiness re-runs one check — the "I started Docker, check again"
// path, which costs one probe rather than a whole report. Fire-and-forget.
//
// Events: readiness:progress for the row, then readiness:finished carrying the
// whole refreshed report, so the screen re-renders from one payload.
func (a *App) RecheckReadiness(checkID string) Result {
	if checkID == CheckBuildEnvironment {
		// Refused here rather than thirty milliseconds later in a goroutine:
		// there is nothing to run, so spinning the row and emitting a
		// started/finished pair for it would be theatre. ReadinessCheck.Derived
		// says the same thing on the row, so a screen need never get here.
		return failed(&UIError{
			Code:    ErrCodeInvalidInput,
			Message: "Whether this machine can build is worked out from the other rows, so it cannot be checked on its own.",
			Hint:    "Re-check apt, the container runtime or WSL instead. This row is recomputed with whichever one you fix.",
		})
	}
	return a.runReadiness(checkID, false, nil)
}

// CancelReadinessCheck stops a readiness pass. Safe to call when nothing is
// running, in which case it succeeds and does nothing.
func (a *App) CancelReadinessCheck() Result {
	a.mu.Lock()
	a.readinessJob.stop()
	a.mu.Unlock()
	return ok()
}

// RunReadinessAction runs the remedy attached to one check, then re-checks that
// row. Fire-and-forget. checkID names the row, not the action: a row has at
// most one remedy, and identifying it by the thing it fixes is what lets the UI
// call this straight from the row it is rendering.
//
// It refuses any action marked Elevated. This application does not escalate
// privilege — an operator who wants an elevated fix copies the command into a
// shell where they can see what it does. That is a deliberate posture: a
// desktop app that silently acquires root to be helpful is the thing this
// audience will not install.
//
// Events: readiness:progress, then readiness:finished with Action true.
func (a *App) RunReadinessAction(checkID string) Result {
	a.mu.RLock()
	report := a.readinessReport
	a.mu.RUnlock()

	var action *ReadinessAction
	for i := range report.Checks {
		if report.Checks[i].ID == checkID {
			action = report.Checks[i].Action
			break
		}
	}
	switch {
	case action == nil:
		return failed(&UIError{
			Code:    ErrCodeInvalidInput,
			Message: fmt.Sprintf("There is no one-command fix for %q.", checkID),
			Hint:    "Follow the remedy shown on that row, then re-check it.",
		})
	case !action.Runnable:
		return failed(&UIError{
			Code:    ErrCodeInvalidInput,
			Message: "That fix needs administrator rights, so this application will not run it.",
			Hint:    "Copy the command and run it in a terminal you have elevated yourself, then re-check the row.",
			Command: action.Command,
		})
	}
	return a.runReadiness(checkID, true, action)
}

// runReadiness is the shared body of RecheckReadiness and RunReadinessAction.
func (a *App) runReadiness(checkID string, isAction bool, action *ReadinessAction) Result {
	if checkID == "" {
		return failed(errInvalid("No check was named.", "Pick a row on the readiness screen."))
	}
	a.mu.Lock()
	if a.stopping || a.closePending {
		a.mu.Unlock()
		return failed(errBusy("Debark is closing."))
	}
	if isAction {
		if err := a.mutationErrorLocked(); err != nil {
			a.mu.Unlock()
			return failed(err)
		}
	}
	if a.readinessJob.running {
		a.mu.Unlock()
		return failed(errBusy("A readiness check is already running."))
	}
	ctx, gen := a.readinessJob.start(a.baseCtx)
	a.readinessReport.RunningCheck = checkID
	for i := range a.readinessReport.Checks {
		if a.readinessReport.Checks[i].ID == checkID {
			a.readinessReport.Checks[i].Running = true
		}
	}
	checker, runner := a.checker, a.runner
	a.mu.Unlock()

	a.emitReadinessProgress(ReadinessProgress{CheckID: checkID, Action: isAction, Done: 0, Total: 1,
		Message: readinessProgressMessage(isAction, action)})

	go func() {
		// Read once, outside the row loop below: it may probe the binary.
		bin := a.debarkPath()
		var actionErr *UIError
		if action != nil {
			out := runner(ctx, action.Command[0], action.Command[1:]...)
			if !out.OK() {
				actionErr = &UIError{
					Code:      ErrCodeCLI,
					Message:   out.Message(),
					Hint:      "Run the command yourself to see what it says, then re-check this row.",
					Command:   action.Command,
					Details:   action.Display,
					Retryable: true,
				}
			}
		}

		res, err := checker.RunOne(ctx, checkID)
		a.mu.Lock()
		current := a.readinessJob.finish(gen)
		if current {
			a.readinessReport.RunningCheck = ""
			if err == nil {
				// Through the readiness package's own With, not a merge of
				// view rows: With re-derives the build-environment row from
				// the new set, and that row is BLOCKING. Folding view rows
				// left it stale, so an operator who started Docker and
				// re-checked the container row saw it turn green while the
				// row that governs the build button stayed red — the wall
				// this screen exists to remove, reached by fixing the problem.
				a.readinessRaw = a.readinessRaw.With(res)
				refreshed := readinessView(a.readinessRaw, bin)
				refreshed.Checking = a.readinessReport.Checking
				a.readinessReport = refreshed
			}
			for i := range a.readinessReport.Checks {
				a.readinessReport.Checks[i].Running = false
			}
			a.readinessReport.Error = actionErr
		}
		view := a.readinessReport.clone()
		a.mu.Unlock()
		if !current {
			return
		}

		uerr := actionErr
		if uerr == nil && err != nil {
			uerr = &UIError{
				Code:    ErrCodeInvalidInput,
				Message: fmt.Sprintf("%q is not a check that can be re-run on its own.", checkID),
				Hint:    "Run the whole readiness check instead.",
				Details: err.Error(),
			}
		}
		a.emitReadinessProgress(ReadinessProgress{CheckID: checkID, Action: isAction, Done: 1, Total: 1})
		a.emitReadinessFinished(ReadinessFinished{
			OK:        uerr == nil && ctx.Err() == nil,
			Cancelled: ctx.Err() != nil,
			CheckID:   checkID,
			Action:    isAction,
			Report:    view,
			Error:     uerr,
		})
	}()
	return ok()
}

// ---------------------------------------------------------------------------
// Screen 2 — target selection.
// ---------------------------------------------------------------------------

// SupportedArchitectures lists the architectures the base picker may offer.
// Quick: it reads the cached CLI probe.
func (a *App) SupportedArchitectures() ArchesResult {
	res := ArchesResult{Arches: []string{"amd64", "arm64"}, Default: "amd64"}
	p, uerr := a.cliProbe()
	if uerr != nil {
		res.Error = uerr
		return res
	}
	if p.HostArch != "" {
		res.Default = p.HostArch
		found := false
		for _, x := range res.Arches {
			if x == p.HostArch {
				found = true
			}
		}
		if !found {
			res.Arches = append(res.Arches, p.HostArch)
			sort.Strings(res.Arches)
		}
	}
	return res
}

// ListBases returns the stock bases compiled into the debark binary, for one
// architecture. An empty arch means "whatever debark defaults to"; read the
// returned Arch rather than assuming. Quick: one `snapshot list-bases --json`.
//
// Every row is an assumption about a default install, not a measurement of a
// machine. BasesResult.Caveat carries the sentence the screen must show.
func (a *App) ListBases(arch string) BasesResult {
	res := BasesResult{Arch: arch, Bases: []BaseView{}, Caveat: BaseCaveat}
	if a.cli == nil {
		res.Error = errNoDep("the debark command-line tool")
		return res
	}
	ctx, cancel := context.WithTimeout(a.base(), 30*time.Second)
	defer cancel()

	list, err := a.cli.ListBases(ctx, arch)
	if err != nil {
		res.Error = uiErrorFrom(err)
		return res
	}
	if list == nil {
		res.Error = &UIError{
			Code:    ErrCodeCLI,
			Message: "debark returned no base listing at all.",
			Hint:    "Check that the installed debark supports `snapshot list-bases --json`.",
		}
		return res
	}
	res.Arch = list.Arch
	for _, e := range list.Bases {
		res.Bases = append(res.Bases, baseView(e))
	}
	return res
}

// ChooseSnapshotFile opens the native file picker for a snapshot. Blocks until
// the operator dismisses the dialog, which is what a modal does.
//
// Cancelling is not an error: check Cancelled before Error. Nothing is uploaded
// anywhere — the path is read locally by the debark binary and by nothing
// else.
func (a *App) ChooseSnapshotFile() FilePickResult {
	return a.openFile("Choose a snapshot", []wruntime.FileFilter{
		{DisplayName: "Snapshots (*.tar.zst, *.tar.gz, *.tar)", Pattern: "*.tar.zst;*.tar.gz;*.tar"},
		{DisplayName: "All files", Pattern: "*.*"},
	})
}

// InspectSnapshot reads a snapshot the operator chose from disk, so the screen
// can show what machine it describes before committing to it. Quick: one
// `snapshot inspect --json`. Nothing is uploaded anywhere.
func (a *App) InspectSnapshot(path string) SnapshotResult {
	var res SnapshotResult
	if path == "" {
		res.Error = errInvalid("No snapshot file was given.", "Choose a snapshot file first.")
		return res
	}
	if a.cli == nil {
		res.Error = errNoDep("the debark command-line tool")
		return res
	}
	ctx, cancel := context.WithTimeout(a.base(), 60*time.Second)
	defer cancel()

	info, err := a.cli.InspectSnapshot(ctx, path)
	if err != nil {
		res.Error = uiErrorFrom(err)
		return res
	}
	res.Snapshot = snapshotView(info.Path, info.Snapshot)
	return res
}

// SelectTarget commits to a target and makes it the one every other screen
// works against. Quick.
//
// The selection tray survives a target change on purpose: an operator
// comparing two bases should not lose their list. Entries are re-marked against
// the new catalogue the next time the tray is read, so a package the new target
// does not carry shows as unknown rather than silently disappearing.
//
// Events: target:changed.
func (a *App) SelectTarget(sel TargetSelection) TargetResult {
	var res TargetResult
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return TargetResult{Error: err}
	}
	if a.targetPending {
		a.mu.Unlock()
		return TargetResult{Error: errBusy("A target is already being prepared.")}
	}
	a.targetPending = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.targetPending = false; a.targetCancel = nil; a.mu.Unlock() }()
	switch sel.Kind {
	case TargetKindBase:
		if sel.BaseID == "" {
			res.Error = errInvalid("No base was chosen.", "Pick a distribution, release and variant.")
			return res
		}
	case TargetKindSnapshot:
		if sel.SnapshotPath == "" {
			res.Error = errInvalid("No snapshot file was chosen.", "Choose a snapshot file from disk.")
			return res
		}
	default:
		res.Error = errInvalid("That is not a kind of target this application knows.",
			`Choose either a stock base ("base") or a snapshot file ("snapshot").`)
		return res
	}

	ctx, cancel := context.WithTimeout(a.base(), 60*time.Second)
	defer cancel()

	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return TargetResult{Error: err}
	}
	a.targetCancel = cancel
	a.mu.Unlock()
	ct, problems, err := a.resolver.Resolve(ctx, sel)
	if err != nil {
		res.Error = uiErrorFrom(err)
		if res.Error.Code == ErrCodeInternal {
			res.Error.Code = ErrCodeTargetInvalid
		}
		// The problems explain the failure when resolution failed *because*
		// nothing usable was left, so they are worth more here than anywhere.
		res.SourceProblems = sourceProblemViews(problems)
		return res
	}

	view := targetView(sel, ct)
	// Catalogue identity omits installed-package state. A refreshed snapshot
	// at the same path must still invalidate the completed draft checkpoint.
	fingerprint := ""
	if sel.Kind == TargetKindSnapshot {
		file, err := os.Open(sel.SnapshotPath)
		if err != nil {
			return TargetResult{Error: uiErrorFrom(err)}
		}
		hash := sha256.New()
		_, hashErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(hashErr, closeErr, ctx.Err()); err != nil {
			return TargetResult{Error: uiErrorFrom(err)}
		}
		fingerprint = fmt.Sprintf("%x", hash.Sum(nil))
	}
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return TargetResult{Error: err}
	}
	if a.target.Selected && a.targetFingerprint == fingerprint && reflect.DeepEqual(a.catTarget, ct) {
		view = a.target
		a.mu.Unlock()
		return TargetResult{Target: view, SourceProblems: sourceProblemViews(problems)}
	}
	// Wait for a previous target's catalogue to relinquish its cache before
	// Ready can switch the catalogue underneath it. The UI remains responsive.
	a.catalogJob.stop()
	done := a.catalogJob.done
	a.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return TargetResult{Error: errBusy("The previous package preparation is still stopping. Try again shortly.")}
		}
	}
	a.mu.Lock()
	// Resolution and cancellation above are asynchronous boundaries: a build
	// may have started meanwhile. A stale target request cannot commit then.
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return TargetResult{Error: err}
	}
	// Carried even on success, and this is the point of the whole mechanism: a
	// source that could not be parsed is a whole archive missing from the
	// catalogue, and the picker just has fewer rows with nothing on screen to
	// say why. Dropping these here would make the one failure an operator
	// cannot detect for themselves also the one nobody is told about.
	res.SourceProblems = sourceProblemViews(problems)
	ready, rerr := false, error(nil)
	count, stale, builtAt := 0, false, ""
	if a.cat != nil {
		ready, rerr = a.cat.Ready(ct)
		if ready {
			// Ready has just read the cache marker to answer at all, so the
			// size, age and freshness of the catalogue it found are already
			// in hand. This is the warm path, and the only one the second run
			// takes: leaving these zero here is what made Categories().Total
			// 0 on every run after the first, and what made the picker's "out
			// of date / Refresh" banner unreachable for the only kind of
			// catalogue that can be out of date.
			count, stale, builtAt = catalogFacts(a.cat)
		}
	}
	view.CatalogReady = ready

	a.targetGeneration++
	a.targetFingerprint = fingerprint
	view.Generation = a.targetGeneration
	a.undo = selectionUndo{}
	a.target = view
	a.catTarget = ct
	a.catalogStatus = CatalogStatus{
		Generation:   a.targetGeneration,
		State:        catalogStateFor(ready),
		TargetID:     view.ID,
		Ready:        ready,
		FromCache:    ready,
		Stale:        stale,
		PackageCount: count,
		BuiltAt:      builtAt,
		Progress:     CatalogProgress{Generation: a.targetGeneration, Fraction: -1, OverallFraction: -1},
	}
	a.revalidateSelectionLocked(ctx)
	selection := a.summaryLocked()
	a.mu.Unlock()

	if rerr != nil {
		res.Error = uiErrorFromCatalog(rerr)
	}
	a.emitTargetChanged(view)
	a.emitSelectionChanged(selection)
	res.Target = view
	return res
}

// CurrentTarget returns the selected target, or a TargetView with Selected
// false when there is none. Fast.
func (a *App) CurrentTarget() TargetResult {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return TargetResult{Target: a.target}
}

// ClearTarget forgets the selected target and the catalogue that went with it.
// The selection tray is left alone. Fast.
//
// Events: target:changed, with Selected false.
func (a *App) ClearTarget() Result {
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return failed(err)
	}
	if a.targetPending || a.catalogJob.running {
		a.mu.Unlock()
		return failed(errBusy("Wait for package preparation to stop before clearing the target."))
	}
	a.catalogJob.stop()
	a.targetGeneration++
	a.undo = selectionUndo{}
	a.target = TargetView{Generation: a.targetGeneration}
	a.catTarget = catalog.Target{}
	a.catalogStatus = CatalogStatus{Generation: a.targetGeneration, State: CatalogStateNone, Progress: CatalogProgress{Generation: a.targetGeneration, Fraction: -1, OverallFraction: -1}}
	a.revalidateSelectionLocked(context.Background())
	view, selection := a.target, a.summaryLocked()
	a.mu.Unlock()
	a.emitTargetChanged(view)
	a.emitSelectionChanged(selection)
	return ok()
}

// ---------------------------------------------------------------------------
// Screen 3 — the catalogue behind the package picker.
// ---------------------------------------------------------------------------

// CatalogStatus returns the catalogue's whole state. Fast, and safe to call at
// any time: it is how a screen that mounted after the events recovers.
func (a *App) CatalogStatus() CatalogStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.catalogStatus
}

// StartCatalogBuild fetches and parses the target's indexes. Fire-and-forget.
//
// force rebuilds even when a usable cache exists — the "refresh" the UI offers
// when a catalogue reports itself stale. Without it, a ready catalogue returns
// immediately and emits a finished event, so the caller's state machine is the
// same either way.
//
// Events: catalog:started, catalog:progress (many), catalog:finished (exactly
// one, including on cancellation and failure).
func (a *App) StartCatalogBuild(force bool) Result {
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return failed(err)
	}
	if a.targetPending {
		a.mu.Unlock()
		return failed(errBusy("Wait for the target to finish loading."))
	}
	if a.catalogJob.running {
		a.mu.Unlock()
		return failed(errBusy("The catalogue is already being built."))
	}
	if !a.target.Selected {
		a.mu.Unlock()
		return failed(errNoTarget())
	}
	if a.cat == nil {
		a.mu.Unlock()
		return failed(errNoDep("the package catalogue"))
	}
	if a.catalogStatus.Ready && !force {
		status := a.catalogStatus
		a.mu.Unlock()
		a.emitCatalogFinished(CatalogFinished{OK: true, Status: status})
		return ok()
	}
	ctx, gen := a.catalogJob.start(a.baseCtx)
	a.catalogStatus = CatalogStatus{
		Generation: a.targetGeneration,
		State:      CatalogStateBuilding,
		TargetID:   a.target.ID,
		Building:   true,
		Progress:   CatalogProgress{Generation: a.targetGeneration, Phase: CatalogPhaseResolve, Label: "Starting", Fraction: -1, OverallFraction: -1},
	}
	status, cat, ct := a.catalogStatus, a.cat, a.catTarget
	a.mu.Unlock()

	a.emitCatalogStarted(status)
	started := time.Now()

	go func() {
		err := cat.Build(ctx, ct, func(p catalog.Progress) {
			view := catalogProgressView(p)
			view.Generation = status.Generation
			a.mu.Lock()
			current := a.catalogJob.gen == gen && a.catalogJob.running && a.targetGeneration == status.Generation
			if current {
				a.catalogStatus.Progress = view
			}
			a.mu.Unlock()
			if current {
				a.emitCatalogProgress(view)
			}
		})

		cancelled := errors.Is(err, context.Canceled) || ctx.Err() != nil
		var uerr *UIError
		if err != nil && !cancelled {
			uerr = uiErrorFromCatalog(err)
		}

		// Read outside the lock: the catalogue has its own, and this is the
		// build path's half of the same defect the warm path above carries.
		// BuiltAt is the marker's own timestamp rather than time.Now(),
		// because the two must agree — a screen that showed "built just now"
		// here and the marker's minute-old stamp on the next run would be
		// reporting a rebuild that did not happen.
		count, builtAt := 0, ""
		if err == nil && !cancelled {
			count, _, builtAt = catalogFacts(cat)
			if builtAt == "" {
				builtAt = time.Now().UTC().Format(time.RFC3339)
			}
		}

		a.mu.Lock()
		current := a.catalogJob.finish(gen)
		if current {
			a.catalogStatus.Building = false
			a.catalogStatus.Cancelled = cancelled
			a.catalogStatus.Error = uerr
			switch {
			case cancelled:
				a.catalogStatus.State = CatalogStateCancelled
			case err != nil:
				a.catalogStatus.State = CatalogStateFailed
			default:
				a.catalogStatus.State = CatalogStateReady
				a.catalogStatus.Ready = true
				a.catalogStatus.FromCache = false
				a.catalogStatus.PackageCount = count
				a.catalogStatus.BuiltAt = builtAt
				a.catalogStatus.Stale = false
			}
			a.target.CatalogReady = a.catalogStatus.Ready
			a.revalidateSelectionLocked(ctx)
		}
		status := a.catalogStatus
		selection := a.summaryLocked()
		a.mu.Unlock()
		if !current {
			return
		}
		a.emitCatalogFinished(CatalogFinished{
			OK:         err == nil && !cancelled,
			Cancelled:  cancelled,
			Status:     status,
			DurationMS: time.Since(started).Milliseconds(),
			Error:      uerr,
		})
		a.emitSelectionChanged(selection)
	}()
	return ok()
}

// CancelCatalogBuild stops a catalogue build. Safe to call when nothing is
// running. A cancelled build always finishes with catalog:finished carrying
// Cancelled true, and leaves no half-written cache behind.
func (a *App) CancelCatalogBuild() Result {
	a.mu.Lock()
	a.catalogJob.stop()
	a.mu.Unlock()
	return ok()
}

// SearchPackages returns one page of results. Quick, and the one method with a
// hard latency budget: under 100 ms at the 95th percentile, including this
// bridge round-trip.
//
// The frontend never receives the whole package set. It asks for a window and
// gets at most SearchPageMax rows plus Total, which is what lets a virtualiser
// scroll 60,000 results without any of them having been sent.
func (a *App) SearchPackages(q SearchQuery) SearchResult {
	res := SearchResult{Rows: []PackageRow{}, Query: q, Offset: q.Offset, Limit: q.Limit}
	if q.Limit > SearchPageMax {
		res.Truncated = true
	}

	a.mu.RLock()
	cat, ready := a.cat, a.catalogStatus.Ready
	a.mu.RUnlock()
	if cat == nil {
		res.Error = errNoDep("the package catalogue")
		return res
	}
	if !ready {
		res.Error = errCatalogNotReady()
		return res
	}

	ctx, cancel := context.WithTimeout(a.base(), 10*time.Second)
	defer cancel()

	page, err := cat.Search(ctx, catalog.Query{
		Text:     q.Text,
		Category: q.Category,
		AppsOnly: q.AppsOnly,
		Offset:   q.Offset,
		Limit:    q.Limit,
	})
	if err != nil {
		res.Error = uiErrorFromCatalog(err)
		return res
	}

	a.mu.RLock()
	rows := make([]PackageRow, 0, len(page.Entries))
	for i := range page.Entries {
		rows = append(rows, a.rowLocked(page.Entries[i]))
	}
	a.mu.RUnlock()

	res.Rows = rows
	res.Total = page.Total
	res.Offset = page.Offset
	res.Limit = page.Query.Limit
	res.Query = SearchQuery{
		Text:     page.Query.Text,
		Category: page.Query.Category,
		AppsOnly: page.Query.AppsOnly,
		Offset:   page.Query.Offset,
		Limit:    page.Query.Limit,
	}
	res.TookMS = page.Elapsed.Milliseconds()
	return res
}

// Categories returns the two-tier grouping the sidebar renders: DEP-11
// freedesktop categories for applications, apt Sections for everything else.
// Quick, and the one listing on this surface that is not paged — there are tens
// of these, not tens of thousands.
func (a *App) Categories() CategoriesResult {
	res := CategoriesResult{Categories: []CategoryView{}}

	a.mu.RLock()
	cat, ready, total := a.cat, a.catalogStatus.Ready, a.catalogStatus.PackageCount
	a.mu.RUnlock()
	if cat == nil {
		res.Error = errNoDep("the package catalogue")
		return res
	}
	if !ready {
		res.Error = errCatalogNotReady()
		return res
	}

	ctx, cancel := context.WithTimeout(a.base(), 10*time.Second)
	defer cancel()

	cats, err := cat.Categories(ctx)
	if err != nil {
		res.Error = uiErrorFromCatalog(err)
		return res
	}
	for _, c := range cats {
		res.Categories = append(res.Categories, CategoryView{
			ID:    c.ID,
			Tier:  CategoryTier(c.Tier),
			Name:  c.Name,
			Count: c.Count,
		})
	}
	res.Total = total
	return res
}

// GetPackage returns one package's detail panel. Quick. Found false with no
// error is the ordinary answer for a name the target's indexes do not carry.
func (a *App) GetPackage(name string) PackageResult {
	var res PackageResult
	if strings.TrimSpace(name) == "" {
		res.Error = errInvalid("No package name was given.", "Click a row, or type a name.")
		return res
	}

	a.mu.RLock()
	cat, ready := a.cat, a.catalogStatus.Ready
	a.mu.RUnlock()
	if cat == nil {
		res.Error = errNoDep("the package catalogue")
		return res
	}
	if !ready {
		res.Error = errCatalogNotReady()
		return res
	}

	ctx, cancel := context.WithTimeout(a.base(), 10*time.Second)
	defer cancel()

	entry, found, err := cat.Get(ctx, name)
	if err != nil {
		res.Error = uiErrorFromCatalog(err)
		return res
	}
	if !found {
		return res
	}

	a.mu.RLock()
	row := a.rowLocked(entry)
	a.mu.RUnlock()

	res.Found = true
	res.Package = PackageDetail{
		Name:                 row.Name,
		DisplayName:          row.DisplayName,
		Version:              row.Version,
		Suite:                row.Suite,
		Component:            row.Component,
		Arch:                 row.Arch,
		Section:              row.Section,
		Priority:             row.Priority,
		Categories:           row.Categories,
		Summary:              row.Summary,
		Description:          entry.Description,
		DescriptionTruncated: entry.DescriptionTruncated,
		InstalledSizeBytes:   row.InstalledSizeBytes,
		DownloadSizeBytes:    row.DownloadSizeBytes,
		IsApp:                row.IsApp,
		Selected:             row.Selected,
		Source:               row.Source,
		Homepage:             entry.Homepage,
		Warnings:             row.Warnings,
	}
	return res
}

// PackageRows hydrates up to PackageRowsMax names into rows, for a tray that
// holds names and needs to draw them. Quick. Names not in the catalogue come
// back in Missing rather than as an error: an operator may know something the
// index does not, and debark gives the authoritative answer at build time.
func (a *App) PackageRows(names []string) PackageRowsResult {
	res := PackageRowsResult{Rows: []PackageRow{}}
	if len(names) == 0 {
		return res
	}
	if len(names) > PackageRowsMax {
		res.Error = errTooMany(len(names), PackageRowsMax, "packages")
		return res
	}

	a.mu.RLock()
	cat, ready := a.cat, a.catalogStatus.Ready
	a.mu.RUnlock()
	if cat == nil {
		res.Error = errNoDep("the package catalogue")
		return res
	}
	if !ready {
		res.Error = errCatalogNotReady()
		return res
	}

	ctx, cancel := context.WithTimeout(a.base(), 15*time.Second)
	defer cancel()

	for _, n := range names {
		entry, found, err := cat.Get(ctx, n)
		if err != nil {
			res.Error = uiErrorFromCatalog(err)
			return res
		}
		if !found {
			res.Missing = append(res.Missing, n)
			continue
		}
		a.mu.RLock()
		res.Rows = append(res.Rows, a.rowLocked(entry))
		a.mu.RUnlock()
	}
	return res
}

// ---------------------------------------------------------------------------
// Screen 3 — the selection tray.
// ---------------------------------------------------------------------------

// Selection returns the tray's summary: counts, estimated sizes and warnings.
// It never carries the entries; SelectionPage does that. Fast.
func (a *App) Selection() SelectionSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.summaryLocked()
}

// SelectionKeys returns every key in the tray and nothing else. Fast.
//
// This is what the virtualised list marks its rows from. A recycled row has to
// know whether the package it is about to draw is selected, and the tray is the
// only authoritative answer: PackageRow.Selected is a snapshot taken when the
// page was produced and may be a frame stale. Before this method the only way
// to get the keys was to page the whole tray with SelectionPage, which costs
// ceil(total/500) round trips and carries versions, summaries, sizes and
// warnings the list never draws — on every `selection:changed`.
//
// For an apt row the key IS the package name, so marking a row is a set
// membership test against PackageRow.Name. URL and file rows key on the URL and
// the absolute path, which no catalogue row can collide with.
//
// It is deliberately unpaged: paging a membership test would reintroduce the
// round trips it exists to remove, and a key is a short string the operator put
// there themselves rather than a catalogue row. Beyond SelectionKeysMax the
// result is Truncated with no keys at all — see the field for why partial is
// worse than empty.
func (a *App) SelectionKeys() SelectionKeysResult {
	a.mu.RLock()
	defer a.mu.RUnlock()

	n := a.sel.len()
	res := SelectionKeysResult{Keys: []string{}, Total: n, Revision: a.sel.rev}
	if n > SelectionKeysMax {
		res.Truncated = true
		return res
	}
	res.Keys = a.sel.keys()
	for i, k := range res.Keys {
		res.Keys[i] = selectionKey(a.sel.byKey[k])
	}
	return res
}

// SelectionPage returns one page of tray entries. Fast. limit 0 means
// SelectionPageMax; anything larger is clamped.
func (a *App) SelectionPage(offset, limit int) SelectionPage {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > SelectionPageMax {
		limit = SelectionPageMax
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	all := a.sel.entries()
	page := SelectionPage{Entries: []SelectionEntry{}, Total: len(all), Offset: offset, Limit: limit}
	if offset >= len(all) {
		return page
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	page.Entries = append(page.Entries, all[offset:end]...)
	for i := range page.Entries {
		page.Entries[i] = selectionEntryView(page.Entries[i])
	}
	return page
}

// AddPackages adds apt package names to the tray, up to AddPackagesMax at a
// time. Quick: each name is looked up so the row can be drawn and warned about
// straight away.
//
// Events: selection:changed.
func (a *App) AddPackages(names []string) SelectionSummary {
	if len(names) > AddPackagesMax {
		s := a.Selection()
		s.Error = errTooMany(len(names), AddPackagesMax, "packages")
		return s
	}
	return a.addEntries(names, SourceAPT, nil)
}

// AddURLs adds vendor `.deb` download URLs to the tray. Quick, and it makes no
// network request: the URL is checked for shape only, and is fetched by
// debark at build time and by nothing else.
//
// Events: selection:changed.
func (a *App) AddURLs(urls []URLInput) SelectionSummary {
	if len(urls) > AddPackagesMax {
		s := a.Selection()
		s.Error = errTooMany(len(urls), AddPackagesMax, "URLs")
		return s
	}
	values := make([]string, 0, len(urls))
	digests := make(map[string]string, len(urls))
	var rejected []RejectedInput
	for _, in := range urls {
		u := strings.TrimSpace(in.URL)
		if u == "" {
			continue
		}
		values = append(values, u)
		d, ok := normaliseSHA256(in.SHA256)
		switch {
		case ok:
			// Keyed by URL because that is the entry key for a URL row.
			digests[u] = d
		case strings.TrimSpace(in.SHA256) != "":
			// Refused rather than silently ignored. A digest that does not
			// reach the build while the operator believes it did is the one
			// outcome worse than having no digest at all.
			rejected = append(rejected, RejectedInput{
				Value:  in.SHA256,
				Reason: "That is not a SHA-256 digest.",
				Hint:   "A SHA-256 is 64 hexadecimal characters. A leading \"sha256:\" is fine. The URL was not added.",
			})
			values = values[:len(values)-1]
		}
	}
	s := a.addEntries(values, SourceURL, digests)
	s.Rejected = append(s.Rejected, rejected...)
	return s
}

// normaliseSHA256 accepts what an operator is likely to paste — a bare digest,
// a "sha256:" prefix, surrounding whitespace, upper case — and returns the
// lowercase hex form debark's --digest flag expects.
//
// It returns false for anything else rather than passing it through: a
// malformed digest that reaches the CLI fails the build with a message about
// flag syntax, which tells the operator nothing about the mistake they made.
func normaliseSHA256(v string) (string, bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", false
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "sha256:"), "SHA256:")
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return "", false
	}
	out := make([]byte, 64)
	for i := 0; i < 64; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			out[i] = c
		case c >= 'A' && c <= 'F':
			out[i] = c + ('a' - 'A')
		default:
			return "", false
		}
	}
	return string(out), true
}

// AddLocalDebs adds local `.deb` files to the tray. Quick.
//
// Events: selection:changed.
func (a *App) AddLocalDebs(paths []string) SelectionSummary {
	if len(paths) > AddPackagesMax {
		s := a.Selection()
		s.Error = errTooMany(len(paths), AddPackagesMax, "files")
		return s
	}
	return a.addEntries(paths, SourceFile, nil)
}

// ChooseLocalDebs opens the native multi-select file picker for `.deb` files
// and adds nothing: pass the result to AddLocalDebs. Blocks until the dialog is
// dismissed.
func (a *App) ChooseLocalDebs() FilePickResult {
	a.mu.RLock()
	ctx, ready := a.ctx, a.runtimeReady
	a.mu.RUnlock()
	if !ready {
		return FilePickResult{Paths: []string{}, Error: errNoRuntime("open a file picker")}
	}
	paths, err := wruntime.OpenMultipleFilesDialog(ctx, wruntime.OpenDialogOptions{
		Title: "Choose vendor .deb files",
		Filters: []wruntime.FileFilter{
			{DisplayName: "Debian packages (*.deb)", Pattern: "*.deb"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
	if err != nil {
		return FilePickResult{Paths: []string{}, Error: dialogError(err)}
	}
	if len(paths) == 0 {
		return FilePickResult{Paths: []string{}, Cancelled: true}
	}
	return FilePickResult{Path: paths[0], Paths: paths}
}

// ChooseSigningKey returns only a file reference. Signing and validation
// remain the CLI's responsibility; no private bytes cross the bridge.
func (a *App) ChooseSigningKey() FilePickResult {
	return a.openFile("Choose signing key", []wruntime.FileFilter{{DisplayName: "All files", Pattern: "*"}})
}

// RemovePackages removes tray entries by key. The key is SelectionEntry.Key:
// the package name for apt rows, the URL for url rows, the absolute path for
// file rows. Fast.
//
// Events: selection:changed.
func (a *App) RemovePackages(keys []string) SelectionSummary {
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		s := a.summaryLocked()
		s.Error = err
		a.mu.Unlock()
		return s
	}
	before := a.sel.entries()
	removed := 0
	for _, k := range keys {
		for _, e := range before {
			if selectionKey(e) == k {
				k = e.Key
				break
			}
		}
		if a.sel.remove(k) {
			removed++
		}
	}
	if removed > 0 {
		a.selectionRevision++
		a.saveUndoLocked(before, "Undo removal")
	}
	s := a.summaryLocked()
	a.mu.Unlock()

	s.Removed = removed
	if removed > 0 {
		a.emitSelectionChanged(s)
	}
	return s
}

// ClearSelection empties the tray. Fast.
//
// Events: selection:changed.
func (a *App) ClearSelection() SelectionSummary {
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		s := a.summaryLocked()
		s.Error = err
		a.mu.Unlock()
		return s
	}
	removed := a.sel.len()
	if removed > 0 {
		before := a.sel.entries()
		a.sel.init()
		a.selectionRevision++
		a.saveUndoLocked(before, "Undo clear")
	}
	s := a.summaryLocked()
	a.mu.Unlock()

	s.Removed = removed
	a.emitSelectionChanged(s)
	return s
}

// UndoSelection restores only the most recent eligible edit, atomically. The
// saved entries are original Go values, including credentialed URLs/digests.
func (a *App) UndoSelection(token string) SelectionSummary {
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		s := a.summaryLocked()
		s.Error = err
		a.mu.Unlock()
		return s
	}
	if token == "" || token != a.undo.token || a.undo.revision != a.selectionRevision || a.undo.target != a.targetGeneration {
		s := a.summaryLocked()
		s.Error = errInvalid("That selection change can no longer be undone.", "Only the most recent removal or clear can be restored. Add the packages again.")
		a.mu.Unlock()
		return s
	}
	before := a.sel.len()
	entries := a.undo.entries
	a.sel.init()
	for _, e := range entries {
		a.sel.add(e)
	}
	a.selectionRevision++
	a.undo = selectionUndo{}
	s := a.summaryLocked()
	s.Added = a.sel.len() - before
	a.mu.Unlock()
	a.emitSelectionChanged(s)
	return s
}

type selectionUndo struct {
	token, label     string
	revision, target uint64
	entries          []SelectionEntry
}

func (a *App) saveUndoLocked(entries []SelectionEntry, label string) {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	a.undo = selectionUndo{token: fmt.Sprintf("%x", nonce), label: label, revision: a.selectionRevision, target: a.targetGeneration, entries: entries}
}

func selectionKey(e SelectionEntry) string {
	if e.Source == SourceURL && selectionURLView(e.URL) != e.URL {
		return fmt.Sprintf("url:%x", sha256.Sum256([]byte(e.Key)))
	}
	return e.Key
}

func selectionURLView(url string) string {
	return strings.TrimPrefix(cliadapter.RedactArgv([]string{"url:" + url})[0], "url:")
}

func selectionEntryView(e SelectionEntry) SelectionEntry {
	if e.Source == SourceURL {
		e.Key = selectionKey(e)
		e.URL = selectionURLView(e.URL)
	}
	e.Warnings = append([]SelectionWarning(nil), e.Warnings...)
	return e
}

// ParsePackageList reports what a pasted list WOULD do, without doing it, so
// the operator can look before they commit. Quick.
//
// It returns counts and a bounded sample, never the whole parsed list: a paste
// can be tens of thousands of lines and the frontend already has the text.
// AddPackageList commits the same text.
func (a *App) ParsePackageList(text string) ParsedList {
	res := ParsedList{Unknown: []string{}, Sample: []PackageRow{}, Rejected: []RejectedInput{}}
	if len(text) > PackageListMaxBytes {
		res.Error = &UIError{
			Code:    ErrCodeTooManyItems,
			Message: fmt.Sprintf("That list is %d bytes, and the limit is %d.", len(text), PackageListMaxBytes),
			Hint:    "Split it, or point the build at the file with a packages.txt input instead.",
		}
		return res
	}

	names, rejected := parseNameList(text)
	res.Rejected = rejected
	res.Count = len(names)

	a.mu.RLock()
	cat, ready := a.cat, a.catalogStatus.Ready
	for _, n := range names {
		if !a.sel.has(n) {
			res.NewCount++
		}
	}
	a.mu.RUnlock()

	if cat == nil || !ready {
		// Without a catalogue nothing can be classified, which is not an
		// error: the operator may paste a list before building the index.
		res.UnknownCount = len(names)
		return res
	}

	ctx, cancel := context.WithTimeout(a.base(), 20*time.Second)
	defer cancel()

	for _, n := range names {
		entry, found, err := cat.Get(ctx, n)
		if err != nil {
			res.Error = uiErrorFromCatalog(err)
			return res
		}
		if !found {
			res.UnknownCount++
			if len(res.Unknown) < 100 {
				res.Unknown = append(res.Unknown, n)
			}
			continue
		}
		res.KnownCount++
		if len(res.Sample) < 50 {
			a.mu.RLock()
			res.Sample = append(res.Sample, a.rowLocked(entry))
			a.mu.RUnlock()
		}
	}
	return res
}

// AddPackageList commits a pasted list to the tray. Quick.
//
// Events: selection:changed.
func (a *App) AddPackageList(text string) SelectionSummary {
	if len(text) > PackageListMaxBytes {
		s := a.Selection()
		s.Error = &UIError{
			Code:    ErrCodeTooManyItems,
			Message: fmt.Sprintf("That list is %d bytes, and the limit is %d.", len(text), PackageListMaxBytes),
			Hint:    "Split it, or point the build at the file with a packages.txt input instead.",
		}
		return s
	}
	names, rejected := parseNameList(text)
	if len(names) > AddPackagesMax {
		s := a.Selection()
		s.Error = errTooMany(len(names), AddPackagesMax, "packages")
		return s
	}
	s := a.addEntries(names, SourceAPT, nil)
	s.Rejected = append(s.Rejected, rejected...)
	return s
}

// ---------------------------------------------------------------------------
// Screen 4 — build and progress.
// ---------------------------------------------------------------------------

// PreviewCommand returns the command a build with these options would run,
// without running it, with vendor-URL credentials removed. Fast.
//
// Rule 8: the CLI stays the complete interface, and the GUI shows the command
// where it reasonably can. Every screen that can start a build should show
// this, and CopyToClipboard is beside it for a reason.
//
// # It is redacted, and the frontend must say so
//
// Argv and Display are cliadapter.RedactArgv's output, not the exact argv. The
// exact argv is still exactly what runs — StartBuild hands the adapter the same
// spec, and the adapter derives the real command line from it — but the string
// this method returns is built to be read and copied, and a string built to be
// copied must not carry a password. docs/security-review.md §6.1a settles why:
// the preview's two audiences want different things, a redacted command answers
// "what is about to run on my machine?" completely, and the application cannot
// tell whether the clipboard is going to a shell, a ticket or a chat message.
// There is deliberately no reveal control.
//
// The cost is real and the UI beside this must not paper over it: RedactURL
// drops the WHOLE query string, not just the credential, because a presigned
// URL's secret lives in a parameter whose name is not standardised. So a vendor
// URL carrying an ordinary, secret-free query string — a version pin, a mirror
// id — comes back as "…/agent.deb?REDACTED" too. For that input the preview is
// a faithful description of the build and NOT a paste-and-run script. Copy that
// says otherwise ("exactly what will run", "copy this line and you get the same
// bundle") is false the moment a URL like that is in the tray.
//
// # It validates
//
// Validate is called here for the same reason StartBuild calls it: a preview
// that renders a command the application would then refuse to run sends the
// operator to a terminal with a command line that was never runnable. The
// clearest case is a digest paired with a URL containing "=", which
// `build --digest` misbinds silently (§6.5a) and Validate refuses. On a refusal
// Argv and Display are empty and Error carries the sentence — CommandPreview
// has had the field for exactly this since the surface was frozen.
func (a *App) PreviewCommand(opts BuildOptions) CommandPreview {
	var res CommandPreview
	spec, uerr := a.buildSpec(opts)
	if uerr != nil {
		res.Error = uerr
		return res
	}
	if a.cli == nil {
		res.Error = errNoDep("the debark command-line tool")
		return res
	}
	// Same order as StartBuild, so the two cannot disagree about which of a
	// spec's problems is reported first.
	if err := spec.Validate(); err != nil {
		res.Error = uiErrorFrom(err)
		return res
	}
	shown := cliadapter.RedactArgv(a.cli.Command(spec))
	res.Argv = shown
	res.Display = shellDisplay(shown)
	return res
}

// StartBuild builds the bundle. Fire-and-forget.
//
// The target and the tray are not arguments: they are application state, so the
// tray stays the single source of truth for what gets built and cannot
// disagree with what the operator is looking at.
//
// Events: build:started, then build:progress and build:event as the NDJSON
// stream arrives, then build:finished exactly once — including on cancellation
// and failure.
//
// An "incomplete" outcome is a success that produced a short bundle: the
// summary's ExitClass says so and the UI must not present it as a clean build.
//
// The Command carried by build:started and by BuildStatus is the REDACTED
// command line, for the same reason PreviewCommand's is — those are the two
// strings the build screen renders and its details drawer copies. The build
// itself is unaffected: Build is handed the spec, and the adapter builds the
// exact argv from it. See PreviewCommand for what redaction costs a reader.
func (a *App) StartBuild(opts BuildOptions) Result {
	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return failed(err)
	}
	if a.catalogJob.running {
		a.mu.Unlock()
		return failed(errBusy("Wait for package preparation to finish before building."))
	}
	spec, uerr := a.buildSpecLocked(opts)
	if uerr != nil {
		a.mu.Unlock()
		return failed(uerr)
	}
	if a.cli == nil {
		a.mu.Unlock()
		return failed(errNoDep("the debark command-line tool"))
	}
	if err := spec.Validate(); err != nil {
		a.mu.Unlock()
		return failed(uiErrorFrom(err))
	}

	// shown is for the screen and the details drawer, and is redacted. It is
	// deliberately the ONLY command line this function holds: the build runs
	// from spec, and the adapter derives the exact argv from it again inside
	// Build, so there is no unredacted argv in scope here that a later edit
	// could put on the screen by mistake. See PreviewCommand's comment for the
	// decision, and docs/security-review.md §6.1a for the argument.
	shown := cliadapter.RedactArgv(a.cli.Command(spec))
	ctx, gen := a.buildJob.start(a.baseCtx)
	a.buildLog.reset()
	a.buildState = BuildStatus{
		TargetGeneration:  a.targetGeneration,
		SelectionRevision: a.selectionRevision,
		Running:           true,
		Progress:          BuildProgress{Phase: BuildPhaseStarting, Fraction: -1, Message: "Starting debark"},
		Command:           shown,
		TargetID:          a.target.ID,
		ItemCount:         a.sel.len(),
		StartedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	started := BuildStarted{
		TargetGeneration:  a.buildState.TargetGeneration,
		SelectionRevision: a.buildState.SelectionRevision,
		Command:           shown,
		TargetID:          a.buildState.TargetID,
		ItemCount:         a.buildState.ItemCount,
		StartedAt:         a.buildState.StartedAt,
		OutputDir:         opts.OutputDir,
	}
	cli := a.cli
	a.mu.Unlock()

	a.emitBuildStarted(started)
	a.emitLifecycle()
	begin := time.Now()

	go func() {
		result, err := cli.Build(ctx, spec, func(e cliadapter.Event) {
			a.recordBuildEvent(gen, e)
		})

		cancelled := ctx.Err() != nil || cliadapterCancelled(err)
		summary := buildSummary(result)
		var uerr *UIError
		// Result and error are not mutually exclusive: an incomplete build
		// (exit 3) returns a fully populated result alongside an error whose
		// class is "incomplete", and Unresolved and FetchFailed are the only
		// place that detail exists. So the summary is kept either way.
		if err != nil && !cancelled {
			uerr = uiErrorFrom(err)
		}

		a.mu.Lock()
		current := a.buildJob.finish(gen)
		if current {
			a.buildState.Running = false
			a.buildState.Finished = true
			a.buildState.Cancelled = cancelled
			a.buildState.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			a.buildState.Summary = summary
			a.buildState.Error = uerr
			a.buildState.EventCount = a.buildLog.total
			switch {
			case cancelled:
				a.buildState.Progress.Phase = BuildPhaseCancelled
			case err != nil:
				a.buildState.Progress.Phase = BuildPhaseFailed
			default:
				a.buildState.Progress.Phase = BuildPhaseFinished
				a.buildState.Progress.Fraction = 1
			}
			if summary != nil && summary.BundlePath != "" {
				a.buildBundle = summary.BundlePath
			}
			if err == nil && !cancelled && summary != nil && summary.BundlePath != "" && summary.ExitClass == "success" && len(summary.Unresolved) == 0 && len(summary.FetchFailed) == 0 {
				a.completeTarget, a.completeSelection = a.buildState.TargetGeneration, a.buildState.SelectionRevision
			}
		}
		a.mu.Unlock()
		if !current {
			return
		}
		a.emitBuildFinished(BuildFinished{
			OK:         err == nil && !cancelled,
			Cancelled:  cancelled,
			Summary:    summary,
			DurationMS: time.Since(begin).Milliseconds(),
			Error:      uerr,
		})
		a.emitLifecycle()
	}()
	return ok()
}

// CancelBuild stops a running build. Safe to call when nothing is running.
// A cancelled build always finishes with build:finished carrying Cancelled
// true; a few build:event messages already in flight may still arrive before
// it.
func (a *App) CancelBuild() Result {
	a.mu.Lock()
	a.buildJob.stop()
	a.mu.Unlock()
	return ok()
}

// BuildStatus returns the build's whole state. Fast, and how the build screen
// recovers after the operator navigates away and back.
func (a *App) BuildStatus() BuildStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := a.buildState
	s.EventCount = a.buildLog.total
	return s
}

// BuildLog returns raw debark events for the collapsed-by-default details
// drawer. Fast.
//
// Page with sinceSeq: pass 0 the first time and NextSeq afterwards. The backend
// keeps the last BuildLogRing events; Dropped says how many fell out, and the
// drawer must say so rather than implying the log is complete. The bundle's
// evidence.json is the auditable record, and is where an auditor looks.
//
// The two are NOT the same set, and an earlier version of this comment implied
// they were. Measured on a real `build --base` run: the --json-events stream
// carried fifteen events and the bundle's evidence.json recorded thirteen. The
// stream also carries the base-synthesis phase — a second backend.selected,
// apt.update and apt.resolve for the throwaway snapshot the base is turned
// into — and that phase is not part of what the bundle attests to. So the
// drawer can hold events the bundle does not, which is the right way round,
// but "the full record is in evidence.json" was wrong.
func (a *App) BuildLog(sinceSeq, limit int) BuildLogPage {
	if limit <= 0 || limit > BuildLogPageMax {
		limit = BuildLogPageMax
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.buildLog.page(sinceSeq, limit)
}

// ChooseDirectory opens the native directory picker. Blocks until the dialog is
// dismissed. It serves both the build screen (where does the bundle go) and the
// export screen (which folder on the drive).
func (a *App) ChooseDirectory(title, defaultDir string) FilePickResult {
	a.mu.RLock()
	ctx, ready := a.ctx, a.runtimeReady
	a.mu.RUnlock()
	if !ready {
		return FilePickResult{Paths: []string{}, Error: errNoRuntime("open a folder picker")}
	}
	if title == "" {
		title = "Choose a folder"
	}
	if defaultDir != "" {
		if st, err := os.Stat(defaultDir); err != nil || !st.IsDir() {
			defaultDir = ""
		}
	}
	path, err := wruntime.OpenDirectoryDialog(ctx, wruntime.OpenDialogOptions{
		Title:                title,
		DefaultDirectory:     defaultDir,
		CanCreateDirectories: true,
	})
	if err != nil {
		return FilePickResult{Paths: []string{}, Error: dialogError(err)}
	}
	if path == "" {
		return FilePickResult{Paths: []string{}, Cancelled: true}
	}
	return FilePickResult{Path: path, Paths: []string{path}}
}

// ---------------------------------------------------------------------------
// Screen 5 — export to a mounted volume.
// ---------------------------------------------------------------------------

// ListVolumes enumerates mounted volumes. Quick, and cheap enough to poll on a
// timer: compare Fingerprint and skip the redraw when it has not moved.
//
// Mounted volumes only. No block device is ever opened, nothing is partitioned
// or formatted, no bootable medium is written, and nothing needs root. On a
// platform with no enumeration, Supported is false and the UI offers a plain
// folder picker instead of showing an error.
func (a *App) ListVolumes() VolumesResult {
	res := VolumesResult{Volumes: []VolumeView{}, Supported: true}
	vols, err := export.ListVolumes()
	if err != nil {
		if errors.Is(err, export.ErrUnsupportedPlatform) {
			res.Supported = false
			return res
		}
		res.Error = uiErrorFromExport(err)
		return res
	}
	for _, v := range vols {
		res.Volumes = append(res.Volumes, volumeView(v))
	}
	res.Fingerprint = export.Fingerprint(vols)
	return res
}

// PlanExport reports exactly what a copy would do — how many files, how many
// bytes, whether it fits, and what is worth warning about — without writing
// anything. Quick for a normal bundle; it walks the source directory, so a very
// large bundle on slow media takes proportionally longer.
//
// Show it and let the operator confirm. A copy that can be predicted to fail
// should never be started.
func (a *App) PlanExport(opts ExportOptions) ExportPlanResult {
	var res ExportPlanResult
	req, uerr := a.exportRequest(opts)
	if uerr != nil {
		res.Error = uerr
		return res
	}
	ctx, cancel := context.WithTimeout(a.base(), 2*time.Minute)
	defer cancel()

	plan, err := export.NewExporter(export.Options{SkipVerify: opts.SkipVerify}).Plan(ctx, req)
	if err != nil {
		res.Error = uiErrorFromExport(err)
		return res
	}
	res.Plan = planView(plan)
	// The exporter warns about an incomplete SOURCE; this is the destination,
	// which it equally knows about and has never been asked. A drive already
	// carrying a half-finished bundle is the one thing the operator must be
	// told before they confirm, because the folder looks finished.
	if incomplete, ierr := export.IsIncomplete(req.DestDir); ierr == nil {
		res.Plan.DestinationIncomplete = incomplete
	}
	return res
}

// InspectDestination reports what is already sitting at a chosen destination.
// Fast: two stat calls, and it writes nothing.
//
// Call it the moment a destination is chosen, before PlanExport walks the
// bundle. The case it exists for is a drive that still carries the marker an
// interrupted export left behind: that folder looks exactly like a finished
// bundle and is not one, and it is the only thing on this screen an operator
// cannot see for themselves.
//
// It takes the same ExportOptions StartExport does, and resolves the folder the
// same way, so what it reports on is the folder a copy would really touch —
// including the default subdirectory, which is the bundle folder's own name.
func (a *App) InspectDestination(opts ExportOptions) DestinationView {
	res := DestinationView{Destination: opts.Destination, MarkerName: export.MarkerName}
	req, uerr := a.exportRequest(opts)
	if uerr != nil {
		res.Error = uerr
		return res
	}
	res.Path = req.DestDir

	if st, err := os.Stat(req.DestDir); err == nil && st.IsDir() {
		res.Exists = true
	}
	incomplete, err := export.IsIncomplete(req.DestDir)
	if err != nil {
		res.Error = uiErrorFromExport(err)
		return res
	}
	res.Incomplete = incomplete
	if incomplete {
		res.Message = IncompleteDestinationMessage
	}
	return res
}

// StartExport copies the bundle onto the chosen destination. Fire-and-forget.
//
// It is a plain filesystem copy: every file is written, fsynced, then re-read
// and SHA-256 compared, and the destination carries an "incomplete" marker for
// the whole time the copy is in flight. Only a passing verification removes it,
// so a cancelled, crashed or interrupted copy leaves the drive marked bad
// rather than marked good.
//
// Events: export:started, export:progress, export:finished exactly once.
func (a *App) StartExport(opts ExportOptions) Result {
	a.mu.RLock()
	guard := a.mutationErrorLocked()
	a.mu.RUnlock()
	if guard != nil {
		return failed(guard)
	}
	req, uerr := a.exportRequest(opts)
	if uerr != nil {
		return failed(uerr)
	}

	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		a.mu.Unlock()
		return failed(err)
	}
	ctx, gen := a.exportJob.start(a.baseCtx)
	a.exportStatus = ExportStatus{
		Running:       true,
		BundlePath:    req.SourceDir,
		Destination:   opts.Destination,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		VerifySkipped: opts.SkipVerify,
		Progress:      ExportProgress{Phase: ExportPhaseChecking, Fraction: -1, Message: "Checking the destination"},
	}
	status := a.exportStatus
	a.mu.Unlock()

	a.emitExportStarted(status)
	a.emitLifecycle()
	begin := time.Now()

	go func() {
		exporter := export.NewExporter(export.Options{
			SkipVerify: opts.SkipVerify,
			Progress: func(p export.Progress) {
				view := exportProgressView(p)
				a.mu.Lock()
				current := a.exportJob.gen == gen && a.exportJob.running
				if current {
					a.exportStatus.Progress = view
				}
				a.mu.Unlock()
				if current {
					a.emitExportProgress(view)
				}
			},
		})
		result, err := exporter.Export(ctx, req)

		cancelled := ctx.Err() != nil || export.IsCancelled(err)
		var uerr *UIError
		if err != nil && !cancelled {
			uerr = uiErrorFromExport(err)
		}

		a.mu.Lock()
		current := a.exportJob.finish(gen)
		if current {
			a.exportStatus.Running = false
			a.exportStatus.Finished = true
			a.exportStatus.Cancelled = cancelled
			a.exportStatus.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			a.exportStatus.Error = uerr
			a.exportStatus.DestinationPath = req.DestDir
			if result != nil {
				a.exportStatus.Verified = result.Complete && !opts.SkipVerify && !cancelled && err == nil
				a.exportStatus.Summary = result.Summary
				applyVerification(&a.exportStatus, result.Verification)
			}
			switch {
			case cancelled:
				a.exportStatus.Progress.Phase = ExportPhaseCancelled
			case err != nil:
				a.exportStatus.Progress.Phase = ExportPhaseFailed
			default:
				a.exportStatus.Progress.Phase = ExportPhaseFinished
				a.exportStatus.Progress.Fraction = 1
			}
		}
		status := a.exportStatus
		a.mu.Unlock()
		if !current {
			return
		}
		a.emitExportFinished(ExportFinished{
			OK:         err == nil && !cancelled,
			Cancelled:  cancelled,
			Status:     status,
			DurationMS: time.Since(begin).Milliseconds(),
			Error:      uerr,
		})
		a.emitLifecycle()
	}()
	return ok()
}

// CancelExport stops a running copy. Safe to call when nothing is running.
//
// A cancelled copy leaves the incomplete marker on the destination on purpose:
// the drive holds a partial bundle, and the marker is what stops anything from
// treating it as usable. export:finished arrives with Cancelled true.
func (a *App) CancelExport() Result {
	a.mu.Lock()
	a.exportJob.stop()
	a.mu.Unlock()
	return ok()
}

// ExportStatus returns the copy's whole state. Fast.
func (a *App) ExportStatus() ExportStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.exportStatus
}

// StartVerify checks a bundle with `debark verify --json`. Fire-and-forget.
//
// This is the standalone check — "is the bundle on this drive still good?" The
// copy runs its own verification pass as part of StartExport; this is the one
// an operator reaches for afterwards.
//
// Events: verify:started, verify:finished exactly once.
func (a *App) StartVerify(bundlePath string) Result {
	if bundlePath == "" {
		a.mu.RLock()
		bundlePath = a.buildBundle
		a.mu.RUnlock()
	}
	if bundlePath == "" {
		return failed(errInvalid("No bundle was given to check.", "Build a bundle, or choose a bundle folder."))
	}
	if a.cli == nil {
		return failed(errNoDep("the debark command-line tool"))
	}

	a.mu.Lock()
	if a.stopping || a.closePending {
		a.mu.Unlock()
		return failed(errBusy("Debark is closing."))
	}
	if a.verifyJob.running {
		a.mu.Unlock()
		return failed(errBusy("A verification is already running."))
	}
	ctx, gen := a.verifyJob.start(a.baseCtx)
	a.verifyStatus = VerifyStatus{
		Running:    true,
		BundlePath: bundlePath,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	status, cli := a.verifyStatus, a.cli
	a.mu.Unlock()

	keys := a.trustedKeys()
	a.emitVerifyStarted(status)
	begin := time.Now()

	go func() {
		report, err := verifyBundle(ctx, cli, bundlePath, keys)
		cancelled := ctx.Err() != nil || cliadapterCancelled(err)

		var uerr *UIError
		// As with Build, the report and the error are not exclusive: a bundle
		// that fails verification is reported through both, and the report's
		// Problems are the only place the detail exists.
		if err != nil && !cancelled && !report.OK {
			uerr = uiErrorFrom(err)
			uerr.Code = ErrCodeVerifyFailed
		} else if err != nil && !cancelled {
			uerr = uiErrorFrom(err)
		}

		view := verifyStatusView(bundlePath, report)
		a.mu.Lock()
		current := a.verifyJob.finish(gen)
		if current {
			view.Running = false
			view.Finished = true
			view.Cancelled = cancelled
			view.StartedAt = a.verifyStatus.StartedAt
			view.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			view.Error = uerr
			a.verifyStatus = view
		}
		a.mu.Unlock()
		if !current {
			return
		}
		a.emitVerifyFinished(VerifyFinished{
			OK:         view.OK && uerr == nil,
			Cancelled:  cancelled,
			Status:     view,
			DurationMS: time.Since(begin).Milliseconds(),
			Error:      uerr,
		})
	}()
	return ok()
}

// trustedKeys is the set of public keys the verification screen checks a
// bundle's signature against: the operator's own, when this machine has one.
//
// This is not the GUI deciding what is trustworthy — it is the GUI saying
// which key it signed with. `debark verify BUNDLE` with no --key and no
// configured verify_keys trusts nothing at all, so a bundle this application
// had built and signed one screen earlier failed its own verification with "no
// signature verifies against a trusted key". Found by hack/e2e, which is the
// first thing that ever built a real bundle and then checked it.
//
// It is the PUBLIC key, derived with the CLI adapter's own rule rather than a
// literal ".pub". When the file is not there — no key yet, or signing
// delegated to gpg or a plugin — nothing is passed and debark's own
// configuration decides, which is exactly the previous behaviour.
func (a *App) trustedKeys() []string {
	a.mu.RLock()
	priv := a.keyPath
	a.mu.RUnlock()
	if priv == "" {
		return nil
	}
	pub := cliadapter.PublicKeyPath(priv)
	if st, err := os.Stat(pub); err != nil || st.IsDir() {
		return nil
	}
	return []string{pub}
}

// verifyBundle runs the verification, using the keyed form when the adapter
// supports it. See cliadapter.KeyedVerifier for why that is an extension
// interface and not a third parameter on Adapter.Verify.
func verifyBundle(ctx context.Context, cli cliadapter.Adapter, bundlePath string, keys []string) (cliadapter.VerifyReport, error) {
	if kv, ok := cli.(cliadapter.KeyedVerifier); ok && len(keys) > 0 {
		return kv.VerifyWithKeys(ctx, bundlePath, keys)
	}
	return cli.Verify(ctx, bundlePath)
}

// CancelVerify stops a running verification. Safe to call when nothing is
// running.
func (a *App) CancelVerify() Result {
	a.mu.Lock()
	a.verifyJob.stop()
	a.mu.Unlock()
	return ok()
}

// VerifyStatus returns the verification's whole state. Fast.
func (a *App) VerifyStatus() VerifyStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.verifyStatus
}

// ---------------------------------------------------------------------------
// Internals below this line. Nothing here is part of the frontend contract.
// ---------------------------------------------------------------------------

func (a *App) base() context.Context {
	a.mu.RLock()
	ctx := a.baseCtx
	a.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// openFile is the shared body of the single-file dialogs.
func (a *App) openFile(title string, filters []wruntime.FileFilter) FilePickResult {
	a.mu.RLock()
	ctx, ready := a.ctx, a.runtimeReady
	a.mu.RUnlock()
	if !ready {
		return FilePickResult{Paths: []string{}, Error: errNoRuntime("open a file picker")}
	}
	path, err := wruntime.OpenFileDialog(ctx, wruntime.OpenDialogOptions{Title: title, Filters: filters})
	if err != nil {
		return FilePickResult{Paths: []string{}, Error: dialogError(err)}
	}
	if path == "" {
		return FilePickResult{Paths: []string{}, Cancelled: true}
	}
	return FilePickResult{Path: path, Paths: []string{path}}
}

// rowLocked projects a catalogue entry into a row. Caller holds mu at least for
// reading, because the row's Selected flag comes from the tray.
func (a *App) rowLocked(e catalog.Entry) PackageRow {
	row := PackageRow{
		Name:        e.Name,
		DisplayName: e.AppName,
		Version:     e.Version,
		Suite:       e.Suite,
		Component:   e.Component,
		Arch:        e.Arch,
		Section:     e.Section,
		Priority:    e.Priority,
		Categories:  e.Categories,
		Summary:     e.Summary,
		// The catalogue reports installed size in KiB, as apt does. The
		// conversion happens here, once, at the boundary.
		InstalledSizeBytes: e.InstalledSizeKiB * 1024,
		DownloadSizeBytes:  e.DownloadSizeBytes,
		IsApp:              e.IsApp,
		Selected:           a.sel.has(e.Name),
		Source:             SourceAPT,
	}
	if a.policy != nil {
		row.Warnings = a.policy.WarnFor(a.target, SelectionEntry{
			Key: e.Name, Name: e.Name, DisplayName: e.AppName,
			Version: e.Version, Summary: e.Summary, Source: SourceAPT,
			InstalledSizeBytes: row.InstalledSizeBytes,
			DownloadSizeBytes:  row.DownloadSizeBytes,
			Known:              true,
		})
	}
	return row
}

// addEntries is the shared body of the four add methods.
// addEntries is the shared add path. digests is keyed by entry key and is
// only ever non-nil for URL rows; an entry with no digest is normal and means
// the operator did not supply one, never that one was lost on the way here.
func (a *App) addEntries(values []string, src PackageSource, digests map[string]string) SelectionSummary {
	a.mu.RLock()
	guard, targetGeneration := a.mutationErrorLocked(), a.targetGeneration
	a.mu.RUnlock()
	if guard != nil {
		s := a.Selection()
		s.Error = guard
		return s
	}
	var rejected []RejectedInput
	type pending struct {
		key   string
		entry SelectionEntry
	}
	var items []pending

	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		e, rej := newSelectionEntry(v, src)
		if rej != nil {
			rejected = append(rejected, *rej)
			continue
		}
		if d := digests[e.Key]; d != "" {
			e.SHA256 = d
		}
		items = append(items, pending{key: e.Key, entry: e})
	}

	// Hydrate apt names against the catalogue, outside the lock.
	a.mu.RLock()
	cat, ready := a.cat, a.catalogStatus.Ready
	a.mu.RUnlock()
	if cat != nil && ready && src == SourceAPT {
		ctx, cancel := context.WithTimeout(a.base(), 20*time.Second)
		for i := range items {
			entry, found, err := cat.Get(ctx, items[i].entry.Name)
			if err != nil {
				break
			}
			if !found {
				continue
			}
			items[i].entry.Known = true
			items[i].entry.DisplayName = entry.AppName
			items[i].entry.Version = entry.Version
			items[i].entry.Summary = entry.Summary
			items[i].entry.InstalledSizeBytes = entry.InstalledSizeKiB * 1024
			items[i].entry.DownloadSizeBytes = entry.DownloadSizeBytes
		}
		cancel()
	}

	a.mu.Lock()
	if err := a.mutationErrorLocked(); err != nil {
		s := a.summaryLocked()
		s.Error = err
		a.mu.Unlock()
		return s
	}
	if targetGeneration != a.targetGeneration {
		s := a.summaryLocked()
		s.Error = errBusy("The target changed while adding packages. Try again for the current target.")
		a.mu.Unlock()
		return s
	}
	added := 0
	changed := false
	for i := range items {
		e := items[i].entry
		old, exists := a.sel.byKey[e.Key]
		if exists && e.Source == SourceURL && e.SHA256 == "" {
			e.SHA256 = old.SHA256
		}
		if !exists || old.SHA256 != e.SHA256 {
			changed = true
		}
		if a.policy != nil {
			e.Warnings = a.policy.WarnFor(a.target, e)
		}
		if a.sel.add(e) {
			added++
		}
	}
	if len(items) > 0 {
		a.undo = selectionUndo{}
	}
	if changed {
		a.selectionRevision++
	}
	s := a.summaryLocked()
	a.mu.Unlock()

	s.Added = added
	s.Rejected = rejected
	if len(items) > 0 {
		a.emitSelectionChanged(s)
	}
	return s
}

// summaryLocked builds the tray summary. Caller holds mu at least for reading.
func (a *App) revalidateSelectionLocked(ctx context.Context) {
	for _, key := range a.sel.order {
		e := a.sel.byKey[key]
		if e.Source == SourceAPT {
			e.Known, e.DisplayName, e.Version, e.Summary = false, "", "", ""
			e.InstalledSizeBytes, e.DownloadSizeBytes = 0, 0
			if a.cat != nil && a.catalogStatus.Ready {
				entry, found, err := a.cat.Get(ctx, e.Name)
				if err == nil && found {
					e.Known, e.DisplayName, e.Version, e.Summary = true, entry.AppName, entry.Version, entry.Summary
					e.InstalledSizeBytes, e.DownloadSizeBytes = entry.InstalledSizeKiB*1024, entry.DownloadSizeBytes
				}
			}
		}
		e.Warnings = nil
		if a.policy != nil {
			e.Warnings = a.policy.WarnFor(a.target, e)
		}
		a.sel.byKey[key] = e
	}
}

func (a *App) summaryLocked() SelectionSummary {
	s := SelectionSummary{Revision: a.sel.rev, UndoToken: a.undo.token, UndoLabel: a.undo.label}
	seen := map[string]bool{}
	for _, e := range a.sel.entries() {
		s.Total++
		switch e.Source {
		case SourceURL:
			s.URLCount++
		case SourceFile:
			s.FileCount++
		default:
			s.PackageCount++
		}
		if !e.Known && e.Source == SourceAPT {
			s.UnknownCount++
		}
		s.InstalledSizeBytes += e.InstalledSizeBytes
		s.DownloadSizeBytes += e.DownloadSizeBytes
		for _, w := range e.Warnings {
			k := w.Kind + "\x00" + w.Package
			if seen[k] {
				continue
			}
			seen[k] = true
			s.Warnings = append(s.Warnings, w)
		}
	}
	if a.policy != nil {
		for _, w := range a.policy.WarnForSelection(a.target, s) {
			k := w.Kind + "\x00" + w.Package
			if seen[k] {
				continue
			}
			seen[k] = true
			s.Warnings = append(s.Warnings, w)
		}
	}
	return s
}

// buildSpec turns the operator's options plus the current target and tray into
// the adapter's spec. It is the only place that translation happens, so
// PreviewCommand and StartBuild cannot disagree about what will run.
func (a *App) buildSpec(opts BuildOptions) (cliadapter.BuildSpec, *UIError) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.buildSpecLocked(opts)
}

func (a *App) buildSpecLocked(opts BuildOptions) (cliadapter.BuildSpec, *UIError) {
	target := a.target
	entries := a.sel.entries()

	if !target.Selected {
		return cliadapter.BuildSpec{}, errNoTarget()
	}
	if len(entries) == 0 {
		return cliadapter.BuildSpec{}, &UIError{
			Code:    ErrCodeNoSelection,
			Message: "Nothing has been chosen to put in the bundle.",
			Hint:    "Pick some packages, or add a vendor .deb by URL or from disk.",
		}
	}
	if opts.OutputDir == "" {
		return cliadapter.BuildSpec{}, errInvalid("No output folder was chosen.",
			"Pick the folder the bundle should be written into.")
	}

	spec := cliadapter.BuildSpec{
		Format:                    buildjob.OutputFormat(opts.Format),
		SignerRef:                 opts.SignerRef,
		NoSign:                    opts.NoSign,
		SBOM:                      opts.SBOM,
		Recommends:                opts.recommends(),
		Upgrades:                  opts.Upgrades,
		Update:                    opts.Update,
		NoPrune:                   opts.NoPrune,
		Backend:                   opts.Backend,
		AcknowledgeRedistribution: opts.AcknowledgeRedistribution,
	}
	switch target.Kind {
	case TargetKindSnapshot:
		spec.SnapshotPath = target.SnapshotPath
	default:
		spec.BaseID = target.ID
		spec.Arch = target.Arch
	}

	name := opts.OutputName
	if name == "" {
		name = defaultBundleName(target)
	}
	spec.OutPath = filepath.Join(opts.OutputDir, name)

	for _, e := range entries {
		switch e.Source {
		case SourceURL:
			// The digest is the whole reason the tray collects one: it is
			// what makes this input operator-attested rather than merely
			// downloaded. Dropping it here would build a bundle the operator
			// believes they vouched for and did not.
			spec.URLs = append(spec.URLs, buildjob.URLInput{URL: e.URL, SHA256: e.SHA256})
		case SourceFile:
			spec.LocalDebs = append(spec.LocalDebs, e.Path)
		default:
			spec.Packages = append(spec.Packages, e.Name)
		}
	}
	return spec, nil
}

// recommends resolves the two spellings of the Install-Recommends override
// into the one three-state value the adapter takes.
//
// Recommends wins when it is set, so the pair cannot contradict: NoRecommends
// is only ever consulted as the older spelling of "definitely exclude". The
// distinction the bool could not make is between "follow the target" and
// "definitely exclude", which are different builds — a target whose apt.conf
// turns recommends on gets them under the first and not the second.
func (o BuildOptions) recommends() *bool {
	if o.Recommends != nil {
		return o.Recommends
	}
	if o.NoRecommends {
		off := false
		return &off
	}
	return nil
}

// exportRequest turns export options plus application state into the exporter's
// request, including the free-space number the plan is checked against.
func (a *App) exportRequest(opts ExportOptions) (export.Request, *UIError) {
	src := opts.BundlePath
	if src == "" {
		a.mu.RLock()
		src = a.buildBundle
		a.mu.RUnlock()
	}
	if src == "" {
		return export.Request{}, errInvalid("No bundle was chosen to copy.",
			"Build a bundle first, or choose a bundle folder from disk.")
	}
	if opts.Destination == "" {
		return export.Request{}, errInvalid("No destination was chosen.",
			"Pick a mounted drive or a folder to copy the bundle into.")
	}

	sub := opts.Subdir
	if sub == "" {
		sub = filepath.Base(filepath.Clean(src))
	}
	dest := filepath.Join(opts.Destination, sub)

	// The destination is measured, not looked up.
	//
	// This used to derive the number from ListVolumes() by longest
	// mount-point prefix, and that was wrong in exactly the way the comment
	// here said it was written to prevent. ListVolumes is a DRIVE CHOOSER,
	// not a filesystem table: it deliberately omits filesystems that are not
	// destinations, so a path on one of them matched nothing, free space came
	// back unknown, and Plan downgraded the hard "this will not fit" refusal
	// to a soft "could not be measured" warning. The operator then starts a
	// copy that cannot finish and discovers it partway through, on the drive
	// they were about to carry to a machine with no network.
	//
	// The gap is not exotic: wherever "/" is rejected, nothing covers
	// /tmp/... or a path under the operator's home directory. That is every
	// container, and — by the same pseudo-filesystem rule, on a real desktop
	// with no container anywhere — a live-USB session and an overlayroot
	// install.
	//
	// export.FreeSpaceAt asks the destination itself, which is also strictly
	// more correct than the chooser's number: a bind mount, a subvolume or a
	// nested mount under a listed mount point can have different free space
	// from it. It measures dest rather than opts.Destination so that a
	// subdirectory which is itself a mount point is answered about correctly;
	// the path need not exist, and its nearest existing ancestor is used.
	//
	// It cannot block this bound method for longer than a frame: the syscall
	// runs under export's own budget, and a destination that misses it
	// genuinely has no answer and comes back FreeSpaceUnknown. Clamping,
	// f_bavail-versus-f_bfree and the two different meanings of zero all live
	// in export now rather than here — a plan-time number and the chooser's
	// number must not be computed two ways.
	return export.Request{
		SourceDir:     src,
		DestDir:       dest,
		DestFreeBytes: export.FreeSpaceAt(dest),
	}, nil
}

// recordBuildEvent is the EventSink. It must not block: the build's output pipe
// is behind this call, so it appends to a ring under a short lock and returns.
func (a *App) recordBuildEvent(gen uint64, e cliadapter.Event) {
	view := buildEventView(e)

	a.mu.Lock()
	if a.buildJob.gen != gen || !a.buildJob.running {
		a.mu.Unlock()
		return
	}
	view.Seq = a.buildLog.append(view)
	progress, changed := buildProgressFrom(a.buildState.Progress, e)
	if changed {
		a.buildState.Progress = progress
	}
	a.buildState.EventCount = a.buildLog.total
	a.mu.Unlock()

	a.emitBuildEvent(view)
	if changed {
		a.emitBuildProgress(progress)
	}
}

// ---------------------------------------------------------------------------
// The selection tray: an ordered set keyed by SelectionEntry.Key.
// ---------------------------------------------------------------------------

type selection struct {
	order []string
	byKey map[string]SelectionEntry
	// rev is the identity of the current key SET. It moves when a key enters
	// or leaves, and not when an entry's contents change under an unchanged
	// key — see SelectionSummary.Revision for why that distinction is the
	// whole value of the field.
	rev uint64
}

// init empties the tray. It bumps rev rather than resetting it: a revision
// that went backwards would let a stale SelectionKeys response look current,
// which is precisely the mistake the number exists to prevent.
func (s *selection) init() {
	s.order = nil
	s.byKey = map[string]SelectionEntry{}
	s.rev++
}

func (s *selection) len() int { return len(s.order) }

func (s *selection) has(key string) bool {
	if s.byKey == nil {
		return false
	}
	_, ok := s.byKey[key]
	return ok
}

func (s *selection) add(e SelectionEntry) bool {
	if s.byKey == nil {
		s.init()
	}
	if _, dup := s.byKey[e.Key]; dup {
		// The contents may have changed — a row hydrating against a catalogue
		// that has since been built — but the key set has not, so rev holds.
		s.byKey[e.Key] = e
		return false
	}
	s.byKey[e.Key] = e
	s.order = append(s.order, e.Key)
	s.rev++
	return true
}

func (s *selection) remove(key string) bool {
	if s.byKey == nil {
		return false
	}
	if _, ok := s.byKey[key]; !ok {
		return false
	}
	delete(s.byKey, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.rev++
	return true
}

func (s *selection) entries() []SelectionEntry {
	out := make([]SelectionEntry, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.byKey[k])
	}
	return out
}

// keys returns the tray's keys in tray order, as the caller's own slice.
//
// A copy rather than s.order itself: order is mutated in place by remove, and
// handing the live slice out would be the same defect ReadinessReport.clone
// exists to fix one screen over.
func (s *selection) keys() []string {
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// ---------------------------------------------------------------------------
// The build event ring.
// ---------------------------------------------------------------------------

type eventRing struct {
	items   []BuildEvent
	limit   int
	total   int
	dropped int
}

func (r *eventRing) reset() {
	r.items = nil
	r.total = 0
	r.dropped = 0
}

func (r *eventRing) append(e BuildEvent) int {
	r.total++
	e.Seq = r.total
	r.items = append(r.items, e)
	if r.limit > 0 && len(r.items) > r.limit {
		drop := len(r.items) - r.limit
		r.items = append([]BuildEvent(nil), r.items[drop:]...)
		r.dropped += drop
	}
	return e.Seq
}

func (r *eventRing) page(sinceSeq, limit int) BuildLogPage {
	page := BuildLogPage{Events: []BuildEvent{}, NextSeq: sinceSeq, Total: r.total, Dropped: r.dropped}
	for _, e := range r.items {
		if e.Seq <= sinceSeq {
			continue
		}
		page.Events = append(page.Events, e)
		page.NextSeq = e.Seq
		if len(page.Events) >= limit {
			break
		}
	}
	if page.NextSeq < sinceSeq {
		page.NextSeq = sinceSeq
	}
	return page
}

// ---------------------------------------------------------------------------
// Projections. Every backend type stops here.
// ---------------------------------------------------------------------------

func baseView(e base.ListEntry) BaseView {
	return BaseView{
		ID: e.ID, Description: e.Description,
		DistroID: e.DistroID, VersionID: e.VersionID, Codename: e.Codename,
		Variant: e.Variant, Arch: e.Arch,
		Seeds: e.Seeds, Excludes: e.Excludes, Recommends: e.Recommends,
		Digest: e.Digest,
	}
}

func snapshotView(path string, s snapshot.Snapshot) SnapshotView {
	v := SnapshotView{
		Path:          path,
		SchemaVersion: s.SchemaVersion,
		DistroID:      s.Target.DistroID,
		VersionID:     s.Target.VersionID,
		Codename:      s.Target.Codename,
		Arch:          s.Target.Arch,
		ForeignArchs:  s.Target.ForeignArchs,
		OriginKind:    s.Origin.Kind,
		CreatedAt:     s.CreatedAt,
		PackageCount:  s.InstalledCount,
		// All three describe the base a SYNTHESIZED snapshot was made from and
		// are empty for a captured one. The digest is deliberately not called
		// "digest": it is the base definition's, never the snapshot's, and the
		// old name promised an identity that is blank for exactly the
		// snapshots this product tells operators to prefer.
		BaseID:             s.Origin.BaseID,
		OriginSource:       s.Origin.Source,
		OriginSourceDigest: s.Origin.SourceDigest,
	}
	if s.Origin.BaseID != "" {
		if _, _, variant, ok := splitBaseID(s.Origin.BaseID); ok {
			v.Variant = variant
		}
	}
	return v
}

// bindArchiveScope collects the components and suites a resolved target's own
// sources name, first-seen order, deduplicated.
//
// First-seen rather than sorted because that is the order the target lists
// them, and a suite list reading "noble, noble-updates, noble-security" says
// something a sorted one does not. Deduplicated because several stanzas
// commonly name the same suite, and the screen is answering "what does this
// catalogue cover", not "how many stanzas are there".
func bindArchiveScope(sources []catalog.Source) (components, suites []string) {
	seenC, seenS := map[string]bool{}, map[string]bool{}
	for _, src := range sources {
		for _, c := range src.Components {
			if c == "" || seenC[c] {
				continue
			}
			seenC[c] = true
			components = append(components, c)
		}
		for _, su := range src.Suites {
			if su == "" || seenS[su] {
				continue
			}
			seenS[su] = true
			suites = append(suites, su)
		}
	}
	return components, suites
}

func targetView(sel TargetSelection, t catalog.Target) TargetView {
	v := TargetView{
		Selected:  true,
		Kind:      sel.Kind,
		DistroID:  t.DistroID,
		VersionID: t.VersionID,
		Codename:  t.Codename,
		Arch:      t.Arch,
	}
	switch sel.Kind {
	case TargetKindSnapshot:
		v.ID = t.Identity()
		v.SnapshotPath = sel.SnapshotPath
		// A snapshot is usually a measurement, but not always: `snapshot
		// from-base` writes one synthesized from a stock base, and that is an
		// assumption about a machine nobody has looked at. Reporting it as
		// measured is the one mistake this distinction exists to prevent — if
		// the base assumed packages the real target lacks, the bundle is short
		// and the operator finds out on the offline side, where they cannot
		// fix it.
		//
		// The resolver carries the snapshot's origin.base_id through in
		// Target.BaseID, which is empty for a captured snapshot. That
		// emptiness is the signal; there is no origin-kind field on the frozen
		// catalog.Target.
		if t.BaseID != "" {
			v.Assumed = true
			v.Caveat = SynthesizedSnapshotCaveat
			v.Variant = ""
			if _, _, variant, ok := splitBaseID(t.BaseID); ok {
				v.Variant = variant
			}
		}
	default:
		v.ID = sel.BaseID
		if t.BaseID != "" {
			v.ID = t.BaseID
		}
		if _, _, variant, ok := splitBaseID(v.ID); ok {
			v.Variant = variant
		}
		v.Assumed = true
		v.Caveat = BaseCaveat
	}
	v.Components, v.Suites = bindArchiveScope(t.Sources)
	v.Label = t.Display()
	if v.Label == "" {
		v.Label = v.ID
	}
	return v
}

func catalogStateFor(ready bool) CatalogState {
	if ready {
		return CatalogStateReady
	}
	return CatalogStateNotBuilt
}

func catalogProgressView(p catalog.Progress) CatalogProgress {
	return CatalogProgress{
		Phase:           CatalogPhase(p.Phase),
		PhaseIndex:      p.PhaseIndex,
		PhaseCount:      p.PhaseCount,
		Label:           p.Label,
		Item:            p.Item,
		Current:         p.Current,
		Total:           p.Total,
		BytesDone:       p.BytesDone,
		BytesTotal:      p.BytesTotal,
		Fraction:        p.Fraction(),
		OverallFraction: p.OverallFraction(),
		ElapsedMS:       p.Elapsed.Milliseconds(),
	}
}

// readinessLocate answers "which debark" for the readiness package, using
// the binary this application actually invokes rather than a second search.
//
// internal/readiness has its own stand-in locator (PATH, then beside the
// executable) and its BinaryLocator comment names this wiring as the
// application layer's job, because that package must not import
// internal/cliadapter. Until it was wired, the two agreed only by coincidence:
// they agree whenever the adapter is discovering the binary the same way, and
// disagree the moment cliadapter.Options.BinaryPath names one somewhere else —
// which is exactly what that field exists for. The symptom was severe out of
// all proportion to the cause: the debark-binary row is the only BLOCKING
// row on the first screen, so a GUI happily running an explicitly configured
// binary still reported "The debark command-line tool was not found on this
// machine" and refused to let the operator continue. Found by hack/e2e, which
// drives a binary that is deliberately not on PATH.
//
// The fallback to cliadapter.Locate matters for the case where the binary is
// present but does not answer `version --json`: Probe returns a zero Probe
// then, and reporting "not found" for a binary that is right there would be a
// worse answer than the one the readiness row can give from a path.
func (a *App) readinessLocate(_ context.Context) (string, error) {
	if p, uerr := a.cliProbe(); uerr == nil && p.Path != "" {
		return p.Path, nil
	}
	return cliadapter.Locate()
}

// debarkPath is the located binary, or "" when there is none. Used to make a
// readiness action name the same binary the rest of the application runs.
func (a *App) debarkPath() string {
	p, uerr := a.cliProbe()
	if uerr != nil {
		return ""
	}
	return p.Path
}

func readinessView(r readiness.Report, bin string) ReadinessReport {
	out := ReadinessReport{
		Checks:    make([]ReadinessCheck, 0, len(r.Results)),
		Platform:  r.Platform,
		CheckedAt: r.StartedAt.UTC().Format(time.RFC3339),
		// Duration is the wall time of the whole concurrent run, which is the
		// number the cold-start budget cares about.
		DurationMS: r.Duration.Milliseconds(),
	}
	for _, res := range r.Results {
		out.Checks = append(out.Checks, readinessCheckView(res, bin))
	}
	out.recount()
	return out
}

// readinessCheckView projects one row. bin is the located debark binary, or
// "" when there is none.
//
// bin exists because internal/readiness writes its remedy commands with the
// program NAME — `debark keygen --out …` — which is the right thing for that
// package to do: it is the command an operator would type, and that package
// has no way to know which binary this application settled on. But
// RunReadinessAction execs the argv, so a bare name means the remedy button
// runs whatever is on PATH, which is a different binary from the one every
// build uses, or none at all. Substituting here keeps rule 8 honest too: the
// command shown is the command run, because the display is rendered from the
// rewritten argv rather than the original.
func readinessCheckView(res readiness.Result, bin string) ReadinessCheck {
	c := ReadinessCheck{
		ID:         res.ID,
		Title:      res.Title,
		Status:     ReadinessStatus(res.Status),
		Severity:   ReadinessSeverity(res.Severity),
		Summary:    res.Summary,
		Remedy:     res.Remedy,
		Detail:     res.Detail,
		DurationMS: res.Duration.Milliseconds(),
		// The readiness package refuses this one by name in RunOne, because
		// it has no probe: it is computed from the apt, WSL and container
		// rows. Saying so on the row is what lets a screen not offer a button
		// that cannot work — on one of only two blocking rows.
		Derived: res.ID == CheckBuildEnvironment,
	}
	if res.Action != nil {
		argv := readinessActionArgv(res.Action.Command, bin)
		c.Action = &ReadinessAction{
			Label:    res.Action.Label,
			Command:  argv,
			Display:  shellDisplay(argv),
			Elevated: res.Action.Elevated,
			Runnable: !res.Action.Elevated && len(argv) > 0,
			Note:     res.Action.Note,
		}
	}
	return c
}

// readinessActionArgv replaces a bare "debark" at argv[0] with the binary
// this application located. Every other command — `sudo apt-get install
// docker.io`, `wsl --install` — is left exactly as the readiness package wrote
// it, because those name programs this application does not own.
func readinessActionArgv(cmd []string, bin string) []string {
	if len(cmd) == 0 || bin == "" || cmd[0] != cliadapter.ProgramName {
		return cmd
	}
	out := make([]string, len(cmd))
	copy(out, cmd)
	out[0] = bin
	return out
}

// clone returns a report whose rows are the caller's own.
//
// A plain copy of the struct copies the slice HEADER, not the array behind it,
// and this package writes into that array in place — runReadiness sets
// Checks[i].Running while a row re-runs, and a re-check replaces the whole
// row set. So every ReadinessReport handed out shared storage with the one
// the application keeps mutating: a Go caller that held a row saw it change
// under it, and reading it while a check ran was a data race the frontend was
// only spared by the bridge serialising each value immediately.
//
// Found by hack/e2e, which held the signing-key row across running that row's
// own remedy and crashed on a nil Action when the row was replaced underneath
// it by the passing version.
//
// Action pointers are deliberately shared rather than deep-copied: nothing in
// this package mutates an Action in place — readinessCheckView builds a fresh
// one on every projection — so the pointer is immutable in practice and
// copying it would only add garbage on a path the shell calls often.
func (r ReadinessReport) clone() ReadinessReport {
	if r.Checks == nil {
		return r
	}
	rows := make([]ReadinessCheck, len(r.Checks))
	copy(rows, r.Checks)
	r.Checks = rows
	return r
}

// recount recomputes the derived headline fields from the rows.
func (r *ReadinessReport) recount() {
	r.BlockingCount, r.DegradedCount = 0, 0
	for _, c := range r.Checks {
		if c.Status != ReadinessProblem {
			continue
		}
		switch c.Severity {
		case SeverityBlocking:
			r.BlockingCount++
		case SeverityDegraded:
			r.DegradedCount++
		}
	}
	r.CanBuild = r.BlockingCount == 0 && len(r.Checks) > 0
	r.RemoteBuilder = bindRemoteBuilder(r.Checks, r.Platform)
}

// bindRemoteBuilder decides whether "build on a machine you can reach" applies,
// and returns it as data.
//
// The decision itself is not new — the readiness screen was making it, by
// matching on the platform string plus a set of check ids, because the advice
// existed only as prose inside one row's Remedy. That put a product decision in
// the view layer, where a rewording of that Remedy would silently change when
// the panel appeared. It belongs here.
//
// Two ways in. The first is the derived build-environment row failing, which is
// the flat statement that nothing on this machine can run apt.
//
// The second is the Windows case that row does not always reach in time. It
// used to count WSL and the container runtime as the two local routes and fire
// only when BOTH were blocked. There are not two: debark runs apt natively or
// in a container and nothing else, native apt is not registered on Windows, and
// WSL 2 is not a route (see readiness.DeriveBuildEnvironment). So on Windows
// there is exactly ONE local route, and it takes two rows to work — a container
// runtime that answers, and a Linux build of debark to mount into it, because
// a non-Linux host re-execs what it mounts.
//
// Either row failing breaks that one route, so the condition is "any of them",
// not "all of them". Requiring all of them meant a machine with working WSL 2
// and a broken container route never saw this panel at all, which is the same
// arithmetic that let a green WSL row defeat the self-binary gate one layer
// down. Since the derivation now reaches every one of those machines on its
// own, this arm is insurance against a stale derived row rather than the main
// path, and it cannot fire where the derivation says the machine can build:
// build-environment is only ok on Windows when both of these rows are ok.
func bindRemoteBuilder(checks []ReadinessCheck, platform string) *RemoteBuilderAdvice {
	var blocked []string
	localBlocked, noEnvironment := 0, false

	for _, c := range checks {
		problem := c.Status == ReadinessProblem
		switch c.ID {
		case CheckBuildEnvironment:
			noEnvironment = problem
		case CheckContainer, CheckSelfBinary:
			if problem {
				localBlocked++
			}
		}
		// apt is a route too, on the platform that has it, so it belongs in
		// the list of rows to point at even though it is not a "local option"
		// in the Windows sense below.
		//
		// self-binary is not a route at all: it is the gate on the container
		// one, because on a non-Linux host debark mounts and re-execs a
		// Linux build of itself. It belongs here for the same reason it
		// belongs in DeriveBuildEnvironment's reasoning — when it is the
		// obstacle, a panel that named only the container row would point the
		// operator at a runtime that is already running.
		if problem && (c.ID == CheckAPT || c.ID == CheckWSL || c.ID == CheckContainer || c.ID == CheckSelfBinary) {
			blocked = append(blocked, c.ID)
		}
	}

	switch {
	case noEnvironment:
	case strings.HasPrefix(platform, "windows") && localBlocked > 0:
	default:
		return nil
	}
	reason := RemoteBuilderNoEnvironment
	if !noEnvironment {
		reason = RemoteBuilderLocalBlocked
	}
	return &RemoteBuilderAdvice{
		Reason:  reason,
		Message: RemoteBuilderMessage,
		Hint:    RemoteBuilderHint,
		Blocked: blocked,
	}
}

func readinessProgressMessage(isAction bool, action *ReadinessAction) string {
	if isAction && action != nil {
		return action.Label
	}
	return "Checking"
}

func volumeView(v export.Volume) VolumeView {
	return VolumeView{
		Path: v.Path, Label: v.Label, Device: v.Device, FSType: v.FSType,
		TotalBytes: v.TotalBytes, FreeBytes: v.FreeBytes,
		Kind:      VolumeKind(v.Kind.String()),
		Removable: v.Removable, ReadOnly: v.ReadOnly, Writable: v.Writable,
		Note: v.Note,
	}
}

func planView(p *export.Plan) ExportPlan {
	if p == nil {
		return ExportPlan{}
	}
	return ExportPlan{
		SourceDir: p.SourceDir, DestDir: p.DestDir,
		TotalFiles: p.TotalFiles, TotalBytes: p.TotalBytes,
		RequiredBytes: p.RequiredBytes, FreeBytes: p.FreeBytes,
		MarginBytes: p.MarginBytes, Warnings: p.Warnings,
		EquivalentCommand: p.EquivalentCommand(),
	}
}

func exportProgressView(p export.Progress) ExportProgress {
	view := ExportProgress{
		File:       p.File,
		FilesDone:  p.FilesDone,
		FilesTotal: p.FilesTotal,
		BytesDone:  p.BytesDone,
		BytesTotal: p.BytesTotal,
		Fraction:   -1,
	}
	switch p.Phase {
	case export.PhaseCopy:
		view.Phase = ExportPhaseCopying
	case export.PhaseFlush:
		view.Phase = ExportPhaseFlushing
	case export.PhaseVerify:
		view.Phase = ExportPhaseVerifying
	default:
		view.Phase = ExportPhaseCopying
	}
	if p.BytesTotal > 0 {
		view.Fraction = float64(p.BytesDone) / float64(p.BytesTotal)
	}
	if p.Elapsed > 0 {
		view.BytesPerSec = int64(float64(p.BytesDone) / p.Elapsed.Seconds())
	}
	return view
}

// applyVerification folds the exporter's own verification report into the
// status the screen renders.
//
// Method and Caveat come across because they are the exporter's account of what
// it checked and what that does and does not prove, and only the exporter knows
// it. The frontend used to carry both sentences as hard-coded constants — the
// exact drift the view layer exists to prevent, because a change to what the
// exporter verifies would have left the screen making a claim that was no
// longer true.
//
// Mismatches come across because a failed verification without them is a wall.
// "The copy did not verify" is not an answer; which files, and how they
// differed, is. They are clamped like every other list on this surface.
func applyVerification(status *ExportStatus, v *export.VerifyReport) {
	if status == nil || v == nil {
		return
	}
	status.VerifyMethod = v.Method
	status.VerifyCaveat = v.Caveat
	status.FilesChecked = v.FilesChecked
	status.BytesChecked = v.BytesChecked
	status.MismatchCount = len(v.Mismatches)

	shown := v.Mismatches
	if len(shown) > ExportMismatchMax {
		shown = shown[:ExportMismatchMax]
		status.MismatchesTruncated = true
	}
	status.Mismatches = make([]ExportMismatch, 0, len(shown))
	for _, m := range shown {
		status.Mismatches = append(status.Mismatches, ExportMismatch{
			Path:       m.Rel,
			Reason:     string(m.Reason),
			Detail:     m.Detail,
			WantBytes:  m.WantSize,
			GotBytes:   m.GotSize,
			WantSHA256: m.WantSHA256,
			GotSHA256:  m.GotSHA256,
		})
	}
}

func verifyStatusView(path string, r verify.Report) VerifyStatus {
	v := VerifyStatus{
		BundlePath: path,
		OK:         r.OK,
		Signed:     r.Signed,
		Findings:   []VerifyFinding{},
	}
	if v.BundlePath == "" {
		v.BundlePath = r.BundlePath
	}
	for _, p := range r.Problems {
		f := VerifyFinding{Severity: "error", Code: p.Kind, Message: p.Message, Path: p.Path}
		if p.Expected != "" || p.Got != "" {
			f.Hint = fmt.Sprintf("Expected %s, found %s.", p.Expected, p.Got)
		}
		v.Findings = append(v.Findings, f)
	}
	for _, w := range r.Warnings {
		v.Findings = append(v.Findings, VerifyFinding{Severity: "warning", Message: w})
	}
	return v
}

func buildSummary(r *buildjob.BuildResult) *BuildSummary {
	if r == nil {
		return nil
	}
	s := &BuildSummary{
		BundlePath: r.BundlePath, BundleID: r.BundleID,
		LockRef: r.LockRef, ManifestRef: r.ManifestRef,
		Signed:    r.Signed,
		ExitClass: string(r.ExitClass),
		Stats: BuildStats{
			Added: r.Stats.Added, Removed: r.Stats.Removed, Unchanged: r.Stats.Unchanged,
			PackageCount: r.Stats.PackageCount, Bytes: r.Stats.Bytes,
			DownloadedBytes: r.Stats.DownloadedBytes, DurationSeconds: r.Stats.DurationSeconds,
		},
	}
	s.Warnings, s.Truncated = clampStrings(r.Warnings, s.Truncated)
	s.Unresolved, s.Truncated = clampStrings(r.Unresolved, s.Truncated)
	s.FetchFailed, s.Truncated = clampStrings(r.FetchFailed, s.Truncated)
	return s
}

func clampStrings(in []string, truncated bool) ([]string, bool) {
	if len(in) <= SummaryListMax {
		return in, truncated
	}
	return in[:SummaryListMax], true
}

func buildEventView(e evidence.Event) BuildEvent {
	v := BuildEvent{TS: e.TS, Type: e.Type, Level: e.Level, Msg: e.Msg, Attrs: e.Attrs}
	// Raw is what the details drawer shows, and the drawer is the reason this
	// audience trusts the screen at all — so leaving the field declared,
	// documented and empty was worse than not having it. This is a re-encode
	// of the decoded event rather than the literal bytes off the pipe (the
	// adapter hands over a decoded evidence.Event, not the line), which is
	// faithful in content and may differ in key order. Said plainly on the
	// field's own doc comment rather than implied here.
	if b, err := json.Marshal(e); err == nil {
		v.Raw = string(b)
	}
	return v
}

// buildBaseName reduces a URL or path to the file name a person recognises.
//
// It exists because the two event shapes that name a file disagree: a
// `progress` event carries a full URL and a `fetch.file` event carries a bare
// filename, so a consumer reading BuildProgress.Package saw the field change
// shape mid-download. Normalising here means every consumer sees one thing.
func buildBaseName(s string) string {
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/\\"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return s
}

// buildProgressFrom folds one event into the progress state. It reports whether
// anything the UI renders actually changed, so that a build emitting thousands
// of events does not emit thousands of identical progress updates.
//
// It maps debark's event types onto the GUI's phases and does nothing else.
// It is not reasoning about the build; it is reading a label off a stream.
func buildProgressFrom(cur BuildProgress, e evidence.Event) (BuildProgress, bool) {
	next := cur
	switch e.Type {
	case evidence.TypeBuildStarted, evidence.TypeSnapshotLoaded, evidence.TypeBackendSelected:
		next.Phase = BuildPhaseStarting
	case evidence.TypeAPTUpdate, evidence.TypeAPTResolve:
		next.Phase = BuildPhaseResolving
	case evidence.TypeFetchFile, evidence.TypeStoreHit, evidence.TypeProgress:
		next.Phase = BuildPhaseDownloading
		if p, ok := cliadapter.ProgressOf(e); ok {
			next.BytesDone, next.BytesTotal = p.Bytes, p.TotalBytes
			// Deliberately NOT p.Fraction(). That is one file's progress, and
			// downloads run concurrently, so assigning it here makes the run
			// fraction jump between unrelated files. There is no run-level
			// denominator on the wire at all — nothing says how many files a
			// build will fetch — so the honest value is indeterminate.
			next.Fraction = -1
			if p.URL != "" {
				next.Package = buildBaseName(p.URL)
			}
		}
		if f, ok := cliadapter.FetchFileOf(e); ok && f.Filename != "" {
			next.Package = buildBaseName(f.Filename)
		}
	case evidence.TypeRepoIndexed, evidence.TypeBundleAssembled, evidence.TypePruned:
		next.Phase = BuildPhaseAssembling
	case evidence.TypeManifestSigned:
		next.Phase = BuildPhaseSigning
	case evidence.TypeVerifyResult, evidence.TypeClosedWorld:
		next.Phase = BuildPhaseVerifying
	case evidence.TypeBuildFinished:
		next.Phase = BuildPhaseFinished
		next.Fraction = 1
	}
	if next.Phase != BuildPhaseDownloading && cur.Phase == BuildPhaseDownloading {
		// Leaving the download phase clears the per-file numbers. Without
		// this a finished build keeps reporting whatever file happened to be
		// in flight when the last progress event arrived.
		next.Package = ""
		next.BytesDone, next.BytesTotal = 0, 0
	}
	if e.Msg != "" {
		next.Message = e.Msg
	}
	return next, next != cur
}

// ---------------------------------------------------------------------------
// Errors. Everything that becomes a UIError goes through here.
// ---------------------------------------------------------------------------

func ok() Result               { return Result{OK: true} }
func failed(e *UIError) Result { return Result{OK: false, Error: e} }

func errInvalid(message, hint string) *UIError {
	return &UIError{Code: ErrCodeInvalidInput, Message: message, Hint: hint}
}

func errBusy(message string) *UIError {
	return &UIError{Code: ErrCodeBusy, Message: message,
		Hint: "Wait for it to finish, or cancel it first."}
}

func errNoTarget() *UIError {
	return &UIError{Code: ErrCodeNoTarget,
		Message: "No target has been chosen yet.",
		Hint:    "Pick a stock base, or a snapshot file taken from the machine you are building for."}
}

func errCatalogNotReady() *UIError {
	return &UIError{Code: ErrCodeCatalogNotReady,
		Message: "The package list for this target has not been built yet.",
		Hint:    "Build the catalogue — it downloads the target's own package indexes once and caches them."}
}

func errTooMany(got, limit int, noun string) *UIError {
	return &UIError{Code: ErrCodeTooManyItems,
		Message: fmt.Sprintf("That is %d %s at once, and the limit is %d.", got, noun, limit),
		Hint:    fmt.Sprintf("Send them in batches of %d or fewer.", limit)}
}

func errNoDep(what string) *UIError {
	return &UIError{Code: ErrCodeNotImplemented,
		Message: fmt.Sprintf("This build of the application has no %s wired in.", what),
		Hint:    "This is a development build. Report it: the application was constructed without one of its dependencies."}
}

func errNoRuntime(what string) *UIError {
	return &UIError{Code: ErrCodeNoRuntime,
		Message: fmt.Sprintf("This application cannot %s without its desktop window.", what),
		Hint:    "This happens only in a headless test harness."}
}

func dialogError(err error) *UIError {
	return &UIError{Code: ErrCodeInternal,
		Message: "The file picker could not be opened.",
		Hint:    "Type the path instead, if the screen offers a field for it.",
		Details: err.Error()}
}

// uiErrorFrom converts any error into something renderable. A *cliadapter.Error
// already carries a summary, a hint, the argv, the captured stderr and the exit
// class, so it converts field for field; anything else gets a generic shell
// that still has a message and still says what to do next.
//
// Nothing on this surface may show an exit code on its own. That is a
// definition-of-done item for the whole project, and this function is where it
// is kept.
//
// # Command is redacted here, and this is the fourth call site
//
// cliadapter.Error.Argv() is the exact argv that failed, credentials and all —
// internal/cliadapter/events.go says so in as many words, noting that "Error.
// Argv is an exported accessor with a 'copy the command' button behind it".
// UIError.Command is that button's content. So this is a render site by
// exactly the definition docs/security-review.md §6.1a uses, and it goes
// through RedactArgv like the other three.
//
// It was not one of the three §6.1a enumerated, and that is the point rather
// than a footnote: the review's own argument for putting the redaction in ONE
// function was that "a redaction applied at each place someone remembers is a
// redaction that will be missed at the place nobody thought of". This was that
// place. §6.1 states that the error path "already redacts it" — true of
// errSafeURL on the summary's URL fields, false of the argv, and the drawer is
// where an operator goes when something breaks. Driven, not read: a build with
// a credentialed vendor URL put the password in plain text, twice, in the
// "What failed, verbatim" drawer of the running app.
//
// Blast radius is small and worth stating: RedactArgv only rewrites a "url:"
// positional input and the URL half of a --digest pair, so it is a no-op on
// every argv that is not a build — a readiness remedy, verify, keygen,
// snapshot inspect. Only the build error changes, and only when a vendor URL
// carried userinfo or a query string.
//
// The cost is that "What failed, verbatim" is now verbatim except for the
// credential. That is the right trade for a string with a copy button, and it
// is the same one made for the preview and the build screen — but the drawer's
// own heading now overstates what it shows, and the words are frontend's.
func uiErrorFrom(err error) *UIError {
	if err == nil {
		return nil
	}
	if ce, ok := cliadapter.AsError(err); ok {
		code := ce.ExitCode()
		out := &UIError{
			Code:      ErrCodeCLI,
			Message:   ce.Summary(),
			Hint:      ce.Hint(),
			Details:   ce.Stderr(),
			Command:   cliadapter.RedactArgv(ce.Argv()),
			ExitClass: ce.ClassName(),
			ExitCode:  &code,
			Retryable: retryableClass(ce.ClassName()),
		}
		if ce.Canceled() {
			out.Code = ErrCodeCancelled
			out.Retryable = true
		}
		if out.Message == "" {
			out.Message = "debark could not finish the command."
			out.Hint = "Open the details drawer to see what it printed."
		}
		return out
	}
	if errors.Is(err, context.Canceled) {
		return &UIError{Code: ErrCodeCancelled, Message: "That was cancelled.", Retryable: true}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &UIError{Code: ErrCodeInternal,
			Message:   "That took longer than expected and was given up on.",
			Hint:      "Try again; if it keeps happening, run the same command yourself to see where it stops.",
			Retryable: true}
	}
	return &UIError{Code: ErrCodeInternal,
		Message: firstSentence(err.Error()),
		Hint:    "Open the details drawer for the full message.",
		Details: err.Error()}
}

// retryableClass says whether running the same command again could plausibly
// work. A usage or policy failure cannot; an environment or fetch failure can.
func retryableClass(class string) bool {
	switch class {
	case "environment", "incomplete", "resolution":
		return true
	default:
		return false
	}
}

// uiErrorFromCatalog turns the catalogue's sentinels into rows the operator can
// act on. Each sentinel exists precisely because it has a different next step,
// so each gets a different hint.
func uiErrorFromCatalog(err error) *UIError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, catalog.ErrNotBuilt):
		return errCatalogNotReady()
	case errors.Is(err, catalog.ErrStale):
		return &UIError{Code: ErrCodeCatalogNotReady,
			Message:   "The cached package list is older than what the archive is publishing now.",
			Hint:      "It is still fine to browse. Refresh it when you want the newest versions shown.",
			Retryable: true}
	case errors.Is(err, catalog.ErrCacheBusy):
		// Not a fault, and specifically not the "rebuild it" advice below: the
		// cache is mid-commit and will be readable in a moment. Offering a
		// rebuild here would send an operator into a forty-second download to
		// fix a file that was about to be fine.
		return &UIError{Code: ErrCodeCatalogNotReady,
			Message:   "The package list is being written right now.",
			Hint:      "Try again in a moment — another window or another run is finishing a rebuild.",
			Details:   err.Error(),
			Retryable: true}
	case errors.Is(err, catalog.ErrCacheCorrupt), errors.Is(err, catalog.ErrCacheVersion):
		return &UIError{Code: ErrCodeCatalogFailed,
			Message:   "The cached package list could not be read.",
			Hint:      "Rebuild it — the cache is derived data and rebuilding is always safe.",
			Details:   err.Error(),
			Retryable: true}
	case errors.Is(err, catalog.ErrBuildInProgress):
		return errBusy("The catalogue is already being built.")
	case errors.Is(err, catalog.ErrClosed):
		return &UIError{Code: ErrCodeInternal,
			Message: "The package list was closed while it was still in use.",
			Hint:    "Restart the application.",
			Details: err.Error()}
	case errors.Is(err, context.Canceled):
		return &UIError{Code: ErrCodeCancelled, Message: "The catalogue build was cancelled.", Retryable: true}
	}
	return &UIError{Code: ErrCodeCatalogFailed,
		Message:   firstSentence(err.Error()),
		Hint:      "Check the network and the target's archive addresses, then try again.",
		Details:   err.Error(),
		Retryable: true}
}

// uiErrorFromExport converts the exporter's own error type, which already
// carries a summary, a hint and a detail block written for this screen.
func uiErrorFromExport(err error) *UIError {
	if err == nil {
		return nil
	}
	var ee *export.ExportError
	if errors.As(err, &ee) {
		return &UIError{
			Code:      ErrCodeExportFailed,
			Message:   ee.Summary,
			Hint:      ee.Hint,
			Details:   ee.Detail(),
			Retryable: !export.IsOutOfSpace(err),
		}
	}
	if errors.Is(err, export.ErrUnsupportedPlatform) {
		return &UIError{Code: ErrCodeNotImplemented,
			Message: "This platform cannot list mounted drives.",
			Hint:    "Choose a folder instead; the copy itself works the same way."}
	}
	return &UIError{Code: ErrCodeExportFailed,
		Message: firstSentence(err.Error()),
		Hint:    "Check the drive is still connected and has room, then try again.",
		Details: err.Error(), Retryable: true}
}

func cliadapterCancelled(err error) bool {
	if ce, ok := cliadapter.AsError(err); ok {
		return ce.Canceled()
	}
	return errors.Is(err, context.Canceled)
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// defaultResolver is the TargetResolver used when none is injected.
//
// It delegates to catalog.ResolveTarget, which reads a stock base's apt
// sources out of base.Definition.Sources and a snapshot's out of the archive's
// own captured sources files, parses them, and reports every stanza it could
// not use. An earlier version of this type filled the identity fields from the
// CLI and left Sources empty, because at the time nothing in the tree could
// produce them; a real catalogue build then failed Target.Validate. That is
// no longer true, and the CLI is the wrong place to get them from anyway —
// `snapshot list-bases --json` deliberately omits sources.
type defaultResolver struct{ cli cliadapter.Adapter }

func (d defaultResolver) Resolve(ctx context.Context, sel TargetSelection) (catalog.Target, []catalog.SourceProblem, error) {
	// Deliberately not routed through the CLI adapter. Both halves come from
	// debark's own exported packages in-process — base.Resolve and
	// snapshot.Open — so there is no subprocess to spawn and no --json shape
	// to keep in step. See docs/dev/catalogue-sourcing.md.
	return catalog.ResolveTarget(ctx, catalog.TargetSelection{
		Kind:         catalogKindFor(sel.Kind),
		BaseID:       sel.BaseID,
		Arch:         sel.Arch,
		SnapshotPath: sel.SnapshotPath,
	})
}

// catalogKindFor maps this package's target kind onto the catalogue's. The two
// vocabularies are deliberately separate — the bridge is a serialisation
// boundary and the view types must not be a catalog import away from the
// frontend — so the mapping lives here rather than either package assuming the
// other's spelling.
func catalogKindFor(k TargetKind) catalog.TargetKind {
	if k == TargetKindSnapshot {
		return catalog.TargetSnapshot
	}
	return catalog.TargetBase
}

// newSelectionEntry classifies one raw input. It validates shape only: it makes
// no network request, opens no file, and asks nothing about what a package
// contains. debark answers all of that at build time.
func newSelectionEntry(v string, src PackageSource) (SelectionEntry, *RejectedInput) {
	switch src {
	case SourceURL:
		if !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			return SelectionEntry{}, &RejectedInput{Value: v,
				Reason: "That is not an http or https URL.",
				Hint:   "Paste the full download address of the .deb, starting with https://."}
		}
		return SelectionEntry{Key: v, Name: pathBase(v), URL: v, Source: SourceURL, Known: true}, nil
	case SourceFile:
		abs, err := filepath.Abs(v)
		if err != nil {
			abs = v
		}
		if !strings.EqualFold(filepath.Ext(abs), ".deb") {
			return SelectionEntry{}, &RejectedInput{Value: v,
				Reason: "That file is not a .deb.",
				Hint:   "Pick the .deb the vendor gave you."}
		}
		return SelectionEntry{Key: abs, Name: filepath.Base(abs), Path: abs, Source: SourceFile, Known: true}, nil
	default:
		if !validPackageName(v) {
			return SelectionEntry{}, &RejectedInput{Value: v,
				Reason: "That is not a package name.",
				Hint:   "Package names are lower case letters, digits, plus, minus and dots."}
		}
		return SelectionEntry{Key: v, Name: v, Source: SourceAPT}, nil
	}
}

// validPackageName checks the shape Debian policy defines. It is a spelling
// check, not a decision: whether the package exists, and what it depends on, is
// apt's business and debark's to ask.
func validPackageName(s string) bool {
	if len(s) < 2 {
		return false
	}
	// A "name=version" pin is accepted whole; the version half is passed
	// through to the CLI untouched and is never compared with anything.
	if i := strings.IndexByte(s, '='); i > 0 {
		s = s[:i]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+' || c == '-' || c == '.':
			if i == 0 {
				return false
			}
		case c >= 'A' && c <= 'Z':
			// Uppercase is not legal in a binary package name, but rejecting
			// a pasted "Firefox" with a spelling hint is friendlier than
			// letting the build fail thirty seconds later.
			return false
		default:
			return false
		}
	}
	return true
}

// parseNameList splits a pasted blob the way a packages.txt is read: one name
// per line, "#" starts a comment, blank lines ignored, duplicates collapsed.
func parseNameList(text string) ([]string, []RejectedInput) {
	var names []string
	var rejected []RejectedInput
	seen := map[string]bool{}

	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, f := range strings.Fields(line) {
			if seen[f] {
				continue
			}
			seen[f] = true
			if !validPackageName(f) {
				rejected = append(rejected, RejectedInput{Value: f,
					Reason: "That is not a package name.",
					Hint:   "Remove it, or add it as a URL or a local .deb instead."})
				continue
			}
			names = append(names, f)
		}
	}
	return names, rejected
}

func defaultBundleName(t TargetView) string {
	parts := []string{"bundle"}
	if t.DistroID != "" {
		parts = []string{t.DistroID}
	}
	if t.VersionID != "" {
		parts = append(parts, t.VersionID)
	}
	if t.Arch != "" {
		parts = append(parts, t.Arch)
	}
	parts = append(parts, time.Now().Format("20060102"))
	return strings.Join(parts, "-")
}

// splitBaseID splits "<distro>:<version>/<variant>".
func splitBaseID(id string) (distro, version, variant string, ok bool) {
	colon := strings.IndexByte(id, ':')
	if colon <= 0 {
		return "", "", "", false
	}
	distro = id[:colon]
	rest := id[colon+1:]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return distro, rest[:slash], rest[slash+1:], true
	}
	return distro, rest, "", true
}

func pathBase(u string) string {
	u = strings.SplitN(u, "?", 2)[0]
	if i := strings.LastIndexByte(u, '/'); i >= 0 && i+1 < len(u) {
		return u[i+1:]
	}
	return u
}

// shellDisplay renders an argv the way a person would type it, quoting only
// what needs quoting. For showing and for copying; nothing here ever passes a
// string to a shell.
func shellDisplay(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, s := range argv {
		if s == "" || strings.ContainsAny(s, " \t\"'\\$`*?[]{}();&|<>#~!") {
			parts = append(parts, "'"+strings.ReplaceAll(s, "'", `'\''`)+"'")
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// firstSentence trims a raw error down to something a banner can hold, leaving
// the whole thing for the details drawer.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Something went wrong."
	}
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// sourceProblemViews projects the catalogue's source problems onto the bridge.
//
// A projection rather than a pass-through, like every other view type here:
// catalog.SourceProblem carries a Stanza index and a Format that mean
// something to a parser test and nothing to an operator, and pinning the
// frontend to the catalogue's field names would make an internal rename a
// frontend change.
func sourceProblemViews(problems []catalog.SourceProblem) []SourceProblemView {
	if len(problems) == 0 {
		return nil
	}
	out := make([]SourceProblemView, 0, len(problems))
	for _, p := range problems {
		out = append(out, SourceProblemView{
			Kind:       string(p.Kind),
			Deliberate: p.Deliberate(),
			File:       p.File,
			Line:       p.Line,
			Reason:     p.Reason,
			Text:       p.Text,
		})
	}
	return out
}
