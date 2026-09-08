package catalog

// DEP-11 (AppStream) parsing — the applications tier of the catalogue.
//
// This file fetches dists/<suite>/<component>/dep11/Components-<arch>.yml.gz
// and turns it into the three things that make a package list browsable by
// someone who does not already know Debian's naming: a human application name
// ("LibreOffice Writer", not "libreoffice-writer"), a human-written one-line
// summary, and freedesktop categories.
//
// # It is a 3% layer, not a peer of the package list
//
// docs/dev/index-formats.md §6.1: DEP-11 describes 2,132 of 70,853 Ubuntu
// noble binary packages — 3.01%, of which 2.65% are desktop applications.
// Every design choice here follows from that. Nothing in this file creates a
// catalogue row; it only enriches rows the Packages index already produced.
// A component naming a package that does not exist contributes nothing, a
// component whose file 404s costs the operator a few nicer labels, and
// neither is a build failure.
//
// # The trap that silently empties the applications tier
//
// §7.3: gopkg.in/yaml.v3 returns *yaml.TypeError on real archive data — a
// duplicate locale key inside one component of Ubuntu's universe DEP-11. It is
// an *accumulating* error, not a stream error: the decoder has already
// populated everything it could and the stream is still correctly positioned.
// The naive `if err != nil { break }` stops at component 449 of 2,587 and
// discards 83% of the applications, with no crash and nothing in the UI to
// suggest anything went wrong. dep11Decode therefore treats *yaml.TypeError
// as soft, counts it, and carries on. dep11_test.go proves it with real
// fixture data, because this is the failure mode nobody would notice.
//
// # No new dependency
//
// gopkg.in/yaml.v3 is already in go.mod (as an indirect requirement, via
// debark -> pault.ag/go/debian). Importing it here adds no module to the
// build and no code to the binary that was not already linkable; the only
// effect of a later `go mod tidy` would be to drop the `// indirect` comment.
// go.mod is frozen, so that is left alone deliberately (rule 6).
//
// # The surface the rest of the package uses
//
// Everything here is unexported and prefixed dep11, because package catalog is
// written by four concurrent packages and a redeclaration breaks the build for
// all of them. The three things a caller needs:
//
//	idx, err := dep11Load(ctx, target.IndexRefs(), dep11Options{Progress: p})
//	idx.Reconcile(func(pkg string) bool { _, ok := packages[pkg]; return ok })
//	for i := range entries { idx.Apply(&entries[i]) }
//
// dep11Load ignores refs that are not IndexDEP11, so the whole IndexRefs()
// slice can be passed straight in. Reconcile is optional — Apply is driven
// from the entry side and so cannot invent a row for a package that does not
// exist — but it makes the dangling-reference count visible. dep11Decode is
// the network-free half, for callers with bytes already in hand (a cache
// rebuild, a test). idx.Stats() reports what the pass saw.

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Tuning constants.
const (
	// dep11ReadBuffer is the buffer yaml.NewDecoder reads through. §7.4
	// measured the streaming decode at ~62 MB/s over a 1 MiB buffer.
	dep11ReadBuffer = 1 << 20

	// dep11MaxDecompressedBytes caps what one Components-<arch>.yml may expand
	// to. The largest real file is 18.6 MB (§1); 512 MiB is four orders of
	// magnitude of headroom and still stops a malicious mirror from expanding
	// a small download into the whole heap. The catalogue is not a trust
	// boundary (catalogue-sourcing.md), but "not verified" is not the same as
	// "will happily be OOM-killed by a stranger".
	dep11MaxDecompressedBytes = 512 << 20

	// dep11ProgressBytes is how often the download phase reports bytes.
	dep11ProgressBytes = 256 << 10
)

// dep11App is everything DEP-11 contributes to one binary package: the fields
// Entry needs, and nothing else.
//
// It is deliberately a plain value type with no reference to the Packages
// side. packages.go builds entries, dep11.go builds this, and catalog.go
// joins them by package name — so neither parser imports the other and
// neither can be blocked on the other's internals.
type dep11App struct {
	// Package is the binary package name from the component's Package: field,
	// and the only key the join uses. Never ID: §7.2 — ID is the desktop-entry
	// id ("htop.desktop", "org.gnome.Klotski") and does not equal the package
	// name.
	Package string
	// ID is the AppStream component id, kept for diagnostics and because an
	// addon's Extends: points at one.
	ID string
	// Type is the component type verbatim: desktop-application, addon,
	// generic, font, inputmethod, codec, console-application, icon-theme,
	// web-application (§6.2).
	Type string
	// Name is the C/English application name — Entry.AppName.
	Name string
	// Summary is the C/English one-line summary — Entry.Summary, which the
	// contract says DEP-11 wins when it knows the package.
	Summary string
	// Categories are the freedesktop categories, deduplicated, in file order.
	// Absent on 14.2% of components (§6.3), so empty is normal.
	Categories []string
	// IconRef is an opaque reference to a cached icon, never a URL. Nothing in
	// this package fetches an icon; see dep11IconRef.
	IconRef string
	// IsApp is true only for Type: desktop-application. It backs
	// Query.AppsOnly, whose whole value is that it means "the things a person
	// would call an application" — a font or a gstreamer codec is neither.
	IsApp bool
}

// Apply copies this component's contribution onto e. It never clears a field
// the Packages index already filled: a component with no Summary leaves the
// Description-derived one in place.
func (a dep11App) Apply(e *Entry) {
	if e == nil {
		return
	}
	if a.Name != "" {
		e.AppName = a.Name
	}
	if a.Summary != "" {
		e.Summary = a.Summary
	}
	if len(a.Categories) > 0 {
		e.Categories = append(e.Categories[:0:0], a.Categories...)
	}
	if a.IconRef != "" {
		e.IconRef = a.IconRef
	}
	if a.IsApp {
		e.IsApp = true
	}
}

// dep11Failure records one index file that could not be read. It is not an
// error return because a DEP-11 file is optional (IndexRef doc comment): the
// build continues and the operator gets package names instead of application
// names for that component.
type dep11Failure struct {
	Ref IndexRef
	Err error
}

func (f dep11Failure) String() string { return f.Ref.URL() + ": " + f.Err.Error() }

// dep11Stats is what the parse actually saw. Every number here exists because
// something in index-formats.md said it would be non-zero on real data, and a
// count that is silently zero is how a regression hides.
type dep11Stats struct {
	// Files is how many IndexDEP11 refs were fetched and decoded.
	Files int
	// NotPublished is how many returned 404/410. Expected, not an error: a
	// component may simply not publish DEP-11.
	NotPublished int
	// Failures are refs that could not be read for any other reason.
	Failures []dep11Failure

	// Documents is every YAML document in every stream, headers included.
	Documents int
	// Headers is documents carrying a File: key (§7.1). One per file
	// normally, but a header is detected by its key and not by position, so a
	// concatenated stream with several is handled.
	Headers int
	// Components is documents that named a Package.
	Components int
	// Merged is components folded into the lookup.
	Merged int
	// Addons is components skipped as extensions of another component:
	// Type: addon, or anything carrying Extends:. They describe a plugin, so
	// their Name is the plugin's ("TIFF Documents"), not the package's (§6.2).
	// Extends occurs on 245 of the archive's components and addon on 244, so
	// the Extends: half of the rule catches about one component in the whole
	// Ubuntu archive that the type alone would have missed.
	Addons int
	// Skipped is documents that were neither a header nor a usable component
	// — no Package:, or a component abandoned wholesale by a TypeError.
	Skipped int
	// Superseded is components whose package another component had already
	// claimed. Real: Ubuntu noble main+universe has 2,587 components for 2,189
	// distinct packages (§6.1).
	Superseded int
	// TypeErrors is how many documents came back with a *yaml.TypeError and
	// were kept anyway. §7.3. If this is a hard error the catalogue loses 83%
	// of its applications in silence.
	TypeErrors int
	// TypeErrorDetail holds the first few yaml messages, for a log line.
	TypeErrorDetail []string

	// Apps is merged components with Type: desktop-application.
	Apps int
	// ByType counts merged and skipped components by their Type: field.
	ByType map[string]int

	// Dangling is components naming a package with no stanza in the Packages
	// index, dropped by Reconcile. 57 in Ubuntu noble (§6.1) — real staleness,
	// not a parser bug: noble's DEP-11 was generated 2023-11-12, five months
	// before noble released.
	Dangling int
	// DanglingPackages are those names, sorted.
	DanglingPackages []string

	// CompressedBytes and DecodedBytes are what came off the wire and what the
	// YAML decoder read.
	CompressedBytes int64
	DecodedBytes    int64
	// Elapsed is wall time inside dep11Load.
	Elapsed time.Duration
}

// String is a single log line for a completed DEP-11 pass.
func (s dep11Stats) String() string {
	var b strings.Builder
	b.WriteString("dep11: ")
	b.WriteString(strconv.Itoa(s.Files))
	b.WriteString(" file(s)")
	if s.NotPublished > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(s.NotPublished))
		b.WriteString(" not published")
	}
	if n := len(s.Failures); n > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(n))
		b.WriteString(" unreadable")
	}
	fmt.Fprintf(&b, ", %d components, %d merged (%d apps)", s.Components, s.Merged, s.Apps)
	if s.Addons > 0 {
		fmt.Fprintf(&b, ", %d addons skipped", s.Addons)
	}
	if s.Superseded > 0 {
		fmt.Fprintf(&b, ", %d superseded", s.Superseded)
	}
	if s.TypeErrors > 0 {
		fmt.Fprintf(&b, ", %d yaml type error(s) tolerated", s.TypeErrors)
	}
	if s.Dangling > 0 {
		fmt.Fprintf(&b, ", %d dangling package ref(s)", s.Dangling)
	}
	if s.Elapsed > 0 {
		fmt.Fprintf(&b, ", %s", s.Elapsed.Round(time.Millisecond))
	}
	return b.String()
}

// dep11Header is the stream's first document (§7.1). Five keys occur in both
// Ubuntu and Debian and no others; every one is treated as optional because
// the DEP-11 spec permits more and neither archive ships them.
type dep11Header struct {
	// File is always "DEP-11" and is how a header is told from a component.
	File string
	// Version is quoted in the file ('0.14' on Ubuntu noble, '0.16' on Debian
	// bookworm) so it decodes as a string. Never float64 it, never compare it.
	Version string
	// Origin is "ubuntu-noble-main", "debian-bookworm-main".
	Origin string
	// MediaBaseURL is where screenshots and remote icons live. It is parsed
	// and then ignored: contract-brief rule 3 limits network access to the
	// archives in the target's own sources and operator-typed vendor URLs, and
	// appstream.ubuntu.com is neither. Nothing in this repository may fetch
	// from it.
	MediaBaseURL string
	// Time is when the metadata was generated, not when the suite was
	// released, and is months stale in practice (§6.1).
	Time string
}

// dep11Index is the join side of the applications tier: a lookup from binary
// package name to the one component that describes it.
//
// It is the whole dep11.go -> catalog.go interface: catalog.go merges it into
// the entries packages.go produced; nothing here reads or imports the
// Packages side.
type dep11Index struct {
	byPackage map[string]dep11App
	headers   []dep11Header
	stats     dep11Stats
}

// dep11NewIndex returns an empty index ready to have components folded into it.
func dep11NewIndex() *dep11Index {
	return &dep11Index{
		byPackage: make(map[string]dep11App),
		stats:     dep11Stats{ByType: make(map[string]int)},
	}
}

// Len is how many packages have DEP-11 data. Expect ~3% of the catalogue.
func (x *dep11Index) Len() int {
	if x == nil {
		return 0
	}
	return len(x.byPackage)
}

// Lookup returns the component describing pkg, if there is one.
func (x *dep11Index) Lookup(pkg string) (dep11App, bool) {
	if x == nil {
		return dep11App{}, false
	}
	a, ok := x.byPackage[pkg]
	return a, ok
}

// Apply enriches one entry in place and reports whether DEP-11 knew it. It is
// the intended merge call: iterate the entries the Packages index produced and
// hand each one here. Doing it this way rather than iterating the DEP-11 side
// means a component naming a package that does not exist cannot invent a row
// (§6.1's 57 dangling refs) without any explicit check.
func (x *dep11Index) Apply(e *Entry) bool {
	if x == nil || e == nil {
		return false
	}
	a, ok := x.byPackage[e.Name]
	if !ok {
		return false
	}
	a.Apply(e)
	return true
}

// Packages lists every package the index describes, sorted. For tests,
// diagnostics and cache writing; not for the hot path.
func (x *dep11Index) Packages() []string {
	if x == nil {
		return nil
	}
	out := make([]string, 0, len(x.byPackage))
	for k := range x.byPackage {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Apps returns every merged component, sorted by package name.
func (x *dep11Index) Apps() []dep11App {
	if x == nil {
		return nil
	}
	out := make([]dep11App, 0, len(x.byPackage))
	for _, k := range x.Packages() {
		out = append(out, x.byPackage[k])
	}
	return out
}

// Headers returns the header document of every stream read, in order.
func (x *dep11Index) Headers() []dep11Header {
	if x == nil {
		return nil
	}
	return x.headers
}

// Stats reports what the pass saw.
func (x *dep11Index) Stats() dep11Stats {
	if x == nil {
		return dep11Stats{}
	}
	return x.stats
}

// Reconcile drops every component whose package has no stanza in the Packages
// index and returns how many it dropped.
//
// Optional — Apply already cannot invent a row — but calling it keeps the
// index honest about its own size and surfaces the count, which is a genuine
// staleness signal about the archive rather than a defect here. exists is
// packages.go's "is this a package name I saw?" predicate; a nil predicate is
// a no-op.
func (x *dep11Index) Reconcile(exists func(pkg string) bool) int {
	if x == nil || exists == nil {
		return 0
	}
	var dropped []string
	for name := range x.byPackage {
		if !exists(name) {
			dropped = append(dropped, name)
		}
	}
	sort.Strings(dropped)
	for _, name := range dropped {
		a := x.byPackage[name]
		delete(x.byPackage, name)
		x.stats.Merged--
		if a.IsApp {
			x.stats.Apps--
		}
	}
	x.stats.Dangling += len(dropped)
	x.stats.DanglingPackages = append(x.stats.DanglingPackages, dropped...)
	sort.Strings(x.stats.DanglingPackages)
	return len(dropped)
}

// Add folds one component into the index, applying the precedence rule below,
// and reports whether it became (or replaced) the entry for its package.
//
// Several components can name one package: Ubuntu noble has 2,587 components
// for 2,189 distinct packages (§6.1). The rule is by component *type*, never
// by anything version-like — rule 1 forbids ranking by version, and nothing in
// a DEP-11 component is a version anyway:
//
//	desktop-application > console-application > web-application > everything else
//
// and, among equals, the first one in file order wins. Positional, documented,
// and reproducible; it decides which label a row shows and nothing else.
func (x *dep11Index) Add(a dep11App) bool {
	if x == nil || a.Package == "" {
		return false
	}
	if cur, ok := x.byPackage[a.Package]; ok {
		x.stats.Superseded++
		if dep11TypeRank(cur.Type) >= dep11TypeRank(a.Type) {
			return false
		}
		if cur.IsApp {
			x.stats.Apps--
		}
		x.stats.Merged--
	}
	x.byPackage[a.Package] = a
	x.stats.Merged++
	if a.IsApp {
		x.stats.Apps++
	}
	return true
}

// Component types that actually occur in the archive (§6.2). Anything not
// listed is decoded, counted by type, and merged at the lowest rank — an
// unknown type is cheap to carry and a new AppStream type appearing in a
// future suite must not silently vanish.
const (
	dep11TypeDesktopApp = "desktop-application"
	dep11TypeConsoleApp = "console-application"
	dep11TypeWebApp     = "web-application"
	dep11TypeAddon      = "addon"
)

// dep11TypeRank orders component types for the merge. Higher wins.
func dep11TypeRank(t string) int {
	switch t {
	case dep11TypeDesktopApp:
		return 4
	case dep11TypeConsoleApp:
		return 3
	case dep11TypeWebApp:
		return 2
	case dep11TypeAddon:
		return 0
	default:
		// generic, font, codec, inputmethod, icon-theme, and anything new.
		return 1
	}
}

// dep11Document is one YAML document: a header or a component. One struct for
// both because a stream mixes them and a header is identified by its File:
// key, not by being first (§7.1).
//
// Fields the GUI does not render are deliberately absent. yaml.v3 skips an
// unknown key without descending into its value, so omitting Description,
// Keywords, Screenshots, Releases and the rest is not just tidy — it is most
// of why this decode is fast and why §7.5's 11 MB of translations never
// reaches the heap.
type dep11Document struct {
	// Header keys.
	File         string `yaml:"File"`
	Version      string `yaml:"Version"`
	Origin       string `yaml:"Origin"`
	MediaBaseURL string `yaml:"MediaBaseUrl"`
	Time         string `yaml:"Time"`

	// Component keys. ID, Name, Package, Summary and Type are the only ones
	// present on 100% of components (§6.3); Categories is missing on 14.2%
	// and Icon on 8.9%, so both must be optional in the decoded struct.
	Type       string         `yaml:"Type"`
	ID         string         `yaml:"ID"`
	Package    string         `yaml:"Package"`
	Name       dep11Localized `yaml:"Name"`
	Summary    dep11Localized `yaml:"Summary"`
	Categories []string       `yaml:"Categories"`
	Extends    []string       `yaml:"Extends"`
	Icon       dep11Icon      `yaml:"Icon"`
}

// isHeader reports whether this document is the stream header.
func (d *dep11Document) isHeader() bool { return d.File != "" }

// dep11Localized decodes a locale-keyed map down to the single English value
// the UI will show.
//
// Name and Summary are maps from locale to string, and 83% of the payload is
// locales this application will never render (§7.5): 1,105 distinct keys, of
// which the GUI wants one. A custom unmarshaler is the difference between
// keeping 2.3 MB and keeping 13.7 MB, and it is applied during the decode so
// the strings are never retained rather than allocated and dropped.
//
// C is present under Name and Summary on 2,587 of 2,587 Ubuntu components, but
// is the *first* key only 63.5% of the time (§7.2) — it is 19th in the htop
// component. Look it up by key; never take the first entry.
//
// If localisation is ever offered, it belongs in a second pass over the cached
// component blob, not in a decoder that keeps every locale resident.
type dep11Localized struct {
	value string
}

// String is the chosen English value, or "" when the map had no English key.
func (l dep11Localized) String() string { return l.value }

// UnmarshalYAML picks the best English key out of the mapping node.
//
// It never returns an error. A non-nil, non-*yaml.TypeError error from an
// Unmarshaler makes yaml.v3 fail the whole Decode call, which would turn a
// malformed field in one component into a dead stream — exactly the failure
// §7.3 is about. A shape this does not recognise yields an empty string and
// the row falls back to its package name.
func (l *dep11Localized) UnmarshalYAML(n *yaml.Node) error {
	l.value = ""
	if n == nil {
		return nil
	}
	// A bare scalar is not what the archive ships, but costs one line to
	// accept and means a hand-written or future variant still renders.
	if n.Kind == yaml.ScalarNode {
		l.value = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	best := -1
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
			continue
		}
		rank := dep11LocaleRank(k.Value)
		// Strictly less, so a duplicated key keeps the first occurrence and
		// the result is deterministic whatever the file's key order.
		if rank >= 0 && (best < 0 || rank < best) {
			best, l.value = rank, v.Value
		}
	}
	return nil
}

// dep11LocaleRank orders locale keys by how much this application wants them.
// Lower is better; -1 means "not English, discard".
func dep11LocaleRank(locale string) int {
	switch locale {
	case "C", "C.UTF-8":
		return 0
	case "en":
		return 1
	case "en_US":
		return 2
	case "en_GB":
		return 3
	}
	// en_AU, en_CA, en@shaw and the rest: better than nothing, worse than any
	// of the above.
	if strings.HasPrefix(locale, "en_") || strings.HasPrefix(locale, "en@") {
		return 4
	}
	return -1
}

// dep11IconFile is one entry of an Icon: cached/remote list.
type dep11IconFile struct {
	Name   string `yaml:"name"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
	// URL is only present under remote:, and is relative to the header's
	// MediaBaseUrl — which this application must not fetch. Decoded so that
	// the shape is understood, never used.
	URL string `yaml:"url"`
}

// dep11Icon is the Icon: mapping. It has up to three kinds and they are not
// the same shape: cached and remote are lists of maps, stock is a plain
// string (§7.2). Decoding the whole thing as map[string]string fails.
type dep11Icon struct {
	Cached []dep11IconFile `yaml:"cached"`
	Remote []dep11IconFile `yaml:"remote"`
	Stock  string          `yaml:"stock"`
}

// dep11IconPreferredSize is the icon edge length preferred when a component
// offers several: it matches dists/<suite>/<component>/dep11/icons-64x64.tar.gz,
// the tarball a future icon fetcher would extract.
const dep11IconPreferredSize = 64

// dep11IconRef builds the opaque Entry.IconRef for a component. It records a
// reference and fetches nothing.
//
// Icons ship as separate tarballs and are out of scope for this package — and,
// on the evidence, probably out of scope altogether: §7.6 measures the
// universe 64x64 tarball at 7.7 MB for icons that decorate the 3% of rows with
// DEP-11 data, in a virtualised list that shows ~50 rows at a time. See the
// recommendation in dep11.go's companion note in the implementation notes.
//
// Format, chosen here because IconRef is opaque by contract and nothing else
// may build a path from it:
//
//	dep11-cached:<suite>/<component>/<W>x<H>/<name>   e.g. dep11-cached:noble/main/64x64/htop_htop.png
//	dep11-stock:<name>                                e.g. dep11-stock:libreoffice-draw
//
// A cached reference resolves against the archive tarball and needs nothing
// but the archive. A stock reference is an icon-theme name resolvable only on
// a machine with that theme installed, so it is recorded second and a renderer
// is free to ignore it. remote: is never recorded: its URL is relative to
// MediaBaseUrl, and rule 3 forbids fetching that host.
func dep11IconRef(ref IndexRef, ic dep11Icon) string {
	if best, ok := dep11PickIcon(ic.Cached); ok {
		return "dep11-cached:" + ref.Suite + "/" + ref.Component + "/" +
			strconv.Itoa(best.Width) + "x" + strconv.Itoa(best.Height) + "/" + best.Name
	}
	if ic.Stock != "" {
		return "dep11-stock:" + ic.Stock
	}
	return ""
}

// dep11PickIcon chooses the cached icon closest to the preferred size,
// preferring a larger one on a tie so a HiDPI display has something to scale
// down rather than up.
func dep11PickIcon(files []dep11IconFile) (dep11IconFile, bool) {
	var best dep11IconFile
	found := false
	for _, f := range files {
		if f.Name == "" {
			continue
		}
		if !found {
			best, found = f, true
			continue
		}
		if dep11IconScore(f) < dep11IconScore(best) {
			best = f
		}
	}
	return best, found
}

// dep11IconScore is distance from the preferred size, with larger beating
// smaller at equal distance.
func dep11IconScore(f dep11IconFile) int {
	d := f.Width - dep11IconPreferredSize
	if d < 0 {
		return -2*d + 1
	}
	return 2 * d
}

// dep11Decode reads one DEP-11 YAML stream and folds every usable component
// into dst. ref supplies the suite and component an icon reference needs.
//
// It returns an error only for something that genuinely ends the stream: a
// read failure, a YAML syntax error, or a cancelled context. A
// *yaml.TypeError is counted and stepped over — see the file comment and
// §7.3. This is the single most important line of behaviour in this package.
func dep11Decode(ctx context.Context, r io.Reader, ref IndexRef, dst *dep11Index) error {
	if dst == nil {
		return errors.New("dep11: nil index")
	}
	dec := yaml.NewDecoder(bufio.NewReaderSize(r, dep11ReadBuffer))
	for {
		// Checked every document rather than every N: a 2,587-document stream
		// costs 2,587 atomic loads against a ~300 ms decode, and an operator
		// who pressed Cancel wants it to stop now.
		if err := ctx.Err(); err != nil {
			return err
		}

		var doc dep11Document
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				return fmt.Errorf("dep11: %s: %w", ref.Path(), err)
			}
			// Soft. The decoder populated what it could and the stream is
			// still positioned correctly; only the specific field that hit the
			// problem was abandoned. Keep the component, count it, carry on.
			dst.stats.TypeErrors++
			for _, msg := range te.Errors {
				if len(dst.stats.TypeErrorDetail) >= 8 {
					break
				}
				dst.stats.TypeErrorDetail = append(dst.stats.TypeErrorDetail,
					ref.Path()+": "+msg)
			}
		}

		dst.stats.Documents++
		switch {
		case doc.isHeader():
			dst.stats.Headers++
			dst.headers = append(dst.headers, dep11Header{
				File:         doc.File,
				Version:      doc.Version,
				Origin:       doc.Origin,
				MediaBaseURL: doc.MediaBaseURL,
				Time:         doc.Time,
			})
			continue
		case doc.Package == "":
			// No Package: means nothing can be joined to it — an empty
			// document, or a component the decoder abandoned wholesale
			// because its top-level mapping had a duplicate key.
			dst.stats.Skipped++
			continue
		}

		dst.stats.Components++
		if dst.stats.ByType == nil {
			dst.stats.ByType = make(map[string]int)
		}
		dst.stats.ByType[doc.Type]++

		// Addons describe a plugin, not the package: evince's two addon
		// components in the real main index are called "TIFF Documents" and
		// "XPS Documents", and letting either name the evince row would be
		// worse than showing no application name at all. They also carry
		// Extends: pointing at another component's ID and are not standalone
		// installables (§6.2).
		if doc.Type == dep11TypeAddon || len(doc.Extends) > 0 {
			dst.stats.Addons++
			continue
		}

		dst.Add(dep11App{
			Package:    doc.Package,
			ID:         doc.ID,
			Type:       doc.Type,
			Name:       doc.Name.String(),
			Summary:    doc.Summary.String(),
			Categories: dep11CleanCategories(doc.Categories),
			IconRef:    dep11IconRef(ref, doc.Icon),
			IsApp:      doc.Type == dep11TypeDesktopApp,
		})
	}
}

// dep11CleanCategories trims, drops empties and deduplicates while preserving
// file order. 135 distinct strings occur across the archive and 1,662 of 2,219
// components carry more than one (§6.4); the UI groups by the 13 freedesktop
// main categories and treats the long tail as searchable keywords, which is
// search.go's decision to make and not this parser's to pre-empt.
func dep11CleanCategories(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dep11Options configures a load. The zero value works and uses a shared
// client with sane timeouts.
type dep11Options struct {
	// Client is the HTTP client. Injected so tests serve fixtures from
	// httptest and CI never touches the network.
	Client *http.Client
	// UserAgent is sent with every request. Empty uses dep11DefaultUserAgent.
	UserAgent string
	// MaxDecompressedBytes caps one expanded index. Zero uses
	// dep11MaxDecompressedBytes; negative disables the cap.
	MaxDecompressedBytes int64
	// Progress, if set, receives download and parse reports. It is called
	// from the calling goroutine and must not block.
	Progress func(Progress)
	// Started is when the enclosing build began, for Progress.Elapsed. Zero
	// means "when this load started".
	Started time.Time
}

const dep11DefaultUserAgent = "debark-gui-catalog/1 (+https://github.com/inferops/debark/gui)"

var (
	dep11ClientOnce   sync.Once
	dep11SharedClient *http.Client
)

// dep11DefaultClient is the client used when none is injected. No overall
// Timeout: a 6 MB index over a slow mirror is a legitimate multi-minute
// download and cancellation is the context's job. The per-stage timeouts are
// what make an unreachable mirror fail in seconds instead of hanging.
func dep11DefaultClient() *http.Client {
	dep11ClientOnce.Do(func() {
		dep11SharedClient = &http.Client{
			// Shared with packagesDefaultClient: an archive may not redirect
			// an index fetch off http/https, and may not downgrade https to
			// http. See igCheckIndexRedirect in sources.go.
			CheckRedirect: igCheckIndexRedirect,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				MaxIdleConnsPerHost:   4,
				ForceAttemptHTTP2:     true,
			},
		}
	})
	return dep11SharedClient
}

// dep11Load fetches and parses every IndexDEP11 ref and returns the lookup.
//
// refs may be the full output of Target.IndexRefs(); anything that is not an
// IndexDEP11 ref is ignored, so callers do not have to filter. Order is
// preserved, which is what makes the "first component of equal rank wins"
// merge rule reproducible across runs.
//
// A file that 404s, or that cannot be read at all, does not fail the load:
// DEP-11 is optional per component (IndexRef's contract) and the consequence
// of losing it is nicer labels on 3% of rows, not a wrong catalogue. Both
// outcomes are counted in Stats — NotPublished for 404/410, Failures for
// anything else — so a caller that wants to surface "the archive's app
// metadata was unreachable" can. The only error returned is a cancelled
// context or a malformed stream that a mirror actually served as 200 OK.
func dep11Load(ctx context.Context, refs []IndexRef, opts dep11Options) (*dep11Index, error) {
	started := opts.Started
	if started.IsZero() {
		started = time.Now()
	}
	idx := dep11NewIndex()

	var todo []IndexRef
	for _, r := range refs {
		if r.Kind == IndexDEP11 && r.URL() != "" {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		idx.stats.Elapsed = time.Since(started)
		return idx, nil
	}

	for i, ref := range todo {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dep11Report(opts, started, Progress{
			Phase:   PhaseDownload,
			Label:   "Downloading application metadata for " + ref.Suite + "/" + ref.Component,
			Item:    ref.Path(),
			Current: int64(i),
			Total:   int64(len(todo)),
		})

		err := dep11LoadRef(ctx, ref, idx, opts, started)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			idx.stats.Failures = append(idx.stats.Failures, dep11Failure{Ref: ref, Err: err})
		}

		dep11Report(opts, started, Progress{
			Phase:   PhaseParse,
			Label:   "Reading application metadata for " + ref.Suite + "/" + ref.Component,
			Item:    ref.Path(),
			Current: int64(i + 1),
			Total:   int64(len(todo)),
		})
	}

	idx.stats.Elapsed = time.Since(started)
	return idx, nil
}

// dep11LoadRef fetches and decodes one index file into idx.
func dep11LoadRef(ctx context.Context, ref IndexRef, idx *dep11Index, opts dep11Options, started time.Time) error {
	client := opts.Client
	if client == nil {
		client = dep11DefaultClient()
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = dep11DefaultUserAgent
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL(), nil)
	if err != nil {
		return fmt.Errorf("dep11: %s: %w", ref.URL(), err)
	}
	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dep11: %s: %w", ref.URL(), err)
	}
	defer func() {
		// Drain a little so the connection can be reused, then close. Never
		// drain the whole body: these are megabytes.
		_, _ = io.CopyN(io.Discard, resp.Body, 1<<12)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		// Not an error. A component may simply not publish DEP-11; the
		// IndexRef contract says so explicitly.
		idx.stats.NotPublished++
		return nil
	default:
		return fmt.Errorf("dep11: %s: unexpected status %s", ref.URL(), resp.Status)
	}

	counted := &dep11CountingReader{
		r:     resp.Body,
		every: dep11ProgressBytes,
	}
	if opts.Progress != nil {
		total := resp.ContentLength
		counted.tick = func(n int64) {
			dep11Report(opts, started, Progress{
				Phase:      PhaseDownload,
				Label:      "Downloading application metadata for " + ref.Suite + "/" + ref.Component,
				Item:       ref.Path(),
				BytesDone:  n,
				BytesTotal: total,
			})
		}
	}

	// The file is .gz, but sniff rather than assume. Go's transport
	// transparently decompresses a response it labelled Content-Encoding:
	// gzip, and some mirrors label a .gz file that way, which would hand us
	// plain YAML; a fixture served uncompressed in a test is the same case.
	br := bufio.NewReaderSize(counted, dep11ReadBuffer)
	var body io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("dep11: %s: gzip: %w", ref.URL(), err)
		}
		defer func() { _ = zr.Close() }()
		body = zr
	}

	limit := opts.MaxDecompressedBytes
	if limit == 0 {
		limit = dep11MaxDecompressedBytes
	}
	decoded := &dep11CountingReader{r: body, limit: limit, what: ref.Path()}

	idx.stats.Files++
	err = dep11Decode(ctx, decoded, ref, idx)
	idx.stats.CompressedBytes += counted.n
	idx.stats.DecodedBytes += decoded.n
	return err
}

// dep11Report fills in the parts of Progress that are the same for every
// report and calls the callback.
func dep11Report(opts dep11Options, started time.Time, p Progress) {
	if opts.Progress == nil {
		return
	}
	p.PhaseIndex = p.Phase.Index()
	p.PhaseCount = len(Phases)
	if p.Label == "" {
		p.Label = p.Phase.Label()
	}
	p.Elapsed = time.Since(started)
	opts.Progress(p)
}

// dep11CountingReader counts bytes, optionally caps them, and optionally
// reports progress every `every` bytes.
type dep11CountingReader struct {
	r     io.Reader
	n     int64
	limit int64 // <= 0 disables
	what  string
	every int64
	next  int64
	tick  func(int64)
}

func (c *dep11CountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.limit > 0 && c.n > c.limit {
		return n, fmt.Errorf("dep11: %s: index exceeds %d bytes decompressed", c.what, c.limit)
	}
	if c.tick != nil && c.every > 0 && c.n >= c.next {
		c.next = c.n + c.every
		c.tick(c.n)
	}
	return n, err
}
