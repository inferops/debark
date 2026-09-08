package catalog

// search.go is the in-memory index behind Catalog.Search, Catalog.Categories
// and Catalog.Get. It is deliberately self-contained: newSearchIndex takes a
// []Entry and everything else is a method, so the orchestration in catalog.go
// can hand it whatever the parser or the cache produced without reshaping it.
//
// # Why it looks like this
//
// The budget is keystroke -> rendered results in under 100 ms p95, *including*
// the Wails bridge round-trip, over roughly 70,000 entries, on every keystroke.
// That leaves the Go side a small fraction of 100 ms, which rules out three
// things that a naive implementation does per query:
//
//  1. Case folding. strings.ToLower over 70,000 names and summaries allocates
//     several megabytes per keystroke. Folding happens once, at index time.
//  2. Per-row allocation. Every haystack lives in one string (the arena) and a
//     query slices it; a Go string slice expression shares the backing array,
//     so matching allocates nothing at all.
//  3. Materialising the match set. "lib" matches roughly 32,000 rows in a real
//     Ubuntu main+universe index and the caller wants 50 of them. Collecting
//     32,000 indices to return 50 is 128 KB of garbage per keystroke, so the
//     search makes two passes instead: one that only counts (into a fixed
//     640-entry histogram on the stack) and one that writes exactly the
//     requested page. Allocation per query is O(Limit), never O(matches).
//
// A linear scan over a compact arena is enough at this size; §"Measured" in
// docs/performance.md carries the numbers. Nothing here builds a trigram index,
// a suffix array or an inverted index, because nothing here needs to.
//
// # Immutability is the concurrency story
//
// A searchIndex is fully built by newSearchIndex and never mutated afterwards.
// Every field is read-only from that moment, so any number of goroutines may
// call Search, Categories and Get concurrently without a lock. The UI queries
// from whichever goroutine the bridge hands it, and a build may be running at
// the same time; the way that is safe is that a rebuild produces a *new* index
// and the owner swaps the pointer, rather than mutating this one.

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// searchIndex is an immutable, queryable view of one catalogue.
//
// Positions: an "index position" is a row's place in name-ascending order, and
// is the unit every internal structure is keyed on — rows, the arena, the
// bitset, the posting lists. order maps an index position back to a position
// in the entries slice. Scanning in index-position order therefore walks the
// arena forwards, which is both cache-friendly and the reason the result of a
// scan is already in name order before any ranking is applied.
type searchIndex struct {
	// entries is the caller's slice, aliased rather than copied: a second copy
	// of 70,000 Entry values is 15 MB for no benefit. The index never writes
	// to it and neither may the caller after handing it over.
	entries []Entry

	// order[pos] is the position in entries of the row at index position pos.
	// Sorted by Name ascending, ties broken by original position so the order
	// is a deterministic function of the input.
	order []int32

	// arena holds every folded haystack — name, application name, summary —
	// back to back in index-position order. It is a string rather than a
	// []byte precisely so that slicing it is free.
	arena string
	rows  []searchRow

	// apps is a bitset over index positions: bit set means Entry.IsApp.
	// Consulted only when a query combines AppsOnly with a category filter;
	// AppsOnly on its own uses appRows directly.
	apps    []uint64
	appRows []int32

	// postings maps a Category.ID to every index position in that category,
	// ascending. Both tiers live in one map because the ids are already
	// tier-prefixed and cannot collide. A category filter therefore costs a
	// map lookup and a scan of the members, not a scan of the catalogue.
	postings map[string][]int32

	// cats is the precomputed Categories() answer, in contract order.
	cats []Category

	// bytes is the index's own resident footprint, excluding entries.
	bytes int64
}

// searchRow locates one row's three folded haystacks inside the arena.
// Sixteen bytes per row: 1.1 MB for 70,000 packages.
type searchRow struct {
	nameOff uint32
	appOff  uint32
	sumOff  uint32
	end     uint32
}

// The rank ladder. A row's rank is tier*searchLenBuckets + lengthBucket, a
// small integer, which is what lets the search count matches into a fixed
// histogram and emit them in rank order without ever sorting.
//
// Tiers interleave the package name and the DEP-11 application name because
// the contract ranks them together: someone typing "image manipulation" is
// naming gimp, not describing it. Within one kind of match the package name
// wins, so an exact name beats an exact application name, a name prefix beats
// an application-name prefix, and so on.
//
//	0  name == query                       ("vim" for vim)
//	1  application name == query           ("gimp" for gimp)
//	2  name starts with query              ("vim-youcompleteme" for vim)
//	3  application name starts with query
//	4  query starts a word inside the name  ("chromium-browser" for browser)
//	5  query starts a word inside the application name
//	6  query occurs anywhere in the name    ("zlib1g" for lib)
//	7  query occurs anywhere in the application name
//	8  query starts a word in the summary
//	9  query occurs anywhere in the summary
//
// Tiers 0-7 are the "name or application name matched" group the Catalog
// contract requires to come first; tiers 8-9 are the summary-only group.
const (
	searchTierNameExact = iota * searchLenBuckets
	searchTierAppExact
	searchTierNamePrefix
	searchTierAppPrefix
	searchTierNameWord
	searchTierAppWord
	searchTierNameSub
	searchTierAppSub
	searchTierSummaryWord
	searchTierSummarySub
	// searchRankCount is one past the last representable rank.
	searchRankCount
)

// searchLenBuckets is how many name-length buckets each tier is divided into.
// Within a tier a shorter package name sorts first, so typing "vi" puts vim
// above vim-youcompleteme; names of 63 bytes and longer share the last bucket,
// where alphabetical order takes over. Summary tiers always use bucket 0, so a
// summary-only match set is ordered purely by name — exactly what the Catalog
// contract promises for that group.
const searchLenBuckets = 64

// searchNoRank marks a row that did not match at all.
const searchNoRank = -1

// searchMaxArena is the largest folded-haystack arena a row's uint32 offsets
// may address, less headroom for the three fields of the row being written.
// Two gigabytes rather than four, so the bound holds on a 32-bit int as well;
// a real Ubuntu main+universe index folds to about 5 MB.
const searchMaxArena = (1 << 31) - (4 << 20)

// Match kinds, strongest first. searchMatchKind returns one of these.
const (
	searchKindExact = iota
	searchKindPrefix
	searchKindWord
	searchKindSub
	searchKindNone
)

// searchMainCategories are the thirteen freedesktop main categories, and they
// are the whole of the applications tier.
//
// docs/dev/index-formats.md §6.4 measured 135 distinct Categories strings in a
// real Ubuntu DEP-11 stream, of which 122 are a long tail down to a single
// package each (Productivity, Profiling, VideoConference). A sidebar built from
// the observed vocabulary would be a 135-row list in front of a 3%-coverage
// tier, which is worse than no tier at all. Grouping by the main categories
// covers essentially every desktop application — §6.4 found only five with no
// main category — and leaves the rest to be found by typing, which is what the
// search box is for.
//
// The subsidiary categories are still filterable: Query.Category accepts any
// well-formed id and the posting lists carry them all. They are simply not
// advertised.
var searchMainCategories = [...]string{
	"AudioVideo", "Audio", "Video", "Development", "Education", "Game",
	"Graphics", "Network", "Office", "Science", "Settings", "System", "Utility",
}

var searchMainCategorySet = func() map[string]bool {
	m := make(map[string]bool, len(searchMainCategories))
	for _, c := range searchMainCategories {
		m[c] = true
	}
	return m
}()

// newSearchIndex builds the index over entries.
//
// entries is aliased, not copied, and must not be mutated afterwards: the whole
// design assumes the corpus is frozen once the index exists. A rebuild makes a
// new index.
//
// Cost at 70,000 entries is one sort and one linear pass; it belongs in
// PhaseIndex of a build, between parsing and saving.
func newSearchIndex(entries []Entry) *searchIndex {
	n := len(entries)
	ix := &searchIndex{
		entries:  entries,
		order:    make([]int32, n),
		rows:     make([]searchRow, n),
		apps:     make([]uint64, (n+63)/64),
		postings: make(map[string][]int32),
	}
	for i := range ix.order {
		ix.order[i] = int32(i)
	}
	// Ties broken by original position rather than left to the sort, so the
	// order is a pure function of the input even when an archive ships the
	// same Package: name twice (docs/dev/index-formats.md §4.4).
	sort.Slice(ix.order, func(a, b int) bool {
		ea, eb := &entries[ix.order[a]], &entries[ix.order[b]]
		if ea.Name != eb.Name {
			return ea.Name < eb.Name
		}
		return ix.order[a] < ix.order[b]
	})

	// 80 bytes per row is close to the measured average of a folded name plus
	// an application name plus a one-line summary, so the arena grows a couple
	// of times at most.
	buf := make([]byte, 0, n*80)
	for pos, ei := range ix.order {
		e := &entries[ei]
		var r searchRow
		// The guard is what makes searchOffset safe to narrow. Two gigabytes of
		// folded names and summaries is some four hundred times a full Ubuntu
		// main+universe index, so the ceiling is not reachable in practice —
		// but a wrapped offset would return the wrong package for every query
		// after it, and a row that degrades to "no searchable text" is a far
		// better failure: it keeps its place in the name order, in Get and in
		// every category, and only stops matching typed text.
		if len(buf) <= searchMaxArena {
			r.nameOff = searchOffset(len(buf))
			buf = searchAppendFold(buf, e.Name)
			r.appOff = searchOffset(len(buf))
			buf = searchAppendFold(buf, e.AppName)
			r.sumOff = searchOffset(len(buf))
			buf = searchAppendFold(buf, e.Summary)
			r.end = searchOffset(len(buf))
		}
		ix.rows[pos] = r

		if e.IsApp {
			ix.apps[pos>>6] |= 1 << (pos & 63)
			ix.appRows = append(ix.appRows, int32(pos))
		}
		for _, c := range e.Categories {
			if c == "" {
				continue
			}
			// Guarded against a component that lists the same category twice:
			// a duplicated posting would show the row twice in a filtered page.
			id := AppCategoryID(c)
			p := ix.postings[id]
			if len(p) > 0 && p[len(p)-1] == int32(pos) {
				continue
			}
			ix.postings[id] = append(p, int32(pos))
		}
		if sec := searchSection(e.Section); sec != "" {
			id := SectionCategoryID(sec)
			ix.postings[id] = append(ix.postings[id], int32(pos))
		}
	}
	ix.arena = string(buf)

	// append over-allocates; the section postings are the big ones (every row
	// carries a section) and handing back half a megabyte of spare capacity for
	// the lifetime of the process is not worth the copy it saves.
	postingBytes := int64(0)
	for id, p := range ix.postings {
		if cap(p) > len(p)+len(p)/8 {
			q := make([]int32, len(p))
			copy(q, p)
			ix.postings[id] = q
			p = q
		}
		postingBytes += int64(len(id)) + int64(cap(p))*4 + 48
	}

	ix.cats = searchCategories(ix.postings)

	ix.bytes = int64(len(ix.arena)) +
		int64(len(ix.rows))*16 +
		int64(cap(ix.order))*4 +
		int64(cap(ix.apps))*8 +
		int64(cap(ix.appRows))*4 +
		postingBytes +
		int64(len(ix.cats))*int64(64)
	return ix
}

// len is the number of entries the index holds.
func (ix *searchIndex) len() int {
	if ix == nil {
		return 0
	}
	return len(ix.entries)
}

// memoryBytes is the index's own resident footprint in bytes, not counting the
// entries slice it was built over. Reported rather than estimated because the
// whole application has a 400 MB idle budget and this is the part of it this
// file is answerable for.
func (ix *searchIndex) memoryBytes() int64 {
	if ix == nil {
		return 0
	}
	return ix.bytes
}

// Search answers one query with one page.
//
// Ordering is the rank ladder above: name and application-name matches before
// summary-only matches, exact before prefix before word-start before plain
// substring, shorter names before longer ones, and Name ascending to break
// every remaining tie. That makes the order a deterministic function of the
// corpus and the query text alone, which is what stops the list reshuffling
// under the operator between keystrokes.
//
// This refines rather than contradicts the wording on Catalog.Search, which
// says rows are "ordered by Name ascending" within each group. The group
// boundary the contract fixes — name-or-application-name matches before
// summary-only matches — is exactly the boundary between tiers 0-7 and tiers
// 8-9, and it is honoured. Inside the first group the order is by rank and then
// by name rather than by name alone, because plain alphabetical order buries
// vim under vim-youcompleteme, and the operator this product exists for does
// not know either name. Summary-only matches are ordered by name and nothing
// else, which is the contract's wording verbatim. Anyone tightening iface.go's
// comment should say "by relevance, then by Name ascending".
//
// An empty (or whitespace-only) Text is a browse, not an error: the whole
// catalogue in name order, paged.
//
// Page.Total is the full match count, never the page length, because the
// virtualiser sizes its scrollbar from it without receiving the rows.
func (ix *searchIndex) Search(ctx context.Context, q Query) (Page, error) {
	start := time.Now()
	q = q.Normalized()
	if ix == nil {
		return Page{}, ErrNotBuilt
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}

	cand, all, appsFilter := ix.searchScope(q)
	count := len(ix.rows)
	if !all {
		count = len(cand)
	}

	text := searchFold(q.Text)
	if text == "" {
		return ix.searchBrowse(ctx, q, cand, all, appsFilter, count, start)
	}

	// Pass one: count only. A fixed 640-entry histogram on the stack replaces
	// the slice of every match a one-pass implementation would need.
	var hist [searchRankCount]int
	total := 0
	for k := 0; k < count; k++ {
		if k&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return Page{}, err
			}
		}
		i := k
		if !all {
			i = int(cand[k])
		}
		if appsFilter && !ix.searchIsApp(i) {
			continue
		}
		if r := ix.searchRank(i, text); r != searchNoRank {
			hist[r]++
			total++
		}
	}

	want := 0
	if q.Offset < total {
		want = total - q.Offset
		if want > q.Limit {
			want = q.Limit
		}
	}
	out := make([]Entry, want)
	if want > 0 {
		// base[r] is the position the next row of rank r takes in the result.
		// Because pass two walks the same candidates in the same order, and
		// because rows are visited in name order, this reproduces "sort by
		// rank, then by name" exactly — as a counting sort, in one pass, with
		// only the requested window ever written.
		var base [searchRankCount]int
		acc := 0
		for r := 0; r < searchRankCount; r++ {
			base[r] = acc
			acc += hist[r]
		}
		lo, hi := q.Offset, q.Offset+want
		filled := 0
		for k := 0; k < count && filled < want; k++ {
			if k&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return Page{}, err
				}
			}
			i := k
			if !all {
				i = int(cand[k])
			}
			if appsFilter && !ix.searchIsApp(i) {
				continue
			}
			r := ix.searchRank(i, text)
			if r == searchNoRank {
				continue
			}
			pos := base[r]
			base[r]++
			if pos >= lo && pos < hi {
				out[pos-lo] = ix.entries[ix.order[i]]
				filled++
			}
		}
	}

	return Page{
		Entries: out,
		Total:   total,
		Offset:  q.Offset,
		Query:   q,
		Elapsed: time.Since(start),
	}, nil
}

// Categories returns the two-tier grouping, precomputed at index time.
//
// Applications first, then sections, each by name ascending — the order the
// Catalog contract fixes, so the sidebar does not reshuffle between calls.
// Counts are over the whole catalogue and are exactly what a filter on that id
// would return.
func (ix *searchIndex) Categories(ctx context.Context) ([]Category, error) {
	if ix == nil {
		return nil, ErrNotBuilt
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Category, len(ix.cats))
	copy(out, ix.cats)
	return out, nil
}

// Get returns one entry by exact binary package name.
//
// A binary search over the name order rather than a map: 70,000 map keys is
// about 4 MB of resident memory to answer a lookup the operator makes once per
// click, against a handful of comparisons that cost nothing.
func (ix *searchIndex) Get(ctx context.Context, name string) (Entry, bool, error) {
	if ix == nil {
		return Entry{}, false, ErrNotBuilt
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	k := sort.Search(len(ix.order), func(j int) bool {
		return ix.entries[ix.order[j]].Name >= name
	})
	if k < len(ix.order) {
		if e := &ix.entries[ix.order[k]]; e.Name == name {
			return entryWithDescription(*e), true, nil
		}
	}
	return Entry{}, false, nil
}

// searchScope resolves the query's filters into the set of index positions
// worth looking at.
//
// all is true when that set is every row, in which case cand is nil and the
// scan runs over positions directly; this is the common case and it avoids
// materialising a 280 KB candidate list per keystroke. appsFilter reports
// whether the IsApp bitset still has to be consulted per row, which is only
// when AppsOnly is combined with a category filter.
//
// A malformed Category is not an error: a stale deep link shows the catalogue
// rather than a failure, which is what ParseCategoryID's ok result is for. A
// well-formed id with no members is a different thing entirely and yields an
// empty candidate set, so the page comes back with Total 0.
func (ix *searchIndex) searchScope(q Query) (cand []int32, all bool, appsFilter bool) {
	if _, _, ok := ParseCategoryID(q.Category); ok {
		return ix.postings[q.Category], false, q.AppsOnly
	}
	if q.AppsOnly {
		return ix.appRows, false, false
	}
	return nil, true, false
}

// searchBrowse answers a query with no text. The candidate set is already in
// name order, so the page is a window onto it and nothing needs ranking.
func (ix *searchIndex) searchBrowse(ctx context.Context, q Query, cand []int32, all, appsFilter bool, count int, start time.Time) (Page, error) {
	if !appsFilter {
		// O(Limit): the picker's opening state does not touch 70,000 rows.
		total := count
		want := 0
		if q.Offset < total {
			want = total - q.Offset
			if want > q.Limit {
				want = q.Limit
			}
		}
		out := make([]Entry, want)
		for j := 0; j < want; j++ {
			i := q.Offset + j
			if !all {
				i = int(cand[q.Offset+j])
			}
			out[j] = ix.entries[ix.order[i]]
		}
		return Page{Entries: out, Total: total, Offset: q.Offset, Query: q, Elapsed: time.Since(start)}, nil
	}

	// AppsOnly inside a category: the members have to be counted, but there
	// are at most a few hundred of them.
	total := 0
	out := make([]Entry, 0, q.Limit)
	for k := 0; k < count; k++ {
		if k&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return Page{}, err
			}
		}
		i := int(cand[k])
		if !ix.searchIsApp(i) {
			continue
		}
		if total >= q.Offset && len(out) < q.Limit {
			out = append(out, ix.entries[ix.order[i]])
		}
		total++
	}
	return Page{Entries: out, Total: total, Offset: q.Offset, Query: q, Elapsed: time.Since(start)}, nil
}

// searchIsApp reports whether the row at index position i is a DEP-11 desktop
// application.
func (ix *searchIndex) searchIsApp(i int) bool {
	return ix.apps[i>>6]&(1<<(i&63)) != 0
}

// searchRank scores one row against an already-folded query, or returns
// searchNoRank.
//
// The order of the tests is the optimisation: an exact name match cannot be
// beaten, so nothing else is examined; the summary — the longest haystack, and
// the one most rows have — is only scanned when neither name matched at all.
func (ix *searchIndex) searchRank(i int, text string) int {
	r := &ix.rows[i]
	name := ix.arena[r.nameOff:r.appOff]

	tier := -1
	switch searchMatchKind(name, text) {
	case searchKindExact:
		return searchTierNameExact + searchLenBucket(int(r.appOff-r.nameOff))
	case searchKindPrefix:
		tier = searchTierNamePrefix
	case searchKindWord:
		tier = searchTierNameWord
	case searchKindSub:
		tier = searchTierNameSub
	}

	if r.sumOff > r.appOff {
		app := ix.arena[r.appOff:r.sumOff]
		t := -1
		switch searchMatchKind(app, text) {
		case searchKindExact:
			t = searchTierAppExact
		case searchKindPrefix:
			t = searchTierAppPrefix
		case searchKindWord:
			t = searchTierAppWord
		case searchKindSub:
			t = searchTierAppSub
		}
		if t >= 0 && (tier < 0 || t < tier) {
			tier = t
		}
	}
	if tier >= 0 {
		return tier + searchLenBucket(int(r.appOff-r.nameOff))
	}

	sum := ix.arena[r.sumOff:r.end]
	switch searchMatchKind(sum, text) {
	case searchKindExact, searchKindPrefix, searchKindWord:
		return searchTierSummaryWord
	case searchKindSub:
		return searchTierSummarySub
	}
	return searchNoRank
}

// searchLenBucket maps a package name's length onto its slot within a tier.
func searchLenBucket(n int) int {
	if n >= searchLenBuckets {
		return searchLenBuckets - 1
	}
	return n
}

// searchOffset narrows an arena length to the uint32 a searchRow stores it as.
// Every call site has checked searchMaxArena first, which is what makes the
// conversion safe; see newSearchIndex.
func searchOffset(n int) uint32 {
	return uint32(n) //nolint:gosec // bounded by searchMaxArena at every call site
}

// searchMatchKind classifies how needle occurs in hay. Both are already
// folded, and needle is never empty.
//
// The needle is matched literally, byte for byte: this is strings.Index and
// not a regexp, so "c++" and "g++" are searches for those packages rather than
// pattern errors, and "." matches a dot.
func searchMatchKind(hay, needle string) int {
	i := strings.Index(hay, needle)
	if i < 0 {
		return searchKindNone
	}
	if i == 0 {
		if len(hay) == len(needle) {
			return searchKindExact
		}
		return searchKindPrefix
	}
	// Not at the start: look for an occurrence that begins a word, so that
	// "browser" finds chromium-browser ahead of a match buried mid-token.
	for {
		if !searchIsWordByte(hay[i-1]) {
			return searchKindWord
		}
		j := strings.Index(hay[i+1:], needle)
		if j < 0 {
			return searchKindSub
		}
		i += 1 + j
	}
}

// searchIsWordByte reports whether c continues a word. Everything that is not
// an ASCII letter or digit separates one — "-", ".", "_", "+", "/", a space —
// which is exactly the punctuation Debian package names and DEP-11 application
// names are built out of. Bytes above ASCII are treated as word bytes so that
// a multi-byte rune is never mistaken for a separator.
func searchIsWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= utf8.RuneSelf
}

// searchSection normalises an apt Section: value for grouping.
//
// Entry.Section is documented as already stripped of the archive's "component/"
// prefix, but docs/dev/index-formats.md §4.5 measured 99 distinct Section
// values across Ubuntu main+universe collapsing to about 58 real ones, and a
// prefix that slipped through would put "devel" and "universe/devel" in the
// sidebar as two unrelated categories. Stripping here as well costs one byte
// scan per row at index time and makes the grouping independent of the parser.
func searchSection(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// searchCategories turns the posting lists into the sidebar's two tiers.
func searchCategories(postings map[string][]int32) []Category {
	out := make([]Category, 0, len(postings))
	for id, p := range postings {
		tier, name, ok := ParseCategoryID(id)
		if !ok || len(p) == 0 {
			continue
		}
		if tier == TierApplication && !searchMainCategorySet[name] {
			continue
		}
		out = append(out, Category{ID: id, Tier: tier, Name: name, Count: len(p)})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Tier != out[b].Tier {
			return out[a].Tier == TierApplication
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// searchFold reduces s to the form the index matches on: lower case, with
// Latin accents removed.
//
// Accent folding matters because DEP-11 summaries and application names are
// prose a human wrote — "Générateur", "Aplicación", "Größe" — and an operator
// typing ASCII must still find them. It is done here, once per string at index
// time, rather than per row per keystroke.
//
// A string that is already folded is returned unchanged, which is the case for
// essentially every Debian package name and costs no allocation.
func searchFold(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || (c >= 'A' && c <= 'Z') {
			return string(searchAppendFold(make([]byte, 0, len(s)+8), s))
		}
	}
	return s
}

// searchAppendFold appends the folded form of s to dst. Appending rather than
// returning a string is what lets the whole arena be built with one buffer and
// no per-row garbage.
func searchAppendFold(dst []byte, s string) []byte {
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			break
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	if i == len(s) {
		return dst
	}
	for _, r := range s[i:] {
		if r < utf8.RuneSelf {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			dst = utf8.AppendRune(dst, r)
			continue
		}
		r = unicode.ToLower(r)
		// Combining marks are dropped, which folds a decomposed "e" plus
		// U+0301 to the same "e" the precomposed form folds to.
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		if rep, ok := searchFoldRunes[r]; ok {
			dst = append(dst, rep...)
			continue
		}
		dst = utf8.AppendRune(dst, r)
	}
	return dst
}

// searchFoldRunes maps precomposed Latin letters to their ASCII base, keyed on
// the lower-case form because searchAppendFold lower-cases first.
//
// Latin-1 Supplement and Latin Extended-A only. That covers the languages
// DEP-11 metadata is actually written in; anything outside it is left alone
// and still matches when typed verbatim. Doing this without a table would mean
// golang.org/x/text, and this repository hand-reviews every dependency.
var searchFoldRunes = func() map[rune]string {
	m := make(map[rune]string, 192)
	add := func(rep, runes string) {
		for _, r := range runes {
			m[r] = rep
		}
	}
	add("a", "àáâãäåāăą")
	add("ae", "æ")
	add("c", "çćĉċč")
	add("d", "ďđð")
	add("e", "èéêëēĕėęě")
	add("g", "ĝğġģ")
	add("h", "ĥħ")
	add("i", "ìíîïĩīĭįı")
	add("ij", "ĳ")
	add("j", "ĵ")
	add("k", "ķĸ")
	add("l", "ĺļľŀł")
	add("n", "ñńņňŉŋ")
	add("o", "òóôõöøōŏő")
	add("oe", "œ")
	add("r", "ŕŗř")
	add("s", "śŝşšſ")
	add("ss", "ß")
	add("t", "ţťŧ")
	add("th", "þ")
	add("u", "ùúûüũūŭůűų")
	add("w", "ŵ")
	add("y", "ýÿŷ")
	add("z", "źżž")
	return m
}()
