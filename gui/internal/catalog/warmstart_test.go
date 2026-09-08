package catalog_test

// The warm start: a cache written by an earlier run, opened by a later one.
//
// Every other test in this package builds and searches inside one process, so
// the in-memory index is always a side effect of the same call that wrote the
// file. A real operator never does that. Their second run opens a Catalog that
// has never built anything, asks Ready — which answers from disk — and then
// searches. That sequence is the one below and it is the only one that can
// tell a catalogue that loaded from the cache apart from one that merely knows
// a cache exists.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/catalog/fake"
)

// warmStartCache writes a cache for cacheTestTarget into a temporary cache
// root and returns the target, exactly as a first run would leave it behind.
func warmStartCache(t *testing.T) catalog.Target {
	t.Helper()
	root := t.TempDir()
	// os.UserCacheDir reads XDG_CACHE_HOME on Linux and LocalAppData on
	// Windows; setting both keeps this hermetic on either.
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	target := cacheTestTarget()
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      target,
		Entries:     cacheTestEntries(),
		ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	return target
}

// TestWarmStartReadyThenSearch is the second run. Ready true is the whole
// promise the picker opens on: it means "there is a catalogue for this target,
// go and browse it". A Catalog that answers true and then cannot answer a
// query has told the screen a catalogue exists and left it waiting for rows
// that are never coming.
func TestWarmStartReadyThenSearch(t *testing.T) {
	target := warmStartCache(t)

	// A brand new Catalog, as a second run has: nothing built, nothing loaded.
	c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
	t.Cleanup(func() { _ = c.Close() })

	ready, err := c.Ready(target)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Fatalf("Ready = false, want true: the cache this test just wrote is the one the picker opens on")
	}

	ctx := context.Background()

	page, err := c.Search(ctx, catalog.Query{})
	if err != nil {
		t.Fatalf("Search after Ready true = %v, want a page: Ready promised the picker a catalogue", err)
	}
	if page.Total != len(cacheTestEntries()) {
		t.Errorf("Search Total = %d, want %d", page.Total, len(cacheTestEntries()))
	}
	if len(page.Entries) == 0 {
		t.Errorf("Search returned no rows over a catalogue of %d", page.Total)
	}

	cats, err := c.Categories(ctx)
	if err != nil {
		t.Fatalf("Categories after Ready true = %v, want the sidebar's groups", err)
	}
	if len(cats) == 0 {
		t.Errorf("Categories returned nothing over a catalogue of %d", page.Total)
	}

	e, found, err := c.Get(ctx, "gimp")
	if err != nil {
		t.Fatalf("Get after Ready true = %v", err)
	}
	if !found || e.Name != "gimp" {
		t.Errorf("Get(gimp) = (%q, %v), want the cached row", e.Name, found)
	}
}

// TestWarmStartSearchWithoutReady keeps the other half of the contract honest.
// Ready is what makes a catalogue browsable; a Catalog that was never told
// which target it serves still owes the caller ErrNotBuilt and not an empty
// page, because "no catalogue" and "no matches" are different screens.
func TestWarmStartSearchWithoutReady(t *testing.T) {
	warmStartCache(t)

	c := catalog.New(catalog.Options{})
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Search(context.Background(), catalog.Query{}); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Search on a Catalog bound to no target = %v, want ErrNotBuilt", err)
	}
}

// warmStartOtherTarget is a second base, different enough that its cache lands
// in its own directory: switching between two targets is the other way a
// Catalog can be asked for rows it does not have.
func warmStartOtherTarget() catalog.Target {
	t := cacheTestTarget()
	t.BaseID = "debian:12/standard"
	t.DistroID = "debian"
	t.VersionID = "12"
	t.Codename = "bookworm"
	t.PrettyName = "Debian 12"
	t.Sources = []catalog.Source{{
		Types:      []string{"deb"},
		URIs:       []string{"http://deb.debian.org/debian"},
		Suites:     []string{"bookworm"},
		Components: []string{"main"},
	}}
	return t
}

func warmStartOtherEntries() []catalog.Entry {
	return []catalog.Entry{
		{Name: "bookworm-only-one", Arch: "amd64", Section: "misc"},
		{Name: "bookworm-only-two", Arch: "amd64", Section: "misc"},
	}
}

// saveWarmCache writes a cache for t into whatever cache root is in force.
func saveWarmCache(t *testing.T, target catalog.Target, entries []catalog.Entry) {
	t.Helper()
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      target,
		Entries:     entries,
		ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache for %s: %v", target.CacheKey(), err)
	}
}

// TestWarmStartTargetSwitch is the same defect wearing its other face. A
// Catalog that keeps serving whatever it happens to have loaded will answer
// every query about the second target with the first target's packages —
// plausible rows for the wrong base, with nothing on screen to say so.
func TestWarmStartTargetSwitch(t *testing.T) {
	first := warmStartCache(t) // sets the cache root and writes first's cache
	second := warmStartOtherTarget()
	saveWarmCache(t, second, warmStartOtherEntries())

	c := catalog.New(catalog.Options{})
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	if ready, err := c.Ready(first); err != nil || !ready {
		t.Fatalf("Ready(first) = (%v, %v), want (true, nil)", ready, err)
	}
	page, err := c.Search(ctx, catalog.Query{})
	if err != nil {
		t.Fatalf("Search(first): %v", err)
	}
	if page.Total != len(cacheTestEntries()) {
		t.Fatalf("Search(first) Total = %d, want %d", page.Total, len(cacheTestEntries()))
	}

	// The operator picks a different base. Same Catalog, as the application
	// keeps exactly one.
	if ready, err := c.Ready(second); err != nil || !ready {
		t.Fatalf("Ready(second) = (%v, %v), want (true, nil)", ready, err)
	}
	page, err = c.Search(ctx, catalog.Query{})
	if err != nil {
		t.Fatalf("Search(second): %v", err)
	}
	if page.Total != len(warmStartOtherEntries()) {
		t.Fatalf("Search(second) Total = %d, want %d: the first target's rows are still being served",
			page.Total, len(warmStartOtherEntries()))
	}
	if _, found, err := c.Get(ctx, "gimp"); err != nil || found {
		t.Errorf("Get(gimp) on the second target = (%v, %v), want not found: that package is the first target's",
			found, err)
	}

	// And back again, which must not have cost the first target anything.
	if ready, err := c.Ready(first); err != nil || !ready {
		t.Fatalf("Ready(first) again = (%v, %v), want (true, nil)", ready, err)
	}
	if _, found, err := c.Get(ctx, "gimp"); err != nil || !found {
		t.Errorf("Get(gimp) back on the first target = (%v, %v), want found", found, err)
	}
}

// TestWarmStartSwitchToUnbuiltTarget is the same rule at its sharper edge: the
// second target has no cache at all. "No catalogue for this target" is a
// screen with a build button on it; the first target's package list is not.
func TestWarmStartSwitchToUnbuiltTarget(t *testing.T) {
	first := warmStartCache(t)
	second := warmStartOtherTarget() // deliberately never saved

	c := catalog.New(catalog.Options{})
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	if ready, err := c.Ready(first); err != nil || !ready {
		t.Fatalf("Ready(first) = (%v, %v), want (true, nil)", ready, err)
	}
	if _, err := c.Search(ctx, catalog.Query{}); err != nil {
		t.Fatalf("Search(first): %v", err)
	}

	ready, err := c.Ready(second)
	if err != nil {
		t.Fatalf("Ready(second): %v", err)
	}
	if ready {
		t.Fatalf("Ready(second) = true, want false: nothing was ever built for it")
	}
	if _, err := c.Search(ctx, catalog.Query{}); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Search after switching to an unbuilt target = %v, want ErrNotBuilt", err)
	}
	if _, err := c.Categories(ctx); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Categories after switching to an unbuilt target = %v, want ErrNotBuilt", err)
	}
	if _, found, err := c.Get(ctx, "gimp"); found || !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Get after switching to an unbuilt target = (%v, %v), want (false, ErrNotBuilt)", found, err)
	}
}

// TestWarmStartConcurrentFirstQueries is the picker's first frame: a Search
// and a Categories go out together, and a Search follows every keystroke after
// that. All of them can arrive before the cache has finished loading.
func TestWarmStartConcurrentFirstQueries(t *testing.T) {
	target := warmStartCache(t)

	c := catalog.New(catalog.Options{})
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	if ready, err := c.Ready(target); err != nil || !ready {
		t.Fatalf("Ready = (%v, %v), want (true, nil)", ready, err)
	}

	const n = 16
	totals := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 3 {
			case 0:
				cats, err := c.Categories(ctx)
				errs[i] = err
				totals[i] = -len(cats) // only checked for an error
			case 1:
				_, found, err := c.Get(ctx, "gimp")
				errs[i] = err
				if err == nil && !found {
					errs[i] = errors.New("Get(gimp) not found")
				}
				totals[i] = -1
			default:
				page, err := c.Search(ctx, catalog.Query{})
				errs[i] = err
				totals[i] = page.Total
			}
		}(i)
	}
	close(start)
	wg.Wait()

	want := len(cacheTestEntries())
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("concurrent query %d: %v", i, errs[i])
		}
		if totals[i] >= 0 && totals[i] != want {
			t.Errorf("concurrent query %d Total = %d, want %d", i, totals[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// The optional half of the contract, answered on the warm path.
// ---------------------------------------------------------------------------

// catalogFacts is the optional interface internal/app probes for. Declared
// here rather than imported so that this test fails if a method is renamed or
// its signature changes, which is the only thing keeping the two declarations
// in step.
type catalogFacts interface {
	PackageCount() int
	Stale() bool
	BuiltAt() time.Time
}

// TestWarmStartFactsBeforeAnyQuery is the second run's first frame.
//
// The picker asks CatalogStatus before it asks for a row, and on a warm start
// Ready is the only method that has been called by then — the cache is
// materialised later, by whichever query comes first. So a package count that
// only a load can supply is a package count the first frame does not have,
// which is exactly how Categories().Total came to be 0 on every run after the
// first.
//
// Asserting before the first Search is therefore the whole point of this test:
// a version that searched first would pass over the defect.
func TestWarmStartFactsBeforeAnyQuery(t *testing.T) {
	target := warmStartCache(t)

	c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
	t.Cleanup(func() { _ = c.Close() })

	facts, ok := c.(catalogFacts)
	if !ok {
		t.Fatal("the Catalog does not implement the optional facts interface internal/app probes for")
	}

	if got := facts.PackageCount(); got != 0 {
		t.Fatalf("PackageCount before Ready = %d, want 0: nothing has named a target yet", got)
	}

	ready, err := c.Ready(target)
	if err != nil || !ready {
		t.Fatalf("Ready = (%v, %v), want (true, nil)", ready, err)
	}

	want := len(cacheTestEntries())
	if got := facts.PackageCount(); got != want {
		t.Fatalf("PackageCount after Ready, before any query = %d, want %d", got, want)
	}
	if facts.BuiltAt().IsZero() {
		t.Fatal("BuiltAt after Ready is the zero time: the marker carries a built_at and it was read to answer Ready at all")
	}
	if facts.Stale() {
		t.Fatal("Stale over a cache written moments ago with no index digest on either side: unknown is not stale")
	}

	// And the load path agrees with the marker path. The two are separate
	// sources for the same number — meta.json's entry_count and the rows that
	// materialise out of catalog.bin — and a disagreement between them is the
	// shape of defect a single-source test cannot see.
	if _, err := c.Search(context.Background(), catalog.Query{Limit: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := facts.PackageCount(); got != want {
		t.Fatalf("PackageCount after the cache materialised = %d, want %d: the marker and the file disagree", got, want)
	}
	if facts.BuiltAt().IsZero() {
		t.Fatal("BuiltAt after the load is the zero time")
	}
}

// TestWarmStartFactsForUnbuiltTargetAreZero keeps the other half honest. The
// documented contract is "0 when not ready", and a Catalog that kept the
// previous target's count would put the wrong number on the screen of a target
// that has no catalogue at all.
func TestWarmStartFactsForUnbuiltTargetAreZero(t *testing.T) {
	target := warmStartCache(t)

	c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
	t.Cleanup(func() { _ = c.Close() })
	facts := c.(catalogFacts)

	if _, err := c.Ready(target); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if facts.PackageCount() == 0 {
		t.Fatal("PackageCount over the built target is 0, so this test proves nothing")
	}

	other := target
	other.Codename = "jammy"
	other.VersionID = "22.04"
	ready, err := c.Ready(other)
	if err != nil {
		t.Fatalf("Ready(other): %v", err)
	}
	if ready {
		t.Fatal("Ready true for a target with no cache")
	}
	if got := facts.PackageCount(); got != 0 {
		t.Fatalf("PackageCount after switching to an unbuilt target = %d, want 0", got)
	}
	if !facts.BuiltAt().IsZero() {
		t.Fatalf("BuiltAt after switching to an unbuilt target = %v, want the zero time", facts.BuiltAt())
	}
}

// TestWarmStartFactsAcrossProcesses writes the cache in a child process and
// reads it back in this one.
//
// The other warm-start tests here construct a fresh Catalog over a file this
// process wrote, which is close to the real thing but not it: everything the
// writer computed is still in this address space, and a fact that leaked from
// the writing path into the reading one would go unnoticed. A child process
// leaves nothing behind but the bytes on disk, which is what an operator's
// second run actually has.
func TestWarmStartFactsAcrossProcesses(t *testing.T) {
	if os.Getenv("DEBARK_GUI_WARMSTART_CHILD") != "" {
		// Not reachable: the child runs TestWarmStartCacheWriterChild.
		t.Skip("child process")
	}
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	cmd := exec.Command(os.Args[0], "-test.run", "^TestWarmStartCacheWriterChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		"DEBARK_GUI_WARMSTART_CHILD=1",
		"XDG_CACHE_HOME="+root,
		"LocalAppData="+root,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child writer failed: %v\n%s", err, out)
	}

	target := cacheTestTarget()
	c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
	t.Cleanup(func() { _ = c.Close() })

	ready, err := c.Ready(target)
	if err != nil || !ready {
		t.Fatalf("Ready over a cache written by another process = (%v, %v)\nchild said:\n%s", ready, err, out)
	}
	facts := c.(catalogFacts)
	want := len(cacheTestEntries())
	if got := facts.PackageCount(); got != want {
		t.Fatalf("PackageCount over another process's cache = %d, want %d", got, want)
	}
	if facts.BuiltAt().IsZero() {
		t.Fatal("BuiltAt over another process's cache is the zero time")
	}
	page, err := c.Search(context.Background(), catalog.Query{Limit: 50})
	if err != nil {
		t.Fatalf("Search over another process's cache: %v", err)
	}
	if page.Total != want {
		t.Fatalf("Search Total = %d, want %d: the count on screen and the rows behind it disagree", page.Total, want)
	}
}

// TestWarmStartCacheWriterChild is the child half of the test above. It is a
// no-op unless DEBARK_GUI_WARMSTART_CHILD is set, so a normal `go test` run
// neither writes a cache outside a temporary directory nor spends time here.
func TestWarmStartCacheWriterChild(t *testing.T) {
	if os.Getenv("DEBARK_GUI_WARMSTART_CHILD") == "" {
		t.Skip("not the child process")
	}
	target := cacheTestTarget()
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      target,
		Entries:     cacheTestEntries(),
		ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
}

// TestWarmStartStaleIsAnsweredBeforeAnyQuery is the picker's "this catalogue
// is out of date / Refresh" banner.
//
// A cache-loaded catalogue is the only kind that can be stale: one this
// process just built is fresh by construction. So the warm path is the only
// path where the answer is ever interesting, and Ready is the only method
// called on it before the screen renders.
func TestWarmStartStaleIsAnsweredBeforeAnyQuery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	target := cacheTestTarget()
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	// What the archive published when this cache was written.
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:            target,
		Entries:           cacheTestEntries(),
		IndexDigest:       "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		IndexDigestSource: catalog.CacheDigestRelease,
		ToolVersion:       "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	// What it publishes now. The digest is deliberately not part of the cache
	// key (docs/dev/cache-format.md §6.3), so this must find the same
	// directory rather than miss it.
	moved := target
	moved.IndexDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
	t.Cleanup(func() { _ = c.Close() })

	ready, err := c.Ready(moved)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Fatal("Ready false over a stale cache: a stale catalogue still loads and the operator is told, it is not hidden")
	}
	facts := c.(catalogFacts)
	if !facts.Stale() {
		t.Fatal("Stale false after Ready over a cache the archive has moved on from: the refresh banner can never appear")
	}
	if got := facts.PackageCount(); got != len(cacheTestEntries()) {
		t.Fatalf("PackageCount over a stale cache = %d, want %d: stale is browsable, not empty", got, len(cacheTestEntries()))
	}

	// And the load agrees with the marker. LoadCache derives staleness from
	// the file it opens; Ready derives it from meta.json without opening
	// anything. Two derivations of one fact is how a banner comes and goes as
	// the first query lands.
	page, err := c.Search(context.Background(), catalog.Query{Limit: 50})
	if err != nil {
		t.Fatalf("Search over a stale cache: %v", err)
	}
	if page.Total != len(cacheTestEntries()) {
		t.Fatalf("Search Total over a stale cache = %d, want %d", page.Total, len(cacheTestEntries()))
	}
	if !facts.Stale() {
		t.Fatal("Stale became false once the cache materialised: the marker path and the load path disagree")
	}
}

// TestWarmStartFreshAndUnknownAreNotStale keeps the other two rows of §6.4's
// table honest. A banner that appears when it should not is worse than one
// that never appears: it sends an operator into a forty-second rebuild they
// did not need, every time they open the picker.
func TestWarmStartFreshAndUnknownAreNotStale(t *testing.T) {
	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	cases := []struct {
		name          string
		savedDigest   string
		currentDigest string
	}{
		{"digests equal", digest, digest},
		{"target digest unknown", digest, ""},
		{"cache digest unknown", "", digest},
		{"both unknown, which is every catalogue today", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", root)
			t.Setenv("LocalAppData", root)

			target := cacheTestTarget()
			dir, err := catalog.CacheDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
				Target:      target,
				Entries:     cacheTestEntries(),
				IndexDigest: tc.savedDigest,
				ToolVersion: "debark-gui warmstart-test",
			}); err != nil {
				t.Fatalf("SaveCache: %v", err)
			}

			now := target
			now.IndexDigest = tc.currentDigest

			c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
			t.Cleanup(func() { _ = c.Close() })
			ready, err := c.Ready(now)
			if err != nil || !ready {
				t.Fatalf("Ready = (%v, %v), want (true, nil)", ready, err)
			}
			facts := c.(catalogFacts)
			if facts.Stale() {
				t.Fatal("Stale true: unknown is not stale, and equal is not stale")
			}
			if _, err := c.Search(context.Background(), catalog.Query{Limit: 1}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if facts.Stale() {
				t.Fatal("Stale became true once the cache materialised: the marker path and the load path disagree")
			}
		})
	}
}

// TestBuiltCatalogueIsNotStale. A build re-resolves against the live archive,
// so what it writes is fresh whatever was on screen when it started — and a
// refresh banner over a catalogue built seconds ago would send the operator
// straight back round the same forty seconds.
func TestBuiltCatalogueIsNotStale(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	target := cacheTestTarget()
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      target,
		Entries:     cacheTestEntries(),
		IndexDigest: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	stale := target
	stale.IndexDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

	c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Ready(stale); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	facts := c.(catalogFacts)
	if !facts.Stale() {
		t.Fatal("the fixture is not stale, so this test proves nothing")
	}

	// Rewriting the cache with the digest the archive now publishes is what a
	// refresh does, minus the network.
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      stale,
		Entries:     cacheTestEntries(),
		IndexDigest: stale.IndexDigest,
		ToolVersion: "debark-gui warmstart-test",
	}); err != nil {
		t.Fatalf("SaveCache (refresh): %v", err)
	}
	if _, err := c.Ready(stale); err != nil {
		t.Fatalf("Ready after refresh: %v", err)
	}
	if facts.Stale() {
		t.Fatal("still stale after the cache was rewritten against the digest the archive now publishes")
	}
}

// TestWarmStartLoadDoesNotLeaveItsOwnGarbageBehind pins the fix for the
// idle-memory finding.
//
// Loading a catalogue is the largest allocation this application ever makes,
// and almost all of it is immediately garbage: the whole catalog.bin buffer
// plus the search index's scratch arena, against a live catalogue that is
// smaller than either. Measured on the app's own 85,565-entry cache, the load
// used to return with HeapAlloc at 68.4 MB where the catalogue itself is
// 39.8 MB, and the ~28 MB difference then stayed charged to the process — the
// Go scavenger only returns what is above the heap goal, and twice a 39 MB
// live heap is above everything the process holds. It survived ninety seconds
// of idle in the real desktop app, where it read as 29 MB of memory the
// catalogue was wrongly assumed to be retaining.
//
// catalogImpl.load now ends with debug.FreeOSMemory. This test asserts the
// consequence rather than the call: after a load, the uncollected garbage the
// load left behind is a small fraction of the file it just read. Deleting the
// release makes the big case fail by roughly the size of the cache.
//
// Both cases are synthetic, because the property is about the load path and
// not about the archive; the real-corpus figures live in
// docs/performance.md and in TestPerfLoadMemoryProfile.
func TestWarmStartLoadDoesNotLeaveItsOwnGarbageBehind(t *testing.T) {
	cases := []struct {
		name string
		// entries is how many rows the synthetic catalogue holds.
		entries int
		// maxGarbage is the ceiling on what the load may leave uncollected.
		// It is generous: without the release, the small case leaves about
		// its own file size and the large case several times this bound.
		maxGarbage uint64
	}{
		{name: "small catalogue", entries: 2000, maxGarbage: 2 << 20},
		{name: "picker-sized catalogue", entries: 30000, maxGarbage: 4 << 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := warmStartSyntheticEntries(t, tc.entries)
			root := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", root)
			t.Setenv("LocalAppData", root)
			target := cacheTestTarget()
			dir, err := catalog.CacheDir(target)
			if err != nil {
				t.Fatal(err)
			}
			written, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
				Target:      target,
				Entries:     entries,
				ToolVersion: "debark-gui warmstart-test",
			})
			if err != nil {
				t.Fatalf("SaveCache: %v", err)
			}
			// entries is deliberately not referenced again: Go's liveness is
			// precise, so the corpus is collectable from here and the load
			// below is measured against a heap that does not still hold it.

			c := catalog.New(catalog.Options{ToolVersion: "debark-gui warmstart-test"})
			t.Cleanup(func() { _ = c.Close() })
			loader, ok := c.(interface{ LoadTarget(catalog.Target) error })
			if !ok {
				t.Fatal("the catalogue no longer exposes LoadTarget; this test loads through the same call the app does")
			}

			runtime.GC()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			if err := loader.LoadTarget(target); err != nil {
				t.Fatalf("LoadTarget: %v", err)
			}

			// Read at once: this is the state the process is in the instant
			// the load returns, which is the state the operator's machine
			// sits in while the picker is idle.
			var atReturn runtime.MemStats
			runtime.ReadMemStats(&atReturn)

			// Then find out how much of that was live. Whatever the
			// difference is, the load allocated it and no collection has
			// swept it.
			runtime.GC()
			runtime.GC()
			var live runtime.MemStats
			runtime.ReadMemStats(&live)

			// A query, so the catalogue cannot have been elided and so the
			// numbers describe one that works.
			page, err := c.Search(context.Background(), catalog.Query{})
			if err != nil {
				t.Fatalf("Search after LoadTarget: %v", err)
			}
			if page.Total != tc.entries {
				t.Fatalf("Search Total = %d, want %d", page.Total, tc.entries)
			}

			var garbage uint64
			if atReturn.HeapAlloc > live.HeapAlloc {
				garbage = atReturn.HeapAlloc - live.HeapAlloc
			}
			retained := int64(live.HeapAlloc) - int64(before.HeapAlloc)
			t.Logf("%d entries, %d bytes of catalog.bin: %.2f MB retained, %.2f MB left uncollected by the load",
				tc.entries, written.BinSize, float64(retained)/(1<<20), float64(garbage)/(1<<20))

			if garbage > tc.maxGarbage {
				t.Errorf("the load left %.2f MB uncollected, want at most %.2f MB.\n"+
					"catalogImpl.load must end by returning its own transient memory — a %d-byte "+
					"cache buffer and the index's scratch arena — or the process stays charged for "+
					"it for as long as the picker is open. See docs/performance.md, "+
					"\"What the load actually costs the process\".",
					float64(garbage)/(1<<20), float64(tc.maxGarbage)/(1<<20), written.BinSize)
			}
		})
	}
}

// warmStartSyntheticEntries drains n rows out of the fake corpus, which is the
// only generator in this repository that produces package names, application
// names and summaries with realistic length and variety. Row text is most of
// what a load allocates, so a corpus of short uniform strings would not
// exercise the thing under test.
func warmStartSyntheticEntries(t *testing.T, n int) []catalog.Entry {
	t.Helper()
	f := fake.New(n)
	out := make([]catalog.Entry, 0, f.Len())
	for off := 0; off < f.Len(); off += catalog.MaxPageSize {
		p, err := f.Search(context.Background(), catalog.Query{Offset: off, Limit: catalog.MaxPageSize})
		if err != nil {
			t.Fatalf("draining the fake corpus: %v", err)
		}
		out = append(out, p.Entries...)
	}
	if len(out) != n {
		t.Fatalf("fake corpus produced %d entries, wanted %d", len(out), n)
	}
	return out
}
