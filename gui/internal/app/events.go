package app

import (
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// This file is the event half of the frozen binding surface: the exact set of
// names the backend may emit, and the only helpers that may emit them.
//
// The frontend subscribes by string. That makes an event name a published
// identifier with no compiler behind it, so three things hold here:
//
//  1. Every name is a constant. Nothing in this repository may call
//     runtime.EventsEmit with a literal.
//  2. Every name is `noun:verb`, lower case, one colon.
//  3. Every name has a typed helper below, so a caller cannot pair the right
//     name with the wrong payload. The helpers are how the other
//     packages emit; they should never touch emit directly.
//
// The list is mirrored in docs/dev/binding-surface.md. If the two ever
// disagree, this file is right and the document is stale — but they must not
// disagree, because the frontend reads the document.

// ---------------------------------------------------------------------------
// Event names. This list is the contract. Adding one is additive; renaming one
// is a breaking change to five frontend packages at once.
// ---------------------------------------------------------------------------

const (
	// EventAppLifecycle updates close state and availability of draft edits.
	EventAppLifecycle = "app:lifecycle"
	// EventReadinessStarted fires once when a readiness pass or a readiness
	// action begins. Payload: ReadinessReport (with Checking true).
	EventReadinessStarted = "readiness:started"
	// EventReadinessProgress fires per check evaluated, and while an action
	// runs. Payload: ReadinessProgress.
	EventReadinessProgress = "readiness:progress"
	// EventReadinessFinished fires exactly once per pass or action, including
	// cancelled and failed ones. Payload: ReadinessFinished.
	EventReadinessFinished = "readiness:finished"

	// EventTargetChanged fires whenever the selected target changes, whoever
	// changed it. Payload: TargetView. It also fires with Selected false when
	// the target is cleared.
	EventTargetChanged = "target:changed"

	// EventCatalogStarted fires once when a catalogue build begins.
	// Payload: CatalogStatus.
	EventCatalogStarted = "catalog:started"
	// EventCatalogProgress fires repeatedly during a catalogue build. It is
	// rate-limited by the emitter, not by the frontend.
	// Payload: CatalogProgress.
	EventCatalogProgress = "catalog:progress"
	// EventCatalogFinished fires exactly once per build, including cancelled
	// and failed ones. Payload: CatalogFinished.
	EventCatalogFinished = "catalog:finished"

	// EventSelectionChanged fires whenever the tray changes.
	// Payload: SelectionSummary.
	EventSelectionChanged = "selection:changed"

	// EventBuildStarted fires once when a bundle build begins.
	// Payload: BuildStarted.
	EventBuildStarted = "build:started"
	// EventBuildProgress fires as the build's phase or counters move.
	// Payload: BuildProgress.
	EventBuildProgress = "build:progress"
	// EventBuildEvent fires once per raw debark NDJSON event, for the
	// details drawer. Payload: BuildEvent. A build can emit thousands of
	// these; the frontend must not render them unless the drawer is open, and
	// may instead page BuildLog when it is.
	EventBuildEvent = "build:event"
	// EventBuildFinished fires exactly once per build, including cancelled
	// and failed ones. Payload: BuildFinished.
	EventBuildFinished = "build:finished"

	// EventExportStarted fires once when a copy begins. Payload: ExportStatus.
	EventExportStarted = "export:started"
	// EventExportProgress fires as bytes move. Payload: ExportProgress.
	EventExportProgress = "export:progress"
	// EventExportFinished fires exactly once per copy, including cancelled and
	// failed ones. Payload: ExportFinished.
	EventExportFinished = "export:finished"

	// EventVerifyStarted fires once when a verification pass begins.
	// Payload: VerifyStatus.
	EventVerifyStarted = "verify:started"
	// EventVerifyFinished fires exactly once per pass, including cancelled and
	// failed ones. Payload: VerifyFinished.
	EventVerifyFinished = "verify:finished"

	// EventAppError fires for a failure that belongs to no call the frontend
	// made — a background goroutine that died, a subsystem that fell over
	// between screens. Payload: UIError. The shell renders it as a banner.
	// It is deliberately rare: an error caused by a method call is returned
	// by that method, not emitted.
	EventAppError = "app:error"
)

// EventNames returns every event name this application may emit, in the order
// docs/dev/binding-surface.md lists them.
//
// It exists so a test can assert that the document and the code agree, and so
// a debug panel can subscribe to everything without a hand-maintained list.
func EventNames() []string {
	return []string{
		EventAppLifecycle,
		EventReadinessStarted,
		EventReadinessProgress,
		EventReadinessFinished,
		EventTargetChanged,
		EventCatalogStarted,
		EventCatalogProgress,
		EventCatalogFinished,
		EventSelectionChanged,
		EventBuildStarted,
		EventBuildProgress,
		EventBuildEvent,
		EventBuildFinished,
		EventExportStarted,
		EventExportProgress,
		EventExportFinished,
		EventVerifyStarted,
		EventVerifyFinished,
		EventAppError,
	}
}

// ---------------------------------------------------------------------------
// The emitter.
// ---------------------------------------------------------------------------

// Emitter delivers one event to the frontend.
//
// It is injectable so the whole application can run headless: the Wails
// runtime's EventsEmit calls log.Fatalf when handed a context that is not the
// one from a lifecycle hook, so a test, a fake-backed harness or a CLI smoke
// run would not merely fail to emit — it would kill the process. Deps.Emit
// replaces it; App.Startup installs the real one.
type Emitter func(name string, payload any)

// emit delivers one event, or drops it.
//
// Dropping is correct rather than lax. Events are a notification channel, not
// a source of truth: every long-running operation on this surface also has a
// status method precisely so that a screen which missed an event can recover
// the state. Nothing may block on an emit and nothing may fail because of one.
func (a *App) emit(name string, payload any) {
	a.mu.RLock()
	fn := a.emitter
	a.mu.RUnlock()
	if fn == nil {
		a.droppedEvents.Add(1)
		return
	}
	fn(name, payload)
}

// wailsEmitter returns an Emitter bound to a live Wails context. It is only
// ever built from the context handed to Startup.
func wailsEmitter(ctx wailsContext) Emitter {
	return func(name string, payload any) {
		wruntime.EventsEmit(ctx.ctx, name, payload)
	}
}

// droppedEventCount reports how many events were emitted with no runtime
// attached. Unexported on purpose: it is a diagnostic for the headless test
// harness, and every exported method on App becomes part of the frozen
// frontend surface.
func (a *App) droppedEventCount() int64 { return a.droppedEvents.Load() }

// ---------------------------------------------------------------------------
// Typed helpers. Every emit in this package goes through one of these.
// ---------------------------------------------------------------------------

func (a *App) emitReadinessStarted(r ReadinessReport) { a.emit(EventReadinessStarted, r) }
func (a *App) emitReadinessProgress(p ReadinessProgress) {
	a.emit(EventReadinessProgress, p)
}
func (a *App) emitReadinessFinished(f ReadinessFinished) { a.emit(EventReadinessFinished, f) }

func (a *App) emitTargetChanged(t TargetView) { a.emit(EventTargetChanged, t) }

func (a *App) emitCatalogStarted(s CatalogStatus)    { a.emit(EventCatalogStarted, s) }
func (a *App) emitCatalogProgress(p CatalogProgress) { a.emit(EventCatalogProgress, p) }
func (a *App) emitCatalogFinished(f CatalogFinished) { a.emit(EventCatalogFinished, f) }

func (a *App) emitSelectionChanged(s SelectionSummary) {
	a.emit(EventSelectionChanged, s)
	a.emitLifecycle()
}

func (a *App) emitLifecycle() { a.emit(EventAppLifecycle, a.LifecycleStatus()) }

func (a *App) emitBuildStarted(s BuildStarted)   { a.emit(EventBuildStarted, s) }
func (a *App) emitBuildProgress(p BuildProgress) { a.emit(EventBuildProgress, p) }
func (a *App) emitBuildEvent(e BuildEvent)       { a.emit(EventBuildEvent, e) }
func (a *App) emitBuildFinished(f BuildFinished) { a.emit(EventBuildFinished, f) }

func (a *App) emitExportStarted(s ExportStatus)    { a.emit(EventExportStarted, s) }
func (a *App) emitExportProgress(p ExportProgress) { a.emit(EventExportProgress, p) }
func (a *App) emitExportFinished(f ExportFinished) { a.emit(EventExportFinished, f) }

func (a *App) emitVerifyStarted(s VerifyStatus)    { a.emit(EventVerifyStarted, s) }
func (a *App) emitVerifyFinished(f VerifyFinished) { a.emit(EventVerifyFinished, f) }

// emitAppError announces a failure that belongs to no call. Use it only for
// background failures; an error caused by a bound method is returned by that
// method in its result's Error field.
func (a *App) emitAppError(e *UIError) {
	if e == nil {
		return
	}
	a.emit(EventAppError, e)
}
