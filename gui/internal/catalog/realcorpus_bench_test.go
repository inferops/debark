package catalog

// realcorpus_bench_test.go — the contract brief's budgets measured against the
// real Ubuntu archive, not against fixtures or a synthetic corpus (track
// T-PERF-REAL).
//
// docs/performance.md carries every number this file produces. What it adds
// over search_bench_test.go and the parser tests is the corpus: the actual
// `Packages` and DEP-11 indexes an operator's target points at, staged outside
// the repository by hack/fetch-indexes.go.
//
// # Why the corpus is not committed
//
// Ubuntu noble main+universe is ~80 MB of `Packages` and ~20 MB of DEP-11
// uncompressed. Committing that would be a mistake, and the fetcher refuses to
// write inside the module for the same reason. Every benchmark here is
// therefore skipped unless DEBARK_PERF_CORPUS names a staged corpus:
//
//	go run ./hack/fetch-indexes.go -out /tmp/corpus -only ubuntu
//	DEBARK_PERF_CORPUS=/tmp/corpus go test ./internal/catalog/ \
//	  -run '^$' -bench Perf -benchtime 5x
//
// # Why a fake transport rather than the network
//
// A benchmark that downloads 26 MB per iteration measures the mirror, not the
// code. perfTransport serves the staged bytes through the same
// http.RoundTripper seam the tests use for fixtures, so PackagesLoader.Load,
// dep11Load and catalogImpl.Build run their real code paths — decompress,
// parse, merge, reconcile, encode, load back — against real archive bytes at
// disk speed. The one measurement that genuinely needs the network is
// TestPerfCatalogBuildNetwork, which is gated separately and runs once.
//
// # Naming
//
// internal/catalog is shared with other packages, so every symbol this file
// introduces is prefixed perf.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// perfCorpusEnv names the directory hack/fetch-indexes.go staged.
	perfCorpusEnv = "DEBARK_PERF_CORPUS"
	// perfEntriesEnv overrides where TestPerfWriteCorpusJSON writes the
	// entries file that DEBARK_BENCH_CORPUS then points the search
	// benchmarks at. Defaults to <corpus>/entries-noble-amd64.json.
	perfEntriesEnv = "DEBARK_PERF_ENTRIES"
	// perfNetworkEnv opts in to the one test that actually contacts
	// archive.ubuntu.com.
	perfNetworkEnv = "DEBARK_PERF_NETWORK"
)

// perfMirror is the archive root the staged corpus was fetched from. It has to
// match hack/fetch-indexes.go's, because perfTransport maps a URL back to a
// staged filename.
const perfMirror = "http://archive.ubuntu.com/ubuntu"

// perfCorpusDir returns the staged corpus directory, or skips.
func perfCorpusDir(tb testing.TB) string {
	tb.Helper()
	dir := os.Getenv(perfCorpusEnv)
	if dir == "" {
		tb.Skipf("%s is not set; stage a corpus with `go run ./hack/fetch-indexes.go -out <dir> -only ubuntu`", perfCorpusEnv)
	}
	if _, err := os.Stat(dir); err != nil {
		tb.Skipf("%s=%s is not readable: %v", perfCorpusEnv, dir, err)
	}
	return dir
}

// perfTarget is a realistic Ubuntu 24.04 target: main and universe, release
// and -updates. docs/dev/index-formats.md §2.1 measured this union at 75,176
// distinct names, and notes that sizing anything at the release-only 70,854
// is 6% short of what a real machine sees.
func perfTarget() Target {
	return Target{
		Kind:       TargetBase,
		BaseID:     "ubuntu:24.04/desktop",
		DistroID:   "ubuntu",
		VersionID:  "24.04",
		Codename:   "noble",
		PrettyName: "Ubuntu 24.04 LTS",
		Arch:       "amd64",
		Sources: []Source{{
			Types:      []string{"deb"},
			URIs:       []string{perfMirror},
			Suites:     []string{"noble", "noble-updates"},
			Components: []string{"main", "universe"},
		}},
	}
}

// perfStagedName maps an archive URL path back to the filename
// hack/fetch-indexes.go wrote. It parses the path rather than taking an
// IndexRef so that it works for whatever the loaders ask for, including refs
// the corpus does not carry (which become a 404 — the same answer the archive
// gives for a component that publishes no such index).
func perfStagedName(urlPath string) string {
	parts := strings.Split(strings.Trim(urlPath, "/"), "/")
	at := -1
	for i, p := range parts {
		if p == "dists" {
			at = i
			break
		}
	}
	if at < 0 || len(parts) < at+4 {
		return ""
	}
	suite, component := parts[at+1], parts[at+2]
	last := parts[len(parts)-1]
	switch {
	case last == "Packages.gz":
		return "ubuntu-" + suite + "-" + component + "-Packages.gz"
	case strings.HasPrefix(last, "Components-") && strings.HasSuffix(last, ".yml.gz"):
		return "ubuntu-" + suite + "-" + component + "-" + last
	case last == "Release" || last == "InRelease":
		return "ubuntu-" + suite + "-" + last
	}
	return ""
}

// perfTransport serves the staged corpus over the loaders' own HTTP seam.
//
// Bodies are read once and held, so a benchmark that builds the catalogue
// repeatedly measures decompress-parse-index-encode rather than the page
// cache. Anything the corpus does not carry is a 404, which both loaders treat
// as "this component publishes no such index" rather than as a failure.
type perfTransport struct {
	dir string

	mu     sync.Mutex
	bodies map[string][]byte

	served int64 // bytes handed back, cumulative
	hits   int64
	misses int64
}

func perfNewTransport(dir string) *perfTransport {
	return &perfTransport{dir: dir, bodies: make(map[string][]byte)}
}

func (t *perfTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	name := perfStagedName(req.URL.Path)
	body, ok := t.body(name)
	hdr := make(http.Header)
	if !ok {
		t.mu.Lock()
		t.misses++
		t.mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     hdr,
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    req,
		}, nil
	}
	t.mu.Lock()
	t.hits++
	t.served += int64(len(body))
	t.mu.Unlock()
	hdr.Set("Content-Type", "application/octet-stream")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func (t *perfTransport) body(name string) ([]byte, bool) {
	if name == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if b, ok := t.bodies[name]; ok {
		return b, b != nil
	}
	b, err := os.ReadFile(filepath.Join(t.dir, name))
	if err != nil {
		t.bodies[name] = nil
		return nil, false
	}
	t.bodies[name] = b
	return b, true
}

func (t *perfTransport) client() *http.Client { return &http.Client{Transport: t} }

// perfGunzip expands one staged .gz into memory.
func perfGunzip(tb testing.TB, path string) []byte {
	tb.Helper()
	f, err := os.Open(path)
	if err != nil {
		tb.Skipf("staged corpus is missing %s: %v", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		tb.Fatalf("gzip %s: %v", path, err)
	}
	defer func() { _ = zr.Close() }()
	b, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// perfIndexRef is the ref a parse benchmark attributes its stanzas to.
func perfIndexRef(suite, component string, kind IndexKind) IndexRef {
	return IndexRef{URI: perfMirror, Suite: suite, Component: component, Arch: "amd64", Kind: kind}
}

// perfPackagesFiles is every staged `Packages` index, with the coordinates the
// parser needs and the sub-benchmark name to report it under.
func perfPackagesFiles() []struct {
	Name      string
	File      string
	Suite     string
	Component string
} {
	return []struct {
		Name      string
		File      string
		Suite     string
		Component string
	}{
		{"noble-main", "ubuntu-noble-main-Packages.gz", "noble", "main"},
		{"noble-universe", "ubuntu-noble-universe-Packages.gz", "noble", "universe"},
		{"noble-updates-main", "ubuntu-noble-updates-main-Packages.gz", "noble-updates", "main"},
		{"noble-updates-universe", "ubuntu-noble-updates-universe-Packages.gz", "noble-updates", "universe"},
	}
}

// perfDep11Files is every staged DEP-11 index.
func perfDep11Files() []struct {
	Name      string
	File      string
	Suite     string
	Component string
} {
	return []struct {
		Name      string
		File      string
		Suite     string
		Component string
	}{
		{"noble-main", "ubuntu-noble-main-Components-amd64.yml.gz", "noble", "main"},
		{"noble-universe", "ubuntu-noble-universe-Components-amd64.yml.gz", "noble", "universe"},
		{"noble-updates-main", "ubuntu-noble-updates-main-Components-amd64.yml.gz", "noble-updates", "main"},
		{"noble-updates-universe", "ubuntu-noble-updates-universe-Components-amd64.yml.gz", "noble-updates", "universe"},
	}
}

// perfEntriesOnce holds the parsed real catalogue for the whole test binary.
// Parsing 100 MB of indexes per benchmark would charge the cache measurements
// for work the product does once per target.
var (
	perfEntriesOnce  sync.Once
	perfEntriesValue []Entry
	perfEntriesStats PackagesResult
	perfEntriesApps  int
	perfEntriesErr   error
)

// perfBuildEntries runs the real Packages + DEP-11 path over the staged corpus
// and returns the merged, DEP-11-enriched entries — exactly what
// catalogImpl.Build hands to SaveCache.
func perfBuildEntries(tb testing.TB) []Entry {
	tb.Helper()
	dir := perfCorpusDir(tb)
	perfEntriesOnce.Do(func() {
		tr := perfNewTransport(dir)
		t := perfTarget()
		loader := PackagesLoader{HTTPClient: tr.client()}
		res, err := loader.Load(context.Background(), t, nil)
		if err != nil {
			perfEntriesErr = fmt.Errorf("parsing the staged Packages indexes: %w", err)
			return
		}
		idx, err := dep11Load(context.Background(), t.IndexRefs(), dep11Options{Client: tr.client()})
		if err != nil {
			perfEntriesErr = fmt.Errorf("decoding the staged DEP-11 indexes: %w", err)
			return
		}
		if idx != nil {
			byName := make(map[string]struct{}, len(res.Entries))
			for i := range res.Entries {
				byName[res.Entries[i].Name] = struct{}{}
			}
			idx.Reconcile(func(pkg string) bool { _, ok := byName[pkg]; return ok })
			for i := range res.Entries {
				if idx.Apply(&res.Entries[i]) {
					perfEntriesApps++
				}
			}
		}
		perfEntriesValue = res.Entries
		perfEntriesStats = res
	})
	if perfEntriesErr != nil {
		tb.Fatal(perfEntriesErr)
	}
	if len(perfEntriesValue) == 0 {
		tb.Fatal("the staged corpus produced no entries")
	}
	return perfEntriesValue
}

// perfCacheRoot points CacheRoot() at a scratch directory for the duration of
// one test or benchmark, so a measurement never writes 19 MB into the
// operator's real cache.
func perfCacheRoot(tb testing.TB) {
	tb.Helper()
	root := tb.TempDir()
	// os.UserCacheDir reads LocalAppData on Windows, XDG_CACHE_HOME then HOME
	// elsewhere. Set all three rather than branching on GOOS.
	tb.Setenv("LocalAppData", root)
	tb.Setenv("XDG_CACHE_HOME", root)
	tb.Setenv("HOME", root)
}

// TestPerfCorpusInventory records what the corpus actually is, so every number
// in docs/performance.md can name the bytes it was taken against. Run with -v.
func TestPerfCorpusInventory(t *testing.T) {
	dir := perfCorpusDir(t)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var totalGz, totalPlain int64
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(n, ".gz"):
			totalGz += fi.Size()
		case n == "MANIFEST.txt":
		default:
			totalPlain += fi.Size()
		}
		t.Logf("%-48s %12d bytes", n, fi.Size())
	}
	t.Logf("compressed total %d bytes, uncompressed total %d bytes", totalGz, totalPlain)
}

// TestPerfWriteCorpusJSON produces the file DEBARK_BENCH_CORPUS expects: a
// JSON array of catalog.Entry, built from the real indexes by the repository's
// own parsers. It is the bridge between this file and search_bench_test.go,
// and it is what makes the definition-of-done search figure a real one.
func TestPerfWriteCorpusJSON(t *testing.T) {
	dir := perfCorpusDir(t)
	entries := perfBuildEntries(t)

	out := os.Getenv(perfEntriesEnv)
	if out == "" {
		out = filepath.Join(dir, "entries-noble-amd64.json")
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(entries); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}

	apps, withApp, sections := 0, 0, map[string]int{}
	for i := range entries {
		if entries[i].IsApp {
			apps++
		}
		if entries[i].AppName != "" {
			withApp++
		}
		sections[entries[i].Section]++
	}
	t.Logf("wrote %s: %d entries, %d bytes", out, len(entries), fi.Size())
	t.Logf("indexes read %d, stanzas %d, emitted %d, duplicate names %d, missing indexes %d",
		perfEntriesStats.Indexes, perfEntriesStats.Stats.Stanzas, perfEntriesStats.Stats.Emitted,
		perfEntriesStats.DuplicateNames, len(perfEntriesStats.Missing))
	t.Logf("DEP-11: %d entries carry an application name, %d are desktop applications (%.2f%% of the catalogue)",
		withApp, apps, 100*float64(apps)/float64(len(entries)))
	t.Logf("distinct sections after stripping the component prefix: %d", len(sections))
}

// TestPerfPackagesStanzaCounts reproduces docs/dev/index-formats.md §2.1
// against today's archive, so the corpus behind every other number here is
// identified rather than assumed.
func TestPerfPackagesStanzaCounts(t *testing.T) {
	dir := perfCorpusDir(t)
	var total, names int64
	seen := make(map[string]struct{}, 80000)
	for _, f := range perfPackagesFiles() {
		body := perfGunzip(t, filepath.Join(dir, f.File))
		var n int64
		stats, err := ParsePackagesIndex(context.Background(), bytes.NewReader(body),
			perfIndexRef(f.Suite, f.Component, IndexPackages), func(e Entry) {
				n++
				seen[e.Name] = struct{}{}
			})
		if err != nil {
			t.Fatalf("%s: %v", f.File, err)
		}
		total += stats.Stanzas
		names += n
		t.Logf("%-24s %10d bytes, %6d stanzas, %6d emitted, %d malformed lines",
			f.Name, len(body), stats.Stanzas, stats.Emitted, stats.MalformedLines)
	}
	t.Logf("all staged indexes: %d stanzas, %d emitted, %d distinct package names", total, names, len(seen))
}

// BenchmarkPerfPackagesParse is the `Packages` parse phase against the real
// full-size indexes, replacing the extrapolation docs/performance.md carried.
// ns/op is per whole index; MB/s comes from SetBytes over the decompressed
// bytes, which is the unit the 10 s parse budget is spent in.
func BenchmarkPerfPackagesParse(b *testing.B) {
	dir := perfCorpusDir(b)
	for _, f := range perfPackagesFiles() {
		body := perfGunzip(b, filepath.Join(dir, f.File))
		ref := perfIndexRef(f.Suite, f.Component, IndexPackages)
		b.Run(f.Name, func(b *testing.B) {
			var stanzas int64
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var n int64
				if _, err := ParsePackagesIndex(context.Background(), bytes.NewReader(body), ref,
					func(Entry) { n++ }); err != nil {
					b.Fatal(err)
				}
				stanzas = n
			}
			b.StopTimer()
			secs := b.Elapsed().Seconds() / float64(b.N)
			b.ReportMetric(float64(stanzas), "stanzas")
			if secs > 0 {
				b.ReportMetric(float64(stanzas)/secs, "stanzas/s")
			}
			b.ReportMetric(secs*1000, "ms/index")
		})
	}
}

// BenchmarkPerfPackagesLoadAll is the whole `Packages` half of a build: every
// staged index decompressed, parsed, merged and deduplicated through
// PackagesLoader.Load, which is what the build's parse phase actually runs.
func BenchmarkPerfPackagesLoadAll(b *testing.B) {
	dir := perfCorpusDir(b)
	tr := perfNewTransport(dir)
	loader := PackagesLoader{HTTPClient: tr.client()}
	t := perfTarget()

	// Warm the transport's byte cache so the first iteration is not paying to
	// read 26 MB off disk.
	if _, err := loader.Load(context.Background(), t, nil); err != nil {
		b.Fatal(err)
	}

	var entries, stanzas int64
	var plain int64
	for _, f := range perfPackagesFiles() {
		plain += int64(len(perfGunzip(b, filepath.Join(dir, f.File))))
	}
	b.SetBytes(plain)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := loader.Load(context.Background(), t, nil)
		if err != nil {
			b.Fatal(err)
		}
		entries = int64(len(res.Entries))
		stanzas = res.Stats.Stanzas
	}
	b.StopTimer()
	b.ReportMetric(float64(entries), "entries")
	b.ReportMetric(float64(stanzas), "stanzas")
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)*1000, "ms/load")
}

// BenchmarkPerfDep11Decode is the DEP-11 decode against the real full-size
// AppStream indexes, replacing the extrapolation from a 20 MB synthetic
// corpus. docs/dev/index-formats.md §7.4 measured ~284 ms for a typed decode
// of the same data during index research; this is the shipped decoder.
func BenchmarkPerfDep11Decode(b *testing.B) {
	dir := perfCorpusDir(b)
	for _, f := range perfDep11Files() {
		body := perfGunzip(b, filepath.Join(dir, f.File))
		ref := perfIndexRef(f.Suite, f.Component, IndexDEP11)
		b.Run(f.Name, func(b *testing.B) {
			var components int
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				idx := dep11NewIndex()
				if err := dep11Decode(context.Background(), bytes.NewReader(body), ref, idx); err != nil {
					b.Fatal(err)
				}
				components = idx.Len()
			}
			b.StopTimer()
			secs := b.Elapsed().Seconds() / float64(b.N)
			b.ReportMetric(float64(components), "components")
			b.ReportMetric(secs*1000, "ms/index")
		})
	}
}

// BenchmarkPerfDep11LoadAll is both DEP-11 indexes through dep11Load — fetch
// seam, gzip sniff, streaming decode, merge — which is the build's phase 3.
func BenchmarkPerfDep11LoadAll(b *testing.B) {
	dir := perfCorpusDir(b)
	tr := perfNewTransport(dir)
	refs := perfTarget().IndexRefs()
	opts := dep11Options{Client: tr.client()}
	if _, err := dep11Load(context.Background(), refs, opts); err != nil {
		b.Fatal(err)
	}
	var plain int64
	for _, f := range perfDep11Files() {
		plain += int64(len(perfGunzip(b, filepath.Join(dir, f.File))))
	}
	var components int
	b.SetBytes(plain)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx, err := dep11Load(context.Background(), refs, opts)
		if err != nil {
			b.Fatal(err)
		}
		components = idx.Len()
	}
	b.StopTimer()
	b.ReportMetric(float64(components), "components")
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)*1000, "ms/load")
}

// BenchmarkPerfCacheSave writes the real catalogue: build the image, write it,
// fsync it, rename it. Paid once per catalogue build.
func BenchmarkPerfCacheSave(b *testing.B) {
	entries := perfBuildEntries(b)
	t := perfTarget()
	dir := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SaveCache(context.Background(), dir, CacheInput{
			Target: t, Entries: entries, ToolVersion: "debark-gui perf",
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if fi, err := os.Stat(filepath.Join(dir, "catalog.bin")); err == nil {
		b.ReportMetric(float64(fi.Size())/(1<<20), "MB-on-disk")
	}
	b.ReportMetric(float64(len(entries)), "entries")
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)*1000, "ms/save")
}

// BenchmarkPerfCacheLoad is the < 500 ms budget's real measurement: a warm
// start against a cache built from the real archive rather than an 11.8 MB
// synthetic file. One ReadFile, one CRC, full structural validation, nothing
// decoded.
func BenchmarkPerfCacheLoad(b *testing.B) {
	entries := perfBuildEntries(b)
	t := perfTarget()
	dir := b.TempDir()
	if _, err := SaveCache(context.Background(), dir, CacheInput{
		Target: t, Entries: entries, ToolVersion: "debark-gui perf",
	}); err != nil {
		b.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "catalog.bin"))
	if err != nil {
		b.Fatal(err)
	}

	var n int
	b.SetBytes(fi.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := LoadCacheDir(dir)
		if err != nil {
			b.Fatal(err)
		}
		n = c.Len()
	}
	b.StopTimer()
	b.ReportMetric(float64(fi.Size())/(1<<20), "MB-on-disk")
	b.ReportMetric(float64(n), "entries")
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)*1000, "ms/load")
}

// BenchmarkPerfCacheValidate is the same load with the bytes already resident:
// CRC plus every bounds check, and no file I/O. The difference between this
// and BenchmarkPerfCacheLoad is what the read costs.
func BenchmarkPerfCacheValidate(b *testing.B) {
	entries := perfBuildEntries(b)
	dir := b.TempDir()
	if _, err := SaveCache(context.Background(), dir, CacheInput{
		Target: perfTarget(), Entries: entries, ToolVersion: "debark-gui perf",
	}); err != nil {
		b.Fatal(err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "catalog.bin"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CacheFromBytes(buf); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)*1000, "ms/validate")
}

// BenchmarkPerfCatalogBuild is the whole build — fetch, decompress, parse,
// merge, DEP-11, reconcile, encode, fsync, load back — with the fetch served
// from the staged corpus so the number is the machine's and not the mirror's.
// The parse phase's 10 s budget is what this is measured against;
// TestPerfCatalogBuildNetwork adds the download that a first run really pays.
func BenchmarkPerfCatalogBuild(b *testing.B) {
	dir := perfCorpusDir(b)
	perfCacheRoot(b)
	tr := perfNewTransport(dir)
	t := perfTarget()

	var entries int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := New(Options{
			Packages:    PackagesLoader{HTTPClient: tr.client()},
			DEP11:       dep11Options{Client: tr.client()},
			ToolVersion: "debark-gui perf",
		})
		if err := c.Build(context.Background(), t, nil); err != nil {
			b.Fatal(err)
		}
		p, err := c.Search(context.Background(), Query{})
		if err != nil {
			b.Fatal(err)
		}
		entries = p.Total
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(entries), "entries")
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N), "s/build")
}

// TestPerfCatalogBuildPhases breaks one staged build into its phases, which is
// the split the 10 s parse budget is written against and the split the
// progress UI shows. Not a benchmark: it is one run, reported per phase.
func TestPerfCatalogBuildPhases(t *testing.T) {
	dir := perfCorpusDir(t)
	perfCacheRoot(t)
	tr := perfNewTransport(dir)
	// Warm the byte cache so "download" is the loader's work and not the
	// first read of 26 MB off a cold disk; the network test measures the real
	// transfer.
	if _, err := (PackagesLoader{HTTPClient: tr.client()}).Load(context.Background(), perfTarget(), nil); err != nil {
		t.Fatal(err)
	}

	c := New(Options{
		Packages:    PackagesLoader{HTTPClient: tr.client()},
		DEP11:       dep11Options{Client: tr.client()},
		ToolVersion: "debark-gui perf",
	})
	defer func() { _ = c.Close() }()

	started := time.Now()
	perfRunPhases(t, c, perfTarget(), started)
}

// TestPerfCatalogBuildNetwork is the only measurement here that contacts
// archive.ubuntu.com: the real first run an operator pays, download included.
// Gated because a benchmark suite must not depend on a mirror.
func TestPerfCatalogBuildNetwork(t *testing.T) {
	if os.Getenv(perfNetworkEnv) == "" {
		t.Skipf("%s is not set; this is the only test that talks to %s", perfNetworkEnv, perfMirror)
	}
	perfCacheRoot(t)
	c := New(Options{ToolVersion: "debark-gui perf"})
	defer func() { _ = c.Close() }()
	perfRunPhases(t, c, perfTarget(), time.Now())
}

// perfRunPhases runs one build and reports where the time went, by phase.
func perfRunPhases(tb testing.TB, c Catalog, t Target, started time.Time) {
	tb.Helper()

	// Phases interleave: PackagesLoader.Load reports download then parse, and
	// dep11Load then reports download and parse again for each of its four
	// refs. So a "span" here is first-seen to last-seen, and the segment table
	// below is the honest one — it charges each stretch of wall clock to
	// whichever phase was reporting during it.
	type segment struct {
		phase Phase
		start time.Duration
	}
	var (
		mu       sync.Mutex
		segs     []segment
		first    = map[Phase]time.Duration{}
		last     = map[Phase]time.Duration{}
		count    = map[Phase]int{}
		charged  = map[Phase]time.Duration{}
		reports  int
		maxBytes int64
	)
	err := c.Build(context.Background(), t, func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports++
		count[p.Phase]++
		if _, seen := first[p.Phase]; !seen {
			first[p.Phase] = p.Elapsed
		}
		last[p.Phase] = p.Elapsed
		if len(segs) == 0 || segs[len(segs)-1].phase != p.Phase {
			segs = append(segs, segment{phase: p.Phase, start: p.Elapsed})
		}
		if p.BytesDone > maxBytes {
			maxBytes = p.BytesDone
		}
	})
	total := time.Since(started)
	if err != nil {
		tb.Fatalf("build: %v", err)
	}
	for i, s := range segs {
		end := total
		if i+1 < len(segs) {
			end = segs[i+1].start
		}
		charged[s.phase] += end - s.start
	}

	tb.Logf("build finished in %v over %d progress reports in %d contiguous stretches",
		total.Round(time.Millisecond), reports, len(segs))
	for _, ph := range Phases {
		if count[ph] == 0 {
			continue
		}
		tb.Logf("  phase %-9s first %8v, last %8v, wall clock charged %8v over %d reports",
			ph, first[ph].Round(time.Millisecond), last[ph].Round(time.Millisecond),
			charged[ph].Round(time.Millisecond), count[ph])
	}

	p, err := c.Search(context.Background(), Query{})
	if err != nil {
		tb.Fatalf("search after build: %v", err)
	}
	tb.Logf("catalogue holds %d entries", p.Total)

	dir, err := CacheDir(t)
	if err == nil {
		if fi, serr := os.Stat(filepath.Join(dir, "catalog.bin")); serr == nil {
			tb.Logf("cache file %d bytes (%.2f MB)", fi.Size(), float64(fi.Size())/(1<<20))
		}
	}
}

// TestPerfResidentMemory measures what a loaded real catalogue actually
// retains: the cache buffer plus the search index, which together are the
// catalogue's whole contribution to the 400 MB idle budget.
//
// This is the catalogue's footprint inside a Go process, not the resident set
// of the desktop app — that number needs the built binary and belongs to the
// app-level pass.
func TestPerfResidentMemory(t *testing.T) {
	entries := perfBuildEntries(t)
	dir := t.TempDir()
	if _, err := SaveCache(context.Background(), dir, CacheInput{
		Target: perfTarget(), Entries: entries, ToolVersion: "debark-gui perf",
	}); err != nil {
		t.Fatal(err)
	}

	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	report := func(what string, delta int64, n int) {
		t.Logf("%-34s %7.2f MB (%.0f B/entry)", what, float64(delta)/(1<<20), float64(delta)/float64(n))
	}

	base := heap()
	c, err := LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := c.Len()
	afterFile := heap()

	// Exactly what catalogImpl.load does: materialise every row, then build
	// the search index over it. The CacheFile itself is dropped afterwards —
	// the index aliases the entries, not the file — so the steady state is
	// entries plus index.
	rows := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		e, ok := c.Entry(i)
		if !ok {
			t.Fatalf("entry %d did not materialise", i)
		}
		rows = append(rows, e)
	}
	afterEntries := heap()

	ix := newSearchIndex(rows)
	afterIndex := heap()

	t.Logf("real catalogue: %d entries, %d bytes on disk", n, c.Size())
	report("cache file resident", afterFile-base, n)
	report("+ entries materialised", afterEntries-afterFile, n)
	report("+ search index", afterIndex-afterEntries, n)
	report("loaded catalogue, steady state", afterIndex-afterFile, n)
	t.Logf("search index self-reported footprint %.2f MB", float64(ix.memoryBytes())/(1<<20))
	runtime.KeepAlive(c)
	runtime.KeepAlive(rows)
	runtime.KeepAlive(ix)
}

// ---------------------------------------------------------------------------
// The load path's effect on the *process*, not on the live heap
// ---------------------------------------------------------------------------
//
// TestPerfResidentMemory above measures HeapAlloc deltas: what the catalogue
// retains. That is not the same question as what the operating system sees,
// and the two answers differ by tens of megabytes.
//
// The app-level memory pass measured the real desktop process growing 65.6 MB
// across a catalogue load whose retained footprint scales to ~36.4 MB, and
// left the ~29 MB difference as a hypothesis it could not test from /proc:
// /proc cannot tell "the catalogue is holding this" from "the Go runtime has
// not handed the pages back". runtime.MemStats can, and these are the fields
// that do it:
//
//	HeapAlloc                live bytes — what is genuinely retained
//	HeapSys - HeapReleased   address space this process is charged for
//	HeapIdle - HeapReleased  spans the runtime holds and is not using
//
// The last of those is the whole question. It is what a peak allocation
// leaves behind, it is invisible to HeapAlloc, and it is exactly what
// debug.FreeOSMemory returns.

// perfAppCacheEnv names a directory the *application* wrote — one of
// <cache root>/debark-gui/catalog/<key>. It holds the app's own default
// target, 85,565 entries, rather than the staged corpus's 75,176.
const perfAppCacheEnv = "DEBARK_PERF_APPCACHE"

// perfAppCacheDir returns the app-written cache directory, or skips.
func perfAppCacheDir(tb testing.TB) string {
	tb.Helper()
	dir := os.Getenv(perfAppCacheEnv)
	if dir == "" {
		tb.Skipf("%s is not set; point it at a <cache>/debark-gui/catalog/<key> directory the app wrote", perfAppCacheEnv)
	}
	if _, err := os.Stat(filepath.Join(dir, CacheBinName)); err != nil {
		tb.Skipf("%s=%s holds no %s: %v", perfAppCacheEnv, dir, CacheBinName, err)
	}
	// Cleaned because the comparison below is against a path CacheDir built
	// with filepath.Join, and a forward-slashed value from the environment
	// would never equal it on Windows.
	return filepath.Clean(dir)
}

// perfPointCacheRootAt makes CacheRoot resolve to dir's grandparent, so that
// CacheDir(target) is dir and the load runs through LoadCache exactly as the
// application does rather than through LoadCacheDir.
//
// os.UserCacheDir reads XDG_CACHE_HOME on Linux and LocalAppData on Windows;
// both are set so the same test runs on machine A and machine B.
func perfPointCacheRootAt(tb testing.TB, dir string) {
	tb.Helper()
	root := filepath.Dir(filepath.Dir(filepath.Dir(dir))) // <root>/debark-gui/catalog/<key>
	tb.Setenv("XDG_CACHE_HOME", root)
	tb.Setenv("LocalAppData", root)
	got, err := CacheRoot()
	if err != nil {
		tb.Fatalf("CacheRoot: %v", err)
	}
	if want := filepath.Join(root, "debark-gui", "catalog"); got != want {
		tb.Skipf("this platform's user cache directory is not redirectable by environment: CacheRoot is %s, wanted %s", got, want)
	}
}

// perfMem is one observation of the process's memory, from both sides of the
// question: the Go runtime's own accounting, and what the kernel charges.
type perfMem struct {
	what       string
	heapAlloc  uint64
	heapSys    uint64
	heapIdle   uint64
	heapInuse  uint64
	heapRel    uint64
	sys        uint64
	totalAlloc uint64
	numGC      uint32
	rss        uint64 // 0 where procfs is not available
	// unreturned is HeapIdle - HeapReleased: heap the runtime holds and is
	// not using, which is the quantity this whole section exists to measure.
	unreturned uint64
}

// perfReadMem snapshots MemStats and, on Linux, VmRSS. It does not force a
// GC: the point of most of these observations is the state the process is
// actually in, and that is not a post-GC state.
func perfReadMem(what string) perfMem {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	o := perfMem{
		what:       what,
		heapAlloc:  m.HeapAlloc,
		heapSys:    m.HeapSys,
		heapIdle:   m.HeapIdle,
		heapInuse:  m.HeapInuse,
		heapRel:    m.HeapReleased,
		sys:        m.Sys,
		totalAlloc: m.TotalAlloc,
		numGC:      m.NumGC,
		rss:        perfSelfRSS(),
	}
	if o.heapIdle > o.heapRel {
		o.unreturned = o.heapIdle - o.heapRel
	}
	return o
}

// perfSelfRSS reads VmRSS out of /proc/self/status, in bytes. It returns 0
// anywhere without procfs — Windows, notably — so every RSS column in this
// section is blank on machine A and populated on machine B.
func perfSelfRSS() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		var kib uint64
		if _, err := fmt.Sscanf(f[1], "%d", &kib); err != nil {
			return 0
		}
		return kib * 1024
	}
	return 0
}

func perfLogMem(tb testing.TB, obs ...perfMem) {
	tb.Helper()
	mb := func(v uint64) string { return fmt.Sprintf("%8.2f", float64(v)/1e6) }
	tb.Logf("%-38s %9s %9s %9s %9s %9s %9s %9s %5s",
		"checkpoint", "HeapAlloc", "HeapInuse", "HeapIdle", "HeapRel", "unreturned", "Sys", "RSS", "GCs")
	for _, o := range obs {
		rss := "       -"
		if o.rss != 0 {
			rss = mb(o.rss)
		}
		tb.Logf("%-38s %9s %9s %9s %9s %9s %9s %9s %5d",
			o.what, mb(o.heapAlloc), mb(o.heapInuse), mb(o.heapIdle), mb(o.heapRel),
			mb(o.unreturned), mb(o.sys), rss, o.numGC)
	}
}

// TestPerfLoadMemoryProfile answers the question /proc left open: after a
// catalogue load, how much of what the process holds is the catalogue, and how
// much is heap the Go runtime has not returned to the operating system?
//
// It runs the application's own warm-start path — New, then LoadTarget, which
// is catalogImpl.load — against the cache the application itself wrote, and
// reports MemStats at four checkpoints. Nothing forces a GC before the "after
// load" reading, because the running app does not either.
func TestPerfLoadMemoryProfile(t *testing.T) {
	dir := perfAppCacheDir(t)
	meta, err := ReadCacheMeta(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", CacheMetaName, err)
	}
	perfPointCacheRootAt(t, dir)
	target := meta.Target
	if got, err := CacheDir(target); err != nil || got != dir {
		t.Skipf("the cache directory does not derive from its own meta.json target: CacheDir is %q, err %v, wanted %q", got, err, dir)
	}

	runtime.GC()
	runtime.GC()
	base := perfReadMem("0. process baseline, before load")

	c := New(Options{ToolVersion: "debark-gui perf"})
	lt, ok := c.(interface{ LoadTarget(Target) error })
	if !ok {
		t.Fatal("the catalogue implementation no longer exposes LoadTarget")
	}
	start := time.Now()
	if err := lt.LoadTarget(target); err != nil {
		t.Fatalf("LoadTarget: %v", err)
	}
	loadWall := time.Since(start)
	afterLoad := perfReadMem("1. after load, no GC forced")

	// What the catalogue genuinely retains: a full GC with it still reachable.
	runtime.GC()
	runtime.GC()
	afterGC := perfReadMem("2. + two forced GCs (live heap)")

	// And what a page release then hands back.
	freeStart := time.Now()
	debug.FreeOSMemory()
	freeWall := time.Since(freeStart)
	afterFree := perfReadMem("3. + debug.FreeOSMemory")

	perfLogMem(t, base, afterLoad, afterGC, afterFree)

	n := 0
	if pc, ok := c.(interface{ PackageCount() int }); ok {
		n = pc.PackageCount()
	}
	t.Logf("catalogue: %d entries, %s is %d bytes on disk, key %s", n, CacheBinName, meta.BinSize, meta.CacheKey)
	t.Logf("LoadTarget wall clock            %.1f ms", float64(loadWall.Microseconds())/1000)
	t.Logf("debug.FreeOSMemory wall clock    %.1f ms", float64(freeWall.Microseconds())/1000)
	t.Logf("live heap the catalogue retains  %.2f MB  (HeapAlloc 2 - HeapAlloc 0)",
		float64(int64(afterGC.heapAlloc)-int64(base.heapAlloc))/1e6)
	t.Logf("garbage still uncollected at 1   %.2f MB  (HeapAlloc 1 - HeapAlloc 2)",
		float64(int64(afterLoad.heapAlloc)-int64(afterGC.heapAlloc))/1e6)
	t.Logf("heap held but unused at 1        %.2f MB  (HeapIdle - HeapReleased)",
		float64(afterLoad.unreturned)/1e6)
	t.Logf("heap held but unused at 2        %.2f MB", float64(afterGC.unreturned)/1e6)
	t.Logf("returned by FreeOSMemory         %.2f MB  (HeapReleased 3 - HeapReleased 1)",
		float64(int64(afterFree.heapRel)-int64(afterLoad.heapRel))/1e6)
	if base.rss != 0 {
		t.Logf("process RSS across the load      %.2f MB -> %.2f MB -> %.2f MB after release",
			float64(base.rss)/1e6, float64(afterLoad.rss)/1e6, float64(afterFree.rss)/1e6)
	}

	// A search, so that nothing above can be optimised away and so the
	// numbers describe a catalogue that works.
	pg, err := c.Search(context.Background(), Query{Text: "browser", Limit: 50})
	if err != nil {
		t.Fatalf("Search after load: %v", err)
	}
	t.Logf("post-load search: %d matches, %d rows returned", pg.Total, len(pg.Entries))
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
