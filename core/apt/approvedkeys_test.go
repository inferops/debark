package apt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/snapshot"
)

// --approved-keys is the control docs/threat-model.md §3.2 names as the
// defence against a snapshot that carries both a malicious source and the key
// that authenticates it. It was enforced against the snapshot document's own
// keyring_fingerprints -- strings the untrusted side of the air gap wrote --
// so two tamperings walked straight through it:
//
//	C  the keyring file's bytes swapped for an attacker's key, with
//	   keyring_fingerprints left naming the genuine Debian archive keys
//	D  no keyring at all, one fabricated keyring_fingerprints entry naming
//	   an approved fingerprint
//
// Both are exercised below against the real Debian 12 capture. So are the
// honest cases: a control that refuses everything is not a control, so
// TestApprovedKeys_HonestSnapshotPasses and _UnrelatedListRefuses are as
// load-bearing as the refusals.

const debianArchiveKeyring = "/usr/share/keyrings/debian-archive-keyring.gpg"

// honestDebian12 captures the real checked-in Debian 12 fixture through
// snapshot.Capture -- the same path a real target takes -- and materialises
// its files/ tree, returning both. Unlike snapshotFromPrototypeState this
// produces a document with real KeyringFingerprints, which is the whole
// point: the honest case has to be honest all the way down.
//
// The fixture's zero-byte /etc/apt/sources.list is left out deliberately. A
// source file declaring no sources at all still trips checkApprovedKeys'
// "nothing pins this to a key" refusal, which is correct and fail-closed but
// would make an honest PASS unreachable; a deb822-only target (the shape
// Debian 12 ships) is what this models.
func honestDebian12(t *testing.T) (*snapshot.Snapshot, string) {
	t.Helper()
	tarPath := filepath.Join("..", "..", "testdata", "real-targets", "debian-12-state.tar.gz")
	if _, err := os.Stat(tarPath); err != nil {
		t.Skipf("fixture not found at %s: %v", tarPath, err)
	}
	stateDir := extractPrototypeState(t, tarPath, t.TempDir())

	root := t.TempDir()
	copyInto := func(src, dstRel string) {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		dst := filepath.Join(root, filepath.FromSlash(dstRel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyDir := func(srcRel, dstRel string) {
		entries, err := os.ReadDir(filepath.Join(stateDir, filepath.FromSlash(srcRel)))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			copyInto(filepath.Join(stateDir, filepath.FromSlash(srcRel), e.Name()), dstRel+"/"+e.Name())
		}
	}
	copyInto(filepath.Join(stateDir, "dpkg-status"), "var/lib/dpkg/status")
	copyDir("apt/sources.list.d", "etc/apt/sources.list.d")
	copyDir("apt/preferences.d", "etc/apt/preferences.d")
	copyDir("apt/trusted.gpg.d", "etc/apt/trusted.gpg.d")
	copyDir("keyrings/usr/share/keyrings", "usr/share/keyrings")
	arch, err := os.ReadFile(filepath.Join(stateDir, "arch"))
	if err != nil {
		t.Fatal(err)
	}
	writeRootFile(t, root, "var/lib/dpkg/arch", string(arch))
	writeRootFile(t, root, "etc/os-release",
		"ID=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n")

	snap, fs, err := snapshot.Capture(context.Background(), snapshot.CaptureOptions{Root: root, IncludeKeyrings: true})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := snapshot.Validate(snap); err != nil {
		t.Fatalf("the honest capture does not validate: %v", err)
	}
	filesDir := t.TempDir()
	for archivePath, data := range fs.Bytes {
		p := filepath.Join(filesDir, filepath.FromSlash(archivePath))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return snap, filesDir
}

func writeRootFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// genuineApprovedKey returns one real fingerprint out of the keyring the
// fixture's debian.sources actually pins itself to.
func genuineApprovedKey(t *testing.T, snap *snapshot.Snapshot) string {
	t.Helper()
	for _, kf := range snap.KeyringFingerprints {
		if kf.Keyring == debianArchiveKeyring {
			return kf.Fingerprint
		}
	}
	t.Fatalf("fixture records no fingerprints for %s", debianArchiveKeyring)
	return ""
}

// attackerKeyMaterial is real, valid OpenPGP key material that is certainly
// not a Debian archive key: the Tailscale keyring out of the Ubuntu fixture.
// Real bytes from a checked-in capture -- nothing here ever runs gpg.
func attackerKeyMaterial(t *testing.T) []byte {
	t.Helper()
	tarPath := filepath.Join("..", "..", "testdata", "real-targets", "ubuntu-2404-state.tar.gz")
	if _, err := os.Stat(tarPath); err != nil {
		t.Skipf("fixture not found at %s: %v", tarPath, err)
	}
	stateDir := extractPrototypeState(t, tarPath, t.TempDir())
	data, err := os.ReadFile(filepath.Join(stateDir, "keyrings", "usr", "share", "keyrings", "tailscale-archive-keyring.gpg"))
	if err != nil {
		t.Fatalf("read attacker key material: %v", err)
	}
	return data
}

func buildWithApprovedKeys(t *testing.T, snap *snapshot.Snapshot, filesDir string, approved []string) (*PrivateRoot, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "aptroot")
	return BuildPrivateRoot(RootSpec{
		Dir:                 dir,
		ArchivesDir:         filepath.Join(dir, "..", "archives"),
		Arch:                snap.Target.Arch,
		PhasedPolicy:        snapshot.PhasedNeverInclude,
		Snapshot:            snap,
		SnapshotFilesDir:    filesDir,
		CopySnapshotSources: true,
		ApprovedKeys:        approved,
	})
}

func mustBePolicyRefusal(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted", what)
	}
	if got := dferr.ClassOf(err).String(); got != "policy" {
		t.Errorf("%s: error class = %s, want policy (exit 6): %v", what, got, err)
	}
}

// Case A. Everything below is meaningless if this ever stops passing.
func TestApprovedKeys_HonestSnapshotPasses(t *testing.T) {
	snap, filesDir := honestDebian12(t)
	root, err := buildWithApprovedKeys(t, snap, filesDir, []string{genuineApprovedKey(t, snap)})
	if err != nil {
		t.Fatalf("an honest snapshot with a correct approved list was refused: %v", err)
	}
	found := false
	for _, rec := range root.SignedBy {
		if rec.OriginalPath == debianArchiveKeyring && len(rec.Fingerprints) > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("no Signed-By record carried fingerprints derived from the copied keyring: %+v", root.SignedBy)
	}
}

// Case B. The control has to actually refuse when it should.
func TestApprovedKeys_UnrelatedListRefuses(t *testing.T) {
	snap, filesDir := honestDebian12(t)
	_, err := buildWithApprovedKeys(t, snap, filesDir,
		[]string{"1111111111111111111111111111111111111111"})
	mustBePolicyRefusal(t, err, "an honest snapshot against an approved list matching nothing in it")
}

// Case C: the attacker swaps the keyring file's bytes for their own key --
// which is what apt will really verify against -- and leaves
// keyring_fingerprints naming the genuine Debian archive keys. Deciding on
// the document's claim approved the attacker's key and, worse, recorded the
// approved fingerprint in lock.Origin.KeyFingerprint for packages it never
// signed.
func TestApprovedKeys_SwappedKeyMaterialRefused(t *testing.T) {
	snap, filesDir := honestDebian12(t)
	approved := genuineApprovedKey(t, snap)

	var archivePath string
	for _, f := range snap.APT.Keyrings {
		if f.Path == debianArchiveKeyring {
			archivePath = f.ArchivePath
		}
	}
	if archivePath == "" {
		t.Fatalf("fixture no longer captures %s", debianArchiveKeyring)
	}
	attacker := attackerKeyMaterial(t)
	if err := os.WriteFile(filepath.Join(filesDir, filepath.FromSlash(archivePath)), attacker, 0o644); err != nil {
		t.Fatal(err)
	}
	// keyring_fingerprints deliberately untouched: the document still swears
	// this file holds the Debian archive keys.

	root, err := buildWithApprovedKeys(t, snap, filesDir, []string{approved})
	mustBePolicyRefusal(t, err, "a source whose keyring bytes were swapped for an attacker's key")
	if root != nil {
		for _, rec := range root.SignedBy {
			for _, fp := range rec.Fingerprints {
				if fp == approved {
					t.Errorf("the approved fingerprint %s was still attributed to %s after its bytes were swapped", fp, rec.OriginalPath)
				}
			}
		}
	}
}

// Case D: no key material at all, one fabricated keyring_fingerprints entry
// naming the approved fingerprint and pointing signed_by at the real source
// file. The Signed-By is stripped for want of a keyring, which alone must
// fail closed -- a source verified against the private root's whole combined
// trusted.gpg.d is pinned to nothing.
func TestApprovedKeys_FabricatedFingerprintRefused(t *testing.T) {
	snap, filesDir := honestDebian12(t)
	approved := genuineApprovedKey(t, snap)

	var sourceArchivePath string
	for _, f := range snap.APT.Sources {
		if strings.HasSuffix(f.Path, "debian.sources") {
			sourceArchivePath = f.ArchivePath
		}
	}
	if sourceArchivePath == "" {
		t.Fatal("fixture no longer captures debian.sources")
	}
	snap.APT.Keyrings = nil
	snap.KeyringFingerprints = []snapshot.KeyFingerprint{{
		Fingerprint: approved,
		Keyring:     debianArchiveKeyring,
		SignedBy:    []string{sourceArchivePath},
	}}

	_, err := buildWithApprovedKeys(t, snap, filesDir, []string{approved})
	mustBePolicyRefusal(t, err, "a fabricated fingerprint for a keyring the snapshot does not carry")
	if err != nil && !strings.Contains(err.Error(), "stripped") {
		t.Errorf("the refusal should name the stripped Signed-By as the reason, got: %v", err)
	}
}

// A stripped Signed-By must fail closed on its own terms, with or without a
// fingerprint claim: apt verifies such a source against the private root's
// whole combined keyring set, so nobody is in a position to say an approved
// key authenticated it.
func TestApprovedKeys_StrippedSignedByRefused(t *testing.T) {
	snap, filesDir := honestDebian12(t)
	approved := genuineApprovedKey(t, snap)
	snap.APT.Keyrings = nil // the Signed-By target is gone; the field gets stripped

	_, err := buildWithApprovedKeys(t, snap, filesDir, []string{approved})
	mustBePolicyRefusal(t, err, "a source whose Signed-By was stripped")
}

// A keyring whose bytes are not an OpenPGP keyring at all yields no
// fingerprints, so nothing can be approved through it. This is the
// deliberate answer to "what happens when a keyring cannot be parsed":
// fail closed, and say so in a warning rather than silently.
func TestApprovedKeys_UnparseableKeyringRefused(t *testing.T) {
	snap, filesDir := honestDebian12(t)
	approved := genuineApprovedKey(t, snap)

	for _, f := range snap.APT.Keyrings {
		if f.Path == debianArchiveKeyring {
			if err := os.WriteFile(filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath)),
				[]byte("this is not an OpenPGP keyring\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	_, err := buildWithApprovedKeys(t, snap, filesDir, []string{approved})
	mustBePolicyRefusal(t, err, "a source pinned to a keyring that cannot be parsed")

	// Without an approved-keys policy the root still builds -- refusing to
	// resolve at all because one keyring is corrupt would be a worse answer
	// -- but the operator must be told.
	root, err := buildWithApprovedKeys(t, snap, filesDir, nil)
	if err != nil {
		t.Fatalf("an unparseable keyring must not break a build with no approved-keys policy: %v", err)
	}
	warned := false
	for _, w := range root.Warnings {
		if w.Code == "private-root.keyring-unparseable" {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about the unreadable keyring: %+v", root.Warnings)
	}
}

// An inline armoured Signed-By key belongs to no keyring, so it can be traced
// to no approved fingerprint. A file whose only Signed-By is inline already
// failed closed; this pins the mixed case, where a second stanza is properly
// pinned and must not carry the inline one in on its approval.
func TestApprovedKeys_InlineKeyRefused(t *testing.T) {
	approved := "04B54C3CDCA79751B16BC6B5225629DF75B188BD"
	records := []SignedByRecord{
		{SourceFile: "etc/apt/sources.list.d/x.sources", Stanza: 0,
			OriginalPath: debianArchiveKeyring, RewrittenTo: "/root/trusted.gpg.d/d.gpg",
			Fingerprints: []string{approved}},
		{SourceFile: "etc/apt/sources.list.d/x.sources", Stanza: 1, Inline: true},
	}
	err := checkApprovedKeys(
		[]string{"etc/apt/sources.list.d/x.sources"},
		map[string][]SignedByRecord{"etc/apt/sources.list.d/x.sources": records},
		[]string{approved})
	mustBePolicyRefusal(t, err, "a source mixing a pinned stanza with an inline armoured key")

	// The pinned stanza alone still passes: this must not have become a
	// blanket refusal.
	if err := checkApprovedKeys(
		[]string{"etc/apt/sources.list.d/x.sources"},
		map[string][]SignedByRecord{"etc/apt/sources.list.d/x.sources": records[:1]},
		[]string{approved}); err != nil {
		t.Errorf("a properly pinned source was refused: %v", err)
	}

	// And with no policy configured at all, nothing is refused: the policy is
	// opt-in.
	if err := checkApprovedKeys(
		[]string{"etc/apt/sources.list.d/x.sources"},
		map[string][]SignedByRecord{"etc/apt/sources.list.d/x.sources": records},
		nil); err != nil {
		t.Errorf("approved-keys is opt-in, but an empty list refused: %v", err)
	}
}
