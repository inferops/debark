package snapshot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The confirmed attack: a captured "warning" carrying cursor-up and
// erase-line, printed by the CLI underneath the digest line, scrolls back
// over the digest and replaces it with a forged one -- aimed squarely at the
// out-of-band digest comparison docs/threat-model.md §6 asks a reviewer
// to make.
const cursorUpEraseLine = "\x1b[1A\x1b[2Kdigest:      sha256:0000000000000000000000000000000000000000000000000000000000000000"

func honestDocument(t *testing.T) *Snapshot {
	t.Helper()
	return &Snapshot{
		SchemaVersion: SchemaVersion,
		CreatedAt:     "2026-09-04T00:00:00Z",
		Tool:          Tool{Name: "debark", Version: "0"},
		Origin:        Origin{Kind: OriginCaptured},
		Target: Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
			APTVersion: "2.6.1", DpkgVersion: "1.21.23",
		},
		DpkgStatus: File{
			Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status",
			SHA256: sha256Hex(nil), Size: 0,
		},
	}
}

func TestValidateRefusesTerminalControlSequences(t *testing.T) {
	cases := map[string]func(*Snapshot){
		"warnings":            func(s *Snapshot) { s.Warnings = []string{cursorUpEraseLine} },
		"labels value":        func(s *Snapshot) { s.Labels = map[string]string{"site": cursorUpEraseLine} },
		"labels key":          func(s *Snapshot) { s.Labels = map[string]string{cursorUpEraseLine: "x"} },
		"target.codename":     func(s *Snapshot) { s.Target.Codename = cursorUpEraseLine },
		"target.pretty_name":  func(s *Snapshot) { s.Target.PrettyName = cursorUpEraseLine },
		"target.apt_version":  func(s *Snapshot) { s.Target.APTVersion = "2.6.1" + cursorUpEraseLine },
		"target.dpkg_version": func(s *Snapshot) { s.Target.DpkgVersion = "1.21.23" + cursorUpEraseLine },
		"target.machine_id":   func(s *Snapshot) { s.Target.MachineID = cursorUpEraseLine },
		"tool.version":        func(s *Snapshot) { s.Tool.Version = cursorUpEraseLine },
		"file path":           func(s *Snapshot) { s.DpkgStatus.Path = "/var/lib/dpkg/\x1b[2Kstatus" },
		// A bare newline is as good as ESC for forging a whole extra line of
		// output, so it must be refused for the same reason.
		"newline in a warning": func(s *Snapshot) { s.Warnings = []string{"ok\n  digest:      sha256:forged"} },
		// C1 CSI (U+009B), the 8-bit form of ESC-[.
		"c1 csi in a warning": func(s *Snapshot) { s.Warnings = []string{"\u009b1A\u009b2K"} },
		// origin.assumed_installed reaches the rule by a different road --
		// checkAssumedInstalled, not checkDisplayStrings -- and belongs in
		// this inventory anyway, because the inventory is the list of snapshot
		// strings a terminal ever renders and a road of its own is exactly how
		// one of them gets forgotten. It is also the only entry here that
		// travels on into a bundle and is printed again by install, on a
		// machine whose operator never saw the builder's output.
		"origin.assumed_installed": func(s *Snapshot) {
			s.Origin = Origin{
				Kind: OriginSynthesized, BaseID: "debian:12/minimal",
				AssumedInstalled: []string{"bash:amd64", cursorUpEraseLine},
			}
		},
	}
	for name, mutate := range cases {
		s := honestDocument(t)
		if err := Validate(s); err != nil {
			t.Fatalf("control document failed to validate: %v", err)
		}
		mutate(s)
		err := Validate(s)
		if err == nil {
			t.Errorf("%s: Validate accepted a value carrying terminal control sequences", name)
			continue
		}
		if errClass(err) != "verification" {
			t.Errorf("%s: error class = %s, want verification: %v", name, errClass(err), err)
		}
	}
}

func TestValidateBoundsWarningsAndLabels(t *testing.T) {
	s := honestDocument(t)
	s.Warnings = make([]string, maxWarnings+1)
	for i := range s.Warnings {
		s.Warnings[i] = "x"
	}
	if err := Validate(s); err == nil {
		t.Errorf("Validate accepted %d warnings, over the %d limit", len(s.Warnings), maxWarnings)
	}

	s = honestDocument(t)
	s.Warnings = []string{strings.Repeat("x", maxWarningLen+1)}
	if err := Validate(s); err == nil {
		t.Errorf("Validate accepted a %d-byte warning, over the %d limit", maxWarningLen+1, maxWarningLen)
	}

	s = honestDocument(t)
	s.Labels = map[string]string{}
	for i := 0; i <= maxLabels; i++ {
		s.Labels["k"+strings.Repeat("x", i)] = "v"
	}
	if err := Validate(s); err == nil {
		t.Errorf("Validate accepted %d labels, over the %d limit", len(s.Labels), maxLabels)
	}

	s = honestDocument(t)
	s.Labels = map[string]string{"site": strings.Repeat("v", maxLabelValueLen+1)}
	if err := Validate(s); err == nil {
		t.Error("Validate accepted an over-long label value")
	}

	// The honest shapes must still pass: a handful of ordinary warnings and
	// labels is exactly what a real capture produces.
	s = honestDocument(t)
	s.Warnings = []string{"snapshot: /etc/apt/preferences: no such file"}
	s.Labels = map[string]string{"site": "hq", "ticket": "OPS-1234"}
	if err := Validate(s); err != nil {
		t.Errorf("Validate refused an ordinary document: %v", err)
	}
}

// TestOpenRefusesTerminalControlSequences is the end-to-end half: the
// sequence has to be refused where the untrusted archive is read, not merely
// where a hand-built struct is checked.
func TestOpenRefusesTerminalControlSequences(t *testing.T) {
	path, _ := realDebian12Snapshot(t)
	ms := patchDocument(t, readArchiveMembers(t, path), func(s *Snapshot) {
		s.Warnings = []string{cursorUpEraseLine}
	})
	tampered := writeArchiveMembers(t, filepath.Join(t.TempDir(), "escapes.tar.zst"), ms)

	a, err := Open(context.Background(), tampered)
	if err == nil {
		a.Close()
		t.Fatal("Open accepted a snapshot whose warnings carry cursor-movement escapes")
	}
	if errClass(err) != "verification" {
		t.Errorf("error class = %s, want verification: %v", errClass(err), err)
	}
}

// TestCaptureSanitisesHostileTargetStrings: capturing an already-hostile
// target must still yield a document the builder can open. The refusal above
// is the defence; this is what stops it from also being a foot-gun that
// makes an honest capture unreadable with no explanation.
func TestCaptureSanitisesHostileTargetStrings(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "etc/os-release",
		"ID=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=book\x1b[2Kworm\nPRETTY_NAME=\"Debian \x1b[1A12\"\n")
	writeFixtureFile(t, root, "var/lib/dpkg/status", "Package: bash\nStatus: install ok installed\nVersion: 5.2\n\n")
	writeFixtureFile(t, root, "var/lib/dpkg/arch", "amd64\n")

	s, fs := mustCapture(t, CaptureOptions{Root: root, IncludeKeyrings: false})
	if strings.ContainsRune(s.Target.Codename, 0x1b) || strings.ContainsRune(s.Target.PrettyName, 0x1b) {
		t.Errorf("capture recorded escape sequences verbatim: codename=%q pretty_name=%q",
			s.Target.Codename, s.Target.PrettyName)
	}
	if err := Validate(s); err != nil {
		t.Fatalf("a capture of a hostile target produced a document Validate refuses: %v", err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(path, s, fs); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open refused a sanitised honest capture: %v", err)
	}
	a.Close()
}
