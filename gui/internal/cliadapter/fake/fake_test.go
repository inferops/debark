package fake_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"

	"github.com/inferops/debark/gui/internal/cliadapter"
	"github.com/inferops/debark/gui/internal/cliadapter/fake"
)

// fullSpec exercises every field of BuildSpec at once, so the argv test below
// pins the whole translation rather than the two flags someone remembered.
func boolPtr(b bool) *bool { return &b }

func fullSpec() cliadapter.BuildSpec {
	return cliadapter.BuildSpec{
		BaseID:    "ubuntu:24.04/server",
		Arch:      "arm64",
		Packages:  []string{"nginx", "curl=8.5.0-2ubuntu10.1"},
		URLs:      []buildjob.URLInput{{URL: "https://vendor.example/agent.deb", SHA256: strings.Repeat("ab", 32)}, {URL: "https://vendor.example/plugin.deb"}},
		LocalDebs: []string{"/srv/debs/inhouse.deb"},
		LocalDirs: []string{"/srv/vendor"},
		ListFiles: []string{"/srv/packages.txt"},
		OutPath:   "/media/usb/bundle.debark.tar.zst",
		Format:    buildjob.FormatTar,
		SignerRef: "gpg:ABCD1234",
		SBOM:      true, Recommends: boolPtr(false), Upgrades: true, Update: true, NoPrune: true,
		Backend: "container", Image: "docker.io/library/ubuntu:24.04",
		PolicyPath: "/etc/debark/policy.yaml", ApprovedKeysPath: "/etc/debark/keys.txt",
		AcknowledgeRedistribution: true,
		EmbedBinary:               "/usr/bin/debark-linux-arm64",
	}
}

// The frozen flag construction. Command must return exactly this, and the
// real adapter must run exactly this, so a change here is a contract change.
func TestCommandPinsFlagConstruction(t *testing.T) {
	want := []string{
		"debark", "build",
		"--base", "ubuntu:24.04/server",
		"--arch", "arm64",
		"--list", "/srv/packages.txt",
		"--local-dir", "/srv/vendor",
		"--tar", "/media/usb/bundle.debark.tar.zst",
		"--update", "--no-prune", "--upgrades", "--no-recommends",
		"--backend", "container",
		"--image", "docker.io/library/ubuntu:24.04",
		"--sign", "gpg:ABCD1234",
		"--approved-keys", "/etc/debark/keys.txt",
		"--policy", "/etc/debark/policy.yaml",
		"--acknowledge-redistribution",
		"--embed-binary", "/usr/bin/debark-linux-arm64",
		"--sbom",
		"--digest", "https://vendor.example/agent.deb=" + strings.Repeat("ab", 32),
		"--json",
		"--",
		"apt:nginx", "apt:curl=8.5.0-2ubuntu10.1",
		"url:https://vendor.example/agent.deb", "url:https://vendor.example/plugin.deb",
		"file:/srv/debs/inhouse.deb",
	}
	a := &fake.Adapter{}
	got := a.Command(fullSpec())
	if len(got) != len(want) {
		t.Fatalf("argv has %d elements, want %d\n got: %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Command must be BuildArgv and nothing else, or the fake and the real
	// adapter can disagree about what the operator was shown.
	direct := cliadapter.BuildArgv(fullSpec())
	if strings.Join(direct, " ") != strings.Join(got, " ") {
		t.Errorf("Command != BuildArgv:\n %q\n %q", got, direct)
	}
}

// --json is not optional: it is what stops debark asking a question on a
// terminal the GUI does not have.
func TestArgvAlwaysPassesJSON(t *testing.T) {
	argv := cliadapter.BuildArgv(cliadapter.BuildSpec{SnapshotPath: "s.tar.zst", Packages: []string{"nginx"}})
	var found bool
	for _, s := range argv {
		if s == "--json" {
			found = true
		}
		if s == "--interactive" {
			t.Fatal("--interactive must never appear in a GUI command line")
		}
	}
	if !found {
		t.Fatalf("argv %q does not pass --json", argv)
	}
}

// The --json-events destination is this adapter's private temp file, and it
// must not appear in the command an operator is shown.
func TestArgvOmitsJSONEvents(t *testing.T) {
	for _, s := range cliadapter.BuildArgv(fullSpec()) {
		if strings.HasPrefix(s, "--json-events") {
			t.Fatalf("argv leaks the event destination: %q", s)
		}
	}
}

func TestArgvDefaultsToOutNotTar(t *testing.T) {
	argv := cliadapter.BuildArgv(cliadapter.BuildSpec{
		SnapshotPath: "s.tar.zst", Packages: []string{"nginx"}, OutPath: "/tmp/b",
	})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--out /tmp/b") {
		t.Errorf("default format did not become --out: %q", joined)
	}
	if strings.Contains(joined, "--tar") {
		t.Errorf("default format emitted --tar: %q", joined)
	}
}

func TestValidateRejectsWhatTheCLIRejects(t *testing.T) {
	base := cliadapter.BuildSpec{SnapshotPath: "s.tar.zst", Packages: []string{"nginx"}}
	cases := map[string]cliadapter.BuildSpec{
		"no target":            {Packages: []string{"nginx"}},
		"both targets":         {SnapshotPath: "s", BaseID: "ubuntu:24.04/server", Packages: []string{"nginx"}},
		"arch with snapshot":   {SnapshotPath: "s", Arch: "arm64", Packages: []string{"nginx"}},
		"sign and no-sign":     {SnapshotPath: "s", Packages: []string{"nginx"}, SignerRef: "k.key", NoSign: true},
		"nothing selected":     {SnapshotPath: "s"},
		"empty url":            {SnapshotPath: "s", URLs: []buildjob.URLInput{{URL: "  "}}},
		"digest is not sha256": {SnapshotPath: "s", URLs: []buildjob.URLInput{{URL: "https://x/y.deb", SHA256: "sha256:abc"}}},
		"unknown format":       {SnapshotPath: "s", Packages: []string{"nginx"}, Format: "iso"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			err := spec.Validate()
			if err == nil {
				t.Fatal("Validate accepted a spec the CLI would refuse")
			}
			e, ok := cliadapter.AsError(err)
			if !ok {
				t.Fatalf("Validate returned %T, want *cliadapter.Error", err)
			}
			if !e.HasClass(dferr.Usage) {
				t.Errorf("class = %s, want usage", e.ClassName())
			}
			assertActionable(t, e)
		})
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate rejected a legal spec: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The scripted build
// ---------------------------------------------------------------------------

func replay(t *testing.T, a *fake.Adapter, spec cliadapter.BuildSpec) ([]cliadapter.Event, *buildjob.BuildResult, error) {
	t.Helper()
	var got []cliadapter.Event
	res, err := a.Build(context.Background(), spec, func(e cliadapter.Event) { got = append(got, e) })
	return got, res, err
}

func demoSpec() cliadapter.BuildSpec {
	return cliadapter.BuildSpec{
		SnapshotPath: "/srv/snapshots/prod.tar.zst",
		Packages:     []string{"nginx"},
		URLs:         []buildjob.URLInput{{URL: "https://vendor.example/agent_4.2.0_amd64.deb"}},
		OutPath:      "/srv/out/bundle",
	}
}

func TestBuildReplaysAConvincingScript(t *testing.T) {
	events, res, err := replay(t, &fake.Adapter{}, demoSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res == nil || res.ExitClass != buildjob.ExitSuccess {
		t.Fatalf("result = %+v, want a successful BuildResult", res)
	}
	if len(events) < 40 {
		t.Fatalf("script delivered %d events; that is not a build worth demoing", len(events))
	}

	seen := map[string]int{}
	for _, e := range events {
		if e.Schema != evidence.SchemaVersion {
			t.Fatalf("event %q carries schema %q, want %q", e.Type, e.Schema, evidence.SchemaVersion)
		}
		if e.TS == "" {
			t.Fatalf("event %q has no timestamp", e.Type)
		}
		seen[e.Type]++
	}
	// The arc a progress UI has to render.
	for _, typ := range []string{
		evidence.TypeSnapshotLoaded, evidence.TypeBackendSelected, evidence.TypeAPTUpdate,
		evidence.TypeAPTResolve, evidence.TypeFetchFile, evidence.TypeProgress,
		evidence.TypeStoreHit, evidence.TypeDoctorFinding, evidence.TypeClosedWorld,
		evidence.TypeRepoIndexed, evidence.TypeManifestSigned, evidence.TypeBundleAssembled,
		evidence.TypeWarning,
	} {
		if seen[typ] == 0 {
			t.Errorf("the script never emits %q", typ)
		}
	}
	// Timestamps advance, so a UI that sorts or diffs on TS behaves.
	var prev time.Time
	for _, e := range events {
		ts, perr := time.Parse("2006-01-02T15:04:05Z", e.TS)
		if perr != nil {
			t.Fatalf("event %q has unparseable TS %q", e.Type, e.TS)
		}
		if ts.Before(prev) {
			t.Fatalf("event %q went backwards in time (%s after %s)", e.Type, e.TS, prev)
		}
		prev = ts
	}
	// bundle.assembled is last: the UI switches screens on it.
	if last := events[len(events)-1]; last.Type != evidence.TypeBundleAssembled {
		t.Errorf("last event is %q, want %q", last.Type, evidence.TypeBundleAssembled)
	}
}

// The three progress shapes a naive UI gets wrong, all present on purpose.
func TestScriptExercisesTheAwkwardProgressShapes(t *testing.T) {
	events, _, err := replay(t, &fake.Adapter{}, demoSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var unknownTotal, retries, byteTicks int
	for _, e := range events {
		p, ok := cliadapter.ProgressOf(e)
		if !ok {
			continue
		}
		switch {
		case p.Retry:
			retries++
			if p.Attempt < 1 || p.Attempts < p.Attempt {
				t.Errorf("retry notice has attempt %d of %d", p.Attempt, p.Attempts)
			}
		default:
			byteTicks++
			if p.URL == "" {
				t.Error("byte progress with no url; there is no other correlation key")
			}
			if p.TotalBytes == -1 {
				unknownTotal++
				if f := p.Fraction(); f != -1 {
					t.Errorf("Fraction() = %v with an unknown total, want -1", f)
				}
				if p.Done() {
					t.Error("Done() is true with an unknown total")
				}
			}
		}
	}
	if unknownTotal == 0 {
		t.Error("no progress event carries total_bytes = -1; that path must be exercised")
	}
	if retries == 0 {
		t.Error("the script never retries a download")
	}
	if byteTicks < 20 {
		t.Errorf("only %d byte-progress events; a bar needs more than that to move", byteTicks)
	}
}

// There is no cumulative counter on the wire. This is how the build screen's
// progress UI must build one, and it is here so that the shape is proven
// against the fake before it is written against a real build.
func TestFilesAndBytesDoneComeFromFetchFileOnly(t *testing.T) {
	events, res, err := replay(t, &fake.Adapter{}, demoSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var files int
	var bytes int64
	weakProvenance := 0
	for _, e := range events {
		f, ok := cliadapter.FetchFileOf(e)
		if !ok {
			continue
		}
		files++
		bytes += f.Size
		if f.Filename == "" || f.Digest == "" {
			t.Errorf("fetch.file for %s is missing filename or digest", f.URL)
		}
		if f.Verification == "url-unverified" {
			weakProvenance++
		}
	}
	if files == 0 || bytes == 0 {
		t.Fatalf("accumulated %d files / %d bytes from fetch.file", files, bytes)
	}
	if weakProvenance == 0 {
		t.Error("the vendor URL with no digest should report url-unverified so the UI can say so")
	}
	if bytes > res.Stats.Bytes {
		t.Errorf("accumulated %d bytes, more than the result's total of %d", bytes, res.Stats.Bytes)
	}
}

func TestBuildValidatesBeforeRunning(t *testing.T) {
	a := &fake.Adapter{}
	_, err := a.Build(context.Background(), cliadapter.BuildSpec{Packages: []string{"nginx"}}, nil)
	if err == nil {
		t.Fatal("Build accepted a spec with no target")
	}
	e, _ := cliadapter.AsError(err)
	assertActionable(t, e)
}

func TestBuildAcceptsANilSink(t *testing.T) {
	if _, err := (&fake.Adapter{}).Build(context.Background(), demoSpec(), nil); err != nil {
		t.Fatalf("Build with a nil sink: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestBuildHonoursCancellationMidStream(t *testing.T) {
	a := &fake.Adapter{Speed: 0.02} // ~200 ms of scripted time, so cancel lands mid-stream
	ctx, cancel := context.WithCancel(context.Background())
	var n int
	done := make(chan error, 1)
	go func() {
		_, err := a.Build(ctx, demoSpec(), func(cliadapter.Event) {
			n++
			if n == 5 {
				cancel()
			}
		})
		done <- err
	}()
	select {
	case err := <-done:
		e, ok := cliadapter.AsError(err)
		if !ok {
			t.Fatalf("cancelled Build returned %T (%v), want *cliadapter.Error", err, err)
		}
		if !e.Canceled() {
			t.Errorf("Canceled() is false on a cancelled build: %v", e)
		}
		assertActionable(t, e)
		if n >= len(fake.DefaultScript(demoSpec())) {
			t.Errorf("the whole script was delivered (%d events) despite cancellation", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Build did not return after its context was cancelled")
	}
}

func TestBuildRefusesAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var delivered int
	_, err := (&fake.Adapter{}).Build(ctx, demoSpec(), func(cliadapter.Event) { delivered++ })
	if err == nil {
		t.Fatal("Build ran with a cancelled context")
	}
	if delivered != 0 {
		t.Errorf("%d events were delivered on a cancelled context", delivered)
	}
}

// ---------------------------------------------------------------------------
// Failure injection
// ---------------------------------------------------------------------------

// The definition-of-done item this package owns: every exit class renders an
// actionable message, and none surfaces a raw exit code alone.
func TestEveryExitClassRendersAnActionableError(t *testing.T) {
	for _, class := range dferr.Classes() {
		t.Run(class.String(), func(t *testing.T) {
			a := &fake.Adapter{FailWith: fake.FailureForClass(class), FailAfter: -1}
			var events []cliadapter.Event
			res, err := a.Build(context.Background(), fullSpec(), func(e cliadapter.Event) { events = append(events, e) })
			if err == nil {
				t.Fatal("injected failure did not fail the build")
			}
			e, ok := cliadapter.AsError(err)
			if !ok {
				t.Fatalf("Build returned %T, want *cliadapter.Error", err)
			}
			if e.Class() != class {
				t.Errorf("class = %s (exit %d), want %s", e.ClassName(), e.ExitCode(), class)
			}
			if e.ExitCode() != int(class) {
				t.Errorf("ExitCode() = %d, want %d", e.ExitCode(), int(class))
			}
			assertActionable(t, e)
			if len(e.Argv()) == 0 {
				t.Error("the error carries no argv; the details drawer would be empty")
			}
			if len(events) == 0 {
				t.Error("FailAfter -1 should replay the whole script before failing")
			}
			// Exit 3 is the one class that still produced a bundle, and its
			// only detail is in the result.
			if class == dferr.Incomplete {
				if res == nil {
					t.Fatal("an incomplete build returned no result; Unresolved and FetchFailed are the only place the detail exists")
				}
				if res.ExitClass != buildjob.ExitIncomplete {
					t.Errorf("result.ExitClass = %q, want %q", res.ExitClass, buildjob.ExitIncomplete)
				}
				if len(res.Unresolved) == 0 && len(res.FetchFailed) == 0 {
					t.Error("an incomplete result names neither an unresolved input nor a failed download")
				}
			} else if class != dferr.Success && res != nil {
				t.Errorf("class %s returned a result as well as an error", class)
			}
		})
	}
}

// A usage failure carries cobra's whole help text. It must land in the
// details drawer and never in the summary.
func TestUsageFailureKeepsTheUsageBlockOutOfTheSummary(t *testing.T) {
	e := fake.UsageFailure("")([]string{"debark", "build"})
	if strings.Contains(e.Summary(), "Usage:") || strings.Contains(e.Summary(), "--base string") {
		t.Errorf("summary swallowed the usage block: %q", e.Summary())
	}
	if !strings.Contains(e.Stderr(), "Usage:") {
		t.Error("the usage block is not in Stderr(), so the details drawer has nothing to show")
	}
	if strings.HasPrefix(e.Summary(), "debark: ") {
		t.Errorf("summary kept the %q prefix: %q", "debark: ", e.Summary())
	}
}

// An error debark printed with a hint must keep the hint separate from the
// message, because that is where the UI renders each of them.
func TestHintIsSplitFromTheSummary(t *testing.T) {
	e := fake.EnvironmentFailure()([]string{"debark", "build"})
	if e.Hint() == "" {
		t.Fatal("the hint line was folded into the summary")
	}
	if strings.Contains(e.Summary(), e.Hint()) {
		t.Errorf("summary %q contains the hint %q", e.Summary(), e.Hint())
	}
	if !strings.Contains(e.Hint(), "docker") {
		t.Errorf("hint = %q, want debark's own next action", e.Hint())
	}
}

// Exit 3 leaves stderr empty, because build reports through --json and a
// silent error. The class description has to stand in.
func TestIncompleteFailureFallsBackToTheClassDescription(t *testing.T) {
	e := fake.IncompleteFailure()([]string{"debark", "build"})
	if e.Stderr() != "" {
		t.Fatalf("this test assumes an empty stderr, got %q", e.Stderr())
	}
	if e.Summary() != dferr.Incomplete.Description() {
		t.Errorf("summary = %q, want the frozen class description %q", e.Summary(), dferr.Incomplete.Description())
	}
	assertActionable(t, e)
}

// A process that died in a way debark did not choose is an environment
// problem, not a usage one.
func TestExitCodeOutsideTheTableIsEnvironment(t *testing.T) {
	for _, code := range []int{-1, 8, 127, 3221225477} {
		e := cliadapter.NewError([]string{"debark", "build"}, code, "")
		if !e.HasClass(dferr.Environment) {
			t.Errorf("exit %d classified as %s, want environment", code, e.ClassName())
		}
		if e.ExitCode() != code {
			t.Errorf("exit %d was not preserved (got %d)", code, e.ExitCode())
		}
		assertActionable(t, e)
		if !strings.Contains(e.Summary(), strconv.Itoa(code)) {
			t.Errorf("summary %q loses the raw status %d", e.Summary(), code)
		}
	}
}

func TestBinaryMissingIsActionable(t *testing.T) {
	e := fake.BinaryMissingFailure()([]string{"debark", "version", "--json"})
	assertActionable(t, e)
	if !e.HasClass(dferr.Environment) {
		t.Errorf("class = %s, want environment", e.ClassName())
	}
}

// Exit 0 with an unusable result is still an error, and still has to say
// something a person can act on.
func TestSuccessFailureStillCarriesASummary(t *testing.T) {
	e := fake.SuccessFailure()([]string{"debark", "build"})
	assertActionable(t, e)
	if e.ExitCode() != 0 {
		t.Errorf("ExitCode() = %d, want 0", e.ExitCode())
	}
}

func TestFailAfterStopsTheStreamWhereItSays(t *testing.T) {
	a := &fake.Adapter{FailWith: fake.ResolutionFailure("acme-agent"), FailAfter: 3}
	events, _, err := replay(t, a, demoSpec())
	if err == nil {
		t.Fatal("injected failure did not fail the build")
	}
	if len(events) != 3 {
		t.Errorf("delivered %d events, want 3", len(events))
	}
	e, _ := cliadapter.AsError(err)
	if !strings.Contains(e.Summary(), "acme-agent") {
		t.Errorf("summary %q does not name the package that could not be resolved", e.Summary())
	}
}

func TestPerMethodErrorsAreInjectable(t *testing.T) {
	argv := []string{"debark", "verify", "/media/usb/bundle"}
	a := &fake.Adapter{VerifyErr: fake.VerificationFailure()(argv)}
	if _, err := a.Verify(context.Background(), "/media/usb/bundle"); err == nil {
		t.Fatal("VerifyErr was ignored")
	} else {
		e, ok := cliadapter.AsError(err)
		if !ok {
			t.Fatalf("Verify returned %T, want *cliadapter.Error", err)
		}
		if !e.HasClass(dferr.Verification) {
			t.Errorf("class = %s, want verification", e.ClassName())
		}
		assertActionable(t, e)
	}
}

// ---------------------------------------------------------------------------
// The other five methods
// ---------------------------------------------------------------------------

func TestListBasesReturnsTheRealBuiltinTable(t *testing.T) {
	list, err := (&fake.Adapter{}).ListBases(context.Background(), "amd64")
	if err != nil {
		t.Fatalf("ListBases: %v", err)
	}
	if list.Arch != "amd64" {
		t.Errorf("Arch = %q, want amd64", list.Arch)
	}
	if list.SchemaVersion == "" {
		t.Error("the listing carries no schema_version")
	}
	if len(list.Bases) < 6 {
		t.Fatalf("only %d bases; the builtin table is larger than that", len(list.Bases))
	}
	distros := map[string]bool{}
	for _, b := range list.Bases {
		distros[b.DistroID] = true
		if b.Digest == "" {
			t.Errorf("base %s has no digest; two builds of the same id cannot be told apart", b.ID)
		}
		if len(b.Seeds) == 0 {
			t.Errorf("base %s names no seeds", b.ID)
		}
		if b.Arch != "amd64" {
			t.Errorf("base %s was rendered for %q, not the requested amd64", b.ID, b.Arch)
		}
	}
	for _, want := range []string{"debian", "ubuntu"} {
		if !distros[want] {
			t.Errorf("the listing has no %s releases", want)
		}
	}
}

func TestListBasesDefaultsToTheHostArch(t *testing.T) {
	list, err := (&fake.Adapter{}).ListBases(context.Background(), "")
	if err != nil {
		t.Fatalf("ListBases: %v", err)
	}
	if list.Arch == "" {
		t.Error("an empty arch produced a listing that does not say what it was rendered for")
	}
}

func TestProbeReportsWhatTheAppBranchesOn(t *testing.T) {
	p, err := (&fake.Adapter{}).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.Path == "" || p.Version() == "" || p.Edition() == "" || p.Platform() == "" || p.HostArch == "" {
		t.Errorf("Probe is incomplete: %+v", p)
	}
	if !p.Capabilities.Bases || !p.Capabilities.JSONEvents {
		t.Errorf("capabilities = %+v, want a binary that can do both", p.Capabilities)
	}
}

func TestInspectSnapshotCarriesThePathTheCLIDoesNotReport(t *testing.T) {
	const path = "/srv/snapshots/prod.tar.zst"
	info, err := (&fake.Adapter{}).InspectSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}
	if info.Path != path {
		t.Errorf("Path = %q, want %q", info.Path, path)
	}
	if info.Snapshot.SchemaVersion == "" {
		t.Error("the snapshot document has no schema_version")
	}
	if info.Synthesized() {
		t.Error("the demo snapshot should be captured, not synthesized")
	}
	if info.Target().Arch == "" {
		t.Error("the target has no architecture")
	}
}

func TestKeygenDerivesThePublicPathLikeCoreSign(t *testing.T) {
	a := &fake.Adapter{}
	k, err := a.Keygen(context.Background(), "/home/op/.config/debark/operator.key")
	if err != nil {
		t.Fatalf("Keygen: %v", err)
	}
	if k.PublicKeyPath != "/home/op/.config/debark/operator.pub" {
		t.Errorf("PublicKeyPath = %q, want the .key suffix replaced by .pub", k.PublicKeyPath)
	}
	if k.KeyID == "" || k.PrivateKeyPath == "" {
		t.Errorf("KeyInfo is incomplete: %+v", k)
	}
	// --out is marked required by the CLI, and the fake refuses it the same way.
	if _, err := a.Keygen(context.Background(), ""); err == nil {
		t.Error("Keygen accepted an empty --out")
	} else {
		e, _ := cliadapter.AsError(err)
		assertActionable(t, e)
	}
}

func TestVerifyReportsAPassingBundle(t *testing.T) {
	r, err := (&fake.Adapter{}).Verify(context.Background(), "/media/usb/bundle")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK || !r.Signed || r.BundlePath != "/media/usb/bundle" {
		t.Errorf("report = %+v, want a signed, passing bundle at the given path", r)
	}
}

func TestCallsAreRecorded(t *testing.T) {
	a := &fake.Adapter{}
	_, _ = a.Probe(context.Background())
	_, _ = a.ListBases(context.Background(), "amd64")
	_, _ = a.Build(context.Background(), demoSpec(), nil)
	got := a.Calls()
	want := []string{"Probe", "ListBases", "Build"}
	if len(got) != len(want) {
		t.Fatalf("recorded %d calls, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Method != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i].Method, want[i])
		}
		if len(got[i].Argv) == 0 {
			t.Errorf("call %q recorded no argv", got[i].Method)
		}
	}
	if a.LastSpec().SnapshotPath != demoSpec().SnapshotPath {
		t.Error("LastSpec did not record the build's spec")
	}
	a.Reset()
	if len(a.Calls()) != 0 {
		t.Error("Reset left calls behind")
	}
}

// assertActionable is the definition-of-done check, in one place: an error a
// person can act on, and nothing that renders as a bare exit code.
func assertActionable(t *testing.T, e *cliadapter.Error) {
	t.Helper()
	if e == nil {
		t.Fatal("nil error where one was expected")
	}
	if strings.TrimSpace(e.Summary()) == "" {
		t.Error("the error has no summary; a raw exit code would be all the UI could show")
	}
	if _, err := strconv.Atoi(strings.TrimSpace(e.Summary())); err == nil {
		t.Errorf("the summary is nothing but a number: %q", e.Summary())
	}
	msg := e.Error()
	if !strings.Contains(msg, e.Summary()) {
		t.Errorf("Error() = %q does not contain the summary %q", msg, e.Summary())
	}
	if !strings.Contains(msg, e.ClassName()) {
		t.Errorf("Error() = %q does not name the class %q", msg, e.ClassName())
	}
}

// A fake whose probe says the binary has no --json-events must behave like
// one: the build runs to completion and the sink is never called. Without it
// there is no way to drive the indeterminate build screen without finding a
// decade-old debark.
func TestBuildStreamsNothingWithoutTheCapability(t *testing.T) {
	probe := cliadapter.Probe{
		Path:         "/usr/bin/debark",
		Capabilities: cliadapter.Capabilities{Bases: true, Keygen: true},
	}
	a := &fake.Adapter{ProbeResult: &probe}

	var seen int
	res, err := a.Build(context.Background(), fullSpec(), func(cliadapter.Event) { seen++ })
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res == nil {
		t.Fatal("a build against an older binary produced no result")
	}
	if seen != 0 {
		t.Fatalf("delivered %d events from a binary that cannot stream any", seen)
	}

	// And the default fake, which claims the capability, still streams.
	b := &fake.Adapter{}
	seen = 0
	if _, err := b.Build(context.Background(), fullSpec(), func(cliadapter.Event) { seen++ }); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if seen == 0 {
		t.Fatal("the default fake streamed nothing")
	}
}

// The Install-Recommends override has three states, because the CLI now has
// three flags' worth of them: --recommends, --no-recommends, and neither. A
// bool could not tell "follow the target" from "definitely exclude", which are
// different builds — a target whose apt.conf turns recommends on gets them
// under the first and not the second.
func TestRecommendsHasThreeStates(t *testing.T) {
	spec := func(r *bool) cliadapter.BuildSpec {
		return cliadapter.BuildSpec{BaseID: "ubuntu:24.04/server", Packages: []string{"nginx"}, Recommends: r}
	}
	has := func(argv []string, flag string) bool {
		for _, a := range argv {
			if a == flag {
				return true
			}
		}
		return false
	}

	follow := cliadapter.BuildArgv(spec(nil))
	if has(follow, "--recommends") || has(follow, "--no-recommends") {
		t.Fatalf("a nil override passed a flag: %v", follow)
	}
	off := cliadapter.BuildArgv(spec(boolPtr(false)))
	if !has(off, "--no-recommends") || has(off, "--recommends") {
		t.Fatalf("false did not become --no-recommends alone: %v", off)
	}
	on := cliadapter.BuildArgv(spec(boolPtr(true)))
	if !has(on, "--recommends") || has(on, "--no-recommends") {
		t.Fatalf("true did not become --recommends alone: %v", on)
	}
}
