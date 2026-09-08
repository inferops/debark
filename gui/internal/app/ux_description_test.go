package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferops/debark/gui/internal/catalog"
)

func TestUXPackageDetailsDescriptionSurvivesWarmCache(t *testing.T) {
	a, target := newWarmApp(t)
	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	const body = "Short summary\nFull existing description.\n\nSecond paragraph."
	_, err = catalog.SaveCache(context.Background(), dir, catalog.CacheInput{Target: target, Entries: []catalog.Entry{
		{Name: "described", Summary: "Short summary", Description: body, DescriptionTruncated: true, Arch: "amd64"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	selected := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: target.BaseID, Arch: target.Arch})
	if selected.Error != nil {
		t.Fatal(selected.Error)
	}
	detail := a.GetPackage("described")
	if detail.Error != nil || !detail.Found || detail.Package.Description != body || !detail.Package.DescriptionTruncated {
		t.Fatalf("GetPackage did not project authoritative description: %+v", detail)
	}
	page := a.SearchPackages(SearchQuery{Text: "described", Limit: 10})
	wire, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if page.Error != nil || len(page.Rows) != 1 || strings.Contains(string(wire), "description") || strings.Contains(string(wire), "Full existing") {
		t.Fatalf("SearchPackages grew a description payload: %s", wire)
	}
}
