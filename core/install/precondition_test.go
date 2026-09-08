package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskShortfall(t *testing.T) {
	cases := []struct {
		name          string
		free          uint64
		required      int64
		wantShortfall int64
		wantShort     bool
	}{
		{"plenty of room", 1_000_000, 100, 0, false},
		{"exactly enough", 100, 100, 0, false},
		{"short by one byte", 99, 100, 1, true},
		{"nothing required", 0, 0, 0, false},
		{"negative required (defensive)", 0, -5, 0, false},
		{"short by a lot", 10, 1_000_000, 999_990, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortfall, short := diskShortfall(tc.free, tc.required)
			if shortfall != tc.wantShortfall || short != tc.wantShort {
				t.Errorf("diskShortfall(%d, %d) = (%d, %v), want (%d, %v)",
					tc.free, tc.required, shortfall, short, tc.wantShortfall, tc.wantShort)
			}
		})
	}
}

func TestReadOSRelease(t *testing.T) {
	root := t.TempDir()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n" +
		"NAME=\"Debian GNU/Linux\"\n" +
		"VERSION_ID=\"12\"\n" +
		"VERSION=\"12 (bookworm)\"\n" +
		"VERSION_CODENAME=bookworm\n" +
		"ID=debian\n" +
		"# a comment line, must be ignored\n" +
		"HOME_URL=\"https://www.debian.org/\"\n"
	if err := os.WriteFile(filepath.Join(etcDir, "os-release"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	rel := readOSRelease(root)
	if rel.ID != "debian" || rel.VersionID != "12" || rel.VersionCodename != "bookworm" {
		t.Errorf("got %+v", rel)
	}
}

func TestReadOSRelease_Missing(t *testing.T) {
	rel := readOSRelease(t.TempDir())
	if rel != (osRelease{}) {
		t.Errorf("expected zero value for a missing file, got %+v", rel)
	}
}

func TestUnquoteShellValue(t *testing.T) {
	cases := map[string]string{
		`"bookworm"`:            "bookworm",
		`'bookworm'`:            "bookworm",
		"bookworm":              "bookworm",
		`"Debian GNU/Linux 12"`: "Debian GNU/Linux 12",
		`""`:                    "",
		"":                      "",
	}
	for in, want := range cases {
		if got := unquoteShellValue(in); got != want {
			t.Errorf("unquoteShellValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if err := lookExecutable(self); err != nil {
		t.Errorf("lookExecutable(%q) = %v, want nil", self, err)
	}
	if err := lookExecutable(filepath.Join(t.TempDir(), "definitely-does-not-exist")); err == nil {
		t.Errorf("expected an error for a nonexistent path")
	}
	if err := lookExecutable(""); err == nil {
		t.Errorf("expected an error for an empty path")
	}
	if err := lookExecutable(t.TempDir()); err == nil {
		t.Errorf("expected an error for a directory")
	}
}
