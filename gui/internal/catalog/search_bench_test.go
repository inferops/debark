// Package catalog_test holds the search benchmarks.
//
// They live in the external test package because they build their corpus with
// internal/catalog/fake, which imports internal/catalog: a benchmark written in
// package catalog would be an import cycle. The index itself is reached through
// the export hook in search_test.go.
//
// # What these measure, and why they are shaped this way
//
// The contract budget is "keystroke -> rendered results in under 100 ms p95,
// including the Wails bridge round-trip". Only the frontend can measure the
// round-trip; what this file measures is the Go half, which is the half that
// scales with the corpus. `go test -bench` reports a mean, and a mean is the
// wrong statistic for a budget written as a p95, so each benchmark times every
// individual query and reports p50/p95/p99 in milliseconds as custom metrics
// alongside the usual ns/op.
//
// # Pointing this at a real universe index
//
// The definition of done requires this number re-measured against a real
// Ubuntu main+universe catalogue. Set DEBARK_BENCH_CORPUS to a file holding a
// JSON array of catalog.Entry and every benchmark below runs against that
// instead of the fake, with no other change:
//
//	DEBARK_BENCH_CORPUS=/tmp/noble-amd64.json \
//	  go test ./internal/catalog/ -run '^$' -bench SearchIndex -benchtime 300x
//
// Producing that file is one call to json.NewEncoder over whatever the
// Packages/DEP-11 parsers built — deliberately a plain slice of the frozen
// Entry type, so it does not depend on the on-disk cache format.
package catalog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/catalog/fake"
)

// searchBenchCorpusEnv names a JSON array of catalog.Entry to benchmark
// against instead of the generated corpus.
const searchBenchCorpusEnv = "DEBARK_BENCH_CORPUS"

// searchBenchSize is the corpus size when generating one: Ubuntu main +
// universe is roughly 70,000 binary packages, and that is the number every
// budget in the contract brief is written against.
const searchBenchSize = 70000

var (
	searchBenchIndex   *catalog.SearchIndexForBench
	searchBenchEntries []catalog.Entry
	searchBenchSource  string
)

// searchBenchLoad builds the index once for the whole benchmark binary.
// Building it per benchmark would charge every case for a sort and a 5 MB
// arena that the product pays for once per catalogue build.
func searchBenchLoad(b *testing.B) *catalog.SearchIndexForBench {
	b.Helper()
	if searchBenchIndex != nil {
		return searchBenchIndex
	}
	if path := os.Getenv(searchBenchCorpusEnv); path != "" {
		f, err := os.Open(path)
		if err != nil {
			b.Fatalf("opening %s=%s: %v", searchBenchCorpusEnv, path, err)
		}
		defer f.Close()
		var entries []catalog.Entry
		if err := json.NewDecoder(f).Decode(&entries); err != nil {
			b.Fatalf("decoding %s: %v", path, err)
		}
		searchBenchEntries = entries
		searchBenchSource = path
	} else {
		f := fake.New(searchBenchSize)
		entries := make([]catalog.Entry, 0, f.Len())
		for off := 0; off < f.Len(); off += catalog.MaxPageSize {
			p, err := f.Search(context.Background(), catalog.Query{Offset: off, Limit: catalog.MaxPageSize})
			if err != nil {
				b.Fatalf("draining the fake corpus: %v", err)
			}
			entries = append(entries, p.Entries...)
		}
		searchBenchEntries = entries
		searchBenchSource = fmt.Sprintf("fake.New(%d)", searchBenchSize)
	}
	searchBenchIndex = catalog.NewSearchIndexForBench(searchBenchEntries)
	return searchBenchIndex
}

// searchBenchClockNs is the measured granularity of the monotonic clock, in
// nanoseconds.
//
// It has to be measured rather than assumed: Go's nanotime ticks every 0.5 ms
// on Windows and every few tens of nanoseconds on Linux. Timing a single 2 ms
// query against a 0.5 ms tick produces a distribution made mostly of rounding —
// which is what made an earlier version of this file report a p50 of exactly
// zero for every query under half a millisecond.
var searchBenchClockNs = searchBenchClockGranularity()

func searchBenchClockGranularity() int64 {
	best := int64(1) << 62
	for i := 0; i < 50; i++ {
		t0 := time.Now()
		var d time.Duration
		for d == 0 {
			d = time.Since(t0)
		}
		if int64(d) < best {
			best = int64(d)
		}
	}
	if best < 1 {
		best = 1
	}
	return best
}

// searchBenchRun measures the latency distribution of one query.
//
// Queries are timed in groups just large enough that the clock's granularity is
// under 5% of a sample, and the reported percentiles are per-query times
// derived from those groups. On a fine-grained clock the group is one query and
// the percentiles are the true tail; on Windows a 2 ms query is timed a handful
// at a time, which smooths the extreme tail but keeps every number honest. The
// group size and the measured clock granularity are reported alongside the
// percentiles so a reader can tell which regime a number came from.
func searchBenchRun(b *testing.B, q catalog.Query) {
	ix := searchBenchLoad(b)
	ctx := context.Background()

	// Warm up and estimate, so the first timed sample is not paying for a cold
	// page fault over the arena and the group size is based on a real number.
	warm := time.Now()
	reps := 0
	for reps < 8 || time.Since(warm) < 20*time.Millisecond {
		if _, err := ix.Search(ctx, q); err != nil {
			b.Fatal(err)
		}
		reps++
	}
	est := int64(time.Since(warm)) / int64(reps)

	group := 1
	if est > 0 {
		if want := int(20*searchBenchClockNs/est) + 1; want > group {
			group = want
		}
	}
	if group > b.N {
		group = b.N
	}
	if group < 1 {
		group = 1
	}

	samples := make([]time.Duration, 0, b.N/group+1)
	total := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i+group <= b.N; i += group {
		t0 := time.Now()
		for j := 0; j < group; j++ {
			p, err := ix.Search(ctx, q)
			if err != nil {
				b.Fatal(err)
			}
			total = p.Total
		}
		samples = append(samples, time.Since(t0)/time.Duration(group))
	}
	b.StopTimer()

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	ms := func(p float64) float64 {
		if len(samples) == 0 {
			return 0
		}
		k := int(float64(len(samples)-1) * p)
		return float64(samples[k]) / float64(time.Millisecond)
	}
	b.ReportMetric(ms(0.50), "p50-ms")
	b.ReportMetric(ms(0.95), "p95-ms")
	b.ReportMetric(ms(0.99), "p99-ms")
	b.ReportMetric(float64(total), "matches")
	b.ReportMetric(float64(group), "queries/sample")
	b.ReportMetric(float64(searchBenchClockNs), "clock-ns")
}

// BenchmarkSearchIndexKeystroke1 is the first character the operator types.
// The cheapest needle and the largest match set: strings.Index degenerates to
// a byte scan, and almost everything matches, so this is the case where the
// ranking work rather than the searching work dominates.
func BenchmarkSearchIndexKeystroke1(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "l"})
}

// BenchmarkSearchIndexKeystroke2 is the second keystroke.
func BenchmarkSearchIndexKeystroke2(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "li"})
}

// BenchmarkSearchIndexLib is the worst case the contract brief names: "lib"
// matches roughly 32,000 rows of a real main+universe index, so it is the
// query where a naive implementation allocates a slice of every match to
// return fifty of them.
func BenchmarkSearchIndexLib(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "lib"})
}

// BenchmarkSearchIndexLibDeepPage is the same worst case scrolled to the end
// of its result set: the virtualiser may ask for any offset, and an
// implementation that reaches offset 30,000 by walking a materialised match
// list would show it here.
func BenchmarkSearchIndexLibDeepPage(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "lib", Offset: 30000})
}

// BenchmarkSearchIndexNoMatch is a long query that matches nothing — the state
// the search box is in whenever the operator is halfway through typing a name
// that does not exist. Every haystack, including every summary, is scanned in
// full.
func BenchmarkSearchIndexNoMatch(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "zzzz-no-such-package-anywhere-in-this-archive"})
}

// BenchmarkSearchIndexWord is an ordinary word an operator looking for an
// application would type — the case DEP-11 exists to serve.
func BenchmarkSearchIndexWord(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "browser"})
}

// BenchmarkSearchIndexBrowse is the picker's opening state: no text, no
// filter, first page. It must be O(page) and not O(catalogue).
func BenchmarkSearchIndexBrowse(b *testing.B) {
	searchBenchRun(b, catalog.Query{})
}

// BenchmarkSearchIndexBrowseDeepPage is that same browse scrolled 60,000 rows
// down, which the contract requires to scroll without dropped frames.
func BenchmarkSearchIndexBrowseDeepPage(b *testing.B) {
	searchBenchRun(b, catalog.Query{Offset: 60000})
}

// BenchmarkSearchIndexCategory is a section-filtered browse — the sidebar
// click.
func BenchmarkSearchIndexCategory(b *testing.B) {
	searchBenchRun(b, catalog.Query{Category: catalog.SectionCategoryID("libs")})
}

// BenchmarkSearchIndexCategoryText is a sidebar click plus typing, which is
// the filter combination the picker actually produces.
func BenchmarkSearchIndexCategoryText(b *testing.B) {
	searchBenchRun(b, catalog.Query{Category: catalog.SectionCategoryID("libs"), Text: "lib"})
}

// BenchmarkSearchIndexAppsOnly is the applications tier: 3% of the archive,
// which is the point of the filter.
func BenchmarkSearchIndexAppsOnly(b *testing.B) {
	searchBenchRun(b, catalog.Query{AppsOnly: true, Text: "e"})
}

// BenchmarkSearchIndexMaxPage asks for the largest page the contract allows,
// which is what bounds the per-query allocation.
func BenchmarkSearchIndexMaxPage(b *testing.B) {
	searchBenchRun(b, catalog.Query{Text: "lib", Limit: catalog.MaxPageSize})
}

// BenchmarkSearchIndexParallel runs the worst-case query from several
// goroutines at once, which is the shape of the bridge under a fast typist:
// the index takes no lock, so this must scale rather than serialise.
func BenchmarkSearchIndexParallel(b *testing.B) {
	ix := searchBenchLoad(b)
	q := catalog.Query{Text: "lib"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			if _, err := ix.Search(ctx, q); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSearchIndexBuild is the cost of building the index itself, which is
// PhaseIndex of a catalogue build and is paid once per target, not per
// keystroke.
func BenchmarkSearchIndexBuild(b *testing.B) {
	searchBenchLoad(b)
	entries := searchBenchEntries
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix := catalog.NewSearchIndexForBench(entries)
		if ix.MemoryBytesForBench() == 0 {
			b.Fatal("index reported no footprint")
		}
	}
}

// TestSearchIndexFootprint is not a benchmark but belongs with them: it prints
// the index's resident footprint over a full-size corpus, which is the number
// the 400 MB idle budget is spent against. Run it with -v.
func TestSearchIndexFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 70,000-entry corpus")
	}
	var b testing.B
	ix := searchBenchLoad(&b)
	n := len(searchBenchEntries)
	if n == 0 {
		t.Fatal("empty corpus")
	}
	bytes := ix.MemoryBytesForBench()
	t.Logf("corpus %s: %d entries, index footprint %.2f MB (%.1f B/entry), excluding the entries themselves",
		searchBenchSource, n, float64(bytes)/(1<<20), float64(bytes)/float64(n))
	// A guard rather than a measurement: 512 bytes per entry would be 35 MB at
	// 70,000 rows and a sign that the index had started keeping whole records.
	if per := bytes / int64(n); per > 512 {
		t.Errorf("index footprint is %d bytes per entry, which is too much to hold beside the entries", per)
	}
}
