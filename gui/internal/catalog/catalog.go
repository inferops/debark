package catalog

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"
)

// catalogImpl is the Catalog: it composes the four halves of this package —
// fetch and parse (packages.go, dep11.go), persist (cache.go) and search
// (search.go) — and owns the lifecycle they share.
//
// Everything interesting was decided elsewhere; this file is the wiring, and
// the two decisions it does make are worth stating.
//
// **One search implementation, not two.** cache.go deliberately decodes no
// entry at load: it exposes Haystack, IsApp and SectionID so a caller can
// filter over the raw buffer and materialise only the rows a page returns.
// Search could have been built on those accessors. It is not. After a load,
// every entry is materialised once and handed to search.go's index, because
// the alternative is two implementations of relevance — a cache-backed one and
// an in-memory one — that must agree forever, where the path exercised on a
// cold start differs from the one exercised after a rebuild. No test naturally
// catches that divergence and every user eventually hits it.
//
// The measurements say the cost is affordable: a 6.3 ms load, about 5 ms to
// materialise 70,000 entries at 65 ns each, and a 7.4 ms index build, against
// a 500 ms budget. The zero-decode format still earns its place — it is what
// makes the load 6.3 ms and the file 11.8 MB, and it validates in full on
// every open. Materialising once, deliberately and measurably, is a different
// thing from decoding eagerly because the format left no choice.
//
// **A Catalog is bound to one target, and Ready is what binds it.** Build and
// Ready take a Target; Search, Categories and Get do not. Build binds by
// loading what it just wrote — but on every run after the first, Build is
// never called. Ready is then the only method that is ever told which target
// this Catalog serves, so it records it, and the read path materialises that
// target's cache the first time it is asked for a row.
//
// Ready answering true while Search answers ErrNotBuilt is the one combination
// this type must never produce. Ready true is what sends the UI straight to
// the picker; a picker that then cannot be searched shows loading placeholders
// against a catalogue the application has just told it exists, forever, with
// nothing to click and nothing in the log. That is not a visible failure, it
// is a hang, and it is the ordinary second run.
//
// Searching a Catalog that has been told about no target at all is still
// ErrNotBuilt rather than an empty page, so "no catalogue yet" and "no
// matches" can never be confused — one is a screen offering to build, the
// other is a screen offering to clear a filter.
type catalogImpl struct {
	loader PackagesLoader
	dep11  dep11Options

	mu     sync.RWMutex
	target Target
	index  *searchIndex
	closed bool

	// bound is the target the last valid Ready call asked about, and the one
	// the read path must serve. Ready is where a Catalog learns which target
	// it is for on a warm start — Build is the only other method that takes
	// one, and on a second run Build is never called. Recording it here is
	// what lets Search materialise a cache this process did not write.
	//
	// boundKey and loadedKey are the same two targets' CacheKeys, kept as
	// strings because comparing them is on the path of every Search and
	// Target.CacheKey sorts, joins and hashes to produce one.
	bound     Target
	hasBound  bool
	boundKey  string
	loadedKey string

	// count, stale and builtAt are what the catalogue can say about itself
	// beyond the frozen interface: how many rows it holds, whether the
	// archive has published since it was written, and when that was.
	//
	// They are recorded by Ready as well as by load, and that is the point.
	// On every run after the first, Ready is the only method called before
	// the picker opens — the cache is materialised later, on the first query
	// — so a fact only load could supply is a fact the first frame does not
	// have. Ready gets all three from the same meta.json it already reads to
	// answer at all, at no extra cost.
	count   int
	stale   bool
	builtAt time.Time

	// loadMu serialises the on-demand load. The picker fires a Search on
	// every keystroke and asks for Categories beside it, so without this the
	// first frame after a warm start would decode and index the same file
	// once per caller.
	loadMu sync.Mutex

	// building guards against two concurrent builds of the same catalogue.
	// Not a queue: the second caller is told, because a UI that silently
	// serialises two builds looks frozen for twice as long as it should.
	building bool

	// toolVersion is recorded in the cache marker so a cache written by a
	// different build is identifiable after the fact.
	toolVersion string
}

// Options configures a Catalog. The zero value is valid and uses defaults.
type Options struct {
	// Packages configures index fetching. Its HTTPClient is the seam tests
	// use to serve fixtures instead of reaching the network.
	Packages PackagesLoader
	// DEP11 configures AppStream fetching. Its Client is the same seam.
	DEP11 dep11Options
	// ToolVersion is recorded in the cache marker, e.g. "debark-gui 0.1.0".
	ToolVersion string
}

// New returns a Catalog that fetches from the network and caches to disk.
func New(opts Options) Catalog {
	c := &catalogImpl{loader: opts.Packages, dep11: opts.DEP11}
	if c.dep11.UserAgent == "" {
		c.dep11.UserAgent = c.loader.UserAgent
	}
	c.toolVersion = opts.ToolVersion
	return c
}

// Build fetches the target's indexes, parses them, writes the cache and loads
// the result into memory.
//
// It reports progress through every phase and is cancellable at each one. A
// cancelled build leaves no partial catalogue: the cache is written under a
// temporary name and committed by a marker file, so an interrupted run reads
// back as "not built" rather than as a short one.
func (c *catalogImpl) Build(ctx context.Context, t Target, progress func(Progress)) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if err := t.Validate(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.building {
		c.mu.Unlock()
		return ErrBuildInProgress
	}
	c.building = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.building = false
		c.mu.Unlock()
	}()

	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}
	started := time.Now()

	// Phase 1-2: the binary indexes. This is the part that must succeed —
	// without Packages there is no catalogue at all.
	res, err := c.loader.Load(ctx, t, report)
	if err != nil {
		return err
	}

	// Phase 3: AppStream. Deliberately not fatal. DEP-11 covers about 3% of
	// packages and turns "libreoffice-writer" into "LibreOffice Writer"; an
	// archive that does not publish it, or a decode that partly fails, costs
	// nicer names on a minority of rows and nothing else. Refusing to build a
	// 70,000-row catalogue over that would be the wrong trade.
	apps, derr := dep11Load(ctx, t.IndexRefs(), c.dep11WithProgress(report, started))
	if derr != nil && ctx.Err() != nil {
		return derr
	}
	if apps != nil {
		byName := make(map[string]struct{}, len(res.Entries))
		for i := range res.Entries {
			byName[res.Entries[i].Name] = struct{}{}
		}
		// Drop components naming packages this target does not have. Ubuntu
		// noble's DEP-11 was generated months before release and refers to
		// packages that no longer exist; a phantom row is worse than a plain
		// one.
		apps.Reconcile(func(pkg string) bool { _, ok := byName[pkg]; return ok })
		for i := range res.Entries {
			apps.Apply(&res.Entries[i])
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 4: persist, then load back through exactly the path a warm start
	// uses. Building the in-memory index from the freshly parsed slice instead
	// would mean the first session after a build ran on data that had never
	// been through the encoder — so a format bug would surface only on the
	// second run, on someone else's machine.
	report(Progress{Phase: PhaseSave, Label: PhaseSave.Label(), Elapsed: time.Since(started)})
	dir, err := CacheDir(t)
	if err != nil {
		return err
	}
	if _, err := SaveCache(ctx, dir, CacheInput{
		Target:      t,
		Entries:     res.Entries,
		ToolVersion: c.toolVersion,
	}); err != nil {
		return err
	}

	if _, err := c.load(t); err != nil {
		return err
	}
	report(Progress{Phase: PhaseDone, Label: PhaseDone.Label(), Elapsed: time.Since(started)})
	return nil
}

// Ready reports whether a usable cache exists for this target, so the UI can
// go straight to the picker instead of offering to build one.
//
// A stale cache counts as ready: it loads, and the operator is told it may be
// out of date. Showing an empty screen because an index digest moved would be
// the worse answer.
//
// Asking binds this Catalog to t. That is not a side effect smuggled into a
// predicate: it is the answer to "which target is this Catalog for?" on the
// only path where nothing else ever supplies one. The check itself stays what
// the contract requires — a stat and a small header read, never a full load —
// because binding is a field assignment and the cache is materialised later,
// on the first query, by whichever of Search, Categories or Get comes first.
//
// The target is recorded whether the answer is true or false, which is what
// keeps a Catalog from serving the previous target's rows after the operator
// switches to one that has no cache: the read path serves the bound target or
// it fails, and it never quietly serves a different one.
func (c *catalogImpl) Ready(t Target) (bool, error) {
	if err := c.checkOpen(); err != nil {
		return false, err
	}
	if err := t.Validate(); err != nil {
		return false, nil
	}
	key := t.CacheKey()
	c.mu.Lock()
	c.bound, c.hasBound, c.boundKey = t, true, key
	c.mu.Unlock()

	dir, err := CacheDir(t)
	if err != nil {
		return false, err
	}
	meta, ok, err := cacheReadyMeta(dir, t)
	if err != nil {
		return false, err
	}

	// Record what the marker says about the cache this Catalog is now bound
	// to, including when it is not ready — which is what clears the previous
	// target's count instead of leaving the picker showing it.
	c.mu.Lock()
	if ok {
		c.count, c.stale, c.builtAt = meta.EntryCount, cacheStale(t, meta), meta.BuiltAt
	} else {
		c.count, c.stale, c.builtAt = 0, false, time.Time{}
	}
	c.mu.Unlock()
	return ok, nil
}

// PackageCount is how many rows the catalogue holds, and 0 when there is none.
//
// Additive to the frozen interface, like Stale and BuiltAt below, and answered
// without a load: on a warm start it is meta.json's entry_count, and after a
// load it is the number of rows that actually materialised, which is the same
// number unless the file held a record the format could not decode.
func (c *catalogImpl) PackageCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.count
}

// BuiltAt is when the loaded or bound cache was written, zero when there is
// none.
func (c *catalogImpl) BuiltAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.builtAt
}

// Search returns one page. It runs entirely in memory and never touches the
// network; the one exception is the first query of a warm start, which
// materialises the bound target's cache before answering. That costs about
// 20 ms once — a 6 ms read, 5 ms to materialise the entries and 7 ms to index
// them — and nothing on every query after it.
func (c *catalogImpl) Search(ctx context.Context, q Query) (Page, error) {
	ix, err := c.ensureLoaded()
	if err != nil {
		return Page{}, err
	}
	return ix.Search(ctx, q)
}

// Categories returns the two-tier grouping: DEP-11 application categories over
// a complete apt Section: catalogue.
func (c *catalogImpl) Categories(ctx context.Context) ([]Category, error) {
	ix, err := c.ensureLoaded()
	if err != nil {
		return nil, err
	}
	return ix.Categories(ctx)
}

// Get returns one entry by exact binary package name.
func (c *catalogImpl) Get(ctx context.Context, name string) (Entry, bool, error) {
	ix, err := c.ensureLoaded()
	if err != nil {
		return Entry{}, false, err
	}
	return ix.Get(ctx, name)
}

// Close releases the in-memory index. The on-disk cache is left alone: it is
// the point of having written it.
func (c *catalogImpl) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index = nil
	c.closed = true
	return nil
}

// Stale reports whether the catalogue was built from indexes the archive has
// since republished. Additive to the frozen interface: a caller that does not
// ask is not misled, and one that does can offer a refresh.
//
// Answered on the warm path as well as after a load, because a cache-loaded
// catalogue is the only kind that can be stale and Ready is the only method a
// warm start calls before the picker opens.
//
// It is false whenever freshness is unknown, and today freshness is always
// unknown: nothing populates either Target.IndexDigest or the marker's
// index_digest, so the comparison in cacheStale never has two sides. See
// docs/dev/cache-format.md §6.2.
func (c *catalogImpl) Stale() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stale
}

// LoadTarget loads an existing cache without building, for a caller that wants
// the warm start paid for up front rather than on the first query. It is not
// required: Ready binds the target and the read path materialises it on
// demand, so a caller that only ever uses the frozen interface still gets a
// working catalogue. This exists for one that would rather spend the 20 ms
// somewhere it is not being watched.
func (c *catalogImpl) LoadTarget(t Target) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	key := t.CacheKey()
	if _, err := c.load(t); err != nil {
		return err
	}
	c.mu.Lock()
	c.bound, c.hasBound, c.boundKey = t, true, key
	c.mu.Unlock()
	return nil
}

// load reads the cache for t and swaps in a fresh index.
//
// ErrStale is not a failure: LoadCache returns a usable file alongside it, and
// the catalogue is served with the staleness recorded.
func (c *catalogImpl) load(t Target) (*searchIndex, error) {
	cf, err := LoadCache(t)
	stale := errors.Is(err, ErrStale)
	if err != nil && !stale {
		return nil, err
	}
	if cf == nil {
		return nil, ErrNotBuilt
	}

	n := cf.Len()
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		e, ok := cf.Entry(i)
		if !ok {
			// A record the format could not decode. cache.go counts these and
			// keeps serving; skipping the row is right, because the
			// alternative is refusing a 70,000-row catalogue over one damaged
			// entry the operator can neither see nor fix.
			continue
		}
		entries = append(entries, e)
	}

	// Read everything still wanted from cf *before* the index is built, so
	// that this is cf's last use and the 19 MB file buffer is collectable
	// while newSearchIndex is running.
	//
	// This is not a style preference. Go's collector is precise about
	// liveness: a pointer is live until its last use, so reading
	// cf.meta.BuiltAt after newSearchIndex — which is where this line used to
	// be — pinned the whole cache buffer across the peak of the load. It cost
	// 19 MB of peak heap, and peak heap is what the process is charged for
	// long after the bytes are garbage. Measured in docs/performance.md,
	// "What the load actually costs the process".
	builtAt := cf.meta.BuiltAt

	ix := newSearchIndex(entries)
	key := t.CacheKey()

	c.mu.Lock()
	c.target = t
	c.loadedKey = key
	c.index = ix
	c.stale = stale
	// len(entries), not cf.Len(): the count the UI shows must be the number of
	// rows a search can return, and the two differ exactly when a record was
	// skipped above.
	c.count = len(entries)
	c.builtAt = builtAt
	c.mu.Unlock()

	// Hand the load's own transient memory back to the operating system,
	// once, here, and nowhere else.
	//
	// A load is the largest allocation this application ever makes and almost
	// all of it is garbage the moment it finishes: the 19 MB catalog.bin
	// buffer plus about 9 MB of index-construction scratch, against a live
	// catalogue of 38.7 MB. Measured on the app's own 85,565-entry cache,
	// HeapAlloc is 68.4 MB when this function returns and 39.8 MB after a
	// collection — and the pages behind the 28.5 MB difference stay charged
	// to the process, because the Go scavenger only returns what is above the
	// heap goal and twice a 39 MB live heap is above everything this process
	// is holding. That is the ~29 MB the app-level memory pass could see in
	// /proc and could not explain from it, and it survives 90 seconds of idle
	// because nothing about idling changes the heap goal.
	//
	// debug.FreeOSMemory is a real cost — a full stop-the-world collection,
	// measured at 4.7-26 ms on machine A and 8-11 ms on machine B — so it is
	// spent deliberately and exactly once per load:
	//
	//   - This is the one moment in the process's life where a forced
	//     collection is nearly free. The caller is already inside a load it
	//     is waiting on, no frame is animating, and the next thing that
	//     happens is a picker sitting idle.
	//   - It is not on a timer. A periodic release would pay the same
	//     collection over and over for nothing and would fight the pacer
	//     while someone is typing, which is the budget that matters most.
	//   - It is not per query. Search allocates one page per call and
	//     nothing that scales with the corpus; there is no drift to sweep up.
	//   - load runs at most once per target per process: ensureLoaded holds
	//     loadMu and re-checks loadedKey, so the picker's first frame firing
	//     Search and Categories together produces one load and one release.
	//
	// docs/performance.md, "What the load actually costs the process", holds
	// the measurement and the before/after resident set of the real app.
	debug.FreeOSMemory()

	// Returned as well as stored: the caller that asked for this load is
	// serving a query against this target, and re-reading the field would
	// hand it whatever a concurrent Build or target switch had put there
	// since.
	return ix, nil
}

// ensureLoaded returns the index for the bound target, materialising it from
// the cache on the way if this process has not built one.
//
// The identity check is the point of the second half. An index left over from
// a target the operator has moved on from is worse than no index at all: it
// answers every query with plausible rows for the wrong base, and there is
// nothing on screen to say so. Serving only the bound target means a switch to
// a target with no cache fails loudly instead.
func (c *catalogImpl) ensureLoaded() (*searchIndex, error) {
	c.mu.RLock()
	ix, closed, loadedKey, bound, hasBound, boundKey := c.index, c.closed, c.loadedKey, c.bound, c.hasBound, c.boundKey
	c.mu.RUnlock()

	if closed {
		return nil, ErrClosed
	}
	if ix != nil && (!hasBound || loadedKey == boundKey) {
		return ix, nil
	}
	if !hasBound {
		// Nothing has ever named a target for this Catalog. Not a warm start:
		// a caller that reached Search before Build or Ready.
		return nil, ErrNotBuilt
	}

	// One load, not one per caller. The picker's first frame fires a Search
	// and a Categories together, and a Search on every keystroke after that.
	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	c.mu.RLock()
	ix, closed, loadedKey = c.index, c.closed, c.loadedKey
	c.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if ix != nil && loadedKey == boundKey {
		return ix, nil
	}

	// A failure here leaves the previous target's index in place but still
	// unreachable, because the identity check above rejects it. The caller
	// gets the load's own error — ErrNotBuilt, ErrCacheCorrupt or
	// ErrCacheVersion — each of which the UI already turns into a rebuild.
	ix, err := c.load(bound)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	closed = c.closed
	c.mu.RUnlock()
	if closed {
		// Closed while the cache was being read. Releasing the index is the
		// whole point of Close, so hand back ErrClosed rather than the copy
		// this call happens to be holding.
		return nil, ErrClosed
	}
	return ix, nil
}

func (c *catalogImpl) checkOpen() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return ErrClosed
	}
	return nil
}

// dep11WithProgress copies the DEP-11 options with this run's progress sink
// and start time attached, so the two loaders report one continuous job rather
// than two that each start from zero.
func (c *catalogImpl) dep11WithProgress(report func(Progress), started time.Time) dep11Options {
	o := c.dep11
	o.Progress = report
	o.Started = started
	return o
}

// compile-time proof that the implementation satisfies the frozen interface.
var _ Catalog = (*catalogImpl)(nil)
