package app

// The second run, at the level the operator actually experiences it.
//
// Every other test in this package hands App the catalogue fake, whose Ready
// and Search are two views of one in-memory slice and therefore agree by
// construction. That is precisely the property a real warm start does not
// have: Ready answers from a file on disk, Search answers from memory, and on
// every run after the first the file was written by a process that is gone.
// So these tests wire the real internal/catalog against a cache written
// before the App exists, and drive the bound methods in the order the screens
// call them — SelectTarget, then CatalogStatus, then SearchPackages.

import (
	"context"
	"testing"
	"time"

	"github.com/inferops/debark/gui/internal/catalog"
	clifake "github.com/inferops/debark/gui/internal/cliadapter/fake"
)

// warmResolver stands in for the deb822 join TargetResolver documents as
// missing. It hands back one fixed target, which is all these tests need and
// is what lets them use the real catalogue instead of the fake.
type warmResolver struct{ target catalog.Target }

func (r warmResolver) Resolve(context.Context, TargetSelection) (catalog.Target, []catalog.SourceProblem, error) {
	return r.target, nil, nil
}

func warmTarget() catalog.Target {
	return catalog.Target{
		Kind:       catalog.TargetBase,
		BaseID:     "ubuntu:24.04/desktop",
		DistroID:   "ubuntu",
		VersionID:  "24.04",
		Codename:   "noble",
		PrettyName: "Ubuntu 24.04 LTS",
		Arch:       "amd64",
		Sources: []catalog.Source{{
			Types:      []string{"deb"},
			URIs:       []string{"http://archive.ubuntu.com/ubuntu"},
			Suites:     []string{"noble"},
			Components: []string{"main", "universe"},
		}},
	}
}

func warmEntries() []catalog.Entry {
	return []catalog.Entry{
		{Name: "git", Version: "1:2.43.0-1", Suite: "noble", Component: "main", Arch: "amd64",
			Section: "vcs", Summary: "fast, scalable, distributed revision control system",
			InstalledSizeKiB: 40000, DownloadSizeBytes: 3700000},
		{Name: "gimp", Version: "2.10.36-3", Suite: "noble", Component: "main", Arch: "amd64",
			Section: "graphics", AppName: "GNU Image Manipulation Program", IsApp: true,
			Categories: []string{"Graphics"}, Summary: "GNU Image Manipulation Program",
			InstalledSizeKiB: 100000, DownloadSizeBytes: 9000000},
		{Name: "libc6", Version: "2.39-0ubuntu8", Suite: "noble", Component: "main", Arch: "amd64",
			Section: "libs", Summary: "GNU C Library: Shared libraries"},
	}
}

// newWarmApp writes a cache the way a first run leaves one behind, then builds
// an App over a Catalog that has never built anything — which is every run
// after the first.
func newWarmApp(t *testing.T) (*App, catalog.Target) {
	t.Helper()
	root := t.TempDir()
	// os.UserCacheDir reads XDG_CACHE_HOME on Linux and LocalAppData on
	// Windows; setting both keeps this hermetic on either.
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	target := warmTarget()
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      target,
		Entries:     warmEntries(),
		ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	a := New(Deps{
		CLI:      clifake.New(),
		Catalog:  catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"}),
		Resolver: warmResolver{target: target},
		Version:  "test",
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })
	return a, target
}

// TestWarmStartPickerFirstPage is the whole defect in one function. The picker
// opens on CatalogStatus.State — "ready" sends it to the list, "not-built" to
// the build button — and then asks SearchPackages for its first page. A status
// of ready over a search that cannot be answered is not an error the screen
// can show: it is placeholder rows that never resolve, with nothing in the log.
func TestWarmStartPickerFirstPage(t *testing.T) {
	a, _ := newWarmApp(t)

	sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"})
	if sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}
	if !sel.Target.CatalogReady {
		t.Fatal("CatalogReady = false over a cache that is on disk: the picker would offer to build one that exists")
	}

	st := a.CatalogStatus()
	if st.State != CatalogStateReady || !st.Ready {
		t.Fatalf("CatalogStatus = %q/%v, want ready/true", st.State, st.Ready)
	}

	// Exactly what the picker does next.
	res := a.SearchPackages(SearchQuery{Limit: 50})
	if res.Error != nil {
		t.Fatalf("SearchPackages on a ready catalogue = %v (%s): the screen was told a catalogue exists",
			res.Error.Message, res.Error.Code)
	}
	if res.Total != len(warmEntries()) {
		t.Fatalf("SearchPackages Total = %d, want %d", res.Total, len(warmEntries()))
	}
	if len(res.Rows) != len(warmEntries()) {
		t.Fatalf("SearchPackages returned %d rows, want %d", len(res.Rows), len(warmEntries()))
	}

	// The sidebar and the detail panel go out in the same frame.
	cats := a.Categories()
	if cats.Error != nil {
		t.Fatalf("Categories on a ready catalogue = %v (%s)", cats.Error.Message, cats.Error.Code)
	}
	if len(cats.Categories) == 0 {
		t.Fatal("Categories returned nothing over a catalogue of three packages")
	}
	pkg := a.GetPackage("gimp")
	if pkg.Error != nil {
		t.Fatalf("GetPackage on a ready catalogue = %v (%s)", pkg.Error.Message, pkg.Error.Code)
	}
	if !pkg.Found || pkg.Package.Name != "gimp" {
		t.Fatalf("GetPackage(gimp) = (%v, %q), want the cached row", pkg.Found, pkg.Package.Name)
	}
}

// TestWarmStartBuildShortCircuitStaysUsable covers the other door into the
// picker. A screen that calls StartCatalogBuild without force over a ready
// catalogue gets an immediate catalog:finished and no build at all, on the
// grounds that the state machine ends up the same either way. It only ends up
// the same if the catalogue is genuinely searchable afterwards.
func TestWarmStartBuildShortCircuitStaysUsable(t *testing.T) {
	a, _ := newWarmApp(t)
	if sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}
	if r := a.StartCatalogBuild(false); !r.OK {
		t.Fatalf("StartCatalogBuild(false) over a ready catalogue: %v", r.Error)
	}
	res := a.SearchPackages(SearchQuery{Limit: 50})
	if res.Error != nil {
		t.Fatalf("SearchPackages after the ready short circuit = %v (%s)", res.Error.Message, res.Error.Code)
	}
	if res.Total != len(warmEntries()) {
		t.Fatalf("SearchPackages Total = %d, want %d", res.Total, len(warmEntries()))
	}
}

// TestWarmStartWithNoCacheStillOffersToBuild is the first run, kept honest by
// the same harness. "No catalogue for this target yet" must remain reachable:
// a fix that made every target look ready would trade a hang for a wrong
// screen.
func TestWarmStartWithNoCacheStillOffersToBuild(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	a := New(Deps{
		CLI:      clifake.New(),
		Catalog:  catalog.New(catalog.Options{}),
		Resolver: warmResolver{target: warmTarget()},
		Version:  "test",
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"})
	if sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}
	if sel.Target.CatalogReady {
		t.Fatal("CatalogReady = true with nothing on disk")
	}
	if st := a.CatalogStatus(); st.State != CatalogStateNotBuilt {
		t.Fatalf("CatalogStatus = %q, want not-built", st.State)
	}
	res := a.SearchPackages(SearchQuery{Limit: 50})
	if res.Error == nil || res.Error.Code != ErrCodeCatalogNotReady {
		t.Fatalf("SearchPackages with no catalogue = %v, want catalog.not_ready", res.Error)
	}
}

// TestWarmStartCatalogStatusCounts is the picker's header on the second run.
//
// CatalogStatus is what the screen mounts on, Categories().Total is read
// straight off it, and on the warm path nothing had ever assigned either. The
// symptom was not an error: it was a sidebar reporting 0 packages beside a
// list of rows, on every run after the first, forever.
func TestWarmStartCatalogStatusCounts(t *testing.T) {
	a, _ := newWarmApp(t)

	sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"})
	if sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}

	st := a.CatalogStatus()
	want := len(warmEntries())
	if st.PackageCount != want {
		t.Fatalf("CatalogStatus.PackageCount on the cache path = %d, want %d", st.PackageCount, want)
	}
	if st.BuiltAt == "" {
		t.Fatal("CatalogStatus.BuiltAt on the cache path is empty: the marker carries a built_at")
	}
	if _, err := time.Parse(time.RFC3339, st.BuiltAt); err != nil {
		t.Fatalf("CatalogStatus.BuiltAt = %q, which is not RFC 3339: %v", st.BuiltAt, err)
	}
	if !st.FromCache {
		t.Fatal("CatalogStatus.FromCache false on the cache path")
	}

	// The number the sidebar actually renders, and the rows behind it.
	cats := a.Categories()
	if cats.Error != nil {
		t.Fatalf("Categories: %v", cats.Error)
	}
	if cats.Total != want {
		t.Fatalf("Categories().Total = %d, want %d", cats.Total, want)
	}
	res := a.SearchPackages(SearchQuery{Limit: 50})
	if res.Error != nil {
		t.Fatalf("SearchPackages: %v", res.Error)
	}
	if res.Total != cats.Total {
		t.Fatalf("the header says %d packages and the list says %d", cats.Total, res.Total)
	}
}

// TestWarmStartCountsClearedOnTargetSwitch. The count is a property of the
// selected target's catalogue, and a target with no catalogue must not inherit
// the previous one's number — that would be a worse defect than 0, because it
// is plausible.
func TestWarmStartCountsClearedOnTargetSwitch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	built := warmTarget()
	dir, err := catalog.CacheDir(built)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target: built, Entries: warmEntries(), ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	unbuilt := built
	unbuilt.Codename = "jammy"
	unbuilt.VersionID = "22.04"
	unbuilt.BaseID = "ubuntu:22.04/desktop"

	res := &switchingResolver{targets: []catalog.Target{built, unbuilt}}
	a := New(Deps{
		CLI:      clifake.New(),
		Catalog:  catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"}),
		Resolver: res,
		Version:  "test",
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); r.Error != nil {
		t.Fatalf("SelectTarget(built): %v", r.Error)
	}
	if got := a.CatalogStatus().PackageCount; got != len(warmEntries()) {
		t.Fatalf("PackageCount on the built target = %d, want %d", got, len(warmEntries()))
	}

	if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:22.04/desktop", Arch: "amd64"}); r.Error != nil {
		t.Fatalf("SelectTarget(unbuilt): %v", r.Error)
	}
	st := a.CatalogStatus()
	if st.Ready {
		t.Fatal("Ready true for a target with no cache")
	}
	if st.PackageCount != 0 {
		t.Fatalf("PackageCount after switching to a target with no catalogue = %d, want 0", st.PackageCount)
	}
	if st.BuiltAt != "" {
		t.Fatalf("BuiltAt after switching to a target with no catalogue = %q, want empty", st.BuiltAt)
	}
}

// switchingResolver hands back a different target on each call, so one App can
// be walked from a target that has a cache to one that does not.
type switchingResolver struct {
	targets []catalog.Target
	n       int
}

func (r *switchingResolver) Resolve(context.Context, TargetSelection) (catalog.Target, []catalog.SourceProblem, error) {
	t := r.targets[min(r.n, len(r.targets)-1)]
	r.n++
	return t, nil, nil
}

// TestWarmStartStaleReachesCatalogStatus is the whole of the second defect.
//
// The picker's "This catalogue is out of date / Refresh" banner renders off
// CatalogStatus.stale. Nothing assigned it on the cache path, and the cache
// path is the only one where it can be true — a catalogue this session built
// is fresh by construction. So the banner could not appear for the only kind
// of catalogue that can be out of date, and no test inside internal/app could
// see that, because the fake it drives holds no cache at all.
func TestWarmStartStaleReachesCatalogStatus(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	built := warmTarget()
	dir, err := catalog.CacheDir(built)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:            built,
		Entries:           warmEntries(),
		IndexDigest:       "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		IndexDigestSource: catalog.CacheDigestRelease,
		ToolVersion:       "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	// The resolver is where a live index digest would arrive from, which is
	// the point stated plainly: this is the one seam that has to carry it.
	moved := built
	moved.IndexDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	a := New(Deps{
		CLI:      clifake.New(),
		Catalog:  catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"}),
		Resolver: warmResolver{target: moved},
		Version:  "test",
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	if sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}

	st := a.CatalogStatus()
	if !st.Ready {
		t.Fatal("a stale catalogue must still be ready: stale is browsable, and refusing to load one would break working offline")
	}
	if !st.Stale {
		t.Fatal("CatalogStatus.Stale false over a cache the archive has moved on from: the Refresh banner cannot appear")
	}
	if st.Error != nil {
		t.Fatalf("stale reported as an error: %v — it is not one", st.Error)
	}
	if st.PackageCount != len(warmEntries()) {
		t.Fatalf("PackageCount over a stale catalogue = %d, want %d", st.PackageCount, len(warmEntries()))
	}
	// The rows are there to browse while the banner is up.
	if res := a.SearchPackages(SearchQuery{Limit: 50}); res.Error != nil || res.Total != len(warmEntries()) {
		t.Fatalf("SearchPackages over a stale catalogue = (%d rows, %v), want %d rows and no error",
			res.Total, res.Error, len(warmEntries()))
	}
}

// TestWarmStartFreshCatalogueIsNotStale is the other half, and matters more
// than it looks: a banner that appears when it should not sends the operator
// into a rebuild they did not need, every time they open the picker.
func TestWarmStartFreshCatalogueIsNotStale(t *testing.T) {
	a, _ := newWarmApp(t)
	if sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}
	if st := a.CatalogStatus(); st.Stale {
		t.Fatal("CatalogStatus.Stale true over a cache with no digest on either side: unknown is not stale")
	}
}
