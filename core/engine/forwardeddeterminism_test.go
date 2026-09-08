package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// TestBuild_ForwardedContainerEventsDoNotReachTheBundle is the determinism
// half of the container event-forwarding change, driven the way 5d26dac drove
// the progress one: two complete builds of one request whose ONLY difference
// is how many events the inner container process narrated, compared byte for
// byte on evidence.json.
//
// evidence.json is hashed by the manifest and covered by the signature, so a
// difference here is two bundle ids for byte-identical output. That is the
// trap 5d26dac found in core/fetch's 250 ms progress events -- same bytes, 2
// events fast and 9 slow, evidence.json 3450 against 5220 -- and forwarding a
// live stream from a SECOND process is the same trap one layer out.
func TestBuild_ForwardedContainerEventsDoNotReachTheBundle(t *testing.T) {
	buildWith := func(t *testing.T, nEvents int) []byte {
		t.Helper()
		h := newHarness(t)
		// Deps.Backend is cleared so selection goes through the
		// Deps.SelectBackend seam, which is the only place a test can see
		// the sink core/apt is really given: apt.Selection.Events is b.sink,
		// the MultiSink of the operator's live stream and the clock-pinned
		// collector. A backend handed in directly as Deps.Backend never
		// receives it, so a test using one could not reproduce this at all.
		h.deps.Backend = nil
		h.deps.SelectBackend = func(ctx context.Context, sel apt.Selection) (apt.Backend, apt.Capabilities, error) {
			return &fwdBackend{t: t, events: sel.Events, nEvents: nEvents}, apt.Capabilities{Available: true}, nil
		}
		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := eng.Build(context.Background(), h.req); err != nil {
			t.Fatalf("Build: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(h.outDir, "evidence.json"))
		if err != nil {
			t.Fatalf("reading evidence.json: %v", err)
		}
		return b
	}

	var quiet, chatty []byte
	// 4 is what a real inner `resolve --backend local` emits: one
	// backend.selected, one snapshot.loaded, one apt.update, one apt.resolve.
	// 23 is what the same request produces when the inner apt retries index
	// fetches and the inner binary is a version that narrates more.
	t.Run("inner process narrated 4 events", func(t *testing.T) { quiet = buildWith(t, 4) })
	t.Run("inner process narrated 23 events", func(t *testing.T) { chatty = buildWith(t, 23) })

	if string(quiet) != string(chatty) {
		t.Errorf("two builds of one request produced different evidence.json (%d bytes vs %d) purely from how much "+
			"the process inside the container chose to say.\nevidence.json is covered by the manifest and the "+
			"signature, so this is two different bundle ids for byte-identical output.", len(quiet), len(chatty))
	}
	if len(quiet) == 0 {
		t.Fatal("evidence.json is empty; the test proves nothing")
	}
	if strings.Contains(string(quiet), evidence.AttrForwardedFrom) {
		t.Errorf("evidence.json carries %q: an event from a process this driver spends three functions refusing to "+
			"trust is in the signed record.\n%s", evidence.AttrForwardedFrom, quiet)
	}
}

// fwdBackend emits a caller-chosen number of FORWARDED events on its way to
// a successful resolve, exactly as core/apt's container backend does when it
// reads the event stream of the debark process running inside the container
// (core/apt/containerevents.go): the host's own stamp on the forwarded_from
// attribute, and no TS, which the sink fills in.
//
// The count is a parameter here for the same reason progressEmittingFetcher's
// is: in production it is not a property of the request. It is a property of
// whichever debark binary happened to be mounted into the container and of
// what its apt met that afternoon -- a different inner version, a different
// apt, a retried index fetch, and the number and wording change while the
// bundle's bytes do not.
type fwdBackend struct {
	t       *testing.T
	events  evidence.Sink
	nEvents int
}

func (b *fwdBackend) Kind() lock.Backend { return lock.BackendContainer }

func (b *fwdBackend) Probe(ctx context.Context, target snapshot.Target) (apt.Capabilities, error) {
	return apt.Capabilities{Available: true}, nil
}

func (b *fwdBackend) Resolve(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
	for i := 0; i < b.nEvents; i++ {
		if b.events != nil {
			b.events.Emit(evidence.Event{
				Type: evidence.TypeAPTUpdate,
				Msg:  "apt-get update",
				Attrs: map[string]any{
					"failed":                   false,
					"entries":                  float64(12 + i),
					evidence.AttrForwardedFrom: evidence.ForwardedFromContainer,
				},
			})
		}
	}
	return fixturePlan(b.t), nil
}

func (b *fwdBackend) ClosedWorld(ctx context.Context, in apt.ClosedWorldInput) (lock.ClosedWorld, error) {
	return lock.ClosedWorld{Result: lock.ClosedWorldOK}, nil
}

// TestForwardedEventsStillReachTheOperatorsStream is the half that must not
// move, and the entire justification for the half above.
//
// Keeping forwarded events out of the bundle is only acceptable because the
// live stream still carries every one of them, exactly as emitted. That
// stream is the whole point of the change: before it, a 61 s container build
// reported four events, all host-side, and there was nothing to show an
// operator between "backend selected" and "finished". A reproducible artefact
// bought by silently discarding the stream would be a worse product than the
// one that had the bug, and the test above cannot tell the difference.
func TestForwardedEventsStillReachTheOperatorsStream(t *testing.T) {
	b := &build{createdAt: "2026-01-01T00:00:00Z"}
	caller := &recordingSink{}
	collector := evidence.NewCollector(nil)
	sink := evidence.MultiSink{caller, buildClockSink{b: b, next: collector}}

	for i := 1; i <= 4; i++ {
		sink.Emit(evidence.Event{
			Type: evidence.TypeAPTResolve, Msg: "apt resolve complete",
			Attrs: map[string]any{
				"selections":               i,
				evidence.AttrForwardedFrom: evidence.ForwardedFromContainer,
			},
		})
	}
	// One host-raised event, so the assertions distinguish "forwarded events
	// were dropped" from "the sink stopped working".
	sink.Emit(evidence.New(evidence.TypeBackendSelected, "backend selected", map[string]any{"backend": "container"}))

	if got := len(caller.events); got != 5 {
		t.Errorf("the operator's live stream saw %d events, want all 5: these events are the container backend's "+
			"only progress information", got)
	}
	if got := caller.events[0].Attrs["selections"]; got != 1 {
		t.Errorf("the live stream's first forwarded event carries selections=%v, want the exact value 1", got)
	}
	if got := caller.events[0].Attrs[evidence.AttrForwardedFrom]; got != evidence.ForwardedFromContainer {
		t.Errorf("the live stream lost the provenance marker (%v): an operator watching a stream must be able to "+
			"tell what the host observed from what the container claimed", got)
	}

	got := collector.Events()
	if len(got) != 1 {
		t.Fatalf("the bundle's copy recorded %d events, want only the host-raised one: %+v", len(got), got)
	}
	if got[0].Type != evidence.TypeBackendSelected {
		t.Errorf("the bundle's copy kept %q, want only %q", got[0].Type, evidence.TypeBackendSelected)
	}
}

// TestForwardedFilterIsKeyedOnProvenanceNotOnType pins the reason the filter
// looks at an attribute instead of extending liveOnlyTypes.
//
// A type list has to be kept up to date with a producer in another process
// and another release, and the day it falls behind is the day an untrusted
// claim lands in a signed document. The attribute is stamped by the
// forwarding host on every event it forwards, whatever the type, so a type
// nobody has thought of yet is covered the moment it appears.
func TestForwardedFilterIsKeyedOnProvenanceNotOnType(t *testing.T) {
	b := &build{createdAt: "2026-01-01T00:00:00Z"}
	collector := evidence.NewCollector(nil)
	sink := buildClockSink{b: b, next: collector}

	for _, typ := range []string{
		evidence.TypeAPTUpdate, evidence.TypeAPTResolve, evidence.TypeBackendSelected,
		evidence.TypeSnapshotLoaded, evidence.TypeWarning, evidence.TypeInputExternal,
	} {
		sink.Emit(evidence.Event{Type: typ, Attrs: map[string]any{
			evidence.AttrForwardedFrom: evidence.ForwardedFromContainer,
		}})
		// The identical type WITHOUT the marker is a host observation and
		// must still be recorded, or this filter would be a type ban.
		sink.Emit(evidence.Event{Type: typ, Attrs: map[string]any{"host": true}})
	}

	got := collector.Events()
	if len(got) != 6 {
		t.Fatalf("the bundle's copy recorded %d events, want 6 (one per type, host-raised only)", len(got))
	}
	for _, e := range got {
		if _, forwarded := e.Attrs[evidence.AttrForwardedFrom]; forwarded {
			t.Errorf("a forwarded %s event reached the bundle's copy: %+v", e.Type, e)
		}
	}
}
