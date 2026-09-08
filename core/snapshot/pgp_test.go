package snapshot

import (
	"os"
	"testing"
)

// Ground truth for these fixture keys was independently obtained with
// `gpg --show-keys --with-fingerprint --with-colons` against the real
// keyring files (extracted from testdata/real-targets) before they were copied into these fixtures, so this test
// checks the hand-rolled parser against a real, independent OpenPGP
// implementation's answer, not just against itself.

func TestParseKeyringFileBinaryRealKey(t *testing.T) {
	data := readTestdata(t, "debian12-list/etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg")
	keys, err := parseKeyringFile(data)
	if err != nil {
		t.Fatalf("parseKeyringFile: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1: %+v", len(keys), keys)
	}
	k := keys[0]
	if k.Fingerprint != "4D64FEC119C2029067D6E791F8D2585B8783D481" {
		t.Errorf("fingerprint = %q, want the real Debian 12 stable release key", k.Fingerprint)
	}
	if k.KeyID != "F8D2585B8783D481" {
		t.Errorf("key id = %q, want the last 16 hex chars of the fingerprint", k.KeyID)
	}
	wantUID := "Debian Stable Release Key (12/bookworm) <debian-release@lists.debian.org>"
	if len(k.UserIDs) != 1 || k.UserIDs[0] != wantUID {
		t.Errorf("user ids = %v, want [%q]", k.UserIDs, wantUID)
	}
}

func TestParseKeyringFileMultiKeyRealFile(t *testing.T) {
	data := readTestdata(t, "ubuntu2404-deb822/usr/share/keyrings/ubuntu-archive-keyring.gpg")
	keys, err := parseKeyringFile(data)
	if err != nil {
		t.Fatalf("parseKeyringFile: %v", err)
	}
	want := map[string]bool{
		"790BC7277767219C42C86F933B4FE6ACC0B21F32": true,
		"843938DF228D22F7B3742BC0D94AA3F0EFE21092": true,
		"F6ECB3762474EDA9D21B7022871920D1991BC93C": true,
	}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(keys), len(want), keys)
	}
	for _, k := range keys {
		if !want[k.Fingerprint] {
			t.Errorf("unexpected fingerprint %q", k.Fingerprint)
		}
		delete(want, k.Fingerprint)
	}
	if len(want) != 0 {
		t.Errorf("missing expected fingerprints: %v", want)
	}
}

func TestParseKeyringFileArmored(t *testing.T) {
	armored := `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEY865UxYJKwYBBAHaRw8BAQdAd7Z0srwuhlB6JKFkcf4HU4SSS/xcRfwEQWzr
crf6AEq0SURlYmlhbiBTdGFibGUgUmVsZWFzZSBLZXkgKDEyL2Jvb2t3b3JtKSA8
ZGViaWFuLXJlbGVhc2VAbGlzdHMuZGViaWFuLm9yZz6IlgQTFggAPhYhBE1k/sEZ
wgKQZ9bnkfjSWFuHg9SBBQJjzrlTAhsDBQkPCZwABQsJCAcCBhUKCQgLAgQWAgMB
Ah4BAheAAAoJEPjSWFuHg9SBSgwBAP9qpeO5z1s5m4D4z3TcqDo1wez6DNya27QW
WoG/4oBsAQCEN8Z00DXagPHbwrvsY2t9BCsT+PgnSn9biobwX7bDDg==
=5NZE
-----END PGP PUBLIC KEY BLOCK-----
`
	keys, err := parseKeyringFile([]byte(armored))
	if err != nil {
		t.Fatalf("parseKeyringFile: %v", err)
	}
	if len(keys) != 1 || keys[0].Fingerprint != "4D64FEC119C2029067D6E791F8D2585B8783D481" {
		t.Fatalf("got %+v, want the same key as the binary form", keys)
	}
}

func TestParseInlineArmoredKeyMatchesFileForm(t *testing.T) {
	// The inline-armored fixture embeds exactly the debian-archive-bookworm-
	// stable key; both parse paths must agree on its fingerprint.
	fileKeys, err := parseKeyringFile(readTestdata(t, "debian12-list/etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg"))
	if err != nil {
		t.Fatal(err)
	}
	sources := readTestdata(t, "inline-armored/etc/apt/sources.list.d/inline.sources")
	refs := extractSignedBy(sources)
	if len(refs) != 1 || refs[0].Armored == "" {
		t.Fatalf("fixture did not yield an inline armored ref: %+v", refs)
	}
	inlineKeys, err := parseInlineArmoredKey(refs[0].Armored)
	if err != nil {
		t.Fatalf("parseInlineArmoredKey: %v", err)
	}
	if len(inlineKeys) != 1 || len(fileKeys) != 1 || inlineKeys[0].Fingerprint != fileKeys[0].Fingerprint {
		t.Fatalf("inline=%+v file=%+v, fingerprints must match", inlineKeys, fileKeys)
	}
}

func TestParseKeyringFileEmpty(t *testing.T) {
	keys, err := parseKeyringFile(nil)
	if err != nil {
		t.Fatalf("an empty keyring must parse cleanly (real 0-byte keyrings exist), got error: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("got %d keys from empty input", len(keys))
	}
}

func TestParseKeyringFileGarbageProducesErrorNotPanic(t *testing.T) {
	if _, err := parseKeyringFile([]byte("this is not a keyring at all, just text\n")); err == nil {
		t.Error("expected an error for non-OpenPGP content")
	}
	if _, err := parseKeyringFile([]byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\nnot base64!!\n-----END PGP PUBLIC KEY BLOCK-----\n")); err == nil {
		t.Error("expected an error for malformed armor")
	}
}

func TestLooksArmored(t *testing.T) {
	if !looksArmored([]byte("  \n-----BEGIN PGP PUBLIC KEY BLOCK-----\n...")) {
		t.Error("armored content with leading whitespace not detected")
	}
	if looksArmored([]byte{0x99, 0x02, 0x0d}) {
		t.Error("binary content misdetected as armored")
	}
}

// readTestdata reads a file under testdata/, failing the test on error.
func readTestdata(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + rel)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", rel, err)
	}
	return data
}
