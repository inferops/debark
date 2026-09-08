package engine

import (
	"slices"
	"testing"

	"github.com/inferops/debark/core/evidence"
)

// TestBuildClockSinkStampsCollaboratorEvents pins the third and last
// determinism defect the 2026-09-05 integration matrix surfaced.
//
// core/engine pins every event it raises itself to the build clock, but a
// COLLABORATOR's event is not its to pin. core/apt reaches its sink through
// evidence.Emit, whose New() stamps time.Now() into TS before the event is
// handed over, so apt.update, apt.resolve and one backend.selected went into
// evidence.json carrying the real clock while every engine-raised event
// around them sat at the pinned created_at. evidence.json is hashed by the
// manifest and the manifest is signed, so two builds of the same request
// under the same SOURCE_DATE_EPOCH still produced different bundles.
//
// The pre-stamped case is the whole point: a first attempt at this fix only
// filled in an EMPTY TS, which is what evidence.Event's contract describes,
// and it changed nothing at all — the matrix row failed again on the same
// three events. That is why the assertion below is on an event that already
// carries a real timestamp.
func TestBuildClockSinkStampsCollaboratorEvents(t *testing.T) {
	const fixed = "2026-01-01T00:00:00Z"
	b := &build{createdAt: fixed}
	col := evidence.NewCollector(nil)
	sink := buildClockSink{b: b, next: col}

	// Exactly how core/apt reaches this sink: evidence.Emit -> New(), which
	// has already put the real clock in TS.
	sink.Emit(evidence.New("apt.resolve", "", nil))
	// And the empty-TS shape too, which some sinks are handed directly.
	sink.Emit(evidence.Event{Type: "apt.update"})

	events := col.Events()
	if len(events) != 2 {
		t.Fatalf("collector recorded %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.TS != fixed {
			t.Errorf("%s was recorded with TS %q, want the build clock %q;\n"+
				"an unpinned timestamp here reaches evidence.json -> the manifest that hashes it -> the signature over that",
				e.Type, e.TS, fixed)
		}
	}
}

// TestBuildClockSinkLeavesEventsAloneBeforeTheClockIsKnown guards the one
// case where overwriting would destroy information rather than normalise it:
// createdAt is read at Emit time (the sink is built in resolveDeps), so an
// event emitted before the clock is known must keep whatever timestamp it
// arrived with instead of being blanked, leaving the sink's own fallback to
// handle it.
func TestBuildClockSinkLeavesEventsAloneBeforeTheClockIsKnown(t *testing.T) {
	b := &build{} // clock not known yet
	col := evidence.NewCollector(nil)
	sink := buildClockSink{b: b, next: col}

	sink.Emit(evidence.Event{Type: "apt.update", TS: "2019-09-09T09:09:09Z"})
	b.createdAt = "2026-02-02T02:02:02Z" // set later
	sink.Emit(evidence.Event{Type: "apt.resolve", TS: "2019-09-09T09:09:09Z"})

	events := col.Events()
	if len(events) != 2 {
		t.Fatalf("collector recorded %d events, want 2", len(events))
	}
	if events[0].TS != "2019-09-09T09:09:09Z" {
		t.Errorf("an event emitted before the clock was known got TS %q, want its own timestamp kept", events[0].TS)
	}
	if events[1].TS != "2026-02-02T02:02:02Z" {
		t.Errorf("TS = %q, want the clock set after the sink was built; the sink must read createdAt at Emit time", events[1].TS)
	}
}

// TestOnlyTheCollectorIsClockPinned pins the SHAPE of the composition in
// resolveDeps, which is load-bearing and easy to "simplify" back.
//
// The first version wrapped the whole fan-out —
// buildClockSink{next: MultiSink{callerSink, collector}} — which pinned the
// operator's live stream too. Deps.Events is the CLI's --json-events NDJSON
// output and its progress renderer; under SOURCE_DATE_EPOCH that stream
// reported every line at the same instant, and in the project's own
// reproducible-build path (hack/reproducible-check.sh exports
// `git log -1 --format=%ct`) that instant is the last commit's date. Only the
// collector becomes evidence.json, so only the collector needs the build
// clock; rewriting what an operator watches happen in real time was never
// part of the goal.
func TestOnlyTheCollectorIsClockPinned(t *testing.T) {
	const fixed = "2026-01-01T00:00:00Z"
	const real = "2019-09-09T09:09:09Z"
	b := &build{createdAt: fixed}
	caller := &recordingSink{}
	collector := evidence.NewCollector(nil)

	// The composition resolveDeps builds.
	sink := evidence.MultiSink{caller, buildClockSink{b: b, next: collector}}
	sink.Emit(evidence.Event{Type: "apt.resolve", TS: real})

	if len(caller.events) != 1 || caller.events[0].TS != real {
		t.Errorf("the caller's live sink saw TS %v, want its own %q left alone", caller.timestamps(), real)
	}
	got := collector.Events()
	if len(got) != 1 || got[0].TS != fixed {
		t.Errorf("the collector recorded TS %v, want the pinned build clock %q", got, fixed)
	}
}

type recordingSink struct{ events []evidence.Event }

func (s *recordingSink) Emit(e evidence.Event) { s.events = append(s.events, e) }
func (s *recordingSink) Close() error          { return nil }
func (s *recordingSink) timestamps() []string {
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.TS)
	}
	return out
}

// TestEnvironmentAttrsAreDroppedFromTheBundleCopy pins the last difference the
// 2026-09-05 matrix's determinism row had left after the clock was pinned.
//
// apt.update carries "entries", the count of index entries apt reported
// fetching. Two builds of the same request on ubuntu 22.04 recorded 18 and
// then 20 — a fact about what the archive served, not about the install set —
// and evidence.json is hashed by the manifest the signature covers, so the
// bundle id moved with it.
//
// The operator's live stream must still see the exact value: it is real
// diagnostics, and only the bundle's own copy has to be reproducible. That
// asymmetry is the whole design, so it is asserted in both directions.
func TestEnvironmentAttrsAreDroppedFromTheBundleCopy(t *testing.T) {
	b := &build{createdAt: "2026-01-01T00:00:00Z"}
	caller := &recordingSink{}
	collector := evidence.NewCollector(nil)
	sink := evidence.MultiSink{caller, buildClockSink{b: b, next: collector}}

	// Every declared environment attribute is emitted with a distinct value,
	// so the assertions below are driven by environmentAttrs itself rather
	// than by a hand-copied list. Adding a fourth attribute without
	// stripping it is the mistake this loop exists to make impossible;
	// "sources_fetched" and "sources_failed" were added later and would
	// otherwise have needed someone to remember this test.
	attrs := map[string]any{"failed": false}
	for i, name := range environmentAttrs[evidence.TypeAPTUpdate] {
		attrs[name] = 18 + i
	}
	sink.Emit(evidence.New(evidence.TypeAPTUpdate, "apt-get update", attrs))

	got := collector.Events()
	if len(got) != 1 {
		t.Fatalf("collector recorded %d events, want 1", len(got))
	}
	for _, name := range environmentAttrs[evidence.TypeAPTUpdate] {
		if _, present := got[0].Attrs[name]; present {
			t.Errorf("the bundle's copy kept the machine-dependent attribute %q: %v", name, got[0].Attrs)
		}
	}
	if got[0].Attrs["failed"] != false {
		t.Errorf("a reproducible attribute was dropped too: %v", got[0].Attrs)
	}

	if len(caller.events) != 1 {
		t.Fatalf("caller sink saw %d events, want 1", len(caller.events))
	}
	for i, name := range environmentAttrs[evidence.TypeAPTUpdate] {
		if caller.events[0].Attrs[name] != 18+i {
			t.Errorf("the operator's live stream lost the exact value of %q: %v; "+
				"only the bundle's copy has to be reproducible", name, caller.events[0].Attrs)
		}
	}

	// The list itself has to name the two counts added for the no-network
	// case: they are exactly as environment-dependent as "entries" is, and
	// leaving either one in evidence.json would put a transient mirror
	// failure into the manifest the signature covers.
	for _, want := range []string{"entries", "sources_fetched", "sources_failed"} {
		if !slices.Contains(environmentAttrs[evidence.TypeAPTUpdate], want) {
			t.Errorf("environmentAttrs does not strip %q from apt.update: %v", want, environmentAttrs[evidence.TypeAPTUpdate])
		}
	}
}
