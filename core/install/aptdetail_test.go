package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// --- defect 2(a): the failing command's own explanation ---------------------

// TestRunProcess_FailureDetail_KeepsAptsExplanation is the measured failure
// from 2026-09-06: a foreign-arch bundle installed on a target without
// `dpkg --add-architecture i386`. apt-get exited 100 and the output said, in
// so many words, what was wrong - and install reported
//
//	apt-get install: apt-get exited 100: Reading package lists...
//
// because the error kept the FIRST line of the output, which is apt's banner.
// Two entirely different failures were indistinguishable in the report for
// hours as a result. The explanation is on an indented continuation line
// under dpkg's header line, so keeping "the lines that look like errors" is
// not enough on its own either.
func TestRunProcess_FailureDetail_KeepsAptsExplanation(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_APT_INSTALL_OUTPUT_FILE", goldenPath(t, "apt-install-foreign-arch-refused.txt"))
	t.Setenv("FAKE_APT_INSTALL_EXIT", "100")

	_, err := runProcess(context.Background(), testBinaryPath(t), []string{"install", "zlib1g:i386"}, buildEnv(false))
	if err == nil {
		t.Fatalf("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "package architecture (i386) does not match system (amd64)") {
		t.Errorf("error = %q\nwant it to carry dpkg's own explanation", msg)
	}
	if strings.Contains(msg, "Reading package lists") {
		t.Errorf("error = %q\nwant the banner line dropped, not kept", msg)
	}
	if !strings.Contains(msg, "exited 100") {
		t.Errorf("error = %q, want the exit code preserved", msg)
	}
	t.Logf("operator sees: %s", msg)
}

// TestApply_AptFailure_ProblemCarriesTheReason is the same thing end to end:
// the text has to survive all the way into Report.Problems, which is what is
// written to the durable evidence log and printed as the failure.
func TestApply_AptFailure_ProblemCarriesTheReason(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	// The plan simulates clean and the real install then fails - the exact
	// asymmetry the foreign-arch measurement produced, and the reason this
	// error text is sometimes the only thing an operator gets.
	t.Setenv("FAKE_APT_INSTALL_OUTPUT_FILE", goldenPath(t, "apt-install-foreign-arch-refused.txt"))
	t.Setenv("FAKE_APT_INSTALL_EXIT", "100")

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !strings.Contains(err.Error(), "package architecture (i386) does not match system (amd64)") {
		t.Errorf("err = %q\nwant the real reason", err.Error())
	}
	found := false
	for _, p := range report.Problems {
		if strings.Contains(p, "package architecture (i386) does not match system (amd64)") {
			found = true
		}
	}
	if !found {
		t.Errorf("Problems = %v\nwant one carrying the real reason", report.Problems)
	}
	t.Logf("operator sees: debark: %s", err.Error())
}

// TestRunProcess_FailureDetail_IsBounded: this string reaches a Go error, a
// Report and the evidence log's JSON, so a failure that produces hundreds of
// error lines must still yield a message, not a transcript. The bound is
// asserted as a literal rather than against the constant so the assertion
// stays meaningful if the constant is ever changed by accident.
func TestRunProcess_FailureDetail_IsBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("Reading package lists...\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "dpkg: error processing archive /pool/pkg%03d.deb (--unpack):\n package %03d is broken in a way that takes a whole line to describe\n", i, i)
	}
	sb.WriteString("E: Sub-process /usr/bin/dpkg returned an error code (1)\n")
	big := filepath.Join(t.TempDir(), "big-failure.txt")
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_APT_INSTALL_OUTPUT_FILE", big)
	t.Setenv("FAKE_APT_INSTALL_EXIT", "100")

	_, err := runProcess(context.Background(), testBinaryPath(t), []string{"install", "everything"}, buildEnv(false))
	if err == nil {
		t.Fatalf("expected an error")
	}
	msg := err.Error()
	if len(msg) > 800 {
		t.Errorf("error is %d bytes; a message, not a transcript, was the point: %q", len(msg), msg)
	}
	if !strings.Contains(msg, "...") {
		t.Errorf("error = %q, want the elision marked rather than silently cut", msg)
	}
	// The middle is what gets elided: the first failure and apt's closing
	// summary are the two ends worth keeping.
	if !strings.Contains(msg, "pkg000.deb") {
		t.Errorf("error = %q, want the first failure kept", msg)
	}
	if !strings.Contains(msg, "E: Sub-process") {
		t.Errorf("error = %q, want the closing summary kept", msg)
	}
}

// TestRunProcess_FailureDetail_NoErrorLines falls back to the LAST lines,
// because output ends with the failure and begins with the banner.
func TestRunProcess_FailureDetail_NoErrorLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet-failure.txt")
	if err := os.WriteFile(path, []byte("Reading package lists...\nBuilding dependency tree...\nsomething unhelpful happened\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_APT_INSTALL_OUTPUT_FILE", path)
	t.Setenv("FAKE_APT_INSTALL_EXIT", "100")

	_, err := runProcess(context.Background(), testBinaryPath(t), []string{"install", "x"}, buildEnv(false))
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !strings.Contains(err.Error(), "something unhelpful happened") {
		t.Errorf("error = %q, want the last line rather than the banner", err.Error())
	}
}

// TestRunProcess_FailureDetail_SilentFailure: a tool that fails with no
// output at all gets a message with no dangling separator.
func TestRunProcess_FailureDetail_SilentFailure(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_APT_INSTALL_EXIT", "7")

	_, err := runProcess(context.Background(), testBinaryPath(t), []string{"install", "x"}, buildEnv(false))
	if err == nil {
		t.Fatalf("expected an error")
	}
	if strings.HasSuffix(err.Error(), ": ") || strings.HasSuffix(err.Error(), ":") {
		t.Errorf("error = %q, want no dangling separator when there is nothing to say", err.Error())
	}
}

// --- defect 2(b): apt's unmet-dependency block ------------------------------

// unmetCulprit is the line measured verbatim on 2026-09-06 - the only line
// in the whole failure that names which package and which dependency.
const unmetCulprit = "zlib1g:i386 : Depends: libc6:i386 (>= 2.4) but it is not installable"

// TestParseAptSim_UnmetDeps_KeepsTheCulpritLine: the problem filter used to
// keep only "E: " lines and lines containing "unmet dependencies"/"Unable to
// correct problems", which kept the header and the summary and dropped the
// one line in between that says what is actually broken. The blocker then
// read as a mystery.
func TestParseAptSim_UnmetDeps_KeepsTheCulpritLine(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "apt-sim-unmet-deps-foreign.txt"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_, _, _, problems := parseAptSim(data)

	joined := strings.Join(problems, " | ")
	if !strings.Contains(joined, unmetCulprit) {
		t.Fatalf("problems = %v\nwant one naming the culprit: %q", problems, unmetCulprit)
	}
	// The narrative apt wrote is preserved in order: header, culprit, summary.
	want := []string{
		"The following packages have unmet dependencies:",
		unmetCulprit,
		"E: Unable to correct problems, you have held broken packages.",
	}
	if !equalStrings(problems, want) {
		t.Errorf("problems =\n  %v\nwant\n  %v", problems, want)
	}
	t.Logf("operator sees: %s", joined)
}

// TestPlan_UnmetDeps_ProblemNamesTheCulprit is the same thing through the
// runner: a simulate that fails this way must leave the culprit in the
// report, and Apply must refuse before touching anything.
func TestPlan_UnmetDeps_ProblemNamesTheCulprit(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_ARCH", "amd64")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-unmet-deps-foreign.txt"))
	t.Setenv("FAKE_APT_SIM_EXIT", "100")

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true})
	if err == nil {
		t.Fatalf("expected a refusal")
	}
	if got := dferr.ClassOf(err); got != dferr.Resolution {
		t.Errorf("class = %v (exit %d), want Resolution (exit 5): %v", got, dferr.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), unmetCulprit) {
		t.Errorf("err = %q\nwant the culprit named", err.Error())
	}
	found := false
	for _, p := range report.Problems {
		if strings.Contains(p, unmetCulprit) {
			found = true
		}
	}
	if !found {
		t.Errorf("Problems = %v\nwant one naming the culprit", report.Problems)
	}
	if report.Applied {
		t.Errorf("Applied = true; a plan with problems must refuse before mutating")
	}
	t.Logf("operator sees: debark: %s", err.Error())
}

// TestParseAptSim_UnmetDeps_BlockBoundaries pins where the block starts and
// stops - the same rule core/apt's ParseSimulate applies to the same output,
// so the two parsers cannot disagree about where apt's answer ends. A
// continuation line (apt indents further when one package has several
// unsatisfied relationships) belongs to the block; an indented line after the
// block has ended does not.
func TestParseAptSim_UnmetDeps_BlockBoundaries(t *testing.T) {
	out := "Reading package lists...\n" +
		"The following packages have unmet dependencies:\n" +
		" libfoo:i386 : Depends: libbar:i386 (>= 1.0) but it is not installable\n" +
		"              Recommends: libbaz:i386 but it is not going to be installed\n" +
		" libqux:i386 : Depends: libquux:i386 but it is not installable\n" +
		"E: Unable to correct problems, you have held broken packages.\n" +
		" this indented line is after the block and is not apt naming a package\n"

	_, _, _, problems := parseAptSim([]byte(out))
	want := []string{
		"The following packages have unmet dependencies:",
		"libfoo:i386 : Depends: libbar:i386 (>= 1.0) but it is not installable",
		"Recommends: libbaz:i386 but it is not going to be installed",
		"libqux:i386 : Depends: libquux:i386 but it is not installable",
		"E: Unable to correct problems, you have held broken packages.",
	}
	if !equalStrings(problems, want) {
		t.Errorf("problems =\n  %v\nwant\n  %v", problems, want)
	}
}

// TestParseAptSim_UnmetDeps_Bounded: Report.Problems is joined into an error
// message and written to the evidence log, so a pathological block must be
// capped rather than copied wholesale.
func TestParseAptSim_UnmetDeps_Bounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("The following packages have unmet dependencies:\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, " pkg%03d:i386 : Depends: libc6:i386 but it is not installable\n", i)
	}
	sb.WriteString("E: Unable to correct problems, you have held broken packages.\n")

	// The bound is asserted as a literal rather than against the constant, so
	// the assertion stays meaningful if the constant is ever changed by
	// accident.
	_, _, _, problems := parseAptSim([]byte(sb.String()))
	if len(problems) > 25 {
		t.Errorf("problems has %d entries; the block must be capped", len(problems))
	}
	joined := strings.Join(problems, " | ")
	if !strings.Contains(joined, "pkg000:i386") {
		t.Errorf("the first culprits must survive the cap: %v", problems)
	}
	if !strings.Contains(joined, "omitted") {
		t.Errorf("the cap must be visible, not silent: %v", problems)
	}
	if !strings.Contains(joined, "E: Unable to correct problems") {
		t.Errorf("apt's summary must still be kept after the cap: %v", problems)
	}
}

// TestParseAptSim_Goldens_Unchanged: the pre-existing goldens have no unmet
// block, and the new block handling must not have changed what they parse to.
func TestParseAptSim_Goldens_Unchanged(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "apt-sim-unknown-package.txt"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_, _, _, problems := parseAptSim(data)
	want := []string{"E: Unable to locate package this-package-does-not-exist-xyz"}
	if !equalStrings(problems, want) {
		t.Errorf("problems = %v, want %v", problems, want)
	}
}
