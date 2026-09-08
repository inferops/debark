// Package catalog answers one question: what can the operator install on this
// target?
//
// It is a browsing index, built from the target's own apt indexes — the
// `Packages` files the target's sources point at, enriched with the DEP-11
// metadata (`Components-<arch>.yml.gz`) that carries real application names,
// summaries and freedesktop categories. It exists to remove the blank
// `packages.txt` problem: an operator who does not already know that the
// package is called `libreoffice-writer` cannot type it.
//
// # The catalogue decides nothing
//
// This is the load-bearing rule of the package and every signature below is
// shaped by it. Nothing here resolves a dependency, compares two version
// strings, or reasons about what supersedes what. That is the `debark`
// engine's job, apt is its oracle, and a second implementation that can
// disagree with the first is two products. A version string that reaches this
// package is a label printed next to a name; the version that actually gets
// installed is decided by apt, inside debark, at build time, from the
// snapshot — long after anything here has been forgotten.
//
// The practical consequence is that a wrong answer from this package is a
// cosmetic defect, never a correctness one. The cache may be stale, the
// displayed version may not be the one the build picks, and neither can
// produce a short bundle. That asymmetry is why the cache is allowed to be
// fast and unverified (docs/dev/cache-format.md) rather than a trust boundary.
//
// # Size is the other constraint
//
// Ubuntu `main` + `universe` is roughly 70,000 binary packages. Every choice
// here follows from that number: `Search` returns a `Page` and never a slice
// of everything, `Page` carries `Total` so the virtualiser can size a
// scrollbar it will never fill, `Build` reports `Progress` because the first
// run is measured in tens of seconds, and the parsed result is cached on disk
// because the second run must be under 500 ms.
//
// # Lifecycle
//
// A Catalog value is bound to one target. `Build` and `Ready` take a Target
// because they may be called before anything is loaded; `Search`, `Categories`
// and `Get` do not, because by the time they are useful the catalogue for one
// target is open. A `Search` on a Catalog with nothing loaded returns
// ErrNotBuilt — it does not silently return an empty page, because "no
// results" and "no catalogue" are different things on screen.
//
// Construction lives in catalog.go; the on-disk cache lives in cache.go;
// `Packages` parsing in packages.go and DEP-11 in dep11.go. This file is the
// contract all four and the whole frontend compile against, and it does not
// change.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Catalog answers "what can I install on this target?" It is a browsing
// index built from the target's own apt indexes. It decides nothing.
//
// Implementations must be safe for concurrent use by multiple goroutines.
// The Wails bridge calls bound methods from whichever goroutine handles the
// request, and the frontend fires a Search on every keystroke while a build
// may still be running.
type Catalog interface {
	// Build fetches and parses the target's indexes, reporting progress.
	// Cancellable. Populates the on-disk cache.
	//
	// progress may be nil. When it is not, it is called from Build's own
	// goroutine, synchronously, and must not block: it is on the path of a
	// job the operator is watching. Deliver to the UI by posting an event,
	// not by doing work in the callback.
	//
	// Cancelling ctx aborts the build and returns an error satisfying
	// errors.Is(err, context.Canceled). A cancelled build leaves no partial
	// cache behind that a later load could mistake for a complete one; see
	// docs/dev/cache-format.md. On success the built catalogue is loaded and
	// Search may be called immediately, without a second trip to disk.
	Build(ctx context.Context, t Target, progress func(Progress)) error

	// Ready reports whether a usable cache exists for this target, so the UI
	// can skip straight to the picker.
	//
	// It is the first thing the target screen calls after the operator picks
	// a target, so it must be cheap — a stat and a small header read, never a
	// full load and never a network round-trip. false with a nil error is the
	// ordinary "not built yet" answer and is not a failure; a non-nil error
	// means the cache directory itself could not be consulted (permissions,
	// unreadable home) and is worth showing.
	//
	// Ready true does not promise fresh. A cache whose index digest no longer
	// matches the archive is still perfectly usable for browsing; freshness is
	// reported separately by the cache header so the UI can offer a rebuild
	// without blocking on one.
	Ready(t Target) (bool, error)

	// Search returns one page. Runs entirely in Go. Must meet the < 100 ms
	// p95 budget.
	//
	// The budget includes the Wails bridge round-trip, which is why the page
	// is small and Total is a number rather than a slice. q is normalised
	// before use (see Query.Normalized), so a zero Limit is a sane default
	// rather than an empty page.
	//
	// Ordering is fixed and defined by the contract, not by the caller: rows
	// whose Name or AppName matched come before rows where only the Summary
	// matched, and within each group rows are ordered by Name ascending.
	// Paging is stable as long as the underlying catalogue does not change, so
	// a virtualiser may request arbitrary offsets in any order.
	//
	// Returns ErrNotBuilt when no catalogue is loaded for this Catalog.
	Search(ctx context.Context, q Query) (Page, error)

	// Categories returns the two-tier grouping: DEP-11 freedesktop categories
	// for applications, apt Section: for everything else.
	//
	// The full list, not a page: there are tens of these, not tens of
	// thousands, and the sidebar renders all of them. Counts reflect the whole
	// catalogue, unfiltered by any query. Ordering is tier first
	// (applications, then sections), then Name ascending, so the sidebar does
	// not reshuffle between calls.
	Categories(ctx context.Context) ([]Category, error)

	// Get returns one entry by exact binary package name.
	//
	// The bool is false, with a nil error, when the target's indexes simply do
	// not carry that name — the ordinary answer for a package the operator
	// typed or a name that came from somewhere else. An error means the
	// lookup could not be performed at all.
	Get(ctx context.Context, name string) (Entry, bool, error)

	// Close releases whatever the catalogue holds open — the mapped cache
	// file, a partially written temporary — and makes every subsequent call
	// return ErrClosed. It is safe to call more than once.
	Close() error
}

// Sentinel errors the UI must be able to tell apart. Every one of them has a
// different thing for the operator to do next, which is the test for whether
// a condition deserves a sentinel at all. Compare with errors.Is; an
// implementation may wrap these with detail.
var (
	// ErrNotBuilt means there is no catalogue for this target yet. The UI
	// shows the "build the catalogue" call to action, not an error banner:
	// this is the expected state the first time an operator picks a target.
	ErrNotBuilt = errors.New("catalog: not built for this target")

	// ErrStale means a cache exists and was loaded, but the archive has
	// published new indexes since it was written — its recorded index digest
	// no longer matches the target's. The catalogue remains usable; the UI
	// offers a refresh rather than blocking on one. Nothing incorrect can
	// follow from browsing a stale catalogue, because the build re-resolves
	// against the live archive regardless of what was on screen.
	ErrStale = errors.New("catalog: cache is stale for this target")

	// ErrCacheCorrupt means the cache file failed its own integrity check —
	// truncated, garbled, or written by a crash. The answer is always to
	// rebuild and never to crash, so callers should treat it exactly as they
	// treat ErrNotBuilt, plus a line in the log.
	ErrCacheCorrupt = errors.New("catalog: cache is corrupt")

	// ErrCacheBusy means the cache could not be read because another process
	// is committing one right now, and the reader ran out of patience waiting
	// for it. Nothing is wrong with the file: retrying works.
	//
	// It exists because the alternative was a lie. A cache is committed by two
	// renames — catalog.bin, then meta.json — and a reader that lands between
	// them sees a marker and an index that disagree, which is exactly what
	// ErrCacheCorrupt describes. LoadCacheDir already retried, already declined
	// to delete a directory it could see being written, and then reported
	// ErrCacheCorrupt anyway. So a second window browsing while the first
	// rebuilt could be told the cache was damaged and offered a rebuild it did
	// not need, over a file that was about to be perfectly good.
	//
	// This does not change §6.4's invalidation rule, which is about a QUIET
	// directory: a mismatch with no commit in flight is still ErrCacheCorrupt
	// and the directory is still removed. Only the case that already refused
	// to invalidate is renamed to what it actually is.
	ErrCacheBusy = errors.New("catalog: a cache is being written for this target")

	// ErrCacheVersion means the cache on disk was written by a different
	// format version of this application. Also a rebuild: the cache is
	// derived data and is never migrated. Distinguished from ErrCacheCorrupt
	// so a downgrade reads as an upgrade artefact rather than as disk damage.
	ErrCacheVersion = errors.New("catalog: unsupported cache format version")

	// ErrBuildInProgress means Build was called while a build for this
	// Catalog was already running. The UI's build button is disabled during a
	// build, so seeing this is a bug in the caller, not an operator error —
	// but it must be an error rather than a second concurrent download of
	// 60 MB of indexes.
	ErrBuildInProgress = errors.New("catalog: a build is already in progress")

	// ErrClosed means the Catalog has been closed. Distinguished from
	// ErrNotBuilt because it is always a lifecycle bug rather than a state
	// the operator can act on.
	ErrClosed = errors.New("catalog: closed")
)

// Cancellation is not a sentinel here on purpose: a cancelled Build returns an
// error satisfying errors.Is(err, context.Canceled), which is what every other
// package in the Go ecosystem already checks. The UI distinguishes "the
// operator pressed Cancel" from "the download failed" on exactly that test.

// CacheFormatVersion is the version of the on-disk cache layout this build
// reads and writes. A cache carrying any other value is rebuilt, never
// migrated; see docs/dev/cache-format.md, which is the normative description
// of the bytes and must be updated in the same commit that changes this
// number.
const CacheFormatVersion = 2

// CacheRoot returns the directory that holds every cached catalogue.
//
// It is defined here rather than in cache.go because it is contract, not
// implementation: the settings screen shows this path, a support request asks
// the operator to delete it, and the cache work must not be free to put it
// somewhere else. os.UserCacheDir does exactly the right thing on both
// platforms this app ships to — $XDG_CACHE_HOME, or ~/.cache when it is
// unset, on Linux; %LocalAppData% on Windows — so no per-OS branch is
// written here, and none should be added.
func CacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("catalog: locating the user cache directory: %w", err)
	}
	return filepath.Join(dir, "debark-gui", "catalog"), nil
}

// TargetKind says how the operator named the machine the catalogue is for.
//
// It is recorded because the two kinds carry different guarantees, exactly as
// snapshot.Origin does in the core: a snapshot was measured from a real
// machine, a base is an assumption about a machine that may not exist yet. The
// catalogue treats them identically — both reduce to a set of apt sources —
// but the UI says which one it is looking at, and the cache key must not let
// them collide.
type TargetKind string

const (
	// TargetBase is a stock base definition compiled into debark, named by
	// its id: "ubuntu:26.04/desktop". Mirrors snapshot.OriginSynthesized.
	TargetBase TargetKind = "base"
	// TargetSnapshot is a snapshot archive the operator chose from disk.
	// Mirrors snapshot.OriginCaptured.
	TargetSnapshot TargetKind = "snapshot"
)

// Source is one apt source as deb822 records it: the four fields that decide
// which index files exist. It is the catalogue's whole input.
//
// The field names are apt's own (Types, URIs, Suites, Components) rather than
// invented ones, because this value is populated by reading a target's
// sources.list.d verbatim — from a snapshot's captured APT.Sources, or from
// the Sources document on a base definition — and a translation layer between
// two vocabularies is a place for a bug to live.
//
// Signed-By is deliberately absent. The catalogue does not verify archive
// signatures, does not ship keyrings, and is not a trust boundary; debark
// verifies everything that is actually installed. Recording a keyring path
// here would imply otherwise.
type Source struct {
	// Types is deb, deb-src, or both. Only "deb" produces catalogue entries;
	// a deb-src-only stanza contributes nothing and is skipped.
	Types []string `json:"types"`
	// URIs are the archive roots, e.g. "http://archive.ubuntu.com/ubuntu".
	// A stanza may list more than one mirror; each is a separate place the
	// same indexes can be fetched from, so IndexRefs expands them all and the
	// fetcher may choose.
	URIs []string `json:"uris"`
	// Suites are the distributions, e.g. "noble", "noble-updates",
	// "noble-security". A suite ending in "/" is a flat repository with no
	// dists/ hierarchy; those are skipped by IndexRefs and documented as out
	// of scope, because a vendor's flat repo has neither components nor
	// DEP-11 and the operator reaches those .debs by URL instead.
	Suites []string `json:"suites"`
	// Components are the archive components, e.g. "main", "universe". They
	// are part of the cache key: enabling universe more than doubles the
	// catalogue, and a cache built without it must not be served for a target
	// that has it.
	Components []string `json:"components"`
}

// Target identifies the machine the catalogue is for, and is the input to the
// cache key.
//
// Field names follow debark's own vocabulary — snapshot.Target for the
// identity fields, base.ListEntry for the id — so that a value built from
// either source is a transcription rather than a translation.
type Target struct {
	// Kind is how the operator named this target.
	Kind TargetKind `json:"kind"`
	// BaseID is the base definition's id, "<distro>:<version>/<variant>",
	// e.g. "ubuntu:26.04/desktop". Set when Kind is TargetBase. Same value as
	// snapshot.Origin.BaseID and base.ListEntry.ID.
	BaseID string `json:"base_id,omitempty"`
	// SnapshotPath is the absolute path to the snapshot archive the operator
	// chose. Set when Kind is TargetSnapshot.
	//
	// It is part of the display identity but NOT part of the cache key: the
	// same snapshot copied to a second path must hit the same cache, and two
	// different snapshots at the same path must not. What distinguishes them
	// is the identity fields and Sources below, plus IndexDigest.
	SnapshotPath string `json:"snapshot_path,omitempty"`

	// DistroID, VersionID and Codename are the target identity, straight from
	// snapshot.Target: ID, VERSION_ID and VERSION_CODENAME of the target's
	// /etc/os-release. "ubuntu", "26.04", "resolute".
	DistroID  string `json:"distro_id"`
	VersionID string `json:"version_id"`
	Codename  string `json:"codename"`
	// PrettyName is the human string for the title bar, e.g.
	// "Ubuntu 26.04 LTS". Display only, and excluded from the cache key: a
	// prettier label must not orphan a 18 MB cache.
	PrettyName string `json:"pretty_name,omitempty"`
	// Arch is the dpkg architecture, e.g. "amd64". One catalogue is one
	// architecture: a base's sources and therefore its indexes differ per
	// architecture, so "what can I install" has no architecture-free answer.
	//
	// Foreign architectures (snapshot.Target.ForeignArchs) are deliberately
	// not browsed. An i386 co-installed library is something an operator names
	// explicitly, not something they find by scrolling, and indexing them
	// would make Get(name) ambiguous. Entry.Arch is therefore always Arch or
	// "all".
	Arch string `json:"arch"`

	// Sources is every apt source in play, in the order the target lists
	// them. Order is significant for display only: when two sources offer the
	// same package name, the later one supplies Entry.Version. See Entry.
	Sources []Source `json:"sources"`

	// IndexDigest is what changes when the underlying index content changes.
	// It is the SHA-256 over the archive's own recorded hashes of exactly the
	// index files this catalogue consumes — derived from each suite's
	// Release file, not by hashing tens of megabytes of Packages — so it can
	// be recomputed from a few kilobytes. docs/dev/cache-format.md pins the
	// exact derivation, and cache.go is the only thing that computes it.
	//
	// It is empty on a Target that has not talked to the archive yet, which
	// is the normal state on the target screen. Empty means "unknown", never
	// "no indexes": Ready must not treat an empty digest as a mismatch, and
	// nothing may key a cache directory on it. Staleness is a comparison made
	// when the value is known, and a shrug when it is not.
	IndexDigest string `json:"index_digest,omitempty"`
}

// Validate reports whether the target carries enough to build a catalogue for.
// It checks shape, not truth: it cannot know whether an archive exists.
func (t Target) Validate() error {
	switch t.Kind {
	case TargetBase:
		if t.BaseID == "" {
			return errors.New("catalog: base target has no base id")
		}
	case TargetSnapshot:
		if t.SnapshotPath == "" {
			return errors.New("catalog: snapshot target has no snapshot path")
		}
	default:
		return fmt.Errorf("catalog: unknown target kind %q", t.Kind)
	}
	if t.DistroID == "" || t.VersionID == "" {
		return errors.New("catalog: target has no distro id or version id")
	}
	if t.Arch == "" {
		return errors.New("catalog: target has no architecture")
	}
	if len(t.IndexRefs()) == 0 {
		return errors.New("catalog: target has no usable binary apt sources")
	}
	return nil
}

// Suites returns every suite the target's sources name, deduplicated and
// sorted. For display, and for the cache key.
func (t Target) Suites() []string {
	return uniqueSorted(t.Sources, func(s Source) []string { return s.Suites })
}

// Components returns every component the target's sources name, deduplicated
// and sorted. For display, and for the cache key — components are the single
// biggest lever on catalogue size, so a cache built for one set must never be
// served for another.
func (t Target) Components() []string {
	return uniqueSorted(t.Sources, func(s Source) []string { return s.Components })
}

// Display is the one-line name for this target in the UI.
func (t Target) Display() string {
	name := t.PrettyName
	if name == "" {
		name = strings.TrimSpace(t.DistroID + " " + t.VersionID)
	}
	if name == "" {
		name = "unknown target"
	}
	return name + " (" + t.Arch + ")"
}

// Identity is the canonical text the cache key hashes: every fact that
// changes which packages exist, and nothing that does not.
//
// It is exported because it is the thing to print when two machines disagree
// about whether they have the same target, and because a test that pins the
// cache key needs to be able to say why the key changed. It is deliberately
// line-oriented plain text rather than JSON: it must be diffable by eye in a
// bug report, and it must not change because a struct field was reordered.
//
// PrettyName, SnapshotPath, BaseID and IndexDigest are all absent. The first
// two are cosmetic. BaseID is absent because a base and a snapshot describing
// the same release with the same sources genuinely are the same catalogue and
// should share one 18 MB file — Kind is absent for the same reason.
// IndexDigest is absent because it changes whenever the archive publishes,
// and keying a directory on it would leave a new copy of the catalogue on disk
// every day; freshness is recorded inside the cache instead.
func (t Target) Identity() string {
	var b strings.Builder
	fmt.Fprintf(&b, "catalog-identity/v%d\n", CacheFormatVersion)
	fmt.Fprintf(&b, "distro_id=%s\n", t.DistroID)
	fmt.Fprintf(&b, "version_id=%s\n", t.VersionID)
	fmt.Fprintf(&b, "codename=%s\n", t.Codename)
	fmt.Fprintf(&b, "arch=%s\n", t.Arch)
	// One line per concrete index, sorted: this is the set of files the
	// catalogue is made of, which is exactly what "the same catalogue" means.
	refs := t.IndexRefs()
	lines := make([]string, 0, len(refs))
	for _, r := range refs {
		lines = append(lines, "index="+r.ID())
	}
	sort.Strings(lines)
	lines = dedupe(lines)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// CacheKey is the stable, filesystem-safe directory name for this target's
// cache. Two Targets that would produce the same catalogue produce the same
// key; any difference that changes which packages exist changes it.
//
// The shape is "<distro>-<version>-<arch>-<12 hex>": readable enough that an
// operator clearing one release's cache by hand can see which directory is
// which, hashed enough that no archive URI, suite name or component list can
// smuggle a path separator, a drive letter or a dot-dot into a path this
// application then writes to.
func (t Target) CacheKey() string {
	sum := sha256.Sum256([]byte(t.Identity()))
	parts := []string{
		safeKeyPart(t.DistroID),
		safeKeyPart(t.VersionID),
		safeKeyPart(t.Arch),
		hex.EncodeToString(sum[:6]),
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "-")
}

// IndexKind is which of the two index families a reference points at.
type IndexKind string

const (
	// IndexPackages is dists/<suite>/<component>/binary-<arch>/Packages.gz —
	// the apt binary index. It is what makes an Entry exist at all.
	IndexPackages IndexKind = "packages"
	// IndexDEP11 is dists/<suite>/<component>/dep11/Components-<arch>.yml.gz —
	// AppStream metadata. It is what turns "libreoffice-writer" into
	// "LibreOffice Writer", with a category and a summary a human wrote. It is
	// optional: a component may not publish it, and a missing DEP-11 file is
	// not a build failure.
	IndexDEP11 IndexKind = "dep11"
)

// IndexRef names one index file to fetch: a single (archive, suite,
// component, architecture, kind) tuple.
//
// It exists on this side of the contract because two packages need to agree on
// it exactly — packages.go fetches the IndexPackages refs and dep11.go the
// IndexDEP11 ones — and because the set of refs is precisely what the cache
// key and the index digest are computed over. Deriving the paths twice is how
// the two packages come to disagree about which archive they read.
type IndexRef struct {
	// URI is the archive root, with no trailing slash.
	URI string `json:"uri"`
	// Suite, Component and Arch are the coordinates within it.
	Suite     string    `json:"suite"`
	Component string    `json:"component"`
	Arch      string    `json:"arch"`
	Kind      IndexKind `json:"kind"`
}

// Path is the ref's location relative to the archive root.
//
// Ubuntu and Debian both file arch:all packages into binary-<arch>/Packages
// alongside the native ones, so no separate binary-all fetch is issued; if a
// third-party archive publishes only binary-all, its arch:all packages are
// missed, which costs a row in a picker and nothing else.
//
// The compressed variant is named because it is the only one worth fetching:
// Packages.gz is roughly a fifth of Packages, the difference is tens of
// megabytes on the first run, and every archive that publishes one publishes
// the other. A fetcher that wants .xz may substitute the extension itself.
func (r IndexRef) Path() string {
	switch r.Kind {
	case IndexPackages:
		return "dists/" + r.Suite + "/" + r.Component + "/binary-" + r.Arch + "/Packages.gz"
	case IndexDEP11:
		return "dists/" + r.Suite + "/" + r.Component + "/dep11/Components-" + r.Arch + ".yml.gz"
	default:
		return ""
	}
}

// ReleasePath is the location of the suite's Release file, relative to the
// archive root. Every ref in a suite shares one, and it is where the recorded
// hashes that make up Target.IndexDigest come from.
//
// InRelease (the inline-signed form) is not used: this package does not verify
// signatures, so the detached Release is the smaller, simpler fetch. That is a
// statement about scope, not a shortcut — see the package comment.
func (r IndexRef) ReleasePath() string {
	return "dists/" + r.Suite + "/Release"
}

// URL is the absolute location to fetch.
func (r IndexRef) URL() string {
	if p := r.Path(); p != "" {
		return strings.TrimRight(r.URI, "/") + "/" + p
	}
	return ""
}

// ID is the ref's stable identity: the archive root and the repo-relative
// path, space-separated. It is what Identity hashes and what the index digest
// is computed over, so it must never include anything transient.
func (r IndexRef) ID() string {
	return strings.TrimRight(r.URI, "/") + " " + r.Path()
}

// IndexRefs expands the target's sources into every index file the catalogue
// is built from, in source order, deduplicated.
//
// Skipped, silently and by design: deb-src stanzas, stanzas with no URI, and
// flat repositories (a suite ending in "/", which has no dists/ hierarchy and
// therefore no components and no DEP-11). A flat vendor repo contributes
// nothing browsable; the operator reaches those .debs by URL, which is a
// different screen.
//
// Skipped and *not* silently: any URI whose scheme is not http or https. Use
// IndexRefsWithProblems to see those; this function drops them, so no caller
// can turn a cdrom: or file: URI into a request by forgetting to look.
func (t Target) IndexRefs() []IndexRef {
	refs, _ := t.IndexRefsWithProblems()
	return refs
}

// IndexRefsWithProblems is IndexRefs plus the sources it would not fetch from.
//
// The second return exists because "this target has 2 sources the catalogue
// cannot browse" is a sentence an operator needs and a dropped URI cannot
// produce on its own. The three Resolve functions append these to the problems
// they already return, so the existing rendering path carries them with no new
// plumbing anywhere.
//
// The scheme filter lives here rather than only at the point of parsing
// because this is the last place before a URL is built: IndexRef.URL is
// concatenation and the result goes straight into http.NewRequestWithContext.
// A Target assembled by hand — a test, a future caller, a cache round-trip —
// must not be able to reach the network with a scheme nobody allowed, and the
// only way to guarantee that is to filter where the refs are made. See
// igIndexSchemes in sources.go for what is on the list and why http is.
func (t Target) IndexRefsWithProblems() ([]IndexRef, []SourceProblem) {
	var refs []IndexRef
	var problems []SourceProblem
	seen := make(map[string]bool)
	// One problem per distinct rejected URI, not one per index file it would
	// have produced: the operator has one source to think about, and a stanza
	// with four components would otherwise report the same fact eight times.
	reported := make(map[string]bool)
	// A second map rather than a shared one with prefixed keys: a URI and a
	// ref id are different kinds of string and nothing is gained by making
	// one namespace hold both.
	reportedRefs := make(map[string]bool)
	for _, s := range t.Sources {
		if !hasType(s.Types, "deb") {
			continue
		}
		for _, raw := range s.URIs {
			uri := strings.TrimSpace(raw)
			if uri == "" {
				// Reported, not skipped. A URI that is empty or entirely
				// whitespace contributes nothing, and dropping it in silence
				// is the one failure sources.go's header calls unacceptable:
				// the picker is simply short a repository and nothing says
				// why. FuzzSecSourcesOneLine reached this with "dEB  0 0",
				// where the one-line parser splits on space and tab but not
				// on a vertical tab, so the vertical tab became the URI.
				if raw != "" && !reported[raw] {
					reported[raw] = true
					problems = append(problems, SourceProblem{
						Kind:   SourceProblemNoURI,
						Text:   srcClip(raw, srcMaxProblemText),
						Reason: "this source's archive URI is blank, so there is nothing to fetch from; its packages are not in this catalogue",
					})
				}
				continue
			}
			if scheme, malformed, ok := igFetchableURI(uri); !ok {
				if !reported[uri] {
					reported[uri] = true
					problems = append(problems, igUnfetchableURIProblem(uri, scheme, malformed))
				}
				continue
			}
			for _, suite := range s.Suites {
				suite = strings.TrimSpace(suite)
				if suite == "" || strings.HasSuffix(suite, "/") || strings.Contains(suite, "/") {
					continue
				}
				for _, comp := range s.Components {
					comp = strings.TrimSpace(comp)
					if comp == "" {
						continue
					}
					// A scheme and an authority are not enough: the suite and
					// the component become path segments, and a segment can
					// still make the finished URL unparseable. A component
					// named "%" produces ".../dists/0/%/binary-amd64/..." and
					// url.Parse refuses it with `invalid URL escape "%/b"`,
					// which is what http.NewRequestWithContext would then
					// return from inside the fetch -- a network-shaped error
					// for a malformed source, which is the exact complaint
					// docs/security-review.md §3.1 made about the local
					// schemes.
					//
					// So the finished URL is parsed here, with the same
					// package the HTTP client uses. Checked once per
					// suite/component rather than once per index kind,
					// because the operator has one line to fix and does not
					// need to be told about it twice.
					probe := IndexRef{URI: strings.TrimRight(uri, "/"), Suite: suite, Component: comp, Arch: t.Arch, Kind: IndexPackages}
					if _, err := url.Parse(probe.URL()); err != nil {
						if !reportedRefs[probe.ID()] {
							reportedRefs[probe.ID()] = true
							problems = append(problems, igUnusableRefProblem(suite, comp, err))
						}
						continue
					}
					for _, kind := range []IndexKind{IndexPackages, IndexDEP11} {
						r := IndexRef{URI: strings.TrimRight(uri, "/"), Suite: suite, Component: comp, Arch: t.Arch, Kind: kind}
						if seen[r.ID()] {
							continue
						}
						seen[r.ID()] = true
						refs = append(refs, r)
					}
				}
			}
		}
	}
	return refs, problems
}

// igUnfetchableURIProblem describes one archive URI the catalogue will not
// fetch from, in the vocabulary an operator can act on.
//
// The reason names the scheme and says what the consequence is, because the
// consequence is the part that matters: the packages behind that source are
// not in the picker, and the operator has to know that before they conclude
// the catalogue is complete.
//
// malformed splits the two cases, and the split is the whole point of having
// two kinds. An unsupported scheme is a documented scope limit — Deliberate
// reports true, and a UI shows it quietly alongside the deb-src stanzas it
// always skips. An http or https URI with no host is a broken line in the
// operator's own file, Deliberate reports false, and the UI shows it loudly,
// because it is the only one of the two they can fix.
func igUnfetchableURIProblem(uri, scheme string, malformed bool) SourceProblem {
	// The scheme is a fragment of the URI, so it comes from the file being
	// parsed and is sanitised before it is interpolated into a sentence a UI
	// renders. srcClip does the same for Text below.
	scheme = srcClip(scheme, srcMaxProblemText)
	if malformed {
		return SourceProblem{
			Kind:   SourceProblemMalformed,
			Text:   srcClip(uri, srcMaxProblemText),
			Reason: "this source's archive URI has a " + scheme + ": scheme but names no host, so there is nowhere to fetch its indexes from; its packages are not in this catalogue",
		}
	}
	var reason string
	switch scheme {
	case "":
		reason = "this source's archive URI names no scheme, so the catalogue cannot fetch its indexes; its packages are not in this catalogue"
	case "cdrom":
		reason = "this source is install media (cdrom:), which the catalogue does not read; its packages are not in this catalogue, but debark can still install from it on the target"
	case "file", "copy":
		reason = "this source is a local directory (" + scheme + ":), which the catalogue fetches nothing from; its packages are not in this catalogue"
	default:
		reason = "the catalogue fetches indexes over http and https only, and this source uses " + scheme + ":; its packages are not in this catalogue"
	}
	return SourceProblem{
		Kind:   SourceProblemUnsupportedScheme,
		Text:   srcClip(uri, srcMaxProblemText),
		Reason: reason,
	}
}

// igUnusableRefProblem describes a suite and component pair that cannot be
// turned into a URL.
//
// SourceProblemMalformed rather than a new kind: this is a broken value in the
// operator's own file, which is precisely what that kind already means, and
// Deliberate correctly reports false for it. Adding a kind here would give the
// UI a third thing to render for a case an operator acts on identically.
func igUnusableRefProblem(suite, component string, err error) SourceProblem {
	return SourceProblem{
		Kind:   SourceProblemMalformed,
		Text:   srcClip(suite+"/"+component, srcMaxProblemText),
		Reason: srcSanitize("this source's suite and component do not make a usable index URL (" + err.Error() + "); its packages are not in this catalogue"),
	}
}

// Entry is one selectable apt binary package, as the target's own index
// reports it.
//
// Extended descriptions stay compressed in the search index and are decoded
// only for Get. Search rows keep the first-line Summary and do not materialise
// a paragraph for every visible result.
type Entry struct {
	// Name is the binary package name — the string that goes in
	// packages.txt and on the debark command line. Unique within a
	// catalogue, and the key Get takes.
	Name string `json:"name"`

	// Version is a version string one of the target's indexes offers for this
	// package.
	//
	// DISPLAY ONLY. It is not apt's candidate and must never be presented as
	// one, because computing a candidate means comparing versions across
	// suites and applying pin priorities — engine work, forbidden here, and
	// already implemented correctly on the other side of the CLI seam. When
	// two sources carry the same name, this is simply the one from the source
	// the target listed later. What actually gets installed is decided by apt
	// inside debark at build time, from the snapshot, and may differ from
	// this string.
	//
	// No code in this repository may compare two of these. Not to sort, not
	// to pick, not to decide anything.
	Version string `json:"version"`

	// Suite is which suite supplied Version — "noble", "noble-updates". Shown
	// beside the version so a version that looks surprising explains itself,
	// and never used to rank anything.
	Suite string `json:"suite,omitempty"`
	// Component is the archive component the package lives in: "main",
	// "universe", "non-free". Worth a field of its own because it is the one
	// piece of catalogue metadata with a consequence downstream — debark
	// raises a redistribution flag for the restricted components — so the
	// picker can warn before the build does.
	Component string `json:"component,omitempty"`
	// Arch is the package's architecture: the target's arch, or "all".
	Arch string `json:"arch"`
	// Section is apt's Section: field — "net", "devel", "libs", "python" —
	// with the archive's "component/" prefix stripped: a stanza saying
	// "universe/net" becomes "net". The prefix restates Component, which has
	// its own field, and leaving it in would split one section into as many
	// sidebar rows as there are components (docs/dev/index-formats.md §4.5).
	//
	// It is the second tier of the category grouping, for everything DEP-11
	// does not know is an application — which is the large majority: DEP-11
	// covers about 3% of an Ubuntu archive, so this tier is the complete
	// index and the application tier is a shortcut into it.
	Section string `json:"section,omitempty"`
	// Priority is apt's Priority: field — "required", "important",
	// "standard", "optional", "extra". Detail-panel context, and a hint to
	// the operator that a package is part of the base system rather than
	// something to add. Never used to rank or to decide.
	Priority string `json:"priority,omitempty"`

	// Summary is the one-line description to show in a list row.
	//
	// When DEP-11 knows this package it is DEP-11's summary, which a human
	// wrote for an application catalogue; otherwise it is the first line of
	// the Packages Description: field. One field rather than two, because the
	// UI never wants both and a "which one do I show?" decision replicated in
	// every renderer is a decision that will be made differently in each.
	Summary string `json:"summary,omitempty"`
	// Description is the full text supplied by a Packages Description field,
	// including its first line. Empty means the index supplied only a summary.
	// It is materialised by Get, never by the production search/page path.
	Description string `json:"description,omitempty"`
	// DescriptionTruncated makes bounded metadata retention visible to callers.
	DescriptionTruncated bool `json:"description_truncated,omitempty"`
	// descriptionData is a bounded zlib stream, kept compressed between queries.
	descriptionData string
	// Homepage is the upstream URL from the Packages stanza, when it has one.
	// Detail panel only. It is the one long-ish field kept per row, because a
	// detail panel with nowhere to send the operator is a dead end and a
	// second pass over a 60 MB index to recover it is not worth the memory it
	// saves.
	Homepage string `json:"homepage,omitempty"`

	// InstalledSizeKiB is apt's Installed-Size: field, which is in KiB and not
	// bytes — the unit is in the name because the two are indistinguishable at
	// a glance and a factor of 1024 in a "this bundle needs 3 GB" warning is
	// the kind of bug that ships.
	InstalledSizeKiB int64 `json:"installed_size_kib,omitempty"`
	// DownloadSizeBytes is the Size: field: the .deb's own size, in bytes.
	// The selection tray sums it to estimate the download, which is the number
	// an operator on a slow link actually cares about. An estimate only — it
	// counts nothing about dependencies, which the catalogue cannot see and
	// must not try to.
	DownloadSizeBytes int64 `json:"download_size_bytes,omitempty"`

	// AppName is the human application name from DEP-11 — "LibreOffice
	// Writer" for libreoffice-writer, "GNU Image Manipulation Program" for
	// gimp. Empty when DEP-11 does not describe this package, which is the
	// case for the large majority of a 70,000-package archive. This field is
	// the entire reason DEP-11 is parsed at all: it is what makes a catalogue
	// browsable by someone who does not already know Debian's naming.
	AppName string `json:"app_name,omitempty"`
	// Categories are the freedesktop categories from DEP-11 —
	// "Graphics", "Development", "AudioVideo". Empty for a non-application.
	// The first tier of the category grouping.
	Categories []string `json:"categories,omitempty"`
	// IsApp reports whether DEP-11 describes this package as a desktop
	// application (as opposed to an addon, a font, a codec, or nothing at
	// all). It backs Query.AppsOnly, which is the filter that turns 70,000
	// rows into the few hundred a non-expert was looking for.
	IsApp bool `json:"is_app,omitempty"`
	// IconRef is an opaque reference to this application's cached icon, or
	// empty when there is none. Opaque because the resolution — DEP-11 icon
	// cache tarball, local theme, or nothing — is the catalogue's business
	// and not the frontend's; the UI passes it back through the binding
	// surface rather than building a path from it. May be empty for every
	// entry in a build that does not fetch icons; the UI must render without
	// one.
	IconRef string `json:"icon_ref,omitempty"`
}

// Query is one search request. Text, one category, one filter, one page.
//
// Deliberately not extensible: no sort order, no field selection, no
// expression language. Ordering is part of the contract rather than the
// caller's choice, so that every screen ranks results identically and the
// implementation is free to precompute exactly one order.
type Query struct {
	// Text is the operator's search box, matched case-insensitively as a
	// substring of the package name, the DEP-11 application name and the
	// summary. Empty means "no text filter" and returns the whole catalogue,
	// paged — which is the state the picker opens in.
	//
	// AppName is matched alongside Name rather than alongside Summary, and
	// ranks with it: someone typing "image manipulation" is naming gimp, not
	// describing it, and a product whose whole purpose is to be searchable by
	// people who do not know Debian's package names must find it.
	//
	// It is a substring match and not a fuzzy one on purpose: an operator
	// typing "post" for postgresql must not be shown "hostapd" scoring
	// three-quarters of a point higher for reasons no one can explain.
	Text string `json:"text,omitempty"`
	// Category filters to one Category.ID, from Categories. Empty means all.
	// It is the ID rather than the display name so that an application
	// category and an apt section that happen to share a word cannot collide.
	Category string `json:"category,omitempty"`
	// AppsOnly restricts results to entries with IsApp set — the DEP-11
	// desktop applications. The single most useful filter in the product: it
	// is the difference between browsing a software centre and browsing an
	// archive.
	AppsOnly bool `json:"apps_only,omitempty"`
	// Offset is the index of the first row to return, counted from the start
	// of the full match set. The virtualiser sets it to whatever the scroll
	// position implies, so it may be large and may jump.
	Offset int `json:"offset,omitempty"`
	// Limit is the maximum number of rows to return. Zero means
	// DefaultPageSize; anything above MaxPageSize is clamped to it, because
	// nothing on this bridge is allowed to carry an unbounded slice of rows.
	Limit int `json:"limit,omitempty"`
}

// Page sizes. A page is what crosses the Wails bridge on every keystroke, so
// it is sized for a screenful plus overscan, not for a result set.
const (
	// DefaultPageSize is what a Query with no Limit gets: comfortably more
	// than a viewport of rows at the smallest window this app runs in.
	DefaultPageSize = 50
	// MaxPageSize is the hard ceiling. It exists so that no caller — bug,
	// frontend experiment, or a future bound method — can turn Search into
	// "give me all 70,000 rows" and blame the bridge for the result.
	MaxPageSize = 500
)

// Normalized returns q with its paging clamped to the contract. Every
// implementation must apply it before serving a query, and every test may
// rely on it, so that a zero Query is a valid one.
func (q Query) Normalized() Query {
	if q.Offset < 0 {
		q.Offset = 0
	}
	switch {
	case q.Limit <= 0:
		q.Limit = DefaultPageSize
	case q.Limit > MaxPageSize:
		q.Limit = MaxPageSize
	}
	q.Text = strings.TrimSpace(q.Text)
	return q
}

// Page is one screenful of results, plus what the UI needs to reason about the
// rest of them without receiving it.
type Page struct {
	// Entries are the rows for this page, already in contract order. Never
	// longer than the effective Limit; may be shorter, or empty, at the end
	// of the result set.
	Entries []Entry `json:"entries"`
	// Total is the number of entries matching the query, not the number
	// returned. The virtualiser multiplies it by the row height to size its
	// scrollbar, which is the whole reason it exists: a list can be scrolled
	// through 60,000 rows without any of them having been sent.
	Total int `json:"total"`
	// Offset is the offset these rows start at — the normalised value, not
	// necessarily what the caller asked for. The frontend uses it to place
	// rows in the virtual list without trusting its own bookkeeping.
	Offset int `json:"offset"`
	// Query is the normalised query these results answer.
	//
	// Echoed back because search is fired per keystroke over an asynchronous
	// bridge, and responses can arrive out of order: the frontend compares
	// this against what it currently has typed and drops anything stale. A
	// UI that cannot do that shows the results for "fire" after the operator
	// has finished typing "firefox".
	Query Query `json:"query"`
	// Elapsed is how long the search took in Go, measured — not the
	// round-trip, which only the frontend can see. It is here because the
	// < 100 ms p95 budget is a measured number recorded in docs/performance.md
	// rather than an assertion, and a number nobody can read is a number
	// nobody will check.
	Elapsed time.Duration `json:"elapsed_ns"`
}

// CategoryTier is which of the two groupings a category belongs to.
//
// Two tiers rather than one because the archive genuinely has two kinds of
// answer to "what sort of thing is this?": DEP-11 says "Graphics" about the
// forty applications a person might want, and apt says "libs" about the
// twenty thousand packages nobody browses for. Flattening them into one list
// would bury the useful tier under the exhaustive one.
type CategoryTier string

const (
	// TierApplication is a freedesktop category from DEP-11: Graphics,
	// Development, Office, AudioVideo, Network, Game, Utility. Shown first,
	// because it is what an operator who does not know package names is
	// looking for.
	TierApplication CategoryTier = "application"
	// TierSection is an apt Section: from the Packages index — net, devel,
	// libs, python, admin. The complete grouping, for the operator who knows
	// what they are doing.
	TierSection CategoryTier = "section"
)

// Category is one entry in the sidebar's two-tier grouping.
type Category struct {
	// ID is the stable key to put in Query.Category. It is tier-prefixed —
	// "app:Graphics", "section:graphics" — so that a freedesktop category and
	// an apt section with the same word are different filters. Build it with
	// AppCategoryID or SectionCategoryID; never by concatenation at a call
	// site.
	ID string `json:"id"`
	// Tier is which grouping this is.
	Tier CategoryTier `json:"tier"`
	// Name is the label to render: the freedesktop category or the apt
	// section, as written in the index. Not localised — DEP-11 categories are
	// defined in English by the freedesktop spec, and inventing translations
	// here would make them stop matching the filter.
	Name string `json:"name"`
	// Count is how many entries the catalogue holds in this category, before
	// any text or apps-only filter. It is shown next to the label, and it is
	// what tells an operator that "Games" is worth opening and "Localization"
	// is not.
	Count int `json:"count"`
}

// Category ID prefixes. Exported because a deep link into a category arrives
// from outside this package and has to be recognised, and because the view
// layer must not spell them itself.
const (
	AppCategoryPrefix     = "app:"
	SectionCategoryPrefix = "section:"
)

// AppCategoryID returns the Query.Category value for a DEP-11 freedesktop
// category.
func AppCategoryID(name string) string { return AppCategoryPrefix + name }

// SectionCategoryID returns the Query.Category value for an apt Section.
func SectionCategoryID(name string) string { return SectionCategoryPrefix + name }

// ParseCategoryID splits an id back into its tier and name. ok is false for
// anything that is not a well-formed id, which callers must treat as "no
// category filter" rather than as an error: a stale deep link should show the
// catalogue, not a failure.
func ParseCategoryID(id string) (tier CategoryTier, name string, ok bool) {
	switch {
	case strings.HasPrefix(id, AppCategoryPrefix):
		name = strings.TrimPrefix(id, AppCategoryPrefix)
		return TierApplication, name, name != ""
	case strings.HasPrefix(id, SectionCategoryPrefix):
		name = strings.TrimPrefix(id, SectionCategoryPrefix)
		return TierSection, name, name != ""
	default:
		return "", "", false
	}
}

// Phase is which stage of a catalogue build is running.
//
// The phases are named rather than counted because the UI's contract is that
// it never shows a spinner with no explanation: at any instant it must be able
// to say what is being waited on, and "step 3 of 6" cannot.
type Phase string

const (
	// PhaseResolve is working out which index files this target needs. Fast,
	// local, and included so that the very first progress callback has
	// something honest to say before any network is touched.
	PhaseResolve Phase = "resolve"
	// PhaseRelease is fetching each suite's Release file — a few kilobytes
	// each, which is where the recorded index hashes come from. Separate from
	// the download phase because it is where an unreachable mirror or a
	// missing suite is discovered, and that failure needs its own message.
	PhaseRelease Phase = "release"
	// PhaseDownload is fetching the Packages and DEP-11 indexes. The long,
	// network-bound phase — tens of megabytes — and the one where BytesDone
	// and BytesTotal are meaningful.
	PhaseDownload Phase = "download"
	// PhaseParse is turning those bytes into entries. CPU-bound, and the
	// phase the < 10 s budget is about.
	PhaseParse Phase = "parse"
	// PhaseIndex is building the search order and the category counts.
	PhaseIndex Phase = "index"
	// PhaseSave is writing the cache to disk.
	PhaseSave Phase = "save"
	// PhaseDone is the terminal callback. A Build that succeeds emits exactly
	// one of these as its last callback, with Current == Total, so the UI has
	// a defined moment to snap the bar to 100% rather than inferring it from
	// the function returning.
	PhaseDone Phase = "done"
)

// Phases is every phase in the order they occur. Progress.PhaseCount is
// len(Phases) for a full build; an implementation that skips one (a cached
// Release, a component with no DEP-11) still reports the same total, so the
// bar does not jump backwards.
var Phases = []Phase{PhaseResolve, PhaseRelease, PhaseDownload, PhaseParse, PhaseIndex, PhaseSave, PhaseDone}

// Label is the default human phrase for a phase, present tense, for a UI that
// has nothing better to show. Progress.Label is usually more specific and
// should be preferred when it is set.
func (p Phase) Label() string {
	switch p {
	case PhaseResolve:
		return "Working out which package lists to fetch"
	case PhaseRelease:
		return "Checking the archive"
	case PhaseDownload:
		return "Downloading package lists"
	case PhaseParse:
		return "Reading package lists"
	case PhaseIndex:
		return "Building the search index"
	case PhaseSave:
		return "Saving the catalogue"
	case PhaseDone:
		return "Catalogue ready"
	default:
		return "Working"
	}
}

// Index returns the phase's position in Phases, or -1. Lets a UI draw an
// overall bar across a multi-phase job without hard-coding the order.
func (p Phase) Index() int {
	for i, x := range Phases {
		if x == p {
			return i
		}
	}
	return -1
}

// Progress is one report from a running Build.
//
// It is designed so that a progress view can always answer three questions:
// what is happening, how far through it is, and what specific thing is being
// waited on. A build that downloads 24 index files across 4 suites is opaque
// unless the UI can say "downloading noble/universe — 9 of 24, 31 MB of
// 58 MB", and an operator staring at an unexplained bar for forty seconds
// concludes the application has hung. That is the failure this type exists to
// prevent.
type Progress struct {
	// Phase is which stage is running.
	Phase Phase `json:"phase"`
	// PhaseIndex and PhaseCount place the phase in the whole job, so an
	// overall bar can be drawn as (PhaseIndex + phase fraction) / PhaseCount.
	// PhaseCount is len(Phases) and does not change during a build.
	PhaseIndex int `json:"phase_index"`
	PhaseCount int `json:"phase_count"`

	// Label is the human sentence for this instant, present tense, and never
	// empty. Implementations set it to something more specific than
	// Phase.Label whenever they can.
	Label string `json:"label"`
	// Item is the specific thing being worked on, when there is one —
	// "noble/universe/binary-amd64/Packages.gz". Empty for phases with no
	// per-item structure. Shown in smaller type under Label; never the only
	// thing shown, because a path is not an explanation.
	Item string `json:"item,omitempty"`

	// Current and Total count items within the phase: index files fetched,
	// stanzas parsed. Total is 0 when the count is not yet known, which makes
	// the phase indeterminate — see Fraction.
	Current int64 `json:"current"`
	Total   int64 `json:"total"`

	// BytesDone and BytesTotal are the download phase's byte counters, and
	// are zero elsewhere. BytesTotal is 0 when no Content-Length was offered,
	// which is common enough on archive mirrors that the UI must render a
	// download with an unknown size without breaking.
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`

	// Elapsed is time since Build was called. Included so the UI can show a
	// running clock without keeping its own, and so a log of callbacks is a
	// usable profile of where a slow build spent its time.
	Elapsed time.Duration `json:"elapsed_ns"`
}

// Fraction is how far through the current phase this report is, in [0,1], or
// -1 when the phase is indeterminate. The -1 is explicit rather than a 0 so
// that a UI can choose an indeterminate bar instead of drawing an empty one
// that never moves.
func (p Progress) Fraction() float64 {
	if p.Total <= 0 {
		if p.BytesTotal > 0 {
			return clamp01(float64(p.BytesDone) / float64(p.BytesTotal))
		}
		return -1
	}
	return clamp01(float64(p.Current) / float64(p.Total))
}

// OverallFraction is how far through the whole build this report is, in
// [0,1]. Phases are weighted equally, which is a lie the operator forgives
// and an unexplained pause is not: what matters is that the bar always moves
// forwards and reaches the end exactly when the build does.
func (p Progress) OverallFraction() float64 {
	if p.PhaseCount <= 0 {
		return -1
	}
	if p.Phase == PhaseDone {
		return 1
	}
	f := p.Fraction()
	if f < 0 {
		f = 0
	}
	return clamp01((float64(p.PhaseIndex) + f) / float64(p.PhaseCount))
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// safeKeyPart reduces s to characters that are safe in a path segment on
// every filesystem this app runs on. Anything else becomes "-": the hash in
// the key carries the distinguishing power, so collapsing is safe and
// escaping would only make the directory names unreadable.
func safeKeyPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

func hasType(types []string, want string) bool {
	if len(types) == 0 {
		// A stanza with no explicit Types is a deb source: that is deb822's
		// own default, and a target's sources.list.d written by hand often
		// omits it.
		return want == "deb"
	}
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

func uniqueSorted(sources []Source, pick func(Source) []string) []string {
	var out []string
	for _, s := range sources {
		for _, v := range pick(s) {
			v = strings.TrimSpace(v)
			if v != "" {
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// dedupe removes adjacent duplicates from a sorted slice, in place.
func dedupe(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, v := range sorted[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
