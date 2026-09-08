package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inferops/debark/gui/internal/cliadapter"

	catfake "github.com/inferops/debark/gui/internal/catalog/fake"
	clifake "github.com/inferops/debark/gui/internal/cliadapter/fake"
	"github.com/inferops/debark/gui/internal/readiness"
)

type recorder struct {
	mu     sync.Mutex
	events map[string]int
}

func newRecorder() *recorder { return &recorder{events: map[string]int{}} }

func (r *recorder) emit(name string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[name]++
}

func (r *recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[name]
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newApp(t *testing.T) (*App, *recorder) {
	t.Helper()
	r := newRecorder()
	cat := catfake.NewDefault()
	cat.SetReady(false)
	a := New(Deps{
		CLI:     clifake.New(),
		Catalog: cat,
		Emit:    r.emit,
		Version: "test",
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })
	return a, r
}

func TestBindReturnsTheApp(t *testing.T) {
	a, _ := newApp(t)
	b := a.Bind()
	if len(b) != 1 {
		t.Fatalf("Bind() = %d entries, want 1", len(b))
	}
	if b[0] != any(a) {
		t.Fatal("Bind() did not return the App")
	}
}

func TestCancelIsSafeWhenIdle(t *testing.T) {
	a, _ := newApp(t)
	cancels := map[string]func() Result{
		"catalog":   a.CancelCatalogBuild,
		"build":     a.CancelBuild,
		"export":    a.CancelExport,
		"verify":    a.CancelVerify,
		"readiness": a.CancelReadinessCheck,
	}
	for name, fn := range cancels {
		if r := fn(); !r.OK {
			t.Fatalf("cancel %s on an idle app: %v", name, r.Error)
		}
	}
}

func TestEveryScreenEndToEnd(t *testing.T) {
	a, rec := newApp(t)

	// Screen 1 - readiness.
	if got := a.Readiness(); got.Checking {
		t.Fatal("readiness should be idle before the first check")
	}
	if r := a.StartReadinessCheck(); !r.OK {
		t.Fatalf("StartReadinessCheck: %v", r.Error)
	}
	waitFor(t, "readiness:finished", func() bool { return rec.count(EventReadinessFinished) == 1 })
	rep := a.Readiness()
	if len(rep.Checks) == 0 {
		t.Fatal("readiness produced no rows")
	}
	for _, c := range rep.Checks {
		if c.Summary == "" {
			t.Fatalf("check %q has no summary", c.ID)
		}
		if c.Status == ReadinessProblem && c.Remedy == "" {
			t.Fatalf("check %q is a problem with no remedy: that is the wall", c.ID)
		}
	}

	// Screen 2 - target selection.
	arches := a.SupportedArchitectures()
	if len(arches.Arches) == 0 {
		t.Fatal("no architectures offered")
	}
	bases := a.ListBases(arches.Default)
	if bases.Error != nil {
		t.Fatalf("ListBases: %v", bases.Error)
	}
	if len(bases.Bases) == 0 {
		t.Fatal("no bases offered")
	}
	if bases.Caveat == "" {
		t.Fatal("the assumption caveat is missing")
	}
	sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: bases.Bases[0].ID, Arch: bases.Arch})
	if sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}
	if !sel.Target.Selected || !sel.Target.Assumed || sel.Target.Caveat == "" {
		t.Fatalf("target view wrong: %+v", sel.Target)
	}
	if rec.count(EventTargetChanged) != 1 {
		t.Fatal("target:changed did not fire")
	}

	// Screen 3 - catalogue.
	if got := a.SearchPackages(SearchQuery{}); got.Error == nil || got.Error.Code != ErrCodeCatalogNotReady {
		t.Fatalf("search before build should say not-ready, got %+v", got.Error)
	}
	if r := a.StartCatalogBuild(false); !r.OK {
		t.Fatalf("StartCatalogBuild: %v", r.Error)
	}
	waitFor(t, "catalog:finished", func() bool { return rec.count(EventCatalogFinished) == 1 })
	if rec.count(EventCatalogProgress) == 0 {
		t.Fatal("no catalog:progress events")
	}
	st := a.CatalogStatus()
	if !st.Ready || st.State != CatalogStateReady {
		t.Fatalf("catalogue not ready: %+v", st)
	}

	page := a.SearchPackages(SearchQuery{Text: "lib", Limit: 10})
	if page.Error != nil {
		t.Fatalf("SearchPackages: %v", page.Error)
	}
	if len(page.Rows) == 0 || page.Total < len(page.Rows) {
		t.Fatalf("bad page: rows=%d total=%d", len(page.Rows), page.Total)
	}
	cats := a.Categories()
	if cats.Error != nil || len(cats.Categories) == 0 {
		t.Fatalf("Categories: %v (%d rows)", cats.Error, len(cats.Categories))
	}
	det := a.GetPackage(page.Rows[0].Name)
	if det.Error != nil || !det.Found {
		t.Fatalf("GetPackage: %v found=%v", det.Error, det.Found)
	}
	if got := a.SearchPackages(SearchQuery{Limit: SearchPageMax + 1}); !got.Truncated {
		t.Fatal("an over-large limit should report Truncated")
	}

	// Screen 3 - the tray.
	name := page.Rows[0].Name
	s := a.AddPackages([]string{name, name})
	if s.Added != 1 || s.Total != 1 {
		t.Fatalf("AddPackages deduplication: %+v", s)
	}
	if rec.count(EventSelectionChanged) == 0 {
		t.Fatal("selection:changed did not fire")
	}
	if s = a.AddURLs([]URLInput{{URL: "https://example.invalid/vendor.deb"}, {URL: "not a url"}}); s.URLCount != 1 || len(s.Rejected) != 1 {
		t.Fatalf("AddURLs: %+v", s)
	}
	// A supplied digest must survive into the tray: it is what makes the input
	// operator-attested, and a UI that collects one and drops it is worse than
	// one that never asked.
	attested := "https://example.invalid/attested.deb"
	digest := "SHA256:ABcd" + strings.Repeat("0", 60)
	if s = a.AddURLs([]URLInput{{URL: attested, SHA256: digest}}); len(s.Rejected) != 0 {
		t.Fatalf("AddURLs with a digest rejected it: %+v", s.Rejected)
	}
	selPage := a.SelectionPage(0, 100)
	found := false
	for _, e := range selPage.Entries {
		if e.Key != attested {
			continue
		}
		found = true
		if e.SHA256 != "abcd"+strings.Repeat("0", 60) {
			t.Fatalf("digest not normalised onto the entry: %q", e.SHA256)
		}
	}
	if !found {
		t.Fatal("the attested URL is not in the selection")
	}
	// And a malformed one must be refused, not silently ignored.
	if s = a.AddURLs([]URLInput{{URL: "https://example.invalid/bad.deb", SHA256: "nope"}}); len(s.Rejected) == 0 {
		t.Fatal("a malformed digest was accepted silently")
	}
	parsed := a.ParsePackageList("# a comment\n" + name + "\nNotAName\n")
	if parsed.Count != 1 || len(parsed.Rejected) != 1 {
		t.Fatalf("ParsePackageList: %+v", parsed)
	}
	// Three: one apt name, one plain URL, one URL with an expected digest.
	if p := a.SelectionPage(0, 0); p.Total != 3 || len(p.Entries) != 3 {
		t.Fatalf("SelectionPage: %+v", p)
	}
	if got := a.PackageRows(make([]string, PackageRowsMax+1)); got.Error == nil {
		t.Fatal("an over-large hydrate batch should be refused")
	}

	// Screen 4 - build.
	if pv := a.PreviewCommand(BuildOptions{OutputDir: t.TempDir()}); pv.Error != nil || len(pv.Argv) == 0 || pv.Display == "" {
		t.Fatalf("PreviewCommand: %v argv=%v", pv.Error, pv.Argv)
	}
	if r := a.StartBuild(BuildOptions{}); r.OK {
		t.Fatal("a build with no output folder should be refused")
	}
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatalf("StartBuild: %v", r.Error)
	}
	waitFor(t, "build:finished", func() bool { return rec.count(EventBuildFinished) == 1 })
	bs := a.BuildStatus()
	if !bs.Finished || bs.Running {
		t.Fatalf("build status: %+v", bs)
	}
	if len(bs.Command) == 0 {
		t.Fatal("the build did not record the command it ran")
	}
	if bs.EventCount == 0 {
		t.Fatal("no raw events were recorded for the drawer")
	}
	log := a.BuildLog(0, 10)
	if len(log.Events) == 0 || log.Events[0].Seq != 1 {
		t.Fatalf("BuildLog: %+v", log)
	}
	next := a.BuildLog(log.NextSeq, 10)
	for _, e := range next.Events {
		if e.Seq <= log.NextSeq {
			t.Fatal("BuildLog paging returned an event twice")
		}
	}

	// Screen 5 - export and verify.
	if vols := a.ListVolumes(); vols.Error != nil {
		t.Fatalf("ListVolumes: %v", vols.Error)
	}
	if bs.Summary != nil && bs.Summary.BundlePath != "" {
		dest := t.TempDir()
		if r := a.StartExport(ExportOptions{Destination: dest}); r.OK {
			waitFor(t, "export:finished", func() bool { return rec.count(EventExportFinished) == 1 })
			if es := a.ExportStatus(); !es.Finished {
				t.Fatalf("export status: %+v", es)
			}
		}
	}
	if r := a.StartVerify(""); r.OK {
		waitFor(t, "verify:finished", func() bool { return rec.count(EventVerifyFinished) == 1 })
		if vs := a.VerifyStatus(); !vs.Finished {
			t.Fatalf("verify status: %+v", vs)
		}
	}

	// Every event that fired must be a name from the frozen list.
	known := map[string]bool{}
	for _, n := range EventNames() {
		known[n] = true
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for n := range rec.events {
		if !known[n] {
			t.Fatalf("emitted %q, which is not in EventNames()", n)
		}
	}
}

func TestEveryErrorIsActionable(t *testing.T) {
	a, _ := newApp(t)
	cases := map[string]*UIError{
		"no target":     a.StartBuild(BuildOptions{OutputDir: "x"}).Error,
		"not ready":     a.SearchPackages(SearchQuery{}).Error,
		"bad target":    a.SelectTarget(TargetSelection{Kind: "nonsense"}).Error,
		"no bundle":     a.StartVerify("").Error,
		"too many":      a.AddPackages(make([]string, AddPackagesMax+1)).Error,
		"no such check": a.RunReadinessAction("nope").Error,
	}
	for name, e := range cases {
		if e == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if e.Code == "" || e.Message == "" || e.Hint == "" {
			t.Fatalf("%s: not actionable: %+v", name, e)
		}
	}
}

func TestHeadlessDropsEventsInsteadOfDying(t *testing.T) {
	a := New(Deps{CLI: clifake.New(), Catalog: catfake.NewDefault()})
	a.Startup(context.Background())
	defer a.Shutdown(context.Background())

	a.emitAppError(&UIError{Code: "x", Message: "y"})
	if a.droppedEventCount() != 1 {
		t.Fatalf("dropped = %d, want 1", a.droppedEventCount())
	}
	if info := a.AppInfo(); !info.Headless {
		t.Fatal("AppInfo should report headless with no Wails runtime")
	}
}

// ---------------------------------------------------------------------------
// The readiness screen must talk about the binary this application runs.
//
// internal/readiness deliberately does not import internal/cliadapter, and its
// BinaryLocator comment names the application layer as the place the two are
// made to agree. Both tests below cover one half of that wiring; both failed
// before it existed, and the first one failed in a way that stopped the whole
// application: the debark-binary row is the only blocking row on the first
// screen, so a GUI running an explicitly configured binary reported it missing
// and refused to go on. Found by hack/e2e against a real binary off PATH.
// ---------------------------------------------------------------------------

// fakeProbePath is the path internal/cliadapter/fake reports from Probe. The
// file need not exist: these tests assert which path the readiness screen
// talks about, not what a binary at it would say.
const fakeProbePath = "/usr/bin/debark"

func TestReadinessAsksTheAdapterWhereDebarkIs(t *testing.T) {
	a, rec := newApp(t)

	if r := a.StartReadinessCheck(); !r.OK {
		t.Fatalf("StartReadinessCheck: %v", r.Error)
	}
	waitFor(t, "readiness:finished", func() bool { return rec.count(EventReadinessFinished) == 1 })

	var row *ReadinessCheck
	rep := a.Readiness()
	for i := range rep.Checks {
		if rep.Checks[i].ID == "debark-binary" {
			row = &rep.Checks[i]
		}
	}
	if row == nil {
		t.Fatal("there is no debark-binary row")
	}
	if !strings.Contains(row.Summary+row.Detail, fakeProbePath) {
		t.Fatalf("the row does not name the binary the app uses (%s): summary=%q detail=%q",
			fakeProbePath, row.Summary, row.Detail)
	}
	// Whatever a binary at that path answers, "not found on this machine" is
	// the one verdict that cannot be right: the application located it.
	if row.Status == ReadinessProblem && row.Severity == SeverityBlocking {
		t.Fatalf("the row blocks the whole application: %s", row.Summary)
	}
}

// e2eActionChecker returns exactly the signing-key row internal/readiness
// writes when no key exists, with its remedy command spelled as the bare
// program name — which is correct for that package and wrong to exec.
type e2eActionChecker struct{}

func (e2eActionChecker) Run(context.Context) readiness.Report {
	return readiness.Report{Results: []readiness.Result{e2eActionChecker{}.row()}}
}

func (e2eActionChecker) RunOne(context.Context, string) (readiness.Result, error) {
	return e2eActionChecker{}.row(), nil
}

func (e2eActionChecker) row() readiness.Result {
	return readiness.Result{
		ID:       "signing-key",
		Title:    "Operator signing key",
		Status:   readiness.StatusProblem,
		Severity: readiness.SeverityInfo,
		Summary:  "No operator signing key was found, so bundles will be built unsigned.",
		Remedy:   "Create one now, or carry on and sign later.",
		Action: &readiness.Action{
			Label:   "Create a signing key",
			Command: []string{"debark", "keygen", "--out", "/keys/operator.key"},
		},
	}
}

func TestReadinessRemedyRunsTheBinaryTheAppRuns(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	var ran []string
	a := New(Deps{
		CLI:       clifake.New(),
		Catalog:   catfake.NewDefault(),
		Readiness: e2eActionChecker{},
		Runner: func(_ context.Context, name string, args ...string) readiness.CommandOutput {
			mu.Lock()
			ran = append([]string{name}, args...)
			mu.Unlock()
			return readiness.CommandOutput{}
		},
		Emit: rec.emit,
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	if r := a.StartReadinessCheck(); !r.OK {
		t.Fatalf("StartReadinessCheck: %v", r.Error)
	}
	waitFor(t, "readiness:finished", func() bool { return rec.count(EventReadinessFinished) == 1 })

	rep := a.Readiness()
	if len(rep.Checks) != 1 || rep.Checks[0].Action == nil {
		t.Fatalf("expected one row with an action, got %+v", rep.Checks)
	}
	act := rep.Checks[0].Action
	want := []string{fakeProbePath, "keygen", "--out", "/keys/operator.key"}
	if !slices.Equal(act.Command, want) {
		t.Fatalf("action argv = %v, want %v", act.Command, want)
	}
	// Rule 8: the command shown is the command run, so the display is
	// rendered from the rewritten argv and not from the original.
	if wantDisplay := strings.Join(want, " "); act.Display != wantDisplay {
		t.Fatalf("action display = %q, want %q", act.Display, wantDisplay)
	}

	if r := a.RunReadinessAction("signing-key"); !r.OK {
		t.Fatalf("RunReadinessAction: %v", r.Error)
	}
	waitFor(t, "readiness:finished (action)", func() bool { return rec.count(EventReadinessFinished) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(ran, want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
}

// A report the frontend (or a Go caller) holds must not change under it. This
// package writes Checks[i] in place while a single row re-runs, so a plain
// struct copy shared the array and the caller's rows changed mid-flight.
func TestReadinessReportRowsAreTheCallersOwn(t *testing.T) {
	a, rec := newApp(t)
	if r := a.StartReadinessCheck(); !r.OK {
		t.Fatalf("StartReadinessCheck: %v", r.Error)
	}
	waitFor(t, "readiness:finished", func() bool { return rec.count(EventReadinessFinished) == 1 })

	first, second := a.Readiness(), a.Readiness()
	if len(first.Checks) == 0 {
		t.Fatal("readiness produced no rows")
	}
	first.Checks[0].Summary = "clobbered"
	if second.Checks[0].Summary == "clobbered" {
		t.Fatal("two Readiness() calls share row storage")
	}
	if a.Readiness().Checks[0].Summary == "clobbered" {
		t.Fatal("writing to a returned report changed the application's own state")
	}
}

// ---------------------------------------------------------------------------
// The verification screen must check against the key the build signed with.
// ---------------------------------------------------------------------------

// e2eKeyedAdapter is the fake adapter plus cliadapter.KeyedVerifier, recording
// which keys the application asked for.
type e2eKeyedAdapter struct {
	cliadapter.Adapter
	mu   sync.Mutex
	keys []string
	seen bool
}

func (k *e2eKeyedAdapter) VerifyWithKeys(ctx context.Context, bundlePath string, keys []string) (cliadapter.VerifyReport, error) {
	k.mu.Lock()
	k.keys, k.seen = keys, true
	k.mu.Unlock()
	// The embedded Adapter supplies Verify; this type only overrides
	// VerifyWithKeys, so the promoted method is the plain one.
	return k.Verify(ctx, bundlePath)
}

func TestVerifyChecksAgainstTheOperatorsOwnKey(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "operator.key")
	pub := cliadapter.PublicKeyPath(priv)

	rec := newRecorder()
	cli := &e2eKeyedAdapter{Adapter: clifake.New()}
	a := New(Deps{
		CLI:            cli,
		Catalog:        catfake.NewDefault(),
		Emit:           rec.emit,
		SigningKeyPath: priv,
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	// No key on disk yet: nothing is passed, so debark's own configured
	// trust set stays in charge. This is the pre-existing behaviour and must
	// not change.
	if r := a.StartVerify(filepath.Join(dir, "bundle")); !r.OK {
		t.Fatalf("StartVerify: %v", r.Error)
	}
	waitFor(t, "verify:finished", func() bool { return rec.count(EventVerifyFinished) == 1 })
	cli.mu.Lock()
	seen := cli.seen
	cli.mu.Unlock()
	if seen {
		t.Fatal("a key was passed although none exists on disk")
	}

	// With the operator's public key beside the private one — which is what
	// `debark keygen` writes — it must be what the bundle is checked
	// against. Without this the GUI cannot verify a bundle it signed itself.
	if err := os.WriteFile(pub, []byte("untrusted-comment: e2e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := a.StartVerify(filepath.Join(dir, "bundle")); !r.OK {
		t.Fatalf("StartVerify: %v", r.Error)
	}
	waitFor(t, "verify:finished", func() bool { return rec.count(EventVerifyFinished) == 2 })
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if !slices.Equal(cli.keys, []string{pub}) {
		t.Fatalf("verified against %v, want %v", cli.keys, []string{pub})
	}
}

// New must know where the signing key is even when nobody told it, or the
// application and the readiness screen are looking at different files.
func TestSigningKeyPathDefaultsToTheReadinessDefault(t *testing.T) {
	a := New(Deps{CLI: clifake.New(), Catalog: catfake.NewDefault()})
	t.Cleanup(func() { a.Shutdown(context.Background()) })
	if want := readiness.DefaultSigningKeyPath(); a.keyPath != want {
		t.Fatalf("keyPath = %q, want the readiness default %q", a.keyPath, want)
	}
}

// TestCatalogBuildAssignsPackageCount is the build path's half of the defect
// TestWarmStartCatalogStatusCounts covers on the cache path.
//
// Both were broken and fixing one would not have fixed the other, which is why
// there are two tests: a first run that builds a catalogue and then reports 0
// packages is the same wrong header as a second run that loads one and reports
// 0. The count comes off the catalogue rather than off the build, so a build
// that succeeded and a cache that was loaded cannot disagree about it.
func TestCatalogBuildAssignsPackageCount(t *testing.T) {
	r := newRecorder()
	cat := catfake.NewDefault()
	cat.SetReady(false)
	a := New(Deps{CLI: clifake.New(), Catalog: cat, Emit: r.emit, Version: "test"})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	if sel := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); sel.Error != nil {
		t.Fatalf("SelectTarget: %v", sel.Error)
	}
	if got := a.CatalogStatus().PackageCount; got != 0 {
		t.Fatalf("PackageCount before any catalogue = %d, want 0", got)
	}
	if res := a.StartCatalogBuild(false); !res.OK {
		t.Fatalf("StartCatalogBuild: %v", res.Error)
	}
	waitFor(t, "catalog:finished", func() bool { return r.count(EventCatalogFinished) == 1 })

	st := a.CatalogStatus()
	if !st.Ready {
		t.Fatalf("catalogue not ready after a successful build: %+v", st)
	}
	if st.PackageCount != cat.Len() {
		t.Fatalf("CatalogStatus.PackageCount after a build = %d, want %d", st.PackageCount, cat.Len())
	}
	if st.BuiltAt == "" {
		t.Fatal("CatalogStatus.BuiltAt empty after a successful build")
	}
	if cats := a.Categories(); cats.Total != cat.Len() {
		t.Fatalf("Categories().Total after a build = %d, want %d", cats.Total, cat.Len())
	}
}
