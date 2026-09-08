package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestUXUndoRetainsOriginalVendorInputsWithoutReturningCredentials(t *testing.T) {
	a, _, events := newRedactionApp(t)
	redTrayWithCredentials(t, a)
	before := a.SelectionKeys()
	page := a.SelectionPage(0, 100)
	key := page.Entries[1].Key
	if key == redCredURL || !strings.HasPrefix(key, "url:") {
		t.Fatalf("credentialed URL has no opaque key: %q", key)
	}
	removed := a.RemovePackages([]string{key})
	if removed.Removed != 1 || removed.UndoToken == "" {
		t.Fatalf("remove: %+v", removed)
	}
	restored := a.UndoSelection(removed.UndoToken)
	if restored.Error != nil || restored.UndoToken != "" || !slices.Equal(before.Keys, a.SelectionKeys().Keys) {
		t.Fatalf("restore: %+v", restored)
	}
	if restored.Revision <= removed.Revision {
		t.Fatal("Undo did not advance the revision")
	}
	spec, err := a.buildSpec(BuildOptions{OutputDir: t.TempDir(), NoSign: true})
	if err != nil || len(spec.URLs) != 3 || spec.URLs[0].URL != redCredURL || spec.URLs[0].SHA256 != redSHA {
		t.Fatalf("Undo lost the original vendor reference/digest: %+v, %+v", spec.URLs, err)
	}
	cleared := a.ClearSelection()
	if cleared.Total != 0 || cleared.UndoToken == "" {
		t.Fatalf("clear: %+v", cleared)
	}
	if r := a.UndoSelection(cleared.UndoToken); r.Error != nil || r.Total != before.Total {
		t.Fatalf("clear Undo: %+v", r)
	}
	if r := a.UndoSelection(cleared.UndoToken); r.Error == nil {
		t.Fatal("Undo token was reusable")
	}
	views := []any{a.Selection(), a.SelectionPage(0, 100), a.SelectionKeys(), events(EventSelectionChanged)}
	for _, view := range views {
		wire, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), redSecret) || strings.Contains(string(wire), redPresigned) {
			t.Fatalf("selection/Undo projection exposed credentials: %s", wire)
		}
	}
}

func TestUXUndoInvalidatesAfterInterveningMutations(t *testing.T) {
	for _, mutation := range []string{"add", "duplicate", "target", "clear-target", "digest"} {
		t.Run(mutation, func(t *testing.T) {
			a, _ := newApp(t)
			redTrayWithCredentials(t, a)
			removed := a.RemovePackages([]string{"jq"})
			if removed.UndoToken == "" {
				t.Fatal("no Undo")
			}
			switch mutation {
			case "add":
				a.AddPackages([]string{"git"})
			case "duplicate":
				a.AddURLs([]URLInput{{URL: redCredURL, SHA256: redSHA}})
			case "target":
				if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/server", Arch: "amd64"}); r.Error != nil {
					t.Fatal(r.Error)
				}
			case "clear-target":
				a.ClearTarget()
			case "digest":
				a.AddURLs([]URLInput{{URL: redCredURL, SHA256: strings.Repeat("4", 64)}})
			}
			before := a.SelectionKeys()
			r := a.UndoSelection(removed.UndoToken)
			if r.Error == nil || r.Error.Hint == "" || r.UndoToken != "" || !slices.Equal(a.SelectionKeys().Keys, before.Keys) {
				t.Fatalf("stale Undo mutated selection: %+v", r)
			}
		})
	}
}

func TestUXUnchangedTargetRetainsSelectionGenerationAndUndo(t *testing.T) {
	a, _ := newApp(t)
	sel := TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}
	first := a.SelectTarget(sel)
	a.AddPackages([]string{"git", "gimp"})
	removed := a.RemovePackages([]string{"git"})
	second := a.SelectTarget(sel)
	if second.Error != nil || first.Target.Generation != second.Target.Generation || a.Selection().Total != 1 || a.Selection().UndoToken != removed.UndoToken {
		t.Fatalf("revisit reset the current draft: %+v / %+v", first, second)
	}
}
