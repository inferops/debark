package app

// Tests for docs/security-review.md §6.1a and §6.5a — what the build screen and
// the preview panel are allowed to put in front of a person, and what the
// process is still given.
//
// Two properties, and they pull in opposite directions, which is the whole
// reason this file exists as its own thing:
//
//  1. Every command line that reaches the bridge is REDACTED. A string built to
//     be read and copied must not carry a password.
//  2. The command line that reaches the PROCESS is EXACT. Redaction anywhere on
//     that path turns every credentialed build into a fetch of
//     https://REDACTED@vendor.example/… and breaks the product outright.
//
// Getting them the wrong way round is the single risk in this change, so both
// are asserted against the same run.

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/inferops/debark/gui/internal/cliadapter"

	catfake "github.com/inferops/debark/gui/internal/catalog/fake"
	clifake "github.com/inferops/debark/gui/internal/cliadapter/fake"
)

const (
	// redSecret is a password in the userinfo of a vendor URL. apt-style
	// credentials in a URL are a real deployment, which is why the tray accepts
	// one at all.
	redSecret = "s3cr3t-deploy-token"
	// redPresigned is the other shape: the credential is a query parameter
	// whose name is not standardised, so it cannot be told from a harmless one.
	redPresigned = "AKIAEXAMPLESIGNATUREVALUE"
	redSHA       = "3333333333333333333333333333333333333333333333333333333333333333"

	redCredURL     = "https://deploy:" + redSecret + "@vendor.example/pool/agent_2.1.0_amd64.deb"
	redPresignURL  = "https://cdn.example/pool/tool.deb?X-Amz-Signature=" + redPresigned
	redInnocentURL = "https://plain.example/pool/plain.deb"
)

// newRedactionApp is newApp with two things it does not give: the fake adapter
// itself, so a test can read the argv the process was actually handed, and the
// event payloads rather than a count, so build:started's Command can be read.
func newRedactionApp(t *testing.T) (*App, *clifake.Adapter, func(string) []any) {
	t.Helper()
	cli := &clifake.Adapter{}
	cat := catfake.NewDefault()
	cat.SetReady(false)

	var mu sync.Mutex
	seen := map[string][]any{}

	a := New(Deps{
		CLI:     cli,
		Catalog: cat,
		Emit: func(name string, payload any) {
			mu.Lock()
			seen[name] = append(seen[name], payload)
			mu.Unlock()
		},
		Version: "test",
	})
	a.Startup(context.Background())
	t.Cleanup(func() { a.Shutdown(context.Background()) })

	return a, cli, func(name string) []any {
		mu.Lock()
		defer mu.Unlock()
		return append([]any(nil), seen[name]...)
	}
}

// redTrayWithCredentials selects a target and fills the tray with the three
// URL shapes that matter: userinfo, a presigned query token, and one with
// neither.
func redTrayWithCredentials(t *testing.T, a *App) {
	t.Helper()
	if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); r.Error != nil {
		t.Fatalf("SelectTarget: %v", r.Error)
	}
	if s := a.AddPackages([]string{"jq"}); s.Error != nil {
		t.Fatalf("AddPackages: %v", s.Error)
	}
	s := a.AddURLs([]URLInput{
		{URL: redCredURL, SHA256: redSHA},
		{URL: redPresignURL},
		{URL: redInnocentURL, SHA256: redSHA},
	})
	if s.Error != nil {
		t.Fatalf("AddURLs: %v", s.Error)
	}
	if s.URLCount != 3 {
		t.Fatalf("the tray took %d of 3 URLs; the fixture is not the one this test describes", s.URLCount)
	}
}

func redContains(argv []string, needle string) bool {
	return slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, needle) })
}

// TestBuildRunsTheExactArgvWhileTheScreenShowsTheRedactedOne is the test that
// catches getting the two halves backwards, in either direction.
//
// Backwards one way — the display strings keep the credential — and §6.1 is
// unfixed: the operator's password is on the build screen and in their
// clipboard. Backwards the other way — the redaction reaches the process — and
// every build with a credentialed vendor URL fetches https://REDACTED@… and
// fails. The first is a security defect and the second is an outage, so both
// are asserted here rather than trusted to a reading of the code.
func TestBuildRunsTheExactArgvWhileTheScreenShowsTheRedactedOne(t *testing.T) {
	a, cli, events := newRedactionApp(t)
	redTrayWithCredentials(t, a)
	opts := BuildOptions{OutputDir: t.TempDir(), NoSign: true}

	// --- What a person is shown before the build ---------------------------

	pv := a.PreviewCommand(opts)
	if pv.Error != nil {
		t.Fatalf("PreviewCommand: %v", pv.Error)
	}
	for _, secret := range []string{redSecret, redPresigned} {
		if redContains(pv.Argv, secret) {
			t.Errorf("CommandPreview.Argv still carries %q:\n%v", secret, pv.Argv)
		}
		if strings.Contains(pv.Display, secret) {
			t.Errorf("CommandPreview.Display still carries %q:\n%s", secret, pv.Display)
		}
	}
	if !redContains(pv.Argv, "REDACTED") {
		t.Errorf("nothing in the preview says a redaction happened:\n%v", pv.Argv)
	}
	// The redaction must be visible in the copied line too, not only in the
	// argv the frontend rarely renders directly.
	if !strings.Contains(pv.Display, "REDACTED") {
		t.Errorf("the copied line does not say a redaction happened:\n%s", pv.Display)
	}
	// Everything that is not a credential survives, or the preview has stopped
	// answering the question rule 8 asks it to answer.
	for _, want := range []string{
		"--base", "ubuntu:24.04/desktop", "--json", "--",
		"apt:jq",
		"url:" + redInnocentURL,
		"url:https://REDACTED@vendor.example/pool/agent_2.1.0_amd64.deb",
		redInnocentURL + "=" + redSHA,
		// The digest is not a secret. Losing it would hide, in the previewed
		// command, the very attestation the operator asked for.
		"https://REDACTED@vendor.example/pool/agent_2.1.0_amd64.deb=" + redSHA,
	} {
		if !slices.Contains(pv.Argv, want) {
			t.Errorf("the previewed command lost %q:\n%v", want, pv.Argv)
		}
	}

	// --- What the build screen is shown while and after it runs ------------

	if r := a.StartBuild(opts); !r.OK {
		t.Fatalf("StartBuild: %v", r.Error)
	}
	waitFor(t, "build:finished", func() bool { return len(events(EventBuildFinished)) == 1 })

	started := events(EventBuildStarted)
	if len(started) != 1 {
		t.Fatalf("build:started fired %d times, want 1", len(started))
	}
	bs, ok := started[0].(BuildStarted)
	if !ok {
		t.Fatalf("build:started carried %T, not BuildStarted", started[0])
	}
	status := a.BuildStatus()

	for name, argv := range map[string][]string{
		"BuildStarted.Command": bs.Command,
		"BuildStatus.Command":  status.Command,
	} {
		if len(argv) == 0 {
			t.Fatalf("%s is empty; the screen has no command to show at all", name)
		}
		for _, secret := range []string{redSecret, redPresigned} {
			if redContains(argv, secret) {
				t.Errorf("%s still carries %q:\n%v", name, secret, argv)
			}
		}
		if !redContains(argv, "REDACTED") {
			t.Errorf("%s does not say a redaction happened:\n%v", name, argv)
		}
		// The two must be the same string. The build screen renders one and
		// its details drawer copies the other; a difference between them is a
		// screen that contradicts itself.
		if !slices.Equal(argv, bs.Command) {
			t.Errorf("%s differs from the build:started payload:\n%v\n%v", name, argv, bs.Command)
		}
	}
	// And it must be the same string the preview showed, or the operator read
	// one command and watched a different one run.
	if !slices.Equal(pv.Argv, status.Command) {
		t.Errorf("the preview and the running build disagree:\npreview %v\nstatus  %v", pv.Argv, status.Command)
	}

	// --- What the process was actually given -------------------------------

	// The fake records Build with cliadapter.BuildArgv(spec) — literally the
	// argv the real adapter execs (events.go: `exec.CommandContext(ctx, bin,
	// runArgv[1:]...)`). So this is the executed command line, not a
	// reconstruction of it.
	var executed []string
	for _, c := range cli.Calls() {
		if c.Method == "Build" {
			executed = c.Argv
		}
	}
	if executed == nil {
		t.Fatal("the fake never recorded a Build call")
	}
	if !slices.Equal(executed, cliadapter.BuildArgv(cli.LastSpec())) {
		t.Fatalf("the executed argv is not BuildArgv of the spec Build received:\n%v", executed)
	}
	for _, secret := range []string{redSecret, redPresigned} {
		if !redContains(executed, secret) {
			t.Fatalf("the EXECUTED argv no longer carries %q. The redaction has reached the "+
				"process: every build with a credentialed vendor URL will now fetch "+
				"https://REDACTED@… and fail. Redact where the argv becomes text, never "+
				"where it becomes a process.\n%v", secret, executed)
		}
	}
	if redContains(executed, "REDACTED") {
		t.Fatalf("the executed argv contains the redaction marker:\n%v", executed)
	}

	// Displayed and executed differ, and differ ONLY in the credentials. A
	// redaction that changed the shape of the command would make the preview
	// answer rule 8's question with a different command.
	if slices.Equal(executed, status.Command) {
		t.Fatal("the displayed command and the executed one are identical; nothing was redacted")
	}
	if len(executed) != len(status.Command) {
		t.Fatalf("redaction changed the argv's shape: %d elements executed, %d shown",
			len(executed), len(status.Command))
	}
	for i := range executed {
		shown := status.Command[i]
		if shown == executed[i] {
			continue
		}
		if !strings.Contains(shown, "REDACTED") {
			t.Errorf("argv[%d] differs from what ran without saying it was redacted:\nran   %s\nshown %s",
				i, executed[i], shown)
		}
	}
}

// TestRedactArgvIsNotAppliedToTheSpec guards the quieter half of the same
// mistake: a spec scrubbed on the way in would leave every display string
// looking correct while the build fetched a URL nobody typed.
func TestRedactArgvIsNotAppliedToTheSpec(t *testing.T) {
	a, cli, _ := newRedactionApp(t)
	redTrayWithCredentials(t, a)

	// PreviewCommand alone, with no build: even the preview path must hand the
	// adapter the real spec, because Command's contract is that the previewed
	// command IS the executed one and the adapter is entitled to rely on it.
	if pv := a.PreviewCommand(BuildOptions{OutputDir: t.TempDir()}); pv.Error != nil {
		t.Fatalf("PreviewCommand: %v", pv.Error)
	}
	spec := cli.LastSpec()
	var found bool
	for _, u := range spec.URLs {
		if u.URL == redCredURL {
			found = true
		}
		if strings.Contains(u.URL, "REDACTED") {
			t.Fatalf("the spec handed to the adapter was redacted: %s", u.URL)
		}
	}
	if !found {
		t.Fatalf("the credentialed URL did not reach the spec at all: %+v", spec.URLs)
	}
}

// TestPreviewCommandRefusesWhateverStartBuildWouldRefuse is §6.1a's third item.
//
// CommandPreview has carried an Error field since the surface was frozen, and
// StartBuild has always called Validate. PreviewCommand did not, so the panel
// would render a command the application then refused to run — the operator
// reads it, copies it, and finds out on Build. The case that makes it more than
// tidiness is the digest one: `build --digest` misbinds a URL containing "="
// silently (§6.5a), so the command shown was not merely un-runnable here, it
// was one that would have run against a different key on the other end.
func TestPreviewCommandRefusesWhateverStartBuildWouldRefuse(t *testing.T) {
	// A URL with a query string, which is an ordinary shape, plus the digest
	// the operator asked to be checked.
	const queryURL = "https://vendor.example/download?file=agent_2.1.0_amd64.deb"

	cases := []struct {
		name    string
		tray    func(t *testing.T, a *App)
		opts    func(dir string) BuildOptions
		wantSub string
	}{{
		name: "a digest paired with a URL containing =",
		tray: func(t *testing.T, a *App) {
			if s := a.AddURLs([]URLInput{{URL: queryURL, SHA256: redSHA}}); s.Error != nil {
				t.Fatalf("AddURLs: %v", s.Error)
			}
		},
		opts:    func(dir string) BuildOptions { return BuildOptions{OutputDir: dir, NoSign: true} },
		wantSub: "cannot be passed to this debark",
	}, {
		name: "a signing key and write-it-unsigned together",
		tray: func(t *testing.T, a *App) {
			if s := a.AddPackages([]string{"jq"}); s.Error != nil {
				t.Fatalf("AddPackages: %v", s.Error)
			}
		},
		opts: func(dir string) BuildOptions {
			return BuildOptions{OutputDir: dir, NoSign: true, SignerRef: "key.pem"}
		},
		wantSub: "unsigned",
	}, {
		name: "an unknown output format",
		tray: func(t *testing.T, a *App) {
			if s := a.AddPackages([]string{"jq"}); s.Error != nil {
				t.Fatalf("AddPackages: %v", s.Error)
			}
		},
		opts: func(dir string) BuildOptions {
			return BuildOptions{OutputDir: dir, NoSign: true, Format: "iso"}
		},
		wantSub: "output format",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := newRedactionApp(t)
			if r := a.SelectTarget(TargetSelection{Kind: TargetKindBase, BaseID: "ubuntu:24.04/desktop", Arch: "amd64"}); r.Error != nil {
				t.Fatalf("SelectTarget: %v", r.Error)
			}
			tc.tray(t, a)
			opts := tc.opts(t.TempDir())

			pv := a.PreviewCommand(opts)
			if pv.Error == nil {
				t.Fatalf("PreviewCommand rendered a command the build would refuse:\n%s", pv.Display)
			}
			// Nothing may be shown alongside the refusal. A command in the
			// panel next to "this will not run" is the thing that gets copied.
			if len(pv.Argv) != 0 || pv.Display != "" {
				t.Errorf("a refused preview still carried a command: argv=%v display=%q", pv.Argv, pv.Display)
			}
			if pv.Error.Message == "" {
				t.Error("the refusal has no message; UIError with an empty Message is a bug")
			}
			if pv.Error.Hint == "" {
				t.Error("the refusal has no hint, and this is a thing the operator can act on")
			}
			if !strings.Contains(strings.ToLower(pv.Error.Message+" "+pv.Error.Hint), tc.wantSub) {
				t.Errorf("the refusal does not mention %q:\n%s\n%s", tc.wantSub, pv.Error.Message, pv.Error.Hint)
			}

			// The two doors into a build must give the same answer. That is
			// the whole point: a preview that disagrees with StartBuild is a
			// preview of a different application.
			sb := a.StartBuild(opts)
			if sb.OK {
				t.Fatal("StartBuild accepted what PreviewCommand refused")
			}
			if sb.Error.Code != pv.Error.Code || sb.Error.Message != pv.Error.Message {
				t.Errorf("preview and StartBuild refuse differently:\npreview %s / %s\nbuild   %s / %s",
					pv.Error.Code, pv.Error.Message, sb.Error.Code, sb.Error.Message)
			}
		})
	}
}

// TestAFailedBuildDoesNotPutTheCredentialInTheDetailsDrawer covers the fourth
// call site, which docs/security-review.md §6.1a did not enumerate.
//
// UIError.Command is cliadapter.Error.Argv(), and internal/cliadapter/events.go
// records what that is for: "Error.Argv is an exported accessor with a 'copy
// the command' button behind it". So the drawer an operator opens when a build
// fails was rendering the password in plain text, twice, while the preview
// panel two screens back had just been fixed not to. Photographed in the
// running app before this test was written.
//
// The other half is asserted too: the argv that FAILED is still the exact one,
// because the process really did run with the credential and an error that
// described a different command line would be a worse artefact than a redacted
// one.
func TestAFailedBuildDoesNotPutTheCredentialInTheDetailsDrawer(t *testing.T) {
	a, cli, events := newRedactionApp(t)
	cli.FailWith = clifake.EnvironmentFailure()
	redTrayWithCredentials(t, a)

	if r := a.StartBuild(BuildOptions{OutputDir: t.TempDir(), NoSign: true}); !r.OK {
		t.Fatalf("StartBuild: %v", r.Error)
	}
	waitFor(t, "build:finished", func() bool { return len(events(EventBuildFinished)) == 1 })

	fin, ok := events(EventBuildFinished)[0].(BuildFinished)
	if !ok {
		t.Fatalf("build:finished carried %T, not BuildFinished", events(EventBuildFinished)[0])
	}
	status := a.BuildStatus()
	if fin.Error == nil || status.Error == nil {
		t.Fatalf("the injected failure produced no UIError: finished=%+v status=%+v", fin.Error, status.Error)
	}

	for name, ue := range map[string]*UIError{
		"build:finished Error": fin.Error,
		"BuildStatus.Error":    status.Error,
	} {
		if len(ue.Command) == 0 {
			t.Fatalf("%s carries no command at all; the drawer has nothing to show", name)
		}
		for _, secret := range []string{redSecret, redPresigned} {
			if redContains(ue.Command, secret) {
				t.Errorf("%s.Command still carries %q — that is the details drawer's copy button:\n%v",
					name, secret, ue.Command)
			}
			// Details is captured stderr. debark's own messages are already
			// redacted upstream, but a leak there would be the same defect
			// wearing a different field name.
			if strings.Contains(ue.Details, secret) {
				t.Errorf("%s.Details carries %q:\n%s", name, secret, ue.Details)
			}
		}
		if !redContains(ue.Command, "REDACTED") {
			t.Errorf("%s.Command does not say a redaction happened:\n%v", name, ue.Command)
		}
	}

	// The failure still describes the command that actually ran, argument for
	// argument, in the same order and with the same shape.
	executed := cliadapter.BuildArgv(cli.LastSpec())
	if len(executed) != len(fin.Error.Command) {
		t.Fatalf("the reported argv has %d elements and the executed one %d; the error no longer "+
			"describes the run", len(fin.Error.Command), len(executed))
	}
	if !redContains(executed, redSecret) {
		t.Fatal("the executed argv lost the credential: the redaction has reached the process")
	}
}

// TestRedactArgvLeavesANonBuildErrorAlone pins the blast radius claimed above.
// A readiness remedy, a verify or a keygen carries file paths and package
// names, not vendor URLs, and must reach the drawer character for character —
// a redactor that rewrote them would make every OTHER error message worse to
// buy nothing.
func TestRedactArgvLeavesANonBuildErrorAlone(t *testing.T) {
	for name, argv := range map[string][]string{
		"keygen":           {"debark", "keygen", "--out", "/home/op/.config/debark/key.pem"},
		"verify":           {"debark", "verify", "--json", "/home/op/bundles/b1"},
		"snapshot inspect": {"debark", "snapshot", "inspect", "--json", "/mnt/usb/real.tar.zst"},
		"a remedy":         {"sudo", "apt-get", "install", "-y", "docker.io"},
		"a build with no URL inputs": {
			"debark", "build", "--base", "ubuntu:24.04/minimal", "--json", "--", "apt:jq",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ue := uiErrorFrom(cliadapter.NewError(argv, 2, "debark: it did not work\n"))
			if ue == nil {
				t.Fatal("uiErrorFrom returned nil for a real *cliadapter.Error")
			}
			if !slices.Equal(ue.Command, argv) {
				t.Errorf("the redaction rewrote an argv with no credential in it:\nwas  %v\nnow  %v", argv, ue.Command)
			}
		})
	}
}
