package install

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestParseAptSim_Golden runs the parser against real, recorded apt-get -s
// output (testdata/apt-sim-*.txt — see the header comment in each file for
// image, date and exact command). Table-driven per the header lines.
func TestParseAptSim_Golden(t *testing.T) {
	cases := []struct {
		name        string
		golden      string
		wantInstall []string
		wantUpgrade []string
		wantRemove  []string
		wantProblem bool
	}{
		{
			name:        "fresh install (jq + 2 deps)",
			golden:      "apt-sim-install-new.txt",
			wantInstall: []string{"jq=1.6-2.1+deb12u2", "libjq1=1.6-2.1+deb12u2", "libonig5=6.9.8-1"},
		},
		{
			name:   "already the newest version",
			golden: "apt-sim-already-current.txt",
			// No Inst lines at all: parseAptSim correctly finds nothing to do.
		},
		{
			name:       "single removal",
			golden:     "apt-sim-remove.txt",
			wantRemove: []string{"jq=1.6-2.1+deb12u2"},
		},
		{
			name:       "removal with autoremoved dependents",
			golden:     "apt-sim-remove-autoremove.txt",
			wantRemove: []string{"jq=1.6-2.1+deb12u2", "libjq1=1.6-2.1+deb12u2", "libonig5=6.9.8-1"},
		},
		{
			name:        "unknown package name",
			golden:      "apt-sim-unknown-package.txt",
			wantProblem: true,
		},
		{
			name:        "full-upgrade with bracketed old versions",
			golden:      "apt-sim-full-upgrade.txt",
			wantUpgrade: []string{"base-files=12.4+deb12u15", "liblzma5=5.4.1-1+deb12u1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			toInstall, toUpgrade, toRemove, problems := parseAptSim(data)

			if !reflect.DeepEqual(toInstall, tc.wantInstall) && (len(toInstall) != 0 || len(tc.wantInstall) != 0) {
				t.Errorf("toInstall = %v, want %v", toInstall, tc.wantInstall)
			}
			if !reflect.DeepEqual(toUpgrade, tc.wantUpgrade) && (len(toUpgrade) != 0 || len(tc.wantUpgrade) != 0) {
				t.Errorf("toUpgrade = %v, want %v", toUpgrade, tc.wantUpgrade)
			}
			if !reflect.DeepEqual(toRemove, tc.wantRemove) && (len(toRemove) != 0 || len(tc.wantRemove) != 0) {
				t.Errorf("toRemove = %v, want %v", toRemove, tc.wantRemove)
			}
			if hasProblem := len(problems) > 0; hasProblem != tc.wantProblem {
				t.Errorf("problems = %v (len %d), wantProblem = %v", problems, len(problems), tc.wantProblem)
			}
		})
	}
}

func TestParseInstLine(t *testing.T) {
	cases := []struct {
		line        string
		wantName    string
		wantVersion string
		wantUpgrade bool
		wantOK      bool
	}{
		{
			line:        "Inst libonig5 (6.9.8-1 Debian:12.15/oldstable [amd64])",
			wantName:    "libonig5",
			wantVersion: "6.9.8-1",
			wantUpgrade: false,
			wantOK:      true,
		},
		{
			line:        "Inst base-files [12.4+deb12u14] (12.4+deb12u15 Debian:12.15/oldstable [amd64])",
			wantName:    "base-files",
			wantVersion: "12.4+deb12u15",
			wantUpgrade: true,
			wantOK:      true,
		},
		{
			line:   "Inst",
			wantOK: false,
		},
		{
			line:   "Conf jq (1.6-2.1+deb12u2 Debian:12.15/oldstable [amd64])",
			wantOK: false, // not an Inst line
		},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			name, version, upgrade, ok := parseInstLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if name != tc.wantName || version != tc.wantVersion || upgrade != tc.wantUpgrade {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", name, version, upgrade, tc.wantName, tc.wantVersion, tc.wantUpgrade)
			}
		})
	}
}

func TestParseRemvLine(t *testing.T) {
	name, version, ok := parseRemvLine("Remv jq [1.6-2.1+deb12u2]")
	if !ok || name != "jq" || version != "1.6-2.1+deb12u2" {
		t.Fatalf("got (%q, %q, %v)", name, version, ok)
	}
	if _, _, ok := parseRemvLine("Inst jq (1.0)"); ok {
		t.Fatalf("parseRemvLine should reject a non-Remv line")
	}
}

// TestParseAptSim_AlreadyCurrentDerivation exercises the same subtraction
// runner.go uses for Report.AlreadyCurrent, against the golden where nothing
// needs to change.
func TestParseAptSim_AlreadyCurrentDerivation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "apt-sim-already-current.txt"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	toInstall, toUpgrade, _, _ := parseAptSim(data)
	requested := 1 // "jq" was the only thing asked for
	already := requested - len(toInstall) - len(toUpgrade)
	if already != 1 {
		t.Fatalf("already_current = %d, want 1", already)
	}
}
