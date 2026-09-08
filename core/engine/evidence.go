package engine

import "github.com/inferops/debark/core/evidence"

// emit and warn are the ONLY two ways anything in this package may hand an
// event to b.sink. Every other file in this package must call one of these
// two methods — never evidence.Emit/evidence.Warn directly — which is the
// entire enforcement mechanism for the rule below: there is nowhere else in
// the package that also knows how to build an Event, so there is nothing
// else to keep in sync.
//
// Why this exists (principle 2, SOURCE_DATE_EPOCH — see clock.go):
// evidence.Emit, evidence.Warn and evidence.Collector.Document all stamp
// the real wall clock unconditionally (core/evidence/api.go's New and
// Collector.Emit/Document call time.Now() directly); core/evidence is a
// package this package does not own — its sink implementations are owned
// elsewhere — and its exported api.go signatures are frozen, so there is no
// clock-injection seam on the other side of that call. Using them as-is would
// silently defeat SOURCE_DATE_EPOCH for every event's timestamp, including the
// events written into evidence.json itself — a bundle built twice with the
// same fixed clock would still get two different evidence.json byte
// sequences purely from real-time event timestamps, which is exactly the
// kind of non-reproducibility this package exists to remove (see
// TestDeterminism in test/integration).
//
// The fix stays entirely inside this package: every Sink this build hands
// events to — b.collector, and whatever external sink the caller supplied
// via Deps.Events, fanned out together as b.sink — only fills in an empty
// Event.TS (see Collector.Emit's `if e.TS == "" { ... }`); an ndjsonSink and
// a fileSink follow the identical pattern. Setting TS here, to
// b.createdAt, before the event ever reaches a Sink, is therefore honoured
// everywhere, not just in the bundle's own evidence.json copy. Combined
// with finalizeBundle building evidence.json's Document by hand (Schema,
// CreatedAt: b.createdAt, Context: b.evidenceCtx, Events from
// b.collector.Events()) instead of calling Collector.Document() — which
// also stamps real time for the document header — this is the complete
// fix: nothing that ends up in an artefact ever reads the real clock
// except effectiveCreatedAt, once, in engineImpl.Build.
func (b *build) emit(typ, msg string, attrs map[string]any) {
	b.sendEvent(typ, "", msg, attrs)
}

// warn is emit's warning-level counterpart (evidence.Warn's equivalent).
func (b *build) warn(typ, msg string, attrs map[string]any) {
	b.sendEvent(typ, evidence.LevelWarn, msg, attrs)
}

func (b *build) sendEvent(typ, level, msg string, attrs map[string]any) {
	if b.sink == nil {
		return
	}
	b.sink.Emit(evidence.Event{
		Schema: evidence.SchemaVersion,
		TS:     b.createdAt,
		Type:   typ,
		Level:  level,
		Msg:    msg,
		Attrs:  attrs,
	})
}

// buildClockSink stamps this build's fixed clock onto every event on its way
// into the collector — the copy that becomes evidence.json.
//
// emit/warn above cover every event THIS package raises, and the rule that
// nothing here may call evidence.Emit directly is enforceable because the
// package is small enough to read. Neither reaches an event raised by a
// COLLABORATOR. core/apt's backends take an evidence.Sink of their own
// (LocalOptions.Events, ContainerOptions.Events) and emit through it as
// `evidence.Event{Type: ..., Attrs: ...}` — deliberately leaving TS empty,
// because evidence.Event's own contract says sinks "set Schema and TS when
// empty", and every sink in core/evidence honours that by calling
// time.Now(). Correct against that contract, and it silently defeats
// SOURCE_DATE_EPOCH: those events land in evidence.json, which the manifest
// hashes, which the signature covers.
//
// Measured on the 2026-09-05 matrix run, after both the harness clock fix
// and the closed-world mask fix had made lock.json and repo/Release
// byte-identical: three events — apt.update, apt.resolve and one of the two
// backend.selected — still carried real wall-clock times twelve seconds
// apart between two builds of the same request, while every engine-raised
// event around them sat at the pinned createdAt. Same evidence.json length,
// different bytes; different manifest; different signature.
//
// Wrapping the sink is the fix rather than passing a clock into core/apt:
// core/apt would need a new option on two backends (and any future one) and
// would still be free to forget it, whereas an event cannot reach this
// build's collector without passing through here. It also keeps the
// property evidence.go already claims — "nothing that ends up in an artefact
// ever reads the real clock except effectiveCreatedAt, once" — true for
// collaborators and not just for this package.
//
// The TS is REPLACED, not merely filled in when empty. Filling in the empty
// case is not enough and was measured not to be: core/apt reaches its sink
// through evidence.Emit, whose New() has already stamped time.Now() into TS
// before the event is handed over (core/evidence/api.go), so a
// fill-when-empty wrapper never fires on exactly the events that need it.
// Overwriting is also not a loss of information here — b.sendEvent above
// already stamps every engine-raised event with the same b.createdAt, so
// per-event timing is not something evidence.json has ever recorded, and
// this only makes collaborators consistent with that existing decision.
//
// An empty b.createdAt leaves the event alone rather than blanking it: TS is
// read at Emit time (the sink is built in resolveDeps), and a build that
// somehow emitted before its clock was known is better served by the sink's
// own fallback than by an empty timestamp.
type buildClockSink struct {
	b    *build
	next evidence.Sink
}

// liveOnlyTypes are event types that never reach the bundle's copy of the
// stream at all, whatever their attributes say.
//
// "progress" is the only one, and the reason is its COUNT rather than any
// value inside it. core/fetch's progressWriter emits on a 250 ms wall clock
// (fetcher.go), so how many progress events a download produces is a fact
// about the network that afternoon and nothing else. Measured, with the real
// fetcher against one httptest server serving one fixture .deb at two
// different pacings -- same bytes, same digest, same Fetched result:
//
//	progress events reaching the collector: fast=2  slow=9
//
// Seven extra events in evidence.json, which the manifest hashes and the
// signature covers, for a build whose output is byte-identical. That makes
// any build with a vendor URL unreproducible, and it is invisible to the
// project's existing determinism checks because the demo build resolves
// apt:jq and has no vendor URL to download.
//
// This is environmentAttrs' rule applied one level up, and the same remedy:
// the events stay exact on the live stream an operator watches, and are
// simply not something the bundle records about itself. An operator who
// wants them has --json-events, which is the stream this does not touch.
//
// Dropping the whole type rather than a "bytes" attribute is deliberate.
// progress carries only url/bytes/total_bytes, so an event with the varying
// parts removed says nothing at all, and a run of nine identical empty
// events is still nine events.
var liveOnlyTypes = map[string]bool{
	evidence.TypeProgress: true,
}

// environmentAttrs are event attributes that record what THIS MACHINE met
// rather than what was requested, and so must not reach evidence.json — which
// the manifest hashes and the signature covers.
//
// The entries are apt.update's "entries" (core/apt/local.go's
// len(upd.Entries)), and its two companions "sources_fetched" and
// "sources_failed", which say how many configured sources apt ended up with
// an index for and how many it did not. All three answer "what did this
// machine's network do", which is the definition above; the two counts were
// added because "entries" and "failed" together could not distinguish a
// healthy update from one where nothing was fetched at all (apt exits 0
// either way), and they are stripped here for exactly the same reason
// "entries" is.
//
// Taking "entries" as the worked example: it is the number of index entries
// apt reported fetching. Two
// builds of the same request on ubuntu 22.04 recorded 18 and then 20, which is
// a fact about what the archive served and how much of it apt already had, not
// about the install set. It was the last difference left in the 2026-09-05
// matrix's determinism row after the clock was pinned — same evidence.json
// length, one attribute apart, and from there a different manifest and a
// different signature.
//
// This is the same rule stripCacheDependentStats (reproducible.go) already
// applies to lock.Stats.DownloadedBytes, for the same reason and with the same
// remedy: the value stays exact on the live event stream an operator watches,
// and is simply not a fact the bundle records about itself. Dropping it here
// rather than in core/apt keeps that decision in one reviewable place — the
// same argument the clock pinning makes one comment above — and leaves the
// backends free to report everything they know.
//
// A new attribute belongs here when its value can differ between two builds of
// one request. If that list ever grows past a handful, the shape to reach for
// is an allow-list of reproducible attributes rather than a deny-list of
// environmental ones.
var environmentAttrs = map[string][]string{
	evidence.TypeAPTUpdate: {"entries", "sources_fetched", "sources_failed"},
}

func (s buildClockSink) Emit(e evidence.Event) {
	// Dropped before anything else: this sink's only consumer is the
	// collector that becomes evidence.json, and the operator's live stream
	// is the OTHER arm of the MultiSink in resolveDeps, so it still sees
	// every progress event. See liveOnlyTypes.
	if liveOnlyTypes[e.Type] {
		return
	}
	// The same rule, keyed on WHO raised the event rather than on what type
	// it is. An event carrying evidence.AttrForwardedFrom was not raised by
	// this process: core/apt's container backend read it back from the
	// debark running inside the container and re-emitted it, stamping that
	// attribute itself, unconditionally, over anything the far process wrote
	// there (core/apt/containerevents.go).
	//
	// Two reasons, either sufficient on its own.
	//
	// Determinism, which is liveOnlyTypes' reason one layer out. How many
	// events an inner process emits, in what words, with which attributes, is
	// a property of whichever debark binary was mounted into the container
	// and of what its apt met that afternoon -- not of the request. Some of it
	// is already known to vary: apt.update's "entries", "sources_fetched" and
	// "sources_failed" are stripped by environmentAttrs immediately below,
	// precisely because two builds of one request recorded 18 and then 20.
	// Forwarding a live stream into a hashed document is the trap 5d26dac had
	// just found in core/fetch's 250 ms progress events, where the same bytes
	// produced evidence.json of 3450 and 5220 and two bundle ids for one
	// output. Filtering by attribute rather than by type is what makes this
	// closed rather than a list to keep extending: a new event type the inner
	// process starts emitting is covered the day it appears.
	//
	// Provenance. evidence.json is the record an auditor reads in five years,
	// and the container driver spends containerCrossCheckEnvelope,
	// containerCheckEnvelopeFilename and containerReverifyFiles refusing to
	// take that same process at its word about anything. Its narration does
	// not belong in a signed document under the same schema as this host's own
	// observations, told apart only by an attribute a later reader has to know
	// to look for.
	//
	// Nothing is lost that was ever there: no inner event reached evidence.json
	// before, because none crossed the container boundary at all. What the
	// bundle records about a container build is unchanged -- the plan, the
	// lock, the resolver identity and the image digest, every one of them a
	// host-side conclusion.
	if _, forwarded := e.Attrs[evidence.AttrForwardedFrom]; forwarded {
		return
	}
	if s.b.createdAt != "" {
		e.TS = s.b.createdAt
	}
	if drop := environmentAttrs[e.Type]; len(drop) > 0 && e.Attrs != nil {
		// Copy before mutating: the caller's live sink is handed the SAME
		// Event value by MultiSink, and Attrs is a map — deleting in place
		// would reach through and strip the operator's stream too, which is
		// the half this is deliberately not touching.
		attrs := make(map[string]any, len(e.Attrs))
		for k, v := range e.Attrs {
			attrs[k] = v
		}
		for _, k := range drop {
			delete(attrs, k)
		}
		e.Attrs = attrs
	}
	s.next.Emit(e)
}

func (s buildClockSink) Close() error { return s.next.Close() }
