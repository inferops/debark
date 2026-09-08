package apt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// --- what may be believed of an inner process's line ------------------------

// TestContainerDecodeEventLine is the validator, driven over the shapes a
// hostile or broken inner process can put on its own stderr.
//
// Two answers per case, and they are different questions. isEvent says "this
// line claims to be a debark event", which decides whether it is passed
// through as ordinary diagnostics or consumed; ok says "and it may be shown
// to a person". Conflating them is how a line gets both refused as an event
// and printed verbatim in an error message, which would let the inner process
// choose the text of a host-authored failure simply by prefixing it with a
// schema field.
func TestContainerDecodeEventLine(t *testing.T) {
	ev := func(fields string) string {
		return `{"schema":"debark.events/v1",` + fields + "}\n"
	}

	cases := []struct {
		name        string
		line        string
		wantIsEvent bool
		wantOK      bool
		check       func(t *testing.T, e evidence.Event)
	}{
		{
			name:        "an ordinary apt diagnostic line",
			line:        "E: Unable to locate package nosuchpkg\n",
			wantIsEvent: false,
		},
		{
			name:        "an empty line",
			line:        "\n",
			wantIsEvent: false,
		},
		{
			name:        "a pretty-printed --json envelope's opening brace",
			line:        "{\n",
			wantIsEvent: false,
		},
		{
			name:        "a pretty-printed --json envelope's body line",
			line:        "  \"backend\": \"container\",\n",
			wantIsEvent: false,
		},
		{
			name:        "JSON from something that is not debark",
			line:        `{"level":"info","msg":"docker says hello"}` + "\n",
			wantIsEvent: false,
		},
		{
			name:        "a future schema version",
			line:        `{"schema":"debark.events/v2","type":"apt.update"}` + "\n",
			wantIsEvent: false,
		},
		{
			name:        "the real apt.update event a container resolve emits",
			line:        ev(`"ts":"2026-09-07T15:06:45Z","type":"apt.update","msg":"apt-get update","attrs":{"failed":false,"entries":16,"sources_fetched":4,"sources_failed":0}`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if e.Type != evidence.TypeAPTUpdate {
					t.Errorf("Type = %q", e.Type)
				}
				if e.Msg != "apt-get update" {
					t.Errorf("Msg = %q", e.Msg)
				}
				if e.Attrs["failed"] != false || e.Attrs["entries"] != float64(16) {
					t.Errorf("attributes lost: %+v", e.Attrs)
				}
				if e.TS != "" {
					t.Errorf("TS = %q, want empty: the inner process's clock is not checkable and the host's sink stamps it", e.TS)
				}
				if e.Attrs[evidence.AttrForwardedFrom] != evidence.ForwardedFromContainer {
					t.Errorf("provenance not stamped: %+v", e.Attrs)
				}
			},
		},
		{
			name:        "an event type this build does not define",
			line:        ev(`"type":"apt.exfiltrate","msg":"trust me"`),
			wantIsEvent: true, wantOK: false,
		},
		{
			name:        "an empty type",
			line:        ev(`"type":"","msg":"x"`),
			wantIsEvent: true, wantOK: false,
		},
		{
			name:        "a level nobody defines",
			line:        ev(`"type":"warning","level":"critical","msg":"x"`),
			wantIsEvent: true, wantOK: false,
		},
		{
			name:        "each defined level survives",
			line:        ev(`"type":"warning","level":"error","msg":"x"`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if e.Level != evidence.LevelError {
					t.Errorf("Level = %q, want %q", e.Level, evidence.LevelError)
				}
			},
		},
		{
			name: "a terminal escape sequence in the message",
			// \u001b, not a raw ESC byte: raw control characters are not
			// legal inside a JSON string, so this is exactly how a producer
			// that wanted one would have to spell it.
			line:        ev(`"type":"apt.resolve","msg":"done\u001b[2K\u001b[31mFATAL: your bundle is compromised\u001b[0m"`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if strings.ContainsRune(e.Msg, 0x1b) {
					t.Errorf("Msg still carries ESC, which a terminal reads as a command and not as text: %q", e.Msg)
				}
				// Replaced, not removed: a message built to close up into a
				// shorter, cleaner-looking sentence must not be allowed to.
				if !strings.Contains(e.Msg, "FATAL") {
					t.Errorf("Msg lost the text around the escape, which is the part a reader needs to see: %q", e.Msg)
				}
			},
		},
		{
			name:        "a carriage return used to overwrite the line",
			line:        ev(`"type":"apt.resolve","msg":"ok\r                    \rEVERYTHING IS FINE"`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if strings.ContainsAny(e.Msg, "\r\n") {
					t.Errorf("Msg still carries CR/LF: %q", e.Msg)
				}
			},
		},
		{
			name:        "a nested attribute value",
			line:        ev(`"type":"apt.update","attrs":{"failed":false,"nested":{"a":{"b":{"c":1}}},"list":[1,2,3]}`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if _, ok := e.Attrs["nested"]; ok {
					t.Errorf("a nested object survived: %+v", e.Attrs)
				}
				if _, ok := e.Attrs["list"]; ok {
					t.Errorf("an array survived: %+v", e.Attrs)
				}
				if e.Attrs["failed"] != false {
					t.Errorf("the scalar beside them was lost: %+v", e.Attrs)
				}
			},
		},
		{
			name:        "an attribute key that is not a key",
			line:        ev(`"type":"apt.update","attrs":{"ok_key":1,"bad key":2,"\u001b[31m":3,"../../etc/passwd":4}`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if _, ok := e.Attrs["ok_key"]; !ok {
					t.Errorf("the good key was dropped: %+v", e.Attrs)
				}
				for _, bad := range []string{"bad key", "\x1b[31m", "../../etc/passwd"} {
					if _, ok := e.Attrs[bad]; ok {
						t.Errorf("key %q survived: %+v", bad, e.Attrs)
					}
				}
			},
		},
		{
			name:        "the inner process claims its own provenance",
			line:        ev(`"type":"apt.update","attrs":{"forwarded_from":"the host itself, honestly"}`),
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if got := e.Attrs[evidence.AttrForwardedFrom]; got != evidence.ForwardedFromContainer {
					t.Errorf("forwarded_from = %v: the container was allowed to describe where its own bytes came "+
						"from, and core/engine drops events on exactly this key", got)
				}
			},
		},
		{
			name:        "invalid UTF-8 in the message",
			line:        "{\"schema\":\"debark.events/v1\",\"type\":\"warning\",\"msg\":\"bad\xff\xfename\"}\n",
			wantIsEvent: true, wantOK: true,
			check: func(t *testing.T, e evidence.Event) {
				if !json.Valid(mustMarshal(t, e)) {
					t.Errorf("the forwarded event does not re-encode as valid JSON: %+v", e)
				}
			},
		},
		{
			name:        "trailing garbage after the object",
			line:        ev(`"type":"apt.update"`)[:len(ev(`"type":"apt.update"`))-1] + " not json\n",
			wantIsEvent: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, isEvent, ok := containerDecodeEventLine([]byte(tc.line))
			if isEvent != tc.wantIsEvent {
				t.Fatalf("isEvent = %v, want %v (line: %q)", isEvent, tc.wantIsEvent, tc.line)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (line: %q)", ok, tc.wantOK, tc.line)
			}
			if ok && tc.check != nil {
				tc.check(t, e)
			}
		})
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestContainerDecodeEventLineBoundsOneEvent covers the limits, each of which
// is a bound on what ONE untrusted process can make this one hold or draw.
func TestContainerDecodeEventLineBoundsOneEvent(t *testing.T) {
	t.Run("a very long message is capped", func(t *testing.T) {
		long := strings.Repeat("A", 10000)
		line := `{"schema":"debark.events/v1","type":"warning","msg":"` + long + `"}`
		e, isEvent, ok := containerDecodeEventLine([]byte(line))
		if !isEvent || !ok {
			t.Fatalf("isEvent=%v ok=%v", isEvent, ok)
		}
		if n := len([]rune(e.Msg)); n > containerEventMsgMax+3 {
			t.Errorf("Msg is %d runes, want at most %d plus an ellipsis", n, containerEventMsgMax)
		}
	})

	t.Run("a very long attribute value is capped", func(t *testing.T) {
		long := strings.Repeat("B", 10000)
		line := `{"schema":"debark.events/v1","type":"warning","attrs":{"detail":"` + long + `"}}`
		e, _, ok := containerDecodeEventLine([]byte(line))
		if !ok {
			t.Fatal("rejected")
		}
		if n := len([]rune(e.Attrs["detail"].(string))); n > containerEventValueMax+3 {
			t.Errorf("attribute is %d runes, want at most %d plus an ellipsis", n, containerEventValueMax)
		}
	})

	t.Run("too many attributes are capped", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"schema":"debark.events/v1","type":"warning","attrs":{`)
		for i := 0; i < 500; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"k%03d":%d`, i, i)
		}
		b.WriteString("}}")
		e, _, ok := containerDecodeEventLine([]byte(b.String()))
		if !ok {
			t.Fatal("rejected")
		}
		// containerEventAttrsMax from the inner process, plus the one the
		// host stamps.
		if len(e.Attrs) > containerEventAttrsMax+1 {
			t.Errorf("kept %d attributes, want at most %d", len(e.Attrs), containerEventAttrsMax+1)
		}
		// Which ones survive must be a property of the input, not of Go's
		// map iteration order, or two runs of one build disagree.
		for i := 0; i < 5; i++ {
			if _, ok := e.Attrs[fmt.Sprintf("k%03d", i)]; !ok {
				t.Errorf("k%03d did not survive; the cap is not applied in a stable order", i)
			}
		}
	})

	t.Run("an over-long key is dropped", func(t *testing.T) {
		line := `{"schema":"debark.events/v1","type":"warning","attrs":{"` + strings.Repeat("k", 500) + `":1,"fine":2}}`
		e, _, ok := containerDecodeEventLine([]byte(line))
		if !ok {
			t.Fatal("rejected")
		}
		if len(e.Attrs) != 2 { // "fine" plus the host's stamp
			t.Errorf("Attrs = %+v, want only the short key and the provenance stamp", e.Attrs)
		}
	})
}

// --- the stream splitter ----------------------------------------------------

// newTestEventStream returns a splitter and the two things a caller inspects:
// the diagnostic buffer that becomes containerRunResult.Stderr, and the
// events that reached the host.
func newTestEventStream() (*containerEventStream, *bytes.Buffer, *[]evidence.Event) {
	var diag bytes.Buffer
	var got []evidence.Event
	sink := &containerEventSink{emit: func(e evidence.Event) { got = append(got, e) }}
	return &containerEventStream{diag: &diag, sink: sink}, &diag, &got
}

// TestContainerEventStreamSplitsOutputArrivingInArbitraryChunks is the
// property os/exec actually imposes: it copies the container's stream in
// whatever sizes the pipe hands it, so a line arrives split at any byte,
// including the middle of a UTF-8 sequence and the middle of a JSON string.
//
// Driven at every possible split point rather than at a few chosen ones,
// because "works when the chunk boundary happens to fall on a newline" is
// exactly the bug this shape produces.
func TestContainerEventStreamSplitsOutputArrivingInArbitraryChunks(t *testing.T) {
	const stream = "Get:1 http://archive.ubuntu.com/ubuntu noble InRelease\n" +
		`{"schema":"debark.events/v1","type":"apt.update","msg":"apt-get update — 16 entries","attrs":{"failed":false}}` + "\n" +
		"Reading package lists...\n" +
		`{"schema":"debark.events/v1","type":"apt.resolve","msg":"apt resolve complete","attrs":{"selections":3}}` + "\n" +
		"E: Sub-process returned an error code\n"

	for split := 1; split < len(stream); split++ {
		w, diag, got := newTestEventStream()
		if _, err := w.Write([]byte(stream[:split])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := w.Write([]byte(stream[split:])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		w.Close()

		if len(*got) != 2 {
			t.Fatalf("split at %d: forwarded %d events, want 2", split, len(*got))
		}
		if (*got)[0].Type != evidence.TypeAPTUpdate || (*got)[1].Type != evidence.TypeAPTResolve {
			t.Fatalf("split at %d: events out of order or wrong: %+v", split, *got)
		}
		if (*got)[0].Attrs["failed"] != false {
			t.Fatalf("split at %d: attribute lost: %+v", split, (*got)[0].Attrs)
		}
		wantDiag := "Get:1 http://archive.ubuntu.com/ubuntu noble InRelease\n" +
			"Reading package lists...\n" +
			"E: Sub-process returned an error code\n"
		if diag.String() != wantDiag {
			t.Fatalf("split at %d: diagnostics = %q, want %q", split, diag.String(), wantDiag)
		}
	}
}

// TestContainerEventStreamKeepsAptsFailureVisible is why event lines are
// removed from the diagnostic buffer rather than merely copied out of it.
//
// containerLastLines shows the LAST six lines of the captured output in four
// error paths. With `--json-events -` on the inner argv and no separation, an
// inner failure would report six event objects and apt's actual message would
// be off the top.
func TestContainerEventStreamKeepsAptsFailureVisible(t *testing.T) {
	w, diag, got := newTestEventStream()
	var b strings.Builder
	b.WriteString("E: Unable to locate package nosuchpkg\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, `{"schema":"debark.events/v1","type":"progress","attrs":{"bytes":%d}}`+"\n", i)
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	w.Close()

	if len(*got) != 20 {
		t.Errorf("forwarded %d events, want 20", len(*got))
	}
	tail := containerLastLines(diag.Bytes(), 6)
	if !strings.Contains(tail, "Unable to locate package nosuchpkg") {
		t.Errorf("apt's own failure is no longer in the last six lines an error message shows: %q", tail)
	}
}

// TestContainerEventStreamRefusesAnUnboundedLine covers the case where the
// inner process never writes a newline at all: the splitter must not grow its
// buffer without limit, and the bytes must still reach the diagnostics.
func TestContainerEventStreamRefusesAnUnboundedLine(t *testing.T) {
	w, diag, got := newTestEventStream()
	chunk := strings.Repeat("x", 8<<10)
	for i := 0; i < 64; i++ { // 512 KiB with no newline
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if len(w.pending) > containerEventLineMax {
			t.Fatalf("pending grew to %d bytes, past the %d cap", len(w.pending), containerEventLineMax)
		}
	}
	// The line finally ends, and the next real event is still read.
	if _, err := w.Write([]byte("\n" + `{"schema":"debark.events/v1","type":"apt.resolve"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	if len(*got) != 1 {
		t.Errorf("forwarded %d events, want 1: the splitter did not recover after the over-long line", len(*got))
	}
	if diag.Len() < 64*len(chunk) {
		t.Errorf("diagnostics hold %d bytes, want at least the %d that were written: output was lost, not just refused as an event",
			diag.Len(), 64*len(chunk))
	}
}

// TestContainerEventStreamCapsAndCountsWhatItRefused pins the cap and the
// summary that makes it visible. Dropping silently is how a validator becomes
// invisible: the events it refused never arrive, so nothing else can say so.
func TestContainerEventStreamCapsAndCountsWhatItRefused(t *testing.T) {
	w, _, got := newTestEventStream()
	var b strings.Builder
	for i := 0; i < containerEventsMax+250; i++ {
		b.WriteString(`{"schema":"debark.events/v1","type":"apt.update"}` + "\n")
	}
	// Plus some that claim to be events and are not believable.
	for i := 0; i < 7; i++ {
		b.WriteString(`{"schema":"debark.events/v1","type":"apt.exfiltrate"}` + "\n")
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	w.Close()

	if len(*got) != containerEventsMax {
		t.Errorf("forwarded %d events, want the cap of %d", len(*got), containerEventsMax)
	}
	counts := w.sink.counts()
	if counts.Forwarded != containerEventsMax || counts.Dropped != 250 || counts.Rejected != 7 {
		t.Errorf("counts = %+v, want forwarded=%d dropped=250 rejected=7", counts, containerEventsMax)
	}

	var summary []evidence.Event
	containerEmitForwardSummary(sinkFunc(func(e evidence.Event) { summary = append(summary, e) }), counts)
	if len(summary) != 1 {
		t.Fatalf("the host emitted %d summary events, want 1", len(summary))
	}
	if summary[0].Level != evidence.LevelWarn {
		t.Errorf("summary level = %q, want warn", summary[0].Level)
	}
	if summary[0].Attrs["rejected"] != 7 || summary[0].Attrs["dropped_over_limit"] != 250 {
		t.Errorf("summary does not say what was refused: %+v", summary[0].Attrs)
	}
	// The summary is about a stream that never enters the bundle, so it must
	// not enter it either.
	if summary[0].Attrs[evidence.AttrForwardedFrom] != evidence.ForwardedFromContainer {
		t.Errorf("the summary is not marked as belonging to the forwarded stream: %+v", summary[0].Attrs)
	}

	// And nothing at all when there was nothing to report.
	summary = nil
	containerEmitForwardSummary(sinkFunc(func(e evidence.Event) { summary = append(summary, e) }),
		containerEventCounts{Forwarded: 4})
	if len(summary) != 0 {
		t.Errorf("a clean run emitted a summary: %+v", summary)
	}
}

// sinkFunc adapts a function to evidence.Sink.
type sinkFunc func(evidence.Event)

func (f sinkFunc) Emit(e evidence.Event) { f(e) }
func (sinkFunc) Close() error            { return nil }

// --- the two callers that forward, and the one that must not ----------------

// TestContainerResolveForwardsTheInnerProcessesEvents drives the whole thing
// through Backend.Resolve: the inner argv must ask for the stream, and every
// event the inner process writes must reach this backend's sink.
//
// The fake writes its events THROUGH the onEvent hook rather than returning
// them in the result, which is the difference the whole change is about: a
// 61 s build that reports four events when it finishes is indistinguishable
// from one that reports nothing.
func TestContainerResolveForwardsTheInnerProcessesEvents(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	root := t.TempDir()
	archivesDir := filepath.Join(root, "archives")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDeb := []byte("fake .deb bytes for the forwarding test")
	env := containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "jq", Arch: "amd64", Version: "1.7.1", Filename: "jq_1.7.1_amd64.deb",
				SHA256: digest.Bytes(fakeDeb), Size: int64(len(fakeDeb)), Reason: lock.ReasonRequested},
		}},
		Files: []containerEnvelopeFile{
			{Name: "jq", Arch: "amd64", Version: "1.7.1", Filename: "jq_1.7.1_amd64.deb",
				SHA256: digest.Bytes(fakeDeb), Size: int64(len(fakeDeb))},
		},
	}
	if err := os.WriteFile(filepath.Join(archivesDir, "jq_1.7.1_amd64.deb"), fakeDeb, 0o644); err != nil {
		t.Fatal(err)
	}

	var argv []string
	var runs int
	inner := fakeContainerPlanRun(t, fx.workDir, env, &argv)
	execContainerFn = func(ctx context.Context, runtimePath string, a []string, onEvent func(evidence.Event)) (containerRunResult, error) {
		runs++
		if runs > 1 {
			// On Windows and macOS /archives is a named volume, so Resolve
			// makes a SECOND container run to copy it to the host
			// (containerCopyVolumeToHost). That one runs `sh -c cp`, not
			// debark, and correctly carries no hook.
			return containerRunResult{Argv: append([]string{runtimePath}, a...)}, nil
		}
		if onEvent == nil {
			t.Fatal("Resolve did not pass an event hook, so nothing from inside the container can ever cross back")
		}
		// What the inner `resolve --backend local` really emits, in order.
		for _, e := range []evidence.Event{
			{Type: evidence.TypeSnapshotLoaded, Msg: "snapshot loaded"},
			{Type: evidence.TypeBackendSelected, Msg: "backend selected", Attrs: map[string]any{"backend": "local"}},
			{Type: evidence.TypeAPTUpdate, Msg: "apt-get update", Attrs: map[string]any{"failed": false}},
			{Type: evidence.TypeAPTResolve, Msg: "apt resolve complete", Attrs: map[string]any{"selections": float64(1)}},
		} {
			e.Attrs = withForwardMark(e.Attrs)
			onEvent(e)
		}
		return inner(ctx, runtimePath, a, nil)
	}

	var seen []evidence.Event
	b := newContainerBackend(ContainerOptions{
		SelfPath: fx.selfPath,
		Events:   sinkFunc(func(e evidence.Event) { seen = append(seen, e) }),
	})
	if _, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if !containsFlagValue(argv, "--json-events", "-") {
		t.Errorf("the inner argv does not ask for the event stream; argv=%v", argv)
	}
	want := map[string]bool{
		evidence.TypeSnapshotLoaded: false,
		evidence.TypeAPTUpdate:      false,
		evidence.TypeAPTResolve:     false,
	}
	for _, e := range seen {
		if _, interesting := want[e.Type]; interesting {
			want[e.Type] = true
		}
	}
	for typ, got := range want {
		if !got {
			t.Errorf("%s never reached this backend's sink; the phase information the GUI needs is still trapped "+
				"inside the container", typ)
		}
	}
}

// withForwardMark stamps the attribute the host stamps, so the fake produces
// what execContainer's real path produces.
func withForwardMark(attrs map[string]any) map[string]any {
	out := map[string]any{evidence.AttrForwardedFrom: evidence.ForwardedFromContainer}
	for k, v := range attrs {
		out[k] = v
	}
	return out
}

// containsFlagValue reports whether argv carries `flag value` adjacently.
func containsFlagValue(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// TestContainerClosedWorldDoesNotForwardAndItsOutputIsUnchanged is the half
// that must not move, and it is not an oversight.
//
// ClosedWorld's captured output is digested into lock.ClosedWorld, which
// reaches lock.json, the lock digest, the bundle id and the signature. Adding
// `--json-events -` there would put a second process's narration — wall-clock
// timestamps included — into that digest, which is the identical defect the
// CommandDigest note in container.go records for the raw docker argv. A
// closed-world check also has no progress to report.
func TestContainerClosedWorldDoesNotForwardAndItsOutputIsUnchanged(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	lk := &lock.Lock{SchemaVersion: lock.SchemaVersion, Packages: []lock.Package{
		{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
	}}
	repoDir := writeClosedWorldBundle(t, lk)

	var argv []string
	var hookWasPassed bool
	inner := closedWorldFakeRun(t, fx, repoDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "demo-app", Arch: "amd64", Version: "1.0-1", Reason: lock.ReasonRequested},
		}},
	}, 0, "")
	execContainerFn = func(ctx context.Context, runtimePath string, a []string, onEvent func(evidence.Event)) (containerRunResult, error) {
		argv = a
		hookWasPassed = onEvent != nil
		return inner(ctx, runtimePath, a, onEvent)
	}

	b := newContainerBackend(ContainerOptions{
		SelfPath: fx.selfPath,
		Events:   sinkFunc(func(evidence.Event) {}),
	})
	if _, err := b.ClosedWorld(context.Background(), ClosedWorldInput{
		Snapshot: fx.snap, SnapshotFilesDir: fx.filesDir,
		BundleRepoDir: repoDir, Install: []string{"demo-app:amd64=1.0-1"}, WorkDir: fx.workDir,
	}); err != nil {
		t.Fatalf("ClosedWorld: %v", err)
	}

	if hookWasPassed {
		t.Error("ClosedWorld passed an event hook, so its captured output is now filtered — and that output is " +
			"digested into lock.json and covered by the signature")
	}
	for _, a := range argv {
		if a == "--json-events" {
			t.Errorf("ClosedWorld's inner argv asks for the event stream; argv=%v", argv)
		}
	}
}

// TestBaseSnapshotInContainerForwardsTheInnerProcessesEvents is the same
// again for `snapshot from-base`, which basecontainer.go runs in a container
// and which resolves a whole seed closure inside with nothing crossing back.
// Measured before this change: a 19 s run emitted two events, both host-side.
//
// Its "-" is stdout rather than stderr, because that command has no envelope
// on stdout to protect and uses the ordinary sink. execContainer watches both
// streams so neither caller has to encode that difference.
func TestBaseSnapshotInContainerForwardsTheInnerProcessesEvents(t *testing.T) {
	fx := setupBaseContainerFixture(t)

	var argv []string
	inner := fakeFromBaseRun(t, filepath.Join(fx.workDir, fx.outName), 0, "", &argv)
	execContainerFn = func(ctx context.Context, runtimePath string, a []string, onEvent func(evidence.Event)) (containerRunResult, error) {
		if onEvent == nil {
			t.Fatal("BaseSnapshotInContainer did not pass an event hook")
		}
		onEvent(evidence.Event{Type: evidence.TypeAPTResolve, Msg: "apt resolve complete",
			Attrs: withForwardMark(map[string]any{"selections": float64(412)})})
		return inner(ctx, runtimePath, a, nil)
	}

	var seen []evidence.Event
	in := fx.input()
	in.Events = sinkFunc(func(e evidence.Event) { seen = append(seen, e) })
	if _, err := BaseSnapshotInContainer(context.Background(), in); err != nil {
		t.Fatalf("BaseSnapshotInContainer: %v", err)
	}

	if !containsFlagValue(argv, "--json-events", "-") {
		t.Errorf("the inner argv does not ask for the event stream; argv=%v", argv)
	}
	var found bool
	for _, e := range seen {
		if e.Type == evidence.TypeAPTResolve && e.Attrs["selections"] == float64(412) {
			found = true
		}
	}
	if !found {
		t.Errorf("the inner from-base's apt.resolve never reached the caller's sink: %+v", seen)
	}
}

// TestContainerForwardEventToIsNilWithoutASink pins the property every
// "unchanged" claim above rests on: with no sink there is no hook, and with
// no hook execContainer takes the path it took before this file existed.
func TestContainerForwardEventToIsNilWithoutASink(t *testing.T) {
	if containerForwardEventTo(nil) != nil {
		t.Error("a backend with no event sink was given a forwarding hook, which changes its captured output for nothing")
	}
	if containerForwardEventTo(sinkFunc(func(evidence.Event) {})) == nil {
		t.Error("a backend with an event sink was given no hook")
	}
	// And the argv follows the hook, not the other way round.
	withEvents := containerBuildInnerArgv(containerResolveArgs{Arch: "amd64", JSONEvents: true})
	without := containerBuildInnerArgv(containerResolveArgs{Arch: "amd64"})
	if !containsFlagValue(withEvents, "--json-events", "-") {
		t.Errorf("JSONEvents did not add the flag: %v", withEvents)
	}
	for _, a := range without {
		if a == "--json-events" {
			t.Errorf("the flag appeared without JSONEvents: %v", without)
		}
	}
	// The flag order the resolve contract fixes: output flags last, --json
	// last of all.
	if withEvents[len(withEvents)-1] != "--json" {
		t.Errorf("--json is no longer the final flag: %v", withEvents)
	}
}

var _ = snapshot.Target{}
