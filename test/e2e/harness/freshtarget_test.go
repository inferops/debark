package harness

import (
	"regexp"
	"strings"
	"testing"

	"github.com/inferops/debark/core/distro"
)

// This file guards the property added on 2026-09-05: the fresh target is the
// machine the snapshot captured, not a stock release image.
//
// What can honestly be tested here, and what cannot, is worth stating plainly
// rather than papering over. The claim itself — "installing this bundle on
// this machine works, and would not have worked on a different one" — is only
// observable with containers: it needs a real dpkg with i386 added, a real
// apt-mark hold, a real apt refusing or accepting a real bundle. Those rows
// are foreign-arch-i386, multiarch-coexist, held-package and
// stale-installed-version, and only a matrix run can settle them.
//
// What IS testable without Docker is the plumbing that makes the claim
// reachable at all, and every test below is aimed at the specific
// single-token edit that would quietly undo it: naming the release image
// where the committed one belongs, sharing one image name between two
// concurrent rows, producing a reference Docker will not accept, or dropping
// a line from the pre-commit strip that keeps the target's own apt cache from
// standing in for the bundle. Each of those compiles, runs, and shows up only
// as fixtures that stop testing what they say they test — the failure mode
// this whole harness exists to make impossible.

func testRun(fixture, versionID, arch string) *fixtureRun {
	return &fixtureRun{
		s:       Scenario{Name: fixture, Target: TargetSpec{Arch: arch}},
		opts:    RunOpts{RunID: "rdeadbeef"},
		release: distro.Release{DistroID: "debian", VersionID: versionID},
		log:     &strings.Builder{},
		row:     &RowResult{ExitClasses: map[string]string{}, StageMS: map[string]int64{}},
		byRole:  map[string]*Container{},
	}
}

// dockerRefRe is Docker's own reference grammar for the part of a name this
// harness generates: lowercase alphanumerics in groups separated by a single
// period, one or two underscores, or one or more dashes, optionally followed
// by ":tag". A reference that violates it is rejected by the daemon with a
// message ("invalid reference format") that says nothing about which of the
// dozen things in a row's name was wrong, on a code path that only runs
// minutes into a container-only test.
var dockerRefRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?::[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127})?$`)

// TestFreshTargetImageRefusesTheStockImage is the guard for the regression
// itself. freshTargetImage must have no fallback: an empty targetImage means
// the row never froze the snapshotted machine, and answering that with the
// release image is exactly the bug fixed on 2026-09-05 — silently, since the
// row then runs green against a machine nobody described.
func TestFreshTargetImageRefusesTheStockImage(t *testing.T) {
	r := testRun("held-package", "12", "amd64")

	ref, err := r.freshTargetImage()
	if err == nil {
		t.Fatalf("freshTargetImage returned %q for a row that never committed the state container; "+
			"it must refuse, because any image it names here is the wrong machine", ref)
	}
	if ref != "" {
		t.Errorf("freshTargetImage returned both an error and the image %q; a caller that ignores the error must not get a usable substitute", ref)
	}
	// The message is the only thing standing between a future reader and
	// "just default it to the release image", so it has to say so.
	if !strings.Contains(err.Error(), "release image") {
		t.Errorf("error %q does not warn against falling back to the stock release image", err)
	}

	r.targetImage = "dfe2e-rdeadbeef-held-package-12-amd64-targetimg:e2e"
	got, err := r.freshTargetImage()
	if err != nil {
		t.Fatalf("freshTargetImage after a commit: %v", err)
	}
	if got != r.targetImage {
		t.Errorf("freshTargetImage = %q, want the committed image %q", got, r.targetImage)
	}
	if got == r.release.Ref() {
		t.Errorf("freshTargetImage returned the release image %q", got)
	}
}

// TestTargetImageRefIsDockerLegal catches a reference the daemon would reject
// only at `docker commit` time, minutes into a row.
func TestTargetImageRefIsDockerLegal(t *testing.T) {
	for _, tc := range []struct{ fixture, version, arch string }{
		{"held-package", "12", "amd64"},
		{"multiarch-coexist", "24.04", "amd64"},      // a version with a '.' in it
		{"foreign-arch-i386", "24.04", "arm64"},      // a fixture name with digits
		{"Mixed Case & Symbols!", "12", "amd64"},     // a hostile fixture name
		{strings.Repeat("long-", 40), "12", "arm64"}, // long enough to be hash-suffixed
	} {
		ref := testRun(tc.fixture, tc.version, tc.arch).targetImageRef()
		if !dockerRefRe.MatchString(ref) {
			t.Errorf("targetImageRef(%q, %q, %q) = %q, which is not a legal Docker reference", tc.fixture, tc.version, tc.arch, ref)
		}
		if ref != strings.ToLower(ref) {
			t.Errorf("targetImageRef(%q, ...) = %q contains an uppercase letter; a Docker repository name may not", tc.fixture, ref)
		}
	}
}

// TestTargetImageRefIsUniquePerRow is the image-level twin of the container
// name collision containerName's own comment records: a target.matrix:true
// fixture runs at more than one architecture against the same release
// concurrently, and a shared image name would have one row's `docker commit`
// retag the other row's target machine mid-run — the second row would then
// install its bundle on the first row's machine, which is the very confusion
// this change removes.
func TestTargetImageRefIsUniquePerRow(t *testing.T) {
	rows := []struct{ fixture, version, arch string }{
		{"basic-install-matrix", "12", "amd64"},
		{"basic-install-matrix", "12", "arm64"},
		{"basic-install-matrix", "13", "amd64"},
		{"held-package", "12", "amd64"},
	}
	seen := map[string]string{}
	for _, row := range rows {
		ref := testRun(row.fixture, row.version, row.arch).targetImageRef()
		label := row.fixture + "/" + row.version + "/" + row.arch
		if prev, dup := seen[ref]; dup {
			t.Errorf("rows %s and %s share the image reference %q", prev, label, ref)
		}
		seen[ref] = label
		if !strings.Contains(ref, "rdeadbeef") {
			t.Errorf("%s: image reference %q does not carry the run id, so Sweep cannot scope it to this run", label, ref)
		}
	}
}

// TestPreCommitScriptStripsHarnessScaffolding pins the three things that must
// come off the state container before it becomes the target's image. Each has
// its own way of producing a green row that proves nothing, and each is a
// plausible "cleanup" for someone shortening this script:
//
//   - /debark left behind means the install stage can run a binary the
//     pipeline never actually transferred across the air gap.
//   - /work left behind bakes the snapshot tarball and the harness's key
//     directory into the machine under test.
//   - the apt archive cache left behind lets apt satisfy a selection from the
//     target's own cached .deb instead of the bundle's pool, so a bundle
//     missing that file installs cleanly anyway (see preCommitScript's own
//     comment for why the private root does not stop this).
func TestPreCommitScriptStripsHarnessScaffolding(t *testing.T) {
	for _, want := range []string{"rm -rf /work", "rm -f /debark", "apt-get clean"} {
		if !strings.Contains(preCommitScript, want) {
			t.Errorf("preCommitScript does not %q:\n%s", want, preCommitScript)
		}
	}
	// Without `set -e` a failing rm is swallowed and the image is committed
	// with the scaffolding still in it — the silent version of every failure
	// above.
	if !strings.HasPrefix(preCommitScript, "set -e") {
		t.Errorf("preCommitScript does not start with `set -e`, so a failed strip would commit anyway:\n%s", preCommitScript)
	}
	// /var/lib/apt/lists is deliberately kept: it is part of what the
	// snapshotted machine looks like and cannot make an install succeed for
	// the wrong reason. Asserting its absence here keeps a well-meant
	// "shrink the image" edit from quietly changing the machine.
	if strings.Contains(preCommitScript, "/var/lib/apt/lists") {
		t.Errorf("preCommitScript removes /var/lib/apt/lists; those index files are part of the snapshotted machine and install cannot acquire through them:\n%s", preCommitScript)
	}
}

// TestCommitArgsLabelTheImageForSweeping is the other half of "the harness
// sweeps what it creates". A committed image that does not carry
// LabelRun=<run id> is invisible to Sweep, so a crashed process leaves it in
// the daemon's store permanently — the quiet leak this host has seen before,
// and one nothing later can attribute to a run.
func TestCommitArgsLabelTheImageForSweeping(t *testing.T) {
	r := testRun("held-package", "12", "amd64")
	labels := r.containerLabels("targetimage")
	args := commitArgs("cafe1234", r.targetImageRef(), labels)

	joined := strings.Join(args, " ")
	for _, want := range []string{
		`LABEL ` + LabelRun + `="rdeadbeef"`,
		`LABEL ` + LabelMarker + `="1"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("commit argv does not carry %s:\n%v", want, args)
		}
	}
	// The container and the new reference are positional and must be the
	// last two words, in that order: swap them and docker commits the image
	// name as if it were a container.
	if got := args[len(args)-2:]; got[0] != "cafe1234" || got[1] != r.targetImageRef() {
		t.Errorf("commit argv ends with %v, want [container ref]", got)
	}
	if args[0] != "commit" {
		t.Errorf("commit argv starts with %q, want \"commit\"", args[0])
	}
}

// TestCommitArgsQuoteLabelValues guards the one way a label can quietly stop
// being the label Sweep looks for: `--change` is parsed as a Dockerfile
// instruction, so an unquoted value containing a space becomes two labels.
func TestCommitArgsQuoteLabelValues(t *testing.T) {
	args := commitArgs("c1", "img:e2e", map[string]string{LabelRun: "r1 and more"})
	for i, a := range args {
		if a == "--change" && i+1 < len(args) {
			if got, want := args[i+1], `LABEL `+LabelRun+`="r1 and more"`; got != want {
				t.Errorf("label instruction = %q, want %q", got, want)
			}
		}
	}
}
