// Command gui-e2e is the online-builder half of the GUI's end-to-end proof.
//
// It drives the application's OWN Go layer — internal/app on top of
// internal/cliadapter — against a real debark binary, in the same order and
// through the same bound methods the five screens call. Nothing here shells
// out to debark itself: every invocation goes through cliadapter, which is
// the point. A harness that ran `debark build` directly would prove that
// debark works, which was never in doubt; what has never been exercised is
// the seam between this application and it.
//
// The offline half — verify and install with --network none — is
// hack/demo-gui-e2e.sh, because it must happen in a different container with
// no network and nothing of this application present. That split mirrors
// ../hack/demo-airgap.sh, which is the prior art for the container
// side and was read rather than re-invented.
//
// Usage:
//
//	gui-e2e -debark /w/debark -work /w -base debian:12/minimal -packages jq
//
// It writes a JSON report to -report (default <work>/gui-e2e-report.json) so
// the shell script and CI can assert on the outcome without parsing prose,
// and a running commentary to stderr so a person watching can see where it is.
//
// Every step is a bound method. The step names in the report are the method
// names, so a failure names the seam that broke.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/inferops/debark/gui/internal/app"
	"github.com/inferops/debark/gui/internal/cliadapter"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\ngui-e2e: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// step is one bound method call and what it produced. Detail is deliberately
// a rendered sentence rather than a struct: the report is read by a person
// first and by a script second.
type step struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// report is the whole run, written to -report as JSON.
type report struct {
	OK            bool     `json:"ok"`
	StartedAt     string   `json:"started_at"`
	DurationMS    int64    `json:"duration_ms"`
	Platform      string   `json:"platform"`
	GUIVersion    string   `json:"gui_version"`
	DebarkPath    string   `json:"debark_path"`
	DebarkVersion string   `json:"debark_version"`
	BaseID        string   `json:"base_id"`
	Arch          string   `json:"arch"`
	Packages      []string `json:"packages"`

	// URLs are the vendor .deb downloads, if any. They matter out of
	// proportion to their number: they are the only input on the local apt
	// backend that produces fetch.file and progress events at all, because
	// core/fetch handles them and apt handles everything else. See
	// docs/dev/cli-surface.md §5.3.
	URLs []app.URLInput `json:"urls,omitempty"`

	// Command is what PreviewCommand showed the operator, verbatim. Rule 8
	// says the UI shows the command it is about to run; this records that it
	// is the same command that ran.
	Command []string `json:"command"`

	KeyPath   string `json:"key_path,omitempty"`
	PublicKey string `json:"public_key_path,omitempty"`

	Readiness *app.ReadinessReport `json:"readiness,omitempty"`
	Build     *app.BuildSummary    `json:"build,omitempty"`
	Verify    *app.VerifyStatus    `json:"verify,omitempty"`
	Export    *app.ExportStatus    `json:"export,omitempty"`

	// MediaPath is where the export landed: the folder the offline stage
	// treats as the removable drive.
	MediaPath string `json:"media_path,omitempty"`

	// Events counts each bound event name that fired, and EventTypes counts
	// each debark NDJSON event type that reached the details drawer. Both
	// are assertions in disguise: an empty EventTypes means the --json-events
	// stream never arrived, which every fake hides.
	Events     map[string]int `json:"events"`
	EventTypes map[string]int `json:"event_types"`

	Steps []step   `json:"steps"`
	Fail  string   `json:"failure,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// ---------------------------------------------------------------------------
// Event collection
// ---------------------------------------------------------------------------

// collector is Deps.Emit. The real application hands events to Wails; a
// headless run counts them and keeps the terminal payloads, which is how this
// harness waits for a build without polling a status method it does not trust.
type collector struct {
	mu     sync.Mutex
	counts map[string]int

	buildDone  chan app.BuildFinished
	verifyDone chan app.VerifyFinished
	exportDone chan app.ExportFinished
	readyDone  chan app.ReadinessFinished
}

func newCollector() *collector {
	return &collector{
		counts: map[string]int{},
		// Buffered: a finished event must never block the goroutine that
		// emitted it, and this harness sometimes starts waiting a moment
		// after the operation it is waiting for.
		buildDone:  make(chan app.BuildFinished, 4),
		verifyDone: make(chan app.VerifyFinished, 4),
		exportDone: make(chan app.ExportFinished, 4),
		readyDone:  make(chan app.ReadinessFinished, 8),
	}
}

func (c *collector) emit(name string, payload any) {
	c.mu.Lock()
	c.counts[name]++
	c.mu.Unlock()

	switch name {
	case app.EventBuildFinished:
		if v, ok := payload.(app.BuildFinished); ok {
			select {
			case c.buildDone <- v:
			default:
			}
		}
	case app.EventVerifyFinished:
		if v, ok := payload.(app.VerifyFinished); ok {
			select {
			case c.verifyDone <- v:
			default:
			}
		}
	case app.EventExportFinished:
		if v, ok := payload.(app.ExportFinished); ok {
			select {
			case c.exportDone <- v:
			default:
			}
		}
	case app.EventReadinessFinished:
		if v, ok := payload.(app.ReadinessFinished); ok {
			select {
			case c.readyDone <- v:
			default:
			}
		}
	}
}

func (c *collector) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------------

type harness struct {
	a   *app.App
	c   *collector
	rep *report
	now time.Time
}

func run() error {
	var (
		binary   = flag.String("debark", "", "path to the debark binary (required)")
		work     = flag.String("work", "", "working directory for the key, the bundle and the media (required)")
		baseID   = flag.String("base", "debian:12/minimal", "stock base id to build against")
		arch     = flag.String("arch", "", "dpkg architecture; empty means whatever debark defaults to")
		pkgList  = flag.String("packages", "jq", "comma-separated apt package names to put in the bundle")
		urlList  = flag.String("urls", "", "comma-separated vendor .deb downloads, each URL or URL=SHA256")
		repPath  = flag.String("report", "", "where to write the JSON report (default <work>/gui-e2e-report.json)")
		timeout  = flag.Duration("timeout", 20*time.Minute, "overall deadline")
		mediaDir = flag.String("media", "", "directory to export the bundle into (default <work>/media)")
	)
	flag.Parse()

	if *binary == "" || *work == "" {
		flag.Usage()
		return errors.New("-debark and -work are both required")
	}
	packages := splitList(*pkgList)
	urls, uerr := parseURLs(*urlList)
	if uerr != nil {
		return uerr
	}
	if len(packages) == 0 && len(urls) == 0 {
		return errors.New("neither -packages nor -urls listed anything to build")
	}
	if *repPath == "" {
		*repPath = filepath.Join(*work, "gui-e2e-report.json")
	}
	if *mediaDir == "" {
		*mediaDir = filepath.Join(*work, "media")
	}

	keyPath := filepath.Join(*work, "operator.key")
	rep := &report{
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		Platform:   platform(),
		BaseID:     *baseID,
		Arch:       *arch,
		Packages:   packages,
		URLs:       urls,
		KeyPath:    keyPath,
		MediaPath:  *mediaDir,
		Events:     map[string]int{},
		EventTypes: map[string]int{},
	}

	adapter, err := cliadapter.New(cliadapter.Options{BinaryPath: *binary})
	if err != nil {
		return fmt.Errorf("build the CLI adapter: %w", err)
	}

	c := newCollector()
	a := app.New(app.Deps{
		CLI: adapter,
		// Catalog is deliberately nil. The catalogue is the browsing index
		// behind the picker; it decides nothing about what a build contains,
		// and building one needs the target's apt sources, which
		// docs/dev/cli-surface.md records the CLI does not hand over for a
		// stock base. This harness proves the build and bundle seam, and adds
		// its packages by name exactly as an operator typing into the tray
		// would. Measuring the catalogue against a real index is a separate
		// piece of work with its own budget.
		Emit:           c.emit,
		Version:        "gui-e2e",
		SigningKeyPath: keyPath,
	})
	// A plain context, so App.Startup records "headless" and never installs
	// the Wails emitter — which would call log.Fatalf here.
	a.Startup(context.Background())
	defer a.Shutdown(context.Background())

	h := &harness{a: a, c: c, rep: rep, now: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	runErr := h.all(ctx, packages, urls, *mediaDir)

	rep.DurationMS = time.Since(h.now).Milliseconds()
	rep.Events = c.snapshot()
	rep.OK = runErr == nil
	if runErr != nil {
		rep.Fail = runErr.Error()
	}
	if werr := writeReport(*repPath, rep); werr != nil {
		// A lost report is worth reporting, but never worth turning a passing
		// run into a failing one or a failing one into a different failure.
		fmt.Fprintf(os.Stderr, "gui-e2e: could not write the report to %s: %v\n", *repPath, werr)
	} else {
		fmt.Fprintf(os.Stderr, "\nreport: %s\n", *repPath)
	}
	return runErr
}

// all is the operator's path through the application, screen by screen.
func (h *harness) all(ctx context.Context, packages []string, urls []app.URLInput, mediaDir string) error {
	if err := h.info(); err != nil {
		return err
	}
	if err := h.readiness(ctx); err != nil {
		return err
	}
	if err := h.target(); err != nil {
		return err
	}
	if err := h.tray(packages, urls); err != nil {
		return err
	}
	if err := h.build(ctx); err != nil {
		return err
	}
	if err := h.verify(ctx); err != nil {
		return err
	}
	return h.export(ctx, mediaDir)
}

// ---------------------------------------------------------------------------
// Screen 0 — the about box.
// ---------------------------------------------------------------------------

func (h *harness) info() error {
	return h.step("AppInfo", func() (string, error) {
		info := h.a.AppInfo()
		if info.Error != nil {
			return "", uiErr("AppInfo", info.Error)
		}
		if info.DebarkPath == "" || info.DebarkVersion == "" {
			return "", fmt.Errorf("the probe returned no path or version: %+v", info)
		}
		h.rep.DebarkPath = info.DebarkPath
		h.rep.DebarkVersion = info.DebarkVersion
		h.rep.GUIVersion = info.Version
		return fmt.Sprintf("debark %s at %s", info.DebarkVersion, info.DebarkPath), nil
	})
}

// ---------------------------------------------------------------------------
// Screen 1 — readiness, including the signing key the bundle is signed with.
// ---------------------------------------------------------------------------

func (h *harness) readiness(ctx context.Context) error {
	err := h.step("StartReadinessCheck", func() (string, error) {
		if r := h.a.StartReadinessCheck(); !r.OK {
			return "", uiErr("StartReadinessCheck", r.Error)
		}
		fin, werr := waitFor(ctx, h.c.readyDone, "readiness:finished")
		if werr != nil {
			return "", werr
		}
		rep := fin.Report
		h.rep.Readiness = &rep
		var problems []string
		for _, c := range rep.Checks {
			if c.Status == app.ReadinessProblem {
				problems = append(problems, fmt.Sprintf("%s(%s)", c.ID, c.Severity))
			}
			// The invariant the whole screen rests on. A problem with no
			// remedy is the wall this application exists to remove, and it is
			// worth failing the run over.
			if c.Status == app.ReadinessProblem && c.Remedy == "" {
				return "", fmt.Errorf("readiness row %q is a problem with no remedy", c.ID)
			}
		}
		detail := fmt.Sprintf("%d rows, can_build=%v", len(rep.Checks), rep.CanBuild)
		if len(problems) > 0 {
			detail += ", problems: " + strings.Join(problems, " ")
		}
		if !rep.CanBuild {
			return detail, errors.New("readiness says this machine cannot build")
		}
		return detail, nil
	})
	if err != nil {
		return err
	}

	// The signing key comes from the application's own remedy button, not
	// from a shell running keygen beside it. That is the whole point: the key
	// the bundle is signed with must be one the GUI made.
	return h.step("RunReadinessAction(signing-key)", func() (string, error) {
		row, found := h.check("signing-key")
		if !found {
			return "", errors.New("there is no signing-key row on the readiness screen")
		}
		if row.Status == app.ReadinessOK {
			h.note("a signing key already existed at " + h.rep.KeyPath + "; the keygen remedy was not exercised")
			return "already present: " + row.Summary, nil
		}
		if row.Action == nil {
			return "", errors.New("the signing-key row offers no action")
		}
		if !row.Action.Runnable {
			return "", fmt.Errorf("the signing-key action is not runnable: %s", row.Action.Display)
		}
		if r := h.a.RunReadinessAction("signing-key"); !r.OK {
			return "", uiErr("RunReadinessAction", r.Error)
		}
		fin, werr := waitFor(ctx, h.c.readyDone, "readiness:finished (action)")
		if werr != nil {
			return "", werr
		}
		if fin.Error != nil {
			return "", uiErr("the keygen remedy", fin.Error)
		}
		after, ok := h.check("signing-key")
		if !ok || after.Status != app.ReadinessOK {
			return "", fmt.Errorf("the signing-key row is still %q after running its own remedy", statusOf(after, ok))
		}
		pub := publicKeyBeside(h.rep.KeyPath)
		if _, serr := os.Stat(h.rep.KeyPath); serr != nil {
			return "", fmt.Errorf("the remedy reported success but %s is not there: %w", h.rep.KeyPath, serr)
		}
		if _, serr := os.Stat(pub); serr != nil {
			return "", fmt.Errorf("no public key beside the private one at %s: %w", pub, serr)
		}
		h.rep.PublicKey = pub
		return fmt.Sprintf("%s ran, key at %s", row.Action.Display, h.rep.KeyPath), nil
	})
}

func (h *harness) check(id string) (app.ReadinessCheck, bool) {
	rep := h.a.Readiness()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return app.ReadinessCheck{}, false
}

// ---------------------------------------------------------------------------
// Screen 2 — the target.
// ---------------------------------------------------------------------------

func (h *harness) target() error {
	var arch string
	err := h.step("SupportedArchitectures", func() (string, error) {
		res := h.a.SupportedArchitectures()
		if res.Error != nil {
			return "", uiErr("SupportedArchitectures", res.Error)
		}
		arch = h.rep.Arch
		if arch == "" {
			arch = res.Default
		}
		if arch == "" {
			return "", errors.New("no default architecture and none given")
		}
		h.rep.Arch = arch
		return fmt.Sprintf("%v, default %s", res.Arches, res.Default), nil
	})
	if err != nil {
		return err
	}

	err = h.step("ListBases", func() (string, error) {
		res := h.a.ListBases(arch)
		if res.Error != nil {
			return "", uiErr("ListBases", res.Error)
		}
		if len(res.Bases) == 0 {
			return "", errors.New("debark offered no bases at all")
		}
		// The base id must come from the listing, never from the flag alone:
		// picking a row the engine offered is what an operator does, and it
		// catches an id this binary does not have.
		for _, b := range res.Bases {
			if b.ID == h.rep.BaseID {
				return fmt.Sprintf("%d bases for %s, chose %s", len(res.Bases), res.Arch, b.ID), nil
			}
		}
		ids := make([]string, 0, len(res.Bases))
		for _, b := range res.Bases {
			ids = append(ids, b.ID)
		}
		return "", fmt.Errorf("this debark does not offer %q; it offers %s", h.rep.BaseID, strings.Join(ids, ", "))
	})
	if err != nil {
		return err
	}

	return h.step("SelectTarget", func() (string, error) {
		res := h.a.SelectTarget(app.TargetSelection{
			Kind:   app.TargetKindBase,
			BaseID: h.rep.BaseID,
			Arch:   arch,
		})
		if res.Error != nil {
			return "", uiErr("SelectTarget", res.Error)
		}
		if !res.Target.Selected {
			return "", errors.New("SelectTarget returned no selection")
		}
		if !res.Target.Assumed || res.Target.Caveat == "" {
			return "", errors.New("a stock base must be marked assumed and carry its caveat")
		}
		return res.Target.Label + " — " + res.Target.Caveat, nil
	})
}

// ---------------------------------------------------------------------------
// Screen 3 — the tray.
// ---------------------------------------------------------------------------

func (h *harness) tray(packages []string, urls []app.URLInput) error {
	if len(packages) > 0 {
		err := h.step("AddPackages", func() (string, error) {
			s := h.a.AddPackages(packages)
			if s.Error != nil {
				return "", uiErr("AddPackages", s.Error)
			}
			if len(s.Rejected) > 0 {
				return "", fmt.Errorf("the tray rejected %d of them: %+v", len(s.Rejected), s.Rejected)
			}
			if s.Total != len(packages) {
				return "", fmt.Errorf("tray holds %d entries, expected %d", s.Total, len(packages))
			}
			return fmt.Sprintf("%d entries: %s", s.Total, strings.Join(packages, " ")), nil
		})
		if err != nil {
			return err
		}
	}
	if len(urls) == 0 {
		return nil
	}
	return h.step("AddURLs", func() (string, error) {
		s := h.a.AddURLs(urls)
		if s.Error != nil {
			return "", uiErr("AddURLs", s.Error)
		}
		if len(s.Rejected) > 0 {
			return "", fmt.Errorf("the tray rejected %d of them: %+v", len(s.Rejected), s.Rejected)
		}
		if s.URLCount != len(urls) {
			return "", fmt.Errorf("tray holds %d URLs, expected %d", s.URLCount, len(urls))
		}
		// The digest is the whole reason the tray collects one: it is what
		// makes the input operator-attested rather than merely downloaded. A
		// UI that asks for one and drops it is worse than one that never
		// asked, so check it survived onto the entry.
		page := h.a.SelectionPage(0, app.SelectionPageMax)
		for _, in := range urls {
			if in.SHA256 == "" {
				continue
			}
			found := false
			for _, e := range page.Entries {
				if e.Key == in.URL && strings.EqualFold(e.SHA256, in.SHA256) {
					found = true
				}
			}
			if !found {
				return "", fmt.Errorf("the digest for %s did not survive into the tray", in.URL)
			}
		}
		return fmt.Sprintf("%d vendor downloads", s.URLCount), nil
	})
}

// parseURLs reads the -urls flag: URL, or URL=SHA256 when the operator has an
// expected digest. The "=" split is from the right, because a URL may contain
// one and a SHA-256 may not.
func parseURLs(s string) ([]app.URLInput, error) {
	var out []app.URLInput
	for _, raw := range splitList(s) {
		in := app.URLInput{URL: raw}
		if i := strings.LastIndex(raw, "="); i > 0 {
			if d := raw[i+1:]; len(d) == 64 {
				in.URL, in.SHA256 = raw[:i], d
			}
		}
		if !strings.HasPrefix(in.URL, "https://") && !strings.HasPrefix(in.URL, "http://") {
			return nil, fmt.Errorf("-urls: %q is not an http(s) URL", in.URL)
		}
		out = append(out, in)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Screen 4 — the build.
// ---------------------------------------------------------------------------

func (h *harness) build(ctx context.Context) error {
	opts := app.BuildOptions{
		OutputDir:  filepath.Dir(h.rep.KeyPath),
		OutputName: "bundle",
		SignerRef:  h.rep.KeyPath,
	}

	err := h.step("PreviewCommand", func() (string, error) {
		pv := h.a.PreviewCommand(opts)
		if pv.Error != nil {
			return "", uiErr("PreviewCommand", pv.Error)
		}
		if len(pv.Argv) == 0 || pv.Display == "" {
			return "", errors.New("the preview produced no command")
		}
		h.rep.Command = pv.Argv
		return pv.Display, nil
	})
	if err != nil {
		return err
	}

	return h.step("StartBuild", func() (string, error) {
		if r := h.a.StartBuild(opts); !r.OK {
			return "", uiErr("StartBuild", r.Error)
		}
		fin, werr := waitFor(ctx, h.c.buildDone, "build:finished")
		if werr != nil {
			return "", werr
		}
		st := h.a.BuildStatus()
		if st.Summary != nil {
			s := *st.Summary
			h.rep.Build = &s
		}
		h.countEventTypes()

		if fin.Error != nil {
			// The lists are the only place a failing build names anything:
			// stderr is empty for exits 3, 5 and 6 (cli-surface.md C1), so
			// without them the run would report "a download failed" and stop.
			return "", fmt.Errorf("%w%s", uiErr("the build", fin.Error), buildDetail(fin.Summary))
		}
		if !fin.OK || fin.Summary == nil {
			return "", fmt.Errorf("the build did not succeed: %+v", fin)
		}
		sum := fin.Summary
		switch {
		case sum.ExitClass != "success":
			return "", fmt.Errorf("the build finished %q, not success: unresolved=%v fetch_failed=%v",
				sum.ExitClass, sum.Unresolved, sum.FetchFailed)
		case !sum.Signed:
			return "", errors.New("the bundle is unsigned, though a signing key was given")
		case sum.BundlePath == "":
			return "", errors.New("the build reported no bundle path")
		case sum.Stats.PackageCount == 0:
			return "", errors.New("the bundle contains no packages")
		}
		// The event stream is the part every fake gets right and every real
		// run can get wrong. An empty drawer means --json-events never
		// arrived, and the whole build screen would have shown a dead bar.
		if len(h.rep.EventTypes) == 0 {
			return "", errors.New("no debark events reached the details drawer")
		}
		if st.Command == nil {
			return "", errors.New("the build did not record the command it ran")
		}
		if !sameArgv(st.Command, h.rep.Command) {
			return "", fmt.Errorf("the command shown differs from the command run:\n  shown: %v\n  ran:   %v",
				h.rep.Command, st.Command)
		}
		return fmt.Sprintf("%s, %d packages, %d bytes, signed, %d event types",
			sum.BundlePath, sum.Stats.PackageCount, sum.Stats.Bytes, len(h.rep.EventTypes)), nil
	})
}

// buildDetail renders what a failing build named, from the result rather than
// from the error. See cliadapter's Build: the adapter deliberately leaves the
// generic class description in place and hands the lists to the caller.
func buildDetail(sum *app.BuildSummary) string {
	if sum == nil {
		return ""
	}
	var b strings.Builder
	add := func(label string, items []string) {
		for _, it := range items {
			fmt.Fprintf(&b, "\n    %s: %s", label, it)
		}
	}
	add("fetch failed", sum.FetchFailed)
	add("unresolved", sum.Unresolved)
	add("warning", sum.Warnings)
	if sum.Truncated {
		b.WriteString("\n    (lists truncated; the whole record is in the bundle's evidence.json)")
	}
	return b.String()
}

// countEventTypes reads the details drawer the way the build screen does, and
// records which debark event types actually arrived.
func (h *harness) countEventTypes() {
	since := 0
	for {
		page := h.a.BuildLog(since, 500)
		for _, e := range page.Events {
			h.rep.EventTypes[e.Type]++
		}
		if page.NextSeq == since || len(page.Events) == 0 {
			return
		}
		since = page.NextSeq
	}
}

// ---------------------------------------------------------------------------
// Screen 5 — verify, then export to the "drive".
// ---------------------------------------------------------------------------

func (h *harness) verify(ctx context.Context) error {
	return h.step("StartVerify", func() (string, error) {
		if r := h.a.StartVerify(""); !r.OK {
			return "", uiErr("StartVerify", r.Error)
		}
		fin, werr := waitFor(ctx, h.c.verifyDone, "verify:finished")
		if werr != nil {
			return "", werr
		}
		st := fin.Status
		h.rep.Verify = &st
		if fin.Error != nil {
			return "", uiErr("the verification", fin.Error)
		}
		if !st.OK {
			return "", fmt.Errorf("the bundle this application just built does not verify: %+v", st.Findings)
		}
		if !st.Signed {
			return "", errors.New("verify reports the bundle is unsigned")
		}
		return fmt.Sprintf("%s: ok, signed", st.BundlePath), nil
	})
}

func (h *harness) export(ctx context.Context, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("make the media directory: %w", err)
	}

	err := h.step("PlanExport", func() (string, error) {
		res := h.a.PlanExport(app.ExportOptions{Destination: dest})
		if res.Error != nil {
			return "", uiErr("PlanExport", res.Error)
		}
		if res.Plan.TotalFiles == 0 {
			return "", errors.New("the export plan copies nothing")
		}
		return fmt.Sprintf("%d files, %d bytes into %s", res.Plan.TotalFiles, res.Plan.TotalBytes, res.Plan.DestDir), nil
	})
	if err != nil {
		return err
	}

	return h.step("StartExport", func() (string, error) {
		if r := h.a.StartExport(app.ExportOptions{Destination: dest}); !r.OK {
			return "", uiErr("StartExport", r.Error)
		}
		fin, werr := waitFor(ctx, h.c.exportDone, "export:finished")
		if werr != nil {
			return "", werr
		}
		st := fin.Status
		h.rep.Export = &st
		if fin.Error != nil {
			return "", uiErr("the export", fin.Error)
		}
		if !st.Verified {
			return "", fmt.Errorf("the copy was not verified: %s", st.Summary)
		}
		if st.DestinationPath == "" {
			return "", errors.New("the export reported no destination path")
		}
		h.rep.MediaPath = st.DestinationPath
		return fmt.Sprintf("%s, verified", st.DestinationPath), nil
	})
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

func (h *harness) step(name string, fn func() (string, error)) error {
	fmt.Fprintf(os.Stderr, "==> %s\n", name)
	begin := time.Now()
	detail, err := fn()
	s := step{Name: name, OK: err == nil, Detail: detail, DurationMS: time.Since(begin).Milliseconds()}
	if err != nil {
		s.Detail = err.Error()
	}
	h.rep.Steps = append(h.rep.Steps, s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "    FAILED: %v\n", err)
		return fmt.Errorf("%s: %w", name, err)
	}
	if detail != "" {
		fmt.Fprintf(os.Stderr, "    %s\n", detail)
	}
	return nil
}

func (h *harness) note(s string) { h.rep.Notes = append(h.rep.Notes, s) }

// waitFor blocks for one terminal event. Every long-running bound method
// promises exactly one, including on failure and cancellation, so a timeout
// here is itself a finding.
func waitFor[T any](ctx context.Context, ch <-chan T, what string) (T, error) {
	var zero T
	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		return zero, fmt.Errorf("timed out waiting for %s: %w", what, ctx.Err())
	}
}

// uiErr renders a *app.UIError the way the shell would, so a failure in this
// harness reads like the banner an operator would have seen. It also enforces
// the definition-of-done item: a bound error with no hint is not actionable,
// and saying so here is cheaper than noticing it on screen.
func uiErr(what string, e *app.UIError) error {
	if e == nil {
		return fmt.Errorf("%s failed with no error attached", what)
	}
	msg := fmt.Sprintf("%s: [%s] %s", what, e.Code, e.Message)
	if e.Hint != "" {
		msg += " — " + e.Hint
	} else {
		msg += " — (no hint: not actionable)"
	}
	if len(e.Command) > 0 {
		msg += "\n    command: " + strings.Join(e.Command, " ")
	}
	if e.Details != "" {
		msg += "\n    details: " + firstLines(e.Details, 12)
	}
	return errors.New(msg)
}

func statusOf(c app.ReadinessCheck, found bool) string {
	if !found {
		return "missing"
	}
	return string(c.Status)
}

// publicKeyBeside applies the CLI's own rule: the private path with .key
// replaced by .pub, or .pub appended when there was no .key suffix.
func publicKeyBeside(priv string) string {
	if strings.HasSuffix(priv, ".key") {
		return strings.TrimSuffix(priv, ".key") + ".pub"
	}
	return priv + ".pub"
}

func sameArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n    ... (%d more lines)", len(lines)-n)
}

// writeReport writes the report as JSON. encoding/json already emits map keys
// in sorted order, so two runs of the same shape diff cleanly.
func writeReport(path string, rep *report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// platform is what the report records about the machine the builder half ran
// on. It is GOOS/GOARCH of THIS binary, which on the container path is the
// container, not the desktop.
func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }
