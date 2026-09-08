package catalog

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUXDescriptionOnlyHydratesForDetails(t *testing.T) {
	const body = "A short summary\nAn extended body with a detail-only sentinel.\n\n  preformatted text"
	image, err := cacheBuildImage([]Entry{{Name: "example", Summary: "A short summary", Description: body}})
	if err != nil {
		t.Fatal(err)
	}
	cf, err := CacheFromBytes(image)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := cf.Entry(0)
	if !ok || row.Description != "" || row.descriptionData == "" {
		t.Fatalf("page materialised description: %+v", row)
	}
	ix := newSearchIndex([]Entry{row})
	page, err := ix.Search(context.Background(), Query{Text: "detail-only", Limit: 20})
	if err != nil || page.Total != 0 {
		t.Fatalf("extended text changed search matches: %+v %v", page, err)
	}
	got, ok, err := ix.Get(context.Background(), "example")
	if err != nil || !ok || got.Description != body || got.Summary != "A short summary" || got.DescriptionTruncated {
		t.Fatalf("detail lost supplied text: %+v %v", got, err)
	}
	if row.Description != "" {
		t.Fatal("Get mutated the shared search row")
	}
	got, ok = cf.Get("example")
	if !ok || got.Description != body {
		t.Fatal("direct cache Get lost description")
	}
	binary.LittleEndian.PutUint32(image[8:12], 1)
	if _, err := CacheFromBytes(image); !errors.Is(err, ErrCacheVersion) {
		t.Fatalf("version 1 cache was not invalidated: %v", err)
	}
}

func TestUXDescriptionParserAndCacheBounds(t *testing.T) {
	text := "Package: example\nDescription: summary\n " + strings.Repeat("界", 30000) + "\n tail beyond limit\n\n"
	entries, _ := pkgParseAll(t, []byte(text), pkgRef("noble", "main"))
	if len(entries) != 1 || len(entries[0].Description) > packagesMaxDescriptionBytes || !entries[0].DescriptionTruncated || !utf8.ValidString(entries[0].Description) {
		t.Fatal("parser did not bound extended UTF-8 metadata visibly")
	}
	entries[0].Description = strings.Repeat("界", 30000)
	entries[0].DescriptionTruncated = false
	image, err := cacheBuildImage(entries)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := CacheFromBytes(image)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cf.Get("example")
	if len(got.Description) > packagesMaxDescriptionBytes || !got.DescriptionTruncated || !utf8.ValidString(got.Description) {
		t.Fatal("cache writer did not preserve its independent detail bound")
	}
}

func TestUXDescriptionInvalidCompressedTextIsVisibleFallback(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", packagesMaxDescriptionBytes+1), string([]byte{0xff})} {
		var packed bytes.Buffer
		zw := zlib.NewWriter(&packed)
		_, _ = zw.Write([]byte(text))
		_ = zw.Close()
		entry := entryWithDescription(Entry{Summary: "still searchable", descriptionData: packed.String()})
		if entry.Description != "" || !entry.DescriptionTruncated || entry.Summary != "still searchable" {
			t.Fatal("untrusted compressed metadata did not degrade to the summary")
		}
	}
	entry := entryWithDescription(Entry{descriptionData: "not a zlib stream"})
	if entry.Description != "" || !entry.DescriptionTruncated {
		t.Fatal("invalid stream was treated as a description")
	}
}

func TestUXDescriptionMergedBudgetKeepsSummaryAndReplacementOrder(t *testing.T) {
	merger := newPackagesMerger(0)
	body := strings.Repeat("x", packagesMaxDescriptionBytes)
	for i := 0; i < packagesMaxDescriptionsBytes/packagesMaxDescriptionBytes; i++ {
		merger.add(Entry{Name: fmt.Sprintf("package-%d", i), Description: body})
	}
	merger.add(Entry{Name: "overflow", Summary: "summary stays", Description: body})
	overflow := merger.entries[len(merger.entries)-1]
	if overflow.Description != "" || !overflow.DescriptionTruncated || overflow.Summary != "summary stays" {
		t.Fatal("aggregate budget did not preserve the selectable package with a visible limitation")
	}
	merger.add(Entry{Name: "package-0", Summary: "later source"})
	merger.add(Entry{Name: "reclaimed", Description: body})
	if merger.entries[0].Summary != "later source" || merger.entries[len(merger.entries)-1].Description != body || merger.descriptionBytes > packagesMaxDescriptionsBytes {
		t.Fatal("replacement stopped using source order or failed to reclaim its description budget")
	}
}

func BenchmarkUXDescriptionCache70000(b *testing.B) {
	entries := make([]Entry, 70000)
	for i := range entries {
		entries[i] = Entry{Name: fmt.Sprintf("package-%05d", i), Summary: "Short list summary", Description: "Short list summary\n" + strings.Repeat("Extended package metadata for the Details view. ", 40)}
	}
	image, err := cacheBuildImage(entries)
	if err != nil {
		b.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		name := "CacheAnd50Rows"
		if full {
			name = "FullSearchIndex"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				cf, err := CacheFromBytes(image)
				if err != nil {
					b.Fatal(err)
				}
				count := 50
				if full {
					count = cf.Len()
				}
				rows := make([]Entry, count)
				for i := range rows {
					rows[i], _ = cf.Entry(i)
					if rows[i].Description != "" {
						b.Fatal("page inflated descriptions")
					}
				}
				if full {
					ix := newSearchIndex(rows)
					page, err := ix.Search(context.Background(), Query{Text: "package", Limit: 50})
					if err != nil || len(page.Entries) != 50 {
						b.Fatalf("search failed: %+v %v", page, err)
					}
				}
			}
			b.ReportMetric(float64(len(image))/1024/1024, "cache-MiB")
		})
	}
}
