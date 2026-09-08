package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// --- fixtures ---------------------------------------------------------------

// synthesizedSnapshotJSON renders a snapshot.json exactly as a bundle built
// from a base definition carries one: origin.kind "synthesized", a base id,
// and the assumed installed set as name:arch.
//
// It is written out as literal JSON rather than marshalled from
// snapshot.Snapshot, deliberately. What install reads is a file of bytes that
// crossed an air gap; building the fixture from the Go type would quietly
// test this package against whatever that type happens to serialise to today,
// and would make the tests here depend on core/snapshot at run time in a
// package that must not depend on it at all. The separate guard in
// snapshotdoc_guard_test.go is where the two shapes are held to each other -
// including a check that these very fixtures are documents core/snapshot's
// own Validate accepts, so they cannot drift into a shape debark never
// writes.
func synthesizedSnapshotJSON(baseID string, assumed []string) string {
	quoted := make([]string, len(assumed))
	for i, a := range assumed {
		quoted[i] = strconv.Quote(a)
	}
	return `{
  "schema_version": "debark.snapshot/v1",
  "created_at": "2026-09-06T12:00:00Z",
  "tool": { "name": "debark", "version": "1.0.0" },
  "target": {
    "distro_id": "debian", "version_id": "12", "codename": "bookworm",
    "arch": "amd64", "apt_version": "2.6.1", "dpkg_version": "1.21.22"
  },
  "dpkg_status": {
    "path": "/var/lib/dpkg/status",
    "archive_path": "var/lib/dpkg/status",
    "size": 4096,
    "sha256": "` + fakeSHA256("synthesized dpkg status") + `"
  },
  "origin": {
    "kind": "synthesized",
    "base_id": ` + strconv.Quote(baseID) + `,
    "source": "builtin",
    "source_digest": "` + fakeSHA256("base definition") + `",
    "assumed_installed": [` + strings.Join(quoted, ", ") + `]
  }
}`
}

// capturedSnapshotJSON is the ordinary case: a measurement of one real
// machine. core/snapshot's Validate refuses base fields and an
// assumed_installed list on such a document, so there are none here.
func capturedSnapshotJSON() string {
	return `{
  "schema_version": "debark.snapshot/v1",
  "created_at": "2026-09-06T12:00:00Z",
  "tool": { "name": "debark", "version": "1.0.0" },
  "target": {
    "distro_id": "debian", "version_id": "12", "codename": "bookworm",
    "arch": "amd64", "apt_version": "2.6.1", "dpkg_version": "1.21.22"
  },
  "dpkg_status": {
    "path": "/var/lib/dpkg/status",
    "archive_path": "var/lib/dpkg/status",
    "size": 4096,
    "sha256": "` + fakeSHA256("captured dpkg status") + `"
  },
  "origin": { "kind": "captured" }
}`
}

// writeBundleSnapshot drops a snapshot.json into an already-built fixture
// bundle. newFixtureBundle deliberately does not write one - most of this
// package's tests are about a bundle's lock and pool, not its provenance -
// so the tests that care add it explicitly.
func writeBundleSnapshot(t *testing.T, bundleDir, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bundleDir, snapshotDocName), []byte(doc), 0o644); err != nil {
		t.Fatalf("write %s: %v", snapshotDocName, err)
	}
}

// fakeDpkgQuery points the helper process's dpkg-query branch at a file
// holding the given lines, in the exact shape
// `dpkg-query -W --showformat='${Package}:${Architecture} ${Status}\n'`
// produces. Every line here was copied from real output (WSL Ubuntu 24.04,
// dpkg 1.22.6, 2026-09-06): "adduser:all install ok installed",
// "apparmor:amd64 install ok installed", and so on.
func fakeDpkgQuery(t *testing.T, lines ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dpkg-query-out.txt")
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fake dpkg-query output: %v", err)
	}
	t.Setenv("FAKE_DPKG_QUERY_FILE", path)
}

// planWithSnapshot runs Plan against a fixture bundle carrying the given
// snapshot.json, with apt's simulation wired up so the run reaches the end
// rather than stopping on a plan problem.
func planWithSnapshot(t *testing.T, snapshotDoc string) (*Report, []([]string)) {
	t.Helper()
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	if snapshotDoc != "" {
		writeBundleSnapshot(t, bundleDir, snapshotDoc)
	}
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	log := newFakeLogFile(t)

	report, err := New(deps).Plan(context.Background(), bundleDir, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return report, fakeArgvLog(t, log)
}

// hasWarningContaining is the assertion helper the rest of this package
// already writes inline; named here because these tests make it six times.
func hasWarningContaining(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// --- dpkg's three-field ${Status} -------------------------------------------

// TestParseDpkgInstalledSet is the trap this project has already paid for,
// pinned. dpkg's ${Status} is three fields - desired action, error flag,
// current status - and only the last two say whether the package is on the
// machine. A HELD package reports "hold ok installed": installed, configured,
// usable, differing only because someone asked apt not to change it.
//
// This test is written so that it FAILS against a naive
// strings.Contains(status, "install ok installed"): "hold ok installed" does
// not contain that substring, so the naive reading calls a held package
// absent. Here the negation is the only direction that matters - a false
// "not installed" becomes a package reported as missing from the assumed
// base, which turns an ordinary, correctly pinned machine into a report
// claiming the bundle is short.
func TestParseDpkgInstalledSet(t *testing.T) {
	out := strings.Join([]string{
		"adduser:all install ok installed",
		"bash:amd64 install ok installed",
		"zlib1g:i386 hold ok installed", // held: PRESENT
		"nano:amd64 deinstall ok config-files",
		"gcc-12:amd64 install ok half-configured",
		"perl:amd64 install reinstreq installed",
		"ifupdown: unknown ok not-installed",
		"", // blank lines and short lines are skipped, never guessed at
		"garbage",
	}, "\n")

	got := parseDpkgInstalledSet([]byte(out))

	want := map[string]bool{"adduser:all": true, "bash:amd64": true, "zlib1g:i386": true}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is not in the installed set; dpkg reported it installed", name)
		}
	}
	for _, name := range []string{"nano:amd64", "gcc-12:amd64", "perl:amd64", "ifupdown:", "garbage"} {
		if got[name] {
			t.Errorf("%s is in the installed set; dpkg did not report it installed", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("installed set = %v, want exactly %v", got, want)
	}
}

// TestParseDpkgInstalledSet_CRLF: the helper process writes with whatever
// newline the platform's os.WriteFile was handed, and this suite runs on
// Windows as well as Linux. A stray \r would end up glued to the status word
// and every package would read as not-installed.
func TestParseDpkgInstalledSet_CRLF(t *testing.T) {
	got := parseDpkgInstalledSet([]byte("bash:amd64 install ok installed\r\nzlib1g:i386 hold ok installed\r\n"))
	if !got["bash:amd64"] || !got["zlib1g:i386"] {
		t.Errorf("CRLF output parsed as %v", got)
	}
}

func TestDpkgStatusInstalled(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"install ok installed", true},
		{"hold ok installed", true}, // the trap
		{"deinstall ok installed", false},
		{"purge ok installed", false},
		{"install ok unpacked", false},
		{"install ok half-configured", false},
		{"install reinstreq installed", false},
		{"unknown ok not-installed", false},
	}
	for _, tc := range cases {
		f := strings.Fields(tc.status)
		if got := dpkgStatusInstalled(f[0], f[1], f[2]); got != tc.want {
			t.Errorf("dpkgStatusInstalled(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// --- the comparison ---------------------------------------------------------

// TestCompareAssumedInstalled_Multiarch is the arch-blindness this project
// has already been bitten by twice, asserted directly: an i386 package the
// base assumed is NOT satisfied by the amd64 build of the same name. A
// comparison keyed on bare names would call this machine complete and the
// bundle would be short by a real library.
func TestCompareAssumedInstalled_Multiarch(t *testing.T) {
	origin := bundleOrigin{
		Kind:             originSynthesized,
		BaseID:           "debian:12/minimal",
		AssumedInstalled: []string{"libfoo:i386", "libfoo:amd64", "bash:amd64"},
	}
	installed := map[string]bool{"libfoo:amd64": true, "bash:amd64": true}

	d, unusable := compareAssumedInstalled(origin, installed)
	if unusable != 0 {
		t.Errorf("unusable = %d, want 0", unusable)
	}
	if d.Assumed != 3 {
		t.Errorf("Assumed = %d, want 3", d.Assumed)
	}
	if len(d.Missing) != 1 || d.Missing[0] != "libfoo:i386" {
		t.Fatalf("Missing = %v, want exactly [libfoo:i386] - libfoo:amd64 must not satisfy libfoo:i386", d.Missing)
	}
}

func TestCompareAssumedInstalled_DuplicatesCountOnce(t *testing.T) {
	origin := bundleOrigin{
		Kind:             originSynthesized,
		AssumedInstalled: []string{"bash:amd64", "bash:amd64", "gone:amd64", "gone:amd64", "  ", ""},
	}
	d, _ := compareAssumedInstalled(origin, map[string]bool{"bash:amd64": true})
	if d.Assumed != 2 {
		t.Errorf("Assumed = %d, want 2 (a duplicated entry is one package)", d.Assumed)
	}
	if len(d.Missing) != 1 || d.Missing[0] != "gone:amd64" {
		t.Errorf("Missing = %v, want [gone:amd64] once", d.Missing)
	}
}

// TestCompareAssumedInstalled_TerminalControlCharactersAreDropped: install
// prints a sample of these names, and core/snapshot's Validate refuses a
// document carrying control characters in them for exactly that reason
// (the threat model - a name carrying ESC[1A ESC[2K can scroll back over
// the digest a reviewer was told to compare). Validate ran on the machine
// that built the bundle, though, and this one runs on the target reading
// bytes that crossed an air gap, so the check is made again here rather than
// inherited.
func TestCompareAssumedInstalled_TerminalControlCharactersAreDropped(t *testing.T) {
	origin := bundleOrigin{
		Kind: originSynthesized,
		AssumedInstalled: []string{
			"bash:amd64",
			"evil\x1b[1A\x1b[2K:amd64",
			"also evil:amd64", // an embedded space is not a package name either
			strings.Repeat("x", maxEntryLen+1) + ":amd64",
		},
	}
	d, unusable := compareAssumedInstalled(origin, map[string]bool{"bash:amd64": true})
	if unusable != 3 {
		t.Errorf("unusable = %d, want 3", unusable)
	}
	if d.Assumed != 1 {
		t.Errorf("Assumed = %d, want 1", d.Assumed)
	}
	for _, m := range d.Missing {
		if !printableEntry(m) {
			t.Errorf("Missing carries an unprintable entry %q", m)
		}
	}
}

// --- the sentence an operator reads -----------------------------------------

func TestDivergenceSentence_NothingMissing(t *testing.T) {
	got := divergenceSentence(&BaseDivergence{BaseID: "ubuntu:26.04/desktop", Assumed: 1847})
	if !strings.Contains(got, "1,847") {
		t.Errorf("sentence does not carry the grouped total: %q", got)
	}
	if !strings.Contains(got, "nothing it assumed is missing") {
		t.Errorf("sentence does not state the clean result: %q", got)
	}
}

// TestDivergenceSentence_BoundedAndSorted: an operator on a serial console
// does not want four hundred names. The sentence names the first ten of the
// sorted list and says how many it did not name; BaseDivergence.Missing keeps
// the lot for --json.
func TestDivergenceSentence_BoundedAndSorted(t *testing.T) {
	var missing []string
	for i := 0; i < 25; i++ {
		missing = append(missing, fmt.Sprintf("pkg%02d:amd64", i))
	}
	// Shuffled going in, so the ordering asserted below is this code's doing
	// and not the caller's.
	sort.Sort(sort.Reverse(sort.StringSlice(missing)))
	d, _ := compareAssumedInstalled(
		bundleOrigin{Kind: originSynthesized, BaseID: "b", AssumedInstalled: missing},
		map[string]bool{})

	got := divergenceSentence(d)
	if !strings.HasPrefix(got, "25 of the 25 assumed base packages are not present") {
		t.Errorf("sentence = %q", got)
	}
	if !strings.Contains(got, "and 15 more") {
		t.Errorf("sentence does not bound the sample: %q", got)
	}
	if strings.Contains(got, "pkg10:amd64") {
		t.Errorf("sentence names an eleventh package: %q", got)
	}
	for i := 0; i < maxNamedMissing; i++ {
		name := fmt.Sprintf("pkg%02d:amd64", i)
		if !strings.Contains(got, name) {
			t.Errorf("sentence does not name %s, which sorts within the first %d: %q", name, maxNamedMissing, got)
		}
	}
	// Sorted, in the sentence as well as in the field.
	if at0, at1 := strings.Index(got, "pkg00:amd64"), strings.Index(got, "pkg01:amd64"); at0 > at1 {
		t.Errorf("sample is not in sorted order: %q", got)
	}
	if len(d.Missing) != 25 {
		t.Errorf("Missing carries %d entries, want the complete 25 - the bound is on the sentence, not on the field", len(d.Missing))
	}
}

// TestDivergenceSentence_EmptyAssumedSet: "0 of 0 missing" is literally true
// and reads as a clean bill of health for a check that had nothing to check.
// core/base cannot produce such a base, so a document that carries one is not
// one debark wrote, and the sentence says what actually happened.
func TestDivergenceSentence_EmptyAssumedSet(t *testing.T) {
	got := divergenceSentence(&BaseDivergence{BaseID: "b", Assumed: 0})
	if strings.Contains(got, "nothing it assumed is missing") {
		t.Errorf("an empty assumed set reported as a clean result: %q", got)
	}
	if !strings.Contains(got, "nothing to compare") {
		t.Errorf("sentence = %q", got)
	}
}

func TestGroupThousands(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1847: "1,847", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

// --- end to end through Plan and Apply --------------------------------------

// TestExecute_SynthesizedBase_NothingMissing: the operator is told the bundle
// rests on an assumption even when the assumption holds perfectly. The
// warning is not conditional on a finding, because "this bundle was not built
// from a snapshot of your machine" is itself the finding; the count line
// simply reports zero.
func TestExecute_SynthesizedBase_NothingMissing(t *testing.T) {
	fakeDpkgQuery(t,
		"bash:amd64 install ok installed",
		"libc6:amd64 install ok installed",
		"adduser:all install ok installed",
	)
	report, _ := planWithSnapshot(t, synthesizedSnapshotJSON("ubuntu:26.04/desktop",
		[]string{"adduser:all", "bash:amd64", "libc6:amd64"}))

	if !hasWarningContaining(report.Warnings, "built against an assumed base (ubuntu:26.04/desktop)") {
		t.Errorf("no assumed-base warning; warnings = %v", report.Warnings)
	}
	if !hasWarningContaining(report.Warnings, "nothing it assumed is missing") {
		t.Errorf("no clean-result warning; warnings = %v", report.Warnings)
	}
	if report.BaseDivergence == nil {
		t.Fatal("BaseDivergence is nil for a synthesized bundle")
	}
	if report.BaseDivergence.Assumed != 3 || len(report.BaseDivergence.Missing) != 0 {
		t.Errorf("BaseDivergence = %+v, want Assumed 3 and no Missing", report.BaseDivergence)
	}
	if report.BaseDivergence.BaseID != "ubuntu:26.04/desktop" {
		t.Errorf("BaseID = %q", report.BaseDivergence.BaseID)
	}
	// It warns; it never refuses.
	if !report.OK {
		t.Errorf("a divergence finding must not make the plan not-OK; problems = %v", report.Problems)
	}
}

// TestExecute_SynthesizedBase_SomeMissing is the headline case: a machine
// that has FEWER packages than the base assumed, which is the direction that
// produces a short bundle.
func TestExecute_SynthesizedBase_SomeMissing(t *testing.T) {
	fakeDpkgQuery(t,
		"bash:amd64 install ok installed",
		"libc6:amd64 install ok installed",
	)
	report, _ := planWithSnapshot(t, synthesizedSnapshotJSON("ubuntu:26.04/desktop",
		[]string{"bash:amd64", "gnome-shell:amd64", "libc6:amd64", "xserver-xorg:amd64"}))

	d := report.BaseDivergence
	if d == nil {
		t.Fatal("BaseDivergence is nil")
	}
	if d.Assumed != 4 {
		t.Errorf("Assumed = %d, want 4", d.Assumed)
	}
	want := []string{"gnome-shell:amd64", "xserver-xorg:amd64"}
	if strings.Join(d.Missing, ",") != strings.Join(want, ",") {
		t.Errorf("Missing = %v, want %v (sorted)", d.Missing, want)
	}
	if !hasWarningContaining(report.Warnings, "2 of the 4 assumed base packages are not present") {
		t.Errorf("count sentence missing; warnings = %v", report.Warnings)
	}
	if !hasWarningContaining(report.Warnings, "gnome-shell:amd64, xserver-xorg:amd64") {
		t.Errorf("sample missing; warnings = %v", report.Warnings)
	}
	if !report.OK {
		t.Errorf("divergence must warn, never refuse; problems = %v", report.Problems)
	}
}

// TestExecute_SynthesizedBase_HeldPackageIsPresent exercises a known trap
// through the whole path rather than only against the parser: a package the
// operator has HELD is installed, and must not be
// reported as a gap in the base. Against a naive
// strings.Contains(status, "install ok installed") this test fails, because
// "hold ok installed" does not contain that substring and zlib1g:amd64 would
// come back as missing.
func TestExecute_SynthesizedBase_HeldPackageIsPresent(t *testing.T) {
	fakeDpkgQuery(t,
		"bash:amd64 install ok installed",
		"zlib1g:amd64 hold ok installed",
	)
	report, _ := planWithSnapshot(t, synthesizedSnapshotJSON("debian:12/minimal",
		[]string{"bash:amd64", "zlib1g:amd64"}))

	d := report.BaseDivergence
	if d == nil {
		t.Fatal("BaseDivergence is nil")
	}
	if len(d.Missing) != 0 {
		t.Fatalf("Missing = %v; a held package reports \"hold ok installed\" and IS installed", d.Missing)
	}
	if d.Assumed != 2 {
		t.Errorf("Assumed = %d, want 2", d.Assumed)
	}
}

// TestExecute_SynthesizedBase_MultiarchThroughThePlan: libfoo:i386 assumed,
// only libfoo:amd64 installed. Same assertion as the unit test above, made
// through the real dpkg-query output shape, because the two halves of this
// (asking dpkg for ${Package}:${Architecture} rather than ${binary:Package},
// and comparing the qualified strings) can each be right on their own and
// still not meet.
func TestExecute_SynthesizedBase_MultiarchThroughThePlan(t *testing.T) {
	fakeDpkgQuery(t,
		"libfoo:amd64 install ok installed",
		"bash:amd64 install ok installed",
	)
	report, _ := planWithSnapshot(t, synthesizedSnapshotJSON("debian:12/minimal",
		[]string{"bash:amd64", "libfoo:i386"}))

	d := report.BaseDivergence
	if d == nil {
		t.Fatal("BaseDivergence is nil")
	}
	if len(d.Missing) != 1 || d.Missing[0] != "libfoo:i386" {
		t.Fatalf("Missing = %v, want [libfoo:i386]; the amd64 build of the same name must not satisfy it", d.Missing)
	}
}

// TestExecute_CapturedSnapshot_ProducesNothing: no warning, no field, and no
// dpkg-query invocation. The last of the three is the one worth asserting
// through the argv log - a captured snapshot is the overwhelmingly common
// case, and "no cost" is a claim about what ran, not only about what the
// report says.
func TestExecute_CapturedSnapshot_ProducesNothing(t *testing.T) {
	fakeDpkgQuery(t, "bash:amd64 install ok installed")
	report, argv := planWithSnapshot(t, capturedSnapshotJSON())

	if report.BaseDivergence != nil {
		t.Errorf("BaseDivergence = %+v for a captured snapshot, want nil", report.BaseDivergence)
	}
	for _, w := range report.Warnings {
		if strings.Contains(w, "assumed base") || strings.Contains(w, "divergence") {
			t.Errorf("captured snapshot produced a base warning: %q", w)
		}
	}
	for _, call := range argv {
		for _, a := range call[1:] {
			if a == "-W" {
				t.Errorf("dpkg-query ran for a captured snapshot: %v", call)
			}
		}
	}
}

// TestExecute_SnapshotJSONMissing_WarnsAndProceeds. A bundle without a
// snapshot.json must still install: verification has already proved the bytes
// are the ones the signed manifest names, and this is a reporting feature, so
// refusing would put debark's own paperwork ahead of the operator's outage.
// It warns rather than staying silent because silence is ambiguous in the
// wrong direction - an operator who sees nothing cannot tell "captured from
// your machine, nothing to say" from "nobody checked".
func TestExecute_SnapshotJSONMissing_WarnsAndProceeds(t *testing.T) {
	report, _ := planWithSnapshot(t, "") // no snapshot.json written at all

	if !report.OK {
		t.Errorf("a bundle with no snapshot.json must still plan cleanly; problems = %v", report.Problems)
	}
	if report.BaseDivergence != nil {
		t.Errorf("BaseDivergence = %+v, want nil", report.BaseDivergence)
	}
	if !hasWarningContaining(report.Warnings, "could not tell whether this bundle was built against an assumed base") {
		t.Errorf("no degradation warning; warnings = %v", report.Warnings)
	}
}

func TestExecute_SnapshotJSONMalformed_WarnsAndProceeds(t *testing.T) {
	report, _ := planWithSnapshot(t, "{ this is not json")

	if !report.OK {
		t.Errorf("a bundle with an unparseable snapshot.json must still plan cleanly; problems = %v", report.Problems)
	}
	if report.BaseDivergence != nil {
		t.Errorf("BaseDivergence = %+v, want nil", report.BaseDivergence)
	}
	if !hasWarningContaining(report.Warnings, "could not tell whether this bundle was built against an assumed base") {
		t.Errorf("no degradation warning; warnings = %v", report.Warnings)
	}
}

// TestExecute_DpkgQueryFails_WarnsAndProceeds: the machine's installed set
// could not be read. Same rule - a dpkg-query that cannot answer is not
// evidence about the bundle, so it must not change the bundle's fate - but
// the assumed-base warning still stands, because that one is a fact about the
// bundle and needs no query to establish.
func TestExecute_DpkgQueryFails_WarnsAndProceeds(t *testing.T) {
	fakeDpkgQuery(t)
	t.Setenv("FAKE_DPKG_QUERY_EXIT", "2")
	report, _ := planWithSnapshot(t, synthesizedSnapshotJSON("debian:12/minimal", []string{"bash:amd64"}))

	if !report.OK {
		t.Errorf("a failed dpkg-query must not fail the plan; problems = %v", report.Problems)
	}
	if report.BaseDivergence != nil {
		t.Errorf("BaseDivergence = %+v, want nil when the machine could not be queried", report.BaseDivergence)
	}
	if !hasWarningContaining(report.Warnings, "built against an assumed base") {
		t.Errorf("the assumed-base warning needs no query and must survive one failing; warnings = %v", report.Warnings)
	}
	if !hasWarningContaining(report.Warnings, "could not list this machine's installed packages") {
		t.Errorf("no query-failure warning; warnings = %v", report.Warnings)
	}
}

// TestExecute_SynthesizedBase_ReportedBeforeAptRuns. The finding must reach
// the operator above apt's own output rather than underneath a screen of it,
// and a run that dies at `apt-get update` must still carry it. Asserted on
// the call order, which is the only thing that actually guarantees either.
func TestExecute_SynthesizedBase_ReportedBeforeAptRuns(t *testing.T) {
	fakeDpkgQuery(t, "bash:amd64 install ok installed")
	_, argv := planWithSnapshot(t, synthesizedSnapshotJSON("debian:12/minimal", []string{"bash:amd64", "gone:amd64"}))

	queryAt, aptAt := -1, -1
	for i, call := range argv {
		for _, a := range call[1:] {
			if a == "-W" && queryAt < 0 {
				queryAt = i
			}
			if (a == "update" || a == "install") && aptAt < 0 {
				aptAt = i
			}
		}
	}
	if queryAt < 0 {
		t.Fatalf("dpkg-query never ran; calls = %v", argv)
	}
	if aptAt < 0 {
		t.Fatalf("apt-get never ran; calls = %v", argv)
	}
	if queryAt > aptAt {
		t.Errorf("the base-divergence query ran at call %d, after apt at call %d", queryAt, aptAt)
	}
}

// TestApply_SynthesizedBase_CarriesTheFinding: Apply runs Plan and then
// installs, so the finding must be on the applied report too. Plan and Apply
// share execute(), which is what makes this true of the control flow rather
// than only of two independent code paths - and is what this asserts.
func TestApply_SynthesizedBase_CarriesTheFinding(t *testing.T) {
	lk := simpleLock()
	bundleDir := newFixtureBundle(t, fixtureOptions{Lock: lk})
	writeBundleSnapshot(t, bundleDir, synthesizedSnapshotJSON("debian:12/minimal",
		[]string{"bash:amd64", "gone:amd64"}))
	deps := baseDeps(t, &fakeVerifier{report: okVerifyReport(lk.Target)})
	deps.Root = newRootFixture(t, "debian", "12", "bookworm")
	t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
	fakeDpkgQuery(t, "bash:amd64 install ok installed")

	report, err := New(deps).Apply(context.Background(), bundleDir, Options{Yes: true, EvidencePath: filepath.Join(t.TempDir(), "evidence.jsonl")})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !report.Applied || !report.OK {
		t.Fatalf("Apply did not succeed: applied=%v ok=%v problems=%v", report.Applied, report.OK, report.Problems)
	}
	d := report.BaseDivergence
	if d == nil || len(d.Missing) != 1 || d.Missing[0] != "gone:amd64" {
		t.Fatalf("Apply lost the base-divergence finding: %+v", d)
	}
}
