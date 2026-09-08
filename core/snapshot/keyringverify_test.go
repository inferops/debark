package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are the regression suite for docs/threat-model.md §3.2: a
// snapshot that carries both a malicious source and the key that
// authenticates it. They work on the real, checked-in Debian 12 capture,
// mutated in a temp directory (never in testdata/), because the whole point
// is that genuine key material and a genuine document are what the attacker
// starts from.
//
// The honest cases are as load-bearing as the hostile ones: a change that
// "fixed" this by refusing everything would be no fix at all, so
// TestOpenAcceptsHonestRealSnapshot and the round trip above it must keep
// passing.

// realDebian12Snapshot captures the real Debian 12 fixture and writes it as a
// genuine snapshot.tar.zst, returning the archive path and the document.
func realDebian12Snapshot(t *testing.T) (string, *Snapshot) {
	t.Helper()
	root := extractPrototypeCapture(t, "../../testdata/real-targets/debian-12-state.tar.gz", debianOSRelease)
	s, fs := mustCapture(t, CaptureOptions{Root: root, IncludeKeyrings: true})
	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(path, s, fs); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	return path, s
}

// attackerKeyring returns real, valid OpenPGP key material that is certainly
// not a Debian archive key: the Tailscale keyring out of the Ubuntu fixture.
// Real bytes, not a generated key -- nothing here ever touches gpg.
func attackerKeyring(t *testing.T) []byte {
	t.Helper()
	root := extractPrototypeCapture(t, "../../testdata/real-targets/ubuntu-2404-state.tar.gz", ubuntuOSRelease)
	data, err := os.ReadFile(filepath.Join(root, "usr", "share", "keyrings", "tailscale-archive-keyring.gpg"))
	if err != nil {
		t.Fatalf("read attacker key material: %v", err)
	}
	return data
}

type archiveMember struct {
	name string
	data []byte
}

func readArchiveMembers(t *testing.T, path string) []archiveMember {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, err := zstdDecompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var out []archiveMember
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, archiveMember{hdr.Name, data})
	}
	return out
}

func writeArchiveMembers(t *testing.T, path string, ms []archiveMember) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range ms {
		if err := tw.WriteHeader(&tar.Header{
			Name: m.name, Typeflag: tar.TypeReg, Size: int64(len(m.data)), Mode: 0o644, ModTime: archiveEpoch,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed, err := zstdCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// patchDocument rewrites the snapshot.json member in place through fn.
func patchDocument(t *testing.T, ms []archiveMember, fn func(*Snapshot)) []archiveMember {
	t.Helper()
	for i := range ms {
		if ms[i].name != DocumentName {
			continue
		}
		var s Snapshot
		if err := json.Unmarshal(ms[i].data, &s); err != nil {
			t.Fatal(err)
		}
		fn(&s)
		b, err := json.MarshalIndent(&s, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		ms[i].data = b
		return ms
	}
	t.Fatalf("no %s member in archive", DocumentName)
	return nil
}

const debianArchiveKeyringPath = "/usr/share/keyrings/debian-archive-keyring.gpg"

// TestOpenAcceptsHonestRealSnapshot is the control. Every refusal below is
// only meaningful while this passes.
func TestOpenAcceptsHonestRealSnapshot(t *testing.T) {
	path, s := realDebian12Snapshot(t)
	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open refused an honest, genuine capture: %v", err)
	}
	defer a.Close()
	if len(a.Snapshot.KeyringFingerprints) != len(s.KeyringFingerprints) {
		t.Errorf("keyring_fingerprints changed on load: %d, want %d",
			len(a.Snapshot.KeyringFingerprints), len(s.KeyringFingerprints))
	}
	if len(a.Snapshot.Warnings) != 0 {
		t.Errorf("honest snapshot gained warnings on load: %v", a.Snapshot.Warnings)
	}
}

// TestOpenRefusesSwappedKeyringMaterial is bypass C: the attacker replaces
// the keyring file's bytes with their own key and updates the document's
// digest and size so the archive still verifies against itself, while
// keyring_fingerprints goes on naming the genuine Debian archive keys.
// Before fingerprints were re-derived from the bytes, this opened cleanly
// and every downstream consumer -- including --approved-keys and
// lock.Origin.KeyFingerprint -- was told the genuine key had signed it.
func TestOpenRefusesSwappedKeyringMaterial(t *testing.T) {
	path, _ := realDebian12Snapshot(t)
	attacker := attackerKeyring(t)

	ms := readArchiveMembers(t, path)
	member := FilesDir + "/" + strings.TrimPrefix(debianArchiveKeyringPath, "/")
	swapped := false
	for i := range ms {
		if ms[i].name == member {
			ms[i].data = attacker
			swapped = true
		}
	}
	if !swapped {
		t.Fatalf("fixture no longer carries %s", member)
	}
	ms = patchDocument(t, ms, func(s *Snapshot) {
		for i := range s.APT.Keyrings {
			if s.APT.Keyrings[i].Path == debianArchiveKeyringPath {
				s.APT.Keyrings[i].SHA256 = sha256Hex(attacker)
				s.APT.Keyrings[i].Size = int64(len(attacker))
			}
		}
		// keyring_fingerprints deliberately untouched: still claims Debian.
	})
	tampered := writeArchiveMembers(t, filepath.Join(t.TempDir(), "swapped.tar.zst"), ms)

	a, err := Open(context.Background(), tampered)
	if err == nil {
		a.Close()
		t.Fatal("Open accepted a snapshot whose keyring bytes were swapped for an attacker's key while its recorded fingerprints went on naming the genuine Debian archive keys")
	}
	if errClass(err) != "verification" {
		t.Errorf("error class = %s, want verification: %v", errClass(err), err)
	}
}

// TestOpenRefusesFabricatedFingerprint is bypass D: no key material at all,
// just an invented keyring_fingerprints entry naming an approved fingerprint
// and pointing its signed_by at a real source file.
func TestOpenRefusesFabricatedFingerprint(t *testing.T) {
	path, s := realDebian12Snapshot(t)
	var approved string
	for _, kf := range s.KeyringFingerprints {
		if kf.Keyring == debianArchiveKeyringPath {
			approved = kf.Fingerprint
			break
		}
	}
	if approved == "" {
		t.Fatalf("fixture no longer records fingerprints for %s", debianArchiveKeyringPath)
	}

	member := FilesDir + "/" + strings.TrimPrefix(debianArchiveKeyringPath, "/")
	var kept []archiveMember
	for _, m := range readArchiveMembers(t, path) {
		if m.name == member {
			continue
		}
		kept = append(kept, m)
	}
	kept = patchDocument(t, kept, func(s *Snapshot) {
		s.APT.Keyrings = nil
		s.KeyringFingerprints = []KeyFingerprint{{
			Fingerprint: approved,
			Keyring:     debianArchiveKeyringPath,
			SignedBy:    []string{"etc/apt/sources.list.d/debian.sources"},
		}}
	})
	tampered := writeArchiveMembers(t, filepath.Join(t.TempDir(), "fabricated.tar.zst"), kept)

	a, err := Open(context.Background(), tampered)
	if err == nil {
		a.Close()
		t.Fatal("Open accepted a fingerprint claimed for a keyring the snapshot does not carry")
	}
	if errClass(err) != "verification" {
		t.Errorf("error class = %s, want verification: %v", errClass(err), err)
	}
}

// TestOpenRefusesUnrecordedKeyInKeyring is the other direction: bytes that
// contain a key the document never records. Auditing the document would show
// nothing of it, while apt would happily verify against it.
func TestOpenRefusesUnrecordedKeyInKeyring(t *testing.T) {
	path, _ := realDebian12Snapshot(t)
	attacker := attackerKeyring(t)

	ms := readArchiveMembers(t, path)
	member := FilesDir + "/" + strings.TrimPrefix(debianArchiveKeyringPath, "/")
	var appended []byte
	for i := range ms {
		if ms[i].name == member {
			appended = append(append([]byte(nil), ms[i].data...), attacker...)
			ms[i].data = appended
		}
	}
	ms = patchDocument(t, ms, func(s *Snapshot) {
		for i := range s.APT.Keyrings {
			if s.APT.Keyrings[i].Path == debianArchiveKeyringPath {
				s.APT.Keyrings[i].SHA256 = sha256Hex(appended)
				s.APT.Keyrings[i].Size = int64(len(appended))
			}
		}
	})
	tampered := writeArchiveMembers(t, filepath.Join(t.TempDir(), "extra-key.tar.zst"), ms)

	a, err := Open(context.Background(), tampered)
	if err == nil {
		a.Close()
		t.Fatal("Open accepted a keyring carrying a key the document does not record")
	}
	if errClass(err) != "verification" {
		t.Errorf("error class = %s, want verification: %v", errClass(err), err)
	}
}

// TestOpenRefusesFingerprintForUnparseableKeyring pins the deliberate choice
// for corrupt key material: bytes that are not a keyring contain no keys, so
// a fingerprint recorded against them is a claim about nothing.
func TestOpenRefusesFingerprintForUnparseableKeyring(t *testing.T) {
	path, _ := realDebian12Snapshot(t)
	garbage := []byte("this is not an OpenPGP keyring\n")

	ms := readArchiveMembers(t, path)
	member := FilesDir + "/" + strings.TrimPrefix(debianArchiveKeyringPath, "/")
	for i := range ms {
		if ms[i].name == member {
			ms[i].data = garbage
		}
	}
	ms = patchDocument(t, ms, func(s *Snapshot) {
		for i := range s.APT.Keyrings {
			if s.APT.Keyrings[i].Path == debianArchiveKeyringPath {
				s.APT.Keyrings[i].SHA256 = sha256Hex(garbage)
				s.APT.Keyrings[i].Size = int64(len(garbage))
			}
		}
	})
	tampered := writeArchiveMembers(t, filepath.Join(t.TempDir(), "garbage.tar.zst"), ms)

	a, err := Open(context.Background(), tampered)
	if err == nil {
		a.Close()
		t.Fatal("Open accepted fingerprints recorded against bytes that are not a keyring at all")
	}
	if errClass(err) != "verification" {
		t.Errorf("error class = %s, want verification: %v", errClass(err), err)
	}
}

// TestOpenAcceptsUnparseableKeyringWithNoClaims is the honest half of the
// same choice: capture already tolerates a keyring it cannot read (it warns
// and records no fingerprints for it), so a snapshot shaped that way must
// still open. Fail-closed happens where the decision is made -- core/apt
// finds no fingerprint for it and --approved-keys refuses the source -- not
// by making the snapshot unreadable.
func TestOpenAcceptsUnparseableKeyringWithNoClaims(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "etc/os-release", debianOSRelease)
	writeFixtureFile(t, root, "var/lib/dpkg/status", "Package: bash\nStatus: install ok installed\nVersion: 5.2\n\n")
	writeFixtureFile(t, root, "var/lib/dpkg/arch", "amd64\n")
	writeFixtureFile(t, root, "etc/apt/sources.list", "deb http://deb.debian.org/debian bookworm main\n")
	writeFixtureFile(t, root, "etc/apt/trusted.gpg.d/broken.gpg", "not a keyring at all\n")

	s, fs := mustCapture(t, CaptureOptions{Root: root, IncludeKeyrings: true})
	if len(s.KeyringFingerprints) != 0 {
		t.Fatalf("expected no fingerprints from an unreadable keyring, got %+v", s.KeyringFingerprints)
	}
	if len(s.APT.Trusted) != 1 {
		t.Fatalf("expected the unreadable keyring to still be captured, got %+v", s.APT.Trusted)
	}
	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(path, s, fs); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open refused an honest snapshot carrying an unreadable keyring: %v", err)
	}
	a.Close()
}

// TestOpenRefusesDuplicateDocument: a tar may name a member twice, and "last
// one wins" lets a crafted archive show one document to any other reader and
// a different one to debark.
func TestOpenRefusesDuplicateDocument(t *testing.T) {
	path, _ := realDebian12Snapshot(t)
	ms := readArchiveMembers(t, path)
	var doc archiveMember
	for _, m := range ms {
		if m.name == DocumentName {
			doc = m
		}
	}
	forged := patchDocument(t, []archiveMember{doc}, func(s *Snapshot) {
		s.Target.Codename = "forged"
	})
	ms = append(ms, forged[0])
	dup := writeArchiveMembers(t, filepath.Join(t.TempDir(), "dup.tar.zst"), ms)

	a, err := Open(context.Background(), dup)
	if err == nil {
		a.Close()
		t.Fatalf("Open accepted an archive with two %s members (last one won: codename %q)", DocumentName, a.Snapshot.Target.Codename)
	}
	if errClass(err) != "verification" {
		t.Errorf("error class = %s, want verification: %v", errClass(err), err)
	}
}

// TestArchiveDigestIdentifiesTheFile: Archive.Digest becomes
// lock.snapshot_digest and manifest.snapshot_digest, whose only job is to
// answer "which snapshot was this bundle built from". Two archives that
// differ in a field Open normalises must not answer that question the same
// way -- which they did, because the digest was taken after normalisation.
func TestArchiveDigestIdentifiesTheFile(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "etc/os-release", debianOSRelease)
	writeFixtureFile(t, root, "var/lib/dpkg/status", "Package: bash\nStatus: install ok installed\nVersion: 5.2\n\n")
	writeFixtureFile(t, root, "var/lib/dpkg/arch", "amd64\n")
	writeFixtureFile(t, root, "etc/machine-id", "deadbeefdeadbeefdeadbeefdeadbeef\n")
	writeFixtureFile(t, root, "etc/apt/apt.conf.d/99proxy",
		"Acquire::http::Proxy \"http://user:hunter2@proxy.internal:3128\";\n")

	s, fs := mustCapture(t, CaptureOptions{Root: root, Redact: true, IncludeKeyrings: false})
	honestPath := filepath.Join(t.TempDir(), "honest.tar.zst")
	if err := WriteArchive(honestPath, s, fs); err != nil {
		t.Fatal(err)
	}

	// The same document, still claiming redactions=[machine-id, proxies,
	// labels], but with the credentials and the machine id put back.
	raw, err := os.ReadFile(filepath.Join(root, "etc", "apt", "apt.conf.d", "99proxy"))
	if err != nil {
		t.Fatal(err)
	}
	ms := readArchiveMembers(t, honestPath)
	for i := range ms {
		if strings.HasSuffix(ms[i].name, "99proxy") {
			ms[i].data = raw
		}
	}
	ms = patchDocument(t, ms, func(s *Snapshot) {
		s.Target.MachineID = "deadbeefdeadbeefdeadbeefdeadbeef"
		for i := range s.APT.Conf {
			if strings.HasSuffix(s.APT.Conf[i].Path, "99proxy") {
				s.APT.Conf[i].SHA256 = sha256Hex(raw)
				s.APT.Conf[i].Size = int64(len(raw))
				s.APT.Conf[i].Redacted = false
			}
		}
	})
	leakyPath := writeArchiveMembers(t, filepath.Join(t.TempDir(), "leaky.tar.zst"), ms)

	honest, err := Open(context.Background(), honestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer honest.Close()
	leaky, err := Open(context.Background(), leakyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaky.Close()

	if honest.Digest == leaky.Digest {
		t.Errorf("two byte-different snapshots -- one of them still carrying proxy credentials and a machine id it claims to have redacted -- share the digest %s, so snapshot_digest cannot identify which one a bundle was built from", honest.Digest)
	}
	// The redaction safety net must still have run, and must have said so.
	if leaky.Snapshot.Target.MachineID != "" {
		t.Error("reapplyRedactions no longer strips a machine id the document claims to have redacted")
	}
	loud := false
	for _, w := range leaky.Snapshot.Warnings {
		if strings.Contains(w, "claimed redactions it had not applied") {
			loud = true
		}
	}
	if !loud {
		t.Errorf("re-stripping a document that lied about its own redactions produced no warning: %v", leaky.Snapshot.Warnings)
	}
	if len(honest.Snapshot.Warnings) != 0 {
		t.Errorf("an honest redacted snapshot must gain no warning on load, got %v", honest.Snapshot.Warnings)
	}
}
