package cliadapter

// Tests for events.go: Adapter.Build and the event tail behind it.
//
// Two layers, deliberately:
//
//   - The tailer is driven directly, with drain called by hand. Every content
//     rule in cli-surface.md §5 — a partial final line, a malformed line, an
//     unknown type, total_bytes = -1 — is a deterministic assertion there,
//     with no process, no goroutine and no clock.
//
//   - Build is driven against a REAL child process, compiled on the fly from
//     eventsHelperSource. It understands --json-events, writes NDJSON with
//     realistic timing, prints a --json result and exits with a chosen code.
//     Nothing here needs a debark binary, an apt mirror or a network.
//
// The one property that cannot be tested at either layer alone is the one
// most likely to be got wrong: events written just before the process exits
// must still be delivered. TestBuildDrainsEventsWrittenBeforeExit pins it by
// setting the poll interval to an hour, so the only drain that can possibly
// happen is the one after Wait returns.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
)

// ---------------------------------------------------------------------------
// The tail, driven by hand
// ---------------------------------------------------------------------------

func TestEventsTailerDeliversLinesInOrder(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	for i := 0; i < 5; i++ {
		write(eventsLine(t, evidence.TypeProgress, map[string]any{"url": fmt.Sprintf("u%d", i)}) + "\n")
	}
	tl.drain()

	got := rec.attrStrings("url")
	want := []string{"u0", "u1", "u2", "u3", "u4"}
	if !slices.Equal(got, want) {
		t.Fatalf("events out of order or missing:\n got %v\nwant %v", got, want)
	}
	if tl.malformed != 0 {
		t.Fatalf("malformed = %d, want 0", tl.malformed)
	}
}

// A read lands wherever the writer happened to be. Half a JSON object must
// never be decoded, and it must not be lost either.
func TestEventsTailerHoldsBackPartialFinalLine(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	full := eventsLine(t, evidence.TypeFetchFile, map[string]any{"url": "http://example/a.deb", "size": 12})
	write(full + "\n")
	// The writer is mid-line: no newline yet.
	write(full[:len(full)/2])
	tl.drain()

	if n := rec.count(); n != 1 {
		t.Fatalf("delivered %d events, want 1 — the half-written line must be held back", n)
	}
	// The rest of the line arrives.
	write(full[len(full)/2:] + "\n")
	tl.drain()

	if n := rec.count(); n != 2 {
		t.Fatalf("delivered %d events, want 2 — the completed line must be delivered", n)
	}
	if tl.malformed != 0 {
		t.Fatalf("malformed = %d, want 0: a partial line is not a broken one", tl.malformed)
	}
	if got := rec.attrStrings("url"); got[1] != "http://example/a.deb" {
		t.Fatalf("reassembled line decoded wrong: %v", got)
	}
}

// Best-effort NDJSON: debark drops an event it cannot encode, and a crashed
// run leaves whatever it left. A line that will not decode is counted and
// skipped — never fatal, and never the end of the stream.
func TestEventsTailerSurvivesMalformedLines(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	write(eventsLine(t, evidence.TypeAPTUpdate, map[string]any{"entries": 12}) + "\n")
	write("{\"schema\":\"debark.events/v1\",\"type\":\"progress\"\n") // truncated object
	write("not json at all\n")
	write("[1,2,3]\n") // valid JSON, not an event object
	write("\n")        // a blank line is not a dropped event
	write(eventsLine(t, evidence.TypeBundleAssembled, map[string]any{"package_count": 7}) + "\n")
	tl.drain()

	if got := rec.types(); !slices.Equal(got, []string{evidence.TypeAPTUpdate, evidence.TypeBundleAssembled}) {
		t.Fatalf("surrounding events lost: %v", got)
	}
	if tl.malformed != 3 {
		t.Fatalf("malformed = %d, want 3 (truncated, non-JSON, non-object)", tl.malformed)
	}
}

// Four declared types are never emitted, and a future release may add more.
// Accept anything; never wait for a particular type.
func TestEventsTailerAcceptsUnknownAndNeverEmittedTypes(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	write(eventsLine(t, evidence.TypeBuildStarted, nil) + "\n") // declared, never emitted today
	write(eventsLine(t, "quantum.entangled", map[string]any{"spin": "up"}) + "\n")
	write(eventsLine(t, evidence.TypeBuildFinished, nil) + "\n")
	tl.drain()

	want := []string{evidence.TypeBuildStarted, "quantum.entangled", evidence.TypeBuildFinished}
	if got := rec.types(); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if tl.malformed != 0 {
		t.Fatalf("malformed = %d: an unknown type is not a malformed line", tl.malformed)
	}
}

// total_bytes is -1 when the server sent no Content-Length. It must survive
// the round trip to the sink so ProgressOf can report an indeterminate bar.
func TestEventsTailerProgressWithUnknownTotal(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	write(eventsLine(t, evidence.TypeProgress, map[string]any{
		"url": "https://cdn.example/vendor.deb", "bytes": 4096, "total_bytes": -1,
	}) + "\n")
	write(eventsLine(t, evidence.TypeProgress, map[string]any{
		"url": "https://cdn.example/vendor.deb", "attempt": 2, "attempts": 3,
	}) + "\n")
	tl.drain()

	events := rec.all()
	if len(events) != 2 {
		t.Fatalf("delivered %d events, want 2", len(events))
	}
	p, ok := ProgressOf(events[0])
	if !ok {
		t.Fatal("ProgressOf did not recognise the byte-progress event")
	}
	if p.TotalBytes != -1 {
		t.Fatalf("TotalBytes = %d, want -1", p.TotalBytes)
	}
	if f := p.Fraction(); f != -1 {
		t.Fatalf("Fraction() = %v, want -1 (render an indeterminate bar)", f)
	}
	if p.Bytes != 4096 {
		t.Fatalf("Bytes = %d, want 4096", p.Bytes)
	}
	r, _ := ProgressOf(events[1])
	if !r.Retry || r.Attempt != 2 || r.Attempts != 3 {
		t.Fatalf("retry notice decoded wrong: %+v", r)
	}
}

// debark opens the events file itself, after the process starts. Until then
// there is nothing to read, and a build that fails before it builds its
// evidence sink never creates the file at all.
func TestEventsTailerMissingFileIsNotAnError(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	tl.drain() // the file does not exist yet
	tl.drain()
	if rec.count() != 0 || tl.malformed != 0 {
		t.Fatalf("a missing file produced events (%d) or errors (%d)", rec.count(), tl.malformed)
	}

	write(eventsLine(t, evidence.TypeSnapshotLoaded, map[string]any{"arch": "amd64"}) + "\n")
	tl.drain()
	if rec.count() != 1 {
		t.Fatalf("delivered %d events after the file appeared, want 1", rec.count())
	}
}

// A corrupt file can hold megabytes with no newline in it. The carry-over
// buffer is bounded; the line is counted like any other unreadable one and
// the tail resynchronises at the next newline.
func TestEventsTailerSkipsOverlongLine(t *testing.T) {
	path, write := eventsScratchFile(t)
	rec := newEventsRecorder()
	tl := &eventsTailer{path: path, sink: rec.sink}
	defer tl.closeFile()

	write("{" + strings.Repeat("x", eventsMaxLineBytes+16) + "\n")
	write(eventsLine(t, evidence.TypeBundleAssembled, map[string]any{"package_count": 3}) + "\n")
	tl.drain()

	if got := rec.types(); !slices.Equal(got, []string{evidence.TypeBundleAssembled}) {
		t.Fatalf("did not resynchronise after an over-long line: %v", got)
	}
	if tl.malformed != 1 {
		t.Fatalf("malformed = %d, want 1", tl.malformed)
	}
}

// sink may be nil: events are decoded and discarded.
func TestEventsTailerNilSinkDecodesAnyway(t *testing.T) {
	path, write := eventsScratchFile(t)
	tl := &eventsTailer{path: path}
	defer tl.closeFile()

	write(eventsLine(t, evidence.TypeStoreHit, map[string]any{"name": "zlib1g"}) + "\n")
	write("nonsense\n")
	tl.drain()

	if tl.delivered != 1 || tl.malformed != 1 {
		t.Fatalf("delivered = %d, malformed = %d; want 1 and 1", tl.delivered, tl.malformed)
	}
}

// ---------------------------------------------------------------------------
// argv: what runs versus what the operator is shown
// ---------------------------------------------------------------------------

// The temp path is an implementation detail. Command shows --json, because
// the GUI really runs with it, and never --json-events, because a path under
// the system temp directory is meaningless to paste.
func TestCommandShowsJSONAndNeverTheEventsFile(t *testing.T) {
	spec := eventsSpec()
	a, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := a.Command(spec)
	if !slices.Equal(got, BuildArgv(spec)) {
		t.Fatalf("Command drifted from BuildArgv:\n got %v\nwant %v", got, BuildArgv(spec))
	}
	if slices.Contains(got, "--json-events") {
		t.Fatalf("Command leaked the event stream destination: %v", got)
	}
	if !slices.Contains(got, "--json") {
		t.Fatalf("Command omitted --json, which the GUI really runs with: %v", got)
	}
}

// Appending would put the flag after the "--" separator, where cobra reads it
// as a package name. Every real build has positional arguments, so this is
// the bug that would ship.
func TestBuildArgvWithEventsInsertsBeforeTheSeparator(t *testing.T) {
	t.Run("with positional inputs", func(t *testing.T) {
		argv := BuildArgv(eventsSpec())
		got := buildArgvWithEvents(argv, "/tmp/ev.ndjson")

		sep := slices.Index(got, "--")
		at := slices.Index(got, "--json-events")
		if at < 0 || got[at+1] != "/tmp/ev.ndjson" {
			t.Fatalf("the flag or its value is missing: %v", got)
		}
		if sep < 0 {
			t.Fatalf("BuildArgv lost its separator: %v", got)
		}
		if at > sep {
			t.Fatalf("--json-events landed after the -- separator, where it is a package name: %v", got)
		}
		if got[at-1] != "--json" {
			t.Fatalf("--json-events should follow --json, got %q before it", got[at-1])
		}
		if !slices.Equal(append(got[:at:at], got[at+2:]...), argv) {
			t.Fatalf("insertion changed something other than the flag:\n got %v\nwant %v", got, argv)
		}
	})

	t.Run("with no positional inputs", func(t *testing.T) {
		spec := BuildSpec{BaseID: "ubuntu:24.04/server", ListFiles: []string{"packages.txt"}}
		argv := BuildArgv(spec)
		if slices.Contains(argv, "--") {
			t.Fatalf("expected no separator in %v", argv)
		}
		got := buildArgvWithEvents(argv, "/tmp/ev.ndjson")
		if n := len(got); got[n-2] != "--json-events" || got[n-1] != "/tmp/ev.ndjson" {
			t.Fatalf("expected the flag at the end, got %v", got)
		}
	})

	t.Run("a flag value that looks like --json", func(t *testing.T) {
		// Pathological but decidable: BuildArgv emits its own --json after
		// every flag value, so the LAST occurrence is always the real one.
		spec := eventsSpec()
		spec.OutPath = "--json"
		got := buildArgvWithEvents(BuildArgv(spec), "/tmp/ev.ndjson")
		at := slices.Index(got, "--json-events")
		if got[at-1] != "--json" || got[at-2] == "--out" {
			t.Fatalf("inserted next to the --out value instead of the real --json: %v", got)
		}
	})
}

func TestBuildExitStatusReadsTheProcess(t *testing.T) {
	if got := buildExitStatus(nil); got != 0 {
		t.Fatalf("nil error = %d, want 0", got)
	}
	if got := buildExitStatus(errors.New("wait failed")); got != eventsNoExitStatus {
		t.Fatalf("non-exit error = %d, want %d", got, eventsNoExitStatus)
	}
}

// ---------------------------------------------------------------------------
// Build, against a real child process
// ---------------------------------------------------------------------------

func TestBuildStreamsEventsAndReturnsResult(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	want := eventsResult(buildjob.ExitSuccess)
	eventsRunHelper(t, eventsHelperScript{
		Lines: []string{
			eventsLine(t, evidence.TypeSnapshotLoaded, map[string]any{"arch": "amd64"}),
			eventsLine(t, evidence.TypeAPTResolve, map[string]any{"selections": 7, "unresolved": 0}),
			eventsLine(t, evidence.TypeProgress, map[string]any{"url": "u", "bytes": 10, "total_bytes": -1}),
			eventsLine(t, evidence.TypeFetchFile, map[string]any{"url": "u", "size": 10}),
			eventsLine(t, evidence.TypeBundleAssembled, map[string]any{"package_count": 7}),
		},
		GapMS:  8,
		Stdout: eventsMarshalIndent(t, want),
	})

	rec := newEventsRecorder()
	got, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), rec.sink)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got == nil || got.BundleID != want.BundleID || got.Stats.PackageCount != want.Stats.PackageCount {
		t.Fatalf("result decoded wrong: %+v", got)
	}
	wantTypes := []string{
		evidence.TypeSnapshotLoaded, evidence.TypeAPTResolve,
		evidence.TypeProgress, evidence.TypeFetchFile, evidence.TypeBundleAssembled,
	}
	if gotTypes := rec.types(); !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("events wrong or out of order:\n got %v\nwant %v", gotTypes, wantTypes)
	}
	eventsAssertCleanedUp(t, *tempPath)
}

// The classic bug. A build emits its last events and exits inside one poll
// interval, so the tail has never read them: only a drain AFTER the process
// has gone can deliver them. The poll interval here is an hour, which no test
// waits for, so every event that arrives proves the final drain ran.
func TestBuildDrainsEventsWrittenBeforeExit(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, time.Hour)
	tempPath := eventsCaptureTempPath(t)

	const n = 200
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, eventsLine(t, evidence.TypeProgress, map[string]any{"url": fmt.Sprintf("u%d", i)}))
	}
	eventsRunHelper(t, eventsHelperScript{
		Lines: lines,
		// A half-written final line, which is what a build that is killed or
		// crashes leaves behind. It must be dropped, not decoded, and it must
		// not swallow the 200 good lines in front of it.
		Partial: `{"schema":"debark.events/v1","type":"prog`,
		Stdout:  eventsMarshalIndent(t, eventsResult(buildjob.ExitSuccess)),
	})

	rec := newEventsRecorder()
	start := time.Now()
	if _, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), rec.sink); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Build waited %v: it polled instead of draining after the process exited", elapsed)
	}
	got := rec.attrStrings("url")
	if len(got) != n {
		t.Fatalf("delivered %d of %d events — the tail after the process exited was lost", len(got), n)
	}
	for i, u := range got {
		if want := fmt.Sprintf("u%d", i); u != want {
			t.Fatalf("event %d is %q, want %q: order was not preserved", i, u, want)
		}
	}
	eventsAssertCleanedUp(t, *tempPath)
}

// Exit 3 prints the result and THEN fails. Both halves are the answer: the
// error says the build did not succeed, the result says which inputs are
// missing, and a caller that reads only one of them is wrong.
func TestBuildIncompleteReturnsResultAndError(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	want := eventsResult(buildjob.ExitIncomplete)
	want.Unresolved = []string{"nginx-extras"}
	want.FetchFailed = []string{"https://vendor.example/tool_1.2_amd64.deb"}
	eventsRunHelper(t, eventsHelperScript{
		Lines:  []string{eventsLine(t, evidence.TypeBundleAssembled, map[string]any{"package_count": 7})},
		Stdout: eventsMarshalIndent(t, want),
		// A failing build reports through a SILENT error: stderr is empty.
		Exit: int(dferr.Incomplete),
	})

	rec := newEventsRecorder()
	got, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), rec.sink)
	if err == nil {
		t.Fatal("exit 3 returned no error")
	}
	if got == nil {
		t.Fatal("exit 3 returned no result: Unresolved and FetchFailed are the only place that detail exists")
	}
	if !slices.Equal(got.Unresolved, want.Unresolved) || !slices.Equal(got.FetchFailed, want.FetchFailed) {
		t.Fatalf("result lost its detail: %+v", got)
	}
	adErr, ok := AsError(err)
	if !ok {
		t.Fatalf("error is not a *cliadapter.Error: %T", err)
	}
	if !adErr.HasClass(dferr.Incomplete) {
		t.Fatalf("class = %s, want incomplete", adErr.ClassName())
	}
	if adErr.Summary() == "" {
		t.Fatal("summary is empty: no error path may surface a raw exit code alone")
	}
	if adErr.Canceled() {
		t.Fatal("a failed build reported itself as cancelled")
	}
	if slices.Contains(adErr.Argv(), "--json-events") {
		t.Fatalf("the error leaked the event stream destination: %v", adErr.Argv())
	}
	if rec.count() != 1 {
		t.Fatalf("delivered %d events, want 1", rec.count())
	}
	eventsAssertCleanedUp(t, *tempPath)
}

// A build can emit nothing at all — an early usage failure never opens the
// file. That is not an error, and the result still decodes.
func TestBuildWithNoEventsAtAll(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	eventsRunHelper(t, eventsHelperScript{
		SkipFile: true,
		Stdout:   eventsMarshalIndent(t, eventsResult(buildjob.ExitSuccess)),
	})

	rec := newEventsRecorder()
	got, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), rec.sink)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got == nil {
		t.Fatal("no result")
	}
	if rec.count() != 0 {
		t.Fatalf("delivered %d events from a build that emitted none", rec.count())
	}
	eventsAssertCleanedUp(t, *tempPath)
}

// debark succeeded and the adapter could not use its output. Exit code 0 is
// a legitimate argument to the error constructors for exactly this case.
func TestBuildUndecodableResult(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	eventsRunHelper(t, eventsHelperScript{Stdout: "this is not the result you are looking for\n"})

	got, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), nil)
	if err == nil {
		t.Fatal("undecodable output returned no error")
	}
	if got != nil {
		t.Fatalf("returned a result built from output that would not decode: %+v", got)
	}
	adErr, ok := AsError(err)
	if !ok {
		t.Fatalf("error is not a *cliadapter.Error: %T", err)
	}
	if !strings.Contains(adErr.Summary(), "could not be read") {
		t.Fatalf("summary does not say what happened: %q", adErr.Summary())
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("the decode failure was not kept as the cause: %v", err)
	}
	eventsAssertCleanedUp(t, *tempPath)
}

// Stop is not a failure of the work. The process is killed, the events that
// already arrived are kept, the temp file goes, and the goroutine goes with
// it.
func TestBuildCancelledMidStream(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	lines := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		lines = append(lines, eventsLine(t, evidence.TypeProgress, map[string]any{"url": fmt.Sprintf("u%d", i)}))
	}
	eventsRunHelper(t, eventsHelperScript{
		Lines: lines,
		GapMS: 20,
		// Long enough that a build allowed to finish would blow the test's
		// own timeout: reaching the assertions at all proves the kill landed.
		SleepMS: 120_000,
		Stdout:  eventsMarshalIndent(t, eventsResult(buildjob.ExitSuccess)),
	})

	rec := newEventsRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopWatching := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		deadline := time.After(30 * time.Second)
		for rec.count() < 3 {
			select {
			case <-rec.ping:
			case <-stopWatching:
				return
			case <-deadline:
				cancel()
				return
			}
		}
		cancel()
	}()

	before := runtime.NumGoroutine()
	start := time.Now()
	_, err := eventsAdapter(t, bin).Build(ctx, eventsSpec(), rec.sink)
	elapsed := time.Since(start)
	close(stopWatching)
	<-watcherDone

	if err == nil {
		t.Fatal("a cancelled build returned no error")
	}
	adErr, ok := AsError(err)
	if !ok {
		t.Fatalf("error is not a *cliadapter.Error: %T", err)
	}
	if !adErr.Canceled() {
		t.Fatalf("cancellation was reported as a failure: %q (%s)", adErr.Summary(), adErr.ClassName())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) is false: %v", err)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("Build took %v: the process was not killed", elapsed)
	}
	if rec.count() == 0 {
		t.Fatal("the events that had already arrived were discarded")
	}
	eventsAssertCleanedUp(t, *tempPath)
	eventsAssertNoGoroutineLeak(t, before)
}

// Nothing is spawned and no file is reserved for a spec that cannot become a
// legal command line.
// The defect Capabilities.JSONEvents was declared for and never used.
//
// cobra does not shrug at an unknown flag: a debark that predates
// --json-events answers it with an error and its usage block, exit 1. Passing
// it unconditionally therefore turned EVERY build against such a binary into a
// usage failure, rather than a build with no progress stream — which is the
// degraded mode the capability describes and the reason the field exists.
func TestBuildOmitsJSONEventsWhenTheBinaryHasNone(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	want := eventsResult(buildjob.ExitSuccess)
	want.BundleID = "no-json-events-9f8e7d6c"
	eventsRunHelper(t, eventsHelperScript{
		NoJSONEvents: true,
		Stdout:       eventsMarshalIndent(t, want),
	})

	rec := newEventsRecorder()
	got, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), rec.sink)
	if err != nil {
		t.Fatalf("a build against a binary with no --json-events failed: %v", err)
	}
	if got == nil || got.BundleID != want.BundleID {
		t.Fatalf("result decoded wrong: %+v", got)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("delivered %d events from a binary that cannot stream any", n)
	}
	// Nothing to tail means nothing to reserve: no temp file, no goroutine.
	if *tempPath != "" {
		t.Fatalf("an events file was reserved for a build that cannot write one: %s", *tempPath)
	}
	eventsAssertNoTailers(t)
}

// The converse, and the case that must not regress: a binary that DOES declare
// the flag still gets it, and still streams.
func TestBuildPassesJSONEventsWhenTheBinaryDeclaresIt(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	tempPath := eventsCaptureTempPath(t)

	eventsRunHelper(t, eventsHelperScript{
		Lines:  []string{eventsLine(t, evidence.TypeBundleAssembled, map[string]any{"package_count": 1})},
		Stdout: eventsMarshalIndent(t, eventsResult(buildjob.ExitSuccess)),
	})

	rec := newEventsRecorder()
	if _, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), rec.sink); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("delivered %d events, want 1 — the stream was not tailed", n)
	}
	eventsAssertCleanedUp(t, *tempPath)
}

func TestBuildValidatesBeforeSpawning(t *testing.T) {
	tempPath := eventsCaptureTempPath(t)

	got, err := eventsAdapter(t, filepath.Join(t.TempDir(), "no-such-debark")).
		Build(context.Background(), BuildSpec{}, nil)
	if err == nil {
		t.Fatal("an empty spec was accepted")
	}
	if got != nil {
		t.Fatalf("a rejected spec produced a result: %+v", got)
	}
	adErr, ok := AsError(err)
	if !ok {
		t.Fatalf("error is not a *cliadapter.Error: %T", err)
	}
	if !adErr.HasClass(dferr.Usage) {
		t.Fatalf("class = %s, want usage", adErr.ClassName())
	}
	if *tempPath != "" {
		t.Fatalf("a rejected spec still reserved %s", *tempPath)
	}
	eventsAssertNoTailers(t)
}

// Build must exec the binary the adapter was configured with, not the bare
// program name. Reading a.bin directly instead of going through binary()
// races Probe and, worse, silently ignores Options.BinaryPath: on a machine
// where debark ships beside the GUI but is not on PATH, Probe would succeed
// and every build would fail.
func TestBuildExecsTheLocatedBinary(t *testing.T) {
	// In both cases the helper carries a bundle id no debark could produce,
	// so decoding it is proof of which process actually ran.
	t.Run("the configured path", func(t *testing.T) {
		bin := eventsHelperBinary(t)
		eventsSetPollInterval(t, 5*time.Millisecond)

		want := eventsResult(buildjob.ExitSuccess)
		want.BundleID = "configured-binary-b0a1c2d3"
		eventsRunHelper(t, eventsHelperScript{Stdout: eventsMarshalIndent(t, want)})

		got, err := eventsAdapter(t, bin).Build(context.Background(), eventsSpec(), nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got == nil || got.BundleID != want.BundleID {
			t.Fatalf("Build did not exec the configured binary %s: got %+v", bin, got)
		}
		eventsAssertNoTailers(t)
	})

	// The case that separates a.binary() from a.bin. With no configured path
	// the cached field is empty until discovery runs, so a Build that read it
	// directly would exec the bare name — and here the bare name is not on
	// PATH. This is the shipping arrangement it would break: debark
	// installed beside the GUI, where Probe finds it and every build fails.
	t.Run("discovered beside the executable", func(t *testing.T) {
		bin := eventsHelperBinary(t) // built while PATH is still intact
		eventsSetPollInterval(t, 5*time.Millisecond)

		beside := eventsInstallBesideExecutable(t, bin)

		want := eventsResult(buildjob.ExitSuccess)
		want.BundleID = "beside-the-executable-e4f5a6b7"
		eventsRunHelper(t, eventsHelperScript{Stdout: eventsMarshalIndent(t, want)})

		a, err := New(Options{}) // no BinaryPath: discovery is lazy
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := a.Build(context.Background(), eventsSpec(), nil)
		if err != nil {
			t.Fatalf("Build did not locate %s: %v", beside, err)
		}
		if got == nil || got.BundleID != want.BundleID {
			t.Fatalf("Build execed something other than the located binary: %+v", got)
		}
		eventsAssertNoTailers(t)
	})
}

// Build and Probe both fill in the cached binary path on first use, from
// whatever goroutine gets there first — the app does exactly this when the
// readiness check and a build overlap. Reading that field without the lock is
// a race the suite would otherwise never see, because no other test runs two
// methods at once. Meaningful under -race; harmless without it.
func TestBuildAndProbeShareTheBinaryLock(t *testing.T) {
	bin := eventsHelperBinary(t)
	eventsSetPollInterval(t, 5*time.Millisecond)
	eventsInstallBesideExecutable(t, bin)
	eventsRunHelper(t, eventsHelperScript{
		Stdout: eventsMarshalIndent(t, eventsResult(buildjob.ExitSuccess)),
	})

	// No BinaryPath, so the cached path starts empty and both calls race to
	// discover it.
	a, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// The outcome is not the point: the helper is not debark, so this
		// fails. Touching the shared field at the same time is the point.
		_, _ = a.Probe(context.Background())
	}()
	go func() {
		defer wg.Done()
		if _, err := a.Build(context.Background(), eventsSpec(), nil); err != nil {
			t.Errorf("Build: %v", err)
		}
	}()
	wg.Wait()
	eventsAssertNoTailers(t)
}

// eventsInstallBesideExecutable puts a stand-in debark next to this test
// binary and takes it off PATH, so that locating it exercises the
// beside-the-executable fallback — the arrangement where debark ships with
// the GUI. It returns the path it installed.
func eventsInstallBesideExecutable(t *testing.T, helper string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find this test binary: %v", err)
	}
	beside := filepath.Join(filepath.Dir(self), procExecutableName())
	if _, err := os.Stat(beside); err == nil {
		t.Skipf("%s already exists; not overwriting it", beside)
	}
	body, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if err := os.WriteFile(beside, body, 0o755); err != nil {
		t.Skipf("cannot place a debark beside the test binary: %v", err)
	}
	// Removed rather than left behind: invoke.go's own discovery tests run in
	// this same binary and would find it.
	t.Cleanup(func() { eventsRemoveInsistently(t, beside) })

	// Nothing named debark on PATH, so LookPath must fail and the fallback
	// must be what answers. Set after the helper is compiled: go build needs
	// a PATH of its own.
	t.Setenv("PATH", t.TempDir())
	return beside
}

// eventsRemoveInsistently deletes a file that a process has just stopped
// executing. Windows can hold the image open for a moment after the process
// is gone, and leaving this particular file behind would change what another package discovery tests find.
func eventsRemoveInsistently(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("could not remove %s; invoke.go's discovery tests will find it", path)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The binary is missing entirely: the machine cannot do the job, and the
// message says so rather than reporting a status nothing produced.
func TestBuildReportsAMissingBinary(t *testing.T) {
	tempPath := eventsCaptureTempPath(t)
	missing := filepath.Join(t.TempDir(), "no-such-debark")

	got, err := eventsAdapter(t, missing).Build(context.Background(), eventsSpec(), nil)
	if err == nil {
		t.Fatal("a missing binary returned no error")
	}
	if got != nil {
		t.Fatalf("a missing binary produced a result: %+v", got)
	}
	adErr, ok := AsError(err)
	if !ok {
		t.Fatalf("error is not a *cliadapter.Error: %T", err)
	}
	if !strings.Contains(adErr.Summary(), missing) {
		t.Fatalf("summary does not name the binary it could not run: %q", adErr.Summary())
	}
	eventsAssertCleanedUp(t, *tempPath)
	eventsAssertNoTailers(t)
}

// ---------------------------------------------------------------------------
// Test plumbing
// ---------------------------------------------------------------------------

// eventsAdapter builds the real adapter around a binary, through the same
// constructor the application uses. Going through New rather than the struct
// is what makes these tests exercise the configured-path route: a Build that
// read the cached field directly would ignore Options.BinaryPath and exec
// whatever "debark" happened to be on PATH.
func eventsAdapter(t *testing.T, bin string) Adapter {
	t.Helper()
	a, err := New(Options{BinaryPath: bin})
	if err != nil {
		t.Fatalf("New(%q): %v", bin, err)
	}
	return a
}

// eventsSpec is a spec with one of everything the argv builder can emit,
// including positional inputs — which is what makes the "--" separator, and
// therefore the insertion point of --json-events, matter.
func eventsSpec() BuildSpec {
	return BuildSpec{
		BaseID:   "ubuntu:24.04/server",
		Arch:     "amd64",
		Packages: []string{"nginx"},
		URLs: []buildjob.URLInput{{
			URL:    "https://vendor.example/tool_1.2_amd64.deb",
			SHA256: strings.Repeat("ab", 32),
		}},
		OutPath: "out/bundle",
	}
}

func eventsResult(class buildjob.ExitClass) buildjob.BuildResult {
	return buildjob.BuildResult{
		SchemaVersion: buildjob.SchemaVersion,
		LockRef:       "lock.json",
		ManifestRef:   "manifest.json",
		BundlePath:    "out/bundle",
		BundleID:      "2c9f0d18-6b4a-4f31-9c7e-5a0b3d8e1f26",
		Signed:        true,
		Stats:         buildjob.Stats{Added: 7, PackageCount: 7, Bytes: 3_849_312},
		ExitClass:     class,
	}
}

// eventsRecorder is an EventSink that does not block: it appends under a
// mutex nothing else contends, and wakes a watcher with a non-blocking send.
type eventsRecorder struct {
	mu     sync.Mutex
	events []Event
	// ping is written once, at construction, and only ever read afterwards —
	// so a watcher may select on it without taking the mutex, and a nil
	// channel (which blocks forever) is unreachable.
	ping chan struct{}
}

func newEventsRecorder() *eventsRecorder {
	return &eventsRecorder{ping: make(chan struct{}, 1)}
}

func (r *eventsRecorder) sink(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	select {
	case r.ping <- struct{}{}:
	default:
	}
}

func (r *eventsRecorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *eventsRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *eventsRecorder) types() []string {
	out := []string{}
	for _, e := range r.all() {
		out = append(out, e.Type)
	}
	return out
}

func (r *eventsRecorder) attrStrings(key string) []string {
	out := []string{}
	for _, e := range r.all() {
		s, _ := e.Attrs[key].(string)
		out = append(out, s)
	}
	return out
}

func eventsLine(t *testing.T, typ string, attrs map[string]any) string {
	t.Helper()
	b, err := json.Marshal(Event{
		Schema: evidence.SchemaVersion,
		TS:     "2026-09-06T10:15:00Z",
		Type:   typ,
		Attrs:  attrs,
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(b)
}

func eventsMarshalIndent(t *testing.T, v any) string {
	t.Helper()
	// MarshalIndent, because that is what the CLI prints: a multi-line
	// document, which is precisely why it cannot share a line-oriented stream
	// with the event stream.
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b) + "\n"
}

// eventsScratchFile returns a path that does not exist yet and a function
// that appends to it, creating it on first use — the same order debark
// does things in.
func eventsScratchFile(t *testing.T) (string, func(string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.ndjson")
	var f *os.File
	return path, func(s string) {
		t.Helper()
		if f == nil {
			var err error
			if f, err = os.Create(path); err != nil {
				t.Fatalf("create %s: %v", path, err)
			}
			t.Cleanup(func() { _ = f.Close() })
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func eventsSetPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := eventsPollInterval
	t.Cleanup(func() { eventsPollInterval = prev })
	eventsPollInterval = d
}

// eventsCaptureTempPath records the events file a build is about to use, so a
// test can prove it was removed. The path never appears in the interface, so
// this hook is the only way to see it — which is the point of the hook.
func eventsCaptureTempPath(t *testing.T) *string {
	t.Helper()
	prev := eventsNewTempFile
	t.Cleanup(func() { eventsNewTempFile = prev })
	var captured string
	eventsNewTempFile = func() (string, func(), error) {
		path, remove, err := prev()
		captured = path
		return path, remove, err
	}
	return &captured
}

func eventsAssertCleanedUp(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("no events file was ever reserved")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the events file %s outlived the build (stat err: %v)", path, err)
	}
	eventsAssertNoTailers(t)
}

func eventsAssertNoTailers(t *testing.T) {
	t.Helper()
	if n := eventsTailersLive.Load(); n != 0 {
		t.Fatalf("%d tail goroutine(s) still running after Build returned", n)
	}
}

// eventsAssertNoGoroutineLeak is the coarse check to go with the exact one.
// It allows a moment to settle, because os/exec and the runtime retire their
// own goroutines on their own schedule.
func eventsAssertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// The stand-in for `debark build`
// ---------------------------------------------------------------------------

// eventsHelperScript drives the helper process. Its field names are the JSON
// keys — the helper declares the same struct, with no tags on either side, so
// the two cannot drift over a tag nobody looks at.
type eventsHelperScript struct {
	Lines   []string
	Partial string
	GapMS   int
	Stdout  string
	SleepMS int
	Exit    int
	// SkipFile makes the helper never create the events file, the way a build
	// that fails before it opens its evidence sink does not.
	SkipFile bool
	// NoJSONEvents makes the helper a debark too old to know the flag: its
	// `--help` omits --json-events from the flags block, and being passed the
	// flag anyway is a hard failure. That is what the real binary does — cobra
	// answers an unknown flag with an error and its usage block — and it is
	// the whole point of the capability check.
	NoJSONEvents bool
}

const eventsHelperEnv = "DEBARK_GUI_EVENTS_HELPER"

// eventsRunHelper points the next Build at this script. The child inherits
// the environment, which is how it arrives.
func eventsRunHelper(t *testing.T, s eventsHelperScript) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal helper script: %v", err)
	}
	t.Setenv(eventsHelperEnv, string(b))
}

// eventsHelperSource is a stand-in for `debark build`: it understands
// --json-events, ignores every other flag, and does exactly what the script
// in its environment says. It also asserts the two things about argv that the
// adapter must never get wrong — that --json is present, and that
// --json-events is a flag rather than a positional argument — because a real
// process is the only place those can be checked for real.
const eventsHelperSource = `package main

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

type script struct {
	Lines        []string
	Partial      string
	GapMS        int
	Stdout       string
	SleepMS      int
	Exit         int
	SkipFile     bool
	NoJSONEvents bool
}

// helpText is cobra's shape, reduced to the block the adapter scans: an
// unindented heading and indented flag lines.
func helpText(noEvents bool) string {
	out := "Usage:\n  debark [command]\n\nFlags:\n" +
		"  -h, --help                 help for debark\n" +
		"      --json                 print the documented JSON object\n"
	if !noEvents {
		out += "      --json-events string   stream NDJSON evidence events to PATH\n"
	}
	return out + "      --no-color             disable coloured output\n"
}

func fail(code int, msg string) {
	os.Stderr.WriteString("helper: " + msg + "\n")
	os.Exit(code)
}

func main() {
	var s script
	if raw := os.Getenv("DEBARK_GUI_EVENTS_HELPER"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			fail(64, "bad script: "+err.Error())
		}
	}

	args := os.Args[1:]

	// The capability probe. A real debark answers --help on stdout and exits
	// 0; a binary too old to know --json-events simply does not list it.
	if len(args) == 1 && args[0] == "--help" {
		os.Stdout.WriteString(helpText(s.NoJSONEvents))
		os.Exit(0)
	}

	separator := len(args)
	for i, a := range args {
		if a == "--" {
			separator = i
			break
		}
	}
	path := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--json-events" {
			if i > separator {
				fail(67, "--json-events came after the -- separator, where it is a package name: "+strings.Join(args, " "))
			}
			path = args[i+1]
		}
	}
	switch {
	case s.NoJSONEvents && path != "":
		// What cobra does with an unknown flag, and the failure this whole
		// capability check exists to prevent.
		fail(72, "unknown flag --json-events was passed to a binary that has none: "+strings.Join(args, " "))
	case !s.NoJSONEvents && path == "":
		fail(65, "no --json-events path in: "+strings.Join(args, " "))
	}
	seenJSON := false
	for _, a := range args[:separator] {
		if a == "--json" {
			seenJSON = true
		}
		if a == "--interactive" {
			fail(68, "--interactive must never be passed")
		}
	}
	if !seenJSON {
		fail(66, "no --json in: "+strings.Join(args, " "))
	}

	if !s.SkipFile && path != "" {
		f, err := os.Create(path)
		if err != nil {
			fail(69, "create "+path+": "+err.Error())
		}
		for _, line := range s.Lines {
			if s.GapMS > 0 {
				time.Sleep(time.Duration(s.GapMS) * time.Millisecond)
			}
			if _, err := f.WriteString(line + "\n"); err != nil {
				fail(70, "write: "+err.Error())
			}
		}
		if s.Partial != "" {
			if _, err := f.WriteString(s.Partial); err != nil {
				fail(70, "write: "+err.Error())
			}
		}
		if err := f.Close(); err != nil {
			fail(71, "close: "+err.Error())
		}
	}
	if s.SleepMS > 0 {
		time.Sleep(time.Duration(s.SleepMS) * time.Millisecond)
	}
	if s.Stdout != "" {
		os.Stdout.WriteString(s.Stdout)
	}
	os.Exit(s.Exit)
}
`

// eventsHelperBinary compiles the stand-in. It needs a Go toolchain and
// nothing else — no debark binary, no apt mirror, no network — and skips
// rather than fails when there is not one, so the tailer tests above still
// run wherever they are.
func eventsHelperBinary(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "debark-events-helper")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	// Best effort: a just-exited executable is occasionally still locked on
	// Windows, and that is not a reason to fail a passing test.
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("go.mod", "module debarkeventshelper\n\ngo 1.21\n")
	write("main.go", eventsHelperSource)

	out := filepath.Join(dir, "helper")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	// GOWORK=off so a workspace in a parent directory cannot pull this
	// throwaway module into itself; GOFLAGS cleared so an inherited -mod flag
	// cannot either.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the event stream helper (no usable Go toolchain?): %v\n%s", err, b)
	}
	return out
}
