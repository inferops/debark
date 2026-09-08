package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
)

// TestFindingsCarryNoTerminalControlCharacters is the regression test for the
// injection display.go describes. Both halves of a doctor report are
// attacker-supplied — Evidence is a line lifted verbatim out of a maintainer
// script in the .deb, and the redistribution check's Evidence and the
// Package/Version on every finding come from the bundle's own lock.json — and
// `debark doctor BUNDLE` reads all of it through bundle.Open, which never
// verifies. internal/cli then prints them with a plain %s.
//
// Measured unsanitised, both of the payloads below arrived intact: cursor-up
// and erase-line, aimed at the human comparison docs/threat-model.md §6
// asks a reviewer to perform on the output of this very command.
func TestFindingsCarryNoTerminalControlCharacters(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The line must fire the network heuristic, or there is no finding to
	// carry the payload.
	evil := "\x1b[2A\x1b[2K  everything looks fine  curl http://a.example/x"
	deb := buildFixtureDeb(t, fixtureDeb{
		Scripts: map[string]string{"postinst": "#!/bin/sh\n" + evil + "\n"},
	})
	if err := os.WriteFile(filepath.Join(dir, "repo", "pool", "e.deb"), deb, 0o644); err != nil {
		t.Fatal(err)
	}

	l := &lock.Lock{Packages: []lock.Package{{
		Name:     "evil\x1b[31m",
		Version:  "1.0\x07",
		Filename: "pool/e.deb",
		Reason:   lock.ReasonExternal,
		// U+009B is C1 CSI: one rune, no ESC byte, and a terminal acts on it
		// exactly as it does on the two-byte ESC-[.
		Origin: lock.Origin{URI: "https://x/\u009b2K\u009b1Aforged"},
	}}}

	report, err := Run(context.Background(), Input{BundleDir: dir, Lock: l, ScanScripts: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) == 0 {
		t.Fatal("no findings; the payloads need a finding to travel in")
	}
	for _, f := range report.Findings {
		for field, s := range map[string]string{
			"Package": f.Package, "Version": f.Version,
			"Message": f.Message, "Evidence": f.Evidence,
		} {
			if i := strings.IndexFunc(s, isTerminalControl); i >= 0 {
				t.Errorf("%s.%s carries a control character at %d: %q", f.Check, field, i, s)
			}
		}
	}

	// Repair, not refusal: the finding itself must survive, or suppressing it
	// would be the false clean result this package must never produce.
	var sawScript bool
	for _, f := range report.Findings {
		if f.Check == CheckNetworkPostinst && strings.Contains(f.Evidence, "curl http://a.example/x") {
			sawScript = true
		}
	}
	if !sawScript {
		t.Errorf("the network finding was lost along with its escapes; findings: %+v", report.Findings)
	}
}

// isTerminalControl is core/snapshot/displaystrings.go's hasControlChars as a
// rune predicate: C0, DEL, and the C1 range whose members include the 8-bit
// CSI.
func isTerminalControl(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// TestEvidenceIsBounded protects maxEvidenceLen. A maintainer script has no
// obligation to contain a newline: measured, a script that is ONE line of
// 8,388,566 bytes containing "curl" produced a single Evidence string of
// 8,388,578 bytes, which the CLI prints in full and --json embeds.
func TestEvidenceIsBounded(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	oneLine := "curl http://a.example/" + strings.Repeat("x", 4<<20)
	deb := buildFixtureDeb(t, fixtureDeb{Scripts: map[string]string{"postinst": oneLine}})
	if err := os.WriteFile(filepath.Join(dir, "repo", "pool", "long.deb"), deb, 0o644); err != nil {
		t.Fatal(err)
	}
	l := &lock.Lock{Packages: []lock.Package{
		{Name: "long", Version: "1.0", Filename: "pool/long.deb"},
	}}
	report, err := Run(context.Background(), Input{BundleDir: dir, Lock: l, ScanScripts: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) == 0 {
		t.Fatal("no findings for a script line that matches")
	}
	for _, f := range report.Findings {
		// displaySafe can expand what it keeps, so the assertion is on the
		// order of magnitude the bound sets, not on the exact byte count.
		if len(f.Evidence) > 6*maxEvidenceLen {
			t.Errorf("%s Evidence is %d bytes, bound is %d", f.Check, len(f.Evidence), maxEvidenceLen)
		}
		if len(f.Message) > 6*maxEvidenceLen {
			t.Errorf("%s Message is %d bytes, bound is %d", f.Check, len(f.Message), maxEvidenceLen)
		}
	}
}

// TestDisplaySafeLeavesRealEvidenceAlone is the other side of the sanitiser:
// it must be the identity on everything a real report contains, or it would
// be quietly rewriting the evidence an operator is asked to judge.
func TestDisplaySafeLeavesRealEvidenceAlone(t *testing.T) {
	for _, s := range []string{
		"postinst:5: curl --create-dirs -o \"${TARGETDIR}/index.fits\" \"${BASE_URL}/index.fits\"",
		"pool/main/h/hello/hello_2.10-3_amd64.deb (source package: hello)",
		"missing linux-headers-6.1.0-13-amd64",
		"https://packages.example.com/vendor/agent_2.1.0_amd64.deb",
		"description: Transitional package for the chromium snap",
		"café — naïve — 日本語", // non-ASCII is text, not control
	} {
		if got := displaySafe(s, maxEvidenceLen); got != s {
			t.Errorf("displaySafe rewrote real evidence:\n  in  %q\n  out %q", s, got)
		}
	}
}

// TestUnscannableDebIsNotReportedAsClean covers the false clean result
// checkNetworkPostinst now closes. A .deb whose control member uses a
// compression this build cannot decode used to set debFile.Unscannable, which
// nothing read — so the package reached the report looking exactly like one
// with no maintainer scripts, and the CLI renders that as
// NoNetworkActionFound.
func TestUnscannableDebIsNotReportedAsClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A .deb whose control member names a compression decompressMember does
	// not implement. Nothing else about it is malformed.
	raw := buildAr([]arMember{
		{Name: "debian-binary", Data: []byte("2.0\n")},
		{Name: "control.tar.lzma", Data: []byte("not real lzma data")},
	})
	if err := os.WriteFile(filepath.Join(dir, "repo", "pool", "opaque.deb"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	l := &lock.Lock{Packages: []lock.Package{
		{Name: "opaque", Version: "1.0", Filename: "pool/opaque.deb"},
	}}

	report, err := Run(context.Background(), Input{BundleDir: dir, Lock: l, ScanScripts: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got *Finding
	for i := range report.Findings {
		if report.Findings[i].Check == CheckNetworkPostinst {
			got = &report.Findings[i]
		}
	}
	if got == nil {
		t.Fatalf("a .deb whose scripts could not be read produced no network-postinst finding; findings: %+v", report.Findings)
	}
	if got.Severity != SeverityWarn {
		t.Errorf("severity = %q, want %q", got.Severity, SeverityWarn)
	}
	// No lock flag: the package is unread, not known to touch the network,
	// and policy's deny_flags matches lock flags rather than check ids.
	if got.Flag != "" {
		t.Errorf("Flag = %q, want empty for an unreadable package", got.Flag)
	}
	if !strings.Contains(got.Evidence, "lzma") {
		t.Errorf("Evidence should name why it could not be read, got %q", got.Evidence)
	}
}

// TestScannableDebStillReportsNothingWhenClean is the guard against the fix
// above turning into noise: a .deb doctor CAN read, whose scripts contain no
// signal, must still produce no network-postinst finding. Experiment E7 found
// 0 unscannable packages in 1,749 real ones, so this is the case that
// actually happens.
func TestScannableDebStillReportsNothingWhenClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	deb := buildFixtureDeb(t, fixtureDeb{
		Scripts: map[string]string{"postinst": "#!/bin/sh\nset -e\nldconfig\n"},
	})
	if err := os.WriteFile(filepath.Join(dir, "repo", "pool", "quiet.deb"), deb, 0o644); err != nil {
		t.Fatal(err)
	}
	l := &lock.Lock{Packages: []lock.Package{
		{Name: "quiet", Version: "1.0", Filename: "pool/quiet.deb"},
	}}
	report, err := Run(context.Background(), Input{BundleDir: dir, Lock: l, ScanScripts: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range report.Findings {
		if f.Check == CheckNetworkPostinst {
			t.Errorf("a readable, quiet .deb produced a network-postinst finding: %+v", f)
		}
	}
}

// TestMissingDebStaysSilent keeps the documented benign case benign: a lock
// entry naming a file the bundle does not carry (mid-build, or a pool that
// was pruned) is core/verify's business, not a doctor warning on every
// package.
func TestMissingDebStaysSilent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &lock.Lock{Packages: []lock.Package{
		{Name: "absent", Version: "1.0", Filename: "pool/absent.deb"},
	}}
	report, err := Run(context.Background(), Input{BundleDir: dir, Lock: l, ScanScripts: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("a missing .deb should stay silent, got %+v", report.Findings)
	}
}

// TestFlagsForMatchesSanitisedNames pins the coupling sanitiseFinding creates:
// Run rewrites Finding.Package, so FlagsFor has to put its argument through
// the same rewrite or core/engine's doctorFlagsFor(report.Findings, pkg.Name)
// would silently return nothing for such a package.
func TestFlagsForMatchesSanitisedNames(t *testing.T) {
	const raw = "weird\x1b[31m"
	findings := []Finding{{
		Check: CheckSnapShim, Severity: SeverityWarn,
		Package: displaySafe(raw, maxNameLen), Flag: lock.FlagSnapShim,
	}}
	if got := FlagsFor(findings, raw); len(got) != 1 || got[0] != lock.FlagSnapShim {
		t.Errorf("FlagsFor(%q) = %v, want [%s]", raw, got, lock.FlagSnapShim)
	}
}

// TestParseDebReaderRejectsNothingItUsedToAccept is a cheap belt on the two
// new refusals: an ordinary .deb round-trips unchanged.
func TestParseDebReaderRejectsNothingItUsedToAccept(t *testing.T) {
	deb := buildFixtureDeb(t, fixtureDeb{
		Control: "Package: hello\nVersion: 2.10-3\nDescription: friendly\n a folded line\n .\n and another\n",
		Scripts: map[string]string{"postinst": "#!/bin/sh\nldconfig\n"},
	})
	d, err := parseDebReader(bytes.NewReader(deb))
	if err != nil {
		t.Fatalf("parseDebReader: %v", err)
	}
	if got := stanzaGet(d.Control, "Package"); got != "hello" {
		t.Errorf("Package = %q, want hello", got)
	}
	if got := stanzaGet(d.Control, "Description"); !strings.Contains(got, "a folded line") ||
		!strings.Contains(got, "and another") {
		t.Errorf("folded Description not reassembled: %q", got)
	}
}
