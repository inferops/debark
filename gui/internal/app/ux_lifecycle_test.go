package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/export"
)

func TestUXCloseReturnsPromptlyAndKeepWorkingDoesNotCancel(t *testing.T) {
	a, cli := uxJobApp(t)
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatal(r.Error)
	}
	<-cli.entered
	asked, answer := make(chan bool, 2), make(chan bool)
	a.confirmClose = func(_ context.Context, active bool) (bool, error) { asked <- active; return <-answer, nil }
	var quits atomic.Int32
	a.quit = func(context.Context) { quits.Add(1) }
	begin := time.Now()
	if !a.BeforeClose(context.Background()) || time.Since(begin) > 100*time.Millisecond {
		t.Fatal("native hook blocked or allowed premature closure")
	}
	if !<-asked {
		t.Fatal("running build was not identified as active work")
	}
	if !a.BeforeClose(context.Background()) {
		t.Fatal("second native close was allowed during confirmation")
	}
	select {
	case <-asked:
		t.Fatal("duplicate confirmation")
	default:
	}
	answer <- false
	waitFor(t, "Keep working", func() bool { a.mu.RLock(); defer a.mu.RUnlock(); return !a.closePending })
	select {
	case <-cli.cancelled:
		t.Fatal("Keep working cancelled the operation")
	default:
	}
	if quits.Load() != 0 || !a.LifecycleStatus().BuildRunning || a.LifecycleStatus().Stopping {
		t.Fatal("Keep working changed job state")
	}
}

func TestUXStopAndCloseWaitsForCleanupAndDoesNotPromptAgain(t *testing.T) {
	a, cli := uxJobApp(t)
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatal(r.Error)
	}
	<-cli.entered
	var prompts atomic.Int32
	a.confirmClose = func(context.Context, bool) (bool, error) { prompts.Add(1); return true, nil }
	quit := make(chan struct{}, 1)
	a.quit = func(context.Context) { quit <- struct{}{} }
	a.BeforeClose(context.Background())
	<-cli.cancelled
	if !a.LifecycleStatus().Stopping {
		t.Fatal("no Stopping state during cancellation cleanup")
	}
	if r := a.AddPackages([]string{"curl"}); r.Error == nil {
		t.Fatal("selection mutation during shutdown")
	}
	select {
	case <-quit:
		t.Fatal("quit before the cancelled worker cleaned up")
	default:
	}
	close(cli.release)
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("did not quit after cleanup")
	}
	if a.BeforeClose(context.Background()) {
		t.Fatal("Wails Quit re-entry was vetoed after cleanup")
	}
	if prompts.Load() != 1 {
		t.Fatal("Stop and close caused another unsaved-selection prompt")
	}
	if !a.BuildStatus().Cancelled || !a.LifecycleStatus().UnsavedSelection {
		t.Fatal("late successful result fulfilled a cancelled draft")
	}
}

func TestUXCloseTimeoutKeepsAppOpenAndAllowsRetry(t *testing.T) {
	a, cli := uxJobApp(t)
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatal(r.Error)
	}
	<-cli.entered
	a.closeTimeout = 30 * time.Millisecond
	a.confirmClose = func(context.Context, bool) (bool, error) { return true, nil }
	var quits atomic.Int32
	a.quit = func(context.Context) { quits.Add(1) }
	a.BeforeClose(context.Background())
	<-cli.cancelled
	waitFor(t, "timeout", func() bool { return a.LifecycleStatus().Error != nil })
	state := a.LifecycleStatus()
	if state.Stopping || !state.BuildRunning || state.Error.Hint == "" || !state.Error.Retryable || quits.Load() != 0 {
		t.Fatalf("timeout forced closure or hid recovery: %+v", state)
	}
	close(cli.release)
	waitFor(t, "late cancellation", func() bool { return a.BuildStatus().Finished })
	if quits.Load() != 0 {
		t.Fatal("late completion silently closed after timeout")
	}
	if r := a.AddPackages([]string{"curl"}); r.Error != nil {
		t.Fatalf("app was unusable after timeout: %+v", r.Error)
	}
	a.BeforeClose(context.Background())
	waitFor(t, "explicit close retry", func() bool { return quits.Load() == 1 })
}

func TestUXCompletedDraftClosesWithoutUnsavedPrompt(t *testing.T) {
	a, cli := uxJobApp(t)
	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatal(r.Error)
	}
	<-cli.entered
	close(cli.release)
	waitFor(t, "complete draft", func() bool { return a.BuildStatus().Finished })
	var prompts, quits atomic.Int32
	a.confirmClose = func(context.Context, bool) (bool, error) { prompts.Add(1); return false, nil }
	a.quit = func(context.Context) { quits.Add(1) }
	a.BeforeClose(context.Background())
	waitFor(t, "close fulfilled draft", func() bool { return quits.Load() == 1 })
	if prompts.Load() != 0 {
		t.Fatal("completed current draft warned of unsaved work")
	}
}

func TestUXCopyBlocksBuildAndCancellationKeepsIncompleteMarker(t *testing.T) {
	a, _ := uxJobApp(t)
	root := t.TempDir()
	bundle, drive := filepath.Join(root, "bundle"), filepath.Join(root, "drive")
	for _, path := range []string{bundle, drive} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(`{"fixture":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	a.emitter = func(name string, payload any) {
		if name == EventExportProgress && payload.(ExportProgress).Phase == ExportPhaseCopying {
			once.Do(func() { close(entered); <-release })
		}
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if r := a.StartExport(ExportOptions{BundlePath: bundle, Destination: drive}); !r.OK {
		t.Fatal(r.Error)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("copy did not enter file transfer")
	}
	if r := a.StartBuild(BuildOptions{OutputDir: root, NoSign: true}); r.Error == nil || r.Error.Code != ErrCodeBusy {
		t.Fatalf("build started over an active copy: %+v", r)
	}
	if r := a.ClearSelection(); r.Error == nil || r.Error.Code != ErrCodeBusy {
		t.Fatalf("selection changed during copy: %+v", r)
	}
	a.CancelExport()
	close(release)
	waitFor(t, "cancelled copy", func() bool { return a.ExportStatus().Finished })
	status := a.ExportStatus()
	if !status.Cancelled || status.Verified {
		t.Fatalf("cancelled copy looked verified: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(drive, "bundle", export.MarkerName)); err != nil {
		t.Fatalf("cancelled destination lacks incomplete marker: %v", err)
	}
}

func TestUXHeadlessNativeActionsReportUnavailable(t *testing.T) {
	a, _ := newApp(t)
	if r := a.ChooseSigningKey(); r.Error == nil || r.Error.Code != ErrCodeNoRuntime || r.Path != "" {
		t.Fatalf("headless key chooser invented a reference: %+v", r)
	}
	if r := a.RequestClose(); r.Error == nil || r.Error.Code != ErrCodeNoRuntime {
		t.Fatalf("headless Quit invented native success: %+v", r)
	}
}

type uxCloseCatalog struct {
	catalog.Catalog
	closes atomic.Int32
}

func (c *uxCloseCatalog) Close() error { c.closes.Add(1); return c.Catalog.Close() }

func TestUXShutdownCancelsEveryWorkerBeforeClosingCatalogueOnce(t *testing.T) {
	a, _ := newApp(t)
	cat := &uxCloseCatalog{Catalog: a.cat}
	a.cat = cat
	jobs := []*job{&a.readinessJob, &a.catalogJob, &a.buildJob, &a.exportJob, &a.verifyJob}
	contexts := make([]context.Context, len(jobs))
	for i, j := range jobs {
		contexts[i], _ = j.start(a.baseCtx)
	}
	begin := time.Now()
	a.Shutdown(context.Background())
	if time.Since(begin) > 100*time.Millisecond {
		t.Fatal("shutdown blocked on worker cleanup")
	}
	for _, ctx := range contexts {
		if ctx.Err() != context.Canceled {
			t.Fatal("shutdown left a worker uncancelled")
		}
	}
	if cat.closes.Load() != 0 {
		t.Fatal("catalogue closed while a worker still used it")
	}
	a.mu.Lock()
	for _, j := range jobs {
		j.finish(j.gen)
	}
	a.mu.Unlock()
	waitFor(t, "catalogue cleanup", func() bool { return cat.closes.Load() == 1 })
	a.Shutdown(context.Background())
	if cat.closes.Load() != 1 {
		t.Fatal("catalogue cleanup ran twice")
	}
}
