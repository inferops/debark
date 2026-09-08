// Package evidence is debark's structured record of what happened, written
// as if an auditor will read it in five years (principle 6).
//
// Events are objects, not log strings. The free CLI writes them to
// evidence.json inside the bundle and streams them as NDJSON with
// --json-events. A commercial product would aggregate and retain them; it
// would never invent new ones.
package evidence

import "errors"

// SchemaVersion is the only events schema debark writes.
const SchemaVersion = "debark.events/v1"

// FileName is the evidence file's name inside a bundle.
const FileName = "evidence.json"

// Event types. This list is the contract; adding a type is additive, changing
// the meaning of one is not.
const (
	TypeSnapshotCreated = "snapshot.created"
	TypeSnapshotLoaded  = "snapshot.loaded"
	TypeBackendSelected = "backend.selected"
	TypeAPTUpdate       = "apt.update"
	TypeAPTResolve      = "apt.resolve"
	TypeFetchFile       = "fetch.file"
	TypeInputExternal   = "input.external"
	TypeStoreHit        = "store.hit"
	TypePolicyFinding   = "policy.finding"
	TypeDoctorFinding   = "doctor.finding"
	TypeRepoIndexed     = "repo.indexed"
	TypeClosedWorld     = "closed_world.checked"
	TypeManifestSigned  = "manifest.signed"
	TypeBundleAssembled = "bundle.assembled"
	TypePruned          = "bundle.pruned"
	TypeVerifyResult    = "verify.result"
	TypeInstallPlan     = "install.plan"
	TypeInstallResult   = "install.result"
	TypeWarning         = "warning"
	TypeBuildStarted    = "build.started"
	TypeBuildFinished   = "build.finished"
	TypeProgress        = "progress"
)

// Event is one record. Attrs holds the type-specific fields; every value must
// be JSON-encodable and must never contain a secret. The identity fields an
// auditor needs later (operator, build host, image digest) travel in the
// Context of the sink, not in every event.
type Event struct {
	Schema string `json:"schema"`
	// TS is RFC 3339 UTC.
	TS   string `json:"ts"`
	Type string `json:"type"`
	// Level is info, warn or error. Absent means info.
	Level string `json:"level,omitempty"`
	// Msg is a short human sentence; machines read Attrs, not this.
	Msg   string         `json:"msg,omitempty"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Level values.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Document is the evidence.json file: a header plus the recorded events.
// (NDJSON streaming emits the Event objects alone, one per line.)
type Document struct {
	Schema    string `json:"schema"`
	CreatedAt string `json:"created_at"`
	// Context is the run identity: tool version, edition, backend, build host
	// kind, operator when the operator chose to record one. Never a machine
	// fingerprint, never anything collected without the operator asking for it.
	Context map[string]any `json:"context,omitempty"`
	Events  []Event        `json:"events"`
}

// Sink receives events. Implementations must be safe for concurrent use: the
// fetcher emits from several goroutines.
//
// A Sink never blocks the work it observes; a slow or failing sink drops
// events rather than failing a build, except for the file sink's final Close,
// whose error is reported.
type Sink interface {
	// Emit records one event. Implementations set Schema and TS when empty.
	Emit(e Event)
	// Close flushes and releases the sink.
	Close() error
}

// MultiSink fans one event out to several sinks (file plus terminal plus
// NDJSON stream).
type MultiSink []Sink

func (m MultiSink) Emit(e Event) {
	for _, s := range m {
		if s != nil {
			s.Emit(e)
		}
	}
}

// Close closes every sink and reports every failure, joined.
//
// It used to keep only the FIRST error and drop the rest, which is the one
// shape this type must not have: the Sink contract two paragraphs above says
// a failing sink drops events rather than failing a build "except for the
// file sink's final Close, whose error is reported", and NewFileSink's own
// doc says that error "must not be swallowed, because it is usually the only
// durable copy of the run". A MultiSink is exactly where that copy is fanned
// out alongside a terminal renderer and an NDJSON stream, so keeping the
// first error meant an earlier, less important sink's failure could hide the
// one failure the contract singles out — "could not write evidence.json"
// silently replaced by "could not finish the progress line".
//
// errors.Join, rather than picking the file sink out by type: this package
// cannot know which member the caller cares about, and a joined error keeps
// every one of them findable with errors.Is/errors.As while still reading as
// a single failure. Every sink is closed either way; only the reporting
// changed.
func (m MultiSink) Close() error {
	var errs []error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Discard is a Sink that drops everything. It is the zero value used when a
// caller does not want evidence, so no code path needs a nil check.
type Discard struct{}

func (Discard) Emit(Event)   {}
func (Discard) Close() error { return nil }

// AttrForwardedFrom is the attribute key a debark process stamps onto an
// event it did not raise itself but received from ANOTHER debark process
// and re-emitted. Its value names the boundary the event crossed.
//
// One key, defined here rather than in the package that forwards, because two
// packages disagree about a marker's spelling exactly once and then never
// notice: the producer (core/apt's container backend) and the consumer
// (core/engine's buildClockSink, which keeps forwarded events out of the
// bundle's own record) are in different packages and neither imports the
// other.
//
// The forwarding side sets it LAST and unconditionally, overwriting whatever
// the far process put there. That is what makes it a statement by the host
// about where bytes came from rather than a claim the far process gets to
// make about itself.
const AttrForwardedFrom = "forwarded_from"

// ForwardedFromContainer is AttrForwardedFrom's value for an event the
// container backend read back from the debark process running inside the
// container (core/apt/containerevents.go).
const ForwardedFromContainer = "container"

// eventTypes is every type constant above, as a set. It exists so that a
// process reading events produced by ANOTHER process can refuse a type this
// build does not define, without keeping a second copy of the list that will
// drift from this one.
var eventTypes = map[string]bool{
	TypeSnapshotCreated: true,
	TypeSnapshotLoaded:  true,
	TypeBackendSelected: true,
	TypeAPTUpdate:       true,
	TypeAPTResolve:      true,
	TypeFetchFile:       true,
	TypeInputExternal:   true,
	TypeStoreHit:        true,
	TypePolicyFinding:   true,
	TypeDoctorFinding:   true,
	TypeRepoIndexed:     true,
	TypeClosedWorld:     true,
	TypeManifestSigned:  true,
	TypeBundleAssembled: true,
	TypePruned:          true,
	TypeVerifyResult:    true,
	TypeInstallPlan:     true,
	TypeInstallResult:   true,
	TypeWarning:         true,
	TypeBuildStarted:    true,
	TypeBuildFinished:   true,
	TypeProgress:        true,
}

// KnownType reports whether typ is one of the event types this package
// defines.
//
// Only a reader of UNTRUSTED events needs this. Everything in this repository
// emits a constant from the block above, so the question never arises;
// the container backend, which reads a stream produced by a separate process
// it cannot vouch for, is the caller. Adding a type is additive per the block
// above's own contract, so an older reader meeting a newer producer's type
// refuses it rather than misreading it -- which is the safe direction for
// something that is about to be shown to an operator.
func KnownType(typ string) bool { return eventTypes[typ] }
