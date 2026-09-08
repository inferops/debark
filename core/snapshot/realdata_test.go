package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise Capture against genuine captured target state
// (testdata/real-targets/*.tar.gz, in the bash prototype's own target-state/
// layout: dpkg-status, apt/sources.list[.d], apt/preferences.d,
// apt/trusted.gpg.d, keyrings/<original path>, arch, foreign-arches,
// os-release, codename, version_id) rather than a hand-written fixture, so
// the parsers meet real deb822 sources, real binary and armoured keyrings,
// and (for the Ubuntu capture) a real 588-package dpkg status file.
//
// The prototype captures neither apt.conf.d nor machine-id -- precisely the
// gap this package fills -- so both are synthesised here before Capture runs.
// The archive is remapped from the prototype's own bundle layout onto a real
// filesystem-root layout (etc/apt/..., var/lib/dpkg/...) as it goes; the
// fingerprint counts asserted below were independently confirmed with
// `gpg --show-keys --with-fingerprint --with-colons` against the same
// extracted keyring files.

func TestCaptureRealDebian12Target(t *testing.T) {
	root := extractPrototypeCapture(t, "../../testdata/real-targets/debian-12-state.tar.gz", debianOSRelease)
	s, fs := mustCapture(t, CaptureOptions{Root: root, IncludeKeyrings: true})

	if s.Target.DistroID != "debian" || s.Target.VersionID != "12" || s.Target.Codename != "bookworm" {
		t.Errorf("target identity = %+v", s.Target)
	}
	if s.Target.Arch != "amd64" {
		t.Errorf("arch = %q", s.Target.Arch)
	}
	if s.InstalledCount != 88 {
		t.Errorf("installed_count = %d, want 88 (per testdata/real-targets/README.md)", s.InstalledCount)
	}
	if s.Target.MachineID == "" {
		t.Error("synthesised machine-id not captured")
	}
	if len(s.APT.Conf) == 0 {
		t.Error("synthesised apt.conf.d not captured")
	}

	// 9 trusted.gpg.d/*.asc files (2+2+1+2+2+1+2+2+1 = 15 fingerprints) plus
	// the one referenced usr/share/keyrings/debian-archive-keyring.gpg (15
	// fingerprints, both debian.sources stanzas point at the same file) =
	// 10 distinct keyring files, 30 fingerprints.
	if len(s.APT.Trusted) != 9 {
		t.Errorf("APT.Trusted = %d files, want 9", len(s.APT.Trusted))
	}
	if len(s.APT.Keyrings) != 1 {
		t.Fatalf("APT.Keyrings = %+v, want 1 (debian-archive-keyring.gpg)", s.APT.Keyrings)
	}
	if !strings.HasSuffix(s.APT.Keyrings[0].Path, "debian-archive-keyring.gpg") {
		t.Errorf("APT.Keyrings[0] = %+v", s.APT.Keyrings[0])
	}
	if len(s.KeyringFingerprints) != 30 {
		t.Errorf("KeyringFingerprints = %d, want 30", len(s.KeyringFingerprints))
	}
	// The bookworm stable release key appears both standalone in
	// trusted.gpg.d and inside the combined debian-archive-keyring.gpg.
	count := 0
	for _, kf := range s.KeyringFingerprints {
		if kf.Fingerprint == "4D64FEC119C2029067D6E791F8D2585B8783D481" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("bookworm-stable key appears %d times, want 2 (trusted.gpg.d + combined keyring)", count)
	}
	// debian-archive-removed-keys.gpg is present in usr/share/keyrings but
	// referenced by no source: it must not be captured.
	for _, f := range s.APT.Keyrings {
		if strings.Contains(f.Path, "removed-keys") {
			t.Errorf("an unreferenced keyring was captured: %s", f.Path)
		}
	}

	if len(s.Warnings) != 0 {
		t.Errorf("unexpected warnings against a genuine, complete target: %v", s.Warnings)
	}

	if err := Validate(s); err != nil {
		t.Errorf("Validate: %v", err)
	}
	_ = fs
}

func TestCaptureRealUbuntu2404Target(t *testing.T) {
	root := extractPrototypeCapture(t, "../../testdata/real-targets/ubuntu-2404-state.tar.gz", ubuntuOSRelease)
	s, _ := mustCapture(t, CaptureOptions{Root: root, IncludeKeyrings: true})

	if s.Target.DistroID != "ubuntu" || s.Target.VersionID != "24.04" || s.Target.Codename != "noble" {
		t.Errorf("target identity = %+v", s.Target)
	}
	if s.InstalledCount != 588 {
		t.Errorf("installed_count = %d, want 588 (per testdata/real-targets/README.md)", s.InstalledCount)
	}

	// trusted.gpg.d: 2 files, 1 fingerprint each = 2. Referenced:
	// ubuntu-archive-keyring.gpg (3 keys, both ubuntu.sources stanzas point
	// at it) + tailscale-archive-keyring.gpg (2 keys, from tailscale.list's
	// signed-by=) = 2 more files, 5 more fingerprints. Total 4 files, 7 keys.
	if len(s.APT.Trusted) != 2 {
		t.Errorf("APT.Trusted = %d files, want 2", len(s.APT.Trusted))
	}
	if len(s.APT.Keyrings) != 2 {
		t.Fatalf("APT.Keyrings = %+v, want 2", s.APT.Keyrings)
	}
	if len(s.KeyringFingerprints) != 7 {
		t.Errorf("KeyringFingerprints = %d, want 7", len(s.KeyringFingerprints))
	}
	haveTailscale, haveUbuntu := false, false
	for _, f := range s.APT.Keyrings {
		if strings.Contains(f.Path, "tailscale") {
			haveTailscale = true
		}
		if strings.Contains(f.Path, "ubuntu-archive-keyring") {
			haveUbuntu = true
		}
	}
	if !haveTailscale || !haveUbuntu {
		t.Errorf("APT.Keyrings = %+v, want both tailscale and ubuntu-archive-keyring", s.APT.Keyrings)
	}
	// the well-known Ubuntu Archive Automatic Signing Key (2018) fingerprint.
	found := false
	for _, kf := range s.KeyringFingerprints {
		if kf.Fingerprint == "790BC7277767219C42C86F933B4FE6ACC0B21F32" {
			found = true
		}
	}
	if !found {
		t.Error("well-known Ubuntu archive signing key fingerprint not found")
	}

	if err := Validate(s); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

const debianOSRelease = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION="12 (bookworm)"
VERSION_CODENAME=bookworm
ID=debian
`

const ubuntuOSRelease = `PRETTY_NAME="Ubuntu 24.04.3 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
`

// extractPrototypeCapture extracts a snapshot-target.sh archive
// (target-state/dpkg-status, apt/sources.list[.d], apt/preferences.d,
// apt/trusted.gpg.d, keyrings/<original absolute path>, arch,
// foreign-arches) into a fresh real-filesystem-root layout under a temp
// directory, adding the machine-id and apt.conf.d the prototype never
// captured. Skips (does not fail) the test when the archive is not present,
// so the package still builds and tests cleanly from a checkout that lacks
// the (large, separately-owned) testdata/real-targets tree.
func extractPrototypeCapture(t *testing.T, tarGzPath, osRelease string) string {
	t.Helper()
	f, err := os.Open(tarGzPath)
	if err != nil {
		t.Skipf("real-target fixture not available at %s: %v", tarGzPath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()

	root := t.TempDir()
	var archLines []string

	tr := tar.NewReader(gz)
	const prefix = "target-state/"
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if !strings.HasPrefix(hdr.Name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(hdr.Name, prefix)

		if hdr.Typeflag == tar.TypeReg && (rel == "arch" || rel == "foreign-arches") {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			archLines = append(archLines, strings.Fields(string(data))...)
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		var dest string
		switch {
		case rel == "dpkg-status":
			dest = "var/lib/dpkg/status"
		case rel == "apt/sources.list":
			dest = "etc/apt/sources.list"
		case rel == "apt/preferences":
			dest = "etc/apt/preferences"
		case strings.HasPrefix(rel, "apt/sources.list.d/"):
			dest = "etc/apt/sources.list.d/" + strings.TrimPrefix(rel, "apt/sources.list.d/")
		case strings.HasPrefix(rel, "apt/preferences.d/"):
			dest = "etc/apt/preferences.d/" + strings.TrimPrefix(rel, "apt/preferences.d/")
		case strings.HasPrefix(rel, "apt/trusted.gpg.d/"):
			dest = "etc/apt/trusted.gpg.d/" + strings.TrimPrefix(rel, "apt/trusted.gpg.d/")
		case strings.HasPrefix(rel, "keyrings/usr/share/keyrings/"):
			dest = "usr/share/keyrings/" + strings.TrimPrefix(rel, "keyrings/usr/share/keyrings/")
		case strings.HasPrefix(rel, "keyrings/etc/apt/keyrings/"):
			dest = "etc/apt/keyrings/" + strings.TrimPrefix(rel, "keyrings/etc/apt/keyrings/")
		default:
			continue // os-release (a dangling symlink in the prototype's own capture), codename, version_id, installed-packages.txt, snapshot-date: not part of a filesystem root
		}

		full := filepath.Join(root, filepath.FromSlash(dest))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFixtureFile(t, root, "var/lib/dpkg/arch", strings.Join(archLines, "\n")+"\n")
	writeFixtureFile(t, root, "etc/os-release", osRelease)
	// Synthesised: snapshot-target.sh captures neither of these, which is
	// exactly the gap this package closes.
	writeFixtureFile(t, root, "etc/machine-id", "deadbeefdeadbeefdeadbeefdeadbeef\n")
	writeFixtureFile(t, root, "etc/apt/apt.conf.d/99synthetic", "APT::Install-Recommends \"true\";\n")

	return root
}
