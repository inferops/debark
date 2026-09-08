package cli

// Golden tests for the human-summary renderers and the --json shapes,
// driven entirely by hand-built value structs (never by calling into
// another package's implementation), per the contract brief: these must stay
// green regardless of how much of core/{verify,install,doctor,bundle,engine}
// is finished in this checkout.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/doctor"
	"github.com/inferops/debark/core/install"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/verify"
	"github.com/inferops/debark/internal/cli/render"
)

func testCtx(jsonMode bool, colour bool) (*Ctx, *bytes.Buffer) {
	var out bytes.Buffer
	return &Ctx{
		Stdout:   &out,
		Stderr:   &bytes.Buffer{},
		Style:    render.Style{Enabled: colour},
		jsonMode: jsonMode,
	}, &out
}

// --- verify -----------------------------------------------------------

func sampleVerifyReportOK() *verify.Report {
	return &verify.Report{
		SchemaVersion: verify.SchemaVersion,
		CheckedAt:     "2026-09-03T12:00:00Z",
		BundlePath:    "/bundles/site-42",
		BundleID:      "bd-1",
		OK:            true,
		Signed:        true,
		Signatures: []verify.SignatureResult{
			{SignerKind: "ed25519-file", KeyID: "a1b2c3d4", Algorithm: "ed25519", Valid: true, Trusted: true},
		},
		FilesChecked: 17,
		BytesChecked: 9_800_000,
		Target:       verify.ReportTarget{DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64"},
	}
}

func sampleVerifyReportFailed() *verify.Report {
	return &verify.Report{
		SchemaVersion: verify.SchemaVersion,
		BundlePath:    "/media/usb/bundle",
		OK:            false,
		Signed:        true,
		Signatures: []verify.SignatureResult{
			{SignerKind: "ed25519-file", KeyID: "a1b2c3d4", Algorithm: "ed25519", Valid: true, Trusted: true},
		},
		FilesChecked: 17,
		BytesChecked: 9_800_000,
		Problems: []verify.Problem{
			{
				Kind:     verify.ProblemFileDigest,
				Path:     "repo/pool/h/htop/htop_3.3.0-1_amd64.deb",
				Message:  "file digest mismatch",
				Expected: "sha256:7c2e...",
				Got:      "sha256:41af...",
			},
		},
		Target: verify.ReportTarget{DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64"},
	}
}

func TestPrintVerifyHumanOK(t *testing.T) {
	ctx, out := testCtx(false, false)
	printVerifyHuman(ctx, sampleVerifyReportOK())
	got := out.String()
	for _, want := range []string{"OK", "/bundles/site-42", "ubuntu 24.04 (noble) amd64", "valid by a1b2c3d4", "17 files", "9.8 MB"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("colour disabled but ANSI present:\n%q", got)
	}
}

func TestPrintVerifyHumanFailedShowsProblem(t *testing.T) {
	ctx, out := testCtx(false, false)
	printVerifyHuman(ctx, sampleVerifyReportFailed())
	got := out.String()
	for _, want := range []string{
		"FAILED", "/media/usb/bundle",
		"problem: repo/pool/h/htop/htop_3.3.0-1_amd64.deb: file digest mismatch",
		"expected sha256:7c2e..., got sha256:41af...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPrintVerifyHumanColourEnabledEmitsANSI(t *testing.T) {
	ctx, out := testCtx(false, true)
	printVerifyHuman(ctx, sampleVerifyReportOK())
	if !strings.ContainsRune(out.String(), '\x1b') {
		t.Error("colour enabled but no ANSI escape found")
	}
}

func TestRenderVerifyReportJSONShape(t *testing.T) {
	ctx, out := testCtx(true, false)
	err := renderVerifyReport(ctx, sampleVerifyReportOK())
	if err != nil {
		t.Fatalf("renderVerifyReport: %v", err)
	}
	var got verify.Report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if got.SchemaVersion != verify.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", got.SchemaVersion, verify.SchemaVersion)
	}
	if !got.OK || got.BundlePath != "/bundles/site-42" {
		t.Errorf("got = %+v", got)
	}
	if strings.ContainsRune(out.String(), '\x1b') {
		t.Error("--json output must never contain ANSI escapes")
	}
}

func TestRenderVerifyReportNotOKReturnsVerificationClass(t *testing.T) {
	ctx, _ := testCtx(false, false)
	err := renderVerifyReport(ctx, sampleVerifyReportFailed())
	if err == nil {
		t.Fatal("want a non-nil error when report.OK is false")
	}
	if dferr.ExitCode(err) != 4 {
		t.Errorf("class = %d, want 4 (verification)", dferr.ExitCode(err))
	}
}

// The stderr line must NAME the failure, not merely restate that there was
// one: a failing command that writes "failed verification" and nothing else
// is what this replaced.
func TestVerifyFailureErrorNamesTheProblem(t *testing.T) {
	r := sampleVerifyReportFailed()
	r.Problems = append(r.Problems, verify.Problem{
		Kind: verify.ProblemFileUnexpected, Path: "repo/stray.deb", Message: "file not in the manifest",
	})
	got := verifyFailureError(r).Error()
	for _, want := range []string{
		"/media/usb/bundle",
		"2 problems",
		"repo/pool/h/htop/htop_3.3.0-1_amd64.deb: file digest mismatch",
		"(and 1 more)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in stderr line:\n%s", want, got)
		}
	}
}

func TestVerifyFailureErrorHintsAtTrustOnlyForTrustProblems(t *testing.T) {
	digestFailure := sampleVerifyReportFailed()
	if h := dferr.HintOf(verifyFailureError(digestFailure)); h != "" {
		t.Errorf("a digest mismatch is not a missing-key problem; got hint %q", h)
	}

	untrusted := sampleVerifyReportFailed()
	untrusted.Problems = []verify.Problem{{
		Kind: verify.ProblemSignatureUntrusted, Message: "no signature verifies against a trusted key",
	}}
	if h := dferr.HintOf(verifyFailureError(untrusted)); !strings.Contains(h, "--key") {
		t.Errorf("want a hint naming --key for an untrusted signature; got %q", h)
	}
}

// --- install ------------------------------------------------------------

func sampleInstallReport(ok, applied bool) *install.Report {
	return &install.Report{
		SchemaVersion:  install.SchemaVersion,
		BundlePath:     "/bundles/site-42",
		Verify:         &verify.Report{OK: true},
		Applied:        applied,
		OK:             ok,
		ToInstall:      []string{"vim=2:9.1.0-1", "tmux=3.4-1"},
		ToUpgrade:      nil,
		AlreadyCurrent: 3,
		TargetExpected: lock.Target{DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64"},
		TargetActual:   lock.Target{DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64"},
	}
}

func TestPrintInstallHumanPlan(t *testing.T) {
	ctx, out := testCtx(false, false)
	printInstallHuman(ctx, sampleInstallReport(true, false))
	got := out.String()
	for _, want := range []string{"install plan for", "to install: 2", "unchanged:  3", "ubuntu 24.04 (noble) amd64"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPrintInstallHumanApplied(t *testing.T) {
	ctx, out := testCtx(false, false)
	printInstallHuman(ctx, sampleInstallReport(true, true))
	if !strings.Contains(out.String(), "installed /bundles/site-42") {
		t.Errorf("got:\n%s", out.String())
	}
}

func TestInstallExitClassPrefersVerifyFailure(t *testing.T) {
	r := sampleInstallReport(false, false)
	r.Verify.OK = false
	if got := installExitClass(r); got != 4 {
		t.Errorf("installExitClass = %d, want 4 (verification)", got)
	}
}

func TestInstallFailureErrorNamesTheProblem(t *testing.T) {
	r := sampleInstallReport(false, false)
	r.Problems = []string{"dpkg returned 1 for libfoo1"}
	if got := installFailureError(r).Error(); !strings.Contains(got, "dpkg returned 1 for libfoo1") {
		t.Errorf("stderr line does not name the problem:\n%s", got)
	}
}

func TestInstallFailureErrorNamesTheArchMismatch(t *testing.T) {
	r := sampleInstallReport(false, false)
	r.TargetActual.Arch = "arm64"
	got := installFailureError(r)
	if !strings.Contains(got.Error(), "the bundle is for amd64, this machine is arm64") {
		t.Errorf("stderr line does not name the mismatch:\n%s", got.Error())
	}
	if dferr.ExitCode(got) != 7 {
		t.Errorf("class = %d, want 7 (target-mismatch)", dferr.ExitCode(got))
	}
}

func TestInstallExitClassDetectsArchMismatch(t *testing.T) {
	r := sampleInstallReport(false, false)
	r.TargetActual.Arch = "arm64"
	if got := installExitClass(r); got != 7 {
		t.Errorf("installExitClass = %d, want 7 (target-mismatch)", got)
	}
}

// --- build ----------------------------------------------------------------

func sampleSnapshot() *snapshot.Snapshot {
	return &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Target: snapshot.Target{
			DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
		},
	}
}

func sampleBuildResult(class buildjob.ExitClass) *buildjob.BuildResult {
	return &buildjob.BuildResult{
		SchemaVersion: buildjob.SchemaVersion,
		BundlePath:    "./site-42-bundle",
		Signed:        true,
		Stats: buildjob.Stats{
			Added: 14, Unchanged: 0, Removed: 0,
			Bytes: 9_800_000, DownloadedBytes: 9_800_000, PackageCount: 14,
		},
		ExitClass: class,
	}
}

func TestPrintBuildHumanAnswersFourQuestions(t *testing.T) {
	ctx, out := testCtx(false, false)
	printBuildHuman(ctx, sampleSnapshot(), sampleBuildResult(buildjob.ExitSuccess))
	got := out.String()
	// what target
	if !strings.Contains(got, "ubuntu 24.04 (noble) amd64") {
		t.Errorf("missing target in:\n%s", got)
	}
	// what will be installed
	if !strings.Contains(got, "14 packages") {
		t.Errorf("missing package count in:\n%s", got)
	}
	// how much must be downloaded
	if !strings.Contains(got, "9.8 MB") {
		t.Errorf("missing size in:\n%s", got)
	}
	// can this artifact be trusted
	if !strings.Contains(got, "trust:      signed\n") {
		t.Errorf("missing exact trust line in:\n%s", got)
	}
}

func TestPrintBuildHumanUnsignedSaysSo(t *testing.T) {
	ctx, out := testCtx(false, false)
	r := sampleBuildResult(buildjob.ExitSuccess)
	r.Signed = false
	printBuildHuman(ctx, sampleSnapshot(), r)
	if !strings.Contains(out.String(), "trust:      unsigned\n") {
		t.Errorf("got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "trust:      signed\n") {
		t.Errorf("unsigned result must not also match the signed line: got:\n%s", out.String())
	}
}

func TestRenderBuildResultIncompleteExitsThree(t *testing.T) {
	ctx, _ := testCtx(false, false)
	r := sampleBuildResult(buildjob.ExitIncomplete)
	r.Unresolved = []string{"acme-agent"}
	err := renderBuildResult(ctx, sampleSnapshot(), r)
	if err == nil {
		t.Fatal("want an error for an incomplete result")
	}
	if dferr.ExitCode(err) != 3 {
		t.Errorf("class = %d, want 3 (incomplete)", dferr.ExitCode(err))
	}
}

// A failing build must name the package and the URL on stderr. Before this,
// the class was the only thing that crossed: "incomplete" and nothing else.
func TestBuildFailureErrorNamesUnresolvedAndFailedURLs(t *testing.T) {
	r := sampleBuildResult(buildjob.ExitIncomplete)
	r.Unresolved = []string{"acme-agent", "acme-tools"}
	r.FetchFailed = []string{"https://vendor.example/acme_1.0_amd64.deb"}

	err := buildFailureError(r)
	got := err.Error()
	for _, want := range []string{
		"./site-42-bundle", "incomplete",
		"2 inputs apt could not satisfy: acme-agent, acme-tools",
		"1 URL that did not download: https://vendor.example/acme_1.0_amd64.deb",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in stderr line:\n%s", want, got)
		}
	}
	if dferr.HintOf(err) == "" {
		t.Error("an incomplete build should say what to do next")
	}
}

func TestBuildFailureErrorElidesLongLists(t *testing.T) {
	r := sampleBuildResult(buildjob.ExitIncomplete)
	r.Unresolved = []string{"a", "b", "c", "d", "e"}
	got := buildFailureError(r).Error()
	if !strings.Contains(got, "a, b, c and 2 more") {
		t.Errorf("want the list elided after three names, got:\n%s", got)
	}
}

// A closed-world failure has no unresolved input and no failed URL; its
// explanation is a warning sentence, and without it the line said only
// "resolution".
func TestBuildFailureErrorFallsBackToTheClosedWorldWarning(t *testing.T) {
	r := sampleBuildResult(buildjob.ExitResolution)
	r.Warnings = []string{
		"bundle is unsigned",
		"closed-world check failed: this bundle may not install cleanly with no network. libfoo1 is unsatisfiable",
	}
	got := buildFailureError(r).Error()
	if !strings.Contains(got, "libfoo1 is unsatisfiable") {
		t.Errorf("want the closed-world warning, not the first warning, in:\n%s", got)
	}
	if dferr.ExitCode(buildFailureError(r)) != 5 {
		t.Errorf("class = %d, want 5 (resolution)", dferr.ExitCode(buildFailureError(r)))
	}
}

func TestExitClassFromStringRoundTripsBuildjobConstants(t *testing.T) {
	cases := map[buildjob.ExitClass]int{
		buildjob.ExitSuccess:      0,
		buildjob.ExitUsage:        1,
		buildjob.ExitEnvironment:  2,
		buildjob.ExitIncomplete:   3,
		buildjob.ExitVerification: 4,
		buildjob.ExitResolution:   5,
		buildjob.ExitPolicy:       6,
		buildjob.ExitTarget:       7,
	}
	for ec, want := range cases {
		if got := int(exitClassFromString(string(ec))); got != want {
			t.Errorf("exitClassFromString(%q) = %d, want %d", ec, got, want)
		}
	}
}

// --- doctor -----------------------------------------------------------

func TestPrintDoctorHumanNoFindings(t *testing.T) {
	ctx, out := testCtx(false, false)
	printDoctorHuman(ctx, &doctor.Report{Scanned: 5})
	if !strings.Contains(out.String(), "no obvious issues found") {
		t.Errorf("got:\n%s", out.String())
	}
}

func TestPrintDoctorHumanWithFindings(t *testing.T) {
	ctx, out := testCtx(false, false)
	printDoctorHuman(ctx, &doctor.Report{
		Scanned: 5,
		Findings: []doctor.Finding{
			{Check: doctor.CheckSnapShim, Severity: doctor.SeverityWarn, Package: "chromium", Version: "1.0", Message: "installs a snap shim", Evidence: "postinst calls snap install"},
			{Check: doctor.CheckRedistribution, Severity: doctor.SeverityNote, Package: "libfoo", Message: "from multiverse"},
		},
	})
	got := out.String()
	for _, want := range []string{"snap-shim", "installs a snap shim", "chromium 1.0", "postinst calls snap install", "redistribution", "from multiverse"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// --- inspect ------------------------------------------------------------

func sampleBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Dir: "/bundles/site-42",
		Manifest: &manifest.Manifest{
			SchemaVersion: manifest.SchemaVersion,
			BundleID:      "bd-1",
			CreatedAt:     "2026-09-03T12:00:00Z",
			Tool:          manifest.Tool{Name: "debark", Version: "1.0.0", Edition: "community"},
			Target:        manifest.Target{DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64"},
			Repository:    manifest.Repository{PackageCount: 14, PoolBytes: 9_800_000},
			Files:         make([]manifest.File, 17),
		},
		Signature: &manifest.SignatureFile{
			Schema: manifest.SignatureSchemaVersion,
			Signatures: []manifest.Signature{
				{SignerKind: manifest.SignerEd25519File, KeyID: "a1b2c3d4", Algorithm: "ed25519"},
			},
		},
		Lock: &lock.Lock{
			SchemaVersion: lock.SchemaVersion,
			Resolver:      lock.Resolver{Backend: lock.BackendLocal, APTVersion: "2.8.3", DpkgVersion: "1.22.6"},
			Install:       []string{"vim=2:9.1.0-1", "tmux=3.4-1"},
			ClosedWorld:   lock.ClosedWorld{Result: lock.ClosedWorldOK},
			Warnings: []lock.Warning{
				{Code: "redistribution.multiverse", Message: "vlc is in multiverse", Packages: []string{"vlc"}},
			},
		},
	}
}

func TestPrintInspectHuman(t *testing.T) {
	ctx, out := testCtx(false, false)
	b := sampleBundle()
	printInspectHuman(ctx, b, true)
	got := out.String()
	for _, want := range []string{
		"/bundles/site-42", "bd-1", "ubuntu 24.04 (noble) amd64",
		"14 (9.8 MB pool)", "17", "yes (ed25519-file a1b2c3d4)",
		"local backend, apt 2.8.3 / dpkg 1.22.6", "2 packages",
		"closed-world check: ok", "redistribution.multiverse",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestInspectJSONSchemaVersion(t *testing.T) {
	b := sampleBundle()
	out := inspectJSON{
		SchemaVersion: inspectSchemaVersion,
		BundlePath:    b.Dir,
		Signed:        true,
		Manifest:      b.Manifest,
		Lock:          b.Lock,
	}
	enc, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(enc, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != inspectSchemaVersion {
		t.Errorf("schema_version = %v", m["schema_version"])
	}
	if _, ok := m["manifest"].(map[string]any); !ok {
		t.Error("manifest not embedded as an object")
	}
	if _, ok := m["lock"].(map[string]any); !ok {
		t.Error("lock not embedded as an object")
	}
}

// --- build --recommends / --no-recommends ---------------------------------

// Options.Recommends is a *bool and the engine means all three of its states,
// but only two were reachable from the command line: --no-recommends existed
// and nothing turned it back on. A target whose own Install-Recommends is off
// therefore could not be given a bundle that includes recommends at all.
func TestRecommendsOverrideHasThreeStates(t *testing.T) {
	if got := recommendsOverride(false, false); got != nil {
		t.Errorf("neither flag = %v, want nil (follow the target's apt.conf)", *got)
	}
	got := recommendsOverride(true, false)
	if got == nil || !*got {
		t.Errorf("--recommends = %v, want a pointer to true", got)
	}
	got = recommendsOverride(false, true)
	if got == nil || *got {
		t.Errorf("--no-recommends = %v, want a pointer to false", got)
	}
}
