package catalog

// packages.go — fetching and parsing the target's `Packages` indexes.
//
// This file turns the IndexPackages refs of a Target into catalog.Entry
// values. It is the whole of the catalogue's first tier: DEP-11 (dep11.go)
// only decorates rows that already exist because a stanza was read here.
//
// # Why a hand-rolled deb822 scanner
//
// pault.ag/go/debian/control is in the module graph transitively and would
// parse these files correctly. It is not used, for two measured reasons.
//
//   - It materialises a control.Paragraph per stanza — `map[string]string`
//     plus an `Order []string` — which is exactly strategy C in
//     docs/dev/index-formats.md §5: 235 MB of heap for 70,854 stanzas against
//     a 400 MB idle budget, to keep 64 fields per row of which this package
//     displays 9. The scanner below never builds a per-stanza map; it copies
//     the nine fields it keeps straight into an Entry and drops every other
//     line without allocating (strategy A, 15.3 MB).
//   - Promoting a transitive dependency to a direct one is a dependency
//     decision under contract-brief rule 6, requiring a written justification
//     rather than a `go get`. Paying that for a 200-line scanner that has to
//     exist anyway to avoid the map is not a good trade.
//
// Everything here is stdlib.
//
// # What it deliberately does not do
//
// It does not compare two version strings, for any purpose. See
// packagesMerger.add: the duplicate-name rule is positional, and the reason it
// has to be is contract-brief rule 1.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PackagesUserAgent identifies this application to the archives it fetches
// from. Archive operators are entitled to know who is pulling 19 MB from
// them, and an honest agent string is the difference between a mirror
// rate-limiting "some Go program" and rate-limiting nothing.
//
// It carries no URL: contract-brief rule 3 says this application contacts no
// debark-operated endpoint, and the cleanest way to keep that true is for
// no such address to appear in the code at all.
const PackagesUserAgent = "debark-gui (apt catalogue index reader)"

const (
	// packagesDefaultConcurrency bounds how many index files are fetched at
	// once. An archive is one host: opening a connection per component across
	// four suites is 24 sockets pointed at one mirror, which is rude and no
	// faster than four.
	packagesDefaultConcurrency = 4

	// packagesDefaultMaxIndexBytes caps a single compressed index. Ubuntu's
	// `universe` — the largest index this application will meet in practice —
	// is 19,315,644 bytes (docs/dev/index-formats.md §1). 256 MiB is an order
	// of magnitude of headroom and still bounds what a hostile or broken
	// mirror can make this process allocate.
	packagesDefaultMaxIndexBytes = 256 << 20

	// packagesDefaultMaxDecompressedBytes caps what one Packages index may
	// expand to, and is deliberately the same 512 MiB dep11.go has used since
	// it was written (dep11MaxDecompressedBytes).
	//
	// packagesDefaultMaxIndexBytes bounds the *compressed* body. Nothing
	// bounded the inflated stream, and gzip reaches roughly 1000:1 on
	// repetitive input, so 256 MB of accepted body is about 256 GB of
	// "Package: p1" stanzas separated by blank lines — on a machine whose
	// whole idle-memory budget is 400 MB. The largest real index is Ubuntu's
	// universe/amd64 at ~19 MB compressed and ~130 MB expanded, so this is
	// nearly four times the biggest honest file and four orders of magnitude
	// short of the bomb.
	//
	// Two loaders in one package disagreeing about whether a decompression
	// bomb is worth bounding is what made this a defect rather than a
	// judgement call (docs/security-review.md §3.2), and it was not the one
	// with the cap that was wrong.
	packagesDefaultMaxDecompressedBytes = 512 << 20

	// packagesDefaultMaxEntries caps how many stanzas one index may emit.
	//
	// The byte cap alone is not enough. A minimal stanza is thirteen bytes,
	// so 512 MiB of accepted text is about 41 million stanzas, and each one
	// becomes an Entry the caller keeps — several gigabytes of live heap
	// inside a limit that was only ever about bytes on the wire. Counting the
	// things that are retained is the cheaper and more direct backstop.
	//
	// Ubuntu main + universe is roughly 70,000 binary packages in total
	// (contract-brief.md), and this is a per-index cap, so a million is more
	// than an order of magnitude above any single real file while still
	// bounding what one hostile index can retain.
	packagesDefaultMaxEntries = 1 << 20

	// packagesScanBufferInitial / packagesScanBufferMax size the line scanner.
	//
	// THIS IS NOT A ROUND NUMBER PICKED FOR COMFORT. bufio.Scanner's default
	// maximum token is bufio.MaxScanTokenSize (64 KiB), and the longest single
	// field value in the real Ubuntu `universe` index is `Provides:` on
	// librust-winapi-dev at 70,830 bytes — Debian's copy is 75,639
	// (docs/dev/index-formats.md §4.2). A default scanner therefore fails with
	// bufio.ErrTooLong partway through real archive data, which is a silently
	// truncated catalogue. 4 MiB is ~55x the largest value measured and still
	// a bounded allocation.
	packagesScanBufferInitial = 64 << 10
	packagesScanBufferMax     = 4 << 20

	// packagesCancelCheckEvery is how often the parse loop looks at the
	// context. At ~90 ms for 70,000 stanzas a check every 512 stanzas costs
	// nothing and bounds the cancellation delay at well under a millisecond.
	packagesCancelCheckEvery = 512

	// packagesProgressInterval throttles progress callbacks. Every callback
	// eventually crosses the Wails bridge; a byte counter firing per 32 KiB
	// read would emit thousands of events for one download.
	packagesProgressInterval = 100 * time.Millisecond

	// packagesBytesPerStanza is a sizing heuristic only: the measured Ubuntu
	// `universe` index is 19,315,644 compressed bytes for 64,755 stanzas, or
	// ~298 bytes each. Rounding down to 256 over-allocates the entry slice
	// slightly rather than growing it through a dozen doublings of a 6 MB
	// backing array. It affects allocation, never correctness.
	packagesBytesPerStanza = 256

	// packagesMaxPrealloc bounds that heuristic, so a mirror advertising an
	// absurd Content-Length cannot turn into an absurd make().
	packagesMaxPrealloc = 1 << 20

	// Extended text is display metadata. Bound both one hostile stanza and the
	// complete parse; exceeding either keeps the summary and marks the detail.
	packagesMaxDescriptionBytes  = 64 << 10
	packagesMaxDescriptionsBytes = 64 << 20
)

// ErrPackagesIndexMissing means the archive does not publish a `Packages` file
// for one (suite, component, arch) tuple — a 404 from every mirror offering
// it.
//
// It is not fatal and Load does not return it: a component that publishes no
// binary index for this architecture is a normal archive layout, and the
// answer is a shorter catalogue, not a failed build. Load records the fact in
// PackagesResult.Missing so that the UI and a support log can both see which
// components contributed nothing. A network failure, by contrast, IS fatal —
// see PackagesFetchError — because "the mirror was unreachable" and "the
// mirror says there is nothing here" are different facts and a catalogue that
// conflates them is silently incomplete in a way no operator can detect.
var ErrPackagesIndexMissing = errors.New("catalog: archive publishes no Packages index for this component")

// PackagesFetchError is a failure to obtain one index that is not the archive
// saying "no such file". Unreachable host, TLS failure, 500, truncated body,
// unreadable gzip: every one of them means the catalogue would be missing rows
// it should have had, so every one of them fails the build.
type PackagesFetchError struct {
	// Ref is the index that could not be read.
	Ref IndexRef
	// URL is the absolute URL that was attempted, kept separately because a
	// group of mirrors shares everything except this.
	URL string
	// Status is the HTTP status code when there was a response, or 0.
	Status int
	// Err is the underlying transport, decompression or parse error, if any.
	Err error
}

func (e *PackagesFetchError) Error() string {
	switch {
	case e.Status != 0 && e.Err != nil:
		return fmt.Sprintf("catalog: fetching %s: HTTP %d: %v", e.URL, e.Status, e.Err)
	case e.Status != 0:
		return fmt.Sprintf("catalog: fetching %s: HTTP %d", e.URL, e.Status)
	case e.Err != nil:
		return fmt.Sprintf("catalog: fetching %s: %v", e.URL, e.Err)
	default:
		return "catalog: fetching " + e.URL + ": failed"
	}
}

func (e *PackagesFetchError) Unwrap() error { return e.Err }

// PackagesIndexMiss records one index the archive does not publish. It exists
// so that "the catalogue has no `restricted` packages" can be explained rather
// than wondered about.
type PackagesIndexMiss struct {
	// Ref is the index that was not there.
	Ref IndexRef
	// URL is the last URL tried for it.
	URL string
	// Status is the HTTP status that said so — 404, or 410.
	Status int
	// Reason is a human sentence for a log or a details drawer.
	Reason string
}

// PackagesParseStats is what one index turned out to contain. Every field is
// a count of something that happened rather than a judgement about it: a
// caller decides whether 40 malformed lines is a broken mirror or a quirk.
type PackagesParseStats struct {
	// Stanzas is how many stanzas the parser saw, including ones it dropped.
	Stanzas int64
	// Emitted is how many of those had a Package: field and became an Entry.
	Emitted int64
	// SkippedStanzas is how many had fields but no usable Package: name --
	// absent, or carrying a control character. See pkgUsableName.
	SkippedStanzas int64
	// MalformedLines is how many non-blank, non-continuation lines carried no
	// field name — the signature of a truncated or non-deb822 file.
	MalformedLines int64
}

// PackagesResult is everything one Load produced.
type PackagesResult struct {
	// Entries is the merged, deduplicated catalogue, in first-seen order
	// across the target's indexes. Ordering for display is search.go's
	// business; this order is simply stable.
	Entries []Entry
	// Indexes is how many index files were actually read.
	Indexes int
	// Missing is every index the archive does not publish. Empty on a
	// well-formed target, and never a failure.
	Missing []PackagesIndexMiss
	// Stats aggregates the per-index parse counts.
	Stats PackagesParseStats
	// DuplicateNames is how many stanzas named a package some earlier stanza
	// had already named — within one index or across two. The measured figure
	// for Ubuntu noble main+universe is 1, and for Debian bookworm main it is
	// 4 (docs/dev/index-formats.md §4.4). A number in the thousands means the
	// same index was fetched twice and is worth a line in a log.
	DuplicateNames int64
	// BytesDownloaded is the compressed byte count actually transferred.
	BytesDownloaded int64
}

// PackagesLoader fetches and parses the `Packages` half of a target's indexes.
//
// The zero value works and uses the defaults documented on each field; the
// fields exist so that tests can serve fixtures from httptest and so that a
// caller can lower the concurrency for a mirror it knows to be fragile.
type PackagesLoader struct {
	// HTTPClient is the client every request goes through. nil means a client
	// with a 60 second per-request timeout. Injectable because no test in
	// this repository may touch the network.
	HTTPClient *http.Client
	// UserAgent overrides PackagesUserAgent. Empty means PackagesUserAgent.
	UserAgent string
	// Concurrency bounds simultaneous downloads. <= 0 means
	// packagesDefaultConcurrency.
	Concurrency int
	// MaxIndexBytes caps one compressed index. <= 0 means
	// packagesDefaultMaxIndexBytes.
	MaxIndexBytes int64
	// MaxDecompressedBytes caps one index after decompression. <= 0 means
	// packagesDefaultMaxDecompressedBytes. This is the field dep11.go has
	// always had; the shape is deliberately identical so the two loaders can
	// be reasoned about together.
	MaxDecompressedBytes int64
	// MaxEntries caps how many package stanzas one index may emit. <= 0 means
	// packagesDefaultMaxEntries.
	MaxEntries int64
}

// packagesLimits is what one parse is allowed to consume. It is a struct
// rather than two parameters so that adding a third limit later does not
// re-thread every call site, and so a test can say what it is varying.
type packagesLimits struct {
	// Decompressed caps the inflated stream. <= 0 disables the cap, which no
	// production path does — only a test that means to feed something huge on
	// purpose.
	Decompressed int64
	// Entries caps emitted stanzas. <= 0 disables.
	Entries int64
}

// packagesDefaultLimits is what a caller with no opinion gets. Used by tests
// and by anything that parses a body it did not fetch.
func packagesDefaultLimits() packagesLimits {
	return packagesLimits{
		Decompressed: packagesDefaultMaxDecompressedBytes,
		Entries:      packagesDefaultMaxEntries,
	}
}

// Load fetches every IndexPackages ref of t and parses it into entries.
//
// It reports through progress — which may be nil — using PhaseDownload while
// bytes are moving and PhaseParse while they are being read. The two phases
// are separated rather than pipelined so that the UI can say which of the two
// it is waiting on: a fused download-and-parse pass would flicker between
// them, and the measured split is 0.4 s of parsing against tens of seconds of
// downloading (docs/dev/index-formats.md §7.4), so there is nothing to gain by
// overlapping them.
//
// Cancelling ctx aborts promptly, mid-download or mid-parse, and returns an
// error satisfying errors.Is(err, context.Canceled).
func (l PackagesLoader) Load(ctx context.Context, t Target, progress func(Progress)) (PackagesResult, error) {
	var res PackagesResult

	groups := packagesGroupsFor(t)
	if len(groups) == 0 {
		return res, errors.New("catalog: target names no binary Packages indexes to fetch")
	}

	rep := &packagesReporter{fn: progress, start: time.Now(), total: int64(len(groups))}

	// Phase 1: download. Compressed bytes are held in memory — 19.3 MB for
	// the largest real index, and roughly 26 MB for a whole Ubuntu target
	// with updates and security enabled. The decompressed form (73 MB for
	// `universe` alone) is never materialised: phase 2 streams it through the
	// scanner and keeps only the nine fields an Entry declares
	// (docs/dev/index-formats.md §5).
	rep.send(Progress{
		Phase: PhaseDownload,
		Label: fmt.Sprintf("Downloading %s", packagesPlural(len(groups), "package list")),
		Total: int64(len(groups)),
	}, true)

	bodies := make([][]byte, len(groups))
	misses := make([]*PackagesIndexMiss, len(groups))
	errs := make([]error, len(groups))

	l.packagesDownloadAll(ctx, groups, bodies, misses, errs, rep)

	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("catalog: downloading package lists: %w", err)
	}
	for i := range errs {
		if errs[i] != nil {
			return res, errs[i]
		}
	}

	var compressed int64
	for _, b := range bodies {
		compressed += int64(len(b))
	}
	res.BytesDownloaded = compressed

	rep.send(Progress{
		Phase:      PhaseDownload,
		Label:      fmt.Sprintf("Downloaded %s", packagesPlural(len(groups), "package list")),
		Current:    int64(len(groups)),
		Total:      int64(len(groups)),
		BytesDone:  compressed,
		BytesTotal: rep.bytesTotal.Load(),
	}, true)

	// Phase 2: parse, serially and in ref order. Serial because it is CPU
	// work of well under a second in total, and IN REF ORDER because the
	// duplicate-name rule is positional (packagesMerger.add) and must not
	// depend on which mirror answered first.
	merger := newPackagesMerger(compressed)

	rep.send(Progress{
		Phase: PhaseParse,
		Label: fmt.Sprintf("Reading %s", packagesPlural(len(groups), "package list")),
		Total: int64(len(groups)),
	}, true)

	var read int
	for i, g := range groups {
		if miss := misses[i]; miss != nil {
			res.Missing = append(res.Missing, *miss)
			rep.send(Progress{
				Phase:   PhaseParse,
				Label:   miss.Reason,
				Item:    packagesItem(g.ref),
				Current: int64(i + 1),
				Total:   int64(len(groups)),
			}, true)
			continue
		}

		stats, err := packagesParseBody(ctx, bodies[i], g.ref, l.limits(), merger.add)
		bodies[i] = nil // release the compressed copy as soon as it is spent
		if err != nil {
			return res, err
		}
		read++
		res.Stats.Stanzas += stats.Stanzas
		res.Stats.Emitted += stats.Emitted
		res.Stats.SkippedStanzas += stats.SkippedStanzas
		res.Stats.MalformedLines += stats.MalformedLines

		rep.send(Progress{
			Phase:   PhaseParse,
			Label:   fmt.Sprintf("Read %s/%s — %d packages", g.ref.Suite, g.ref.Component, stats.Emitted),
			Item:    packagesItem(g.ref),
			Current: int64(i + 1),
			Total:   int64(len(groups)),
		}, true)
	}

	res.Indexes = read
	res.Entries = merger.entries
	res.DuplicateNames = merger.duplicates

	rep.send(Progress{
		Phase:   PhaseParse,
		Label:   fmt.Sprintf("Read %d packages from %s", len(res.Entries), packagesPlural(read, "package list")),
		Current: int64(len(groups)),
		Total:   int64(len(groups)),
	}, true)

	return res, nil
}

// packagesDownloadAll runs the download phase across a bounded worker pool.
// Results are written into per-group slots so that the phase's concurrency
// cannot leak into the order anything is merged in.
func (l PackagesLoader) packagesDownloadAll(
	ctx context.Context,
	groups []packagesGroup,
	bodies [][]byte,
	misses []*PackagesIndexMiss,
	errs []error,
	rep *packagesReporter,
) {
	workers := l.Concurrency
	if workers <= 0 {
		workers = packagesDefaultConcurrency
	}
	if workers > len(groups) {
		workers = len(groups)
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(groups) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				body, miss, err := l.packagesFetchGroup(ctx, groups[i], rep)
				bodies[i], misses[i], errs[i] = body, miss, err
				rep.finishOne(packagesItem(groups[i].ref))
			}
		}()
	}
	wg.Wait()
}

// packagesGroup is one logical index — a (suite, component, arch) tuple — and
// every URI that offers it.
//
// Target.IndexRefs expands a source's URIs into one ref each, and its doc says
// each is "a separate place the same indexes can be fetched from, so IndexRefs
// expands them all and the fetcher may choose". This is that choice: the same
// 19 MB index is downloaded once, from the first URI that answers, and the
// remaining URIs are fallbacks. Downloading it once per mirror would multiply
// both the transfer and the resident bytes for identical content.
type packagesGroup struct {
	// ref is the preferred ref: the first URI the target listed for this
	// tuple. It supplies Suite, Component and Arch for every Entry the group
	// produces, and its position fixes the group's merge order.
	ref IndexRef
	// urls is every URL that offers this index, in the target's own order.
	urls []string
}

// packagesGroupsFor collects the target's IndexPackages refs into one group
// per (suite, component, arch), preserving the order IndexRefs returned them
// in. That order is the merge order, and the merge order is the whole of the
// duplicate-name rule.
func packagesGroupsFor(t Target) []packagesGroup {
	var groups []packagesGroup
	at := make(map[string]int)
	for _, r := range t.IndexRefs() {
		if r.Kind != IndexPackages {
			continue
		}
		key := r.Suite + "\x00" + r.Component + "\x00" + r.Arch
		if i, ok := at[key]; ok {
			groups[i].urls = append(groups[i].urls, r.URL())
			continue
		}
		at[key] = len(groups)
		groups = append(groups, packagesGroup{ref: r, urls: []string{r.URL()}})
	}
	return groups
}

// packagesFetchGroup downloads one group, trying its URLs in order.
//
// The return triple is the whole point of the function: (body, nil, nil) is
// success, (nil, miss, nil) is "the archive says this component publishes no
// index here", and (nil, nil, err) is "something went wrong and the catalogue
// would be wrong if we carried on". A 404 from every mirror is a miss; any
// other failure, from any mirror, when no mirror succeeded, is an error.
func (l PackagesLoader) packagesFetchGroup(ctx context.Context, g packagesGroup, rep *packagesReporter) ([]byte, *PackagesIndexMiss, error) {
	var (
		lastMiss *PackagesIndexMiss
		firstErr error
	)
	for _, url := range g.urls {
		body, status, err := l.packagesGet(ctx, url, g.ref, rep)
		switch {
		case err == nil:
			return body, nil, nil
		case errors.Is(err, ErrPackagesIndexMissing):
			lastMiss = &PackagesIndexMiss{
				Ref:    g.ref,
				URL:    url,
				Status: status,
				Reason: fmt.Sprintf("%s/%s publishes no %s package list (HTTP %d) — its packages are not in the catalogue", g.ref.Suite, g.ref.Component, g.ref.Arch, status),
			}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, nil, err
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return nil, lastMiss, nil
}

// packagesGet performs one request and returns the compressed body.
func (l PackagesLoader) packagesGet(ctx context.Context, url string, ref IndexRef, rep *packagesReporter) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, &PackagesFetchError{Ref: ref, URL: url, Err: err}
	}
	req.Header.Set("User-Agent", l.userAgent())
	// Ask for the bytes as published. The path already ends in .gz, so
	// transfer compression would only re-compress compressed data, and
	// declaring identity keeps Content-Length meaningful — Go's transport
	// otherwise adds Accept-Encoding: gzip itself and silently strips the
	// header from the response it decompresses.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := l.client().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		return nil, 0, &PackagesFetchError{Ref: ref, URL: url, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return nil, resp.StatusCode, fmt.Errorf("%w: %s", ErrPackagesIndexMissing, url)
	case resp.StatusCode != http.StatusOK:
		return nil, resp.StatusCode, &PackagesFetchError{Ref: ref, URL: url, Status: resp.StatusCode}
	}

	limit := l.maxIndexBytes()
	if cl := resp.ContentLength; cl > 0 {
		if cl > limit {
			return nil, resp.StatusCode, &PackagesFetchError{
				Ref: ref, URL: url, Status: resp.StatusCode,
				Err: fmt.Errorf("index declares %d bytes, over the %d byte limit", cl, limit),
			}
		}
		rep.addTotal(cl)
	}

	var buf bytes.Buffer
	if cl := resp.ContentLength; cl > 0 && cl <= limit && cl < packagesMaxGrow {
		buf.Grow(int(cl))
	}
	item := packagesItem(ref)
	n, err := buf.ReadFrom(io.LimitReader(&packagesCountingReader{ctx: ctx, r: resp.Body, rep: rep, item: item}, limit+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, resp.StatusCode, ctxErr
		}
		return nil, resp.StatusCode, &PackagesFetchError{Ref: ref, URL: url, Status: resp.StatusCode, Err: err}
	}
	if n > limit {
		return nil, resp.StatusCode, &PackagesFetchError{
			Ref: ref, URL: url, Status: resp.StatusCode,
			Err: fmt.Errorf("index exceeds the %d byte limit", limit),
		}
	}
	return buf.Bytes(), resp.StatusCode, nil
}

// packagesMaxGrow bounds a Content-Length-driven Grow so the int conversion is
// safe on every platform this builds for.
const packagesMaxGrow = 1 << 30

func (l PackagesLoader) client() *http.Client {
	if l.HTTPClient != nil {
		return l.HTTPClient
	}
	return packagesDefaultClient
}

// The redirect policy lives in this file rather than beside igIndexSchemes
// in sources.go for one concrete reason: internal/audit keeps an inventory
// of every file allowed to import a network package, and sources.go is not
// on it and should not be. Adding an entry there is a design decision, not
// a test fix, and the parser that reads /etc/apt/sources.list has no
// business holding an http.Request. It is shared with dep11.go, which is
// on the inventory for the same reason this file is.
//
// igMaxIndexRedirects bounds a redirect chain. Go's default is ten; archives
// redirect at most once or twice in practice (a mirror redirector, then the
// mirror), so five is generous and a chain longer than that is a loop or a
// misconfiguration rather than a mirror.
const igMaxIndexRedirects = 5

// igCheckIndexRedirect is the CheckRedirect both index HTTP clients use.
//
// Neither client set one, so both took Go's default: follow up to ten
// redirects, to any host, including https: -> http:
// (docs/security-review.md §3.3). Two of those three are worth refusing and
// one is not, and the difference is worth writing down rather than leaving to
// whoever reads the four lines next.
//
// Refused: a scheme that is not on igIndexSchemes, and a downgrade from https
// to http. The first is the same rule IndexRefs applies to the URI a snapshot
// names, applied again to the URI a *server* names, so a redirect cannot
// reintroduce the protocol set the allow-list just removed. The second is the
// only redirect that can take a fetch that was confidentiality-protected and
// make it not be, which is a decision no archive gets to make for the
// operator.
//
// Allowed, deliberately: a redirect to another host. A mirror redirector that
// sends a client to a nearby mirror is ordinary apt infrastructure -- it is
// what mirrors.ubuntu.com and the httpredir generation of Debian services
// exist to do -- and refusing it would break legitimate targets to close
// nothing. §3.1 is the reason: the host set is already whatever the snapshot
// names, and a snapshot that wanted to name evil.example could simply name
// it. What the allow-list removes is the set of *protocols* an untrusted file
// can steer this process into; the host set was never bounded and pretending
// otherwise here would be a check that costs real users and buys an attacker
// nothing.
func igCheckIndexRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= igMaxIndexRedirects {
		return fmt.Errorf("stopped after %d redirects", igMaxIndexRedirects)
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if !igIndexSchemes[scheme] {
		return fmt.Errorf("refusing a redirect to %s: the catalogue fetches over http and https only", scheme+":")
	}
	if len(via) > 0 && strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && scheme == "http" {
		return fmt.Errorf("refusing a redirect from https to http for %s: an archive does not get to downgrade a fetch the operator's own sources asked to protect", req.URL.Host)
	}
	return nil
}

// packagesDefaultClient is shared so that connections to one archive are
// pooled across index fetches rather than renegotiating TLS per file. The
// timeout is per request and generous: `universe` is 19 MB and an operator on
// a slow link is the normal case, not the exceptional one.
// CheckRedirect is igCheckIndexRedirect, shared with the DEP-11 client: an
// archive may not redirect an index fetch off http/https, and may not
// downgrade https to http. See igCheckIndexRedirect for what is deliberately
// still allowed and why.
var packagesDefaultClient = &http.Client{
	Timeout:       10 * time.Minute,
	CheckRedirect: igCheckIndexRedirect,
}

func (l PackagesLoader) userAgent() string {
	if l.UserAgent != "" {
		return l.UserAgent
	}
	return PackagesUserAgent
}

func (l PackagesLoader) maxIndexBytes() int64 {
	if l.MaxIndexBytes > 0 {
		return l.MaxIndexBytes
	}
	return packagesDefaultMaxIndexBytes
}

func (l PackagesLoader) limits() packagesLimits {
	lim := packagesDefaultLimits()
	if l.MaxDecompressedBytes != 0 {
		lim.Decompressed = l.MaxDecompressedBytes
	}
	if l.MaxEntries != 0 {
		lim.Entries = l.MaxEntries
	}
	return lim
}

// packagesCountingReader feeds the download byte counter and gives
// cancellation a check on every read, so a cancelled build stops at the next
// buffer rather than at the end of a 19 MB transfer.
type packagesCountingReader struct {
	ctx  context.Context
	r    io.Reader
	rep  *packagesReporter
	item string
}

func (c *packagesCountingReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.rep.addBytes(int64(n), c.item)
	}
	return n, err
}

// packagesParseBody decompresses one downloaded index and parses it, under
// lim.
//
// The limits are applied here rather than inside ParsePackagesIndex because
// that function is exported and takes a reader: a caller who has already
// decided what to hand it has already decided how big it is. Here the body
// came off a socket from a host a snapshot named, and the expansion factor is
// the archive's choice, not ours.
func packagesParseBody(ctx context.Context, body []byte, ref IndexRef, lim packagesLimits, emit func(Entry)) (PackagesParseStats, error) {
	var stats PackagesParseStats
	url := ref.URL()

	var src io.Reader = bytes.NewReader(body)
	// The published file is gzip, but an intermediary that decompresses on
	// the operator's behalf is a real deployment (some corporate proxies do
	// it). Sniffing the two magic bytes costs nothing and turns a baffling
	// "invalid header" into a working catalogue.
	compressed := false
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return stats, &PackagesFetchError{Ref: ref, URL: url, Err: fmt.Errorf("reading gzip: %w", err)}
		}
		defer func() { _ = gz.Close() }()
		src = gz
		compressed = true
	}

	// Only a compressed body needs the decompressed cap: an uncompressed one
	// was already bounded by packagesGet's io.LimitReader on the way in, and
	// wrapping it a second time would report the wrong limit in the error.
	if compressed && lim.Decompressed > 0 {
		src = &packagesLimitReader{r: src, limit: lim.Decompressed, ref: ref, url: url}
	}

	// emit has no error channel — it is called from inside the scanner loop —
	// so the entry cap raises a flag and stops handing entries over, and the
	// flag is turned into a PackagesFetchError once the parse returns. The
	// caller then sees one error shape however the limit was reached.
	emitted, overEntries := int64(0), false
	counted := emit
	if lim.Entries > 0 {
		counted = func(e Entry) {
			emitted++
			if emitted > lim.Entries {
				overEntries = true
				return
			}
			emit(e)
		}
	}

	stats, err := ParsePackagesIndex(ctx, src, ref, counted)
	if err != nil {
		return stats, err
	}
	if overEntries {
		return stats, &PackagesFetchError{
			Ref: ref, URL: url,
			Err: fmt.Errorf("index declares more than %d package stanzas, which no real archive component does", lim.Entries),
		}
	}
	return packagesParseVerdict(stats, body, ref, url)
}

// packagesLimitReader fails the parse when an index expands past limit,
// mirroring dep11CountingReader. It reports through PackagesFetchError so the
// caller sees the same shape a truncated download produces.
type packagesLimitReader struct {
	r     io.Reader
	n     int64
	limit int64
	ref   IndexRef
	url   string
}

func (c *packagesLimitReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, &PackagesFetchError{
			Ref: c.ref, URL: c.url,
			Err: fmt.Errorf("index expands to more than %d bytes, which no real archive index does", c.limit),
		}
	}
	return n, err
}

// packagesParseVerdict is the "arrived, decompressed, yielded nothing" check,
// unchanged and lifted out so both parse paths share it.
func packagesParseVerdict(stats PackagesParseStats, body []byte, ref IndexRef, url string) (PackagesParseStats, error) {
	// A file that arrived, decompressed, contained something, and yielded no
	// packages is not an empty component: it is a file this parser could not
	// read — an error page a mirror served as 200, or a format nobody here
	// has seen. Surfacing it is the point; catalogue-sourcing.md's one
	// unacceptable failure mode is the silently incomplete catalogue, because
	// it is the only one an operator cannot see.
	//
	// The condition is deliberately "had structure but no packages" rather
	// than "had bytes": an archive may legitimately publish an empty (or
	// blank-line-only) Packages file for a component with nothing in it for
	// this architecture, and that is a shorter catalogue, not a failure.
	if stats.Emitted == 0 && (stats.Stanzas > 0 || stats.MalformedLines > 0) {
		return stats, &PackagesFetchError{
			Ref: ref, URL: url,
			Err: fmt.Errorf("index carried %d bytes but no readable package stanzas (%d unreadable lines, %d nameless stanzas)",
				len(body), stats.MalformedLines, stats.SkippedStanzas),
		}
	}
	return stats, nil
}

// ParsePackagesIndex reads one decompressed `Packages` stream and calls emit
// once per binary package stanza.
//
// It is exported because it is the useful unit to test and to reuse: given a
// reader it needs no network, and the ref supplies only the Suite, Component
// and Arch that the stanzas themselves do not carry.
//
// emit receives an Entry by value and must not retain the slices it is handed
// — there are none; every string in an Entry is freshly allocated and owned by
// the callee.
//
// The parser skips unknown fields silently and by design. The real archive
// carries 64 distinct field names including vendor-specific ones like
// Ubuntu-Oem-Kernel-Flavour and Postgresql-Catversion, and a derivative's
// snapshot will carry names nobody here has seen
// (docs/dev/index-formats.md §2.2), so an allow-list that errors on the rest
// would fail on data apt reads happily.
func ParsePackagesIndex(ctx context.Context, r io.Reader, ref IndexRef, emit func(Entry)) (PackagesParseStats, error) {
	var stats PackagesParseStats

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, packagesScanBufferInitial), packagesScanBufferMax)

	var (
		cur              Entry
		hasFields        bool
		field            = pkgFieldSkip
		since            int64
		descriptionBytes int
		description      strings.Builder
	)

	flush := func() {
		if !hasFields {
			return
		}
		cur.Description = description.String()
		stats.Stanzas++
		if !pkgUsableName(cur.Name) {
			stats.SkippedStanzas++
		} else {
			// Suite and Component are properties of the index file, not of
			// the stanza: no Packages stanza states which suite it came from,
			// and Entry documents both as "which one supplied this row".
			cur.Suite = ref.Suite
			cur.Component = ref.Component
			if cur.Arch == "" {
				cur.Arch = ref.Arch
			}
			cur.Section = pkgStripComponent(cur.Section, ref.Component)
			if descriptionBytes+len(cur.Description) > packagesMaxDescriptionsBytes {
				cur.Description = ""
				cur.DescriptionTruncated = true
			}
			descriptionBytes += len(cur.Description)
			stats.Emitted++
			emit(cur)
		}
		cur = Entry{}
		description.Reset()
		hasFields = false
		field = pkgFieldSkip
	}

	for sc.Scan() {
		// bufio.ScanLines has already dropped a trailing \r, which covers the
		// CRLF case docs/dev/index-formats.md §4.6 warns about for
		// operator-supplied snapshots.
		line := sc.Bytes()

		if len(line) == 0 {
			flush()
			since++
			if since >= packagesCancelCheckEvery {
				since = 0
				if err := ctx.Err(); err != nil {
					return stats, fmt.Errorf("catalog: reading %s: %w", ref.ID(), err)
				}
			}
			continue
		}

		// A continuation line. This branch has to exist even though not one
		// field this parser keeps is ever folded in a real Packages file,
		// because a continuation of a field it SKIPS still must not be read
		// as a new field: Debian folds Tag: across lines and its values
		// contain "::", so a parser that split every line on the first colon
		// would invent a field called "uitoolkit" (§4.1).
		if line[0] == ' ' || line[0] == '\t' {
			if field == pkgFieldDescription {
				pkgAppendDescription(&cur, &description, string(bytes.TrimRight(line[1:], " \t\r")))
			} else if field != pkgFieldSkip {
				pkgAppendFold(&cur, field, string(bytes.TrimRight(line[1:], " \t\r")))
			}
			continue
		}

		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			stats.MalformedLines++
			// Do not let a garbage line silently extend the previous field.
			field = pkgFieldSkip
			continue
		}

		hasFields = true
		name := line[:colon]
		value := bytes.TrimRight(bytes.TrimLeft(line[colon+1:], " \t"), " \t\r")
		field = pkgFieldOf(name)
		if field == pkgFieldDescription {
			description.Reset()
		}
		pkgSetField(&cur, field, value)
	}

	if err := sc.Err(); err != nil {
		// The most likely error here is bufio.ErrTooLong, and if it ever
		// appears it means packagesScanBufferMax is too small for something
		// the archive now ships. Say so plainly rather than reporting a
		// half-read index as a short catalogue.
		return stats, &PackagesFetchError{Ref: ref, URL: ref.URL(), Err: fmt.Errorf("reading index: %w", err)}
	}
	// The last stanza in a file need not be followed by a blank line (§2).
	flush()

	if err := ctx.Err(); err != nil {
		return stats, fmt.Errorf("catalog: reading %s: %w", ref.ID(), err)
	}
	return stats, nil
}

// pkgField is which of the nine fields an Entry keeps a line belongs to.
//
// It is an integer rather than the field name so that the parser never builds
// a map per stanza. That is not a micro-optimisation: the measured cost of
// keeping a map[string]string per stanza across a real Ubuntu catalogue is
// 235 MB against a 400 MB budget (docs/dev/index-formats.md §5).
type pkgField uint8

const (
	pkgFieldSkip pkgField = iota
	pkgFieldPackage
	pkgFieldVersion
	pkgFieldArchitecture
	pkgFieldSection
	pkgFieldPriority
	pkgFieldDescription
	pkgFieldHomepage
	pkgFieldInstalledSize
	pkgFieldSize
)

// pkgFieldOf maps a field name to its slot, case-insensitively.
//
// Field names are case-insensitive by specification. The archive is perfectly
// consistent about them, but a snapshot's sources are operator-supplied and
// §2 says not to rely on it for input this application did not generate.
// Switching on length first keeps this to at most three byte comparisons per
// line, which matters at roughly five million lines per build.
func pkgFieldOf(name []byte) pkgField {
	switch len(name) {
	case 4:
		if bytes.EqualFold(name, pkgNameSize) {
			return pkgFieldSize
		}
	case 7:
		switch {
		case bytes.EqualFold(name, pkgNamePackage):
			return pkgFieldPackage
		case bytes.EqualFold(name, pkgNameVersion):
			return pkgFieldVersion
		case bytes.EqualFold(name, pkgNameSection):
			return pkgFieldSection
		}
	case 8:
		switch {
		case bytes.EqualFold(name, pkgNamePriority):
			return pkgFieldPriority
		case bytes.EqualFold(name, pkgNameHomepage):
			return pkgFieldHomepage
		}
	case 11:
		if bytes.EqualFold(name, pkgNameDescription) {
			return pkgFieldDescription
		}
	case 12:
		if bytes.EqualFold(name, pkgNameArchitecture) {
			return pkgFieldArchitecture
		}
	case 14:
		if bytes.EqualFold(name, pkgNameInstalledSize) {
			return pkgFieldInstalledSize
		}
	}
	return pkgFieldSkip
}

var (
	pkgNameSize          = []byte("Size")
	pkgNamePackage       = []byte("Package")
	pkgNameVersion       = []byte("Version")
	pkgNameSection       = []byte("Section")
	pkgNamePriority      = []byte("Priority")
	pkgNameHomepage      = []byte("Homepage")
	pkgNameDescription   = []byte("Description")
	pkgNameArchitecture  = []byte("Architecture")
	pkgNameInstalledSize = []byte("Installed-Size")
)

// pkgSetField copies one field value into the entry being built.
//
// Translation-en is not downloaded by the catalogue. Only description text
// already supplied by the Packages stream is retained.
func pkgSetField(e *Entry, f pkgField, v []byte) {
	switch f {
	case pkgFieldPackage:
		e.Name = string(v)
	case pkgFieldVersion:
		// Display only. Nothing in this repository compares two of these;
		// see packagesMerger.add.
		e.Version = string(v)
	case pkgFieldArchitecture:
		e.Arch = string(v)
	case pkgFieldSection:
		// The component prefix is stripped at flush time, when the ref's
		// component is in scope.
		e.Section = string(v)
	case pkgFieldPriority:
		e.Priority = string(v)
	case pkgFieldDescription:
		// Keep list/search text as the first line even when a long body follows.
		e.Summary = string(v)
		e.Description = ""
		e.DescriptionTruncated = false
	case pkgFieldHomepage:
		e.Homepage = string(v)
	case pkgFieldInstalledSize:
		// KiB, not bytes, and absent on 0.20% of real stanzas. Absent stays
		// zero, which Entry documents as "unknown"; a parse failure does the
		// same rather than failing the stanza.
		e.InstalledSizeKiB = pkgParseInt(v)
	case pkgFieldSize:
		e.DownloadSizeBytes = pkgParseInt(v)
	case pkgFieldSkip:
	}
}

// pkgAppendFold extends a kept field with a continuation line.
//
// Values are joined with "\n" and the single leading whitespace character is
// stripped, which is deb822's own rule. No field kept here folds in the real
// corpus — the only folded fields measured are Tag, X-Cargo-Built-Using and
// Description-en in Translation-en, none of which reach an Entry — but the
// general case is implemented because "it never happens in the two archives
// someone measured" is not the same as "it never happens".
//
// Description preserves paragraph breaks and preformatted indentation in a
// separate bounded field. It never extends the first-line Summary.
func pkgAppendFold(e *Entry, f pkgField, text string) {
	switch f {
	case pkgFieldVersion:
		e.Version += "\n" + text
	case pkgFieldArchitecture:
		e.Arch += "\n" + text
	case pkgFieldSection:
		e.Section += "\n" + text
	case pkgFieldPriority:
		e.Priority += "\n" + text
	case pkgFieldHomepage:
		e.Homepage += "\n" + text
	case pkgFieldPackage, pkgFieldDescription, pkgFieldInstalledSize, pkgFieldSize, pkgFieldSkip:
		// Nothing to extend: a numeric field's continuation is meaningless, a
		// package NAME has no second line at all.
		//
		// Folding the name used to be implemented along with the rest, and it
		// turned a stanza no archive would publish -- but an operator-supplied
		// snapshot or a hostile mirror could -- into an Entry called
		// "hello\nevil". Entry.Name is not a label: it is the cache's
		// binary-search key, the tray's identity, one element of the argv a
		// build runs ("apt:"+name), and a line in the packages.txt-shaped list
		// the CLI can be handed instead. Taking the first line and ignoring
		// the rest is also what apt does with a field it does not expect
		// folded.
	}
}

// A builder keeps even a hostile description made of thousands of tiny lines
// linear in size. Repeated string concatenation would copy the whole prefix
// on every line despite the final size bound.
func pkgAppendDescription(e *Entry, description *strings.Builder, text string) {
	if e.DescriptionTruncated {
		return
	}
	if description.Len() == 0 {
		description.WriteString(cacheClip(e.Summary, packagesMaxDescriptionBytes))
	}
	if text == "." {
		text = ""
	}
	remaining := packagesMaxDescriptionBytes - description.Len()
	if remaining <= len(text) {
		e.DescriptionTruncated = true
		if remaining > 0 {
			description.WriteString(cacheClip("\n"+text, remaining))
		}
		return
	}
	description.WriteByte('\n')
	description.WriteString(text)
}

// pkgUsableName reports whether a Package: value is a name the rest of the
// application can carry, rather than merely a non-empty string.
//
// This is not a Debian-policy validator and must not become one. Derivatives
// ship names nobody here has seen, and §2.2's rule is that an allow-list which
// errors on the rest fails on data apt reads happily. The question asked here
// is narrower and structural: a control character makes a name unusable as a
// key, as a line and as an argument. A newline becomes two lines in a
// line-oriented package list and in the command rule 8 requires the UI to show
// before the operator agrees to it; os/exec refuses an argument containing NUL
// outright.
//
// What is honestly at stake, since overstating it would be the wrong record to
// leave: internal/app's validPackageName already refuses every one of these at
// the moment a row is added to the selection tray, so none of them reaches an
// argv today. The defect this guards is one layer earlier and is a real one —
// a row the picker offers, that the application then refuses to accept, with a
// message telling the operator that what they clicked is not a package name.
// The guard here is also the layer that does not depend on the tray staying
// strict.
//
// A stanza whose name fails this is dropped and counted in SkippedStanzas,
// which is exactly what already happens to a stanza with no name at all.
// Offering a row that cannot be selected is the worse of the two answers, and
// if a whole index is like this, packagesParseVerdict still reports it as
// unreadable rather than as an empty component.
func pkgUsableName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		// Bytes rather than runes: every character being excluded is ASCII,
		// and the multi-byte sequences a range loop would decode are exactly
		// the ones this must not reject. DEL is included because it is a
		// control character that is neither printable nor typeable.
		if c := name[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return strings.TrimSpace(name) != ""
}

// pkgParseInt reads a non-negative decimal field, returning 0 for anything it
// cannot read. A malformed Installed-Size is a missing size, not a broken
// catalogue.
func pkgParseInt(v []byte) int64 {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// pkgStripComponent removes the archive's "component/" prefix from a Section:
// value: "universe/net" becomes "net".
//
// Measured: Ubuntu main uses bare sections and universe prefixes every one of
// them, so main+universe carries 99 distinct Section values that collapse to
// about 58 real ones (docs/dev/index-formats.md §4.5). Entry.Section documents
// the stripped form as its contract, because the prefix only restates
// Entry.Component and leaving it in would split one sidebar row into as many
// rows as there are components — which is search.go's grouping scrambled.
//
// The ref's own component is stripped first; anything else before a slash is
// stripped as a fallback, since an archive that names its component
// differently from its section prefix is still describing the same thing.
func pkgStripComponent(section, component string) string {
	if section == "" {
		return ""
	}
	if component != "" {
		if rest, ok := strings.CutPrefix(section, component+"/"); ok {
			return rest
		}
	}
	if i := strings.LastIndexByte(section, '/'); i >= 0 {
		return section[i+1:]
	}
	return section
}

// packagesMerger accumulates entries across indexes, one row per package name.
type packagesMerger struct {
	entries          []Entry
	slot             map[string]int
	duplicates       int64
	descriptionBytes int
}

func newPackagesMerger(compressedBytes int64) *packagesMerger {
	n := int(compressedBytes / packagesBytesPerStanza)
	if n < 64 {
		n = 64
	}
	if n > packagesMaxPrealloc {
		n = packagesMaxPrealloc
	}
	return &packagesMerger{entries: make([]Entry, 0, n), slot: make(map[string]int, n)}
}

// add applies the duplicate-name rule: THE LAST STANZA READ WINS.
//
// Read this before changing it. Package names are not unique, either within
// one index or across the several a target enables. Measured: Ubuntu noble
// universe carries android-platform-frameworks-native-headers twice, at
// 1:10.0.0+r36-1 and 1:34.0.4-1build3; Debian bookworm main carries linux-doc,
// linux-doc-6.1, linux-source and linux-source-6.1 twice each
// (docs/dev/index-formats.md §4.4).
//
// apt resolves that by comparing versions. THIS REPOSITORY MAY NOT.
// Contract-brief rule 1 forbids comparing two version strings to decide
// anything, and "keep the newer one" is precisely that decision — it is also
// wrong on its own terms, because the version apt would actually install
// depends on pin priorities and suite order that this package cannot see.
//
// So the rule here is positional and nothing else: whichever stanza is read
// last replaces whichever was read before it, where "last" means later in the
// file, and files are read in Target.IndexRefs order — which is the target's
// own source order. That is exactly what Entry.Version's contract already
// promises: "when two sources carry the same name, this is simply the one from
// the source the target listed later". It happens to select the newer version
// in both measured cases; that is a coincidence of how archives are written
// and must never be relied on or described as the intent.
//
// The row keeps its first-seen position so that the catalogue does not
// reshuffle because a duplicate appeared; only its contents are replaced.
func (m *packagesMerger) add(e Entry) {
	i, duplicate := m.slot[e.Name]
	if duplicate {
		m.descriptionBytes -= len(m.entries[i].Description)
	}
	if m.descriptionBytes+len(e.Description) > packagesMaxDescriptionsBytes {
		e.Description = ""
		e.DescriptionTruncated = true
	}
	m.descriptionBytes += len(e.Description)
	if duplicate {
		m.entries[i] = e
		m.duplicates++
		return
	}
	m.slot[e.Name] = len(m.entries)
	m.entries = append(m.entries, e)
}

// packagesReporter serialises progress callbacks from the concurrent download
// phase and throttles the byte counter.
type packagesReporter struct {
	fn    func(Progress)
	start time.Time
	total int64

	bytesDone  atomic.Int64
	bytesTotal atomic.Int64
	done       atomic.Int64

	mu   sync.Mutex
	last time.Time
}

func (r *packagesReporter) addTotal(n int64) { r.bytesTotal.Add(n) }

func (r *packagesReporter) finishOne(item string) {
	done := r.done.Add(1)
	r.send(Progress{
		Phase:      PhaseDownload,
		Label:      fmt.Sprintf("Downloading package lists — %d of %d", done, r.total),
		Item:       item,
		Current:    done,
		Total:      r.total,
		BytesDone:  r.bytesDone.Load(),
		BytesTotal: r.bytesTotal.Load(),
	}, true)
}

func (r *packagesReporter) addBytes(n int64, item string) {
	done := r.bytesDone.Add(n)
	r.send(Progress{
		Phase:      PhaseDownload,
		Label:      fmt.Sprintf("Downloading package lists — %d of %d", r.done.Load(), r.total),
		Item:       item,
		Current:    r.done.Load(),
		Total:      r.total,
		BytesDone:  done,
		BytesTotal: r.bytesTotal.Load(),
	}, false)
}

// send fills in the fields every Progress shares and delivers it. Unforced
// reports are dropped if one went out less than packagesProgressInterval ago;
// forced ones — a phase boundary, a finished file — always go.
func (r *packagesReporter) send(p Progress, force bool) {
	if r == nil || r.fn == nil {
		return
	}
	r.mu.Lock()
	now := time.Now()
	if !force && now.Sub(r.last) < packagesProgressInterval {
		r.mu.Unlock()
		return
	}
	r.last = now
	r.mu.Unlock()

	p.PhaseIndex = p.Phase.Index()
	p.PhaseCount = len(Phases)
	p.Elapsed = now.Sub(r.start)
	if p.Label == "" {
		// Progress.Label is documented as never empty: the UI's contract is
		// that it can always say what it is waiting on.
		p.Label = p.Phase.Label()
	}
	r.fn(p)
}

// packagesItem renders a ref the way Progress.Item is documented to look:
// "noble/universe/binary-amd64/Packages.gz".
func packagesItem(r IndexRef) string {
	return strings.TrimPrefix(r.Path(), "dists/")
}

func packagesPlural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
