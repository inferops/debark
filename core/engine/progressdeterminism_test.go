package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
)

// progressEmittingFetcher is a fake fetcher that succeeds and emits a
// caller-chosen number of progress events on the way, exactly as
// core/fetch's progressWriter does through the same Options.Events sink.
//
// The count is a parameter here because in production it is a wall clock.
// Measured with the real fetcher against one httptest server serving one
// fixture .deb at two pacings -- same bytes, same digest, same result -- the
// collector received 2 progress events on the fast run and 9 on the slow
// one. This fake reproduces that difference deterministically so a test can
// assert what it costs.
type progressEmittingFetcher struct {
	events   evidence.Sink
	result   fetch.Result
	nEvents  int
	totalLen int64
}

func (f *progressEmittingFetcher) Fetch(ctx context.Context, in buildjob.URLInput) (*fetch.Fetched, error) {
	for i := 1; i <= f.nEvents; i++ {
		evidence.Emit(f.events, evidence.TypeProgress, "", map[string]any{
			"url":         in.URL,
			"bytes":       int64(i) * f.totalLen / int64(f.nEvents),
			"total_bytes": f.totalLen,
		})
	}
	return f.result.Fetched, f.result.Err
}

func (f *progressEmittingFetcher) FetchAll(ctx context.Context, in []buildjob.URLInput) []fetch.Result {
	out := make([]fetch.Result, 0, len(in))
	for _, one := range in {
		fetched, err := f.Fetch(ctx, one)
		out = append(out, fetch.Result{Input: one, Fetched: fetched, Err: err})
	}
	return out
}

// TestBuild_ProgressDoesNotReachTheBundle is the determinism defect any build
// with a vendor URL had, and the reason the project's existing checks could
// not see it: hack/reproducible-check.sh builds the binary, and the demo
// build resolves apt:jq, so nothing downloads a vendor URL and nothing emits
// a progress event.
//
// core/fetch's progressWriter emits on a 250 ms wall clock, so the NUMBER of
// progress events a download produces is a fact about the network that
// afternoon. Every one of them reached the collector that becomes
// evidence.json, which the manifest hashes and the signature covers, so two
// builds of one request over a fast and a slow link produced two different
// bundle ids for byte-identical output.
func TestBuild_ProgressDoesNotReachTheBundle(t *testing.T) {
	const vendorURL = "https://vendor.example/acme-agent_2.1.0_amd64.deb"
	fakeDeb := []byte("pretend .deb bytes for the progress determinism test")
	digestHex := digest.Bytes(fakeDeb)

	// buildWith runs one complete build whose single vendor download emits
	// nEvents progress events, and returns the bundle's evidence.json.
	buildWith := func(t *testing.T, nEvents int) []byte {
		t.Helper()
		h := newHarness(t)
		h.store.objects[digestHex] = fakeDeb
		h.req.Inputs.URLs = []buildjob.URLInput{{URL: vendorURL}}
		newFetcher = func(opts fetch.Options) fetch.Fetcher {
			return &progressEmittingFetcher{
				events:   opts.Events,
				nEvents:  nEvents,
				totalLen: int64(len(fakeDeb)),
				result: fetch.Result{Fetched: &fetch.Fetched{
					URL:          vendorURL,
					Filename:     "acme-agent_2.1.0_amd64.deb",
					Digest:       digestHex,
					Size:         int64(len(fakeDeb)),
					StorePath:    h.store.Path(digestHex),
					Verification: lock.VerifiedURLUnverified,
				}},
			}
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

	var fast, slow []byte
	t.Run("fast link, 2 progress events", func(t *testing.T) { fast = buildWith(t, 2) })
	t.Run("slow link, 9 progress events", func(t *testing.T) { slow = buildWith(t, 9) })

	if string(fast) != string(slow) {
		t.Errorf("two builds of one request produced different evidence.json (%d bytes vs %d) purely from how fast the download ran.\n"+
			"evidence.json is covered by the manifest and the signature, so this is two different bundle ids for byte-identical output.",
			len(fast), len(slow))
	}
	if len(fast) == 0 {
		t.Fatal("evidence.json is empty; the test proves nothing")
	}
}

// TestProgressStillReachesTheOperatorsStream is the half that must not move.
// Dropping progress from the bundle's copy is only acceptable because the
// live stream -- the other arm of the MultiSink resolveDeps builds, and what
// --json-events writes -- still carries every event exactly as emitted. A
// progress bar that stopped working would be too high a price for a
// reproducible artefact, and would not be noticed by the test above.
func TestProgressStillReachesTheOperatorsStream(t *testing.T) {
	b := &build{createdAt: "2026-01-01T00:00:00Z"}
	caller := &recordingSink{}
	collector := evidence.NewCollector(nil)
	sink := evidence.MultiSink{caller, buildClockSink{b: b, next: collector}}

	for i := 1; i <= 9; i++ {
		sink.Emit(evidence.New(evidence.TypeProgress, "", map[string]any{
			"url": "https://vendor.example/a.deb", "bytes": i * 100, "total_bytes": 900,
		}))
	}
	// One non-progress event, so the assertions below distinguish "the type
	// was dropped" from "the sink stopped working".
	sink.Emit(evidence.New(evidence.TypeFetchFile, "fetched a.deb", map[string]any{"filename": "a.deb"}))

	if got := len(caller.events); got != 10 {
		t.Errorf("the operator's live stream saw %d events, want all 10: progress is what a progress bar is made of", got)
	}
	var progressSeen int
	for _, e := range caller.events {
		if e.Type == evidence.TypeProgress {
			progressSeen++
		}
	}
	if progressSeen != 9 {
		t.Errorf("the live stream saw %d progress events, want 9", progressSeen)
	}
	if got := caller.events[0].Attrs["bytes"]; got != 100 {
		t.Errorf("the live stream's first progress event carries bytes=%v, want the exact value 100", got)
	}

	got := collector.Events()
	if len(got) != 1 {
		t.Fatalf("the bundle's copy recorded %d events, want only the non-progress one: %+v", len(got), got)
	}
	if got[0].Type != evidence.TypeFetchFile {
		t.Errorf("the bundle's copy kept %q, want only %q", got[0].Type, evidence.TypeFetchFile)
	}
}
