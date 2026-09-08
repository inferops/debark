package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"

	"github.com/inferops/debark/gui/internal/catalog"
	catfake "github.com/inferops/debark/gui/internal/catalog/fake"
	"github.com/inferops/debark/gui/internal/cliadapter"
	clifake "github.com/inferops/debark/gui/internal/cliadapter/fake"
)

type uxControlledBuild struct {
	*clifake.Adapter
	entered   chan cliadapter.BuildSpec
	release   chan struct{}
	cancelled chan struct{}
	result    *buildjob.BuildResult
	err       error
}

func (c *uxControlledBuild) Build(ctx context.Context, spec cliadapter.BuildSpec, _ cliadapter.EventSink) (*buildjob.BuildResult, error) {
	c.entered <- spec
	select {
	case <-ctx.Done():
		close(c.cancelled)
		<-c.release
	case <-c.release:
	}
	return c.result, c.err
}

func uxJobApp(t *testing.T) (*App, *uxControlledBuild) {
	t.Helper()
	cli := &uxControlledBuild{Adapter: clifake.New(), entered: make(chan cliadapter.BuildSpec, 1), release: make(chan struct{}), cancelled: make(chan struct{}), result: &buildjob.BuildResult{BundlePath: "test-bundle", ExitClass: buildjob.ExitSuccess}}
	a := New(Deps{CLI: cli, Catalog: catfake.NewDefault()})
	a.Startup(context.Background())
	t.Cleanup(func() {
		select {
		case <-cli.release:
		default:
			close(cli.release)
		}
		a.Shutdown(context.Background())
	})
	if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	a.AddPackages([]string{"git", "gimp"})
	return a, cli
}

func TestUXActiveBuildBlocksConflictingBindingsAndCapturesDraft(t *testing.T) {
	a, cli := uxJobApp(t)
	before := a.LifecycleStatus()
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatal(r.Error)
	}
	spec := <-cli.entered
	state := a.BuildStatus()
	if state.TargetGeneration != before.TargetGeneration || state.SelectionRevision != before.SelectionRevision || state.TargetID != spec.BaseID || state.ItemCount != len(spec.Packages) {
		t.Fatalf("checkpoint and process request disagree: %+v / %+v", state, spec)
	}
	for name, run := range map[string]func() *UIError{
		"add":    func() *UIError { return a.AddPackages([]string{"curl"}).Error },
		"url":    func() *UIError { return a.AddURLs([]URLInput{{URL: redCredURL}}).Error },
		"paste":  func() *UIError { return a.AddPackageList("curl").Error },
		"remove": func() *UIError { return a.RemovePackages([]string{"git"}).Error },
		"clear":  func() *UIError { return a.ClearSelection().Error },
		"undo":   func() *UIError { return a.UndoSelection("stale").Error },
		"target": func() *UIError {
			return a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/server", Arch: "amd64"}).Error
		},
		"clear-target": func() *UIError { return a.ClearTarget().Error },
		"catalogue":    func() *UIError { return a.StartCatalogBuild(true).Error },
		"build":        func() *UIError { return a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}).Error },
		"export":       func() *UIError { return a.StartExport(ExportOptions{}).Error },
	} {
		t.Run(name, func(t *testing.T) {
			if e := run(); e == nil || e.Code != ErrCodeBusy {
				t.Fatalf("conflicting binding was not guarded: %+v", e)
			}
		})
	}
	if !slices.Equal(a.SelectionKeys().Keys, spec.Packages) || a.LifecycleStatus().SelectionRevision != before.SelectionRevision {
		t.Fatal("a guarded edit changed the draft")
	}
	close(cli.release)
	waitFor(t, "build finish", func() bool { return a.BuildStatus().Finished })
	if a.LifecycleStatus().UnsavedSelection {
		t.Fatal("complete current draft still prompts as unsaved")
	}
	a.AddPackages([]string{"curl"})
	if !a.LifecycleStatus().UnsavedSelection || a.BuildStatus().ItemCount != state.ItemCount || a.BuildStatus().TargetGeneration != state.TargetGeneration {
		t.Fatal("editing the next draft changed the completed run")
	}
}

type uxResolverFunc func(context.Context, TargetSelection) (catalog.Target, []catalog.SourceProblem, error)

func (f uxResolverFunc) Resolve(ctx context.Context, sel TargetSelection) (catalog.Target, []catalog.SourceProblem, error) {
	return f(ctx, sel)
}

func TestUXTargetResolverCannotCommitOverAStartedBuild(t *testing.T) {
	a, cli := uxJobApp(t)
	entered, release := make(chan struct{}), make(chan struct{})
	a.resolver = uxResolverFunc(func(ctx context.Context, sel TargetSelection) (catalog.Target, []catalog.SourceProblem, error) {
		close(entered)
		<-release
		return defaultResolver{}.Resolve(ctx, sel)
	})
	result := make(chan TargetResult, 1)
	go func() {
		result <- a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/server", Arch: "amd64"})
	}()
	<-entered
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		close(release)
		t.Fatal(r.Error)
	}
	<-cli.entered
	close(release)
	if r := <-result; r.Error == nil || r.Error.Code != ErrCodeBusy {
		t.Fatalf("late target committed: %+v", r)
	}
	if a.CurrentTarget().Target.ID != "ubuntu:24.04/desktop" {
		t.Fatal("build identity changed under the run")
	}
}

func TestUXOnlyCompleteUncancelledBuildFulfilsDraft(t *testing.T) {
	for _, outcome := range []string{"complete", "unsigned", "incomplete", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			a, cli := uxJobApp(t)
			switch outcome {
			case "complete":
				cli.result.Signed = true
			case "incomplete":
				cli.result.ExitClass = buildjob.ExitIncomplete
			case "failed":
				cli.err = errors.New("build failed")
			}
			if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
				t.Fatal(r.Error)
			}
			<-cli.entered
			if outcome == "cancelled" {
				a.CancelBuild()
				<-cli.cancelled
			}
			close(cli.release)
			waitFor(t, "terminal build", func() bool { return a.BuildStatus().Finished })
			wantUnsaved := outcome != "complete" && outcome != "unsigned"
			if a.LifecycleStatus().UnsavedSelection != wantUnsaved {
				t.Fatalf("%s checkpoint: %+v", outcome, a.LifecycleStatus())
			}
		})
	}
}

func TestUXChangedDigestInvalidatesCompletedDraftWithoutChangingKeys(t *testing.T) {
	a, _ := newApp(t)
	redTrayWithCredentials(t, a)
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatal(r.Error)
	}
	waitFor(t, "bundle", func() bool { return a.BuildStatus().Finished })
	before, keys := a.LifecycleStatus(), a.SelectionKeys()
	a.AddURLs([]URLInput{{URL: redCredURL, SHA256: strings.Repeat("5", 64)}})
	if !a.LifecycleStatus().UnsavedSelection || a.LifecycleStatus().SelectionRevision <= before.SelectionRevision || a.SelectionKeys().Revision != keys.Revision {
		t.Fatal("changed digest failed to invalidate draft independently of the key cache")
	}
}

func TestUXTargetChangeRevalidatesRetainedAPTEntries(t *testing.T) {
	a, _ := uxJobApp(t)
	if a.Selection().UnknownCount != 0 {
		t.Fatal("fixture entries were not initially known")
	}
	cat := a.cat.(*catfake.Fake)
	cat.SetReady(false)
	if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/server", Arch: "amd64"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if s := a.Selection(); s.Total != 2 || s.UnknownCount != 2 || s.DownloadSizeBytes != 0 {
		t.Fatalf("old target metadata survived: %+v", s)
	}
	if r := a.StartCatalogBuild(false); !r.OK {
		t.Fatal(r.Error)
	}
	waitFor(t, "new target catalogue", func() bool { return a.CatalogStatus().Ready })
	if a.Selection().UnknownCount != 0 || a.Selection().Total != 2 {
		t.Fatal("retained entries were not revalidated after preparation")
	}
}

func TestUXRefreshedSnapshotAtSamePathChangesDraftGeneration(t *testing.T) {
	a, _ := uxJobApp(t)
	path := filepath.Join(t.TempDir(), "snapshot.tar")
	a.resolver = warmResolver{target: warmTarget()}
	sel := TargetSelection{Kind: TargetKindSnapshot, SnapshotPath: path}
	if err := os.WriteFile(path, []byte("first captured state"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := a.SelectTarget(sel)
	unchanged := a.SelectTarget(sel)
	if first.Error != nil || unchanged.Error != nil || first.Target.Generation != unchanged.Target.Generation {
		t.Fatal("unchanged snapshot did not retain generation")
	}
	if err := os.WriteFile(path, []byte("second captured state"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := a.SelectTarget(sel)
	if changed.Error != nil || changed.Target.Generation <= first.Target.Generation {
		t.Fatalf("refreshed snapshot retained a stale checkpoint: %+v", changed)
	}
}
