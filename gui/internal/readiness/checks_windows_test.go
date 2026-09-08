//go:build windows

package readiness

import (
	"context"
	"strings"
	"testing"
	"unicode/utf16"
)

// utf16LE encodes s the way wsl.exe writes it, so the parsing tests exercise
// the bytes that actually arrive rather than a convenient approximation.
func utf16LE(s string, bom bool) string {
	var b []byte
	if bom {
		b = append(b, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return string(b)
}

func TestDecodeConsole(t *testing.T) {
	const plain = "  NAME      STATE     VERSION\n* Ubuntu    Running   2\n"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"utf-16 with a bom", utf16LE(plain, true), plain},
		{"utf-16 without a bom", utf16LE(plain, false), plain},
		{"plain utf-8 is left alone", plain, plain},
		{"empty", "", ""},
		{"too short to be utf-16", "a", "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeConsole(c.in); got != c.want {
				t.Errorf("decodeConsole = %q, want %q", got, c.want)
			}
		})
	}
}

// TestParseWSLDistros covers the table wsl.exe prints, including the parts that
// are localised. Nothing here may depend on the header or the STATE column: on
// a German or Japanese Windows those words differ and the parser must not care.
func TestParseWSLDistros(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []wslDistro
	}{
		{
			name: "a normal machine",
			in: "  NAME              STATE           VERSION\n" +
				"* Ubuntu-22.04      Running         2\n" +
				"  docker-desktop    Stopped         2\n",
			want: []wslDistro{
				{Name: "Ubuntu-22.04", Version: 2, Default: true},
				{Name: "docker-desktop", Version: 2},
			},
		},
		{
			name: "a localised header and state",
			in: "  NAME      ZUSTAND     VERSION\n" +
				"* Debian    Beendet     1\n",
			want: []wslDistro{{Name: "Debian", Version: 1, Default: true}},
		},
		{
			name: "the default marker touching the name",
			in:   "  NAME STATE VERSION\n*Ubuntu Running 2\n",
			want: []wslDistro{{Name: "Ubuntu", Version: 2, Default: true}},
		},
		{
			name: "no distributions registered",
			in:   "Windows Subsystem for Linux has no installed distributions.\n",
			want: nil,
		},
		{
			name: "the install help text",
			in:   "Copyright (c) Microsoft Corporation.\nUsage: wsl.exe [Argument]\n",
			want: nil,
		},
		{name: "empty", in: "", want: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseWSLDistros(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("parsed %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseWSLVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"WSL version: 2.0.9.0\nKernel version: 5.15.133.1-1\n", "2.0.9.0"},
		{"WSL-Version: 2.1.5.0\n", "2.1.5.0"},
		{"", ""},
		{"no numbers here\n", ""},
	}
	for _, c := range cases {
		if got := parseWSLVersion(c.in); got != c.want {
			t.Errorf("parseWSLVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDecideWSL is the honest-answer test for the platform the brief says to
// build but not promise. Every failing row must name the exact command, and no
// row may be blocking on its own — Docker Desktop is a complete alternative and
// DeriveBuildEnvironment owns the "nothing at all" verdict.
func TestDecideWSL(t *testing.T) {
	cases := []struct {
		name         string
		obs          wslObservation
		wantStatus   Status
		wantMentions string
		wantCommand  string
		wantElevated bool
	}{
		{
			name:         "no wsl.exe at all",
			obs:          wslObservation{},
			wantStatus:   StatusProblem,
			wantMentions: "no wsl.exe",
		},
		{
			name:         "wsl present, component not installed",
			obs:          wslObservation{Path: `C:\Windows\System32\wsl.exe`, StatusOut: CommandOutput{ExitCode: 1, Stderr: "Windows Subsystem for Linux has no installed distributions."}},
			wantStatus:   StatusProblem,
			wantMentions: "not installed",
			wantCommand:  "wsl --install",
			wantElevated: true,
		},
		{
			name:         "installed but no distribution",
			obs:          wslObservation{Path: `C:\wsl.exe`, ComponentPresent: true, StatusOut: CommandOutput{}},
			wantStatus:   StatusProblem,
			wantMentions: "no Linux distribution is registered",
			wantCommand:  "wsl --install --distribution Ubuntu",
			wantElevated: true,
		},
		{
			name:         "a WSL 1 distribution",
			obs:          wslObservation{Path: `C:\wsl.exe`, ComponentPresent: true, Distros: []wslDistro{{Name: "Debian", Version: 1, Default: true}}},
			wantStatus:   StatusProblem,
			wantMentions: "runs on WSL 1",
			wantCommand:  "wsl --set-version Debian 2",
		},
		{
			name: "a working WSL 2",
			obs: wslObservation{
				Path: `C:\wsl.exe`, ComponentPresent: true, Version: "2.0.9.0",
				Distros: []wslDistro{{Name: "Ubuntu-22.04", Version: 2, Default: true}},
			},
			wantStatus:   StatusOK,
			wantMentions: "WSL 2.0.9.0 is available",
		},
		{
			name: "WSL 1 and WSL 2 side by side",
			obs: wslObservation{
				Path: `C:\wsl.exe`, ComponentPresent: true,
				Distros: []wslDistro{{Name: "Legacy", Version: 1}, {Name: "Ubuntu", Version: 2}},
			},
			wantStatus:   StatusOK,
			wantMentions: "available",
		},
		{
			name:         "wsl did not answer",
			obs:          wslObservation{Path: `C:\wsl.exe`, StatusOut: CommandOutput{TimedOut: true, ExitCode: -1}, ListOut: CommandOutput{TimedOut: true, ExitCode: -1}},
			wantStatus:   StatusProblem,
			wantMentions: "did not answer within the timeout",
			wantCommand:  "wsl --status",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideWSL(c.obs)
			if got.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, c.wantStatus)
			}
			if got.Severity == SeverityBlocking {
				t.Error("no single WSL row may block: Docker Desktop and a remote builder are both alternatives")
			}
			if !strings.Contains(got.Summary, c.wantMentions) {
				t.Errorf("Summary = %q, want it to mention %q", got.Summary, c.wantMentions)
			}
			switch {
			case c.wantCommand == "" && got.Action != nil:
				t.Errorf("offered %q, want no action", got.Action.String())
			case c.wantCommand != "":
				if got.Action == nil {
					t.Fatalf("no action offered; want %q", c.wantCommand)
				}
				if got.Action.String() != c.wantCommand {
					t.Errorf("action = %q, want %q", got.Action.String(), c.wantCommand)
				}
				if got.Action.Elevated != c.wantElevated {
					t.Errorf("Elevated = %v, want %v", got.Action.Elevated, c.wantElevated)
				}
			}
			if err := (Report{Results: []Result{got}}).Validate(); err != nil {
				t.Errorf("row breaks the contract: %v", err)
			}
		})
	}
}

// TestWSLRemediesOfferTheRemoteBuilder: on a managed laptop where policy blocks
// both WSL and Docker Desktop, building elsewhere is the only route that will
// ever work, so it has to be stated rather than implied.
func TestWSLRemediesOfferTheRemoteBuilder(t *testing.T) {
	rows := []Result{
		decideWSL(wslObservation{}),
		decideWSL(wslObservation{Path: `C:\wsl.exe`}),
	}
	for _, r := range rows {
		if !strings.Contains(r.Remedy, "Linux machine") {
			t.Errorf("remedy %q does not mention building on another machine", r.Remedy)
		}
	}
	if !strings.Contains(noBuildEnvironmentRemedy(), "Linux machine") {
		t.Errorf("the blocking remedy %q does not mention building on another machine", noBuildEnvironmentRemedy())
	}
}

// TestProbeWSLDecodesAndBoundsItsCommands checks the two things the probe owes
// the decision: UTF-16 output turned into something parseable, and all three
// wsl.exe calls made under the one deadline it was given.
func TestProbeWSLDecodesAndBoundsItsCommands(t *testing.T) {
	list := "  NAME      STATE     VERSION\n* Ubuntu    Running   2\n"
	fr := newFakeRunner(map[string]CommandOutput{
		"wsl": {Stdout: utf16LE(list, true)},
	})
	o := Options{Runner: fr.run, LookPath: lookPathIn("wsl")}.withDefaults()

	obs := probeWSL(context.Background(), o)
	if len(obs.Distros) != 1 || obs.Distros[0].Name != "Ubuntu" || obs.Distros[0].Version != 2 {
		t.Fatalf("distros = %+v, want one WSL 2 Ubuntu", obs.Distros)
	}
	if !obs.ComponentPresent {
		t.Error("a registered distribution means the component is present")
	}
	if got := len(fr.seen()); got != 3 {
		t.Errorf("made %d calls (%v), want --status, --version and --list --verbose", got, fr.seen())
	}
}

func TestFreeSpaceOnThisVolume(t *testing.T) {
	free, total, err := freeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("freeSpace: %v", err)
	}
	if total == 0 || free > total {
		t.Errorf("free = %d, total = %d", free, total)
	}
}

func TestFreeSpaceRejectsAMissingVolume(t *testing.T) {
	if _, _, err := freeSpace(`Q:\definitely\not\a\volume`); err == nil {
		t.Error("freeSpace must report an error rather than a made-up number")
	}
}

// TestWSLRowDoesNotPromiseAContainerlessBuild is the other half of the
// WSL defect. The derivation counting WSL 2 as a route was one half; the row
// itself said so, in green: "WSL 2 is available, so builds can run without a
// container." They cannot, on any machine, ever — debark runs apt natively
// or in a container and nothing else, and native apt is not registered on
// Windows at all.
//
// It lives in this file, not readiness_test.go, because decideWSL and
// wslObservation are behind //go:build windows. Asserting a Windows-only
// symbol from a portable test file does not fail on Windows and does not
// compile on Linux, where a runtime t.Skip is far too late — the whole package
// fails to build, so go vet, go test ./... and golangci-lint all go red on the
// shipping platform while the Windows suite stays green and cannot see it.
// That is exactly the defect fixed in core/apt's TestFileURI at the same time,
// back when the engine and this application were separate repositories.
func TestWSLRowDoesNotPromiseAContainerlessBuild(t *testing.T) {
	got := decideWSL(wslObservation{
		Path:             `C:\Windows\System32\wsl.exe`,
		ComponentPresent: true,
		Version:          "2.0.9.0",
		Distros:          []wslDistro{{Name: "Ubuntu", Version: 2, Default: true}},
	})
	if got.Status != StatusOK {
		t.Fatalf("a healthy WSL 2 is not a problem: %+v", got)
	}
	if strings.Contains(got.Summary, "without a container") {
		t.Fatalf("the WSL row still promises a containerless build: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "container runtime") {
		t.Errorf("the WSL row does not say where a build actually goes: %q", got.Summary)
	}
}
