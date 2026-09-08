package snapshot

import (
	"context"
	"strings"
	"testing"
)

func mustCapture(t *testing.T, opts CaptureOptions) (*Snapshot, *FileSet) {
	t.Helper()
	s, fs, err := Capture(context.Background(), opts)
	if err != nil {
		t.Fatalf("Capture(%q): %v", opts.Root, err)
	}
	return s, fs
}

func findFile(files []File, suffix string) (File, bool) {
	for _, f := range files {
		if strings.HasSuffix(f.Path, suffix) {
			return f, true
		}
	}
	return File{}, false
}

func TestCaptureDebian12List(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})

	if s.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %q", s.SchemaVersion)
	}
	if s.Target.DistroID != "debian" || s.Target.VersionID != "12" || s.Target.Codename != "bookworm" {
		t.Errorf("target identity = %+v", s.Target)
	}
	if s.Target.PrettyName != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("pretty_name = %q", s.Target.PrettyName)
	}
	if s.Target.Arch != "amd64" || len(s.Target.ForeignArchs) != 0 {
		t.Errorf("arch = %q foreign=%v", s.Target.Arch, s.Target.ForeignArchs)
	}
	if s.Target.APTVersion != "2.6.1" {
		t.Errorf("apt_version = %q", s.Target.APTVersion)
	}
	if s.Target.DpkgVersion != "1.21.22" {
		t.Errorf("dpkg_version = %q", s.Target.DpkgVersion)
	}
	if s.Target.MachineID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("machine_id = %q", s.Target.MachineID)
	}
	if s.Target.OSRelease == nil {
		t.Fatal("os_release not captured")
	}

	// 3 stanzas in the fixture, one "deinstall ok config-files" -> 2.
	if s.InstalledCount != 2 {
		t.Errorf("installed_count = %d, want 2", s.InstalledCount)
	}
	if _, ok := fs.Bytes[s.DpkgStatus.ArchivePath]; !ok {
		t.Error("dpkg status bytes not in FileSet")
	}

	if len(s.APT.Sources) != 1 || !strings.HasSuffix(s.APT.Sources[0].Path, "/etc/apt/sources.list") {
		t.Errorf("APT.Sources = %+v", s.APT.Sources)
	}
	if len(s.APT.Preferences) != 0 || len(s.APT.Conf) != 0 {
		t.Errorf("expected no preferences/conf in this fixture: prefs=%v conf=%v", s.APT.Preferences, s.APT.Conf)
	}
	if len(s.APT.Trusted) != 1 {
		t.Fatalf("APT.Trusted = %+v, want 1 entry", s.APT.Trusted)
	}
	if len(s.APT.Keyrings) != 0 {
		t.Errorf("no Signed-By references in this fixture, but APT.Keyrings = %+v", s.APT.Keyrings)
	}
	if len(s.KeyringFingerprints) != 1 {
		t.Fatalf("KeyringFingerprints = %+v, want 1", s.KeyringFingerprints)
	}
	kf := s.KeyringFingerprints[0]
	if kf.Fingerprint != "4D64FEC119C2029067D6E791F8D2585B8783D481" {
		t.Errorf("fingerprint = %q", kf.Fingerprint)
	}
	if len(kf.SignedBy) != 0 {
		t.Errorf("a trusted.gpg.d key is ambient trust, not referenced by any source: SignedBy = %v", kf.SignedBy)
	}

	if len(s.Warnings) != 0 {
		t.Errorf("unexpected warnings for a complete fixture: %v", s.Warnings)
	}
	if len(s.Redactions) != 0 {
		t.Errorf("Redact was not requested: %v", s.Redactions)
	}
}

func TestCaptureUbuntu2404Deb822(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/ubuntu2404-deb822", IncludeKeyrings: true})

	if s.Target.DistroID != "ubuntu" || s.Target.VersionID != "24.04" || s.Target.Codename != "noble" {
		t.Errorf("target identity = %+v", s.Target)
	}
	if s.Target.APTVersion != "2.8.3" {
		t.Errorf("apt_version = %q", s.Target.APTVersion)
	}
	if s.InstalledCount != 3 {
		t.Errorf("installed_count = %d, want 3", s.InstalledCount)
	}

	src, ok := findFile(s.APT.Sources, "ubuntu.sources")
	if !ok {
		t.Fatal("ubuntu.sources not captured")
	}
	if _, ok := fs.Bytes[src.ArchivePath]; !ok {
		t.Error("ubuntu.sources bytes missing from FileSet")
	}

	if len(s.APT.Conf) != 1 {
		t.Errorf("APT.Conf = %+v, want the one 70debconf fragment", s.APT.Conf)
	}
	// 70debconf sets nothing about recommends -> default applies.
	if v, explicit := InstallRecommends(s, fs); !v || explicit {
		t.Errorf("InstallRecommends = %v,%v, want true,false (default)", v, explicit)
	}

	kr, ok := findFile(s.APT.Keyrings, "ubuntu-archive-keyring.gpg")
	if !ok {
		t.Fatalf("APT.Keyrings = %+v, want the referenced ubuntu-archive-keyring.gpg", s.APT.Keyrings)
	}
	if _, ok := fs.Bytes[kr.ArchivePath]; !ok {
		t.Error("referenced keyring bytes missing from FileSet")
	}
	if len(s.KeyringFingerprints) != 3 {
		t.Fatalf("KeyringFingerprints = %+v, want 3", s.KeyringFingerprints)
	}
	for _, kf := range s.KeyringFingerprints {
		if len(kf.SignedBy) != 1 || !strings.HasSuffix(kf.SignedBy[0], "ubuntu.sources") {
			t.Errorf("SignedBy for %s = %v, want [.../ubuntu.sources] (referenced by both stanzas, deduped to one)", kf.Fingerprint, kf.SignedBy)
		}
	}

	if len(s.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", s.Warnings)
	}
}

func TestCaptureMixedListAndSources(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/mixed", IncludeKeyrings: true})

	if len(s.APT.Sources) != 2 {
		t.Fatalf("APT.Sources = %+v, want 2 (one .list, one .sources)", s.APT.Sources)
	}
	if len(s.APT.Keyrings) != 2 {
		t.Fatalf("APT.Keyrings = %+v, want 2 (keyA.gpg and keyB.gpg)", s.APT.Keyrings)
	}
	if len(s.KeyringFingerprints) != 2 {
		t.Fatalf("KeyringFingerprints = %+v, want 2 (one key per file)", s.KeyringFingerprints)
	}
	byFP := map[string]KeyFingerprint{}
	for _, kf := range s.KeyringFingerprints {
		byFP[kf.Fingerprint] = kf
	}
	a, ok := byFP["F6ECB3762474EDA9D21B7022871920D1991BC93C"] // keyA: ubuntu-keyring-2018-archive
	if !ok {
		t.Fatalf("missing keyA fingerprint, got %+v", byFP)
	}
	if !strings.HasSuffix(a.Keyring, "keyA.gpg") || len(a.SignedBy) != 1 || !strings.HasSuffix(a.SignedBy[0], "classic.list") {
		t.Errorf("keyA linkage wrong: %+v", a)
	}
	b, ok := byFP["4D64FEC119C2029067D6E791F8D2585B8783D481"] // keyB: debian bookworm-stable
	if !ok {
		t.Fatalf("missing keyB fingerprint, got %+v", byFP)
	}
	if !strings.HasSuffix(b.Keyring, "keyB.gpg") || len(b.SignedBy) != 1 || !strings.HasSuffix(b.SignedBy[0], "modern.sources") {
		t.Errorf("keyB linkage wrong: %+v", b)
	}
}

func TestCaptureInlineArmoredKey(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/inline-armored", IncludeKeyrings: true})

	if len(s.APT.Keyrings) != 0 {
		t.Errorf("an inline key has no separate file; APT.Keyrings = %+v", s.APT.Keyrings)
	}
	if len(s.KeyringFingerprints) != 1 {
		t.Fatalf("KeyringFingerprints = %+v, want 1", s.KeyringFingerprints)
	}
	kf := s.KeyringFingerprints[0]
	if kf.Fingerprint != "4D64FEC119C2029067D6E791F8D2585B8783D481" {
		t.Errorf("fingerprint = %q", kf.Fingerprint)
	}
	if !strings.HasSuffix(kf.Keyring, "inline.sources") {
		t.Errorf("Keyring = %q, want the .sources file itself (the key has no separate path)", kf.Keyring)
	}
	if len(kf.SignedBy) != 1 || !strings.HasSuffix(kf.SignedBy[0], "inline.sources") {
		t.Errorf("SignedBy = %v, want the same .sources file", kf.SignedBy)
	}
	if len(s.Warnings) != 0 {
		t.Errorf("a valid inline key must not warn: %v", s.Warnings)
	}
}

func TestCaptureInstallRecommendsFalseNestedBlock(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/recommends-false"})

	if len(s.APT.Conf) != 1 {
		t.Fatalf("APT.Conf = %+v", s.APT.Conf)
	}
	value, explicit := InstallRecommends(s, fs)
	if value || !explicit {
		t.Errorf("InstallRecommends = %v,%v, want false,true", value, explicit)
	}
	// Default-Release must be captured verbatim (as part of apt.conf.d),
	// even though the schema does not parse it out as its own field.
	if !strings.Contains(string(fs.Bytes[s.APT.Conf[0].ArchivePath]), "Default-Release") {
		t.Error("Default-Release not present in the captured apt.conf.d bytes")
	}
}

func TestCaptureForeignArch(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/foreign-arch-i386"})
	if s.Target.Arch != "amd64" {
		t.Errorf("arch = %q", s.Target.Arch)
	}
	if len(s.Target.ForeignArchs) != 1 || s.Target.ForeignArchs[0] != "i386" {
		t.Errorf("foreign_archs = %v, want [i386]", s.Target.ForeignArchs)
	}
}

func TestCaptureMissingKeyringIsWarningNotFailure(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/missing-keyring", IncludeKeyrings: true})

	if len(s.APT.Keyrings) != 0 {
		t.Errorf("APT.Keyrings = %+v, want none (the referenced file does not exist)", s.APT.Keyrings)
	}
	if len(s.KeyringFingerprints) != 0 {
		t.Errorf("KeyringFingerprints = %+v, want none", s.KeyringFingerprints)
	}
	if len(s.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly one", s.Warnings)
	}
	if !strings.Contains(s.Warnings[0], "does-not-exist.gpg") {
		t.Errorf("warning does not mention the missing keyring: %q", s.Warnings[0])
	}
}

func TestCaptureIncludeKeyringsFalseSkipsEverything(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/ubuntu2404-deb822", IncludeKeyrings: false})
	if len(s.APT.Trusted) != 0 || len(s.APT.Keyrings) != 0 || len(s.KeyringFingerprints) != 0 {
		t.Errorf("IncludeKeyrings=false must skip all keyring capture: trusted=%v keyrings=%v fps=%v",
			s.APT.Trusted, s.APT.Keyrings, s.KeyringFingerprints)
	}
}

func TestCaptureWithRedact(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{
		Root:            "testdata/debian12-list",
		IncludeKeyrings: true,
		Redact:          true,
		Labels:          map[string]string{"site": "hq"},
	})
	if s.Target.MachineID != "" {
		t.Errorf("machine id not redacted: %q", s.Target.MachineID)
	}
	if s.Labels != nil {
		t.Errorf("labels not redacted: %v", s.Labels)
	}
	want := []string{RedactLabels, RedactMachineID, RedactProxies}
	if !equalStrings(s.Redactions, want) {
		t.Errorf("Redactions = %v, want %v", s.Redactions, want)
	}
	_ = fs
}

func TestCaptureLabelsWithoutRedact(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", Labels: map[string]string{"site": "hq"}})
	if s.Labels["site"] != "hq" {
		t.Errorf("labels = %v", s.Labels)
	}
	if len(s.Redactions) != 0 {
		t.Errorf("Redact was not requested: %v", s.Redactions)
	}
}

func TestCaptureRootMustBeADirectory(t *testing.T) {
	_, _, err := Capture(context.Background(), CaptureOptions{Root: "testdata/does-not-exist"})
	if err == nil {
		t.Fatal("expected an error for a non-existent root")
	}
	if got := errClass(err); got != "usage" {
		t.Errorf("class = %s, want usage", got)
	}
}

func TestCaptureMissingDpkgStatusIsFatal(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "etc/os-release", "ID=debian\nVERSION_ID=12\n")
	writeFixtureFile(t, dir, "var/lib/dpkg/arch", "amd64\n")
	// deliberately no var/lib/dpkg/status

	_, _, err := Capture(context.Background(), CaptureOptions{Root: dir})
	if err == nil {
		t.Fatal("expected an error when dpkg status is entirely absent")
	}
	if got := errClass(err); got != "environment" {
		t.Errorf("class = %s, want environment", got)
	}
}

func TestCaptureContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Capture(ctx, CaptureOptions{Root: "testdata/debian12-list"})
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestCountInstalled(t *testing.T) {
	data := []byte(`Package: a
Status: install ok installed

Package: b
Status: deinstall ok config-files

Package: c
Status: install ok installed
`)
	if n := countInstalled(data); n != 2 {
		t.Errorf("countInstalled = %d, want 2", n)
	}
}
