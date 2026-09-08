// Package app is the Wails binding surface: the exact set of Go methods the
// frontend may call, and the exact set of events the backend may emit.
//
// This file holds the *view types* — the only Go types that cross the bridge.
// Nothing here is a `core/` type from the debark module and nothing here is
// an `internal/catalog` or `internal/cliadapter` type. That is a deliberate
// serialisation boundary (contract brief, frozen contract 3): a refactor of a
// backend package must not be a frontend change, and a schema bump in the core
// repository must not silently rename a field the JavaScript reads.
//
// Rules that apply to every type in this file:
//
//   - Every field carries an explicit `json` tag. The tag is the contract; the
//     Go name is not. Renaming a Go field is free, renaming a tag is not.
//   - Every result type that can fail carries `Error *UIError`. Bound methods
//     do not return Go `error` — see bindings.go for why.
//   - A version string is *display only*. Nothing in this repository may
//     compare two of them (non-negotiable rule 1).
//   - No type here holds an unbounded slice of catalogue rows. Where a slice
//     could grow with the catalogue, the method that fills it clamps it and
//     says so via a `truncated` field.
package app

// LifecycleView is authoritative close/edit state. SelectionRevision tracks
// build inputs, including a changed digest on an existing URL.
type LifecycleView struct {
	BuildRunning      bool     `json:"build_running"`
	ExportRunning     bool     `json:"export_running"`
	Stopping          bool     `json:"stopping"`
	UnsavedSelection  bool     `json:"unsaved_selection"`
	TargetGeneration  uint64   `json:"target_generation"`
	SelectionRevision uint64   `json:"selection_revision"`
	Error             *UIError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Limits. These are part of the frozen contract: the frontend may rely on them
// and must not ask for more.
// ---------------------------------------------------------------------------

const (
	// SearchPageDefault is the page size used when SearchQuery.Limit is 0.
	SearchPageDefault = 50
	// SearchPageMax is the largest page SearchPackages will return. A larger
	// Limit is clamped and SearchResult.Truncated is set. It equals
	// catalog.MaxPageSize; bindings.go asserts that at construction time so
	// the two cannot drift apart silently.
	SearchPageMax = 500

	// PackageRowsMax is the largest batch PackageRows accepts. The selection
	// tray hydrates in batches; it does not ask for everything at once.
	PackageRowsMax = 500

	// SelectionPageMax is the largest page SelectionPage will return.
	SelectionPageMax = 500

	// SelectionKeysMax is the largest number of keys SelectionKeys will
	// return.
	//
	// It is not a page size: SelectionKeys is deliberately unpaged, and this
	// is the ceiling above which it stops answering the question at all. The
	// frozen contract forbids an unbounded slice of catalogue ROWS, and this
	// is not one — a key is one short string the operator themselves put in
	// the tray, so the bound is the operator's own list rather than the
	// archive's 70,000 packages. 50,000 keys is about a megabyte on the wire,
	// which is already far past any tray a person builds by hand; past it
	// SelectionKeysResult.Truncated is set and the UI shows the count instead
	// of a tick per row.
	SelectionKeysMax = 50000

	// AddPackagesMax is the largest batch AddPackages accepts.
	AddPackagesMax = 5000

	// PackageListMaxBytes is the largest paste-a-list payload accepted by
	// ParsePackageList and AddPackageList (4 MiB — far past any real list).
	PackageListMaxBytes = 4 << 20

	// BuildLogPageMax is the largest page BuildLog will return.
	BuildLogPageMax = 500
	// BuildLogRing is how many raw build events the backend retains for the
	// details drawer. Older events are dropped and counted in
	// BuildLogPage.Dropped; the full record lives in the bundle's
	// evidence.json, which is where an auditor looks.
	BuildLogRing = 5000

	// SummaryListMax caps the string lists in BuildSummary (warnings,
	// unresolved, fetch-failed). Beyond it BuildSummary.Truncated is set and
	// the operator is pointed at evidence.json.
	SummaryListMax = 500

	// ExportMismatchMax caps the per-file failures ExportStatus carries. A
	// drive that is failing tends to fail everywhere at once, and a list of
	// twenty thousand paths is not more useful than a list of five hundred
	// plus the true count. ExportStatus.MismatchCount is that count.
	ExportMismatchMax = 500
)

// ---------------------------------------------------------------------------
// Errors — the actionable-error contract.
// ---------------------------------------------------------------------------

// Stable error codes. The UI may branch on Code; it must never parse Message.
const (
	// ErrCodeNotImplemented marks a stub whose owning stub has not
	// landed yet. It is a real, renderable error, not a panic.
	ErrCodeNotImplemented = "app.not_implemented"
	// ErrCodeInvalidInput is a malformed or missing argument.
	ErrCodeInvalidInput = "app.invalid_input"
	// ErrCodeTooManyItems is a batch above one of the limits above.
	ErrCodeTooManyItems = "app.too_many_items"
	// ErrCodeBusy is "that operation is already running".
	ErrCodeBusy = "app.busy"
	// ErrCodeCancelled is the operator's own cancellation, surfaced where a
	// result is asked for after one.
	ErrCodeCancelled = "app.cancelled"
	// ErrCodeInternal is a bug in this application.
	ErrCodeInternal = "app.internal"

	// ErrCodeNoTarget means no target has been selected yet.
	ErrCodeNoTarget = "target.none"
	// ErrCodeTargetInvalid means the selection could not be resolved.
	ErrCodeTargetInvalid = "target.invalid"

	// ErrCodeCatalogNotReady means the catalogue for the current target has
	// not been built. The UI's move is to offer StartCatalogBuild.
	ErrCodeCatalogNotReady = "catalog.not_ready"
	// ErrCodeCatalogFailed means the catalogue build failed.
	ErrCodeCatalogFailed = "catalog.failed"

	// ErrCodeNoSelection means the build was asked for with an empty tray.
	ErrCodeNoSelection = "selection.empty"

	// ErrCodeCLI is a failure reported by the debark binary. ExitClass and
	// ExitCode are populated; Details carries the captured stderr.
	ErrCodeCLI = "cli.failed"
	// ErrCodeCLIMissing means the debark binary could not be located.
	ErrCodeCLIMissing = "cli.missing"

	// ErrCodeDialogCancelled means the operator dismissed a native dialog. It
	// is reported as FilePickResult.Cancelled rather than as an error; the
	// code exists for the cases where a caller passed a path that came from a
	// cancelled dialog.
	ErrCodeDialogCancelled = "dialog.cancelled"

	// ErrCodeNoRuntime means a method that needs the desktop runtime (a native
	// dialog, the clipboard) was called while running headless.
	ErrCodeNoRuntime = "app.no_runtime"

	// ErrCodeExportFailed and ErrCodeVerifyFailed are the export screen's own
	// failures (free space, a removed drive, a mismatched digest).
	ErrCodeExportFailed = "export.failed"
	ErrCodeVerifyFailed = "verify.failed"
)

// UIError is the only error shape that crosses the bridge.
//
// It exists because a raw exit code is not an error message. Every field below
// answers a question the operator will actually ask, in the order they ask it:
// what happened (Message), what do I do now (Hint), what exactly ran and what
// did it say (Command, Details), and where does this sit in debark's frozen
// taxonomy (ExitClass, ExitCode).
//
// "Every error path renders an actionable message; none surface a raw exit
// code alone" is a definition-of-done item for the whole project. This type is
// where it is enforced: a UIError with an empty Message is a bug, and Hint is
// expected on anything the operator can act on.
type UIError struct {
	// Code is a stable machine-readable identifier, e.g. "catalog.not_ready".
	// Branch on this; never on Message.
	Code string `json:"code"`
	// Message is one complete sentence in plain language, in the operator's
	// terms rather than the program's. It is what the banner shows.
	Message string `json:"message"`
	// Hint is the next action, as an imperative sentence: "Install docker or
	// podman, or choose the local backend." Empty only when there genuinely
	// is nothing to suggest.
	Hint string `json:"hint,omitempty"`
	// Details is the raw material for the collapsed details drawer: captured
	// stderr, a stack of wrapped errors, a parser's complaint. It may be long
	// and it may be ugly. It is never the primary message.
	Details string `json:"details,omitempty"`
	// Command is the argv that failed, when a subprocess was involved. The UI
	// shows it so the operator can run it themselves — rule 8.
	//
	// Redacted, like CommandPreview.Argv and for the same reason: this is the
	// content of the details drawer's copy button, and a string with a copy
	// button must not carry a password. Read CommandPreview's comment before
	// writing any caption next to this — in particular, a heading promising
	// the failure "verbatim" now overstates what this field holds. A build
	// error is the only argv the redaction touches; a readiness remedy, a
	// verify or a keygen passes through unchanged.
	Command []string `json:"command,omitempty"`
	// ExitClass is debark's frozen class name ("environment", "resolution",
	// "policy", ...) when the failure came from the binary. Empty otherwise.
	ExitClass string `json:"exit_class,omitempty"`
	// ExitCode is the process exit status when there was one, and null when
	// there was not. It is supporting evidence, never the message.
	ExitCode *int `json:"exit_code,omitempty"`
	// Retryable is true when running the same thing again could plausibly
	// succeed (a network blip, a busy drive) and false when it cannot (a bad
	// flag, a missing key). The UI uses it to decide whether to offer Retry.
	Retryable bool `json:"retryable"`
	// DocURL points at local documentation when there is a page that explains
	// this class of failure. Never a debark-operated endpoint; this is a
	// file:// or a distro documentation link, and it is optional.
	DocURL string `json:"doc_url,omitempty"`
}

// Result is the envelope for a method that has nothing to return but success
// or a renderable failure. Every fire-and-forget starter returns one.
type Result struct {
	// OK is true when the call was accepted. For a fire-and-forget starter it
	// means "the work has begun", not "the work succeeded" — the outcome
	// arrives as the matching noun:finished event.
	OK bool `json:"ok"`
	// Error is set when OK is false, and is nil when OK is true.
	Error *UIError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Application identity.
// ---------------------------------------------------------------------------

// AppInfo is what the About panel and the shell's footer show.
type AppInfo struct {
	Name string `json:"name"`
	// Version is this GUI's version, not the CLI's.
	Version string `json:"version"`
	// Platform and Arch are the *builder's* GOOS/GOARCH. They are not the
	// target's architecture; nothing in this app runs on the target.
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	// DebarkPath and DebarkVersion are filled in once the binary has been
	// probed, and are empty before that or when it could not be found.
	DebarkPath    string `json:"debark_path,omitempty"`
	DebarkVersion string `json:"debark_version,omitempty"`
	// ProgressEvents says whether the located binary can stream build
	// progress: it is debark's `--json-events` capability, reported here
	// because it changes what the build screen may draw.
	//
	// When it is false a build still runs and still finishes — the adapter
	// leaves the flag off rather than dying on a usage error — but NO
	// `build:progress` or `build:event` will arrive, the details drawer stays
	// empty, and BuildProgress.Phase never leaves "starting". Show an
	// indeterminate spinner and say the binary is too old to report progress.
	// Drawing a progress bar that will never move is the failure this field
	// exists to prevent.
	//
	// It is false before the binary has been probed and whenever probing
	// failed, which is the safe reading: a screen that assumed progress and
	// got none has nothing to fall back to.
	ProgressEvents bool `json:"progress_events"`
	// Headless is true when no Wails desktop runtime is attached — the mode
	// the fakes and the tests run in. When it is true, the native dialog and
	// clipboard methods return ErrCodeNoRuntime and events are dropped.
	Headless bool `json:"headless"`
	// Error is set when probing the binary failed. AppInfo is still returned:
	// the shell renders regardless.
	Error *UIError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Readiness — screen 1.
// ---------------------------------------------------------------------------

// ReadinessStatus is whether a check passed. It is deliberately separate from
// ReadinessSeverity, because "did this pass" and "how much does it matter that
// it did not" are different questions: collapsing them would make a passing
// check that would have been blocking indistinguishable from a failing one,
// and the screen could no longer show a green row for the thing that matters
// most.
type ReadinessStatus string

const (
	// ReadinessOK means the check found what it was looking for.
	ReadinessOK ReadinessStatus = "ok"
	// ReadinessProblem means it did not, and Remedy says what to do.
	ReadinessProblem ReadinessStatus = "problem"
	// ReadinessSkipped means the check did not apply on this platform or
	// could not run. Never a failure, never blocking.
	ReadinessSkipped ReadinessStatus = "skipped"
)

// ReadinessSeverity is what a failing check costs the operator.
//
// The three values are the whole point of the type: without them the only
// expressible outcome is "not ready", which is exactly the wall this screen
// exists to avoid.
type ReadinessSeverity string

const (
	// SeverityBlocking stops a build outright.
	SeverityBlocking ReadinessSeverity = "blocking"
	// SeverityDegraded narrows what is possible without stopping it. The
	// operator keeps working and may never reach the limit.
	SeverityDegraded ReadinessSeverity = "degraded"
	// SeverityInfo is worth knowing and costs nothing today. A passing check
	// always carries it, and so does a missing signing key: not having one is
	// a choice debark supports, not a fault.
	SeverityInfo ReadinessSeverity = "info"
)

// Well-known readiness check ids. They mirror internal/readiness's own
// constants one for one, and they are stable: the frontend keys its copy off
// them, so they outlive any rewording.
const (
	CheckBinary           = "debark-binary"
	CheckAPT              = "apt"
	CheckContainer        = "container"
	CheckWSL              = "wsl"
	CheckSelfBinary       = "self-binary"
	CheckBuildEnvironment = "build-environment"
	CheckSigningKey       = "signing-key"
	CheckDiskSpace        = "disk-space"
	CheckArchiveNetwork   = "archive-network"
)

// ReadinessAction is the concrete remedy a row can offer as a button. It is
// data, not behaviour: it says what would be run, and the operator decides.
type ReadinessAction struct {
	// Label is the button text, imperative and short.
	Label string `json:"label"`
	// Command is the exact argv, already split. It is not a shell string: no
	// pipes, no globs, no variables anyone is expected to expand. Rule 8 —
	// the UI shows it before the operator agrees to it.
	Command []string `json:"command"`
	// Display is Command rendered the way a person would type it, for showing
	// and for copy-to-clipboard. Never parse it back.
	Display string `json:"display"`
	// Elevated is true when the command needs root or Administrator.
	Elevated bool `json:"elevated"`
	// Runnable is true when RunReadinessAction will actually run this. It is
	// false for every elevated action: this application does not escalate
	// privilege, and an operator who wants an elevated fix copies the command
	// into a shell where they can see what it does. That is a deliberate
	// security posture, not a missing feature — a desktop app that silently
	// acquires root to "help" is the thing this audience will not install.
	Runnable bool `json:"runnable"`
	// Note is the sentence to show beside the command when running it is not
	// the whole story — "log out and back in before this takes effect".
	Note string `json:"note,omitempty"`
}

// ReadinessCheck is one row on the readiness screen.
//
// Summary and Remedy are two sentences on purpose. Summary states what is true
// ("Docker is installed but its daemon is not running"); Remedy states what
// the operator does about it ("Start Docker, then run this check again"). A
// row that merges them states neither clearly, and a row with a Summary and no
// Remedy is the wall.
type ReadinessCheck struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Status   ReadinessStatus   `json:"status"`
	Severity ReadinessSeverity `json:"severity"`
	// Summary is one sentence saying what is true. Always set.
	Summary string `json:"summary"`
	// Remedy is one sentence saying what to do next. Always set when Status
	// is "problem"; that invariant is what makes the screen actionable.
	Remedy string `json:"remedy,omitempty"`
	// Action is the concrete command that would fix this, when one exists.
	Action *ReadinessAction `json:"action,omitempty"`
	// Detail is raw evidence for a details drawer. Never required reading —
	// the row must make sense without it.
	Detail string `json:"detail,omitempty"`
	// DurationMS is how long this check took, for the cold-start budget.
	DurationMS int64 `json:"duration_ms"`
	// Running is true while this one row is being re-checked, so the UI can
	// spin that row rather than the whole screen.
	Running bool `json:"running"`
	// Derived is true for a row computed from the other rows rather than
	// probed. RecheckReadiness refuses one, because there is nothing to run.
	//
	// Exactly one row is derived today: "build-environment", the answer to "is
	// there any way at all to run apt on this machine", read off the apt, WSL
	// and container rows. It is also one of only two BLOCKING rows, so a UI
	// that offered "check again" on it offered the button that matters most on
	// the one row where it does nothing. Nothing on the type said so, and the
	// only way to find out was to call and read the error.
	//
	// A derived row still follows its inputs: re-check "container" and this
	// row is recomputed with it, in the same readiness:finished payload. Point
	// the operator at the input rows instead of at this one.
	Derived bool `json:"derived"`
}

// Reasons a RemoteBuilderAdvice applies. Branch on Reason, never on Message.
const (
	// RemoteBuilderNoEnvironment means the derived build-environment row is a
	// problem: there is no way at all to run apt on this machine.
	RemoteBuilderNoEnvironment = "no-build-environment"
	// RemoteBuilderLocalBlocked means every local route this platform offers
	// is unavailable — on Windows, both WSL and a container runtime. The
	// build-environment row may not have said so yet, but the operator is in
	// the same position.
	RemoteBuilderLocalBlocked = "local-options-blocked"
)

// RemoteBuilderMessage and RemoteBuilderHint are what the readiness screen says
// when building on another machine is the way forward.
//
// They live in the contract, beside BaseCaveat, because they are a claim about
// what the product supports rather than a piece of copy: a bundle is an
// ordinary folder, so building it elsewhere and carrying it back is a
// first-class way to use this tool and not a workaround.
const (
	RemoteBuilderMessage = "This machine has no way to run apt, so a bundle cannot be built on it."
	RemoteBuilderHint    = "Build on a Linux machine you can reach and copy the bundle back. A bundle is an ordinary folder, so this is fully supported — and on a managed laptop, where WSL and container runtimes are both often blocked by policy, it is the only route that will ever work."
)

// RemoteBuilderAdvice is "build somewhere else", as data rather than as prose
// buried in one row's Remedy.
//
// It exists because the advice deserves more weight than a line inside a red
// row, and because the alternative is what the screen was doing: pattern
// matching on Platform plus a set of check ids to work out for itself whether
// the sentence applied. That is a decision, it lives on this side of the
// bridge, and a rewording of one Remedy would have silently changed when the
// panel appeared.
type RemoteBuilderAdvice struct {
	// Reason is why this applies: RemoteBuilderNoEnvironment or
	// RemoteBuilderLocalBlocked.
	Reason string `json:"reason"`
	// Message is one sentence saying what is true, and Hint is what to do.
	Message string `json:"message"`
	Hint    string `json:"hint"`
	// Blocked names the check ids that led here, so the panel can point at the
	// rows rather than repeating them. It may be empty.
	Blocked []string `json:"blocked,omitempty"`
}

// ReadinessReport is the whole screen.
type ReadinessReport struct {
	// Checks are the rows, blockers first and nice-to-knows last. The order
	// comes from the readiness package and does not change between runs, so
	// the list never reshuffles under the operator.
	Checks []ReadinessCheck `json:"checks"`
	// CanBuild is the single question the shell asks: may the operator
	// proceed? It is true when nothing is blocking.
	CanBuild bool `json:"can_build"`
	// BlockingCount and DegradedCount drive the summary line.
	BlockingCount int `json:"blocking_count"`
	DegradedCount int `json:"degraded_count"`
	// Platform is GOOS/GOARCH. Half the remedies differ by it, and a bug
	// report needs it.
	Platform string `json:"platform,omitempty"`
	// Checking is true while a full pass is in flight.
	Checking bool `json:"checking"`
	// RunningCheck is the id of the single check or action running right now,
	// empty during a full pass or when idle.
	RunningCheck string `json:"running_check,omitempty"`
	// CheckedAt is RFC 3339 UTC of the last full pass, empty before the first.
	CheckedAt string `json:"checked_at,omitempty"`
	// DurationMS is the wall time of the last full pass.
	DurationMS int64 `json:"duration_ms"`
	// RemoteBuilder is set when building on another machine is the way
	// forward, and nil otherwise. Show it above the rows when it is there: on
	// a machine where nothing local can run apt it is the only advice that
	// helps, and burying it under "install WSL" wastes it.
	RemoteBuilder *RemoteBuilderAdvice `json:"remote_builder,omitempty"`
	Error         *UIError             `json:"error,omitempty"`
}

// ReadinessProgress is the `readiness:progress` payload.
type ReadinessProgress struct {
	// CheckID is the check being evaluated or acted on.
	CheckID string `json:"check_id,omitempty"`
	Title   string `json:"title,omitempty"`
	// Action is true when this progress belongs to a RunReadinessAction call
	// rather than to a check pass.
	Action  bool   `json:"action"`
	Message string `json:"message,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
}

// ReadinessFinished is the `readiness:finished` payload.
type ReadinessFinished struct {
	OK        bool `json:"ok"`
	Cancelled bool `json:"cancelled"`
	// CheckID is set when this finish belongs to a single re-check or an
	// action, and empty after a full pass.
	CheckID string `json:"check_id,omitempty"`
	Action  bool   `json:"action"`
	// Report is the refreshed report, always present, so the screen can
	// re-render from one payload without a follow-up call.
	Report ReadinessReport `json:"report"`
	Error  *UIError        `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Target selection — screen 2.
// ---------------------------------------------------------------------------

// TargetKind distinguishes the two ways an operator names a target.
type TargetKind string

const (
	// TargetKindBase is a stock base compiled into the debark binary. It is
	// an assumption about a machine, not a measurement of one.
	TargetKindBase TargetKind = "base"
	// TargetKindSnapshot is a snapshot file the operator picked from disk,
	// measured on a real machine. Nothing is uploaded anywhere.
	TargetKindSnapshot TargetKind = "snapshot"
)

// BaseCaveat is the sentence the target screen must show beside every stock
// base. It is here, in the contract, rather than in the frontend, because it
// is a claim about the product and not a piece of copy.
const BaseCaveat = "A stock base is an assumption about a default install, not a measurement of the machine you are building for. " +
	"If the target has packages a default install does not, the bundle may be short. " +
	"Take a snapshot on the real machine when you can."

// SynthesizedSnapshotCaveat is the sentence shown for a snapshot that was
// itself synthesized from a stock base rather than captured on a machine.
//
// It exists separately from BaseCaveat because the operator's situation is
// different and more easily missed: they chose a *snapshot file*, which is the
// path the product describes as the measured one, and nothing about holding a
// file says whether a machine was ever involved. `snapshot from-base` writes a
// perfectly ordinary snapshot that records its own origin, and that record is
// the only thing distinguishing the two.
//
// Like BaseCaveat this lives in the contract rather than the frontend: it is a
// claim about what the product knows, not a piece of copy.
const SynthesizedSnapshotCaveat = "This snapshot was synthesized from a stock base, not captured on a machine. " +
	"It carries the same assumption a base does: if the real target has packages the base did not assume, the bundle may be short. " +
	"Take a snapshot on the real machine when you can."

// BaseView is one row in the stock-base picker: a projection of debark's
// `snapshot list-bases --json` entry, not that type itself.
type BaseView struct {
	// ID is "<distro>:<version>/<variant>", e.g. "ubuntu:24.04/desktop".
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	DistroID    string `json:"distro_id"`
	VersionID   string `json:"version_id"`
	Codename    string `json:"codename"`
	Variant     string `json:"variant,omitempty"`
	Arch        string `json:"arch"`
	// Seeds are the metapackages whose closure stands for a stock install.
	// Shown so the operator can see what the base claims. Display only.
	Seeds    []string `json:"seeds,omitempty"`
	Excludes []string `json:"excludes,omitempty"`
	// Recommends reports APT::Install-Recommends for the seed resolution.
	Recommends bool `json:"recommends"`
	// Digest distinguishes two builds claiming the same id.
	Digest string `json:"digest"`
}

// BasesResult is what the stock-base picker renders.
type BasesResult struct {
	Arch  string     `json:"arch"`
	Bases []BaseView `json:"bases"`
	// Caveat is BaseCaveat, repeated here so a frontend that only reads this
	// object still shows it.
	Caveat string   `json:"caveat"`
	Error  *UIError `json:"error,omitempty"`
}

// ArchesResult lists the architectures the base picker may offer.
type ArchesResult struct {
	Arches []string `json:"arches"`
	// Default is the builder's own architecture when it is in the list.
	Default string   `json:"default"`
	Error   *UIError `json:"error,omitempty"`
}

// SnapshotView is a projection of `snapshot inspect --json` — enough to show
// the operator what they picked and let them confirm it is the right machine.
type SnapshotView struct {
	// Path is the local file the operator chose. It never leaves this machine.
	Path          string `json:"path"`
	SchemaVersion string `json:"schema_version,omitempty"`
	DistroID      string `json:"distro_id"`
	VersionID     string `json:"version_id"`
	Codename      string `json:"codename"`
	Variant       string `json:"variant,omitempty"`
	Arch          string `json:"arch"`
	// OriginKind is "captured" for a snapshot taken on a real machine and
	// "synthesized" for one produced from a base definition. The UI says so:
	// the distinction is the difference between a fact and an assumption.
	//
	// The values are debark's own (snapshot.OriginCaptured /
	// OriginSynthesized) and pass through verbatim — this is not a vocabulary
	// of ours. An earlier version of this comment said "measured", which no
	// producer ever emits; a frontend written against it would have treated
	// every captured snapshot as unrecognised.
	OriginKind string `json:"origin_kind,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	// PackageCount is how many packages the snapshot says are installed.
	PackageCount int `json:"package_count"`
	// ForeignArchs are the extra dpkg architectures the target has enabled —
	// "i386" beside "amd64", most often. It matters on the confirm-this-is-the-
	// right-machine screen because the catalogue browses Arch and only Arch: a
	// target with foreign architectures can install things this application
	// will never show, and the operator reaches those by naming them.
	ForeignArchs []string `json:"foreign_archs,omitempty"`

	// BaseID and OriginSource describe where a SYNTHESIZED snapshot came from,
	// and are empty for a captured one. That emptiness is itself the signal —
	// there is nothing else on this type that distinguishes an assumption from
	// a measurement except OriginKind, and these say which assumption.
	//
	// BaseID is the base definition's id, "ubuntu:26.04/desktop".
	BaseID string `json:"base_id,omitempty"`
	// OriginSource is where that definition came from: "builtin" for one
	// compiled into the binary, or the base name of the operator's own file.
	// Never an absolute path — debark does not put the layout of the machine
	// that ran from-base into a snapshot that travels.
	OriginSource string `json:"origin_source,omitempty"`
	// OriginSourceDigest tells two snapshots claiming the same BaseID apart
	// when the definition itself changed underneath them.
	//
	// It is EMPTY for every captured snapshot, which is why it is no longer
	// called "digest": that name promised an identity for the snapshot itself,
	// and a screen written against it would have rendered a blank field for
	// exactly the snapshots the product tells operators to prefer. It is the
	// base definition's digest or nothing.
	OriginSourceDigest string `json:"origin_source_digest,omitempty"`
}

// SnapshotResult wraps SnapshotView with the error contract.
type SnapshotResult struct {
	Snapshot SnapshotView `json:"snapshot"`
	Error    *UIError     `json:"error,omitempty"`
}

// TargetSelection is the argument to SelectTarget: exactly one of the two
// kinds, with the fields that kind needs.
type TargetSelection struct {
	Kind TargetKind `json:"kind"`
	// BaseID and Arch are required when Kind is "base".
	BaseID string `json:"base_id,omitempty"`
	Arch   string `json:"arch,omitempty"`
	// SnapshotPath is required when Kind is "snapshot".
	SnapshotPath string `json:"snapshot_path,omitempty"`
}

// TargetView is the selected target as every other screen sees it.
type TargetView struct {
	Generation uint64 `json:"generation"`
	// Selected is false for the zero value, which is what CurrentTarget
	// returns before the operator has chosen anything.
	Selected bool       `json:"selected"`
	Kind     TargetKind `json:"kind,omitempty"`
	// ID is the base id or the snapshot's identity string; it is also the key
	// the catalogue cache is stored under.
	ID string `json:"id,omitempty"`
	// Label is the display string: "Ubuntu 24.04 desktop (amd64)".
	Label     string `json:"label,omitempty"`
	DistroID  string `json:"distro_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Codename  string `json:"codename,omitempty"`
	Variant   string `json:"variant,omitempty"`
	Arch      string `json:"arch,omitempty"`
	// SnapshotPath is set only for Kind "snapshot".
	SnapshotPath string `json:"snapshot_path,omitempty"`
	// Assumed is true for a stock base and for a synthesized snapshot: the
	// target's contents are inferred rather than measured. The UI shows the
	// caveat whenever this is true.
	Assumed bool   `json:"assumed"`
	Caveat  string `json:"caveat,omitempty"`
	// Components and Suites are what the catalogue will actually cover, in the
	// order the target's own sources list them, deduplicated.
	//
	// They live here rather than on SnapshotView because here is the only
	// place they exist. A snapshot captures its apt sources as FILES, so
	// `snapshot inspect --json` has nothing parsed to report; the stanzas are
	// parsed during target resolution, which is what produces this view.
	//
	// They are worth showing. "universe" present or absent is the difference
	// between 70,000 rows and 25,000, and an operator who cannot find a
	// package is usually looking at a target whose sources do not carry it —
	// which the picker cannot say, because a package that is not in the index
	// is indistinguishable from one that does not exist.
	Components []string `json:"components,omitempty"`
	Suites     []string `json:"suites,omitempty"`
	// CatalogReady is true when a usable catalogue cache exists for this
	// target, so the shell can go straight to the picker.
	CatalogReady bool `json:"catalog_ready"`
}

// TargetResult wraps TargetView with the error contract.
type TargetResult struct {
	Target TargetView `json:"target"`
	Error  *UIError   `json:"error,omitempty"`
	// SourceProblems is every apt source the resolver could not use.
	//
	// Present on success as well as failure, and that is the whole reason it
	// exists: a source that could not be read is a whole archive absent from
	// the catalogue, and the only symptom is a picker with fewer rows in it.
	// Of everything that can go wrong here it is the one an operator has no
	// way to notice, so it is the one thing that must always be said out loud.
	SourceProblems []SourceProblemView `json:"source_problems,omitempty"`
}

// SourceProblemView is one apt source that did not become part of the
// catalogue, and why.
type SourceProblemView struct {
	// Kind is the machine-readable reason, from catalog.SourceProblem.
	Kind string `json:"kind"`
	// Deliberate separates a documented scope limit from something wrong.
	//
	// Render the two differently. A `deb-src` line, a flat vendor repository,
	// a source for another architecture and a stanza the operator disabled are
	// all skipped on purpose and are worth a quiet line at most. A malformed
	// or unreadable source is a defect and deserves attention: it means the
	// catalogue is short by an archive nobody chose to leave out.
	Deliberate bool `json:"deliberate"`
	// File is the sources file as the target itself names it, e.g.
	// "/etc/apt/sources.list.d/ubuntu.sources".
	File string `json:"file,omitempty"`
	// Line is 1-based, and 0 when the problem is not tied to one line.
	Line int `json:"line,omitempty"`
	// Reason is one plain sentence, already written for a person.
	Reason string `json:"reason"`
	// Text is the offending source text, clipped, with control characters
	// flattened. Safe to render, but it is operator data: escape it.
	Text string `json:"text,omitempty"`
}

// ---------------------------------------------------------------------------
// Catalogue — screen 3's data source.
// ---------------------------------------------------------------------------

// CatalogPhase is which stage of a catalogue build is running. The values
// mirror internal/catalog's own phases one for one, because inventing a second
// vocabulary for the same seven steps would only create a translation table
// that goes stale.
type CatalogPhase string

const (
	CatalogPhaseResolve  CatalogPhase = "resolve"
	CatalogPhaseRelease  CatalogPhase = "release"
	CatalogPhaseDownload CatalogPhase = "download"
	CatalogPhaseParse    CatalogPhase = "parse"
	CatalogPhaseIndex    CatalogPhase = "index"
	CatalogPhaseSave     CatalogPhase = "save"
	CatalogPhaseDone     CatalogPhase = "done"
)

// CatalogState is the catalogue's condition, as distinct from the phase of a
// build that may or may not be running. A screen renders the state; a progress
// bar renders the phase.
type CatalogState string

const (
	// CatalogStateNone means no target is selected.
	CatalogStateNone CatalogState = "none"
	// CatalogStateNotBuilt is the ordinary first-run state: a target is
	// chosen and there is no cache for it. The UI shows a "build the
	// catalogue" call to action, not an error.
	CatalogStateNotBuilt CatalogState = "not-built"
	// CatalogStateBuilding means a build is in flight.
	CatalogStateBuilding CatalogState = "building"
	// CatalogStateReady means Search may be called.
	CatalogStateReady CatalogState = "ready"
	// CatalogStateFailed means the last build failed. Status.Error says why.
	CatalogStateFailed CatalogState = "failed"
	// CatalogStateCancelled means the operator cancelled the last build.
	CatalogStateCancelled CatalogState = "cancelled"
)

// CatalogProgress is the `catalog:progress` payload — a projection of
// internal/catalog's Progress, with its two derived fractions precomputed so
// the frontend does no arithmetic it could get wrong.
type CatalogProgress struct {
	Generation uint64       `json:"generation"`
	Phase      CatalogPhase `json:"phase"`
	// PhaseIndex and PhaseCount place the phase in the whole job.
	PhaseIndex int `json:"phase_index"`
	PhaseCount int `json:"phase_count"`
	// Label is the human sentence for this instant, present tense, and never
	// empty while a build is running.
	Label string `json:"label"`
	// Item is the specific thing being worked on — an index file path — or
	// empty. Shown under Label; never the only thing shown, because a path is
	// not an explanation.
	Item string `json:"item,omitempty"`
	// Current and Total count items within the phase. Total 0 means the count
	// is not yet known, which makes the phase indeterminate.
	Current int64 `json:"current"`
	Total   int64 `json:"total"`
	// BytesDone and BytesTotal are the download phase's counters and are zero
	// elsewhere. BytesTotal 0 means the mirror offered no Content-Length,
	// which is common; render it without breaking.
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
	// Fraction is progress within the phase, 0..1, or -1 when indeterminate.
	// The -1 is explicit so the UI can pick an indeterminate bar rather than
	// drawing an empty one that never moves.
	Fraction float64 `json:"fraction"`
	// OverallFraction is progress across the whole build, 0..1, or -1. Phases
	// are weighted equally: the bar always moves forwards and reaches the end
	// exactly when the build does.
	OverallFraction float64 `json:"overall_fraction"`
	// ElapsedMS is milliseconds since the build started.
	ElapsedMS int64 `json:"elapsed_ms"`
}

// CatalogStatus is the catalogue's whole state, readable at any time.
//
// It exists because events are not a source of truth: a screen that mounts
// late, a drawer that reopens, or a reloaded window has missed everything
// emitted so far. Every long-running operation on this surface therefore has
// a status method as well as its events.
type CatalogStatus struct {
	Generation uint64       `json:"generation"`
	State      CatalogState `json:"state"`
	// TargetID is the target this status describes, empty when none is
	// selected.
	TargetID string `json:"target_id,omitempty"`
	// Ready is true when Search may be called. It is State == "ready",
	// restated as a boolean because that is the question every screen asks.
	Ready bool `json:"ready"`
	// Building is true while a build is in flight.
	Building bool `json:"building"`
	// Stale is true when a usable cache exists but the archive has published
	// new indexes since it was written. The catalogue stays usable and the UI
	// offers a refresh rather than blocking on one: nothing incorrect can
	// follow from browsing a stale catalogue, because the build re-resolves
	// against the live archive regardless of what was on screen.
	Stale    bool            `json:"stale"`
	Progress CatalogProgress `json:"progress"`
	// PackageCount is the size of the built catalogue, 0 when not ready.
	PackageCount int `json:"package_count"`
	// BuiltAt is RFC 3339 UTC of the cache entry, empty when not ready.
	BuiltAt string `json:"built_at,omitempty"`
	// FromCache is true when the current readiness came from disk rather than
	// from a build in this session.
	FromCache bool `json:"from_cache"`
	// Cancelled is true when the last build ended because the operator
	// cancelled it.
	Cancelled bool     `json:"cancelled"`
	Error     *UIError `json:"error,omitempty"`
}

// CatalogFinished is the `catalog:finished` payload.
type CatalogFinished struct {
	OK        bool          `json:"ok"`
	Cancelled bool          `json:"cancelled"`
	Status    CatalogStatus `json:"status"`
	// DurationMS is wall time for the build, for the performance record.
	DurationMS int64    `json:"duration_ms"`
	Error      *UIError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Packages — screen 3.
// ---------------------------------------------------------------------------

// PackageSource says where a row came from. Vendor `.deb`s appear as ordinary
// rows next to apt packages; this is the only thing that distinguishes them.
type PackageSource string

const (
	// SourceAPT is a package from the target's own apt indexes.
	SourceAPT PackageSource = "apt"
	// SourceURL is a vendor `.deb` the operator typed a URL for.
	SourceURL PackageSource = "url"
	// SourceFile is a local `.deb` the operator picked from disk.
	SourceFile PackageSource = "file"
)

// PackageRow is one row in the virtualised list.
//
// It is kept small on purpose: this is the type that crosses the bridge fifty
// at a time, sixty times a second while someone scrolls. Anything the row does
// not render belongs in PackageDetail instead.
type PackageRow struct {
	Name string `json:"name"`
	// DisplayName is the human application name from DEP-11 when there is one
	// ("Firefox Web Browser"), empty otherwise.
	DisplayName string `json:"display_name,omitempty"`
	// Version is a version string one of the target's indexes offers.
	//
	// DISPLAY ONLY, and not apt's candidate. Nothing in this repository may
	// compare two of these — not to sort, not to pick, not to decide
	// anything. What actually gets installed is decided by apt inside
	// debark at build time and may differ from this string.
	Version string `json:"version,omitempty"`
	// Suite is which suite supplied Version — "noble", "noble-updates" — so a
	// surprising version explains itself.
	Suite string `json:"suite,omitempty"`
	// Component is the archive component: "main", "universe", "non-free". It
	// is here because it has a consequence downstream — debark raises a
	// redistribution flag for the restricted components — so the picker can
	// warn before the build does.
	Component string `json:"component,omitempty"`
	Arch      string `json:"arch,omitempty"`
	// Section is the apt `Section:` field, the grouping for non-applications.
	Section string `json:"section,omitempty"`
	// Priority is apt's `Priority:` field. Context, never a ranking.
	Priority string `json:"priority,omitempty"`
	// Categories are freedesktop categories from DEP-11, for applications.
	Categories []string `json:"categories,omitempty"`
	// Summary is the one-line description: DEP-11's when it knows this
	// package, otherwise the first line of the index's Description.
	Summary string `json:"summary,omitempty"`
	// InstalledSizeBytes and DownloadSizeBytes are BYTES. The catalogue
	// reports installed size in KiB, as apt does; the conversion happens once,
	// here at the boundary, and the unit is in the field name because a factor
	// of 1024 in a "this bundle needs 3 GB" warning is the kind of bug that
	// ships. 0 means unknown.
	InstalledSizeBytes int64 `json:"installed_size_bytes"`
	DownloadSizeBytes  int64 `json:"download_size_bytes"`
	// IsApp is true when DEP-11 describes this package as a desktop
	// application. It is what turns 70,000 rows into the few hundred a
	// non-expert was looking for.
	IsApp bool `json:"is_app"`
	// Selected reflects the tray at the moment the page was produced. The
	// tray is authoritative; treat this as a hint that may be one frame stale.
	Selected bool          `json:"selected"`
	Source   PackageSource `json:"source"`
	// Warnings are selection-time warnings that apply to this package — the
	// snap-transitional warning, notably. They are attached to the row so the
	// operator sees them *before* selecting, not after building.
	Warnings []SelectionWarning `json:"warnings,omitempty"`
}

// PackageDetail is the expanded view of one package.
//
// It is a flat struct rather than an embedding of PackageRow, because Wails
// generates its JavaScript models by reflection and an anonymous embedded
// field does not reliably flatten. Duplication here buys a predictable shape
// on the other side of the bridge.
// It carries no dependency fields. That is deliberate and it is not an
// oversight in the catalogue: `Depends`, `Recommends` and `Provides` are the
// raw material of dependency reasoning, and the surest way to keep this
// application from growing a second resolver is to never carry them across the
// bridge in the first place. The authoritative answer to "what else will this
// pull in?" is the build itself, which is apt's answer, arriving as events.
type PackageDetail struct {
	Name                 string        `json:"name"`
	DisplayName          string        `json:"display_name,omitempty"`
	Version              string        `json:"version,omitempty"`
	Suite                string        `json:"suite,omitempty"`
	Component            string        `json:"component,omitempty"`
	Arch                 string        `json:"arch,omitempty"`
	Section              string        `json:"section,omitempty"`
	Priority             string        `json:"priority,omitempty"`
	Categories           []string      `json:"categories,omitempty"`
	Summary              string        `json:"summary,omitempty"`
	Description          string        `json:"description,omitempty"`
	DescriptionTruncated bool          `json:"description_truncated,omitempty"`
	InstalledSizeBytes   int64         `json:"installed_size_bytes"`
	DownloadSizeBytes    int64         `json:"download_size_bytes"`
	IsApp                bool          `json:"is_app"`
	Selected             bool          `json:"selected"`
	Source               PackageSource `json:"source"`
	// Homepage is the upstream URL from the index, when it has one.
	Homepage string `json:"homepage,omitempty"`
	// URL and Path are set for SourceURL and SourceFile rows.
	URL      string             `json:"url,omitempty"`
	Path     string             `json:"path,omitempty"`
	Warnings []SelectionWarning `json:"warnings,omitempty"`
}

// PackageResult is GetPackage's return. Found is false when the name is not in
// the catalogue; that is not an error.
type PackageResult struct {
	Package PackageDetail `json:"package"`
	Found   bool          `json:"found"`
	Error   *UIError      `json:"error,omitempty"`
}

// PackageRowsResult is PackageRows' return: the rows that were found, and the
// names that were not.
type PackageRowsResult struct {
	Rows    []PackageRow `json:"rows"`
	Missing []string     `json:"missing,omitempty"`
	Error   *UIError     `json:"error,omitempty"`
}

// SearchQuery is one page request.
//
// Offset and Limit are the whole paging contract: there is no cursor and no
// server-side scroll state, because the virtualiser owns the scroll position
// and asks for whatever window it needs. Paging is stable as long as the
// catalogue does not change, so offsets may be requested in any order.
//
// There is no sort field. Ordering is part of the contract rather than the
// caller's choice — name matches before summary matches, then name ascending —
// so that every screen ranks results identically and the implementation is
// free to precompute exactly one order. There is no "selected only" filter
// either: the tray is its own list, read with SelectionPage.
type SearchQuery struct {
	// Text is the raw search box contents, matched case-insensitively as a
	// substring of the package name and of the summary. Empty returns the
	// whole catalogue, paged — the state the picker opens in. The frontend
	// does not pre-process it.
	Text string `json:"text"`
	// Category filters to one CategoryView.ID from Categories. The id is
	// tier-prefixed ("app:Graphics", "section:net"), which is what keeps a
	// freedesktop category and an apt section that share a word from
	// colliding. Empty means all. Never build one by concatenation; use the
	// id the Categories call returned.
	Category string `json:"category,omitempty"`
	// Offset is the first row wanted, zero-based. It may be large and it may
	// jump: it is whatever the scroll position implies.
	Offset int `json:"offset"`
	// Limit is how many rows are wanted. 0 means SearchPageDefault; anything
	// above SearchPageMax is clamped.
	Limit int `json:"limit"`
	// AppsOnly restricts results to DEP-11 desktop applications — the single
	// most useful filter in the product.
	AppsOnly bool `json:"apps_only,omitempty"`
}

// SearchResult is one page.
type SearchResult struct {
	Rows []PackageRow `json:"rows"`
	// Total is the number of rows the query matched, not the number returned.
	// The virtualiser sizes its scrollbar from this without ever receiving
	// the rows.
	Total  int `json:"total"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	// Query echoes the request. A debounced search box fires several requests
	// and they may complete out of order; comparing this against the current
	// input is how a stale page is discarded.
	Query SearchQuery `json:"query"`
	// TookMS is the backend's own time for this page, excluding the bridge.
	// It is what the < 100 ms budget is measured against.
	TookMS int64 `json:"took_ms"`
	// Truncated is true when Limit was clamped to SearchPageMax.
	Truncated bool     `json:"truncated"`
	Error     *UIError `json:"error,omitempty"`
}

// CategoryTier is which of the two groupings a category belongs to.
type CategoryTier string

const (
	// TierApplication is a freedesktop category from DEP-11: Graphics,
	// Development, Office, AudioVideo. Shown first, because it is what an
	// operator who does not know package names is looking for.
	TierApplication CategoryTier = "application"
	// TierSection is an apt `Section:` — net, devel, libs, python. The
	// complete grouping, for the operator who knows what they are doing.
	TierSection CategoryTier = "section"
)

// CategoryView is one row in the sidebar's two-tier grouping.
//
// Two tiers, not a tree: the list is flat and every entry names its tier. The
// sidebar renders the application tier first and the section tier below it.
type CategoryView struct {
	// ID is the value to put in SearchQuery.Category, tier-prefixed.
	ID   string       `json:"id"`
	Tier CategoryTier `json:"tier"`
	// Name is the label to render, as written in the index. Not localised:
	// freedesktop categories are defined in English and inventing
	// translations here would make them stop matching the filter.
	Name string `json:"name"`
	// Count is how many packages the catalogue holds in this category, before
	// any text or apps-only filter. It is what tells an operator that "Games"
	// is worth opening and "Localization" is not.
	Count int `json:"count"`
}

// CategoriesResult is the whole grouping. It is bounded by construction —
// there are tens of categories, not tens of thousands — so it is the one
// listing on this surface that is not paged.
type CategoriesResult struct {
	// Categories is every category, application tier first, then sections,
	// each tier ordered by Name ascending. The order does not change between
	// calls, so the sidebar does not reshuffle.
	Categories []CategoryView `json:"categories"`
	// Total is the catalogue's package count, for the "All packages" row.
	Total int      `json:"total"`
	Error *UIError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Selection tray — screen 3's right-hand side.
// ---------------------------------------------------------------------------

// Selection warning kinds. The frontend styles by Kind and prints Message.
const (
	// WarnSnapTransitional is the Ubuntu case that must be surfaced at
	// selection time: the archive package is a transitional stub that pulls
	// in a snap, so the bundle will not contain what the operator expects.
	WarnSnapTransitional = "snap-transitional"
	// WarnExternalURL marks a vendor `.deb` fetched from outside the target's
	// own archive.
	WarnExternalURL = "external-url"
	// WarnLargeDownload marks a selection that will make the bundle very big.
	WarnLargeDownload = "large-download"
	// WarnUnknownPackage marks a tray entry that is not in the catalogue.
	WarnUnknownPackage = "unknown-package"
	// WarnArchMismatch marks a local `.deb` whose architecture does not match
	// the target's.
	WarnArchMismatch = "arch-mismatch"
)

// SelectionWarning is one thing worth telling the operator about a choice,
// said at the moment they make it rather than at the end of a build.
type SelectionWarning struct {
	Kind string `json:"kind"`
	// Package is the entry the warning is about, empty for a warning about
	// the selection as a whole.
	Package string `json:"package,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	DocURL  string `json:"doc_url,omitempty"`
}

// RejectedInput is one thing the operator asked for that was not accepted,
// with the reason in the operator's terms.
type RejectedInput struct {
	Value  string `json:"value"`
	Reason string `json:"reason"`
	Hint   string `json:"hint,omitempty"`
}

// SelectionEntry is one row in the tray.
type SelectionEntry struct {
	// Key is the stable identity RemovePackages takes: package name for apt,
	// absolute path for files, and URL for vendor inputs. A credentialed URL
	// uses an opaque hash key so the original reference stays only in Go.
	Key         string        `json:"key"`
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name,omitempty"`
	Version     string        `json:"version,omitempty"`
	Summary     string        `json:"summary,omitempty"`
	Source      PackageSource `json:"source"`
	URL         string        `json:"url,omitempty"`
	Path        string        `json:"path,omitempty"`
	// InstalledSizeBytes and DownloadSizeBytes are bytes, 0 when unknown —
	// which is normal for a URL, because nothing here fetches it to find out.
	InstalledSizeBytes int64 `json:"installed_size_bytes"`
	DownloadSizeBytes  int64 `json:"download_size_bytes"`
	// Known is false for a name that is not in the catalogue. Such an entry
	// is kept, not dropped: the operator may know something the index does
	// not, and debark will give the authoritative answer at build time.
	Known bool `json:"known"`
	// SHA256 is the operator's expected digest for a URL row, lowercase hex,
	// empty when they did not supply one.
	//
	// It is not decoration. debark compares the fetched file against it and
	// fails the build rather than bundling something else, which upgrades that
	// input's recorded provenance from url-unverified to user-digest — the
	// difference between "we downloaded this from somewhere" and "the operator
	// said what this should be and it was". For an audience whose whole job is
	// deciding what may cross an air gap, that is the point of the field.
	//
	// It reaches the CLI as `--digest URL=SHA256`. Anything that carries a
	// digest into the tray must carry it through to BuildSpec as well: a UI
	// that accepts a digest and then builds without it is worse than one that
	// never asked, because the operator believes they attested something.
	SHA256   string             `json:"sha256,omitempty"`
	Warnings []SelectionWarning `json:"warnings,omitempty"`
}

// URLInput is a vendor .deb URL with the operator's optional expected digest.
//
// AddURLs takes these rather than bare strings so the digest has somewhere to
// live between the dialog that collects it and the build that enforces it.
type URLInput struct {
	URL string `json:"url"`
	// SHA256 is optional. Lowercase hex, 64 characters; a "sha256:" prefix and
	// surrounding whitespace are accepted and normalised away.
	SHA256 string `json:"sha256,omitempty"`
}

// SelectionSummary is the tray's header and the answer to "can I build yet?".
// It never carries the entries themselves; SelectionPage does that.
type SelectionSummary struct {
	UndoToken    string `json:"undo_token"`
	UndoLabel    string `json:"undo_label"`
	Total        int    `json:"total"`
	PackageCount int    `json:"package_count"`
	URLCount     int    `json:"url_count"`
	FileCount    int    `json:"file_count"`
	// UnknownCount is how many entries are not in the catalogue.
	UnknownCount int `json:"unknown_count"`
	// InstalledSizeBytes and DownloadSizeBytes are the sums over the entries
	// that report a size. They are estimates for display and are not promises
	// about the bundle, which will contain dependencies this application
	// never sees and must never reason about. The UI labels them as
	// estimates; a number presented as a total that then doubles is worse
	// than no number.
	InstalledSizeBytes int64 `json:"installed_size_bytes"`
	DownloadSizeBytes  int64 `json:"download_size_bytes"`
	// Warnings are the selection-wide warnings, deduplicated.
	Warnings []SelectionWarning `json:"warnings,omitempty"`
	// Rejected is populated by the mutating methods (AddPackages, AddURLs,
	// AddLocalDebs, AddPackageList) and is empty on a plain read.
	Rejected []RejectedInput `json:"rejected,omitempty"`
	// Added and Removed report what the call that produced this summary did.
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Revision is the identity of the current SET OF KEYS in the tray. It
	// increases whenever a key is added, removed or the tray is cleared, and
	// never decreases within one run of the application.
	//
	// It exists for the virtualised list. A recycled row has to know whether
	// the package it is about to draw is selected, and the authoritative
	// answer is the key set — so the list caches what SelectionKeys returned
	// and compares this number against SelectionKeysResult.Revision to decide
	// whether the cache is still good. Every `selection:changed` payload
	// carries it, so the list re-fetches once per real change instead of on
	// every event.
	//
	// It deliberately does NOT move when an entry's contents change without
	// its key changing — a tray row hydrating against a freshly built
	// catalogue, for instance. Those are not changes to the set of selected
	// packages, and a cached key set is still correct across them.
	Revision uint64   `json:"revision"`
	Error    *UIError `json:"error,omitempty"`
}

// SelectionKeysResult is SelectionKeys' return: every key in the tray and
// nothing else.
//
// It exists because the virtualiser asks "is this row selected?" sixty times a
// second while someone scrolls, and the only authoritative answer is the tray.
// Before it, the sole way to get the keys was to page the whole tray with
// SelectionPage — ceil(total/500) round trips, every one of them carrying
// versions, summaries, sizes and warnings the list does not draw — on every
// `selection:changed`. This carries only what the question needs.
type SelectionKeysResult struct {
	// Keys is every SelectionEntry.Key in the tray, in tray order. For an apt
	// row the key is the package name, which is what PackageRow.Name holds,
	// so marking a row is a set membership test and nothing more. For a URL
	// row it is the URL, and for a local file the absolute path.
	Keys []string `json:"keys"`
	// Total is how many entries the tray holds. It equals len(Keys) unless
	// Truncated is set, in which case it is the true count.
	Total int `json:"total"`
	// Revision matches SelectionSummary.Revision. Compare it against the
	// revision on the last `selection:changed` payload to know whether a
	// cached key set is current, and discard a response whose revision is
	// older than one already applied — several of these can be in flight at
	// once and they may complete out of order.
	Revision uint64 `json:"revision"`
	// Truncated is true when the tray holds more than SelectionKeysMax
	// entries. Keys is then empty rather than partial: half a key set would
	// silently draw selected rows as unselected, which is worse than
	// admitting the list cannot be marked. Show Total and drop the per-row
	// ticks.
	Truncated bool     `json:"truncated"`
	Error     *UIError `json:"error,omitempty"`
}

// SelectionPage is one page of tray entries.
type SelectionPage struct {
	Entries []SelectionEntry `json:"entries"`
	Total   int              `json:"total"`
	Offset  int              `json:"offset"`
	Limit   int              `json:"limit"`
	Error   *UIError         `json:"error,omitempty"`
}

// ParsedList is ParsePackageList's return: what a pasted blob would do,
// without doing it.
type ParsedList struct {
	// Count is how many distinct names the text yielded.
	Count int `json:"count"`
	// NewCount is how many of those are not already in the tray.
	NewCount int `json:"new_count"`
	// KnownCount is how many are in the catalogue.
	KnownCount int `json:"known_count"`
	// Unknown lists names not found in the catalogue, clamped to 100 entries.
	// UnknownCount is the true total.
	Unknown      []string `json:"unknown,omitempty"`
	UnknownCount int      `json:"unknown_count"`
	// Duplicates is how many lines named something already listed.
	Duplicates int `json:"duplicates"`
	// Sample is up to 50 hydrated rows for a preview table.
	Sample []PackageRow `json:"sample,omitempty"`
	// Rejected lists lines that were not names at all.
	Rejected []RejectedInput    `json:"rejected,omitempty"`
	Warnings []SelectionWarning `json:"warnings,omitempty"`
	Error    *UIError           `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Build — screen 4.
// ---------------------------------------------------------------------------

// BuildOptions is everything the build screen collects.
//
// The target and the selection are not in here: they are application state,
// set by SelectTarget and the tray methods, and the build uses whatever is
// current. That keeps the tray the single source of truth for what gets built
// and stops it disagreeing with what the operator is looking at.
//
// Every field below maps to exactly one debark flag. There is deliberately
// nothing here that the command line cannot express — rule 8, the CLI stays
// the complete interface — which is also why there is no "dry run" and no
// "recommends on": debark has neither flag.
type BuildOptions struct {
	// OutputDir is the directory the bundle is written into. Required.
	OutputDir string `json:"output_dir"`
	// OutputName is the bundle's own name inside OutputDir. Empty means the
	// backend names it from the target and the date.
	OutputName string `json:"output_name,omitempty"`
	// Format is "dir" or "tar"; empty means "dir". It decides whether
	// OutputDir/OutputName becomes --out or --tar.
	Format string `json:"format,omitempty"`

	// SignerRef is --sign: a private key file, "gpg:<keyid>" or
	// "plugin:<name>". Setting it makes signing REQUIRED — the build fails
	// rather than writing an unsigned bundle.
	SignerRef string `json:"signer_ref,omitempty"`
	// NoSign is --no-sign: write an unsigned bundle, explicitly. With neither
	// this nor SignerRef, debark uses a configured default key if there is
	// one and writes an unsigned bundle if there is not — and BuildSummary
	// then reports Signed false, which the UI must say out loud.
	NoSign bool `json:"no_sign,omitempty"`

	// SBOM is --sbom: also write sbom.cdx.json.
	SBOM bool `json:"sbom,omitempty"`
	// Recommends overrides the target's own Install-Recommends. Three states,
	// because the CLI has three:
	//
	//	null / absent   follow the target's apt.conf — no flag is passed
	//	false           --no-recommends: a smaller bundle
	//	true            --recommends: pull recommended packages in
	//
	// Send null, not false, for "let the target decide". They are different
	// builds: a target whose apt.conf turns recommends on gets them with null
	// and does not with false.
	Recommends *bool `json:"recommends,omitempty"`
	// NoRecommends is the older one-directional spelling of
	// Recommends=false, kept because the CLI only had --no-recommends when
	// this surface was frozen and the current frontend still sends it.
	//
	// Recommends wins whenever it is set, so the two cannot contradict each
	// other. Prefer Recommends; this field should go with the redesign.
	//
	// Deprecated: use Recommends.
	NoRecommends bool `json:"no_recommends,omitempty"`
	// Upgrades is --upgrades: add a full-upgrade pass for packages already on
	// the target.
	Upgrades bool `json:"upgrades,omitempty"`
	// Update is --update: refresh indexes, re-resolve, fetch newer versions,
	// then prune superseded files. Without it a build is additive.
	Update bool `json:"update,omitempty"`
	// NoPrune is --no-prune: with Update, keep superseded files. No effect
	// without Update.
	NoPrune bool `json:"no_prune,omitempty"`
	// Backend is --backend: "auto", "local" or "container". Empty means the
	// configured backend.
	Backend string `json:"backend,omitempty"`
	// AcknowledgeRedistribution is --acknowledge-redistribution.
	//
	// The UI must set it only after the operator has actually seen and
	// accepted the redistribution notice on screen. It is not a convenience
	// flag; it is the record that a person was asked.
	AcknowledgeRedistribution bool `json:"acknowledge_redistribution,omitempty"`
}

// BuildPhase is where a build has got to. These are the GUI's phases, mapped
// from debark's event stream; they are not debark's own event types.
type BuildPhase string

const (
	BuildPhaseIdle        BuildPhase = "idle"
	BuildPhaseStarting    BuildPhase = "starting"
	BuildPhaseResolving   BuildPhase = "resolving"
	BuildPhaseDownloading BuildPhase = "downloading"
	BuildPhaseAssembling  BuildPhase = "assembling"
	BuildPhaseSigning     BuildPhase = "signing"
	BuildPhaseVerifying   BuildPhase = "verifying"
	BuildPhaseFinished    BuildPhase = "finished"
	BuildPhaseFailed      BuildPhase = "failed"
	BuildPhaseCancelled   BuildPhase = "cancelled"
)

// BuildProgress is the `build:progress` payload.
//
// # Progress is indeterminate by default, and that is the contract
//
// A build screen must render correctly having received NOTHING but the phase.
// Three independent reasons it may:
//
//  1. The located debark has no --json-events, so there is no stream at all.
//     AppInfo.ProgressEvents says so before a build starts.
//  2. The CONTAINER backend emits about eleven events for a complete build.
//     No fetch.file, no progress, not even apt.resolve — the inner container
//     process's events do not cross back out. Measured on a real end-to-end
//     run; the core repository declined to forward the inner stream as part of
//     that work, so this is the shipping behaviour and not a bug to wait out.
//  3. On the local backend apt.resolve's selection count is an upper bound on
//     work rather than a file count, and arrives after apt has already
//     downloaded everything inside one blocking exec.
//
// So Fraction is -1 far more often than it is a number, Package is frequently
// empty, and a bar that only moves when counters do will sit still through an
// entire successful build. Drive the screen from Phase, and treat every
// counter as a bonus.
type BuildProgress struct {
	Phase   BuildPhase `json:"phase"`
	Message string     `json:"message,omitempty"`
	// Package is what is being worked on right now, when the phase has one.
	Package string `json:"package,omitempty"`
	// Current and Total count packages in the current phase; 0 total means
	// not yet known.
	Current int64 `json:"current"`
	Total   int64 `json:"total"`
	// BytesDone and BytesTotal describe the file named by Package — one file,
	// not the run. BytesTotal is 0 when not yet known and -1 when the archive
	// sent no Content-Length, which is common and is not an error. Both are
	// cleared when the download phase ends, so a finished build never reports
	// a stale mid-download count.
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
	// Fraction is 0..1, or -1 for indeterminate.
	//
	// It is -1 for the whole download phase, deliberately. Nothing on the wire
	// says how many files a build will fetch — `apt.resolve`'s selection count
	// is an upper bound on work, not a file count, because a package already
	// in the local store consumes a selection and downloads nothing — so there
	// is no denominator to divide by and any run percentage would be invented.
	// A per-file fraction is computable and belongs beside its filename, where
	// it cannot be misread as "the build is this far along".
	Fraction float64 `json:"fraction"`
}

// BuildStats mirrors the numbers debark reports, projected into the view
// layer so a schema bump in the core repository is not a frontend change.
type BuildStats struct {
	Added           int   `json:"added"`
	Removed         int   `json:"removed"`
	Unchanged       int   `json:"unchanged"`
	PackageCount    int   `json:"package_count"`
	Bytes           int64 `json:"bytes"`
	DownloadedBytes int64 `json:"downloaded_bytes"`
	DurationSeconds int   `json:"duration_seconds"`
}

// BuildSummary is the outcome of a finished build.
type BuildSummary struct {
	BundlePath  string `json:"bundle_path,omitempty"`
	BundleID    string `json:"bundle_id,omitempty"`
	LockRef     string `json:"lock_ref,omitempty"`
	ManifestRef string `json:"manifest_ref,omitempty"`
	// Signed is true when at least one signature block was written. When it
	// is false the UI must say the bundle is unsigned; silence is not an
	// option here.
	Signed bool       `json:"signed"`
	Stats  BuildStats `json:"stats"`
	// Warnings, Unresolved and FetchFailed are clamped to SummaryListMax.
	Warnings    []string `json:"warnings,omitempty"`
	Unresolved  []string `json:"unresolved,omitempty"`
	FetchFailed []string `json:"fetch_failed,omitempty"`
	// Truncated is true when any of the three lists above was clamped. The
	// complete record is in the bundle's evidence.json.
	Truncated bool `json:"truncated"`
	// ExitClass is debark's outcome class as a string: "success",
	// "incomplete", "resolution", and so on. "incomplete" in particular means
	// a bundle exists but is short — a case the UI must not present as
	// success.
	ExitClass string `json:"exit_class,omitempty"`
}

// BuildStatus is the build's whole state, readable at any time. Same reason as
// CatalogStatus: events are not a source of truth for a screen that mounts
// late or a window that reloaded.
type BuildStatus struct {
	TargetGeneration  uint64 `json:"target_generation"`
	SelectionRevision uint64 `json:"selection_revision"`
	Running           bool   `json:"running"`
	Finished          bool   `json:"finished"`
	Cancelled         bool   `json:"cancelled"`
	// Progress is the last progress reported. Its Phase is the state machine
	// the UI renders.
	Progress BuildProgress `json:"progress"`
	// Command is the command line this build ran, so the screen can show it
	// while the build is in flight and afterwards. Rule 8.
	//
	// Redacted, exactly as CommandPreview.Argv is and for the same reason:
	// this is the pair the build screen renders and its details drawer copies.
	// The build itself ran from the unredacted argv. Read CommandPreview's
	// comment before writing any caption next to this — a URL with a query
	// string loses the whole query string, secret or not.
	Command []string `json:"command,omitempty"`
	// TargetID and ItemCount describe what was built, for a header that
	// survives navigating away and back.
	TargetID   string `json:"target_id,omitempty"`
	ItemCount  int    `json:"item_count"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	// EventCount is how many raw events have been recorded, so the drawer can
	// show a badge without fetching them.
	EventCount int `json:"event_count"`
	// Summary is nil until the build finishes.
	Summary *BuildSummary `json:"summary,omitempty"`
	Error   *UIError      `json:"error,omitempty"`
}

// BuildStarted is the `build:started` payload.
type BuildStarted struct {
	TargetGeneration  uint64 `json:"target_generation"`
	SelectionRevision uint64 `json:"selection_revision"`
	// Command is redacted, the same string BuildStatus.Command carries. See
	// CommandPreview for what redaction removes and what it costs a reader.
	Command   []string `json:"command"`
	TargetID  string   `json:"target_id"`
	ItemCount int      `json:"item_count"`
	StartedAt string   `json:"started_at"`
	OutputDir string   `json:"output_dir"`
}

// BuildFinished is the `build:finished` payload.
type BuildFinished struct {
	OK        bool `json:"ok"`
	Cancelled bool `json:"cancelled"`
	// Summary is nil when the build failed before producing one.
	Summary    *BuildSummary `json:"summary,omitempty"`
	DurationMS int64         `json:"duration_ms"`
	Error      *UIError      `json:"error,omitempty"`
}

// BuildEvent is one raw debark NDJSON event, projected for the details
// drawer.
//
// This is the one place a pass-through of debark's own `evidence.Event`
// would have been defensible — it is a versioned wire type that exists purely
// to be serialised. It is projected anyway, for three reasons: the frozen
// contract forbids a `core/` type at the bridge; the drawer wants the raw line
// verbatim for copy-paste, which a decoded struct cannot give back; and Seq is
// a GUI concept the core type has no field for. Nothing is lost, because Attrs
// already passes arbitrary payload through untyped.
type BuildEvent struct {
	// Seq is this event's position in the run, starting at 1. BuildLog pages
	// by it.
	Seq int `json:"seq"`
	// TS is the event's own RFC 3339 timestamp.
	TS string `json:"ts,omitempty"`
	// Type is debark's event type, e.g. "apt.resolve", "fetch.file".
	Type string `json:"type"`
	// Level is "info", "warn" or "error"; empty means info.
	Level string `json:"level,omitempty"`
	Msg   string `json:"msg,omitempty"`
	// Attrs is the event's type-specific payload, untouched.
	Attrs map[string]any `json:"attrs,omitempty"`
	// Raw is the event as one NDJSON line, for the drawer to show and copy.
	//
	// It is a re-encoding of the decoded event, not the literal bytes off the
	// pipe: the CLI adapter delivers a decoded evidence.Event, so the original
	// line is gone by the time this is built. Every field and value is
	// therefore faithful, and the key order may differ from what debark
	// wrote. That distinction matters to anyone diffing this against a
	// bundle's own evidence.json, which is why it is stated rather than
	// implied — an earlier version of this comment promised "the exact NDJSON
	// line as debark emitted it" and the field was never populated at all.
	Raw string `json:"raw,omitempty"`
}

// BuildLogPage is one page of the raw event log.
type BuildLogPage struct {
	Events []BuildEvent `json:"events"`
	// NextSeq is the Seq to ask for next. When it equals the requested
	// SinceSeq there was nothing new.
	NextSeq int `json:"next_seq"`
	// Total is how many events the run has produced.
	Total int `json:"total"`
	// Dropped is how many were discarded from the ring buffer because the run
	// produced more than BuildLogRing. The drawer says so rather than
	// pretending the log is complete.
	Dropped int      `json:"dropped"`
	Error   *UIError `json:"error,omitempty"`
}

// CommandPreview is the command the GUI is about to run, with vendor-URL
// credentials removed.
//
// Rule 8: the CLI stays the complete interface, and the GUI shows the command
// where it reasonably can. This method is how.
//
// # Redacted, and what that costs the reader
//
// This is NOT the byte-exact argv. It is cliadapter.RedactArgv's output, and
// the difference matters to whoever writes the words next to it.
//
// The exact argv still runs — PreviewCommand and StartBuild are handed the same
// spec, and the adapter derives the real command line from it — but the string
// in this type exists to be read and put on a clipboard, and the application
// cannot tell whether that clipboard is going to a shell, a script, a ticket or
// a chat message. Only one of the four is a safe home for a password.
// docs/security-review.md §6.1a is the decision; there is deliberately no
// reveal control, because a secret you can reveal is a secret on the screen.
//
// The redaction is core's own (fetch.RedactURL) and removes more than userinfo:
// the WHOLE query string goes, because a presigned URL's credential lives in a
// parameter whose name is not standardised and cannot be told from a harmless
// one by pattern. So all three of these are possible in Display:
//
//	url:https://deploy:tok@vendor.example/a.deb  ->  url:https://REDACTED@vendor.example/a.deb
//	url:https://cdn.example/a.deb?token=abc      ->  url:https://cdn.example/a.deb?REDACTED
//	url:https://cdn.example/a.deb?mirror=eu      ->  url:https://cdn.example/a.deb?REDACTED
//
// The third has no secret in it at all and is redacted anyway. It is the case
// the UI copy has to be honest about: for a tray holding any URL with userinfo
// or a query string, this is a faithful description of the build and NOT a
// paste-and-run script. A caption promising the reader that the line is
// "exactly what will run", or that copying it reproduces the bundle from a
// terminal, is false for those inputs. Say instead that credentials and query
// strings are hidden here, and that a terminal run needs them typed back in.
// The operator has not lost anything: the URL they typed is in the selection
// tray where they typed it.
//
// The marker is always visible ("REDACTED"), never a silent removal, so nobody
// reads the result as a URL that never had a credential.
type CommandPreview struct {
	// Argv is the command as an argument vector — no shell quoting, no shell
	// involved. Redacted; see the type comment.
	Argv []string `json:"argv"`
	// Display is Argv rendered as a copy-pasteable single line, shell-quoted
	// for a POSIX shell. It is for showing and copying, never for executing:
	// nothing in this application passes a string to a shell. Redacted; see
	// the type comment.
	Display string `json:"display"`
	// Error is set when the options cannot be turned into a legal command
	// line, and then Argv and Display are empty.
	//
	// PreviewCommand validates the spec exactly as StartBuild does, so the
	// preview panel cannot render a command the application would refuse to
	// run. The case that motivated it: an expected SHA-256 paired with a URL
	// containing "=", which `build --digest` misbinds silently (§6.5a). Before
	// this, the operator read a command, copied it, and found out on Build.
	Error *UIError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Export — screen 5.
// ---------------------------------------------------------------------------

// VolumeKind is the coarse class of a destination — what the UI groups and
// colours by. Deliberately coarser than the filesystem type: the operator
// cares about "is this the stick I just plugged in", not about exfat versus
// vfat.
type VolumeKind string

const (
	VolumeKindUnknown VolumeKind = "unknown"
	// VolumeKindRemovable is media the operator can physically pull out. This
	// is the case the whole screen exists for.
	VolumeKindRemovable VolumeKind = "removable"
	// VolumeKindFixed is an internal disk. Shown, never ranked first, never
	// auto-selected.
	VolumeKindFixed VolumeKind = "fixed"
	// VolumeKindNetwork is an NFS/SMB/sshfs mount. A real destination, but
	// its capacity numbers are advisory.
	VolumeKindNetwork VolumeKind = "network"
)

// VolumeView is one already-mounted filesystem the operator could copy to.
//
// Mounted volumes only. This application performs a plain filesystem copy and
// nothing else: no block device is ever opened, nothing is partitioned or
// formatted, no bootable media is written, and nothing needs root. That is a
// permanent do-not-build item, not a missing feature.
//
// The fields exist to make the chooser trustworthy. Picking the wrong
// destination is the failure that matters on this screen, so the row must show
// enough to tell a 64 GB USB stick from the root filesystem at a glance.
type VolumeView struct {
	// Path is the directory to copy into, and is also the volume's identity:
	// it is what ExportOptions.Destination takes.
	Path string `json:"path"`
	// Label is the name to show, best-effort and never empty.
	Label string `json:"label"`
	// Device is the mount source as the kernel reports it. Display and
	// disambiguation only; nothing ever opens it.
	Device string `json:"device,omitempty"`
	// FSType is the filesystem type. Shown because it is the difference
	// between "this stick will work" and a support ticket — vfat's 4 GiB
	// per-file limit, for instance.
	FSType string `json:"fs_type,omitempty"`
	// TotalBytes and FreeBytes describe capacity. FreeBytes is space
	// available to THIS user, not to root, so it is the number that actually
	// predicts whether the copy fits. Both 0 when capacity is unknown.
	TotalBytes uint64     `json:"total_bytes"`
	FreeBytes  uint64     `json:"free_bytes"`
	Kind       VolumeKind `json:"kind"`
	Removable  bool       `json:"removable"`
	// ReadOnly is the filesystem mounted read-only. Distinct from Writable:
	// a read-write filesystem this user has no permission on is
	// Writable=false, ReadOnly=false, and the two need different advice.
	ReadOnly bool `json:"read_only"`
	// Writable is a cheap, no-write best-effort answer to "can this user
	// create files here". The definitive test happens when the copy starts.
	Writable bool `json:"writable"`
	// Note is a short explanation when something about this volume is off.
	// Empty when it is an unremarkable, usable destination.
	Note string `json:"note,omitempty"`
}

// VolumesResult lists mounted volumes, removable media first and the machine's
// own root filesystem last, so a keyboard-driven operator never lands on it by
// default.
type VolumesResult struct {
	Volumes []VolumeView `json:"volumes"`
	// Fingerprint is a short token that changes when the set of destinations
	// meaningfully changes. A UI that re-lists on a timer compares it and
	// skips the redraw when it has not moved.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Supported is false on a platform with no volume enumeration. The UI
	// then offers a plain directory picker instead of showing an error: an
	// unsupported platform is not a broken one.
	Supported bool     `json:"supported"`
	Error     *UIError `json:"error,omitempty"`
}

// ExportOptions is a copy request.
type ExportOptions struct {
	// BundlePath is the bundle folder to copy. Empty means "the bundle the
	// last build produced".
	BundlePath string `json:"bundle_path,omitempty"`
	// Destination is a mounted volume's Path, or any directory. It is a path,
	// never a device.
	Destination string `json:"destination"`
	// Subdir is the folder to create under Destination. Empty means the
	// bundle folder's own name.
	Subdir string `json:"subdir,omitempty"`
	// SkipVerify turns off the post-copy verification pass.
	//
	// Leave it false. Verification — re-reading every byte off the drive and
	// comparing digests — is the step that makes this feature worth having,
	// and this field exists only so that a caller who genuinely does not want
	// it has to say so. With it set, the incomplete marker is removed without
	// anything having been read back, and ExportStatus reports VerifySkipped.
	SkipVerify bool `json:"skip_verify,omitempty"`
}

// ExportPlan is everything a copy is about to do, computed without writing
// anything. It is safe to show the operator and ask them to confirm.
type ExportPlan struct {
	SourceDir  string `json:"source_dir"`
	DestDir    string `json:"dest_dir"`
	TotalFiles int    `json:"total_files"`
	TotalBytes int64  `json:"total_bytes"`
	// RequiredBytes is TotalBytes with every file rounded up to an allocation
	// unit, plus the margin. This, not TotalBytes, is what was compared
	// against FreeBytes.
	RequiredBytes int64 `json:"required_bytes"`
	// FreeBytes is the free space that was measured, or -1 for unknown.
	FreeBytes int64 `json:"free_bytes"`
	// MarginBytes is the slack the copy refuses to consume.
	MarginBytes int64 `json:"margin_bytes"`
	// Warnings are things worth knowing that are not fatal: a filename FAT
	// cannot store, a file too big for FAT32, unknown free space.
	Warnings []string `json:"warnings,omitempty"`
	// DestinationIncomplete is true when DestDir ALREADY holds the marker an
	// interrupted export leaves behind — someone pulled the drive out, the
	// machine lost power, or a previous copy was cancelled.
	//
	// It is not a refusal: copying again is exactly the right fix, and the
	// copy will rewrite the marker and only remove it if verification passes.
	// It is a warning the operator must see first, because the thing on that
	// drive right now looks like a bundle, is not one, and is about to be
	// overwritten. The engine has always known this; nothing crossed the
	// bridge to say so.
	DestinationIncomplete bool `json:"destination_incomplete"`
	// EquivalentCommand is the shell command that would do the same copy,
	// argv-style.
	//
	// Present because the CLI stays the complete interface and the GUI shows
	// that command where it reasonably can — and this is one of the places it
	// reasonably can. It also settles, for an operator who does not yet trust
	// the app, exactly what "export" means: a file copy, not something that
	// touches the drive's partition table.
	EquivalentCommand []string `json:"equivalent_command,omitempty"`
}

// ExportPlanResult wraps ExportPlan with the error contract. An error here
// means the copy must not start — including the free-space refusal, which
// exists so that a copy predicted to fail is never begun.
type ExportPlanResult struct {
	Plan  ExportPlan `json:"plan"`
	Error *UIError   `json:"error,omitempty"`
}

// DestinationView is what is already sitting at a chosen destination, answered
// without reading the bundle and without writing anything.
//
// It exists for the moment between "the operator picked a drive" and "the
// operator confirmed the plan". An interrupted export leaves a folder that
// looks exactly like a finished bundle apart from one marker file, and the
// exporter has always known the difference — nothing crossed the bridge to say
// so, so the screen could not warn about the one thing it must never let
// someone carry through an air gap.
type DestinationView struct {
	// Destination is the folder the operator chose, echoed back.
	Destination string `json:"destination"`
	// Path is where the bundle would actually land: Destination joined with
	// the subdirectory. It is computed exactly the way StartExport computes
	// it, so this reports on the folder the copy would really touch.
	Path string `json:"path"`
	// Exists is whether Path is there at all. False is the ordinary, happy
	// case: a fresh destination.
	Exists bool `json:"exists"`
	// Incomplete is true when Path holds the marker an unfinished export left
	// behind. Treat what is there as unusable — that is the marker's whole
	// purpose — and say so before the operator confirms anything.
	Incomplete bool `json:"incomplete"`
	// MarkerName is the marker file's name, so the message can name the file
	// an operator would find if they looked. Never build a path from it.
	MarkerName string `json:"marker_name"`
	// Message is the sentence to show when Incomplete is true, and empty
	// otherwise.
	Message string   `json:"message,omitempty"`
	Error   *UIError `json:"error,omitempty"`
}

// IncompleteDestinationMessage is what the export screen says about a
// destination that still carries an interrupted copy's marker.
//
// It lives here rather than in the frontend for the same reason BaseCaveat
// does: it is a claim about what the product knows, not a piece of copy.
const IncompleteDestinationMessage = "This folder holds a bundle from a copy that did not finish, so what is on the drive now must not be used. " +
	"Copying again replaces it, and the folder stays marked until every file has been read back and checked."

// ExportPhase is where a copy has got to.
type ExportPhase string

const (
	ExportPhaseIdle      ExportPhase = "idle"
	ExportPhaseChecking  ExportPhase = "checking"
	ExportPhaseCopying   ExportPhase = "copying"
	ExportPhaseFlushing  ExportPhase = "flushing"
	ExportPhaseVerifying ExportPhase = "verifying"
	ExportPhaseFinished  ExportPhase = "finished"
	ExportPhaseFailed    ExportPhase = "failed"
	ExportPhaseCancelled ExportPhase = "cancelled"
)

// ExportProgress is the `export:progress` payload.
type ExportProgress struct {
	Phase   ExportPhase `json:"phase"`
	Message string      `json:"message,omitempty"`
	// File is the relative path being copied right now.
	File       string `json:"file,omitempty"`
	FilesDone  int    `json:"files_done"`
	FilesTotal int    `json:"files_total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	// BytesPerSec is a smoothed rate for the "time remaining" line, 0 when
	// not yet meaningful.
	BytesPerSec int64 `json:"bytes_per_sec"`
	// Fraction is 0..1, or -1 for indeterminate.
	Fraction float64 `json:"fraction"`
}

// ExportStatus is the copy's whole state, readable at any time.
type ExportStatus struct {
	Running     bool           `json:"running"`
	Finished    bool           `json:"finished"`
	Cancelled   bool           `json:"cancelled"`
	Progress    ExportProgress `json:"progress"`
	BundlePath  string         `json:"bundle_path,omitempty"`
	Destination string         `json:"destination,omitempty"`
	// DestinationPath is where the bundle actually landed, once known.
	DestinationPath string `json:"destination_path,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	// Verified is true when the post-copy verification passed. When Verify
	// was false it stays false and VerifySkipped is true, so the UI never
	// shows an unverified copy as verified.
	Verified      bool `json:"verified"`
	VerifySkipped bool `json:"verify_skipped"`
	// Summary is the exporter's own one-sentence account of the outcome,
	// including what verification did and did not prove. Empty until the copy
	// finishes.
	Summary string `json:"summary,omitempty"`

	// VerifyMethod says, in one sentence an operator can read, exactly what
	// was checked — every file hashed as written, fsynced, re-read off the
	// drive and hashed again, with both the size and the two digests having to
	// match. When verification was skipped it says that instead.
	//
	// It comes from the exporter, which is the only thing that knows what it
	// did. The frontend previously carried this sentence as a hard-coded
	// constant, which is exactly the drift the view layer exists to prevent: a
	// change to what the exporter checks would have left the screen making a
	// claim that was no longer true.
	VerifyMethod string `json:"verify_method,omitempty"`
	// VerifyCaveat is the limit of that check, stated honestly: the copy is
	// proved to have arrived intact, and nothing here promises those flash
	// cells will still read back correctly months from now. Also the
	// exporter's own sentence, for the same reason.
	VerifyCaveat string `json:"verify_caveat,omitempty"`
	// FilesChecked and BytesChecked are what the verification pass actually
	// read back off the drive. Both 0 when it was skipped.
	FilesChecked int   `json:"files_checked"`
	BytesChecked int64 `json:"bytes_checked"`
	// Mismatches are the files that did not survive the trip, clamped to
	// ExportMismatchMax.
	//
	// Empty on a passing verification. On a failing one this is the only place
	// the detail exists: "the copy failed" without the list is a wall, and the
	// operator's next question is always which files and how.
	Mismatches []ExportMismatch `json:"mismatches,omitempty"`
	// MismatchCount is the true number of failures, which is what to render as
	// the headline. MismatchesTruncated says the list above is shorter.
	MismatchCount       int      `json:"mismatch_count"`
	MismatchesTruncated bool     `json:"mismatches_truncated"`
	Error               *UIError `json:"error,omitempty"`
}

// ExportMismatch is one file that did not survive the trip to the drive.
type ExportMismatch struct {
	// Path is the file's path relative to the bundle folder.
	Path string `json:"path"`
	// Reason is why it failed, from the exporter's own vocabulary:
	//
	//	missing     the file is not on the drive at all
	//	size        it is there and the wrong length
	//	content     right length, wrong bytes — a silently dropped write
	//	not_copied  it appeared on the drive without the copy writing it
	//	unreadable  the drive would not give it back
	//
	// Branch on this, never on Detail.
	Reason string `json:"reason"`
	// Detail is supporting evidence, when there is any. Never the message.
	Detail string `json:"detail,omitempty"`
	// WantBytes and GotBytes are the sizes that disagreed, 0 when the reason
	// is not about size.
	WantBytes int64 `json:"want_bytes"`
	GotBytes  int64 `json:"got_bytes"`
	// WantSHA256 and GotSHA256 are the digests that disagreed, lowercase hex,
	// empty when the reason is not about content. Show them as evidence; they
	// are what makes "this drive corrupted the copy" a fact rather than a
	// guess.
	WantSHA256 string `json:"want_sha256,omitempty"`
	GotSHA256  string `json:"got_sha256,omitempty"`
}

// ExportFinished is the `export:finished` payload.
type ExportFinished struct {
	OK         bool         `json:"ok"`
	Cancelled  bool         `json:"cancelled"`
	Status     ExportStatus `json:"status"`
	DurationMS int64        `json:"duration_ms"`
	Error      *UIError     `json:"error,omitempty"`
}

// VerifyFinding is one thing a verification pass reported.
type VerifyFinding struct {
	// Severity is "error", "warning" or "info".
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
	// Path is the bundle-relative file the finding is about, when there is
	// one.
	Path string `json:"path,omitempty"`
}

// VerifyStatus is the verification pass's whole state.
type VerifyStatus struct {
	Running    bool   `json:"running"`
	Finished   bool   `json:"finished"`
	Cancelled  bool   `json:"cancelled"`
	BundlePath string `json:"bundle_path,omitempty"`
	// OK is the verdict, meaningful only when Finished is true.
	OK bool `json:"ok"`
	// Signed reports whether the bundle carried a signature at all. An
	// unsigned bundle that is otherwise intact is OK-and-unsigned, and the UI
	// must distinguish the two.
	Signed   bool            `json:"signed"`
	Findings []VerifyFinding `json:"findings,omitempty"`
	// ExitClass is debark's class for the verification outcome.
	ExitClass  string   `json:"exit_class,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Error      *UIError `json:"error,omitempty"`
}

// VerifyFinished is the `verify:finished` payload.
type VerifyFinished struct {
	OK         bool         `json:"ok"`
	Cancelled  bool         `json:"cancelled"`
	Status     VerifyStatus `json:"status"`
	DurationMS int64        `json:"duration_ms"`
	Error      *UIError     `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Native dialogs.
// ---------------------------------------------------------------------------

// FilePickResult is every native file dialog's return.
//
// Cancellation is not an error: dismissing a dialog is a normal thing to do,
// and a UI that shows a red banner for it is wrong. Check Cancelled first.
type FilePickResult struct {
	// Path is the first (often only) chosen path, empty when cancelled.
	Path string `json:"path"`
	// Paths is every chosen path. Single-selection dialogs return zero or one
	// entry here, so a caller can treat both the same way.
	Paths     []string `json:"paths"`
	Cancelled bool     `json:"cancelled"`
	Error     *UIError `json:"error,omitempty"`
}
