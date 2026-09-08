// Package fake is an in-memory catalog.Catalog for building and testing the
// user interface against.
//
// It exists because the frontend is on the critical path and the real
// catalogue is not: five packages build screens against this package before any
// `Packages` file has been parsed, and they must not have to rework those
// screens afterwards. So this is not a stub. It holds a realistic corpus,
// searches it the way the contract says the real one will, ranks the way the
// real one will, pages the way the real one will, and can be scaled to the
// seventy thousand entries the budgets are written against.
//
// # Nothing here touches the network or the disk
//
// No fetch, no cache file, no temporary directory. A test may run a thousand
// builds and leave nothing behind.
//
// # Exercising the states the UI actually has to render
//
// A UI built only against the happy path renders the happy path. Every
// interesting state is therefore injectable, through Options:
//
//	f := fake.New(70000)                     // full-size corpus
//	f.SetOptions(fake.Options{
//	    BuildDelay:  8 * time.Second,        // a build worth showing a bar for
//	    BuildFailAt: 0.4,                    // ...that fails 40% of the way in
//	})
//	f.SetReady(false)                        // no cache: Search returns ErrNotBuilt
//
// The knobs, in one place:
//
//   - Options.BuildDelay      how long a Build takes, spread over its phases.
//   - Options.BuildFailAt     fraction of the way through Build at which it
//     fails; 0 disables. Use with BuildErr to choose the
//     error, or take the default one.
//   - Options.BuildErr        the error BuildFailAt returns.
//   - Options.ParseTicks      how many progress callbacks the parse phase
//     emits, for testing a smooth bar against a jumpy one.
//   - Options.SearchDelay     makes every Search slow, for testing the
//     debounce and the "searching..." state.
//   - Options.SearchErr       makes every Search fail.
//   - Options.CategoriesErr   makes Categories fail, for the sidebar's error
//     state.
//   - Options.GetErr          makes Get fail.
//   - Options.ReadyErr        makes Ready fail — an unreadable cache
//     directory, which is a real thing that happens.
//   - Options.CloseErr        makes Close fail.
//   - Options.Stale           makes Stale() report true, which is the picker's
//     "this catalogue is out of date / Refresh" banner.
//   - SetReady(false)         makes Ready report false and Search return
//     catalog.ErrNotBuilt, which is the state a target
//     with no cache is in. A successful Build flips it
//     back to true.
//
// Options are read at the start of each call, so they may be changed while the
// UI is running — a dev-only "make the next build fail" menu item works.
//
// # Determinism
//
// Two Fakes built with the same n hold byte-identical corpora, and an entry's
// attributes depend only on its own name (see corpus.go). Golden tests are
// therefore safe, and a bug reproduced against fake.New(70000) reproduces on
// another machine.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/inferops/debark/gui/internal/catalog"
)

// Fake implements catalog.Catalog over an in-memory corpus. Safe for
// concurrent use, because the thing it stands in for has to be: the bridge
// calls Search from one goroutine while a Build runs on another.
type Fake struct {
	// Everything below the mutex is either immutable after New (the corpus
	// and its derived indexes) or mutable state guarded by mu. The corpus is
	// never mutated, so Search may read it after releasing the lock.
	mu sync.RWMutex

	entries []catalog.Entry
	byName  map[string]int
	// lowerName and lowerSummary are the search haystacks, lower-cased once
	// at construction rather than on every keystroke. The real
	// implementation does the same thing on disk, for the same reason: 70,000
	// strings.ToLower calls per keystroke is the difference between meeting
	// the 100 ms budget and not.
	lowerName    []string
	lowerSummary []string
	categories   []catalog.Category

	opts       Options
	ready      bool
	building   bool
	closed     bool
	buildCount int
	lastTarget catalog.Target
	lastBuilt  time.Time
}

// Options are the fake's injectable behaviours. The zero value is the happy
// path: instant builds, instant searches, no failures.
type Options struct {
	// BuildDelay is how long a Build takes in total, spread evenly across its
	// progress callbacks. Zero means instantaneous, which is what tests want
	// and what a progress view cannot be developed against.
	BuildDelay time.Duration

	// BuildFailAt is the fraction of the way through a build, in (0,1], at
	// which Build gives up and returns BuildErr. Zero disables it.
	//
	// A fraction rather than a phase name because the interesting question
	// for the UI is not "which phase failed" but "what does a bar that stops
	// at 40% and turns into an error look like" — and because a fraction
	// keeps working if the phase list ever changes.
	BuildFailAt float64
	// BuildErr is the error a BuildFailAt build returns. When nil, a
	// descriptive default is used that reads like a real download failure.
	BuildErr error

	// ParseTicks is how many progress callbacks the parse phase emits.
	// Defaults to defaultParseTicks. Set it to 1 to see how the UI copes with
	// a phase that reports nothing until it finishes, or to 500 to check the
	// event path is not doing per-callback layout work.
	ParseTicks int

	// SearchDelay is added to every Search. The debounce, the pending-state
	// spinner and the out-of-order-response handling in picker-search all
	// need a Search slow enough to race.
	SearchDelay time.Duration
	// SearchErr, when set, is returned by every Search.
	SearchErr error
	// CategoriesErr, when set, is returned by every Categories call.
	CategoriesErr error
	// GetErr, when set, is returned by every Get.
	GetErr error
	// ReadyErr, when set, is returned by every Ready call. Models an
	// unreadable cache directory, which is what a wrong HOME or a full disk
	// looks like from here.
	ReadyErr error
	// CloseErr, when set, is returned by Close.
	CloseErr error
	// Stale makes the catalogue report itself as built from indexes the
	// archive has since republished, which is the state the picker's "this
	// catalogue is out of date / Refresh" banner renders. A Fake holds no
	// cache and no digest, so this is the only way to reach that state
	// without one.
	Stale bool
}

const defaultParseTicks = 12

// errBuildFailed is the default failure BuildFailAt produces. It is written
// the way a real one should read: what was being done, and what to do about
// it. If the UI renders this badly, it will render the real one badly too.
var errBuildFailed = errors.New("downloading package lists: archive.ubuntu.com: connection reset by peer — check the network and try again")

var _ catalog.Catalog = (*Fake)(nil)

// New returns a Fake holding n entries. n is clamped to the curated table's
// size at the bottom and MaxEntryCount at the top; pass DefaultEntryCount for
// a corpus that starts instantly, or 70000 to reproduce Ubuntu main +
// universe.
//
// The Fake starts ready, so a frontend that only wants a picker to render
// against needs exactly one line and no build. Call SetReady(false) to get the
// other state.
func New(n int) *Fake {
	entries := buildCorpus(n)
	f := &Fake{
		entries:      entries,
		byName:       make(map[string]int, len(entries)),
		lowerName:    make([]string, len(entries)),
		lowerSummary: make([]string, len(entries)),
		ready:        true,
		lastBuilt:    time.Now(),
	}
	for i, e := range entries {
		f.byName[e.Name] = i
		// AppName is folded into the name haystack rather than the summary
		// one: "GNU Image Manipulation Program" is the human name of gimp,
		// and an operator typing it is naming the package, not describing it.
		n := e.Name
		if e.AppName != "" {
			n += " " + e.AppName
		}
		f.lowerName[i] = strings.ToLower(n)
		f.lowerSummary[i] = strings.ToLower(e.Summary)
	}
	f.categories = computeCategories(entries)
	return f
}

// NewDefault returns New(DefaultEntryCount). The one-liner for a test or a
// `wails dev` session with no opinion about size.
func NewDefault() *Fake { return New(DefaultEntryCount) }

// SetOptions replaces the injectable behaviours. Safe to call at any time,
// including while a build is running — the running build keeps the options it
// started with.
func (f *Fake) SetOptions(o Options) {
	f.mu.Lock()
	f.opts = o
	f.mu.Unlock()
}

// Options returns the current options.
func (f *Fake) Options() Options {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.opts
}

// SetReady forces what Ready reports. false is the "no cache for this target"
// state: Ready returns false and Search, Categories and Get return
// catalog.ErrNotBuilt. A successful Build sets it back to true.
func (f *Fake) SetReady(ready bool) {
	f.mu.Lock()
	f.ready = ready
	f.mu.Unlock()
}

// Len is the number of entries in the corpus. For tests and for a dev banner
// that wants to say what it is looking at.
func (f *Fake) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.entries)
}

// PackageCount is how many rows the catalogue holds, and 0 when it is not
// ready — the same contract the real implementation answers on the warm path.
//
// Separate from Len, which is the corpus this Fake was constructed with
// whatever state it is in. A test that flips SetReady(false) is describing a
// target with no cache, and a catalogue that reported 70,000 packages in that
// state would let a "package_count is wired" assertion pass over a screen that
// still shows the previous target's number.
func (f *Fake) PackageCount() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.ready {
		return 0
	}
	return len(f.entries)
}

// Stale is the optional half of the Catalog contract. A Fake holds no cache
// and no index digest, so it is never stale unless Options.Stale says so —
// which is how a screen's "this catalogue is out of date" banner gets
// developed against something.
func (f *Fake) Stale() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ready && f.opts.Stale
}

// BuiltAt is when the catalogue became ready, and the zero time when it is
// not.
func (f *Fake) BuiltAt() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.ready {
		return time.Time{}
	}
	return f.lastBuilt
}

// BuildCount is how many times Build has been called, successfully or not.
// Lets a test assert that the UI did not build the catalogue twice.
func (f *Fake) BuildCount() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.buildCount
}

// LastBuilt is when the corpus last became ready — construction time, or the
// end of the most recent successful Build.
func (f *Fake) LastBuilt() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastBuilt
}

// LastTarget is the target of the most recent Build call. Lets a test assert
// that the target screen passed the target the operator actually chose.
func (f *Fake) LastTarget() catalog.Target {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastTarget
}

// Build simulates a multi-phase catalogue build.
//
// It walks catalog.Phases, emitting believable progress for each: one callback
// per suite in the release phase, one per index file in the download phase
// with running byte counters, ParseTicks callbacks in the parse phase, and a
// terminal PhaseDone. The whole run takes Options.BuildDelay, divided evenly
// across the callbacks, and checks ctx before every one — so cancelling
// halfway really does stop halfway.
func (f *Fake) Build(ctx context.Context, t catalog.Target, progress func(catalog.Progress)) error {
	f.mu.Lock()
	switch {
	case f.closed:
		f.mu.Unlock()
		return catalog.ErrClosed
	case f.building:
		f.mu.Unlock()
		return catalog.ErrBuildInProgress
	}
	if t.DistroID == "" && t.BaseID == "" && t.SnapshotPath == "" {
		f.mu.Unlock()
		return errors.New("fake: Build called with an empty target — the target screen must pass the target the operator chose")
	}
	opts := f.opts
	f.building = true
	f.buildCount++
	f.lastTarget = t
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.building = false
		f.mu.Unlock()
	}()

	start := time.Now()
	ticks := planBuild(t, opts)
	perTick := time.Duration(0)
	if opts.BuildDelay > 0 && len(ticks) > 0 {
		perTick = opts.BuildDelay / time.Duration(len(ticks))
	}
	failAt := -1
	if opts.BuildFailAt > 0 {
		failAt = int(opts.BuildFailAt * float64(len(ticks)))
		if failAt >= len(ticks) {
			failAt = len(ticks) - 1
		}
	}

	for i, p := range ticks {
		if err := sleepCtx(ctx, perTick); err != nil {
			return fmt.Errorf("fake: catalogue build cancelled: %w", err)
		}
		if failAt >= 0 && i >= failAt {
			err := opts.BuildErr
			if err == nil {
				err = errBuildFailed
			}
			return fmt.Errorf("fake: catalogue build failed: %w", err)
		}
		if progress != nil {
			p.Elapsed = time.Since(start)
			progress(p)
		}
	}

	f.mu.Lock()
	f.ready = true
	f.lastBuilt = time.Now()
	f.mu.Unlock()
	return nil
}

// Ready reports whether a catalogue is available. The target argument is
// recorded but not otherwise used: one Fake stands for one catalogue, and
// pretending it holds a cache per target would be modelling the cache rather
// than the interface.
func (f *Fake) Ready(t catalog.Target) (bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return false, catalog.ErrClosed
	}
	if f.opts.ReadyErr != nil {
		return false, f.opts.ReadyErr
	}
	return f.ready, nil
}

// Search returns one page, ranked and paged exactly as the contract
// describes: case-insensitive substring over the name (including the DEP-11
// application name) and the summary, name matches first, name ascending within
// each group.
func (f *Fake) Search(ctx context.Context, q catalog.Query) (catalog.Page, error) {
	start := time.Now()

	f.mu.RLock()
	closed, ready, opts := f.closed, f.ready, f.opts
	entries, lname, lsum := f.entries, f.lowerName, f.lowerSummary
	f.mu.RUnlock()

	switch {
	case closed:
		return catalog.Page{}, catalog.ErrClosed
	case !ready:
		return catalog.Page{}, catalog.ErrNotBuilt
	}
	if err := sleepCtx(ctx, opts.SearchDelay); err != nil {
		return catalog.Page{}, err
	}
	if opts.SearchErr != nil {
		return catalog.Page{}, opts.SearchErr
	}
	if err := ctx.Err(); err != nil {
		return catalog.Page{}, err
	}

	q = q.Normalized()
	text := strings.ToLower(q.Text)
	tier, catName, hasCat := catalog.ParseCategoryID(q.Category)

	// Two ordered buckets rather than one scored list. The corpus is already
	// sorted by name, so appending in order gives "name matches first, then
	// summary matches, each by name ascending" with no sort at query time —
	// which is the same trick the real implementation has to use to stay
	// inside the budget.
	nameHits := make([]int, 0, 64)
	sumHits := make([]int, 0, 64)
	for i := range entries {
		e := &entries[i]
		if q.AppsOnly && !e.IsApp {
			continue
		}
		if hasCat && !inCategory(e, tier, catName) {
			continue
		}
		switch {
		case text == "":
			nameHits = append(nameHits, i)
		case strings.Contains(lname[i], text):
			nameHits = append(nameHits, i)
		case strings.Contains(lsum[i], text):
			sumHits = append(sumHits, i)
		}
	}

	total := len(nameHits) + len(sumHits)
	rows := make([]catalog.Entry, 0, q.Limit)
	for n := 0; n < q.Limit; n++ {
		idx := q.Offset + n
		if idx >= total {
			break
		}
		var e catalog.Entry
		if idx < len(nameHits) {
			e = entries[nameHits[idx]]
		} else {
			e = entries[sumHits[idx-len(nameHits)]]
		}
		rows = append(rows, e)
	}

	return catalog.Page{
		Entries: rows,
		Total:   total,
		Offset:  q.Offset,
		Query:   q,
		Elapsed: time.Since(start),
	}, nil
}

// Categories returns the two-tier grouping, counted over the whole corpus.
func (f *Fake) Categories(ctx context.Context) ([]catalog.Category, error) {
	f.mu.RLock()
	closed, ready, opts, cats := f.closed, f.ready, f.opts, f.categories
	f.mu.RUnlock()

	switch {
	case closed:
		return nil, catalog.ErrClosed
	case !ready:
		return nil, catalog.ErrNotBuilt
	case opts.CategoriesErr != nil:
		return nil, opts.CategoriesErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Copied, not aliased: a caller handed the slice must not be a window
	// onto the fake's own state.
	out := make([]catalog.Category, len(cats))
	copy(out, cats)
	return out, nil
}

// Get returns one entry by exact name.
func (f *Fake) Get(ctx context.Context, name string) (catalog.Entry, bool, error) {
	f.mu.RLock()
	closed, ready, opts := f.closed, f.ready, f.opts
	entries, byName := f.entries, f.byName
	f.mu.RUnlock()

	switch {
	case closed:
		return catalog.Entry{}, false, catalog.ErrClosed
	case !ready:
		return catalog.Entry{}, false, catalog.ErrNotBuilt
	case opts.GetErr != nil:
		return catalog.Entry{}, false, opts.GetErr
	}
	if err := ctx.Err(); err != nil {
		return catalog.Entry{}, false, err
	}
	i, ok := byName[name]
	if !ok {
		return catalog.Entry{}, false, nil
	}
	return entries[i], true, nil
}

// Close makes every subsequent call return catalog.ErrClosed. Idempotent: a
// second Close is not an error, because a UI that closes on both navigation
// and shutdown will call it twice.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.opts.CloseErr
}

// inCategory reports whether e belongs to the given category. Applications
// match on their freedesktop categories, everything else on its apt Section.
func inCategory(e *catalog.Entry, tier catalog.CategoryTier, name string) bool {
	switch tier {
	case catalog.TierApplication:
		for _, c := range e.Categories {
			if c == name {
				return true
			}
		}
		return false
	case catalog.TierSection:
		return e.Section == name
	default:
		return true
	}
}

// computeCategories counts both tiers over the corpus, once. Applications
// first, then sections, each by name — the order the contract promises, so the
// sidebar never reshuffles.
func computeCategories(entries []catalog.Entry) []catalog.Category {
	apps := map[string]int{}
	sections := map[string]int{}
	for i := range entries {
		e := &entries[i]
		for _, c := range e.Categories {
			apps[c]++
		}
		if e.Section != "" {
			sections[e.Section]++
		}
	}
	out := make([]catalog.Category, 0, len(apps)+len(sections))
	for name, n := range apps {
		out = append(out, catalog.Category{
			ID:    catalog.AppCategoryID(name),
			Tier:  catalog.TierApplication,
			Name:  name,
			Count: n,
		})
	}
	for name, n := range sections {
		out = append(out, catalog.Category{
			ID:    catalog.SectionCategoryID(name),
			Tier:  catalog.TierSection,
			Name:  name,
			Count: n,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier == catalog.TierApplication
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// planBuild lays out every progress callback a build will make, before it
// makes any of them. Computing the whole plan up front is what lets the delay
// be divided evenly and the failure point be expressed as a fraction, and it
// means the callbacks a test sees are a pure function of the target and the
// options.
func planBuild(t catalog.Target, opts Options) []catalog.Progress {
	phaseCount := len(catalog.Phases)
	idx := func(p catalog.Phase) int { return p.Index() }

	suites := t.Suites()
	if len(suites) == 0 {
		suites = []string{"noble", "noble-updates", "noble-security"}
	}
	items := indexItems(t)

	var out []catalog.Progress

	out = append(out, catalog.Progress{
		Phase: catalog.PhaseResolve, PhaseIndex: idx(catalog.PhaseResolve), PhaseCount: phaseCount,
		Label:   fmt.Sprintf("Working out which package lists %s needs", t.Display()),
		Current: 1, Total: 1,
	})

	for i, s := range suites {
		out = append(out, catalog.Progress{
			Phase: catalog.PhaseRelease, PhaseIndex: idx(catalog.PhaseRelease), PhaseCount: phaseCount,
			Label: "Checking the archive", Item: "dists/" + s + "/Release",
			Current: int64(i + 1), Total: int64(len(suites)),
		})
	}

	var totalBytes int64
	sizes := make([]int64, len(items))
	for i, it := range items {
		sizes[i] = itemBytes(it)
		totalBytes += sizes[i]
	}
	var done int64
	for i, it := range items {
		done += sizes[i]
		out = append(out, catalog.Progress{
			Phase: catalog.PhaseDownload, PhaseIndex: idx(catalog.PhaseDownload), PhaseCount: phaseCount,
			Label: "Downloading package lists", Item: it,
			Current: int64(i + 1), Total: int64(len(items)),
			BytesDone: done, BytesTotal: totalBytes,
		})
	}

	ticks := opts.ParseTicks
	if ticks <= 0 {
		ticks = defaultParseTicks
	}
	// The parse phase counts stanzas, not files: that is what the real one
	// has to report, because one 40 MB Packages file is most of the phase and
	// a per-file bar would sit still through it.
	const stanzasPerTick = 6000
	for i := 1; i <= ticks; i++ {
		out = append(out, catalog.Progress{
			Phase: catalog.PhaseParse, PhaseIndex: idx(catalog.PhaseParse), PhaseCount: phaseCount,
			Label:   "Reading package lists",
			Item:    fmt.Sprintf("%d packages", i*stanzasPerTick),
			Current: int64(i), Total: int64(ticks),
		})
	}

	out = append(out,
		catalog.Progress{
			Phase: catalog.PhaseIndex, PhaseIndex: idx(catalog.PhaseIndex), PhaseCount: phaseCount,
			Label: "Building the search index", Current: 1, Total: 1,
		},
		catalog.Progress{
			Phase: catalog.PhaseSave, PhaseIndex: idx(catalog.PhaseSave), PhaseCount: phaseCount,
			Label: "Saving the catalogue", Item: t.CacheKey(), Current: 1, Total: 1,
		},
		catalog.Progress{
			Phase: catalog.PhaseDone, PhaseIndex: idx(catalog.PhaseDone), PhaseCount: phaseCount,
			Label: "Catalogue ready", Current: 1, Total: 1,
		},
	)
	return out
}

// indexItems is the list of index files a build pretends to fetch. Real refs
// when the target carries sources, a believable stand-in when it does not —
// a frontend package experimenting with a half-filled Target still gets a job
// that looks like the real one.
func indexItems(t catalog.Target) []string {
	refs := t.IndexRefs()
	if len(refs) > 0 {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			out = append(out, r.Path())
		}
		return out
	}
	var out []string
	for _, suite := range []string{"noble", "noble-updates", "noble-security"} {
		for _, comp := range []string{"main", "universe"} {
			out = append(out,
				"dists/"+suite+"/"+comp+"/binary-amd64/Packages.gz",
				"dists/"+suite+"/"+comp+"/dep11/Components-amd64.yml.gz",
			)
		}
	}
	return out
}

// itemBytes gives an index file a plausible size, derived from its path so it
// is stable across runs. The spread matters: a bar that advances in equal
// steps hides the fact that one file is most of the download.
func itemBytes(path string) int64 {
	h := fnv64(path)
	switch {
	case strings.Contains(path, "/universe/") && strings.Contains(path, "Packages"):
		return int64(24_000_000 + h%12_000_000)
	case strings.Contains(path, "Packages"):
		return int64(4_000_000 + h%4_000_000)
	default:
		return int64(1_000_000 + h%6_000_000)
	}
}

// sleepCtx waits for d, or returns ctx's error if the context is done first.
// A zero d still checks the context, so a cancelled build with no delay
// configured stops on its next tick rather than running to completion — which
// is what a test asserting cancellation needs.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
