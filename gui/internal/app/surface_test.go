package app

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/snapshot"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/export"
	"github.com/inferops/debark/gui/internal/readiness"
)

// ---------------------------------------------------------------------------
// SelectionKeys — what the virtualised list marks its rows from.
// ---------------------------------------------------------------------------

func TestSelectionKeysAnswersTheVirtualisersQuestion(t *testing.T) {
	a, _ := newApp(t)

	empty := a.SelectionKeys()
	if len(empty.Keys) != 0 || empty.Total != 0 || empty.Truncated {
		t.Fatalf("an empty tray reported %+v", empty)
	}

	added := a.AddPackages([]string{"vim", "curl"})
	if added.Error != nil {
		t.Fatalf("AddPackages: %v", added.Error)
	}
	if added.Revision == empty.Revision {
		t.Fatal("adding two packages did not move the revision")
	}

	got := a.SelectionKeys()
	if !slices.Equal(got.Keys, []string{"vim", "curl"}) {
		t.Fatalf("keys = %v, want the tray order [vim curl]", got.Keys)
	}
	if got.Total != 2 {
		t.Fatalf("total = %d, want 2", got.Total)
	}
	// The whole point of the revision: the summary the event carried and the
	// keys the list fetched must agree about which tray they describe.
	if got.Revision != added.Revision {
		t.Fatalf("SelectionKeys revision %d disagrees with selection:changed's %d",
			got.Revision, added.Revision)
	}

	// Re-adding a name already in the tray changes the entry, not the key set.
	// A revision that moved here would make the list refetch on every add.
	again := a.AddPackages([]string{"vim"})
	if again.Revision != added.Revision {
		t.Fatalf("revision moved from %d to %d without the key set changing",
			added.Revision, again.Revision)
	}

	removed := a.RemovePackages([]string{"vim"})
	if removed.Revision == added.Revision {
		t.Fatal("removing a package did not move the revision")
	}
	if k := a.SelectionKeys(); !slices.Equal(k.Keys, []string{"curl"}) {
		t.Fatalf("keys after a removal = %v, want [curl]", k.Keys)
	}

	// Clearing must move the revision FORWARDS. A counter that reset would let
	// a stale response look current, which is the one thing it exists to stop.
	cleared := a.ClearSelection()
	if cleared.Revision <= removed.Revision {
		t.Fatalf("revision went from %d to %d across a clear; it must only increase",
			removed.Revision, cleared.Revision)
	}
}

// The keys handed out must be the caller's own, for the same reason
// ReadinessReport.clone exists: the tray mutates its order slice in place.
func TestSelectionKeysAreTheCallersOwn(t *testing.T) {
	a, _ := newApp(t)
	a.AddPackages([]string{"vim", "curl"})

	first := a.SelectionKeys()
	first.Keys[0] = "clobbered"
	if a.SelectionKeys().Keys[0] == "clobbered" {
		t.Fatal("writing to a returned key slice changed the tray")
	}
}

// Past the ceiling the answer is "no keys", not "some keys": a partial set
// would silently draw selected rows as unselected.
func TestSelectionKeysRefusesRatherThanTruncates(t *testing.T) {
	a, _ := newApp(t)
	a.mu.Lock()
	for i := 0; i < SelectionKeysMax+1; i++ {
		a.sel.add(SelectionEntry{Key: packageNameForTest(i), Name: packageNameForTest(i), Source: SourceAPT})
	}
	a.mu.Unlock()

	got := a.SelectionKeys()
	if !got.Truncated {
		t.Fatalf("a tray of %d entries was not reported truncated", got.Total)
	}
	if len(got.Keys) != 0 {
		t.Fatalf("a truncated result carried %d keys; it must carry none", len(got.Keys))
	}
	if got.Total != SelectionKeysMax+1 {
		t.Fatalf("total = %d, want the true count %d", got.Total, SelectionKeysMax+1)
	}
}

func packageNameForTest(i int) string {
	const digits = "abcdefghij"
	out := []byte("pkg-")
	if i == 0 {
		return string(append(out, 'a'))
	}
	for i > 0 {
		out = append(out, digits[i%10])
		i /= 10
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// A destination that already holds an interrupted copy.
// ---------------------------------------------------------------------------

// The exporter has always known that a folder carrying the marker is unusable.
// Nothing crossed the bridge to say so, so the screen could not warn about the
// one thing an operator cannot see for themselves: a half-copied bundle looks
// exactly like a finished one.
func TestInspectDestinationWarnsAboutAnInterruptedCopy(t *testing.T) {
	a, _ := newApp(t)
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	// The exporter refuses an empty source, so the bundle needs something in
	// it before PlanExport will get as far as looking at the destination.
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	drive := filepath.Join(root, "usb")
	opts := ExportOptions{BundlePath: bundle, Destination: drive}

	fresh := a.InspectDestination(opts)
	if fresh.Error != nil {
		t.Fatalf("InspectDestination: %v", fresh.Error)
	}
	if fresh.Exists || fresh.Incomplete {
		t.Fatalf("a destination that does not exist reported %+v", fresh)
	}
	if want := filepath.Join(drive, "bundle"); fresh.Path != want {
		t.Fatalf("path = %q, want the folder StartExport would write, %q", fresh.Path, want)
	}
	if fresh.MarkerName != export.MarkerName {
		t.Fatalf("marker_name = %q, want %q", fresh.MarkerName, export.MarkerName)
	}

	// What an interrupted export leaves behind.
	dest := filepath.Join(drive, "bundle")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, export.MarkerName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := a.InspectDestination(opts)
	if got.Error != nil {
		t.Fatalf("InspectDestination: %v", got.Error)
	}
	if !got.Exists || !got.Incomplete {
		t.Fatalf("a marked destination reported %+v", got)
	}
	if got.Message == "" {
		t.Fatal("a marked destination carried no sentence for the operator")
	}

	// And the plan says it too, so a screen that only plans is not left blind.
	plan := a.PlanExport(opts)
	if plan.Error != nil {
		t.Fatalf("PlanExport: %v", plan.Error)
	}
	if !plan.Plan.DestinationIncomplete {
		t.Fatal("the plan did not report that the destination is marked incomplete")
	}
}

// ---------------------------------------------------------------------------
// What verification proved, said by the thing that did it.
// ---------------------------------------------------------------------------

// The two sentences the export screen shows about verification were hard-coded
// constants in the frontend — exactly the drift the view layer exists to
// prevent. They belong to the exporter, which is the only thing that knows
// what it checked, and they must cross the bridge.
func TestExportCarriesWhatVerificationProved(t *testing.T) {
	a, rec := newApp(t)
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	drive := filepath.Join(root, "usb")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := ExportOptions{BundlePath: bundle, Destination: drive}
	if r := a.StartExport(opts); !r.OK {
		t.Fatalf("StartExport: %v", r.Error)
	}
	waitFor(t, "export:finished", func() bool { return rec.count(EventExportFinished) == 1 })

	st := a.ExportStatus()
	if st.Error != nil {
		t.Fatalf("export failed: %v", st.Error)
	}
	if !st.Verified {
		t.Fatalf("a clean export was not reported verified: %+v", st.Progress)
	}
	if st.VerifyMethod == "" {
		t.Fatal("no verify_method crossed the bridge; the screen has nothing to say what was checked")
	}
	if st.VerifyCaveat == "" {
		t.Fatal("no verify_caveat crossed the bridge; the screen would overclaim")
	}
	if st.FilesChecked != 1 {
		t.Fatalf("files_checked = %d, want 1", st.FilesChecked)
	}
	if st.MismatchCount != 0 || len(st.Mismatches) != 0 {
		t.Fatalf("a passing verification reported %d mismatches", st.MismatchCount)
	}
}

// A failed verification must be able to name the files that failed. Without
// them "the copy did not verify" is a wall, and the operator's next question —
// which files, and how — has no answer anywhere on the surface.
//
// Driven through the projection rather than a real drive: producing a genuine
// mismatch means corrupting a file between the write and the read-back, which
// no test can do reliably. What matters here is that every field survives.
func TestExportListsTheFilesThatFailedVerification(t *testing.T) {
	var st ExportStatus
	applyVerification(&st, &export.VerifyReport{
		Method:       "every file was hashed, flushed and re-read",
		Caveat:       "this proves the drive returned what it was given",
		FilesChecked: 3,
		BytesChecked: 4096,
		Mismatches: []export.Mismatch{{
			Rel:        "pool/main/n/nginx/nginx_1.24-1_amd64.deb",
			Reason:     export.MismatchContent,
			Detail:     "the drive returned different bytes",
			WantSize:   2048,
			GotSize:    2048,
			WantSHA256: "aa",
			GotSHA256:  "bb",
		}, {
			Rel:    "manifest.json",
			Reason: export.MismatchMissing,
		}},
	})

	if st.VerifyMethod == "" || st.VerifyCaveat == "" {
		t.Fatalf("the exporter's own sentences did not survive: %+v", st)
	}
	if st.FilesChecked != 3 || st.BytesChecked != 4096 {
		t.Fatalf("checked counts = %d files / %d bytes, want 3 / 4096", st.FilesChecked, st.BytesChecked)
	}
	if st.MismatchCount != 2 || len(st.Mismatches) != 2 || st.MismatchesTruncated {
		t.Fatalf("mismatches = %d listed, count %d, truncated %v", len(st.Mismatches), st.MismatchCount, st.MismatchesTruncated)
	}

	first := st.Mismatches[0]
	if first.Path != "pool/main/n/nginx/nginx_1.24-1_amd64.deb" {
		t.Fatalf("path = %q", first.Path)
	}
	if first.Reason != string(export.MismatchContent) {
		t.Fatalf("reason = %q, want %q", first.Reason, export.MismatchContent)
	}
	// Right length, wrong bytes — the case a size check alone cannot see, and
	// the reason the digests have to reach the screen as evidence.
	if first.WantBytes != first.GotBytes {
		t.Fatalf("sizes disagreed on a content mismatch: %d vs %d", first.WantBytes, first.GotBytes)
	}
	if first.WantSHA256 != "aa" || first.GotSHA256 != "bb" {
		t.Fatalf("digests did not survive: %q vs %q", first.WantSHA256, first.GotSHA256)
	}
	if st.Mismatches[1].Reason != string(export.MismatchMissing) {
		t.Fatalf("second reason = %q, want %q", st.Mismatches[1].Reason, export.MismatchMissing)
	}
}

// The clamp, exercised directly: a failing drive fails everywhere at once, and
// a list of twenty thousand paths helps nobody.
func TestExportMismatchListIsClamped(t *testing.T) {
	var st ExportStatus
	report := &export.VerifyReport{Method: "m", Caveat: "c"}
	for i := 0; i < ExportMismatchMax+7; i++ {
		report.Mismatches = append(report.Mismatches, export.Mismatch{
			Rel:    packageNameForTest(i),
			Reason: export.MismatchContent,
		})
	}
	applyVerification(&st, report)

	if len(st.Mismatches) != ExportMismatchMax {
		t.Fatalf("listed %d mismatches, want the cap %d", len(st.Mismatches), ExportMismatchMax)
	}
	if !st.MismatchesTruncated {
		t.Fatal("a clamped list was not marked truncated")
	}
	if st.MismatchCount != ExportMismatchMax+7 {
		t.Fatalf("mismatch_count = %d, want the true total %d", st.MismatchCount, ExportMismatchMax+7)
	}
}

// ---------------------------------------------------------------------------
// Facts the target screen had and threw away.
// ---------------------------------------------------------------------------

// A synthesized snapshot's origin is the only thing distinguishing an
// assumption from a measurement, and the digest field promised an identity it
// never had for a captured one.
func TestSnapshotViewCarriesTheOrigin(t *testing.T) {
	captured := snapshotView("/tmp/target.tar.zst", snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Target: snapshot.Target{
			DistroID: "ubuntu", VersionID: "24.04", Codename: "noble",
			Arch: "amd64", ForeignArchs: []string{"i386"},
		},
		Origin:         snapshot.Origin{Kind: snapshot.OriginCaptured},
		InstalledCount: 1847,
	})
	if !slices.Equal(captured.ForeignArchs, []string{"i386"}) {
		t.Fatalf("foreign_archs = %v, want [i386]", captured.ForeignArchs)
	}
	// The trap the rename fixes: for a captured snapshot this is empty, and a
	// field called "digest" made that look like a missing identity.
	if captured.OriginSourceDigest != "" || captured.BaseID != "" || captured.OriginSource != "" {
		t.Fatalf("a captured snapshot claimed an origin base: %+v", captured)
	}

	synth := snapshotView("/tmp/from-base.tar.zst", snapshot.Snapshot{
		Target: snapshot.Target{DistroID: "ubuntu", VersionID: "26.04", Arch: "amd64"},
		Origin: snapshot.Origin{
			Kind:         snapshot.OriginSynthesized,
			BaseID:       "ubuntu:26.04/desktop",
			Source:       "builtin",
			SourceDigest: "3f2a",
		},
	})
	if synth.BaseID != "ubuntu:26.04/desktop" || synth.OriginSource != "builtin" {
		t.Fatalf("the base a synthesized snapshot came from did not cross: %+v", synth)
	}
	if synth.OriginSourceDigest != "3f2a" {
		t.Fatalf("origin_source_digest = %q, want %q", synth.OriginSourceDigest, "3f2a")
	}
	if synth.Variant != "desktop" {
		t.Fatalf("variant = %q, want desktop", synth.Variant)
	}
}

// Components and suites exist after resolution and nowhere else: a snapshot
// captures its sources as files, so nothing parsed reaches InspectSnapshot.
// "universe" present or absent is the difference between 70,000 rows and
// 25,000, so the target screen has to be able to say which.
func TestTargetViewReportsWhatTheCatalogueWillCover(t *testing.T) {
	got := targetView(
		TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"},
		catalog.Target{
			Kind: catalog.TargetBase, BaseID: "ubuntu:24.04/desktop",
			DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
			Sources: []catalog.Source{
				{Suites: []string{"noble", "noble-updates"}, Components: []string{"main", "universe"}},
				// A second stanza naming the same component must not repeat it.
				{Suites: []string{"noble-security"}, Components: []string{"main"}},
			},
		})

	if !slices.Equal(got.Components, []string{"main", "universe"}) {
		t.Fatalf("components = %v, want [main universe] deduplicated", got.Components)
	}
	if !slices.Equal(got.Suites, []string{"noble", "noble-updates", "noble-security"}) {
		t.Fatalf("suites = %v, want the target's own order", got.Suites)
	}
}

// ---------------------------------------------------------------------------
// The derived readiness row.
// ---------------------------------------------------------------------------

// build-environment has no probe: RunOne refuses it by name because it is
// computed from the apt, WSL and container rows. Nothing on the type said so,
// and it is one of only two BLOCKING rows — so a screen offering "check again"
// offered the button that matters most on the one row where it does nothing.
func TestDerivedReadinessRowSaysSoAndRefusesARecheck(t *testing.T) {
	a, rec := newApp(t)
	if r := a.StartReadinessCheck(); !r.OK {
		t.Fatalf("StartReadinessCheck: %v", r.Error)
	}
	waitFor(t, "readiness:finished", func() bool { return rec.count(EventReadinessFinished) == 1 })

	var derived, probed int
	for _, c := range a.Readiness().Checks {
		if c.Derived {
			derived++
			if c.ID != CheckBuildEnvironment {
				t.Fatalf("%q is marked derived and should not be", c.ID)
			}
			continue
		}
		probed++
	}
	if derived != 1 {
		t.Fatalf("%d rows marked derived, want exactly build-environment", derived)
	}
	if probed == 0 {
		t.Fatal("every row was derived; the report has no probes in it")
	}

	// Refused up front, with a sentence naming what to do instead — not
	// thirty milliseconds later out of a goroutine that spun the row first.
	r := a.RecheckReadiness(CheckBuildEnvironment)
	if r.OK {
		t.Fatal("a derived row accepted a re-check")
	}
	if r.Error == nil || r.Error.Hint == "" {
		t.Fatalf("the refusal did not say what to do instead: %+v", r.Error)
	}
	if got := rec.count(EventReadinessFinished); got != 1 {
		t.Fatalf("the refusal emitted readiness events (%d finished, want 1)", got)
	}
}

// The defect the derived flag exposes: re-checking one row must recompute the
// row derived from it. Folding view rows left build-environment stale, so an
// operator who started Docker and re-checked the container row saw it turn
// green while the row that governs the build button stayed red.
func TestRecheckingARowRederivesTheBlockingRow(t *testing.T) {
	a, rec := newApp(t)
	if r := a.StartReadinessCheck(); !r.OK {
		t.Fatalf("StartReadinessCheck: %v", r.Error)
	}
	waitFor(t, "readiness:finished", func() bool { return rec.count(EventReadinessFinished) == 1 })

	before, ok := readinessRow(a.Readiness(), CheckBuildEnvironment)
	if !ok {
		t.Skip("this platform contributes no build-environment row")
	}

	// Re-check whichever probed row this platform actually has. The outcome
	// does not matter; that the derived row is recomputed from it does.
	var probe string
	for _, c := range a.Readiness().Checks {
		if !c.Derived && c.ID != CheckBinary {
			probe = c.ID
			break
		}
	}
	if probe == "" {
		t.Skip("no probed row to re-check on this platform")
	}
	if r := a.RecheckReadiness(probe); !r.OK {
		t.Fatalf("RecheckReadiness(%q): %v", probe, r.Error)
	}
	waitFor(t, "readiness:finished (recheck)", func() bool { return rec.count(EventReadinessFinished) == 2 })

	after, ok := readinessRow(a.Readiness(), CheckBuildEnvironment)
	if !ok {
		t.Fatal("the derived row disappeared after a single re-check")
	}
	// It is still derived, still summarised, and still governs the build
	// button — a re-derivation that produced an empty row would break the
	// "a problem always has a remedy" invariant silently.
	if !after.Derived || after.Summary == "" {
		t.Fatalf("the re-derived row is malformed: %+v", after)
	}
	if after.Status == ReadinessProblem && after.Remedy == "" {
		t.Fatal("the re-derived row is a problem with no remedy: that is the wall")
	}
	if after.Severity != before.Severity && after.Status == before.Status {
		t.Fatalf("the row's severity changed without its status: %v then %v", before, after)
	}
}

func readinessRow(r ReadinessReport, id string) (ReadinessCheck, bool) {
	for _, c := range r.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return ReadinessCheck{}, false
}

// ---------------------------------------------------------------------------
// "Build somewhere else", as data.
// ---------------------------------------------------------------------------

// The advice existed only as prose inside one row's Remedy, so the screen
// pattern-matched on the platform string plus a set of check ids to decide
// whether to show it. That is a product decision living in the view layer,
// where a rewording of that Remedy would silently change when the panel
// appears.
func TestRemoteBuilderAdviceIsStructured(t *testing.T) {
	problem := func(id string) ReadinessCheck {
		return ReadinessCheck{ID: id, Status: ReadinessProblem, Severity: SeverityDegraded, Summary: "not available"}
	}
	fine := func(id string) ReadinessCheck {
		return ReadinessCheck{ID: id, Status: ReadinessOK, Severity: SeverityInfo, Summary: "available"}
	}

	cases := []struct {
		name     string
		platform string
		checks   []ReadinessCheck
		want     string // "" means no advice
		blocked  []string
	}{{
		name:     "a healthy linux builder is told nothing",
		platform: "linux/amd64",
		checks:   []ReadinessCheck{fine(CheckAPT), fine(CheckContainer), fine(CheckBuildEnvironment)},
	}, {
		name:     "nothing can run apt",
		platform: "linux/amd64",
		checks: []ReadinessCheck{
			problem(CheckAPT), problem(CheckContainer),
			{ID: CheckBuildEnvironment, Status: ReadinessProblem, Severity: SeverityBlocking, Summary: "no route"},
		},
		want:    RemoteBuilderNoEnvironment,
		blocked: []string{CheckAPT, CheckContainer},
	}, {
		// The managed-laptop case, and the reason the second arm exists: the
		// one local route is blocked, and this is the only advice that will
		// ever help.
		name:     "windows with wsl and docker both blocked",
		platform: "windows/amd64",
		checks:   []ReadinessCheck{problem(CheckWSL), problem(CheckContainer), fine(CheckBuildEnvironment)},
		want:     RemoteBuilderLocalBlocked,
		blocked:  []string{CheckWSL, CheckContainer},
	}, {
		// The Windows machine this was measured on, and the case the second arm could not
		// reach. Docker Desktop is running, WSL 2 is healthy, and there is no
		// Linux debark to mount into the container -- so the one local route
		// is broken. The old condition counted WSL and the container runtime
		// as two routes and fired only when BOTH were blocked, so a working
		// WSL 2 installation suppressed the panel entirely on exactly the
		// machine that needed it.
		//
		// The derived row now reaches this machine on its own, so the reason
		// is no-environment; the second arm is insurance against a stale
		// derived row. Both paths had to be fixed, because they were the same
		// arithmetic mistake one layer apart.
		name:     "windows where the container runtime runs but debark has no linux build of itself",
		platform: "windows/amd64",
		checks: []ReadinessCheck{
			fine(CheckWSL), fine(CheckContainer), problem(CheckSelfBinary),
			{ID: CheckBuildEnvironment, Status: ReadinessProblem, Severity: SeverityBlocking, Summary: "no route"},
		},
		want:    RemoteBuilderNoEnvironment,
		blocked: []string{CheckSelfBinary},
	}, {
		// The same machine with a derived row that has not caught up yet --
		// the frame between a self-binary re-check landing and the report
		// being re-derived. The advice must still be there.
		name:     "windows, self-binary blocked, derived row stale",
		platform: "windows/amd64",
		checks:   []ReadinessCheck{fine(CheckWSL), fine(CheckContainer), problem(CheckSelfBinary), fine(CheckBuildEnvironment)},
		want:     RemoteBuilderLocalBlocked,
		blocked:  []string{CheckSelfBinary},
	}, {
		// A healthy Windows builder. WSL being unhappy is not a route being
		// blocked, so it must not raise the panel on its own.
		name:     "windows with a working container route and unhappy wsl",
		platform: "windows/amd64",
		checks:   []ReadinessCheck{problem(CheckWSL), fine(CheckContainer), fine(CheckSelfBinary), fine(CheckBuildEnvironment)},
	}, {
		// The container runtime is running and debark still cannot use it,
		// because on a non-Linux host it mounts and re-execs a Linux build of
		// itself and there is not one. A panel that named only the container
		// row would be pointing at the green row and telling the operator to
		// install what they are already running.
		name:     "windows with no container runtime and no linux build either",
		platform: "windows/amd64",
		checks: []ReadinessCheck{
			problem(CheckWSL), problem(CheckContainer), problem(CheckSelfBinary),
			{ID: CheckBuildEnvironment, Status: ReadinessProblem, Severity: SeverityBlocking, Summary: "no route"},
		},
		want:    RemoteBuilderNoEnvironment,
		blocked: []string{CheckWSL, CheckContainer, CheckSelfBinary},
	}, {
		// Not Windows: a Linux box with no container runtime still has apt.
		name:     "linux with no container runtime",
		platform: "linux/amd64",
		checks:   []ReadinessCheck{fine(CheckAPT), problem(CheckContainer), fine(CheckBuildEnvironment)},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := ReadinessReport{Checks: tc.checks, Platform: tc.platform}
			rep.recount()

			if tc.want == "" {
				if rep.RemoteBuilder != nil {
					t.Fatalf("advice offered where none applies: %+v", rep.RemoteBuilder)
				}
				return
			}
			if rep.RemoteBuilder == nil {
				t.Fatal("no advice where it is the only thing that helps")
			}
			if rep.RemoteBuilder.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", rep.RemoteBuilder.Reason, tc.want)
			}
			if rep.RemoteBuilder.Message == "" || rep.RemoteBuilder.Hint == "" {
				t.Fatalf("advice with nothing to render: %+v", rep.RemoteBuilder)
			}
			if !slices.Equal(rep.RemoteBuilder.Blocked, tc.blocked) {
				t.Fatalf("blocked = %v, want %v", rep.RemoteBuilder.Blocked, tc.blocked)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Install-Recommends, now that the CLI has three states for it.
// ---------------------------------------------------------------------------

// The bool could not tell "follow the target" from "definitely exclude", which
// are different builds: a target whose apt.conf turns recommends on gets them
// under the first and not the second. The frozen frontend still sends the old
// no_recommends, so the two spellings must resolve deterministically.
func TestBuildOptionsRecommendsHasThreeStates(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		opts BuildOptions
		want *bool
	}{
		{"neither: follow the target", BuildOptions{}, nil},
		{"the old spelling still excludes", BuildOptions{NoRecommends: true}, &off},
		{"explicitly excluded", BuildOptions{Recommends: &off}, &off},
		{"explicitly included", BuildOptions{Recommends: &on}, &on},
		// They cannot contradict: the three-state field wins outright, so
		// there is no combination that needs a rule to resolve it.
		{"the new spelling wins over the old", BuildOptions{Recommends: &on, NoRecommends: true}, &on},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.opts.recommends()
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("recommends() = %v, want nil (no flag at all)", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("recommends() = nil, want %v", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("recommends() = %v, want %v", *got, *tc.want)
			}
		})
	}
}

// And it must survive the translation into a command line, or PreviewCommand
// shows the operator something the build will not run.
func TestPreviewCommandCarriesTheRecommendsOverride(t *testing.T) {
	a, _ := newApp(t)
	if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); r.Error != nil {
		t.Fatalf("SelectTarget: %v", r.Error)
	}
	if s := a.AddPackages([]string{"nginx"}); s.Error != nil {
		t.Fatalf("AddPackages: %v", s.Error)
	}
	on := true
	opts := BuildOptions{OutputDir: t.TempDir(), Recommends: &on}

	cmd := a.PreviewCommand(opts)
	if cmd.Error != nil {
		t.Fatalf("PreviewCommand: %v", cmd.Error)
	}
	if !slices.Contains(cmd.Argv, "--recommends") {
		t.Fatalf("the override did not reach the command line: %v", cmd.Argv)
	}
	if slices.Contains(cmd.Argv, "--no-recommends") {
		t.Fatalf("both flags were passed, which the CLI rejects: %v", cmd.Argv)
	}
}

// TestReadinessCheckIDsAreMirrored is the test that would have caught the
// self-binary constant never arriving here.
//
// internal/app declares its own copy of every check id, deliberately: the
// frontend keys off these and they are part of the frozen binding surface, so
// they must not move when a row is reworded inside internal/readiness. The
// cost of that deliberate duplication is that adding a check there and
// forgetting it here compiles, runs, and silently produces a row the bound
// surface has no name for — which is exactly what happened to self-binary.
func TestReadinessCheckIDsAreMirrored(t *testing.T) {
	mirrored := map[string]string{
		readiness.CheckBinary:           CheckBinary,
		readiness.CheckAPT:              CheckAPT,
		readiness.CheckWSL:              CheckWSL,
		readiness.CheckContainer:        CheckContainer,
		readiness.CheckSelfBinary:       CheckSelfBinary,
		readiness.CheckBuildEnvironment: CheckBuildEnvironment,
		readiness.CheckSigningKey:       CheckSigningKey,
		readiness.CheckDiskSpace:        CheckDiskSpace,
		readiness.CheckArchiveNetwork:   CheckArchiveNetwork,
	}
	for _, id := range readiness.CheckIDs() {
		got, ok := mirrored[id]
		if !ok {
			t.Errorf("internal/readiness can produce the check id %q and internal/app has no constant for it — add one to types.go and to the ReadinessCheck.id union in docs/dev/binding-surface.md", id)
			continue
		}
		if got != id {
			t.Errorf("internal/app's constant for %q is %q: the ids are what the frontend keys off and they must be identical", id, got)
		}
	}
	if len(mirrored) != len(readiness.CheckIDs()) {
		t.Errorf("this test names %d ids and internal/readiness produces %d", len(mirrored), len(readiness.CheckIDs()))
	}
}

// ---------------------------------------------------------------------------
// Handing an operator-chosen path to the operating system's URL handler.
// ---------------------------------------------------------------------------

// TestFileURIEscapesAnOperatorChosenPath is the defect, driven rather than
// described.
//
// OpenPath built its URL as "file://" + filepath.ToSlash(abs). abs comes
// straight out of a file dialog — a bundle folder or an export destination the
// operator picked — so it is whatever they named it. Nothing escaped it, and
// what reached the OS URL handler was measured to be:
//
//	"…/release #2"   -> path "…/release ", fragment "2"
//	"…/what?now"     -> path "…/what",     query "now"
//	"…/100%25 done"  -> path "…/100% done" — a different directory entirely
//	"…/a\tb"         -> does not parse as a URL at all
//
// The first arm of this test is that concatenation, kept verbatim, so the
// table proves the old form was wrong rather than merely restating what the
// new one does. A test written only against the escaper would pass against the
// bug.
//
// It drives apt.FileURI, which is the implementation now: this package carried
// a verbatim copy for one wave because the function was unexported in the core
// (core 61b3e7b exported it so the copy could go). The coverage stays here
// anyway, and that is not duplication for its own sake — core's own
// fileuri_export_test.go asserts that FileURI is REACHABLE and correct, while
// this asserts the two properties RevealPath depends on, over the paths a file
// DIALOG produces. If a core change ever weakened the escaping, the GUI's CI
// should go red rather than the GUI silently opening a different folder. What
// was removed is a second implementation; a second requirement still exists.
func TestFileURIEscapesAnOperatorChosenPath(t *testing.T) {
	// Absolute POSIX-shaped paths, so this is one behaviour on both platforms.
	// The Windows drive-letter case is separate, below.
	paths := []string{
		"/home/op/bundles/release #2",
		"/home/op/bundles/what?now",
		"/home/op/bundles/100%25 done",
		"/home/op/bundles/ext dir",
		"/home/op/bundles/a\tb",
		"/home/op/bundles/plain",
		"/home/op/bundles/каталог",
		"/home/op/bundles/semi;colon&amp",
	}

	naive := func(abs string) string { return "file://" + filepath.ToSlash(abs) }

	t.Run("the concatenation this replaced was wrong", func(t *testing.T) {
		// Not every path breaks it — "plain" and "ext dir" survive — so the
		// assertion is that at least the four named ones do, and that the
		// naive and escaped forms genuinely differ where they should.
		broken := 0
		for _, p := range paths {
			u, err := url.Parse(naive(p))
			if err != nil || u.Path != p {
				broken++
			}
		}
		if broken < 4 {
			t.Fatalf("only %d of %d paths broke the naive concatenation; this test is no longer describing the defect it was written for", broken, len(paths))
		}
	})

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			got := apt.FileURI(p)

			// Property 1: one field, no whitespace. A URL handler is handed
			// this as a single argument and a shell or a .desktop Exec line
			// splits on spaces.
			if len(strings.Fields(got)) != 1 {
				t.Errorf("apt.FileURI(%q) = %q, which is %d whitespace-separated fields", p, got, len(strings.Fields(got)))
			}

			// Property 2: it parses, as file:, with an empty authority. A
			// non-empty authority is the drive-letter defect — "file://C:/x"
			// puts "C:" in the host.
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("apt.FileURI(%q) = %q, which does not parse: %v", p, got, err)
			}
			if u.Scheme != "file" {
				t.Errorf("scheme = %q, want file", u.Scheme)
			}
			if u.Host != "" {
				t.Errorf("apt.FileURI(%q) = %q, whose authority is %q — the path has leaked into the host", p, got, u.Host)
			}
			if u.Fragment != "" || u.RawQuery != "" {
				t.Errorf("apt.FileURI(%q) = %q, which the parser split into fragment %q and query %q", p, got, u.Fragment, u.RawQuery)
			}

			// Property 3: it decodes back to exactly what went in. This is the
			// one that matters — the handler must open the folder the operator
			// chose and not a different one.
			if want := filepath.ToSlash(p); u.Path != want {
				t.Errorf("apt.FileURI(%q) decodes to %q, want %q", p, u.Path, want)
			}
		})
	}
}

// TestFileURIKeepsADriveLetterOutOfTheAuthority. "file://C:/x" has only two
// slashes, so "C:" lands in the URI's authority rather than its path. The
// third slash is what keeps the drive letter part of the path.
//
// The expected string is genuinely OS-dependent and both answers are correct,
// which is why this asserts the properties rather than a literal: FileURI
// normalises with filepath.ToSlash, a no-op wherever the separator is already
// "/", so on Linux the backslashes are ordinary filename characters and come
// back percent-encoded, while on Windows they become separators. Hard-coding
// either one is how core/apt's TestFileURI came to fail on Linux while passing
// here.
func TestFileURIKeepsADriveLetterOutOfTheAuthority(t *testing.T) {
	const in = `D:\projects\ext dir`
	got := apt.FileURI(in)

	if !strings.HasPrefix(got, "file:///") {
		t.Fatalf("apt.FileURI(%q) = %q, want three slashes so the drive letter stays in the path", in, got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("apt.FileURI(%q) = %q, which does not parse: %v", in, got, err)
	}
	if u.Host != "" {
		t.Errorf("authority = %q; the drive letter has become the host", u.Host)
	}
	// The leading slash is part of the answer, not an artefact: it is what
	// keeps "D:" in the path instead of the authority. core/apt's own test
	// spells the expectation the same way.
	if want := "/" + strings.TrimPrefix(filepath.ToSlash(in), "/"); u.Path != want {
		t.Errorf("decodes to %q, want %q (GOOS=%s)", u.Path, want, runtime.GOOS)
	}
	if len(strings.Fields(got)) != 1 {
		t.Errorf("apt.FileURI(%q) = %q, which is not a single field", in, got)
	}
}

// TestBusyCacheIsNotOfferedAsARebuild. ErrCacheBusy and ErrCacheCorrupt are
// the same disagreement seen under different circumstances, and the operator
// must not be told the same thing about them: one clears itself, the other
// needs a rebuild. Offering a forty-second download to fix a file that is
// about to be fine is the failure this distinction exists to prevent.
func TestBusyCacheIsNotOfferedAsARebuild(t *testing.T) {
	busy := uiErrorFromCatalog(fmt.Errorf("wrapped: %w", catalog.ErrCacheBusy))
	if busy == nil {
		t.Fatal("no UIError for a busy cache")
	}
	if !busy.Retryable {
		t.Error("a busy cache is the one condition that clears itself; it must be retryable")
	}
	// Asserted as the code and the instruction rather than by sniffing for
	// the word "rebuild": the busy hint names a rebuild as the thing ANOTHER
	// process is doing, which is exactly the useful part of it. What must not
	// happen is this operator being told to start one.
	if busy.Code != ErrCodeCatalogNotReady {
		t.Errorf("busy code = %q, want %q — a cache mid-write is not a failed catalogue", busy.Code, ErrCodeCatalogNotReady)
	}
	if !strings.Contains(strings.ToLower(busy.Hint), "try again") {
		t.Errorf("busy hint does not tell the operator the one thing that works: %q", busy.Hint)
	}

	corrupt := uiErrorFromCatalog(fmt.Errorf("wrapped: %w", catalog.ErrCacheCorrupt))
	if corrupt == nil {
		t.Fatal("no UIError for a corrupt cache")
	}
	if corrupt.Code != ErrCodeCatalogFailed {
		t.Errorf("corrupt code = %q, want %q", corrupt.Code, ErrCodeCatalogFailed)
	}
	if !strings.Contains(strings.ToLower(corrupt.Hint), "rebuild it") {
		t.Errorf("a genuinely damaged cache must still be offered a rebuild: %q", corrupt.Hint)
	}
	if busy.Message == corrupt.Message {
		t.Error("busy and corrupt render the same sentence, so the operator cannot tell which they have")
	}
}

// ---------------------------------------------------------------------------
// The export plan's free-space number, and where it comes from.
// ---------------------------------------------------------------------------

// TestPlanExportMeasuresTheDestinationRatherThanLookingItUp is the end of the
// wiring the export work found broken.
//
// exportRequest used to derive DestFreeBytes by matching the destination
// against ListVolumes() by longest mount-point prefix. ListVolumes is a drive
// CHOOSER — it deliberately omits filesystems that are not destinations — so
// a path on one of them matched nothing, DestFreeBytes came back
// FreeSpaceUnknown, and export.Plan downgraded its hard "this will not fit"
// refusal to a soft "could not be measured" warning. The operator then starts
// a copy that cannot finish and discovers it partway through, on the drive
// they were about to carry to a machine with no network.
//
// t.TempDir() is the fixture precisely because it is where that failed. In a
// container ListVolumes returns only the bind mounts, so nothing covers /tmp;
// on a live-USB session or an overlayroot install "/" is rejected as a
// pseudo-filesystem and nothing covers it either. This test therefore fails
// on the old code in a container and passes on the new one anywhere the
// platform can measure at all.
func TestPlanExportMeasuresTheDestinationRatherThanLookingItUp(t *testing.T) {
	a, _ := newApp(t)
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The destination itself has to exist — checkDestination requires either
	// it or its parent — but the bundle subdirectory below it does not, which
	// is the normal shape and the one FreeSpaceAt's ancestor walk is for.
	drive := filepath.Join(root, "usb")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}

	if export.FreeSpaceAt(drive) == export.FreeSpaceUnknown {
		t.Skipf("this platform cannot measure free space (GOOS=%s); there is nothing to assert",
			runtime.GOOS)
	}

	plan := a.PlanExport(ExportOptions{BundlePath: bundle, Destination: drive})
	if plan.Error != nil {
		t.Fatalf("PlanExport: %v", plan.Error)
	}
	if plan.Plan.FreeBytes < 0 {
		t.Fatalf("the plan reports free space %d (unknown) for a destination this machine CAN "+
			"measure. export.Plan will now downgrade its refusal to a warning, and an export "+
			"that cannot fit will be started rather than refused.", plan.Plan.FreeBytes)
	}
	if plan.Plan.RequiredBytes <= 0 {
		t.Fatalf("required bytes = %d; the comparison has nothing to compare", plan.Plan.RequiredBytes)
	}
	// And the number must be the destination's own, not some other
	// filesystem's: the same figure FreeSpaceAt gives for the path the plan
	// says it will write into.
	direct := export.FreeSpaceAt(plan.Plan.DestDir)
	if direct == export.FreeSpaceUnknown {
		t.Fatalf("FreeSpaceAt(%q) is unknown while the plan measured %d", plan.Plan.DestDir, plan.Plan.FreeBytes)
	}
	// Compared as an order of magnitude, not for equality: this machine is
	// doing other things and free space moves between the two calls.
	if ratio := float64(plan.Plan.FreeBytes) / float64(direct); ratio < 0.5 || ratio > 2 {
		t.Errorf("the plan measured %d free but the destination has %d; that is not the same filesystem",
			plan.Plan.FreeBytes, direct)
	}

	// The warning that stands in for a measurement must not be there.
	for _, w := range plan.Plan.Warnings {
		if strings.Contains(strings.ToLower(w), "could not be measured") {
			t.Errorf("the plan still carries the unknown-free-space warning: %q", w)
		}
	}
}
