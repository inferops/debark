package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// searchTestCorpus is the fixture every ranking assertion in this file reads.
// It is deliberately small and hand-written: the ranking rule is a claim about
// which of two named packages comes first, and a generated corpus cannot state
// that claim.
//
// It is also deliberately not in name order, so that every test that asserts an
// order is also asserting that the index established one.
func searchTestCorpus() []Entry {
	return []Entry{
		{Name: "xab", Section: "misc", Summary: "xab tool"},
		{Name: "vim-youcompleteme", Section: "editors", Summary: "fast, as-you-type, fuzzy-search code completion for Vim"},
		{Name: "firefox", Section: "web", Summary: "Safe and easy web browser from Mozilla",
			AppName: "Firefox Web Browser", Categories: []string{"Network", "WebBrowser"}, IsApp: true},
		{Name: "ab", Section: "misc", Summary: "alpha beta"},
		{Name: "libstdc++6", Section: "universe/libs", Summary: "GNU Standard C++ Library v3"},
		{Name: "gnumeric", Section: "gnome", Summary: "spreadsheet application for GNOME",
			AppName: "Gnumeric Spreadsheet", Categories: []string{"Office", "Spreadsheet"}, IsApp: true},
		{Name: "vim", Section: "editors", Summary: "Vi IMproved - enhanced vi editor"},
		{Name: "w3m", Section: "web", Summary: "text-based web browser"},
		{Name: "chromium-browser", Section: "web", Summary: "Transitional package",
			AppName: "Chromium Web Browser", Categories: []string{"Network", "WebBrowser"}, IsApp: true},
		{Name: "x-ab", Section: "misc", Summary: "cross ab"},
		{Name: "zlib1g", Section: "libs", Summary: "compression library - runtime"},
		{Name: "vim-tiny", Section: "editors", Summary: "Vi IMproved - enhanced vi editor - compact version"},
		{Name: "gimp", Section: "graphics", Summary: "GNU Image Manipulation Program",
			AppName: "GNU Image Manipulation Program", Categories: []string{"Graphics", "RasterGraphics"}, IsApp: true},
		{Name: "abc", Section: "misc", Summary: "alphabet soup"},
		{Name: "libc6", Section: "libs", Summary: "GNU C Library: Shared libraries"},
		// A DEP-11 component that is not a desktop application but still
		// carries categories — an addon, in DEP-11 terms. It is what makes
		// AppsOnly and a category filter two different filters.
		{Name: "libreoffice-calc", Section: "editors", Summary: "office productivity suite -- spreadsheet",
			AppName: "LibreOffice Calc", Categories: []string{"Office", "Spreadsheet"}},
		{Name: "gvim", Section: "editors", Summary: "Vi IMproved - enhanced vi editor (GUI)"},
		// Accented prose in the DEP-11 fields, precomposed.
		{Name: "gnome-etiquettes", Section: "gnome", Summary: "Créer des étiquettes pour vos dossiers",
			AppName: "Générateur d'Étiquettes", Categories: []string{"Office"}, IsApp: true},
		// The same accent, decomposed: "e" followed by U+0301. It must fold to
		// the same thing the precomposed form does.
		{Name: "tearoom", Section: "utils", Summary: "Cafe\u0301 client for tea rooms"},
		// A DEP-11 category outside the thirteen freedesktop main ones: still
		// filterable, deliberately not advertised by Categories.
		{Name: "fooedit", Section: "editors", Summary: "a small text editor",
			AppName: "Foo Editor", Categories: []string{"TextEditor"}, IsApp: true},
	}
}

// searchTestNames is every name in searchTestCorpus, in the order the index
// must produce for a browse.
var searchTestNames = []string{
	"ab", "abc", "chromium-browser", "firefox", "fooedit", "gimp",
	"gnome-etiquettes", "gnumeric", "gvim", "libc6", "libreoffice-calc",
	"libstdc++6", "tearoom", "vim", "vim-tiny", "vim-youcompleteme", "w3m",
	"x-ab", "xab", "zlib1g",
}

func searchTestIndex(t testing.TB) *searchIndex {
	t.Helper()
	return newSearchIndex(searchTestCorpus())
}

func searchTestPageNames(p Page) []string {
	out := make([]string, len(p.Entries))
	for i, e := range p.Entries {
		out[i] = e.Name
	}
	return out
}

func searchTestSearch(t testing.TB, ix *searchIndex, q Query) Page {
	t.Helper()
	p, err := ix.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Ranking
// ---------------------------------------------------------------------------

func TestSearchRanking(t *testing.T) {
	ix := searchTestIndex(t)

	cases := []struct {
		name  string
		text  string
		want  []string // the full ordered result set
		about string
	}{
		{
			name:  "exact beats prefix beats word start beats substring",
			text:  "ab",
			want:  []string{"ab", "abc", "x-ab", "xab"},
			about: "the whole ladder in one query, over names that differ only in how they contain it",
		},
		{
			name:  "exact name first, then shortest prefix",
			text:  "vim",
			want:  []string{"vim", "vim-tiny", "vim-youcompleteme", "gvim"},
			about: "typing vim must not bury vim under vim-youcompleteme",
		},
		{
			name:  "name beats application name beats summary",
			text:  "browser",
			want:  []string{"chromium-browser", "firefox", "w3m"},
			about: "firefox is reachable only through its DEP-11 application name; w3m only through its summary",
		},
		{
			name:  "application name outranks a summary-only match",
			text:  "spreadsheet",
			want:  []string{"gnumeric", "libreoffice-calc"},
			about: "someone typing a product category is naming gnumeric, not describing libreoffice",
		},
		{
			name:  "case is folded",
			text:  "FiReFoX",
			want:  []string{"firefox"},
			about: "",
		},
		{
			name:  "accents are folded on the query",
			text:  "GÉNÉRATEUR",
			want:  []string{"gnome-etiquettes"},
			about: "",
		},
		{
			name:  "accents are folded on the index",
			text:  "etiquettes",
			want:  []string{"gnome-etiquettes"},
			about: "an ASCII query must reach an accented application name",
		},
		{
			name:  "decomposed accents fold the same as precomposed ones",
			text:  "café",
			want:  []string{"tearoom"},
			about: "the summary spells it e + U+0301; the query spells it U+00E9",
		},
		{
			name:  "punctuation is literal text",
			text:  "c++",
			want:  []string{"libstdc++6"},
			about: "",
		},
		{
			name:  "regex metacharacters match nothing rather than everything",
			text:  ".*",
			want:  nil,
			about: "if this ever returns the catalogue, the search has grown a pattern language",
		},
		{
			name:  "a query matching nothing is an empty page, not an error",
			text:  "no-such-package-anywhere",
			want:  nil,
			about: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := searchTestSearch(t, ix, Query{Text: tc.text})
			got := searchTestPageNames(p)
			if len(got) != len(tc.want) {
				t.Fatalf("Search(%q) returned %v, want %v (%s)", tc.text, got, tc.want, tc.about)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Search(%q) returned %v, want %v (%s)", tc.text, got, tc.want, tc.about)
				}
			}
			if p.Total != len(tc.want) {
				t.Errorf("Search(%q).Total = %d, want %d", tc.text, p.Total, len(tc.want))
			}
			if p.Entries == nil {
				t.Errorf("Search(%q).Entries is nil; it must marshal as [] and not null", tc.text)
			}
		})
	}
}

func TestSearchRankingIsStable(t *testing.T) {
	ix := searchTestIndex(t)
	// A tie on rank must fall back to name, and the whole order must be a pure
	// function of the corpus and the query: a list that reshuffles between
	// keystrokes reads as broken.
	for _, text := range []string{"", "e", "lib", "vi", "editor", "web"} {
		first := searchTestPageNames(searchTestSearch(t, ix, Query{Text: text, Limit: MaxPageSize}))
		for i := 0; i < 8; i++ {
			again := searchTestPageNames(searchTestSearch(t, ix, Query{Text: text, Limit: MaxPageSize}))
			if strings.Join(first, ",") != strings.Join(again, ",") {
				t.Fatalf("Search(%q) is not deterministic:\n  %v\n  %v", text, first, again)
			}
		}
	}

	// Equal-rank rows specifically: every "libs" section row ties at the same
	// rank for an empty query, and must come back in name order.
	p := searchTestSearch(t, ix, Query{Category: SectionCategoryID("libs"), Limit: MaxPageSize})
	want := []string{"libc6", "libstdc++6", "zlib1g"}
	if got := searchTestPageNames(p); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tied rows came back as %v, want %v", got, want)
	}
}

func TestSearchRankingTiesBreakOnNameLengthThenName(t *testing.T) {
	// Four names that all match "zz" as a prefix, so tier is identical and only
	// the length-then-name tiebreak separates them.
	ix := newSearchIndex([]Entry{
		{Name: "zzz-long-name-here", Section: "misc"},
		{Name: "zzb", Section: "misc"},
		{Name: "zza", Section: "misc"},
		{Name: "zz-mid", Section: "misc"},
	})
	want := []string{"zza", "zzb", "zz-mid", "zzz-long-name-here"}
	got := searchTestPageNames(searchTestSearch(t, ix, Query{Text: "zz"}))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSearchEmptyQueryBrowsesEverything(t *testing.T) {
	ix := searchTestIndex(t)
	for _, text := range []string{"", "   ", "\t\n "} {
		p := searchTestSearch(t, ix, Query{Text: text, Limit: MaxPageSize})
		if p.Total != len(searchTestNames) {
			t.Fatalf("Search(%q).Total = %d, want %d", text, p.Total, len(searchTestNames))
		}
		got := searchTestPageNames(p)
		if strings.Join(got, ",") != strings.Join(searchTestNames, ",") {
			t.Fatalf("Search(%q) browse order = %v, want %v", text, got, searchTestNames)
		}
		if p.Query.Text != "" {
			t.Errorf("Search(%q).Query.Text = %q, want empty after normalisation", text, p.Query.Text)
		}
	}
}

// ---------------------------------------------------------------------------
// Paging
// ---------------------------------------------------------------------------

func TestSearchPaging(t *testing.T) {
	ix := searchTestIndex(t)
	n := len(searchTestNames)

	t.Run("total is the full match count, not the page length", func(t *testing.T) {
		for _, off := range []int{0, 1, 7, n - 1} {
			p := searchTestSearch(t, ix, Query{Offset: off, Limit: 3})
			if p.Total != n {
				t.Errorf("offset %d: Total = %d, want %d", off, p.Total, n)
			}
			if p.Offset != off {
				t.Errorf("offset %d: Page.Offset = %d", off, p.Offset)
			}
		}
	})

	t.Run("pages concatenate to the whole ordered set", func(t *testing.T) {
		var got []string
		for off := 0; off < n; off += 3 {
			got = append(got, searchTestPageNames(searchTestSearch(t, ix, Query{Offset: off, Limit: 3}))...)
		}
		if strings.Join(got, ",") != strings.Join(searchTestNames, ",") {
			t.Errorf("paged browse = %v, want %v", got, searchTestNames)
		}
	})

	t.Run("pages of a ranked query concatenate too", func(t *testing.T) {
		full := searchTestPageNames(searchTestSearch(t, ix, Query{Text: "e", Limit: MaxPageSize}))
		var got []string
		for off := 0; off < len(full); off += 2 {
			got = append(got, searchTestPageNames(searchTestSearch(t, ix, Query{Text: "e", Offset: off, Limit: 2}))...)
		}
		if strings.Join(got, ",") != strings.Join(full, ",") {
			t.Errorf("paged ranked query = %v, want %v", got, full)
		}
	})

	t.Run("offset past the end is an empty page with a real total", func(t *testing.T) {
		for _, off := range []int{n, n + 1, 1_000_000} {
			p := searchTestSearch(t, ix, Query{Offset: off})
			if len(p.Entries) != 0 {
				t.Errorf("offset %d returned %d rows", off, len(p.Entries))
			}
			if p.Entries == nil {
				t.Errorf("offset %d returned a nil slice", off)
			}
			if p.Total != n {
				t.Errorf("offset %d: Total = %d, want %d", off, p.Total, n)
			}
		}
		p := searchTestSearch(t, ix, Query{Text: "vim", Offset: 99})
		if len(p.Entries) != 0 || p.Total != 4 {
			t.Errorf("ranked offset past the end: %d rows, Total %d, want 0 rows and Total 4", len(p.Entries), p.Total)
		}
	})

	t.Run("negative offset is clamped", func(t *testing.T) {
		p := searchTestSearch(t, ix, Query{Offset: -5, Limit: 2})
		if p.Offset != 0 {
			t.Errorf("Page.Offset = %d, want 0", p.Offset)
		}
		if got := searchTestPageNames(p); got[0] != searchTestNames[0] {
			t.Errorf("first row = %q, want %q", got[0], searchTestNames[0])
		}
	})

	t.Run("limit 0 means DefaultPageSize", func(t *testing.T) {
		p := searchTestSearch(t, ix, Query{})
		if p.Query.Limit != DefaultPageSize {
			t.Errorf("Query.Limit = %d, want %d", p.Query.Limit, DefaultPageSize)
		}
	})

	t.Run("limit above MaxPageSize is clamped", func(t *testing.T) {
		big := newSearchIndex(searchTestBigCorpus(2000))
		p := searchTestSearch(t, big, Query{Limit: MaxPageSize * 10})
		if p.Query.Limit != MaxPageSize {
			t.Errorf("Query.Limit = %d, want %d", p.Query.Limit, MaxPageSize)
		}
		if len(p.Entries) != MaxPageSize {
			t.Errorf("returned %d rows, want %d", len(p.Entries), MaxPageSize)
		}
		if p.Total != 2000 {
			t.Errorf("Total = %d, want 2000", p.Total)
		}
	})
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

func TestSearchFilters(t *testing.T) {
	ix := searchTestIndex(t)

	cases := []struct {
		name     string
		q        Query
		want     []string
		wantSize int
	}{
		{
			name: "section tier",
			q:    Query{Category: SectionCategoryID("web")},
			want: []string{"chromium-browser", "firefox", "w3m"},
		},
		{
			name: "section tier strips a component prefix",
			q:    Query{Category: SectionCategoryID("libs")},
			want: []string{"libc6", "libstdc++6", "zlib1g"},
		},
		{
			name: "application tier",
			q:    Query{Category: AppCategoryID("Office")},
			want: []string{"gnome-etiquettes", "gnumeric", "libreoffice-calc"},
		},
		{
			name: "the two tiers do not collide on a shared word",
			q:    Query{Category: AppCategoryID("Graphics")},
			want: []string{"gimp"},
		},
		{
			name: "a subsidiary category is filterable even though it is not advertised",
			q:    Query{Category: AppCategoryID("TextEditor")},
			want: []string{"fooedit"},
		},
		{
			name: "apps only",
			q:    Query{AppsOnly: true},
			want: []string{"chromium-browser", "firefox", "fooedit", "gimp", "gnome-etiquettes", "gnumeric"},
		},
		{
			name: "apps only inside a category",
			q:    Query{AppsOnly: true, Category: AppCategoryID("Office")},
			want: []string{"gnome-etiquettes", "gnumeric"},
		},
		{
			name: "apps only combined with text",
			q:    Query{AppsOnly: true, Text: "browser"},
			want: []string{"chromium-browser", "firefox"},
		},
		{
			name: "category combined with text",
			q:    Query{Category: SectionCategoryID("editors"), Text: "vi"},
			// libreoffice-calc is last because "productivity" in its summary is
			// the only place it carries "vi" — a summary-only match, ranked
			// below every name match, which is the contract.
			want: []string{"vim", "vim-tiny", "vim-youcompleteme", "gvim", "libreoffice-calc"},
		},
		{
			name: "a well-formed category with no members is empty, not everything",
			q:    Query{Category: SectionCategoryID("nonexistent")},
			want: nil,
		},
		{
			name: "an application category with no members is empty too",
			q:    Query{Category: AppCategoryID("Nonexistent")},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.q
			q.Limit = MaxPageSize
			p := searchTestSearch(t, ix, q)
			got := searchTestPageNames(p)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if p.Total != len(tc.want) {
				t.Errorf("Total = %d, want %d", p.Total, len(tc.want))
			}
		})
	}
}

func TestSearchMalformedCategoryIsNoFilter(t *testing.T) {
	ix := searchTestIndex(t)
	// A stale deep link must show the catalogue, not a failure: that is what
	// ParseCategoryID's ok result is for.
	for _, id := range []string{"libs", "app:", "section:", "", "App:Graphics", "app :Graphics", "graphics:web"} {
		p := searchTestSearch(t, ix, Query{Category: id, Limit: MaxPageSize})
		if p.Total != len(searchTestNames) {
			t.Errorf("Category %q: Total = %d, want the whole catalogue (%d)", id, p.Total, len(searchTestNames))
		}
	}
}

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

func TestSearchCategories(t *testing.T) {
	ix := searchTestIndex(t)
	cats, err := ix.Categories(context.Background())
	if err != nil {
		t.Fatalf("Categories: %v", err)
	}

	want := []Category{
		{ID: "app:Graphics", Tier: TierApplication, Name: "Graphics", Count: 1},
		{ID: "app:Network", Tier: TierApplication, Name: "Network", Count: 2},
		{ID: "app:Office", Tier: TierApplication, Name: "Office", Count: 3},
		{ID: "section:editors", Tier: TierSection, Name: "editors", Count: 6},
		{ID: "section:gnome", Tier: TierSection, Name: "gnome", Count: 2},
		{ID: "section:graphics", Tier: TierSection, Name: "graphics", Count: 1},
		{ID: "section:libs", Tier: TierSection, Name: "libs", Count: 3},
		{ID: "section:misc", Tier: TierSection, Name: "misc", Count: 4},
		{ID: "section:utils", Tier: TierSection, Name: "utils", Count: 1},
		{ID: "section:web", Tier: TierSection, Name: "web", Count: 3},
	}
	if len(cats) != len(want) {
		t.Fatalf("Categories returned %d rows, want %d:\n%v", len(cats), len(want), cats)
	}
	for i := range want {
		if cats[i] != want[i] {
			t.Errorf("Categories[%d] = %+v, want %+v", i, cats[i], want[i])
		}
	}

	// The long tail of subsidiary freedesktop categories is deliberately not
	// advertised: docs/dev/index-formats.md §6.4 measured 122 of them.
	for _, c := range cats {
		if c.Tier == TierApplication && !searchMainCategorySet[c.Name] {
			t.Errorf("Categories advertised the subsidiary category %q", c.Name)
		}
	}

	// Every advertised count must equal what filtering on that id returns.
	for _, c := range cats {
		p := searchTestSearch(t, ix, Query{Category: c.ID, Limit: MaxPageSize})
		if p.Total != c.Count {
			t.Errorf("Category %q counts %d but filters to %d", c.ID, c.Count, p.Total)
		}
	}
}

func TestSearchCategoriesCopiesItsAnswer(t *testing.T) {
	ix := searchTestIndex(t)
	a, err := ix.Categories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a[0].Count = -1
	b, err := ix.Categories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b[0].Count == -1 {
		t.Error("Categories handed the caller a window onto the index's own state")
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestSearchGet(t *testing.T) {
	ix := searchTestIndex(t)
	for _, name := range searchTestNames {
		e, ok, err := ix.Get(context.Background(), name)
		if err != nil || !ok {
			t.Fatalf("Get(%q) = (_, %v, %v), want found", name, ok, err)
		}
		if e.Name != name {
			t.Errorf("Get(%q) returned %q", name, e.Name)
		}
	}
	for _, name := range []string{"", "vi", "VIM", "zzzz", "aa"} {
		if _, ok, err := ix.Get(context.Background(), name); ok || err != nil {
			t.Errorf("Get(%q) = (_, %v, %v), want (zero, false, nil)", name, ok, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Degenerate corpora
// ---------------------------------------------------------------------------

func TestSearchEmptyAndNilIndex(t *testing.T) {
	empty := newSearchIndex(nil)
	p := searchTestSearch(t, empty, Query{Text: "anything"})
	if p.Total != 0 || len(p.Entries) != 0 || p.Entries == nil {
		t.Errorf("empty index: %+v", p)
	}
	if cats, err := empty.Categories(context.Background()); err != nil || len(cats) != 0 {
		t.Errorf("empty index Categories = (%v, %v)", cats, err)
	}
	if _, ok, err := empty.Get(context.Background(), "x"); ok || err != nil {
		t.Errorf("empty index Get = (_, %v, %v)", ok, err)
	}
	if empty.len() != 0 || empty.memoryBytes() < 0 {
		t.Errorf("empty index len=%d bytes=%d", empty.len(), empty.memoryBytes())
	}

	var nilIx *searchIndex
	if _, err := nilIx.Search(context.Background(), Query{}); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("nil index Search = %v, want ErrNotBuilt", err)
	}
	if _, err := nilIx.Categories(context.Background()); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("nil index Categories = %v, want ErrNotBuilt", err)
	}
	if _, _, err := nilIx.Get(context.Background(), "x"); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("nil index Get = %v, want ErrNotBuilt", err)
	}
	if nilIx.len() != 0 || nilIx.memoryBytes() != 0 {
		t.Error("nil index must report an empty footprint rather than panicking")
	}
}

func TestSearchDuplicateNamesAreDeterministic(t *testing.T) {
	// docs/dev/index-formats.md §4.4: an archive really does ship the same
	// Package: name twice. Both rows exist; the order between them is the
	// order they arrived in, not a version comparison — nothing in this
	// repository may compare two version strings.
	corpus := []Entry{
		{Name: "dupe", Version: "2", Section: "misc", Summary: "second stanza"},
		{Name: "aaa", Section: "misc"},
		{Name: "dupe", Version: "1", Section: "misc", Summary: "first stanza"},
	}
	for i := 0; i < 5; i++ {
		ix := newSearchIndex(corpus)
		p := searchTestSearch(t, ix, Query{Text: "dupe"})
		if len(p.Entries) != 2 {
			t.Fatalf("got %d rows, want 2", len(p.Entries))
		}
		if p.Entries[0].Version != "2" || p.Entries[1].Version != "1" {
			t.Fatalf("duplicate order = %q,%q; want the input order 2,1",
				p.Entries[0].Version, p.Entries[1].Version)
		}
	}
}

// ---------------------------------------------------------------------------
// Folding
// ---------------------------------------------------------------------------

func TestSearchFold(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"libc6", "libc6"},
		{"LibC6", "libc6"},
		{"Générateur", "generateur"},
		{"Cafe\u0301", "cafe"},
		{"café", "cafe"},
		{"Größe", "grosse"},
		{"Æon Œuvre", "aeon oeuvre"},
		{"Łódź", "lodz"},
		{"ÅNGSTRÖM", "angstrom"},
		{"Ω", "ω"}, // outside Latin: lower-cased, otherwise left alone
	}
	for _, tc := range cases {
		if got := searchFold(tc.in); got != tc.want {
			t.Errorf("searchFold(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// An already-folded string must come back without an allocation, which is
	// what keeps 70,000 package names out of the heap at index time.
	s := "python3-async-client"
	if allocs := testing.AllocsPerRun(100, func() { _ = searchFold(s) }); allocs != 0 {
		t.Errorf("searchFold of an already-folded string allocated %v times", allocs)
	}
}

func TestSearchMatchKind(t *testing.T) {
	cases := []struct {
		hay, needle string
		want        int
	}{
		{"vim", "vim", searchKindExact},
		{"vim-tiny", "vim", searchKindPrefix},
		{"chromium-browser", "browser", searchKindWord},
		{"gvim", "vim", searchKindSub},
		{"firefox web browser", "browser", searchKindWord},
		{"libstdc++6", "c++", searchKindSub},
		{"", "vim", searchKindNone},
		{"vim", "emacs", searchKindNone},
		// The first occurrence is mid-word but a later one starts a word.
		{"unbrowsable web browser", "browser", searchKindWord},
		// Every occurrence is mid-word.
		{"unbrowsable rebrowser", "browser", searchKindSub},
	}
	for _, tc := range cases {
		if got := searchMatchKind(tc.hay, tc.needle); got != tc.want {
			t.Errorf("searchMatchKind(%q, %q) = %d, want %d", tc.hay, tc.needle, got, tc.want)
		}
	}
}

func TestSearchSection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"devel", "devel"},
		{"universe/devel", "devel"},
		{"multiverse/non-free/libs", "libs"},
		{" net ", "net"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := searchSection(tc.in); got != tc.want {
			t.Errorf("searchSection(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestSearchCancellation(t *testing.T) {
	ix := newSearchIndex(searchTestBigCorpus(5000))

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ix.Search(ctx, Query{Text: "pkg"}); !errors.Is(err, context.Canceled) {
			t.Errorf("Search = %v, want context.Canceled", err)
		}
		if _, err := ix.Categories(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Categories = %v, want context.Canceled", err)
		}
		if _, _, err := ix.Get(ctx, "pkg-00001"); !errors.Is(err, context.Canceled) {
			t.Errorf("Get = %v, want context.Canceled", err)
		}
	})

	t.Run("cancelled mid-scan", func(t *testing.T) {
		// The scan checks ctx every 1024 rows. This context answers "not
		// cancelled" for the entry check and the first in-loop check, then
		// reports cancellation — which lands the abort strictly inside the
		// scan rather than at the door.
		for _, q := range []Query{
			{Text: "pkg"}, // counting pass
			{Text: "pkg", AppsOnly: true, Category: "app:Demo"}, // apps-only browse pass
		} {
			ctx := &searchTestFlakyCtx{after: 2}
			if _, err := ix.Search(ctx, q); !errors.Is(err, context.Canceled) {
				t.Errorf("Search(%+v) = %v, want context.Canceled", q, err)
			}
			if ctx.calls < 3 {
				t.Errorf("Search(%+v) checked the context %d times; the abort was not inside the scan", q, ctx.calls)
			}
		}
	})

	t.Run("cancelled in the emit pass", func(t *testing.T) {
		// Far enough in that the first pass completes and the second one is
		// still running when the context goes.
		ctx := &searchTestFlakyCtx{after: 8}
		if _, err := ix.Search(ctx, Query{Text: "pkg", Offset: 4900}); !errors.Is(err, context.Canceled) {
			t.Errorf("Search = %v, want context.Canceled", err)
		}
	})
}

// searchTestFlakyCtx reports success for the first `after` calls to Err and
// cancellation from then on, which makes "cancelled while scanning" a
// deterministic test rather than a race with a timer.
type searchTestFlakyCtx struct {
	after int
	calls int
}

func (c *searchTestFlakyCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *searchTestFlakyCtx) Done() <-chan struct{}       { return nil }
func (c *searchTestFlakyCtx) Value(any) any               { return nil }
func (c *searchTestFlakyCtx) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestSearchConcurrentReaders is the assertion behind the index's concurrency
// guarantee: a searchIndex is immutable after construction, so any number of
// goroutines may query it at once without a lock. Run with -race.
func TestSearchConcurrentReaders(t *testing.T) {
	ix := newSearchIndex(searchTestBigCorpus(3000))
	texts := []string{"", "pkg", "pkg-001", "e", "demo", "zzzz", "widget"}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				q := Query{
					Text:     texts[(g+i)%len(texts)],
					Offset:   (g * i) % 500,
					Limit:    25,
					AppsOnly: (g+i)%3 == 0,
				}
				if (g+i)%4 == 0 {
					q.Category = SectionCategoryID("demo")
				}
				if _, err := ix.Search(context.Background(), q); err != nil {
					t.Errorf("Search: %v", err)
					return
				}
				if _, err := ix.Categories(context.Background()); err != nil {
					t.Errorf("Categories: %v", err)
					return
				}
				if _, _, err := ix.Get(context.Background(), fmt.Sprintf("pkg-%05d", i)); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Fixtures and the hook the external benchmark package needs
// ---------------------------------------------------------------------------

// searchTestBigCorpus generates n uniform entries. It exists for the paging,
// cancellation and concurrency tests, which care about size and not about
// prose; ranking is asserted against searchTestCorpus instead, and the
// benchmarks run against internal/catalog/fake.
func searchTestBigCorpus(n int) []Entry {
	out := make([]Entry, n)
	for i := range out {
		out[i] = Entry{
			Name:    fmt.Sprintf("pkg-%05d", i),
			Section: "demo",
			Summary: fmt.Sprintf("demo package number %d with a widget", i),
			Arch:    "amd64",
		}
		if i%50 == 0 {
			out[i].IsApp = true
			out[i].AppName = fmt.Sprintf("Demo Application %d", i)
			out[i].Categories = []string{"Utility", "Demo"}
		}
	}
	return out
}

// SearchIndexForBench exposes the index to the benchmark file.
//
// The benchmarks build their corpus with internal/catalog/fake, which imports
// this package — so a benchmark written in package catalog would be an import
// cycle and it has to live in package catalog_test instead. These two
// declarations are the standard export_test.go hook, are compiled only into
// the test binary, and are not part of the package's API.
type SearchIndexForBench = searchIndex

// NewSearchIndexForBench builds an index over entries, for the benchmark file.
func NewSearchIndexForBench(entries []Entry) *SearchIndexForBench { return newSearchIndex(entries) }

// MemoryBytesForBench is the index's own resident footprint, for the benchmark
// file's memory report.
func (ix *searchIndex) MemoryBytesForBench() int64 { return ix.memoryBytes() }
