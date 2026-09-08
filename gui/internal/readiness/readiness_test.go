package readiness

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSeverityAtLeast(t *testing.T) {
	cases := []struct {
		s, other Severity
		want     bool
	}{
		{SeverityBlocking, SeverityBlocking, true},
		{SeverityBlocking, SeverityDegraded, true},
		{SeverityBlocking, SeverityInfo, true},
		{SeverityDegraded, SeverityBlocking, false},
		{SeverityDegraded, SeverityDegraded, true},
		{SeverityInfo, SeverityDegraded, false},
		{SeverityInfo, SeverityInfo, true},
	}
	for _, c := range cases {
		if got := c.s.AtLeast(c.other); got != c.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.s, c.other, got, c.want)
		}
	}
}

func TestActionString(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want string
	}{
		{"plain", []string{"wsl", "--install"}, "wsl --install"},
		{"path with a space", []string{"debark", "keygen", "--out", `C:\Program Files\k.key`}, `debark keygen --out "C:\\Program Files\\k.key"`},
		{"empty argument", []string{"x", ""}, `x ""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Action{Command: c.cmd}).String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestReportBooleans pins the one promise the rest of the application reads off
// this package: only a blocking problem stops a build, and a degraded or
// informational one never does.
func TestReportBooleans(t *testing.T) {
	r := Report{Results: []Result{
		{ID: CheckBinary, Status: StatusOK, Severity: SeverityInfo, Summary: "fine"},
		{ID: CheckContainer, Status: StatusProblem, Severity: SeverityDegraded, Summary: "no docker", Remedy: "install it"},
		{ID: CheckSigningKey, Status: StatusProblem, Severity: SeverityInfo, Summary: "no key", Remedy: "make one"},
	}}

	if !r.CanBuild() {
		t.Error("a degraded container and a missing key must not stop a build")
	}
	if got := len(r.Problems(SeverityDegraded)); got != 1 {
		t.Errorf("Problems(degraded) = %d rows, want 1", got)
	}
	if got := len(r.Problems(SeverityInfo)); got != 2 {
		t.Errorf("Problems(info) = %d rows, want 2", got)
	}

	r.Results = append(r.Results, Result{ID: CheckBuildEnvironment, Status: StatusProblem, Severity: SeverityBlocking, Summary: "nothing", Remedy: "install something"})
	if r.CanBuild() {
		t.Error("a blocking problem must stop a build")
	}
	if got, ok := r.Get(CheckSigningKey); !ok || got.Summary != "no key" {
		t.Errorf("Get(signing-key) = %+v, %v", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get returned a result for an unknown id")
	}
}

// TestReportValidate is the guard against the wall this package exists to
// prevent: a row that reports a problem and offers nothing to do about it.
func TestReportValidate(t *testing.T) {
	cases := []struct {
		name    string
		res     Result
		wantErr string
	}{
		{"complete row", Result{ID: "a", Status: StatusProblem, Severity: SeverityDegraded, Summary: "s", Remedy: "r"}, ""},
		{"no summary", Result{ID: "a", Status: StatusOK, Severity: SeverityInfo}, "no summary"},
		{"problem with no remedy", Result{ID: "a", Status: StatusProblem, Severity: SeverityBlocking, Summary: "s"}, "problem with no remedy"},
		{"action with no command", Result{ID: "a", Status: StatusProblem, Severity: SeverityInfo, Summary: "s", Remedy: "r", Action: &Action{Label: "go"}}, "action with no command"},
		{"action with no label", Result{ID: "a", Status: StatusProblem, Severity: SeverityInfo, Summary: "s", Remedy: "r", Action: &Action{Command: []string{"x"}}}, "action with no label"},
		{"passing row carrying a severity", Result{ID: "a", Status: StatusOK, Severity: SeverityBlocking, Summary: "s"}, "non-problem carrying severity"},
		{"skipped row is fine", Result{ID: "a", Status: StatusSkipped, Severity: SeverityInfo, Summary: "s"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Report{Results: []Result{c.res}}.Validate()
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("Validate() = nil, want an error mentioning %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("Validate() = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// TestDeriveBuildEnvironment is the heart of the degrade-rather-than-gate rule:
// one missing option is never fatal, and only the absence of every option is.
// Every row here is a machine somebody actually has.
func TestDeriveBuildEnvironment(t *testing.T) {
	ok := func(id string) Result { return Result{ID: id, Status: StatusOK, Severity: SeverityInfo, Summary: "ok"} }
	bad := func(id string) Result {
		return Result{ID: id, Status: StatusProblem, Severity: SeverityDegraded, Summary: "no", Remedy: "do this"}
	}

	cases := []struct {
		name         string
		in           []Result
		wantStatus   Status
		wantSeverity Severity
		wantMentions string
	}{
		{"Debian host, no container", []Result{ok(CheckAPT), bad(CheckContainer)}, StatusOK, SeverityInfo, "native apt"},
		{"Fedora host with podman", []Result{bad(CheckAPT), ok(CheckContainer)}, StatusOK, SeverityInfo, "a container runtime"},
		{"Ubuntu host with docker", []Result{ok(CheckAPT), ok(CheckContainer)}, StatusOK, SeverityInfo, "native apt and a container runtime"},
		{"Linux with neither", []Result{bad(CheckAPT), bad(CheckContainer)}, StatusProblem, SeverityBlocking, "no way to run apt"},
		{"Windows with Docker Desktop only", []Result{bad(CheckWSL), ok(CheckContainer)}, StatusOK, SeverityInfo, "a container runtime"},
		{"locked-down Windows laptop", []Result{bad(CheckWSL), bad(CheckContainer)}, StatusProblem, SeverityBlocking, "no way to run apt"},
		// WSL 2 is not a route. debark runs apt natively or in a container
		// and nothing else, and on Windows native apt is not registered at
		// all, so a machine with WSL 2 and no usable container cannot build --
		// which is what it now says. This row used to read StatusOK and
		// "WSL 2", and every build on that machine failed.
		{"Windows with WSL 2 and no container", []Result{ok(CheckWSL), bad(CheckContainer)}, StatusProblem, SeverityBlocking, "no way to run apt"},
		{"Windows with WSL 2 and a container", []Result{ok(CheckWSL), ok(CheckContainer)}, StatusOK, SeverityInfo, "a container runtime"},
		{"unrelated rows only", []Result{ok(CheckBinary), bad(CheckSigningKey)}, StatusSkipped, SeverityInfo, "nothing to conclude"},
		{"a WSL row on its own concludes nothing", []Result{ok(CheckWSL)}, StatusSkipped, SeverityInfo, "nothing to conclude"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveBuildEnvironment(c.in)
			if got.ID != CheckBuildEnvironment {
				t.Errorf("ID = %q", got.ID)
			}
			if got.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, c.wantStatus)
			}
			if got.Severity != c.wantSeverity {
				t.Errorf("Severity = %q, want %q", got.Severity, c.wantSeverity)
			}
			if !strings.Contains(got.Summary, c.wantMentions) {
				t.Errorf("Summary = %q, want it to mention %q", got.Summary, c.wantMentions)
			}
			if got.Status == StatusProblem && got.Remedy == "" {
				t.Error("a blocking build environment with no remedy is exactly the wall this package forbids")
			}
			if err := (Report{Results: []Result{got}}).Validate(); err != nil {
				t.Errorf("derived row breaks the contract: %v", err)
			}
		})
	}
}

// TestReportWithRederives is the "I started Docker, check again" path: folding
// one fresh row back in has to move the derived headline with it.
func TestReportWithRederives(t *testing.T) {
	start := Report{Results: []Result{
		{ID: CheckAPT, Status: StatusProblem, Severity: SeverityDegraded, Summary: "no apt", Remedy: "x"},
		{ID: CheckContainer, Status: StatusProblem, Severity: SeverityDegraded, Summary: "docker is stopped", Remedy: "start it"},
	}}
	start.Results = append(start.Results, DeriveBuildEnvironment(start.Results))

	if start.CanBuild() {
		t.Fatal("a machine with no apt and a stopped docker must not be buildable")
	}

	after := start.With(Result{ID: CheckContainer, Status: StatusOK, Severity: SeverityInfo, Summary: "docker is running"})
	if !after.CanBuild() {
		t.Error("starting Docker must clear the blocker without a full re-run")
	}
	if got, _ := after.Get(CheckBuildEnvironment); got.Status != StatusOK {
		t.Errorf("derived row = %q, want ok", got.Status)
	}
	if got, _ := start.Get(CheckContainer); got.Status != StatusProblem {
		t.Error("With must not mutate the report it was called on")
	}
	if n := len(after.Results); n != 3 {
		t.Errorf("With produced %d rows, want 3", n)
	}
}

func TestReportWithAppendsUnknownRow(t *testing.T) {
	after := Report{}.With(Result{ID: CheckDiskSpace, Status: StatusOK, Severity: SeverityInfo, Summary: "plenty"})
	if _, ok := after.Get(CheckDiskSpace); !ok {
		t.Error("With must add a row the report did not already have")
	}
	if got, ok := after.Get(CheckBuildEnvironment); !ok || got.Status != StatusSkipped {
		t.Errorf("derived row = %+v, want a skipped row", got)
	}
}

func TestSortResultsFollowsCheckOrder(t *testing.T) {
	rs := []Result{
		{ID: CheckDiskSpace},
		{ID: "something-added-later"},
		{ID: CheckBuildEnvironment},
		{ID: CheckBinary},
		{ID: CheckContainer},
	}
	sortResults(rs)
	want := []string{CheckBinary, CheckContainer, CheckBuildEnvironment, CheckDiskSpace, "something-added-later"}
	for i, id := range want {
		if rs[i].ID != id {
			t.Fatalf("position %d = %q, want %q (whole order: %v)", i, rs[i].ID, id, ids(rs))
		}
	}
}

func TestJoinWords(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b and c"},
	}
	for _, c := range cases {
		if got := joinWords(c.in); got != c.want {
			t.Errorf("joinWords(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"\n\n  hello \nworld", "hello"},
		{"only", "only"},
	}
	for _, c := range cases {
		if got := firstLine(c.in); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func ids(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// TestWSLNeverCreditedAsARoute is the measured defect, as a table.
//
// The self-binary gate below only fires when no OTHER route is ok, so a green
// WSL 2 row silently defeated it. Row 1 is an ordinary Windows machine with
// Docker Desktop, measured live on real hardware: can_build true, a green
// "This machine can build bundles" banner, and every build then failing. Row 3
// is the only combination where the gate worked, and it needs a Windows
// machine with no WSL 2 -- but Docker Desktop's default backend IS WSL 2 and
// docker-desktop registers itself as a distribution, so on the machines the
// gate exists for it could essentially never fire.
//
// Written as the full truth table rather than as the one failing row, because
// the defect was not one wrong answer: it was a route that should not have
// been in the set at all, and only the whole table shows that.
func TestWSLNeverCreditedAsARoute(t *testing.T) {
	row := func(id string, ok bool) Result {
		if ok {
			return Result{ID: id, Status: StatusOK, Severity: SeverityInfo, Summary: "ok"}
		}
		return Result{ID: id, Status: StatusProblem, Severity: SeverityDegraded, Summary: "no", Remedy: "do this"}
	}

	cases := []struct {
		wsl, container, self bool
		wantStatus           Status
		wantCanBuild         bool
		// wantMentions is the phrase the summary must carry, so a right
		// verdict with a misleading sentence still fails.
		wantMentions string
	}{
		{true, true, false, StatusProblem, false, "no Linux build of itself"},
		{true, false, false, StatusProblem, false, "no way to run apt"},
		{false, true, false, StatusProblem, false, "no Linux build of itself"},
		{true, true, true, StatusOK, true, "a container runtime"},
		{false, true, true, StatusOK, true, "a container runtime"},
		{false, false, true, StatusProblem, false, "no way to run apt"},
		{true, false, true, StatusProblem, false, "no way to run apt"},
		{false, false, false, StatusProblem, false, "no way to run apt"},
	}

	for _, c := range cases {
		name := fmt.Sprintf("wsl=%v container=%v self=%v", c.wsl, c.container, c.self)
		t.Run(name, func(t *testing.T) {
			in := []Result{row(CheckWSL, c.wsl), row(CheckContainer, c.container), row(CheckSelfBinary, c.self)}
			got := DeriveBuildEnvironment(in)
			if got.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q (summary %q)", got.Status, c.wantStatus, got.Summary)
			}
			if !strings.Contains(got.Summary, c.wantMentions) {
				t.Errorf("Summary = %q, want it to mention %q", got.Summary, c.wantMentions)
			}
			if strings.Contains(got.Summary, "WSL") {
				t.Errorf("Summary = %q credits WSL 2, which is not a route", got.Summary)
			}
			if got.Status == StatusProblem && got.Remedy == "" {
				t.Error("a blocking verdict with no remedy is the wall")
			}

			rep := Report{Results: append(in, got)}
			if rep.CanBuild() != c.wantCanBuild {
				t.Errorf("CanBuild = %v, want %v", rep.CanBuild(), c.wantCanBuild)
			}
		})
	}
}

// TestContainerCheckGetsItsOwnTimeout is a false negative measured on real
// hardware, pinned as a rule.
//
// `docker info` on a Windows machine with Docker Desktop running and healthy
// took 6431 ms on the first call after the daemon had been idle, then 558 ms
// and 455 ms. The general per-check bound is five seconds, so that first call
// timed out and a perfectly working Docker Desktop reported "installed but did
// not answer within the timeout" on every cold run.
//
// It matters more than it used to. With WSL 2 no longer counted as a build
// route, this row is the whole build verdict on Windows and macOS: a false
// negative here tells a machine that can build that it cannot.
func TestContainerCheckGetsItsOwnTimeout(t *testing.T) {
	c := New(Options{})
	general := c.timeoutFor(CheckBinary)
	container := c.timeoutFor(CheckContainer)

	if container <= general {
		t.Fatalf("the container check gets %v and everything else gets %v; a healthy Docker Desktop's first call was measured at 6431 ms", container, general)
	}
	// The measured cold call, with room. Asserting the measurement rather than
	// the constant, so raising defaultTimeout cannot silently swallow this.
	if container < 7*time.Second {
		t.Errorf("container timeout is %v, which is under the 6431 ms cold call this exists for", container)
	}
	for _, id := range CheckIDs() {
		if id == CheckContainer {
			continue
		}
		if got := c.timeoutFor(id); got != general {
			t.Errorf("check %q gets %v, want the general %v — only the container probe has a budget of its own", id, got, general)
		}
	}
}

// TestContainerTimeoutIsOverridable keeps the seam a test can use: a table
// test for the wedged-socket path must not have to wait twenty seconds for it.
func TestContainerTimeoutIsOverridable(t *testing.T) {
	c := New(Options{Timeout: 50 * time.Millisecond, ContainerTimeout: 80 * time.Millisecond})
	if got := c.timeoutFor(CheckContainer); got != 80*time.Millisecond {
		t.Errorf("ContainerTimeout = %v, want the value the caller set", got)
	}
	if got := c.timeoutFor(CheckAPT); got != 50*time.Millisecond {
		t.Errorf("Timeout = %v, want the value the caller set", got)
	}
}
