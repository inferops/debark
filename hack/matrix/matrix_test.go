package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/test/e2e/harness"
)

// ---------------------------------------------------------------------------
// Row classification: a row the harness killed is never a product verdict
// ---------------------------------------------------------------------------

// killedRow is the shape of a row the 2026-09-03 run actually produced: the
// harness killed the build at --row-timeout=12m and recorded the killed
// process's exit status as though debark had chosen it. Reproducing that
// row here is how the timeout status is proven without waiting 720 seconds
// for a real one.
//
// TimedOut is what RunFixture stamps on such a row today, reading its own
// context the instant execute() returns and before teardown. It is the whole
// input to the classification: fixing it at true here, and at false in
// concludedRow below, is what makes the two cases distinguishable in a unit
// test at all — the pre-fix code could only see the context error, which by
// the time classifyRow reads it says the same thing for both.
func killedRow() harness.RowResult {
	return harness.RowResult{
		Fixture:  "vendor-deb-ingest",
		Distro:   "debian",
		Version:  "12",
		Arch:     "amd64",
		Status:   harness.StatusFail,
		Stage:    "build",
		Blocker:  "exit 1 (usage), want 0 (success): ",
		TotalMS:  721096,
		TimedOut: true,
	}
}

// concludedRow is the case the TimedOut field exists to protect: debark
// reached a real verdict — a verification failure, the most valuable kind of
// row in a result file — a few seconds inside the budget, and the row's
// container teardown then ran past the deadline. RunFixture read its context
// before teardown and saw nothing, so TimedOut is false; the per-row context
// classifyRow is handed afterwards has since latched DeadlineExceeded.
func concludedRow() harness.RowResult {
	return harness.RowResult{
		Fixture:  "tamper-modified-deb",
		Distro:   "debian",
		Version:  "12",
		Arch:     "amd64",
		Status:   harness.StatusFail,
		Stage:    "verify",
		Blocker:  `exit 0 (success), want 6 (verification): manifest digest mismatch not reported`,
		TotalMS:  719400,
		TimedOut: false,
	}
}

func TestClassifyRowTimeoutReplacesTheFakeUsageError(t *testing.T) {
	got := classifyRow(killedRow(), context.DeadlineExceeded, 12*time.Minute)

	if got.Status != StatusTimeout {
		t.Fatalf("status = %q, want %q", got.Status, StatusTimeout)
	}
	if string(StatusTimeout) != "timeout" {
		t.Errorf("StatusTimeout = %q, want the JSON value %q", StatusTimeout, "timeout")
	}
	// The headline must be the timeout, not the exit status of the process
	// the harness itself killed.
	if !strings.HasPrefix(got.Blocker, "row timeout:") {
		t.Errorf("blocker should lead with the timeout, got %q", got.Blocker)
	}
	// It must carry how long the row ran and how far it got.
	for _, want := range []string{"--row-timeout=12m0s", "721.1s", `"build"`, "never reached a verdict"} {
		if !strings.Contains(got.Blocker, want) {
			t.Errorf("blocker %q missing %q", got.Blocker, want)
		}
	}
	// The original text is kept, demoted rather than discarded.
	if !strings.Contains(got.Blocker, "exit 1 (usage), want 0 (success)") {
		t.Errorf("blocker dropped the row's own last message: %q", got.Blocker)
	}
	// Stage and duration stay on the row, where the JSON reader looks.
	if got.Stage != "build" || got.TotalMS != 721096 {
		t.Errorf("stage/total changed: %q %d", got.Stage, got.TotalMS)
	}
}

func TestClassifyRowAbortedBySignal(t *testing.T) {
	row := killedRow()
	row.TotalMS = 12400
	got := classifyRow(row, context.Canceled, 12*time.Minute)

	if got.Status != StatusAborted {
		t.Fatalf("status = %q, want %q", got.Status, StatusAborted)
	}
	if string(StatusAborted) != "aborted" {
		t.Errorf("StatusAborted = %q, want the JSON value %q", StatusAborted, "aborted")
	}
	for _, want := range []string{"run aborted:", "12.4s", `"build"`} {
		if !strings.Contains(got.Blocker, want) {
			t.Errorf("blocker %q missing %q", got.Blocker, want)
		}
	}
}

func TestClassifyRowLeavesRealVerdictsAlone(t *testing.T) {
	t.Run("row finished on its own", func(t *testing.T) {
		in := concludedRow()
		got := classifyRow(in, nil, 12*time.Minute)
		if got.Status != in.Status || got.Blocker != in.Blocker {
			t.Fatalf("a row that ended by itself was re-labelled: %q / %q", got.Status, got.Blocker)
		}
	})
	t.Run("passing row whose teardown outlived the deadline", func(t *testing.T) {
		// RunFixture reports pass only when every stage completed, so a
		// deadline that expires afterwards (during container teardown) is
		// not a timeout of the row and must not be reported as one. The
		// exemption is checked ahead of TimedOut, so it holds even on a row
		// carrying the field — which is why this starts from killedRow().
		in := killedRow()
		in.Status = harness.StatusPass
		in.Stage = "done"
		in.Blocker = ""
		got := classifyRow(in, context.DeadlineExceeded, 12*time.Minute)
		if got.Status != harness.StatusPass || got.Blocker != "" {
			t.Fatalf("a passing row was re-labelled %q: %q", got.Status, got.Blocker)
		}
	})
	t.Run("unknown context error on a row that concluded", func(t *testing.T) {
		in := concludedRow()
		got := classifyRow(in, errors.New("something else"), time.Minute)
		if got.Status != in.Status {
			t.Fatalf("status = %q, want it untouched (%q)", got.Status, in.Status)
		}
	})
}

// TestClassifyRowKeepsAVerdictReachedBeforeTheDeadline is the regression test
// for the mislabelling RowResult.TimedOut removed. The row below reached a
// real verification failure inside its budget and only its container teardown
// crossed --row-timeout, so the per-row context classifyRow reads afterwards
// says DeadlineExceeded — indistinguishable, from out here, from a row the
// harness killed mid-build.
//
// Getting this backwards is not a cosmetic relabel: it moves the row out of
// the fail bucket, which makes the process exit 3 ("no verdict, check the
// host") instead of 1 ("the product failed"), and buries the row's own
// blocker under a timeout message. The one row a reader most needs is the one
// that disappears.
func TestClassifyRowKeepsAVerdictReachedBeforeTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctxErr error
	}{
		{"deadline crossed during teardown", context.DeadlineExceeded},
		{"signal arrived during teardown", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := concludedRow()
			got := classifyRow(in, tc.ctxErr, 12*time.Minute)

			if got.Status != harness.StatusFail {
				t.Fatalf("status = %q, want %q: the row reached a verdict before the deadline and RunFixture said so (TimedOut=false)",
					got.Status, harness.StatusFail)
			}
			if got.Blocker != in.Blocker {
				t.Errorf("blocker rewritten to %q, want the row's own verdict %q", got.Blocker, in.Blocker)
			}
			// The counters a nightly job keys on must follow: a fail row is
			// exit 1, a timeout row is exit 3.
			s := summarize([]harness.RowResult{got})
			if s.Fail != 1 || s.Timeout != 0 || s.Aborted != 0 {
				t.Errorf("summary = %+v, want the row counted as a product failure", s)
			}
		})
	}
}

// TestClassifyRowStopsAKilledRowEvenWithoutAContextLabel covers the other
// direction of the same split. row.TimedOut says the harness ended the row;
// the context error only names WHICH ending. Before the field existed, an
// unrecognised error meant no evidence and the row was returned untouched —
// which for a killed row means publishing the exit status of a process the
// harness itself killed, the exact 2026-09-03 mislabelling. With the evidence
// on the row, the missing label costs a precise message, not the verdict.
func TestClassifyRowStopsAKilledRowEvenWithoutAContextLabel(t *testing.T) {
	for _, ctxErr := range []error{nil, errors.New("something else")} {
		got := classifyRow(killedRow(), ctxErr, 12*time.Minute)
		if got.Status != StatusTimeout {
			t.Errorf("classifyRow(killed row, %v) status = %q, want %q — a row the harness killed must never be published as a product verdict",
				ctxErr, got.Status, StatusTimeout)
		}
		if strings.HasPrefix(got.Blocker, "exit 1 (usage)") {
			t.Errorf("blocker still leads with the killed process's exit status: %q", got.Blocker)
		}
	}
}

func TestClassifyRowWithoutAStage(t *testing.T) {
	in := killedRow()
	in.Stage = ""
	got := classifyRow(in, context.DeadlineExceeded, time.Minute)
	if !strings.Contains(got.Blocker, `"unknown"`) {
		t.Fatalf("a stage-less row should say so: %q", got.Blocker)
	}
}

// ---------------------------------------------------------------------------
// Summary and JSON schema: additive, and every row still accounted for
// ---------------------------------------------------------------------------

func TestSummarizeAccountsForEveryRow(t *testing.T) {
	rows := []harness.RowResult{
		{Status: harness.StatusPass},
		{Status: harness.StatusFail},
		{Status: harness.StatusBlocked},
		{Status: harness.StatusBlocked},
		{Status: harness.StatusSkipped},
		{Status: StatusTimeout},
		{Status: StatusTimeout},
		{Status: StatusTimeout},
		{Status: StatusAborted},
	}
	s := summarize(rows)
	want := summaryCounts{Total: 9, Pass: 1, Fail: 1, Blocked: 2, Skipped: 1, Timeout: 3, Aborted: 1}
	if s != want {
		t.Fatalf("summarize() = %+v, want %+v", s, want)
	}
	if sum := s.Pass + s.Fail + s.Blocked + s.Skipped + s.Timeout + s.Aborted; sum != s.Total {
		t.Fatalf("total %d != sum of buckets %d — the invariant a reader checks", s.Total, sum)
	}
}

func TestResultDocIsAdditiveToSchemaV1(t *testing.T) {
	doc := resultDoc{
		MatrixResult: harness.MatrixResult{
			SchemaVersion: harness.ResultSchemaVersion,
			GeneratedAt:   time.Now().UTC(),
			Rows:          []harness.RowResult{classifyRow(killedRow(), context.DeadlineExceeded, 12*time.Minute)},
		},
	}
	doc.Summary = summarize(doc.Rows)

	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Same schema version: new status values and new summary keys are
	// additive, so a v1 reader is still a valid reader.
	var version string
	if err := json.Unmarshal(got["schema_version"], &version); err != nil {
		t.Fatal(err)
	}
	if version != "debark.e2e.matrixresult/v1" {
		t.Errorf("schema_version = %q, want the unchanged v1", version)
	}
	for _, key := range []string{"schema_version", "generated_at", "host", "config", "rows", "summary"} {
		if _, ok := got[key]; !ok {
			t.Errorf("v1 top-level key %q went missing", key)
		}
	}
	// Embedding harness.MatrixResult must not produce two "summary" keys:
	// the shallower field wins, and the count is what a text scan sees.
	if n := strings.Count(string(b), `"summary"`); n != 1 {
		t.Fatalf("found %d \"summary\" keys in the document, want exactly 1: %s", n, b)
	}

	var summary map[string]int
	if err := json.Unmarshal(got["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"total", "pass", "fail", "blocked", "skipped", "timeout", "aborted"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary key %q missing", key)
		}
	}
	if summary["timeout"] != 1 || summary["total"] != 1 {
		t.Errorf("summary = %v, want one timed-out row", summary)
	}

	// A pre-existing reader that only knows harness.Summary still decodes.
	var old struct {
		Summary harness.Summary `json:"summary"`
		Rows    []struct {
			Status string `json:"status"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("a v1-only reader can no longer parse the document: %v", err)
	}
	if old.Summary.Total != 1 {
		t.Errorf("v1 reader sees total %d, want 1", old.Summary.Total)
	}
	if old.Rows[0].Status != "timeout" {
		t.Errorf("row status in JSON = %q, want %q", old.Rows[0].Status, "timeout")
	}
}

// TestTimedOutIsAdditiveToTheRowSchema pins the other half of the additivity
// claim: "timed_out" is a new key on an existing object, and omitempty keeps
// it off every row that finished normally — so a v1 reader's row objects are
// byte-for-byte what they were except on the rows the harness stopped, which
// are the rows it needs to treat differently anyway.
func TestTimedOutIsAdditiveToTheRowSchema(t *testing.T) {
	rowKeys := func(t *testing.T, row harness.RowResult) map[string]json.RawMessage {
		t.Helper()
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return got
	}

	if _, present := rowKeys(t, concludedRow())["timed_out"]; present {
		t.Error(`a row that reached its own verdict carries a "timed_out" key; omitempty should keep it out entirely`)
	}
	raw, present := rowKeys(t, killedRow())["timed_out"]
	if !present {
		t.Fatal(`a row the harness killed has no "timed_out" key — the fact classifyRow keys on never reaches the result file`)
	}
	if string(raw) != "true" {
		t.Errorf(`"timed_out" = %s, want true`, raw)
	}
}

// ---------------------------------------------------------------------------
// Cleanup scoping: the sweep must never reach another project's containers
// ---------------------------------------------------------------------------

// dockerLabelFilterMatches models `docker ps --filter label=key=value`
// against one container's labels, so the filter this runner builds can be
// checked against containers it must never match without starting any.
func dockerLabelFilterMatches(filter string, labels map[string]string) bool {
	expr, ok := strings.CutPrefix(filter, "label=")
	if !ok {
		return false
	}
	key, want, hasValue := strings.Cut(expr, "=")
	got, present := labels[key]
	if !hasValue {
		return present
	}
	return present && got == want
}

func TestRunFilterOnlyMatchesThisRun(t *testing.T) {
	const runID = "rdeadbeef"
	filter, err := runFilter(runID)
	if err != nil {
		t.Fatalf("runFilter: %v", err)
	}
	if want := "label=debark.e2e.run=rdeadbeef"; filter != want {
		t.Fatalf("filter = %q, want %q", filter, want)
	}
	// It must be the label harness.Sweep itself filters on, or the sweep and
	// the message describing it would drift apart.
	if !strings.Contains(filter, harness.LabelRun) {
		t.Fatalf("filter %q does not use harness.LabelRun (%q)", filter, harness.LabelRun)
	}

	ourLabels := map[string]string{}
	for _, l := range runLabels(runID) {
		k, v, _ := strings.Cut(l, "=")
		ourLabels[k] = v
	}
	if !dockerLabelFilterMatches(filter, ourLabels) {
		t.Errorf("filter %q does not match this run's own container labels %v", filter, ourLabels)
	}

	// Everything else on a busy shared host must be untouched.
	for name, labels := range map[string]map[string]string{
		"unlabelled container":             {},
		"someone else's project":           {"com.docker.compose.project": "unrelated-app"},
		"another debark run":               {harness.LabelMarker: "1", harness.LabelRun: "rcafebabe"},
		"debark-marked, no run id":         {harness.LabelMarker: "1"},
		"run label present but empty":      {harness.LabelRun: ""},
		"run id as a prefix of ours":       {harness.LabelRun: "rdeadbee"},
		"run id with ours as a prefix":     {harness.LabelRun: "rdeadbeefcafe"},
		"different key, same value":        {"some.other.label": runID},
		"marker on an unrelated project":   {"debark.e2e.something": runID},
		"empty labels map with nil values": nil,
	} {
		if dockerLabelFilterMatches(filter, labels) {
			t.Errorf("filter %q would have removed a %s (%v)", filter, name, labels)
		}
	}
}

func TestRunFilterRefusesAnUnscopedSweep(t *testing.T) {
	for _, runID := range []string{"", "   ", "\t\n"} {
		if _, err := runFilter(runID); err == nil {
			t.Errorf("runFilter(%q) returned no error: an empty run id must never become a filter", runID)
		}
	}
}

func TestSweepRunRefusesEmptyRunIDWithoutCallingDocker(t *testing.T) {
	var sb strings.Builder
	sweepRun("", &sb)
	if !strings.Contains(sb.String(), "empty run id") {
		t.Fatalf("sweepRun(\"\") said %q, want a refusal mentioning the empty run id", sb.String())
	}
}

func TestRunLabelsCarryBothMarkerAndRunID(t *testing.T) {
	got := runLabels("r1234")
	want := []string{"debark.e2e=1", "debark.e2e.run=r1234"}
	if len(got) != len(want) {
		t.Fatalf("runLabels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runLabels = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Signals: a SIGTERM cleans up instead of orphaning containers
// ---------------------------------------------------------------------------

func TestInterruptGuardCancelsAndSweepsBeforeExiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cleanups atomic.Int32
	exited := make(chan int, 1)
	g := &interruptGuard{
		stderr:  &strings.Builder{},
		runID:   "r1234",
		cancel:  cancel,
		cleanup: func() { cleanups.Add(1) },
		grace:   20 * time.Millisecond,
		exit:    func(code int) { exited <- code },
	}

	sig := make(chan os.Signal, 2)
	done := make(chan struct{})
	go g.watch(sig, done)
	sig <- syscall.SIGTERM

	select {
	case code := <-exited:
		if code != exitInterrupted {
			t.Errorf("exit code = %d, want %d", code, exitInterrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guard never exited after SIGTERM")
	}
	if ctx.Err() == nil {
		t.Error("the run context was not cancelled, so in-flight rows would keep running")
	}
	if n := cleanups.Load(); n != 1 {
		t.Errorf("cleanup ran %d time(s), want exactly 1 — a signal must sweep this run's containers", n)
	}
	if !g.tripped.Load() {
		t.Error("tripped was not set, so run() would not report the interrupted exit code")
	}
}

func TestInterruptGuardSweepsImmediatelyOnASecondSignal(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cleanups atomic.Int32
	exited := make(chan int, 1)
	g := &interruptGuard{
		stderr: &strings.Builder{},
		cancel: cancel,
		// A grace long enough that only the second signal can end this test
		// in time.
		cleanup: func() { cleanups.Add(1) },
		grace:   10 * time.Minute,
		exit:    func(code int) { exited <- code },
	}

	sig := make(chan os.Signal, 2)
	go g.watch(sig, make(chan struct{}))
	sig <- syscall.SIGTERM
	sig <- syscall.SIGINT

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("a second signal did not short-circuit the grace period")
	}
	if n := cleanups.Load(); n != 1 {
		t.Errorf("cleanup ran %d time(s), want exactly 1", n)
	}
}

func TestInterruptGuardDoesNothingWhenTheRunFinishes(t *testing.T) {
	var cleanups atomic.Int32
	g := &interruptGuard{
		stderr:  &strings.Builder{},
		cancel:  func() {},
		cleanup: func() { cleanups.Add(1) },
		grace:   time.Minute,
		exit:    func(int) { t.Error("exit called on the normal path") },
	}

	done := make(chan struct{})
	returned := make(chan struct{})
	go func() { g.watch(make(chan os.Signal), done); close(returned) }()
	close(done)

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not return once the run finished")
	}
	if n := cleanups.Load(); n != 0 {
		t.Errorf("cleanup ran %d time(s) on the normal path, want 0 (run()'s own defer owns it there)", n)
	}
	if g.tripped.Load() {
		t.Error("tripped set without a signal")
	}
}

// ---------------------------------------------------------------------------
// Environment preflight
// ---------------------------------------------------------------------------

func TestAptProbeArgsAreLabelledForThisRunOnly(t *testing.T) {
	const runID = "rfeedface"
	args := aptProbeArgs("docker.io/library/debian:bookworm-slim@sha256:abc", runID)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --rm",
		"--name debark-e2e-preflight-" + runID,
		"--label debark.e2e=1",
		"--label " + harness.LabelRun + "=" + runID,
		"docker.io/library/debian:bookworm-slim@sha256:abc apt-get update -qq",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("probe argv %q missing %q", joined, want)
		}
	}
	// The probe is the one container this command starts itself; if its
	// deadline kills the docker CLI, the sweep has to be able to find it.
	filter, err := runFilter(runID)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" {
			k, v, _ := strings.Cut(args[i+1], "=")
			labels[k] = v
		}
	}
	if !dockerLabelFilterMatches(filter, labels) {
		t.Fatalf("the probe container would be orphaned: labels %v do not match %q", labels, filter)
	}
	// The image must be the last thing before the command, so no flag can
	// accidentally be parsed as the image.
	if args[len(args)-4] != "docker.io/library/debian:bookworm-slim@sha256:abc" {
		t.Fatalf("argv shape changed: %v", args)
	}
}

func TestClassifyAptProbe(t *testing.T) {
	t.Run("healthy host", func(t *testing.T) {
		ok, detail := classifyAptProbe(harness.CmdResult{ExitCode: 0}, nil, false, 4*time.Second, time.Minute)
		if !ok {
			t.Fatalf("a clean apt-get update was rejected: %s", detail)
		}
		if !strings.Contains(detail, "4.0s") {
			t.Errorf("detail %q should say how long it took", detail)
		}
	})
	t.Run("hung apt (the 2026-09-03 host)", func(t *testing.T) {
		ok, detail := classifyAptProbe(harness.CmdResult{}, errors.New("signal: killed"), true, time.Minute, time.Minute)
		if ok {
			t.Fatal("a hung apt-get update must refuse the run")
		}
		for _, want := range []string{"did not finish within 1m0s", "few seconds"} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail %q missing %q", detail, want)
			}
		}
	})
	t.Run("apt failed fast", func(t *testing.T) {
		res := harness.CmdResult{ExitCode: 100, Stderr: []byte("E: Could not resolve host: deb.debian.org\n")}
		ok, detail := classifyAptProbe(res, nil, false, 2*time.Second, time.Minute)
		if ok {
			t.Fatal("a non-zero apt-get update must refuse the run")
		}
		if !strings.Contains(detail, "exited 100") || !strings.Contains(detail, "Could not resolve host") {
			t.Errorf("detail %q should carry the exit code and apt's own last line", detail)
		}
	})
	t.Run("docker itself unusable", func(t *testing.T) {
		ok, detail := classifyAptProbe(harness.CmdResult{}, errors.New("exec docker: not found"), false, 0, time.Minute)
		if ok || !strings.Contains(detail, "not found") {
			t.Errorf("ok=%v detail=%q", ok, detail)
		}
	})
}

func TestEnvPreflightRefusalExplainsItselfAndTheBypass(t *testing.T) {
	msg := envPreflightRefusal("apt-get update did not finish within 1m0s", 19)
	for _, want := range []string{
		"ENVIRONMENT PREFLIGHT FAILED",
		"refusing to start 19 row(s)",
		"apt-get update did not finish within 1m0s",
		"hack/matrix/results/README.md",
		"--skip-env-preflight",
		"No result file was written",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

func TestPlannedImagesAreDistinctAndInJobOrder(t *testing.T) {
	deb12, err := distro.Resolve("debian", "12", "")
	if err != nil {
		t.Fatal(err)
	}
	noble, err := distro.Resolve("ubuntu", "24.04", "")
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Release: deb12}, {Release: deb12}, {Release: noble}, {Release: deb12}}
	got := plannedImages(jobs)
	want := []string{deb12.Ref(), noble.Ref()}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("plannedImages = %v, want %v", got, want)
	}
	if plannedImages(nil) != nil {
		t.Error("plannedImages(nil) should be empty")
	}
}

func TestShortRefKeepsEnoughDigestToRecogniseTheImage(t *testing.T) {
	const pinned = "docker.io/library/debian@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171"
	if got, want := shortRef(pinned), "docker.io/library/debian@sha256:88200866dfff"; got != want {
		t.Errorf("shortRef = %q, want %q", got, want)
	}
	if got := shortRef("debian:bookworm-slim"); got != "debian:bookworm-slim" {
		t.Errorf("an unpinned ref was rewritten: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Console report
// ---------------------------------------------------------------------------

func TestSummaryLineAppendsNewCountsOnlyWhenTheyHappened(t *testing.T) {
	clean := summaryCounts{Total: 3, Pass: 3}
	if got, want := clean.line(), "3 total: 3 pass, 0 fail, 0 blocked, 0 skipped"; got != want {
		t.Errorf("healthy run summary line changed:\n got %q\nwant %q", got, want)
	}
	stopped := summaryCounts{Total: 3, Timeout: 2, Aborted: 1}
	if got := stopped.line(); !strings.Contains(got, "2 timeout") || !strings.Contains(got, "1 aborted") {
		t.Errorf("summary line = %q, want the timeout and aborted counts", got)
	}
}

func TestPrintSummaryTableGivesEachStoppedRowItsOwnLine(t *testing.T) {
	rows := []harness.RowResult{
		classifyRow(killedRow(), context.DeadlineExceeded, 12*time.Minute),
		classifyRow(killedRow(), context.DeadlineExceeded, 12*time.Minute),
		{Fixture: "happy", Distro: "debian", Version: "12", Arch: "amd64", Status: harness.StatusPass, Stage: "done", TotalMS: 29866},
	}
	doc := resultDoc{MatrixResult: harness.MatrixResult{Rows: rows}}
	doc.Summary = summarize(rows)

	var sb strings.Builder
	printSummaryTable(&sb, doc)
	out := sb.String()
	t.Logf("rendered report:\n%s", out) // `go test -v` shows what a reader sees

	if !strings.Contains(out, "TIMEOUT") {
		t.Errorf("the status column should read TIMEOUT:\n%s", out)
	}
	if !strings.Contains(out, "The harness stopped 2 row(s) before debark reached a verdict") {
		t.Errorf("missing the stopped-rows block:\n%s", out)
	}
	for _, want := range []string{"killed at --row-timeout", "after 721.1s", `furthest stage "build"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stopped-row line missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "apt-get update -qq") {
		t.Errorf("several timeouts should point at the host check:\n%s", out)
	}
}

func TestPrintSummaryTableStaysQuietWhenNothingWasStopped(t *testing.T) {
	rows := []harness.RowResult{{Fixture: "happy", Status: harness.StatusPass, Stage: "done"}}
	doc := resultDoc{MatrixResult: harness.MatrixResult{Rows: rows}}
	doc.Summary = summarize(rows)

	var sb strings.Builder
	printSummaryTable(&sb, doc)
	if strings.Contains(sb.String(), "The harness stopped") {
		t.Errorf("a clean run should not print the stopped-rows block:\n%s", sb.String())
	}
}

// runtimeErroredRow is the shape the 2026-09-07 run produced when QEMU binfmt
// registration disappeared from the host between two consecutive `docker
// exec`s into the same arm64 container. The row got as far as `build` and
// then every exec returned 255 with "exec format error" — an exit code
// debark cannot produce, because its own table is frozen at 0-7.
//
// The harness reports this as RowResult.RuntimeError, decided inside the row
// from the exit code itself (scenario.go's reportStageFailure), for the same
// reason TimedOut is reported from inside: by the time the runner sees the
// row it can no longer tell a runtime-chosen exit from a product-chosen one.
func runtimeErroredRow() harness.RowResult {
	return harness.RowResult{
		Fixture:      "basic-install-matrix",
		Distro:       "ubuntu",
		Version:      "22.04",
		Arch:         "arm64",
		Status:       harness.StatusBlocked,
		Stage:        "build",
		Blocker:      "exit 255 is not one of debark's own exit classes (0-7), so the product did not choose it — the container runtime or a signal did: exec /debark: exec format error",
		TotalMS:      23795,
		RuntimeError: true,
	}
}

// TestClassifyRowRuntimeExitIsNotAProductSignal pins the fix for the second
// mislabelling this runner has had, and it is the same mistake as the first:
// a row nothing in debark decided, filed as though debark had decided it.
//
// Before this, such a row stayed `blocked` and drove process exit 1, which
// hack/matrix/README.md defines as "at least one row is fail or blocked: a
// product signal". It cost the 2026-09-07 run a red row and a red exit code
// for an emulator that evaporated mid-run.
func TestClassifyRowRuntimeExitIsNotAProductSignal(t *testing.T) {
	got := classifyRow(runtimeErroredRow(), nil, 12*time.Minute)
	if got.Status != StatusErrored {
		t.Fatalf("status = %q, want %q — a runtime-chosen exit is not a product verdict", got.Status, StatusErrored)
	}
	// The blocker is not rewritten: unlike a timeout, the harness's own
	// sentence already names the cause exactly, and replacing it would throw
	// away the exit code and the "exec format error" a reader needs.
	if !strings.Contains(got.Blocker, "exec format error") {
		t.Errorf("blocker lost the runtime's own explanation: %q", got.Blocker)
	}
	if got.Status == harness.StatusBlocked {
		t.Error("still blocked, so the process would still exit 1")
	}
}

// TestClassifyRowTimeoutWinsOverRuntimeExit fixes the order of the two
// checks. A row killed at --row-timeout very often leaves a runtime-chosen
// exit behind — SIGKILL is 137, which is 128+9 and outside 0-7 — so a row can
// carry both flags. It is a timeout: that is what actually happened to it,
// and it is the status whose repetition across rows tells a reader the host
// is sick. Classifying it as `errored` instead would hide that signal in a
// bucket that says nothing about the budget.
func TestClassifyRowTimeoutWinsOverRuntimeExit(t *testing.T) {
	in := killedRow()
	in.RuntimeError = true
	in.Blocker = "exit 137 is not one of debark's own exit classes (0-7)"
	got := classifyRow(in, context.DeadlineExceeded, 12*time.Minute)
	if got.Status != StatusTimeout {
		t.Fatalf("status = %q, want %q: a killed row is a timeout whatever the dying process left behind", got.Status, StatusTimeout)
	}
}

// TestSummaryCountsErroredAndTotalStillBalances keeps the one invariant the
// schema promises a reader: total is the sum of every bucket, now seven of
// them. A new status that is counted nowhere would silently break it.
func TestSummaryCountsErroredAndTotalStillBalances(t *testing.T) {
	rows := []harness.RowResult{
		{Status: harness.StatusPass},
		{Status: harness.StatusFail},
		{Status: harness.StatusBlocked},
		{Status: harness.StatusSkipped},
		{Status: StatusTimeout},
		{Status: StatusAborted},
		{Status: StatusErrored},
		{Status: StatusErrored},
	}
	s := summarize(rows)
	if s.Errored != 2 {
		t.Errorf("errored = %d, want 2", s.Errored)
	}
	sum := s.Pass + s.Fail + s.Blocked + s.Skipped + s.Timeout + s.Aborted + s.Errored
	if s.Total != sum {
		t.Fatalf("total = %d but the buckets sum to %d — a status is counted nowhere", s.Total, sum)
	}
}
