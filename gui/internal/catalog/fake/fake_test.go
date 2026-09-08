package fake_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/catalog/fake"
)

// testTarget is a Target complete enough for Build to accept, shaped the way
// one built from an Ubuntu base definition really is.
func testTarget() catalog.Target {
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
			Suites:     []string{"noble", "noble-updates"},
			Components: []string{"main", "universe"},
		}},
	}
}

func TestCorpusIsDeterministicAndWellFormed(t *testing.T) {
	t.Parallel()

	a, b := fake.New(3000), fake.New(3000)
	if a.Len() != b.Len() {
		t.Fatalf("two corpora of the same size differ in length: %d vs %d", a.Len(), b.Len())
	}

	ctx := context.Background()
	pa, err := a.Search(ctx, catalog.Query{Limit: catalog.MaxPageSize})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	pb, err := b.Search(ctx, catalog.Query{Limit: catalog.MaxPageSize})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Entries hold a slice, so == is unavailable; compare the fields that
	// would betray a non-deterministic generator.
	for i := range pa.Entries {
		x, y := pa.Entries[i], pb.Entries[i]
		if x.Name != y.Name || x.Version != y.Version || x.Section != y.Section ||
			x.InstalledSizeKiB != y.InstalledSizeKiB || x.Suite != y.Suite {
			t.Fatalf("row %d differs between two identical corpora: %+v vs %+v", i, x, y)
		}
	}

	// Every row must be well formed enough to render: a name, a version, an
	// architecture and a non-negative size. A blank cell in the picker is a
	// defect in the corpus, not in the screen.
	var prev string
	seen := map[string]bool{}
	for i, e := range pa.Entries {
		switch {
		case e.Name == "":
			t.Fatalf("row %d has no name", i)
		case e.Version == "":
			t.Fatalf("row %d (%s) has no version", i, e.Name)
		case e.Arch == "":
			t.Fatalf("row %d (%s) has no arch", i, e.Name)
		case e.Summary == "":
			t.Fatalf("row %d (%s) has no summary", i, e.Name)
		case e.InstalledSizeKiB < 0 || e.DownloadSizeBytes < 0:
			t.Fatalf("row %d (%s) has a negative size", i, e.Name)
		case seen[e.Name]:
			t.Fatalf("duplicate package name %q", e.Name)
		case prev != "" && e.Name < prev:
			t.Fatalf("corpus is not sorted by name: %q after %q", e.Name, prev)
		}
		seen[e.Name] = true
		prev = e.Name
	}
}

func TestCorpusSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ask     int
		wantMin int
		wantMax int
	}{
		{"zero falls back to the curated table", 0, 100, 200},
		{"small ask still keeps the curated table", 10, 100, 200},
		{"exact ask is honoured", 2500, 2500, 2500},
		{"default", fake.DefaultEntryCount, fake.DefaultEntryCount, fake.DefaultEntryCount},
		{"full archive", 70000, 70000, 70000},
		{"over the maximum is clamped", fake.MaxEntryCount + 5000, fake.MaxEntryCount - 200, fake.MaxEntryCount},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fake.New(tc.ask).Len()
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("New(%d).Len() = %d, want between %d and %d", tc.ask, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()

	f := fake.New(fake.DefaultEntryCount)
	ctx := context.Background()

	tests := []struct {
		name  string
		query catalog.Query
		check func(t *testing.T, p catalog.Page)
	}{
		{
			name:  "empty query returns the whole catalogue paged",
			query: catalog.Query{},
			check: func(t *testing.T, p catalog.Page) {
				if p.Total != f.Len() {
					t.Fatalf("Total = %d, want %d", p.Total, f.Len())
				}
				if len(p.Entries) != catalog.DefaultPageSize {
					t.Fatalf("got %d rows, want the default page size %d", len(p.Entries), catalog.DefaultPageSize)
				}
			},
		},
		{
			name:  "exact name is found and ranked first",
			query: catalog.Query{Text: "firefox"},
			check: func(t *testing.T, p catalog.Page) {
				if len(p.Entries) == 0 || p.Entries[0].Name != "firefox" {
					t.Fatalf("first row = %+v, want firefox", firstName(p))
				}
			},
		},
		{
			name:  "search is case insensitive",
			query: catalog.Query{Text: "FiReFoX"},
			check: func(t *testing.T, p catalog.Page) {
				if len(p.Entries) == 0 || p.Entries[0].Name != "firefox" {
					t.Fatalf("first row = %q, want firefox", firstName(p))
				}
			},
		},
		{
			name:  "substring matches inside a name",
			query: catalog.Query{Text: "postgres"},
			check: func(t *testing.T, p catalog.Page) {
				if !containsName(p, "postgresql") || !containsName(p, "postgresql-client") {
					t.Fatalf("postgres search missed the postgresql packages: %v", names(p))
				}
			},
		},
		{
			name:  "name matches sort before summary-only matches",
			query: catalog.Query{Text: "editor", Limit: catalog.MaxPageSize},
			check: func(t *testing.T, p catalog.Page) {
				sawSummaryOnly := false
				for _, e := range p.Entries {
					nameMatch := strings.Contains(strings.ToLower(e.Name+" "+e.AppName), "editor")
					if !nameMatch {
						sawSummaryOnly = true
						continue
					}
					if sawSummaryOnly {
						t.Fatalf("name match %q came after a summary-only match", e.Name)
					}
				}
			},
		},
		{
			name:  "the DEP-11 application name is searchable",
			query: catalog.Query{Text: "image manipulation", Limit: catalog.MaxPageSize},
			check: func(t *testing.T, p catalog.Page) {
				if !containsName(p, "gimp") {
					t.Fatalf("searching the human application name did not find gimp: %v", names(p))
				}
			},
		},
		{
			name:  "apps-only returns only DEP-11 applications",
			query: catalog.Query{AppsOnly: true, Limit: catalog.MaxPageSize},
			check: func(t *testing.T, p catalog.Page) {
				if p.Total == 0 {
					t.Fatal("apps-only returned nothing")
				}
				if p.Total >= f.Len() {
					t.Fatalf("apps-only Total = %d is not a subset of %d", p.Total, f.Len())
				}
				for _, e := range p.Entries {
					if !e.IsApp {
						t.Fatalf("%q is not an application", e.Name)
					}
				}
			},
		},
		{
			name:  "an application category filters to that category",
			query: catalog.Query{Category: catalog.AppCategoryID("Graphics"), Limit: catalog.MaxPageSize},
			check: func(t *testing.T, p catalog.Page) {
				if p.Total == 0 {
					t.Fatal("Graphics returned nothing")
				}
				for _, e := range p.Entries {
					if !hasCategory(e, "Graphics") {
						t.Fatalf("%q is not in Graphics: %v", e.Name, e.Categories)
					}
				}
			},
		},
		{
			name:  "a section category filters on the apt section",
			query: catalog.Query{Category: catalog.SectionCategoryID("libs"), Limit: catalog.MaxPageSize},
			check: func(t *testing.T, p catalog.Page) {
				if p.Total == 0 {
					t.Fatal("section libs returned nothing")
				}
				for _, e := range p.Entries {
					if e.Section != "libs" {
						t.Fatalf("%q has section %q, want libs", e.Name, e.Section)
					}
				}
			},
		},
		{
			name:  "an unparseable category is ignored rather than failing",
			query: catalog.Query{Category: "not-a-category-id"},
			check: func(t *testing.T, p catalog.Page) {
				if p.Total != f.Len() {
					t.Fatalf("Total = %d, want the whole catalogue %d", p.Total, f.Len())
				}
			},
		},
		{
			name:  "text and category compose",
			query: catalog.Query{Text: "lib", Category: catalog.SectionCategoryID("libdevel"), Limit: catalog.MaxPageSize},
			check: func(t *testing.T, p catalog.Page) {
				for _, e := range p.Entries {
					if e.Section != "libdevel" || !strings.Contains(strings.ToLower(e.Name+" "+e.Summary), "lib") {
						t.Fatalf("%q does not satisfy both filters", e.Name)
					}
				}
			},
		},
		{
			name:  "no match returns an empty page and a zero total",
			query: catalog.Query{Text: "zzzz-no-such-package-zzzz"},
			check: func(t *testing.T, p catalog.Page) {
				if p.Total != 0 || len(p.Entries) != 0 {
					t.Fatalf("Total = %d, rows = %d, want 0 and 0", p.Total, len(p.Entries))
				}
			},
		},
		{
			name:  "the query is echoed back normalised",
			query: catalog.Query{Text: "  git  ", Limit: 9999, Offset: -5},
			check: func(t *testing.T, p catalog.Page) {
				if p.Query.Text != "git" {
					t.Fatalf("Query.Text = %q, want the trimmed text", p.Query.Text)
				}
				if p.Query.Limit != catalog.MaxPageSize {
					t.Fatalf("Query.Limit = %d, want the clamp %d", p.Query.Limit, catalog.MaxPageSize)
				}
				if p.Query.Offset != 0 || p.Offset != 0 {
					t.Fatalf("negative offset was not normalised: %d/%d", p.Query.Offset, p.Offset)
				}
			},
		},
		{
			name:  "an offset past the end returns no rows but keeps the total",
			query: catalog.Query{Offset: 10_000_000},
			check: func(t *testing.T, p catalog.Page) {
				if len(p.Entries) != 0 {
					t.Fatalf("got %d rows past the end", len(p.Entries))
				}
				if p.Total != f.Len() {
					t.Fatalf("Total = %d, want %d — the scrollbar must still be sizeable", p.Total, f.Len())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := f.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(p.Entries) > p.Query.Limit {
				t.Fatalf("page carries %d rows, over its own limit %d", len(p.Entries), p.Query.Limit)
			}
			tc.check(t, p)
		})
	}
}

// TestSearchPagingIsStable walks the whole result set one page at a time and
// checks it reconstructs exactly the single-page answer. This is the property
// the virtualiser depends on: it will ask for arbitrary windows, out of order,
// and must never see a row twice or miss one.
func TestSearchPagingIsStable(t *testing.T) {
	t.Parallel()

	f := fake.New(1200)
	ctx := context.Background()
	q := catalog.Query{Text: "lib"}

	whole, err := f.Search(ctx, catalog.Query{Text: "lib", Limit: catalog.MaxPageSize})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if whole.Total == 0 {
		t.Fatal("no results to page through")
	}

	const page = 37 // deliberately not a divisor of anything
	var walked []string
	for off := 0; off < whole.Total; off += page {
		q.Offset, q.Limit = off, page
		p, err := f.Search(ctx, q)
		if err != nil {
			t.Fatalf("Search at offset %d: %v", off, err)
		}
		if p.Total != whole.Total {
			t.Fatalf("Total changed between pages: %d then %d", whole.Total, p.Total)
		}
		if p.Offset != off {
			t.Fatalf("Page.Offset = %d, want %d", p.Offset, off)
		}
		for _, e := range p.Entries {
			walked = append(walked, e.Name)
		}
	}
	if len(walked) != whole.Total {
		t.Fatalf("walked %d rows, Total says %d", len(walked), whole.Total)
	}
	for i, name := range walked[:min(len(whole.Entries), len(walked))] {
		if name != whole.Entries[i].Name {
			t.Fatalf("row %d: paged walk gave %q, single page gave %q", i, name, whole.Entries[i].Name)
		}
	}
}

func TestGet(t *testing.T) {
	t.Parallel()

	f := fake.New(fake.DefaultEntryCount)
	ctx := context.Background()

	tests := []struct {
		name      string
		lookup    string
		wantOK    bool
		wantIsApp bool
	}{
		{"a curated command-line package", "git", true, false},
		{"a curated application", "gimp", true, true},
		{"a name that is not in the index", "definitely-not-a-package", false, false},
		{"the empty name", "", false, false},
		{"a prefix of a real name is not a match", "gi", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok, err := f.Get(ctx, tc.lookup)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("Get(%q) ok = %v, want %v", tc.lookup, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if e.Name != tc.lookup {
				t.Fatalf("Get(%q) returned %q", tc.lookup, e.Name)
			}
			if e.IsApp != tc.wantIsApp {
				t.Fatalf("Get(%q).IsApp = %v, want %v", tc.lookup, e.IsApp, tc.wantIsApp)
			}
		})
	}
}

func TestCategories(t *testing.T) {
	t.Parallel()

	f := fake.New(4000)
	ctx := context.Background()
	cats, err := f.Categories(ctx)
	if err != nil {
		t.Fatalf("Categories: %v", err)
	}
	if len(cats) == 0 {
		t.Fatal("no categories")
	}

	// Applications first, then sections, each ascending by name: the order
	// the contract promises so the sidebar does not reshuffle.
	seenSection := false
	var prev string
	for _, c := range cats {
		switch c.Tier {
		case catalog.TierApplication:
			if seenSection {
				t.Fatalf("application category %q came after a section", c.Name)
			}
		case catalog.TierSection:
			if !seenSection {
				seenSection, prev = true, ""
			}
		default:
			t.Fatalf("category %q has unknown tier %q", c.Name, c.Tier)
		}
		if prev != "" && c.Name < prev {
			t.Fatalf("categories out of order within a tier: %q after %q", c.Name, prev)
		}
		prev = c.Name

		if c.Count <= 0 {
			t.Fatalf("category %q has count %d", c.Name, c.Count)
		}
		tier, name, ok := catalog.ParseCategoryID(c.ID)
		if !ok || tier != c.Tier || name != c.Name {
			t.Fatalf("category id %q does not round-trip to (%q, %q)", c.ID, c.Tier, c.Name)
		}

		// The count must be the number of rows the filter actually returns,
		// or the sidebar lies about what clicking it will do.
		p, err := f.Search(ctx, catalog.Query{Category: c.ID, Limit: 1})
		if err != nil {
			t.Fatalf("Search in %q: %v", c.ID, err)
		}
		if p.Total != c.Count {
			t.Fatalf("category %q says %d but the filter returns %d", c.ID, c.Count, p.Total)
		}
	}
}

func TestBuildProgress(t *testing.T) {
	t.Parallel()

	f := fake.New(500)
	var got []catalog.Progress
	if err := f.Build(context.Background(), testTarget(), func(p catalog.Progress) {
		got = append(got, p)
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) < len(catalog.Phases) {
		t.Fatalf("got %d progress reports, want at least one per phase (%d)", len(got), len(catalog.Phases))
	}

	var lastOverall float64 = -1
	var lastPhaseIdx = -1
	sawDownloadBytes := false
	for i, p := range got {
		if p.Label == "" {
			t.Fatalf("report %d has no label — the UI must never show an unexplained bar", i)
		}
		if p.PhaseCount != len(catalog.Phases) {
			t.Fatalf("report %d PhaseCount = %d, want %d", i, p.PhaseCount, len(catalog.Phases))
		}
		if p.PhaseIndex != p.Phase.Index() {
			t.Fatalf("report %d PhaseIndex = %d but %q is at %d", i, p.PhaseIndex, p.Phase, p.Phase.Index())
		}
		if p.PhaseIndex < lastPhaseIdx {
			t.Fatalf("report %d went backwards: phase %q after %d", i, p.Phase, lastPhaseIdx)
		}
		lastPhaseIdx = p.PhaseIndex

		if o := p.OverallFraction(); o < lastOverall {
			t.Fatalf("report %d overall fraction went backwards: %v after %v", i, o, lastOverall)
		} else {
			lastOverall = o
		}
		if p.Phase == catalog.PhaseDownload {
			if p.BytesTotal <= 0 || p.BytesDone <= 0 || p.BytesDone > p.BytesTotal {
				t.Fatalf("download report %d has nonsense bytes: %d/%d", i, p.BytesDone, p.BytesTotal)
			}
			sawDownloadBytes = true
		}
	}
	if !sawDownloadBytes {
		t.Fatal("no download phase reported bytes")
	}

	last := got[len(got)-1]
	if last.Phase != catalog.PhaseDone {
		t.Fatalf("last report is %q, want %q", last.Phase, catalog.PhaseDone)
	}
	if last.OverallFraction() != 1 {
		t.Fatalf("the terminal report is at %v, want 1", last.OverallFraction())
	}
	if f.BuildCount() != 1 {
		t.Fatalf("BuildCount = %d, want 1", f.BuildCount())
	}
	if f.LastTarget().BaseID != testTarget().BaseID {
		t.Fatalf("LastTarget = %+v", f.LastTarget())
	}
}

func TestBuildErrorPaths(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("mirror is on fire")

	tests := []struct {
		name    string
		opts    fake.Options
		target  catalog.Target
		prepare func(*fake.Fake)
		wantIs  error
		wantErr bool
		// wantProgressBefore, when > 0, requires at least that many progress
		// reports before the failure — a bar that fails at 0% is not the same
		// UI state as one that fails at 40%.
		wantProgressBefore int
	}{
		{
			name:   "the happy path",
			target: testTarget(),
		},
		{
			name:    "an empty target is rejected",
			target:  catalog.Target{},
			wantErr: true,
		},
		{
			name:               "a mid-way failure reports progress first",
			opts:               fake.Options{BuildFailAt: 0.5, BuildErr: sentinel},
			target:             testTarget(),
			wantIs:             sentinel,
			wantErr:            true,
			wantProgressBefore: 3,
		},
		{
			name:    "a failure with no error configured still fails",
			opts:    fake.Options{BuildFailAt: 0.1},
			target:  testTarget(),
			wantErr: true,
		},
		{
			name:    "a closed catalogue refuses to build",
			target:  testTarget(),
			prepare: func(f *fake.Fake) { _ = f.Close() },
			wantIs:  catalog.ErrClosed,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fake.New(300)
			f.SetOptions(tc.opts)
			if tc.prepare != nil {
				tc.prepare(f)
			}
			n := 0
			err := f.Build(context.Background(), tc.target, func(catalog.Progress) { n++ })
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("want an error, got nil")
			case !tc.wantErr && err != nil:
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("error %v does not wrap %v", err, tc.wantIs)
			}
			if n < tc.wantProgressBefore {
				t.Fatalf("got %d progress reports before the failure, want at least %d", n, tc.wantProgressBefore)
			}
		})
	}
}

func TestBuildIsCancellable(t *testing.T) {
	t.Parallel()

	f := fake.New(300)
	f.SetOptions(fake.Options{BuildDelay: 2 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	done := make(chan error, 1)
	go func() {
		done <- f.Build(ctx, testTarget(), func(catalog.Progress) { seen++ })
	}()

	// Give it enough time to have started, then pull the rug.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled build returned %v, want something wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled build did not return promptly — the UI's Cancel button would appear dead")
	}

	// A cancelled build must not leave the catalogue claiming it built one,
	// and must not leave the fake wedged in the building state.
	if err := f.Build(context.Background(), testTarget(), nil); err != nil {
		t.Fatalf("a build after a cancelled one failed: %v", err)
	}
}

func TestBuildRejectsConcurrentBuilds(t *testing.T) {
	t.Parallel()

	f := fake.New(300)
	f.SetOptions(fake.Options{BuildDelay: 500 * time.Millisecond})

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.Build(context.Background(), testTarget(), func(catalog.Progress) {
			select {
			case started <- struct{}{}:
			default:
			}
		})
	}()
	<-started

	if err := f.Build(context.Background(), testTarget(), nil); !errors.Is(err, catalog.ErrBuildInProgress) {
		t.Fatalf("second Build returned %v, want ErrBuildInProgress", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first Build: %v", err)
	}
}

func TestReadyAndNotBuiltState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := fake.New(300)

	if ok, err := f.Ready(testTarget()); err != nil || !ok {
		t.Fatalf("a new Fake should be ready: %v %v", ok, err)
	}

	f.SetReady(false)
	ok, err := f.Ready(testTarget())
	if err != nil || ok {
		t.Fatalf("after SetReady(false): ok = %v, err = %v", ok, err)
	}

	// Every read path must report the not-built state rather than pretending
	// the catalogue is empty. "No results" and "no catalogue" are different
	// screens.
	if _, err := f.Search(ctx, catalog.Query{}); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Search = %v, want ErrNotBuilt", err)
	}
	if _, err := f.Categories(ctx); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Categories = %v, want ErrNotBuilt", err)
	}
	if _, _, err := f.Get(ctx, "git"); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Get = %v, want ErrNotBuilt", err)
	}

	if err := f.Build(ctx, testTarget(), nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ok, err := f.Ready(testTarget()); err != nil || !ok {
		t.Fatalf("a successful Build should make the catalogue ready: %v %v", ok, err)
	}
	if _, err := f.Search(ctx, catalog.Query{}); err != nil {
		t.Fatalf("Search after Build: %v", err)
	}
}

func TestInjectedFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	ctx := context.Background()

	tests := []struct {
		name string
		opts fake.Options
		call func(*fake.Fake) error
	}{
		{
			name: "Search",
			opts: fake.Options{SearchErr: boom},
			call: func(f *fake.Fake) error { _, err := f.Search(ctx, catalog.Query{}); return err },
		},
		{
			name: "Categories",
			opts: fake.Options{CategoriesErr: boom},
			call: func(f *fake.Fake) error { _, err := f.Categories(ctx); return err },
		},
		{
			name: "Get",
			opts: fake.Options{GetErr: boom},
			call: func(f *fake.Fake) error { _, _, err := f.Get(ctx, "git"); return err },
		},
		{
			name: "Ready",
			opts: fake.Options{ReadyErr: boom},
			call: func(f *fake.Fake) error { _, err := f.Ready(testTarget()); return err },
		},
		{
			name: "Close",
			opts: fake.Options{CloseErr: boom},
			call: func(f *fake.Fake) error { return f.Close() },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fake.New(300)
			f.SetOptions(tc.opts)
			if err := tc.call(f); !errors.Is(err, boom) {
				t.Fatalf("%s returned %v, want the injected error", tc.name, err)
			}
		})
	}
}

func TestSearchDelayIsCancellable(t *testing.T) {
	t.Parallel()

	f := fake.New(300)
	f.SetOptions(fake.Options{SearchDelay: time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := f.Search(ctx, catalog.Query{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search returned %v, want the context deadline", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Search ignored the deadline for %v", d)
	}
}

func TestClosedCatalogRefusesEverything(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := fake.New(300)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}

	if _, err := f.Search(ctx, catalog.Query{}); !errors.Is(err, catalog.ErrClosed) {
		t.Fatalf("Search = %v, want ErrClosed", err)
	}
	if _, err := f.Categories(ctx); !errors.Is(err, catalog.ErrClosed) {
		t.Fatalf("Categories = %v, want ErrClosed", err)
	}
	if _, _, err := f.Get(ctx, "git"); !errors.Is(err, catalog.ErrClosed) {
		t.Fatalf("Get = %v, want ErrClosed", err)
	}
	if _, err := f.Ready(testTarget()); !errors.Is(err, catalog.ErrClosed) {
		t.Fatalf("Ready = %v, want ErrClosed", err)
	}
}

// TestConcurrentUse is the property the bridge actually needs: many goroutines
// searching while a build runs. Meaningful under -race, which CI runs.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()

	f := fake.New(2000)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.Build(ctx, testTarget(), func(catalog.Progress) {})
	}()

	var wg [8]chan struct{}
	for i := range wg {
		wg[i] = make(chan struct{})
		go func(c chan struct{}) {
			defer close(c)
			for n := 0; n < 50; n++ {
				if _, err := f.Search(ctx, catalog.Query{Text: "lib", Offset: n * 10}); err != nil {
					t.Errorf("Search: %v", err)
					return
				}
				if _, err := f.Categories(ctx); err != nil {
					t.Errorf("Categories: %v", err)
					return
				}
			}
		}(wg[i])
	}
	for _, c := range wg {
		<-c
	}
	<-done
}

// BenchmarkSearchFullArchive is the number that matters: one keystroke against
// a full-size corpus. The fake is not the real implementation, but if the fake
// cannot do this in single-digit milliseconds, no frontend built against it is
// testing anything realistic.
func BenchmarkSearchFullArchive(b *testing.B) {
	f := fake.New(70000)
	ctx := context.Background()
	q := catalog.Query{Text: "lib", Limit: catalog.DefaultPageSize}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Search(ctx, q); err != nil {
			b.Fatal(err)
		}
	}
}

func firstName(p catalog.Page) string {
	if len(p.Entries) == 0 {
		return "<empty page>"
	}
	return p.Entries[0].Name
}

func names(p catalog.Page) []string {
	out := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e.Name)
	}
	return out
}

func containsName(p catalog.Page, want string) bool {
	for _, e := range p.Entries {
		if e.Name == want {
			return true
		}
	}
	return false
}

func hasCategory(e catalog.Entry, want string) bool {
	for _, c := range e.Categories {
		if c == want {
			return true
		}
	}
	return false
}
