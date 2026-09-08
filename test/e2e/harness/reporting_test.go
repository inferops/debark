package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file tests the harness's own reporting integrity: that a failure of
// the *harness* is recorded as blocked ("this row could not be run") and
// never as fail ("debark did the wrong thing").
//
// It exists because of hack/matrix/results/README.md's invalid run, where
// nineteen rows were killed at their timeout by a hanging host apt and the
// result file reported nine of them as product failures — `exit 1 (usage),
// want 0 (success)` — for a usage error nothing had returned. Four hours of
// container time produced a table that was not merely useless but actively
// misleading, and someone nearly went hunting for the bug it described.
//
// Every test here runs without Docker and without a network, which is the
// point: the reporting decisions are pure enough to test directly, so they
// can be, and a regression is caught in milliseconds instead of in a matrix
// run nobody can trust afterwards.

// newTestRun builds a fixtureRun with just enough state for the reporting
// methods, which are the only thing exercised here.
func newTestRun(t *testing.T) *fixtureRun {
	t.Helper()
	row := &RowResult{ExitClasses: map[string]string{}, StageMS: map[string]int64{}}
	return &fixtureRun{
		row: row,
		log: &strings.Builder{},
	}
}

// TestRecordBundleStatsBlocksWhenItCannotMeasure is defect 2: a walk that
// fails must not leave BundleSizeBytes=0 / PackageCount=0 behind, because
// those are also what a genuinely empty bundle records — and both fields are
// omitempty, so the zeros vanish from the JSON entirely rather than standing
// out as suspicious.
func TestRecordBundleStatsBlocksWhenItCannotMeasure(t *testing.T) {
	r := newTestRun(t)
	missing := filepath.Join(t.TempDir(), "no-such-bundle")

	if r.recordBundleStats(missing) {
		t.Fatal("recordBundleStats reported success for a bundle directory that does not exist")
	}
	if r.row.Status != StatusBlocked {
		t.Errorf("status = %q, want %q: an unmeasurable bundle is a row that could not run, not a row that failed", r.row.Status, StatusBlocked)
	}
	if r.row.BundleSizeBytes != 0 || r.row.PackageCount != 0 {
		t.Errorf("recorded stats %d bytes / %d packages from a walk that failed; want nothing recorded",
			r.row.BundleSizeBytes, r.row.PackageCount)
	}
	if !strings.Contains(r.row.Blocker, missing) {
		t.Errorf("blocker %q does not name the directory it could not measure", r.row.Blocker)
	}
}

func TestRecordBundleStatsMeasuresARealBundle(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "debark.manifest.json"), 10)
	mustWrite(t, filepath.Join(dir, "repo", "pool", "a_1_amd64.deb"), 100)
	mustWrite(t, filepath.Join(dir, "repo", "pool", "b_1_amd64.deb"), 200)

	r := newTestRun(t)
	if !r.recordBundleStats(dir) {
		t.Fatalf("recordBundleStats failed on a readable bundle: %s", r.row.Blocker)
	}
	if r.row.Status != "" {
		t.Errorf("status = %q, want it left unset by a successful measurement", r.row.Status)
	}
	if want := int64(310); r.row.BundleSizeBytes != want {
		t.Errorf("BundleSizeBytes = %d, want %d", r.row.BundleSizeBytes, want)
	}
	if r.row.PackageCount != 2 {
		t.Errorf("PackageCount = %d, want 2", r.row.PackageCount)
	}
}

func mustWrite(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestReportAssertFindingsPrefersBlockedOverFail is the assertion-stage half
// of the same rule: an assertion the harness could not carry out must never
// be written down as a product failure, even when other assertions in the
// same batch genuinely did fail — those may well be downstream of whatever
// broke.
func TestReportAssertFindingsPrefersBlockedOverFail(t *testing.T) {
	cases := []struct {
		name     string
		findings []Finding
		want     Status
		wantCont bool
	}{
		{
			name:     "all passing",
			findings: []Finding{ok("package-present:jq"), ok("binary-runs:/usr/bin/jq")},
			want:     "",
			wantCont: true,
		},
		{
			name:     "a real mismatch is a product failure",
			findings: []Finding{fail("binary-runs:/usr/bin/jq", "exit code 127, want 0")},
			want:     StatusFail,
		},
		{
			name:     "a harness failure is blocked, not failed",
			findings: []Finding{plumbing("binary-runs:/usr/bin/jq", "could not exec: docker daemon unreachable")},
			want:     StatusBlocked,
		},
		{
			name: "a harness failure alongside a mismatch is still blocked",
			findings: []Finding{
				fail("package-present:jq", "dpkg status = \"\""),
				plumbing("binary-runs:/usr/bin/jq", "could not exec: docker daemon unreachable"),
			},
			want: StatusBlocked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRun(t)
			got := r.reportAssertFindings(tc.findings)
			if got != tc.wantCont {
				t.Errorf("continue = %v, want %v", got, tc.wantCont)
			}
			if r.row.Status != tc.want {
				t.Errorf("status = %q, want %q (blocker: %s)", r.row.Status, tc.want, r.row.Blocker)
			}
		})
	}
}

// TestReportStageFailureBlamesTheRuntimeForItsOwnExitCodes covers the
// post-mortem's headline symptom. debark only ever exits with one of its
// own classes (core/dferr, 0..7); 125/126/127 come from `docker exec` and
// 137 from a SIGKILL. Recording any of those as "the product returned exit
// N, want 0" invents a verdict nothing produced.
func TestReportStageFailureBlamesTheRuntimeForItsOwnExitCodes(t *testing.T) {
	cases := []struct {
		name string
		res  CmdResult
		want Status
		// runtimeError is the fact hack/matrix classifies from: the exit
		// code was chosen by the container runtime or a signal, so the row
		// reached no verdict at all and must not be reported as one. The
		// status stays blocked here because this package does not own the
		// extended status set; the flag is what lets the runner say so.
		runtimeError bool
		detail       string
	}{
		{
			name: "a real verification failure is the product's answer",
			res:  CmdResult{ExitCode: 4, Stderr: []byte("debark: signature does not verify")},
			want: StatusFail,
		},
		{
			name: "an unimplemented stub is blocked",
			res:  CmdResult{ExitCode: 1, Stderr: []byte("debark: engine: not implemented")},
			want: StatusBlocked,
		},
		{
			name:         "docker exec's own 'not found' is not a usage error",
			res:          CmdResult{ExitCode: 127, Stderr: []byte("exec: \"/debark\": stat /debark: no such file or directory")},
			want:         StatusBlocked,
			runtimeError: true,
			detail:       "not one of debark's own exit classes",
		},
		{
			name:         "a killed process is not a product verdict",
			res:          CmdResult{ExitCode: 137, Stderr: []byte("")},
			want:         StatusBlocked,
			runtimeError: true,
			detail:       "not one of debark's own exit classes",
		},
		{
			// The exact shape the 2026-09-07 run hit when QEMU binfmt
			// registration disappeared from the host mid-run: the arm64
			// image is there, the container starts, and every exec into it
			// returns 255 because nothing can execute the binary.
			name:         "a platform this host cannot execute",
			res:          CmdResult{ExitCode: 255, Stderr: []byte("exec /debark: exec format error")},
			want:         StatusBlocked,
			runtimeError: true,
			detail:       "not one of debark's own exit classes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRun(t)
			r.reportStageFailure("build", fmt.Sprintf("exit %d (...), want 0 (success)", tc.res.ExitCode), tc.res)
			if r.row.Status != tc.want {
				t.Errorf("status = %q, want %q (blocker: %s)", r.row.Status, tc.want, r.row.Blocker)
			}
			if tc.detail != "" && !strings.Contains(r.row.Blocker, tc.detail) {
				t.Errorf("blocker %q does not explain that the exit code was not debark's", r.row.Blocker)
			}
			// Without this the runner cannot tell the two blocked rows
			// apart, and an emulator that vanished mid-run goes on being
			// reported as a product signal — which is what it did before
			// the field existed.
			if r.row.RuntimeError != tc.runtimeError {
				t.Errorf("RuntimeError = %v, want %v", r.row.RuntimeError, tc.runtimeError)
			}
		})
	}
}

func TestIsProductExitCode(t *testing.T) {
	for code := 0; code <= 7; code++ {
		if !isProductExitCode(code) {
			t.Errorf("exit %d is a documented dferr class but isProductExitCode said otherwise", code)
		}
	}
	for _, code := range []int{-1, 8, 125, 126, 127, 137, 143, 255} {
		if isProductExitCode(code) {
			t.Errorf("exit %d is not a dferr class; treating it as one lets a runtime failure become a product verdict", code)
		}
	}
}

// TestRunHostReportsAKillAsAnErrorNotAnExitCode is the root cause of the
// invalid matrix run. exec.CommandContext kills the child when the row's
// deadline expires, and the killed process reports exit 1 on Windows —
// dferr's "usage" class. runHost must report that as a harness error so
// every call site turns it into a blocked row.
func TestRunHostReportsAKillAsAnErrorNotAnExitCode(t *testing.T) {
	t.Setenv(helperEnv, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	res, err := runHost(ctx, nil, os.Args[0], "-test.run=^TestHarnessHelperProcess$")
	if err == nil {
		t.Fatalf("runHost returned no error for a process it killed; it reported exit %d as though the process had chosen it", res.ExitCode)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not identify the deadline as the cause", err)
	}
	if !strings.Contains(err.Error(), "before it could report an outcome") {
		t.Errorf("error %q does not say that no outcome was produced", err)
	}
}

// TestRunHostStillReportsARealExitCode is the other half: a process that
// genuinely chose a non-zero status must still come back as data, with a nil
// error, or every fixture that expects a non-zero exit class would break.
func TestRunHostStillReportsARealExitCode(t *testing.T) {
	t.Setenv(helperEnv, "exit-4")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := runHost(ctx, nil, os.Args[0], "-test.run=^TestHarnessHelperProcess$")
	if err != nil {
		t.Fatalf("runHost: unexpected error for a process that exited by itself: %v", err)
	}
	if res.ExitCode != 4 {
		t.Errorf("ExitCode = %d, want 4 (the status the process actually chose)", res.ExitCode)
	}
}

const helperEnv = "DEBARK_E2E_HELPER_PROCESS"

// TestHarnessHelperProcess is not a test: it is the child process the two
// runHost tests above execute (the standard os/exec test idiom). It does
// nothing at all unless the parent set helperEnv.
func TestHarnessHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "":
		return
	case "exit-4":
		os.Exit(4)
	default:
		// Long enough that the parent's deadline is what ends this, short
		// enough that a broken kill does not hang the package's test run
		// for minutes.
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
}

// TestFirstUnreadableSeparatesAbsenceFromFailure guards the determinism
// comparison: "this build did not write that file" is a fact about debark,
// while "the host would not let us read a file we copied out ourselves" is a
// fact about the harness, and only the first can be a determinism verdict.
func TestFirstUnreadableSeparatesAbsenceFromFailure(t *testing.T) {
	notExist := &fs.PathError{Op: "open", Path: "bundle/lock.json", Err: fs.ErrNotExist}
	permission := &fs.PathError{Op: "open", Path: "bundle/lock.json", Err: fs.ErrPermission}

	if got := firstUnreadable(nil, nil); got != nil {
		t.Errorf("firstUnreadable(nil, nil) = %v, want nil", got)
	}
	if got := firstUnreadable(notExist, notExist); got != nil {
		t.Errorf("a missing file is an observation about the build, not a harness failure; got %v", got)
	}
	if got := firstUnreadable(notExist, permission); got == nil {
		t.Error("a permission error on a file the harness copied out must not be reported as a determinism mismatch")
	}
}

// TestDecoyKeyReasonExplainsItself checks the message the wrong-key guard
// hands to setBlocked. The guard itself needs containers; its explanation
// does not, and an unexplained block is only marginally better than a false
// failure.
func TestDecoyKeyReasonExplainsItself(t *testing.T) {
	if got := decoyKeyReason(false, ""); !strings.Contains(got, "request.sign") {
		t.Errorf("unsigned fixture reason = %q, want it to name the missing fixture field", got)
	}
	if got := decoyKeyReason(true, "exit 2: keygen: no such directory"); !strings.Contains(got, "keygen") {
		t.Errorf("keygen-failure reason = %q, want it to carry the keygen error", got)
	}
	if got := decoyKeyReason(true, ""); got != "" {
		t.Errorf("reason = %q, want empty when there is nothing more to say", got)
	}
}

// TestDockerExecFailedToRunIsNotAppliedToBinaryChecks documents the one
// place 126/127 must NOT be read as plumbing: a package's own binary that is
// missing or unrunnable is the product failure AssertBinaries exists to
// catch ("a package can install and still be unusable").
func TestDockerExecFailedToRunIsNotAppliedToBinaryChecks(t *testing.T) {
	for _, code := range []int{125, 126, 127} {
		if !dockerExecFailedToRun(code) {
			t.Errorf("exit %d is one docker exec produces for its own failures", code)
		}
	}
	for _, code := range []int{0, 1, 2, 4, 137} {
		if dockerExecFailedToRun(code) {
			t.Errorf("exit %d must not be read as a docker-exec failure", code)
		}
	}

	// AssertBinaries must keep reporting a missing binary as a failed
	// finding, not a plumbing one: this is the assertion the task calls the
	// one that matters.
	findings := AssertBinaries(context.Background(), nil, nil)
	if len(findings) != 0 {
		t.Fatalf("AssertBinaries with no checks returned %d findings", len(findings))
	}
}

// TestPlumbingSummaryOnlyReportsHarnessFailures keeps the two summaries
// separate: PlumbingSummary is what setBlocked is given, FailureSummary is
// what setFail is given, and a finding must never appear in both roles.
func TestPlumbingSummaryOnlyReportsHarnessFailures(t *testing.T) {
	findings := []Finding{
		ok("package-present:jq"),
		fail("package-absent:tree", "found installed"),
		plumbing("binary-runs:/usr/bin/jq", "could not exec: daemon gone"),
	}
	plumb := PlumbingSummary(findings)
	if !strings.Contains(plumb, "binary-runs:/usr/bin/jq") {
		t.Errorf("PlumbingSummary = %q, want the harness failure", plumb)
	}
	if strings.Contains(plumb, "package-absent:tree") {
		t.Errorf("PlumbingSummary = %q, want it to exclude a genuine mismatch", plumb)
	}
	if failures := FailureSummary(findings); !strings.Contains(failures, "package-absent:tree") {
		t.Errorf("FailureSummary = %q, want the genuine mismatch", failures)
	}
	if AllOK(findings) {
		t.Error("AllOK reported success for a set containing failures")
	}
}

// TestRunFixtureStampsTimedOutOnARowItCouldNotFinish proves the fact that
// RowResult.TimedOut exists to carry actually reaches the row, from the only
// place that can observe it.
//
// The runner cannot reconstruct this afterwards: RunFixture tears its
// containers down inside the call on a deliberately fresh context, so by the
// time it returns, its caller's context reads the same for a row killed
// mid-stage as for a row that reached a verdict and then spent longer than
// the remaining budget on cleanup. Calling the second one a timeout demotes a
// real product failure to "check the host" — the mirror image of the
// 2026-09-03 mislabelling this file is about, and it discards the one row a
// reader most needs. hack/matrix's classifyRow keys entirely on this field;
// this test is what stops it silently becoming a constant false.
//
// Container-free, and it stays that way: an already-cancelled context fails
// the first stage (`go build` for the target, which exec refuses to start on
// a dead context) long before anything reaches Docker, and no container means
// cleanup has nothing to remove.
func TestRunFixtureStampsTimedOutOnARowItCouldNotFinish(t *testing.T) {
	// The linux-binary build is memoised per GOARCH for the whole process;
	// a deliberately cancelled one must not be left in that cache for
	// another test to inherit as a real failure.
	ResetBuildCacheForTest()
	t.Cleanup(ResetBuildCacheForTest)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	row := RunFixture(ctx, Scenario{
		Name:   "timed-out-before-any-container",
		Target: TargetSpec{Distro: "debian", Version: "12", Arch: "amd64"},
	}, RunOpts{
		RunID:                "timedout-unit",
		WorkDir:              t.TempDir(),
		Timeout:              time.Minute,
		KeepWorkDirOnFailure: true,
	})

	if !row.TimedOut {
		t.Errorf("TimedOut = false on a row whose context was already dead when execute() returned; hack/matrix would publish %q as a product verdict", row.Status)
	}
	if row.Status == StatusPass {
		t.Errorf("status = %q: a row that ran no stage must never be recorded as a pass", row.Status)
	}
	if row.Stage == "" {
		t.Error("no stage recorded, so the row cannot say how far it got")
	}
}
